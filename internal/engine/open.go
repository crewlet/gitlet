package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/gitlet/internal/bus"
	"github.com/crewlet/gitlet/internal/config"
	"github.com/crewlet/gitlet/internal/flock"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/ledger"
	"github.com/crewlet/gitlet/internal/model"
)

const (
	// shutdownBudget bounds Shutdown when its context has no deadline
	// (docs/SPEC.md, section 6.4).
	shutdownBudget = 3 * time.Second
	// identityLockWait bounds the wait for the lock around agent id creation.
	identityLockWait = 10 * time.Second
	// extractCacheSize is how many parsed files the extraction cache keeps.
	extractCacheSize = 4096
	// sessionTries and sessionRetry: how often and how far apart Open tries
	// the session lock before concluding another session holds it.
	sessionTries = 5
	sessionRetry = 20 * time.Millisecond
)

func open(ctx context.Context, opt Options) (*Engine, error) {
	dir := opt.Dir
	if dir == "" {
		dir = "."
	}
	repo, err := gitx.Open(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("engine: open %s: %w", dir, err)
	}
	e := &Engine{}
	e.opt = opt
	e.now = opt.Now
	if e.now == nil {
		e.now = time.Now
	}
	e.log = opt.Logger
	if e.log == nil {
		e.log = slog.New(slog.DiscardHandler)
	}
	e.repo = repo
	e.bg = repo.Background()
	if opt.Config != nil {
		e.cfg = *opt.Config
	} else if e.cfg, err = config.Load(ctx, e.bg); err != nil {
		return nil, fmt.Errorf("engine: config: %w", err)
	}
	e.cfg = withDefaults(e.cfg)
	e.checkGit(ctx)

	e.stateDir = filepath.Join(repo.CommonDir, "gitlet")
	if err := os.MkdirAll(e.stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	e.ledgerPath = filepath.Join(e.stateDir, "ledger.db")
	if abs, err := filepath.Abs(e.ledgerPath); err == nil {
		e.ledgerPath = abs
	}
	led, err := openLedger(ctx, e.ledgerPath)
	if err != nil {
		return nil, fmt.Errorf("engine: ledger: %w", err)
	}
	e.led, e.notes = led, led
	opened := false
	defer func() {
		if !opened {
			_ = e.freeResources()
		}
	}()

	id, err := e.resolveAgentID(ctx)
	if err != nil {
		return nil, err
	}
	e.me = identity(id, opt.Client)
	if opt.Session {
		if err := e.takeSession(); err != nil {
			return nil, err
		}
	}
	remote := e.cfg.Remote
	if opt.Offline {
		// Offline engines never contact the remote: the bus works on the
		// local refs/gitlet/* as it does without a remote.
		remote = ""
	}
	e.bus = bus.New(e.bg, id, bus.Options{
		Remote:  remote,
		Main:    e.cfg.Main,
		LockDir: e.stateDir,
		PushLog: led,
		Now:     e.now,
	})
	e.pollSem = make(chan struct{}, 1)
	e.extract = newExtractCache(extractCacheSize)
	e.resetCaches()
	opened = true
	return e, nil
}

// withDefaults fills unset durations, counts and names of c from
// config.Defaults, so a partial Options.Config (tests) cannot make the
// engine spin or divide by zero.
func withDefaults(c config.Config) config.Config {
	d := config.Defaults()
	durations := []struct{ v, def *time.Duration }{
		{&c.SnapshotInterval, &d.SnapshotInterval},
		{&c.PollInterval, &d.PollInterval},
		{&c.Lease, &d.Lease},
		{&c.ThreadTimeout, &d.ThreadTimeout},
		{&c.StaleItem, &d.StaleItem},
		{&c.PublishTimeout, &d.PublishTimeout},
		{&c.OutsideWindow, &d.OutsideWindow},
		{&c.OutsideInterval, &d.OutsideInterval},
		{&c.ReapAfter, &d.ReapAfter},
	}
	for _, x := range durations {
		if *x.v <= 0 {
			*x.v = *x.def
		}
	}
	counts := []struct{ v, def *int }{
		{&c.ThreadMessages, &d.ThreadMessages},
		{&c.BroadFiles, &d.BroadFiles},
		{&c.BroadSymbols, &d.BroadSymbols},
		{&c.OutsideMax, &d.OutsideMax},
	}
	for _, x := range counts {
		if *x.v <= 0 {
			*x.v = *x.def
		}
	}
	if c.MaxSnapshotFile <= 0 {
		c.MaxSnapshotFile = d.MaxSnapshotFile
	}
	if c.BranchPrefix == "" {
		c.BranchPrefix = d.BranchPrefix
	}
	if c.SnapshotUntracked == "" {
		c.SnapshotUntracked = d.SnapshotUntracked
	}
	if c.Main == "" {
		c.Main = "main"
	}
	return c
}

// checkGit enables the merge-tree based checks when git is at least 2.38
// and otherwise records the warning every result carries (docs/SPEC.md,
// section 14.1).
func (e *Engine) checkGit(ctx context.Context) {
	ok, err := e.bg.AtLeast(ctx, gitx.MinMajor, gitx.MinMinor)
	if err == nil && ok {
		e.gitOK = true
		return
	}
	version := "of unknown version"
	if major, minor, err := e.bg.Version(ctx); err == nil {
		version = fmt.Sprintf("%d.%d", major, minor)
	}
	e.gitWarn = fmt.Sprintf("git %s is older than %d.%d: predicted conflicts and the merge checks of submit and update are disabled",
		version, gitx.MinMajor, gitx.MinMinor)
}

// resolveAgentID returns Options.Agent, else the id stored in the ledger for
// this worktree, else a new id stored for it. One agent per worktree: the
// CLI and the MCP server of a worktree share the id (docs/SPEC.md, section 5).
func (e *Engine) resolveAgentID(ctx context.Context) (model.AgentID, error) {
	if e.opt.Agent != "" {
		if !model.ValidAgentID(e.opt.Agent) {
			return "", fmt.Errorf("engine: invalid agent id %q: want [a-z0-9-], at most 48 characters", e.opt.Agent)
		}
		return e.opt.Agent, nil
	}
	lctx, cancel := context.WithTimeout(ctx, identityLockWait)
	defer cancel()
	lock, err := flock.Acquire(lctx, filepath.Join(e.stateDir, "locks", "identity.lock"))
	if err != nil {
		return "", fmt.Errorf("engine: identity lock: %w", err)
	}
	defer lock.Unlock()
	id, ok, err := e.led.AgentFor(ctx, e.repo.Root)
	if err != nil {
		return "", fmt.Errorf("engine: agent id: %w", err)
	}
	if ok && model.ValidAgentID(id) {
		return id, nil
	}
	id = model.NewAgentID(clientName(e.opt.Client), hostName())
	if !e.led.ReadOnly() {
		if err := e.led.SetAgentFor(ctx, e.repo.Root, id); err != nil {
			return "", fmt.Errorf("engine: store agent id: %w", err)
		}
	}
	return id, nil
}

// identity describes who is behind the agent id, as published in state.json.
func identity(id model.AgentID, client string) model.Agent {
	return model.Agent{ID: id, Client: clientName(client), Host: hostName(), User: userName()}
}

func clientName(c string) string {
	if c = strings.TrimSpace(c); c == "" {
		return "cli"
	}
	return c
}

func hostName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "localhost"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return strings.ToLower(h)
}

func userName() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil {
		name := u.Username
		if i := strings.LastIndexByte(name, '\\'); i >= 0 {
			name = name[i+1:]
		}
		return name
	}
	return ""
}

// takeSession takes <common>/gitlet/sessions/<agent>.lock for the engine's
// lifetime. When another live process holds it, this engine runs read-only
// and its results carry a same_worktree note with the command to run instead.
// A few retries ride out a CLI process probing the lock (sessionLive) at
// the same moment.
func (e *Engine) takeSession() error {
	var (
		l   *flock.Lock
		err error
	)
	for try := 0; try < sessionTries; try++ {
		if l, err = flock.TryLock(e.sessionPath()); !errors.Is(err, flock.ErrLocked) {
			break
		}
		time.Sleep(sessionRetry)
	}
	switch {
	case err == nil:
		e.session = l
	case errors.Is(err, flock.ErrLocked):
		e.sameWorktree = true
		e.worktreeCmd = worktreeCommand(e.repo.Root, e.mainStartPoint())
	default:
		return fmt.Errorf("engine: session lock: %w", err)
	}
	return nil
}

func (e *Engine) sessionPath() string {
	return filepath.Join(e.stateDir, "sessions", string(e.me.ID)+".lock")
}

// sessionLive reports whether a live session (an MCP server engine) holds
// this worktree's session lock. Engines without a session probe it
// briefly; the session's own engine knows it holds it.
func (e *Engine) sessionLive() bool {
	if e.session != nil || e.sameWorktree {
		return true
	}
	l, err := flock.TryLock(e.sessionPath())
	if err != nil {
		return errors.Is(err, flock.ErrLocked)
	}
	_ = l.Unlock()
	return false
}

// syncsNotes reports whether this engine reconciles the agent's notes in
// the ledger. Exactly one process per agent may: the session holder, or,
// when no session is live, the CLI. Two processes syncing different views
// would make notes flap between resolved and new.
func (e *Engine) syncsNotes() bool {
	if e.sameWorktree || e.led.ReadOnly() {
		return false
	}
	return e.session != nil || !e.sessionLive()
}

// mainStartPoint is the revision a new worktree should start from.
func (e *Engine) mainStartPoint() string {
	if e.cfg.Remote != "" {
		return e.cfg.Remote + "/" + e.cfg.Main
	}
	return e.cfg.Main
}

// worktreeCommand is the git worktree add command suggested to a second
// session in a worktree: a sibling directory with a detached HEAD on main,
// where start then creates the work branch.
func worktreeCommand(root, startPoint string) string {
	dir := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-"+model.RandomHex(4))
	return "git worktree add --detach " + shellQuote(dir) + " " + shellQuote(startPoint)
}

// shellQuote quotes s for a POSIX shell when it needs it.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:@+=,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (e *Engine) close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	return e.freeResources()
}

// freeResources frees the locks and the ledger. Callers hold mu or own e alone.
func (e *Engine) freeResources() error {
	var errs []error
	if e.agentLock != nil {
		errs = append(errs, e.agentLock.Unlock())
		e.agentLock, e.agentLockDepth = nil, 0
	}
	if e.session != nil {
		errs = append(errs, e.session.Unlock())
		e.session = nil
	}
	if e.led != nil {
		if err := closeLedger(e.ledgerPath); err != nil {
			errs = append(errs, fmt.Errorf("engine: close ledger: %w", err))
		}
	}
	return errors.Join(errs...)
}

// ledgers shares one *ledger.Ledger per database among the engines of a
// process, such as an MCP server serving several worktrees of one clone.
var ledgers = struct {
	sync.Mutex
	m map[string]*sharedLedger
}{m: map[string]*sharedLedger{}}

type sharedLedger struct {
	l    *ledger.Ledger
	refs int
}

// openLedger opens the ledger at path, or shares the one already open.
func openLedger(ctx context.Context, path string) (*ledger.Ledger, error) {
	ledgers.Lock()
	defer ledgers.Unlock()
	if s, ok := ledgers.m[path]; ok {
		s.refs++
		return s.l, nil
	}
	l, err := ledger.Open(ctx, path, ledger.Options{})
	if err != nil {
		return nil, err
	}
	ledgers.m[path] = &sharedLedger{l: l, refs: 1}
	return l, nil
}

// closeLedger releases one reference and closes the ledger with the last.
func closeLedger(path string) error {
	ledgers.Lock()
	defer ledgers.Unlock()
	s, ok := ledgers.m[path]
	if !ok {
		return nil
	}
	if s.refs--; s.refs > 0 {
		return nil
	}
	delete(ledgers.m, path)
	return s.l.Close()
}

func (e *Engine) shutdown(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, shutdownBudget)
		defer cancel()
	}
	e.mu.Lock()
	err := e.finalWrite(ctx)
	e.mu.Unlock()
	return errors.Join(err, e.close())
}

// finalWrite is Shutdown's last word (docs/SPEC.md, section 8): a final
// heartbeat while active or submitted items remain, otherwise the deletion
// of this agent's refs. Network failures and the budget running out are
// not errors: it is best effort.
func (e *Engine) finalWrite(ctx context.Context) error {
	if e.closed || e.readOnly() {
		return nil
	}
	st, stateOID, err := e.bus.ReadOwnState(ctx)
	if err != nil {
		return fmt.Errorf("engine: shutdown: %w", err)
	}
	if hasOpenItems(st.Items) {
		_, err := e.mutateState(ctx, func(*model.AgentState) error { return nil })
		return quiet(ctx, err)
	}
	_, outboxOID, err := e.bus.ReadOwnOutbox(ctx)
	if err != nil {
		return fmt.Errorf("engine: shutdown: %w", err)
	}
	if stateOID == "" && outboxOID == "" {
		return nil
	}
	dctx, cancel := context.WithTimeout(ctx, shutdownBudget)
	defer cancel()
	if err := e.bus.DeleteOwn(dctx); err != nil {
		return quiet(dctx, fmt.Errorf("engine: shutdown: %w", err))
	}
	return nil
}

// quiet drops errors that best-effort work tolerates: network failures and
// an exhausted context.
func quiet(ctx context.Context, err error) error {
	if err == nil || gitx.IsNetwork(err) || ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

// hasOpenItems reports whether any item is active or submitted.
func hasOpenItems(items []model.WorkItem) bool {
	for _, it := range items {
		if it.Status.Open() {
			return true
		}
	}
	return false
}

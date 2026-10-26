package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/engine"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
)

const (
	defaultTickInterval = 5 * time.Second
	// shutdownBudget bounds the final heartbeat or ref cleanup of all engines
	// (docs/SPEC.md, section 6.4).
	shutdownBudget = 3 * time.Second
)

var errClosed = errors.New("mcpserver: server is shutting down")

// backend is what the server needs from an engine. *engine.Engine satisfies
// it; tests substitute their own.
type backend interface {
	Call(ctx context.Context, verb string, input any) (api.Output, error)
	Tick(ctx context.Context) error
	Shutdown(ctx context.Context) error
	ReadOnly() bool
	Agent() model.Agent
}

type impl struct {
	opt   Options
	log   *slog.Logger
	srv   *mcp.Server
	tools []*tool

	// Seams replaced by tests.
	openEngine func(ctx context.Context, opt engine.Options) (backend, error)
	findRoot   func(ctx context.Context, dir string) (string, error)

	// ctx lives until Close: engines open under it and tick loops stop with it.
	ctx    context.Context
	cancel context.CancelFunc
	ticks  sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	roots   map[string]string    // directory -> worktree root
	engines map[string]*worktree // by worktree root

	closeOnce sync.Once
	closeErr  error
}

// worktree holds the engine for one worktree root. Its mutex is held while
// the engine opens, so concurrent first calls open it once.
type worktree struct {
	root   string
	mu     sync.Mutex
	eng    backend
	closed bool
}

func newServer(opt Options) *Server {
	s := &Server{}
	s.opt = opt
	if s.opt.Dir == "" {
		if wd, err := os.Getwd(); err == nil {
			s.opt.Dir = wd
		}
	} else if abs, err := filepath.Abs(s.opt.Dir); err == nil {
		s.opt.Dir = abs
	}
	if s.opt.TickInterval == 0 {
		s.opt.TickInterval = defaultTickInterval
	}
	if s.opt.Version == "" {
		s.opt.Version = "dev"
	}
	s.log = opt.Logger
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if s.opt.Agent == "" {
		s.opt.Agent = model.AgentID(os.Getenv("GITLET_AGENT"))
	}
	if s.opt.Agent != "" && !model.ValidAgentID(s.opt.Agent) {
		s.log.Error("gitlet: ignoring invalid agent id override; use lowercase letters, digits and dashes, at most 48",
			"agent", s.opt.Agent)
		s.opt.Agent = ""
	}

	open := opt.OpenEngine
	if open == nil {
		open = engine.Open
	}
	s.openEngine = func(ctx context.Context, eo engine.Options) (backend, error) {
		e, err := open(ctx, eo)
		if err != nil {
			return nil, err
		}
		return e, nil
	}
	s.findRoot = func(ctx context.Context, dir string) (string, error) {
		r, err := gitx.Open(ctx, dir)
		if err != nil {
			return "", err
		}
		return r.Root, nil
	}

	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.roots = make(map[string]string)
	s.engines = make(map[string]*worktree)

	s.srv = mcp.NewServer(
		&mcp.Implementation{Name: "gitlet", Title: "gitlet", Version: s.opt.Version},
		&mcp.ServerOptions{Instructions: Instructions, InitializedHandler: s.initialized},
	)
	s.tools = buildTools(s.log)
	for _, t := range s.tools {
		s.srv.AddTool(t.def, s.handler(t))
	}
	s.srv.AddReceivingMiddleware(toolOrder(s.tools))
	return s
}

func (s *Server) mcp() *mcp.Server { return s.srv }

func (s *Server) run(ctx context.Context, t mcp.Transport) error {
	err := s.srv.Run(ctx, t)
	if cerr := s.close(); cerr != nil {
		s.log.Warn("gitlet: shutdown incomplete", "err", cerr)
	}
	if err == nil || ctx.Err() != nil || errors.Is(err, mcp.ErrConnectionClosed) {
		// Asked to stop, or the client went away: both are a normal end.
		return nil
	}
	return fmt.Errorf("mcpserver: %w", err)
}

func (s *Server) close() error {
	s.closeOnce.Do(func() { s.closeErr = s.shutdown() })
	return s.closeErr
}

// shutdown stops the tick loops and shuts every engine down in parallel,
// within shutdownBudget in total.
func (s *Server) shutdown() error {
	s.mu.Lock()
	s.closed = true
	wts := make([]*worktree, 0, len(s.engines))
	for _, wt := range s.engines {
		wts = append(wts, wt)
	}
	s.mu.Unlock()
	s.cancel()

	ctx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer cancel()
	ticksDone := make(chan struct{})
	go func() { s.ticks.Wait(); close(ticksDone) }()
	select {
	case <-ticksDone:
	case <-ctx.Done():
		s.log.Warn("gitlet: a background tick did not stop in time")
	}

	errs := make([]error, len(wts))
	var wg sync.WaitGroup
	for i, wt := range wts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = wt.shutdown(ctx, s.log)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (wt *worktree) shutdown(ctx context.Context, log *slog.Logger) (err error) {
	wt.mu.Lock()
	defer wt.mu.Unlock()
	wt.closed = true
	if wt.eng == nil {
		return nil
	}
	defer func() {
		if p := recover(); p != nil {
			log.Error("gitlet: panic during shutdown", "worktree", wt.root, "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("mcpserver: shut down %s: panic: %v", wt.root, p)
		}
	}()
	if err := wt.eng.Shutdown(ctx); err != nil {
		return fmt.Errorf("mcpserver: shut down %s: %w", wt.root, err)
	}
	log.Info("gitlet: engine shut down", "worktree", wt.root)
	return nil
}

// initialized opens the default worktree's engine as soon as the client
// finished the handshake, so snapshots and leases start before the first
// tool call. Clients on the stateless protocol never send it; their first
// tool call opens the engine instead.
func (s *Server) initialized(_ context.Context, req *mcp.InitializedRequest) {
	client := clientName(req.ClientInfo())
	go func() {
		if _, _, err := s.engineFor(s.ctx, s.opt.Dir, client); err != nil && !errors.Is(err, errClosed) {
			s.log.Warn("gitlet: cannot serve the default worktree yet", "dir", s.opt.Dir, "err", err)
		}
	}()
}

// engineFor returns the engine serving the worktree that contains dir,
// opening it on first use. The default worktree's engine gets the agent id
// override; other worktrees keep their own ids (one agent per worktree).
func (s *Server) engineFor(ctx context.Context, dir, client string) (backend, string, error) {
	root, err := s.rootOf(ctx, dir)
	if err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, root, errClosed
	}
	wt := s.engines[root]
	if wt == nil {
		wt = &worktree{root: root}
		s.engines[root] = wt
	}
	s.mu.Unlock()

	wt.mu.Lock()
	defer wt.mu.Unlock()
	switch {
	case wt.closed:
		return nil, root, errClosed
	case wt.eng != nil:
		return wt.eng, root, nil
	}

	eo := engine.Options{
		Dir:     root,
		Client:  client,
		Session: true,
		Logger:  s.log.With("worktree", root),
	}
	if def, err := s.rootOf(ctx, s.opt.Dir); err == nil && def == root {
		eo.Agent = s.opt.Agent
	}
	eng, err := s.openEngine(s.ctx, eo)
	if err != nil {
		return nil, root, fmt.Errorf("mcpserver: open the engine: %w", err)
	}
	wt.eng = eng
	agent := eng.Agent()
	s.log.Info("gitlet: serving worktree", "worktree", root, "agent", agent.ID, "client", client)
	if eng.ReadOnly() {
		s.log.Warn("gitlet: another session serves this worktree; this one is read-only", "worktree", root)
	}
	if s.opt.TickInterval > 0 {
		s.ticks.Add(1)
		go s.tickLoop(root, eng)
	}
	return eng, root, nil
}

// rootOf resolves the worktree root of dir, caching the answer.
func (s *Server) rootOf(ctx context.Context, dir string) (string, error) {
	s.mu.Lock()
	root, ok := s.roots[dir]
	s.mu.Unlock()
	if ok {
		return root, nil
	}
	root, err := s.findRoot(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("mcpserver: find the worktree: %w", err)
	}
	root = filepath.Clean(root)
	s.mu.Lock()
	s.roots[dir] = root
	s.mu.Unlock()
	return root, nil
}

// tickLoop runs the engine's background work every TickInterval until the
// server closes. A failing tick is logged once per distinct error, so a
// remote that stays down does not flood the log.
func (s *Server) tickLoop(root string, eng backend) {
	defer s.ticks.Done()
	t := time.NewTicker(s.opt.TickInterval)
	defer t.Stop()
	var last string
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		err := s.tick(root, eng)
		switch {
		case err == nil:
			if last != "" {
				s.log.Info("gitlet: background work recovered", "worktree", root)
			}
			last = ""
		case s.ctx.Err() != nil:
			return
		case err.Error() != last:
			last = err.Error()
			s.log.Warn("gitlet: background work failed", "worktree", root, "err", err)
		default:
			s.log.Debug("gitlet: background work failed again", "worktree", root, "err", err)
		}
	}
}

func (s *Server) tick(root string, eng backend) (err error) {
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("gitlet: panic in background work", "worktree", root, "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return eng.Tick(s.ctx)
}

// knownClients are the client names gitlet init uses (install.ClaudeCode,
// install.Codex, install.Cursor). They are spelled out because install's
// tests import this package.
var knownClients = []string{"claude-code", "codex", "cursor"}

// clientName turns the MCP clientInfo name into the client part of the
// agent id: the name gitlet init uses for a client it knows (Codex reports
// "codex-mcp-client"), otherwise the name itself, lowercased.
func clientName(info *mcp.Implementation) string {
	if info == nil {
		return "mcp"
	}
	name := strings.ToLower(strings.TrimSpace(info.Name))
	if name == "" {
		return "mcp"
	}
	for _, c := range knownClients {
		if strings.HasPrefix(name, c) {
			return c
		}
	}
	return name
}

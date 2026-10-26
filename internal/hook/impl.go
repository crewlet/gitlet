package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/ledger"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/render"
)

// Time limits (docs/SPEC.md, section 14.2). Client hooks run between an
// agent's tool calls and give up quickly; git hooks run inside the agent's
// own git commands and may take a little longer (pre-push runs git status
// and a merge check).
const (
	clientBudget = 1500 * time.Millisecond
	gitBudget    = 3 * time.Second
	busyTimeout  = time.Second // ledger busy timeout for every hook
)

// maxDoc is the largest state document a hook reads (docs/SPEC.md, section 6).
const maxDoc = 1 << 20

const usage = "usage: gitlet hook claude-code session-start|user-prompt-submit|post-tool-use" +
	" | git prepare-commit-msg <file> [source] [sha] | git post-commit | git pre-push <remote> <url>" +
	" | sequence-editor <todo-file>"

// runner is one hook invocation: its streams, its time budgets and the few
// functions tests replace.
type runner struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	clientBudget   time.Duration
	gitBudget      time.Duration
	openRepo       func(ctx context.Context, dir string) (*gitx.Repo, error)
	getwd          func() (string, error)
	getenv         func(key string) string
	now            func() time.Time
}

func newRunner(stdin io.Reader, stdout, stderr io.Writer) *runner {
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	return &runner{
		stdin:        stdin,
		stdout:       stdout,
		stderr:       stderr,
		clientBudget: clientBudget,
		gitBudget:    gitBudget,
		openRepo:     gitx.Open,
		getwd:        os.Getwd,
		getenv:       os.Getenv,
		now:          time.Now,
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return newRunner(stdin, stdout, stderr).run(ctx, args)
}

// run dispatches on args. Everything but the sequence editor returns 0 and
// survives panics, so a hook never fails the agent's tool call or git command.
func (r *runner) run(ctx context.Context, args []string) (code int) {
	if len(args) > 0 && args[0] == "sequence-editor" {
		return r.sequenceEditor(ctx, args[1:])
	}
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(r.stderr, "gitlet hook: internal error: %v\n", p)
			code = 0
		}
	}()
	switch {
	case len(args) == 0:
		r.usage()
	case args[0] == "claude-code":
		r.claudeCode(ctx, args[1:])
	case args[0] == "git":
		r.git(ctx, args[1:])
	default:
		r.usage()
	}
	return 0
}

func (r *runner) usage() { fmt.Fprintln(r.stderr, usage) }

// report prints a hook error to stderr. Hooks swallow errors (docs/SPEC.md,
// section 14.2) but still say what went wrong.
func (r *runner) report(err error) {
	switch {
	case err == nil, errors.Is(err, context.Canceled), errors.Is(err, ledger.ErrReadOnly):
		// The host gave up on us, or a newer gitlet owns the ledger.
	case errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintln(r.stderr, "gitlet hook: gave up: out of time")
	default:
		fmt.Fprintf(r.stderr, "gitlet hook: %v\n", err)
	}
}

// session is what hooks read: the work tree, the clone's ledger and the
// worktree's agent ("" while the worktree has none).
type session struct {
	repo   *gitx.Repo
	ledger *ledger.Ledger
	agent  model.AgentID
}

func (s *session) close() { s.ledger.Close() }

// openSession opens the repository containing dir, its ledger and agent.
// It returns nil and no error when there is nothing to do: dir is not in a
// git work tree, or gitlet never ran in this clone (a hook never creates
// the ledger).
func (r *runner) openSession(ctx context.Context, dir string) (*session, error) {
	repo, err := r.openRepo(ctx, dir)
	if errors.Is(err, gitx.ErrNotRepo) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Hooks are work the agent did not ask for: never take optional locks,
	// least of all the index lock a running git commit holds.
	repo = repo.Background()
	path := filepath.Join(repo.CommonDir, "gitlet", "ledger.db")
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	l, err := ledger.Open(ctx, path, ledger.Options{BusyTimeout: busyTimeout})
	if err != nil {
		return nil, err
	}
	agent, err := r.agentFor(ctx, l, repo.Root)
	if err != nil {
		l.Close()
		return nil, err
	}
	return &session{repo: repo, ledger: l, agent: agent}, nil
}

// newNotes returns the agent's notes not shown yet, only those of severity
// sev unless it is "".
func (s *session) newNotes(ctx context.Context, sev model.Severity) ([]model.Note, error) {
	stored, err := s.ledger.OpenNotes(ctx, s.agent)
	if err != nil {
		return nil, err
	}
	var notes []model.Note
	for _, n := range stored {
		if n.Status == ledger.NoteNew && (sev == "" || n.Note.Severity == sev) {
			notes = append(notes, n.Note)
		}
	}
	return notes, nil
}

// shownInFull returns the notes render.Notes shows in full: the first
// render.MaxFullNotes after a stable sort by severity, as render sorts them.
func shownInFull(notes []model.Note) []model.Note {
	sorted := slices.Clone(notes)
	slices.SortStableFunc(sorted, func(a, b model.Note) int { return a.Severity.Rank() - b.Severity.Rank() })
	return sorted[:min(len(sorted), render.MaxFullNotes)]
}

// markShown marks notes shown. A failed mark (the ledger stayed busy past
// its timeout) is swallowed: it only means the notes show again later.
func (s *session) markShown(ctx context.Context, notes []model.Note, now time.Time) {
	ids := make([]string, len(notes))
	for i, n := range notes {
		ids[i] = n.ID
	}
	_ = s.ledger.MarkShown(ctx, s.agent, ids, now)
}

// agentFor returns the worktree's agent: GITLET_AGENT when set, else the id
// the ledger stores for the worktree root (docs/SPEC.md, section 5), else "".
func (r *runner) agentFor(ctx context.Context, l *ledger.Ledger, root string) (model.AgentID, error) {
	if v := strings.TrimSpace(r.getenv("GITLET_AGENT")); v != "" {
		if !model.ValidAgentID(model.AgentID(v)) {
			return "", fmt.Errorf("GITLET_AGENT %q is not a valid agent id", v)
		}
		return model.AgentID(v), nil
	}
	id, ok, err := l.AgentFor(ctx, root)
	if err != nil || !ok || !model.ValidAgentID(id) {
		return "", err
	}
	return id, nil
}

// stateRef is the agent's local state ref (docs/SPEC.md, section 6).
func stateRef(agent model.AgentID) string { return "refs/gitlet/agents/" + string(agent) + "/state" }

// agentItems reads the agent's work items from its local state ref and
// returns them with the current branch ("" when detached). There are no
// items when the agent has no state yet or a newer gitlet wrote it.
func agentItems(ctx context.Context, repo *gitx.Repo, agent model.AgentID) (items []model.WorkItem, branch string, err error) {
	if agent == "" {
		return nil, "", nil
	}
	data, err := repo.ReadFile(ctx, stateRef(agent), "state.json")
	if errors.Is(err, gitx.ErrNotFound) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxDoc {
		return nil, "", fmt.Errorf("%s: state.json is larger than %d bytes", stateRef(agent), maxDoc)
	}
	var st model.AgentState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, "", fmt.Errorf("%s: %w", stateRef(agent), err)
	}
	if st.Format > model.Format {
		return nil, "", nil // status reports it
	}
	if branch, err = repo.CurrentBranch(ctx); err != nil {
		return nil, "", err
	}
	return st.Items, branch, nil
}

// pickItem chooses the item work on branch belongs to: the active item on
// that branch, else the only active item if it records no branch (an
// implicit item opened on a detached HEAD). Work on any other branch is not
// the item's.
func pickItem(items []model.WorkItem, branch string) *model.WorkItem {
	for i := range items {
		if it := &items[i]; usable(it) && branch != "" && it.Branch == branch {
			return it
		}
	}
	if it := soleActive(items); it != nil && it.Branch == "" {
		return it
	}
	return nil
}

// soleActive returns the only active item, or nil when there are none or
// several.
func soleActive(items []model.WorkItem) *model.WorkItem {
	var only *model.WorkItem
	for i := range items {
		if it := &items[i]; usable(it) {
			if only != nil {
				return nil
			}
			only = it
		}
	}
	return only
}

// usable reports whether it is active with a well-formed id. Items with
// malformed ids are skipped, since their id ends up in commit messages.
func usable(it *model.WorkItem) bool {
	return it.Status == model.ItemActive && model.ValidItemID(it.ID)
}

// readAll reads rd up to max bytes and gives up when ctx is done: a host
// that never closes stdin must not hold the hook past its budget.
func readAll(ctx context.Context, rd io.Reader, max int64) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- result{err: fmt.Errorf("reading stdin: %v", p)}
			}
		}()
		data, err := io.ReadAll(io.LimitReader(rd, max+1))
		if err == nil && int64(len(data)) > max {
			err = fmt.Errorf("input larger than %d bytes", max)
		}
		done <- result{data, err}
	}()
	select {
	case res := <-done:
		return res.data, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// writeFileAtomic replaces path with data through a temporary file in the
// same directory, so a reader sees the old or the new content, never a
// mix. It keeps the file's permissions and writes through symlinks.
func writeFileAtomic(path string, data []byte) error {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		path = p
	}
	mode := fs.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".gitlet-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

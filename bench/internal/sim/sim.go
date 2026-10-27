// Package sim runs the multi-agent benchmark: scripted agents take tasks
// from a shared backlog, edit a real Go repository in their own clones of a
// shared bare remote, and land their work through a merge bot that runs CI.
// The same agents and tasks run under each coordination mode, so the
// difference between runs is the coordination alone.
package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/gitlet/bench/internal/gitenv"
	"github.com/crewlet/gitlet/bench/internal/goedit"
)

// Mode is a coordination strategy.
type Mode string

// The modes.
const (
	ModeGit      Mode = "git"      // plain git: rebase only when submitting
	ModeGitSync  Mode = "gitsync"  // plain git, rebasing onto main whenever it moves
	ModeFileLock Mode = "filelock" // advisory file locks: wait for earlier tasks on the same files
	ModeGitlet   Mode = "gitlet"   // gitlet, acting on its notes
)

// Config is one run.
type Config struct {
	Name         string
	Mode         Mode
	Agents       int
	Seed         uint64
	Compliance   float64
	CIRounds     int
	ResolveSpeed int
	MaxRounds    int
	RoundWall    time.Duration
	CoordCost    float64 // rounds an agent spends on one coordination call
	DupJudge     string  // gitlet: "oracle" judges every duplicate note rightly, "act" abandons on act notes only
	Overlap      string  // gitlet: "defer" waits out overlapping edits of earlier tasks, "notes" only those overlap notes name, "ignore" goes on
	Gitlet       string
	Keep         bool // keep the clones and CI checkout after the run
	Work         string
	TasksFile    string
}

// Sim is the state of one run.
type Sim struct {
	cfg     Config
	env     *gitenv.Env
	dir     string
	remote  string
	ciDir   string
	path    string // PATH for agent processes
	tasks   []*Task
	runs    map[string]*TaskRun // by task id
	byItem  map[string]string   // gitlet item id -> task id
	blocked map[string]bool     // tasks blocked on a contract at the start of the round
	backlog []*Task

	mu      sync.Mutex
	round   int
	main    string
	renames goedit.Renames
	prs     []*PR
	agents  []*Agent
	events  *json.Encoder
	evMu    sync.Mutex
	ciCache map[string]ciResult
	ciRuns  int
	ciTime  time.Duration
	start   time.Time
	stats   GitletStats
}

type ciResult struct {
	pass bool
	errs []string
}

// PR is one submission to the merge bot.
type PR struct {
	Task      string
	Agent     *Agent
	Head      string
	Submitted int
	Ready     int
	done      bool
}

// Run runs the benchmark for one configuration.
func Run(ctx context.Context, env *gitenv.Env, fx *Fixture, tasks []*Task, cfg Config) (*Result, error) {
	if cfg.Name == "" {
		cfg.Name = fmt.Sprintf("%s-n%d-s%d", cfg.Mode, cfg.Agents, cfg.Seed)
	}
	if cfg.CoordCost == 0 {
		cfg.CoordCost = 0.25
	}
	if cfg.Overlap == "" {
		cfg.Overlap = "defer"
	}
	if cfg.Mode == ModeGitlet && cfg.RoundWall == 0 {
		cfg.RoundWall = 6 * time.Second
	}
	s := &Sim{cfg: cfg, env: env, tasks: tasks, runs: map[string]*TaskRun{}, byItem: map[string]string{}, renames: goedit.Renames{}, ciCache: map[string]ciResult{}}
	s.dir = filepath.Join(cfg.Work, "runs", cfg.Name)
	if err := os.RemoveAll(s.dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, err
	}
	ev, err := os.Create(filepath.Join(s.dir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	defer ev.Close()
	s.events = json.NewEncoder(ev)
	if err := s.setup(ctx, fx); err != nil {
		return nil, err
	}
	s.backlog = append([]*Task(nil), tasks...)
	s.start = time.Now()
	for s.round = 0; s.round < cfg.MaxRounds; s.round++ {
		t0 := time.Now()
		// Hand out tasks in agent order, so that which agent gets which
		// task does not depend on goroutine scheduling, and note which
		// tasks are blocked on a contract as the round starts.
		s.blocked = map[string]bool{}
		for _, a := range s.agents {
			if a.ready() && !a.nextTaken {
				a.next, a.nextTaken = s.take(), true
			}
			if a.state == stBlocked && a.cur != nil {
				s.blocked[a.cur.ID] = true
			}
		}
		var wg sync.WaitGroup
		for _, a := range s.agents {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						s.event(a, "panic", map[string]any{"error": fmt.Sprint(r)})
						a.fail(fmt.Sprint(r))
					}
				}()
				a.act(ctx)
			}()
		}
		wg.Wait()
		if err := s.mergeBot(ctx); err != nil {
			return nil, err
		}
		if s.finished() {
			break
		}
		if d := cfg.RoundWall - time.Since(t0); d > 0 {
			time.Sleep(d)
		}
	}
	res := s.result(ctx)
	if err := WriteJSON(filepath.Join(s.dir, "result.json"), res); err != nil {
		return nil, err
	}
	if !cfg.Keep {
		for _, d := range []string{"agents", "ci"} {
			if err := os.RemoveAll(filepath.Join(s.dir, d)); err != nil {
				return nil, err
			}
		}
	}
	return res, nil
}

func (s *Sim) setup(ctx context.Context, fx *Fixture) error {
	s.remote = filepath.Join(s.dir, "remote.git")
	if _, err := s.env.Git(ctx, s.dir, "clone", "-q", "--bare", fx.Dir, s.remote); err != nil {
		return err
	}
	main, err := s.env.Git(ctx, s.remote, "rev-parse", "refs/heads/main")
	if err != nil {
		return err
	}
	s.main = main
	s.ciDir = filepath.Join(s.dir, "ci")
	if _, err := s.env.Git(ctx, s.remote, "worktree", "add", "-q", "--detach", s.ciDir, main); err != nil {
		return err
	}
	s.path = strings.Join([]string{filepath.Dir(s.cfg.Gitlet), "/usr/local/go/bin", "/usr/bin", "/bin"}, string(os.PathListSeparator))
	rng := rand.New(rand.NewPCG(s.cfg.Seed, 77))
	for i := range s.cfg.Agents {
		id := fmt.Sprintf("agent-%02d", i+1)
		dir := filepath.Join(s.dir, "agents", id)
		if _, err := s.env.Git(ctx, s.dir, "clone", "-q", s.remote, dir); err != nil {
			return err
		}
		if s.cfg.Mode == ModeGitlet {
			// Scale gitlet's clock to the simulation: a round stands for
			// about a minute of agent work and lasts RoundWall, so its
			// intervals shrink by the same factor (docs/SPEC.md, section 15).
			for k, v := range map[string]string{
				"gitlet.snapshotInterval": "1s",
				"gitlet.threadTimeout":    "60s",
				"gitlet.publishTimeout":   "90s",
				"gitlet.branchPrefix":     "gitlet/",
			} {
				if _, err := s.env.Git(ctx, dir, "config", k, v); err != nil {
					return err
				}
			}
		}
		a := &Agent{sim: s, ID: id, Dir: dir, rng: rand.New(rand.NewPCG(rng.Uint64(), uint64(i)))}
		a.envs = []string{"GITLET_AGENT=" + id, "PATH=" + s.path}
		s.agents = append(s.agents, a)
	}
	return nil
}

// take hands out the next task.
func (s *Sim) take() *Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.backlog) == 0 {
		return nil
	}
	t := s.backlog[0]
	s.backlog = s.backlog[1:]
	return t
}

func (s *Sim) finished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.backlog) > 0 {
		return false
	}
	for _, a := range s.agents {
		if a.state != stDone && a.state != stIdle {
			return false
		}
		if a.state == stIdle && a.cur != nil {
			return false
		}
	}
	return true
}

func (s *Sim) event(a *Agent, kind string, data map[string]any) {
	rec := map[string]any{"round": s.round, "kind": kind, "t": time.Since(s.start).Seconds()}
	if a != nil {
		rec["agent"] = a.ID
		if a.cur != nil {
			rec["task"] = a.cur.ID
		}
	}
	for k, v := range data {
		rec[k] = v
	}
	s.evMu.Lock()
	defer s.evMu.Unlock()
	_ = s.events.Encode(rec)
}

func (s *Sim) submit(a *Agent, head string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prs = append(s.prs, &PR{Task: a.cur.ID, Agent: a, Head: head, Submitted: s.round, Ready: s.round + s.cfg.CIRounds})
}

// mergeBot merges the pull requests whose CI time is up, in submission
// order: a textual conflict with main or a failing build sends the pull
// request back to its agent, otherwise main gets a merge commit.
func (s *Sim) mergeBot(ctx context.Context) error {
	var ready []*PR
	for _, pr := range s.prs {
		if !pr.done && pr.Ready <= s.round {
			ready = append(ready, pr)
		}
	}
	sort.SliceStable(ready, func(i, j int) bool {
		if ready[i].Ready != ready[j].Ready {
			return ready[i].Ready < ready[j].Ready
		}
		return ready[i].Agent.ID < ready[j].Agent.ID
	})
	for _, pr := range ready {
		pr.done = true
		a := pr.Agent
		tr := s.runs[pr.Task]
		tree, conflict, err := mergeTree(ctx, s.env, s.remote, s.main, pr.Head)
		if err != nil {
			return err
		}
		if conflict != nil {
			tr.MergeConflicts++
			s.event(a, "bounce", map[string]any{"reason": "conflict", "files": conflict.Files, "lines": conflict.Lines})
			a.bounced("conflict", tr)
			continue
		}
		pass, errs, err := s.ci(ctx, tree)
		if err != nil {
			return err
		}
		if !pass {
			tr.CIFailures++
			s.event(a, "bounce", map[string]any{"reason": "ci", "errors": errs[:min(5, len(errs))]})
			a.bounced("ci", tr)
			continue
		}
		msg := fmt.Sprintf("Merge %s: %s", tr.ID, tr.Intent)
		commit, err := s.env.Git(ctx, s.remote, "commit-tree", tree, "-p", s.main, "-p", pr.Head, "-m", msg)
		if err != nil {
			return err
		}
		if _, err := s.env.Git(ctx, s.remote, "update-ref", "refs/heads/main", commit, s.main); err != nil {
			return err
		}
		s.main = commit
		for _, op := range tr.Ops {
			if op.Kind == goedit.RenameFunc {
				dir, _, _ := strings.Cut(op.Target, ":")
				s.renames[op.Target] = goedit.FuncKey(dir, "", op.NewName)
			}
		}
		tr.Landed = s.round
		s.event(a, "landed", map[string]any{"main": commit})
		a.landed()
	}
	return nil
}

// ci builds a tree in the CI checkout. Results are cached by tree.
func (s *Sim) ci(ctx context.Context, tree string) (bool, []string, error) {
	if r, ok := s.ciCache[tree]; ok {
		return r.pass, r.errs, nil
	}
	if _, err := s.env.Git(ctx, s.ciDir, "read-tree", "-u", "--reset", tree); err != nil {
		return false, nil, err
	}
	t0 := time.Now()
	pass, errs, err := Build(ctx, s.env, s.ciDir)
	s.ciTime += time.Since(t0)
	s.ciRuns++
	if err != nil {
		return false, nil, err
	}
	s.ciCache[tree] = ciResult{pass, errs}
	return pass, errs, nil
}

// Build is the CI and local check during a run: build every library package
// of the module. Cross-task breakage can only appear there, because tasks
// add code only to library packages; each task alone passed the check it
// was generated with (a build with --check build, as in the README, or go vet
// over the whole module), and the final main is vetted too.
func Build(ctx context.Context, env *gitenv.Env, dir string) (bool, []string, error) {
	r, err := env.Run(ctx, dir, nil, "go", "list", "-e", "-f", "{{if and (ne .Name \"main\") .GoFiles}}{{.ImportPath}}{{end}}", "./...")
	if err != nil {
		return false, nil, err
	}
	if r.Code != 0 {
		return false, []string{strings.TrimSpace(r.Stderr)}, nil
	}
	pkgs := strings.Fields(r.Stdout)
	args := append([]string{"build", "-trimpath"}, pkgs...)
	r, err = env.Run(ctx, dir, nil, "go", args...)
	if err != nil {
		return false, nil, err
	}
	if r.Code == 0 {
		return true, nil, nil
	}
	var errs []string
	for _, l := range strings.Split(r.Stderr, "\n") {
		if strings.Contains(l, ".go:") {
			errs = append(errs, strings.TrimSpace(l))
		}
	}
	if len(errs) == 0 {
		errs = []string{strings.TrimSpace(r.Stderr)}
	}
	return false, errs, nil
}

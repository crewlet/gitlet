// Command gitlet-bench measures whether gitlet reduces the cost of many
// agents working on one repository, against plain-git baselines. See
// bench/README.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/crewlet/gitlet/bench/internal/gitenv"
	"github.com/crewlet/gitlet/bench/internal/goedit"
	"github.com/crewlet/gitlet/bench/internal/sim"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = gen(os.Args[2:])
	case "run":
		err = run(os.Args[2:])
	case "oracle":
		err = oracle(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gitlet-bench:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  gitlet-bench gen --work DIR --target SRC --seed N --tasks N [--changelog]
  gitlet-bench run --work DIR --tasks FILE --mode git|gitsync|filelock|gitlet --agents N --seed N
  gitlet-bench oracle --work DIR --tasks FILE --limit N RUNDIR...`)
	os.Exit(2)
}

type common struct {
	work, target, goCache string
}

func (c *common) flags(fs *flag.FlagSet) {
	fs.StringVar(&c.work, "work", "", "work directory (fixture, caches, runs)")
	fs.StringVar(&c.target, "target", "", "target Go module source to copy as the base")
	fs.StringVar(&c.goCache, "gocache", "", "Go build cache (default WORK/gocache)")
}

func (c *common) env() (*gitenv.Env, error) {
	if c.work == "" {
		return nil, fmt.Errorf("--work is required")
	}
	abs, err := filepath.Abs(c.work)
	if err != nil {
		return nil, err
	}
	c.work = abs
	if c.goCache == "" {
		c.goCache = filepath.Join(c.work, "gocache")
	}
	return gitenv.New(filepath.Join(c.work, "env"), c.goCache)
}

func gen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	var c common
	c.flags(fs)
	seed := fs.Uint64("seed", 1, "seed")
	n := fs.Int("tasks", 32, "tasks to generate")
	profile := fs.String("profile", "clustered", "contention profile: spread, clustered or hot")
	dup := fs.Float64("dup", 0.08, "share of duplicate tasks")
	changelog := fs.Bool("changelog", false, "every task adds a CHANGELOG entry")
	out := fs.String("out", "", "output file (default WORK/tasks-SEED.json)")
	check := fs.String("check", "vet", "how candidate tasks are validated alone: vet (go vet over the module) or build (library packages)")
	exclude := fs.String("exclude", "examples,conformance,internal/readme,internal/docs,internal/oauthtest,internal/testing", "directories no task targets")
	_ = fs.Parse(args)
	env, err := c.env()
	if err != nil {
		return err
	}
	ctx := context.Background()
	fx, err := sim.PrepareFixture(ctx, env, c.target, filepath.Join(c.work, "fixture"))
	if err != nil {
		return err
	}
	t, err := goedit.Load(fx.Dir)
	if err != nil {
		return err
	}
	cfg := sim.GenConfig{Tasks: *n, Seed: *seed, DupRate: *dup, DupWindow: 6, Changelog: *changelog, Mix: sim.DefaultMix, Exclude: strings.Split(*exclude, ","), HotSet: 12}
	switch *profile {
	case "spread":
		cfg.Skew, cfg.Focus = 1.0, 0
	case "clustered":
		cfg.Skew, cfg.Focus = 1.3, 0.3
	case "hot":
		cfg.Skew, cfg.Focus = 1.6, 0.6
	default:
		return fmt.Errorf("unknown profile %q", *profile)
	}
	// Generate extra, keep the first n that build on their own.
	var kept []*sim.Task
	for attempt := 0; len(kept) < *n && attempt < 4; attempt++ {
		cfg.Tasks = *n + *n/2 + 4
		cfg.Seed = *seed*1000 + uint64(attempt)
		tasks, err := sim.Generate(t, cfg)
		if err != nil {
			return err
		}
		validate := sim.CI
		if *check == "build" {
			validate = sim.Build
		}
		ok, bad := sim.ValidateTasks(ctx, env, fx, c.work, tasks, max(1, runtime.NumCPU()-1), validate)
		for id, why := range bad {
			fmt.Fprintf(os.Stderr, "dropped %s: %s\n", id, why)
		}
		// A duplicate whose original was dropped is dropped too.
		have := map[string]bool{}
		for _, tk := range ok {
			if tk.DupOf != "" && !have[tk.DupOf] {
				continue
			}
			have[tk.ID] = true
			kept = append(kept, tk)
			if len(kept) == *n {
				break
			}
		}
		if len(kept) < *n {
			kept = nil
		}
	}
	if len(kept) < *n {
		return fmt.Errorf("could not generate %d valid tasks", *n)
	}
	// Renumber in backlog order so ids read t01..tNN.
	renumber(kept)
	path := *out
	if path == "" {
		path = filepath.Join(c.work, fmt.Sprintf("tasks-%s-%d.json", *profile, *seed))
	}
	fmt.Fprintf(os.Stderr, "wrote %d tasks to %s\n", len(kept), path)
	return sim.WriteJSON(path, kept)
}

// renumber gives tasks ids t01.. in order and rewrites op tags, files and
// duplicate links to match.
func renumber(tasks []*sim.Task) {
	ids := map[string]string{}
	for i, t := range tasks {
		ids[t.ID] = fmt.Sprintf("T%02d", i+1)
	}
	for _, t := range tasks {
		old := t.ID
		t.ID = ids[old]
		if t.DupOf != "" {
			t.DupOf = ids[t.DupOf]
		}
		for i := range t.Ops {
			op := &t.Ops[i]
			for from, to := range ids {
				if strings.HasPrefix(op.Tag, from) && len(op.Tag) == len(from)+1 {
					op.Tag = strings.ToLower(to) + op.Tag[len(from):]
					op.File = strings.ReplaceAll(op.File, from+"_", strings.ToLower(to)+"_")
					break
				}
			}
		}
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var c common
	c.flags(fs)
	var cfg sim.Config
	tasksFile := fs.String("tasks", "", "tasks file from gen")
	limit := fs.Int("limit", 0, "use only the first N tasks of the backlog")
	fs.StringVar((*string)(&cfg.Mode), "mode", "git", "git, gitsync, filelock or gitlet")
	fs.IntVar(&cfg.Agents, "agents", 4, "agents")
	fs.Uint64Var(&cfg.Seed, "seed", 1, "seed for agent choices")
	fs.Float64Var(&cfg.Compliance, "compliance", 1, "gitlet: probability an agent acts on a note")
	fs.IntVar(&cfg.CIRounds, "ci-rounds", 2, "rounds a pull request waits for CI")
	fs.IntVar(&cfg.ResolveSpeed, "resolve-speed", 20, "conflict or fix lines an agent handles per round")
	fs.IntVar(&cfg.MaxRounds, "max-rounds", 400, "stop after this many rounds")
	fs.DurationVar(&cfg.RoundWall, "round-wall", 0, "minimum wall time per round (gitlet mode default 6s)")
	fs.StringVar(&cfg.Gitlet, "gitlet", "", "gitlet binary")
	fs.StringVar(&cfg.Name, "name", "", "run name (default mode-agents-seed)")
	fs.StringVar(&cfg.DupJudge, "dup-judge", "oracle", "gitlet: oracle or act")
	fs.StringVar(&cfg.Overlap, "overlap", "defer", "gitlet: defer on overlap notes and who, notes (defer on notes only) or ignore")
	fs.BoolVar(&cfg.Keep, "keep", false, "keep the run's clones after it ends")
	_ = fs.Parse(args)
	env, err := c.env()
	if err != nil {
		return err
	}
	var tasks []*sim.Task
	if err := sim.ReadJSON(*tasksFile, &tasks); err != nil {
		return err
	}
	if *limit > 0 && *limit < len(tasks) {
		tasks = tasks[:*limit]
	}
	cfg.Work = c.work
	cfg.TasksFile = *tasksFile
	ctx := context.Background()
	fx, err := sim.PrepareFixture(ctx, env, c.target, filepath.Join(c.work, "fixture"))
	if err != nil {
		return err
	}
	res, err := sim.Run(ctx, env, fx, tasks, cfg)
	if err != nil {
		return err
	}
	fmt.Println(res.Summary())
	return nil
}

// oracle judges runs' notes against the pairwise ground truth of their
// backlog, which it computes once per backlog and caches.
func oracle(args []string) error {
	fs := flag.NewFlagSet("oracle", flag.ExitOnError)
	var c common
	c.flags(fs)
	tasksFile := fs.String("tasks", "", "tasks file from gen")
	limit := fs.Int("limit", 0, "use only the first N tasks of the backlog")
	_ = fs.Parse(args)
	env, err := c.env()
	if err != nil {
		return err
	}
	var tasks []*sim.Task
	if err := sim.ReadJSON(*tasksFile, &tasks); err != nil {
		return err
	}
	if *limit > 0 && *limit < len(tasks) {
		tasks = tasks[:*limit]
	}
	cache := filepath.Join(c.work, fmt.Sprintf("truth-%s-%d.json", strings.TrimSuffix(filepath.Base(*tasksFile), ".json"), len(tasks)))
	var truths []sim.PairTruth
	if err := sim.ReadJSON(cache, &truths); err != nil {
		ctx := context.Background()
		fx, err := sim.PrepareFixture(ctx, env, c.target, filepath.Join(c.work, "fixture"))
		if err != nil {
			return err
		}
		if truths, err = sim.PairTruths(ctx, env, fx, c.work, tasks); err != nil {
			return err
		}
		sim.SortTruths(truths)
		if err := sim.WriteJSON(cache, truths); err != nil {
			return err
		}
	}
	out := map[string]*sim.Detection{}
	for _, dir := range fs.Args() {
		d, err := sim.Detect(dir, truths)
		if err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		out[filepath.Base(dir)] = d
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

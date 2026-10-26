package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/crewlet/gitlet/bench/internal/gitenv"
	"github.com/crewlet/gitlet/bench/internal/goedit"
)

// Fixture is the target repository at its base commit, kept as a git
// repository that runs clone from.
type Fixture struct {
	Dir  string // a git repository with the base on main
	Base string // base commit
}

const changelogBase = `# Changelog

## Unreleased

- Benchmark baseline.

## v1.8.0

- Released upstream.
`

// PrepareFixture copies the target source into dir as one commit. The
// source is copied without its .git directory.
func PrepareFixture(ctx context.Context, env *gitenv.Env, src, dir string) (*Fixture, error) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		base, err := env.Git(ctx, dir, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		return &Fixture{Dir: dir, Base: base}, nil
	}
	if err := copyTree(src, dir); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte(changelogBase), 0o644); err != nil {
		return nil, err
	}
	// gofmt everything once so that later reformatting of edited files
	// changes only the edited lines.
	if r, err := env.Run(ctx, dir, nil, "gofmt", "-l", "-w", "."); err != nil || r.Code != 0 {
		return nil, fmt.Errorf("gofmt fixture: %v %s", err, r.Stderr)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "base"}} {
		if _, err := env.Git(ctx, dir, args...); err != nil {
			return nil, err
		}
	}
	base, err := env.Git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	return &Fixture{Dir: dir, Base: base}, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".github") && p != src {
			return filepath.SkipDir
		}
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
}

// CI is the build check: go vet over the whole module, which type-checks
// every package and its tests. It returns the error lines.
func CI(ctx context.Context, env *gitenv.Env, dir string) (bool, []string, error) {
	r, err := env.Run(ctx, dir, nil, "go", "vet", "-trimpath", "./...")
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

// ValidateTasks applies each task alone to a fresh checkout of the base and
// keeps the ones that pass check: CI (go vet over the module) or, cheaper,
// Build.
func ValidateTasks(ctx context.Context, env *gitenv.Env, fx *Fixture, work string, tasks []*Task, workers int, check func(context.Context, *gitenv.Env, string) (bool, []string, error)) (ok []*Task, bad map[string]string) {
	bad = map[string]string{}
	var mu sync.Mutex
	jobs := make(chan int)
	var wg sync.WaitGroup
	good := make([]bool, len(tasks))
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir := filepath.Join(work, fmt.Sprintf("validate-%d", w))
			_ = os.RemoveAll(dir)
			if _, err := env.Git(ctx, fx.Dir, "worktree", "add", "-f", "--detach", dir, fx.Base); err != nil {
				panic(err)
			}
			for i := range jobs {
				reason := validateOne(ctx, env, dir, fx.Base, tasks[i], check)
				mu.Lock()
				if reason == "" {
					good[i] = true
				} else {
					bad[tasks[i].ID] = reason
				}
				mu.Unlock()
			}
		}()
	}
	for i := range tasks {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	for i, t := range tasks {
		if good[i] {
			ok = append(ok, t)
		}
	}
	return ok, bad
}

func validateOne(ctx context.Context, env *gitenv.Env, dir, base string, t *Task, check func(context.Context, *gitenv.Env, string) (bool, []string, error)) string {
	if _, err := env.Git(ctx, dir, "reset", "-q", "--hard", base); err != nil {
		return err.Error()
	}
	if _, err := env.Git(ctx, dir, "clean", "-qfdx"); err != nil {
		return err.Error()
	}
	gone, err := goedit.ApplyAll(dir, t.Ops, nil)
	if err != nil {
		return "apply: " + err.Error()
	}
	if len(gone) > 0 {
		return "gone: " + strings.Join(gone, ",")
	}
	pass, errs, err := check(ctx, env, dir)
	if err != nil {
		return err.Error()
	}
	if !pass {
		return "check: " + strings.Join(errs[:min(3, len(errs))], " | ")
	}
	return ""
}

// WriteJSON writes v to path.
func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// ReadJSON reads path into v.
func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// LookPath finds a binary or panics.
func LookPath(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		panic(err)
	}
	return p
}

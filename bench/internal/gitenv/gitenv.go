// Package gitenv runs git, go and gitlet for the benchmark in an environment
// isolated from the user's own git configuration, so that signing, push
// negotiation or identity settings on the host cannot change the results.
package gitenv

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Env is one isolated environment: a private global git config, a fixed
// identity and a shared Go build cache.
type Env struct {
	Dir      string // holds gitconfig
	GoCache  string
	extra    []string
	Identity string
}

// New writes the private git config under dir and returns the environment.
func New(dir, goCache string) (*Env, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	cfg := `[user]
	name = bench
	email = bench@example.invalid
[commit]
	gpgsign = false
[tag]
	gpgsign = false
[init]
	defaultBranch = main
[advice]
	detachedHead = false
	skippedCherryPicks = false
[rebase]
	autoStash = false
[merge]
	conflictStyle = merge
[core]
	autocrlf = false
[gc]
	auto = 0
[maintenance]
	auto = false
`
	path := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		return nil, err
	}
	e := &Env{Dir: dir, GoCache: goCache}
	e.extra = []string{
		"GIT_CONFIG_GLOBAL=" + path,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
		"LC_ALL=C",
	}
	if goCache != "" {
		e.extra = append(e.extra, "GOCACHE="+goCache)
	}
	return e, nil
}

// Environ returns the process environment for a command, with the given
// additions. Host variables that would leak the user's git setup are dropped.
func (e *Env) Environ(add ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "GIT_CONFIG_"), strings.HasPrefix(k, "GIT_AUTHOR_"),
			strings.HasPrefix(k, "GIT_COMMITTER_"), k == "GIT_DIR", k == "GIT_WORK_TREE",
			k == "GIT_INDEX_FILE", k == "GITLET_AGENT", k == "GOFLAGS", k == "GOCACHE",
			k == "GH_TOKEN", k == "GITHUB_TOKEN":
			continue
		}
		out = append(out, kv)
	}
	out = append(out, e.extra...)
	return append(out, add...)
}

// Result is the outcome of one command.
type Result struct {
	Stdout   string
	Stderr   string
	Code     int
	Duration time.Duration
}

// Run runs name with args in dir. A non-zero exit is not an error; only a
// failure to start, or a timeout, is.
func (e *Env) Run(ctx context.Context, dir string, env []string, name string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = e.Environ(env...)
	cmd.Stdin = nil
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	cmd.WaitDelay = 5 * time.Second
	start := time.Now()
	err := cmd.Run()
	r := Result{Stdout: out.String(), Stderr: errb.String(), Duration: time.Since(start)}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			r.Code = ee.ExitCode()
			if ctx.Err() != nil {
				return r, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), ctx.Err())
			}
			return r, nil
		}
		return r, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return r, nil
}

// Git runs git in dir and returns trimmed stdout, failing on a non-zero exit.
func (e *Env) Git(ctx context.Context, dir string, args ...string) (string, error) {
	r, err := e.Run(ctx, dir, nil, "git", args...)
	if err != nil {
		return "", err
	}
	if r.Code != 0 {
		return "", fmt.Errorf("git %s: exit %d: %s", strings.Join(args, " "), r.Code, strings.TrimSpace(r.Stderr+r.Stdout))
	}
	return strings.TrimSpace(r.Stdout), nil
}

// MustGit is Git for setup steps where any failure is a harness bug.
func (e *Env) MustGit(ctx context.Context, dir string, args ...string) string {
	out, err := e.Git(ctx, dir, args...)
	if err != nil {
		panic(err)
	}
	return out
}

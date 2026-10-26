package hook

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// helperEnv makes the test binary run Run with its arguments and exit, so
// that real git commands can call it as a hook or sequence editor.
const helperEnv = "GITLET_HOOK_TEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(Run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(runTests(m))
}

// runTests makes git hermetic: a private global config, no system config,
// and nothing inherited from a git process that runs the tests.
func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "gitlet-hook-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	cfg := filepath.Join(dir, "gitconfig")
	const config = "[user]\n\tname = gitlet test\n\temail = test@example.com\n" +
		"[init]\n\tdefaultBranch = main\n" +
		"[commit]\n\tgpgsign = false\n[tag]\n\tgpgsign = false\n"
	if err := os.WriteFile(cfg, []byte(config), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	os.Setenv("GIT_CONFIG_GLOBAL", cfg)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_EDITOR", "GIT_SEQUENCE_EDITOR", "GITLET_AGENT", "GITLET_DROP",
	} {
		os.Unsetenv(k)
	}
	return m.Run()
}

// gitIn runs git in dir with extra environment and returns its trimmed
// stdout, failing the test on error.
func gitIn(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	out, errOut, err := gitRun(dir, env, nil, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, errOut)
	}
	return out
}

// gitRun runs git and returns its trimmed stdout and stderr.
func gitRun(dir string, env []string, stdin []byte, args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return strings.TrimSpace(o.String()), e.String(), err
}

// newGitRepo creates a repository with one commit on main and returns its
// work tree, with symlinks resolved as git reports it.
func newGitRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, nil, "init", "-q")
	commitFile(t, dir, "README", "hello\n", "Initial commit")
	return dir
}

// commitFile writes a file and commits it with git commit -m.
func commitFile(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, nil, "add", name)
	gitIn(t, dir, nil, "commit", "-q", "-m", msg)
}

// helperCommand returns a shell command that runs the test binary as
// gitlet hook with args.
func helperCommand(t *testing.T, args ...string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return helperEnv + "=1 " + shellQuote(exe) + " " + strings.Join(args, " ")
}

// installHook installs the test binary as the git hook name in dir.
func installHook(t *testing.T, dir, name string) {
	t.Helper()
	hooks := filepath.Join(gitIn(t, dir, nil, "rev-parse", "--path-format=absolute", "--git-common-dir"), "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec env " + helperCommand(t, "git", name) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(hooks, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

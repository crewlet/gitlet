package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/gitlet/internal/cli"
)

// asGitletEnv makes the test binary run as the gitlet command. update
// rebases with GIT_SEQUENCE_EDITOR set to "<gitlet> hook sequence-editor",
// and the engine's default gitlet binary is os.Executable(): this test
// binary. TestMain sets the variable for the processes git starts, so the
// sequence editor really is gitlet's own command line.
const asGitletEnv = "GITLET_E2E_AS_GITLET"

func TestMain(m *testing.M) {
	if os.Getenv(asGitletEnv) == "1" {
		os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(runTests(m))
}

// runTests makes every git command hermetic: a private global
// configuration, no system configuration, and none of the caller's GIT_* or
// GITLET_* variables. The clones also get their own identity (see
// newFixture), so nothing depends on the developer's git setup.
func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "gitlet-e2e-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	cfg := filepath.Join(dir, "gitconfig")
	const gitconfig = "[user]\n\tname = gitlet e2e\n\temail = e2e@example.invalid\n" +
		"[init]\n\tdefaultBranch = main\n" +
		"[commit]\n\tgpgsign = false\n[tag]\n\tgpgsign = false\n"
	if err := os.WriteFile(cfg, []byte(gitconfig), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") || strings.HasPrefix(k, "GITLET_") {
			os.Unsetenv(k)
		}
	}
	os.Setenv("GIT_CONFIG_GLOBAL", cfg)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Setenv("GIT_TERMINAL_PROMPT", "0")
	os.Setenv(asGitletEnv, "1")
	// Runs of this binary as gitlet exit at once under the race detector,
	// which otherwise sleeps a second at exit.
	os.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	return m.Run()
}

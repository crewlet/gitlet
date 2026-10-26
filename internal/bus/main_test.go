package bus

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain makes git hermetic for the integration tests: a private global
// config, no system config, and no repository or identity leaking in from
// the environment.
func TestMain(m *testing.M) { os.Exit(runTests(m)) }

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "gitlet-bus-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	cfg := filepath.Join(dir, "gitconfig")
	content := "[user]\n\tname = gitlet test\n\temail = test@gitlet.invalid\n" +
		"[init]\n\tdefaultBranch = main\n" +
		"[commit]\n\tgpgsign = false\n" +
		"[tag]\n\tgpgsign = false\n"
	if err := os.WriteFile(cfg, []byte(content), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	os.Setenv("GIT_CONFIG_GLOBAL", cfg)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS",
		"GIT_CONFIG_COUNT", "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE",
		"GIT_SSH", "GIT_SSH_COMMAND",
	} {
		os.Unsetenv(k)
	}
	return m.Run()
}

// Package install sets up agent clients and git hooks to use gitlet
// (docs/SPEC.md, section 14.3): MCP server entries, the agent skill
// (embedded), Claude Code hooks, and optional git hooks. It merges into
// existing files instead of overwriting unrelated settings.
package install

import "context"

// Client names accepted by Install.
const (
	ClaudeCode = "claude-code"
	Codex      = "codex"
	Cursor     = "cursor"
)

// Options configures Install.
type Options struct {
	Root     string   // worktree root
	Clients  []string // default: Detect(Root), or ClaudeCode when nothing is detected
	GitHooks bool     // also install prepare-commit-msg, post-commit and pre-push
	HooksDir string   // git hooks directory (gitx.Repo.HooksDir); required with GitHooks
	Binary   string   // command used to start gitlet; default "gitlet"
	DryRun   bool
}

// Change is one file Install wrote or would write.
type Change struct {
	Path    string
	Action  string // "create", "update", "unchanged", "skip"
	Summary string // for "skip": the line to add by hand
}

// Install writes client configuration and hooks.
func Install(ctx context.Context, opt Options) ([]Change, error) { return install(ctx, opt) }

// Detect returns the clients that look configured in root (.claude/,
// .mcp.json, .codex/, AGENTS.md, .cursor/).
func Detect(root string) []string { return detect(root) }

// Skill returns the embedded agent skill (SKILL.md).
func Skill() string { return skill }

// Package hook implements hook entry points (docs/SPEC.md, section 14.2):
// Claude Code hooks, git hooks, and the rebase sequence editor used by
// update. Hooks depend only on gitx, ledger and render: they never open an
// engine and never contact the remote, so they stay fast. Client and git
// hooks never fail the host: errors go to stderr and the exit code is 0.
package hook

import (
	"context"
	"io"
)

// Run dispatches on args:
//
//	claude-code session-start|user-prompt-submit|post-tool-use
//	    reads the event JSON from stdin, uses its cwd, and prints
//	    {"hookSpecificOutput":{"hookEventName":...,"additionalContext":...}}
//	    with new notes (a short status for session-start). post-tool-use
//	    skips gitlet's own tools (tool_name mcp__gitlet__*).
//	git prepare-commit-msg <file> [source] [sha]
//	    adds Gitlet-Item and Gitlet-Agent trailers when the worktree has an item.
//	git post-commit
//	    prints new act notes to stderr.
//	git pre-push <remote> <url>
//	    prints the submit checks it can make offline to stderr.
//	sequence-editor <todo-file>
//	    rewrites a rebase todo: "pick <oid>" lines whose commit is listed in
//	    GITLET_DROP (space-separated oids or prefixes) become "drop <oid>".
//	    Its errors do fail, so a broken todo aborts the rebase.
//
// It returns the process exit code.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return run(ctx, args, stdin, stdout, stderr)
}

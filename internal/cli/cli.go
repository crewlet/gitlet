// Package cli is gitlet's command line, built with cobra. Verb commands are
// generated from api.Verbs (docs/SPEC.md, section 4.1): required arguments
// are positional in table order, every other argument is a long flag of the
// same name, and --json prints the structured result. Output is the same
// text the MCP tools return, with options written as commands.
//
// CLI-only commands: mcp, init, hook, doctor, version.
package cli

import "io"

// Build information, set through ldflags.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Main runs the CLI with args (without the program name) and returns the
// process exit code: 0 for ok and nothing_to_do, 1 for errors, 2 for usage
// errors, 3 for refusals.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return run(args, stdin, stdout, stderr)
}

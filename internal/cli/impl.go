package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/crewlet/gitlet/internal/api"
)

// Exit codes (see Main).
const (
	exitOK      = 0
	exitError   = 1
	exitUsage   = 2
	exitRefused = 3
)

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := &app{deps: defaultDeps(), stdin: stdin, stdout: stdout, stderr: stderr}
	return a.main(ctx, args)
}

// app is one run of the command line.
type app struct {
	deps   deps
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// exitErr ends a command with an exit code. A nil err means the command
// already said what it had to say.
type exitErr struct {
	code int
	err  error
}

func (e *exitErr) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit %d", e.code)
	}
	return e.err.Error()
}

func (e *exitErr) Unwrap() error { return e.err }

// failed is a runtime error (exit 1).
func failed(err error) error { return &exitErr{code: exitError, err: err} }

// usageErr is a mistake in how a command was called (exit 2). The message
// is followed by the command's correct form.
type usageErr struct {
	cmd *cobra.Command
	msg string
}

func (e *usageErr) Error() string { return e.msg }

func usagef(cmd *cobra.Command, format string, args ...any) error {
	return &usageErr{cmd: cmd, msg: fmt.Sprintf(format, args...)}
}

func (a *app) main(ctx context.Context, args []string) int {
	if args == nil {
		args = []string{} // cobra reads os.Args when args are nil
	}
	root := a.rootCommand()
	root.SetArgs(args)
	cmd, err := root.ExecuteContextC(ctx)
	return a.exit(cmd, err)
}

// exit prints err and returns the exit code for it. Errors that come from
// cobra itself (flag parsing) are usage errors.
func (a *app) exit(cmd *cobra.Command, err error) int {
	if err == nil {
		return exitOK
	}
	var ee *exitErr
	if errors.As(err, &ee) {
		if ee.err != nil {
			fmt.Fprintf(a.stderr, "gitlet: %s\n", message(ee.err))
		}
		return ee.code
	}
	var ue *usageErr
	if errors.As(err, &ue) {
		cmd = ue.cmd
	}
	a.printUsageError(cmd, err.Error())
	return exitUsage
}

// message is err's text for people: the "cli: " context is for logs and
// wrapping, not for the terminal.
func message(err error) string { return strings.TrimPrefix(err.Error(), "cli: ") }

func (a *app) printUsageError(cmd *cobra.Command, msg string) {
	path := "gitlet"
	if cmd != nil {
		path = cmd.CommandPath()
	}
	fmt.Fprintf(a.stderr, "%s: %s\n", path, msg)
	if strings.Contains(msg, "unknown shorthand flag") {
		fmt.Fprintln(a.stderr, `hint: put -- before an argument that starts with "-"`)
	}
	if cmd == nil || !cmd.HasParent() {
		fmt.Fprintln(a.stderr, `Run "gitlet" to list the commands.`)
		return
	}
	if u := cmd.Annotations[annUsage]; u != "" {
		fmt.Fprintf(a.stderr, "usage: %s\n", u)
	} else {
		fmt.Fprintf(a.stderr, "usage: %s\n", cmd.UseLine())
	}
	if ex := cmd.Annotations[annExample]; ex != "" {
		fmt.Fprintf(a.stderr, "example: %s\n", ex)
	}
}

// Command annotations.
const (
	annUsage   = "gitlet.usage"   // the full correct form, shown with usage errors
	annExample = "gitlet.example" // one example call
	annVerb    = "gitlet.verb"    // set on commands generated from api.Verbs
)

// group is a section of the overview.
type group struct {
	title string
	cmds  []string
}

// groups orders the overview by purpose. Verbs missing here are listed
// under "More verbs", so a new verb in the table still shows up.
var groups = []group{
	{"Start and orient", []string{"start", "status", "who", "why"}},
	{"Own and depend on interfaces", []string{"claim", "publish", "need", "use", "release"}},
	{"Talk it through", []string{"discuss", "accept", "dismiss"}},
	{"Land your work", []string{"update", "submit", "abandon"}},
	{"Set up", []string{"init", "mcp", "doctor", "hook", "version"}},
}

const intro = `gitlet lets AI coding agents and the people next to them work on one git
repository at the same time: it shows overlapping work early and turns
contested interfaces into contracts.`

func (a *app) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "gitlet",
		Short:         "Coordinate AI coding agents working on one git repository",
		Long:          intro,
		Version:       Version,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			msg := fmt.Sprintf("unknown command %q", args[0])
			if s := cmd.SuggestionsFor(args[0]); len(s) > 0 {
				msg += fmt.Sprintf("; did you mean %s?", strings.Join(s, " or "))
			}
			return usagef(cmd, "%s", msg)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			a.printOverview(cmd)
			return nil
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetVersionTemplate(versionLine() + "\n")
	root.SetIn(a.stdin)
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &usageErr{cmd: cmd, msg: err.Error()}
	})
	defaultHelp := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if !cmd.HasParent() {
			a.printOverview(cmd)
			return
		}
		defaultHelp(cmd, args)
	})

	for _, v := range api.Verbs {
		root.AddCommand(a.verbCommand(v))
	}
	root.AddCommand(a.initCommand(), a.mcpCommand(), a.doctorCommand(), a.hookCommand(), a.versionCommand())
	return root
}

// printOverview lists the commands grouped by purpose, each with the first
// sentence of its short help.
func (a *app) printOverview(root *cobra.Command) {
	byName := make(map[string]*cobra.Command)
	for _, c := range root.Commands() {
		byName[c.Name()] = c
	}
	listed := make(map[string]bool)
	sections := append([]group(nil), groups...)
	var more []string
	for _, v := range api.Verbs {
		if !inGroups(v.Name) {
			more = append(more, v.Name)
		}
	}
	if len(more) > 0 {
		sections = append(sections, group{"More verbs", more})
	}

	w := a.stdout
	fmt.Fprintf(w, "%s\n\nUsage: gitlet <command> [arguments] [flags]\n", intro)
	for _, g := range sections {
		fmt.Fprintf(w, "\n%s:\n", g.title)
		for _, name := range g.cmds {
			c := byName[name]
			if c == nil || listed[name] {
				continue
			}
			listed[name] = true
			fmt.Fprintf(w, "  %-9s %s\n", name, wrap(firstSentence(c.Short), helpWidth-12, 12))
		}
	}
	fmt.Fprint(w, `
Verbs take --json to print the structured result and --worktree to act on
another worktree. Run "gitlet <command> --help" for its arguments and an example.
`)
}

func inGroups(name string) bool {
	for _, g := range groups {
		for _, c := range g.cmds {
			if c == name {
				return true
			}
		}
	}
	return false
}

// firstSentence returns s up to and including its first full stop.
func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i >= 0 {
		return s[:i+1]
	}
	return s
}

func versionLine() string {
	s := "gitlet " + Version
	var extra []string
	if Commit != "" {
		extra = append(extra, "commit "+Commit)
	}
	if Date != "" {
		extra = append(extra, "built "+Date)
	}
	if len(extra) > 0 {
		s += " (" + strings.Join(extra, ", ") + ")"
	}
	return s
}

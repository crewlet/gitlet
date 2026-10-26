package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/bus"
	"github.com/crewlet/gitlet/internal/config"
	"github.com/crewlet/gitlet/internal/engine"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/hook"
	"github.com/crewlet/gitlet/internal/install"
	"github.com/crewlet/gitlet/internal/ledger"
	"github.com/crewlet/gitlet/internal/mcpserver"
	"github.com/crewlet/gitlet/internal/model"
)

// verbEngine is the part of *engine.Engine a verb command uses.
type verbEngine interface {
	Call(ctx context.Context, verb string, input any) (api.Output, error)
	Close() error
}

// deps are the CLI's links to the rest of gitlet; tests replace them.
type deps struct {
	openEngine func(ctx context.Context, opt engine.Options) (verbEngine, error)
	openRepo   func(ctx context.Context, dir string) (*gitx.Repo, error)
	install    func(ctx context.Context, opt install.Options) ([]install.Change, error)
	hook       func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int
	serve      func(ctx context.Context, opt mcpserver.Options, stdin io.Reader, stdout io.Writer) error
	// ghStatus reports whether gh is installed and signed in.
	ghStatus func(ctx context.Context) (installed, signedIn bool)
}

func defaultDeps() deps {
	return deps{
		openEngine: func(ctx context.Context, opt engine.Options) (verbEngine, error) {
			e, err := engine.Open(ctx, opt)
			if err != nil {
				return nil, err
			}
			return e, nil
		},
		openRepo: gitx.Open,
		install:  install.Install,
		hook:     hook.Run,
		serve:    serveMCP,
		ghStatus: ghStatus,
	}
}

// serveMCP is mcpserver.Serve on the streams Main was given instead of the
// process's own stdin and stdout.
func serveMCP(ctx context.Context, opt mcpserver.Options, stdin io.Reader, stdout io.Writer) error {
	t := &mcp.IOTransport{Reader: io.NopCloser(stdin), Writer: nopWriteCloser{stdout}}
	return mcpserver.New(opt).Run(ctx, t)
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usagef(cmd, "takes no arguments, got %s", quoteAll(args))
	}
	return nil
}

func repoError(dir string, err error) error {
	if errors.Is(err, gitx.ErrNotRepo) {
		return fmt.Errorf("cli: %s is not inside a git work tree; run gitlet in a repository or pass --worktree", dir)
	}
	return fmt.Errorf("cli: open repository at %s: %w", dir, err)
}

// ---- mcp ----

func (a *app) mcpCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run the MCP server on stdin and stdout (agent clients start it).",
		Long: `Run gitlet's MCP server on stdin and stdout. Agent clients start it from the
MCP configuration that gitlet init writes. One server runs per agent session;
it serves the worktree it starts in, and any other worktree a tool call names.
Logs go to stderr. GITLET_AGENT overrides the agent id of the default worktree.`,
		Args: noArgs,
		Annotations: map[string]string{
			annUsage:   "gitlet mcp [--worktree <dir>]",
			annExample: "gitlet mcp",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := worktreeDir(cmd.Flags())
			if err != nil {
				return failed(err)
			}
			agent, err := agentOverride()
			if err != nil {
				return failed(err)
			}
			opt := mcpserver.Options{
				Dir:     dir,
				Version: Version,
				Agent:   agent,
				Logger:  slog.New(slog.NewTextHandler(a.stderr, nil)),
			}
			if err := a.deps.serve(cmd.Context(), opt, a.stdin, a.stdout); err != nil {
				return failed(fmt.Errorf("cli: mcp: %w", err))
			}
			return nil
		},
	}
	cmd.Flags().String("worktree", "", "default worktree (default: the current directory)")
	return cmd
}

// ---- hook ----

func (a *app) hookCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "hook <kind> <event> [args...]",
		Short: "Hook entry points for agent clients and git (not for direct use).",
		Long: `Hook entry points, called by agent clients and git (docs/SPEC.md, section 14.2):

  gitlet hook claude-code session-start|user-prompt-submit|post-tool-use
  gitlet hook git prepare-commit-msg|post-commit|pre-push [args...]
  gitlet hook sequence-editor <todo-file>

gitlet init installs them. Client and git hooks never fail the host.`,
		DisableFlagParsing: true,
		Annotations: map[string]string{
			annUsage: "gitlet hook <kind> <event> [args...]",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if code := a.deps.hook(cmd.Context(), args, a.stdin, a.stdout, a.stderr); code != 0 {
				return &exitErr{code: code}
			}
			return nil
		},
	}
}

// ---- version ----

func (a *app) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "version",
		Short:       "Print the gitlet version.",
		Args:        noArgs,
		Annotations: map[string]string{annUsage: "gitlet version"},
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintln(a.stdout, versionLine())
			return err
		},
	}
}

// ---- init ----

var knownClients = []string{install.ClaudeCode, install.Codex, install.Cursor}

func (a *app) initCommand() *cobra.Command {
	var clients []string
	var gitHooks, dryRun bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up agent clients and git hooks for this repository.",
		Long: `Set up agent clients and git hooks for this repository:

  claude-code  .mcp.json, the skill in .claude/skills/gitlet/SKILL.md and hooks
               in .claude/settings.json
  codex        .codex/config.toml and a gitlet section in AGENTS.md
  cursor       .cursor/mcp.json and .cursor/rules/gitlet.mdc

Without --client, init sets up the clients it detects, or Claude Code when it
detects none. Existing files are merged, never overwritten. With --git-hooks it
also installs prepare-commit-msg, post-commit and pre-push; a hook that is not
gitlet's is left alone and init prints the line to add to it.`,
		Example: "  gitlet init\n  gitlet init --client claude-code --client codex --git-hooks\n  gitlet init --dry-run",
		Args:    noArgs,
		Annotations: map[string]string{
			annUsage:   "gitlet init [--client claude-code|codex|cursor]... [--git-hooks] [--dry-run] [--worktree <dir>]",
			annExample: "gitlet init --client claude-code --git-hooks",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, c := range clients {
				if !slices.Contains(knownClients, c) {
					return usagef(cmd, "unknown client %q; use %s", c, orList(knownClients, "or"))
				}
			}
			return a.runInit(cmd, install.Options{Clients: clients, GitHooks: gitHooks, DryRun: dryRun})
		},
	}
	fs := cmd.Flags()
	fs.SortFlags = false
	fs.StringSliceVar(&clients, "client", nil, "client to set up: "+orList(knownClients, "or")+" (repeat the flag or separate with commas; default: detected)")
	fs.BoolVar(&gitHooks, "git-hooks", false, "also install the prepare-commit-msg, post-commit and pre-push hooks")
	fs.BoolVar(&dryRun, "dry-run", false, "print the changes without writing them")
	fs.String("worktree", "", "worktree to set up (default: the current directory)")
	return cmd
}

func (a *app) runInit(cmd *cobra.Command, opt install.Options) error {
	ctx := cmd.Context()
	dir, err := worktreeDir(cmd.Flags())
	if err != nil {
		return failed(err)
	}
	repo, err := a.deps.openRepo(ctx, dir)
	if err != nil {
		return failed(repoError(dir, err))
	}
	opt.Root = repo.Root
	if opt.GitHooks {
		if opt.HooksDir, err = repo.HooksDir(ctx); err != nil {
			return failed(fmt.Errorf("cli: init: find the git hooks directory: %w", err))
		}
	}
	changes, err := a.deps.install(ctx, opt)
	if err != nil {
		return failed(fmt.Errorf("cli: init: %w", err))
	}
	written := false
	for _, c := range changes {
		line := fmt.Sprintf("%-9s  %s", c.Action, relTo(repo.Root, c.Path))
		if c.Summary != "" {
			line += ": " + c.Summary
		}
		fmt.Fprintln(a.stdout, line)
		written = written || c.Action == "create" || c.Action == "update"
	}
	switch {
	case opt.DryRun:
		fmt.Fprintln(a.stdout, "Dry run: nothing was written.")
	case written:
		fmt.Fprintln(a.stdout, "Restart running agent sessions so they load gitlet.")
	}
	return nil
}

// relTo shows path relative to root when it lies inside it.
func relTo(root, path string) string {
	if !filepath.IsAbs(path) {
		return path
	}
	if rel, err := filepath.Rel(root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel
	}
	return path
}

// ---- doctor ----

// check is one line of doctor's report.
type check struct {
	status string // ok, warn, fail or skip
	what   string
	detail string
}

func (a *app) doctorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check git, the remote, gh, the ledger, client configs and ref access.",
		Long: `Check the setup: git's version (2.38 or later for merge checks), the remote and
main branch, the local ledger, the agent client configs, gh, and ref access on
the remote: a create-only push of refs/gitlet/probe/<agent>, a non-fast-forward
update, then a delete. Exits 1 when a check fails.`,
		Args: noArgs,
		Annotations: map[string]string{
			annUsage:   "gitlet doctor [--worktree <dir>]",
			annExample: "gitlet doctor",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := worktreeDir(cmd.Flags())
			if err != nil {
				return failed(err)
			}
			checks := a.doctor(cmd.Context(), dir)
			fails, warns := 0, 0
			for _, c := range checks {
				fmt.Fprintf(a.stdout, "%-4s  %-10s  %s\n", c.status, c.what, c.detail)
				switch c.status {
				case "fail":
					fails++
				case "warn":
					warns++
				}
			}
			switch {
			case fails > 0:
				fmt.Fprintf(a.stdout, "%d check%s failed.\n", fails, plural(fails))
				return &exitErr{code: exitError}
			case warns > 0:
				fmt.Fprintf(a.stdout, "No failures, %d warning%s.\n", warns, plural(warns))
			default:
				fmt.Fprintln(a.stdout, "All checks passed.")
			}
			return nil
		},
	}
	cmd.Flags().String("worktree", "", "worktree to check (default: the current directory)")
	return cmd
}

// doctor runs every check it can. Checks that need the repository are
// skipped when it cannot be opened.
func (a *app) doctor(ctx context.Context, dir string) []check {
	var checks []check
	add := func(status, what, format string, args ...any) {
		checks = append(checks, check{status, what, fmt.Sprintf(format, args...)})
	}
	if repo, err := a.deps.openRepo(ctx, dir); err != nil {
		add("fail", "repository", "%s", message(repoError(dir, err)))
	} else {
		add("ok", "repository", "%s", repo.Root)
		a.doctorRepo(ctx, repo, add)
	}

	switch installed, signedIn := a.deps.ghStatus(ctx); {
	case !installed:
		add("warn", "gh", "not installed: submit returns a compare URL instead of opening a pull request")
	case !signedIn:
		add("warn", "gh", "not signed in (run gh auth login): submit returns a compare URL until then")
	default:
		add("ok", "gh", "installed and signed in")
	}
	return checks
}

func (a *app) doctorRepo(ctx context.Context, repo *gitx.Repo, add func(status, what, format string, args ...any)) {
	switch major, minor, err := repo.Version(ctx); {
	case err != nil:
		add("fail", "git", "cannot read git's version: %v", err)
	case major > gitx.MinMajor || major == gitx.MinMajor && minor >= gitx.MinMinor:
		add("ok", "git", "%d.%d", major, minor)
	default:
		add("warn", "git", "%d.%d is older than %d.%d: predicted conflicts and the merge checks of update and submit are off",
			major, minor, gitx.MinMajor, gitx.MinMinor)
	}

	cfg, cfgErr := config.Load(ctx, repo)
	switch {
	case cfgErr != nil:
		add("fail", "config", "%v", cfgErr)
	case cfg.Remote == "":
		add("warn", "remote", "none: local mode, for agents in worktrees of this clone only (main branch %s)", cfg.Main)
	default:
		url, err := repo.RemoteURL(ctx, cfg.Remote)
		if err != nil {
			add("fail", "remote", "%s: %v", cfg.Remote, err)
		} else {
			add("ok", "remote", "%s %s, main branch %s", cfg.Remote, url, cfg.Main)
		}
	}

	agent := model.AgentID("")
	path := filepath.Join(repo.CommonDir, "gitlet", "ledger.db")
	if led, err := ledger.Open(ctx, path, ledger.Options{BusyTimeout: 2 * time.Second}); err != nil {
		add("fail", "ledger", "cannot open %s: %v", path, err)
	} else {
		if led.ReadOnly() {
			add("warn", "ledger", "%s was written by a newer gitlet; this one can only read it", path)
		} else {
			add("ok", "ledger", "%s", path)
		}
		if id, ok, err := led.AgentFor(ctx, repo.Root); err == nil && ok {
			agent = id
		}
		if err := led.Close(); err != nil {
			add("warn", "ledger", "close %s: %v", path, err)
		}
	}

	a.doctorClients(ctx, repo.Root, add)

	switch {
	case cfgErr != nil:
		add("skip", "ref access", "the configuration does not load")
	case cfg.Remote == "":
		add("skip", "ref access", "no remote to probe")
	default:
		a.doctorProbe(ctx, repo, cfg, agent, add)
	}
}

// doctorClients asks install what gitlet init would change; nothing means
// the client configs are in place.
func (a *app) doctorClients(ctx context.Context, root string, add func(status, what, format string, args ...any)) {
	changes, err := a.deps.install(ctx, install.Options{Root: root, DryRun: true})
	if err != nil {
		add("warn", "clients", "cannot check the client configs: %v", err)
		return
	}
	var pending []string
	for _, c := range changes {
		if c.Action == "create" || c.Action == "update" {
			pending = append(pending, c.Action+" "+relTo(root, c.Path))
		}
	}
	clients := install.Detect(root)
	if len(clients) == 0 {
		clients = []string{install.ClaudeCode}
	}
	if len(pending) > 0 {
		add("warn", "clients", "gitlet init would %s: run gitlet init", strings.Join(pending, ", "))
		return
	}
	add("ok", "clients", "%s set up", strings.Join(clients, ", "))
}

// doctorProbe tests ref access on the remote through the bus. The engine has
// no probe of its own, so doctor builds a bus for the worktree's agent (a
// throwaway id when it has none yet) with a push log in memory: the probe
// ref is deleted at the end and leaves nothing to remember.
func (a *app) doctorProbe(ctx context.Context, repo *gitx.Repo, cfg config.Config, agent model.AgentID, add func(status, what, format string, args ...any)) {
	if agent == "" {
		host, err := os.Hostname()
		if err != nil {
			host = "host"
		}
		agent = model.NewAgentID("doctor", host)
	}
	b := bus.New(repo, agent, bus.Options{
		Remote:  cfg.Remote,
		Main:    cfg.Main,
		LockDir: filepath.Join(repo.CommonDir, "gitlet"),
		PushLog: memPushLog{},
	})
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := b.Probe(ctx)
	switch {
	case gitx.IsNetwork(err):
		add("fail", "ref access", "%s is unreachable: %v", cfg.Remote, err)
	case gitx.IsPermission(err):
		add("fail", "ref access", "%s refuses gitlet's refs, so gitlet will run read-only: %v", cfg.Remote, err)
	case err != nil:
		add("fail", "ref access", "%v", err)
	case res.Create && res.NonFastForward && res.Delete:
		add("ok", "ref access", "create, non-fast-forward update and delete of %s%s work", bus.ProbePrefix, agent)
	default:
		detail := fmt.Sprintf("create %s, non-fast-forward update %s, delete %s", works(res.Create), works(res.NonFastForward), works(res.Delete))
		if res.Detail != "" {
			detail += ": " + res.Detail
		}
		add("fail", "ref access", "%s", detail)
	}
}

func works(ok bool) string {
	if ok {
		return "works"
	}
	return "refused"
}

// memPushLog is a bus.PushLog that forgets everything when doctor exits.
type memPushLog map[string]string

func (m memPushLog) LastPushed(_ context.Context, ref string) (string, error) { return m[ref], nil }

func (m memPushLog) SetLastPushed(_ context.Context, ref, oid string) error {
	if oid == "" {
		delete(m, ref)
	} else {
		m[ref] = oid
	}
	return nil
}

// ghStatus runs gh auth status, which exits 0 when gh is signed in.
func ghStatus(ctx context.Context) (installed, signedIn bool) {
	path, err := exec.LookPath("gh")
	if err != nil {
		return false, false
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "auth", "status")
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "NO_COLOR=1")
	return true, cmd.Run() == nil
}

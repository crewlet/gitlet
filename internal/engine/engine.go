// Package engine implements gitlet's verbs (docs/SPEC.md, sections 4 to 12)
// on top of git, the ref bus, the footprints, the rules and the ledger. The
// CLI, the MCP server and the tests all go through it, so behaviour is the
// same everywhere.
//
// An Engine serves one agent in one worktree. It is safe for concurrent use:
// verbs and Tick are serialized internally, and every write to the agent's
// own refs happens under the per-agent file lock, so CLI and server
// processes for the same worktree never lose each other's updates.
package engine

import (
	"context"
	"log/slog"
	"time"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/config"
	"github.com/crewlet/gitlet/internal/model"
)

// Options configures Open.
type Options struct {
	Dir    string        // any directory inside the worktree
	Client string        // "claude-code", "codex", "cursor", "cli", ...
	Agent  model.AgentID // overrides the stored or generated id (GITLET_AGENT)
	Now    func() time.Time
	Config *config.Config // overrides git config, for tests
	// Session makes the engine take the worktree's session lock for its
	// lifetime (the MCP server). When another live session holds it, the
	// engine runs read-only and adds a same_worktree note.
	Session bool
	// Offline never contacts the remote (tests, hooks).
	Offline bool
	// Upkeep makes verbs do a tick's upkeep (settled decisions, automatic
	// claims, renewals, landing) when no session is live for the worktree,
	// because nobody ticks then (the CLI).
	Upkeep bool
	// Exe is the gitlet binary used for GIT_SEQUENCE_EDITOR during update;
	// default os.Executable().
	Exe    string
	Logger *slog.Logger // default: discard
}

// Engine is gitlet for one agent in one worktree.
type Engine struct{ impl }

// Open opens the repository containing opt.Dir, loads configuration, opens
// the ledger in <git common dir>/gitlet/ledger.db and resolves the agent id.
// It does not contact the remote.
func Open(ctx context.Context, opt Options) (*Engine, error) { return open(ctx, opt) }

// Close releases the session lock and closes the ledger. It does not delete refs.
func (e *Engine) Close() error { return e.close() }

// Shutdown writes a final heartbeat, or deletes this agent's refs when it has
// no active or submitted items (best effort within ctx), then closes.
func (e *Engine) Shutdown(ctx context.Context) error { return e.shutdown(ctx) }

// Agent returns this engine's agent.
func (e *Engine) Agent() model.Agent { return e.agent() }

// Root returns the worktree root.
func (e *Engine) Root() string { return e.root() }

// ReadOnly reports whether this engine may not publish (another session holds
// the worktree, the remote refused writes, or the ledger is newer).
func (e *Engine) ReadOnly() bool { return e.readOnly() }

// Config returns the effective configuration.
func (e *Engine) Config() config.Config { return e.config() }

// Tick does the background work (docs/SPEC.md, sections 6 to 11): poll the
// remote when the poll interval passed, snapshot the worktree when it changed
// and the snapshot interval passed, open a provisional item, renew leases
// while active, detect landed items, apply settled threads, auto-claim and
// release, and re-run the rules. Callers run it every few seconds.
func (e *Engine) Tick(ctx context.Context) error { return e.tick(ctx) }

// Call runs a verb by name. input is a pointer to the verb's input type, the
// input value itself, or JSON (json.RawMessage or []byte). Refusals are
// results with Outcome refused, not errors; errors mean gitlet itself failed.
func (e *Engine) Call(ctx context.Context, verb string, input any) (api.Output, error) {
	return e.call(ctx, verb, input)
}

// Start opens a work item for a stated task (docs/SPEC.md, section 7).
func (e *Engine) Start(ctx context.Context, in api.StartInput) (*api.Reply[api.StartData], error) {
	return e.start(ctx, in)
}

// Status reports this agent's work, everything around it, and every open note.
func (e *Engine) Status(ctx context.Context, in api.StatusInput) (*api.Reply[api.StatusData], error) {
	return e.status(ctx, in)
}

// Who lists the agents working on a file or symbol.
func (e *Engine) Who(ctx context.Context, in api.WhoInput) (*api.Reply[api.WhoData], error) {
	return e.who(ctx, in)
}

// Why explains the last change to a line, file or symbol.
func (e *Engine) Why(ctx context.Context, in api.WhyInput) (*api.Reply[api.WhyData], error) {
	return e.why(ctx, in)
}

// Claim takes ownership of an interface.
func (e *Engine) Claim(ctx context.Context, in api.ClaimInput) (*api.Reply[api.ClaimData], error) {
	return e.claim(ctx, in)
}

// Publish publishes the signature of an owned interface (docs/SPEC.md, section 12.1).
func (e *Engine) Publish(ctx context.Context, in api.PublishInput) (*api.Reply[api.PublishData], error) {
	return e.publish(ctx, in)
}

// Need records that this agent's work depends on an interface.
func (e *Engine) Need(ctx context.Context, in api.NeedInput) (*api.Reply[api.NeedData], error) {
	return e.need(ctx, in)
}

// Use brings a published interface into the branch and pins it (section 12.2).
func (e *Engine) Use(ctx context.Context, in api.UseInput) (*api.Reply[api.UseData], error) {
	return e.use(ctx, in)
}

// Release drops a claim and/or need.
func (e *Engine) Release(ctx context.Context, in api.ReleaseInput) (*api.Reply[api.ReleaseData], error) {
	return e.release(ctx, in)
}

// Discuss posts a message or proposal in a thread (section 11).
func (e *Engine) Discuss(ctx context.Context, in api.DiscussInput) (*api.Reply[api.DiscussData], error) {
	return e.discuss(ctx, in)
}

// Accept accepts a proposal.
func (e *Engine) Accept(ctx context.Context, in api.AcceptInput) (*api.Reply[api.AcceptData], error) {
	return e.accept(ctx, in)
}

// Dismiss closes a note.
func (e *Engine) Dismiss(ctx context.Context, in api.DismissInput) (*api.Reply[api.DismissData], error) {
	return e.dismiss(ctx, in)
}

// Update rebases the branch onto main safely (section 12.3).
func (e *Engine) Update(ctx context.Context, in api.UpdateInput) (*api.Reply[api.UpdateData], error) {
	return e.update(ctx, in)
}

// Submit checks, pushes and opens a pull request (section 12.4).
func (e *Engine) Submit(ctx context.Context, in api.SubmitInput) (*api.Reply[api.SubmitData], error) {
	return e.submit(ctx, in)
}

// Abandon stops the current item.
func (e *Engine) Abandon(ctx context.Context, in api.AbandonInput) (*api.Reply[api.AbandonData], error) {
	return e.abandon(ctx, in)
}

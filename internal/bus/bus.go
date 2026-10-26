// Package bus moves gitlet's coordination state through refs on the shared
// git remote (docs/SPEC.md, section 6). Each agent writes only its own state,
// outbox and wip refs; claims are the one contested write and use
// compare-and-swap pushes that the remote referees. With no remote, the bus
// works directly on the shared .git of the local worktrees.
//
// The bus never holds the per-agent write lock; the engine does, around
// read-modify-write cycles of the agent's own documents.
package bus

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
)

// Ref namespaces.
const (
	Prefix       = "refs/gitlet/"
	AgentsPrefix = "refs/gitlet/agents/" // + <agent>/state, <agent>/outbox
	WipPrefix    = "refs/gitlet/wip/"    // + <agent>/<item>
	ClaimsPrefix = "refs/gitlet/claims/" // + <key>
	ProbePrefix  = "refs/gitlet/probe/"  // + <agent>, used by doctor
)

// Size limits for documents (docs/SPEC.md, section 6).
const (
	MaxReadDoc  = 1 << 20   // readers skip larger documents
	MaxWriteDoc = 256 << 10 // writers refuse larger documents
)

// Errors returned by the bus.
var (
	// ErrClaimLost: a claim write was refused because the claim is held by
	// someone else or changed since it was read.
	ErrClaimLost = errors.New("claim held or changed by another agent")
	// ErrUnreachable: the remote could not be reached; claims need it.
	ErrUnreachable = errors.New("remote unreachable; claims need the remote")
	// ErrObserver: the remote refused writes (permission denied); the bus is
	// read-only until it re-probes.
	ErrObserver = errors.New("no permission to push gitlet refs; running read-only")
	// ErrTooLarge: a document exceeds MaxWriteDoc.
	ErrTooLarge = errors.New("document too large to publish")
	// ErrIdentityCollision: our own ref changed twice under us, so another
	// process is writing as this agent.
	ErrIdentityCollision = errors.New("another process is writing this agent's refs")
)

// RejectedError is a push the remote rejected for a reason other than a
// lease, for example a pre-receive hook or push protection, or a snapshot
// WriteWip refused to push.
type RejectedError struct {
	Ref    string
	Kind   gitx.RejectKind
	Reason string
	Remote []string // remote: lines
}

func (e *RejectedError) Error() string { return "push of " + e.Ref + " rejected: " + e.Reason }

// StateRef, OutboxRef, WipRef and ClaimRef build canonical ref names.
func StateRef(a model.AgentID) string               { return AgentsPrefix + string(a) + "/state" }
func OutboxRef(a model.AgentID) string              { return AgentsPrefix + string(a) + "/outbox" }
func WipRef(a model.AgentID, i model.ItemID) string { return WipPrefix + string(a) + "/" + string(i) }
func ClaimRef(key string) string                    { return ClaimsPrefix + key }

// TrackingRef maps a canonical refs/gitlet/... name to where fetches put it:
// refs/gitlet-remote/<remote>/... (the canonical name itself without a remote).
func TrackingRef(remote, ref string) string { return trackingRef(remote, ref) }

// Repo is the subset of *gitx.Repo the bus uses.
type Repo interface {
	HashBlob(ctx context.Context, data []byte) (string, error)
	MakeTree(ctx context.Context, entries []gitx.TreeEntry) (string, error)
	CommitTree(ctx context.Context, tree string, parents []string, message string, id gitx.Identity) (string, error)
	OpenReader(ctx context.Context) (*gitx.Reader, error)
	RevParse(ctx context.Context, rev string) (string, error)
	UpdateRef(ctx context.Context, ref, newOID, oldOID string) error
	DeleteRef(ctx context.Context, ref, oldOID string) error
	ListRefs(ctx context.Context, prefix string) (map[string]string, error)
	ListRefsByDate(ctx context.Context, prefix string, max int) ([]gitx.RefInfo, error)
	Fetch(ctx context.Context, opt gitx.FetchOptions) error
	Push(ctx context.Context, opt gitx.PushOptions) (gitx.PushReport, error)
	PushExcluded(ctx context.Context, commit string, published []string, maxSize int64) ([]string, error)
	LsRemote(ctx context.Context, remote string, patterns ...string) (map[string]string, error)
}

// PushLog remembers what this clone last pushed to each ref (the ledger).
type PushLog interface {
	LastPushed(ctx context.Context, ref string) (string, error)
	SetLastPushed(ctx context.Context, ref, oid string) error
}

// Options configures a Bus.
type Options struct {
	Remote  string // "" means no remote: work on local refs only
	Main    string // main branch short name
	LockDir string // <common dir>/gitlet: holds fetch.lock and the fetch stamp
	PushLog PushLog
	Now     func() time.Time
	// FetchReuse: a fetch by any process newer than this is reused instead of
	// fetching again. Default 5 s.
	FetchReuse time.Duration
	// MaxSnapshotFile is gitlet.maxSnapshotFile: WriteWip refuses a snapshot
	// whose push would publish a larger file. Default 5 MiB.
	MaxSnapshotFile int64
}

// Bus is one agent's connection to the shared state. It is safe for
// concurrent use.
type Bus struct{ impl }

// New returns a Bus for agent me.
func New(repo Repo, me model.AgentID, opt Options) *Bus { return newBus(repo, me, opt) }

// Remote returns the configured remote name ("" when none).
func (b *Bus) Remote() string { return b.remote() }

// MainRef returns the ref holding gitlet's view of main:
// refs/gitlet-main/<remote> with a remote, refs/heads/<main> without.
func (b *Bus) MainRef() string { return b.mainRef() }

// ---- reading ----

// FetchResult describes a poll.
type FetchResult struct {
	At          time.Time // when the fetch whose result is used ran
	Reused      bool      // another process fetched recently; nothing was fetched now
	Fingerprint string    // hash of the local tracking refs after the fetch
}

// Fetch polls the remote (docs/SPEC.md, section 6.2): one metadata fetch of
// agents, claims and main with --prune, under the clone-wide fetch lock,
// reusing a fetch newer than FetchReuse. Without a remote it only computes
// the fingerprint. Network failures return an error for which
// gitx.IsNetwork is true.
func (b *Bus) Fetch(ctx context.Context) (FetchResult, error) { return b.fetch(ctx) }

// FetchOutside fetches every branch head into refs/gitlet-heads/<remote>/*
// (no prune) and returns the branches updated within window, newest first,
// at most max, excluding main.
func (b *Bus) FetchOutside(ctx context.Context, window time.Duration, max int) ([]gitx.RefInfo, error) {
	return b.fetchOutside(ctx, window, max)
}

// FetchWip makes a peer's snapshot available locally and returns its oid.
// want is the snapshot oid from the peer's state; nothing is fetched when the
// local tracking ref already has it.
func (b *Bus) FetchWip(ctx context.Context, agent model.AgentID, item model.ItemID, want string) (string, error) {
	return b.fetchWip(ctx, agent, item, want)
}

// PruneWips deletes local tracking refs of peers' snapshots that are not in
// keep (canonical wip ref names).
func (b *Bus) PruneWips(ctx context.Context, keep map[string]bool) error {
	return b.pruneWips(ctx, keep)
}

// ClaimView is a claim with the oid of the commit it was read from.
type ClaimView struct {
	Claim model.Claim
	OID   string
}

// State is the decoded shared state.
type State struct {
	Agents   map[model.AgentID]model.AgentState
	Outboxes map[model.AgentID]model.Outbox
	Claims   map[string]ClaimView // by symbol key
	// Refs maps every canonical agents/ and claims/ ref to its oid, for
	// lease observation (namespace "refs/gitlet/").
	Refs    map[string]string
	Main    string   // oid of main, "" if unknown
	Skipped []string // documents skipped (newer format, too large, invalid), with the reason
}

// Load decodes the shared state from the tracking refs (or local refs
// without a remote). This agent's own documents come from its local refs,
// which are always at least as new as the remote copies. Documents are
// validated (ids, paths, size) and skipped with a reason when invalid.
func (b *Bus) Load(ctx context.Context) (*State, error) { return b.load(ctx) }

// ---- own refs ----

// ReadOwnState returns this agent's state from its local ref and the ref's
// oid ("" and a zero state when missing).
func (b *Bus) ReadOwnState(ctx context.Context) (model.AgentState, string, error) {
	return b.readOwnState(ctx)
}

// ReadOwnOutbox returns this agent's outbox from its local ref and its oid.
func (b *Bus) ReadOwnOutbox(ctx context.Context) (model.Outbox, string, error) {
	return b.readOwnOutbox(ctx)
}

// WriteState writes st to the local state ref (compare-and-swap on seenOID,
// "" for create) and pushes it with a lease on the oid last pushed. When the
// remote is unreachable the local write stands and Flush pushes it later;
// the returned error then satisfies gitx.IsNetwork.
func (b *Bus) WriteState(ctx context.Context, st model.AgentState, seenOID string) (string, error) {
	return b.writeState(ctx, st, seenOID)
}

// WriteOutbox is WriteState for the outbox.
func (b *Bus) WriteOutbox(ctx context.Context, ob model.Outbox, seenOID string) (string, error) {
	return b.writeOutbox(ctx, ob, seenOID)
}

// WriteWip points this agent's wip ref for item at commit and pushes it in a
// push of its own. A rejection returns *RejectedError and moves the local ref
// back to the snapshot last pushed, so Flush never pushes the rejected one.
// With a remote, it also returns *RejectedError, writing and pushing
// nothing, when the push would publish files a snapshot never carries:
// denied or larger than MaxSnapshotFile, from commits not on the remote yet
// (docs/SPEC.md, section 9.1).
func (b *Bus) WriteWip(ctx context.Context, item model.ItemID, commit string) error {
	return b.writeWip(ctx, item, commit)
}

// DeleteWip deletes this agent's wip ref for item, locally and remotely.
func (b *Bus) DeleteWip(ctx context.Context, item model.ItemID) error { return b.deleteWip(ctx, item) }

// DeleteOwn deletes this agent's state, outbox and every wip ref, locally and
// remotely, each with a lease (best effort within ctx).
func (b *Bus) DeleteOwn(ctx context.Context) error { return b.deleteOwn(ctx) }

// Flush pushes own refs whose local oid differs from the last pushed one
// (after the remote was unreachable), and recreates own refs the remote lost.
// It skips an update the remote already rejected in this process. Each
// rejection is a *RejectedError naming the ref in the returned error; a
// rejected snapshot also moves the local wip ref back, as in WriteWip.
func (b *Bus) Flush(ctx context.Context) error { return b.flush(ctx) }

// ---- claims ----

// CreateClaim creates refs/gitlet/claims/<c.Key> with a create-only push.
// parent, when set, becomes the claim commit's only parent (a stub). won
// reports whether this agent now holds it; current is the claim as it stands
// either way (fetched when lost). Without a reachable remote it returns
// ErrUnreachable and never falls back to local compare-and-swap.
func (b *Bus) CreateClaim(ctx context.Context, c model.Claim, parent string) (won bool, current ClaimView, err error) {
	return b.createClaim(ctx, c, parent)
}

// UpdateClaim replaces the claim whose commit is seenOID with c (renewal,
// contract, requirements, transfer or takeover). Returns ErrClaimLost when
// the remote ref is no longer seenOID.
func (b *Bus) UpdateClaim(ctx context.Context, c model.Claim, seenOID, parent string) (ClaimView, error) {
	return b.updateClaim(ctx, c, seenOID, parent)
}

// ClaimUpdate is one entry of a batched claim write.
type ClaimUpdate struct {
	Claim   model.Claim
	SeenOID string
	Parent  string
}

// ClaimOutcome is the result of one batched claim write.
type ClaimOutcome struct {
	Key  string
	View ClaimView
	Err  error // ErrClaimLost or *RejectedError
}

// UpdateClaims writes several claims in one push (a lease per ref, not
// atomic), for renewals.
func (b *Bus) UpdateClaims(ctx context.Context, updates []ClaimUpdate) ([]ClaimOutcome, error) {
	return b.updateClaims(ctx, updates)
}

// DeleteClaim deletes the claim if it is still seenOID (ErrClaimLost otherwise).
func (b *Bus) DeleteClaim(ctx context.Context, key, seenOID string) error {
	return b.deleteClaim(ctx, key, seenOID)
}

// ---- branches and maintenance ----

// RefreshMain updates the user's refs/remotes/<remote>/<main> and gitlet's
// main ref with a foreground fetch, for update and submit. It returns main's oid.
func (b *Bus) RefreshMain(ctx context.Context) (string, error) { return b.refreshMain(ctx) }

// PushBranch pushes commit to refs/heads/<branch> on the remote. It
// fast-forwards when possible, otherwise uses a lease on oldOID (the oid this
// clone last pushed; "" when never). The user's pre-push hooks run.
func (b *Bus) PushBranch(ctx context.Context, commit, branch, oldOID string) (gitx.PushResult, error) {
	return b.pushBranch(ctx, commit, branch, oldOID)
}

// Reap deletes the given refs of a long-gone agent, each with a lease on the
// oid seen (canonical ref name -> oid).
func (b *Bus) Reap(ctx context.Context, refs map[string]string) error { return b.reap(ctx, refs) }

// ProbeResult says which ref operations the remote allows.
type ProbeResult struct {
	Create, NonFastForward, Delete bool
	Detail                         string
}

// Probe tests ref access on the remote with refs/gitlet/probe/<agent>.
func (b *Bus) Probe(ctx context.Context) (ProbeResult, error) { return b.probe(ctx) }

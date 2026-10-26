// Package rules turns what one server knows into notes for its own agent
// (docs/SPEC.md, sections 9.6, 10 and 11). Rules are pure functions of a
// View: no I/O, no clock reads, deterministic output (stable note ids and
// versions, sorted).
package rules

import (
	"time"

	"github.com/crewlet/gitlet/internal/model"
)

// Peer is another agent as this server sees it.
type Peer struct {
	State     model.AgentState
	Outbox    model.Outbox
	Lapsed    bool       // judged on this server's observed time (section 8)
	IdleSince *time.Time // when its state oid was first seen, set when Lapsed
}

// ClaimView is a claim plus what this server knows about its ref.
type ClaimView struct {
	Claim     model.Claim
	OID       string
	Lapsed    bool
	IdleSince *time.Time
	Landed    bool // its contract has landed on main (section 7), computed by the caller
}

// Outside is an ordinary branch that is not a gitlet item.
type Outside struct {
	Branch    string
	Head      string
	Footprint model.Footprint
}

// Conflict is a predicted textual conflict between one of my items and
// another item (git merge-tree of both heads), restricted by the caller to
// files both footprints change, lockfiles excluded.
type Conflict struct {
	Mine  model.ItemID
	Other model.ItemID
	Paths []string
}

// MainView is current main seen from one of my items: the footprint of main
// against the item's base.
type MainView struct {
	Main      string // main's oid
	Footprint model.Footprint
	Skipped   bool // main changed too much since the base; symbol-level checks skipped
}

// Limits holds the configured bounds (docs/SPEC.md, section 15).
type Limits struct {
	ThreadMessages int           // per participant; default 3
	ThreadTimeout  time.Duration // default 10m
	StaleItem      time.Duration // default 24h
	PublishTimeout time.Duration // default 15m
	AutoClaim      bool
	Migrations     []string // extra migration directory globs
}

// View is everything the rules may look at.
type View struct {
	Me        model.AgentID
	Mine      model.AgentState // my agent state, with my items and footprints
	MyOutbox  model.Outbox
	Peers     map[model.AgentID]Peer
	Claims    map[string]ClaimView // by symbol key
	Outside   []Outside
	Conflicts []Conflict
	Main      map[model.ItemID]MainView
	// ClashSeen maps signature thread ids to when this server first saw the
	// clash; the caller persists it. Threads missing here count as seen now.
	ClashSeen map[string]time.Time
	// SameWorktree is set when another gitlet session already serves this
	// worktree; WorktreeCommand is the git worktree add command to suggest.
	SameWorktree    bool
	WorktreeCommand string
	Limits          Limits
	Now             time.Time
}

// Evaluate returns the notes for View.Me, sorted by severity (act, warn,
// info), then kind, then id. Every note that offers discuss has Thread set.
func Evaluate(v View) []model.Note { return evaluate(v) }

// Threads assembles the threads View.Me takes part in: signature threads for
// current clashes, and note threads that carry messages, from every
// participant's outbox, with agreement or fallback worked out.
func Threads(v View) []model.ThreadState { return threads(v) }

// Action is one step the engine takes to apply a settled thread.
type Action string

const (
	ActClaim    Action = "claim"    // I am the owner and hold no claim: create it (or wait for the holder's transfer)
	ActTransfer Action = "transfer" // I hold the claim but someone else owns the outcome: hand it over (epoch + 1)
	ActNeed     Action = "need"     // I am not the owner: record a need with my requirement
	ActRace     Action = "race"     // fallback with no holder: attempt a create-only claim
)

// Decision is the outcome of a settled signature thread for View.Me.
type Decision struct {
	Thread       string
	Key          string
	Symbol       model.Symbol
	Owner        model.AgentID // "" for a race
	OwnerItem    model.ItemID
	Actions      []Action
	Requirement  string              // my latest message, for ActNeed
	Requirements []model.Requirement // every non-owner's latest message, for the claim
	Proposal     *model.Proposal
	FellBack     bool
	ClaimOID     string // the claim as seen, for ActTransfer
	Epoch        int    // the claim's epoch as seen
}

// Decisions returns what View.Me must do about settled signature threads.
// Each action is idempotent for the engine: it is returned until the shared
// state shows it done.
func Decisions(v View) []Decision { return decisions(v) }

// AutoClaim is a symbol View.Me should claim automatically.
type AutoClaim struct {
	Symbol model.Symbol
	Item   model.ItemID
	Reason string
}

// AutoClaims returns unclaimed symbols whose resolved, breaking signature
// change by one of my items meets another item's references or clashes with
// another item, when Limits.AutoClaim is set.
func AutoClaims(v View) []AutoClaim { return autoClaims(v) }

// Release is a claim of mine that should be deleted.
type Release struct {
	Key    string
	OID    string
	Reason string
}

// Releases returns my claims to delete: their item landed, was abandoned or
// is gone; their contract landed; or they were automatic, have no contract,
// and my footprint no longer changes the symbol.
func Releases(v View) []Release { return releases(v) }

// Similarity scores intent a against intent b in [0, 1] by TF-IDF cosine over
// corpus (which should include a and b), with the normalization of
// docs/SPEC.md, section 10.2. shared is the number of shared content terms.
func Similarity(a, b string, corpus []string) (score float64, shared int) {
	return similarity(a, b, corpus)
}

// Match says whether ref, from a footprint of the given Footprint.Version,
// refers to sym (docs/SPEC.md, section 9.6). resolved is false when the
// match relies only on a unique method name, in which case uniqueMethod
// must be true for a match.
func Match(ref model.Ref, version int, sym model.Symbol, uniqueMethod bool) (match, resolved bool) {
	return matchRef(ref, version, sym, uniqueMethod)
}

// HotFile classifies a path: "lockfile", "changelog", or "".
func HotFile(path string) string { return hotFile(path) }

// MigrationSeq returns the migration directory and sequence number of a
// path, when it is a migration (docs/SPEC.md, section 10.3).
func MigrationSeq(path string, extraGlobs []string) (dir, seq string, ok bool) {
	return migrationSeq(path, extraGlobs)
}

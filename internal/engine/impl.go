package engine

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/gitlet/internal/bus"
	"github.com/crewlet/gitlet/internal/config"
	"github.com/crewlet/gitlet/internal/flock"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/ledger"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/rules"
)

// impl holds the engine's state. Engine embeds it, so every field is
// reachable as e.field from the methods on *Engine.
//
// Locking: mu serializes the verbs and the locked phase of Tick, and guards
// every field below it. The poll state has its own small lock because Tick
// fetches from the remote without holding mu, so a slow remote never blocks
// a verb (docs/SPEC.md, section 6.2).
type impl struct {
	// Set by Open and read-only afterwards.
	opt          Options
	now          func() time.Time
	log          *slog.Logger
	cfg          config.Config
	repo         *gitx.Repo // foreground commands: the agent asked for them
	bg           *gitx.Repo // background commands: never take optional locks
	led          *ledger.Ledger
	notes        noteStore // led, behind an interface so decoration is testable
	bus          *bus.Bus
	me           model.Agent
	stateDir     string // <git common dir>/gitlet
	ledgerPath   string // key of led in the process-wide ledger registry
	gitOK        bool   // git >= 2.38: merge-tree based checks are enabled
	gitWarn      string // set when git is too old
	session      *flock.Lock
	sameWorktree bool   // another live session holds this worktree's session lock
	worktreeCmd  string // the git worktree add command suggested to it

	// Read-only reasons that can appear while running. They are atomic so
	// ReadOnly can be asked without waiting for a verb or Tick.
	observer  atomic.Bool // the remote refused our writes
	collision atomic.Bool // another process writes this agent's refs

	// Polling, shared by the verbs and Tick's unlocked network phase.
	pollSem chan struct{} // one fetch at a time in this engine
	poll    pollState
	fetches atomic.Int64 // fetches attempted, for tests

	mu sync.Mutex // serializes verbs and Tick; guards everything below

	closed         bool
	verbWarnings   []string // warnings gathered while the current verb runs
	agentLock      *flock.Lock
	agentLockDepth int

	// Shared state as last loaded from refs, with this agent's own documents
	// replaced by what it last wrote.
	shared    *bus.State
	loadedFP  string // fetch fingerprint shared was loaded at
	mainOIDs  string // memoized main oid for the loaded state
	obs       ledger.Observation
	obsLoaded bool

	// observerSince is when observer mode was entered or last re-probed.
	observerSince time.Time

	// Snapshots and activity.
	snap          snapRecord
	syncedMain    string // main oid the item footprints were last synced against
	activity      time.Time
	activitySaved time.Time

	// Caches. All are hints: dropping them only costs time.
	extract       *extractCache
	commits       map[string]bool           // commit oids known to exist locally
	contracts     map[string]bool           // contract landed memo, by key+sigHash+stub+main
	mainViews     map[string]rules.MainView // by base..main
	prevMain      map[model.ItemID]rules.MainView
	prevConflicts map[string]rules.Conflict // by mine/other item pair
	landChecks    map[string]bool           // landing memo, by item+head+main
	ghChecked     map[model.ItemID]time.Time
	prClosed      map[model.ItemID]bool // pull requests gh reported closed without merging
	outsideRefs   []gitx.RefInfo
	outside       []rules.Outside
	lastReap      time.Time
	lastTick      time.Time
	lastView      *rules.View
	lastNotes     []model.Note
}

func (e *Engine) agent() model.Agent    { return e.me }
func (e *Engine) root() string          { return e.repo.Root }
func (e *Engine) config() config.Config { return e.cfg }

// readOnly reports whether this engine may not publish: another session
// holds the worktree, the remote refused writes (observer mode), another
// process writes as this agent (identity collision), or the ledger was
// written by a newer gitlet.
func (e *Engine) readOnly() bool {
	return e.sameWorktree || e.observer.Load() || e.collision.Load() || (e.led != nil && e.led.ReadOnly())
}

package bus

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
)

// The real repository is the Repo the bus is built for.
var _ Repo = (*gitx.Repo)(nil)

const (
	defaultFetchReuse = 5 * time.Second
	// observerRetry is how long the bus stays read-only after the remote
	// refused a push for lack of permission; the next push after it is the
	// re-probe (docs/SPEC.md, section 6.1).
	observerRetry = time.Hour
	// settledAge is how long landed and abandoned items stay in state.json
	// (docs/SPEC.md, section 6.4).
	settledAge = time.Hour
)

// Files and messages of metadata commits (docs/SPEC.md, section 6).
const (
	stateFile     = "state.json"
	outboxFile    = "outbox.json"
	claimFile     = "claim.json"
	probeFile     = "probe.json"
	stateMessage  = "gitlet: state"
	outboxMessage = "gitlet: outbox"
	claimMessage  = "gitlet: claim"
	probeMessage  = "gitlet: probe"
)

// Local namespaces fetches write into.
const (
	remotePrefix = "refs/gitlet-remote/" // + <remote>/agents/..., claims/..., wip/...
	mainPrefix   = "refs/gitlet-main/"   // + <remote>
	headsPrefix  = "refs/gitlet-heads/"  // + <remote>/<branch>
)

var errNoRemote = errors.New("no remote configured")

type impl struct {
	repo       Repo
	me         model.AgentID
	remoteName string
	mainBranch string
	lockDir    string
	pushLog    PushLog
	now        func() time.Time
	fetchReuse time.Duration

	// isNetwork and isPermission classify failed git invocations. They are
	// gitx's classifiers; tests replace them because those need real git
	// output to recognise a failure.
	isNetwork    func(error) bool
	isPermission func(error) bool

	// writeMu serializes writes of this agent's own refs within the process,
	// so the local compare-and-swap, the push and the push log stay in step.
	writeMu sync.Mutex
	// refused maps an own ref to the value the remote last rejected for it
	// ("" for a deletion), so Flush does not push that value again. Guarded
	// by writeMu.
	refused map[string]string

	obsMu         sync.Mutex
	observerSince time.Time // when a push was last refused for permission; zero when writable
}

func trackingRef(remote, ref string) string {
	if remote == "" || !strings.HasPrefix(ref, Prefix) {
		return ref
	}
	return remotePrefix + remote + "/" + strings.TrimPrefix(ref, Prefix)
}

func newBus(repo Repo, me model.AgentID, opt Options) *Bus {
	b := &Bus{impl{
		repo:         repo,
		me:           me,
		remoteName:   opt.Remote,
		mainBranch:   opt.Main,
		lockDir:      opt.LockDir,
		pushLog:      opt.PushLog,
		now:          opt.Now,
		fetchReuse:   opt.FetchReuse,
		isNetwork:    gitx.IsNetwork,
		isPermission: gitx.IsPermission,
		refused:      map[string]string{},
	}}
	if b.mainBranch == "" {
		b.mainBranch = "main"
	}
	if b.pushLog == nil {
		b.pushLog = &memPushLog{m: map[string]string{}}
	}
	if b.now == nil {
		b.now = time.Now
	}
	if b.fetchReuse == 0 {
		b.fetchReuse = defaultFetchReuse
	}
	return b
}

func (b *Bus) remote() string { return b.remoteName }

func (b *Bus) mainRef() string {
	if b.remoteName == "" {
		return "refs/heads/" + b.mainBranch
	}
	return mainPrefix + b.remoteName
}

// tracking returns where fetches put the canonical ref.
func (b *Bus) tracking(ref string) string { return trackingRef(b.remoteName, ref) }

// localOID returns the oid ref points at, or "" when it does not exist.
func (b *Bus) localOID(ctx context.Context, ref string) (string, error) {
	oid, err := b.repo.RevParse(ctx, ref)
	if errors.Is(err, gitx.ErrNotFound) {
		return "", nil
	}
	return oid, err
}

// listCanonical lists the local refs under the canonical prefix, or under its
// tracking location when tracking is set, keyed by canonical name.
func (b *Bus) listCanonical(ctx context.Context, prefix string, tracking bool) (map[string]string, error) {
	local := prefix
	if tracking {
		local = b.tracking(prefix)
	}
	refs, err := b.repo.ListRefs(ctx, local)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(refs))
	for name, oid := range refs {
		if rest, ok := strings.CutPrefix(name, local); ok {
			out[prefix+rest] = oid
		}
	}
	return out, nil
}

// ownRefs returns this agent's state, outbox and wip refs, from the local
// namespace or from their tracking copies, keyed by canonical name.
func (b *Bus) ownRefs(ctx context.Context, tracking bool) (map[string]string, error) {
	out := map[string]string{}
	for _, prefix := range []string{AgentsPrefix + string(b.me) + "/", WipPrefix + string(b.me) + "/"} {
		refs, err := b.listCanonical(ctx, prefix, tracking)
		if err != nil {
			return nil, err
		}
		for ref, oid := range refs {
			if b.isOwnRef(ref) {
				out[ref] = oid
			}
		}
	}
	return out, nil
}

// isOwnRef reports whether ref is one of this agent's state, outbox or wip refs.
func (b *Bus) isOwnRef(ref string) bool {
	if ref == StateRef(b.me) || ref == OutboxRef(b.me) {
		return true
	}
	item, ok := strings.CutPrefix(ref, WipPrefix+string(b.me)+"/")
	return ok && model.ValidItemID(model.ItemID(item))
}

// isWipRef reports whether ref is a snapshot ref.
func isWipRef(ref string) bool { return strings.HasPrefix(ref, WipPrefix) }

// sortedKeys returns the keys of m in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// memPushLog is the PushLog used when Options has none: it lasts as long as
// the process.
type memPushLog struct {
	mu sync.Mutex
	m  map[string]string
}

func (l *memPushLog) LastPushed(_ context.Context, ref string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m[ref], nil
}

func (l *memPushLog) SetLastPushed(_ context.Context, ref, oid string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if oid == "" {
		delete(l.m, ref)
	} else {
		l.m[ref] = oid
	}
	return nil
}

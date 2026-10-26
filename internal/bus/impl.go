package bus

// TODO(implement): docs/SPEC.md, section 6.

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
)

var errNotImplemented = errors.New("bus: not implemented")

type impl struct{}

func trackingRef(remote, ref string) string { return ref }

func newBus(repo Repo, me model.AgentID, opt Options) *Bus { return &Bus{} }
func (b *Bus) remote() string                              { return "" }
func (b *Bus) mainRef() string                             { return "" }
func (b *Bus) fetch(ctx context.Context) (FetchResult, error) {
	return FetchResult{}, errNotImplemented
}
func (b *Bus) fetchOutside(ctx context.Context, window time.Duration, max int) ([]gitx.RefInfo, error) {
	return nil, errNotImplemented
}
func (b *Bus) fetchWip(ctx context.Context, agent model.AgentID, item model.ItemID, want string) (string, error) {
	return "", errNotImplemented
}
func (b *Bus) pruneWips(ctx context.Context, keep map[string]bool) error { return errNotImplemented }
func (b *Bus) load(ctx context.Context) (*State, error)                  { return nil, errNotImplemented }
func (b *Bus) readOwnState(ctx context.Context) (model.AgentState, string, error) {
	return model.AgentState{}, "", errNotImplemented
}
func (b *Bus) readOwnOutbox(ctx context.Context) (model.Outbox, string, error) {
	return model.Outbox{}, "", errNotImplemented
}
func (b *Bus) writeState(ctx context.Context, st model.AgentState, seenOID string) (string, error) {
	return "", errNotImplemented
}
func (b *Bus) writeOutbox(ctx context.Context, ob model.Outbox, seenOID string) (string, error) {
	return "", errNotImplemented
}
func (b *Bus) writeWip(ctx context.Context, item model.ItemID, commit string) error {
	return errNotImplemented
}
func (b *Bus) deleteWip(ctx context.Context, item model.ItemID) error { return errNotImplemented }
func (b *Bus) deleteOwn(ctx context.Context) error                    { return errNotImplemented }
func (b *Bus) flush(ctx context.Context) error                        { return errNotImplemented }
func (b *Bus) createClaim(ctx context.Context, c model.Claim, parent string) (bool, ClaimView, error) {
	return false, ClaimView{}, errNotImplemented
}
func (b *Bus) updateClaim(ctx context.Context, c model.Claim, seenOID, parent string) (ClaimView, error) {
	return ClaimView{}, errNotImplemented
}
func (b *Bus) updateClaims(ctx context.Context, updates []ClaimUpdate) ([]ClaimOutcome, error) {
	return nil, errNotImplemented
}
func (b *Bus) deleteClaim(ctx context.Context, key, seenOID string) error { return errNotImplemented }
func (b *Bus) refreshMain(ctx context.Context) (string, error)            { return "", errNotImplemented }
func (b *Bus) pushBranch(ctx context.Context, commit, branch, oldOID string) (gitx.PushResult, error) {
	return gitx.PushResult{}, errNotImplemented
}
func (b *Bus) reap(ctx context.Context, refs map[string]string) error { return errNotImplemented }
func (b *Bus) probe(ctx context.Context) (ProbeResult, error) {
	return ProbeResult{}, errNotImplemented
}

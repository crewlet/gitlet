package engine

// Interface verbs: claim, publish, need, use, release (docs/SPEC.md,
// sections 4, 6.1, 11 and 12).
//
// TODO(implement): these stubs belong to a follow-up engineer. Each verb
// follows the pattern of verbs_info.go:
//
//	release, err := e.beginVerb(ctx, snap) // snap true for claim and publish
//	if err != nil {
//		return nil, err
//	}
//	defer release()
//	sym, refusal, err := e.resolveSymbol(ctx, "claim", in.Symbol, "note", in.Note)
//	... claimFor, myChange, myItemFor, needsOn, myNeed, waitingOn, newClaim,
//	    createClaim, updateClaim, deleteClaim, claimInfo, mutateOutbox,
//	    writeRefusal ...
//	e.finish(ctx, &r.Result, in.Note)

import (
	"context"

	"github.com/crewlet/gitlet/internal/api"
)

func (e *Engine) claim(ctx context.Context, in api.ClaimInput) (*api.Reply[api.ClaimData], error) {
	return nil, errNotImplemented
}

func (e *Engine) publish(ctx context.Context, in api.PublishInput) (*api.Reply[api.PublishData], error) {
	return nil, errNotImplemented
}

func (e *Engine) need(ctx context.Context, in api.NeedInput) (*api.Reply[api.NeedData], error) {
	return nil, errNotImplemented
}

func (e *Engine) use(ctx context.Context, in api.UseInput) (*api.Reply[api.UseData], error) {
	return nil, errNotImplemented
}

func (e *Engine) release(ctx context.Context, in api.ReleaseInput) (*api.Reply[api.ReleaseData], error) {
	return nil, errNotImplemented
}

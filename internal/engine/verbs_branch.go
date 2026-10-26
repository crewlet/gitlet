package engine

// Branch verbs: update, submit, abandon (docs/SPEC.md, sections 7 and 12).
//
// TODO(implement): these stubs belong to a follow-up engineer. Useful
// helpers: beginVerb (snap true for submit), activeItem, mutateState,
// releaseItem (abandon), detectLanding, contractLanded, mainOID,
// footprint, mainView, writeRefusal and finish. e.gitOK says whether the
// merge-tree checks are available (git 2.38); e.bus has RefreshMain and
// PushBranch; e.opt.Exe is the binary for GIT_SEQUENCE_EDITOR.

import (
	"context"

	"github.com/crewlet/gitlet/internal/api"
)

func (e *Engine) update(ctx context.Context, in api.UpdateInput) (*api.Reply[api.UpdateData], error) {
	return nil, errNotImplemented
}

func (e *Engine) submit(ctx context.Context, in api.SubmitInput) (*api.Reply[api.SubmitData], error) {
	return nil, errNotImplemented
}

func (e *Engine) abandon(ctx context.Context, in api.AbandonInput) (*api.Reply[api.AbandonData], error) {
	return nil, errNotImplemented
}

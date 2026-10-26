package engine

// Thread verbs: discuss, accept, dismiss (docs/SPEC.md, sections 4, 10.1
// and 11).
//
// TODO(implement): these stubs belong to a follow-up engineer. Useful
// helpers: beginVerb, resolveNoteID, resolveThreadID, resolveProposalID,
// threads, lightView, mutateOutbox, writeRefusal and finish. dismiss marks
// the note with the ledger's Dismiss (e.led) and does not pass it to
// finish, which would mark it acted instead.

import (
	"context"

	"github.com/crewlet/gitlet/internal/api"
)

func (e *Engine) discuss(ctx context.Context, in api.DiscussInput) (*api.Reply[api.DiscussData], error) {
	return nil, errNotImplemented
}

func (e *Engine) accept(ctx context.Context, in api.AcceptInput) (*api.Reply[api.AcceptData], error) {
	return nil, errNotImplemented
}

func (e *Engine) dismiss(ctx context.Context, in api.DismissInput) (*api.Reply[api.DismissData], error) {
	return nil, errNotImplemented
}

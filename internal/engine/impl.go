package engine

// TODO(implement): everything below.

import (
	"context"
	"errors"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/config"
	"github.com/crewlet/gitlet/internal/model"
)

var errNotImplemented = errors.New("engine: not implemented")

type impl struct{}

func open(ctx context.Context, opt Options) (*Engine, error) { return nil, errNotImplemented }
func (e *Engine) close() error                               { return nil }
func (e *Engine) shutdown(ctx context.Context) error         { return nil }
func (e *Engine) agent() model.Agent                         { return model.Agent{} }
func (e *Engine) root() string                               { return "" }
func (e *Engine) readOnly() bool                             { return false }
func (e *Engine) config() config.Config                      { return config.Config{} }
func (e *Engine) tick(ctx context.Context) error             { return errNotImplemented }
func (e *Engine) call(ctx context.Context, verb string, input any) (api.Output, error) {
	return nil, errNotImplemented
}
func (e *Engine) start(ctx context.Context, in api.StartInput) (*api.Reply[api.StartData], error) {
	return nil, errNotImplemented
}
func (e *Engine) status(ctx context.Context, in api.StatusInput) (*api.Reply[api.StatusData], error) {
	return nil, errNotImplemented
}
func (e *Engine) who(ctx context.Context, in api.WhoInput) (*api.Reply[api.WhoData], error) {
	return nil, errNotImplemented
}
func (e *Engine) why(ctx context.Context, in api.WhyInput) (*api.Reply[api.WhyData], error) {
	return nil, errNotImplemented
}
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
func (e *Engine) discuss(ctx context.Context, in api.DiscussInput) (*api.Reply[api.DiscussData], error) {
	return nil, errNotImplemented
}
func (e *Engine) accept(ctx context.Context, in api.AcceptInput) (*api.Reply[api.AcceptData], error) {
	return nil, errNotImplemented
}
func (e *Engine) dismiss(ctx context.Context, in api.DismissInput) (*api.Reply[api.DismissData], error) {
	return nil, errNotImplemented
}
func (e *Engine) update(ctx context.Context, in api.UpdateInput) (*api.Reply[api.UpdateData], error) {
	return nil, errNotImplemented
}
func (e *Engine) submit(ctx context.Context, in api.SubmitInput) (*api.Reply[api.SubmitData], error) {
	return nil, errNotImplemented
}
func (e *Engine) abandon(ctx context.Context, in api.AbandonInput) (*api.Reply[api.AbandonData], error) {
	return nil, errNotImplemented
}

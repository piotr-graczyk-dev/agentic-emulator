// Package ios is the iOS Simulator backend (stub; implemented in a follow-up step).
package ios

import (
	"context"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

type Backend struct{}

func New() *Backend { return &Backend{} }

func (b *Backend) Platform() api.Platform                         { return api.IOS }
func (b *Backend) List(ctx context.Context) ([]api.Device, error) { return nil, nil }
func (b *Backend) Create(ctx context.Context, req api.CreateRequest) (api.Device, error) {
	return api.Device{}, api.ErrNotImplemented
}
func (b *Backend) Boot(ctx context.Context, id string, opts api.BootOptions) (api.Device, error) {
	return api.Device{}, api.ErrNotImplemented
}
func (b *Backend) Shutdown(ctx context.Context, id string) error { return api.ErrNotImplemented }
func (b *Backend) Reset(ctx context.Context, id string) error    { return api.ErrNotImplemented }
func (b *Backend) SetProfile(ctx context.Context, id string, p api.Profile) (api.Device, error) {
	return api.Device{}, api.ErrNotImplemented
}
func (b *Backend) Screenshot(ctx context.Context, id string, o api.ScreenshotOptions) (api.Screenshot, error) {
	return api.Screenshot{}, api.ErrNotImplemented
}
func (b *Backend) UITree(ctx context.Context, id string) (*api.UITree, error) {
	return nil, api.ErrNotImplemented
}
func (b *Backend) Tap(ctx context.Context, id string, x, y int) error { return api.ErrNotImplemented }
func (b *Backend) Swipe(ctx context.Context, id string, x1, y1, x2, y2, ms int) error {
	return api.ErrNotImplemented
}
func (b *Backend) TypeText(ctx context.Context, id, text string) error { return api.ErrNotImplemented }
func (b *Backend) Key(ctx context.Context, id, key string) error       { return api.ErrNotImplemented }
func (b *Backend) Install(ctx context.Context, id, path string) error  { return api.ErrNotImplemented }
func (b *Backend) Launch(ctx context.Context, id, app string) error    { return api.ErrNotImplemented }
func (b *Backend) Terminate(ctx context.Context, id, app string) error { return api.ErrNotImplemented }
func (b *Backend) Close() error                                        { return nil }

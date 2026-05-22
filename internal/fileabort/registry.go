package fileabort

import (
	"context"
	"os"
	"sync"
)

type ctxKey struct{}

// Registry tracks open archive handles. CloseAll unblocks reads stuck in kernel I/O.
type Registry struct {
	mu    sync.Mutex
	files []*os.File
}

// WithContext attaches r to ctx.
func WithContext(ctx context.Context, r *Registry) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, r)
}

// FromContext returns the registry attached via WithContext, or nil.
func FromContext(ctx context.Context) *Registry {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(ctxKey{}).(*Registry)
	return r
}

// Register adds f, returns the unregister func.
func (r *Registry) Register(f *os.File) func() {
	if r == nil || f == nil {
		return func() {}
	}
	r.mu.Lock()
	r.files = append(r.files, f)
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, x := range r.files {
			if x == f {
				r.files = append(r.files[:i], r.files[i+1:]...)
				return
			}
		}
	}
}

// CloseAll closes every registered file, draining the registry.
func (r *Registry) CloseAll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	files := r.files
	r.files = nil
	r.mu.Unlock()
	for _, f := range files {
		_ = f.Close()
	}
}

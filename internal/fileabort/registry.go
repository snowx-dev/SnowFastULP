package fileabort

import (
	"context"
	"os"
	"sync"
)

type ctxKey struct{}

// Registry tracks open archive handles. CloseAll unblocks reads stuck in
// kernel I/O. Once closed, the registry stays closed (sticky): a Register
// after CloseAll closes the file immediately and returns a no-op unregister,
// so no descriptor can escape the abort sweep.
type Registry struct {
	mu     sync.Mutex
	files  []*os.File
	closed bool
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

// Register adds f, returns the unregister func. On a closed registry the file
// is closed immediately and the returned unregister is a no-op — the handle
// never joins the registry after the abort sweep.
func (r *Registry) Register(f *os.File) func() {
	if r == nil || f == nil {
		return func() {}
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = f.Close()
		return func() {}
	}
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

// CloseAll closes every registered file and marks the registry closed. The
// closed flag is set under the mutex BEFORE draining, so a concurrent Register
// observes it and closes its file instead of appending. Repeated calls are safe.
func (r *Registry) CloseAll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closed = true
	files := r.files
	r.files = nil
	r.mu.Unlock()
	for _, f := range files {
		_ = f.Close()
	}
}

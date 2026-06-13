package jobs

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// ProgressFn reports handler progress: the value is stored in the job's
// progress JSONB column (must marshal to a JSON object) and published on
// the Redis events channel. The returned error is informational — handlers
// may log it but should keep working; cancellation arrives via the handler
// context, never via this function.
type ProgressFn func(ctx context.Context, progress any) error

// Handler executes one job kind. Implementations must:
//   - honor ctx cancellation promptly (that is the cooperative-cancel and
//     drain mechanism — there is no other stop signal);
//   - be idempotent or tolerate re-execution (jobs are re-queued after
//     worker crashes and retried after failures);
//   - return a result that marshals to a JSON object (or nil).
type Handler interface {
	// Kind is the dispatch key this handler serves (dotted lowercase).
	Kind() string
	// Run executes the job. A nil error finishes the job succeeded with
	// the returned result; an error triggers retry/failure semantics
	// (context.Canceled while cancel was requested → canceled).
	Run(ctx context.Context, job *Job, report ProgressFn) (result any, err error)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc struct {
	K string
	F func(ctx context.Context, job *Job, report ProgressFn) (any, error)
}

// Kind implements Handler.
func (h HandlerFunc) Kind() string { return h.K }

// Run implements Handler.
func (h HandlerFunc) Run(ctx context.Context, job *Job, report ProgressFn) (any, error) {
	return h.F(ctx, job, report)
}

// Registry maps job kinds to handlers. Register at startup, Resolve from
// worker goroutines; both are safe concurrently.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Handler
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{handlers: make(map[string]Handler)}
}

// Register adds a handler; a duplicate kind or empty kind is a programming
// error and fails loudly.
func (r *Registry) Register(h Handler) error {
	kind := h.Kind()
	if kind == "" {
		return fmt.Errorf("jobs: registry: handler with empty kind (%T)", h)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.handlers[kind]; dup {
		return fmt.Errorf("jobs: registry: duplicate handler for kind %q", kind)
	}
	r.handlers[kind] = h
	return nil
}

// MustRegister is Register that panics — for static startup wiring where a
// duplicate is unrecoverable.
func (r *Registry) MustRegister(h Handler) {
	if err := r.Register(h); err != nil {
		panic(err)
	}
}

// Resolve returns the handler for kind, or nil when unregistered.
func (r *Registry) Resolve(kind string) Handler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.handlers[kind]
}

// Kinds lists registered kinds, sorted (startup logging).
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.handlers))
	for k := range r.handlers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

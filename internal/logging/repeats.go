package logging

import (
	"context"
	"log/slog"
	"sync"
)

// Repeats lowers to debug a message logged under the same key in the current
// or the previous pass, so a problem that persists across passes is reported
// once, and again only after a pass in which it was absent. A key may carry
// several messages in one pass; each is tracked on its own. A nil Repeats
// lowers nothing.
type Repeats struct {
	mu   sync.Mutex
	prev map[string]struct{}
	cur  map[string]struct{}
}

// NewRepeats returns an empty Repeats.
func NewRepeats() *Repeats {
	return &Repeats{prev: map[string]struct{}{}, cur: map[string]struct{}{}}
}

// NextPass starts a pass. Messages not seen in the pass that just ended are
// forgotten.
func (r *Repeats) NextPass() {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.prev, r.cur = r.cur, map[string]struct{}{}
}

// Level records message under key and returns level for a first sighting,
// or debug for a repeat.
func (r *Repeats) Level(key, message string, level slog.Level) slog.Level {
	if r == nil {
		return level
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	entry := key + "\x00" + message

	_, inCur := r.cur[entry]
	_, inPrev := r.prev[entry]
	r.cur[entry] = struct{}{}

	if inCur || inPrev {
		return slog.LevelDebug
	}

	return level
}

// WithRepeats returns a context that carries repeats.
func WithRepeats(ctx context.Context, repeats *Repeats) context.Context {
	return context.WithValue(ctx, repeatsKey, repeats)
}

// RepeatsFromContext returns the Repeats ctx carries, or nil, which lowers
// nothing.
func RepeatsFromContext(ctx context.Context) *Repeats {
	repeats, _ := ctx.Value(repeatsKey).(*Repeats)

	return repeats
}

package controller

import (
	"context"
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// routeSyncRetrier runs the full route syncs that reconcilers other than the
// route reconcilers trigger, and owns their retry. Such a sync is global: it
// writes every route's status and every tunnel's document, and asks for a
// requeue when it leaves work that no event brings back. The caller that
// triggered it cannot own that retry, since its own state may already be
// settled, and many callers retrying one global sync would multiply its
// Cloudflare reads. So a sync that fails or asks for a requeue leaves one
// retry owed here, however many callers it came from, and the retrier runs it
// until a sync comes back clean.
type routeSyncRetrier struct {
	routeSync func(context.Context) (ctrl.Result, error)
	// interval spaces the retries of a failed sync that named no delay.
	interval time.Duration
	wake     chan struct{}
	// waitHook, when set, runs each time the loop starts waiting for a due
	// retry (tests).
	waitHook func()

	mu sync.Mutex
	// seq numbers the syncs in the order they start.
	seq uint64
	// isOwed is set by a failed sync, and owedSince is the start number of
	// the latest one: only a clean sync that started after it did that sync's
	// work, so only such a sync pays the debt.
	isOwed    bool
	owedSince uint64
	// dueAt is when the owed retry runs: the soonest time a failed sync asked
	// for since the last retry began. dueSet reports whether one has asked.
	dueAt  time.Time
	dueSet bool
}

func newRouteSyncRetrier(routeSync func(context.Context) (ctrl.Result, error), interval time.Duration) *routeSyncRetrier {
	return &routeSyncRetrier{routeSync: routeSync, interval: interval, wake: make(chan struct{}, 1)}
}

// Sync runs the route sync and returns its result, leaving a retry owed when
// it failed or asked for a requeue.
func (r *routeSyncRetrier) Sync(ctx context.Context) (ctrl.Result, error) {
	r.mu.Lock()
	r.seq++
	start := r.seq
	r.mu.Unlock()

	result, err := r.routeSync(ctx)
	r.record(start, result, err)

	return result, err
}

// record folds the outcome of the sync numbered start into the debt.
func (r *routeSyncRetrier) record(start uint64, result ctrl.Result, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err == nil && result.RequeueAfter == 0 {
		if start > r.owedSince {
			r.isOwed = false
			r.dueSet = false
		}

		return
	}

	delay := result.RequeueAfter
	if delay == 0 {
		delay = r.interval
	}

	r.isOwed = true
	r.owedSince = max(r.owedSince, start)

	if due := time.Now().Add(delay); !r.dueSet || due.Before(r.dueAt) {
		r.dueAt = due
		r.dueSet = true
	}

	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *routeSyncRetrier) owed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.isOwed
}

// untilDue reports whether a retry is owed and how long until it is due.
func (r *routeSyncRetrier) untilDue() (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.dueSet {
		return r.isOwed, r.interval
	}

	return r.isOwed, time.Until(r.dueAt)
}

// retry runs the owed sync once, unless a clean sync from another caller paid
// the debt while the retrier waited. Only the syncs that fail after the retry
// began set the next due time, so a short delay asked for once does not stick.
func (r *routeSyncRetrier) retry(ctx context.Context) {
	r.mu.Lock()
	owed := r.isOwed
	r.dueSet = false
	r.mu.Unlock()

	if !owed {
		return
	}

	if _, err := r.Sync(ctx); err != nil {
		log.FromContext(ctx).Error(err, "retried route sync failed")
	}
}

// Start runs the retry loop until ctx ends. A failed sync that asks for a
// sooner retry while the loop waits wakes it, and the wait is cut short.
func (r *routeSyncRetrier) Start(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.wake:
		}

		if !r.drain(ctx) {
			return nil
		}
	}
}

// drain retries while a retry is owed, and reports false when ctx ended.
func (r *routeSyncRetrier) drain(ctx context.Context) bool {
	for {
		owed, wait := r.untilDue()
		if !owed {
			return true
		}

		if wait > 0 {
			timer := time.NewTimer(wait)

			if r.waitHook != nil {
				r.waitHook()
			}

			select {
			case <-ctx.Done():
				timer.Stop()

				return false
			case <-r.wake:
				timer.Stop()

				continue
			case <-timer.C:
			}
		}

		r.retry(ctx)
	}
}

// NeedLeaderElection makes only the leader retry: only the leader syncs.
func (r *routeSyncRetrier) NeedLeaderElection() bool { return true }

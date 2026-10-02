package controller

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"
)

// startRetrier runs the retrier's loop until the test ends, and returns a
// channel that receives each time the loop starts waiting for a due retry.
func startRetrier(t *testing.T, retrier *routeSyncRetrier) <-chan struct{} {
	t.Helper()

	waiting := make(chan struct{}, 1)
	retrier.waitHook = func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- retrier.Start(ctx) }()

	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})

	return waiting
}

// countingSync is a fake route sync that answers call n with outcomes[n-1],
// repeating the last one, and reports each call on calls.
type countingSync struct {
	outcomes []ctrl.Result
	errs     []error
	n        atomic.Int32
	calls    chan int32
}

func newCountingSync(outcomes []ctrl.Result, errs []error) *countingSync {
	return &countingSync{outcomes: outcomes, errs: errs, calls: make(chan int32, 64)}
}

func (c *countingSync) sync(context.Context) (ctrl.Result, error) {
	n := c.n.Add(1)
	i := min(int(n), len(c.outcomes)) - 1
	c.calls <- n

	return c.outcomes[i], c.errs[i]
}

func receive(t *testing.T, ch <-chan int32) int32 {
	t.Helper()

	select {
	case n := <-ch:
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("the sync was not called")

		return 0
	}
}

func receiveWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("the retry loop did not start waiting")
	}
}

// TestRouteSyncRetrier_RetriesUntilTheSyncComesBackClean pins that a triggered
// route sync that failed or asked for a requeue is run again, by the retrier,
// until one comes back clean. The callers do not own that retry: the sync is
// global, and no route event brings it back.
func TestRouteSyncRetrier_RetriesUntilTheSyncComesBackClean(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name  string
		first ctrl.Result
		err   error
	}{
		{name: "the sync asked for a requeue", first: ctrl.Result{RequeueAfter: time.Millisecond}},
		{name: "the sync failed", err: errRouteSyncFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := newCountingSync(
				[]ctrl.Result{tt.first, tt.first, {}},
				[]error{tt.err, tt.err, nil})
			retrier := newRouteSyncRetrier(fake.sync, time.Millisecond)
			startRetrier(t, retrier)

			result, err := retrier.Sync(context.Background())
			assert.Equal(t, tt.first, result, "the caller still sees the sync's own result")
			require.ErrorIs(t, err, tt.err)

			for want := int32(1); want <= 3; want++ {
				assert.Equal(t, want, receive(t, fake.calls))
			}

			// The fake reports a call before its result is recorded.
			require.Eventually(t, func() bool { return !retrier.owed() }, 5*time.Second, time.Millisecond,
				"a clean sync ends the retries; the loop waits for a new debt")
		})
	}
}

// TestRouteSyncRetrier_CoalescesOwers pins that many callers whose syncs asked
// for a requeue cost one retry, not one each: every retry is a full route sync
// reading every tunnel from Cloudflare, and a cluster-wide condition, such as
// one tenant's broken tunnel, makes every sync ask.
func TestRouteSyncRetrier_CoalescesOwers(t *testing.T) {
	t.Parallel()

	var (
		syncs   atomic.Int32
		healthy atomic.Bool
	)

	retrier := newRouteSyncRetrier(func(context.Context) (ctrl.Result, error) {
		syncs.Add(1)

		if healthy.Load() {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{RequeueAfter: time.Hour}, nil
	}, time.Hour)

	for range 5 {
		_, _ = retrier.Sync(context.Background())
	}

	require.Equal(t, int32(5), syncs.Load(), "each caller's own sync runs")
	assert.True(t, retrier.owed(), "and leaves one retry owed")

	healthy.Store(true)
	retrier.retry(context.Background())

	assert.Equal(t, int32(6), syncs.Load(), "the five owers cost one retry")
	assert.False(t, retrier.owed(), "a clean retry pays the debt")
}

// TestRouteSyncRetrier_TakesEachRetrysOwnDelay pins that a retry waits the
// delay its own sync asked for. A short delay asked for once, after a lost push
// race say, must not stick while later syncs ask for the longer error delay:
// every retry is a full route sync reading every tunnel from Cloudflare.
func TestRouteSyncRetrier_TakesEachRetrysOwnDelay(t *testing.T) {
	t.Parallel()

	fake := newCountingSync(
		[]ctrl.Result{{RequeueAfter: time.Millisecond}, {RequeueAfter: time.Hour}},
		[]error{nil, nil})
	retrier := newRouteSyncRetrier(fake.sync, time.Hour)
	waiting := startRetrier(t, retrier)

	_, _ = retrier.Sync(context.Background())
	require.Equal(t, int32(1), receive(t, fake.calls))
	require.Equal(t, int32(2), receive(t, fake.calls), "the short delay was honoured")

	// The loop now waits for the retry the second sync asked for.
	for {
		receiveWait(t, waiting)

		if owed, wait := retrier.untilDue(); owed && wait > 30*time.Minute {
			break
		}
	}
}

// TestRouteSyncRetrier_SoonerRequestShortensTheWait pins that a failed sync
// asking for a sooner retry while the loop waits cuts the wait short, rather
// than waiting out the longer delay asked for first.
func TestRouteSyncRetrier_SoonerRequestShortensTheWait(t *testing.T) {
	t.Parallel()

	fake := newCountingSync(
		[]ctrl.Result{{RequeueAfter: time.Hour}, {RequeueAfter: time.Millisecond}, {}},
		[]error{nil, nil, nil})
	retrier := newRouteSyncRetrier(fake.sync, time.Hour)
	waiting := startRetrier(t, retrier)

	_, _ = retrier.Sync(context.Background())
	require.Equal(t, int32(1), receive(t, fake.calls))
	receiveWait(t, waiting)

	_, _ = retrier.Sync(context.Background())
	require.Equal(t, int32(2), receive(t, fake.calls))

	assert.Equal(t, int32(3), receive(t, fake.calls), "the retry ran after the sooner delay, not the hour")
}

// TestRouteSyncRetrier_DueTimeIsTheSoonestOwed pins how failures that arrive
// before the retry runs set its due time: the soonest request wins, so a later
// caller asking for a longer delay cannot push back a retry that is already
// due sooner, and a debt a clean sync paid leaves no due time behind for the
// next failure to inherit.
func TestRouteSyncRetrier_DueTimeIsTheSoonestOwed(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		outcomes []ctrl.Result
		soon     bool
	}{
		{
			name:     "a later, longer request",
			outcomes: []ctrl.Result{{RequeueAfter: time.Millisecond}, {RequeueAfter: time.Hour}},
			soon:     true,
		},
		{
			name:     "a request after a paid debt",
			outcomes: []ctrl.Result{{RequeueAfter: time.Millisecond}, {}, {RequeueAfter: time.Hour}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := newCountingSync(tt.outcomes, make([]error, len(tt.outcomes)))
			retrier := newRouteSyncRetrier(fake.sync, time.Hour)

			for range tt.outcomes {
				_, _ = retrier.Sync(context.Background())
			}

			owed, wait := retrier.untilDue()
			require.True(t, owed)

			if tt.soon {
				assert.Less(t, wait, time.Minute, "the sooner request still holds")
			} else {
				assert.Greater(t, wait, 30*time.Minute, "only the request after the paid debt counts")
			}
		})
	}
}

// TestRouteSyncRetrier_SkipsARetryAnotherSyncPaid pins that a retry whose debt
// a clean sync from another caller paid does not run: that sync already did
// the work, and the retry would be one more full route sync for nothing.
func TestRouteSyncRetrier_SkipsARetryAnotherSyncPaid(t *testing.T) {
	t.Parallel()

	fake := newCountingSync([]ctrl.Result{{RequeueAfter: time.Hour}, {}}, []error{nil, nil})
	retrier := newRouteSyncRetrier(fake.sync, time.Hour)

	_, _ = retrier.Sync(context.Background())
	_, _ = retrier.Sync(context.Background())
	require.False(t, retrier.owed())

	retrier.retry(context.Background())
	assert.Equal(t, int32(2), fake.n.Load(), "the paid retry did not run")
}

// TestRouteSyncRetrier_OlderCleanSyncKeepsANewerDebt pins that a clean sync
// pays only the debts of syncs that started before it. One that started
// earlier but finished after a failure did not do the failed sync's work, so
// the retry stays owed; otherwise a quiet cluster would never run it.
func TestRouteSyncRetrier_OlderCleanSyncKeepsANewerDebt(t *testing.T) {
	t.Parallel()

	t.Run("another caller", func(t *testing.T) {
		t.Parallel()

		started, release := make(chan struct{}), make(chan struct{})

		var calls atomic.Int32

		retrier := newRouteSyncRetrier(func(context.Context) (ctrl.Result, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-release

				return ctrl.Result{}, nil
			}

			return ctrl.Result{}, errRouteSyncFailed
		}, time.Hour)

		done := make(chan struct{})

		go func() {
			defer close(done)

			_, _ = retrier.Sync(context.Background())
		}()

		<-started

		_, err := retrier.Sync(context.Background())
		require.ErrorIs(t, err, errRouteSyncFailed)

		close(release)
		<-done

		assert.True(t, retrier.owed(), "the clean sync started before the failure")
	})

	t.Run("the retry itself", func(t *testing.T) {
		t.Parallel()

		started, release := make(chan struct{}), make(chan struct{})

		var calls atomic.Int32

		retrier := newRouteSyncRetrier(func(context.Context) (ctrl.Result, error) {
			switch calls.Add(1) {
			case 2:
				close(started)
				<-release

				return ctrl.Result{}, nil
			default:
				return ctrl.Result{}, errRouteSyncFailed
			}
		}, time.Hour)

		_, _ = retrier.Sync(context.Background())

		done := make(chan struct{})

		go func() {
			defer close(done)

			retrier.retry(context.Background())
		}()

		<-started

		_, err := retrier.Sync(context.Background())
		require.ErrorIs(t, err, errRouteSyncFailed)

		close(release)
		<-done

		assert.True(t, retrier.owed(), "the retry started before the failure")
	})
}

// TestRouteSyncRetrier_StopsWithItsContext pins that the retry loop ends when
// the manager stops it, as on losing leadership, even while it waits for an
// owed retry.
func TestRouteSyncRetrier_StopsWithItsContext(t *testing.T) {
	t.Parallel()

	retrier := newRouteSyncRetrier(func(context.Context) (ctrl.Result, error) {
		return ctrl.Result{RequeueAfter: time.Hour}, nil
	}, time.Hour)

	waiting := make(chan struct{}, 1)
	retrier.waitHook = func() { waiting <- struct{}{} }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- retrier.Start(ctx) }()

	_, _ = retrier.Sync(context.Background())
	receiveWait(t, waiting)
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the retry loop did not stop with its context")
	}

	assert.True(t, retrier.NeedLeaderElection(), "only the leader syncs")
}

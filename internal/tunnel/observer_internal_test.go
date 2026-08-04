package tunnel

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudflare/cloudflared/connection"
)

// recordingSink counts the events an Observer dispatches to it.
type recordingSink struct {
	events chan connection.Event
}

func newRecordingSink() *recordingSink {
	return &recordingSink{events: make(chan connection.Event, 8)}
}

func (s *recordingSink) OnTunnelEvent(event connection.Event) {
	select {
	case s.events <- event:
	default:
	}
}

// drain discards events already buffered from an earlier phase of a test.
// deliversWithin returns on the FIRST event it receives, so a live-phase send
// can still be sitting in the buffer when the test moves on to assert that
// nothing is delivered any more; without draining, that leftover reads as a
// post-Close delivery.
func (s *recordingSink) drain() {
	for {
		select {
		case <-s.events:
		default:
			return
		}
	}
}

// deliversWithin reports whether the observer reaches the sink inside the
// window. The send is retried because RegisterSink and the event send race in
// the vendored dispatch loop: a single event can be picked up before the
// pending registration and dispatched to nobody.
func deliversWithin(observer *connection.Observer, sink *recordingSink, window time.Duration) bool {
	deadline := time.Now().Add(window)

	for time.Now().Before(deadline) {
		observer.SendDisconnect(0)

		select {
		case <-sink.events:
			return true
		case <-time.After(10 * time.Millisecond):
		}
	}

	return false
}

// TestObserverClose_ReleasesSinksAndStopsDispatch pins the vendored-fork
// contract this package's per-attempt observer ownership rests on: after
// Close() the dispatch goroutine has returned, so the sinks registered against
// that observer are released and stop receiving events. Without it every
// bootstrap attempt's tunnelstate.ConnTracker would stay registered for the
// life of the process and be walked on every subsequent event.
func TestObserverClose_ReleasesSinksAndStopsDispatch(t *testing.T) {
	t.Parallel()

	zlog := newZerologLogger()
	observer := connection.NewObserver(&zlog, &zlog)

	sink := newRecordingSink()
	observer.RegisterSink(sink)

	require.True(t, deliversWithin(observer, sink, 2*time.Second),
		"a live observer must dispatch to its registered sinks")

	observer.Close()
	sink.drain()

	// Eventually, not a single shot: the vendored dispatch loop selects over
	// closeChan and the event channel with no priority between them, so an event
	// already pending when Close lands can still be delivered once. The contract
	// is that delivery STOPS, not that it stops on the very next send.
	assert.Eventually(t, func() bool {
		return !deliversWithin(observer, sink, 100*time.Millisecond)
	}, 2*time.Second, 50*time.Millisecond,
		"a closed observer must not dispatch to sinks it has released")

	// Close is idempotent: the retry loop closes each attempt's observer from a
	// defer, and a caller must never have to track whether it already did.
	assert.NotPanics(t, observer.Close, "Close must be safe to call more than once")

	// RegisterSink must not block once the dispatch goroutine is gone, or a
	// late registration would wedge the caller forever.
	done := make(chan struct{})

	go func() {
		defer close(done)

		observer.RegisterSink(newRecordingSink())
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RegisterSink blocked on a closed observer")
	}
}

// TestBuildTunnelConfig_ObserverIsPerAttempt pins that every bootstrap attempt
// gets its own Observer. Sharing one across attempts kept its sink list —
// one tunnelstate.ConnTracker per attempt, none of them removable — growing for
// as long as a retry loop ran, and every dispatched event walked the whole
// list. Sharing the tracker instead is not an option: the vendored supervisor
// reads HasConnectedWith off it to choose a fallback protocol, so one attempt's
// outcome would steer the next attempt's control flow.
func TestBuildTunnelConfig_ObserverIsPerAttempt(t *testing.T) {
	token := newTestToken()
	zlog := newZerologLogger()

	withFreshRegisterer(func() {
		ensureRetrySafeRegisterer()

		tunnelCfg1, _, err := buildTunnelConfig(t.Context(), token, "http://localhost:8080", connection.AutoSelectFlag, &zlog)
		require.NoError(t, err)
		require.NotNil(t, tunnelCfg1.Observer)

		tunnelCfg2, _, err := buildTunnelConfig(t.Context(), token, "http://localhost:8080", connection.AutoSelectFlag, &zlog)
		require.NoError(t, err)

		assert.NotSame(t, tunnelCfg1.Observer, tunnelCfg2.Observer,
			"each bootstrap attempt must own its Observer so closing the attempt releases its sinks")
	})
}

// TestNewAttemptObserver_ReleasedWhenAttemptContextEnds pins the ownership
// rule: nothing has to remember to dispose of an attempt's observer, because
// the attempt's own context releases it. An attempt that fails anywhere between
// building its config and returning still gives its sinks back.
func TestNewAttemptObserver_ReleasedWhenAttemptContextEnds(t *testing.T) {
	t.Parallel()

	attemptCtx, endAttempt := context.WithCancel(t.Context())
	zlog := newZerologLogger()
	observer := newAttemptObserver(attemptCtx, &zlog)

	sink := newRecordingSink()
	observer.RegisterSink(sink)
	require.True(t, deliversWithin(observer, sink, 2*time.Second),
		"an in-flight attempt's observer must dispatch to its sinks")

	endAttempt()
	sink.drain()

	assert.Eventually(t, func() bool {
		return !deliversWithin(observer, sink, 100*time.Millisecond)
	}, 2*time.Second, 50*time.Millisecond,
		"ending the attempt must release the observer's sinks")
}

// TestBuildTunnelConfig_ObserverReleasedWhenAttemptContextEnds pins the wiring
// between the two halves above: buildTunnelConfig must hand the ATTEMPT's
// context to the observer it builds. Handing it a background context instead
// revives the per-attempt sink leak while both halves keep passing — one
// observer per attempt is still true, and an observer still releases with its
// context; only the context it was given would be wrong.
func TestBuildTunnelConfig_ObserverReleasedWhenAttemptContextEnds(t *testing.T) {
	token := newTestToken()
	zlog := newZerologLogger()

	withFreshRegisterer(func() {
		ensureRetrySafeRegisterer()

		attemptCtx, endAttempt := context.WithCancel(t.Context())

		tunnelCfg, _, err := buildTunnelConfig(attemptCtx, token, "http://localhost:8080", connection.AutoSelectFlag, &zlog)
		require.NoError(t, err)
		require.NotNil(t, tunnelCfg.Observer)

		sink := newRecordingSink()
		tunnelCfg.Observer.RegisterSink(sink)
		require.True(t, deliversWithin(tunnelCfg.Observer, sink, 2*time.Second),
			"the attempt's observer must be live while the attempt runs")

		endAttempt()
		sink.drain()

		assert.Eventually(t, func() bool {
			return !deliversWithin(tunnelCfg.Observer, sink, 100*time.Millisecond)
		}, 2*time.Second, 50*time.Millisecond,
			"ending the attempt must release the observer buildTunnelConfig built for it")
	})
}

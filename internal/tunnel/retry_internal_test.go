package tunnel

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/tunnelrpc/pogs"
)

// testRetryDelays keeps the retry-loop tests fast: backoff waits use these
// instead of the real (seconds-to-minutes) defaults.
const (
	testBaseDelay = time.Millisecond
	testMaxDelay  = 4 * time.Millisecond
)

var errFakeRetryable = errors.New("fake: edge unreachable")

// identityJitter is injected in place of fullJitter so the loop-control-flow
// tests stay deterministic: the requested delay is used verbatim, with none
// of fullJitter's real randomness.
func identityJitter(d time.Duration) time.Duration { return d }

// Every test below that calls startTunnelWithRetry is NOT parallel and runs
// inside withFreshRegisterer: startTunnelWithRetry unconditionally calls
// ensureRetrySafeRegisterer, which reads and writes the package-level
// prometheus.DefaultRegisterer -- the same hazard withFreshRegisterer's own
// doc comment already warns about for the buildOrchestrator tests in
// bootstrap_internal_test.go.

func TestStartTunnelWithRetry_SucceedsOnFirstAttempt(t *testing.T) {
	var calls atomic.Int32

	dial := func(context.Context, *Config) error {
		calls.Add(1)

		return nil
	}

	withFreshRegisterer(func() {
		err := startTunnelWithRetry(t.Context(), &Config{}, make(chan struct{}), dial,
			testBaseDelay, testMaxDelay, identityJitter)

		require.NoError(t, err)
	})

	assert.Equal(t, int32(1), calls.Load())
}

func TestStartTunnelWithRetry_RetriesRetryableErrorThenSucceeds(t *testing.T) {
	var calls atomic.Int32

	dial := func(context.Context, *Config) error {
		n := calls.Add(1)
		if n < 3 {
			return errFakeRetryable
		}

		return nil
	}

	withFreshRegisterer(func() {
		err := startTunnelWithRetry(t.Context(), &Config{}, make(chan struct{}), dial,
			testBaseDelay, testMaxDelay, identityJitter)

		require.NoError(t, err)
	})

	assert.Equal(t, int32(3), calls.Load(), "must retry the retryable error until it succeeds")
}

func TestStartTunnelWithRetry_NonRetryableErrorReturnsImmediately(t *testing.T) {
	var calls atomic.Int32

	terminalErr := errors.Mark(errors.New("bad token"), errNonRetryableStart)
	dial := func(context.Context, *Config) error {
		calls.Add(1)

		return errors.Wrap(terminalErr, "dial")
	}

	var err error

	withFreshRegisterer(func() {
		err = startTunnelWithRetry(t.Context(), &Config{}, make(chan struct{}), dial,
			testBaseDelay, testMaxDelay, identityJitter)
	})

	require.Error(t, err)
	// A cockroachdb/errors Mark is a logical-identity match (message + type),
	// not a chain-linked sentinel -- testify's ErrorIs uses the stdlib
	// errors.Is, which does not know how to walk it. Check with the same
	// package that produced the mark, matching how production code classifies.
	assert.True(t, errors.Is(err, errNonRetryableStart), "err must classify as non-retryable")
	assert.Equal(t, int32(1), calls.Load(), "a non-retryable error must not be retried")
}

func TestStartTunnelWithRetry_PostConnectionFailureNotRetried(t *testing.T) {
	var calls atomic.Int32

	postConnectionErr := errors.New("tunnel daemon: all connections lost")
	dial := func(_ context.Context, cfg *Config) error {
		calls.Add(1)
		cfg.OnConnected()

		return postConnectionErr
	}

	var err error

	withFreshRegisterer(func() {
		err = startTunnelWithRetry(t.Context(), &Config{}, make(chan struct{}), dial,
			testBaseDelay, testMaxDelay, identityJitter)
	})

	require.Error(t, err)
	assert.Equal(t, postConnectionErr, err, "a failure after the tunnel connected must be returned unchanged")
	assert.Equal(t, int32(1), calls.Load(), "a post-connection failure must not be retried by the bootstrap loop")
}

func TestStartTunnelWithRetry_ForwardsOnConnectedToCaller(t *testing.T) {
	var callerNotified atomic.Bool

	cfg := &Config{
		OnConnected: func() { callerNotified.Store(true) },
	}

	dial := func(_ context.Context, attemptCfg *Config) error {
		attemptCfg.OnConnected()

		return errors.New("dies after connecting")
	}

	withFreshRegisterer(func() {
		_ = startTunnelWithRetry(t.Context(), cfg, make(chan struct{}), dial,
			testBaseDelay, testMaxDelay, identityJitter)
	})

	assert.True(t, callerNotified.Load(), "the caller-supplied OnConnected must still fire")
}

func TestStartTunnelWithRetry_DrainDuringBackoffBreaksLoop(t *testing.T) {
	var calls atomic.Int32

	dial := func(context.Context, *Config) error {
		calls.Add(1)

		return errFakeRetryable
	}

	drainC := make(chan struct{})
	close(drainC)

	var err error

	// A large backoff proves the loop returned because of the drain signal,
	// not because the timer happened to fire first.
	withFreshRegisterer(func() {
		err = startTunnelWithRetry(t.Context(), &Config{}, drainC, dial,
			time.Hour, time.Hour, identityJitter)
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled, "a drain mid-backoff must resolve as a clean shutdown, not a failure")
	assert.Equal(t, int32(1), calls.Load(), "the loop must not dial again after the drain fires")
}

func TestStartTunnelWithRetry_ContextCancelledDuringBackoffBreaksLoop(t *testing.T) {
	var calls atomic.Int32

	dial := func(context.Context, *Config) error {
		calls.Add(1)

		return errFakeRetryable
	}

	ctx, cancel := context.WithCancel(t.Context())

	// The context is still live for attempt 1 (proving the loop reaches the
	// backoff wait, not the already-done short-circuit) and is only
	// cancelled once the loop is parked in the select below the large
	// backoff -- pinning the ctx.Done() interruption path specifically.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	var err error

	withFreshRegisterer(func() {
		err = startTunnelWithRetry(ctx, &Config{}, make(chan struct{}), dial,
			time.Hour, time.Hour, identityJitter)
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int32(1), calls.Load(), "the loop must not dial again once the context is done")
}

func TestStartTunnelWithRetry_ContextAlreadyDoneSkipsRetry(t *testing.T) {
	var calls atomic.Int32

	dial := func(context.Context, *Config) error {
		calls.Add(1)

		return errFakeRetryable
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var err error

	withFreshRegisterer(func() {
		err = startTunnelWithRetry(ctx, &Config{}, make(chan struct{}), dial,
			testBaseDelay, testMaxDelay, identityJitter)
	})

	require.Error(t, err)
	assert.Equal(t, int32(1), calls.Load(),
		"a dial failure observed with an already-done context must not be retried")
	// The raw dial error is not guaranteed to already be context.Canceled-
	// flavored (cloudflared's own shutdown handling can wrap it differently
	// depending on exactly where it was cancelled), but a shutdown must
	// still read as one to main.go's "!errors.Is(err, context.Canceled)"
	// exit(1) gate -- otherwise a SIGTERM landing in this exact window would
	// spuriously exit non-zero and log an ERROR for what is a clean shutdown.
	assert.ErrorIs(t, err, context.Canceled,
		"a failure observed with an already-done context must read as a clean shutdown, not a crash")
}

// TestStartTunnelWithRetry_UsesInjectedJitter pins that the loop's sleep on
// each attempt goes through the jitter function -- not the raw exponential
// delay -- and that jitter is fed the correctly-doubling pre-jitter delay on
// each successive attempt.
func TestStartTunnelWithRetry_UsesInjectedJitter(t *testing.T) {
	var calls atomic.Int32

	dial := func(context.Context, *Config) error {
		n := calls.Add(1)
		if n < 3 {
			return errFakeRetryable
		}

		return nil
	}

	var jitterInputs []time.Duration

	jitter := func(d time.Duration) time.Duration {
		jitterInputs = append(jitterInputs, d)

		return time.Microsecond // near-instant regardless of d, keeps the test fast
	}

	var err error

	withFreshRegisterer(func() {
		err = startTunnelWithRetry(t.Context(), &Config{}, make(chan struct{}), dial,
			testBaseDelay, testMaxDelay, jitter)
	})

	require.NoError(t, err)
	require.Equal(t, []time.Duration{testBaseDelay, 2 * testBaseDelay}, jitterInputs,
		"jitter must see the doubling pre-jitter delay on each attempt, not a fixed value")
}

func TestNextBootstrapRetryDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		current time.Duration
		max     time.Duration
		want    time.Duration
	}{
		{name: "doubles under the cap", current: 2 * time.Second, max: 30 * time.Second, want: 4 * time.Second},
		{name: "doubling exceeds the cap", current: 20 * time.Second, max: 30 * time.Second, want: 30 * time.Second},
		{name: "already at the cap stays capped", current: 30 * time.Second, max: 30 * time.Second, want: 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, nextBootstrapRetryDelay(tt.current, tt.max))
		})
	}
}

// TestFullJitter_WithinRange pins the "full jitter" contract: the returned
// wait is always in [0, ceiling]. Run many iterations since the function is
// randomized -- a bug that only breaks the bound on some seeds still needs to
// be caught.
func TestFullJitter_WithinRange(t *testing.T) {
	t.Parallel()

	const ceiling = 30 * time.Second

	for range 500 {
		got := fullJitter(ceiling)

		if got < 0 || got > ceiling {
			t.Fatalf("fullJitter(%s) = %s, want in [0, %s]", ceiling, got, ceiling)
		}
	}
}

// TestFullJitter_ZeroCeilingReturnsZero guards the degenerate input: a zero
// or negative ceiling must return zero rather than panicking (math/rand/v2's
// N-family panics on a non-positive bound).
func TestFullJitter_ZeroCeilingReturnsZero(t *testing.T) {
	t.Parallel()

	assert.Equal(t, time.Duration(0), fullJitter(0))
	assert.Equal(t, time.Duration(0), fullJitter(-time.Second))
}

// fakeRegisterer is a minimal prometheus.Registerer whose Register always
// returns a caller-supplied error, so tests can force a specific failure
// without needing a genuine non-AlreadyRegisteredError condition out of the
// real prometheus registry.
type fakeRegisterer struct {
	registerErr error
}

func (f fakeRegisterer) Register(prometheus.Collector) error { return f.registerErr }
func (f fakeRegisterer) MustRegister(...prometheus.Collector) {
	panic("fakeRegisterer.MustRegister is not exercised by these tests")
}

func (f fakeRegisterer) Unregister(prometheus.Collector) bool { return false }

// TestRetrySafeRegisterer_SwallowsAlreadyRegistered pins the core behavior:
// registering a fresh collector describing an already-registered metric
// (same name/labels, different object -- exactly what a second
// buildOrchestrator call produces) must not panic. Uses its own local
// registry, not prometheus.DefaultRegisterer, so it is safe to parallelize.
func TestRetrySafeRegisterer_SwallowsAlreadyRegistered(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	wrapped := retrySafeRegisterer{reg}

	wrapped.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "wt601_test_items_total", Help: "h"}))

	assert.NotPanics(t, func() {
		wrapped.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "wt601_test_items_total", Help: "h"}))
	})
}

// TestRetrySafeRegisterer_PanicsOnNonDuplicateError pins that the swallow is
// scoped to AlreadyRegisteredError specifically: a genuine registration
// failure (invalid descriptor, inconsistent labels, etc.) must still panic,
// matching MustRegister's documented contract, not be silently dropped. Uses
// a fake Registerer, not prometheus.DefaultRegisterer, so it is safe to
// parallelize.
func TestRetrySafeRegisterer_PanicsOnNonDuplicateError(t *testing.T) {
	t.Parallel()

	genuineErr := errors.New("boom: not a duplicate registration")
	wrapped := retrySafeRegisterer{fakeRegisterer{registerErr: genuineErr}}

	assert.PanicsWithError(t, genuineErr.Error(), func() {
		wrapped.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "wt601_test_items2_total", Help: "h"}))
	})
}

// TestEnsureRetrySafeRegisterer_Idempotent pins that calling it more than
// once (which happens on every bootstrap attempt after the first) is safe:
// the registerer stays usable and does not nest wrappers indefinitely.
//
// NOT parallel: temporarily replaces prometheus.DefaultRegisterer.
func TestEnsureRetrySafeRegisterer_Idempotent(t *testing.T) {
	withFreshRegisterer(func() {
		ensureRetrySafeRegisterer()
		ensureRetrySafeRegisterer()
		ensureRetrySafeRegisterer()

		_, wrapped := prometheus.DefaultRegisterer.(retrySafeRegisterer)
		assert.True(t, wrapped)

		assert.NotPanics(t, func() {
			prometheus.DefaultRegisterer.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "wt601_test_items3_total", Help: "h"}))
			prometheus.DefaultRegisterer.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "wt601_test_items3_total", Help: "h"}))
		})
	})
}

// TestStartTunnelWithRetry_InstallsRetrySafeRegisterer pins the wiring: the
// retry loop itself must install the fix, not just the pieces it is built
// from -- a caller of StartTunnelWithRetry should never need to know this
// detail exists.
//
// NOT parallel: temporarily replaces prometheus.DefaultRegisterer.
func TestStartTunnelWithRetry_InstallsRetrySafeRegisterer(t *testing.T) {
	dial := func(context.Context, *Config) error { return nil }

	withFreshRegisterer(func() {
		_, alreadyWrapped := prometheus.DefaultRegisterer.(retrySafeRegisterer)
		require.False(t, alreadyWrapped, "test setup: registerer must start unwrapped")

		err := startTunnelWithRetry(t.Context(), &Config{}, make(chan struct{}), dial,
			testBaseDelay, testMaxDelay, identityJitter)
		require.NoError(t, err)

		_, wrapped := prometheus.DefaultRegisterer.(retrySafeRegisterer)
		assert.True(t, wrapped, "startTunnelWithRetry must install the retry-safe registerer")
	})
}

const (
	logBootstrapRetry        = "tunnel bootstrap dial failed, retrying"
	logRegistrationRejected  = "the edge rejected this connector's registration, retrying"
	registrationRejectedHint = "check the tunnel token and that the tunnel still exists"
)

// retryLogFor drives startTunnelWithRetry with dial failing twice with err and
// then succeeding, and returns how many times it dialed and what it logged.
func retryLogFor(t *testing.T, err error) (int32, string) {
	t.Helper()

	var (
		calls  atomic.Int32
		logged bytes.Buffer
	)

	dial := func(context.Context, *Config) error {
		if calls.Add(1) < 3 {
			return err
		}

		return nil
	}

	cfg := &Config{Logger: slog.New(slog.NewTextHandler(&logged, nil))}

	withFreshRegisterer(func() {
		require.NoError(t, startTunnelWithRetry(t.Context(), cfg, make(chan struct{}), dial,
			testBaseDelay, testMaxDelay, identityJitter))
	})

	return calls.Load(), logged.String()
}

// TestStartTunnelWithRetry_PermanentRegistrationRejectionIsRetriedAndNamed pins
// the handling of a registration the edge marks Permanent: cloudflared reads an
// Unauthorized response on a freshly created tunnel as possibly transient, so
// the loop keeps retrying, but each attempt is logged with a line that tells
// the operator what to check instead of the generic dial-failure one.
func TestStartTunnelWithRetry_PermanentRegistrationRejectionIsRetriedAndNamed(t *testing.T) {
	rejected := errors.Wrap(connection.ServerRegisterTunnelError{
		Cause:     errors.New("Unauthorized: failed to get tunnel"),
		Permanent: true,
	}, "initialize supervisor")

	calls, logged := retryLogFor(t, rejected)

	assert.Equal(t, int32(3), calls, "a Permanent rejection must still be retried")
	assert.Equal(t, 2, strings.Count(logged, logRegistrationRejected),
		"each rejected attempt must log the distinct line once")
	assert.Contains(t, logged, registrationRejectedHint)
	assert.Contains(t, logged, "Unauthorized: failed to get tunnel", "the line must carry the error")
	assert.NotContains(t, logged, logBootstrapRetry,
		"the distinct line replaces the generic one rather than doubling it")
}

// TestStartTunnelWithRetry_RetryableRegistrationErrorKeepsGenericLog is the
// other half: a registration error the edge did not mark Permanent is an
// ordinary retry and must not send the operator to check the token.
func TestStartTunnelWithRetry_RetryableRegistrationErrorKeepsGenericLog(t *testing.T) {
	transient := errors.Wrap(connection.ServerRegisterTunnelError{
		Cause:     errors.New("edge is busy"),
		Permanent: false,
	}, "initialize supervisor")

	calls, logged := retryLogFor(t, transient)

	assert.Equal(t, int32(3), calls)
	assert.Equal(t, 2, strings.Count(logged, logBootstrapRetry))
	assert.NotContains(t, logged, logRegistrationRejected)
}

// rpcTransportError stands in for a registration RPC that failed in transport. It
// unwraps to a *pogs.RPCError the way cloudflared's wrapRPCError result does;
// it cannot be that value itself, since the zero RPCError's Error() panics.
type rpcTransportError struct{}

func (rpcTransportError) Error() string { return "rpc: control stream closed" }

func (rpcTransportError) Unwrap() error { return &pogs.RPCError{} }

// TestStartTunnelWithRetry_RegistrationRPCFailureKeepsGenericLog pins that a
// registration the RPC layer failed to complete is not reported as the edge
// refusing the token. cloudflared marks every registration error Permanent
// unless the edge said to retry, including a timeout or a dropped control
// stream, which are network problems the token has nothing to do with.
func TestStartTunnelWithRetry_RegistrationRPCFailureKeepsGenericLog(t *testing.T) {
	failed := errors.Wrap(connection.ServerRegisterTunnelError{
		Cause:     rpcTransportError{},
		Permanent: true,
	}, "initialize supervisor")

	calls, logged := retryLogFor(t, failed)

	assert.Equal(t, int32(3), calls)
	assert.Equal(t, 2, strings.Count(logged, logBootstrapRetry))
	assert.NotContains(t, logged, logRegistrationRejected)
}

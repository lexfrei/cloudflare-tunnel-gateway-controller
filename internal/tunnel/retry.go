package tunnel

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// defaultBootstrapRetryBaseDelay is the first backoff wait after a
	// retryable bootstrap-dial failure.
	defaultBootstrapRetryBaseDelay = 2 * time.Second
	// defaultBootstrapRetryMaxDelay caps the exponential backoff so a
	// persistent outage still retries every 30s rather than growing
	// unboundedly.
	defaultBootstrapRetryMaxDelay = 30 * time.Second
)

// errNonRetryableStart marks StartTunnel errors that stem from invalid
// input -- a tunnel token that does not parse, an unsupported protocol or
// TLS setting -- rather than a network condition. Retrying with the same
// Config reproduces the identical failure, so StartTunnelWithRetry
// classifies via errors.Is against this sentinel instead of matching
// cloudflared's error text (which is not a stable contract across vendor
// bumps) and returns immediately rather than backing off.
//
// Deliberately NOT covered: a well-formed tunnel token that the Cloudflare
// edge rejects (revoked, deleted tunnel, wrong secret) rather than a token
// that fails to parse. That distinction is made inside the vendored
// cloudflared supervisor -- connection.ServerRegisterTunnelError carries a
// Permanent bool from the registration RPC -- but it never crosses back out
// to this package: the error is unwrapped to its Cause and the Permanent
// bit consumed as a plain retry/no-retry decision before StartTunnel ever
// sees it (supervisor/tunnel.go's serveTunnel, in the vendored fork).
// cloudflared itself treats an "Unauthorized" registration response as
// possibly transient (edge propagation lag on a newly created tunnel) and
// retries it internally for the same reason. Matching on that string from
// here would be exactly the fragile, upgrade-breaking coupling the vendor
// rebase discipline in CLAUDE.md warns against, so a rejected-but-parseable
// token falls through to the retryable path: it retries indefinitely with
// capped backoff, staying NotReady and logging the error on every attempt
// instead of exiting. See docs/operations/troubleshooting.md for the
// operator-facing diagnosis of that case.
var errNonRetryableStart = errors.New("non-retryable tunnel start error")

// markNonRetryable tags err with errNonRetryableStart without altering its
// Error() text (Wrap with an empty message adds only a stack frame, no
// prefix -- see cockroachdb/errors' WrapWithDepth). StartTunnel calls this
// at its deterministic, config-derived failure points so
// StartTunnelWithRetry can classify them via errors.Is.
func markNonRetryable(err error) error {
	return errors.Wrap(errors.Mark(err, errNonRetryableStart), "")
}

// ensureRetrySafeRegisterer swaps prometheus.DefaultRegisterer for a
// wrapper that tolerates re-registering the same collector, unless it is
// already wrapped. StartTunnelWithRetry calls this before every attempt.
//
// It exists because a bootstrap retry calls StartTunnel -- and therefore
// buildOrchestrator and, further in, the vendored supervisor.NewSupervisor
// -- more than once in the same process. Both unconditionally MustRegister
// cloudflared's own Prometheus collectors on prometheus.DefaultRegisterer
// (origins.NewMetrics inside buildTunnelConfig; v3.NewMetrics inside
// NewSupervisor), written assuming "construct once per process." A second
// bootstrap attempt registering the exact same collector a second time is
// not a bug to report -- it is the still-registered collector from the
// first (failed) attempt, so client_golang's AlreadyRegisteredError for
// that specific case is swallowed rather than left to panic. A genuine
// registration failure (a different metric, malformed descriptor) is not
// an AlreadyRegisteredError and still panics.
//
// buildOrchestrator's origins.NewMetrics call is the only one this package
// controls directly and could otherwise fix by memoizing instead (reusing
// one collector across attempts keeps that specific metric's value alive
// across retries); v3.NewMetrics is inside the vendored fork with no
// injection point for a memoized registerer, so this wrapper is the only
// fix available without a fork patch. One mechanism for both is simpler
// than two, and it comes at a bounded, documented cost: the collector
// object from a failed attempt is orphaned (unregistered but referenced by
// that attempt's already-discarded origin dialer/DNS resolver), so these
// specific cloudflared-internal metrics stop advancing until the process
// actually connects or restarts. Neither collector is on the proxy's own
// documented metrics surface.
func ensureRetrySafeRegisterer() {
	if _, ok := prometheus.DefaultRegisterer.(retrySafeRegisterer); ok {
		return
	}

	prometheus.DefaultRegisterer = retrySafeRegisterer{prometheus.DefaultRegisterer}
}

// retrySafeRegisterer wraps a prometheus.Registerer so MustRegister no
// longer panics when a collector describing an already-registered metric
// (same name and labels, not necessarily the same object) is registered
// again. See ensureRetrySafeRegisterer for why that happens on a bootstrap
// retry, and why swallowing it here is safe.
type retrySafeRegisterer struct {
	prometheus.Registerer
}

func (r retrySafeRegisterer) MustRegister(collectors ...prometheus.Collector) {
	for _, c := range collectors {
		err := r.Register(c)
		if err == nil {
			continue
		}

		var alreadyRegistered prometheus.AlreadyRegisteredError
		if errors.As(err, &alreadyRegistered) {
			continue
		}

		panic(err)
	}
}

// StartTunnelWithRetry wraps StartTunnel in a capped exponential-backoff
// retry loop for the BOOTSTRAP dial: the window before the tunnel has ever
// registered a connection with the Cloudflare edge. A transient failure in
// that window -- cluster DNS not yet reachable after a node reboot, the edge
// briefly unreachable -- retries instead of returning, so the caller does
// not exit and crash-loop while the condition clears on its own.
//
// The retry is deliberately scoped to the bootstrap window only. cloudflared
// reconnects transparently on its own once a connection has been
// established (that is what Config.OnConnected observes); if StartTunnel
// still returns an error after OnConnected has already fired once, that
// means cloudflared exhausted its own reconnect budget entirely, which is a
// different failure than "never got up in the first place" and is returned
// unchanged here. Retrying it silently would be actively worse than the
// previous exit(1): proxy.Router's tunnel-connected readiness gate is a
// one-way latch (see its doc comment) that never flips back to NotReady, so
// a pod stuck in this state would keep reporting Ready while unable to serve
// any traffic.
//
// A non-retryable failure (see errNonRetryableStart) also returns
// immediately -- retrying a deterministic misconfiguration never succeeds.
//
// A close of drainC breaks the loop promptly, even mid-backoff-wait,
// mirroring how proxy.ResolveStartupProtocol already handles a drain during
// its own startup wait -- so a SIGTERM during a long backoff sleep does not
// burn the pod's termination grace period.
func StartTunnelWithRetry(ctx context.Context, cfg *Config, drainC <-chan struct{}) error {
	return startTunnelWithRetry(ctx, cfg, drainC, StartTunnel,
		defaultBootstrapRetryBaseDelay, defaultBootstrapRetryMaxDelay, fullJitter)
}

// runBootstrapAttempt runs a single dial attempt with a fresh, wrapped
// OnConnected so the caller can observe whether the tunnel connected during
// THIS attempt specifically, without disturbing the caller-supplied
// OnConnected (still invoked, so readiness still flips on a real connect).
func runBootstrapAttempt(
	ctx context.Context,
	cfg *Config,
	dial func(context.Context, *Config) error,
) (bool, error) {
	var connectedFlag atomic.Bool

	attemptCfg := *cfg
	userOnConnected := cfg.OnConnected
	attemptCfg.OnConnected = func() {
		connectedFlag.Store(true)

		if userOnConnected != nil {
			userOnConnected()
		}
	}

	// Sequenced as two statements, not "return connectedFlag.Load(),
	// dial(...)": Go evaluates return-expression operands left to right, so
	// that form would read connectedFlag BEFORE dial ever runs -- always
	// false, since dial is what invokes attemptCfg.OnConnected and sets it.
	err := dial(ctx, &attemptCfg)

	return connectedFlag.Load(), err
}

// startTunnelWithRetry is the testable core of StartTunnelWithRetry. dial,
// baseDelay, maxDelay, and jitter are injected so tests can drive the loop's
// control flow -- classification, backoff growth, drain/context interruption
// -- deterministically, without dialing cloudflared, waiting out the real
// backoff durations, or fighting real randomness.
func startTunnelWithRetry(
	ctx context.Context,
	cfg *Config,
	drainC <-chan struct{},
	dial func(context.Context, *Config) error,
	baseDelay, maxDelay time.Duration,
	jitter func(time.Duration) time.Duration,
) error {
	// Before the first attempt, not once per attempt: the check inside is
	// already idempotent, and installing it before dial's first call covers
	// the same-process double-dial hazard regardless of which attempt first
	// hits it.
	ensureRetrySafeRegisterer()

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	delay := baseDelay

	for attempt := 1; ; attempt++ {
		connected, err := runBootstrapAttempt(ctx, cfg, dial)
		if err == nil {
			return nil
		}

		if connected {
			// Connected at least once during this attempt -- out of scope
			// for the bootstrap retry, see the doc comment above.
			return err
		}

		if errors.Is(err, errNonRetryableStart) {
			return err
		}

		if ctx.Err() != nil {
			// Not "return err": the raw dial error is not guaranteed to
			// already be context.Canceled-flavored -- cloudflared's own
			// shutdown handling can wrap a cancellation differently
			// depending on exactly where it landed -- but it must still
			// read as a clean shutdown to main.go's
			// "!errors.Is(err, context.Canceled)" exit(1) gate. Otherwise a
			// SIGTERM landing in this exact window would spuriously exit
			// non-zero and log an ERROR for what is a clean shutdown.
			return errors.Wrap(ctx.Err(), "tunnel bootstrap retry")
		}

		wait := jitter(delay)

		// err.Error() (not err) -- cockroachdb/errors formats a raw error
		// value under slog's default "%+v" attribute encoding with an
		// attached stack trace, which would otherwise repeat on every
		// attempt and drown the log in noise across a long outage.
		logger.Error("tunnel bootstrap dial failed, retrying",
			"attempt", attempt, "error", err.Error(), "backoff", wait)

		select {
		case <-drainC:
			return context.Canceled
		case <-ctx.Done():
			return errors.Wrap(ctx.Err(), "tunnel bootstrap retry")
		case <-time.After(wait):
		}

		delay = nextBootstrapRetryDelay(delay, maxDelay)
	}
}

// nextBootstrapRetryDelay doubles the backoff delay, capped at maxDelay.
func nextBootstrapRetryDelay(current, maxDelay time.Duration) time.Duration {
	next := current * 2
	if next > maxDelay || next <= 0 {
		return maxDelay
	}

	return next
}

// fullJitter implements the "full jitter" backoff strategy: a uniformly
// random duration in [0, ceiling]. Every proxy replica that hit the same
// bootstrap failure at nearly the same time -- e.g. all of them dialing
// right after the same node reboot -- would otherwise retry in lockstep
// against the same recovering dependency; spreading their actual wait times
// out avoids that. A non-positive ceiling returns zero rather than panicking
// (math/rand/v2's N-family requires a positive bound).
func fullJitter(ceiling time.Duration) time.Duration {
	if ceiling <= 0 {
		return 0
	}

	return time.Duration(rand.Int64N(int64(ceiling) + 1)) //nolint:gosec // jitter timing, not a security context -- crypto/rand is unnecessary overhead here
}

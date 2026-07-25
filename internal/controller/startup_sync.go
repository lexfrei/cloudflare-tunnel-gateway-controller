package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/cockroachdb/errors"
	ctrl "sigs.k8s.io/controller-runtime"
)

// startupSyncRetryInterval spaces retries of a failed startup sync. It matches
// apiErrorRequeueDelay so the startup path retries transient API failures at
// the same rate as the reconcile path does.
const startupSyncRetryInterval = apiErrorRequeueDelay

// errRequeueRequested marks a degraded-success sync outcome: a nil error
// carrying a requeue interval. The startup retry loop treats it as a failed
// attempt (see startupAttempt).
var errRequeueRequested = errors.New("sync requested a requeue")

// startupSyncAttempt adapts a reconciler's full sync into the closure
// runStartupSync retries.
func startupSyncAttempt(params *syncUpdateParams) func(context.Context) error {
	return startupAttempt(params, syncAndUpdateStatusCommon)
}

// startupAttempt builds one retryable attempt over the given sync run. The
// attempt fails when the run returned an error, when a sync error was
// swallowed into a requeue interval (the reconcile-facing contract -- an
// error return would make controller-runtime override the interval), or when
// the proxy config push failed. The push matters as much as the sync: the
// initial push is what makes route-less proxy replicas ready, and a failed
// push leaves no cached config for endpoint-event resyncs to replay, so a
// sync-succeeded/push-failed attempt left standing would re-open the #581
// deadlock from the push side.
func startupAttempt(
	params *syncUpdateParams,
	run func(context.Context, *syncUpdateParams) (ctrl.Result, error),
) func(context.Context) error {
	return func(ctx context.Context) error {
		var syncErr, pushErr error

		params.onSyncError = func(err error) { syncErr = err }
		params.onPushError = func(err error) { pushErr = err }

		result, err := run(ctx, params)
		if err != nil {
			return err
		}

		if syncErr != nil {
			return syncErr
		}

		// A requeue request with a nil error is a degraded success: a tunnel
		// group or a per-Gateway resolve failed transiently and the sync
		// wants to be re-run (e.g. a GatewayConfig not yet visible leaves its
		// dedicated data plane without any config push). On a route-less
		// cluster this loop is the only requeue channel, so honor it. This
		// stays bounded: deterministic misconfigurations do not set a requeue
		// interval, only transient failures do.
		if result.RequeueAfter > 0 {
			return errors.Wrapf(errRequeueRequested, "requeue interval %s", result.RequeueAfter)
		}

		return pushErr
	}
}

// runStartupSync performs a route controller's initial full sync, retrying
// until it succeeds or ctx is cancelled.
//
// markComplete fires after the FIRST attempt regardless of outcome: route
// reconciles are gated only on the initial sync having been attempted, and
// they requeue through the same sync path, so blocking them on success would
// add nothing but latency.
//
// The retry loop is load-bearing on a fresh install: the initial sync is the
// only thing that pushes a config to route-less proxy replicas, and proxy
// readiness requires a received config. A one-shot sync that lost the startup
// race against GatewayClassConfig visibility left the data plane permanently
// configless and deadlocked helm --wait (#581).
func runStartupSync(
	ctx context.Context,
	logger *slog.Logger,
	interval time.Duration,
	markComplete func(),
	sync func(context.Context) error,
) {
	err := sync(ctx)

	markComplete()

	if err == nil {
		logger.Info("startup sync completed successfully")

		return
	}

	// A failure caused by manager shutdown is not worth alarming logs.
	if ctx.Err() != nil {
		return
	}

	logger.Error("startup sync failed; retrying until it succeeds",
		"error", err, "retryInterval", interval.String())

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if retryErr := sync(ctx); retryErr != nil {
				if ctx.Err() != nil {
					return
				}

				logger.Warn("startup sync retry failed", "error", retryErr)

				continue
			}

			logger.Info("startup sync completed successfully")

			return
		}
	}
}

package proxy

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// Edge transport values understood by the tunnel layer. http2 and quic pin a
// single transport; auto lets cloudflared negotiate (QUIC with HTTP/2 fallback).
const (
	protocolHTTP2 = "http2"
	protocolQUIC  = "quic"
	protocolAuto  = "auto"
)

// ResolveStartupProtocol decides the edge transport the proxy should dial,
// given the operator-configured PROXY_TUNNEL_PROTOCOL value (auto|http2|quic or
// empty/unset, which means auto).
//
// An explicit "http2" or "quic" is returned immediately — the operator pinned
// it, so the proxy does not wait. For "auto" or the empty value the proxy waits
// up to `wait` for the first config to arrive on firstConfig: if that config
// carries a GRPCRoute it returns "http2" (gRPC needs http2 because cloudflared
// drops HTTP trailers over QUIC, losing grpc-status), otherwise it returns
// "auto". A wait timeout, a cancelled ctx, or a closed drain channel also
// resolve to "auto" so a no-route or shutting-down proxy never blocks the dial
// forever. drain matters during shutdown specifically: SIGTERM closes the
// drain channel while the context deliberately stays alive (two-stage
// shutdown), and a pod terminated before its first config push must not burn
// the startup wait out of its termination grace budget. This makes the
// default auto transport honest for gRPC without penalising non-gRPC
// deployments beyond the time to their first config push.
func ResolveStartupProtocol(
	ctx context.Context,
	configured string,
	firstConfig <-chan *Config,
	wait time.Duration,
	drain <-chan struct{},
	logger *slog.Logger,
) string {
	if logger == nil {
		logger = slog.Default()
	}

	switch strings.ToLower(strings.TrimSpace(configured)) {
	case protocolHTTP2:
		return protocolHTTP2
	case protocolQUIC:
		return protocolQUIC
	}

	// auto / unset: learn whether a GRPCRoute is served before dialing.
	select {
	case cfg := <-firstConfig:
		if cfg != nil && cfg.HasGRPCRoute {
			logger.Info("tunnel transport: upgrading auto to http2 because a GRPCRoute is present at startup " +
				"(cloudflared drops HTTP trailers over QUIC, so gRPC needs http2)")

			return protocolHTTP2
		}

		return protocolAuto
	case <-time.After(wait):
		logger.Info("tunnel transport: no config received within the startup window, dialing auto",
			"wait", wait.String())

		return protocolAuto
	case <-ctx.Done():
		return protocolAuto
	case <-drain:
		logger.Info("tunnel transport: drain signalled during the startup window, dialing auto immediately")

		return protocolAuto
	}
}

// GRPCRestartNeeded reports whether a freshly-applied config requires a proxy
// restart to serve gRPC. It is true when the config carries a GRPCRoute but the
// proxy dialed a non-http2 edge transport (auto or quic), which cannot carry the
// grpc-status trailer over QUIC. dialedProtocol is the transport chosen at
// startup; an empty value means the proxy has not dialed yet, so no restart is
// implied (the startup resolver still owns the decision).
func GRPCRestartNeeded(dialedProtocol string, hasGRPCRoute bool) bool {
	if !hasGRPCRoute {
		return false
	}

	dialed := strings.ToLower(strings.TrimSpace(dialedProtocol))

	return dialed != "" && dialed != protocolHTTP2
}

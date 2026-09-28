package proxy

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
)

// Edge transport values understood by the tunnel layer. http2 and quic pin a
// single transport; auto lets cloudflared negotiate (QUIC with HTTP/2 fallback).
const (
	protocolHTTP2 = "http2"
	protocolQUIC  = "quic"
	protocolAuto  = "auto"
)

// ErrNoFirstConfig is returned by AwaitFirstConfig when the wait runs out
// before the controller pushed a config.
var ErrNoFirstConfig = errors.New("no config received from the controller")

// ErrDrainBeforeFirstConfig is returned by AwaitFirstConfig when the drain
// channel closes before the first config arrived.
var ErrDrainBeforeFirstConfig = errors.New("drain signalled before the first config arrived")

// AwaitFirstConfig blocks until the first config arrives on firstConfig and
// returns it. A tunnel-mode proxy calls it before dialing the edge: the edge
// sends traffic to any connector registered for the tunnel, so a proxy that
// registers before it holds a config answers every request with a 404.
//
// The wait ends with ErrNoFirstConfig after wait, with the context's error
// when ctx ends, and with ErrDrainBeforeFirstConfig when drain closes. drain
// matters during shutdown specifically: SIGTERM closes it while the context
// deliberately stays alive (two-stage shutdown), and a pod terminated before
// its first config push must not burn the wait out of its termination grace
// budget.
func AwaitFirstConfig(
	ctx context.Context,
	firstConfig <-chan *Config,
	wait time.Duration,
	drain <-chan struct{},
) (*Config, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case cfg := <-firstConfig:
		return cfg, nil
	case <-timer.C:
		return nil, errors.Wrapf(ErrNoFirstConfig, "waited %s", wait)
	case <-ctx.Done():
		return nil, errors.Wrap(ctx.Err(), "waiting for the first config")
	case <-drain:
		return nil, ErrDrainBeforeFirstConfig
	}
}

// StartupProtocol decides the edge transport the proxy should dial, given the
// operator-configured PROXY_TUNNEL_PROTOCOL value (auto|http2|quic or
// empty/unset, which means auto) and the first config the proxy received.
//
// An explicit "http2" or "quic" is returned as is. For "auto" or the empty
// value it returns "http2" when the first config carries a GRPCRoute (gRPC
// needs http2 because cloudflared drops HTTP trailers over QUIC, losing
// grpc-status), and "auto" otherwise.
func StartupProtocol(configured string, first *Config, logger *slog.Logger) string {
	if logger == nil {
		logger = slog.Default()
	}

	switch strings.ToLower(strings.TrimSpace(configured)) {
	case protocolHTTP2:
		return protocolHTTP2
	case protocolQUIC:
		return protocolQUIC
	}

	if first != nil && first.HasGRPCRoute {
		logger.Info("tunnel transport: upgrading auto to http2 because a GRPCRoute is present at startup " +
			"(cloudflared drops HTTP trailers over QUIC, so gRPC needs http2)")

		return protocolHTTP2
	}

	return protocolAuto
}

// GRPCRestartNeeded reports whether a freshly-applied config requires a proxy
// restart to serve gRPC. It is true when the config carries a GRPCRoute but the
// proxy dialed a non-http2 edge transport (auto or quic), which cannot carry the
// grpc-status trailer over QUIC. dialedProtocol is the transport chosen at
// startup; an empty value means the proxy has not dialed yet, so no restart is
// implied (StartupProtocol still owns the decision).
func GRPCRestartNeeded(dialedProtocol string, hasGRPCRoute bool) bool {
	if !hasGRPCRoute {
		return false
	}

	dialed := strings.ToLower(strings.TrimSpace(dialedProtocol))

	return dialed != "" && dialed != protocolHTTP2
}

package proxy_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func grpcConfigChan(hasGRPC bool) <-chan *proxy.Config {
	ch := make(chan *proxy.Config, 1)
	ch <- &proxy.Config{HasGRPCRoute: hasGRPC}

	return ch
}

func TestResolveStartupProtocol(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		configured string
		firstCfg   <-chan *proxy.Config
		wait       time.Duration
		want       string
	}{
		// Explicit choices are honoured immediately, without waiting for config.
		{name: "explicit http2 returns immediately", configured: "http2", firstCfg: make(chan *proxy.Config), wait: time.Second, want: "http2"},
		{name: "explicit quic returns immediately", configured: "quic", firstCfg: make(chan *proxy.Config), wait: time.Second, want: "quic"},
		{name: "explicit http2 case-insensitive", configured: "HTTP2", firstCfg: make(chan *proxy.Config), wait: time.Second, want: "http2"},
		// auto/unset waits for the first config and upgrades to http2 only on gRPC.
		{name: "auto with gRPC upgrades to http2", configured: "auto", firstCfg: grpcConfigChan(true), wait: time.Second, want: "http2"},
		{name: "auto without gRPC stays auto", configured: "auto", firstCfg: grpcConfigChan(false), wait: time.Second, want: "auto"},
		{name: "empty with gRPC upgrades to http2", configured: "", firstCfg: grpcConfigChan(true), wait: time.Second, want: "http2"},
		{name: "empty without gRPC stays auto", configured: "", firstCfg: grpcConfigChan(false), wait: time.Second, want: "auto"},
		{name: "padded auto with gRPC upgrades", configured: " auto ", firstCfg: grpcConfigChan(true), wait: time.Second, want: "http2"},
		// No config arrives within the window: fall back to auto, never block forever.
		{name: "auto with no config in window stays auto", configured: "auto", firstCfg: make(chan *proxy.Config), wait: 10 * time.Millisecond, want: "auto"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := proxy.ResolveStartupProtocol(context.Background(), tt.configured, tt.firstCfg, tt.wait, slog.Default())
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestResolveStartupProtocol_CtxCancelled proves a shutdown signal during the
// auto-wait does not hang the proxy: a cancelled context resolves to auto
// immediately instead of blocking for the full wait.
func TestResolveStartupProtocol_CtxCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := proxy.ResolveStartupProtocol(ctx, "auto", make(chan *proxy.Config), time.Hour, slog.Default())
	assert.Equal(t, "auto", got)
}

func TestGRPCRestartNeeded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		dialed  string
		hasGRPC bool
		want    bool
	}{
		{name: "no gRPC never needs restart", dialed: "quic", hasGRPC: false, want: false},
		{name: "not yet dialed (empty) does not warn", dialed: "", hasGRPC: true, want: false},
		{name: "dialed http2 serves gRPC fine", dialed: "http2", hasGRPC: true, want: false},
		{name: "dialed http2 case-insensitive", dialed: "HTTP2", hasGRPC: true, want: false},
		{name: "dialed auto plus gRPC needs restart", dialed: "auto", hasGRPC: true, want: true},
		{name: "dialed quic plus gRPC needs restart", dialed: "quic", hasGRPC: true, want: true},
		{name: "dialed auto without gRPC is fine", dialed: "auto", hasGRPC: false, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, proxy.GRPCRestartNeeded(tt.dialed, tt.hasGRPC))
		})
	}
}

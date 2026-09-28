package proxy_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func TestStartupProtocol(t *testing.T) {
	t.Parallel()

	withGRPC := &proxy.Config{Version: 1, HasGRPCRoute: true}
	withoutGRPC := &proxy.Config{Version: 1}

	tests := []struct {
		name       string
		configured string
		first      *proxy.Config
		want       string
	}{
		// Explicit choices are honoured whatever the first config carries.
		{name: "explicit http2", configured: "http2", first: withoutGRPC, want: "http2"},
		{name: "explicit quic stays quic even with gRPC", configured: "quic", first: withGRPC, want: "quic"},
		{name: "explicit http2 case-insensitive", configured: "HTTP2", first: withoutGRPC, want: "http2"},
		// auto/unset upgrades to http2 only on gRPC.
		{name: "auto with gRPC upgrades to http2", configured: "auto", first: withGRPC, want: "http2"},
		{name: "auto without gRPC stays auto", configured: "auto", first: withoutGRPC, want: "auto"},
		{name: "empty with gRPC upgrades to http2", configured: "", first: withGRPC, want: "http2"},
		{name: "empty without gRPC stays auto", configured: "", first: withoutGRPC, want: "auto"},
		{name: "padded auto with gRPC upgrades", configured: " auto ", first: withGRPC, want: "http2"},
		{name: "auto with no config stays auto", configured: "auto", first: nil, want: "auto"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, proxy.StartupProtocol(tt.configured, tt.first, slog.Default()))
		})
	}
}

func TestAwaitFirstConfig_ReturnsTheFirstConfig(t *testing.T) {
	t.Parallel()

	first := &proxy.Config{Version: 7}

	ch := make(chan *proxy.Config, 1)
	ch <- first

	got, err := proxy.AwaitFirstConfig(context.Background(), ch, time.Hour, nil)
	require.NoError(t, err)
	assert.Same(t, first, got)
}

func TestAwaitFirstConfig_TimesOut(t *testing.T) {
	t.Parallel()

	got, err := proxy.AwaitFirstConfig(context.Background(), make(chan *proxy.Config), 10*time.Millisecond, nil)
	require.ErrorIs(t, err, proxy.ErrNoFirstConfig)
	assert.Nil(t, got)
}

// TestAwaitFirstConfig_CtxCancelled proves a forced shutdown during the wait
// does not hang the proxy.
func TestAwaitFirstConfig_CtxCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := proxy.AwaitFirstConfig(ctx, make(chan *proxy.Config), time.Hour, nil)
	require.ErrorIs(t, err, context.Canceled)
}

// TestAwaitFirstConfig_DrainSignalCutsTheWait pins the two-stage shutdown
// contract during the wait: SIGTERM closes the DRAIN channel (the context
// deliberately stays alive so in-flight work finishes), and a pod terminated
// before its first config push must not burn the wait out of its termination
// grace.
func TestAwaitFirstConfig_DrainSignalCutsTheWait(t *testing.T) {
	t.Parallel()

	drain := make(chan struct{})
	close(drain)

	start := time.Now()
	_, err := proxy.AwaitFirstConfig(context.Background(), make(chan *proxy.Config), time.Hour, drain)

	require.ErrorIs(t, err, proxy.ErrDrainBeforeFirstConfig)
	assert.Less(t, time.Since(start), 10*time.Second,
		"a closed drain channel must cut the wait immediately")
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

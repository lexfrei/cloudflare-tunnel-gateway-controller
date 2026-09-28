package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
)

const firstConfigTestToken = "first-config-token"

// quietWindow is how long a test watches for a dial that must not happen.
const quietWindow = 300 * time.Millisecond

// registeringStarter stands in for tunnel.StartTunnelWithRetry, the only code
// in the proxy that opens edge connections. Like the real one once the edge
// accepts the connector, it reports the registration through OnConnected, then
// serves until the context ends.
type registeringStarter struct {
	started chan *tunnel.Config
}

func newRegisteringStarter() *registeringStarter {
	return &registeringStarter{started: make(chan *tunnel.Config, 1)}
}

func (s *registeringStarter) start(ctx context.Context, cfg *tunnel.Config, _ <-chan struct{}) error {
	cfg.OnConnected()
	s.started <- cfg

	<-ctx.Done()

	// Every test here ends the dial by cancelling its context.
	return context.Canceled
}

// pushFirstConfig delivers a config the way the controller does: a PUT to the
// config API, which is serving while the dial waits.
func pushFirstConfig(t *testing.T, api http.Handler, cfg *proxy.Config) {
	t.Helper()

	body, err := json.Marshal(cfg)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+firstConfigTestToken)

	recorder := httptest.NewRecorder()
	api.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
}

type dialOutcome struct {
	err error
}

func runDialTunnel(
	ctx context.Context,
	router *proxy.Router,
	graceC <-chan struct{},
	wait time.Duration,
	starter *registeringStarter,
) <-chan dialOutcome {
	done := make(chan dialOutcome, 1)

	go func() {
		err := dialTunnel(ctx, slog.New(slog.DiscardHandler), router, graceC, wait,
			&tunnel.Config{OnConnected: router.SetTunnelConnected}, starter.start)
		done <- dialOutcome{err: err}
	}()

	return done
}

// TestDialTunnel_RegistersOnlyAfterTheFirstConfig pins that a tunnel-mode
// proxy opens no edge connection until the controller has pushed a config,
// whatever transport it is set to. A connector the edge knows about gets
// traffic, and without a config it answers every request with a 404 and every
// gRPC call with Unimplemented.
func TestDialTunnel_RegistersOnlyAfterTheFirstConfig(t *testing.T) {
	for _, protocol := range []string{"http2", "quic", "auto", ""} {
		t.Run("protocol="+protocol, func(t *testing.T) {
			t.Setenv("PROXY_TUNNEL_PROTOCOL", protocol)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			router := proxy.NewRouter()
			api := proxy.NewConfigAPI(router, firstConfigTestToken)
			starter := newRegisteringStarter()

			done := runDialTunnel(ctx, router, make(chan struct{}), time.Hour, starter)

			select {
			case <-starter.started:
				t.Fatal("the tunnel was dialed before the first config arrived")
			case outcome := <-done:
				t.Fatalf("the dial ended before the first config arrived: %v", outcome.err)
			case <-time.After(quietWindow):
			}

			assert.False(t, router.TunnelConnected(), "no edge registration before the first config")

			pushFirstConfig(t, api, &proxy.Config{Version: 1})

			select {
			case <-starter.started:
			case <-time.After(10 * time.Second):
				t.Fatal("the tunnel was not dialed after the first config arrived")
			}

			assert.True(t, router.TunnelConnected(), "the edge registration follows the first config")
			assert.True(t, router.IsReady())

			cancel()

			outcome := <-done
			assert.ErrorIs(t, outcome.err, context.Canceled)
		})
	}
}

// TestDialTunnel_NoFirstConfigFailsWithoutDialing pins the bound on the wait:
// a proxy the controller never configures exits with an error rather than
// registering with nothing to route by, or waiting forever.
func TestDialTunnel_NoFirstConfigFailsWithoutDialing(t *testing.T) {
	t.Setenv("PROXY_TUNNEL_PROTOCOL", "")

	router := proxy.NewRouter()
	starter := newRegisteringStarter()

	var outcome dialOutcome

	select {
	case outcome = <-runDialTunnel(t.Context(), router, make(chan struct{}), 50*time.Millisecond, starter):
	case <-starter.started:
		t.Fatal("the tunnel was dialed without a config")
	case <-time.After(10 * time.Second):
		t.Fatal("the wait for the first config is not bounded")
	}

	require.Error(t, outcome.err)
	assert.False(t, router.TunnelConnected())
}

// TestDialTunnel_DrainBeforeFirstConfigExitsCleanly pins that a pod terminated
// while it waits for its first config shuts down without dialing and without
// reporting a failure.
func TestDialTunnel_DrainBeforeFirstConfigExitsCleanly(t *testing.T) {
	t.Setenv("PROXY_TUNNEL_PROTOCOL", "http2")

	router := proxy.NewRouter()
	starter := newRegisteringStarter()

	graceC := make(chan struct{})
	close(graceC)

	var outcome dialOutcome

	select {
	case outcome = <-runDialTunnel(t.Context(), router, graceC, time.Hour, starter):
	case <-starter.started:
		t.Fatal("the tunnel was dialed after the drain started")
	case <-time.After(10 * time.Second):
		t.Fatal("a drain does not end the wait for the first config")
	}

	require.NoError(t, outcome.err)
}

// TestDialTunnel_FirstConfigWithGRPCRouteDialsHTTP2 pins that the transport is
// chosen from the first config: auto becomes http2 when it carries a GRPCRoute.
func TestDialTunnel_FirstConfigWithGRPCRouteDialsHTTP2(t *testing.T) {
	t.Setenv("PROXY_TUNNEL_PROTOCOL", "auto")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	router := proxy.NewRouter()
	api := proxy.NewConfigAPI(router, firstConfigTestToken)
	starter := newRegisteringStarter()

	done := runDialTunnel(ctx, router, make(chan struct{}), time.Hour, starter)

	pushFirstConfig(t, api, &proxy.Config{Version: 1, HasGRPCRoute: true})

	select {
	case cfg := <-starter.started:
		assert.Equal(t, "http2", cfg.Protocol)
	case <-time.After(10 * time.Second):
		t.Fatal("the tunnel was not dialed after the first config arrived")
	}

	cancel()
	<-done
}

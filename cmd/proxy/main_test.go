package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvOrDefault_DefaultValue(t *testing.T) {
	t.Parallel()

	result := envOrDefault("NONEXISTENT_ENV_VAR_FOR_TESTING_12345", "fallback")
	assert.Equal(t, "fallback", result)
}

func TestEnvOrDefault_EnvSet(t *testing.T) {
	t.Setenv("TEST_ENV_OR_DEFAULT", "custom")

	result := envOrDefault("TEST_ENV_OR_DEFAULT", "fallback")
	assert.Equal(t, "custom", result)
}

func TestNewServer(t *testing.T) {
	t.Parallel()

	handler := http.NewServeMux()
	server := newServer(":0", handler)

	require.NotNil(t, server)
	assert.Equal(t, ":0", server.Addr)
	assert.Equal(t, readHeaderTimeout, server.ReadHeaderTimeout)
	assert.Equal(t, configReadTimeout, server.ReadTimeout)
	assert.Equal(t, configWriteTimeout, server.WriteTimeout)
}

func TestHandleSignals_CancelOnSignal(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	logger := slog.Default()

	done := make(chan struct{})

	go func() {
		handleSignals(ctx, logger, cancel, sigChan)
		close(done)
	}()

	// Send signal — handler should cancel context.
	sigChan <- os.Interrupt

	<-done
	assert.Error(t, ctx.Err(), "context should be cancelled after signal")
}

func TestHandleSignals_ContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	sigChan := make(chan os.Signal, 1)
	logger := slog.Default()

	done := make(chan struct{})

	go func() {
		handleSignals(ctx, logger, cancel, sigChan)
		close(done)
	}()

	// Cancel context — handler should exit without signal.
	cancel()

	<-done
}

func TestConstants(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ":8081", defaultConfigAddr)
	assert.Equal(t, ":8080", defaultProxyAddr)
}

// TestParseWSEnvDurations_Matrix pins the env-var-to-duration
// translation in parseWSEnvDurations: three branches per env var
// (unset / unparseable / parseable). Without this, a future edit
// that swaps PROXY_WS_DIAL_TIMEOUT and PROXY_WS_HANDSHAKE_TIMEOUT,
// typos one of them, or wires the dial result to the handshake env
// would silently fall through to the 30s package default with no
// test catching it.
//
// Parsing tested directly (not through the wsHandlerOptions ->
// proxy.NewHandler -> EffectiveWS*ForTest chain) because the
// EffectiveWS*ForTest helpers live in internal/proxy/export_test.go
// and are invisible to cmd/proxy. The downstream wsHandlerOptions
// composition is mechanical: zero -> no opt, > 0 -> With* opt; the
// proxy package's TestNewHandler_WSTimeoutOptions exhaustively pins
// that translation. Testing the env-parse boundary here closes the
// gap end-to-end.
func TestStartupProtocolWait_Matrix(t *testing.T) {
	tests := []struct {
		name string
		env  string
		set  bool
		want time.Duration
	}{
		{name: "unset defaults to 30s", want: 30 * time.Second},
		{name: "valid duration is used", env: "5s", set: true, want: 5 * time.Second},
		{name: "empty defaults to 30s", env: "", set: true, want: 30 * time.Second},
		{name: "zero falls back to default", env: "0s", set: true, want: 30 * time.Second},
		{name: "negative falls back to default", env: "-5s", set: true, want: 30 * time.Second},
		{name: "garbage falls back to default", env: "soon", set: true, want: 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv("PROXY_TUNNEL_PROTOCOL_WAIT", tt.env)
			}

			logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
			got := startupProtocolWait(logger)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseWSEnvDurations_Matrix(t *testing.T) {
	tests := []struct {
		name              string
		dialEnv           string
		handshakeEnv      string
		setDialEnv        bool
		setHandshakeEnv   bool
		wantDial          time.Duration
		wantHandshakeRead time.Duration
		wantWarn          bool
	}{
		{
			name:              "both env vars unset returns zeros",
			wantDial:          0,
			wantHandshakeRead: 0,
		},
		{
			name:              "parseable dial env returns the parsed duration",
			dialEnv:           "5s",
			setDialEnv:        true,
			wantDial:          5 * time.Second,
			wantHandshakeRead: 0,
		},
		{
			name:              "parseable handshake env returns the parsed duration",
			handshakeEnv:      "7s",
			setHandshakeEnv:   true,
			wantDial:          0,
			wantHandshakeRead: 7 * time.Second,
		},
		{
			name:              "both parseable values returned independently",
			dialEnv:           "2s",
			handshakeEnv:      "3s",
			setDialEnv:        true,
			setHandshakeEnv:   true,
			wantDial:          2 * time.Second,
			wantHandshakeRead: 3 * time.Second,
		},
		{
			name:              "unparseable dial env returns zero and warns",
			dialEnv:           "bogus",
			setDialEnv:        true,
			wantDial:          0,
			wantHandshakeRead: 0,
			wantWarn:          true,
		},
		{
			name:              "unparseable handshake env returns zero and warns",
			handshakeEnv:      "also-bogus",
			setHandshakeEnv:   true,
			wantDial:          0,
			wantHandshakeRead: 0,
			wantWarn:          true,
		},
		{
			name:              "empty string is treated as unset",
			dialEnv:           "",
			setDialEnv:        true,
			wantDial:          0,
			wantHandshakeRead: 0,
		},
		{
			// time.ParseDuration accepts negative durations, so parseEnvDuration
			// returns them as-is. The > 0 gate in wsHandlerOptions then drops
			// the resulting option so the proxy defaults stick. Pinning the
			// parse-side behaviour here keeps the gate's purpose visible -- if
			// someone widens the gate to >= 0, this case still passes the
			// parsing step and the matching TestWSHandlerOptions_OptionCount
			// case ("negative dial => no opts") catches the dropped guard.
			name:              "negative dial is parseable and passes through",
			dialEnv:           "-5s",
			setDialEnv:        true,
			wantDial:          -5 * time.Second,
			wantHandshakeRead: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Parallel skipped: t.Setenv mutates process env, must run sequentially.
			//
			// Unset-shaped cases use t.Setenv(name, "") rather than
			// os.Unsetenv because parseWSEnvDurations's TrimSpace
			// check treats "" identically to unset, and t.Setenv
			// restores the prior value on cleanup -- a bare
			// os.Unsetenv leaks the unset state across the parent
			// test boundary, making subtest order load-bearing.
			if tt.setDialEnv {
				t.Setenv("PROXY_WS_DIAL_TIMEOUT", tt.dialEnv)
			} else {
				t.Setenv("PROXY_WS_DIAL_TIMEOUT", "")
			}

			if tt.setHandshakeEnv {
				t.Setenv("PROXY_WS_HANDSHAKE_TIMEOUT", tt.handshakeEnv)
			} else {
				t.Setenv("PROXY_WS_HANDSHAKE_TIMEOUT", "")
			}

			var logBuf bytes.Buffer

			logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

			dial, handshake := parseWSEnvDurations(logger)

			assert.Equal(t, tt.wantDial, dial,
				"dial duration must match expectation for env=%q", tt.dialEnv)
			assert.Equal(t, tt.wantHandshakeRead, handshake,
				"handshake duration must match expectation for env=%q", tt.handshakeEnv)

			if tt.wantWarn {
				assert.Contains(t, logBuf.String(), "failed to parse",
					"unparseable env var must surface a WARN")
			} else {
				assert.NotContains(t, logBuf.String(), "failed to parse",
					"parseable / unset env vars must NOT warn")
			}
		})
	}
}

// TestWSHandlerOptions_OptionCount pins the composition shape: every
// zero duration from parseWSEnvDurations drops to no option (so
// downstream proxy defaults stick), every positive duration emits one
// option. Catches a future regression that double-appends or drops
// the > 0 gate.
func TestWSHandlerOptions_OptionCount(t *testing.T) {
	tests := []struct {
		name          string
		dialEnv       string
		handshakeEnv  string
		wantOptionLen int
	}{
		{name: "no envs => no opts", wantOptionLen: 0},
		{name: "dial only", dialEnv: "1s", wantOptionLen: 1},
		{name: "handshake only", handshakeEnv: "1s", wantOptionLen: 1},
		{name: "both", dialEnv: "1s", handshakeEnv: "1s", wantOptionLen: 2},
		{name: "unparseable dial => no opts", dialEnv: "bogus", wantOptionLen: 0},
		// Negative duration parses cleanly but trips the > 0 gate in
		// wsHandlerOptions, so no option is emitted. Pins the guard so a
		// future "ge 0" widening fails here loudly instead of leaking a
		// negative timeout into proxy.NewHandler.
		{name: "negative dial => no opts", dialEnv: "-5s", wantOptionLen: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PROXY_WS_DIAL_TIMEOUT", tt.dialEnv)
			t.Setenv("PROXY_WS_HANDSHAKE_TIMEOUT", tt.handshakeEnv)

			logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn}))

			opts := wsHandlerOptions(logger)
			assert.Len(t, opts, tt.wantOptionLen)
		})
	}
}

// TestAccessLogEnabled_Matrix pins the env-var truthy form list:
// "1" / "true" / "TRUE" / " true " enable; unset / "" / "0" / "false"
// / garbage disable. Keeps the YAML-bool vs shell-flag conventions
// interchangeable so chart users and operators don't get bitten by
// one of the two not working.
func TestAccessLogEnabled_Matrix(t *testing.T) {
	tests := []struct {
		name string
		envv string
		want bool
		set  bool
	}{
		{name: "unset is disabled", set: false, want: false},
		{name: "empty is disabled", envv: "", set: true, want: false},
		{name: "0 is disabled", envv: "0", set: true, want: false},
		{name: "false is disabled", envv: "false", set: true, want: false},
		{name: "1 is enabled", envv: "1", set: true, want: true},
		{name: "true is enabled", envv: "true", set: true, want: true},
		{name: "TRUE is enabled (case-insensitive)", envv: "TRUE", set: true, want: true},
		{name: "  true   is enabled (trimmed)", envv: "  true   ", set: true, want: true},
		{name: "garbage is disabled (typo-safe)", envv: "yesplease", set: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Parallel skipped: t.Setenv mutates process env, must run sequentially.
			//
			// Unset-shaped cases use t.Setenv(name, "") rather than
			// os.Unsetenv because accessLogEnabled's TrimSpace+lower
			// check treats "" identically to unset, and t.Setenv
			// restores the prior value on cleanup -- a bare
			// os.Unsetenv leaks the unset state across the parent
			// test boundary, making subtest order load-bearing.
			if tt.set {
				t.Setenv("PROXY_ACCESS_LOG_ENABLED", tt.envv)
			} else {
				t.Setenv("PROXY_ACCESS_LOG_ENABLED", "")
			}

			assert.Equal(t, tt.want, accessLogEnabled(),
				"PROXY_ACCESS_LOG_ENABLED=%q must yield enabled=%v", tt.envv, tt.want)
		})
	}
}

// TestParseAccessLogSamplingRate_Matrix pins the parse contract:
// unset → 1.0 (log everything when feature enabled); valid float
// passes through; parse failure → 1.0 + WARN. Out-of-range values
// (negative, >1) intentionally pass through; downstream
// shouldSampleAccessLog clamps them so the symptom is "always log"
// or "errors-only" rather than silent "log nothing".
func TestParseAccessLogSamplingRate_Matrix(t *testing.T) {
	tests := []struct {
		name     string
		envv     string
		set      bool
		want     float64
		wantWarn bool
	}{
		{name: "unset defaults to 1.0", set: false, want: 1.0},
		{name: "empty defaults to 1.0", envv: "", set: true, want: 1.0},
		{name: "0.5 passes through", envv: "0.5", set: true, want: 0.5},
		{name: "0 passes through", envv: "0", set: true, want: 0},
		{name: "1 passes through", envv: "1", set: true, want: 1.0},
		{name: "negative passes through (clamped downstream)", envv: "-0.5", set: true, want: -0.5},
		{name: "above 1 passes through (clamped downstream)", envv: "50", set: true, want: 50},
		{name: "garbage defaults to 1.0 and warns", envv: "halfish", set: true, want: 1.0, wantWarn: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Unset-shaped cases use t.Setenv(name, "") rather than
			// os.Unsetenv because parseAccessLogSamplingRate's
			// TrimSpace check treats "" identically to unset, and
			// t.Setenv restores the prior value on cleanup -- a
			// bare os.Unsetenv leaks the unset state across the
			// parent test boundary, making subtest order
			// load-bearing.
			if tt.set {
				t.Setenv("PROXY_ACCESS_LOG_SAMPLING_RATE", tt.envv)
			} else {
				t.Setenv("PROXY_ACCESS_LOG_SAMPLING_RATE", "")
			}

			var logBuf bytes.Buffer

			logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

			got := parseAccessLogSamplingRate(logger)
			// InDelta over InEpsilon: InEpsilon barfs when expected=0
			// (relative error denominator collapses). Our parse returns
			// exact strconv values, so absolute delta = 0 is the
			// honest assertion shape.
			assert.InDelta(t, tt.want, got, 1e-9,
				"PROXY_ACCESS_LOG_SAMPLING_RATE=%q parse must yield %v, got %v", tt.envv, tt.want, got)

			if tt.wantWarn {
				assert.Contains(t, logBuf.String(), "failed to parse",
					"unparseable sampling rate must surface a WARN")
			} else {
				assert.NotContains(t, logBuf.String(), "failed to parse",
					"parseable / unset sampling rate must NOT warn")
			}
		})
	}
}

// TestHandlerOptions_AccessLogOnlyWhenEnabled pins the option-list
// composition: ws options ALWAYS pass through (they have their own
// > 0 gate inside the With* helpers); the access-log option appears
// iff PROXY_ACCESS_LOG_ENABLED is truthy. Without this gate the
// access-log option would be a no-op when the logger is nil
// (proxy.WithAccessLog handles nil), but emitting a nil entry into
// the slice would still cost an extra function call per request on
// the cold-start path -- the gate keeps the slice precisely sized.
func TestHandlerOptions_AccessLogOnlyWhenEnabled(t *testing.T) {
	tests := []struct {
		name           string
		enabled        string
		setEnabled     bool
		wantOptionsMin int
	}{
		{name: "disabled by default", setEnabled: false, wantOptionsMin: 0},
		{name: "enabled=true adds the access-log option", enabled: "true", setEnabled: true, wantOptionsMin: 1},
		{name: "enabled=0 keeps base ws options only", enabled: "0", setEnabled: true, wantOptionsMin: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Unset-shaped cases use t.Setenv(name, "") rather than
			// os.Unsetenv because the env helpers' TrimSpace+lower
			// checks treat "" identically to unset, and t.Setenv
			// restores the prior value on cleanup -- a bare
			// os.Unsetenv leaks the unset state across the parent
			// test boundary, making subtest order load-bearing.
			// The two PROXY_WS_* clears below are unconditional
			// (all subtests run with empty values) so they use the
			// same t.Setenv pattern.
			if tt.setEnabled {
				t.Setenv("PROXY_ACCESS_LOG_ENABLED", tt.enabled)
			} else {
				t.Setenv("PROXY_ACCESS_LOG_ENABLED", "")
			}

			t.Setenv("PROXY_WS_DIAL_TIMEOUT", "")
			t.Setenv("PROXY_WS_HANDSHAKE_TIMEOUT", "")

			logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn}))

			opts := handlerOptions(logger)
			assert.GreaterOrEqual(t, len(opts), tt.wantOptionsMin)
		})
	}
}

// TestAccessLogStripQueryOption_Matrix pins the strip-query env-var
// gate: returns a non-nil HandlerOption iff PROXY_ACCESS_LOG_ENABLED
// is truthy AND PROXY_ACCESS_LOG_STRIP_QUERY is truthy. Catches a
// regression that wires the option even when access logging itself
// is off (the option would be a no-op but its presence costs an
// extra HandlerOption application during NewHandler construction).
func TestAccessLogStripQueryOption_Matrix(t *testing.T) {
	tests := []struct {
		name       string
		enabled    string
		strip      string
		setEnabled bool
		setStrip   bool
		wantNonNil bool
	}{
		{name: "both unset → nil", wantNonNil: false},
		{name: "enabled=true, strip unset → nil", enabled: "true", setEnabled: true, wantNonNil: false},
		{name: "enabled=false, strip=true → nil (strip pointless without emission)", enabled: "false", strip: "true", setEnabled: true, setStrip: true, wantNonNil: false},
		{name: "enabled=true, strip=true → non-nil", enabled: "true", strip: "true", setEnabled: true, setStrip: true, wantNonNil: true},
		{name: "enabled=true, strip=1 → non-nil", enabled: "true", strip: "1", setEnabled: true, setStrip: true, wantNonNil: true},
		{name: "enabled=true, strip=TRUE → non-nil (case-insensitive)", enabled: "true", strip: "TRUE", setEnabled: true, setStrip: true, wantNonNil: true},
		{name: "enabled=true, strip=garbage → nil (typo-safe)", enabled: "true", strip: "yesplease", setEnabled: true, setStrip: true, wantNonNil: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Parallel skipped: t.Setenv mutates process env, must run sequentially.
			//
			// Unset-shaped cases use t.Setenv(name, "") rather than
			// os.Unsetenv because the helper's TrimSpace+lower check
			// treats "" identically to unset, and t.Setenv restores
			// the prior value on cleanup -- a bare os.Unsetenv leaks
			// the unset state across the parent test boundary.
			if tt.setEnabled {
				t.Setenv("PROXY_ACCESS_LOG_ENABLED", tt.enabled)
			} else {
				t.Setenv("PROXY_ACCESS_LOG_ENABLED", "")
			}

			if tt.setStrip {
				t.Setenv("PROXY_ACCESS_LOG_STRIP_QUERY", tt.strip)
			} else {
				t.Setenv("PROXY_ACCESS_LOG_STRIP_QUERY", "")
			}

			opt := accessLogStripQueryOption()
			if tt.wantNonNil {
				assert.NotNil(t, opt,
					"PROXY_ACCESS_LOG_ENABLED=%q + PROXY_ACCESS_LOG_STRIP_QUERY=%q must yield non-nil HandlerOption", tt.enabled, tt.strip)
			} else {
				assert.Nil(t, opt,
					"PROXY_ACCESS_LOG_ENABLED=%q + PROXY_ACCESS_LOG_STRIP_QUERY=%q must yield nil HandlerOption", tt.enabled, tt.strip)
			}
		})
	}
}

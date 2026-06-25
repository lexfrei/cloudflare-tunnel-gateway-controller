package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSelfProbeProtocol_Triggered pins the gate that distinguishes a
// config-convergence probe from ordinary traffic: both the trigger header and
// the echo-header sentinel must be present. Anything else is real traffic and
// must never be short-circuited.
func TestSelfProbeProtocol_Triggered(t *testing.T) {
	t.Parallel()

	proto := defaultSelfProbeProtocols()[0]

	tests := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{
			name:    "trigger + sentinel present",
			headers: map[string]string{"K-Network-Probe": "probe", "K-Network-Hash": "override"},
			want:    true,
		},
		{
			name:    "echo header already concrete (hop-2 of the old loop) is NOT a probe",
			headers: map[string]string{"K-Network-Probe": "probe", "K-Network-Hash": "ep-abc123"},
			want:    false,
		},
		{
			name:    "missing trigger header",
			headers: map[string]string{"K-Network-Hash": "override"},
			want:    false,
		},
		{
			name:    "ordinary traffic",
			headers: map[string]string{},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/", nil)
			for name, value := range tt.headers {
				req.Header.Set(name, value)
			}

			assert.Equal(t, tt.want, proto.triggered(req))
		})
	}
}

// TestSelfProbeProtocol_EchoValue extracts the concrete value the matched rule's
// RequestHeaderModifier would set on the echo header. The handler echoes this
// LITERAL value (including any ep-/tr- rollout prefix) so the prober's
// version==value check converges.
func TestSelfProbeProtocol_EchoValue(t *testing.T) {
	t.Parallel()

	proto := defaultSelfProbeProtocols()[0]

	setHash := func(name, value string) RouteFilter {
		return RouteFilter{
			Type:                  FilterRequestHeaderModifier,
			RequestHeaderModifier: &HeaderModifier{Set: []HeaderValue{{Name: name, Value: value}}},
		}
	}

	tests := []struct {
		name      string
		filters   []RouteFilter
		wantValue string
		wantFound bool
	}{
		{
			name:      "concrete ep- prefixed value",
			filters:   []RouteFilter{setHash("K-Network-Hash", "ep-4d38b8fc")},
			wantValue: "ep-4d38b8fc",
			wantFound: true,
		},
		{
			name:      "lowercase header name still matches via canonicalization",
			filters:   []RouteFilter{setHash("k-network-hash", "tr-deadbeef")},
			wantValue: "tr-deadbeef",
			wantFound: true,
		},
		{
			name: "last Set entry wins, mirroring header Set semantics",
			filters: []RouteFilter{{
				Type: FilterRequestHeaderModifier,
				RequestHeaderModifier: &HeaderModifier{Set: []HeaderValue{
					{Name: "K-Network-Hash", Value: "first"},
					{Name: "K-Network-Hash", Value: "second"},
				}},
			}},
			wantValue: "second",
			wantFound: true,
		},
		{
			name:      "no echo-setting filter",
			filters:   []RouteFilter{setHash("X-Other", "value")},
			wantValue: "",
			wantFound: false,
		},
		{
			name:      "empty value is treated as not found",
			filters:   []RouteFilter{setHash("K-Network-Hash", "")},
			wantValue: "",
			wantFound: false,
		},
		{
			name:      "no filters",
			filters:   nil,
			wantValue: "",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			value, found := proto.echoValue(tt.filters)
			assert.Equal(t, tt.wantFound, found)
			assert.Equal(t, tt.wantValue, value)
		})
	}
}

// TestAnswerSelfProbe covers the gateway-authoritative responder directly: it
// writes a body-less 200 + echo header when a registered protocol triggers and
// the matched rule sets the echo value, and otherwise leaves the response
// untouched (so the caller forwards).
func TestAnswerSelfProbe(t *testing.T) {
	t.Parallel()

	probeRule := func(echoValue string) *RouteRule {
		return &RouteRule{
			Filters: []RouteFilter{{
				Type:                  FilterRequestHeaderModifier,
				RequestHeaderModifier: &HeaderModifier{Set: []HeaderValue{{Name: "K-Network-Hash", Value: echoValue}}},
			}},
		}
	}

	probeReq := func() *http.Request {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/", nil)
		req.Header.Set("K-Network-Probe", "probe")
		req.Header.Set("K-Network-Hash", "override")

		return req
	}

	t.Run("answers triggered probe with the rule's concrete value", func(t *testing.T) {
		t.Parallel()

		recorder := httptest.NewRecorder()
		answered := answerSelfProbe(recorder, probeReq(), probeRule("ep-4d38b8fc"))

		require.True(t, answered)
		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.Equal(t, "ep-4d38b8fc", recorder.Header().Get("K-Network-Hash"))
		assert.Equal(t, 0, recorder.Body.Len(), "the probe ack must be body-less")
	})

	t.Run("does not answer when the rule has no echo-setting filter", func(t *testing.T) {
		t.Parallel()

		recorder := httptest.NewRecorder()
		answered := answerSelfProbe(recorder, probeReq(), &RouteRule{})

		assert.False(t, answered)
		assert.Empty(t, recorder.Header().Get("K-Network-Hash"))
	})

	t.Run("does not answer ordinary (non-probe) traffic", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/", nil)
		recorder := httptest.NewRecorder()
		answered := answerSelfProbe(recorder, req, probeRule("ep-abc"))

		assert.False(t, answered)
	})

	t.Run("nil rule is never answered", func(t *testing.T) {
		t.Parallel()

		recorder := httptest.NewRecorder()
		assert.False(t, answerSelfProbe(recorder, probeReq(), nil))
	})
}

// TestAnswerSelfProbe_GenericProtocol proves the responder is not Knative-bound:
// a custom SelfProbeProtocol with different header names is answered the same
// way, via the explicit-list seam (no global state).
func TestAnswerSelfProbe_GenericProtocol(t *testing.T) {
	t.Parallel()

	protocols := []SelfProbeProtocol{{
		Name:          "custom",
		TriggerHeader: "X-My-Probe",
		TriggerValue:  "yes",
		EchoHeader:    "X-My-Version",
		Sentinel:      "ask",
	}}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/", nil)
	req.Header.Set("X-My-Probe", "yes")
	req.Header.Set("X-My-Version", "ask")

	rule := &RouteRule{Filters: []RouteFilter{{
		Type:                  FilterRequestHeaderModifier,
		RequestHeaderModifier: &HeaderModifier{Set: []HeaderValue{{Name: "X-My-Version", Value: "v42"}}},
	}}}

	recorder := httptest.NewRecorder()
	require.True(t, answerSelfProbeWith(protocols, recorder, req, rule))
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "v42", recorder.Header().Get("X-My-Version"))
}

// TestSelfProbeProtocols_RegistersKnative locks that the net-gateway-api probe
// stays registered — dropping it would silently reintroduce the stuck-ingress
// regression.
func TestSelfProbeProtocols_RegistersKnative(t *testing.T) {
	t.Parallel()

	var found bool
	for _, proto := range defaultSelfProbeProtocols() {
		if proto.Name == "knative-net-gateway-api" {
			found = true
			assert.Equal(t, "K-Network-Probe", proto.TriggerHeader)
			assert.Equal(t, "K-Network-Hash", proto.EchoHeader)
			assert.Equal(t, "override", proto.Sentinel)
		}
	}
	assert.True(t, found, "the Knative net-gateway-api self-probe protocol must stay registered")
}

// TestRequestFromInClusterListener locks the security scope: only requests that
// passed through InClusterProbeMiddleware (the in-cluster listener) carry the
// flag. The cloudflared tunnel/edge path uses the bare handler, so an external
// client can never forge a probe to extract the config value or get a synthetic
// 200.
func TestRequestFromInClusterListener(t *testing.T) {
	t.Parallel()

	t.Run("bare request is not in-cluster", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/", nil)
		assert.False(t, requestFromInClusterListener(req.Context()))
	})

	t.Run("middleware stamps the flag", func(t *testing.T) {
		t.Parallel()

		var seen bool
		next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen = requestFromInClusterListener(r.Context())
		})

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/", nil)
		InClusterProbeMiddleware(next).ServeHTTP(httptest.NewRecorder(), req)

		assert.True(t, seen, "middleware must stamp the in-cluster flag on the request context")
	})
}

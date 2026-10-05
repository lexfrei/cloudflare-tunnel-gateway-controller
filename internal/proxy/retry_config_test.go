package proxy_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func TestRouteRetry_JSON_RoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		retry    proxy.RouteRetry
		wantJSON string
	}{
		{
			name:     "all fields",
			retry:    proxy.RouteRetry{Codes: []int{500, 503}, Attempts: 3, Backoff: 100 * time.Millisecond},
			wantJSON: `{"codes":[500,503],"attempts":3,"backoff":"100ms"}`,
		},
		{
			name:     "no codes, zero backoff",
			retry:    proxy.RouteRetry{Attempts: 1},
			wantJSON: `{"attempts":1}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(tt.retry)
			require.NoError(t, err)
			assert.JSONEq(t, tt.wantJSON, string(data))

			var decoded proxy.RouteRetry
			require.NoError(t, json.Unmarshal(data, &decoded))
			assert.Equal(t, tt.retry, decoded)
		})
	}
}

func TestRouteRetry_UnmarshalJSON_InvalidBackoff(t *testing.T) {
	t.Parallel()

	var retry proxy.RouteRetry

	err := json.Unmarshal([]byte(`{"attempts":1,"backoff":"soon"}`), &retry)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid retry backoff")
}

func routeWithRetry(retry *gatewayv1.HTTPRouteRetry) *gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches:     []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}}},
					BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("svc", 80, 1)},
					Retry:       retry,
				},
			},
		},
	}
}

func TestConvertHTTPRoutes_Retry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		retry *gatewayv1.HTTPRouteRetry
		want  *proxy.RouteRetry
	}{
		{
			name:  "no retry stanza",
			retry: nil,
			want:  nil,
		},
		{
			name: "all fields",
			retry: &gatewayv1.HTTPRouteRetry{
				Codes:    []gatewayv1.HTTPRouteRetryStatusCode{500, 502},
				Attempts: new(3),
				Backoff:  durationPtr("250ms"),
			},
			want: &proxy.RouteRetry{Codes: []int{500, 502}, Attempts: 3, Backoff: 250 * time.Millisecond},
		},
		{
			name:  "empty stanza takes the implementation defaults",
			retry: &gatewayv1.HTTPRouteRetry{},
			want:  &proxy.RouteRetry{Attempts: proxy.DefaultRetryAttempts, Backoff: proxy.DefaultRetryBackoff},
		},
		{
			name:  "explicit zero backoff is kept",
			retry: &gatewayv1.HTTPRouteRetry{Attempts: new(2), Backoff: durationPtr("0s")},
			want:  &proxy.RouteRetry{Attempts: 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{routeWithRetry(tt.retry)}, "cluster.local", nil, nil, nil, nil)

			require.Len(t, cfg.Rules, 1)
			assert.Equal(t, tt.want, cfg.Rules[0].Retry)
			assert.Empty(t, cfg.Diagnostics)
		})
	}
}

// TestConvertHTTPRoutes_InvalidRetry_PartiallyInvalidDiagnostic pins
// that an unparseable backoff drops the retry policy but keeps the rule
// serving, reported as PartiallyInvalid like an unparseable timeout.
//
// A negative attempts or backoff gets the same treatment: an object stored
// before the CRD gained its minimum can still carry one, and letting it
// through would make the proxy refuse the whole pushed config.
func TestConvertHTTPRoutes_InvalidRetry_PartiallyInvalidDiagnostic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		retry *gatewayv1.HTTPRouteRetry
	}{
		{name: "unparseable backoff", retry: &gatewayv1.HTTPRouteRetry{Attempts: new(2), Backoff: durationPtr("soon")}},
		{name: "negative attempts", retry: &gatewayv1.HTTPRouteRetry{Attempts: new(-1)}},
		{name: "negative backoff", retry: &gatewayv1.HTTPRouteRetry{Attempts: new(1), Backoff: durationPtr("-1s")}},
		{name: "code out of range", retry: &gatewayv1.HTTPRouteRetry{Codes: []gatewayv1.HTTPRouteRetryStatusCode{200}, Attempts: new(1)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{routeWithRetry(tt.retry)}, "cluster.local", nil, nil, nil, nil)

			require.Len(t, cfg.Rules, 1)
			assert.Nil(t, cfg.Rules[0].Retry)
			assert.Zero(t, cfg.Rules[0].UnavailableStatus)
			require.NoError(t, cfg.Validate(), "the converted config must stay pushable")

			require.Len(t, cfg.Diagnostics, 1)
			assert.Equal(t, proxy.DiagnosticAccepted, cfg.Diagnostics[0].Target)
			assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedValue), cfg.Diagnostics[0].Reason)
			assert.False(t, cfg.Diagnostics[0].WholeRule)
			assert.Contains(t, cfg.Diagnostics[0].Message, "retry")
		})
	}
}

func TestConfig_Validate_RejectsInvalidRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		retry   proxy.RouteRetry
		wantErr string
	}{
		{name: "negative attempts", retry: proxy.RouteRetry{Attempts: -1}, wantErr: "retry attempts must be non-negative"},
		{name: "negative backoff", retry: proxy.RouteRetry{Attempts: 1, Backoff: -time.Second}, wantErr: "retry backoff must be non-negative"},
		{name: "code below 400", retry: proxy.RouteRetry{Codes: []int{399}, Attempts: 1}, wantErr: "retry code must be within 400-599"},
		{name: "code above 599", retry: proxy.RouteRetry{Codes: []int{600}, Attempts: 1}, wantErr: "retry code must be within 400-599"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := proxy.Config{Version: 1, Rules: []proxy.RouteRule{{
				Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
				Retry:    &tt.retry,
			}}}

			err := cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestConfig_Validate_AcceptsRetryBoundaries(t *testing.T) {
	t.Parallel()

	cfg := proxy.Config{Version: 1, Rules: []proxy.RouteRule{{
		Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
		Retry:    &proxy.RouteRetry{Codes: []int{400, 599}},
	}}}

	require.NoError(t, cfg.Validate(), "codes 400 and 599, zero attempts and zero backoff are all valid")
}

// TestConvertHTTPRoutes_RetryAttemptsOverCap pins that a rule asking for more
// retries than the proxy allows is served with the cap and reported as a
// Warning Event, not PartiallyInvalid: attempts is a maximum, so retrying less
// keeps the route fully valid and the spec forbids PartiallyInvalid on it.
func TestConvertHTTPRoutes_RetryAttemptsOverCap(t *testing.T) {
	t.Parallel()

	route := routeWithRetry(&gatewayv1.HTTPRouteRetry{Codes: []gatewayv1.HTTPRouteRetryStatusCode{503}, Attempts: new(proxy.MaxRetryAttempts + 1)})

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.NotNil(t, cfg.Rules[0].Retry)
	assert.Equal(t, proxy.MaxRetryAttempts, cfg.Rules[0].Retry.Attempts)
	assert.Equal(t, []int{503}, cfg.Rules[0].Retry.Codes)

	require.Len(t, cfg.Diagnostics, 1)
	assert.Equal(t, proxy.DiagnosticEvent, cfg.Diagnostics[0].Target)
	assert.Equal(t, proxy.EventTypeWarning, cfg.Diagnostics[0].EventType)
	assert.Contains(t, cfg.Diagnostics[0].Message, "attempts")
}

func TestConvertHTTPRoutes_RetryAttemptsAtCapHasNoDiagnostic(t *testing.T) {
	t.Parallel()

	route := routeWithRetry(&gatewayv1.HTTPRouteRetry{Attempts: new(proxy.MaxRetryAttempts)})

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{route}, "cluster.local", nil, nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Equal(t, proxy.MaxRetryAttempts, cfg.Rules[0].Retry.Attempts)
	assert.Empty(t, cfg.Diagnostics)
}

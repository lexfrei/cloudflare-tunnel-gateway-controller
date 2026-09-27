package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// unknownFilterType stands in for a filter a newer controller emits and this
// proxy image does not know, which is how an unknown type reaches the proxy.
const unknownFilterType proxy.RouteFilterType = "FilterFromANewerController"

// TestRouter_UnknownFilterTypeFailsOnlyItsRule pins that a filter type the
// proxy cannot compile costs the rule carrying it, not the pushed document.
//
// The rule is not skipped either. Skipping would let its requests fall through
// to the next matching rule and reach a backend without the filter's effect;
// the Gateway API forbids skipping a filter it cannot honour, and requires the
// requests it would have processed to receive an HTTP error instead.
func TestRouter_UnknownFilterTypeFailsOnlyItsRule(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	backends := []proxy.BackendRef{{URL: backend.URL, Weight: 1}}
	prefix := func(value string) []proxy.RouteMatch {
		return []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: value}}}
	}

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   prefix("/admin"),
				Filters:   []proxy.RouteFilter{{Type: unknownFilterType}},
				Backends:  backends,
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches:   prefix("/per-backend"),
				Backends: []proxy.BackendRef{{
					URL: backend.URL, Weight: 1,
					Filters: []proxy.RouteFilter{{Type: unknownFilterType}},
				}},
			},
			{
				// The failing backend carries no weight, so a request here
				// reaches the healthy one only if the failure stayed with the
				// backend rather than closing the whole rule.
				Hostnames: []string{"app.example.com"},
				Matches:   prefix("/mixed"),
				Backends: []proxy.BackendRef{
					{URL: backend.URL, Weight: 1},
					{URL: backend.URL, Weight: 0, Filters: []proxy.RouteFilter{{Type: unknownFilterType}}},
				},
			},
			{Hostnames: []string{"app.example.com"}, Matches: prefix("/"), Backends: backends},
			{Hostnames: []string{"other.example.com"}, Matches: prefix("/"), Backends: backends},
		},
	}

	require.NoError(t, cfg.Validate(), "an unknown filter type must not refuse the whole document")

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(cfg))

	handler := proxy.NewHandler(router)

	tests := []struct {
		url      string
		wantCode int
		wantHit  bool
	}{
		{url: "http://app.example.com/admin", wantCode: http.StatusInternalServerError},
		{url: "http://app.example.com/per-backend", wantCode: http.StatusInternalServerError},
		{url: "http://app.example.com/mixed", wantCode: http.StatusOK, wantHit: true},
		{url: "http://app.example.com/", wantCode: http.StatusOK, wantHit: true},
		{url: "http://other.example.com/", wantCode: http.StatusOK, wantHit: true},
	}

	for _, tt := range tests {
		before := hits.Load()

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.url, nil))

		assert.Equal(t, tt.wantCode, recorder.Code, tt.url)
		assert.Equal(t, tt.wantHit, hits.Load() > before, "backend reached for %s", tt.url)
	}
}

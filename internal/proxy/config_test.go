package proxy_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func TestProxyConfig_JSON_RoundTrip(t *testing.T) {
	t.Parallel()

	original := &proxy.Config{
		Version: 42,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{
						Path: &proxy.PathMatch{
							Type:  proxy.PathMatchExact,
							Value: "/api/v1/users",
						},
						Headers: []proxy.HeaderMatch{
							{Type: proxy.HeaderMatchExact, Name: "X-Env", Value: "prod"},
						},
						QueryParams: []proxy.QueryParamMatch{
							{Type: proxy.QueryParamMatchExact, Name: "format", Value: "json"},
						},
						Method: http.MethodGet,
					},
				},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterRequestHeaderModifier,
						RequestHeaderModifier: &proxy.HeaderModifier{
							Set:    []proxy.HeaderValue{{Name: "X-Forwarded-Proto", Value: "https"}},
							Add:    []proxy.HeaderValue{{Name: "X-Custom", Value: "value"}},
							Remove: []string{"X-Internal"},
						},
					},
				},
				Backends: []proxy.BackendRef{
					{URL: "http://users.default.svc.cluster.local:8080", Weight: 80},
					{URL: "http://users-canary.default.svc.cluster.local:8080", Weight: 20},
				},
			},
		},
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)

	parsed, err := proxy.ParseConfig(data)
	require.NoError(t, err)

	assert.Equal(t, original.Version, parsed.Version)
	assert.Equal(t, len(original.Rules), len(parsed.Rules))

	rule := parsed.Rules[0]
	assert.Equal(t, []string{"app.example.com"}, rule.Hostnames)
	assert.Equal(t, proxy.PathMatchExact, rule.Matches[0].Path.Type)
	assert.Equal(t, "/api/v1/users", rule.Matches[0].Path.Value)
	assert.Equal(t, http.MethodGet, rule.Matches[0].Method)
	assert.Len(t, rule.Matches[0].Headers, 1)
	assert.Len(t, rule.Matches[0].QueryParams, 1)
	assert.Len(t, rule.Backends, 2)
}

func TestProxyConfig_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  proxy.Config
		wantErr string
	}{
		{
			name: "valid minimal config",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}}},
				},
			},
		},
		{
			name: "valid empty rules",
			config: proxy.Config{
				Version: 0,
				Rules:   []proxy.RouteRule{},
			},
		},
		{
			name: "negative version",
			config: proxy.Config{
				Version: -1,
			},
			wantErr: "version must be non-negative",
		},
		{
			name: "rule without backends",
			config: proxy.Config{
				Version: 1,
				Rules:   []proxy.RouteRule{{Backends: nil}},
			},
			wantErr: "rule[0]: at least one backend is required",
		},
		{
			name: "redirect-only rule without backends is valid",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{
						Filters: []proxy.RouteFilter{
							{
								Type: proxy.FilterRequestRedirect,
								RequestRedirect: &proxy.RedirectConfig{
									Hostname:   new("other.example.com"),
									StatusCode: new(301),
								},
							},
						},
					},
				},
			},
		},
		{
			name: "backend without URL",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{Backends: []proxy.BackendRef{{URL: "", Weight: 1}}},
				},
			},
			wantErr: "rule[0]: backend[0]: url is required",
		},
		{
			name: "negative backend weight",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: -1}}},
				},
			},
			wantErr: "rule[0]: backend[0]: weight must be non-negative",
		},
		{
			name: "unknown path match type",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{
						Matches:  []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: "Unknown", Value: "/"}}},
						Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
					},
				},
			},
			wantErr: "unknown path match type",
		},
		{
			name: "empty path value",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{
						Matches:  []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchExact, Value: ""}}},
						Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
					},
				},
			},
			wantErr: "path: value is required",
		},
		{
			name: "unknown header match type",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{
						Matches: []proxy.RouteMatch{
							{Headers: []proxy.HeaderMatch{{Type: "Invalid", Name: "X", Value: "v"}}},
						},
						Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
					},
				},
			},
			wantErr: "unknown header match type",
		},
		{
			name: "header without name",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{
						Matches: []proxy.RouteMatch{
							{Headers: []proxy.HeaderMatch{{Type: proxy.HeaderMatchExact, Name: "", Value: "v"}}},
						},
						Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
					},
				},
			},
			wantErr: "name is required",
		},
		{
			name: "unknown query param type",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{
						Matches: []proxy.RouteMatch{
							{QueryParams: []proxy.QueryParamMatch{{Type: "Bad", Name: "q", Value: "v"}}},
						},
						Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
					},
				},
			},
			wantErr: "unknown query param match type",
		},
		{
			name: "query param without name",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{
						Matches: []proxy.RouteMatch{
							{QueryParams: []proxy.QueryParamMatch{
								{Type: proxy.QueryParamMatchExact, Name: "", Value: "v"},
							}},
						},
						Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
					},
				},
			},
			wantErr: "name is required",
		},
		{
			name: "unknown filter type",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{
						Filters:  []proxy.RouteFilter{{Type: "Unknown"}},
						Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
					},
				},
			},
			wantErr: "unknown filter type",
		},
		{
			name: "RequestHeaderModifier without config",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{
						Filters:  []proxy.RouteFilter{{Type: proxy.FilterRequestHeaderModifier}},
						Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
					},
				},
			},
			wantErr: "rule[0]: filter[0]: requestHeaderModifier config is required",
		},
		{
			name: "ResponseHeaderModifier without config",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{
						Filters:  []proxy.RouteFilter{{Type: proxy.FilterResponseHeaderModifier}},
						Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
					},
				},
			},
			wantErr: "rule[0]: filter[0]: responseHeaderModifier config is required",
		},
		{
			name: "RequestRedirect without config",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{
						Filters:  []proxy.RouteFilter{{Type: proxy.FilterRequestRedirect}},
						Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
					},
				},
			},
			wantErr: "rule[0]: filter[0]: requestRedirect config is required",
		},
		{
			name: "URLRewrite without config",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{
						Filters:  []proxy.RouteFilter{{Type: proxy.FilterURLRewrite}},
						Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
					},
				},
			},
			wantErr: "rule[0]: filter[0]: urlRewrite config is required",
		},
		{
			name: "RequestMirror without config",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{
						Filters:  []proxy.RouteFilter{{Type: proxy.FilterRequestMirror}},
						Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
					},
				},
			},
			wantErr: "rule[0]: filter[0]: requestMirror config is required",
		},
		{
			name: "all valid filter types",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{
						Filters: []proxy.RouteFilter{
							{Type: proxy.FilterRequestHeaderModifier, RequestHeaderModifier: &proxy.HeaderModifier{}},
							{Type: proxy.FilterResponseHeaderModifier, ResponseHeaderModifier: &proxy.HeaderModifier{}},
							{Type: proxy.FilterRequestRedirect, RequestRedirect: &proxy.RedirectConfig{}},
							{Type: proxy.FilterURLRewrite, URLRewrite: &proxy.URLRewriteConfig{}},
							{Type: proxy.FilterRequestMirror, RequestMirror: &proxy.MirrorConfig{BackendURL: "http://mirror:80"}},
						},
						Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
					},
				},
			},
		},
		{
			name: "all valid path match types",
			config: proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{
					{
						Matches: []proxy.RouteMatch{
							{Path: &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/exact"}},
							{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/prefix"}},
							{Path: &proxy.PathMatch{Type: proxy.PathMatchRegularExpression, Value: "/regex.*"}},
						},
						Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.config.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

func TestRouteTimeouts_JSON_RoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		timeouts proxy.RouteTimeouts
		wantJSON string
	}{
		{
			name:     "both timeouts",
			timeouts: proxy.RouteTimeouts{Request: 10 * time.Second, Backend: 5 * time.Second},
			wantJSON: `{"request":"10s","backend":"5s"}`,
		},
		{
			name:     "request only",
			timeouts: proxy.RouteTimeouts{Request: 500 * time.Millisecond},
			wantJSON: `{"request":"500ms"}`,
		},
		{
			name:     "zero values omitted",
			timeouts: proxy.RouteTimeouts{},
			wantJSON: `{}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(tt.timeouts)
			require.NoError(t, err)
			assert.JSONEq(t, tt.wantJSON, string(data))

			var decoded proxy.RouteTimeouts
			err = json.Unmarshal(data, &decoded)
			require.NoError(t, err)
			assert.Equal(t, tt.timeouts, decoded)
		})
	}
}

func TestRouteTimeouts_UnmarshalJSON_Invalid(t *testing.T) {
	t.Parallel()

	var rt proxy.RouteTimeouts

	err := json.Unmarshal([]byte(`{"request":"not-a-duration"}`), &rt)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid request timeout")
}

func TestParseConfig_InvalidJSON(t *testing.T) {
	t.Parallel()

	_, err := proxy.ParseConfig([]byte(`{invalid`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse config JSON")
}

func TestParseConfig_ValidationFailure(t *testing.T) {
	t.Parallel()

	_, err := proxy.ParseConfig([]byte(`{"version": -1}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config")
}

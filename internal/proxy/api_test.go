package proxy_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func TestConfigAPI_PutConfig(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	api := proxy.NewConfigAPI(router)

	cfg := proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				Backends:  []proxy.BackendRef{{URL: "http://backend:80", Weight: 1}},
			},
		},
	}

	body, err := json.Marshal(cfg)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	api.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, int64(1), router.ConfigVersion())
}

func TestConfigAPI_GetConfig(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	api := proxy.NewConfigAPI(router)

	// First load a config.
	cfg := proxy.Config{
		Version: 42,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				Backends:  []proxy.BackendRef{{URL: "http://backend:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(&cfg)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/config", nil)
	recorder := httptest.NewRecorder()

	api.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Header().Get("Content-Type"), "application/json")

	var response proxy.ConfigStatus
	err = json.NewDecoder(recorder.Body).Decode(&response)
	require.NoError(t, err)
	assert.Equal(t, int64(42), response.Version)
}

func TestConfigAPI_InvalidJSON(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	api := proxy.NewConfigAPI(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/config",
		bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	api.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestConfigAPI_InvalidConfig(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	api := proxy.NewConfigAPI(router)

	cfg := proxy.Config{
		Version: -1,
	}

	body, err := json.Marshal(cfg)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/config",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	api.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestConfigAPI_MethodNotAllowed(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	api := proxy.NewConfigAPI(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/config", nil)
	recorder := httptest.NewRecorder()

	api.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusMethodNotAllowed, recorder.Code)
}

func TestConfigAPI_HealthEndpoint(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	api := proxy.NewConfigAPI(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()

	api.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
}

func TestConfigAPI_ReadyEndpoint(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	api := proxy.NewConfigAPI(router)

	// Not ready before config is loaded.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil)
	recorder := httptest.NewRecorder()

	api.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)

	// Ready after config loaded.
	cfg := &proxy.Config{
		Version: 1,
		Rules:   []proxy.RouteRule{{Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}}}},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil)
	recorder = httptest.NewRecorder()

	api.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusOK, recorder.Code)
}

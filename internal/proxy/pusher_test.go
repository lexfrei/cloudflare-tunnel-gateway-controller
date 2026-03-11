package proxy_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func TestConfigPusher_PushToSingleEndpoint(t *testing.T) {
	t.Parallel()

	var receivedConfig proxy.Config

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPut {
			writer.WriteHeader(http.StatusMethodNotAllowed)

			return
		}

		err := json.NewDecoder(req.Body).Decode(&receivedConfig)
		if err != nil {
			writer.WriteHeader(http.StatusBadRequest)

			return
		}

		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	pusher := proxy.NewConfigPusher(http.DefaultClient, "")

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				Backends:  []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
			},
		},
	}

	endpoints := []string{server.URL + "/config"}

	results := pusher.Push(t.Context(), cfg, endpoints)

	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err)
	assert.Equal(t, int64(1), receivedConfig.Version)
}

func TestConfigPusher_PushToMultipleEndpoints(t *testing.T) {
	t.Parallel()

	var successCount atomic.Int32

	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		successCount.Add(1)
		writer.WriteHeader(http.StatusOK)
	})

	server1 := httptest.NewServer(handler)
	defer server1.Close()

	server2 := httptest.NewServer(handler)
	defer server2.Close()

	pusher := proxy.NewConfigPusher(http.DefaultClient, "")

	cfg := &proxy.Config{Version: 1}

	endpoints := []string{
		server1.URL + "/config",
		server2.URL + "/config",
	}

	results := pusher.Push(t.Context(), cfg, endpoints)

	require.Len(t, results, 2)
	assert.Equal(t, int32(2), successCount.Load())

	for _, result := range results {
		assert.NoError(t, result.Err)
	}
}

func TestConfigPusher_PartialFailure(t *testing.T) {
	t.Parallel()

	goodServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer goodServer.Close()

	badServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer badServer.Close()

	pusher := proxy.NewConfigPusher(http.DefaultClient, "")

	cfg := &proxy.Config{Version: 1}

	endpoints := []string{
		goodServer.URL + "/config",
		badServer.URL + "/config",
	}

	results := pusher.Push(t.Context(), cfg, endpoints)

	require.Len(t, results, 2)

	var successCount, failCount int

	for _, result := range results {
		if result.Err != nil {
			failCount++
		} else {
			successCount++
		}
	}

	assert.Equal(t, 1, successCount)
	assert.Equal(t, 1, failCount)
}

func TestConfigPusher_UnreachableEndpoint(t *testing.T) {
	t.Parallel()

	pusher := proxy.NewConfigPusher(http.DefaultClient, "")

	cfg := &proxy.Config{Version: 1}

	endpoints := []string{"http://127.0.0.1:1/config"}

	results := pusher.Push(t.Context(), cfg, endpoints)

	require.Len(t, results, 1)
	assert.Error(t, results[0].Err)
}

package proxy_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cockroachdb/errors"
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

// TestConfigPusher_TokenIsolation pins THE isolation property of the push
// path: PushWithToken sends exactly the token it is given (never the pusher's
// shared default), and an empty token sends NO Authorization header. A
// regression that fell back to the shared token would hand the shared plane's
// credential to tenant-controlled pods.
func TestConfigPusher_TokenIsolation(t *testing.T) {
	t.Parallel()

	var captured atomic.Value // string

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		captured.Store(req.Header.Get("Authorization"))
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	pusher := proxy.NewConfigPusher(http.DefaultClient, "shared-secret")
	cfg := &proxy.Config{Version: 1}
	endpoints := []string{server.URL + "/config"}

	pusher.PushWithToken(t.Context(), cfg, endpoints, "tenant-token")
	assert.Equal(t, "Bearer tenant-token", captured.Load(),
		"a per-partition token must be sent verbatim, never the shared default")

	pusher.PushWithToken(t.Context(), cfg, endpoints, "")
	assert.Empty(t, captured.Load(),
		"an empty token must send NO Authorization header — never fall back to the shared default")

	pusher.Push(t.Context(), cfg, endpoints)
	assert.Equal(t, "Bearer shared-secret", captured.Load(),
		"the shared-plane Push must use the pusher's default token")
}

// TestConfigPusher_StaleVersionRecoveryRejectsNon200 pins the m2 fix: during
// 409 stale-version recovery, a non-200 status (e.g. 401 token mismatch in the
// multi-token world) must surface as an unexpected-status error, not a JSON
// decode failure that masks the real cause.
func TestConfigPusher_StaleVersionRecoveryRejectsNon200(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte("unauthorized"))

			return
		}

		writer.WriteHeader(http.StatusConflict)
	}))
	defer server.Close()

	pusher := proxy.NewConfigPusher(http.DefaultClient, "")
	results := pusher.PushWithToken(t.Context(), &proxy.Config{Version: 1},
		[]string{server.URL + "/config"}, "wrong-token")

	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.NotContains(t, results[0].Err.Error(), "decode proxy status",
		"a 401 during recovery must not be masked as a JSON decode error")
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

// TestConfigPusher_StaleVersionRecovery pins the controller-restart clock-skew
// recovery. After a restart the version counter re-seeds from wall clock, so a
// backward NTP adjustment can leave the fresh controller issuing versions BELOW
// the ones a replica still holds from the previous instance. A replica version
// strictly ABOVE the current counter is the signature of that skew (only a
// prior instance could have issued it), so the pusher bumps the counter above
// the replica and re-pushes. The counter is pinned below the replica's version
// to make the skew deterministic; the test is therefore sequential (no
// t.Parallel — it mutates the process-global counter).
func TestConfigPusher_StaleVersionRecovery(t *testing.T) {
	const counterBase = int64(1_000_000)

	restore := proxy.SetConfigVersionCounterForTest(counterBase)
	defer proxy.SetConfigVersionCounterForTest(restore)

	// The replica sits above the counter → restart clock skew, not a lost race.
	const proxyVersion = counterBase + 1_000

	var pushCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodPut:
			count := pushCount.Add(1)

			var cfg proxy.Config

			decodeErr := json.NewDecoder(req.Body).Decode(&cfg)
			if decodeErr != nil {
				writer.WriteHeader(http.StatusBadRequest)

				return
			}

			if count == 1 {
				// First attempt: reject as stale.
				http.Error(writer, "stale config version", http.StatusConflict)

				return
			}

			// Second attempt: the retry must carry a version above the replica.
			if cfg.Version <= proxyVersion {
				http.Error(writer, "still stale", http.StatusConflict)

				return
			}

			writer.WriteHeader(http.StatusOK)

		case http.MethodGet:
			// Return proxy's current version for recovery.
			status := proxy.ConfigStatus{Version: proxyVersion, Ready: true}

			data, _ := json.Marshal(status)
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(data)
		}
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

	results := pusher.Push(t.Context(), cfg, []string{server.URL + "/config"})

	require.Len(t, results, 1)
	assert.NoError(t, results[0].Err, "restart clock skew should be recovered automatically")
	assert.Equal(t, int32(2), pushCount.Load(), "should have pushed twice (initial + retry)")
}

// TestConfigPusher_LostRaceAbandonsPush pins the #584 decision: a 409 whose
// replica version is at or below the current counter means a concurrent
// same-process pusher already delivered a NEWER config. The recovery must NOT
// re-push the older payload (that would force-overwrite the newer config); it
// abandons the push and reports ErrLostConfigPushRace, sending no retry. The
// process-global counter (seeded from wall clock, ~1.7e12) is far above the
// replica's version here, so this is the lost-race branch without pinning.
func TestConfigPusher_LostRaceAbandonsPush(t *testing.T) {
	t.Parallel()

	var putCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodPut:
			putCount.Add(1)
			// A concurrent sibling already advanced the replica; reject as stale.
			http.Error(writer, "stale config version", http.StatusConflict)

		case http.MethodGet:
			status := proxy.ConfigStatus{Version: 5000, Ready: true}
			data, _ := json.Marshal(status)
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(data)
		}
	}))
	defer server.Close()

	pusher := proxy.NewConfigPusher(http.DefaultClient, "")

	results := pusher.Push(t.Context(), &proxy.Config{Version: 1}, []string{server.URL + "/config"})

	require.Len(t, results, 1)
	assert.True(t, errors.Is(results[0].Err, proxy.ErrLostConfigPushRace),
		"a 409 at or below the counter is a lost race, not a recoverable restart skew")
	assert.Equal(t, int32(1), putCount.Load(),
		"a lost race must not re-push the older payload; exactly one PUT (the initial one) is sent")
}

// TestConfigPusher_ConcurrentStaleVersionRetry exercises concurrent restart-skew
// recoveries for their -race safety AND their convergence semantics. Two
// endpoints both report a replica version above the pinned counter (genuine
// clock skew). Concurrent recoveries share the process-global counter, so the
// FIRST to bump it recovers immediately; a sibling that reads the counter after
// that bump now sees the replica at or below it and abandons as a lost race —
// that endpoint self-heals on the next sync (the syncer invalidates the skip
// key on the push error). The test is sequential because it pins the counter.
func TestConfigPusher_ConcurrentStaleVersionRetry(t *testing.T) {
	const counterBase = int64(2_000_000)

	restore := proxy.SetConfigVersionCounterForTest(counterBase)
	defer proxy.SetConfigVersionCounterForTest(restore)

	const proxyVersion = counterBase + 5_000

	// Two endpoints both return 409 on first push, then accept any retry.
	// Running with -race detects a data race on Config or the shared counter.
	newStaleServer := func() *httptest.Server {
		var count atomic.Int32

		return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
			switch req.Method {
			case http.MethodPut:
				attempt := count.Add(1)
				if attempt == 1 {
					http.Error(writer, "stale", http.StatusConflict)

					return
				}

				writer.WriteHeader(http.StatusOK)

			case http.MethodGet:
				status := proxy.ConfigStatus{Version: proxyVersion, Ready: true}
				data, _ := json.Marshal(status)
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write(data)
			}
		}))
	}

	server1 := newStaleServer()
	defer server1.Close()

	server2 := newStaleServer()
	defer server2.Close()

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

	results := pusher.Push(t.Context(), cfg, []string{
		server1.URL + "/config",
		server2.URL + "/config",
	})

	require.Len(t, results, 2)

	recovered := 0

	for _, result := range results {
		if result.Err == nil {
			recovered++

			continue
		}

		assert.True(t, errors.Is(result.Err, proxy.ErrLostConfigPushRace),
			"a concurrent restart-skew retry either recovers or abandons as a lost race — never any other error")
	}

	assert.GreaterOrEqual(t, recovered, 1,
		"the first recovery to bump the counter must succeed; any sibling that lost the counter race self-heals next sync")
}

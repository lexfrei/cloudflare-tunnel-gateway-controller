package controller

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestRecordPush_LostRaceInvalidatesSkipKeyWithoutPoisoningCache pins how a
// lost-race outcome (#584) flows through recordPush: a push that returns
// ErrLostConfigPushRace is a normal push failure, so it must invalidate the
// partition's steady-state skip key (lastPushedHash → "") to force the next
// sync to re-deliver the current desired config, while leaving the cached
// lastCfg untouched — only a fully-successful push may update the replay cache.
func TestRecordPush_LostRaceInvalidatesSkipKeyWithoutPoisoningCache(t *testing.T) {
	t.Parallel()

	healthy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(healthy.Close)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	proxySyncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	ctx := context.Background()

	// A healthy sync seeds the shared partition's replay cache and skip key.
	route := pushFallbackRoute("web", "web.example.com")
	_, err := proxySyncer.SyncPartition(ctx, sharedPartitionKey, "",
		[]string{healthy.URL + "/config"}, []*gatewayv1.HTTPRoute{route}, nil, nil, nil)
	require.NoError(t, err)

	proxySyncer.syncMu.Lock()
	seeded := proxySyncer.targets[sharedPartitionKey]
	goodCfg := seeded.lastCfg
	goodHash := seeded.lastPushedHash
	proxySyncer.syncMu.Unlock()

	require.NotNil(t, goodCfg, "the healthy sync must have cached a config")
	require.NotEmpty(t, goodHash, "the healthy sync must have set the skip key")

	// A later push loses the race to a concurrent same-process pusher. recordPush
	// receives that error alongside a DIFFERENT config the replica never accepted.
	lostErr := errors.Wrap(proxy.ErrLostConfigPushRace, healthy.URL+"/config")
	neverApplied := &proxy.Config{Version: goodCfg.Version + 1000}
	proxySyncer.recordPush(sharedPartitionKey, "", "hash-never-applied", neverApplied,
		[]string{healthy.URL + "/config"}, lostErr)

	proxySyncer.syncMu.Lock()
	after := proxySyncer.targets[sharedPartitionKey]
	proxySyncer.syncMu.Unlock()

	assert.Empty(t, after.lastPushedHash,
		"a lost race invalidates the skip key so the next sync re-delivers the current config")
	assert.Same(t, goodCfg, after.lastCfg,
		"a lost race must not poison the replay cache with the config the replica never accepted")
	assert.Equal(t, 1, after.consecutivePushFail,
		"a lost race counts as a push failure (self-heals: a later successful sync resets it)")
}

// raceBarrierProxy mimics the real proxy config API (api.go + Router.UpdateConfig:
// 409 when a PUT version is below the current version, current version on GET),
// with a two-PUT barrier so the two concurrent syncs are applied newer-first,
// forcing the older config's PUT to land second and get the 409. Later PUTs (a
// recovery retry) and every GET are served live.
type raceBarrierProxy struct {
	server *httptest.Server

	mu          sync.Mutex
	current     int64
	lastHost    string
	winnerHost  string
	barrierDone bool
	buffered    int

	gate chan *bufferedConfigPut
}

type bufferedConfigPut struct {
	cfg  proxy.Config
	done chan int
}

func newRaceBarrierProxy(t *testing.T) *raceBarrierProxy {
	t.Helper()

	barrier := &raceBarrierProxy{gate: make(chan *bufferedConfigPut, 2)}
	barrier.server = httptest.NewServer(http.HandlerFunc(barrier.handle))
	t.Cleanup(barrier.server.Close)

	go barrier.coordinate()

	return barrier
}

func (rp *raceBarrierProxy) handle(writer http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet {
		rp.mu.Lock()
		status := proxy.ConfigStatus{Version: rp.current, Ready: true}
		rp.mu.Unlock()

		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)

		data, _ := json.Marshal(status)
		_, _ = writer.Write(data)

		return
	}

	if req.Method != http.MethodPut {
		writer.WriteHeader(http.StatusMethodNotAllowed)

		return
	}

	var cfg proxy.Config

	decodeErr := json.NewDecoder(req.Body).Decode(&cfg)
	if decodeErr != nil {
		writer.WriteHeader(http.StatusBadRequest)

		return
	}

	rp.mu.Lock()
	buffer := !rp.barrierDone && rp.buffered < 2
	if buffer {
		rp.buffered++
	}
	rp.mu.Unlock()

	if buffer {
		pending := &bufferedConfigPut{cfg: cfg, done: make(chan int, 1)}
		rp.gate <- pending

		select {
		case status := <-pending.done:
			writer.WriteHeader(status)
		case <-time.After(5 * time.Second):
			writer.WriteHeader(http.StatusInternalServerError)
		}

		return
	}

	rp.applyLive(writer, &cfg)
}

func (rp *raceBarrierProxy) coordinate() {
	first := <-rp.gate
	second := <-rp.gate

	higher, lower := first, second
	if lower.cfg.Version > higher.cfg.Version {
		higher, lower = lower, higher
	}

	rp.mu.Lock()
	rp.current = higher.cfg.Version
	rp.lastHost = firstConfigHost(&higher.cfg)
	rp.winnerHost = rp.lastHost
	rp.mu.Unlock()
	higher.done <- http.StatusOK

	// The lower-version PUT is stale against the applied higher version.
	lower.done <- http.StatusConflict

	rp.mu.Lock()
	rp.barrierDone = true
	rp.mu.Unlock()
}

func (rp *raceBarrierProxy) applyLive(writer http.ResponseWriter, cfg *proxy.Config) {
	rp.mu.Lock()
	defer rp.mu.Unlock()

	if cfg.Version > 0 && cfg.Version < rp.current {
		writer.WriteHeader(http.StatusConflict)

		return
	}

	rp.current = cfg.Version
	rp.lastHost = firstConfigHost(cfg)
	writer.WriteHeader(http.StatusOK)
}

// snapshot returns the host of the last-applied config and the host of the
// intended winner (the higher-version buffered config).
func (rp *raceBarrierProxy) snapshot() (string, string) {
	rp.mu.Lock()
	defer rp.mu.Unlock()

	return rp.lastHost, rp.winnerHost
}

func firstConfigHost(cfg *proxy.Config) string {
	if len(cfg.Rules) == 0 || len(cfg.Rules[0].Hostnames) == 0 {
		return ""
	}

	return cfg.Rules[0].Hostnames[0]
}

// TestSyncPartition_ConcurrentPush_OlderDoesNotOverwriteNewer is the #584
// integration test: two concurrent SyncPartition calls on the same partition
// build two configs whose versions come from the real monotonic counter, and
// the barrier applies the newer one first so the older one's PUT lands second
// and gets a 409. The stale-version recovery must recognise the lost race and
// abandon the push, so the replica keeps the newer config; the losing call
// surfaces ErrLostConfigPushRace, which invalidates the skip key for the next
// sync. Before the fix the older config force-overwrote the newer one.
func TestSyncPartition_ConcurrentPush_OlderDoesNotOverwriteNewer(t *testing.T) {
	t.Parallel()

	barrier := newRaceBarrierProxy(t)

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	proxySyncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	ctx := context.Background()

	endpoints := []string{barrier.server.URL + "/config"}

	var (
		waitGroup sync.WaitGroup
		errA      error
		errB      error
	)

	waitGroup.Add(2)

	go func() {
		defer waitGroup.Done()

		_, errA = proxySyncer.SyncPartition(ctx, sharedPartitionKey, "", endpoints,
			[]*gatewayv1.HTTPRoute{pushFallbackRoute("route-a", "a.example.com")}, nil, nil, nil)
	}()

	go func() {
		defer waitGroup.Done()

		_, errB = proxySyncer.SyncPartition(ctx, sharedPartitionKey, "", endpoints,
			[]*gatewayv1.HTTPRoute{pushFallbackRoute("route-b", "b.example.com")}, nil, nil, nil)
	}()

	waitGroup.Wait()

	lastHost, winnerHost := barrier.snapshot()
	require.Contains(t, []string{"a.example.com", "b.example.com"}, winnerHost)
	assert.Equal(t, winnerHost, lastHost,
		"the replica must end on the higher-version config; the older push must not force-overwrite it")

	// Exactly one call lost the race and must report it distinguishably; the
	// winner succeeds. Which route wins depends on build order under syncMu, so
	// accept either assignment.
	lostRaceErrors := 0

	for _, syncErr := range []error{errA, errB} {
		if syncErr == nil {
			continue
		}

		assert.True(t, errors.Is(syncErr, proxy.ErrLostConfigPushRace),
			"the only expected push error is a lost race, not a silent overwrite")

		lostRaceErrors++
	}

	assert.Equal(t, 1, lostRaceErrors, "exactly one concurrent sync loses the race")
}

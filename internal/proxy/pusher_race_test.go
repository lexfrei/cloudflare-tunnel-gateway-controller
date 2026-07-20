package proxy_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// reorderProxy is a faithful stand-in for the real proxy config API (see
// internal/proxy/api.go + Router.UpdateConfig): it rejects a PUT whose version
// is below its current version with 409 and reports its current version on GET.
// On top of that it adds a two-PUT barrier so a test can force the ORDER in
// which two concurrent pushes are applied at the replica, independent of which
// goroutine wins the network race: the first two PUTs are buffered and then
// applied in DESCENDING version order (newer first), which makes the older push
// deterministically land second and receive the 409. Any later PUT (a recovery
// retry) and every GET is served live.
type reorderProxy struct {
	server *httptest.Server

	mu          sync.Mutex
	current     int64
	lastApplied proxy.Config
	winnerHost  string // first hostname of the higher-version buffered config
	barrierDone bool
	buffered    int

	gate chan *pendingPut
}

type pendingPut struct {
	cfg  proxy.Config
	done chan int
}

func newReorderProxy(t *testing.T) *reorderProxy {
	t.Helper()

	proxyServer := &reorderProxy{gate: make(chan *pendingPut, 2)}
	proxyServer.server = httptest.NewServer(http.HandlerFunc(proxyServer.handle))
	t.Cleanup(proxyServer.server.Close)

	go proxyServer.coordinate()

	return proxyServer
}

func (rp *reorderProxy) url() string { return rp.server.URL + "/config" }

func (rp *reorderProxy) handle(writer http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet {
		rp.mu.Lock()
		status := proxy.ConfigStatus{Version: rp.current, Ready: true}
		rp.mu.Unlock()

		data, _ := json.Marshal(status)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
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
		pending := &pendingPut{cfg: cfg, done: make(chan int, 1)}
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

// coordinate applies the first two buffered PUTs in descending version order so
// the newer config is applied first and the older one deterministically gets
// the 409, reproducing "the older config's PUT arrives after the newer one".
func (rp *reorderProxy) coordinate() {
	first := <-rp.gate
	second := <-rp.gate

	higher, lower := first, second
	if lower.cfg.Version > higher.cfg.Version {
		higher, lower = lower, higher
	}

	rp.mu.Lock()
	rp.current = higher.cfg.Version
	rp.lastApplied = higher.cfg
	rp.winnerHost = firstHostname(&higher.cfg)
	rp.mu.Unlock()
	higher.done <- http.StatusOK

	// The lower-version PUT is now stale against the applied higher version.
	lower.done <- http.StatusConflict

	rp.mu.Lock()
	rp.barrierDone = true
	rp.mu.Unlock()
}

func (rp *reorderProxy) applyLive(writer http.ResponseWriter, cfg *proxy.Config) {
	rp.mu.Lock()
	defer rp.mu.Unlock()

	if cfg.Version > 0 && cfg.Version < rp.current {
		writer.WriteHeader(http.StatusConflict)

		return
	}

	rp.current = cfg.Version
	rp.lastApplied = *cfg
	writer.WriteHeader(http.StatusOK)
}

func (rp *reorderProxy) applied() (proxy.Config, string) {
	rp.mu.Lock()
	defer rp.mu.Unlock()

	return rp.lastApplied, rp.winnerHost
}

func firstHostname(cfg *proxy.Config) string {
	if len(cfg.Rules) == 0 || len(cfg.Rules[0].Hostnames) == 0 {
		return ""
	}

	return cfg.Rules[0].Hostnames[0]
}

func raceTestConfig(version int64, hostname string) *proxy.Config {
	return &proxy.Config{
		Version: version,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{hostname},
				Backends:  []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
			},
		},
	}
}

// TestConfigPusher_LosingRaceDoesNotOverwriteNewerConfig reproduces #584: two
// concurrent pushes carrying different configs hit one replica; the newer
// config (higher version) is applied first and the older one arrives second and
// gets a 409. The stale-version recovery must NOT force-overwrite the newer
// config with the older payload. Before the fix, the recovery bumped the
// counter above the replica and re-pushed the older config, leaving the replica
// on the OLDER config (the bug). After the fix, the recovery recognises a lost
// race and abandons the push, so the replica keeps the newer config.
func TestConfigPusher_LosingRaceDoesNotOverwriteNewerConfig(t *testing.T) {
	t.Parallel()

	proxyServer := newReorderProxy(t)
	pusher := proxy.NewConfigPusher(http.DefaultClient, "")

	oldCfg := raceTestConfig(100, "old.example.com")
	newCfg := raceTestConfig(200, "new.example.com")

	var (
		waitGroup            sync.WaitGroup
		oldResult, newResult proxy.PushResult
	)

	waitGroup.Add(2)

	go func() {
		defer waitGroup.Done()

		results := pusher.Push(t.Context(), newCfg, []string{proxyServer.url()})
		newResult = results[0]
	}()

	go func() {
		defer waitGroup.Done()

		results := pusher.Push(t.Context(), oldCfg, []string{proxyServer.url()})
		oldResult = results[0]
	}()

	waitGroup.Wait()

	applied, winnerHost := proxyServer.applied()
	require.Equal(t, "new.example.com", winnerHost, "the higher-version config must be the intended winner")
	assert.Equal(t, "new.example.com", firstHostname(&applied),
		"the replica must end on the newer config; the older push must not force-overwrite it")

	assert.NoError(t, newResult.Err, "the winning push succeeds")
	assert.True(t, errors.Is(oldResult.Err, proxy.ErrLostConfigPushRace),
		"the losing push must report a lost race, not a silent success that overwrote the newer config")
}

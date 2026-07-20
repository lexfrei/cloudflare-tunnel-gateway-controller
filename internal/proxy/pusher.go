package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/cockroachdb/errors"
)

var errUnexpectedStatusCode = errors.New("unexpected status code")

// PushResult holds the outcome of a config push to a single endpoint.
type PushResult struct {
	Endpoint string
	Err      error
}

// ConfigPusher pushes proxy config to enhanced-cloudflared replicas via HTTP API.
type ConfigPusher struct {
	client    *http.Client
	authToken string
}

// NewConfigPusher creates a ConfigPusher with the given HTTP client and optional auth token.
func NewConfigPusher(client *http.Client, authToken string) *ConfigPusher {
	return &ConfigPusher{client: client, authToken: authToken}
}

// Push sends the config to all endpoints concurrently and returns results.
func (p *ConfigPusher) Push(ctx context.Context, cfg *Config, endpoints []string) []PushResult {
	return p.PushWithToken(ctx, cfg, endpoints, p.authToken)
}

// PushWithToken pushes with an explicit Bearer token (empty = no auth
// header), overriding the pusher's default. Per-Gateway data planes carry
// their own tokens; using the shared default for them would hand the shared
// plane's credential to tenant-controlled pods.
func (p *ConfigPusher) PushWithToken(ctx context.Context, cfg *Config, endpoints []string, authToken string) []PushResult {
	body, err := json.Marshal(cfg)
	if err != nil {
		results := make([]PushResult, len(endpoints))
		for idx, endpoint := range endpoints {
			results[idx] = PushResult{
				Endpoint: endpoint,
				Err:      fmt.Errorf("marshal config: %w", err),
			}
		}

		return results
	}

	results := make([]PushResult, len(endpoints))

	var waitGroup sync.WaitGroup

	for idx, endpoint := range endpoints {
		waitGroup.Go(func() {
			results[idx] = p.pushToEndpoint(ctx, endpoint, body, authToken)
		})
	}

	waitGroup.Wait()

	return results
}

func (p *ConfigPusher) pushToEndpoint(ctx context.Context, endpoint string, body []byte, authToken string) PushResult {
	result := p.doPush(ctx, endpoint, body, authToken)
	if result.Err == nil {
		return result
	}

	// On stale version (409 Conflict), fetch the proxy's current version and
	// decide between the two causes of a 409, which need opposite handling.
	if !errors.Is(result.Err, errStaleVersion) {
		return result
	}

	proxyVersion, fetchErr := p.fetchProxyVersion(ctx, endpoint, authToken)
	if fetchErr != nil {
		return PushResult{Endpoint: endpoint, Err: fmt.Errorf("stale version recovery: %w", fetchErr)}
	}

	// The config version counter is monotonic within a process, so every
	// version THIS process has issued is <= its current value. If the replica's
	// version is at or below the counter, a concurrent same-process pusher
	// issued and delivered a newer config and won the race: re-pushing our older
	// payload would force-overwrite it, so abandon this push. The lost-race
	// error flows through the syncer's normal push-failure path, invalidating
	// the partition's steady-state skip key so the next sync re-delivers the
	// current desired config. Only a replica version strictly ABOVE the counter
	// proves the version came from a previous controller instance whose clock
	// ran ahead — the restart clock-skew case this recovery exists for, where
	// bumping the counter above the replica and re-pushing is correct.
	if proxyVersion <= configVersionCounter.Load() {
		return PushResult{Endpoint: endpoint, Err: errors.Wrap(ErrLostConfigPushRace, endpoint)}
	}

	bumpVersionCounter(proxyVersion)

	// Unmarshal into a local copy to avoid data races — multiple goroutines
	// may retry concurrently for different endpoints.
	var retryCfg Config

	unmarshalErr := json.Unmarshal(body, &retryCfg)
	if unmarshalErr != nil {
		return PushResult{Endpoint: endpoint, Err: fmt.Errorf("stale version recovery unmarshal: %w", unmarshalErr)}
	}

	retryCfg.Version = configVersionCounter.Add(1)

	retryBody, marshalErr := json.Marshal(retryCfg)
	if marshalErr != nil {
		return PushResult{Endpoint: endpoint, Err: fmt.Errorf("stale version recovery marshal: %w", marshalErr)}
	}

	return p.doPush(ctx, endpoint, retryBody, authToken)
}

func (p *ConfigPusher) doPush(ctx context.Context, endpoint string, body []byte, authToken string) PushResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return PushResult{Endpoint: endpoint, Err: fmt.Errorf("create request: %w", err)}
	}

	req.Header.Set("Content-Type", "application/json")

	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return PushResult{Endpoint: endpoint, Err: fmt.Errorf("send request: %w", err)}
	}
	defer resp.Body.Close()

	// Drain body to allow connection reuse.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusConflict {
		return PushResult{
			Endpoint: endpoint,
			Err:      errors.Wrap(errStaleVersion, endpoint),
		}
	}

	if resp.StatusCode != http.StatusOK {
		return PushResult{
			Endpoint: endpoint,
			Err:      errors.Wrapf(errUnexpectedStatusCode, "%d", resp.StatusCode),
		}
	}

	return PushResult{Endpoint: endpoint}
}

// fetchProxyVersion queries a proxy endpoint for its current config version.
func (p *ConfigPusher) fetchProxyVersion(ctx context.Context, endpoint, authToken string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return 0, fmt.Errorf("create GET request: %w", err)
	}

	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("send GET request: %w", err)
	}
	defer resp.Body.Close()

	// Check the status BEFORE decoding: a 401 (token mismatch in the
	// multi-token world) or any non-200 returns a non-JSON body, and decoding
	// it would mask the real cause as a confusing "invalid character" error.
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)

		return 0, errors.Wrapf(errUnexpectedStatusCode, "fetching proxy version: %d", resp.StatusCode)
	}

	var status ConfigStatus

	decodeErr := json.NewDecoder(resp.Body).Decode(&status)
	if decodeErr != nil {
		return 0, fmt.Errorf("decode proxy status: %w", decodeErr)
	}

	return status.Version, nil
}

// bumpVersionCounter ensures the config version counter is above proxyVersion.
func bumpVersionCounter(proxyVersion int64) {
	for {
		current := configVersionCounter.Load()
		if current > proxyVersion {
			return
		}

		if configVersionCounter.CompareAndSwap(current, proxyVersion+1) {
			return
		}
	}
}

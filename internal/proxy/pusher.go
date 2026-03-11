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
	results := make([]PushResult, len(endpoints))

	var waitGroup sync.WaitGroup

	for idx, endpoint := range endpoints {
		waitGroup.Go(func() {
			results[idx] = p.pushToEndpoint(ctx, endpoint, cfg)
		})
	}

	waitGroup.Wait()

	return results
}

func (p *ConfigPusher) pushToEndpoint(ctx context.Context, endpoint string, cfg *Config) PushResult {
	body, err := json.Marshal(cfg)
	if err != nil {
		return PushResult{Endpoint: endpoint, Err: fmt.Errorf("marshal config: %w", err)}
	}

	result := p.doPush(ctx, endpoint, body)
	if result.Err == nil {
		return result
	}

	// On stale version (409 Conflict), fetch the proxy's current version,
	// bump the config counter above it, and retry once. This recovers from
	// clock skew after controller restart (e.g., NTP adjustment).
	if !errors.Is(result.Err, errStaleVersion) {
		return result
	}

	proxyVersion, fetchErr := p.fetchProxyVersion(ctx, endpoint)
	if fetchErr != nil {
		return PushResult{Endpoint: endpoint, Err: fmt.Errorf("stale version recovery: %w", fetchErr)}
	}

	bumpVersionCounter(proxyVersion)

	// Mutate version directly — avoids JSON round-trip which could
	// lose data if marshaling is not perfectly round-trip-stable.
	cfg.Version = configVersionCounter.Add(1)

	retryBody, marshalErr := json.Marshal(cfg)
	if marshalErr != nil {
		return PushResult{Endpoint: endpoint, Err: fmt.Errorf("stale version recovery marshal: %w", marshalErr)}
	}

	return p.doPush(ctx, endpoint, retryBody)
}

func (p *ConfigPusher) doPush(ctx context.Context, endpoint string, body []byte) PushResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return PushResult{Endpoint: endpoint, Err: fmt.Errorf("create request: %w", err)}
	}

	req.Header.Set("Content-Type", "application/json")

	if p.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.authToken)
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
func (p *ConfigPusher) fetchProxyVersion(ctx context.Context, endpoint string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return 0, fmt.Errorf("create GET request: %w", err)
	}

	if p.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.authToken)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("send GET request: %w", err)
	}
	defer resp.Body.Close()

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

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
			results[idx] = p.pushToEndpoint(ctx, endpoint, body)
		})
	}

	waitGroup.Wait()

	return results
}

func (p *ConfigPusher) pushToEndpoint(ctx context.Context, endpoint string, body []byte) PushResult {
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

	if resp.StatusCode != http.StatusOK {
		return PushResult{
			Endpoint: endpoint,
			Err:      errors.Wrapf(errUnexpectedStatusCode, "%d", resp.StatusCode),
		}
	}

	return PushResult{Endpoint: endpoint}
}

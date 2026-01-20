//go:build conformance

package conformance

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"sigs.k8s.io/gateway-api/conformance/utils/config"
	"sigs.k8s.io/gateway-api/conformance/utils/roundtripper"
)

// CloudflareRoundTripper routes requests through Cloudflare CDN instead of
// directly to Gateway IP addresses. This is necessary because Cloudflare Tunnel
// does not expose an in-cluster IP - all traffic flows through Cloudflare's edge.
//
// The official Gateway API conformance tests build request URLs using
// Gateway.Status.Addresses, which for Cloudflare Tunnel contains a CNAME
// hostname (TUNNEL_ID.cfargotunnel.com), not an IP address.
//
// This RoundTripper intercepts requests and:
// 1. Rewrites the URL to use HTTPS with the request's Host header
// 2. Makes the request through Cloudflare CDN
// 3. Parses the echo server response format expected by conformance tests.
type CloudflareRoundTripper struct {
	// Debug enables verbose logging of requests and responses.
	Debug bool

	// TimeoutConfig contains timeout settings for HTTP requests.
	TimeoutConfig config.TimeoutConfig

	// MaxRetries is the maximum number of retry attempts for transient errors.
	// Cloudflare CDN propagation can cause initial request failures.
	// Default: 5
	MaxRetries int

	// RetryInterval is the duration to wait between retry attempts.
	// Default: 2s
	RetryInterval time.Duration
}

// CaptureRoundTrip implements the roundtripper.RoundTripper interface.
// It routes requests through Cloudflare CDN using the Host header for routing.
//
//nolint:gocritic // Interface defined by gateway-api conformance suite
func (c *CloudflareRoundTripper) CaptureRoundTrip(request roundtripper.Request) (*roundtripper.CapturedRequest, *roundtripper.CapturedResponse, error) {
	maxRetries := c.MaxRetries
	if maxRetries == 0 {
		maxRetries = 5
	}

	retryInterval := c.RetryInterval
	if retryInterval == 0 {
		retryInterval = 2 * time.Second
	}

	var (
		lastErr error
		cReq    *roundtripper.CapturedRequest
		cRes    *roundtripper.CapturedResponse
	)

	for attempt := range maxRetries {
		cReq, cRes, lastErr = c.doRoundTrip(request)
		if lastErr == nil {
			return cReq, cRes, nil
		}

		if c.Debug && request.T != nil {
			request.T.Logf("CloudflareRoundTripper: attempt %d/%d failed: %v", attempt+1, maxRetries, lastErr)
		}

		// Don't sleep after last attempt
		if attempt < maxRetries-1 {
			time.Sleep(retryInterval)
		}
	}

	return nil, nil, fmt.Errorf("all %d attempts failed, last error: %w", maxRetries, lastErr)
}

//nolint:gocritic // Interface type defined by gateway-api conformance suite
func (c *CloudflareRoundTripper) doRoundTrip(request roundtripper.Request) (*roundtripper.CapturedRequest, *roundtripper.CapturedResponse, error) {
	req, _, method, err := c.buildHTTPRequest(request)
	if err != nil {
		return nil, nil, err
	}

	resp, err := c.executeRequest(request, req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	return c.parseResponse(resp, method)
}

//nolint:gocritic // Interface type defined by gateway-api conformance suite
func (c *CloudflareRoundTripper) buildHTTPRequest(request roundtripper.Request) (*http.Request, string, string, error) {
	// Build the Cloudflare URL using the Host header
	host := request.Host
	if host == "" {
		host = request.URL.Host
	}

	// Cloudflare Tunnel always uses HTTPS at the edge
	cloudflareURL := url.URL{
		Scheme:   "https",
		Host:     host,
		Path:     request.URL.Path,
		RawQuery: request.URL.RawQuery,
	}

	method := request.Method
	if method == "" {
		method = http.MethodGet
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.TimeoutConfig.RequestTimeout)
	defer cancel()

	var reqBody io.Reader
	if request.Body != "" {
		reqBody = strings.NewReader(request.Body)
	}

	req, err := http.NewRequestWithContext(ctx, method, cloudflareURL.String(), reqBody)
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to create request: %w", err)
	}

	// Set Host header explicitly (important for routing)
	req.Host = host

	// Copy headers from the request
	if request.Headers != nil {
		for name, values := range request.Headers {
			for _, value := range values {
				req.Header.Add(name, value)
			}
		}
	}

	return req, host, method, nil
}

//nolint:gocritic // Interface type defined by gateway-api conformance suite
func (c *CloudflareRoundTripper) executeRequest(request roundtripper.Request, req *http.Request) (*http.Response, error) {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, //nolint:gosec // Conformance tests may use self-signed certs
		},
		DisableKeepAlives: true,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   c.TimeoutConfig.RequestTimeout,
	}

	if request.UnfollowRedirect {
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	if c.Debug && request.T != nil {
		dump, _ := httputil.DumpRequestOut(req, true)
		request.T.Logf("CloudflareRoundTripper Request:\n%s\n", dump)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if c.Debug && request.T != nil {
		dump, _ := httputil.DumpResponse(resp, true)
		request.T.Logf("CloudflareRoundTripper Response:\n%s\n", dump)
	}

	return resp, nil
}

func (c *CloudflareRoundTripper) parseResponse(resp *http.Response, method string) (*roundtripper.CapturedRequest, *roundtripper.CapturedResponse, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Parse captured request from echo server response
	cReq := &roundtripper.CapturedRequest{}

	if resp.Header.Get("Content-Type") == "application/json" {
		err = json.Unmarshal(body, cReq)
		if err != nil {
			// Not all responses are JSON, that's okay
			cReq.Method = method
		}
	} else {
		cReq.Method = method
	}

	// Build captured response
	cRes := &roundtripper.CapturedResponse{
		StatusCode:    resp.StatusCode,
		ContentLength: resp.ContentLength,
		Protocol:      resp.Proto,
		Headers:       resp.Header,
	}

	if resp.TLS != nil {
		cRes.PeerCertificates = resp.TLS.PeerCertificates
	}

	// Handle redirects
	if roundtripper.IsRedirect(resp.StatusCode) {
		redirectURL, locErr := resp.Location()
		if locErr == nil {
			cRes.RedirectRequest = &roundtripper.RedirectRequest{
				Scheme: redirectURL.Scheme,
				Host:   redirectURL.Hostname(),
				Port:   redirectURL.Port(),
				Path:   redirectURL.Path,
			}
		}
	}

	return cReq, cRes, nil
}

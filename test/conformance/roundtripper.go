//go:build conformance

package conformance

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"sigs.k8s.io/gateway-api/conformance/utils/config"
	"sigs.k8s.io/gateway-api/conformance/utils/roundtripper"
)

// TunnelRoundTripper routes requests through Cloudflare edge instead of directly
// to Gateway IP addresses. This is necessary because Cloudflare Tunnel does not
// expose an in-cluster IP — all traffic flows through Cloudflare's edge network.
//
// The official conformance tests build request URLs using Gateway.Status.Addresses,
// which for our controller contains a tunnel CNAME (TUNNEL_ID.cfargotunnel.com).
// The DefaultRoundTripper sends plain HTTP to that address, but Cloudflare edge
// requires HTTPS with proper Host header for routing.
//
// This RoundTripper:
//  1. Rewrites the URL to use HTTPS with the Host header as the authority
//  2. Connects to Cloudflare edge via TLS (trusting public CAs)
//  3. Parses the echo server JSON response expected by conformance tests
type TunnelRoundTripper struct {
	Debug         bool
	TimeoutConfig config.TimeoutConfig
}

// CaptureRoundTrip implements roundtripper.RoundTripper.
func (t *TunnelRoundTripper) CaptureRoundTrip(request roundtripper.Request) (*roundtripper.CapturedRequest, *roundtripper.CapturedResponse, error) {
	host := request.Host
	if host == "" {
		host = request.URL.Host
	}

	// Cloudflare edge always uses HTTPS. Rewrite URL to target the hostname
	// from the Host header — Cloudflare routes based on SNI/Host.
	cfURL := url.URL{
		Scheme:   "https",
		Host:     host,
		Path:     request.URL.Path,
		RawQuery: request.URL.RawQuery,
	}

	method := request.Method
	if method == "" {
		method = http.MethodGet
	}

	ctx, cancel := context.WithTimeout(context.Background(), t.TimeoutConfig.RequestTimeout)
	defer cancel()

	var reqBody io.Reader
	if request.Body != "" {
		reqBody = strings.NewReader(request.Body)
	}

	req, err := http.NewRequestWithContext(ctx, method, cfURL.String(), reqBody)
	if err != nil {
		return nil, nil, fmt.Errorf("building request: %w", err)
	}

	req.Host = host

	for name, values := range request.Headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// addr is "host:port" — keep it as-is, DNS will resolve the
			// test hostname to Cloudflare edge IPs via CNAME.
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: host,
		},
		DisableKeepAlives: true,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   t.TimeoutConfig.RequestTimeout,
	}

	if request.UnfollowRedirect {
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	if t.Debug && request.T != nil {
		dump, _ := httputil.DumpRequestOut(req, true)
		request.T.Logf("TunnelRoundTripper request:\n%s\n", string(dump))
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if t.Debug && request.T != nil {
		dump, _ := httputil.DumpResponse(resp, true)
		request.T.Logf("TunnelRoundTripper response:\n%s\n", string(dump))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("reading response body: %w", err)
	}

	cReq := &roundtripper.CapturedRequest{}

	if resp.Header.Get("Content-Type") == "application/json" {
		if jsonErr := json.Unmarshal(body, cReq); jsonErr != nil {
			cReq.Method = method
		}
	} else {
		cReq.Method = method
	}

	cRes := &roundtripper.CapturedResponse{
		StatusCode:    resp.StatusCode,
		ContentLength: resp.ContentLength,
		Protocol:      resp.Proto,
		Headers:       resp.Header,
	}

	if resp.TLS != nil {
		cRes.PeerCertificates = resp.TLS.PeerCertificates
	}

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

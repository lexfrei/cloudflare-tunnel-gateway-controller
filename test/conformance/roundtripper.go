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
	"os"
	"strings"

	"sigs.k8s.io/gateway-api/conformance/utils/config"
	"sigs.k8s.io/gateway-api/conformance/utils/roundtripper"
)

// TunnelRoundTripper routes requests through Cloudflare edge instead of directly
// to Gateway IP addresses. This is necessary because Cloudflare Tunnel does not
// expose an in-cluster IP — all traffic flows through Cloudflare's edge network.
//
// The *.cfargotunnel.com CNAME used as Gateway address only has AAAA records
// pointing to Cloudflare ULA (fd10::/8) which are not publicly routable.
// Instead, we connect to the real tunnel hostname (e.g. v2-test.lex.la) which
// has proper DNS pointing to Cloudflare edge. TLS SNI uses the tunnel hostname
// so Cloudflare routes traffic to our tunnel. The HTTP Host header carries the
// test-specific hostname for our proxy to route on.
type TunnelRoundTripper struct {
	Debug         bool
	TimeoutConfig config.TimeoutConfig
}

// originalHostHeader carries the test's intended host (the HTTP Host or the
// gRPC :authority) to the in-cluster proxy when the wire host must be the
// Cloudflare edge hostname instead. The proxy's extractHost reads it before the
// real Host, so it is the only signal that lets a non-edge hostname match.
// Shared by TunnelRoundTripper (HTTP) and TunnelGRPCClient (gRPC).
const originalHostHeader = "X-Original-Host"

// tunnelHostname returns the hostname used to reach the tunnel via Cloudflare edge.
func tunnelHostname() string {
	if h := os.Getenv("CONFORMANCE_TUNNEL_HOSTNAME"); h != "" {
		return h
	}

	return "v2-test.lex.la"
}

// CaptureRoundTrip implements roundtripper.RoundTripper.
//
// Note: request.Protocol is intentionally ignored. Even if a test asks for
// H2CPriorKnowledge, we always send HTTPS to the Cloudflare edge (the edge is
// HTTPS-only). The backend-protocol features (h2c, etc.) are validated by what
// the proxy-to-backend leg negotiates, not by the test client's wire format.
//
//nolint:gocritic // request is passed by value to match the roundtripper.RoundTripper interface signature.
func (t *TunnelRoundTripper) CaptureRoundTrip(request roundtripper.Request) (*roundtripper.CapturedRequest, *roundtripper.CapturedResponse, error) {
	edgeHost := tunnelHostname()
	method := requestMethod(&request)

	ctx, cancel := context.WithTimeout(context.Background(), t.TimeoutConfig.RequestTimeout)
	defer cancel()

	req, err := buildEdgeRequest(ctx, &request, edgeHost, method)
	if err != nil {
		return nil, nil, err
	}

	if t.Debug && request.T != nil {
		dump, _ := httputil.DumpRequestOut(req, true)
		request.T.Logf("TunnelRoundTripper request:\n%s\n", string(dump))
	}

	resp, err := t.edgeHTTPClient(request.UnfollowRedirect, edgeHost).Do(req)
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

	return captureRequest(resp, body, method), captureResponse(resp), nil
}

// requestMethod returns the request method, defaulting to GET.
func requestMethod(request *roundtripper.Request) string {
	if request.Method == "" {
		return http.MethodGet
	}

	return request.Method
}

// buildEdgeRequest targets the request at the tunnel edge hostname while
// carrying the test's intended Host via X-Original-Host so the in-cluster proxy
// can still route on it. Cloudflare edge validates the wire Host against
// configured DNS records, so any hostname not registered on the account
// (example.com, rewrite.example, etc.) must travel out-of-band.
func buildEdgeRequest(
	ctx context.Context,
	request *roundtripper.Request,
	edgeHost, method string,
) (*http.Request, error) {
	host := request.Host
	if host == "" {
		host = request.URL.Host
	}

	// Target the tunnel hostname (for DNS resolution) but preserve the
	// original path and query from the test request.
	cfURL := url.URL{
		Scheme:   "https",
		Host:     edgeHost,
		Path:     request.URL.Path,
		RawQuery: request.URL.RawQuery,
	}

	var reqBody io.Reader
	if request.Body != "" {
		reqBody = strings.NewReader(request.Body)
	}

	req, err := http.NewRequestWithContext(ctx, method, cfURL.String(), reqBody)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	if host != edgeHost {
		req.Header.Set(originalHostHeader, host)
	}

	req.Host = edgeHost

	for name, values := range request.Headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}

	return req, nil
}

// edgeHTTPClient builds an HTTP client that always dials the tunnel edge
// hostname with SNI set to it, so Cloudflare routes to our tunnel.
func (t *TunnelRoundTripper) edgeHTTPClient(unfollowRedirect bool, edgeHost string) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Replace the target with the tunnel hostname for DNS resolution.
			// The URL already points to edgeHost, so this is a safety net.
			_, port, _ := net.SplitHostPort(addr)
			edgeAddr := net.JoinHostPort(edgeHost, port)

			return (&net.Dialer{}).DialContext(ctx, network, edgeAddr)
		},
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// SNI must match a hostname with DNS CNAME to the tunnel,
			// otherwise Cloudflare edge won't route to our tunnel.
			ServerName: edgeHost,
		},
		DisableKeepAlives: true,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   t.TimeoutConfig.RequestTimeout,
	}

	if unfollowRedirect {
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	return client
}

// captureRequest decodes the echo backend's JSON reflection of the request the
// proxy delivered. A non-JSON body (or a decode failure) falls back to just the
// method, matching the upstream DefaultRoundTripper.
func captureRequest(resp *http.Response, body []byte, method string) *roundtripper.CapturedRequest {
	cReq := &roundtripper.CapturedRequest{}

	if resp.Header.Get("Content-Type") != "application/json" {
		cReq.Method = method

		return cReq
	}

	err := json.Unmarshal(body, cReq)
	if err != nil {
		cReq.Method = method
	}

	return cReq
}

// captureResponse builds the CapturedResponse, including peer certificates and
// the parsed redirect target when the status is a redirect.
func captureResponse(resp *http.Response) *roundtripper.CapturedResponse {
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

	return cRes
}

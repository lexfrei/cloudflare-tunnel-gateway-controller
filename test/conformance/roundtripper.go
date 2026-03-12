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

// tunnelHostname returns the hostname used to reach the tunnel via Cloudflare edge.
func tunnelHostname() string {
	if h := os.Getenv("CONFORMANCE_TUNNEL_HOSTNAME"); h != "" {
		return h
	}

	return "v2-test.lex.la"
}

// CaptureRoundTrip implements roundtripper.RoundTripper.
func (t *TunnelRoundTripper) CaptureRoundTrip(request roundtripper.Request) (*roundtripper.CapturedRequest, *roundtripper.CapturedResponse, error) {
	host := request.Host
	if host == "" {
		host = request.URL.Host
	}

	edgeHost := tunnelHostname()

	// Build URL targeting the tunnel hostname (for DNS resolution) but
	// preserving the original path and query from the test request.
	cfURL := url.URL{
		Scheme:   "https",
		Host:     edgeHost,
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

	// Cloudflare edge validates Host header against configured DNS records.
	// *.cfargotunnel.com is an internal hostname not reachable from internet.
	// Replace it with the edge hostname. For test-specific hostnames
	// (e.g., *.example.com from conformance suite), keep them — Cloudflare
	// should pass them through since SNI matches our tunnel's DNS record.
	if strings.HasSuffix(host, ".cfargotunnel.com") {
		host = edgeHost
	}

	req.Host = host

	for name, values := range request.Headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}

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

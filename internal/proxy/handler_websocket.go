package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"
)

const (
	// wsDialTimeout bounds how long the handler waits for a TCP / TLS
	// connection to the upstream WebSocket backend. The dial is the only
	// pre-handshake slow path; once the conn is established the handshake
	// is bound by wsHandshakeReadTimeout instead.
	wsDialTimeout = 30 * time.Second
	// wsHandshakeReadTimeout bounds how long the handler waits for the
	// backend's 101 Switching Protocols response. Independent from the
	// long-lived post-101 read window: the deadline is cleared before the
	// bidirectional copy begins.
	wsHandshakeReadTimeout = 30 * time.Second
)

// errBackendCABundleInvalid is returned when a BackendTLSPolicy's CA
// bundle PEM fails to parse during a WebSocket upgrade dial. Wrapped at
// the call site with the backend host context so the caller can include
// it in a single error chain without the linter flagging a dynamic error.
var errBackendCABundleInvalid = errors.New("BackendTLSPolicy CA bundle did not parse")

// proxyWebSocketUpgrade handles an HTTP/1.1 WebSocket upgrade request
// manually, bypassing httputil.ReverseProxy.
//
// stdlib's ReverseProxy.handleUpgradeResponse calls Hijack() on the
// ResponseWriter BEFORE writing the 101 status, then writes the raw
// HTTP/1.1 response bytes onto the hijacked conn. That works for an
// HTTP/1.1 response writer backed by a real TCP socket — the bytes go
// straight to the client. It does NOT work for cloudflared's HTTP/2
// ResponseWriter (`http2RespWriter`):
//
//   - http2RespWriter.Hijack requires statusWritten=true; ReverseProxy
//     never writes status before Hijack so the call fails and the
//     default error handler emits a non-101 response, which Cloudflare
//     edge then propagates as 403 / 502 to the client.
//   - Even if Hijack succeeded, the raw HTTP/1.1 status line ReverseProxy
//     writes (`HTTP/1.1 101 Switching Protocols\r\n…`) would be serialized
//     as HTTP/2 DATA frames over the tunnel; the edge cannot reconstruct
//     a WebSocket handshake from that. Native cloudflared sidesteps the
//     problem by using `WriteRespHeaders` to translate 101 → 200 and
//     serialize the upgrade headers into a single
//     `cf-cloudflared-user-headers` blob that the edge unpacks back to a
//     proper 101 on the HTTP/1.1 client side.
//
// We mirror native cloudflared's flow: dial the backend ourselves, write
// the upgrade request, parse the response, then call `w.WriteHeader(101)`
// (which the cloudflared writer translates correctly) BEFORE the hijack.
// After the hijack, only opaque WebSocket frames flow in both directions.
//
// Triggered only when (a) the operator declared the backend as
// WebSocket-capable via `appProtocol: kubernetes.io/ws[s]` and (b) the
// client request actually carries upgrade headers — see the gating in
// `proxyToBackend`.
func (h *Handler) proxyWebSocketUpgrade(
	w http.ResponseWriter,
	req *http.Request,
	backendURL *url.URL,
	backendTLS *BackendTLSConfig,
) {
	backendConn, err := dialBackendForUpgrade(req.Context(), backendURL, backendTLS)
	if err != nil {
		slog.Warn("websocket upgrade: backend dial failed",
			"error", err, "backend", backendURL.Host)
		http.Error(w, "bad gateway", http.StatusBadGateway)

		return
	}

	defer func() { _ = backendConn.Close() }()

	outReq := buildBackendUpgradeRequest(req, backendURL)

	err = outReq.Write(backendConn)
	if err != nil {
		slog.Warn("websocket upgrade: writing request to backend failed", "error", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)

		return
	}

	// Bound the wait for the 101 response separately from the long-lived
	// post-101 stream — clearing the deadline before bidirectional copy
	// below.
	_ = backendConn.SetReadDeadline(time.Now().Add(wsHandshakeReadTimeout))

	backendReader := bufio.NewReader(backendConn)

	resp, err := http.ReadResponse(backendReader, outReq)
	if err != nil {
		slog.Warn("websocket upgrade: reading backend response failed", "error", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)

		return
	}

	defer func() { _ = resp.Body.Close() }()

	_ = backendConn.SetReadDeadline(time.Time{})

	if resp.StatusCode != http.StatusSwitchingProtocols {
		// Backend refused the upgrade — forward the response as-is. Don't
		// hijack; the bytestream is a regular HTTP response body.
		copyHeaderValues(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)

		return
	}

	pipeWebSocket(w, backendConn, backendReader, resp.Header)
}

// pipeWebSocket completes the 101 handshake on the client side, then
// copies bytes bidirectionally between the hijacked client conn and the
// backend conn. Split from proxyWebSocketUpgrade to keep the per-function
// statement count within the funlen budget; the deferred close of
// `clientConn` only runs after both copy goroutines exit, ensuring the
// hijacked conn is freed cleanly.
func pipeWebSocket(
	w http.ResponseWriter,
	backendConn net.Conn,
	backendReader *bufio.Reader,
	responseHeader http.Header,
) {
	copyHeaderValues(w.Header(), responseHeader)
	w.WriteHeader(http.StatusSwitchingProtocols)

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		slog.Error("websocket upgrade: ResponseWriter does not implement http.Hijacker",
			"type", fmt.Sprintf("%T", w))

		return
	}

	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		slog.Error("websocket upgrade: hijack failed", "error", err)

		return
	}

	defer func() { _ = clientConn.Close() }()

	// Forward any client bytes already buffered into clientBuf before
	// hijack (a buffered Reader may have read past the request line).
	flushBufferedToBackend(clientBuf, backendConn)

	// Bidirectional pipe; first error from either direction ends the
	// session. Deferred clientConn/backendConn close cleans up both ends.
	errCh := make(chan error, 2)

	go func() {
		_, copyErr := io.Copy(backendConn, clientConn)
		errCh <- copyErr
	}()

	go func() {
		_, copyErr := io.Copy(clientConn, backendReader)
		errCh <- copyErr
	}()

	<-errCh
}

// buildBackendUpgradeRequest clones the inbound request and rewrites its
// URL + Host to point at the backend. The clone preserves headers
// (Connection, Upgrade, Sec-WebSocket-*) that the backend needs to
// complete the RFC 6455 handshake. RequestURI is cleared because outgoing
// http.Request.Write rejects it.
func buildBackendUpgradeRequest(req *http.Request, backendURL *url.URL) *http.Request {
	outReq := req.Clone(req.Context())
	outReq.URL = &url.URL{
		Scheme:   backendURL.Scheme,
		Host:     backendURL.Host,
		Path:     req.URL.Path,
		RawQuery: req.URL.RawQuery,
	}
	outReq.RequestURI = ""
	outReq.Host = backendURL.Host

	return outReq
}

// flushBufferedToBackend writes any bytes the hijack returned in the
// buffered Reader to the backend before the bidirectional copy starts.
// Typically empty for a WebSocket upgrade — the client sends no body and
// blocks for the 101 — but a buffered Reader may have read past the
// request line in some implementations.
func flushBufferedToBackend(clientBuf *bufio.ReadWriter, backendConn net.Conn) {
	if clientBuf == nil {
		return
	}

	buffered := clientBuf.Reader.Buffered()
	if buffered == 0 {
		return
	}

	prefix := make([]byte, buffered)

	_, readErr := io.ReadFull(clientBuf.Reader, prefix)
	if readErr != nil {
		return
	}

	_, _ = backendConn.Write(prefix)
}

// dialBackendForUpgrade opens a fresh TCP (or TLS) connection to the
// backend. The hijacked WebSocket conn cannot be returned to the cached
// transport pool, so every upgrade dials a new socket.
//
// TLS configuration honours the same BackendTLSPolicy / SAN-list logic
// the regular HTTP transport uses (see buildBackendTLSConfig). When the
// backend URL is https:// but no BackendTLSPolicy is attached, we fall
// back to system roots so the handshake still completes against a
// public-trust certificate — parity with stdlib http.Transport's
// default behaviour.
func dialBackendForUpgrade(
	ctx context.Context,
	backendURL *url.URL,
	backendTLS *BackendTLSConfig,
) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: wsDialTimeout}

	if backendURL.Scheme != schemeHTTPS {
		conn, err := dialer.DialContext(ctx, "tcp", backendURL.Host)
		if err != nil {
			return nil, fmt.Errorf("dial backend %q: %w", backendURL.Host, err)
		}

		return conn, nil
	}

	tlsConfig, err := backendUpgradeTLSConfig(backendURL, backendTLS)
	if err != nil {
		return nil, err
	}

	rawConn, err := dialer.DialContext(ctx, "tcp", backendURL.Host)
	if err != nil {
		return nil, fmt.Errorf("dial backend %q: %w", backendURL.Host, err)
	}

	tlsConn := tls.Client(rawConn, tlsConfig)

	handshakeErr := tlsConn.HandshakeContext(ctx)
	if handshakeErr != nil {
		_ = rawConn.Close()

		return nil, fmt.Errorf("backend TLS handshake to %q: %w", backendURL.Host, handshakeErr)
	}

	return tlsConn, nil
}

// backendUpgradeTLSConfig assembles the *tls.Config used for the WS
// upgrade dial. Splits from dialBackendForUpgrade so the latter stays
// within the funlen budget while keeping the TLS-vs-plaintext branch
// readable.
func backendUpgradeTLSConfig(backendURL *url.URL, backendTLS *BackendTLSConfig) (*tls.Config, error) {
	if backendTLS == nil {
		return &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: backendURL.Hostname(),
		}, nil
	}

	rootCAs := x509.NewCertPool()
	if backendTLS.CABundlePEM != "" {
		if ok := rootCAs.AppendCertsFromPEM([]byte(backendTLS.CABundlePEM)); !ok {
			return nil, fmt.Errorf("%w for backend %q", errBackendCABundleInvalid, backendURL.Host)
		}
	}

	return buildBackendTLSConfig(backendTLS, rootCAs), nil
}

// copyHeaderValues performs a shallow copy of src into dst, preserving
// multi-value headers. Used in the WS upgrade path; mirrors stdlib's
// internal copyHeader without forcing a httputil dependency from this
// file.
func copyHeaderValues(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

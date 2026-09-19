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
	"os"
	"runtime/debug"
	"time"
)

const (
	// defaultWSDialTimeout bounds how long the handler waits for a TCP /
	// TLS connection to the upstream WebSocket backend. The dial is the
	// only pre-handshake slow path; once the conn is established the
	// handshake is bound by defaultWSHandshakeReadTimeout instead. The
	// effective per-Handler value can be overridden via
	// WithWSDialTimeout; zero (or no option) means "use this default".
	defaultWSDialTimeout = 30 * time.Second
	// defaultWSHandshakeReadTimeout bounds how long the handler waits for
	// the backend's 101 Switching Protocols response. Independent from the
	// long-lived post-101 read window: the deadline is cleared before the
	// bidirectional copy begins. Overridable via
	// WithWSHandshakeReadTimeout.
	defaultWSHandshakeReadTimeout = 30 * time.Second
	// defaultWSIdleTimeout bounds an established session that has gone
	// silent. Nothing in the proxy closes a hijacked pair once the 101
	// has landed: both copies sit blocked on a read, so a client that
	// disappears without a close frame leaves its backend conn, two
	// goroutines and a tunnel stream held until something outside the
	// proxy reaps them. An hour is long enough that a session anything
	// is still using is not reached, and short enough that an abandoned
	// one does not outlive the pod. Overridable via
	// WithWSIdleTimeout — transport keepalives are not session bytes
	// and do not hold the bound open, so an application whose sockets
	// legitimately sit silent for longer needs a larger value.
	defaultWSIdleTimeout = time.Hour
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
	filters []Filter,
	hostname string,
) {
	backendConn, err := h.dialBackendForUpgrade(req.Context(), backendURL, backendTLS)
	if err != nil {
		slog.Warn("websocket upgrade: backend dial failed",
			"error", err, "backend", backendURL.Host)
		h.recordBackendError(hostname, backendErrReasonWSDial)
		http.Error(w, "bad gateway", http.StatusBadGateway)

		return
	}

	defer func() { _ = backendConn.Close() }()

	outReq := buildBackendUpgradeRequest(req, backendURL)

	err = outReq.Write(backendConn)
	if err != nil {
		slog.Warn("websocket upgrade: writing request to backend failed", "error", err)
		h.recordBackendError(hostname, backendErrReasonWSHandshake)
		http.Error(w, "bad gateway", http.StatusBadGateway)

		return
	}

	// Bound the wait for the 101 response separately from the long-lived
	// post-101 stream — clearing the deadline before bidirectional copy
	// below.
	_ = backendConn.SetReadDeadline(time.Now().Add(h.effectiveWSHandshakeReadTimeout()))

	backendReader := bufio.NewReader(backendConn)

	resp, err := http.ReadResponse(backendReader, outReq)
	if err != nil {
		slog.Warn("websocket upgrade: reading backend response failed", "error", err)
		h.recordBackendError(hostname, backendErrReasonWSHandshake)
		http.Error(w, "bad gateway", http.StatusBadGateway)

		return
	}

	defer func() { _ = resp.Body.Close() }()

	_ = backendConn.SetReadDeadline(time.Time{})

	// Apply rule-level + backend-level ResponseFilters (e.g.,
	// ResponseHeaderModifier) to the backend's response BEFORE copying
	// its headers to the client. The non-upgrade path runs the same
	// pipeline via httputil.ReverseProxy.ModifyResponse; the upgrade
	// path bypasses that callback, so we apply the filters here for both
	// the 101 success branch and the non-101 fallback. Gateway API
	// makes no exception for upgrade responses -- the spec-compliant
	// behavior is to transform headers consistently regardless of which
	// status code the backend returned.
	ApplyResponseFilters(filters, resp)

	if resp.StatusCode != http.StatusSwitchingProtocols {
		// Backend refused the upgrade — forward the response to the
		// client. Headers are already filter-modified by the
		// ApplyResponseFilters call above; the body streams through
		// unchanged. No hijack: the bytestream is a regular HTTP
		// response body, not a post-101 WebSocket frame stream.
		copyHeaderValues(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)

		return
	}

	pipeWebSocket(w, backendConn, backendReader, resp.Header, h.effectiveWSIdleTimeout())
}

// pipeWebSocket completes the 101 handshake on the client side, then
// copies bytes bidirectionally between the hijacked client conn and the
// backend conn. Split from proxyWebSocketUpgrade to keep the per-function
// statement count within the funlen budget.
//
// The wait ends on the FIRST direction to finish, which is the intended
// design: either end closing means the session is over. Whichever copy is
// still blocked is then freed by a close it is reading through — clientConn
// here, backendConn in the caller. Over a tunnel that clientConn close does
// nothing (see wsIdleGuard); the copy blocked on it is released instead when
// this handler returns and cloudflared finishes off the stream.
//
// idle is the third way for the wait to end: a session carrying no bytes at
// all for that long, which from inside the two copies is indistinguishable
// from a healthy one nobody happens to be talking on.
func pipeWebSocket(
	w http.ResponseWriter,
	backendConn net.Conn,
	backendReader *bufio.Reader,
	responseHeader http.Header,
	idle time.Duration,
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

	fromClient, fromBackend := armIdleBound(idle, backendConn, clientConn, backendReader)

	go copyWebSocketSide(backendConn, fromClient, errCh)
	go copyWebSocketSide(clientConn, fromBackend, errCh)

	err = <-errCh
	if errors.Is(err, os.ErrDeadlineExceeded) {
		slog.Info("websocket: no bytes in either direction past the idle bound, closing the session",
			"idle_timeout", idle)
	}
}

// armIdleBound instruments both directions of a hijacked session so the
// backend read deadline expires only once idle has passed with no bytes
// moving either way. A non-positive idle leaves the sources untouched
// and the session unbounded.
func armIdleBound(
	idle time.Duration,
	backendConn net.Conn,
	clientConn net.Conn,
	backendReader io.Reader,
) (io.Reader, io.Reader) {
	if idle <= 0 {
		return clientConn, backendReader
	}

	guard := &wsIdleGuard{backendConn: backendConn, idle: idle}
	guard.extend()

	return guard.wrap(clientConn), guard.wrap(backendReader)
}

// wsIdleGuard carries a hijacked session's idle bound. It arms a read
// deadline on the BACKEND conn and pushes it forward on every read from
// either direction, so the deadline fires only once the whole session
// has been silent and traffic one way keeps the other way alive.
//
// The backend conn is the actuator because it is the only end that is
// always a real socket. cloudflared hands the hijack a
// localProxyConnection (vendor/github.com/cloudflare/cloudflared
// /connection/connection.go) whose SetDeadline, SetReadDeadline and
// Close are all no-ops, and http2RespWriter.Close returns nil, so a
// bound placed on the client conn holds in an httptest test and does
// nothing over a tunnel. Expiring the backend read ends the wait in
// pipeWebSocket, which closes both ends on its way out.
//
// Wrapping both sources costs io.Copy its WriteTo fast path on the
// backend direction, where the source is a *bufio.Reader, so each
// session allocates a copy buffer there instead. Accepted: the wrapper
// is what makes traffic one way hold the other way's window open, and
// the bytes are already being copied through user space on every hop of
// this path.
//
// A read deadline is only observed by a goroutine that reaches a read,
// so the one session this does not reclaim is one whose
// backend-to-client copy is wedged writing to a client that has stopped
// reading: no read is attempted on the backend conn, and the expired
// deadline sits unnoticed. Bounding that case means a write deadline,
// which would also cut off a client that is merely slow.
type wsIdleGuard struct {
	backendConn net.Conn
	idle        time.Duration
	// src is the wrapped direction; nil on the instance that only
	// carries the bound.
	src io.Reader
}

// Read passes the wrapped direction through and counts any bytes it
// carried as session activity.
//
// The read error goes back to io.Copy exactly as it arrived: io.Copy
// tests it against io.EOF by identity, so wrapping here would turn a
// clean end-of-stream into a copy failure.
func (g *wsIdleGuard) Read(p []byte) (int, error) {
	n, err := g.src.Read(p)
	if n > 0 {
		g.extend()
	}

	return n, err //nolint:wrapcheck // io.Copy compares this error against io.EOF by identity
}

func (g *wsIdleGuard) extend() {
	_ = g.backendConn.SetReadDeadline(time.Now().Add(g.idle))
}

// wrap returns src instrumented so bytes read through it count as
// session activity.
func (g *wsIdleGuard) wrap(src io.Reader) io.Reader {
	return &wsIdleGuard{backendConn: g.backendConn, idle: g.idle, src: src}
}

// errWebSocketCopyPanic ends a session whose copy goroutine panicked.
var errWebSocketCopyPanic = errors.New("panic while copying websocket bytes")

// copyWebSocketSide copies one direction of a hijacked WebSocket session and
// reports how it ended.
//
// The recover is what keeps one connection's failure from being everyone's: a
// panic in a goroutine cannot be recovered by whoever started it, so an
// unguarded copy takes the process down and every other tenant's connection
// with it. This holds on every transport, not only QUIC — x/net/http2's
// per-stream recover covers the handler goroutine, never the ones it spawns.
func copyWebSocketSide(dst io.Writer, src io.Reader, errCh chan<- error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("websocket: panic while copying, closing the session",
				"panic", recovered,
				"stack", string(debug.Stack()))

			errCh <- errWebSocketCopyPanic
		}
	}()

	_, copyErr := io.Copy(dst, src)
	errCh <- copyErr
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
	// Prepend the backend's base path and merge its query (e.g. an
	// ExternalBackend's spec.path "/v1?x=1"), matching the non-WebSocket
	// rewrite; a no-op for Service/ServiceImport URLs.
	applyBackendBasePath(outReq.URL, backendURL.Path, backendURL.RawQuery)
	outReq.RequestURI = ""
	outReq.Host = backendURL.Host

	// req.Clone copies every header, so the two proxy-internal ones the plain
	// path deletes have to be deleted here too: neither means anything to a
	// backend, and the marker would advertise how this proxy signals to itself.
	outReq.Header.Del(originalHostHeader)
	outReq.Header.Del(hostRewrittenHeader)

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
//
// Method on Handler (not a free function) so the per-Handler
// effectiveWSDialTimeout flows in -- otherwise the function couldn't
// see the WithWSDialTimeout override and would silently keep using
// the 30s default.
func (h *Handler) dialBackendForUpgrade(
	ctx context.Context,
	backendURL *url.URL,
	backendTLS *BackendTLSConfig,
) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: h.effectiveWSDialTimeout()}

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

package proxy_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// errFakeStatusNotWritten matches the cloudflared http2RespWriter
// error message verbatim at the time this fake was authored. Declared
// as a static var so the err113 linter is satisfied and so the brittle
// string lives at one fix-up point.
//
// Caveat: tests that string-match against this constant assert the
// FAKE's own behaviour (i.e. that the fake produces the exact text
// this constant declares), not that the fake stayed in sync with the
// upstream cloudflared message. A future cloudflared bump that renames
// the message will NOT cause this test to fail — re-vendoring updates
// `vendor/`, but this constant remains pinned to the old wording.
// Detecting genuine cloudflared drift requires either a separate
// integration check against the real http2RespWriter or a manual
// inspection at re-vendor time.
var errFakeStatusNotWritten = errors.New("status not yet written before attempting to hijack connection")

// fakeCloudflaredRespWriter mimics the contract of cloudflared's
// `http2RespWriter` (vendor/github.com/cloudflare/cloudflared/connection
// /http2.go) for unit tests that need to exercise tunnel-mode behaviour
// without a real Cloudflare Tunnel. See `internal/proxy/handler_websocket
// .go` for the production-side rationale on why the custom WebSocket
// upgrade path exists in the first place.
//
// The two contract points that mattered for the production WebSocket bug:
//
//  1. `Hijack` returns an error when WriteHeader has not been called yet
//     (cloudflared raises `status not yet written before attempting to
//     hijack connection`). This is exactly the precondition that broke
//     `httputil.ReverseProxy.handleUpgradeResponse` over HTTP/2 — it
//     calls Hijack BEFORE writing the 101 status. The fake reproduces
//     that failure deterministically so the regression is pinned in CI.
//
//  2. `WriteHeader(101)` is translated to status 200 on the recorded
//     response (HTTP/2 has no 1xx semantics; cloudflared rewrites the
//     status before sending to the edge, which translates back to 101
//     for HTTP/1.1 clients). The fake records the translated status so
//     tests can assert the rewrite happened.
//
// Post-hijack bytes go through a `net.Pipe` pair so a test can drive
// raw bytes into the handler's hijacked conn via `HijackedClient`. For
// most WebSocket-path tests, asserting `Status() == 200` plus
// `Hijacked() == true` is sufficient — the byte round-trip is already
// covered by the `httptest.NewServer`-based integration tests, and the
// fake's purpose is to pin the HTTP/2 hijack precondition.
//
// `Write` does NOT auto-set status. The exact production wire
// behaviour is more nuanced: cloudflared's `http2RespWriter.Write`
// delegates to the stdlib HTTP/2 response writer, which DOES
// implicitly call `WriteHeader(200)` on the first Write if status was
// not set. The fake deliberately diverges from that stdlib-implicit
// auto-status because the production WebSocket path always calls
// `WriteHeader` explicitly before `Write` / `Hijack`; the strict
// fake catches any future caller that violates that invariant
// instead of papering over it. Callers that need stdlib-style
// behaviour should use `httptest.ResponseRecorder`.
type fakeCloudflaredRespWriter struct {
	mu sync.Mutex

	headers       http.Header
	body          bytes.Buffer
	status        int
	statusWritten bool
	hijacked      bool

	// serverSide is returned from Hijack so the handler's post-101
	// bidirectional copy reads / writes against the pipe. clientSide is
	// the matching end exposed via HijackedClient() so a test driver can
	// inject and read post-upgrade bytes.
	serverSide net.Conn
	clientSide net.Conn
}

func newFakeCloudflaredRespWriter() *fakeCloudflaredRespWriter {
	server, client := net.Pipe()

	return &fakeCloudflaredRespWriter{
		headers:    make(http.Header),
		serverSide: server,
		clientSide: client,
	}
}

func (f *fakeCloudflaredRespWriter) Header() http.Header {
	return f.headers
}

// Write records body bytes for later inspection via Body(). Unlike
// stdlib's http.ResponseWriter, it does NOT auto-call WriteHeader(200)
// on the first Write — cloudflared's http2RespWriter.Write does not
// either, and tests using this fake should call WriteHeader explicitly
// the way the production WebSocket path does.
func (f *fakeCloudflaredRespWriter) Write(payload []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// bytes.Buffer is not safe for concurrent use; keep it under the
	// same mutex as statusWritten so a future test exercising parallel
	// Write calls cannot race on the body buffer either. The error
	// wrap is defensive — bytes.Buffer.Write never returns a non-nil
	// error today (per its stdlib docs), but if the underlying writer
	// is ever swapped for one that does, the wrapping makes the
	// origin obvious at the test failure site without forcing every
	// caller to add `%w`.
	n, err := f.body.Write(payload)
	if err != nil {
		return n, fmt.Errorf("fake cloudflared resp writer body buffer: %w", err)
	}

	return n, nil
}

// WriteHeader mirrors cloudflared's http2RespWriter — 101 is translated
// to 200 because HTTP/2 has no 1xx semantics. Calls after hijack are
// no-ops (cloudflared logs a warning and returns). First-write-wins
// after that.
func (f *fakeCloudflaredRespWriter) WriteHeader(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.hijacked || f.statusWritten {
		return
	}

	if status == http.StatusSwitchingProtocols {
		status = http.StatusOK
	}

	f.status = status
	f.statusWritten = true
}

// Hijack enforces the cloudflared precondition: status must be written
// first. Returns the server side of an internal net.Pipe so the handler
// can do its bidirectional copy. The matching client side is exposed
// via HijackedClient().
func (f *fakeCloudflaredRespWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.statusWritten {
		return nil, nil, errFakeStatusNotWritten
	}

	if f.hijacked {
		return nil, nil, http.ErrHijacked
	}

	f.hijacked = true
	brw := bufio.NewReadWriter(bufio.NewReader(f.serverSide), bufio.NewWriter(f.serverSide))

	return f.serverSide, brw, nil
}

// Flush is a no-op — cloudflared's http2RespWriter flushes underlying
// HTTP/2 frames, but for the fake there is nothing to flush.
func (f *fakeCloudflaredRespWriter) Flush() {}

// Status returns the recorded status code (after any 101→200
// translation). Returns zero if no WriteHeader call ever landed —
// the only branch in which status stays unset, because Write does
// not auto-set status and Hijack rejects when statusWritten is false.
func (f *fakeCloudflaredRespWriter) Status() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.status
}

// Body returns a copy of the bytes written to the response. Useful for
// tests that exercise non-upgrade tunnel-mode paths and need to assert
// on the response body; the production WebSocket path bypasses Write
// entirely (writes go through the hijacked conn instead).
func (f *fakeCloudflaredRespWriter) Body() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.body.Bytes())
}

// Hijacked reports whether the writer was successfully hijacked.
func (f *fakeCloudflaredRespWriter) Hijacked() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.hijacked
}

// HijackedClient returns the test-side conn of the post-upgrade pipe.
// Bytes written here flow to the handler's hijacked read; bytes the
// handler writes to its hijacked conn arrive here.
func (f *fakeCloudflaredRespWriter) HijackedClient() net.Conn {
	return f.clientSide
}

// Compile-time check that the fake satisfies both http.ResponseWriter
// and http.Hijacker, so future signature drift in stdlib surfaces here.
var (
	_ http.ResponseWriter = (*fakeCloudflaredRespWriter)(nil)
	_ http.Hijacker       = (*fakeCloudflaredRespWriter)(nil)
)

// TestFakeCloudflaredRespWriter_HijackRequiresStatusFirst pins the
// cloudflared precondition: Hijack before WriteHeader is an error. This
// is the exact failure mode that broke production tunnel-mode WebSocket
// in the original ReverseProxy path; without enforcing it the fake
// would falsely pass tests that would fail in production.
func TestFakeCloudflaredRespWriter_HijackRequiresStatusFirst(t *testing.T) {
	t.Parallel()

	fake := newFakeCloudflaredRespWriter()
	t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

	_, _, err := fake.Hijack()
	require.Error(t, err, "Hijack must reject when no status has been written — mirrors cloudflared http2RespWriter")
	assert.Contains(t, err.Error(), "status not yet written",
		"error message must surface the precondition by name so future debug output stays diagnostic")

	fake.WriteHeader(http.StatusSwitchingProtocols)

	conn, _, err := fake.Hijack()
	require.NoError(t, err, "Hijack must succeed once status has been written")
	require.NotNil(t, conn)
	assert.True(t, fake.Hijacked(), "Hijacked() must reflect the successful hijack")
}

// TestFakeCloudflaredRespWriter_NonUpgradeWriteRecordsBody covers the
// non-upgrade tunnel-mode path the WebSocket tests never reach. The
// fake exists primarily for the upgrade contract, but it also has to
// behave correctly when a tunnel-mode handler writes a regular HTTP
// response body — future tests of non-WS tunnel paths (e.g. CORS
// preflight, ResponseFilters, etc.) will use the same fake and need
// Body() to return what was written. Without this pin a refactor of
// Write or Body could silently break those future tests.
func TestFakeCloudflaredRespWriter_NonUpgradeWriteRecordsBody(t *testing.T) {
	t.Parallel()

	fake := newFakeCloudflaredRespWriter()
	t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

	fake.Header().Set("Content-Type", "application/json")
	fake.WriteHeader(http.StatusCreated)

	payload := []byte(`{"resource":"created"}`)

	n, err := fake.Write(payload)
	require.NoError(t, err)
	assert.Equal(t, len(payload), n, "Write must report all bytes written")

	assert.Equal(t, http.StatusCreated, fake.Status(), "WriteHeader status must persist through subsequent Write calls")
	assert.Equal(t, payload, fake.Body(), "Body() must return the bytes written via Write — verbatim, no truncation, no re-ordering")
	assert.Equal(t, "application/json", fake.Header().Get("Content-Type"),
		"Header() must surface the headers set before WriteHeader, so non-upgrade tunnel-mode tests can assert on them")
}

// TestFakeCloudflaredRespWriter_IdempotenceAndPostHijackNoOps covers
// the three contract branches the load-bearing tests never reach but
// the fake documents: first-write-wins on WriteHeader, WriteHeader
// after Hijack is a silent no-op, and a second Hijack returns
// http.ErrHijacked. All three mirror cloudflared http2RespWriter
// semantics. Without these pins a future refactor of the fake could
// silently break callers that rely on the documented contract.
func TestFakeCloudflaredRespWriter_IdempotenceAndPostHijackNoOps(t *testing.T) {
	t.Parallel()

	t.Run("WriteHeader is first-write-wins", func(t *testing.T) {
		t.Parallel()

		fake := newFakeCloudflaredRespWriter()
		t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

		fake.WriteHeader(http.StatusCreated)
		fake.WriteHeader(http.StatusInternalServerError)

		assert.Equal(t, http.StatusCreated, fake.Status(),
			"second WriteHeader must be a no-op — mirrors cloudflared and stdlib alike")
	})

	t.Run("WriteHeader after Hijack is a silent no-op", func(t *testing.T) {
		t.Parallel()

		fake := newFakeCloudflaredRespWriter()
		t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

		fake.WriteHeader(http.StatusSwitchingProtocols)
		_, _, err := fake.Hijack()
		require.NoError(t, err)

		// Post-hijack WriteHeader call must NOT mutate status — cloudflared
		// logs a warning and returns; the fake silently no-ops.
		fake.WriteHeader(http.StatusBadGateway)

		assert.Equal(t, http.StatusOK, fake.Status(),
			"WriteHeader after Hijack must not change the recorded status")
	})

	t.Run("second Hijack returns ErrHijacked", func(t *testing.T) {
		t.Parallel()

		fake := newFakeCloudflaredRespWriter()
		t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

		fake.WriteHeader(http.StatusSwitchingProtocols)

		_, _, firstErr := fake.Hijack()
		require.NoError(t, firstErr)

		_, _, secondErr := fake.Hijack()
		require.Error(t, secondErr, "a second Hijack must reject — the conn is already detached")
		assert.ErrorIs(t, secondErr, http.ErrHijacked,
			"the rejection must be the standard ErrHijacked sentinel so callers can pattern-match it")
	})
}

// TestFakeCloudflaredRespWriter_TranslatesSwitchingProtocols pins the
// 101→200 status rewrite cloudflared performs because HTTP/2 has no
// 1xx semantics. The Cloudflare edge translates the 200 back to 101
// for HTTP/1.1 clients on the wire — but as far as the proxy code is
// concerned the recorded status must be 200.
func TestFakeCloudflaredRespWriter_TranslatesSwitchingProtocols(t *testing.T) {
	t.Parallel()

	fake := newFakeCloudflaredRespWriter()
	t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

	fake.WriteHeader(http.StatusSwitchingProtocols)

	assert.Equal(t, http.StatusOK, fake.Status(),
		"WriteHeader(101) must record status 200 — HTTP/2 prohibits 1xx so cloudflared rewrites before sending")
}

// TestFakeCloudflaredRespWriter_NonUpgradeStatusPassesThrough confirms
// the translation only applies to 101 — other statuses must be recorded
// verbatim. Without this guard a future refactor could over-eagerly
// rewrite 200 / 502 / etc. and the production code path would still
// pass tests for the wrong reason.
func TestFakeCloudflaredRespWriter_NonUpgradeStatusPassesThrough(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status int
	}{
		{"200 OK passes through", http.StatusOK},
		{"404 passes through", http.StatusNotFound},
		{"502 passes through", http.StatusBadGateway},
		{"504 passes through", http.StatusGatewayTimeout},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeCloudflaredRespWriter()
			t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

			fake.WriteHeader(tc.status)
			assert.Equal(t, tc.status, fake.Status(), "only 101 is translated; %d must pass through unchanged", tc.status)
		})
	}
}

// TestHandler_BackendProtocolWebSocket_TunnelMode_HijackAfterStatus is
// the load-bearing test that the fake exists for: it drives a real
// WebSocket upgrade request through the proxy handler using the fake
// cloudflared writer and asserts the handler reaches the post-101
// hijack point with status written first. Without the custom upgrade
// path landed in #244, this test fails because httputil.ReverseProxy
// .handleUpgradeResponse hijacks before WriteHeader — exactly the
// contract the fake enforces.
//
// Byte-level round-trip is deliberately NOT exercised here:
// websocket.NewClient on the test side would attempt its own HTTP
// handshake over the already-hijacked pipe and read the backend's
// raw WS frames as a "malformed HTTP response". The existing
// httptest.NewServer-based TestHandler_BackendProtocolWebSocket_Echo
// covers the byte path through a real http.Hijacker; this test
// covers the HTTP/2-style precondition the real tunnel imposes.
// Together the two pin the full production contract.
//
// Pinning this locally means any future refactor that re-routes
// WebSocket upgrades back through ReverseProxy (or through any other
// hijack-before-WriteHeader path) breaks deterministically in CI
// instead of through a production-only failure.
func TestHandler_BackendProtocolWebSocket_TunnelMode_HijackAfterStatus(t *testing.T) {
	t.Parallel()

	backend := newWSEchoBackend(t, false)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Matches: []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{URL: backend.URL, Weight: 1, Protocol: proxy.BackendProtocolHTTP, WebSocket: true},
				},
			},
		},
	}))

	handler := proxy.NewHandler(router)
	fake := newFakeCloudflaredRespWriter()

	t.Cleanup(func() {
		_ = fake.serverSide.Close()
		_ = fake.clientSide.Close()
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

	// Run the handler in a goroutine — after hijack it blocks in
	// bidirectional copy until the pipe closes. Closing the client side
	// in cleanup unblocks the copy goroutines cleanly.
	handlerDone := make(chan struct{})

	go func() {
		defer close(handlerDone)
		handler.ServeHTTP(fake, req)
	}()

	require.Eventually(t, fake.Hijacked, 5*time.Second, 25*time.Millisecond,
		"handler must reach the post-101 hijack within 5s — anything longer suggests the WS path "+
			"is silently going through the broken ReverseProxy.handleUpgradeResponse flow")

	assert.Equal(t, http.StatusOK, fake.Status(),
		"recorded status must be 200 — the WriteHeader(101) call was correctly translated by the fake, "+
			"proving the handler wrote status BEFORE hijacking (the exact contract that broke in production)")

	// Closing the client side breaks the copy loop on the handler; it
	// returns and handlerDone fires.
	_ = fake.HijackedClient().Close()
	<-handlerDone
}

// TestReverseProxyUpgradeOverFakeWriter_ReproducesProductionFailure is
// the true negative control: it runs the stdlib
// httputil.ReverseProxy upgrade flow against a real WebSocket backend
// using the fake cloudflared writer as the response writer, and
// asserts the upgrade FAILS with the exact symptom we saw in
// production (status != 101, hijack never succeeded).
//
// This is the regression pin we'd need to repurpose into a positive
// test if anyone tries to revert the custom proxyWebSocketUpgrade path
// in #244 and route WebSocket through ReverseProxy again. Run the
// reverted handler over this fake → this test passes (in the wrong
// way) → the rest of the proxy WS suite fails → the regression is
// caught locally before push.
//
// Mechanism: ReverseProxy.handleUpgradeResponse calls Hijack BEFORE
// writing 101. The fake's hijack precondition rejects (mirroring
// cloudflared's http2RespWriter), the default error handler writes
// 502, and we assert that 502 is what landed on the writer. Without
// the fake faithfully reproducing the precondition, this test
// silently flips to passing the wrong way and the load-bearing
// regression pin disappears.
func TestReverseProxyUpgradeOverFakeWriter_ReproducesProductionFailure(t *testing.T) {
	t.Parallel()

	backend := newWSEchoBackend(t, false)

	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)

	reverseProxy := httputil.NewSingleHostReverseProxy(backendURL)

	fake := newFakeCloudflaredRespWriter()
	t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, backend.URL+"/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

	reverseProxy.ServeHTTP(fake, req)

	// ReverseProxy.handleUpgradeResponse calls Hijack before WriteHeader.
	// The fake rejects (statusWritten=false), and the default error
	// handler writes 502 via fake.WriteHeader. Without the fix in #244
	// this is exactly the production-visible failure.
	assert.Equal(t, http.StatusBadGateway, fake.Status(),
		"ReverseProxy over an HTTP/2-style writer must record 502 — the default error handler "+
			"is the only path that runs when Hijack is rejected for missing WriteHeader. "+
			"If this becomes anything else, either the fake's precondition is broken or stdlib changed its upgrade flow.")
	assert.False(t, fake.Hijacked(),
		"a hijack-before-WriteHeader call must not flip the hijacked flag — otherwise the precondition is a no-op")
}

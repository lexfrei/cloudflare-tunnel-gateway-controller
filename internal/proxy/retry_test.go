package proxy_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// failingBackend answers status for the first failures hits, then 200 "ok".
// It returns the server and a pointer to its hit counter.
func failingBackend(t *testing.T, failures int32, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var hits atomic.Int32

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= failures {
			writer.WriteHeader(status)
			fmt.Fprintf(writer, "failure %d", hits.Load())

			return
		}

		_, _ = io.WriteString(writer, "ok")
	}))
	t.Cleanup(backend.Close)

	return backend, &hits
}

func retryHandler(t *testing.T, backendURL string, retry *proxy.RouteRetry, timeouts *proxy.RouteTimeouts) *proxy.Handler {
	t.Helper()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Matches:  []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
			Backends: []proxy.BackendRef{{URL: backendURL, Weight: 1, Protocol: proxy.BackendProtocolHTTP}},
			Retry:    retry,
			Timeouts: timeouts,
		}},
	}))

	return proxy.NewHandler(router)
}

func serve(handler http.Handler, method, body string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequestWithContext(context.Background(), method, "http://app.example.com/", reader)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	return rec
}

func TestRetry_StatusCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		retry      *proxy.RouteRetry
		failures   int32
		status     int
		wantStatus int
		wantBody   string
		wantHits   int32
	}{
		{
			name:       "recovers within the attempt budget",
			retry:      &proxy.RouteRetry{Codes: []int{500}, Attempts: 3},
			failures:   2,
			status:     http.StatusInternalServerError,
			wantStatus: http.StatusOK,
			wantBody:   "ok",
			wantHits:   3,
		},
		{
			name:       "exhausted budget surfaces the last backend response",
			retry:      &proxy.RouteRetry{Codes: []int{500}, Attempts: 3},
			failures:   4,
			status:     http.StatusInternalServerError,
			wantStatus: http.StatusInternalServerError,
			wantBody:   "failure 4",
			wantHits:   4,
		},
		{
			name:       "unlisted code is not retried",
			retry:      &proxy.RouteRetry{Codes: []int{500}, Attempts: 3},
			failures:   1,
			status:     http.StatusServiceUnavailable,
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   "failure 1",
			wantHits:   1,
		},
		{
			name:       "no retry policy means one attempt",
			retry:      nil,
			failures:   1,
			status:     http.StatusInternalServerError,
			wantStatus: http.StatusInternalServerError,
			wantBody:   "failure 1",
			wantHits:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			backend, hits := failingBackend(t, tt.failures, tt.status)
			rec := serve(retryHandler(t, backend.URL, tt.retry, nil), http.MethodGet, "")

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, tt.wantBody, rec.Body.String())
			assert.Equal(t, tt.wantHits, hits.Load())
		})
	}
}

func TestRetry_ReplaysRequestBody(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		bodies []string
		hits   atomic.Int32
	)

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		data, _ := io.ReadAll(req.Body)

		mu.Lock()
		bodies = append(bodies, string(data))
		mu.Unlock()

		if hits.Add(1) == 1 {
			writer.WriteHeader(http.StatusBadGateway)
		}
	}))
	t.Cleanup(backend.Close)

	rec := serve(retryHandler(t, backend.URL, &proxy.RouteRetry{Codes: []int{502}, Attempts: 1}, nil), http.MethodPost, "payload")

	assert.Equal(t, http.StatusOK, rec.Code)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"payload", "payload"}, bodies, "the retry must resend the full body")
}

func TestRetry_OversizedBodyIsSentOnceIntact(t *testing.T) {
	t.Parallel()

	large := strings.Repeat("x", proxy.MaxRetryBodyBytes+1)

	var received atomic.Int64

	var hits atomic.Int32

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		n, _ := io.Copy(io.Discard, req.Body)
		received.Store(n)
		hits.Add(1)
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(backend.Close)

	rec := serve(retryHandler(t, backend.URL, &proxy.RouteRetry{Codes: []int{500}, Attempts: 2}, nil), http.MethodPost, large)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, int32(1), hits.Load(), "a body too large to replay must not be retried")
	assert.Equal(t, int64(len(large)), received.Load(), "the single attempt must carry the whole body")
}

func TestRetry_ConnectionErrorIsRetried(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			conn, _, err := http.NewResponseController(writer).Hijack()
			if err == nil {
				_ = conn.Close()
			}

			return
		}

		_, _ = io.WriteString(writer, "ok")
	}))
	t.Cleanup(backend.Close)

	rec := serve(retryHandler(t, backend.URL, &proxy.RouteRetry{Attempts: 1}, nil), http.MethodPost, "x")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int32(2), hits.Load())
}

func TestRetry_BackendTimeoutIsRetried(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if hits.Add(1) == 1 {
			select {
			case <-req.Context().Done():
			case <-time.After(2 * time.Second):
			}

			return
		}

		_, _ = io.WriteString(writer, "ok")
	}))
	t.Cleanup(backend.Close)

	start := time.Now()
	rec := serve(retryHandler(t, backend.URL,
		&proxy.RouteRetry{Attempts: 1},
		&proxy.RouteTimeouts{Request: 5 * time.Second, Backend: 100 * time.Millisecond},
	), http.MethodGet, "")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int32(2), hits.Load())
	assert.Less(t, time.Since(start), time.Second, "the stalled attempt must be cut at the backend timeout")
}

func TestRetry_RequestTimeoutBoundsAllAttempts(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(backend.Close)

	start := time.Now()
	rec := serve(retryHandler(t, backend.URL,
		&proxy.RouteRetry{Codes: []int{500}, Attempts: 100},
		&proxy.RouteTimeouts{Request: 300 * time.Millisecond},
	), http.MethodGet, "")

	assert.Equal(t, http.StatusGatewayTimeout, rec.Code, "an expired request budget is a timeout, not the last 500")
	assert.Less(t, time.Since(start), time.Second)
}

func TestRetry_BackoffSeparatesAttempts(t *testing.T) {
	t.Parallel()

	backend, hits := failingBackend(t, 1, http.StatusServiceUnavailable)

	start := time.Now()
	rec := serve(retryHandler(t, backend.URL, &proxy.RouteRetry{Codes: []int{503}, Attempts: 1, Backoff: 200 * time.Millisecond}, nil), http.MethodGet, "")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int32(2), hits.Load())
	assert.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond)
}

func TestRetry_ClientCancelIsNotRetried(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	ctx, cancel := context.WithCancel(context.Background())

	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		cancel()
		<-req.Context().Done()
	}))
	t.Cleanup(backend.Close)

	handler := retryHandler(t, backend.URL, &proxy.RouteRetry{Attempts: 3}, nil)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "http://app.example.com/", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, int32(1), hits.Load())
}

// TestRetry_StreamingSurvivesRequestTimeout pins that the request budget
// stops once the final response headers arrive: with a retry policy the
// request timeout is enforced by the retry budget rather than the
// transport's header deadline, and it must not cut the streaming body.
func TestRetry_StreamingSurvivesRequestTimeout(t *testing.T) {
	t.Parallel()

	rec := serve(retryHandler(t, sseBackend(t).URL,
		&proxy.RouteRetry{Codes: []int{500}, Attempts: 1},
		&proxy.RouteTimeouts{Request: 200 * time.Millisecond},
	), http.MethodGet, "")

	assert.Equal(t, http.StatusOK, rec.Code)

	for idx := range sseFrames {
		assert.Contains(t, rec.Body.String(), fmt.Sprintf("data: event-%d", idx))
	}
}

func TestRetry_TunnelMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		backend func(t *testing.T) string
		// contentLength, when set, replaces the request body with an empty
		// reader that is not http.NoBody, the shape the HTTP/2 server hands
		// over: 0 when the edge closed the stream with the headers, -1 when it
		// left the stream open.
		contentLength *int64
		check         func(t *testing.T, fake *fakeCloudflaredRespWriter)
	}{
		{
			name: "retried response reaches the writer",
			backend: func(t *testing.T) string {
				t.Helper()

				backend, _ := failingBackend(t, 1, http.StatusInternalServerError)

				return backend.URL
			},
			check: func(t *testing.T, fake *fakeCloudflaredRespWriter) {
				t.Helper()
				assert.Equal(t, http.StatusOK, fake.Status())
				assert.Equal(t, "ok", string(fake.Body()))
			},
		},
		{
			name: "HTTP/2 request closed with its headers is retried",
			backend: func(t *testing.T) string {
				t.Helper()

				backend, _ := failingBackend(t, 1, http.StatusInternalServerError)

				return backend.URL
			},
			contentLength: new(int64(0)),
			check: func(t *testing.T, fake *fakeCloudflaredRespWriter) {
				t.Helper()
				assert.Equal(t, http.StatusOK, fake.Status())
				assert.Equal(t, "ok", string(fake.Body()))
			},
		},
		{
			name: "HTTP/2 request whose open stream ends without data is retried",
			backend: func(t *testing.T) string {
				t.Helper()

				backend, _ := failingBackend(t, 1, http.StatusInternalServerError)

				return backend.URL
			},
			contentLength: new(int64(-1)),
			check: func(t *testing.T, fake *fakeCloudflaredRespWriter) {
				t.Helper()
				assert.Equal(t, http.StatusOK, fake.Status())
				assert.Equal(t, "ok", string(fake.Body()))
			},
		},
		{
			name: "streaming body survives the request budget",
			backend: func(t *testing.T) string {
				t.Helper()

				return sseBackend(t).URL
			},
			check: func(t *testing.T, fake *fakeCloudflaredRespWriter) {
				t.Helper()
				assert.Equal(t, http.StatusOK, fake.Status())

				for idx := range sseFrames {
					assert.Contains(t, string(fake.Body()), fmt.Sprintf("data: event-%d", idx))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handler := retryHandler(t, tt.backend(t),
				&proxy.RouteRetry{Codes: []int{500}, Attempts: 1},
				&proxy.RouteTimeouts{Request: 200 * time.Millisecond},
			)

			fake := newFakeCloudflaredRespWriter()
			t.Cleanup(func() {
				_ = fake.serverSide.Close()
				_ = fake.clientSide.Close()
			})

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
			if tt.contentLength != nil {
				req.Body = io.NopCloser(strings.NewReader(""))
				req.ContentLength = *tt.contentLength
			}

			handler.ServeHTTP(fake, req)

			tt.check(t, fake)
		})
	}
}

const sseFrames = 3

// sseBackend flushes headers at once, then emits sseFrames events 200ms apart,
// so the body outlives a 200ms request timeout.
func sseBackend(t *testing.T) *httptest.Server {
	t.Helper()

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)

		flusher, _ := writer.(http.Flusher)
		flusher.Flush()

		for idx := range sseFrames {
			fmt.Fprintf(writer, "data: event-%d\n\n", idx)
			flusher.Flush()
			time.Sleep(200 * time.Millisecond)
		}
	}))
	t.Cleanup(backend.Close)

	return backend
}

func TestRetry_WebSocketUpgradeIsNotRetried(t *testing.T) {
	t.Parallel()

	backend, hits := failingBackend(t, 1, http.StatusInternalServerError)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Matches:  []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
			Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1, Protocol: proxy.BackendProtocolHTTP, WebSocket: true}},
			Retry:    &proxy.RouteRetry{Codes: []int{500}, Attempts: 2},
		}},
	}))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

	proxy.NewHandler(router).ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, int32(1), hits.Load(), "an upgrade is one handshake, never replayed")
}

// TestRetry_UnknownLengthBodyStreamsWithoutWaiting pins that a body of
// unknown length is forwarded as it arrives rather than buffered up front:
// the backend must see the first chunk while the client is still sending, and
// an attempt that failed before the upload finished is not replayed.
func TestRetry_UnknownLengthBodyStreamsWithoutWaiting(t *testing.T) {
	t.Parallel()

	firstChunk := make(chan string, 1)

	var hits atomic.Int32

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		hits.Add(1)

		buf := make([]byte, 5)
		_, _ = io.ReadFull(req.Body, buf)
		firstChunk <- string(buf)

		// Without it the server would read the rest of the unfinished upload
		// before ending the response.
		writer.Header().Set("Connection", "close")
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(backend.Close)

	handler := retryHandler(t, backend.URL, &proxy.RouteRetry{Codes: []int{500}, Attempts: 2}, nil)

	bodyReader, bodyWriter := io.Pipe()
	t.Cleanup(func() { _ = bodyWriter.Close() })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://app.example.com/", bodyReader)
	req.ContentLength = -1

	done := make(chan *httptest.ResponseRecorder)

	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		done <- rec
	}()

	_, _ = bodyWriter.Write([]byte("hello"))

	select {
	case got := <-firstChunk:
		assert.Equal(t, "hello", got)
	case <-time.After(2 * time.Second):
		t.Fatal("the backend never saw the first chunk: the body was held back")
	}

	select {
	case rec := <-done:
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Equal(t, int32(1), hits.Load(), "an upload the client is still sending cannot be replayed")
	case <-time.After(2 * time.Second):
		t.Fatal("the response waited for the upload to finish")
	}
}

func TestRetry_ExhaustedConnectionErrorsAnswer502(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		hits.Add(1)

		conn, _, err := http.NewResponseController(writer).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(backend.Close)

	rec := serve(retryHandler(t, backend.URL, &proxy.RouteRetry{Attempts: 1}, nil), http.MethodPost, "x")

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Equal(t, int32(2), hits.Load())
}

// TestRetry_StalledDiscardedBodyDoesNotBlockRetry pins that a retriable
// response whose body never finishes is closed, not drained, so the next
// attempt is not held hostage by it. Both a body of unknown length and one
// that declares a small Content-Length and then stalls are covered.
func TestRetry_StalledDiscardedBodyDoesNotBlockRetry(t *testing.T) {
	t.Parallel()

	for _, declaredLength := range []string{"", "100"} {
		t.Run("content-length="+declaredLength, func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int32

			release := make(chan struct{})

			backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
				if hits.Add(1) == 1 {
					if declaredLength != "" {
						writer.Header().Set("Content-Length", declaredLength)
					}

					writer.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(writer, "partial")
					http.NewResponseController(writer).Flush()

					select {
					case <-release:
					case <-req.Context().Done():
					}

					return
				}

				_, _ = io.WriteString(writer, "ok")
			}))
			t.Cleanup(backend.Close)
			// Registered after backend.Close so it runs first: the stalled
			// handler must return before Close can finish.
			t.Cleanup(func() { close(release) })

			done := make(chan *httptest.ResponseRecorder, 1)

			go func() {
				done <- serve(retryHandler(t, backend.URL, &proxy.RouteRetry{Codes: []int{503}, Attempts: 1}, nil), http.MethodGet, "")
			}()

			select {
			case rec := <-done:
				assert.Equal(t, http.StatusOK, rec.Code)
				assert.Equal(t, int32(2), hits.Load())
			case <-time.After(2 * time.Second):
				t.Fatal("the retry waited on the discarded response body")
			}
		})
	}
}

func TestRetry_RequestTimeoutDuringBackoff(t *testing.T) {
	t.Parallel()

	backend, hits := failingBackend(t, 10, http.StatusInternalServerError)

	start := time.Now()
	rec := serve(retryHandler(t, backend.URL,
		&proxy.RouteRetry{Codes: []int{500}, Attempts: 3, Backoff: 5 * time.Second},
		&proxy.RouteTimeouts{Request: 200 * time.Millisecond},
	), http.MethodGet, "")

	assert.Equal(t, http.StatusGatewayTimeout, rec.Code)
	assert.Equal(t, int32(1), hits.Load(), "the budget ran out during the first backoff")
	assert.Less(t, time.Since(start), time.Second)
}

func TestRetry_EveryAttemptTimesOut(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		<-req.Context().Done()
	}))
	t.Cleanup(backend.Close)

	rec := serve(retryHandler(t, backend.URL,
		&proxy.RouteRetry{Attempts: 2},
		&proxy.RouteTimeouts{Backend: 50 * time.Millisecond},
	), http.MethodGet, "")

	assert.Equal(t, http.StatusGatewayTimeout, rec.Code)
	assert.Equal(t, int32(3), hits.Load())
}

func TestRetry_UnknownLengthSmallBodyIsReplayed(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		bodies []string
		hits   atomic.Int32
	)

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		data, _ := io.ReadAll(req.Body)

		mu.Lock()
		bodies = append(bodies, string(data))
		mu.Unlock()

		if hits.Add(1) == 1 {
			writer.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(backend.Close)

	handler := retryHandler(t, backend.URL, &proxy.RouteRetry{Codes: []int{500}, Attempts: 1}, nil)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://app.example.com/", io.NopCloser(strings.NewReader("payload")))
	req.ContentLength = -1

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"payload", "payload"}, bodies)
}

// TestRetry_AttemptsCappedInProxy pins the proxy-side cap: a pushed config
// asking for more retries than MaxRetryAttempts still stops at the cap.
func TestRetry_AttemptsCappedInProxy(t *testing.T) {
	t.Parallel()

	backend, hits := failingBackend(t, 1000, http.StatusServiceUnavailable)

	rec := serve(retryHandler(t, backend.URL, &proxy.RouteRetry{Codes: []int{503}, Attempts: 1000}, nil), http.MethodGet, "")

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, int32(proxy.MaxRetryAttempts+1), hits.Load())
}

// TestRetry_ZeroBackoffStillWaitsTheFloor pins that a zero backoff does not
// mean back-to-back attempts.
func TestRetry_ZeroBackoffStillWaitsTheFloor(t *testing.T) {
	t.Parallel()

	backend, hits := failingBackend(t, 3, http.StatusServiceUnavailable)

	start := time.Now()
	rec := serve(retryHandler(t, backend.URL, &proxy.RouteRetry{Codes: []int{503}, Attempts: 3}, nil), http.MethodGet, "")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int32(4), hits.Load())
	assert.GreaterOrEqual(t, time.Since(start), 3*proxy.MinRetryBackoff)
}

// TestRetry_FullDuplexUploadSurvivesEarlyAnswer pins that an upload the
// backend answers before it ends, with a retriable status the proxy cannot
// act on, is passed through whole: refusing the retry must not cut the
// attempt that is still streaming in both directions.
func TestRetry_FullDuplexUploadSurvivesEarlyAnswer(t *testing.T) {
	t.Parallel()

	const size = 4 << 20

	var received atomic.Int64

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		controller := http.NewResponseController(writer)
		_ = controller.EnableFullDuplex()

		writer.WriteHeader(http.StatusServiceUnavailable)
		_ = controller.Flush()

		n, _ := io.Copy(writer, req.Body)
		received.Store(n)
	}))
	t.Cleanup(backend.Close)

	handler := retryHandler(t, backend.URL, &proxy.RouteRetry{Codes: []int{503}, Attempts: 1}, nil)

	bodyReader, bodyWriter := io.Pipe()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://app.example.com/", bodyReader)
	req.ContentLength = -1

	go func() {
		chunk := []byte(strings.Repeat("x", 64<<10))
		for range size / len(chunk) {
			_, _ = bodyWriter.Write(chunk)
		}

		_ = bodyWriter.Close()
	}()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, int64(size), received.Load(), "the backend must receive the whole upload")
	assert.Equal(t, size, rec.Body.Len(), "the echoed response must reach the client whole")
}

// TestRetry_MislengthBodyFailsBeforeAnyAttempt pins that a body whose length
// differs from its declared Content-Length is refused up front: every attempt
// would fail the same length check, so retrying it only multiplies backend
// load.
func TestRetry_MislengthBodyFailsBeforeAnyAttempt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		contentLength int64
	}{
		{name: "body longer than declared", contentLength: 3},
		{name: "body shorter than declared", contentLength: 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// The transport fails the length check after sending the request
			// head, often before the backend dispatches it, so attempts are
			// counted as connections rather than handler hits.
			var conns atomic.Int32

			backend := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
				_, _ = io.Copy(io.Discard, req.Body)
				writer.WriteHeader(http.StatusInternalServerError)
			}))
			backend.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					conns.Add(1)
				}
			}
			backend.Start()
			t.Cleanup(backend.Close)

			handler := retryHandler(t, backend.URL, &proxy.RouteRetry{Codes: []int{500}, Attempts: 2}, nil)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://app.example.com/", strings.NewReader("payload"))
			req.ContentLength = tt.contentLength

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusBadGateway, rec.Code)
			assert.Zero(t, conns.Load(), "a body that does not match its length must not reach the backend")
		})
	}
}

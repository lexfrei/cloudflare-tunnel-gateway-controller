package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplayBody_Next(t *testing.T) {
	t.Parallel()

	t.Run("a body read to its end is replayed", func(t *testing.T) {
		t.Parallel()

		recorder := &replayBody{src: io.NopCloser(strings.NewReader("payload"))}
		first := recorder.attempt()

		data, err := io.ReadAll(first)
		require.NoError(t, err)
		assert.Equal(t, "payload", string(data))

		next, ok := recorder.next(t.Context())
		require.True(t, ok)

		replayed, err := io.ReadAll(next)
		require.NoError(t, err)
		assert.Equal(t, "payload", string(replayed))

		_, err = first.Read(make([]byte, 1))
		require.ErrorIs(t, err, errAttemptAbandoned, "the abandoned attempt must not read the source again")
	})

	t.Run("an unread source is handed on", func(t *testing.T) {
		t.Parallel()

		recorder := &replayBody{src: io.NopCloser(strings.NewReader("payload"))}
		first := recorder.attempt()

		next, ok := recorder.next(t.Context())
		require.True(t, ok)

		_, err := first.Read(make([]byte, 1))
		require.ErrorIs(t, err, errAttemptAbandoned)

		data, err := io.ReadAll(next)
		require.NoError(t, err)
		assert.Equal(t, "payload", string(data))
	})

	t.Run("a partly read source is not resent and stays with its attempt", func(t *testing.T) {
		t.Parallel()

		recorder := &replayBody{src: io.NopCloser(strings.NewReader("payload"))}
		first := recorder.attempt()

		_, err := first.Read(make([]byte, 3))
		require.NoError(t, err)

		_, ok := recorder.next(t.Context())
		assert.False(t, ok)

		rest, err := io.ReadAll(first)
		require.NoError(t, err, "a refused hand-off must not cut the attempt that is still sending")
		assert.Equal(t, "load", string(rest))
	})

	t.Run("a body over the limit is not resent", func(t *testing.T) {
		t.Parallel()

		recorder := &replayBody{src: io.NopCloser(strings.NewReader(strings.Repeat("x", maxRetryBodyBytes+1)))}
		_, err := io.ReadAll(recorder.attempt())
		require.NoError(t, err)

		_, ok := recorder.next(t.Context())
		assert.False(t, ok)
	})

	t.Run("a partly read source is refused without waiting for a read in progress", func(t *testing.T) {
		t.Parallel()

		src, writer := io.Pipe()
		t.Cleanup(func() { _ = writer.Close() })

		recorder := &replayBody{src: src}
		first := recorder.attempt()

		go func() { _, _ = writer.Write([]byte("abc")) }()

		_, err := first.Read(make([]byte, 3))
		require.NoError(t, err)

		go func() { _, _ = first.Read(make([]byte, 1)) }()

		waitReading(t, recorder)

		_, ok := recorder.next(t.Context())
		assert.False(t, ok)
	})
}

// TestReplayBody_NextWaitsForReadInProgress covers a hand-off while the
// attempt's transport is still reading a body nothing has been read from yet,
// as an HTTP/1 transport does when it probes a bodyless-method request whose
// stream is still open.
func TestReplayBody_NextWaitsForReadInProgress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// settle ends the read in progress, or the wait.
		settle   func(writer *io.PipeWriter, cancel context.CancelFunc)
		wantNext bool
	}{
		{
			name:     "the stream ends without data: replayed",
			settle:   func(writer *io.PipeWriter, _ context.CancelFunc) { _ = writer.Close() },
			wantNext: true,
		},
		{
			name:   "data arrives: refused",
			settle: func(writer *io.PipeWriter, _ context.CancelFunc) { _, _ = writer.Write([]byte("x")) },
		},
		{
			name:   "the read fails: refused",
			settle: func(writer *io.PipeWriter, _ context.CancelFunc) { _ = writer.CloseWithError(errFakeReset) },
		},
		{
			name:   "the request ends first: refused",
			settle: func(_ *io.PipeWriter, cancel context.CancelFunc) { cancel() },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src, writer := io.Pipe()
			t.Cleanup(func() { _ = writer.Close() })

			recorder := &replayBody{src: src, probed: true}
			first := recorder.attempt()

			go func() { _, _ = first.Read(make([]byte, 1)) }()

			waitReading(t, recorder)

			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)

			type result struct {
				body io.ReadCloser
				ok   bool
			}

			done := make(chan result, 1)

			go func() {
				body, ok := recorder.next(ctx)
				done <- result{body: body, ok: ok}
			}()

			select {
			case <-done:
				t.Fatal("the hand-off was decided while the read was still in progress")
			case <-time.After(50 * time.Millisecond):
			}

			tt.settle(writer, cancel)

			select {
			case got := <-done:
				require.Equal(t, tt.wantNext, got.ok)

				if got.ok {
					data, err := io.ReadAll(got.body)
					require.NoError(t, err)
					assert.Empty(t, data)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the hand-off kept waiting after the read ended")
			}
		})
	}
}

func waitReading(t *testing.T, recorder *replayBody) {
	t.Helper()

	require.Eventually(t, func() bool {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()

		return recorder.reading > 0
	}, time.Second, time.Millisecond)
}

var (
	errNoRecordedBody = errors.New("first attempt must carry the recorded body")
	errFakeReset      = errors.New("connection reset by peer")
)

// probeThenFail is a transport whose first attempt fails while a read of the
// body it started is still waiting, as the HTTP/1 transport does when writing
// the head to a stale keep-alive connection fails during its body probe.
type probeThenFail struct {
	calls atomic.Int32
}

func (p *probeThenFail) RoundTrip(req *http.Request) (*http.Response, error) {
	if p.calls.Add(1) > 1 {
		_, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("reading replayed body: %w", err)
		}

		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
	}

	body, ok := req.Body.(*attemptBody)
	if !ok {
		return nil, errNoRecordedBody
	}

	go func() { _, _ = body.Read(make([]byte, 1)) }()

	for {
		body.body.mu.Lock()
		reading := body.body.reading
		body.body.mu.Unlock()

		if reading > 0 {
			return nil, errFakeReset
		}

		time.Sleep(time.Millisecond)
	}
}

func TestRetryTransport_RetriesOnceAProbeReadEndsEmpty(t *testing.T) {
	t.Parallel()

	src, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://backend.example.com/", src)
	req.ContentLength = -1

	next := &probeThenFail{}
	transport := &retryTransport{next: next, policy: &RouteRetry{Attempts: 1}}

	go func() {
		time.Sleep(50 * time.Millisecond)

		_ = writer.Close()
	}()

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)

	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(2), next.calls.Load())
}

// TestRetryTransport_ReadAnswerToOpenUploadClosesPromptly covers an HTTP/2
// response body whose Close waits for the request stream, while that
// stream's writer is parked reading an upload the client has not finished.
// Cancelling the attempt context first is what releases it.
func TestRetryTransport_ReadAnswerToOpenUploadClosesPromptly(t *testing.T) {
	t.Parallel()

	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "answer")
	}))
	backend.EnableHTTP2 = true
	backend.StartTLS()
	t.Cleanup(backend.Close)

	src, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, backend.URL, src)
	req.ContentLength = -1

	transport := &retryTransport{
		next:   backend.Client().Transport,
		policy: &RouteRetry{Attempts: 1, Codes: []int{http.StatusServiceUnavailable}},
	}

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "answer", string(body))

	// Ending the upload is the only thing that frees a stuck Close, so the
	// watchdog does that and records that it had to.
	var stuck atomic.Bool

	watchdog := time.AfterFunc(2*time.Second, func() {
		stuck.Store(true)
		_ = writer.Close()
	})

	_ = resp.Body.Close()

	watchdog.Stop()
	assert.False(t, stuck.Load(), "closing the answer waited for the client's body")
}

var errHookRefused = errors.New("refused")

// TestRetryTransport_RefusedInformationalDiscardsPromptly covers the same
// stuck HTTP/2 Close on the path that throws the answer away because the
// client's 1xx hook refused a held informational response.
func TestRetryTransport_RefusedInformationalDiscardsPromptly(t *testing.T) {
	t.Parallel()

	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusEarlyHints)
		_, _ = io.WriteString(writer, "answer")
	}))
	backend.EnableHTTP2 = true
	backend.StartTLS()
	t.Cleanup(backend.Close)

	src, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })

	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
		Got1xxResponse: func(int, textproto.MIMEHeader) error { return errHookRefused },
	})
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, backend.URL, src)
	req.ContentLength = -1

	transport := &retryTransport{
		next:   backend.Client().Transport,
		policy: &RouteRetry{Attempts: 1, Codes: []int{http.StatusServiceUnavailable}},
	}

	var stuck atomic.Bool

	watchdog := time.AfterFunc(2*time.Second, func() {
		stuck.Store(true)
		_ = writer.Close()
	})

	resp, err := transport.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}

	watchdog.Stop()
	require.Error(t, err)
	assert.False(t, stuck.Load(), "discarding the answer waited for the client's body")
}

// TestRetryTransport_EarlyAnswerToOpenUploadIsNotHeldBack covers the case
// the probe wait must not reach: a backend that answers a POST before reading
// its body, over HTTP/2 where the transport returns while its body read
// still waits. A full-duplex client that sends nothing until it sees the
// response would otherwise never get the 503.
func TestRetryTransport_EarlyAnswerToOpenUploadIsNotHeldBack(t *testing.T) {
	t.Parallel()

	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	backend.EnableHTTP2 = true
	backend.StartTLS()
	t.Cleanup(backend.Close)

	src, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, backend.URL, src)
	req.ContentLength = -1

	transport := &retryTransport{
		next:   backend.Client().Transport,
		policy: &RouteRetry{Attempts: 1, Codes: []int{http.StatusServiceUnavailable}},
	}

	done := make(chan int, 1)

	go func() {
		resp, err := transport.RoundTrip(req)
		if err == nil {
			_ = resp.Body.Close()
			done <- resp.StatusCode
		}
	}()

	select {
	case status := <-done:
		assert.Equal(t, http.StatusServiceUnavailable, status)
	case <-time.After(2 * time.Second):
		t.Fatal("the backend's answer waited for the client's body")
	}
}

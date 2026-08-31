package proxy

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// pipeWebSocketPanicTimeout bounds each case. A goroutine that neither returns
// nor crashes would otherwise hang the suite instead of failing it.
const pipeWebSocketPanicTimeout = 5 * time.Second

// panicOnRead is a net.Conn whose Read panics. Close and the rest delegate, so
// the deferred cleanup inside pipeWebSocket behaves normally.
type panicOnRead struct {
	net.Conn
}

func (panicOnRead) Read([]byte) (int, error) {
	panic("websocket read exploded")
}

// hijackToConn is a ResponseWriter that hands out a caller-supplied conn.
type hijackToConn struct {
	*httptest.ResponseRecorder

	conn net.Conn
	buf  *bufio.ReadWriter
}

func (h *hijackToConn) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, h.buf, nil
}

func newHijackToConn(conn net.Conn) *hijackToConn {
	return &hijackToConn{
		ResponseRecorder: httptest.NewRecorder(),
		conn:             conn,
		buf:              bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn)),
	}
}

// runPipeWebSocket runs pipeWebSocket and reports whether it returned in time.
func runPipeWebSocket(t *testing.T, w http.ResponseWriter, backendConn net.Conn, backendReader *bufio.Reader) bool {
	t.Helper()

	done := make(chan struct{})

	go func() {
		defer close(done)

		pipeWebSocket(w, backendConn, backendReader, http.Header{})
	}()

	select {
	case <-done:
		return true
	case <-time.After(pipeWebSocketPanicTimeout):
		return false
	}
}

// These cases deliberately do not use fakeCloudflaredRespWriter, which
// docs/development/proxy-architecture.md mandates for response-touching proxy
// code. The guarded code runs entirely on the already-hijacked net.Conn and
// never touches the ResponseWriter again, and the fake cannot hand out a conn
// that panics on Read, which is the whole fixture here.

// TestPipeWebSocket_ContainsClientSidePanic pins panic containment in the
// client-to-backend copy goroutine.
//
// A panic in a goroutine cannot be recovered by whoever started it, so an
// unguarded copy takes the whole proxy process down and every other tenant's
// connection with it. That happens on any transport: the per-stream recover in
// x/net/http2 only covers the handler goroutine, not the ones it spawns.
func TestPipeWebSocket_ContainsClientSidePanic(t *testing.T) {
	t.Parallel()

	clientEnd, clientPeer := net.Pipe()
	t.Cleanup(func() { _ = clientPeer.Close() })

	backendEnd, backendPeer := net.Pipe()
	t.Cleanup(func() { _ = backendEnd.Close() })
	t.Cleanup(func() { _ = backendPeer.Close() })

	writer := newHijackToConn(panicOnRead{Conn: clientEnd})

	require.True(t, runPipeWebSocket(t, writer, backendEnd, bufio.NewReader(backendPeer)),
		"a contained panic must end the session instead of hanging it")
}

// TestPipeWebSocket_ContainsBackendSidePanic pins the same contract on the
// backend-to-client copy, which reads through a bufio.Reader the caller owns.
func TestPipeWebSocket_ContainsBackendSidePanic(t *testing.T) {
	t.Parallel()

	clientEnd, clientPeer := net.Pipe()
	t.Cleanup(func() { _ = clientPeer.Close() })

	backendEnd, backendPeer := net.Pipe()
	t.Cleanup(func() { _ = backendEnd.Close() })
	t.Cleanup(func() { _ = backendPeer.Close() })

	writer := newHijackToConn(clientEnd)

	require.True(t, runPipeWebSocket(t, writer, backendEnd, bufio.NewReader(panicOnRead{Conn: backendPeer})),
		"a contained panic must end the session instead of hanging it")
}

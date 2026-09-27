package proxy_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// statusLog records every status its WriteHeader receives.
type statusLog struct {
	header   http.Header
	statuses []int
}

func (s *statusLog) Header() http.Header { return s.header }

func (s *statusLog) Write(p []byte) (int, error) { return len(p), nil }

func (s *statusLog) WriteHeader(status int) { s.statuses = append(s.statuses, status) }

// TestCountingResponseWriter_InformationalStatusIsNotFinal pins that a 1xx
// relayed ahead of the real response, 103 Early Hints for example, is passed
// on but neither recorded as the status nor allowed to swallow the final one.
func TestCountingResponseWriter_InformationalStatusIsNotFinal(t *testing.T) {
	t.Parallel()

	inner := &statusLog{header: http.Header{}}
	counted := proxy.NewCountingResponseWriterForTest(inner)

	counted.WriteHeader(http.StatusEarlyHints)
	counted.WriteHeader(http.StatusNotFound)

	assert.Equal(t, []int{http.StatusEarlyHints, http.StatusNotFound}, inner.statuses)
	assert.Equal(t, http.StatusNotFound, counted.Status())
}

// TestCountingResponseWriter_SwitchingProtocolsIsFinal pins the exception: a
// 101 ends the HTTP exchange, so it is what the access log and metrics record.
func TestCountingResponseWriter_SwitchingProtocolsIsFinal(t *testing.T) {
	t.Parallel()

	inner := &statusLog{header: http.Header{}}
	counted := proxy.NewCountingResponseWriterForTest(inner)

	counted.WriteHeader(http.StatusSwitchingProtocols)
	counted.WriteHeader(http.StatusOK)

	assert.Equal(t, []int{http.StatusSwitchingProtocols}, inner.statuses)
	assert.Equal(t, http.StatusSwitchingProtocols, counted.Status())
}

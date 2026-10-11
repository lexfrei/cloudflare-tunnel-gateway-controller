//go:build e2e

package e2e

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

var errPollTransport = errors.New("EOF")

func TestPollOutcome_DescribesEachShape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		echo *echoResponse
		resp *echoHTTPResponse
		err  error
		want string
	}{
		{
			name: "transport error before any response",
			err:  errPollTransport,
			want: "request failed: EOF",
		},
		{
			name: "response that could not be read",
			resp: &echoHTTPResponse{StatusCode: http.StatusOK},
			err:  errPollTransport,
			want: "status 200, then: EOF",
		},
		{
			name: "status without an echo body",
			echo: &echoResponse{},
			resp: &echoHTTPResponse{StatusCode: http.StatusNotFound},
			want: "status 404",
		},
		{
			name: "echo answered",
			echo: &echoResponse{Pod: "echo-v2-abc"},
			resp: &echoHTTPResponse{StatusCode: http.StatusOK},
			want: "status 200 from pod echo-v2-abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var outcome pollOutcome

			outcome.recordHTTP(tt.echo, tt.resp, tt.err)

			assert.Equal(t, "1 attempt(s), last: "+tt.want, outcome.String())
		})
	}
}

func TestPollOutcome_KeepsOnlyTheLastAttempt(t *testing.T) {
	t.Parallel()

	var outcome pollOutcome

	outcome.recordHTTP(nil, nil, errPollTransport)
	outcome.recordHTTP(&echoResponse{}, &echoHTTPResponse{StatusCode: http.StatusBadGateway}, nil)

	assert.Equal(t, "2 attempt(s), last: status 502", outcome.String())
}

func TestPollOutcome_NoAttempt(t *testing.T) {
	t.Parallel()

	var outcome pollOutcome

	assert.Equal(t, "no attempt completed", outcome.String())
}

func TestPollOutcome_AnnotateExtendsTheLastAttempt(t *testing.T) {
	t.Parallel()

	var outcome pollOutcome

	outcome.recordHTTP(&echoResponse{Pod: "echo-v1-abc"}, &echoHTTPResponse{StatusCode: http.StatusOK}, nil)
	outcome.annotate("path absent from echo-v3 logs")

	assert.Equal(t, "1 attempt(s), last: status 200 from pod echo-v1-abc; path absent from echo-v3 logs", outcome.String())
}

// An edge error page lands in makeRequest's parse error whole, and a
// timeout message carrying it would bury the line that matters.
func TestPollOutcome_TruncatesLongDescriptions(t *testing.T) {
	t.Parallel()

	var outcome pollOutcome

	outcome.record(strings.Repeat("x", maxOutcomeLen+1))

	assert.Equal(t, "1 attempt(s), last: "+strings.Repeat("x", maxOutcomeLen)+" (truncated)", outcome.String())

	outcome.record(strings.Repeat("y", maxOutcomeLen))

	assert.Equal(t, "2 attempt(s), last: "+strings.Repeat("y", maxOutcomeLen), outcome.String())
}

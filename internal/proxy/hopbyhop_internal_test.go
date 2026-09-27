package proxy

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRemoveHopByHopHeaders_FixedSet pins every header of the RFC 7230 set,
// checked on the header map itself: some of them never survive a real round
// trip, so an end-to-end test cannot tell whether this function removed them.
func TestRemoveHopByHopHeaders_FixedSet(t *testing.T) {
	t.Parallel()

	fixed := []string{
		"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "Trailer", "Transfer-Encoding", "Upgrade",
	}

	header := http.Header{}
	for _, name := range fixed {
		header.Set(name, "x")
	}

	header.Set("X-End-To-End", "kept")

	removeHopByHopHeaders(header)

	for _, name := range fixed {
		assert.Empty(t, header.Values(name), name)
	}

	assert.Equal(t, "kept", header.Get("X-End-To-End"))
}

// TestRemoveHopByHopHeaders_TE pins that TE is dropped except for the
// "trailers" token, which is all that is forwarded.
func TestRemoveHopByHopHeaders_TE(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		te   []string
		want []string
	}{
		{name: "trailers kept", te: []string{"trailers"}, want: []string{"trailers"}},
		{name: "other coding dropped", te: []string{"gzip"}, want: nil},
		{name: "trailers kept from a list", te: []string{"gzip, trailers"}, want: []string{"trailers"}},
		{name: "trailers kept across values", te: []string{"gzip", "Trailers"}, want: []string{"trailers"}},
		{name: "absent stays absent", te: nil, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			header := http.Header{}
			for _, value := range tt.te {
				header.Add(headerTE, value)
			}

			removeHopByHopHeaders(header)

			assert.Equal(t, tt.want, header.Values(headerTE))
		})
	}
}

// TestRemoveHopByHopHeaders_ConnectionNamed pins that every header the
// Connection header names goes too, across values and with stray spaces.
func TestRemoveHopByHopHeaders_ConnectionNamed(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	header.Add("Connection", "X-One , x-two")
	header.Add("Connection", "X-Three")
	header.Set("X-One", "1")
	header.Set("X-Two", "2")
	header.Set("X-Three", "3")
	header.Set("X-Four", "4")

	removeHopByHopHeaders(header)

	for _, name := range []string{"X-One", "X-Two", "X-Three"} {
		assert.Empty(t, header.Values(name), name)
	}

	assert.Equal(t, "4", header.Get("X-Four"))
}

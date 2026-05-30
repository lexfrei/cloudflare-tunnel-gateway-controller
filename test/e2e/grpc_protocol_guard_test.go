//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

func TestGRPCRequiresHTTP2SkipReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		protocol string
		wantSkip bool
	}{
		{name: "http2 runs", protocol: "http2", wantSkip: false},
		{name: "http2 mixed case runs", protocol: "HTTP2", wantSkip: false},
		{name: "http2 padded runs", protocol: " http2 ", wantSkip: false},
		{name: "quic skips", protocol: "quic", wantSkip: true},
		{name: "auto skips", protocol: "auto", wantSkip: true},
		{name: "empty skips", protocol: "", wantSkip: true},
		{name: "typo skips", protocol: "quik", wantSkip: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reason := grpcRequiresHTTP2SkipReason(tt.protocol)

			if tt.wantSkip {
				if reason == "" {
					t.Fatalf("protocol %q: want a non-empty skip reason, got empty", tt.protocol)
				}

				// The message must name the fix so an operator can act on it
				// without reading source.
				if !strings.Contains(reason, "http2") {
					t.Errorf("protocol %q: skip reason should mention http2, got %q", tt.protocol, reason)
				}
			} else if reason != "" {
				t.Fatalf("protocol %q: want run (empty reason), got skip %q", tt.protocol, reason)
			}
		})
	}
}

package metrics

import (
	"errors"
	"net/http"
	"testing"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/stretchr/testify/assert"
)

// Test error definitions for error classification tests.
var (
	errContextDeadline   = errors.New("context deadline exceeded")
	errRequestTimeout    = errors.New("request timeout")
	errConnectionRefused = errors.New("dial tcp: connection refused")
	errNoSuchHost        = errors.New("no such host")
	errRandomError       = errors.New("some random error")
	errWrapper           = errors.New("wrapper")
)

func TestClassifyCloudflareError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected string
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: "",
		},
		{
			name:     "auth error 401",
			err:      &cloudflare.Error{StatusCode: http.StatusUnauthorized},
			expected: "auth",
		},
		{
			name:     "auth error 403",
			err:      &cloudflare.Error{StatusCode: http.StatusForbidden},
			expected: "auth",
		},
		{
			name:     "rate limit error",
			err:      &cloudflare.Error{StatusCode: http.StatusTooManyRequests},
			expected: "rate_limit",
		},
		{
			name:     "server error 500",
			err:      &cloudflare.Error{StatusCode: http.StatusInternalServerError},
			expected: "server_error",
		},
		{
			name:     "server error 503",
			err:      &cloudflare.Error{StatusCode: http.StatusServiceUnavailable},
			expected: "server_error",
		},
		{
			name:     "client error 400",
			err:      &cloudflare.Error{StatusCode: http.StatusBadRequest},
			expected: "client_error",
		},
		{
			name:     "client error 404",
			err:      &cloudflare.Error{StatusCode: http.StatusNotFound},
			expected: "client_error",
		},
		{
			name:     "timeout error",
			err:      errContextDeadline,
			expected: "timeout",
		},
		{
			name:     "timeout error variant",
			err:      errRequestTimeout,
			expected: "timeout",
		},
		{
			name:     "network error connection refused",
			err:      errConnectionRefused,
			expected: "network",
		},
		{
			name:     "network error no such host",
			err:      errNoSuchHost,
			expected: "network",
		},
		{
			name:     "unknown error",
			err:      errRandomError,
			expected: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := ClassifyCloudflareError(tt.err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestClassifyCloudflareErrorWrapped(t *testing.T) {
	t.Parallel()

	apiErr := &cloudflare.Error{StatusCode: http.StatusUnauthorized}
	wrappedErr := errors.Join(errWrapper, apiErr)

	result := ClassifyCloudflareError(wrappedErr)
	assert.Equal(t, "auth", result)
}

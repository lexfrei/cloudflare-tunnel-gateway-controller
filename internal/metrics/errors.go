package metrics

import (
	"errors"
	"net/http"
	"strings"

	"github.com/cloudflare/cloudflare-go/v6"
)

// Error type constants for metrics labels.
const (
	ErrorTypeAuth        = "auth"
	ErrorTypeRateLimit   = "rate_limit"
	ErrorTypeServerError = "server_error"
	ErrorTypeClientError = "client_error"
	ErrorTypeTimeout     = "timeout"
	ErrorTypeNetwork     = "network"
	ErrorTypeUnknown     = "unknown"
)

// ClassifyCloudflareError classifies an error from Cloudflare API for metrics labeling.
// Returns an empty string for nil errors.
func ClassifyCloudflareError(err error) string {
	if err == nil {
		return ""
	}

	// Check for typed errors from cloudflare-go SDK
	// cloudflare.Error is an alias to apierror.Error in cloudflare-go v6
	var apiErr *cloudflare.Error
	if errors.As(err, &apiErr) {
		return classifyByStatusCode(apiErr.StatusCode)
	}

	// Fallback for non-API errors based on error message
	return classifyByErrorMessage(err.Error())
}

func classifyByStatusCode(statusCode int) string {
	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return ErrorTypeAuth
	case statusCode == http.StatusTooManyRequests:
		return ErrorTypeRateLimit
	case statusCode >= http.StatusInternalServerError && statusCode < 600:
		return ErrorTypeServerError
	case statusCode >= http.StatusBadRequest && statusCode < http.StatusInternalServerError:
		return ErrorTypeClientError
	default:
		return ErrorTypeUnknown
	}
}

func classifyByErrorMessage(errStr string) string {
	errLower := strings.ToLower(errStr)

	switch {
	case strings.Contains(errLower, "timeout") || strings.Contains(errLower, "deadline"):
		return ErrorTypeTimeout
	case strings.Contains(errLower, "connection refused") || strings.Contains(errLower, "no such host"):
		return ErrorTypeNetwork
	default:
		return ErrorTypeUnknown
	}
}

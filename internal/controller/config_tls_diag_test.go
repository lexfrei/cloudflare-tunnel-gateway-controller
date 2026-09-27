package controller

import (
	"crypto/tls"
	"crypto/x509"
	stderrors "errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
)

// TestDescribeConfigTLSError pins the operator-facing reason for each way the
// config API handshake can fail, through the wrapping a real push adds.
func TestDescribeConfigTLSError(t *testing.T) {
	t.Parallel()

	wrap := func(err error) error {
		return fmt.Errorf("failed to push config to 1/1 endpoints: %w",
			stderrors.Join(fmt.Errorf("send request: %w", &url.Error{Op: "Put", URL: "https://plane", Err: err})))
	}

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "foreign CA",
			err:  wrap(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}),
			want: "not issued by this controller",
		},
		{
			name: "wrong plane",
			err:  wrap(&tls.CertificateVerificationError{Err: x509.HostnameError{Host: "a"}}),
			want: "names a different data plane",
		},
		{
			name: "expired",
			err:  wrap(&tls.CertificateVerificationError{Err: x509.CertificateInvalidError{Reason: x509.Expired}}),
			want: "not valid",
		},
		{
			name: "plaintext server",
			err:  wrap(tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}),
			want: "does not speak TLS",
		},
		{
			name: "plaintext endpoint",
			err:  wrap(ErrPlaintextConfigEndpoint),
			want: "plaintext",
		},
		{name: "unrelated", err: wrap(errors.New("connection refused")), want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := describeConfigTLSError(tt.err)
			if tt.want == "" {
				assert.Empty(t, got)

				return
			}

			assert.Contains(t, got, tt.want)
		})
	}
}

// TestProxyPushFailureMessage_NamesTheTLSReason pins that the route status of
// a plane whose handshake fails says why, not only that the push failed.
func TestProxyPushFailureMessage_NamesTheTLSReason(t *testing.T) {
	t.Parallel()

	tlsFailure := fmt.Errorf("send request: %w",
		&url.Error{Op: "Put", URL: "https://plane", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}})

	assert.Contains(t, proxyPushFailureMessage("tenant-a/edge", tlsFailure), "not issued by this controller")
	assert.NotContains(t, proxyPushFailureMessage("tenant-a/edge", errors.New("connection refused")), "certificate")
}

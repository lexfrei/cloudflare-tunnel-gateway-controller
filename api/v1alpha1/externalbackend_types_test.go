package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExternalBackendSpec_URL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec ExternalBackendSpec
		want string
	}{
		{
			name: "https with default-ish port",
			spec: ExternalBackendSpec{Scheme: ExternalBackendSchemeHTTPS, Host: "api.example.com", Port: 443},
			want: "https://api.example.com:443",
		},
		{
			name: "http with custom port",
			spec: ExternalBackendSpec{Scheme: ExternalBackendSchemeHTTP, Host: "api.example.com", Port: 8080},
			want: "http://api.example.com:8080",
		},
		{
			name: "with base path",
			spec: ExternalBackendSpec{Scheme: ExternalBackendSchemeHTTPS, Host: "api.example.com", Port: 8443, Path: "/v1"},
			want: "https://api.example.com:8443/v1",
		},
		{
			name: "bracketed IPv6 literal preserved",
			spec: ExternalBackendSpec{Scheme: ExternalBackendSchemeHTTP, Host: "[2001:db8::1]", Port: 80},
			want: "http://[2001:db8::1]:80",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, tt.spec.URL())
		})
	}
}

func TestExternalBackendSpec_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spec    ExternalBackendSpec
		wantErr bool
		wantSub string
	}{
		{
			name: "valid http",
			spec: ExternalBackendSpec{Scheme: ExternalBackendSchemeHTTP, Host: "api.example.com", Port: 80},
		},
		{
			name: "valid https with path",
			spec: ExternalBackendSpec{Scheme: ExternalBackendSchemeHTTPS, Host: "api.example.com", Port: 443, Path: "/base"},
		},
		{
			name:    "invalid scheme",
			spec:    ExternalBackendSpec{Scheme: "ftp", Host: "api.example.com", Port: 80},
			wantErr: true,
			wantSub: "scheme must be http or https",
		},
		{
			name:    "empty host",
			spec:    ExternalBackendSpec{Scheme: ExternalBackendSchemeHTTP, Host: "", Port: 80},
			wantErr: true,
			wantSub: "host must not be empty",
		},
		{
			name:    "port zero",
			spec:    ExternalBackendSpec{Scheme: ExternalBackendSchemeHTTP, Host: "api.example.com", Port: 0},
			wantErr: true,
			wantSub: "out of range",
		},
		{
			name:    "port too high",
			spec:    ExternalBackendSpec{Scheme: ExternalBackendSchemeHTTP, Host: "api.example.com", Port: 70000},
			wantErr: true,
			wantSub: "out of range",
		},
		{
			name:    "path without leading slash",
			spec:    ExternalBackendSpec{Scheme: ExternalBackendSchemeHTTP, Host: "api.example.com", Port: 80, Path: "base"},
			wantErr: true,
			wantSub: "must begin with",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.spec.Validate()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantSub)

				return
			}

			require.NoError(t, err)
		})
	}
}

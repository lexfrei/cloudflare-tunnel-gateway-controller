package proxy_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestRequestRedirect_OmitsSchemeDefaultPort pins the HTTPRequestRedirectFilter
// Port godoc: Location carries no port when an http redirect lands on 80 or an
// https one on 443, whether the scheme comes from the filter or the request.
func TestRequestRedirect_OmitsSchemeDefaultPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		configScheme  string
		requestScheme string
		port          int32
		want          string
	}{
		{name: "https on 443", configScheme: "https", port: 443, want: "https://app.example.com/p"},
		{name: "http on 80", configScheme: "http", port: 80, want: "http://app.example.com/p"},
		{name: "https on 80 keeps the port", configScheme: "https", port: 80, want: "https://app.example.com:80/p"},
		{name: "http on 443 keeps the port", configScheme: "http", port: 443, want: "http://app.example.com:443/p"},
		{name: "https on 8443 keeps the port", configScheme: "https", port: 8443, want: "https://app.example.com:8443/p"},
		{name: "request scheme http on 80", requestScheme: "http", port: 80, want: "http://app.example.com/p"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			config := &proxy.RedirectConfig{Port: new(tt.port)}
			if tt.configScheme != "" {
				config.Scheme = new(tt.configScheme)
			}

			req := &http.Request{
				Host:   "app.example.com",
				URL:    &url.URL{Scheme: tt.requestScheme, Path: "/p"},
				Header: http.Header{},
			}

			resp := proxy.NewRequestRedirect(config).ProcessRequest(req)
			require.NotNil(t, resp)

			defer resp.Body.Close()

			assert.Equal(t, tt.want, resp.Header.Get("Location"))
		})
	}
}

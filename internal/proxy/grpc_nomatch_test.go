package proxy_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// rawCodec is a passthrough codec so the test can invoke an arbitrary gRPC
// method path without generated protobuf stubs.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) {
	raw, ok := v.([]byte)
	if !ok {
		return nil, errors.New("rawCodec: not a []byte")
	}

	return raw, nil
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	out, ok := v.(*[]byte)
	if !ok {
		return errors.New("rawCodec: not a *[]byte")
	}

	*out = append((*out)[:0], data...)

	return nil
}

func (rawCodec) Name() string { return "raw" }

// TestGRPCClientObservesUnimplementedOnNoMatch pins the gRPC half of the
// Gateway API v1.6.0 listeners clause (kubernetes-sigs/gateway-api#4408):
// traffic that matches no configured hostname must surface to a gRPC client
// as Unimplemented. The proxy answers a bare HTTP 404 on the no-match path;
// the gRPC HTTP-to-status mapping has clients synthesize UNIMPLEMENTED from
// it, and this test observes that end to end with a real grpc-go client
// over HTTP/2.
func TestGRPCClientObservesUnimplementedOnNoMatch(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	handler := proxy.NewHandler(router)

	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	creds := credentials.NewTLS(&tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	})

	conn, err := grpc.NewClient(
		"passthrough:///"+srv.Listener.Addr().String(),
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawCodec{})),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out []byte

	err = conn.Invoke(ctx, "/unmatched.Service/Ping", []byte{}, &out)
	require.Error(t, err)
	require.Equalf(t, codes.Unimplemented, status.Code(err),
		"a gRPC client must observe Unimplemented for a request matching no configured hostname, got: %v", err)
}

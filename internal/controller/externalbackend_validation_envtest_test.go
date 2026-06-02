//go:build envtest

package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

// TestExternalBackend_SchemaValidation pins the CRD-level OpenAPI validation
// (scheme enum, port range, host pattern, required fields) enforced by the
// kube-apiserver at admission time. These attach to the CRD's openAPIV3Schema
// and are not run by the fake client, so they are exercised through the real
// envtest control plane.
func TestExternalBackend_SchemaValidation(t *testing.T) {
	t.Parallel()

	require.NotNil(t, envK8sClient, "envtest must be wired up; see suite_envtest_test.go")

	cases := []struct {
		name    string
		spec    v1alpha1.ExternalBackendSpec
		wantErr bool
		wantSub string
	}{
		{
			name: "valid https backend accepted",
			spec: v1alpha1.ExternalBackendSpec{
				Scheme: v1alpha1.ExternalBackendSchemeHTTPS,
				Host:   "api.example.com",
				Port:   8443,
				Path:   "/v1",
			},
		},
		{
			name: "valid http backend accepted",
			spec: v1alpha1.ExternalBackendSpec{
				Scheme: v1alpha1.ExternalBackendSchemeHTTP,
				Host:   "api.example.com",
				Port:   80,
			},
		},
		{
			name: "invalid scheme rejected (enum)",
			spec: v1alpha1.ExternalBackendSpec{
				Scheme: "ftp",
				Host:   "api.example.com",
				Port:   80,
			},
			wantErr: true,
			wantSub: "scheme",
		},
		{
			name: "port above range rejected",
			spec: v1alpha1.ExternalBackendSpec{
				Scheme: v1alpha1.ExternalBackendSchemeHTTP,
				Host:   "api.example.com",
				Port:   70000,
			},
			wantErr: true,
			wantSub: "port",
		},
		{
			name: "port zero rejected",
			spec: v1alpha1.ExternalBackendSpec{
				Scheme: v1alpha1.ExternalBackendSchemeHTTP,
				Host:   "api.example.com",
				Port:   0,
			},
			wantErr: true,
			wantSub: "port",
		},
		{
			name: "host with embedded scheme rejected (pattern)",
			spec: v1alpha1.ExternalBackendSpec{
				Scheme: v1alpha1.ExternalBackendSchemeHTTP,
				Host:   "http://api.example.com",
				Port:   80,
			},
			wantErr: true,
			wantSub: "host",
		},
		{
			name: "path without leading slash rejected (pattern)",
			spec: v1alpha1.ExternalBackendSpec{
				Scheme: v1alpha1.ExternalBackendSchemeHTTP,
				Host:   "api.example.com",
				Port:   80,
				Path:   "novalidslash",
			},
			wantErr: true,
			wantSub: "path",
		},
	}

	for idx, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eb := &v1alpha1.ExternalBackend{
				ObjectMeta: metav1.ObjectMeta{
					Name:      stringHashSuffix("extbackend-validation", idx, tc.name),
					Namespace: "default",
				},
				Spec: tc.spec,
			}

			err := envK8sClient.Create(context.Background(), eb)
			if tc.wantErr {
				require.Error(t, err, "invalid ExternalBackend spec must be rejected at admission")
				assert.Contains(t, strings.ToLower(err.Error()), strings.ToLower(tc.wantSub),
					"rejection message should reference the offending field")

				return
			}

			require.NoError(t, err, "valid ExternalBackend spec must pass admission")
			_ = envK8sClient.Delete(context.Background(), eb)
		})
	}
}

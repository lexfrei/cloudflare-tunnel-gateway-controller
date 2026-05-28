//go:build e2e

package e2e

import (
	"crypto/tls"
	"crypto/x509"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// Phase 0 RED test for the TLS-only backend helper consumed by the
// rotation / gRPC-TLS / mirror-TLS e2e suites in later phases. The helper
// must emit a self-consistent Secret / ConfigMap / Deployment / Service
// quadruple: the CA in the ConfigMap signs the leaf certificate in the
// Secret, and the Deployment serves only that leaf over TLS on 8443.
func TestBuildTLSBackendManifests_ShapeAndSignature(t *testing.T) {
	t.Parallel()

	manifests, err := BuildTLSBackendManifests("test-ns", "tls-backend")
	require.NoError(t, err)

	// Secret carries a TLS keypair.
	require.NotNil(t, manifests.Secret)
	assert.Equal(t, "tls-backend", manifests.Secret.Name)
	assert.Equal(t, "test-ns", manifests.Secret.Namespace)
	assert.Equal(t, corev1.SecretTypeTLS, manifests.Secret.Type)
	require.Contains(t, manifests.Secret.Data, corev1.TLSCertKey)
	require.Contains(t, manifests.Secret.Data, corev1.TLSPrivateKeyKey)
	_, parseErr := tls.X509KeyPair(manifests.Secret.Data[corev1.TLSCertKey], manifests.Secret.Data[corev1.TLSPrivateKeyKey])
	require.NoError(t, parseErr, "Secret's tls.crt/tls.key must parse as a keypair")

	// ConfigMap carries the CA that signed the leaf.
	require.NotNil(t, manifests.ConfigMap)
	assert.Equal(t, "tls-backend-ca", manifests.ConfigMap.Name)
	assert.Equal(t, "test-ns", manifests.ConfigMap.Namespace)
	caPEM, ok := manifests.ConfigMap.Data["ca.crt"]
	require.True(t, ok, "ConfigMap must carry a ca.crt key")
	require.True(t, strings.Contains(caPEM, "BEGIN CERTIFICATE"), "ca.crt must be PEM-encoded")

	// CA actually signed the leaf.
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM([]byte(caPEM)), "ca.crt must be parseable")
	leafPEM := manifests.Secret.Data[corev1.TLSCertKey]
	leafBlock, _ := pemDecodeFirst(leafPEM)
	require.NotNil(t, leafBlock, "leaf cert must be PEM-encoded")
	leaf, parseErr := x509.ParseCertificate(leafBlock.Bytes)
	require.NoError(t, parseErr)
	_, verifyErr := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "tls-backend.test-ns.svc.cluster.local"})
	require.NoError(t, verifyErr, "leaf must verify against CA for the in-cluster DNS name")

	// Deployment runs an nginx image and mounts both the Secret and ConfigMap.
	require.NotNil(t, manifests.Deployment)
	assert.Equal(t, "tls-backend", manifests.Deployment.Name)
	assert.Equal(t, "test-ns", manifests.Deployment.Namespace)
	require.Len(t, manifests.Deployment.Spec.Template.Spec.Containers, 1)
	container := manifests.Deployment.Spec.Template.Spec.Containers[0]
	assert.Contains(t, container.Image, "nginx", "Deployment must use an nginx image for TLS termination")
	mountTargets := map[string]bool{}
	for _, vm := range container.VolumeMounts {
		mountTargets[vm.MountPath] = true
	}
	assert.True(t, mountTargets["/etc/nginx/tls"], "Deployment must mount the TLS keypair")
	assert.True(t, mountTargets["/etc/nginx/conf.d"], "Deployment must mount the nginx config")

	// Service exposes 8443 with appProtocol https.
	require.NotNil(t, manifests.Service)
	assert.Equal(t, "tls-backend", manifests.Service.Name)
	assert.Equal(t, "test-ns", manifests.Service.Namespace)
	require.Len(t, manifests.Service.Spec.Ports, 1)
	port := manifests.Service.Spec.Ports[0]
	assert.Equal(t, int32(8443), port.Port)
	require.NotNil(t, port.AppProtocol)
	assert.Equal(t, "https", *port.AppProtocol)
}

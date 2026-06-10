//go:build e2e

package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	pb "sigs.k8s.io/gateway-api/conformance/echo-basic/grpcechoserver"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// TestGRPCRouteOverTLSBackend pins the gRPC-over-TLS-backend path through the
// real tunnel: a BackendTLSPolicy targeting the gRPC backend's Service makes
// the proxy dial the backend over TLS (ALPN-negotiated HTTP/2) and verify it
// against the policy's CA. The backend serves TLS ONLY, so a successful Echo
// call through the tunnel proves the TLS handshake actually happened -- a
// cleartext h2c dial (the no-policy default) would be refused outright.
func TestGRPCRouteOverTLSBackend(t *testing.T) {
	cfg := loadTestConfig(t)

	if reason := grpcRequiresHTTP2SkipReason(cfg.TunnelProtocol); reason != "" {
		t.Skip(reason)
	}

	k8sClient := newK8sClient(t, cfg.KubeContext)
	ctx := context.Background()

	setupTestNamespace(t, k8sClient, cfg)
	setupGateway(t, k8sClient, cfg)

	const backendName = "grpc-tls-echo"

	serviceFQDN := backendName + "." + cfg.TestNamespace + ".svc.cluster.local"

	caPEM, certPEM, keyPEM := generateBackendCA(t, serviceFQDN)

	deployTLSGRPCEchoBackend(t, k8sClient, cfg.TestNamespace, backendName, caPEM, certPEM, keyPEM)

	policy := buildBackendTLSPolicy(backendName, cfg.TestNamespace, serviceFQDN)
	createBackendTLSPolicy(t, k8sClient, policy)

	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), policy)
	})

	route := buildGRPCRoute("grpc-tls-e2e", cfg, backendName)
	createGRPCRoute(t, k8sClient, route)

	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), route)
	})

	waitForGRPCRouteAccepted(t, k8sClient, route)

	conn, err := grpc.NewClient(
		cfg.TunnelHostname+":443",
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})),
	)
	require.NoError(t, err, "failed to create gRPC client")

	t.Cleanup(func() { _ = conn.Close() })

	echoClient := pb.NewGrpcEchoClient(conn)

	var resp *pb.EchoResponse

	pollErr := wait.PollUntilContextTimeout(ctx, 3*time.Second, 120*time.Second, true,
		func(pollCtx context.Context) (bool, error) {
			callCtx, cancel := context.WithTimeout(pollCtx, 10*time.Second)
			defer cancel()

			r, callErr := echoClient.Echo(callCtx, &pb.EchoRequest{})
			if callErr != nil {
				t.Logf("gRPC Echo over TLS backend not ready yet: %v", callErr)

				return false, nil
			}

			resp = r

			return true, nil
		},
	)
	require.NoError(t, pollErr,
		"gRPC Echo against the TLS-only backend must succeed: the proxy must dial TLS with the policy CA, a cleartext dial would be refused")
	require.NotNil(t, resp)

	assert.Contains(t, resp.GetAssertions().GetFullyQualifiedMethod(), grpcEchoService,
		"echo response should report the GrpcEcho service it was dispatched to")
}

// generateBackendCA builds a throwaway CA plus a server certificate for the
// given DNS name, returning PEM-encoded (caCert, serverCert, serverKey).
func generateBackendCA(t *testing.T, dnsName string) (string, string, string) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "e2e-backend-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{dnsName},
	}

	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	require.NoError(t, err)

	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	require.NoError(t, err)

	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER}))

	return caPEM, certPEM, keyPEM
}

// deployTLSGRPCEchoBackend deploys the echo-basic image in gRPC TLS mode: the
// server cert/key are mounted from a Secret and the echo serves TLS-only gRPC
// on container port 3000. A ConfigMap with the CA is created for the
// BackendTLSPolicy to reference.
func deployTLSGRPCEchoBackend(t *testing.T, k8sClient client.Client, namespace, name, caPEM, certPEM, keyPEM string) {
	t.Helper()
	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-cert", Namespace: namespace},
		StringData: map[string]string{"tls.crt": certPEM, "tls.key": keyPEM},
	}
	applyObject(ctx, t, k8sClient, secret)

	caConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-ca", Namespace: namespace},
		Data:       map[string]string{"ca.crt": caPEM},
	}
	applyObject(ctx, t, k8sClient, caConfigMap)

	replicas := int32(1)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{{
						Name: "tls",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{SecretName: name + "-cert"},
						},
					}},
					Containers: []corev1.Container{{
						Name:  name,
						Image: echoBasicImage,
						Env: []corev1.EnvVar{
							{Name: "GRPC_ECHO_SERVER", Value: "1"},
							{Name: "TLS_SERVER_CERT", Value: "/tls/tls.crt"},
							{Name: "TLS_SERVER_PRIVKEY", Value: "/tls/tls.key"},
							{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
							}},
							{Name: "NAMESPACE", ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
							}},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "tls", MountPath: "/tls", ReadOnly: true}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: *mustParseQuantity("10m")},
						},
					}},
				},
			},
		},
	}
	applyObject(ctx, t, k8sClient, deploy)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": name},
			Ports: []corev1.ServicePort{
				{Name: "grpc-tls", Port: 8080, TargetPort: intstr.FromInt32(3000), Protocol: corev1.ProtocolTCP},
			},
		},
	}
	applyObject(ctx, t, k8sClient, svc)

	waitForDeployment(ctx, t, k8sClient, namespace, name, 120*time.Second)
}

// buildBackendTLSPolicy targets the backend Service and validates against the
// generated CA with the service FQDN as SNI/hostname.
func buildBackendTLSPolicy(serviceName, namespace, hostname string) *gatewayv1.BackendTLSPolicy {
	return &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName + "-tls", Namespace: namespace},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{{
				LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{
					Kind: "Service",
					Name: gatewayv1.ObjectName(serviceName),
				},
			}},
			Validation: gatewayv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gatewayv1.LocalObjectReference{{
					Kind: "ConfigMap",
					Name: gatewayv1.ObjectName(serviceName + "-ca"),
				}},
				Hostname: gatewayv1.PreciseHostname(hostname),
			},
		},
	}
}

// createBackendTLSPolicy creates (or replaces) the policy.
func createBackendTLSPolicy(t *testing.T, k8sClient client.Client, policy *gatewayv1.BackendTLSPolicy) {
	t.Helper()
	ctx := context.Background()

	existing := &gatewayv1.BackendTLSPolicy{}

	err := k8sClient.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, existing)
	if err == nil {
		require.NoError(t, k8sClient.Delete(ctx, existing))
		time.Sleep(time.Second)
	}

	require.NoError(t, k8sClient.Create(ctx, policy))
	t.Logf("created BackendTLSPolicy %s/%s", policy.Namespace, policy.Name)
}

// applyObject create-or-replaces a namespaced object: existing objects are
// deleted first so reruns against a reused cluster start from the new spec.
func applyObject(ctx context.Context, t *testing.T, k8sClient client.Client, obj client.Object) {
	t.Helper()

	existing := obj.DeepCopyObject().(client.Object)

	err := k8sClient.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, existing)
	if err == nil {
		require.NoError(t, k8sClient.Delete(ctx, existing))
		time.Sleep(time.Second)
	}

	require.NoError(t, k8sClient.Create(ctx, obj))
}

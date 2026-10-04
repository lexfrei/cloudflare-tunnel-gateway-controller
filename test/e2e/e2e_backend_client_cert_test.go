//go:build e2e

package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
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

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

const (
	mtlsBackendName      = "mtls-echo"
	mtlsSharedGateway    = "mtls-shared"
	mtlsDedicatedGateway = "mtls-dedicated"
	mtlsClientA          = "e2e-client-a"
	mtlsClientB          = "e2e-client-b"
	mtlsSamples          = 20
)

// TestBackendClientCertSelectionEndToEnd pins which Gateway's client
// certificate a backend receives when a route has several parents. One Gateway
// is served by the shared plane and presents client A, the other by its own
// dedicated plane and presents client B. Both planes run on the one test
// tunnel, so each receives the union of their routes and certificate parents,
// and the edge spreads requests across both; every sampled request must carry
// the same certificate whichever connector served it:
//
//   - accepted on both Gateways: the first parent in spec order, client A,
//   - refused on the first Gateway in spec order: client B, never A,
//   - attached only through a ListenerSet of the shared Gateway: client A.
func TestBackendClientCertSelectionEndToEnd(t *testing.T) {
	cfg := loadTestConfig(t)
	httpClient := tunnelClient()
	k8sClient := newK8sClient(t, cfg.KubeContext)
	ctx := context.Background()

	setupTestNamespace(t, k8sClient, cfg)
	setupMTLSBackend(ctx, t, k8sClient, cfg.TestNamespace)
	setupMTLSGateways(ctx, t, k8sClient, cfg)

	listenerSet := buildMTLSListenerSet(cfg.TestNamespace)
	applyObject(ctx, t, k8sClient, listenerSet)

	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), listenerSet) })

	tests := []struct {
		name    string
		path    string
		parents []gatewayv1.ParentReference
		wantCN  string
	}{
		{
			name: "accepted on both Gateways",
			path: "/mtls-both",
			parents: []gatewayv1.ParentReference{
				{Name: mtlsSharedGateway}, {Name: mtlsDedicatedGateway},
			},
			wantCN: mtlsClientA,
		},
		{
			name: "refused on the first Gateway",
			path: "/mtls-refused",
			parents: []gatewayv1.ParentReference{
				{Name: mtlsSharedGateway, SectionName: new(gatewayv1.SectionName("absent"))},
				{Name: mtlsDedicatedGateway},
			},
			wantCN: mtlsClientB,
		},
		{
			name: "attached through a ListenerSet",
			path: "/mtls-listenerset",
			parents: []gatewayv1.ParentReference{
				{Kind: new(gatewayv1.Kind("ListenerSet")), Name: gatewayv1.ObjectName(listenerSet.Name)},
			},
			wantCN: mtlsClientA,
		},
	}

	for _, tt := range tests {
		route := buildMTLSRoute(cfg, tt.path, tt.parents)
		createHTTPRoute(t, k8sClient, route)

		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), route) })

		t.Run(tt.name, func(t *testing.T) {
			waitForClientCertCN(ctx, t, httpClient, cfg.TunnelHostname, tt.path, tt.wantCN)

			for range mtlsSamples {
				echo, resp, err := makeRequest(ctx, t, httpClient, cfg.TunnelHostname, http.MethodGet, tt.path, nil)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode)
				assert.Equal(t, tt.wantCN, peerCertCommonName(t, echo), "pod %s", echo.Pod)
			}
		})
	}
}

// setupMTLSBackend deploys an echo that serves TLS only and demands a client
// certificate signed by the test CA, and a BackendTLSPolicy for it. It also
// creates the two client certificate Secrets the Gateways reference.
func setupMTLSBackend(ctx context.Context, t *testing.T, k8sClient client.Client, namespace string) {
	t.Helper()

	serviceFQDN := mtlsBackendName + "." + namespace + ".svc.cluster.local"

	caCert, caKey := newTestCA(t)
	serverCert, serverKey := issueTestCert(t, caCert, caKey, serviceFQDN, x509.ExtKeyUsageServerAuth)

	for _, name := range []string{mtlsClientA, mtlsClientB} {
		cert, key := issueTestCert(t, caCert, caKey, name, x509.ExtKeyUsageClientAuth)
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Type:       corev1.SecretTypeTLS,
			StringData: map[string]string{corev1.TLSCertKey: cert, corev1.TLSPrivateKeyKey: key},
		}
		applyObject(ctx, t, k8sClient, secret)
	}

	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw}))

	applyObject(ctx, t, k8sClient, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: mtlsBackendName + "-cert", Namespace: namespace},
		StringData: map[string]string{"tls.crt": serverCert, "tls.key": serverKey},
	})
	applyObject(ctx, t, k8sClient, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: mtlsBackendName + "-ca", Namespace: namespace},
		Data:       map[string]string{"ca.crt": caPEM},
	})
	applyObject(ctx, t, k8sClient, buildMTLSEchoDeployment(namespace))
	applyObject(ctx, t, k8sClient, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: mtlsBackendName, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": mtlsBackendName},
			// Only the echo's TLS listener is exposed, so no request reaches
			// it without a TLS handshake.
			Ports: []corev1.ServicePort{
				{Name: "https", Port: 443, TargetPort: intstr.FromInt32(8443), Protocol: corev1.ProtocolTCP},
			},
		},
	})

	policy := buildBackendTLSPolicy(mtlsBackendName, namespace, serviceFQDN)
	applyObject(ctx, t, k8sClient, policy)

	t.Cleanup(func() { _ = k8sClient.Delete(context.WithoutCancel(ctx), policy) })

	waitForDeployment(ctx, t, k8sClient, namespace, mtlsBackendName, 120*time.Second)
}

func buildMTLSEchoDeployment(namespace string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: mtlsBackendName, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": mtlsBackendName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": mtlsBackendName}},
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{Name: "tls", VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{SecretName: mtlsBackendName + "-cert"},
						}},
						{Name: "ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: mtlsBackendName + "-ca"},
						}}},
					},
					Containers: []corev1.Container{{
						Name:  mtlsBackendName,
						Image: echoBasicImage,
						Env: []corev1.EnvVar{
							{Name: "TLS_SERVER_CERT", Value: "/tls/tls.crt"},
							{Name: "TLS_SERVER_PRIVKEY", Value: "/tls/tls.key"},
							{Name: "TLS_CLIENT_CACERTS", Value: "/ca/ca.crt"},
							{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
							}},
							{Name: "NAMESPACE", ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
							}},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "tls", MountPath: "/tls", ReadOnly: true},
							{Name: "ca", MountPath: "/ca", ReadOnly: true},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: *mustParseQuantity("10m")},
						},
					}},
				},
			},
		},
	}
}

// setupMTLSGateways creates the shared-plane Gateway presenting client A and
// the dedicated-plane Gateway presenting client B, and waits for both to be
// programmed.
func setupMTLSGateways(ctx context.Context, t *testing.T, k8sClient client.Client, cfg testConfig) {
	t.Helper()

	copyTunnelTokenSecret(ctx, t, k8sClient, cfg)

	gwConfig := &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "mtls-config", Namespace: cfg.TestNamespace},
		Spec: v1alpha1.GatewayConfigSpec{
			TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: "pg-tunnel-token"},
			Replicas:             new(int32(1)),
		},
	}
	applyObject(ctx, t, k8sClient, gwConfig)

	t.Cleanup(func() { _ = k8sClient.Delete(context.WithoutCancel(ctx), gwConfig) })

	shared := buildMTLSGateway(cfg.TestNamespace, mtlsSharedGateway, mtlsClientA)
	shared.Spec.AllowedListeners = &gatewayv1.AllowedListeners{
		Namespaces: &gatewayv1.ListenerNamespaces{From: new(gatewayv1.NamespacesFromSame)},
	}

	dedicated := buildMTLSGateway(cfg.TestNamespace, mtlsDedicatedGateway, mtlsClientB)
	dedicated.Spec.Infrastructure = &gatewayv1.GatewayInfrastructure{
		ParametersRef: &gatewayv1.LocalParametersReference{
			Group: "cf.k8s.lex.la", Kind: "GatewayConfig", Name: gwConfig.Name,
		},
	}

	for _, gateway := range []*gatewayv1.Gateway{shared, dedicated} {
		applyObject(ctx, t, k8sClient, gateway)

		t.Cleanup(func() { _ = k8sClient.Delete(context.WithoutCancel(ctx), gateway) })
	}

	waitForPerGatewayDeploymentReady(ctx, t, k8sClient,
		types.NamespacedName{Name: "cf-proxy-" + mtlsDedicatedGateway, Namespace: cfg.TestNamespace})

	for _, gateway := range []*gatewayv1.Gateway{shared, dedicated} {
		waitForGatewayProgrammed(ctx, t, k8sClient, types.NamespacedName{Name: gateway.Name, Namespace: gateway.Namespace})
	}
}

func buildMTLSGateway(namespace, name, clientCertSecret string) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			TLS: &gatewayv1.GatewayTLSConfig{Backend: &gatewayv1.GatewayBackendTLS{
				ClientCertificateRef: &gatewayv1.SecretObjectReference{Name: gatewayv1.ObjectName(clientCertSecret)},
			}},
			Listeners: []gatewayv1.Listener{{
				Name:     "https",
				Port:     443,
				Protocol: gatewayv1.HTTPSProtocolType,
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{From: new(gatewayv1.NamespacesFromSame)},
				},
			}},
		},
	}
}

// buildMTLSListenerSet attaches a catch-all HTTP entry on another port to the
// shared Gateway, so it neither conflicts with nor narrows the Gateway's own
// listener.
func buildMTLSListenerSet(namespace string) *gatewayv1.ListenerSet {
	return &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "mtls-ls", Namespace: namespace},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: mtlsSharedGateway},
			Listeners: []gatewayv1.ListenerEntry{{
				Name:     "http",
				Port:     80,
				Protocol: gatewayv1.HTTPProtocolType,
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{From: new(gatewayv1.NamespacesFromSame)},
				},
			}},
		},
	}
}

func buildMTLSRoute(cfg testConfig, path string, parents []gatewayv1.ParentReference) *gatewayv1.HTTPRoute {
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: strings.TrimPrefix(path, "/"), Namespace: cfg.TestNamespace},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parents},
			Hostnames:       []gatewayv1.Hostname{gatewayv1.Hostname(cfg.TunnelHostname)},
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches:     []gatewayv1.HTTPRouteMatch{{Path: pathPrefix(path)}},
				BackendRefs: []gatewayv1.HTTPBackendRef{backendRef(mtlsBackendName, 443, nil)},
			}},
		},
	}
}

// waitForClientCertCN polls until the backend answers through the tunnel with
// the wanted client certificate, which also waits out the rollout of the
// config to every connector.
func waitForClientCertCN(ctx context.Context, t *testing.T, httpClient *http.Client, host, path, wantCN string) {
	t.Helper()

	err := wait.PollUntilContextTimeout(ctx, 3*time.Second, 3*time.Minute, true,
		func(pollCtx context.Context) (bool, error) {
			echo, resp, reqErr := makeRequest(pollCtx, t, httpClient, host, http.MethodGet, path, nil)
			if reqErr != nil || resp.StatusCode != http.StatusOK {
				return false, nil //nolint:nilerr // the route is still rolling out; retry until timeout
			}

			return peerCertCommonName(t, echo) == wantCN, nil
		},
	)
	require.NoError(t, err, "backend never received client certificate %s on %s", wantCN, path)
}

func peerCertCommonName(t *testing.T, echo *echoResponse) string {
	t.Helper()

	if echo == nil || len(echo.TLS.PeerCertificates) == 0 {
		return ""
	}

	block, _ := pem.Decode([]byte(echo.TLS.PeerCertificates[0]))
	require.NotNil(t, block, "echo reported a peer certificate that is not PEM")

	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)

	return cert.Subject.CommonName
}

func newTestCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "e2e-mtls-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return cert, key
}

// issueTestCert returns a PEM certificate and key for name signed by the CA;
// name is also the DNS SAN, which a client certificate does not need but
// which is harmless.
func issueTestCert(
	t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, name string, usage x509.ExtKeyUsage,
) (string, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     []string{name},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	require.NoError(t, err)

	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

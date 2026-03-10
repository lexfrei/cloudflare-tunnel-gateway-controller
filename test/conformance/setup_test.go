//go:build conformance

package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// echoBasicImage is the official Gateway API conformance echo server.
const echoBasicImage = "gcr.io/k8s-staging-gateway-api/echo-basic:v20260204-monthly-2026.01-60-g28382302"

// newK8sClient creates a controller-runtime client using the given kubeconfig context.
func newK8sClient(t *testing.T, kubeContext string) client.Client {
	t.Helper()

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{
		CurrentContext: kubeContext,
	}

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	restConfig, err := kubeConfig.ClientConfig()
	require.NoError(t, err, "failed to get kubeconfig for context %s", kubeContext)

	s := scheme.Scheme
	require.NoError(t, gatewayv1.Install(s))

	k8sClient, err := client.New(restConfig, client.Options{Scheme: s})
	require.NoError(t, err, "failed to create kubernetes client")

	return k8sClient
}

// setupEchoBackends deploys echo-v1 and echo-v2 backends using the official
// Gateway API conformance echo-basic server.
func setupEchoBackends(t *testing.T, k8sClient client.Client, cfg testConfig) {
	t.Helper()

	ctx := context.Background()

	backends := []struct {
		name string
	}{
		{name: "echo-v1"},
		{name: "echo-v2"},
		{name: "echo-v3"},
	}

	for _, backend := range backends {
		deployEchoBackend(t, ctx, k8sClient, cfg.TestNamespace, backend.name)
	}

	// Wait for backends to be ready.
	for _, backend := range backends {
		waitForDeployment(t, ctx, k8sClient, cfg.TestNamespace, backend.name, 120*time.Second)
	}
}

func deployEchoBackend(t *testing.T, ctx context.Context, k8sClient client.Client, namespace, name string) {
	t.Helper()

	replicas := int32(1)

	// Check if deployment already exists.
	existing := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, existing); err == nil {
		t.Logf("deployment %s/%s already exists, skipping", namespace, name)
		return
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  name,
							Image: echoBasicImage,
							Env: []corev1.EnvVar{
								{
									Name: "POD_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.name",
										},
									},
								},
								{
									Name: "NAMESPACE",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.namespace",
										},
									},
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: *mustParseQuantity("10m"),
								},
							},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, deploy))
	t.Logf("created deployment %s/%s", namespace, name)

	// Create service.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": name},
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       80,
					TargetPort: intstr.FromInt32(3000),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, svc))
	t.Logf("created service %s/%s", namespace, name)
}

func waitForDeployment(t *testing.T, ctx context.Context, k8sClient client.Client, namespace, name string, timeout time.Duration) {
	t.Helper()

	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true,
		func(pollCtx context.Context) (bool, error) {
			deploy := &appsv1.Deployment{}
			if getErr := k8sClient.Get(pollCtx, types.NamespacedName{Name: name, Namespace: namespace}, deploy); getErr != nil {
				return false, nil
			}

			return deploy.Status.ReadyReplicas >= 1, nil
		},
	)
	require.NoError(t, err, "deployment %s/%s did not become ready", namespace, name)
	t.Logf("deployment %s/%s is ready", namespace, name)
}

// setupGateway creates the Gateway resource for conformance tests.
func setupGateway(t *testing.T, k8sClient client.Client, cfg testConfig) {
	t.Helper()

	ctx := context.Background()

	// Check if Gateway already exists.
	existing := &gatewayv1.Gateway{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cfg.GatewayName, Namespace: cfg.Namespace}, existing); err == nil {
		t.Logf("gateway %s/%s already exists, skipping", cfg.Namespace, cfg.GatewayName)
		return
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.GatewayName,
			Namespace: cfg.Namespace,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https",
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{
							From: new(gatewayv1.NamespacesFromAll),
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, gateway))
	t.Logf("created gateway %s/%s", cfg.Namespace, cfg.GatewayName)

	// Wait for Gateway to be accepted.
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 60*time.Second, true,
		func(pollCtx context.Context) (bool, error) {
			gw := &gatewayv1.Gateway{}
			if getErr := k8sClient.Get(pollCtx, types.NamespacedName{Name: cfg.GatewayName, Namespace: cfg.Namespace}, gw); getErr != nil {
				return false, nil
			}

			for _, condition := range gw.Status.Conditions {
				if condition.Type == string(gatewayv1.GatewayConditionAccepted) &&
					condition.Status == metav1.ConditionTrue {
					return true, nil
				}
			}

			return false, nil
		},
	)
	require.NoError(t, err, "gateway did not become accepted")
	t.Logf("gateway %s/%s is accepted", cfg.Namespace, cfg.GatewayName)
}

func mustParseQuantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

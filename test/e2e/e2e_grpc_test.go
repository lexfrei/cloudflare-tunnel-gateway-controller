//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
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

const (
	// grpcEchoService is the fully-qualified gRPC service the echo-basic
	// image serves when GRPC_ECHO_SERVER=1.
	grpcEchoService = "gateway_api_conformance.echo_basic.grpcecho.GrpcEcho"
	grpcEchoMethod  = "Echo"
)

// TestGRPCRouteEndToEnd deploys the gRPC echo backend, binds a GRPCRoute with
// an exact service/method match, and dials a real gRPC client through the
// Cloudflare tunnel. It asserts the call reaches the backend (the echo
// response carries the fully-qualified method), proving GRPCRoute traffic
// routes through the in-process proxy.
//
//nolint:funlen // one end-to-end scenario with deploy + wait + gRPC dial steps
func TestGRPCRouteEndToEnd(t *testing.T) {
	cfg := loadTestConfig()
	k8sClient := newK8sClient(t, cfg.KubeContext)
	ctx := context.Background()

	setupTestNamespace(t, k8sClient, cfg)
	deployGRPCEchoBackend(t, k8sClient, cfg.TestNamespace, "grpc-echo")
	setupGateway(t, k8sClient, cfg)

	route := buildGRPCRoute("grpc-e2e", cfg, "grpc-echo")
	createGRPCRoute(t, k8sClient, route)

	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, route)
	})

	waitForGRPCRouteAccepted(t, k8sClient, route)

	// Dial through the tunnel edge over TLS; the GRPCRoute hostname is the
	// tunnel hostname, so the proxy matches /{service}/Echo and forwards h2c
	// to the backend.
	conn, err := grpc.NewClient(
		cfg.TunnelHostname+":443",
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})),
	)
	require.NoError(t, err, "failed to create gRPC client")

	t.Cleanup(func() { _ = conn.Close() })

	echoClient := pb.NewGrpcEchoClient(conn)

	// The proxy programs the route asynchronously; retry the call until it
	// lands (or the deadline trips).
	var resp *pb.EchoResponse

	pollErr := wait.PollUntilContextTimeout(ctx, 3*time.Second, 90*time.Second, true,
		func(pollCtx context.Context) (bool, error) {
			callCtx, cancel := context.WithTimeout(pollCtx, 10*time.Second)
			defer cancel()

			r, callErr := echoClient.Echo(callCtx, &pb.EchoRequest{})
			if callErr != nil {
				t.Logf("gRPC Echo not ready yet: %v", callErr)

				return false, nil
			}

			resp = r

			return true, nil
		},
	)
	require.NoError(t, pollErr, "gRPC Echo did not succeed through the tunnel in time")
	require.NotNil(t, resp)

	assert.Contains(t, resp.GetAssertions().GetFullyQualifiedMethod(), grpcEchoService,
		"echo response should report the GrpcEcho service it was dispatched to")
}

// deployGRPCEchoBackend deploys the echo-basic image in gRPC mode
// (GRPC_ECHO_SERVER=1, serving gRPC on container port 3000) plus a Service
// exposing port 8080 → 3000.
func deployGRPCEchoBackend(t *testing.T, k8sClient client.Client, namespace, name string) {
	t.Helper()
	ctx := context.Background()

	replicas := int32(1)
	grpcEnv := "1"

	existing := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, existing); err == nil {
		t.Logf("deployment %s/%s already exists, skipping", namespace, name)
	} else {
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  name,
								Image: echoBasicImage,
								Env: []corev1.EnvVar{
									{Name: "GRPC_ECHO_SERVER", Value: grpcEnv},
									{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
									}},
									{Name: "NAMESPACE", ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
									}},
								},
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{corev1.ResourceCPU: *mustParseQuantity("10m")},
								},
							},
						},
					},
				},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, deploy))
		t.Logf("created gRPC echo deployment %s/%s", namespace, name)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": name},
			Ports: []corev1.ServicePort{
				{Name: "grpc", Port: 8080, TargetPort: intstr.FromInt32(3000), Protocol: corev1.ProtocolTCP},
			},
		},
	}
	if err := k8sClient.Create(ctx, svc); err != nil {
		t.Logf("service %s/%s create: %v (may already exist)", namespace, name, err)
	}

	waitForDeployment(t, ctx, k8sClient, namespace, name, 120*time.Second)
}

func buildGRPCRoute(name string, cfg testConfig, backend string) *gatewayv1.GRPCRoute {
	gatewayNS := gatewayv1.Namespace(cfg.Namespace)
	hostname := gatewayv1.Hostname(cfg.TunnelHostname)
	exact := gatewayv1.GRPCMethodMatchExact
	svc := grpcEchoService
	method := grpcEchoMethod
	port := gatewayv1.PortNumber(8080)

	return &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cfg.TestNamespace},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(cfg.GatewayName), Namespace: &gatewayNS},
				},
			},
			Hostnames: []gatewayv1.Hostname{hostname},
			Rules: []gatewayv1.GRPCRouteRule{
				{
					Matches: []gatewayv1.GRPCRouteMatch{
						{Method: &gatewayv1.GRPCMethodMatch{Type: &exact, Service: &svc, Method: &method}},
					},
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: gatewayv1.ObjectName(backend), Port: &port,
							},
						}},
					},
				},
			},
		},
	}
}

func createGRPCRoute(t *testing.T, k8sClient client.Client, route *gatewayv1.GRPCRoute) {
	t.Helper()
	ctx := context.Background()

	existing := &gatewayv1.GRPCRoute{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: route.Name, Namespace: route.Namespace}, existing); err == nil {
		require.NoError(t, k8sClient.Delete(ctx, existing))
		time.Sleep(time.Second)
	}

	require.NoError(t, k8sClient.Create(ctx, route))
	t.Logf("created GRPCRoute %s/%s", route.Namespace, route.Name)
}

func waitForGRPCRouteAccepted(t *testing.T, k8sClient client.Client, route *gatewayv1.GRPCRoute) {
	t.Helper()

	err := wait.PollUntilContextTimeout(context.Background(), 2*time.Second, 90*time.Second, true,
		func(pollCtx context.Context) (bool, error) {
			current := &gatewayv1.GRPCRoute{}
			if getErr := k8sClient.Get(pollCtx, types.NamespacedName{Name: route.Name, Namespace: route.Namespace}, current); getErr != nil {
				return false, nil
			}

			for _, parent := range current.Status.Parents {
				for _, cond := range parent.Conditions {
					if cond.Type == string(gatewayv1.RouteConditionAccepted) && cond.Status == metav1.ConditionTrue {
						return true, nil
					}
				}
			}

			return false, nil
		},
	)
	require.NoError(t, err, "GRPCRoute did not become Accepted in time")
}

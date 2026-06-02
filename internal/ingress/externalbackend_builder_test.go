package ingress_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
)

func newExternalBackend(name, namespace, scheme, host string, port int32, path string) *v1alpha1.ExternalBackend {
	return &v1alpha1.ExternalBackend{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.ExternalBackendSpec{
			Scheme: v1alpha1.ExternalBackendScheme(scheme), Host: host, Port: port, Path: path,
		},
	}
}

func externalBackendRef(name string, namespace *gatewayv1.Namespace) gatewayv1.HTTPBackendRef {
	group := gatewayv1.Group("cf.k8s.lex.la")
	kind := gatewayv1.Kind("ExternalBackend")

	return gatewayv1.HTTPBackendRef{
		BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{
				Group:     &group,
				Kind:      &kind,
				Name:      gatewayv1.ObjectName(name),
				Namespace: namespace,
			},
		},
	}
}

func ebBuilderScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1beta1.Install(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	return scheme
}

func TestBuild_ExternalBackend_Resolved(t *testing.T) {
	t.Parallel()

	eb := newExternalBackend("ext-api", "default", "https", "api.example.com", 8443, "/v1")
	cli := fake.NewClientBuilder().WithScheme(ebBuilderScheme(t)).WithObjects(eb).Build()

	builder := ingress.NewBuilder("cluster.local", nil, cli, nil, nil)
	routes := []gatewayv1.HTTPRoute{siRoute("test-route", "default", externalBackendRef("ext-api", nil))}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "https://api.example.com:8443/v1", buildResult.Rules[0].Service.Value,
		"an ExternalBackend must resolve to its declared scheme://host:port/path")
	assert.Empty(t, buildResult.FailedRefs)
}

func TestBuild_ExternalBackendNotFound(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientBuilder().WithScheme(ebBuilderScheme(t)).Build()

	builder := ingress.NewBuilder("cluster.local", nil, cli, nil, nil)
	routes := []gatewayv1.HTTPRoute{siRoute("test-route", "default", externalBackendRef("missing", nil))}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.FailedRefs, 1)
	assert.Equal(t, "BackendNotFound", buildResult.FailedRefs[0].Reason)
	assert.Equal(t, "missing", buildResult.FailedRefs[0].BackendName)
}

func TestBuild_ExternalBackendMalformed(t *testing.T) {
	t.Parallel()

	// A bare host:port slips past the CRD host pattern (which permits a colon for
	// bracketed IPv6). It must surface ResolvedRefs=False, not a green status.
	eb := newExternalBackend("ext-bad", "default", "https", "internal-api:8080", 443, "")
	cli := fake.NewClientBuilder().WithScheme(ebBuilderScheme(t)).WithObjects(eb).Build()

	builder := ingress.NewBuilder("cluster.local", nil, cli, nil, nil)
	routes := []gatewayv1.HTTPRoute{siRoute("test-route", "default", externalBackendRef("ext-bad", nil))}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.FailedRefs, 1)
	assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedValue), buildResult.FailedRefs[0].Reason,
		"a malformed ExternalBackend must surface ResolvedRefs=False, not a silent dial-time 500")
}

func TestBuild_ExternalBackendCrossNamespace_WithGrant(t *testing.T) {
	t.Parallel()

	eb := newExternalBackend("ext-api", "backend", "https", "api.example.com", 443, "")
	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "allow-eb", Namespace: "backend"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: "default"},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{Group: "cf.k8s.lex.la", Kind: "ExternalBackend"},
			},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(ebBuilderScheme(t)).WithObjects(eb, grant).Build()

	validator := referencegrant.NewValidator(cli)
	builder := ingress.NewBuilder("cluster.local", validator, cli, nil, nil)

	ns := gatewayv1.Namespace("backend")
	routes := []gatewayv1.HTTPRoute{siRoute("test-route", "default", externalBackendRef("ext-api", &ns))}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "https://api.example.com:443", buildResult.Rules[0].Service.Value)
	assert.Empty(t, buildResult.FailedRefs)
}

func TestBuild_ExternalBackendCrossNamespace_ServiceGrantDenies(t *testing.T) {
	t.Parallel()

	eb := newExternalBackend("ext-api", "backend", "https", "api.example.com", 443, "")
	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "allow-svc-only", Namespace: "backend"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: "default"},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{Group: "", Kind: "Service"},
			},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(ebBuilderScheme(t)).WithObjects(eb, grant).Build()

	validator := referencegrant.NewValidator(cli)
	builder := ingress.NewBuilder("cluster.local", validator, cli, nil, nil)

	ns := gatewayv1.Namespace("backend")
	routes := []gatewayv1.HTTPRoute{siRoute("test-route", "default", externalBackendRef("ext-api", &ns))}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.FailedRefs, 1)
	assert.Equal(t, "RefNotPermitted", buildResult.FailedRefs[0].Reason,
		"a Service-keyed grant must not authorize an ExternalBackend cross-namespace ref")
}

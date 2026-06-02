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
	mcsv1alpha1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
)

func newServiceImport(name, namespace string, ports ...int32) *mcsv1alpha1.ServiceImport {
	svcPorts := make([]mcsv1alpha1.ServicePort, 0, len(ports))
	for _, p := range ports {
		svcPorts = append(svcPorts, mcsv1alpha1.ServicePort{Port: p})
	}

	return &mcsv1alpha1.ServiceImport{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: mcsv1alpha1.ServiceImportSpec{
			Type:  mcsv1alpha1.ClusterSetIP,
			Ports: svcPorts,
		},
	}
}

func serviceImportBackendRef(name string, namespace *gatewayv1.Namespace, port gatewayv1.PortNumber) gatewayv1.HTTPBackendRef {
	group := gatewayv1.Group("multicluster.x-k8s.io")
	kind := gatewayv1.Kind("ServiceImport")

	return gatewayv1.HTTPBackendRef{
		BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{
				Group:     &group,
				Kind:      &kind,
				Name:      gatewayv1.ObjectName(name),
				Namespace: namespace,
				Port:      &port,
			},
		},
	}
}

func siRoute(name, namespace string, backend gatewayv1.HTTPBackendRef) gatewayv1.HTTPRoute {
	return gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"app.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{BackendRefs: []gatewayv1.HTTPBackendRef{backend}},
			},
		},
	}
}

func siScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1beta1.Install(scheme))
	utilruntime.Must(mcsv1alpha1.Install(scheme))

	return scheme
}

func TestBuild_ServiceImportBackend_Resolved(t *testing.T) {
	t.Parallel()

	si := newServiceImport("imported", "default", 80)
	fakeClient := fake.NewClientBuilder().WithScheme(siScheme(t)).WithObjects(si).Build()

	builder := ingress.NewBuilder("cluster.local", nil, fakeClient, nil, nil)
	routes := []gatewayv1.HTTPRoute{siRoute("test-route", "default", serviceImportBackendRef("imported", nil, 80))}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "http://imported.default.svc.clusterset.local:80", buildResult.Rules[0].Service.Value,
		"a ServiceImport backend must resolve to its clusterset.local URL")
	assert.Empty(t, buildResult.FailedRefs)
}

func TestBuild_ServiceImportNotFound(t *testing.T) {
	t.Parallel()

	fakeClient := fake.NewClientBuilder().WithScheme(siScheme(t)).Build()

	builder := ingress.NewBuilder("cluster.local", nil, fakeClient, nil, nil)
	routes := []gatewayv1.HTTPRoute{siRoute("test-route", "default", serviceImportBackendRef("missing", nil, 80))}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, ingress.CatchAllService, buildResult.Rules[0].Service.Value)
	require.Len(t, buildResult.FailedRefs, 1)
	assert.Equal(t, "BackendNotFound", buildResult.FailedRefs[0].Reason)
	assert.Equal(t, "missing", buildResult.FailedRefs[0].BackendName)
	assert.Equal(t, "clusterset.local", buildResult.FailedRefs[0].Domain,
		"a ServiceImport failed-ref must carry the clusterset domain so the controller marks the matching proxy backend 500")
}

func TestBuild_ServiceImportPortNotExported(t *testing.T) {
	t.Parallel()

	si := newServiceImport("imported", "default", 80)
	fakeClient := fake.NewClientBuilder().WithScheme(siScheme(t)).WithObjects(si).Build()

	builder := ingress.NewBuilder("cluster.local", nil, fakeClient, nil, nil)
	routes := []gatewayv1.HTTPRoute{siRoute("test-route", "default", serviceImportBackendRef("imported", nil, 9999))}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, ingress.CatchAllService, buildResult.Rules[0].Service.Value)
	require.Len(t, buildResult.FailedRefs, 1)
	assert.Equal(t, "BackendNotFound", buildResult.FailedRefs[0].Reason,
		"a port not exported by the ServiceImport is unresolvable → BackendNotFound")
}

func TestBuild_ServiceImportCrossNamespace_WithServiceImportGrant(t *testing.T) {
	t.Parallel()

	si := newServiceImport("imported", "backend", 80)
	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "allow-si", Namespace: "backend"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: "default"},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{Group: "multicluster.x-k8s.io", Kind: "ServiceImport"},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(siScheme(t)).WithObjects(si, grant).Build()

	validator := referencegrant.NewValidator(fakeClient)
	builder := ingress.NewBuilder("cluster.local", validator, fakeClient, nil, nil)

	ns := gatewayv1.Namespace("backend")
	routes := []gatewayv1.HTTPRoute{siRoute("test-route", "default", serviceImportBackendRef("imported", &ns, 80))}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 2)
	assert.Equal(t, "http://imported.backend.svc.clusterset.local:80", buildResult.Rules[0].Service.Value)
	assert.Empty(t, buildResult.FailedRefs, "a ServiceImport-keyed grant must permit the cross-namespace ref")
}

func TestBuild_ServiceImportCrossNamespace_ServiceGrantDenies(t *testing.T) {
	t.Parallel()

	si := newServiceImport("imported", "backend", 80)
	// Grant keyed on core Service must NOT authorize a ServiceImport ref.
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
	fakeClient := fake.NewClientBuilder().WithScheme(siScheme(t)).WithObjects(si, grant).Build()

	validator := referencegrant.NewValidator(fakeClient)
	builder := ingress.NewBuilder("cluster.local", validator, fakeClient, nil, nil)

	ns := gatewayv1.Namespace("backend")
	routes := []gatewayv1.HTTPRoute{siRoute("test-route", "default", serviceImportBackendRef("imported", &ns, 80))}

	buildResult := builder.Build(context.Background(), routes)

	require.Len(t, buildResult.Rules, 1)
	assert.Equal(t, ingress.CatchAllService, buildResult.Rules[0].Service.Value)
	require.Len(t, buildResult.FailedRefs, 1)
	assert.Equal(t, "RefNotPermitted", buildResult.FailedRefs[0].Reason,
		"a Service-keyed grant must not authorize a ServiceImport cross-namespace ref")
}

package controller

// Covers the tunnel ingress document's size and shape: one rule per hostname,
// written whatever its size, since the document only feeds the Cloudflare
// dashboard and no route depends on it being accepted.

import (
	"context"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

const documentClassTunnelID = "99999999-9999-4999-8999-999999999999"

// documentRoute builds a route whose single rule serves the given number of
// hostnames and path matches. The document holds one rule per distinct
// hostname, so only hostnames fill it; the matches are there to show they do
// not.
//
// spec.hostnames is MaxItems=16 and a rule's matches are MaxItems=64, so the
// callers stay inside what a real apiserver accepts: the fake client enforces
// neither, and a fixture a real apiserver would reject invites claims about
// tenant behaviour that no tenant can reproduce.
func documentRoute(namespace, name string, hostnames, matches int) *gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix
	port := gatewayv1.PortNumber(80)

	hosts := make([]gatewayv1.Hostname, 0, hostnames)
	for i := range hostnames {
		hosts = append(hosts, gatewayv1.Hostname(fmt.Sprintf("h%d.%s.%s.example.com", i, name, namespace)))
	}

	pathMatches := make([]gatewayv1.HTTPRouteMatch, 0, matches)
	for i := range matches {
		pathMatches = append(pathMatches, gatewayv1.HTTPRouteMatch{
			Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new(fmt.Sprintf("/p%d", i))},
		})
	}

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{
					Name:      "shared-gw",
					Namespace: new(gatewayv1.Namespace("default")),
				}},
			},
			Hostnames: hosts,
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: pathMatches,
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: &port},
							Weight:                 new(int32(1)),
						}},
					},
				},
			},
		},
	}
}

// largeDocumentRoutes x 16 hostnames is 1008, past the 1000 rules an earlier
// version of this controller refused to write.
const largeDocumentRoutes = 63

// largeDocumentObjects is one shared tunnel serving two namespaces whose
// routes add up to more than 1000 hostnames.
func largeDocumentObjects(t *testing.T) []runtime.Object {
	t.Helper()

	objects := documentBaseObjects()
	for i := range largeDocumentRoutes {
		objects = append(objects, documentRoute("tenant-a", fmt.Sprintf("r%02d", i), 16, 64))
	}

	return append(objects, documentRoute("tenant-b", "small", 1, 1))
}

// documentBaseObjects is the shared tunnel and the backends both tenants use.
func documentBaseObjects() []runtime.Object {
	return []runtime.Object{
		&gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "cf-test"},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: skipTestControllerName,
				ParametersRef: &gatewayv1.ParametersReference{
					Group: config.ParametersRefGroup, Kind: config.ParametersRefKind, Name: "cfg",
				},
			},
		},
		&v1alpha1.GatewayClassConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "cfg"},
			Spec: v1alpha1.GatewayClassConfigSpec{
				CloudflareCredentialsSecretRef: v1alpha1.SecretReference{Name: "creds", Namespace: "default"},
				AccountID:                      "test-account",
				TunnelID:                       documentClassTunnelID,
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
			Data:       map[string][]byte{"api-token": []byte("test-token")},
		},
		&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "shared-gw", Namespace: "default"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cf-test", Listeners: httpListener()},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "tenant-a"},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "tenant-b"},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
		},
	}
}

// TestSyncAllRoutes_SpecMaximumRouteIsSixteenRules pins what the
// hostname-only document buys: a route at the spec's ceilings, 16 hostnames
// and a rule with 64 matches, is 16 rules, not 1024.
func TestSyncAllRoutes_SpecMaximumRouteIsSixteenRules(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := partitionSyncSyncerFor(t, api, append(documentBaseObjects(), documentRoute("tenant-a", "big", 16, 64)))

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	assert.Len(t, api.lastIngress(t, documentClassTunnelID), 17, "16 hostnames plus the catch-all")
}

// TestSyncAllRoutes_LargeDocumentIsWritten pins that the document is written
// whatever its size: there is no rule cap to refuse it at.
func TestSyncAllRoutes_LargeDocumentIsWritten(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := partitionSyncSyncerFor(t, api, largeDocumentObjects(t))

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	assert.Len(t, api.hostnamesFor(documentClassTunnelID), largeDocumentRoutes*16+1)
}

// TestSyncAllRoutes_PerPathDocumentIsRewrittenPerHostname covers the first
// sync over a document written with one rule per path match, duplicate
// entries included: every old rule goes, and each hostname is left exactly
// once, without a path.
func TestSyncAllRoutes_PerPathDocumentIsRewrittenPerHostname(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	route := documentRoute("tenant-a", "app", 2, 3)
	hostname := string(route.Spec.Hostnames[0])
	service := "http://svc.tenant-a.svc.cluster.local:80"

	api.seed(documentClassTunnelID, []map[string]any{
		{"hostname": hostname, "path": "/p0*", "service": service},
		{"hostname": hostname, "path": "/p1*", "service": service},
		{"hostname": hostname, "path": "/p1*", "service": service},
		{"hostname": "gone.example.com", "service": service},
		{"service": "http_status:404"},
	})

	syncer := partitionSyncSyncerFor(t, api, append(documentBaseObjects(), route))

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	assert.Equal(t, []map[string]any{
		{"hostname": hostname, "service": service},
		{"hostname": string(route.Spec.Hostnames[1]), "service": service},
		{"service": "http_status:404"},
	}, api.lastIngress(t, documentClassTunnelID))
}

// TestSyncAllRoutes_FailedDocumentReadIsCounted pins that a write that fails
// before the PUT, here on reading the current document, is counted as a sync
// error like a failed PUT: route status no longer shows these failures, so
// the counter is where an operator finds them.
func TestSyncAllRoutes_FailedDocumentReadIsCounted(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := partitionSyncSyncerFor(t, api, append(documentBaseObjects(), documentRoute("tenant-a", "app", 1, 1)))

	reg := prometheus.NewRegistry()
	syncer.Metrics = cfmetrics.NewCollector(reg)

	api.server.Close()

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)

	assert.InDelta(t, 1, gatheredCounterTotal(t, reg, "cftunnel_sync_errors_total"), 0)
}

// gatheredCounterTotal sums every series of the named counter.
func gatheredCounterTotal(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()

	families, err := reg.Gather()
	require.NoError(t, err)

	var total float64

	for _, family := range families {
		if family.GetName() != name {
			continue
		}

		for _, metric := range family.GetMetric() {
			total += metric.GetCounter().GetValue()
		}
	}

	return total
}

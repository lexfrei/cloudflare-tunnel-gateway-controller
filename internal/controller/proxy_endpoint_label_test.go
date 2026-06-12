package controller

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

// TestProxyEndpointReconciler_TruncatedGatewayLabelResolvesPartition pins the
// label→partition mapping for LONG Gateway names: the rendered label value is
// truncated to 63 characters (label limit), but partition keys carry the FULL
// Gateway name — the reconciler must resolve the truncated value back to the
// real Gateway instead of resyncing a partition that does not exist.
func TestProxyEndpointReconciler_TruncatedGatewayLabelResolvesPartition(t *testing.T) {
	t.Parallel()

	longName := strings.Repeat("very-long-gateway-name-", 4) // 92 chars
	labelValue := render.GatewayLabelValue(longName)
	require.LessOrEqual(t, len(labelValue), 63, "label values cap at 63")
	require.NotEqual(t, longName, labelValue, "the fixture must actually exercise truncation")

	var pushes atomic.Int32

	tenantServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			pushes.Add(1)
		}

		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tenantServer.Close)

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: longName, Namespace: "tenant"}},
		&discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cf-proxy-slice",
				Namespace: "tenant",
				Labels:    map[string]string{render.GatewayLabel: labelValue},
			},
		},
	).Build()

	proxySyncer := NewProxySyncer("cluster.local", "", "", fakeClient, slog.Default())

	ctx := context.Background()

	_, err := proxySyncer.SyncPartition(ctx, "tenant/"+longName, "",
		[]string{tenantServer.URL + "/config"},
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("tenant-r", "tenant.example.com")}, nil, nil, nil)
	require.NoError(t, err)

	pushesBefore := pushes.Load()

	reconciler := &ProxyEndpointReconciler{
		Client:      fakeClient,
		ProxySyncer: proxySyncer,
	}

	_, err = reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "tenant", Name: "cf-proxy-slice"},
	})
	require.NoError(t, err)

	assert.Equal(t, pushesBefore+1, pushes.Load(),
		"the truncated label value must resolve to the real Gateway's partition and replay its config")
}

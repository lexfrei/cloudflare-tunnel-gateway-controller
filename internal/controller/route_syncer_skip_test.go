package controller

// Pins the steady-state Cloudflare write skip: the configurations endpoint
// is a whole-document update with no rule-level PATCH, so the only way to
// reduce API write traffic is to not write when the desired document equals
// the deployed one. These tests drive SyncAllRoutes against an httptest
// Cloudflare API and count the PUTs.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
)

const skipTestControllerName = "cf.k8s.lex.la/test-controller"

// fakeTunnelAPI emulates the two Cloudflare configuration endpoints the
// syncer talks to, serving a fixed current ingress and counting writes.
type fakeTunnelAPI struct {
	server   *httptest.Server
	putCount atomic.Int32
	ingress  []map[string]any
}

func newFakeTunnelAPI(t *testing.T, currentIngress []map[string]any) *fakeTunnelAPI {
	t.Helper()

	api := &fakeTunnelAPI{ingress: currentIngress}

	api.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet:
			payload := map[string]any{
				"success": true,
				"errors":  []any{},
				"result": map[string]any{
					"config": map[string]any{"ingress": api.ingress},
				},
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(payload)
		case http.MethodPut:
			api.putCount.Add(1)

			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"success": true,
				"errors":  []any{},
				"result":  map[string]any{"config": map[string]any{}},
			})
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	t.Cleanup(api.server.Close)

	return api
}

// newSkipTestSyncer builds a RouteSyncer over a fake cluster carrying the
// GatewayClass → GatewayClassConfig → Secret chain the resolver walks, with
// the Cloudflare client pointed at the fake API.
func newSkipTestSyncer(t *testing.T, api *fakeTunnelAPI) *RouteSyncer {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cf-test"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: skipTestControllerName,
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  config.ParametersRefKind,
				Name:  "cfg",
			},
		},
	}
	gccConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg"},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{Name: "creds", Namespace: "default"},
			AccountID:                      "test-account",
			TunnelID:                       "test-tunnel",
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
		Data:       map[string][]byte{"api-token": []byte("test-token")},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gccConfig, secret).
		Build()

	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	syncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", skipTestControllerName, resolver, cfmetrics.NewNoopCollector(), slog.Default())
	syncer.cloudflareClientFactory = func(_ *config.ResolvedConfig) *cloudflare.Client {
		return cloudflare.NewClient(
			option.WithAPIToken("test-token"),
			option.WithBaseURL(api.server.URL),
		)
	}

	return syncer
}

func TestSyncAllRoutes_SkipsWriteWhenConfigUnchanged(t *testing.T) {
	t.Parallel()

	// The deployed config already matches the desired no-routes document:
	// just the catch-all.
	api := newFakeTunnelAPI(t, []map[string]any{
		{"service": ingress.CatchAllService},
	})

	syncer := newSkipTestSyncer(t, api)

	_, result, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, int32(0), api.putCount.Load(),
		"an unchanged ingress document must not be rewritten")
}

func TestSyncAllRoutes_WritesWhenDocumentDiffers(t *testing.T) {
	t.Parallel()

	// The deployed config carries a stale rule the desired (no-routes)
	// document does not: the sync must write the corrected document.
	api := newFakeTunnelAPI(t, []map[string]any{
		{"hostname": "stale.example.com", "service": "http://stale.default.svc.cluster.local:80"},
		{"service": ingress.CatchAllService},
	})

	syncer := newSkipTestSyncer(t, api)

	_, result, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, int32(1), api.putCount.Load(),
		"a differing ingress document must be written")
}

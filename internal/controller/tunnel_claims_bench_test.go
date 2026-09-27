package controller

import (
	"fmt"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// BenchmarkCollectTunnelClaims measures one rebuild of the tunnel claim set, as
// every arbitrating reconcile does, for a cluster with N dedicated data planes.
// Cloudflare verification is stubbed out: this measures the Kubernetes reads
// and the per-Gateway resolve, which is what repeats on every pass. A Secret
// write that fans out to every managed Gateway costs N of these.
//
// The fake client copies objects through the scheme on every read, which is
// more work than the informer cache's DeepCopy, so the numbers are an upper
// bound on the production cost.
func BenchmarkCollectTunnelClaims(b *testing.B) {
	for _, planes := range []int{10, 100, 500} {
		b.Run(strconv.Itoa(planes), func(b *testing.B) {
			fakeClient := claimsBenchClient(b, planes)
			resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(), verifiedClaims())

			b.ReportAllocs()

			for b.Loop() {
				if claims := claimsFromCluster(b, fakeClient, resolver); len(claims) != planes {
					b.Fatalf("collected %d claims", len(claims))
				}
			}
		})
	}
}

func claimsBenchClient(b *testing.B, planes int) client.Client {
	b.Helper()

	objects := make([]client.Object, 0, 3+3*planes)
	objects = append(objects, claimsGatewayClass(), claimsClassConfig(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
		Data:       map[string][]byte{"api-token": []byte("test-token")},
	})

	for i := range planes {
		namespace := fmt.Sprintf("team-%d", i)
		tunnelID := fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1)

		objects = append(objects,
			claimsGateway(namespace, "gw", 0, "token"),
			claimsGatewayConfig(namespace, "token"),
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "token", Namespace: namespace},
				Data:       map[string][]byte{"tunnel-token": []byte(infraTunnelTokenFor(b, tunnelID))},
			},
		)
	}

	return setupGatewayFakeClient(objects...)
}

package controller_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/controller"
)

// TestProxySyncer_ResyncAllPartitions_ReplaysConcurrently pins that one
// partition's slow connector does not hold back the replay to every other
// partition. Every replica blocks its replay until all of them have received
// one, so a sequential walk stalls on the first and never completes the set.
func TestProxySyncer_ResyncAllPartitions_ReplaysConcurrently(t *testing.T) {
	t.Parallel()

	const partitions = 3

	var gated atomic.Bool

	arrivals := make(chan struct{}, partitions)
	release := make(chan struct{})

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	syncer := controller.NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	ctx := context.Background()

	for i := range partitions {
		replica := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
			if gated.Load() {
				arrivals <- struct{}{}

				select {
				case <-release:
				case <-req.Context().Done():
				}
			}

			writer.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(replica.Close)

		name := fmt.Sprintf("tenant-%d", i)
		routes := []*gatewayv1.HTTPRoute{partitionTestRoute(name, name+".example.com", "svc")}

		_, err := syncer.SyncPartition(ctx, 0, "default/"+name, "",
			[]string{replica.URL + "/config"}, routes, nil, nil, nil)
		require.NoError(t, err, "seeding push must succeed")
	}

	gated.Store(true)

	done := make(chan error, 1)

	go func() { done <- syncer.ResyncAllPartitions(ctx) }()

	// Released on every exit path, so a failed assertion does not leave the
	// handlers blocked until their push timeout.
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	timeout := time.After(3 * time.Second)

	for arrived := range partitions {
		select {
		case <-arrivals:
		case <-timeout:
			t.Fatalf("only %d of %d partitions received a replay while the others were in flight", arrived, partitions)
		}
	}

	close(release)
	require.NoError(t, <-done)
}

// TestProxySyncer_ResyncAllPartitions_ReportsEveryFailure pins that running the
// replays concurrently still returns each partition's failure to the caller.
func TestProxySyncer_ResyncAllPartitions_ReportsEveryFailure(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool

	testClient := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	syncer := controller.NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	ctx := context.Background()

	names := []string{"tenant-a", "tenant-b"}

	for _, name := range names {
		replica := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			if failing.Load() {
				writer.WriteHeader(http.StatusInternalServerError)

				return
			}

			writer.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(replica.Close)

		routes := []*gatewayv1.HTTPRoute{partitionTestRoute(name, name+".example.com", "svc")}

		_, err := syncer.SyncPartition(ctx, 0, "default/"+name, "",
			[]string{replica.URL + "/config"}, routes, nil, nil, nil)
		require.NoError(t, err, "seeding push must succeed")
	}

	failing.Store(true)

	err := syncer.ResyncAllPartitions(ctx)
	require.Error(t, err)

	assert.Equal(t, len(names), strings.Count(err.Error(), "failed to resync config"),
		"every failed partition must be reported")
}

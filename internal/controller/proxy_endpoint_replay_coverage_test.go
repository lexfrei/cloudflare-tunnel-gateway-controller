package controller

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

// proxyFirstConfigWait mirrors the proxy's two-minute wait for its first
// config: a replay retry must come back well inside it.
const proxyFirstConfigWait = 2 * time.Minute

// missingPodAddress is a pod the EndpointSlice lists and DNS does not return.
const missingPodAddress = "192.0.2.10"

// staleDNSLookup answers every lookup with the replica's loopback address only,
// the way cluster DNS answers for a moment after a new pod joined the slice.
func staleDNSLookup(context.Context, string) ([]string, error) {
	return []string{"127.0.0.1"}, nil
}

// serviceEndpoint rewrites a replica URL onto a Service host name, so the push
// goes through the lookup instead of dialing the literal address.
func serviceEndpoint(t *testing.T, replicaURL string) string {
	t.Helper()

	parsed, err := url.Parse(replicaURL)
	require.NoError(t, err)

	_, port, err := net.SplitHostPort(parsed.Host)
	require.NoError(t, err)

	parsed.Host = net.JoinHostPort("proxy-config.system.svc", port)

	return parsed.String()
}

func sliceEndpoint(address string, terminating bool) discoveryv1.Endpoint {
	return discoveryv1.Endpoint{
		Addresses:  []string{address},
		Conditions: discoveryv1.EndpointConditions{Terminating: &terminating},
	}
}

func proxySlice(namespace string, labels map[string]string, endpoints ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "es", Namespace: namespace, Labels: labels},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   endpoints,
	}
}

// typedSlice overrides the slice's address type when addressType is set.
func typedSlice(slice *discoveryv1.EndpointSlice, addressType discoveryv1.AddressType) *discoveryv1.EndpointSlice {
	if addressType != "" {
		slice.AddressType = addressType
	}

	return slice
}

func reconcileSlice(t *testing.T, reconciler *ProxyEndpointReconciler, namespace string) (ctrl.Result, error) {
	t.Helper()

	return reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "es", Namespace: namespace},
	})
}

func replayScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, discoveryv1.AddToScheme(scheme))
	require.NoError(t, gatewayv1.Install(scheme))

	return scheme
}

// TestProxyEndpointReconcile_RequeuesWhenDNSMissesASlicePod covers a replay
// that ran while DNS still returned only the old pods. It succeeded against
// every address it resolved, but the new pod listed in the EndpointSlice got
// nothing, so the reconcile must come back soon rather than wait for a slice
// change that a NotReady pod will not cause.
func TestProxyEndpointReconcile_RequeuesWhenDNSMissesASlicePod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		addressType discoveryv1.AddressType
		endpoints   []discoveryv1.Endpoint
		wantRequeue bool
	}{
		{
			name:        "a slice pod DNS did not return",
			endpoints:   []discoveryv1.Endpoint{sliceEndpoint("127.0.0.1", false), sliceEndpoint(missingPodAddress, false)},
			wantRequeue: true,
		},
		{
			name:      "every slice pod resolved",
			endpoints: []discoveryv1.Endpoint{sliceEndpoint("127.0.0.1", false)},
		},
		{
			name:      "the pod DNS did not return is terminating",
			endpoints: []discoveryv1.Endpoint{sliceEndpoint("127.0.0.1", false), sliceEndpoint(missingPodAddress, true)},
		},
		{
			// Mid-rollout, stale DNS returns only the old pod, already
			// terminating. It is still a slice address, so the check runs and
			// finds the new pod missing.
			name: "DNS returns only a terminating pod",
			endpoints: []discoveryv1.Endpoint{
				sliceEndpoint("127.0.0.1", true),
				sliceEndpoint(missingPodAddress, false),
			},
			wantRequeue: true,
		},
		{
			// Only a Service without publishNotReadyAddresses marks a pod not
			// ready, and DNS then leaves that pod out on purpose.
			name: "the pod DNS did not return is not ready",
			endpoints: []discoveryv1.Endpoint{
				sliceEndpoint("127.0.0.1", false),
				{
					Addresses:  []string{missingPodAddress},
					Conditions: discoveryv1.EndpointConditions{Ready: new(false), Terminating: new(false)},
				},
			},
		},
		{
			name: "a pod with no terminating condition is not terminating",
			endpoints: []discoveryv1.Endpoint{
				sliceEndpoint("127.0.0.1", false),
				{Addresses: []string{missingPodAddress}},
			},
			wantRequeue: true,
		},
		{
			// A ClusterIP Service name resolves to its virtual address, which
			// never matches a pod address, so checking it would requeue forever.
			name:      "the name resolves to no slice pod",
			endpoints: []discoveryv1.Endpoint{sliceEndpoint("198.51.100.7", false)},
		},
		{
			// A host name never matches a resolved address, so checking it
			// would requeue forever.
			name:        "an FQDN slice is not checked",
			addressType: discoveryv1.AddressTypeFQDN,
			endpoints:   []discoveryv1.Endpoint{sliceEndpoint("proxy-0.example", false)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			replica := newRaceReplica(t)
			endpoint := serviceEndpoint(t, replica.endpoint())

			testClient := fake.NewClientBuilder().WithScheme(replayScheme(t)).
				WithObjects(typedSlice(proxySlice("system", nil, tt.endpoints...), tt.addressType)).Build()
			syncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
			syncer.lookupHost = staleDNSLookup

			_, err := syncer.SyncRoutes(context.Background(), 0, []string{endpoint},
				[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)
			require.NoError(t, err)

			reconciler := &ProxyEndpointReconciler{Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{endpoint}}

			result, err := reconcileSlice(t, reconciler, "system")
			require.NoError(t, err)

			if tt.wantRequeue {
				assert.Positive(t, result.RequeueAfter, "a slice pod the replay missed must be retried")
				assert.Less(t, result.RequeueAfter, proxyFirstConfigWait)
			} else {
				assert.Zero(t, result.RequeueAfter)
			}
		})
	}
}

// TestProxyEndpointReconcile_MissedPodLineCarriesTheComponent pins that the
// line an operator greps for when a replay misses a pod names the
// reconciler that wrote it.
func TestProxyEndpointReconcile_MissedPodLineCarriesTheComponent(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)
	endpoint := serviceEndpoint(t, replica.endpoint())

	testClient := fake.NewClientBuilder().WithScheme(replayScheme(t)).
		WithObjects(proxySlice("system", nil, sliceEndpoint("127.0.0.1", false), sliceEndpoint(missingPodAddress, false))).Build()
	syncer := NewProxySyncer("cluster.local", "", "", testClient, slog.New(slog.DiscardHandler))
	syncer.lookupHost = staleDNSLookup

	_, err := syncer.SyncRoutes(context.Background(), 0, []string{endpoint},
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)
	require.NoError(t, err)

	var logs strings.Builder

	ctx := logging.WithLogger(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)))
	reconciler := &ProxyEndpointReconciler{Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{endpoint}}

	_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "es", Namespace: "system"}})
	require.NoError(t, err)

	var missedLine string

	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "proxy config replay missed pods") {
			missedLine = line
		}
	}

	require.NotEmpty(t, missedLine, "the missed-pod line is logged")
	assert.Contains(t, missedLine, "component=proxy-endpoint-reconciler")
}

// TestProxyEndpointReconcile_PerGatewayRequeuesWhenDNSMissesASlicePod is the
// same check on a per-Gateway data plane's EndpointSlice.
func TestProxyEndpointReconcile_PerGatewayRequeuesWhenDNSMissesASlicePod(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)
	endpoint := serviceEndpoint(t, replica.endpoint())

	objects := []client.Object{
		&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "team-a"}},
		proxySlice("team-a", map[string]string{render.GatewayLabel: render.GatewayLabelValue("gw")},
			sliceEndpoint("127.0.0.1", false), sliceEndpoint(missingPodAddress, false)),
	}
	testClient := fake.NewClientBuilder().WithScheme(replayScheme(t)).WithObjects(objects...).Build()
	syncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	syncer.lookupHost = staleDNSLookup

	seedPartition(t, syncer, endpoint)

	result, err := reconcileSlice(t, &ProxyEndpointReconciler{Client: testClient, ProxySyncer: syncer}, "team-a")
	require.NoError(t, err)
	assert.Positive(t, result.RequeueAfter, "a slice pod the replay missed must be retried")
	assert.Less(t, result.RequeueAfter, proxyFirstConfigWait)
}

var errLookupTimedOut = errors.New("lookup timed out")

// failingLookup stands in for a DNS lookup that times out or answers NXDOMAIN.
func failingLookup(context.Context, string) ([]string, error) {
	return nil, errLookupTimedOut
}

// localhostEndpoint names the replica by a host name the system resolver can
// still resolve on its own, so a push that fell back to the unresolved name
// would reach it.
func localhostEndpoint(t *testing.T, replicaURL string) string {
	t.Helper()

	parsed, err := url.Parse(replicaURL)
	require.NoError(t, err)

	_, port, err := net.SplitHostPort(parsed.Host)
	require.NoError(t, err)

	parsed.Host = net.JoinHostPort("localhost", port)

	return parsed.String()
}

// TestProxyEndpointReconcile_FailedLookupIsAReplayError covers a lookup of the
// proxy Service name that fails. Pushing to the unresolved name would let the
// dialer resolve it to one pod and count that as a replay to all of them, and
// the slice check cannot see it, so the replay must fail instead and retry.
func TestProxyEndpointReconcile_FailedLookupIsAReplayError(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)
	endpoint := localhostEndpoint(t, replica.endpoint())

	testClient := fake.NewClientBuilder().WithScheme(replayScheme(t)).
		WithObjects(proxySlice("system", nil, sliceEndpoint("127.0.0.1", false), sliceEndpoint(missingPodAddress, false))).Build()
	syncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	syncer.lookupHost = staleDNSLookup

	_, err := syncer.SyncRoutes(context.Background(), 0, []string{endpoint},
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)
	require.NoError(t, err)

	var puts atomic.Int32

	replica.setOnPut(func() { puts.Add(1) })

	syncer.lookupHost = failingLookup

	reconciler := &ProxyEndpointReconciler{Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{endpoint}}

	_, err = reconcileSlice(t, reconciler, "system")
	require.Error(t, err, "a failed lookup must fail the replay")
	assert.ErrorIs(t, err, errLookupTimedOut)
	assert.Zero(t, puts.Load(), "nothing may be pushed to the unresolved name")
}

// TestSyncPartition_FailedLookupIsAPushError pins the same rule on the full
// sync, which resolves its endpoints the same way.
func TestSyncPartition_FailedLookupIsAPushError(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)

	var puts atomic.Int32

	replica.setOnPut(func() { puts.Add(1) })

	syncer := newReplaySyncer()
	syncer.lookupHost = failingLookup

	_, err := syncer.SyncPartition(context.Background(), 0, replayRaceKey, "",
		[]string{localhostEndpoint(t, replica.endpoint())},
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)
	require.ErrorIs(t, err, errLookupTimedOut)
	assert.Zero(t, puts.Load(), "nothing may be pushed to the unresolved name")
}

// TestSyncPartition_FailedLookupIsAPushErrorOverTLS pins the same rule on the
// TLS push. Port 1 has no listener, so a dial would fail with a connection
// error rather than the lookup's.
func TestSyncPartition_FailedLookupIsAPushErrorOverTLS(t *testing.T) {
	t.Parallel()

	syncer := NewProxySyncer("cluster.local", "token", "", fake.NewClientBuilder().Build(),
		slog.New(slog.DiscardHandler), WithConfigAPIAuthority(testAuthority(t)))
	syncer.lookupHost = failingLookup

	_, err := syncer.SyncPartition(context.Background(), 0, replayRaceKey, "token",
		[]string{"https://localhost:1/config"},
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)
	require.ErrorIs(t, err, errLookupTimedOut)
}

// TestResolveEndpoints_IPLiteralNeedsNoLookup pins that an endpoint naming an
// address is pushed to as is, whatever the resolver would say.
func TestResolveEndpoints_IPLiteralNeedsNoLookup(t *testing.T) {
	t.Parallel()

	resolved := resolveEndpoints(context.Background(), failingLookup, []string{"http://127.0.0.1:8081/config"})

	require.Len(t, resolved, 1)
	assert.Equal(t, "http://127.0.0.1:8081/config", resolved[0].url)
	assert.NoError(t, resolved[0].err)
}

// TestResolveEndpoints_EmptyAnswerIsAFailedLookup pins that a lookup answering
// with no addresses fails the endpoint like a lookup error does.
func TestResolveEndpoints_EmptyAnswerIsAFailedLookup(t *testing.T) {
	t.Parallel()

	empty := func(context.Context, string) ([]string, error) { return nil, nil }

	resolved := resolveEndpoints(context.Background(), empty, []string{"http://proxy-config.system.svc:8081/config"})

	require.Len(t, resolved, 1)
	assert.ErrorIs(t, resolved[0].err, errNoAddresses)
}

// TestProxyEndpointReconcile_FailedReplayKeepsTheError pins that a replay a
// replica refused or never answered stays a reconcile error, so it reaches the
// controller's error metric and its retry follows the capped backoff.
func TestProxyEndpointReconcile_FailedReplayKeepsTheError(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)

	testClient := fake.NewClientBuilder().WithScheme(replayScheme(t)).
		WithObjects(proxySlice("system", nil, sliceEndpoint("127.0.0.1", false))).Build()
	syncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())

	_, err := syncer.SyncRoutes(context.Background(), 0, []string{replica.endpoint()},
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)
	require.NoError(t, err)

	replica.failing.Store(true)

	reconciler := &ProxyEndpointReconciler{Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{replica.endpoint()}}

	result, err := reconcileSlice(t, reconciler, "system")
	require.Error(t, err)
	assert.Zero(t, result.RequeueAfter, "the retry of a failure belongs to the rate limiter")
}

// TestReplayRateLimiter_CapsTheBackoffInsideTheProxyWait pins the retry of a
// failed replay. A new pod joins the slice as soon as it has an address, often
// before its image is pulled, and it causes no further slice change while it
// waits NotReady for its config. The retries must therefore keep coming inside
// the proxy's two-minute wait, where controller-runtime's default backoff
// grows to minutes.
func TestReplayRateLimiter_CapsTheBackoffInsideTheProxyWait(t *testing.T) {
	t.Parallel()

	limiter := replayRateLimiter()
	item := ctrl.Request{NamespacedName: types.NamespacedName{Name: "es", Namespace: "system"}}

	assert.Equal(t, replayRetryBaseDelay, limiter.When(item), "the first retry comes quickly")

	var last time.Duration
	for range 40 {
		last = limiter.When(item)
	}

	assert.Equal(t, replayRetryDelay, last, "later retries stay at the cap")
	assert.Less(t, replayRetryDelay, proxyFirstConfigWait)
}

// TestReplayControllerOptions_CarryTheCappedLimiter pins the options
// SetupWithManager registers the controller with. Without them the controller
// falls back to controller-runtime's default, whose backoff reaches 1000s.
// Dropping only the WithOptions call leaves SetupWithManager's options variable
// unused, which does not compile; removing that variable too goes uncaught here.
func TestReplayControllerOptions_CarryTheCappedLimiter(t *testing.T) {
	t.Parallel()

	limiter := replayControllerOptions().RateLimiter
	require.NotNil(t, limiter)

	item := ctrl.Request{NamespacedName: types.NamespacedName{Name: "es", Namespace: "system"}}

	var last time.Duration
	for range 40 {
		last = limiter.When(item)
	}

	assert.Equal(t, replayRetryDelay, last)
}

// TestReplayRateLimiter_KeepsTheControllerWideBucket pins the other half of
// controller-runtime's default limiter: past its burst, a bucket shared by
// all slices delays a retry whatever that slice's own backoff says. The bucket
// under test refills once an hour, so no refill can land between the calls
// and the result does not depend on how fast they run.
func TestReplayRateLimiter_KeepsTheControllerWideBucket(t *testing.T) {
	t.Parallel()

	const burst = 3

	limiter := replayRateLimiterWith(rate.NewLimiter(rate.Every(time.Hour), burst))
	request := func(i int) ctrl.Request {
		return ctrl.Request{NamespacedName: types.NamespacedName{Name: fmt.Sprintf("es-%d", i), Namespace: "system"}}
	}

	for i := range burst {
		assert.Equal(t, replayRetryBaseDelay, limiter.When(request(i)), "the burst passes at the per-item delay")
	}

	assert.Greater(t, limiter.When(request(burst)), replayRetryDelay,
		"past the burst the shared bucket delays a slice's first retry")
}

// TestNewReplayBucket_MatchesControllerRuntimeDefault pins the shared bucket's
// size to controller-runtime's default: 10 retries per second, burst of 100.
func TestNewReplayBucket_MatchesControllerRuntimeDefault(t *testing.T) {
	t.Parallel()

	bucket := newReplayBucket()

	assert.Equal(t, rate.Limit(10), bucket.Limit())
	assert.Equal(t, 100, bucket.Burst())
}

// TestSliceCoverage_ComparesAddressesNotSpellings pins that an IPv6 pod
// address written differently in the EndpointSlice and in the DNS answer still
// counts as reached.
func TestSliceCoverage_ComparesAddressesNotSpellings(t *testing.T) {
	t.Parallel()

	resolved := []pushEndpoint{
		{url: "http://[2001:0db8::0001]:8081/config"},
		{url: "http://127.0.0.1:8081/config"},
	}

	reached := &sliceCoverage{want: []string{"2001:0db8:0:0::1", "127.0.0.1"}, known: []string{"2001:0db8:0:0::1", "127.0.0.1"}}
	assert.Empty(t, reached.missedBy(resolved))

	// The only slice address DNS returned is spelled differently, so the
	// check runs and finds the pod DNS did not return.
	missed := &sliceCoverage{want: []string{"2001:0db8:0:0::1", "2001:db8::2"}, known: []string{"2001:0db8:0:0::1", "2001:db8::2"}}
	assert.Equal(t, []string{"2001:db8::2"}, missed.missedBy(resolved))
}

// nxdomainLookup answers the way cluster DNS does for a headless Service with
// no pods: the name does not exist.
func nxdomainLookup(_ context.Context, host string) ([]string, error) {
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// TestProxyEndpointReconcile_ScaledToZero pins a data plane with no pods, a
// normal state: its Service name does not resolve, and with no pod in the
// slice there is nothing to push, so the replay succeeds. A name that does
// not resolve while the slice lists pods, or a lookup that fails for another
// reason, is still an error.
func TestProxyEndpointReconcile_ScaledToZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		lookup    hostLookup
		endpoints []discoveryv1.Endpoint
		wantErr   bool
	}{
		{name: "no pods and no such host", lookup: nxdomainLookup},
		{
			name:      "only a terminating pod and no such host",
			lookup:    nxdomainLookup,
			endpoints: []discoveryv1.Endpoint{sliceEndpoint(missingPodAddress, true)},
		},
		{
			name:      "a pod and no such host",
			lookup:    nxdomainLookup,
			endpoints: []discoveryv1.Endpoint{sliceEndpoint(missingPodAddress, false)},
			wantErr:   true,
		},
		{name: "no pods and a lookup that timed out", lookup: failingLookup, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			replica := newRaceReplica(t)
			endpoint := serviceEndpoint(t, replica.endpoint())

			testClient := fake.NewClientBuilder().WithScheme(replayScheme(t)).
				WithObjects(proxySlice("system", serviceLabel("proxy-config"), tt.endpoints...)).Build()
			syncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
			syncer.lookupHost = staleDNSLookup

			_, err := syncer.SyncRoutes(context.Background(), 0, []string{endpoint},
				[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)
			require.NoError(t, err)

			syncer.lookupHost = tt.lookup

			reconciler := &ProxyEndpointReconciler{Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{endpoint}}

			result, err := reconcileSlice(t, reconciler, "system")
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Zero(t, result.RequeueAfter)
		})
	}
}

// serviceLabel marks a slice as belonging to the named Service.
func serviceLabel(name string) map[string]string {
	return map[string]string{discoveryv1.LabelServiceName: name}
}

// TestProxyEndpointReconcile_ScaledToZeroIsPerService pins that only the
// Service the empty slice belongs to counts as scaled to zero. With two
// --proxy-endpoints Services, a name that does not resolve for the other one
// is still a failed push.
func TestProxyEndpointReconcile_ScaledToZeroIsPerService(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		lookup  hostLookup
		wantErr bool
	}{
		{name: "the other Service does not resolve either", lookup: nxdomainLookup, wantErr: true},
		{
			name: "the other Service resolves",
			lookup: func(ctx context.Context, host string) ([]string, error) {
				if strings.HasPrefix(host, "proxy-a.") {
					return nxdomainLookup(ctx, host)
				}

				return staleDNSLookup(ctx, host)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			endpointA := namedServiceEndpoint(t, "proxy-a", newRaceReplica(t).endpoint())
			endpointB := namedServiceEndpoint(t, "proxy-b", newRaceReplica(t).endpoint())
			endpoints := []string{endpointA, endpointB}

			testClient := fake.NewClientBuilder().WithScheme(replayScheme(t)).
				WithObjects(proxySlice("system", serviceLabel("proxy-a"))).Build()
			syncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
			syncer.lookupHost = staleDNSLookup

			_, err := syncer.SyncRoutes(context.Background(), 0, endpoints,
				[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)
			require.NoError(t, err)

			syncer.lookupHost = tt.lookup

			reconciler := &ProxyEndpointReconciler{Client: testClient, ProxySyncer: syncer, ProxyEndpoints: endpoints}

			_, err = reconcileSlice(t, reconciler, "system")
			if tt.wantErr {
				require.Error(t, err, "proxy-b is not the Service scaled to zero")

				return
			}

			require.NoError(t, err)
		})
	}
}

// namedServiceEndpoint rewrites a replica URL onto the named Service in the
// system namespace, keeping the replica's port.
func namedServiceEndpoint(t *testing.T, service, replicaURL string) string {
	t.Helper()

	parsed, err := url.Parse(replicaURL)
	require.NoError(t, err)

	_, port, err := net.SplitHostPort(parsed.Host)
	require.NoError(t, err)

	parsed.Host = net.JoinHostPort(service+".system.svc", port)

	return parsed.String()
}

// TestProxyEndpointReconcile_ChecksEverySliceOfTheService covers a proxy
// Service split over several EndpointSlices: the replay one slice triggers is
// checked against the pods every slice of the Service lists, so a pod DNS does
// not return yet is noticed whichever slice holds it, and a plane whose pods
// are all in another slice is not taken for one scaled to zero. A slice of
// another Service or of another address family does not count.
func TestProxyEndpointReconcile_ChecksEverySliceOfTheService(t *testing.T) {
	t.Parallel()

	const missingIPv6Pod = "2001:db8::10"

	tests := []struct {
		name         string
		trigger      *discoveryv1.EndpointSlice
		otherService string
		otherType    discoveryv1.AddressType
		other        []discoveryv1.Endpoint
		lookup       hostLookup
		wantRequeue  bool
		wantErr      bool
	}{
		{
			name:         "the missed pod is in the triggering slice, the stale pods in another",
			trigger:      proxySlice("system", serviceLabel("proxy-config"), sliceEndpoint(missingPodAddress, false)),
			otherService: "proxy-config",
			other:        []discoveryv1.Endpoint{sliceEndpoint("127.0.0.1", false)},
			wantRequeue:  true,
		},
		{
			name:         "the missed pod is in another slice of the Service",
			trigger:      proxySlice("system", serviceLabel("proxy-config"), sliceEndpoint("127.0.0.1", false)),
			otherService: "proxy-config",
			other:        []discoveryv1.Endpoint{sliceEndpoint(missingPodAddress, false)},
			wantRequeue:  true,
		},
		{
			name:         "the stale pods are in a slice of another Service",
			trigger:      proxySlice("system", serviceLabel("proxy-config"), sliceEndpoint(missingPodAddress, false)),
			otherService: "unrelated",
			other:        []discoveryv1.Endpoint{sliceEndpoint("127.0.0.1", false)},
		},
		{
			name: "the stale pods are in a slice of another address family",
			trigger: typedSlice(proxySlice("system", serviceLabel("proxy-config"),
				sliceEndpoint(missingIPv6Pod, false)), discoveryv1.AddressTypeIPv6),
			otherService: "proxy-config",
			other:        []discoveryv1.Endpoint{sliceEndpoint("127.0.0.1", false)},
		},
		{
			name:         "the plane's pods are all in another slice and its name does not resolve",
			trigger:      proxySlice("system", serviceLabel("proxy-config")),
			otherService: "proxy-config",
			other:        []discoveryv1.Endpoint{sliceEndpoint(missingPodAddress, false)},
			lookup:       nxdomainLookup,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			replica := newRaceReplica(t)
			endpoint := serviceEndpoint(t, replica.endpoint())

			other := typedSlice(proxySlice("system", serviceLabel(tt.otherService), tt.other...), tt.otherType)
			other.Name = "es-other"

			testClient := fake.NewClientBuilder().WithScheme(replayScheme(t)).WithObjects(tt.trigger, other).Build()
			syncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
			syncer.lookupHost = staleDNSLookup

			_, err := syncer.SyncRoutes(context.Background(), 0, []string{endpoint},
				[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)
			require.NoError(t, err)

			if tt.lookup != nil {
				syncer.lookupHost = tt.lookup
			}

			reconciler := &ProxyEndpointReconciler{Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{endpoint}}

			result, err := reconcileSlice(t, reconciler, "system")
			if tt.wantErr {
				require.Error(t, err, "a plane with pods in another slice is not scaled to zero")

				return
			}

			require.NoError(t, err)

			if tt.wantRequeue {
				assert.Positive(t, result.RequeueAfter, "the pod the replay missed must be retried")
				assert.Less(t, result.RequeueAfter, proxyFirstConfigWait)
			} else {
				assert.Zero(t, result.RequeueAfter)
			}
		})
	}
}

var errSliceListUnavailable = errors.New("EndpointSlice list unavailable")

// TestProxyEndpointReconcile_UnlistableSiblingsCheckTheTriggeringSlice pins the
// fallback when the other slices of the Service cannot be listed: the replay is
// still checked against the triggering slice rather than failing or skipping it.
func TestProxyEndpointReconcile_UnlistableSiblingsCheckTheTriggeringSlice(t *testing.T) {
	t.Parallel()

	replica := newRaceReplica(t)
	endpoint := serviceEndpoint(t, replica.endpoint())

	testClient := fake.NewClientBuilder().WithScheme(replayScheme(t)).
		WithObjects(proxySlice("system", serviceLabel("proxy-config"),
			sliceEndpoint("127.0.0.1", false), sliceEndpoint(missingPodAddress, false))).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*discoveryv1.EndpointSliceList); ok {
					return errSliceListUnavailable
				}

				return c.List(ctx, list, opts...)
			},
		}).Build()
	syncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	syncer.lookupHost = staleDNSLookup

	_, err := syncer.SyncRoutes(context.Background(), 0, []string{endpoint},
		[]*gatewayv1.HTTPRoute{pushFallbackRoute("r-a", "a.example.com")}, nil, nil, nil)
	require.NoError(t, err)

	reconciler := &ProxyEndpointReconciler{Client: testClient, ProxySyncer: syncer, ProxyEndpoints: []string{endpoint}}

	result, err := reconcileSlice(t, reconciler, "system")
	require.NoError(t, err)
	assert.Positive(t, result.RequeueAfter, "the pod the replay missed must be retried")
}

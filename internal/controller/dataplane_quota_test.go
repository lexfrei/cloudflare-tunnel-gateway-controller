package controller

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// quotaEpoch anchors the fixture creation times. Only their ORDER matters.
var quotaEpoch = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// quotaGateway builds one opted-in Gateway plus the GatewayConfig and
// connector-token Secret behind its parametersRef. ageRank orders it against
// its siblings: rank 0 is the oldest, and therefore the first admitted.
//
// Gateways in one namespace share a tunnel deliberately — same-namespace claims
// never reject each other, so nothing here trips the tunnel-ownership rule and
// a refusal can only have come from the cap.
func quotaGateway(t *testing.T, namespace, name string, ageRank int, tunnelID string) []client.Object {
	t.Helper()

	return []client.Object{
		&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         namespace,
				UID:               types.UID(namespace + "/" + name),
				CreationTimestamp: metav1.NewTime(quotaEpoch.Add(time.Duration(ageRank) * time.Hour)),
			},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: "cloudflare-tunnel",
				Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: "HTTP"}},
				Infrastructure: &gatewayv1.GatewayInfrastructure{
					ParametersRef: &gatewayv1.LocalParametersReference{
						Group: "cf.k8s.lex.la", Kind: "GatewayConfig", Name: name + "-config",
					},
				},
			},
		},
		&v1alpha1.GatewayConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-config", Namespace: namespace},
			Spec: v1alpha1.GatewayConfigSpec{
				TunnelTokenSecretRef: v1alpha1.LocalSecretReference{Name: name + "-token"},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-token", Namespace: namespace},
			Data:       map[string][]byte{"tunnel-token": []byte(infraTunnelTokenFor(t, tunnelID))},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cf-proxy-" + name + "-auth", Namespace: namespace},
			Data:       map[string][]byte{"auth-token": []byte("generated-bearer")},
		},
	}
}

// quotaFixtures builds the managed GatewayClass chain carrying the cap, three
// opted-in Gateways in "tenant" and one in "neighbour".
func quotaFixtures(t *testing.T, capacity *int32) client.WithWatch {
	t.Helper()

	const (
		tenantTunnel    = "550e8400-e29b-41d4-a716-446655440000"
		neighbourTunnel = "660e8400-e29b-41d4-a716-446655440000"
	)

	objects := make([]client.Object, 0, 19)
	objects = append(objects,
		&gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: "test-controller",
				ParametersRef: &gatewayv1.ParametersReference{
					Group: config.ParametersRefGroup, Kind: config.ParametersRefKind, Name: "test-config",
				},
			},
		},
		&v1alpha1.GatewayClassConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "test-config"},
			Spec: v1alpha1.GatewayClassConfigSpec{
				CloudflareCredentialsSecretRef: v1alpha1.SecretReference{Name: "cf-credentials", Namespace: "default"},
				TunnelID:                       "12345678-1234-1234-1234-123456789abc",
				MaxDataPlanesPerNamespace:      capacity,
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cf-credentials", Namespace: "default"},
			Data:       map[string][]byte{"api-token": []byte("token")},
		},
	)

	objects = append(objects, quotaGateway(t, "tenant", "gw-old", 0, tenantTunnel)...)
	objects = append(objects, quotaGateway(t, "tenant", "gw-mid", 1, tenantTunnel)...)
	objects = append(objects, quotaGateway(t, "tenant", "gw-new", 2, tenantTunnel)...)
	objects = append(objects, quotaGateway(t, "neighbour", "gw-solo", 3, neighbourTunnel)...)

	return setupGatewayFakeClient(objects...)
}

// reconcileQuotaGateway reconciles one Gateway and returns its updated copy.
func reconcileQuotaGateway(
	t *testing.T,
	fakeClient client.WithWatch,
	recorder events.EventRecorder,
	namespace, name string,
) gatewayv1.Gateway {
	t.Helper()

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
		ProxyImage:     "ghcr.io/example/proxy:v1.2.3",
		Recorder:       recorder,
		ViewStore:      newMergeViewStore(),
	}

	ctx := context.Background()

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	require.NoError(t, err)

	var updated gatewayv1.Gateway
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &updated))

	return updated
}

// quotaAcceptedCondition reconciles the named Gateway and returns its Accepted
// condition.
func quotaAcceptedCondition(
	t *testing.T,
	fakeClient client.WithWatch,
	namespace, name string,
) *metav1.Condition {
	t.Helper()

	updated := reconcileQuotaGateway(t, fakeClient, nil, namespace, name)

	condition := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, condition, "%s/%s must carry an Accepted condition", namespace, name)

	return condition
}

// TestGatewayReconciler_DataPlaneQuota pins what a tenant sees when they ask
// for more dedicated data planes than the operator allows: the oldest keep
// theirs, the newest are refused, and no other namespace is touched.
func TestGatewayReconciler_DataPlaneQuota(t *testing.T) {
	t.Parallel()

	t.Run("the two oldest are admitted and the newest is refused", func(t *testing.T) {
		t.Parallel()

		fakeClient := quotaFixtures(t, new(int32(2)))

		for _, name := range []string{"gw-old", "gw-mid"} {
			assert.Equal(t, metav1.ConditionTrue, quotaAcceptedCondition(t, fakeClient, "tenant", name).Status,
				"%s is within the cap and must keep its data plane", name)
		}

		refused := quotaAcceptedCondition(t, fakeClient, "tenant", "gw-new")
		assert.Equal(t, metav1.ConditionFalse, refused.Status)
		assert.Equal(t, "DataPlaneQuotaExceeded", refused.Reason)
	})

	t.Run("the refusal message names the cap and no other Gateway", func(t *testing.T) {
		t.Parallel()

		refused := quotaAcceptedCondition(t, quotaFixtures(t, new(int32(2))), "tenant", "gw-new")

		assert.Contains(t, refused.Message, "2", "the tenant must be told the cap they hit")

		// The refused tenant reads this message. Naming a sibling Gateway would
		// hand one tenant another's object names where namespaces are the
		// tenancy boundary — the same reason a tunnel refusal never names the
		// holder.
		for _, other := range []string{"gw-old", "gw-mid", "gw-solo", "neighbour"} {
			assert.NotContains(t, refused.Message, other,
				"the refusal must not name another Gateway or namespace")
		}
	})

	t.Run("a refused Gateway is not Programmed, and not for being invalid", func(t *testing.T) {
		t.Parallel()

		refused := reconcileQuotaGateway(t, quotaFixtures(t, new(int32(2))), nil, "tenant", "gw-new")

		programmed := findCondition(refused.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		require.NotNil(t, programmed)
		assert.Equal(t, metav1.ConditionFalse, programmed.Status,
			"a Gateway whose plane is never rendered must not report itself Programmed")

		// Invalid means "syntactically or semantically invalid" per the spec,
		// and a Gateway refused for capacity is neither -- the same reason
		// Accepted does not say InvalidParameters. NoResources is the spec's
		// reason for a Gateway left unscheduled because the infrastructure to
		// run it is not available, which is what a cap declares.
		assert.Equal(t, string(gatewayv1.GatewayReasonNoResources), programmed.Reason,
			"a capacity refusal must not send the operator hunting a spec mistake")
	})

	t.Run("an unresolvable Gateway still holds its slot", func(t *testing.T) {
		t.Parallel()

		// The cap's anti-bypass clause. A Gateway asks for a plane by carrying
		// spec.infrastructure.parametersRef, and counting only the ones whose
		// configuration currently RESOLVES would hand a tenant a bypass: break
		// one of your own tokens, free a slot, create another Gateway. It would
		// also make the verdict move with an unrelated Secret's availability.
		fakeClient := quotaFixtures(t, new(int32(2)))
		ctx := context.Background()

		require.NoError(t, fakeClient.Delete(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-mid-token", Namespace: "tenant"},
		}))

		refused := quotaAcceptedCondition(t, fakeClient, "tenant", "gw-new")
		assert.Equal(t, metav1.ConditionFalse, refused.Status,
			"a Gateway whose token is unreadable still consumes its slot")
		assert.Equal(t, reasonDataPlaneQuotaExceeded, refused.Reason)
	})

	t.Run("sharing tunnels does not waive the cap", func(t *testing.T) {
		t.Parallel()

		// allowSharedTunnels waives tunnel arbitration only. The opt-out lives
		// inside tunnelRejection, so hoisting it one frame up -- the shape of an
		// ordinary refactor -- would skip refuseOverQuota entirely and silently
		// disable the cap. Worst here of the three layers: status would report
		// Accepted=True while the infra reconciler still refused to render,
		// which is the divergence the shared claim set exists to prevent.
		fakeClient := quotaFixtures(t, new(int32(2)))
		ctx := context.Background()

		var classConfig v1alpha1.GatewayClassConfig
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "test-config"}, &classConfig))

		classConfig.Spec.AllowSharedTunnels = true
		require.NoError(t, fakeClient.Update(ctx, &classConfig))

		refused := quotaAcceptedCondition(t, fakeClient, "tenant", "gw-new")
		assert.Equal(t, metav1.ConditionFalse, refused.Status)
		assert.Equal(t, reasonDataPlaneQuotaExceeded, refused.Reason)
	})

	t.Run("a neighbouring namespace is unaffected by the tenant going over", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, metav1.ConditionTrue,
			quotaAcceptedCondition(t, quotaFixtures(t, new(int32(2))), "neighbour", "gw-solo").Status,
			"the cap is per namespace; another tenant exceeding it must not refuse this one")
	})

	t.Run("an unset cap admits every Gateway", func(t *testing.T) {
		t.Parallel()

		fakeClient := quotaFixtures(t, nil)

		for _, name := range []string{"gw-old", "gw-mid", "gw-new"} {
			assert.Equal(t, metav1.ConditionTrue, quotaAcceptedCondition(t, fakeClient, "tenant", name).Status,
				"%s must be admitted when the operator set no cap", name)
		}
	})

	t.Run("a zero cap refuses rather than admits", func(t *testing.T) {
		t.Parallel()

		// Minimum=1 keeps 0 out of a current cluster, so it arrives only from a
		// CRD predating the bound or a hand-edited one. Reading it as unlimited
		// there would grant the opposite of what the operator wrote.
		refused := quotaAcceptedCondition(t, quotaFixtures(t, new(int32(0))), "tenant", "gw-new")
		assert.Equal(t, metav1.ConditionFalse, refused.Status)
		assert.Equal(t, reasonDataPlaneQuotaExceeded, refused.Reason)
	})

	t.Run("dropping the parametersRef is a working escape hatch", func(t *testing.T) {
		t.Parallel()

		fakeClient := quotaFixtures(t, new(int32(2)))
		ctx := context.Background()

		// The remedy the refusal message and the docs both offer the tenant:
		// give up the dedicated plane and be served by the shared one. It has to
		// work without the operator raising the cap, and it has to free the slot
		// -- only Gateways asking for their own plane consume one.
		var refused gatewayv1.Gateway
		require.NoError(t, fakeClient.Get(ctx,
			types.NamespacedName{Name: "gw-new", Namespace: "tenant"}, &refused))

		refused.Spec.Infrastructure = nil
		require.NoError(t, fakeClient.Update(ctx, &refused))

		assert.Equal(t, metav1.ConditionTrue, quotaAcceptedCondition(t, fakeClient, "tenant", "gw-new").Status,
			"a Gateway on the shared plane consumes no slot and must be accepted")
	})

	t.Run("the refusal raises one Warning Event and does not repeat it", func(t *testing.T) {
		t.Parallel()

		fakeClient := quotaFixtures(t, new(int32(2)))
		recorder := events.NewFakeRecorder(10)

		reconcileQuotaGateway(t, fakeClient, recorder, "tenant", "gw-new")

		fired := drainEvents(recorder)
		require.Len(t, fired, 1, "the refusal must reach the operator as an Event")
		assert.Contains(t, fired[0], "DataPlaneQuotaExceeded")
		assert.Contains(t, fired[0], corev1.EventTypeWarning)

		// The refusal requeues for as long as the Gateway stands, so re-eventing
		// every pass would let the refused tenant choose the event volume.
		reconcileQuotaGateway(t, fakeClient, recorder, "tenant", "gw-new")
		assert.Empty(t, drainEvents(recorder), "an unchanged verdict must not re-event")
	})
}

// TestOverQuotaGateways pins the decision itself, away from the reconcilers
// that consume it: who is refused, and that the answer does not depend on the
// order the claims arrive in.
func TestOverQuotaGateways(t *testing.T) {
	t.Parallel()

	claim := func(namespace, name string, ageRank int, uid string) dataPlaneClaim {
		return dataPlaneClaim{
			Key:       namespace + "/" + name,
			Namespace: namespace,
			CreatedAt: quotaEpoch.Add(time.Duration(ageRank) * time.Hour),
			UID:       uid,
		}
	}

	tests := []struct {
		name     string
		capacity *int32
		claims   []dataPlaneClaim
		want     []string
	}{
		{
			name:   "no cap refuses nothing",
			claims: []dataPlaneClaim{claim("a", "one", 0, "u1"), claim("a", "two", 1, "u2")},
		},
		// Admission rejects anything below 1, so these two only arrive through a
		// hand-edited CRD. Refusing every plane is what the operator who wrote
		// the value asked for; reading it as unlimited would grant the opposite.
		{
			name:     "a zero cap refuses every plane",
			capacity: new(int32(0)),
			claims:   []dataPlaneClaim{claim("a", "one", 0, "u1"), claim("a", "two", 1, "u2")},
			want:     []string{"a/one", "a/two"},
		},
		{
			name:     "a negative cap refuses every plane rather than indexing out of range",
			capacity: new(int32(-1)),
			claims:   []dataPlaneClaim{claim("a", "one", 0, "u1")},
			want:     []string{"a/one"},
		},
		{
			name:     "under the cap refuses nothing",
			capacity: new(int32(2)),
			claims:   []dataPlaneClaim{claim("a", "one", 0, "u1")},
		},
		{
			name:     "exactly at the cap refuses nothing",
			capacity: new(int32(2)),
			claims:   []dataPlaneClaim{claim("a", "one", 0, "u1"), claim("a", "two", 1, "u2")},
		},
		{
			name:     "over the cap refuses the newest",
			capacity: new(int32(2)),
			claims: []dataPlaneClaim{
				claim("a", "one", 0, "u1"), claim("a", "two", 1, "u2"), claim("a", "three", 2, "u3"),
			},
			want: []string{"a/three"},
		},
		{
			name:     "arrival order does not change the verdict",
			capacity: new(int32(2)),
			claims: []dataPlaneClaim{
				claim("a", "three", 2, "u3"), claim("a", "one", 0, "u1"), claim("a", "two", 1, "u2"),
			},
			want: []string{"a/three"},
		},
		{
			name:     "equal timestamps break on UID, not on list order",
			capacity: new(int32(1)),
			claims:   []dataPlaneClaim{claim("a", "zed", 0, "u9"), claim("a", "amy", 0, "u1")},
			want:     []string{"a/zed"},
		},
		{
			name:     "namespaces are counted separately",
			capacity: new(int32(1)),
			claims: []dataPlaneClaim{
				claim("a", "one", 0, "u1"), claim("a", "two", 1, "u2"), claim("b", "one", 2, "u3"),
			},
			want: []string{"a/two"},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			refused := overQuotaGateways(testCase.capacity, testCase.claims)

			got := make([]string, 0, len(refused))
			for key := range refused {
				got = append(got, key)
			}

			assert.ElementsMatch(t, testCase.want, got)
		})
	}
}

// TestDataPlaneQuotaMessage pins what the refused tenant is told: the cap they
// hit, and that it is per namespace so they know deleting one of their own
// Gateways frees a slot.
func TestDataPlaneQuotaMessage(t *testing.T) {
	t.Parallel()

	message := dataPlaneQuotaMessage(3)

	assert.Contains(t, message, "3")
	assert.Contains(t, message, "namespace")
}

// TestDataPlaneQuotaMessageBelowOneOffersNoDeletion pins the advice a tenant
// gets when the cap allows no dedicated plane at all. Deleting a sibling
// Gateway frees nothing there, and this message is the only thing the refused
// tenant sees. Negatives take the same branch as zero: overQuotaGateways
// clamps them to the same refusal, so the two must not explain it differently.
func TestDataPlaneQuotaMessageBelowOneOffersNoDeletion(t *testing.T) {
	t.Parallel()

	for _, capacity := range []int32{0, -1} {
		message := dataPlaneQuotaMessage(capacity)

		assert.NotContains(t, message, "delete", "capacity %d", capacity)
		assert.NotContains(t, message, "already has", "capacity %d", capacity)
		assert.NotContains(t, message, "-1", "a cap below one must not be quoted back at the tenant")
		assert.Contains(t, message, "drop spec.infrastructure.parametersRef", "capacity %d", capacity)
	}
}

// TestDataPlaneQuotaLimitAgreesWithItsCount pins the wording of the phrase both
// tenant-facing surfaces quote, at the cap an operator wanting strict isolation
// is most likely to set.
func TestDataPlaneQuotaLimitAgreesWithItsCount(t *testing.T) {
	t.Parallel()

	assert.Contains(t, dataPlaneQuotaLimit(1), "1 Gateway with")
	assert.Contains(t, dataPlaneQuotaLimit(2), "2 Gateways with")
}

// TestDataPlaneQuotaWordingIsShared pins that the Gateway status and the route
// status quote ONE rendering of the cap. They are the two halves of what a
// refused tenant reads, and two hand-maintained copies drift.
func TestDataPlaneQuotaWordingIsShared(t *testing.T) {
	t.Parallel()

	infra := &infraGateways{overQuota: map[string]int32{"team-a/gw": 1}}

	routeErr := gatewaySyncError("team-a/gw", map[string]error{}, infra)
	require.Error(t, routeErr)

	limit := dataPlaneQuotaLimit(1)
	assert.Contains(t, routeErr.Error(), limit)
	assert.Contains(t, dataPlaneQuotaMessage(1), limit)
}

// TestDataPlaneQuotaErrorCarriesBothSentinels pins that a capacity refusal
// answers to config.ErrInvalidParameters as well as to its own sentinel.
//
// errors.Mark does not carry the reference's unwrap chain, so the two marks are
// independent. Without the second, a refusal reaching handleResolveError would
// look like a transient API failure on an opted-in Gateway and take the
// requeue-without-status branch, leaving the tenant no condition to read.
func TestDataPlaneQuotaErrorCarriesBothSentinels(t *testing.T) {
	t.Parallel()

	err := dataPlaneQuotaError(2)

	// errors.Is here is cockroachdb's, matching the consumers. A mark is
	// invisible to the standard library's Is, so assert.ErrorIs reports false
	// for an error the production branches match.
	assert.True(t, errors.Is(err, errDataPlaneQuotaExceeded),
		"the status writer keys the refusal reason on this sentinel")
	assert.True(t, errors.Is(err, config.ErrInvalidParameters),
		"every branch keyed on a deterministic spec problem must match it")
}

// TestDataPlaneQuotaMessageSurvivesConditionTruncation pins the length budget
// the refusal dedup depends on: the stored condition is
// truncateMessage("Refused: " + msg), and isRefusalReported asks whether it
// CONTAINS the rendered message. Let the message grow past the budget and the
// substring is never found again, so every requeue re-emits the Error log and
// the Warning Event at a rate the refused tenant chooses.
func TestDataPlaneQuotaMessageSurvivesConditionTruncation(t *testing.T) {
	t.Parallel()

	stored := refusedConditionPrefix + dataPlaneQuotaMessage(999999)
	assert.LessOrEqual(t, len(stored), maxConditionMessageLength,
		"a refusal message that truncates breaks the dedup that keeps the Event from repeating")
}

package controller

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// classConflictEvent drives one event through the class-conflict handler and
// returns the keys it enqueued.
func classConflictEvent(t *testing.T, deliver func(queue workqueue.TypedRateLimitingInterface[reconcile.Request])) []string {
	t.Helper()

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	deliver(queue)

	keys := make([]string, 0, queue.Len())
	for queue.Len() > 0 {
		request, _ := queue.Get()
		keys = append(keys, request.Namespace+"/"+request.Name)
		queue.Done(request)
	}

	return keys
}

// TestGatewayReconciler_ClassConflictEnqueuesEveryManagedGateway pins the
// trigger. A Gateway landing on a previously unused class changes the verdict
// of every Gateway on every other managed class, none of which was written, so
// only a handler on that Gateway's event reaches them. The same handler brings
// them back when it is deleted, and stays quiet for an edit that keeps the
// Gateway's class, and whenever the managed classes agree.
func TestGatewayReconciler_ClassConflictEnqueuesEveryManagedGateway(t *testing.T) {
	t.Parallel()

	reconcilerFor := func(cli client.Client) *GatewayReconciler {
		return &GatewayReconciler{Client: cli, ControllerName: "test-controller"}
	}

	t.Run("a Gateway puts a conflicting class in use", func(t *testing.T) {
		t.Parallel()

		fakeClient := quotaFixtures(t, nil)
		createAll(t, fakeClient, conflictingClass("test-controller"))

		keys := classConflictEvent(t, func(queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			reconcilerFor(fakeClient).classConflictHandler().Create(context.Background(),
				event.CreateEvent{Object: strayGateway()}, queue)
		})
		assert.Contains(t, keys, "tenant/gw-old")
		assert.Contains(t, keys, "neighbour/gw-solo",
			"every managed Gateway in every namespace changes verdict, not only the event's neighbours")
	})

	t.Run("the Gateway that put the class in use is deleted", func(t *testing.T) {
		t.Parallel()

		// The delete event arrives after the informer dropped the Gateway, so
		// the class already reads as unused; the handler must still bring the
		// others back.
		fakeClient := quotaFixtures(t, nil)
		createAll(t, fakeClient, withoutStray(conflictingClass("test-controller")))

		keys := classConflictEvent(t, func(queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			reconcilerFor(fakeClient).classConflictHandler().Delete(context.Background(),
				event.DeleteEvent{Object: strayGateway()}, queue)
		})
		assert.Contains(t, keys, "tenant/gw-old")
	})

	t.Run("a Gateway moves onto a conflicting class", func(t *testing.T) {
		t.Parallel()

		fakeClient := quotaFixtures(t, nil)
		createAll(t, fakeClient, conflictingClass("test-controller"))

		before := strayGateway()
		before.Spec.GatewayClassName = "cloudflare-tunnel"

		keys := classConflictEvent(t, func(queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			reconcilerFor(fakeClient).classConflictHandler().Update(context.Background(),
				event.UpdateEvent{ObjectOld: before, ObjectNew: strayGateway()}, queue)
		})
		assert.Contains(t, keys, "tenant/gw-old")
	})

	t.Run("the only Gateway on a conflicting class moves to an agreeing one", func(t *testing.T) {
		t.Parallel()

		fakeClient := quotaFixtures(t, nil)
		createAll(t, fakeClient, withoutStray(conflictingClass("test-controller")))

		after := strayGateway()
		after.Spec.GatewayClassName = "cloudflare-tunnel"
		require.NoError(t, fakeClient.Create(context.Background(), after))

		keys := classConflictEvent(t, func(queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			reconcilerFor(fakeClient).classConflictHandler().Update(context.Background(),
				event.UpdateEvent{ObjectOld: strayGateway(), ObjectNew: after}, queue)
		})
		assert.Contains(t, keys, "tenant/gw-old", "leaving the conflicting class must bring the others back")
	})

	t.Run("the only Gateway on a conflicting class is edited in place", func(t *testing.T) {
		t.Parallel()

		// Its class stays in use across the edit, so nothing flips, though a
		// judgement on the new object alone cannot tell this from a create.
		fakeClient := quotaFixtures(t, nil)
		createAll(t, fakeClient, conflictingClass("test-controller"))

		edited := strayGateway()
		edited.Spec.Listeners = append(edited.Spec.Listeners,
			gatewayv1.Listener{Name: "other", Port: 8080, Protocol: "HTTP"})

		keys := classConflictEvent(t, func(queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			reconcilerFor(fakeClient).classConflictHandler().Update(context.Background(),
				event.UpdateEvent{ObjectOld: strayGateway(), ObjectNew: edited}, queue)
		})
		assert.Empty(t, keys, "an edit that keeps the Gateway's class flips no verdict")
	})

	t.Run("two Gateways put a conflicting class in use together", func(t *testing.T) {
		t.Parallel()

		// The informer writes its cache before it delivers events, so both
		// Gateways can be cached by the time either handler runs. Each then
		// sees the other already holding the class in use.
		fakeClient := quotaFixtures(t, nil)
		createAll(t, fakeClient, conflictingClass("test-controller"))

		second := strayGateway()
		second.Name = "stray-two"
		require.NoError(t, fakeClient.Create(context.Background(), second))

		keys := classConflictEvent(t, func(queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			handler := reconcilerFor(fakeClient).classConflictHandler()
			handler.Create(context.Background(), eventCreate(strayGateway()), queue)
			handler.Create(context.Background(), eventCreate(second), queue)
		})
		assert.Contains(t, keys, "tenant/gw-old", "a conflict two Gateways start together must still reach the others")
	})

	t.Run("the managed classes agree", func(t *testing.T) {
		t.Parallel()

		fakeClient := quotaFixtures(t, nil)

		keys := classConflictEvent(t, func(queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			reconcilerFor(fakeClient).classConflictHandler().Create(context.Background(),
				eventCreate(strayGateway()), queue)
		})
		assert.Empty(t, keys, "with one GatewayClassConfig no Gateway event can change another's verdict")
	})
}

func eventCreate(gateway *gatewayv1.Gateway) event.CreateEvent {
	return event.CreateEvent{Object: gateway}
}

// TestClassConflictWatchIsRegistered pins the wiring the handler needs, which a
// direct call to it cannot reach.
func TestClassConflictWatchIsRegistered(t *testing.T) {
	t.Parallel()

	source, err := os.ReadFile("gateway_controller.go")
	require.NoError(t, err)

	packed := strings.Join(strings.Fields(string(source)), "")

	assert.Contains(t, packed,
		"Watches(&gatewayv1.Gateway{},r.classConflictHandler(),"+
			"builder.WithPredicates(predicate.GenerationChangedPredicate{}),",
		"without this watch a Gateway landing on an unused conflicting class leaves the others Accepted")
}

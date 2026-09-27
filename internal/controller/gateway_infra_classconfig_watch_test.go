package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

// TestGatewayInfraReconciler_ClassConfigWatchUpdate drives GatewayClassConfig
// UPDATE events through the infra reconciler's watch, predicates first and
// handler second. A spec edit of this controller's class config re-renders
// every opted-in Gateway; a config no managed class references, and a status
// write on our own, enqueue nothing.
func TestGatewayInfraReconciler_ClassConfigWatchUpdate(t *testing.T) {
	t.Parallel()

	classConfig := func(name string, generation int64) *v1alpha1.GatewayClassConfig {
		return &v1alpha1.GatewayClassConfig{ObjectMeta: metav1.ObjectMeta{Name: name, Generation: generation}}
	}

	withStatus := classConfig("class-config", 1)
	withStatus.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready"}}

	tests := []struct {
		name   string
		oldObj *v1alpha1.GatewayClassConfig
		newObj *v1alpha1.GatewayClassConfig
		want   []string
	}{
		{
			name:   "spec edit of our class config",
			oldObj: classConfig("class-config", 1),
			newObj: classConfig("class-config", 2),
			want:   []string{infraNamespace + "/edge", infraNamespace + "/elder"},
		},
		{
			name:   "spec edit of a config no managed class references",
			oldObj: classConfig("someone-elses-config", 1),
			newObj: classConfig("someone-elses-config", 2),
		},
		{
			name:   "status write on our class config",
			oldObj: classConfig("class-config", 1),
			newObj: withStatus,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reconciler := newInfraReconciler(t, infraQuotaFixtures(t, new(int32(1)))...)
			updateEvent := event.UpdateEvent{ObjectOld: tt.oldObj, ObjectNew: tt.newObj}

			var queued []string

			passed := true

			for _, pred := range classConfigWatchPredicates() {
				passed = passed && pred.Update(updateEvent)
			}

			if passed {
				queue := workqueue.NewTypedRateLimitingQueue(
					workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
				defer queue.ShutDown()

				handler.EnqueueRequestsFromMapFunc(reconciler.classConfigInfraGateways).
					Update(context.Background(), updateEvent, queue)

				for queue.Len() > 0 {
					item, _ := queue.Get()
					queued = append(queued, item.Namespace+"/"+item.Name)
					queue.Done(item)
				}
			}

			assert.ElementsMatch(t, tt.want, queued)
		})
	}
}

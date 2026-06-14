package render

import (
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Autoscaler renders the per-Gateway HorizontalPodAutoscaler when
// spec.autoscaling is set; nil otherwise. The HPA scales the rendered proxy
// Deployment on the data plane's in-flight request gauge as a Pods-type
// custom metric — concurrency is the saturation signal for an I/O-bound L7
// hop, not CPU. Serving the metric requires a metrics adapter
// (prometheus-adapter or KEDA) exposing it through the custom-metrics API;
// without one the HPA reports FailedGetPodsMetric and holds minReplicas —
// visible degradation, never silent.
func Autoscaler(input *Input) *autoscalingv2.HorizontalPodAutoscaler {
	auto := input.Config.Spec.Autoscaling
	if auto == nil {
		return nil
	}

	// Clamp the (possibly defaulted) min to max so the rendered object is
	// always apiserver-valid: CEL rejects the explicit min>max case, but the
	// implicit default (2) with maxReplicas below it must degrade gracefully
	// rather than produce an HPA the apiserver rejects on every reconcile.
	minReplicas := min(auto.EffectiveMinReplicas(), auto.MaxReplicas)
	target := resource.NewQuantity(int64(auto.TargetInflightPerPod), resource.DecimalSI)

	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:        DeploymentName(input.Gateway),
			Namespace:   input.Gateway.Namespace,
			Labels:      resourceLabels(input.Gateway),
			Annotations: resourceAnnotations(input.Gateway),
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       DeploymentName(input.Gateway),
			},
			MinReplicas: &minReplicas,
			MaxReplicas: auto.MaxReplicas,
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.PodsMetricSourceType,
					Pods: &autoscalingv2.PodsMetricSource{
						Metric: autoscalingv2.MetricIdentifier{Name: auto.EffectiveMetricName()},
						Target: autoscalingv2.MetricTarget{
							Type:         autoscalingv2.AverageValueMetricType,
							AverageValue: target,
						},
					},
				},
			},
		},
	}
}

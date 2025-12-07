package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

const (
	// ConditionTypeValid indicates whether the GatewayClassConfig is valid.
	ConditionTypeValid = "Valid"

	// ConditionTypeSecretsResolved indicates whether all referenced secrets exist.
	ConditionTypeSecretsResolved = "SecretsResolved"

	// configValidationRequeueDelay is the delay before re-validating config.
	configValidationRequeueDelay = 5 * time.Minute

	// maxConditionMessageLength is the maximum length for condition messages.
	maxConditionMessageLength = 256
)

// GatewayClassConfigReconciler reconciles GatewayClassConfig resources.
// It validates the configuration and updates status conditions.
type GatewayClassConfigReconciler struct {
	client.Client

	Scheme           *runtime.Scheme
	DefaultNamespace string
}

func (r *GatewayClassConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var config v1alpha1.GatewayClassConfig

	err := r.Get(ctx, req.NamespacedName, &config)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, errors.Wrap(err, "failed to get GatewayClassConfig")
	}

	logger.Info("validating GatewayClassConfig", "name", config.Name)

	// Validate secrets and collect conditions
	conditions := r.validateConfig(ctx, &config)

	// Update status
	config.Status.Conditions = conditions

	updateErr := r.Status().Update(ctx, &config)
	if updateErr != nil {
		return ctrl.Result{}, errors.Wrap(updateErr, "failed to update GatewayClassConfig status")
	}

	// Requeue periodically to re-validate (secrets might be created/deleted)
	return ctrl.Result{RequeueAfter: configValidationRequeueDelay}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GatewayClassConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	//nolint:wrapcheck // controller-runtime builder pattern
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.GatewayClassConfig{}).
		// Watch Secrets to re-validate when they change
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToConfigs),
		).
		Complete(r)
}

func (r *GatewayClassConfigReconciler) validateConfig(
	ctx context.Context,
	config *v1alpha1.GatewayClassConfig,
) []metav1.Condition {
	now := metav1.Now()
	secretsResolved := true

	var validationErrors []string

	// Validate credentials secret
	credResolved, credErrors := r.validateCredentialsSecret(ctx, config)
	if !credResolved {
		secretsResolved = false
	}

	validationErrors = append(validationErrors, credErrors...)

	// Validate tunnel token secret if cloudflared is enabled
	if config.Spec.IsCloudflaredEnabled() {
		tokenResolved, tokenErrors := r.validateTunnelTokenSecret(ctx, config)
		if !tokenResolved {
			secretsResolved = false
		}

		validationErrors = append(validationErrors, tokenErrors...)
	}

	// Build conditions
	return []metav1.Condition{
		r.buildSecretsCondition(secretsResolved, config.Generation, now),
		r.buildValidCondition(validationErrors, config.Generation, now),
	}
}

func (r *GatewayClassConfigReconciler) validateCredentialsSecret(
	ctx context.Context,
	config *v1alpha1.GatewayClassConfig,
) (bool, []string) {
	credRef := config.Spec.CloudflareCredentialsSecretRef

	credNamespace := credRef.Namespace
	if credNamespace == "" {
		credNamespace = r.DefaultNamespace
	}

	credSecret := &corev1.Secret{}

	credErr := r.Get(ctx, types.NamespacedName{Name: credRef.Name, Namespace: credNamespace}, credSecret)
	if credErr != nil {
		var errs []string
		if apierrors.IsNotFound(credErr) {
			errs = append(errs, fmt.Sprintf("credentials secret '%s' not found in namespace '%s'",
				credRef.Name, credNamespace))
		} else {
			errs = append(errs, "failed to get credentials secret: "+credErr.Error())
		}

		return false, errs
	}

	// Check for required key
	apiTokenKey := credRef.GetAPITokenKey()
	if _, ok := credSecret.Data[apiTokenKey]; !ok {
		return false, []string{fmt.Sprintf("credentials secret missing key '%s'", apiTokenKey)}
	}

	return true, nil
}

func (r *GatewayClassConfigReconciler) validateTunnelTokenSecret(
	ctx context.Context,
	config *v1alpha1.GatewayClassConfig,
) (bool, []string) {
	if config.Spec.TunnelTokenSecretRef == nil {
		return false, []string{"tunnelTokenSecretRef is required when cloudflared.enabled is true"}
	}

	tokenRef := config.Spec.TunnelTokenSecretRef

	tokenNamespace := tokenRef.Namespace
	if tokenNamespace == "" {
		tokenNamespace = r.DefaultNamespace
	}

	tokenSecret := &corev1.Secret{}

	tokenErr := r.Get(ctx, types.NamespacedName{Name: tokenRef.Name, Namespace: tokenNamespace}, tokenSecret)
	if tokenErr != nil {
		var errs []string
		if apierrors.IsNotFound(tokenErr) {
			errs = append(errs, fmt.Sprintf("tunnel token secret '%s' not found in namespace '%s'",
				tokenRef.Name, tokenNamespace))
		} else {
			errs = append(errs, "failed to get tunnel token secret: "+tokenErr.Error())
		}

		return false, errs
	}

	tokenKey := tokenRef.GetTunnelTokenKey()
	if _, ok := tokenSecret.Data[tokenKey]; !ok {
		return false, []string{fmt.Sprintf("tunnel token secret missing key '%s'", tokenKey)}
	}

	return true, nil
}

func (r *GatewayClassConfigReconciler) buildSecretsCondition(
	resolved bool,
	generation int64,
	now metav1.Time,
) metav1.Condition {
	if resolved {
		return metav1.Condition{
			Type:               ConditionTypeSecretsResolved,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             "SecretsFound",
			Message:            "All referenced secrets exist and contain required keys",
		}
	}

	return metav1.Condition{
		Type:               ConditionTypeSecretsResolved,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: generation,
		LastTransitionTime: now,
		Reason:             "SecretsMissing",
		Message:            "One or more referenced secrets are missing or invalid",
	}
}

func (r *GatewayClassConfigReconciler) buildValidCondition(
	validationErrors []string,
	generation int64,
	now metav1.Time,
) metav1.Condition {
	if len(validationErrors) == 0 {
		return metav1.Condition{
			Type:               ConditionTypeValid,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             "Valid",
			Message:            "Configuration is valid",
		}
	}

	// Build error message
	errMsg := validationErrors[0]
	if len(validationErrors) > 1 {
		errMsg = fmt.Sprintf("%s (and %d more errors)", errMsg, len(validationErrors)-1)
	}

	if len(errMsg) > maxConditionMessageLength {
		errMsg = errMsg[:maxConditionMessageLength-3] + "..."
	}

	return metav1.Condition{
		Type:               ConditionTypeValid,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: generation,
		LastTransitionTime: now,
		Reason:             "Invalid",
		Message:            errMsg,
	}
}

func (r *GatewayClassConfigReconciler) secretToConfigs(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}

	// List all GatewayClassConfigs and check if any reference this secret
	var configList v1alpha1.GatewayClassConfigList

	err := r.List(ctx, &configList)
	if err != nil {
		return nil
	}

	var requests []reconcile.Request

	for i := range configList.Items {
		cfg := &configList.Items[i]

		if SecretMatchesConfig(secret, cfg) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: cfg.Name},
			})
		}
	}

	return requests
}

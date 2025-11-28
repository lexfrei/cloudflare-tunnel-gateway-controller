package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

func TestGatewayClassConfigReconciler_Reconcile_NotFound(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &GatewayClassConfigReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		DefaultNamespace: "default",
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: "non-existent-config",
		},
	}

	result, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestGatewayClassConfigReconciler_Reconcile_Valid(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	// Create valid secrets
	credentialsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-token"),
		},
	}

	tunnelTokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tunnel-token",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"tunnel-token": []byte("test-tunnel-token"),
		},
	}

	config := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-config",
			Generation: 1,
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			TunnelID: "test-tunnel-id",
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			TunnelTokenSecretRef: &v1alpha1.SecretReference{
				Name:      "tunnel-token",
				Namespace: "default",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(config, credentialsSecret, tunnelTokenSecret).
		WithStatusSubresource(config).
		Build()

	r := &GatewayClassConfigReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		DefaultNamespace: "default",
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: "test-config",
		},
	}

	result, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, configValidationRequeueDelay, result.RequeueAfter)

	// Verify status was updated
	var updatedConfig v1alpha1.GatewayClassConfig
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-config"}, &updatedConfig)
	require.NoError(t, err)

	require.Len(t, updatedConfig.Status.Conditions, 2)

	// Find conditions
	var secretsCondition, validCondition *metav1.Condition
	for i := range updatedConfig.Status.Conditions {
		switch updatedConfig.Status.Conditions[i].Type {
		case ConditionTypeSecretsResolved:
			secretsCondition = &updatedConfig.Status.Conditions[i]
		case ConditionTypeValid:
			validCondition = &updatedConfig.Status.Conditions[i]
		}
	}

	require.NotNil(t, secretsCondition)
	assert.Equal(t, metav1.ConditionTrue, secretsCondition.Status)
	assert.Equal(t, "SecretsFound", secretsCondition.Reason)

	require.NotNil(t, validCondition)
	assert.Equal(t, metav1.ConditionTrue, validCondition.Status)
	assert.Equal(t, "Valid", validCondition.Reason)
}

func TestGatewayClassConfigReconciler_Reconcile_MissingCredentialsSecret(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	config := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-config",
			Generation: 1,
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			TunnelID: "test-tunnel-id",
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "non-existent-secret",
				Namespace: "default",
			},
			Cloudflared: v1alpha1.CloudflaredConfig{
				Enabled: ptr(false),
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(config).
		WithStatusSubresource(config).
		Build()

	r := &GatewayClassConfigReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		DefaultNamespace: "default",
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: "test-config",
		},
	}

	result, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, configValidationRequeueDelay, result.RequeueAfter)

	// Verify status reflects missing secret
	var updatedConfig v1alpha1.GatewayClassConfig
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-config"}, &updatedConfig)
	require.NoError(t, err)

	var secretsCondition *metav1.Condition
	for i := range updatedConfig.Status.Conditions {
		if updatedConfig.Status.Conditions[i].Type == ConditionTypeSecretsResolved {
			secretsCondition = &updatedConfig.Status.Conditions[i]
			break
		}
	}

	require.NotNil(t, secretsCondition)
	assert.Equal(t, metav1.ConditionFalse, secretsCondition.Status)
	assert.Equal(t, "SecretsMissing", secretsCondition.Reason)
}

func TestGatewayClassConfigReconciler_Reconcile_MissingTunnelTokenSecret(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	// Credentials secret exists
	credentialsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-token"),
		},
	}

	config := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-config",
			Generation: 1,
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			TunnelID: "test-tunnel-id",
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			// Cloudflared enabled by default, but no tunnel token secret ref
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(config, credentialsSecret).
		WithStatusSubresource(config).
		Build()

	r := &GatewayClassConfigReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		DefaultNamespace: "default",
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: "test-config",
		},
	}

	result, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, configValidationRequeueDelay, result.RequeueAfter)

	// Verify status reflects missing tunnel token
	var updatedConfig v1alpha1.GatewayClassConfig
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-config"}, &updatedConfig)
	require.NoError(t, err)

	var validCondition *metav1.Condition
	for i := range updatedConfig.Status.Conditions {
		if updatedConfig.Status.Conditions[i].Type == ConditionTypeValid {
			validCondition = &updatedConfig.Status.Conditions[i]
			break
		}
	}

	require.NotNil(t, validCondition)
	assert.Equal(t, metav1.ConditionFalse, validCondition.Status)
	assert.Contains(t, validCondition.Message, "tunnelTokenSecretRef is required")
}

func TestGatewayClassConfigReconciler_Reconcile_MissingAPITokenKey(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	// Secret exists but missing api-token key
	credentialsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"wrong-key": []byte("test-token"),
		},
	}

	config := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-config",
			Generation: 1,
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			TunnelID: "test-tunnel-id",
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			Cloudflared: v1alpha1.CloudflaredConfig{
				Enabled: ptr(false),
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(config, credentialsSecret).
		WithStatusSubresource(config).
		Build()

	r := &GatewayClassConfigReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		DefaultNamespace: "default",
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: "test-config",
		},
	}

	result, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, configValidationRequeueDelay, result.RequeueAfter)

	// Verify status reflects missing key
	var updatedConfig v1alpha1.GatewayClassConfig
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-config"}, &updatedConfig)
	require.NoError(t, err)

	var validCondition *metav1.Condition
	for i := range updatedConfig.Status.Conditions {
		if updatedConfig.Status.Conditions[i].Type == ConditionTypeValid {
			validCondition = &updatedConfig.Status.Conditions[i]
			break
		}
	}

	require.NotNil(t, validCondition)
	assert.Equal(t, metav1.ConditionFalse, validCondition.Status)
	assert.Contains(t, validCondition.Message, "missing key")
}

func TestGatewayClassConfigReconciler_SecretToConfigs(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	config1 := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "config-1",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			TunnelID: "tunnel-1",
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
		},
	}

	config2 := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "config-2",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			TunnelID: "tunnel-2",
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "other-secret",
				Namespace: "default",
			},
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(config1, config2, secret).
		Build()

	r := &GatewayClassConfigReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		DefaultNamespace: "default",
	}

	requests := r.secretToConfigs(context.Background(), secret)

	require.Len(t, requests, 1)
	assert.Equal(t, "config-1", requests[0].Name)
}

func TestGatewayClassConfigReconciler_SecretToConfigs_WrongType(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &GatewayClassConfigReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		DefaultNamespace: "default",
	}

	// Pass wrong type
	wrongType := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
	}

	requests := r.secretToConfigs(context.Background(), wrongType)

	assert.Nil(t, requests)
}

func TestGatewayClassConfigReconciler_SecretToConfigs_TunnelTokenSecret(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	config := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "config-1",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			TunnelID: "tunnel-1",
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			TunnelTokenSecretRef: &v1alpha1.SecretReference{
				Name:      "tunnel-token",
				Namespace: "default",
			},
		},
	}

	tunnelTokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tunnel-token",
			Namespace: "default",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(config, tunnelTokenSecret).
		Build()

	r := &GatewayClassConfigReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		DefaultNamespace: "default",
	}

	requests := r.secretToConfigs(context.Background(), tunnelTokenSecret)

	require.Len(t, requests, 1)
	assert.Equal(t, "config-1", requests[0].Name)
}

func TestGatewayClassConfigReconciler_BuildSecretsCondition(t *testing.T) {
	t.Parallel()

	r := &GatewayClassConfigReconciler{}
	now := metav1.Now()

	tests := []struct {
		name           string
		resolved       bool
		expectedStatus metav1.ConditionStatus
		expectedReason string
	}{
		{
			name:           "resolved",
			resolved:       true,
			expectedStatus: metav1.ConditionTrue,
			expectedReason: "SecretsFound",
		},
		{
			name:           "not_resolved",
			resolved:       false,
			expectedStatus: metav1.ConditionFalse,
			expectedReason: "SecretsMissing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			condition := r.buildSecretsCondition(tt.resolved, 1, now)

			assert.Equal(t, ConditionTypeSecretsResolved, condition.Type)
			assert.Equal(t, tt.expectedStatus, condition.Status)
			assert.Equal(t, tt.expectedReason, condition.Reason)
			assert.Equal(t, int64(1), condition.ObservedGeneration)
		})
	}
}

func TestGatewayClassConfigReconciler_BuildValidCondition(t *testing.T) {
	t.Parallel()

	r := &GatewayClassConfigReconciler{}
	now := metav1.Now()

	tests := []struct {
		name            string
		errors          []string
		expectedStatus  metav1.ConditionStatus
		expectedReason  string
		messageContains string
	}{
		{
			name:            "no_errors",
			errors:          nil,
			expectedStatus:  metav1.ConditionTrue,
			expectedReason:  "Valid",
			messageContains: "valid",
		},
		{
			name:            "one_error",
			errors:          []string{"error 1"},
			expectedStatus:  metav1.ConditionFalse,
			expectedReason:  "Invalid",
			messageContains: "error 1",
		},
		{
			name:            "multiple_errors",
			errors:          []string{"error 1", "error 2", "error 3"},
			expectedStatus:  metav1.ConditionFalse,
			expectedReason:  "Invalid",
			messageContains: "2 more errors",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			condition := r.buildValidCondition(tt.errors, 1, now)

			assert.Equal(t, ConditionTypeValid, condition.Type)
			assert.Equal(t, tt.expectedStatus, condition.Status)
			assert.Equal(t, tt.expectedReason, condition.Reason)
			assert.Contains(t, condition.Message, tt.messageContains)
			assert.Equal(t, int64(1), condition.ObservedGeneration)
		})
	}
}

func TestGatewayClassConfigReconciler_BuildValidCondition_LongMessage(t *testing.T) {
	t.Parallel()

	r := &GatewayClassConfigReconciler{}
	now := metav1.Now()

	// Create a very long error message using strings.Repeat
	longError := strings.Repeat("x", 300)

	condition := r.buildValidCondition([]string{longError}, 1, now)

	assert.LessOrEqual(t, len(condition.Message), maxConditionMessageLength)
	assert.NotEmpty(t, condition.Message)
}

func TestGatewayClassConfigReconciler_Constants(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Valid", ConditionTypeValid)
	assert.Equal(t, "SecretsResolved", ConditionTypeSecretsResolved)
	assert.Equal(t, 256, maxConditionMessageLength)
}

func TestGatewayClassConfigReconciler_ValidateConfig_CloudflaredDisabled(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	// Credentials secret exists
	credentialsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-token"),
		},
	}

	config := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-config",
			Generation: 1,
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			TunnelID: "test-tunnel-id",
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			Cloudflared: v1alpha1.CloudflaredConfig{
				Enabled: ptr(false),
			},
			// No tunnel token needed when cloudflared is disabled
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(config, credentialsSecret).
		WithStatusSubresource(config).
		Build()

	r := &GatewayClassConfigReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		DefaultNamespace: "default",
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: "test-config",
		},
	}

	result, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, configValidationRequeueDelay, result.RequeueAfter)

	// Verify status is valid
	var updatedConfig v1alpha1.GatewayClassConfig
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-config"}, &updatedConfig)
	require.NoError(t, err)

	var validCondition *metav1.Condition
	for i := range updatedConfig.Status.Conditions {
		if updatedConfig.Status.Conditions[i].Type == ConditionTypeValid {
			validCondition = &updatedConfig.Status.Conditions[i]
			break
		}
	}

	require.NotNil(t, validCondition)
	assert.Equal(t, metav1.ConditionTrue, validCondition.Status)
}

func TestGatewayClassConfigReconciler_DefaultNamespace(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	// Secret in controller's default namespace
	credentialsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "cloudflare-system",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-token"),
		},
	}

	// Config without namespace in secret ref - should use default namespace
	config := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-config",
			Generation: 1,
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			TunnelID: "test-tunnel-id",
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name: "cf-credentials",
				// No namespace specified
			},
			Cloudflared: v1alpha1.CloudflaredConfig{
				Enabled: ptr(false),
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(config, credentialsSecret).
		WithStatusSubresource(config).
		Build()

	r := &GatewayClassConfigReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		DefaultNamespace: "cloudflare-system",
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: "test-config",
		},
	}

	result, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, configValidationRequeueDelay, result.RequeueAfter)

	// Verify status is valid - secret was found using default namespace
	var updatedConfig v1alpha1.GatewayClassConfig
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-config"}, &updatedConfig)
	require.NoError(t, err)

	var secretsCondition *metav1.Condition
	for i := range updatedConfig.Status.Conditions {
		if updatedConfig.Status.Conditions[i].Type == ConditionTypeSecretsResolved {
			secretsCondition = &updatedConfig.Status.Conditions[i]
			break
		}
	}

	require.NotNil(t, secretsCondition)
	assert.Equal(t, metav1.ConditionTrue, secretsCondition.Status)
}

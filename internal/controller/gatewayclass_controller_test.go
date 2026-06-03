package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/pkg/consts"
)

// errTransientRead is a non-NotFound read error used to exercise the transient
// CRD-read path: the controller must requeue rather than persist a verdict.
var errTransientRead = errors.New("simulated transient apiserver failure")

// transientErrorReader is a client.Reader whose Get always fails with a
// non-NotFound error, simulating an apiserver hiccup or not-yet-propagated RBAC
// when the SupportedVersion check reads the gatewayclasses CRD.
type transientErrorReader struct{}

func (transientErrorReader) Get(
	_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption,
) error {
	return errTransientRead
}

func (transientErrorReader) List(
	_ context.Context, _ client.ObjectList, _ ...client.ListOption,
) error {
	return errTransientRead
}

// gatewayClassSchemeWithCRD returns a scheme registered for both Gateway API
// and apiextensions types so the fake client can serve the gatewayclasses CRD
// used by the SupportedVersion check.
func gatewayClassSchemeWithCRD(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, apiextensionsv1.AddToScheme(scheme))

	return scheme
}

// gatewayClassCRDObject builds a gatewayclasses CRD carrying the given
// bundle-version annotation. An empty bundleVersion produces a CRD with no
// annotation, exercising the missing-annotation path.
func gatewayClassCRDObject(bundleVersion string) *apiextensionsv1.CustomResourceDefinition {
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: gatewayClassCRDName,
		},
	}

	if bundleVersion != "" {
		crd.Annotations = map[string]string{
			consts.BundleVersionAnnotation: bundleVersion,
		}
	}

	return crd
}

// findGatewayClassCondition returns the condition of the given type, or nil.
func findGatewayClassCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}

	return nil
}

func TestGatewayClassReconciler_Reconcile_NotFound(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &GatewayClassReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: "non-existent-class",
		},
	}

	result, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestGatewayClassReconciler_Reconcile_WrongControllerName(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "other-class",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "other-controller",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass).
		WithStatusSubresource(gatewayClass).
		Build()

	r := &GatewayClassReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: "other-class",
		},
	}

	result, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestGatewayClassReconciler_Reconcile_MatchingController(t *testing.T) {
	t.Parallel()

	scheme := gatewayClassSchemeWithCRD(t)

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cloudflare-tunnel",
			Generation: 1,
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "cloudflare-tunnel-controller",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gatewayClassCRDObject(consts.BundleVersion)).
		WithStatusSubresource(gatewayClass).
		Build()

	r := &GatewayClassReconciler{
		Client:              fakeClient,
		Scheme:              scheme,
		ControllerName:      "cloudflare-tunnel-controller",
		BundleVersionReader: fakeClient,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: "cloudflare-tunnel",
		},
	}

	result, err := r.Reconcile(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify status conditions were set
	var updatedClass gatewayv1.GatewayClass
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "cloudflare-tunnel"}, &updatedClass)
	require.NoError(t, err)

	require.Len(t, updatedClass.Status.Conditions, 2)

	acceptedCondition := findGatewayClassCondition(
		updatedClass.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
	supportedVersionCondition := findGatewayClassCondition(
		updatedClass.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))

	require.NotNil(t, acceptedCondition)
	assert.Equal(t, metav1.ConditionTrue, acceptedCondition.Status)
	assert.Equal(t, string(gatewayv1.GatewayClassReasonAccepted), acceptedCondition.Reason)
	assert.Equal(t, int64(1), acceptedCondition.ObservedGeneration)
	assert.Contains(t, acceptedCondition.Message, "accepted by cloudflare-tunnel controller")

	require.NotNil(t, supportedVersionCondition)
	assert.Equal(t, metav1.ConditionTrue, supportedVersionCondition.Status)
	assert.Equal(t, string(gatewayv1.GatewayClassReasonSupportedVersion), supportedVersionCondition.Reason)
	assert.Equal(t, int64(1), supportedVersionCondition.ObservedGeneration)
}

func TestGatewayClassReconciler_SetAcceptedConditions(t *testing.T) {
	t.Parallel()

	scheme := gatewayClassSchemeWithCRD(t)
	reader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClassCRDObject(consts.BundleVersion)).
		Build()

	r := &GatewayClassReconciler{
		ControllerName:      "test-controller",
		BundleVersionReader: reader,
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-class",
			Generation: 5,
		},
	}

	require.NoError(t, r.setAcceptedConditions(context.Background(), gatewayClass))

	require.Len(t, gatewayClass.Status.Conditions, 2)

	accepted := findGatewayClassCondition(
		gatewayClass.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
	supportedVersion := findGatewayClassCondition(
		gatewayClass.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))

	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status)
	assert.Equal(t, int64(5), accepted.ObservedGeneration)

	require.NotNil(t, supportedVersion)
	assert.Equal(t, metav1.ConditionTrue, supportedVersion.Status)
	assert.Equal(t, int64(5), supportedVersion.ObservedGeneration)
}

// runSupportedVersionCheck builds a reconciler whose BundleVersionReader serves
// the given CRD objects and returns the resulting SupportedVersion condition.
func runSupportedVersionCheck(
	t *testing.T,
	reader client.Reader,
) metav1.Condition {
	t.Helper()

	r := &GatewayClassReconciler{
		ControllerName:      "test-controller",
		BundleVersionReader: reader,
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "test-class", Generation: 3},
	}

	// Every caller of this helper exercises a deterministic verdict, so
	// setAcceptedConditions must not error (errors are reserved for transient
	// read failures, covered separately).
	require.NoError(t, r.setAcceptedConditions(context.Background(), gatewayClass))

	condition := findGatewayClassCondition(
		gatewayClass.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))
	require.NotNil(t, condition)

	return *condition
}

func TestGatewayClassReconciler_SupportedVersion_SupportedBundle(t *testing.T) {
	t.Parallel()

	scheme := gatewayClassSchemeWithCRD(t)
	reader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClassCRDObject(consts.BundleVersion)).
		Build()

	condition := runSupportedVersionCheck(t, reader)

	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, string(gatewayv1.GatewayClassReasonSupportedVersion), condition.Reason)
}

func TestGatewayClassReconciler_SupportedVersion_PatchVersionAccepted(t *testing.T) {
	t.Parallel()

	// A different patch release of the same major.minor is compatible: the
	// SupportedVersion check must match on major.minor, not the full version.
	scheme := gatewayClassSchemeWithCRD(t)
	reader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClassCRDObject("v1.5.0")).
		Build()

	condition := runSupportedVersionCheck(t, reader)

	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, string(gatewayv1.GatewayClassReasonSupportedVersion), condition.Reason)
}

func TestGatewayClassReconciler_SupportedVersion_UnsupportedBundle(t *testing.T) {
	t.Parallel()

	// v1.4.0 predates the ListenerSet Standard-channel promotion (v1.5.0), so
	// it is not supported and must surface UnsupportedVersion.
	scheme := gatewayClassSchemeWithCRD(t)
	reader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClassCRDObject("v1.4.0")).
		Build()

	condition := runSupportedVersionCheck(t, reader)

	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, string(gatewayv1.GatewayClassReasonUnsupportedVersion), condition.Reason)
	assert.Contains(t, condition.Message, "v1.4.0")
}

func TestGatewayClassReconciler_SupportedVersion_NewerMinorRejected(t *testing.T) {
	t.Parallel()

	// A newer minor than the controller was built against is also unsupported:
	// the controller can only attest to the major.minor it ships with, so it
	// must not claim support for fields it has never seen.
	scheme := gatewayClassSchemeWithCRD(t)
	reader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClassCRDObject("v1.6.0")).
		Build()

	condition := runSupportedVersionCheck(t, reader)

	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, string(gatewayv1.GatewayClassReasonUnsupportedVersion), condition.Reason)
	assert.Contains(t, condition.Message, "v1.6.0")
}

func TestGatewayClassReconciler_SupportedVersion_MissingAnnotation(t *testing.T) {
	t.Parallel()

	scheme := gatewayClassSchemeWithCRD(t)
	reader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClassCRDObject("")).
		Build()

	condition := runSupportedVersionCheck(t, reader)

	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, string(gatewayv1.GatewayClassReasonUnsupportedVersion), condition.Reason)
	assert.Contains(t, condition.Message, consts.BundleVersionAnnotation)
}

func TestGatewayClassReconciler_SupportedVersion_MalformedAnnotation(t *testing.T) {
	t.Parallel()

	// The annotation is present but its value is not a parseable version, so the
	// check fails closed with UnsupportedVersion and names the offending value.
	scheme := gatewayClassSchemeWithCRD(t)
	reader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClassCRDObject("garbage")).
		Build()

	condition := runSupportedVersionCheck(t, reader)

	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, string(gatewayv1.GatewayClassReasonUnsupportedVersion), condition.Reason)
	assert.Contains(t, condition.Message, "garbage")
}

func TestGatewayClassReconciler_SupportedVersion_CRDNotFound(t *testing.T) {
	t.Parallel()

	scheme := gatewayClassSchemeWithCRD(t)
	reader := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	condition := runSupportedVersionCheck(t, reader)

	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, string(gatewayv1.GatewayClassReasonUnsupportedVersion), condition.Reason)
}

func TestGatewayClassReconciler_SupportedVersion_NoReader(t *testing.T) {
	t.Parallel()

	condition := runSupportedVersionCheck(t, nil)

	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, string(gatewayv1.GatewayClassReasonUnsupportedVersion), condition.Reason)
}

// A transient (non-NotFound) CRD read error must not stick a misleading
// UnsupportedVersion on the GatewayClass. Instead the reconcile surfaces the
// error (so controller-runtime requeues and the check self-heals), while the
// required Accepted condition is still persisted.
func TestGatewayClassReconciler_SupportedVersion_TransientReadError_Requeues(t *testing.T) {
	t.Parallel()

	scheme := gatewayClassSchemeWithCRD(t)

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
	}

	mainClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass).
		WithStatusSubresource(gatewayClass).
		Build()

	r := &GatewayClassReconciler{
		Client:              mainClient,
		Scheme:              scheme,
		ControllerName:      "test-controller",
		BundleVersionReader: transientErrorReader{},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "cloudflare-tunnel"}}

	_, err := r.Reconcile(context.Background(), req)
	require.Error(t, err, "a transient CRD read error must surface so the reconcile requeues")
	assert.ErrorIs(t, err, errTransientRead)

	var updated gatewayv1.GatewayClass
	require.NoError(t, mainClient.Get(context.Background(),
		types.NamespacedName{Name: "cloudflare-tunnel"}, &updated))

	// Accepted (required, Standard channel) is persisted even though the
	// best-effort bundle check failed.
	accepted := findGatewayClassCondition(
		updated.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status)

	// SupportedVersion is NOT written: a momentary read failure must not be
	// recorded as UnsupportedVersion.
	supportedVersion := findGatewayClassCondition(
		updated.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusSupportedVersion))
	assert.Nil(t, supportedVersion, "transient read error must not persist a SupportedVersion condition")
}

// TestBundleVersionConstantIsParseable is a canary on the vendored Gateway API
// bundle-version constant. bundleVersionSupported fails closed if
// consts.BundleVersion cannot be parsed, a branch that is unreachable while the
// constant stays well-formed. Pin that invariant: if a re-vendor changes the
// version format, this fails instead of silently disabling the SupportedVersion
// check at runtime.
func TestBundleVersionConstantIsParseable(t *testing.T) {
	t.Parallel()

	major, minor, ok := parseMajorMinor(consts.BundleVersion)
	require.True(t, ok, "vendored consts.BundleVersion %q must be parseable", consts.BundleVersion)
	assert.Positive(t, major)
	assert.GreaterOrEqual(t, minor, 0)
}

func TestParseMajorMinor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		version   string
		wantMajor int
		wantMinor int
		wantOK    bool
	}{
		{name: "v-prefixed", version: "v1.5.1", wantMajor: 1, wantMinor: 5, wantOK: true},
		{name: "no prefix", version: "1.5.1", wantMajor: 1, wantMinor: 5, wantOK: true},
		{name: "major.minor only", version: "v1.5", wantMajor: 1, wantMinor: 5, wantOK: true},
		{name: "whitespace", version: "  v2.3.0  ", wantMajor: 2, wantMinor: 3, wantOK: true},
		{name: "major only", version: "v1", wantOK: false},
		{name: "empty", version: "", wantOK: false},
		{name: "non-numeric major", version: "vX.5.1", wantOK: false},
		{name: "non-numeric minor", version: "v1.Y.1", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			major, minor, ok := parseMajorMinor(tt.version)
			assert.Equal(t, tt.wantOK, ok)

			if tt.wantOK {
				assert.Equal(t, tt.wantMajor, major)
				assert.Equal(t, tt.wantMinor, minor)
			}
		})
	}
}

func TestGatewayClassReconciler_UpdateStatus_ControllerMismatch(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	// GatewayClass with a different controller name
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "other-class",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "other-controller",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass).
		WithStatusSubresource(gatewayClass).
		Build()

	r := &GatewayClassReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
	}

	// updateStatus should silently return nil for non-matching controllers
	err := r.updateStatus(context.Background(), types.NamespacedName{Name: "other-class"})
	assert.NoError(t, err)

	// Verify no conditions were set
	var updatedClass gatewayv1.GatewayClass
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "other-class"}, &updatedClass)
	require.NoError(t, err)
	assert.Empty(t, updatedClass.Status.Conditions)
}

func TestGatewayClassReconciler_UpdateStatus_NotFound(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &GatewayClassReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
	}

	err := r.updateStatus(context.Background(), types.NamespacedName{Name: "non-existent"})
	assert.Error(t, err)
}

func TestGatewayClassReconciler_Reconcile_IdempotentStatusUpdate(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cloudflare-tunnel",
			Generation: 2,
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass).
		WithStatusSubresource(gatewayClass).
		Build()

	r := &GatewayClassReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: "cloudflare-tunnel",
		},
	}

	// First reconcile
	result, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Second reconcile (should be idempotent)
	result, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify conditions still correct
	var updatedClass gatewayv1.GatewayClass
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "cloudflare-tunnel"}, &updatedClass)
	require.NoError(t, err)
	assert.Len(t, updatedClass.Status.Conditions, 2)
}

func TestGatewayClassReconciler_Reconcile_WrongType(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &GatewayClassReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
	}

	// Request for a non-existent GatewayClass
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: "missing",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

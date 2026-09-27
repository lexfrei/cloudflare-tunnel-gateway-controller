package controller

import (
	"context"
	"time"

	"github.com/cockroachdb/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/configtls"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

const (
	// eventReasonConfigTLSReplaced reports a leaf that failed verification
	// against the controller CA and was replaced by a fresh slot.
	eventReasonConfigTLSReplaced = "ConfigTLSCertificateReplaced"

	// maxConfigTLSSlotAttempts bounds how many consecutive unusable slots one
	// reconcile walks past before giving up and reporting.
	maxConfigTLSSlotAttempts = 16

	// configTLSRecheckInterval brings a TLS plane back for its renewal check
	// when nothing else reconciles it.
	configTLSRecheckInterval = time.Hour
)

var errNoUsableConfigTLSSlot = errors.New("no usable config API certificate slot")

func (r *GatewayInfraReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}

	return time.Now()
}

// ensureConfigTLSSecret returns the name of a valid, Gateway-owned config API
// leaf for the Gateway's plane, issuing one when needed; "" when config API
// TLS is off.
//
// The controller holds create but never update on Secrets outside its own
// namespace, so a leaf is never rewritten. The walk starts at the slot the
// rendered Deployment mounts and never goes below it: that slot is kept while
// it verifies, recreated in place when deleted, and otherwise the next slot
// is tried. A slot holding a Secret this Gateway does not own is skipped,
// never adopted. Concurrent issuers walk the same sequence and create-only
// writes let exactly one leaf land per slot, so they settle on the same one.
func (r *GatewayInfraReconciler) ensureConfigTLSSecret(ctx context.Context, gateway *gatewayv1.Gateway) (string, error) {
	if r.ConfigAuthority == nil {
		return "", nil
	}

	start, err := r.mountedConfigTLSSlot(ctx, gateway)
	if err != nil {
		return "", err
	}

	names := []string{render.ConfigServerName(gateway, r.ClusterDomain)}
	now := r.clock()

	for index := start; index < start+maxConfigTLSSlotAttempts; index++ {
		key := types.NamespacedName{Name: render.ConfigTLSSecretName(gateway, index), Namespace: gateway.Namespace}

		usable, err := r.configTLSSlotUsable(ctx, gateway, key, names, now)
		if err != nil {
			return "", err
		}

		if usable {
			return key.Name, nil
		}
	}

	return "", errors.Wrapf(errNoUsableConfigTLSSlot, "tried %d slots from %d", maxConfigTLSSlotAttempts, start)
}

func (r *GatewayInfraReconciler) mountedConfigTLSSlot(ctx context.Context, gateway *gatewayv1.Gateway) (int, error) {
	var deployment appsv1.Deployment

	err := r.Get(ctx, types.NamespacedName{Name: render.DeploymentName(gateway), Namespace: gateway.Namespace}, &deployment)
	if apierrors.IsNotFound(err) {
		return 0, nil
	}

	if err != nil {
		return 0, errors.Wrap(err, "reading rendered deployment for its config API certificate slot")
	}

	index, _ := render.ConfigTLSSecretIndex(gateway, &deployment)

	return index, nil
}

// configTLSSlotUsable reports whether the slot at key holds, or now holds
// after a create, a leaf this plane can serve.
func (r *GatewayInfraReconciler) configTLSSlotUsable(
	ctx context.Context, gateway *gatewayv1.Gateway, key types.NamespacedName, names []string, now time.Time,
) (bool, error) {
	var existing corev1.Secret

	err := r.Get(ctx, key, &existing)
	if apierrors.IsNotFound(err) {
		created, createErr := r.createConfigTLSLeaf(ctx, gateway, key, names, now)
		if createErr != nil || created {
			return created, createErr
		}

		// Lost the create race: judge whatever landed.
		err = r.Get(ctx, key, &existing)
	}

	if err != nil {
		return false, errors.Wrapf(err, "reading config API certificate %s", key)
	}

	if !metav1.IsControlledBy(&existing, gateway) {
		return false, nil
	}

	checkErr := r.ConfigAuthority.Check(
		existing.Data[corev1.TLSCertKey], existing.Data[corev1.TLSPrivateKeyKey], names, now)
	if checkErr == nil {
		return true, nil
	}

	if !errors.Is(checkErr, configtls.ErrRenewalDue) {
		r.event(gateway, corev1.EventTypeWarning, eventReasonConfigTLSReplaced,
			"config API certificate "+key.Name+" does not verify against the controller CA and is being replaced: "+
				checkErr.Error())
	}

	return false, nil
}

// createConfigTLSLeaf reports false without error when another writer created
// the slot first.
func (r *GatewayInfraReconciler) createConfigTLSLeaf(
	ctx context.Context, gateway *gatewayv1.Gateway, key types.NamespacedName, names []string, now time.Time,
) (bool, error) {
	certPEM, keyPEM, err := r.ConfigAuthority.Issue(names, now)
	if err != nil {
		return false, errors.Wrap(err, "issuing config API certificate")
	}

	secret := tlsSecret(key, certPEM, keyPEM)
	if err := controllerutil.SetControllerReference(gateway, secret, r.Scheme); err != nil {
		return false, errors.Wrap(err, "setting owner on config API certificate")
	}

	if err := r.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return false, nil
		}

		return false, errors.Wrapf(err, "creating config API certificate %s", key)
	}

	return true, nil
}

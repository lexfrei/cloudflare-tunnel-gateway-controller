package controller

import (
	"context"
	"strings"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// errProxyAuthSecretMissingKey is the sentinel wrapped by
// readGeneratedAuthToken so callers can errors.Is() against it.
var errProxyAuthSecretMissingKey = errors.New("shared-proxy auth secret exists but has no auth-token key")

// errInvalidProxyAuthSecretRef is the sentinel wrapped by
// parseProxyAuthSecretRef on a malformed --proxy-auth-secret-ref value.
var errInvalidProxyAuthSecretRef = errors.New("--proxy-auth-secret-ref must be in `<namespace>/<name>` format")

// parseProxyAuthSecretRef parses the "<namespace>/<name>" form of
// --proxy-auth-secret-ref, mirroring NewProxySecretReconciler's parsing of
// --proxy-token-secret (same format, different flag).
func parseProxyAuthSecretRef(raw string) (types.NamespacedName, error) {
	raw = strings.TrimSpace(raw)

	parts := strings.SplitN(raw, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return types.NamespacedName{}, errors.Wrapf(errInvalidProxyAuthSecretRef, "got %q", raw)
	}

	return types.NamespacedName{Namespace: parts[0], Name: parts[1]}, nil
}

// ensureProxyAuthSecret guarantees the SHARED proxy's config-API auth Secret
// exists, generating and creating it once if missing, and returns the
// resolved token. It is CREATE-ONLY, mirroring
// GatewayInfraReconciler.ensureGeneratedAuthSecret for the per-Gateway data
// planes: an existing Secret's token is reused verbatim and never rotated,
// because a fresh token on every call would roll the proxy pods endlessly.
//
// Unlike the per-Gateway path, this is called directly (not from a
// reconciler) as one of the controller's first startup steps, with a plain,
// uncached client — see its call site in Run for why: the controller's own
// push client needs the resolved token value, and wiring it via a pod-level
// secretKeyRef the way a bring-your-own token is would deadlock the
// controller's own pod. Kubernetes cannot start a container whose
// secretKeyRef points at a Secret that does not exist yet, and the only code
// that would create that Secret is the very process kubelet is refusing to
// start.
//
// There is no ownership/adoption concept here (unlike the per-Gateway path):
// the shared plane has no owning Gateway object to assert against, and a
// pre-existing Secret at this deterministic name -- however it got there --
// is simply reused as-is.
func ensureProxyAuthSecret(ctx context.Context, c client.Client, key types.NamespacedName) (string, error) {
	var existing corev1.Secret
	if err := c.Get(ctx, key, &existing); err == nil {
		return readGeneratedAuthToken(&existing, key)
	} else if !apierrors.IsNotFound(err) {
		return "", errors.Wrapf(err, "checking shared-proxy auth secret %s", key)
	}

	token, err := generateAuthToken()
	if err != nil {
		return "", errors.Wrap(err, "generating config-api auth token")
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Data:       map[string][]byte{generatedAuthTokenKey: []byte(token)},
	}

	if err := c.Create(ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", errors.Wrapf(err, "creating shared-proxy auth secret %s", key)
		}

		// Lost the create race (another controller replica, or a manual
		// `kubectl create` landing between our Get and our Create): re-read
		// and reuse whatever token actually won, rather than overwrite it.
		if err := c.Get(ctx, key, &existing); err != nil {
			return "", errors.Wrapf(err, "re-reading shared-proxy auth secret after create race %s", key)
		}

		return readGeneratedAuthToken(&existing, key)
	}

	return token, nil
}

// resolveProxyAuthToken returns a copy of cfg with ProxyAuthToken filled in
// from the generated shared-proxy auth Secret when cfg.ProxyAuthSecretRef is
// set (the chart-generated path). cfg.ProxyAuthToken already carries the
// resolved value for the bring-your-own path (a pod-level secretKeyRef the
// chart wired directly), so this is a no-op copy in that case.
//
// A plain, uncached client is built here rather than using mgr.GetClient():
// the manager's cached client blocks reads until its informer cache has
// synced, which only happens once mgr.Start() runs, much later in Run --
// calling it here, before Start, would deadlock waiting on a cache that is
// never going to start pumping.
func resolveProxyAuthToken(ctx context.Context, mgr ctrl.Manager, cfg *Config) (*Config, error) {
	resolved := *cfg

	if cfg.ProxyAuthSecretRef == "" {
		return &resolved, nil
	}

	authSecretKey, err := parseProxyAuthSecretRef(cfg.ProxyAuthSecretRef)
	if err != nil {
		return nil, errors.Wrap(err, "invalid --proxy-auth-secret-ref")
	}

	authClient, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return nil, errors.Wrap(err, "creating direct client for proxy auth secret")
	}

	token, err := ensureProxyAuthSecret(ctx, authClient, authSecretKey)
	if err != nil {
		return nil, errors.Wrap(err, "ensuring shared-proxy auth secret")
	}

	resolved.ProxyAuthToken = token

	return &resolved, nil
}

// readGeneratedAuthToken extracts and validates the auth-token key of an
// existing Secret. A Secret at the expected name with no (or an empty)
// auth-token key is a broken state -- fail closed rather than silently hand
// back an empty token that would make the config API accept an empty Bearer
// value as valid.
func readGeneratedAuthToken(secret *corev1.Secret, key types.NamespacedName) (string, error) {
	token, ok := secret.Data[generatedAuthTokenKey]
	if !ok || len(token) == 0 {
		return "", errors.Wrapf(errProxyAuthSecretMissingKey, "%s", key)
	}

	return string(token), nil
}

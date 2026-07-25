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

// errProxyAuthSecretMissingKey is the sentinel wrapped by readAuthSecretKey
// so callers can errors.Is() against it.
var errProxyAuthSecretMissingKey = errors.New("shared-proxy auth secret exists but has no usable value at the expected key")

// errProxyAuthSecretNotFound is the sentinel wrapped when a bring-your-own
// reference (generate=false) does not exist. Unlike the generate=true path,
// a BYO reference is NEVER auto-created: minting a Secret at a name the
// operator chose for their own token would silently misconfigure the very
// thing they pointed the controller at, trading one hole (unauthenticated)
// for a quieter one (authenticated against a token nobody who reads the
// operator's Secret actually knows).
var errProxyAuthSecretNotFound = errors.New("shared-proxy auth secret not found (bring-your-own reference must already exist)")

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

// ensureProxyAuthSecret resolves the SHARED proxy's config-API auth token
// from a Secret, generating and creating it once when generate is true and
// the Secret is missing. It is the SINGLE mechanism the controller uses to
// learn this token for its own push auth, for both the bring-your-own
// (generate=false, proxy.authTokenSecretRef.name set) and chart-generated
// (generate=true) cases: earlier revisions resolved BYO via a pod-level
// secretKeyRef and only the generated case through this function, which left
// two independent mechanisms and, on the controller's own pod, a secretKeyRef
// that is safe only because a BYO Secret happens to already exist -- a
// property of the deployment, not something the code enforced. Wiring BOTH
// paths through one direct-API resolution removes that distinction
// structurally: nothing the controller's own pod spec references can ever be
// missing at container-start time, because the controller never asks
// Kubernetes to hand it a Secret via secretKeyRef in the first place.
//
// generate=false is CREATE-ONLY *disabled*: a missing Secret is a
// configuration error (errProxyAuthSecretNotFound), never silently papered
// over by minting one at the operator's chosen name. generate=true is
// CREATE-ONLY *enabled* and mirrors
// GatewayInfraReconciler.ensureGeneratedAuthSecret for the per-Gateway data
// planes: an existing Secret's token is reused verbatim and never rotated,
// because a fresh token on every call would roll the proxy pods endlessly.
//
// There is no ownership/adoption concept here (unlike the per-Gateway path):
// the shared plane has no owning Gateway object to assert against, and a
// pre-existing Secret at this name -- however it got there -- is simply
// reused as-is, never overwritten (the controller holds Secrets `create`,
// never `update`/`patch`, and this function never attempts either).
func ensureProxyAuthSecret(
	ctx context.Context, c client.Client, key types.NamespacedName, dataKey string, generate bool,
) (string, error) {
	var existing corev1.Secret
	if err := c.Get(ctx, key, &existing); err == nil {
		return readAuthSecretKey(&existing, key, dataKey)
	} else if !apierrors.IsNotFound(err) {
		return "", errors.Wrapf(err, "checking shared-proxy auth secret %s", key)
	} else if !generate {
		return "", errors.Wrapf(errProxyAuthSecretNotFound, "%s", key)
	}

	token, err := generateAuthToken()
	if err != nil {
		return "", errors.Wrap(err, "generating config-api auth token")
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Data:       map[string][]byte{dataKey: []byte(token)},
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

		return readAuthSecretKey(&existing, key, dataKey)
	}

	return token, nil
}

// resolveProxyAuthToken returns a copy of cfg with ProxyAuthToken resolved
// via ensureProxyAuthSecret when cfg.ProxyAuthSecretRef is set -- the chart
// always sets it, for both the bring-your-own and generated cases (see
// ensureProxyAuthSecret's doc comment for why unifying on one resolution
// path matters). cfg.ProxyAuthSecretRef being empty is a true no-op: the
// direct --proxy-auth-token path (still supported for callers outside the
// chart) is returned untouched, and no API calls are made at all.
//
// A plain, uncached client is built here rather than using mgr.GetClient():
// its cache-backed reader has not been started yet at this point in Run
// (that happens inside mgr.Start(), much later), and informerCache.Get
// (sigs.k8s.io/controller-runtime/pkg/cache) returns ErrCacheNotStarted
// immediately in that state rather than blocking -- so the failure would be
// fast and clean, but 100% guaranteed on every startup, which is just as
// unusable as a hang would have been. Writes on that same client always go
// straight to the API server regardless of cache state, so only the Get
// half of ensureProxyAuthSecret is actually affected -- reason enough on its
// own for a client that works consistently at this point in startup.
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

	dataKey := cfg.ProxyAuthSecretKey
	if dataKey == "" {
		dataKey = generatedAuthTokenKey
	}

	token, err := ensureProxyAuthSecret(ctx, authClient, authSecretKey, dataKey, cfg.ProxyAuthSecretGenerate)
	if err != nil {
		return nil, errors.Wrap(err, "resolving shared-proxy auth secret")
	}

	resolved.ProxyAuthToken = token

	return &resolved, nil
}

// readAuthSecretKey extracts and validates dataKey from an existing Secret.
// A Secret at the expected name with no (or an empty) value at dataKey is a
// broken state -- fail closed rather than silently hand back an empty token
// that would make the config API accept an empty Bearer value as valid.
func readAuthSecretKey(secret *corev1.Secret, key types.NamespacedName, dataKey string) (string, error) {
	token, ok := secret.Data[dataKey]
	if !ok || len(token) == 0 {
		return "", errors.Wrapf(errProxyAuthSecretMissingKey, "%s key %q", key, dataKey)
	}

	return string(token), nil
}

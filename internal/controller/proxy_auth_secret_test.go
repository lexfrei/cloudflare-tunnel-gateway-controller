package controller

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func proxyAuthSecretKey() types.NamespacedName {
	return types.NamespacedName{Name: "release-proxy-auth-token", Namespace: "cf-system"}
}

func TestParseProxyAuthSecretRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    types.NamespacedName
		wantErr bool
	}{
		{name: "valid", raw: "cf-system/release-proxy-auth-token", want: proxyAuthSecretKey()},
		{name: "empty", raw: "", wantErr: true},
		{name: "no slash", raw: "release-proxy-auth-token", wantErr: true},
		{name: "empty namespace", raw: "/release-proxy-auth-token", wantErr: true},
		{name: "empty name", raw: "cf-system/", wantErr: true},
		{name: "trims whitespace", raw: "  cf-system/release-proxy-auth-token  ", want: proxyAuthSecretKey()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseProxyAuthSecretRef(tt.raw)
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, errInvalidProxyAuthSecretRef)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestResolveProxyAuthToken_NoOpWhenSecretRefEmpty pins backward
// compatibility for every caller that does not set --proxy-auth-secret-ref
// (raw manifests, older values files, anyone still on the direct
// --proxy-auth-token path): no generation, no API calls, no error -- cfg
// comes back with ProxyAuthToken untouched, exactly as Run behaved before
// this flag existed. mgr is passed as nil and must never be dereferenced on
// this path; a real manager needs a live rest.Config to construct, which a
// unit test should not need just to prove a no-op branch stays a no-op.
func TestResolveProxyAuthToken_NoOpWhenSecretRefEmpty(t *testing.T) {
	t.Parallel()

	cfg := &Config{ProxyAuthToken: "byo-token", ProxyAuthSecretRef: ""}

	resolved, err := resolveProxyAuthToken(context.Background(), nil, cfg)
	require.NoError(t, err)
	assert.Equal(t, "byo-token", resolved.ProxyAuthToken)
	assert.NotSame(t, cfg, resolved, "must return a copy, never alias or mutate the caller's Config")
}

// TestResolveProxyAuthToken_NoOpWhenSecretRefEmpty's empty-token sibling:
// the common case where neither auth flag is set at all (raw manifests
// before an operator opts into either path).
func TestResolveProxyAuthToken_NoOpWhenBothEmpty(t *testing.T) {
	t.Parallel()

	cfg := &Config{}

	resolved, err := resolveProxyAuthToken(context.Background(), nil, cfg)
	require.NoError(t, err)
	assert.Empty(t, resolved.ProxyAuthToken)
}

// TestResolveProxyAuthToken_RejectsMalformedSecretRef pins the entry point's
// own error-propagation for an invalid --proxy-auth-secret-ref, not just the
// underlying parseProxyAuthSecretRef helper (already covered directly by
// TestParseProxyAuthSecretRef). resolveProxyAuthToken must return the parse
// error rather than panic or swallow it, and it must never touch mgr to get
// there: parsing happens before client.New(mgr.GetConfig(), ...), so mgr
// stays nil here exactly like the no-op tests above.
func TestResolveProxyAuthToken_RejectsMalformedSecretRef(t *testing.T) {
	t.Parallel()

	cfg := &Config{ProxyAuthSecretRef: "no-slash-here"}

	resolved, err := resolveProxyAuthToken(context.Background(), nil, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, errInvalidProxyAuthSecretRef)
	assert.Nil(t, resolved)
}

// TestEnsureProxyAuthSecret_GeneratesWhenMissing pins the secure-by-default
// case: no Secret exists yet at the shared plane's generated name, so
// ensureProxyAuthSecret(generate=true) creates one with a random token and
// returns it.
func TestEnsureProxyAuthSecret_GeneratesWhenMissing(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	key := proxyAuthSecretKey()

	token, err := ensureProxyAuthSecret(context.Background(), fakeClient, key, generatedAuthTokenKey, true)
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	assert.Len(t, token, hex.EncodedLen(generatedAuthTokenBytes))

	var secret corev1.Secret
	require.NoError(t, fakeClient.Get(context.Background(), key, &secret))
	assert.Equal(t, []byte(token), secret.Data[generatedAuthTokenKey])
}

// TestEnsureProxyAuthSecret_ReusesExisting pins the upgrade-safety
// requirement: an already-present Secret's token -- however it got there,
// including a value nobody generated (an operator's own manual `kubectl
// create`, or a foreign value from an unrelated process) -- is returned
// verbatim and NEVER overwritten, so upgrading a release never rotates the
// token or rolls the proxy pods on its own. generate=true here on purpose:
// even when the caller WOULD be allowed to create, an existing Secret is
// still never touched.
//
// The interceptor makes this a structural guarantee, not an inference from
// "the assertion below still matches": ensureProxyAuthSecret is only ever
// granted Get and Create RBAC (see clusterrole.yaml/deploy/rbac/role.yaml --
// no update, no patch), and this test fails the moment the code path
// exercises either, regardless of what it would have written.
func TestEnsureProxyAuthSecret_ReusesExisting(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	key := proxyAuthSecretKey()
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Data:       map[string][]byte{generatedAuthTokenKey: []byte("already-here-token")},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			t.Fatal("ensureProxyAuthSecret must never Update an existing auth Secret")

			return nil
		},
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			t.Fatal("ensureProxyAuthSecret must never Patch an existing auth Secret")

			return nil
		},
	}).Build()

	token, err := ensureProxyAuthSecret(context.Background(), fakeClient, key, generatedAuthTokenKey, true)
	require.NoError(t, err)
	assert.Equal(t, "already-here-token", token)

	var secret corev1.Secret
	require.NoError(t, fakeClient.Get(context.Background(), key, &secret))
	assert.Equal(t, []byte("already-here-token"), secret.Data[generatedAuthTokenKey],
		"an existing token must never be overwritten")
}

// TestEnsureProxyAuthSecret_ErrorsOnMissingKey fails closed: a Secret at the
// expected name with no value at dataKey, or an empty one -- the realistic
// case is a hand-created Secret with a typo'd key name -- is a broken state,
// not something to silently paper over with an empty token (which would
// make the config API accept an empty Bearer value as valid, reopening
// exactly the hole this whole change closes).
func TestEnsureProxyAuthSecret_ErrorsOnMissingKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data map[string][]byte
	}{
		{name: "key absent", data: map[string][]byte{"wrong-key": []byte("irrelevant")}},
		{name: "key present but empty", data: map[string][]byte{generatedAuthTokenKey: {}}},
		{name: "no data at all", data: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))

			key := proxyAuthSecretKey()
			broken := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
				Data:       tt.data,
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(broken).Build()

			token, err := ensureProxyAuthSecret(context.Background(), fakeClient, key, generatedAuthTokenKey, true)
			require.Error(t, err)
			assert.ErrorIs(t, err, errProxyAuthSecretMissingKey)
			assert.Empty(t, token, "a failed resolution must never hand back a usable-looking token")
		})
	}
}

// TestEnsureProxyAuthSecret_CreateRaceReusesWinner covers two controller
// replicas (or a controller racing a manual `kubectl create`) both missing
// the Secret on their existence check and both attempting Create: the loser
// must NOT error out or overwrite -- it re-reads and returns whatever token
// actually landed, exactly like the per-Gateway
// ensureGeneratedAuthSecret/AlreadyExists path this mirrors.
func TestEnsureProxyAuthSecret_CreateRaceReusesWinner(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	key := proxyAuthSecretKey()
	winner := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Data:       map[string][]byte{generatedAuthTokenKey: []byte("winner-token")},
	}

	var gets int

	builder := fake.NewClientBuilder().WithScheme(scheme)
	fakeClient := builder.WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, gotKey client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isSecret := obj.(*corev1.Secret); isSecret && gotKey.Name == key.Name {
				gets++
				if gets == 1 {
					return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, gotKey.Name)
				}
			}

			return cl.Get(ctx, gotKey, obj, opts...)
		},
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isSecret := obj.(*corev1.Secret); isSecret {
				// Another writer already landed the Secret between our
				// existence check and our own create.
				if err := cl.Create(ctx, winner.DeepCopy(), opts...); err != nil {
					return errors.Wrap(err, "seeding create-race winner")
				}

				return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, obj.GetName())
			}

			return cl.Create(ctx, obj, opts...)
		},
	}).Build()

	token, err := ensureProxyAuthSecret(context.Background(), fakeClient, key, generatedAuthTokenKey, true)
	require.NoError(t, err)
	assert.Equal(t, "winner-token", token, "must reuse whichever token won the create race, not error or overwrite")
}

// TestEnsureProxyAuthSecret_BringYourOwnCustomKey pins the unified-mechanism
// requirement: a bring-your-own Secret (generate=false) is read at whatever
// data key the operator configured (proxy.authTokenSecretRef.key), not
// hardcoded to the chart-generated convention. This is the case that a
// single generate-always design would have broken: a BYO Secret using a
// non-default key would fail to resolve even though it is perfectly valid.
func TestEnsureProxyAuthSecret_BringYourOwnCustomKey(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	key := types.NamespacedName{Name: "my-own-auth-secret", Namespace: "cf-system"}
	byo := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Data:       map[string][]byte{"my-custom-key": []byte("byo-token")},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(byo).Build()

	token, err := ensureProxyAuthSecret(context.Background(), fakeClient, key, "my-custom-key", false)
	require.NoError(t, err)
	assert.Equal(t, "byo-token", token)
}

// TestEnsureProxyAuthSecret_BringYourOwnMissingFailsClosed pins the other
// half of the bring-your-own contract: generate=false NEVER creates a
// Secret, even when it is missing. Silently minting one at an
// operator-chosen name would misconfigure whatever they actually intended
// to point the controller at -- trading the unauthenticated hole this
// change closes for a quieter one (an authenticated config API whose token
// nobody who reads the operator's own Secret actually knows). The
// interceptor fails the test if Create is ever attempted, so this is a
// structural guarantee, not an inference from the returned error.
func TestEnsureProxyAuthSecret_BringYourOwnMissingFailsClosed(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	key := types.NamespacedName{Name: "my-own-auth-secret", Namespace: "cf-system"}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			t.Fatal("ensureProxyAuthSecret(generate=false) must never Create a missing bring-your-own Secret")

			return nil
		},
	}).Build()

	token, err := ensureProxyAuthSecret(context.Background(), fakeClient, key, "auth-token", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, errProxyAuthSecretNotFound)
	assert.Empty(t, token)
}

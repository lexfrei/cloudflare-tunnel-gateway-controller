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

// TestEnsureProxyAuthSecret_GeneratesWhenMissing pins the secure-by-default
// case: no Secret exists yet at the shared plane's generated name, so
// ensureProxyAuthSecret creates one with a random token and returns it.
func TestEnsureProxyAuthSecret_GeneratesWhenMissing(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	key := proxyAuthSecretKey()

	token, err := ensureProxyAuthSecret(context.Background(), fakeClient, key)
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	assert.Len(t, token, hex.EncodedLen(generatedAuthTokenBytes))

	var secret corev1.Secret
	require.NoError(t, fakeClient.Get(context.Background(), key, &secret))
	assert.Equal(t, []byte(token), secret.Data[generatedAuthTokenKey])
}

// TestEnsureProxyAuthSecret_ReusesExisting pins the upgrade-safety
// requirement: an already-present Secret's token is returned verbatim and
// NEVER regenerated, so upgrading a release never rotates the token or rolls
// the proxy pods on its own.
func TestEnsureProxyAuthSecret_ReusesExisting(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	key := proxyAuthSecretKey()
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Data:       map[string][]byte{generatedAuthTokenKey: []byte("already-here-token")},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()

	token, err := ensureProxyAuthSecret(context.Background(), fakeClient, key)
	require.NoError(t, err)
	assert.Equal(t, "already-here-token", token)

	var secret corev1.Secret
	require.NoError(t, fakeClient.Get(context.Background(), key, &secret))
	assert.Equal(t, []byte("already-here-token"), secret.Data[generatedAuthTokenKey],
		"an existing token must never be overwritten")
}

// TestEnsureProxyAuthSecret_ErrorsOnMissingKey fails closed: a Secret at the
// expected name with no auth-token key (or an empty one) is a broken state,
// not something to silently paper over with an empty token.
func TestEnsureProxyAuthSecret_ErrorsOnMissingKey(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	key := proxyAuthSecretKey()
	broken := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Data:       map[string][]byte{"wrong-key": []byte("irrelevant")},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(broken).Build()

	_, err := ensureProxyAuthSecret(context.Background(), fakeClient, key)
	require.Error(t, err)
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

	token, err := ensureProxyAuthSecret(context.Background(), fakeClient, key)
	require.NoError(t, err)
	assert.Equal(t, "winner-token", token, "must reuse whichever token won the create race, not error or overwrite")
}

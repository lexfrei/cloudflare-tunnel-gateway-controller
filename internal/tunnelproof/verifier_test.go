package tunnelproof_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelproof"
)

const (
	testAccount = "0123456789abcdef0123456789abcdef"
	testTunnel  = "22222222-2222-2222-2222-222222222222"
	testAPIKey  = "api-token"
)

// fakeTokenAPI serves GET accounts/{a}/cfd_tunnel/{t}/token the way Cloudflare
// does: the tunnel's genuine connector token as the result string.
type fakeTokenAPI struct {
	server *httptest.Server
	status atomic.Int32
	token  atomic.Value // string
	calls  atomic.Int32
}

func newFakeTokenAPI(t *testing.T, realToken string) *fakeTokenAPI {
	t.Helper()

	api := &fakeTokenAPI{}
	api.status.Store(http.StatusOK)
	api.token.Store(realToken)

	api.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		api.calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")

		want := "/accounts/" + testAccount + "/cfd_tunnel/" + testTunnel + "/token"
		if request.Method != http.MethodGet || request.URL.Path != want {
			writer.WriteHeader(http.StatusNotFound)
			_, _ = writer.Write([]byte(`{"success":false,"errors":[{"code":1003,"message":"not found"}],"messages":[],"result":null}`))

			return
		}

		status := int(api.status.Load())
		writer.WriteHeader(status)

		if status != http.StatusOK {
			_, _ = writer.Write([]byte(`{"success":false,"errors":[{"code":1000,"message":"failure"}],"messages":[],"result":null}`))

			return
		}

		body, _ := json.Marshal(map[string]any{
			"success": true, "errors": []any{}, "messages": []any{},
			"result": api.token.Load().(string),
		})
		_, _ = writer.Write(body)
	}))
	t.Cleanup(api.server.Close)

	return api
}

func (api *fakeTokenAPI) factory(apiToken string) *cloudflare.Client {
	return cloudflare.NewClient(
		option.WithAPIToken(apiToken),
		option.WithBaseURL(api.server.URL),
		option.WithMaxRetries(0),
	)
}

// encodeToken renders a connector token the way Cloudflare issues it.
func encodeToken(t *testing.T, account, tunnelID string, secret []byte) string {
	t.Helper()

	raw, err := json.Marshal(map[string]any{"a": account, "t": tunnelID, "s": secret})
	require.NoError(t, err)

	return base64.StdEncoding.EncodeToString(raw)
}

func parse(t *testing.T, token string) *tunnel.Token {
	t.Helper()

	parsed, err := tunnel.ParseTunnelToken(token)
	require.NoError(t, err)

	return parsed
}

// clock is a settable time source for TTL tests.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newVerifier(t *testing.T, api *fakeTokenAPI, clk *clock) *tunnelproof.Verifier {
	t.Helper()

	verifier := tunnelproof.NewVerifier(api.factory)

	if clk != nil {
		verifier.SetClock(clk.Now)
	}

	return verifier
}

var (
	realSecret  = []byte("the-genuine-tunnel-secret-32-bytes!")
	forgeSecret = []byte("a-secret-the-tenant-made-up-here")
)

func TestNewVerifier_NilFactoryProvesNothing(t *testing.T) {
	t.Parallel()

	genuine := encodeToken(t, testAccount, testTunnel, realSecret)

	proof := tunnelproof.NewVerifier(nil).Verify(context.Background(), testAPIKey, parse(t, genuine))
	assert.Equal(t, tunnelownership.ProofUnknown, proof, "a verifier that can ask nobody must fail closed")
}

func TestVerify_MatchingSecretIsVerified(t *testing.T) {
	t.Parallel()

	genuine := encodeToken(t, testAccount, testTunnel, realSecret)
	api := newFakeTokenAPI(t, genuine)

	proof := newVerifier(t, api, nil).Verify(context.Background(), testAPIKey, parse(t, genuine))
	assert.Equal(t, tunnelownership.ProofVerified, proof)
}

func TestVerify_SecretMismatchIsRefuted(t *testing.T) {
	t.Parallel()

	api := newFakeTokenAPI(t, encodeToken(t, testAccount, testTunnel, realSecret))
	forged := encodeToken(t, testAccount, testTunnel, forgeSecret)

	proof := newVerifier(t, api, nil).Verify(context.Background(), testAPIKey, parse(t, forged))
	assert.Equal(t, tunnelownership.ProofRefuted, proof,
		"knowing a tunnel's UUID and account is not holding its secret")
}

func TestVerify_UnknownTunnelIsRefuted(t *testing.T) {
	t.Parallel()

	api := newFakeTokenAPI(t, encodeToken(t, testAccount, testTunnel, realSecret))
	other := encodeToken(t, testAccount, "33333333-3333-3333-3333-333333333333", realSecret)

	proof := newVerifier(t, api, nil).Verify(context.Background(), testAPIKey, parse(t, other))
	assert.Equal(t, tunnelownership.ProofRefuted, proof)
}

// TestVerify_RejectedCredentialDecidesNothing pins that a 401 or 403 says
// something about the credential, not the claim. A revoked or mis-scoped API
// token cannot be told apart from one for the wrong account by status code, and
// refuting on it would tear down every dedicated plane using that credential.
func TestVerify_RejectedCredentialDecidesNothing(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			genuine := encodeToken(t, testAccount, testTunnel, realSecret)
			api := newFakeTokenAPI(t, genuine)
			api.status.Store(int32(status))

			proof := newVerifier(t, api, nil).Verify(context.Background(), testAPIKey, parse(t, genuine))
			assert.Equal(t, tunnelownership.ProofUnknown, proof)
		})
	}
}

// TestVerify_ConfirmationSurvivesARevokedCredential pins the consequence that
// matters: a confirmed claim stays confirmed when its credential stops working.
func TestVerify_ConfirmationSurvivesARevokedCredential(t *testing.T) {
	t.Parallel()

	genuine := encodeToken(t, testAccount, testTunnel, realSecret)
	api := newFakeTokenAPI(t, genuine)
	clk := &clock{now: time.Unix(1_000_000, 0)}
	verifier := newVerifier(t, api, clk)

	require.Equal(t, tunnelownership.ProofVerified, verifier.Verify(context.Background(), testAPIKey, parse(t, genuine)))

	api.status.Store(http.StatusUnauthorized)
	clk.now = clk.now.Add(2 * time.Hour)

	assert.Equal(t, tunnelownership.ProofVerified, verifier.Verify(context.Background(), testAPIKey, parse(t, genuine)))
}

// TestVerify_VerdictsAreKeptPerCredential pins that a verdict obtained with
// one credential is not reused for another: the check proves the access of the
// credential that will write.
func TestVerify_VerdictsAreKeptPerCredential(t *testing.T) {
	t.Parallel()

	genuine := encodeToken(t, testAccount, testTunnel, realSecret)
	api := newFakeTokenAPI(t, genuine)
	verifier := newVerifier(t, api, nil)

	require.Equal(t, tunnelownership.ProofVerified, verifier.Verify(context.Background(), "credential-a", parse(t, genuine)))
	require.Equal(t, tunnelownership.ProofVerified, verifier.Verify(context.Background(), "credential-b", parse(t, genuine)))
	assert.EqualValues(t, 2, api.calls.Load())
}

// TestRecheckInterval_ComesBackBeforeAConfirmationIsTwiceStale pins the
// cadence callers requeue on: shorter than a confirmation's lifetime, so a
// rotated secret is found within at most one lifetime plus this interval.
func TestRecheckInterval_ComesBackBeforeAConfirmationIsTwiceStale(t *testing.T) {
	t.Parallel()

	assert.Positive(t, tunnelproof.RecheckInterval)
	assert.LessOrEqual(t, tunnelproof.RecheckInterval, time.Hour)
}

func TestVerify_APIDownWithNewClaimIsUnknown(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			genuine := encodeToken(t, testAccount, testTunnel, realSecret)
			api := newFakeTokenAPI(t, genuine)
			api.status.Store(int32(status))

			proof := newVerifier(t, api, nil).Verify(context.Background(), testAPIKey, parse(t, genuine))
			assert.Equal(t, tunnelownership.ProofUnknown, proof,
				"an outage is not an answer, so it must neither prove nor refute")
		})
	}
}

func TestVerify_UnreachableAPIIsUnknown(t *testing.T) {
	t.Parallel()

	genuine := encodeToken(t, testAccount, testTunnel, realSecret)
	api := newFakeTokenAPI(t, genuine)
	api.server.Close()

	proof := newVerifier(t, api, nil).Verify(context.Background(), testAPIKey, parse(t, genuine))
	assert.Equal(t, tunnelownership.ProofUnknown, proof)
}

func TestVerify_EmptyCredentialIsUnknownWithoutACall(t *testing.T) {
	t.Parallel()

	genuine := encodeToken(t, testAccount, testTunnel, realSecret)
	api := newFakeTokenAPI(t, genuine)

	proof := newVerifier(t, api, nil).Verify(context.Background(), "", parse(t, genuine))
	assert.Equal(t, tunnelownership.ProofUnknown, proof)
	assert.Zero(t, api.calls.Load())
}

func TestVerify_VerifiedIsCachedThenRechecked(t *testing.T) {
	t.Parallel()

	genuine := encodeToken(t, testAccount, testTunnel, realSecret)
	api := newFakeTokenAPI(t, genuine)
	clk := &clock{now: time.Unix(1_000_000, 0)}
	verifier := newVerifier(t, api, clk)

	require.Equal(t, tunnelownership.ProofVerified, verifier.Verify(context.Background(), testAPIKey, parse(t, genuine)))
	require.Equal(t, tunnelownership.ProofVerified, verifier.Verify(context.Background(), testAPIKey, parse(t, genuine)))
	assert.EqualValues(t, 1, api.calls.Load(), "a fresh verdict must be served from the cache")

	// The tunnel's secret is rotated: after the TTL, the old token is refuted.
	api.token.Store(encodeToken(t, testAccount, testTunnel, forgeSecret))
	clk.now = clk.now.Add(2 * time.Hour)

	assert.Equal(t, tunnelownership.ProofRefuted, verifier.Verify(context.Background(), testAPIKey, parse(t, genuine)))
	assert.EqualValues(t, 2, api.calls.Load())
}

func TestVerify_APIDownKeepsAnExpiredVerifiedClaim(t *testing.T) {
	t.Parallel()

	genuine := encodeToken(t, testAccount, testTunnel, realSecret)
	api := newFakeTokenAPI(t, genuine)
	clk := &clock{now: time.Unix(1_000_000, 0)}
	verifier := newVerifier(t, api, clk)

	require.Equal(t, tunnelownership.ProofVerified, verifier.Verify(context.Background(), testAPIKey, parse(t, genuine)))

	api.status.Store(http.StatusServiceUnavailable)
	clk.now = clk.now.Add(48 * time.Hour)

	assert.Equal(t, tunnelownership.ProofVerified, verifier.Verify(context.Background(), testAPIKey, parse(t, genuine)),
		"only a definite answer demotes a verified claim; an outage must not evict it")
}

func TestVerify_RefutedIsCachedBriefly(t *testing.T) {
	t.Parallel()

	api := newFakeTokenAPI(t, encodeToken(t, testAccount, testTunnel, realSecret))
	forged := parse(t, encodeToken(t, testAccount, testTunnel, forgeSecret))
	clk := &clock{now: time.Unix(1_000_000, 0)}
	verifier := newVerifier(t, api, clk)

	require.Equal(t, tunnelownership.ProofRefuted, verifier.Verify(context.Background(), testAPIKey, forged))
	require.Equal(t, tunnelownership.ProofRefuted, verifier.Verify(context.Background(), testAPIKey, forged))
	assert.EqualValues(t, 1, api.calls.Load(), "a refutation must not be re-asked on every reconcile")

	// After the refutation expires, an outage leaves the claim unproven rather
	// than refuted: nothing definite was said this time.
	api.status.Store(http.StatusServiceUnavailable)
	clk.now = clk.now.Add(time.Hour)

	assert.Equal(t, tunnelownership.ProofUnknown, verifier.Verify(context.Background(), testAPIKey, forged))
}

// TestVerify_OutageIsAskedAgainAfterAShortPause pins the outage cadence: every
// arbitrating reconcile collects every claim, so asking again on each one
// would multiply calls to an API that is already failing, and a hanging API
// would hold every reconcile for the request timeout per claim.
func TestVerify_OutageIsAskedAgainAfterAShortPause(t *testing.T) {
	t.Parallel()

	genuine := encodeToken(t, testAccount, testTunnel, realSecret)
	api := newFakeTokenAPI(t, genuine)
	api.status.Store(http.StatusServiceUnavailable)
	clk := &clock{now: time.Unix(1_000_000, 0)}
	verifier := newVerifier(t, api, clk)

	require.Equal(t, tunnelownership.ProofUnknown, verifier.Verify(context.Background(), testAPIKey, parse(t, genuine)))
	require.Equal(t, tunnelownership.ProofUnknown, verifier.Verify(context.Background(), testAPIKey, parse(t, genuine)))
	assert.EqualValues(t, 1, api.calls.Load(), "a failed lookup must not be repeated at once")

	api.status.Store(http.StatusOK)
	clk.now = clk.now.Add(time.Minute)

	assert.Equal(t, tunnelownership.ProofVerified, verifier.Verify(context.Background(), testAPIKey, parse(t, genuine)),
		"recovery must be seen within a minute, not after a verdict's TTL")
	assert.EqualValues(t, 2, api.calls.Load())
}

// TestVerify_OutagePausesAnExpiredConfirmationToo pins the same cadence for a
// claim verified before the outage: it stays verified, and is not re-asked on
// every reconcile either.
func TestVerify_OutagePausesAnExpiredConfirmationToo(t *testing.T) {
	t.Parallel()

	genuine := encodeToken(t, testAccount, testTunnel, realSecret)
	api := newFakeTokenAPI(t, genuine)
	clk := &clock{now: time.Unix(1_000_000, 0)}
	verifier := newVerifier(t, api, clk)

	require.Equal(t, tunnelownership.ProofVerified, verifier.Verify(context.Background(), testAPIKey, parse(t, genuine)))

	api.status.Store(http.StatusServiceUnavailable)
	clk.now = clk.now.Add(2 * time.Hour)

	require.Equal(t, tunnelownership.ProofVerified, verifier.Verify(context.Background(), testAPIKey, parse(t, genuine)))
	require.Equal(t, tunnelownership.ProofVerified, verifier.Verify(context.Background(), testAPIKey, parse(t, genuine)))
	assert.EqualValues(t, 2, api.calls.Load(), "one re-check after expiry, then a pause")
}

// TestVerify_CacheKeyIsNotTheSecret pins that the cache never holds a secret:
// neither the claimant's nor the one Cloudflare returned.
func TestVerify_CacheKeyIsNotTheSecret(t *testing.T) {
	t.Parallel()

	genuine := encodeToken(t, testAccount, testTunnel, realSecret)
	api := newFakeTokenAPI(t, genuine)
	verifier := newVerifier(t, api, nil)

	require.Equal(t, tunnelownership.ProofVerified, verifier.Verify(context.Background(), testAPIKey, parse(t, genuine)))

	for _, key := range verifier.CacheKeys() {
		assert.NotContains(t, key, string(realSecret))
		assert.NotContains(t, key, base64.StdEncoding.EncodeToString(realSecret))
		assert.False(t, strings.Contains(key, testTunnel), "the key is a digest, not a readable identity")
	}
}

// TestVerify_KeyCoversTheIdentity pins that two tokens with one secret but
// different tunnels are separate verdicts.
func TestVerify_KeyCoversTheIdentity(t *testing.T) {
	t.Parallel()

	genuine := encodeToken(t, testAccount, testTunnel, realSecret)
	api := newFakeTokenAPI(t, genuine)
	verifier := newVerifier(t, api, nil)

	require.Equal(t, tunnelownership.ProofVerified, verifier.Verify(context.Background(), testAPIKey, parse(t, genuine)))

	other := encodeToken(t, testAccount, uuid.MustParse("33333333-3333-3333-3333-333333333333").String(), realSecret)
	assert.Equal(t, tunnelownership.ProofRefuted, verifier.Verify(context.Background(), testAPIKey, parse(t, other)))
}

// TestVerify_IdentityMustMatchNotJustTheSecret pins the account and tunnel half
// of the comparison: Cloudflare answering with a token for a different account
// or tunnel is a refutation even when the secret bytes happen to match.
func TestVerify_IdentityMustMatchNotJustTheSecret(t *testing.T) {
	t.Parallel()

	for name, issued := range map[string][2]string{
		"other account": {"fedcba9876543210fedcba9876543210", testTunnel},
		"other tunnel":  {testAccount, "33333333-3333-3333-3333-333333333333"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			claimant := encodeToken(t, testAccount, testTunnel, realSecret)
			api := newFakeTokenAPI(t, claimant)
			api.token.Store(encodeToken(t, issued[0], issued[1], realSecret))

			proof := newVerifier(t, api, nil).Verify(context.Background(), testAPIKey, parse(t, claimant))
			assert.Equal(t, tunnelownership.ProofRefuted, proof)
		})
	}
}

func TestVerify_StatusCodesThatDecideAndThatDoNot(t *testing.T) {
	t.Parallel()

	for status, want := range map[int]tunnelownership.Proof{
		http.StatusBadRequest:     tunnelownership.ProofRefuted,
		http.StatusRequestTimeout: tunnelownership.ProofUnknown,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			genuine := encodeToken(t, testAccount, testTunnel, realSecret)
			api := newFakeTokenAPI(t, genuine)
			api.status.Store(int32(status))

			assert.Equal(t, want, newVerifier(t, api, nil).Verify(context.Background(), testAPIKey, parse(t, genuine)))
		})
	}
}

func TestVerify_UnparsableIssuedTokenDecidesNothing(t *testing.T) {
	t.Parallel()

	genuine := encodeToken(t, testAccount, testTunnel, realSecret)
	api := newFakeTokenAPI(t, genuine)
	api.token.Store("not a connector token")

	assert.Equal(t, tunnelownership.ProofUnknown,
		newVerifier(t, api, nil).Verify(context.Background(), testAPIKey, parse(t, genuine)))
}

// TestVerify_MalformedAccountTagNeverReachesTheAPI pins the verifier's own
// guard: the tag is copied into the request, so a tag that is not an account
// ID is refuted without asking.
func TestVerify_MalformedAccountTagNeverReachesTheAPI(t *testing.T) {
	t.Parallel()

	api := newFakeTokenAPI(t, encodeToken(t, testAccount, testTunnel, realSecret))
	token := &tunnel.Token{
		AccountTag:   testAccount + "/x?",
		TunnelID:     uuid.MustParse(testTunnel),
		TunnelSecret: realSecret,
	}

	assert.Equal(t, tunnelownership.ProofRefuted, newVerifier(t, api, nil).Verify(context.Background(), testAPIKey, token))
	assert.Zero(t, api.calls.Load())
}

// TestVerify_CacheDropsWhatNoLongerNeedsKeeping pins what bounds the cache: an
// expired refutation goes, and so does a confirmation nobody has asked about
// for a whole confirmation lifetime after it lapsed, while a confirmation in
// use stays.
func TestVerify_CacheDropsWhatNoLongerNeedsKeeping(t *testing.T) {
	t.Parallel()

	genuine := encodeToken(t, testAccount, testTunnel, realSecret)
	forged := encodeToken(t, testAccount, testTunnel, forgeSecret)
	api := newFakeTokenAPI(t, genuine)
	clk := &clock{now: time.Unix(1_000_000, 0)}
	verifier := newVerifier(t, api, clk)

	require.Equal(t, tunnelownership.ProofRefuted, verifier.Verify(context.Background(), testAPIKey, parse(t, forged)))
	require.Equal(t, tunnelownership.ProofVerified, verifier.Verify(context.Background(), "idle-credential", parse(t, genuine)))
	require.Len(t, verifier.CacheKeys(), 2)

	clk.now = clk.now.Add(10 * time.Minute)
	require.Equal(t, tunnelownership.ProofVerified, verifier.Verify(context.Background(), testAPIKey, parse(t, genuine)))
	assert.Len(t, verifier.CacheKeys(), 2, "the expired refutation must go; both confirmations stay")

	clk.now = clk.now.Add(3 * time.Hour)
	require.Equal(t, tunnelownership.ProofVerified, verifier.Verify(context.Background(), testAPIKey, parse(t, genuine)))
	assert.Len(t, verifier.CacheKeys(), 1, "a confirmation unused for a lifetime past its lapse must go")
}

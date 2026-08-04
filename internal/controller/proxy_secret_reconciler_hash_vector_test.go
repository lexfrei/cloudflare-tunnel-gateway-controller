package controller

import "testing"

// TestHashSecretData_Vectors pins hashSecretData against hex digests
// computed independently of this codebase (Python hashlib.sha256 over
// the documented "key=" + value + NUL byte stream, sorted by key) so a
// silent algorithm change here is caught even if a future caller
// mirrors the same (now-wrong) behaviour on both sides of a comparison.
//
// These vectors are the lockstep contract for issue #537: the chart
// cannot reproduce this hash at `helm template` time (no Secret
// content is available without Helm's `lookup`, which returns empty
// outside a live install and is unavailable for the
// External-Secrets-Operator flow before the Secret exists). Keep this
// vector current if hashSecretData ever changes so a future chart-side
// implementation, wired through helm-unittest's lookup mocking, has a
// fixed target to pin against.
func TestHashSecretData_Vectors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data map[string][]byte
		want string
	}{
		{
			name: "single key",
			data: map[string][]byte{
				"tunnel-token": []byte("super-secret-tunnel-token-abc123"),
			},
			want: "a4aad21e76d5b68b1cbf9a3aed1c2daa87ac2a841b54a0e20906974ee3179273",
		},
		{
			name: "multiple keys sort before hashing",
			data: map[string][]byte{
				"b-key": []byte("second"),
				"a-key": []byte("first"),
			},
			want: "152f2fb5b38710f8a0781a1e86ba1c4a88cff54400032ce039997803365785cb",
		},
		{
			name: "empty data is the well-known empty-input SHA-256",
			data: map[string][]byte{},
			want: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := hashSecretData(tt.data); got != tt.want {
				t.Errorf("hashSecretData(%v) = %q, want %q", tt.data, got, tt.want)
			}
		})
	}
}

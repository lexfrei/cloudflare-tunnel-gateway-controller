package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCanonicalEscapedPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain path unchanged", in: "/a/b", want: "/a/b"},
		{name: "root", in: "/", want: "/"},
		{name: "trailing slash kept", in: "/a/b/", want: "/a/b/"},
		{name: "empty segments kept", in: "/a//b", want: "/a//b"},
		{name: "single dot segment removed", in: "/a/./b", want: "/a/b"},
		{name: "double dot removes previous segment", in: "/a/b/../c", want: "/a/c"},
		{name: "double dot above root stays at root", in: "/../a", want: "/a"},
		{name: "trailing double dot leaves directory", in: "/a/b/..", want: "/a/"},
		{name: "trailing single dot leaves directory", in: "/a/.", want: "/a/"},
		{name: "only double dot", in: "/a/..", want: "/"},
		{name: "double dot consumes an empty segment", in: "/a//../b", want: "/a/b"},
		{name: "dots inside a segment are data", in: "/a/..b/c.", want: "/a/..b/c."},
		{name: "percent-encoded dots are dot segments", in: "/a/b/%2e%2E/c", want: "/a/c"},
		{name: "percent-encoded unreserved is decoded", in: "/%61%7E%2D%5F", want: "/a~-_"},
		{name: "reserved escape kept with upper-case hex", in: "/a%2fb", want: "/a%2Fb"},
		{name: "non-ASCII escape kept with upper-case hex", in: "/caf%c3%a9", want: "/caf%C3%A9"},
		{name: "segment holding an escaped separator is one segment", in: "/x/a%2Fb/../c", want: "/x/c"},
		{name: "sub-delims untouched", in: "/a;b=c/d@e:f", want: "/a;b=c/d@e:f"},
		{name: "non-origin form untouched", in: "*", want: "*"},
		{name: "invalid escape kept", in: "/a%zz", want: "/a%zz"},
		{name: "truncated escape kept", in: "/a%4", want: "/a%4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, canonicalEscapedPath(tt.in))
		})
	}
}

func TestMatchForm(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "no escapes", in: "/a/b", want: "/a/b"},
		{name: "reserved escape decoded", in: "/users/%40me", want: "/users/@me"},
		{name: "non-ASCII escape decoded", in: "/caf%C3%A9", want: "/café"},
		{name: "escaped slash kept", in: "/a%2Fb", want: "/a%2Fb"},
		{name: "escaped percent kept", in: "/a%25", want: "/a%25"},
		{name: "invalid escape kept", in: "/a%zz", want: "/a%zz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, matchForm(tt.in))
		})
	}
}

func TestMatchForm_EscapedPercentStaysDistinctFromEscapedSlash(t *testing.T) {
	t.Parallel()

	assert.NotEqual(t, matchForm("/a%2Fb"), matchForm("/a%252Fb"))
}

func TestTrimMatchedPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		escaped string
		prefix  string
		want    string
	}{
		{name: "plain prefix", escaped: "/app/x", prefix: "/app", want: "/x"},
		{name: "escape inside the prefix", escaped: "/u%40/x%40", prefix: "/u@", want: "/x%40"},
		{name: "escape in the prefix value", escaped: "/u@/x", prefix: "/u%40", want: "/x"},
		{name: "multi-byte escape inside the prefix", escaped: "/b%C3%A9/x", prefix: "/b%C3%A9", want: "/x"},
		{name: "escaped slash right after the prefix", escaped: "/app%2Fx", prefix: "/app", want: "%2Fx"},
		{name: "not a prefix", escaped: "/other", prefix: "/app", want: "/other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, trimMatchedPrefix(tt.escaped, tt.prefix))
		})
	}
}

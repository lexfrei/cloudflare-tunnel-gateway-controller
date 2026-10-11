package proxy

import (
	"encoding/hex"
	"net/url"
	"strings"
)

// canonicalEscapedPath returns the RFC 3986 normal form of an escaped
// origin-form path: percent-encoded unreserved characters decoded and the
// remaining escapes upper-cased (section 6.2.2), then dot segments removed
// (section 5.2.4). An escaped "/" stays escaped and so stays inside its
// segment. Input that is not an origin-form path is returned as is.
func canonicalEscapedPath(escaped string) string {
	if !strings.HasPrefix(escaped, "/") {
		return escaped
	}

	normalized := normalizePercentEncoding(escaped)
	if !strings.Contains(normalized, "/.") {
		return normalized
	}

	return removeDotSegments(normalized)
}

// canonicalizeRequestPath rewrites u's path to its canonical form, so that
// matching, filters and the backend all see one path.
func canonicalizeRequestPath(u *url.URL) {
	escaped := u.EscapedPath()
	if canonical := canonicalEscapedPath(escaped); canonical != escaped {
		setEscapedPath(u, canonical)
	}
}

// setEscapedPath sets target's path from its escaped form, keeping Path and
// RawPath coherent the way url.Parse does. A value that does not unescape
// is taken as literal.
func setEscapedPath(target *url.URL, escaped string) {
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		target.Path, target.RawPath = escaped, ""

		return
	}

	target.Path, target.RawPath = decoded, ""
	if target.EscapedPath() != escaped {
		target.RawPath = escaped
	}
}

// matchForm is the form Exact and PathPrefix compare: a canonical escaped
// path with every escape decoded except "%2F" and "%25", so an escaped
// reserved character equals its literal while an escaped "/" stays inside
// its segment and an escaped "%" cannot form a new escape.
func matchForm(escaped string) string {
	if !strings.Contains(escaped, "%") {
		return escaped
	}

	var out strings.Builder

	out.Grow(len(escaped))

	for i := 0; i < len(escaped); i++ {
		if decoded, ok := matchDecodableEscape(escaped, i); ok {
			out.WriteByte(decoded)

			i += 2

			continue
		}

		out.WriteByte(escaped[i])
	}

	return out.String()
}

// trimMatchedPrefix returns what follows prefix in the escaped path, both
// compared in match form, or escaped itself when prefix does not lead it.
func trimMatchedPrefix(escaped, prefix string) string {
	want := matchForm(prefix)
	if !strings.HasPrefix(matchForm(escaped), want) {
		return escaped
	}

	i := 0
	for range len(want) {
		if _, ok := matchDecodableEscape(escaped, i); ok {
			i += 2
		}

		i++
	}

	return escaped[i:]
}

func matchDecodableEscape(escaped string, i int) (byte, bool) {
	decoded, ok := decodeEscape(escaped, i)

	return decoded, ok && decoded != '/' && decoded != '%'
}

// decodeEscape decodes the "%XX" escape at escaped[i], if there is one.
func decodeEscape(escaped string, i int) (byte, bool) {
	if escaped[i] != '%' || i+2 >= len(escaped) {
		return 0, false
	}

	decoded, err := hex.DecodeString(escaped[i+1 : i+3])
	if err != nil {
		return 0, false
	}

	return decoded[0], true
}

// escapedPathValue returns value as an escaped path: unchanged when it
// unescapes, otherwise escaped as a literal.
func escapedPathValue(value string) string {
	_, err := url.PathUnescape(value)
	if err != nil {
		return (&url.URL{Path: value}).EscapedPath()
	}

	return value
}

func normalizePercentEncoding(escaped string) string {
	if !strings.Contains(escaped, "%") {
		return escaped
	}

	var out strings.Builder

	out.Grow(len(escaped))

	for i := 0; i < len(escaped); i++ {
		decoded, ok := decodeEscape(escaped, i)

		switch {
		case !ok:
			out.WriteByte(escaped[i])

			continue
		case isUnreserved(decoded):
			out.WriteByte(decoded)
		default:
			out.WriteString(strings.ToUpper(escaped[i : i+3]))
		}

		i += 2
	}

	return out.String()
}

func removeDotSegments(path string) string {
	segments := strings.Split(path[1:], "/")
	out := make([]string, 0, len(segments))

	for i, segment := range segments {
		last := i == len(segments)-1

		switch segment {
		case ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, segment)

			continue
		}

		// A dot segment in last position still names a directory.
		if last {
			out = append(out, "")
		}
	}

	return "/" + strings.Join(out, "/")
}

func isUnreserved(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
		c == '-' || c == '.' || c == '_' || c == '~'
}

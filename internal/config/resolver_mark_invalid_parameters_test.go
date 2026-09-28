package config_test

import (
	stderrors "errors"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

var errInner = stderrors.New("inner cause")

// TestMarkInvalidParameters_BothTraversalsReachTheCause pins that the
// classification wrapper is transparent to both ways of walking an error
// chain: the standard library's multi-error Unwrap and cockroachdb's
// single-cause traversal, which UnwrapAll, mark equality and the detail and
// redaction helpers use.
func TestMarkInvalidParameters_BothTraversalsReachTheCause(t *testing.T) {
	t.Parallel()

	err := config.MarkInvalidParameters(errInner)

	assert.ErrorIs(t, err, errInner)
	assert.ErrorIs(t, err, config.ErrInvalidParameters)
	assert.Equal(t, errInner, errors.UnwrapAll(err), "cockroachdb's traversal must reach the cause")
	assert.Equal(t, errInner.Error(), err.Error(), "the message is the cause's, without the sentinel's text")
}

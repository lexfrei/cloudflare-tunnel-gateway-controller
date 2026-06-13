package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDrainSignalled pins the pre-dial drain check: a SIGTERM that lands
// before the tunnel dial must be observable without blocking, so
// runTunnelMode can skip the edge dial entirely instead of registering a
// connector only to immediately unregister it (burning termination grace).
func TestDrainSignalled(t *testing.T) {
	t.Parallel()

	open := make(chan struct{})
	assert.False(t, drainSignalled(open), "an open drain channel must not read as signalled")

	closed := make(chan struct{})
	close(closed)
	assert.True(t, drainSignalled(closed), "a closed drain channel must read as signalled")
}

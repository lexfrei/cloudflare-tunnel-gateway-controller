package logging_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
)

func TestRepeats(t *testing.T) {
	t.Parallel()

	repeats := logging.NewRepeats()

	repeats.NextPass()
	assert.Equal(t, slog.LevelError, repeats.Level("k", "boom", slog.LevelError), "first sighting")
	assert.Equal(t, slog.LevelDebug, repeats.Level("k", "boom", slog.LevelError), "same pass")
	assert.Equal(t, slog.LevelError, repeats.Level("other", "boom", slog.LevelError), "other key")

	repeats.NextPass()
	assert.Equal(t, slog.LevelDebug, repeats.Level("k", "boom", slog.LevelError), "seen last pass")
	assert.Equal(t, slog.LevelError, repeats.Level("k", "bang", slog.LevelError), "message changed")

	repeats.NextPass()
	repeats.NextPass()
	assert.Equal(t, slog.LevelError, repeats.Level("k", "bang", slog.LevelError), "absent for a pass")

	repeats.NextPass()
	repeats.Level("multi", "first", slog.LevelError)
	repeats.Level("multi", "second", slog.LevelError)
	repeats.NextPass()
	assert.Equal(t, slog.LevelDebug, repeats.Level("multi", "first", slog.LevelError), "one key, two messages")
	assert.Equal(t, slog.LevelDebug, repeats.Level("multi", "second", slog.LevelError), "one key, two messages")

	var unset *logging.Repeats

	unset.NextPass()
	assert.Equal(t, slog.LevelWarn, unset.Level("k", "boom", slog.LevelWarn), "nil logs everything")
}

func TestRepeatsFromContext(t *testing.T) {
	t.Parallel()

	repeats := logging.NewRepeats()

	assert.Same(t, repeats, logging.RepeatsFromContext(logging.WithRepeats(context.Background(), repeats)))
	assert.Nil(t, logging.RepeatsFromContext(context.Background()))
}

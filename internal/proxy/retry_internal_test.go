package proxy

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplayBody_Next(t *testing.T) {
	t.Parallel()

	t.Run("a body read to its end is replayed", func(t *testing.T) {
		t.Parallel()

		recorder := &replayBody{src: io.NopCloser(strings.NewReader("payload"))}
		first := recorder.attempt()

		data, err := io.ReadAll(first)
		require.NoError(t, err)
		assert.Equal(t, "payload", string(data))

		next, ok := recorder.next()
		require.True(t, ok)

		replayed, err := io.ReadAll(next)
		require.NoError(t, err)
		assert.Equal(t, "payload", string(replayed))

		_, err = first.Read(make([]byte, 1))
		require.ErrorIs(t, err, errAttemptAbandoned, "the abandoned attempt must not read the source again")
	})

	t.Run("an unread source is handed on", func(t *testing.T) {
		t.Parallel()

		recorder := &replayBody{src: io.NopCloser(strings.NewReader("payload"))}
		first := recorder.attempt()

		next, ok := recorder.next()
		require.True(t, ok)

		_, err := first.Read(make([]byte, 1))
		require.ErrorIs(t, err, errAttemptAbandoned)

		data, err := io.ReadAll(next)
		require.NoError(t, err)
		assert.Equal(t, "payload", string(data))
	})

	t.Run("a partly read source is not resent and stays with its attempt", func(t *testing.T) {
		t.Parallel()

		recorder := &replayBody{src: io.NopCloser(strings.NewReader("payload"))}
		first := recorder.attempt()

		_, err := first.Read(make([]byte, 3))
		require.NoError(t, err)

		_, ok := recorder.next()
		assert.False(t, ok)

		rest, err := io.ReadAll(first)
		require.NoError(t, err, "a refused hand-off must not cut the attempt that is still sending")
		assert.Equal(t, "load", string(rest))
	})

	t.Run("a body over the limit is not resent", func(t *testing.T) {
		t.Parallel()

		recorder := &replayBody{src: io.NopCloser(strings.NewReader(strings.Repeat("x", maxRetryBodyBytes+1)))}
		_, err := io.ReadAll(recorder.attempt())
		require.NoError(t, err)

		_, ok := recorder.next()
		assert.False(t, ok)
	})

	t.Run("a read still in progress blocks the hand-off", func(t *testing.T) {
		t.Parallel()

		src, writer := io.Pipe()
		t.Cleanup(func() { _ = writer.Close() })

		recorder := &replayBody{src: src}
		first := recorder.attempt()

		go func() { _, _ = first.Read(make([]byte, 1)) }()

		require.Eventually(t, func() bool {
			recorder.mu.Lock()
			defer recorder.mu.Unlock()

			return recorder.reading > 0
		}, time.Second, time.Millisecond)

		_, ok := recorder.next()
		assert.False(t, ok)
	})
}

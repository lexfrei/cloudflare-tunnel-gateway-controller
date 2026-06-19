package tunnel

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/cloudflare/cloudflared/connection"
)

func TestResolveGracePeriod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "zero falls back to default", in: 0, want: defaultGracePeriod},
		{name: "negative falls back to default", in: -time.Second, want: defaultGracePeriod},
		{name: "positive value passes through", in: 10 * time.Second, want: 10 * time.Second},
		{name: "above cloudflared cap is clamped", in: 5 * time.Minute, want: connection.MaxGracePeriod},
		{name: "exactly the cap passes through", in: connection.MaxGracePeriod, want: connection.MaxGracePeriod},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, resolveGracePeriod(tt.in))
		})
	}
}

func TestGraceChannel_ExplicitChannelIndependentOfContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	explicit := make(chan struct{})

	got := graceChannel(ctx, explicit)

	// Cancelling the context must NOT close an explicit grace channel —
	// the whole point of the split is that the drain trigger and the hard
	// context are independent.
	cancel()

	select {
	case <-got:
		t.Fatal("explicit grace channel closed by context cancellation")
	case <-time.After(50 * time.Millisecond):
	}

	close(explicit)

	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("explicit grace channel close not observed")
	}
}

func TestGraceChannel_NilDerivesFromContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	got := graceChannel(ctx, nil)

	select {
	case <-got:
		t.Fatal("derived grace channel closed before context cancellation")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()

	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("derived grace channel did not close on context cancellation")
	}
}

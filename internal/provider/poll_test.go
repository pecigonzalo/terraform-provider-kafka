package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPollUntil(t *testing.T) {
	errFetch := errors.New("metadata request failed")

	tests := []struct {
		name string
		// ctx returns the context to wait on and a cancel func to defer.
		ctx func(t *testing.T) (context.Context, context.CancelFunc)
		// results is what the fake done func returns on each call, in order.
		// The last entry repeats once the slice is exhausted.
		results []fakeResult
		// cancelOnCall cancels the context during the Nth call (1-based), 0 for never.
		cancelOnCall int
		wantCalls    int
		wantErr      error
	}{
		{
			name:      "succeeds_immediately",
			ctx:       background,
			results:   []fakeResult{{done: true}},
			wantCalls: 1,
		},
		{
			name:      "succeeds_after_polls",
			ctx:       background,
			results:   []fakeResult{{done: false}, {done: false}, {done: true}},
			wantCalls: 3,
		},
		{
			name:      "fetch_error_is_returned",
			ctx:       background,
			results:   []fakeResult{{done: false}, {err: errFetch}},
			wantCalls: 2,
			wantErr:   errFetch,
		},
		{
			name:      "times_out_when_deadline_passed",
			ctx:       expiredDeadline,
			results:   []fakeResult{{done: false}},
			wantCalls: 0,
			wantErr:   context.DeadlineExceeded,
		},
		{
			name:      "cancelled_before_start",
			ctx:       cancelledContext,
			results:   []fakeResult{{done: false}},
			wantCalls: 0,
			wantErr:   context.Canceled,
		},
		{
			name:         "cancelled_while_waiting",
			ctx:          background,
			results:      []fakeResult{{done: false}},
			cancelOnCall: 2,
			wantCalls:    2,
			wantErr:      context.Canceled,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.ctx(t)
			defer cancel()

			calls := 0
			fetch := func(context.Context) (bool, error) {
				calls++
				if tc.cancelOnCall == calls {
					cancel()
				}
				r := tc.results[min(calls, len(tc.results))-1]
				return r.done, r.err
			}

			// Interval 0 keeps the tests fast: no real sleeping happens.
			err := pollUntil(ctx, 0, fetch)

			require.Equal(t, tc.wantCalls, calls, "number of polls")
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

type fakeResult struct {
	done bool
	err  error
}

func background(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithCancel(context.Background())
}

func expiredDeadline(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
}

func cancelledContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx, cancel
}

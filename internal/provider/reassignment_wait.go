package provider

import (
	"context"
	"time"
)

// reassignmentPollInterval is how long to wait between checks on a replica
// reassignment that Kafka has not finished yet.
const reassignmentPollInterval = 2 * time.Second

// waitForReassignment calls done until it reports true. It sleeps interval
// between calls. It returns the first error from done, or ctx.Err() if ctx
// ends before done reports true.
func waitForReassignment(ctx context.Context, interval time.Duration, done func(context.Context) (bool, error)) error {
	for {
		// Check first so a cancelled context never starts another poll.
		if err := ctx.Err(); err != nil {
			return err
		}

		finished, err := done(ctx)
		if err != nil {
			return err
		}
		if finished {
			return nil
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

const (
	// readSettleInterval is how long to wait between re-reads of a topic that
	// does not match state yet.
	readSettleInterval = 300 * time.Millisecond

	// readSettleWindow bounds how long Read keeps re-reading. Brokers were
	// observed to lag for around a second; a difference that is still there
	// after this window is treated as a real change.
	readSettleWindow = 5 * time.Second
)

// configUnsettled reports whether any key tracked in state is missing from the
// configuration the broker reported, or has a different value there.
func configUnsettled(tracked types.Map, reported map[string]string) bool {
	for k, v := range tracked.Elements() {
		want, ok := v.(types.String)
		if !ok || want.IsNull() || want.IsUnknown() {
			continue
		}
		if got, found := reported[k]; !found || got != want.ValueString() {
			return true
		}
	}
	return false
}

// topicUnsettled reports whether what the broker reported for the topic
// disagrees with state: a different partition count or replication factor, or
// a tracked configuration key that is missing or has another value. Attributes
// that are null in state (for example right after an import) are not compared.
func topicUnsettled(info kafkaclient.TopicInfo, state *TopicResourceModel) bool {
	if !state.Partitions.IsNull() && !state.Partitions.IsUnknown() &&
		int64(len(info.Partitions)) != state.Partitions.ValueInt64() {
		return true
	}
	if !state.ReplicationFactor.IsNull() && !state.ReplicationFactor.IsUnknown() {
		// Replicas differing between partitions means a reassignment is still
		// in progress, which is also not settled.
		factor, err := replicaCount(info)
		if err != nil || int64(factor) != state.ReplicationFactor.ValueInt64() {
			return true
		}
	}
	return !state.Config.IsNull() && configUnsettled(state.Config, info.Config)
}

// minAbsentChecks is how many consecutive "topic not found" answers it takes
// before a topic is reported as gone. Fewer than that is treated as a lagging
// broker, not a deletion. With readSettleInterval between checks this is about
// two seconds, which is also how long a topic deleted outside Terraform takes
// to be noticed.
const minAbsentChecks = 8

// readTopicSettled returns the topic, re-reading for a short while if it
// disagrees with state (see topicUnsettled) or is reported as not found.
//
// A topic write returns once the controller accepted it, but other brokers can
// answer from older metadata for a moment, and the client may talk to a
// different broker on each request. kafka-go also serves metadata from a cache
// that a background refresh replaces wholesale with the answer of one broker,
// so a topic that was visible can briefly disappear again. A Read straight
// after a write could then see a setting missing or at its old value, the old
// partition count, or no topic at all, and report it as drift or as a
// deletion. Waiting after the write cannot fix that, because the next request
// can still reach a lagging broker, so Read retries instead.
//
// A change made outside Terraform costs the full window once, then is reported
// as drift. A topic is reported as gone (an error wrapping
// kafkaclient.ErrTopicNotFound) only after minAbsentChecks consecutive
// not-found answers within the window. If the window ends before that many
// checks, for example because the cluster is slow, the error does not wrap
// ErrTopicNotFound, so the resource is kept in state instead of being dropped.
func (r *topicResource) readTopicSettled(ctx context.Context, topic string, state *TopicResourceModel) (kafkaclient.TopicInfo, error) {
	return settleTopicRead(ctx, func(ctx context.Context) (kafkaclient.TopicInfo, error) {
		return r.kafka.Topic(ctx, topic)
	}, state, readSettleInterval, readSettleWindow)
}

// settleTopicRead implements readTopicSettled on top of read, so the retry
// behavior can be tested without a cluster.
func settleTopicRead(
	ctx context.Context,
	read func(context.Context) (kafkaclient.TopicInfo, error),
	state *TopicResourceModel,
	interval, window time.Duration,
) (kafkaclient.TopicInfo, error) {
	var (
		last    kafkaclient.TopicInfo // latest topic that was found
		found   bool
		absent  int // consecutive not-found answers
		lastErr error
	)
	// check reads once and reports whether to stop: the topic matches state, or
	// it has been absent often enough to be considered gone.
	check := func(ctx context.Context) (bool, error) {
		info, err := read(ctx)
		switch {
		case errors.Is(err, kafkaclient.ErrTopicNotFound):
			absent++
			lastErr = err
			return absent >= minAbsentChecks, nil
		case err != nil:
			return false, err
		}
		absent = 0
		last, found = info, true
		return !topicUnsettled(info, state), nil
	}

	if done, err := check(ctx); err != nil || done && found {
		return last, err
	}

	waitCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	pollErr := pollUntil(waitCtx, interval, check)
	// Running out of time is expected for a change that really happened: report
	// what the broker said last.
	if pollErr != nil && !errors.Is(pollErr, context.DeadlineExceeded) {
		return last, pollErr
	}
	switch {
	case absent >= minAbsentChecks:
		return kafkaclient.TopicInfo{}, lastErr
	case found:
		return last, nil
	default:
		// Deliberately not wrapping lastErr: matching ErrTopicNotFound would drop the resource.
		msg := fmt.Sprintf("topic not found, but the settle window ended after %d check(s) so it is not treated as deleted: %s",
			absent, lastErr.Error())
		return kafkaclient.TopicInfo{}, errors.New(msg)
	}
}

// waitForTopicVisible waits until a just-created topic can be read back.
// Creating a topic returns once the controller accepted it, but the broker that
// answers the next metadata request, or the client's cached metadata, can still
// predate it and report it as not found. Read retries a missing topic as well,
// so this only keeps the common case quick and does not need to prove stability.
//
// Running out of the settle window is not an error: the topic was created, so
// the apply succeeds and a later Read reports whatever the cluster says then.
func (r *topicResource) waitForTopicVisible(ctx context.Context, topic string) error {
	return waitForVisible(ctx, func(ctx context.Context) error {
		_, err := r.kafka.Topic(ctx, topic)
		return err
	}, readSettleInterval, readSettleWindow)
}

func waitForVisible(ctx context.Context, read func(context.Context) error, interval, window time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	err := pollUntil(waitCtx, interval, func(ctx context.Context) (bool, error) {
		err := read(ctx)
		if errors.Is(err, kafkaclient.ErrTopicNotFound) {
			return false, nil
		}
		return err == nil, err
	})
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

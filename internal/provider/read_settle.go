package provider

import (
	"context"
	"errors"
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
		factor, err := kafkaclientReplicaCount(info)
		if err != nil || int64(factor) != state.ReplicationFactor.ValueInt64() {
			return true
		}
	}
	return !state.Config.IsNull() && configUnsettled(state.Config, info.Config)
}

// readTopicSettled returns the topic, re-reading for a short while if it
// disagrees with state (see topicUnsettled).
//
// A topic write returns once the controller accepted it, but other brokers can
// answer from older metadata for a moment, and the client may talk to a
// different broker on each request. A Read straight after a write could then
// see a setting missing or at its old value, or the old partition count, and
// report it as drift. Waiting after the write cannot fix that, because the next
// request can still reach a lagging broker, so Read retries instead. A change
// made outside Terraform costs the full window once, then is reported as drift.
func (r *topicResource) readTopicSettled(ctx context.Context, topic string, state *TopicResourceModel) (kafkaclient.TopicInfo, error) {
	info, err := r.kafka.Topic(ctx, topic)
	if err == nil && !topicUnsettled(info, state) {
		return info, nil
	}
	if err != nil && !errors.Is(err, kafkaclient.ErrTopicNotFound) {
		return info, err
	}

	// A topic that is missing is retried as well: a broker that has not heard
	// about a recent create reports it as not found, and treating that as a
	// deletion would drop the resource from state. A topic that really is gone
	// costs the full window once.
	waitCtx, cancel := context.WithTimeout(ctx, readSettleWindow)
	defer cancel()
	pollErr := pollUntil(waitCtx, readSettleInterval, func(ctx context.Context) (bool, error) {
		info, err = r.kafka.Topic(ctx, topic)
		if errors.Is(err, kafkaclient.ErrTopicNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return !topicUnsettled(info, state), nil
	})
	// Running out of time is expected for a change that really happened: report
	// what the broker said last.
	if pollErr != nil && !errors.Is(pollErr, context.DeadlineExceeded) {
		return info, pollErr
	}
	return info, err
}

// waitForTopicVisible waits until the topic can be read back after creating
// it. Creating a topic returns once the controller accepted it, but the broker
// that answers the next metadata request, or the client's cached metadata, can
// still predate it and report the topic as not found. Without this wait the
// next Read would take that for a deletion and drop the resource from state.
//
// Running out of the settle window is not an error: the topic was created, so
// the apply succeeds and a later Read reports whatever the cluster says then.
func (r *topicResource) waitForTopicVisible(ctx context.Context, topic string) error {
	waitCtx, cancel := context.WithTimeout(ctx, readSettleWindow)
	defer cancel()
	err := pollUntil(waitCtx, readSettleInterval, func(ctx context.Context) (bool, error) {
		_, err := r.kafka.Topic(ctx, topic)
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

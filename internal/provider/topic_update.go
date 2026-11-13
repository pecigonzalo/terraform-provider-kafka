package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
	"github.com/pecigonzalo/terraform-provider-kafka/internal/placement"
)

// topicOperations is the part of the Kafka client the topic update logic
// needs. *kafkaclient.Client satisfies it; tests substitute a fake.
type topicOperations interface {
	Topic(ctx context.Context, name string) (kafkaclient.TopicInfo, error)
	Topics(ctx context.Context) ([]kafkaclient.TopicInfo, error)
	Brokers(ctx context.Context) ([]kafkaclient.BrokerInfo, error)
	CreatePartitions(ctx context.Context, topic string, total int, replicas [][]int) error
	AlterPartitionReassignments(ctx context.Context, topic string, assignments []kafkaclient.PartitionAssignment) error
	ListPartitionReassignments(ctx context.Context, topic string, partitions ...int) ([]kafkaclient.PartitionReassignment, error)
	AlterTopicConfig(ctx context.Context, name string, previous, config map[string]string) error
	Timeout() time.Duration
}

// topicUpdater changes the replication factor and partition count of an
// existing topic and waits for the cluster to reflect the change.
type topicUpdater struct {
	client topicOperations
	// reassignmentInterval and partitionInterval are the pauses between
	// checks while waiting for a reassignment or for added partitions.
	reassignmentInterval time.Duration
	partitionInterval    time.Duration
	// settleInterval and settleWindow set how often, and for how long, a topic
	// that is reported as not found is read again.
	settleInterval time.Duration
	settleWindow   time.Duration
}

func newTopicUpdater(client topicOperations) *topicUpdater {
	return &topicUpdater{
		client:               client,
		reassignmentInterval: reassignmentPollInterval,
		partitionInterval:    partitionPollInterval,
		settleInterval:       readSettleInterval,
		settleWindow:         readSettleWindow,
	}
}

// update applies the differences between state and plan: first the
// configuration, then the replication factor, then the partition count. It
// returns the model to save. When a step fails, the model holds the prior state
// with the attributes of the steps that succeeded already changed, so state
// matches the cluster, and the error says which step failed.
func (u *topicUpdater) update(ctx context.Context, plan, state TopicResourceModel) (TopicResourceModel, error) {
	saved := state
	topic := plan.Name.ValueString()

	if !plan.Config.Equal(state.Config) {
		tflog.Info(ctx, "Updating topic configuration")
		if err := u.client.AlterTopicConfig(ctx, topic, configMap(state.Config), configMap(plan.Config)); err != nil {
			return saved, fmt.Errorf("unable to update topic configuration: %w", err)
		}
		saved.Config = plan.Config
	}
	if !plan.ReplicationFactor.Equal(state.ReplicationFactor) {
		tflog.Info(ctx, "Updating topic replication factor")
		if err := u.replicationFactor(ctx, topic, int(plan.ReplicationFactor.ValueInt64())); err != nil {
			return saved, fmt.Errorf("unable to update topic replication factor: %w", err)
		}
		saved.ReplicationFactor = plan.ReplicationFactor
	}
	if !plan.Partitions.Equal(state.Partitions) {
		tflog.Info(ctx, "Updating topic partitions")
		if err := u.partitions(ctx, topic, int(plan.Partitions.ValueInt64())); err != nil {
			return saved, fmt.Errorf("unable to update topic partitions: %w", err)
		}
		saved.Partitions = plan.Partitions
	}
	return plan, nil
}

// readTopic reads the topic. A broker that has not caught up can report a
// topic as not found, so it reads again within the settle window. A topic that
// is still not found after the window gives an error that wraps
// [kafkaclient.ErrTopicNotFound]. Other errors are returned at once.
func (u *topicUpdater) readTopic(ctx context.Context, topic string) (kafkaclient.TopicInfo, error) {
	var (
		info    kafkaclient.TopicInfo
		lastErr error
	)
	waitCtx, cancel := context.WithTimeout(ctx, u.settleWindow)
	defer cancel()
	err := pollUntil(waitCtx, u.settleInterval, func(ctx context.Context) (bool, error) {
		got, err := u.client.Topic(ctx, topic)
		if errors.Is(err, kafkaclient.ErrTopicNotFound) {
			lastErr = err
			return false, nil
		}
		if err != nil {
			return false, err
		}
		info = got
		return true, nil
	})
	switch {
	case err == nil:
		return info, nil
	case lastErr != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		return kafkaclient.TopicInfo{}, lastErr
	default:
		return kafkaclient.TopicInfo{}, err
	}
}

// replicationFactor changes the replication factor of every partition of the
// topic to want. The live replica counts are used rather than state, which may
// be stale. Increasing only appends replicas, and decreasing only drops them,
// so no data moves beyond what the change needs. It returns once Kafka has
// finished reassigning.
func (u *topicUpdater) replicationFactor(ctx context.Context, topic string, want int) error {
	brokers, err := u.client.Brokers(ctx)
	if err != nil {
		return err
	}
	if want > len(brokers) {
		return errors.New("replication factor cannot be higher than the number of brokers")
	}

	info, err := u.readTopic(ctx, topic)
	if err != nil {
		return err
	}
	// Use the live replica count, not state, which may be stale.
	have, err := replicaCount(info)
	if err != nil {
		return err
	}
	if have == want {
		// The replica lists can already show the target while data still moves,
		// for example after an earlier attempt timed out. Wait for the
		// reassignment to end before reporting success.
		return u.waitForReassignment(ctx, topic, want)
	}

	current := placementPartitions(info)
	var assignments []placement.Assignment
	if want > have {
		others, err := u.otherPartitions(ctx, topic)
		if err != nil {
			return err
		}
		assignments, err = placement.IncreaseReplicationFactor(placementBrokers(brokers), current, others, want)
		if err != nil {
			return err
		}
	} else {
		assignments, err = placement.DecreaseReplicationFactor(current, want)
		if err != nil {
			return err
		}
	}

	request := make([]kafkaclient.PartitionAssignment, 0, len(assignments))
	for _, a := range assignments {
		request = append(request, kafkaclient.PartitionAssignment{Partition: a.Partition, Replicas: a.Replicas})
	}
	if err := u.client.AlterPartitionReassignments(ctx, topic, request); err != nil {
		return fmt.Errorf("errors changing replication factor: %w", err)
	}

	// The reassignment is asynchronous. Wait for it to finish so that the
	// next Read and the saved state reflect the cluster, not the plan.
	return u.waitForReassignment(ctx, topic, want)
}

// waitForReassignment waits until reassignmentDone reports true, for at most
// the client timeout.
func (u *topicUpdater) waitForReassignment(ctx context.Context, topic string, want int) error {
	timeout := u.client.Timeout()
	waitCtx, cancel := withOptionalTimeout(ctx, timeout)
	defer cancel()
	err := pollUntil(waitCtx, u.reassignmentInterval, func(ctx context.Context) (bool, error) {
		return u.reassignmentDone(ctx, topic, want)
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf(
			"timed out after %s waiting for Kafka to finish reassigning replicas of topic %q to replication factor %d; "+
				"the reassignment may still be in progress, so run terraform plan again to refresh the actual state: %w",
			timeout, topic, want, err,
		)
	}
	if err != nil {
		return fmt.Errorf("waiting for replica reassignment of topic %q: %w", topic, err)
	}
	return nil
}

// reassignmentDone reports whether every partition of the topic has the wanted
// replica count and no reassignment is still in progress. The replica counts
// are checked first: a broker with stale metadata, or a reassignment not yet
// visible, still reports the old counts, so it is never mistaken for done. A
// topic that cannot be found is treated as not settled for the same reason.
func (u *topicUpdater) reassignmentDone(ctx context.Context, topic string, want int) (bool, error) {
	info, err := u.client.Topic(ctx, topic)
	if errors.Is(err, kafkaclient.ErrTopicNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, p := range info.Partitions {
		if len(p.Replicas) != want {
			return false, nil
		}
	}
	inProgress, err := u.client.ListPartitionReassignments(ctx, topic)
	if err != nil {
		return false, err
	}
	return len(inProgress) == 0, nil
}

// partitions grows the topic to want partitions, giving the new partitions the
// topic's live replication factor. The live partition count is used rather
// than state, which may be stale. It returns once the topic reports the new
// partitions.
func (u *topicUpdater) partitions(ctx context.Context, topic string, want int) error {
	brokers, err := u.client.Brokers(ctx)
	if err != nil {
		return err
	}
	info, err := u.readTopic(ctx, topic)
	if err != nil {
		return err
	}

	// Use the live partition count, not state, which may be stale.
	have := len(info.Partitions)
	if want < have {
		return fmt.Errorf("partition count can't be reduced from %d to %d", have, want)
	}
	if want == have {
		return nil
	}

	factor, err := replicaCount(info)
	if err != nil {
		return err
	}
	others, err := u.otherPartitions(ctx, topic)
	if err != nil {
		return err
	}
	assignments, err := placement.ExtendPartitions(placementBrokers(brokers), placementPartitions(info), others, want-have, factor)
	if err != nil {
		return err
	}
	replicas := make([][]int, 0, len(assignments))
	for _, a := range assignments {
		replicas = append(replicas, a.Replicas)
	}

	if err := u.client.CreatePartitions(ctx, topic, want, replicas); err != nil {
		if !errors.Is(err, kafkaclient.ErrInvalidPartitions) {
			return err
		}
		// The count read above can come from cached metadata that predates
		// partitions added moments ago. If the topic already has enough
		// partitions the change has been made; otherwise the error is real.
		latest, readErr := u.client.Topic(ctx, topic)
		if readErr != nil || len(latest.Partitions) < want {
			return err
		}
	}

	return u.waitForPartitions(ctx, topic, want)
}

// waitForPartitions waits until the topic reports at least the wanted number of
// partitions. The client caches topic metadata for several seconds and brokers
// learn of the change asynchronously, so a Read straight after adding
// partitions can still see the old count; waiting here makes the saved state
// and anything reading the topic afterwards agree.
func (u *topicUpdater) waitForPartitions(ctx context.Context, topic string, want int) error {
	timeout := u.client.Timeout()
	waitCtx, cancel := withOptionalTimeout(ctx, timeout)
	defer cancel()

	err := pollUntil(waitCtx, u.partitionInterval, func(ctx context.Context) (bool, error) {
		info, err := u.client.Topic(ctx, topic)
		if errors.Is(err, kafkaclient.ErrTopicNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return len(info.Partitions) >= want, nil
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf(
			"timed out after %s waiting for topic %q to report %d partitions; "+
				"the partitions were added, so run terraform plan again to refresh the actual state: %w",
			timeout, topic, want, err,
		)
	}
	if err != nil {
		return fmt.Errorf("waiting for partitions of topic %q: %w", topic, err)
	}
	return nil
}

// otherPartitions returns the placement of every topic except the given one.
// The topic itself is excluded because the placement already accounts for it
// and it changes during the update.
func (u *topicUpdater) otherPartitions(ctx context.Context, topic string) ([]placement.Partition, error) {
	topics, err := u.client.Topics(ctx)
	if err != nil {
		return nil, err
	}
	var out []placement.Partition
	for _, t := range topics {
		if t.Name != topic {
			out = append(out, placementPartitions(t)...)
		}
	}
	return out, nil
}

// withOptionalTimeout bounds ctx by timeout, or returns it unchanged when
// timeout is not positive.
func withOptionalTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return ctx, func() {}
}

func placementBrokers(brokers []kafkaclient.BrokerInfo) []placement.Broker {
	out := make([]placement.Broker, 0, len(brokers))
	for _, b := range brokers {
		out = append(out, placement.Broker{ID: b.ID, Rack: b.Rack})
	}
	return out
}

func placementPartitions(topic kafkaclient.TopicInfo) []placement.Partition {
	out := make([]placement.Partition, 0, len(topic.Partitions))
	for _, p := range topic.Partitions {
		out = append(out, placement.Partition{ID: p.ID, Leader: p.Leader, Replicas: p.Replicas})
	}
	return out
}

// replicaCount returns the replication factor of a topic. It returns an error
// if the number of replicas differs across partitions, which also happens
// while a reassignment is in progress.
func replicaCount(topic kafkaclient.TopicInfo) (int, error) {
	count := -1
	for _, p := range topic.Partitions {
		if count == -1 {
			count = len(p.Replicas)
		}
		if count != len(p.Replicas) {
			return count, fmt.Errorf("the replica count isn't the same across partitions %d != %d", count, len(p.Replicas))
		}
	}
	return count, nil
}

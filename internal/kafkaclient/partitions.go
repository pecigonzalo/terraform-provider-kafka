package kafkaclient

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/segmentio/kafka-go"
)

// defaultRequestTimeout is the broker-side timeout sent with partition
// requests when the client has no timeout configured.
const defaultRequestTimeout = 30 * time.Second

var (
	// ErrInvalidPartitions is returned (wrapped) when the broker rejects a
	// partition count, for example because it would not increase the topic's
	// partition count.
	ErrInvalidPartitions = errors.New("invalid partition count")

	// ErrInvalidReplicaAssignment is returned (wrapped) when the broker
	// rejects a replica assignment, for example because it names an unknown
	// broker or repeats one.
	ErrInvalidReplicaAssignment = errors.New("invalid replica assignment")
)

// PartitionAssignment is the replica list for one partition. The first
// replica is the preferred leader.
type PartitionAssignment struct {
	Partition int
	Replicas  []int
}

// PartitionReassignment describes a partition whose replicas are being moved.
// Replicas is the current replica list; Adding and Removing are the replicas
// still being added or removed.
type PartitionReassignment struct {
	Partition int
	Replicas  []int
	Adding    []int
	Removing  []int
}

// CreatePartitions grows a topic to total partitions. replicas holds the
// replica list for each new partition, in partition order, so its length must
// be the number of partitions being added: total minus the current count.
//
// It returns errors wrapping [ErrTopicNotFound], [ErrInvalidPartitions] or
// [ErrInvalidReplicaAssignment] for the matching broker rejections.
//
// kafka-go provides CreatePartitions, so this wraps it and adds argument
// validation, a check of the per-topic result and error classification.
func (c *Client) CreatePartitions(ctx context.Context, topic string, total int, replicas [][]int) error {
	if err := validateCreatePartitions(topic, total, replicas); err != nil {
		return fmt.Errorf("create partitions of topic %q: %w", topic, err)
	}

	assignments := make([]kafka.TopicPartitionAssignment, len(replicas))
	for i, rs := range replicas {
		ids := make([]int32, len(rs))
		for j, id := range rs {
			ids[j] = int32(id)
		}
		assignments[i] = kafka.TopicPartitionAssignment{BrokerIDs: ids}
	}
	resp, err := c.kc.CreatePartitions(ctx, &kafka.CreatePartitionsRequest{
		Topics: []kafka.TopicPartitionsConfig{{
			Name:                      topic,
			Count:                     int32(total),
			TopicPartitionAssignments: assignments,
		}},
	})
	if err != nil {
		return fmt.Errorf("create partitions of topic %q: %w", topic, err)
	}
	topicErr, ok := resp.Errors[topic]
	if !ok {
		return fmt.Errorf("create partitions of topic %q: the broker returned no result for the topic", topic)
	}
	if topicErr != nil {
		return fmt.Errorf("create partitions of topic %q: %w", topic, classifyPartitionError(topicErr))
	}
	return nil
}

func validateCreatePartitions(topic string, total int, replicas [][]int) error {
	switch {
	case topic == "":
		return errors.New("topic name must not be empty")
	case len(replicas) == 0:
		return errors.New("at least one new partition assignment is required")
	case total <= len(replicas):
		return fmt.Errorf("total partitions %d must exceed the %d new assignments", total, len(replicas))
	}
	for i, rs := range replicas {
		if err := validateReplicas(rs); err != nil {
			return fmt.Errorf("new partition %d: %w", i, err)
		}
		if len(rs) != len(replicas[0]) {
			return fmt.Errorf("new partition %d has %d replicas, others have %d", i, len(rs), len(replicas[0]))
		}
	}
	return nil
}

func validateReplicas(replicas []int) error {
	if len(replicas) == 0 {
		return errors.New("replica list must not be empty")
	}
	for i, id := range replicas {
		if id < 0 {
			return fmt.Errorf("broker id %d is negative", id)
		}
		if slices.Contains(replicas[:i], id) {
			return fmt.Errorf("broker %d appears more than once", id)
		}
	}
	return nil
}

// AlterPartitionReassignments sets the replica list of the given partitions.
// The cluster applies the change asynchronously: the call returns once the
// broker has accepted it, and [Reader.ListPartitionReassignments] reports
// whether it is still in progress.
//
// The request carries a broker-side timeout derived from the client's
// configured timeout (half of it, or 30s when none is set). kafka-go sends the request's own Timeout field as the
// broker's timeoutMs, and a zero value makes the broker answer
// REQUEST_TIMED_OUT before accepting the change.
//
// It returns errors wrapping [ErrTopicNotFound] or
// [ErrInvalidReplicaAssignment] for the matching broker rejections.
func (c *Client) AlterPartitionReassignments(ctx context.Context, topic string, assignments []PartitionAssignment) error {
	if topic == "" {
		return errors.New("alter partition reassignments: topic name must not be empty")
	}
	if len(assignments) == 0 {
		return fmt.Errorf("alter partition reassignments of topic %q: at least one assignment is required", topic)
	}
	reqAssignments := make([]kafka.AlterPartitionReassignmentsRequestAssignment, len(assignments))
	for i, a := range assignments {
		if err := validateReplicas(a.Replicas); err != nil {
			return fmt.Errorf("alter partition reassignments of topic %q: partition %d: %w", topic, a.Partition, err)
		}
		reqAssignments[i] = kafka.AlterPartitionReassignmentsRequestAssignment{
			PartitionID: a.Partition,
			BrokerIDs:   slices.Clone(a.Replicas),
		}
	}

	resp, err := c.kc.AlterPartitionReassignments(ctx, &kafka.AlterPartitionReassignmentsRequest{
		Topic:       topic,
		Assignments: reqAssignments,
		Timeout:     c.requestTimeout(),
	})
	if err != nil {
		return fmt.Errorf("alter partition reassignments of topic %q: %w", topic, err)
	}
	var errs []error
	if resp.Error != nil {
		errs = append(errs, classifyPartitionError(resp.Error))
	}
	for _, pr := range resp.PartitionResults {
		if pr.Error != nil {
			errs = append(errs, fmt.Errorf("partition %d: %w", pr.PartitionID, classifyPartitionError(pr.Error)))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("alter partition reassignments of topic %q: %w", topic, errors.Join(errs...))
	}
	return nil
}

// ListPartitionReassignments returns the reassignments in progress for the
// given partitions of a topic, or for all partitions of the topic when none
// are given. Partitions that are not being reassigned are omitted, so an
// empty result means every listed partition has finished moving.
func (r *Reader) ListPartitionReassignments(ctx context.Context, topic string, partitions ...int) ([]PartitionReassignment, error) {
	if topic == "" {
		return nil, errors.New("list partition reassignments: topic name must not be empty")
	}
	if len(partitions) == 0 {
		// An empty index list asks the broker for nothing, so look the
		// partitions up.
		md, err := r.Metadata(ctx, topic)
		if err != nil {
			return nil, fmt.Errorf("list partition reassignments of topic %q: %w", topic, err)
		}
		for _, t := range md.Topics {
			if t.Name != topic {
				continue
			}
			if t.Error != nil {
				return nil, fmt.Errorf("list partition reassignments of topic %q: %w", topic, classifyPartitionError(t.Error))
			}
			for _, p := range t.Partitions {
				partitions = append(partitions, p.ID)
			}
		}
		if len(partitions) == 0 {
			return nil, fmt.Errorf("list partition reassignments of topic %q: %w", topic, ErrTopicNotFound)
		}
	}

	resp, err := r.kc.ListPartitionReassignments(ctx, &kafka.ListPartitionReassignmentsRequest{
		Topics:  map[string]kafka.ListPartitionReassignmentsRequestTopic{topic: {PartitionIndexes: partitions}},
		Timeout: r.requestTimeout(),
	})
	if err != nil {
		return nil, fmt.Errorf("list partition reassignments of topic %q: %w", topic, err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("list partition reassignments of topic %q: %w", topic, classifyPartitionError(resp.Error))
	}
	var out []PartitionReassignment
	for _, p := range resp.Topics[topic].Partitions {
		out = append(out, PartitionReassignment{
			Partition: p.PartitionIndex,
			Replicas:  p.Replicas,
			Adding:    p.AddingReplicas,
			Removing:  p.RemovingReplicas,
		})
	}
	slices.SortFunc(out, func(a, b PartitionReassignment) int { return a.Partition - b.Partition })
	return out, nil
}

// requestTimeout is the timeout sent to the broker with requests that carry
// one. It is half the client's configured timeout, leaving the other half for
// the response to travel back before the client gives up (kafka-go does the
// same for the requests it times itself), or a default when none is set.
func (r *Reader) requestTimeout() time.Duration {
	if t := r.Timeout(); t > 0 {
		return t / 2
	}
	return defaultRequestTimeout
}

// classifyPartitionError adds the package's sentinel errors to the matching
// broker error codes while keeping the original error in the chain.
func classifyPartitionError(err error) error {
	switch {
	case errors.Is(err, kafka.UnknownTopicOrPartition):
		return fmt.Errorf("%w: %w", ErrTopicNotFound, err)
	case errors.Is(err, kafka.InvalidPartitionNumber):
		return fmt.Errorf("%w: %w", ErrInvalidPartitions, err)
	case errors.Is(err, kafka.InvalidReplicaAssignment):
		return fmt.Errorf("%w: %w", ErrInvalidReplicaAssignment, err)
	}
	return err
}

package kafkaclient

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"
)

// defaultRequestTimeout is the broker-side timeout sent with partition
// requests when the client has no timeout configured.
const defaultRequestTimeout = 30 * time.Second

// minRequestTimeout is the smallest broker-side timeout sent with a request.
const minRequestTimeout = time.Second

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
// The call checks the arguments before it sends the request. It then checks the
// result for the topic, and it returns an error when the broker gives none.
func (c *Client) CreatePartitions(ctx context.Context, topic string, total int, replicas [][]int) error {
	if err := validateCreatePartitions(topic, total, replicas); err != nil {
		return fmt.Errorf("create partitions of topic %q: %w", topic, err)
	}

	assignments := make([]kafka.TopicPartitionAssignment, len(replicas))
	for i, rs := range replicas {
		ids := make([]int32, len(rs))
		for j, id := range rs {
			ids[j] = int32(id) //nolint:gosec // validateReplicas checks the range
		}
		assignments[i] = kafka.TopicPartitionAssignment{BrokerIDs: ids}
	}
	resp, err := c.kc.CreatePartitions(ctx, &kafka.CreatePartitionsRequest{
		Topics: []kafka.TopicPartitionsConfig{{
			Name:                      topic,
			Count:                     int32(total), //nolint:gosec // validateCreatePartitions checks the range
			TopicPartitionAssignments: assignments,
		}},
	})
	if err != nil {
		return fmt.Errorf("create partitions of topic %q: %w", topic, err)
	}
	return checkResults("create partitions of topic", []string{topic}, resp.Errors)
}

func validateCreatePartitions(topic string, total int, replicas [][]int) error {
	switch {
	case topic == "":
		return errors.New("topic name must not be empty")
	case len(replicas) == 0:
		return errors.New("at least one new partition assignment is required")
	case total > math.MaxInt32:
		return fmt.Errorf("total partitions %d is more than %d", total, math.MaxInt32)
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
		if id > math.MaxInt32 {
			return fmt.Errorf("broker id %d is more than %d", id, math.MaxInt32)
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
// configured timeout (half of it with a one second floor, or 30s when none is set). kafka-go sends
// the request's own Timeout field as the broker's timeoutMs, and a zero value makes the broker answer
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
	seen := make(map[int]bool, len(assignments))
	for i, a := range assignments {
		if a.Partition < 0 {
			return fmt.Errorf("alter partition reassignments of topic %q: partition number %d is negative", topic, a.Partition)
		}
		if seen[a.Partition] {
			return fmt.Errorf("alter partition reassignments of topic %q: partition %d appears more than once", topic, a.Partition)
		}
		seen[a.Partition] = true
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
	results := make(map[string]error, len(resp.PartitionResults))
	for _, pr := range resp.PartitionResults {
		results[strconv.Itoa(pr.PartitionID)] = pr.Error
	}
	var errs []error
	if resp.Error != nil {
		errs = append(errs, classifyBrokerError(resp.Error))
	}
	errs = append(errs, checkResults("partition", nil, results))
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("alter partition reassignments of topic %q: %w", topic, err)
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
	// An empty topic map asks the broker for every reassignment in the cluster,
	// which avoids looking partitions up in cached metadata that may predate a
	// partition just created; the result is filtered to the topic below.
	topics := map[string]kafka.ListPartitionReassignmentsRequestTopic{}
	if len(partitions) > 0 {
		topics[topic] = kafka.ListPartitionReassignmentsRequestTopic{PartitionIndexes: partitions}
	}
	resp, err := r.kc.ListPartitionReassignments(ctx, &kafka.ListPartitionReassignmentsRequest{
		Topics:  topics,
		Timeout: r.requestTimeout(),
	})
	if err != nil {
		return nil, fmt.Errorf("list partition reassignments of topic %q: %w", topic, err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("list partition reassignments of topic %q: %w", topic, classifyBrokerError(resp.Error))
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
		// Floor at one second: a sub-millisecond value truncates to a zero
		// timeoutMs, which the broker rejects. The floor may exceed half the
		// client timeout, which is harmless since the client gives up first.
		return max(t/2, minRequestTimeout)
	}
	return defaultRequestTimeout
}

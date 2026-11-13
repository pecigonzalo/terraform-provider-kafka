package kafkaclient

import (
	"context"
	"errors"
	"math"
	"net"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/protocol/alterpartitionreassignments"
	"github.com/segmentio/kafka-go/protocol/createpartitions"
	"github.com/segmentio/kafka-go/protocol/listpartitionreassignments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// partitionsFakeTransport answers the partition APIs with canned responses
// and records what it was sent.
type partitionsFakeTransport struct {
	err    error
	create *createpartitions.Response
	alter  *alterpartitionreassignments.Response
	list   *listpartitionreassignments.Response

	creates []*createpartitions.Request
	alters  []*alterpartitionreassignments.Request
	lists   []*listpartitionreassignments.Request
}

func (f *partitionsFakeTransport) RoundTrip(_ context.Context, _ net.Addr, req kafka.Request) (kafka.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	switch r := req.(type) {
	case *createpartitions.Request:
		f.creates = append(f.creates, r)
		return f.create, nil
	case *alterpartitionreassignments.Request:
		f.alters = append(f.alters, r)
		return f.alter, nil
	case *listpartitionreassignments.Request:
		f.lists = append(f.lists, r)
		return f.list, nil
	}
	return nil, errors.New("unexpected request")
}

func newPartitionsClient(t *testing.T, timeout time.Duration, rt kafka.RoundTripper) *Client {
	t.Helper()
	c, err := newClient(t.Context(), Config{BootstrapServer: "b:9092", Timeout: timeout}, fakeAWSLoader)
	require.NoError(t, err)
	c.kc.Transport = rt
	return c
}

func TestCreatePartitions(t *testing.T) {
	rt := &partitionsFakeTransport{create: &createpartitions.Response{
		Results: []createpartitions.ResponseResult{{Name: "t"}},
	}}
	c := newPartitionsClient(t, 10*time.Second, rt)

	require.NoError(t, c.CreatePartitions(t.Context(), "t", 4, [][]int{{1, 2}, {3, 1}}))

	require.Len(t, rt.creates, 1)
	req := rt.creates[0]
	require.Len(t, req.Topics, 1)
	assert.Equal(t, "t", req.Topics[0].Name)
	assert.Equal(t, int32(4), req.Topics[0].Count)
	assert.Equal(t, []createpartitions.RequestAssignment{{BrokerIDs: []int32{1, 2}}, {BrokerIDs: []int32{3, 1}}}, req.Topics[0].Assignments)
	assert.Equal(t, int32(5000), req.TimeoutMs, "half the configured timeout")
}

func TestCreatePartitions_responseErrors(t *testing.T) {
	tests := []struct {
		name    string
		results []createpartitions.ResponseResult
		want    error
	}{
		{"unknown_topic", []createpartitions.ResponseResult{{Name: "t", ErrorCode: int16(kafka.UnknownTopicOrPartition)}}, ErrTopicNotFound},
		{"invalid_partitions", []createpartitions.ResponseResult{{Name: "t", ErrorCode: int16(kafka.InvalidPartitionNumber), ErrorMessage: "must increase"}}, ErrInvalidPartitions},
		{"invalid_assignment", []createpartitions.ResponseResult{{Name: "t", ErrorCode: int16(kafka.InvalidReplicaAssignment)}}, ErrInvalidReplicaAssignment},
		{"other_error_keeps_kafka_error", []createpartitions.ResponseResult{{Name: "t", ErrorCode: int16(kafka.ClusterAuthorizationFailed)}}, kafka.ClusterAuthorizationFailed},
		{"missing_result", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &partitionsFakeTransport{create: &createpartitions.Response{Results: tc.results}}
			err := newPartitionsClient(t, time.Second, rt).CreatePartitions(t.Context(), "t", 3, [][]int{{1}})
			require.Error(t, err)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			}
		})
	}
}

func TestCreatePartitions_transportError(t *testing.T) {
	boom := errors.New("connection reset")
	c := newPartitionsClient(t, time.Second, &partitionsFakeTransport{err: boom})
	err := c.CreatePartitions(t.Context(), "t", 3, [][]int{{1}})
	require.ErrorIs(t, err, boom)
}

func TestCreatePartitions_validation(t *testing.T) {
	tests := []struct {
		name     string
		topic    string
		total    int
		replicas [][]int
	}{
		{"empty_topic", "", 3, [][]int{{1}}},
		{"no_assignments", "t", 3, nil},
		{"total_not_above_new_count", "t", 1, [][]int{{1}}},
		{"empty_replica_list", "t", 3, [][]int{{}}},
		{"duplicate_broker", "t", 3, [][]int{{1, 1}}},
		{"negative_broker", "t", 3, [][]int{{-1}}},
		{"broker_above_int32", "t", 3, [][]int{{math.MaxInt32 + 1}}},
		{"total_above_int32", "t", math.MaxInt32 + 1, [][]int{{1}}},
		{"uneven_replication", "t", 4, [][]int{{1, 2}, {3}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &partitionsFakeTransport{}
			err := newPartitionsClient(t, time.Second, rt).CreatePartitions(t.Context(), tc.topic, tc.total, tc.replicas)
			require.Error(t, err)
			assert.Empty(t, rt.creates, "invalid input must not reach the broker")
		})
	}
}

func TestAlterPartitionReassignments(t *testing.T) {
	tests := []struct {
		name        string
		timeout     time.Duration
		wantTimeout int32
	}{
		{"configured_timeout_is_halved", 10 * time.Second, 5000},
		{"unset_timeout_uses_default", 0, 30000},
		{"tiny_timeout_is_floored", time.Millisecond, 1000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &partitionsFakeTransport{alter: &alterpartitionreassignments.Response{}}
			c := newPartitionsClient(t, tc.timeout, rt)

			err := c.AlterPartitionReassignments(t.Context(), "t", []PartitionAssignment{
				{Partition: 0, Replicas: []int{1, 2}},
				{Partition: 1, Replicas: []int{2, 3}},
			})
			require.NoError(t, err)

			require.Len(t, rt.alters, 1)
			req := rt.alters[0]
			assert.Equal(t, tc.wantTimeout, req.TimeoutMs, "a zero timeoutMs makes the broker answer REQUEST_TIMED_OUT")
			require.Len(t, req.Topics, 1)
			assert.Equal(t, "t", req.Topics[0].Name)
			assert.Equal(t, []alterpartitionreassignments.RequestPartition{
				{PartitionIndex: 0, Replicas: []int32{1, 2}},
				{PartitionIndex: 1, Replicas: []int32{2, 3}},
			}, req.Topics[0].Partitions)
		})
	}
}

func TestAlterPartitionReassignments_responseErrors(t *testing.T) {
	partErr := func(code kafka.Error) *alterpartitionreassignments.Response {
		return &alterpartitionreassignments.Response{Results: []alterpartitionreassignments.ResponseResult{{
			Name: "t",
			Partitions: []alterpartitionreassignments.ResponsePartition{
				{PartitionIndex: 0},
				{PartitionIndex: 1, ErrorCode: int16(code), ErrorMessage: "bad"},
			},
		}}}
	}
	tests := []struct {
		name string
		resp *alterpartitionreassignments.Response
		want error
	}{
		{"top_level_error", &alterpartitionreassignments.Response{ErrorCode: int16(kafka.RequestTimedOut)}, kafka.RequestTimedOut},
		{"partition_invalid_assignment", partErr(kafka.InvalidReplicaAssignment), ErrInvalidReplicaAssignment},
		{"partition_unknown_topic", partErr(kafka.UnknownTopicOrPartition), ErrTopicNotFound},
		{"partition_other_error", partErr(kafka.NotController), kafka.NotController},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &partitionsFakeTransport{alter: tc.resp}
			err := newPartitionsClient(t, time.Second, rt).AlterPartitionReassignments(t.Context(), "t",
				[]PartitionAssignment{{Partition: 1, Replicas: []int{1}}})
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestAlterPartitionReassignments_transportErrorAndValidation(t *testing.T) {
	boom := errors.New("connection reset")
	c := newPartitionsClient(t, time.Second, &partitionsFakeTransport{err: boom})
	require.ErrorIs(t, c.AlterPartitionReassignments(t.Context(), "t", []PartitionAssignment{{Replicas: []int{1}}}), boom)

	rt := &partitionsFakeTransport{}
	c = newPartitionsClient(t, time.Second, rt)
	require.Error(t, c.AlterPartitionReassignments(t.Context(), "", []PartitionAssignment{{Replicas: []int{1}}}))
	require.Error(t, c.AlterPartitionReassignments(t.Context(), "t", nil))
	require.Error(t, c.AlterPartitionReassignments(t.Context(), "t", []PartitionAssignment{{Replicas: []int{1, 1}}}))
	require.Error(t, c.AlterPartitionReassignments(t.Context(), "t", []PartitionAssignment{{Partition: -1, Replicas: []int{1}}}))
	require.Error(t, c.AlterPartitionReassignments(t.Context(), "t", []PartitionAssignment{
		{Partition: 0, Replicas: []int{1}}, {Partition: 0, Replicas: []int{2}},
	}))
	assert.Empty(t, rt.alters)
}

func TestAlterPartitionReassignments_doesNotMutateInput(t *testing.T) {
	rt := &partitionsFakeTransport{alter: &alterpartitionreassignments.Response{}}
	in := []PartitionAssignment{{Partition: 0, Replicas: []int{1, 2}}}
	require.NoError(t, newPartitionsClient(t, time.Second, rt).AlterPartitionReassignments(t.Context(), "t", in))
	assert.Equal(t, []PartitionAssignment{{Partition: 0, Replicas: []int{1, 2}}}, in)
}

func TestListPartitionReassignments(t *testing.T) {
	rt := &partitionsFakeTransport{list: &listpartitionreassignments.Response{
		Topics: []listpartitionreassignments.ResponseTopic{{
			Name: "t",
			Partitions: []listpartitionreassignments.ResponsePartition{
				{PartitionIndex: 2, Replicas: []int32{1, 2, 3}, AddingReplicas: []int32{3}},
				{PartitionIndex: 0, Replicas: []int32{1, 2}, RemovingReplicas: []int32{2}},
			},
		}},
	}}
	c := newPartitionsClient(t, 4*time.Second, rt)

	got, err := c.ListPartitionReassignments(t.Context(), "t", 0, 1, 2)
	require.NoError(t, err)

	assert.Equal(t, []PartitionReassignment{
		{Partition: 0, Replicas: []int{1, 2}, Removing: []int{2}},
		{Partition: 2, Replicas: []int{1, 2, 3}, Adding: []int{3}},
	}, got, "sorted by partition")
	require.Len(t, rt.lists, 1)
	assert.Equal(t, int32(2000), rt.lists[0].TimeoutMs)
	assert.Equal(t, []int32{0, 1, 2}, rt.lists[0].Topics[0].PartitionIndexes)
}

func TestListPartitionReassignments_nothingInProgress(t *testing.T) {
	rt := &partitionsFakeTransport{list: &listpartitionreassignments.Response{}}
	got, err := newPartitionsClient(t, time.Second, rt).ListPartitionReassignments(t.Context(), "t", 0)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListPartitionReassignments_allPartitionsWhenNoneGiven(t *testing.T) {
	rt := &partitionsFakeTransport{list: &listpartitionreassignments.Response{
		Topics: []listpartitionreassignments.ResponseTopic{
			{Name: "other", Partitions: []listpartitionreassignments.ResponsePartition{{PartitionIndex: 0}}},
			{Name: "t", Partitions: []listpartitionreassignments.ResponsePartition{{PartitionIndex: 5, Replicas: []int32{1}}}},
		},
	}}
	got, err := newPartitionsClient(t, time.Second, rt).ListPartitionReassignments(t.Context(), "t")
	require.NoError(t, err)
	require.Len(t, rt.lists, 1)
	assert.Empty(t, rt.lists[0].Topics, "no topic filter is sent, so partitions missing from cached metadata are covered")
	assert.Equal(t, []PartitionReassignment{{Partition: 5, Replicas: []int{1}}}, got, "other topics are filtered out")
}

func TestListPartitionReassignments_errors(t *testing.T) {
	t.Run("top_level_error", func(t *testing.T) {
		rt := &partitionsFakeTransport{list: &listpartitionreassignments.Response{ErrorCode: int16(kafka.ClusterAuthorizationFailed)}}
		_, err := newPartitionsClient(t, time.Second, rt).ListPartitionReassignments(t.Context(), "t", 0)
		require.ErrorIs(t, err, kafka.ClusterAuthorizationFailed)
	})
	t.Run("transport_error", func(t *testing.T) {
		boom := errors.New("connection reset")
		_, err := newPartitionsClient(t, time.Second, &partitionsFakeTransport{err: boom}).ListPartitionReassignments(t.Context(), "t", 0)
		require.ErrorIs(t, err, boom)
	})
	t.Run("empty_topic", func(t *testing.T) {
		_, err := newPartitionsClient(t, time.Second, &partitionsFakeTransport{}).ListPartitionReassignments(t.Context(), "")
		require.Error(t, err)
	})
}

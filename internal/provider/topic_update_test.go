package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

// fakeTopicOps is an in-memory topicOperations. The topic is read from
// topics, and writes are recorded rather than applied, so each test decides
// what the cluster reports after a write.
type fakeTopicOps struct {
	brokers []kafkaclient.BrokerInfo
	topics  []kafkaclient.TopicInfo
	timeout time.Duration

	// topicReads counts Topic calls. topicFn, when set, answers them instead of
	// looking the topic up in topics.
	topicReads int
	topicFn    func(call int) (kafkaclient.TopicInfo, error)

	// listFn answers ListPartitionReassignments by call number (1-based).
	listFn    func(call int) ([]kafkaclient.PartitionReassignment, error)
	listCalls int

	alterErr  error
	createErr error

	// configErr is returned by AlterTopicConfig. configCalls counts the calls
	// and configPrevious and configNew hold the arguments of the last one.
	configErr      error
	configCalls    int
	configPrevious map[string]string
	configNew      map[string]string

	altered      []kafkaclient.PartitionAssignment
	created      [][]int
	createdTotal int
	createCalls  int
	alterCalls   int
	topicsCalls  int
}

func (f *fakeTopicOps) Topic(_ context.Context, name string) (kafkaclient.TopicInfo, error) {
	f.topicReads++
	if f.topicFn != nil {
		return f.topicFn(f.topicReads)
	}
	for _, t := range f.topics {
		if t.Name == name {
			return t, nil
		}
	}
	return kafkaclient.TopicInfo{}, kafkaclient.ErrTopicNotFound
}

func (f *fakeTopicOps) Topics(context.Context) ([]kafkaclient.TopicInfo, error) {
	f.topicsCalls++
	return f.topics, nil
}

func (f *fakeTopicOps) Brokers(context.Context) ([]kafkaclient.BrokerInfo, error) {
	return f.brokers, nil
}

func (f *fakeTopicOps) CreatePartitions(_ context.Context, _ string, total int, replicas [][]int) error {
	f.createCalls++
	f.createdTotal, f.created = total, replicas
	return f.createErr
}

func (f *fakeTopicOps) AlterPartitionReassignments(_ context.Context, _ string, a []kafkaclient.PartitionAssignment) error {
	f.alterCalls++
	f.altered = a
	return f.alterErr
}

func (f *fakeTopicOps) ListPartitionReassignments(context.Context, string, ...int) ([]kafkaclient.PartitionReassignment, error) {
	f.listCalls++
	if f.listFn == nil {
		return nil, nil
	}
	return f.listFn(f.listCalls)
}

func (f *fakeTopicOps) AlterTopicConfig(_ context.Context, _ string, previous, config map[string]string) error {
	f.configCalls++
	f.configPrevious, f.configNew = previous, config
	return f.configErr
}

func (f *fakeTopicOps) Timeout() time.Duration { return f.timeout }

func fakeBrokers(ids ...int) []kafkaclient.BrokerInfo {
	out := make([]kafkaclient.BrokerInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, kafkaclient.BrokerInfo{ID: id})
	}
	return out
}

// fakeTopic builds a topic whose partition i has the given replicas; the first
// replica leads.
func fakeTopic(name string, replicas ...[]int) kafkaclient.TopicInfo {
	t := kafkaclient.TopicInfo{Name: name}
	for i, r := range replicas {
		t.Partitions = append(t.Partitions, kafkaclient.PartitionInfo{ID: i, Leader: r[0], Replicas: r})
	}
	return t
}

func testUpdater(f *fakeTopicOps) *topicUpdater {
	u := newTopicUpdater(f)
	u.reassignmentInterval = time.Millisecond
	u.partitionInterval = time.Millisecond
	u.settleInterval = time.Millisecond
	u.settleWindow = 20 * time.Millisecond
	return u
}

func TestTopicUpdaterReplicationFactor(t *testing.T) {
	t.Run("increase_appends_brokers_and_waits", func(t *testing.T) {
		f := &fakeTopicOps{brokers: fakeBrokers(1, 2, 3), timeout: time.Second}
		before := fakeTopic("t", []int{1}, []int{2})
		after := fakeTopic("t", []int{1, 2}, []int{2, 3})
		f.topics = []kafkaclient.TopicInfo{before}
		f.topicFn = func(call int) (kafkaclient.TopicInfo, error) {
			// Call 1 is the live read before the change. Brokers then still
			// report the old replicas for one more read (metadata lag).
			if call <= 2 {
				return before, nil
			}
			return after, nil
		}
		// The reassignment finishes after several polls.
		f.listFn = func(call int) ([]kafkaclient.PartitionReassignment, error) {
			if call < 3 {
				return []kafkaclient.PartitionReassignment{{Partition: 0}}, nil
			}
			return nil, nil
		}

		require.NoError(t, testUpdater(f).replicationFactor(t.Context(), "t", 2))

		require.Equal(t, 1, f.alterCalls)
		require.Len(t, f.altered, 2)
		for _, a := range f.altered {
			assert.Len(t, a.Replicas, 2, "partition %d", a.Partition)
		}
		assert.Equal(t, 1, f.altered[0].Replicas[0], "existing replica keeps its place")
		assert.Equal(t, 2, f.altered[1].Replicas[0], "existing replica keeps its place")
		assert.Equal(t, 3, f.listCalls, "polled until the reassignment list was empty")
	})

	t.Run("decrease_keeps_leader", func(t *testing.T) {
		f := &fakeTopicOps{brokers: fakeBrokers(1, 2, 3), timeout: time.Second}
		before := fakeTopic("t", []int{1, 2, 3})
		before.Partitions[0].Leader = 3
		f.topics = []kafkaclient.TopicInfo{before}
		f.topicFn = func(call int) (kafkaclient.TopicInfo, error) {
			if call == 1 {
				return before, nil
			}
			return fakeTopic("t", []int{3}), nil
		}

		require.NoError(t, testUpdater(f).replicationFactor(t.Context(), "t", 1))

		require.Len(t, f.altered, 1)
		assert.Equal(t, []int{3}, f.altered[0].Replicas)
		assert.Equal(t, 0, f.topicsCalls, "decreasing needs no information about other topics")
	})

	t.Run("no_change_when_live_count_matches", func(t *testing.T) {
		// State may be stale: the live topic already has the wanted factor.
		f := &fakeTopicOps{brokers: fakeBrokers(1, 2, 3), topics: []kafkaclient.TopicInfo{fakeTopic("t", []int{1, 2})}}

		require.NoError(t, testUpdater(f).replicationFactor(t.Context(), "t", 2))

		assert.Zero(t, f.alterCalls)
	})

	t.Run("more_replicas_than_brokers_is_rejected", func(t *testing.T) {
		f := &fakeTopicOps{brokers: fakeBrokers(1, 2), topics: []kafkaclient.TopicInfo{fakeTopic("t", []int{1})}}

		err := testUpdater(f).replicationFactor(t.Context(), "t", 3)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "replication factor cannot be higher than the number of brokers")
		assert.Zero(t, f.alterCalls)
	})

	t.Run("uneven_replicas_are_rejected", func(t *testing.T) {
		f := &fakeTopicOps{brokers: fakeBrokers(1, 2, 3), topics: []kafkaclient.TopicInfo{fakeTopic("t", []int{1, 2}, []int{2})}}

		err := testUpdater(f).replicationFactor(t.Context(), "t", 3)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "isn't the same across partitions")
		assert.Zero(t, f.alterCalls)
	})

	t.Run("missing_topic_is_an_error", func(t *testing.T) {
		f := &fakeTopicOps{brokers: fakeBrokers(1, 2, 3)}

		err := testUpdater(f).replicationFactor(t.Context(), "t", 2)

		assert.ErrorIs(t, err, kafkaclient.ErrTopicNotFound)
	})

	t.Run("reassignment_error_is_reported", func(t *testing.T) {
		f := &fakeTopicOps{
			brokers:  fakeBrokers(1, 2, 3),
			topics:   []kafkaclient.TopicInfo{fakeTopic("t", []int{1})},
			alterErr: kafkaclient.ErrInvalidReplicaAssignment,
		}

		err := testUpdater(f).replicationFactor(t.Context(), "t", 2)

		require.Error(t, err)
		require.ErrorIs(t, err, kafkaclient.ErrInvalidReplicaAssignment)
		assert.Contains(t, err.Error(), "errors changing replication factor")
		assert.Zero(t, f.listCalls, "no waiting after a rejected reassignment")
	})

	t.Run("error_while_waiting_is_reported", func(t *testing.T) {
		errList := errors.New("controller unavailable")
		f := &fakeTopicOps{
			brokers: fakeBrokers(1, 2, 3),
			topics:  []kafkaclient.TopicInfo{fakeTopic("t", []int{1})},
			timeout: time.Second,
		}
		f.topicFn = func(call int) (kafkaclient.TopicInfo, error) {
			if call == 1 {
				return fakeTopic("t", []int{1}), nil
			}
			return fakeTopic("t", []int{1, 2}), nil
		}
		f.listFn = func(int) ([]kafkaclient.PartitionReassignment, error) { return nil, errList }

		err := testUpdater(f).replicationFactor(t.Context(), "t", 2)

		assert.ErrorIs(t, err, errList)
	})

	t.Run("topic_without_partitions_is_an_error", func(t *testing.T) {
		f := &fakeTopicOps{
			brokers: fakeBrokers(1, 2, 3),
			topics:  []kafkaclient.TopicInfo{{Name: "t"}},
		}

		err := testUpdater(f).replicationFactor(t.Context(), "t", 2)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "no partitions")
		assert.Zero(t, f.alterCalls)
	})

	t.Run("cancelled_context_stops_waiting_with_a_timeout_set", func(t *testing.T) {
		// The default client timeout is positive, so this is the case a real
		// apply meets. A cancelled apply must stop the wait before the timeout.
		f := &fakeTopicOps{
			brokers: fakeBrokers(1, 2, 3),
			topics:  []kafkaclient.TopicInfo{fakeTopic("t", []int{1})},
			timeout: time.Minute,
		}
		ctx, cancel := context.WithCancel(t.Context())
		f.topicFn = func(call int) (kafkaclient.TopicInfo, error) {
			if call == 1 {
				return fakeTopic("t", []int{1}), nil
			}
			return fakeTopic("t", []int{1, 2}), nil
		}
		f.listFn = func(int) ([]kafkaclient.PartitionReassignment, error) {
			cancel()
			return []kafkaclient.PartitionReassignment{{Partition: 0}}, nil
		}

		err := testUpdater(f).replicationFactor(ctx, "t", 2)

		require.ErrorIs(t, err, context.Canceled)
		require.NotErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("cancelled_context_stops_waiting", func(t *testing.T) {
		f := &fakeTopicOps{brokers: fakeBrokers(1, 2, 3), topics: []kafkaclient.TopicInfo{fakeTopic("t", []int{1})}}
		ctx, cancel := context.WithCancel(t.Context())
		f.topicFn = func(call int) (kafkaclient.TopicInfo, error) {
			if call == 1 {
				return fakeTopic("t", []int{1}), nil
			}
			return fakeTopic("t", []int{1, 2}), nil
		}
		f.listFn = func(int) ([]kafkaclient.PartitionReassignment, error) {
			cancel()
			return []kafkaclient.PartitionReassignment{{Partition: 0}}, nil
		}

		err := testUpdater(f).replicationFactor(ctx, "t", 2)

		assert.ErrorIs(t, err, context.Canceled)
	})
}

func TestTopicUpdaterPartitions(t *testing.T) {
	t.Run("grow_uses_live_replication_factor_and_waits", func(t *testing.T) {
		f := &fakeTopicOps{
			brokers: fakeBrokers(1, 2, 3),
			topics:  []kafkaclient.TopicInfo{fakeTopic("t", []int{1, 2}), fakeTopic("other", []int{3})},
			timeout: time.Second,
		}
		f.topicFn = func(call int) (kafkaclient.TopicInfo, error) {
			// Brokers report the old count for two more reads after the write.
			if call <= 3 {
				return fakeTopic("t", []int{1, 2}), nil
			}
			return fakeTopic("t", []int{1, 2}, []int{2, 3}, []int{3, 1}), nil
		}

		require.NoError(t, testUpdater(f).partitions(t.Context(), "t", 3))

		assert.Equal(t, 3, f.createdTotal)
		require.Len(t, f.created, 2, "one replica list per added partition")
		for _, r := range f.created {
			assert.Len(t, r, 2, "new partitions get the topic's replication factor")
			assert.NotEqual(t, r[0], r[1])
		}
		assert.Equal(t, 4, f.topicReads, "waited until the new count was visible")
	})

	t.Run("topic_without_partitions_is_an_error", func(t *testing.T) {
		f := &fakeTopicOps{
			brokers: fakeBrokers(1, 2, 3),
			topics:  []kafkaclient.TopicInfo{{Name: "t"}},
		}

		err := testUpdater(f).partitions(t.Context(), "t", 2)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "no partitions")
		assert.Zero(t, f.createCalls)
	})

	t.Run("reducing_is_rejected", func(t *testing.T) {
		f := &fakeTopicOps{brokers: fakeBrokers(1, 2, 3), topics: []kafkaclient.TopicInfo{fakeTopic("t", []int{1}, []int{2}, []int{3})}}

		err := testUpdater(f).partitions(t.Context(), "t", 2)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "partition count can't be reduced from 3 to 2")
		assert.Zero(t, f.createCalls)
	})

	t.Run("no_change_when_live_count_matches", func(t *testing.T) {
		f := &fakeTopicOps{brokers: fakeBrokers(1, 2, 3), topics: []kafkaclient.TopicInfo{fakeTopic("t", []int{1}, []int{2})}}

		require.NoError(t, testUpdater(f).partitions(t.Context(), "t", 2))

		assert.Zero(t, f.createCalls)
	})

	t.Run("create_error_is_reported", func(t *testing.T) {
		f := &fakeTopicOps{
			brokers:   fakeBrokers(1, 2, 3),
			topics:    []kafkaclient.TopicInfo{fakeTopic("t", []int{1})},
			createErr: kafkaclient.ErrInvalidReplicaAssignment,
		}

		err := testUpdater(f).partitions(t.Context(), "t", 2)

		assert.ErrorIs(t, err, kafkaclient.ErrInvalidReplicaAssignment)
	})

	t.Run("stale_partition_count_is_not_an_error_once_visible", func(t *testing.T) {
		// The first read comes from metadata that predates partitions added a
		// moment ago, so the broker rejects the "growth". The topic already has
		// the wanted count on a fresh read.
		f := &fakeTopicOps{
			brokers:   fakeBrokers(1, 2, 3),
			topics:    []kafkaclient.TopicInfo{fakeTopic("t", []int{1})},
			timeout:   time.Second,
			createErr: kafkaclient.ErrInvalidPartitions,
		}
		f.topicFn = func(call int) (kafkaclient.TopicInfo, error) {
			if call == 1 {
				return fakeTopic("t", []int{1}), nil
			}
			return fakeTopic("t", []int{1}, []int{2}), nil
		}

		require.NoError(t, testUpdater(f).partitions(t.Context(), "t", 2))
	})

	t.Run("invalid_partition_count_stays_an_error_if_not_applied", func(t *testing.T) {
		f := &fakeTopicOps{
			brokers:   fakeBrokers(1, 2, 3),
			topics:    []kafkaclient.TopicInfo{fakeTopic("t", []int{1})},
			createErr: kafkaclient.ErrInvalidPartitions,
		}

		err := testUpdater(f).partitions(t.Context(), "t", 2)

		assert.ErrorIs(t, err, kafkaclient.ErrInvalidPartitions)
	})

	t.Run("times_out_if_partitions_never_appear", func(t *testing.T) {
		f := &fakeTopicOps{
			brokers: fakeBrokers(1, 2, 3),
			topics:  []kafkaclient.TopicInfo{fakeTopic("t", []int{1})},
			timeout: 30 * time.Millisecond,
		}

		err := testUpdater(f).partitions(t.Context(), "t", 2)

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Contains(t, err.Error(), "timed out")
	})
}

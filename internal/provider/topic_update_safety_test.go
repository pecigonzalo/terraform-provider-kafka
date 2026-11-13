package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

func TestTopicUpdaterReplicationFactor_WaitsWhenCountAlreadyMatches(t *testing.T) {
	// A timed-out earlier attempt leaves metadata that already lists the target
	// replicas while data still moves.
	t.Run("waits_for_running_reassignment", func(t *testing.T) {
		f := &fakeTopicOps{
			brokers: fakeBrokers(1, 2, 3),
			topics:  []kafkaclient.TopicInfo{fakeTopic("t", []int{1, 2})},
			timeout: time.Second,
		}
		f.listFn = func(call int) ([]kafkaclient.PartitionReassignment, error) {
			if call < 3 {
				return []kafkaclient.PartitionReassignment{{Partition: 0}}, nil
			}
			return nil, nil
		}

		require.NoError(t, testUpdater(f).replicationFactor(t.Context(), "t", 2))

		assert.Zero(t, f.alterCalls, "no new reassignment is started")
		assert.Equal(t, 3, f.listCalls, "polled until the reassignment list was empty")
	})

	t.Run("times_out_when_reassignment_never_ends", func(t *testing.T) {
		f := &fakeTopicOps{
			brokers: fakeBrokers(1, 2, 3),
			topics:  []kafkaclient.TopicInfo{fakeTopic("t", []int{1, 2})},
			timeout: 30 * time.Millisecond,
		}
		f.listFn = func(int) ([]kafkaclient.PartitionReassignment, error) {
			return []kafkaclient.PartitionReassignment{{Partition: 0}}, nil
		}

		err := testUpdater(f).replicationFactor(t.Context(), "t", 2)

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Contains(t, err.Error(), "timed out")
	})
}

func TestTopicUpdater_RetriesTopicNotFound(t *testing.T) {
	notFoundThenFound := func(misses int, found kafkaclient.TopicInfo) func(int) (kafkaclient.TopicInfo, error) {
		return func(call int) (kafkaclient.TopicInfo, error) {
			if call <= misses {
				return kafkaclient.TopicInfo{}, kafkaclient.ErrTopicNotFound
			}
			return found, nil
		}
	}

	t.Run("replication_factor_recovers_from_lagging_broker", func(t *testing.T) {
		f := &fakeTopicOps{brokers: fakeBrokers(1, 2, 3), timeout: time.Second}
		f.topicFn = notFoundThenFound(3, fakeTopic("t", []int{1, 2}))

		require.NoError(t, testUpdater(f).replicationFactor(t.Context(), "t", 2))
	})

	t.Run("partitions_recover_from_lagging_broker", func(t *testing.T) {
		f := &fakeTopicOps{brokers: fakeBrokers(1, 2, 3), timeout: time.Second}
		f.topicFn = notFoundThenFound(3, fakeTopic("t", []int{1, 2}, []int{2, 3}))

		require.NoError(t, testUpdater(f).partitions(t.Context(), "t", 2))
	})

	t.Run("replication_factor_reports_a_missing_topic_after_the_window", func(t *testing.T) {
		f := &fakeTopicOps{brokers: fakeBrokers(1, 2, 3)}

		err := testUpdater(f).replicationFactor(t.Context(), "t", 2)

		require.ErrorIs(t, err, kafkaclient.ErrTopicNotFound)
		assert.Greater(t, f.topicReads, 1, "the read was retried")
	})

	t.Run("partitions_report_a_missing_topic_after_the_window", func(t *testing.T) {
		f := &fakeTopicOps{brokers: fakeBrokers(1, 2, 3)}

		err := testUpdater(f).partitions(t.Context(), "t", 2)

		require.ErrorIs(t, err, kafkaclient.ErrTopicNotFound)
		assert.Greater(t, f.topicReads, 1, "the read was retried")
	})

	t.Run("other_errors_are_not_retried", func(t *testing.T) {
		errBroker := errors.New("broker unavailable")
		f := &fakeTopicOps{brokers: fakeBrokers(1, 2, 3)}
		f.topicFn = func(int) (kafkaclient.TopicInfo, error) { return kafkaclient.TopicInfo{}, errBroker }

		err := testUpdater(f).partitions(t.Context(), "t", 2)

		require.ErrorIs(t, err, errBroker)
		assert.Equal(t, 1, f.topicReads)
	})
}

func topicModel(partitions, factor int64, config map[string]string) TopicResourceModel {
	elements := map[string]attr.Value{}
	for k, v := range config {
		elements[k] = types.StringValue(v)
	}
	return TopicResourceModel{
		ID:                types.StringValue("t"),
		Name:              types.StringValue("t"),
		Partitions:        types.Int64Value(partitions),
		ReplicationFactor: types.Int64Value(factor),
		Config:            types.MapValueMust(types.StringType, elements),
	}
}

func TestTopicUpdaterUpdate(t *testing.T) {
	t.Run("applies_every_change_and_passes_old_and_new_configuration", func(t *testing.T) {
		f := &fakeTopicOps{brokers: fakeBrokers(1, 2, 3), timeout: time.Second}
		f.topicFn = func(call int) (kafkaclient.TopicInfo, error) {
			switch call {
			case 1: // live read before the factor change
				return fakeTopic("t", []int{1}), nil
			case 2, 3: // wait for the reassignment, then read before adding partitions
				return fakeTopic("t", []int{1, 2}), nil
			default:
				return fakeTopic("t", []int{1, 2}, []int{2, 3}), nil
			}
		}
		state := topicModel(1, 1, map[string]string{"a": "1", "b": "2"})
		plan := topicModel(2, 2, map[string]string{"a": "9"})

		got, err := testUpdater(f).update(t.Context(), plan, state)

		require.NoError(t, err)
		assert.Equal(t, plan, got)
		assert.Equal(t, map[string]string{"a": "1", "b": "2"}, f.configPrevious)
		assert.Equal(t, map[string]string{"a": "9"}, f.configNew)
	})

	t.Run("a_failed_configuration_step_keeps_the_prior_state", func(t *testing.T) {
		errConfig := errors.New("broker refused")
		f := &fakeTopicOps{brokers: fakeBrokers(1, 2, 3), configErr: errConfig}
		state := topicModel(1, 1, map[string]string{"a": "1"})
		plan := topicModel(2, 2, map[string]string{"a": "2"})

		got, err := testUpdater(f).update(t.Context(), plan, state)

		require.ErrorIs(t, err, errConfig)
		assert.Contains(t, err.Error(), "configuration")
		assert.Equal(t, state, got)
		assert.Zero(t, f.alterCalls)
	})

	t.Run("a_failed_replication_factor_step_saves_the_configuration", func(t *testing.T) {
		f := &fakeTopicOps{
			brokers:  fakeBrokers(1, 2, 3),
			topics:   []kafkaclient.TopicInfo{fakeTopic("t", []int{1})},
			alterErr: kafkaclient.ErrInvalidReplicaAssignment,
		}
		state := topicModel(1, 1, map[string]string{"a": "1"})
		plan := topicModel(2, 2, map[string]string{"a": "2"})

		got, err := testUpdater(f).update(t.Context(), plan, state)

		require.ErrorIs(t, err, kafkaclient.ErrInvalidReplicaAssignment)
		assert.Contains(t, err.Error(), "replication factor")
		want := state
		want.Config = plan.Config
		assert.Equal(t, want, got, "only the configuration changed")
		assert.Zero(t, f.createCalls, "partitions are not changed after a failure")
	})

	t.Run("a_failed_partitions_step_saves_configuration_and_replication_factor", func(t *testing.T) {
		f := &fakeTopicOps{
			brokers:   fakeBrokers(1, 2, 3),
			timeout:   time.Second,
			createErr: errors.New("create partitions failed"),
		}
		f.topicFn = func(call int) (kafkaclient.TopicInfo, error) {
			// Call 1 shows the old factor, later calls show the new one.
			if call == 1 {
				return fakeTopic("t", []int{1}), nil
			}
			return fakeTopic("t", []int{1, 2}), nil
		}
		f.topics = []kafkaclient.TopicInfo{fakeTopic("t", []int{1})}
		state := topicModel(1, 1, map[string]string{"a": "1"})
		plan := topicModel(2, 2, map[string]string{"a": "2"})

		got, err := testUpdater(f).update(t.Context(), plan, state)

		require.ErrorIs(t, err, f.createErr)
		assert.Contains(t, err.Error(), "partitions")
		want := plan
		want.Partitions = state.Partitions
		assert.Equal(t, want, got, "partitions keep the old value")
	})

	t.Run("an_unchanged_configuration_is_not_sent", func(t *testing.T) {
		f := &fakeTopicOps{brokers: fakeBrokers(1, 2, 3), topics: []kafkaclient.TopicInfo{fakeTopic("t", []int{1}, []int{2})}}
		state := topicModel(2, 1, map[string]string{"a": "1"})
		plan := topicModel(2, 1, map[string]string{"a": "1"})

		_, err := testUpdater(f).update(t.Context(), plan, state)

		require.NoError(t, err)
		assert.Zero(t, f.configCalls)
	})
}

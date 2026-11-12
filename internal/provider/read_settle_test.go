package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/require"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

func TestConfigUnsettled(t *testing.T) {
	// tracked builds state with every key set to "1".
	tracked := func(keys ...string) types.Map {
		elems := map[string]attr.Value{}
		for _, k := range keys {
			elems[k] = types.StringValue("1")
		}
		return types.MapValueMust(types.StringType, elems)
	}

	tests := []struct {
		name      string
		tracked   types.Map
		reported  map[string]string
		unsettled bool
	}{
		{"null_tracks_nothing", types.MapNull(types.StringType), nil, false},
		{"empty_tracks_nothing", tracked(), map[string]string{"a": "1"}, false},
		{"all_present", tracked("a"), map[string]string{"a": "1", "b": "2"}, false},
		{"missing_key", tracked("a"), map[string]string{"b": "2"}, true},
		{"one_of_two_missing", tracked("a", "b"), map[string]string{"a": "1"}, true},
		{"nothing_reported", tracked("a"), nil, true},
		{"stale_value", tracked("a"), map[string]string{"a": "0"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.unsettled, configUnsettled(tc.tracked, tc.reported))
		})
	}
}

func TestTopicUnsettled(t *testing.T) {
	// topic builds a topic with the given partition count, every partition
	// having the given number of replicas, and the given config.
	topic := func(partitions, replicas int, config map[string]string) kafkaclient.TopicInfo {
		info := kafkaclient.TopicInfo{Config: config}
		for i := range partitions {
			info.Partitions = append(info.Partitions, kafkaclient.PartitionInfo{ID: i, Replicas: make([]int, replicas)})
		}
		return info
	}
	state := func(partitions, factor int64) *TopicResourceModel {
		return &TopicResourceModel{
			Partitions:        types.Int64Value(partitions),
			ReplicationFactor: types.Int64Value(factor),
			Config:            types.MapNull(types.StringType),
		}
	}

	t.Run("matching", func(t *testing.T) {
		require.False(t, topicUnsettled(topic(3, 2, nil), state(3, 2)))
	})
	t.Run("stale_partition_count", func(t *testing.T) {
		require.True(t, topicUnsettled(topic(3, 2, nil), state(5, 2)))
	})
	t.Run("stale_replication_factor", func(t *testing.T) {
		require.True(t, topicUnsettled(topic(3, 1, nil), state(3, 2)))
	})
	t.Run("uneven_replicas_mean_reassignment_in_progress", func(t *testing.T) {
		info := topic(3, 2, nil)
		info.Partitions[0].Replicas = make([]int, 1)
		require.True(t, topicUnsettled(info, state(3, 2)))
	})
	t.Run("null_attributes_are_not_compared", func(t *testing.T) {
		imported := &TopicResourceModel{
			Partitions:        types.Int64Null(),
			ReplicationFactor: types.Int64Null(),
			Config:            types.MapNull(types.StringType),
		}
		require.False(t, topicUnsettled(topic(3, 2, nil), imported))
	})
	t.Run("stale_config", func(t *testing.T) {
		st := state(3, 2)
		st.Config = types.MapValueMust(types.StringType, map[string]attr.Value{"a": types.StringValue("1")})
		require.True(t, topicUnsettled(topic(3, 2, map[string]string{"a": "0"}), st))
		require.False(t, topicUnsettled(topic(3, 2, map[string]string{"a": "1"}), st))
	})
}

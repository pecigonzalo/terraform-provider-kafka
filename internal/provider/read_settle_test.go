package provider

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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

// scriptedTopicReads replays results in order and repeats the last one.
type scriptedTopicReads struct {
	results []error // nil means the topic is found
	cycle   bool    // repeat results from the start instead of the last one
	calls   int
}

func (s *scriptedTopicReads) read(context.Context) (kafkaclient.TopicInfo, error) {
	i := min(s.calls, len(s.results)-1)
	if s.cycle {
		i = s.calls % len(s.results)
	}
	s.calls++
	if s.results[i] != nil {
		return kafkaclient.TopicInfo{}, s.results[i]
	}
	return kafkaclient.TopicInfo{Name: "t", Partitions: []kafkaclient.PartitionInfo{{ID: 0, Replicas: []int{1}}}}, nil
}

func TestSettleTopicRead(t *testing.T) {
	gone := fmt.Errorf("topic %q: %w", "t", kafkaclient.ErrTopicNotFound)
	boom := errors.New("boom")
	state := &TopicResourceModel{
		Partitions:        types.Int64Value(1),
		ReplicationFactor: types.Int64Value(1),
		Config:            types.MapNull(types.StringType),
	}
	const interval, window = time.Millisecond, 2 * time.Second

	tests := []struct {
		name      string
		results   []error
		wantFound bool
		wantErr   error
	}{
		{"found_at_once", []error{nil}, true, nil},
		{"absent_briefly_then_present", []error{gone, gone, nil}, true, nil},
		{"other_error_is_returned", []error{boom}, false, boom},
		{"other_error_after_absence_is_returned", []error{gone, boom}, false, boom},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &scriptedTopicReads{results: tc.results}
			info, err := settleTopicRead(t.Context(), s.read, state, interval, window)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "t", info.Name)
		})
	}

	t.Run("absent_consistently_is_gone", func(t *testing.T) {
		s := &scriptedTopicReads{results: []error{gone}}
		_, err := settleTopicRead(t.Context(), s.read, state, interval, 50*time.Millisecond)
		require.ErrorIs(t, err, kafkaclient.ErrTopicNotFound)
		require.GreaterOrEqual(t, s.calls, minAbsentChecks, "must check several times before reporting a deletion")
	})

	t.Run("visible_then_absent_then_settled", func(t *testing.T) {
		// A stale refresh makes the topic vanish between two sightings: the first
		// shows the old partition count, then it is missing, then it settles.
		partitions := func(n int) kafkaclient.TopicInfo {
			info := kafkaclient.TopicInfo{Name: "t"}
			for i := range n {
				info.Partitions = append(info.Partitions, kafkaclient.PartitionInfo{ID: i, Replicas: []int{1}})
			}
			return info
		}
		replies := []struct {
			info kafkaclient.TopicInfo
			err  error
		}{{partitions(2), nil}, {kafkaclient.TopicInfo{}, gone}, {partitions(1), nil}}
		calls := 0
		read := func(context.Context) (kafkaclient.TopicInfo, error) {
			r := replies[min(calls, len(replies)-1)]
			calls++
			return r.info, r.err
		}
		info, err := settleTopicRead(t.Context(), read, state, interval, window)
		require.NoError(t, err)
		require.Len(t, info.Partitions, 1)
		require.Equal(t, len(replies), calls)
	})

	t.Run("window_ending_early_is_not_a_deletion", func(t *testing.T) {
		// A zero window ends before any retry, as on a cluster slow enough that
		// only one check fits: one not-found answer must not drop the resource.
		s := &scriptedTopicReads{results: []error{gone}}
		_, err := settleTopicRead(t.Context(), s.read, state, interval, 0)
		require.Error(t, err)
		require.NotErrorIs(t, err, kafkaclient.ErrTopicNotFound)
	})

	t.Run("isolated_absences_while_unsettled_are_not_a_deletion", func(t *testing.T) {
		// The topic never settles against state (partitions differ), so Read polls
		// until the window ends. Absences never come five in a row, so it is not gone.
		mismatch := &TopicResourceModel{Partitions: types.Int64Value(9), ReplicationFactor: types.Int64Null(), Config: types.MapNull(types.StringType)}
		s := &scriptedTopicReads{results: []error{nil, nil, gone}, cycle: true}
		info, err := settleTopicRead(t.Context(), s.read, mismatch, interval, 50*time.Millisecond)
		require.NoError(t, err)
		require.Equal(t, "t", info.Name)
	})
}

func TestWaitForVisible(t *testing.T) {
	gone := fmt.Errorf("topic: %w", kafkaclient.ErrTopicNotFound)
	boom := errors.New("boom")

	t.Run("returns_at_first_sighting", func(t *testing.T) {
		results := []error{gone, gone, nil, gone}
		calls := 0
		err := waitForVisible(t.Context(), func(context.Context) error {
			r := results[min(calls, len(results)-1)]
			calls++
			return r
		}, time.Millisecond, 2*time.Second)
		require.NoError(t, err)
		require.Equal(t, 3, calls)
	})
	t.Run("never_visible_is_not_an_error", func(t *testing.T) {
		err := waitForVisible(t.Context(), func(context.Context) error { return gone }, time.Millisecond, 20*time.Millisecond)
		require.NoError(t, err)
	})
	t.Run("other_error_is_returned", func(t *testing.T) {
		err := waitForVisible(t.Context(), func(context.Context) error { return boom }, time.Millisecond, time.Second)
		require.ErrorIs(t, err, boom)
	})
}

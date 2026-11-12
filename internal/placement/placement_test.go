package placement

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func brokersWithRacks(racks ...string) []Broker {
	out := make([]Broker, len(racks))
	for i, r := range racks {
		out[i] = Broker{ID: i + 1, Rack: r}
	}
	return out
}

func replicaLists(as []Assignment) [][]int {
	out := make([][]int, len(as))
	for i, a := range as {
		out[i] = a.Replicas
	}
	return out
}

func TestExtendPartitions(t *testing.T) {
	tests := []struct {
		name    string
		brokers []Broker
		topic   []Partition
		others  []Partition
		add     int
		rf      int
		want    [][]int
	}{
		{
			name:    "no_racks_spreads_over_brokers",
			brokers: brokersWithRacks("", "", ""),
			add:     3, rf: 1,
			want: [][]int{{1}, {2}, {3}},
		},
		{
			name:    "racks_put_replicas_in_different_racks",
			brokers: brokersWithRacks("a", "a", "b", "b"),
			add:     2, rf: 2,
			want: [][]int{{1, 3}, {2, 4}},
		},
		{
			name:    "rf_equals_broker_count",
			brokers: brokersWithRacks("a", "b", "c"),
			add:     2, rf: 3,
			want: [][]int{{1, 2, 3}, {2, 1, 3}},
		},
		{
			name:    "new_partitions_are_numbered_after_existing",
			brokers: brokersWithRacks("", "", ""),
			topic:   []Partition{{ID: 0, Leader: 1, Replicas: []int{1}}, {ID: 1, Leader: 2, Replicas: []int{2}}},
			add:     1, rf: 1,
			want: [][]int{{3}},
		},
		{
			name:    "avoids_brokers_loaded_by_other_topics",
			brokers: brokersWithRacks("", "", ""),
			others:  []Partition{{ID: 0, Leader: 1, Replicas: []int{1}}, {ID: 1, Leader: 1, Replicas: []int{1}}},
			add:     2, rf: 1,
			want: [][]int{{2}, {3}},
		},
		{
			name:    "zero_partitions_added",
			brokers: brokersWithRacks("", ""),
			add:     0, rf: 1,
			want: [][]int{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtendPartitions(tc.brokers, tc.topic, tc.others, tc.add, tc.rf)
			require.NoError(t, err)
			assert.Equal(t, tc.want, replicaLists(got))
		})
	}
}

func TestExtendPartitions_numbersPartitions(t *testing.T) {
	topic := []Partition{{ID: 0, Leader: 1, Replicas: []int{1}}, {ID: 1, Leader: 2, Replicas: []int{2}}}
	got, err := ExtendPartitions(brokersWithRacks("", "", ""), topic, nil, 2, 1)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, 2, got[0].Partition)
	assert.Equal(t, 3, got[1].Partition)
}

func TestPlacementErrors(t *testing.T) {
	brokers := brokersWithRacks("a", "b")
	topic := []Partition{{ID: 0, Leader: 1, Replicas: []int{1}}}

	t.Run("extend_rf_above_broker_count", func(t *testing.T) {
		_, err := ExtendPartitions(brokers, nil, nil, 1, 3)
		require.ErrorIs(t, err, ErrNotEnoughBrokers)
	})
	t.Run("increase_rf_above_broker_count", func(t *testing.T) {
		_, err := IncreaseReplicationFactor(brokers, topic, nil, 3)
		require.ErrorIs(t, err, ErrNotEnoughBrokers)
	})
	t.Run("rf_below_one", func(t *testing.T) {
		_, err := ExtendPartitions(brokers, nil, nil, 1, 0)
		require.Error(t, err)
		_, err = DecreaseReplicationFactor(topic, 0)
		require.Error(t, err)
	})
	t.Run("negative_add", func(t *testing.T) {
		_, err := ExtendPartitions(brokers, nil, nil, -1, 1)
		require.Error(t, err)
	})
	t.Run("duplicate_broker", func(t *testing.T) {
		_, err := ExtendPartitions([]Broker{{ID: 1}, {ID: 1}}, nil, nil, 1, 1)
		require.Error(t, err)
	})
	t.Run("replicas_on_unknown_brokers_leave_no_candidate", func(t *testing.T) {
		p := []Partition{{ID: 0, Leader: 7, Replicas: []int{7, 1}}}
		_, err := IncreaseReplicationFactor(brokersWithRacks("", ""), p, nil, 3)
		require.ErrorIs(t, err, ErrNotEnoughBrokers)
	})
}

func TestIncreaseReplicationFactor(t *testing.T) {
	brokers := brokersWithRacks("a", "a", "b", "b", "c", "c")
	topic := []Partition{
		{ID: 1, Leader: 3, Replicas: []int{3}},
		{ID: 0, Leader: 1, Replicas: []int{1}},
	}
	got, err := IncreaseReplicationFactor(brokers, topic, nil, 3)
	require.NoError(t, err)
	require.Len(t, got, 2)

	// Output is ordered by partition ID and keeps the existing replicas first.
	assert.Equal(t, 0, got[0].Partition)
	assert.Equal(t, 1, got[0].Replicas[0])
	assert.Equal(t, 1, got[1].Partition)
	assert.Equal(t, 3, got[1].Replicas[0])
	for _, a := range got {
		assert.Len(t, a.Replicas, 3)
		assert.Equal(t, 3, rackCount(brokers, a.Replicas), "replicas %v must span three racks", a.Replicas)
	}
}

func TestIncreaseReplicationFactor_keepsLargerAndEqualPartitions(t *testing.T) {
	topic := []Partition{
		{ID: 0, Leader: 1, Replicas: []int{1, 2}},
		{ID: 1, Leader: 2, Replicas: []int{2}},
	}
	got, err := IncreaseReplicationFactor(brokersWithRacks("", "", ""), topic, nil, 2)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2}, got[0].Replicas)
	assert.Len(t, got[1].Replicas, 2)
}

func TestDecreaseReplicationFactor(t *testing.T) {
	tests := []struct {
		name  string
		topic []Partition
		rf    int
		want  [][]int
	}{
		{
			name:  "drops_from_the_end",
			topic: []Partition{{ID: 0, Leader: 1, Replicas: []int{1, 2, 3}}},
			rf:    2,
			want:  [][]int{{1, 2}},
		},
		{
			name:  "keeps_leader_when_it_is_last",
			topic: []Partition{{ID: 0, Leader: 3, Replicas: []int{1, 2, 3}}},
			rf:    2,
			want:  [][]int{{1, 3}},
		},
		{
			name:  "keeps_leader_in_the_middle",
			topic: []Partition{{ID: 0, Leader: 2, Replicas: []int{1, 2, 3}}},
			rf:    1,
			want:  [][]int{{2}},
		},
		{
			name:  "smaller_partitions_unchanged",
			topic: []Partition{{ID: 0, Leader: 1, Replicas: []int{1}}},
			rf:    2,
			want:  [][]int{{1}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecreaseReplicationFactor(tc.topic, tc.rf)
			require.NoError(t, err)
			assert.Equal(t, tc.want, replicaLists(got))
		})
	}
}

func TestDecreaseReplicationFactor_doesNotMutateInput(t *testing.T) {
	topic := []Partition{{ID: 0, Leader: 2, Replicas: []int{1, 2, 3}}}
	_, err := DecreaseReplicationFactor(topic, 1)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, topic[0].Replicas)
}

func rackCount(brokers []Broker, replicas []int) int {
	racks := map[string]bool{}
	for _, id := range replicas {
		for _, b := range brokers {
			if b.ID == id {
				racks[b.Rack] = true
			}
		}
	}
	return len(racks)
}

// scenario is a randomly generated cluster and topic.
type scenario struct {
	brokers []Broker
	racks   int // distinct non-empty racks; 0 when no broker has one
	topic   []Partition
	others  []Partition
	rf      int
}

func genScenario(r *rand.Rand) scenario {
	n := 1 + r.Intn(12)
	racks := 0
	if r.Intn(3) > 0 {
		racks = 1 + r.Intn(n)
	}
	s := scenario{brokers: make([]Broker, n), racks: racks}
	seen := map[string]bool{}
	for i := range s.brokers {
		s.brokers[i] = Broker{ID: 100 + i*3} // non-contiguous IDs
		if racks > 0 {
			s.brokers[i].Rack = fmt.Sprintf("r%d", r.Intn(racks))
			seen[s.brokers[i].Rack] = true
		}
	}
	s.racks = len(seen)
	s.rf = 1 + r.Intn(n)
	for i := 0; i < r.Intn(6); i++ {
		s.others = append(s.others, randomPartition(r, i, s.brokers, 1+r.Intn(n)))
	}
	return s
}

func randomPartition(r *rand.Rand, id int, brokers []Broker, rf int) Partition {
	perm := r.Perm(len(brokers))
	replicas := make([]int, rf)
	for i := range replicas {
		replicas[i] = brokers[perm[i]].ID
	}
	return Partition{ID: id, Leader: replicas[0], Replicas: replicas}
}

func maxSpread(counts map[int]int, brokers []Broker) int {
	lo, hi := int(^uint(0)>>1), 0
	for _, b := range brokers {
		lo, hi = min(lo, counts[b.ID]), max(hi, counts[b.ID])
	}
	return hi - lo
}

func checkAssignment(t *testing.T, s scenario, a Assignment, rf int) {
	t.Helper()
	require.Len(t, a.Replicas, rf, "partition %d replica count", a.Partition)
	ids := map[int]bool{}
	for _, b := range s.brokers {
		ids[b.ID] = true
	}
	seen := map[int]bool{}
	for _, id := range a.Replicas {
		require.True(t, ids[id], "partition %d uses unknown broker %d", a.Partition, id)
		require.False(t, seen[id], "partition %d repeats broker %d", a.Partition, id)
		seen[id] = true
	}
	if s.racks > 0 {
		// Brokers without a rack never exist in this generator once racks > 0.
		want := min(rf, s.racks)
		assert.Equal(t, want, rackCount(s.brokers, a.Replicas), "partition %d rack spread %v", a.Partition, a.Replicas)
	}
}

func TestExtendPartitions_properties(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	for i := range 500 {
		s := genScenario(r)
		add := 1 + r.Intn(3*len(s.brokers))

		got, err := ExtendPartitions(s.brokers, nil, nil, add, s.rf)
		require.NoError(t, err)
		require.Len(t, got, add)
		replicas, leaders := map[int]int{}, map[int]int{}
		for _, a := range got {
			checkAssignment(t, s, a, s.rf)
			leaders[a.Replicas[0]]++
			for _, id := range a.Replicas {
				replicas[id]++
			}
		}
		// On an empty cluster without racks the result is exactly balanced.
		if s.racks == 0 {
			assert.LessOrEqual(t, maxSpread(replicas, s.brokers), 1, "scenario %d replicas %v", i, replicas)
			assert.LessOrEqual(t, maxSpread(leaders, s.brokers), 2, "scenario %d leaders %v", i, leaders)
		}

		again, err := ExtendPartitions(s.brokers, nil, nil, add, s.rf)
		require.NoError(t, err)
		assert.Equal(t, got, again, "placement must be deterministic")
	}
}

func TestExtendPartitions_doesNotMutateInputs(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	s := genScenario(r)
	s.topic = []Partition{randomPartition(r, 0, s.brokers, 1)}
	brokers := slices.Clone(s.brokers)
	topic := []Partition{{ID: 0, Leader: s.topic[0].Leader, Replicas: slices.Clone(s.topic[0].Replicas)}}
	others := make([]Partition, len(s.others))
	for i, p := range s.others {
		others[i] = Partition{ID: p.ID, Leader: p.Leader, Replicas: slices.Clone(p.Replicas)}
	}

	_, err := ExtendPartitions(s.brokers, s.topic, s.others, 4, 1)
	require.NoError(t, err)
	assert.Equal(t, brokers, s.brokers)
	assert.Equal(t, topic, s.topic)
	assert.Equal(t, others, s.others)
}

func TestIncreaseReplicationFactor_properties(t *testing.T) {
	r := rand.New(rand.NewSource(99))
	for range 500 {
		s := genScenario(r)
		from := 1 + r.Intn(s.rf)
		for p := 0; p < 1+r.Intn(2*len(s.brokers)); p++ {
			s.topic = append(s.topic, randomPartition(r, p, s.brokers, from))
		}
		before := make([]Partition, len(s.topic))
		for j, p := range s.topic {
			before[j] = Partition{ID: p.ID, Leader: p.Leader, Replicas: slices.Clone(p.Replicas)}
		}

		got, err := IncreaseReplicationFactor(s.brokers, s.topic, s.others, s.rf)
		require.NoError(t, err)
		require.Len(t, got, len(s.topic))
		assert.Equal(t, before, s.topic, "input must not be mutated")
		for j, a := range got {
			require.Equal(t, before[j].ID, a.Partition)
			require.Len(t, a.Replicas, s.rf)
			assert.Equal(t, before[j].Replicas, a.Replicas[:from], "existing replicas keep their order")
			seen := map[int]bool{}
			for _, id := range a.Replicas {
				require.False(t, seen[id], "partition %d repeats broker %d", a.Partition, id)
				seen[id] = true
			}
			// A rack spread is only promised when the starting replicas already had one.
			if s.racks > 0 && rackCount(s.brokers, before[j].Replicas) == from {
				assert.Equal(t, min(s.rf, s.racks), rackCount(s.brokers, a.Replicas), "partition %d: %v", a.Partition, a.Replicas)
			}
		}
	}
}

func TestDecreaseReplicationFactor_properties(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	for range 500 {
		s := genScenario(r)
		var topic []Partition
		for p := 0; p < 1+r.Intn(10); p++ {
			part := randomPartition(r, p, s.brokers, s.rf)
			part.Leader = part.Replicas[r.Intn(len(part.Replicas))]
			topic = append(topic, part)
		}
		rf := 1 + r.Intn(s.rf)
		before := make([]Partition, len(topic))
		for j, p := range topic {
			before[j] = Partition{ID: p.ID, Leader: p.Leader, Replicas: slices.Clone(p.Replicas)}
		}

		got, err := DecreaseReplicationFactor(topic, rf)
		require.NoError(t, err)
		assert.Equal(t, before, topic, "input must not be mutated")
		for j, a := range got {
			require.Len(t, a.Replicas, rf)
			assert.Contains(t, a.Replicas, before[j].Leader, "leader must be kept")
			for _, id := range a.Replicas {
				assert.Contains(t, before[j].Replicas, id, "no broker may be introduced")
			}
		}
	}
}

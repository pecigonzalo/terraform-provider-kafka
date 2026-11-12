// Package placement computes Kafka replica assignments. It is pure: it does no
// I/O, holds no state between calls and uses no randomness, so the same inputs
// always produce the same assignments.
//
// The functions spread the replicas of a partition across racks when the
// brokers report racks and enough racks exist, and otherwise across brokers.
// Across partitions they keep the number of replicas and leaders per broker
// balanced, counting the load that other topics already put on each broker.
//
// Brokers without a rack are treated as each being in a rack of their own, so
// a cluster with no rack information is simply balanced across brokers.
package placement

import (
	"errors"
	"fmt"
	"slices"
)

// ErrNotEnoughBrokers is returned when the requested replication factor is
// higher than the number of brokers available. Match it with [errors.Is].
var ErrNotEnoughBrokers = errors.New("replication factor is higher than the number of brokers")

// Broker is a broker that may host replicas.
type Broker struct {
	ID int
	// Rack is empty when the broker has no rack configured.
	Rack string
}

// Partition is the current placement of one partition. Replicas is the
// replica list as the cluster reports it; its first element is the preferred
// leader. Leader is the broker currently leading the partition.
type Partition struct {
	ID       int
	Leader   int
	Replicas []int
}

// Assignment is the desired replica list for one partition. The first
// replica is the preferred leader.
type Assignment struct {
	Partition int
	Replicas  []int
}

// ExtendPartitions returns assignments for add new partitions of a topic,
// each with replicationFactor replicas. The new partitions are numbered after
// the highest partition in topic. Existing partitions are not touched.
//
// topic is the topic's current placement and others is the placement of every
// other topic in the cluster; both count towards broker load so that new
// replicas and leaders land on the least loaded brokers. add == 0 returns no
// assignments. It returns an error wrapping [ErrNotEnoughBrokers] when
// replicationFactor exceeds the number of brokers.
func ExtendPartitions(brokers []Broker, topic, others []Partition, add, replicationFactor int) ([]Assignment, error) {
	if add < 0 {
		return nil, fmt.Errorf("number of partitions to add must not be negative, got %d", add)
	}
	c, err := newCluster(brokers, replicationFactor)
	if err != nil {
		return nil, err
	}
	c.addLoad(others)
	c.addLoad(topic)

	next := 0
	for _, p := range topic {
		next = max(next, p.ID+1)
	}
	out := make([]Assignment, 0, add)
	for i := range add {
		replicas, err := c.fill(nil, replicationFactor)
		if err != nil {
			return nil, err
		}
		// Lead with the replica that leads the fewest partitions so far.
		leader := slices.MinFunc(replicas, c.compareLeaders)
		replicas = slices.Insert(slices.DeleteFunc(replicas, func(id int) bool { return id == leader }), 0, leader)
		c.leaders[leader]++
		out = append(out, Assignment{Partition: next + i, Replicas: replicas})
	}
	return out, nil
}

// IncreaseReplicationFactor returns assignments that grow every partition of
// topic to replicationFactor replicas by appending brokers to its replica
// list. Existing replicas keep their order, so the preferred leader and all
// existing data stay where they are. Partitions that already have
// replicationFactor or more replicas are returned unchanged.
//
// Added brokers prefer racks the partition does not use yet, then the least
// loaded brokers; others is the placement of every other topic. It returns an
// error wrapping [ErrNotEnoughBrokers] when replicationFactor exceeds the
// number of brokers.
func IncreaseReplicationFactor(brokers []Broker, topic, others []Partition, replicationFactor int) ([]Assignment, error) {
	c, err := newCluster(brokers, replicationFactor)
	if err != nil {
		return nil, err
	}
	c.addLoad(others)
	c.addLoad(topic)

	out := make([]Assignment, 0, len(topic))
	for _, p := range sortedByID(topic) {
		replicas, err := c.fill(slices.Clone(p.Replicas), replicationFactor)
		if err != nil {
			return nil, fmt.Errorf("partition %d: %w", p.ID, err)
		}
		out = append(out, Assignment{Partition: p.ID, Replicas: replicas})
	}
	return out, nil
}

// DecreaseReplicationFactor returns assignments that shrink every partition
// of topic to replicationFactor replicas. The leader is always kept and the
// other replicas are dropped from the end of the list. Partitions that
// already have replicationFactor or fewer replicas are returned unchanged.
func DecreaseReplicationFactor(topic []Partition, replicationFactor int) ([]Assignment, error) {
	if replicationFactor < 1 {
		return nil, fmt.Errorf("replication factor must be at least 1, got %d", replicationFactor)
	}
	out := make([]Assignment, 0, len(topic))
	for _, p := range sortedByID(topic) {
		replicas := slices.Clone(p.Replicas)
		for i := len(replicas) - 1; i >= 0 && len(replicas) > replicationFactor; i-- {
			if replicas[i] == p.Leader {
				continue
			}
			replicas = slices.Delete(replicas, i, i+1)
		}
		out = append(out, Assignment{Partition: p.ID, Replicas: replicas})
	}
	return out, nil
}

// cluster tracks broker load while assignments are being built.
type cluster struct {
	brokers  []Broker // sorted by ID
	rackOf   map[int]string
	replicas map[int]int // replicas hosted per broker
	leaders  map[int]int // partitions led per broker
}

func newCluster(brokers []Broker, replicationFactor int) (*cluster, error) {
	if replicationFactor < 1 {
		return nil, fmt.Errorf("replication factor must be at least 1, got %d", replicationFactor)
	}
	if replicationFactor > len(brokers) {
		return nil, fmt.Errorf("replication factor %d with %d brokers: %w", replicationFactor, len(brokers), ErrNotEnoughBrokers)
	}
	c := &cluster{
		brokers:  slices.Clone(brokers),
		rackOf:   make(map[int]string, len(brokers)),
		replicas: make(map[int]int, len(brokers)),
		leaders:  make(map[int]int, len(brokers)),
	}
	slices.SortFunc(c.brokers, func(a, b Broker) int { return a.ID - b.ID })
	for _, b := range c.brokers {
		if _, dup := c.rackOf[b.ID]; dup {
			return nil, fmt.Errorf("broker %d is listed more than once", b.ID)
		}
		c.rackOf[b.ID] = b.Rack
	}
	return c, nil
}

func (c *cluster) addLoad(partitions []Partition) {
	for _, p := range partitions {
		for _, id := range p.Replicas {
			c.replicas[id]++
		}
		c.leaders[p.Leader]++
	}
}

// rackKey identifies the failure domain of a broker. Brokers without a rack,
// and replicas on brokers that are not in the candidate list, each count as a
// domain of their own.
func (c *cluster) rackKey(id int) string {
	if rack := c.rackOf[id]; rack != "" {
		return "rack:" + rack
	}
	return fmt.Sprintf("broker:%d", id)
}

// fill appends brokers to replicas until it has replicationFactor entries,
// counting each added replica towards broker load.
func (c *cluster) fill(replicas []int, replicationFactor int) ([]int, error) {
	for len(replicas) < replicationFactor {
		id, err := c.pick(replicas)
		if err != nil {
			return nil, err
		}
		replicas = append(replicas, id)
		c.replicas[id]++
	}
	return replicas, nil
}

// pick returns the broker that should host the next replica of a partition
// that already has the given replicas. It prefers the rack holding the fewest
// of those replicas, then the broker hosting the fewest replicas overall, then
// the lowest ID.
func (c *cluster) pick(have []int) (int, error) {
	rackUse := make(map[string]int, len(have))
	for _, id := range have {
		rackUse[c.rackKey(id)]++
	}
	best, found := Broker{}, false
	for _, b := range c.brokers {
		if slices.Contains(have, b.ID) {
			continue
		}
		if !found || c.better(b.ID, best.ID, rackUse) {
			best, found = b, true
		}
	}
	if !found {
		// Reachable when replicas sit on brokers outside the candidate list.
		return 0, fmt.Errorf("no broker left to host another replica: %w", ErrNotEnoughBrokers)
	}
	return best.ID, nil
}

func (c *cluster) better(a, b int, rackUse map[string]int) bool {
	if ra, rb := rackUse[c.rackKey(a)], rackUse[c.rackKey(b)]; ra != rb {
		return ra < rb
	}
	return c.replicas[a] < c.replicas[b]
}

func (c *cluster) compareLeaders(a, b int) int {
	if d := c.leaders[a] - c.leaders[b]; d != 0 {
		return d
	}
	if d := c.replicas[a] - c.replicas[b]; d != 0 {
		return d
	}
	return a - b
}

func sortedByID(ps []Partition) []Partition {
	out := slices.Clone(ps)
	slices.SortStableFunc(out, func(a, b Partition) int { return a.ID - b.ID })
	return out
}

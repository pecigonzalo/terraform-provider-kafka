package provider

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
	"github.com/stretchr/testify/require"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

// rackSettleTimeout bounds how long a check waits for the brokers to report a
// change. Brokers learn of changes asynchronously and the client caches
// metadata, so a read straight after an apply can still show the old state.
const rackSettleTimeout = 60 * time.Second

// topicLayout is a topic's placement together with the racks of the brokers.
type topicLayout struct {
	partitions []kafkaclient.PartitionInfo
	rackOf     map[int]string
}

// racks returns the distinct racks hosting the given replicas.
func (l topicLayout) racks(replicas []int) []string {
	var out []string
	for _, id := range replicas {
		if rack := l.rackOf[id]; !slices.Contains(out, rack) {
			out = append(out, rack)
		}
	}
	return out
}

// newRackTestClient connects to the acceptance cluster.
func newRackTestClient(t *testing.T) *kafkaclient.Client {
	t.Helper()

	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance tests need TF_ACC")
	}
	address, _, _ := strings.Cut(os.Getenv("KAFKA_BOOTSTRAP_SERVERS"), ",")
	client, err := kafkaclient.New(t.Context(), kafkaclient.Config{BootstrapServer: address, Timeout: 10 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// readLayout reads the live placement of a topic and the racks of all brokers.
func readLayout(ctx context.Context, client *kafkaclient.Client, topic string) (topicLayout, error) {
	brokers, err := client.Brokers(ctx)
	if err != nil {
		return topicLayout{}, err
	}
	info, err := client.Topic(ctx, topic)
	if err != nil {
		return topicLayout{}, err
	}
	layout := topicLayout{partitions: info.Partitions, rackOf: make(map[int]string, len(brokers))}
	for _, b := range brokers {
		layout.rackOf[b.ID] = b.Rack
	}
	return layout, nil
}

// checkLayout returns a check that polls the topic until verify accepts its
// placement, or fails with the last verification error after
// rackSettleTimeout. Verification errors are expected while a broker still
// serves old metadata, so they are retried rather than reported at once.
func checkLayout(t *testing.T, client *kafkaclient.Client, topic string, verify func(topicLayout) error) resource.TestCheckFunc {
	t.Helper()

	return func(*terraform.State) error {
		ctx, cancel := context.WithTimeout(t.Context(), rackSettleTimeout)
		defer cancel()

		var last error
		for {
			layout, err := readLayout(ctx, client, topic)
			if err == nil {
				err = verify(layout)
			}
			if err == nil {
				return nil
			}
			last = err
			select {
			case <-ctx.Done():
				return fmt.Errorf("topic %q placement not as expected after %s: %w", topic, rackSettleTimeout, last)
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
}

// snapshotLayout returns a check that stores the settled placement of a topic
// in dst once every partition has want replicas and a leader.
func snapshotLayout(t *testing.T, client *kafkaclient.Client, topic string, want int, dst *topicLayout) resource.TestCheckFunc {
	t.Helper()

	return checkLayout(t, client, topic, func(l topicLayout) error {
		if err := verifyShape(l, want); err != nil {
			return err
		}
		*dst = l
		return nil
	})
}

// verifyShape checks every partition has want distinct replicas, a leader among
// them and no reassignment-induced duplicates.
func verifyShape(l topicLayout, want int) error {
	for _, p := range l.partitions {
		if len(p.Replicas) != want {
			return fmt.Errorf("partition %d has replicas %v, want %d replicas", p.ID, p.Replicas, want)
		}
		if len(slices.Compact(slices.Sorted(slices.Values(p.Replicas)))) != len(p.Replicas) {
			return fmt.Errorf("partition %d has a broker more than once: %v", p.ID, p.Replicas)
		}
		if !slices.Contains(p.Replicas, p.Leader) {
			return fmt.Errorf("partition %d leader %d is not among replicas %v", p.ID, p.Leader, p.Replicas)
		}
	}
	return nil
}

// verifyRackSpread checks every partition has replicas on want racks.
func verifyRackSpread(l topicLayout, want int) error {
	for _, p := range l.partitions {
		if got := l.racks(p.Replicas); len(got) != want {
			return fmt.Errorf("partition %d replicas %v are on racks %v, want %d racks", p.ID, p.Replicas, got, want)
		}
	}
	return nil
}

func rackTestSteps(t *testing.T, steps []resource.TestStep) {
	t.Helper()

	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_8_0)},
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps:                    steps,
	})
}

// TestAccRackClusterBrokers confirms the acceptance brokers report the racks
// the rack tests rely on: brokers 0 and 1 in one rack and broker 2 in another.
func TestAccRackClusterBrokers(t *testing.T) {
	client := newRackTestClient(t)

	require.Eventually(t, func() bool {
		brokers, err := client.Brokers(t.Context())
		if err != nil || len(brokers) != clusterBrokers {
			return false
		}
		for _, b := range brokers {
			if b.Rack != brokerRack(b.ID) {
				return false
			}
		}
		return true
	}, rackSettleTimeout, 500*time.Millisecond, "brokers do not report the configured racks")
}

// TestAccTopicResourceRackReplicationFactorIncrease raises the replication
// factor from 1 to 2 and checks, on the real cluster, that every partition ends
// up on two racks and that the replica it already had did not move.
func TestAccTopicResourceRackReplicationFactorIncrease(t *testing.T) {
	const name = "rack-rf-increase"
	client := newRackTestClient(t)
	var before topicLayout

	rackTestSteps(t, []resource.TestStep{
		{
			Config: testAccTopicResourceConfig(name, 6, 1),
			Check:  snapshotLayout(t, client, name, 1, &before),
		},
		{
			Config: testAccTopicResourceConfig(name, 6, 2),
			Check: checkLayout(t, client, name, func(l topicLayout) error {
				if err := verifyShape(l, 2); err != nil {
					return err
				}
				if err := verifyRackSpread(l, 2); err != nil {
					return err
				}
				for _, p := range l.partitions {
					old := before.partitions[p.ID]
					if p.Replicas[0] != old.Replicas[0] {
						return fmt.Errorf("partition %d moved its existing replica from %v to %v", p.ID, old.Replicas, p.Replicas)
					}
				}
				return nil
			}),
		},
	})
}

// TestAccTopicResourceRackPartitionGrowth adds partitions to a replicated topic
// and checks the new partitions get the live replication factor, span both
// racks, and use no broker twice, while the existing partitions stay as they
// were.
func TestAccTopicResourceRackPartitionGrowth(t *testing.T) {
	const name = "rack-partition-growth"
	client := newRackTestClient(t)
	var before topicLayout

	rackTestSteps(t, []resource.TestStep{
		{
			Config: testAccTopicResourceConfig(name, 2, 2),
			Check:  snapshotLayout(t, client, name, 2, &before),
		},
		{
			Config: testAccTopicResourceConfig(name, 8, 2),
			Check: checkLayout(t, client, name, func(l topicLayout) error {
				if len(l.partitions) != 8 {
					return fmt.Errorf("topic has %d partitions, want 8", len(l.partitions))
				}
				if err := verifyShape(l, 2); err != nil {
					return err
				}
				if err := verifyRackSpread(l, 2); err != nil {
					return err
				}
				for _, old := range before.partitions {
					if got := l.partitions[old.ID].Replicas; !slices.Equal(got, old.Replicas) {
						return fmt.Errorf("existing partition %d changed replicas from %v to %v", old.ID, old.Replicas, got)
					}
				}
				return nil
			}),
		},
	})
}

// TestAccTopicResourceRackFullReplication creates a topic with replication
// factor 3 on a cluster with two racks and checks every partition uses all
// three brokers and so both racks. It then grows the partitions and checks the
// new ones do the same.
func TestAccTopicResourceRackFullReplication(t *testing.T) {
	const name = "rack-full-replication"
	client := newRackTestClient(t)

	verify := func(partitions int) resource.TestCheckFunc {
		return checkLayout(t, client, name, func(l topicLayout) error {
			if len(l.partitions) != partitions {
				return fmt.Errorf("topic has %d partitions, want %d", len(l.partitions), partitions)
			}
			if err := verifyShape(l, 3); err != nil {
				return err
			}
			if err := verifyRackSpread(l, 2); err != nil {
				return err
			}
			for _, p := range l.partitions {
				if got := slices.Sorted(slices.Values(p.Replicas)); !slices.Equal(got, []int{0, 1, 2}) {
					return fmt.Errorf("partition %d replicas %v do not use all three brokers", p.ID, p.Replicas)
				}
			}
			return nil
		})
	}

	rackTestSteps(t, []resource.TestStep{
		{Config: testAccTopicResourceConfig(name, 3, 3), Check: verify(3)},
		{Config: testAccTopicResourceConfig(name, 6, 3), Check: verify(6)},
	})
}

// TestAccTopicResourceRackReplicationFactorDecrease lowers the replication
// factor and checks every partition keeps its leader and only loses replicas it
// already had.
func TestAccTopicResourceRackReplicationFactorDecrease(t *testing.T) {
	const name = "rack-rf-decrease"
	client := newRackTestClient(t)
	var before topicLayout

	rackTestSteps(t, []resource.TestStep{
		{
			Config: testAccTopicResourceConfig(name, 6, 3),
			Check:  snapshotLayout(t, client, name, 3, &before),
		},
		{
			Config: testAccTopicResourceConfig(name, 6, 2),
			Check: checkLayout(t, client, name, func(l topicLayout) error {
				if err := verifyShape(l, 2); err != nil {
					return err
				}
				for _, p := range l.partitions {
					old := before.partitions[p.ID]
					if p.Leader != old.Leader {
						return fmt.Errorf("partition %d leader changed from %d to %d", p.ID, old.Leader, p.Leader)
					}
					for _, id := range p.Replicas {
						if !slices.Contains(old.Replicas, id) {
							return fmt.Errorf("partition %d gained replica %d: %v -> %v", p.ID, id, old.Replicas, p.Replicas)
						}
					}
				}
				return nil
			}),
		},
		{
			Config: testAccTopicResourceConfig(name, 6, 1),
			Check: checkLayout(t, client, name, func(l topicLayout) error {
				if err := verifyShape(l, 1); err != nil {
					return err
				}
				for _, p := range l.partitions {
					if p.Leader != before.partitions[p.ID].Leader {
						return fmt.Errorf("partition %d leader changed from %d to %d", p.ID, before.partitions[p.ID].Leader, p.Leader)
					}
				}
				return nil
			}),
		},
	})
}

// TestAccTopicResourceRackReplicationFactorTooHigh checks a replication factor
// above the broker count is rejected on create and on update, with the
// existing error, and leaves the topic unchanged.
func TestAccTopicResourceRackReplicationFactorTooHigh(t *testing.T) {
	const name = "rack-rf-too-high"
	tooHigh := regexp.MustCompile(`(?is)replication factor cannot be higher than the number of brokers`)
	client := newRackTestClient(t)

	rackTestSteps(t, []resource.TestStep{
		{
			Config:      testAccTopicResourceConfig(name, 1, clusterBrokers+1),
			ExpectError: tooHigh,
		},
		{
			Config: testAccTopicResourceConfig(name, 2, 1),
			Check:  checkLayout(t, client, name, func(l topicLayout) error { return verifyShape(l, 1) }),
		},
		{
			Config:      testAccTopicResourceConfig(name, 2, clusterBrokers+1),
			ExpectError: tooHigh,
		},
		{
			Config: testAccTopicResourceConfig(name, 2, 1),
			Check:  checkLayout(t, client, name, func(l topicLayout) error { return verifyShape(l, 1) }),
		},
	})
}

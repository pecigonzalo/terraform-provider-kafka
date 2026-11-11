package provider

import (
	"fmt"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
	kafka "github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccTopicResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Create and Read testing
			{
				Config: testAccTopicResourceConfig(
					"one",
					1,
					1,
				),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("kafka_topic.test", "name", "one"),
					resource.TestCheckResourceAttr("kafka_topic.test", "id", "one"),
				),
			},
			// ImportState testing
			{
				ResourceName:      "kafka_topic.test",
				ImportState:       true,
				ImportStateVerify: true,
				// This is not normally necessary, but is here because this
				// example code does not have an actual upstream service.
				// Once the Read method is able to refresh information from
				// the upstream service, this can be removed.
				ImportStateVerifyIgnore: []string{"configuration"},
			},
			// Update and Read testing
			{
				Config: testAccTopicResourceConfig(
					"two",
					1,
					1,
				),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("kafka_topic.test", "id", "two"),
				),
			},
			// Delete testing automatically occurs in TestCase
		},
	})
}

func TestAccTopicResourceGenerateConfig(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks: []tfversion.TerraformVersionCheck{
			tfversion.SkipBelow(tfversion.Version1_14_0),
		},
		Steps: []resource.TestStep{
			{
				Config: testAccTopicResourceConfig("generated", 1, 1),
			},
			{
				ResourceName:    "kafka_topic.test",
				ImportState:     true,
				ImportStateKind: resource.ImportBlockWithID,
				GenerateConfig:  true,
			},
		},
	})
}

func testAccTopicResourceConfig(name string, partitions int, replication_factor int) string {
	return fmt.Sprintf(providerConfig+`
resource "kafka_topic" "test" {
  name = %[1]q
  partitions = %v
  replication_factor = %v
}
`, name, partitions, replication_factor)
}

func TestIncreaseReplicas(t *testing.T) {
	tests := []struct {
		name     string
		desired  int
		replicas []int
		brokers  []int
		want     []int
	}{
		{"adds_missing_broker", 3, []int{2, 1}, []int{1, 2, 3}, []int{2, 1, 3}},
		{"no_change_when_at_desired", 2, []int{2, 1}, []int{1, 2, 3}, []int{2, 1}},
		{"limited_by_available_brokers", 5, []int{1}, []int{1, 2}, []int{1, 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := slices.Clone(tc.replicas)
			got := increaseReplicas(tc.desired, input, tc.brokers)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.replicas, input, "input must not be modified")
		})
	}
}

func TestReduceReplicas(t *testing.T) {
	tests := []struct {
		name     string
		desired  int
		replicas []int
		leader   int
		want     []int
	}{
		{"keeps_leader_first", 1, []int{2, 1, 3}, 2, []int{2}},
		{"keeps_leader_last", 1, []int{1, 3, 2}, 2, []int{2}},
		{"drops_from_end", 2, []int{2, 1, 3}, 2, []int{2, 1}},
		{"larger_set", 2, []int{2, 1, 3, 5, 4}, 2, []int{2, 1}},
		{"leader_in_middle", 2, []int{1, 2, 3, 4}, 2, []int{1, 2}},
		{"unknown_leader", 2, []int{1, 2, 3}, -1, []int{1, 2}},
		{"no_change_when_at_desired", 3, []int{1, 2, 3}, 1, []int{1, 2, 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := slices.Clone(tc.replicas)
			got := reduceReplicas(tc.desired, input, tc.leader)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.replicas, input, "input must not be modified")
		})
	}
}

func TestConfigEntries(t *testing.T) {
	m := types.MapValueMust(types.StringType, map[string]attr.Value{
		"retention.ms": types.StringValue(`say "hi"`),
		"null.value":   types.StringNull(),
	})
	entries := configEntries(m)
	assert.Equal(t, []kafka.ConfigEntry{{ConfigName: "retention.ms", ConfigValue: `say "hi"`}}, entries)
	assert.Empty(t, configEntries(types.MapNull(types.StringType)))
}

package provider

import (
	"fmt"
	"regexp"
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

func TestAccTopicResourcePartitions(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccTopicResourceConfig("partitions-topic", 1, 1),
				Check:  resource.TestCheckResourceAttr("kafka_topic.test", "partitions", "1"),
			},
			{
				Config: testAccTopicResourceConfig("partitions-topic", 3, 1),
				Check:  resource.TestCheckResourceAttr("kafka_topic.test", "partitions", "3"),
			},
			// Shrinking is not supported by Kafka and must fail at plan time
			{
				Config:      testAccTopicResourceConfig("partitions-topic", 2, 1),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`partitions\s+can't\s+be\s+reduced`),
			},
		},
	})
}

func TestAccTopicResourceReplicationFactorExceedsBrokers(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// The test cluster has a single broker, so replication_factor = 2 must fail at plan time
			{
				Config:      testAccTopicResourceConfig("replication-topic", 1, 2),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?i)replication\s+factor\s+cannot\s+be\s+higher`),
			},
		},
	})
}

func TestAccTopicResourceConfiguration(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Set a key; the harness fails if a follow-up plan is non-empty
			{
				Config: testAccTopicResourceConfigWithRetention("config-topic", `"60000"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("kafka_topic.test", "configuration.%", "1"),
					resource.TestCheckResourceAttr("kafka_topic.test", "configuration.retention.ms", "60000"),
				),
			},
			// Change the value
			{
				Config: testAccTopicResourceConfigWithRetention("config-topic", `"120000"`),
				Check:  resource.TestCheckResourceAttr("kafka_topic.test", "configuration.retention.ms", "120000"),
			},
			// Import tracks no configuration unless it is configured
			{
				ResourceName:            "kafka_topic.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"configuration"},
			},
			// Remove the block entirely
			{
				Config: testAccTopicResourceConfig("config-topic", 1, 1),
				Check:  resource.TestCheckNoResourceAttr("kafka_topic.test", "configuration.retention.ms"),
			},
		},
	})
}

func testAccTopicResourceConfigWithRetention(name string, retention string) string {
	return fmt.Sprintf(providerConfig+`
resource "kafka_topic" "test" {
  name               = %[1]q
  partitions         = 1
  replication_factor = 1
  configuration = {
    "retention.ms" = %[2]s
  }
}
`, name, retention)
}

func TestAccTopicResourceInvalid(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      testAccTopicResourceConfig("invalid", 0, 1),
				ExpectError: regexp.MustCompile(`(?s)partitions.*at least 1`),
			},
			{
				Config:      testAccTopicResourceConfig("invalid", 1, 0),
				ExpectError: regexp.MustCompile(`(?s)replication_factor.*at least 1`),
			},
		},
	})
}

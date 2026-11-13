package provider

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/require"
)

const (
	// Kafka's default for max.message.bytes and retention.ms.
	defaultMaxMessageBytes = "1048588"
	defaultRetentionMs     = "604800000"
)

// firstBroker returns the address of the first broker in KAFKA_BOOTSTRAP_SERVERS.
func firstBroker() string {
	address, _, _ := strings.Cut(os.Getenv("KAFKA_BOOTSTRAP_SERVERS"), ",")
	return address
}

// setTopicConfigOutsideTerraform sets one topic setting with a raw client, as
// another tool would, and waits until every broker reports it.
func setTopicConfigOutsideTerraform(t *testing.T, topic, key, value string) {
	t.Helper()

	client := &kafka.Client{Addr: kafka.TCP(firstBroker()), Timeout: 10 * time.Second}
	resp, err := client.IncrementalAlterConfigs(t.Context(), &kafka.IncrementalAlterConfigsRequest{
		Resources: []kafka.IncrementalAlterConfigsRequestResource{{
			ResourceType: kafka.ResourceTypeTopic,
			ResourceName: topic,
			Configs: []kafka.IncrementalAlterConfigsRequestConfig{{
				Name: key, Value: value, ConfigOperation: kafka.ConfigOperationSet,
			}},
		}},
	})
	require.NoError(t, err)
	for _, res := range resp.Resources {
		require.NoError(t, res.Error)
	}
	waitForTopicConfig(t, topic, key, value)
}

// deleteTopicOutsideTerraform deletes a topic with a raw client and waits until
// every broker reports it as gone.
func deleteTopicOutsideTerraform(t *testing.T, topic string) {
	t.Helper()

	client := &kafka.Client{Addr: kafka.TCP(firstBroker()), Timeout: 10 * time.Second}
	resp, err := client.DeleteTopics(t.Context(), &kafka.DeleteTopicsRequest{Topics: []string{topic}})
	require.NoError(t, err)
	require.NoError(t, resp.Errors[topic])
	waitForTopicGone(t, topic)
}

// waitForTopicGone fails the test unless every broker stops reporting the
// topic. It asks for the topic configuration, which never creates a topic. A
// metadata request could create it again if the cluster allows that.
func waitForTopicGone(t *testing.T, topic string) {
	t.Helper()

	for address := range strings.SplitSeq(os.Getenv("KAFKA_BOOTSTRAP_SERVERS"), ",") {
		client := &kafka.Client{Addr: kafka.TCP(address), Timeout: 10 * time.Second}
		require.Eventually(t, func() bool {
			resp, err := client.DescribeConfigs(t.Context(), &kafka.DescribeConfigsRequest{
				Resources: []kafka.DescribeConfigRequestResource{{
					ResourceType: kafka.ResourceTypeTopic,
					ResourceName: topic,
				}},
			})
			return err == nil && len(resp.Resources) == 1 && errors.Is(resp.Resources[0].Error, kafka.UnknownTopicOrPartition)
		}, 30*time.Second, 200*time.Millisecond, "broker %s still reports topic %q", address, topic)
	}
}

// checkTopicConfigEventually returns a check that waits until every broker
// reports value for the setting.
func checkTopicConfigEventually(t *testing.T, topic, key, value string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		waitForTopicConfig(t, topic, key, value)
		return nil
	}
}

func testAccTopicResourceConfigWithSettings(name string, settings map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(settings)) {
		fmt.Fprintf(&b, "    %q = %q\n", k, settings[k])
	}
	return fmt.Sprintf(providerConfig+`
resource "kafka_topic" "test" {
  name               = %[1]q
  partitions         = 1
  replication_factor = 1
  configuration = {
%[2]s  }
}
`, name, b.String())
}

// TestAccTopicResourceUpdateKeepsUnmanagedSettings changes a managed key after
// another tool set a key that is not in configuration. The other key must stay.
func TestAccTopicResourceUpdateKeepsUnmanagedSettings(t *testing.T) {
	const topic = "update-keeps-unmanaged"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccTopicResourceConfigWithRetention(topic, `"60000"`),
				Check:  resource.TestCheckResourceAttr("kafka_topic.test", "configuration.retention.ms", "60000"),
			},
			{
				PreConfig: func() { setTopicConfigOutsideTerraform(t, topic, "max.message.bytes", "2097152") },
				Config:    testAccTopicResourceConfigWithRetention(topic, `"120000"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("kafka_topic.test", "configuration.%", "1"),
					resource.TestCheckResourceAttr("kafka_topic.test", "configuration.retention.ms", "120000"),
					checkTopicConfigEventually(t, topic, "retention.ms", "120000"),
					checkTopicConfigEventually(t, topic, "max.message.bytes", "2097152"),
				),
			},
		},
	})
}

// TestAccTopicResourceUpdateRemovedKeyReturnsToDefault removes one key from
// configuration. That setting must return to the broker default, and the key
// that stays must keep its value.
func TestAccTopicResourceUpdateRemovedKeyReturnsToDefault(t *testing.T) {
	const topic = "update-removes-key"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccTopicResourceConfigWithSettings(topic, map[string]string{
					"retention.ms":      "60000",
					"max.message.bytes": "2097152",
				}),
				Check: checkTopicConfigEventually(t, topic, "max.message.bytes", "2097152"),
			},
			{
				Config: testAccTopicResourceConfigWithSettings(topic, map[string]string{"retention.ms": "60000"}),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("kafka_topic.test", "configuration.%", "1"),
					resource.TestCheckNoResourceAttr("kafka_topic.test", "configuration.max.message.bytes"),
					checkTopicConfigEventually(t, topic, "max.message.bytes", defaultMaxMessageBytes),
					checkTopicConfigEventually(t, topic, "retention.ms", "60000"),
				),
			},
			{
				Config: testAccTopicResourceConfigWithSettings(topic, map[string]string{"max.message.bytes": "2097152"}),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkTopicConfigEventually(t, topic, "retention.ms", defaultRetentionMs),
					checkTopicConfigEventually(t, topic, "max.message.bytes", "2097152"),
				),
			},
		},
	})
}

// TestAccTopicResourceDeletedOutsideTerraform deletes a topic with a raw client.
// The next plan must notice and plan to create it again, and applying must
// create it.
func TestAccTopicResourceDeletedOutsideTerraform(t *testing.T) {
	const topic = "deleted-outside-terraform"
	config := testAccTopicResourceConfig(topic, 1, 1)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  resource.TestCheckResourceAttr("kafka_topic.test", "name", topic),
			},
			{
				PreConfig:          func() { deleteTopicOutsideTerraform(t, topic) },
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: config,
				Check:  resource.TestCheckResourceAttr("kafka_topic.test", "name", topic),
			},
		},
	})
}

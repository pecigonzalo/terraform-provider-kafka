package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestAccTopicDataSource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Read testing
			{
				Config: testAccTopicDataSourceConfig(existingTopic),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.kafka_topic.test", "id", existingTopic),
					resource.TestCheckResourceAttr("data.kafka_topic.test", "name", existingTopic),
					resource.TestCheckResourceAttr("data.kafka_topic.test", "partitions", "1"),
					resource.TestCheckResourceAttr("data.kafka_topic.test", "replication_factor", "1"),
				),
			},
		},
	})
}

func testAccTopicDataSourceConfig(name string) string {
	return fmt.Sprintf(providerConfig+`
data "kafka_topic" "test" {
  name = %[1]q
}
`, name)
}

func TestAccTopicDataSourceNotFound(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccTopicDataSourceConfig("ds-topic-does-not-exist"),
				// The provider reports a read failure when the topic is missing.
				ExpectError: regexp.MustCompile(`Unable to read topic`),
			},
		},
	})
}

func TestAccTopicDataSourceConfiguration(t *testing.T) {
	const name = "ds-config-topic"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Create the topic first so the data source has something to read.
			{
				Config: testAccTopicDataSourceConfigurationTopic(name, "60000"),
				Check:  resource.TestCheckResourceAttr("kafka_topic.test", "configuration.retention.ms", "60000"),
			},
			{
				Config: testAccTopicDataSourceConfigurationTopic(name, "60000") + `
data "kafka_topic" "test" {
  name = kafka_topic.test.name
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.kafka_topic.test", "name", name),
					// Broker defaults are returned too, so only assert the key we set.
					resource.TestCheckResourceAttr("data.kafka_topic.test", "configuration.retention.ms", "60000"),
				),
			},
		},
	})
}

func testAccTopicDataSourceConfigurationTopic(name string, retention string) string {
	return fmt.Sprintf(providerConfig+`
resource "kafka_topic" "test" {
  name               = %[1]q
  partitions         = 1
  replication_factor = 1
  configuration = {
    "retention.ms" = %[2]q
  }
}
`, name, retention)
}

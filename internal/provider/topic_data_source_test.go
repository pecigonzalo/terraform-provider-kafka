package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
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

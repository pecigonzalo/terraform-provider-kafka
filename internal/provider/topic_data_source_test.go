package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
)

func TestAccExampleDataSource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Read testing
			{
				Config: testAccExampleDataSourceConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.kafka_topic.test", "id", "example"),
					resource.TestCheckResourceAttr("data.kafka_topic.test", "name", "example"),
					resource.TestCheckResourceAttr("data.kafka_topic.test", "partitions", "3"),
					resource.TestCheckResourceAttr("data.kafka_topic.test", "replication_factor", "3"),
				),
			},
		},
	})
}

const testAccExampleDataSourceConfig = providerConfig + `
data "kafka_topic" "test" {
  name = "example"
}
`

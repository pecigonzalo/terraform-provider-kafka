package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
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

func testAccTopicResourceConfig(name string, partitions int, replication_factor int) string {
	return fmt.Sprintf(providerConfig+`
resource "kafka_topic" "test" {
  name = %[1]q
  partitions = %v
  replication_factor = %v
}
`, name, partitions, replication_factor)
}

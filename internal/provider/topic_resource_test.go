package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
)

func TestAccExampleResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Create and Read testing
			{
				Config: testAccExampleResourceConfig(
					"one",
					3,
					3,
				),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("msk_topic.test", "configurable_attribute", "one"),
					resource.TestCheckResourceAttr("msk_topic.test", "id", "example-id"),
				),
			},
			// ImportState testing
			{
				ResourceName:      "msk_topic.test",
				ImportState:       true,
				ImportStateVerify: true,
				// This is not normally necessary, but is here because this
				// example code does not have an actual upstream service.
				// Once the Read method is able to refresh information from
				// the upstream service, this can be removed.
				ImportStateVerifyIgnore: []string{"configurable_attribute"},
			},
			// Update and Read testing
			{
				Config: testAccExampleResourceConfig(
					"two",
					3,
					3,
				),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("msk_topic.test", "configurable_attribute", "two"),
				),
			},
			// Delete testing automatically occurs in TestCase
		},
	})
}

func testAccExampleResourceConfig(name string, partitions int, replication_factor int) string {
	return fmt.Sprintf(providerConfig+`
resource "msk_topic" "test" {
  name = %[1]q
  partitions = %v
  replication_factor = %v
}
`, name, partitions, replication_factor)
}

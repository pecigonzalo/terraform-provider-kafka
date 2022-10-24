package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/stretchr/testify/assert"
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

func TestIncreaseReplicas(t *testing.T) {
	assert := assert.New(t)
	desiredCount := 3
	currentReplicas := []int{2, 1}
	brokerIDs := []int{1, 2, 3}

	expectedReplicas := []int{2, 1, 3}
	newReplicas := increaseReplicas(desiredCount, currentReplicas, brokerIDs)

	assert.EqualValues(expectedReplicas, newReplicas, "Increase replica expected should be the same")
}

func TestReduceReplicas(t *testing.T) {
	assert := assert.New(t)

	desiredCount := 1
	currentReplicas := []int{2, 1, 3}
	leader := 2

	expectedReplicas := []int{2}
	newReplicas := reduceReplicas(desiredCount, currentReplicas, leader)

	assert.Equal(expectedReplicas, newReplicas, "Increase replica expected should be the same")

	desiredCount = 2
	currentReplicas = []int{2, 1, 3}
	leader = 2

	expectedReplicas = []int{2, 3}
	newReplicas = reduceReplicas(desiredCount, currentReplicas, leader)

	assert.Equal(expectedReplicas, newReplicas, "Increase replica expected should be the same")

	desiredCount = 2
	currentReplicas = []int{2, 1, 3, 5, 4}
	leader = 2

	expectedReplicas = []int{2, 4}
	newReplicas = reduceReplicas(desiredCount, currentReplicas, leader)

	assert.Equal(expectedReplicas, newReplicas, "Increase replica expected should be the same")
}

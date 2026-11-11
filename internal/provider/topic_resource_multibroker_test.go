package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

// TestAccTopicResourceReplicationFactorMultiBroker changes replication factor
// and then partitions on real brokers, so the reassignment wait and its request
// timeout are exercised end to end, and new partitions must get the topic's
// replication factor.
func TestAccTopicResourceReplicationFactorMultiBroker(t *testing.T) {
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_8_0)},
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccTopicResourceConfig("rf-multibroker", 3, 1),
				Check:  resource.TestCheckResourceAttr("kafka_topic.test", "replication_factor", "1"),
			},
			// Increase: waits for the reassignment to finish before the next Read.
			{
				Config: testAccTopicResourceConfig("rf-multibroker", 3, 3),
				Check:  resource.TestCheckResourceAttr("kafka_topic.test", "replication_factor", "3"),
			},
			// Decrease: exercises the reduce path against real replicas.
			{
				Config: testAccTopicResourceConfig("rf-multibroker", 3, 2),
				Check:  resource.TestCheckResourceAttr("kafka_topic.test", "replication_factor", "2"),
			},
			// Add partitions: new partitions are spread over the brokers and must
			// keep the replication factor, otherwise the next Read reports mixed
			// replica counts.
			{
				Config: testAccTopicResourceConfig("rf-multibroker", 5, 2),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("kafka_topic.test", "partitions", "5"),
					resource.TestCheckResourceAttr("kafka_topic.test", "replication_factor", "2"),
				),
			},
		},
	})
}

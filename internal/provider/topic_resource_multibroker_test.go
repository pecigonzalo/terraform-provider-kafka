package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

// TestAccTopicResourceReplicationFactorMultiBroker changes replication factor
// on real brokers, so the reassignment wait and its request timeout are
// exercised end to end. The default acceptance cluster has one broker, so this
// test starts its own three-broker cluster when KAFKA_TEST_BROKERS=3:
//
//	KAFKA_TEST_BROKERS=3 TF_ACC=1 \
//	  go test ./internal/provider -run TestAccTopicResourceReplicationFactorMultiBroker
func TestAccTopicResourceReplicationFactorMultiBroker(t *testing.T) {
	if clusterBrokerCount() < 3 {
		t.Skip("set KAFKA_TEST_BROKERS=3 to run this against a three-broker cluster")
	}

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
		},
	})
}

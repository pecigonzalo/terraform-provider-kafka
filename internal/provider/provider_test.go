package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

const (
	// providerConfig is a shared configuration to combine with the actual
	// test configuration so the Kafka client is properly configured.
	// The bootstrap server is not set here: TestMain starts an isolated Kafka
	// on a free port and exports it as KAFKA_BOOTSTRAP_SERVERS.
	providerConfig = `
provider "kafka" {
  tls = {
    enabled = false
  }
  sasl = {
    enabled = false
  }
}
`
)

// testAccProtoV6ProviderFactories are used to instantiate a provider during
// acceptance testing. The factory function will be invoked for every Terraform
// CLI command executed to create a provider server to which the CLI can
// reattach.
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"kafka": providerserver.NewProtocol6WithError(New("test")()),
}

func testAccPreCheck(t *testing.T) {}

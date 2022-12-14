package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

const (
	// providerConfig is a shared configuration to combine with the actual
	// test configuration so the Kafka client is properly configured.
	// It is also possible to use the KAFKA_ environment variables instead,
	// such as updating the Makefile and running the testing through that tool.
	providerConfig = `
provider "kafka" {
  bootstrap_servers = ["127.0.0.1:9092"]
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

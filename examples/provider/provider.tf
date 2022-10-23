terraform {
  required_providers {
    kafka = {
      source = "registry.terraform.io/pecigonzalo/kafka"
    }
  }
}

provider "kafka" {
  bootstrap_servers = ["127.0.0.1:9092"]
  tls = {
    enabled = false
  }
  sasl = {
    enabled = false
  }
}

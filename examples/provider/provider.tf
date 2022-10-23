terraform {
  required_providers {
    msk = {
      source = "registry.terraform.io/pecigonzalo/msk"
    }
  }
}

provider "msk" {
  bootstrap_servers = ["127.0.0.1:9092"]
  tls = {
    enabled = false
  }
  sasl = {
    enabled = false
  }
}

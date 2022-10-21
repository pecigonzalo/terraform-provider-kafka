terraform {
  required_providers {
    msk = {
      source = "registry.terraform.io/pecigonzalo/msk"
    }
  }
}

provider "msk" {
  bootstrap_servers = ["localhost:9198"]
  tls_enabled       = true
}

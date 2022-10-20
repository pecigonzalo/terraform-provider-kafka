terraform {
  required_providers {
    msk = {
      source = "registry.terraform.io/pecigonzalo/msk"
    }
  }
}

provider "msk" {}

data "msk_topic" "example" {}

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

locals {
  topics = {
    "example" = {
      partitions         = 1
      replication_factor = 1
      configuration = {
        "cleanup.policy" = "delete"
      }
    }
  }
}

resource "msk_topic" "example" {
  for_each           = local.topics
  name               = each.key
  partitions         = each.value.partitions
  replication_factor = each.value.replication_factor
  configuration      = each.value.configuration
}

data "msk_topic" "example" {
  name = msk_topic.example["example"].id
}

output "msk_topic_resource" {
  value = msk_topic.example
}

output "msk_topic_data" {
  value = msk_topic.example
}

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

resource "kafka_topic" "example" {
  for_each           = local.topics
  name               = each.key
  partitions         = each.value.partitions
  replication_factor = each.value.replication_factor
  configuration      = each.value.configuration
}

data "kafka_topic" "example" {
  name = kafka_topic.example["example"].id
}

output "kafka_topic_resource" {
  value = kafka_topic.example
}

output "kafka_topic_data" {
  value = kafka_topic.example
}

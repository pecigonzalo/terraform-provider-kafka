---
# generated by https://github.com/hashicorp/terraform-plugin-docs
page_title: "kafka_topic Resource - terraform-provider-kafka"
subcategory: ""
description: |-
  Kafka Topic resource
---

# kafka_topic (Resource)

Kafka Topic resource

## Example Usage

```terraform
resource "kafka_topic" "example" {
  name               = "example"
  partitions         = 3
  replication_factor = 3
  configuration = {
    "cleanup.policy" = "cleanup"
  }
}
```

<!-- schema generated by tfplugindocs -->
## Schema

### Required

- `name` (String) Topic name
- `partitions` (Number) Topic partitions count
- `replication_factor` (Number) Topic replication factor count

### Optional

- `configuration` (Map of String) Configuration

### Read-Only

- `id` (String) Topic id

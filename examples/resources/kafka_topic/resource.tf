resource "kafka_topic" "example" {
  name               = "example"
  partitions         = 3
  replication_factor = 3
  configuration = {
    "cleanup.policy" = "cleanup"
  }
}

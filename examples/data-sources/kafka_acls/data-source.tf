# Every ACL on the cluster.
data "kafka_acls" "all" {}

# The ACLs of one topic, including wildcard and prefixed ACLs that apply to it.
data "kafka_acls" "orders" {
  resource_type         = "topic"
  resource_name         = "orders"
  resource_pattern_type = "match"
}

# What a principal is allowed to read.
data "kafka_acls" "alice_reads" {
  principal       = "User:alice"
  operation       = "read"
  permission_type = "allow"
}

output "alice_read_acl_ids" {
  value = [for acl in data.kafka_acls.alice_reads.acls : acl.id]
}

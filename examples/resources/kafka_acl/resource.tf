# Allow a user to read a topic.
resource "kafka_acl" "read_orders" {
  resource_type   = "topic"
  resource_name   = "orders"
  principal       = "User:alice"
  operation       = "read"
  permission_type = "allow"
}

# Allow a consumer group prefix, from one host only.
resource "kafka_acl" "orders_consumers" {
  resource_type         = "group"
  resource_name         = "orders-"
  resource_pattern_type = "prefixed"
  principal             = "User:alice"
  host                  = "10.0.0.5"
  operation             = "read"
  permission_type       = "allow"
}

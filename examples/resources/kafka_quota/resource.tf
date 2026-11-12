# Limit one user's traffic through one client application.
resource "kafka_quota" "billing" {
  user      = { name = "alice" }
  client_id = { name = "billing" }

  producer_byte_rate = 1048576
  consumer_byte_rate = 2097152
  request_percentage = 25
}

# Apply a default to every client id that has no quota of its own.
resource "kafka_quota" "default_client" {
  client_id = { default = true }

  producer_byte_rate = 524288
}

# Limit new connections from one IP address.
resource "kafka_quota" "office" {
  ip = { name = "203.0.113.7" }

  connection_creation_rate = 10
}

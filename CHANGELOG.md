# Changelog

## Unreleased

BREAKING CHANGES:

* provider: `tls.enabled` now defaults to `true`, as the schema always documented. Set `tls.enabled = false` to connect without TLS. (#152)
* provider: `bootstrap_servers` is now optional and falls back to the comma-separated `KAFKA_BOOTSTRAP_SERVERS` environment variable. The implicit `localhost:9092` default is removed, so a configuration with neither set is now an error. (#152)
* provider: A malformed `KAFKA_SASL_ENABLED` or `KAFKA_TIMEOUT` value now fails provider configuration instead of being silently ignored. (#156)

FEATURES:

* **New Resource:** `kafka_acl` manages a single ACL entry (resource type, name and pattern type, principal, host, operation and permission type). Every attribute forces replacement because Kafka cannot update an ACL. Import with `resource_type|resource_name|resource_pattern_type|principal|host|operation|permission_type`, for example `topic|orders|literal|User:alice|*|read|allow`, where `%` and `|` inside a value are written `%25` and `%7C`. (#177)
* **New Resource:** `kafka_quota` manages client quotas for a `user`, `client_id` or `ip` entity, including the default entity. Supports `producer_byte_rate`, `consumer_byte_rate`, `request_percentage`, `controller_mutation_rate` and `connection_creation_rate`. Requires Kafka 2.6 or later. Import with an id such as `user=alice,client-id=billing`, `ip=10.0.0.1` or `user=<default>`. (#178)

ENHANCEMENTS:

* resource/`kafka_topic`: `partitions` can no longer be reduced and `replication_factor` can no longer exceed the broker count; both are now rejected at plan time instead of partway through apply. (#159)
* resource/`kafka_topic`: `name` is validated at plan time to use only letters, digits, `.`, `_` and `-`, to be 1 to 249 characters long, and not to be `.` or `..`. `partitions`, `replication_factor` and the provider `timeout` must be at least 1. (#154, #158)
* resource/`kafka_topic`: `replication_factor` changes now wait until the reassignment has finished before the apply completes. If the wait exceeds the provider `timeout`, the error asks you to run `terraform plan` to refresh. (#161)
* resource/`kafka_topic`: Partition and replication factor updates are computed from the topic's live state on the broker rather than from possibly stale Terraform state. (#153, #181)
* provider: SASL mechanisms `scram-sha256` and `scram-sha512` now work, and `scram-sha-256` and `scram-sha-512` are also accepted. SASL username and password fall back to `KAFKA_SASL_*` environment variables independently of `mechanism`. (#152)
* resource/`kafka_topic`: Errors from creating, deleting and altering topic configuration are now reported for every affected topic instead of only the first. (#157)

BUG FIXES:

* resource/`kafka_topic`: `configuration` now tracks only the keys set in your configuration, so broker defaults no longer cause perpetual drift. `configuration` is now optional only. Existing state that holds the full broker configuration plans a one-time update that clears `configuration` when the block is omitted, which resets those keys to broker defaults. (#154)
* resource/`kafka_topic`: A failed replica reassignment is now reported instead of being silently ignored. (#153)
* resource/`kafka_topic`: Deleting a topic that is already gone succeeds and no longer panics. (#153)
* resource/`kafka_topic`: `configuration` values are now converted with typed entries instead of stripping quotes from their string form, and null or unknown values are skipped. (#153)

NOTES:

* The provider now talks to Kafka through a client built on `kafka-go` instead of `topicctl`, and `topicctl` is no longer a dependency. This is an internal change, but the behavior changes below follow from it. (#167, #169, #170, #171, #173, #174, #176, #180, #181)
* resource/`kafka_topic`: A topic deleted outside Terraform is now noticed after a short settle window of about two seconds (eight consecutive not-found checks) instead of immediately. This avoids removing a topic from state because of a briefly lagging broker. (#176)
* resource/`kafka_topic`: A failure to create a topic now uses the diagnostic title `Client Error` instead of `Client Response Error`. The message still names the topic and the broker error. (#176)
* resource/`kafka_topic`: Replica placement for `replication_factor` changes and partition growth is now deterministic and rack-aware, and differs from the randomized placement `topicctl` used. New replicas go to the rack holding the fewest replicas of that partition, then to the least loaded broker. Increases only append replicas to the existing ones, and decreases keep the leader. This affects which brokers new replicas and partitions land on, not correctness. (#180, #181)
* provider: Cluster metadata is cached for at most two seconds, so reads see recent topic changes sooner at the cost of slightly more frequent metadata requests. (#173)
* data source/`kafka_topic`: Reads through the new client. The `version` attribute is still `0`, as before. (#173)

## 0.5.3 (2026-07-15)

NOTES:

* Dependency updates only: `github.com/docker/cli`, `google.golang.org/grpc` and `golang.org/x/net`. (#139, #140, #141)

## 0.5.2 (2026-07-15)

NOTES:

* Dependency updates only, including `topicctl` 1.23.1, `kafka-go` 0.4.51, `terraform-plugin-docs` 0.25.0, the Terraform plugin runtime libraries, `golang.org/x/crypto`, `google.golang.org/grpc`, `github.com/opencontainers/runc` and `goreleaser-action`. (#113, #114, #121, #122, #128, #131, #132, #133, #134, #135, #136, #138)

## 0.5.1 (2024-09-04)

NOTES:

* Maintenance release: dependency updates (`kafka-go`, `topicctl`, `terraform-plugin-docs`, `grpc` and GitHub Actions), and the acceptance tests now run against Terraform 1.4 through 1.8 and a KRaft Kafka image. (#43, #44, #45, #51, #66, #67, #112)

## 0.5.0 (2023-06-13)

NOTES:

* The provider is now built with Go 1.19. The acceptance test cluster uses KRaft Kafka images. (#37)

## 0.4.0 (2023-01-10)

BUG FIXES:

* resource/`kafka_topic`: A topic that no longer exists is handled during plan and apply instead of failing.

## 0.3.0 (2022-12-21)

NOTES:

* Dependency updates only: `terraform-plugin-framework` 1.0.1 and `topicctl` 1.8.0. (#11, #12, #13, #14, #15)

## 0.2.0 (2022-11-24)

BUG FIXES:

* provider: Errors raised while creating the Kafka client are now reported. (#10)

## 0.1.0 (2022-10-24)

ENHANCEMENTS:

* provider: Configuration can be supplied through environment variables (`KAFKA_BOOTSTRAP_SERVERS`, `KAFKA_SASL_ENABLED`, `KAFKA_TIMEOUT` and others).
* provider: The Kafka client has a default timeout of 300 seconds.

BUG FIXES:

* resource/`kafka_topic`: Replicas are no longer sorted when a topic is updated.
* resource/`kafka_topic`: Fixed the replica generator used when reducing the replica count.

## 0.0.1 (2022-10-23)

FEATURES:

* **New Resource:** `kafka_topic`
* **New Data Source:** `kafka_topic`
* provider: Multiple authentication methods are supported.

NOTES:

* Initial release.

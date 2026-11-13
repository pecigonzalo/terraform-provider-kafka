# Changelog

## Unreleased

BREAKING CHANGES:

* provider: `tls.enabled` is now `true` by default. The schema always said this. To connect without TLS, set `tls.enabled = false`. (#152)
* provider: `bootstrap_servers` is now optional. If you do not set it, the provider reads the `KAFKA_BOOTSTRAP_SERVERS` environment variable. This variable holds a comma-separated list. The old default `localhost:9092` is removed. If you set neither, the provider gives an error. (#152)
* provider: A bad value in `KAFKA_SASL_ENABLED` or `KAFKA_TIMEOUT` now causes an error. Before, the provider ignored it. (#156)

FEATURES:

* **New Resource:** `kafka_acl` manages one ACL entry. Kafka cannot change an ACL, so a change to any attribute replaces the entry. To import, use the id `resource_type|resource_name|resource_pattern_type|principal|host|operation|permission_type`, for example `topic|orders|literal|User:alice|*|read|allow`. In a value, write `%` as `%25` and `|` as `%7C`. (#177)
* **New Resource:** `kafka_quota` manages the quotas of one `user`, `client_id` or `ip` entity. An entity can be the default entity. The limits are `producer_byte_rate`, `consumer_byte_rate`, `request_percentage`, `controller_mutation_rate` and `connection_creation_rate`. Kafka 2.6 or later is necessary. To import, use an id such as `user=alice,client-id=billing`, `ip=10.0.0.1` or `user=<default>`. (#178)

ENHANCEMENTS:

* resource/`kafka_topic`: Two changes now cause an error during plan. Before, they caused an error during apply. You cannot lower `partitions`. You cannot set `replication_factor` above the number of brokers. (#159)
* resource/`kafka_topic`: The provider checks `name` during plan. It can only have letters, digits, `.`, `_` and `-`. It must have 1 to 249 characters. It cannot be `.` or `..`. `partitions`, `replication_factor` and the provider `timeout` must be 1 or more. (#154, #158)
* resource/`kafka_topic`: When you change `replication_factor`, apply now waits until Kafka finishes the change. If this takes longer than the provider `timeout`, the error tells you to run `terraform plan`. (#161)
* resource/`kafka_topic`: When you change `partitions` or `replication_factor`, the provider uses the topic state from the broker. Before, it could use old Terraform state. (#153, #181)
* resource/`kafka_topic`: When more than one topic fails to create, delete or change, the error now lists all of them. Before, it showed only the first. (#157)
* provider: The SASL mechanisms `scram-sha256` and `scram-sha512` now work. The spellings `scram-sha-256` and `scram-sha-512` are also valid. The SASL user name and password can come from the `KAFKA_SASL_*` environment variables, with any `mechanism`. (#152)

BUG FIXES:

* resource/`kafka_topic`: `configuration` now tracks only the keys that you set. Broker defaults no longer cause a difference in each plan. `configuration` is now optional only. If your state holds all broker settings and you do not set `configuration`, the next plan clears it one time. This resets those keys to the broker defaults. (#154)
* resource/`kafka_topic`: The provider now shows an error when a replica change fails. Before, it ignored the error. (#153)
* resource/`kafka_topic`: Delete no longer stops with a panic. Delete succeeds if the topic is already gone. (#153)
* resource/`kafka_topic`: The provider now converts `configuration` values with typed entries. Before, it removed quotes from the text of the value. The provider skips null and unknown values. (#153)

NOTES:

* The provider now uses a client based on `kafka-go`. It no longer uses `topicctl`. This is an internal change. The changes below come from it. (#167, #169, #170, #171, #173, #174, #176, #180, #181)
* resource/`kafka_topic`: If you delete a topic outside Terraform, the provider now sees this after about two seconds. Before, it saw this at once. The provider checks eight times in a row before it removes the topic from state. This stops a slow broker from causing a wrong removal. (#176)
* resource/`kafka_topic`: When a topic fails to create, the error title is now `Client Error`. Before, it was `Client Response Error`. The message still has the topic name and the broker error. (#176)
* resource/`kafka_topic`: The provider now chooses the brokers for new replicas and new partitions with fixed rules. `topicctl` chose them at random. The rules are:
    * Put each new replica in the rack that has the fewest replicas of that partition.
    * Then use the broker that has the fewest replicas.
    * To increase `replication_factor`, the provider only adds replicas. To decrease it, the provider keeps the leader.
    * This changes which brokers hold the new replicas. It does not change if the result is correct. (#180, #181)
* provider: The provider keeps cluster information for 2 seconds at most. Reads see new topic changes sooner. The provider sends a little more requests to the brokers. (#173)
* data source/`kafka_topic`: The data source uses the new client. `version` is still `0`, as before. (#173)

## 0.5.3 (2026-07-15)

NOTES:

* This release has only dependency updates: `github.com/docker/cli`, `google.golang.org/grpc` and `golang.org/x/net`. (#139, #140, #141)

## 0.5.2 (2026-07-15)

NOTES:

* This release has only dependency updates. They include `topicctl` 1.23.1, `kafka-go` 0.4.51, `terraform-plugin-docs` 0.25.0, the Terraform plugin libraries, `golang.org/x/crypto`, `google.golang.org/grpc`, `github.com/opencontainers/runc` and `goreleaser-action`. (#113, #114, #121, #122, #128, #131, #132, #133, #134, #135, #136, #138)

## 0.5.1 (2024-09-04)

NOTES:

* This release has dependency updates (`kafka-go`, `topicctl`, `terraform-plugin-docs`, `grpc` and GitHub Actions). The tests now run with Terraform 1.4 to 1.8 and a Kafka image that uses KRaft. (#43, #44, #45, #51, #66, #67, #112)

## 0.5.0 (2023-06-13)

NOTES:

* The provider now uses Go 1.19. The test cluster uses Kafka images that use KRaft. (#37)

## 0.4.0 (2023-01-10)

BUG FIXES:

* resource/`kafka_topic`: Plan and apply now work when the topic does not exist. Before, they failed.

## 0.3.0 (2022-12-21)

NOTES:

* This release has only dependency updates: `terraform-plugin-framework` 1.0.1 and `topicctl` 1.8.0. (#11, #12, #13, #14, #15)

## 0.2.0 (2022-11-24)

BUG FIXES:

* provider: The provider now shows the errors that occur when it creates the Kafka client. (#10)

## 0.1.0 (2022-10-24)

ENHANCEMENTS:

* provider: You can set the configuration with environment variables, for example `KAFKA_BOOTSTRAP_SERVERS`, `KAFKA_SASL_ENABLED` and `KAFKA_TIMEOUT`.
* provider: The Kafka client has a default timeout of 300 seconds.

BUG FIXES:

* resource/`kafka_topic`: An update no longer sorts the replicas.
* resource/`kafka_topic`: The provider now makes correct replica lists when it reduces the number of replicas.

## 0.0.1 (2022-10-23)

FEATURES:

* **New Resource:** `kafka_topic`
* **New Data Source:** `kafka_topic`
* provider: The provider supports more than one authentication method.

NOTES:

* This is the first release.

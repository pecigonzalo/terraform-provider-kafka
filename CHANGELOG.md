# Changelog

The entries for the next release are in the directory `.changelog/`, one file for each pull request. To see them, run `make changelog-preview`. To add an entry, read "Changelog entries" in `README.md`.

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

# Changelog

## Unreleased

BREAKING CHANGES:

* provider: `tls.enabled` is now `true` by default. In v0.5.3, TLS was off if you did not set `tls.enabled`. The documentation said the default was `true`. If your broker does not use TLS, set `tls = { enabled = false }`. (#152)
* provider: The SASL mechanism `plain` now sends PLAIN. In v0.5.3, it sent SCRAM-SHA-256. If your broker uses SCRAM-SHA-256, set `mechanism = "scram-sha256"`. (#152)
* provider: The SASL mechanisms `scram-sha256` and `scram-sha512` now work. The names `scram-sha-256` and `scram-sha-512` are also valid. In v0.5.3, these mechanisms gave the error `unable to detect SASL mechanism`. (#152)
* provider: The SASL user name and password now come from the configuration or from `KAFKA_SASL_USERNAME` and `KAFKA_SASL_PASSWORD`. This is true for all mechanisms. In v0.5.3, the provider read them from the configuration only if you set `mechanism`. Check that your credentials are correct. (#152)
* provider: `bootstrap_servers` is now optional. If you do not set it, the provider uses `KAFKA_BOOTSTRAP_SERVERS`. This variable holds a comma-separated list. The default `localhost:9092` is removed. If you set neither, the provider gives an error. Set one of them. (#152)
* provider: A bad value in `KAFKA_SASL_ENABLED` or `KAFKA_TIMEOUT` now gives an error. In v0.5.3, the provider ignored it. Correct the value. (#156)
* provider: `timeout` must be 1 or more. In v0.5.3, the provider did not check it. If you set `timeout` to 0, use a larger value. (#154)
* provider: There is no default SASL mechanism now. In earlier versions, the default was `aws-msk-iam`. SASL is still on by default, so you must set `sasl.mechanism` or `KAFKA_SASL_MECHANISM`. If you used the old default, set `mechanism = "aws-msk-iam"`. If you set no mechanism and SASL is on, the provider gives an error that lists the valid mechanisms. To turn SASL off, set `sasl = { enabled = false }`. (#186)
* provider: With `aws-msk-iam`, a SASL user name or password is now an error. This includes `KAFKA_SASL_USERNAME` and `KAFKA_SASL_PASSWORD`. In earlier versions, the provider ignored these values without a message. This mechanism uses your AWS credentials. Remove the user name and the password. (#186)
* provider: With `plain`, `scram-sha256` and `scram-sha512`, an empty SASL user name or password is now an error. The error names the missing value. It never shows the value. (#186)
* provider: An empty `KAFKA_*` environment variable is now the same as an unset variable. In earlier versions, an empty `KAFKA_SASL_MECHANISM` gave an error. (#186)
* provider: With `aws-msk-iam`, an empty AWS region is now an error. Set `AWS_REGION` or a region in the AWS configuration. (#186)
* resource/`kafka_acl`, data source `kafka_acls`: `host` must be `*` or one IP address. Kafka matches hosts exactly. It does not support network ranges (such as `10.0.0.0/8`), partial wildcards or host names. An ACL with such a host never matches a client, so a deny ACL did nothing. Plan now gives an error. If you use a range or a host name, replace it with one ACL for each IP address. The principal type `User` must have this exact case. A value such as `user:alice` gives an error. Write `User:alice`. (#184)
* resource/`kafka_topic`: `partitions` and `replication_factor` must be 1 or more. `name` must have 1 to 249 characters. It can have only letters, digits, `.`, `_` and `-`. It cannot be `.` or `..`. Plan gives an error if a value is not valid. Correct the value. (#154, #158)
* resource/`kafka_topic`: Plan now gives an error if you decrease `partitions`. Plan also gives an error if `replication_factor` is more than the number of brokers. In v0.5.3, these errors came during apply. (#159)
* resource/`kafka_topic`: `configuration` is now optional only. The provider now tracks only the keys that you set. In v0.5.3, `configuration` was also computed, and state held all broker settings. Read the upgrade note below before you apply. (#154)

UPGRADE NOTES:

* resource/`kafka_topic`: Run `terraform plan` after you upgrade. Read the plan before you apply it. If you do not set `configuration`, the plan changes `configuration` to null. If you set some keys, the plan removes the other keys from state. When you apply, the provider sends only the keys that you set. Kafka then resets all other topic settings to the broker defaults. This includes settings that you or another tool set outside Terraform. To keep a setting, add it to `configuration` before you apply. (#154, #176)
* provider: Before you upgrade, check your provider configuration: (#186)
    * If SASL is on and you set no mechanism, set `mechanism`. If you used the old default, use `mechanism = "aws-msk-iam"`.
    * If you use `aws-msk-iam`, remove `sasl.username`, `sasl.password`, `KAFKA_SASL_USERNAME` and `KAFKA_SASL_PASSWORD`.
    * If you use `plain`, `scram-sha256` or `scram-sha512`, set a user name and a password.
    * If you use `aws-msk-iam`, make sure that an AWS region is set.
    * If you set `KAFKA_TIMEOUT` to 0 or less, use a value of 1 or more.

FEATURES:

* **New Resource:** `kafka_acl` manages one ACL entry. Kafka cannot change an ACL, so a change to any attribute replaces the entry. To import, use the id `resource_type|resource_name|resource_pattern_type|principal|host|operation|permission_type`, for example `topic|orders|literal|User:alice|*|read|allow`. In a value, write `%` as `%25` and `|` as `%7C`. (#177)
* **New Resource:** `kafka_quota` manages the quotas of one `user`, `client_id` or `ip` entity. An entity can be the default entity. The limits are `producer_byte_rate`, `consumer_byte_rate`, `request_percentage`, `controller_mutation_rate` and `connection_creation_rate`. Kafka 2.6 or later is necessary. To import, use an id such as `user=alice,client-id=billing`, `ip=10.0.0.1` or `user=<default>`. (#178)
* **New Data Source:** `kafka_acls` lists the ACL entries that match the filters. The filters are `resource_type`, `resource_name`, `resource_pattern_type`, `principal`, `host`, `operation` and `permission_type`. A filter that you do not set matches all values. The result is the list `acls`. It is sorted by `id`. Each entry has the attributes and the `id` of `kafka_acl`. (#184)

ENHANCEMENTS:

* provider: The provider now shows a warning if the mechanism is `plain` and TLS is off. With this setup, the password goes over the network in clear text. (#186)
* provider: The time to open one connection to a broker is now the smaller of `timeout` and 30 seconds. Before, it was `timeout`, which is 300 seconds by default. One broker that you cannot reach no longer stops an operation for five minutes. `timeout` still limits each request. (#186)
* provider: New environment variables `KAFKA_TLS_ENABLED` and `KAFKA_TLS_SKIP_VERIFY`. A value in the configuration takes precedence. The documentation now lists all environment variables, and the risks of `tls.skip_verify`. (#186)
* resource/`kafka_acl`: Create and delete now wait for as long as the provider `timeout` allows, until the broker shows the change. If this takes longer, the error tells you to run `terraform apply` again. (#184)
* resource/`kafka_topic`: When you change `replication_factor`, apply now waits until Kafka finishes the change. If this takes longer than the provider `timeout`, the error tells you to run `terraform plan`. (#161)
* resource/`kafka_topic`: When you change `partitions` or `replication_factor`, the provider reads the current topic state from the broker. In v0.5.3, it could use old Terraform state. (#153, #181)
* resource/`kafka_topic`: When more than one topic fails to create, delete or change, the error now lists all of them. Before, it showed only the first. (#157)

BUG FIXES:

* resource/`kafka_topic`: The provider now shows an error when a change to the replicas of a topic fails. In v0.5.3, it ignored the error. (#153)
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

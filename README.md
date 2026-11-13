# Terraform Kafka Provider

A Terraform provider for Kafka powered by [kafka-go](https://github.com/segmentio/kafka-go).

## State
- [x] Migrate to use `kafka-go` directly instead of `topicctl` wrappers
- [x] Authentication
  - [x] SASL
    - [x] IAM
    - [x] SCRAM
    - [x] PLAINTEXT
  - [x] PLAINTEXT
- [x] Topic management
- [x] ACL management
- [x] Quota management
- [x] Development
  - [x] Local acceptance testing Kafka
  - [x] Automated release process
  - [x] Automated testing process

## Requirements

- [Terraform](https://www.terraform.io/downloads.html) >= 1.0
- [Go](https://golang.org/doc/install) >= 1.27

### Optional

- [direnv](https://direnv.net/)
- [Nix](https://nixos.org/) with [Flakes](https://nixos.wiki/wiki/Flakes)

## Building The Provider

1. Clone the repository
1. Enter the repository directory
1. Build the provider using the Go `install` command:

```shell
go install
```

## Running checks

The Nix development shell has the tools that CI uses. Enter it with `nix develop`. Then run these commands:

```shell
go test -race -count=1 ./...
golangci-lint run ./...
govulncheck ./...
```

`make testacc` runs the acceptance tests. It needs Docker.

## Adding Dependencies

This provider uses [Go modules](https://github.com/golang/go/wiki/Modules).
Please see the Go documentation for the most up to date information about using Go modules.

To add a new dependency `github.com/author/dependency` to your Terraform provider:

```shell
go get github.com/author/dependency
go mod tidy
```

Then commit the changes to `go.mod` and `go.sum`.

## Using the provider

```terraform
terraform {
  required_providers {
    kafka = {
      source = "pecigonzalo/kafka"
    }
  }
}

# This example is for local development only. It turns TLS and SASL off.
# TLS and SASL are on by default. If you keep SASL on, you must set
# `sasl.mechanism` (`plain`, `scram-sha256`, `scram-sha512` or `aws-msk-iam`).
provider "kafka" {
  bootstrap_servers = ["localhost:9092"]
  tls  = { enabled = false }
  sasl = { enabled = false }
}

resource "kafka_topic" "example" {
  name               = "example"
  partitions         = 3
  replication_factor = 1
  configuration = {
    "retention.ms" = "60000"
  }
}
```

The provider can also be configured with the `KAFKA_BOOTSTRAP_SERVERS`,
`KAFKA_TLS_ENABLED`, `KAFKA_TLS_SKIP_VERIFY`, `KAFKA_SASL_ENABLED`,
`KAFKA_SASL_MECHANISM`, `KAFKA_SASL_USERNAME`, `KAFKA_SASL_PASSWORD` and
`KAFKA_TIMEOUT` environment variables. TLS and SASL are enabled by default.
There is no default SASL mechanism: you must set `sasl.mechanism` or
`KAFKA_SASL_MECHANISM`. See the [documentation](docs/) for all options.

## Developing the Provider

If you wish to work on the provider, you'll first need [Go](http://www.golang.org) installed on your machine (see [Requirements](#requirements) above).

To compile the provider, run `go install`. This will build the provider and put the provider binary in the `$GOPATH/bin` directory.

To generate or update documentation, run `go generate`.

In order to run the full suite of Acceptance tests, run `make testacc`.

_Note:_ Acceptance tests start a local Kafka container with Docker and need
Terraform installed.

```shell
make testacc
```

Acceptance tests run against a three-broker KRaft cluster, so replication
factor and partition changes are tested on real replicas. Every run starts its
own containers on free ports, so runs can execute in parallel.

## Changelog entries

Each pull request that changes what users see adds one file to the directory `.changelog/`. Name the file with the number of your pull request and `.txt`, for example `.changelog/154.txt`. The name has only digits before `.txt`. A change that only affects the code, the tests or the build needs no file.

The file has one block for each entry. A block has this form:

````markdown
```release-note:TYPE
Text of the entry. (#154)
```
````

Write the text of the entry in the same style as the existing entries. Start with the thing that changes, for example `resource/`kafka_topic`:` or `provider:`. End the text with the number of your pull request in parentheses, for example `(#154)`. The build prints the text as you write it.

Use one of these types:

| Type | Section | Use it for |
|---|---|---|
| `breaking-change` | BREAKING CHANGES | A change that can break an existing configuration |
| `upgrade-note` | UPGRADE NOTES | Steps to do when you upgrade |
| `new-resource` | FEATURES | A new resource. The build adds `**New Resource:**`. |
| `new-data-source` | FEATURES | A new data source. The build adds `**New Data Source:**`. |
| `feature` | FEATURES | Any other new feature |
| `enhancement` | ENHANCEMENTS | An improvement to a feature that exists |
| `bug` | BUG FIXES | A fix |
| `note` | NOTES | Information that is not a change to a feature |

Examples:

````markdown
```release-note:breaking-change
provider: `timeout` must be 1 or more. Set a larger value if you used 0. (#154)
```

```release-note:upgrade-note
resource/`kafka_topic`: Run `terraform plan` after you upgrade. (#154)
```

```release-note:new-resource
`kafka_quota` manages the quotas of one entity. (#154)
```

```release-note:new-data-source
`kafka_acls` lists the ACL entries that match the filters. (#154)
```

```release-note:feature
provider: Support for a new SASL mechanism. (#154)
```

```release-note:enhancement
provider: New environment variable `KAFKA_TLS_ENABLED`. (#154)
```

```release-note:bug
resource/`kafka_topic`: Delete succeeds if the topic is already gone. (#154)
```

```release-note:note
provider: The build now uses a newer Go version. (#154)
```
````

One file can have more than one block. Each block ends with a marker.

If an entry belongs to more than one pull request, write all numbers in the marker, for example `(#154, #158)`. Name the file with the first number, `.changelog/154.txt`. If the entry has a list, you can put the marker at the end of the first line instead of the end of the last line.

To check the files, run `make changelog-check`. It checks that the file name has only digits, that every block has a known type, that every block has a marker, and that the first number of the marker is the number in the file name. To see the entries that are not in a release yet, commit them and run `make changelog-preview`.

### Release steps

1. Make sure that `main` has all the entries for the release and that `make changelog-check` passes.
2. Run `make changelog-release VERSION=x.y.z`. This writes the section `## x.y.z (date)` at the top of `CHANGELOG.md`. The build uses the entries that are new since the last release tag.
3. Read the new section. If an entry is wrong, run `git checkout CHANGELOG.md`, correct the entry file, commit it, and do step 2 again.
4. Commit `CHANGELOG.md`, merge the change, and create the tag `vx.y.z` on the merge commit.

Do not delete entry files after a release. The next build finds the new entries by comparing the files at the last release tag with the files at `HEAD`. The tag must be on a commit that has `.changelog/`. If the last tag was made before `.changelog/` existed, the build uses all entries.

## FAQ

> **Why not use [Mongey/terraform-provider-kafka](https://github.com/Mongey/terraform-provider-kafka)?**

Mongey's provider while supporting many features is powered by [sarama](https://github.com/Shopify/sarama) which while a great library, its not as ergonomic as **kafka-go**. Additionally, I wanted to support AWS MSK, which is not currently suported by **sarama**. I initially was going to re-write Mongey's provider, but found that it would be simpler to start over with a minimal feature set.

> **Does it support Zookeeper admin actions?**

Not at this time. Given Kafka migration to KRaft and easy of use of the broker based admin API, I would rather focus on those for now.

## License

This project uses the MIT license. Read the [LICENSE](LICENSE) file.

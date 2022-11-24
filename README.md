# Terraform Kafka Provider

A Terraform provider for Kafka powered by [kafka-go](https://github.com/segmentio/kafka-go).

## State
- [ ] Migrate to use `kafka-go` directly instead of `topicctl` wrappers
- [x] Authentication
  - [x] SASL
    - [x] IAM
    - [x] SCRAM
    - [x] PLAINTEXT
  - [x] PLAINTEXT
- [x] Topic management
- [ ] ACL management
- [ ] Quota management
- [x] Development
  - [x] Local acceptance testing Kafka
  - [x] Automated release process
  - [x] Automated testing process

## Requirements

- [Terraform](https://www.terraform.io/downloads.html) >= 1.0
- [Go](https://golang.org/doc/install) >= 1.18

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

Fill this in for each provider

## Developing the Provider

If you wish to work on the provider, you'll first need [Go](http://www.golang.org) installed on your machine (see [Requirements](#requirements) above).

To compile the provider, run `go install`. This will build the provider and put the provider binary in the `$GOPATH/bin` directory.

To generate or update documentation, run `go generate`.

In order to run the full suite of Acceptance tests, run `make testacc`.

_Note:_ Acceptance tests create real resources, and often cost money to run.

```shell
make testacc
```

## FAQ

> **Why not use [Mongey/terraform-provider-kafka](https://github.com/Mongey/terraform-provider-kafka)?**

Mongey's provider while supporting many features is powered by [sarama](https://github.com/Shopify/sarama) which while a great library, its not as ergonomic as **kafka-go**. Additionally, I wanted to support AWS MSK, which is not currently suported by **sarama**. I initially was going to re-write Mongey's provider, but found that it would be simpler to start over with a minimal feature set.

> **Does it support Zookeeper admin actions?**

Not at this time. Given Kafka migration to KRaft and easy of use of the broker based admin API, I would rather focus on those for now.

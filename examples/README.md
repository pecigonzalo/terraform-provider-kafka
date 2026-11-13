# Examples

This directory holds example Terraform configurations. The documentation uses most of them. You can also run them with the Terraform CLI.

The command `go generate ./...` formats the examples with `terraform fmt`. It also builds the files in `docs/` from the examples and from the provider schema.

The documentation tool reads only these files. It ignores all other `*.tf` files. This lets you keep examples that you can run, but that you do not want in the documentation.

* `provider/provider.tf`: the example for the provider page.
* `data-sources/<data source name>/data-source.tf`: the example for the page of that data source.
* `resources/<resource name>/resource.tf`: the example for the page of that resource.

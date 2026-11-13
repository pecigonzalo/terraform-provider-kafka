//go:build tools

package tools

import (
	// Documentation generation
	_ "github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs"

	// Changelog generation
	_ "github.com/hashicorp/go-changelog/cmd/changelog-build"
)

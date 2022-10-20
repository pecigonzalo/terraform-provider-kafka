package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure MskProvider satisfies various provider interfaces.
var _ provider.Provider = &MskProvider{}
var _ provider.ProviderWithMetadata = &MskProvider{}

// MskProvider defines the provider implementation.
type MskProvider struct {
	// version is set to the provider version on release, "dev" when the
	// provider is built and ran locally, and "test" when running acceptance
	// testing.
	version string
}

// MskProviderModel describes the provider data model.
type MskProviderModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
}

func (p *MskProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "msk"
	resp.Version = p.version
}

func (p *MskProvider) GetSchema(ctx context.Context) (tfsdk.Schema, diag.Diagnostics) {
	return tfsdk.Schema{
		Attributes: map[string]tfsdk.Attribute{
			"endpoint": {
				MarkdownDescription: "Example provider attribute",
				Optional:            true,
				Type:                types.StringType,
			},
		},
	}, nil
	// return tfsdk.Schema{
	// 	Attributes: map[string]tfsdk.Attribute{
	// 		"bootstrap_servers": {
	// 			MarkdownDescription: "A list of kafka brokers",
	// 			// Required:            true,
	// 			Optional: true,
	// 			Type: types.ListType{
	// 				ElemType: types.StringType,
	// 			},
	// 		},
	// 		"tls_enabled": {
	// 			MarkdownDescription: "A list of kafka brokers",
	// 			Optional:            true,
	// 			Type:                types.BoolType,
	// 		},
	// 	},
	// }, nil
}

func (p *MskProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data MskProviderModel

	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	// Configuration values are now available.
	// if data.Endpoint.IsNull() { /* ... */ }

	// Example client configuration for data sources and resources
	client := http.DefaultClient
	resp.DataSourceData = client
	resp.ResourceData = client
}

func (p *MskProvider) Resources(ctx context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewTopicResource,
	}
}

func (p *MskProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewExampleDataSource,
	}
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &MskProvider{
			version: version,
		}
	}
}

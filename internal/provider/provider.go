package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/segmentio/topicctl/pkg/admin"
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
	BootstrapServers []types.String `tfsdk:"bootstrap_servers"`
	TLSEnabled       types.Bool     `tfsdk:"tls_enabled"`
}

func (p *MskProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "msk"
	resp.Version = p.version
}

func (p *MskProvider) GetSchema(ctx context.Context) (tfsdk.Schema, diag.Diagnostics) {
	return tfsdk.Schema{
		Attributes: map[string]tfsdk.Attribute{
			"bootstrap_servers": {
				MarkdownDescription: "A list of kafka brokers",
				Required:            true,
				Type: types.ListType{
					ElemType: types.StringType,
				},
			},
			"tls_enabled": {
				MarkdownDescription: "A list of kafka brokers",
				Optional:            true,
				Type:                types.BoolType,
			},
		},
	}, nil
}

func (p *MskProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config MskProviderModel

	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)

	if resp.Diagnostics.HasError() {
		return
	}

	brokerConfig := admin.BrokerAdminClientConfig{
		ConnectorConfig: admin.ConnectorConfig{
			BrokerAddr: config.BootstrapServers[0].Value,
			TLS: admin.TLSConfig{
				Enabled: config.TLSEnabled.Value,
			},
			SASL: admin.SASLConfig{
				Enabled:   true,
				Mechanism: admin.SASLMechanismAWSMSKIAM,
			},
		},
		ReadOnly: false,
	}
	client, err := admin.NewBrokerAdminClient(
		ctx,
		brokerConfig,
	)
	if err != nil {
		resp.Diagnostics.AddError("Unable to create MSK client",
			"An unexpected error occurred when creating the Kafka client"+
				"Kafka Error:"+err.Error())
	}

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
		NewTopicDataSource,
	}
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &MskProvider{
			version: version,
		}
	}
}

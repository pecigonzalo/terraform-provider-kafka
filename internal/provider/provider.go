package provider

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/segmentio/topicctl/pkg/admin"
)

// Ensure KafkaProvider satisfies various provider interfaces.
var _ provider.Provider = &KafkaProvider{}
var _ provider.ProviderWithMetadata = &KafkaProvider{}

// KafkaProvider defines the provider implementation.
type KafkaProvider struct {
	typeName string
	// version is set to the provider version on release, "dev" when the
	// provider is built and ran locally, and "test" when running acceptance
	// testing.
	version string
}

// KafkaProviderModel describes the provider data model.
type KafkaProviderModel struct {
	BootstrapServers []types.String  `tfsdk:"bootstrap_servers"`
	SASL             SASLConfigModel `tfsdk:"sasl"`
	TLS              TLSConfigModel  `tfsdk:"tls"`
	Timeout          types.Int64     `tfsdk:"timeout"`
}

// SASLConfigModel describes a SASL Authentication configuration
type SASLConfigModel struct {
	Enabled   types.Bool   `tfsdk:"enabled"`
	Mechanism types.String `tfsdk:"mechanism"`
	Username  types.String `tfsdk:"username"`
	Password  types.String `tfsdk:"password"`
}

// TLSConfigModel describes a SASL Authentication configuration
type TLSConfigModel struct {
	Enabled    types.Bool `tfsdk:"enabled"`
	SkipVerify types.Bool `tfsdk:"skip_verify"`
}

func (p *KafkaProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = p.typeName
	resp.Version = p.version
}

func (p *KafkaProvider) GetSchema(ctx context.Context) (tfsdk.Schema, diag.Diagnostics) {
	return tfsdk.Schema{
		Attributes: map[string]tfsdk.Attribute{
			"bootstrap_servers": {
				MarkdownDescription: "A list of Kafka brokers",
				Required:            true,
				Type: types.ListType{
					ElemType: types.StringType,
				},
			},
			"tls": {
				MarkdownDescription: "TLS Configuration",
				Optional:            true,
				Computed:            true,
				Attributes: tfsdk.SingleNestedAttributes(map[string]tfsdk.Attribute{
					"enabled": {
						MarkdownDescription: "Enable TLS communication with Kafka brokers (default: true)",
						Optional:            true,
						Computed:            true,
						Type:                types.BoolType,
					},
					"skip_verify": {
						MarkdownDescription: "Skips TLS verification when connecting to the brokers (default: false)",
						Optional:            true,
						Computed:            true,
						Type:                types.BoolType,
					},
				}),
			},
			"sasl": {
				MarkdownDescription: "SASL Authentication",
				Optional:            true,
				Computed:            true,
				Attributes: tfsdk.SingleNestedAttributes(map[string]tfsdk.Attribute{
					"enabled": {
						MarkdownDescription: "Enable SASL Authentication",
						Optional:            true,
						Computed:            true,
						Type:                types.BoolType,
					},
					"mechanism": {
						MarkdownDescription: "SASL mechanism to use. One of plain, scram-sha512, scram-sha256, aws-msk-iam (default: aws-msk-iam)",
						Optional:            true,
						Computed:            true,
						Type:                types.StringType,
					},
					"username": {
						MarkdownDescription: "Username for SASL authentication",
						Optional:            true,
						Type:                types.StringType,
						Sensitive:           true,
					},
					"password": {
						MarkdownDescription: "Password for SASL authentication",
						Optional:            true,
						Type:                types.StringType,
						Sensitive:           true,
					},
				}),
			},
			"timeout": {
				MarkdownDescription: "Timeout for provider operations (default: 300)",
				Optional:            true,
				Computed:            true,
				Type:                types.Int64Type,
			},
		},
	}, nil
}

func (p *KafkaProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	// Populate config
	var config KafkaProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var brokerConfig admin.BrokerAdminClientConfig

	// Configure TLS settings
	brokerConfig.TLS.Enabled = config.TLS.Enabled.Value
	brokerConfig.TLS.SkipVerify = config.TLS.SkipVerify.Value

	// Configure SASL if enabled
	if config.SASL.Enabled.Value {
		saslConfig, err := p.generateSASLConfig(ctx, config.SASL, resp)
		if err != nil {
			resp.Diagnostics.AddError("Unable to create Kafka client", err.Error())
			return
		}
		brokerConfig.SASL = saslConfig
	}

	brokerConfig.ReadOnly = true
	dataSourceClient, err := admin.NewBrokerAdminClient(
		ctx,
		brokerConfig,
	)
	if err != nil {
		resp.Diagnostics.AddError("Unable to create Kafka client",
			"An unexpected error occurred when creating the Kafka client "+
				"Kafka Error: "+err.Error())
		return
	}
	resp.DataSourceData = dataSourceClient

	brokerConfig.ReadOnly = false
	resourceClient, err := admin.NewBrokerAdminClient(
		ctx,
		brokerConfig,
	)
	if err != nil {
		resp.Diagnostics.AddError("Unable to create Kafka client",
			"An unexpected error occurred when creating the Kafka client "+
				"Kafka Error: "+err.Error())
		return
	}
	resp.ResourceData = resourceClient
}

// generateSASLConfig returns a SASLConfig{} or an error given a SASLModel
func (p *KafkaProvider) generateSASLConfig(ctx context.Context, sasl SASLConfigModel, resp *provider.ConfigureResponse) (admin.SASLConfig, error) {
	saslMechanism := p.getEnv("SASL_MECHANISM", "aws-msk-iam")
	if !sasl.Mechanism.IsNull() {
		saslMechanism = sasl.Mechanism.Value
	}

	switch admin.SASLMechanism(saslMechanism) {
	case admin.SASLMechanismScramSHA512:
	case admin.SASLMechanismScramSHA256:
	case admin.SASLMechanismPlain:
		return admin.SASLConfig{
			Enabled:   sasl.Enabled.Value,
			Mechanism: admin.SASLMechanismScramSHA256,
			Username:  sasl.Username.Value,
			Password:  sasl.Password.Value,
		}, nil
	case admin.SASLMechanismAWSMSKIAM:
		return admin.SASLConfig{
			Enabled:   sasl.Enabled.Value,
			Mechanism: admin.SASLMechanismAWSMSKIAM,
			Username:  sasl.Username.Value,
			Password:  sasl.Password.Value,
		}, nil
	}
	return admin.SASLConfig{}, fmt.Errorf("unable to detect SASL mechanism: %s", sasl.Mechanism.Value)
}

func (p *KafkaProvider) Resources(ctx context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewTopicResource,
	}
}

func (p *KafkaProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewTopicDataSource,
	}
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &KafkaProvider{
			typeName: "kafka",
			version:  version,
		}
	}
}

func (p *KafkaProvider) getEnv(key, fallback string) string {
	envVarPrefix := fmt.Sprintf("%s_", strings.ToUpper(p.typeName))
	if value, ok := os.LookupEnv(envVarPrefix + key); ok {
		return value
	}
	return fallback
}

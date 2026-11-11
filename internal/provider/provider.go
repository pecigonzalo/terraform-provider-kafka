package provider

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/segmentio/topicctl/pkg/admin"
)

// Ensure KafkaProvider satisfies various provider interfaces.
var _ provider.Provider = &kafkaProvider{}

// kafkaProvider defines the provider implementation.
type kafkaProvider struct {
	typeName string
	// version is set to the provider version on release, "dev" when the
	// provider is built and ran locally, and "test" when running acceptance
	// testing.
	version string
}

// kafkaProviderModel describes the provider data model.
type kafkaProviderModel struct {
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

// TLSConfigModel describes a TLS configuration
type TLSConfigModel struct {
	Enabled    types.Bool `tfsdk:"enabled"`
	SkipVerify types.Bool `tfsdk:"skip_verify"`
}

func (p *kafkaProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = p.typeName
	resp.Version = p.version
}

func (p *kafkaProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Attributes: map[string]schema.Attribute{
			"bootstrap_servers": schema.ListAttribute{
				MarkdownDescription: "A list of Kafka brokers. Only the first entry is used to bootstrap the " +
					"connection. Can also be set with the `KAFKA_BOOTSTRAP_SERVERS` environment variable (comma separated).",
				Optional:    true,
				ElementType: types.StringType,
			},
			"tls": schema.SingleNestedAttribute{
				MarkdownDescription: "TLS Configuration",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"enabled": schema.BoolAttribute{
						MarkdownDescription: "Enable TLS communication with Kafka brokers (default: true)",
						Optional:            true,
					},
					"skip_verify": schema.BoolAttribute{
						MarkdownDescription: "Skips TLS verification when connecting to the brokers (default: false)",
						Optional:            true,
					},
				},
			},
			"sasl": schema.SingleNestedAttribute{
				MarkdownDescription: "SASL Authentication",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"enabled": schema.BoolAttribute{
						MarkdownDescription: "Enable SASL Authentication",
						Optional:            true,
					},
					"mechanism": schema.StringAttribute{
						MarkdownDescription: "SASL mechanism to use. One of plain, scram-sha256, scram-sha512, aws-msk-iam (default: aws-msk-iam)",
						Optional:            true,
					},
					"username": schema.StringAttribute{
						MarkdownDescription: "Username for SASL authentication",
						Optional:            true,
						Sensitive:           true,
					},
					"password": schema.StringAttribute{
						MarkdownDescription: "Password for SASL authentication",
						Optional:            true,
						Sensitive:           true,
					},
				},
			},
			"timeout": schema.Int64Attribute{
				MarkdownDescription: "Timeout for provider operations in seconds (default: 300)",
				Optional:            true,
			},
		},
	}
}

func (p *kafkaProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	// Populate config
	var config kafkaProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	envVarPrefix := strings.ToUpper(p.typeName)

	for _, server := range config.BootstrapServers {
		if server.IsUnknown() {
			resp.Diagnostics.AddAttributeError(
				path.Root("bootstrap_servers"),
				"Unknown Kafka bootstrap servers",
				"The provider cannot create the Kafka client as there is an unknown configuration value. "+
					fmt.Sprintf("Either target apply the source of the value first, set the value statically in the configuration, or use the %s_BOOTSTRAP_SERVERS environment variable.", envVarPrefix),
			)
			break
		}
	}

	if config.SASL.Username.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("sasl.username"),
			"Unknown Kafka SASL username",
			"The provider cannot create the Kafka client as there is an unknown configuration value for the SASL username. "+
				fmt.Sprintf("Either target apply the source of the value first, set the value statically in the configuration, or use the %s_SASL_USERNAME environment variable.", envVarPrefix),
		)
	}
	if config.SASL.Password.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("sasl.password"),
			"Unknown Kafka SASL password",
			"The provider cannot create the Kafka client as there is an unknown configuration value for the SASL password. "+
				fmt.Sprintf("Either target apply the source of the value first, set the value statically in the configuration, or use the %s_SASL_PASSWORD environment variable.", envVarPrefix),
		)
	}

	if resp.Diagnostics.HasError() {
		return
	}

	var brokerConfig admin.BrokerAdminClientConfig

	// Bootstrap servers
	bootstrapServer, err := p.bootstrapServer(config.BootstrapServers)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("bootstrap_servers"), "Missing Kafka bootstrap servers", err.Error())
		return
	}
	brokerConfig.BrokerAddr = bootstrapServer

	// SASL configuration
	saslConfigEnabled := p.getEnvBool("SASL_ENABLED", true)
	if !config.SASL.Enabled.IsNull() {
		saslConfigEnabled = config.SASL.Enabled.ValueBool()
	}
	if saslConfigEnabled {
		saslConfig, err := p.generateSASLConfig(config.SASL)
		if err != nil {
			resp.Diagnostics.AddError("Unable to create Kafka client", err.Error())
			return
		}
		brokerConfig.SASL = saslConfig
	}

	// Configure TLS settings
	// TLS is enabled unless explicitly disabled, as documented in the schema
	brokerConfig.TLS.Enabled = true
	if !config.TLS.Enabled.IsNull() {
		brokerConfig.TLS.Enabled = config.TLS.Enabled.ValueBool()
	}
	brokerConfig.TLS.SkipVerify = config.TLS.SkipVerify.ValueBool()

	// Configure timeout
	defaultTimeout := int64(p.getEnvInt("TIMEOUT", 300))
	if !config.Timeout.IsNull() {
		defaultTimeout = config.Timeout.ValueInt64()
	}
	kafkaClientTimeout := time.Second * time.Duration(defaultTimeout)

	tflog.Debug(ctx, "Creating Kafka client")
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
	dataSourceClient.GetConnector().KafkaClient.Timeout = time.Duration(kafkaClientTimeout)
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
	resourceClient.GetConnector().KafkaClient.Timeout = time.Duration(kafkaClientTimeout)
	resp.ResourceData = resourceClient
	tflog.Info(ctx, "Configured Kafka client", map[string]any{"success": true})
}

// bootstrapServer returns the broker address used to bootstrap the client.
// Configured values take precedence over the environment. Only one server is
// required, so the first entry is used.
func (p *kafkaProvider) bootstrapServer(configured []types.String) (string, error) {
	if len(configured) > 0 {
		if server := configured[0].ValueString(); server != "" {
			return server, nil
		}
	}
	for _, server := range strings.Split(p.getEnv("BOOTSTRAP_SERVERS", ""), ",") {
		if server = strings.TrimSpace(server); server != "" {
			return server, nil
		}
	}
	return "", fmt.Errorf("set bootstrap_servers or the %s_BOOTSTRAP_SERVERS environment variable", strings.ToUpper(p.typeName))
}

// generateSASLConfig returns a SASLConfig{} or an error given a SASLModel.
// Values set in the configuration take precedence over environment variables.
func (p *kafkaProvider) generateSASLConfig(sasl SASLConfigModel) (admin.SASLConfig, error) {
	mechanism := p.getEnv("SASL_MECHANISM", "aws-msk-iam")
	if !sasl.Mechanism.IsNull() {
		mechanism = sasl.Mechanism.ValueString()
	}
	username := p.getEnv("SASL_USERNAME", "")
	if !sasl.Username.IsNull() {
		username = sasl.Username.ValueString()
	}
	password := p.getEnv("SASL_PASSWORD", "")
	if !sasl.Password.IsNull() {
		password = sasl.Password.ValueString()
	}

	// Accept both "scram-sha256" and "scram-sha-256" style names.
	normalized := strings.ToLower(strings.TrimSpace(mechanism))
	normalized = strings.ReplaceAll(normalized, "scram-sha-", "scram-sha")

	var saslMechanism admin.SASLMechanism
	switch normalized {
	case "plain":
		saslMechanism = admin.SASLMechanismPlain
	case "scram-sha256":
		saslMechanism = admin.SASLMechanismScramSHA256
	case "scram-sha512":
		saslMechanism = admin.SASLMechanismScramSHA512
	case "aws-msk-iam":
		return admin.SASLConfig{
			Enabled:   true,
			Mechanism: admin.SASLMechanismAWSMSKIAM,
		}, nil
	default:
		return admin.SASLConfig{}, fmt.Errorf("unable to detect SASL mechanism: %q", mechanism)
	}

	return admin.SASLConfig{
		Enabled:   true,
		Mechanism: saslMechanism,
		Username:  username,
		Password:  password,
	}, nil
}

func (p *kafkaProvider) Resources(ctx context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewTopicResource,
	}
}

func (p *kafkaProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewTopicDataSource,
	}
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &kafkaProvider{
			typeName: "kafka",
			version:  version,
		}
	}
}

func (p *kafkaProvider) getEnv(key, fallback string) string {
	envVarPrefix := fmt.Sprintf("%s_", strings.ToUpper(p.typeName))
	if value, ok := os.LookupEnv(envVarPrefix + key); ok {
		return value
	}
	return fallback
}

func (p *kafkaProvider) getEnvInt(key string, fallback int) int {
	envVar := p.getEnv(key, "")
	if envVar == "" {
		return fallback
	}

	result, err := strconv.Atoi(envVar)
	if err != nil {
		return fallback
	}
	return result
}

func (p *kafkaProvider) getEnvBool(key string, fallback bool) bool {
	envVar := p.getEnv(key, "")
	if envVar == "" {
		return fallback
	}

	result, err := strconv.ParseBool(envVar)
	if err != nil {
		return fallback
	}
	return result
}

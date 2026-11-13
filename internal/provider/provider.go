package provider

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
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
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
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

	var clientConfig kafkaclient.Config

	// Bootstrap servers
	bootstrapServer, err := p.bootstrapServer(config.BootstrapServers)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("bootstrap_servers"), "Missing Kafka bootstrap servers", err.Error())
		return
	}
	clientConfig.BootstrapServer = bootstrapServer

	// SASL configuration
	saslConfigEnabled, err := p.getEnvBool("SASL_ENABLED", true)
	if err != nil {
		resp.Diagnostics.AddError("Invalid Kafka provider environment variable", err.Error())
		return
	}
	if !config.SASL.Enabled.IsNull() {
		saslConfigEnabled = config.SASL.Enabled.ValueBool()
	}
	if saslConfigEnabled {
		saslConfig, err := p.generateSASLConfig(config.SASL)
		if err != nil {
			resp.Diagnostics.AddError("Unable to create Kafka client", err.Error())
			return
		}
		clientConfig.SASL = saslConfig
	}

	// Configure TLS settings
	// TLS is enabled unless explicitly disabled, as documented in the schema
	clientConfig.TLS.Enabled = true
	if !config.TLS.Enabled.IsNull() {
		clientConfig.TLS.Enabled = config.TLS.Enabled.ValueBool()
	}
	clientConfig.TLS.SkipVerify = config.TLS.SkipVerify.ValueBool()

	// Configure timeout
	envTimeout, err := p.getEnvInt("TIMEOUT", 300)
	if err != nil {
		resp.Diagnostics.AddError("Invalid Kafka provider environment variable", err.Error())
		return
	}
	defaultTimeout := int64(envTimeout)
	if !config.Timeout.IsNull() {
		defaultTimeout = config.Timeout.ValueInt64()
	}
	clientConfig.Timeout = time.Second * time.Duration(defaultTimeout)

	tflog.Debug(ctx, "Creating Kafka client")

	// One read-write kafkaclient client serves the whole provider: data
	// sources only read, so they get its read-only view, and resources get the
	// client itself. The client does not contact the broker until it is first
	// used.
	//
	// The framework has no hook that runs when the provider is torn down, so
	// the client is never closed explicitly. It holds only idle broker
	// connections and the transport's background metadata refresh, both of
	// which end with the provider process that Terraform starts per run.
	kafkaClient, err := kafkaclient.New(ctx, clientConfig)
	if err != nil {
		resp.Diagnostics.AddError("Unable to create Kafka client",
			"An unexpected error occurred when creating the Kafka client "+
				"Kafka Error: "+err.Error())
		return
	}
	resp.DataSourceData = kafkaClient.ReadOnly()
	resp.ResourceData = kafkaClient
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
	for server := range strings.SplitSeq(p.getEnv("BOOTSTRAP_SERVERS", ""), ",") {
		if server = strings.TrimSpace(server); server != "" {
			return server, nil
		}
	}
	return "", fmt.Errorf("set bootstrap_servers or the %s_BOOTSTRAP_SERVERS environment variable", strings.ToUpper(p.typeName))
}

// generateSASLConfig returns the SASL settings for the client, or an error
// given a SASLModel. Values set in the configuration take precedence over
// environment variables. The mechanism is normalised, and the AWS MSK IAM
// mechanism carries no username or password because it signs with AWS
// credentials.
func (p *kafkaProvider) generateSASLConfig(sasl SASLConfigModel) (*kafkaclient.SASLConfig, error) {
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

	parsed, err := kafkaclient.ParseSASLMechanism(mechanism)
	if err != nil {
		return nil, err
	}
	if parsed == kafkaclient.SASLMechanismAWSMSKIAM {
		return &kafkaclient.SASLConfig{Mechanism: string(parsed)}, nil
	}
	return &kafkaclient.SASLConfig{
		Mechanism: string(parsed),
		Username:  username,
		Password:  password,
	}, nil
}

func (p *kafkaProvider) Resources(ctx context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewTopicResource,
		NewACLResource,
		NewQuotaResource,
	}
}

func (p *kafkaProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewTopicDataSource,
		NewACLsDataSource,
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

// envVarName returns the full environment variable name for key.
func (p *kafkaProvider) envVarName(key string) string {
	return fmt.Sprintf("%s_%s", strings.ToUpper(p.typeName), key)
}

func (p *kafkaProvider) getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(p.envVarName(key)); ok {
		return value
	}
	return fallback
}

// getEnvInt returns the integer value of an environment variable. An unset or
// empty variable yields fallback. A non-empty value that is not an integer
// yields an error naming the variable.
func (p *kafkaProvider) getEnvInt(key string, fallback int) (int, error) {
	envVar := p.getEnv(key, "")
	if envVar == "" {
		return fallback, nil
	}

	result, err := strconv.Atoi(envVar)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", p.envVarName(key), envVar)
	}
	return result, nil
}

// getEnvBool returns the boolean value of an environment variable. An unset or
// empty variable yields fallback. A non-empty value that is not a boolean
// yields an error naming the variable.
func (p *kafkaProvider) getEnvBool(key string, fallback bool) (bool, error) {
	envVar := p.getEnv(key, "")
	if envVar == "" {
		return fallback, nil
	}

	result, err := strconv.ParseBool(envVar)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean (true/false), got %q", p.envVarName(key), envVar)
	}
	return result, nil
}

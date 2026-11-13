package provider

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
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
					"connection. If you do not set it, the provider uses the `KAFKA_BOOTSTRAP_SERVERS` environment " +
					"variable (comma separated).",
				Optional:    true,
				ElementType: types.StringType,
			},
			"tls": schema.SingleNestedAttribute{
				MarkdownDescription: "TLS configuration. TLS is on by default.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"enabled": schema.BoolAttribute{
						MarkdownDescription: "Enable TLS communication with Kafka brokers (default: `true`). If you do not set it, " +
							"the provider uses the `KAFKA_TLS_ENABLED` environment variable. Turn TLS off only for local " +
							"development. Without TLS, all traffic goes over the network in clear text.",
						Optional: true,
					},
					"skip_verify": schema.BoolAttribute{
						MarkdownDescription: "Turn off the checks of the broker certificate and of the host name (default: `false`). " +
							"If you do not set it, the provider uses the `KAFKA_TLS_SKIP_VERIFY` environment variable. " +
							"If you turn the checks off, an attacker on the network can read or change the traffic. " +
							"The attacker can also see the SASL credentials. " +
							"If your brokers use a certificate from a private certificate authority, do not use this option. " +
							"Install the certificate authority in the system certificate store instead. " +
							"On Linux and other Unix systems (but not macOS), you can also set the `SSL_CERT_FILE` or " +
							"`SSL_CERT_DIR` environment variable. These variables have no effect on macOS and Windows.",
						Optional: true,
					},
				},
			},
			"sasl": schema.SingleNestedAttribute{
				MarkdownDescription: "SASL authentication. SASL is on by default. You must set `mechanism`. " +
					"To turn SASL off, set `sasl = { enabled = false }`.",
				Optional: true,
				Attributes: map[string]schema.Attribute{
					"enabled": schema.BoolAttribute{
						MarkdownDescription: "Enable SASL authentication (default: `true`). If you do not set it, the provider uses " +
							"the `KAFKA_SASL_ENABLED` environment variable.",
						Optional: true,
					},
					"mechanism": schema.StringAttribute{
						MarkdownDescription: "SASL mechanism. One of `plain`, `scram-sha256`, `scram-sha512` or `aws-msk-iam`. " +
							"The names `scram-sha-256` and `scram-sha-512` also work. There is no default. " +
							"If SASL is on and you do not set it, the provider uses the `KAFKA_SASL_MECHANISM` " +
							"environment variable. If that is also not set, the provider gives an error. " +
							"With `plain`, the password goes over the network in clear text unless TLS is on.",
						Optional: true,
					},
					"username": schema.StringAttribute{
						MarkdownDescription: "User name for SASL authentication. Necessary for `plain`, `scram-sha256` and " +
							"`scram-sha512`. If you do not set it, the provider uses the `KAFKA_SASL_USERNAME` " +
							"environment variable. Do not set it with `aws-msk-iam`: the provider gives an error.",
						Optional:  true,
						Sensitive: true,
					},
					"password": schema.StringAttribute{
						MarkdownDescription: "Password for SASL authentication. Necessary for `plain`, `scram-sha256` and " +
							"`scram-sha512`. If you do not set it, the provider uses the `KAFKA_SASL_PASSWORD` " +
							"environment variable. Do not set it with `aws-msk-iam`: the provider gives an error.",
						Optional:  true,
						Sensitive: true,
					},
				},
			},
			"timeout": schema.Int64Attribute{
				MarkdownDescription: "Timeout for provider operations in seconds (default: 300). It must be 1 or more. " +
					"If you do not set it, the provider uses the `KAFKA_TIMEOUT` environment variable. " +
					"The time to open one connection to a broker is the smaller of this value and 30 seconds.",
				Optional:   true,
				Validators: []validator.Int64{int64validator.AtLeast(1)},
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
			path.Root("sasl").AtName("username"),
			"Unknown Kafka SASL username",
			"The provider cannot create the Kafka client as there is an unknown configuration value for the SASL username. "+
				fmt.Sprintf("Either target apply the source of the value first, set the value statically in the configuration, or use the %s_SASL_USERNAME environment variable.", envVarPrefix),
		)
	}
	if config.SASL.Password.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("sasl").AtName("password"),
			"Unknown Kafka SASL password",
			"The provider cannot create the Kafka client as there is an unknown configuration value for the SASL password. "+
				fmt.Sprintf("Either target apply the source of the value first, set the value statically in the configuration, or use the %s_SASL_PASSWORD environment variable.", envVarPrefix),
		)
	}

	// A value that the chosen setup does not use may stay unknown.
	saslOff := !config.SASL.Enabled.IsUnknown() && !config.SASL.Enabled.IsNull() && !config.SASL.Enabled.ValueBool()
	tlsOff := !config.TLS.Enabled.IsUnknown() && !config.TLS.Enabled.IsNull() && !config.TLS.Enabled.ValueBool()
	for _, setting := range []struct {
		path    path.Path
		name    string
		unknown bool
	}{
		{path.Root("tls").AtName("enabled"), "tls.enabled", config.TLS.Enabled.IsUnknown()},
		{path.Root("tls").AtName("skip_verify"), "tls.skip_verify", !tlsOff && config.TLS.SkipVerify.IsUnknown()},
		{path.Root("sasl").AtName("enabled"), "sasl.enabled", config.SASL.Enabled.IsUnknown()},
		{path.Root("sasl").AtName("mechanism"), "sasl.mechanism", !saslOff && config.SASL.Mechanism.IsUnknown()},
		{path.Root("timeout"), "timeout", config.Timeout.IsUnknown()},
	} {
		if setting.unknown {
			resp.Diagnostics.AddAttributeError(
				setting.path,
				"Unknown Kafka provider setting",
				fmt.Sprintf("The provider cannot create the Kafka client because the value of %s is unknown. ", setting.name)+
					"Either target apply the source of the value first, or set the value statically in the configuration.",
			)
		}
	}

	if resp.Diagnostics.HasError() {
		return
	}

	clientConfig := p.buildClientConfig(config, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

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

// maxTimeoutSeconds is the largest timeout that fits in a time.Duration.
const maxTimeoutSeconds = int64(math.MaxInt64 / int64(time.Second))

// buildClientConfig resolves the client settings from the configuration and
// the environment. A value in the configuration takes precedence over the
// environment. The method adds errors and warnings to diags. The result is
// not usable if diags has an error.
func (p *kafkaProvider) buildClientConfig(config kafkaProviderModel, diags *diag.Diagnostics) kafkaclient.Config {
	var clientConfig kafkaclient.Config

	bootstrapServer, err := p.bootstrapServer(config.BootstrapServers)
	if err != nil {
		diags.AddAttributeError(path.Root("bootstrap_servers"), "Missing Kafka bootstrap servers", err.Error())
		return clientConfig
	}
	clientConfig.BootstrapServer = bootstrapServer

	// TLS is on unless you turn it off.
	tlsEnabled, err := p.resolveBool(config.TLS.Enabled, "TLS_ENABLED", true)
	if err != nil {
		diags.AddError("Invalid Kafka provider environment variable", err.Error())
		return clientConfig
	}
	tlsSkipVerify, err := p.resolveBool(config.TLS.SkipVerify, "TLS_SKIP_VERIFY", false)
	if err != nil {
		diags.AddError("Invalid Kafka provider environment variable", err.Error())
		return clientConfig
	}
	clientConfig.TLS.Enabled = tlsEnabled
	clientConfig.TLS.SkipVerify = tlsSkipVerify

	// SASL is on unless you turn it off.
	saslEnabled, err := p.resolveBool(config.SASL.Enabled, "SASL_ENABLED", true)
	if err != nil {
		diags.AddError("Invalid Kafka provider environment variable", err.Error())
		return clientConfig
	}
	if saslEnabled {
		saslConfig, err := p.generateSASLConfig(config.SASL)
		if err != nil {
			diags.AddError("Unable to create Kafka client", err.Error())
			return clientConfig
		}
		clientConfig.SASL = saslConfig
		if saslConfig.Mechanism == string(kafkaclient.SASLMechanismPlain) && !tlsEnabled {
			diags.AddWarning(
				"SASL PLAIN without TLS",
				"The SASL mechanism is plain and TLS is off. The provider sends the SASL user name and "+
					"password over the network in clear text. Anyone on the network can read them. "+
					"Turn TLS on, or use this setup only for local development.",
			)
		}
	}

	// The provider timeout is the request timeout. The dial timeout is shorter.
	timeoutSeconds, err := p.getEnvInt("TIMEOUT", 300)
	if err != nil {
		diags.AddError("Invalid Kafka provider environment variable", err.Error())
		return clientConfig
	}
	if timeoutSeconds < 1 {
		diags.AddError("Invalid Kafka provider environment variable",
			fmt.Sprintf("%s must be 1 or more, got %d", p.envVarName("TIMEOUT"), timeoutSeconds))
		return clientConfig
	}
	timeout := int64(timeoutSeconds)
	if !config.Timeout.IsNull() {
		timeout = config.Timeout.ValueInt64()
	}
	if timeout > maxTimeoutSeconds {
		diags.AddError("Invalid Kafka provider timeout",
			fmt.Sprintf("The timeout is too large. Use %d seconds or less.", maxTimeoutSeconds))
		return clientConfig
	}
	clientConfig.Timeout = time.Second * time.Duration(timeout)
	clientConfig.DialTimeout = kafkaclient.DialTimeoutFor(clientConfig.Timeout)

	return clientConfig
}

// resolveBool returns the configured value if it is set. Otherwise it returns
// the value of the environment variable, or fallback if that is also not set.
// An invalid environment value is an error even when the configuration is set.
func (p *kafkaProvider) resolveBool(configured types.Bool, envKey string, fallback bool) (bool, error) {
	fromEnv, err := p.getEnvBool(envKey, fallback)
	if err != nil {
		return false, err
	}
	if !configured.IsNull() {
		return configured.ValueBool(), nil
	}
	return fromEnv, nil
}

// generateSASLConfig returns the SASL settings for the client, or an error.
// Values set in the configuration take precedence over environment variables.
// The mechanism has no default. The AWS MSK IAM mechanism signs with AWS
// credentials, so it does not accept a user name or a password. The other
// mechanisms need both. Error messages never contain a credential value.
func (p *kafkaProvider) generateSASLConfig(sasl SASLConfigModel) (*kafkaclient.SASLConfig, error) {
	mechanism := p.getEnv("SASL_MECHANISM", "")
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

	if strings.TrimSpace(mechanism) == "" {
		return nil, fmt.Errorf("SASL is on, but no SASL mechanism is set. "+
			"Set sasl.mechanism or the %s environment variable to one of: %s. "+
			"The names scram-sha-256 and scram-sha-512 also work. "+
			"To turn SASL off, set sasl = { enabled = false } or %s=false",
			p.envVarName("SASL_MECHANISM"), strings.Join(kafkaclient.SASLMechanismNames(), ", "),
			p.envVarName("SASL_ENABLED"))
	}
	parsed, err := kafkaclient.ParseSASLMechanism(mechanism)
	if err != nil {
		return nil, fmt.Errorf("%w. Use one of: %s", err, strings.Join(kafkaclient.SASLMechanismNames(), ", "))
	}

	if parsed == kafkaclient.SASLMechanismAWSMSKIAM {
		var unused []string
		if strings.TrimSpace(username) != "" {
			unused = append(unused, "a SASL user name (sasl.username or "+p.envVarName("SASL_USERNAME")+")")
		}
		if strings.TrimSpace(password) != "" {
			unused = append(unused, "a SASL password (sasl.password or "+p.envVarName("SASL_PASSWORD")+")")
		}
		if len(unused) > 0 {
			return nil, fmt.Errorf("the %s mechanism does not use a user name or a password, but you set %s. "+
				"The provider signs requests with your AWS credentials. Remove the unused value",
				parsed, strings.Join(unused, " and "))
		}
		return &kafkaclient.SASLConfig{Mechanism: string(parsed)}, nil
	}

	var missing []string
	if strings.TrimSpace(username) == "" {
		missing = append(missing, "user name (sasl.username or "+p.envVarName("SASL_USERNAME")+")")
	}
	if strings.TrimSpace(password) == "" {
		missing = append(missing, "password (sasl.password or "+p.envVarName("SASL_PASSWORD")+")")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the %s mechanism needs a user name and a password. Empty: the SASL %s",
			parsed, strings.Join(missing, " and the SASL "))
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

// getEnv returns the value of an environment variable. An unset or empty
// variable yields fallback.
func (p *kafkaProvider) getEnv(key, fallback string) string {
	if value := os.Getenv(p.envVarName(key)); value != "" {
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

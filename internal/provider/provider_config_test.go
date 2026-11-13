package provider

import (
	"maps"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

func TestGenerateSASLConfig(t *testing.T) {
	p := &kafkaProvider{typeName: "kafka"}
	null := SASLConfigModel{
		Mechanism: types.StringNull(),
		Username:  types.StringNull(),
		Password:  types.StringNull(),
	}

	t.Run("no_mechanism_is_an_error_that_lists_the_mechanisms", func(t *testing.T) {
		cfg, err := p.generateSASLConfig(null)
		require.Error(t, err)
		assert.Nil(t, cfg)
		for _, want := range []string{
			"plain", "scram-sha256", "scram-sha512", "aws-msk-iam", "scram-sha-256", "scram-sha-512",
			"sasl = { enabled = false }", "KAFKA_SASL_MECHANISM",
		} {
			assert.Contains(t, err.Error(), want)
		}
	})

	for name, want := range map[string]kafkaclient.SASLMechanism{
		"plain":         kafkaclient.SASLMechanismPlain,
		"scram-sha256":  kafkaclient.SASLMechanismScramSHA256,
		"scram-sha-256": kafkaclient.SASLMechanismScramSHA256,
		"scram-sha512":  kafkaclient.SASLMechanismScramSHA512,
		"scram-sha-512": kafkaclient.SASLMechanismScramSHA512,
	} {
		t.Run(name, func(t *testing.T) {
			m := null
			m.Mechanism = types.StringValue(name)
			m.Username = types.StringValue("user")
			m.Password = types.StringValue("pass")
			cfg, err := p.generateSASLConfig(m)
			require.NoError(t, err)
			assert.Equal(t, string(want), cfg.Mechanism)
			assert.Equal(t, "user", cfg.Username)
			assert.Equal(t, "pass", cfg.Password)
		})
	}

	t.Run("unknown mechanism errors", func(t *testing.T) {
		m := null
		m.Mechanism = types.StringValue("bogus")
		_, err := p.generateSASLConfig(m)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unable to detect SASL mechanism")
	})

	t.Run("aws-msk-iam without credentials has none", func(t *testing.T) {
		m := null
		m.Mechanism = types.StringValue("aws-msk-iam")
		cfg, err := p.generateSASLConfig(m)
		require.NoError(t, err)
		assert.Empty(t, cfg.Username)
		assert.Empty(t, cfg.Password)
	})

	t.Run("every generated mechanism passes client validation", func(t *testing.T) {
		for _, name := range []string{"plain", "scram-sha256", "scram-sha-512", "aws-msk-iam"} {
			m := null
			m.Mechanism = types.StringValue(name)
			if name != "aws-msk-iam" {
				m.Username = types.StringValue("user")
				m.Password = types.StringValue("pass")
			}
			sasl, err := p.generateSASLConfig(m)
			require.NoError(t, err, name)
			cfg := kafkaclient.Config{BootstrapServer: "broker:9092", Timeout: time.Second, SASL: sasl}
			assert.NoError(t, cfg.Validate(), name)
		}
	})

	t.Run("env used when config unset", func(t *testing.T) {
		t.Setenv("KAFKA_SASL_MECHANISM", "plain")
		t.Setenv("KAFKA_SASL_USERNAME", "envuser")
		t.Setenv("KAFKA_SASL_PASSWORD", "envpass")
		cfg, err := p.generateSASLConfig(null)
		require.NoError(t, err)
		assert.Equal(t, string(kafkaclient.SASLMechanismPlain), cfg.Mechanism)
		assert.Equal(t, "envuser", cfg.Username)
		assert.Equal(t, "envpass", cfg.Password)
	})

	t.Run("mechanism in config keeps env credentials", func(t *testing.T) {
		t.Setenv("KAFKA_SASL_USERNAME", "envuser")
		t.Setenv("KAFKA_SASL_PASSWORD", "envpass")
		m := null
		m.Mechanism = types.StringValue("scram-sha512")
		cfg, err := p.generateSASLConfig(m)
		require.NoError(t, err)
		assert.Equal(t, "envuser", cfg.Username)
		assert.Equal(t, "envpass", cfg.Password)
	})

	t.Run("config credentials override env", func(t *testing.T) {
		t.Setenv("KAFKA_SASL_MECHANISM", "plain")
		t.Setenv("KAFKA_SASL_USERNAME", "envuser")
		t.Setenv("KAFKA_SASL_PASSWORD", "envpass")
		m := null
		m.Username = types.StringValue("cfguser")
		cfg, err := p.generateSASLConfig(m)
		require.NoError(t, err)
		assert.Equal(t, "cfguser", cfg.Username)
	})
}

func TestBootstrapServer(t *testing.T) {
	p := &kafkaProvider{typeName: "kafka"}

	t.Run("config_first_entry_wins", func(t *testing.T) {
		t.Setenv("KAFKA_BOOTSTRAP_SERVERS", "env:9092")
		got, err := p.bootstrapServer([]types.String{types.StringValue("a:9092"), types.StringValue("b:9092")})
		require.NoError(t, err)
		assert.Equal(t, "a:9092", got)
	})
	t.Run("falls_back_to_env", func(t *testing.T) {
		t.Setenv("KAFKA_BOOTSTRAP_SERVERS", " env1:9092 , env2:9092")
		got, err := p.bootstrapServer(nil)
		require.NoError(t, err)
		assert.Equal(t, "env1:9092", got)
	})
	t.Run("errors_when_unset", func(t *testing.T) {
		t.Setenv("KAFKA_BOOTSTRAP_SERVERS", "")
		_, err := p.bootstrapServer(nil)
		assert.Error(t, err)
	})
}

func TestGetEnvInt(t *testing.T) {
	p := &kafkaProvider{typeName: "kafka"}

	tests := []struct {
		name    string
		value   string
		want    int
		wantErr bool
	}{
		{name: "empty_uses_fallback", value: "", want: 300},
		{name: "valid_integer", value: "42", want: 42},
		{name: "negative_integer", value: "-1", want: -1},
		{name: "malformed_errors", value: "abc", wantErr: true},
		{name: "float_errors", value: "1.5", wantErr: true},
		{name: "whitespace_errors", value: " 10", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KAFKA_TIMEOUT", tc.value)
			got, err := p.getEnvInt("TIMEOUT", 300)
			if tc.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "KAFKA_TIMEOUT")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestGetEnvBool(t *testing.T) {
	p := &kafkaProvider{typeName: "kafka"}

	tests := []struct {
		name    string
		value   string
		want    bool
		wantErr bool
	}{
		{name: "empty_uses_fallback", value: "", want: true},
		{name: "true", value: "true", want: true},
		{name: "false", value: "false", want: false},
		{name: "numeric_one", value: "1", want: true},
		{name: "numeric_zero", value: "0", want: false},
		{name: "typo_errors", value: "flase", wantErr: true},
		{name: "yes_errors", value: "yes", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KAFKA_SASL_ENABLED", tc.value)
			got, err := p.getEnvBool("SASL_ENABLED", true)
			if tc.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "KAFKA_SASL_ENABLED")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestGenerateSASLConfig_Credentials(t *testing.T) {
	p := &kafkaProvider{typeName: "kafka"}

	tests := []struct {
		name      string
		mechanism string
		username  types.String
		password  types.String
		envUser   string
		envPass   string
		wantErr   []string // text that the error must contain
	}{
		{name: "plain_complete", mechanism: "plain", username: types.StringValue("u"), password: types.StringValue("p")},
		{name: "plain_complete_from_env", mechanism: "plain", envUser: "u", envPass: "p"},
		{name: "plain_no_user", mechanism: "plain", password: types.StringValue("p"), wantErr: []string{"user name", "sasl.username"}},
		{name: "plain_no_password", mechanism: "plain", username: types.StringValue("u"), wantErr: []string{"password", "sasl.password"}},
		{name: "plain_empty_user_in_config", mechanism: "plain", username: types.StringValue(""), password: types.StringValue("p"), envUser: "u", wantErr: []string{"user name"}},
		{name: "scram256_no_credentials", mechanism: "scram-sha256", wantErr: []string{"user name", "password"}},
		{name: "scram512_no_password", mechanism: "scram-sha512", username: types.StringValue("u"), wantErr: []string{"password"}},
		{name: "plain_whitespace_user", mechanism: "plain", username: types.StringValue("  "), password: types.StringValue("p"), wantErr: []string{"user name"}},
		{name: "plain_whitespace_password", mechanism: "plain", username: types.StringValue("u"), password: types.StringValue(" \t"), wantErr: []string{"password"}},
		{name: "aws_with_whitespace_values", mechanism: "aws-msk-iam", username: types.StringValue(" "), password: types.StringValue(" ")},
		{name: "aws_without_credentials", mechanism: "aws-msk-iam"},
		{name: "aws_with_empty_config_values", mechanism: "aws-msk-iam", username: types.StringValue(""), password: types.StringValue("")},
		{name: "aws_with_user_in_config", mechanism: "aws-msk-iam", username: types.StringValue("secret-user"), wantErr: []string{"does not use", "user name"}},
		{name: "aws_with_password_in_config", mechanism: "aws-msk-iam", password: types.StringValue("secret-pass"), wantErr: []string{"does not use", "password"}},
		{name: "aws_with_user_in_env", mechanism: "aws-msk-iam", envUser: "secret-user", wantErr: []string{"does not use", "KAFKA_SASL_USERNAME"}},
		{name: "aws_with_password_in_env", mechanism: "aws-msk-iam", envPass: "secret-pass", wantErr: []string{"does not use", "KAFKA_SASL_PASSWORD"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KAFKA_SASL_USERNAME", tc.envUser)
			t.Setenv("KAFKA_SASL_PASSWORD", tc.envPass)
			m := SASLConfigModel{
				Mechanism: types.StringValue(tc.mechanism),
				Username:  tc.username,
				Password:  tc.password,
			}
			cfg, err := p.generateSASLConfig(m)
			if len(tc.wantErr) == 0 {
				require.NoError(t, err)
				require.NotNil(t, cfg)
				return
			}
			require.Error(t, err)
			for _, want := range tc.wantErr {
				assert.Contains(t, err.Error(), want)
			}
			for _, secret := range []string{"secret-user", "secret-pass"} {
				assert.NotContains(t, err.Error(), secret, "an error must never print a credential value")
			}
		})
	}
}

// nullProviderModel returns a provider configuration with every value unset
// and a bootstrap server.
func nullProviderModel() kafkaProviderModel {
	return kafkaProviderModel{
		BootstrapServers: []types.String{types.StringValue("broker:9092")},
		SASL: SASLConfigModel{
			Enabled:   types.BoolNull(),
			Mechanism: types.StringNull(),
			Username:  types.StringNull(),
			Password:  types.StringNull(),
		},
		TLS:     TLSConfigModel{Enabled: types.BoolNull(), SkipVerify: types.BoolNull()},
		Timeout: types.Int64Null(),
	}
}

// clearProviderEnv unsets every KAFKA_ variable for one test.
func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"BOOTSTRAP_SERVERS", "TLS_ENABLED", "TLS_SKIP_VERIFY", "SASL_ENABLED",
		"SASL_MECHANISM", "SASL_USERNAME", "SASL_PASSWORD", "TIMEOUT",
	} {
		t.Setenv("KAFKA_"+key, "")
	}
}

func TestBuildClientConfig_EmptyEnvIsUnset(t *testing.T) {
	p := &kafkaProvider{typeName: "kafka"}

	for _, key := range []string{
		"BOOTSTRAP_SERVERS", "TLS_ENABLED", "TLS_SKIP_VERIFY", "SASL_ENABLED",
		"SASL_MECHANISM", "SASL_USERNAME", "SASL_PASSWORD", "TIMEOUT",
	} {
		t.Run(key, func(t *testing.T) {
			clearProviderEnv(t)
			config := nullProviderModel()
			config.SASL.Mechanism = types.StringValue("plain")
			config.SASL.Username = types.StringValue("u")
			config.SASL.Password = types.StringValue("p")
			t.Setenv("KAFKA_"+key, "")

			var diags diag.Diagnostics
			got := p.buildClientConfig(config, &diags)
			require.False(t, diags.HasError(), "%v", diags)
			assert.True(t, got.TLS.Enabled)
			assert.False(t, got.TLS.SkipVerify)
			assert.Equal(t, 300*time.Second, got.Timeout)
			require.NotNil(t, got.SASL)
		})
	}

	t.Run("empty_mechanism_variable_gives_the_missing_mechanism_error", func(t *testing.T) {
		clearProviderEnv(t)
		var diags diag.Diagnostics
		p.buildClientConfig(nullProviderModel(), &diags)
		require.True(t, diags.HasError())
		assert.Contains(t, diags.Errors()[0].Detail(), "no SASL mechanism is set")
	})
}

func TestBuildClientConfig_MissingMechanism(t *testing.T) {
	p := &kafkaProvider{typeName: "kafka"}
	clearProviderEnv(t)

	t.Run("sasl_on_by_default", func(t *testing.T) {
		var diags diag.Diagnostics
		p.buildClientConfig(nullProviderModel(), &diags)
		require.True(t, diags.HasError())
		assert.Contains(t, diags.Errors()[0].Detail(), "scram-sha512")
	})
	t.Run("sasl_off_needs_no_mechanism", func(t *testing.T) {
		config := nullProviderModel()
		config.SASL.Enabled = types.BoolValue(false)
		var diags diag.Diagnostics
		got := p.buildClientConfig(config, &diags)
		require.False(t, diags.HasError(), "%v", diags)
		assert.Nil(t, got.SASL)
	})
	t.Run("sasl_off_with_environment_variable", func(t *testing.T) {
		t.Setenv("KAFKA_SASL_ENABLED", "false")
		var diags diag.Diagnostics
		got := p.buildClientConfig(nullProviderModel(), &diags)
		require.False(t, diags.HasError(), "%v", diags)
		assert.Nil(t, got.SASL)
	})
}

func TestBuildClientConfig_PlainWithoutTLSWarning(t *testing.T) {
	p := &kafkaProvider{typeName: "kafka"}

	tests := []struct {
		name        string
		mechanism   string
		tlsEnabled  bool
		skipVerify  bool
		saslEnabled bool
		wantWarning bool
	}{
		{name: "plain_without_tls_warns", mechanism: "plain", saslEnabled: true, wantWarning: true},
		{name: "plain_with_tls_is_quiet", mechanism: "plain", tlsEnabled: true, saslEnabled: true},
		{name: "plain_with_tls_skip_verify_is_quiet", mechanism: "plain", tlsEnabled: true, skipVerify: true, saslEnabled: true},
		{name: "scram_without_tls_is_quiet", mechanism: "scram-sha256", saslEnabled: true},
		{name: "aws_without_tls_is_quiet", mechanism: "aws-msk-iam", saslEnabled: true},
		{name: "sasl_off_without_tls_is_quiet", saslEnabled: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearProviderEnv(t)
			config := nullProviderModel()
			config.TLS.Enabled = types.BoolValue(tc.tlsEnabled)
			config.TLS.SkipVerify = types.BoolValue(tc.skipVerify)
			config.SASL.Enabled = types.BoolValue(tc.saslEnabled)
			if tc.mechanism != "" {
				config.SASL.Mechanism = types.StringValue(tc.mechanism)
			}
			if tc.mechanism != "aws-msk-iam" {
				config.SASL.Username = types.StringValue("user")
				config.SASL.Password = types.StringValue("secret-pass")
			}

			var diags diag.Diagnostics
			p.buildClientConfig(config, &diags)
			require.False(t, diags.HasError(), "%v", diags)
			if !tc.wantWarning {
				assert.Zero(t, diags.WarningsCount())
				return
			}
			require.Equal(t, 1, diags.WarningsCount())
			warning := diags.Warnings()[0]
			assert.Contains(t, warning.Detail(), "clear text")
			assert.NotContains(t, warning.Detail(), "secret-pass")
		})
	}
}

func TestBuildClientConfig_Timeout(t *testing.T) {
	p := &kafkaProvider{typeName: "kafka"}

	tests := []struct {
		name        string
		env         string
		config      types.Int64
		wantErr     string
		wantTimeout time.Duration
		wantDial    time.Duration
	}{
		{name: "default", config: types.Int64Null(), wantTimeout: 300 * time.Second, wantDial: kafkaclient.MaxDialTimeout},
		{name: "short_config_sets_both", config: types.Int64Value(5), wantTimeout: 5 * time.Second, wantDial: 5 * time.Second},
		{name: "config_at_the_dial_limit", config: types.Int64Value(30), wantTimeout: 30 * time.Second, wantDial: 30 * time.Second},
		{name: "long_config_limits_the_dial_timeout", config: types.Int64Value(900), wantTimeout: 900 * time.Second, wantDial: kafkaclient.MaxDialTimeout},
		{name: "env_is_used", env: "10", config: types.Int64Null(), wantTimeout: 10 * time.Second, wantDial: 10 * time.Second},
		{name: "config_wins_over_env", env: "10", config: types.Int64Value(60), wantTimeout: 60 * time.Second, wantDial: kafkaclient.MaxDialTimeout},
		{name: "config_too_large_is_an_error", config: types.Int64Value(math.MaxInt64), wantErr: "too large"},
		{name: "env_too_large_is_an_error", env: "9223372036854775807", config: types.Int64Null(), wantErr: "too large"},
		{name: "env_zero_is_an_error", env: "0", config: types.Int64Null(), wantErr: "KAFKA_TIMEOUT must be 1 or more"},
		{name: "env_negative_is_an_error", env: "-5", config: types.Int64Null(), wantErr: "KAFKA_TIMEOUT must be 1 or more"},
		{name: "env_not_a_number_is_an_error", env: "soon", config: types.Int64Null(), wantErr: "KAFKA_TIMEOUT must be an integer"},
		{name: "invalid_env_is_an_error_even_with_config", env: "0", config: types.Int64Value(60), wantErr: "KAFKA_TIMEOUT must be 1 or more"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearProviderEnv(t)
			t.Setenv("KAFKA_TIMEOUT", tc.env)
			config := nullProviderModel()
			config.SASL.Enabled = types.BoolValue(false)
			config.Timeout = tc.config

			var diags diag.Diagnostics
			got := p.buildClientConfig(config, &diags)
			if tc.wantErr != "" {
				require.True(t, diags.HasError())
				assert.Contains(t, diags.Errors()[0].Detail(), tc.wantErr)
				return
			}
			require.False(t, diags.HasError(), "%v", diags)
			assert.Equal(t, tc.wantTimeout, got.Timeout)
			assert.Equal(t, tc.wantDial, got.DialTimeout)
		})
	}
}

func TestBuildClientConfig_TLS(t *testing.T) {
	p := &kafkaProvider{typeName: "kafka"}

	tests := []struct {
		name           string
		envEnabled     string
		envSkipVerify  string
		config         TLSConfigModel
		wantEnabled    bool
		wantSkipVerify bool
		wantErr        string
	}{
		{name: "defaults", config: TLSConfigModel{Enabled: types.BoolNull(), SkipVerify: types.BoolNull()}, wantEnabled: true},
		{name: "env_turns_tls_off", envEnabled: "false", config: TLSConfigModel{Enabled: types.BoolNull(), SkipVerify: types.BoolNull()}},
		{name: "env_turns_skip_verify_on", envSkipVerify: "true", config: TLSConfigModel{Enabled: types.BoolNull(), SkipVerify: types.BoolNull()}, wantEnabled: true, wantSkipVerify: true},
		{name: "config_enabled_wins_over_env", envEnabled: "false", config: TLSConfigModel{Enabled: types.BoolValue(true), SkipVerify: types.BoolNull()}, wantEnabled: true},
		{name: "config_disabled_wins_over_env", envEnabled: "true", config: TLSConfigModel{Enabled: types.BoolValue(false), SkipVerify: types.BoolNull()}},
		{name: "config_skip_verify_false_wins_over_env", envSkipVerify: "true", config: TLSConfigModel{Enabled: types.BoolNull(), SkipVerify: types.BoolValue(false)}, wantEnabled: true},
		{name: "config_skip_verify_true_wins_over_env", envSkipVerify: "false", config: TLSConfigModel{Enabled: types.BoolNull(), SkipVerify: types.BoolValue(true)}, wantEnabled: true, wantSkipVerify: true},
		{name: "invalid_enabled_env", envEnabled: "yes", config: TLSConfigModel{Enabled: types.BoolNull(), SkipVerify: types.BoolNull()}, wantErr: "KAFKA_TLS_ENABLED must be a boolean"},
		{name: "invalid_skip_verify_env", envSkipVerify: "maybe", config: TLSConfigModel{Enabled: types.BoolNull(), SkipVerify: types.BoolNull()}, wantErr: "KAFKA_TLS_SKIP_VERIFY must be a boolean"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearProviderEnv(t)
			t.Setenv("KAFKA_TLS_ENABLED", tc.envEnabled)
			t.Setenv("KAFKA_TLS_SKIP_VERIFY", tc.envSkipVerify)
			config := nullProviderModel()
			config.SASL.Enabled = types.BoolValue(false)
			config.TLS = tc.config

			var diags diag.Diagnostics
			got := p.buildClientConfig(config, &diags)
			if tc.wantErr != "" {
				require.True(t, diags.HasError())
				assert.Contains(t, diags.Errors()[0].Detail(), tc.wantErr)
				return
			}
			require.False(t, diags.HasError(), "%v", diags)
			assert.Equal(t, tc.wantEnabled, got.TLS.Enabled)
			assert.Equal(t, tc.wantSkipVerify, got.TLS.SkipVerify)
		})
	}
}

func TestSchemaDescriptionsNameEveryEnvironmentVariable(t *testing.T) {
	var resp provider.SchemaResponse
	(&kafkaProvider{typeName: "kafka"}).Schema(t.Context(), provider.SchemaRequest{}, &resp)

	var all strings.Builder
	var collect func(attrs map[string]schema.Attribute)
	collect = func(attrs map[string]schema.Attribute) {
		for _, a := range attrs {
			all.WriteString(a.GetMarkdownDescription())
			if nested, ok := a.(schema.SingleNestedAttribute); ok {
				collect(nested.Attributes)
			}
		}
	}
	collect(resp.Schema.Attributes)

	for _, name := range []string{
		"KAFKA_BOOTSTRAP_SERVERS", "KAFKA_TLS_ENABLED", "KAFKA_TLS_SKIP_VERIFY", "KAFKA_SASL_ENABLED",
		"KAFKA_SASL_MECHANISM", "KAFKA_SASL_USERNAME", "KAFKA_SASL_PASSWORD", "KAFKA_TIMEOUT",
	} {
		assert.Contains(t, all.String(), name)
	}
	assert.NotContains(t, all.String(), "default: aws-msk-iam")
}

func TestGetEnv_EmptyIsUnset(t *testing.T) {
	p := &kafkaProvider{typeName: "kafka"}
	t.Setenv("KAFKA_SASL_MECHANISM", "")
	assert.Equal(t, "fallback", p.getEnv("SASL_MECHANISM", "fallback"))
	t.Setenv("KAFKA_SASL_MECHANISM", "plain")
	assert.Equal(t, "plain", p.getEnv("SASL_MECHANISM", "fallback"))
}

// configureWith runs Configure with the given values for the nested settings.
// A nil value is a null attribute.
func configureWith(t *testing.T, tlsValues, saslValues map[string]tftypes.Value, timeout tftypes.Value) *provider.ConfigureResponse {
	t.Helper()
	p := &kafkaProvider{typeName: "kafka"}

	var schemaResp provider.SchemaResponse
	p.Schema(t.Context(), provider.SchemaRequest{}, &schemaResp)
	objectType, ok := schemaResp.Schema.Type().TerraformType(t.Context()).(tftypes.Object)
	require.True(t, ok)

	nested := func(name string, values map[string]tftypes.Value) tftypes.Value {
		nestedType := objectType.AttributeTypes[name].(tftypes.Object)
		all := map[string]tftypes.Value{}
		for attr, attrType := range nestedType.AttributeTypes {
			all[attr] = tftypes.NewValue(attrType, nil)
		}
		maps.Copy(all, values)
		return tftypes.NewValue(nestedType, all)
	}
	raw := tftypes.NewValue(objectType, map[string]tftypes.Value{
		"bootstrap_servers": tftypes.NewValue(objectType.AttributeTypes["bootstrap_servers"], []tftypes.Value{
			tftypes.NewValue(tftypes.String, "broker:9092"),
		}),
		"tls":     nested("tls", tlsValues),
		"sasl":    nested("sasl", saslValues),
		"timeout": timeout,
	})

	resp := &provider.ConfigureResponse{}
	p.Configure(t.Context(), provider.ConfigureRequest{
		Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: raw},
	}, resp)
	return resp
}

func TestConfigure(t *testing.T) {
	str := func(v string) tftypes.Value { return tftypes.NewValue(tftypes.String, v) }
	boolean := func(v bool) tftypes.Value { return tftypes.NewValue(tftypes.Bool, v) }
	nullTimeout := tftypes.NewValue(tftypes.Number, nil)

	t.Run("plain_without_tls_creates_a_client_and_warns", func(t *testing.T) {
		clearProviderEnv(t)
		resp := configureWith(t,
			map[string]tftypes.Value{"enabled": boolean(false)},
			map[string]tftypes.Value{"mechanism": str("plain"), "username": str("user"), "password": str("pass")},
			nullTimeout)
		require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
		assert.Equal(t, 1, resp.Diagnostics.WarningsCount())
		assert.NotNil(t, resp.ResourceData)
		assert.NotNil(t, resp.DataSourceData)
	})

	t.Run("missing_mechanism_is_an_error_and_creates_no_client", func(t *testing.T) {
		clearProviderEnv(t)
		resp := configureWith(t, nil, nil, nullTimeout)
		require.True(t, resp.Diagnostics.HasError())
		assert.Contains(t, resp.Diagnostics.Errors()[0].Detail(), "no SASL mechanism is set")
		assert.Nil(t, resp.ResourceData)
	})

	t.Run("unknown_tls_enabled_is_an_error", func(t *testing.T) {
		clearProviderEnv(t)
		resp := configureWith(t,
			map[string]tftypes.Value{"enabled": tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue)},
			map[string]tftypes.Value{"enabled": boolean(false)},
			nullTimeout)
		require.True(t, resp.Diagnostics.HasError())
		assert.Contains(t, resp.Diagnostics.Errors()[0].Detail(), "tls.enabled")
	})

	t.Run("unknown_mechanism_is_fine_if_sasl_is_off", func(t *testing.T) {
		clearProviderEnv(t)
		resp := configureWith(t, nil,
			map[string]tftypes.Value{
				"enabled":   boolean(false),
				"mechanism": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
			},
			nullTimeout)
		require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
	})

	t.Run("unknown_skip_verify_is_fine_if_tls_is_off", func(t *testing.T) {
		clearProviderEnv(t)
		resp := configureWith(t,
			map[string]tftypes.Value{
				"enabled":     boolean(false),
				"skip_verify": tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue),
			},
			map[string]tftypes.Value{"enabled": boolean(false)},
			nullTimeout)
		require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
	})

	t.Run("unknown_setting_error_points_at_the_attribute", func(t *testing.T) {
		clearProviderEnv(t)
		resp := configureWith(t, nil,
			map[string]tftypes.Value{"mechanism": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)},
			nullTimeout)
		require.True(t, resp.Diagnostics.HasError())
		withPath, ok := resp.Diagnostics.Errors()[0].(diag.DiagnosticWithPath)
		require.True(t, ok)
		assert.Equal(t, "sasl.mechanism", withPath.Path().String())
	})

	t.Run("unknown_mechanism_is_an_error_that_names_the_setting", func(t *testing.T) {
		clearProviderEnv(t)
		resp := configureWith(t, nil,
			map[string]tftypes.Value{"mechanism": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)},
			nullTimeout)
		require.True(t, resp.Diagnostics.HasError())
		assert.Contains(t, resp.Diagnostics.Errors()[0].Detail(), "sasl.mechanism")
	})
}

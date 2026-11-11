package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/segmentio/topicctl/pkg/admin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateSASLConfig(t *testing.T) {
	p := &kafkaProvider{typeName: "kafka"}
	null := SASLConfigModel{
		Mechanism: types.StringNull(),
		Username:  types.StringNull(),
		Password:  types.StringNull(),
	}

	t.Run("defaults to aws-msk-iam", func(t *testing.T) {
		cfg, err := p.generateSASLConfig(null)
		assert.NoError(t, err)
		assert.Equal(t, admin.SASLMechanismAWSMSKIAM, cfg.Mechanism)
	})

	for name, want := range map[string]admin.SASLMechanism{
		"plain":         admin.SASLMechanismPlain,
		"scram-sha256":  admin.SASLMechanismScramSHA256,
		"scram-sha-256": admin.SASLMechanismScramSHA256,
		"scram-sha512":  admin.SASLMechanismScramSHA512,
		"scram-sha-512": admin.SASLMechanismScramSHA512,
	} {
		t.Run(name, func(t *testing.T) {
			m := null
			m.Mechanism = types.StringValue(name)
			m.Username = types.StringValue("user")
			m.Password = types.StringValue("pass")
			cfg, err := p.generateSASLConfig(m)
			assert.NoError(t, err)
			assert.Equal(t, want, cfg.Mechanism)
			assert.Equal(t, "user", cfg.Username)
			assert.Equal(t, "pass", cfg.Password)
		})
	}

	t.Run("unknown mechanism errors", func(t *testing.T) {
		m := null
		m.Mechanism = types.StringValue("bogus")
		_, err := p.generateSASLConfig(m)
		assert.Error(t, err)
	})

	t.Run("env used when config unset", func(t *testing.T) {
		t.Setenv("KAFKA_SASL_MECHANISM", "plain")
		t.Setenv("KAFKA_SASL_USERNAME", "envuser")
		t.Setenv("KAFKA_SASL_PASSWORD", "envpass")
		cfg, err := p.generateSASLConfig(null)
		assert.NoError(t, err)
		assert.Equal(t, admin.SASLMechanismPlain, cfg.Mechanism)
		assert.Equal(t, "envuser", cfg.Username)
		assert.Equal(t, "envpass", cfg.Password)
	})

	t.Run("mechanism in config keeps env credentials", func(t *testing.T) {
		t.Setenv("KAFKA_SASL_USERNAME", "envuser")
		t.Setenv("KAFKA_SASL_PASSWORD", "envpass")
		m := null
		m.Mechanism = types.StringValue("scram-sha512")
		cfg, err := p.generateSASLConfig(m)
		assert.NoError(t, err)
		assert.Equal(t, "envuser", cfg.Username)
		assert.Equal(t, "envpass", cfg.Password)
	})

	t.Run("config credentials override env", func(t *testing.T) {
		t.Setenv("KAFKA_SASL_MECHANISM", "plain")
		t.Setenv("KAFKA_SASL_USERNAME", "envuser")
		m := null
		m.Username = types.StringValue("cfguser")
		cfg, err := p.generateSASLConfig(m)
		assert.NoError(t, err)
		assert.Equal(t, "cfguser", cfg.Username)
	})
}

func TestBootstrapServer(t *testing.T) {
	p := &kafkaProvider{typeName: "kafka"}

	t.Run("config_first_entry_wins", func(t *testing.T) {
		t.Setenv("KAFKA_BOOTSTRAP_SERVERS", "env:9092")
		got, err := p.bootstrapServer([]types.String{types.StringValue("a:9092"), types.StringValue("b:9092")})
		assert.NoError(t, err)
		assert.Equal(t, "a:9092", got)
	})
	t.Run("falls_back_to_env", func(t *testing.T) {
		t.Setenv("KAFKA_BOOTSTRAP_SERVERS", " env1:9092 , env2:9092")
		got, err := p.bootstrapServer(nil)
		assert.NoError(t, err)
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

package provider

import (
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/segmentio/topicctl/pkg/admin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

func TestKafkaClientConfig(t *testing.T) {
	t.Run("tls_and_sasl_enabled", func(t *testing.T) {
		in := admin.BrokerAdminClientConfig{}
		in.BrokerAddr = "broker:9093"
		in.TLS = admin.TLSConfig{Enabled: true, SkipVerify: true}
		in.SASL = admin.SASLConfig{
			Enabled:   true,
			Mechanism: admin.SASLMechanismScramSHA256,
			Username:  "user",
			Password:  "pass",
		}

		got := kafkaClientConfig(in, 30*time.Second)

		assert.Equal(t, "broker:9093", got.BootstrapServer)
		assert.Equal(t, kafkaclient.TLSConfig{Enabled: true, SkipVerify: true}, got.TLS)
		assert.Equal(t, 30*time.Second, got.Timeout)
		require.NotNil(t, got.SASL)
		assert.Equal(t, "user", got.SASL.Username)
		assert.Equal(t, "pass", got.SASL.Password)
		mechanism, err := kafkaclient.ParseSASLMechanism(got.SASL.Mechanism)
		require.NoError(t, err)
		assert.Equal(t, kafkaclient.SASLMechanismScramSHA256, mechanism)
		assert.NoError(t, got.Validate())
	})

	t.Run("sasl_disabled_leaves_sasl_nil", func(t *testing.T) {
		in := admin.BrokerAdminClientConfig{}
		in.BrokerAddr = "broker:9092"

		got := kafkaClientConfig(in, time.Second)

		assert.Nil(t, got.SASL)
		assert.False(t, got.TLS.Enabled)
	})

	t.Run("every_generated_mechanism_is_accepted", func(t *testing.T) {
		p := &kafkaProvider{typeName: "kafka"}
		for _, name := range []string{"plain", "scram-sha256", "scram-sha-512", "aws-msk-iam"} {
			sasl, err := p.generateSASLConfig(SASLConfigModel{Mechanism: types.StringValue(name)})
			require.NoError(t, err, name)
			in := admin.BrokerAdminClientConfig{}
			in.BrokerAddr = "broker:9092"
			in.SASL = sasl
			assert.NoError(t, kafkaClientConfig(in, time.Second).Validate(), name)
		}
	})
}

func TestKafkaclientReplicaCount(t *testing.T) {
	partition := func(id int, replicas ...int) kafkaclient.PartitionInfo {
		return kafkaclient.PartitionInfo{ID: id, Replicas: replicas}
	}

	t.Run("uniform", func(t *testing.T) {
		got, err := kafkaclientReplicaCount(kafkaclient.TopicInfo{
			Partitions: []kafkaclient.PartitionInfo{partition(0, 1, 2), partition(1, 2, 3)},
		})
		require.NoError(t, err)
		assert.Equal(t, 2, got)
	})

	t.Run("uneven_is_an_error", func(t *testing.T) {
		_, err := kafkaclientReplicaCount(kafkaclient.TopicInfo{
			Partitions: []kafkaclient.PartitionInfo{partition(0, 1, 2), partition(1, 2)},
		})
		assert.Error(t, err)
	})

	t.Run("no_partitions_matches_resource", func(t *testing.T) {
		got, err := kafkaclientReplicaCount(kafkaclient.TopicInfo{})
		require.NoError(t, err)
		want, _ := replicaCount(admin.TopicInfo{})
		assert.Equal(t, want, got)
	})
}

func TestTopicDataSourceConfigure(t *testing.T) {
	t.Run("accepts_read_only_client", func(t *testing.T) {
		d := &topicDataSource{}
		resp := &datasource.ConfigureResponse{}
		d.Configure(t.Context(), datasource.ConfigureRequest{ProviderData: &kafkaclient.ReadOnlyClient{}}, resp)
		assert.False(t, resp.Diagnostics.HasError())
		assert.NotNil(t, d.client)
	})

	t.Run("rejects_topicctl_client", func(t *testing.T) {
		d := &topicDataSource{}
		resp := &datasource.ConfigureResponse{}
		d.Configure(t.Context(), datasource.ConfigureRequest{ProviderData: &admin.BrokerAdminClient{}}, resp)
		assert.True(t, resp.Diagnostics.HasError())
		assert.Nil(t, d.client)
	})

	t.Run("nil_provider_data_is_ignored", func(t *testing.T) {
		d := &topicDataSource{}
		resp := &datasource.ConfigureResponse{}
		d.Configure(t.Context(), datasource.ConfigureRequest{}, resp)
		assert.False(t, resp.Diagnostics.HasError())
	})
}

func TestTopicResourceConfigure(t *testing.T) {
	t.Run("accepts_resource_clients", func(t *testing.T) {
		topicctlClient := &admin.BrokerAdminClient{}
		kafkaClient := &kafkaclient.Client{}
		r := &topicResource{}
		resp := &resource.ConfigureResponse{}
		r.Configure(t.Context(), resource.ConfigureRequest{
			ProviderData: &resourceClients{kafka: kafkaClient, topicctl: topicctlClient},
		}, resp)
		assert.False(t, resp.Diagnostics.HasError())
		assert.Same(t, kafkaClient, r.kafka)
		assert.Same(t, topicctlClient, r.topicctl)
	})

	t.Run("rejects_topicctl_client", func(t *testing.T) {
		r := &topicResource{}
		resp := &resource.ConfigureResponse{}
		r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: &admin.BrokerAdminClient{}}, resp)
		assert.True(t, resp.Diagnostics.HasError())
		assert.Nil(t, r.kafka)
		assert.Nil(t, r.topicctl)
	})

	t.Run("rejects_read_only_client", func(t *testing.T) {
		r := &topicResource{}
		resp := &resource.ConfigureResponse{}
		r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: &kafkaclient.ReadOnlyClient{}}, resp)
		assert.True(t, resp.Diagnostics.HasError())
		assert.Nil(t, r.kafka)
		assert.Nil(t, r.topicctl)
	})

	t.Run("nil_provider_data_is_ignored", func(t *testing.T) {
		r := &topicResource{}
		resp := &resource.ConfigureResponse{}
		r.Configure(t.Context(), resource.ConfigureRequest{}, resp)
		assert.False(t, resp.Diagnostics.HasError())
		assert.Nil(t, r.kafka)
		assert.Nil(t, r.topicctl)
	})
}

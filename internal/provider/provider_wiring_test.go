package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

func TestReplicaCount(t *testing.T) {
	partition := func(id int, replicas ...int) kafkaclient.PartitionInfo {
		return kafkaclient.PartitionInfo{ID: id, Replicas: replicas}
	}

	t.Run("uniform", func(t *testing.T) {
		got, err := replicaCount(kafkaclient.TopicInfo{
			Partitions: []kafkaclient.PartitionInfo{partition(0, 1, 2), partition(1, 2, 3)},
		})
		require.NoError(t, err)
		assert.Equal(t, 2, got)
	})

	t.Run("uneven_is_an_error", func(t *testing.T) {
		_, err := replicaCount(kafkaclient.TopicInfo{
			Partitions: []kafkaclient.PartitionInfo{partition(0, 1, 2), partition(1, 2)},
		})
		assert.Error(t, err)
	})

	t.Run("no_partitions_is_an_error", func(t *testing.T) {
		_, err := replicaCount(kafkaclient.TopicInfo{Name: "t"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no partitions")
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

	t.Run("rejects_read_write_client", func(t *testing.T) {
		d := &topicDataSource{}
		resp := &datasource.ConfigureResponse{}
		d.Configure(t.Context(), datasource.ConfigureRequest{ProviderData: &kafkaclient.Client{}}, resp)
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
	t.Run("accepts_client", func(t *testing.T) {
		kafkaClient := &kafkaclient.Client{}
		r := &topicResource{}
		resp := &resource.ConfigureResponse{}
		r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: kafkaClient}, resp)
		assert.False(t, resp.Diagnostics.HasError())
		assert.Same(t, kafkaClient, r.kafka)
	})

	t.Run("rejects_read_only_client", func(t *testing.T) {
		r := &topicResource{}
		resp := &resource.ConfigureResponse{}
		r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: &kafkaclient.ReadOnlyClient{}}, resp)
		assert.True(t, resp.Diagnostics.HasError())
		assert.Nil(t, r.kafka)
	})

	t.Run("nil_provider_data_is_ignored", func(t *testing.T) {
		r := &topicResource{}
		resp := &resource.ConfigureResponse{}
		r.Configure(t.Context(), resource.ConfigureRequest{}, resp)
		assert.False(t, resp.Diagnostics.HasError())
		assert.Nil(t, r.kafka)
	})
}

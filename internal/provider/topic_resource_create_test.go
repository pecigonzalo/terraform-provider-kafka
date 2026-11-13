package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTopicResourceSaveCreated(t *testing.T) {
	r := &topicResource{}
	var schemaResp resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &schemaResp)

	newResponse := func() *resource.CreateResponse {
		return &resource.CreateResponse{State: tfsdk.State{
			Schema: schemaResp.Schema,
			Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(t.Context()), nil),
		}}
	}
	newData := func() *TopicResourceModel {
		return &TopicResourceModel{
			ID:                types.StringUnknown(),
			Name:              types.StringValue("orders"),
			Partitions:        types.Int64Value(3),
			ReplicationFactor: types.Int64Value(1),
			Config:            types.MapNull(types.StringType),
		}
	}

	t.Run("read_back_succeeds", func(t *testing.T) {
		resp := newResponse()
		r.saveCreated(t.Context(), newData(), func(context.Context) error { return nil }, resp)

		require.Empty(t, resp.Diagnostics)
		var got TopicResourceModel
		require.False(t, resp.State.Get(t.Context(), &got).HasError())
		assert.Equal(t, "orders", got.ID.ValueString())
	})

	t.Run("read_back_fails_still_saves_state_with_a_warning", func(t *testing.T) {
		resp := newResponse()
		r.saveCreated(t.Context(), newData(), func(context.Context) error { return errors.New("not authorized") }, resp)

		require.False(t, resp.Diagnostics.HasError(), "a failed read-back must not fail the apply")
		require.Len(t, resp.Diagnostics.Warnings(), 1)
		assert.Contains(t, resp.Diagnostics.Warnings()[0].Detail(), "not authorized")
		var got TopicResourceModel
		require.False(t, resp.State.Get(t.Context(), &got).HasError())
		assert.Equal(t, "orders", got.ID.ValueString(), "the created topic must be in state")
		assert.Equal(t, int64(3), got.Partitions.ValueInt64())
	})
}

func TestTopicResourceWithoutClient(t *testing.T) {
	// Each operation must report an error and not panic when Configure did
	// not set the client. The check runs before the request is read, so empty
	// requests are enough.
	r := &topicResource{}

	t.Run("create", func(t *testing.T) {
		resp := &resource.CreateResponse{}
		r.Create(t.Context(), resource.CreateRequest{}, resp)
		require.True(t, resp.Diagnostics.HasError())
		assert.Equal(t, missingClientSummary, resp.Diagnostics.Errors()[0].Summary())
	})
	t.Run("read", func(t *testing.T) {
		resp := &resource.ReadResponse{}
		r.Read(t.Context(), resource.ReadRequest{}, resp)
		require.True(t, resp.Diagnostics.HasError())
		assert.Equal(t, missingClientSummary, resp.Diagnostics.Errors()[0].Summary())
	})
	t.Run("update", func(t *testing.T) {
		resp := &resource.UpdateResponse{}
		r.Update(t.Context(), resource.UpdateRequest{}, resp)
		require.True(t, resp.Diagnostics.HasError())
		assert.Equal(t, missingClientSummary, resp.Diagnostics.Errors()[0].Summary())
	})
	t.Run("delete", func(t *testing.T) {
		resp := &resource.DeleteResponse{}
		r.Delete(t.Context(), resource.DeleteRequest{}, resp)
		require.True(t, resp.Diagnostics.HasError())
		assert.Equal(t, missingClientSummary, resp.Diagnostics.Errors()[0].Summary())
	})
}

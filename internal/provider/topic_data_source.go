package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/segmentio/topicctl/pkg/admin"
)

// Ensure provider defined types fully satisfy framework interfaces
var _ datasource.DataSource = &TopicDataSource{}

func NewTopicDataSource() datasource.DataSource {
	return &TopicDataSource{}
}

// TopicDataSource defines the data source implementation.
type TopicDataSource struct {
	client *admin.BrokerAdminClient
}

// TopicDataSourceModel describes the data source data model.
type TopicDataSourceModel struct {
	ID                types.String `tfsdk:"id"`
	Name              types.String `tfsdk:"name"`
	Partitions        types.Int64  `tfsdk:"partitions"`
	ReplicationFactor types.Int64  `tfsdk:"replication_factor"`
	Version           types.Int64  `tfsdk:"version"`
	Config            types.Map    `tfsdk:"configuration"`
}

func (d *TopicDataSource) Metadata(ctx context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_topic"
}

func (d *TopicDataSource) GetSchema(ctx context.Context) (tfsdk.Schema, diag.Diagnostics) {
	return tfsdk.Schema{
		MarkdownDescription: "Topic data source",

		Attributes: map[string]tfsdk.Attribute{
			"id": {
				Type:     types.StringType,
				Computed: true,
			},
			"name": {
				MarkdownDescription: "Topic name",
				Type:                types.StringType,
				Required:            true,
			},
			"partitions": {
				MarkdownDescription: "Topic partitions count",
				Type:                types.Int64Type,
				Computed:            true,
			},
			"replication_factor": {
				MarkdownDescription: "Topic replication factor",
				Type:                types.Int64Type,
				Computed:            true,
			},
			"version": {
				MarkdownDescription: "Topic version",
				Type:                types.Int64Type,
				Computed:            true,
			},
			"configuration": {
				MarkdownDescription: "Configuration version",
				Type:                types.MapType{ElemType: types.StringType},
				Computed:            true,
			},
		},
	}, nil
}

func (d *TopicDataSource) Configure(ctx context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	// Prevent panic if the provider has not been configured.
	if req.ProviderData == nil {
		return
	}

	client, ok := req.ProviderData.(*admin.BrokerAdminClient)

	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Data Source Configure Type",
			fmt.Sprintf("Expected *http.Client, got: %T. Please report this issue to the provider developers.", req.ProviderData),
		)

		return
	}

	d.client = client
}

func (d *TopicDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data TopicDataSourceModel

	// Read Terraform configuration data into the model
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	topicInfo, err := d.client.GetTopic(ctx, data.Name.ValueString(), true)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to read topic, got error: %s", err))
		return
	}

	replicationFactor, err := replicaCount(topicInfo)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to get replica count, got error: %s", err))
		return
	}

	data.ID = types.StringValue(topicInfo.Name)
	data.Name = types.StringValue(topicInfo.Name)
	data.Partitions = types.Int64Value(int64(len(topicInfo.Partitions)))
	data.ReplicationFactor = types.Int64Value(int64(replicationFactor))
	data.Version = types.Int64Value(int64(topicInfo.Version))

	configElement := make(map[string]attr.Value)
	for k, v := range topicInfo.Config {
		configElement[k] = types.StringValue(v)
	}
	data.Config = types.MapValueMust(
		types.StringType,
		configElement,
	)

	// Write logs using the tflog package
	// Documentation: https://terraform.io/plugin/log
	tflog.Trace(ctx, "read a data source")

	// Save data into Terraform state
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

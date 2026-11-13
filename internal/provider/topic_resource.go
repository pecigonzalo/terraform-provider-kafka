package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

// topicNameCharsRegexp matches the characters Kafka allows in topic names.
var topicNameCharsRegexp = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// Ensure provider defined types fully satisfy framework interfaces
var (
	_ resource.Resource                = &topicResource{}
	_ resource.ResourceWithConfigure   = &topicResource{}
	_ resource.ResourceWithImportState = &topicResource{}
	_ resource.ResourceWithModifyPlan  = &topicResource{}
)

func NewTopicResource() resource.Resource {
	return &topicResource{}
}

// topicResource defines the resource implementation.
//
// Every operation goes through kafka. Changing the replication factor or
// growing the partition count computes the new placement with the placement
// package and applies it through the same client.
type topicResource struct {
	kafka *kafkaclient.Client
}

// TopicResourceModel describes the resource data model.
type TopicResourceModel struct {
	ID                types.String `tfsdk:"id"`
	Name              types.String `tfsdk:"name"`
	Partitions        types.Int64  `tfsdk:"partitions"`
	ReplicationFactor types.Int64  `tfsdk:"replication_factor"`
	Config            types.Map    `tfsdk:"configuration"`
}

func (r *topicResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_topic"
}

func (r *topicResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		// This description is used by the documentation generator and the language server.
		MarkdownDescription: "Kafka Topic resource",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Topic id",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Topic name. Must be 1 to 249 characters long, contain only ASCII letters, " +
					"digits, `.`, `_` and `-`, and must not be `.` or `..`.",
				Required: true,
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 249),
					stringvalidator.RegexMatches(
						topicNameCharsRegexp,
						"must contain only ASCII letters, digits, '.', '_' or '-'",
					),
					stringvalidator.NoneOf(".", ".."),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"partitions": schema.Int64Attribute{
				MarkdownDescription: "Topic partitions count",
				Required:            true,
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
			},
			"replication_factor": schema.Int64Attribute{
				MarkdownDescription: "Topic replication factor count",
				Required:            true,
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
			},
			"configuration": schema.MapAttribute{
				MarkdownDescription: "Topic configuration overrides. Terraform manages only the keys set here. " +
					"When you change this map, Terraform sets the keys in it and removes the keys that you took out, " +
					"so those settings return to the broker default. Terraform does not change any other topic " +
					"setting, so a setting made outside Terraform stays as it is and does not show as a difference. " +
					"Changing this map needs Kafka 2.3 or later.",
				ElementType: types.StringType,
				Optional:    true,
			},
		},
	}
}

func (r *topicResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	// Prevent panic if the provider has not been configured.
	if req.ProviderData == nil {
		return
	}

	client, ok := req.ProviderData.(*kafkaclient.Client)

	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *kafkaclient.Client, got: %T. Please report this issue to the provider developers.", req.ProviderData),
		)

		return
	}

	r.kafka = client
}

// ModifyPlan rejects changes Kafka cannot apply, so they fail at plan time
// instead of partway through apply. The apply-time checks remain as a safety net.
func (r *topicResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// Destroy has a null plan: there is nothing to validate.
	if req.Plan.Raw.IsNull() {
		return
	}

	// Create has a null state.
	var plan, state *TopicResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if !req.State.Raw.IsNull() {
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	}
	if resp.Diagnostics.HasError() {
		return
	}

	if state != nil {
		validatePartitionsNotReduced(plan, state, resp)
	}
	if state == nil || !plan.ReplicationFactor.Equal(state.ReplicationFactor) {
		r.validateReplicationFactorAtPlan(ctx, plan, resp)
	}
}

// validatePartitionsNotReduced errors when the planned partition count is below the current one.
func validatePartitionsNotReduced(plan, state *TopicResourceModel, resp *resource.ModifyPlanResponse) {
	if plan.Partitions.IsUnknown() || state.Partitions.IsNull() {
		return
	}
	if plan.Partitions.ValueInt64() < state.Partitions.ValueInt64() {
		resp.Diagnostics.AddAttributeError(
			path.Root("partitions"),
			"Partition count can't be reduced",
			fmt.Sprintf("Kafka does not support removing partitions: partitions can't be reduced from %d to %d.",
				state.Partitions.ValueInt64(), plan.Partitions.ValueInt64()),
		)
	}
}

// validateReplicationFactorAtPlan errors when the planned replication factor is above the broker count.
// It skips the check when the value is unknown or the provider is not configured yet.
func (r *topicResource) validateReplicationFactorAtPlan(ctx context.Context, plan *TopicResourceModel, resp *resource.ModifyPlanResponse) {
	if plan.ReplicationFactor.IsUnknown() || r.kafka == nil {
		return
	}
	brokerIDs, err := r.kafka.BrokerIDs(ctx)
	if err != nil {
		// The cluster may be unreachable during plan; apply repeats this check.
		tflog.Debug(ctx, fmt.Sprintf("Skipping replication factor plan check: %s", err))
		return
	}
	if plan.ReplicationFactor.ValueInt64() > int64(len(brokerIDs)) {
		resp.Diagnostics.AddAttributeError(
			path.Root("replication_factor"),
			"Replication factor exceeds broker count",
			fmt.Sprintf("Replication factor cannot be higher than the number of brokers: requested %d, cluster has %d brokers.",
				plan.ReplicationFactor.ValueInt64(), len(brokerIDs)),
		)
	}
}

func (r *topicResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data *TopicResourceModel

	// Read Terraform plan data into the model
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, fmt.Sprintf("Creating topic %s", data.Name.ValueString()))
	err := r.kafka.CreateTopic(ctx, kafkaclient.NewTopic{
		Name:              data.Name.ValueString(),
		Partitions:        int(data.Partitions.ValueInt64()),
		ReplicationFactor: int(data.ReplicationFactor.ValueInt64()),
		Config:            configMap(data.Config),
	})
	if err != nil {
		resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to create topic, got error: %s", err))
		return
	}
	r.saveCreated(ctx, data, func(ctx context.Context) error {
		return r.waitForTopicVisible(ctx, data.Name.ValueString())
	}, resp)
}

// saveCreated saves the created topic to state after waiting for it to become
// readable. The topic exists by now, so a failure to read it back must not stop
// the state from being saved, or the next apply would fail with "already
// exists". It is reported as a warning instead.
func (r *topicResource) saveCreated(ctx context.Context, data *TopicResourceModel, wait func(context.Context) error, resp *resource.CreateResponse) {
	if err := wait(ctx); err != nil {
		resp.Diagnostics.AddWarning(
			"Topic created but could not be read back",
			fmt.Sprintf("Topic %q was created, but reading it back failed: %s. The topic is saved to state; "+
				"run terraform plan again to refresh it.", data.Name.ValueString(), err),
		)
	}
	data.ID = data.Name
	tflog.Trace(ctx, "Created topic")

	// Save data into Terraform state
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *topicResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data *TopicResourceModel

	// Read Terraform prior state data into the model
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	topicInfo, err := r.readTopicSettled(ctx, data.ID.ValueString(), data)
	if err != nil {
		if errors.Is(err, kafkaclient.ErrTopicNotFound) {
			// If the Topic does not exist, we remove it and return
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to read topic, got error: %s", err))
		return
	}

	replicationFactor, err := replicaCount(topicInfo)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to get replica count, got error: %s", err))
		return
	}

	data.Name = types.StringValue(topicInfo.Name)
	data.Partitions = types.Int64Value(int64(len(topicInfo.Partitions)))
	data.ReplicationFactor = types.Int64Value(int64(replicationFactor))
	// Brokers report every topic config, defaults included. Only track the keys
	// the user manages, otherwise defaults would show up as perpetual drift.
	// A null configuration stays null.
	if !data.Config.IsNull() {
		configElement := map[string]attr.Value{}
		for k := range data.Config.Elements() {
			if v, ok := topicInfo.Config[k]; ok {
				configElement[k] = types.StringValue(v)
			}
		}
		data.Config = types.MapValueMust(types.StringType, configElement)
	}

	// Save updated data into Terraform state
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
}

func (r *topicResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Read Terraform plan data into the model
	var data *TopicResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	// Read Terraform state data into the model
	var state *TopicResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	saved, err := newTopicUpdater(r.kafka).update(ctx, *data, *state)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to update topic, got error: %s", err))
	}
	// Save what the cluster has now. After a failed step this is the prior
	// state with the steps that worked already applied.
	resp.Diagnostics.Append(resp.State.Set(ctx, &saved)...)
}

// configMap converts a Terraform map of strings into a plain map, dropping
// null and unknown elements.
func configMap(config types.Map) map[string]string {
	out := make(map[string]string, len(config.Elements()))
	for k, v := range config.Elements() {
		str, ok := v.(types.String)
		if !ok || str.IsNull() || str.IsUnknown() {
			continue
		}
		out[k] = str.ValueString()
	}
	return out
}

func (r *topicResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data *TopicResourceModel

	// Read Terraform prior state data into the model
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	// A topic that is already gone is the desired end state.
	err := r.kafka.DeleteTopics(ctx, data.Name.ValueString())
	if err != nil && !errors.Is(err, kafkaclient.ErrTopicNotFound) {
		resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to delete topic, got error: %s", err))
		return
	}
}

func (r *topicResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

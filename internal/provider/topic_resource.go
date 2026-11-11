package provider

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

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
	kafka "github.com/segmentio/kafka-go"
	"github.com/segmentio/topicctl/pkg/admin"
	"github.com/segmentio/topicctl/pkg/apply/assigners"
	"github.com/segmentio/topicctl/pkg/apply/extenders"
	"github.com/segmentio/topicctl/pkg/apply/pickers"
)

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
type topicResource struct {
	client *admin.BrokerAdminClient
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
				MarkdownDescription: "Topic name",
				Required:            true,
				Validators:          []validator.String{stringvalidator.LengthBetween(1, 249)},
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
				MarkdownDescription: "Topic configuration overrides. Only the keys set here are managed; " +
					"any other topic configuration is left at the broker default.",
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

	client, ok := req.ProviderData.(*admin.BrokerAdminClient)

	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *admin.BrokerAdminClient, got: %T. Please report this issue to the provider developers.", req.ProviderData),
		)

		return
	}

	r.client = client
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
	if plan.ReplicationFactor.IsUnknown() || r.client == nil {
		return
	}
	brokerIDs, err := r.client.GetBrokerIDs(ctx)
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

	// Generate KafkaConfig
	configEntries := configEntries(data.Config)
	topicConfig := kafka.TopicConfig{
		Topic:             data.Name.ValueString(),
		NumPartitions:     int(data.Partitions.ValueInt64()),
		ReplicationFactor: int(data.ReplicationFactor.ValueInt64()),
		ConfigEntries:     configEntries,
	}

	tflog.Info(ctx, fmt.Sprintf("Creating topic %s", data.Name.ValueString()))
	createRequest := kafka.CreateTopicsRequest{
		Topics: []kafka.TopicConfig{topicConfig},
	}
	// We use the internal client create topic, as the external one does not handle Errors in response
	clientResp, err := r.client.GetConnector().KafkaClient.CreateTopics(ctx, &createRequest)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to create topic, got error: %s", err))
		return
	}
	if err := joinErrors(clientResp.Errors, topicName); err != nil {
		resp.Diagnostics.AddError("Client Response Error", fmt.Sprintf("Unable to create topic, got error: %s", err))
		return
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

	topicInfo, err := r.client.GetTopic(ctx, data.ID.ValueString(), true)
	if err != nil {
		if errors.Is(err, admin.ErrTopicDoesNotExist) {
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

// replicaCount returns the replication_factor for a partition
// Returns an error if it cannot determine the count, or if the number of
// replicas is different across partitions
func replicaCount(topicInfo admin.TopicInfo) (int, error) {
	count := -1
	for _, p := range topicInfo.Partitions {
		if count == -1 {
			count = len(p.Replicas)
		}
		if count != len(p.Replicas) {
			return count, fmt.Errorf("the replica count isn't the same across partitions %d != %d", count, len(p.Replicas))
		}
	}
	return count, nil
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

	if !data.Config.Equal(state.Config) {
		tflog.Info(ctx, "Updating topic configuration")
		err := r.updateConfig(ctx, data)
		if err != nil {
			resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to update topic configuration, got error: %s", err))
			return
		}
	}
	if !data.ReplicationFactor.Equal(state.ReplicationFactor) {
		tflog.Info(ctx, "Updating topic replication factor")
		err := r.updateReplicationFactor(ctx, data)
		if err != nil {
			resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to update topic replication factor, got error: %s", err))
			return
		}
	}
	if !data.Partitions.Equal(state.Partitions) {
		tflog.Info(ctx, "Updating topic partitions")
		err := r.updatePartitions(ctx, data)
		if err != nil {
			resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to update topic partitions, got error: %s", err))
			return
		}
	}

	// Save updated data into Terraform state
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *topicResource) updateConfig(ctx context.Context, data *TopicResourceModel) error {
	// Generate KafkaConfig
	configEntries := configEntries(data.Config)

	alterConfigsRequest := kafka.AlterConfigsRequest{
		Resources: []kafka.AlterConfigRequestResource{
			{
				ResourceType: kafka.ResourceTypeTopic,
				ResourceName: data.Name.ValueString(),
				Configs:      configEntriesToAlterConfigs(configEntries),
			},
		},
	}

	clientResp, err := r.client.GetConnector().KafkaClient.AlterConfigs(ctx, &alterConfigsRequest)
	if err != nil {
		return err
	}
	return joinErrors(clientResp.Errors, func(res kafka.AlterConfigsResponseResource) string {
		return fmt.Sprintf("%s %q", kafka.ResourceType(res.Type), res.Name)
	})
}

// joinErrors returns every non-nil error in errs as a single error, or nil if
// there are none. Each error is prefixed with describe(key) so the message
// names the topic or resource it belongs to. Entries are sorted by that
// description so the message is deterministic.
func joinErrors[K comparable](errs map[K]error, describe func(K) string) error {
	type failure struct {
		name string
		err  error
	}
	var failures []failure
	for k, err := range errs {
		if err != nil {
			failures = append(failures, failure{name: describe(k), err: err})
		}
	}
	slices.SortFunc(failures, func(a, b failure) int {
		return cmp.Compare(a.name, b.name)
	})
	wrapped := make([]error, 0, len(failures))
	for _, f := range failures {
		wrapped = append(wrapped, fmt.Errorf("%s: %w", f.name, f.err))
	}
	return errors.Join(wrapped...)
}

// configEntries converts a Terraform map of strings into Kafka config entries.
func configEntries(config types.Map) []kafka.ConfigEntry {
	var entries []kafka.ConfigEntry
	for k, v := range config.Elements() {
		str, ok := v.(types.String)
		if !ok || str.IsNull() || str.IsUnknown() {
			continue
		}
		entries = append(entries, kafka.ConfigEntry{
			ConfigName:  k,
			ConfigValue: str.ValueString(),
		})
	}
	return entries
}

// Generate a AlterConfigRequestConfig from ConfigEntry
func configEntriesToAlterConfigs(
	configEntries []kafka.ConfigEntry,
) []kafka.AlterConfigRequestConfig {
	apiConfigs := []kafka.AlterConfigRequestConfig{}
	for _, entry := range configEntries {
		apiConfigs = append(
			apiConfigs,
			kafka.AlterConfigRequestConfig{
				Name:  entry.ConfigName,
				Value: entry.ConfigValue,
			},
		)
	}
	return apiConfigs
}

func (r *topicResource) updateReplicationFactor(ctx context.Context, data *TopicResourceModel) error {
	brokerIDs, err := r.client.GetBrokerIDs(ctx)
	if err != nil {
		return err
	}

	if data.ReplicationFactor.ValueInt64() > int64(len(brokerIDs)) {
		return fmt.Errorf("replication factor cannot be higher than the number of brokers")
	}

	topicInfo, err := r.client.GetTopic(ctx, data.Name.ValueString(), false)
	if err != nil {
		return err
	}
	brokersInfo, err := r.client.GetBrokers(ctx, brokerIDs)
	if err != nil {
		return err
	}

	// Don't include the topic for this applier since the picker already considers
	// broker placement within the topic and, also, the placement might change during
	// the apply process.
	nonAppliedTopics := []admin.TopicInfo{}
	topics, err := r.client.GetTopics(ctx, nil, false)
	if err != nil {
		return err
	}
	for _, topic := range topics {
		if topic.Name != data.Name.ValueString() {
			nonAppliedTopics = append(
				nonAppliedTopics,
				topic,
			)
		}
	}

	// Use the live replica count, not state, which may be stale
	replicasPresent64, err := replicaCount(topicInfo)
	if err != nil {
		return err
	}
	replicasWanted := data.ReplicationFactor.ValueInt64()
	replicasPresent := int64(replicasPresent64)
	if replicasWanted == replicasPresent {
		return nil
	}

	var newPartitionsInfo []admin.PartitionInfo
	for _, partition := range topicInfo.Partitions {
		if replicasWanted > replicasPresent {
			// Add replicas
			partition.Replicas = increaseReplicas(int(replicasWanted), partition.Replicas, brokerIDs)
			newPartitionsInfo = append(newPartitionsInfo, partition)
		} else if replicasWanted < replicasPresent {
			// Removing replicas
			partition.Replicas = reduceReplicas(int(replicasWanted), partition.Replicas, partition.Leader)
			newPartitionsInfo = append(newPartitionsInfo, partition)
		} else {
			return fmt.Errorf("unable to identify replica factor change type")
		}
	}
	topicInfo.Partitions = newPartitionsInfo
	newAssignments := topicInfo.ToAssignments()

	picker := pickers.NewClusterUsePicker(brokersInfo, nonAppliedTopics)
	assigner := assigners.NewCrossRackAssigner(brokersInfo, picker)

	assignments, err := assigner.Assign(data.Name.ValueString(), newAssignments)
	if err != nil {
		return err
	}

	apiAssignments := []kafka.AlterPartitionReassignmentsRequestAssignment{}
	for _, assignment := range assignments {
		apiAssignment := kafka.AlterPartitionReassignmentsRequestAssignment{
			PartitionID: assignment.ID,
			BrokerIDs:   assignment.Replicas,
		}
		apiAssignments = append(apiAssignments, apiAssignment)
	}
	alterPartitionReassignmentsRequest := kafka.AlterPartitionReassignmentsRequest{
		Topic:       data.Name.ValueString(),
		Assignments: apiAssignments,
	}

	tflog.Info(ctx, fmt.Sprintf("%v", alterPartitionReassignmentsRequest.Assignments))

	clientResp, err := r.client.GetConnector().KafkaClient.AlterPartitionReassignments(ctx, &alterPartitionReassignmentsRequest)
	if err != nil {
		return err
	}
	if clientResp.Error != nil {
		return clientResp.Error
	}
	if len(clientResp.PartitionResults) > 0 {
		partErrors := []error{}
		for _, partResult := range clientResp.PartitionResults {
			if partResult.Error != nil {
				partErrors = append(partErrors, partResult.Error)
			}
		}
		if len(partErrors) > 0 {
			return fmt.Errorf("errors changing replication factor: %s", partErrors)
		}
	}

	// The reassignment is asynchronous. Wait for it to finish so that the
	// next Read and the saved state reflect the cluster, not the plan.
	waitCtx := ctx
	timeout := r.client.GetConnector().KafkaClient.Timeout
	if timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	err = waitForReassignment(waitCtx, reassignmentPollInterval, func(ctx context.Context) (bool, error) {
		return r.replicaReassignmentDone(ctx, data.Name.ValueString(), int(data.ReplicationFactor.ValueInt64()))
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf(
			"timed out after %s waiting for Kafka to finish reassigning replicas of topic %q to replication factor %d; "+
				"the reassignment may still be in progress, so run terraform plan again to refresh the actual state: %w",
			timeout, data.Name.ValueString(), data.ReplicationFactor.ValueInt64(), err,
		)
	}
	if err != nil {
		return fmt.Errorf("waiting for replica reassignment of topic %q: %w", data.Name.ValueString(), err)
	}
	return nil
}

// replicaReassignmentDone reports whether every partition of the topic has the
// desired replica count and no partition reassignment is still in progress.
func (r *topicResource) replicaReassignmentDone(ctx context.Context, topic string, replicationFactor int) (bool, error) {
	topicInfo, err := r.client.GetTopic(ctx, topic, false)
	if err != nil {
		return false, err
	}

	partitionIDs := make([]int, 0, len(topicInfo.Partitions))
	for _, partition := range topicInfo.Partitions {
		if len(partition.Replicas) != replicationFactor {
			return false, nil
		}
		partitionIDs = append(partitionIDs, partition.ID)
	}

	listResp, err := r.client.GetConnector().KafkaClient.ListPartitionReassignments(ctx, &kafka.ListPartitionReassignmentsRequest{
		Topics: map[string]kafka.ListPartitionReassignmentsRequestTopic{
			topic: {PartitionIndexes: partitionIDs},
		},
	})
	if err != nil {
		return false, err
	}
	if listResp.Error != nil {
		return false, listResp.Error
	}
	return len(listResp.Topics[topic].Partitions) == 0, nil
}

// increaseReplicas returns a new replica list grown to the desired size using
// brokers not already hosting a replica. The input slice is not modified.
func increaseReplicas(desired int, replicas []int, brokerIDs []int) []int {
	newReplicas := slices.Clone(replicas)
	for _, id := range brokerIDs {
		if len(newReplicas) >= desired {
			break
		}
		if !slices.Contains(newReplicas, id) {
			newReplicas = append(newReplicas, id)
		}
	}
	return newReplicas
}

// reduceReplicas returns a new replica list shrunk to the desired size. The
// leader is always kept; other replicas are dropped from the end of the list.
// The input slice is not modified.
func reduceReplicas(desired int, replicas []int, leader int) []int {
	newReplicas := slices.Clone(replicas)
	for i := len(newReplicas) - 1; i >= 0 && len(newReplicas) > desired; i-- {
		if newReplicas[i] == leader {
			continue
		}
		newReplicas = slices.Delete(newReplicas, i, i+1)
	}
	return newReplicas
}

func (r *topicResource) updatePartitions(ctx context.Context, data *TopicResourceModel) error {
	brokerIDs, err := r.client.GetBrokerIDs(ctx)
	if err != nil {
		return err
	}
	brokersInfo, err := r.client.GetBrokers(ctx, brokerIDs)
	if err != nil {
		return err
	}
	topicInfo, err := r.client.GetTopic(ctx, data.Name.ValueString(), false)
	if err != nil {
		return err
	}

	// Use the live partition count, not state, which may be stale
	currentPartitions := len(topicInfo.Partitions)
	if int(data.Partitions.ValueInt64()) < currentPartitions {
		return fmt.Errorf("partition count can't be reduced from %d to %d", currentPartitions, data.Partitions.ValueInt64())
	}
	extraPartitions := int(data.Partitions.ValueInt64()) - currentPartitions
	if extraPartitions == 0 {
		return nil
	}

	currAssignments := topicInfo.ToAssignments()
	for _, b := range brokersInfo {
		tflog.Debug(ctx, fmt.Sprintf("Broker ID: %v Rack: %s", b.ID, b.Rack))
	}
	picker := pickers.NewRandomizedPicker()
	extender := extenders.NewBalancedExtender(
		brokersInfo,
		false,
		picker,
	)
	desiredAssignments, err := extender.Extend(
		data.Name.ValueString(),
		currAssignments,
		extraPartitions,
	)
	if err != nil {
		return err
	}
	desiredAssignments = desiredAssignments[len(desiredAssignments)-extraPartitions:]

	tflog.Info(ctx, fmt.Sprintf("Assignments: %v", desiredAssignments))

	err = r.client.AddPartitions(ctx, data.Name.ValueString(), desiredAssignments)
	if err != nil {
		return err
	}

	return nil
}

func (r *topicResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data *TopicResourceModel

	// Read Terraform prior state data into the model
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	clientResp, err := r.client.GetConnector().KafkaClient.DeleteTopics(ctx, &kafka.DeleteTopicsRequest{
		Topics: []string{data.Name.ValueString()},
	})
	if err != nil {
		resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to delete topic, got error: %s", err))
		return
	}
	failures := make(map[string]error, len(clientResp.Errors))
	for name, v := range clientResp.Errors {
		// A topic that is already gone is the desired end state
		if errors.Is(v, kafka.UnknownTopicOrPartition) {
			continue
		}
		failures[name] = v
	}
	if err := joinErrors(failures, topicName); err != nil {
		resp.Diagnostics.AddError("Client Response Error", fmt.Sprintf("Unable to delete topic, got error: %s", err))
		return
	}
}

// topicName describes a topic key of a Kafka response for error messages.
func topicName(name string) string {
	return name
}

func (r *topicResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

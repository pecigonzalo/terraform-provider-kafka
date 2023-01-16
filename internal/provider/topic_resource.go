package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/pecigonzalo/terraform-provider-kafka/internal/modifier"
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
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"partitions": schema.Int64Attribute{
				MarkdownDescription: "Topic partitions count",
				Required:            true,
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"replication_factor": schema.Int64Attribute{
				MarkdownDescription: "Topic replication factor count",
				Required:            true,
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"configuration": schema.MapAttribute{
				MarkdownDescription: "Configuration",
				ElementType:         types.StringType,
				Optional:            true,
				Computed:            true,
				PlanModifiers: []planmodifier.Map{
					mapplanmodifier.UseStateForUnknown(),
					modifier.MapDefaultValue(types.MapValueMust(
						types.StringType,
						map[string]attr.Value{},
					)),
				},
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
			fmt.Sprintf("Expected *http.Client, got: %T. Please report this issue to the provider developers.", req.ProviderData),
		)

		return
	}

	r.client = client
}

func (r *topicResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data *TopicResourceModel

	// Read Terraform plan data into the model
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	// Generate KafkaConfig
	var configEntries []kafka.ConfigEntry
	for k, v := range data.Config.Elements() {
		configEntries = append(configEntries, kafka.ConfigEntry{
			ConfigName: k,
			// TODO: Why do we have to do this ugly remove quotes?
			// Lets try and fix this in the datamodel or similar
			ConfigValue: v.String()[1 : len(v.String())-1],
		})
	}
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
	for _, v := range clientResp.Errors {
		if v != nil {
			resp.Diagnostics.AddError("Client Response Error", fmt.Sprintf("Unable to create topic, got error: %s", v))
			return
		}
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
		switch err {
		case admin.ErrTopicDoesNotExist:
			// If the Topic does not exist, we remove it and return
			resp.State.RemoveResource(ctx)
			return
		default:
			resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to read topic, got error: %s", err))
			return
		}
	}

	replicationFactor, err := replicaCount(topicInfo)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to get replica count, got error: %s", err))
		return
	}

	data.Name = types.StringValue(topicInfo.Name)
	data.Partitions = types.Int64Value(int64(len(topicInfo.Partitions)))
	data.ReplicationFactor = types.Int64Value(int64(replicationFactor))
	configElement := map[string]attr.Value{}
	for k, v := range topicInfo.Config {
		configElement[k] = types.StringValue(v)
	}
	data.Config = types.MapValueMust(
		types.StringType,
		configElement,
	)

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
		err := r.updateConfig(ctx, data, req, resp)
		if err != nil {
			resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to update topic configuration, got error: %s", err))
			return
		}
	}
	if !data.ReplicationFactor.Equal(state.ReplicationFactor) {
		tflog.Info(ctx, "Updating topic replication factor")
		err := r.updateReplicationFactor(ctx, state, data, req, resp)
		if err != nil {
			resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to update topic replication factor, got error: %s", err))
			return
		}
	}
	if !data.Partitions.Equal(state.Partitions) {
		tflog.Info(ctx, "Updating topic partitions")
		err := r.updatePartitions(ctx, state, data, req, resp)
		if err != nil {
			resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to update topic partitions, got error: %s", err))
			return
		}
	}

	// Save updated data into Terraform state
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *topicResource) updateConfig(ctx context.Context, data *TopicResourceModel, req resource.UpdateRequest, resp *resource.UpdateResponse) error {
	// Generate KafkaConfig
	var configEntries []kafka.ConfigEntry
	for k, v := range data.Config.Elements() {
		configEntries = append(configEntries, kafka.ConfigEntry{
			ConfigName: k,
			// TODO: Why do we have to do this ugly remove quotes?
			// Lets try and fix this in the datamodel or similar
			ConfigValue: v.String()[1 : len(v.String())-1],
		})
	}

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
	for _, v := range clientResp.Errors {
		return v
	}
	return nil
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

func (r *topicResource) updateReplicationFactor(ctx context.Context, state *TopicResourceModel, data *TopicResourceModel, req resource.UpdateRequest, resp *resource.UpdateResponse) error {
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

	replicasWanted := data.ReplicationFactor.ValueInt64()
	replicasPresent := state.ReplicationFactor.ValueInt64()

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
		return err
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
	return nil
}

func containsId(id int, ids []int) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func increaseReplicas(desired int, replicas []int, brokerIDs []int) []int {
	newReplicas := replicas
	for _, id := range brokerIDs {
		if !containsId(id, newReplicas) {
			newReplicas = append(newReplicas, id)
		}
		if len(newReplicas) == desired {
			return newReplicas
		}
	}
	return newReplicas
}

func reduceReplicas(desired int, replicas []int, leader int) []int {
	if len(replicas) > desired {
		newReplicas := []int{}
		for i, replica := range replicas {
			if replica == leader {
				continue
			} else {
				newReplicas = append(replicas[:i], replicas[i+1:]...)
			}
		}
		return reduceReplicas(desired, newReplicas, leader)
	} else {
		return replicas
	}
}

func (r *topicResource) updatePartitions(ctx context.Context, state *TopicResourceModel, data *TopicResourceModel, req resource.UpdateRequest, resp *resource.UpdateResponse) error {
	if data.Partitions.ValueInt64() < state.Partitions.ValueInt64() {
		return fmt.Errorf("partition count can't be reduced")
	}

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

	currAssignments := topicInfo.ToAssignments()
	for _, b := range brokersInfo {
		tflog.Debug(ctx, fmt.Sprintf("Broker ID: %v Rack: %s", b.ID, b.Rack))
	}
	extraPartitions := int(data.Partitions.ValueInt64()) - int(state.Partitions.ValueInt64())

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
	for _, v := range clientResp.Errors {
		if v != nil {
			kafkaError := v.(kafka.Error)
			resp.Diagnostics.AddError("Client Response Error", fmt.Sprintf("Unable to delete topic, got error: %s", kafkaError.Error()))
			return
		}
	}
}

func (r *topicResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

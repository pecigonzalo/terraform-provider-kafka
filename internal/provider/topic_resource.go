package provider

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
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
var _ resource.Resource = &TopicResource{}
var _ resource.ResourceWithImportState = &TopicResource{}

func NewTopicResource() resource.Resource {
	return &TopicResource{}
}

// TopicResource defines the resource implementation.
type TopicResource struct {
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

func (r *TopicResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_topic"
}

func (r *TopicResource) GetSchema(ctx context.Context) (tfsdk.Schema, diag.Diagnostics) {
	return tfsdk.Schema{
		// This description is used by the documentation generator and the language server.
		MarkdownDescription: "Kafka Topic resource",

		Attributes: map[string]tfsdk.Attribute{
			"id": {
				MarkdownDescription: "Topic id",
				Type:                types.StringType,
				Computed:            true,
				PlanModifiers: []tfsdk.AttributePlanModifier{
					resource.UseStateForUnknown(),
				},
			},
			"name": {
				MarkdownDescription: "Topic name",
				Type:                types.StringType,
				Required:            true,
				PlanModifiers: []tfsdk.AttributePlanModifier{
					resource.RequiresReplace(),
					resource.UseStateForUnknown(),
				},
			},
			"partitions": {
				MarkdownDescription: "Topic partitions count",
				Type:                types.Int64Type,
				Required:            true,
				PlanModifiers: []tfsdk.AttributePlanModifier{
					resource.UseStateForUnknown(),
				},
			},
			"replication_factor": {
				MarkdownDescription: "Topic replication factor count",
				Type:                types.Int64Type,
				Required:            true,
				PlanModifiers: []tfsdk.AttributePlanModifier{
					resource.UseStateForUnknown(),
				},
			},
			"configuration": {
				MarkdownDescription: "Configuration",
				Type:                types.MapType{ElemType: types.StringType},
				Optional:            true,
				Computed:            true,
				PlanModifiers: []tfsdk.AttributePlanModifier{
					resource.UseStateForUnknown(),
					modifier.DefaultAttribute(types.Map{
						ElemType: types.StringType,
						Elems:    map[string]attr.Value{},
					}),
				},
			},
		},
	}, nil
}

func (r *TopicResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *TopicResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data *TopicResourceModel

	// Read Terraform plan data into the model
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	// Generate KafkaConfig
	var configEntries []kafka.ConfigEntry
	for k, v := range data.Config.Elems {
		configEntries = append(configEntries, kafka.ConfigEntry{
			ConfigName: k,
			// TODO: Why do we have to do this ugly remove quotes?
			// Lets try and fix this in the datamodel or similar
			ConfigValue: v.String()[1 : len(v.String())-1],
		})
	}
	topicConfig := kafka.TopicConfig{
		Topic:             data.Name.Value,
		NumPartitions:     int(data.Partitions.Value),
		ReplicationFactor: int(data.ReplicationFactor.Value),
		ConfigEntries:     configEntries,
	}

	tflog.Info(ctx, fmt.Sprintf("Creating topic %s", data.Name.Value))
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

func (r *TopicResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data *TopicResourceModel

	// Read Terraform prior state data into the model
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	topicInfo, err := r.client.GetTopic(ctx, data.ID.Value, true)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to read topic, got error: %s", err))
		return
	}

	replicationFactor, err := replicaCount(topicInfo)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to get replica count, got error: %s", err))
		return
	}

	data.Name = types.String{Value: topicInfo.Name}
	data.Partitions = types.Int64{Value: int64(len(topicInfo.Partitions))}
	data.ReplicationFactor = types.Int64{Value: int64(replicationFactor)}
	configElement := map[string]attr.Value{}
	for k, v := range topicInfo.Config {
		configElement[k] = types.String{Value: v}
	}
	data.Config = types.Map{
		ElemType: types.StringType,
		Elems:    configElement,
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

func (r *TopicResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
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
		tflog.Debug(ctx, "Updating topic configuration")
		err := r.updateConfig(ctx, data, req, resp)
		if err != nil {
			resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to update topic configuration, got error: %s", err))
			return
		}
	}
	if !data.ReplicationFactor.Equal(state.ReplicationFactor) {
		tflog.Debug(ctx, "Updating topic replication factor")
		err := r.updateReplicationFactor(ctx, state, data, req, resp)
		if err != nil {
			resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to update topic replication factor, got error: %s", err))
			return
		}
	}
	if !data.Partitions.Equal(state.Partitions) {
		tflog.Debug(ctx, "Updating topic partitions")
		err := r.updatePartitions(ctx, state, data, req, resp)
		if err != nil {
			resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to update topic partitions, got error: %s", err))
			return
		}
	}

	// Save updated data into Terraform state
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *TopicResource) updateConfig(ctx context.Context, data *TopicResourceModel, req resource.UpdateRequest, resp *resource.UpdateResponse) error {
	// Generate KafkaConfig
	var configEntries []kafka.ConfigEntry
	for k, v := range data.Config.Elems {
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
				ResourceName: data.Name.Value,
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

func (r *TopicResource) updateReplicationFactor(ctx context.Context, state *TopicResourceModel, data *TopicResourceModel, req resource.UpdateRequest, resp *resource.UpdateResponse) error {
	brokerIDs, err := r.client.GetBrokerIDs(ctx)
	if err != nil {
		return err
	}

	if data.ReplicationFactor.Value > int64(len(brokerIDs)) {
		return fmt.Errorf("replication factor cannot be higher than the number of brokers")
	}

	topicInfo, err := r.client.GetTopic(ctx, data.Name.Value, false)
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
		if topic.Name != data.Name.Value {
			nonAppliedTopics = append(
				nonAppliedTopics,
				topic,
			)
		}
	}

	replicasWanted := data.ReplicationFactor.Value
	replicasPresent := state.ReplicationFactor.Value

	// TODO: From here on, this method is quite ugly, we need to refactor it
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

	assignments, err := assigner.Assign(data.Name.Value, newAssignments)
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
		Topic:       data.Name.Value,
		Assignments: apiAssignments,
		Timeout:     time.Minute * 30, // TODO: Allow for configuration in the provider
	}

	tflog.Info(ctx, fmt.Sprintf("%v", alterPartitionReassignmentsRequest.Assignments))

	clientResp, err := r.client.GetConnector().KafkaClient.AlterPartitionReassignments(ctx, &alterPartitionReassignmentsRequest)
	if err != nil {
		return err
	}
	if clientResp.Error != nil {
		return err
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
		for i, replica := range replicas {
			if replica == leader {
				continue
			} else {
				replicas = append(replicas[:i], replicas[i+1:]...)
			}
		}
		return reduceReplicas(desired, replicas, leader)
	} else {
		return replicas
	}
}

func (r *TopicResource) updatePartitions(ctx context.Context, state *TopicResourceModel, data *TopicResourceModel, req resource.UpdateRequest, resp *resource.UpdateResponse) error {
	if data.Partitions.Value < state.Partitions.Value {
		return fmt.Errorf("partition count can't be reduced")
	}
	extraPartitions := int(data.Partitions.Value) - int(state.Partitions.Value)

	brokerIDs, err := r.client.GetBrokerIDs(ctx)
	if err != nil {
		return err
	}
	brokersInfo, err := r.client.GetBrokers(ctx, brokerIDs)
	if err != nil {
		return err
	}
	topicInfo, err := r.client.GetTopic(ctx, data.Name.Value, false)
	if err != nil {
		return err
	}

	currAssignments := topicInfo.ToAssignments()
	for _, b := range brokersInfo {
		tflog.Warn(ctx, b.Rack)
	}

	picker := pickers.NewRandomizedPicker()

	extender := extenders.NewBalancedExtender(
		brokersInfo,
		true,
		picker,
	)
	desiredAssignments, err := extender.Extend(
		data.Name.Value,
		currAssignments,
		extraPartitions,
	)
	if err != nil {
		return err
	}

	err = r.client.AssignPartitions(ctx, data.Name.Value, desiredAssignments)
	if err != nil {
		return err
	}
	return nil
}

func (r *TopicResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data *TopicResourceModel

	// Read Terraform prior state data into the model
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	clientResp, err := r.client.GetConnector().KafkaClient.DeleteTopics(ctx, &kafka.DeleteTopicsRequest{
		Topics: []string{data.Name.Value},
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

func (r *TopicResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

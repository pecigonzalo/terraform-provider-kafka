package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/boolvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/float64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/objectvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

const (
	// quotaPollInterval is how long to wait between checks that a quota change
	// is visible on the broker.
	quotaPollInterval = 500 * time.Millisecond

	// quotaDefaultTimeout bounds waiting for a change when the client has no
	// timeout configured.
	quotaDefaultTimeout = 5 * time.Minute

	// minQuotaBrokerVersion is the first Kafka release with the quota APIs.
	minQuotaBrokerVersion = "2.6"
)

var (
	_ resource.Resource                     = &quotaResource{}
	_ resource.ResourceWithConfigure        = &quotaResource{}
	_ resource.ResourceWithImportState      = &quotaResource{}
	_ resource.ResourceWithConfigValidators = &quotaResource{}
)

// NewQuotaResource returns the kafka_quota resource.
func NewQuotaResource() resource.Resource {
	return &quotaResource{}
}

// quotaAPI is the part of the Kafka client the quota resource uses.
type quotaAPI interface {
	DescribeQuotas(ctx context.Context, filter kafkaclient.QuotaFilter) ([]kafkaclient.QuotaEntry, error)
	AlterQuotas(ctx context.Context, entity kafkaclient.QuotaEntity, set kafkaclient.QuotaValues, remove []string) error
}

type quotaResource struct {
	client quotaAPI
	// timeout bounds how long a write waits for the broker to report it.
	timeout time.Duration
	// interval is the time between checks while waiting.
	interval time.Duration
}

// quotaResourceModel describes the resource data model.
type quotaResourceModel struct {
	ID       types.String          `tfsdk:"id"`
	User     *quotaEntityPartModel `tfsdk:"user"`
	ClientID *quotaEntityPartModel `tfsdk:"client_id"`
	IP       *quotaEntityPartModel `tfsdk:"ip"`

	ProducerByteRate       types.Float64 `tfsdk:"producer_byte_rate"`
	ConsumerByteRate       types.Float64 `tfsdk:"consumer_byte_rate"`
	RequestPercentage      types.Float64 `tfsdk:"request_percentage"`
	ControllerMutationRate types.Float64 `tfsdk:"controller_mutation_rate"`
	ConnectionCreationRate types.Float64 `tfsdk:"connection_creation_rate"`
}

// limit returns the model field holding the limit for key.
func (m *quotaResourceModel) limit(key kafkaclient.QuotaKey) *types.Float64 {
	switch key {
	case kafkaclient.QuotaProducerByteRate:
		return &m.ProducerByteRate
	case kafkaclient.QuotaConsumerByteRate:
		return &m.ConsumerByteRate
	case kafkaclient.QuotaRequestPercentage:
		return &m.RequestPercentage
	case kafkaclient.QuotaControllerMutationRate:
		return &m.ControllerMutationRate
	case kafkaclient.QuotaConnectionCreationRate:
		return &m.ConnectionCreationRate
	}
	return nil
}

// managedLimits returns the limits set in the model: the ones Terraform manages.
func (m *quotaResourceModel) managedLimits() kafkaclient.QuotaValues {
	values := kafkaclient.QuotaValues{}
	for _, key := range kafkaclient.QuotaKeys() {
		if v := m.limit(key); !v.IsNull() && !v.IsUnknown() {
			values[key] = v.ValueFloat64()
		}
	}
	return values
}

// setLimits replaces the model's limits with values; keys without a value
// become null.
func (m *quotaResourceModel) setLimits(values kafkaclient.QuotaValues) {
	for _, key := range kafkaclient.QuotaKeys() {
		if v, ok := values[key]; ok {
			*m.limit(key) = types.Float64Value(v)
		} else {
			*m.limit(key) = types.Float64Null()
		}
	}
}

func (r *quotaResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_quota"
}

// quotaEntityPartSchema describes one entity component, which is either a
// named entity or the default entity.
func quotaEntityPartSchema(description string, extra ...validator.Object) schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		MarkdownDescription: description,
		Optional:            true,
		Validators:          extra,
		PlanModifiers:       []planmodifier.Object{objectplanmodifier.RequiresReplace()},
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				MarkdownDescription: "Name of the entity. Set either `name` or `default`.",
				Optional:            true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
					stringvalidator.ExactlyOneOf(path.MatchRelative().AtParent().AtName("default")),
				},
			},
			"default": schema.BoolAttribute{
				MarkdownDescription: "Set to `true` to apply the quota to the default entity, which covers every " +
					"entity of this type without a more specific quota. Set either `name` or `default`.",
				Optional:   true,
				Validators: []validator.Bool{boolvalidator.Equals(true)},
			},
		},
	}
}

func quotaLimitSchema(description string, extra ...validator.Float64) schema.Float64Attribute {
	return schema.Float64Attribute{
		MarkdownDescription: description + " Must not be negative. Remove the attribute to remove the limit.",
		Optional:            true,
		Validators:          append([]validator.Float64{float64validator.AtLeast(0)}, extra...),
	}
}

func (r *quotaResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Kafka client quota: the rate limits that apply to one entity (a user, a client id, " +
			"a combination of the two, or an IP address). Requires Kafka 2.6 or later.\n\n" +
			"The entity is fixed: changing it replaces the quota. The limits are updated in place. " +
			"Only the limits set in the configuration are managed; a limit that is set on the broker but not " +
			"in the configuration is left alone.\n\n" +
			"Use `user` and `client_id` together or alone, or `ip` alone. At least one limit is required. " +
			"Kafka only accepts `connection_creation_rate` for an `ip` entity and the other limits for `user` and `client_id` entities.\n\n" +
			"The id has the form `user=alice,client-id=billing` (types in the order `user`, `client-id`, `ip`, " +
			"names URL-escaped, the default entity written as `<default>`), and is what `terraform import` takes.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Quota id, for example `user=alice,client-id=billing`, `ip=10.0.0.1` or " +
					"`user=<default>`. Entity names are URL-escaped.",
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"user": quotaEntityPartSchema(
				"The user (principal) the quota applies to. Can be combined with `client_id`, not with `ip`.",
				objectvalidator.ConflictsWith(path.MatchRoot("ip")),
			),
			"client_id": quotaEntityPartSchema(
				"The client id the quota applies to. Can be combined with `user`, not with `ip`.",
				objectvalidator.ConflictsWith(path.MatchRoot("ip")),
			),
			"ip": quotaEntityPartSchema(
				"The IP address the quota applies to. Cannot be combined with `user` or `client_id`, and only "+
					"supports `connection_creation_rate`.",
				objectvalidator.ConflictsWith(
					path.MatchRoot("producer_byte_rate"), path.MatchRoot("consumer_byte_rate"),
					path.MatchRoot("request_percentage"), path.MatchRoot("controller_mutation_rate"),
				),
			),
			"producer_byte_rate": quotaLimitSchema("Maximum bytes per second the entity may publish, per broker."),
			"consumer_byte_rate": quotaLimitSchema("Maximum bytes per second the entity may fetch, per broker."),
			"request_percentage": quotaLimitSchema("Maximum share of broker request handling time (network and I/O threads) the entity may use, as a percentage."),
			"controller_mutation_rate": quotaLimitSchema("Maximum rate of partition creations and deletions the entity may " +
				"cause, in partitions per second."),
			"connection_creation_rate": quotaLimitSchema("Maximum number of connections per second the entity may create, per broker. "+
				"Only supported with `ip`.", float64validator.AlsoRequires(path.MatchRoot("ip"))),
		},
	}
}

func (r *quotaResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.AtLeastOneOf(
			path.MatchRoot("user"), path.MatchRoot("client_id"), path.MatchRoot("ip"),
		),
		resourcevalidator.AtLeastOneOf(
			path.MatchRoot("producer_byte_rate"), path.MatchRoot("consumer_byte_rate"),
			path.MatchRoot("request_percentage"), path.MatchRoot("controller_mutation_rate"),
			path.MatchRoot("connection_creation_rate"),
		),
	}
}

func (r *quotaResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
	if client == nil {
		resp.Diagnostics.AddError("Missing Kafka client", "The provider did not supply a Kafka client. Please report this issue to the provider developers.")
		return
	}
	r.client = client
	r.timeout = client.Timeout()
	r.interval = quotaPollInterval
}

func (r *quotaResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data *quotaResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	entity := quotaEntityFromModel(data)
	if err := validateQuotaEntityCombination(entity); err != nil {
		resp.Diagnostics.AddError("Invalid quota entity", err.Error())
		return
	}
	id := encodeQuotaID(entity)

	existing, err := describeQuota(ctx, r.client, entity)
	if err != nil {
		addQuotaError(&resp.Diagnostics, "Unable to check for an existing quota", err)
		return
	}
	if existing != nil && len(existing.Values) > 0 {
		resp.Diagnostics.AddError(
			"Quota already exists",
			fmt.Sprintf("The entity %q already has client quotas. To manage them with Terraform, import them with `terraform import` using the id %q.", id, id),
		)
		return
	}

	limits := data.managedLimits()
	tflog.Info(ctx, "Creating quota", map[string]any{"id": id})
	if err := r.client.AlterQuotas(ctx, entity, limits, nil); err != nil {
		addQuotaError(&resp.Diagnostics, "Unable to create quota", err)
		return
	}

	// Save state before waiting: the quota exists now, so if the wait fails
	// Terraform must still know about it. Without this, the next apply would
	// fail with "Quota already exists".
	data.ID = types.StringValue(id)
	resp.Diagnostics.Append(resp.State.Set(ctx, data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.settle(ctx, entity, quotaExpectation{present: limits}); err != nil {
		addQuotaWarning(&resp.Diagnostics, "Quota created but not visible yet", err)
	}
}

func (r *quotaResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data *quotaResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	entity, err := decodeQuotaID(data.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid quota id", err.Error())
		return
	}

	// Only the limits already in state are tracked, so limits set on the broker
	// outside Terraform do not show up as drift. After an import nothing is in
	// state yet and every limit on the broker is adopted.
	imported := data.User == nil && data.ClientID == nil && data.IP == nil
	tracked := data.managedLimits()

	// Right after a write another broker can still answer from older state, so
	// Read retries for a short while before treating a difference as drift or
	// the quota as gone.
	entry, err := r.observe(ctx, entity, quotaExpectation{present: tracked}, readSettleWindow)
	if err != nil {
		addQuotaError(&resp.Diagnostics, "Unable to read quota", err)
		return
	}
	if entry == nil {
		resp.State.RemoveResource(ctx)
		return
	}

	if imported {
		tracked = entry.Values
	} else {
		observed := kafkaclient.QuotaValues{}
		for key := range tracked {
			if v, ok := entry.Values[key]; ok {
				observed[key] = v
			}
		}
		tracked = observed
	}
	data.setEntity(entity)
	data.setLimits(tracked)
	resp.Diagnostics.Append(resp.State.Set(ctx, data)...)
}

func (r *quotaResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state *quotaResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	entity, err := decodeQuotaID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid quota id", err.Error())
		return
	}

	set := plan.managedLimits()
	var remove []string
	for key := range state.managedLimits() {
		if _, kept := set[key]; !kept {
			remove = append(remove, string(key))
		}
	}
	tflog.Info(ctx, "Updating quota", map[string]any{"id": state.ID.ValueString()})
	if err := r.client.AlterQuotas(ctx, entity, set, remove); err != nil {
		addQuotaError(&resp.Diagnostics, "Unable to update quota", err)
		return
	}
	exp := quotaExpectation{present: set}
	for _, k := range remove {
		exp.absent = append(exp.absent, kafkaclient.QuotaKey(k))
	}

	// The broker accepted the change, so save the planned values before
	// waiting. If the wait fails, state still holds the values the broker
	// accepted, and the next refresh shows what the broker reports.
	plan.ID = state.ID
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.settle(ctx, entity, exp); err != nil {
		addQuotaWarning(&resp.Diagnostics, "Quota change not visible yet", err)
	}
}

func (r *quotaResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data *quotaResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	entity, err := decodeQuotaID(data.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid quota id", err.Error())
		return
	}

	managed := data.managedLimits()
	if len(managed) == 0 {
		return
	}
	remove := make([]string, 0, len(managed))
	exp := quotaExpectation{}
	for _, key := range kafkaclient.QuotaKeys() {
		if _, ok := managed[key]; ok {
			remove = append(remove, string(key))
			exp.absent = append(exp.absent, key)
		}
	}
	tflog.Info(ctx, "Deleting quota", map[string]any{"id": data.ID.ValueString()})
	// Removing limits that are already gone succeeds, so an absent quota is not an error.
	if err := r.client.AlterQuotas(ctx, entity, nil, remove); err != nil {
		addQuotaError(&resp.Diagnostics, "Unable to delete quota", err)
		return
	}
	if err := r.settle(ctx, entity, exp); err != nil {
		addQuotaError(&resp.Diagnostics, "Quota removal did not become visible", err)
	}
}

func (r *quotaResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	entity, err := decodeQuotaID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import id", err.Error())
		return
	}
	// Store the canonical form so a differently ordered id imports to the same state.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), encodeQuotaID(entity))...)
}

// quotaExpectation describes what the broker must report: every key in
// present with that value, and none of the keys in absent. A quota that does
// not exist meets the expectation only when nothing is expected to be present.
type quotaExpectation struct {
	present kafkaclient.QuotaValues
	absent  []kafkaclient.QuotaKey
}

func (e quotaExpectation) met(entry *kafkaclient.QuotaEntry) bool {
	if entry == nil {
		return len(e.present) == 0
	}
	for key, want := range e.present {
		if got, ok := entry.Values[key]; !ok || got != want {
			return false
		}
	}
	for _, key := range e.absent {
		if _, ok := entry.Values[key]; ok {
			return false
		}
	}
	return true
}

// describeQuota returns the quota on exactly entity, or nil if there is none.
func describeQuota(ctx context.Context, api quotaAPI, entity kafkaclient.QuotaEntity) (*kafkaclient.QuotaEntry, error) {
	entries, err := api.DescribeQuotas(ctx, kafkaclient.QuotaFilter{Entity: entity, Strict: true})
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if sameQuotaEntity(entries[i].Entity, entity) {
			return &entries[i], nil
		}
	}
	return nil, nil
}

// settle waits until the broker reports the expected state. Writes return once
// the controller accepted them, but the broker answering a later request can
// still be behind, so the change is only treated as done when it is visible.
func (r *quotaResource) settle(ctx context.Context, entity kafkaclient.QuotaEntity, exp quotaExpectation) error {
	timeout := r.timeout
	if timeout <= 0 {
		timeout = quotaDefaultTimeout
	}
	_, err := observeQuota(ctx, r.client, entity, exp, r.interval, timeout)
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s waiting for the broker to report the change; "+
			"the change was accepted, so run terraform plan again to refresh the actual state: %w", timeout, err)
	}
	return err
}

// observe reads the quota for Read. It returns nil, with no error, only when
// the quota is confirmed gone: minAbsentChecks consecutive describes in the
// window found no quota. A difference that survives the window is real drift,
// so the last observation is returned for it.
//
// If the window ends before any describe finished, or before enough absent
// answers, the error does not say the quota is gone, so Read keeps the
// resource in state. A slow broker must not look like a deleted quota.
func (r *quotaResource) observe(ctx context.Context, entity kafkaclient.QuotaEntity, exp quotaExpectation, window time.Duration) (*kafkaclient.QuotaEntry, error) {
	waitCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	var (
		last   *kafkaclient.QuotaEntry // latest quota that was found
		absent int                     // consecutive describes without the quota
	)
	err := pollUntil(waitCtx, r.interval, func(ctx context.Context) (bool, error) {
		entry, err := describeQuota(ctx, r.client, entity)
		if err != nil {
			return false, err
		}
		if entry == nil {
			absent++
			return absent >= minAbsentChecks, nil
		}
		absent = 0
		last = entry
		return exp.met(entry), nil
	})
	windowEnded := errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil
	if err != nil && !windowEnded {
		return nil, err
	}
	switch {
	case absent >= minAbsentChecks:
		return nil, nil
	case absent > 0:
		return nil, fmt.Errorf("quota not found, but the read window ended after %d check(s), so it is not treated as deleted", absent)
	case last == nil:
		return nil, fmt.Errorf("no answer from the broker within %s, so the quota is kept in state: %w", window, context.DeadlineExceeded)
	}
	return last, nil
}

// observeQuota describes the quota until exp is met or timeout passes. It
// returns the last observation, and an error wrapping context.DeadlineExceeded
// if exp was still not met at the end.
func observeQuota(ctx context.Context, api quotaAPI, entity kafkaclient.QuotaEntity, exp quotaExpectation, interval, timeout time.Duration) (*kafkaclient.QuotaEntry, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last *kafkaclient.QuotaEntry
	err := pollUntil(waitCtx, interval, func(ctx context.Context) (bool, error) {
		entry, err := describeQuota(ctx, api, entity)
		if err != nil {
			return false, err
		}
		last = entry
		return exp.met(entry), nil
	})
	return last, err
}

// quotaErrorMessage turns an error from the quota APIs into a diagnostic
// detail that says what to do about it.
func quotaErrorMessage(err error) string {
	switch {
	case errors.Is(err, kafkaclient.ErrQuotasUnsupported):
		return fmt.Sprintf("The Kafka cluster does not offer the client quota APIs, which require Kafka %s or later. "+
			"Upgrade the brokers or remove the kafka_quota resources. Details: %s", minQuotaBrokerVersion, err)
	case errors.Is(err, kafkaclient.ErrQuotaAuthorizationFailed):
		return "The principal the provider authenticates as is not allowed to manage client quotas. " +
			"Grant it DESCRIBE_CONFIGS and ALTER_CONFIGS on the cluster resource. Details: " + err.Error()
	}
	return err.Error()
}

// addQuotaWarning reports err as a warning. It is for a failed wait after the
// broker accepted a change: the change is done, so the apply is not a failure.
func addQuotaWarning(diags *diag.Diagnostics, summary string, err error) {
	diags.AddWarning(summary, quotaErrorMessage(err))
}

func addQuotaError(diags *diag.Diagnostics, summary string, err error) {
	diags.AddError(summary, quotaErrorMessage(err))
}

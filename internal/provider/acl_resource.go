package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

const (
	// aclSettleInterval is how long to wait between checks that an ACL change
	// is visible.
	aclSettleInterval = 250 * time.Millisecond

	// aclDefaultTimeout bounds how long Create and Delete wait for a change to
	// become visible when the client has no timeout configured. Normally the
	// provider timeout is used, see aclResource.settleTimeout.
	aclDefaultTimeout = 5 * time.Minute

	// aclReadSettleWindow bounds how long Read keeps re-describing an ACL that
	// is missing before it is reported as gone.
	aclReadSettleWindow = 3 * time.Second

	// aclIDSeparator separates the fields of an ACL id. It is escaped inside
	// field values, see escapeACLIDField.
	aclIDSeparator = "|"

	// aclIDFields is the number of fields in an ACL id.
	aclIDFields = 7
)

var (
	_ resource.Resource                   = &aclResource{}
	_ resource.ResourceWithConfigure      = &aclResource{}
	_ resource.ResourceWithImportState    = &aclResource{}
	_ resource.ResourceWithValidateConfig = &aclResource{}
)

// aclAPI is the part of the Kafka client the ACL resource uses.
type aclAPI interface {
	Timeout() time.Duration
	CreateACL(ctx context.Context, acl kafkaclient.ACL) error
	DescribeACLs(ctx context.Context, filter kafkaclient.ACLFilter) ([]kafkaclient.ACL, error)
	DeleteACLs(ctx context.Context, filter kafkaclient.ACLFilter) (int, error)
}

// NewACLResource returns the kafka_acl resource.
func NewACLResource() resource.Resource {
	return &aclResource{
		settleInterval: aclSettleInterval,
		readWindow:     aclReadSettleWindow,
	}
}

type aclResource struct {
	client aclAPI

	// Timings for waiting until ACL changes are visible. They are fields so
	// tests can shorten them. Writes wait up to the client's timeout (the
	// provider timeout) instead of a fixed bound, see settleTimeout.
	settleInterval time.Duration
	readWindow     time.Duration
}

// settleTimeout bounds how long Create and Delete wait for a change to become
// visible on the broker that answers describe requests: the provider timeout,
// or aclDefaultTimeout when none is configured.
func (r *aclResource) settleTimeout() time.Duration {
	if t := r.client.Timeout(); t > 0 {
		return t
	}
	return aclDefaultTimeout
}

// ACLResourceModel describes the kafka_acl resource data model.
type ACLResourceModel struct {
	ID                  types.String `tfsdk:"id"`
	ResourceType        types.String `tfsdk:"resource_type"`
	ResourceName        types.String `tfsdk:"resource_name"`
	ResourcePatternType types.String `tfsdk:"resource_pattern_type"`
	Principal           types.String `tfsdk:"principal"`
	Host                types.String `tfsdk:"host"`
	Operation           types.String `tfsdk:"operation"`
	PermissionType      types.String `tfsdk:"permission_type"`
}

func (r *aclResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_acl"
}

func (r *aclResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Kafka ACL resource. Manages a single access control entry. " +
			"Kafka cannot modify an ACL, so changing any attribute destroys the entry and creates a new one.\n\n" +
			"The cluster must have an authorizer configured and the provider's principal must be allowed to " +
			"manage ACLs (the `Alter` operation on the `cluster` resource).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "ACL id: the seven attributes joined by `|` in this order: `resource_type`, " +
					"`resource_name`, `resource_pattern_type`, `principal`, `host`, `operation`, `permission_type`. " +
					"A `|` or `%` inside a value is written as `%7C` or `%25`. " +
					"Example: `topic|orders|literal|User:alice|*|read|allow`.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"resource_type": schema.StringAttribute{
				MarkdownDescription: "Type of resource the ACL applies to: " + enumDocs(aclResourceTypes) + ".",
				Required:            true,
				Validators:          []validator.String{canonicalEnum(kafkaclient.ParseACLResourceType)},
				PlanModifiers:       replace,
			},
			"resource_name": schema.StringAttribute{
				MarkdownDescription: "Name of the resource, for example a topic name or consumer group id. " +
					"Use `*` with a `literal` pattern to match every resource of the type, and `kafka-cluster` " +
					"for the `cluster` resource type.",
				Required:      true,
				PlanModifiers: replace,
			},
			"resource_pattern_type": schema.StringAttribute{
				MarkdownDescription: "How `resource_name` is matched: `literal` (exact name) or `prefixed` " +
					"(names starting with `resource_name`). Defaults to `literal`.",
				Optional:      true,
				Computed:      true,
				Default:       stringdefault.StaticString(string(kafkaclient.ACLPatternTypeLiteral)),
				Validators:    []validator.String{canonicalEnum(kafkaclient.ParseACLPatternType)},
				PlanModifiers: replace,
			},
			"principal": schema.StringAttribute{
				MarkdownDescription: "Principal the ACL applies to, as `Type:name`, for example `User:alice`. " +
					"`User:*` matches every user.",
				Required:      true,
				PlanModifiers: replace,
			},
			"host": schema.StringAttribute{
				MarkdownDescription: "Host the principal is allowed or denied access from. `*` (the default) " +
					"matches every host.",
				Optional:      true,
				Computed:      true,
				Default:       stringdefault.StaticString("*"),
				PlanModifiers: replace,
			},
			"operation": schema.StringAttribute{
				MarkdownDescription: "Operation the ACL allows or denies: " + enumDocs(aclOperations) +
					". Which operations are valid depends on `resource_type`.",
				Required:      true,
				Validators:    []validator.String{canonicalEnum(kafkaclient.ParseACLOperation)},
				PlanModifiers: replace,
			},
			"permission_type": schema.StringAttribute{
				MarkdownDescription: "Whether the ACL allows or denies the operation: `allow` or `deny`.",
				Required:            true,
				Validators:          []validator.String{canonicalEnum(kafkaclient.ParseACLPermissionType)},
				PlanModifiers:       replace,
			},
		},
	}
}

func (r *aclResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
	// A nil *Client must not become a non-nil interface.
	if client != nil {
		r.client = client
	}
}

// ValidateConfig applies the cross-attribute rules Kafka enforces (for
// example which operations a resource type supports), so mistakes fail at
// plan time. Values that are not known yet are not checked.
func (r *aclResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var data ACLResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Defaults are not applied to the config: fill them in for the check.
	if data.ResourcePatternType.IsNull() {
		data.ResourcePatternType = types.StringValue(string(kafkaclient.ACLPatternTypeLiteral))
	}
	if data.Host.IsNull() {
		data.Host = types.StringValue("*")
	}
	acl, known := data.acl()
	if !known {
		return
	}
	if err := acl.Validate(); err != nil {
		resp.Diagnostics.AddError("Invalid ACL", err.Error())
	}
}

func (r *aclResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data ACLResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	acl, _ := data.acl()

	tflog.Info(ctx, "Creating ACL", aclLogFields(acl))
	if err := r.client.CreateACL(ctx, acl); err != nil {
		addACLError(&resp.Diagnostics, "Unable to create ACL", err)
		return
	}
	data.ID = types.StringValue(encodeACLID(acl))

	// Save state before waiting: the ACL exists now, so if the wait fails
	// Terraform must still know about it (the resource is marked tainted).
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.waitForACL(ctx, acl, true, r.settleTimeout()); err != nil {
		addACLError(&resp.Diagnostics, "ACL created but not visible yet", err)
	}
}

func (r *aclResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data ACLResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	acl, _ := data.acl()

	// Brokers learn ACLs asynchronously, so a missing ACL is re-checked for a
	// short while before it is reported as gone.
	err := r.waitForACL(ctx, acl, true, r.readWindow)
	switch {
	case err == nil:
		// Present. The describe filter is exact, so state is already correct.
		resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
	case errors.Is(err, errACLNotSettled):
		tflog.Info(ctx, "ACL no longer exists, removing from state", aclLogFields(acl))
		resp.State.RemoveResource(ctx)
	default:
		addACLError(&resp.Diagnostics, "Unable to read ACL", err)
	}
}

// Update is never called: every attribute requires replacement. It fails
// loudly rather than silently accepting a change it cannot apply.
func (r *aclResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError(
		"ACLs cannot be updated in place",
		"Kafka has no ACL update, so every kafka_acl attribute forces replacement. "+
			"This is a bug in the provider, please report it to the provider developers.",
	)
}

func (r *aclResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data ACLResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	acl, _ := data.acl()

	tflog.Info(ctx, "Deleting ACL", aclLogFields(acl))
	// Deleting an ACL that is already gone deletes nothing and is not an error.
	if _, err := r.client.DeleteACLs(ctx, exactACLFilter(acl)); err != nil {
		addACLError(&resp.Diagnostics, "Unable to delete ACL", err)
		return
	}
	if err := r.waitForACL(ctx, acl, false, r.settleTimeout()); err != nil {
		addACLError(&resp.Diagnostics, "ACL deleted but still visible", err)
	}
}

func (r *aclResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	acl, err := decodeACLID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid ACL import id", err.Error())
		return
	}
	data := ACLResourceModel{
		ID:                  types.StringValue(encodeACLID(acl)),
		ResourceType:        types.StringValue(string(acl.ResourceType)),
		ResourceName:        types.StringValue(acl.ResourceName),
		ResourcePatternType: types.StringValue(string(acl.PatternType)),
		Principal:           types.StringValue(acl.Principal),
		Host:                types.StringValue(acl.Host),
		Operation:           types.StringValue(string(acl.Operation)),
		PermissionType:      types.StringValue(string(acl.PermissionType)),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), data.ID)...)
}

// acl converts the model to a kafkaclient.ACL. known is false when any
// attribute is null or unknown, in which case the ACL is incomplete.
func (m ACLResourceModel) acl() (acl kafkaclient.ACL, known bool) {
	attrs := []types.String{m.ResourceType, m.ResourceName, m.ResourcePatternType, m.Principal, m.Host, m.Operation, m.PermissionType}
	for _, a := range attrs {
		if a.IsNull() || a.IsUnknown() {
			known = false
			break
		}
		known = true
	}
	return kafkaclient.ACL{
		ResourceType:   kafkaclient.ACLResourceType(m.ResourceType.ValueString()),
		ResourceName:   m.ResourceName.ValueString(),
		PatternType:    kafkaclient.ACLPatternType(m.ResourcePatternType.ValueString()),
		Principal:      m.Principal.ValueString(),
		Host:           m.Host.ValueString(),
		Operation:      kafkaclient.ACLOperation(m.Operation.ValueString()),
		PermissionType: kafkaclient.ACLPermissionType(m.PermissionType.ValueString()),
	}, known
}

// exactACLFilter returns a filter that matches only the given ACL.
func exactACLFilter(acl kafkaclient.ACL) kafkaclient.ACLFilter {
	return kafkaclient.ACLFilter(acl)
}

// errACLNotSettled means the ACL did not reach the expected visibility
// (present or absent) before the wait ended.
var errACLNotSettled = errors.New("ACL did not reach the expected state in time")

// waitForACL polls describe until the ACL is visible (present) or no longer
// visible (!present). It stops after timeout or when ctx ends, and returns an
// error wrapping errACLNotSettled if the state was not reached, or the
// describe error if the broker fails.
//
// Brokers learn ACL changes from the controller asynchronously and the client
// may talk to a different broker on each request, so a describe right after a
// change can still show the old state.
func (r *aclResource) waitForACL(ctx context.Context, acl kafkaclient.ACL, present bool, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	filter := exactACLFilter(acl)
	err := pollUntil(waitCtx, r.settleInterval, func(ctx context.Context) (bool, error) {
		found, err := r.client.DescribeACLs(ctx, filter)
		if err != nil {
			return false, err
		}
		return slices.Contains(found, acl) == present, nil
	})
	switch {
	case err == nil:
		return nil
	case waitCtx.Err() != nil && errors.Is(err, waitCtx.Err()) && ctx.Err() == nil:
		return errACLNotSettled
	default:
		return err
	}
}

// aclLogFields describes an ACL for logs. It contains no credentials: ACLs
// hold names and principals only.
func aclLogFields(acl kafkaclient.ACL) map[string]any {
	return map[string]any{
		"resource_type": string(acl.ResourceType),
		"resource_name": acl.ResourceName,
		"pattern_type":  string(acl.PatternType),
		"principal":     acl.Principal,
		"host":          acl.Host,
		"operation":     string(acl.Operation),
		"permission":    string(acl.PermissionType),
	}
}

// addACLError reports err with advice matching its cause.
func addACLError(diags *diag.Diagnostics, summary string, err error) {
	diags.AddError(summary, aclErrorDetail(err))
}

// aclAccess says what a caller does with ACLs, which decides the advice given
// when the broker refuses it.
type aclAccess struct {
	subject   string // resource or data source name, for example "kafka_acl"
	verb      string // "manage" or "read"
	passive   string // "managed" or "read"
	operation string // cluster operation the principal needs
}

var (
	aclManageAccess = aclAccess{subject: "kafka_acl", verb: "manage", passive: "managed", operation: "Alter"}
	aclReadAccess   = aclAccess{subject: "kafka_acls", verb: "read", passive: "read", operation: "Describe"}
)

// aclErrorDetail maps the kafkaclient sentinel errors to actionable advice for
// the kafka_acl resource and keeps the underlying error text.
func aclErrorDetail(err error) string {
	return aclErrorDetailFor(aclManageAccess, err)
}

// aclErrorDetailFor is aclErrorDetail for the given kind of access.
func aclErrorDetailFor(access aclAccess, err error) string {
	switch {
	case errors.Is(err, kafkaclient.ErrSecurityDisabled):
		return "The broker has no authorizer configured, so ACLs cannot be " + access.passive + ". " +
			"Enable one (for example `authorizer.class.name` on the brokers) before using " + access.subject + ".\n\n" +
			"Error: " + err.Error()
	case errors.Is(err, kafkaclient.ErrNotAuthorized):
		return "The provider's principal is not authorized to " + access.verb + " ACLs. " +
			"Grant it the " + access.operation + " operation on the cluster resource.\n\n" +
			"Error: " + err.Error()
	case errors.Is(err, kafkaclient.ErrInvalidACL):
		return "The ACL is not valid: " + err.Error()
	case errors.Is(err, errACLNotSettled):
		return "The broker did not report the change in time; brokers can lag behind the controller. " +
			"Run terraform apply again to re-check it.\n\n" +
			"Error: " + err.Error()
	default:
		return err.Error()
	}
}

// encodeACLID builds the id of an ACL: the seven fields joined by
// aclIDSeparator, each escaped so the separator cannot occur inside a field.
func encodeACLID(acl kafkaclient.ACL) string {
	fields := []string{
		string(acl.ResourceType), acl.ResourceName, string(acl.PatternType),
		acl.Principal, acl.Host, string(acl.Operation), string(acl.PermissionType),
	}
	for i, f := range fields {
		fields[i] = escapeACLIDField(f)
	}
	return strings.Join(fields, aclIDSeparator)
}

// decodeACLID parses an id made by encodeACLID and validates the ACL it
// describes.
func decodeACLID(id string) (kafkaclient.ACL, error) {
	parts := strings.Split(id, aclIDSeparator)
	if len(parts) != aclIDFields {
		return kafkaclient.ACL{}, fmt.Errorf("expected an id of %d fields separated by %q "+
			"(resource_type|resource_name|resource_pattern_type|principal|host|operation|permission_type), "+
			"for example %q, got %d fields in %q",
			aclIDFields, aclIDSeparator, "topic|orders|literal|User:alice|*|read|allow", len(parts), id)
	}
	for i, p := range parts {
		parts[i] = unescapeACLIDField(p)
	}

	var acl kafkaclient.ACL
	var err error
	if acl.ResourceType, err = kafkaclient.ParseACLResourceType(parts[0]); err != nil {
		return kafkaclient.ACL{}, err
	}
	acl.ResourceName = parts[1]
	if acl.PatternType, err = kafkaclient.ParseACLPatternType(parts[2]); err != nil {
		return kafkaclient.ACL{}, err
	}
	acl.Principal = parts[3]
	acl.Host = parts[4]
	if acl.Operation, err = kafkaclient.ParseACLOperation(parts[5]); err != nil {
		return kafkaclient.ACL{}, err
	}
	if acl.PermissionType, err = kafkaclient.ParseACLPermissionType(parts[6]); err != nil {
		return kafkaclient.ACL{}, err
	}
	if err := acl.Validate(); err != nil {
		return kafkaclient.ACL{}, err
	}
	return acl, nil
}

var (
	aclIDEscaper   = strings.NewReplacer("%", "%25", aclIDSeparator, "%7C")
	aclIDUnescaper = strings.NewReplacer("%7C", aclIDSeparator, "%25", "%")
)

func escapeACLIDField(s string) string   { return aclIDEscaper.Replace(s) }
func unescapeACLIDField(s string) string { return aclIDUnescaper.Replace(s) }

// aclResourceTypes and aclOperations list the values named in the schema
// documentation. Validation itself goes through the kafkaclient parse helpers;
// TestACLDocumentedValuesAreValid keeps these lists in step with them.
var (
	aclResourceTypes = []kafkaclient.ACLResourceType{
		kafkaclient.ACLResourceTypeTopic, kafkaclient.ACLResourceTypeGroup,
		kafkaclient.ACLResourceTypeCluster, kafkaclient.ACLResourceTypeTransactionalID,
		kafkaclient.ACLResourceTypeDelegationToken,
	}
	aclOperations = []kafkaclient.ACLOperation{
		kafkaclient.ACLOperationAll, kafkaclient.ACLOperationRead, kafkaclient.ACLOperationWrite,
		kafkaclient.ACLOperationCreate, kafkaclient.ACLOperationDelete, kafkaclient.ACLOperationAlter,
		kafkaclient.ACLOperationDescribe, kafkaclient.ACLOperationClusterAction,
		kafkaclient.ACLOperationDescribeConfigs, kafkaclient.ACLOperationAlterConfigs,
		kafkaclient.ACLOperationIdempotentWrite,
	}
)

func enumDocs[T ~string](values []T) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "`" + string(v) + "`"
	}
	return strings.Join(quoted, ", ")
}

// canonicalEnum validates a string with a kafkaclient parse helper. The helper
// accepts any letter case, but state stores the canonical value, so a
// differently cased value would show up as a permanent difference: only the
// canonical form is accepted.
func canonicalEnum[T ~string](parse func(string) (T, error)) validator.String {
	return enumValidator{check: func(s string) error {
		v, err := parse(s)
		if err != nil {
			return err
		}
		if string(v) != s {
			return fmt.Errorf("value %q must be written as %q", s, string(v))
		}
		return nil
	}}
}

type enumValidator struct {
	check func(string) error
}

func (v enumValidator) Description(context.Context) string {
	return "value must be one of the values documented for this attribute"
}

func (v enumValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v enumValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if err := v.check(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid attribute value", err.Error())
	}
}

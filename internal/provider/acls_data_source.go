package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

var (
	_ datasource.DataSource                   = &aclsDataSource{}
	_ datasource.DataSourceWithConfigure      = &aclsDataSource{}
	_ datasource.DataSourceWithValidateConfig = &aclsDataSource{}
)

// aclDescriber is the part of the Kafka client the kafka_acls data source uses.
type aclDescriber interface {
	DescribeACLs(ctx context.Context, filter kafkaclient.ACLFilter) ([]kafkaclient.ACL, error)
}

// NewACLsDataSource returns the kafka_acls data source.
func NewACLsDataSource() datasource.DataSource {
	return &aclsDataSource{}
}

type aclsDataSource struct {
	client aclDescriber
}

// aclsDataSourceModel describes the kafka_acls data source data model.
type aclsDataSourceModel struct {
	ID                  types.String       `tfsdk:"id"`
	ResourceType        types.String       `tfsdk:"resource_type"`
	ResourceName        types.String       `tfsdk:"resource_name"`
	ResourcePatternType types.String       `tfsdk:"resource_pattern_type"`
	Principal           types.String       `tfsdk:"principal"`
	Host                types.String       `tfsdk:"host"`
	Operation           types.String       `tfsdk:"operation"`
	PermissionType      types.String       `tfsdk:"permission_type"`
	ACLs                []ACLResourceModel `tfsdk:"acls"`
}

func (d *aclsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_acls"
}

func (d *aclsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the Kafka ACL entries that match the given filters. A filter that is not set " +
			"matches everything, so without any filter every ACL on the cluster is returned. " +
			"Kafka has no pagination for ACLs: all matches are returned.\n\n" +
			"The data source reports what the broker answers when it is read. Brokers learn ACLs asynchronously, " +
			"so to read the ACLs of a `kafka_acl` resource created in the same configuration, make the filter " +
			"depend on the resource (for example by referencing one of its attributes) so the read happens after " +
			"the create has settled.\n\n" +
			"The provider's principal needs the `Describe` operation on the `cluster` resource.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Identifier of the data source, derived from the filters. It has no meaning " +
					"outside Terraform; use the `id` of the entries in `acls` to refer to an ACL.",
				Computed: true,
			},
			"resource_type": schema.StringAttribute{
				MarkdownDescription: "Only return ACLs for this type of resource: `any` or " +
					enumDocs(aclResourceTypes) + ". `any` is the same as not setting it.",
				Optional:   true,
				Validators: []validator.String{canonicalEnum(kafkaclient.ParseACLResourceTypeFilter)},
			},
			"resource_name": schema.StringAttribute{
				MarkdownDescription: "Only return ACLs for the resource with this name, for example a topic name. " +
					"With the `literal` pattern type the name must match exactly: ACLs for the `*` wildcard name " +
					"are only returned when `*` is asked for. Use the `match` pattern type to also get the " +
					"wildcard and prefixed ACLs that apply to the name.",
				Optional: true,
			},
			"resource_pattern_type": schema.StringAttribute{
				MarkdownDescription: "Which kind of ACLs `resource_name` selects: `literal` (ACLs written for " +
					"exactly that name), `prefixed` (ACLs written for exactly that prefix), `match` (every ACL " +
					"that applies to the name, including wildcard and prefixed ACLs) or `any` (literal and " +
					"prefixed ACLs with that name). `match` and `any` can only be used in filters, not in " +
					"`kafka_acl`. Not setting it is the same as `any`.",
				Optional:   true,
				Validators: []validator.String{canonicalEnum(kafkaclient.ParseACLPatternTypeFilter)},
			},
			"principal": schema.StringAttribute{
				MarkdownDescription: "Only return ACLs for this principal, as `Type:name`, for example `User:alice`. " +
					"The principal must match exactly: `User:*` only returns ACLs written for every user.",
				Optional: true,
			},
			"host": schema.StringAttribute{
				MarkdownDescription: "Only return ACLs for this host. The host must match exactly: `*` only " +
					"returns ACLs written for every host.",
				Optional: true,
			},
			"operation": schema.StringAttribute{
				MarkdownDescription: "Only return ACLs for this operation: `any` or " + enumDocs(aclOperations) +
					". `any` is the same as not setting it.",
				Optional:   true,
				Validators: []validator.String{canonicalEnum(kafkaclient.ParseACLOperationFilter)},
			},
			"permission_type": schema.StringAttribute{
				MarkdownDescription: "Only return ACLs that `allow` or `deny`, or `any` of them. `any` is the " +
					"same as not setting it.",
				Optional:   true,
				Validators: []validator.String{canonicalEnum(kafkaclient.ParseACLPermissionTypeFilter)},
			},
			"acls": schema.ListNestedAttribute{
				MarkdownDescription: "The matching ACLs, sorted by `id`. Empty when nothing matches.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{
							MarkdownDescription: "ACL id, in the format of the `kafka_acl` resource id and so usable " +
								"with `terraform import`: the seven attributes joined by `|`. " +
								"Example: `topic|orders|literal|User:alice|*|read|allow`.",
							Computed: true,
						},
						"resource_type": schema.StringAttribute{
							MarkdownDescription: "Type of resource the ACL applies to.",
							Computed:            true,
						},
						"resource_name": schema.StringAttribute{
							MarkdownDescription: "Name of the resource.",
							Computed:            true,
						},
						"resource_pattern_type": schema.StringAttribute{
							MarkdownDescription: "How `resource_name` is matched: `literal` or `prefixed`.",
							Computed:            true,
						},
						"principal": schema.StringAttribute{
							MarkdownDescription: "Principal the ACL applies to.",
							Computed:            true,
						},
						"host": schema.StringAttribute{
							MarkdownDescription: "Host the ACL applies to.",
							Computed:            true,
						},
						"operation": schema.StringAttribute{
							MarkdownDescription: "Operation the ACL allows or denies.",
							Computed:            true,
						},
						"permission_type": schema.StringAttribute{
							MarkdownDescription: "Whether the ACL allows or denies the operation.",
							Computed:            true,
						},
					},
				},
			},
		},
	}
}

func (d *aclsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*kafkaclient.ReadOnlyClient)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Data Source Configure Type",
			fmt.Sprintf("Expected *kafkaclient.ReadOnlyClient, got: %T. Please report this issue to the provider developers.", req.ProviderData),
		)
		return
	}
	// A nil *ReadOnlyClient must not become a non-nil interface.
	if client != nil {
		d.client = client
	}
}

// ValidateConfig rejects filters whose fields cannot match the same ACL, so
// the mistake shows at plan time. Filters with values that are not known yet
// are not checked.
func (d *aclsDataSource) ValidateConfig(ctx context.Context, req datasource.ValidateConfigRequest, resp *datasource.ValidateConfigResponse) {
	var data aclsDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	filter, known := data.filter()
	if !known {
		return
	}
	if err := filter.CheckConsistency(); err != nil {
		resp.Diagnostics.AddError("Invalid ACL filter", err.Error())
	}
}

func (d *aclsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data aclsDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if d.client == nil {
		resp.Diagnostics.AddError("Missing Kafka client", "The provider did not supply a Kafka client. Please report this issue to the provider developers.")
		return
	}
	filter, _ := data.filter()
	if err := filter.CheckConsistency(); err != nil {
		resp.Diagnostics.AddError("Invalid ACL filter", err.Error())
		return
	}

	tflog.Debug(ctx, "Reading ACLs", map[string]any{"filter": data.ID})
	found, err := d.client.DescribeACLs(ctx, filter)
	if err != nil {
		resp.Diagnostics.AddError("Unable to read ACLs", aclErrorDetailFor(aclReadAccess, err))
		return
	}

	data.ID = types.StringValue(aclsFilterID(filter))
	data.ACLs = aclModels(found)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// filter converts the configured filters to a kafkaclient.ACLFilter. known is
// false when any filter is unknown, in which case the filter is incomplete.
// Unset filters are left empty, which means "any".
func (m aclsDataSourceModel) filter() (filter kafkaclient.ACLFilter, known bool) {
	attrs := []types.String{m.ResourceType, m.ResourceName, m.ResourcePatternType, m.Principal, m.Host, m.Operation, m.PermissionType}
	known = !slices.ContainsFunc(attrs, types.String.IsUnknown)
	return kafkaclient.ACLFilter{
		ResourceType:   kafkaclient.ACLResourceType(m.ResourceType.ValueString()),
		ResourceName:   m.ResourceName.ValueString(),
		PatternType:    kafkaclient.ACLPatternType(m.ResourcePatternType.ValueString()),
		Principal:      m.Principal.ValueString(),
		Host:           m.Host.ValueString(),
		Operation:      kafkaclient.ACLOperation(m.Operation.ValueString()),
		PermissionType: kafkaclient.ACLPermissionType(m.PermissionType.ValueString()),
	}, known
}

// aclModels converts ACLs to their model, sorted by id so the result is the
// same however the broker orders its answer. It never returns nil, so an
// empty result is an empty list rather than null.
func aclModels(acls []kafkaclient.ACL) []ACLResourceModel {
	models := make([]ACLResourceModel, 0, len(acls))
	for _, acl := range acls {
		models = append(models, ACLResourceModel{
			ID:                  types.StringValue(encodeACLID(acl)),
			ResourceType:        types.StringValue(string(acl.ResourceType)),
			ResourceName:        types.StringValue(acl.ResourceName),
			ResourcePatternType: types.StringValue(string(acl.PatternType)),
			Principal:           types.StringValue(acl.Principal),
			Host:                types.StringValue(acl.Host),
			Operation:           types.StringValue(string(acl.Operation)),
			PermissionType:      types.StringValue(string(acl.PermissionType)),
		})
	}
	slices.SortFunc(models, func(a, b ACLResourceModel) int {
		return strings.Compare(a.ID.ValueString(), b.ID.ValueString())
	})
	return models
}

// aclsFilterIDPrefix starts the id of the kafka_acls data source.
const aclsFilterIDPrefix = "acls"

// aclsFilterID derives the data source id from the filter: the seven filter
// fields joined like an ACL id, so equal filters give equal ids. A filter set
// to "any" is the same as one left unset and gives the same id.
func aclsFilterID(f kafkaclient.ACLFilter) string {
	norm := func(s, anyValue string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == anyValue {
			return ""
		}
		return s
	}
	fields := []string{
		norm(string(f.ResourceType), "any"), f.ResourceName, norm(string(f.PatternType), "any"),
		f.Principal, f.Host, norm(string(f.Operation), "any"), norm(string(f.PermissionType), "any"),
	}
	for i, v := range fields {
		fields[i] = escapeACLIDField(v)
	}
	return aclsFilterIDPrefix + aclIDSeparator + strings.Join(fields, aclIDSeparator)
}

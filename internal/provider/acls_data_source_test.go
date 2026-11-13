package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	fwdatasourceschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

// fakeACLDescriber answers describe with canned ACLs and records the filter.
type fakeACLDescriber struct {
	acls      []kafkaclient.ACL
	err       error
	gotFilter kafkaclient.ACLFilter
	calls     int
}

func (f *fakeACLDescriber) DescribeACLs(_ context.Context, filter kafkaclient.ACLFilter) ([]kafkaclient.ACL, error) {
	f.calls++
	f.gotFilter = filter
	return f.acls, f.err
}

func aclWith(mutate func(*kafkaclient.ACL)) kafkaclient.ACL {
	a := testACL()
	mutate(&a)
	return a
}

// aclsFilterConfig builds a data source config value. Keys missing from set are
// null; a nil entry is an unknown value.
func aclsConfigValue(t *testing.T, set map[string]*string) tftypes.Value {
	t.Helper()
	d := &aclsDataSource{}
	resp := &datasource.SchemaResponse{}
	d.Schema(t.Context(), datasource.SchemaRequest{}, resp)
	typ := resp.Schema.Type().TerraformType(t.Context()).(tftypes.Object)

	vals := map[string]tftypes.Value{}
	for name, at := range typ.AttributeTypes {
		vals[name] = tftypes.NewValue(at, nil)
	}
	for name, v := range set {
		if v == nil {
			vals[name] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
			continue
		}
		vals[name] = tftypes.NewValue(tftypes.String, *v)
	}
	return tftypes.NewValue(typ, vals)
}

func strPtr(s string) *string { return &s }

func TestACLsDataSourceValidateConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  map[string]*string
		wantErr string // empty means valid
	}{
		{"no_filters", nil, ""},
		{"all_filters", map[string]*string{
			"resource_type": strPtr("topic"), "resource_name": strPtr("orders"), "resource_pattern_type": strPtr("literal"),
			"principal": strPtr("User:alice"), "host": strPtr("*"), "operation": strPtr("read"), "permission_type": strPtr("allow"),
		}, ""},
		{"filter_only_pattern_match", map[string]*string{"resource_pattern_type": strPtr("match"), "resource_name": strPtr("orders")}, ""},
		{"filter_only_any_values", map[string]*string{
			"resource_type": strPtr("any"), "resource_pattern_type": strPtr("any"), "operation": strPtr("any"), "permission_type": strPtr("any"),
		}, ""},
		{"unknown_values_are_not_checked", map[string]*string{"resource_type": strPtr("group"), "operation": nil}, ""},
		{"unknown_enum_value", map[string]*string{"operation": strPtr("poke")}, "unknown operation"},
		{"operation_not_valid_for_type", map[string]*string{"resource_type": strPtr("group"), "operation": strPtr("write")}, "not valid for resource type"},
		{"cluster_with_other_name", map[string]*string{"resource_type": strPtr("cluster"), "resource_name": strPtr("x")}, "must be"},
		{"prefixed_wildcard", map[string]*string{"resource_pattern_type": strPtr("prefixed"), "resource_name": strPtr("*")}, "wildcard"},
		{"malformed_principal", map[string]*string{"principal": strPtr("alice")}, "Type:name"},
		{"wildcard_inside_host", map[string]*string{"host": strPtr("10.*")}, "wildcard"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &aclsDataSource{}
			resp := &datasource.ValidateConfigResponse{}
			d.ValidateConfig(t.Context(), datasource.ValidateConfigRequest{
				Config: tfsdk.Config{Schema: schemaOf(t, d), Raw: aclsConfigValue(t, tc.config)},
			}, resp)
			if tc.wantErr == "" {
				assert.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
				return
			}
			require.True(t, resp.Diagnostics.HasError())
			assert.Contains(t, resp.Diagnostics.Errors()[0].Detail(), tc.wantErr)
		})
	}
}

// TestACLsDataSourceAttributeValidators checks the per-attribute validators,
// which the framework runs before ValidateConfig.
func TestACLsDataSourceAttributeValidators(t *testing.T) {
	d := &aclsDataSource{}
	sch := schemaOf(t, d)
	tests := []struct {
		attr    string
		value   string
		wantErr string // empty means valid
	}{
		{"resource_type", "any", ""},
		{"resource_type", "topic", ""},
		{"resource_type", "Topic", `must be written as "topic"`},
		{"resource_type", "queue", "unknown resource type"},
		{"resource_pattern_type", "match", ""},
		{"resource_pattern_type", "any", ""},
		{"resource_pattern_type", "prefixed", ""},
		{"resource_pattern_type", "glob", "unknown pattern type"},
		{"operation", "any", ""},
		{"operation", "poke", "unknown operation"},
		{"permission_type", "any", ""},
		{"permission_type", "maybe", "unknown permission type"},
	}
	for _, tc := range tests {
		t.Run(tc.attr+"_"+tc.value, func(t *testing.T) {
			attr, ok := sch.Attributes[tc.attr].(fwdatasourceschema.StringAttribute)
			require.True(t, ok)
			require.NotEmpty(t, attr.Validators)
			var diags diag.Diagnostics
			for _, v := range attr.Validators {
				resp := &validator.StringResponse{}
				v.ValidateString(t.Context(), validator.StringRequest{
					Path: path.Root(tc.attr), ConfigValue: types.StringValue(tc.value),
				}, resp)
				diags.Append(resp.Diagnostics...)
			}
			if tc.wantErr == "" {
				assert.False(t, diags.HasError(), "%v", diags)
				return
			}
			require.True(t, diags.HasError())
			assert.Contains(t, diags.Errors()[0].Detail(), tc.wantErr)
		})
	}
}

func schemaOf(t *testing.T, d *aclsDataSource) fwdatasourceschema.Schema {
	t.Helper()
	resp := &datasource.SchemaResponse{}
	d.Schema(t.Context(), datasource.SchemaRequest{}, resp)
	return resp.Schema
}

func TestACLsDataSourceRead(t *testing.T) {
	read := aclWith(func(a *kafkaclient.ACL) { a.Operation = kafkaclient.ACLOperationRead })
	write := aclWith(func(a *kafkaclient.ACL) { a.Operation = kafkaclient.ACLOperationWrite })
	other := aclWith(func(a *kafkaclient.ACL) { a.ResourceName = "events"; a.Principal = "User:bob" })

	run := func(t *testing.T, fake *fakeACLDescriber, config map[string]*string) (*datasource.ReadResponse, aclsDataSourceModel) {
		t.Helper()
		d := &aclsDataSource{client: fake}
		sch := schemaOf(t, d)
		resp := &datasource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: aclsConfigValue(t, nil)}}
		d.Read(t.Context(), datasource.ReadRequest{Config: tfsdk.Config{Schema: sch, Raw: aclsConfigValue(t, config)}}, resp)
		var got aclsDataSourceModel
		if !resp.Diagnostics.HasError() {
			diags := resp.State.Get(t.Context(), &got)
			require.False(t, diags.HasError(), "%v", diags)
		}
		return resp, got
	}

	t.Run("sorted_by_id_whatever_the_broker_order", func(t *testing.T) {
		fake := &fakeACLDescriber{acls: []kafkaclient.ACL{write, other, read}}
		resp, got := run(t, fake, nil)
		require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)

		var ids []string
		for _, m := range got.ACLs {
			ids = append(ids, m.ID.ValueString())
		}
		assert.Equal(t, []string{
			"topic|events|literal|User:bob|*|read|allow",
			"topic|orders|literal|User:alice|*|read|allow",
			"topic|orders|literal|User:alice|*|write|allow",
		}, ids)
	})

	t.Run("elements_carry_all_attributes", func(t *testing.T) {
		fake := &fakeACLDescriber{acls: []kafkaclient.ACL{read}}
		_, got := run(t, fake, nil)
		require.Len(t, got.ACLs, 1)
		m := got.ACLs[0]
		assert.Equal(t, "topic", m.ResourceType.ValueString())
		assert.Equal(t, "orders", m.ResourceName.ValueString())
		assert.Equal(t, "literal", m.ResourcePatternType.ValueString())
		assert.Equal(t, "User:alice", m.Principal.ValueString())
		assert.Equal(t, "*", m.Host.ValueString())
		assert.Equal(t, "read", m.Operation.ValueString())
		assert.Equal(t, "allow", m.PermissionType.ValueString())
	})

	t.Run("empty_result_is_an_empty_list", func(t *testing.T) {
		resp, got := run(t, &fakeACLDescriber{}, nil)
		require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
		assert.NotNil(t, got.ACLs)
		assert.Empty(t, got.ACLs)
	})

	t.Run("passes_filters_through_and_unset_means_any", func(t *testing.T) {
		fake := &fakeACLDescriber{}
		run(t, fake, map[string]*string{
			"resource_type": strPtr("topic"), "resource_name": strPtr("orders"), "resource_pattern_type": strPtr("match"),
			"principal": strPtr("User:alice"),
		})
		assert.Equal(t, kafkaclient.ACLFilter{
			ResourceType: kafkaclient.ACLResourceTypeTopic, ResourceName: "orders",
			PatternType: kafkaclient.ACLPatternTypeMatch, Principal: "User:alice",
		}, fake.gotFilter)
	})

	t.Run("id_is_derived_from_the_filter", func(t *testing.T) {
		_, got := run(t, &fakeACLDescriber{}, map[string]*string{"resource_name": strPtr("orders"), "operation": strPtr("read")})
		assert.Equal(t, "acls||orders||||read|", got.ID.ValueString())
	})

	t.Run("inconsistent_filter_is_rejected_without_calling_the_broker", func(t *testing.T) {
		fake := &fakeACLDescriber{}
		resp, _ := run(t, fake, map[string]*string{"resource_type": strPtr("group"), "operation": strPtr("write")})
		require.True(t, resp.Diagnostics.HasError())
		assert.Zero(t, fake.calls)
	})

	t.Run("missing_client", func(t *testing.T) {
		d := &aclsDataSource{}
		sch := schemaOf(t, d)
		resp := &datasource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: aclsConfigValue(t, nil)}}
		d.Read(t.Context(), datasource.ReadRequest{Config: tfsdk.Config{Schema: sch, Raw: aclsConfigValue(t, nil)}}, resp)
		require.True(t, resp.Diagnostics.HasError())
		assert.Equal(t, "Missing Kafka client", resp.Diagnostics.Errors()[0].Summary())
	})
}

func TestACLsDataSourceReadErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"no_authorizer", fmt.Errorf("describe ACLs: %w", kafkaclient.ErrSecurityDisabled), "no authorizer configured, so ACLs cannot be read"},
		{"not_authorized", fmt.Errorf("describe ACLs: %w", kafkaclient.ErrNotAuthorized), "Describe operation on the cluster"},
		{"other", errors.New("boom"), "boom"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &aclsDataSource{client: &fakeACLDescriber{err: tc.err}}
			sch := schemaOf(t, d)
			resp := &datasource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: aclsConfigValue(t, nil)}}
			d.Read(t.Context(), datasource.ReadRequest{Config: tfsdk.Config{Schema: sch, Raw: aclsConfigValue(t, nil)}}, resp)

			require.True(t, resp.Diagnostics.HasError())
			err := resp.Diagnostics.Errors()[0]
			assert.Equal(t, "Unable to read ACLs", err.Summary())
			assert.Contains(t, err.Detail(), tc.want)
		})
	}
}

func TestACLErrorDetailSharedMapping(t *testing.T) {
	// The resource keeps its wording; the data source differs only in the
	// verb and operation that fit reading.
	err := fmt.Errorf("x: %w", kafkaclient.ErrNotAuthorized)
	assert.Contains(t, aclErrorDetailFor(aclManageAccess, err), "not authorized to manage ACLs. Grant it the Alter operation")
	assert.Contains(t, aclErrorDetailFor(aclReadAccess, err), "not authorized to read ACLs. Grant it the Describe operation")
	assert.Equal(t, aclErrorDetailFor(aclManageAccess, err), aclErrorDetail(err))

	err = fmt.Errorf("x: %w", kafkaclient.ErrSecurityDisabled)
	assert.Contains(t, aclErrorDetailFor(aclManageAccess, err), "cannot be managed")
	assert.Contains(t, aclErrorDetailFor(aclReadAccess, err), "cannot be read")
	assert.Contains(t, aclErrorDetailFor(aclReadAccess, err), "before using kafka_acls")
}

func TestACLsFilterID(t *testing.T) {
	tests := []struct {
		name   string
		filter kafkaclient.ACLFilter
		want   string
	}{
		{"empty", kafkaclient.ACLFilter{}, "acls|||||||"},
		{"any_is_unset", kafkaclient.ACLFilter{
			ResourceType: "any", PatternType: "ANY", Operation: "any", PermissionType: "any",
		}, "acls|||||||"},
		{"all_fields", kafkaclient.ACLFilter{
			ResourceType: kafkaclient.ACLResourceTypeTopic, ResourceName: "orders", PatternType: kafkaclient.ACLPatternTypeMatch,
			Principal: "User:alice", Host: "*", Operation: kafkaclient.ACLOperationRead, PermissionType: kafkaclient.ACLPermissionTypeDeny,
		}, "acls|topic|orders|match|User:alice|*|read|deny"},
		{"separator_is_escaped", kafkaclient.ACLFilter{ResourceName: "a|b", Principal: "User:100%"}, "acls||a%7Cb||User:100%25|||"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, aclsFilterID(tc.filter))
		})
	}

	t.Run("different_filters_differ", func(t *testing.T) {
		a := aclsFilterID(kafkaclient.ACLFilter{ResourceName: "a"})
		b := aclsFilterID(kafkaclient.ACLFilter{Principal: "a"})
		assert.NotEqual(t, a, b)
	})
}

func TestACLModelsOrder(t *testing.T) {
	tests := []struct {
		name string
		in   []kafkaclient.ACL
		want []string
	}{
		{"nil", nil, []string{}},
		{"by_type_then_name", []kafkaclient.ACL{
			aclWith(func(a *kafkaclient.ACL) { a.ResourceType = kafkaclient.ACLResourceTypeTopic; a.ResourceName = "b" }),
			aclWith(func(a *kafkaclient.ACL) {
				a.ResourceType = kafkaclient.ACLResourceTypeGroup
				a.ResourceName = "z"
				a.Operation = kafkaclient.ACLOperationRead
			}),
			aclWith(func(a *kafkaclient.ACL) { a.ResourceType = kafkaclient.ACLResourceTypeTopic; a.ResourceName = "a" }),
		}, []string{
			"group|z|literal|User:alice|*|read|allow",
			"topic|a|literal|User:alice|*|read|allow",
			"topic|b|literal|User:alice|*|read|allow",
		}},
		{"deny_sorts_before_allow_for_same_acl_otherwise", []kafkaclient.ACL{
			aclWith(func(a *kafkaclient.ACL) { a.PermissionType = kafkaclient.ACLPermissionTypeDeny }),
			aclWith(func(a *kafkaclient.ACL) {}),
		}, []string{
			"topic|orders|literal|User:alice|*|read|allow",
			"topic|orders|literal|User:alice|*|read|deny",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := []string{}
			for _, m := range aclModels(tc.in) {
				got = append(got, m.ID.ValueString())
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestACLsDataSourceConfigure(t *testing.T) {
	t.Run("accepts_read_only_client", func(t *testing.T) {
		d := &aclsDataSource{}
		resp := &datasource.ConfigureResponse{}
		d.Configure(t.Context(), datasource.ConfigureRequest{ProviderData: &kafkaclient.ReadOnlyClient{}}, resp)
		assert.False(t, resp.Diagnostics.HasError())
		assert.NotNil(t, d.client)
	})
	t.Run("rejects_the_read_write_client", func(t *testing.T) {
		d := &aclsDataSource{}
		resp := &datasource.ConfigureResponse{}
		d.Configure(t.Context(), datasource.ConfigureRequest{ProviderData: &kafkaclient.Client{}}, resp)
		assert.True(t, resp.Diagnostics.HasError())
		assert.Nil(t, d.client)
	})
	t.Run("nil_provider_data_is_ignored", func(t *testing.T) {
		d := &aclsDataSource{}
		resp := &datasource.ConfigureResponse{}
		d.Configure(t.Context(), datasource.ConfigureRequest{}, resp)
		assert.False(t, resp.Diagnostics.HasError())
		assert.Nil(t, d.client)
	})
	t.Run("nil_client_stays_unset", func(t *testing.T) {
		d := &aclsDataSource{}
		resp := &datasource.ConfigureResponse{}
		var c *kafkaclient.ReadOnlyClient
		d.Configure(t.Context(), datasource.ConfigureRequest{ProviderData: c}, resp)
		assert.Nil(t, d.client)
	})
}

// ---- Acceptance tests ----

// testAccACLsConfig creates two ACLs for one topic and principal (read and
// write) and reads them back with the given data source blocks. Every data
// source references an attribute of the second ACL, so it is read after both
// ACLs have been created and have settled.
func testAccACLsConfig(topic, principal, dataSources string) string {
	return providerConfig + fmt.Sprintf(`
resource "kafka_acl" "read" {
  resource_type   = "topic"
  resource_name   = %[1]q
  principal       = %[2]q
  operation       = "read"
  permission_type = "allow"
}

resource "kafka_acl" "write" {
  resource_type   = "topic"
  resource_name   = %[1]q
  principal       = %[2]q
  operation       = "write"
  permission_type = "allow"

  depends_on = [kafka_acl.read]
}
%[3]s
`, topic, principal, dataSources)
}

// testAccACLsCheckIDs fails unless the data source's acls are exactly wantIDs
// in that order, or, with contains, include them.
func testAccACLsCheckIDs(name string, contains bool, wantIDs ...string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[name]
		if !ok {
			return fmt.Errorf("data source %s not found in state", name)
		}
		count, err := strconv.Atoi(rs.Primary.Attributes["acls.#"])
		if err != nil {
			return fmt.Errorf("%s: acls.# is not a number: %w", name, err)
		}
		got := make([]string, 0, count)
		for i := range count {
			got = append(got, rs.Primary.Attributes[fmt.Sprintf("acls.%d.id", i)])
		}
		if contains {
			for _, want := range wantIDs {
				found := false
				for _, g := range got {
					found = found || g == want
				}
				if !found {
					return fmt.Errorf("%s: acl %q not among %d results", name, want, len(got))
				}
			}
			return nil
		}
		if strings.Join(got, "\n") != strings.Join(wantIDs, "\n") {
			return fmt.Errorf("%s: acls = %v, want %v", name, got, wantIDs)
		}
		return nil
	}
}

func TestAccACLsDataSource(t *testing.T) {
	suffix := testAccACLSuffix(t)
	topic := "acls-ds-" + suffix
	principal := "User:acls-ds-" + suffix
	readID := fmt.Sprintf("topic|%s|literal|%s|*|read|allow", topic, principal)
	writeID := fmt.Sprintf("topic|%s|literal|%s|*|write|allow", topic, principal)

	dataSources := fmt.Sprintf(`
data "kafka_acls" "by_resource" {
  resource_type = "topic"
  resource_name = kafka_acl.write.resource_name
}

data "kafka_acls" "by_principal" {
  principal = kafka_acl.write.principal
}

data "kafka_acls" "by_operation" {
  resource_name = kafka_acl.write.resource_name
  operation     = "write"
}

data "kafka_acls" "everything" {
  depends_on = [kafka_acl.write]
}

data "kafka_acls" "nothing" {
  resource_name = "acls-ds-missing-%s"
  depends_on    = [kafka_acl.write]
}
`, suffix)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccACLCheckGone(t, topic),
		Steps: []resource.TestStep{
			{
				Config: testAccACLsConfig(topic, principal, dataSources),
				Check: resource.ComposeAggregateTestCheckFunc(
					// By resource type and name: both ACLs, sorted by id.
					resource.TestCheckResourceAttr("data.kafka_acls.by_resource", "acls.#", "2"),
					testAccACLsCheckIDs("data.kafka_acls.by_resource", false, readID, writeID),
					resource.TestCheckResourceAttr("data.kafka_acls.by_resource", "acls.0.operation", "read"),
					resource.TestCheckResourceAttr("data.kafka_acls.by_resource", "acls.0.principal", principal),
					resource.TestCheckResourceAttr("data.kafka_acls.by_resource", "acls.0.host", "*"),
					resource.TestCheckResourceAttr("data.kafka_acls.by_resource", "acls.0.resource_pattern_type", "literal"),
					resource.TestCheckResourceAttr("data.kafka_acls.by_resource", "id",
						fmt.Sprintf("acls|topic|%s|||||", topic)),
					// By principal.
					testAccACLsCheckIDs("data.kafka_acls.by_principal", false, readID, writeID),
					// By operation.
					testAccACLsCheckIDs("data.kafka_acls.by_operation", false, writeID),
					// No filter returns at least these two.
					testAccACLsCheckIDs("data.kafka_acls.everything", true, readID, writeID),
					// A filter that matches nothing is an empty list, not an error.
					resource.TestCheckResourceAttr("data.kafka_acls.nothing", "acls.#", "0"),
				),
			},
		},
	})
}

func TestAccACLsDataSourceInvalidFilter(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: providerConfig + `
data "kafka_acls" "test" {
  resource_type = "group"
  operation     = "write"
}
`,
				ExpectError: regexp.MustCompile(`not valid for resource type`),
			},
			{
				Config: providerConfig + `
data "kafka_acls" "test" {
  operation = "poke"
}
`,
				ExpectError: regexp.MustCompile(`unknown operation`),
			},
		},
	})
}

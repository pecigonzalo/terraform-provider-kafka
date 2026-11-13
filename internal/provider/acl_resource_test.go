package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	fwresourceschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
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

func testACL() kafkaclient.ACL {
	return kafkaclient.ACL{
		ResourceType:   kafkaclient.ACLResourceTypeTopic,
		ResourceName:   "orders",
		PatternType:    kafkaclient.ACLPatternTypeLiteral,
		Principal:      "User:alice",
		Host:           "*",
		Operation:      kafkaclient.ACLOperationRead,
		PermissionType: kafkaclient.ACLPermissionTypeAllow,
	}
}

func TestACLIDRoundTrip(t *testing.T) {
	tests := map[string]func(a *kafkaclient.ACL){
		"plain":                func(*kafkaclient.ACL) {},
		"separator_in_name":    func(a *kafkaclient.ACL) { a.ResourceName = "a|b" },
		"percent_in_name":      func(a *kafkaclient.ACL) { a.ResourceName = "100%" },
		"escape_lookalike":     func(a *kafkaclient.ACL) { a.ResourceName = "x%7Cy%257C" },
		"separator_in_princ":   func(a *kafkaclient.ACL) { a.Principal = "User:a|b" },
		"prefixed_wildcard_ok": func(a *kafkaclient.ACL) { a.PatternType = kafkaclient.ACLPatternTypePrefixed; a.ResourceName = "app." },
		"specific_host":        func(a *kafkaclient.ACL) { a.Host = "10.0.0.1" },
		"deny_group": func(a *kafkaclient.ACL) {
			a.ResourceType = kafkaclient.ACLResourceTypeGroup
			a.PermissionType = kafkaclient.ACLPermissionTypeDeny
		},
		"cluster": func(a *kafkaclient.ACL) {
			a.ResourceType = kafkaclient.ACLResourceTypeCluster
			a.ResourceName = kafkaclient.ClusterResourceName
			a.Operation = kafkaclient.ACLOperationAlter
		},
		"unicode_name": func(a *kafkaclient.ACL) { a.ResourceName = "café" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			want := testACL()
			mutate(&want)
			id := encodeACLID(want)
			got, err := decodeACLID(id)
			require.NoError(t, err, id)
			assert.Equal(t, want, got)
		})
	}

	t.Run("documented_example", func(t *testing.T) {
		assert.Equal(t, "topic|orders|literal|User:alice|*|read|allow", encodeACLID(testACL()))
	})
}

func TestDecodeACLIDErrors(t *testing.T) {
	tests := map[string]string{
		"empty":               "",
		"too_few_fields":      "topic|orders|literal|User:alice|*|read",
		"too_many_fields":     "topic|orders|literal|User:alice|*|read|allow|extra",
		"unescaped_separator": "topic|a|b|literal|User:alice|*|read|allow",
		"unknown_resource":    "queue|orders|literal|User:alice|*|read|allow",
		"filter_only_pattern": "topic|orders|match|User:alice|*|read|allow",
		"unknown_operation":   "topic|orders|literal|User:alice|*|frobnicate|allow",
		"any_permission":      "topic|orders|literal|User:alice|*|read|any",
		"bad_principal":       "topic|orders|literal|alice|*|read|allow",
		"empty_name":          "topic||literal|User:alice|*|read|allow",
		"operation_for_type":  "group|g|literal|User:alice|*|write|allow",
		"cluster_wrong_name":  "cluster|prod|literal|User:alice|*|alter|allow",
	}
	for name, id := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := decodeACLID(id)
			require.Error(t, err)
		})
	}

	t.Run("field_count_error_shows_format", func(t *testing.T) {
		_, err := decodeACLID("topic|orders")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resource_type|resource_name")
		assert.Contains(t, err.Error(), "topic|orders|literal|User:alice|*|read|allow")
	})
}

func TestCanonicalEnumValidator(t *testing.T) {
	v := canonicalEnum(kafkaclient.ParseACLOperation)
	tests := []struct {
		name    string
		value   types.String
		wantErr bool
	}{
		{"canonical", types.StringValue("read"), false},
		{"wrong_case", types.StringValue("READ"), true},
		{"unknown", types.StringValue("poke"), true},
		{"filter_only_any", types.StringValue("any"), true},
		{"null_skipped", types.StringNull(), false},
		{"unknown_skipped", types.StringUnknown(), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := &validator.StringResponse{}
			v.ValidateString(t.Context(), validator.StringRequest{ConfigValue: tc.value}, resp)
			assert.Equal(t, tc.wantErr, resp.Diagnostics.HasError(), resp.Diagnostics)
		})
	}
}

// TestACLDocumentedValuesAreValid keeps the values named in the schema
// documentation in step with what the kafkaclient parse helpers accept.
func TestACLDocumentedValuesAreValid(t *testing.T) {
	for _, v := range aclResourceTypes {
		_, err := kafkaclient.ParseACLResourceType(string(v))
		assert.NoError(t, err, string(v))
	}
	for _, v := range aclOperations {
		_, err := kafkaclient.ParseACLOperation(string(v))
		assert.NoError(t, err, string(v))
	}
}

func TestACLResourceModelACL(t *testing.T) {
	full := ACLResourceModel{
		ResourceType: types.StringValue("topic"), ResourceName: types.StringValue("orders"),
		ResourcePatternType: types.StringValue("literal"), Principal: types.StringValue("User:alice"),
		Host: types.StringValue("*"), Operation: types.StringValue("read"), PermissionType: types.StringValue("allow"),
	}
	got, known := full.acl()
	assert.True(t, known)
	assert.Equal(t, testACL(), got)

	unknownName := full
	unknownName.ResourceName = types.StringUnknown()
	_, known = unknownName.acl()
	assert.False(t, known, "unknown value")

	nullHost := full
	nullHost.Host = types.StringNull()
	_, known = nullHost.acl()
	assert.False(t, known, "null value")
}

func TestACLErrorDetail(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"no_authorizer", fmt.Errorf("create ACL: %w", kafkaclient.ErrSecurityDisabled), "no authorizer configured"},
		{"not_authorized", fmt.Errorf("create ACL: %w", kafkaclient.ErrNotAuthorized), "not authorized to manage ACLs"},
		{"invalid", fmt.Errorf("create ACL: %w", kafkaclient.ErrInvalidACL), "not valid"},
		{"not_settled", errACLNotSettled, "apply again"},
		{"other", errors.New("boom"), "boom"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Contains(t, aclErrorDetail(tc.err), tc.want)
		})
	}
}

func TestACLResourceConfigure(t *testing.T) {
	t.Run("accepts_client", func(t *testing.T) {
		client, err := kafkaclient.New(t.Context(), kafkaclient.Config{BootstrapServer: "localhost:9092", Timeout: time.Second})
		require.NoError(t, err)
		r := &aclResource{}
		resp := &fwresource.ConfigureResponse{}
		r.Configure(t.Context(), fwresource.ConfigureRequest{ProviderData: client}, resp)
		assert.False(t, resp.Diagnostics.HasError())
		assert.NotNil(t, r.client)
	})
	t.Run("rejects_client_without_connection", func(t *testing.T) {
		r := &aclResource{}
		resp := &fwresource.ConfigureResponse{}
		r.Configure(t.Context(), fwresource.ConfigureRequest{ProviderData: &kafkaclient.Client{}}, resp)
		assert.True(t, resp.Diagnostics.HasError())
		assert.Nil(t, r.client)
	})
	t.Run("rejects_other_types", func(t *testing.T) {
		r := &aclResource{}
		resp := &fwresource.ConfigureResponse{}
		r.Configure(t.Context(), fwresource.ConfigureRequest{ProviderData: &kafkaclient.ReadOnlyClient{}}, resp)
		assert.True(t, resp.Diagnostics.HasError())
		assert.Nil(t, r.client)
	})
	t.Run("nil_provider_data_is_ignored", func(t *testing.T) {
		r := &aclResource{}
		resp := &fwresource.ConfigureResponse{}
		r.Configure(t.Context(), fwresource.ConfigureRequest{}, resp)
		assert.False(t, resp.Diagnostics.HasError())
	})
}

// fakeACLAPI is an in-memory aclAPI. A change becomes visible to describe
// only after lag describe calls, like a broker that has not caught up yet.
type fakeACLAPI struct {
	mu          sync.Mutex
	acls        []kafkaclient.ACL
	lag         int // describe calls that still show the previous state
	pending     int
	before      []kafkaclient.ACL
	describeErr error
	describes   int
	created     []kafkaclient.ACL
	deleted     []kafkaclient.ACLFilter
	timeout     time.Duration
	hidden      bool // created ACLs never become visible, deleted ones never go
	hang        bool // describe waits until its context ends
}

func (f *fakeACLAPI) Timeout() time.Duration { return f.timeout }

func (f *fakeACLAPI) CreateACL(_ context.Context, acl kafkaclient.ACL) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.before = slices.Clone(f.acls)
	f.pending = f.lag
	f.created = append(f.created, acl)
	if f.hidden {
		return nil
	}
	f.acls = append(f.acls, acl)
	return nil
}

func (f *fakeACLAPI) DescribeACLs(ctx context.Context, filter kafkaclient.ACLFilter) ([]kafkaclient.ACL, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.describes++
	if f.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	view := f.acls
	if f.pending > 0 {
		f.pending--
		view = f.before
	}
	var out []kafkaclient.ACL
	for _, a := range view {
		if exactACLFilter(a) == filter {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeACLAPI) DeleteACLs(_ context.Context, filter kafkaclient.ACLFilter) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.before = slices.Clone(f.acls)
	f.pending = f.lag
	f.deleted = append(f.deleted, filter)
	if f.hidden {
		return 0, nil
	}
	n := 0
	f.acls = slices.DeleteFunc(f.acls, func(a kafkaclient.ACL) bool {
		if exactACLFilter(a) == filter {
			n++
			return true
		}
		return false
	})
	return n, nil
}

func newTestACLResource(api aclAPI) *aclResource {
	return &aclResource{client: api, settleInterval: time.Millisecond, readWindow: 50 * time.Millisecond}
}

func TestWaitForACL(t *testing.T) {
	t.Run("present_immediately", func(t *testing.T) {
		api := &fakeACLAPI{acls: []kafkaclient.ACL{testACL()}}
		require.NoError(t, newTestACLResource(api).waitForACL(t.Context(), testACL(), true, time.Second))
		assert.Equal(t, 1, api.describes)
	})

	t.Run("appears_after_lag", func(t *testing.T) {
		api := &fakeACLAPI{lag: 3}
		require.NoError(t, api.CreateACL(t.Context(), testACL()))
		require.NoError(t, newTestACLResource(api).waitForACL(t.Context(), testACL(), true, 2*time.Second))
		assert.Equal(t, 4, api.describes, "three stale describes then one that sees the ACL")
	})

	t.Run("disappears_after_lag", func(t *testing.T) {
		api := &fakeACLAPI{lag: 2, acls: []kafkaclient.ACL{testACL()}}
		_, err := api.DeleteACLs(t.Context(), exactACLFilter(testACL()))
		require.NoError(t, err)
		require.NoError(t, newTestACLResource(api).waitForACL(t.Context(), testACL(), false, 2*time.Second))
		assert.Equal(t, 3, api.describes)
	})

	t.Run("times_out_when_never_visible", func(t *testing.T) {
		api := &fakeACLAPI{}
		err := newTestACLResource(api).waitForACL(t.Context(), testACL(), true, 20*time.Millisecond)
		assert.ErrorIs(t, err, errACLNotSettled)
	})

	t.Run("other_acls_do_not_count", func(t *testing.T) {
		other := testACL()
		other.Operation = kafkaclient.ACLOperationWrite
		api := &fakeACLAPI{acls: []kafkaclient.ACL{other}}
		err := newTestACLResource(api).waitForACL(t.Context(), testACL(), true, 20*time.Millisecond)
		assert.ErrorIs(t, err, errACLNotSettled)
	})

	t.Run("describe_error_is_returned", func(t *testing.T) {
		api := &fakeACLAPI{describeErr: fmt.Errorf("describe ACLs: %w", kafkaclient.ErrNotAuthorized)}
		err := newTestACLResource(api).waitForACL(t.Context(), testACL(), true, time.Second)
		assert.ErrorIs(t, err, kafkaclient.ErrNotAuthorized)
		assert.NotErrorIs(t, err, errACLNotSettled)
		assert.Equal(t, 1, api.describes, "a failing describe is not retried")
	})

	t.Run("cancelled_context_is_not_reported_as_timeout", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := newTestACLResource(&fakeACLAPI{}).waitForACL(ctx, testACL(), true, time.Second)
		assert.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, errACLNotSettled)
	})
}

func TestReadACL(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		api := &fakeACLAPI{acls: []kafkaclient.ACL{testACL()}}
		require.NoError(t, newTestACLResource(api).readACL(t.Context(), testACL()))
	})

	t.Run("absent_a_few_times_then_present_is_kept", func(t *testing.T) {
		api := &fakeACLAPI{lag: minAbsentChecks - 1}
		require.NoError(t, api.CreateACL(t.Context(), testACL()))
		r := newTestACLResource(api)
		r.readWindow = 5 * time.Second
		require.NoError(t, r.readACL(t.Context(), testACL()))
		assert.Equal(t, minAbsentChecks, api.describes)
	})

	t.Run("absent_every_time_is_gone_after_the_required_checks", func(t *testing.T) {
		api := &fakeACLAPI{}
		r := newTestACLResource(api)
		r.readWindow = 5 * time.Second
		err := r.readACL(t.Context(), testACL())
		require.ErrorIs(t, err, errACLGone)
		assert.Equal(t, minAbsentChecks, api.describes)
	})

	t.Run("window_ends_before_enough_checks_keeps_the_acl", func(t *testing.T) {
		r := newTestACLResource(&fakeACLAPI{})
		r.settleInterval = 10 * time.Millisecond
		r.readWindow = 15 * time.Millisecond
		err := r.readACL(t.Context(), testACL())
		require.Error(t, err)
		assert.NotErrorIs(t, err, errACLGone)
	})

	t.Run("a_describe_that_never_finishes_keeps_the_acl", func(t *testing.T) {
		api := &fakeACLAPI{hang: true}
		r := newTestACLResource(api)
		err := r.readACL(t.Context(), testACL())
		require.Error(t, err)
		assert.NotErrorIs(t, err, errACLGone)
	})

	t.Run("describe_error_is_returned", func(t *testing.T) {
		api := &fakeACLAPI{describeErr: fmt.Errorf("describe ACLs: %w", kafkaclient.ErrNotAuthorized)}
		err := newTestACLResource(api).readACL(t.Context(), testACL())
		assert.ErrorIs(t, err, kafkaclient.ErrNotAuthorized)
	})
}

func TestACLReadResourceStates(t *testing.T) {
	read := func(t *testing.T, api *fakeACLAPI) *fwresource.ReadResponse {
		t.Helper()
		r := newTestACLResource(api)
		r.readWindow = 5 * time.Second
		sch := aclSchema(t, r)
		state := tfsdk.State{Schema: sch, Raw: aclModelObject(t, r, testACL())}
		resp := &fwresource.ReadResponse{State: state}
		r.Read(t.Context(), fwresource.ReadRequest{State: state}, resp)
		return resp
	}

	t.Run("present_keeps_the_resource", func(t *testing.T) {
		resp := read(t, &fakeACLAPI{acls: []kafkaclient.ACL{testACL()}})
		require.False(t, resp.Diagnostics.HasError())
		assert.False(t, resp.State.Raw.IsNull())
	})

	t.Run("absent_every_time_removes_the_resource", func(t *testing.T) {
		resp := read(t, &fakeACLAPI{})
		require.False(t, resp.Diagnostics.HasError())
		assert.True(t, resp.State.Raw.IsNull())
	})

	t.Run("a_describe_that_never_finishes_keeps_the_resource", func(t *testing.T) {
		api := &fakeACLAPI{hang: true}
		r := newTestACLResource(api)
		sch := aclSchema(t, r)
		state := tfsdk.State{Schema: sch, Raw: aclModelObject(t, r, testACL())}
		resp := &fwresource.ReadResponse{State: state}
		r.Read(t.Context(), fwresource.ReadRequest{State: state}, resp)
		require.True(t, resp.Diagnostics.HasError())
		assert.False(t, resp.State.Raw.IsNull(), "the resource stays in state")
	})
}

func TestACLSettleTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{"uses_provider_timeout", 42 * time.Second, 42 * time.Second},
		{"unset_uses_default", 0, aclDefaultTimeout},
		{"negative_uses_default", -time.Second, aclDefaultTimeout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestACLResource(&fakeACLAPI{timeout: tc.timeout})
			assert.Equal(t, tc.want, r.settleTimeout())
		})
	}
}

// aclModelObject builds the object value of the resource model for acl.
func aclModelObject(t *testing.T, r *aclResource, acl kafkaclient.ACL) tftypes.Value {
	t.Helper()
	return tftypes.NewValue(aclSchema(t, r).Type().TerraformType(t.Context()), map[string]tftypes.Value{
		"id":                    tftypes.NewValue(tftypes.String, encodeACLID(acl)),
		"resource_type":         tftypes.NewValue(tftypes.String, string(acl.ResourceType)),
		"resource_name":         tftypes.NewValue(tftypes.String, acl.ResourceName),
		"resource_pattern_type": tftypes.NewValue(tftypes.String, string(acl.PatternType)),
		"principal":             tftypes.NewValue(tftypes.String, acl.Principal),
		"host":                  tftypes.NewValue(tftypes.String, acl.Host),
		"operation":             tftypes.NewValue(tftypes.String, string(acl.Operation)),
		"permission_type":       tftypes.NewValue(tftypes.String, string(acl.PermissionType)),
	})
}

func aclSchema(t *testing.T, r *aclResource) fwresourceschema.Schema {
	t.Helper()
	resp := &fwresource.SchemaResponse{}
	r.Schema(t.Context(), fwresource.SchemaRequest{}, resp)
	return resp.Schema
}

// TestACLWritesWaitUpToProviderTimeout shows Create and Delete are bounded by
// the client timeout: with a change that never becomes visible they stop after
// that timeout with advice to apply again.
func TestACLWritesWaitUpToProviderTimeout(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		api := &fakeACLAPI{hidden: true, timeout: 30 * time.Millisecond}
		r := newTestACLResource(api)
		sch := aclSchema(t, r)
		plan := tfsdk.Plan{Schema: sch, Raw: aclModelObject(t, r, testACL())}
		resp := &fwresource.CreateResponse{State: tfsdk.State{Schema: sch}}
		r.Create(t.Context(), fwresource.CreateRequest{Plan: plan}, resp)

		require.True(t, resp.Diagnostics.HasError())
		assert.Contains(t, resp.Diagnostics.Errors()[0].Detail(), "apply again")
		assert.Greater(t, api.describes, 1, "describe was retried until the timeout")
		assert.False(t, resp.State.Raw.IsNull(), "state is saved before waiting")
	})

	t.Run("delete", func(t *testing.T) {
		api := &fakeACLAPI{hidden: true, timeout: 30 * time.Millisecond, acls: []kafkaclient.ACL{testACL()}}
		r := newTestACLResource(api)
		sch := aclSchema(t, r)
		state := tfsdk.State{Schema: sch, Raw: aclModelObject(t, r, testACL())}
		resp := &fwresource.DeleteResponse{State: state}
		r.Delete(t.Context(), fwresource.DeleteRequest{State: state}, resp)

		require.True(t, resp.Diagnostics.HasError())
		assert.Contains(t, resp.Diagnostics.Errors()[0].Detail(), "apply again")
		assert.Greater(t, api.describes, 1)
	})

	t.Run("create_succeeds_when_visible", func(t *testing.T) {
		api := &fakeACLAPI{lag: 3, timeout: 2 * time.Second}
		r := newTestACLResource(api)
		sch := aclSchema(t, r)
		plan := tfsdk.Plan{Schema: sch, Raw: aclModelObject(t, r, testACL())}
		resp := &fwresource.CreateResponse{State: tfsdk.State{Schema: sch}}
		r.Create(t.Context(), fwresource.CreateRequest{Plan: plan}, resp)

		require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
		assert.Equal(t, 4, api.describes)
	})

	t.Run("cancelled_context_is_not_advice_to_apply_again", func(t *testing.T) {
		api := &fakeACLAPI{hidden: true, timeout: time.Minute}
		r := newTestACLResource(api)
		sch := aclSchema(t, r)
		plan := tfsdk.Plan{Schema: sch, Raw: aclModelObject(t, r, testACL())}
		resp := &fwresource.CreateResponse{State: tfsdk.State{Schema: sch}}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		r.Create(ctx, fwresource.CreateRequest{Plan: plan}, resp)

		require.True(t, resp.Diagnostics.HasError())
		assert.NotContains(t, resp.Diagnostics.Errors()[0].Detail(), "apply again")
	})
}

// ---- Acceptance tests ----

// testAccACLSuffix returns a name fragment unique to one test run, so tests do
// not collide with each other or with leftovers.
func testAccACLSuffix(t *testing.T) string {
	t.Helper()
	suffix, err := randomSuffix()
	require.NoError(t, err)
	return suffix
}

func testAccACLConfig(name, principal, operation string) string {
	return providerConfig + fmt.Sprintf(`
resource "kafka_acl" "test" {
  resource_type = "topic"
  resource_name = %[1]q
  principal     = %[2]q
  operation     = %[3]q

  permission_type = "allow"
}
`, name, principal, operation)
}

// testAccACLClient connects to the acceptance cluster.
func testAccACLClient(t *testing.T) *kafkaclient.Client {
	t.Helper()
	bootstrap, _, _ := strings.Cut(os.Getenv("KAFKA_BOOTSTRAP_SERVERS"), ",")
	client, err := kafkaclient.New(t.Context(), kafkaclient.Config{BootstrapServer: bootstrap, Timeout: 30 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// testAccACLCheckGone returns a check that fails unless no ACL for the topic
// is left. Brokers learn about deletions asynchronously, so it polls.
func testAccACLCheckGone(t *testing.T, topic string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		client := testAccACLClient(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var left []kafkaclient.ACL
		err := pollUntil(ctx, 200*time.Millisecond, func(ctx context.Context) (bool, error) {
			var err error
			left, err = client.DescribeACLs(ctx, kafkaclient.ACLFilter{
				ResourceType: kafkaclient.ACLResourceTypeTopic, ResourceName: topic,
				PatternType: kafkaclient.ACLPatternTypeLiteral,
			})
			return err == nil && len(left) == 0, err
		})
		if err != nil {
			return fmt.Errorf("ACLs for topic %q still exist: %v (%w)", topic, left, err)
		}
		return nil
	}
}

// testAccACLWaitGone waits until describe reports no ACL for the topic several
// times in a row. A single empty answer is not enough: each request can reach
// a broker that has not yet learned about the deletion.
func testAccACLWaitGone(t *testing.T, client *kafkaclient.Client, topic string) {
	t.Helper()
	const consecutive = 10
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	empty := 0
	err := pollUntil(ctx, 100*time.Millisecond, func(ctx context.Context) (bool, error) {
		found, err := client.DescribeACLs(ctx, kafkaclient.ACLFilter{
			ResourceType: kafkaclient.ACLResourceTypeTopic, ResourceName: topic,
			PatternType: kafkaclient.ACLPatternTypeLiteral,
		})
		if err != nil {
			return false, err
		}
		if len(found) == 0 {
			empty++
		} else {
			empty = 0
		}
		return empty >= consecutive, nil
	})
	require.NoError(t, err, "ACLs for topic %q did not disappear", topic)
}

func TestAccACLResource(t *testing.T) {
	suffix := testAccACLSuffix(t)
	topic := "acl-test-" + suffix
	principal := "User:acl-" + suffix
	wantID := fmt.Sprintf("topic|%s|literal|%s|*|read|allow", topic, principal)
	replacedID := fmt.Sprintf("topic|%s|literal|%s|*|write|allow", topic, principal)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccACLCheckGone(t, topic),
		Steps: []resource.TestStep{
			// Create and read, with defaults applied.
			{
				Config: testAccACLConfig(topic, principal, "read"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("kafka_acl.test", "id", wantID),
					resource.TestCheckResourceAttr("kafka_acl.test", "resource_pattern_type", "literal"),
					resource.TestCheckResourceAttr("kafka_acl.test", "host", "*"),
					resource.TestCheckResourceAttr("kafka_acl.test", "operation", "read"),
				),
			},
			// Import by id.
			{
				ResourceName:      "kafka_acl.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
			// Changing an attribute replaces the ACL.
			{
				Config: testAccACLConfig(topic, principal, "write"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("kafka_acl.test", "id", replacedID),
					func(*terraform.State) error {
						// The old entry must be gone, not left behind.
						client := testAccACLClient(t)
						found, err := client.DescribeACLs(t.Context(), kafkaclient.ACLFilter{
							ResourceType: kafkaclient.ACLResourceTypeTopic, ResourceName: topic,
							PatternType: kafkaclient.ACLPatternTypeLiteral,
						})
						if err != nil {
							return err
						}
						for _, a := range found {
							if a.Operation == kafkaclient.ACLOperationRead {
								return fmt.Errorf("read ACL still exists after replacement: %v", a)
							}
						}
						return nil
					},
				),
			},
			// Destroy happens automatically, then CheckDestroy verifies it is gone.
		},
	})
}

func TestAccACLResourceDeletedOutsideTerraform(t *testing.T) {
	suffix := testAccACLSuffix(t)
	topic := "acl-gone-" + suffix
	principal := "User:acl-" + suffix

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccACLCheckGone(t, topic),
		Steps: []resource.TestStep{
			{Config: testAccACLConfig(topic, principal, "read")},
			// Remove the ACL behind Terraform's back: the refresh must drop it
			// from state and the plan must recreate it.
			{
				PreConfig: func() {
					client := testAccACLClient(t)
					n, err := client.DeleteACLs(t.Context(), kafkaclient.ACLFilter{
						ResourceType: kafkaclient.ACLResourceTypeTopic, ResourceName: topic,
						PatternType: kafkaclient.ACLPatternTypeLiteral,
					})
					require.NoError(t, err)
					require.Equal(t, 1, n)
					testAccACLWaitGone(t, client, topic)
				},
				Config: testAccACLConfig(topic, principal, "read"),
				Check:  resource.TestCheckResourceAttr("kafka_acl.test", "operation", "read"),
			},
		},
	})
}

func TestAccACLResourceInvalid(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      testAccACLConfig("t", "User:a", "poke"),
				ExpectError: regexp.MustCompile(`unknown operation`),
			},
			{
				// Valid enum values that Kafka rejects together.
				Config: providerConfig + `
resource "kafka_acl" "test" {
  resource_type   = "group"
  resource_name   = "g"
  principal       = "User:a"
  operation       = "write"
  permission_type = "allow"
}
`,
				ExpectError: regexp.MustCompile(`not valid for resource type`),
			},
			{
				Config:        testAccACLConfig("t", "User:a", "read"),
				ResourceName:  "kafka_acl.test",
				ImportState:   true,
				ImportStateId: "topic|only-two",
				ExpectError:   regexp.MustCompile(`Invalid ACL import id`),
			},
		},
	})
}

package kafkaclient

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/protocol/createacls"
	"github.com/segmentio/kafka-go/protocol/deleteacls"
	"github.com/segmentio/kafka-go/protocol/describeacls"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeACLRoundTripper serves canned ACL responses and records the requests.
type fakeACLRoundTripper struct {
	createResp   *createacls.Response
	describeResp *describeacls.Response
	deleteResp   *deleteacls.Response
	err          error

	gotCreate   *createacls.Request
	gotDescribe *describeacls.Request
	gotDelete   *deleteacls.Request
}

func (f *fakeACLRoundTripper) RoundTrip(_ context.Context, _ net.Addr, req kafka.Request) (kafka.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	switch r := req.(type) {
	case *createacls.Request:
		f.gotCreate = r
		return f.createResp, nil
	case *describeacls.Request:
		f.gotDescribe = r
		return f.describeResp, nil
	case *deleteacls.Request:
		f.gotDelete = r
		return f.deleteResp, nil
	}
	return nil, errors.New("unexpected request")
}

func newACLTestClient(t *testing.T, rt *fakeACLRoundTripper) *Client {
	t.Helper()
	c, err := newClient(t.Context(), Config{BootstrapServer: "b:9092"}, fakeAWSLoader)
	require.NoError(t, err)
	c.kc.Transport = rt
	return c
}

func validACL() ACL {
	return ACL{
		ResourceType: ACLResourceTypeTopic, ResourceName: "orders", PatternType: ACLPatternTypeLiteral,
		Principal: "User:alice", Host: "*", Operation: ACLOperationRead, PermissionType: ACLPermissionTypeAllow,
	}
}

func TestParseACLEnums(t *testing.T) {
	tests := []struct {
		name    string
		parse   func(string) (string, error)
		input   string
		want    string
		wantErr bool
	}{}
	wrap := func(f func(string) (ACLResourceType, error)) func(string) (string, error) {
		return func(s string) (string, error) { v, err := f(s); return string(v), err }
	}
	wrapP := func(f func(string) (ACLPatternType, error)) func(string) (string, error) {
		return func(s string) (string, error) { v, err := f(s); return string(v), err }
	}
	wrapO := func(f func(string) (ACLOperation, error)) func(string) (string, error) {
		return func(s string) (string, error) { v, err := f(s); return string(v), err }
	}
	wrapM := func(f func(string) (ACLPermissionType, error)) func(string) (string, error) {
		return func(s string) (string, error) { v, err := f(s); return string(v), err }
	}
	add := func(name string, p func(string) (string, error), in, want string, wantErr bool) {
		tests = append(tests, struct {
			name    string
			parse   func(string) (string, error)
			input   string
			want    string
			wantErr bool
		}{name, p, in, want, wantErr})
	}
	for _, v := range []string{"topic", "group", "cluster", "transactional_id", "delegation_token"} {
		add("resource_"+v, wrap(ParseACLResourceType), v, v, false)
	}
	add("resource_case", wrap(ParseACLResourceType), " TOPIC ", "topic", false)
	add("resource_any_rejected", wrap(ParseACLResourceType), "any", "", true)
	add("resource_invalid", wrap(ParseACLResourceType), "broker", "", true)
	add("resource_filter_any", wrap(ParseACLResourceTypeFilter), "Any", "any", false)

	add("pattern_literal", wrapP(ParseACLPatternType), "literal", "literal", false)
	add("pattern_prefixed_case", wrapP(ParseACLPatternType), "PREFIXED", "prefixed", false)
	add("pattern_match_rejected", wrapP(ParseACLPatternType), "match", "", true)
	add("pattern_any_rejected", wrapP(ParseACLPatternType), "any", "", true)
	add("pattern_filter_match", wrapP(ParseACLPatternTypeFilter), "match", "match", false)
	add("pattern_filter_any", wrapP(ParseACLPatternTypeFilter), "any", "any", false)
	add("pattern_invalid", wrapP(ParseACLPatternType), "glob", "", true)

	for _, v := range []string{"all", "read", "write", "create", "delete", "alter", "describe",
		"cluster_action", "describe_configs", "alter_configs", "idempotent_write"} {
		add("op_"+v, wrapO(ParseACLOperation), v, v, false)
	}
	add("op_case", wrapO(ParseACLOperation), "Describe_Configs", "describe_configs", false)
	add("op_any_rejected", wrapO(ParseACLOperation), "any", "", true)
	add("op_filter_any", wrapO(ParseACLOperationFilter), "any", "any", false)
	add("op_invalid", wrapO(ParseACLOperation), "execute", "", true)
	add("op_empty", wrapO(ParseACLOperation), "", "", true)

	add("perm_allow", wrapM(ParseACLPermissionType), "allow", "allow", false)
	add("perm_deny_case", wrapM(ParseACLPermissionType), "DENY", "deny", false)
	add("perm_any_rejected", wrapM(ParseACLPermissionType), "any", "", true)
	add("perm_filter_any", wrapM(ParseACLPermissionTypeFilter), "any", "any", false)
	add("perm_invalid", wrapM(ParseACLPermissionType), "maybe", "", true)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.parse(tc.input)
			if tc.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, ErrInvalidACL)
				assert.Contains(t, err.Error(), "valid values are")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseErrorListsValidValues(t *testing.T) {
	_, err := ParseACLPermissionType("maybe")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow, deny")
	assert.NotContains(t, err.Error(), "any")
}

func TestKafkaMappingRoundTrip(t *testing.T) {
	for _, rt := range resourceTypeTable {
		for _, op := range operationTable {
			acl := ACL{
				ResourceType: rt.value, ResourceName: "n", PatternType: ACLPatternTypePrefixed,
				Principal: "User:a", Host: "h", Operation: op.value, PermissionType: ACLPermissionTypeDeny,
			}
			if rt.filter || op.filter {
				continue
			}
			// Bypass Validate (operation/resource combos): map directly.
			r, _ := enumCode("r", acl.ResourceType, resourceTypeTable)
			o, _ := enumCode("o", acl.Operation, operationTable)
			got, err := aclFromKafka(kafka.ResourceType(r), "n", kafka.PatternTypePrefixed, "User:a", "h",
				kafka.ACLOperationType(o), kafka.ACLPermissionTypeDeny)
			require.NoError(t, err)
			assert.Equal(t, acl, got)
		}
	}
}

func TestACLToKafka(t *testing.T) {
	entry, err := validACL().toKafka()
	require.NoError(t, err)
	assert.Equal(t, kafka.ACLEntry{
		ResourceType: kafka.ResourceTypeTopic, ResourceName: "orders", ResourcePatternType: kafka.PatternTypeLiteral,
		Principal: "User:alice", Host: "*", Operation: kafka.ACLOperationTypeRead, PermissionType: kafka.ACLPermissionTypeAllow,
	}, entry)
}

func TestACLValidate(t *testing.T) {
	mod := func(f func(*ACL)) ACL { a := validACL(); f(&a); return a }
	tests := []struct {
		name    string
		acl     ACL
		wantErr bool
	}{
		{"valid_topic", validACL(), false},
		{"valid_prefixed", mod(func(a *ACL) { a.PatternType = ACLPatternTypePrefixed; a.ResourceName = "ord" }), false},
		{"valid_wildcard_literal", mod(func(a *ACL) { a.ResourceName = "*" }), false},
		{"valid_wildcard_principal", mod(func(a *ACL) { a.Principal = "User:*" }), false},
		{"valid_cluster", mod(func(a *ACL) {
			a.ResourceType = ACLResourceTypeCluster
			a.ResourceName = ClusterResourceName
			a.Operation = ACLOperationClusterAction
		}), false},
		{"valid_group", mod(func(a *ACL) { a.ResourceType = ACLResourceTypeGroup; a.Operation = ACLOperationRead }), false},
		{"empty_resource_name", mod(func(a *ACL) { a.ResourceName = "" }), true},
		{"empty_principal", mod(func(a *ACL) { a.Principal = "" }), true},
		{"principal_without_type", mod(func(a *ACL) { a.Principal = "alice" }), true},
		{"principal_empty_name", mod(func(a *ACL) { a.Principal = "User:" }), true},
		{"empty_host", mod(func(a *ACL) { a.Host = "" }), true},
		{"partial_wildcard_host", mod(func(a *ACL) { a.Host = "10.0.*" }), true},
		{"network_range_host", mod(func(a *ACL) { a.Host = "10.0.0.0/8" }), true},
		{"ipv6_host", mod(func(a *ACL) { a.Host = "2001:db8::1" }), false},
		{"lower_case_user_principal", mod(func(a *ACL) { a.Principal = "user:alice" }), true},
		{"custom_principal_type", mod(func(a *ACL) { a.Principal = "Group:admins" }), false},
		{"prefixed_wildcard", mod(func(a *ACL) { a.PatternType = ACLPatternTypePrefixed; a.ResourceName = "*" }), true},
		{"cluster_wrong_name", mod(func(a *ACL) {
			a.ResourceType = ACLResourceTypeCluster
			a.ResourceName = "my-cluster"
			a.Operation = ACLOperationAlter
		}), true},
		{"cluster_prefixed", mod(func(a *ACL) {
			a.ResourceType = ACLResourceTypeCluster
			a.ResourceName = ClusterResourceName
			a.PatternType = ACLPatternTypePrefixed
			a.Operation = ACLOperationAlter
		}), true},
		{"operation_invalid_for_resource", mod(func(a *ACL) { a.ResourceType = ACLResourceTypeGroup; a.Operation = ACLOperationWrite }), true},
		{"operation_any", mod(func(a *ACL) { a.Operation = ACLOperationAny }), true},
		{"pattern_match", mod(func(a *ACL) { a.PatternType = ACLPatternTypeMatch }), true},
		{"permission_any", mod(func(a *ACL) { a.PermissionType = ACLPermissionTypeAny }), true},
		{"resource_any", mod(func(a *ACL) { a.ResourceType = ACLResourceTypeAny }), true},
		{"zero_value", ACL{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.acl.Validate()
			if tc.wantErr {
				require.ErrorIs(t, err, ErrInvalidACL)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestCreateACL(t *testing.T) {
	t.Run("success_sends_mapped_entry", func(t *testing.T) {
		rt := &fakeACLRoundTripper{createResp: &createacls.Response{Results: []createacls.ResponseACLs{{}}}}
		require.NoError(t, newACLTestClient(t, rt).CreateACL(t.Context(), validACL()))
		require.NotNil(t, rt.gotCreate)
		require.Len(t, rt.gotCreate.Creations, 1)
		got := rt.gotCreate.Creations[0]
		assert.Equal(t, int8(kafka.ResourceTypeTopic), got.ResourceType)
		assert.Equal(t, "orders", got.ResourceName)
		assert.Equal(t, int8(kafka.PatternTypeLiteral), got.ResourcePatternType)
		assert.Equal(t, "User:alice", got.Principal)
		assert.Equal(t, "*", got.Host)
		assert.Equal(t, int8(kafka.ACLOperationTypeRead), got.Operation)
		assert.Equal(t, int8(kafka.ACLPermissionTypeAllow), got.PermissionType)
	})

	t.Run("validation_failure_sends_nothing", func(t *testing.T) {
		rt := &fakeACLRoundTripper{}
		a := validACL()
		a.Principal = ""
		err := newACLTestClient(t, rt).CreateACL(t.Context(), a)
		require.ErrorIs(t, err, ErrInvalidACL)
		assert.Nil(t, rt.gotCreate)
	})

	errTests := []struct {
		name     string
		rt       *fakeACLRoundTripper
		wantIs   []error
		wantNot  []error
		wantText string
	}{
		{
			name:   "per_acl_not_authorized",
			rt:     &fakeACLRoundTripper{createResp: &createacls.Response{Results: []createacls.ResponseACLs{{ErrorCode: int16(kafka.ClusterAuthorizationFailed), ErrorMessage: "denied"}}}},
			wantIs: []error{ErrNotAuthorized, kafka.ClusterAuthorizationFailed},
		},
		{
			name:    "per_acl_security_disabled",
			rt:      &fakeACLRoundTripper{createResp: &createacls.Response{Results: []createacls.ResponseACLs{{ErrorCode: int16(kafka.SecurityDisabled)}}}},
			wantIs:  []error{ErrSecurityDisabled, kafka.SecurityDisabled},
			wantNot: []error{ErrNotAuthorized},
		},
		{
			name:    "per_acl_other_error",
			rt:      &fakeACLRoundTripper{createResp: &createacls.Response{Results: []createacls.ResponseACLs{{ErrorCode: int16(kafka.InvalidRequest)}}}},
			wantIs:  []error{kafka.InvalidRequest},
			wantNot: []error{ErrNotAuthorized, ErrSecurityDisabled},
		},
		{
			name:     "call_error_wrapped",
			rt:       &fakeACLRoundTripper{err: kafka.ClusterAuthorizationFailed},
			wantIs:   []error{ErrNotAuthorized},
			wantText: "create ACL",
		},
		{
			name:     "wrong_result_count",
			rt:       &fakeACLRoundTripper{createResp: &createacls.Response{}},
			wantText: "0 results",
		},
	}
	for _, tc := range errTests {
		t.Run(tc.name, func(t *testing.T) {
			err := newACLTestClient(t, tc.rt).CreateACL(t.Context(), validACL())
			require.Error(t, err)
			for _, w := range tc.wantIs {
				require.ErrorIs(t, err, w)
			}
			for _, w := range tc.wantNot {
				require.NotErrorIs(t, err, w)
			}
			if tc.wantText != "" {
				assert.Contains(t, err.Error(), tc.wantText)
			}
		})
	}
}

func TestDescribeACLs(t *testing.T) {
	t.Run("multiple_resources_flattened", func(t *testing.T) {
		rt := &fakeACLRoundTripper{describeResp: &describeacls.Response{Resources: []describeacls.Resource{
			{
				ResourceType: int8(kafka.ResourceTypeTopic), ResourceName: "orders", PatternType: int8(kafka.PatternTypeLiteral),
				ACLs: []describeacls.ResponseACL{
					{Principal: "User:alice", Host: "*", Operation: int8(kafka.ACLOperationTypeRead), PermissionType: int8(kafka.ACLPermissionTypeAllow)},
					{Principal: "User:bob", Host: "10.0.0.1", Operation: int8(kafka.ACLOperationTypeWrite), PermissionType: int8(kafka.ACLPermissionTypeDeny)},
				},
			},
			{
				ResourceType: int8(kafka.ResourceTypeGroup), ResourceName: "app-", PatternType: int8(kafka.PatternTypePrefixed),
				ACLs: []describeacls.ResponseACL{
					{Principal: "User:carol", Host: "*", Operation: int8(kafka.ACLOperationTypeAll), PermissionType: int8(kafka.ACLPermissionTypeAllow)},
				},
			},
		}}}
		got, err := newACLTestClient(t, rt).ReadOnly().DescribeACLs(t.Context(), ACLFilter{ResourceType: ACLResourceTypeTopic, Principal: "User:alice"})
		require.NoError(t, err)
		require.Len(t, got, 3)
		assert.Equal(t, ACL{ACLResourceTypeTopic, "orders", ACLPatternTypeLiteral, "User:alice", "*", ACLOperationRead, ACLPermissionTypeAllow}, got[0])
		assert.Equal(t, ACL{ACLResourceTypeTopic, "orders", ACLPatternTypeLiteral, "User:bob", "10.0.0.1", ACLOperationWrite, ACLPermissionTypeDeny}, got[1])
		assert.Equal(t, ACL{ACLResourceTypeGroup, "app-", ACLPatternTypePrefixed, "User:carol", "*", ACLOperationAll, ACLPermissionTypeAllow}, got[2])

		// The filter was mapped, with empty enums sent as "any".
		f := rt.gotDescribe.Filter
		assert.Equal(t, int8(kafka.ResourceTypeTopic), f.ResourceTypeFilter)
		assert.Equal(t, "User:alice", f.PrincipalFilter)
		assert.Equal(t, int8(kafka.PatternTypeAny), f.ResourcePatternTypeFilter)
		assert.Equal(t, int8(kafka.ACLOperationTypeAny), f.Operation)
		assert.Equal(t, int8(kafka.ACLPermissionTypeAny), f.PermissionType)
	})

	t.Run("empty_result", func(t *testing.T) {
		rt := &fakeACLRoundTripper{describeResp: &describeacls.Response{}}
		got, err := newACLTestClient(t, rt).DescribeACLs(t.Context(), ACLFilter{})
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("response_error_security_disabled", func(t *testing.T) {
		rt := &fakeACLRoundTripper{describeResp: &describeacls.Response{ErrorCode: int16(kafka.SecurityDisabled), ErrorMessage: "no authorizer"}}
		_, err := newACLTestClient(t, rt).DescribeACLs(t.Context(), ACLFilter{})
		require.ErrorIs(t, err, ErrSecurityDisabled)
		assert.ErrorIs(t, err, kafka.SecurityDisabled)
	})

	t.Run("response_error_not_authorized", func(t *testing.T) {
		rt := &fakeACLRoundTripper{describeResp: &describeacls.Response{ErrorCode: int16(kafka.ClusterAuthorizationFailed)}}
		_, err := newACLTestClient(t, rt).DescribeACLs(t.Context(), ACLFilter{})
		require.ErrorIs(t, err, ErrNotAuthorized)
	})

	t.Run("call_error", func(t *testing.T) {
		rt := &fakeACLRoundTripper{err: errors.New("boom")}
		_, err := newACLTestClient(t, rt).DescribeACLs(t.Context(), ACLFilter{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "boom")
	})

	t.Run("unsupported_broker_value", func(t *testing.T) {
		rt := &fakeACLRoundTripper{describeResp: &describeacls.Response{Resources: []describeacls.Resource{{
			ResourceType: 99, ResourceName: "x", PatternType: int8(kafka.PatternTypeLiteral),
			ACLs: []describeacls.ResponseACL{{Principal: "User:a", Host: "*", Operation: 3, PermissionType: 3}},
		}}}}
		_, err := newACLTestClient(t, rt).DescribeACLs(t.Context(), ACLFilter{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported")
	})

	t.Run("invalid_filter_sends_nothing", func(t *testing.T) {
		rt := &fakeACLRoundTripper{}
		_, err := newACLTestClient(t, rt).DescribeACLs(t.Context(), ACLFilter{Operation: "execute"})
		require.ErrorIs(t, err, ErrInvalidACL)
		assert.Nil(t, rt.gotDescribe)
	})
}

func TestDeleteACLs(t *testing.T) {
	match := func(code kafka.Error) deleteacls.MatchingACL {
		return deleteacls.MatchingACL{
			ErrorCode:    int16(code),
			ResourceType: int8(kafka.ResourceTypeTopic), ResourceName: "orders", ResourcePatternType: int8(kafka.PatternTypeLiteral),
			Principal: "User:alice", Host: "*", Operation: int8(kafka.ACLOperationTypeRead), PermissionType: int8(kafka.ACLPermissionTypeAllow),
		}
	}
	filter := ACLFilter{ResourceType: ACLResourceTypeTopic, ResourceName: "orders", PatternType: ACLPatternTypeLiteral, Principal: "User:alice"}

	t.Run("counts_deleted", func(t *testing.T) {
		rt := &fakeACLRoundTripper{deleteResp: &deleteacls.Response{FilterResults: []deleteacls.FilterResult{
			{MatchingACLs: []deleteacls.MatchingACL{match(0), match(0), match(0)}},
		}}}
		n, err := newACLTestClient(t, rt).DeleteACLs(t.Context(), filter)
		require.NoError(t, err)
		assert.Equal(t, 3, n)
		require.Len(t, rt.gotDelete.Filters, 1)
		f := rt.gotDelete.Filters[0]
		assert.Equal(t, int8(kafka.ResourceTypeTopic), f.ResourceTypeFilter)
		assert.Equal(t, "orders", f.ResourceNameFilter)
		assert.Equal(t, int8(kafka.PatternTypeLiteral), f.ResourcePatternTypeFilter)
		assert.Equal(t, int8(kafka.ACLOperationTypeAny), f.Operation)
	})

	t.Run("none_matched", func(t *testing.T) {
		rt := &fakeACLRoundTripper{deleteResp: &deleteacls.Response{FilterResults: []deleteacls.FilterResult{{}}}}
		n, err := newACLTestClient(t, rt).DeleteACLs(t.Context(), filter)
		require.NoError(t, err)
		assert.Zero(t, n)
	})

	t.Run("filter_result_error", func(t *testing.T) {
		rt := &fakeACLRoundTripper{deleteResp: &deleteacls.Response{FilterResults: []deleteacls.FilterResult{
			{ErrorCode: int16(kafka.ClusterAuthorizationFailed)},
		}}}
		n, err := newACLTestClient(t, rt).DeleteACLs(t.Context(), filter)
		require.ErrorIs(t, err, ErrNotAuthorized)
		assert.Zero(t, n)
	})

	t.Run("matching_acl_error_counts_only_successes", func(t *testing.T) {
		rt := &fakeACLRoundTripper{deleteResp: &deleteacls.Response{FilterResults: []deleteacls.FilterResult{
			{MatchingACLs: []deleteacls.MatchingACL{match(0), match(kafka.SecurityDisabled)}},
		}}}
		n, err := newACLTestClient(t, rt).DeleteACLs(t.Context(), filter)
		require.ErrorIs(t, err, ErrSecurityDisabled)
		assert.Equal(t, 1, n)
	})

	t.Run("call_error", func(t *testing.T) {
		rt := &fakeACLRoundTripper{err: kafka.SecurityDisabled}
		_, err := newACLTestClient(t, rt).DeleteACLs(t.Context(), filter)
		require.ErrorIs(t, err, ErrSecurityDisabled)
	})

	t.Run("match_all_filter_rejected", func(t *testing.T) {
		for name, f := range map[string]ACLFilter{
			"zero":      {},
			"explicit":  {ResourceType: ACLResourceTypeAny, PatternType: ACLPatternTypeAny, Operation: ACLOperationAny, PermissionType: ACLPermissionTypeAny},
			"mixed_any": {ResourceType: "ANY"},
		} {
			rt := &fakeACLRoundTripper{}
			_, err := newACLTestClient(t, rt).DeleteACLs(t.Context(), f)
			require.ErrorIs(t, err, ErrInvalidACL, name)
			assert.Nil(t, rt.gotDelete, name)
		}
	})

	t.Run("invalid_filter_rejected", func(t *testing.T) {
		for name, f := range map[string]ACLFilter{
			"bad_pattern":   {PatternType: "glob", ResourceName: "x"},
			"bad_principal": {Principal: "alice"},
			"bad_host":      {Host: "1.*"},
		} {
			rt := &fakeACLRoundTripper{}
			_, err := newACLTestClient(t, rt).DeleteACLs(t.Context(), f)
			require.ErrorIs(t, err, ErrInvalidACL, name)
			assert.Nil(t, rt.gotDelete, name)
		}
	})
}

func TestACLFilterCheckConsistency(t *testing.T) {
	tests := []struct {
		name    string
		filter  ACLFilter
		wantErr string // empty means valid
	}{
		{"empty", ACLFilter{}, ""},
		{"topic_read", ACLFilter{ResourceType: ACLResourceTypeTopic, Operation: ACLOperationRead}, ""},
		{"any_resource_type_any_operation", ACLFilter{ResourceType: ACLResourceTypeAny, Operation: ACLOperationIdempotentWrite}, ""},
		{"operation_without_resource_type", ACLFilter{Operation: ACLOperationClusterAction}, ""},
		{"cluster_default_name", ACLFilter{ResourceType: ACLResourceTypeCluster, ResourceName: ClusterResourceName}, ""},
		{"cluster_match_pattern", ACLFilter{ResourceType: ACLResourceTypeCluster, PatternType: ACLPatternTypeMatch}, ""},
		{"prefixed_topic", ACLFilter{ResourceType: ACLResourceTypeTopic, ResourceName: "app.", PatternType: ACLPatternTypePrefixed}, ""},
		{"cluster_other_name", ACLFilter{ResourceType: ACLResourceTypeCluster, ResourceName: "x"}, "must be"},
		{"cluster_prefixed", ACLFilter{ResourceType: ACLResourceTypeCluster, PatternType: ACLPatternTypePrefixed}, "no prefixed ACLs"},
		{"prefixed_wildcard", ACLFilter{ResourceName: "*", PatternType: ACLPatternTypePrefixed}, "wildcard"},
		{"group_write", ACLFilter{ResourceType: ACLResourceTypeGroup, Operation: ACLOperationWrite}, "not valid for resource type"},
		{"topic_cluster_action", ACLFilter{ResourceType: ACLResourceTypeTopic, Operation: ACLOperationClusterAction}, "not valid for resource type"},
		{"bad_principal", ACLFilter{Principal: "alice"}, "Type:name"},
		{"bad_host", ACLFilter{Host: "10.*"}, "wildcard"},
		{"unknown_enum", ACLFilter{Operation: "poke"}, "unknown operation"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.filter.CheckConsistency()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrInvalidACL)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestValidateHost(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		wantErr string // empty means valid
	}{
		{"wildcard", "*", ""},
		{"ipv4", "10.0.0.1", ""},
		{"ipv4_loopback", "127.0.0.1", ""},
		{"ipv6", "2001:db8::1", ""},
		{"ipv6_loopback", "::1", ""},
		{"ipv4_mapped_ipv6", "::ffff:10.0.0.1", ""},
		{"empty", "", "must not be empty"},
		{"ipv4_range", "10.0.0.0/8", "does not support network ranges"},
		{"ipv6_range", "2001:db8::/32", "does not support network ranges"},
		{"partial_wildcard", "10.0.*", "does not support network ranges"},
		{"host_name", "broker1.example.com", "must be \"*\" or an IP address"},
		{"ipv4_too_few_parts", "10.0.0", "must be \"*\" or an IP address"},
		{"ipv4_too_many_parts", "10.0.0.1.5", "must be \"*\" or an IP address"},
		{"ipv4_out_of_range", "256.0.0.1", "must be \"*\" or an IP address"},
		{"with_port", "10.0.0.1:9092", "must be \"*\" or an IP address"},
		{"with_space", " 10.0.0.1", "must be \"*\" or an IP address"},
		{"bracketed_ipv6", "[::1]", "must be \"*\" or an IP address"},
		{"two_wildcards", "**", "must be \"*\" or an IP address"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHost(tc.host)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrInvalidACL)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestValidatePrincipal(t *testing.T) {
	tests := []struct {
		name      string
		principal string
		wantErr   string // empty means valid
	}{
		{"user", "User:alice", ""},
		{"user_wildcard", "User:*", ""},
		{"custom_type", "Group:admins", ""},
		{"custom_type_lower_case", "service:billing", ""},
		{"name_with_colon", "User:CN=alice:team", ""},
		{"name_with_spaces", "User:CN=Alice Smith,OU=Dev", ""},
		{"empty", "", "must not be empty"},
		{"no_colon", "alice", "Type:name"},
		{"empty_type", ":alice", "Type:name"},
		{"empty_name", "User:", "Type:name"},
		{"only_colon", ":", "Type:name"},
		{"lower_case_user", "user:alice", `write "User:alice"`},
		{"upper_case_user", "USER:alice", `write "User:alice"`},
		{"mixed_case_user", "uSER:alice:team", `write "User:alice:team"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePrincipal(tc.principal)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrInvalidACL)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestACLFilterUsesHostAndPrincipalRules(t *testing.T) {
	tests := []struct {
		name    string
		filter  ACLFilter
		wantErr bool
	}{
		{"ip_host", ACLFilter{Host: "10.0.0.1"}, false},
		{"range_host", ACLFilter{Host: "10.0.0.0/8"}, true},
		{"user_principal", ACLFilter{Principal: "User:alice"}, false},
		{"wrong_case_principal", ACLFilter{Principal: "user:alice"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.filter.Validate()
			if tc.wantErr {
				assert.ErrorIs(t, err, ErrInvalidACL)
				return
			}
			assert.NoError(t, err)
		})
	}
}

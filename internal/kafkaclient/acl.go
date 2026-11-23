package kafkaclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"

	"github.com/segmentio/kafka-go"
)

// Sentinel errors returned (wrapped) by the ACL operations. Match them with
// [errors.Is].
var (
	// ErrNotAuthorized means the broker refused the request because the
	// authenticated principal lacks permission (any *AuthorizationFailed
	// Kafka error code).
	ErrNotAuthorized = errors.New("kafkaclient: not authorized")

	// ErrSecurityDisabled means the broker has no authorizer configured, so
	// ACLs cannot be managed (Kafka error SECURITY_DISABLED).
	ErrSecurityDisabled = errors.New("kafkaclient: broker has no authorizer configured (security disabled)")

	// ErrInvalidACL means an ACL or ACL filter failed client-side validation
	// and was not sent to the broker.
	ErrInvalidACL = errors.New("kafkaclient: invalid ACL")
)

// ClusterResourceName is the only resource name Kafka accepts for the
// cluster resource type.
const ClusterResourceName = "kafka-cluster"

// ACLResourceType is the kind of resource an ACL applies to.
type ACLResourceType string

// ACL resource types. [ACLResourceTypeAny] is only valid in filters.
const (
	ACLResourceTypeAny             ACLResourceType = "any"
	ACLResourceTypeTopic           ACLResourceType = "topic"
	ACLResourceTypeGroup           ACLResourceType = "group"
	ACLResourceTypeCluster         ACLResourceType = "cluster"
	ACLResourceTypeTransactionalID ACLResourceType = "transactional_id"
	ACLResourceTypeDelegationToken ACLResourceType = "delegation_token"
)

// ACLPatternType is how an ACL's resource name is matched.
type ACLPatternType string

// ACL pattern types. [ACLPatternTypeAny] and [ACLPatternTypeMatch] are only
// valid in filters.
const (
	ACLPatternTypeAny      ACLPatternType = "any"
	ACLPatternTypeMatch    ACLPatternType = "match"
	ACLPatternTypeLiteral  ACLPatternType = "literal"
	ACLPatternTypePrefixed ACLPatternType = "prefixed"
)

// ACLOperation is the operation an ACL allows or denies. [ACLOperationAny]
// is only valid in filters.
type ACLOperation string

// ACL operations.
const (
	ACLOperationAny             ACLOperation = "any"
	ACLOperationAll             ACLOperation = "all"
	ACLOperationRead            ACLOperation = "read"
	ACLOperationWrite           ACLOperation = "write"
	ACLOperationCreate          ACLOperation = "create"
	ACLOperationDelete          ACLOperation = "delete"
	ACLOperationAlter           ACLOperation = "alter"
	ACLOperationDescribe        ACLOperation = "describe"
	ACLOperationClusterAction   ACLOperation = "cluster_action"
	ACLOperationDescribeConfigs ACLOperation = "describe_configs"
	ACLOperationAlterConfigs    ACLOperation = "alter_configs"
	ACLOperationIdempotentWrite ACLOperation = "idempotent_write"
)

// ACLPermissionType says whether an ACL allows or denies. [ACLPermissionTypeAny]
// is only valid in filters.
type ACLPermissionType string

// ACL permission types.
const (
	ACLPermissionTypeAny   ACLPermissionType = "any"
	ACLPermissionTypeAllow ACLPermissionType = "allow"
	ACLPermissionTypeDeny  ACLPermissionType = "deny"
)

// ACL is a single, fully specified access control entry.
type ACL struct {
	ResourceType   ACLResourceType
	ResourceName   string
	PatternType    ACLPatternType
	Principal      string // "Type:name", e.g. "User:alice" or "User:*"
	Host           string // "*" or a specific host address
	Operation      ACLOperation
	PermissionType ACLPermissionType
}

// ACLFilter selects ACLs for describe and delete. An empty string field, or
// an enum field set to its "any" value (or left empty), matches everything.
type ACLFilter struct {
	ResourceType   ACLResourceType
	ResourceName   string
	PatternType    ACLPatternType
	Principal      string
	Host           string
	Operation      ACLOperation
	PermissionType ACLPermissionType
}

// Enum tables: the string form is the public value, the code is the Kafka
// wire value (identical to kafka-go's constants).
type enumEntry[T ~string] struct {
	value  T
	code   int8
	filter bool // only valid in filters
}

var (
	resourceTypeTable = []enumEntry[ACLResourceType]{
		{ACLResourceTypeAny, int8(kafka.ResourceTypeAny), true},
		{ACLResourceTypeTopic, int8(kafka.ResourceTypeTopic), false},
		{ACLResourceTypeGroup, int8(kafka.ResourceTypeGroup), false},
		{ACLResourceTypeCluster, int8(kafka.ResourceTypeCluster), false},
		{ACLResourceTypeTransactionalID, int8(kafka.ResourceTypeTransactionalID), false},
		{ACLResourceTypeDelegationToken, int8(kafka.ResourceTypeDelegationToken), false},
	}
	patternTypeTable = []enumEntry[ACLPatternType]{
		{ACLPatternTypeAny, int8(kafka.PatternTypeAny), true},
		{ACLPatternTypeMatch, int8(kafka.PatternTypeMatch), true},
		{ACLPatternTypeLiteral, int8(kafka.PatternTypeLiteral), false},
		{ACLPatternTypePrefixed, int8(kafka.PatternTypePrefixed), false},
	}
	operationTable = []enumEntry[ACLOperation]{
		{ACLOperationAny, int8(kafka.ACLOperationTypeAny), true},
		{ACLOperationAll, int8(kafka.ACLOperationTypeAll), false},
		{ACLOperationRead, int8(kafka.ACLOperationTypeRead), false},
		{ACLOperationWrite, int8(kafka.ACLOperationTypeWrite), false},
		{ACLOperationCreate, int8(kafka.ACLOperationTypeCreate), false},
		{ACLOperationDelete, int8(kafka.ACLOperationTypeDelete), false},
		{ACLOperationAlter, int8(kafka.ACLOperationTypeAlter), false},
		{ACLOperationDescribe, int8(kafka.ACLOperationTypeDescribe), false},
		{ACLOperationClusterAction, int8(kafka.ACLOperationTypeClusterAction), false},
		{ACLOperationDescribeConfigs, int8(kafka.ACLOperationTypeDescribeConfigs), false},
		{ACLOperationAlterConfigs, int8(kafka.ACLOperationTypeAlterConfigs), false},
		{ACLOperationIdempotentWrite, int8(kafka.ACLOperationTypeIdempotentWrite), false},
	}
	permissionTypeTable = []enumEntry[ACLPermissionType]{
		{ACLPermissionTypeAny, int8(kafka.ACLPermissionTypeAny), true},
		{ACLPermissionTypeAllow, int8(kafka.ACLPermissionTypeAllow), false},
		{ACLPermissionTypeDeny, int8(kafka.ACLPermissionTypeDeny), false},
	}
)

// parseEnum matches s case-insensitively (ignoring surrounding space)
// against table. Filter-only values are accepted only when filter is true.
func parseEnum[T ~string](kind, s string, filter bool, table []enumEntry[T]) (T, error) {
	norm := strings.ToLower(strings.TrimSpace(s))
	valid := make([]string, 0, len(table))
	for _, e := range table {
		if e.filter && !filter {
			continue
		}
		valid = append(valid, string(e.value))
		if string(e.value) == norm {
			return e.value, nil
		}
	}
	return "", fmt.Errorf("%w: unknown %s %q, valid values are: %s",
		ErrInvalidACL, kind, s, strings.Join(valid, ", "))
}

func enumCode[T ~string](kind string, v T, table []enumEntry[T]) (int8, error) {
	for _, e := range table {
		if e.value == v {
			return e.code, nil
		}
	}
	return 0, fmt.Errorf("%w: unknown %s %q", ErrInvalidACL, kind, string(v))
}

func enumFromCode[T ~string](kind string, code int8, table []enumEntry[T]) (T, error) {
	for _, e := range table {
		if e.code == code && !e.filter {
			return e.value, nil
		}
	}
	return "", fmt.Errorf("broker returned unsupported ACL %s code %d", kind, code)
}

// ParseACLResourceType parses a concrete resource type (topic, group,
// cluster, transactional_id, delegation_token), case-insensitively.
func ParseACLResourceType(s string) (ACLResourceType, error) {
	return parseEnum("resource type", s, false, resourceTypeTable)
}

// ParseACLPatternType parses a concrete pattern type (literal, prefixed),
// case-insensitively.
func ParseACLPatternType(s string) (ACLPatternType, error) {
	return parseEnum("pattern type", s, false, patternTypeTable)
}

// ParseACLOperation parses a concrete operation, case-insensitively.
func ParseACLOperation(s string) (ACLOperation, error) {
	return parseEnum("operation", s, false, operationTable)
}

// ParseACLPermissionType parses a concrete permission type (allow, deny),
// case-insensitively.
func ParseACLPermissionType(s string) (ACLPermissionType, error) {
	return parseEnum("permission type", s, false, permissionTypeTable)
}

// ParseACLResourceTypeFilter is like [ParseACLResourceType] but also accepts "any".
func ParseACLResourceTypeFilter(s string) (ACLResourceType, error) {
	return parseEnum("resource type", s, true, resourceTypeTable)
}

// ParseACLPatternTypeFilter is like [ParseACLPatternType] but also accepts "any" and "match".
func ParseACLPatternTypeFilter(s string) (ACLPatternType, error) {
	return parseEnum("pattern type", s, true, patternTypeTable)
}

// ParseACLOperationFilter is like [ParseACLOperation] but also accepts "any".
func ParseACLOperationFilter(s string) (ACLOperation, error) {
	return parseEnum("operation", s, true, operationTable)
}

// ParseACLPermissionTypeFilter is like [ParseACLPermissionType] but also accepts "any".
func ParseACLPermissionTypeFilter(s string) (ACLPermissionType, error) {
	return parseEnum("permission type", s, true, permissionTypeTable)
}

// validOperations lists, per resource type, the operations Kafka accepts in
// an ACL (mirrors Kafka's AclEntry.supportedOperations). The broker rejects
// anything else with INVALID_REQUEST.
var validOperations = map[ACLResourceType][]ACLOperation{
	ACLResourceTypeTopic: {
		ACLOperationAll, ACLOperationRead, ACLOperationWrite, ACLOperationCreate,
		ACLOperationDelete, ACLOperationAlter, ACLOperationDescribe,
		ACLOperationDescribeConfigs, ACLOperationAlterConfigs,
	},
	ACLResourceTypeGroup: {
		ACLOperationAll, ACLOperationRead, ACLOperationDelete, ACLOperationDescribe,
	},
	ACLResourceTypeCluster: {
		ACLOperationAll, ACLOperationCreate, ACLOperationClusterAction,
		ACLOperationDescribeConfigs, ACLOperationAlterConfigs,
		ACLOperationIdempotentWrite, ACLOperationAlter, ACLOperationDescribe,
	},
	ACLResourceTypeTransactionalID: {
		ACLOperationAll, ACLOperationDescribe, ACLOperationWrite,
	},
	ACLResourceTypeDelegationToken: {
		ACLOperationAll, ACLOperationDescribe,
	},
}

// Validate checks the ACL against the rules Kafka enforces, so mistakes are
// reported before a request is sent. It returns an error wrapping
// [ErrInvalidACL]. Rules:
//
//   - every enum must be a concrete value (no any/match);
//   - resource name, principal and host must be non-empty;
//   - principal must have the form "Type:name" with both parts non-empty, and
//     the type "User" must be written with that exact case;
//   - host must be "*" or an IP address;
//   - the cluster resource requires name [ClusterResourceName] and a literal pattern;
//   - a prefixed pattern cannot use the wildcard name "*";
//   - the operation must be supported by the resource type.
func (a ACL) Validate() error {
	_, err := a.parse()
	return err
}

// aclEnums holds the parsed enum values of an ACL or an ACL filter.
type aclEnums struct {
	resourceType   ACLResourceType
	patternType    ACLPatternType
	operation      ACLOperation
	permissionType ACLPermissionType
}

// parse checks the ACL as [ACL.Validate] describes and returns its parsed enum
// values. The parsed values have the canonical case. Use them, and not the
// text in the ACL, to build a request.
func (a ACL) parse() (aclEnums, error) {
	rt, err := ParseACLResourceType(string(a.ResourceType))
	if err != nil {
		return aclEnums{}, err
	}
	pt, err := ParseACLPatternType(string(a.PatternType))
	if err != nil {
		return aclEnums{}, err
	}
	op, err := ParseACLOperation(string(a.Operation))
	if err != nil {
		return aclEnums{}, err
	}
	perm, err := ParseACLPermissionType(string(a.PermissionType))
	if err != nil {
		return aclEnums{}, err
	}
	if a.ResourceName == "" {
		return aclEnums{}, fmt.Errorf("%w: resource name must not be empty", ErrInvalidACL)
	}
	if err := validatePrincipal(a.Principal); err != nil {
		return aclEnums{}, err
	}
	if err := validateHost(a.Host); err != nil {
		return aclEnums{}, err
	}
	if err := checkResourceRules(rt, a.ResourceName, pt, op); err != nil {
		return aclEnums{}, err
	}
	return aclEnums{resourceType: rt, patternType: pt, operation: op, permissionType: perm}, nil
}

// checkResourceRules checks the rules that link the resource type, the
// resource name, the pattern type and the operation. An empty name, and a type
// or operation of "any", are not checked. It returns an error wrapping
// [ErrInvalidACL].
func checkResourceRules(rt ACLResourceType, name string, pt ACLPatternType, op ACLOperation) error {
	if rt == ACLResourceTypeCluster {
		if name != "" && name != ClusterResourceName {
			return fmt.Errorf("%w: resource name for resource type cluster must be %q, got %q",
				ErrInvalidACL, ClusterResourceName, name)
		}
		if pt == ACLPatternTypePrefixed {
			return fmt.Errorf("%w: resource type cluster has no prefixed ACLs, its pattern type is literal", ErrInvalidACL)
		}
	}
	if pt == ACLPatternTypePrefixed && name == "*" {
		return fmt.Errorf("%w: pattern type prefixed cannot be combined with the wildcard resource name %q; use literal", ErrInvalidACL, "*")
	}
	if rt != ACLResourceTypeAny && op != ACLOperationAny && !slices.Contains(validOperations[rt], op) {
		return fmt.Errorf("%w: operation %q is not valid for resource type %q, valid operations are: %s",
			ErrInvalidACL, op, rt, joinStrings(validOperations[rt]))
	}
	return nil
}

func joinStrings[T ~string](vs []T) string {
	ss := make([]string, len(vs))
	for i, v := range vs {
		ss[i] = string(v)
	}
	return strings.Join(ss, ", ")
}

// validatePrincipal checks that p has the form "Type:name" with a type and a
// name. Kafka compares the type with exact case, and the type "User" is the
// one every broker knows, so another case of "user" is an error. Other types
// are valid, because Kafka allows custom principal types.
func validatePrincipal(p string) error {
	if p == "" {
		return fmt.Errorf("%w: principal must not be empty", ErrInvalidACL)
	}
	typ, name, ok := strings.Cut(p, ":")
	if !ok || typ == "" || name == "" {
		return fmt.Errorf(`%w: principal %q must have the form "Type:name", e.g. "User:alice"`, ErrInvalidACL, p)
	}
	if typ != "User" && strings.EqualFold(typ, "user") {
		return fmt.Errorf(`%w: principal %q has the type %q. Kafka compares the type with exact case, so write "User:%s"`,
			ErrInvalidACL, p, typ, name)
	}
	return nil
}

// validateHost checks that h is "*" or an IP address. Kafka matches the host
// of an ACL exactly. It does not support host names, partial wildcards or
// network ranges, so an ACL with such a value never matches any client.
func validateHost(h string) error {
	if h == "" {
		return fmt.Errorf(`%w: host must not be empty (use "*" for all hosts)`, ErrInvalidACL)
	}
	if h == "*" || net.ParseIP(h) != nil {
		return nil
	}
	return fmt.Errorf(`%w: host %q must be "*" or an IP address. Kafka matches hosts exactly. `+
		`It does not support network ranges (such as 10.0.0.0/8), partial wildcards or host names`,
		ErrInvalidACL, h)
}

// isMatchAll reports whether the filter has no restriction at all.
func (f ACLFilter) isMatchAll() bool {
	isAny := func(s string) bool { return strings.EqualFold(orAny(s), "any") }
	return isAny(string(f.ResourceType)) &&
		f.ResourceName == "" &&
		isAny(string(f.PatternType)) &&
		f.Principal == "" &&
		f.Host == "" &&
		isAny(string(f.Operation)) &&
		isAny(string(f.PermissionType))
}

// orAny returns "any" for an empty value, and the trimmed value otherwise.
func orAny(s string) string {
	if s = strings.TrimSpace(s); s == "" {
		return "any"
	}
	return s
}

// Validate checks the filter: enums must be known values (empty means
// "any"), and a non-empty principal or host follows the same format rules as
// in [ACL.Validate]. For deletion, [Client.DeleteACLs] additionally refuses a
// filter that would match every ACL.
func (f ACLFilter) Validate() error {
	_, err := f.toKafka()
	return err
}

// CheckConsistency reports filters whose fields cannot all match the same
// ACL, because no ACL Kafka accepts has that combination. Such a filter is
// valid on the wire and simply matches nothing, so this is for catching
// mistakes early. It runs [ACLFilter.Validate] first and returns an error
// wrapping [ErrInvalidACL]. Rules, applied to the fields that are set:
//
//   - the cluster resource type requires resource name [ClusterResourceName]
//     and not a prefixed pattern;
//   - a prefixed pattern cannot be combined with the wildcard name "*";
//   - the operation must be supported by the resource type.
func (f ACLFilter) CheckConsistency() error {
	e, err := f.parse()
	if err != nil {
		return err
	}
	return checkResourceRules(e.resourceType, f.ResourceName, e.patternType, e.operation)
}

// parse checks the enums, the principal and the host of the filter. It returns
// the parsed enum values, which have the canonical case.
func (f ACLFilter) parse() (aclEnums, error) {
	rt, err := ParseACLResourceTypeFilter(orAny(string(f.ResourceType)))
	if err != nil {
		return aclEnums{}, err
	}
	pt, err := ParseACLPatternTypeFilter(orAny(string(f.PatternType)))
	if err != nil {
		return aclEnums{}, err
	}
	op, err := ParseACLOperationFilter(orAny(string(f.Operation)))
	if err != nil {
		return aclEnums{}, err
	}
	perm, err := ParseACLPermissionTypeFilter(orAny(string(f.PermissionType)))
	if err != nil {
		return aclEnums{}, err
	}
	if f.Host != "" {
		if err := validateHost(f.Host); err != nil {
			return aclEnums{}, err
		}
	}
	if f.Principal != "" {
		if err := validatePrincipal(f.Principal); err != nil {
			return aclEnums{}, err
		}
	}
	return aclEnums{resourceType: rt, patternType: pt, operation: op, permissionType: perm}, nil
}

// toKafka converts the filter to kafka-go's wire form, validating it.
func (f ACLFilter) toKafka() (kafka.ACLFilter, error) {
	e, err := f.parse()
	if err != nil {
		return kafka.ACLFilter{}, err
	}
	codes, err := e.codes()
	if err != nil {
		return kafka.ACLFilter{}, err
	}
	return kafka.ACLFilter{
		ResourceTypeFilter:        kafka.ResourceType(codes.resourceType),
		ResourceNameFilter:        f.ResourceName,
		ResourcePatternTypeFilter: kafka.PatternType(codes.patternType),
		PrincipalFilter:           f.Principal,
		HostFilter:                f.Host,
		Operation:                 kafka.ACLOperationType(codes.operation),
		PermissionType:            kafka.ACLPermissionType(codes.permissionType),
	}, nil
}

// aclCodes holds the Kafka wire codes of four parsed enum values.
type aclCodes struct{ resourceType, patternType, operation, permissionType int8 }

// codes looks up the Kafka wire codes of the parsed enum values.
func (e aclEnums) codes() (aclCodes, error) {
	var c aclCodes
	var err error
	if c.resourceType, err = enumCode("resource type", e.resourceType, resourceTypeTable); err != nil {
		return aclCodes{}, err
	}
	if c.patternType, err = enumCode("pattern type", e.patternType, patternTypeTable); err != nil {
		return aclCodes{}, err
	}
	if c.operation, err = enumCode("operation", e.operation, operationTable); err != nil {
		return aclCodes{}, err
	}
	if c.permissionType, err = enumCode("permission type", e.permissionType, permissionTypeTable); err != nil {
		return aclCodes{}, err
	}
	return c, nil
}

func (a ACL) toKafka() (kafka.ACLEntry, error) {
	e, err := a.parse()
	if err != nil {
		return kafka.ACLEntry{}, err
	}
	codes, err := e.codes()
	if err != nil {
		return kafka.ACLEntry{}, err
	}
	return kafka.ACLEntry{
		ResourceType:        kafka.ResourceType(codes.resourceType),
		ResourceName:        a.ResourceName,
		ResourcePatternType: kafka.PatternType(codes.patternType),
		Principal:           a.Principal,
		Host:                a.Host,
		Operation:           kafka.ACLOperationType(codes.operation),
		PermissionType:      kafka.ACLPermissionType(codes.permissionType),
	}, nil
}

func aclFromKafka(rt kafka.ResourceType, name string, pt kafka.PatternType, principal, host string,
	op kafka.ACLOperationType, perm kafka.ACLPermissionType,
) (ACL, error) {
	r, err := enumFromCode("resource type", int8(rt), resourceTypeTable)
	if err != nil {
		return ACL{}, err
	}
	p, err := enumFromCode("pattern type", int8(pt), patternTypeTable)
	if err != nil {
		return ACL{}, err
	}
	o, err := enumFromCode("operation", int8(op), operationTable)
	if err != nil {
		return ACL{}, err
	}
	pm, err := enumFromCode("permission type", int8(perm), permissionTypeTable)
	if err != nil {
		return ACL{}, err
	}
	return ACL{
		ResourceType: r, ResourceName: name, PatternType: p,
		Principal: principal, Host: host, Operation: o, PermissionType: pm,
	}, nil
}

// wrapACLError prefixes err with action and adds the matching sentinel. The
// original error stays in the chain, so kafka.Error codes still match.
func wrapACLError(action string, err error) error {
	return fmt.Errorf("%s: %w", action, classifyBrokerError(err))
}

// CreateACL creates a single ACL. The ACL is validated first (see
// [ACL.Validate]); a validation failure wraps [ErrInvalidACL] and nothing is
// sent. Broker errors are reported even when the call itself succeeded;
// authorization failures wrap [ErrNotAuthorized] and a broker without an
// authorizer wraps [ErrSecurityDisabled].
func (c *Client) CreateACL(ctx context.Context, acl ACL) error {
	entry, err := acl.toKafka()
	if err != nil {
		return fmt.Errorf("create ACL: %w", err)
	}
	resp, err := c.kc.CreateACLs(ctx, &kafka.CreateACLsRequest{ACLs: []kafka.ACLEntry{entry}})
	if err != nil {
		return wrapACLError("create ACL", err)
	}
	if len(resp.Errors) != 1 {
		return fmt.Errorf("create ACL: broker returned %d results for 1 ACL", len(resp.Errors))
	}
	if err := resp.Errors[0]; err != nil {
		return wrapACLError("create ACL", err)
	}
	return nil
}

// DescribeACLs returns every ACL matching filter, flattened into one slice.
// An empty filter returns all ACLs. Errors are mapped as in [Client.CreateACL].
func (r *Reader) DescribeACLs(ctx context.Context, filter ACLFilter) ([]ACL, error) {
	kf, err := filter.toKafka()
	if err != nil {
		return nil, fmt.Errorf("describe ACLs: %w", err)
	}
	resp, err := r.kc.DescribeACLs(ctx, &kafka.DescribeACLsRequest{Filter: kf})
	if err != nil {
		return nil, wrapACLError("describe ACLs", err)
	}
	if resp.Error != nil {
		return nil, wrapACLError("describe ACLs", resp.Error)
	}
	acls := make([]ACL, 0, len(resp.Resources))
	for _, res := range resp.Resources {
		for _, d := range res.ACLs {
			acl, err := aclFromKafka(res.ResourceType, res.ResourceName, res.PatternType,
				d.Principal, d.Host, d.Operation, d.PermissionType)
			if err != nil {
				return nil, fmt.Errorf("describe ACLs: %w", err)
			}
			acls = append(acls, acl)
		}
	}
	return acls, nil
}

// DeleteACLs deletes every ACL matching filter and returns how many were
// deleted. A filter that would match all ACLs (everything empty or "any") is
// rejected with [ErrInvalidACL] as a safety guard. If the broker reports a
// failure for some matches, the count covers only the ACLs that were
// deleted and the error describes the failure.
func (c *Client) DeleteACLs(ctx context.Context, filter ACLFilter) (int, error) {
	if filter.isMatchAll() {
		return 0, fmt.Errorf("delete ACLs: %w: refusing to delete with a filter that matches every ACL", ErrInvalidACL)
	}
	kf, err := filter.toKafka()
	if err != nil {
		return 0, fmt.Errorf("delete ACLs: %w", err)
	}
	resp, err := c.kc.DeleteACLs(ctx, &kafka.DeleteACLsRequest{
		Filters: []kafka.DeleteACLsFilter{{
			ResourceTypeFilter:        kf.ResourceTypeFilter,
			ResourceNameFilter:        kf.ResourceNameFilter,
			ResourcePatternTypeFilter: kf.ResourcePatternTypeFilter,
			PrincipalFilter:           kf.PrincipalFilter,
			HostFilter:                kf.HostFilter,
			Operation:                 kf.Operation,
			PermissionType:            kf.PermissionType,
		}},
	})
	if err != nil {
		return 0, wrapACLError("delete ACLs", err)
	}
	deleted := 0
	var errs []error
	for _, res := range resp.Results {
		if res.Error != nil {
			errs = append(errs, wrapACLError("delete ACLs", res.Error))
			continue
		}
		for _, m := range res.MatchingACLs {
			if m.Error != nil {
				errs = append(errs, wrapACLError("delete ACLs", m.Error))
				continue
			}
			deleted++
		}
	}
	return deleted, errors.Join(errs...)
}

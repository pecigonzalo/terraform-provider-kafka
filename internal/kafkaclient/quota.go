package kafkaclient

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/protocol/describeclientquotas"
)

// Kafka API keys for the client quota APIs (KIP-546, Kafka 2.6+).
const (
	apiKeyDescribeClientQuotas = 48
	apiKeyAlterClientQuotas    = 49
)

// describe match types, from the DescribeClientQuotas protocol.
const (
	matchExact   int8 = 0
	matchDefault int8 = 1
	matchAny     int8 = 2
)

var (
	// ErrQuotasUnsupported is returned when the broker does not offer the
	// client quota APIs, which require Kafka 2.6 or later.
	ErrQuotasUnsupported = errors.New("client quotas are not supported by this broker (requires Kafka 2.6 or later)")

	// ErrQuotaAuthorizationFailed is returned when the broker denies the
	// quota operation to the authenticated principal.
	ErrQuotaAuthorizationFailed = errors.New("not authorized to manage client quotas")
)

// QuotaEntityType identifies a kind of quota entity.
type QuotaEntityType string

// Supported quota entity types.
const (
	QuotaEntityUser     QuotaEntityType = "user"
	QuotaEntityClientID QuotaEntityType = "client-id"
	QuotaEntityIP       QuotaEntityType = "ip"
)

var quotaEntityTypes = []QuotaEntityType{QuotaEntityUser, QuotaEntityClientID, QuotaEntityIP}

// ParseQuotaEntityType returns the entity type named s, or an error listing
// the valid values.
func ParseQuotaEntityType(s string) (QuotaEntityType, error) {
	for _, t := range quotaEntityTypes {
		if string(t) == s {
			return t, nil
		}
	}
	return "", fmt.Errorf("invalid quota entity type %q: valid values are %s", s, joinQuoted(quotaEntityTypes))
}

// QuotaEntityName is the name part of one entity component. Exactly one of
// Name (non-empty) or Default must be set; use [NamedQuotaEntity] and
// [DefaultQuotaEntity]. Kafka represents the default entity with a null name.
type QuotaEntityName struct {
	Name    string
	Default bool
}

// NamedQuotaEntity is a component for the entity called name.
func NamedQuotaEntity(name string) QuotaEntityName { return QuotaEntityName{Name: name} }

// DefaultQuotaEntity is the component for the default entity of its type.
func DefaultQuotaEntity() QuotaEntityName { return QuotaEntityName{Default: true} }

// QuotaEntity is a set of entity-type to entity-name pairs, for example
// {user: alice, client-id: default}. A type that is absent from the map is
// unspecified, which is different from the explicit default.
type QuotaEntity map[QuotaEntityType]QuotaEntityName

// Validate checks that the entity is non-empty and well formed.
func (e QuotaEntity) Validate() error {
	if len(e) == 0 {
		return errors.New("quota entity must specify at least one entity type")
	}
	return e.validateParts()
}

func (e QuotaEntity) validateParts() error {
	for t, n := range e {
		if _, err := ParseQuotaEntityType(string(t)); err != nil {
			return err
		}
		switch {
		case n.Default && n.Name != "":
			return fmt.Errorf("quota entity %q: set either a name or default, not both", t)
		case !n.Default && n.Name == "":
			return fmt.Errorf("quota entity %q: name must not be empty (use default for the default entity)", t)
		}
	}
	return nil
}

// sortedTypes returns the entity types in a stable order.
func (e QuotaEntity) sortedTypes() []QuotaEntityType {
	types := make([]QuotaEntityType, 0, len(e))
	for t := range e {
		types = append(types, t)
	}
	slices.Sort(types)
	return types
}

// QuotaKey is a client quota configuration key.
type QuotaKey string

// Supported quota keys.
const (
	QuotaProducerByteRate       QuotaKey = "producer_byte_rate"
	QuotaConsumerByteRate       QuotaKey = "consumer_byte_rate"
	QuotaRequestPercentage      QuotaKey = "request_percentage"
	QuotaControllerMutationRate QuotaKey = "controller_mutation_rate"
	QuotaConnectionCreationRate QuotaKey = "connection_creation_rate"
)

var quotaKeys = []QuotaKey{
	QuotaProducerByteRate,
	QuotaConsumerByteRate,
	QuotaRequestPercentage,
	QuotaControllerMutationRate,
	QuotaConnectionCreationRate,
}

// QuotaKeys returns the supported quota keys.
func QuotaKeys() []QuotaKey { return slices.Clone(quotaKeys) }

// ParseQuotaKey returns the quota key named s, or an error listing the valid
// values.
func ParseQuotaKey(s string) (QuotaKey, error) {
	for _, k := range quotaKeys {
		if string(k) == s {
			return k, nil
		}
	}
	return "", fmt.Errorf("invalid quota key %q: valid values are %s", s, joinQuoted(quotaKeys))
}

func joinQuoted[T ~string](vs []T) string {
	q := make([]string, len(vs))
	for i, v := range vs {
		q[i] = fmt.Sprintf("%q", string(v))
	}
	return strings.Join(q, ", ")
}

// QuotaValues maps quota keys to their limits.
type QuotaValues map[QuotaKey]float64

// QuotaEntry is one entity and the quota values configured on it.
type QuotaEntry struct {
	Entity QuotaEntity
	Values QuotaValues
}

// QuotaFilter selects quotas to describe. The zero value matches every
// entity. Each component of Entity matches that exact name (or the default
// entity); each type in AnyOf matches entities with any name for the type.
// When Strict is set, entities with types not named by the filter are
// excluded; otherwise they are included.
type QuotaFilter struct {
	Entity QuotaEntity
	AnyOf  []QuotaEntityType
	Strict bool
}

func (f QuotaFilter) validate() error {
	if err := f.Entity.validateParts(); err != nil {
		return err
	}
	for _, t := range f.AnyOf {
		if _, err := ParseQuotaEntityType(string(t)); err != nil {
			return err
		}
		if _, dup := f.Entity[t]; dup {
			return fmt.Errorf("quota entity type %q is in both Entity and AnyOf", t)
		}
	}
	return nil
}

func (f QuotaFilter) components() []kafka.DescribeClientQuotasRequestComponent {
	comps := make([]kafka.DescribeClientQuotasRequestComponent, 0, len(f.Entity)+len(f.AnyOf))
	for _, t := range f.Entity.sortedTypes() {
		n := f.Entity[t]
		c := kafka.DescribeClientQuotasRequestComponent{EntityType: string(t), MatchType: matchExact, Match: n.Name}
		if n.Default {
			c.MatchType, c.Match = matchDefault, ""
		}
		comps = append(comps, c)
	}
	for _, t := range f.AnyOf {
		comps = append(comps, kafka.DescribeClientQuotasRequestComponent{EntityType: string(t), MatchType: matchAny})
	}
	return comps
}

// CheckQuotaSupport reports whether the broker offers the client quota APIs.
// It returns an error wrapping [ErrQuotasUnsupported] when it does not. The
// result is not cached.
func (r *Reader) CheckQuotaSupport(ctx context.Context) error {
	return r.requireAPIs(ctx, apiKeyDescribeClientQuotas, apiKeyAlterClientQuotas)
}

func (r *Reader) requireAPIs(ctx context.Context, keys ...int) error {
	resp, err := r.kc.ApiVersions(ctx, &kafka.ApiVersionsRequest{})
	if err != nil {
		return fmt.Errorf("check client quota support: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("check client quota support: %w", resp.Error)
	}
	for _, key := range keys {
		if !slices.ContainsFunc(resp.ApiKeys, func(a kafka.ApiVersionsResponseApiKey) bool { return a.ApiKey == key }) {
			return fmt.Errorf("broker does not offer API key %d: %w", key, ErrQuotasUnsupported)
		}
	}
	return nil
}

// DescribeQuotas returns the quota entries matching filter. It returns an
// error wrapping [ErrQuotasUnsupported] on brokers older than Kafka 2.6 and
// [ErrQuotaAuthorizationFailed] when the principal may not describe quotas.
func (r *Reader) DescribeQuotas(ctx context.Context, filter QuotaFilter) ([]QuotaEntry, error) {
	if err := filter.validate(); err != nil {
		return nil, fmt.Errorf("describe quotas: %w", err)
	}
	if err := r.requireAPIs(ctx, apiKeyDescribeClientQuotas); err != nil {
		return nil, err
	}

	// kafka-go's Client.DescribeClientQuotas drops the response's top-level
	// error code, so send the request through the transport and check it here.
	req := &describeclientquotas.Request{Strict: filter.Strict}
	for _, c := range filter.components() {
		req.Components = append(req.Components, describeclientquotas.Component{
			EntityType: c.EntityType, MatchType: c.MatchType, Match: c.Match,
		})
	}
	resp, err := r.roundTripDescribe(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("describe quotas: %w", err)
	}
	if resp.ErrorCode != 0 {
		return nil, fmt.Errorf("describe quotas: %w", quotaCodeError(resp.ErrorCode, resp.ErrorMessage))
	}

	entries := make([]QuotaEntry, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		entry := QuotaEntry{Entity: QuotaEntity{}, Values: QuotaValues{}}
		for _, ent := range e.Entities {
			entry.Entity[QuotaEntityType(ent.EntityType)] = QuotaEntityName{Name: ent.EntityName, Default: ent.EntityName == ""}
		}
		for _, v := range e.Values {
			entry.Values[QuotaKey(v.Key)] = v.Value
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (r *Reader) roundTripDescribe(ctx context.Context, req *describeclientquotas.Request) (*describeclientquotas.Response, error) {
	if r.kc.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.kc.Timeout)
		defer cancel()
	}
	rt := r.kc.Transport
	if rt == nil {
		rt = kafka.DefaultTransport
	}
	m, err := rt.RoundTrip(ctx, r.kc.Addr, req)
	if err != nil {
		return nil, err
	}
	resp, ok := m.(*describeclientquotas.Response)
	if !ok {
		return nil, fmt.Errorf("unexpected response type %T", m)
	}
	return resp, nil
}

// AlterQuotas sets the values in set and removes the keys in remove on one
// entity. At least one operation is required and a key may not appear in both.
// It returns an error wrapping [ErrQuotasUnsupported] on brokers older than
// Kafka 2.6 and [ErrQuotaAuthorizationFailed] when the principal may not alter
// quotas.
func (c *Client) AlterQuotas(ctx context.Context, entity QuotaEntity, set QuotaValues, remove []string) error {
	ops, err := buildQuotaOps(entity, set, remove)
	if err != nil {
		return fmt.Errorf("alter quotas: %w", err)
	}
	if err := c.requireAPIs(ctx, apiKeyAlterClientQuotas); err != nil {
		return err
	}

	entities := make([]kafka.AlterClientQuotaEntity, 0, len(entity))
	for _, t := range entity.sortedTypes() {
		entities = append(entities, kafka.AlterClientQuotaEntity{EntityType: string(t), EntityName: entity[t].Name})
	}
	resp, err := c.kc.AlterClientQuotas(ctx, &kafka.AlterClientQuotasRequest{
		Entries: []kafka.AlterClientQuotaEntry{{Entities: entities, Ops: ops}},
	})
	if err != nil {
		return fmt.Errorf("alter quotas: %w", classifyQuotaError(err))
	}
	var errs []error
	for _, e := range resp.Entries {
		if e.Error != nil {
			errs = append(errs, classifyQuotaError(e.Error))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("alter quotas: %w", errors.Join(errs...))
	}
	return nil
}

func buildQuotaOps(entity QuotaEntity, set QuotaValues, remove []string) ([]kafka.AlterClientQuotaOps, error) {
	if err := entity.Validate(); err != nil {
		return nil, err
	}
	if len(set) == 0 && len(remove) == 0 {
		return nil, errors.New("nothing to alter: set and remove are both empty")
	}
	ops := make([]kafka.AlterClientQuotaOps, 0, len(set)+len(remove))
	keys := make([]QuotaKey, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if _, err := ParseQuotaKey(string(k)); err != nil {
			return nil, err
		}
		v := set[k]
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return nil, fmt.Errorf("quota %q: value %v must be a finite, non-negative number", k, v)
		}
		ops = append(ops, kafka.AlterClientQuotaOps{Key: string(k), Value: v})
	}
	seen := map[QuotaKey]bool{}
	for _, s := range remove {
		k, err := ParseQuotaKey(s)
		if err != nil {
			return nil, err
		}
		if _, ok := set[k]; ok {
			return nil, fmt.Errorf("quota %q is in both set and remove", k)
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		ops = append(ops, kafka.AlterClientQuotaOps{Key: string(k), Remove: true})
	}
	return ops, nil
}

func quotaCodeError(code int16, msg string) error {
	if msg == "" {
		return classifyQuotaError(kafka.Error(code))
	}
	return classifyQuotaError(fmt.Errorf("%w: %s", kafka.Error(code), msg))
}

// classifyQuotaError adds [ErrQuotaAuthorizationFailed] to authorization
// failures while keeping the original error in the chain.
func classifyQuotaError(err error) error {
	if errors.Is(err, kafka.ClusterAuthorizationFailed) {
		return fmt.Errorf("%w: %w", ErrQuotaAuthorizationFailed, err)
	}
	return err
}

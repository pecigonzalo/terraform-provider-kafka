package provider

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

// quotaDefaultToken stands for the default entity of a type in a quota id. It
// can never equal an escaped name, because escaping turns "<" into "%3C".
const quotaDefaultToken = "<default>"

// quotaEntityOrder is the fixed order in which entity types appear in an id.
var quotaEntityOrder = []kafkaclient.QuotaEntityType{
	kafkaclient.QuotaEntityUser,
	kafkaclient.QuotaEntityClientID,
	kafkaclient.QuotaEntityIP,
}

// quotaEntityPartModel is one entity component of the resource: either a
// named entity or the default entity of its type.
type quotaEntityPartModel struct {
	Name    types.String `tfsdk:"name"`
	Default types.Bool   `tfsdk:"default"`
}

// quotaEntityPart returns the component for the entity name n.
func quotaEntityPart(n kafkaclient.QuotaEntityName) *quotaEntityPartModel {
	if n.Default {
		return &quotaEntityPartModel{Name: types.StringNull(), Default: types.BoolValue(true)}
	}
	return &quotaEntityPartModel{Name: types.StringValue(n.Name), Default: types.BoolNull()}
}

// entityName converts a component to its client representation. A component
// with neither a name nor default = true yields an empty name, which the
// client rejects.
func (p *quotaEntityPartModel) entityName() kafkaclient.QuotaEntityName {
	if p.Default.ValueBool() {
		return kafkaclient.DefaultQuotaEntity()
	}
	return kafkaclient.NamedQuotaEntity(p.Name.ValueString())
}

// quotaEntityFromModel builds the quota entity described by the model.
func quotaEntityFromModel(m *quotaResourceModel) kafkaclient.QuotaEntity {
	entity := kafkaclient.QuotaEntity{}
	for typ, part := range map[kafkaclient.QuotaEntityType]*quotaEntityPartModel{
		kafkaclient.QuotaEntityUser:     m.User,
		kafkaclient.QuotaEntityClientID: m.ClientID,
		kafkaclient.QuotaEntityIP:       m.IP,
	} {
		if part != nil {
			entity[typ] = part.entityName()
		}
	}
	return entity
}

// setEntity stores entity in the model, leaving unspecified types null.
func (m *quotaResourceModel) setEntity(entity kafkaclient.QuotaEntity) {
	m.User, m.ClientID, m.IP = nil, nil, nil
	if n, ok := entity[kafkaclient.QuotaEntityUser]; ok {
		m.User = quotaEntityPart(n)
	}
	if n, ok := entity[kafkaclient.QuotaEntityClientID]; ok {
		m.ClientID = quotaEntityPart(n)
	}
	if n, ok := entity[kafkaclient.QuotaEntityIP]; ok {
		m.IP = quotaEntityPart(n)
	}
}

// validateQuotaEntityCombination checks that Kafka accepts the set of entity
// types: user and client-id may be combined, ip stands alone.
func validateQuotaEntityCombination(entity kafkaclient.QuotaEntity) error {
	if len(entity) == 0 {
		return errors.New("a quota entity needs at least one of user, client-id or ip")
	}
	if _, hasIP := entity[kafkaclient.QuotaEntityIP]; hasIP && len(entity) > 1 {
		return errors.New("the ip entity cannot be combined with user or client-id")
	}
	return nil
}

// encodeQuotaID returns the id of the quota on entity: the components in the
// fixed order user, client-id, ip, written as type=value and joined by commas.
// Names are query-escaped, so they never contain "," or "=" and the default
// entity (written as "<default>") cannot collide with a name.
//
// For example "user=alice,client-id=billing", or "user=<default>" for the
// default user. A user literally named "<default>" is "user=%3Cdefault%3E".
func encodeQuotaID(entity kafkaclient.QuotaEntity) string {
	parts := make([]string, 0, len(entity))
	for _, typ := range quotaEntityOrder {
		n, ok := entity[typ]
		if !ok {
			continue
		}
		value := quotaDefaultToken
		if !n.Default {
			value = url.QueryEscape(n.Name)
		}
		parts = append(parts, string(typ)+"="+value)
	}
	return strings.Join(parts, ",")
}

// decodeQuotaID is the inverse of [encodeQuotaID]. The error names the id and
// the expected format so it can be shown to a user importing a quota.
func decodeQuotaID(id string) (kafkaclient.QuotaEntity, error) {
	const format = `expected "<type>=<name>" components separated by commas, for example "user=alice,client-id=billing", ` +
		`"ip=10.0.0.1" or "user=<default>"`
	fail := func(format2 string, args ...any) (kafkaclient.QuotaEntity, error) {
		return nil, fmt.Errorf("invalid quota id %q: %s; %s", id, fmt.Sprintf(format2, args...), format)
	}
	if id == "" {
		return fail("the id is empty")
	}
	entity := kafkaclient.QuotaEntity{}
	for part := range strings.SplitSeq(id, ",") {
		rawType, rawName, ok := strings.Cut(part, "=")
		if !ok {
			return fail("component %q has no \"=\"", part)
		}
		typ, err := kafkaclient.ParseQuotaEntityType(rawType)
		if err != nil {
			return fail("%v", err)
		}
		if _, dup := entity[typ]; dup {
			return fail("entity type %q appears more than once", typ)
		}
		switch rawName {
		case "":
			return fail("the name of %q is empty", typ)
		case quotaDefaultToken:
			entity[typ] = kafkaclient.DefaultQuotaEntity()
		default:
			name, err := url.QueryUnescape(rawName)
			if err != nil {
				return fail("the name of %q is not correctly escaped: %v", typ, err)
			}
			if name == "" {
				return fail("the name of %q is empty", typ)
			}
			entity[typ] = kafkaclient.NamedQuotaEntity(name)
		}
	}
	if err := validateQuotaEntityCombination(entity); err != nil {
		return fail("%v", err)
	}
	return entity, nil
}

// sameQuotaEntity reports whether a and b name the same entity.
func sameQuotaEntity(a, b kafkaclient.QuotaEntity) bool {
	return maps.Equal(a, b)
}

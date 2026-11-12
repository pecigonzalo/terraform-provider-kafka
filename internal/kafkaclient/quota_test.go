package kafkaclient

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/protocol/alterclientquotas"
	"github.com/segmentio/kafka-go/protocol/apiversions"
	"github.com/segmentio/kafka-go/protocol/describeclientquotas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeQuotaRoundTripper serves canned ApiVersions, DescribeClientQuotas and
// AlterClientQuotas responses and records the quota requests it receives.
type fakeQuotaRoundTripper struct {
	apiKeys   []int16
	apiErr    error
	describe  *describeclientquotas.Response
	alter     *alterclientquotas.Response
	describes []*describeclientquotas.Request
	alters    []*alterclientquotas.Request
}

func (f *fakeQuotaRoundTripper) RoundTrip(_ context.Context, _ net.Addr, req kafka.Request) (kafka.Response, error) {
	switch r := req.(type) {
	case *apiversions.Request:
		if f.apiErr != nil {
			return nil, f.apiErr
		}
		resp := &apiversions.Response{}
		for _, k := range f.apiKeys {
			resp.ApiKeys = append(resp.ApiKeys, apiversions.ApiKeyResponse{ApiKey: k, MaxVersion: 1})
		}
		return resp, nil
	case *describeclientquotas.Request:
		f.describes = append(f.describes, r)
		return f.describe, nil
	case *alterclientquotas.Request:
		f.alters = append(f.alters, r)
		return f.alter, nil
	}
	return nil, errors.New("unexpected request")
}

func newQuotaClient(t *testing.T, rt *fakeQuotaRoundTripper) *Client {
	t.Helper()
	c, err := newClient(t.Context(), Config{BootstrapServer: "b:9092"}, fakeAWSLoader)
	require.NoError(t, err)
	c.kc.Transport = rt
	return c
}

func quotaAPIs() []int16 { return []int16{18, 48, 49} }

func TestParseQuotaKey(t *testing.T) {
	for _, k := range QuotaKeys() {
		got, err := ParseQuotaKey(string(k))
		require.NoError(t, err)
		assert.Equal(t, k, got)
	}
	for _, in := range []string{"", "producer", "PRODUCER_BYTE_RATE"} {
		t.Run("invalid_"+in, func(t *testing.T) {
			_, err := ParseQuotaKey(in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "producer_byte_rate")
			assert.Contains(t, err.Error(), "connection_creation_rate")
		})
	}
}

func TestParseQuotaEntityType(t *testing.T) {
	got, err := ParseQuotaEntityType("client-id")
	require.NoError(t, err)
	assert.Equal(t, QuotaEntityClientID, got)

	_, err = ParseQuotaEntityType("topic")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"user", "client-id", "ip"`)
}

func TestFilterComponents(t *testing.T) {
	f := QuotaFilter{
		Entity: QuotaEntity{
			QuotaEntityUser:     NamedQuotaEntity("alice"),
			QuotaEntityClientID: DefaultQuotaEntity(),
		},
		AnyOf: []QuotaEntityType{QuotaEntityIP},
	}
	require.NoError(t, f.validate())
	assert.Equal(t, []kafka.DescribeClientQuotasRequestComponent{
		{EntityType: "client-id", MatchType: matchDefault},
		{EntityType: "user", MatchType: matchExact, Match: "alice"},
		{EntityType: "ip", MatchType: matchAny},
	}, f.components())
}

func TestDescribeQuotas(t *testing.T) {
	rt := &fakeQuotaRoundTripper{
		apiKeys: quotaAPIs(),
		describe: &describeclientquotas.Response{Entries: []describeclientquotas.ResponseQuotas{
			{
				Entities: []describeclientquotas.Entity{{EntityType: "user", EntityName: "alice"}},
				Values:   []describeclientquotas.Value{{Key: "producer_byte_rate", Value: 1024}, {Key: "consumer_byte_rate", Value: 2048}},
			},
			{
				Entities: []describeclientquotas.Entity{{EntityType: "user", EntityName: ""}, {EntityType: "client-id", EntityName: "c1"}},
				Values:   []describeclientquotas.Value{{Key: "request_percentage", Value: 50}},
			},
		}},
	}
	c := newQuotaClient(t, rt)

	got, err := c.ReadOnly().DescribeQuotas(t.Context(), QuotaFilter{
		Entity: QuotaEntity{QuotaEntityUser: NamedQuotaEntity("alice")}, Strict: true,
	})
	require.NoError(t, err)
	assert.Equal(t, []QuotaEntry{
		{
			Entity: QuotaEntity{QuotaEntityUser: NamedQuotaEntity("alice")},
			Values: QuotaValues{QuotaProducerByteRate: 1024, QuotaConsumerByteRate: 2048},
		},
		{
			Entity: QuotaEntity{QuotaEntityUser: DefaultQuotaEntity(), QuotaEntityClientID: NamedQuotaEntity("c1")},
			Values: QuotaValues{QuotaRequestPercentage: 50},
		},
	}, got)

	require.Len(t, rt.describes, 1)
	assert.True(t, rt.describes[0].Strict)
	assert.Equal(t, []describeclientquotas.Component{{EntityType: "user", MatchType: matchExact, Match: "alice"}}, rt.describes[0].Components)
}

func TestDescribeQuotas_Errors(t *testing.T) {
	tests := []struct {
		name     string
		filter   QuotaFilter
		apiKeys  []int16
		resp     *describeclientquotas.Response
		wantIs   error
		wantSent bool
	}{
		{
			name: "top_level_error_code", apiKeys: quotaAPIs(), wantSent: true,
			resp:   &describeclientquotas.Response{ErrorCode: int16(kafka.InvalidRequest), ErrorMessage: "bad"},
			wantIs: kafka.InvalidRequest,
		},
		{
			name: "authorization_failed", apiKeys: quotaAPIs(), wantSent: true,
			resp:   &describeclientquotas.Response{ErrorCode: int16(kafka.ClusterAuthorizationFailed)},
			wantIs: ErrQuotaAuthorizationFailed,
		},
		{
			name: "unsupported_broker", apiKeys: []int16{18},
			wantIs: ErrQuotasUnsupported,
		},
		{
			name: "invalid_filter_type", apiKeys: quotaAPIs(),
			filter: QuotaFilter{Entity: QuotaEntity{"topic": NamedQuotaEntity("x")}},
		},
		{
			name: "empty_name_in_filter", apiKeys: quotaAPIs(),
			filter: QuotaFilter{Entity: QuotaEntity{QuotaEntityUser: {}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fakeQuotaRoundTripper{apiKeys: tc.apiKeys, describe: tc.resp}
			_, err := newQuotaClient(t, rt).DescribeQuotas(t.Context(), tc.filter)
			require.Error(t, err)
			if tc.wantIs != nil {
				assert.ErrorIs(t, err, tc.wantIs)
			}
			assert.Equal(t, tc.wantSent, len(rt.describes) == 1)
		})
	}
}

func TestAlterQuotas(t *testing.T) {
	rt := &fakeQuotaRoundTripper{apiKeys: quotaAPIs(), alter: &alterclientquotas.Response{}}
	c := newQuotaClient(t, rt)

	err := c.AlterQuotas(t.Context(),
		QuotaEntity{QuotaEntityUser: NamedQuotaEntity("alice"), QuotaEntityClientID: DefaultQuotaEntity()},
		QuotaValues{QuotaProducerByteRate: 1024},
		[]string{"consumer_byte_rate"},
	)
	require.NoError(t, err)

	require.Len(t, rt.alters, 1)
	require.Len(t, rt.alters[0].Entries, 1)
	e := rt.alters[0].Entries[0]
	assert.Equal(t, []alterclientquotas.Entity{
		{EntityType: "client-id", EntityName: ""},
		{EntityType: "user", EntityName: "alice"},
	}, e.Entities)
	assert.Equal(t, []alterclientquotas.Ops{
		{Key: "producer_byte_rate", Value: 1024},
		{Key: "consumer_byte_rate", Remove: true},
	}, e.Ops)
	assert.False(t, rt.alters[0].ValidateOnly)
}

func TestAlterQuotas_Errors(t *testing.T) {
	user := QuotaEntity{QuotaEntityUser: NamedQuotaEntity("alice")}
	tests := []struct {
		name     string
		apiKeys  []int16
		entity   QuotaEntity
		set      QuotaValues
		remove   []string
		resp     *alterclientquotas.Response
		wantIs   error
		wantSent bool
	}{
		{name: "empty_entity", apiKeys: quotaAPIs(), set: QuotaValues{QuotaProducerByteRate: 1}},
		{name: "default_and_name", apiKeys: quotaAPIs(), set: QuotaValues{QuotaProducerByteRate: 1},
			entity: QuotaEntity{QuotaEntityUser: {Name: "a", Default: true}}},
		{name: "empty_name", apiKeys: quotaAPIs(), set: QuotaValues{QuotaProducerByteRate: 1},
			entity: QuotaEntity{QuotaEntityUser: {}}},
		{name: "unknown_entity_type", apiKeys: quotaAPIs(), set: QuotaValues{QuotaProducerByteRate: 1},
			entity: QuotaEntity{"topic": NamedQuotaEntity("t")}},
		{name: "nothing_to_do", apiKeys: quotaAPIs(), entity: user},
		{name: "negative_value", apiKeys: quotaAPIs(), entity: user, set: QuotaValues{QuotaProducerByteRate: -1}},
		{name: "unknown_set_key", apiKeys: quotaAPIs(), entity: user, set: QuotaValues{"bogus": 1}},
		{name: "unknown_remove_key", apiKeys: quotaAPIs(), entity: user, remove: []string{"bogus"}},
		{name: "key_in_both", apiKeys: quotaAPIs(), entity: user,
			set: QuotaValues{QuotaProducerByteRate: 1}, remove: []string{"producer_byte_rate"}},
		{name: "unsupported_broker", apiKeys: []int16{18, 48}, entity: user,
			set: QuotaValues{QuotaProducerByteRate: 1}, wantIs: ErrQuotasUnsupported},
		{name: "per_entry_error", apiKeys: quotaAPIs(), entity: user, wantSent: true,
			set: QuotaValues{QuotaProducerByteRate: 1}, wantIs: kafka.InvalidRequest,
			resp: &alterclientquotas.Response{Results: []alterclientquotas.ResponseQuotas{
				{ErrorCode: int16(kafka.InvalidRequest), ErrorMessage: "nope"}}}},
		{name: "authorization_failed", apiKeys: quotaAPIs(), entity: user, wantSent: true,
			set: QuotaValues{QuotaProducerByteRate: 1}, wantIs: ErrQuotaAuthorizationFailed,
			resp: &alterclientquotas.Response{Results: []alterclientquotas.ResponseQuotas{
				{ErrorCode: int16(kafka.ClusterAuthorizationFailed)}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fakeQuotaRoundTripper{apiKeys: tc.apiKeys, alter: tc.resp}
			err := newQuotaClient(t, rt).AlterQuotas(t.Context(), tc.entity, tc.set, tc.remove)
			require.Error(t, err)
			if tc.wantIs != nil {
				assert.ErrorIs(t, err, tc.wantIs)
			}
			assert.Equal(t, tc.wantSent, len(rt.alters) == 1)
		})
	}
}

func TestCheckQuotaSupport(t *testing.T) {
	tests := []struct {
		name    string
		apiKeys []int16
		apiErr  error
		wantIs  error
		wantErr bool
	}{
		{name: "supported", apiKeys: quotaAPIs()},
		{name: "missing_alter", apiKeys: []int16{48}, wantIs: ErrQuotasUnsupported},
		{name: "missing_describe", apiKeys: []int16{49}, wantIs: ErrQuotasUnsupported},
		{name: "transport_error_is_not_unsupported", apiErr: errors.New("boom"), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fakeQuotaRoundTripper{apiKeys: tc.apiKeys, apiErr: tc.apiErr}
			err := newQuotaClient(t, rt).CheckQuotaSupport(t.Context())
			switch {
			case tc.wantIs != nil:
				assert.ErrorIs(t, err, tc.wantIs)
			case tc.wantErr:
				require.Error(t, err)
				assert.NotErrorIs(t, err, ErrQuotasUnsupported)
			default:
				assert.NoError(t, err)
			}
		})
	}
}

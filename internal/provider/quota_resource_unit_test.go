package provider

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

func named(s string) kafkaclient.QuotaEntityName { return kafkaclient.NamedQuotaEntity(s) }

func TestQuotaIDRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		entity kafkaclient.QuotaEntity
		id     string
	}{
		{"user", kafkaclient.QuotaEntity{"user": named("alice")}, "user=alice"},
		{"user_and_client", kafkaclient.QuotaEntity{"user": named("alice"), "client-id": named("billing")}, "user=alice,client-id=billing"},
		{"ip", kafkaclient.QuotaEntity{"ip": named("10.0.0.1")}, "ip=10.0.0.1"},
		{"default_user", kafkaclient.QuotaEntity{"user": kafkaclient.DefaultQuotaEntity()}, "user=<default>"},
		{
			"default_client_named_user",
			kafkaclient.QuotaEntity{"user": named("alice"), "client-id": kafkaclient.DefaultQuotaEntity()},
			"user=alice,client-id=<default>",
		},
		{"name_equal_to_default_token", kafkaclient.QuotaEntity{"user": named("<default>")}, "user=%3Cdefault%3E"},
		{"separators_in_name", kafkaclient.QuotaEntity{"user": named("a,b=c"), "client-id": named("x y")}, "user=a%2Cb%3Dc,client-id=x+y"},
		{"percent_in_name", kafkaclient.QuotaEntity{"client-id": named("100%")}, "client-id=100%25"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.id, encodeQuotaID(tc.entity))
			got, err := decodeQuotaID(tc.id)
			require.NoError(t, err)
			assert.Equal(t, tc.entity, got)
		})
	}
}

func TestQuotaIDDistinctEntitiesDoNotCollide(t *testing.T) {
	entities := []kafkaclient.QuotaEntity{
		{"user": named("a,client-id=b")},
		{"user": named("a"), "client-id": named("b")},
		{"user": named("<default>")},
		{"user": kafkaclient.DefaultQuotaEntity()},
	}
	seen := map[string]int{}
	for i, e := range entities {
		id := encodeQuotaID(e)
		if j, dup := seen[id]; dup {
			t.Fatalf("entities %d and %d share id %q", j, i, id)
		}
		seen[id] = i
	}
}

func TestDecodeQuotaIDNormalizesOrder(t *testing.T) {
	got, err := decodeQuotaID("client-id=b,user=a")
	require.NoError(t, err)
	assert.Equal(t, "user=a,client-id=b", encodeQuotaID(got))
}

func TestDecodeQuotaIDErrors(t *testing.T) {
	tests := []struct {
		name, id, wantSubstr string
	}{
		{"empty", "", "empty"},
		{"no_equals", "alice", `has no "="`},
		{"unknown_type", "topic=a", "invalid quota entity type"},
		{"empty_name", "user=", "empty"},
		{"empty_component", "user=a,", `has no "="`},
		{"duplicate_type", "user=a,user=b", "more than once"},
		{"ip_with_user", "ip=1.1.1.1,user=a", "ip entity cannot be combined"},
		{"bad_escape", "user=%zz", "not correctly escaped"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeQuotaID(tc.id)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantSubstr)
			assert.Contains(t, err.Error(), "for example", "error should show the expected format")
		})
	}
}

func TestValidateQuotaEntityCombination(t *testing.T) {
	tests := []struct {
		name    string
		entity  kafkaclient.QuotaEntity
		wantErr bool
	}{
		{"empty", kafkaclient.QuotaEntity{}, true},
		{"user", kafkaclient.QuotaEntity{"user": named("a")}, false},
		{"client", kafkaclient.QuotaEntity{"client-id": named("a")}, false},
		{"user_client", kafkaclient.QuotaEntity{"user": named("a"), "client-id": named("b")}, false},
		{"ip", kafkaclient.QuotaEntity{"ip": named("1.1.1.1")}, false},
		{"ip_user", kafkaclient.QuotaEntity{"ip": named("1.1.1.1"), "user": named("a")}, true},
		{"ip_client", kafkaclient.QuotaEntity{"ip": named("1.1.1.1"), "client-id": named("a")}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateQuotaEntityCombination(tc.entity)
			assert.Equal(t, tc.wantErr, err != nil, "err = %v", err)
		})
	}
}

func TestQuotaEntityModelMapping(t *testing.T) {
	entities := []kafkaclient.QuotaEntity{
		{"user": named("alice")},
		{"user": kafkaclient.DefaultQuotaEntity(), "client-id": named("c")},
		{"client-id": kafkaclient.DefaultQuotaEntity()},
		{"ip": named("10.1.1.1")},
	}
	for _, entity := range entities {
		t.Run(encodeQuotaID(entity), func(t *testing.T) {
			var m quotaResourceModel
			m.setEntity(entity)
			assert.Equal(t, entity, quotaEntityFromModel(&m))
		})
	}

	t.Run("unset_types_stay_null", func(t *testing.T) {
		var m quotaResourceModel
		m.setEntity(kafkaclient.QuotaEntity{"user": named("alice")})
		assert.Nil(t, m.ClientID)
		assert.Nil(t, m.IP)
		assert.True(t, m.User.Default.IsNull(), "default must be null for a named entity so state matches the plan")
		assert.True(t, quotaEntityPart(kafkaclient.DefaultQuotaEntity()).Name.IsNull())
	})
}

func TestQuotaLimitsModelMapping(t *testing.T) {
	var m quotaResourceModel
	m.setLimits(kafkaclient.QuotaValues{
		kafkaclient.QuotaProducerByteRate:       1024,
		kafkaclient.QuotaConnectionCreationRate: 0.5,
	})
	assert.Equal(t, types.Float64Value(1024), m.ProducerByteRate)
	assert.True(t, m.ConsumerByteRate.IsNull())
	assert.Equal(t, kafkaclient.QuotaValues{
		kafkaclient.QuotaProducerByteRate:       1024,
		kafkaclient.QuotaConnectionCreationRate: 0.5,
	}, m.managedLimits())

	m.setLimits(nil)
	assert.Empty(t, m.managedLimits())

	for _, key := range kafkaclient.QuotaKeys() {
		assert.NotNil(t, m.limit(key), "every supported key needs a model field: %s", key)
	}
}

func TestQuotaExpectationMet(t *testing.T) {
	entry := func(v kafkaclient.QuotaValues) *kafkaclient.QuotaEntry { return &kafkaclient.QuotaEntry{Values: v} }
	tests := []struct {
		name  string
		exp   quotaExpectation
		entry *kafkaclient.QuotaEntry
		want  bool
	}{
		{"absent_quota_meets_empty_expectation", quotaExpectation{}, nil, true},
		{"absent_quota_meets_absent_keys", quotaExpectation{absent: []kafkaclient.QuotaKey{"producer_byte_rate"}}, nil, true},
		{"absent_quota_fails_present_keys", quotaExpectation{present: kafkaclient.QuotaValues{"producer_byte_rate": 1}}, nil, false},
		{"matching_value", quotaExpectation{present: kafkaclient.QuotaValues{"producer_byte_rate": 1}}, entry(kafkaclient.QuotaValues{"producer_byte_rate": 1}), true},
		{"old_value", quotaExpectation{present: kafkaclient.QuotaValues{"producer_byte_rate": 2}}, entry(kafkaclient.QuotaValues{"producer_byte_rate": 1}), false},
		{"missing_key", quotaExpectation{present: kafkaclient.QuotaValues{"consumer_byte_rate": 2}}, entry(kafkaclient.QuotaValues{"producer_byte_rate": 2}), false},
		{"extra_keys_ignored", quotaExpectation{present: kafkaclient.QuotaValues{"producer_byte_rate": 1}}, entry(kafkaclient.QuotaValues{"producer_byte_rate": 1, "consumer_byte_rate": 9}), true},
		{"removed_key_still_present", quotaExpectation{absent: []kafkaclient.QuotaKey{"consumer_byte_rate"}}, entry(kafkaclient.QuotaValues{"consumer_byte_rate": 9}), false},
		{"removed_key_gone", quotaExpectation{absent: []kafkaclient.QuotaKey{"consumer_byte_rate"}}, entry(kafkaclient.QuotaValues{"producer_byte_rate": 1}), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.exp.met(tc.entry))
		})
	}
}

// fakeQuotaAPI answers DescribeQuotas from a script: each call consumes the
// next response, and the last one repeats.
type fakeQuotaAPI struct {
	responses [][]kafkaclient.QuotaEntry
	err       error
	calls     int
	filters   []kafkaclient.QuotaFilter
}

func (f *fakeQuotaAPI) DescribeQuotas(_ context.Context, filter kafkaclient.QuotaFilter) ([]kafkaclient.QuotaEntry, error) {
	f.filters = append(f.filters, filter)
	i := min(f.calls, len(f.responses)-1)
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.responses[i], nil
}

func (f *fakeQuotaAPI) AlterQuotas(context.Context, kafkaclient.QuotaEntity, kafkaclient.QuotaValues, []string) error {
	return nil
}

func quotaEntries(entity kafkaclient.QuotaEntity, v kafkaclient.QuotaValues) []kafkaclient.QuotaEntry {
	return []kafkaclient.QuotaEntry{{Entity: entity, Values: v}}
}

func TestObserveQuota(t *testing.T) {
	entity := kafkaclient.QuotaEntity{"user": named("alice")}
	want := quotaExpectation{present: kafkaclient.QuotaValues{"producer_byte_rate": 20}}
	stale := quotaEntries(entity, kafkaclient.QuotaValues{"producer_byte_rate": 10})
	fresh := quotaEntries(entity, kafkaclient.QuotaValues{"producer_byte_rate": 20})

	t.Run("returns_immediately_when_already_visible", func(t *testing.T) {
		api := &fakeQuotaAPI{responses: [][]kafkaclient.QuotaEntry{fresh}}
		entry, err := observeQuota(t.Context(), api, entity, want, time.Millisecond, time.Second)
		require.NoError(t, err)
		require.NotNil(t, entry)
		assert.Equal(t, 1, api.calls)
	})

	t.Run("polls_until_the_change_is_visible", func(t *testing.T) {
		api := &fakeQuotaAPI{responses: [][]kafkaclient.QuotaEntry{nil, stale, stale, fresh}}
		entry, err := observeQuota(t.Context(), api, entity, want, time.Millisecond, 5*time.Second)
		require.NoError(t, err)
		assert.Equal(t, 20.0, entry.Values["producer_byte_rate"])
		assert.Equal(t, 4, api.calls)
	})

	t.Run("describes_with_an_exact_strict_filter", func(t *testing.T) {
		api := &fakeQuotaAPI{responses: [][]kafkaclient.QuotaEntry{fresh}}
		_, err := observeQuota(t.Context(), api, entity, want, time.Millisecond, time.Second)
		require.NoError(t, err)
		require.Len(t, api.filters, 1)
		assert.Equal(t, kafkaclient.QuotaFilter{Entity: entity, Strict: true}, api.filters[0])
	})

	t.Run("ignores_entries_for_other_entities", func(t *testing.T) {
		other := quotaEntries(kafkaclient.QuotaEntity{"user": named("alice"), "client-id": named("x")}, kafkaclient.QuotaValues{"producer_byte_rate": 20})
		api := &fakeQuotaAPI{responses: [][]kafkaclient.QuotaEntry{other}}
		entry, err := observeQuota(t.Context(), api, entity, quotaExpectation{}, time.Millisecond, time.Second)
		require.NoError(t, err)
		assert.Nil(t, entry)
	})

	t.Run("times_out_with_the_last_observation", func(t *testing.T) {
		api := &fakeQuotaAPI{responses: [][]kafkaclient.QuotaEntry{stale}}
		entry, err := observeQuota(t.Context(), api, entity, want, time.Millisecond, 30*time.Millisecond)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.NotNil(t, entry)
		assert.Equal(t, 10.0, entry.Values["producer_byte_rate"])
	})

	t.Run("honours_a_cancelled_context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		api := &fakeQuotaAPI{responses: [][]kafkaclient.QuotaEntry{stale}}
		_, err := observeQuota(ctx, api, entity, want, time.Millisecond, time.Hour)
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, api.calls, "a cancelled context must not start a request")
	})

	t.Run("stops_on_client_error", func(t *testing.T) {
		api := &fakeQuotaAPI{responses: [][]kafkaclient.QuotaEntry{nil}, err: kafkaclient.ErrQuotaAuthorizationFailed}
		_, err := observeQuota(t.Context(), api, entity, want, time.Millisecond, time.Second)
		require.ErrorIs(t, err, kafkaclient.ErrQuotaAuthorizationFailed)
		assert.Equal(t, 1, api.calls)
	})
}

func TestQuotaResourceSettleAndObserve(t *testing.T) {
	entity := kafkaclient.QuotaEntity{"user": named("alice")}
	want := quotaExpectation{present: kafkaclient.QuotaValues{"producer_byte_rate": 20}}
	stale := quotaEntries(entity, kafkaclient.QuotaValues{"producer_byte_rate": 10})

	t.Run("settle_reports_a_timeout_with_guidance", func(t *testing.T) {
		r := &quotaResource{client: &fakeQuotaAPI{responses: [][]kafkaclient.QuotaEntry{stale}}, timeout: 20 * time.Millisecond, interval: time.Millisecond}
		err := r.settle(t.Context(), entity, want)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Contains(t, err.Error(), "run terraform plan again")
	})

	t.Run("settle_succeeds_when_visible_later", func(t *testing.T) {
		fresh := quotaEntries(entity, kafkaclient.QuotaValues{"producer_byte_rate": 20})
		r := &quotaResource{client: &fakeQuotaAPI{responses: [][]kafkaclient.QuotaEntry{stale, fresh}}, timeout: time.Second, interval: time.Millisecond}
		require.NoError(t, r.settle(t.Context(), entity, want))
	})

	t.Run("observe_returns_last_value_instead_of_failing_when_window_ends", func(t *testing.T) {
		r := &quotaResource{client: &fakeQuotaAPI{responses: [][]kafkaclient.QuotaEntry{stale}}, interval: time.Millisecond}
		entry, err := r.observe(t.Context(), entity, want, 20*time.Millisecond)
		require.NoError(t, err)
		require.NotNil(t, entry)
		assert.Equal(t, 10.0, entry.Values["producer_byte_rate"])
	})

	t.Run("observe_propagates_real_errors", func(t *testing.T) {
		r := &quotaResource{client: &fakeQuotaAPI{responses: [][]kafkaclient.QuotaEntry{nil}, err: errors.New("boom")}, interval: time.Millisecond}
		_, err := r.observe(t.Context(), entity, want, time.Second)
		require.Error(t, err)
	})
}

func TestQuotaErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want []string
	}{
		{"unsupported", fmt.Errorf("alter quotas: %w", kafkaclient.ErrQuotasUnsupported), []string{"Kafka 2.6 or later"}},
		{"unauthorized", fmt.Errorf("describe quotas: %w", kafkaclient.ErrQuotaAuthorizationFailed), []string{"ALTER_CONFIGS", "DESCRIBE_CONFIGS"}},
		{"other", errors.New("connection refused"), []string{"connection refused"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := quotaErrorMessage(tc.err)
			for _, w := range tc.want {
				assert.Contains(t, msg, w)
			}
		})
	}
}

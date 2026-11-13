package kafkaclient

import (
	"errors"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyBrokerError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		sentinel error // nil means no sentinel is added
	}{
		{"unknown_topic", kafka.UnknownTopicOrPartition, ErrTopicNotFound},
		{"invalid_partition_number", kafka.InvalidPartitionNumber, ErrInvalidPartitions},
		{"invalid_replica_assignment", kafka.InvalidReplicaAssignment, ErrInvalidReplicaAssignment},
		{"security_disabled", kafka.SecurityDisabled, ErrSecurityDisabled},
		{"cluster_authorization", kafka.ClusterAuthorizationFailed, ErrNotAuthorized},
		{"topic_authorization", kafka.TopicAuthorizationFailed, ErrNotAuthorized},
		{"group_authorization", kafka.GroupAuthorizationFailed, ErrNotAuthorized},
		{"transactional_id_authorization", kafka.TransactionalIDAuthorizationFailed, ErrNotAuthorized},
		{"delegation_token_authorization", kafka.DelegationTokenAuthorizationFailed, ErrNotAuthorized},
		{"wrapped_error_is_found", errors.Join(errors.New("context"), kafka.UnknownTopicOrPartition), ErrTopicNotFound},
		{"other_error_has_no_sentinel", kafka.InvalidRequest, nil},
		{"unsupported_version_needs_an_extra_rule", kafka.UnsupportedVersion, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyBrokerError(tc.err)

			require.ErrorIs(t, got, tc.err, "the original error stays in the chain")
			for _, s := range []error{ErrTopicNotFound, ErrInvalidPartitions, ErrInvalidReplicaAssignment, ErrSecurityDisabled, ErrNotAuthorized} {
				assert.Equal(t, errors.Is(s, tc.sentinel), errors.Is(got, s), "sentinel %v", s)
			}
		})
	}
}

func TestClassifyBrokerError_ExtraRuleApplies(t *testing.T) {
	rule := sentinelRule{kafka.UnsupportedVersion, ErrIncrementalAlterUnsupported}

	got := classifyBrokerError(kafka.UnsupportedVersion, rule)

	require.ErrorIs(t, got, ErrIncrementalAlterUnsupported)
	assert.ErrorIs(t, got, kafka.UnsupportedVersion)
}

func TestCheckResults(t *testing.T) {
	errBoom := errors.New("boom")
	errOther := errors.New("other")

	tests := []struct {
		name     string
		expected []string
		results  map[string]error
		// want is the expected error message; empty means no error.
		want string
	}{
		{name: "nothing_expected_and_no_results", results: map[string]error{}},
		{name: "all_results_succeed", expected: []string{"orders"}, results: map[string]error{"orders": nil}},
		{name: "one_error", results: map[string]error{"orders": errBoom}, want: `act "orders": boom`},
		{
			name:    "several_errors_sorted_by_name",
			results: map[string]error{"zeta": errOther, "alpha": errBoom},
			want:    "act \"alpha\": boom\nact \"zeta\": other",
		},
		{
			name:     "nil_entry_is_skipped",
			expected: []string{"a", "b"},
			results:  map[string]error{"a": nil, "b": errBoom},
			want:     `act "b": boom`,
		},
		{
			name:     "expected_name_without_result_fails",
			expected: []string{"orders"},
			results:  map[string]error{},
			want:     `act "orders": the broker returned no result`,
		},
		{
			name:     "missing_and_failed_results_are_sorted_together",
			expected: []string{"b", "c", "c"},
			results:  map[string]error{"a": errBoom, "b": nil},
			want:     "act \"a\": boom\nact \"c\": the broker returned no result",
		},
		{
			name:    "result_that_nobody_expected_is_still_checked",
			results: map[string]error{"extra": errBoom},
			want:    `act "extra": boom`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := checkResults("act", tc.expected, tc.results)

			if tc.want == "" {
				require.NoError(t, got)
				return
			}
			require.Error(t, got)
			assert.Equal(t, tc.want, got.Error())
		})
	}
}

func TestCheckResults_MissingResultMatchesErrNoResult(t *testing.T) {
	got := checkResults("act", []string{"orders"}, nil)

	assert.ErrorIs(t, got, errNoResult)
}

func TestCheckResults_KeepsCauseAndAddsSentinel(t *testing.T) {
	got := checkResults("act", nil, map[string]error{"orders": kafka.UnknownTopicOrPartition})

	require.ErrorIs(t, got, kafka.UnknownTopicOrPartition)
	assert.ErrorIs(t, got, ErrTopicNotFound)
}

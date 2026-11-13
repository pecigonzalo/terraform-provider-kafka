package kafkaclient

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/protocol/alterconfigs"
	"github.com/segmentio/kafka-go/protocol/createtopics"
	"github.com/segmentio/kafka-go/protocol/deletetopics"
	"github.com/segmentio/kafka-go/protocol/incrementalalterconfigs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// topicWriteRoundTripper serves canned CreateTopics, DeleteTopics and
// AlterConfigs responses and records the requests it receives.
type topicWriteRoundTripper struct {
	err     error
	create  *createtopics.Response
	delete  *deletetopics.Response
	alter   *alterconfigs.Response

	incremental  *incrementalalterconfigs.Response
	incrementals []*incrementalalterconfigs.Request
	creates []*createtopics.Request
	deletes []*deletetopics.Request
	alters  []*alterconfigs.Request
}

func (f *topicWriteRoundTripper) RoundTrip(_ context.Context, _ net.Addr, req kafka.Request) (kafka.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	switch r := req.(type) {
	case *createtopics.Request:
		f.creates = append(f.creates, r)
		return f.create, nil
	case *deletetopics.Request:
		f.deletes = append(f.deletes, r)
		return f.delete, nil
	case *incrementalalterconfigs.Request:
		f.incrementals = append(f.incrementals, r)
		return f.incremental, nil
	case *alterconfigs.Request:
		f.alters = append(f.alters, r)
		return f.alter, nil
	}
	return nil, errors.New("unexpected request")
}

func newTopicWriteClient(t *testing.T, rt *topicWriteRoundTripper) *Client {
	t.Helper()
	c, err := newClient(t.Context(), Config{BootstrapServer: "b:9092"}, fakeAWSLoader)
	require.NoError(t, err)
	c.kc.Transport = rt
	return c
}

func code(e kafka.Error) int16 { return int16(e) }

func TestClientCreateTopic(t *testing.T) {
	transportErr := errors.New("connection refused")
	tests := []struct {
		name    string
		rt      *topicWriteRoundTripper
		wantErr error  // matched with errors.Is; nil means success
		wantMsg string // substring of the error message
	}{
		{
			name: "success",
			rt:   &topicWriteRoundTripper{create: &createtopics.Response{Topics: []createtopics.ResponseTopic{{Name: "orders"}}}},
		},
		{
			name:    "transport_error",
			rt:      &topicWriteRoundTripper{err: transportErr},
			wantErr: transportErr,
			wantMsg: `create topic "orders"`,
		},
		{
			name: "per_topic_error_in_body",
			rt: &topicWriteRoundTripper{create: &createtopics.Response{Topics: []createtopics.ResponseTopic{
				{Name: "orders", ErrorCode: code(kafka.TopicAlreadyExists)},
			}}},
			wantErr: kafka.TopicAlreadyExists,
			wantMsg: `create topic "orders"`,
		},
		{
			name: "invalid_replication_factor",
			rt: &topicWriteRoundTripper{create: &createtopics.Response{Topics: []createtopics.ResponseTopic{
				{Name: "orders", ErrorCode: code(kafka.InvalidReplicationFactor)},
			}}},
			wantErr: kafka.InvalidReplicationFactor,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTopicWriteClient(t, tc.rt)

			err := c.CreateTopic(t.Context(), NewTopic{
				Name: "orders", Partitions: 3, ReplicationFactor: 2,
				Config: map[string]string{"retention.ms": "1000", "cleanup.policy": "compact"},
			})

			if tc.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Contains(t, err.Error(), tc.wantMsg)
			}
			if tc.rt.err == nil {
				require.Len(t, tc.rt.creates, 1)
			}
		})
	}
}

func TestClientCreateTopic_Request(t *testing.T) {
	rt := &topicWriteRoundTripper{create: &createtopics.Response{Topics: []createtopics.ResponseTopic{{Name: "orders"}}}}
	c := newTopicWriteClient(t, rt)

	require.NoError(t, c.CreateTopic(t.Context(), NewTopic{
		Name: "orders", Partitions: 3, ReplicationFactor: 2,
		Config: map[string]string{"retention.ms": "1000", "cleanup.policy": "compact"},
	}))

	require.Len(t, rt.creates, 1)
	require.Len(t, rt.creates[0].Topics, 1)
	got := rt.creates[0].Topics[0]
	assert.Equal(t, "orders", got.Name)
	assert.EqualValues(t, 3, got.NumPartitions)
	assert.EqualValues(t, 2, got.ReplicationFactor)
	assert.Equal(t, []createtopics.RequestConfig{
		{Name: "cleanup.policy", Value: "compact"},
		{Name: "retention.ms", Value: "1000"},
	}, got.Configs, "config entries are sent sorted by key")
}

func TestClientDeleteTopics(t *testing.T) {
	transportErr := errors.New("connection refused")
	tests := []struct {
		name         string
		rt           *topicWriteRoundTripper
		names        []string
		wantErr      bool
		wantNotFound bool
		wantIs       error
	}{
		{
			name:  "success",
			rt:    &topicWriteRoundTripper{delete: &deletetopics.Response{Responses: []deletetopics.ResponseTopic{{Name: "orders"}}}},
			names: []string{"orders"},
		},
		{
			name:    "transport_error",
			rt:      &topicWriteRoundTripper{err: transportErr},
			names:   []string{"orders"},
			wantErr: true,
			wantIs:  transportErr,
		},
		{
			name: "missing_topic_is_not_found",
			rt: &topicWriteRoundTripper{delete: &deletetopics.Response{Responses: []deletetopics.ResponseTopic{
				{Name: "orders", ErrorCode: code(kafka.UnknownTopicOrPartition)},
			}}},
			names:        []string{"orders"},
			wantErr:      true,
			wantNotFound: true,
			wantIs:       kafka.UnknownTopicOrPartition,
		},
		{
			name: "other_per_topic_error_is_not_not_found",
			rt: &topicWriteRoundTripper{delete: &deletetopics.Response{Responses: []deletetopics.ResponseTopic{
				{Name: "orders", ErrorCode: code(kafka.TopicAuthorizationFailed)},
			}}},
			names:   []string{"orders"},
			wantErr: true,
			wantIs:  kafka.TopicAuthorizationFailed,
		},
		{
			name: "several_topics_report_each_failure",
			rt: &topicWriteRoundTripper{delete: &deletetopics.Response{Responses: []deletetopics.ResponseTopic{
				{Name: "a"},
				{Name: "b", ErrorCode: code(kafka.TopicAuthorizationFailed)},
				{Name: "c", ErrorCode: code(kafka.UnknownTopicOrPartition)},
			}}},
			names:        []string{"a", "b", "c"},
			wantErr:      true,
			wantNotFound: true,
			wantIs:       kafka.TopicAuthorizationFailed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTopicWriteClient(t, tc.rt)

			err := c.DeleteTopics(t.Context(), tc.names...)

			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.ErrorIs(t, err, tc.wantIs)
			assert.Equal(t, tc.wantNotFound, errors.Is(err, ErrTopicNotFound))
		})
	}
}

func TestClientDeleteTopics_SendsNames(t *testing.T) {
	rt := &topicWriteRoundTripper{delete: &deletetopics.Response{}}
	c := newTopicWriteClient(t, rt)

	require.NoError(t, c.DeleteTopics(t.Context(), "a", "b"))

	require.Len(t, rt.deletes, 1)
	assert.Equal(t, []string{"a", "b"}, rt.deletes[0].TopicNames)
}

func TestClientAlterTopicConfig(t *testing.T) {
	transportErr := errors.New("connection refused")
	topicType := int8(kafka.ResourceTypeTopic)
	tests := []struct {
		name            string
		rt              *topicWriteRoundTripper
		wantIs          error // nil means success
		wantNotFound    bool
		wantUnsupported bool
	}{
		{
			name: "success",
			rt: &topicWriteRoundTripper{incremental: &incrementalalterconfigs.Response{Responses: []incrementalalterconfigs.ResponseAlterResponse{
				{ResourceType: topicType, ResourceName: "orders"},
			}}},
		},
		{
			name:   "transport_error",
			rt:     &topicWriteRoundTripper{err: transportErr},
			wantIs: transportErr,
		},
		{
			name: "per_resource_error_in_body",
			rt: &topicWriteRoundTripper{incremental: &incrementalalterconfigs.Response{Responses: []incrementalalterconfigs.ResponseAlterResponse{
				{ResourceType: topicType, ResourceName: "orders", ErrorCode: code(kafka.InvalidConfiguration), ErrorMessage: "bad value"},
			}}},
			wantIs: kafka.InvalidConfiguration,
		},
		{
			name: "missing_topic_is_not_found",
			rt: &topicWriteRoundTripper{incremental: &incrementalalterconfigs.Response{Responses: []incrementalalterconfigs.ResponseAlterResponse{
				{ResourceType: topicType, ResourceName: "orders", ErrorCode: code(kafka.UnknownTopicOrPartition)},
			}}},
			wantIs:       kafka.UnknownTopicOrPartition,
			wantNotFound: true,
		},
		{
			name: "unsupported_version_in_body_names_the_minimum_version",
			rt: &topicWriteRoundTripper{incremental: &incrementalalterconfigs.Response{Responses: []incrementalalterconfigs.ResponseAlterResponse{
				{ResourceType: topicType, ResourceName: "orders", ErrorCode: code(kafka.UnsupportedVersion)},
			}}},
			wantIs:          kafka.UnsupportedVersion,
			wantUnsupported: true,
		},
		{
			name:            "unsupported_version_as_call_error_names_the_minimum_version",
			rt:              &topicWriteRoundTripper{err: kafka.UnsupportedVersion},
			wantIs:          kafka.UnsupportedVersion,
			wantUnsupported: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTopicWriteClient(t, tc.rt)

			err := c.AlterTopicConfig(t.Context(), "orders", nil, map[string]string{"retention.ms": "1000"})

			if tc.wantIs == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.wantIs)
			assert.Equal(t, tc.wantNotFound, errors.Is(err, ErrTopicNotFound))
			assert.Equal(t, tc.wantUnsupported, errors.Is(err, ErrIncrementalAlterUnsupported))
			if tc.wantUnsupported {
				assert.Contains(t, err.Error(), "Kafka 2.3")
			}
			assert.Contains(t, err.Error(), `"orders"`)
		})
	}
}

func TestClientAlterTopicConfig_Request(t *testing.T) {
	rt := &topicWriteRoundTripper{incremental: &incrementalalterconfigs.Response{}}
	c := newTopicWriteClient(t, rt)

	previous := map[string]string{"gone": "1", "kept": "old", "also.gone": "x"}
	config := map[string]string{"new": "2", "kept": "3"}
	require.NoError(t, c.AlterTopicConfig(t.Context(), "orders", previous, config))

	require.Len(t, rt.incrementals, 1)
	require.Len(t, rt.incrementals[0].Resources, 1)
	res := rt.incrementals[0].Resources[0]
	assert.Equal(t, int8(kafka.ResourceTypeTopic), res.ResourceType)
	assert.Equal(t, "orders", res.ResourceName)
	assert.Equal(t, []incrementalalterconfigs.RequestConfig{
		{Name: "kept", Value: "3", ConfigOperation: int8(kafka.ConfigOperationSet)},
		{Name: "new", Value: "2", ConfigOperation: int8(kafka.ConfigOperationSet)},
		{Name: "also.gone", ConfigOperation: int8(kafka.ConfigOperationDelete)},
		{Name: "gone", ConfigOperation: int8(kafka.ConfigOperationDelete)},
	}, res.Configs, "set the managed keys, delete the removed keys, and leave all others")
}

func TestClientAlterTopicConfig_RemovingEveryKeyOnlyDeletes(t *testing.T) {
	rt := &topicWriteRoundTripper{incremental: &incrementalalterconfigs.Response{}}
	c := newTopicWriteClient(t, rt)

	require.NoError(t, c.AlterTopicConfig(t.Context(), "orders", map[string]string{"a": "1"}, nil))

	require.Len(t, rt.incrementals, 1)
	assert.Equal(t, []incrementalalterconfigs.RequestConfig{
		{Name: "a", ConfigOperation: int8(kafka.ConfigOperationDelete)},
	}, rt.incrementals[0].Resources[0].Configs)
}

func TestClientAlterTopicConfig_NoChangeSendsNothing(t *testing.T) {
	rt := &topicWriteRoundTripper{}
	c := newTopicWriteClient(t, rt)

	require.NoError(t, c.AlterTopicConfig(t.Context(), "orders", nil, nil))

	assert.Empty(t, rt.incrementals)
	assert.Empty(t, rt.alters, "the full-replace API is never used")
}

func TestJoinResponseErrors(t *testing.T) {
	errBoom := errors.New("boom")
	errOther := errors.New("other")

	tests := []struct {
		name string
		errs map[string]error
		// want is the expected error message; empty means no error.
		want string
	}{
		{name: "empty_map_returns_nil", errs: map[string]error{}},
		{name: "all_nil_returns_nil", errs: map[string]error{"orders": nil, "users": nil}},
		{name: "one_error", errs: map[string]error{"orders": errBoom}, want: `act "orders": boom`},
		{
			name: "several_errors_sorted_by_name",
			errs: map[string]error{"zeta": errOther, "alpha": errBoom},
			want: "act \"alpha\": boom\nact \"zeta\": other",
		},
		{name: "nil_entry_is_skipped", errs: map[string]error{"a": nil, "b": errBoom}, want: `act "b": boom`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := joinResponseErrors(tc.errs, "act", nil)

			if tc.want == "" {
				require.NoError(t, got)
				return
			}
			require.Error(t, got)
			assert.Equal(t, tc.want, got.Error())
		})
	}
}

func TestJoinResponseErrors_KeepsWrappedCause(t *testing.T) {
	errBoom := errors.New("boom")

	got := joinResponseErrors(map[string]error{"orders": errBoom}, "act", nil)

	assert.ErrorIs(t, got, errBoom)
}

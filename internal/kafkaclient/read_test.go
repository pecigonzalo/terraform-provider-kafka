package kafkaclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/protocol/describeconfigs"
	"github.com/segmentio/kafka-go/protocol/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedRoundTripper answers metadata and describe-configs requests from
// canned data and records what was asked.
type scriptedRoundTripper struct {
	brokers []metadata.ResponseBroker
	topics  []metadata.ResponseTopic
	configs map[string]describeconfigs.ResponseResource
	err     error

	configRequests int
}

func (s *scriptedRoundTripper) RoundTrip(_ context.Context, _ net.Addr, req kafka.Request) (kafka.Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	switch r := req.(type) {
	case *metadata.Request:
		resp := &metadata.Response{Brokers: s.brokers}
		for _, t := range s.topics {
			if len(r.TopicNames) == 0 || slices.Contains(r.TopicNames, t.Name) {
				resp.Topics = append(resp.Topics, t)
			}
		}
		for _, n := range r.TopicNames {
			if !hasTopic(s.topics, n) {
				resp.Topics = append(resp.Topics, metadata.ResponseTopic{
					Name: n, ErrorCode: int16(kafka.UnknownTopicOrPartition),
				})
			}
		}
		return resp, nil
	case *describeconfigs.Request:
		s.configRequests++
		resp := &describeconfigs.Response{}
		for _, res := range r.Resources {
			cfg, ok := s.configs[res.ResourceName]
			if !ok {
				cfg = describeconfigs.ResponseResource{ResourceName: res.ResourceName}
			}
			resp.Resources = append(resp.Resources, cfg)
		}
		return resp, nil
	}
	return nil, fmt.Errorf("unexpected request %T", req)
}

func hasTopic(ts []metadata.ResponseTopic, name string) bool {
	for _, t := range ts {
		if t.Name == name {
			return true
		}
	}
	return false
}

func newTestReader(t *testing.T, rt kafka.RoundTripper) *Reader {
	t.Helper()
	c, err := newClient(t.Context(), Config{BootstrapServer: "b:9092"}, fakeAWSLoader)
	require.NoError(t, err)
	c.kc.Transport = rt
	return c.Reader
}

func entry(name, value string, source int8) describeconfigs.ResponseConfigEntry {
	return describeconfigs.ResponseConfigEntry{ConfigName: name, ConfigValue: value, ConfigSource: source}
}

func partition(id, leader int32, replicas, isr []int32) metadata.ResponsePartition {
	return metadata.ResponsePartition{PartitionIndex: id, LeaderID: leader, ReplicaNodes: replicas, IsrNodes: isr}
}

func fixture() *scriptedRoundTripper {
	return &scriptedRoundTripper{
		brokers: []metadata.ResponseBroker{
			{NodeID: 1, Host: "b1", Port: 9092, Rack: "a"},
			{NodeID: 2, Host: "b2", Port: 9093},
			{NodeID: 3, Host: "b3", Port: 9094, Rack: "c"},
		},
		topics: []metadata.ResponseTopic{
			{Name: "even", Partitions: []metadata.ResponsePartition{
				// deliberately out of order to check sorting by ID
				partition(1, 2, []int32{2, 3}, []int32{2}),
				partition(0, 1, []int32{1, 2}, []int32{1, 2}),
			}},
			{Name: "uneven", Partitions: []metadata.ResponsePartition{
				partition(0, 1, []int32{1, 2, 3}, []int32{1, 2, 3}),
				partition(1, 2, []int32{2}, []int32{2}),
			}},
		},
		configs: map[string]describeconfigs.ResponseResource{
			"even": {ResourceName: "even", ConfigEntries: []describeconfigs.ResponseConfigEntry{
				entry("retention.ms", "1000", 1),
				entry("min.insync.replicas", "2", 2),     // dynamic broker config is kept
				entry("segment.bytes", "1073741824", 5),  // default: dropped
				entry("compression.type", "producer", 4), // static broker: dropped
				{ConfigName: "ssl.key", IsSensitive: true, ConfigSource: 1},
			}},
			"uneven": {ResourceName: "uneven"},
		},
	}
}

func TestReaderTopic(t *testing.T) {
	r := newTestReader(t, fixture())

	got, err := r.Topic(t.Context(), "even")
	require.NoError(t, err)

	assert.Equal(t, "even", got.Name)
	assert.Equal(t, []PartitionInfo{
		{ID: 0, Leader: 1, Replicas: []int{1, 2}, ISR: []int{1, 2}},
		{ID: 1, Leader: 2, Replicas: []int{2, 3}, ISR: []int{2}},
	}, got.Partitions)
	assert.Equal(t, map[string]string{
		"retention.ms":        "1000",
		"min.insync.replicas": "2",
		"ssl.key":             "SENSITIVE",
	}, got.Config)
}

func TestReaderTopic_UnevenReplicasRetained(t *testing.T) {
	r := newTestReader(t, fixture())

	got, err := r.Topic(t.Context(), "uneven")
	require.NoError(t, err)

	require.Len(t, got.Partitions, 2)
	assert.Len(t, got.Partitions[0].Replicas, 3)
	assert.Len(t, got.Partitions[1].Replicas, 1)
	assert.NotNil(t, got.Config, "topic with no overrides has an empty, non-nil config")
	assert.Empty(t, got.Config)
}

func TestReaderTopic_NotFound(t *testing.T) {
	rt := fixture()
	r := newTestReader(t, rt)

	_, err := r.Topic(t.Context(), "missing")

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTopicNotFound)
	assert.Zero(t, rt.configRequests, "no config request for a missing topic")
}

func TestReaderTopic_ConfigResourceErrors(t *testing.T) {
	tests := []struct {
		name         string
		code         kafka.Error
		wantNotFound bool
	}{
		{name: "topic_deleted_between_requests", code: kafka.UnknownTopicOrPartition, wantNotFound: true},
		{name: "authorization_failed", code: kafka.TopicAuthorizationFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := fixture()
			rt.configs["even"] = describeconfigs.ResponseResource{ResourceName: "even", ErrorCode: int16(tc.code)}
			r := newTestReader(t, rt)

			_, err := r.Topic(t.Context(), "even")

			require.Error(t, err)
			assert.Equal(t, tc.wantNotFound, errors.Is(err, ErrTopicNotFound))
			if !tc.wantNotFound {
				assert.ErrorIs(t, err, tc.code)
			}
		})
	}
}

func TestReaderTopic_MetadataTopicError(t *testing.T) {
	rt := fixture()
	rt.topics = []metadata.ResponseTopic{{Name: "even", ErrorCode: int16(kafka.TopicAuthorizationFailed)}}
	r := newTestReader(t, rt)

	_, err := r.Topic(t.Context(), "even")

	require.Error(t, err)
	assert.ErrorIs(t, err, kafka.TopicAuthorizationFailed)
	assert.NotErrorIs(t, err, ErrTopicNotFound)
}

func TestReaderTopics(t *testing.T) {
	tests := []struct {
		name      string
		rt        *scriptedRoundTripper
		wantNames []string
	}{
		{name: "all_topics", rt: fixture(), wantNames: []string{"even", "uneven"}},
		{name: "empty_cluster", rt: &scriptedRoundTripper{}, wantNames: []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestReader(t, tc.rt)

			got, err := r.Topics(t.Context())

			require.NoError(t, err)
			names := make([]string, 0, len(got))
			for _, ti := range got {
				names = append(names, ti.Name)
			}
			assert.Equal(t, tc.wantNames, names)
			if len(tc.wantNames) == 0 {
				assert.Zero(t, tc.rt.configRequests, "no config request for an empty cluster")
			}
		})
	}
}

func TestReaderBrokers(t *testing.T) {
	r := newTestReader(t, fixture())

	got, err := r.Brokers(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []BrokerInfo{
		{ID: 1, Host: "b1", Port: 9092, Rack: "a"},
		{ID: 2, Host: "b2", Port: 9093},
		{ID: 3, Host: "b3", Port: 9094, Rack: "c"},
	}, got)

	ids, err := r.BrokerIDs(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, ids)
}

func TestReaderBrokers_EmptyCluster(t *testing.T) {
	r := newTestReader(t, &scriptedRoundTripper{})

	got, err := r.Brokers(t.Context())
	require.NoError(t, err)
	assert.Empty(t, got)

	ids, err := r.BrokerIDs(t.Context())
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestReader_TransportErrorWrapped(t *testing.T) {
	sentinel := errors.New("connection refused")
	r := newTestReader(t, &scriptedRoundTripper{err: sentinel})
	ctx := t.Context()

	calls := map[string]func() error{
		"Topic":     func() error { _, err := r.Topic(ctx, "x"); return err },
		"Topics":    func() error { _, err := r.Topics(ctx); return err },
		"Brokers":   func() error { _, err := r.Brokers(ctx); return err },
		"BrokerIDs": func() error { _, err := r.BrokerIDs(ctx); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			require.Error(t, err)
			assert.ErrorIs(t, err, sentinel)
			assert.NotErrorIs(t, err, ErrTopicNotFound)
		})
	}
}

func TestReader_DescribeConfigsTransportErrorWrapped(t *testing.T) {
	sentinel := errors.New("boom")
	rt := &failOnConfigs{scriptedRoundTripper: fixture(), err: sentinel}
	r := newTestReader(t, rt)

	_, err := r.Topic(t.Context(), "even")

	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.NotErrorIs(t, err, ErrTopicNotFound)
}

type failOnConfigs struct {
	*scriptedRoundTripper
	err error
}

func (f *failOnConfigs) RoundTrip(ctx context.Context, a net.Addr, req kafka.Request) (kafka.Response, error) {
	if _, ok := req.(*describeconfigs.Request); ok {
		return nil, f.err
	}
	return f.scriptedRoundTripper.RoundTrip(ctx, a, req)
}

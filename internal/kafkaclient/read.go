package kafkaclient

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/segmentio/kafka-go"
)

// ErrTopicNotFound is returned (wrapped) when the cluster reports that a
// topic does not exist. Match it with [errors.Is].
var ErrTopicNotFound = errors.New("topic not found")

// Config source values reported by DescribeConfigs (KIP-226). Entries coming
// from the broker defaults are not part of a topic's own configuration.
const (
	configSourceStaticBroker int8 = 4
	configSourceDefault      int8 = 5

	// sensitivePlaceholder is reported for sensitive entries whose value the
	// broker withholds.
	sensitivePlaceholder = "SENSITIVE"
)

// BrokerInfo describes a broker registered in the cluster.
type BrokerInfo struct {
	ID   int
	Host string
	Port int
	// Rack is empty when the broker has no rack configured.
	Rack string
}

// PartitionInfo describes one partition of a topic. Replicas is the full
// replica list as reported by the cluster, so callers can detect uneven
// replication across partitions.
type PartitionInfo struct {
	ID       int
	Leader   int
	Replicas []int
	ISR      []int
}

// TopicInfo describes a topic.
//
// Config holds the topic's explicitly set configuration, as topicctl did:
// entries that only reflect broker defaults or static broker configuration
// are omitted. Sensitive entries with a withheld value are reported as
// "SENSITIVE". Partitions are ordered by partition ID.
type TopicInfo struct {
	Name       string
	Partitions []PartitionInfo
	Config     map[string]string
}

// Topic returns the named topic. It returns an error wrapping
// [ErrTopicNotFound] when the topic does not exist.
func (r *Reader) Topic(ctx context.Context, name string) (TopicInfo, error) {
	topics, err := r.topics(ctx, []string{name})
	if err != nil {
		return TopicInfo{}, err
	}
	if len(topics) == 0 {
		return TopicInfo{}, fmt.Errorf("topic %q: %w", name, ErrTopicNotFound)
	}
	return topics[0], nil
}

// Topics returns every topic in the cluster, including internal ones.
func (r *Reader) Topics(ctx context.Context) ([]TopicInfo, error) {
	return r.topics(ctx, nil)
}

// Brokers returns the brokers registered in the cluster.
func (r *Reader) Brokers(ctx context.Context) ([]BrokerInfo, error) {
	md, err := r.Metadata(ctx)
	if err != nil {
		return nil, err
	}
	brokers := make([]BrokerInfo, 0, len(md.Brokers))
	for _, b := range md.Brokers {
		brokers = append(brokers, BrokerInfo{ID: b.ID, Host: b.Host, Port: b.Port, Rack: b.Rack})
	}
	return brokers, nil
}

// BrokerIDs returns the IDs of the brokers registered in the cluster.
func (r *Reader) BrokerIDs(ctx context.Context) ([]int, error) {
	brokers, err := r.Brokers(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(brokers))
	for _, b := range brokers {
		ids = append(ids, b.ID)
	}
	return ids, nil
}

// topics reads metadata and configuration for names, or for all topics when
// names is empty. Topics that do not exist are omitted from the result.
func (r *Reader) topics(ctx context.Context, names []string) ([]TopicInfo, error) {
	md, err := r.Metadata(ctx, names...)
	if err != nil {
		return nil, err
	}

	infos := make([]TopicInfo, 0, len(md.Topics))
	resources := make([]kafka.DescribeConfigRequestResource, 0, len(md.Topics))
	for _, t := range md.Topics {
		if t.Error != nil {
			if errors.Is(t.Error, kafka.UnknownTopicOrPartition) {
				continue
			}
			return nil, fmt.Errorf("fetch metadata for topic %q: %w", t.Name, t.Error)
		}
		info := TopicInfo{Name: t.Name, Partitions: make([]PartitionInfo, 0, len(t.Partitions))}
		for _, p := range t.Partitions {
			info.Partitions = append(info.Partitions, PartitionInfo{
				ID:       p.ID,
				Leader:   p.Leader.ID,
				Replicas: brokerIDs(p.Replicas),
				ISR:      brokerIDs(p.Isr),
			})
		}
		slices.SortFunc(info.Partitions, func(a, b PartitionInfo) int { return a.ID - b.ID })
		infos = append(infos, info)
		resources = append(resources, kafka.DescribeConfigRequestResource{
			ResourceType: kafka.ResourceTypeTopic,
			ResourceName: t.Name,
		})
	}
	if len(infos) == 0 {
		return infos, nil
	}

	resp, err := r.kc.DescribeConfigs(ctx, &kafka.DescribeConfigsRequest{Resources: resources})
	if err != nil {
		return nil, fmt.Errorf("describe topic configs: %w", err)
	}
	configs := make(map[string]map[string]string, len(resp.Resources))
	for _, res := range resp.Resources {
		if res.Error != nil {
			if errors.Is(res.Error, kafka.UnknownTopicOrPartition) {
				// Deleted between the metadata and config requests.
				continue
			}
			return nil, fmt.Errorf("describe configs for topic %q: %w", res.ResourceName, res.Error)
		}
		configs[res.ResourceName] = topicConfig(res.ConfigEntries)
	}

	// Drop topics deleted between the two requests, keeping order.
	out := infos[:0]
	for _, info := range infos {
		cfg, ok := configs[info.Name]
		if !ok {
			continue
		}
		info.Config = cfg
		out = append(out, info)
	}
	return out, nil
}

func topicConfig(entries []kafka.DescribeConfigResponseConfigEntry) map[string]string {
	cfg := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDefault || e.ConfigSource == configSourceDefault || e.ConfigSource == configSourceStaticBroker {
			continue
		}
		if e.ConfigValue == "" && e.IsSensitive {
			cfg[e.ConfigName] = sensitivePlaceholder
			continue
		}
		cfg[e.ConfigName] = e.ConfigValue
	}
	return cfg
}

func brokerIDs(brokers []kafka.Broker) []int {
	ids := make([]int, 0, len(brokers))
	for _, b := range brokers {
		ids = append(ids, b.ID)
	}
	return ids
}

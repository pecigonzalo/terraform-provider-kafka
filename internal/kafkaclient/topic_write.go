package kafkaclient

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/segmentio/kafka-go"
)

// NewTopic describes a topic to create.
type NewTopic struct {
	Name              string
	Partitions        int
	ReplicationFactor int
	// Config holds the topic configuration overrides. Nil or empty leaves
	// every setting at the broker default.
	Config map[string]string
}

// CreateTopic creates a topic. The broker accepting the request is not the
// same as every broker already knowing the topic: other brokers learn about it
// asynchronously, so a read straight afterwards can still miss it.
//
// kafka-go reports per-topic failures in the response body rather than as the
// call error; both are returned here. A response with no result for the topic
// is an error too, because the topic may not exist. A failure reported by the broker keeps
// its [kafka.Error] in the chain, so callers can match, for example,
// [kafka.TopicAlreadyExists] with [errors.Is].
func (c *Client) CreateTopic(ctx context.Context, topic NewTopic) error {
	resp, err := c.kc.CreateTopics(ctx, &kafka.CreateTopicsRequest{
		Topics: []kafka.TopicConfig{{
			Topic:             topic.Name,
			NumPartitions:     topic.Partitions,
			ReplicationFactor: topic.ReplicationFactor,
			ConfigEntries:     configEntries(topic.Config),
		}},
	})
	if err != nil {
		return fmt.Errorf("create topic %q: %w", topic.Name, err)
	}
	return checkResults("create topic", []string{topic.Name}, resp.Errors)
}

// DeleteTopics deletes the named topics. Failures reported for individual
// topics are joined into the returned error, ordered by topic name. A topic
// with no result in the response is a failure too. A topic
// that does not exist yields an error wrapping [ErrTopicNotFound], so with a
// single name a caller can treat "already gone" as success:
//
//	if err := c.DeleteTopics(ctx, name); err != nil && !errors.Is(err, ErrTopicNotFound) { ... }
//
// With several names that check would also swallow the other failures, so
// inspect the joined errors individually in that case.
func (c *Client) DeleteTopics(ctx context.Context, names ...string) error {
	resp, err := c.kc.DeleteTopics(ctx, &kafka.DeleteTopicsRequest{Topics: names})
	if err != nil {
		return fmt.Errorf("delete topics %q: %w", names, err)
	}
	return checkResults("delete topic", names, resp.Errors)
}

// ErrIncrementalAlterUnsupported means the broker does not offer incremental
// alter of configuration, which needs Kafka 2.3 or later.
var ErrIncrementalAlterUnsupported = errors.New(
	"the broker does not support incremental alter of configuration; this needs Kafka 2.3 or later")

// incrementalAlterUnsupported marks an unsupported-version error as
// [ErrIncrementalAlterUnsupported]. It applies only to the alter call.
var incrementalAlterUnsupported = sentinelRule{kafka.UnsupportedVersion, ErrIncrementalAlterUnsupported}

// AlterTopicConfig changes only the configuration keys that Terraform manages.
// It sets every key in config. It deletes every key that is in previous and not
// in config, so that setting returns to the broker default. It does not touch
// any other key, so a setting made outside Terraform stays as it is. The call
// uses the incremental alter API (Kafka 2.3 or later) and sends nothing when
// there is no change.
//
// Failures reported per resource are returned too, and so is a response with no
// result for the topic. A topic that does not
// exist yields an error wrapping [ErrTopicNotFound]. A broker without the API
// yields an error wrapping [ErrIncrementalAlterUnsupported].
func (c *Client) AlterTopicConfig(ctx context.Context, name string, previous, config map[string]string) error {
	configs := incrementalConfigs(previous, config)
	if len(configs) == 0 {
		return nil
	}
	resp, err := c.kc.IncrementalAlterConfigs(ctx, &kafka.IncrementalAlterConfigsRequest{
		Resources: []kafka.IncrementalAlterConfigsRequestResource{{
			ResourceType: kafka.ResourceTypeTopic,
			ResourceName: name,
			Configs:      configs,
		}},
	})
	if err != nil {
		return fmt.Errorf("alter config of topic %q: %w", name, classifyBrokerError(err, incrementalAlterUnsupported))
	}
	results := make(map[string]error, len(resp.Resources))
	for _, res := range resp.Resources {
		results[res.ResourceName] = res.Error
	}
	return checkResults("alter config of topic", []string{name}, results, incrementalAlterUnsupported)
}

// incrementalConfigs builds the operations for AlterTopicConfig: a set for
// every key in config, then a delete for every key that is only in previous.
// Each group is ordered by key so requests are deterministic.
func incrementalConfigs(previous, config map[string]string) []kafka.IncrementalAlterConfigsRequestConfig {
	configs := make([]kafka.IncrementalAlterConfigsRequestConfig, 0, len(config))
	for _, k := range slices.Sorted(maps.Keys(config)) {
		configs = append(configs, kafka.IncrementalAlterConfigsRequestConfig{
			Name: k, Value: config[k], ConfigOperation: kafka.ConfigOperationSet,
		})
	}
	for _, k := range slices.Sorted(maps.Keys(previous)) {
		if _, kept := config[k]; !kept {
			configs = append(configs, kafka.IncrementalAlterConfigsRequestConfig{
				Name: k, ConfigOperation: kafka.ConfigOperationDelete,
			})
		}
	}
	return configs
}

// configEntries converts a config map to kafka-go entries, ordered by key so
// requests are deterministic.
func configEntries(config map[string]string) []kafka.ConfigEntry {
	entries := make([]kafka.ConfigEntry, 0, len(config))
	for _, k := range slices.Sorted(maps.Keys(config)) {
		entries = append(entries, kafka.ConfigEntry{ConfigName: k, ConfigValue: config[k]})
	}
	return entries
}

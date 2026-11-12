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
// call error; both are returned here. A failure reported by the broker keeps
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
	return joinResponseErrors(resp.Errors, "create topic", nil)
}

// DeleteTopics deletes the named topics. Failures reported for individual
// topics are joined into the returned error, ordered by topic name. A topic
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
	return joinResponseErrors(resp.Errors, "delete topic", func(err error) error {
		if errors.Is(err, kafka.UnknownTopicOrPartition) {
			return ErrTopicNotFound
		}
		return nil
	})
}

// AlterTopicConfig sets the topic's configuration overrides to config. The
// call replaces the whole set of overrides (the broker's non-incremental
// AlterConfigs): keys left out of config go back to the broker default.
//
// Failures reported per resource are returned too. A topic that does not
// exist yields an error wrapping [ErrTopicNotFound].
func (c *Client) AlterTopicConfig(ctx context.Context, name string, config map[string]string) error {
	configs := make([]kafka.AlterConfigRequestConfig, 0, len(config))
	for _, e := range configEntries(config) {
		configs = append(configs, kafka.AlterConfigRequestConfig{Name: e.ConfigName, Value: e.ConfigValue})
	}
	resp, err := c.kc.AlterConfigs(ctx, &kafka.AlterConfigsRequest{
		Resources: []kafka.AlterConfigRequestResource{{
			ResourceType: kafka.ResourceTypeTopic,
			ResourceName: name,
			Configs:      configs,
		}},
	})
	if err != nil {
		return fmt.Errorf("alter config of topic %q: %w", name, err)
	}
	errs := make(map[string]error, len(resp.Errors))
	for res, err := range resp.Errors {
		if err != nil {
			errs[res.Name] = err
		}
	}
	return joinResponseErrors(errs, "alter config of topic", func(err error) error {
		if errors.Is(err, kafka.UnknownTopicOrPartition) {
			return ErrTopicNotFound
		}
		return nil
	})
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

// joinResponseErrors joins the non-nil per-name errors of a response, ordered
// by name. Each is prefixed with action and the name. When classify returns a
// sentinel for an error, the sentinel is added to the chain next to the
// original error.
func joinResponseErrors(errs map[string]error, action string, classify func(error) error) error {
	var out []error
	for _, name := range slices.Sorted(maps.Keys(errs)) {
		err := errs[name]
		if err == nil {
			continue
		}
		if classify != nil {
			if sentinel := classify(err); sentinel != nil {
				out = append(out, fmt.Errorf("%s %q: %w: %w", action, name, sentinel, err))
				continue
			}
		}
		out = append(out, fmt.Errorf("%s %q: %w", action, name, err))
	}
	return errors.Join(out...)
}

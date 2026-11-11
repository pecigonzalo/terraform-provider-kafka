package provider

import (
	"log"
	"os"
	"testing"

	dockertest "github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	kafka "github.com/segmentio/kafka-go"
)

const (
	existingTopic = "read.me"
)

// Configure mock Kafka cluster and teardown
func TestMain(t *testing.M) {
	// Skip docker setup if not running acceptance
	if os.Getenv("TF_ACC") == "" {
		code := t.Run()
		os.Exit(code)
	}

	pool, err := dockertest.NewPool("")
	if err != nil {
		log.Fatalf("Could not connect to docker: %s", err)
	}

	network, err := pool.CreateNetwork("terraform-provider-kafka")
	if err != nil {
		log.Fatalf("Could not start network: %s", err)
	}

	kafkaContainer, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "docker.io/apache/kafka",
		Tag:        "3.9.1",
		Env: []string{
			"KAFKA_NODE_ID=1",
			"KAFKA_LISTENER_SECURITY_PROTOCOL_MAP=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT,PLAINTEXT_HOST:PLAINTEXT",
			"KAFKA_ADVERTISED_LISTENERS=PLAINTEXT_HOST://localhost:9092,PLAINTEXT://kafka:19092",
			"KAFKA_PROCESS_ROLES=broker,controller",
			"KAFKA_CONTROLLER_QUORUM_VOTERS=1@kafka:29093",
			"KAFKA_LISTENERS=CONTROLLER://:29093,PLAINTEXT_HOST://:9092,PLAINTEXT://:19092",
			"KAFKA_INTER_BROKER_LISTENER_NAME=PLAINTEXT",
			"KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER",
			"CLUSTER_ID=4L6g3nShT-eMCtK--X86sw",
			"KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR=1",
			"KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS=0",
			"KAFKA_TRANSACTION_STATE_LOG_MIN_ISR=1",
			"KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR=1",
			"KAFKA_LOG_DIRS=/tmp/kraft-combined-logs",
		},
		Hostname:  "kafka",
		NetworkID: network.Network.ID,
		PortBindings: map[docker.Port][]docker.PortBinding{
			"9092/tcp": {{HostIP: "localhost", HostPort: "9092/tcp"}},
		},
	})
	if err != nil {
		log.Fatalf("Could not start resource: %s", err)
	}

	waitForKafka := func() error {
		conn, err := kafka.Dial("tcp", "localhost:9092")
		if err != nil {
			pool.Client.Logs(docker.LogsOptions{
				Container:   kafkaContainer.Container.ID,
				RawTerminal: true,
			})
			return err
		}
		defer conn.Close()

		_, err = conn.ApiVersions()
		if err != nil {
			return err
		}

		// Bootstrap test topic
		topic := kafka.TopicConfig{
			Topic:             existingTopic,
			NumPartitions:     1,
			ReplicationFactor: 1,
		}
		err = conn.CreateTopics(topic)
		if err != nil {
			return err
		}

		return nil
	}

	if err = pool.Retry(waitForKafka); err != nil {
		log.Fatalf("could not connect to kafka: %s", err)
	}

	code := t.Run()

	// You can't defer this because os.Exit doesn't care for defer
	if err := pool.Purge(kafkaContainer); err != nil {
		log.Fatalf("Could not purge resource: %s", err)
	}
	if err := pool.RemoveNetwork(network); err != nil {
		log.Fatalf("Could not purge network: %s", err)
	}

	os.Exit(code)
}

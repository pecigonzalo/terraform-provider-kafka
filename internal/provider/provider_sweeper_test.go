package provider

import (
	"log"
	"os"
	"testing"

	"github.com/ory/dockertest/v3"
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

	zkContainer, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository:   "docker.io/bitnami/zookeeper",
		Tag:          "3.8",
		Env:          []string{"ALLOW_ANONYMOUS_LOGIN=yes"},
		Hostname:     "zookeeper",
		NetworkID:    network.Network.ID,
		ExposedPorts: []string{"2181"},
	})
	if err != nil {
		log.Fatalf("Could not start resource: %s", err)
	}

	kafkaContainer, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "docker.io/bitnami/kafka",
		Tag:        "3.3",
		Env: []string{
			"KAFKA_CFG_ZOOKEEPER_CONNECT=zookeeper:2181",
			"KAFKA_CFG_BROKER_ID=0",
			"ALLOW_PLAINTEXT_LISTENER=yes",
			"KAFKA_CFG_LISTENERS=PLAINTEXT://:9092",
			"KAFKA_CFG_ADVERTISED_LISTENERS=PLAINTEXT://127.0.0.1:9092",
		},
		Hostname:  "kafka-1",
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
	if err := pool.Purge(zkContainer); err != nil {
		log.Fatalf("Could not purge resource: %s", err)
	}
	if err := pool.RemoveNetwork(network); err != nil {
		log.Fatalf("Could not purge network: %s", err)
	}

	os.Exit(code)
}

package provider

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"testing"

	dockertest "github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	kafka "github.com/segmentio/kafka-go"
)

const (
	existingTopic = "read.me"

	// orphanExpirySeconds makes Docker kill the test container even if the test
	// process dies before it can clean up.
	orphanExpirySeconds = 600
)

// TestMain starts an isolated Kafka cluster for acceptance tests and tears it
// down afterwards. Every run uses its own network, container name and host
// port, so concurrent runs (for example from parallel sessions or CI jobs on a
// shared Docker host) do not collide, and a leftover from a crashed run never
// blocks the next one.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	// Skip docker setup if not running acceptance
	if os.Getenv("TF_ACC") == "" {
		return m.Run()
	}

	pool, err := dockertest.NewPool("")
	if err != nil {
		log.Printf("Could not connect to docker: %s", err)
		return 1
	}

	suffix, err := randomSuffix()
	if err != nil {
		log.Printf("Could not generate run id: %s", err)
		return 1
	}

	port, err := freePort()
	if err != nil {
		log.Printf("Could not find a free port: %s", err)
		return 1
	}

	network, err := pool.CreateNetwork("terraform-provider-kafka-" + suffix)
	if err != nil {
		log.Printf("Could not start network: %s", err)
		return 1
	}
	defer func() {
		if err := pool.RemoveNetwork(network); err != nil {
			log.Printf("Could not purge network: %s", err)
		}
	}()

	hostPort := fmt.Sprint(port)
	kafkaContainer, err := pool.RunWithOptions(&dockertest.RunOptions{
		Name:       "terraform-provider-kafka-" + suffix,
		Repository: "docker.io/apache/kafka",
		Tag:        "3.9.1",
		Env: []string{
			"KAFKA_NODE_ID=1",
			"KAFKA_LISTENER_SECURITY_PROTOCOL_MAP=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT,PLAINTEXT_HOST:PLAINTEXT",
			// The host listener uses the same port inside and outside the
			// container so the advertised address works from the host.
			"KAFKA_ADVERTISED_LISTENERS=PLAINTEXT_HOST://localhost:" + hostPort + ",PLAINTEXT://kafka:19092",
			"KAFKA_PROCESS_ROLES=broker,controller",
			"KAFKA_CONTROLLER_QUORUM_VOTERS=1@kafka:29093",
			"KAFKA_LISTENERS=CONTROLLER://:29093,PLAINTEXT_HOST://:" + hostPort + ",PLAINTEXT://:19092",
			"KAFKA_INTER_BROKER_LISTENER_NAME=PLAINTEXT",
			"KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER",
			"CLUSTER_ID=4L6g3nShT-eMCtK--X86sw",
			"KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR=1",
			"KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS=0",
			"KAFKA_TRANSACTION_STATE_LOG_MIN_ISR=1",
			"KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR=1",
			"KAFKA_LOG_DIRS=/tmp/kraft-combined-logs",
		},
		Hostname:     "kafka",
		NetworkID:    network.Network.ID,
		ExposedPorts: []string{hostPort + "/tcp"},
		PortBindings: map[docker.Port][]docker.PortBinding{
			docker.Port(hostPort + "/tcp"): {{HostIP: "127.0.0.1", HostPort: hostPort}},
		},
	})
	if err != nil {
		log.Printf("Could not start resource: %s", err)
		return 1
	}
	defer func() {
		if err := pool.Purge(kafkaContainer); err != nil {
			log.Printf("Could not purge resource: %s", err)
		}
	}()
	if err := kafkaContainer.Expire(orphanExpirySeconds); err != nil {
		log.Printf("Could not set container expiry: %s", err)
	}

	address := "127.0.0.1:" + hostPort
	waitForKafka := func() error {
		conn, err := kafka.Dial("tcp", address)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()

		if _, err = conn.ApiVersions(); err != nil {
			return err
		}

		// Bootstrap test topic
		return conn.CreateTopics(kafka.TopicConfig{
			Topic:             existingTopic,
			NumPartitions:     1,
			ReplicationFactor: 1,
		})
	}
	if err := pool.Retry(waitForKafka); err != nil {
		log.Printf("could not connect to kafka: %s", err)
		return 1
	}

	// The provider reads its bootstrap server from the environment in tests.
	if err := os.Setenv("KAFKA_BOOTSTRAP_SERVERS", address); err != nil {
		log.Printf("Could not set bootstrap servers: %s", err)
		return 1
	}

	return m.Run()
}

// randomSuffix returns a short random identifier for per-run resource names.
func randomSuffix() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// freePort asks the OS for an unused TCP port on the loopback interface.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

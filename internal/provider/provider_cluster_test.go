package provider

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"testing"

	dockertest "github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	kafka "github.com/segmentio/kafka-go"
)

// clusterBrokerCount returns how many brokers the acceptance tests start, from
// KAFKA_TEST_BROKERS. It defaults to 1.
func clusterBrokerCount() int {
	n, err := strconv.Atoi(os.Getenv("KAFKA_TEST_BROKERS"))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// runMultiBroker starts n Kafka brokers in KRaft mode on the test network, all
// voting in one controller quorum. Each broker is reachable from the host on its
// own port, and the brokers reach each other by container hostname. It exports
// the host addresses as KAFKA_BOOTSTRAP_SERVERS and returns the exit code of m.
func runMultiBroker(m *testing.M, pool *dockertest.Pool, networkID, suffix string, n int) int {
	const (
		internalPort   = 19092
		controllerPort = 29093
	)

	voters := make([]string, n)
	for i := range n {
		voters[i] = fmt.Sprintf("%d@kafka-%d:%d", i, i, controllerPort)
	}

	addresses := make([]string, n)
	containers := make([]*dockertest.Resource, 0, n)
	defer func() {
		for _, c := range containers {
			if err := pool.Purge(c); err != nil {
				log.Printf("Could not purge resource: %s", err)
			}
		}
	}()

	for i := range n {
		port, err := freePort()
		if err != nil {
			log.Printf("Could not find a free port: %s", err)
			return 1
		}
		hostPort := fmt.Sprint(port)
		addresses[i] = "127.0.0.1:" + hostPort

		c, err := pool.RunWithOptions(&dockertest.RunOptions{
			Name:       fmt.Sprintf("terraform-provider-kafka-%s-%d", suffix, i),
			Repository: "docker.io/apache/kafka",
			Tag:        "3.9.1",
			Env: []string{
				fmt.Sprintf("KAFKA_NODE_ID=%d", i),
				fmt.Sprintf("KAFKA_BROKER_RACK=AZ%d", i),
				"KAFKA_PROCESS_ROLES=broker,controller",
				"KAFKA_CONTROLLER_QUORUM_VOTERS=" + strings.Join(voters, ","),
				"KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER",
				fmt.Sprintf("KAFKA_LISTENERS=CONTROLLER://:%d,PLAINTEXT_HOST://:%s,PLAINTEXT://:%d", controllerPort, hostPort, internalPort),
				fmt.Sprintf("KAFKA_ADVERTISED_LISTENERS=PLAINTEXT_HOST://localhost:%s,PLAINTEXT://kafka-%d:%d", hostPort, i, internalPort),
				"KAFKA_LISTENER_SECURITY_PROTOCOL_MAP=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT,PLAINTEXT_HOST:PLAINTEXT",
				"KAFKA_INTER_BROKER_LISTENER_NAME=PLAINTEXT",
				"CLUSTER_ID=4L6g3nShT-eMCtK--X86sw",
				fmt.Sprintf("KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR=%d", n),
				fmt.Sprintf("KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR=%d", n),
				"KAFKA_TRANSACTION_STATE_LOG_MIN_ISR=1",
				"KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS=0",
				"KAFKA_LOG_DIRS=/tmp/kraft-combined-logs",
			},
			Hostname:     fmt.Sprintf("kafka-%d", i),
			NetworkID:    networkID,
			ExposedPorts: []string{hostPort + "/tcp"},
			PortBindings: map[docker.Port][]docker.PortBinding{
				docker.Port(hostPort + "/tcp"): {{HostIP: "127.0.0.1", HostPort: hostPort}},
			},
		})
		if err != nil {
			log.Printf("Could not start broker %d: %s", i, err)
			return 1
		}
		containers = append(containers, c)
		if err := c.Expire(orphanExpirySeconds); err != nil {
			log.Printf("Could not set container expiry: %s", err)
		}
	}

	// Wait for every broker to answer, then create the shared test topic.
	for _, address := range addresses {
		waitForBroker := func() error {
			conn, err := kafka.Dial("tcp", address)
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			_, err = conn.ApiVersions()
			return err
		}
		if err := pool.Retry(waitForBroker); err != nil {
			log.Printf("could not connect to broker %s: %s", address, err)
			return 1
		}
	}
	createTopic := func() error {
		conn, err := kafka.Dial("tcp", addresses[0])
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		return conn.CreateTopics(kafka.TopicConfig{
			Topic:             existingTopic,
			NumPartitions:     1,
			ReplicationFactor: 1,
		})
	}
	if err := pool.Retry(createTopic); err != nil {
		log.Printf("could not create test topic: %s", err)
		return 1
	}

	if err := os.Setenv("KAFKA_BOOTSTRAP_SERVERS", strings.Join(addresses, ",")); err != nil {
		log.Printf("Could not set bootstrap servers: %s", err)
		return 1
	}
	return m.Run()
}

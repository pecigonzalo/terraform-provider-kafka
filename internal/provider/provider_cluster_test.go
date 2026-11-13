package provider

import (
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	dockertest "github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	kafka "github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/require"
)

// clusterBrokers is the number of brokers the acceptance tests run against.
// Three matches a real cluster and lets replication factor and partition
// changes be tested on real replicas.
const clusterBrokers = 3

// brokerRack returns the rack of broker id. The racks are uneven on purpose:
// brokers 0 and 1 share rack AZ0 and broker 2 is alone in AZ1, so replica
// placement has to spread over two racks with a different number of brokers.
func brokerRack(id int) string {
	if id < 2 {
		return "AZ0"
	}
	return "AZ1"
}

// runCluster starts n Kafka brokers in KRaft mode on the test network, all
// voting in one controller quorum. Each broker is reachable from the host on its
// own port, and the brokers reach each other by container hostname. It exports
// the host addresses as KAFKA_BOOTSTRAP_SERVERS and returns the exit code of m.
func runCluster(m *testing.M, pool *dockertest.Pool, networkID, suffix string, n int) int {
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
				"KAFKA_BROKER_RACK=" + brokerRack(i),
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
				// Enable ACL support. Every listener is PLAINTEXT, so all clients and
				// the brokers themselves authenticate as User:ANONYMOUS; making that
				// principal a super user keeps inter-broker and existing unauthenticated
				// traffic working while still letting ACLs be created and described.
				"KAFKA_AUTHORIZER_CLASS_NAME=org.apache.kafka.metadata.authorizer.StandardAuthorizer",
				"KAFKA_SUPER_USERS=User:ANONYMOUS",
				"KAFKA_ALLOW_EVERYONE_IF_NO_ACL_FOUND=true",
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

	// Topic creation goes through the controller, but each broker serves metadata
	// from its own cache, which is updated asynchronously. The provider only
	// bootstraps from the first address, so make sure every broker already knows
	// the topic and its leader before any test reads it.
	for _, address := range addresses {
		if err := pool.Retry(func() error { return topicVisible(address, existingTopic) }); err != nil {
			log.Printf("test topic %q is not visible on broker %s: %s", existingTopic, address, err)
			return 1
		}
	}

	if err := os.Setenv("KAFKA_BOOTSTRAP_SERVERS", strings.Join(addresses, ",")); err != nil {
		log.Printf("Could not set bootstrap servers: %s", err)
		return 1
	}
	return m.Run()
}

// topicVisible returns an error unless the broker at address reports every
// partition of topic with an elected leader.
func topicVisible(address, topic string) error {
	conn, err := kafka.Dial("tcp", address)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	partitions, err := conn.ReadPartitions(topic)
	if err != nil {
		return err
	}
	if len(partitions) == 0 {
		return fmt.Errorf("no partitions reported for topic %q", topic)
	}
	for _, p := range partitions {
		if p.Leader.Host == "" {
			return fmt.Errorf("partition %d of topic %q has no leader", p.ID, topic)
		}
	}
	return nil
}

// waitForTopicConfig fails the test unless every broker in
// KAFKA_BOOTSTRAP_SERVERS reports value for the topic configuration key.
//
// A topic data source has no expected value to settle against, and the client
// may talk to a different broker on each request, so a data source read right
// after the topic was written can reach a broker that has not applied the
// change yet. Tests that read such a topic call this from PreConfig first.
func waitForTopicConfig(t *testing.T, topic, key, value string) {
	t.Helper()

	for address := range strings.SplitSeq(os.Getenv("KAFKA_BOOTSTRAP_SERVERS"), ",") {
		client := &kafka.Client{Addr: kafka.TCP(address), Timeout: 10 * time.Second}
		require.Eventually(t, func() bool {
			resp, err := client.DescribeConfigs(t.Context(), &kafka.DescribeConfigsRequest{
				Resources: []kafka.DescribeConfigRequestResource{{
					ResourceType: kafka.ResourceTypeTopic,
					ResourceName: topic,
					ConfigNames:  []string{key},
				}},
			})
			if err != nil || len(resp.Resources) != 1 || resp.Resources[0].Error != nil {
				return false
			}
			for _, entry := range resp.Resources[0].ConfigEntries {
				if entry.ConfigName == key && entry.ConfigValue == value {
					return true
				}
			}
			return false
		}, 30*time.Second, 200*time.Millisecond,
			"broker %s does not report %s=%s for topic %q", address, key, value, topic)
	}
}

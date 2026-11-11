package provider

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net"
	"os"
	"testing"

	dockertest "github.com/ory/dockertest/v3"
)

const (
	existingTopic = "read.me"

	// orphanExpirySeconds makes Docker kill the test container even if the test
	// process dies before it can clean up.
	orphanExpirySeconds = 600
)

// TestMain starts an isolated three-broker Kafka cluster for acceptance tests
// and tears it down afterwards. Every run uses its own network, container names
// and host ports, so concurrent runs (for example from parallel sessions or CI jobs on a
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

	return runCluster(m, pool, network.Network.ID, suffix, clusterBrokers)
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

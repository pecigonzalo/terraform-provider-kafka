package provider

import (
	"archive/tar"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	dockertest "github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	kafka "github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl/plain"
	"github.com/stretchr/testify/require"
)

// The secured cluster is a second, separate single-node Kafka that the
// acceptance tests use for everything the PLAINTEXT cluster cannot show: TLS,
// SASL (PLAIN, SCRAM-SHA-256 and SCRAM-SHA-512) and a provider principal that
// is not a super user.
//
// Nothing is mounted from the host. The certificate, the broker properties
// and the start script are generated in memory for each run and copied into
// the container through the Docker API between creating and starting it, so
// the setup behaves the same on Linux CI and on a Docker daemon that runs in a
// VM and shares only part of the host file system. No key material is ever
// written to the repository or the host.
const (
	secureNodeID = 1

	// secureSeedTopic exists on the secured cluster before any test runs.
	secureSeedTopic = "secure.read.me"

	secureAdminUser   = "admin"
	secureLimitedUser = "limited"

	// secureContainerDir is where the generated files live inside the
	// container. /tmp is writable by the image's unprivileged user.
	secureContainerDir = "/tmp/secure"

	// secureContainerUID owns the generated files. It is the uid of the
	// unprivileged user in the apache/kafka image.
	secureContainerUID = 1000

	secureReadyTimeout = 3 * time.Minute
)

// secureClusterInfo describes the running secured cluster. It is set from
// TestMain before any test runs and is read-only afterwards. It is deliberately
// a variable of this test package and not process environment, which the
// provider would pick up as configuration.
type secureClusterInfo struct {
	// Address is the host:port of the SASL_SSL listener.
	Address string
	// AdminPassword and LimitedPassword are random per run.
	AdminPassword   string
	LimitedPassword string

	container *dockertest.Resource

	readyOnce sync.Once
	readyErr  error
}

// secureCluster is nil unless the acceptance tests are running.
var secureCluster *secureClusterInfo

// startSecureCluster creates and starts the secured broker and returns without
// waiting for it to accept connections, so it boots while the PLAINTEXT cluster
// does. Tests call [requireSecureCluster] to wait for readiness. The caller
// must call [secureClusterInfo.stop].
func startSecureCluster(pool *dockertest.Pool, suffix string) (*secureClusterInfo, error) {
	const image, tag = "docker.io/apache/kafka", "3.9.1"

	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("find a free port: %w", err)
	}
	hostPort := fmt.Sprint(port)

	info := &secureClusterInfo{Address: "127.0.0.1:" + hostPort}
	if info.AdminPassword, err = randomSecret(); err != nil {
		return nil, err
	}
	if info.LimitedPassword, err = randomSecret(); err != nil {
		return nil, err
	}

	archive, err := secureClusterFiles(hostPort, info.AdminPassword, info.LimitedPassword)
	if err != nil {
		return nil, fmt.Errorf("generate broker files: %w", err)
	}

	if err := ensureImage(pool, image, tag); err != nil {
		return nil, err
	}

	container, err := pool.Client.CreateContainer(docker.CreateContainerOptions{
		Name: "terraform-provider-kafka-secure-" + suffix,
		Config: &docker.Config{
			Image: image + ":" + tag,
			// Replace the image's entrypoint: formatting the storage with SCRAM
			// credentials needs a command line the stock one does not offer.
			Entrypoint:   []string{"/bin/sh", secureContainerDir + "/start.sh"},
			ExposedPorts: map[docker.Port]struct{}{docker.Port(hostPort + "/tcp"): {}},
		},
		HostConfig: &docker.HostConfig{
			PortBindings: map[docker.Port][]docker.PortBinding{
				docker.Port(hostPort + "/tcp"): {{HostIP: "127.0.0.1", HostPort: hostPort}},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create secured broker container: %w", err)
	}
	info.container = &dockertest.Resource{Container: container}

	if err := pool.Client.UploadToContainer(container.ID, docker.UploadToContainerOptions{
		InputStream: archive,
		Path:        "/tmp",
	}); err != nil {
		info.stop(pool)
		return nil, fmt.Errorf("copy files into secured broker: %w", err)
	}
	if err := pool.Client.StartContainer(container.ID, nil); err != nil {
		info.stop(pool)
		return nil, fmt.Errorf("start secured broker: %w", err)
	}
	// Kill a container that outlives its test run, as the main cluster does.
	time.AfterFunc(orphanExpirySeconds*time.Second, func() { _ = pool.Client.KillContainer(docker.KillContainerOptions{ID: container.ID}) })
	return info, nil
}

// stop removes the container. It is safe to call on a partially started cluster.
func (c *secureClusterInfo) stop(pool *dockertest.Pool) {
	if c == nil || c.container == nil {
		return
	}
	if err := pool.Purge(c.container); err != nil {
		fmt.Printf("Could not purge secured broker: %s\n", err)
	}
}

func ensureImage(pool *dockertest.Pool, repository, tag string) error {
	if _, err := pool.Client.InspectImage(repository + ":" + tag); err == nil {
		return nil
	}
	if err := pool.Client.PullImage(docker.PullImageOptions{Repository: repository, Tag: tag}, docker.AuthConfiguration{}); err != nil {
		return fmt.Errorf("pull %s:%s: %w", repository, tag, err)
	}
	return nil
}

func randomSecret() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// secureClusterFiles returns a tar archive holding the broker's start script,
// properties and a freshly generated self-signed certificate and key. The
// archive extracts to secureContainerDir.
func secureClusterFiles(hostPort, adminPassword, limitedPassword string) (*bytes.Reader, error) {
	certPEM, keyPEM, err := selfSignedCertificate()
	if err != nil {
		return nil, err
	}

	properties := strings.Join([]string{
		"process.roles=broker,controller",
		fmt.Sprintf("node.id=%d", secureNodeID),
		// The controller listener is plaintext but bound to the container's
		// loopback and never published, so only the broker itself can reach it.
		// Its principal is User:ANONYMOUS, which is why that principal is a
		// super user; client listeners always authenticate as a named user.
		fmt.Sprintf("controller.quorum.voters=%d@127.0.0.1:9093", secureNodeID),
		"controller.listener.names=CONTROLLER",
		fmt.Sprintf("listeners=CONTROLLER://127.0.0.1:9093,SASL_SSL://:%s", hostPort),
		fmt.Sprintf("advertised.listeners=SASL_SSL://localhost:%s", hostPort),
		"listener.security.protocol.map=CONTROLLER:PLAINTEXT,SASL_SSL:SASL_SSL",
		"inter.broker.listener.name=SASL_SSL",
		"sasl.mechanism.inter.broker.protocol=PLAIN",
		"sasl.enabled.mechanisms=PLAIN,SCRAM-SHA-256,SCRAM-SHA-512",
		fmt.Sprintf(`listener.name.sasl_ssl.plain.sasl.jaas.config=org.apache.kafka.common.security.plain.PlainLoginModule required username="%[1]s" password="%[2]s" user_%[1]s="%[2]s" user_%[3]s="%[4]s";`,
			secureAdminUser, adminPassword, secureLimitedUser, limitedPassword),
		"listener.name.sasl_ssl.scram-sha-256.sasl.jaas.config=org.apache.kafka.common.security.scram.ScramLoginModule required;",
		"listener.name.sasl_ssl.scram-sha-512.sasl.jaas.config=org.apache.kafka.common.security.scram.ScramLoginModule required;",
		"ssl.keystore.type=PEM",
		"ssl.keystore.location=" + secureContainerDir + "/server.pem",
		"ssl.truststore.type=PEM",
		"ssl.truststore.location=" + secureContainerDir + "/cert.pem",
		// The broker connects to itself over TLS for inter-broker traffic.
		"ssl.endpoint.identification.algorithm=",
		"authorizer.class.name=org.apache.kafka.metadata.authorizer.StandardAuthorizer",
		"allow.everyone.if.no.acl.found=false",
		"super.users=User:" + secureAdminUser + ";User:ANONYMOUS",
		"log.dirs=/tmp/kraft-logs",
		"offsets.topic.replication.factor=1",
		"transaction.state.log.replication.factor=1",
		"transaction.state.log.min.isr=1",
		"group.initial.rebalance.delay.ms=0",
		"",
	}, "\n")

	// SCRAM credentials of a KRaft cluster live in the metadata log, so they are
	// written when the storage is formatted.
	scram := func(user, password string) string {
		return fmt.Sprintf("--add-scram 'SCRAM-SHA-256=[name=%[1]s,password=%[2]s]' --add-scram 'SCRAM-SHA-512=[name=%[1]s,password=%[2]s]'", user, password)
	}
	script := fmt.Sprintf(`#!/bin/sh
set -e
/opt/kafka/bin/kafka-storage.sh format --ignore-formatted \
  --cluster-id 4L6g3nShT-eMCtK--X86sw \
  --config %[1]s/server.properties \
  %[2]s \
  %[3]s
exec /opt/kafka/bin/kafka-server-start.sh %[1]s/server.properties
`, secureContainerDir, scram(secureAdminUser, adminPassword), scram(secureLimitedUser, limitedPassword))

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(name string, mode int64, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: mode, Size: int64(len(data)),
			Uid: secureContainerUID, Gid: secureContainerUID, Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "secure/", Mode: 0o755, Uid: secureContainerUID, Gid: secureContainerUID, Typeflag: tar.TypeDir,
	}); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		name string
		mode int64
		data []byte
	}{
		{"secure/start.sh", 0o700, []byte(script)},
		{"secure/server.properties", 0o600, []byte(properties)},
		{"secure/server.pem", 0o600, append(append([]byte{}, keyPEM...), certPEM...)},
		{"secure/cert.pem", 0o644, certPEM},
	} {
		if err := write(f.name, f.mode, f.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return bytes.NewReader(buf.Bytes()), nil
}

// selfSignedCertificate returns a PEM certificate and PKCS#8 private key valid
// for localhost and 127.0.0.1. The certificate is its own issuer, so a client
// that verifies it against the system roots must reject it.
func selfSignedCertificate() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "terraform-provider-kafka acceptance test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// requireSecureCluster waits for the secured cluster to accept authenticated
// connections, then returns it. The first caller pays the wait; the container
// has been booting since TestMain. It skips when acceptance tests are off.
func requireSecureCluster(t *testing.T) *secureClusterInfo {
	t.Helper()
	if secureCluster == nil {
		t.Skip("secured cluster is only available when TF_ACC is set")
	}
	secureCluster.readyOnce.Do(func() {
		secureCluster.readyErr = secureCluster.waitReady()
	})
	require.NoError(t, secureCluster.readyErr)
	return secureCluster
}

// waitReady polls until the admin user can authenticate over TLS, then creates
// the seed topic and waits for it to be visible. Every wait is bounded.
func (c *secureClusterInfo) waitReady() error {
	deadline := time.Now().Add(secureReadyTimeout)
	retry := func(what string, fn func() error) error {
		var err error
		for time.Now().Before(deadline) {
			if err = fn(); err == nil {
				return nil
			}
			time.Sleep(500 * time.Millisecond)
		}
		return fmt.Errorf("%s: %w", what, err)
	}

	if err := retry("secured broker did not become ready", func() error {
		conn, err := c.adminDialer().Dial("tcp", c.Address)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		_, err = conn.ApiVersions()
		return err
	}); err != nil {
		return err
	}
	if err := retry("could not create the secure seed topic", func() error {
		conn, err := c.adminDialer().Dial("tcp", c.Address)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		err = conn.CreateTopics(kafka.TopicConfig{Topic: secureSeedTopic, NumPartitions: 1, ReplicationFactor: 1})
		if errors.Is(err, kafka.TopicAlreadyExists) {
			return nil
		}
		return err
	}); err != nil {
		return err
	}
	return retry("secure seed topic is not visible", func() error {
		conn, err := c.adminDialer().Dial("tcp", c.Address)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		partitions, err := conn.ReadPartitions(secureSeedTopic)
		if err != nil {
			return err
		}
		if len(partitions) == 0 || partitions[0].Leader.Host == "" {
			return fmt.Errorf("topic %q has no leader yet", secureSeedTopic)
		}
		return nil
	})
}

// adminDialer dials the secured cluster as the super user. It skips
// certificate verification because the certificate is self-signed; the
// verifying path is covered by its own test through the provider.
func (c *secureClusterInfo) adminDialer() *kafka.Dialer {
	return &kafka.Dialer{
		Timeout:       10 * time.Second,
		TLS:           &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}, //nolint:gosec // self-signed test certificate
		SASLMechanism: plain.Mechanism{Username: secureAdminUser, Password: c.AdminPassword},
	}
}

package provider

import (
	"context"
	"crypto/tls"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	kafka "github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl/plain"
	"github.com/stretchr/testify/require"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

// secureProviderBlock renders a provider block that talks to the secured
// cluster the way a user would configure it: TLS and SASL set explicitly in
// the configuration, and the bootstrap address in the block, which takes
// precedence over the KAFKA_BOOTSTRAP_SERVERS the other cluster exports.
func secureProviderBlock(alias, mechanism, user, password string, skipVerify bool) string {
	aliasLine := ""
	if alias != "" {
		aliasLine = fmt.Sprintf("alias = %q", alias)
	}
	return fmt.Sprintf(`
provider "kafka" {
  %s
  bootstrap_servers = [%q]
  tls = {
    enabled     = true
    skip_verify = %t
  }
  sasl = {
    enabled   = true
    mechanism = %q
    username  = %q
    password  = %q
  }
}
`, aliasLine, secureCluster.Address, skipVerify, mechanism, user, password)
}

func secureAdminProvider(mechanism string) string {
	return secureProviderBlock("", mechanism, secureAdminUser, secureCluster.AdminPassword, true)
}

func secureName(t *testing.T, kind string) string {
	t.Helper()
	suffix, err := randomSuffix()
	require.NoError(t, err)
	return fmt.Sprintf("tf-acc-secure-%s-%s", kind, suffix)
}

// secureAdminTransport connects to the secured cluster as the super user.
func secureAdminTransport() *kafka.Transport {
	return &kafka.Transport{
		TLS:         &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}, //nolint:gosec // self-signed test certificate
		SASL:        plain.Mechanism{Username: secureAdminUser, Password: secureCluster.AdminPassword},
		DialTimeout: 10 * time.Second,
	}
}

// secureTopicExists reports whether the topic exists, as seen by the super user.
func secureTopicExists(ctx context.Context, topic string) (bool, error) {
	client := &kafka.Client{Addr: kafka.TCP(secureCluster.Address), Transport: secureAdminTransport(), Timeout: 10 * time.Second}
	resp, err := client.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{topic}})
	if err != nil {
		return false, err
	}
	for _, tp := range resp.Topics {
		if tp.Name == topic {
			return tp.Error == nil, nil
		}
	}
	return false, nil
}

// secureWaitTopic polls until the topic's existence matches want, within 30s.
func secureWaitTopic(topic string, want bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return pollUntil(ctx, 200*time.Millisecond, func(ctx context.Context) (bool, error) {
		got, err := secureTopicExists(ctx, topic)
		return err == nil && got == want, nil
	})
}

func secureCheckTopicExists(topic string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if err := secureWaitTopic(topic, true); err != nil {
			return fmt.Errorf("topic %q is not on the secured cluster: %w", topic, err)
		}
		return nil
	}
}

func secureCheckTopicAbsent(topic string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if err := secureWaitTopic(topic, false); err != nil {
			return fmt.Errorf("topic %q is still on the secured cluster: %w", topic, err)
		}
		return nil
	}
}

// secureAdminClient is a kafkaclient connected to the secured cluster as the
// super user, for inspecting ACLs and quotas.
func secureAdminClient(t *testing.T) *kafkaclient.Client {
	t.Helper()
	client, err := kafkaclient.New(t.Context(), kafkaclient.Config{
		BootstrapServer: secureCluster.Address,
		TLS:             kafkaclient.TLSConfig{Enabled: true, SkipVerify: true},
		SASL:            &kafkaclient.SASLConfig{Mechanism: "plain", Username: secureAdminUser, Password: secureCluster.AdminPassword},
		Timeout:         30 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestAccSecureClusterAuthenticated connects with TLS (verification skipped
// for the self-signed certificate) and each SASL mechanism as the super user,
// reads an existing topic through the data source and creates and destroys a
// topic.
func TestAccSecureClusterAuthenticated(t *testing.T) {
	requireSecureCluster(t)
	for _, mechanism := range []string{"plain", "scram-sha256", "scram-sha512"} {
		t.Run(mechanism, func(t *testing.T) {
			topic := secureName(t, "auth")
			resource.Test(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				CheckDestroy:             secureCheckTopicAbsent(topic),
				Steps: []resource.TestStep{{
					Config: secureAdminProvider(mechanism) + fmt.Sprintf(`
data "kafka_topic" "seed" {
  name = %q
}

resource "kafka_topic" "test" {
  name               = %q
  partitions         = 2
  replication_factor = 1
}
`, secureSeedTopic, topic),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.kafka_topic.seed", "name", secureSeedTopic),
						resource.TestCheckResourceAttr("data.kafka_topic.seed", "partitions", "1"),
						resource.TestCheckResourceAttr("kafka_topic.test", "name", topic),
						resource.TestCheckResourceAttr("kafka_topic.test", "partitions", "2"),
						secureCheckTopicExists(topic),
					),
				}},
			})
		})
	}
}

// With verification on, the self-signed certificate is rejected and the
// diagnostic says why.
func TestAccSecureClusterRejectsUntrustedCertificate(t *testing.T) {
	requireSecureCluster(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: secureProviderBlock("", "plain", secureAdminUser, secureCluster.AdminPassword, false) + fmt.Sprintf(`
data "kafka_topic" "seed" {
  name = %q
}
`, secureSeedTopic),
			ExpectError: regexp.MustCompile(`(?s)failed\s+to\s+verify\s+certificate:\s+x509:\s+certificate\s+signed\s+by\s+unknown\s+authority`),
		}},
	})
}

// recordingT lets a test observe the failure of a resource.Test run, so it can
// assert on the full diagnostic text, including that something is absent from
// it. The ExpectError option can only match, not exclude.
type recordingT struct {
	*testing.T
	mu       sync.Mutex
	messages []string
}

func (r *recordingT) record(args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, fmt.Sprint(args...))
}

func (r *recordingT) Error(args ...any)                 { r.record(args...) }
func (r *recordingT) Errorf(format string, args ...any) { r.record(fmt.Sprintf(format, args...)) }
func (r *recordingT) Fatal(args ...any)                 { r.record(args...); runtime.Goexit() }
func (r *recordingT) Fatalf(format string, args ...any) {
	r.record(fmt.Sprintf(format, args...))
	runtime.Goexit()
}
func (r *recordingT) FailNow() { runtime.Goexit() }
func (r *recordingT) Fail()    {}

func (r *recordingT) output() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.messages, "\n")
}

// runExpectingFailure runs tc and returns everything it reported as failed. It
// fails t if tc passes.
func runExpectingFailure(t *testing.T, tc resource.TestCase) string {
	t.Helper()
	rec := &recordingT{T: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		resource.Test(rec, tc)
	}()
	<-done
	out := rec.output()
	require.NotEmpty(t, out, "the test case was expected to fail")
	return out
}

// A wrong password fails with an authentication diagnostic, and the password
// (wrong or right) appears nowhere in the diagnostic output.
func TestAccSecureClusterWrongPassword(t *testing.T) {
	requireSecureCluster(t)
	for _, mechanism := range []string{"plain", "scram-sha256", "scram-sha512"} {
		t.Run(mechanism, func(t *testing.T) {
			wrong, err := randomSecret()
			require.NoError(t, err)
			out := runExpectingFailure(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{{
					Config: secureProviderBlock("", mechanism, secureAdminUser, wrong, true) + fmt.Sprintf(`
data "kafka_topic" "seed" {
  name = %q
}
`, secureSeedTopic),
				}},
			})

			require.Regexp(t, `(?is)SASL\s+Authentication\s+Failed`, out)
			require.NotContains(t, out, wrong, "the configured password leaked into the diagnostic")
			require.NotContains(t, out, secureCluster.AdminPassword)
			require.NotContains(t, out, secureCluster.LimitedPassword)
		})
	}
}

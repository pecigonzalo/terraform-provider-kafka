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

// secureProviderBlock returns a provider block for the secured cluster, as a
// user writes it. It sets TLS, SASL and the bootstrap address in the block. The
// block value has priority over the KAFKA_BOOTSTRAP_SERVERS variable that the
// other cluster sets.
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

// secureAdminTransport returns a transport for the super user.
func secureAdminTransport() *kafka.Transport {
	return &kafka.Transport{
		TLS:         &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}, //nolint:gosec // self-signed test certificate
		SASL:        plain.Mechanism{Username: secureAdminUser, Password: secureCluster.AdminPassword},
		DialTimeout: 10 * time.Second,
	}
}

// secureTopicExists reports if the super user sees the topic.
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

// secureWaitTopic polls until the topic exists (or not) as want says. The limit
// is 30 seconds.
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

// secureAdminClient returns a client for the super user. Tests use it to read
// ACLs and quotas.
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

// TestAccSecureClusterAuthenticated connects as the super user with TLS and with
// each SASL mechanism. It skips the certificate check because the certificate
// is self-signed. For each mechanism, it reads a topic with the data source,
// and it creates and destroys a topic.
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

// With certificate verification on, the provider rejects the self-signed
// certificate. The diagnostic gives the reason.
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

// recordingT records the failure messages of a resource.Test run. A test can
// then check the full diagnostic text, also to show that a text is not in it.
// The ExpectError option can only match text. It cannot show that text is
// missing.
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

// runExpectingFailure runs tc and returns the failure messages. It fails t if
// tc passes.
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

// A wrong password gives an authentication diagnostic. No password is in the
// diagnostic text: not the wrong one, and not a correct one.
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

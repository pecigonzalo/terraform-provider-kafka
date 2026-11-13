package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	kafka "github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl"
	"github.com/segmentio/kafka-go/sasl/scram"
	"github.com/stretchr/testify/require"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

func secureLimitedProvider(alias string) string {
	return secureProviderBlock(alias, "scram-sha256", secureLimitedUser, secureCluster.LimitedPassword, true)
}

// secureCheckNoACLs fails unless the super user sees no ACL on the topic.
// Brokers learn ACL changes asynchronously, so a single empty answer is not
// proof; the check requires the answer to stay empty for the whole window.
func secureCheckNoACLs(t *testing.T, topic string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		client := secureAdminClient(t)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var found []kafkaclient.ACL
		_ = pollUntil(ctx, 200*time.Millisecond, func(ctx context.Context) (bool, error) {
			acls, err := client.DescribeACLs(ctx, kafkaclient.ACLFilter{
				ResourceType: kafkaclient.ACLResourceTypeTopic, ResourceName: topic,
				PatternType: kafkaclient.ACLPatternTypeLiteral,
			})
			if err == nil && len(acls) > 0 {
				found = acls
				return true, nil
			}
			return false, nil
		})
		if len(found) > 0 {
			return fmt.Errorf("ACLs were created for topic %q although the principal is not authorized: %v", topic, found)
		}
		return nil
	}
}

// secureCheckNoQuota fails unless the super user sees no quota for the user.
func secureCheckNoQuota(t *testing.T, user string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		client := secureAdminClient(t)
		entity := kafkaclient.QuotaEntity{kafkaclient.QuotaEntityUser: kafkaclient.NamedQuotaEntity(user)}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var found *kafkaclient.QuotaEntry
		_ = pollUntil(ctx, 200*time.Millisecond, func(ctx context.Context) (bool, error) {
			entry, err := describeQuota(ctx, client, entity)
			if err == nil && entry != nil {
				found = entry
				return true, nil
			}
			return false, nil
		})
		if found != nil {
			return fmt.Errorf("a quota exists for user %q although the principal is not authorized: %+v", user, found)
		}
		return nil
	}
}

// As a principal with no ACLs, creating an ACL, a quota or a topic fails with
// the diagnostics the resources promise. Each case first checks that nothing
// reached the cluster, then applies the same resource as the super user to show
// that the failure left no state or cluster object behind to collide with.
func TestAccSecureClusterNotSuperUser(t *testing.T) {
	requireSecureCluster(t)

	t.Run("acl", func(t *testing.T) {
		topic := secureName(t, "acl")
		cfg := fmt.Sprintf(`
resource "kafka_acl" "test" {
  resource_type   = "topic"
  resource_name   = %q
  principal       = "User:somebody"
  operation       = "read"
  permission_type = "allow"
}
`, topic)
		resource.Test(t, resource.TestCase{
			PreCheck:                 func() { testAccPreCheck(t) },
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			CheckDestroy:             secureCheckNoACLs(t, topic),
			Steps: []resource.TestStep{
				{
					Config:      secureLimitedProvider("") + cfg,
					ExpectError: regexp.MustCompile(`(?s)not\s+authorized\s+to\s+manage\s+ACLs.*Alter\s+operation\s+on\s+the\s+cluster`),
				},
				{
					PreConfig: func() { require.NoError(t, secureCheckNoACLs(t, topic)(nil)) },
					Config:    secureAdminProvider("plain") + cfg,
					Check:     resource.TestCheckResourceAttrSet("kafka_acl.test", "id"),
				},
			},
		})
	})

	t.Run("quota", func(t *testing.T) {
		user := secureName(t, "quota")
		cfg := fmt.Sprintf(`
resource "kafka_quota" "test" {
  user               = { name = %q }
  producer_byte_rate = 1048576
}
`, user)
		resource.Test(t, resource.TestCase{
			PreCheck:                 func() { testAccPreCheck(t) },
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			CheckDestroy:             secureCheckNoQuota(t, user),
			Steps: []resource.TestStep{
				{
					Config:      secureLimitedProvider("") + cfg,
					ExpectError: regexp.MustCompile(`(?s)not\s+allowed\s+to\s+manage\s+client\s+quotas.*ALTER_CONFIGS`),
				},
				{
					PreConfig: func() { require.NoError(t, secureCheckNoQuota(t, user)(nil)) },
					Config:    secureAdminProvider("plain") + cfg,
					Check:     resource.TestCheckResourceAttr("kafka_quota.test", "user.name", user),
				},
			},
		})
	})

	t.Run("topic", func(t *testing.T) {
		topic := secureName(t, "topic")
		cfg := fmt.Sprintf(`
resource "kafka_topic" "test" {
  name               = %q
  partitions         = 1
  replication_factor = 1
}
`, topic)
		resource.Test(t, resource.TestCase{
			PreCheck:                 func() { testAccPreCheck(t) },
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			CheckDestroy:             secureCheckTopicAbsent(topic),
			Steps: []resource.TestStep{
				{
					Config:      secureLimitedProvider("") + cfg,
					ExpectError: regexp.MustCompile(`(?s)Unable\s+to\s+create\s+topic.*Topic\s+Authorization\s+Failed`),
				},
				{
					PreConfig: func() { require.NoError(t, secureCheckTopicAbsent(topic)(nil)) },
					Config:    secureAdminProvider("plain") + cfg,
					Check:     secureCheckTopicExists(topic),
				},
			},
		})
	})
}

// Once the super user grants the ACLs, created in the same configuration with
// the kafka_acl resource, the limited user can manage a topic.
func TestAccSecureClusterGrantedACLs(t *testing.T) {
	requireSecureCluster(t)
	topic := secureName(t, "granted")
	principal := "User:" + secureLimitedUser

	acl := func(name, operation string) string {
		return fmt.Sprintf(`
resource "kafka_acl" %[1]q {
  provider        = kafka.admin
  resource_type   = "topic"
  resource_name   = %[2]q
  principal       = %[3]q
  operation       = %[4]q
  permission_type = "allow"
}
`, name, topic, principal, operation)
	}
	grants := acl("create", "create") + acl("describe", "describe") +
		acl("describe_configs", "describe_configs") + acl("delete", "delete") + acl("alter_configs", "alter_configs")

	providers := secureProviderBlock("admin", "plain", secureAdminUser, secureCluster.AdminPassword, true) +
		secureLimitedProvider("limited")
	topicCfg := fmt.Sprintf(`
resource "kafka_topic" "test" {
  provider           = kafka.limited
  name               = %q
  partitions         = 1
  replication_factor = 1
  depends_on = [
    kafka_acl.create, kafka_acl.describe, kafka_acl.describe_configs,
    kafka_acl.delete, kafka_acl.alter_configs,
  ]
}
`, topic)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             resource.ComposeAggregateTestCheckFunc(secureCheckTopicAbsent(topic), secureCheckNoACLs(t, topic)),
		Steps: []resource.TestStep{
			// The grants alone first, so the wait for the brokers to enforce them
			// happens between steps, bounded, and not as a fixed delay.
			{
				Config: providers + grants,
				Check:  resource.TestCheckResourceAttr("kafka_acl.create", "principal", principal),
			},
			{
				PreConfig: func() { secureWaitDescribeAllowed(t, topic) },
				Config:    providers + grants + topicCfg,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("kafka_topic.test", "name", topic),
					secureCheckTopicExists(topic),
				),
			},
		},
	})
}

// secureWaitDescribeAllowed waits until the broker lets the limited user
// describe the topic, which is how the grant shows up on the broker serving the
// request. A topic that does not exist yet answers "unknown topic" once the
// user is allowed to ask, and "authorization failed" before.
func secureWaitDescribeAllowed(t *testing.T, topic string) {
	t.Helper()
	transport := secureAdminTransport()
	transport.SASL = scram256(t)
	client := &kafka.Client{Addr: kafka.TCP(secureCluster.Address), Transport: transport, Timeout: 10 * time.Second}
	require.Eventually(t, func() bool {
		resp, err := client.Metadata(t.Context(), &kafka.MetadataRequest{Topics: []string{topic}})
		if err != nil || len(resp.Topics) != 1 {
			return false
		}
		return !errors.Is(resp.Topics[0].Error, kafka.TopicAuthorizationFailed)
	}, 30*time.Second, 200*time.Millisecond, "the grant for %q did not reach the broker", topic)
}

func scram256(t *testing.T) sasl.Mechanism {
	t.Helper()
	m, err := scram.Mechanism(scram.SHA256, secureLimitedUser, secureCluster.LimitedPassword)
	require.NoError(t, err)
	return m
}

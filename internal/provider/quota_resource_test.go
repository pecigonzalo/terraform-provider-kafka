package provider

import (
	"context"
	"fmt"
	"maps"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/protocol/describeclientquotas"
	"github.com/stretchr/testify/require"

	"github.com/pecigonzalo/terraform-provider-kafka/internal/kafkaclient"
)

// testAccQuotaName returns an entity name unique to the test, so quotas left
// behind by one test never collide with another.
func testAccQuotaName(t *testing.T, kind string) string {
	suffix, err := randomSuffix()
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("tf-acc-%s-%s", kind, suffix)
}

func TestAccQuotaResource(t *testing.T) {
	user := testAccQuotaName(t, "user")
	id := "user=" + user + ",client-id=app"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckQuotaGone(id),
		Steps: []resource.TestStep{
			{
				Config: testAccQuotaConfig(user, `
  producer_byte_rate = 1048576
  consumer_byte_rate = 2097152
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("kafka_quota.test", "id", id),
					resource.TestCheckResourceAttr("kafka_quota.test", "user.name", user),
					resource.TestCheckResourceAttr("kafka_quota.test", "client_id.name", "app"),
					resource.TestCheckResourceAttr("kafka_quota.test", "producer_byte_rate", "1048576"),
					resource.TestCheckResourceAttr("kafka_quota.test", "consumer_byte_rate", "2097152"),
					resource.TestCheckNoResourceAttr("kafka_quota.test", "request_percentage"),
				),
			},
			// Update a limit in place and add another.
			{
				Config: testAccQuotaConfig(user, `
  producer_byte_rate = 4194304
  consumer_byte_rate = 2097152
  request_percentage = 25
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("kafka_quota.test", "producer_byte_rate", "4194304"),
					resource.TestCheckResourceAttr("kafka_quota.test", "request_percentage", "25"),
				),
			},
			// Removing a limit from the configuration removes it on the broker.
			{
				Config: testAccQuotaConfig(user, `
  producer_byte_rate = 4194304
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("kafka_quota.test", "producer_byte_rate", "4194304"),
					resource.TestCheckNoResourceAttr("kafka_quota.test", "consumer_byte_rate"),
					resource.TestCheckNoResourceAttr("kafka_quota.test", "request_percentage"),
				),
			},
			// A follow-up plan shows nothing: the removed limits are really gone.
			{
				Config:   testAccQuotaConfig(user, "  producer_byte_rate = 4194304\n"),
				PlanOnly: true,
			},
			{
				PreConfig: func() {
					waitForQuotaValues(t, id, kafkaclient.QuotaValues{kafkaclient.QuotaProducerByteRate: 4194304})
				},
				ResourceName:      "kafka_quota.test",
				ImportState:       true,
				ImportStateId:     id,
				ImportStateVerify: true,
			},
		},
	})
}

// pinnedTransport returns a transport that dials address for every
// connection, whatever broker the cluster metadata names. A request through it
// reaches that broker only.
func pinnedTransport(address string) *kafka.Transport {
	return &kafka.Transport{
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
}

// describeQuotaOnBroker asks one named broker for the quota on exactly entity.
// It returns nil if that broker reports no quota.
//
// The provider client sends a DescribeClientQuotas request to the controller
// that its cached metadata names. A KRaft cluster answers every metadata
// request with a random live broker as the controller, so the request goes to
// a different broker after each metadata refresh. A client that points at one
// bootstrap address does not ask that broker. This function dials the wanted
// broker for every connection, so the answer comes from that broker only.
func describeQuotaOnBroker(ctx context.Context, address string, entity kafkaclient.QuotaEntity) (*kafkaclient.QuotaEntry, error) {
	transport := pinnedTransport(address)
	defer transport.CloseIdleConnections()

	req := &describeclientquotas.Request{Strict: true}
	for entityType, name := range entity {
		component := describeclientquotas.Component{EntityType: string(entityType), Match: name.Name}
		if name.Default {
			component.MatchType, component.Match = 1, ""
		}
		req.Components = append(req.Components, component)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	message, err := transport.RoundTrip(ctx, kafka.TCP(address), req)
	if err != nil {
		return nil, err
	}
	resp, ok := message.(*describeclientquotas.Response)
	if !ok {
		return nil, fmt.Errorf("unexpected response type %T", message)
	}
	if resp.ErrorCode != 0 {
		return nil, fmt.Errorf("broker %s answered with error code %d: %s", address, resp.ErrorCode, resp.ErrorMessage)
	}
	for _, e := range resp.Entries {
		entry := kafkaclient.QuotaEntry{Entity: kafkaclient.QuotaEntity{}, Values: kafkaclient.QuotaValues{}}
		for _, part := range e.Entities {
			entry.Entity[kafkaclient.QuotaEntityType(part.EntityType)] = kafkaclient.QuotaEntityName{Name: part.EntityName, Default: part.EntityName == ""}
		}
		for _, v := range e.Values {
			entry.Values[kafkaclient.QuotaKey(v.Key)] = v.Value
		}
		if sameQuotaEntity(entry.Entity, entity) {
			return &entry, nil
		}
	}
	return nil, nil
}

// waitForQuotaValues fails the test unless every broker reports exactly the
// wanted limits for the quota with the given id. An empty want means that
// every broker must report no quota.
//
// The provider client reaches one broker at a time and can switch to another.
// A broker that has not applied a recent change yet answers with the old
// state. Tests call this from PreConfig, after a change made outside
// Terraform, so that the next step reads the same state from every broker. An
// import has no expected state to wait for, so it needs this.
func waitForQuotaValues(t *testing.T, id string, want kafkaclient.QuotaValues) {
	t.Helper()

	entity, err := decodeQuotaID(id)
	require.NoError(t, err)
	require.NoError(t, waitForQuotaOnEveryBroker(t.Context(), entity, want))
}

// waitForQuotaOnEveryBroker polls each broker of the cluster, one at a time,
// until it reports exactly the wanted limits for the entity. An empty want
// means no quota. It returns an error if one broker still reports something
// else after 30 seconds.
func waitForQuotaOnEveryBroker(ctx context.Context, entity kafkaclient.QuotaEntity, want kafkaclient.QuotaValues) error {
	addresses, err := testAccBrokerAddresses(ctx)
	if err != nil {
		return err
	}
	for _, address := range addresses {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var entry *kafkaclient.QuotaEntry
		err := pollUntil(ctx, 100*time.Millisecond, func(ctx context.Context) (bool, error) {
			var err error
			entry, err = describeQuotaOnBroker(ctx, address, entity)
			if err != nil {
				return false, err
			}
			if entry == nil {
				return len(want) == 0, nil
			}
			return maps.Equal(entry.Values, want), nil
		})
		cancel()
		if err != nil {
			return fmt.Errorf("broker %s does not report the limits %v for quota %v (last answer %v): %w", address, want, entity, entry, err)
		}
	}
	return nil
}

// testAccBrokerAddresses returns the address of every broker in the cluster,
// as the cluster advertises them.
func testAccBrokerAddresses(ctx context.Context) ([]string, error) {
	bootstrap, _, _ := strings.Cut(os.Getenv("KAFKA_BOOTSTRAP_SERVERS"), ",")
	client, err := kafkaclient.New(ctx, kafkaclient.Config{BootstrapServer: bootstrap, Timeout: 10 * time.Second})
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()
	metadata, err := client.Metadata(ctx)
	if err != nil {
		return nil, err
	}
	addresses := make([]string, 0, len(metadata.Brokers))
	for _, broker := range metadata.Brokers {
		addresses = append(addresses, net.JoinHostPort(broker.Host, strconv.Itoa(broker.Port)))
	}
	return addresses, nil
}

// testAccQuotaClient returns a client for the first broker in
// KAFKA_BOOTSTRAP_SERVERS, for changes made outside Terraform.
func testAccQuotaClient(t *testing.T) *kafkaclient.Client {
	t.Helper()
	address, _, _ := strings.Cut(os.Getenv("KAFKA_BOOTSTRAP_SERVERS"), ",")
	client, err := kafkaclient.New(t.Context(), kafkaclient.Config{BootstrapServer: address, Timeout: 10 * time.Second})
	require.NoError(t, err)
	return client
}

// TestAccQuotaResourceDeletedOutsideTerraform checks that a quota removed
// outside Terraform is noticed and created again.
func TestAccQuotaResourceDeletedOutsideTerraform(t *testing.T) {
	user := testAccQuotaName(t, "gone")
	id := "user=" + user + ",client-id=app"
	config := testAccQuotaConfig(user, "  producer_byte_rate = 1048576\n")

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckQuotaGone(id),
		Steps: []resource.TestStep{
			{Config: config},
			// Remove the quota behind Terraform's back: the refresh must drop it
			// from state and the plan must create it again.
			{
				PreConfig: func() {
					entity, err := decodeQuotaID(id)
					require.NoError(t, err)
					require.NoError(t, testAccQuotaClient(t).AlterQuotas(t.Context(), entity, nil,
						[]string{string(kafkaclient.QuotaProducerByteRate)}))
					require.NoError(t, testAccQuotaAbsent(id))
				},
				Config: config,
				Check:  resource.TestCheckResourceAttr("kafka_quota.test", "producer_byte_rate", "1048576"),
			},
		},
	})
}

// TestAccQuotaResourceLimitChangedOutsideTerraform checks that a limit changed
// outside Terraform is reported as a difference and set back by apply.
func TestAccQuotaResourceLimitChangedOutsideTerraform(t *testing.T) {
	user := testAccQuotaName(t, "drift")
	id := "user=" + user + ",client-id=app"
	config := testAccQuotaConfig(user, "  producer_byte_rate = 1048576\n")

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckQuotaGone(id),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  resource.TestCheckResourceAttr("kafka_quota.test", "producer_byte_rate", "1048576"),
			},
			{
				PreConfig: func() {
					entity, err := decodeQuotaID(id)
					require.NoError(t, err)
					require.NoError(t, testAccQuotaClient(t).AlterQuotas(t.Context(), entity,
						kafkaclient.QuotaValues{kafkaclient.QuotaProducerByteRate: 2097152}, nil))
					waitForQuotaValues(t, id, kafkaclient.QuotaValues{kafkaclient.QuotaProducerByteRate: 2097152})
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			// Apply sets the limit back to the configured value.
			{
				Config: config,
				Check:  resource.TestCheckResourceAttr("kafka_quota.test", "producer_byte_rate", "1048576"),
			},
			{
				PreConfig: func() {
					waitForQuotaValues(t, id, kafkaclient.QuotaValues{kafkaclient.QuotaProducerByteRate: 1048576})
				},
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

func TestAccQuotaResourceDefaultEntities(t *testing.T) {
	client := testAccQuotaName(t, "client")
	id := "client-id=" + client

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckQuotaGone(id),
		Steps: []resource.TestStep{
			{
				Config: providerConfig + fmt.Sprintf(`
resource "kafka_quota" "test" {
  user      = { default = true }
  client_id = { name = %q }
  producer_byte_rate = 10
}
`, client),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("kafka_quota.test", "id", "user=<default>,client-id="+client),
					resource.TestCheckResourceAttr("kafka_quota.test", "user.default", "true"),
					resource.TestCheckNoResourceAttr("kafka_quota.test", "user.name"),
					resource.TestCheckResourceAttr("kafka_quota.test", "producer_byte_rate", "10"),
				),
			},
			{
				PreConfig: func() {
					waitForQuotaValues(t, "user=<default>,client-id="+client, kafkaclient.QuotaValues{kafkaclient.QuotaProducerByteRate: 10})
				},
				ResourceName:      "kafka_quota.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccQuotaResourceIP(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: providerConfig + `
resource "kafka_quota" "test" {
  ip = { name = "203.0.113.7" }
  connection_creation_rate = 5
}
`,
				Check: resource.TestCheckResourceAttr("kafka_quota.test", "id", "ip=203.0.113.7"),
			},
			{
				PreConfig: func() {
					waitForQuotaValues(t, "ip=203.0.113.7", kafkaclient.QuotaValues{kafkaclient.QuotaConnectionCreationRate: 5})
				},
				ResourceName:      "kafka_quota.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

// Changing the entity replaces the quota, and the old entity is cleaned up.
func TestAccQuotaResourceReplacesOnEntityChange(t *testing.T) {
	first := testAccQuotaName(t, "first")
	second := testAccQuotaName(t, "second")
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckQuotaGone("user=" + second + ",client-id=app"),
		Steps: []resource.TestStep{
			{Config: testAccQuotaConfig(first, "  producer_byte_rate = 1000\n")},
			{
				Config: testAccQuotaConfig(second, "  producer_byte_rate = 1000\n"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("kafka_quota.test", "id", "user="+second+",client-id=app"),
					testAccCheckQuotaGone("user="+first+",client-id=app"),
				),
			},
		},
	})
}

// TestAccQuotaResourceRejectsExisting checks that create refuses an entity that
// already has quotas. The check is best effort: it asks one broker, which can
// be behind. The test makes it reliable: it waits until every broker reports
// the quota before it applies.
func TestAccQuotaResourceRejectsExisting(t *testing.T) {
	user := testAccQuotaName(t, "existing")
	id := "user=" + user
	entity, err := decodeQuotaID(id)
	require.NoError(t, err)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					client := testAccQuotaClient(t)
					limits := kafkaclient.QuotaValues{kafkaclient.QuotaProducerByteRate: 1000}
					require.NoError(t, client.AlterQuotas(t.Context(), entity, limits, nil))
					t.Cleanup(func() {
						_ = client.AlterQuotas(context.Background(), entity, nil, []string{string(kafkaclient.QuotaProducerByteRate)})
					})
					waitForQuotaValues(t, id, limits)
				},
				Config: providerConfig + fmt.Sprintf(`
resource "kafka_quota" "test" {
  user = { name = %q }
  consumer_byte_rate = 1000
}
`, user),
				ExpectError: regexp.MustCompile(`Quota already exists`),
			},
		},
	})
}

func TestAccQuotaResourceInvalidConfig(t *testing.T) {
	tests := []struct {
		name, body, wantErr string
	}{
		{"no_limit", `user = { name = "a" }`, `(?s)At least one of these attributes`},
		{"no_entity", `producer_byte_rate = 1`, `(?s)At least one of these attributes`},
		{"connection_rate_without_ip", "user = { name = \"a\" }\n  connection_creation_rate = 1", `(?s)(must be specified|Missing)`},
		{"producer_rate_with_ip", "ip = { name = \"1.1.1.1\" }\n  producer_byte_rate = 1", `(?s)cannot be specified when`},
		{"negative_limit", "user = { name = \"a\" }\n  producer_byte_rate = -1", `(?s)must be at least 0`},
		{"ip_with_user", "user = { name = \"a\" }\n  ip = { name = \"1.1.1.1\" }\n  producer_byte_rate = 1", `(?s)cannot be specified when`},
		{"name_and_default", "user = { name = \"a\", default = true }\n  producer_byte_rate = 1", `(?s)(only one|exactly one|cannot be specified when)`},
		{"neither_name_nor_default", "user = {}\n  producer_byte_rate = 1", `(?s)No attribute specified`},
		{"default_false", "user = { default = false }\n  producer_byte_rate = 1", `(?s)(must be|value)`},
		{"empty_name", "user = { name = \"\" }\n  producer_byte_rate = 1", `(?s)at least 1`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				PreCheck:                 func() { testAccPreCheck(t) },
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{{
					Config:      providerConfig + "resource \"kafka_quota\" \"test\" {\n  " + tc.body + "\n}\n",
					PlanOnly:    true,
					ExpectError: regexp.MustCompile(tc.wantErr),
				}},
			})
		})
	}
}

func TestAccQuotaResourceImportMalformedID(t *testing.T) {
	user := testAccQuotaName(t, "import")
	id := "user=" + user + ",client-id=app"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckQuotaGone(id),
		Steps: []resource.TestStep{
			{Config: testAccQuotaConfig(user, "  producer_byte_rate = 1000\n")},
			{
				ResourceName:  "kafka_quota.test",
				ImportState:   true,
				ImportStateId: "alice",
				ExpectError:   regexp.MustCompile(`invalid quota id "alice"`),
			},
			{
				ResourceName:  "kafka_quota.test",
				ImportState:   true,
				ImportStateId: "user=does-not-exist-anywhere",
				ExpectError:   regexp.MustCompile(`(?s)Cannot import non-existent remote object`),
			},
		},
	})
}

func testAccQuotaConfig(user, limits string) string {
	return providerConfig + fmt.Sprintf(`
resource "kafka_quota" "test" {
  user      = { name = %q }
  client_id = { name = "app" }
%s
}
`, user, limits)
}

// testAccCheckQuotaGone returns a CheckDestroy that fails if the quota with the
// given id still exists on the broker.
func testAccCheckQuotaGone(id string) resource.TestCheckFunc {
	return func(_ *terraform.State) error { return testAccQuotaAbsent(id) }
}

// testAccQuotaAbsent fails unless the quota with the given id is gone from
// every broker. Brokers learn about removals at different times, so it asks
// each broker for up to 30 seconds.
func testAccQuotaAbsent(id string) error {
	entity, err := decodeQuotaID(id)
	if err != nil {
		return err
	}
	return waitForQuotaOnEveryBroker(context.Background(), entity, nil)
}

package provider

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

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
				ResourceName:      "kafka_quota.test",
				ImportState:       true,
				ImportStateId:     id,
				ImportStateVerify: true,
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

func TestAccQuotaResourceRejectsExisting(t *testing.T) {
	user := testAccQuotaName(t, "existing")
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: providerConfig + fmt.Sprintf(`
resource "kafka_quota" "first" {
  user = { name = %[1]q }
  producer_byte_rate = 1000
}
resource "kafka_quota" "second" {
  user = { name = %[1]q }
  consumer_byte_rate = 1000
  depends_on = [kafka_quota.first]
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
// every broker. Brokers learn about removals asynchronously, so it waits for a
// short while before reporting a quota that is still there.
func testAccQuotaAbsent(id string) error {
	entity, err := decodeQuotaID(id)
	if err != nil {
		return err
	}
	ctx := context.Background()
	for address := range strings.SplitSeq(os.Getenv("KAFKA_BOOTSTRAP_SERVERS"), ",") {
		client, err := kafkaclient.New(ctx, kafkaclient.Config{BootstrapServer: address, Timeout: 10 * time.Second})
		if err != nil {
			return err
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			entry, err := describeQuota(ctx, client, entity)
			if err != nil {
				return err
			}
			if entry == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("quota %q still exists on broker %s: %v", id, address, entry.Values)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	return nil
}

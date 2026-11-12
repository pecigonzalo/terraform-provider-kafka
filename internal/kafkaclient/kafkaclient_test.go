package kafkaclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/protocol/metadata"
	"github.com/segmentio/kafka-go/sasl/aws_msk_iam_v2"
	"github.com/segmentio/kafka-go/sasl/plain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testUser     = "alice"
	testPassword = "s3cr3t-p4ssw0rd-do-not-leak"
)

func fakeAWSLoader(ctx context.Context) (aws.Config, error) {
	return aws.Config{Region: "eu-west-1"}, nil
}

func TestParseSASLMechanism(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    SASLMechanism
		wantErr bool
	}{
		{name: "plain", input: "plain", want: SASLMechanismPlain},
		{name: "scram_sha256", input: "scram-sha256", want: SASLMechanismScramSHA256},
		{name: "scram_sha_256", input: "scram-sha-256", want: SASLMechanismScramSHA256},
		{name: "scram_sha512", input: "scram-sha512", want: SASLMechanismScramSHA512},
		{name: "scram_sha_512", input: "scram-sha-512", want: SASLMechanismScramSHA512},
		{name: "aws_msk_iam", input: "aws-msk-iam", want: SASLMechanismAWSMSKIAM},
		{name: "case_and_space_insensitive", input: "  SCRAM-SHA-256 ", want: SASLMechanismScramSHA256},
		{name: "unknown", input: "gssapi", wantErr: true},
		{name: "empty", input: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSASLMechanism(tc.input)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestNewTransport_SASL(t *testing.T) {
	tests := []struct {
		name     string
		sasl     *SASLConfig
		wantName string // expected sasl.Mechanism.Name(); empty means SASL disabled
		wantErr  bool
	}{
		{name: "disabled", sasl: nil},
		{name: "plain", sasl: &SASLConfig{Mechanism: "plain", Username: testUser, Password: testPassword}, wantName: "PLAIN"},
		{name: "scram_sha256", sasl: &SASLConfig{Mechanism: "scram-sha256", Username: testUser, Password: testPassword}, wantName: "SCRAM-SHA-256"},
		{name: "scram_sha_256", sasl: &SASLConfig{Mechanism: "scram-sha-256", Username: testUser, Password: testPassword}, wantName: "SCRAM-SHA-256"},
		{name: "scram_sha512", sasl: &SASLConfig{Mechanism: "scram-sha512", Username: testUser, Password: testPassword}, wantName: "SCRAM-SHA-512"},
		{name: "scram_sha_512", sasl: &SASLConfig{Mechanism: "scram-sha-512", Username: testUser, Password: testPassword}, wantName: "SCRAM-SHA-512"},
		{name: "aws_msk_iam", sasl: &SASLConfig{Mechanism: "aws-msk-iam"}, wantName: "AWS_MSK_IAM"},
		{name: "empty_mechanism_defaults_to_aws_msk_iam", sasl: &SASLConfig{}, wantName: "AWS_MSK_IAM"},
		{name: "unknown_mechanism", sasl: &SASLConfig{Mechanism: "gssapi", Username: testUser, Password: testPassword}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{BootstrapServer: "localhost:9092", SASL: tc.sasl}

			transport, err := newTransport(t.Context(), cfg, fakeAWSLoader)
			if tc.wantErr {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), testPassword)
				assert.NotContains(t, err.Error(), testUser)
				return
			}
			require.NoError(t, err)
			if tc.wantName == "" {
				assert.Nil(t, transport.SASL)
				return
			}
			require.NotNil(t, transport.SASL)
			assert.Equal(t, tc.wantName, transport.SASL.Name())
		})
	}
}

func TestNewTransport_SASLMechanismTypes(t *testing.T) {
	cfg := Config{BootstrapServer: "b:9092", SASL: &SASLConfig{Mechanism: "plain", Username: testUser, Password: testPassword}}
	transport, err := newTransport(t.Context(), cfg, fakeAWSLoader)
	require.NoError(t, err)
	assert.Equal(t, plain.Mechanism{Username: testUser, Password: testPassword}, transport.SASL)

	cfg.SASL = &SASLConfig{Mechanism: "aws-msk-iam"}
	transport, err = newTransport(t.Context(), cfg, fakeAWSLoader)
	require.NoError(t, err)
	iam, ok := transport.SASL.(*aws_msk_iam_v2.Mechanism)
	require.True(t, ok, "got %T", transport.SASL)
	assert.Equal(t, "eu-west-1", iam.Region)
	assert.NotNil(t, iam.Signer)
}

func TestNewTransport_AWSLoaderError(t *testing.T) {
	sentinel := errors.New("no credentials")
	loader := func(context.Context) (aws.Config, error) { return aws.Config{}, sentinel }

	_, err := newTransport(t.Context(), Config{
		BootstrapServer: "b:9092",
		SASL:            &SASLConfig{Mechanism: "aws-msk-iam"},
	}, loader)
	require.ErrorIs(t, err, sentinel)
}

func TestNewTransport_TLS(t *testing.T) {
	tests := []struct {
		name       string
		tls        TLSConfig
		wantTLS    bool
		wantSkip   bool
		wantServer string
	}{
		{name: "disabled", tls: TLSConfig{}},
		{name: "disabled_ignores_skip_verify", tls: TLSConfig{SkipVerify: true}},
		{name: "enabled", tls: TLSConfig{Enabled: true}, wantTLS: true},
		{name: "enabled_skip_verify", tls: TLSConfig{Enabled: true, SkipVerify: true}, wantTLS: true, wantSkip: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transport, err := newTransport(t.Context(), Config{BootstrapServer: "b:9093", TLS: tc.tls}, fakeAWSLoader)
			require.NoError(t, err)
			if !tc.wantTLS {
				assert.Nil(t, transport.TLS)
				return
			}
			require.NotNil(t, transport.TLS)
			assert.Equal(t, tc.wantSkip, transport.TLS.InsecureSkipVerify)
			assert.GreaterOrEqual(t, transport.TLS.MinVersion, uint16(0x0303), "TLS 1.2 minimum")
		})
	}
}

func TestNewTransport_Timeout(t *testing.T) {
	transport, err := newTransport(t.Context(), Config{BootstrapServer: "b:9092", Timeout: 7 * time.Second}, fakeAWSLoader)
	require.NoError(t, err)
	assert.Equal(t, 7*time.Second, transport.DialTimeout)
	assert.Equal(t, clientID, transport.ClientID)
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{name: "valid", cfg: Config{BootstrapServer: "b:9092"}},
		{name: "missing_broker", cfg: Config{}, wantErr: true},
		{name: "blank_broker", cfg: Config{BootstrapServer: "  "}, wantErr: true},
		{name: "negative_timeout", cfg: Config{BootstrapServer: "b:9092", Timeout: -1}, wantErr: true},
		{name: "bad_mechanism", cfg: Config{BootstrapServer: "b:9092", SASL: &SASLConfig{Mechanism: "nope", Password: testPassword}}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), testPassword)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestSASLConfigFormattingRedactsPassword(t *testing.T) {
	s := SASLConfig{Mechanism: "plain", Username: testUser, Password: testPassword}
	for _, out := range []string{
		fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), s.String(),
		fmt.Sprintf("%v", &s),
	} {
		assert.NotContains(t, out, testPassword)
	}
}

func TestNew(t *testing.T) {
	c, err := New(t.Context(), Config{BootstrapServer: "localhost:9092", Timeout: 3 * time.Second})
	require.NoError(t, err)
	require.NotNil(t, c.kc)
	assert.Equal(t, 3*time.Second, c.kc.Timeout)
	assert.Equal(t, "localhost:9092", c.kc.Addr.String())
	require.NoError(t, c.Close())

	_, err = New(t.Context(), Config{})
	require.Error(t, err)
}

// fakeRoundTripper serves a canned metadata response.
type fakeRoundTripper struct {
	gotTopics []string
}

func (f *fakeRoundTripper) RoundTrip(_ context.Context, _ net.Addr, req kafka.Request) (kafka.Response, error) {
	m := req.(*metadata.Request)
	f.gotTopics = append(f.gotTopics, m.TopicNames...)
	return &metadata.Response{Brokers: []metadata.ResponseBroker{{NodeID: 1, Host: "h", Port: 9092}}}, nil
}

func TestReadOnlyViewReadsThroughSameClient(t *testing.T) {
	rt := &fakeRoundTripper{}
	c, err := newClient(t.Context(), Config{BootstrapServer: "b:9092"}, fakeAWSLoader)
	require.NoError(t, err)
	c.kc.Transport = rt

	ro := c.ReadOnly()
	resp, err := ro.Metadata(t.Context())
	require.NoError(t, err)
	require.Len(t, resp.Brokers, 1)
	assert.Equal(t, 1, resp.Brokers[0].ID)
}

// TestReadOnlyClientHasNoMutatingMethods is the structural guard: the
// read-only view may only expose methods that Reader (the read side) has,
// and Reader must not grow methods that look like writes.
func TestReadOnlyClientHasNoMutatingMethods(t *testing.T) {
	readerMethods := methodNames(reflect.TypeFor[*Reader]())
	for _, name := range methodNames(reflect.TypeFor[*ReadOnlyClient]()) {
		require.Contains(t, readerMethods, name, "ReadOnlyClient.%s is not a Reader method", name)
	}
	for _, name := range readerMethods {
		for _, prefix := range []string{"Create", "Delete", "Alter", "Update", "Set", "Add", "Remove", "Write", "Produce"} {
			assert.Falsef(t, strings.HasPrefix(name, prefix), "Reader.%s looks mutating; put it on Client", name)
		}
	}
	_, hasClose := reflect.TypeFor[*ReadOnlyClient]().MethodByName("Close")
	assert.False(t, hasClose, "ReadOnlyClient must not own the transport lifecycle")
}

func methodNames(t reflect.Type) []string {
	names := make([]string, 0, t.NumMethod())
	for i := range t.NumMethod() {
		names = append(names, t.Method(i).Name)
	}
	return names
}

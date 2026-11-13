// Package kafkaclient is a thin wrapper around a single kafka-go client.
//
// It owns the translation from provider-level connection settings (broker
// address, TLS, SASL, timeout) to a configured [kafka.Transport], and it
// separates read operations from mutating ones at the type level: a [Client]
// can read and write, while the [ReadOnlyClient] returned by
// [Client.ReadOnly] cannot reach any mutating method.
package kafkaclient

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxDialTimeout is the longest time the provider waits to open one
// connection to a broker. A longer provider timeout does not raise it. Without
// this limit, one broker that cannot be reached stops every operation for the
// whole provider timeout.
const MaxDialTimeout = 30 * time.Second

// DialTimeoutFor returns the dial timeout for a provider timeout: the smaller
// of the two values.
func DialTimeoutFor(timeout time.Duration) time.Duration {
	return min(timeout, MaxDialTimeout)
}

// SASLMechanism is a normalised SASL mechanism name.
type SASLMechanism string

// Supported SASL mechanisms.
const (
	SASLMechanismPlain       SASLMechanism = "plain"
	SASLMechanismScramSHA256 SASLMechanism = "scram-sha256"
	SASLMechanismScramSHA512 SASLMechanism = "scram-sha512"
	SASLMechanismAWSMSKIAM   SASLMechanism = "aws-msk-iam"
)

// SASLMechanismNames lists the mechanism names that the provider documents.
var SASLMechanismNames = []string{
	string(SASLMechanismPlain),
	string(SASLMechanismScramSHA256),
	string(SASLMechanismScramSHA512),
	string(SASLMechanismAWSMSKIAM),
}

// ParseSASLMechanism normalises a user supplied mechanism name. It accepts
// both the "scram-sha256" and "scram-sha-256" spellings, ignores case and
// surrounding whitespace, and returns an error for unknown names.
func ParseSASLMechanism(name string) (SASLMechanism, error) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	normalized = strings.ReplaceAll(normalized, "scram-sha-", "scram-sha")

	switch m := SASLMechanism(normalized); m {
	case SASLMechanismPlain, SASLMechanismScramSHA256, SASLMechanismScramSHA512, SASLMechanismAWSMSKIAM:
		return m, nil
	default:
		if normalized == "" {
			return "", fmt.Errorf("SASL mechanism is empty: choose one of %s", strings.Join(SASLMechanismNames, ", "))
		}
		return "", fmt.Errorf("unable to detect SASL mechanism: %q", name)
	}
}

// TLSConfig controls transport encryption.
type TLSConfig struct {
	// Enabled turns TLS on.
	Enabled bool
	// SkipVerify disables server certificate verification. Insecure.
	SkipVerify bool
}

// SASLConfig controls SASL authentication. The zero value of Config.SASL
// (a nil pointer) disables SASL.
type SASLConfig struct {
	// Mechanism is the mechanism name as accepted by [ParseSASLMechanism].
	// It is required: there is no default.
	Mechanism string
	// Username is used by plain and scram mechanisms.
	Username string
	// Password is used by plain and scram mechanisms.
	Password string
}

// String redacts the password so a SASLConfig can be formatted safely.
func (s SASLConfig) String() string {
	return fmt.Sprintf("SASLConfig{Mechanism: %q, Username: %q, Password: [REDACTED]}", s.Mechanism, s.Username)
}

// GoString implements [fmt.GoStringer] and redacts the password.
func (s SASLConfig) GoString() string { return s.String() }

// Config holds the settings needed to build a [Client].
type Config struct {
	// BootstrapServer is the "host:port" address of a broker.
	BootstrapServer string
	// TLS configures transport encryption.
	TLS TLSConfig
	// SASL configures authentication; nil disables SASL.
	SASL *SASLConfig
	// Timeout bounds each request. Zero uses kafka-go defaults.
	Timeout time.Duration
	// DialTimeout bounds the time to open one connection to a broker. Zero
	// uses kafka-go defaults.
	DialTimeout time.Duration
}

// Validate checks the configuration without touching the network.
func (c Config) Validate() error {
	if strings.TrimSpace(c.BootstrapServer) == "" {
		return errors.New("bootstrap server is required")
	}
	if c.Timeout < 0 {
		return fmt.Errorf("timeout must not be negative, got %s", c.Timeout)
	}
	if c.DialTimeout < 0 {
		return fmt.Errorf("dial timeout must not be negative, got %s", c.DialTimeout)
	}
	if c.SASL != nil {
		if _, err := ParseSASLMechanism(c.SASL.Mechanism); err != nil {
			return err
		}
	}
	return nil
}

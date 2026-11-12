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

// SASLMechanism is a normalised SASL mechanism name.
type SASLMechanism string

// Supported SASL mechanisms.
const (
	SASLMechanismPlain       SASLMechanism = "plain"
	SASLMechanismScramSHA256 SASLMechanism = "scram-sha256"
	SASLMechanismScramSHA512 SASLMechanism = "scram-sha512"
	SASLMechanismAWSMSKIAM   SASLMechanism = "aws-msk-iam"
)

// DefaultSASLMechanism is the mechanism the provider uses when SASL is
// enabled and no mechanism is configured.
const DefaultSASLMechanism = SASLMechanismAWSMSKIAM

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
	// An empty value selects [DefaultSASLMechanism].
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
	// Timeout bounds dialing and each request. Zero uses kafka-go defaults.
	Timeout time.Duration
}

// Validate checks the configuration without touching the network.
func (c Config) Validate() error {
	if strings.TrimSpace(c.BootstrapServer) == "" {
		return errors.New("bootstrap server is required")
	}
	if c.Timeout < 0 {
		return fmt.Errorf("timeout must not be negative, got %s", c.Timeout)
	}
	if c.SASL != nil && c.SASL.Mechanism != "" {
		if _, err := ParseSASLMechanism(c.SASL.Mechanism); err != nil {
			return err
		}
	}
	return nil
}

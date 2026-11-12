package kafkaclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sigv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl"
	"github.com/segmentio/kafka-go/sasl/aws_msk_iam_v2"
	"github.com/segmentio/kafka-go/sasl/plain"
	"github.com/segmentio/kafka-go/sasl/scram"
)

const clientID = "terraform-provider-kafka"

// metadataTTL bounds how stale the transport's cached cluster metadata can be.
//
// kafka-go answers Metadata requests from a cache that a background goroutine
// refreshes at random intervals below this value (default 6s). Only a
// CreateTopics response forces an earlier refresh, and topic DescribeConfigs
// requests go to the controller named by that cache, so a topic read right
// after a change made elsewhere can otherwise see state up to 6s old.
//
// 2s cuts the worst case to 2s. The cost is one all-topics Metadata request
// per refresh, roughly every second on average, for as long as the client
// lives. The value also bounds how long each refresh may take (kafka-go uses
// it as the request deadline). A failed refresh keeps the previous cache, so
// a slow one only delays freshness. Going lower raises request load on large
// clusters for little gain.
const metadataTTL = 2 * time.Second

// awsConfigLoader loads the ambient AWS configuration. It is a seam so the
// aws-msk-iam path can be tested without AWS credentials or files.
type awsConfigLoader func(ctx context.Context) (aws.Config, error)

func defaultAWSConfigLoader(ctx context.Context) (aws.Config, error) {
	return awsconfig.LoadDefaultConfig(ctx)
}

// newTransport maps cfg to a kafka.Transport. It performs no network I/O.
func newTransport(ctx context.Context, cfg Config, loadAWS awsConfigLoader) (*kafka.Transport, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	transport := &kafka.Transport{
		ClientID:    clientID,
		DialTimeout: cfg.Timeout,
		MetadataTTL: metadataTTL,
	}

	if cfg.TLS.Enabled {
		transport.TLS = &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: cfg.TLS.SkipVerify, //nolint:gosec // opt-in via skip_verify
		}
	}

	if cfg.SASL != nil {
		mechanism, err := newSASLMechanism(ctx, *cfg.SASL, loadAWS)
		if err != nil {
			return nil, err
		}
		transport.SASL = mechanism
	}

	return transport, nil
}

func newSASLMechanism(ctx context.Context, cfg SASLConfig, loadAWS awsConfigLoader) (sasl.Mechanism, error) {
	name := cfg.Mechanism
	if name == "" {
		name = string(DefaultSASLMechanism)
	}
	mechanism, err := ParseSASLMechanism(name)
	if err != nil {
		return nil, err
	}

	switch mechanism {
	case SASLMechanismPlain:
		return plain.Mechanism{Username: cfg.Username, Password: cfg.Password}, nil
	case SASLMechanismScramSHA256:
		return newScram(scram.SHA256, cfg)
	case SASLMechanismScramSHA512:
		return newScram(scram.SHA512, cfg)
	case SASLMechanismAWSMSKIAM:
		awsCfg, err := loadAWS(ctx)
		if err != nil {
			return nil, fmt.Errorf("load aws configuration for %s: %w", mechanism, err)
		}
		return &aws_msk_iam_v2.Mechanism{
			Signer:      sigv4.NewSigner(),
			Region:      awsCfg.Region,
			Credentials: awsCfg.Credentials,
		}, nil
	default:
		// Unreachable: ParseSASLMechanism only returns known values.
		return nil, fmt.Errorf("unsupported SASL mechanism %q", mechanism)
	}
}

func newScram(algo scram.Algorithm, cfg SASLConfig) (sasl.Mechanism, error) {
	m, err := scram.Mechanism(algo, cfg.Username, cfg.Password)
	if err != nil {
		// Deliberately do not wrap err: it is not guaranteed to be free of
		// credential material, and callers only need to know the setup failed.
		return nil, fmt.Errorf("create %s SASL mechanism for user %q", algo.Name(), cfg.Username)
	}
	return m, nil
}

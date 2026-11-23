package kafkaclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
)

// Reader exposes the read-only operations of a Kafka cluster. It has no
// mutating methods, which is what makes [ReadOnlyClient] safe to hand to
// data sources. Add an operation to Reader only if it never changes the
// cluster. This includes topic, broker, ACL and quota reads.
type Reader struct {
	kc *kafka.Client

	// apiMu guards knownAPIs.
	apiMu sync.Mutex
	// knownAPIs holds the Kafka API keys that a broker has reported. A broker
	// does not lose an API, so the client keeps these for its whole life.
	knownAPIs map[int]struct{}
}

// Timeout returns the request timeout the client was configured with. Callers
// that wait for the cluster to converge use it to bound how long they wait.
func (r *Reader) Timeout() time.Duration {
	return r.kc.Timeout
}

// metadata returns cluster metadata for the given topics, or for all topics
// when none are given.
func (r *Reader) metadata(ctx context.Context, topics ...string) (*kafka.MetadataResponse, error) {
	resp, err := r.kc.Metadata(ctx, &kafka.MetadataRequest{Topics: topics})
	if err != nil {
		return nil, fmt.Errorf("fetch cluster metadata: %w", err)
	}
	return resp, nil
}

// Client is the read-write Kafka client. It embeds the read side and is the
// home of mutating operations, so a *Client can do everything while a
// *ReadOnlyClient obtained from [Client.ReadOnly] can only read.
type Client struct {
	*Reader
}

// ReadOnlyClient is a restricted view of a [Client] that only exposes the
// read operations of [Reader]. It deliberately does not expose the
// underlying kafka.Client.
type ReadOnlyClient struct {
	*Reader
}

// New builds a Client from cfg. It validates the configuration and builds
// the transport but does not contact the broker.
func New(ctx context.Context, cfg Config) (*Client, error) {
	return newClient(ctx, cfg, defaultAWSConfigLoader)
}

func newClient(ctx context.Context, cfg Config, loadAWS awsConfigLoader) (*Client, error) {
	transport, err := newTransport(ctx, cfg, loadAWS)
	if err != nil {
		return nil, fmt.Errorf("create kafka client: %w", err)
	}
	return &Client{Reader: &Reader{kc: &kafka.Client{
		Addr:      kafka.TCP(cfg.BootstrapServer),
		Timeout:   cfg.Timeout,
		Transport: transport,
	}}}, nil
}

// ReadOnly returns a view of c that cannot perform mutating operations.
func (c *Client) ReadOnly() *ReadOnlyClient {
	return &ReadOnlyClient{Reader: c.Reader}
}

var errNilClient = errors.New("kafkaclient: nil client")

// Close releases idle connections held by the client's transport. It returns
// an error for a nil client. A nil client is a programming mistake, and the
// error makes it visible instead of hiding it.
func (c *Client) Close() error {
	if c == nil || c.Reader == nil {
		return errNilClient
	}
	if t, ok := c.kc.Transport.(*kafka.Transport); ok {
		t.CloseIdleConnections()
	}
	return nil
}

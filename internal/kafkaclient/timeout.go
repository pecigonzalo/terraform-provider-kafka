package kafkaclient

import "time"

// Timeout returns the request timeout the client was configured with. Callers
// that wait for the cluster to converge use it to bound how long they wait.
func (r *Reader) Timeout() time.Duration {
	return r.kc.Timeout
}

package kafkaclient

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/segmentio/kafka-go"
)

// errNoResult means the broker answered a request but gave no result for one
// of the names in the request. The write may not have happened.
var errNoResult = errors.New("the broker returned no result")

// sentinelRule links a broker error code to the sentinel error that callers
// match with [errors.Is].
type sentinelRule struct {
	code     error
	sentinel error
}

// brokerSentinels is the one table that maps broker error codes to the
// sentinel errors of this package.
var brokerSentinels = []sentinelRule{
	{kafka.UnknownTopicOrPartition, ErrTopicNotFound},
	{kafka.InvalidPartitionNumber, ErrInvalidPartitions},
	{kafka.InvalidReplicaAssignment, ErrInvalidReplicaAssignment},
	{kafka.SecurityDisabled, ErrSecurityDisabled},
	{kafka.ClusterAuthorizationFailed, ErrNotAuthorized},
	{kafka.TopicAuthorizationFailed, ErrNotAuthorized},
	{kafka.GroupAuthorizationFailed, ErrNotAuthorized},
	{kafka.TransactionalIDAuthorizationFailed, ErrNotAuthorized},
	{kafka.DelegationTokenAuthorizationFailed, ErrNotAuthorized},
}

// classifyBrokerError adds the matching sentinel error to the chain of err and
// keeps err in the chain. The rules in extra apply before the shared table. An
// error with no matching rule is returned as it is.
func classifyBrokerError(err error, extra ...sentinelRule) error {
	for _, rule := range slices.Concat(extra, brokerSentinels) {
		if errors.Is(err, rule.code) {
			return fmt.Errorf("%w: %w", rule.sentinel, err)
		}
	}
	return err
}

// checkResults checks the per-name results of a broker response. A nil entry
// in results is a success. It returns an error for each name that has an error
// or, when the name is in expected, has no entry. The errors are joined and
// ordered by name. Each is prefixed with action and the name, and is classified
// as in [classifyBrokerError].
func checkResults(action string, expected []string, results map[string]error, extra ...sentinelRule) error {
	names := slices.Sorted(maps.Keys(results))
	for _, name := range expected {
		if _, ok := results[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)

	var errs []error
	for _, name := range names {
		err, ok := results[name]
		switch {
		case !ok:
			err = errNoResult
		case err == nil:
			continue
		}
		errs = append(errs, fmt.Errorf("%s %q: %w", action, name, classifyBrokerError(err, extra...)))
	}
	return errors.Join(errs...)
}

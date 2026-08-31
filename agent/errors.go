package agent

import "errors"

var (
	ErrDeadlineRequired = errors.New("agent: context deadline is required")
	errConsumerStopped = errors.New("agent: event consumer stopped")
)

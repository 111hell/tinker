package agent

import "errors"

var (
	ErrDeadlineRequired = errors.New("agent: context deadline is required")
	ErrTruncated        = errors.New("agent: model output truncated")
	ErrBlocked          = errors.New("agent: model output blocked")
	errConsumerStopped  = errors.New("agent: event consumer stopped")
)

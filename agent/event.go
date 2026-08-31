package agent

type EventType string

const (
	MessageDelta  EventType = "message_delta"
	ToolStarted   EventType = "tool_started"
	ToolCompleted EventType = "tool_completed"
	RunCompleted  EventType = "run_completed"
)

// Event reports model output, tool execution, or the final run result.
type Event struct {
	Type     EventType
	Delta    Delta
	ToolCall ToolCall
	Message  Message
	Messages []Message
	Usage    Usage
}

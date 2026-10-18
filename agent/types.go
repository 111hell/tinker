package agent

import (
	"context"
	"encoding/json"
	"iter"
)

type Config struct {
	Model        Model
	SystemPrompt string
	Tools        []Tool
	Skills       []Skill
}

type Agent struct {
	model        Model
	systemPrompt string
	tools        map[string]Tool
	definitions  []Tool
}

type Role string

const (
	UserRole      Role = "user"
	AssistantRole Role = "assistant"
	ToolRole      Role = "tool"
)

// Message is the provider-neutral conversation record.
type Message struct {
	Role       Role
	Text       string
	Reasoning  string
	ImageURLs  []string
	ToolCalls  []ToolCall
	ToolCallID string
	// Continuation holds protocol-owned assistant data. Preserve it unchanged in history.
	// It may contain encrypted reasoning or signatures; do not display it as text.
	Continuation *Continuation
	FinishReason FinishReason
}

type Continuation struct {
	Protocol string
	Data     json.RawMessage
}

type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishToolCalls FinishReason = "tool_calls"
	FinishLength    FinishReason = "length"
	FinishBlocked   FinishReason = "blocked"
)

func UserMessage(text string) Message { return Message{Role: UserRole, Text: text} }

func UserImageMessage(text string, imageURLs ...string) Message {
	return Message{Role: UserRole, Text: text, ImageURLs: imageURLs}
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type Request struct {
	SystemPrompt string
	Messages     []Message
	Tools        []Tool
}

type Usage struct {
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	TotalTokens     int64
}

// Delta is one provider-neutral model stream update. Usage is a cumulative
// snapshot for the current response (zero means no update). Continuation is a
// complete replacement, not a fragment. Built-in adapters report FinishReason;
// custom Models that omit it retain the original end-of-stream behavior.
type Delta struct {
	Text         string
	Reasoning    string
	ToolCall     *ToolCallDelta
	Usage        Usage
	Continuation *Continuation
	FinishReason FinishReason
}

type ToolCallDelta struct {
	Index int
	// ID is the accumulated ID; Name and ArgumentsDelta are appended fragments.
	ID             string
	Name           string
	ArgumentsDelta string
}

type Model interface {
	Stream(context.Context, Request) iter.Seq2[Delta, error]
}

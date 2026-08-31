package agent

import (
	"context"
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
	ToolCalls  []ToolCall
	ToolCallID string
}

func UserMessage(text string) Message { return Message{Role: UserRole, Text: text} }

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

// Delta is one provider-neutral model stream update.
type Delta struct {
	Text      string
	Reasoning string
	ToolCall  *ToolCallDelta
	Usage     Usage
}

type ToolCallDelta struct {
	Index          int
	ID             string
	Name           string
	ArgumentsDelta string
}

type Model interface {
	Stream(context.Context, Request) iter.Seq2[Delta, error]
}

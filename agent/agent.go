// Package agent provides a small model-tool loop with streaming events.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
)

func New(config Config) (*Agent, error) {
	if config.Model == nil {
		return nil, errors.New("agent: model is required")
	}
	a := &Agent{
		model: config.Model, systemPrompt: config.SystemPrompt,
		tools: make(map[string]Tool, len(config.Tools)),
	}
	if err := a.registerTools(config.Tools); err != nil {
		return nil, err
	}
	if err := a.loadSkills(config.Skills); err != nil {
		return nil, err
	}
	return a, nil
}

// Run adds input as a user message and runs until the model stops or an error occurs.
// The caller must provide a context with a deadline to bound the model-tool loop.
func (a *Agent) Run(ctx context.Context, history []Message, input string) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		emit := func(event Event) bool { return yield(event, nil) }
		messages := append([]Message(nil), history...)
		user := UserMessage(input)
		messages = append(messages, user)
		var total Usage

		if err := ctx.Err(); err != nil {
			yield(Event{}, err)
			return
		}
		if _, ok := ctx.Deadline(); !ok {
			yield(Event{}, ErrDeadlineRequired)
			return
		}

		for {
			assistant, usage, err := a.stream(ctx, messages, emit)
			if errors.Is(err, errConsumerStopped) {
				return
			}
			if err != nil {
				yield(Event{}, err)
				return
			}
			messages = append(messages, assistant)
			total.InputTokens += usage.InputTokens
			total.OutputTokens += usage.OutputTokens
			total.ReasoningTokens += usage.ReasoningTokens
			total.TotalTokens += usage.TotalTokens

			if len(assistant.ToolCalls) == 0 {
				emit(Event{Type: RunCompleted, Message: assistant,
					Messages: append([]Message(nil), messages...), Usage: total})
				return
			}

			results, err := a.executeTools(ctx, assistant, emit)
			if errors.Is(err, errConsumerStopped) {
				return
			}
			if err != nil {
				yield(Event{}, err)
				return
			}
			messages = append(messages, results...)
		}
	}
}

func (a *Agent) stream(ctx context.Context, messages []Message, emit func(Event) bool) (Message, Usage, error) {
	message := Message{Role: AssistantRole}
	var usage Usage
	request := Request{a.systemPrompt, append([]Message(nil), messages...), append([]Tool(nil), a.definitions...)}
	for delta, err := range a.model.Stream(ctx, request) {
		if err != nil {
			return Message{}, Usage{}, err
		}
		if err := ctx.Err(); err != nil {
			return Message{}, Usage{}, err
		}
		message.Text += delta.Text
		message.Reasoning += delta.Reasoning
		if delta.ToolCall != nil {
			toolCall := delta.ToolCall
			if toolCall.Index < 0 {
				return Message{}, Usage{}, fmt.Errorf("tool call index must be non-negative: %d", toolCall.Index)
			}
			for len(message.ToolCalls) <= toolCall.Index {
				message.ToolCalls = append(message.ToolCalls, ToolCall{})
			}
			call := &message.ToolCalls[toolCall.Index]
			if toolCall.ID != "" {
				call.ID = toolCall.ID
			}
			call.Name += toolCall.Name
			call.Arguments += toolCall.ArgumentsDelta
		}
		usage = delta.Usage
		if (delta.Text != "" || delta.Reasoning != "") &&
			!emit(Event{Type: MessageDelta, Delta: delta}) {
			return Message{}, Usage{}, errConsumerStopped
		}
	}
	return message, usage, nil
}

func (a *Agent) executeTools(
	ctx context.Context, assistant Message, emit func(Event) bool,
) ([]Message, error) {
	results := make([]Message, 0, len(assistant.ToolCalls))
	for _, call := range assistant.ToolCalls {
		if !emit(Event{Type: ToolStarted, ToolCall: call}) {
			return nil, errConsumerStopped
		}
		var output string
		var err error
		switch {
		case a.tools[call.Name].Execute == nil:
			err = fmt.Errorf("tool %q is not registered", call.Name)
		case !json.Valid([]byte(call.Arguments)):
			err = errors.New("tool call not executed: invalid JSON arguments")
		default:
			output, err = a.tools[call.Name].Execute(ctx, call.Arguments)
		}
		if err != nil {
			raw, _ := json.Marshal(map[string]string{"error": err.Error()})
			output = string(raw)
		}
		result := Message{Role: ToolRole, Text: output, ToolCallID: call.ID}
		if !emit(Event{Type: ToolCompleted, ToolCall: call, Message: result}) {
			return nil, errConsumerStopped
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		results = append(results, result)
	}
	return results, nil
}

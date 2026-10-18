// Package openairesponses implements the OpenAI Responses protocol.
package openairesponses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strings"

	"github.com/111hell/tinker/agent"
	"github.com/111hell/tinker/internal/sse"
)

const protocol = "openai-responses"

// Reasoning controls effort and visible summaries independently. Support varies
// by model; model-specific restrictions are reported by the server.
type Reasoning struct {
	Effort  Effort  `json:"effort,omitempty"`
	Summary Summary `json:"summary,omitempty"`
}
type Effort string

const (
	EffortNone    Effort = "none"
	EffortMinimal Effort = "minimal"
	EffortLow     Effort = "low"
	EffortMedium  Effort = "medium"
	EffortHigh    Effort = "high"
	EffortXhigh   Effort = "xhigh"
	EffortMax     Effort = "max"
)

type Summary string

const (
	SummaryAuto     Summary = "auto"
	SummaryConcise  Summary = "concise"
	SummaryDetailed Summary = "detailed"
)

type Config struct {
	APIKey string
	// BaseURL is the API base URL, including /v1 when required.
	BaseURL    string
	Model      string
	HTTPClient *http.Client
	Reasoning  Reasoning
}
type Model struct{ config Config }

var _ agent.Model = (*Model)(nil)

func New(c Config) (*Model, error) {
	if c.APIKey == "" || c.Model == "" {
		return nil, errors.New("openairesponses: APIKey and Model are required")
	}
	if c.BaseURL == "" {
		c.BaseURL = "https://api.openai.com/v1"
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("openairesponses: invalid BaseURL")
	}
	switch c.Reasoning.Effort {
	case "", EffortNone, EffortMinimal, EffortLow, EffortMedium, EffortHigh, EffortXhigh, EffortMax:
	default:
		return nil, fmt.Errorf("openairesponses: unsupported reasoning effort %q", c.Reasoning.Effort)
	}
	switch c.Reasoning.Summary {
	case "", SummaryAuto, SummaryConcise, SummaryDetailed:
	default:
		return nil, fmt.Errorf("openairesponses: unsupported reasoning summary %q", c.Reasoning.Summary)
	}
	if c.HTTPClient == nil {
		c.HTTPClient = http.DefaultClient
	}
	return &Model{config: c}, nil
}

func (m *Model) body(r agent.Request) ([]byte, error) {
	input := make([]any, 0, len(r.Messages))
	for _, msg := range r.Messages {
		if msg.Continuation != nil {
			if msg.Role != agent.AssistantRole || msg.Continuation.Protocol != protocol || len(msg.ImageURLs) > 0 || msg.ToolCallID != "" {
				return nil, errors.New("openairesponses: incompatible continuation")
			}
			var items []json.RawMessage
			if err := json.Unmarshal(msg.Continuation.Data, &items); err != nil || len(items) == 0 {
				return nil, errors.New("openairesponses: invalid continuation output items")
			}
			var text, reasoning string
			var calls []agent.ToolCall
			for _, raw := range items {
				item, err := parseItem(raw)
				if err != nil {
					return nil, err
				}
				switch item.Type {
				case "message":
					for _, part := range item.Content {
						text += part.Text + part.Refusal
					}
				case "reasoning":
					for _, part := range item.Summary {
						reasoning += part.Text
					}
					for _, part := range item.Content {
						reasoning += part.Text
					}
				case "function_call":
					calls = append(calls, agent.ToolCall{ID: item.CallID, Name: item.Name, Arguments: item.Arguments})
				}
				input = append(input, raw)
			}
			if text != msg.Text || reasoning != msg.Reasoning || len(calls) != len(msg.ToolCalls) {
				return nil, errors.New("openairesponses: message differs from continuation")
			}
			for i, call := range calls {
				var native, unified bytes.Buffer
				if err := json.Compact(&native, []byte(call.Arguments)); err != nil {
					return nil, fmt.Errorf("openairesponses: invalid continuation arguments: %w", err)
				}
				if err := json.Compact(&unified, []byte(msg.ToolCalls[i].Arguments)); err != nil {
					return nil, fmt.Errorf("openairesponses: invalid message arguments: %w", err)
				}
				if call.ID != msg.ToolCalls[i].ID || call.Name != msg.ToolCalls[i].Name || !bytes.Equal(native.Bytes(), unified.Bytes()) {
					return nil, errors.New("openairesponses: tool call differs from continuation")
				}
			}
			continue
		}
		if msg.Reasoning != "" {
			return nil, errors.New("openairesponses: reasoning history requires native continuation")
		}
		switch msg.Role {
		case agent.UserRole, agent.AssistantRole:
			if msg.ToolCallID != "" || (msg.Role == agent.UserRole && len(msg.ToolCalls) > 0) || (msg.Role == agent.AssistantRole && len(msg.ImageURLs) > 0) {
				return nil, errors.New("openairesponses: unsupported message fields")
			}
			parts := make([]any, 0, len(msg.ImageURLs)+1)
			if msg.Text != "" || (len(msg.ImageURLs) == 0 && len(msg.ToolCalls) == 0) {
				parts = append(parts, map[string]any{"type": "input_text", "text": msg.Text})
			}
			for _, image := range msg.ImageURLs {
				if image == "" {
					return nil, errors.New("openairesponses: empty image URL")
				}
				parts = append(parts, map[string]any{"type": "input_image", "image_url": image})
			}
			if len(parts) > 0 {
				input = append(input, map[string]any{"role": msg.Role, "content": parts})
			}
			for _, call := range msg.ToolCalls {
				if call.ID == "" || call.Name == "" || !json.Valid([]byte(call.Arguments)) {
					return nil, errors.New("openairesponses: invalid tool call")
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Name, "arguments": call.Arguments})
			}
		case agent.ToolRole:
			if msg.ToolCallID == "" || len(msg.ImageURLs) > 0 || len(msg.ToolCalls) > 0 {
				return nil, errors.New("openairesponses: invalid tool result")
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": msg.ToolCallID, "output": msg.Text})
		default:
			return nil, fmt.Errorf("openairesponses: unsupported role %q", msg.Role)
		}
	}
	body := map[string]any{"model": m.config.Model, "input": input, "stream": true, "store": false, "include": []string{"reasoning.encrypted_content"}}
	if r.SystemPrompt != "" {
		body["instructions"] = r.SystemPrompt
	}
	if m.config.Reasoning != (Reasoning{}) {
		body["reasoning"] = m.config.Reasoning
	}
	if len(r.Tools) > 0 {
		tools := make([]any, 0, len(r.Tools))
		for _, tool := range r.Tools {
			var schema map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tool.InputSchema), &schema); err != nil || schema == nil {
				return nil, fmt.Errorf("openairesponses: tool %q schema must be a JSON object", tool.Name)
			}
			// Responses defaults to strict schema normalization; preserve the existing
			// library contract, which does not require strict-mode JSON schemas.
			tools = append(tools, map[string]any{"type": "function", "name": tool.Name, "description": tool.Description, "parameters": json.RawMessage(tool.InputSchema), "strict": false})
		}
		body["tools"] = tools
	}
	return json.Marshal(body)
}

type outputItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
}

func parseItem(raw json.RawMessage) (outputItem, error) {
	var item outputItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return item, fmt.Errorf("openairesponses: invalid output item: %w", err)
	}
	switch item.Type {
	case "reasoning":
	case "function_call":
	case "message":
		for _, part := range item.Content {
			if part.Type != "output_text" && part.Type != "refusal" {
				return item, fmt.Errorf("openairesponses: unsupported output content %q", part.Type)
			}
		}
	default:
		return item, fmt.Errorf("openairesponses: unsupported output item %q", item.Type)
	}
	return item, nil
}

type streamEvent struct {
	Type        string          `json:"type"`
	Delta       string          `json:"delta"`
	OutputIndex int             `json:"output_index"`
	Item        json.RawMessage `json:"item"`
	Response    struct {
		Output     []json.RawMessage `json:"output"`
		Incomplete struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Usage struct {
			Input   int64 `json:"input_tokens"`
			Output  int64 `json:"output_tokens"`
			Total   int64 `json:"total_tokens"`
			Details struct {
				Reasoning int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	} `json:"response"`
}

func (m *Model) Stream(ctx context.Context, r agent.Request) iter.Seq2[agent.Delta, error] {
	return func(yield func(agent.Delta, error) bool) {
		body, err := m.body(r)
		if err != nil {
			yield(agent.Delta{}, err)
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.config.BaseURL, "/")+"/responses", bytes.NewReader(body))
		if err != nil {
			yield(agent.Delta{}, err)
			return
		}
		req.Header.Set("Authorization", "Bearer "+m.config.APIKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		resp, err := m.config.HTTPClient.Do(req)
		if err != nil {
			yield(agent.Delta{}, err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			data, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				yield(agent.Delta{}, readErr)
			} else {
				yield(agent.Delta{}, fmt.Errorf("openairesponses: HTTP %d: %s", resp.StatusCode, data))
			}
			return
		}
		calls := map[int]agent.ToolCall{}
		indexes := map[int]int{}
		process := func(raw string) bool {
			var e streamEvent
			if err := json.Unmarshal([]byte(raw), &e); err != nil {
				yield(agent.Delta{}, fmt.Errorf("openairesponses: decode stream event: %w", err))
				return false
			}
			switch e.Type {
			case "response.output_text.delta", "response.refusal.delta":
				return yield(agent.Delta{Text: e.Delta}, nil)
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				return yield(agent.Delta{Reasoning: e.Delta}, nil)
			case "response.output_item.added":
				item, err := parseItem(e.Item)
				if err != nil {
					yield(agent.Delta{}, err)
					return false
				}
				if item.Type == "function_call" {
					if _, exists := indexes[e.OutputIndex]; exists {
						yield(agent.Delta{}, errors.New("openairesponses: duplicate function call index"))
						return false
					}
					indexes[e.OutputIndex] = len(indexes)
					calls[e.OutputIndex] = agent.ToolCall{ID: item.CallID, Name: item.Name, Arguments: item.Arguments}
					return yield(agent.Delta{ToolCall: &agent.ToolCallDelta{Index: indexes[e.OutputIndex], ID: item.CallID, Name: item.Name, ArgumentsDelta: item.Arguments}}, nil)
				}
			case "response.function_call_arguments.delta":
				call, ok := calls[e.OutputIndex]
				if !ok {
					yield(agent.Delta{}, errors.New("openairesponses: arguments without function call"))
					return false
				}
				call.Arguments += e.Delta
				calls[e.OutputIndex] = call
				return yield(agent.Delta{ToolCall: &agent.ToolCallDelta{Index: indexes[e.OutputIndex], ArgumentsDelta: e.Delta}}, nil)
			case "response.output_item.done":
				if _, err := parseItem(e.Item); err != nil {
					yield(agent.Delta{}, err)
					return false
				}
			case "error", "response.failed":
				yield(agent.Delta{}, fmt.Errorf("openairesponses: API stream error: %s", raw))
				return false
			case "response.completed", "response.incomplete":
				finish := agent.FinishStop
				if e.Type == "response.incomplete" {
					switch e.Response.Incomplete.Reason {
					case "max_output_tokens":
						finish = agent.FinishLength
					case "content_filter":
						finish = agent.FinishBlocked
					default:
						yield(agent.Delta{}, fmt.Errorf("openairesponses: incomplete response: %s", raw))
						return false
					}
				}
				completedCalls := map[int]bool{}
				for i, rawItem := range e.Response.Output {
					item, err := parseItem(rawItem)
					if err != nil {
						yield(agent.Delta{}, err)
						return false
					}
					if item.Type == "function_call" && e.Type == "response.completed" {
						if item.CallID == "" || item.Name == "" || !json.Valid([]byte(item.Arguments)) || item.Status == "incomplete" {
							yield(agent.Delta{}, errors.New("openairesponses: incomplete function call in completed response"))
							return false
						}
						completedCalls[i] = true
						old := calls[i]
						if _, exists := indexes[i]; !exists {
							indexes[i] = len(indexes)
						}
						if !strings.HasPrefix(item.Arguments, old.Arguments) || (old.ID != "" && old.ID != item.CallID) || (old.Name != "" && old.Name != item.Name) {
							yield(agent.Delta{}, errors.New("openairesponses: inconsistent function call stream"))
							return false
						}
						delta := agent.ToolCallDelta{Index: indexes[i], ArgumentsDelta: strings.TrimPrefix(item.Arguments, old.Arguments)}
						if old.ID == "" {
							delta.ID = item.CallID
						}
						if old.Name == "" {
							delta.Name = item.Name
						}
						if (delta.ID != "" || delta.Name != "" || delta.ArgumentsDelta != "") && !yield(agent.Delta{ToolCall: &delta}, nil) {
							return false
						}
						if finish != agent.FinishBlocked {
							finish = agent.FinishToolCalls
						}
					}
					for _, part := range item.Content {
						if part.Type == "refusal" && e.Type == "response.completed" {
							finish = agent.FinishBlocked
						}
					}
				}
				missingCall := false
				for index := range calls {
					if !completedCalls[index] {
						missingCall = true
					}
				}
				if e.Type == "response.completed" && missingCall {
					yield(agent.Delta{}, errors.New("openairesponses: completed response omitted streamed function calls"))
					return false
				}
				final := agent.Delta{FinishReason: finish, Usage: agent.Usage{InputTokens: e.Response.Usage.Input, OutputTokens: e.Response.Usage.Output, TotalTokens: e.Response.Usage.Total, ReasoningTokens: e.Response.Usage.Details.Reasoning}}
				if len(e.Response.Output) > 0 {
					native, err := json.Marshal(e.Response.Output)
					if err != nil {
						yield(agent.Delta{}, err)
						return false
					}
					final.Continuation = &agent.Continuation{Protocol: protocol, Data: native}
				}
				yield(final, nil)
				return false
			}
			return true
		}
		stopped, err := sse.Read(ctx, resp.Body, process)
		if err != nil {
			yield(agent.Delta{}, err)
		} else if !stopped {
			yield(agent.Delta{}, errors.New("openairesponses: stream ended before terminal response"))
		}
	}
}

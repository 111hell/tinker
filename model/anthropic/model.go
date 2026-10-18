// Package anthropic adapts the native Anthropic Messages API to agent.Model.
package anthropic

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"reflect"
	"strings"

	"github.com/111hell/tinker/agent"
	"github.com/111hell/tinker/internal/sse"
)

// Thinking configures thinking independently of whether its summary is visible.
// Enabled requires BudgetTokens >= 1024 and less than MaxTokens. Adaptive and
// disabled do not accept a budget. Model-specific support is checked by the API.
type Thinking struct {
	Type         string `json:"type"` // enabled, adaptive, or disabled
	BudgetTokens int64  `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"` // summarized or omitted
}

type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

type Config struct {
	APIKey string
	// BaseURL is the API base URL, including /v1; defaults to https://api.anthropic.com/v1.
	BaseURL    string
	Model      string
	HTTPClient *http.Client
	MaxTokens  int64
	Thinking   *Thinking
	// Effort is sent as output_config.effort; it does not enable thinking.
	Effort Effort
}

type Model struct{ config Config }

var _ agent.Model = (*Model)(nil)

func New(c Config) (*Model, error) {
	if c.APIKey == "" || c.Model == "" || c.MaxTokens <= 0 {
		return nil, errors.New("anthropic: APIKey, Model and positive MaxTokens are required")
	}
	if c.BaseURL == "" {
		c.BaseURL = "https://api.anthropic.com/v1"
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("anthropic: invalid BaseURL")
	}
	if c.HTTPClient == nil {
		c.HTTPClient = http.DefaultClient
	}
	if c.Thinking != nil {
		t := *c.Thinking
		c.Thinking = &t
		switch t.Type {
		case "enabled":
			if t.BudgetTokens < 1024 || t.BudgetTokens >= c.MaxTokens {
				return nil, errors.New("anthropic: enabled thinking requires 1024 <= BudgetTokens < MaxTokens")
			}
		case "adaptive", "disabled":
			if t.BudgetTokens != 0 {
				return nil, errors.New("anthropic: budget only supported with enabled thinking")
			}
		default:
			return nil, fmt.Errorf("anthropic: unsupported thinking type %q", t.Type)
		}
		if t.Display != "" && t.Display != "summarized" && t.Display != "omitted" {
			return nil, fmt.Errorf("anthropic: unsupported thinking display %q", t.Display)
		}
		if t.Type == "disabled" && t.Display != "" {
			return nil, errors.New("anthropic: disabled thinking does not accept Display")
		}
	}
	switch c.Effort {
	case "", EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax:
	default:
		return nil, fmt.Errorf("anthropic: unsupported effort %q", c.Effort)
	}
	return &Model{c}, nil
}

type message struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

func decodeObject(s string) (map[string]any, error) {
	var object map[string]any
	decoder := json.NewDecoder(strings.NewReader(s))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("anthropic: expected JSON object")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("anthropic: expected one JSON object")
	}
	return object, nil
}

func object(s string) bool {
	_, err := decodeObject(s)
	return err == nil
}
func image(s string) (map[string]any, error) {
	if strings.HasPrefix(s, "data:") {
		parts := strings.SplitN(s, ",", 2)
		if len(parts) != 2 {
			return nil, errors.New("anthropic: malformed image data URL")
		}
		mime := strings.TrimSuffix(strings.TrimPrefix(parts[0], "data:"), ";base64")
		switch mime {
		case "image/jpeg", "image/png", "image/gif", "image/webp":
		default:
			return nil, fmt.Errorf("anthropic: unsupported image media type %q", mime)
		}
		if !strings.HasSuffix(parts[0], ";base64") {
			return nil, errors.New("anthropic: image data URL must use base64")
		}
		if _, err := base64.StdEncoding.DecodeString(parts[1]); err != nil {
			return nil, fmt.Errorf("anthropic: invalid image data: %w", err)
		}
		return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mime, "data": parts[1]}}, nil
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("anthropic: image must be an HTTP URL or base64 image data URL")
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": s}}, nil
}
func (m *Model) params(r agent.Request) (map[string]any, error) {
	messages := make([]message, 0, len(r.Messages))
	for _, v := range r.Messages {
		if v.Continuation != nil && (v.Role != agent.AssistantRole || v.Continuation.Protocol != "anthropic") {
			return nil, errors.New("anthropic: incompatible continuation")
		}
		if v.Role != agent.UserRole && len(v.ImageURLs) > 0 {
			return nil, errors.New("anthropic: images supported only in user messages")
		}
		if v.Role != agent.AssistantRole && (v.Reasoning != "" || len(v.ToolCalls) > 0) {
			return nil, errors.New("anthropic: reasoning and tool calls require assistant role")
		}
		if v.Role != agent.ToolRole && v.ToolCallID != "" {
			return nil, errors.New("anthropic: ToolCallID requires tool role")
		}
		out := message{Role: string(v.Role), Content: []any{}}
		switch v.Role {
		case agent.UserRole:
			if v.Text != "" {
				out.Content = append(out.Content, map[string]any{"type": "text", "text": v.Text})
			}
			for _, s := range v.ImageURLs {
				b, err := image(s)
				if err != nil {
					return nil, err
				}
				out.Content = append(out.Content, b)
			}
		case agent.AssistantRole:
			if v.Continuation != nil {
				var native []json.RawMessage
				if err := json.Unmarshal(v.Continuation.Data, &native); err != nil || len(native) == 0 {
					return nil, errors.New("anthropic: invalid continuation content")
				}
				var text, reasoning string
				calls := []agent.ToolCall{}
				for _, block := range native {
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(block, &fields); err != nil || fields == nil {
						return nil, errors.New("anthropic: invalid continuation block")
					}
					values, err := stringFields(fields)
					if err != nil {
						return nil, err
					}
					switch values["type"] {
					case "text":
						text += values["text"]
					case "thinking":
						if values["signature"] == "" {
							return nil, errors.New("anthropic: thinking continuation lacks signature")
						}
						reasoning += values["thinking"]
					case "redacted_thinking":
						if len(fields["data"]) == 0 {
							return nil, errors.New("anthropic: redacted thinking lacks data")
						}
					case "tool_use":
						if values["id"] == "" || values["name"] == "" {
							return nil, errors.New("anthropic: invalid continuation tool call")
						}
						calls = append(calls, agent.ToolCall{ID: values["id"], Name: values["name"], Arguments: string(fields["input"])})
					default:
						return nil, fmt.Errorf("anthropic: unsupported continuation block %q", values["type"])
					}
					out.Content = append(out.Content, block)
				}
				if text != v.Text || reasoning != v.Reasoning || len(calls) != len(v.ToolCalls) {
					return nil, errors.New("anthropic: message differs from its continuation")
				}
				for i, c := range calls {
					given := v.ToolCalls[i]
					if c.ID != given.ID || c.Name != given.Name {
						return nil, errors.New("anthropic: tool call differs from its continuation")
					}
					a, err := decodeObject(c.Arguments)
					if err != nil {
						return nil, fmt.Errorf("anthropic: invalid continuation tool arguments: %w", err)
					}
					b, err := decodeObject(given.Arguments)
					if err != nil {
						return nil, fmt.Errorf("anthropic: invalid tool arguments: %w", err)
					}
					if !reflect.DeepEqual(a, b) {
						return nil, errors.New("anthropic: tool arguments differ from continuation")
					}
				}

				break
			}
			if v.Reasoning != "" {
				return nil, errors.New("anthropic: reasoning requires native continuation with signature")
			}
			if v.Text != "" {
				out.Content = append(out.Content, map[string]any{"type": "text", "text": v.Text})
			}
			for _, c := range v.ToolCalls {
				if c.ID == "" || c.Name == "" || !object(c.Arguments) {
					return nil, errors.New("anthropic: invalid tool call")
				}
				out.Content = append(out.Content, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": json.RawMessage(c.Arguments)})
			}
		case agent.ToolRole:
			if v.ToolCallID == "" {
				return nil, errors.New("anthropic: tool result lacks ToolCallID")
			}
			out.Role = "user"
			out.Content = append(out.Content, map[string]any{"type": "tool_result", "tool_use_id": v.ToolCallID, "content": v.Text})
		default:
			return nil, fmt.Errorf("anthropic: unsupported role %q", v.Role)
		}
		// Tool results from parallel calls must share one user turn.
		if len(messages) > 0 && messages[len(messages)-1].Role == out.Role {
			messages[len(messages)-1].Content = append(messages[len(messages)-1].Content, out.Content...)
		} else {
			messages = append(messages, out)
		}
	}
	p := map[string]any{"model": m.config.Model, "max_tokens": m.config.MaxTokens, "stream": true, "messages": messages}
	if r.SystemPrompt != "" {
		p["system"] = r.SystemPrompt
	}
	if m.config.Thinking != nil {
		p["thinking"] = m.config.Thinking
	}
	if m.config.Effort != "" {
		p["output_config"] = map[string]any{"effort": m.config.Effort}
	}
	if len(r.Tools) > 0 {
		tools := make([]any, 0, len(r.Tools))
		for _, t := range r.Tools {
			if t.Name == "" || !object(t.InputSchema) {
				return nil, fmt.Errorf("anthropic: invalid tool %q schema", t.Name)
			}
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": json.RawMessage(t.InputSchema)})
		}
		p["tools"] = tools
	}
	return p, nil
}

func (m *Model) Stream(ctx context.Context, r agent.Request) iter.Seq2[agent.Delta, error] {
	return func(yield func(agent.Delta, error) bool) {
		p, err := m.params(r)
		if err != nil {
			yield(agent.Delta{}, err)
			return
		}
		body, err := json.Marshal(p)
		if err != nil {
			yield(agent.Delta{}, err)
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.config.BaseURL, "/")+"/messages", bytes.NewReader(body))
		if err != nil {
			yield(agent.Delta{}, err)
			return
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("accept", "text/event-stream")
		req.Header.Set("x-api-key", m.config.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := m.config.HTTPClient.Do(req)
		if err != nil {
			yield(agent.Delta{}, err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if e != nil {
				yield(agent.Delta{}, e)
				return
			}
			yield(agent.Delta{}, fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, b))
			return
		}
		m.read(ctx, resp.Body, yield)
	}
}

type block struct {
	fields    map[string]json.RawMessage
	args      string
	toolIndex int
	closed    bool
	values    map[string]string
}

func stringFields(fields map[string]json.RawMessage) (map[string]string, error) {
	values := make(map[string]string)
	for _, key := range []string{"type", "text", "thinking", "signature", "id", "name", "partial_json", "stop_reason"} {
		if value, ok := fields[key]; ok && string(value) != "null" {
			var decoded string
			if err := json.Unmarshal(value, &decoded); err != nil {
				return nil, fmt.Errorf("anthropic: invalid %s: %w", key, err)
			}
			values[key] = decoded
		}
	}
	return values, nil
}

// Usage events are cumulative snapshots, but message_delta may omit fields.
// Decoding into the same value preserves input/cache counts from message_start.
// Unused metadata and future fields are intentionally ignored.
type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokensDetails      struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

func (m *Model) read(ctx context.Context, r io.Reader, yield func(agent.Delta, error) bool) {
	blocks := []block{}
	var tokens usage
	var finish agent.FinishReason
	started := false
	toolCount := 0
	fail := func(e error) bool { yield(agent.Delta{}, e); return false }
	handle := func(data string) bool {
		var e struct {
			Type    string                     `json:"type"`
			Index   int                        `json:"index"`
			Content map[string]json.RawMessage `json:"content_block"`
			Delta   map[string]json.RawMessage `json:"delta"`
			Message struct {
				Usage json.RawMessage `json:"usage"`
			} `json:"message"`
			Usage json.RawMessage `json:"usage"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return fail(fmt.Errorf("anthropic: invalid stream event: %w", err))
		}
		cs, err := stringFields(e.Content)
		if err != nil {
			return fail(err)
		}
		ds, err := stringFields(e.Delta)
		if err != nil {
			return fail(err)
		}
		switch e.Type {
		case "message_start":
			if started {
				return fail(errors.New("anthropic: duplicate message_start"))
			}
			started = true
			if len(e.Message.Usage) > 0 {
				if err := json.Unmarshal(e.Message.Usage, &tokens); err != nil {
					return fail(fmt.Errorf("anthropic: invalid usage: %w", err))
				}
			}
		case "content_block_start":
			if !started || e.Index != len(blocks) {
				return fail(errors.New("anthropic: invalid content block index"))
			}
			b := block{fields: e.Content, values: cs, toolIndex: -1}
			d := agent.Delta{}
			switch cs["type"] {
			case "text":
				d.Text = cs["text"]
			case "thinking":
				d.Reasoning = cs["thinking"]
			case "redacted_thinking":
			case "tool_use":
				b.toolIndex = toolCount
				toolCount++
				if cs["id"] == "" || cs["name"] == "" {
					return fail(errors.New("anthropic: tool call lacks ID or name"))
				}
				d.ToolCall = &agent.ToolCallDelta{Index: b.toolIndex, ID: cs["id"], Name: cs["name"]}
			default:
				return fail(fmt.Errorf("anthropic: unsupported response content block %q", cs["type"]))
			}
			blocks = append(blocks, b)
			if d.Text != "" || d.Reasoning != "" || d.ToolCall != nil {
				return yield(d, nil)
			}
		case "content_block_delta":
			if e.Index < 0 || e.Index >= len(blocks) || blocks[e.Index].closed {
				return fail(errors.New("anthropic: delta for missing or closed block"))
			}
			b := &blocks[e.Index]
			d := agent.Delta{}
			key := ""
			value := ""
			switch ds["type"] {
			case "text_delta":
				key = "text"
				value = ds["text"]
				d.Text = value
			case "thinking_delta":
				key = "thinking"
				value = ds["thinking"]
				d.Reasoning = value
			case "signature_delta":
				key = "signature"
				value = ds["signature"]
			case "input_json_delta":
				if b.toolIndex < 0 {
					return fail(errors.New("anthropic: tool input delta on non-tool block"))
				}
				value = ds["partial_json"]
				b.args += value
				d.ToolCall = &agent.ToolCallDelta{Index: b.toolIndex, ArgumentsDelta: value}
			default:
				return fail(fmt.Errorf("anthropic: unsupported content delta %q", ds["type"]))
			}
			if key != "" {
				expected := key
				if key == "signature" {
					expected = "thinking"
				}
				if b.values["type"] != expected {
					return fail(errors.New("anthropic: delta type does not match content block"))
				}
				b.values[key] += value
				encodedValue, err := json.Marshal(b.values[key])
				if err != nil {
					return fail(err)
				}
				b.fields[key] = encodedValue
			}
			if d.Text != "" || d.Reasoning != "" || d.ToolCall != nil {
				return yield(d, nil)
			}
		case "content_block_stop":
			if e.Index < 0 || e.Index >= len(blocks) || blocks[e.Index].closed {
				return fail(errors.New("anthropic: invalid block stop"))
			}
			b := &blocks[e.Index]
			b.closed = true
			if b.toolIndex >= 0 {
				args := b.args
				if args == "" {
					args = string(b.fields["input"])
					if args == "" {
						args = "{}"
					}
					if !yield(agent.Delta{ToolCall: &agent.ToolCallDelta{Index: b.toolIndex, ArgumentsDelta: args}}, nil) {
						return false
					}
				}

				b.fields["input"] = json.RawMessage(args)
			}
		case "message_delta":
			switch s := ds["stop_reason"]; s {
			case "":
			case "end_turn", "stop_sequence":
				finish = agent.FinishStop
			case "tool_use":
				finish = agent.FinishToolCalls
			case "max_tokens", "model_context_window_exceeded":
				finish = agent.FinishLength
			case "refusal":
				finish = agent.FinishBlocked
			default:
				return fail(fmt.Errorf("anthropic: unsupported stop reason %q", s))
			}
			if len(e.Usage) > 0 {
				if err := json.Unmarshal(e.Usage, &tokens); err != nil {
					return fail(fmt.Errorf("anthropic: invalid usage: %w", err))
				}
			}
		case "message_stop":
			if !started || finish == "" {
				return fail(errors.New("anthropic: incomplete message termination"))
			}
			for _, b := range blocks {
				if !b.closed && finish != agent.FinishLength && finish != agent.FinishBlocked {
					return fail(errors.New("anthropic: message stopped with open content block"))
				}
			}
			content := make([]map[string]json.RawMessage, len(blocks))
			for i, b := range blocks {
				content[i] = b.fields
			}
			usage := agent.Usage{
				InputTokens:     tokens.InputTokens + tokens.CacheCreationInputTokens + tokens.CacheReadInputTokens,
				OutputTokens:    tokens.OutputTokens,
				ReasoningTokens: tokens.OutputTokensDetails.ThinkingTokens,
			}
			usage.TotalTokens = usage.InputTokens + usage.OutputTokens
			if finish != agent.FinishLength && finish != agent.FinishBlocked {
				for _, b := range blocks {
					if b.toolIndex >= 0 && !object(string(b.fields["input"])) {
						return fail(errors.New("anthropic: incomplete or non-object tool arguments"))
					}
				}
			}
			encoded, err := json.Marshal(content)
			if err != nil {
				if finish == agent.FinishLength || finish == agent.FinishBlocked {
					yield(agent.Delta{Usage: usage, FinishReason: finish}, nil)
					return false
				}
				return fail(err)
			}
			yield(agent.Delta{Usage: usage, FinishReason: finish, Continuation: &agent.Continuation{Protocol: "anthropic", Data: encoded}}, nil)
			return false

		case "error":
			return fail(fmt.Errorf("anthropic: stream error: %s", e.Error))
		case "ping":
		default: // New top-level informational event types may be added by the API.
		}
		return true
	}
	ended, err := sse.Read(ctx, r, handle)
	if err != nil {
		fail(err)
	} else if !ended {
		fail(io.ErrUnexpectedEOF)
	}
}

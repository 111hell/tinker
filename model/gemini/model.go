// Package gemini adapts the Gemini Developer API streamGenerateContent protocol.
package gemini

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
	"strings"

	"github.com/111hell/tinker/agent"
	"github.com/111hell/tinker/internal/sse"
)

type ThinkingLevel string

const (
	ThinkingMinimal ThinkingLevel = "MINIMAL"
	ThinkingLow     ThinkingLevel = "LOW"
	ThinkingMedium  ThinkingLevel = "MEDIUM"
	ThinkingHigh    ThinkingLevel = "HIGH"
)

// ThinkingConfig keeps token budgets distinct from model-dependent depth levels.
// IncludeThoughts controls visible summaries independently from either control.
type ThinkingConfig struct {
	Budget          *int64        `json:"thinkingBudget,omitempty"`
	Level           ThinkingLevel `json:"thinkingLevel,omitempty"`
	IncludeThoughts *bool         `json:"includeThoughts,omitempty"`
}

type Config struct {
	APIKey string
	// BaseURL is the API version root, default https://generativelanguage.googleapis.com/v1beta.
	BaseURL    string
	Model      string
	HTTPClient *http.Client
	Thinking   *ThinkingConfig
}

type Model struct {
	endpoint, key string
	client        *http.Client
	thinking      *ThinkingConfig
}

var _ agent.Model = (*Model)(nil)

func New(c Config) (*Model, error) {
	if c.APIKey == "" || c.Model == "" {
		return nil, errors.New("gemini: APIKey and Model are required")
	}
	if c.Thinking != nil {
		t := c.Thinking
		if t.Budget != nil && (*t.Budget < -1 || t.Level != "") {
			return nil, errors.New("gemini: thinking budget must be >= -1 and cannot be combined with level")
		}
		switch t.Level {
		case "", ThinkingMinimal, ThinkingLow, ThinkingMedium, ThinkingHigh:
		default:
			return nil, fmt.Errorf("gemini: unsupported thinking level %q", t.Level)
		}
	}
	root := c.BaseURL
	if root == "" {
		root = "https://generativelanguage.googleapis.com/v1beta"
	}
	u, err := url.Parse(root)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("gemini: invalid BaseURL")
	}
	name := strings.TrimPrefix(c.Model, "models/")
	if strings.ContainsAny(name, "/?#") || name == "" {
		return nil, errors.New("gemini: invalid Model")
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	m := &Model{endpoint: strings.TrimRight(root, "/") + "/models/" + url.PathEscape(name) + ":streamGenerateContent?alt=sse", key: c.APIKey, client: client}
	if c.Thinking != nil {
		t := *c.Thinking
		if t.Budget != nil {
			b := *t.Budget
			t.Budget = &b
		}
		if t.IncludeThoughts != nil {
			b := *t.IncludeThoughts
			t.IncludeThoughts = &b
		}
		m.thinking = &t
	}
	return m, nil
}

type content struct {
	Role  string            `json:"role,omitempty"`
	Parts []json.RawMessage `json:"parts"`
}

func appendPart(c *content, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("gemini: encode part: %w", err)
	}
	c.Parts = append(c.Parts, b)
	return nil
}

func (m *Model) params(r agent.Request) ([]byte, error) {
	body := map[string]any{}
	if r.SystemPrompt != "" {
		body["systemInstruction"] = map[string]any{"parts": []map[string]string{{"text": r.SystemPrompt}}}
	}
	contents := []content{}
	calls := map[string]agent.ToolCall{}
	// Native IDs may be absent. Local IDs only join tool results to their names;
	// they must not be sent back as if they were server-provided IDs.
	nativeIDs := map[string]string{}
	for _, msg := range r.Messages {
		if msg.Continuation != nil && (msg.Continuation.Protocol != "gemini" || msg.Role != agent.AssistantRole) {
			return nil, errors.New("gemini: unsupported continuation protocol or role")
		}
		if msg.Role != agent.ToolRole && msg.ToolCallID != "" {
			return nil, errors.New("gemini: ToolCallID requires tool role")
		}
		if msg.Role != agent.UserRole && len(msg.ImageURLs) > 0 {
			return nil, errors.New("gemini: images require user role")
		}
		if msg.Role != agent.AssistantRole && (msg.Reasoning != "" || len(msg.ToolCalls) > 0) {
			return nil, errors.New("gemini: reasoning and tool calls require assistant role")
		}
		var c content
		switch msg.Role {
		case agent.UserRole:
			c.Role = "user"
			if msg.Text != "" {
				if err := appendPart(&c, map[string]string{"text": msg.Text}); err != nil {
					return nil, err
				}
			}
			for _, image := range msg.ImageURLs {
				header, data, ok := strings.Cut(image, ",")
				if !ok || !strings.HasPrefix(header, "data:image/") || !strings.HasSuffix(header, ";base64") {
					return nil, errors.New("gemini: ImageURLs require base64 image data URLs; remote/file URLs lack a supported image MIME contract")
				}
				if _, err := base64.StdEncoding.DecodeString(data); err != nil {
					return nil, fmt.Errorf("gemini: invalid image base64: %w", err)
				}
				if err := appendPart(&c, map[string]any{"inlineData": map[string]any{"mimeType": strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64"), "data": data}}); err != nil {
					return nil, err
				}
			}
		case agent.AssistantRole:
			c.Role = "model"
			if msg.Continuation != nil {
				if err := json.Unmarshal(msg.Continuation.Data, &c.Parts); err != nil || len(c.Parts) == 0 {
					return nil, errors.New("gemini: invalid continuation parts")
				}
				var nativeCalls []struct {
					ID   string          `json:"id"`
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				}
				var nativeText, nativeReasoning strings.Builder
				for _, p := range c.Parts {
					var part struct {
						Text         string `json:"text"`
						Thought      bool   `json:"thought"`
						FunctionCall *struct {
							ID   string          `json:"id"`
							Name string          `json:"name"`
							Args json.RawMessage `json:"args"`
						} `json:"functionCall"`
					}
					if err := json.Unmarshal(p, &part); err != nil {
						return nil, fmt.Errorf("gemini: continuation part: %w", err)
					}
					if part.Thought {
						nativeReasoning.WriteString(part.Text)
					} else {
						nativeText.WriteString(part.Text)
					}
					if part.FunctionCall != nil {
						nativeCalls = append(nativeCalls, *part.FunctionCall)
					}
				}
				if nativeText.String() != msg.Text || nativeReasoning.String() != msg.Reasoning {
					return nil, errors.New("gemini: continuation text differs from message")
				}
				if len(nativeCalls) != len(msg.ToolCalls) {
					return nil, errors.New("gemini: continuation tool calls differ from message")
				}
				for i, call := range msg.ToolCalls {
					var gotArgs, wantArgs bytes.Buffer
					nativeArgs := nativeCalls[i].Args
					if len(nativeArgs) == 0 {
						nativeArgs = json.RawMessage(`{}`)
					}
					if err := json.Compact(&gotArgs, nativeArgs); err != nil {
						return nil, fmt.Errorf("gemini: continuation arguments: %w", err)
					}
					if err := json.Compact(&wantArgs, []byte(call.Arguments)); err != nil {
						return nil, fmt.Errorf("gemini: tool arguments: %w", err)
					}
					if call.ID != fmt.Sprintf("gemini_call_%d", i) || nativeCalls[i].Name != call.Name || !bytes.Equal(gotArgs.Bytes(), wantArgs.Bytes()) {
						return nil, errors.New("gemini: continuation tool call differs from message")
					}
					nativeIDs[call.ID] = nativeCalls[i].ID
				}
			} else {
				if msg.Reasoning != "" {
					return nil, errors.New("gemini: assistant reasoning requires native continuation")
				}
				if msg.Text != "" {
					if err := appendPart(&c, map[string]string{"text": msg.Text}); err != nil {
						return nil, err
					}
				}
				for _, call := range msg.ToolCalls {
					var args map[string]json.RawMessage
					if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil || args == nil {
						return nil, errors.New("gemini: tool arguments must be a JSON object")
					}
					if err := appendPart(&c, map[string]any{"functionCall": map[string]any{"id": call.ID, "name": call.Name, "args": args}}); err != nil {
						return nil, err
					}
					nativeIDs[call.ID] = call.ID
				}
			}
			for _, call := range msg.ToolCalls {
				if call.ID == "" || call.Name == "" {
					return nil, errors.New("gemini: tool call requires ID and name")
				}
				calls[call.ID] = call
			}
		case agent.ToolRole:
			call, ok := calls[msg.ToolCallID]
			if !ok {
				return nil, fmt.Errorf("gemini: unmatched tool result %q", msg.ToolCallID)
			}
			response := map[string]any{"name": call.Name, "response": map[string]any{"result": msg.Text}}
			if nativeIDs[msg.ToolCallID] != "" {
				response["id"] = nativeIDs[msg.ToolCallID]
			}
			c.Role = "user"
			if err := appendPart(&c, map[string]any{"functionResponse": response}); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("gemini: unsupported role %q", msg.Role)
		}
		if len(c.Parts) == 0 {
			return nil, errors.New("gemini: message has no supported parts")
		}
		if msg.Role == agent.ToolRole && len(contents) > 0 && contents[len(contents)-1].Role == "user" {
			contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, c.Parts...)
		} else {
			contents = append(contents, c)
		}
	}
	body["contents"] = contents
	if len(r.Tools) > 0 {
		defs := []any{}
		for _, t := range r.Tools {
			var schema map[string]json.RawMessage
			if err := json.Unmarshal([]byte(t.InputSchema), &schema); err != nil || schema == nil {
				return nil, fmt.Errorf("gemini: tool %q schema must be a JSON object", t.Name)
			}
			defs = append(defs, map[string]any{"name": t.Name, "description": t.Description, "parametersJsonSchema": schema})
		}
		body["tools"] = []any{map[string]any{"functionDeclarations": defs}}
	}
	if m.thinking != nil {
		body["generationConfig"] = map[string]any{"thinkingConfig": m.thinking}
	}
	return json.Marshal(body)
}

func (m *Model) Stream(ctx context.Context, r agent.Request) iter.Seq2[agent.Delta, error] {
	return func(yield func(agent.Delta, error) bool) {
		body, err := m.params(r)
		if err != nil {
			yield(agent.Delta{}, err)
			return
		}
		var parts []json.RawMessage
		callIndex := 0
		var usage agent.Usage
		for {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(body))
			if err != nil {
				yield(agent.Delta{}, err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-goog-api-key", m.key)
			resp, err := m.client.Do(req)
			if err != nil {
				yield(agent.Delta{}, err)
				return
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				b, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				if readErr != nil {
					yield(agent.Delta{}, readErr)
					return
				}
				yield(agent.Delta{}, fmt.Errorf("gemini: HTTP %d: %s", resp.StatusCode, b))
				return
			}
			var finish agent.FinishReason
			var continuationToken string
			var continuing bool
			var hopUsage agent.Usage
			handle := func(data string) bool {
				var chunk struct {
					Error      json.RawMessage `json:"error"`
					Candidates []struct {
						Index             int     `json:"index"`
						Content           content `json:"content"`
						FinishReason      string  `json:"finishReason"`
						ContinuationToken string  `json:"continuationToken"`
					} `json:"candidates"`
					PromptFeedback struct {
						BlockReason string `json:"blockReason"`
					} `json:"promptFeedback"`
					Usage *struct {
						Input    int64 `json:"promptTokenCount"`
						Output   int64 `json:"candidatesTokenCount"`
						Thinking int64 `json:"thoughtsTokenCount"`
						Total    int64 `json:"totalTokenCount"`
					} `json:"usageMetadata"`
				}
				if err := json.Unmarshal([]byte(data), &chunk); err != nil {
					yield(agent.Delta{}, fmt.Errorf("gemini: decode stream: %w", err))
					return false
				}
				if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
					yield(agent.Delta{}, fmt.Errorf("gemini: API error: %s", chunk.Error))
					return false
				}
				if chunk.Usage != nil {
					u := chunk.Usage
					hopUsage = agent.Usage{InputTokens: u.Input, OutputTokens: u.Output, ReasoningTokens: u.Thinking, TotalTokens: u.Total}
				}
				if chunk.PromptFeedback.BlockReason != "" {
					finish = agent.FinishBlocked
				}
				for _, candidate := range chunk.Candidates {
					if candidate.Index != 0 {
						continue
					}
					if candidate.ContinuationToken != "" {
						continuationToken = candidate.ContinuationToken
					}
					for _, p := range candidate.Content.Parts {
						var part struct {
							Text    string `json:"text"`
							Thought bool   `json:"thought"`
							Call    *struct {
								Name string          `json:"name"`
								Args json.RawMessage `json:"args"`
							} `json:"functionCall"`
						}
						if err := json.Unmarshal(p, &part); err != nil {
							yield(agent.Delta{}, fmt.Errorf("gemini: decode part: %w", err))
							return false
						}
						var fields map[string]json.RawMessage
						if err := json.Unmarshal(p, &fields); err != nil {
							yield(agent.Delta{}, err)
							return false
						}
						for key := range fields {
							switch key {
							case "text", "thought", "thoughtSignature", "functionCall", "partMetadata":
							default:
								yield(agent.Delta{}, fmt.Errorf("gemini: unsupported response part %q", key))
								return false
							}
						}
						parts = append(parts, append(json.RawMessage(nil), p...))
						delta := agent.Delta{}
						if part.Thought {
							delta.Reasoning = part.Text
						} else {
							delta.Text = part.Text
						}
						if (delta.Text != "" || delta.Reasoning != "") && !yield(delta, nil) {
							return false
						}
						if part.Call != nil {
							call := part.Call
							var args map[string]json.RawMessage
							if len(call.Args) == 0 {
								call.Args = json.RawMessage(`{}`)
							}
							if call.Name == "" || json.Unmarshal(call.Args, &args) != nil || args == nil {
								yield(agent.Delta{}, errors.New("gemini: invalid function call"))
								return false
							}
							// Native IDs live only in the raw continuation. A separate local
							// namespace joins results without collisions with native IDs.
							id := fmt.Sprintf("gemini_call_%d", callIndex)
							if !yield(agent.Delta{ToolCall: &agent.ToolCallDelta{Index: callIndex, ID: id, Name: call.Name, ArgumentsDelta: string(call.Args)}}, nil) {
								return false
							}
							callIndex++
						}
					}
					if candidate.FinishReason != "" {
						continuing = false
						switch candidate.FinishReason {
						case "STOP":
							finish = agent.FinishStop
							if callIndex > 0 {
								finish = agent.FinishToolCalls
							}
						case "CONTINUATION":
							continuing = true
						case "MAX_TOKENS":
							finish = agent.FinishLength
						case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT":
							finish = agent.FinishBlocked
						default:
							yield(agent.Delta{}, fmt.Errorf("gemini: unsupported finish reason %q", candidate.FinishReason))
							return false
						}
					}
				}
				if chunk.Usage != nil {
					snapshot := agent.Usage{
						InputTokens:     usage.InputTokens + hopUsage.InputTokens,
						OutputTokens:    usage.OutputTokens + hopUsage.OutputTokens,
						ReasoningTokens: usage.ReasoningTokens + hopUsage.ReasoningTokens,
						TotalTokens:     usage.TotalTokens + hopUsage.TotalTokens,
					}
					if !yield(agent.Delta{Usage: snapshot}, nil) {
						return false
					}
				}
				return true
			}
			stopped, err := sse.Read(ctx, resp.Body, handle)
			resp.Body.Close()
			if err != nil {
				yield(agent.Delta{}, err)
				return
			}
			if stopped {
				return
			}
			usage.InputTokens += hopUsage.InputTokens
			usage.OutputTokens += hopUsage.OutputTokens
			usage.ReasoningTokens += hopUsage.ReasoningTokens
			usage.TotalTokens += hopUsage.TotalTokens
			if continuing && finish == "" {
				if continuationToken == "" {
					yield(agent.Delta{}, errors.New("gemini: CONTINUATION requires continuationToken"))
					return
				}
				// The token resumes generation against the identical original prompt;
				// never append the partial assistant content to this request.
				var request map[string]json.RawMessage
				if err := json.Unmarshal(body, &request); err != nil {
					yield(agent.Delta{}, err)
					return
				}
				token, err := json.Marshal(continuationToken)
				if err != nil {
					yield(agent.Delta{}, err)
					return
				}
				request["continuationToken"] = token
				body, err = json.Marshal(request)
				if err != nil {
					yield(agent.Delta{}, err)
					return
				}
				continue
			}
			if finish == "" {
				yield(agent.Delta{}, errors.New("gemini: stream ended without finish reason"))
				return
			}
			delta := agent.Delta{FinishReason: finish, Usage: usage}
			if len(parts) > 0 {
				data, err := json.Marshal(parts)
				if err != nil {
					yield(agent.Delta{}, fmt.Errorf("gemini: encode continuation: %w", err))
					return
				}
				delta.Continuation = &agent.Continuation{Protocol: "gemini", Data: data}
			}
			yield(delta, nil)
			return
		}
	}
}

package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/111hell/tinker/agent"
)

func TestHTTPRoundTrip(t *testing.T) {
	for _, effort := range []string{"", "high"} {
		t.Run(effort, func(t *testing.T) {
			count := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count++
				if r.URL.Path != "/chat/completions" {
					t.Errorf("path: %s", r.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if effort == "" {
					if _, ok := body["reasoning_effort"]; ok {
						t.Error("default effort sent")
					}
				} else if body["reasoning_effort"] != effort {
					t.Error("missing effort")
				}
				messages := body["messages"].([]any)
				if messages[0].(map[string]any)["content"] != "system" {
					t.Error("missing system")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if count == 1 {
					if body["tools"] == nil {
						t.Error("missing tools")
					}
					if _, ok := messages[1].(map[string]any)["content"].([]any); !ok {
						t.Error("missing image")
					}
					fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"reasoning_content":"think","tool_calls":[{"index":0,"id":"call_","function":{"name":"ec","arguments":"{\"x\":"}},{"index":1,"id":"two","function":{"name":"echo","arguments":"{}"}}]}}]}`+"\n\n")
					fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"one","function":{"name":"ho","arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
				} else {
					if len(messages) != 5 {
						t.Errorf("messages: %#v", messages)
					}
					assistant := messages[2].(map[string]any)
					if assistant["reasoning_content"] != "think" {
						t.Error("reasoning lost")
					}
					calls := assistant["tool_calls"].([]any)
					if calls[0].(map[string]any)["id"] != "call_one" {
						t.Error("id fragments lost")
					}
					if messages[3].(map[string]any)["tool_call_id"] != "call_one" {
						t.Error("result id lost")
					}
					fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n")
				}
				fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7,"completion_tokens_details":{"reasoning_tokens":2}}}`+"\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			config := Config{APIKey: "key", BaseURL: server.URL, Model: "test"}
			config.ReasoningEffort = ""
			if effort != "" {
				config.ReasoningEffort = "high"
			}
			model, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			executions := 0
			a, err := agent.New(agent.Config{Model: model, SystemPrompt: "system", Tools: []agent.Tool{{Name: "echo", Description: "echo", InputSchema: `{"type":"object"}`, Execute: func(context.Context, string) (string, error) { executions++; return "ok", nil }}}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
			defer cancel()
			completed := false
			for event, err := range a.RunMessage(ctx, nil, agent.UserImageMessage("hello", "https://example.com/a.png")) {
				if err != nil {
					t.Fatal(err)
				}
				if event.Type == agent.RunCompleted {
					completed = true
					if event.Message.Text != "done" || event.Usage.TotalTokens != 14 || event.Message.FinishReason != agent.FinishStop {
						t.Fatalf("bad completion: %#v", event)
					}
				}
			}
			if !completed || count != 2 || executions != 2 {
				t.Fatalf("completed=%v requests=%d executions=%d", completed, count, executions)
			}
		})
	}
}

func TestFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       string
	}{
		{"EOF", `data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n", 200, "without finish"},
		{"error", `{"error":{"message":"original failure","type":"invalid_request_error"}}`, 400, "original failure"},
		{"blocked", `data: {"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}` + "\n\ndata: [DONE]\n\n", 200, "blocked"},
		{"length", `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"echo","arguments":"{}"}}]},"finish_reason":"length"}]}` + "\n\ndata: [DONE]\n\n", 200, "truncated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			m, err := New(Config{APIKey: "k", Model: "m", BaseURL: s.URL})
			if err != nil {
				t.Fatal(err)
			}
			called := false
			a, newErr := agent.New(agent.Config{Model: m, Tools: []agent.Tool{{Name: "echo", Description: "echo", InputSchema: `{}`, Execute: func(context.Context, string) (string, error) { called = true; return "", nil }}}})
			if newErr != nil {
				t.Fatal(newErr)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
			defer cancel()
			var got error
			for _, err := range a.Run(ctx, nil, "hi") {
				if err != nil {
					got = err
				}
			}
			if got == nil || !strings.Contains(got.Error(), tc.want) || called {
				t.Fatalf("err=%v called=%v", got, called)
			}
		})
	}
	m, _ := New(Config{APIKey: "k", Model: "m"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range m.Stream(ctx, agent.Request{}) {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	}
	if _, err := New(Config{APIKey: "k", Model: "m", ReasoningEffort: "bogus"}); err == nil {
		t.Fatal("accepted effort")
	}
	for _, msg := range []agent.Message{{Role: agent.AssistantRole, ImageURLs: []string{"a"}}, {Role: agent.UserRole, Reasoning: "x"}, {Role: agent.AssistantRole, Continuation: &agent.Continuation{Protocol: "other"}}} {
		if _, err := messageParam(msg); err == nil {
			t.Fatalf("accepted %#v", msg)
		}
	}
}

func TestCancellationDuringStream(t *testing.T) {
	started := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer s.Close()
	m, err := New(Config{APIKey: "k", Model: "m", BaseURL: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()
	var got error
	for d, err := range m.Stream(ctx, agent.Request{Messages: []agent.Message{agent.UserMessage("hi")}}) {
		if d.Text != "" {
			<-started
			cancel()
		}
		if err != nil {
			got = err
		}
	}
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("cancel: %v", got)
	}
}

func TestToolSchemaPreservesNumbers(t *testing.T) {
	const schema = `{"type":"object","properties":{"id":{"const":9007199254740993,"enum":[9007199254740993]},"amount":{"const":0.12345678901234567890123456789},"large":{"const":1e400}}}`
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []struct {
				Function struct {
					Parameters json.RawMessage `json:"parameters"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if len(body.Tools) != 1 {
			t.Errorf("tools: %+v", body)
			return
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(body.Tools[0].Function.Parameters, &got); err != nil {
			t.Error(err)
			return
		}
		var want map[string]json.RawMessage
		if err := json.Unmarshal([]byte(schema), &want); err != nil {
			t.Error(err)
			return
		}
		if string(got["properties"]) != string(want["properties"]) {
			t.Errorf("parameters changed: %s", body.Tools[0].Function.Parameters)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer s.Close()
	m, err := New(Config{APIKey: "k", Model: "m", BaseURL: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range m.Stream(context.Background(), agent.Request{Tools: []agent.Tool{{Name: "schema", InputSchema: schema}}}) {
		if err != nil {
			t.Fatal(err)
		}
	}
}

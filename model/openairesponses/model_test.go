package openairesponses

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

func event(w http.ResponseWriter, v any) { b, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", b) }
func terminal(w http.ResponseWriter, output []any) {
	event(w, map[string]any{"type": "response.completed", "response": map[string]any{"output": output, "usage": map[string]any{"input_tokens": 12, "output_tokens": 8, "total_tokens": 20, "output_tokens_details": map[string]any{"reasoning_tokens": 3}}}})
}
func newModel(t *testing.T, handler http.HandlerFunc, reasoning Reasoning) *Model {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	m, err := New(Config{APIKey: "test", Model: "test-model", BaseURL: s.URL + "/v1", Reasoning: reasoning})
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func TestToolRoundTrip(t *testing.T) {
	requests := 0
	executed := 0
	reasoning := map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{map[string]any{"type": "summary_text", "text": "thinking"}}, "encrypted_content": "opaque"}
	call := func(id, name, args string) map[string]any {
		return map[string]any{"type": "function_call", "id": "fc_" + id, "call_id": id, "name": name, "arguments": args, "status": "completed"}
	}
	m := newModel(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer test" {
			t.Errorf("request route/auth")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if string(body["store"]) != "false" || string(body["include"]) != `["reasoning.encrypted_content"]` {
			t.Error("missing stateless request fields")
		}
		if string(body["reasoning"]) != `{"effort":"high","summary":"concise"}` {
			t.Errorf("reasoning: %s", body["reasoning"])
		}
		if string(body["instructions"]) != `"system"` {
			t.Error("system omitted")
		}
		var input []map[string]any
		json.Unmarshal(body["input"], &input)
		if requests == 1 {
			if len(input) != 1 || len(input[0]["content"].([]any)) != 2 {
				t.Error("image/text input missing")
			}
			var tools []map[string]any
			json.Unmarshal(body["tools"], &tools)
			if len(tools) != 1 || tools[0]["strict"] != false {
				t.Error("tool schema/strict missing")
			}
			event(w, map[string]any{"type": "response.reasoning_summary_text.delta", "delta": "thinking"})
			event(w, map[string]any{"type": "response.output_item.added", "output_index": 0, "item": reasoning})
			for i, id := range []string{"a", "b"} {
				item := call(id, "lookup", "")
				event(w, map[string]any{"type": "response.output_item.added", "output_index": i + 1, "item": item})
			}
			for _, part := range []string{`{"x":`, `1}`} {
				for i := 1; i <= 2; i++ {
					event(w, map[string]any{"type": "response.function_call_arguments.delta", "output_index": i, "delta": part})
				}
			}
			terminal(w, []any{reasoning, call("a", "lookup", `{"x":1}`), call("b", "lookup", `{"x":1}`)})
		} else {
			if len(input) != 6 || input[1]["encrypted_content"] != "opaque" || input[2]["call_id"] != "a" || input[3]["call_id"] != "b" || input[4]["type"] != "function_call_output" || input[5]["call_id"] != "b" {
				t.Errorf("continuation/tool results lost: %+v", input)
			}
			event(w, map[string]any{"type": "response.output_text.delta", "delta": "done"})
			terminal(w, []any{map[string]any{"type": "message", "id": "msg_2", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "done", "annotations": []any{}}}}})
		}
	}, Reasoning{Effort: EffortHigh, Summary: SummaryConcise})
	a, err := agent.New(agent.Config{Model: m, SystemPrompt: "system", Tools: []agent.Tool{{Name: "lookup", Description: "lookup", InputSchema: `{"type":"object"}`, Execute: func(_ context.Context, args string) (string, error) {
		executed++
		if args != `{"x":1}` {
			t.Errorf("arguments %q", args)
		}
		return "result", nil
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var result agent.Event
	for e, err := range a.RunMessage(ctx, nil, agent.UserImageMessage("look", "https://example.com/a.png")) {
		if err != nil {
			t.Fatal(err)
		}
		result = e
	}
	if requests != 2 || executed != 2 || result.Message.Text != "done" || result.Usage.TotalTokens != 40 {
		t.Fatalf("requests=%d tools=%d result=%+v", requests, executed, result)
	}
}

func TestCompletedMustIncludeStreamedCalls(t *testing.T) {
	for _, output := range [][]any{nil, {map[string]any{"type": "message", "content": []any{}}, map[string]any{"type": "function_call", "call_id": "new", "name": "lookup", "arguments": "{}"}}} {
		m := newModel(t, func(w http.ResponseWriter, r *http.Request) {
			event(w, map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "call_id": "old", "name": "lookup", "arguments": "{}"}})
			terminal(w, output)
		}, Reasoning{})
		var gotErr error
		var finish agent.FinishReason
		for d, err := range m.Stream(context.Background(), agent.Request{}) {
			if err != nil {
				gotErr = err
			}
			finish = d.FinishReason
		}
		if gotErr == nil || finish != "" {
			t.Fatalf("missing call accepted: finish=%s err=%v", finish, gotErr)
		}
	}
}

func TestDefaultsAndSummaryIndependent(t *testing.T) {
	for _, reasoning := range []Reasoning{{}, {Summary: SummaryAuto}, {Effort: EffortNone}} {
		t.Run(fmt.Sprint(reasoning), func(t *testing.T) {
			m := newModel(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				json.NewDecoder(r.Body).Decode(&body)
				if reasoning == (Reasoning{}) {
					if _, ok := body["reasoning"]; ok {
						t.Error("default reasoning sent")
					}
				} else {
					var got Reasoning
					json.Unmarshal(body["reasoning"], &got)
					if got != reasoning {
						t.Errorf("got %+v", got)
					}
				}
				terminal(w, nil)
			}, reasoning)
			for _, err := range m.Stream(context.Background(), agent.Request{}) {
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestFailuresAndFinish(t *testing.T) {
	tests := []struct {
		name, stream, contains string
		status                 int
		finish                 agent.FinishReason
	}{
		{name: "truncated", stream: `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"},"output":[]}}`, finish: agent.FinishLength},
		{name: "blocked", stream: `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"content_filter"},"output":[]}}`, finish: agent.FinishBlocked},
		{name: "failed", stream: `{"type":"response.failed","response":{"error":{"code":"bad","message":"original failure"}}}`, contains: "original failure"},
		{name: "error", stream: `{"type":"error","message":"original error"}`, contains: "original error"},
		{name: "http", stream: `{"error":"original http"}`, status: 400, contains: "original http"},
		{name: "eof", stream: `{"type":"response.output_text.delta","delta":"partial"}`, contains: "before terminal"},
		{name: "malformed", stream: `{`, contains: "decode"},
		{name: "unsupported", stream: `{"type":"response.completed","response":{"output":[{"type":"image_generation_call"}]}}`, contains: "unsupported output"},
		{name: "bad-tool", stream: `{"type":"response.completed","response":{"output":[{"type":"function_call","call_id":"id","name":"test","arguments":"{"}]}}`, contains: "incomplete function"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel(t, func(w http.ResponseWriter, r *http.Request) {
				if tt.status != 0 {
					w.WriteHeader(tt.status)
					fmt.Fprint(w, tt.stream)
				} else {
					fmt.Fprintf(w, "data: %s\n\n", tt.stream)
				}
			}, Reasoning{})
			var gotErr error
			var finish agent.FinishReason
			for d, err := range m.Stream(context.Background(), agent.Request{}) {
				if err != nil {
					gotErr = err
				}
				if d.FinishReason != "" {
					finish = d.FinishReason
				}
			}
			if tt.contains != "" {
				if gotErr == nil || !strings.Contains(gotErr.Error(), tt.contains) {
					t.Fatalf("error=%v", gotErr)
				}
			} else if gotErr != nil || finish != tt.finish {
				t.Fatalf("finish=%s err=%v", finish, gotErr)
			}
		})
	}
}
func TestCancel(t *testing.T) {
	m := newModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}, Reasoning{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var got error
	for _, err := range m.Stream(ctx, agent.Request{}) {
		got = err
	}
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("got %v", got)
	}
}
func TestInvalidConfigurationAndInput(t *testing.T) {
	for _, c := range []Config{{}, {APIKey: "k", Model: "m", Reasoning: Reasoning{Effort: "invalid"}}, {APIKey: "k", Model: "m", Reasoning: Reasoning{Summary: "invalid"}}, {APIKey: "k", Model: "m", BaseURL: "ftp://a"}} {
		if _, err := New(c); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
	m, _ := New(Config{APIKey: "k", Model: "m"})
	for _, msg := range []agent.Message{
		{Role: agent.ToolRole, Text: "x"},
		{Role: agent.AssistantRole, Reasoning: "lost"},
		{Role: agent.AssistantRole, ImageURLs: []string{"x"}},
		{Role: agent.UserRole, ToolCalls: []agent.ToolCall{{ID: "x"}}},
		{Role: agent.AssistantRole, Continuation: &agent.Continuation{Protocol: "anthropic", Data: json.RawMessage(`[]`)}},
		{Role: agent.AssistantRole, Continuation: &agent.Continuation{Protocol: protocol, Data: json.RawMessage(`null`)}},
	} {
		if _, err := m.body(agent.Request{Messages: []agent.Message{msg}}); err == nil {
			t.Fatalf("accepted %+v", msg)
		}
	}
	for _, schema := range []string{`[]`, `null`, `"object"`, `1`, `true`, `{`} {
		if _, err := m.body(agent.Request{Tools: []agent.Tool{{Name: "t", InputSchema: schema}}}); err == nil {
			t.Fatalf("accepted non-object schema %s", schema)
		}
	}
}

func TestContinuationMustMatchMessage(t *testing.T) {
	m, err := New(Config{APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	native := json.RawMessage(`[{"type":"reasoning","id":"rs","summary":[{"type":"summary_text","text":"summary"}],"content":[{"type":"reasoning_text","text":"reason"}],"encrypted_content":"opaque"},{"type":"message","id":"msg","content":[{"type":"output_text","text":"answer"},{"type":"refusal","refusal":"refusal"}]},{"type":"function_call","call_id":"id","name":"tool","arguments":"{\"n\":9007199254740993}"}]`)
	baseline := func() agent.Message {
		return agent.Message{Role: agent.AssistantRole, Text: "answerrefusal", Reasoning: "summaryreason", ToolCalls: []agent.ToolCall{{ID: "id", Name: "tool", Arguments: `{ "n": 9007199254740993 }`}}, Continuation: &agent.Continuation{Protocol: protocol, Data: native}}
	}
	if _, err := m.body(agent.Request{Messages: []agent.Message{baseline()}}); err != nil {
		t.Fatalf("matching native history rejected: %v", err)
	}
	for _, change := range []struct {
		name   string
		mutate func(*agent.Message)
	}{
		{"text", func(m *agent.Message) { m.Text = "changed" }},
		{"reasoning", func(m *agent.Message) { m.Reasoning = "changed" }},
		{"missing calls", func(m *agent.Message) { m.ToolCalls = nil }},
		{"call id", func(m *agent.Message) { m.ToolCalls[0].ID = "changed" }},
		{"call name", func(m *agent.Message) { m.ToolCalls[0].Name = "changed" }},
		{"large integer", func(m *agent.Message) { m.ToolCalls[0].Arguments = `{"n":9007199254740992}` }},
		{"invalid args", func(m *agent.Message) { m.ToolCalls[0].Arguments = `{` }},
	} {
		t.Run(change.name, func(t *testing.T) {
			msg := baseline()
			change.mutate(&msg)
			if _, err := m.body(agent.Request{Messages: []agent.Message{msg}}); err == nil {
				t.Fatal("accepted altered history")
			}
		})
	}
}

func TestToolSchemaPreservesNumbers(t *testing.T) {
	const schema = `{"type":"object","properties":{"id":{"const":9007199254740993,"enum":[9007199254740993,1.234567890123456789,1e400]}}}`
	m := newModel(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []struct {
				Parameters json.RawMessage `json:"parameters"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		} else if len(body.Tools) != 1 || string(body.Tools[0].Parameters) != schema {
			t.Errorf("schema numbers changed: %+v", body.Tools)
		}
		terminal(w, nil)
	}, Reasoning{})
	for _, err := range m.Stream(context.Background(), agent.Request{Tools: []agent.Tool{{Name: "lookup", InputSchema: schema}}}) {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestStreamStopsAfterTerminalOrConsumer(t *testing.T) {
	for _, consumerStops := range []bool{false, true} {
		t.Run(fmt.Sprint(consumerStops), func(t *testing.T) {
			m := newModel(t, func(w http.ResponseWriter, r *http.Request) {
				// Exercise multiline data and a final frame without a trailing blank line.
				fmt.Fprint(w, "event: response.output_text.delta\ndata: {\ndata: \"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
				if consumerStops {
					fmt.Fprint(w, "data: invalid\n\n")
				} else {
					fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}")
				}
			}, Reasoning{})
			count := 0
			for d, err := range m.Stream(context.Background(), agent.Request{}) {
				if err != nil {
					t.Fatal(err)
				}
				count++
				if count == 1 && d.Text != "hello" {
					t.Fatalf("unexpected text: %+v", d)
				}
				if consumerStops {
					break
				}
			}
			if !consumerStops && count != 2 {
				t.Fatalf("got %d deltas", count)
			}
		})
	}
}

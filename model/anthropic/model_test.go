package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/111hell/tinker/agent"
)

func events(v ...string) string {
	var b strings.Builder
	for _, s := range v {
		b.WriteString("data: " + s + "\n\n")
	}
	return b.String()
}

const start = `{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":2,"output_tokens":1,"cache_creation_input_tokens":3,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":0},"service_tier":"standard","speed":"standard","output_tokens_details":{"thinking_tokens":1}}}}`

func end(reason string) string {
	return events(`{"type":"message_delta","delta":{"stop_reason":"`+reason+`"},"usage":{"output_tokens":5,"cache_creation_input_tokens":4,"service_tier":"standard","speed":"standard","output_tokens_details":{"thinking_tokens":2,"text_tokens":3}}}`, `{"type":"message_stop"}`)
}
func textStream() string {
	return events(start, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`, `{"type":"content_block_stop","index":0}`) + end("end_turn")
}
func newTestModel(t *testing.T, url string, modify func(*Config)) *Model {
	t.Helper()
	c := Config{APIKey: "test", BaseURL: url, Model: "caller-selected", MaxTokens: 4096}
	if modify != nil {
		modify(&c)
	}
	m, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	return m
}

func TestToolRoundTrip(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/messages" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("x-api-key") != "test" {
			t.Error("wrong endpoint or headers")
		}
		var p map[string]json.RawMessage
		if e := json.NewDecoder(r.Body).Decode(&p); e != nil {
			t.Error(e)
		}
		if string(p["thinking"]) != `{"type":"enabled","budget_tokens":1024,"display":"omitted"}` || string(p["output_config"]) != `{"effort":"high"}` {
			t.Errorf("thinking configuration: %s %s", p["thinking"], p["output_config"])
		}
		if string(p["system"]) != `"system"` || string(p["max_tokens"]) != "4096" {
			t.Error("missing common parameters")
		}
		if requests == 1 {
			if !strings.Contains(string(p["messages"]), `"type":"base64"`) || !strings.Contains(string(p["messages"]), `"type":"url"`) || !strings.Contains(string(p["tools"]), `"input_schema"`) {
				t.Error("missing images/tools")
			}
			io.WriteString(w, events(start,
				`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"secret"}}`,
				`{"type":"content_block_stop","index":1}`,
				`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"checking"}}`,
				`{"type":"content_block_stop","index":2}`,
				`{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"call1","name":"lookup","input":{}}}`,
				`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"x\":"}}`,
				`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"1}"}}`,
				`{"type":"content_block_stop","index":3}`,
				`{"type":"content_block_start","index":4,"content_block":{"type":"tool_use","id":"call2","name":"lookup","input":{}}}`,
				`{"type":"content_block_delta","index":4,"delta":{"type":"input_json_delta","partial_json":"{\"x\":2}"}}`,
				`{"type":"content_block_stop","index":4}`)+end("tool_use"))
		} else {
			var ms []message
			if e := json.Unmarshal(p["messages"], &ms); e != nil {
				t.Error(e)
			}
			if len(ms) != 3 || ms[1].Role != "assistant" || len(ms[1].Content) != 5 || ms[2].Role != "user" || len(ms[2].Content) != 2 {
				t.Errorf("incorrect round trip: %s", p["messages"])
			}
			if !strings.Contains(string(p["messages"]), `"signature":"opaque"`) || !strings.Contains(string(p["messages"]), `"data":"secret"`) || !strings.Contains(string(p["messages"]), `"tool_use_id":"call2"`) {
				t.Error("native continuation/tool results lost")
			}
			io.WriteString(w, textStream())
		}
	}))
	defer server.Close()
	m := newTestModel(t, server.URL, func(c *Config) {
		c.Thinking = &Thinking{Type: "enabled", BudgetTokens: 1024, Display: "omitted"}
		c.Effort = EffortHigh
	})
	calls := 0
	a, e := agent.New(agent.Config{Model: m, SystemPrompt: "system", Tools: []agent.Tool{{Name: "lookup", Description: "lookup", InputSchema: `{"type":"object"}`, Execute: func(_ context.Context, args string) (string, error) {
		calls++
		if args != fmt.Sprintf(`{"x":%d}`, calls) {
			t.Errorf("arguments %s", args)
		}
		return "result", nil
	}}}})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var result agent.Event
	for ev, e := range a.RunMessage(ctx, nil, agent.UserImageMessage("question", "https://example.com/a.png", "data:image/png;base64,YQ==")) {
		if e != nil {
			t.Fatal(e)
		}
		result = ev
	}
	if calls != 2 || requests != 2 || result.Message.Text != "done" || result.Usage.InputTokens != 32 || result.Usage.OutputTokens != 10 || result.Usage.ReasoningTokens != 4 || result.Usage.TotalTokens != 42 {
		t.Fatalf("calls=%d requests=%d result=%+v", calls, requests, result)
	}
}

func TestOptionalParametersOmitted(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		for _, k := range []string{"thinking", "output_config", "tools", "system"} {
			if _, ok := p[k]; ok {
				t.Errorf("unexpected optional %s", k)
			}
		}
		io.WriteString(w, textStream())
	}))
	defer s.Close()
	m := newTestModel(t, s.URL, nil)
	var finish agent.FinishReason
	for d, e := range m.Stream(context.Background(), agent.Request{Messages: []agent.Message{agent.UserMessage("hi")}}) {
		if e != nil {
			t.Fatal(e)
		}
		if d.FinishReason != "" {
			finish = d.FinishReason
		}
	}
	if finish != agent.FinishStop {
		t.Fatal(finish)
	}
}

func TestValidation(t *testing.T) {
	for _, c := range []Config{
		{APIKey: "x", Model: "m"}, {APIKey: "x", Model: "m", MaxTokens: 10, Effort: "unknown"},
		{APIKey: "x", Model: "m", MaxTokens: 4096, Thinking: &Thinking{Type: "enabled", BudgetTokens: 100}},
		{APIKey: "x", Model: "m", MaxTokens: 4096, Thinking: &Thinking{Type: "adaptive", BudgetTokens: 1024}},
		{APIKey: "x", Model: "m", MaxTokens: 4096, Thinking: &Thinking{Type: "disabled", Display: "omitted"}},
	} {
		if _, e := New(c); e == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
	m := newTestModel(t, "http://example.com", nil)
	for _, v := range []agent.Message{
		{Role: agent.AssistantRole, Reasoning: "no signature"},
		{Role: agent.AssistantRole, ImageURLs: []string{"https://example.com/a"}},
		{Role: agent.ToolRole, Text: "missing ID"},
		{Role: agent.UserRole, ImageURLs: []string{"file:///tmp/image.png"}},
		{Role: agent.AssistantRole, Continuation: &agent.Continuation{Protocol: "responses", Data: json.RawMessage(`[]`)}},
	} {
		if _, e := m.params(agent.Request{Messages: []agent.Message{v}}); e == nil {
			t.Errorf("accepted %+v", v)
		}
	}
}

func TestStreamFailures(t *testing.T) {
	for _, tt := range []struct {
		name, stream, want string
		finish             agent.FinishReason
	}{
		{"EOF", events(start), "unexpected EOF", ""},
		{"error", events(`{"type":"error","error":{"type":"overloaded_error","message":"original error"}}`), "original error", ""},
		{"length", events(start) + end("max_tokens"), "", agent.FinishLength},
		{"blocked", events(start) + end("refusal"), "", agent.FinishBlocked},
		{"partial length", events(start, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"a","name":"lookup","input":{}}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}`, `{"type":"content_block_stop","index":0}`) + end("max_tokens"), "", agent.FinishLength},
		{"bad json", events(`oops`), "invalid stream event", ""},
		{"bad initial usage", events(`{"type":"message_start","message":{"usage":{"input_tokens":"10"}}}`), "invalid usage", ""},
		{"bad usage update", events(start, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":{}}}`), "invalid usage", ""},
		{"open block", events(start, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) + end("end_turn"), "open content block", ""},
		{"invalid arguments", events(start, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"a","name":"lookup","input":{}}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}`, `{"type":"content_block_stop","index":0}`) + end("tool_use"), "incomplete", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tt.stream) }))
			defer s.Close()
			m := newTestModel(t, s.URL, nil)
			var got error
			var finish agent.FinishReason
			for d, e := range m.Stream(context.Background(), agent.Request{}) {
				if e != nil {
					got = e
				}
				if d.FinishReason != "" {
					finish = d.FinishReason
				}
			}
			if (tt.want == "" && got != nil) || (tt.want != "" && (got == nil || !strings.Contains(got.Error(), tt.want))) || finish != tt.finish {
				t.Fatalf("error=%v finish=%q", got, finish)
			}
		})
	}
}

func TestHTTPErrorAndCancellation(t *testing.T) {
	t.Run("http", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"unsupported model option"}`, 400)
		}))
		defer s.Close()
		m := newTestModel(t, s.URL, nil)
		for _, e := range m.Stream(context.Background(), agent.Request{}) {
			if e == nil || !strings.Contains(e.Error(), "unsupported model option") {
				t.Fatal(e)
			}
		}
	})
	t.Run("cancel", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("content-type", "text/event-stream")
			io.WriteString(w, events(start))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer s.Close()
		m := newTestModel(t, s.URL, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		var got error
		for _, e := range m.Stream(ctx, agent.Request{}) {
			got = e
		}
		if !errors.Is(got, context.DeadlineExceeded) {
			t.Fatalf("%v", got)
		}
	})
}

func TestUsageSnapshots(t *testing.T) {
	for _, tt := range []struct {
		name, delta string
		want        agent.Usage
	}{
		{"partial update", `{"output_tokens":9,"input_tokens":11,"cache_read_input_tokens":5,"output_tokens_details":{"text_tokens":8},"service_tier":"standard"}`, agent.Usage{InputTokens: 19, OutputTokens: 9, ReasoningTokens: 1, TotalTokens: 28}},
		{"explicit zero", `{"output_tokens":0,"input_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens_details":{"thinking_tokens":0}}`, agent.Usage{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, events(start, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":`+tt.delta+`}`, `{"type":"message_stop"}`))
			}))
			defer s.Close()
			m := newTestModel(t, s.URL, nil)
			var got agent.Delta
			for d, err := range m.Stream(context.Background(), agent.Request{}) {
				if err != nil {
					t.Fatal(err)
				}
				got = d
			}
			if got.Usage != tt.want || got.FinishReason != agent.FinishStop {
				t.Fatalf("got %+v; want %+v", got, tt.want)
			}
		})
	}
}

func TestContinuationToolArgumentsPrecision(t *testing.T) {
	m := newTestModel(t, "http://example.com", nil)
	native := json.RawMessage(`[{"type":"tool_use","id":"id","name":"lookup","input":{"id":9007199254740993,"nested":[1,{"ok":true}]}}]`)
	for _, tt := range []struct {
		args  string
		valid bool
	}{
		{`{ "nested": [1, {"ok": true}], "id": 9007199254740993 }`, true},
		{`{"id":9007199254740992,"nested":[1,{"ok":true}]}`, false},
		{`{"id":9007199254740993,"nested":[1,{"ok":true}]} {}`, false},
		{`null`, false},
		{`[]`, false},
	} {
		v := agent.Message{Role: agent.AssistantRole, ToolCalls: []agent.ToolCall{{ID: "id", Name: "lookup", Arguments: tt.args}}, Continuation: &agent.Continuation{Protocol: "anthropic", Data: native}}
		p, err := m.params(agent.Request{Messages: []agent.Message{v}})
		if (err == nil) != tt.valid {
			t.Fatalf("args %s: error %v", tt.args, err)
		}
		if err == nil {
			encoded, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(encoded), "9007199254740993") {
				t.Fatalf("precision lost: %s", encoded)
			}
		}
	}
}

func TestStreamReaderTermination(t *testing.T) {
	for _, tt := range []struct {
		name, stream   string
		stopAfterFirst bool
		wantFinish     agent.FinishReason
	}{
		{"terminal without trailing separator", strings.TrimRight(textStream(), "\n"), false, agent.FinishStop},
		{"ignore data after terminal", textStream() + events(`invalid`), false, agent.FinishStop},
		{"consumer stops", events(start, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}`) + events(`invalid`), true, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestModel(t, "http://example.com", nil)
			var finish agent.FinishReason
			count := 0
			m.read(context.Background(), strings.NewReader(tt.stream), func(d agent.Delta, err error) bool {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				count++
				finish = d.FinishReason
				return !tt.stopAfterFirst
			})
			if finish != tt.wantFinish || count == 0 {
				t.Fatalf("count=%d finish=%q", count, finish)
			}
			if tt.stopAfterFirst && count != 1 {
				t.Fatalf("yield called %d times after stop", count)
			}
		})
	}
}

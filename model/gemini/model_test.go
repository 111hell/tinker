package gemini

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

func newTestModel(t *testing.T, handler http.HandlerFunc, thinking *ThinkingConfig) *Model {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	m, err := New(Config{APIKey: "key", Model: "test-model", BaseURL: s.URL, Thinking: thinking})
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func collect(m *Model, ctx context.Context, r agent.Request) ([]agent.Delta, error) {
	var ds []agent.Delta
	for d, err := range m.Stream(ctx, r) {
		if err != nil {
			return ds, err
		}
		ds = append(ds, d)
	}
	return ds, nil
}
func event(w http.ResponseWriter, s string) { fmt.Fprintf(w, "data: %s\n\n", s) }

func TestStreamAndContinuation(t *testing.T) {
	count := 0
	m := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models/test-model:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" || r.Header.Get("x-goog-api-key") != "key" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if _, ok := body["generationConfig"]; ok {
			t.Error("default thinking must be omitted")
		}
		var contents []content
		if err := json.Unmarshal(body["contents"], &contents); err != nil {
			t.Error(err)
		}
		count++
		if count == 1 {
			if !strings.Contains(string(body["systemInstruction"]), "system") || !strings.Contains(string(body["tools"]), "parametersJsonSchema") || !strings.Contains(string(body["contents"]), "inlineData") {
				t.Errorf("incomplete request: %s", body)
			}
			event(w, `{"candidates":[{"content":{"parts":[{"text":"think","thought":true}]}}]}`)
			event(w, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"one"}},"thoughtSignature":"c2ln"},{"functionCall":{"id":"native","name":"lookup","args":{"q":"two"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"thoughtsTokenCount":4,"totalTokenCount":9}}`)
		} else {
			if len(contents) != 3 {
				t.Errorf("contents: %s", body["contents"])
			}
			if !strings.Contains(string(contents[1].Parts[1]), `"thoughtSignature":"c2ln"`) {
				t.Error("signature lost")
			}
			if len(contents[2].Parts) != 2 {
				t.Error("parallel results not grouped")
			}
			if strings.Contains(string(contents[2].Parts[0]), "gemini_call_") {
				t.Error("local ID sent as native")
			}
			if !strings.Contains(string(contents[2].Parts[1]), `"id":"native"`) {
				t.Error("native ID lost")
			}
			event(w, `{"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}]}`)
		}
	}, nil)
	r := agent.Request{SystemPrompt: "system", Messages: []agent.Message{agent.UserImageMessage("hello", "data:image/png;base64,YQ==")}, Tools: []agent.Tool{{Name: "lookup", Description: "Lookup", InputSchema: `{"type":"object"}`}}}
	ds, err := collect(m, context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	var calls []agent.ToolCall
	for _, d := range ds {
		if d.ToolCall != nil {
			c := d.ToolCall
			calls = append(calls, agent.ToolCall{ID: c.ID, Name: c.Name, Arguments: c.ArgumentsDelta})
		}
	}
	last := ds[len(ds)-1]
	if len(calls) != 2 || ds[0].Reasoning != "think" || last.FinishReason != agent.FinishToolCalls || last.Usage.ReasoningTokens != 4 || last.Continuation == nil {
		t.Fatalf("deltas: %+v", ds)
	}
	r.Messages = append(r.Messages, agent.Message{Role: agent.AssistantRole, Reasoning: "think", ToolCalls: calls, Continuation: last.Continuation})
	for _, c := range calls {
		r.Messages = append(r.Messages, agent.Message{Role: agent.ToolRole, ToolCallID: c.ID, Text: "result"})
	}
	ds, err = collect(m, context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if ds[0].Text != "done" || ds[len(ds)-1].FinishReason != agent.FinishStop {
		t.Fatalf("deltas: %+v", ds)
	}
}

func TestThinking(t *testing.T) {
	budget := int64(0)
	visible := false
	for _, cfg := range []*ThinkingConfig{{Budget: &budget, IncludeThoughts: &visible}, {Level: ThinkingHigh}} {
		m := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Generation struct {
					Thinking ThinkingConfig `json:"thinkingConfig"`
				} `json:"generationConfig"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			got := body.Generation.Thinking
			if cfg.Budget != nil && (got.Budget == nil || *got.Budget != 0 || got.IncludeThoughts == nil || *got.IncludeThoughts) {
				t.Errorf("thinking: %+v", got)
			}
			if cfg.Level != "" && got.Level != cfg.Level {
				t.Errorf("thinking: %+v", got)
			}
			event(w, `{"candidates":[{"finishReason":"STOP"}]}`)
		}, cfg)
		if _, err := collect(m, context.Background(), agent.Request{Messages: []agent.Message{agent.UserMessage("hi")}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, cfg := range []*ThinkingConfig{{Level: "extreme"}, {Budget: &budget, Level: ThinkingLow}} {
		if _, err := New(Config{APIKey: "key", Model: "m", Thinking: cfg}); err == nil {
			t.Fatal("accepted invalid thinking")
		}
	}
}

func TestFailuresAndFinish(t *testing.T) {
	tests := []struct {
		name, reply string
		status      int
		want        agent.FinishReason
		fail        bool
	}{
		{name: "length", reply: `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}}}]},"finishReason":"MAX_TOKENS"}]}`, want: agent.FinishLength},
		{name: "blocked", reply: `{"promptFeedback":{"blockReason":"SAFETY"}}`, want: agent.FinishBlocked},
		{name: "api", reply: `{"error":{"code":429,"message":"quota"}}`, fail: true},
		{name: "eof", reply: `{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`, fail: true},
		{name: "bad_json", reply: `{`, fail: true},
		{name: "unknown_part", reply: `{"candidates":[{"content":{"parts":[{"inlineData":{"data":"AA=="}}]},"finishReason":"STOP"}]}`, fail: true},
		{name: "unknown_finish", reply: `{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}]}`, fail: true},
		{name: "http", reply: `{"error":"original"}`, status: 400, fail: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.status != 0 {
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.reply)
					return
				}
				event(w, tc.reply)
			}, nil)
			ds, err := collect(m, context.Background(), agent.Request{})
			if (err != nil) != tc.fail {
				t.Fatalf("err=%v", err)
			}
			if !tc.fail && ds[len(ds)-1].FinishReason != tc.want {
				t.Errorf("finish=%s", ds[len(ds)-1].FinishReason)
			}
		})
	}
}

func TestUnsupportedInputAndCancellation(t *testing.T) {
	m := newTestModel(t, func(w http.ResponseWriter, r *http.Request) { t.Error("request should not reach server") }, nil)
	for _, msg := range []agent.Message{
		agent.UserImageMessage("hi", "https://example.com/a.png"),
		{Role: agent.AssistantRole, Reasoning: "lost signature"},
		{Role: agent.AssistantRole, Continuation: &agent.Continuation{Protocol: "anthropic", Data: json.RawMessage(`[]`)}},
		{Role: agent.ToolRole, ToolCallID: "missing"},
	} {
		if _, err := collect(m, context.Background(), agent.Request{Messages: []agent.Message{msg}}); err == nil {
			t.Fatalf("accepted %+v", msg)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := collect(m, ctx, agent.Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestAgentToolRoundTripAndTruncation(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(fmt.Sprint(truncated), func(t *testing.T) {
			requests, executions := 0, 0
			m := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests == 1 {
					finish := "STOP"
					if truncated {
						finish = "MAX_TOKENS"
					}
					event(w, fmt.Sprintf(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}},"thoughtSignature":"opaque"}]},"finishReason":%q}]}`, finish))
					return
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if !strings.Contains(string(body["contents"]), `"thoughtSignature":"opaque"`) || !strings.Contains(string(body["contents"]), `"functionResponse"`) {
					t.Errorf("roundtrip: %s", body["contents"])
				}
				event(w, `{"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}]}`)
			}, nil)
			a, err := agent.New(agent.Config{Model: m, Tools: []agent.Tool{{Name: "lookup", Description: "Lookup", InputSchema: `{"type":"object"}`, Execute: func(context.Context, string) (string, error) { executions++; return "result", nil }}}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var runErr error
			completed := false
			for e, err := range a.Run(ctx, nil, "hi") {
				if err != nil {
					runErr = err
				}
				if e.Type == agent.RunCompleted {
					completed = true
					if e.Message.Text != "done" {
						t.Errorf("message: %+v", e.Message)
					}
				}
			}
			if truncated {
				if !errors.Is(runErr, agent.ErrTruncated) || executions != 0 || requests != 1 {
					t.Fatalf("err=%v executions=%d requests=%d", runErr, executions, requests)
				}
			} else if runErr != nil || !completed || executions != 1 || requests != 2 {
				t.Fatalf("err=%v completed=%v executions=%d requests=%d", runErr, completed, executions, requests)
			}
		})
	}
}

func TestCancellationDuringStream(t *testing.T) {
	started := make(chan struct{})
	m := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := collect(m, ctx, agent.Request{}); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestUsageAfterFinishAndArgumentPrecision(t *testing.T) {
	m := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if !strings.Contains(string(body["contents"]), "9007199254740993") {
			t.Errorf("tool argument changed: %s", body["contents"])
		}
		event(w, `{"candidates":[{"content":{"parts":[{"text":"a"}]}}]}`)
		event(w, `{"candidates":[{"content":{"parts":[{"text":"b"}]},"finishReason":"STOP"}]}`)
		event(w, `{"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`)
	}, nil)
	ds, err := collect(m, context.Background(), agent.Request{Messages: []agent.Message{{Role: agent.AssistantRole, ToolCalls: []agent.ToolCall{{ID: "a", Name: "lookup", Arguments: `{"id":9007199254740993}`}}}, {Role: agent.ToolRole, ToolCallID: "a", Text: "ok"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 4 || ds[0].Text != "a" || ds[1].Text != "b" || ds[3].Usage.TotalTokens != 5 {
		t.Fatalf("deltas: %+v", ds)
	}
}

func TestTextControlCharacters(t *testing.T) {
	var controls strings.Builder
	for i := 0; i < 32; i++ {
		controls.WriteByte(byte(i))
	}
	text := controls.String() + string(rune(127))
	m := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			System   content   `json:"systemInstruction"`
			Contents []content `json:"contents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		for _, c := range append([]content{body.System}, body.Contents...) {
			var part struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(c.Parts[0], &part); err != nil {
				t.Error(err)
			}
			if part.Text != text {
				t.Errorf("text changed: %q", part.Text)
			}
		}
		event(w, `{"candidates":[{"finishReason":"STOP"}]}`)
	}, nil)
	_, err := collect(m, context.Background(), agent.Request{SystemPrompt: text, Messages: []agent.Message{agent.UserMessage(text), {Role: agent.AssistantRole, Text: text}}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestToolIDNamespaces(t *testing.T) {
	requests := 0
	m := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			event(w, `{"candidates":[{"content":{"parts":[{"functionCall":{"id":"gemini_call_1","name":"first","args":{}}},{"functionCall":{"name":"second","args":{}}}]},"finishReason":"STOP"}]}`)
			return
		}
		var body struct {
			Contents []content `json:"contents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		for i, p := range body.Contents[2].Parts {
			var got struct {
				Response struct {
					ID, Name string
					Response struct{ Result string }
				} `json:"functionResponse"`
			}
			if err := json.Unmarshal(p, &got); err != nil {
				t.Error(err)
			}
			wantName, wantID := "first", "gemini_call_1"
			if i == 1 {
				wantName, wantID = "second", ""
			}
			if got.Response.Name != wantName || got.Response.ID != wantID || got.Response.Response.Result != wantName+" result" {
				t.Errorf("response %d: %+v", i, got)
			}
		}
		event(w, `{"candidates":[{"finishReason":"STOP"}]}`)
	}, nil)
	r := agent.Request{Messages: []agent.Message{agent.UserMessage("hi")}}
	ds, err := collect(m, context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	var calls []agent.ToolCall
	for _, d := range ds {
		if d.ToolCall != nil {
			c := d.ToolCall
			calls = append(calls, agent.ToolCall{ID: c.ID, Name: c.Name, Arguments: c.ArgumentsDelta})
		}
	}
	if len(calls) != 2 || calls[0].ID == calls[1].ID {
		t.Fatalf("calls: %+v", calls)
	}
	r.Messages = append(r.Messages, agent.Message{Role: agent.AssistantRole, ToolCalls: calls, Continuation: ds[len(ds)-1].Continuation})
	for _, c := range calls {
		r.Messages = append(r.Messages, agent.Message{Role: agent.ToolRole, ToolCallID: c.ID, Text: c.Name + " result"})
	}
	if _, err := collect(m, context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r.Messages[1].ToolCalls[0].ID = "changed"
	if _, err := collect(m, context.Background(), r); err == nil {
		t.Fatal("accepted changed local tool ID")
	}
	if requests != 2 {
		t.Fatal("invalid continuation reached server")
	}
}

func TestContinuationHops(t *testing.T) {
	requests, executions := 0, 0
	var original map[string]json.RawMessage
	m := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if requests == 1 {
			original = body
			if _, ok := body["continuationToken"]; ok {
				t.Error("default token sent")
			}
		}
		if requests == 2 || requests == 3 {
			want := `"QQ=="`
			if requests == 3 {
				want = `"Qg=="`
			}
			if string(body["continuationToken"]) != want {
				t.Errorf("token: %s", body["continuationToken"])
			}
			for _, key := range []string{"contents", "systemInstruction", "tools", "generationConfig"} {
				if string(body[key]) != string(original[key]) {
					t.Errorf("%s changed", key)
				}
			}
			if executions != 0 {
				t.Error("tool executed before final finish")
			}
		}
		switch requests {
		case 1:
			event(w, `{"usageMetadata":{"promptTokenCount":2,"totalTokenCount":2}}`)
			event(w, `{"candidates":[{"content":{"parts":[{"text":"think","thought":true,"thoughtSignature":"opaque"},{"functionCall":{"id":"native","name":"lookup","args":{}}}]},"finishReason":"CONTINUATION","continuationToken":"QQ=="}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"thoughtsTokenCount":1,"totalTokenCount":6}}`)
		case 2:
			event(w, `{"candidates":[{"content":{"parts":[{"text":"a"}]},"finishReason":"CONTINUATION","continuationToken":"Qg=="}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":4,"totalTokenCount":6}}`)
		case 3:
			event(w, `{"candidates":[{"content":{"parts":[{"text":"b"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":5,"totalTokenCount":7}}`)
		case 4:
			if _, ok := body["continuationToken"]; ok {
				t.Error("continuation token leaked to next turn")
			}
			if !strings.Contains(string(body["contents"]), `"thoughtSignature":"opaque"`) || !strings.Contains(string(body["contents"]), `"id":"native"`) {
				t.Error("native parts lost")
			}
			event(w, `{"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}]}`)
		default:
			t.Error("unexpected request")
		}
	}, &ThinkingConfig{Level: ThinkingLow})
	a, err := agent.New(agent.Config{Model: m, SystemPrompt: "system", Tools: []agent.Tool{{Name: "lookup", Description: "Lookup", InputSchema: `{"type":"object"}`, Execute: func(context.Context, string) (string, error) { executions++; return "result", nil }}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for e, err := range a.Run(ctx, nil, "hi") {
		if err != nil {
			t.Fatal(err)
		}
		if e.Type == agent.RunCompleted && e.Usage != (agent.Usage{InputTokens: 6, OutputTokens: 12, ReasoningTokens: 1, TotalTokens: 19}) {
			t.Errorf("usage: %+v", e.Usage)
		}
	}
	if requests != 4 || executions != 1 {
		t.Fatalf("requests=%d executions=%d", requests, executions)
	}
}

func TestContinuationFailures(t *testing.T) {
	for _, tc := range []struct {
		name, first, second, want string
		status                    int
		requests                  int
		finish                    agent.FinishReason
	}{
		{name: "missing_token", first: `{"candidates":[{"finishReason":"CONTINUATION"}]}`, want: "continuationToken", requests: 1},
		{name: "second_http", second: `{"error":"upstream detail"}`, status: 503, want: "upstream detail", requests: 2},
		{name: "second_api", second: `{"error":{"message":"quota"}}`, want: "quota", requests: 2},
		{name: "second_eof", second: `{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`, want: "without finish", requests: 2},
		{name: "second_json", second: `{`, want: "decode stream", requests: 2},
		{name: "second_length", second: `{"candidates":[{"finishReason":"MAX_TOKENS"}]}`, requests: 2, finish: agent.FinishLength},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			m := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests == 1 {
					if tc.first != "" {
						event(w, tc.first)
						return
					}
					event(w, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]},"finishReason":"CONTINUATION","continuationToken":"QQ=="}],"usageMetadata":{"totalTokenCount":7}}`)
					return
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.second)
					return
				}
				event(w, tc.second)
			}, nil)
			ds, err := collect(m, context.Background(), agent.Request{})
			if requests != tc.requests {
				t.Fatalf("requests=%d", requests)
			}
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err=%v", err)
				}
				for _, d := range ds {
					if d.FinishReason != "" {
						t.Fatal("premature finish")
					}
				}
			} else if err != nil || ds[len(ds)-1].FinishReason != tc.finish {
				t.Fatalf("deltas=%+v err=%v", ds, err)
			}
			if tc.requests == 2 {
				found := false
				for _, d := range ds {
					if d.Usage.TotalTokens == 7 {
						found = true
					}
				}
				if !found {
					t.Fatal("first-hop usage lost")
				}
			}
		})
	}
}

func TestContinuationStopsWithConsumerOrContext(t *testing.T) {
	for _, mode := range []string{"consumer_text", "consumer_usage", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			requests := 0
			m := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				event(w, `{"candidates":[{"content":{"parts":[{"text":"partial"}]},"finishReason":"CONTINUATION","continuationToken":"QQ=="}],"usageMetadata":{"totalTokenCount":2}}`)
			}, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var gotErr error
			for d, err := range m.Stream(ctx, agent.Request{}) {
				if err != nil {
					gotErr = err
					break
				}
				if mode == "consumer_text" && d.Text != "" {
					break
				}
				if mode == "consumer_usage" && d.Usage.TotalTokens > 0 {
					break
				}
				if mode == "cancel" && d.Text != "" {
					cancel()
				}
			}
			if requests != 1 {
				t.Fatalf("requests=%d", requests)
			}
			if mode == "cancel" && !errors.Is(gotErr, context.Canceled) {
				t.Fatalf("err=%v", gotErr)
			}
		})
	}
}

func TestContinuationFailureNeverExecutesTools(t *testing.T) {
	for _, finish := range []string{"MAX_TOKENS", "API_ERROR"} {
		t.Run(finish, func(t *testing.T) {
			requests, executions := 0, 0
			m := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests == 1 {
					event(w, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]},"finishReason":"CONTINUATION","continuationToken":"QQ=="}]}`)
				} else if finish == "API_ERROR" {
					event(w, `{"error":{"message":"quota"}}`)
				} else {
					event(w, `{"candidates":[{"finishReason":"MAX_TOKENS"}]}`)
				}
			}, nil)
			a, err := agent.New(agent.Config{Model: m, Tools: []agent.Tool{{Name: "lookup", Description: "Lookup", InputSchema: `{"type":"object"}`, Execute: func(context.Context, string) (string, error) { executions++; return "result", nil }}}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var runErr error
			for _, err := range a.Run(ctx, nil, "hi") {
				if err != nil {
					runErr = err
				}
			}
			if runErr == nil || executions != 0 || requests != 2 {
				t.Fatalf("err=%v executions=%d requests=%d", runErr, executions, requests)
			}
			if finish == "MAX_TOKENS" && !errors.Is(runErr, agent.ErrTruncated) {
				t.Fatalf("err=%v", runErr)
			}
		})
	}
}

func TestContinuationTokenAcrossFrames(t *testing.T) {
	for _, finish := range []string{"CONTINUATION", "STOP"} {
		t.Run(finish, func(t *testing.T) {
			requests := 0
			m := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests == 1 {
					event(w, `{"candidates":[{"content":{"parts":[{"text":"a"}]},"continuationToken":"QQ=="}]}`)
					event(w, fmt.Sprintf(`{"candidates":[{"finishReason":%q}]}`, finish))
					return
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if string(body["continuationToken"]) != `"QQ=="` {
					t.Errorf("token: %s", body["continuationToken"])
				}
				event(w, `{"candidates":[{"content":{"parts":[{"text":"b"}]},"finishReason":"STOP"}]}`)
			}, nil)
			ds, err := collect(m, context.Background(), agent.Request{})
			if err != nil {
				t.Fatal(err)
			}
			wantRequests, wantText := 1, "a"
			if finish == "CONTINUATION" {
				wantRequests, wantText = 2, "ab"
			}
			var text strings.Builder
			finishes := 0
			for _, d := range ds {
				text.WriteString(d.Text)
				if d.FinishReason != "" {
					finishes++
				}
			}
			if requests != wantRequests || text.String() != wantText || finishes != 1 {
				t.Fatalf("requests=%d text=%q finishes=%d", requests, text.String(), finishes)
			}
		})
	}
}

func TestPromptBlockStopsPendingContinuation(t *testing.T) {
	requests := 0
	m := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		event(w, `{"candidates":[{"finishReason":"CONTINUATION","continuationToken":"QQ=="}]}`)
		event(w, `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"totalTokenCount":3}}`)
	}, nil)
	ds, err := collect(m, context.Background(), agent.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || ds[len(ds)-1].FinishReason != agent.FinishBlocked {
		t.Fatalf("requests=%d deltas=%+v", requests, ds)
	}
}

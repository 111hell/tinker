package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStreamAccumulatesToolCallDeltas(t *testing.T) {
	model := &scriptedModel{steps: [][]Delta{{
		{ToolCall: &ToolCallDelta{
			Index: 0, ID: "call-1", Name: "sea", ArgumentsDelta: `{"query":"`,
		}},
		{ToolCall: &ToolCallDelta{
			Index: 0, ID: "call-1", Name: "rch", ArgumentsDelta: `alice"}`,
		}},
	}}}
	runtime, err := New(Config{Model: model})
	if err != nil {
		t.Fatal(err)
	}

	message, _, err := runtime.stream(context.Background(), nil, func(Event) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	want := ToolCall{ID: "call-1", Name: "search", Arguments: `{"query":"alice"}`}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0] != want {
		t.Fatalf("unexpected tool calls: %#v", message.ToolCalls)
	}
}

func TestStreamRejectsNegativeToolCallIndex(t *testing.T) {
	model := &scriptedModel{steps: [][]Delta{{
		{ToolCall: &ToolCallDelta{Index: -1, ID: "call-1"}},
	}}}
	runtime, err := New(Config{Model: model})
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = runtime.stream(context.Background(), nil, func(Event) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "index must be non-negative") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunDoesNotExecuteToolWithInvalidArguments(t *testing.T) {
	model := &scriptedModel{steps: [][]Delta{
		{{ToolCall: &ToolCallDelta{Index: 0, ID: "call-1", Name: "search",
			ArgumentsDelta: `{"query":"alice"`}}},
		{{Text: "done"}},
	}}
	called := false
	runtime, err := New(Config{
		Model: model,
		Tools: []Tool{{
			Name: "search", Description: "Search contacts.",
			InputSchema: `{"type":"object"}`,
			Execute: func(context.Context, string) (string, error) {
				called = true
				return "", nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, runErr := range runtime.Run(ctx, nil, "search for Alice") {
		if runErr != nil {
			t.Fatal(runErr)
		}
	}
	if called {
		t.Fatal("tool was called with invalid JSON arguments")
	}
	result := model.requests[1].Messages[len(model.requests[1].Messages)-1]
	if result.Role != ToolRole || !strings.Contains(result.Text, "invalid JSON arguments") {
		t.Fatalf("unexpected tool result: %#v", result)
	}
}

func TestFinishPreventsToolExecution(t *testing.T) {
	for _, finish := range []FinishReason{FinishLength, FinishBlocked} {
		t.Run(string(finish), func(t *testing.T) {
			model := &scriptedModel{steps: [][]Delta{{{ToolCall: &ToolCallDelta{Index: 0, ID: "c", Name: "echo", ArgumentsDelta: `{}`}}, {FinishReason: finish}}}}
			called := false
			a, err := New(Config{Model: model, Tools: []Tool{{Name: "echo", Description: "Echo", InputSchema: `{}`, Execute: func(context.Context, string) (string, error) { called = true; return "", nil }}}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var got error
			for _, err := range a.Run(ctx, nil, "hi") {
				if err != nil {
					got = err
				}
			}
			want := ErrTruncated
			if finish == FinishBlocked {
				want = ErrBlocked
			}
			if !errors.Is(got, want) || called {
				t.Fatalf("err=%v called=%v", got, called)
			}
		})
	}
}

func TestStreamPreservesContinuationAndUsage(t *testing.T) {
	data := json.RawMessage(`[{"signature":"opaque"}]`)
	model := &scriptedModel{steps: [][]Delta{{{Usage: Usage{InputTokens: 2, TotalTokens: 2}}, {Text: "ok"}, {Continuation: &Continuation{Protocol: "test", Data: data}, FinishReason: FinishStop}}}}
	a, err := New(Config{Model: model})
	if err != nil {
		t.Fatal(err)
	}
	msg, usage, err := a.stream(context.Background(), nil, func(Event) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	data[0] = 'x'
	if msg.Continuation == nil || msg.Continuation.Data[0] != '[' || msg.FinishReason != FinishStop || usage.TotalTokens != 2 {
		t.Fatalf("msg=%#v usage=%#v", msg, usage)
	}
}

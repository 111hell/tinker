package agent

import (
	"context"
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

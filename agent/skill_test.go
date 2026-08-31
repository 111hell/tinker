package agent

import (
	"context"
	"iter"
	"strings"
	"testing"
	"time"
)

type scriptedModel struct {
	requests []Request
	steps    [][]Delta
}

func (m *scriptedModel) Stream(_ context.Context, request Request) iter.Seq2[Delta, error] {
	request.Messages = append([]Message(nil), request.Messages...)
	request.Tools = append([]Tool(nil), request.Tools...)
	m.requests = append(m.requests, request)
	step := m.steps[0]
	m.steps = m.steps[1:]
	return func(yield func(Delta, error) bool) {
		for _, delta := range step {
			if !yield(delta, nil) {
				return
			}
		}
	}
}

func TestRunLoadsSkillsAtStartup(t *testing.T) {
	model := &scriptedModel{steps: [][]Delta{
		{{Text: "done"}},
	}}
	runtime, err := New(Config{
		Model: model, SystemPrompt: "base prompt",
		Skills: []Skill{{
			Name:         "contact-analysis",
			Instructions: "Call siu_search_contacts and use only returned fields.",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var completed bool
	for event, runErr := range runtime.Run(ctx, nil, "analyze Alice") {
		if runErr != nil {
			t.Fatal(runErr)
		}
		completed = completed || event.Type == RunCompleted
	}
	if !completed || len(model.requests) != 1 {
		t.Fatalf("completed=%v requests=%d", completed, len(model.requests))
	}
	first := model.requests[0]
	if first.SystemPrompt != "base prompt\n\nCall siu_search_contacts and use only returned fields." {
		t.Fatalf("skill instructions missing from system prompt: %q", first.SystemPrompt)
	}
	if len(first.Tools) != 0 {
		t.Fatalf("unexpected definitions: %#v", first.Tools)
	}
}

func TestNewRejectsDuplicateSkills(t *testing.T) {
	_, err := New(Config{
		Model: &scriptedModel{},
		Skills: []Skill{
			{Name: "example", Instructions: "First."},
			{Name: "example", Instructions: "Second."},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate skill") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplicationToolMayUseLoadSkillName(t *testing.T) {
	model := &scriptedModel{steps: [][]Delta{
		{{ToolCall: &ToolCallDelta{Index: 0, ID: "call-1", Name: "load_skill",
			ArgumentsDelta: `{}`}}},
		{{Text: "done"}},
	}}
	called := false
	runtime, err := New(Config{
		Model: model,
		Tools: []Tool{{
			Name: "load_skill", Description: "application tool", InputSchema: `{}`,
			Execute: func(context.Context, string) (string, error) {
				called = true
				return "application result", nil
			},
		}},
		Skills: []Skill{{Name: "example", Instructions: "Do the example."}},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, runErr := range runtime.Run(ctx, nil, "run application tool") {
		if runErr != nil {
			t.Fatal(runErr)
		}
	}
	if !called {
		t.Fatal("application tool was not called")
	}
}

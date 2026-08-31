package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/janeshirley/tinker/agent"
	"github.com/janeshirley/tinker/model/openaicompat"
)

type searchInput struct {
	Query string `json:"query"`
}

func main() {
	model, err := openaicompat.New(openaicompat.Config{
		APIKey:  os.Getenv("OPENAI_API_KEY"),
		BaseURL: os.Getenv("OPENAI_BASE_URL"),
		Model:   os.Getenv("OPENAI_MODEL"),
	})
	if err != nil {
		log.Fatal(err)
	}

	// This data is trusted application context. The model can only provide query.
	notes := []string{"Go favors small interfaces", "Agent loops alternate model calls and tool results"}
	search := agent.Tool{
		Name:        "search_notes",
		Description: "Search the user's private notes",
		InputSchema: `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`,
		Execute: func(_ context.Context, arguments string) (string, error) {
			var input searchInput
			if err := json.Unmarshal([]byte(arguments), &input); err != nil {
				return "", err
			}
			var matches []string
			for _, note := range notes {
				if strings.Contains(strings.ToLower(note), strings.ToLower(input.Query)) {
					matches = append(matches, note)
				}
			}
			if len(matches) == 0 {
				return "No matching notes.", nil
			}
			return strings.Join(matches, "\n"), nil
		},
	}

	runtime, err := agent.New(agent.Config{
		Model:        model,
		SystemPrompt: "Use search_notes when the user asks about their notes.",
		Tools:        []agent.Tool{search},
	})
	if err != nil {
		log.Fatal(err)
	}

	input := "What do my notes say about agent loops?"
	if len(os.Args) > 1 {
		input = strings.Join(os.Args[1:], " ")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for event, runErr := range runtime.Run(ctx, nil, input) {
		if runErr != nil {
			log.Fatal(runErr)
		}
		if event.Type == agent.MessageDelta && event.Delta.Text != "" {
			fmt.Print(event.Delta.Text)
		}
	}
	fmt.Println()
}

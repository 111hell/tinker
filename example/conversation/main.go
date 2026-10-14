package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/111hell/tinker/agent"
	"github.com/111hell/tinker/model/openaicompat"
)

func main() {
	model, err := openaicompat.New(openaicompat.Config{
		APIKey:  os.Getenv("OPENAI_API_KEY"),
		BaseURL: os.Getenv("OPENAI_BASE_URL"),
		Model:   os.Getenv("OPENAI_MODEL"),
	})
	if err != nil {
		log.Fatal(err)
	}

	runtime, err := agent.New(agent.Config{Model: model})
	if err != nil {
		log.Fatal(err)
	}

	var history []agent.Message
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Println("Enter a message. Press Ctrl-D to exit.")
	for {
		fmt.Print("You: ")
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		next, err := runTurn(ctx, runtime, history, input)
		cancel()
		if err != nil {
			log.Printf("run: %v", err)
			continue
		}
		history = next
	}
	if err := scanner.Err(); err != nil {
		log.Fatal(err)
	}
}

func runTurn(ctx context.Context, runtime *agent.Agent, history []agent.Message, input string) ([]agent.Message, error) {
	next := history
	fmt.Print("Assistant: ")
	for event, err := range runtime.Run(ctx, history, input) {
		if err != nil {
			fmt.Println()
			return history, err
		}
		switch event.Type {
		case agent.MessageDelta:
			fmt.Print(event.Delta.Text)
		case agent.RunCompleted:
			next = event.Messages
		}
	}
	fmt.Println()
	return next, nil
}

// Package openaicompat adapts the OpenAI Chat Completions API to agent.Model.
package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"

	"github.com/111hell/tinker/agent"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
)

type Config struct {
	// APIKey authenticates requests to the OpenAI-compatible endpoint.
	APIKey string
	// BaseURL overrides the official OpenAI API base URL when non-empty.
	BaseURL string
	// Model is the Chat Completions model identifier sent on every request.
	Model string
	// HTTPClient optionally replaces the SDK's default HTTP client.
	HTTPClient *http.Client
}

type Model struct {
	client openai.Client
	name   string
}

var _ agent.Model = (*Model)(nil)

func New(config Config) (*Model, error) {
	if config.APIKey == "" || config.Model == "" {
		return nil, errors.New("openaicompat: APIKey and Model are required")
	}
	if config.BaseURL != "" {
		u, err := url.Parse(config.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("openaicompat: invalid BaseURL %q", config.BaseURL)
		}
	}
	opts := []option.RequestOption{option.WithAPIKey(config.APIKey)}
	if config.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(config.BaseURL))
	}
	if config.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(config.HTTPClient))
	}
	return &Model{client: openai.NewClient(opts...), name: config.Model}, nil
}

func (m *Model) Stream(ctx context.Context, request agent.Request) iter.Seq2[agent.Delta, error] {
	params, err := m.params(request)
	if err != nil {
		return func(yield func(agent.Delta, error) bool) { yield(agent.Delta{}, err) }
	}
	return func(yield func(agent.Delta, error) bool) {
		stream := m.client.Chat.Completions.NewStreaming(ctx, params)
		defer stream.Close()
		var usage agent.Usage
		for stream.Next() {
			chunk := stream.Current()
			if chunk.JSON.Usage.Valid() {
				usage = agent.Usage{
					InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens,
					ReasoningTokens: chunk.Usage.CompletionTokensDetails.ReasoningTokens,
					TotalTokens:     chunk.Usage.TotalTokens,
				}
			}
			for _, choice := range chunk.Choices {
				if choice.Index != 0 {
					continue
				}
				delta := agent.Delta{Text: choice.Delta.Content + choice.Delta.Refusal}
				if delta.Text != "" && !yield(delta, nil) {
					return
				}
				for _, call := range choice.Delta.ToolCalls {
					if !yield(agent.Delta{ToolCall: &agent.ToolCallDelta{
						Index: int(call.Index), ID: call.ID, Name: call.Function.Name,
						ArgumentsDelta: call.Function.Arguments,
					}}, nil) {
						return
					}
				}
			}
		}
		if err := stream.Err(); err != nil {
			yield(agent.Delta{}, err)
			return
		}
		yield(agent.Delta{Usage: usage}, nil)
	}
}

func (m *Model) params(request agent.Request) (openai.ChatCompletionNewParams, error) {
	messages := make([]openai.ChatCompletionMessageParamUnion, 0, len(request.Messages)+1)
	if request.SystemPrompt != "" {
		messages = append(messages, openai.SystemMessage(request.SystemPrompt))
	}
	for _, message := range request.Messages {
		converted, err := messageParam(message)
		if err != nil {
			return openai.ChatCompletionNewParams{}, err
		}
		messages = append(messages, converted)
	}
	tools := make([]openai.ChatCompletionToolUnionParam, 0, len(request.Tools))
	for _, tool := range request.Tools {
		var schema shared.FunctionParameters
		if err := json.Unmarshal([]byte(tool.InputSchema), &schema); err != nil {
			return openai.ChatCompletionNewParams{}, fmt.Errorf("openaicompat: tool %q schema: %w", tool.Name, err)
		}
		if schema == nil {
			return openai.ChatCompletionNewParams{}, fmt.Errorf("openaicompat: tool %q schema must be a JSON object", tool.Name)
		}
		tools = append(tools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name: tool.Name, Description: param.NewOpt(tool.Description), Parameters: schema,
		}))
	}
	return openai.ChatCompletionNewParams{
		Model: shared.ChatModel(m.name), Messages: messages, Tools: tools,
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: param.NewOpt(true)},
	}, nil
}

func messageParam(message agent.Message) (openai.ChatCompletionMessageParamUnion, error) {
	switch message.Role {
	case agent.UserRole:
		return openai.UserMessage(message.Text), nil
	case agent.AssistantRole:
		assistant := openai.ChatCompletionAssistantMessageParam{}
		if message.Text != "" {
			assistant.Content.OfString = param.NewOpt(message.Text)
		}
		if len(message.ToolCalls) > 0 {
			assistant.ToolCalls = make([]openai.ChatCompletionMessageToolCallUnionParam, len(message.ToolCalls))
			for i, call := range message.ToolCalls {
				assistant.ToolCalls[i].OfFunction = &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: call.ID,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name: call.Name, Arguments: call.Arguments,
					},
				}
			}
		}
		return openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant}, nil
	case agent.ToolRole:
		if message.ToolCallID == "" {
			return openai.ChatCompletionMessageParamUnion{}, errors.New("openaicompat: tool result lacks ToolCallID")
		}
		return openai.ToolMessage(message.Text, message.ToolCallID), nil
	default:
		return openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("openaicompat: unsupported role %q", message.Role)
	}
}

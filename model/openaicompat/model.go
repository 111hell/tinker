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
	// ReasoningEffort is sent verbatim; model-specific support is checked by the server.
	// Empty preserves the server default. It does not enable visible reasoning output.
	ReasoningEffort shared.ReasoningEffort
}

type Model struct {
	client openai.Client
	name   string
	effort shared.ReasoningEffort
}

var _ agent.Model = (*Model)(nil)

func New(config Config) (*Model, error) {
	switch config.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return nil, fmt.Errorf("openaicompat: unsupported reasoning effort %q", config.ReasoningEffort)
	}
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
	return &Model{client: openai.NewClient(opts...), name: config.Model, effort: config.ReasoningEffort}, nil
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
		var finish agent.FinishReason
		ids := map[int64]string{}
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
				switch choice.FinishReason {
				case "":
				case "stop":
					finish = agent.FinishStop
				case "tool_calls":
					finish = agent.FinishToolCalls
				case "length":
					finish = agent.FinishLength
				case "content_filter":
					finish = agent.FinishBlocked
				default:
					yield(agent.Delta{}, fmt.Errorf("openaicompat: unsupported finish reason %q", choice.FinishReason))
					return
				}
				// Compatible providers expose reasoning as an extension field.
				var extra struct {
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
				}
				if err := json.Unmarshal([]byte(choice.Delta.RawJSON()), &extra); err != nil {
					yield(agent.Delta{}, fmt.Errorf("openaicompat: decode reasoning: %w", err))
					return
				}
				reasoning := extra.ReasoningContent
				if reasoning == "" {
					reasoning = extra.Reasoning
				}
				delta := agent.Delta{Text: choice.Delta.Content + choice.Delta.Refusal, Reasoning: reasoning}
				if (delta.Text != "" || delta.Reasoning != "") && !yield(delta, nil) {
					return
				}
				for _, call := range choice.Delta.ToolCalls {
					if call.Index < 0 {
						yield(agent.Delta{}, errors.New("openaicompat: negative tool call index"))
						return
					}
					ids[call.Index] += call.ID
					if !yield(agent.Delta{ToolCall: &agent.ToolCallDelta{
						Index: int(call.Index), ID: ids[call.Index], Name: call.Function.Name,
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
		if err := ctx.Err(); err != nil {
			yield(agent.Delta{}, err)
			return
		}
		if finish == "" {
			yield(agent.Delta{}, errors.New("openaicompat: stream ended without finish reason"))
			return
		}
		yield(agent.Delta{Usage: usage, FinishReason: finish}, nil)
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
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(tool.InputSchema), &fields); err != nil {
			return openai.ChatCompletionNewParams{}, fmt.Errorf("openaicompat: tool %q schema: %w", tool.Name, err)
		}
		if fields == nil {
			return openai.ChatCompletionNewParams{}, fmt.Errorf("openaicompat: tool %q schema must be a JSON object", tool.Name)
		}
		schema := make(shared.FunctionParameters, len(fields))
		for key, value := range fields {
			schema[key] = value
		}
		tools = append(tools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name: tool.Name, Description: param.NewOpt(tool.Description), Parameters: schema,
		}))
	}
	return openai.ChatCompletionNewParams{
		Model: shared.ChatModel(m.name), Messages: messages, Tools: tools,
		ReasoningEffort: m.effort,
		StreamOptions:   openai.ChatCompletionStreamOptionsParam{IncludeUsage: param.NewOpt(true)},
	}, nil
}

func messageParam(message agent.Message) (openai.ChatCompletionMessageParamUnion, error) {
	if message.Continuation != nil {
		return openai.ChatCompletionMessageParamUnion{}, errors.New("openaicompat: protocol continuation is not supported")
	}
	if (message.Role != agent.UserRole && len(message.ImageURLs) != 0) ||
		(message.Role != agent.AssistantRole && (message.Reasoning != "" || len(message.ToolCalls) != 0)) ||
		(message.Role != agent.ToolRole && message.ToolCallID != "") {
		return openai.ChatCompletionMessageParamUnion{}, errors.New("openaicompat: unsupported fields for message role")
	}
	switch message.Role {
	case agent.UserRole:
		if len(message.ImageURLs) == 0 {
			return openai.UserMessage(message.Text), nil
		}
		parts := make([]openai.ChatCompletionContentPartUnionParam, 0, len(message.ImageURLs)+1)
		if message.Text != "" {
			parts = append(parts, openai.TextContentPart(message.Text))
		}
		for _, u := range message.ImageURLs {
			parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
				URL: u,
			}))
		}
		return openai.UserMessage(parts), nil
	case agent.AssistantRole:
		assistant := openai.ChatCompletionAssistantMessageParam{}
		if message.Reasoning != "" {
			assistant.SetExtraFields(map[string]any{"reasoning_content": message.Reasoning})
		}
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

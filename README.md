# Tinker

[English](README.md) | [简体中文](README.zh-CN.md)

> A small Go runtime for tool-using agents.

Tinker is a small, model-agnostic Go agent runtime. It drives model–tool interactions and exposes execution as streaming events, while the application owns sessions, memory, and lifecycle.

## Install

```sh
go get github.com/111hell/tinker
```

Requires Go 1.27 or later.

## Example

From the repository root:

```sh
OPENAI_API_KEY=... OPENAI_MODEL=... \
  go run ./example "What do my notes say about agent loops?"
```

See the runnable [`example`](example/) for the complete setup. OpenAI-compatible providers can be configured with `OPENAI_BASE_URL`.

For multi-turn conversation history, see [`example/conversation`](example/conversation/).

## API

- `agent.Model` streams provider-neutral model output.
- `agent.Tool` defines a JSON Schema input and a local Go function.
- `agent.Skill` exposes trusted `SKILL.md` instructions on demand.
- `agent.Agent.Run` executes the loop and streams message, tool, and completion events.
- `model/openaicompat` adapts OpenAI-compatible Chat Completions APIs.

Skills are loaded explicitly with `agent.LoadSkill`. Tinker exposes their metadata first and returns instructions only when the model calls `load_skill`; it does not scan global directories or execute skill scripts.

Tinker intentionally leaves persistence, orchestration, and application lifecycle outside the runtime.

## Model protocols

Choose an adapter explicitly and pass it as `agent.Config.Model`. There is no
model-name routing, discovery, or fallback.

| Package under `model/` | Protocol | Construction-time reasoning configuration |
| --- | --- | --- |
| `openaicompat` | Chat Completions | `Config.ReasoningEffort` |
| `openairesponses` | OpenAI Responses | `Reasoning{Effort, Summary}` |
| `anthropic` | Anthropic Messages | `Thinking`, independent `Effort`; required `MaxTokens` |
| `gemini` | Gemini Developer API | `ThinkingConfig{Budget, Level, IncludeThoughts}` |

Optional reasoning controls are omitted by default. Controls and visible reasoning
are independent; depth levels and token budgets are not translated across protocols.
Gemini continues `CONTINUATION` responses inside one `Stream`, keeping the original
request intact, accumulating per-request usage snapshots, and delaying tool execution
until final completion. Its public tool IDs are local correlation IDs; native IDs
are preserved in continuation data and tool responses.
All adapters support system instructions, text, user images, function tools and
multi-turn tool results. Gemini accepts base64 image data URLs only.

Persist complete `agent.Message` records, including the new `Continuation` field.
It preserves native output items, signed thinking blocks, or thought signatures;
keep these assistant records intact and use them only with the originating protocol.
`MessageDelta` also reports tool, usage, finish, and continuation updates.
`FinishReason` distinguishes normal completion, tool calls, truncation, and blocking.
The Agent returns `ErrTruncated` / `ErrBlocked` before executing tools; direct
`Model.Stream` users must check the finish reason themselves. Cancellation uses
context errors. Unsupported inputs and configuration values return errors;
model-specific support is checked by the server.

See the [Chinese protocol guide](README.zh-CN.md#模型协议与思考配置) for examples,
image and continuation boundaries, endpoint configuration, and official references.
Tests use local HTTP servers and fixed SSE fixtures, not live model services.

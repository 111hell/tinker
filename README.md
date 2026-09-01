# Tinker

[English](README.md) | [简体中文](README.zh-CN.md)

> A tiny Go runtime for tool-using agents.

Tinker runs a cancellable `model → tool → model` loop with streaming events and on-demand skills. Applications keep control of sessions, memory, transport, and scheduling.

## Features

- Small provider-neutral interfaces: `Agent`, `Model`, `Tool`, `Skill`, and `Event`
- Local tool execution with JSON Schema inputs
- Progressive skill loading from `SKILL.md`
- Streaming text, reasoning, tool, and completion events
- Explicit cancellation through a deadline-bound `context.Context`

## Install

```sh
go get github.com/111hell/tinker
```

Requires Go 1.27 or later.

## Quick start

The repository includes a runnable private-note search agent using a tool and a skill:

```sh
export OPENAI_API_KEY=...
export OPENAI_MODEL=...

# Optional for OpenAI-compatible providers:
export OPENAI_BASE_URL=https://example.com/v1/

go run ./example "What do my notes say about agent loops?"
```

See the runnable [`example`](example/) for the complete setup.

## How it works

1. `Agent.Run` sends the conversation, tool definitions, and skill metadata to `Model.Stream`.
2. The model may load relevant skill instructions through the built-in `load_skill` tool.
3. Tinker validates tool arguments, executes tools locally, and returns their results to the model.
4. The loop continues until the model returns a final message or the context ends.

The model chooses what to call; Tinker executes and controls the loop.

## Tools

Tools expose a name, description, JSON Schema, and local Go function. Arguments are JSON; results are plain text.

Pass identity, permissions, and other trusted context through application closures, not model-generated arguments.

## Skills

Load trusted skills explicitly and pass them to the agent:

```go
skill, err := agent.LoadSkill("./skills/contact-search")

runtime, err := agent.New(agent.Config{
	Model:  model,
	Tools:  []agent.Tool{search},
	Skills: []agent.Skill{skill},
})
```

Tinker initially exposes only each skill's name and description. Instructions are loaded from `SKILL.md` when the model calls `load_skill`. Tinker does not scan global skill directories or execute skill scripts and resources.

## Events

`Agent.Run` streams four event types:

- `MessageDelta`: model text or reasoning
- `ToolStarted`: tool execution started
- `ToolCompleted`: tool result or error text
- `RunCompleted`: final message, reusable history, and usage

Model errors and context cancellation are returned through the iterator. Tool errors are returned to the model as tool results.

## Models

`model/openaicompat` provides an OpenAI Chat Completions-compatible adapter. Other providers can implement the small `agent.Model` interface.

Tinker intentionally leaves sessions, persistence, memory, transport, scheduling, and multi-agent orchestration to the application.

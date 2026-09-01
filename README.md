# Tinker

[English](README.md) | [简体中文](README.zh-CN.md)

> A small Go runtime for tool-using agents.

Tinker provides a provider-neutral, cancellable `model → tool → model` loop with streaming events and on-demand skills. Applications retain ownership of sessions, memory, transport, and scheduling.

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

## API

- `agent.Model` streams provider-neutral model output.
- `agent.Tool` defines a JSON Schema input and a local Go function.
- `agent.Skill` exposes trusted `SKILL.md` instructions on demand.
- `agent.Agent.Run` executes the loop and streams message, tool, and completion events.
- `model/openaicompat` adapts OpenAI-compatible Chat Completions APIs.

Skills are loaded explicitly with `agent.LoadSkill`. Tinker exposes their metadata first and returns instructions only when the model calls `load_skill`; it does not scan global directories or execute skill scripts.

Tinker intentionally leaves persistence, orchestration, and application lifecycle outside the runtime.

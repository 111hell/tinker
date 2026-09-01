# Tinker

[English](README.md) | [简体中文](README.zh-CN.md)

> 一个用于工具调用 Agent 的小型 Go Runtime。

Tinker 负责可取消的 `model → tool → model` 循环，并提供流式事件和按需加载的 Skill。Session、Memory、Transport 和业务调度仍由应用掌控。

## 特点

- 小型、Provider 无关的 `Agent`、`Model`、`Tool`、`Skill` 和 `Event` 接口
- 使用 JSON Schema 描述输入，在本地执行工具
- 从 `SKILL.md` 渐进加载 Skill
- 流式输出文本、reasoning、工具状态和运行结果
- 通过带 deadline 的 `context.Context` 显式控制生命周期

## 安装

```sh
go get github.com/111hell/tinker
```

需要 Go 1.27 或更高版本。

## 快速开始

仓库提供了一个使用 Tool 和 Skill 的私有笔记搜索示例：

```sh
export OPENAI_API_KEY=...
export OPENAI_MODEL=...

# 使用 OpenAI 兼容服务时可选：
export OPENAI_BASE_URL=https://example.com/v1/

go run ./example "What do my notes say about agent loops?"
```

完整配置见可运行的 [`example`](example/)。

## 工作方式

1. `Agent.Run` 将对话、Tool 定义和 Skill 元数据传给 `Model.Stream`。
2. 模型可以通过内置的 `load_skill` Tool 加载相关 Skill 指令。
3. Tinker 校验参数、在本地执行 Tool，再把结果交还模型。
4. 循环持续到模型返回最终消息或 context 结束。

模型决定调用什么；Tinker 负责执行并控制循环。

## Tool

Tool 包含名称、描述、JSON Schema 和本地 Go 函数。参数使用 JSON，结果使用普通文本。

身份、权限等可信上下文应通过应用闭包传入，不应来自模型生成的参数。

## Skill

调用方显式加载可信 Skill 并传给 Agent：

```go
skill, err := agent.LoadSkill("./skills/contact-search")

runtime, err := agent.New(agent.Config{
	Model:  model,
	Tools:  []agent.Tool{search},
	Skills: []agent.Skill{skill},
})
```

Tinker 启动时只暴露 Skill 的名称和描述。模型调用 `load_skill` 后，才会加载 `SKILL.md` 指令。Tinker 不扫描全局 Skill 目录，也不执行 Skill scripts 和 resources。

## Event

`Agent.Run` 返回四类流式事件：

- `MessageDelta`：模型文本或 reasoning
- `ToolStarted`：开始执行 Tool
- `ToolCompleted`：Tool 结果或错误文本
- `RunCompleted`：最终消息、可复用历史和用量

模型错误和 context 取消通过 iterator 返回；Tool 错误会作为 Tool Result 交还模型。

## Model

`model/openaicompat` 提供 OpenAI Chat Completions 兼容 Adapter。其他 Provider 只需实现小型 `agent.Model` 接口。

Tinker 不负责 Session、持久化、Memory、Transport、Scheduler 和 Multi-Agent 编排。

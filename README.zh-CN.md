# Tinker

[English](README.md) | [简体中文](README.zh-CN.md)

> 一个用于工具调用 Agent 的小型 Go Runtime。

Tinker 是一个轻量、模型无关的 Go Agent Runtime。它负责驱动模型与工具的交互，并以流式事件暴露运行过程；Session、Memory 和应用生命周期由调用方管理。

## 安装

```sh
go get github.com/111hell/tinker
```

需要 Go 1.27 或更高版本。

## 示例

在仓库根目录运行：

```sh
OPENAI_API_KEY=... OPENAI_MODEL=... \
  go run ./example "What do my notes say about agent loops?"
```

完整配置见可运行的 [`example`](example/)。OpenAI 兼容服务可通过 `OPENAI_BASE_URL` 配置。

## API

- `agent.Model`：输出 Provider 无关的模型流。
- `agent.Tool`：定义 JSON Schema 输入和本地 Go 函数。
- `agent.Skill`：按需提供可信的 `SKILL.md` 指令。
- `agent.Agent.Run`：执行循环并输出消息、Tool 和完成事件。
- `model/openaicompat`：适配 OpenAI Chat Completions 兼容 API。

Skill 通过 `agent.LoadSkill` 显式加载。Tinker 首先只暴露元数据，模型调用 `load_skill` 时才返回指令；它不会扫描全局目录或执行 Skill scripts。

Tinker 不负责持久化、编排和应用生命周期。

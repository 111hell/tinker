# Tinker

[English](README.md) | [简体中文](README.zh-CN.md)

> 一个用于工具调用 Agent 的小型 Go Runtime。

Tinker 提供 Provider 无关、可取消的 `model → tool → model` 循环，并支持流式事件和按需加载 Skill。Session、Memory、Transport 和 Scheduler 仍由应用负责。

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

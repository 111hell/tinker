# Tinker

> A tiny Go runtime for tool-using agents.

Tinker 是一个最小 Go Agent Runtime。它把模型、工具、固定 Skill 和流式事件组成一次可取消的 Agent Run。

```text
user input → model stream → tool call → tool result → model stream → final answer
```

Tinker 只负责这个循环。Session、Memory、Transport 和业务调度仍由应用掌控。

## 特点

- **小而直接**：核心只有 `Agent`、`Model`、`Tool`、`Skill` 和 `Event`。
- **Provider 无关**：核心依赖 `agent.Model` 接口；仓库附带 OpenAI Chat Completions 兼容 Adapter。
- **工具边界明确**：模型只看到名称、描述和 JSON Schema，Go 函数由 Runtime 在本地执行。
- **流式事件**：应用可以实时接收模型文本、工具执行状态和最终结果。
- **显式生命周期**：每次 `Run` 都必须使用带 deadline 的 `context.Context`。

## 安装

```sh
go get github.com/111hell/tinker
```

Tinker 需要 Go 1.27 或更高版本。

## 最小用法

下面是完整的 Model、Tool、Agent 和事件消费链路：

```go
model, err := openaicompat.New(openaicompat.Config{
	APIKey: os.Getenv("OPENAI_API_KEY"),
	Model:  os.Getenv("OPENAI_MODEL"),
})
if err != nil {
	return err
}

echo := agent.Tool{
	Name:        "echo",
	Description: "Echo text back to the model",
	InputSchema: `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`,
	Execute: func(_ context.Context, arguments string) (string, error) {
		var input struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(arguments), &input); err != nil {
			return "", err
		}
		return input.Text, nil
	},
}

runtime, err := agent.New(agent.Config{
	Model:        model,
	SystemPrompt: "Use echo when the user asks you to repeat something.",
	Tools:        []agent.Tool{echo},
})
if err != nil {
	return err
}

ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
defer cancel()

for event, runErr := range runtime.Run(ctx, nil, "Repeat: hello") {
	if runErr != nil {
		return runErr
	}
	if event.Type == agent.MessageDelta {
		fmt.Print(event.Delta.Text)
	}
}
```

`InputSchema` 和模型生成的 `ToolCall.Arguments` 使用 JSON。`Execute` 返回普通文本，不要求 JSON。身份、权限和其他可信上下文应通过应用闭包传入，不应暴露给模型。

仓库内还有一个可直接运行的私有笔记搜索示例：[`example/main.go`](example/main.go)。

```sh
export OPENAI_API_KEY=...
export OPENAI_MODEL=...
# 使用兼容服务时再设置：
export OPENAI_BASE_URL=https://example.com/v1/

go run ./example "What do my notes say about Go?"
```

## 运行模型

1. `Run` 复制调用方传入的历史消息，并追加当前用户输入。
2. Runtime 将消息、System Prompt 和 Tool 定义传给 `Model.Stream`。
3. 如果模型返回 Tool Call，Runtime 校验 JSON arguments，执行对应 Go 函数，并把结果追加为 Tool Message。
4. Runtime 再次调用模型，直到模型返回不包含 Tool Call 的最终消息。

模型决定是否使用工具；Runtime 只负责执行和控制循环。

## 事件

| 事件 | 主要字段 | 含义 |
| --- | --- | --- |
| `MessageDelta` | `Delta` | 模型流式输出的文本或 reasoning |
| `ToolStarted` | `ToolCall` | Runtime 即将执行工具 |
| `ToolCompleted` | `ToolCall`, `Message` | 工具返回结果或错误文本 |
| `RunCompleted` | `Message`, `Messages`, `Usage` | 运行完成，包含最终消息、完整历史和用量 |

模型错误、`context` 取消和缺少 deadline 通过 iterator 的 `error` 返回。工具错误会转换成 Tool Message 交还模型，不会直接终止 Agent Loop。

## Skill

`Skill` 是在 `agent.New` 时加载到 System Prompt 的固定指令：

```go
Skills: []agent.Skill{{
	Name:         "contact-search",
	Instructions: "Call search and answer only from its result.",
}}
```

它不负责 Skill 文件发现、按需加载或资源执行。

## OpenAI 兼容 Adapter

```go
model, err := openaicompat.New(openaicompat.Config{
	APIKey:  apiKey,
	BaseURL: baseURL, // 官方 OpenAI 留空；兼容服务填写 API Base URL。
	Model:   modelName,
})
```

`model/openaicompat` 只处理标准 Chat Completions 字段。厂商私有字段和私有推理协议应由独立 Adapter 处理。

## 包边界

- `agent`：消息、Model、Tool、Skill、Agent Loop 和 Event。
- `model/openaicompat`：基于官方 `openai-go` 的 Chat Completions Model Adapter。

Session、SQLite、Scheduler、Transport、Skill 文件发现/资源执行、MCP、Memory、RAG、Multi-Agent、Handoff、Graph、插件和 Provider Registry 都不在 Tinker 的职责范围内。

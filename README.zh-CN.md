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

多轮对话历史的处理方式见 [`example/conversation`](example/conversation/)。

## API

- `agent.Model`：输出 Provider 无关的模型流。
- `agent.Tool`：定义 JSON Schema 输入和本地 Go 函数。
- `agent.Skill`：按需提供可信的 `SKILL.md` 指令。
- `agent.Agent.Run`：执行循环并输出消息、Tool 和完成事件。
- `model/openaicompat`：适配 OpenAI Chat Completions 兼容 API。

Skill 通过 `agent.LoadSkill` 显式加载。Tinker 首先只暴露元数据，模型调用 `load_skill` 时才返回指令；它不会扫描全局目录或执行 Skill scripts。

Tinker 不负责持久化、编排和应用生命周期。

## 模型协议与思考配置

应用显式构造一个适配器，传给 `agent.Config.Model`；不会根据模型名猜协议、探测或自动回退。兼容 Chat Completions 的服务继续使用 `openaicompat`，无需增加 provider。

| 包（`model/` 下） | 接口 | 思考控制（构造时） | 图片输入 | 续接数据 |
| --- | --- | --- | --- | --- |
| `openaicompat` | Chat Completions | `Config.ReasoningEffort` | 用户消息 URL / data URL，取决于服务 | 兼容扩展 `reasoning_content` 文本 |
| `openairesponses` | OpenAI Responses | `Reasoning{Effort, Summary}`，分别可选 | 用户消息 URL / data URL，取决于服务 | 原生 output items，包括 call ID 和加密 reasoning |
| `anthropic` | Anthropic Messages | `Thinking` 开关 / 预算 / adaptive；独立 `Effort` | 用户消息 HTTP(S) / base64 data URL | 原生 content blocks，包括签名和 redacted thinking |
| `gemini` | Gemini Developer API `streamGenerateContent` | `ThinkingConfig` 预算或档位；独立 `IncludeThoughts` | 用户消息 base64 image data URL | 原生 parts，包括 thought signature；自动处理 `CONTINUATION` |

四种适配器均支持系统指令、文本、多个函数工具、工具结果、多轮续接、流式可见 reasoning、用量和结束原因。工具参数按各自协议解析：Chat Completions / Responses / Messages 聚合字符串片段；Gemini 接收完整 `functionCall.args` 对象。不同协议、不同模型的 thinking/effort 语义不等价，不做档位转换。所有可选思考参数默认省略；设置了强度不代表服务会返回可见 reasoning。

最小构造示例（按需导入对应包，`key` 和 `modelName` 由应用提供；每个构造函数都应检查 `err`）：

```go
m, err := openaicompat.New(openaicompat.Config{
    APIKey: key, Model: modelName, ReasoningEffort: "high",
})
// 或显式选择其他协议：
m, err := openairesponses.New(openairesponses.Config{
    APIKey: key, Model: modelName,
    Reasoning: openairesponses.Reasoning{
        Effort: openairesponses.EffortHigh,
        Summary: openairesponses.SummaryAuto, // 需要可见摘要时单独设置
    },
})
m, err := anthropic.New(anthropic.Config{
    APIKey: key, Model: modelName, MaxTokens: 4096,
    Thinking: &anthropic.Thinking{Type: "enabled", BudgetTokens: 1024},
})
budget, visible := int64(1024), true
m, err := gemini.New(gemini.Config{
    APIKey: key, Model: modelName,
    Thinking: &gemini.ThinkingConfig{Budget: &budget, IncludeThoughts: &visible},
})
```

上面是四个独立片段，不应直接拼成同一个作用域。选定适配器后沿用现有循环：

```go
if err != nil { return err }
a, err := agent.New(agent.Config{Model: m, SystemPrompt: "请简洁回答", Tools: tools})
if err != nil { return err }
ctx, cancel := context.WithTimeout(ctx, time.Minute)
defer cancel()
for event, err := range a.Run(ctx, history, "查询当前状态") {
    if err != nil { return err }
    if event.Type == agent.RunCompleted {
        history = event.Messages // 包含工具结果和协议续接数据
    }
}
```

应用需要接入的公开接口与边界：

- 新增结构体字段会影响使用未具名结构体字面量的外部代码，请使用具名字段构造。
- `agent.Model.Stream` 和 `agent.Agent.Run/RunMessage` 保持不变。`MessageDelta` 现在也包含工具参数、用量、结束和续接更新；仅需显示文字的调用方读取 `Delta.Text/Reasoning` 即可。
- `Message.Continuation` 和 `Delta.Continuation` 是协议拥有的完整快照，不是文本。应用保存/恢复历史时必须完整保留 `Protocol` 和 `Data`，不可裁掉签名、重建为 reasoning 文本或跨协议复用。带续接数据的 assistant 消息会核对显示字段与原生内容是否一致；修改文本或工具字段会报错，应把整条消息视为不可拆分记录。它可能包含敏感或加密内容，不应展示或记录到日志。
- `FinishReason` 区分 `stop`、`tool_calls`、`length`、`blocked`。内置适配器缺少终结事件会返回错误。Agent 遇到截断 / 拦截返回 `agent.ErrTruncated` / `agent.ErrBlocked`，不执行该轮任何工具；已流出的文字只是部分结果。直接调用 `Model.Stream` 的应用须自行检查结束原因。自定义 Model 未发送结束状态时保留原有 EOF 行为。
- 取消和超时沿用 `context` 错误；HTTP / 接口流错误保留服务端错误信息。未知配置值、不支持的消息字段或异协议续接会明确报错；模型特定限制交给服务器，不维护模型能力目录。Anthropic 要求显式正数 `MaxTokens`，enabled thinking 预算至少 1024 且小于它；Gemini 预算和档位互斥，预算 `-1` 表示动态，`0` 表示请求关闭，具体模型是否允许由服务端决定。
- Gemini 的工具 ID 用于 Tinker 内部关联，不保证等于服务端原生 ID；原生 ID 仍完整保存在续接数据中，并用于对应的工具结果回传。
- Gemini 收到 `CONTINUATION` 后，在同一次 `Stream` 中使用原始请求和返回的 `continuationToken` 继续生成；不会把部分输出追加到 `contents`。所有请求完成后才返回最终结束状态、保存完整 parts 并允许执行工具。缺少令牌、后续请求失败或取消会终止本轮，不执行未完成的工具。每次 HTTP 请求内使用最新用量快照，跨续接请求累加，与[官方 Go SDK 的续接用量约定](https://github.com/googleapis/go-genai/blob/v1.74.0/models.go#L5136)一致。
- `BaseURL` 是版本根路径：OpenAI 通常含 `/v1`，Anthropic 含 `/v1`，Gemini 通常含 `/v1beta`；`HTTPClient` 可替换。新原生适配器使用标准库 HTTP，不新增 SDK 依赖，不自动重试；原有 Chat Completions 保留 `openai-go/v3 v3.50.0` 的请求行为。
- 仅覆盖文本、输入图片和本地函数工具。服务端托管工具、音频、视频、输出图片、Gemini 文件 URI / HTTP 图片 URL、跨协议迁移历史不在此接口内；不下载远程图片，也不伪造签名。兼容接口扩展若需要额外不透明续接数据，应增加经验证的处理，不能仅凭模型名视为兼容。

协议依据：[OpenAI Chat Completions](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)、[Responses streaming](https://developers.openai.com/api/reference/resources/responses/streaming-events)、[Anthropic streaming](https://platform.claude.com/docs/en/build-with-claude/streaming)、[Gemini generateContent](https://ai.google.dev/api/generate-content)、[Gemini thinking](https://ai.google.dev/gemini-api/docs/thinking)。

验证使用 `httptest` 本地 HTTP 服务和固定 SSE 响应，覆盖实际适配器请求、可选参数省略、工具往返、续接数据、截断、错误与取消，并包含复杂 Anthropic usage、Gemini 控制字符和工具 ID 碰撞、多个续接请求、Schema 大整数 / 小数 / 指数保真回归。运行 `go test -race ./...` 和 `go vet ./...`；这不代表真实模型服务已验证，也不保证每个模型支持表中全部能力。会话、模型选择、配置加载、存储、权限和交付仍由应用负责。

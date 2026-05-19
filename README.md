# claude-proxy

一个本地 Go 代理，把 Anthropic 风格的 `POST /v1/messages` 请求转发到 DeepSeek 的兼容接口，并在响应结束后打印 token 统计日志。

## 功能

- 监听本地 `127.0.0.1:38471`
- 接收 `POST /v1/messages`
- 转发到 `https://api.deepseek.com/anthropic/v1/messages`
- 如果请求体里存在 `metadata.user_id`，强制改写成 `DEEPSEEK_USER_ID`
- 透传 DeepSeek 的响应头和响应体
- 响应结束后打印一条 token 统计日志

## 当前行为

### 请求处理

- 只处理 `POST /v1/messages`
- `GET /healthz` 返回 `200 ok`
- 如果请求体不是合法 JSON，直接返回 `400`
- 如果 `metadata` 不存在，不会自动补一个 `metadata.user_id`，而是原样转发

### 上游转发

- 上游固定为 `https://api.deepseek.com/anthropic/v1/messages`
- 请求会带上：
  - `Authorization: Bearer $DEEPSEEK_API_KEY`
  - `x-api-key: $DEEPSEEK_API_KEY`
- 会透传大部分原始请求头，但会移除这些头后重设或忽略：
  - `Host`
  - `Content-Length`
  - `Authorization`
  - `x-api-key`

### 响应透传

- 会把 DeepSeek 的响应状态码原样返回
- 会把 DeepSeek 的响应头大部分原样返回
- 不透传这两个响应头：
  - `Transfer-Encoding`
  - `Connection`
- 如果 DeepSeek 返回流式 `text/event-stream`，代理会边读边写并立即 `flush`
- 如果 DeepSeek 返回非流式响应，代理也会边读边写，不会改写响应体格式
- 为了尽量保持上游响应原样，HTTP client 关闭了自动解压缩

## Token 日志

当前程序只打印 token 统计相关日志，不打印其他常规运行日志。

日志格式有两种：

```text
2026/05/19 17:45:18.854280 deepseek token stats: hit=28416 miss=1366 prompt=29782 output=54 status=200
```

或者：

```text
2026/05/19 17:43:19.133553 deepseek token stats: prompt=1234 output=68 status=200
```

### 字段来源

代码会优先按 DeepSeek 实际返回字段解析：

| 日志字段 | 返回字段 | 说明 |
| -------- | -------- | ---- |
| `hit` | `cache_read_input_tokens` | 缓存命中的输入 token |
| `miss` | `input_tokens + cache_creation_input_tokens` | 未命中输入 token，加上新写入缓存的输入 token |
| `prompt` | `hit + miss` | 总输入 token |
| `output` | `output_tokens` | 输出 token |
| `status` | HTTP status code | 上游响应状态码 |

如果上游返回的是另一套兼容字段，也支持降级解析：

- `prompt_tokens`
- `prompt_cache_hit_tokens`
- `prompt_cache_miss_tokens`
- `output_tokens`
- `completion_tokens`

### 日志触发时机

- JSON 响应：响应体读完后打印
- SSE 响应：整条流结束后，从最后一个带 `usage` 的事件里提取并打印
- 如果响应里完全没有可识别的 token 字段，就不会打印日志

## 环境变量

需要设置：

```bash
export DEEPSEEK_API_KEY="your_deepseek_api_key"
export DEEPSEEK_USER_ID="your_user_id"
```

## 启动

```bash
go run main.go
```

启动后监听：

```text
127.0.0.1:38471
```

健康检查：

```bash
curl http://127.0.0.1:38471/healthz
```

预期返回：

```text
ok
```

## 使用方式

把原本请求 Anthropic Messages API 的地址改成：

```text
http://127.0.0.1:38471/v1/messages
```

请求体继续使用 Anthropic Messages 风格即可。

示例：

```bash
curl http://127.0.0.1:38471/v1/messages \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-chat",
    "max_tokens": 512,
    "messages": [
      {
        "role": "user",
        "content": "你好，介绍一下你自己"
      }
    ],
    "metadata": {
      "user_id": "will_be_rewritten"
    }
  }'
```

## 限制

- 只代理一个接口：`/v1/messages`
- 监听地址写死为 `127.0.0.1:38471`
- 上游地址写死为 `https://api.deepseek.com/anthropic/v1/messages`
- 只有当请求体里已经存在 `metadata` 时，才会改写其中的 `user_id`
- token 日志是在响应结束后打印，不是实时打印

## 开发

本地编译：

```bash
go build ./...
```

# oc2api

把 OpenCode Zen 的免费模型包装成 OpenAI **Responses API**（`POST /v1/responses`）的本地网关，用 Go 编写，无第三方依赖。

目标只有一个：**不再出现 `403 FreeTierError`**。免费额度耗尽时返回 429，而不是被判定成"非 OpenCode 客户端"。

---

## 1. 403 FreeTierError 是怎么被触发的

2026-10 起 Zen 的 `/inference` 免费层会对请求做指纹校验，任何一项不匹配都会返回：

```json
{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode."}}
```

实测（本机对 `https://opencode.ai/inference` 反复探测）确认的必需条件：

| 条件 | 要求 |
| --- | --- |
| 凭据 | `Authorization: Bearer public`（不能缺省，且大小写敏感） |
| User-Agent | `opencode/latest/2.1.1/cli`（v2 通道）或 `opencode/1.19.3 ai-sdk/provider-utils/<sdk> runtime/bun/1.3.14`（v1 通道） |
| 会话 ID | **必须**是 `ses_` + 12 位十六进制（倒序时间戳，低 12 位落在 `fc0`-`fff`）+ 14 位 Base62，总长 30 |
| 会话头 | `x-opencode-session`、`x-opencode-session-id`、`x-session-affinity`、`X-Session-Id` 四个都要带 |
| 项目头 | `x-opencode-project`：40 位十六进制（真实客户端用 git root commit，这里用哈希生成） |
| 客户端头 | `x-opencode-client: cli` |
| 工具 | **必须**带齐 `shell`、`edit`、`glob`、`grep`、`read` 五个工具；只有 `read` 也会 403 |
| 流式 | 请求体必须 `stream: true`（Chat 还需 `stream_options.include_usage`） |
| v1 通道 | 额外需要 `x-opencode-request: msg_...`，且 v2 通道**不能**带这个头 |

> 最容易踩的坑：会话 ID 长度或时间戳形状不对。`ses_...` 少一位、随机段不是 14 位、时间戳不以 `ffe` 一类结尾，都会直接 403。很多"明明头都对了还是 403"都是这个原因。

本网关在每次上游请求前自动完成以上整形，客户端不需要关心。

---

## 2. 快速开始

```bash
cd oc2api
go build -o oc2api.exe .

# 直接跑（默认 127.0.0.1:8787，v2 通道，匿名 public，免费模型）
./oc2api.exe

# 或指定配置
./oc2api.exe -config config.example.json -debug
```

请求：

```bash
curl -N http://127.0.0.1:8787/v1/responses \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-5.3-flash","input":"你好","stream":true}'
```

非流式（网关内部仍以 `stream: true` 访问上游，再把 SSE 聚合成标准 JSON）：

```bash
curl http://127.0.0.1:8787/v1/responses \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-5.3-flash","input":"你好","stream":false}'
```

模型列表：

```bash
curl http://127.0.0.1:8787/v1/models
```

---

## 3. 路由与协议

入口统一为 Responses API，网关按模型自动选择上游协议：

| 上游协议 | 适用模型（示例） | 上游端点 |
| --- | --- | --- |
| OpenAI Chat | `glm-5.3-flash`、`deepseek-v4.1-flash`、`mimo-v2.6-flash` | `/openai/v1/chat/completions` |
| Anthropic Messages | `minimax-m2.1`、`qwen3.6-plus` | `/anthropic/v1/messages` |
| OpenAI Responses | `muse-spark-1.3-contributor` | `/openai/v1/responses` |

协议由 `https://models.opencode.ai/api.json` 的 `provider.npm` 决定（10 分钟刷新一次，带本地缓存与名称启发式兜底）。Gemini 系列暂不支持，会返回 501。

**模型命名**：目录在代码层过滤为「免费 / 匿名」模型，并把 id 末尾的 `-free` 去掉后对外暴露。也就是说客户端请求 `glm-5.3-flash`，网关内部使用 `glm-5.3-flash-free` 访问上游；`/v1/models` 返回的也是去掉后缀的名字。`big-pickle`、`grok-code` 这类本身无后缀的模型保持原样。

**下线检测**：两层过滤。第一层每次刷新目录时同时拉取免认证的 `https://opencode.ai/inference/v1/models`，与目录做交集，已下线的 id 直接不出现（目录里 46 个免费模型通常只剩 8-9 个 live）。第二层是启动时用 16 token 逐个探测，运行中遇到 `410` / `ModelDeprecated` / `model not found` 也会即时移除；直接请求已移除的模型返回 `410 model_deprecated`。被移除的模型在 `/healthz` 的 `disabled_models` 里可见。

Responses 侧支持：`input`（字符串或 item 数组）、`instructions`、`tools`/`tool_choice`、`max_output_tokens`、`temperature`、`top_p`、`reasoning.effort`、`stream`。输出为标准 Responses 事件序列（`response.created` → `output_item.added` → `response.output_text.delta` → `response.completed` → `[DONE]`），包含 reasoning、function_call 与 usage。

---

## 4. 缓存（相同请求复用）

每个请求都会按内容计算 SHA-256：

```
model + instructions + messages + tools + tool_choice + 采样参数 + reasoning + stream
```

命中后直接回放上次的完整响应（流式回放 SSE，非流式回放 JSON），不再消耗免费额度，也不会占用出口。

- 默认开启：TTL 30 分钟，最多 500 条，LRU 淘汰。
- 响应头 `x-oc2api-cache: hit|miss` 可观测。
- 请求头 `x-oc2api-cache: bypass` 可强制穿透。
- 缓存是进程内的；重启即失效（免费额度按天计，长跑建议保持进程常驻）。
- 会话 ID 由"指令 + 首条用户消息"的哈希稳定派生，因此同一对话的多次请求会复用同一个上游 session，命中上游的 prompt cache。

---

## 5. 出口与重试

- 默认 `direct` 直连；配置 `proxies` 后按轮转顺序追加出口，`http://`、`https://`、`socks5://` 都支持。
- 代理条目末尾加 `#pool` 表示这是一个**轮换代理池**：不参与冷却，且每次请求都新建连接（不复用 keep-alive），因为出口 IP 是按连接分配的。
- 403（FreeTierError）→ 该出口冷却 900 秒，换出口并用**新的**会话 ID 重试。
- 429（FreeUsageLimitError）→ 该出口冷却 60 秒，换出口重试。
- 5xx / 网络错误 → 冷却 15 秒后换出口。
- 4xx（除 401/403/429）视为请求本身有问题，直接透传给客户端，不浪费出口。
- 所有出口都不可用或耗尽时，返回 429/403/502 与可读错误信息。

---

## 6. 配置

配置文件为 JSON（默认读工作目录 `config.json`，不存在则用默认值），环境变量优先。

| 字段 | 环境变量 | 默认 | 说明 |
| --- | --- | --- | --- |
| `listen` | `OC2API_LISTEN` | `127.0.0.1:8787` | 本地监听地址 |
| `local_key` | `OC2API_LOCAL_KEY` | 空 | 非空时要求本地客户端带同名 Bearer |
| `lane` | `OC2API_LANE` | `v2` | `v2` = `/inference`，`v1` = `/zen` |
| `upstream` | `OC2API_UPSTREAM` | `https://opencode.ai/inference` | 上游地址；v1 通道会自动换成 `/zen` |
| `upstream_mode` | `OC2API_UPSTREAM_MODE` | `catalog` | `catalog` 用目录里的 per-model base URL；`fixed` 全部走 `upstream` |
| `api_key` | `OC2API_KEY` | 空 | 留空即匿名 `public`（只能跑 `*-free`） |
| `direct` | `OC2API_DIRECT` | `true` | 是否使用直连出口 |
| `proxies` | `OC2API_PROXIES` | `[]` | 出口列表，支持 `user:pass@host:port` |
| `free_only` | `OC2API_FREE_ONLY` | `true` | 只暴露零成本模型 |
| `cache_enabled` | `OC2API_CACHE` | `true` | 请求哈希缓存开关 |
| `cache_ttl_seconds` | `OC2API_CACHE_TTL_SECONDS` | `1800` | 缓存有效期 |
| `cache_max_entries` | `OC2API_CACHE_MAX_ENTRIES` | `500` | 缓存条数上限 |
| `catalog_refresh_minutes` | - | `10` | 模型目录刷新间隔 |
| `request_timeout_seconds` | `OC2API_TIMEOUT_SECONDS` | `0` | 单次上游超时，0 为不限 |
| `max_attempts` | `OC2API_MAX_ATTEMPTS` | `0` | 每次请求最多尝试的出口数，0 为全部 |
| `block_seconds` | - | `900` | FreeTierError 出口冷却 |
| `quota_seconds` | - | `60` | 429 出口冷却 |
| `probe_on_start` | `OC2API_PROBE` | `true` | 启动时用 16 token 探测每个免费模型，剔除已下线的 |
| `probe_concurrency` | `OC2API_PROBE_CONCURRENCY` | `1` | 探测并发数 |
| `probe_timeout_seconds` | - | `30` | 单次探测超时 |
| `probe_interval_ms` | - | `700` | 探测间隔，避免触发限流 |
| `reasoning_effort` | `OC2API_REASONING_EFFORT` | 空 | 默认注入的 `reasoning_effort`（`low`/`high`…） |
| `debug` | `OC2API_DEBUG` | `false` | 打印每次上游尝试 |

---

## 7. 部署（Docker）

`deploy/` 下是可直接使用的镜像与编排文件：

```bash
cd deploy
cp ../config.example.json config.json   # 按需修改 listen/local_key
docker build -t oc2api:local .
docker run -d --name oc2api --restart always \
  -p 3334:3334 \
  -v "$PWD/config.json:/app/config.json:ro" \
  --read-only --tmpfs /tmp \
  oc2api:local
```

容器只监听内部端口，健康检查为 `GET /healthz`。若需要被其它容器按固定域名访问（例如 AxonHub 的 `http://muse:3334/v1`），把它接入同一网络并加别名即可，无需改上游配置：

```yaml
networks:
  axonhub-net:
    external: true
    name: axonhub_axonhub-network
```

```yaml
    networks:
      axonhub-net:
        aliases:
          - muse
```

---

## 8. 已知限制

- **只支持匿名免费层**：`Bearer public` 只能调用 `*-free` 模型；付费模型需要真实 `oc_sk_` key（当前实现按你的选择未接入）。
- **免费额度是硬限制**：同一出口额度用尽后返回 429，只能换出口或等额度重置。网关无法绕过额度，只保证不因指纹问题被 403。
- **不支持 Gemini**：目录里少数模型走 Google 协议，入口会返回 501。
- **无状态**：不存储对话历史，`previous_response_id` 不生效，客户端需自行回放 `input`。
- 缓存只存在内存中。

---

## 9. 目录结构

```
oc2api/
  main.go                     启动、配置、优雅退出
  internal/config/            配置加载与环境变量覆盖
  internal/zen/               canonical session、指纹头、5 个核心工具、请求整形
  internal/catalog/           models.opencode.ai 目录与协议分派
  internal/api/               Responses 入口解析、SSE 输出、Chat/Anthropic/Responses 转换
  internal/gateway/           出口池、重试、错误分类
  internal/cache/             请求哈希缓存
  internal/server/            HTTP 路由与编排
```

核心工具的描述与 JSON Schema 取自 OpenCode 客户端（MIT），用于让上游把请求识别为真实 agent 流量。

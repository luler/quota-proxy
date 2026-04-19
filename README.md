# 接口请求配额中间件

一个独立的 HTTP 中间件服务，作为反向代理部署在客户端与后端之间，实现按访问主体限制时间窗口内成功访问次数的功能。

## 功能特性

- **路径分规则限流**：支持不同路径配置不同时间窗口和成功次数上限
- **请求内容匹配**：支持按 Query/Form、JSON Body、请求头内容进行包含匹配
- **并发安全**：通过 Redis Lua 脚本保证并发场景下不会超发
- **灵活的身份识别**：支持按 `identity.extractors` 顺序提取身份，并支持 IP 回退
- **多种成功判定**：支持 HTTP 状态码或 JSON 字段判定
- **SSE 支持**：支持流式响应转发，并在首个有效事件后确认成功计次
- **自定义超限响应**：支持默认 JSON、纯文本、空响应体、JSON 文本响应
- **自动过期**：配额数据会在当前 window/window_count 对应的时间窗口结束后自动过期
- **容错处理**：支持 fail-open 和 fail-close 模式
- **管理接口**：提供查询和重置配额的接口

## 架构

```
Client -> Quota Middleware -> Backend
```

中间件作为反向代理，拦截请求并进行配额检查后转发到上游服务。

## 快速开始

### 使用 Docker Compose

```bash
# 克隆项目
git clone https://github.com/luler/quota-proxy.git
cd quota-proxy

# 复制配置文件
cp config.yaml.example config.yaml

# 启动服务
docker-compose up -d
```

### 本地运行

```bash
# 安装依赖
go mod tidy

# 运行
go run main.go serve
```

## 配置说明

配置文件 `config.yaml.example`：

```yaml
# 服务配置
server:
  port: 3000
  read_timeout: 10s          # 读取请求头的最大时间，防 slowloris 攻击
  idle_timeout: 120s         # keep-alive 空闲连接回收时间
  max_body_size: 104857600   # 请求/响应 body 最大字节数 (100MB)

# 上游服务配置
upstream:
  target: http://localhost:8080  # 后端服务地址
  response_timeout: 120s         # 等待上游返回响应头的最大时间（LLM 推理排队可能较长）

# Redis 配置
redis:
  addr: redis:6379
  password: ""
  db: 0

# 访问主体识别配置
identity:
  strategy: header_priority
  extractors:
    - source: header
      key: Cookie
      regex: '(^|;\\s*)member_id=([^;]+)'
      group: 2
      name: member_id
    - source: header
      key: X-User-Id
      name: X-User-Id
  fallback_to_ip: true

# 配额配置
quota:
  enabled: true
  timezone: Asia/Shanghai
  exclude_paths:
    - /health
    - /metrics
  fail_open: true
  rules:
    - name: chat-daily
      window: day
      window_count: 1
      success_limit: 2
      include_paths:
        - /api/core/chat/**
      request_match:
        query_form:
          include:
            - "(^|&)foo=bar(&|$)"
          exclude:
            - "(^|&)debug=true(&|$)"
        json_body:
          include:
            - '"model":"coder-model-aiproxy"'
        headers:
          include:
            - "(^|\\n)x-user-id:vip-user($|\\n)"
      quota_exceeded_body: 请求过于频繁，请稍后再试

    - name: other-api-hourly
      window: hour
      window_count: 1
      success_limit: 20
      include_paths:
        - /api/other/**
      quota_exceeded_body: ""

# 管理面板配置
admin:
  api_key: ""
```

### 管理面板鉴权说明

- `admin.api_key` 默认值是空字符串 `""`
- 为空时表示未启用管理鉴权，`/__admin/ui` 和其他 `/__admin` 接口可直接访问
- 设置为非空后，`/__admin/ui` 会先进入登录页，其他 `/__admin` 接口需要携带 `X-API-Key`
- 可直接访问 `GET /__admin/ui?api_key=你的密钥`，页面会自动带入并尝试登录
- 前端登录成功后会把 API Key 保存在浏览器本地，后续请求自动附带 `X-API-Key`

### identity.extractors 说明

- `strategy` 支持两种：
  - `header_priority`（默认）：按 `extractors` 顺序依次尝试，命中第一个后立即作为 identity
  - `merge_all`：遍历所有 `extractors`，把命中结果按 `name:value` 用 `|` 拼接成**单一标识**（用于多维联合分组，例如 `app_id:myapp|user_id:alice`）；未命中的 extractor 会贡献空值（如 `app_id:myapp|user_id:`）
- 每条 `extractor` 描述一个取值来源：
  - `source`：`header`（默认，可省略）/ `query` / `cookie` / `ip`
  - `key`：参数名；`source=header` 时为 header 名，`source=query` 时为 query 参数名，`source=cookie` 时为 cookie 名；`source=ip` 时可留空
  - `regex + group`：可选，对取到的原始值再做一次 regex 抽取（group=0 表示整体）
  - `name`：最终 identity 中的字段名
- 提取后的 identity 格式保持为 `type:value`，例如 `member_id:abc123`、`X-User-Id:user123`；merge_all 下为 `name1:v1|name2:v2`
- 所有 extractor 都未命中时，若 `fallback_to_ip: true`，则回退为 `ip:<client-ip>`

### 路径规则说明

- `quota.rules` 按顺序匹配
- 请求命中第一个规则后，即使用该规则的 `window`、`window_count` 和 `success_limit`
- `window_count` 默认值为 `1`，必须是大于等于 `1` 的整数
- 例如：`window: minute + window_count: 5` 表示 5 分钟窗口；`window: day + window_count: 7` 表示 7 天窗口
- `exclude_paths` 优先级高于 `rules`
- 不命中任何规则的请求，不做配额检查，直接转发
- 不同规则会使用不同 Redis key，相互隔离计数
- `include_paths` 命中后，若配置了 `request_match`，则继续按请求内容做 AND 匹配
- `request_match.query_form`：先把 query 参数和 form 参数规范化为类似 `a=1&b=2` 的字符串，再用 regex 做 include/exclude 匹配
- `request_match.json_body`：先把 JSON 请求体规范化为紧凑 JSON 字符串，再用 regex 做 include/exclude 匹配
- `request_match.headers`：先把请求头规范化为按行拼接的 `name:value` 文本，再用 regex 做 include/exclude 匹配
- 每个域内：`include` 非空时要求至少命中一个 regex；`exclude` 任一命中则该域失败
- 优先级上可理解为：`exclude` 高于 `include`，即使已经命中 `include`，只要再命中任一 `exclude`，最终仍然失败
- `request_match` 只支持新的 `query_form/json_body/headers + include/exclude` 结构

### request_match 示例

```yaml
request_match:
  query_form:
    include:
      - '(^|&)foo=bar(&|$)'
    exclude:
      - '(^|&)debug=true(&|$)'
  json_body:
    include:
      - '"model":"coder-model-aiproxy"'
  headers:
    include:
      - '(^|\n)x-user-id:vip-user($|\n)'
```

常见写法：

- 模糊匹配某个 query/form 片段：`foo`
- 精确匹配参数对：`(^|&)foo=bar(&|$)`
- 精确匹配 header 行：`(^|\n)x-user-id:vip-user($|\n)`

### 配额超限自定义返回

- 不配置 `quota_exceeded_body`：返回默认 429 JSON
- `quota_exceeded_body: ""`：返回 429 且响应体为空
- `quota_exceeded_body` 为普通文本：返回 `text/plain`
- `quota_exceeded_body` 为合法 JSON 文本：自动按 JSON 返回

### 环境变量覆盖

所有配置项可通过环境变量覆盖，格式为 `QUOTA_<SECTION>_<KEY>`：

```bash
export QUOTA_SERVER_PORT=9090
export QUOTA_REDIS_ADDR=localhost:6379
```

## API 接口

### 管理面板访问

- 管理页面入口：`GET /__admin/ui`
- 未配置 `admin.api_key` 时可直接访问
- 配置了 `admin.api_key` 后，可先打开 `GET /__admin/ui?api_key=你的密钥` 让页面自动尝试登录
- 受保护的管理接口需要在请求头中带上 `X-API-Key: <admin.api_key>`

### 管理密钥校验

```
POST /__admin/login
Content-Type: application/json

{
  "api_key": "your-admin-key"
}
```

响应：

```json
{
  "code": 200,
  "message": "验证通过"
}
```

### 健康检查

```
GET /health
```

响应：

```json
{
  "status": "ok"
}
```

### 查询配额状态

- 传 `rule`：返回单条规则状态
- 不传 `rule`：返回该 identity 的所有规则状态

```
GET /__admin/quota?identity=xxx&rule=chat-daily
```

响应：

```json
{
  "identity": "X-User-Id:user123",
  "rule": "chat-daily",
  "window": "day",
  "window_count": 1,
  "period_key": "2026-03-18",
  "success_count": 1,
  "pending_count": 0,
  "limit": 2,
  "remaining": 1,
  "rules": [
    "chat-daily",
    "other-api-hourly"
  ]
}
```

不传 `rule` 示例：

```
GET /__admin/quota?identity=xxx
```

```json
{
  "identity": "X-User-Id:user123",
  "quotas": [
    {
      "rule_name": "chat-daily",
      "success_count": 1,
      "pending_count": 0,
      "limit": 2,
      "remaining": 1,
      "window": "day",
      "window_count": 1,
      "period_key": "2026-03-18"
    },
    {
      "rule_name": "other-api-hourly",
      "success_count": 0,
      "pending_count": 0,
      "limit": 20,
      "remaining": 20,
      "window": "hour",
      "window_count": 1,
      "period_key": "2026-03-18-19"
    }
  ],
  "rules": [
    "chat-daily",
    "other-api-hourly"
  ]
}
```

### 重置配额

- 传 `rule`：只重置该规则
- 不传 `rule`：重置该 identity 的所有规则

```
POST /__admin/quota/reset
Content-Type: application/json

{
  "identity": "X-User-Id:user123",
  "rule": "chat-daily"
}
```

响应：

```json
{
  "code": 200,
  "message": "配额已重置",
  "identity": "X-User-Id:user123",
  "rule": "chat-daily"
}
```

不传 `rule` 示例：

```json
{
  "identity": "X-User-Id:user123"
}
```

```json
{
  "code": 200,
  "message": "所有配额已重置",
  "identity": "X-User-Id:user123",
  "rules": [
    "chat-daily",
    "other-api-hourly"
  ]
}
```

## 配额超限响应

默认情况下，当配额用尽时，返回：

```json
{
  "code": 42901,
  "message": "当前时间窗口内成功访问次数已达上限",
  "limit": 2,
  "rule": "chat-daily"
}
```

也可以通过 `quota_exceeded_body` 自定义：

```yaml
quota_exceeded_body: 请求过于频繁，请稍后再试
```

```yaml
quota_exceeded_body: ""
```

```yaml
quota_exceeded_body: '{"code":42901,"message":"当前访问过于频繁"}'
```

## 核心逻辑

### 并发安全机制

采用 "预占名额 + 成功确认/失败回滚" 机制：

1. 请求进入时，检查 `success + pending < limit`
2. 满足条件则 `pending++`（预占）
3. 转发请求到上游
4. 成功则 `pending--, success++`
5. 失败则 `pending--`（回滚）

所有操作通过 Redis Lua 脚本保证原子性。

### 路径规则匹配

1. 先检查 `exclude_paths`
2. 再按配置顺序匹配 `quota.rules[*].include_paths`
3. 若配置了 `request_match`，则继续检测 Query/Form、JSON Body、请求头内容
4. 路径和请求内容条件同时命中后，才使用该规则的窗口和次数限制
5. 未命中规则则直接透传，不做配额检查

### 身份识别优先级

按配置的 header 顺序检查，取第一个非空值：

1. `X-User-Id` → 使用 `X-User-Id:<value>` 作为主体标识
2. 都不存在 → 回退到 `ip:<client_ip>`

### 成功判定模式

#### HTTP 状态码模式

- HTTP 2xx 视为成功
- 其他状态码视为失败

#### JSON 字段模式

- 可配置检查 JSON 响应中的特定字段
- 例如：`code == 0` 视为成功

#### SSE 流式模式

- 当上游响应头 `Content-Type` 包含 `text/event-stream` 时，代理按流式转发，不再读取完整响应体
- SSE 不使用 `success_rule` 的 `json_field` 判定
- SSE 也会先按 `success_rule.require_http_2xx` 检查状态码：为 `true` 时要求 HTTP 2xx；为 `false` 时要求状态码小于 400
- 在状态码满足要求的前提下，SSE 在首个有效事件帧（包含 `data:`、`event:`、`id:` 或 `retry:` 字段）成功写给客户端并 flush 后计为
  1 次成功访问
- 若在首个事件前上游失败、读取失败或客户端写出失败，则回滚本次预占，不扣次数

### 超时与安全机制

请求全生命周期内各超时配置的作用范围：

```
客户端 ──TCP──> 代理服务器 ──TCP──> 上游服务
        │                  │
        │  read_timeout    │  response_timeout
        │  (请求头读取)     │  (等待响应头)
        │                  │
        │  idle_timeout    │  SSE 流传输：无超时限制
        │  (空闲连接回收)   │
        │                  │
        │  max_body_size：请求/响应 body 大小上限
        │
```

| 配置项 | 作用层 | 说明 |
|--------|--------|------|
| `server.read_timeout` | 入站（客户端→代理） | 读取请求头的最大时间，防止 slowloris 慢速攻击 |
| `server.idle_timeout` | 入站（客户端→代理） | keep-alive 连接在两次请求之间的最大空闲时间 |
| `server.max_body_size` | 入站 + 出站 | 请求/响应 body 的最大字节数，防止超大 body 导致内存溢出 |
| `upstream.response_timeout` | 出站（代理→上游） | 等待上游返回响应头的最大时间，LLM 推理排队场景建议设为 120s 或更高 |

注意事项：
- SSE 流式传输不受任何写超时限制，流可以持续数分钟
- `idle_timeout` 和 `response_timeout` 不冲突：前者管入站空闲连接回收，后者管出站等待上游响应

### TTL 自动过期

Redis key 会在当前时间窗口结束时自动过期：

- `window_count: 1` 时，行为与当前 `minute` / `hour` / `day` 一致
- `window: minute`：每 `window_count` 分钟一个窗口，到下一个窗口边界过期
- `window: hour`：每 `window_count` 小时一个窗口，到下一个窗口边界过期
- `window: day`：每 `window_count` 天一个窗口，到下一个窗口边界过期

## 日志

日志文件位于 `runtime/logs/app.log`，包含：

- 请求路径、方法、状态码
- 命中的规则名
- 访问主体标识
- 是否成功判定
- 请求耗时
- 配额拦截记录
- 错误日志

## 开发

```bash
# 编译
go build -o main .

# 运行
./main serve
```

## 测试场景

| 场景                    | 预期行为                   |
|-----------------------|------------------------|
| 不同路径命中不同规则            | 使用各自 window/limit 独立计数 |
| 未达上限时请求               | 正常转发，成功计数+1            |
| 失败请求                  | 不增加成功计数                |
| 达到上限后请求               | 返回 429，不转发             |
| 并发请求                  | 最终成功数不超过限制             |
| 下一窗口请求                | 配额重置                   |
| Redis 异常 (fail-open)  | 请求正常转发                 |
| Redis 异常 (fail-close) | 返回 503                 |


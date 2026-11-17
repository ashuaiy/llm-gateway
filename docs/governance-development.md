# LLM 网关治理：实现、参考与验证

更新于 2026-11-17。预算预占默认关闭，适用于经过验证的 OpenAI 兼容纯文本聊天。新增账务生命周期、故障恢复与管理核对、安全策略和 Key 级限流；测试见 [实测报告](governance-performance.md) 和 [原始 JSON](governance-load-results.json)。

## 成熟项目参考

实现独立编写，不复制上游实现文件。参考固定提交，避免主分支变化后描述失真：

| 来源 | 核对内容 | 本项目采用的机制及差异 |
| --- | --- | --- |
| BricksLLM `c7d80a6` [PII 策略](https://github.com/bricks-cloud/BricksLLM/blob/c7d80a62c2de95733c1e6c95a4eaedc50afaad75/internal/policy/policy.go) | 检测结果与 block/warn/redact 动作分离，redact 单向替换 | 本地规则检测、默认单向掩码；另提供请求内占位符还原，不向第三方 PII 服务发送原文 |
| BricksLLM [消费者](https://github.com/bricks-cloud/BricksLLM/blob/c7d80a62c2de95733c1e6c95a4eaedc50afaad75/internal/message/consumer.go) | 多 goroutine 持续消费内存 channel | 持续多消费者，但队列改为 Redis 持久状态，增加租约、领取者令牌、SQL 提交后确认 |
| BricksLLM [Redis 计数器](https://github.com/bricks-cloud/BricksLLM/blob/c7d80a62c2de95733c1e6c95a4eaedc50afaad75/internal/storage/redis/cache.go) | 时间窗口计数与过期 | 限流计数有 TTL；账务预占必须先落库才开始过期，不能按普通缓存处理 |
| new-api `ce8fc49` [额度预扣](https://github.com/QuantumNous/new-api/blob/ce8fc49e4b1a874aa917dce4f1a8416c52ec8ec0/model/quota_reserve.go)、[计费会话](https://github.com/QuantumNous/new-api/blob/ce8fc49e4b1a874aa917dce4f1a8416c52ec8ec0/service/billing_session.go) | Lua 原子预扣、结算/退款生命周期 | 整数金额、冻结单价；额外持久化 unknown 和核对证据。Redis 故障拒绝新预占，不降级到滞后的 SQL 余额 |
| new-api [AC 关键词](https://github.com/QuantumNous/new-api/blob/ce8fc49e4b1a874aa917dce4f1a8416c52ec8ec0/pkg/jsplugin/keywords.go)、[过滤插件](https://github.com/QuantumNous/new-api/blob/ce8fc49e4b1a874aa917dce4f1a8416c52ec8ec0/plugins/tasks/request-filter/plugin.js) | AC、多规则、流式保留窗口 | 独立 rune AC，保留最长关键词长度减一的尾部。任意正则在完整回复上判断，固定窗口不能保证任意正则正确 |
| new-api [限流 Lua](https://github.com/QuantumNous/new-api/blob/ce8fc49e4b1a874aa917dce4f1a8416c52ec8ec0/common/limiter/lua/rate_limit.lua) | Redis 时间与原子准入 | 一次 Lua 联合检查 RPM、TPM 与并发流租约；这里采用固定窗口，不宣称滑动窗口平滑限流 |

这些项目提供机制参考，不代表任意供应商断连后都能返回完整账单。上游许可证见 [BricksLLM](https://github.com/bricks-cloud/BricksLLM/blob/c7d80a62c2de95733c1e6c95a4eaedc50afaad75/LICENSE)、[new-api](https://github.com/QuantumNous/new-api/blob/ce8fc49e4b1a874aa917dce4f1a8416c52ec8ec0/LICENSE)。

## 请求路径与配置

```text
Key 鉴权 → 输入校验 → 注入筛查/PII 脱敏 → RPM/TPM/并发流准入
 → 冻结单价/原子预占 → 上游 → 先采集 usage，再检查/写 SSE
 → Redis 结算 + outbox → 多消费者租约领取
 → SQL 唯一账单 + 预算投影事务 → Redis 确认 + hash TTL
```

安全拦截在预占之前。输出被拦截但费用已产生时仍结算，旧累加器不会二次扣费。预算模式禁用响应缓存、自动重试和参数覆盖。

与已有配置合并：

```yaml
budget:
  reservationEnabled: true
  reservationRedis:
    addr: localhost:6379
    password: ""
    db: 1
  reservationDefaultMaxTokens: 1024
  reservationMaxOutputTokens: 8192
  reservationRetention: 24h
  reconciliationStaleAfter: 10m
  workerConcurrency: 4
  deliveryBatchSize: 100
gateway:
  guardrailsEnabled: true
  defaultConfig:
    guardrails:
      - type: request_policy
        parameters:
          piiAction: redact
          piiMode: redact
          idCardValidation: format
          injectionAction: block
          injectionKeywords: [忽略之前所有设定, 输出系统提示词]
          injectionPatterns: ['(?i)ignore\s+(all\s+)?previous\s+instructions']
      - type: response_policy
        parameters:
          keywords: [内部访问口令]
          patterns: []
          blockPII: false
```

消费者 1–32 个、批大小 1–1000、终态保留至少 1 分钟。stale 时间至少 3 分钟且大于最大请求时间加 1 分钟。预算 Redis 独立于响应缓存，需要专用 DB、AOF 和 `noeviction`。所有实例共享 Redis 和 SQL；启用前停止旧流量并排空旧队列，不能混跑新旧计费模式。MySQL 使用 `database.driver: mysql` 与 Go MySQL DSN；SQLite/PostgreSQL 保持兼容，故障测试实际覆盖 MySQL 8.4。

## 安全策略

- PII：大陆手机、邮箱、身份证常见格式、`sk-`/AWS Access Key ID，不保证识别任意密钥。多轮文本、prompt、文本 embedding input 和函数 arguments 会检查；图片/音频不读取。
- `piiAction`：redact/block/audit/off；`injectionAction`：block/audit/off。AC 使用 `injectionKeywords`，正则同时保留。非法规则拒绝启动；词库匹配不等于语义防越狱。
- `idCardValidation: checksum` 启用校验位；默认 `format` 保守脱敏，避免格式像身份证的无效号码漏出。校验位不代表身份有效。
- `piiMode: tokenize` 可选回复还原：每请求随机占位符，映射仅在内存中，**API 密钥永不还原**。该请求禁用缓存及幂等响应重放。
- tokenize 缓冲整个 JSON/SSE 后还原，**不保留逐 token 到达时间**。输入、待还原正文、还原结果均设 1 MiB 正文边界，非流式读取在读入前限制；增量替换在扩容前检查，防止重复占位符放大内存。SSE 跨片占位符合并到每个 choice 的第一个文本事件，保留 usage/DONE。
- 还原后再次本地执行 `response_policy`，`blockPII: true` 同样阻止还原出的 PII；不将原文发送给外部审查服务。
- SSE 关键词使用尾部窗口；任意正则和 blockPII 最多缓冲 1 MiB，在 DONE/正常 EOF 判断，避免 `secret$` 错拦截分片 `secret` + `ary`。无 regex 的关键词策略可持续输出已通过的完整帧。
- 输出检查仅支持单 choice 文本 delta；未知 delta、工具/音频/推理字段拒绝。异常结束不释放未检查尾部。SSE 已写出时用 `event: error` 通知，无法改写已发送的 HTTP 200。
- 审计只记动作、规则 ID、trace ID 和 Key ID；用量日志只看到占位符，映射不落 SQL。

## 计费范围与恢复

支持 `/v1/chat/completions`（含 stream）和 `/v1/chat/completions/stream`，允许 GET `/v1/models`；其他收费端点返回 400，旧预算重置入口返回 409。只允许 OpenAI 兼容适配器或本地 deterministic：单候选、纯文本、无工具/response_format，使用 `max_tokens`。需要实际 provider 类型 + model 的准确 USD 价格，单位沿用原项目的 **美分/Token**；缺价格不按免费放行。

金额为 `10^-9 USD` 整数，冻结单价向上取整，Lua 整数不超过 `2^53-1`。`budget_ledgers.actual_units` 是精确账本，`virtual_keys.budget_used` 是异步投影。预占输入按 JSON 字节数加消息开销估计，**不是任意模型的严格 Token 上界**。完整 usage 须有非负且一致的 prompt/completion/total，SSE 只认 `choices: []` 的末尾用量事件。

| 事件 | 行为 |
| --- | --- |
| 额度不足 | 原子拒绝，未调用上游 |
| 完整 usage 已知且不超预占 | 原子结算、退差额、入 outbox |
| 明确未派发 | 全额释放、入 outbox |
| 断连前已收到完整 usage | 使用已采集 usage 结算，不依赖客户端写入成功 |
| 断连/网络错误/无完整 usage | unknown 保留预占，进入管理核对入口 |
| 实际费用超预占 | 保存 reported_settlement 与 actual_exceeds_reserved，等待有证据的补差 |
| 网关被杀 | reserved/unknown 留存，不根据时间猜测免费 |
| SQL 故障 | 任务保留并退避重试，Redis 余额独立 |
| SQL 提交后被杀、未确认 | 租约到期重投，唯一账单阻止二次扣费 |
| 落库并确认 | Lua 原子移除 pending/owner 并设置 24h TTL，重试不延长 TTL |
| 旧版本无 TTL 的终态 | SCAN 分批确认 SQL 账单存在且不在 pending 后补 TTL |

ZSET 到期时间 + owner token 实现默认 30 秒领取租约，多进程互斥领取，旧持有者不能确认新任务。处理一批后立即继续；空队列等待 100 ms，失败退避。MySQL 用 INSERT 唯一键冲突作为幂等门，避免 `clientFoundRows=true` 下 UPSERT 返回 1 引起二次计费。

账务 Lua 共用 hash tag，仍是单 Redis 分片。预占 ID 由服务端 UUID 生成。已过期终态可从 SQL 查询。未知和未落库记录不能自动过期，长期积压仍需运维核对。

### 核对入口

前端 **Billing review** 或以下 JWT 接口，仅 super_admin/tenant_admin，租户隔离：

```text
GET  /api/v1/billing/reservations?offset=0&limit=50
GET  /api/v1/billing/reservations/:id
POST /api/v1/billing/reservations/:id/reconcile
```

```json
{"resolution_id":"unique-audit-id","action":"settle","input_tokens":15,"output_tokens":20,"reason":"provider_export_verified","evidence":"provider request ID and billing export reference"}
```

reason/evidence 必填，actor 从 JWT 生成；相同完整内容重试幂等，冲突 409。release 要求 `reason: confirmed_not_billed`、tokens 为 0。unknown 可核对；reserved 超过 stale 时间才能核对。经核实的超额可以补记真实费用，余额据实变负并阻止新请求。核对证据和原始超额 usage 保留到 SQL，hash 过期后仍能查询和判断幂等。

列表对全局 active 有界扫描，租户过滤后可能出现空页，需继续翻页；集合变化时按 ID 查询做最终确认。监控指标：`llmgw_billing_pending`、`llmgw_billing_unresolved`、`llmgw_billing_oldest_unresolved_seconds`。多实例反映同一集合，聚合用 max，不累加。

### 限流

Virtual Keys 页面/API 支持 rate_limit、rate_limit_window、token_limit_per_minute、max_concurrent_streams，0 为不限；新建未指定 RPM 默认 60。限流随预算预占模式开启。RPM 固定窗口；TPM 在准入分钟预扣、同窗口有权威 usage 时校准，跨窗口不改新窗口，无 usage 保留原估算。这是准入 Token 配额，不是逐 token 输出速率。

并发流独立 30 秒租约，每 10 秒续约；续约失败取消上游，结束或死亡到期释放流槽，不因此退款。使用 Redis TIME 统一时间；超限 429，Redis 故障 503。

## 复现测试

使用一次性 Redis/MySQL，不连接业务数据库：

```bash
docker compose -f docker-compose.governance.yml run --rm verify
docker compose -f docker-compose.governance.yml run --rm -e LLM_GATEWAY_RUN_LOAD=1 -e LLM_GATEWAY_BENCH_REPORT=/workspace/docs/governance-load-results.json verify
docker compose -f docker-compose.governance.yml down
```

verify 跑全量 Go 测试、关键模块 race detector、Go 构建。原生方式需要 Go/CGO，设置 `LLM_GATEWAY_TEST_REDIS` 和 `LLM_GATEWAY_TEST_MYSQL`；未设置时真实集成测试会 **skip**，不算完整验收。前端：`npm ci`、`npm run build`。

| 文件 | 证据 |
| --- | --- |
| internal/billing/store_test.go、service_test.go | 并发预算、整数边界、usage、价格快照、重复结算 |
| worker_test.go、delivery_test.go | TTL、旧状态清理、连续排空、多消费者、租约 fencing |
| crash_test.go、mysql_test.go | 真子进程强杀四阶段、SQL 中断恢复、MySQL 重复投递不二次扣费 |
| internal/limits/limits_test.go | 双实例原子准入、TPM 幂等校准、并发流回收 |
| pkg/guardrail/*policy_test.go、pii_restore_test.go | AC、身份证校验位、跨片输出、regex 锚点、还原后复查与内存放大边界 |
| pkg/proxy/billing_test.go | SSE 完整 usage、断连前采集、CRLF、有界读取 |
| tests/governance/reconcile_test.go | 真 JWT/HTTP/Redis/SQL 的租户隔离和 TTL 后核对幂等 |
| tests/governance/load_test.go | 双 HTTP 网关 + Redis + MySQL 的透支对比与短时性能 |

仍未实现供应商自动对账、Redis 全量丢失/主从切换的强恢复、周期预算重置、在线新旧切换、多媒体/工具计费。Redis 全丢后必须停流核对，不能用滞后 SQL 投影直接重建。没有宣称所有断连都精确自动退款、生产高可用或 1 ms 延迟。

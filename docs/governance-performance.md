# 治理功能实测记录

2026-11-17，Windows 宿主机 Docker Linux/amd64、Go 1.26.4、12 个可见逻辑 CPU、Redis 7.4、MySQL 8.4。两个 httptest HTTP 网关实例在同一测试进程内，共用真实 Redis/SQL，不是两台独立机器。[原始 JSON](governance-load-results.json)；[复现命令](governance-development.md#复现测试)。

## 预算竞争对比

同一 Key，预算 10 USD，每请求输出 10 Token，10 美分/Token（每请求 1 USD），100 并发。模拟上游设置同步屏障，让所有请求完成准入判断后才产生账单，以确定性复现旧的先查后扣竞争。

| 模式 | 成功 | 预算拒绝 | 费用 | 透支 |
| --- | ---: | ---: | ---: | ---: |
| 原有后扣路径 | 100 | 0 | 100 USD | 90 USD |
| Lua 预占 | 10 | 90 | 10 USD | 0 USD |

新模式生成 10 条唯一 SQL 账单。屏障是正确性实验，不模拟生产分布，其 RPS 不用于性能比较。

## 短时端到端负载

开启请求策略、关键词流式输出检查、预占、限流组件（配额不限），两个网关各 4 个消费者。1000 请求、64 并发；本地假上游等待 10 ms 返回 SSE 内容、usage 和 DONE。

| 指标 | 实测 |
| --- | ---: |
| HTTP 完整响应吞吐 | 791.20 RPS |
| p50 / p95 / p99 | 75.90 / 109.47 / 119.54 ms |
| HTTP 成功 / 失败 | 1000 / 0 |
| 请求结束后 outbox 排空 | 4832.00 ms |
| 最终 SQL 账单 | 1000 条 |
| 最终费用 / 透支 | 1000 USD / 0 USD |

请求阶段约 1.26 秒，加排空约 6.10 秒，整体账单完成速率约 164 条/秒。已解除每秒只取 100 条的限制，MySQL 单笔事务仍是瓶颈。**791 RPS 不是持续账单写入容量**；短样本不证明长期稳定吞吐，不含外部 LLM 延迟或真实费用。短样本会受缓存及宿主机调度影响，不应以单次结果作为容量承诺。

## 故障证据

`TestKilledWorkerRecoversReservationAndCommittedLedger` 在 Linux 容器对真实子进程执行 Process.Kill，分别覆盖：仅完成预占、Redis 已结算、消费者已领取、SQL 提交但未确认。

预占阶段仍保留资金，核实测试上游未派发后由带证据的 release 释放；其余阶段由消费者重投恢复。每阶段断言单条账单及正确 SQL 余额。另测 SQL 连接关闭后的恢复、MySQL clientFoundRows=true 下重复投递。测试未模拟 Redis AOF 丢失、主从切换或整机断电。

## 本轮验收

全量 `go test ./... -count=1`、billing/limits/proxy/guardrail/middleware/governance 的 `go test -race`、Go 服务构建、Next.js 生产构建均通过。集成验收设置了真实 Redis/MySQL 地址；负载实验单独显式开启，避免普通测试运行产生或覆盖性能报告。测试未调用付费模型，原生 Windows Go 编译未作为验收依据。

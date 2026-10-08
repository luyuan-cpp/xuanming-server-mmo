# Round 14 — 1 Zone × 45k Robot Stress (2026-06-01)

Run directory: `robot/logs/stress-45k-z1-r14-20260601-093840`
Baseline: `docs/stress/prev-summary-round14.txt` (Round 13 post-fix)

## TL;DR

P0/P3 fixes 显著生效，P1/P2 待二次验证；**新瓶颈暴露**: data_service preload 在更高吞吐下回归。

| 指标 | Round 13 (baseline) | Round 14 | 变化 |
|---|---:|---:|---|
| EnterGame 成功 @ t18m | 73,507 | **84,706** | **+15%** ↑ |
| apply{ok} avg @ t18m | 417.7 ms | **66.5 ms** | **-84%** ↓ ✅ P0 生效 |
| preload{ok} avg @ t18m | 1.9 ms | 64.5 ms | +62.6 ms ⚠️ |
| preload_failed @ t18m | 0 | **3,663** | 回归 ❌ |
| cb_wait{fail} @ t18m | — | 5,510 ms | 命中 5s 超时 |
| Kafka 总 lag | 0 | 48 | 轻微堆积 |
| Robot "Connect failed" 行数（全程） | 509,781 | **47,643** | **-91%** ↓ ✅ P3 生效 |

## 各 Fix 验证

### ✅ P0 — `dispatchReleasePlayer` 全异步（成功）
- `apply_seconds` 平均 **417.7 ms → 66.5 ms（-84%）** ：EnterScene RPC 不再被 500ms gRPC sync 拖住
- 直接结果：吞吐 +15%（73k → 84k），消息率 robot 端 max_login 一度到 822ms（vs 13: 329ms），说明上游能更快推 robot
- scene_manager `release_player_total` 异步 outcome 已可在新 metric 中追踪

### ✅ P3 — muduo 重连日志限流（成功）
- robot.stderr `Connect failed` 行数 **509,781 → 47,643（-91%）**
- 后期排障日志可读性显著恢复

### ⚠️ P1 — gRPC `maxPollers` 2→8（无明显回归，效果待覆盖更高并发）
- 未发现 gRPC 拒绝/排队相关错误
- Round 14 仍未触发 P1 的瓶颈区，需要更高并发或下一轮 burst 场景验证

### ⚠️ P2 — `loginsession` Cleanup 降噪 + 新 metric（无回归）
- login 服务 stderr 干净（无 redis.Nil 噪音）
- 新 metric `loginsession_cleanup_miss_total` 已注册（promauto），需 prom snapshot 解析验证

## ❌ 新瓶颈：data_service preload 回归

| Stage | success | preload_failed | preload{ok} | cb_wait{ok} | cb_wait{fail} |
|---|---:|---:|---:|---:|---:|
| t2m  |  5,351 |     0 |  48.2 ms |    79 ms |   —     |
| t5m  | 17,835 |     0 |  37.4 ms |    86 ms |   —     |
| t10m | 43,530 | 1,241 |  49.0 ms |   168 ms | 5,493 ms |
| t15m | 71,154 | 2,490 |  56.0 ms |   226 ms | 5,498 ms |
| t18m | 84,706 | 3,663 |  64.5 ms |   275 ms | 5,494 ms |

**模式**：t5m 之前为 0 失败；之后失败逐分钟线性累积。`cb_wait{fail}=5.5s` 正好命中 dataloader 5s 回调超时。`kafka_send` 仅 8ms，所以瓶颈不在 Kafka 入口，而是 db service 出队→callback 的吞吐链路。

### 假设
1. **db service 处理速率上限被 P0 解放出的更高 EnterGame QPS 顶到**：之前 EnterScene 同步 500ms 隐式限了 EnterGame 速率（~70k/18m），P0 后吞吐 +15% 直接把 db service 推过临界点
2. **task_result_dispatcher Pub/Sub fanout** 在 cb_wait{ok} 也明显抬升（85→275ms），说明派发链路在排队
3. preload_failed 多发于 t10m+ 表明累积压力相关（不是冷启动 race）

### 下一步建议（Round 15 候选）
- **P5** db service consumer goroutine 池 / 批处理：把 db_task_zone_1 partition 消费速率从约 26k → ≥45k records / 18min（当前缺口 ~40%）
- **P6** dataloader 5s 回调超时上调到 8s 作为短期止血（不治本但避免业务级失败）
- **P7** task_result_dispatcher pub/sub 接收侧 worker 池：cb_wait{ok} 85→275ms 暗示 dispatcher 端处理跟不上 publish 速率

## 验证状态
- ✅ P0 巨幅生效（核心目标达成）
- ✅ P3 日志噪音清理
- ⚠️ P1/P2 无回归，但本轮压力未达到能触发它们的场景
- 🆕 暴露 db service preload 链路新瓶颈，需 Round 15 专项处理

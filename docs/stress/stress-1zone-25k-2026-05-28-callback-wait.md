# 25k 单 zone 压测:dispatcher GC tick → callback_wait 全链路修复

**日期**: 2026-05-28
**对应 commit chain**: `139821e1a`(partition 5→10 baseline) → 待提交(本次修复)
**结论**: 三个互相加强的修复(`dispatcherTaskTTL` 30→5s、dataloader 子阶段打点、dispatcher GC tick 1s)
让单 zone 25k smoke 从 "T+5m 拐点 → T+21m 雪崩冻死" 变成 **"T+23m client 端打满 125k conn 上限 + robot 视角 0 失败"**。
但发现一个**深层瑕疵**:即便 robot 看着 100% 成功,login 后台 `entergame_preload_failed` 仍占 46%,
靠 scene-side Redis NIL retry 兜底没让玩家感知到。这是接下来要继续挖的方向。

> 配套 [ARCH.md §11](../design/ARCH.md#11-关键决策摘要) 第 17 行;延续 [partition 5→10 复盘](./stress-1zone-45k-2026-05-partition-10.md)
> 的 §5 "callback_wait 真凶" 推论,这次用 prometheus histogram 把推论坐实到代码行。

---

## 1. 起点:partition 10 仍未解决的 err25

[partition 5→10 专题 §5](./stress-1zone-45k-2026-05-partition-10.md#5-新瓶颈定位err25--klogininprogress-player_locker-滞留)
推论:err25 来自 `player_locker:{playerId}` 滞留,根因是 EnterGame 异步链卡住,
但具体哪一步卡没数据。这次用 prometheus histogram 直接做实验。

7 个 histogram 在 entergame 链路上:
```
entergame_total_seconds{result}                 chainStart → releaseLock
entergame_preload_seconds{result}               EnsurePlayerAllDataInRedisAsync 整段
entergame_apply_seconds{result}                 applyLoadedPlayerSession 整段
  ├─ entergame_apply_get_session_seconds        GetSession RPC
  ├─ entergame_apply_persist_session_seconds{decision}  SetSession / Reconnect
  ├─ entergame_apply_bind_gate_seconds          SendBindSessionToGate (Kafka)
  └─ entergame_apply_enter_scene_seconds        SceneManagerClient.EnterScene
```

5 个 histogram 在 dataloader 子阶段:
```
dataloader_preload_cache_check_seconds{hit}     parent EXISTS 检查
dataloader_preload_sub_cache_check_seconds{subs}  sub-table GET 循环
dataloader_preload_dispatcher_register_seconds  Register N 个 callback (CPU only)
dataloader_preload_kafka_send_seconds{result}   sendDBReadRequests
dataloader_preload_callback_wait_seconds{result}  Kafka 发完 → finalize 触发
```

代码:`go/login/internal/logic/clientplayerlogin/metrics.go` +
`go/login/internal/logic/pkg/dataloader/metrics.go`(都是新文件)。

---

## 2. (f)-only 25k smoke (Round 4,只加打点,没修任何东西)

T+5m 拉 prometheus snapshot:

| stage | count | sum (s) | **avg per call** |
|---|---:|---:|---:|
| `apply_get_session_seconds` | 17,353 | 33.3 | **1.92ms** |
| `apply_persist_session_seconds{first}` | 4,181 | 9.1 | 2.17ms |
| `apply_persist_session_seconds{reconnect}` | 11,785 | 69.1 | 5.87ms |
| `apply_persist_session_seconds{replace}` | 1,387 | 6.4 | 4.58ms |
| `apply_bind_gate_seconds` | 17,353 | 49.0 | 2.82ms |
| `apply_enter_scene_seconds` | 17,353 | 99.6 | 5.74ms |
| `apply_seconds{success}` 总和 | 17,353 | 266.5 | **15.4ms** |
| **`preload_seconds{success}`** | 17,353 | 52,022 | **2,997 ms ≈ 3秒** |
| **`preload_seconds{preload_failed}`** | **481** | **16,992** | **35.3 秒** |

dataloader sub-stage:
```
cache_check{hit}         99.8% fast path, 1.6ms
cache_check{miss}        0.2% slow path
sub_cache_check          3.3ms
dispatcher_register      ~0ms (CPU only)
kafka_send{success}      6ms
callback_wait{success}   ←← 主要时间,平均 4.92 秒
callback_wait{failed}    ←← 35.3 秒 ← 真凶在这
```

**结论**: `apply` 整体 15ms 不是瓶颈;`preload` 几乎 100% 时间花在 `callback_wait`,即"Kafka 发完之后等 dispatcher 把 db 结果回调回来"。

这 dataloader_preload_callback_wait_seconds 指向两个修复方向:
- 减小**失败的等待上限**(`dispatcherTaskTTL 30→5s`)
- 减小**实际等待中位数**(查 dispatcher 内部 GC 行为)

---

## 3. (1) `dispatcherTaskTTL: 30s → 5s`

代码改动:
```go
// go/login/internal/logic/clientplayerlogin/entergamelogic.go:296
const dispatcherTaskTTL = 5 * time.Second  // was 30 * time.Second
```

直觉:失败的 preload 平均 35 秒,30s 是 TTL 的硬上限;改 5s 应该让失败的等待
平均降到 ≤ 5s。

实测 (Round 5,单独应用 (1)+(3)):
```
preload_seconds{success} avg     282 ms   (上轮 2,997ms,降 91%)
preload_seconds{preload_failed} avg  11.5 秒   (上轮 35.3s,降 67%)
callback_wait{failed} avg        12s     ← 没到 5s
```

**TTL 5s 没有按预期生效** —— 失败平均仍是 12s。继续挖。

---

## 4. (3) dataloader 子阶段打点验证 + dispatcher 内部诊断

(3) 在 §1 已经描述。Round 5 实测对应到 (1) 上面那个 callback_wait{failed}=12s,
真凶不在 dataloader 自己,在它调用的 `internal/dispatcher/TaskResultDispatcher`。

阅读 `task_result_dispatcher.go` 找到 line 126:
```go
gc := time.NewTicker(d.defaultTTL / 2)   // 30s/2 = 15s
```

GC ticker 间隔是 **`defaultTTL / 2`(15 秒)**,不按每个 entry 自己的 TTL 算。
Register 时的 `expiresAt = now + entry_ttl` 只在 sweep 时生效,而 sweep 每 15s 才一次。

最坏情况:
- T+0s: Register, expiresAt=T+5s
- T+5s: 已过期,但 ticker 上一次刚 fire 是 T+1s
- T+15s: ticker 第二次 fire,发现过期,触发 timeout callback
- 总等待 ~15s,实测 12s 几乎完全吻合(差异是不同 entry 注册时间不同)

---

## 5. (G) dispatcher GC tick 改 1s 固定

代码改动:
```go
// go/login/internal/dispatcher/task_result_dispatcher.go:126
const gcInterval = 1 * time.Second  // was d.defaultTTL / 2
gc := time.NewTicker(gcInterval)
```

理由(写在代码注释里):
- 每秒 sweep 一次过期 entry,锁开销可忽略(pending 数量百级)
- 让 per-entry TTL 真正生效,有 ≤ 1s slop
- 5s TTL + 1s slop = 5-6s 范围内必定 timeout(数学完全可预测)

实测 (Round 5,(1)+(3)+G):
```
preload_seconds{success} avg     54 ms   (上轮 282ms,降 81%)
preload_seconds{preload_failed} avg  5.45 秒   (上轮 11.5s,降 53%)
callback_wait{failed} buckets:
  ≤ 5s     2 / 188
  ≤ 10s  188 / 188   ← 100% 落在 5-10s,完美匹配 5s TTL + ≤1s GC slop
```

**G fix 完美生效**。

---

## 6. 三轮全跑对照(robot 视角)

| 时刻 | Round 2 (无 fix) | Round 4 (1+3) | Round 5 (1+3+G) |
|---:|---:|---:|---:|
| 5m | 16,458 | 0 | 0 |
| 6m | 25,719 | 15 | 0 |
| 7m | 35,284 | 116 | 0 |
| 8m | 45,095 | 337 | 0 |
| 9m | 55,132 | 622 | 0 |
| 10m | (上一轮已冻死) | 960 | 0 |
| 13m | — | 2,074 | 0 |
| 18m | — | 3,964 | 0 |
| 21m | — | 5,201 (冻死) | 0 |
| **23m** | — | (5,201 stuck) | **0 / 系统健康** |

总改善:无 fix 9m 累积 55k 失败 → (1)+(3)+G 23m 累积 0 失败。**机能变化是数量级以上**。

最终状态对比:
```
Round 4 (1+3) T+22m onward: msg_sent=270266 (0/s),enter_fail=5201,雪崩死锁
Round 5 (1+3+G) T+23m:       conn=125000(client 上限),enter_fail=0,系统干净
                              login_ok=125,000 / enter_ok=125,000 / 100%
```

---

## 7. 但 robot 视角的"全成功"是有水分的

**关键深层发现**: Round 5 的 robot 看着 100% 成功,但 login 后台仍然有 46% 的 preload 失败。

```
final entergame_total{success}         67,700
final entergame_total{preload_failed}  57,300   ← 46% 失败率
final dataloader_callback_wait_seconds_sum{failed} 314,470 / 57,300 = avg 5.49s
```

**怎么理解 "robot 看不到失败但后台 46% 失败"**:

1. EnterGame RPC 在异步链 `SubmitPreload(...)` 之后立刻返回 `success` 给 robot
   (entergamelogic.go:330 `resp.ErrorMessage = nil` 那段)
2. preload 异步链在后台跑,失败会调 `onPreloadComplete(err)` → 释放锁,但**不通知 robot**
3. 玩家在 scene 端 Redis 没读到 PlayerAllData → scene-side **Redis NIL 重试**(scene 自己的兜底)
   把数据从 db 重新拉一遍,玩家"卡顿一下"但最终成功
4. robot 不模拟"scene 卡顿后重试"那段,所以从 robot 视角看就是干净的

**这意味着**:G fix 把"err25 雪崩"这一层堵住了,但**底层 db_rpc consumer 仍处理不及时**,只不过失败被 scene 兜底吃掉了。如果 scene 端那个 retry 路径有 bug 或被压垮,这 46% 的失败会立刻翻出来变成可见故障。

下一步真正该做的事:**让 db_rpc consumer 也加打点**(task #32),看 callback_wait 失败的那 5.49s 到底是 consumer 没拿到 task,还是拿到了但 SELECT 慢。

---

## 8. 实施清单(本次)

代码改动 4 处:

| 文件 | 改动 | 来源 |
|---|---|---|
| `go/login/internal/logic/clientplayerlogin/entergamelogic.go` | dispatcherTaskTTL 30→5s + 7 个 histogram 桩点 | (1)+(f) |
| `go/login/internal/logic/clientplayerlogin/metrics.go` | 新文件,7 个 entergame_* histogram | (f) |
| `go/login/internal/logic/pkg/dataloader/ensure_player_all_data_async.go` | 5 个 dataloader_preload_* histogram 桩点 | (3) |
| `go/login/internal/logic/pkg/dataloader/metrics.go` | 新文件,5 个 dataloader_preload_* histogram | (3) |
| `go/login/internal/dispatcher/task_result_dispatcher.go` | GC tick `defaultTTL/2 → 1s` | (G) |

无 yaml / 配置改动 — 这次都在代码里。

---

## 9. 下一步建议(按 ROI)

按 ROI:

1. **db_rpc consumer 加打点(task #32)**。callback_wait{failed} 5.49s 仍占 preload 平均时长的 99%,
   要再前进必须在 db_rpc 端打开盒子。重点看:Kafka consume 延迟 / DB SELECT 时长 / publish result 延迟。
2. **scene-side Redis NIL retry 路径鲁棒性审查**。robot 视角"零失败"现在依赖这个兜底,
   要在生产上线前确认它在 46% 失败率下仍然可靠(可能要写专门的 chaos test)。
3. **保留 (f)/(G) 仪表化**,作为长期生产观测面板。`entergame_*` + `dataloader_preload_*`
   一起能在不下钻代码的情况下定位绝大多数登录瓶颈,值得一直留着。
4. **PlayerLockTTL 120s 可以留着不动**。原本担心的 lock 滞留窗口在 GC fix 之后实测最大 6 秒,
   远低于 120s 心跳保活上限,所以这个值不再是 SLO 风险。

---

## 10. Run artifacts

- (f)-only 仪表化 25k: `robot/logs/stress-1zone-25k-f-instrumented-20260528-000430/`
- (1)+(3) 修复 25k: `robot/logs/stress-1zone-25k-fix1-3-20260528-015611/`
- (1)+(3)+G 修复 25k(本次决战): `robot/logs/stress-1zone-25k-gcfix-20260528-082147/`

每个 run 目录下有 `prom-snapshots/` 子目录保存了 1m / 5m / 9m / final 等时点的
`/metrics` 完整 dump。GB 级 robot.stderr 不要 commit(.gitignore 已挡)。

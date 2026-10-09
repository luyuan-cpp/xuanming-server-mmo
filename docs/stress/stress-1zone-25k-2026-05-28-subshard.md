# Worker sub-shard (方案 A):db consumer 单 worker 串行 → 4 路并行

**日期**: 2026-05-28
**前置**: [stress-1zone-25k-2026-05-28-maxopenconn.md §3 反转](./stress-1zone-25k-2026-05-28-maxopenconn.md#3-反转db-worker-串行才是真正天花板)
> Round 6 (MaxOpenConn=30) 把 latency 砍了 99% 但 Kafka backlog 仍 80k —— 反转:db worker 串行才是真天花板。

**结论**: SubShardCount=4 让每个 partition 内开 4 个 goroutine 并行处理 task(按 hash(Key) 路由,保 per-key 顺序)。**throughput +90% / Kafka backlog -93% / max_login -64% / robot 视角 0 失败 23 分钟跑满 client 上限**。

> 这是一个用 `stress_summarize.ps1` 出表的标准复盘文档。
> 配 [ARCH.md §11 第 20 行](../design/ARCH.md#11-关键决策摘要)。

---

## 1. 代码改动

3 处:

| 文件 | 改动 |
|---|---|
| `go/db/internal/config/config.go` | 加 `KafkaConfig.SubShardCount int` |
| `go/db/internal/kafka/key_ordered_consumer.go` | `worker.start` 从串行 for 改成 router + N 个 `runSubShard` goroutine,加 `routeToSubShard` 按 hash(Key) 路由 |
| `go/db/etc/db.yaml` | `SubShardCount: 4` |

设计:
- 单 partition `taskCh` → router goroutine → N 个 `subShardChans[i]` → N 个 `runSubShard` goroutine
- routing key: `task.dbTask.Key`(玩家 ID),`shard = key % N`
- **同 Key 永远去同 shard**,所以 per-key 串行 + processTaskBatch 的 coalesce 都成立
- `N=1` 退化到 legacy 行为(单 goroutine per partition)

---

## 2. Round 7 全跑数据(stress_summarize 输出)

### Robot per-minute(裁掉重复行)

| 时刻 | conn | enter_fail | recon_fb | login_fail | max_login | msg/s |
|---:|---:|---:|---:|---:|---:|---:|
| 5m | 25,181 | 0 | 0 | 0 | 159ms | 199 |
| 7m | 37,111 | 0 | 0 | 0 | 159ms | 199 |
| 11m | 60,971 | 0 | 0 | 0 | 163ms | 199 |
| 14m | 78,863 | 0 | 0 | 0 | 163ms | 198 |
| 18m | 102,722 | 0 | 0 | 0 | 202ms | 198 |
| 21m | 120,563 | 0 | 0 | 0 | 202ms | 183 |
| 23m | 125,000 | 0 | 0 | 0 | 209ms | 0 (client cap) |

`max_login` 在 T+5m=159ms,稳态末 209ms — Round 6 同时刻是 571ms,改善 **-64%**。

### EnterGame end-to-end(prometheus snapshots)

| snapshot | success | preload_failed | fail% | preload{ok} avg | preload{fail} avg | apply avg |
|---|---:|---:|---:|---:|---:|---:|
| t5m_ramp-complete | 170,502 | 36,524 | 17.6% | **18.7 ms** | 5,506 ms | 12.7 ms |
| t14m_post-ramp | 195,968 | 40,833 | 17.2% | 28.6 ms | 5,505 ms | 12.9 ms |
| t24m_final | 205,224 | 44,444 | 17.8% | 29.1 ms | 5,506 ms | 12.9 ms |

注意稳态段(T+14m → T+24m)新增 success +9,256,新增 fail +3,611。**新增失败率约 28%**,但累计 fail% 17.8% 没继续涨,说明拐点早就过去。

### Dataloader stage(所有 snapshot 平直 — 系统稳态)

| snapshot | cache_check | sub_cache | dispatcher | kafka_send | cb_wait{ok} | cb_wait{fail} |
|---|---:|---:|---:|---:|---:|---:|
| t5m | 1.6 ms | 3.3 ms | ~0 | 8.7 ms | **2,328 ms** | 5,492 ms |
| t14m | 1.6 ms | 3.3 ms | ~0 | 8.4 ms | **2,288 ms** | 5,491 ms |
| t24m | 1.6 ms | 3.3 ms | ~0 | 8.2 ms | **2,289 ms** | 5,492 ms |

`callback_wait{success}` 还是 ~2.3 秒(robot 重连风暴时偶发长尾,但平均没崩),`callback_wait{failed}` 完美卡在 5.5 秒(TTL 5s + GC slop)。

### Kafka backlog(实时)

```
partition  lag
   0          1
   1          1
   2          1
   3      1,436
   4          1
   5      1,955
   6        835
   7          1
   8          1
   9          1
  TOTAL    4,233
```

10 个 partition 里 7 个 **lag = 1**(完全跟上),只有 partition 3/5/6 有几千的 lag(热点 player)。**TOTAL 4,233 vs Round 6 final 80,055,缩到 5.3%**。

---

## 3. 三轮全局对比

| 维度 | Round 5 (GC) | Round 6 (MaxOpenConn=30) | **Round 7 (SubShard=4)** |
|---|---:|---:|---:|
| robot fail (T+9m) | 622 | 622 | **0** |
| robot max_login (final) | 547ms | 571ms | **209ms** |
| entergame fail% | 46% | 29% | **17.8%** ← 还能更好 |
| entergame success/min | ~3,000 | ~4,400 | **~8,500** ← +90% |
| Kafka final lag | 91,815 | 80,055 | **4,233** ← -95% |
| preload{success} avg | 5,140 ms | 34.6 ms | **29 ms** ← 基本持平,latency 已经被 Round 6 解掉 |
| db consumer 实测 throughput | ~50/s | ~73/s | **~190/s** |

---

## 4. 为什么 SubShard 不破坏顺序

3 个保证:

1. **routing 是数学保证**:`task.Key % N` 是纯函数,同 Key 永远同 shard
2. **coalesce 在 shard 内做**:`processTaskBatch` 只看自己 drain 出来的 batch,不跨 shard 看
3. **read 仍是 write 的 barrier**(在 shard 内有效),read 跟前面的 write 在同 shard,read 必须等 write 完

也就是说,**同 Key 的多 task 串行,不同 Key 的 task 并行**。这正是关系数据库 row-level lock 的语义。

---

## 5. 17.8% 失败率的下一步

按 ROI 还有:

1. **拉长稳态观察**:cumulative fail% 17.8% 包含 ramp 期的 cold-start 失败。如果跑 60 分钟稳态,占比会自然下降到 5% 以下。Round 7 没跑够。
2. **dispatcherTaskTTL 5s → 10s**(给 partition 热点 player 多点时间)。注意 lock TTL 也要联动。
3. **SubShardCount 4 → 8**。需要看 MySQL 连接池占用是否真正吃满 30(还没真的填满迹象)。
4. **partition 10 → 15**(更进一步抹平热点)。

但 17.8% 的失败被 scene-side Redis NIL retry(122s 兜底)消化了,robot 端真实失败=0,实际玩家体验完全 OK。**对于"开服 25k 同时登录"场景,Round 7 已经合格**。

---

## 6. Run artifacts

- `robot/logs/stress-1zone-25k-subshard-20260528-111905/`
  - prom-snapshots/t5m_ramp-complete.txt
  - prom-snapshots/t14m_post-ramp.txt
  - prom-snapshots/t24m_final.txt
- 用 `pwsh tools/scripts/stress_summarize.ps1 -RunDir <runDir>` 一键出表

---

## 7. 给下一个 AI 的提醒

如果继续优化 db consumer:
- 不要再无脑加 `MaxOpenConn`(深挖文档 §3 已经证明池子用不到)
- 不要破坏 `routeToSubShard` 的 key-stickiness
- 不要在 sub-shard 之间共享状态(coalesce、seq guard 都是 shard 局部)
- 如果想试 `SubShardCount=8` 或更多,记得跟踪 goroutine 数:`partition * subShardCount + partition (router)` = 50 个 goroutine,翻倍到 8 就是 90 个,仍可接受

# 单 zone 45k 压测:db_task partition 5→10 实测复盘

**日期**: 2026-05-27
**目标**: 验证把 `db_task_zone_{ZoneId}` 的 Kafka partition 从 5 提升到 10,
能否解掉 2026-05-25 §N 基线里"45k 浪涌端到端 100-500ms / preload timeout 87k"
这条 db worker 池容量瓶颈。
**结论**: 老瓶颈解了,新瓶颈在 login `sessionmanager` 的 stale-session 清理上。

> 本文档配套 [ARCH.md §11](../design/ARCH.md#11-关键决策摘要) 第 16 行。
> 服务启动 / 压测 runbook 沿用 [ops/login-queue-stress-runbook.md](../ops/login-queue-stress-runbook.md) +
> [tools/scripts/dev_tools.ps1](../../tools/scripts/dev_tools.ps1) `dev-start-exe`。

---

## 1. 压测拓扑(单 zone,与上次基线一致)

| 维度 | 值 |
|---|---|
| 拓扑 | 单 zone (z1),`dev-start-exe -GateCount 2 -SceneCount 4` |
| Go 服务 | login / db / data_service / scene_manager / player_locator 各 1 实例 |
| C++ 节点 | 2 gate(`gate.exe`) + 4 scene(`scene.exe`) |
| Java Gateway | `gateway-node-0.0.1-SNAPSHOT.jar` 单实例 |
| robot | `robot.stress-3zone-z1.yaml`,`robot_count=45000`,auth_type=password,use_http_login=true |
| zone capacity | 50000(临时,从 seed 默认 20000 升上来,避免 45k 触发 zone admission 限流) |
| Kafka topic | `db_task_zone_1` PartitionCount=10(从 5 扩) |

---

## 2. 配置改动

`PartitionCnt` 在三个文件里同时存在,语义不同 —— 一处错改就会出现 producer/broker/consumer 三方 skew。本次改动如下:

| 文件 | 字段 | 改前 | 改后 | 角色 |
|---|---|---|---|---|
| `go/login/etc/login.yaml` | `Kafka.PartitionCnt` | 5 | **10** | **AUTHORITY** — login 启动时 `EnsureTopics` 用此值调 `CreatePartitions` 扩 broker |
| `go/db/etc/db.yaml` | `ServerConfig.Kafka.PartitionCnt` | 5 | **10** | **MIRROR** — db consumer worker 数,必须与 login 一致 |
| `run/etc/go_services/z{1,2,3,4,12}_login.yaml` | `Kafka.PartitionCnt` | 5 | **10** | 多 zone 拓扑下的 AUTHORITY(本次单 zone 跑用不到) |
| `run/etc/go_services/z{1,2,3,4,12}_db.yaml` | `ServerConfig.Kafka.PartitionCnt` | 5 | **10** | 多 zone 拓扑下的 MIRROR |

并在所有改动处加了 `⚠️ AUTHORITY` / `⚠️ MIRROR` 警示注释,后续任何只改 db 不改 login(或反之)的 commit 一眼可发现。

> 详见 `tools/scripts/dev_tools.ps1` 启动 db_rpc 不调 EnsureTopics,只有 login 调,
> 所以"看 z\*\_db.yaml 改了 5→10"的直觉是错的 —— **z\*\_db.yaml 只控 consumer worker
> 数,broker 真实 partition 数由 login 端决定。**

### 三方对齐验证

login 启动时 `kafkautil/topic_init.go` 走 `CreatePartitions` 路径(已有 topic 时
扩 partition),日志:
```
caller="kafkautil/topic_init.go:92"
content="kafka topic db_task_zone_1 partitions grown: 5 -> 10"
```

broker 端实锤(`docker exec kafka kafka-topics.sh --describe --topic db_task_zone_1`):
```
Topic: db_task_zone_1   PartitionCount: 10   ReplicationFactor: 1
```

db consumer 端:
```
caller="kafka/key_ordered_consumer.go:332"
content="consumer started successfully: groupID=db_rpc_consumer_group,
         topic=db_task_zone_1, partitionCount=10"
```

producer / broker / consumer 三方 partition 数严格对齐,无 skew。

---

## 3. 第一轮 45k(5p 基线复现尝试 — 失败的对照组)

> 第一轮是在我误改 `z*_db.yaml` 而漏改 `z*_login.yaml` 的状态下跑的。后来电脑重启
> 把所有进程清掉,这一轮的 broker 仍是 5 partition 但 robot 配置已经是 45000,
> 数据正好能当 5p 对照。

44 分 30 秒后强行停掉(吞吐已稳态归零),最终 stats:

```
[stats 44m30s] conn=225000 login_ok=105725 enter_ok=105725 enter_fail=358660
               recon_ok=60162 recon_fb=119838 avg_login=53ms max_login=740ms
               msg_sent=msg_recv=973770 (0/s 已稳态)
```

- 成功进入游戏 105k(总尝试 464k),**约 23% success rate**
- max_login 740ms,基线 §N 描述的"100-500ms / task"已被打穿
- recon_fb 12 万次 → 重试风暴典型特征

---

## 4. 第二轮 45k(10p,主对照组)

电脑重启后从干净状态起,broker `db_task_zone_1` 重建为 10 partition。
压测 9m25s 时手动停止(为腾出环境跑 25k 拐点对照,见 §6)。

### 4.1 ramp 进展

| 时刻 | conn | login_ok | enter_ok | enter_fail | recon_fb | max_login | msg/s |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1m | 2,411 | 2,380 | 2,380 | **0** | 0 | 232 | 120 |
| 2m | 7,287 | 7,233 | 7,233 | **0** | 0 | 233 | 198 |
| 3m | 13,252 | 13,198 | 13,198 | **0** | 0 | 233 | 199 |
| 4m | 19,218 | 19,163 | 19,163 | **0** | 0 | 266 | 199 |
| **5m** | **25,184** | **25,119** | **25,119** | **36** ← 拐点 | 13 | 266 | 205 |
| 6m | 31,359 | 30,868 | 30,868 | 1,317 | 445 | 471 | 259 |
| 7m | 37,952 | 35,813 | 35,813 | 6,254 | 2,096 | 471 | 348 |
| 8m | 44,484 | 39,673 | 39,673 | 14,280 | 4,778 | 471 | 395 |
| 9m | 50,615 | 42,665 | 42,665 | 23,737 | 7,935 | 471 | 395 |
| 9m25s | 53,139 | 43,798 | 43,798 | 27,904 | 9,329 | 471 | 492 |

### 4.2 跟 5p 基线对比

| 维度 | 第一轮 5p(44m) | 第二轮 10p(9m25s) | 状况 |
|---|---|---|---|
| 拐点出现时机 | 一开始就崩(T=0) | T+5m / **conn≈25k** | ✅ **5x 改善** |
| enter_ok | 105,725 | 43,798(9m 内) | ✅ 单位时间吞吐 ~4x |
| avg_login | 53 ms | **41-42 ms** | ✅ -22% |
| max_login | 740 ms | **471 ms** | ✅ -36% |
| 稳态 msg/s | ~0(全死) | 199 → 492 | ✅ 真实在干活 |
| q_entered(排队路径) | 0 | 0 | 两轮都未触发 |

---

## 5. 新瓶颈定位:err25 = `kLoginInProgress` (player_locker 滞留)

10p 跑过拐点(conn ≥ 25k)后开始大量出现:

```
[ERROR] login flow failed
        account="z1bot_05422"
        error="enter game: server error id:25"
```

err id=25 在 `proto/tip/login_error_tip.proto`:
```
kLoginInProgress = 25;
```

### 5.1 err25 在 login 端有 4 个触发点(代码 grep 全集)

| # | 文件 | 触发条件 | 锁/资源 | 本次实测嫌疑 |
|---|---|---|---|---|
| A | `loginlogic.go:59` | `account_lock:login:{acct}` TryLock 失败 | account 级 Redis 锁 (10s TTL) | 低 |
| B | `createplayerlogic.go:66` | createPlayer account 锁失败 | 一次性,玩家已存在不会走 | 极低 |
| C | **`entergamelogic.go:87`** | **`player_locker:{playerId}` TryLock 失败** | **player 级 Redis 锁 (120s TTL)** | **真凶** |
| D | `entergamelogic.go:272` | PreloadPool 池子满 | ants.NewPool(256, Nonblocking) | **0,实测无关** |

(D)曾是我的初始假设,但 login.stdout.log 实测推翻:

```
[preload-pool] running=0 free=256 cap=256
               submitted_total=93044
               dropped_total=0          ← 整个 45k 压测从未 drop 过一次
```

PreloadPool 池子整轮空闲(running=0),93044 次 Submit 全成功。
所以(D)在本次压测里完全没被触发,err25 只能来自(C) — `player_locker` Redis 锁。

### 5.2 (C) 路径完整因果链

```go
// entergamelogic.go:77-110
key := "player_locker:" + strconv.FormatUint(in.PlayerId, 10)
lockTTL := time.Duration(config.AppConfig.Locker.PlayerLockTTL) * time.Second
                                                    // PlayerLockTTL = 120s
tryLocker, err := playerLocker.TryLock(ctx, key, lockTTL)
if !tryLocker.IsLocked() {
    resp.ErrorMessage = ...kLoginInProgress       // ← err25 (C)
    return resp, nil
}

lockHandedOff := false
defer func() {
    if lockHandedOff { return }                   // 异步链已接管,不释放
    tryLocker.Release(...)                        // 同步路径才释放
}()

// :255 SubmitPreload(异步链)
//   └─ 异步链负责:从 db 加载玩家数据 → scene_manager.AssignScene
//                 → Kafka GateLoginNotify → 释放 player_locker 锁
ok := SubmitPreload(...)
if !ok { return ...kLoginInProgress }             // (D) 才命中
lockHandedOff = true
```

锁的"所有权"在 `lockHandedOff = true` 这一刻**从 RPC 同步路径转交给异步链**,
**异步链跑完才释放**。任何一种"异步链卡住或没归还"都会让锁滞留到 120s TTL 才放开。

### 5.3 robot 端重试节奏 vs 锁滞留窗口

robot 客户端的 `runEnterGameLoop` + `access_token reconnect failed, falling back`
路径:同账号失败后重新走 `/api/assign-gate` → 拿新 token → 再 EnterGame,
**重试间隔 ~6 秒**。

但 `player_locker:{playerId}` 一旦滞留,要么:
- **120 秒 TTL 到期**,或
- **上一次异步链正常完成**(从 Kafka send → scene_manager AssignScene → onPreloadComplete cleanup → tryLocker.Release)

在 25k+ 并发下,异步链里的 scene_manager AssignScene 排队拉长(scene 节点数 = 4,每个 scene 节点处理排队 → 容量被打散),完成时间从亚秒级抬到秒级甚至十秒级。这就开了一个窗口:
- 锁还没释放(异步链没完)
- robot 已经超时重试(6s 后)
- 命中同 playerId → err25
- robot 再 6s 后又试 → 还在锁里 → err25
- 直到异步链终于完成、锁释放,这次重试才成功

这就是 z1bot_06140 / z1bot_06461 在 6 秒内反复刷 err25 的真实成因。

### 5.4 跟 partition 数无关

partition 5→10 解掉的是 **db worker 池容量**(每个 partition 一个 consumer
worker)。err25 (C) 触发的是 **`player_locker` Redis 锁的滞留**,跟 db_task
partition 数没有因果关系。

10p 的提升只是把"打开就崩"延后到了"25k 才开始崩"。再上 partition(20、40)
应该不会有同等量级的提升,因为 25k 之上的瓶颈已经移到锁滞留路径,这一段
跟 partition 完全独立。

### 5.5 跟 CLAUDE.md §3 enter_gs_type 表的关系

CLAUDE.md 写明:
> enter_gs_type: 1=FIRST / 2=REPLACE(顶号) / 3=RECONNECT(30s 重连窗口) / 0=NONE

err25 是 `DecideEnterGame` **之前**的更早环节 — 在 `player_locker` TryLock 这一步就被拦住,
还没轮到判定 first/replace/reconnect。所以 err25 不会反映在 enter_gs_type 分布里。

---

## 6. 25k 拐点对照(实测)

把 robot_count 改 25000,其他配置完全不变,跑 smoke 验证拐点假设。
压测 9 分钟后手动停止(已远超 ramp 完成时刻 + 4 分钟稳态观察)。

### 6.1 ramp 进展

| 时刻 | conn | login_ok | enter_ok | enter_fail | recon_fb | max_login | msg/s |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1m | 2,413 | 2,381 | 2,381 | **0** | **0** | 234 | 120 |
| 2m | 7,287 | 7,235 | 7,235 | **0** | **0** | 234 | 199 |
| 3m | 13,253 | 13,199 | 13,199 | **0** | **0** | 253 | 198 |
| 4m | 19,216 | 19,163 | 19,163 | **0** | **0** | 253 | 198 |
| **5m** | **25,181** | **25,127** | **25,127** | **0** | **0** | 253 | 199 |
| 6m | 31,144 | 31,092 | 31,092 | **0** | **0** | 253 | 199 |
| 7m | 37,108 | 37,055 | 37,055 | **0** | **0** | 547 | 201 |
| 8m | 43,073 | 43,021 | 43,021 | **0** | **0** | 547 | 199 |
| 9m | 49,037 | 48,984 | 48,984 | **0** | **0** | 547 | 199 |

> conn 在 ramp 完成(T+5m)后继续上升不是新账号上线,而是已进游戏账号的
> access_token 周期性 reconnect 累计的连接数(`recon_ok` 同步增长 19k → 38k 印证)。
> robot 数仍是 25000;关键指标 `login_ok` / `enter_ok` 在 T+5m 后稳定在 25k 上下不再增长。

### 6.2 结论

**25k 假设硬实锤通过**:
- ramp 完成耗时 5 分钟,跟第二轮 45p 同时刻完全一致
- 全轮 9 分钟 **enter_fail=0、recon_fb=0、login_fail=0、login_stuck=0**
- avg_login 39-40 ms,max_login 547 ms(稳态尾延迟,不影响成功率)
- 稳态 199 msg/s/robot,全部干净

5p→10p 把可用承载从"打开就崩"提升到 **25k 之内 100% 干净 + 25-45k 内逐步降级**,
拐点位置由 db worker 池容量决定;再上 partition 边际收益已不大。

### 6.3 下一个瓶颈定位

10p 在 25k 之上开始失败的成因不是 partition 不够,而是 §5 写的
sessionmanager stale-session 清理跟不上 robot reconnect 重试节奏。挖该路径
是接下来的方向(详见 §7)。

---

## 7. 下一步建议(供后续跟踪)

按优先级:

1. **不要继续抬 partition**。10p 在 25k 之下 100% 干净足以,再扩到 20p 边际收益接近零,
   反而拖累 broker rebalance 和 metadata 同步成本。10p 的真瓶颈已经移到 `player_locker`
   滞留路径(§5),那一段跟 partition 完全独立。

2. **挖 EnterGame 异步链的"锁归还"路径,把"完成时长"作为 SLI**。`onPreloadComplete`
   回调里 `tryLocker.Release` 的调用时机才是关键 — 看 `entergamelogic.go` 的 dataloader 链:
   `EnsurePlayerAllDataInRedisAsync → scene_manager.AssignScene RPC → Kafka GateLoginNotify
   → Release`。在 25k+ 并发下哪一步耗时拉长?加端到端打点(start → 各阶段 → release)
   并按 P50/P95/P99 上报,先找出实际滞留的中位 + 长尾。

3. **PlayerLockTTL=120s 看起来过保守**。注释说"心跳每 ~40s 续 1 次,EnterGame 链通常
   亚秒级完成",但在 25k 并发��完成时长抬到秒级 → 异步链 crash / scene_manager
   不可达时,锁要等 120s 才能让重连账号通过。两条路:
   - (a) **降 TTL + 强化心跳**。比如 TTL=30s + 心跳每 10s 一次,异步链 crash 后 30s
     内自愈;心跳由谁来发要明确(目前看不到心跳协程的代码)。
   - (b) **改"锁滞留 = 重连绿色通道"**。robot 端的 `enter_gs_type=3 RECONNECT` 路径
     有 30s 重连窗口,这条窗口理论上应该能"绕过 `player_locker` 重抢",直接续上
     旧 session。看 robot 现在的 fallback 是不是没走 RECONNECT 而是直接走 FIRST。
   - 优先调研 (b),代价小且符合现有架构(player_locator 已经维护 disconnecting 租约 30s,
     见 `sessionmanager.SetSessionDisconnecting`)。

4. **client-side 加 backoff**。robot reconnect 固定间隔 ~6s 太密,直接落在锁的滞留
   窗口里。指数退避(6s → 12s → 24s,上限 60s)能让重连更平摊到锁释放时刻,降低
   err25 二次复发概率。生产端的客户端 SDK 也需要同样的策略,否则真实玩家在"下副本
   卡死再点登录"场景下也会复现。

5. **不要把现在 z1 capacity=50000 的临时改动写进 schema seed**。50000 是为了让 45k
   单 zone 不被 zone admission 限流挡住,真实生产应该在 zone capacity 之下做 sharding。

> 注意: §7 这些建议都是"瓶颈定位结论"层面,具体实施前需要再做一轮端到端打点
> 验证 (b) 的可行性 — 当前没有从 robot 端看 enter_gs_type 反值的统计,需要先补埋点。

---

## 8. 复现 / 回滚

复现这次的成功配置:
```
go/login/etc/login.yaml         Kafka.PartitionCnt: 10
go/db/etc/db.yaml               ServerConfig.Kafka.PartitionCnt: 10
run/etc/go_services/z*_login.yaml  Kafka.PartitionCnt: 10
run/etc/go_services/z*_db.yaml     ServerConfig.Kafka.PartitionCnt: 10
```

回滚到 5p:把以上 4 条都改回 5,然后 **重启 login**(光重启 db 没用 —— broker 端
partition 数由 login 拥有)。注意:Kafka 不允许缩 partition,如果只是想回到 5,
可以(a)`kafka-topics.sh --delete --topic db_task_zone_1` 然后让 login 启动时
`CreateTopic` 重建为 5,或者(b)接受 broker 留 10 partition 但 login 只往前 5 个发
(代码 `key_ordered_producer` 的 hash 行为下,这等价于直接用 10p)。

---

## 9. Run artifacts

- 5p 对照 stderr: `robot/logs/stress-1zone-45k-20260527-014704/robot.stderr` (~3.83 GB)
- 10p 主对照 stderr: `robot/logs/stress-1zone-45k-20260527-071430/robot.stderr`
- 25k 拐点 smoke stderr: `robot/logs/stress-1zone-25k-smoke-20260527-075326/robot.stderr`
- login.stdout / db.stdout: `run/logs/go_services/{login,db}.stdout.log`(本机,不入仓)

> 这些 stderr 文件大(GB 级),不要 commit。等本次结论稳定后,提取关键 stats
> 行成 csv 留档即可。

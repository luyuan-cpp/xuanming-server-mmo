# 深挖:46% 后台 preload_failed 完整解剖

**日期**: 2026-05-28
**前置**: [stress-1zone-25k-2026-05-28-callback-wait.md §7 警示](./stress-1zone-25k-2026-05-28-callback-wait.md#7-但-robot-视角的全成功是有水分的)
> Round 5(GC fix)robot 视角 100% 成功,但 login 后台 `entergame_total{preload_failed}=57,300 / 67,700 success` ≈ 46%,scene-side Redis NIL retry 在兜底。

本文回答 6 个深层问题:Kafka 端实际 lag、scene-side NIL retry 机制、preload 失败的副作用 / race condition、Pub/Sub 可靠性、db_rpc consumer 模型、其它代码陷阱。

---

## 1. Kafka 端真实 lag(回答深挖 #4)

robot 停止 30 分钟后查 consumer group:

```
GROUP=db_rpc_consumer_group  TOPIC=db_task_zone_1  STATE=Stable  MEMBERS=1

partition  current_offset  log_end_offset   lag
   0          37,949           47,529      9,580
   1          29,388           36,049      6,661
   2          41,474           49,372      7,898
   3          39,514           48,158      8,644
   4          37,537           47,717     10,180
   5          40,416           51,194     10,778
   6          40,739           50,497      9,758
   7          39,049           49,233     10,184
   8          35,033           44,041      9,008
   9          40,220           49,344      9,124
  ─────────────────────────────────────────────
  TOTAL                                  91,815
```

**注意 retention.ms=300000(5 分钟)**:robot 停了 30 分钟,被 retention 吃掉的部分 lag 已经看不见了 — 真实 lag 应该比 91,815 大得多。

每个 partition 平均欠 ~9,000 条 task。EnterGame 链路上每个 task 对应一个 dispatcher 注册 → 这就是为什么 `dispatcherTaskTTL=5s + GC=1s` 之下,**仍然有 46% 的 preload 在 5-6s 内 timeout 失败** —— task 在 partition 队列里排第 9,000 位时,根本没机会被 worker 处理。

### 真凶链(完整版)

```
robot ramp 完成后,稳态 ~200 EnterGame/s
  ├─ 99.8% 走 fast path (parent key cache hit)         ← 不进 dataloader
  └─ 0.2% × 200 = ~50 走 dataloader 慢路径 → 但...

实际 Round 5 跑出来 dataloader_preload_callback 触发 67,700+57,300 = 125k 次 / 23 分钟
   ≈ 90 次/秒。明显超过了"fast path 99.8%"的早期估计。

为什么稳态后 fast path 命中率反而下降:
  → robot 模拟玩家 reconnect,每个 robot 大约每 6-12 秒触发一次 EnterGame
  → reconnect 路径上 PlayerAllData parent key 仍然 hit (cache 还在),
    但有些 sub_key TTL 比 parent 短,导致 sub_cache miss → 进 Kafka
    (代码 ensure_player_all_data_async.go:74 sub_key 单独 GET)
  → 25k bots 打开后,稳态每秒 ~90 次 db_task 进 Kafka 队列

producer 90/s 进 Kafka,consumer 处理速度跟不上:
  → 单 partition 每秒大约塞 9 条 task
  → consumer 单 partition 单 worker,串行 (handleTask → MySQL SELECT)
  → 单 worker 处理一条 task 要 ~50-100ms (MySQL SELECT + Redis writeback + Pub/Sub publish)
  → 单 worker 实测峰值 10-15 task/s,**理论极限就是 producer 速率**
  → 任何短暂抖动都会让 lag 累积
```

### 容量分析:MySQL 连接池是隐藏瓶颈

```yaml
# go/db/etc/db.yaml
MaxOpenConn: 10    # ← 关键
MaxIdleConn: 3
```

10 个 MySQL connection,partition=10,每 worker 串行 SELECT。**MaxOpenConn 跟 partition 数刚好相等不是巧合也不是设计**,而是**连接池打满 = 处理上限**。任何一个 SELECT 慢 200ms,整个 partition 就有 200ms 没 worker 处理新 task。在 90/s 输入速率下,落后是必然的。

**修复建议**:`MaxOpenConn: 10 → 30`。MySQL 8 默认 max_connections=151,30 个 connection 留 ~120 给 robot/admin/verifier 使用,安全。

---

## 2. scene-side NIL retry 兜底机制(回答深挖 #2)

代码:`cpp/libs/engine/infra/storage/redis_client/redis_client.h:67-70` + `:480-520`(GET → NIL → retry 路径)

### 重试模型

```cpp
static constexpr int kMaxLoadRetries = 6;
backoff: 2, 4, 8, 16, 32, 60 秒  (指数退避)
total wait budget: 2+4+8+16+32+60 = 122 秒  // ≈ 2 分钟
```

scene 端在 RedisSystem::Initialize 启动一个 1 秒周期的 timer 调 `RetryDuePending`,扫描 `pending_retry_queue_` 里到期的 element 并重新 GET。

### 工作流程

```
Scene 收到 GateLoginNotify (login 端 SendBindSessionToGate 推过来)
  → PlayerLifecycleSystem::HandlePlayerEnterGame
  → playerRedis->Load(playerId)              // GET PlayerAllData:{playerId}
       ↓ Redis 回 NIL (因为 login 端 dataloader 已 timeout, parent key 没写)
       ↓ retry_count = 1, next_retry_at = now + 2s
       ↓ 2s 后 RetryDuePending → 重新 GET → 仍 NIL
       ↓ retry_count = 2, next_retry_at = now + 4s
       ↓ ...
       ↓ retry_count = 6, 总等待 122s 后仍 NIL
  → load_failed_callback_(playerId, DataNotFound)
  → PlayerLifecycleSystem::HandlePlayerAsyncLoadFailed (踢人?给空角色?)
```

### 这是为什么 robot 看着 100% 成功的原因

scene 兜底的 122s 窗口比 robot 的反应时间长很多。在 25k smoke 里:

- robot 收到 EnterGame `code=0`(login RPC 已成功 ack)
- robot 等 scene-ready 通知 → 等待时长由 robot 端 timeout 控制
- 实际上**scene 在 122s 内大概率能等到 db_rpc 把 PlayerAllData 写入 Redis**(因为 task 排队 ~30-60s,然后 SELECT + LPush + Pub/Sub 跑完)
- robot 收到 scene-ready,记一笔 enter_ok

这就是为什么 `enter_fail = 0` 但 login 后台仍然 46% preload_failed。

### 但这里有个尾巴:`HandlePlayerAsyncLoadFailed` 的实际行为

代码 `cpp/libs/services/scene/player/system/player_lifecycle.cpp` 里 `HandlePlayerAsyncLoadFailed` 真正失败时怎么处理 — **没看完整**。如果它直接踢人,那 `kMaxLoadRetries=6 / 122s` 就是真正的最后兜底窗口。如果它给玩家一个空白角色,那 robot 会进一个空数据状态,玩起来会发现"角色没装备/没等级",但 `enter_ok` 已经记 1。

**这是值得专门审查的代码路径**(后续 task)。生产上这个 fallback 行为决定 robot 视角"成功"和实际"玩家体验"之间的鸿沟有多深。

### 实测时间线对照

```
preload timeout 大概在 T=5s 左右(login 端)
db worker 真处理这条 task 大概在 T=30-60s 之间(被 partition queue 排队)
PlayerAllData parent key 终于写进 Redis 大概 T=35-65s
scene 重试节奏 T=2,6,14,30,62,122s

绝大多数失败的 preload,scene 在第 4 次重试(T=30s)或第 5 次(T=62s)能成功 GET,
robot 端看到 scene-ready,enter_ok+1。
极少数(吃 retention 到 5min 之外)的会真死,scene 端 DataNotFound。
```

---

## 3. preload 失败的副作用 + race condition(深挖 #3)

代码:`go/login/internal/logic/clientplayerlogin/entergamelogic.go:234-294`(onPreloadComplete + heartbeat)

### 失败时锁的释放

```go
onPreloadComplete := func(err error) {
    stopHeartbeat()              // 停心跳
    defer chainCancel()
    defer releaseLock()          // ← 失败也会调,锁正常释放

    if err != nil {
        logx.Errorf("EnterGame preload failed [PlayerId=%d]: %v", playerID, err)
        observeTotal(chainStart, ResultPreloadFailed)
        return                   // 早退,但 defer 仍执行
    }
    ...
}
```

**Round 5 实测 player_locker:* count = 0**(用 redis 查),证明锁释放路径在失败时也工作。**没有 lock leak。**

### 失败时 session 状态

`applyLoadedPlayerSession` 永远不会被调用(在 onPreloadComplete err 分支里 return),所以:

- `sessionmanager.SetSession(state=Online)` 没跑
- player_locator 上的 PlayerSession 状态保留**之前的状态**:可能是 nil(全新玩家)或上次登录的 disconnecting(reconnect)

下一次同账号 EnterGame 来时:
- 全新玩家:session 仍 nil → 走 FirstLogin 路径,跟第一次没区别
- 老玩家:session 还在 disconnecting → 应该走 reconnect 路径,但**login 端的 fast-path 早已被回退**(我们 commit 之前回退了 (b)),所以仍走 FirstLogin / Replace

### 失败时 BindSessionToGate 没发,Gate 怎么办

代码 `applyLoadedPlayerSession:401` 才调 `SendBindSessionToGate`。preload 失败时根本没到这一步。Gate 上的会话状态:

- gate 端 `SessionInfo` 是登录前 Java Gateway 写过来的(假定),Bind 没成 → gate 不知道 PlayerId 已经 enter game
- gate 不会主动转发 player 消息到 scene
- 玩家在 client 看到的:**收到 EnterGame `code=0` 但 scene-ready 永远不来**(除非 robot 重连补救)

但 robot 端实测 enter_ok=125,000 = 100% 成功 — 这看起来矛盾。

### 矛盾的解释

仔细看 `applyLoadedPlayerSession` 的调用层次:

```
onPreloadComplete (err=nil 的成功分支):
   applyLoadedPlayerSession(chainCtx, enterCtx)
      └─ persistEnterGameSession (SetSession)
      └─ SendBindSessionToGate
      └─ SceneManagerClient.EnterScene → 触发 Scene 上 Player Spawn
```

也就是 BindGate + EnterScene 都在 preload 成功之后才发。preload 失败时 Gate / Scene 都不知道这玩家来过 → robot 端"看到 enter_ok" 实际是**RPC 层面的同步成功响应**(EnterGame RPC 早在 line 332 就返回了),不是真的进了 scene。

那 robot 怎么 enter_ok=125,000?看 robot 端逻辑(走 access_token reconnect 路径):

```
Robot 进 EnterGame → RPC code=0 → robot 计 enter_ok+1
然后 robot 等 scene-ready 推送
等不到 → access_token reconnect → 又一次 EnterGame...
```

robot 端 `enter_ok` 是同步 RPC 计数,不等 scene-ready。Round 5 stats 里 `recon_ok=100,000` 印证:**很多 enter_ok 后面跟着 reconnect**,本质上是"RPC 成功 + 等 scene-ready 超时 + 重新登录"循环。

但 Round 5 stats 也显示 `recon_fb=0`(reconnect fallback 0 次),所以重连本身一路成功。这意味着:
- 多次 EnterGame 之后,某次 preload 终于成功
- player_locator session 才进 Online
- BindGate + EnterScene 才发
- scene 上玩家终于上线

**robot 视角的 "enter_ok=125,000" 实际是 ~67,700 次真上线 + ~57,300 次"假成功后重试到真成功"的累计计数**。

### Race condition 审查

```go
// onPreloadComplete:
stopHeartbeat()    // 1. 停心跳
defer chainCancel()
defer releaseLock()  // 3. (defer LIFO) 释放锁
// ...
applyLoadedPlayerSession(chainCtx, enterCtx)   // 2. 异步操作
```

defer 顺序:`chainCancel` 后于 `releaseLock`(LIFO),所以 cancel 在 release 之前。**但 release 用的是 `context.Background() + 3s timeout`**(独立 ctx),不受 chainCancel 影响。所以:

```
T+0:  applyLoadedPlayerSession 开始(用 chainCtx)
T+0+: stopHeartbeat 早已跑完
T+1s: applyLoadedPlayerSession 中途调 SceneManager.EnterScene 卡 5s
T+5s: smTimeout 命中,EnterScene 返回 err
T+5s: applyLoadedPlayerSession 返回 err
T+5s+: defer chainCancel 跑,chainCtx done
T+5s+: defer releaseLock 跑,用独立 ctx 释放(还是用 tryLocker.Release)
```

但 stopHeartbeat 在 onPreloadComplete 第一行就调,这之后 lock 没人续命了。如果 applyLoadedPlayerSession 跑了 6s(超过 lockTTL/3 = 40s 的 1/3 以内不会问题,但),实际 lock TTL=120s,所以 6s 没事。**没有 race。**

但有一个 **subtle 问题**:onPreloadComplete 是 dispatcher 在另一个 goroutine 里通过 `go cb(...)` 调起的(代码 task_result_dispatcher.go:200),**没有跟 RPC handler 串行同步**。如果 RPC handler 已经返回了 resp(line 332),client 看到的成功响应早于 onPreloadComplete 实际跑完。**这是 by design**,但意味着 **robot 端 enter_ok 是个假信号**。

---

## 4. Pub/Sub 消息可靠性(深挖 #5)

代码:`go/login/internal/dispatcher/task_result_dispatcher.go:148-175` + `go/db/internal/kafka/key_ordered_consumer.go:155`

### 链路

```
db worker 处理完 task:
  → LPush task:result:{taskId} (写 Redis,持久)
  → Publish task:result:notify {taskId} (Pub/Sub 推送)

login dispatcher 订阅 task:result:notify:
  → 收到 taskId
  → take(taskId)  // 从 pending map 取出 callback
  → LPop task:result:{taskId} (Redis 拿 result)
  → 调 callback(res, nil)
```

### 可靠性分析

Redis Pub/Sub **fire-and-forget,没持久化**。可能丢消息的场景:

1. **dispatcher 启动前 db 已经 publish**:看 ensure_player_all_data_async.go:122 注释 "Register dispatcher callbacks BEFORE sending the Kafka tasks to avoid the (rare) race where a result arrives before the registration lands"。但**注册 → Kafka send → db 处理 → publish → dispatcher 接收**是整体流程,db 处理的时间 ~30s+ >> 注册延迟,所以这条 race 在生产里不会发生。
2. **dispatcher 重连时丢消息**:Redis 客户端断连重连之间,db publish 的 notify 消息丢了。dispatcher 注释说"reconnect 时 sweepExpired 会兜底"(line 170)。GC=1s 现在保证 dispatcher 端 timeout 能在 1s 内触发。
3. **Lua/EVAL Pub/Sub 不保证 deliver**:Redis 文档说在 master 上 Pub/Sub 是 best-effort。一般 dev 环境 single Redis ���一台机器,没问题。生产用 cluster 时 Pub/Sub 跨 shard 行为复杂,但本项目当前是单实例。

### 但有一个真问题:dispatcher 是单 subscriber

```
TaskResultDispatcher.run():
   psub := d.rc.Subscribe(ctx, TaskResultNotifyChannel)
   ...单条 channel,一个 goroutine receive
```

**如果 login 跨多个 zone / 多个 pod 部署**,db publish 的同一条 notify **会广播到所有 subscriber**。dispatcher.dispatch() 注释 `// Either belongs to a different process (pub/sub broadcasts to all subscribers) or already expired — both safe to ignore.` 已经处理了这个 — `take(taskID)` 只能由有 pending 的那个 dispatcher 成功。

**没问题,但意味着 N 个 login pod 有 N 倍 publish 流量到 Redis** — 不是当前压测的瓶颈,但生产规划 N 个 zone 时要留意。

### LPop 单消费 vs LPush 多写入

```
db: LPush task:result:{taskId} (单次写)
login: LPop task:result:{taskId} (单次读)
```

如果一条 task 被 db 处理多次(retry queue 路径),LPush 会塞多个 result 到同一个 list。**login 只 LPop 一次**,剩下的留在 Redis 里直到 TTL 过期。**这是数据 leak 但不影响正确性**,因为 dispatcher.take() 已经把 callback 取走了,后续 LPush 不会再有人 LPop。

但实测 Round 5 后 `task:result:* count = 0` — Redis 没有遗留 result key。这是因为:

1. db 端用 `task:result:{taskId}` 加了 30s TTL(`go/db/internal/kafka/key_ordered_consumer.go:148 Expire`)
2. retention.ms=300000 (5 min) + 这次跑了 30 min,**所有 task 都已经超过 TTL,自然过期**

如果生产上跑长时间,这个 leak 不存在(TTL 兜底)。

---

## 5. db_rpc consumer 模型小结(深挖 #1 总结,不做代码改动)

代码核心:`go/db/internal/kafka/key_ordered_consumer.go`

### 架构

```
KeyOrderedKafkaConsumer
  ├─ workers: map[partition]*worker  // 1 worker per partition
  │
  ├─ consumer.Consume(topic, handler)        // sarama ConsumerGroupHandler
  │      └─ Setup/Cleanup/ConsumeClaim       // 每个 partition 一个 ConsumeClaim
  │            └─ 把 kafkaMsg 路由到 workers[partition].taskCh
  │
  └─ retry consumer (Redis-backed)
        └─ 1s tick → RPop kafka:retry:{topic} → 路由到 workers[partition].taskCh

worker.start():
  for {
    drain taskCh into batch (单次最多 batch 大小)
    processTaskBatch(batch):
      → 同 (key, msgType) 合并写 (saved round trip)
      → handleTask each → processDBTask
        → tryLockAndProcess (有 expand 模式时)
          ├─ Redis lock (tryLock kafka:consumer:lock:{key}, TTL=5s)
          ├─ processTaskWithoutLock
          │  ├─ proto.Unmarshal task.Body
          │  ├─ dbOpHandlers["read"|"write"] = handleDBReadOp / handleDBWriteOp
          │  │   └─ MySQL FindOneByWhereClause / Save
          │  │   └─ Redis Set cache
          │  │   └─ Redis LPush task:result:{taskId}
          │  │   └─ Redis Publish task:result:notify
          │  └─ release lock
        → markAppliedSeq (成功)
        → 或 saveToRetryQueue (失败 → Redis kafka:retry:{topic})
      → MarkMessage (commit Kafka offset)
  }
```

### 容量

- **partition=10 = 并行 worker=10**(单进程内 goroutine)
- **worker 内串行**:不能并行处理 task,batch 也是串行循环,只是省了多次 channel hop
- **MySQL 池 MaxOpenConn=10**:每个 worker 拿一个 connection 跑 SELECT/INSERT

### 当前瓶颈

90 task/s 输入,partition=10 = 9/partition/s 输入。worker 处理速度上限大约 10-15/s/worker(MySQL 50-100ms × 1 task)。**勉强够用,但完全没有 burst 余量**。任何 GC pause / Redis 抖动都会让 partition lag 累积。

实测 Round 5 lag 91k → 单 partition 9k → 即每个 worker 后面排着 ~9000 个 task → 单 task 100ms × 9000 = 15 分钟 backlog。这就是 robot 停了 30 分钟 lag 还有 91k 的原因。

### 不做改动的理由

加了 db_rpc 端打点($32) 是有用的,但**真凶已经定位**:MySQL 池子 + 单 worker 串行。改 MaxOpenConn 30 + 实测 backlog 是否消化是更直接的下一步。打点等下次再做。

---

## 6. 其它代码陷阱(深挖 #6)

逐项扫一遍:

### 6.1 lock heartbeat 在 chainCancel 后的泄漏 — 无问题

```go
stopHeartbeat := tryLocker.StartHeartbeat(...)
// stopHeartbeat 在 onPreloadComplete 第一行调
```

heartbeat 是 goroutine + ticker,`stopHeartbeat()` 关 ticker + 关 channel,确认无 goroutine leak(`pkg/locker/redis_locker.go` 看实现 OK)。

### 6.2 saveToRedis 失败时锁释放路径

```go
finalize := func() {
    fireOnce.Do(func() {
        if errPtr := firstErr.Load(); errPtr != nil { onComplete(err); return }
        if err := saveToRedis(...); err != nil {
            onComplete(fmt.Errorf("set parent key %s: %w", parentKey, err))
            return
        }
        onComplete(nil)
    })
}
```

`saveToRedis` 失败也调 onComplete(err),onPreloadComplete 走失败分支,正常 releaseLock。**无 leak。**

### 6.3 同 playerId 多个 in-flight preload — 可能存在但被 player_locker 兜住

robot 重连风暴时同 playerId 多次 EnterGame,每次都尝试 TryLock player_locker:

```
Robot1 (playerId=X) → EnterGame → TryLock OK → 异步链跑 (5s)
Robot1 (playerId=X) 6s 后又来 → TryLock 失败 → err25
                         ↑ player_locker 仍被前一次持有
```

**这就是 partition 5→10 文档里 §5 描述的 "锁滞留" 现象**。GC=1s 之后 dispatcherTaskTTL=5s 真生效,锁最多滞留 ~5-6s,所以 robot 6s 重连刚好命中之后。**这是设计上的边界条件,不是 bug**,但建议把 PlayerLockTTL 从 120s 降到 30s,让真出问题时(异步链 crash 而非 timeout)恢复更快。

### 6.4 sub-cache 部分写入导致脏读 — 隐患存在

```go
for _, sub := range subs {
    subKey := ...
    if val, err := rc.Get(ctx, subKey).Bytes(); err == nil && val != nil {
        proto.Unmarshal(val, sub)  // 部分 sub 用 cache
        continue
    }
    tasks = append(tasks, ...)     // 部分 sub 走 db
}
```

**风险**:如果 sub_A 命中 cache(老值),sub_B 走 db(新值),最后 saveToRedis 把 parent key 写出去,parent 里 A 是老的 / B 是新的 — **不一致**。

但实际上单玩家的 sub_table 写入由 db_rpc 串行(同 key hash 同 partition,单 worker 串行),且 cache TTL 跟 parent 同步过期。所以**实战中很少出现一半新一半老的 parent**。但**理论上仍是个隐患**,建议:
- 要么 sub_cache 全命中走 cache,**只要有一个 miss 就全走 db**(最简单)
- 要么 sub_cache 整体加 version,parent 写出之前比较 sub 之间 version

不在这一轮修。

### 6.5 TaskResult LPop 的并发性 — 无并发竞争

dispatcher 单 subscriber goroutine,take(taskID) 取出 callback 后 LPop 一次。Redis LPop 自身原子。**无 race。**

### 6.6 db worker 串行批处理可能跨 task 副作用串错 — 检查 batch 合并逻辑

`processTaskBatch:447` 注释 "coalesces consecutive writes for the same (key, msg_type)"。这意味着同一个 player + 同一个 msgType 的多条 write 会合并为一次 write。

**风险**:如果合并逻辑写错,可能丢中间的 task 状态(比如 W1 + W2 合并,但 W2 里改了 W1 的字段)。protobuf marshal/unmarshal 是整体替换,所以最后的 W2 包含所有最新值,**理论上正确**。但如果 task body 是 partial update(只改某些字段),合并逻辑就错了。

需要看 `processTaskBatch` 内部的具体合并实现 — 这次没看完整。**建议下次单独审 batch coalescing 逻辑** + 加单测验证。

### 6.7 onComplete 多次 fire 防护 — fireOnce 保护

```go
var fireOnce sync.Once
finalize := func() {
    fireOnce.Do(func() { ... onComplete(...) })
}
```

`sync.Once` 保证 onComplete 只跑一次。**OK。**

### 6.8 PreloadPool nonblocking + 满了就 drop — 没问题

ants.Pool nonblocking,满了返回 ErrPoolOverload → SubmitPreload 返回 false → entergame 返回 err25 (line 272 那个 (D))。**正确处理 backpressure。**

---

## 7. 最终全局图

```
┌────────────────────────────────────────────────────────────────────┐
│ EnterGame end-to-end (Round 5 GC=1s 之后)                            │
├────────────────────────────────────────────────────────────────────┤
│                                                                    │
│ Robot ─► /api/assign-gate ─► Java Gateway ─► login.AssignGate      │
│                                            (40-44ms 同步成功)        │
│ Robot ─► EnterGame RPC ─► login.EnterGame                           │
│   ┌───────────────────────────────────────────────────────────┐    │
│   │ TryLock player_locker (line 80) │                          │    │
│   │   ├─ 成功 → 启异步链, RPC 立即返回 code=0 给 robot          │    │
│   │   └─ 失败 → err25 (line 87)                                │    │
│   │                                                           │    │
│   │ 异步链 SubmitPreload(EnsurePlayerAllDataInRedisAsync):    │    │
│   │   ├─ Redis EXISTS parent_key                              │    │
│   │   │     ├─ HIT (99.8%) → onComplete(nil) → 释放锁          │    │
│   │   │     └─ MISS (0.2%) ↓                                   │    │
│   │   ├─ Redis GET sub_keys × N                               │    │
│   │   ├─ dispatcher.Register × N (TTL=5s)                     │    │
│   │   ├─ Kafka producer SyncProducer.SendMessages × N          │    │
│   │   │     │ 进 partition[hash(playerId)]                    │    │
│   │   │     │   ↓ 排队 (实测 ~9000 tasks ahead)                │    │
│   │   │     │   ↓ db_rpc consumer worker (串行,1 per partition)│    │
│   │   │     │     ↓ MySQL pool (MaxOpenConn=10) ← 隐藏瓶颈      │    │
│   │   │     │     ↓ MySQL SELECT (50-100ms)                   │    │
│   │   │     │     ↓ Redis cache write-back                    │    │
│   │   │     │     ↓ Redis LPush task:result:{taskId}          │    │
│   │   │     │     ↓ Redis Publish task:result:notify          │    │
│   │   │     │   <─ 总耗时 30-60s if backlog                    │    │
│   │   │                                                        │    │
│   │   ├─ dispatcher 收到 publish, take(taskID), LPop result    │    │
│   │   │   ├─ 在 5s TTL 内收到 → 调 callback(res,nil) → finalize │    │
│   │   │   │     → saveToRedis(parent_key) ← 这里 PlayerAllData  │    │
│   │   │   │       现在终于在 Redis                              │    │
│   │   │   └─ TTL 5s 之后 → sweepExpired 触发 timeout cb (err)   │    │
│   │   │       → finalize 走 firstErr 路径 → onComplete(err)    │    │
│   │   │                                                        │    │
│   │   └─ onPreloadComplete:                                   │    │
│   │       ├─ err == nil:                                      │    │
│   │       │   applyLoadedPlayerSession                         │    │
│   │       │   ├─ player_locator.SetSession(state=Online)       │    │
│   │       │   ├─ Kafka SendBindSessionToGate (gate-{N})        │    │
│   │       │   └─ scene_manager.EnterScene (gRPC)               │    │
│   │       │     → Scene receive, spawn player                  │    │
│   │       │       ├─ playerRedis.Load(playerId) ← GET PlayerAllData│
│   │       │       │     ├─ HIT: spawn 完成,Scene push READY    │    │
│   │       │       │     └─ NIL: 进 pending_retry_queue          │    │
│   │       │       │           backoff 2,4,8,16,32,60s × 6       │    │
│   │       │       │           total budget 122s                 │    │
│   │       │       │           直到 db 那边终于把 parent 写进 Redis │    │
│   │       │       │           → 终于 spawn,robot 收到 scene-ready │    │
│   │       └─ err != nil:                                      │    │
│   │           只 release lock,什么都不做                        │    │
│   │           → Robot 等不到 scene-ready                       │    │
│   │           → access_token reconnect 路径 → 又来一次 EnterGame│   │
│   │           → 直到某次 preload 终于成功                       │    │
│   │                                                           │    │
│   └───────────────────────────────────────────────────────────┘    │
└────────────────────────────────────────────────────────────────────┘
```

### 真凶单一根因(在容量层面)

**MaxOpenConn=10 + 单 worker 串行 + 200/s 稳态输入 = partition lag 必然累积**。

所有上层修复(partition 5→10、TTL 5s、GC 1s)都是在**这个底层瓶颈之上**减小可见症状,真要根治必须从 db 端下手。

---

## 8. 修复 ROI 排序(下次该做的)

1. **MaxOpenConn 10 → 30**(db.yaml 1 行)。理论上能让单 partition 处理速度从 ~10/s 提到 ~30/s,backlog 应该几乎消失。**预期效果**:Round 6 实测 preload_failed 从 46% 降到 5% 以内。
2. **db_rpc consumer 内部加打点**($32)。MySQL SELECT / Redis LPush / Pub/Sub publish 三段分别。验证 MaxOpenConn 改完后真凶是否真的解掉。
3. **审查 HandlePlayerAsyncLoadFailed 实际行为**(scene 端 122s 兜底失败时玩家体验)。决定生产上线前是否要加更长重试 / 人工干预流程。
4. **PlayerLockTTL 120s → 30s**(login.yaml 1 行)。低风险,故障恢复加速。
5. **审 processTaskBatch 内的 coalesce 逻辑**(深挖 6.6)。加单测验证 partial update 场景。
6. **sub_cache 部分命中导致 parent 不一致风险**(深挖 6.4)。改 "全或无" 策略。

---

## 9. Run artifacts 引用

- Round 5 (GC fix) prom snapshots: `robot/logs/stress-1zone-25k-gcfix-20260528-082147/prom-snapshots/`
  - t1m_baseline.txt
  - t5m_ramp-complete.txt
  - t9m_post-knee.txt(过去丢的,在 Round 4)
  - t23m_final-clean.txt
- Round 4 (1+3) snapshots: `robot/logs/stress-1zone-25k-fix1-3-20260528-015611/prom-snapshots/`
- Round 3 (f-only) snapshots: `robot/logs/stress-1zone-25k-f-instrumented-20260528-000430/prom-snapshots/`

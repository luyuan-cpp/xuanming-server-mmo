# 玩家异步存盘的丢失窗口（置脏 / 快照跳过 / 重试耗尽）

**Created:** 2026-08-03
**适用:** C++ scene 节点的玩家存盘链（`PlayerLifecycleSystem` + `MessageAsyncClient`）
**相关:** [`db_write_behind_dirty_flag_race.md`](db_write_behind_dirty_flag_race.md)（那份讲的是 **db 服务** 的写回脏标记，不是本文的 scene 侧存盘）、[`proto-compare-dirty-save.md`](proto-compare-dirty-save.md)、[`async-load-disconnect-reconnect-race.md`](async-load-disconnect-reconnect-race.md)

> 为什么单独立一份：`db_write_behind_dirty_flag_race.md` 讨论的是 Go 侧 db 服务「扫脏 key → 批量刷 MySQL」的方案选型，结论是不采用外部脏扫描。但 **scene 节点自己也有一套"置脏 + 异步存盘"**，走的是完全不同的代码路径（proto-compare 快路径 + hiredis 异步回调），它的丢失窗口从没被单独记录过。本文补这一份。

---

## 1. 这条链长什么样

```
业务系统改数据（如 CurrencySystem::AddCurrency 里 comp->dirty = true）
        ↓
周期 tick / 退出流程 调用 SavePlayerToRedis(player)
        ↓
PlayerAllDataMessageFieldsMarshal → message(V_now)
        ↓
【快路径】若 PlayerLastPersistedSnapshotComp.snapshot == V_now → 跳过,return false
        ↓
tlsRedisSystem.GetPlayerDataRedis()->Save(message, playerId)   ← 异步
        ↓  （同时另发 Kafka DBTask 给 db 服务写 MySQL,两条路互不依赖）
hiredis 回调 OnSaved（同一 Redis key 最多一个写在途）
        ├─ reply 为空 / REDIS_REPLY_ERROR → QueueSaveForRetry(6 次后告警,16s 封顶持续重试)
        ├─ 有更新的完整快照等待 → 立即写最新值,旧回调不结束生命周期
        └─ 最新值成功 → save_callback_ → HandlePlayerAsyncSaved
                    ├─ 退出流程: FinishExitAfterPersist → 销毁实体 / 清 session
                    └─ snap.Replace(message)   ← 记录"已持久化 = V_now"
```

关键不变量：**`PlayerLastPersistedSnapshotComp` 只在存盘成功后更新**。快路径靠它判断"要写的内容和盘上一样，可以跳过"。

---

## 2. 两套 Redis key 是分开的（理解后面所有问题的前提）

| 写者 | key | 读者 |
|---|---|---|
| C++ scene `MessageAsyncClient<Guid, PlayerAllData>` | `<PlayerAllData full_name>:{playerId}` | C++ scene 自己（`AsyncLoad`，玩家进场加载） |
| Go db 服务 `handleDBWriteOp` 的写回 | `player_database:{playerId}` / `player_database_1:{playerId}` | login 的读路径 |

**这两组 key 互不覆盖。** 所以 `redis_client.h` 里那句
`"giving up; cache will be stale until dbservice rewrites"`
对 scene 自己的加载路径是**不成立的**：db 服务重写的是 login 用的分表 key，`PlayerAllData:{id}` 不会被任何人修复。

---

## 3. 已经安全的部分（别重复"修"）

审计时逐条验过，下面这些**不是**漏洞：

- **存盘失败不会污染快照。** `OnSaved` 只在成功分支调 `save_callback_`；null reply 与 `REDIS_REPLY_ERROR` 都走 `QueueSaveForRetry`。所以不存在"写失败却记成已持久化、后续被快路径永久跳过"的情形。
- **同 key 存盘串行且只保留最新待写值。** `saving_queue_` 保证同一 key 最多一个 Redis 写在途，`pending_save_queue_[redis_key]` 合并后续完整快照。旧写失败时若已有更新值，直接丢弃旧重试并写最新值。
- **旧完成不会抢先结束生命周期。** 周期存盘在途期间若登出又生成了新快照，旧成功回调不会触发 `HandlePlayerAsyncSaved`；必须等最新快照成功，才允许更新 snapshot、清 session 和销毁实体。这同时关闭了“旧失败重试晚于新成功、把 Redis 覆盖回旧值”的窗口。
- **快路径跳过不会卡住退出。** `HandleExitGameNode` 消费 `SavePlayerToRedis` 的返回值，`false` 时直接 inline 跑 `FinishExitAfterPersist`（AFK 踢下线最容易命中这条）。
- **首次存盘不会被跳过。** 无 snapshot 时 `ShouldPersist(current, nullptr)` 恒为 true。

---

## 4. 真实的丢失窗口

### 4.1 【已修】重试耗尽后静默放弃 —— 数据丢失 + 退出流程永久悬挂

**位置：** `cpp/libs/engine/infra/storage/redis_client/redis_client.h` `QueueSaveForRetry`

```cpp
if (element->retry_count >= kMaxSaveRetries)   // 6 次
{
    LOG_ERROR << "Save exhausted ... giving up; cache will be stale until dbservice rewrites";
    return;                                    // ← 到此为止,没有任何人被通知
}
```

**后果一（数据丢失 / 回档）：** `PlayerAllData:{id}` 停留在上一次成功存盘的内容。玩家下次进场从这个 key 加载，两次存盘之间的全部进度消失。第 2 节已说明 db 服务修不了这个 key。

**后果二（退出流程永久悬挂）：** 退出路径拿到 `savePending == true`，此后完全依赖 `HandlePlayerAsyncSaved` 去跑 `FinishExitAfterPersist`。回调不来 →
`UnregisterPlayer` 标记永远留在实体上 → 实体、`tlsEcs.playerList` 条目、`SessionMap` 条目全部不清理；`IsSaveInFlight(playerId)` 恒为 true，节点疏散/停机 drain 的收敛判定会一直等它（有界看门狗到期后强行推进，但期间日志会指向错误方向）。

**为什么之前没被发现：** 只有一条 `LOG_ERROR`，且措辞暗示"有人会兜底"。加载路径有 `load_failed_callback_`，存盘路径**没有对称的失败回调**——这是接口层面的不对称，调用方无从感知。

**修法（本轮已落码）：** 给 `MessageAsyncClient` 增加与 `SetLoadFailedCallback` 对称的 `SetSaveFailedCallback`。连续失败达到 6 次时只触发一次高危告警，**不再丢弃 payload**；最新值继续按 16 秒封顶退避重试，直到 Redis 恢复或进程进入已有的有界停机流程。scene 侧收到通知后：
1. 打 ERROR 明示数据持久化风险（player_id / redis_key / 重试次数），**不**更新 snapshot；
2. 若玩家正在退出，保留带 `UnregisterPlayer` 的实体与唯一内存态，等待排队存盘成功后再由正常成功回调收尾。不能为了消除 session 悬挂而主动制造确定性回档；节点 drain 自己已有看门狗负责限制停机时间。

同一 key 的写同时经过 `saving_queue_` 串行化：重连时未收到回调的在途写会回到待写队列；若已有更新快照则保留更新者。只有“当前 key 没有更新值等待”的最新成功写才发布生命周期完成回调。

**仍然诚实存在的边界：** Redis 长时间不可用并持续到进程被停机看门狗强制推进时，内存中的最新状态仍可能丢失。彻底关闭需要把完整 `PlayerAllData` 写入独立的 durable outbox，或让下一次加载能从 MySQL 分表重建完整父消息；这不是“多重试几次”能够证明解决的。

### 4.2 【已修】补缴欠款 `debts` 根本没进过存盘

`PlayerCurrencyComp::LoadFromProto / SaveToProto` 写好了却全仓零调用，`PlayerDatabaseMessageFieldsMarshal` 只 `CopyFrom(CurrencyComp)`。GM 挂的欠款与 `AddCurrency` 已扣到一半的 `debt.paid` 进度，玩家一下线重登就整体消失。

注意顺序陷阱：`SaveToProto` 必须在 `mutable_currency()->CopyFrom(...)` **之后**调，否则 CopyFrom 会把刚写进去的 debts 覆盖掉。（本轮已按此顺序落码。）

### 4.3 【以 fail-closed 封住，能力仍未实现】跨节点切场景没有存盘屏障

旧流程中的 `scene_manager::dispatchReleasePlayer` 是 detached goroutine，新节点从 Redis 加载与旧节点存盘之间没有屏障。同步等待也堵不住：C++ 的 `ReleasePlayer` RPC 只保证 `HandleExitGameNode` **入队**了存盘，不保证已落盘。

2026-08-03 起生产默认 `AllowUnsafeCrossNodeHandoff=false`：已有位置的跨节点/跨 zone 请求在 ReleasePlayer、location 更新和成功 Gate 路由之前拒绝，目标场景预留会回滚。首次落点和同物理节点切场景不受影响。开发环境可显式打开旧流程，但不能据此声明跨节点能力可生产。

Gate 路由改为等待 Kafka `RequireOne` broker ACK，只解决“路由命令是否被 broker 接收”，**不是**玩家状态持久化屏障。Kafka offset 也只是单 partition 到达顺序，不是玩家状态因果版本；新节点从旧快照派生出的保存仍可能覆盖旧节点的较新状态。

真正恢复能力仍需要 per-player 交接 epoch（旧节点落盘后写标记，新节点 load 前校验，未就绪则有界退避）——这是跨 Go/C++/db 三段的协议改动。

### 4.4 【设计限制,不是 bug】快照随实体销毁而丢失

`PlayerLastPersistedSnapshotComp` 只存在于存活实体上。重连销毁+重建实体后快照没了，进场后第一次存盘必然实写一次。这是**刻意**的保守取舍：宁可多写一次，也不冒"内存态与 Redis 已经分叉却被跳过"的风险。不要"优化"掉。

### 4.5 【已修】损坏载荷误走加载成功，可能用默认玩家覆盖权威状态

`MessageAsyncClient::OnLoaded` 旧逻辑在载荷超过 `ParseFromArray` 的 `int` 长度上限，或 protobuf 解析失败时，只记录错误，随后仍调用 `load_callback_`。此时回调拿到的是默认或部分解析消息；Scene 又会把 `player_id=0` 补成当前玩家并按“新玩家”继续初始化，后续存盘就可能把默认值写回 `PlayerAllData:{id}`，把一次缓存损坏放大成权威玩家数据丢失。

本轮改为 fail-closed：只有完整解析成功的字符串载荷才调用 `load_callback_`。超长、空值、空指针、protobuf 解析失败和异常 reply 类型都清理在途/重试状态，释放默认或部分解析消息，并以 `RedisError` 调用 `load_failed_callback_`；不会初始化默认玩家，也不会让 key 永久挂在加载队列。只有有界重试耗尽后的 `REDIS_REPLY_NIL` 才以 `DataNotFound` 表示“确实不存在”。

`currency_test` 通过窄 test peer 直接注入单字节损坏 protobuf 与大于 `INT_MAX` 的 reply 长度，分别断言：成功回调为零、失败回调恰好一次且原因是 `RedisError`、在途与待重试队列均清空。超长用伪造 `len` 验证前置边界，不分配 2GB 内存。

---

## 5. 改这条链时的检查清单

1. 新增任何"跳过存盘"的快路径，必须回答：**跳过的依据是否只在存盘成功后才更新？** 依据一旦能在失败路径上被更新，就是永久丢数据。
2. 新增任何异步存盘的调用方，必须回答：**失败时谁被通知？** 只有 `LOG_ERROR` 不算通知。加载路径有 `load_failed_callback_`，存盘路径有 `save_failed_callback_`，两边都要接。
3. 任何依赖"回调一定会来"的收尾逻辑（销毁实体 / 清 session / 解锁），必须有失败分支，否则就是玩家永久悬挂。
4. 动 Redis key 命名前，先确认 scene 与 login/db 是不是同一组 key（见第 2 节）——它们目前**不是**，任何"另一边会帮我修好"的假设都要先证伪。
5. 组件里加了 `dirty` 之类的标志位，要 grep 确认真有读者。本轮就抓到 `PlayerCurrencyComp::dirty` 与 `debts` 的序列化桥写好了却无人调用。

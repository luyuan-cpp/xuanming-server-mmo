# Friend Service Persistence Architecture

> **2026-09-18/19 已随 friend 移植更新到新形态**(见 [friend-port-20260918.md](./friend-port-20260918.md))。
> 本次改动的段落:Data Characteristics(独占库 `mmorpg_friend` + 四张表 + 双 Redis 句柄)、
> 「实现: 显式容量锁行」、「锁序纪律」(新增)、「拉黑表 `friend_block`」(新增)、
> 「存量容量回填门禁」(标为已退役)、「错误处理」(改按 RPC 映射)、Online/Offline(改拉模型)、
> 「与 S2C 推送的分工」(新增)。其余段落(与玩家数据的对比、dirty-flag、上限方案对比、
> 未来分片)**未改动,仍是原文**。
> ⚠ 相关代码**未编译、未运行测试**,行为以验证结果为准。

## Data Characteristics
- **Global cross-zone**: friend 全服一份、多副本、进程无状态;业务代码禁止读 `cfg.ZoneId` 做分支,ZoneId 只影响 etcd 注册路径
- **独占逻辑库 `mmorpg_friend`**(port-decisions D-14 第 1 条):本地与 K8s 同名,不落 `zone_{N}_db`、不借 `mmorpg_global`、不往共享库 `mmorpg` 加表。表结构的唯一事实源是 `proto/friend/friend_table.proto`,建表 / 加列只经 `go/schemamigrate`(服务二进制 `-migrate`)
- **四张表**:`friend`(双向边,每对好友两行)/ `friend_request`(每对 (from,to) 一行)/ `friend_capacity`(显式计数锁行)/ `friend_block`(单向拉黑,本次新增)
- **两个 Redis 句柄**(契约 §4),`Redis DB 3` 的写法已作废:
  - `SharedRedis`:跨运行时契约 key(`player:session:{id}`)的**唯一句柄**,禁 Cluster 分片;
  - `FriendRedis`:friend 私有 key(好友列表缓存、申请缓存、频率配额),可 Cluster;未配置时回落共享库并打 WARN
- **Low-frequency writes**: add/remove friend = a few ops per player per day
- **Relationship data**: bidirectional (A<->B), needs transactional consistency
- **Hard upper limit**: MaxFriends=200, must never exceed (no soft tolerance)

## Persistence Pattern: Direct MySQL + Redis Cache-Aside

Friend/Guild 这类低频关系型数据不适合 Kafka write-behind（那是给玩家高频数据用的），直接写 MySQL + cache-aside。

```
写路径: 业务操作 -> MySQL 事务(先写) -> 成功后 -> 删 Redis 缓存
读路径: Redis GET -> miss -> singleflight MySQL load -> 回填 Redis
```

### 与玩家数据持久化的对比

| 特征 | 玩家数据 | Friend / Guild |
|------|----------|----------------|
| 数据归属 | 单实体(一个玩家) | 共享/关系型(多对多) |
| 写入量 | 高频(背包/任务随时变) | 低频(加好友偶尔操作) |
| 一致性要求 | 最终一致即可 | 操作级一致(加好友立即可见) |
| 持久化方式 | Kafka write-behind | 直接写 MySQL |

### 为什么不用 dirty-flag + 定时 flush

dirty-flag 解决的是"写太频繁，需要攒批"的问题。Friend/Guild 每分钟只有几次写入，MySQL 轻松处理，攒批收益为零。反而引入：
- 加好友延迟可见（等 flush 周期）
- 宕机丢关系数据
- 双向写原子性难保证（flush A 成功 B 失败 → 单向好友）

**结论**: dirty-flag 是高频写入的优化手段，不是通用持久化模式。

## 好友上限强制保证 — 方案对比

### 三种方案

| | 容忍软上限 | 预占名额 | 同步硬检查(采用) |
|--|--|--|--|
| 是否超限 | 可能超1-2 | 不超 | **不超** |
| 名额被锁 | 不锁 | 锁(申请未回复=占名额) | **不锁** |
| 用户体验 | 好 | 差(发了申请加不了别人) | **好** |
| 单向好友风险 | 有短暂窗口 | 无 | **无(回滚兜底)** |
| 实现复杂度 | 最低 | 中 | 高 |

### 预占名额的问题
发申请时预占一个好友位 → 对方很久不回复 → 名额被锁 → 影响正常加好友。
设超时释放机制也不理想：发20个申请锁20个名额，体验很差。

### 选择: 同步硬检查
"同意"时用 FOR UPDATE 行锁原子检查 + 写入，代价是多一次事务（低频操作可接受）。

### 实现: 显式容量锁行(2026-09-18 起,就绪门禁已退役)

```sql
-- 事务外(自动提交):按 player_id 升序补齐双方容量行。
-- 必须在事务外:放进事务会让多个请求各持自己刚插入的新行、又都抢同一个接收者行,
-- 形成 insert-intention 死锁。缺行时按 friend 表的权威边数算初值,绝不猜 0。
INSERT IGNORE INTO friend_capacity (player_id, friend_count)
  SELECT ?, COUNT(*) FROM friend WHERE player_id = ?;

BEGIN;  -- READ COMMITTED
-- ① 容量守卫必须是事务里的第一把锁(见下一节的锁序纪律)
SELECT player_id, friend_count FROM friend_capacity
 WHERE player_id IN (A, B) ORDER BY player_id FOR UPDATE;
-- ② 守卫之后才做判定读,且一律是 FOR UPDATE 当前读:
--    双向拉黑(friend_block)、双向好友边(friend)、申请行(friend_request)、双方 friend_count
-- ③ 写入
UPDATE friend_request SET status=2, updated_ms=? WHERE ...;
INSERT IGNORE INTO friend ... (双向);
-- 每个实际新插入的方向只给该方向的 player_id 加 1;历史单向边不会重复计数
UPDATE friend_capacity SET friend_count=friend_count+1 WHERE player_id=<新边的 player_id>;
COMMIT;
-- 提交后:失效缓存 → S2C 推送(都在事务外;提交之后的失败不得改变方法返回值)
```

- `friend_capacity` 的固定主键行让共享任一玩家的写事务串行化,不依赖
  `COUNT ... FOR UPDATE` 在不同隔离级别下不稳定的 gap-lock 语义
- 单 MySQL 实例,一个事务搞定,不需要分布式事务
- 低频操作(每天几次),行锁开销可忽略

### 锁序纪律(2026-09-18 新增,**改任何写路径之前必须读完**)

**任何写事务,在拿到容量守卫之前,不得做任何锁定读。** 这是 A 仓 2026-08-11 在真 MySQL 8.4
上压测出 1213 的根因结论:"我只锁了本对玩家的行"这个前提不成立 —— 未命中的锁定读在 RR 下锁的是
该键所在的**间隙**,而间隙是跨玩家对共享的。

- **`AcceptFriend` 的申请行 `FOR UPDATE` 已从守卫之前下移到守卫之后**。原因:新增的 `AddFriend`
  权威事务会先锁双方容量行再碰申请行,与旧顺序正好互为 ABBA —— 同一对玩家"一边接受、一边重发申请"
  就能撞上 1213。这不会把当初修掉的那个 1213 带回来:那次修的是"过早把 status 从 1 改成 2 会在
  `idx_to_player` 前缀上触发 1213",修法是把 **UPDATE** 延后,而 UPDATE 仍在守卫之后。
- **隔离级别固定 READ COMMITTED**。RR 的间隙锁会让"同一玩家并发拉黑 16 个不同目标"这类只碰不同行的
  事务互相挡并成环(A 仓实测必炸,且只在 MySQL 上炸 —— TiDB 没有间隙锁)。前提是
  `binlog_format=ROW`(`deploy/k8s/manifests/infra/mysql.yaml:60` 显式设置)。
- **代价:RC 下同一事务内两次普通 SELECT 可能读到不同结果**,所以"守卫之后的判定读用当前读"不是
  可选优化,是正确性要求。
- **容量守卫行同时是"这一对玩家"的串行化载体**:`AddFriend` / `AcceptFriend` / `RemoveFriend` /
  `Block` 都锁同一对容量行,于是两两互斥。RC 下的探针自己挡不住并发插入,挡住并发的是这把守卫;
  新增写路径时先拿守卫,否则所有"权威判定"会静默退化成 check-then-act。

回归证据在 `go/friend/internal/data/friend_guard_lock_order_mysql_test.go`(真 MySQL 门控,
`FRIEND_TEST_MYSQL_DSN`)。**全体 SKIP 时 `go test` 退出码仍是 0**,所以验收必须确认看到 PASS。

### 拉黑表 `friend_block`(2026-09-18 新增)

单向拉黑,主键 `(player_id, blocked_player_id)`,反向索引 `blocked_player_id`(判"对方是否拉黑了我")。
与 `friend` 分表而不是在边上加 flag:拉黑关系是单向的,且被拉黑的人**通常不是**好友,塞进好友边就
无处存放。`Block` 在同一把容量守卫内两个方向各删一次好友边,并**按 `RowsAffected` 给对应的
`friend_count` 减 1** —— 漏减就是计数上漂、玩家永远加不满好友且零报错。

### 存量容量回填门禁 —— **已退役(2026-09-18)**

> `friend_capacity_backfill_v1` 这道 durable readiness gate **已随 friend 移植退役**,
> 代码里的 `RequireFriendCapacityReady` 及其三个调用点、启动期拒启探测全部删除。
> 完整裁定、四条理由与代价见 **[D-10 修订(2026-09-18)](./xuanming-port-decisions-20260910.md)**。
> 一句话:它查的 `guild_schema_migration` 按 D-14 第 8 条留在旧共享库 `mmorpg`,而 friend 现在断言
> 自己只连 `mmorpg_friend` —— 这条查询在任何合法配置下都不可能命中,是可证明的死代码;
> 而它要挡的"半迁移"风险在由 `schemamigrate` 从基线建全、没有任何历史边的新库里结构性不存在。

下面这段是**退役前**的形态,保留以便理解当时为什么需要它:

> MySQL DDL 会隐式提交,因此 `friend_capacity` 表存在不代表历史 `friend` 已回填。
> 迁移先持久化 `friend_capacity_backfill_v1=pending`,再在一个事务里完成:① 现有容量归零;
> ② 按 `friend.player_id` 权威边数重算;③ gate 更新为 `ready`。任一步中止都会回滚容量 DML,
> gate 保持 pending;Friend 服务在对外注册前以及每个容量写事务内都检查 gate,缺失或 pending 直接拒绝。
> 部署顺序必须是:停止所有旧版 Friend 写实例 → 完整执行迁移脚本并确认 gate 为 `ready` → 启动新版 Friend。

**退役后仍然有效的 D-10 实质不变量(一条都没动)**:

1. `friend_capacity` 的显式计数行是好友数硬上限;所有写 friend 边的路径都在同一把容量锁里,
   按 `RowsAffected` 增减 `friend_count`。
2. **缺容量行时按 `friend` 表的权威边数建行,绝不猜 0** —— 猜 0 等于凭空发一整份好友名额。
3. 锁序纪律(见上一节;相对 D-10 原文的唯一变化是申请行锁下移到守卫之后,已在 D-10 修订里记录)。
4. versioned cache generation + Lua CAS,写路径提交后失效缓存。

**残留**:旧共享库 `mmorpg` 里的 friend 三张表不再有任何写者,成为孤儿;已初始化的存量卷 / PVC 里
它们仍然存在(initdb 只在空卷执行),清理需要人工 DDL。若将来真要把存量边导进 `mmorpg_friend`,
**必须先重新设计一次带 durable 标记的回填门禁**,不能直接跑导入。

### 错误处理

好友数上限全域只有 `ErrSenderFriendsFull` / `ErrAcceptorFriendsFull` **一对**哨兵
(Sender = 申请发起方,Acceptor = 接收方),但它们**按 RPC 分别映射**成相反的 tip 码 ——
`AddFriend` 的调用者是 Sender,`AcceptFriend` 的调用者是 Acceptor:

| 哨兵 | AddFriend(me=Sender) | AcceptFriend(me=Acceptor) |
|---|---|---|
| `ErrSenderFriendsFull` | 「你的好友已满」 | 「对方好友已满」 |
| `ErrAcceptorFriendsFull` | 「对方好友已满」 | 「你的好友已满」 |

把其中一条 switch 照抄到另一条路径上会得到**正好相反**的文案,而编译器与单测都看不出来。
映射表的唯一事实源在 `go/friend/internal/data/friend_repo.go:28-37`。

拉黑 `ErrBlocked` **刻意不区分方向**:告诉申请人"是对方拉黑了你"等于把别人的拉黑设置泄露给他。

## 未来分片方案 (1亿+ 玩家)

### 分片策略
- `shard_id = player_id % N`，每个玩家的好友列表固定落在一个分片
- 每人一行存序列化的好友列表，1亿玩家/16分片 = 每片 ~625万行

### 跨分片加好友
```
B 同意:
  1. 同步 RPC -> A 的分片: FOR UPDATE 检查 + 写入(原子)
     A 满了 -> 返回失败，结束
     成功 -> 继续
  2. 本地事务 -> B 的分片: FOR UPDATE 检查 + 写入
     B 满了 -> 同步回滚 A（失败则 Kafka 补偿队列兜底）
     成功 -> 完成
```

### 扩容
翻倍扩容(16->32)，每个分片拆成两半，简单且可预测。

## Online/Offline 状态

### 问题
好友上下线频繁，200好友 x 10K登录/秒 = 2M推送/秒，push 模式开销太大。

### 方案: 读契约 key `player:session:{id}` 的 Pull Model(2026-09-18 改)

> ⚠ **原方案里的 `friend:online:{playerID}` + 心跳续期那一套从未实现过**,已整套删除。
> 它的唯一写者是 `NotifyOnline` / `NotifyOffline` 两个 rpc,而这两个 rpc 在全仓非生成代码里
> **零调用方** —— 也就是说那个键从来没有人写过,`GetFriendList` 里的 `is_online` 恒为 false,
> 而且全程零报错。删掉它比留一个"看起来有、其实没有"的机制更安全。
> 顺带:这两个 rpc 是 gate→friend 的东西向调用,按 D-9 也**不能**跟着服务级的 `ClientProtocol`
> 开关对客户端开放(否则任何客户端可声明任意玩家在线),所以它们随协议一起删。

**存储(不是 friend 的,friend 只读)**:

```
Redis Key: player:session:<playerID> -> proto PlayerSession(含 State / GateId / SessionId / last_active_ts)
写者: login / player_locator / gate
```

这是**跨运行时契约 key**(契约 §4),只能经 `SharedRedis` 读,friend 不许给它加自己的 hash tag 或
版本前缀 —— 改了键名就读不到别人写的值,而且全程零报错(GET 未命中在业务上长得像"玩家离线")。

**查询 (Pull)**:

```
GetFriendList / RecommendFriends
  -> 取好友(候选)列表
  -> SharedRedis MGET player:session:<id> ...(按 ListReadHardLimit 分批)
  -> proto.Unmarshal(PlayerSession);只有 State == SESSION_STATE_ONLINE 算在线
  -> last_active_ts 填 last_active_ms
```

实现在 `go/friend/internal/data/session_reader.go`(`BatchOnlineStatus` :104 / `FillOnlineStatus` :152)。

**失败语义:降级为"全部离线",不让好友列表整个失败。** 在线状态是展示态,为它让整个面板打不开不划算;
代价是"好友其实在线却显示离线"。这不是静默降级 —— 每个受影响的玩家都会计
`friend_online_lookup_total{outcome="error"}` 并打限流日志(AGENTS §11.3)。

⚠ 一条容易写错的:**MGET 的返回长度与入参长度不等时必须整批判离线**,不能按下标取值 ——
错位会把 A 的在线状态贴到 B 身上,那比"全部离线"严重得多。

**在线状态不进缓存**:好友列表缓存(`friend:{f:<pid>}:list:v3`,TTL 30 分钟)里只存 id 与 `since_ms`;
`is_online` / `last_active_ms` 每次请求现读。把它写进 30 分钟 TTL 的缓存等于故意返回过期在线状态。

### 为什么不用 Push
| | Pull | Push |
|--|------|------|
| 登录开销 | 0(会话键本来就有人写) | 200次通知(每个好友) |
| 10K并发登录 | 10K ops(还是别人的) | 2M ops + 跨zone路由 |
| 实现复杂度 | 极低 | 需要知道每个好友的gate，跨zone推送 |
| 实时性 | 打开好友列表时刷新 | 实时 |

**结论**: 游戏好友不需要毫秒级实时，打开好友面板时刷新即可。"你的好友 XXX 上线了"这类登录提示属于
presence 订阅层,需要先有订阅关系与投递缓冲,是**独立立项**,不做在 friend 里(见
[friend-port-20260918.md](./friend-port-20260918.md) §5 不做项第 4 条)。

> 原文这里写"可以在 NotifyOnline 中额外查好友列表并通过 Kafka 推送" —— `NotifyOnline` 已随本次移植
> 删除,这条出路不再存在。

### 与 S2C 推送的分工(2026-09-18 新增)

friend 现在**有**一条 S2C 推送通道,但它与在线状态无关,只做两件事:
`AddFriend` 成功 → 推 `REQUEST_RECEIVED` 给 target;`AcceptFriend` 成功 → 推 `REQUEST_ACCEPTED`
给原申请人。Reject / Remove / Block / Unblock 都不推。

投递语义是 **at-most-once**(`kafkautil.PushToPlayer` → `gate-cmd_g<N>` → gate → TCP,无重试、
无回执、无离线补推),所以事件体只带 `reason + by_player_id + ts_ms`:**推送只是"去拉"的触发信号**,
客户端收到后必须去拉 `GetPendingRequests` / `GetFriendList`。推送只在 MySQL 提交之后、事务之外发,
失败只打日志 + 计指标,**绝不影响 RPC 结果**(`internal/logic/push.go` 里所有函数都不返回 error,
从签名上堵死"顺手 return err")。

# S8 玩家存盘属主围栏(B4c 详细设计)

> **状态:已落码,未编译、未验证。** 2026-09-21 起草并落码,基线 `main = 1bca719e5`;验证序列见 §8.6,由用户 / Codex 执行。落码后又经三视角对抗评审一轮(无 blocker,注释与文档口径订正见 §8.9)。
> 本文取代 `04-asset-channel.md` §4.35 的"B4c 玩家存盘属主围栏"方案:那份方案写于 owner_epoch 机制落地之前,照它去造
> `kClaimAndLoadLuaScript` / `<blob>:owner` token / `PlayerSaveOwnerComp` 会得到第二套平行围栏(违反 AGENTS §11.5-3)。
> 效力:README §2/§3 > 本文 > 04 §4.35 / I1 / C14 / 偏差 #22 / 采纳表 #4 的旧写法。
> 依据:`docs/design/scene-owner-reentry-barrier.md` §3.3 / §6(owner_epoch 的设计)、`docs/design/cross-zone-scene-travel.md`
> §12.3(GO-2)、§12.6(Z1)与 **§12.6.9(与本批的分工,双方 2026-09-21 约定)**。
> 过程:4 路只读侦察 + 综合 + 逐条对抗复核(6 条候选缺口,5 条成立、1 条证伪),另与 owner_epoch 的持有会话(跨 zone 传送)逐条核对。

## 结论(先读这段)

**K1 是什么**:玩家从旧 scene 节点换到新节点时,旧节点的退出存盘因 Redis 抖动迟到,覆盖了新节点已经上报 durable 的资产改动。
帮会侧已据 durable 记了资金,玩家侧的扣款却被旧存盘抹掉 —— 一笔捐献变成两笔钱。

**现状**:04 §4.35 设计之后,跨 zone 会话落了一套 **owner_epoch 归属纪元**,思路与 §4.35 相同、实现更完整:

- scene_manager 每次把玩家放到**另一个物理节点**时 `INCR player:{id}:owner_epoch`,INCR 与写 location 在同一段 Lua 里,**先于**路由事件、先于新节点读 blob;
- 节点存盘走 `kSaveIfGuardLuaScript`,Redis 里的 epoch 与自己缓存的不等就整笔拒绝;被拒后 `HandlePlayerSaveRejected` 判定"已被废黜"并销毁实体;
- DBTask 带 `owner_epoch`,db 服务落 MySQL 前比对;
- 资产 durable 只认"存盘成功回调写进 `PlayerLastPersistedSnapshotComp` 的那份快照里,账本记着这个 seq",被拒的写不发成功回调。

所以 **epoch 非 0 时,跨节点版 K1 已经堵住**,B4c 不再造围栏。B4c 只补四处缺口:

1. **epoch = 0 时资产通道不设防**(阻断级)。兼容窗口里存盘走无守卫的旧 `Save`,资产 RPC 却照常记账、上报 durable,K1 原样成立。
   → 改动类资产 RPC 在 epoch = 0 时回 RETRY,不记账、不触发存盘(§8.2.1)。
2. **`redis_client.h` 同 key 新旧颠倒**。一份写失败后在等退避重试时,新值会绕过它直接发出,之后旧值被当成"更新的值"再发一遍,盖掉新值。
   它让 Redis 暂时回退、丢掉未 durable 的最新进度;**单靠它不会复制资产**(§8.2.2 论证),但它破坏了 durable 推理依赖的队列不变量,
   也是 Z1 修复的前置。→ 修队列入口,并提供只读查询 `HasUnsettledSave` 给 Z1 复用(§8.2.2)。
3. **围栏零测试**:守卫 Lua 与 epoch = 0 闸都没有用例(§8.2.3)。
4. **门禁与告警**:04 要求"规则文件随 B4c 交付",至今没有;D8 的门禁只写了"先落 B4c",而同节点的复制路径 Z1 / GO-2 不在 B4c(§8.3)。

**同节点版 K1 还有两条真正会复制资产的路径,不在 B4c**:Z1(僵尸实体被同节点重登复用,旧内存盖掉盘上在别的节点产生的新进度)
与 GO-2(路由失败回滚让 epoch 退回、之后同值重铸,两个持有者同 epoch)。二者归跨 zone 会话(§12.6.9)。
**因此"共享 / 预发环境开启帮会资产操作"的门禁从"B4c 已落"改为 §8.3 的六条,Z1 与 GO-2 修复都在其中。**

---

## 8.1 §4.35 原方案逐项对照(已被 owner_epoch 取代,不实施)

| §4.35 原方案 | owner_epoch 中的对应实现 | 结论 |
|---|---|---|
| `kClaimAndLoadLuaScript`:加载时 SET 属主、同一段 Lua 里 GET blob,认领早于读 | scene_manager 在 EnterScene 里先 `INCR owner_epoch` 并写 location(同一段 Lua,`go/scene_manager/internal/logic/owner_epoch.go`),之后才路由,新节点才 `AsyncLoad` | 取代。认领由铸造方完成,且严格早于读 |
| `kFencedSaveLuaScript`:属主键缺失则补种、不等返回 0、相等则 SET + SADD | `redis_client.h` 的 `kSaveIfGuardLuaScript`,结构逐行相同 | 已存在 |
| 属主键 `<redis_key>:owner`,不设 TTL | `player:{id}:owner_epoch`,只由铸造方 INCR,删 location 时刻意不删 | 取代 |
| 令牌 `<节点UUID>:<毫秒>:<自增>` + `PlayerSaveOwnerComp` | `PlayerOwnerEpochComp`(`player_ownership_comp.h`),路由事件下发,取 max 更新 | 取代(epoch 由 Go 按路由决策发,比节点自铸 token 更强:见 reentry-barrier §3.3"为什么不能让节点自己读") |
| `Element.owner_token` / `SaveFenced` / 收 0 回调 / 丢弃同 key 排队值 / 不重试 | `Element.guard_key / guard_expected`;`OnSaved` 被拒分支丢同期望值的排队值、保留更新期望值的排队值、回调 `save_rejected_callback_` | 已存在 |
| `HandlePlayerSaveFenced` 挂 `PlayerSaveFencedComp` 回 RETRY 并踢线 | `HandlePlayerSaveRejected` → `DestroyDeposedPlayer` 同步销毁;之后资产 RPC 回 NOT_HERE。另处理了"旧代际在途写被拒但本节点仍是属主"(用当前 epoch 重存) | 取代,且更强 |
| DBTask 挪到存盘成功之后 | DBTask 带 `owner_epoch`,db 的 `key_ordered_consumer` 落库前比对 applied_epoch(reentry-barrier §6.3) | 取代(不再搬 DBTask) |
| data_service 回档写 blob 先 SET 属主 `rollback:<毫秒>` | 回档今天写的不是 scene 读的那份 blob,且生产栅栏为 nil、回档整体停用(`07-rollback-fail-closed.md` 结论第二条、U6) | 不在 B4c;归"回档写 PlayerAllData"那个待认领批次,届时必须经 owner_epoch 守卫 |
| `cpp/tests/redis_fence_test`(新工程) | 不新开工程;用例进既有 `currency_test`(§8.2.3) | 改 |
| 告警规则文件随 B4c 交付 | §8.2.4 | 本批交付 |

§4.35 采纳表 #4 驳回的"加载后 N 秒内资产改动回 RETRY"仍然不需要:围栏按代际拦截,不按时间。

---

## 8.2 B4c 的四处修补

### 8.2.1 epoch = 0 时资产改动类 RPC 回 RETRY(阻断级,G1)

**缺口(已核实)**:

- 建实体时挂零值 `PlayerOwnerEpochComp`,只有路由带来非 0 epoch 才覆盖(`player_lifecycle.cpp` 建实体处与 `ctx.ownerEpoch != 0` 分支)。
- epoch = 0 时 `SavePlayerToRedis` 走无守卫的旧 `Save`,只计 `owner_epoch_unknown`(`player_lifecycle.cpp` 的 owner_epoch 段);db 侧 `taskEpoch == 0` 按 `legacy_zero` 直接放行(`go/db/internal/kafka/key_ordered_consumer.go` 的 `checkOwnerEpoch`)。
- `asset_op_system.cpp` 全文没有任何 epoch 检查,改动闸只看 `PlayerFrozenComp / PlayerTravelHandoffComp / UnregisterPlayer`。
- epoch = 0 的来源:旧版 scene_manager;`player:{id}:owner_epoch` 键缺失时的同节点落点(观察值 0 又不铸造,路由就带 0 过去,对应存量 location 或 Redis 丢键);dev 旁路。新流程下首次落点从 0 铸到 1,正常路径都 ≥ 1。

**修法**(只改 `asset_op_system.cpp`,复用既有 `PlayerOwnerEpochComp`,不新增组件、状态或 tip 码):

1. 新增内部函数 `bool HasFencedOwnership(entt::entity player)`:`PlayerOwnerEpochComp` 存在且 `epoch != 0`。组件缺失与 0 一律按"无围栏"处理(fail-closed)。
2. `IsPlayerMutable` 追加这一条。它的**调用点只有 `AnswerSeenSeq` 的补存**(已见 seq 未 durable 时补一次存盘):epoch = 0 时补存的也是一份无守卫的写,同样不该由资产通道引发;已见 seq 的**答复本身**照回原结局与快照推出的 durable。
   `Decide` 第 5 步的改动闸**不**调用 `IsPlayerMutable`,而是逐条内联同样的判定(为了按原因回不同的 reason),所以第 5 步要另加一支(第 3 条)。以后新增可改判定时两处要同改(代码注释已写明)。
   **跳过补存的可观察后果**:`PlayerLastPersistedSnapshotComp` 只在存盘成功回调里写入,新加载的实体上没有它,`IsAssetOpDurable` 直接返回 false。于是 epoch = 0 的新加载实体上,已见 seq 在首次周期存盘之前(默认 ≤ 300s,`SCENE_PLAYER_SAVE_INTERVAL_SECONDS`)一直报 durable = false,Go 按 `ActionAwaitDurable` 每 500ms 重查。结局本已在 Redis 里,**不复制、不误终结**(玩家离线后经 `LedgerReader` 读盘,结论同样正确),只会抬高 `assetop_reschedule_total{reason="await_durable"}` 一类计数 —— 值班看到它先查是不是有 epoch = 0 的实体。
3. `Decide` 第 5 步,在 `UnregisterPlayer` 判定之后、第 6 步(中止占位)之前加一支:`!HasFencedOwnership(player)` → `Answer(RETRY, kAssetFrozen)` 并 `LOG_WARN "[AssetOp] blocked: owner_epoch unknown …"`(带 rpc / player_id / stream / seq)。
   - **放在第 6 步之前**:中止占位记下的 REJECTED 也是一笔账本改动,同样会被晚到的无守卫旧存盘抹掉。
   - **reason 取 `kAssetFrozen`(27003)而不是 `kAssetBlocked`(27005)**:27005 的文案是"该资产当前被禁止获取",指货币封禁,会误导玩家与客服;27003"角色迁移中,稍后自动重试"与"归属未确认、换一次节点就好"对玩家是同一件事。Go 侧不按 reason 分支(`go/shared/assetop/types.go` 只定义常量),客户端只展示文案。区分靠日志关键字,不新增 tip 码(新增要改 `Tip.xlsx`,不值)。
   - 日志是 WARN 不是 ERROR:epoch = 0 在兼容窗口内是预期状态,真正该响的是"它在共享环境出现了",由 §8.2.4 的查询按关键字计数。
4. 代价(已与跨 zone 会话确认,§12.6.9):卡在 0 的存量玩家(存量 location + 一直落回同节点)在换一次节点之前,资产操作会一直 RETRY。
   根治是 scene_manager 在观察值为 0 时即使同节点也铸造(`mint = !samePhysicalNode || observedEpoch == 0`),跨 zone 会话的一行改动,排在首次编译之后;
   B4c 不碰 `go/scene_manager`。dev 按 AGENTS §6.2.6 压测前清 Redis,首次落点铸 1,不受影响。

### 8.2.2 `redis_client.h` 同 key 新旧颠倒 + 只读查询 `HasUnsettledSave`

**缺口(已核实)**:`MessageAsyncClient` 对每个 key 维护"在途"(`saving_queue_`)与"排队"(`pending_save_queue_`)各至多一份。
成功路径、ERROR 重试路径、重连回收路径都把"排队的那份"当成**更新的值**处理(成功时发它并压住本次回调;失败时发它并丢掉失败的那份;重连时它在就丢掉在途那份)。
这个判断成立的前提是一条隐含的不变量 —— **排队的永远比在途的新**。今天唯一破坏它的入口是 `EnqueueSave`:

```
t0  O 发出 → ERROR → QueueSaveForRetry(O):没有更新的排队值 → O 进 pending,等退避
t1  新快照 N 到达 EnqueueSave:saving_queue_ 里没有这个 key(O 已摘出),已连接 → 直接 IssueSave(N)
    此刻 pending 里是更旧的 O,在途是更新的 N —— 不变量被破坏
t2  N 落地 → OnSaved 成功分支:发现 pending 有值 → 当成"更新的值"发出 O,压住 N 的回调
t3  O 落地 → Redis 里是 O(比 N 旧),回调 O
```
ERROR 路径同理(N 失败 → 发 O、丢 N),重连路径同理(N 在途时断线 → pending 有 O → 丢 N)。

**对资产的后果(本批的判断,评审请证伪)**:durable 的唯一依据是成功回调写进 `PlayerLastPersistedSnapshotComp` 的快照(`asset_op_system.cpp` 的 `IsAssetOpDurable`)。
成功回调只在 pending 为空时发布(`OnSaved` 成功分支),而 pending 为空意味着此刻不存在任何比本次更旧的值。所以一份快照一旦被回调、成为 durable 的依据,
之后不会再有比它更旧的值从队列里落地。颠倒造成的是:Redis 暂时回退、未 durable 的最新进度丢失(实体活着时下一次脏比较会补存;实体若在退出链上被旧值的回调销毁,那段进度永久丢失 —— 这正是 Z1 修复 M1 要防的)。**资产的未 durable 改动不会被 Go 终结,重投时在回退后的数据上重新应用,一次且仅一次。**
所以它不是复制路径,但仍要修:① 它是真实的数据回退缺陷;② durable 推理依赖"回调的快照 == Redis 当前值 == 最新"这条不变量;③ Z1 修复要靠"无在途 / 排队"判断能否销毁实体(§12.6.4 M1 / M4)。

**修法**(只改 `EnqueueSave`,其余路径在不变量成立时原样正确):

```cpp
// 有在途 → 新值进 pending(既有逻辑,排队的比在途新)
if (saving_queue_.find(element->redis_key) != saving_queue_.end()) { pending_save_queue_[...] = element; return; }

// 【新增】没有在途,但 pending 里有一份上次写失败后在等退避的旧值:新值顶替它,继承它的
// retry_count / next_retry_at / save_failure_notified,由定时器按原退避节奏发出,不在这里直接发。
// 直接发会让 pending 里留着更旧的值,之后成功 / 失败 / 重连三条路径都会把它当"更新的值"发出去,
// 盖掉刚落地的新值(同 key 新旧颠倒)。
if (auto pending = pending_save_queue_.find(element->redis_key); pending != pending_save_queue_.end()) { …顶替… return; }

// 未连接 → 进 pending(既有逻辑)
// 否则 IssueSave(既有逻辑)
```

- **继承退避而不是立即发**:那份旧值失败,说明 Redis 此刻可能仍不可写;新值立即发只会在同一个故障上再失败一次、并重置退避。继承 `save_failure_notified` 是因为 `save_failed_callback_` 只打一条 ERROR 并保留退出中的实体(`HandlePlayerAsyncSaveFailed`),同一个 key 的同一段故障不必再报一次。
- **不改的路径**:`IssueSave` 的防御分支、`QueueSaveForRetry`、`OnSaved`(成功 / ERROR / NOSCRIPT / 被拒)、`OnReconnected`、`FlushDuePending`。逐一核过写入排队的全部路径,分两类:**有在途时写入的新值**(`EnqueueSave` 首支、`IssueSave` 的在途防御分支放回)—— 它就是最新快照;**无在途时写入的值**(`EnqueueSave` 顶替分支 / 未连接分支、`IssueSave` 的未连接分支、`QueueSaveForRetry` 与 `OnReconnected` 各自在当时无排队值时)—— 写入那一刻没有在途,不变量天然成立。
- 更新 `EnqueueSave` 上方那段注释,把不变量写成一句明文,后来者改队列时有据可查。

**只读查询**(给 §12.6 Z1 修复与 M4 复用,接口已发跨 zone 会话过目):

```cpp
// 该 key 是否还有未落地的存盘:在途(已发出、等回包),或排队(等在途完成 / 等退避重试 / 等重连)。只读,不改队列。
bool HasUnsettledSave(const MessageKey& key) const;
```

注释里写明两条使用约束(跨 zone 会话 2026-09-21 复核时补充):

- **在 `save_callback_` 里调用它恒为 false**:`OnSaved` 成功路径先摘在途,有排队值就直接发新值且不回调,只有排队为空才回调。它不能在回调里当"还有没有更新值"的信号用;用处在回调**之外** —— 快路径退出(§12.6.4 M4 / Z2:内存等于上次落地快照,但还有一笔内容不同的存盘在途或排队,此时销毁实体,那笔落地会把盘改回去)。
- **只看存盘队列**(`saving_queue_` / `pending_save_queue_`),不看 `pending_retry_queue_`(那是载入的)。

本批**不调用**它(B4c 不改 `player_lifecycle.cpp` 的存盘回调,§12.6.9 约定);Z1 落码时由跨 zone 会话接上。它有本批的单测覆盖,不是无人调用的死代码。

### 8.2.3 测试(面向接口;不依赖墙钟)

全部放进既有测试工程 `cpp/tests/currency_test`,**不新开工程**(新工程要改 vcxproj / sln / `run_cpp_tests.ps1` 三处;`currency_test` 已链接 hiredis / muduo / scene,已在 `run_cpp_tests.ps1` 的运行清单里)。

**`asset_op_system_test.cpp`(epoch 闸)**

- 夹具 `SetUp` 给测试玩家 `emplace<PlayerOwnerEpochComp>` 且 `epoch = 1` —— 否则加闸后既有记账用例全部变成 RETRY。文件里另有自行 `create()` 的实体,逐个补。
- E1 epoch = 0:Debit / Credit / AbortDebit 各一次,均回 RETRY + `kAssetFrozen`,账本无该 seq,持久化钩子未被调用,货币不变。
- E2 组件缺失:同 E1(fail-closed)。
- E3 已见 seq × epoch = 0:先在 epoch = 1 下记一笔 APPLIED(未 durable),再把 epoch 置 0 重查:回原结局 APPLIED、durable 与快照一致、**不补存**(持久化钩子调用次数不增)。
- E4 epoch 从 0 变为非 0 后同一 seq 重投:照常应用(闸只看当前 epoch,不留粘性状态)。

**`currency_test.cpp`(队列与守卫脚本;沿用文件里既有的 `MessageAsyncClientTestPeer`,只加 helper)**

- Q1 纯内存(三条时序,跨 zone 会话复核时点名;"发出"用 Peer 把排队值挪进在途来模拟,不连 Redis):
  - Q1a 继承:S1 在途 → 回 ERROR → 进 pending 退避;`Save(M)`(无在途)→ pending 恰好一份且是 M,M 继承了 S1 的 `retry_count` / `next_retry_at` / `save_failure_notified`,在途为 0。修复前这条红在三个继承字段上(未连接分支直接覆盖 pending,字段被重置)。**它测不到"已连接时绕过排队直接发出"那条颠倒本身**(未连接时修复前 M 也只会进排队),颠倒由 Q3 覆盖。
  - Q1b 到期发的是 M:接 Q1a,把 pending 发出、回成功 → 成功回调恰好一次、带的是 M(按载荷区分),之后在途与排队都为 0 —— S1 再也不会被发出。
  - Q1c M 再失败仍是 M:接 Q1a,把 M 发出、回 ERROR → pending 是 M 不是 S1;再发出、回成功 → 回调带 M。
- Q2 `HasUnsettledSave`:无 / 仅在途 / 仅排队 / 两者都有 四态;查询前后队列计数不变。
- Q3 **真 Redis(颠倒的回归用例)**:读 `MMORPG_TEST_REDIS_ADDR`(`host:port`),未设 `GTEST_SKIP`。连上后用 Peer 造一份在途值再回 ERROR,使其经 `QueueSaveForRetry` 进 pending(退避 500ms;用例不跑事件循环,到期与否无关),无在途;`Save(N)` 之后
  **在途计数必须为 0、pending 是 N**。修复前这条红:N 被直接发出(在途 1、pending 仍是旧值)。只需要"已连接"这一状态,不必等任何回包,结果确定。
- Q4 **真 Redis(守卫脚本语义,G6)**:同一开关。三种情形各跑一次完整存盘并等回调(事件循环带 3s 超时):guard 键等于期望值 → 成功回调、blob 为新字节;
  guard 键不等 → 被拒回调、blob 不变;guard 键缺失 → 成功回调、guard 键被补种为期望值。测试键用固定的高位 player_id,前后清理。
- Q5 纯内存结构断言:`kSaveIfGuardLuaScript` 先 `GET KEYS[2]` 后 `SET KEYS[1]`,含 `cur == false` 补种分支与 `cur ~= ARGV[2]` 的 `return 0`(照 `cross_zone_test.cpp` 的 `LuaIsConditionalDelete` 写法)。它不代替 Q4,只保证没有 Redis 的环境也能抓住脚本被改坏。

Q3 / Q4 需要一个本地 Redis。`MMORPG_TEST_REDIS_ADDR` 是新开关(04 §4.35 当初拟的名字),不设就跳过,不影响既有 CI。

### 8.2.4 告警与运维

**Go 指标 → 新文件 `deploy/k8s/owner-epoch-alerts.yaml`**(PrometheusRule,形状照 `scene-manager-alerts.yaml`):

| 告警 | 表达式 | 级别 | 含义 |
|---|---|---|---|
| `DbStaleOwnerWriteRejected` | `sum(increase(db_stale_owner_write_rejected_total[10m])) > 0` | warning | 有旧持有者的 DBTask 被 db 拒。可能是僵尸 / 双主,也可能是 K1 被正确拦下(旧节点退出存盘迟到);两种都要看 |
| `DbOwnerEpochLegacyZero` | `sum(db_owner_epoch_guard_total{outcome="legacy_zero"}) > 0`(绝对值,见下) | warning | 有 epoch = 0 的写进了 MySQL,兼容窗口仍在;**共享环境开启帮会资产操作的门禁要求它恒为 0**(§8.3) |

指标名以 `go/db/internal/metrics/metrics.go` 为准(侦察核对:`db_stale_owner_write_rejected_total` 无 label;`db_owner_epoch_guard_total{outcome}`)。
落码时核对(`go/db/internal/metrics/metrics.go`):`db_stale_owner_write_rejected_total` 是无 label 的 Counter,注册时即以 0 值导出,`increase` 能抓到第一次;
`db_owner_epoch_guard_total` 是 CounterVec,`legacy_zero` 序列要等第一次发生才出现,`increase` 对"从无到 1"只有一个样本、恰好漏掉第一次(92-handoff §8.1 踩过),
所以这条用**绝对值**判定 —— 代价是进程重启前一直处于告警态,对"应当恒为 0"的门禁不变量正合适。**不**在本批改 go/db 去预置。

**scene 侧只能靠日志**(scene 的计数不进 Prometheus,`dirty_save_stats` 以日志为指标源)。仓库里 Loki 虽配了 `ruler`,但规则目录没有挂载、也没有 `alertmanager_url`,
**目前不存在能生效的日志告警通路**。本批不造一份"配了也不生效"的规则文件,而是把以下 LogQL 写进 runbook,作为值班的固定查询;ruler 接线列入 BK8s(90 清单 G-05):

- `sum(count_over_time({service="scene"} |= "[AssetOp] blocked: owner_epoch unknown" [10m])) > 0` —— 资产操作因 epoch = 0 被挡;
- `sum(count_over_time({service="scene"} |= "[OwnerEpoch]" |~ "owner_epoch_unknown=[1-9]" [10m])) > 0` —— 有 epoch = 0 的存盘(字段格式落码时按 `owner_epoch_stats` 的实际输出核对);
- `sum(count_over_time({service="scene"} |= "save outran reconnect lease" [10m])) > 0` —— 退出存盘超过重连租约才落地(epoch 非 0 时它不再意味着覆盖,保留作辅助信号)。
  - 耦合:这条日志在 `player_lifecycle.cpp` 的 `HandlePlayerAsyncSaved` 退出分支里,正是 Z1 修复要重写的一段。跨 zone 会话 2026-09-21 在消息里确认:落 Z1 时保留日志原文,非改不可时同批改 runbook §9 并知会本线;已登记进 `cross-zone-scene-travel.md` §12.6.9(对方提交 `6ef104664`)。

**runbook**(`docs/ops/cross-zone-failure-test-runbook.md`,跨 zone 会话的文档:不改它的表格行与场景判定,只在末尾追加 §9):
**第二种预期来源** —— 旧节点退出存盘超过租约后被 guard 拒(K1 被正确拦下)。区分:同一 player_id 的 `HandleExitGameNode: Player <P> is exiting the scene node` 早于被拒那一行 `HandlePlayerSaveRejected: owner_epoch CAS rejected save for player <P>`(退出链上没有含 `UnregisterPlayer` 字样的日志)。

---

## 8.3 门禁:"共享 / 预发环境开启帮会资产操作"之前必须同时满足

取代 90 清单 D8 与 91 批次表的"B4c 已落地并验证"这一条。任何一条不满足,资产通道在该环境就不能打开(`AssetOp.Enabled=false`):

1. **B4c 已落地并验证**(本文 §8.6 的序列全过)。
2. **Z1 修复已落地并验证**(`cross-zone-scene-travel.md` §12.6)—— 否则僵尸被同节点重登复用,旧内存盖掉盘上在别的节点产生的新进度,帮会已记账的捐献在玩家侧被抹掉。
3. **GO-2 修复已落地并验证**(同上 §12.3)—— 否则路由失败回滚让 epoch 退回、同值重铸,两个持有者同 epoch,守卫分不开。
4. **`AllowUnsafeCrossNodeHandoff = false`**(`go/scene_manager/etc/scene_manager_service.yaml`;代码默认值已是 false,本地 dev yaml 为 true)。
   旁路下无标记的同 zone 跨节点换手不铸 epoch,新旧实体同持 E。资产通道的 robot 验收与压测也必须在 false 下跑,true 下的全绿不作资产正确性证据。
5. **`owner_epoch_unknown` 与 `db_owner_epoch_guard_total{outcome="legacy_zero"}` 在该环境恒为 0**(§8.2.4)。
6. **玩家 blob 与 `player:{id}:owner_epoch` 在同一个 Redis 实例、非 Cluster 模式**:守卫 Lua 用两个 KEYS,没有 hash tag,Cluster 下会 CROSSSLOT;
   分在两个实例则守卫读的是本实例里自己补种的值。代码里没有断言,由部署核对(k8s 下 scene 的玩家 Redis 与 scene_manager 的 `Redis.Host` 是否同一实例,落码时未能确认)。

第 2、3 条不归帮会线,但它们与 K1 是同一类伤害(帮会已据 durable 记账,玩家侧被旧数据盖回),不写进门禁,D8 就只防住了一半。

## 8.4 残余风险(接受并记录)

- **守卫键缺失时补种**(`kSaveIfGuardLuaScript` 注释):Redis 清空的同时恰有僵尸抢先存盘,它会补种旧值、合法持有者被拒。跨 zone 会话已论证接受;B4c 不动。
- **DBTask 在 Redis 结果之前发出**:被 guard 拒的 blob 仍可能经 DBTask 进 MySQL(当新节点尚无更高 epoch 的 DBTask 时,db 守卫按 match / first 放行)。
  对资产无害的前提是"已落盘账本"只读 Redis、不回退读 MySQL —— 这条契约已在 `go/shared/assetop/reconcile.go` 的 `LedgerReader` 注释与 `07-rollback-fail-closed.md` §7.8.2 写明,B5d 落码时守住。
  MySQL 里那份在 Redis 丢数据(C5)后被冷加载复活,属通用回档,资产侧一致(扣款与账本同在一份数据里)。
- **颠倒期间 Redis 与 MySQL 分叉**(跨 zone 会话补充):`SavePlayerToRedis` 发 Redis 存盘的同时**无条件**发 DBTask,不等 Redis 结果。修复前的颠倒窗口里,MySQL 可能已是新值、Redis 却被旧值盖回,直到下一次存盘。login 预加载在 Redis 键存在时以 Redis 为准,正常流程不受影响;只有 Redis 键丢失或被驱逐、改从 MySQL 读的那一刻,会读到一份"扣了款、但 durable 从未被确认"的状态 —— 后果是丢钱,不是复制(Go 会在该状态上重投,扣款已在,见 Unseen 判定)。B4c 修掉颠倒后此窗口随之关闭,残余只剩"DBTask 先于 Redis 结果落库"这一既有设计,不在本批。
- **login 预加载 EXISTS → SET 非原子**(评审发现,既有代码):`go/login/internal/logic/pkg/dataloader/ensure_player_all_data_async.go` 先 `Exists` 判缺键,等 DB 读回后经 `sync_loader.go` 的 `rc.Set` 无条件写回(不带 NX、不经 owner_epoch 守卫)。
  触发条件:玩家 blob 键丢失(Redis 丢数据 / 驱逐 / 清库)时玩家仍在节点 A 在线 —— A 的带守卫存盘在守卫键缺失时补种并写入,资产 seq 随之 durable、Go 终结;同一玩家此刻重登,login 的 SET 落在 A 的写之后,用 MySQL / 子表缓存里更旧的数据盖掉它,之后铸出新 epoch 的新节点加载的就是这份旧数据:**帮会已记账、玩家侧扣款与账本一起被抹掉,Go 不会重投**。
  前提是 Redis 丢键,概率低,不列入 §8.3 门禁;最小修法是 `sync_loader.go` 的写回改为 `SetNX`(返回 false 视为成功,数据已在),**归 login 批次**,B4c 不改 go/login(AGENTS §10.2 不扩大范围)。
- **TCP 半开**:旧连接上已发出、服务端尚未读到的写,在客户端判断断线并重发新值之后才被执行 —— 同 epoch 下守卫分不开。所有 Redis 客户端的共性,本批不处理。

## 8.5 手改文件

| # | 文件 | 改什么 |
|---|---|---|
| 1 | `cpp/libs/services/scene/player/system/asset_op_system.cpp` | `HasFencedOwnership`;`IsPlayerMutable` 追加;`Decide` 第 5 步加一支(§8.2.1) |
| 2 | `cpp/libs/engine/infra/storage/redis_client/redis_client.h` | `EnqueueSave` 顶替分支 + 不变量注释;`HasUnsettledSave`(§8.2.2) |
| 3 | `cpp/tests/currency_test/asset_op_system_test.cpp` | 夹具补 epoch;E1–E4 |
| 4 | `cpp/tests/currency_test/currency_test.cpp` | Peer helper;Q1–Q5 |
| 5 | `deploy/k8s/owner-epoch-alerts.yaml`(新) | 两条 PrometheusRule |

文档(不计名额):本文;`04-asset-channel.md`(:17 / I1 / C14 / §4.35 / 偏差 #22 / 采纳表 #4 / 需跟进事项第 4 条标为被本文取代);`90-consistency.md` D8、G-05、G-07;`91` 门禁行与授权点;
`07-rollback-fail-closed.md`(T3 与 U6 里的 `rollback:<毫秒>` 属主改为经 owner_epoch 守卫);`docs/ops/cross-zone-failure-test-runbook.md`(末尾 §9 与两处引用订正);`92-handoff.md`、`PROGRESS.md`。

**5 个手改文件,远低于 91 的"≤12"**:§4.35 预估里的新 Lua、新组件、新回调、搬 DBTask、新测试工程都已由 owner_epoch 覆盖或改为复用。

**不改**:`player_lifecycle.{h,cpp}`(存盘回调原则上只读,§12.6.9)、`go/scene_manager`、`go/db`、`cross_zone_test.cpp`、`Tip.xlsx`、任何 proto。

## 8.6 验证序列(给用户;Claude 不执行,未编译)

```text
【前置】MSBuild 一律串行 /m:1 /nr:false;开跑前确认没有别的会话在导表 / proto-gen / MSBuild(90 G-08)。
        cpp/generated/table 的工程登记依赖导表产物(B5a 那 12 个文件、rolenamerule_*),C++ 编译必须在 dev.bat export 成功之后。
        scene_node_service.cpp 的 Agones 块若仍缺失(旧生成器吞掉的),先恢复,否则全节点编译会带着那份缺口。

1. 编译 currency_test(连带 scene 库):
     msbuild cpp\tests\currency_test\currency_test.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64
   通过标准:0 error。
2. 跑不需要 Redis 的用例:
     <输出目录>\currency_test.exe --gtest_filter=AssetOp*:CurrencyTest.*
   通过标准:全绿;Q3 / Q4 显示 SKIPPED。
3. 起一个本地 Redis(docker 的 dev 实例即可,**不要**指向共享 / 预发实例),再跑一遍:
     $env:MMORPG_TEST_REDIS_ADDR = "127.0.0.1:6379"
     <输出目录>\currency_test.exe --gtest_filter=CurrencyTest.*
   通过标准:Q3 / Q4 实际运行且全绿。
   回归证据(建议做一次):临时注释掉 EnqueueSave 新增的顶替分支再跑 Q3,应当红(在途 1、pending 仍是旧值);恢复后绿。结果贴回。
4. tools/scripts/run_cpp_tests.ps1 全量跑一次,既有用例不回归(尤其 AssetOp* 与 CurrencyTest.RedisGuarded*)。
5. 告警文件语法:取出 deploy/k8s/owner-epoch-alerts.yaml 的 spec.groups 跑 promtool check rules(与 scene-manager-alerts.yaml 同法)。
6. 联调(可选,需整套服务):AllowUnsafeCrossNodeHandoff=false,清 Redis;robot 帮会捐献冒烟(B5c 之后)全程日志无
   "[AssetOp] blocked: owner_epoch unknown";把某玩家的 player:{id}:owner_epoch 键删掉、让其同节点重登一次,确认该玩家的资产操作回 RETRY
   且日志出现该关键字,换一次节点后恢复。
失败时保留:第一个编译错误上下文;失败用例完整输出;Q3 / Q4 失败时加跑 redis-cli MONITOR 的片段。
```

## 8.7 与跨 zone 会话的分工

以 `cross-zone-scene-travel.md` §12.6.9 为准:B4c 修 `redis_client.h` 的颠倒并提供 `HasUnsettledSave`,改 `asset_op_system.cpp`、告警与用例;
Z1(§12.6)、GO-2(§12.3)与 `go/scene_manager` 的"观察值为 0 也铸造"归跨 zone 会话。`HandlePlayerAsyncSaved` / `HandlePlayerSaveRejected` 本批只读;
若评审证明必须改,改哪段先与该会话对齐。

## 8.8 与既有正文的偏差

| # | 既有写法 | 本文 | 原因 |
|---|---|---|---|
| 1 | 04 §4.35 整套 token 围栏 | 不实施,改为补 owner_epoch 的缺口 | owner_epoch 已覆盖(§8.1);再造一套违反 AGENTS §11.5-3 |
| 2 | 04 I1"单写者只在重连租约内成立" | epoch 非 0 时由 owner_epoch 保证;例外见 §8.3 第 2–6 条 | 现状已变 |
| 3 | 90 D8 / 91 门禁"先落 B4c" | §8.3 六条 | Z1 / GO-2 是同类复制路径,不在 B4c |
| 4 | 04 §4.35"`redis_fence_test` 新工程" | 用例进 `currency_test` | 少改三处工程文件,且 Peer 现成 |
| 5 | 04 §4.35"规则文件随 B4c 交付" | Go 指标出 PrometheusRule;scene 日志规则写进 runbook,ruler 接线归 BK8s | 仓库里没有能生效的 Loki 告警通路,不造死配置 |

## 8.9 落码后对抗评审处理记录(2026-09-21)

三个视角(C++ 静态正确性 / 队列不变量与资产语义 / 文档与告警一致性)各自评审,每个视角的发现再由独立复核员尝试证伪。
**无 blocker**;代码行为全部核对成立(三个视角合计逐项核过 49 处)。成立的 16 条都是注释、文档口径或既有代码的同类残余,2 条被证伪。

| 视角 | 级别 | 问题 | 处理 |
|---|---|---|---|
| 静态 / 语义(三人同报) | minor | `redis_client.h` 不变量注释"入口只有三处"枚举不全(漏了未连接分支、顶替分支、`IssueSave` 两个防御分支;这些写入时都无在途,不变量照样成立) | 注释与 §8.2.2 改为按"有在途 / 无在途"两类枚举;顶替分支注释补"或断线时停放" |
| 静态 | minor | Q1a 注释称验"新值不被直接发出",但无连接时修复前也不会直接发出 | 注释写明它只验继承字段,颠倒本身由 Q3 覆盖;§8.2.3 同步 |
| 静态 | minor | 泄漏守卫的注释描述的是 third_party/muduo 原版 Hiredis,本工程链接的 muduo_windows 版已修 | 注释改为"不依赖收尾顺序"并注明两版差异 |
| 语义 | minor | 跳过补存让 epoch = 0 的新加载实体上已见 seq 长时间 durable = false | 不改代码(方向安全);§8.2.1 第 2 条写明可观察后果 |
| 语义 | minor | login 预加载 EXISTS → SET 非原子,Redis 丢键时可盖掉已 durable 的 blob | §8.4 记为残余,修法 `SetNX` 归 login 批次 |
| 文档 | major | 08 状态行仍写"未落码" | 已改 |
| 文档 | major | 08 称已订正的 04 采纳表 #4 / 92 / PROGRESS 实际未改 | 04 三处补标注;92 与 PROGRESS 随本批更新 |
| 文档 | major | 08 称 ruler 接线已列入 G-05,实际没有 | 90 G-05 / G-07 补上 |
| 文档 | major | 07 仍把作废的 `rollback:` 属主写成回档必要条件,行号已漂移 | 07 两处改为经 owner_epoch 守卫,指向本文 §8.1 |
| 文档 | major | §8.2.1 称 `IsPlayerMutable` 有两个调用点,实际只有补存一处 | 已改,并写明两处判定要同改 |
| 文档 | major | 引用的"跨 zone 会话约定"对方文档里没有 | 对方已登记进 §12.6.9(`6ef104664`),08 改为引用该节 |
| 文档 | minor | runbook §9 的退出链日志关键字与代码不符;章节引用与"全文唯一"一句不准 | 三处订正 |
| 文档 | minor | 告警 annotation 对 `$value` 的描述不准 | 已改 |
| 文档 | minor | 08 对 Q3 构造方式的描述与用例不符 | 已改 |
| 文档 | minor | `HasFencedOwnership` 插在 `IsPlayerMutable` 说明与定义之间 | 已挪到前面 |
| 文档 | major(**证伪**) | README D8 未订正 | README 把 D8 细节交给 90,90 已订正;新门禁更严但仍包含 B4c,旧句不冲突 |
| 文档 | minor(**证伪**) | `[OwnerEpoch]` LogQL 注释写成"过去 10 分钟" | 原注释没有这样写;累计计数一直为真到重启,正是门禁口径 |


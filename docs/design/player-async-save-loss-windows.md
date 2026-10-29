# 玩家异步存盘：已修复问题、剩余丢失窗口与修复任务

- 创建：2026-08-03；现状核对：2026-10-23。
- 范围：C++ scene 的 `PlayerLifecycleSystem` / `MessageAsyncClient`，以及 Go login 父快照恢复、go/db SQL 围栏与补投链。
- 证据：按本轮工作区源码静态核对，包含尚未提交的并行修改；本文的“已修复”表示已有实现，**不表示本轮完成了构建、测试或故障验收**。交接资格、登录初始行与恢复工具正在并行修改，实施时按下列函数重新核对。
- 当前结论：旧的重试丢弃、欠款漏存、损坏载荷误加载与无交接屏障已有修复；**Redis 提交前丢失 scene 内存、Redis 提交后父快照与同实例 outbox 一起回退，仍有真实丢失窗口**。交 Claude Code 的修复任务见 §7，验收见 §8。
- 上线状态：按[存盘围栏设计](db-write-fencing-mysql.md)文末“上线状态”，项目尚未上线；本文不新增混版迁移要求。
- **2026-10-24 方向变更（先读）**：用户 10-24 明确「玩家数据不要 redis 存储，其他改用的地方还是要用」，见 [TiDB 决策 §9](global-data-layer-tidb-decision.md)。本文的窗口 B（父快照与同实例 outbox 随 Redis 一起回退）以 Redis 父快照为对象，随新存盘路径重写；§7 的待实现修复开工前先向用户确认是否还做。窗口 A（存储不可写时数据只在 scene 内存）与存储选型无关，新设计仍要回答。只定了方向，未设计、未落码。

关联：[存盘围栏与提交屏障](db-write-fencing-mysql.md)、[跨 zone 传送 §15](cross-zone-scene-travel.md#15-跨-zone-完整节点与数据流程2026-10-20源码核对)、[Redis 回退防护](zone-redis-regression-guard-20261022.md)、[组件访问规则](ecs-component-access-rules.md)、[proto 比较存盘](proto-compare-dirty-save.md)。

## 1. 当前存盘链与提交边界

目标职责：scene 将玩家当前完整状态按原 owner / save_seq 提交给存储链；退出、交接与加载各自使用明确的完成证据，旧写者不能覆盖新状态。

```text
业务修改 ECS 内存
  → 周期保存 / 退出 / 交接调用 SavePlayerToRedisImpl
  → Marshal 完整 PlayerAllData；缺持久组件则拒存
  → 比较上次成功快照；允许跳过且没有 DBTask 欠账时才跳过
  → 生成 save_seq，先序列化两条完整 DBTask
  → 非零 owner_epoch：SaveWithCommitBarrier
       同段 Lua 检查 owner、版本和 schema，并写入：
       父快照 + pending + 两条原始 DBTask 意图 + 补投索引
  → 收到有效 Redis 提交回包后，逐份执行 publishDbTasks 钩子，向 Kafka 投递
  → 有更新值待写时继续写最新值；只有最新成功值发布生命周期回调
  → HandlePlayerAsyncSaved 更新快照，并按当前退出 / 交接规则收尾

并行恢复：go/db saveoutbox 补投原始任务
  → 两张 SQL 行分别按版本围栏提交并发布 ACK
  → 两表 ACK 都等于 pending，才允许回收对应 outbox 意图
```

源码入口：[player_lifecycle.cpp](../../cpp/libs/services/scene/player/system/player_lifecycle.cpp) 的 `SavePlayerToRedisImpl` / `HandlePlayerAsyncSaved`；[redis_client.h](../../cpp/libs/engine/infra/storage/redis_client/redis_client.h) 的 `SaveWithCommitBarrier`、严格保存 Lua、`OnSaved`。

| 证据 | 当前证明什么 | 不能据此推断什么 |
|---|---|---|
| 保存已进入内存队列 | scene 持有待写载荷 | 进程死亡后可恢复 |
| 严格 Lua 执行成功 | Redis 已接受父快照、pending 和 outbox；可以投递原始 DBTask | 已 fsync、已有可靠副本、SQL 已提交 |
| Kafka broker 确认 | 任务已被相应 broker 确认接收，保证受实际 ACK / 副本配置约束 | 两张 SQL 表已提交 |
| 两表 SQL ACK 均等于 pending | 当前版本的两张 SQL 行均已提交，并已发布对应证据 | 任意尚未发起保存的 ECS 改动也已提交 |

当前严格 C++ 保存只等待 EVAL / EVALSHA 的执行回包，没有 WAIT / WAITAOF 持久确认。`owner_epoch == 0` 的兼容路径仍走普通 `Save`，不具备上述严格 outbox 证明；默认 go/db 配置是 `Fencing.Mode: dual`，不能用兼容路径验收严格保证。

## 2. 数据位置与已有恢复能力

下表沿用当前键形；父键由 protobuf full_name 加玩家号构造。父键通常为 `PlayerAllData:<player_id>`，尖括号表示占位符，不是键中的字符；屏障键中的花括号是实际字符。

| 数据 | 当前位置 | 职责 |
|---|---|---|
| 完整父快照 | `PlayerAllData:<player_id>` | scene 异步加载；login 在准入前检查、必要时重建 |
| 子表缓存 | `player_database:<player_id>`、`player_database_1:<player_id>` | go/db 的分表缓存，不能代替 SQL 权威行完成核验 |
| owner | `player:<player_id>:owner_epoch` | 父写的归属 CAS |
| 屏障与 outbox | `player:{<player_id>}:save_barrier` | pending、双表 ACK、原始两条任务及版本 / schema 等元数据 |
| 补投索引 | `player:save_pending:<home_zone>:<player_id%64>` | go/db 找到未完成的存盘意图 |
| SQL 权威数据 | 实际落点库的两张玩家表 | 行内围栏提交、冷恢复来源 |

**父键与子表键仍是不同命名空间，但“父键不会被任何人修复”已经过期。** login 的 [EnsurePlayerAllDataInRedisAsync](../../go/login/internal/logic/pkg/dataloader/ensure_player_all_data_async.go) 和 [parentLoadSnapshot.settle](../../go/login/internal/logic/pkg/dataloader/parent_commit_barrier.go) 已实现：

- 父键缺失，或实例身份变化、ACK 超前等需要核验时，强读两张真实 SQL 行；不采信可能落后的子表缓存。
- 身份、数据和围栏完整且版本关系允许时，原子重建 / 采纳完整父快照；提交时重新检查 owner、pending、双 ACK、outbox 版本，防止读后被并发新写覆盖。
- pending 领先 SQL 时拒登等待；两张 SQL 行版本不同则等它们追平；SQL 低于已发布 ACK 时拒绝并转人工处置。
- 任一 SQL 回包缺数据、串号或解析失败都拒绝。解码骨架、空表、旧零序号种子不能作为历史玩家“新号重建”的证据。

[saveoutbox](../../go/db/internal/saveoutbox/service.go) 已能在 scene 消失后补投。它与父快照依赖同一 Redis：**有补投能力不等于具有独立故障域**。补投达到上限或遇确定性终态时保留意图、复查并告警；这可能造成长期拒登，不代表数据已经恢复。发布失败 / 部分发布失败不消耗成功补投次数，详见[围栏设计](db-write-fencing-mysql.md)的补投附注。

玩家域 Cluster / 键名与索引迁移按[玩家态 Redis 集群化设计](redis-player-store-cluster.md)分批推进，不能把设计目标当成本表已落地状态。修复时须与该线对齐原子范围及按玩家保序契约。

## 3. 已有保护与必须保持的不变量

- **失败不更新成功快照。** `HandlePlayerAsyncSaveFailed` 保留旧的 `PlayerLastPersistedSnapshotComp`；只有最新成功值才发布全局成功回调。快照只证明对应 Redis 写成功，不证明 fsync 或 SQL 提交。
- **同 key 一笔在途，后续合并最新完整值。** 失败重试让位于更新值；重连回收在途写也不覆盖更新待写值。旧完成不能提前结束新快照的生命周期。
- **每份获得有效 Redis 提交回包的载荷，先执行自己的 SQL 投递钩子。** 随后才处理后继值与生命周期；被合并掉、没有提交的载荷不投 SQL，有更新值排队时旧成功不发布全局退出 / 交接回调。Lua 已执行但回包丢失或 scene 崩溃时，由 outbox 接续，不能假设钩子已经执行。
- **跳过、排队、拒存是不同结果。** 生命周期内部使用 `SaveResult` 区分，不能把外部 bool 的 `false` 一律解释为“已经保存”。缺组件拒存不会生成交接或释放凭证。
- **SQL 投递欠账会阻止快路径跳过。** `NeedsResave` 让下一次保存重新 marshal 当前内存，而非重放旧快照。
- **首次保存无成功快照，必须实写。** 实体销毁后快照丢失是保守设计，不应通过猜测缓存一致性来跳过首存。

## 4. 已修复问题与当前状态

本节保留原 §4.1–§4.5 编号，供既有源码及设计引用；旧时序只用于解释故障原因，现状以本节为准。

### 4.1 已修复：6 次重试后静默丢弃

旧实现到达 `kMaxSaveRetries` 就丢弃载荷，既没有失败通知，也没有退出收尾。当前 `QueueSaveForRetry` 达到 6 次只触发一次 `SetSaveFailedCallback`，继续保留最新载荷，以 500ms 起步、16 秒封顶退避重试。

[RedisSystem::Initialize](../../cpp/libs/services/scene/core/system/redis.cpp) 已接线成功 / 失败回调；`HandlePlayerAsyncSaveFailed` 记录风险，不更新成功快照。退出中的实体保留 `UnregisterPlayer` 和唯一内存态，等待保存成功。它解决了“客户端自己丢弃待写值”，**没有使内存队列变成耐久队列**；进程死亡或停机到期的边界见 §6.1。

[location 释放绑定](location-release-binding.md)也已收窄断线租约到期时无条件删 location 的路径；其强制释放上限是自身的可用性边界，不能当作最新内存已持久化的证据。已有 SQL 重建与 outbox 能力见 §2，不能再把它们列为尚未实现的修法。

### 4.2 已修复：补缴欠款与已缴进度漏存

[玩家序列化桥](../../cpp/libs/services/scene/player/system/player_database_loader.cpp)已在加载时调用 `PlayerCurrencyComp::LoadFromProto`；保存时先复制 `CurrencyComp`，再调用运行时组件的 `SaveToProto` 写回 debts。顺序保证欠款不会被后续 CopyFrom 覆盖；序列化失败会拒存。

既有回归入口：`CurrencyTest.DebtPersistenceRoundTrip`。本次未运行。

### 4.3 已实现：跨节点交接的保存与 SQL 屏障

旧文“仅靠开关拒绝，能力尚未实现”已过期。当前源 scene 冻结输入、提交完整快照；[handoff_sql_barrier.h](../../cpp/libs/services/scene/player/system/handoff_sql_barrier.h) 的 `kPublishMarkLua` 要求两张 SQL ACK 均等于 pending，再在同一原子步骤转换交接资格并写标记。随后才请求 SceneManager 改派；迟到写受 owner / save_seq 围栏约束。

完整准入、撤销、回滚与目标加载协议见[跨 zone 传送](cross-zone-scene-travel.md)及当前交接源码；本轮未运行联机验收。`ReleasePlayer` 受理、Kafka offset 和路由命令 broker ACK 均不能代替玩家 SQL 双表屏障。

### 4.4 保守设计：成功快照随实体销毁

`PlayerLastPersistedSnapshotComp` 不跨实体保留。实体重建后的第一次保存会实写；这增加一次写入，避免根据失效的内存基线跳过保存。这不是数据丢失漏洞。

### 4.5 已修复：损坏载荷被当作加载成功

`OnLoaded` 对超过 INT_MAX、空字符串、空数据指针、protobuf 解析失败和异常 reply 类型执行 `FailLoad(RedisError)`，清理相应状态，不发布加载成功。null **reply** 则等待连接恢复并重试，与字符串 reply 中 `str == nullptr` 的失败语义不同。NIL 经有界重试耗尽才报告 `DataNotFound`；scene 还校验两张子表的玩家身份。

既有回归入口：`RedisLoadCorruptPayloadFailsClosed`、`RedisLoadOversizePayloadFailsClosed`、`RedisLoadNullReplyRequeuesInsteadOfFailing`。本次未运行。

## 5. 修改这条链时的检查清单

1. 每种完成信号都明确是内存受理、Redis 执行、耐久接管、Kafka 确认还是 SQL 双表提交；调用者仅使用满足自身要求的证据。
2. 跳过依据只在相应提交成功后更新；拒存、owner 被拒、schema 被拒和可重试故障保持不同结果。
3. 覆盖最新值合并、断线重连、回包丢失、重入和旧完成；失败通知不能丢载荷或提前销毁唯一副本。
4. 保存、重放、加载与换手使用同一版本 / 归属规则。新增耐久记录必须有恢复读取者及终态回收者。
5. 多键原子操作与确认必须对应实际实例 / 分片及连接；按当前玩家域迁移状态复核，不按文件名猜测。

## 6. 仍存在的丢失窗口

### 6.1 窗口 A：Redis 未提交，scene 崩溃或停机排空超时

```text
V1 是最后成功保存的版本
  → 玩家在 scene 内存中变为 V2
  → V2 已进入保存队列，但 Redis 持续不可写，严格 Lua 未成功
  → V2 只存在 ECS / saving_queue_ / pending_save_queue_ 内存
  → scene 突然崩溃，或关闭时排空预算到期仍推进退出
  → Redis、Kafka、SQL 都没有可证明的 V2；只能找回 V1 或拒登
```

[Node](../../cpp/libs/engine/core/node/system/node/node.cpp) 的 `kShutdownDrainBudget` 当前是 15 秒；`MaybeFinalizeShutdownInLoop` 在 gRPC 排空且业务预算到期时，即使仍有业务在途，也记录错误并继续 teardown。[scene main](../../cpp/nodes/scene/main.cpp)虽检查保存、剩余实体、Kafka、释放标记与改派待确认，仍受该上限约束。15 秒是业务排空预算，不是所有关闭阶段合计的精确进程退出时长。

现有修复保住的是**活进程里的重试载荷**。outbox 在严格 Lua 成功时才产生；SQL 冷恢复只能找回已提交 SQL 的内容，二者都不能重建从未离开 scene 内存的 V2。

还须单独说明保存之前的窗口：周期保存默认 300 秒，`SCENE_PLAYER_SAVE_INTERVAL_SECONDS=0` 可禁用。业务改动已向玩家确认、却尚未调用保存就崩溃，连 V2 的保存载荷都可能没有生成。**只补退出或重试队列，不等于所有已确认业务进度零丢失。** 修复范围必须按 §7.1 明确。

### 6.2 窗口 B：Redis 已执行，父快照与同实例 outbox 一起回退

```text
V2 的严格 Lua 已执行，父快照 / pending / outbox 均在 Redis 内存
  → scene 在 Kafka 钩子前崩溃，或 Kafka 未可靠接收且 SQL 尚未完整提交
  → scene 的 V2 内存副本已消失
  → Redis 崩溃丢尾 / 切到落后副本，V2 的父快照和 outbox 一起消失
  → 没有其他耐久载体持有完整 V2；补投器无从补出 V2
```

当前 C++ 的成功依据是 Redis 执行回包，不包含刷盘或副本耐久确认。因此，这个窗口与“Lua 同时写入多个键”并不矛盾：原子执行保证这些写入的提交顺序与可见性，不能单独证明跨故障保留。

边界要按剩余副本判断：

| 故障后仍保留的证据 | 当前可做什么 |
|---|---|
| Redis 中 V2 的完整 outbox 仍在 | go/db 可以继续补投，受其次数上限与终态规则约束 |
| Kafka 仍可靠保有 V2 的两条任务 | 消费 / 重试完成后，SQL 可以追平 |
| 两张 SQL 行都已提交 V2 | login 可按 §2 核验并恢复父快照 |
| 仅一张 SQL 行到 V2，另一张和所有完整载荷都丢失 | 不能将半提交当完整恢复；拒登保住一致性，但不代表追回缺失数据 |
| V2 的所有耐久载体均不存在 | 无法凭旧快照、版本号或告警重建 V2 |

Redis 身份核验、SQL 采纳、owner 计数器回退拒绝能防止扩大损坏，不能恢复所有载体都没留下的数据。SceneManager 的铸造耐久确认只覆盖相应铸造时点，不能替代之后每次保存的耐久保证。

## 7. 交 Claude Code：两处剩余窗口的修复任务（待实现）

负责人要求关闭 §6 两个窗口。本节是实施与验收要求，**不是已选定的存储方案，也不代表代码已修复**。本次只更新文档；后续 Claude 修改代码和测试，Codex 执行构建、测试与故障验证。

### 7.1 先明确提交契约，再改代码

1. 在本文补出方案、保证开始的提交点与故障模型，至少覆盖：Redis 不可写期间的 scene 进程死亡 / 15 秒排空到期，以及严格 Lua 成功后父快照与 outbox 一起丢尾 / 切主回退。
2. 区分“已取得耐久受理成功证据的完整快照可恢复”和“所有已向玩家确认的业务变更可恢复”。现有 `kQueued` 仅表示内存入队；修复后作为“受保证保存已受理”的信号，必须在耐久接管之后发出。可以增加待确认状态，但须逐一对齐调用方，不能继续把内存入队报告为受保证成功。确认前后分别杀进程：确认前不得报告耐久成功，确认后必须恢复完整快照；结果未知按原身份幂等核对。本任务的保存契约必须据此闭合，不能把原窗口移到未声明的“准备阶段”后宣布修复。
3. 所有已向玩家确认的业务变更仍包含 §6.1 的未触发周期保存窗口；尚未获得耐久确认的内存态若丢失，也必须如实列为未保证范围。若一并承诺这些变更不丢，须把耐久记录放到业务确认之前并覆盖相应业务入口，不能仅修改保存函数就宣称完成。
4. 写清进程崩溃、Pod / 宿主机消失、磁盘永久损毁分别由什么保证。本机 fsync 文件不能自动证明跨主机可恢复；若采用它，必须说明持久卷生命周期、接管者、可发现性和不覆盖的故障域。

完成标准：每个保证都有具体提交点、可恢复载体和恢复入口；不能将“已经排队”作为耐久成功，也不能靠把 15 秒改大或无限等待来关闭窗口。

### 7.2 建立可恢复的耐久提交与准入

- 窗口 A：最新完整快照须具有独立于 scene 进程内存的恢复来源。若选前置耐久意图，先保存不可变原件及身份，恢复时再通过合法提交路径；**记录待办不等于允许绕过 owner CAS 写 SQL**。
- 窗口 B：生命周期所依赖的完成证据须能覆盖所承诺的 Redis 回退故障，或将独立副本保留到可靠下游提交。单纯复制一份到同 Redis 的另一个 key，仍是同一丢失窗口。
- 恢复待办必须在新加载、换手与后继保存之前得到处理或建立准入屏障。只加写端日志、启动时没有接管者，或允许新 owner 从旧快照继续写后才发现旧待办，都不能证明修复。
- 完整原件至少绑定玩家、原父快照、两条 DBTask 原字节、home zone / topic、owner_epoch、save_seq、schema / writer 参数及必要的准入身份。重复恢复保留原任务身份，按原版本幂等提交；清理必须对应同一版本的完成证据。
- 现有 Redis 成功回调、快照比较、退出销毁、交接资格、资产 / 收件箱等消费者，须逐一说明要求哪一种完成证据，避免用“已耐久接管”冒充原有 Redis / SQL 提交。

具体存储可沿用已有能力比较，但先核对[回退防护 §0、§10](zone-redis-regression-guard-20261022.md)：既有决策已否决在**共享 hiredis 异步连接**上直接插入 WAITAOF，原因是等待刷盘会占住该连接。若采用隔离确认、批量确认或其他耐久提交方案，必须证明确认覆盖本次写入，而非在无关连接上等待后误报成功，并给出 EventLoop 非阻塞、有界排队、连接总账与失败语义。全局改 `appendfsync always` 的成本也没有本轮实测依据。

### 7.3 保持围栏、失败与清理语义

- 恢复使用原 owner / save_seq / task_id；遇更高合法版本不得用旧内容重新铸高 epoch 或增大序号覆盖。owner 缺失或倒退时保留待核对、明确拒绝，不补种或前推后强写。
- 准入与恢复须保证未恢复的受保证快照不会被新加载绕过；若无法安全判定，保持阻断并保留原件。长期拒登可以是故障处置状态，不能作为“数据已经恢复”的通过标准。
- 结果未知、确认不足、磁盘满、fsync 失败、损坏原件及超时都显式失败；失败不得污染成功快照、提前清理最后副本或生成默认存档。
- 限定重试预算、背压和接管生命周期；没有真实证据时不回收。指标使用低基数状态，日志不记录存档正文或凭证。
- 先检查并行改动，沿用现有接口与测试接缝，不重复实现 outbox、SQL 冷恢复或另造一套版本真相。若触及 30+ 文件、安装工具或其他 AGENTS.md §10.2 边界，按该门禁协调。

## 8. 修复验收与交付（Claude 写用例，Codex 执行）

| 场景 | 必须观察到的结果 |
|---|---|
| Redis 全程不可写，在修复后的“受保证保存已受理”信号之后立即杀 scene 并重启 | 该信号之前已有耐久接管，重启从原件恢复完整 V2；不能只证明还保留 V1 |
| 耐久确认之前杀 scene，或耐久写结果未知 | 确认前没有报告耐久成功；结果未知用原身份核对，不伪造成功；尚无耐久记录的内存损失按 §7.1 明确记入未保证范围 |
| Redis 不可写，正常停机越过当前 15 秒业务预算 | 在约定总预算内完成可恢复移交；移交失败明确报告，不能伪报正常排空 |
| 耐久写成功但回包丢失；重放 / 重启再次提交 | 幂等、无重复资产、无错误提前回收 |
| 同玩家 V1 在途，V2 待写，随后杀进程 | 恢复最新受保证完整值 V2；V1 完成不结束 V2 生命周期 |
| Lua 成功后、Kafka 钩子前杀 scene，保留 Redis | 既有 outbox 仍可补齐两表 SQL，保持现有能力 |
| Lua 成功，Kafka 未可靠接受 / SQL 未提交，scene 消失，再回退 Redis 父键与 outbox | 由新增耐久保证恢复完整 V2；仅拒登不算恢复通过 |
| 仅第一张 SQL 表提交 V2 后注入上述组合故障 | 不将半提交判成功；恢复后双表同版且完整内容一致 |
| 恢复与合法新 owner / 更高 save_seq 并发 | 准入顺序可证明；旧原件不提版本强写、不覆盖合法后继 |
| 日志满、持久确认不足、写 / 刷盘失败、原件损坏 | 不报告耐久成功、不清最后副本、不生成默认玩家；有明确错误与告警 |
| 业务已确认但尚未触发保存，立即杀 scene | 单独记录实际结果；若只完成 §7.1 的快照保证，明确这一窗口仍未关闭 |

已有接缝：[currency_test.cpp](../../cpp/tests/currency_test/currency_test.cpp) 的最新值合并、提交钩子顺序、严格 Lua 真 Redis、SQL 双 ACK；[cross_zone_test.cpp](../../cpp/tests/cross_zone_test/cross_zone_test.cpp) 的退出 / 交接生命周期；[replay_test.go](../../go/db/internal/saveoutbox/replay_test.go) / [replay_bounds_test.go](../../go/db/internal/saveoutbox/replay_bounds_test.go) 的补投、重启与上限；login 的 [parent_commit_barrier_test.go](../../go/login/internal/logic/pkg/dataloader/parent_commit_barrier_test.go)、[parent_load_identity_test.go](../../go/login/internal/logic/pkg/dataloader/parent_load_identity_test.go)、[parent_load_missing_history_test.go](../../go/login/internal/logic/pkg/dataloader/parent_load_missing_history_test.go)。

Claude 交付时按 AGENTS.md §10.1 提供实际受影响工程 / 服务、工作目录、完整命令、环境变量、清理步骤、通过标准和失败日志位置；新增用例给出能在修复前失败的运行入口。C++ MSBuild 必须 `/m:1` 串行。真 Redis、进程崩溃、丢尾 / 切主和停机演练不能以 mock 单测替代。性能与容量结论按 AGENTS.md §6 压测纪律保留前后汇总表，不在共享验证窗口擅自启动压测。

**完成门槛：**方案、代码、回归测试和上述适用故障场景均有实际回执；从独立进程重新加载的完整业务状态等于受保证版本，围栏与幂等未退化；未覆盖故障及保存前窗口明确列出。仅“有 ERROR”“重试未丢弃”“outbox 还在”或“登录被拒绝”都不足以判定两个窗口已关闭。

## 9. 本轮验证边界与 Java 版

本轮仅修订文档、索引说明并追加进度，执行相对链接、结构与差异检查；未修改生产代码、协议、配置或生成物，未构建、运行单测、启动服务或注入故障。§7–§8 全部是后续待实现 / 待验证任务。

Java 版归属与存档加载以 SQL 为准，与本仓 Redis 父快照 / Kafka / go/db 链不同构，见[回退防护 §12](zone-redis-regression-guard-20261022.md#12-java-版agents-12)。本轮没有功能变更，不修改 Java 仓及 PARITY.md；不能据此宣布 Java 进程内待保存状态也已具备零丢失保证。后续若扩大业务提交保证，须按 AGENTS.md §12 核对另一版对应的进程死亡边界，并记录适用性与验收结果。

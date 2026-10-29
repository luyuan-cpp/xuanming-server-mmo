# 全仓数据问题与覆盖回档审计（2026-10-18）

> 只读静态审计，**未编译、未运行**。对象是 2026-10-18 的工作区（包括其他会话尚未提交的改动）。
> 本文只登记问题和修复方向，不包含任何代码修改。修复要按 AGENTS.md §10.1 交给 Codex 验证，并按 §12 同步 Java 版。
> **2026-10-19 状态：** §2.1–§2.3 已修复，验证还没跑完；§2.4、§2.5 尚未动手。汇总见 §7，上线前的复查计划见 §8。
> **2026-10-19 更新：** §2.4 三条已落码并带回归测试，未编译、未测试，待 Codex 验证；§2.5 仍未动手。

## 0. 结论

- **有问题。** 10 个子系统一共报了 49 条：24 条 medium、25 条 low，没有 high。
- 对抗复核了 26 条：全部 24 条 medium，加上 2 条与之同根因的 low。**全部成立，没有被推翻的**。24 条 medium 里，复核后有 10 条降为 low；另有 2 对是重复，所以 medium 去重后剩 **12 个根因**。
- 其余 23 条 low **没有复核**。
- 默认配置下没有发现"必然发生的资产复制"。真正的"覆盖回档"只在两处出现：一是 go/db 的 legacy 模式没有行内围栏，二是运维工具被误用。默认配置下最主要的风险是**丢进度**，形态是：location 被删除，但旧持有节点还没把数据落盘。这时 epoch 守卫会拒掉旧节点的存盘，保证旧数据不会盖到新数据上；代价是被拒的那段进度直接没了。
- 用户没有开启的功能（全球匹配、交易资产通道、收件箱消费）里另有 5 个 medium，开通前必须修掉。

## 1. 方法与范围

| 区域 | 覆盖内容 |
|---|---|
| cpp-save-producer | C++ 存盘生产端：置脏、快照、save_seq、父快照提交屏障、DBTask 投递 |
| load-handover | 加载、进场、重连、切场景交接（C++ 场景侧 + go/scene_manager） |
| go-db-pipeline | go/db 消费、围栏、重试、游标、补投、子表缓存 |
| data-service | go/data_service：路由、号段、快照、回档、调试工具 |
| login-session-route | go/login、go/player_locator、go/client_rpc_router、gate 会话 |
| cpp-economy | C++ 背包、货币、战斗结算、资产操作、收件箱执行 |
| go-asset-services | go/trade、go/mail、go/player_inbox、shared/assetop |
| social-match-battle | guild、friend、rank、chat、match、battle 节点 |
| ops-tools-java | tools/merge_zone、回档脚本、schemamigrate、Java gateway_node 与 mall_game |
| go-shared-primitives | snowflake、号段、ownerepoch、savebarrier、leader、kafka 封装 |

- **复核方式：** 全部 medium 加 2 条同根因的 low，按根因分 5 组，每组一名复核员，任务是逐条尝试推翻。
- **不在范围内：**
  - Java 版服务器仓库（`D:\luyuan\xuanming-server-mmo`）。
  - Unity 客户端。
- **各区域未逐行读完的部分：**
  - `player_lifecycle.cpp` 的若干段：交接取证、改派待确认表、EnterScene 应答分派。
  - 各业务系统的持久组件写入口：是否绕过冻结闸或退出闸，没有逐个核对。
  - go/db：`retry_stranded.go`、`durable_enqueue.go` 主体。
  - loginqueue。
  - 帮会逻辑层正文。
  - `k8s_zone_rollback.ps1` 正文。

## 2. 复核后的 medium（12 个根因）

### 2.1 删 location 与持有者落盘未绑定（默认可达，回档到上次成功存盘）

| 编号 | 问题 | 位置 | 复核 | 已知 |
|---|---|---|---|---|
| load-handover#1（⊇ login-session-route#0） | LeaveGame 和 gate 崩溃都会删除 location，但不让持有节点退出或存盘。下次进场时 location 已不存在，于是铸新 epoch、跳过换手门；新节点读到旧快照，旧节点后续存盘被 CAS 拒绝后，实体直接销毁 | `go/scene_manager/internal/logic/enterscenelogic.go:550`、`:680`；`go/player_locator/internal/logic/markofflinelogic.go:107` | 成立，medium | 新 |
| load-handover#0 | 同一连接 LeaveGame 后重进同一频道：gate 的 `ApplyRoute` 不看 epoch，持有节点永远停在旧 epoch，下次真正写盘时被当作"已废黜"销毁 | `cpp/nodes/gate/handler/event/scene_route_helper.h:99-113`、`:165-179` | 成立，medium | 新 |
| cpp-save-producer#0（= load-handover#2） | 退出存盘超过 30s 断线租约仍未落地（Redis 对该节点写失败），租约到期后 LeaveScene 无条件删除 location，后续时序同上 | `cpp/libs/services/scene/player/system/player_lifecycle.cpp:4512`、`:3123-3144` | 成立，medium | 已登记（layer 3 注释、player-async-save-loss-windows.md §4.1） |

**补充说明：**
- gate 不处理 LeaveGame。ExitGame 只在 TCP 断开时代发。
- `markofflinelogic.go` 的同步 LeaveScene 原本是为了修复"location 永久残留"而加的，结果引入了本问题。
- 客户端上线"回选角"后，load-handover#0 会**必然**触发。
- 改包客户端今天就能触发。

**修复方向：** 把"删 location"绑定到"持有者已释放"上。
- LeaveGame 成功后，由 gate 代发 ExitGame，或由 scene_manager 发带 `expected_owner_epoch` 的 ReleasePlayer。
- MarkOffline 与租约到期两条路径：只有当前代 A1′ 标记已写出、或持有节点确认已死时，才删除 location；否则保留 location，让下次进场走换手门。
- 纵深防御：gate 的 `ApplyRoute` 在 epoch 变化时也转发一次。
- 补指标：`stale_owner_write_rejected` 与"退出中且从未落地"同时出现，就是回档的直接证据，应单独计数。

**修复（2026-10-18，代码与测试已落工作区，未编译、未测试，待 Codex 验证）：** 方案与不变量见 [location-release-binding.md](location-release-binding.md)。scene_manager 的 LeaveScene 只在拿到当前代 handoff 标记 / 持有节点已死 / 没有持有者时才删 location，否则保留并经 `ReleasePlayer.session_ended` 请持有节点释放（等满 600s 上限才按旧行为删并计 `forced`）；gate 收到 LeaveGame 时代发 ExitGame 并清空会话路由状态，`ApplyRoute` 加同节点新 epoch 规则；新增 scene `stale_owner_rejected_exiting` 与 `scene_manager_leave_holder_release_total`。

**修复后续（2026-10-18 ~ 10-19，均已落工作区，未编译、未测试、未提交，待 Codex 验证）：**
- **登录撞上未释放的持有者**（设计 §12，收口 §6 残余 5）：gate 崩溃后玩家立刻从别的 gate 重登、被派到别的节点时，换手门回 18 之前由 scene_manager **同步**请 location 的持有节点按会话结束释放，不再等旧会话的断线租约到期才有人请它。指标 `scene_manager_enter_holder_release_total{verdict}`。
- **迟到的会话结束释放误退新会话**（设计 §13，收口 §12.5 残余 1）：
  - 问题：同物理节点重登不铸造 owner_epoch，同一代下实体会被新登录复用。`session_ended` 释放迟到执行时（登录撞门的同步请求超时之后 / LeaveScene 的后台派发晚于新登录），只比 owner_epoch 会把已绑上新会话的实体按断线踢下线。先存盘、不回档，但在线玩家被踢。
  - 修复：`ReleasePlayerRequest` 追加 `covered_session_id = 6` / `covered_login_generation = 7`（这次释放覆盖到的最新登录身份）。scene 在归属代际判释放之后，再比实体的 `PlayerLoginIdentityComp`：实体代次更大，或同代而会话不同，即不退出（先比代次、后比会话号，与 `decideLeaveIdentity` 同口径）。代次 0 = 旧版调用方，按原行为。
  - 填法：LeaveScene 填离场请求的身份；登录撞门填**本次登录自己的身份**。只填 location 的旧身份，会让经 gate 补发绑上更新会话的持有者永不释放、等满 600s 才 `forced`，见设计 §13.2。
  - 指标：scene `[ReleaseFence]` 行新增 `binding_*` 计数，`binding_skip_newer_login` / `binding_skip_conflict` 增长时升 WARN。
  - 复核：四个视角只读评审 + 逐条对抗核实，未发现生产代码问题；确认两处测试缺口并已补用例。
  - Java 版：只读核实，每次登录都自增 owner_epoch、让出请求按 epoch 严格相等，迟到请求碰不到新实例。已有等价行为，不改（设计 §13.10）。
- **仍开着的残余**：释放先于新登录的进场到达持有节点时照常退出（新进场取消退出或重载，不回档）；混版窗口退回旧行为；其余见设计 §6、§12.5、§13.5。

### 2.2 go/db 存盘协议世代不一致（默认配置）

| 编号 | 问题 | 位置 | 复核 | 已知 |
|---|---|---|---|---|
| go-db-pipeline#0 | `go/db/etc/db.yaml` 缺省 `Fencing.Mode: legacy`，K8s 原样沿用。C++ 只要 `owner_epoch≠0` 就无条件走严格提交屏障，login 要求两表 ACK 都等于 pending，而 go/db 只在 dual 下写 ACK。结果是玩家第二次登录起全部失败，跨场景交接只能等冻结上限 | `go/db/internal/kafka/player_cache_publish.go:83`；`go/login/internal/logic/pkg/dataloader/parent_commit_barrier.go:36-43`；`go/db/internal/saveoutbox/service.go:26` | 成立，medium（fail-closed，不直接丢资产） | 新（db-write-fencing-mysql.md:1527 写了"必须升级为 dual"，但缺省值和 §8.4/§8.6 的"混跑安全/随时回退"说法没有跟着改） |
| go-db-pipeline#1 | 存盘补投器没有重投上限、退避或终态标记。存储冷却时每约 6.4s 整行重投一次；遇到确定性拒写（大字段闸）的玩家，每天新增约 1.3 万条接近 1MB 的死信，足以写满 noeviction 的共享 Redis | `go/db/internal/saveoutbox/replay.go:146`、`scripts.go:22-42` | 成立，medium（仅 dual，但 dual 实际上是唯一能工作的模式） | 新 |

**修复方向：**
- 加协议世代的机器门禁：go/db 发现自己是 legacy 而 zone 已是 strict 世代时，拒绝启动。
- 修正 db.yaml 的缺省值、注释，以及 §8.4/§8.6 的说法。
- 补投意图记录尝试次数，做指数退避，设上限后转告警。
- go/db 对确定性拒写在 barrier 上标记终态，补投器看到终态就停止重投。

**修复（2026-10-18，Claude，未编译，待 Codex 验证）**：本节两条与 go-db-pipeline#2 已落码。做法见 [db-write-fencing-mysql.md](db-write-fencing-mysql.md) 文末「2026-10-18 存盘协议世代门禁与补投有界」，以及 §4.7、§8.4、§8.6 的同日更正；进度见 `docs/PROGRESS.md` 同日条目。
- go/db 缺省改为 `dual`。
- legacy 由世代标记与补投桶机器判定，有严格世代证据时拒启；运行中发现即停机。
- 补投改为首见宽限、指数退避、次数上限转告警、终态停止，并在在途重试、主库冷却、legacy 存活时跳过。
- 负责人 2026-10-18 确认尚未上线：混跑、降级、旧二进制排除和回档 `-Apply` 暂不可用这几条残余在上线前不是阻塞项。首次上线直接以 dual 加严格屏障整批起步，见设计文档该附注末段「上线状态」。

### 2.3 跨账号登录态继承（默认可达，安全 + 数据）

| 编号 | 问题 | 位置 | 复核 | 已知 |
|---|---|---|---|---|
| login-session-route#1 | `login_session` 只用 uint32 session_id 建键。gate 崩溃后以同一 node_id 重启、序号从 0 开始，30min TTL 内跳过 Login 的客户端可以继承别人的账号，用它建角、进别人的角色 | `go/login/internal/logic/pkg/loginsession/loginsession.go:38` | 成立，medium | 已登记（client-request-ordering.md §7.2 第 2 条，未修） |

**修复方向：**
- 键改为与会话栅栏一致的 `{gate_instance_id}:{incarnation}`。
- gate 对 CreatePlayer / EnterGame 要求本连接先完成 Login。

> 2026-10-18 已修，未编译、未运行，待 Codex 验证。改动与滚动升级说明见 [client-request-ordering.md](./client-request-ordering.md) §7.2 第 2 条，Java 版不受影响。

### 2.4 战斗结算丢数据

| 编号 | 问题 | 位置 | 复核 | 已知 |
|---|---|---|---|---|
| cpp-economy#0 | 物品号段暂时发不出号时，掉落被吞掉，账本却照样 `RecordApplied`。资产通道遇到同样情况是先预检、不够就回 RETRY，战斗结算没有这么做 | `cpp/libs/services/scene/battle/system/player_battle.cpp:803-817`、`:1800` | 成立，medium（默认可达，需要 data_service 持续故障十几分钟以上） | 新 |
| cpp-economy#1 | 玩家被 GM 封锁金币获取后，`AddCurrency` 返回终局的 `kAssetBlocked`，结算却按暂时失败处理，永远应用不上。药品不扣，掉落和任务进度丢失；下一局开打时 pending 被覆盖 | `player_battle.cpp:1693-1710` | 成立，medium（收件箱消费默认关闭，开启后可达） | 新 |
| social-match-battle#2 | 全球模式下，battle 节点在处理 Kafka 回执前停机或崩溃：评分与帮会历练照常入账，个人结算永不派发。`match_battle_settlement_source` 没有任何读者 | `cpp/nodes/battle/logic/battle_room_manager.cpp:1615-1643` | 成立，medium（`GlobalMatch.Enabled` 门控） | 文档 cross-zone-matchmaking.md:548 的结论与代码不符 |

**修复方向：**
- 结算进入不可重复的副作用（金币）之前，先预检号段余量，不够就整笔延后。
- 区分 `kAssetBlocked`（跳过金币、继续结算）与 `kAssetFrozen`（延后）。
- 停机时先 flush 并处理完回执，再清房；或者把 pending 的落库提前到发结果之前。

**修复（2026-10-19，Claude，代码与回归测试已落工作区，未编译、未测试、未提交，待 Codex 验证）：**
- **cpp-economy#0**：`ApplySettlementToEntity` 在应用缓存与金币之前，调用 `HasItemGuidsForDrops` 预检。
  - 需求量取 `BagService::MaxNewInstancesFor(掉落)`：每个 config 按 `StacksNeededFor(count, max_stack)` 计，是上界。主背包放不下时一个号都不铸，所以一份余量也够「转投临时格」那一次。
  - 余量不够就整笔返回 false，零副作用，保留 pending 等重投；打 `metric=battle_settlement_item_guid_shortage`。
  - 背包满、物品被全服禁发这类终局失败，维持原来的「记 ERROR、继续」。
  - 残余：延后之后，谁来重试取决于路径。
    - 在线且还挂着 `InBattleComp`：发件箱 10s × 12 轮重投，冻结到期时 reaper 再补读一次。
    - 登录钩子或 reaper 补读时恰逢号段仍缺：之后没有周期重试，只能等下次登录。
    - 若号段缺口长过锁的 TTL，玩家又开了下一局，单槽 pending 会被覆盖，这一局整笔丢失。这是 turn-battle-gap-closure.md §9.3「pending 每玩家单槽」的既有残余，本次没改。修复前同一场景只丢掉落，修复后金币与进度会一起丢，但前提是 data_service 故障长过锁的 TTL 且期间玩家又开战。
  - 掉落汇总抽成 `CollectGainedItems`，预检与入包共用一份；同一 config 多行累加改为在 uint32 上限饱和，原先会回绕。
- **cpp-economy#1**：按 `AddCurrency` 的返回码契约分类。
  - `kAssetBlocked`（GM 单人封禁或全服 GainBlock）只跳过金币，其余照常应用并登记账本；打 `metric=battle_settlement_gold_blocked`，结算日志加 `gold_blocked=`。解封后同一局的重投被账本挡住，金币不补发。
  - 其余失败（`kAssetFrozen`、缺组件）仍整笔延后。
  - 「记流水」目前只有 metric 日志：`TransactionType` 没有「封禁拒发」类型，加类型要改 proto，本次没做。
- **social-match-battle#2**：采用第二个方向，把个人 pending 的落库提前到结果第一次发往 broker 之前。
  - `ResultCommit` 新增发送前置条件：`BeginStaging` / `FinishStaging` 一轮落完全部成员的 pending 之后，`CanSend` 才可能为真。落库失败按与投递相同的 1→30s 有界退避重来，过期轮次的迟到回复不改状态。
  - `BattleRoomManager::StagePersonalSettlements` 由 pump 驱动，按成员 EVAL 原 `kSetPendingSettlementScript`；Redis 未连上时不开新一轮。确认模式不再「不落库直接发」。
  - 字节来源统一为 `BuildPersonalSettlementPayloads(冻结原件)`，房间上存一份 `personalSettlementPayloads`。回执之后的 `FinishBattle` 改走 `DispatchStagedSettlement`，只投命令、登记 outbox，**不再改写 pending**；改写会让已销账的 pending 复活，玩家重复领奖。
  - `AbortAllRooms` 不删已落的 pending，停机与崩溃都由 scene 登录钩子或冻结到期补读应用。
  - 语义变化与残余见 [cross-zone-matchmaking.md](cross-zone-matchmaking.md) §13.6「2026-10-19 更正」。
- **回归测试**：
  - `bag_test`：`ItemGuidShortageDefersWholeSettlementBeforeGold`、`ExactItemGuidCapacityCoversTemporaryFallback`、`BagServiceNewInstanceBound.*`、`GmCurrencyBlockSkipsGoldButAppliesRestOfSettlement`、`GlobalCurrencyGainBlockSkipsGoldButAppliesRestOfSettlement`。
  - `turn_battle_engine_test`：`ResultIsNeverSentBeforePersonalSettlementsAreDurable`、`FailedStagingUsesMonotonicBoundedBackoff`、`PersonalSettlementPayloadsAreTheFrozenSourceBytes`、`PersonalSettlementPayloadsRejectIncompleteSource`。既有 3 个 `BattleResultCommit` 用例补了落库前置步骤，断言没有改。
  - battle 节点的 Redis / Kafka 编排没有单测接缝，`BattleRoomManager` 绑在两个单例上，停机丢结算无法在单测里稳定复现。替代验证见 `docs/PROGRESS.md` 同日条目的可选冒烟。
- **Java 版**（`D:\luyuan\xuanming-server-mmo`，0.1.0-SNAPSHOT）：只读核实，三条都有同样问题，**待做**。
  - 第 1 条：`BattleSettlementService.applyGained` 不区分发号失败与包满，照常 `ledger.record`。
  - 第 2 条：金币 `!change.ok()` 一律整笔 DEFERRED。PARITY 第 134 行把它登记为照搬的基线怪癖 B7。
  - 第 3 条：`BattleRoomServiceImpl` 回执后才落 pending，`abortAll` 不派发。
  - 已在 Java 仓 `PARITY.md` 末尾登记一行，状态为「Java 待做」（2026-10-19）。

### 2.5 未开通功能里的卡死与滞留（开通前必须修）

| 编号 | 问题 | 位置 | 复核 | 门控 |
|---|---|---|---|---|
| social-match-battle#0 | 全球匹配的票据与 battle 实例 uuid 强绑定。节点重启或被替换后，state 2/3 的票据永久卡住，玩家再也进不了任何匹配入口 | `battle_room_manager.cpp:807-814`；`go/match/internal/logic/global_match.go:173-192` | 成立，medium | `GlobalMatch.Enabled` |
| go-asset-services#0 | `Commerce.Tick` 只要单条合同出错就整轮返回。失败行的 `updated_ms` 不变，永远排在队头，全服交易被卡住。最容易触发的是离线买家累计 16 条未决 credit 后返回的 `ErrTooManyPending` | `go/trade/internal/commerce/engine.go:768-822` | 成立，medium | `AssetOp.Enabled` |
| go-asset-services#1 | 被自动放弃（180 天）或人工终结的托管扣款腿不写 `durable`，合同永远停在 pay/escrow，卖家托管的资产被扣留。买家点击购买后下线，就能把卖家资产扣作人质 | `engine.go:676-686`；`go/trade/internal/data/asset_op_repo.go:1013-1021` | 成立，medium | `AssetOp.Enabled` |

**修复（social-match-battle#0：2026-10-23，Claude，复核部分成立，未编译、未测试，待 Codex 验证；负责人 2026-10-23 拍板首发开全球匹配，本条为上线阻塞）：**
- 并行会话的 v1 终局协议已给「有 Create 原件」的成局加了截止后 SQL ABORT，第一轮 §4 第 11 条也被它覆盖。仍成立两处：没有原件的成局要等目标 battle 回 ABORTED，目标消失、换 UUID、拨号失败时永无出口；有原件且已过截止的成局被 `AcquireWork` / `Pending` 反复续租，到期 ABORT 被无限推迟。
- 修复：本代租约下无原件即正面证据（`Store.CreateIntentPersisted`），`globalDestroy` 不再等目标应答，由 `Finish(false)` 在行锁内复核同租约且仍无原件，多副本只有持租者能提交；过截止的有原件成局不再发租约（`ErrAwaitingExpiredOutcome`），交给 `RecoverExpiredOutcomes` 争唯一一次 SQL ABORT。截止前一次超时或实例消失仍只记 UNKNOWN，不释放。没有采用「注册中心 UUID 消失超过宽限期」作证据。另加到期巡检 `expired_unrecoverable` 指标。说明见 [cross-zone-matchmaking.md](cross-zone-matchmaking.md) §13.3；命令见 PROGRESS「2026-10-24 Claude：第一轮审计 §2.5 social-match-battle#0」。
- 残余：截止竞态（强制平局与到期 ABORT 抢同一主键）归并行 v1 轨道，须在其验收前加宽限；安全论证依赖该轨道未提交的 `Finish(false)` 复核。
- Java 版：无原件卡死同样存在，Java 待做，已登记 PARITY。

**修复（go-asset-services#0 / #1：2026-10-20 落码，2026-10-22 按独立评审修正；Claude，未编译、未测试，待 Codex 验证）：**
- **#0**：`Engine.Tick` 改为单行隔离（`go/trade/internal/commerce/tick_isolation.go`）。失败分 retry / too_many_pending / corrupt / conflict / role_conflict：暂时失败按有界抖动退避推迟，推迟以 Due 读到的 version 为条件、不改 version，不会旧盖新；确定性坏数据隔离为 Manual，不再无限重试；Due 只取 `updated_ms<=now`。评审发现 `7d9208237` 起每通道只给 3s、而单行子预算固定 5s，卡住的行与整轮同时到期、不推迟，队头阻塞复现；已改为单行预算从父截止时刻推出，并预留 ACK 与推迟的余量。bid_held、acceptBid、撮合、收据 ACK、商城各循环同样处理。
- **#1**：`LegResult` 终局只看 status、不看 durable，放弃与人工终结的扣款腿走与 scene 判拒同一条补偿，只补偿一次；人工判 APPLIED 的实例扣款没有原件时立即隔离，不上架。
- 新增 trade-commerce 告警 5 条与 [trade-commerce-quarantine-runbook.md](../ops/trade-commerce-quarantine-runbook.md)。说明见 [jubaozhai-market.md](jubaozhai-market.md) §15.3；命令见 PROGRESS「2026-10-24 Claude：第一轮审计 §2.5 go-asset-services#0 / #1 交易合同推进修复」。
- 残余：人质窗口只是有界（到自动放弃天数或人工终结）；隔离为 Manual 的合同没有管理入口，只有手册里的只读 SQL。
- Java 版：#0 / #1 本体已有等价；本次评审修正的单行预算余量与分类细节 Java 待做，已登记 PARITY。

## 3. 复核后降为 low 的（10 条）

| 编号 | 问题 | 降级理由 |
|---|---|---|
| go-db-pipeline#3 | legacy 下 MySQL 写没有行内围栏，实例卡顿超过租约时会用旧快照覆盖新版本（G1） | 需要单进程停顿 90s~2min；dual 下已封死；legacy 本身因 2.2 已不可用 |
| go-db-pipeline#4 | Redis AOF 丢尾导致游标与收据回退，legacy 下旧重试覆盖 MySQL | 设计已登记接受的残余（db-retry-owner-routing.md §7.7）；dual 下已封死 |
| data-service#0 | `DeletePlayerData` / `SetPlayerField` 没有鉴权，且与在役的 `save_barrier`、`asset_sql_commit` 同前缀，会删除或覆盖它们 | 仓库里没有调用方，需要运维显式调用 |
| data-service#1 | `debug_import` 用 float64 解析 uint64 的 player_id；`-player` 覆盖时不改行内的 id | 调试工具，默认 `--dry-run=true` |
| data-service#2 | 全局库重建或回档后号段从 1 重新种，item/txlog/snapshot 会重发已用号 | 已登记的运维事件；player 号有 login 探测兜底 |
| data-service#3 | 落点映射只存在 mapping Redis，丢失后会写错库 | 已登记、修复在途；受害面窄 |
| login-session-route#2 | 账号角色目录只在核心 Redis，丢键后角色从账号消失 | 已登记；需要 Redis 丢数据；角色数据仍在库里 |
| cpp-economy#2 | 主背包满时掉落进临时格，临时格没有取出接口，满 200 个后按 FIFO 淘汰 | 实例仍随存档持久化，补一个搬运 RPC 即可找回；"没有取出接口"是新发现 |
| ops-tools-java#0 | copy 模式在开服后撤销合服，会静默回档到合服时刻 | 非默认的 copy 模式，且需要违反 runbook §10.2；dst 上的行不删除 |
| ops-tools-java#1 | 灾难级 PITR 只回退 zone 库，全局库不回退，会造成复制或白亏 | 负责人已接受的代价（zone_data_rollback.md 2026-10-18 修订）；当前 `-Apply` 不可执行。建议在 inventory 阶段输出跨库资产操作清单 |

## 4. low 项

> 另有 2 条 low 已复核成立、仍维持 low：load-handover#3（A2′ 换 epoch 后沿用旧暂存）、go-db-pipeline#2（legacy/dual 混跑时补投覆盖）。下面 23 条**没有复核**。

- **C++ 存盘与加载**
  - `SanitizeSkillList` 在加载时按当前技能表永久删除玩家技能（`player_skill.cpp:104`，两个区域独立报告）。
  - 父快照提交前不检查行大小，DBTask 被永久拒绝后玩家被登录预检锁死（`player_lifecycle.cpp:3292`）。
  - 严格提交屏障取消了 owner 缺键补种，Redis 丢数据后在线玩家会被判废黜，scene-owner-reentry-barrier.md §6.2 描述已过时（`redis_client.h:153`）。
  - 存盘不保留未知字段，scene 回退到旧镜像时会抹掉新字段（`player_data_loader.h:16`）。
- **C++ 资产**
  - `AddCurrency` 没有 uint64 溢出检查（`currency_system.cpp:203`）。
  - `AttachDebt` 合并欠款时会让已过期的旧欠款复活（`:357`）。
  - 交易流水在内存改动时就发出，早于落盘（`transaction_log_system.cpp:84`）。
- **login**
  - 账号 blob 有两个写者不走建角锁（`entergamelogic.go:256`）。
  - 会话栅栏的 player 绑定随 TTL 过期，之后同一连接可以绑第二个角色（`sessionfence/fence.go:158`）。
- **资产服务**
  - 收件箱 Ack 以 Redis 落盘为依据，Redis 丢尾后 GM 处置静默丢失（`store_consume.go:308`）。
- **匹配**
  - 已打完的战斗被全球匹配恢复误判为"未建成"，票据回队（`battle_room_manager.cpp:813`）。
  - 评级来源停写后结果消费卡死（`rating.go:71`）。
- **运维工具与 Java**
  - 合服 P3 只取一次积压快照（`preflight.go:851`）。
  - `-skip-player-rows` 不校验落点（`merge_run.go:429`）。
  - 撤销时榜单 ZADD 导致分数回退（`guild_step.go:443`）。
  - merge_zone 不识别 db_first（`main.go:540`）。
  - `kafka_offset_reset` 不检查回档切点（`kafka_offset_reset.ps1:589`）。
  - 区服 PUT 整行覆盖，漏传字段会把区服改成 OPEN（`AdminZoneController.java:131`）。
  - 商城第二笔支付回执被拒，且不生成退款（`CommerceStore.java:126`）。
- **身份原语**
  - FenceClock 在生产上退化为单调钟（`fenceclock.go:102`，文档称已修但仍有问题）。
  - reclaim 在 slots 键过期时绕过隔离期（`allocator.go:1113`）。
  - login 的毫秒水位前推量 2000ms 不满足契约（`login.go:51`，只在关闭号段时可达）。

## 5. 修复优先级建议

1. **2.1 删 location 与持有者释放绑定。** 默认可达的回档，"回选角"上线前必须修。涉及 gate、login、player_locator、scene_manager、C++ 场景，需要先写设计。
2. **2.2 协议世代门禁与补投上限。** 当前缺省配置下登录不可用；dual 是唯一可用的模式，而 dual 下补投会放大。
3. **2.3 `login_session` 键绑定 incarnation。** 跨账号接管，改动小。
4. **2.4 战斗结算三处。** cpp-economy#0 默认可达；另外两处在开通收件箱、全球匹配前修。
5. **2.5 三处。** 开通全球匹配、交易资产通道前修。
6. **low 项。** 按需处理，优先 `SanitizeSkillList` 与 `AddCurrency` 溢出。

## 6. 证据

- 工作流运行：`wf_f7c7b2f0-c42`。会话转录目录里的 `journal.jsonl` 保存每个代理的完整返回，包括查找员的触发时序和复核员给出的 文件:行 证据。
- 本审计只读，未修改任何代码。

## 7. 修复与验证状态（截至 2026-10-19）

审计之后，2026-10-18 起开了 3 个修复会话，§2.1 的修复会话又派生出 2 个后续会话。代码均由 Claude 落进工作区，已随用户的工作区快照提交（`8b9592de3`、`e611b5d8d`、`1fc103dce`）。协议生成、编译、测试由 Codex 负责。下表"验证"一列以 `docs/PROGRESS.md` 里 Codex 的条目为准。

| 审计项 | 修复内容 | 设计 / 进度条目 | 验证 |
|---|---|---|---|
| §2.1 删 location 与持有者释放绑定 | 只有拿到"持有者已释放"的证据才删 location，否则经 `ReleasePlayer.session_ended` 请持有节点释放，600s 后兜底强删；gate 收到 LeaveGame 时代发 ExitGame；`ApplyRoute` 补上 owner_epoch 维度；新增回档证据指标 | [location-release-binding.md](location-release-binding.md)；PROGRESS「删 location 绑定持有者释放」「静态评审复核两项修正」；事故报告 [incident-location-delete-rollback-2026-10-18.md](../ops/incident-location-delete-rollback-2026-10-18.md) | 已通过：协议生成、6 个 Go 服务编译、C++ 增量 Build。scene_manager 用例分两轮才取得成功记录，不算单轮全绿。未完成：player_locator 测试、`routing_identity_test` 与 `cross_zone_test` 运行、真实环境故障演练 |
| └ 登录撞上未释放的持有者（设计 §12） | 换手门拒绝之前，同步请持有节点释放 | PROGRESS「登录撞上未释放的持有者」 | 同上，与 §2.1 一并验证 |
| └ 迟到的会话结束释放带覆盖身份（设计 §13） | `ReleasePlayerRequest` 追加 `covered_session_id=6` / `covered_login_generation=7`；scene 按登录身份比对后再决定是否退出 | PROGRESS「会话结束释放带覆盖身份」 | scene_manager：vet、定向用例、全量 444 + 226 个用例通过。C++ 55 项中 54 项通过；`EnterFenceEcs.LoadCallbackBindsNewestAdmitted` 因测试夹具给的是空快照而失败，Codex 已备夹具补丁，待负责人决定 |
| §2.2 go/db 协议世代门禁与补投上限 | 缺省改为 dual；legacy 由机器判定，有证据时拒绝启动；补投加首见宽限、指数退避、次数上限和终态 | [db-write-fencing-mysql.md](db-write-fencing-mysql.md) 文末附注；PROGRESS「存盘协议世代门禁与补投有界」 | **7 步验证都没跑。** 负责人确认尚未上线：首次上线直接用 dual 加严格屏障整批起步，混跑和降级相关的残余不构成阻塞 |
| §2.3 `login_session` 键绑定 gate 实例与代次 | 键改为 `{gate_instance_id}:{incarnation}`，旧版 gate 走兼容键；gate 要求先 Login | [client-request-ordering.md](client-request-ordering.md) §7.2 第 2 条；PROGRESS「修跨账号登录态继承」 | Codex 已跑 login 两个相关包：249 通过，vet 与 build 通过。没有跑真实多 gate 环境 |
| §2.4 战斗结算三处 | 号段预检后整笔延后；金币封禁只跳过金币；全球模式个人 pending 在发结果前落库，回执后只派发、不改写 | 本文 §2.4「修复」；[cross-zone-matchmaking.md](cross-zone-matchmaking.md) §13.6「2026-10-19 更正」；PROGRESS「审计 §2.4 战斗结算三处」 | **未编译、未测试**，待 Codex 跑 `bag_test`、`turn_battle_engine_test` 与 battle 节点构建。Java 版三条同样存在，待做 |
| §2.5 全球匹配、交易三处 | **未动手**（对应功能未开通） | — | — |
| §3、§4 low 项 | 只有"存盘不保留未知字段"一项另出了设计稿：[mixed-version-persist-field-protection.md](mixed-version-persist-field-protection.md)，v0.2，未落码，待负责人拍板 | PROGRESS「新旧版本混跑的玩家持久字段保护设计稿 v0.2」 | — |

## 8. 上线前复查计划（2026-10-19 建议）

**结论：上线前需要再查。** 但不是把这次审计原样再跑一遍，而是补上本轮没覆盖到的部分。理由如下：

- 本轮每个区域只派了一名查找员，跑一轮就停了，没有做到"连续几轮都查不出新问题才停"。§1 列出了未读完的代码段，23 条 low 也没有复核，尾部还有多少问题不知道。
- 审计之后又落了大量新代码，修复本身也会引入新风险。例如 600s 兜底强删会退回旧行为，混版窗口里也会退回旧行为。这些新代码目前只做过静态评审。
- 静态审计不能代替运行证据。§2.1 的真实环境故障演练还没开始，§2.2 的 7 步验证还没跑。
- Java 版服务器仓库不在本轮范围内，而按 AGENTS §12，两个版本要一起上线。

**建议顺序**（前一阶段不过，不开下一阶段）：

1. **先把已落的修复验证完（Codex）。**
   - §2.2 的 7 步验证。
   - §2.1 遗留的测试：player_locator、`routing_identity_test`、`cross_zone_test`，以及夹具失败那一条。
   - 真实环境故障演练：登出、租约到期、gate kill -9、Redis 对单节点写失败。演练时 scene_manager 必须设 `AllowUnsafeCrossNodeHandoff: false`，否则测不到修复。
2. **修掉剩余的 medium。**
   - §2.4 的 cpp-economy#0 默认就能触发，上线前必修。
   - cpp-economy#1 在开收件箱消费之前修；social-match-battle#2 在开全球匹配之前修。
   - §2.5 三项在对应功能开通前修，并作为该功能的开通门禁。
3. **第二轮 AI 审计（只读）。** 换一种查法，不重复第一轮的分区查找：
   - **增量对抗评审**：从本审计的基线（`bf3c60632`）到当时 HEAD，把所有涉及存盘、加载、会话、资产的改动，逐个按"会不会丢、会不会旧盖新、会不会复制"去推翻。
   - **补覆盖**：把 §1 列出的未读段与各业务系统的持久组件写入口逐个读完。
   - **复核** 23 条 low。
   - **换视角连续查找**：每轮换一个视角，连续两轮没有新发现才停。视角包括：
     - 按资产追踪：货币、物品、宠物、帮会资金的完整生命周期；
     - 按故障：Redis 丢尾或主从切换、Kafka 重投、节点崩溃、时钟回拨、滚动升级混版；
     - 按入口：每个 handler 与 RPC。
   - **Java 版做同类审计**：在 Java 仓库执行，按该仓库的 `AGENTS.md`。
4. **运行期一致性压测（Codex）。** 按 [data-consistency-stress-testing.md](data-consistency-stress-testing.md) 跑 L2 校验器、L3 机器人、L4 混沌（消费中途杀进程、Redis 故障切换、gate 与 battle 节点强杀），结论按 AGENTS §6 出对比表。
5. **代码冻结后做最后一次增量审计。** 范围是从第 3 步的基线到发布提交的差异。

**额度参考**（同账号实测，见会话记忆）：
- 只读代理每个约占 5 小时窗口的 2.5%。本轮 10 个查找员加 5 个复核员，约用掉 640 万子代理 token。
- 第 3 步预计要 20–40 个代理，需要分到多个额度窗口里跑。查找与复核分阶段进行：先只跑查找，再按剩余额度决定复核的粒度。

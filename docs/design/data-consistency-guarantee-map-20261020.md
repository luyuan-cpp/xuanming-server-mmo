# 数据一致性等级核对（2026-10-20）

> **2026-11-05 指向注**：§0~§5 核对的是 10-20 的架构（Redis 父快照 + Kafka 写回），那条存盘路径 10-30 已删除。现状、新发现和处置见 [§6](#6-2026-11-05-复查player_store-新路径)。

> 只读静态审计，**未编译、未运行**。对象是 2026-10-20 的工作区，包括其他会话尚未提交的改动。
> 本文不改代码，只回答两个问题：各类数据**实际**做到了哪一级一致性；有没有前两轮没登记的数据问题或覆盖回档问题。
> 前两轮：[第一轮](data-overwrite-rollback-audit-20261018.md)、[第二轮](data-overwrite-rollback-audit-r2-20261019.md)。第二轮的第三、四波由另一个会话续跑，本文不重复那部分，查法也不同：本文按数据种类核对保证等级，那边按未读代码段逐行补查。

## 0. 结论

**没有做到强一致。按目前的架构，玩家数据本来也不是强一致设计。** 实际做到的等级如下：

- **玩家存档：单写者 + 代际围栏 + 写回。**
  - scene 内存周期性写进 Redis 父快照（缺省 300s 一次，资产操作、退出、交接时强制存盘），再由 Kafka 异步落到 MySQL/TiDB。
  - 同一时刻只有一个合法写者：owner_epoch 校验与写入在同一段 Lua 里完成。go/db 用同一事务内 FOR UPDATE 的 (epoch, seq) 行围栏挡住旧版本。
  - 代价是有丢失窗口：scene 崩溃时，丢掉上次落盘以来的内存进度，最多约 300s。被拒的那一方就是丢数据的一方。
- **跨服务资产（帮会、邮件、交易、收件箱、战斗结算）：没有 2PC，用 outbox + 幂等账本 + 落盘证据，等级是 at-least-once-idempotent。**
  - 幂等账本与货币、背包在同一行 `player_database`，同一次提交。所以不会出现"资产在、领取标记丢了"这种中间态。
- **帮会、好友、排行分数、全球评分、号段、角色名：各自库内是强一致事务（strong-atomic）。** 跨到玩家侧的那一半仍是 at-least-once。

**缺省配置、没有故障时：** 不会旧数据盖新数据，也不会复制资产；崩溃只丢一段进度。

**最大的结构性前提是 zone Redis 不回退。**
- Redis 父快照是加载真源。帮会通道、收件箱、本地战斗结算还把"写进 Redis"当作落盘证据。
- 全仓只有路由发布用了 WAITAOF。Redis 实例崩溃（AOF everysec，约丢 1s）或主从切换（异步复制，丢毫秒级）时，SQL 行围栏挡不住"内容旧、版本新"的写。
- 本轮 6 条新 low 都出自这一个根因，见 §2.2。单看每条都是罕见故障，合起来是整个体系唯一没有兜底的前提。

**本轮新发现（复核后）：**
- 1 条 medium：战斗打满时限时结算可能整笔丢失，缺省配置可达。
- 15 条 low：Redis 回退一族 6 条，其他 9 条。
- 1 条被推翻。
- 前两轮仍开着的 medium 见 §4。

## 1. 各类数据的一致性等级

等级定义：
- **strong-atomic**：单一权威存储内原子提交。
- **single-writer-fenced**：单写者加代际围栏，有未落盘丢失窗口。
- **at-least-once-idempotent**：重投加幂等键。
- **eventual**：异步写回，有 RPO。
- **best-effort**：可能永久丢失或分叉。
- **none**：没有机制。

"复核"列是复核员逐条尝试推翻后的结论。

| 数据 | 真源 | 等级 | 丢失窗口 | 防旧盖新 / 防复制 | 复核 |
|---|---|---|---|---|---|
| 玩家父快照 `PlayerAllData:{id}` | zone Redis（scene 只从这里加载；login 父键在就走快路径，不读 SQL） | single-writer-fenced | scene 崩溃：≤ 周期存盘 300s（`redis.cpp:276`）。Redis 丢尾：≈1s / 毫秒级 | owner 等值 CAS + pending 单调，与 SET 在同一段 Lua（`redis_client.h:180-183`）。**不防 Redis 自身回退** | 成立 |
| SQL 玩家两表 `player_database` / `_1` | MySQL/TiDB（耐久副本，资产 SQL 证据的来源；不是加载真源） | at-least-once-idempotent，相对 Redis 最终一致 | Kafka 丢消息靠补投，约 31 分钟、8 次后转人工 | 同事务 FOR UPDATE 的 (epoch, seq) 行围栏（`player_row_fence.go:145-191`）。只拦"比库里旧的版本" | 成立 |
| 两表之间 | 各自事务，不原子 | 由屏障门控 | 部分落地时登录、交接拒绝放行，补投追平 | 屏障要求两个 ACK 都等于 pending（`parent_commit_barrier.go:43-47`） | 成立；跨字段不变量 ACK ≤ pending 没有脚本强制 |
| owner_epoch / location / handoff | zone Redis 唯一副本 | 存储层 strong-atomic（铸造与 SET location 同一段 Lua）；耐久性 best-effort | 普通 INCR，没有持久下限（`owner_epoch.go:150`） | Redis 不丢写时成立 | **弱于声明**：Redis 丢尾可重铸同值，见 §2.2 |
| 帮会资产通道、GM 收件箱、本地战斗结算销账 | 玩家侧以"写进 zone Redis"为 durable（`player_lifecycle.cpp:4352` 自述） | at-least-once-idempotent | Redis 丢尾时外部腿已终结、玩家侧回退 | 账本与资产同 blob | 成立（已登记 low） |
| 邮件领取、交易合同、商城发货 | 终结只认 go/db 在 SQL 提交后发布的证据 | at-least-once-idempotent | 审计员称"Redis 丢尾不再造成分歧" | — | **被推翻**：Redis 回退后，新 epoch 合法越过 SQL 行围栏，SQL 证据锚一样失效，见 §2.2 |
| 本地回合制战斗结算 | `battle:settlement:pending:{id}`（zone Redis）+ 玩家 `settlement_ledger` | 名义 at-least-once；实际 best-effort | 缺省可达的整笔丢失，见 §2.1 | 账本与资产同 blob | 成立 |
| 帮会资金、成员、贡献，好友关系，帮会历练结算 | 帮会库 / 好友库（MySQL/TiDB） | 库内 strong-atomic；跨到玩家侧 at-least-once-idempotent | 已提交 RPO = 0 | FOR UPDATE + CAS + 同事务闸门；Kafka 消费幂等闸门与发奖同一事务 | 成立。帮会贡献单列"strong-atomic"偏强，跨玩家侧同帮会资金 |
| 排行分数 / 封榜奖励（go/rank） | rank 独占 TiDB | 分数 at-least-once-idempotent；奖励 30 天后退化为 best-effort | 奖励失败无日志、无指标、无终态 | 分数、回执、游标同一事务 | 奖励部分弱于声明，见 §2.3 |
| 匹配评分 | 全球 SQL 权威：strong-atomic；MatchRedis 路径：best-effort | — | 非 dev/test 档且未开全球匹配时进程 panic，生产只走 SQL | — | 成立 |
| 组队状态、聊天历史 | SharedRedis / ChatRedis | best-effort（按设计，会话态） | Redis 丢失即消失 | — | 成立 |
| 永久 ID 号段、角色名登记 | 全局库 | strong-atomic | 0 | 行锁 + version CAS，提交后才发号；唯一键 | 成立 |
| 落点映射（缺省 `RouteStore.Mode=redis`）、账号角色目录 | Redis 唯一副本 | best-effort | 丢尾回退；整键丢失不可回源 | 读错时多半 fail-closed（stale_topic 进死信、登录被拒） | 成立（已登记 low） |
| zone 库与全局库之间（合服、撤销、整 zone 回档） | 各库各自为真源 | none | 回档只退 zone 库 | 回档执行工具未落地、`RollbackFence` 未接线，目前实际不可执行，等于 fail-closed | 成立（已登记 low） |
| 资产流水 `transaction_log`、assetop 日对账 | Kafka 审计 topic → data_service | best-effort | 发送失败只打 ERROR | 不带 save_seq；日对账只分类 PENDING 行，没有"已终结 outbox vs 已落盘账本"的对账 | 成立（已登记 low） |

**单写者保证**（交接域）：缺省配置下，双登录、换节点、跨 zone 旅行、scene_manager 多副本并发都不会出现两个合法写者。仍会失守的路径有四条：
- 600s forced 强删：设计登记的有意 fail-open，只丢数据，不会旧盖新。
- node_id 复用：同 epoch 双写，可能旧盖新，见 §2.3。
- etcd 单边分区：活节点被误判为死节点，见 §2.3。
- Redis 丢掉铸造：见 §2.2。

## 2. 新发现（复核后）

编号沿用工作流代理的命名，`-v#N` 是复核员补报的。

### 2.1 medium

| 编号 | 问题 | 位置 | 触发时序 | 可达性 |
|---|---|---|---|---|
| cross-service-assets#1 | **战斗打满时限时，scene reaper 在到期当刻删战斗锁，随后到达的合法结算被当成"已作废"，唯一一份 pending 被删掉。** 结果是奖励丢失、消耗品不扣 | reaper 到期判定没有宽限：`player_battle.cpp` reaper 主循环 `effectiveDeadlineMs < nowMs`（约 :2535）→ `ReleaseExpiredFightingFreeze`（约 :2439）→ `ClearBattleFreeze` 条件删锁（:757-762）→ 只补读一次 pending。丢弃分支：无 InBattleComp 且 GET 锁为 nil（约 :2089-2116）→ `AckSettlementPending` 规则 2（:547-550）→ `ClearPendingSettlementIfMatch`。battle 侧：`OnBattleDeadline` 用同一个 `deadline_ms`（`battle_room_manager.cpp:720-722`、`:1248`）→ `FinishBattle` 先 EVAL SET pending、回调里才投命令 | ① match 算出绝对 `deadline_ms`，battle 和 scene 用同一个值。② 到期时 battle 强制平局，异步 SET pending。③ scene reaper（每 30s 一拍）恰好在 battle 的 SET 之前执行：删锁、摘 InBattleComp、补读 pending 读空。④ 结算命令到达在线玩家：没有 InBattleComp、锁为 nil，判作"duplicate delivery or invalidated"；账本里没有这一局，当场销账删 pending。⑤ battle 发件箱探测到已销账，停止重投 | 缺省配置可达，不需要故障或运维操作。要求战斗打满 300s（挂机、长 Boss 战），且 reaper 拍点落在"battle 定时器延迟 + 收尾 + Redis 往返 + 两节点时钟偏差"这个毫秒级窗口里。单场概率约窗口 / 30s，0.01%~0.2% 量级；玩家无法控制拍点，构不成可利用的复制。代码注释（`HoldBattleLockUntilDurable` 一段）已经指出"锁不在 ≠ 已作废"，但只给"已应用待落盘"路径续了锁，没覆盖 reaper 到期删锁这条。全球确认模式（缺省关）同根因，落库重试时窗口更大 |

**修复方向**（任选其一，由修复会话按代码复核后定）：
- reaper 判 FIGHTING 作废改为 `deadline_ms + 宽限`。宽限至少要盖住 battle 收尾预算加时钟偏差上限。
- 或者：补读 pending 为空时先不删锁，保留到宽限期满。
- 或者：结算到达时把"锁不在"只当成"可能已作废"，回查 battle 侧这一局是否真被作废，再决定销账。

无论哪种，都要补一个用例：到期删锁先于 SET pending 时，结算最终仍被应用。

**2026-11-01 状态：已消除（未编译，待 Codex 验证）。** 结算改为只走 match SQL + player_inbox（`player-data-tidb-scaling-plan.md` §5.4），Redis 里没有 pending 可删：`ApplyInboxSettlement` 不读锁、不看冻结，reaper 判作废之后结算照常入账。reaper 也加了宽限（`FreezeExpiresAtMs`）。曾经的修复分支 `claude/recover-battle-settlement-f39089db` 不再合回，去向见 `turn-battle-gap-closure.md` §10。

### 2.2 low · Redis 回退一族（同一根因，建议合并处置）

**共同前提：** zone Redis 实例崩溃（AOF everysec 约丢 1s）、主从切换（异步复制丢毫秒级），或从备份恢复。

**共同机制：**
- 铸造 owner_epoch 用普通 INCR，没有持久下限。
- login 快路径只比对 Redis 内部的 pending 与 ACK，不和 SQL 行围栏比较（`parent_commit_barrier.go:36-49`、`ensure_player_all_data_async.go:84-87`）。
- go/db 行围栏遇到更大的 epoch，直接走 default 分支整行写入（`player_row_fence.go:178-188`）。
- ACK 写入只和自己的旧值比单调，不和 pending 比（`player_cache_publish.go:42-47`）。

| 编号 | 表现 | 后果 |
|---|---|---|
| player-save#1 | 丢尾截断点落在存盘 V_new 之后。主结局：go/db 随后把 SQL ACK 推到 V_new，高于回退后的 pending=V_old，login 永久报 `parent SQL barrier not committed`。V_old 的意图早已清掉，补投与 exhausted 告警都不会触发。次结局（ACK 也一起丢了）：快路径载入 V_old，新铸的 E+1 在第一次存盘时覆盖 SQL 里更新的 V_new | 永久拒登，或静默回档。在线玩家只要再存一次盘就能把 ACK 追平；挂机或已离线的玩家会卡死 |
| identity-routing-redis-v#2 | 丢尾窗口里退出的玩家：实体已销毁，SQL ACK 领先 pending，补投只重放更旧的版本，被判 writeOlder | 一次丢尾会命中这一秒内退出的所有玩家，永久锁登录，要人工跑 `save_barrier_restore` |
| cross-service-assets-v#1 | 上面的覆盖路径会让以 SQL 证据终结的邮件、交易合同、商城发货也出现"外部已终结、玩家侧回退"。推翻了"SQL 证据锚定的通道不受 Redis 丢尾影响"的说法；`db-write-fencing-mysql.md:1057-1059` 断言"dual 下 S1 保住"，只覆盖了计数器同步倒退的情形 | 领取、入账丢失；扣款回退等于复制 |
| player-save#2 / identity-routing-redis-v#1 | 丢掉的是铸造这一步（INCR 回退），新持有者缓存 E+1，下一次存盘被等值 CAS 拒绝，按废黜处理，不存盘就销毁（`player_lifecycle.cpp:4549-4612`） | 丢失"从载入到被拒"的全部进度，最长约一个周期存盘（300s），远大于所称的约 1s |
| identity-routing-redis#1 | 丢掉铸造之后，源 A 遗留的交接标记仍在 TTL 内，B 自己发起的第一跳跨节点换图被放行，并重铸出同一个 N+1 给 C；ReleasePlayer 发往 A，B 成为僵尸。B 的周期存盘按值相等放行，行围栏同 epoch 时比墙钟 seq，后写者胜。`ownerCounterRegressed` 只检测"小于"，零告警 | 同 epoch 双持有者，可能旧盖新。复制方向未完全核实，标 uncertain |

**合并修复方向**（建议先写一份设计，再落码）：
1. 铸 owner_epoch 时以 SQL `fence_owner_epoch` 为地板，或检测到 Redis `run_id` / `master_replid` 变化后先对账计数器。这一条能堵住重铸同值和"新 epoch 越过 SQL 新行"。
2. login 快路径在 Redis 身份变化后的首次加载改走冷路径，与 SQL 行围栏交叉校验。
3. 铸造与屏障 Lua 这类关键写加 WAITAOF（`numlocal=1`，HA 下 `numreplicas=1`）。有延迟代价，要按 AGENTS §6 压测后再定。
4. ACK > pending 时给出自动处置，或至少告警，不要静默永久拒登。

> **（2026-10-22 指向注）** 合并设计稿见 [zone-redis-regression-guard-20261022.md](zone-redis-regression-guard-20261022.md)，有两处修正：
> - 上面第 1 条说它「能堵住新 epoch 越过 SQL 新行」，不成立。任何铸造地板都只能防重铸同值；越过 SQL 新行要在加载侧堵（设计稿 G1）。
> - 第 3 条的 WAITAOF 只适用于有专用连接的 Go 侧铸造。C++ 存盘 Lua 走 hiredis 单连接，不能加。
>
> §0 里「全仓只有路由发布用了 WAITAOF」也要补一句：go/db 的重试、死信入队同样用了 WAITAOF。

### 2.3 low · 其他

| 编号 | 问题 | 位置 | 可达性 / 备注 |
|---|---|---|---|
| handover-single-writer#1 | location 只记 node_id，不记进程实例。同号新进程顶替后，`samePhysicalNode` / `samePlacement` 不铸造 epoch，存活的旧进程与新进程同持一个 E，存盘 CAS 拦不住，墙钟晚发号的一方胜出，可能旧盖新。旧进程还可能写出没有身份闸的改派标记，让新进程的进度在下一跳换图时作废 | `enterscenelogic.go:546-550`、`:687`、`:696-737`；`load_reporter.go:926-931`（同号注册即 `clearNodeDeath`）；`save_seq.h:59-66` | 要求进程暂停或 etcd 单边分区超过租约，加上同号被复用，加上玩家落到同号节点。设计 GO-3 只按活性问题登记；`cross-zone-scene-travel.md` §13.9 写"推迟没有数据风险"，与 `main.cpp:113-115` 注释和 M3/M13 矛盾。*2026-10-23：落地围栏线的 `handoff_fence` 拒绝同 E 不同实例登记，同代双持在当前树上不可达；GO-3 已按持有进程自己盖的资格章落码（接管时铸新 epoch，未编译），见 `cross-zone-scene-travel.md` §13.10。2026-11-11：证据栏里「同号注册即 `clearNodeDeath`」已不成立。断线重连 R15 起，watch PUT 改调 `clearNodeDeathForInstance`（`reentry_barrier.go`），只由领导者在判死的那一代进程回来时清 `death_at`；同号新进程注册时，老实例的再入屏障照常走完。老进程是否还持有这个号，由节点号持有者记录（缺口清单第 33 条，`node_holder.go`）告诉它。均未编译，见 `infra-reconnect-overview.md` §4.1 R15* |
| handover-single-writer-v#1 | etcd 单边分区时，活着的持有节点被判死。C++ 失租后只重注册，不冻结、不存盘、不疏散（`node.cpp:1711-1727`，注释写明不设围栏）。再入屏障"老进程 15s 内疏散存盘"的前提（`enterscenelogic.go:974-976`）落空，接管后丢掉一个本可以落盘的活节点的进度 | `node.cpp:1711-1727`；`etcd_service.cpp:762-772`；`enterscenelogic.go:968-1026`；`leave_release.go:109-110` | 分区要超过租约 TTL 加 20s。只丢不盖。*2026-11-05：新存档路径的判死释放原先只比代次，判死之后持有进程以同一 UUID 重注册、玩家同节点重连复用实体，释放照样执行、在线玩家被踢；已改为释放前 etcd 线性一致复核持有实例、按读到的 (E, H, claim_seq) 比较并写（未编译），残余窗口见 `player-save-direct-commit.md` §5.5。失租不冻结本身不变* |
| player-save#3 | 超过 kafka-go 缺省 `BatchBytes` 1MiB 的 DBTask 意图，补投每次都发布失败；不计次、永远到不了 exhausted、没有告警，还每 10~21s 把整个 zone 的补投暂停 10s | `saveoutbox/replay.go:305-311`；`publisher.go:30-41` | 这种意图本来在 go/db 的 1MiB 整行闸处就会被拒。危害只在于观测缺失、拖慢同 zone 其他玩家的恢复。是 r2-db#1 修复的副作用 |
| identity-routing-redis#2 | 本地战斗 pending 的 SET 落在 Redis 丢尾里，且命令被重启后的 scene 按代次丢弃（或锁已过期）时，重投判"记录缺失 = 已销账"，静默摘除 | `settlement_outbox.h:55-80` | 复核后收窄：玩家离线时，开局 scene 的离线分支会按锁把 pending 写回去，多数情况能自愈 |
| cross-service-assets#2 | 商城交付 poll 只取最早 20 条 PENDING。买家包满或长期离线的单子永远不终态，满 20 单后新付款的订单不再发货 | `DeliveryStore.java:34-52`；`mall.go:280-293`；`engine.go:764-770` | 功能缺省关闭；开启后相当于 medium 卡死，不丢钱。审计员说的"Mall.Tick 遇错整轮返回"已由 `c35e1133d` 修掉 |
| cross-service-assets#3 | GM 回档（单人 / 区 / 全服）整字段覆盖资产账本，写前只设帮会分歧闸，交易、邮件、战斗、收件箱的已终结外部腿不检查，与 `jubaozhai-market.md:327` 的说法不符 | `rollback_logic.go:752-800`；`:60-94` | `RollbackFence` 在仓库里从未赋值，回档入口一律 NotImplemented，属潜伏问题。**接线前必修** |
| social-global#1 | Redis 评分入账读评分失败时，静默按 1500 计算 Δ，并冻结进入账标记 | `go/match/internal/logic/rating.go:156-173`、`:616-624` | 复核后收窄：只在 dev/test 的 Redis 兼容路径；生产走 SQL 权威 |
| social-global#2 | rank 封榜奖励投递失败不打日志、无指标、无终态；邮件过期固定为封榜后 30 天，可恢复的拒绝拖过 30 天就永久丢奖 | `go/rank/internal/reward/worker.go:87-107`；`go/mail/internal/data/repo.go:172` | 仓内没有 Freeze 的调用方，只有外部 admin 手动带奖封榜才触发 |
| social-global-v#1 | `deploy/k8s/rank-alerts.yaml` 引用的指标全仓不存在（`go/rank/internal/metrics` 是空目录），rank 的告警永远不会触发 | `rank-alerts.yaml:50`、`:65`、`:135`、`:151` | 缺省部署即如此。只影响观测，但放大了上一条的"静默" |

### 2.4 被推翻

- **identity-routing-redis#3（加载时同包重复 guid 只记 ERROR 并跳过第二件）**：本服务自己的写入不可能产出同包重复 guid。铸造先查 `store_.Contains`，`PutInstance` 拒绝已存在的 guid，ItemStore 以 guid 为键。只有外部篡改或导入的存档才会触发。作为加固建议保留：改成 fail-closed 拒绝加载（`bag_system.cpp:1394-1400`）。

## 3. 设计文档与代码不符（需更正）

- `db-write-fencing-mysql.md:1057-1059`：称 "dual 下 S1 保住"，只覆盖了计数器同步回退。计数器没回退、只丢存盘时不成立（§2.2）。
- `guild-actor-architecture.md`：actor 加定期落库的方案只是 06-04 的草案，代码是直写 MySQL 事务。
- `cross-zone-scene-travel.md` §13.9：称 GO-3 "推迟没有数据风险"，与 `main.cpp:113-115` 注释、M3/M13 矛盾（§2.3 handover-single-writer#1）。*（2026-10-23 已在 §13.9 加批注更正，`main.cpp` 注释同步改写，见 §13.10）*
- `enterscenelogic.go:974-976` 注释：再入屏障依赖"老进程失租后 15s 内疏散存盘"，当前 C++ 失租不疏散。
- assetop 设计把 durable 说成"盖了入库章"，`player_lifecycle.cpp:4352` 自述只证明已进 Redis。
- `jubaozhai-market.md:327`：称回档排除有合法交易流水的转移，代码只有帮会闸（§2.3 cross-service-assets#3）。

## 4. 当前仍开着的问题（含前两轮，截至本文）

| 类别 | 内容 |
|---|---|
| medium，待修 | ~~本文 cross-service-assets#1（缺省可达）~~（2026-11-01 已消除，见 §2.1）；r2-battle#1（全球匹配门控）；r2-mz#1（copy 合服门控）；r2-ps#1（仅混版，严重度待第二轮第四波裁决）；第一轮 §2.5 三条（全球匹配、交易资产通道门控） |
| 已落码、未验证 | 第一轮 §2.1–§2.4，r2-gate#1、r2-sm#1/2/3、r2-db#1/3，混版字段保护第 2、3a 批。都在等 Codex 编译、测试与故障演练，见第一轮 §7 与第二轮交接 §8.1 |
| 本轮新 low | §2.2 的 6 条（建议合并成一份设计），§2.3 的 9 条 |
| 未覆盖 | 第二轮第三、四波（另一会话在做）；Java 版同类审计；运行期一致性压测（[data-consistency-stress-testing.md](data-consistency-stress-testing.md) L2–L4）。静态审计不能代替这些运行证据 |

## 5. 方法与证据

- 工作流 `wf_491f0544-5ec`：5 个域，每域一名梳理员加一名复核员（复核员按"默认怀疑"逐条推翻）。
  - 第一次运行时 5 名复核员撞上 5 小时额度，全部失败；用 `resumeFromRunId` 续跑，梳理员命中缓存。
  - 子代理 token 合计约 358 万（236.6 万 + 121.4 万）。
- 5 个域：玩家主存档、交接与单写者、跨服务资产、社交与全局数据、身份路由与 Redis 唯一副本。
- 只读核对：代理转录里没有 Edit / Write 调用，也没有把 `git show` 重定向成文件。
- 主会话抽查过的结论：
  - §2.1 的到期判定没有宽限，battle 与 scene 用同一个 `deadline_ms`。
  - §2.2 的四处共同机制：快路径只比 Redis 内部屏障、普通 INCR 铸造、行围栏遇更大 epoch 走 default 写入、ACK 只与自己的旧值比单调。
- 代理完整返回在本会话转录的 `journal.jsonl`。
- **Java 版**：未审，不在本文范围。§2.1、§2.2 两类问题修复时，按 AGENTS §12 只读核对 Java 版是否同构，再决定是否登记 `PARITY.md`。

## 6. 2026-11-05 复查:player_store 新路径

> 四路只读审计加本会话的修复,**未编译、未运行**,对象是 2026-11-05 的工作区。10-30 起玩家存档只在 TiDB 的 `player_store`(go/db 的 `LoadAndClaim` / `Save`),§0~§5 说的 Redis 父快照、Kafka 写回、saveoutbox 都已删除,那几节只作历史保留。

### 6.1 结论

**旧数据盖新数据**:玩家存档这条线在代码层面挡住了。每笔存盘和认领都在一个 TiDB 事务里先 `FOR UPDATE` 锁写权行,再按(代次、持有实例、序号)判定(`go/db/internal/playerstore/decide.go`)。旧持有者、迟到请求、重放只会被拒或幂等成功。四路审计都没找到旧盖新或一物两得的时序。

**强一致**:分数据回答。

| 数据 | 实际等级 | 说明 |
|---|---|---|
| 单个玩家已提交的存档 | 强一致:单写者 + SQL 写权行围栏,提交即落 TiDB 多副本 | 认领和读全部行在同一个事务;scene 每人只有一笔在途,结果未知时同内容、同序号重试;序号按库里的值播种,不会出现同序号不同内容被当成幂等成功 |
| 玩家内存里的进度 | 写回,有丢失窗口 | 定时存盘 60 秒,任务和等级 1 秒内存,资产、结算、收件箱、下线、交接立即存。scene 崩溃丢最后一次提交之后的进度,玩家看到的就是回到最后一次存盘 |
| 跨服务资产(帮会、交易、邮件、收件箱、战斗结算、GM 回收) | 至少一次 + 幂等账本,最终一致 | 账本和资产在同一行、同一次提交;外部腿只认已提交账本才终结(`go/shared/assetop/reconcile.go`) |
| 各服务库内的数据(帮会、好友、榜单、匹配、队伍、邮件、发号器) | 库内事务强一致 | 行锁、CAS、唯一键;消费者的幂等闸门和业务写在同一个事务 |
| 跨库操作(建角跨四个库、过户、合服别名) | 分步提交 + 补偿和对账 | 正常运行能收敛;多集群 PITR 之后见 6.2 第 5 条 |
| 资产流水(审计,不是权威数据) | 改前早于提交、发送失败只打日志;改后提交之后才发 | 见 6.3 第 1 条 |

### 6.2 发现与处置

| # | 严重度 | 问题 | 处置 |
|---|---|---|---|
| 1 | medium | match、guild 的对局结果消费者遇到一次失败或一次在途再均衡,就永久停止取消息,不重建(`go/shared/kafkautil/group_lag.go` 出错即 `StopFetching`,`Hold()` 只进排空证明);match 评分消费组没有 lag 告警。topic 保留 7 天,停超过 7 天永久丢评分和榜单更新。即第二轮的 r2-kc#1 | 「数据覆写审计 R2 交接」会话 11-06 已修:消费者跨代次存活,只有控制面真的维护排空才停取;告警改按分区已提交位点停滞判。去向与验证见 [第二轮审计](data-overwrite-rollback-audit-r2-20261019.md) §5.0、§6 |
| 2 | medium(要运维操作) | trade、guild 的人工终结工具 `cmd/assetopfix` 让值班凭「流水里有这条 correlation_id」判 applied,流水却是内存改动当下发的、早于存盘提交,发送失败也只打日志;工具不读已提交账本,`-force` 能终结任何超过 30 分钟的待定行。判错就一物两得,或卖家物品凭空消失 | 根治:流水改成提交后才发(6.3 第 1 条)。共享判据 `go/shared/assetop/manual_evidence.go`(`JudgeManual`、`RowQuiet`、`QuietWitness`)和 trade 工具已改(6.3 第 4 条);guild 工具 11-06 已改调同一份判据(未编译) |
| 3 | medium(要运维操作) | GM 回收按流水汇总金额、只按 tx_id 去重:同一指令崩溃后重投再应用,会多出一条同 correlation_id 的流水,金额算两遍,扣不够的挂成永不过期的欠款。`exploit_loss_prevention_zh.md` 把窗口写成「单次存盘间隔」,确定性拒存时其实是整段会话 | 根治同第 2 条;`recall_plan.go` 以已提交账本为证据、按 correlation_id 去重和文档更正由 R2 交接会话 11-06 修完(第一轮 §4 第 7 条下游);`ResolveRecallOp` 人工终结 11-06 改为查已提交账本(6.3 第 4 条) |
| 4 | medium(只丢不盖) | 节点和 etcd 单边失联时不冻结、不疏散,租约到期再过再入屏障之后被判死;之后正常换图或重登就会铸新代次派到别的节点,老节点的存盘被拒,丢最后一次存盘之后的进度,最多 60 秒。即 §2.3 的 handover-single-writer-v#1,新增的事实是正常换图也会触发;`reentry_barrier.go` 和 `enterscenelogic.go` 里「失租后 15 秒紧急疏散」的注释与代码不符 | 本会话加身份不确认期间每 5 秒补存(6.3 第 2 条),丢失上限压到约 5 秒;更正 `reentry_barrier.go` 注释。`enterscenelogic.go` 那句 R2 交接会话 11-05 已改为「失租不疏散,15 秒疏散只在身份冲突时」 |
| 5 | medium(只在灾难恢复时) | 一个地域的逻辑库分在多个集群时,PITR 各集群恢复到同一个 T 却没有统一时间点,贴着 T 的建角留下半截:角色不在列表里、名字能被别人再注册、认领回 NO_OWNER 永远进不了场。手册 §4.1a 的核账表只列了过户和删角 | R2 交接会话 11-06 已修:建角残留核账与修复工具,同日评审修正(先查已提交证据、起服之前核账);见第二轮审计 §6 |
| 6 | low(只本地) | 本地 `scene_manager_service.yaml` 开着 `AllowUnsafeCrossNodeHandoff`。旁路放行不铸新代次,目标节点拿同一个代次认领,go/db 回 NOT_HOLDER,同 zone 跨节点换图卡住 | 本会话改成 false,和生产一致 |
| 7 | low | 路由失败回滚时,kafka 报错但消息其实已投递,目标节点又抢在回滚之前完成登记和认领,就成了「没有 location 的现任」;下一次换图或重登按首次落点铸新代次,丢这之前最后一次提交之后的进度。只发生在不凭交接标记的铸造上(首次落点、第二条腿、死节点接管),凭标记的回滚早已靠「标记还在」互斥 | 11-06 本会话已改(6.3 第 5 条),Codex 已跑通(见 6.4) |
| 8 | low | 判死释放(`ReleaseDeadPlayerStoreHolder`)只比代次,交易判死比(代次、持有者、认领序号)三列。误判死与同落点重连认领凑在一次释放 RPC 的窗口里时,认领之后的进度丢 | R2 交接会话 11-06 已修:判死释放比(代次、持有者、认领序号)三列并复核注册表(handover-single-writer-v#1 的 scene_manager 侧);见第二轮审计 §6 |
| 9 | low | rank 封榜奖励被邮件服务拒收时永远重试、没有终态;30 天后邮件一律按过期拒收 | R2 交接会话 11-06 已修(r2-mail#2):发件取 min(封榜 +30 天, now + mail 当前 TTL),收件箱满与超出可投窗口设终态并计指标。目前没有生产调用方触发封榜发奖 |
| 10 | low | `ReleasePlayerName` 非管理员路径不核调用方,只限登记年龄窗口;出 bug 或被越权调用时一名两角 | 2026-11-06 已落码,未编译,待 Codex 验证。名字行改成「预留 → 绑定」状态机(`player_name.bound_ms`):建角提交之后复核角色再绑定;已绑定的 `ReleasePlayerName` 一律拒绝,只有删角写完墓碑之后的收尾能释放;预留态释放之前在玩家片证明角色永远不会提交(必要时写墓碑);不带 token 时只认 login 的签名。核账工具加 `name_unbound`。设计与残余见 [名字设计](guild-phase2/03-names.md) §3.4a |

核实成立的保证(各路复核过):写权判定先判代次、再判持有者、最后才比序号;认领在同一个事务里抬代次再读全部行;交接标记只在最终存盘提交、且没有排队快照之后才写,写了标记就不再存盘;判死释放用 location 记的那一代,库里更新就不动;发号器两套 etcd 都写成功才发号;外部腿只认已提交账本才终结,自动放弃的「已释放」和「账本未见」出自同一次读、按租约令牌提交;写者闸门冻结之后旧位置的写提交不了;搬库翻转结局未知时绝不解冻;过户、删角之后旧会话写不进存档。

### 6.3 本轮改动(验证状态见 6.4)

1. **资产流水跟着存盘提交走。**
   - `TransactionLogSystem`(`cpp/libs/modules/transaction_log/transaction_log_system.*`)开闸之后,只涉及一名玩家的流水按玩家排队,带进程内单调的水位。
   - `PlayerStoreClient` 新增可选的提交水位钩子(`Options::commitMark`、`onCommittedThrough`):收快照时取水位;这一笔 COMMITTED、EQUAL,或快照和已确认的内容一样不用写时,按这份快照的水位放行。被拒、结果未知、被取代都不放行。
   - `MakePlayerStoreOptions` 开闸并接上钩子;实体没了且存盘落定(`onSettled`、`DestroyPlayer`),或本节点重新认领之前(`onClaimed`),丢掉还没放行的。
   - 效果:「流水里有这条」等于「这笔改动已提交」,人工终结工具和 GM 回收不会再拿未提交的改动当证据。反过来不成立:提交之后发送失败、提交成功但回包丢失时进程崩溃,已提交的改动可能没有流水,判「从未应用」仍要读已提交账本。
   - 每名玩家最多排 4096 条,超了丢最旧的一条并打 ERROR(`metric=txlog_pending_overflow`):宁可缺一条,也不发一条没落库的。流水晚于改动发出,最多晚一个存盘周期(60 秒)。
   - 只顺带的行(位置)被推迟时不进状态机,不会触发放行;投影启动校验只许这类行放 `transform`。
2. **身份不确认期间补存。** `player_lifecycle.cpp` 的 `RunIdentityAtRiskSaves`:节点入口注入身份探针时挂一个 1 秒的定时器;身份不确认(keepalive 应答超过半个租约没来、重注册中、紧急疏散中)时,每 5 秒给所有可改的玩家 `SavePlayer` 一次(按玩家号分摊到每秒,和定时存盘同一个做法,不在一个回调里存全员),位置也一起写;身份恢复后回到定时存盘的节奏。这是缩短窗口,不是围栏:最后一次补存到接管之间的改动照样会丢。零丢失只能在失租时冻结或疏散全部玩家,而 2026-09-08 起明确定了「失租只重注册、不关停」(`node.h` 的 `NodeIdConflictReason`):etcd 整体抖动或故障恢复时所有节点会同时冻结或疏散,代价远大于单边失联时最多丢的 5 秒。这是可用性与丢失窗口的取舍,不是待修项;要改需负责人决定,可选做法见 6.4。
3. **本地换图旁路关掉**:`go/scene_manager/etc/scene_manager_service.yaml` 的 `AllowUnsafeCrossNodeHandoff` 改成 false;`reentry_barrier.go` 的时间线注释按实际行为改写;`robot/etc/travel_smoke.yaml` 的说明同步。
4. **人工终结以已提交账本为证据,trade 与 guild 一份判据(11-06 统一)。** `go/shared/assetop/manual_evidence.go` 的 `JudgeManual`:位图分类与自动终结同一套(`ClassifySeq`);有结局时还要收据是这一条请求(`CommittedOutcomeOf`,与重投循环终结同口径);部分发放兼看账本标记和行上粘性的 `last_reason`;判「从未应用」要求写权行已释放、账本没见过、而且行安静(`RowQuiet`:此刻没人持租约,距最后一次可能的投递超过 `ManualQuietGap`,即 30 分钟与租约取大,`last_send_ms` 为 0 按不安静)。账本判不了一律不终结:没流水不等于没应用,也还没有能从提交后的流水还原整笔发放的证明。`go/trade/cmd/assetopfix`:`list` 与 `resolve` 都读账本,终结走 `AssetOpRepo.ResolveManuallyIfQuiet`(条件写带读证据时的 `lease_until_ms`、`last_send_ms`,中间被领过就落空,退出码 3);`OpFromRecord` 抽成重投循环与工具共用的还原函数。guild 工具 11-06 改调同一份判据(请求还原不出按 none)。GM 回收的 `ResolveRecallOp` 也用这份判据读已提交账本,按回收自己的结局对表放行(判不了只收 `PARTIAL`:不放回来源,只会少扣),终结在 settle 里带同样的见证,详见 `exploit_loss_prevention_zh.md`「人工终结」。手册 `trade-commerce-quarantine-runbook.md` §4.1、§4.2 同步。
5. **不凭标记的路由回滚先看交接资格原件(11-06)。** 目标节点入场时先跑 C++ `kRegisterFenceLua`(核对 owner_epoch == 铸出值,写出这一代的资格原件 `player:{id}:handoff_fence`),登记成功之后才认领存档;原件代次只升不降。`go/scene_manager/internal/logic/owner_epoch.go` 的 `luaRollbackPlayerPlacement` 对不凭标记的 bump 在同一次原子执行里读这份原件:已属于铸出值 → 一个字节不动、保留落点(`target_registered`,目标节点就是有 location 的现任);缺失或更早 → 照常 INCR 并退回 location,之后迟到的登记被 owner_epoch 拒掉,目标节点认领不了;比铸出值新或写坏 → 不回滚并告警(`fence_not_proof`,并入 `SceneManagerEnterSceneRollbackRedisError`)。判据与无资格持有者撤销(`location-release-binding.md` §17)同口径。不加 RPC,也不碰登录热路径:只在推路由失败的回滚里多读一个同 slot 的键。设计文档 `cross-zone-scene-travel.md` §13.7「设计残余」第一条、手册 `cross-zone-failure-test-runbook.md` §2.3 / §2.4 同步。

### 6.4 验证状态与没覆盖的(11-10 核对)

6.3 的改动都已随保存提交进 main(`a8af9372d` 起)。Codex 的运行证据按条:

| 条 | 已有的运行证据 | 出处 |
|---|---|---|
| 1 流水跟着提交走 | 11-08 主仓整解(`game.sln` Debug / x64,exit 0)之后,`currency_test` 的 `TransactionLogCommitGateTest.*` 7 条、`bag_test` 新增的 4 条提交水位用例全部 PASS | PROGRESS 11-08 Codex「命名审查阶段复验完成」,`build/verification/naming-audit-20261108/cpp-tests/` |
| 2 身份不确认期间补存 | 11-08 整解编译通过。11-10 补了 3 条单测(`cross_zone_test` 的 `PlayerStoreDirectEcs.UnconfirmedIdentitySavesEachPlayerOnceEveryFiveSeconds` 等),补存入口改为 `PlayerLifecycleSystem::RunIdentityAtRiskSaves`,**未编译,待 Codex** | PROGRESS 11-10 Claude「身份不确认期间补存补单测」 |
| 3 本地换图旁路关掉 | 只改配置与注释,没有运行证据 | — |
| 4 人工终结统一判据 | `go/shared` assetop 144 PASS(9 条真 MySQL 门控 SKIP);trade 的 assetopfix、data、reconcile、svc 共 111 PASS | PROGRESS 11-06 Codex「数据一致性验证续跑」「修正 scene_manager 五项旧夹具」 |
| 5 回滚先看资格原件 | `go/scene_manager` 的 `./internal/logic/...` 全量 756 PASS;回滚 Lua 在真 Redis 7.2.16 与 8.10.2 上 16/16(两种先后、6 类资格原件,返回 4 / 5 时 6 个键逐字节不变) | 同上两条 |

还没有运行证据的:
- 流水「提交之后才出现」的运行期核对,本地跨节点换图冒烟,etcd 单边失联演练,真实 SQL 条件写竞态,Kafka「报错但已投递」注入,告警规则的 promtool 校验。
- 杀进程、TiDB 切主、etcd 分区这类故障注入从没跑过,压测也没跑:「崩溃时不回档」除单测外只有读代码的证据。

待负责人决定的取舍(第 2 条的剩余窗口):节点和 etcd 单边失联时,接管之前最多丢一个补存间隔(5 秒)。要做到零丢失,有两条路,都要改 2026-09-08「失租只重注册、不关停」的定论:
- **节点自己退位**:失租后在宽限期内重注册不成,就走现成的冲突关停(`Node::OnNodeIdConflictShutdown` → `BeginEmergencyRelocateAll`,先存盘、存盘落地再改派)。代价:etcd 整体抖动或故障时,所有节点同时疏散退出。
- **控制面通知退位**:scene_manager 判死时(注册表线性读复核之后),用现成的 `SceneCommand` Kafka 通道按最后已知实例号发「退位」。节点收到后,宽限期内重注册不成才走冲突关停,宽限期加疏散预算(15 秒)要落在再入屏障(20 秒)之内。etcd 整体恢复时,各节点会在宽限期内重注册成功,收到的退位一律作废,不会集体疏散。代价是多一条跨语言协议和它的测试。
- 不改的话,剩余窗口就是这 5 秒,只在单边失联、又恰好有接管触发时出现;写权行保证只丢不盖。
- Java 版:只读核对后,只有流水早于提交一项适用,已在 Java `PARITY.md` 登记待做;人工终结由 R2 交接会话登记;补存与回滚两项 Java 已有等价做法。独立的 xuanming-server 仓不在这次范围内。
- 四路都没读到的:match 的 direct_admission、challenge、spectator、global_pool_matcher;C++ 回合引擎;trade 的 rmb_mall、role_authority;Java `CommerceStore` 全文;tools/dbmove 的 replication、checksum。

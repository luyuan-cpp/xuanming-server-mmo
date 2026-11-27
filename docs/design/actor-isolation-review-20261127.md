# Actor 隔离改造（2026-11-27 Gemini 批次）审阅结论

> 审阅与修复：Claude，2026-11-27。负责人同日定「按推荐改」，其余待定项「按最标准的做法当场定案」。本文所列修复都已落码，**未编译、未运行**，验证命令见 §7。
> 依据：Gemini 交接单《全工作区 5 仓库 Actor / 消息隔离改造交付与审阅报告》与其审计基线（两份都只在 Gemini 本机目录，不在仓库里，本文自成一体）。
> 规则口径：本仓 [engineering-principles.md](../rules/engineering-principles.md#1-模块与职责111) 两层边界、[ecs-component-access-rules.md](ecs-component-access-rules.md)，Pandora 仓 `CLAUDE.md` 第 6 条「系统 / 模块间消息通信」。

## 1. 审阅范围

五仓改动已于 11-27 10:14–10:15 由负责人账号分别提交进各仓 main，内容与交接单所列文件一致：

| 仓库 | 提交 | 内容 |
|---|---|---|
| `xuanming-server`（Pandora Go） | `7f4514f7` | push 广播值隔离、PresenceHub 改单 goroutine 邮箱、ds/hub allocator 旁路协程改 `safego.Go` 并加在途上限、mail 换 `plog.Detach`、auction/team/playerdisplay/killswitch 补 panic 兜底 |
| `xuanming-server-mmo-cpp`（本仓） | `5fe504e65` | Kafka 命令回调 `[&node]` 改按值捕获；Buff/技能表达式改走快照 `Evaluate*`；配方缓存 `static` 改 `thread_local` |
| `xuanming-server-mmo-python` | `45fe6c7`（并行会话顺带提交）、`0811a1f` | 宗师代制回账加锁 / 锁忙转投宗师串行任务；gate 广播逐会话副本；LocalLogin / LocalServiceHub 改走公开接口 |
| `xuanming-server-mmo`（Java） | `87f79f19` | 跨线程回调改捕获 `(playerId, instanceId, ownerEpoch, freezeId)` 并回逻辑线程重查；`playerById` / `step` 加逻辑线程断言 |
| `mmorpg-client`（Unity） | `5bb30c91` | `UiDomainEvents` 取代 World→UI 反向调用与 `BattleUiRoot` 静态委托；三个 Client 补 `Dispose`；快照集合冻结；`RankedUiRoot` 补注销 |

总体判断：两层边界的口径认同；Gemini 的具体落地里有两处回归（§2）、一处结论与它自己写的规则矛盾（§5），另有几处「合规但不是在修 bug」的改动（原写法在单线程或只读前提下本来就安全），保留不动。

## 2. 本批引入的回归（Pandora Go，已改）

### 2.1 PresenceHub 邮箱会静默丢订阅与状态变更 → 已回退为加锁版本

- **问题**：`7f4514f7` 让消费邮箱的 goroutine 同时做推送 I/O。生产推送器 `cmd/locator/main.go` 的 `presencePusher` 走 `kafkax.KeyOrderedProducer.Send`，底层是 Sarama `SyncProducer.SendMessage`（`pkg/kafkax/producer.go:128`），逐条同步等确认、不看 ctx。一轮 flush 推几千个订阅者、或 Kafka 变慢时，邮箱停止消费，`Subscribe` / `Unsubscribe` / `Notify` 堆满 4096 条后直接丢，只有每 100 条一行日志。
- **后果**：丢 `Subscribe` → 好友面板收不到在线状态；丢 `Unsubscribe` → 关注关系残留，玩家下线后无人清；丢 `Notify` → 状态停在旧值，且 `lastSent` 基线跟着错，之后真实的一次变化可能被判成「无净变化」吞掉。交接单「满了记指标」实际只有日志；「`Close()` 立即打断在途推送」在生产不成立（同步生产者不看 ctx，新单测能过只因测试桩会看 ctx）。
- **修复**：`presence.go`、`presence_test.go` 回到 `7f4514f7^` 的加锁版本（入口只在纯内存的结算段与 tick 争一把锁，推送在锁外做，入口从不被 I/O 拖住），并在类型注释写明不改成邮箱的原因。没有保留「未 `Start` 直接 `Close` 不挂死」：生产只有 `Start` 后 `defer Close` 一条路径。

### 2.2 ds / hub allocator 的在途上限会丢位置续期 → 已删上限

- **问题**：`refreshBattleLocations` 每一跳 DS 心跳都调，在途数 ≈ 心跳速率 × locator 时延。按 3 秒超时算，locator 一变慢，每秒约 21 次续期就打满 64，之后续期全被丢；`killStrandedDS` 与它共用同一个计数（本地模式 `killOrphanOnStop` 打开时回收孤儿 DS 也会被挤掉）。原写法每个协程本来就有 3 / 5 秒超时，协程数天然有界。
- **后果**：locator 抖动期间 BATTLE 位置 TTL 可能过期，玩家中途掉线重登找不到原 DS（Pandora 规则第 19、20 条「不能让玩家进不去」）。
- **修复**：`ds_allocator/internal/biz/allocator.go`、`hub_allocator/internal/biz/hub.go` 删掉计数、常量与满即丢分支，保留「先在请求 goroutine 同步 `plog.Detach` 再 `safego.Go`」与原超时；相对 `7f4514f7^` 的净变化只剩这一点。hub 那处原先把常量插在函数注释和函数之间，顺带恢复了注释归属。

## 3. 其余发现的处置

| 仓 | 位置 | 发现 | 处置 |
|---|---|---|---|
| Go | `services/economy/auction/internal/biz/auction.go` | 事件驱动的审计 worker 用了 `safego.Run`，它每轮成功刷新 `pandora_loop_last_round_unix{name="auction_audit_worker"}`；没有审计事件时时间戳不动，将来加统一停摆告警会误报 | **已改**：改成闭包内 `defer safego.Recover(...)` |
| Go | `services/runtime/push/internal/biz/connection.go` | 广播先拷一次 payload，再给每条流各拷一次（N+1 份）；原共享帧构造后无人改，protobuf-go 只读并发 Marshal 安全 | **已改**：入口拷一次后各流共享只读 payload，每条流仍各自构造 `*PushFrame`；单测改为断言「调用方事后改原帧不串改各流」「各流帧指针不同」。`consumer.go` 对 `msg.Value` 的拷贝保留：它让广播帧不钉住 Sarama 整块拉取缓冲 |
| Go | `pkg/playerdisplay/projection.go` | worker panic 被兜住后，该分块结果按「成功但为空」处理，不进 `Failures`，同一 worker 余下分块也不再处理 | **已改**：按分块兜底，panic 的分块记 `errResolverPanicked` 进 `Failures`，worker 继续处理后续分块；补单测 `TestResolveReportsPanickingDependencyAsFailure` |
| C++ | `skill.cpp`、`modifier_buff_impl.cpp`、`buff_impl.cpp` | 语义已核对等价：`bonusDamageExpr` 从未 `Init`（零个参数），空参数求值正确；回血两参、伤害一参都对得上。代价是每次求值新建 exprtk 解析器与符号表（旧路径复用成员解析器，但同样每次编译） | **定案不改**：无状态 `const` 求值是快照共享下唯一不碰共享可变状态的写法；这些表达式目前只在场景技能与 Buff 上用，频率低。将来做实时大世界战斗、这条路径成为热点时，再按「每线程按表达式文本缓存编译结果」优化，不回退到共享可变参数 |
| C++ | `player_equipment_v1.cpp` | `static` 改 `thread_local` 在只有主线程的节点上行为不变 | **定案不改**：它是配表派生的只读缓存，按表加载版本与快照地址失效，不属于需要 `Node` 持有的有状态组件 |
| C++ | `node_kafka_command_handler.h` | 已核对：节点号每进程只选一次（失租重注册沿用同一键，失败即隔离）、`node_uuid` 只生成一次、`maintenancePrepareBoot` 只赋值一次，且都在注册 Kafka 之前；分区本来就按注册时的节点号算 | 等价，无需处置 |
| Python | `nodes/scene_skill_craft.py` | 锁忙时记下的待回账在进程内存字典里，不是交接单说的「持久化」：优雅停机时 `_closing` 让 `mutable()` 返回假，排空任务空转；宗师换到别的 scene 进程也带不走。根因更大：C++（r2-craft#4，`player_craft.cpp` 按区 Redis 哈希台账 + Lua 原子取走 + 失败归还）与 Java 都把摊位目录、口碑待回账、24h 热榜放在 Redis，Python 移植三者都只在本进程内存。只影响口碑与营业战报计数，手工费走买家同事务出箱，不受影响 | **Python 移植缺口，另开会话按 C++ 设计补齐**（见 §6）；「锁空闲当场拿宗师锁 / 锁忙投宗师串行任务」两条路并存的写法一并收成「一律记台账、由宗师自己的串行任务取走入账」 |
| Python | `nodes/gate.py:1488` | 广播改为先序列化、再逐会话反序列化副本；会话写出循环只序列化不改消息，原先共享对象是安全的，单线程 gate 每次世界广播多 N 次解析 | **定案保留**：Python 规则要求不跨会话共享可变 protobuf；要省这笔开销需改输出队列契约（入队预序列化字节），牵动大量用例，Python 版不是性能基准，不改 |
| Java | `world/SceneWorld.java` | `playerById` / `step` 越线即抛异常；53 处调用逐处核过都先回逻辑线程，但 `SceneWorld` 默认守卫恒放行，现有用例都没在守卫下跑过 | **Java 待做**，已登记 Java 仓 `PARITY.md` |
| Java | `battle/PlayerBattleService.java` | 实例号比对与原引用比对等价；`ScenePlayer.ownerEpoch` 是 `final`，多出的代次比对冗余无害。只有「Inbox 已落盘 → 清迟到确认锁」一支收窄：玩家已换实例时不再清，靠下次进场恢复读兜底 | **Java 待做**，已登记 Java 仓 `PARITY.md` |
| Unity | `Game/Ranked/RankedModels.cs`、`Game/Tournament/TournamentModels.cs` | `FreezeList` 遇到 `ReadOnlyCollection<T>` 直接复用，但它只是只读包装，可能包着会变的 `List` | **已改**：一律拷贝（列表很小，只在收包与派生时构造），删掉随之无用的 `using` |
| Unity | `Game/GameClient.cs` | 释放顺序变成先断线再释放子 Client；技能界面已先解绑 | 不改 |
| Unity | `Game/UiDomainEvents.cs` | 仍是静态状态，只是挪进带归属校验的静态类；WebGL 单线程下可接受。`NavigationRejected` 生产代码无订阅者，「目标不可到达」不会重复提示 | 不改 |

## 4. 交接单 7 个核查点的答复

1. **PresenceHub 背压**：问题比「订阅与状态要不要分开背压」更根本，已回退，见 §2.1。
2. **Python 锁忙回退时序**：不会重复消费——所有排空都在宗师自己的锁内（客户端请求分发按玩家加锁、投递任务也先拿锁），且是「读快照 → 提交 → 扣减已排空量」。会残留：停机与换进程时丢失，按 §3 移植补齐。
3. **Java `AfkPassService` 未绑定查询函数**：生产只在 `SceneNode` 构造并经 `withAfkPassService` 绑定，未绑定分支只在单测出现。可接受。
4. **Java 七服务共锁 `player` 表等**：同意留到下一阶段，涉及跨服务表结构与协议，需单独设计。
5. **Unity 快照元素可变性**：接受「集合冻结 + 收包与派生时克隆 + UI 约定只读」的折中；`FreezeList` 短路已去掉。
6. **C++ 表达式热路径**：已定案不改，见 §3。
7. **C++ `player_battle.cpp` 多组件 `Swap`**：交接单说「符合第二层」与新规则字面冲突，已按 §5 定案。

## 5. 定案与判据（写进规则，防止下一个 AI 来回改）

- **写时复制两段式提交是「禁止 Swap 他系统组件」的唯一例外**：`player_battle.cpp` 战后结算里，赛季、试炼志、瑞兽、通行证、江湖等副本都由各所属 System 的 `PrepareBattleCredit` / `PrepareBattleSettlement` 在副本上算出并校验，编排方只读原组件，全部准备成功后在同一同步调用里一次性 `Swap`。这正是「状态私有 + 窄接口」：计算逻辑在所属系统，编排方只负责同成同败。给每个 `Swap` 再包一层 `Commit*` 函数不增加任何约束，不做。已写进 [engineering-principles.md](../rules/engineering-principles.md#1-模块与职责111) 与 [ecs-component-access-rules.md](ecs-component-access-rules.md)。
- **模块内加锁不等于违反 Actor 边界**：状态不出模块、外部只经方法调用、锁内不做 I/O，就是「状态私有」；不必改成单 goroutine 邮箱。确要改成邮箱，邮箱 goroutine 不得做阻塞 I/O，订阅 / 退订这类控制命令不得因队满丢弃。已写进 Pandora `CLAUDE.md` 第 6 条。
- **fire-and-forget 协程靠各自的超时保证数量有界**，不另加满即丢的在途上限；满即丢会把「下游慢」变成「静默丢续期」。已写进 Pandora `CLAUDE.md` 第 6 条。
- **只读共享与防御拷贝的分界**：构造后不再写的值（如广播 payload 字节）可以跨 goroutine 共享，拷一次在 API 入口即可；逐接收方再拷是纯开销。已写进 Pandora `CLAUDE.md` 第 6 条。
- **「合规改写」不当成修 bug**：单线程下按引用捕获长寿对象、共享从不修改的 protobuf，本来就安全；按规则改写可以保留，但交接里不应把它们记成已修的并发缺陷。
- **跨玩家边界防的是交错执行**：Python 宗师代制出事，是因为两次 `await` 之间宗师的快照已过时；C++ 场景单 EventLoop 内，由组件所属 System 在同一同步步骤里改写本节点在线且可写的玩家并请求存盘（`PlayerCraftSystem::RecordProxyOrderReputation`），中间无异步等待，不会交错，允许。目标不在本节点、不可写，或实现里有异步等待时，一律走目标玩家的台账 / 邮箱 / 串行队列。已写进 [engineering-principles.md](../rules/engineering-principles.md#1-模块与职责111) 与 [ecs-component-access-rules.md](ecs-component-access-rules.md)。

## 6. 仍待办

- **Python 版名匠代打摊位目录、口碑待回账与 24h 热榜改走 Redis**：按 C++ r2-craft#4（`cpp/libs/services/scene/player/system/player_craft.cpp` 的 `ProxyReputationLedgerKey`、`kTakeProxyReputationLua`、`GiveBackProxyReputation`、`ApplyTakenProxyReputation`）补齐，另开会话做（Python 场景目前没有控制 Redis 客户端，需新接）。**状态（11-27）**：Python 仓代码与单测已写完，未运行。键、字段与取走脚本同 C++，买家只记台账，回账在宗师自己的锁内原子取走。另外核对时发现，Python scene 其实已有控制 Redis 客户端（`runtime.py` 的 `scene_control_redis`，世界频道目录与落地唤醒在用），已直接复用，不用新接。设计与 pytest 命令见 Python 仓 `docs/design/craft-proxy-shared-ledger-redis.md`。
- **西市热榜「近 24 小时」从不归零（C++ 原有缺陷，Python 照搬）**：C++ `RecordProxyOrderReputation` 只往台账哈希累加 `<id>:24o/24s`，全仓没有归零、过期或分桶，客户端显示的「近 24 小时单数」实际是开服以来的累计。定案按小时分桶：每小时一个哈希 `craft_proxy_hot:{ctl:<zone>}:<UTC 秒/3600>`（与台账同 hash tag），成交时对当前小时键 HINCRBY 并 EXPIRE 25 小时，刷新时一段同 slot 的 Lua 把最近 24 个小时桶按宗师求和；不再写 `:24o/24s`，游戏未上线不做迁移。C++、Python 两边同改，交给上一条的同一会话做；Java 登记待做。**状态（11-27）**：已落码，未编译未运行。
  - C++：`player_craft.{h,cpp}` 台账接缝加 `addHot`（写桶）与 `readHot`（读 24 个桶之和，生产 = 只读脚本 `kSumProxyHotLua` 经 `commandArgv` 发出），新增 `ProxyHotWindowBucketKeys`、`RefreshProxyHotRanking(最新小时)`。`player_craft_quality_test.cpp` 加两条用例：写桶与停写 `:24o/24s`；窗口滚动与「结果未知保留旧镜像」。设计写进 `craft-proxy-reputation-digest-and-hot-ranking.md` §2.6。
  - Python：键、字段与求和脚本同 C++ 逐字节一致（单测钉文本）。本地镜像 = 上次刷新的桶和 + 之后按小时记的本地增量，本地单进程模式只有后者，窗口照样滚动。窗口、写穿不重复计、应答畸形保留旧值都有用例。
  - Java：已登记 Java 仓 `PARITY.md` 待做。
  - Gemini 命令以 `docs/PROGRESS.md` 2026-11-27「热榜读取改走台账接缝」一条为准，它补充并取代前一条「名匠代打西市热榜改为真正的近 24 小时」里的命令。
- **Python `refresh_loop` 遇到非 Redis 异常会让后台任务退出、此后不再刷新**：改为每轮记英文错误日志后继续（同 C++ 定时器），同一会话一并做。**状态（11-27）**：已落码未运行（`logger.exception` 后下一轮照刷，用例 `test_refresh_loop_survives_unexpected_errors`）。同时修了西市列表遍历在线玩家的平方级开销。
- **Java**：§3 两条，已登记 Java 仓 `PARITY.md`，本轮不写 Java 代码。

## 7. 验证（交 Gemini）

本文修复都未运行。Gemini 交接单自报的旧证据（Go 9 包、Python 30 例、Java 319 例、Unity 32 例、C++ 门禁与 194 例）针对的是 `7f4514f7` 等原提交，本轮改动后需重跑：

1. Pandora Go：在 `D:\luyuan\xuanming-server` 用 11-27 交接单的离线 GOWORK 办法跑 `go test ./services/runtime/player_locator/... ./services/battle/ds_allocator/... ./services/battle/hub_allocator/... ./services/runtime/push/... ./services/economy/auction/... ./pkg/playerdisplay/...`；Linux CI 补 `-race`。`gofmt -l` 已核：本轮文件只剩 `auction.go` 第 929 行一段原有的对齐问题，与本轮无关。
2. Unity：`tools/client_compile_check.ps1`；EditMode `RankedClientTests`、`TournamentClientTests`、`ActorIsolationTests`。
3. 本仓只改了文档：`python tools/scripts/gen_docs_index.py --check`。

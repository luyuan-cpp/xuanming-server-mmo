# Actor 隔离改造（2026-11-27 Gemini 批次）审阅结论

> 审阅：Claude，2026-11-27。只读审阅，未改任何代码，未编译、未跑测试。
> 依据：Gemini 交接单《全工作区 5 仓库 Actor / 消息隔离改造交付与审阅报告》与其审计基线（两份都只在 Gemini 本机目录，不在仓库里，本文自成一体）。
> 规则口径：本仓 [engineering-principles.md](../rules/engineering-principles.md#1-模块与职责111) 两层边界、[ecs-component-access-rules.md](ecs-component-access-rules.md)，Pandora 仓 `CLAUDE.md` 第 359、376 行。

## 1. 审阅范围

五仓改动已于 11-27 10:14–10:15 由负责人账号分别提交进各仓 main，内容与交接单所列文件一致：

| 仓库 | 提交 | 内容 |
|---|---|---|
| `xuanming-server`（Pandora Go） | `7f4514f7` | push 广播值隔离、PresenceHub 改单 goroutine 邮箱、ds/hub allocator 旁路协程改 `safego.Go` 并加在途上限、mail 换 `plog.Detach`、auction/team/playerdisplay/killswitch 补 panic 兜底 |
| `xuanming-server-mmo-cpp`（本仓） | `5fe504e65` | Kafka 命令回调 `[&node]` 改按值捕获；Buff/技能表达式改走快照 `Evaluate*`；配方缓存 `static` 改 `thread_local` |
| `xuanming-server-mmo-python` | `45fe6c7`（并行会话顺带提交）、`0811a1f` | 宗师代制回账加锁 / 锁忙转投宗师串行任务；gate 广播逐会话副本；LocalLogin / LocalServiceHub 改走公开接口 |
| `xuanming-server-mmo`（Java） | `87f79f19` | 跨线程回调改捕获 `(playerId, instanceId, ownerEpoch, freezeId)` 并回逻辑线程重查；`playerById` / `step` 加逻辑线程断言 |
| `mmorpg-client`（Unity） | `5bb30c91` | `UiDomainEvents` 取代 World→UI 反向调用与 `BattleUiRoot` 静态委托；三个 Client 补 `Dispose`；快照集合冻结；`RankedUiRoot` 补注销 |

## 2. 必须改（本批引入的回归，都在 Pandora Go）

### 2.1 PresenceHub 邮箱会静默丢订阅与状态变更

- **位置**：`services/runtime/player_locator/internal/biz/presence.go:157`（`enqueue` 满即丢）、`:276`（`Start` 单循环）、`:340`（同一循环里逐订阅者推送）。
- **问题**：消费邮箱的 goroutine 同时做推送 I/O。生产推送器 `cmd/locator/main.go` 的 `presencePusher` 走 `kafkax.KeyOrderedProducer.Send`，底层是 Sarama `SyncProducer.SendMessage`（`pkg/kafkax/producer.go:128`），逐条同步等确认、不看 ctx。一轮 flush 推几千个订阅者、或 Kafka 变慢时，邮箱停止消费，`Subscribe` / `Unsubscribe` / `Notify` 堆满 4096 条后直接丢，只有每 100 条一行日志，没有指标。
- **后果**（旧的加锁版本都不会发生，旧版推送在锁外做，入口从不被 I/O 拖住）：
  - 丢 `Subscribe`：好友面板收不到在线状态推送；
  - 丢 `Unsubscribe`：关注关系残留，玩家下线后不再有人清；
  - 丢 `Notify`：状态停在旧值直到下一次变化；`lastSent` 基线跟着错，之后真实的一次变化可能被判成「无净变化」吞掉。
- **交接单两处说法不成立**：「满队列丢弃并记日志 / 指标」实际只有日志；「`Close()` 立即打断在途 `PushPresence`」在生产不成立（同步生产者不看 ctx），新单测通过只因测试桩 `blockingPusher` 会看 ctx。
- **建议**：`presence.go` 与 `presence_test.go` 回到 `7f4514f7^` 的加锁版本；如要保留「未 `Start` 直接 `Close` 不挂死」，只补一个 `started` 判断。理由：hub 的五张 map 从不出模块，外部只经方法调用，I/O 在锁外，符合两层边界里的「状态私有 + 窄接口」，不是跨边界共享可变状态。
- **若负责人坚持邮箱写法**，至少要同时做到：推送 I/O 挪出邮箱 goroutine（邮箱只改状态、产出不可变批次，另一个发送 goroutine 推送）；订阅类命令不得丢（阻塞或按订阅者合并）；`Notify` 按玩家合并为最新值而不是丢；丢弃计数进指标。代码量明显大于回退。

### 2.2 ds / hub allocator 的在途上限会丢位置续期

- **位置**：`services/battle/ds_allocator/internal/biz/allocator.go:115`（`maxAsyncSideTasks = 64`）、`:326`（`killStrandedDS`）、`:3387`（`refreshBattleLocations`）；`services/battle/hub_allocator/internal/biz/hub.go:1976`、`:1986`。
- **问题**：`refreshBattleLocations` 在每一跳 DS 心跳都调（`allocator.go` 心跳两处入口），在途数 ≈ 心跳速率 × locator 时延。按 3 秒超时算，locator 一变慢，每秒约 21 次续期就打满 64，之后续期全被丢；`killStrandedDS` 与它共用同一个计数，本地模式（`killOrphanOnStop` 只在 local 打开）下回收孤儿 DS 也会被挤掉。旧写法每个协程本来就有 3 / 5 秒超时，协程数天然有界。
- **后果**：locator 抖动期间 BATTLE 位置 TTL 可能过期，玩家中途掉线重登找不到原 DS（Pandora 规则第 19、20 条「不能让玩家进不去」）。
- **建议**：删掉计数与上限，保留「先同步 `plog.Detach` 再 `safego.Go`」与原超时。hub 那处同样删（影响小，但同一写法）。

## 3. 建议与观察（不阻塞）

| 仓 | 位置 | 说明 | 建议 |
|---|---|---|---|
| Go | `auction.go:275` | 事件驱动的审计 worker 用了 `safego.Run`，它会在每轮成功后刷新 `pandora_loop_last_round_unix{name="auction_audit_worker"}`；没有审计事件时时间戳不动，以后若加统一停摆告警会误报 | 改成闭包内 `defer safego.Recover(...)` |
| Go | `connection.go:43`、`:137` | 广播先拷一次 payload，再给每条流各拷一次（N+1 份）；原先共享的帧构造后没人改，protobuf-go 只读并发 Marshal 安全。`consumer.go` 对 `msg.Value` 的拷贝也非必需（Sarama 不复用这块内存） | 入口拷一次后各流共享只读字节，去掉 `toProto` 里的逐流拷贝 |
| Go | `pkg/playerdisplay/projection.go` | worker panic 被兜住后，该批结果按「成功但为空」处理，不进 `Failures` | 可接受（panic 本身有计数告警）；要严谨可在兜底里把在途批标失败 |
| C++ | `skill.cpp`、`modifier_buff_impl.cpp`、`buff_impl.cpp` | 语义已核对等价：`bonusDamageExpr` 从未 `Init`（零个参数），空参数求值正确；回血两参、伤害一参都对得上。代价是每次求值新建 exprtk 解析器与符号表（旧路径复用成员解析器，但同样每次编译） | 不改；下一轮压测看 CPU，有数据再按表行预编译 |
| C++ | `player_equipment_v1.cpp` | `static` 改 `thread_local` 在只有主线程的节点上行为不变，也没解决「避免 static 存储」 | 不改 |
| C++ | `node_kafka_command_handler.h` | 已核对：节点号每进程只选一次（失租重注册沿用同一键，失败即隔离）、`node_uuid` 只生成一次、`maintenancePrepareBoot` 只赋值一次，且都在注册 Kafka 之前；分区本来就按注册时的节点号算。按值捕获与原行为等价 | 无 |
| Python | `scene_skill_craft.py:394` | 锁忙时记下的待回账在内存字典里，不是交接单说的「持久化」。优雅停机时 `_closing` 让 `mutable()` 返回假，排空任务空转；宗师换到别的 scene 进程也带不走。只影响口碑与营业战报计数（手工费走买家同事务出箱，不受影响），离线宗师这条路原来就如此 | 先记录现状；要不丢需把口碑增量也放进买家同事务出箱，属新设计，待负责人定 |
| Python | 同上 | 「锁空闲就当场拿宗师锁」与「锁忙就投宗师串行任务」两条路并存。按 grep 到的取锁点抽查，目前没有「持一把玩家锁再阻塞拿另一把」的路径，当前无死锁环；但 `locked()` 为假时若锁上有排队者，`acquire` 仍会等待，以后谁在持锁时再阻塞拿别人的锁就可能成环 | 可简化为一律投宗师串行任务，删掉当场提交分支 |
| Python | `gate.py:1488` | 广播改为先序列化、再逐会话反序列化副本；会话写出循环只序列化不改消息，原先共享对象是安全的。单线程 gate 上每次世界广播多 N 次解析 | 性能观察项 |
| Java | `SceneWorld.java:295` | `playerById` / `step` 越线即抛异常。53 处调用逐处核过，都先切回逻辑线程（资产操作、收件箱提醒、瑞兽开局回调、同步摘要、换线冷却、改名、邮件出箱等）。但 `SceneWorld` 默认守卫恒放行，只有 `SceneNode` 绑定真守卫，现有 319 个用例都没在守卫下跑过 | 上线前在 `SceneWorld` 测试夹具里绑定单线程守卫跑一遍，或至少冒烟一次 |
| Java | `PlayerBattleService.java:797` | 实例号比对与原引用比对等价；`ScenePlayer.ownerEpoch` 是 `final`，多出的代次比对冗余无害。只有「Inbox 已落盘 → 清迟到确认锁」这一支收窄了：玩家已换实例或离场时不再清，靠下次进场的恢复读兜底 | 可接受；要保持原行为可让这一支只要求 `player != null` |
| Unity | `RankedModels.cs:592`、`TournamentModels.cs:443` | `FreezeList` 遇到 `ReadOnlyCollection<T>` 直接复用，但它只是只读包装，可能包着会变的 `List`。目前构造函数只被同文件的 `FromResponse` / `With*` 调用，传入的都是冻结结果或 proto 重复字段，暂时安全 | 去掉这个短路，一律拷贝（列表很小，只在收包时构造） |
| Unity | `GameClient.cs:3054` | 释放顺序变成先断线再释放子 Client；原来 `SkillCraft` / `Skills` 在断线前释放。技能界面已先解绑，风险低 | 无 |
| Unity | `UiDomainEvents.cs` | 仍是静态可变状态，只是从 `BattleUiRoot` 挪进带归属校验的静态类；WebGL 单线程下可接受。`NavigationRejected` 生产代码无订阅者，「目标不可到达」不会重复提示 | 无 |

## 4. 交接单 7 个核查点的答复

1. **PresenceHub 背压**：问题比「订阅与状态要不要分开背压」更根本，见 §2.1，建议回退。
2. **Python 锁忙回退时序**：不会重复消费——所有排空都在宗师自己的锁内（客户端请求分发 `scene.py` 按玩家加锁、投递任务也先拿锁），且是「读快照 → 提交 → 扣减已排空量」。会残留：停机与换进程时丢失，见 §3。
3. **Java `AfkPassService` 未绑定查询函数**：生产只在 `SceneNode` 构造并经 `withAfkPassService` 绑定，未绑定分支只在单测出现。可接受。
4. **Java 七服务共锁 `player` 表等**：同意留到下一阶段，涉及跨服务表结构与协议，需单独设计。
5. **Unity 快照元素可变性**：接受「集合冻结 + 收包与派生时克隆 + UI 约定只读」的折中；另见 §3 `FreezeList` 短路。
6. **C++ 表达式热路径**：同意先压测观察，见 §3。
7. **C++ `player_battle.cpp` 多组件 `Swap`**：结论与新规则字面冲突，见 §5。

## 5. 规则与代码仍有一处冲突（待负责人定）

`da97bfb11` 写进 [ecs-component-access-rules.md](ecs-component-access-rules.md) 与 [engineering-principles.md](../rules/engineering-principles.md) 的规则是「禁止绕过接口直接 `try_get` / `Swap` / 改写他系统私有组件」，而 `player_battle.cpp` 战后结算仍由编排方直接 `Swap` 账本、赛季、试炼志、瑞兽、通行证、江湖等他系统组件（HEAD 第 3275–3308、3466–3469 行），交接单却称其「符合第二层」。二选一：

- **规则补例外**（改动最小）：写明「单玩家多组件写时复制原子提交——先在副本上改、全部校验通过后同步一次性交换——允许由编排方执行，副本由所属 System 的接口生成与校验」；
- **代码走窄接口**：每个 `Swap` 换成所属 System 的提交函数，仍在同一同步调用里完成，原子性不变。

## 6. 待负责人拍板

1. §2.1 PresenceHub：回退加锁版本（推荐），还是保留邮箱并按 §2.1 末条补齐。
2. §2.2 allocator：删除在途上限（推荐）。
3. §5：规则补例外，还是代码走窄接口。
4. §3 Python 口碑增量是否要做成不丢（需新设计）。

## 7. 验证状态

本审阅未运行任何命令。Gemini 交接单自报：Go 9 个包单测通过（Windows 无 CGO，未跑 `-race`，未覆盖 `data_service`）；Python 30 个用例通过；Java `game-scene` 319 个用例通过；Unity 离线 Roslyn 编译零错、32 个纯 C# 用例通过（未在编辑器、PlayMode、WebGL 跑）；C++ 静态门禁 32 项与 `buff_test` 46、`skill_test` 106、`bag_test` 锻造 42 个用例通过（未跑 Linux 构建）。以上运行证据本审阅未逐份核对原件。

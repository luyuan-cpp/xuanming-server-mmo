# Go match 观战档位、L1 / L2 准入、凭证、全局幂等、可见性闸：语义评审

只读评审，全部基于静态阅读，未编译、未运行。契约：`docs/design/battle-spectate-tiered-fanout.md` §15.1、§15.2、§15.6。
路径前缀省略 `go/match/internal/`（另有说明的除外）。行号取自 2026-10-30 本次读取的工作区；`logic/spectate.go`、`logic/watchbattlelogic.go` 另有「修本地模式观战」会话在改，标记删除逻辑不在本次范围，行号可能再漂移。

**结论**：没有阻断级问题；严重 4 条（1 条是存量缺陷）、一般 8 条。全局路径的意图状态机（LOBBY 结案、重放、顶替、清退）时序正确，`delivery` 确实持久化并在重放时读到。主要问题在 Redis 键和全服 ZSET 的生命周期：它们还按本地模式「战斗最长 300 秒」设计，而全局路径的战斗期限已改成模板时长（30～60 分钟）。

---

## 严重

### S1. 全局路径：观战键 TTL 与战斗期限脱节，档位和赛事局标记 6 分钟失效，活着的场被当过期剔除并退掉配额

- **位置**
  - TTL：`logic/spectate.go:68-74`（`BattleMaxDurationSeconds + 60`），`go/match/etc/match_service.yaml:114` 配 300，即 360 秒。使用点：`logic/spectate_tier.go:79`（档位）、`:201`（中继 gate HASH）、`:493-498`（赛事局标记与索引）。
  - 过期判定：`logic/spectate.go:308-310`；剔除点 `logic/listwatchablebattleslogic.go:84-86`、`logic/spectate.go:343-345`；`logic/spectate.go:284-296` 剔除时一并 ZREM L2 配额与赛事局索引。
  - 全局期限：`authority/match_rules.go:52-54`（提交时刻 + 120 秒 + 模板时长）、`logic/gather.go:270`；`generated/tables/dungeon.json` 三个模板的 `time_limit` 是 1800 / 3600 / 1800。HEAD 上全局期限还是固定 300 秒，「全服匹配」会话改成模板时长后，这个假设就不成立了。
- **触发序列**（`time_limit=1800` 的公开 PVE 局，T 为开局时刻）
  1. T+1 分钟直连席满，`spectate:tier` 写 RELAY，TTL 360 秒；gate 挂接写 HASH，TTL 同样从首次写入起算。
  2. T+5 分钟升 L2，配额 score 约为 T+32 分钟。
  3. T+6 分钟起，任何人调观战列表（本场在前 limit 条内），或随机选场选中本场：score（`created_at_ms`）早于 `now-360s`，走 `removeSpectateBattle`，本场从活跃集合摘除，L2 配额和赛事局索引被 ZREM。battle 仍在广播。
  4. 另一场抢到腾出来的配额，实际 L2 场数超过 B。
  5. T+7 分钟档位键到期，下一名观众读到 DIRECT，重新走 `BeginSpectator`（锁 `match_formation`）+ AddObserver，满员后再升档；HASH 同时过期，各 gate 重新 Attach，HLEN 从 0 重新累计。
  6. 运营标记满 360 秒后，`spectate:featured:{id}` 与索引 score 同时到期，赛事局不再置顶，也不再触发升档。
- **后果**：「档位只升不降」在生产路径上只保证 6 分钟；B 不再是上限（战斗打满 30 分钟时，同一名额每 6 分钟可再放进一场）；30 分钟的赛事局标记只活 6 分钟；活着的场从列表和随机选场消失。
- **最小修法**：`spectateTTLSeconds` 在 `Authority != nil` 时返回 `catalog.MaxBattleSeconds + WorkLeaseMs/1000 + 60`（3780 秒），过期分界随之修正。要和 S2、S3 一起落，否则已结束的场会在集合里留得更久。

### S2. 全局路径：L2 配额没有「战斗结束释放」，结束的场占名额到期限（32～62 分钟），B=4 很快用尽

- **位置**：`logic/spectate_tier.go:338`（score = 战斗期限）、`:252-262`（只清已到期成员）。全局路径能走到 `removeSpectateBattle` 的入口只有：Attach 回房间不存在（`logic/global_spectator.go:287-290`，BROADCAST 档观众不调 RPC，走不到）、记录为 nil（`logic/listwatchablebattleslogic.go:126-127`，但 `loadSpectateBattle` 只在 state 5 时回 nil，COMPLETE 后成局仍是 state 4：`authority/completion.go:444`），以及 S1 的误剔除。
- **触发序列**：4 场热门局依次升 L2，各打 5 分钟结束 → 配额 ZSET 的 4 个成员 score 都在开局后 32～62 分钟 → 第 5 场热门局抢配额时 Lua 回 0（`quota_full`），一直留在 L1，直到最早那场到期。
- **后果**：L2 在生产上大部分时间不可用，热门局退回 battle 按 gate 逐条发 Kafka。`reports/p1-match.md` §5 写的「battle 没有报结束的通道」不成立：全局模式每场都走 `logic.CommitBattleOutcome`（`logic/global_match.go:461-491`）。
- **最小修法**：`CommitBattleOutcome` 拿到 decision 后，用 `LaunchRelayTask` 尽力调一次 `removeSpectateBattle(s, in.GetBattleId())`（幂等，失败只记日志，不影响终局回执）。这样结束的场也立刻离开列表和随机选场。

### S3. 全局路径没有定时清理：活跃集合与赛事局索引无界增长，随机观战会假报「没有可观战的战斗」

- **位置**：`logic/matcher.go:138-153` 全局分支提前 return，`cleanupExpiredSpectateIndex` 的唯一调用点在 `runMatcherRound`（`logic/matcher.go:168`）。全局路径照样 ZADD 活跃集合（`logic/gather.go:527`）。全局 L0 收到房间不存在也不摘索引（`logic/global_spectator.go:568` 丢弃 `roomMissing`）。`logic/listwatchablebattleslogic.go:97-98` 只读未到期的赛事局成员，过期成员读不到也删不掉。
- **触发序列**：每场公开局 ZADD 一次，结束后没人 ZREM → 列表只看最新 limit 条（`logic/listwatchablebattleslogic.go:53`）→ `pickRandomBattle` 最多试 3 次、每次摘 1 个（`logic/spectate.go:316-351`）→ 跑几小时后过期成员占绝大多数，3 次全落空，返回 0 → `closeUnpreparedGlobalWatch(ErrNoWatchableBattle)`（`logic/global_spectator.go:198-199`）把这个 request_id 永久关成 16017，而此刻其实有活着的场。
- **后果**：活跃集合的泄漏是存量问题，本批新增的 `spectate:featured:active` 同样泄漏（量小，但永不回收）；随机观战随运行时间退化。
- **最小修法**：全局分支加一个低频循环调 `cleanupExpiredSpectateIndex`（照 `match.global.expired.audit` 的写法）。必须在 S1 之后落，否则按 360 秒分界会把活着的场也清掉。`resumeGlobalSpectator` 的 `roomMissing` 分支也摘一次索引。

### S4（存量，非本批引入，L1 / L2 继承）. battle 实例消失后，L0 原件永远拿不到取消证明：该玩家的新观战（含 L1 / L2）和排队都卡死

- **位置**：`logic/global_spectator.go:187-191`（prepare 先清退旧 L0，失败直接返回）、`:433-469`（只有带回原 op 与实例的 `cancelled` 才能推到 5）；`cpp/nodes/battle/logic/battle_room_manager.cpp:1228-1231`（实例不符回空包）；`logic/global_match.go:73-75`（排队同样要这份证明）。全工程把行推进到 5 只有这一条路。本地路径有标记 TTL 兜底（`logic/spectate.go` `stopWatching` 注释），全局路径没有对应出口。
- **触发序列**
  1. 玩家 P 是 battle 实例 I 上的 L0 观众（行 state=2）。
  2. I 崩溃或被强杀，端点不可达，或同端点换了新实例。
  3. 后台恢复的 `QueryObserver` 报 RPC 错误或空包，一直 pending（`logic/global_spectator.go:527-534`）。
  4. P 发 WatchBattle：`cancelGlobalSpectator` 的 Remove 报错或空包，回 16004；P 发 JoinQueue：回「原观战退出尚待确认」。没有超时出口。
  5. 点开已崩溃场次的 L0 观众同样中招：Add 失败 → state 4 → 准入截止后转 3 → 等不到 Remove。
- **后果**：P 永久不能观战、不能排队，只能手改库。L0 满员转 L1 也走这条取消路径，但窗口只有 Add 与 Remove 之间。
- **最小修法**：给取消加「原实例已不存在」的判据，满足时不带证明推进到 5：已过成局 `deadline_ms` + `terminalOutcomeGraceMs`（房间必已被 battle 定时器收尾），或注册表里同 node_id 已是别的 uuid（缩短等待）。Add 带 `target_instance_id`，新实例不会接纳旧 op，放宽是安全的。这条放宽了「正证明才关闭」的原规则，要负责人拍板。

---

## 一般

### G1. Attach 回 `broadcast=true` 只同步档位不补配额；先来者失败的 ZREM 还能抹掉后来者成功的账。B 被突破且不自愈

- **位置**：`logic/spectate_tier.go:452-457`（同步分支没有 ZADD）、`:330-332`（新占者失败就 ZREM）、`:308-309`（注释称「最坏短暂多占一个名额」）。
- **触发序列 A**：V1 新占配额 → Promote 在 battle 已生效但 1 秒超时（`:40`）→ V1 退还 → 别的场占走名额 → 新 gate 的 V2 Attach 回 `broadcast=true`，档位写 BROADCAST。此后没有观众再碰配额，这场一直不计数。
- **触发序列 B**：V1 新占，V2 看到 Held；V2 的 Promote 成功并 ZADD；V1 的 Promote 随后超时并 ZREM。本场 BROADCAST，配额里却没有它。
- **后果**：实际 L2 场数超过 B，持续到这场结束，不是「短暂」。
- **最小修法**：同步分支补一次 ZADD（score = `source.deadlineMs`，超额也记，后面的抢占自然被挡）；V1 退还前先复读档位，已是 BROADCAST 就不退。

### G2. 直连席满之后同 op 再发 Add，battle 墓碑回 `kInvalidParameter`，请求被永久关成 16018，不是 LOBBY

- **位置**：`logic/global_spectator.go:570-580`（先写 Redis 升档，成功才写 SQL 结论）；`cpp/nodes/battle/logic/battle_room_manager.cpp:1049-1054`（`cancelled` 的 op 一律 `kInvalidParameter`）、`:1066`（满员分支保留墓碑）；`authority/spectator.go:653-661`（恢复扫描不看行龄，刚写入的 state 1 也会扫到）。
- **触发序列 A**：Add 回满员 → `spectateDirectFullToRelay` 报错（Redis 抖动，或请求 ctx 恰好截止）或 `RejectSpectatorToLobby` 失败 → 行仍是 state 1 → 客户端重试或后台恢复再发同 op 的 Add → `kInvalidParameter` → `rejectGlobalSpectator(16018)`，`close_tip` 先写为准。
- **触发序列 B**：前台与后台恢复并发发同 op 的 Add，先到的回满员，后到的回 `kInvalidParameter`。两边写结论，LOBBY 与 16018 谁先落库谁赢。
- **后果**：契约要求这次请求以 LOBBY 结案，实际可能给观众回「该战斗当前无法观战」，同 request_id 重放也是这个结论；换新 request_id 能恢复。窗口窄，不卡死。
- **最小修法**：Go 侧调换顺序：中继可用就先写 SQL 结论，升档只记日志（`replayGlobalLobby` 已会补升档）。battle 侧让 `SpectatorOperation` 记下首次拒绝码，同 op 重放回原码（C++ 与 Java 同批），序列 B 与存量的 16021 漂成 16018 也一并解决。

### G3. Promote 回房间不存在被吞掉，已结束的 RELAY 场继续签凭证、不摘索引

- **位置**：`logic/spectate_tier.go:326`（`source, _, err :=` 丢弃 `rejectTip`）、`:462-466`（失败后照常往下签凭证）。
- **触发序列**：RELAY 场结束 → 已挂接 gate 的观众不调 Attach，判定该升档 → 抢配额 → Promote 回 `kEntityIsNull` → 只记日志 → 仍签凭证回 LOBBY。配额是 Held 时也不退。
- **后果**：这是 match 在这条路径上唯一能知道场已结束的机会，却丢了；观众拿到无帧的凭证，等 gate 30 秒收尾；索引与配额继续留着。
- **最小修法**：把 tip 返回给调用方；`spectateRejectVerdict` 判 `roomMissing` 时按现有懒剔除口径处理（`removeSpectateBattle`，拒绝这名观众），不论配额是否本次新占。

### G4. 已挂接 / L2 路径不复核实例，battle 节点崩溃或重启后继续签死实例的凭证

- **位置**：`logic/spectate_tier.go:436`、`:443`、`:467-476`（来源已知就直接用，不做任何实例核对）。全局来源是成局上固定的实例（`logic/spectate.go:263-281`）。
- **触发序列**：battle 节点重启（同 node_id 新 uuid）→ 已挂接 gate 的观众与 BROADCAST 档观众都跳过 RPC → 凭证写旧实例 → gate 等不到帧，30 秒后合成 166（`SOURCE_LOST`）→ 客户端再请求，再拿旧实例。本地持续到记录 TTL；全局持续到成局作废（`deadline_ms` + 60 秒，最长约 1 小时）。这期间配额也不释放。
- **后果**：观众反复空等；索引和配额不释放。
- **最小修法**：签票前查内存注册表：`BattleNodes.EntryOfNode(record.GetBattleNodeId())` 成功且 `NodeUuid` 与来源实例不同，就按房间不存在处理（走现有懒剔除与 16018）。查不到不判（`EntryOfInstance` 注释已说明 etcd 抖动时查不到不代表实例已死）。不增加 RPC，L2 不调 battle 的约定不变。

### G5. 升档失败没有退避：每名 RELAY 观众都打一次 Promote，最多阻塞 1 秒，并打一条 Error 日志

- **位置**：`logic/spectate_tier.go:326-334`（Held 也发 RPC）、`:462-466`（每名观众判一次），`:40`（1 秒预算）。
- **触发序列**：battle 节点过载导致 Promote 超时 → 之后每名到达的观众都抢配额、发 RPC、等满 1 秒、打 Error。
- **后果**：battle 最忙的时候被观众请求放大打（L1 本来就是要把观众和 battle 隔开）；不符合 AGENTS §11.3「重试须有上限与退避」；Error 日志未采样。
- **最小修法**：RPC 前加短租约（`SET spectate:promote_try:{id} NX PX 3000`），只有拿到租约的观众发 RPC，其余直接按 RELAY 签票。

### G6. 运营入口的 PermissionDenied 会给本实例记 root Hold

- **位置**：`logic/setspectatefeaturedlogic.go:39-47`、`go/shared/serverbase/interceptor.go:186-189`、`go/shared/drainproof/report.go:74-75`、`:101-103`。`SetSpectateFeatured` 不在 relay 方法表（`go/match/match_service.go:286-290`）。
- **触发序列**：运营工具密钥配错、本机时钟偏差超过 5 分钟，或入口未开启时调用一次 → 回 PermissionDenied → `root.Hold()`，`unknown` 不再清零 → 这个副本之后的排空证明一直 HOLD。
- **后果**：拒绝发生在任何副作用之前，结局是确定的，不该计 Hold。`reports/p1-match.md` §5 已列为风险，未改。
- **最小修法**：把验签挪到 `PublicationAdmission` 外层的独立拦截器，或给 `SpectateFeaturedReject` 增一个拒绝值、不回 gRPC 错误。

### G7. `match_spectator_intent` 没有保留期，LOBBY 观众每个新 request_id 永久留一行

- **位置**：`authority/spectator.go:242`（插入），全仓 `go/` 没有 `DELETE FROM match_spectator*`。
- **触发序列**：万人观战的场，每人每次新发起观战各加一行（请求原文 + 目标场原件），永不删除。
- **后果**：存量设计是低频 L0 下的「永久意图」，L1 / L2 把它变成了大量写入，存储无界增长。`close_tip` / `delivery` 列本身没问题。
- **最小修法**：定保留期（例如 state=5 且不是请求头、超过 N 天的行由低频任务删除），需负责人定 N：删除后极旧的 request_id 重放会被当作新请求。

### G8. 测试没有覆盖上述声称的行为

- `logic/global_spectate_lobby_native_test.go:58-66`：`fakeBattleNode.AddObserver` 满员时可无限重放、不建墓碑，真实 battle 第二次回 `kInvalidParameter`，覆盖不到 G2；`:68-73` 的 `RemoveObserver` 恒回 `cancelled`，覆盖不到 S4。
- 所有夹具的期限都是 `testRelayDeadlineMs`（2100 年），TTL 用默认配置，没有「战斗期限大于 TTL」的用例，覆盖不到 S1、S2。
- `logic/spectate_tier_test.go:451` 的 `TestWatchBattleAttachBroadcastReplySyncsTier` 不断言配额（G1）。
- `logic/setspectatefeaturedlogic_test.go:249` 直接调 `cleanupExpiredSpectateIndex`，注释称「matcher 每轮顺手清」，覆盖不到全局模式没人调（S3）。
- 凭证 Go 与 C++ 测试各算各的 HMAC，字节级一致只靠阅读确认（见下），建议加一条跨语言固定向量（C 批已有 `BroadcastTargetVectors` 的先例）。

---

## 已核对、未发现问题

1. **全局路径的请求时序**
   - L0 满员转 L1：`AddObserver` 回满员 → state 3 + `delivery=LOBBY` 同事务落库 → Remove 拿到关闭证明（`cancelled` 墓碑挡住在途 Add）→ state 5 → 本次请求经 `closedGlobalWatch` → `replayGlobalLobby` 准入。取消未确认时结论已落库，前台重试或后台恢复都能收尾。
   - 同 request_id 重放：state 5 且 LOBBY → `RequireLobbyReplay` 复核请求头、匹配票、组队 / 历练占用 → 重新定位会话 → 新凭证；不重选场，路由取当前会话。
   - 被顶替：state 1 的旧意图同事务关闭；state 2 的只在 `close_tip=0 AND delivery=0` 时记 16016；已记 LOBBY 的不改写，重放由请求头复核回同码。旧 handler 的 `CloseLobbySpectator` / `BeginSpectator` 都卡在 `requireSpectatorHead`，拿到凭证也发不出去。
   - 清退 L0：prepare 开头沿原件取消，失败不结案，后台恢复按 `superseded` 收尾（实例已消失的情形见 S4）。
   - 其余状态组合：业务退出关闭 state 1 时 L1 handler 读到非 LOBBY，凭证不发；`close_tip` 与 `delivery` 互斥由所有写者的 WHERE 条件保证；`CaptureSpectatorExitsTx`、`validLobbyPin` 都能处理只有目标场的原件。
   - `delivery` 列：列清单、读写、proto 驱动的迁移与启动核对一致。proto 是 `uint32` 而不是 §15.2 写的枚举，注释已说明，不影响语义。
2. **档位与 HASH**：Lua 比较后写、`KEEPTTL`、返回实际档位（只是 TTL 见 S1）；先 RPC 后记账，失败不记账，记账失败只多挂一次；并发 Attach 由 battle 幂等；gate 换实例比对 instance 后重新挂接；全局路径 target_instance_id 与记录同源成局，本地路径实例不符时 Attach 回房间不存在并懒剔除（已挂接路径见 G4）。`reports/p1-match.md` 列的 gate 重启竞态还有一种更窄的交错（battle 上旧实例后到、HASH 上新实例后到，HASH 显示已挂而 battle 发往旧实例，要等 HASH 过期），需要旧实例的请求仍在途，不单列。
3. **L2 配额**：Lua 先清过期再判定，同一脚本内完成；Promote 失败只由新占者退还；配额到期可自清，没有永久占满（占用时长见 S2，账目偏差见 G1）。
4. **凭证**：实例与期限来自 battle 回包或记录 / 成局；`source.known()` 不成立时 L1 重新 Attach、L2 问一次 battle，不会签出空实例；`checkedBattleSource` 校验回包。签名字节与 gate 一致：小写 hex 的 HMAC-SHA256，对 payload 原始字节，密钥两边都用原值（`cpp/nodes/gate/spectate/gate_spectate_ticket.cpp:55-56`、`cpp/libs/engine/core/security/token_security.h:34-62`）。TTL = 签发时 + `TicketTTL()`，gate 按墙钟 `expire_at_ms <= now` 拒绝。
5. **可见性闸**：Redis 指定场与随机模式、全局首次与重放（`locateGlobalWatch`）、`SetSpectateFeatured`、列表（含赛事局）都过同一判据；battle 的 Attach / Promote 还有第二道闸；`rememberSpectateSource` 改写记录时保留 visibility。
6. **fail-closed**：`RelayUsable` = 开关开且密钥非空白；不可用时档位键被忽略，满员回 16021，LOBBY 重放回满员，赛事局只写标记；prod 打开中继而密钥短于 32 字节拒启；档位值读不懂回 16004；签票时密钥或来源缺失报错。
7. **运营入口**：`Verifier` 只装 `ops` 一把密钥，别的调用方身份查不到密钥即拒；签名绑定方法名、时间戳与请求摘要（清空 auth 后的确定性 PB）；`sessionInterceptor` 与逻辑入口都拒绝带会话的调用；未配置时 `SpectateOpsAuth` 为 nil，一律拒绝。没有绕过路径（Hold 副作用见 G6）。`etc/match_service.yaml` 已加 `IgnoreContentMethods`，签名不进日志。

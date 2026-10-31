# 任务 JAVA-P1：Java 版观战 gate 中继 / 全服广播（客户端契约与 C++ / Go 版一致）

先读同目录 `00-common-rules.md` 与 Java 仓 `D:\luyuan\xuanming-server-mmo\AGENTS.md`。权威契约：
`D:\luyuan\xuanming-server-mmo-cpp\docs\design\battle-spectate-tiered-fanout.md` §15（§15.2 客户端可见部分两版必须一致）。
C++ / Go 版实现可以只读参考：`cpp/nodes/gate/spectate/`、`cpp/nodes/battle/logic/`、`go/match/internal/logic/spectate_tier.go`、
`spectate_ticket.go`（以实际文件为准，见 `docs/handoff/spectate-tiered-fanout-20261030/reports/p1-*.md`）。
Java 的 proto 由 Codex 跑 `java tools/ContractSync.java --mmorpg ../xuanming-server-mmo-cpp` 同步，生成名直接引用，你不跑。
不跑 mvn / mvnw；只做外科式 Edit；工作区可能混有其他会话的改动。

## 客户端可见（必须与 C++ / Go 版完全一致）
- `WatchBattleResponse.delivery`（DIRECT / LOBBY）、`spectate_ticket`（LOBBY 时非空）；`BattleWatchSummary.featured`。
- gate 本地处理 `GateClientSpectate.SubscribeSpectate` / `UnsubscribeSpectate`（消息号由契约同步来）；凭证无效回
  `MatchSpectateTicketInvalid`；首帧 161，之后每回合 158，终局 166，经大厅连接下发；一个会话同时只订一场。
- `SPECTATE_END_SOURCE_LOST`：源站失联或过了战斗期限 + 60 s 时 gate 本地收尾。
- 凭证格式同 `proto/gate/gate_spectate_ticket.proto`（match 签、gate 验，HMAC-SHA256 小写 hex，新密钥与 gate / battle 令牌分域）。

## 内部实现（Java 惯例，不照抄 C++，§15.7、AGENTS §12）
Java gate 下行走 Redis pub/sub，不依赖 Kafka。**中继档与广播档合成一种机制**：
- **battle**：房间有 `spectateRelayEnabled`（只升不降）。启用后每回合把观战帧（`SpectateFrameEvent` 同形：battle_id、实例、round_index、
  kind、已编码的客户端帧、deadline）只发布一次到 Redis 分片 pub/sub 频道 `game:spectate:{battle_id}`（经 `RedisKeys` 统一命名，
  带命名空间前缀），并把最新状态帧写入 `game:spectate:state:{battle_id}`（带 TTL = 战斗期限 + 余量），供 gate 给新订阅者发首帧。
  `AttachRelayGate` / `PromoteSpectateBroadcast`（内部 gRPC，校验口径同 §15.4：实例、房间存在、公开）都只是幂等地置位这个开关，
  回实例 UUID 与 deadline；Java 不需要逐 gate 挂接，也不需要 L2 配额（只有有本机观众的 gate 才订阅该频道，没有读放大），
  差异写进 PARITY 备注。
- **gate**：`SpectateHub`（battle_id → 本机订阅会话；会话 → battle_id；最新帧缓存；last_round；last_frame 时刻；deadline）。首个本机订阅者
  到来时分片订阅频道、读 `game:spectate:state:{id}` 发首帧；最后一个离开后 linger（30 s）再退订。收帧规则同 §15.5：实例不符丢弃，
  TURN 只收更大的 round，STATE 只更新缓存，END 只扇一次并保留 10 分钟墓碑。扇出：预编码成一份 `ByteBuf`，各 channel 写
  `retainedDuplicate()`；channel 不可写（`!isWritable`）就跳过这一帧并计数，不断开大厅连接；跨 EventLoop 按现有
  `session.execute` 口径投递。空闲 30 s 或过期合成 166（SOURCE_LOST）。Redisson 分片 pub/sub 的具体 API 按项目现有依赖版本选。
- **match**（`WatchBattleService`、`GlobalWatchService` / `GlobalWatchAuthority`）：档位记账（Redis 只升不降）；L0 满员（`kMatchSpectateFull`）且开关开启 →
  调 battle 置位 → 回 LOBBY + 凭证；已是中继档的场直接回 LOBBY + 凭证（不调 battle，不进逐人名单）。全局模式 L1 观众不写占用行，
  回执持久化方式沿用现有 `respond` 口径（重放时重新签发新凭证，凭证有 TTL，不能原样回放旧凭证）。配置：
  `game.match.spectate.relay-enabled`、`ticket-secret`、`ticket-ttl-seconds`，prod 空密钥拒启。featured：内部入口置位 + 列表回填排前（与 C++ 同语义）。
- 指标：gate 订阅数、扇出、跳帧、凭证拒绝、源站失联；match `lobby` outcome。

## 测试
Hub 的订阅替换、首帧、去重、END 墓碑、空闲收尾、不可写跳帧（`EmbeddedChannel` 或现有夹具）；凭证正反例（与 Go 同算法，加一条跨语言向量：
同一 secret + payload 得到与 Go 测试相同的 hex）；match 满员转 LOBBY、重放签新凭证、关闭开关回满员；battle 开关幂等、发布一次、写状态键。
robot 观战场景若断言「大厅连接上 158/161/166 为 0」，LOBBY 档要放开（`BattleSmokeScenario.java`）。

## PARITY
只追加一行「观战 P1 / P2：gate 中继与全服广播（Java 合成一种机制）」，状态「已落码，未编译，待 Codex 验证」，备注写清与 C++ 版的有意差异。

## 交付
完整交付说明写到 `D:\luyuan\xuanming-server-mmo-cpp\docs\handoff\spectate-tiered-fanout-20261030\reports\java-p1.md`，最终回复三五行摘要。

## 2026-10-30 并入的两条修正（来自 Java B 批交付说明 reports/java.md）

1. robot `BattleCrossZoneScenario` 的 Z5 / Z6 观战 1V1：1V1 是计分局，新策略下被拒。改成断言 16018（可见性闸生效），跨区观战的正向覆盖改用公开局（PVE）或另加一步；不要放宽策略。
2. 全球路径的观战列表 / 随机观战现在靠解码建房原件做有界过滤，私密局多时列表变短、随机可能误回 16017。改为：全球成局表加 `spectate_public`（或同义）列并建合适的索引，建房冻结时写入，列表与随机在 SQL 里直接过滤；去掉有界解码过滤。项目未上线，schema 直接改，不做兼容。改 `GlobalMatchStore` 等文件前先看 git diff，别碰其他会话的段落。

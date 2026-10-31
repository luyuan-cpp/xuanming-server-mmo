# 任务 P1-MATCH：match 观战档位、L1 中继准入、L2 升档与配额、观战凭证、赛事局标记

先读同目录 `00-common-rules.md`。权威契约：`docs/design/battle-spectate-tiered-fanout.md` §15（务必读 §15.1、§15.2、§15.6），
以及 §3、§9、§13。proto 已由主会话改好：`match_service.proto`（`eSpectateDelivery`、`WatchBattleResponse.delivery/spectate_ticket`、
`BattleWatchSummary.featured`、`SpectateBattleRecord.tier/battle_instance_id/deadline_ms`）、`match_authority.proto`
（`MatchSpectatorIntentRecord.delivery = 10`，TiDB 表 `match_spectator_intent` 要加列）、`match_internal.proto`
（`SetSpectateFeatured` + `SpectateFeaturedReject`）、`battle_node.proto`（`AttachRelayGate` / `PromoteSpectateBroadcast`）、
`battle_data.proto`（`eSpectateTier`、`eSpectateVisibility`）、`proto/gate/gate_spectate_ticket.proto`（凭证，go_package "gate"）。
A 批、B 批与「本地模式原操作」子任务都改过 match 的观战文件（`spectate.go`、`watchbattlelogic.go`、`global_spectator.go`、
`listwatchablebattleslogic.go`、`spectate_visibility.go`、`authority/spectator.go`、`constants/errors.go`、`metrics/metrics.go`），
先完整读最新版本。`gather.go`、`global_match.go`、`authority/store.go`、`svc/servicecontext.go` 有「全服匹配方案讨论」会话的改动，
只在必要处做最小外科编辑（servicecontext 只加装配几行）。

## 允许改的文件
- `go/match/internal/config/config.go`（新增 `Spectate` 配置段）、`go/match/internal/svc/servicecontext.go`（只加装配）
- `go/match/internal/logic/` 下观战文件，新增 `spectate_tier.go`（档位、L1 准入、L2 升档、配额 Lua）与 `spectate_ticket.go`（凭证签发），
  以及对应测试
- `go/match/internal/authority/spectator.go`（以 LOBBY 结案与重放）、`authority/schema.go` 或该包现有的迁移声明处（加 `delivery` 列）、
  `authority/spectator_native_test.go`
- `go/match/internal/metrics/metrics.go`（只追加观战相关定义与 outcome）
- `go/match/internal/server/matchinternalserver.go` 与新增 `logic/setspectatefeaturedlogic.go`（内部 RPC），serviceauth 调用方 `ops`
- 可新增运维小工具 `go/match/cmd/spectatefeatured/main.go`（签名并调用 `SetSpectateFeatured`；配置从环境变量读，不硬编码密钥）
- `go/match/internal/constants/errors.go` / `errors_test.go`（新码 `ErrSpectateTicketInvalid = uint32(table.MatchError_kMatchSpectateTicketInvalid)`，
  tip 已在 Tip.xlsx，导表后生成）

## 要做的事（按 §15.6，下面只列易错点）
1. **配置 `Spectate`**：`RelayEnabled`、`BroadcastMaxConcurrent`（默认 4）、`BroadcastAutoGateThreshold`（默认 32）、
   `TicketTTLSeconds`（默认 60）、`TicketSecret`、`OpsCallerSecret`；prod（非 dev/test）下 `RelayEnabled` 为真时 `TicketSecret`
   必须 ≥ 32 字节，否则拒启；`OpsCallerSecret` 为空时 `SetSpectateFeatured` 一律拒绝（PermissionDenied）。
2. **档位**（两条路径共用一个函数）：读 `spectate:tier:{id}`（不存在 = DIRECT）；升档用 Lua「新值大于旧值才 SET，保持 TTL」，
   只升不降；所有本场键用同一个 hash tag。
3. **L0 → L1**：L0 的 AddObserver 回 `kMatchSpectateFull`（`ErrSpectateFull`）且 `RelayEnabled` → 升档 RELAY → 本次请求继续做 L1 准入，
   不回 16021；`RelayEnabled` 关闭仍回 16021。
4. **L1 准入**：HASH `spectate:relay_gates:{id}` 里本 gate（node → instance）已记且实例相同就跳过 RPC；否则调 battle
   `AttachRelayGate`（battle 节点地址与 target_instance_id 取法与现有 AddObserver 一致），**成功后**再 HSET（先 RPC 后记账，
   并发重复调用由 battle 幂等兜底）；回包 `broadcast = true` 则把档位同步成 BROADCAST。随后判自动升档（`HLEN ≥ 阈值` 或 featured）。
   battle 回 `kEntityIsNull` 走现有懒剔除口径；其它失败回 16018 / 16004（按 A 批 `spectateRejectVerdict` 与故障口径）。
5. **L2 升档**：Lua 抢配额（清过期 → 已在集合或 `ZCARD < B` 才 ZADD，score = 期限毫秒）→ 调 `PromoteSpectateBroadcast` →
   成功则档位 BROADCAST；失败则 ZREM 退还配额、保持 RELAY；配额满不报错。L2 的观众不调任何 battle RPC，只签凭证。
6. **凭证**：`SpectateTicketPayload{battle_id, player_id, battle_instance_id, deadline_ms, expire_at_ms = now + TTL}`，
   signature = 小写 hex HMAC-SHA256(TicketSecret, payload 字节)，`spectate_ticket` = `SpectateTicket` 序列化字节；
   `battle_instance_id` / `deadline_ms` 取自 battle 回包（L1）或观战记录（L2 / 已挂接时）。
7. **全局模式**（`global_spectator.go` + `authority/spectator.go`）：L1 / L2 观众照常走 `SpectatorIntent`（幂等与顶替旧请求、
   清退玩家旧的 L0 观战复用现有取消流程），但不调 `BeginSpectator`、不锁 formation、不写 `match_spectator` 行，意图直接以
   `state=5, close_tip=0, delivery=LOBBY` 结案；L0 满员转 L1 的那次请求同样以 LOBBY 结案（不写 16021）；重放 LOBBY 结案的意图时
   重新做幂等的 L1 / L2 准入并签新凭证。`delivery` 列按该包现有迁移方式声明（项目未上线，可清库，不做兼容）。
8. **WatchBattleResponse**：L0 成功 `delivery = DIRECT`；L1 / L2 成功 `delivery = LOBBY` + `spectate_ticket`；失败 `NONE`。
9. **赛事局**：`SetSpectateFeatured`（serviceauth 调用方 `ops`，只放行本方法）：记录不存在 → NOT_FOUND；非公开 → PRIVATE；
   置位写 `spectate:featured:{id}`（TTL 同记录），并立即尝试升 L2（第 5 步）；取消只删标记、不降档。
   `ListWatchableBattles` 回填 `featured`，赛事局排最前。
10. **指标**：`match_watch_battle_total{outcome}` 加 `lobby`；新增 `match_spectate_tier_total{tier}`、
    `match_spectate_promote_total{result}`（ok / quota_full / rpc_failed），标签低基数。
11. **测试**（miniredis + 桩 battle，沿用现有夹具）：档位只升不降；L0 满员转 L1 回 LOBBY + 可验签的凭证；同 gate 第二名观众不再调
    Attach、换实例重新 Attach；Attach 失败不记账；达到阈值自动升 L2、配额满保持 L1、Promote 失败退还配额；L2 观众不调 battle；
    `RelayEnabled=false` 时满员回 16021；featured 的 NOT_FOUND / PRIVATE / 升档；凭证签名可被独立 HMAC 校验；
    全局模式（native，需要 TiDB）：LOBBY 结案与重放签新凭证、不写 `match_spectator` 行。
12. 交付说明里给 Codex：导表（新码）→ proto-gen（统一跑）→ `go/match` 下 `go build ./...`、`go vet ./...`、`go test ./... -count=1`，
    TiDB native 测试命令与环境变量，dev 库 `match -migrate` 加列；列出新增配置项与默认值。

## 2026-10-30 更新

- 「全服匹配方案讨论」第二批已落进工作区（`gather.go`、`global_match.go`、`authority/store.go`、`joinqueuelogic.go`、`challengelogic.go`、`metrics/pool.go` 等），这些段落不要碰；`config.go`、`servicecontext.go` 只追加。`spectate_visibility.go`、B 批的观战文件与本地模式 `newLocalObserverAdd` 都已在工作区。
- 随机观战不接逐级扩大匹配范围（已与该会话约定）。
- 单测的模板目录替身见 `logic/catalog_fixture_test.go`。

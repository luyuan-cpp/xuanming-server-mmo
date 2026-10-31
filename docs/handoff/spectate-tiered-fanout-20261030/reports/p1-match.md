# P1-MATCH 交付说明

**未编译、未跑测试,待 Codex 验证。** 只跑过只读的 `gofmt -l`:本任务的行无差异;`config.go`、`servicecontext.go` 的差异来自「全服匹配」会话,未动。

## 1. 改动(前缀 `go/match/`)
- 新增 `internal/logic/spectate_tier.go`(键、Lua、共用的 `spectateAdmissionTier` / `spectateDirectFullToRelay` / `admitSpectateLobby`、`promoteSpectateBroadcast`)、`spectate_ticket.go`、`setspectatefeaturedlogic.go`;`cmd/spectatefeatured/main.go`。
- `logic/watchbattlelogic.go`:判档 → L0(搬到 `admitDirectSeat`)→ 满员转 L1 → `rejectedWatch`;回包带 `delivery`。
- `logic/global_spectator.go`:`locateGlobalWatch`、`admitAndCloseGlobalLobby`、`replayGlobalLobby`、`rejectGlobalSpectatorToLobby`;`resumeGlobalSpectator` 满员转 LOBBY(后台恢复同样)。
- `logic/spectate.go`:`spectateRecordFromFormation`;`removeSpectateBattle` 还配额与赛事局索引。`logic/listwatchablebattleslogic.go`:赛事局排最前。
- `authority/spectator.go`:`delivery` 读写校验;`CloseLobbySpectator`、`RequireLobbyReplay`、`RejectSpectatorToLobby`。迁移不用另写:DDL 由 proto 生成。
- `config/config.go`(`SpectateConf`、`validateSpectate`)、`svc/servicecontext.go`(装配)、`server/matchinternalserver.go`、`metrics/metrics.go`、`constants/errors.go`。

## 2. 新增测试
- `logic/spectate_tier_test.go`:`TestWatchBattle{CorruptTierFailsClosed, DirectFullTurnsToRelayWithVerifiableTicket, AttachesEachGateInstanceOnce, ReattachesWhenRecordLostSource, AttachFailureIsNotRecorded, AttachMissingRoomEvictsIndex, AttachBroadcastReplySyncsTier, AutoPromotesAtGateThreshold, PromoteQuotaFullStaysRelay, PromoteFailureReleasesQuota, BroadcastDisabledNeverPromotes, BroadcastTierViewerCallsNoBattleRPC, BroadcastTierRecoversLostSourceOnce, DirectFullWithRelayUnusableReturnsFull, RelayDisabledIgnoresRaisedTier, DirectSeatAnswersDirectDelivery, RelayTierStillRejectsQueuedPlayer, RelayTierStillRejectsPrivateBattle}`、`TestSpectateTierOnlyRaisesAndKeepsTTL`、`TestSpectateTierKeysShareBattleHashTag`、`TestPromoteFailureKeepsQuotaHeldByAnotherRequest`、`TestClaimBroadcastQuotaExpiresStaleAndCaps`、`TestSpectateRecordFromFormationCarriesSource`、`TestRemoveSpectateBattleReleasesGlobalSetMembers`。
- `logic/spectate_ticket_test.go`(4 条,独立 HMAC 验签)、`logic/setspectatefeaturedlogic_test.go`(8 条)、`config/config_spectate_test.go`(5 条)、`cmd/spectatefeatured/main_test.go`(2 条)。
- 真 TiDB:`authority/spectator_native_test.go` 的 `TestSpectatorTiDB{LobbyCloseWritesNoOccupancyAndPinsBattle, LobbyReplayRejectsQueuedPlayer, FullRejectToLobbySurvivesCancel, LobbyAndCloseTipFirstWriteWins, CorruptDeliveryIsRejected}`;`logic/global_spectate_lobby_native_test.go` 的 `TestGlobalWatchTiDB*`(7 条)。

## 3. 给 Codex(工作目录 `go/match`,协议没再改,不用重跑 proto-gen)
1. `go build ./...`,`go vet ./...`,`go test ./... -count=1`
2. 真 TiDB(`MATCH_IT_TIDB`、`MATCH_IT_REQUIRE_TIDB=1`、`MMORPG_SQL_FIXTURE_*`):`go test ./internal/authority/... -run TestSpectatorTiDB -count=1 -v`;`go test ./internal/logic/... -run TestGlobalWatchTiDB -count=1 -v`
3. dev 库加列:`MATCH_MIGRATION_USER=… MATCH_MIGRATION_PASSWORD=… match -f etc/match_service.yaml -migrate`(`GlobalMatch.Enabled=true`),输出应含 `match_spectator_intent` 加 `delivery`;不跑则全局模式拒绝接流。

通过标准:全部 exit 0,TiDB 用例 0 SKIP。失败保留原文与 `-run <名> -v` 输出。

配置 `Spectate`(yaml 已由 P1-DEPLOY 写好):`RelayEnabled`=false、`BroadcastMaxConcurrent`=4、`BroadcastAutoGateThreshold`=32、`TicketTTLSeconds`=60、`TicketSecret`、`OpsCallerSecret`。

## 4. 需主会话确认的取舍
1. LOBBY 意图的 `original` 写一份只固定目标场的 `MatchSpectatorRecord`(无 `add_request`、无 `match_spectator` 行):随机模式重放不能换场,proto 没有别的字段存 battle_id。
2. 先准入、成功才结案。能证明的拒绝落 `close_tip`;故障时意图留 state 1,可重试。
3. 重放先复核(`RequireLobbyReplay`):否则旧请求能在已开新观战、已排队或已开战后拿到新凭证。被替代回 16016。
4. 多一个全服 ZSET `spectate:featured:active`(§15.6 没列):活跃集合按开局倒序,赛事局很快被挤出第一页,只在页内排序等于看不到。
5. `match_spectate_tier_total` 按观众准入次数计。
6. 「中继可用」= 开关开且有密钥。dev 空密钥按关闭处理,回 16021;开关关闭时无视已升档的键,回到 L0;`SetSpectateFeatured` 只写标记。
7. 配额只由新占到的请求退还,成功后无条件 ZADD:并发时最坏短暂多占 1 个名额。
8. LOBBY 回包不带 `spectator_operation_id`(全局路径原来恒回 request_id)。

## 5. 未完成项与风险
- 未改(不在范围):`docs/PROGRESS.md`、设计文档 §10.1 / §15.8。**Java 版待做**,`PARITY.md` 未登记。
- `etc/match_service.yaml` 的 `IgnoreContentMethods` 缺 `/match.MatchInternal/SetSpectateFeatured`,签名会进请求日志;文件归 P1-DEPLOY,建议补一行。
- 提前结束的 L2 场最长白占配额到 deadline(全局模式 COMPLETE 后成局仍是 state 4,battle 没有报结束的通道)。
- Promote 超时而 battle 实际已升:配额被退,短暂超 B,下一名观众自愈。
- gate 重启竞态:持旧实例的观众晚于新实例 Attach,会把中继目标改回旧实例,直到该 gate 下一名观众重新挂接。
- 每名 L1 观众多 HLEN / EXISTS(配额满时再加一次 Lua),开场风暴下的 Redis 负载未实测。
- `SetSpectateFeatured` 的 PermissionDenied 经 `PublicationAdmission` 计入 root Hold(回 gRPC 错误的入口都如此),未改。

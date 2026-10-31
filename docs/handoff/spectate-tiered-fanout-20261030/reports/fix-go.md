# FIX-GO 交付说明

**未编译、未跑测试,待 Codex 验证。** 只跑过只读的 `gofmt -l`。评审条目动手前都核对过,全部仍成立,没有跳过的。

## 1. 每条的处理

| 条目 | 处理 |
|---|---|
| S1 | `spectateTTLSeconds` 在全局模式返回 3780 秒(`catalog.MaxBattleSeconds + WorkLeaseMs/1000 + 60`),档位、HASH、赛事局标记与索引、过期分界都跟随;本地模式不变 |
| S2 | `CommitBattleOutcome` 调 `releaseSpectateOnOutcome`(`LaunchRelayTask` 里跑,不影响回执)。`removeSpectateBattle` 增删档位、HASH、赛事局标记 |
| S3 | `startGlobalSpectateMaintenance` 一分钟一轮调 `cleanupExpiredSpectateIndex`,matcher 全局分支启动;`resumeGlobalSpectator` 的 `roomMissing` 摘索引 |
| S4 / C 严重-2 | `cancelGlobalSpectator` 拿不到证明时按 `spectatorSourceGoneReason` 判 (a)(b)(c),满足就不带证明推进到 5;state 2 判定成立时不再空查 Query,直接走取消。排队、组队、新观战、后台恢复共用。判定与关闭用独立 3 秒预算,否则黑洞地址的 RPC 会耗尽预算,永远走不到 SQL |
| G1 | Attach 回 `broadcast=true` 补 ZADD(超额也记);新占者失败退还前复读档位,BROADCAST 或读不到都不退 |
| G2 | 中继可用时先 `RejectSpectatorToLobby`,再升档,升档失败只记日志;夹具 `AddObserver` 改成同 op 回首次拒绝码 |
| G3 | `promoteSpectateBroadcast` 返回拒绝码;房间不存在时 `admitSpectateLobby` 拒绝观众,两条路径走现有懒剔除。运营入口回 NOT_FOUND 并撤标记 |
| G4 | 签票前 `battleInstanceReplaced`:同 node_id 已是别的 uuid 按房间不存在处理,查不到不判,不加 RPC |
| G5 | `SET spectate:promote_try:{id} NX EX 3` 放在抢配额之前,拿不到租约不碰配额也不发 RPC,计 `throttled` |
| G6 | 未开启、验签失败、带会话来源一律回 `DENIED`,不回 gRPC 错误;工具打印排查提示并退出 1 |
| G7 | 见第 3 节 |
| G8 | 见第 2 节 |
| C 严重-1 | `applyLoadedPlayerSession` 用 `bindSent` 记下 Bind 已写进 Kafka。之前走 `PushTipToUnboundSession`(payload 的 player_id 留 0,信封照填),之后走 `PushTipToSession`。Bind 写 Kafka 报错算没发出:0 在两种情况下都能送达 |

本地观战标记规则未动,没有新增无条件删标记的调用点。

## 2. 改动文件

前缀 `go/match/`:
- `internal/logic/spectate.go`:TTL、`removeSpectateBattle`、`releaseSpectateOnOutcome`、`battleInstanceReplaced`
- `internal/logic/spectate_tier.go`:租约、`promoteSpectateBroadcast`、`recordBroadcastQuota`、`releaseBroadcastQuotaUnlessPromoted`、`admitSpectateLobby`
- `internal/logic/global_spectator.go`:`cancelGlobalSpectator`、`removeOriginalObserver`、`spectatorSourceGoneReason`、`resumeGlobalSpectator`、`startGlobalSpectateMaintenance`、`globalSpectatorIntentPurge`
- `internal/logic/setspectatefeaturedlogic.go`、`global_match.go`(一处调用)、`matcher.go`(一行启动)
- `internal/authority/spectator.go`:`created_ms` 写入、`SpectatorFormationEnded`、`PurgeClosedSpectatorIntents`
- `internal/metrics/metrics.go`:`throttled` 标签、`spectator_unproven_close_total{reason}`、`spectator_intent_purged_total`
- `cmd/spectatefeatured/main.go`:`reportReply`

`proto/match/match_authority.proto`:`MatchSpectatorIntentRecord` 加 `created_ms = 11` 与索引(任务说明第 11 条授权)。

`go/login/internal/svc/gate_command.go`:`buildPushTipToUnboundSessionCommand`、`PushTipToUnboundSession`。`go/login/internal/logic/clientplayerlogin/entergamelogic.go`:`bindSent`、`binder` 接缝、`notifyEnterGameFailed`。

新增用例:
- `logic/spectate_tier_test.go`:`TestWatchBattleAttachBroadcastReplyRecordsQuotaOverLimit`、`TestPromoteLeaseLetsOneViewerCallBattle`、`TestSpectatePromoteTryKeySharesBattleHashTag`、`TestPromoteFailureKeepsQuotaWhen{TierAlreadyBroadcast,TierUnreadable}`、`TestWatchBattlePromoteMissingRoomRejectsViewerAndEvicts`、`TestWatchBattleRejectsTicketForReplacedInstance`、`TestWatchBattleSignsTicketWhenInstanceNotInRegistry`、`TestGlobalSpectateTTLCoversLongestBattle`、`TestGlobalModeLongBattleSurvivesPastLocalTTL`、`TestReleaseSpectateOnOutcome{ClearsBattleKeys,SkipsWhenDraining}`、`TestGlobalSpectateMaintenanceLoopPrunesIndexes`、`TestGlobalSpectatorIntentPurgeKeepsCursorOnFailure`
- `logic/spectate_ticket_test.go`:`TestSpectateTicketCrossLanguageVector`
- `logic/setspectatefeaturedlogic_test.go`:`TestSetSpectateFeaturedMissingRoomRejectsAndUnmarks`
- `logic/global_spectate_lobby_native_test.go`(TiDB):`TestGlobalWatchTiDB{FullSeatConclusionSurvivesTierRaiseFailure,FullSeatReplayOfSameOpStaysLobby,MissingRoomEvictsIndex,LongBattleKeepsTierAndListingPastOldTTL,ReplacedInstanceRejectedWithoutRPC}`、`TestCommitBattleOutcomeTiDBReleasesSpectateData`、`TestGlobalSpectatorClosesWithoutProofOnlyWhenSourceGone`(6 种判据 × 3 个入口)、`TestGlobalSpectatorPrefersCancelProofWhenInstanceAlive`
- `authority/spectator_native_test.go`:`TestSpectatorFormationEnded`、`TestSpectatorTiDBIntentRetention{PurgesOnlyOldClosedNonHead,PagesPastHeadRows}`
- `cmd/spectatefeatured/main_test.go`:`TestReportReplyExitCodes`
- login:`TestPushTipFenceBeforeAndAfterBind`、`TestApplyLoadedPlayerSession_RecordsWhetherBindWasSent`

修改用例:`TestSetSpectateFeaturedRejectsBadCallerBeforeAnyWrite`(改断言 DENIED);`TestWatchBattlePromote{QuotaFullStaysRelay,FailureReleasesQuota}`、`TestSetSpectateFeaturedQuotaFullThenPromotedByViewer`(第二次尝试前快进过租约);`TestWatchBattleAttachBroadcastReplySyncsTier`(加配额断言);`TestRemoveSpectateBattleReleasesGlobalSetMembers`;`TestNotifyEnterGameFailed_*`;`TestGateCommand*`、`TestPushTipRejectsZeroPlayerID`。

**凭证固定向量**(C++ `gate_spectate_ticket_test.cpp` 补同一条):
- secret:`spectate-ticket-cross-language-vector-secret-01`
- payload 字段:battle_id=880001、player_id=7001、battle_instance_id=`5b0f3c1e-7a42-4d0e-9a51-2f6c8d9e0b17`、deadline_ms=4102444800000、expire_at_ms=1700000060000
- payload hex:`0881db3510d9361a2435623066336331652d376134322d346430652d396135312d3266366338643965306231372080b08fe6b27728e0a499ffbc31`
- signature(小写 hex 的 HMAC-SHA256):`d06fe173dcaeaf5662cfd16c6976d0e2329b52d04db87f109b7a315c5261938d`

## 3. G7 的取舍

表上没有可用的时间列:`original` 里的 `updated_ms` 进不了 SQL,未准备就关闭的行连 `original` 都没有。所以加一列 `created_ms`,按创建时刻算 7 天,不是说明写的「关闭超过 7 天」:
- 关闭点有 5 处,其中 2 处在 `spectator_exit.go`,不在允许范围;创建只有 1 个写入点;
- S4 落地后意图从创建到关闭最多约 1 小时,相对 7 天可以忽略。

清理每分钟一轮,每轮 1000 行,约 144 万行 / 天。按 `(created_ms, 主键)` 翻页,否则一直留在索引前端的请求头行会饿死后面的行。不持玩家锁:state 5 是终态,不是请求头的行不会变回请求头;DELETE 带 `created_ms<cutoff`,挡住旧 request_id 重放重新插入的行。

## 4. 给 Codex(按顺序)

1. 仓库根目录:`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。`SPECTATE_FEATURED_REJECT_DENIED` 和 `created_ms` 要靠它,不跑编译不过。
2. C++:`match_authority.pb.*` 会重生成,`proto.vcxproj` 跟下一轮 C++ 构建一起编,MSBuild `/m:1`。
3. `cd go/match`:`gofmt -l internal cmd`、`go vet ./...`、`go build ./...`、`go test ./... -count=1`。`gofmt -l` 现在会列出 7 个文件,都不是本任务造成的:`authority/pvp_team.go`、`catalog/catalog_test.go`、`config/config.go`、`logic/global_match.go`(`globalAbortPrepared` 前缺空行)、`metrics/pool.go`、`poolgroup/poolgroup.go`、`svc/servicecontext.go`。本任务只在 `global_match.go` 里加了一行调用,其余文件没动。
4. 真 TiDB(`MATCH_IT_TIDB`、`MATCH_IT_REQUIRE_TIDB=1`、`MMORPG_SQL_FIXTURE_*`),在 `go/match` 下:
   - `go test ./internal/authority/... -run 'TestSpectatorTiDB|TestSpectatorFormationEnded' -count=1 -v`
   - `go test ./internal/logic/... -run 'TestGlobalWatchTiDB|TestGlobalSpectator|TestCommitBattleOutcomeTiDB' -count=1 -v`
5. `cd go/login`:`go build ./...`、`go test ./internal/svc/... ./internal/logic/clientplayerlogin/... -count=1`。
6. dev 库:`MATCH_MIGRATION_USER=… MATCH_MIGRATION_PASSWORD=… match -f etc/match_service.yaml -migrate`,不跑则全局模式拒绝接流。迁移器只 ADD COLUMN、不补普通索引,已有库建议清库重建;不重建时清理会退化成全表扫。

通过标准:全部 exit 0,TiDB 用例 0 SKIP。失败保留 `-run <名> -v` 完整输出。

## 5. 需主会话确认或接手

确认:
1. G5 的租约放在抢配额之前,配额满的尝试也被限到 3 秒一次,名额空出后最多晚 3 秒升档。好处是不会有人抢到配额却拿不到租约、再退还。
2. G6 把「带会话来源」也改成 DENIED,比说明多一种。`sessionInterceptor` 在外层仍回 PermissionDenied(`match_service.go`,不在范围),客户端带会话调 MatchInternal 依然会记 Hold。
3. G7 按创建时刻算,见第 3 节。

接手:
- battle 侧「同 op 重放回首次拒绝码」(C++ 与 Java),C++ 向量用例。
- `go/match/internal/svc/servicecontext.go:77` 注释还写着 PermissionDenied。
- `docs/design/battle-spectate-tiered-fanout.md` §15.6(TTL、结束释放、租约、DENIED)、`docs/PROGRESS.md`。
- **Java 版待做**,`PARITY.md` 未登记,S1–S4、G1–G7 都未核对。

## 6. 残余风险

- 崩溃实例上的 L0 观众,node_id 没被复用时要等到本场权威截止 + 60 秒才能关闭,最长约 1 小时(负责人已接受这个上限)。这期间 WatchBattle 回 16004,JoinQueue 回「原观战退出尚待确认」。
- 恢复循环判定成立之前仍会 `HoldRelay`,`unknown` 不清零,崩溃过实例的副本排空证明会 HOLD。这是存量行为,说明没要求改,建议单独评估「只是未获证明」要不要计 Hold。
- 地址不通时每行的 Remove 可能耗满 3 秒,恢复循环只有 2 个在途许可,1024 行约 25 分钟;玩家自己的请求会当场关闭自己那行。
- Promote 在途时战斗结束:清理之后的 ZADD 和升档可能把成员与档位写回,配额白占到期限。窗口是毫秒级。
- 崩溃且没有终局提交的场会留在活跃集合里,最长约 63 分钟才被定期清理摘除,随机选场可能选中。
- 战斗极短时,`publishSpectateBattle` 可能晚于终局清理,这场留在活跃集合里,由观众路径懒剔除或定期清理收回。

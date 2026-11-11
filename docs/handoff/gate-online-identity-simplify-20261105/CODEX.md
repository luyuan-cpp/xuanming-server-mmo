# 在线身份简化第 1 批：Codex 执行清单（2026-11-06）

本批由 Claude 改代码，**未编译、未运行任何测试**（只对改动文件跑过 `gofmt -l`，无输出）。设计见 [gate 在线身份](../../design/gate-online-identity.md) §9.2，计划见同目录 [PLAN.md](PLAN.md)。

**两批合在一次验证里**：负责人 11-06 要求在上一批（删在线域 login Redis 回退）验收之前就落码。请先按 [上一批执行清单](../gate-online-identity-single-source-20261105/CODEX.md) 的步骤跑，再跑本文件的步骤；失败时按文末文件清单判断属于哪一批，回执写清。上一批 CODEX.md 里点名的 `original_role_interceptor_test.go`、`identity_test.go` 等已被本批删除或重写，以本批为准。第 3 批（guild 三个读方法删无会话分支，设计 §9.6 第 1 条）见第 7 节，在第 1、2 批之后执行。第 4 批（删 guild 请求体里被忽略的字段、删全服帮会榜，设计 §9.6 第 2、3 条，2026-11-10）见第 8 节，最后执行；它改了 `guild.proto` 的字段，工作区里只有这一份协议，所以第 1 节的生成就已经带上第 4 批的字段删除。

## 改了什么

- **生成器**：protogen 新增 `WriteGoClientCallableMethods`（`internal/client_callable_methods.go` 及测试），按方法级 `OptionClientRpcKind` 产出全局「客户端可发起方法」集合 `go/shared/generated/clientrpc/methods.go`；`cmd/pipeline.go` 注册；`internal/config/config.go` 加 `client_callable_methods_file`；`etc/proto_gen.yaml` 与 `etc/proto_gen.no-unity.yaml` 都配上。
- **`shared/roleidentity`**：新增 `EntryInterceptor`（在线域唯一入口准入）、`Session` / `WithSession`（唯一的会话 ctx 键）、`SessionOwnership`；`CaptureSession` 只读验签原件，不再收业务另传的声明；删除 `SessionAccount` 及其 `proto.Equal` 比对；新指标 `online_entry_rejected_total{reason}`。
- **七个在线服务**：删除各自的会话解码拦截器与手写客户端方法名单，统一装配 `EntryInterceptor`；取值函数（match `ctxkeys`、chat / friend / guild / trade 的 `session`、`domainhost.Session` / `Player`）保留名字，改读 `roleidentity.Session`。
  - match：删 `sessionInterceptor`、`original_role_interceptor.go`（及测试）；`match_service_internal_guard_test.go` 重写为按生成集合的分类契约测试；改正 5 个文件里的过时注释。
  - chat：删 `session.UnaryServerInterceptor`、`internal/session/identity.go`（及测试）；`buildUnaryInterceptors(ks, onlineEntry)`。
  - friend：`ClientIdentityInterceptor(onlineEntry, original)` 把入口准入与 SQL 预检串在同一位置；预检保留（第 2 批再删）。
  - guild：`GetPlayerGuild` 登记为唯一的匿名只读例外（`guild.go` 的 `guildAnonymousReads`）；CreateGuild / LeaveGuild / DisbandGuild / SetAnnouncement 加 `requireClientSession`，**不再按请求体身份执行无会话调用**；预检保留；`proto/guild/guild.proto` 只改注释。
  - trade：`RmbIdentity.Resolve(ctx, player)` 去掉会话参数，改用 `SessionOwnership`；`RmbIdentityResolver` 接口同步。
  - mail / rank：`domainhost.Guard(budget)` = 整请求预算 + 入口准入；`Serve` / `ServeWithReady` 去掉 `clients` 参数，relay 方法改按生成集合判定；删 mail、rank 的 `ClientMethods()`；夹具同步。

## 客户端可见的变化（如实说明）

- chat / mail / trade：客户端方法缺会话或原身份不全（LeaveGame 之后、绑定事件未到时抢发）由 in-band `kInvalidParameter` 改为入口 gRPC `PermissionDenied`，路由服把它转成 `kServiceUnavailable` 信封。match / friend / guild 原本就是 gRPC 错误。
- rank：LeaveGame 之后（会话里仍有 player_id、账号已清）原来照常受理，现在拒绝。
- guild 四个写方法不带会话：由执行改为 `PermissionDenied`。只影响非 gate 来源的调用；代码里没有这类调用方（唯一的无会话调用方是 mail 读 GetPlayerGuild，已登记为例外）。

## 1. 生成（仓库根）

```powershell
git status --short > $env:TEMP\gate-simplify-before.txt
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml
git status --short > $env:TEMP\gate-simplify-after.txt
```

通过标准：

- 新增 `go/shared/generated/clientrpc/methods.go`。另外只允许出现 `guild.proto` 带来的生成物差异：注释差异（如 `go/proto/guild/guild_grpc.pb.go`），以及第 4 批删字段带来的 `go/proto/guild/guild.pb.go`、`cpp/generated/proto/guild/guild.pb.{h,cc}` 字段差异（第 8 节第 1 步核对）。其它新差异停下报告，不要回退（可能是其他会话未提交的 proto 改动）。
- 生成物里没有 `UpdateGuildScore`、login 的 `Disconnect`，也没有任何 `Notify*`。
- 客户端可发起方法数：friend 10、guild 26、trade 35、mail 5、rank 2、chat 2、`match.MatchService` 12、`teampb.ClientPlayerTeam` 12。（`match.MatchService` 原写 10，没算上 GetBattleReport / ListBattleReports；11-06 提交的生成物里是 12。）

## 2. 静态检查（仓库根）

```powershell
rg -n 'roleidentity\.SessionAccount|CaptureSession\(ctx, |session\.ClientMethods|session\.UnaryServerInterceptor|session\.MetadataKey|domainhost\.SessionMetadata|ClientMethods\(\)|sessionInterceptor\(|originalRoleInterceptor|ctxkeys\.SessionDetailsKey' go --glob '*.go' --glob '!**/vendor/**' --glob '!go/proto/**'
rg -n '"x-session-detail-bin"' go --glob '*.go' --glob '!**/*_test.go' --glob '!go/proto/**' --glob '!**/generated/**' --glob '!**/vendor/**'
rg -n 'proto\.Equal' go/shared/roleidentity
gofmt -l go tools/proto_generator/protogen
```

通过标准：第一条只允许命中 `go/data_service/internal/server/dataserviceserver.go` 的一行注释（与本批无关）；第二条只出现在 `go/shared/callerauth/`、`go/client_rpc_router/`、`go/login/` 与 `go/match/internal/team/authority_server.go`；第三条无输出；`gofmt -l` 对本批文件无输出（其他会话的文件若有输出，只记录，不改）。

## 3. 构建、vet 与测试（串行，每个模块单独执行）

```powershell
Push-Location tools/proto_generator/protogen; go build ./... ; go vet ./... ; go test -count=1 ./... ; Pop-Location
foreach ($m in 'shared','match','chat','friend','guild','trade','mail','rank','login','client_rpc_router') {
    Push-Location "go/$m"
    go build ./... ; go vet ./... ; go test -count=1 ./...
    Pop-Location
}
pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build -GoServices match,chat,friend,guild,trade,mail,rank,login
```

- 通过标准：build / vet 无输出，test 全绿；跳过数与上一轮回执对比，新增跳过要说明。失败保留完整输出，不自动重试、不改断言。
- 重点用例：
  - protogen：`TestClientCallableMethods*`、`TestRenderClientCallableMethodsIsGofmtSource`
  - shared：`roleidentity` 的 `TestEntryInterceptor*`、`TestSessionOwnership*`、`TestWithSessionStoresACopy`
  - match：`TestMatchServicesClassifiedByGeneratedClientSet`、`TestMatchEntry*`
  - guild：`TestLegacyWritesWithoutSessionAreRefused`、`TestOnlyGetPlayerGuildAcceptsCallsWithoutSession`、`TestOnlyGetPlayerGuildIsAnonymousRead`、`TestGeneratedClientSetCoversEveryGuildRPCExceptScoreWritesAndPushes`
  - friend：`TestGeneratedClientSetMatchesFriendServiceDescriptor`、`TestInternalCallSkipsOriginalRolePrecheck`
  - trade：`TestGeneratedClientSetMatchesTradeServiceDescriptors`、`TestRmbIdentityRejectsUnsignedOrIncompleteSessionBeforeDirectoryLookup`
  - chat：`TestOnlineEntryFailsClosed`
  - mail：`TestMailRejectsUnsignedOrIncompleteSessionBeforeRepository`、`TestMailHandlerRejectsCallsWithoutCapturedOriginal`
- guild 的 `internal/logic/expected_guild_integration_test.go` 带 `//go:build integration`，其中「无会话调用被拒且不计数」改为断言拒绝、成员行保留。有 guild 集成测试环境时加 `-tags integration` 跑 `./internal/logic/...`，没有就在回执里写未跑。
- `TestTiDBRatingRankMailChainRecoversAfterCommittedReplyLoss` 用预编的 mail 夹具程序（`RANK_MAIL_CHAIN_MAIL_EXE`），必须用本批源码重编，同上一批清单「注意 1」。

## 4. 红证据（可选，隔离工作树，不动主工作区）

guild 冒名口的回归测试与修复同批落地，没有先跑红。替代做法：在 HEAD 上建一次性工作树，只放进新测试，确认它在旧代码上失败。

```powershell
git worktree add $env:TEMP\gate-simplify-red HEAD
Copy-Item go/guild/internal/logic/client_zone_test.go $env:TEMP\gate-simplify-red\go\guild\internal\logic\client_zone_test.go -Force
Push-Location $env:TEMP\gate-simplify-red\go\guild
go test -count=1 ./internal/logic -run TestLegacyWritesWithoutSessionAreRefused
Pop-Location
git worktree remove --force $env:TEMP\gate-simplify-red
```

预期：四个子用例都失败（旧代码会越过身份闸走到 nil 仓储）。回执记录输出；不论结果如何，最后都要移除这个工作树。

## 5. 本机真连接冒烟

启动方式与 Compose 项目选择同上一批清单第 4 节。

1. 机器人新账号：Login → CreatePlayer → EnterGame → 等 NotifyEnterScene → 聊天、好友、公会、邮件、聚宝斋、排行榜、组队、匹配各发一个客户端请求：全部成功。
2. LeaveGame 后不重新 Login，直接发上面的在线请求：全部被拒（包括排行榜）。
3. 邮件附件领取（mail 经 `GuildRPCDirectory` 不带会话调 guild 的 GetPlayerGuild）：照常成功。机器人没有对应用例时，以第 3 步 `go/mail` 里覆盖 `guild_directory.go` 的测试结果代替，并在回执里写明。
4. 抓 match、chat、friend、guild、trade、mail、rank 的 `/metrics`：`caller_auth_session_verify_total{result!="ok"}` 为 0；`online_entry_rejected_total` 只在第 2 步之后出现 `reason="incomplete_identity"`，不出现 `internal_with_session`、`unverified_session`。

通过标准：1、3、4 成功，2 全部被拒。回执写明账号与各步结果；不把本机冒烟写成压测或生产灰度结论。

## 6. 第 2 批：删 friend 入口预检（2026-11-06 落码，未编译）

在第 1 批之后执行（两批都改 `go/friend`，回执按文末两份文件清单分清归属）。

1. 静态检查（仓库根）：`rg -n 'CheckOriginalRole|OriginalIdentityInterceptor|ClientIdentityInterceptor' go/friend` 应无输出；`gofmt -l go/friend` 应无输出。
2. `go/friend` 下 `go build ./...`、`go vet ./...`、`go test -count=1 ./...` 全绿。重点：`internal/session` 的 `TestEntryCapturesSignedAccountAndGateRevision`、`TestMissingOrIncompleteIdentityNeverReachesBusiness`、`TestRoleFenceParticipantIsInternalOnly`；`internal/logic` 的 `TestEveryClientMethodReportsRoleNotVerifiedInBand`（18 个子用例）、`TestRecommendOnlinePathNeedsOnlyTheVerifiedOriginal`；`friend_test.go` 的拦截器链顺序用例（链长仍为 4）。
3. 有 TiDB 夹具时跑 `internal/data` 的 `TestTiDBFriendOriginalRole_*`：冻结 / 换主 / ABA 下 11 个仓储操作全部回 `ErrOriginalRole`、四张表零写入（只少了原先的预检那一项）。没有夹具就记「跳过」，不算通过。
4. 本机冒烟（接第 5 节的环境）：机器人好友全流程（加好友、同意、列表、待处理、拉黑、解除拉黑、拉黑列表、推荐、在线推荐）成功；LeaveGame 后同样请求被入口拒绝。若有手段让角色冻结（过户准备），好友写请求应带内回 `ErrRoleNotVerified`（kFeatureUnavailable），不是 `kServiceUnavailable`。
5. 回执注明：friend 入口预检已删；库故障时 friend 读写方法现在回 `ErrStorage`（故障，计数告警），不再是入口的 `Aborted`，这是设计 §9.3 记录的有意变化。不宣称性能变化（没有压测对比，AGENTS.md §6）。

第 2 批文件：`go/friend/friend.go`、`friend_test.go`、`internal/logic/friend_logic.go`、`internal/logic/role_not_verified_test.go`（新增）、`internal/data/role_identity.go`、`role_identity_test.go`、`role_identity_tidb_test.go`、`internal/session/entry_test.go`（新增）；删除 `internal/session/identity.go`、`identity_test.go`；`go/shared/serverbase/interceptor.go`（只改注释）。

## 7. 第 3 批：guild 三个读方法删无会话分支（2026-11-06 落码，2026-11-10 单测已过）

设计 §9.6 第 1 条，计划见 [PLAN.md](PLAN.md) 第 6 节。在第 1、2 批之后执行：第 1 批也改过 `go/guild/internal/logic/guild_logic.go` 与 `client_zone_test.go`，失败按本节末尾的文件清单判归属。落码时只对改动文件跑过 `gofmt -l`（无输出）。

**验证状态（2026-11-11 核对）**：Codex 2026-11-10 23:56–23:58 的发布轨道第二轮后台校验在 `go/guild` 跑了 `go build ./...`、`go vet ./...`、`go test ./...`，三个退出码都是 0（`build/verification/release-track-20261110/round2/go-background-validation/guild.{build,vet,test}.exit.txt`）。下面第 2 步列的新用例 `TestLegacyReadsWithoutSessionAreRefused`（3 个子用例）、改过的 6 个取名用例、`TestGetGuildRank_ClientSeesOnlyHomeZone` 以及「要保持绿」的各条，在同目录 `guild.test.jsonl` 里全部 pass。本节文件清单里的 7 个文件在那次运行之后都没再改；之后 `go/guild` 里再改的是 data 层与 `proto/guild/guild_db.proto`，归第 8 节和 PROGRESS 11-11「帮会积分写入不再抢全服锁」验证。第 1 步的静态检查 2026-11-11 由 Claude 复核，仍无输出。**还没跑**：第 2 步末条的 `-tags integration`（要 guild 集成环境），以及第 3 步可选的红证据。合并验证时第 2 步照常随 `go/guild` 全量重跑即可。

改了什么：

- `GetGuild` / `GetGuildRank` / `GetGuildRankByGuild` 与四个写方法一样先过 `requireClientSession`：无会话 → `PermissionDenied`，在归属查询与仓储访问之前。区一律取 `clientZone`，榜单页长一律夹到 `MaxRankPageSize`（缺省 20），请求体里的 `zone_id` 对任何调用方都忽略。`requireClientSession` 的错误文本由 `guild writes require a client session` 改为 `guild request requires a client session`（没有用例断言这段文本）。
- 删 `viewerOf`：CreateGuild / SetAnnouncement / GetGuild 直接把会话里的 player_id 传给 `guildInfoFor`。`visibleIn` 去掉「`zoneID == 0` 不按区过滤」。`expected_guild.go` 的 `bindExpectedGuild` 去掉恒真的 `who.fromClient` 判断（五个调用方都只受理客户端来源，计数结果不变）。
- 只改注释：`player_name_resolver.go`（批量取名上限已没有能超限的路径，截断只是兜底）、`guild_manage_logic.go` 的 `guildInfoFor`、`expected_guild_integration_test.go` 的一行用例说明。
- 生产行为：客户端来源的三个读方法答复不变；不带会话的调用本来在入口准入（`roleidentity.EntryInterceptor`）就被拒，现在业务层也拒。不改 proto、生成物与客户端契约，C++ 不改。

1. 静态检查（仓库根）：

   ```powershell
   rg -n 'viewerOf|visibleZone' go/guild --glob '*.go'
   rg -n -F 'zoneID == 0 ||' go/guild/internal/logic
   rg -n -F 'context.Background()' go/guild/internal/logic/guild_logic_names_test.go
   gofmt -l go/guild
   ```

   通过标准：前两条无输出；第三条只命中 `applicantViews` 与 `myApplicationViews` 两处（这两个装配函数不看会话）；`gofmt -l` 对本批文件无输出（其他会话的文件若有输出，只记录，不改）。

2. 构建、vet 与测试（`go/guild` 下，串行）：

   ```powershell
   Push-Location go/guild
   go build ./...
   go vet ./...
   go test -count=1 ./...
   Pop-Location
   ```

   通过标准：build / vet 无输出，test 全绿；失败保留完整输出，不自动重试、不改断言。重点用例（`./internal/logic`）：

   - 新增 `TestLegacyReadsWithoutSessionAreRefused`：3 个子用例，各断言 `PermissionDenied`、没查归属区、没碰仓储（仓储传 nil，碰了会 panic）。
   - 改过的 `TestGetGuildRank_ClientSeesOnlyHomeZone`：末段由「内部调用保留全服榜与原页长」改为断言无会话被拒、回包为空。
   - `guild_logic_names_test.go` 的 `TestGetGuild_FillsMemberAndLeaderNames`、`TestGetGuild_LeaderMissingFromMembersStillNamed`、`TestGetGuildRank_ResolvesLeaderNamesInOneBatch`、`TestGetGuildRankByGuild_FillsLeaderName`、`TestNamesStayEmptyWithoutResolver`、`TestGetGuild_NameLookupFailureDoesNotFailTheRead`：改成客户端来源（本区旁观者 + `fakeHomeZones`），断言没改。
   - 没改、要保持绿：`TestLegacyWritesWithoutSessionAreRefused`、`TestClientRequestWithoutHomeZoneMappingIsRefused`、`TestClientRequestHomeZoneLookupFailures`、`TestGetGuild_OtherZoneLooksNonexistentToClient`、`TestGetGuild_MergedZoneIsVisibleAndShowsTheFinalZone`、`TestGetGuildRank_ClientReadsTheFinalZoneRank`、`TestGetGuildRankByGuild_ClientOnlyRanksInHomeZone`。
   - 有 guild 集成测试环境时加 `-tags integration` 跑 `./internal/logic/...`：`TestExpectedGuildZeroKeepsLegacyBehavior` 覆盖去掉 `who.fromClient` 之后的计数（每个方法 absent 仍为 1，无会话子用例仍为 0）。没有环境就在回执里写未跑。

3. 红证据（可选，隔离工作树，不动主工作区）：HEAD 上三个读方法还有无会话分支，新用例应当失败。

   ```powershell
   git worktree add $env:TEMP\gate-simplify-red3 HEAD
   Copy-Item go/guild/internal/logic/client_zone_test.go $env:TEMP\gate-simplify-red3\go\guild\internal\logic\client_zone_test.go -Force
   Push-Location $env:TEMP\gate-simplify-red3\go\guild
   go test -count=1 ./internal/logic -run TestLegacyReadsWithoutSessionAreRefused
   Pop-Location
   git worktree remove --force $env:TEMP\gate-simplify-red3
   ```

   预期：三个子用例都失败（旧代码越过身份闸走到 nil 仓储，panic 被用例截住后 `t.Fatalf`）。不论结果如何，最后都要移除这个工作树。

4. 回执注明：三个读方法对客户端的答复不变，不宣称性能变化（没有压测对比，AGENTS.md §6）。

第 3 批文件：`go/guild/internal/logic/guild_logic.go`、`guild_manage_logic.go`、`expected_guild.go`、`player_name_resolver.go`（只改注释）、`client_zone_test.go`、`guild_logic_names_test.go`、`expected_guild_integration_test.go`（只改注释）；文档 `docs/design/gate-online-identity.md`（顶部状态与 §9.6）、本目录 `PLAN.md` 与本文件、`docs/PROGRESS.md`。

## 8. 第 4 批：删 guild 请求体里被忽略的字段与全服帮会榜（2026-11-10 落码，未编译）

设计 §9.6 第 2、3 条，计划见 [PLAN.md](PLAN.md) 第 6 节。负责人 2026-11-10：项目未上线、数据可以清空，两条都按标准做法直接删；全服榜运维和 GM 也不要（这条答复由「删除只写不读的全服帮会榜 guild_rank」会话记进设计文档）。在第 1～3 批之后执行；第 1、3 批也改过 `guild_logic.go` 与 `client_zone_test.go`，失败按本节末尾的文件清单判归属。本批只对改动的 Go 文件跑过 `gofmt -l`（无输出），没有生成协议、没有编译、没有运行任何测试。

改了什么：

- **协议（客户端契约变更）**：`proto/guild/guild.proto` 删掉 6 个请求里服务端早已不采信的字段，字段号写成 `reserved`：
  - CreateGuildRequest：`player_id = 1`、`zone_id = 3`
  - LeaveGuildRequest、DisbandGuildRequest：`player_id = 1`
  - SetAnnouncementRequest：`player_id = 2`
  - GetGuildRankRequest：`zone_id = 3`
  - GetGuildRankByGuildRequest：`zone_id = 2`

  保留 GetPlayerGuildRequest 的 `player_id`（mail 不带会话的只读调用要用）和 UpdateGuildScoreRequest 的 `zone_id`（服务端内部推分）。方法和消息号都不变。
- **guild 逻辑**：`internal/logic/guild_logic.go` 的四个写方法改为 `callerOf(ctx, 0)` 取身份，不再读请求体；其余只改注释。`go/guild/guild.go` 只改启动重建的注释。
- **全服帮会榜**：`internal/data/guild_repo.go` 删掉全服 ZSET `guild_rank` 和常量 `guildRankKey`。
  - `UpdateGuildScore` 只写分区榜，每次推分少一次 ZADD。
  - `RebuildRanks` 只重建分区榜，不再删除或改名全服键。
  - `RemoveGuildFromRank` 只从各分区榜摘除；一张分区榜都没有时直接返回。
  - `GetGuildRankPage` / `GetGuildRank` 不给区（`zoneID == 0`）时，在访问 Redis 之前回新错误 `ErrRankZoneRequired`。逻辑层的区来自 `clientZone`，不会是 0。
  - `zone_id = 0` 的帮会行是坏数据（帮会都按归属区建）：写分时分数照常落库，但不进任何榜，并打英文错误日志；重建时跳过，计数后打日志。
- **测试**：
  - `internal/data`：
    - `rank_page_test.go` 只播种分区榜，新增 `TestGuildRankReadsRequireZone`。
    - `rank_read_snapshot_test.go` 改读分区榜。
    - `rank_zone_integration_test.go`（`integration` 标签）删掉 3 处全服键断言，改用 `assertNoGlobalRank`，并新增 `TestUnzonedGuildIsPersistedButNeverRanked`。
  - `internal/logic`：`client_zone_test.go` 的请求体不再填被删字段，`seedGuild` 不再写全服键；`expected_guild_integration_test.go` 同步。
- **robot**：
  - `guild_smoke_scenario.go`：第 1 步不再故意填错区；第 4 步查别区榜时不再伪造区；第 8 步改为「B 退帮，身份只取会话」（失败步骤名 `member-leave*`）；删掉不再使用的 `tipGuildLeaderCantLeave`。
  - `main.go`、`etc/guild_smoke.yaml` 只改注释。
- 不变的部分：C++ 手写代码、Java 网关、`go/shared/generated/clientrpc/methods.go`。

1. **生成**（仓库根）。客户端可见的协议变了。负责人 2026-11-10 把「客户端代码和协议生成 编译冒烟交给codex」，所以本节用**默认配置**，它会同时写同级客户端仓 `../mmorpg-client`。单独跑本节时先跑 `proto-gen-build`。

   ```powershell
   git status --short > $env:TEMP\gate-simplify-b4-before.txt
   pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run
   Push-Location robot; go mod vendor; Pop-Location
   git status --short > $env:TEMP\gate-simplify-b4-after.txt
   rg -n 'func \(x \*(CreateGuildRequest|LeaveGuildRequest|DisbandGuildRequest|SetAnnouncementRequest)\) GetPlayerId|func \(x \*(CreateGuildRequest|GetGuildRankRequest|GetGuildRankByGuildRequest)\) GetZoneId' go/proto/guild robot/vendor/proto/guild
   rg -n 'func \(x \*GetPlayerGuildRequest\) GetPlayerId|func \(x \*UpdateGuildScoreRequest\) GetZoneId' go/proto/guild robot/vendor/proto/guild
   ```

   - robot 用 `-mod=vendor` 构建，`robot/vendor/proto` 是 `go/proto` 的副本，生成器不会补它（见 `tools/proto_generator/protogen/internal/prototools/retired_outputs.go` 的说明），所以要跑 `go mod vendor`。它会把其他会话从 10-20 以来的协议改动一并带进 vendor；这些差异只记录，不回退。
   - 通过标准：
     - 第一条 rg 无输出。
     - 第二条共命中 4 处（两个目录里 GetPlayerGuildRequest、UpdateGuildScoreRequest 各一处）。
     - `cpp/generated/proto/guild/guild.pb.h` 里这 6 个请求已没有被删字段的访问器。
     - `go/shared/generated/clientrpc/methods.go` 无差异。
     - 其它新差异按第 1 节的口径处理。

2. **客户端**（`../mmorpg-client`，代码 2026-11-11 已由 Claude 改好，Codex 只生成、编译、测试，不再改代码）：
   - 第 1 步按默认配置生成后，在客户端仓跑 `tools/client_compile_check.ps1`，应无错误。
   - Unity EditMode：`MmorpgClient.Tests.EditMode.Net.GameClientReconnectTests` 全类（新增 `RecoveryRetriesPastAStoppedGateWithinTheBudget`、`DefinitiveAssignGateResultEndsRecoveryWithoutRetry`，原有用例不应变化）与 `MmorpgClient.Tests.EditMode.Guild` 全部（`GuildUiTests.ZoneAndNameRejectionsKeepTheEmptyStateWithReadableMessages` 已改为不带区）。有本机栈时跑 PlayMode GuildNetwork 冒烟（`MMORPG_GUILD_SMOKE_ZONE` 照旧）。
   - 改动文件：`Assets/Scripts/Game/Guild/GuildClient.cs`（帮会榜 / 建帮不带区与身份，公告去 `PlayerId`；退帮、解散、任免、踢人、转让带 `ExpectedGuildId`，退帮与解散被拒走 `AcceptWrite` 排队重拉）、`Assets/Scripts/UI/Ugui/Guild/GuildUiRoot.cs`、`Assets/Scripts/Game/GameClient.cs`（`ConnectAndEnter` 记录本连接 gate 已接受票据）、`Assets/Scripts/Game/GameClient.Reconnect.cs`（gate 断线重连在 20 秒期限内对还没连上新 gate 的暂时故障退避重试）、`Assets/Tests/EditMode/Guild/GuildUiTests.cs`、`Assets/Tests/PlayMode/GuildNetwork/GuildEconomyNetworkSmokeTests.cs`（只改第 112 行）、`Assets/Tests/EditMode/Net/GameClientReconnectTests.cs`。`GuildClient.cs` 与两份帮会测试里另有「帮会恢复结论」会话未提交的改动，不属本批；失败时按这份清单判归属。
   - 客户端没有「全服帮会榜」入口，不用删界面。

3. **静态检查**（仓库根）：

   ```powershell
   rg -n -w 'guildRankKey' go/guild
   rg -n -F '"guild_rank"' go/guild robot tools
   rg -n 'CreateGuildRequest\{[^}]*(PlayerId|ZoneId)|GetGuildRank(ByGuild)?Request\{[^}]*ZoneId|(LeaveGuild|DisbandGuild|SetAnnouncement)Request\{[^}]*PlayerId' go robot --glob '*.go' --glob '!**/vendor/**' --glob '!go/proto/**'
   gofmt -l go/guild robot/guild_smoke_scenario.go robot/main.go
   ```

   通过标准：
   - 第一、三条无输出。
   - 第二条只命中 `rank_zone_integration_test.go` 的 `assertNoGlobalRank`。
   - `gofmt -l` 只列出本批之前就不干净的 4 个文件，只记录，不改：
     - `go/guild/internal/data/economy_repo_test.go`
     - `go/guild/internal/data/trial_launch_repo.go`
     - `go/guild/internal/data/trial_lobby_sql.go`
     - `robot/guild_smoke_scenario.go`：HEAD 上就有 3 处 `//(` 注释行差异。

4. **构建、vet 与测试**（串行）：

   ```powershell
   Push-Location go/guild
   go build ./...
   go vet ./...
   go vet -tags integration ./...
   go test -count=1 ./...
   Pop-Location
   Push-Location go/mail; go build ./... ; go vet ./... ; Pop-Location
   Push-Location robot; go build ./... ; go vet ./... ; Pop-Location
   msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64
   pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build -GoServices guild,mail
   ```

   - `go/mail` 经 `GuildRPCDirectory` 引用 guild 协议，所以要一起编。C++ 只需编译通过，因为手写代码不引用被删字段，不需要跑 C++ 测试；必须串行 `/m:1`。
   - 通过标准：build / vet 无输出，test 全绿，跳过数与上一轮回执对比。失败保留完整输出，不自动重试，不改断言。
   - 重点用例：
     - `internal/data`：
       - 新增的 `TestGuildRankReadsRequireZone`：不给区时两条读都回 `ErrRankZoneRequired`，且 miniredis 命令计数为 0，说明拒绝发生在访问 Redis 之前。
       - 以下用例断言没改，只是改读分区榜：`TestGetGuildRankPageHandlesPaginationBoundaries`、`TestGetGuildRankPageUsesOneSnapshotDuringRemoval`、`TestGetGuildRankUsesOneSnapshotDuringScoreChange`、`TestGetGuildRankUsesOneSnapshotDuringRemoval`。
     - `internal/logic`：以下用例只改了请求体，断言没改：`TestCreateGuild_ClientIdentityAndZoneComeFromServer`、`TestLegacyWritesWithoutSessionAreRefused`、`TestLegacyReadsWithoutSessionAreRefused`、`TestGetGuildRank_ClientSeesOnlyHomeZone`、`TestGetGuildRankByGuild_ClientOnlyRanksInHomeZone`。
   - 有 MySQL 时加 `-tags integration` 跑。用例会建一次性库，用 `GUILD_IT_MYSQL_DSN` 指向能建库的账号；自己起一个一次性 MySQL，别借 `codex-*` 容器。

     ```powershell
     Push-Location go/guild
     go test -count=1 -tags integration ./internal/data/... ./internal/logic/...
     Pop-Location
     ```

     - 重点：
       - `TestUpdateGuildScore_WritesTheFinalZoneRank`、`TestRebuildRanks_FollowsTheCurrentMapping`：都加了 `assertNoGlobalRank`。
       - 新增的 `TestUnzonedGuildIsPersistedButNeverRanked`：zone 0 的帮会分数落库、不进任何榜；重建后只剩正常帮会所在区的榜键。（2026-11-11 补注：帮会积分写入改成带版本的条件写入之后，重建后还多一个同区的版本 HASH `guild_rank:version:30`，这条断言已同步改成这两个键；改动与验收见 `docs/PROGRESS.md` 2026-11-11「帮会积分写入不再抢全服锁」。）
       - 原有的 `TestRemoveGuildFromRank_*`、`TestDisbandPath_*`。
     - MySQL 不可达时这些用例会 Skip。Skip 不算通过，回执写「未跑」。

5. **红证据**：本批不做。新用例引用的 `ErrRankZoneRequired` 在 HEAD 上不存在，放到 HEAD 上只会编译失败，不构成行为上的红证据。替代验证有两条：
   - `TestGuildRankReadsRequireZone` 的命令计数断言，证明拒绝发生在访问 Redis 之前；
   - 集成用例的 `assertNoGlobalRank`，证明写分和重建都不再建全服键。

6. **本机冒烟**（接第 5 节的环境，guild、gate、路由服先换上本批二进制）：
   1. 机器人公会冒烟：在 `robot/` 下先 `go build -o robot.exe .`，再 `.\robot.exe -c etc/guild_smoke.yaml`，前置条件见该文件头。通过标准：退出码 0，日志里有 `GUILD_SMOKE_OK`，以及第 8 步的 `member left the guild using the session identity`。
   2. 在帮会用的 Redis（见 `go/guild/etc/guild.yaml`）上执行 `EXISTS guild_rank`，应为 0。旧环境如果残留这个键（本批起没人再写，也没人删），先手工 `DEL guild_rank` 一次再跑。
   3. Unity 客户端按新生成物编出来以后，手动建帮、退帮、解散、改公告、看本区榜各一次，都应成功。没有客户端环境就写「未跑」。

7. **回执注明**：
   - 客户端契约变了：6 个请求删了字段。
   - 全服帮会榜已删：Redis 上少一个键，每次写分少一次 ZADD。
   - 不宣称性能变化，因为没有压测对比（AGENTS.md §6）。

第 4 批文件：

- 协议：`proto/guild/guild.proto`
- go/guild：`guild.go`（只改注释）、`internal/data/guild_repo.go`、`rank_page_test.go`、`rank_read_snapshot_test.go`、`rank_zone_integration_test.go`、`internal/logic/guild_logic.go`、`client_zone_test.go`、`expected_guild_integration_test.go`
- robot：`guild_smoke_scenario.go`、`main.go`（只改注释）、`etc/guild_smoke.yaml`（只改注释）
- 文档：`docs/design/gate-online-identity.md`（顶部状态、§8、§9 标题、§9.6 第 2 条）、本目录 `PLAN.md` 第 6 节第 2 条与本文件、`docs/PROGRESS.md`，以及 Java 仓 `PARITY.md` 的 TODO
- 以下由「删除只写不读的全服帮会榜 guild_rank」会话改：§9.6 第 3 条、PLAN.md 第 6 节第 3 条、帮会排行三份架构文档、`guild_friend_service_notes_zh.md`、`leaderboard-system.md`

## 9. §7 批 1：gate 改 Ed25519 签名、验证方只持公钥（2026-11-11 落码，未编译）

设计见 [gate 在线身份](../../design/gate-online-identity.md) §3、§4、§10.1。负责人 2026-11-11 要求开始处理 §7 的已知边界，并定了「没做完就按最标准的方式做」。项目未上线，gate 与全部验证方同批换算法，不留混跑窗口。本批没有编译，也没有运行任何测试，只对改动的 Go 文件跑过 `gofmt -l`（无输出）。

改了什么：

- **算法**：会话身份签名由 HMAC-SHA256（v1）换成 Ed25519（v2，RFC 8032），canonical 第一行改为 `mmorpg-gate-identity/v2`。签名头名称、±30s 时间窗、nonce 表都不变。
- **gate 只持私钥**：
  - `cpp/libs/engine/core/security/caller_auth.h` 新增 `InitProcessSigner`：启动时解析种子，核对公钥列表里有与它配对的那一把；
  - 旧的 HMAC `Sign` 与 `kSecretEnv` 删除，`Append` 去掉密钥参数；
  - `cpp/nodes/gate/main.cpp` 换成新的启动检查；
  - `client_session_handler.cpp` 三处、`router_muduo_forward.cpp` 一处签名调用，去掉 `std::getenv` 参数。
- **验证方只持公钥**：
  - `go/shared/callerauth` 新增 `keys.go`（`KeySet`、`PublicKeys`、`ParsePublicKeys`、`Ed25519Signer`），验签选项由 `Secrets` 改名为 `Keys`；
  - `GateInterceptor` 读 `MMORPG_INTERNAL_AUTH_PUBLIC_KEYS`，缺失或格式不对时拒启；
  - `callerauthtest.Key` 换成固定种子的 Ed25519 测试密钥，对外用法不变；
  - `go/mail/flow_integration_test.go` 跟着改了一个字段名。
- **login**：
  - 删 `Secrets.InternalAuth`，涉及 `internal/config/secrets.go`、`etc/login.yaml`、`internal/svc/servicecontext.go`、`internal/config/auth_providers_test.go`；
  - `login.go` 的 `newSessionInterceptor` 改读公钥，缺失时启动报错。
- **新工具**：`go/shared/cmd/identitykeygen`，含单测。
- **部署**：
  - 私钥只进 Secret `gate-identity-auth`（键 `signing-key`），只有 gate 引用；
  - 公钥列表明文注入 gate、login 与 7 个在线服务；
  - 改动的文件：`tools/scripts/k8s_deploy.ps1`、`lib/release_common.ps1`（新增 `Assert-InternalIdentityKeyShape`）、`lib/k8s_client_entry.ps1`、`lib/gate_identity_environment.ps1`、`cpp_nodes.ps1`（gate 一处加 `-IncludeSigningKey`）、`deploy-staging.sh`、`deploy/debug/compose.game.yml`、`prepare_docker_debug.py`；
  - dev 档、本机、docker 调试统一用 RFC 8032 §7.1 TEST 1 的公开测试密钥对。

1. **静态检查**（仓库根）：

   ```powershell
   rg -n 'MMORPG_INTERNAL_AUTH_SECRET|INTERNAL_AUTH_SECRET|SecretVerifier|InternalAuthSecret\b|mmorpg-login-caller-auth/v1' go cpp tools deploy --glob '!**/vendor/**' --glob '!**/generated/**'
   rg -n 'caller_auth::Sign\(|kSecretEnv' cpp/nodes/gate cpp/libs/engine/core/security cpp/tests/routing_identity_test
   gofmt -l go/shared go/login go/mail
   ```

   通过标准：
   - 第一条无输出；只允许 `prepare_docker_debug.py` 里 `environment.pop("INTERNAL_AUTH_SECRET", None)` 这一处，它清理的是旧 `.env`。
   - 第二条无输出。
   - `gofmt -l` 对本批文件无输出；其他会话的文件若有输出，只记录，不改。

2. **Go 构建、vet 与测试**（串行，每个模块单独执行）：

   ```powershell
   Push-Location go/shared
   go build ./... ; go vet ./...
   go test -count=1 ./callerauth/... ./cmd/identitykeygen/... ./roleidentity/...
   go test -run '^$' -bench BenchmarkPublicKeysVerify -benchmem ./callerauth
   Pop-Location
   foreach ($m in 'login','chat','friend','guild','trade','mail','rank','match') {
       Push-Location "go/$m"; go build ./... ; go vet ./... ; go test -count=1 ./... ; Pop-Location
   }
   ```

   - 通过标准：build / vet 无输出，test 全绿。失败保留完整输出，不自动重试，不改断言。
   - 重点用例：
     - `callerauth`：`TestCanonicalV2Vector`、`TestEd25519Rfc8032Vector`、`TestMissingPublicKeysRefusesStartup`、`TestMalformedPublicKeysRefuseStartup`（6 个子用例）、`TestPublicKeysRejectWrongLengthSignature`、`TestZeroSignerRefusesToSign`；
     - `verifier_test.go` 全部，其中 `TestVerify_轮换期新旧密钥都认` 覆盖新旧公钥并列；
     - `identitykeygen` 三个用例；
     - login 的 `TestResolveSecretsIncludesAuthProviders`；
     - 各服务里用 `callerauthtest` 的用例，签名已换成 Ed25519，断言没改。
   - 基准：回执记录 `BenchmarkPublicKeysVerify` 的 ns/op、B/op、allocs/op 和机器型号，只记数，不下性能结论（AGENTS.md §6）。
   - 有 MySQL 时，mail 的 `flow_integration_test.go` 与 match 的 `rank_mail_chain_integration_test.go` 按它们原有的口径跑；没有环境就写「未跑」。

3. **C++**（串行 `/m:1`）：

   ```powershell
   msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64
   pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter routing_identity_test
   ```

   - 通过标准：0 错误；`GateAuthenticatedIdentity.CanonicalMatchesGoVector`、`Ed25519MatchesRfc8032Vector`、`SignerRefusesUnpairedOrMalformedKeys` 全过，其余用例不回退。
   - `caller_auth.h` 用到的 `EVP_PKEY_new_raw_private_key`、`EVP_PKEY_get_raw_public_key`、`EVP_DigestSign`、`OPENSSL_cleanse` 在 OpenSSL 3 与 BoringSSL 里都有。若 gate 链接的那一版报找不到声明或 const 不匹配，保留首条错误报告，不要换算法。

4. **部署脚本契约测试**（pwsh 7，仓库根）：

   ```powershell
   foreach ($t in 'gate_identity_environment','go_services_ports','k8s_client_entry_contract','k8s_deploy_contract','k8s_redis_split_contract','logical_db_deploy_contract','redis_player_cluster_contract','lobby_authority_render','k8s_migrate_gate') {
       pwsh -NoProfile -File "tools/scripts/tests/$t.tests.ps1"
   }
   python -m py_compile tools/scripts/prepare_docker_debug.py
   ```

   - 通过标准：全部通过。
   - `k8s_migrate_gate` 本批没改，但它断言 `gate-identity-auth` 恰好建一次。现在只有 Apply-Zone 建它，Go 服务不再引用它，所以应当仍是一次。
   - 失败按用例名报告。

5. **本机冒烟**（dev 档）：
   1. `cpp_nodes.ps1` 起 gate、`go_services.ps1` 起 Go 服务。gate 不打印 `Internal session signing key rejected`；login 与在线服务不因 `MMORPG_INTERNAL_AUTH_PUBLIC_KEYS` 拒启。
   2. 机器人 Login → EnterGame → 聊天、好友、公会、邮件、排行榜各发一个请求，全部成功；各服务 `caller_auth_session_verify_total{result!="ok"}` 为 0。
   3. 负向：先把 `MMORPG_INTERNAL_AUTH_SIGNING_KEY` 与 `MMORPG_INTERNAL_AUTH_PUBLIC_KEYS` 一起显式注入，公钥列表只放另一把 64 位十六进制公钥（例如 `'a' * 64`），再单独起 gate。gate 应拒启，日志带 `does not list the public key`。只设公钥列表、不设私钥时，本机启动器会先以「两者必须一起注入」拒绝，走不到 gate 自己的配对检查。
   4. `k8s_deploy.ps1` dev 档 `-DryRun`：gate 的 env 有 `MMORPG_INTERNAL_AUTH_SIGNING_KEY` 的 secretKeyRef（Secret `gate-identity-auth`、键 `signing-key`），以及明文的 `MMORPG_INTERNAL_AUTH_PUBLIC_KEYS`；8 个验证方只有明文公钥，没有任何 `gate-identity-auth` 引用；login ConfigMap 没有 `Secrets.InternalAuth`。

6. **回执注明**：签名算法已换，验证方只持公钥；性能只有基准数，没有压测对比；Java 只登记待核对。

第 9 批文件：

- C++：`cpp/libs/engine/core/security/caller_auth.h`、`cpp/nodes/gate/main.cpp`、`cpp/nodes/gate/handler/rpc/client_session_handler.cpp`、`router_muduo_forward.cpp`、`cpp/tests/routing_identity_test/session_details_builder_test.cpp`
- Go：
  - `go/shared/callerauth/{callerauth,keys,session,verifier}.go`
  - `go/shared/callerauth/{session_test,verifier_test}.go`
  - `go/shared/callerauth/callerauthtest/callerauthtest.go`
  - `go/shared/cmd/identitykeygen/{main,main_test}.go`（新增）
  - `go/mail/flow_integration_test.go`
  - `go/login/login.go`、`internal/config/secrets.go`、`internal/config/auth_providers_test.go`、`internal/svc/servicecontext.go`、`etc/login.yaml`
- 部署：
  - `tools/scripts/k8s_deploy.ps1`、`tools/scripts/lib/{release_common,k8s_client_entry,gate_identity_environment}.ps1`、`tools/scripts/cpp_nodes.ps1`、`tools/scripts/deploy-staging.sh`、`tools/scripts/prepare_docker_debug.py`、`deploy/debug/compose.game.yml`
  - 测试：`tools/scripts/tests/{gate_identity_environment,go_services_ports,k8s_client_entry_contract,k8s_deploy_contract,k8s_redis_split_contract,logical_db_deploy_contract,redis_player_cluster_contract,lobby_authority_render}.tests.ps1`
- 文档：
  - `docs/design/gate-online-identity.md`（顶部状态、§3、§4、§6、§7.4、§8、§10）
  - `deploy/k8s/README.md`
  - `docs/ops/k8s-open-server-runbook{,_zh,_en}.md`、`docs/ops/linux-staging-stress-runbook.md`
  - `docs/design/workload-secrets-via-k8s-secret.md`、`docs/design/login-queue-2026-05.md`
  - 本文件、`docs/PROGRESS.md`
  - Java 仓 `PARITY.md`

## 回执与范围

- 回执写到同目录 `codex-results.md`：每步命令、退出码、通过与否；失败附首条错误与日志摘要，并注明属于上一批、第 1～4 批中的哪一批，还是第 9 节的 §7 批 1（Ed25519 签名）。第 9 节不改协议，不需要重新生成；它改了 C++ gate，要整解重编。
- 第 1～3 批：C++ 不改，不需要 C++ 编译；客户端不改、不重编；Java 版不适用（设计 §8）。生成步骤若产生 `cpp/generated` 差异，除第 4 批的 `cpp/generated/proto/guild/guild.pb.{h,cc}` 之外，停下报告。（2026-11-11 补注：`proto/guild/guild_db.proto` 加了 `GuildRecord.score_version = 12`，所以 `cpp/generated/proto/guild/guild_db.pb.{h,cc}` 与 `go/proto/guild/guild_db.pb.go` 的差异也是预期的，见 `docs/PROGRESS.md` 2026-11-11「帮会积分写入不再抢全服锁」。）
- 第 4 批：C++ 手写代码不改，但 guild 的 C++ 生成物会变，要重编 C++；客户端要按新协议重新生成并重编（第 8 节）。Java 版本轮不写代码，只在 Java 仓 `PARITY.md` 记 TODO（设计 §8）。

## 文件（本批）

- 生成器：`tools/proto_generator/protogen/internal/client_callable_methods.go`、`client_callable_methods_test.go`（新增），`internal/config/config.go`、`cmd/pipeline.go`、`etc/proto_gen.yaml`、`etc/proto_gen.no-unity.yaml`
- shared：`roleidentity/session.go`、`session_test.go`，`domainhost/host.go`，`callerauth/callerauth.go`、`callerauth/callerauthtest/callerauthtest.go`（只改注释）
- match：`match_service.go`、`match_service_internal_guard_test.go`、`internal/pkg/ctxkeys/ctxkeys.go`；删除 `original_role_interceptor.go`、`original_role_interceptor_test.go`；注释：`internal/logic/activitybattlelogic.go`、`internal/logic/queue.go`、`internal/logic/session_identity_test.go`、`internal/server/matchinternalserver.go`、`internal/team/server.go`
- chat：`chat.go`、`chat_test.go`、`original_actor_chain_test.go`、`internal/session/session.go`；删除 `internal/session/identity.go`、`identity_test.go`
- friend：`friend.go`、`friend_test.go`、`internal/session/session.go`、`session_test.go`、`identity.go`、`identity_test.go`
- guild：`guild.go`、`guild_test.go`、`internal/session/session.go`、`session_test.go`、`identity.go`、`identity_test.go`、`internal/logic/guild_logic.go`、`internal/logic/client_zone_test.go`、`internal/logic/expected_guild_integration_test.go`、`internal/server/guild_internal_server.go`（注释）；`proto/guild/guild.proto`（只改注释）
- trade：`trade.go`、`trade_test.go`、`internal/session/session.go`、`session_test.go`、`internal/svc/role_trade.go`、`role_trade_test.go`、`internal/logic/rmb_logic.go`
- mail：`mail.go`、`internal/server/server.go`、`session_identity_test.go`、`flow_integration_test.go`、`rank_mail_chain_fixture_test.go`
- rank：`rank.go`、`internal/server/server.go`、`rank_mail_chain_fixture_test.go`

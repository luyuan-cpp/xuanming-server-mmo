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

## 7. 第 3 批：guild 三个读方法删无会话分支（2026-11-06 落码，未编译）

设计 §9.6 第 1 条，计划见 [PLAN.md](PLAN.md) 第 6 节。在第 1、2 批之后执行：第 1 批也改过 `go/guild/internal/logic/guild_logic.go` 与 `client_zone_test.go`，失败按本节末尾的文件清单判归属。本批只对改动文件跑过 `gofmt -l`（无输出），没有编译、没有运行任何测试。

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

## 回执与范围

- 回执写到同目录 `codex-results.md`：每步命令、退出码、通过与否；失败附首条错误与日志摘要，并注明属于上一批还是第 1～4 批中的哪一批。
- 第 1～3 批：C++ 不改，不需要 C++ 编译；客户端不改、不重编；Java 版不适用（设计 §8）。生成步骤若产生 `cpp/generated` 差异，除第 4 批的 `cpp/generated/proto/guild/guild.pb.{h,cc}` 之外，停下报告。
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

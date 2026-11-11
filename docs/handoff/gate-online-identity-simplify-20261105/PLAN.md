# Gate 在线身份简化：落码计划与 Codex 验收清单（2026-11-05）

**状态：第 1、2 批均已于 2026-11-06 落码，未编译、未测试，Codex 按同目录 [CODEX.md](CODEX.md) 执行（与上一批合并验证，第 2 批验收见 CODEX.md 第 6 节）。剩余项见本文第 6 节：第 1 条（第 3 批）2026-11-06 已落码，`go/guild` 的 build / vet / test 2026-11-10 已由 Codex 跑通，只差集成用例与可选红证据（CODEX.md 第 7 节开头）；第 2、3 条（第 4 批）2026-11-10 已落码、未编译，验收见 CODEX.md 第 8 节。** 设计与理由见 [gate 在线身份](../../design/gate-online-identity.md) §9，缺口见同文 §7.5。

## 1. 前置条件

1. Codex 已按 [上一批执行清单](../gate-online-identity-single-source-20261105/CODEX.md) 验完「删除在线域 login Redis 回退」，并写好回执。两批改的是同一批文件，不能混在一次验证里。
2. 负责人授权：两批合计预计 30 个以上文件（含测试与生成器），按 AGENTS.md §10.2 落码前要授权。**2026-11-05 负责人已授权第 1 批，条件是第 1 条完成**；第 2 批在第 1 批验收后再确认。截至授权时上一批还没有 Codex 回执（`docs/PROGRESS.md` 里 Codex 当天的记录写明「不代表主树在途身份迁移已通过验证」），所以第 1 批暂缓。第 2.1 节的测试也要等上一批验完再加：它们在现有代码上有红用例，提前加会让上一批的全量 `go test` 变红。**2026-11-06 负责人改为「现在就做」**：第 1 批在上一批验收前落码，测试与实现同批落地，两批合在一次验证里；红证据改用隔离工作树补（CODEX.md 第 4 节）。
3. 工作区有其他会话的并行修改。落码前对每个要改的文件看一次 `git diff`，只动本文列出的范围。

## 2. 落码步骤

### 2.1 第 1 批 a：先写回归测试（只加测试，不改实现）

测试一律用 `go/shared/callerauth/callerauthtest` 造签名会话（`Incoming`）与未签名会话（`Unverified`）。在现有代码上的预期：

| 测试 | 断言 | 现有代码上 |
|---|---|---|
| guild：不带会话调 CreateGuild / LeaveGuild / DisbandGuild / SetAnnouncement，请求体填一个已存在角色的 player_id | 拒绝，库里没有变化 | **红**（现在会执行） |
| guild：不带会话调 GetPlayerGuild | 正常返回 | 绿 |
| rank：带验签会话调 GetRankPage，会话是 LeaveGame 之后的形状（player_id 非 0、账号空、修订 0） | 拒绝 | **红**（现在只看 player_id） |
| match / chat / friend / guild / trade / mail：同一形状的会话调各自一个写方法 | 拒绝 | 绿 |
| 各服务的读方法（如 trade 的 BrowseListings）：同一形状的会话 | 拒绝 | 不确定：有的读方法现在只看 player_id。回执逐个记录，不作为停止条件；第 1 批后必须全部拒绝 |
| match / friend / guild / trade / mail / rank：带验签会话调本进程注册的非客户端方法（RoleFenceParticipant、各 `*Admin`、GuildInternal、MatchInternal 等；chat 只注册了客户端服务） | 拒绝（只断言被拒，不断言具体码） | 绿 |

Codex 跑一次，回执逐条记红 / 绿；与上表不符就停下报告，不改断言。

### 2.2 第 1 批 b：实现

1. **生成器**（`tools/proto_generator/protogen/internal/`）：在 `route_table.go` 旁新增一个生成函数，复用同一份服务收集与 `client_rpc_kind.go` 的方法形态读取。集合规则：服务标了 `OptionIsClientProtocolService`，且方法形态为 REQUEST 或 ONE_WAY。`internal/config/config.go` 加输出路径键（命名参照 `ClientRpcRouterRouteTableFile`），`etc/proto_gen.yaml` 与 `etc/proto_gen.no-unity.yaml` 都配上，输出 `go/shared/generated/clientrpc/`，对外只暴露 `Callable(fullMethod string) bool`。生成器单测：PUSH、SERVER_ONLY、非客户端服务不进集合；REQUEST、ONE_WAY 进集合。路由表不改，路由服不用这个集合（设计 §9.2 第 1 条）。
2. **共享入口拦截器**（`go/shared/roleidentity`）：唯一参数是只读例外名单，规则见设计 §9.2 规则表。`SessionAccount` / `CaptureSession` 改为只读验签原件，不再收 `claimed`。表驱动单测覆盖规则表每一格，外加「有会话头、ctx 里没有验签原件」→ Unauthenticated。
3. **各服务**：每个服务把共享拦截器装在原会话解码拦截器的位置，测试改用 `callerauthtest`。

   | 服务 | 改 | 删 |
   |---|---|---|
   | match | `match_service.go` 装配；`internal/pkg/ctxkeys` 的取值改读原件；改正 `sessionInterceptor` 与 `internal/logic/queue.go` 的过时注释 | `sessionInterceptor` 的解码段；`original_role_interceptor.go` 及其测试 |
   | chat | `chat.go` 装配；`internal/session/session.go` 的取值函数改读原件 | `session.UnaryServerInterceptor`；`identity.go` 的 `OriginalIdentityInterceptor` |
   | friend | `friend.go` 装配；`internal/session/session.go` 取值函数改读原件；`identity.go` 只留 SQL 预检（第 2 批再删） | `ClientMethods`、`UnaryServerInterceptor`、`ClientIdentityInterceptor` |
   | guild | `guild.go` 装配并登记 GetPlayerGuild 为只读例外；`internal/session/session.go`；`internal/logic/guild_logic.go` 的 `callerOf` 删请求体回退 | `ClientMethods`、`UnaryServerInterceptor`；`identity.go` 只留 SQL 预检 |
   | trade | `trade.go` 装配；`internal/session/session.go`；`internal/svc/role_trade.go` 改读原件 | `ClientMethods`、`UnaryServerInterceptor` |
   | mail / rank | `go/shared/domainhost/host.go`：`Guard` 换成共享拦截器，`Session` / `Player` 改读原件；`go/mail/internal/server/server.go` 的 `actor` 简化 | mail 的 `ClientMethods()`；rank `internal/server/server.go` 的名单 |

4. **文档**：落码后把设计 §1「取原账号」一行、§6 与 §7.5 改成落地后的事实，§9.2 标已落码；`docs/PROGRESS.md` 追加一条。

### 2.3 第 2 批：删 friend 入口预检（2026-11-06 已落码，未编译）

- `go/friend/internal/session/identity.go` 删 `OriginalIdentityInterceptor` 里的 SQL 预检，`friend.go` 去掉 `CheckOriginalRole` 的装配，`internal/data/role_identity.go` 删 `CheckOriginalRole`（先确认没有别的调用方）。
- 测试：friend 现有的冻结 / 换主用例全部保留；凡是断言入口回 PermissionDenied / Aborted 的，改为断言业务回 `ErrRoleNotVerified`，逐个核对，不放宽。新增一条：RecommendFriends 的在线玩家路径只要求验签原件。
- 文档：设计 §6 删去 friend 预检那一句，§9.3 标已落码。

落码结果：`identity.go` 两个函数都只为预检存在，整文件删除，拦截器链第 ③ 位直接是共享入口准入；原 `identity_test.go` 的入口用例移到新文件 `internal/session/entry_test.go`，预检用例删除，业务侧拒绝改由新增的 `internal/logic/role_not_verified_test.go` 逐方法钉住（9 个读写方法 × 冻结换主 / 目录读不到 → `ErrRoleNotVerified`，外加在线推荐不调仓储）。TiDB 用例去掉预检那一项，其余 11 个仓储操作不变。逐项推演见设计 §9.3。

## 3. Codex 验收：第 1 批

第 1 批已落码，验收步骤以同目录 [CODEX.md](CODEX.md) 为准（含生成、静态检查、构建测试、隔离工作树补红证据与本机冒烟，并与上一批合并验证）。这里原先的草案已并入 CODEX.md，不再单独维护。

## 4. Codex 验收：第 2 批

已并入 [CODEX.md](CODEX.md) 第 6 节，以那里为准。

## 5. 回执与范围

- 回执写到同目录 `codex-results.md`：每步命令、退出码、通过与否；失败附首条错误与日志摘要。
- C++ 不改，不需要 C++ 编译；生成步骤若产生 `cpp/generated` 差异，停下报告。
- 客户端不改、不重编。Java 版不适用（设计 §8）。

## 6. 剩余

1. **已落码（第 3 批，2026-11-06；`go/guild` 单测 2026-11-10 已由 Codex 跑通，集成用例未跑）**：guild 的 GetGuild、GetGuildRank、GetGuildRankByGuild 删掉无会话分支，与四个写方法一样先过 `requireClientSession`，区一律取 `clientZone`，榜单页长夹到 `MaxRankPageSize`；随之删 `viewerOf`、`visibleIn` 的 `zoneID==0` 口径与 `expected_guild.go` 里恒真的「只计客户端来源」判断。`guild_logic_names_test.go` 6 处改成客户端来源（`clientCtx` + `fakeHomeZones`）；请求者用本区旁观者，`guildInfoFor` 不给他现算待审数，所以没有加待审数替身，也没有为测试新开接缝。`client_zone_test.go`「内部调用保留全服榜」改为断言无会话被拒，新增 `TestLegacyReadsWithoutSessionAreRefused`。文件清单与验收见 CODEX.md 第 7 节（设计 §9.6 第 1 条）。
2. **已落码（第 4 批，2026-11-10，未编译）**：guild 请求体里服务端早已不采信的字段从 `guild.proto` 删掉，字段号写成 `reserved`，涉及六个请求：
   - CreateGuild 的 `player_id` / `zone_id`；
   - LeaveGuild、DisbandGuild、SetAnnouncement 的 `player_id`；
   - GetGuildRank、GetGuildRankByGuild 的 `zone_id`。

   原计划等 Java 对齐两版同批删；负责人 11-10 要求现在就按标准做法做，Java 本轮只在 `PARITY.md` 记 TODO。四个写方法改为 `callerOf(ctx, 0)`。robot 公会冒烟同步：第 1 步不再填错区，第 4 步不再伪造区，第 8 步改为按会话身份退帮。客户端重新生成与编译交给 Codex。与第 3 条同批，验收见 CODEX.md 第 8 节（设计 §9.6 第 2 条）。
3. **已落码（2026-11-10，未编译）**：负责人 2026-11-10 定运维 / GM 不需要全服帮会榜，删掉，不等第 3 批回执。`UpdateGuildScore` / `RebuildRanks` / `RemoveGuildFromRank` 只维护分区榜 `guild_rank:zone:*`；data 层读榜不给区回 `ErrRankZoneRequired`；三份帮会排行架构文档按只有分区榜重写。代码与测试由「Gate 在线身份传递实现审核」会话与第 2 条同批落码，验收见 CODEX.md 第 8 节（设计 §9.6 第 3 条）。

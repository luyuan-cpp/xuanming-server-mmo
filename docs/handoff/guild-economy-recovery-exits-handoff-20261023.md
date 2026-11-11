# 交接：帮会捐献 / 兑换恢复记录的两个出口（2026-10-23）

> **最新复验（2026-11-11，Codex）**：完整帮会与网络 EditMode **437/437**，零失败、零跳过、Unity 退出 0。相关回归 27 条及请求失败分类 20 条按名全部通过，690 份源码配置摘要前后一致。本轮补修恢复文案登记晚于同步窗口通知的时序缺陷。真实联网复验 **1/1** 通过（冻结参考后端），见 §8.6。

> **历史状态（2026-11-11，复验前）**：客户端前两个出口已实现并通过完整帮会 EditMode 263/263；Go 隔离 HEAD 单测、vet、构建与独立 MySQL 集成 10/10 通过，Java 定向单测 36/36 通过，见 §4。随后负责人要求补齐损坏记录出口，新增契约及验证见 §5，真实联网冒烟见 §6。11-10 Claude Code 两路只读审核见 §8 与 §8.1：资产安全未发现问题；第 1 条（停在捐献 / 商店页时恢复结论被进页自动读取覆盖）已改客户端并提交为 `db1295f6`，未编译，Codex 按 §8.2 验证；低危两条（第 4、5 条）11-11 已改客户端、未提交、未编译，见 §8.4；协议第 4 批的客户端跟进（第 6 条）由 Codex 按 `gate-online-identity-simplify-20261105/CODEX.md` 第 2 步改（11-11 进行中）；「双击即确认」负责人已定加 1 秒间隔，11-11 已改客户端、未编译（§8.3）；审核时顺带看到的两处同类问题（本人申请列表读取后停在「正在读取」、回包清理本机记录时的替换竞态提示）11-11 已改、未编译（§8.5）。客户端侧原始测试证据已随 `Logs/` 丢失，见 §8 第 3 条。下文保留原交接规格；[参考补丁](guild-economy-recovery-client-20261023.patch) 基于历史客户端 `242e35060`，当前客户端已落地，不要再次应用。
>
> **规格**：[05-economy.md §5.44.1](../design/guild-phase2/05-economy.md)。负责人 2026-10-22 拍板：超窗记录「玩家确认后作废」。项目未上线，不涉及存量兼容。
>
> **起草**：Claude（会话 `peaceful-haibt-6a19a3`）。按用户分工，Claude 只写服务端，客户端代码、协议生成、编译与冒烟交给 Codex。

---

## 1. 服务端改动（原交接清单；当前验证见 §4）

| 版本 | 文件 | 改动 |
|---|---|---|
| Go | `go/guild/internal/logic/economy_logic.go` | `withClientRequest` 的 `replay` 改为 `(resp, result, found, err)`；第 3 道查重失败与 raced 一样回 `codes.Unavailable`，不回 `once` 的业务拒绝；第 1 道查重失败照旧放行。`clientRequestHit` / `replayDonation` / `replayShopOrder` 透出查询错误 |
| Go | `go/guild/internal/logic/economy_logic_test.go` | 新增 `TestWithClientRequestLookupFailure`（5 个子用例，不连库） |
| Go | `go/guild/internal/logic/economy_flow_integration_test.go` | 新增 `TestEconomyFlowClientRequestOriginalBytesAfterLeavingGuild`（真 MySQL，build tag `integration`） |
| Java | `game-guild/src/main/java/com/game/guild/service/GuildEconomyService.java` | `clientRequestHit` 查询失败抛私有 `ClientRequestLookupException`；`withClientRequest` 第 1 道吞掉、第 3 道转 `GuildFaultException`（信封 1003） |
| Java | `game-guild/src/test/java/com/game/guild/service/GuildEconomyServiceTest.java` | 新增「幂等键_没写成指令而第三道查重失败_按故障抛_不回业务拒绝」 |
| Java | `PARITY.md` | 功能对齐表末尾追加一行 |

Java 仓工作区里另有别的会话未提交的 `game-data` / `game-friend` / `game-match` / `game-trade` 测试改动，与本任务无关，不要一起提交。

### 1.1 Codex 验证命令（按顺序）

1. Go 单测（工作目录 `go/guild`）：

   ```
   go test ./internal/logic -run 'TestWithClientRequestLookupFailure|TestClientRequestReplayBindsOriginalGuild' -count=1 -v
   go vet ./internal/logic
   ```

   通过标准：5 个子用例全绿，vet 无输出。

2. Go 集成（工作目录 `go/guild`，需 `GUILD_TEST_MYSQL_DSN` 指向有 CREATE / DROP DATABASE 权限的一次性 MySQL 账号；DSN 为空会整体 Skip，Skip 不算通过）：

   ```
   go test -tags integration ./internal/logic -run 'EconomyFlowClientRequest|EconomyFlowExpectedGuild|EconomyFlowReplayCannotChangeExpectedGuild' -count=1 -v
   ```

   通过标准：含新增的 `TestEconomyFlowClientRequestOriginalBytesAfterLeavingGuild` 全部 PASS、零 SKIP。失败时保留 `-v` 输出里第一处断言。

3. Java（工作目录 `D:\luyuan\xuanming-server-mmo`）：

   ```
   ./mvnw -B -pl game-guild -am test -Dtest=GuildEconomyServiceTest -Dsurefire.failIfNoSpecifiedTests=false
   ```

   通过标准：`GuildEconomyServiceTest` 0 失败 0 错误，含新增用例；`幂等键_查重失败交给事务复核_键0一次都不查` 不回归（它走的是写成指令的路径）。

## 2. 客户端实现规格（已实现）

仓库 `D:\luyuan\mmorpg-client`，文件 `Assets/Scripts/Game/Guild/GuildClient.cs`。参考补丁可直接 `git apply`，也可以按下面的规格重写。

### 2.1 出口 ①：离帮后照原字节重发

- `Refresh` 里「回 kGuildNotInGuild」那段的状态清理抽成 `ApplyNotInGuild()`（`Info` / `Applicants` 置空、`HasLoaded`、清经济 / 活动 / 邀请、按需排 `MyApplicationsQueued`），`Refresh` 和恢复路径共用。
- `RecoverEconomyRequest` 的预读 `GetPlayerGuild`：回 14002 → `ApplyNotInGuild()` 后照样重发；其它错误 → 照旧 `AcceptWrite` 返回、保留记录；有效快照 → `Apply` 后重发。**删掉** `Info.GuildId != guild` 时的本地拒发。
- `SendDonation` / `SendShopOrder` 在 `ConfirmEconomyReply` 之后、任何 `Apply` / 帮贡就地更新之前判断：`Info == null || Info.GuildId != request.ExpectedGuildId` 时说明回包讲的是原帮会那一笔，只写结论后返回：有合法视图 → `DonationResultText` / `ShopResultText` + 「（原帮会）」；14002 → 「已不在原帮会，原请求未执行，待确认记录已作废。」；1005 → 原有的 `EconomyKeyConflictText`；其余业务拒绝 → `Accept` 的文案。视图状态为 Applied / AppliedPartial 时触发一次 `AssetsChanged`。不重拉本页、不排 `Refresh`。平常的捐献 / 兑换发出时 `ExpectedGuildId == Info.GuildId`，在途期间推送不改 `Info`，所以不会误进这一支。

### 2.2 出口 ②：超窗记录经玩家确认作废

- 新增 `WithinReplayWindow(created, out clockBehind)`：`clockBehind = created <= 0 || now < created`；仍在窗口 = `!clockBehind && now - created < EconomyReplayWindowMs`。`OwnsEconomyRequest` 与 `RecoverEconomyRequest` 共用。
- 不能重发时，第一次只警告并记下这条记录（`_staleEconomyRecord = saved`）。文案由 `StaleEconomyText(clockBehind)` 生成，确定性的，含「安全恢复期」「再点一次「恢复原请求」」。
- **确认判据**：`_staleEconomyRecord == saved && Status == StaleEconomyText(clockBehind)`，即状态栏仍是那句警告时再点一次。满足时作废：`_economyRecovery.Clear(PlayerId, saved)`（CAS），写「已作废超期的待确认记录，原请求未重发；之后的捐献或兑换按新请求处理。」；`Info != null` 时按 method 带着这句文案重拉捐献页或商店页，否则只写文案。绝不重发，也不换新键重发。
- `OwnsEconomyRequest` 发现超窗（预读在网络里停了很久）时也走警告这一步，玩家紧接着再点一次即作废。
- 超窗时不解析请求字节（损坏的旧字节也能作废）；字段头解析失败不能走重发或超窗出口，改走 §5 的损坏记录二次确认出口。
- UI 不改：`GuildWindow` 的刷新键在 `HasPendingEconomyWrite` 时仍显示「恢复原请求」。

### 2.3 用例（`Assets/Tests/EditMode/Guild/GuildEconomyRecoveryTests.cs`）

- **改**：`AuthoritativeGuildChangeCannotExecuteAnUnsubmittedOldRequest` 断言的是旧的本地拒发，与新契约相反，改写为下面第 1 条。
- **新增**：
  1. `[TestCase(donation, inAnotherGuild)]` 四组：首发回空成功（记录保留）→ `RecoverEconomyRequest` → 预读回 `GetPlayerGuildResponse{ErrorMessage=14002}` 或 `ConfirmRecoveryGuild(556)` → 最后一发是原 method、字节与记录一致 → 回 14002 → 记录清空、`HasPendingEconomyWrite == false`、`Status` 含「原帮会」、最后一发仍是那次写（没有重拉本页）、`RefreshQueued == false`。在别的帮时再 `Send` 一次：新键非 0 且不等于原键、`ExpectedGuildId == 556`。不在帮时 `Info == null`。
  2. 同上四组，但重发回合法视图（捐献 `OpId=9, DonateId=2, Applied`；兑换 `OpId=9, GoodsId=101, Count=2, Applied`，兑换另带 `ContributionBalance=9999`）→ 记录清空、`Status` 含「原帮会」、`AssetsChanged` 恰触发 1 次、没有重拉本页；在别的帮时本人成员行的 `ContributionBalance` 没被改成 9999。
  3. `[TestCase(donation, expired)]` 四组（`expired=false` 即 `_now -= 1` 的时钟回拨）：先 `LoadPage`，首发回空成功 → 拨钟 → 第 1 次恢复：零网络、记录保留、`Status` 含「再点一次」→ `Donate(3)` 被挡（零网络，状态栏被换成「已有待确认…」）→ 第 2 次恢复：因为警告已被换掉，仍只警告、记录保留 → 第 3 次恢复：记录清空、原 method 总共只发过 1 次、最后一发是 `GetGuildDonateOptions` / `GetGuildShop`，`ReplyPageReload` 后 `Status` 含「已作废」→ 再 `Send` 是新键。
  4. `ALongDelayedGuildReadCannotReplayAfterTheSafeWindow` 末尾追加：再 `RecoverEconomyRequest` 一次即作废（记录清空、最后一发是 `GetGuildDonateOptions`）。
- **不改、应继续通过**：`ExpiryOrClockRegressionCannotReplayBeyondTheServerDeduplicationWindow`（警告文案仍含「安全恢复」）、`NewProcessReplaysTheSameOriginalRequestAfterConnectionFailure`、`EmptySuccessBusyAndFaultKeepTheKeyAndBlockAnotherWrite`、`InvalidParameterRetiresTheKeyWithoutResendingItAndReloadsThePage`、`ExistingRefreshButtonExposesRecoveryWithoutCreatingAnotherPayment`。旧 `CorruptRecordsFailClosedWithoutNetworkOrDeletion` 的永久拒绝结论由 §5 覆盖，已改写为二次确认契约。

### 2.4 Codex 验证命令

Unity EditMode（`mmorpg-client` 根目录；Unity 编辑器须关闭，或用另一个实例）：

```
Unity.exe -batchmode -nographics -projectPath D:\luyuan\mmorpg-client -runTests -testPlatform EditMode -assemblyNames MmorpgClient.Tests.EditMode.Guild -testResults <out>\guild-editmode.xml -logFile <out>\guild-editmode.log
```

通过标准：`GuildEconomyRecoveryTests` 全绿（含 §2.3 新增），`GuildUiTests` 无回归。失败时保留 xml 与 log 里第一处编译错误或断言。

## 3. 残余（不阻塞）

- 首发仍在服务端途中、重发恰在玩家离帮后到达：重发回 14002，首发若在离帮前已拿到成员行锁仍会提交。不会重复扣款，只是「原请求未执行」的提示不准，结算推送到达后页面自会更新。
- 记录损坏出口已按负责人后续要求纳入本任务，见 §5；真实读取或权限故障仍须先恢复存储可读性，不能盲删。

## 4. 2026-11-05 Codex 实现与验证

客户端在当前 `7a1b038b7` 基线上实现两个出口，并补齐记录头方法白名单、Reset 清除确认状态，以及离帮后本人申请列表自动刷新成功时保留一次原结论。窗口与协议没有改动。测试新增 29 个实例，覆盖离帮 / 换帮原字节重发、原帮订单不污染现帮余额、超窗 / 回拨确认、校时、换角 / 重连、记录替换、损坏请求体与非法方法，以及结论不被自动刷新覆盖。

| 验证 | 实际结果 | 证据 |
|---|---|---|
| 客户端旧实现红灯 | 55 个恢复用例中 24 个预期失败，31 个通过；失败对应本次缺失的恢复行为 | 客户端 `Logs/guild-economy-recovery-20261105/red-tests.xml`、`red-unity.log` |
| 客户端最终 EditMode | Unity 6000.6.0f1 退出 0；263/263，零跳过：GuildClientTests 147、GuildEconomyRecoveryTests 62、GuildWindowTests 54；源码首尾摘要一致 | 客户端 `Logs/guild-economy-recovery-20261105/green-tests.xml`、`green-unity.log`、`green-sources-before.json`、`green-sources-after.json` |
| Go 定向单测、静态检查、构建 | §1.1 两个顶层单测（含 5 个查重失败分支）、`go vet ./internal/logic`、`go build ./...` 均退出 0 | [汇总](../../build/verification/guild-economy-recovery-20261105/go-head-summary.json) |
| Go 真 MySQL 集成 | §1.1 选择器 10/10 PASS、零跳过，含原字节离帮恢复、expected_guild_id 拒绝与并发同键去重 | [集成日志](../../build/verification/guild-economy-recovery-20261105/go-head-integration.log) |
| Java 定向单测与依赖构建 | GuildEconomyServiceTest 36/36，零失败、错误、跳过；11 个 Maven reactor 项通过，目标源码首尾一致 | Java 仓 `build/verification/guild-recovery-20261105-20261106T031338Z/maven.log`、`tests.json` |

**Go 证据范围**：共享工作区首次编译因另一会话正在迁移 `shared/loginsession` 而失败，保留 `go-unit.log`。负责人随后授权在固定 HEAD `993b6da9d4ea51d5e4a27cc6ec17004af0fb97c1` 的隔离副本验证；三份目标源码与共享工作区 SHA-256 相同。测试不覆盖那条在途身份迁移，不恢复或修改他人文件。集成使用本地已有 MySQL 镜像创建独立名称、随机本机端口、tmpfs 数据目录的一次性容器，按正式 `tools/scripts/sql_fixture_env.ps1` 装配当前进程测试资格，运行后仅清理该容器；未使用共享数据库。当前真库测试除 DSN 外还需该脚本要求的权威资格，不能仅靠设置 DSN 声称可运行。

**边界**：Java 对齐版本仍为 `0.1.0-SNAPSHOT`，`PARITY.md` 已追加本轮验证。未执行客户端联网冒烟、打包、真实 TiDB 或跨区域验收。本批当时留下的损坏记录问题由下节继续处理；首发与离帮竞态的提示限制仍保留。本会话未提交或推送；Unity 导入自动改写的九份资源索引仅时间戳变化已单独留存补丁并撤销。

## 5. 2026-11-05 补齐损坏记录出口

按负责人追加要求，损坏头、未知方法、错误角色字段、窗口内坏请求体，以及真实空文件、超长文件和无效 UTF-8 均采用二次确认作废。首次只警告原请求可能已经到账，不发包；同一警告仍显示、同一存储实例及当前角色槽位的原始内容不变时，第二次才删除。不会发送坏请求，也不会代替玩家换新键重试。损坏或超窗确认期间若被换成合法的新请求，本次确认点击只提示记录变化、保留新记录；下一次主动恢复才允许重发。

恢复读取改用 `DurableRequestFileStore.ReadSnapshot`：同一 `.lock` 下从同一流取得解析文本和原始长度/SHA-256，`Snapshot` 同时绑定实例、角色与绝对路径。不能先按 `Load` 的占位串判坏，再给另一份文件内容发放删除许可。`ClearSnapshot` 在同一写者锁下重新核对原字节后删除；其他角色、其他来源和替换记录均不受旧确认影响。已有 `Load/Save/Transition/Clear` 未改。

读取、占用、锁或删除失败时保留记录并撤销确认，解除故障后重新警告；不把无法读取当成已不存在。作废成功只读刷新本人帮会，使旧经济页失效；有效快照或 14002 保留作废结论，读取错误按真实错误显示。申请列表与帮会读取在途期间的新推送优先于旧作废结论。普通无参数刷新仍保持既有语义。

本项只修改两版服务器共用的客户端，不增加服务端契约、不重跑已完成的 Go / Java 验证，也不修改 Claude Code 的在途代码。规格见 `05-economy.md` §5.44.2；执行证据目录为客户端 `Logs/guild-corrupt-recovery-20261105/`。

实际验证使用 Unity 6000.6.0f1：

| 验证 | 实际结果 | 证据（相对上述目录） |
|---|---|---|
| 损坏恢复旧行为红灯 | 87 项中 70 通过、17 个预期失败，零跳过；退出 2 | `red-tests.xml`、`red-unity.log` |
| 确认期间合法替换红灯 | 损坏/超窗警告 × 捐献/兑换替换，4 个预期失败；确认点击错误发起读取 | `replacement-red-tests.xml`、`replacement-red-unity.log` |
| 最终相关 EditMode 回归 | 14 组 501/501，零失败、零跳过，退出 0；含帮会客户端 147、恢复 83、窗口 54、文件存储 12，以及共用存储/网络的相邻调用者 | `final-tests.xml`、`final-unity.log`、`final-exitcode.txt` |
| 源码一致性 | 生产代码、恢复/存储测试及既有帮会测试在运行前后及收尾时 SHA-256 相同 | `final-sources-before.json`、`final-sources-after.json` |

完整筛选器存于 `green-filters.txt`，最终回归沿用该筛选器。未执行联网冒烟、播放器打包或真实资产到账验收。本聊天未执行提交或推送；收尾时检测到外部操作已将相同客户端源码与九份 Unity 导入时间戳一并提交至 `1dfb79433`，保护检查停止了时间戳清理，没有回退外部提交；服务端文档也已被外部提交 `0f484db0d` 收录。本节最终验证记录随后单独补写。

## 6. 2026-11-06 损坏记录出口的真实联网验收

负责人要求补齐联网冒烟。客户端新增独立 PlayMode 程序集 `Assets/Tests/PlayMode/GuildNetwork/`，唯一显式用例 `GuildEconomyNetworkSmokeTests.CorruptRecordRetirementAllowsRealDonationAndRedemption` 通过正式 `GatewayHttpClient`、`GameClient` 和 TCP RPC 登录建角入场，观察器只记录真实请求与回包，不生成响应。启动仅在 batchmode、显式环境开关、精确程序集和测试名全部匹配时使用既有沙盒排除入口；沙盒组件立即禁用，不启动离线地图或角色，未知客户端宿主和世界仍使测试失败。

最终批次为客户端 `Logs/guild-corrupt-online-20261106/attempt-4/`：Unity 6000.6.0f1 原生退出 0，1/1 通过，零失败、零跳过、无超时；运行前后全部生产 C# 源码与本用例/程序集 SHA-256 一致。`result.json`、`results.xml`、`unity.log`、`sources-before.json`、`sources-after.json` 和 `evidence/summary.txt` 保留实际证据。

| 检查 | 实际结果 |
|---|---|
| 捐献前空文件、兑换前损坏头 | 每次首次点击保留原字节、零帮会请求；第二次删除同一坏记录，仅一发 `GetPlayerGuild`、零经济写，捐献/商店缓存失效 |
| 新捐献 | 新非零请求号、原帮会栏正确；恰一发写请求，订单 `APPLIED`；银两 100000→0，资金 0→12000，帮贡可用/累计 0→120，今日次数 0→1 |
| 新兑换 | 与捐献不同的非零请求号和订单号；恰一发写请求，订单 `APPLIED`；帮贡 120→90，物品 15 数量 0→5，限购次数 0→1；银两、资金及累计帮贡保持不变 |
| 收尾 | 成功用例解散本次独占的单人帮会，确认回到未入帮状态；断开自有客户端，恢复先前选角偏好，保留证据 |

后端为本机独立 Compose 项目 `xuanming-guild-corrupt-20261106`（Gateway 18105、Gate 28100），使用独立网络、卷与新账号。Go guild 从上一轮固定 HEAD `993b6da9d4ea51d5e4a27cc6ec17004af0fb97c1` 隔离副本构建，758 份源文件前后摘要不变；其他节点复用已有成功登录基线镜像，未构建或修改 Claude Code 在途主树。镜像、冻结配置及隔离检查见本仓 `build/verification/guild-corrupt-online-20261106/` 的 `provenance.json`、`shared-source-compatibility.json`、`guild-build-result.json`、`isolation-proof.json`、`runtime-readiness.json`。协议核对的 161 个消息号全部一致；已删除的 EnterGameResponse 两个旧合服提示字段仍在客户端生成类中，但客户端无读取调用，故该证据仅证明本轮新账号经济链路，不宣称描述符逐字一致。

失败批次均保留：attempt-1 是新增测试未独立引用 Protobuf 的编译错误；attempt-2 是自动 UI 启动器先创建 ActorWorld 触发保护；attempt-3 已真实建角建帮，但夹具错误要求零余额新角色的货币列表非空。只修本用例装配与取值，按服务端 `econCurrency` / `EnsureCurrencySlots` 契约将缺失货币槽位读作零，保留 Currency 快照非空、GM 后余额、扣款和到账断言，再得到 attempt-4 通过。没有跳过断言或伪造服务端结果。

执行入口及必要环境变量见客户端 `Docs/GuildUI.md` 本日追加节；本次实际运行器保留在 `Logs/guild-corrupt-online-20261106/run-live-smoke.ps1`，密码只经子进程环境传入，不写入证据。此前 EditMode 501/501 证据不变，本轮没有生产源码修改，因此没有重复执行该组测试。未执行 Java 后端联机、播放器打包、真实 TiDB 或跨区验收。

**Claude Code 审核状态**：按负责人要求已准备只读审核材料 `Logs/guild-corrupt-online-20261106/claude-review-prompt.txt` 及限定范围补丁。准备操作 Claude 桌面时收到物理 Esc 停止信号，已停止电脑操作；审核请求尚未发送，不能声称已审核。尚待继续该审核。本聊天没有提交或推送。
## 7. 2026-11-06 续交 Claude Code 审核的状态

负责人继续授权“可以修复，然后交接给 Claude Code 审核”。本次只读复核没有发现本任务新增待修项：帮会恢复生产代码、相关 EditMode 测试，以及新增 GuildNetwork 用例与程序集均与其已保存验证摘要一致。客户端当前核对 HEAD 为 `f8076ce9596f84c745a5b86fdf9c4de5dad414e0`；其他会话已改过 `GameClient.cs`、`GateTcpClient.cs`、`AppBootstrap.cs`，没有审改或回退这些变化。因此 501/501 与 attempt-4 的 1/1 仍是当时源码的真实证据，不能扩大为当前 HEAD 整条联网链已经复验。本次没有重跑构建或测试。逐项 SHA 与适用范围保存在客户端 `Logs/guild-corrupt-online-20261106/review-current-scope.json`。

限定范围的只读审核说明已更新到同目录 `claude-review-prompt.txt`，并填入 Claude Code 的独立新会话草稿。桌面明确显示 `Weekly limit reached`（所有模型周用量 100%，界面显示 11 月 12 日 15:00 重置）；发送点击被系统通知遮挡，后续窗口操作也未能完成，未观察到已发送消息或审核开始，故状态仍为**草稿已准备、未发送、未取得 Claude 审核结果**。没有改付费、权限或登录设置，没有用其他代理冒充 Claude 审核。本次未修改业务源码、未提交或推送，待该外部限制解除后继续发送审核。

## 8. 2026-11-10 Claude Code 审核结论

**范围**：原审核材料（客户端 `Logs/guild-corrupt-online-20261106/` 下的 `claude-review-prompt.txt`、限定范围补丁与 `review-current-scope.json`）在本机已不存在，按本文 §5–§7 与 `05-economy.md` §5.44.1–§5.44.2 重建范围：客户端提交 `1dfb7943`（`GuildClient.cs`、`DurableRequestFileStore.cs`、`GuildEconomyRecoveryTests.cs`、`DurableRequestFileStoreTests.cs`、`Docs/GuildUI.md`）与 `dc6229f7`（`Assets/Tests/PlayMode/GuildNetwork/` 的用例、程序集与说明），并顺查 `GuildWindow`、`GuildUiRoot` 的调用方式。这些文件从 `dc6229f7` 到当前客户端 HEAD `fca37261` 未再改动，审的就是 §5、§6 实测时的代码。只读审核：未改客户端，未编译，未运行测试。

**资产安全部分未发现问题**：损坏与超窗记录都要两次点击才清；损坏记录的清除在写者锁内复核原始长度与 SHA-256，令牌绑定角色、存储实例与路径；读取、锁、删除失败一律保留记录并撤销确认；确认期间槽位换成合法新记录时只提示变化，不借确认重发；换角（`GuildUiRoot` 在 `PlayerId` 变化时调 `Reset`）、重连、文案变化都要求重新警告。超窗作废带着结论重拉对应本页、不清另一页快照，不受下面第 1 条影响。联网用例的启动门控可靠：测试宿主在 `BeforeSceneLoad` 挂上禁用的沙盒组件，`AppBootstrap.AutoSpawn` 在 `AfterSceneLoad` 看到它即退出；口令只走环境变量，证据目录限定在 `Logs` 内且必须全新，收尾恢复选角偏好。

**发现**：

1. **已改未编译（中，见 §8.2）：停在捐献 / 商店页时，恢复结论会被窗口的进页自动补拉立刻覆盖。** `GuildWindow.Render` 最后一行 `MaybeAutoRequest` 看到本页快照为空，就同步发一次本页读取；`Request` 当场把状态栏写成「正在读取帮会，请稍候…」，回包再写成「捐献信息已更新 / 帮会商店已更新」或结算中文案。
   - 损坏记录作废：`RecoverCorruptEconomyRequest` 先 `ClearEconomy()` 再 `Refresh(结论)`；刷新回包写下结论后紧接着 `Changed → SetClient → Render`，结论一帧都显示不出来。
   - §5.44.1 出口 ①、预读发现已在别的帮：`Apply(新帮)` 因换帮清空经济与活动快照，「…（原帮会）」或「已不在原帮会，原请求未执行…」同样被本页读取覆盖（活动页经排队重拉，`RefreshActivities` 也会改写状态栏）。`ConcludedOutsideOriginalGuild` 注释里的「不重拉本页」只约束了 `GuildClient` 自己。
   - 玩家多半是在捐献 / 商店页被「已有待确认的捐献或兑换」挡住后才点「恢复原请求」，这是最常见的路径。资产不受影响（记录照常清除、不重发），但规格要求保留的结论丢了。现有恢复用例与联网冒烟都只驱动 `GuildClient`、不挂窗口，所以 501/501 和 1/1 都没覆盖到。
   - 修法建议：把 §5.44.1 已有的「本人申请列表首读保留结论一次」推广到恢复结论之后的第一次本页读取（`RefreshDonations()`、`RefreshShop()`，换帮时含 `RefreshActivities()`；后者已有 `_activityResult` 跨一次重拉保留文案的同类做法），损坏作废与「已在别的帮」两种结论都登记这次保留；补挂 `GuildWindow`（照 `GuildUiRoot` 接 `Changed`）、分别停在捐献页与商店页的 EditMode 回归用例，覆盖损坏作废与换帮重放两条路。
2. **已定（负责人 11-10：加 1 秒间隔，待改客户端，见 §8.3）：双击即完成二次确认。** 第一次点击只改状态栏、不进 Busy，「恢复原请求」按钮保持可点；连点两下时第二下看到的仍是那句警告，直接作废。这符合「仍显示该警告时再点一次」的字面规则，但玩家来不及读警告。如要加固，可要求警告显示满 1 秒（用已注入的单调时钟）之后的点击才算确认，不改布局与按钮。
3. **证据：客户端侧原始证据已不在本机。** 客户端 `Logs/` 是忽略目录，现只剩 11-08~11-09 的 Unity 与节庆日志；`Logs/guild-economy-recovery-20261105/`、`Logs/guild-corrupt-recovery-20261105/`、`Logs/guild-corrupt-online-20261106/`（含 attempt-1~4、`run-live-smoke.ps1` 与审核材料）都已没有。§4–§6、`05-economy.md` §5.44.1–§5.44.2 与客户端 `Docs/GuildUI.md` 引用的 263/263、501/501、联网 1/1 的结果文件已无法复查，只剩各处摘要；服务端 `build/verification/guild-economy-recovery-20261105/`、`guild-corrupt-online-20261106/` 两份后端证据仍在。以后客户端验证的小件证据（结果 XML、摘要、源码摘要）建议同时存一份到服务端 `build/verification/<运行>/`。

**未做**：第 1 条已在 §8.2 修复（未编译）；客户端 `Docs/GuildUI.md` 两处「审核尚未发送」已随之改正。修复后由 Codex 跑 Unity EditMode 的帮会恢复、文件存储与窗口相关组，加上新增回归用例；联网冒烟可按 §6 复跑（可选）。两版服务端无改动，Java 版不涉及。本次未提交。

### 8.1 同日第二路只读审核的补充

**范围**：同一份审核请求在另一个会话里独立审了一遍，范围同上（6 个文件全文，另看 `GuildWindow`、`GuildUiRoot` 的调用方式），只读，未编译、未运行测试。git 核对这 6 个文件在 `f8076ce9`（§7 的核对 HEAD）与当前客户端 HEAD `fca37261` 之间无差异。原 `review-current-scope.json` 已不在，下表是 11-10 客户端工作区文件的 SHA-256，作为之后复验的基线：

| 客户端文件 | SHA-256 |
|---|---|
| `Assets/Scripts/Game/Guild/GuildClient.cs` | `877d906354972e3eb38f05fb1b1325c1c388e69352c5ca00615027e9be39ca39` |
| `Assets/Scripts/Net/DurableRequestFileStore.cs` | `e8ceef6ac71922ebaa76887710ba192e18f90cb9e0df75f963faa0c5cfedf4b6` |
| `Assets/Tests/EditMode/Guild/GuildEconomyRecoveryTests.cs` | `6e070b3ca9c5ab1b3d039692bd6a7ee5d7d74afe4aba402068f30d449a6ef6a5` |
| `Assets/Tests/EditMode/Net/DurableRequestFileStoreTests.cs` | `2778703ffe852143eee1692dd3ce647f6e593c8aa500876814b5f39cbb5a0868` |
| `Assets/Tests/PlayMode/GuildNetwork/GuildEconomyNetworkSmokeTests.cs` | `5ad040e08ed4f19d50bb6f2c8df0fbd980105767d51f5a57d694885ca5033720` |
| `Assets/Tests/PlayMode/GuildNetwork/MmorpgClient.Tests.PlayMode.GuildNetwork.asmdef` | `06c18e42ad320e6490196f852473f942099330584958e55046c7f252fbebdbb6` |

**复核上文第 1、2 条**：第二路起初漏了这两条，按代码复核后都成立。第 1 条：刷新回包落定后经 `Changed → SetClient → Render`，`Render` 末行 `MaybeAutoRequest()`（客户端 `GuildWindow.cs:257`）在捐献 / 商店页、`Busy` 已清、本页快照为空（`DonationsNeedReload` / `ShopNeedsReload` 返回真，`GuildClient.cs:148-152`）时同步发本页读取（`GuildWindow.cs:288-291`）。第 2 条：首击只写警告、不进 `Busy`，`RefreshCurrentPage`（`GuildWindow.cs:167-170`）照常放行第二击。

**补充发现**（接上文编号）：

4. **低（测试强度，11-11 已改未编译，见 §8.4）：联网用例的扣款断言在零余额路径上可以空过。** `GuildEconomyNetworkSmokeTests.cs:434-440` 的 `Silver()` 把空的 `Currency.Values` 一律读成 0，用于 `:169`（捐献后应为补足额减 100000）与 `:201`（兑换后银两不变）。§6 走的是新角色 GM 补足路径，捐献后的期望值正好是 0；扣款时若货币槽位丢失或被清空，这两条照样通过。服务端契约下首写后槽位必在：GM 补足经 `ResolveCurrencyField` 调 `EnsureCurrencySlots` 补齐到 `kCurrencyMax`（`cpp/libs/modules/currency/system/currency_system.cpp:65-72、91`），背包快照原样拷贝 `CurrencyComp`（`cpp/libs/services/scene/player/system/player_feature_snapshot.cpp:159-160`）。修法：空列表读零只留给补足前那一次；之后的读取先断言 `Values.Count > 0`（最好不少于上一次）再取下标 0。APPLIED 断言本身没问题：`:345-377` 要求终态恰为 APPLIED，帮会资金、帮贡、次数与物品 +5 都是精确值。
5. **低（11-11 已改未编译，见 §8.4）：替换竞态按存储故障提示，与 §5.44.2「提示变化」不符（不会多扣）。** 确认那一击读完快照之后、`ClearSnapshot` 持锁复核之前，若槽位被另一进程换掉，`ClearSnapshot` 抛 `InvalidOperationException`（`DurableRequestFileStore.cs:74`），`GuildClient.cs:1049-1051` 提示「本机待确认记录未能作废，请检查本机存储后重新确认」。`:1049` 已撤销确认，若换进来的是窗口内合法记录，玩家照提示再点就会重发这条新记录。新记录保留，重发用它自己的键，服务端去重，所以不会多扣；但规格要求这次只「提示变化」，现在的文案反而引导一次会发包的点击。超窗路径 `:1079-1080` 同形。只有遵守 `.lock` 的另一进程恰在同一击的两段锁之间写入才会触发，客户端层没有用例（存储层的 CAS 有）。修法：两处单独捕获 `InvalidOperationException`，沿用 `:1216` 的「待确认记录已变化，本次未作废也未重发；请重新读取后恢复。」
6. **协调（不属本功能）：协议第 4 批会改到本次审核的文件。** 服务端工作区未提交的 `proto/guild/guild.proto`（Gate 在线身份简化第 4 批）把 6 个请求里的 `player_id` / `zone_id` 改成 `reserved`，`GuildClient.cs` 仍有 5 处赋值：`:580` `GetGuildRankRequest.ZoneId`、`:642` `CreateGuildRequest.PlayerId` / `ZoneId`、`:743` `SetAnnouncementRequest.PlayerId`、`:751` `LeaveGuildRequest.PlayerId`、`:759` `DisbandGuildRequest.PlayerId`。按 `docs/handoff/gate-online-identity-simplify-20261105/CODEX.md` §8 生成客户端协议后必须删掉这些赋值，否则客户端程序集编译不过；删完后上表 SHA 与 501/501、联网 1/1 都不再对应当前源码。联网用例调用 `_guild.Create(…, _zone)` 与 `_guild.Disband`，若随之改签名要同步。

**服务端与 Java**：本项没有服务端新增工作。§5.44.1 的服务端前提在当前 HEAD 仍在：`go/guild/internal/logic/economy_logic.go:720-728` 第 3 道查重失败回 `codes.Unavailable`，`TestWithClientRequestLookupFailure` 与 `TestEconomyFlowClientRequestOriginalBytesAfterLeavingGuild` 仍在；本次未重跑。Java 版不涉及，不加 `PARITY.md` 行。

**给 Codex**：第 1 条已在 §8.2 改完；第 2 条（§8.3）、第 4、5 条与第 6 条的客户端跟进可放在同一轮，都叠在 §8.2 的改动之上。11-05 的 14 组筛选器 `green-filters.txt` 已随 `Logs/` 丢失，复验至少跑帮会与网络两个 EditMode 程序集（含第 1 条新增的挂窗口用例）：

```
Unity.exe -batchmode -nographics -projectPath D:\luyuan\mmorpg-client -runTests -testPlatform EditMode -assemblyNames "MmorpgClient.Tests.EditMode.Guild;MmorpgClient.Tests.EditMode.Net" -testResults <out>\guild-net-editmode.xml -logFile <out>\guild-net-editmode.log
```

通过标准：零失败、零跳过、退出 0。再按客户端 `Docs/GuildUI.md` 的联网验收节（环境变量与命令，原运行器 `run-live-smoke.ps1` 已丢）复跑联网冒烟：第 4 条改的正是这个用例，改完要重新拿到一次通过。结果 XML、摘要与源码 SHA 同时存一份到服务端 `build/verification/<运行>/`（见上文第 3 条）。

**文档整理**：上文插入本节时，§7 的末段（11-06 发送审核未成功的记录）被挤到了文件末尾，已原样移回 §7。本次只改文档，未提交。

### 8.2 第 1 条的修复（2026-11-10，Claude Code，未编译）

**改法**：只改客户端 `Assets/Scripts/Game/Guild/GuildClient.cs`，窗口、布局、协议与两版服务端都不动。把「离帮后本人申请列表首读保留结论一次」推广成一条规则（规格已同步改 `05-economy.md` §5.44.1 的客户端实现补充与 §5.44.2）：

- **登记**：以下结论写进 `_economyRecoveryNotice`。
  - 作废损坏记录：`Refresh(结论)` 回包落定后登记，有效快照与「不在帮」两支都登记。
  - 不在帮时作废超窗记录：原有。
  - 恢复预读之后的每个结局：`SendDonation` / `SendShopOrder` 新增 `recovering` 参数，只有 `RecoverEconomyRequest` 传 `true`。`OwnsEconomyRequest` 失败、`ConfirmEconomyReply` 不放行、业务拒绝、成功文案都登记。业务拒绝由 `AfterEconomyReject` 在改写成 1005 文案之后、带文案重拉之前登记；成功文案在带文案重拉本页之前登记。
  - `ConcludedOutsideOriginalGuild`：由只在不在帮时登记改为总是登记。
  - 普通捐献 / 兑换 `recovering` 为假，不登记，行为不变。
- **沿用**：本人申请列表、捐献页、商店页、活动页的无文案读取，发出前用 `TakeRecoveryNotice()` 取一次。状态栏仍是这句才算数，取完即清，不论这次读取发没发出去、成没成功。回包时 `KeptRecoveryNotice()` 只在状态栏仍是「正在读取」（在途期间没有推送改写）时沿用。优先级：调用方带的文案 > 恢复结论 > 刚结算完的那条 > 默认文案；活动页排在 `_activityResult` 沿用之后。带文案的本页重拉不取结论，重拉完对另一页的补拉仍能沿用一次。
- **比审核建议多覆盖的同根结局**：重连后在原帮会重发被拒或拿不到订单视图（最常见的恢复路径，重连已清空两页），以及预读后才发现超窗的警告与「记录已变化」。超窗警告被保留后，玩家下一击仍能按「状态栏还是那句警告」完成确认。

**残余**：沿用的是「下一次无文案读取」，不只是自动补拉。停在已加载的页上、之后主动点「刷新」时，若状态栏仍是这句结论，会再显示一次，与原申请列表规则相同。

**用例**：`GuildEconomyRecoveryTests` 新增 8 条，原有用例未改。

| 用例 | 覆盖 |
|---|---|
| `CorruptRetirementConclusionSurvivesTheVisiblePageReload(true/false)` | 照 `GuildUiRoot` 接线停在捐献 / 商店页，两击作废损坏记录；刷新回包后窗口当场补拉本页，回包后仍是作废结论；之后玩家点刷新只得到默认文案 |
| `AnotherGuildReplayConclusionSurvivesTheVisiblePageReload(true/false)` | 预读发现已在别的帮，原帮会那一笔回 14002；窗口补拉现帮会本页后结论仍在 |
| `RejectedReplayAfterReconnectKeepsItsReasonThroughTheVisiblePageReload(true/false)` | 写请求超时、重连、在原帮会重发被拒（次数 / 限购已满）；补拉后状态栏仍是拒绝原因 |
| `AnotherGuildReplayConclusionSurvivesTheFirstActivitiesReload` | 换帮后活动页的首次排队重拉沿用结论，第二次不沿用 |
| `APushDuringTheFirstPageReloadSupersedesTheRecoveryConclusion` | 补拉在途期间推送改写了状态栏，回包后不再恢复旧结论 |

在旧代码上前 7 条应为红（补拉后变成默认文案），推送一条新旧代码都应绿（守住新规则不越界）。

**改后 SHA-256**（取代 §8.1 表中对应两行）：

| 客户端文件 | SHA-256 |
|---|---|
| `Assets/Scripts/Game/Guild/GuildClient.cs` | `9328157d884b887f12bbeee3fc21eb44163f5228d5129b4335c228c4c8679f44` |
| `Assets/Tests/EditMode/Guild/GuildEconomyRecoveryTests.cs` | `84590fc914a1302053c9d265dd41a37ca55d7aead62edbc359e7d7c18157c9e4` |

**给 Codex**（客户端仓 `D:\luyuan\mmorpg-client`；先关掉占用该工程的 Unity；Unity 为 `D:\luyuan\tools\Unity\6000.6.0f1\Editor\Unity.exe`）：

1. 绿：跑 §8.1 的命令（帮会与网络两个 EditMode 程序集）。通过标准：零失败、零跳过、退出 0，结果 XML 里能按名字找到上表 8 条且全部通过。
2. 红（可选，验证用例有效）：先 `git -C D:\luyuan\mmorpg-client diff -- Assets/Scripts/Game/Guild/GuildClient.cs > <out>\fix.patch`，再 `git -C D:\luyuan\mmorpg-client apply -R <out>\fix.patch`，然后只跑新增用例：`-assemblyNames MmorpgClient.Tests.EditMode.Guild -testFilter "VisiblePageReload;FirstActivitiesReload;FirstPageReloadSupersedes"`（这三个片段只匹配上表 5 个方法）。预期 7 条失败、推送那条通过。之后 `git -C D:\luyuan\mmorpg-client apply <out>\fix.patch` 恢复，确认 `GuildClient.cs` 的 SHA 回到上表的值。
3. 失败时保留：结果 XML，以及 Unity 日志里第一条编译错误或失败用例的断言原文。
4. 结果 XML、日志与上表两份文件的 SHA 存一份到服务端 `build/verification/guild-recovery-notice-20261110/`（客户端 `Logs/` 会被清）。
5. §8.1 第 4、5、6 条若在同一轮改，注意它们也会改 `GuildClient.cs` 或联网用例，改完以新 SHA 为准。

客户端改动已由负责人提交为 `db1295f6`（2026-11-10），服务端文档未提交；Java 版不涉及（只改两版共用的客户端），不加 `PARITY.md` 行。

### 8.3 第 2 条：负责人定加 1 秒间隔（2026-11-10 定；2026-11-11 Claude Code 已改客户端，未编译）

**决定**：负责人 2026-11-10 选「加 1 秒间隔」。规格已写进 `05-economy.md` §5.44.1 出口 ②、§5.44.2 首段与末段：超窗与损坏记录的作废确认，除「状态栏仍是那句警告、记录内容未变」之外，还须距这句警告写出满 1 秒（客户端单调时钟）。不满 1 秒的点击不算确认：不作废、不发包、不重新计时、不改状态栏；满 1 秒后的下一击照常确认。不改布局、按钮与文案。

**实现要点**（客户端 `Assets/Scripts/Game/Guild/GuildClient.cs`，叠在 §8.2（客户端提交 `db1295f6`）与 §8.4 的改动之上）：

- 新增 `public const long EconomyRetireConfirmDelayMs = 1000;` 与字段 `_economyWarningShownAtMs`。
- 写警告时记时刻：`RecoverCorruptEconomyRequest` 的警告分支与 `WarnStaleEconomyRequest` 各记一次 `_monotonicMs()`。`OwnsEconomyRequest` 预读后才发现超窗时走后者，同样从写出时起算。§8.2 的结论沿用只是把同一句警告放回状态栏，不重新计时。
- 两处确认分支再判 `_monotonicMs() - _economyWarningShownAtMs >= EconomyRetireConfirmDelayMs`：损坏记录在「匹配同一快照且状态栏仍是警告」之后、`ClearSnapshot` 之前；超窗记录在 `_staleEconomyRecord == saved && Status == StaleEconomyText(clockBehind)` 成立时。不满足就直接返回，不碰确认状态、不调 `Reject`。
- 读取失败、内容被替换、换角、重连的处理不变；`Reset` 把这个时刻随确认状态一起清零。

**用例**：

- EditMode `GuildEconomyRecoveryTests`：`CreateClient()` 注入假单调时钟（构造参数 `monotonicMs`）。所有期望「两击即作废」的用例（含 §8.2 新增用例里的作废路径）在确认那一击之前把时钟拨过 `EconomyRetireConfirmDelayMs`，建议收成一个 helper；不改的话它们在默认的 `Stopwatch` 时钟下会全部变红。新增损坏、超窗各一组：警告后第 999 ms 的点击保留原记录、零网络、状态栏不变；第 1000 ms 的点击作废；中途一次早点击不重新计时；文案变化后重新警告从新时刻起算。
- 窗口层：在 §8.2 挂窗口的用例里连调两次 `RefreshGuild` 按钮的 `onClick`，断言记录保留、零网络。
- PlayMode `GuildEconomyNetworkSmokeTests.RetireCorruptRecord`：首击后立即再点一次，断言原字节保留、零帮会请求；等满 1 秒真实时间后再点，才走现有的作废断言。该用例用默认单调时钟，不改会失败。可与 §8.1 第 4 条同一次改。

**落码（2026-11-11，Claude Code，未编译）**：按上面的要点改了三个客户端文件。

- `GuildClient.cs`：新增 `public const long EconomyRetireConfirmDelayMs = 1000`、字段 `_economyWarningShownAtMs` 与 `RetireConfirmDelayElapsed()`。损坏警告分支与 `WarnStaleEconomyRequest` 写出警告时记时刻；两处确认分支不满间隔就直接返回，不碰确认状态、不调 `Reject`；`Reset` 把时刻清零。
- `GuildEconomyRecoveryTests.cs`：`CreateClient` 注入假单调时钟 `_monoNow`，新增 helper `PassConfirmDelay()`，21 处确认作废的那一击之前都先调用它。新增 5 条：
  - `AClickBeforeTheWarningHasShownForOneSecondIsNotAConfirmation(true/false)`：损坏、超窗各一。第 999 ms 的点击不作废、零网络、状态栏不变；再过 1 ms 就作废，证明早点击不重新计时。
  - `AWarningShownAgainRestartsTheOneSecondCount(true/false)`：文案变化后重新警告，从新警告起算。
  - `ARestoredStaleWarningKeepsItsOriginalConfirmTime`（挂窗口）：预读期间越过恢复期、重发前才写出的超窗警告，经 §8.2 的补拉沿用回状态栏，仍从原写出时刻计。
  - 挂窗口的 `CorruptRetirementConclusionSurvivesTheVisiblePageReload` 补了「连点第二下记录保留、零网络」的断言。
- `GuildEconomyNetworkSmokeTests.cs` 的 `RetireCorruptRecord`：首击后立即再点，断言原字节、零帮会请求、状态栏不变；按真实时间等满 1.2 秒、状态栏仍是警告，再点才走原有的作废断言。

写入时 Codex 正按 `gate-online-identity-simplify-20261105/CODEX.md` 第 2 步改同一批客户端文件（协议第 4 批，即 §8.1 第 6 条：`Browse` / `Create` 去掉区参数、管理类请求带 `ExpectedGuildId` 等）。两边改的是不同函数；逐处编辑时编辑工具都核对过文件未被同时改写。下表 SHA 是写完时的值，含两边的改动；Codex 之后再改这两个文件就以它的回执为准。

**改后 SHA-256**（取代 §8.1、§8.2、§8.4 表中对应行）：

| 客户端文件 | SHA-256 |
|---|---|
| `Assets/Scripts/Game/Guild/GuildClient.cs` | `760e1c02c06c8ab415a733d9eecfdd49d3dc91e572062461d3252a410ce05807` |
| `Assets/Tests/EditMode/Guild/GuildEconomyRecoveryTests.cs` | `4b26bc9933784ed5c1f2a500d8b855c7eb59eed1162e815d4aa7db26ae0d474c` |
| `Assets/Tests/PlayMode/GuildNetwork/GuildEconomyNetworkSmokeTests.cs` | `b0d8045aff8dcfe537256d67227347bacce47dbb6ab1bdb36a093d4537a15aee` |

**给 Codex**（客户端仓 `D:\luyuan\mmorpg-client`；先关掉占用该工程的 Unity；Unity 为 `D:\luyuan\tools\Unity\6000.6.0f1\Editor\Unity.exe`）。代码已改好，只需编译与验证。§8.2、§8.4 与本节可同一轮验证，排在协议第 4 批的生成与客户端改动（`gate-online-identity-simplify-20261105/CODEX.md` 第 1、2 步）之后，一次编译：

1. EditMode：跑 §8.1 的命令（帮会与网络两个 EditMode 程序集）。通过标准：零失败、零跳过、退出 0；按名字找到 §8.2 的 8 条、§8.4 的 2 条与本节新增 5 条，全部通过。
2. 红（可选，变异法，每项做完立即恢复并核对 `GuildClient.cs` 的 SHA 回到改前的值）：
   - 本节：临时把 `EconomyRetireConfirmDelayMs` 改成 `0`，只跑 `-assemblyNames MmorpgClient.Tests.EditMode.Guild -testFilter "ShownForOneSecond;RestartsTheOneSecondCount;CorruptRetirementConclusionSurvivesTheVisiblePageReload"`，预期 6 条全部失败（早点击被当成确认）。
   - §8.4：临时删掉 `GuildClient.cs` 里两行 `catch (InvalidOperationException) { Reject(EconomyRecordChangedText); return; }`，只跑 `-testFilter "ReplacementDuringTheConfirmingClick"`，预期 2 条失败。
3. 联网冒烟：按客户端 `Docs/GuildUI.md`「2026-11-06」节的环境变量与命令，用全新账号、全新证据目录重跑一次，1/1 通过、退出 0。§8.4 第 4 条、本节与协议第 4 批都改了这个用例。
4. 失败时保留：结果 XML，以及 Unity 日志里第一条编译错误或失败用例的断言原文。
5. 结果 XML、日志与改后 SHA 存一份到服务端 `build/verification/guild-recovery-notice-20261110/`。

两版服务端无改动，Java 版不涉及，不加 `PARITY.md` 行。本次未提交。

### 8.4 第 4、5 条的修复（2026-11-11，Claude Code，未编译）

**第 5 条**（`Assets/Scripts/Game/Guild/GuildClient.cs`）：`RecoverCorruptEconomyRequest` 与 `RetireStaleEconomyRequest` 在通用 `catch (Exception)` 之前单独接住存储的 `InvalidOperationException`。`ClearSnapshot` 与 `Clear(player, expected)` 只在「记录已被替换」时抛它。提示收成常量 `EconomyRecordChangedText`，与确认前就发现变化的那一支（`confirmationChanged`）同一句：「待确认记录已变化，本次未作废也未重发；请重新读取后恢复。」两处在调用存储前都已撤销确认，下一击按换进来的记录正常走恢复（窗口内合法记录就用它自己的键重发），符合 §5.44.2「本次只提示变化，下一次由玩家主动恢复」。读取、锁、删除等 I/O 故障仍按原文案。

**第 5 条用例**：新增 `GuildEconomyRecoveryTests.ReplacementDuringTheConfirmingClickOnlyReportsTheChange(true/false)`，损坏、超窗各一条。确认那一击读完快照后、判恢复窗时会读一次注入的 UTC 时钟，正落在两段锁之间。用例在这次读取里按 `.lock` 协议用 `Transition` 换槽，换成窗口内合法的新捐献请求，然后断言：
- 这一击零网络，新记录保留；
- 提示含「已变化」「未作废也未重发」，不含「存储」；
- 下一击预读后按新记录的原字节与键重发；
- 换槽确实发生在这一击里，以免实现调整后用例空过。

旧代码上两条都应红（提示是「请检查本机存储…」）。为此 `CreateClient` 加了可选的时钟参数，其余用例不变。

**第 4 条**（`Assets/Tests/PlayMode/GuildNetwork/GuildEconomyNetworkSmokeTests.cs`）：`Silver()` 拆成两个。
- `SilverBeforeFirstWrite` 只给补足前的第一次读取用，空列表读零并记下槽位数。
- GM 补足与捐献扣款之后一律用 `Silver`，要求槽位存在且不少于上一次，否则失败。
- 捐献后的余额只读一次存成 `silverAfterDonation`，兑换后与它比，避免拿较早的快照去比槽位数。

这一条在正常服务端上做不出红灯，只能靠代码审阅加一次真实联网复跑来验证。

**第 4、5 条改完时的 SHA-256**（已被 §8.3 末的表取代）：

| 客户端文件 | SHA-256 |
|---|---|
| `Assets/Scripts/Game/Guild/GuildClient.cs` | `3db43335e4b971a500a5da4fe0a54547cd90cb8d86e673beb2db341300d3c175` |
| `Assets/Tests/EditMode/Guild/GuildEconomyRecoveryTests.cs` | `9e0aafd05a12c8191c0b0a4b90ac80416e12aca05b9e3a4ff9e385330df96ac2` |
| `Assets/Tests/PlayMode/GuildNetwork/GuildEconomyNetworkSmokeTests.cs` | `2af51ab07ed61e282298046104d22d6980974c3adb63df72b15e45fb3cca4ce8` |

**给 Codex**（客户端仓 `D:\luyuan\mmorpg-client`；先关掉占用该工程的 Unity；Unity 为 `D:\luyuan\tools\Unity\6000.6.0f1\Editor\Unity.exe`）：

1. EditMode：与 §8.2 第 1 步同一条命令、同一轮跑。通过标准同 §8.2，另须按名字找到 `ReplacementDuringTheConfirmingClickOnlyReportsTheChange` 两条且全部通过。
2. 红（可选）：**不要再反向应用整个 `GuildClient.cs` 的工作区补丁**。同一文件里已经还有 §8.3 与协议第 4 批的改动，反向应用会把它们一起撤掉。改用 §8.3 末「给 Codex」第 2 步的变异法。
3. 联网冒烟：按客户端 `Docs/GuildUI.md`「2026-11-06」节的环境变量与命令，用全新账号、全新证据目录重跑一次，1/1 通过、退出 0。
4. 失败时保留：结果 XML，以及 Unity 日志里第一条编译错误或失败用例的断言原文。
5. 结果 XML、日志与上表 SHA 存一份到服务端 `build/verification/guild-recovery-notice-20261110/`（与 §8.2 同一目录）。
6. §8.3（1 秒间隔）已于 2026-11-11 叠在本节之上落码，本节新增用例与联网用例里的作废确认都已按 §8.3 拨钟或等满 1 秒。

本次客户端与服务端文档都未提交；Java 版不涉及（只改两版共用的客户端），不加 `PARITY.md` 行。

### 8.5 审核时顺带看到的两处同类问题（2026-11-11，Claude Code，未编译）

审核时看到、当时判为范围外的两处，负责人 11-11 要求「查出的问题现在修」，一并改了客户端 `Assets/Scripts/Game/Guild/GuildClient.cs`。

**A. 本人申请列表读取后状态栏停在「正在读取帮会，请稍候…」。** `LoadMyApplications` 的回包只换列表、从不写文案，原设计靠调用方在发出读取后自己写结果（`ApplyToJoin` / `CancelApplication` / `Review` 处的注释写明了这一约定）。但排队补拉（`DrainQueued` 的 `MyApplicationsQueued`）与落地恢复（`DrainSync`）发出的这一发没有调用方补写，`Request` 发出时写的「正在读取」就一直留着。玩家未入帮时打开帮会窗口就会看到；被请离 / 帮会解散的通知、申请未获通过的推送也在一帧之后被盖掉。原有 `KickNoticeSurvivesRefresh` 只测到帮会刷新为止，没有走到紧接着的列表重拉。
- 改法：`LoadMyApplications` 记下读取前的文案，回包时若状态栏仍是「正在读取」（在途期间没有推送、调用方也没写别的）就放回去。调用方「发出后再写结果」的约定不变：那时状态栏已不是「正在读取」，不会被放回的旧文案盖掉。
- 这条规则覆盖了 §8.2 里申请列表那一份「恢复结论沿用一次」：恢复结论只是读取前文案的一种。`_economyRecoveryNotice` 只剩捐献页、商店页、活动页三个消费者。不在帮时登记结论的两行已无人读取（`Refresh(结论)` 的「不在帮」分支、超窗作废的 `Info == null` 分支），一并删掉；注释同步。
- 行为变化：以前申请列表只沿用结论一次，玩家之后再拉列表会落到「正在读取」；现在列表读取总是放回读取前的文案。其他操作改写文案、读取失败、`Reset` 之后都不会翻出旧结论。

**B. 回包清理本机记录时发现已被替换，提示成本机故障。** 与 §8.1 第 5 条同类，是那条漏掉的同类入口。服务端回包后 `ConfirmEconomyReply` 按原内容 CAS 清理本机记录；若另一个守 `.lock` 协议的进程已把槽位换成别的记录，原先提示「服务端已回复，本机原记录尚未清理；请保留记录并恢复查账」，玩家照做再点，恢复的是换进来的新请求。现在单独接住 `InvalidOperationException`，提示 `EconomyRecordChangedText`（「待确认记录已变化，本次未作废也未重发；请重新读取后恢复。」）。I/O 故障仍用原文案。

**用例**：
- `GuildUiTests.cs` 的 `GuildClientTests` 新增 `QueuedMyApplicationsReadKeepsTheTextShownBeforeIt`。被请离后、收到申请未获通过的推送后，各排一次列表重拉，回包后状态栏分别仍是被请离通知与推送文案。旧代码上应红（停在「正在读取」）。
- `GuildEconomyRecoveryTests.cs` 有四条随行为变化改了断言：
  - `QueuedApplicationsRefreshPreservesTheOldGuildConclusionOnce` 改名为 `QueuedApplicationsRefreshKeepsTheOldGuildConclusion`。第二段改成「浏览帮会榜改写文案后再拉列表，状态栏是浏览的文案，不翻出旧结论」。
  - `CorruptRetirementConclusionSurvivesNotInGuildAndOneApplicationsRefresh` 第二段同样改写。
  - `AnotherUserActionBeforeApplicationsRefreshSupersedesTheRecoveryConclusion` 与 `ResetDoesNotRestoreAnOldRecoveryConclusionOnApplicationsRefresh` 加了精确断言：回包后分别是「没有待确认的捐献或兑换。」与「尚未加入帮会，和同道相聚于此。」，以前只断言「不是旧结论」，停在「正在读取」也能通过。
  - `LateProcessCannotClearAReplacementRecord` 的断言由「尚未清理」改为「已变化」且不含「尚未清理」。

**并行情况**：写入时另一个会话正按服务端 `docs/design/client-request-ordering.md` §4.4、§4.7 改同一个 `GuildClient.cs` 的 `Request` 失败分支（「确定未执行」与「结果未知」分开收口），并新增 `GuildRequestFailureTests.cs`。两边改的是不同函数，编辑工具都核对过文件未被同时改写。该文件仍在被改，本节不记 SHA，验证按用例名核对。

**给 Codex**（同 §8.3 末那一轮，帮会与网络两个 EditMode 程序集一起跑）：
1. 通过标准追加：按名字找到 `QueuedMyApplicationsReadKeepsTheTextShownBeforeIt`、`QueuedApplicationsRefreshKeepsTheOldGuildConclusion`（4 条）以及上面改过断言的另外三条，全部通过；结果里不应再出现旧名 `QueuedApplicationsRefreshPreservesTheOldGuildConclusionOnce`。
2. 红（可选，变异法）：
   - 临时删掉 `LoadMyApplications` 回包里的 `if (Status == LoadingMessage) Status = statusBefore;`，只跑 `-testFilter "QueuedMyApplicationsReadKeepsTheTextShownBeforeIt;QueuedApplicationsRefreshKeepsTheOldGuildConclusion;AnotherUserActionBeforeApplicationsRefresh;ResetDoesNotRestoreAnOldRecoveryConclusion"`，预期全部失败。
   - 临时删掉 `ConfirmEconomyReply` 里的 `catch (InvalidOperationException)` 那一行，只跑 `-testFilter "LateProcessCannotClearAReplacementRecord"`，预期失败。
   - 每项做完立即恢复，`git diff` 核对只剩原来的改动。
3. 失败时保留结果 XML 与第一条断言原文；结果存 `build/verification/guild-recovery-notice-20261110/`。

本次未提交；Java 版不涉及，不加 `PARITY.md` 行。

### 8.6 2026-11-11 Codex 合并复验与补修

**新增修复**：首次完整 EditMode 为 433/434，唯一失败是 `ARestoredStaleWarningKeepsItsOriginalConfirmTime`。预读期间记录超窗后，`WarnStaleEconomyRequest → Reject → Changed` 同步触发窗口补拉；发送方法却在通知返回后才登记恢复文案，补拉捕获不到警告，甚至把加载提示登记成结论。`ConfirmEconomyReply` 的空回包、故障、记录替换与清理失败也有相同顺序。现在用 `RejectEconomyRecovery` 先登记文案再通知，两条发送入口删除四处晚登记；普通发送、原记录保留、推送优先级及一秒计时保持原契约。

**红绿证据**：原失败用例扩为捐献 / 兑换两条，新增 `UnconfirmedReplayConclusionSurvivesTheVisiblePageReload(true/false)`，同时断言原记录和写请求次数。旧实现四条全部按预期失败、退出 2；修复后全部通过，没有削弱断言或反向撤销整份工作区补丁。

| 验证 | 结果 |
|---|---|
| 离线运行时编译 | 451 份文件，0 错误，退出 0 |
| 完整帮会 EditMode | 323/323，零失败、零跳过 |
| 完整网络 EditMode | 114/114，零失败、零跳过 |
| Unity 进程 | 退出 0 |
| §8.2–8.5 及补充回归 | 按名称核对 27 条全部通过；旧测试名不存在 |
| 并行请求失败分类 | `GuildRequestFailureTests` 20/20 |
| 运行版本 | 690 份 C# / asmdef / csproj 的 SHA-256 前后一致 |

**协议和并行改动**：正式客户端生成脚本在隔离目录运行，58 份绑定实际变化 6 份（含第 4 批 `Guild.cs`），核对后写回。中间一次 Unity 因同时写入的排行榜 / 登录代码尚无 `RankLimit`、`BeyondRankLimit`、`KLoginZoneCreateRoleClosed` 绑定而停在编译期，日志保留。随后从权威 rank proto 正式生成绑定；用户授权仅在本轮忽略目录补齐导表依赖，从 `Tip.xlsx` 正式生成两份 tip proto、发号 state、segments 与文本，再生成两份客户端 tip 绑定，保留同表两项新码的正式分配。没有手改生成物或并行业务逻辑。后端全量协议生成及 C++ / Go / Java 构建不属于本轮通过证据。

**写回方式**：实现、测试和文档使用草稿副本；写回前逐份确认主仓文件仍与复制时 SHA-256 相同，生成物另核对源与产物摘要，写回后逐文件核对 diff。进度只追加，未提交；帮会客户端修复不涉及 Java 服务端。

**联网复验**：全新账号、全新证据目录的 PlayMode 用例 **1/1 通过、零失败、零跳过、退出 0**，690 份源码配置摘要前后无变化。两次损坏记录作废均验证立即第二击无效、等待满 1.2 秒才确认；捐献只写一次，到账后银两 0、帮会资金 12000、帮贡 120、今日次数 1；兑换只写一次，帮贡 90、物品 15 数量 5、限购次数 1，两笔请求键不同。最终已解散专用帮会并断开会话。证据目录 `guild-recovery-online-15e79938fa7345839e5d8416906e2114/`。

**后端范围与环境修复**：使用全新独立 project / network / volumes，冻结 11-06 Go / scene / battle、历史 guild `993b6da9d`，以及本机现存 11-10 gate 镜像；关键身份、资产和帮会契约已核对，属于参考后端冒烟，不能代表当前全部服务端改动。准备期间修正本轮派生 shell 脚本的 CRLF；定位 gate 注册失败的根因是旧初始化清单漏掉 `chat-world-bcast_g2`，被自动建成 1 分区而契约要求 4。核对本轮 broker 与 offset 全零后，仅删除该自有空 topic，按正式 4 分区 / 600000 ms 重建，gate 随后监听和注册通过。原 139、GDB 截获的更早致命日志及修复证据均保留，未改服务端业务源码、未放宽启动校验。

**保留的首轮联网失败**：`guild-recovery-online-845b7bda2be74b63ba0ff075cac7d803/` 在账号 HTTP 预检出现一次 5 秒、零响应字节的传输超时，尚未建角或改资产，原因未确证。随后独立实际业务探针返回 HTTP 200 / code 0 / 零角色，作为补充业务就绪检查；第二次用全新账号 / 目录复验通过。没有修改客户端超时或测试断言，两轮证据均保留。隔离栈清理与非本轮容器对照见 `online-stack/` 的最终结果。

**证据**：`build/verification/guild-recovery-notice-20261110/` 下 `guild-net-editmode-green.xml/.log`、`guild-net-editmode-green-exit.txt`、`verification-summary.json`、`source-before-green.json`、`source-after-green.json`；四条红灯在 `synchronous-notice-red.xml/.log`。首次失败、并行编译失败、正式生成、依赖安装与隔离环境检查证据均保留。

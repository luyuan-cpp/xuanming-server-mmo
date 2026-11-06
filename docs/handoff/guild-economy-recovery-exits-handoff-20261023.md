# 交接：帮会捐献 / 兑换恢复记录的两个出口（2026-10-23）

> **当前状态（2026-11-05）**：客户端前两个出口已实现并通过完整帮会 EditMode 263/263；Go 隔离 HEAD 单测、vet、构建与独立 MySQL 集成 10/10 通过，Java 定向单测 36/36 通过，见 §4。随后负责人要求补齐损坏记录出口，新增契约及验证见 §5。下文保留原交接规格；[参考补丁](guild-economy-recovery-client-20261023.patch) 基于历史客户端 `242e35060`，当前客户端已落地，不要再次应用。
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
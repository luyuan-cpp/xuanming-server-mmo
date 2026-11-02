# 交接：帮会捐献 / 兑换恢复记录的两个出口（2026-10-23）

> **一句话**：服务端两版（Go `go/guild`、Java `game-guild`）已写完、未编译未测试；客户端（`mmorpg-client`）由 Codex 按本文实现，参考补丁 [guild-economy-recovery-client-20261023.patch](guild-economy-recovery-client-20261023.patch)（基于客户端 `242e35060`，只含 `GuildClient.cs`，没有用例）。
>
> **规格**：[05-economy.md §5.44.1](../design/guild-phase2/05-economy.md)。负责人 2026-10-22 拍板：超窗记录「玩家确认后作废」。项目未上线，不涉及存量兼容。
>
> **起草**：Claude（会话 `peaceful-haibt-6a19a3`）。按用户分工，Claude 只写服务端，客户端代码、协议生成、编译与冒烟交给 Codex。

---

## 1. 服务端改动（未提交，待 Codex 验证）

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

## 2. 客户端实现规格（交 Codex）

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
- 超窗时不解析请求字节（损坏的旧字节也能作废）；字段头解析失败仍按「记录损坏」fail-closed（残余，见 §3）。
- UI 不改：`GuildWindow` 的刷新键在 `HasPendingEconomyWrite` 时仍显示「恢复原请求」。

### 2.3 用例（`Assets/Tests/EditMode/Guild/GuildEconomyRecoveryTests.cs`）

- **改**：`AuthoritativeGuildChangeCannotExecuteAnUnsubmittedOldRequest` 断言的是旧的本地拒发，与新契约相反，改写为下面第 1 条。
- **新增**：
  1. `[TestCase(donation, inAnotherGuild)]` 四组：首发回空成功（记录保留）→ `RecoverEconomyRequest` → 预读回 `GetPlayerGuildResponse{ErrorMessage=14002}` 或 `ConfirmRecoveryGuild(556)` → 最后一发是原 method、字节与记录一致 → 回 14002 → 记录清空、`HasPendingEconomyWrite == false`、`Status` 含「原帮会」、最后一发仍是那次写（没有重拉本页）、`RefreshQueued == false`。在别的帮时再 `Send` 一次：新键非 0 且不等于原键、`ExpectedGuildId == 556`。不在帮时 `Info == null`。
  2. 同上四组，但重发回合法视图（捐献 `OpId=9, DonateId=2, Applied`；兑换 `OpId=9, GoodsId=101, Count=2, Applied`，兑换另带 `ContributionBalance=9999`）→ 记录清空、`Status` 含「原帮会」、`AssetsChanged` 恰触发 1 次、没有重拉本页；在别的帮时本人成员行的 `ContributionBalance` 没被改成 9999。
  3. `[TestCase(donation, expired)]` 四组（`expired=false` 即 `_now -= 1` 的时钟回拨）：先 `LoadPage`，首发回空成功 → 拨钟 → 第 1 次恢复：零网络、记录保留、`Status` 含「再点一次」→ `Donate(3)` 被挡（零网络，状态栏被换成「已有待确认…」）→ 第 2 次恢复：因为警告已被换掉，仍只警告、记录保留 → 第 3 次恢复：记录清空、原 method 总共只发过 1 次、最后一发是 `GetGuildDonateOptions` / `GetGuildShop`，`ReplyPageReload` 后 `Status` 含「已作废」→ 再 `Send` 是新键。
  4. `ALongDelayedGuildReadCannotReplayAfterTheSafeWindow` 末尾追加：再 `RecoverEconomyRequest` 一次即作废（记录清空、最后一发是 `GetGuildDonateOptions`）。
- **不改、应继续通过**：`ExpiryOrClockRegressionCannotReplayBeyondTheServerDeduplicationWindow`（警告文案仍含「安全恢复」）、`CorruptRecordsFailClosedWithoutNetworkOrDeletion`、`NewProcessReplaysTheSameOriginalRequestAfterConnectionFailure`、`EmptySuccessBusyAndFaultKeepTheKeyAndBlockAnotherWrite`、`InvalidParameterRetiresTheKeyWithoutResendingItAndReloadsThePage`、`ExistingRefreshButtonExposesRecoveryWithoutCreatingAnotherPayment`。

### 2.4 Codex 验证命令

Unity EditMode（`mmorpg-client` 根目录；Unity 编辑器须关闭，或用另一个实例）：

```
Unity.exe -batchmode -nographics -projectPath D:\luyuan\mmorpg-client -runTests -testPlatform EditMode -assemblyNames MmorpgClient.Tests.EditMode.Guild -testResults <out>\guild-editmode.xml -logFile <out>\guild-editmode.log
```

通过标准：`GuildEconomyRecoveryTests` 全绿（含 §2.3 新增），`GuildUiTests` 无回归。失败时保留 xml 与 log 里第一处编译错误或断言。

## 3. 残余（不阻塞）

- 首发仍在服务端途中、重发恰在玩家离帮后到达：重发回 14002，首发若在离帮前已拿到成员行锁仍会提交。不会重复扣款，只是「原请求未执行」的提示不准，结算推送到达后页面自会更新。
- 记录损坏（`DurableRequestFileStore.Load` 回 `!损坏` / `!无法读取`，或字段头解析失败）仍 fail-closed、没有出口，同样会永久阻塞该角色的捐献 / 兑换。建议另立任务：比照超窗作废的确认流程处理。

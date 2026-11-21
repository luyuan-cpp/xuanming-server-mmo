# 账号注销与 Apple / Google 登录:剩余代码与全部验证交接(2026-11-20,交 Gemini)

## 0. 现状

- 负责人 2026-11-20 把这条线的全部待做交给 Gemini,原 Claude 会话停在这里。设计:[account-deletion.md](../design/account-deletion.md)(§9 定案、§10「收尾批」列了每处改动与文件)。
- **服务端代码已全部写完,而且都在 HEAD**(批量提交 `9824368eb`、`55eae6be3` 收进去的)。工作区里 `data/tip/Tip.xlsx`、`go/guild/guild.go`、`go/match/internal/authority/schema.go`、`role_fence_purge.go`、`proto/match/match_authority.proto` 仍是 M,那是别的会话后来的改动,不是本线的。
- **客户端(`D:\luyuan\mmorpg-client`)本线全部未提交**,与别的会话的改动混在同一批文件里:
  - 未跟踪:`Assets/Plugins/iOS/MmorpgAppleSignIn.mm`、`Assets/Plugins/Android/MmorpgGoogleSignIn.androidlib/`、`Assets/Editor/AppleSignInBuildPostProcess.cs`、`Assets/Scripts/Net/PlatformSignIn.cs`、`Assets/Tests/EditMode/Net/PlatformSignInTests.cs`、`AccountDeletionTests.cs`、`AntiAddictionTests.cs`(实名会话建的,本线在里面加了 `AccountDeletionKickReasonsHaveTexts`)。
  - 修改:`Assets/Scripts/Core/ClientSettings.cs`、`Assets/Scripts/Net/GatewayHttpClient.cs`、`Assets/Scripts/Game/GameClient.cs`、`Assets/Scripts/UI/Ugui/QdaoServerSelectView.cs`、`.Visuals.cs`、`.LoginUtilities.cs`。
- 派生任务「剧本审核流水保留期清理」(`match_battle_script_review` 保留 180 天)也已写完,代码在工作区。那个会话后来撞了额度上限,但代码与 PROGRESS 条目都已落盘。
- **一条都没验证过**:没有生成、编译、测试或冒烟记录。

## 1. 规矩

- 与 [组队跟随交接单](team-follow-afk-hosting-handoff-20261120.md) §1 相同:读 `AGENTS.md` 全文;只在 main、不开分支、不 push;**只有负责人明确要求才 commit**,而且只提交本线的部分;不手改生成产物;日志英文;失败先留首错原文、修根因、不跳测试;结果追加进 `docs/PROGRESS.md`,日志放 `build/verification/<运行名>/`。
- 改代码用最高档模型,编译、测试、冒烟用低档模型(`AGENTS.md` §10.3)。
- 本线补充:
  - tip 码只由导表器发号,不在代码里写数字。本线的码是 login 段 2038~2041 与 mail 段 `MailRecipientDeleted`(应为 17011),已有的码号一个都不能变。
  - 重新烘焙登录预制体前先关掉 Unity。
  - Java:本仓 `java/gateway_node` 是本线的一部分,要编译、要测;独立 Java 仓(`D:\luyuan\xuanming-server-mmo`)不写代码,`PARITY.md` 已登记两条待做。

## 2. 本线的 PROGRESS 条目

按标题开头 grep 定位,每条末尾有完整命令、环境变量与通过标准:

| 日期 | 标题开头 | 步骤 |
|---|---|---|
| 11-19 | Claude:账号注销三批落码 | 第 0~11 步 |
| 11-20 | Claude:账号注销收尾批 | 第 0~9 步 |
| 11-20 | Claude:剧本审核流水保留期清理 | 第 0~9 步 |
| 11-20 | Claude:小游戏 WebSocket 收尾 | 只看第 1 条「小游戏账号注销不了」相关的客户端用例 |

## 3. 按这个顺序做完

1. **生成**(仓库根,只跑一次;同一批若还有支付、组队跟随的交接,合并成一次):
   - `dev.bat gen`:三语言 tip 产物里出现 2038~2041 与 17011,`tip_enum_ids.json` 只增不改;
   - `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`(默认配置,写客户端仓):`go/proto/match` 出现 `MatchRankRemovalRecord`;
   - `pwsh -File go/build.ps1`:login 的 goctl 包装出现 `RequestAccountDeletion`、`CancelAccountDeletion`、`ScheduleAccountDeletion`。
2. **清库**:游戏未上线,按 `AGENTS.md` §6.2 清空 Redis、MySQL / TiDB、etcd,重置 Kafka offset;再对 data_service、match 等跑 `-migrate`。旧库会被形状守卫和缺索引检查拒启,这是预期行为。
3. **静态检查**:`gofmt -l`(各条目列的文件)、`python tools/scripts/check_log_text_english.py`、`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`。
4. **编译**:
   - Go:`go/shared`、`data_service`、`login`、`friend`、`guild`、`mail`、`match`、`player_inbox`、`trade`、`rank` 各跑 `go build ./... && go vet ./...`,guild 另加 `-tags integration`;
   - C++:`msbuild game.sln /m:1`;
   - 本仓 Java 网关:按 11-19 条第 7 步;
   - 客户端:`pwsh -File tools/client_compile_check.ps1 -ShowErrors 40`。
5. **测试**:按第 2 节四条的单测、真库用例与回归步骤逐条跑,再跑 11-19 条第 8 步的 PowerShell 契约与剧本审核流水条目里的循环活性契约。
6. **客户端**:按 11-19 条第 9 步重新烘焙登录预制体,EditMode 过滤器取 11-19 条第 9 步、11-20 收尾批第 7 步与小游戏收尾条目三处的并集。
7. **冒烟**(调试栈加沙箱 mock,`docs/ops/wechat-qq-sandbox-runbook.md` §I.5;把 data_service 的 `AccountDirectory.DeletionCoolingOffSeconds` 配成 0 以免等 7 天),逐条核期望:
   1. 口令账号申请注销 → 冷静期弹窗 → 撤销 → 正常进游戏;
   2. 再申请,冷静期满后执行 → 角色与存档没了 → 同一账号再登录是空角色的新账号,提示「之前的账号已注销」;
   3. Apple / Google(mock):同一枚 id_token 第二次登录被拒;Apple 申请注销时 mock 收到换票与撤销请求;
   4. 有挂机月卡的角色在线时申请 → 设备弹「账号正在注销中…」下线、不自动重连 → 别的玩家看到他离开场景 → 执行时两轮内删完;
   5. 帮会:帮主本人申请被拦(`guild_leader`);运维代办不拦;执行时帮主让给长老;只剩他时帮会解散,并从帮会榜消失;
   6. 1V1 评级榜上的人删完后从榜上消失;
   7. 删完后给他发榜单奖励邮件被拒(`kMailRecipientDeleted`),rank 奖励转放弃、只记 Info 日志。
8. **代码待做**(改代码,最高档模型):
   1. **登录页 Apple / Google 按钮改成官方样式**。现在两个按钮用的是游戏通用底图加文字(`Assets/Scripts/UI/Ugui/QdaoServerSelectView.Visuals.cs` 第 243~246 行,底图 `clean_tab`),App Store 与 Google Play 审核会按品牌规范打回:
      - Apple 按 Human Interface Guidelines「Sign in with Apple」:黑底、白底或白底黑描边三选一,左侧 Apple 标志加「通过 Apple 登录」,圆角,高度不低于 44pt,标志用 Apple 官方素材,不改色、不拉伸;
      - Google 按 Sign-In 品牌规范:浅色或深色两种,左侧 Google「G」标志,文字「使用 Google 账号登录」。
      - 素材从 Apple、Google 官网下载,来源与授权按各自规矩确认。改完重新烘焙预制体,跑 `QdaoServerSelectPrefabTests` 与 `QdaoLoginPresentationTests`。
   2. 修第 1~7 步发现的问题。错误落在别的会话正改的文件里时,先用 `git diff` 分清,不是本线的只记录、不修。
9. **收尾**:在 `docs/PROGRESS.md` 文末追加「Gemini:账号注销与 Apple / Google 登录验证」一条,写清命令、通过数、失败原文摘要和改过的文件;负责人要求提交时,只提交本线的部分。

## 4. 已定案,修 bug 时不要改回

- Apple 撤销走「申请时重新授权、先换票、冷静期提交成功后撤销」,不保存 Apple 令牌;身份令牌一次性消费,不做挑战式 nonce;冷静期 7 天(设计 §9)。
- 帮主只在本人申请的预检里拦;运维代办和执行时都不拦,执行时收尾自动让位或解散(§6.3)。
- 注销下线 = gate 踢线带原因码 + `MarkOffline` 正常登出(§7.2)。只踢 gate 会让挂机月卡角色转成离线托管,删角卡死。
- 同地域跨区踢线本来就行;注销范围是本地域的账号(§7.2、§7.4)。
- 摘评级榜走 `match_rank_removal` 事务发件箱(§6.2 rank 行);删角后的个人邮件拒收,发件方按终态处理。
- 玩家域 Redis 的四个单调计数永久保留;剧本审核流水保留 180 天(§6.2)。

## 5. 不归 Gemini,需要负责人

- 真机测试(iOS、安卓)要 Apple 与 Google 开发者账号。
- 隐私政策写明保留哪些数据、保留多久(§6.2 表的「保留」列)。
- 部署时运维给 login 的 runtimeConfigSecret 加 Apple 三行,并建 Secret `login-apple-signin`(运维手册 §I.4)。
- 登录页与注销弹窗的视觉验收(没有效果图)。

## 6. 已知风险(来自 11-20 收尾批条目)

- 让位选人不看对方的护栏副本,可能选中同在删除中的成员;轮到他收尾时会再让一次。
- 帮会双存储已经不一致(另有一个 role=3)时,让位按「恰好一个帮主」断言失败关闭,要人工修数据。
- `deletionSessions.KickPlayer` 已于 2026-11-21 抽出 `kickPlayerSession` 并在 `go/login/internal/svc/account_deletion_test.go` 补齐 4 条单测(离线/缺失跳过、先踢 gate 再 MarkOffline、踢 gate 失败中止 MarkOffline、无 gate 身份直呼 MarkOffline)。
- 发件预检与建邮箱行之间删角收尾恰好提交时,会留一行空邮箱行。

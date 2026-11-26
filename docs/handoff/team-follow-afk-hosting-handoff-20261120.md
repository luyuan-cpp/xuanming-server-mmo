# 组队跟随 / 离线托管 / 挂机月卡 / 冷却与 Buff 跨重登:待验证项交接(2026-11-20,交 Gemini;2026-11-22 代码收口完,余编译/单测/冒烟交 Gemini)

> **2026-11-22 状态更新（先勿删本交接单，待 Gemini 跑完编译、单测与两条 robot 冒烟后再清理）**：
> - 第 1–4 步（xlsx / 导表 / proto 生成）与 2026-11-21 的编译/测试修复（6 处）已落盘在 `main`（Go / Linux GCC / Unity 105/105 与 C++ 9 个定向套件曾于 11-21 全部通过）。
> - **2026-11-22 深度标准审计并完成全部服务端 + 客户端代码收口（`main` 工作区，未编译、未运行单测/冒烟，详见 `docs/PROGRESS.md` 文末 2026-11-22 条目）**：
>   - 修复跨节点托管落点 `OnOfflineHostingStarted` 早于 `ReadMembership` 导致丢跟随意图、托管队长 15s 周期刷读、`AfkPassSystem::GmSet` 缺可写围栏、`AfkPassCommittedComp` Redis SET/DEL 写失败自愈与托管启动立即补投影、`AfkPassRule` 缺热更校验、空队形 20Hz 空转、50m AOI 离线托管广播刷屏/无效序列化、`RestoreCooldowns` 改 `emplace_or_replace`；
>   - 修复 Unity 客户端托管重登无快照窗口不显示跟随与丢失暂离选择、换角状态残留、`AfkPassClient` 静默切连接未通知 `Changed`、非灵石币种错误文案与续费确认按钮文案；
>   - 已补齐 C++ 与 Unity EditMode 单测，C++ 静态预检（裸指针 13/13、线程 5/5、英文日志 0 违规）全部通过；Java 版已继续登记在 `../xuanming-server-mmo/PARITY.md` TODO。
> - **剩余交 Gemini**：重跑第 5、7、8（含 11-22 新增用例）、9、10、11（两条 robot 冒烟）与按需第 12/13 步；全部验收通过后再删除本文。

## 0. 现状

- 负责人 2026-11-20 把这条线(含它派生的三个会话:托管队员跨场景、挂机月卡开通入口、冷却与 Buff 跨重登)的全部待做交给 Gemini,原 Claude 会话暂停。
- **代码已全部写完**,大部分已提交到各自 main:服务端在 `9824368eb`、`55eae6be3`、`430983b08`、`ccf2840b2` 等批量提交里,客户端 `D:\luyuan\mmorpg-client` 在 `f8076ce9` 及之后。
- **还没提交的本线改动**都在主工作区,没有 worktree:
  - 服务端,11-20「实时冷却与 Buff 跨重登保留」的复核补与独立评审后再补两条:
    - `cpp/libs/services/scene/combat/buff/system/buff.{h,cpp}`、`combat/buff/comp/buff_comp.h`
    - `cpp/libs/services/scene/combat/skill/system/skill.{h,cpp}`
    - `cpp/libs/services/scene/battle/system/player_battle.cpp`
    - `cpp/libs/services/battle/system/turn_battle_engine.cpp`
    - `cpp/tests/buff_test/buff_test.cpp`、`cpp/tests/skill_test/skill_test.cpp`、`cpp/tests/bag_test/player_battle_settlement_test.cpp`
    - `docs/design/realtime-cooldown-buff-persistence.md`、`ecs-component-access-rules.md`、`disconnect-reconnect-audit-20261020.md`
  - 服务端,11-20「挂机月卡复核补齐」改的文档:
    - `docs/design/afk-monthly-card.md`、`payment-recharge.md`、`redis-player-store-cluster.md`、`team-follow-server-trail.md`、`player-connection-lost-hold.md`
    - 另有本交接单、`docs/README.md` 的登记行、`docs/PROGRESS.md` 里本线追加的条目。
  - 客户端,11-19「挂机月卡开通入口」:
    - `Assets/Scripts/Game/AfkPass/`(含 `.meta`)
    - `Assets/Scripts/UI/Ugui/Gameplay/AfkPassUiRoot.cs`
    - `Assets/Tests/EditMode/Tianyong/AfkPassClientTests.cs`(含 `.meta`)
    - `Assets/Scripts/UI/Ugui/WorldHudNavigation.cs`
    - `tools/gen_proto.ps1`、`tools/gen_messageids.ps1`
  - 多个会话共用主工作区,这些文件里可能混有别的会话的改动。提交时按内容分清,只提交本线的部分,而且只在负责人要求时提交。
- **剩下的全是执行**:改 xlsx、导表、协议生成、编译、单测、冒烟、联机验收;失败时按根因修。
- **一条都没验证过**:`build/verification/` 最近一次运行是 11-18;本线用例名(`HostedPlacement*`、`ConnectionLostHoldAfkPassEcs.*`、`BuffKeepOnLogoutTest.*`、`AfkPassSystemTest.*` 等)没有任何运行记录。11-06 APS 快照里 `aoi_test` 84 条通过的记录在之后的改动面前已作废。

## 1. 开工前必读与规矩

- 先读 `AGENTS.md` 全文,重点 §3(生成产物)、§4(proto 流程)、§6.2(冒烟 / 压测前清数据)、§7、§9(禁止事项)、§10.2、§11。`AGENTS.md` §10.1 规定「Claude 不跑命令,由 Gemini 跑」;本批由 Gemini 执行。
- 只在 main 工作区做,不开分支 / worktree;不 `git push` / `git tag`;**只有负责人明确要求才 commit**,而且只提交自己的改动。工作区常有几百个别的会话未提交的文件:不要 `git add -A`,不要回退别人的改动,不要 `git reset` / `git checkout --`。
- 不手改 `generated/` 与各语言 proto 输出树;生成式 handler 只改 `///<<< BEGIN WRITING YOUR CODE` 守护段。
- 日志文案用英文;文档、注释、提交说明用中文。
- C++ 的约束:
  - 类 / 结构体不放裸指针成员。
  - 节点代码不新起线程。
  - MSBuild 一律 `/m:1` 串行,并发会报假的 C1041 / LNK1104。
  - `run_cpp_tests.ps1 -Build` 不重编库:改了库要先整解,再跑测试。
  - 新增 `.cpp` 只登记 `.vcxproj` / `.filters`;`CMakeLists.txt` 由 `build_linux.sh` 按 vcxproj 生成,别手改。
- 失败时的做法:
  - 保留第一条编译错误(文件:行)或完整的失败输出。
  - 修根因。不注释断言、不放宽阈值、不跳测试、不连续自动重试。
  - 报错落在别的会话正改的文件里,先 `git diff` 分清是谁的改动;不是本线的就原样记录、不修。
- 游戏未上线,玩家数据可以全部清空。冒烟前按 `AGENTS.md` §6.2 清 Redis(`dev_tools.ps1 -Command redis-wipe-local -ConfirmWipeAllRedis`)、MySQL、etcd,并重置 Kafka offset。
- 结果写进 `docs/PROGRESS.md`:**在文末追加新条目,不改旧条目**。标题写「Gemini:… 验证」,列出命令、通过数和失败原文摘要。日志放 `build/verification/<运行名>/`。
- Java 版不写代码。负责人定「java 留到 todo」,Java 仓 `PARITY.md` 已登记待做。

## 2. 本线的 PROGRESS 条目

在 `docs/PROGRESS.md` 里按标题 grep 下表的标题开头即可定位。每条末尾有「交 Codex」步骤和通过标准;第 3 节把它们合成一遍跑完的顺序。

| 日期 | 标题开头 | 内容 |
|---|---|---|
| 11-06 | 组队跟随改为服务器沿队长轨迹驱动队员 | 轨迹跟随(纯逻辑 + ECS 驱动)、协议 381 / 382、go/match 队伍刷新 |
| 11-06 | 组队跟随补「离线的队员也跟」 | 断线保留期间照常驱动 |
| 11-06 | 开通挂机的玩家断线后离线托管、照常组队跟随 | 断线保留 + 月卡托管 |
| 11-10 | 组队跟随:入场后补推跟随状态 | `ResendStateAfterEntry`;客户端改为服务器驱动 |
| 11-10 | 离线托管的队员跟着队长跨场景、跨节点 | 托管落点 `hosted_placement`、服务器发起的同 zone 交接 |
| 11-11 | 组队跟随客户端补齐用例与兼容说明 | 客户端 EditMode 用例 |
| 11-11 | 离线托管队员跟随的兜底重试 | 每 15 秒按玩家号错开重试 |
| 11-12 | 组队跟随遗留三项收尾 | 朝向口径、`ActorMoveBatch`(135)、static_assert |
| 11-12 | 离线托管角色随排空 / 疏散搬走 | 排空 / 疏散改派、落点有界重试 |
| 11-12 | 托管归位 | player_locator 放回世界、`ReleasePlayerRequest.keep_held` |
| 11-13 | 托管归位补一处 | 归位信息的存活按月卡对齐 |
| 11-19 | 组队跟随间距放大到 3 倍 | 视野 50、AOI 格子 50(间距被下一条改定) |
| 11-19 | 组队跟随间距改定 3 米 | 间距 3 米、归队阈值 24 |
| 11-19 | 组队跟随与离线托管本线遗留项清零 | `DrivenSlot`、robot 分步拉开、删过时 TODO |
| 11-19 | 组队跟随合并广播的发送层补单测 | 拆出 `actor_move_broadcast.cpp` |
| 11-19 | 实时技能冷却与 Buff 跨重登保留 | 存档字段 28 / 29、Buff 表 `keep_on_logout` 列 |
| 11-19 | 挂机月卡开通入口 | 灵石开通、存档为事实源、Redis 只作投影、客户端面板、robot 冒烟 |
| 11-20 | 挂机月卡复核补齐 | 交接窗口重投白名单、`gen_schema_index` |
| 11-20 | 实时冷却与 Buff 跨重登保留·复核补 | 再施加刷新时长、回合快照按剩余时长 |
| 11-20 | 实时冷却与 Buff 跨重登保留·独立评审后再补 | 本节点表里查不到行的 Buff 原样带回、空冷却列表保留新字段、恢复的到期时刻不漂移 |

## 3. 合并后的执行顺序

一遍跑完上表所有条目。用例名与通过标准以各条目为准;数量对不上时贴实际清单,不要猜。

1. **预检数据目录**:`git status --short data/ proto/`。
   - 本线会动的表只有三类:`data/Buff.xlsx` 与 `data/schema/buff_table.proto`(已提交)、新建的 `data/AfkPassRule.xlsx` 及其 schema、`data/tip/Tip.xlsx` 里的 afk_pass 组。
   - 有别的会话未提交的表改动时,停下报告,不整批导出。
2. **xlsx**:
   - 先跑 `python tools/scripts/afk_pass_xlsx_patch.py new-tables`,再跑 `python tools/scripts/afk_pass_xlsx_patch.py tip-codes`。
   - 32000 段被占时,改脚本顶部的 `TIP_BASE`,并同步 `docs/design/afk-monthly-card.md` §4.5。
3. **导表**:
   - `dev.bat gen`
   - `py tools/data_table_exporter/tools/gen_schema_index.py`
   - 再跑一次带 `--check` 的同一条命令,应为绿。
   - 通过标准:生成 `afkpassrule_table.*` 与 `afk_pass_error_tip.pb.*`;Buff 的生成代码里有 `keep_on_logout`;`data/AGENTS.md` 的索引里 Buff 为 64 列 54 字段,并多一行 AfkPassRule。
4. **协议**:
   - 在仓库根依次跑 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build` 和 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`。用**默认配置**,本线客户端已授权,会写客户端仓。
   - 再到 `D:\luyuan\mmorpg-client` 跑:
     - `pwsh -File tools/gen_proto.ps1 -ProtoRoot D:/luyuan/xuanming-server-mmo-cpp`
     - `pwsh -File tools/gen_messageids.ps1 -ProtoRoot D:/luyuan/xuanming-server-mmo-cpp`
   - 期望:
     - 381 / 382 号不变。
     - `proto/message_id.txt` 多出 `SceneAfkPassClientPlayer` 的 3 个方法。
     - `proto/persist_schema_gen.txt` 只追加行,含 `player_database#28` / `#29`、`SavedBuffComp`、`PlayerUint64Comp#2 … afk_pass_expires_at`;没有改号或换组的报错。
     - 生成了 `EnterSceneRequest.hosted_placement`、`PlayerHostedPlacementEvent`、`ReleasePlayerRequest.keep_held`。
5. **Go**:
   - 环境:`$env:GOTOOLCHAIN='local'; $env:GOPROXY='off'`。
   - 每个目录先 `gofmt -l` 无输出,再 `go vet`。
   - 各目录:
     - `go/match`:`go test ./internal/team/ -count=1`
     - `go/scene_manager`:`go build ./...`;`go test ./internal/logic/ -count=1 -v -run 'HostedPlacement|NoGateIdCrossNode|HomeZoneNotQueriedWithoutGateRoute|CrossNodeWithCurrentMarker|RouteFailureDoesNotDispatchRelease|ResolveEvacuationTarget|HolderRelease|HostedResume|IdentityIsHostedLogin|SceneInitiatedHostedPlacement|ResumeSceneOf|LeaveRelease|UnqualifiedHolder'`;最后 `go test ./... -count=1`
     - `go/player_locator`:`go test ./internal/svc/ ./internal/logic/ -count=1 -v -run 'Hosted|SceneManagerKeyForZone|ZoneSceneManagers|LeaseMonitor'`;最后 `go test ./... -count=1`
     - `go/shared`:`go test ./redisslot/ -count=1`
     - `robot`:`go build ./...`、`go vet ./...`、`go test ./config/...`
   - 既有失败要逐条点名归因(这几个目录都有别的会话的在途改动)。
6. **C++ 预检**(只读,几秒):
   - `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`
   - `python tools/scripts/tests/test_cpp_thread_sources.py`
   - `python tools/scripts/check_log_text_english.py`
7. **C++ 整解**:`msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`。
   - 要求 0 个 `error C` / `LNK`。
   - `view.h`、`player_team.cpp`、`team_follow_system.cpp` 里的 static_assert 必须通过。
8. **C++ 用例**:一律 `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter <工程> -LogDir build/verification/<运行名>`。
   - `aoi_test`:`TeamFollow*`、`AoiGridTest.*`、`AoiVisibilityTest.*` 全过。
   - `aoi_delivery_test`:全量全过,重点 `MoveReceiverGroupingTest.*`。几何按视野 50 重算过。
   - `cross_zone_test`:先用下面的过滤串定向跑,再全量。
     - 过滤串:`ConnectionLostHold*:ExitPersistEcs.*:PlayerTeamFollowTest.*:TeamFollowDriveTest.*:HostedPlacement*:TravelHandoffInitiatorEcs.*:EnterFenceEcs.*:SceneEntryPermissionTest.*:RelocateConfirm*:HostedPresence*:ReleaseFence*:PlayerHandoffGate.*`
     - 新用例 `DrivenSlotIsWhatTheResendAfterEntryReports`、`ResendAfterEntryLeavesTheFormationUntouched`、`ConnectionLostHoldAfkPassEcs.*` 两条都要在列。
   - `bag_test`:`AfkPassRulesTest.*`、`AfkPassSystemTest.*`、`ClientRequestLedgerTest.*`、`PersistRoundTripTest.*`、`PlayerBattleSettlementTest.SnapshotBuffRoundsFollowTheRemainingTime`。第 3 步没导出时,`AfkPassSystemTest` 会在 SetUp 报 `AfkPassRule has not been exported`。
   - `currency_test`:`ClientGmGateTest.*`。
   - `buff_test`:`BuffKeepOnLogoutTest.*`(含 `RestoreCarriesRecordsOfRowsThisNodeDoesNotKnow`)、`BuffDurationTest.*`(含 `ReapplyingRefreshesTheExpiryToTheFullDuration`)、`BuffStackingTest.*`。
   - `skill_test`:`SkillUtilTest.RestoreCooldowns_*`(含 `RestoreCooldowns_EmptyListWithNewerFieldsIsCarried`)。
   - `turn_battle_engine_test`(`-Filter turn_battle`)也跑一遍。
   - 最后不带 `-Filter` 全量跑一遍,与改动前相比没有新增失败。
9. **Linux**:跑 `tools/scripts/build_linux.sh`(不带 `--skip-generate`)或 Docker 整编。
   - 生成的 CMakeLists 里要有本线新增的 `.cpp`,例如 `actor_move_broadcast.cpp`、`afk_pass.cpp`、`player_afk_pass_handler.cpp` 与新表文件;链接无未定义符号。
   - 构建目录要放在 Linux 文件系统里。Windows 绑定挂载大小写不敏感,会掩盖 include 的大小写错误。
10. **客户端**(`D:\luyuan\mmorpg-client`):
    - `pwsh -File tools/client_compile_check.ps1 -ShowErrors 40`,要求 0 error。
    - Unity 6000.6.0f1 batchmode:`-runTests -testPlatform EditMode -testFilter "MmorpgClient.Tests.EditMode.Tianyong.TeamFollowDeclarationTests|MmorpgClient.Tests.EditMode.Tianyong.TeamRegroupPolicyTests|MmorpgClient.Tests.EditMode.Tianyong.ActorWorldFoundationTests|MmorpgClient.Tests.EditMode.Tianyong.AfkPassClientTests|MmorpgClient.Tests.EditMode.Tianyong.WorldHudNavigationLifecycleTests|MmorpgClient.Tests.EditMode.Tianyong.MysteryMerchantClientTests"`。
    - 许可不可用时,改用 PROGRESS 10-10 条目里的离线 NUnit 运行器,并在结果里注明。
11. **robot 冒烟**:本地全套;先按 §6.2 清数据;两条都在 `robot/` 下跑。
    - **组队跟随**:`.\robot.exe -c etc/team_follow_smoke.yaml`,通过时输出 `TEAM_FOLLOW_SMOKE_OK`。
      - F1 / F6 要拉开 60(视野 50 之外),每步走 16,一个方向最多走 8 步。
      - 报「8 个方向都没能让…离开 60 以上」时,记下地图与位置,**不要改阈值重跑**。
      - 真实客户端模式是 `team_follow_smoke.client_follower: true`,通过时输出 `TEAM_FOLLOW_CLIENT_OK`。C4 要求停步间距在 2.0~4.6。
    - **挂机月卡 + 托管跟随**:`.\robot.exe -c etc/afk_pass_follow_smoke.yaml`,通过时输出 `AFK_PASS_FOLLOW_SMOKE_OK … same_node=… cross_node=…`。
      - 前置写在该 yaml 文件头:同一 zone 两个 scene 节点、`SCENE_RUN_MODE=dev`,起步地图至少 3 条分线且分布在两个节点上。
      - scene 日志里要有 `metric=afk_pass_bought`、`afk_pass_gm_set`、`afk_hosting_started`,不能有 `afk_pass_projection_failed`。
      - P3 期间,玩家域 Redis 里 `GET player:afk_pass:{<robot_9308 的 player_id>}` 等于 P1 设的到期秒,`TTL` 约等于剩余秒数。
    - **冷却**(手工):
      1. 放一个有冷却的技能后下线。
      2. 等 25 秒以上(断线保留结束)再重登,立刻再放,应回 `kSkillCooldownNotReady`。
      3. 跨节点换图后再核一次。
12. **联机验收**:新客户端,两个账号,A 是队长,B 是队员。11-10 / 11-12 / 11-13 三条各有步骤清单。
    - B 开通月卡后杀掉客户端:B 留在 A 视野里,跟在 A 身后约 3 米。
    - A 同节点换线、跨节点换线,B 都跟过去。
    - B 重登后,客户端立刻显示「正在跟随」。
    - 视野约 50 米。
    - 反例:B 没有月卡时,25 秒后按断线退出,scene 日志里是 `reason=session_not_live`。
13. **压测**:视野从 10 放到 50 的性能结论,必须按 `AGENTS.md` §6 完整跑一轮人群移动压测,比较放大前后的 AOI 重判耗时与广播量。没有对比表不下结论。这一步不是本次验证的通过门槛。

## 4. 旧条目里已失效的口径(以本节为准)

- **开通月卡不能再直接写 Redis**。11-06 第 3 条与 11-10 / 11-12 / 11-13 联机验收里写的 `SET player:afk_pass:{id} <now+3600>` 自 11-19 起**无效**。
  - 月卡的事实源是存档 `PlayerUint64Comp.afk_pass_expires_at`,scene 的托管判定只读它。Redis 键只是投影,供 player_locator 给断线会话续租。
  - 改用以下任一办法开通:
    - dev / test 下的 GM `GmSetAfkPass(剩余秒数)`。
    - 客户端「历练 → 挂机月卡」面板用灵石开通。
    - 直接跑上面的 robot 月卡冒烟。
  - 客户端开发包的本地 GM 面板**没有**设月卡按钮,这是定案。
- **协议生成用默认配置**。11-06、11-12 两条写的是 `-ConfigPath … proto_gen.no-unity.yaml`,那是客户端授权之前的口径。
- **robot 冒烟不再需要直线 72 米**。11-19 第一条里的这个要求,已被同日「遗留项清零」改成分步走,每步 16 米。
- **robot 里有过时的注释**(只是注释,不影响行为;请顺手改成 60,并说明是分步走):
  - `robot/team_follow_smoke_scenario.go` 文件头第 7、14 行写的「>= 12」,实际常量 `teamFollowSmokeSeparation` 是 60。
  - `robot/etc/team_follow_smoke.yaml` 文件头的 F1「离 B 12 以上」与前置 4「走出 12 以上」。

## 5. 风险与要特别看的

- **视野 ×5 的代价**:AOI 扫描面积约 6 倍,视野内人数最多约 25 倍。单测只验几何,性能见第 3 节第 13 步。
- **月卡投影写失败**:只告警(`metric=afk_pass_projection_failed`),不阻断开通;投影缺失时 player_locator 不给断线会话续租。冒烟日志里要确认没有这条告警。
- **多个会话同时改过的文件**:`player_lifecycle.cpp`、`go/scene_manager/internal/logic/` 下两份 `enterscenelogic.go`、`cross_zone_test.cpp`、`cpp/generated/table/table.vcxproj{,.filters}`。编译错误落在这些文件里时,先分清是谁的改动。
- **服务器只驱动两类角色**:发了 `TeamFollowC2S` 声明跟随的客户端,以及离线托管角色。没升级的旧客户端保持原来的归队行为。
- **相关常量连在一起**:间距 3 米、归队阈值 24、视野 50、AOI 格子 50 之间有 static_assert 连着。改其中一个,编译会提示连带要改的常量。

## 6. 不在本交接范围

- 其它会话的线:AOI 四项(11-11:距离重判、战斗关注、仇恨名单、压力裁表);11-12 的进视野朝向、补发移动、冻结停步;11-20 的账号注销。
- Java 版。
- 挂机月卡设计稿 §12 列出的非目标,例如真钱直购、自动续费。

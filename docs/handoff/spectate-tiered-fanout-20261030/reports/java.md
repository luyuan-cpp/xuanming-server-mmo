# JAVA 交付说明（Java 仓 `D:\luyuan\xuanming-server-mmo`；未编译、未运行，待 Codex 验证）

## 一、改动文件（路径前缀 `game-match/.../match/`、`game-battle/.../battle/`）
**可见性闸（match）**
- 新增 `spectate/SpectateVisibilityPolicy.java`：平铺决策表（:38）、`decide`（:64）、`isPublic`、`createRequestPublic`（:95，空 / 坏字节 false）。
- `proto/game/match/battle_placement.proto`：字段 12 `uint32 spectate_visibility`（存数值，不 import 同步 proto）。
- `gather/GatherPipeline.java:342,344`、`global/GlobalMatchService.java:493,571`：建房时决定并冻结，落点抄同一份原件。全球开局不经 GatherPipeline 组请求，所以两处都改。
- `global/GlobalWatchAuthority.java`：`placement`（:230）带可见性；`list`（:233）一次扫 100 行、`pick`（:240）随机起点看 16 行并回绕，都按原件跳过私密局；`prepare`（:149）提交点复核。`global/GlobalPlacementStore.java:23` 带可见性。
- `spectate/SpectateGatherHooks.java:166`：非公开只留落点。
- `spectate/WatchBattleService.java`：:197-221 指定场在互斥检查之后、清退旧观战之前读一次并判定，循环复用该快照；:269 随机模式复核；:412-465 `Target` / `classify`。
- `spectate/GlobalWatchService.java`：:58-59 清退之前拒绝，:77 复核，:100 `watchable`。
- `spectate/WatchableListService.java:128`：过滤并只摘成员。
- `metrics/MatchMetrics.java`：`WatchOutcome.PRIVATE`、`IndexEviction.PRIVATE`。`spectate/SpectateStore.java` 只改 `Invalid` 注释。

**battle**
- `room/BattleRoom.java:110` `spectatePublic`。
- `room/BattleRoomServiceImpl.java`：:447 第二道闸（1005，留墓碑）；:462 节点配额（16021）；:920、:925、:1057 观众帧扇出。
- `room/BattleRoomService.java` 契约注释；`room/RoomDependencies.java` :50 配额字段、:58 `observerSeatQuotaFor`；`room/RoomTable.java:75` `sum`；`BattleNode.java:191` 装配注入。
- `room/DirectLink.java`：:47 `SharedFrame`、:76 `share`、:84 `send(SharedFrame)`（缺省逐条写原帧）。
- `push/BattleOutbound.java`：:94 `Recipient`、:102 `pushBattleFrameToAll`（中途抛异常也释放原件）。
- `edge/DirectSession.java:102,118`；新增 `edge/EncodedFrame.java`（`retainedDuplicate` + `close` 释放原件）。

**`PARITY.md`**：:413「观战 P0 缺陷」行（说明里的第 374 行，已挪位）备注末尾追加一句；:418 新增一行。

**本地模式原操作：未改代码。** `add334cb` 已落码（`LocalObserverOperation`、`DefaultObserverDialer`、`SpectateGatherHooks`、`WatchBattleService`，game-battle `LocalObserverRpcLoopbackTest` 直连真实 `BattleRoomServiceImpl`）。逐条核对说明要求，都已满足：规范 UUID、now + 25s、实例取落点且为空时 fail-closed、Remove 用同一原件、同场重看先取消。

## 二、测试
**新增**
- `SpectateVisibilityPolicyTest`（8 个，含「每个 MatchMode 都在表里」）。
- `WatchBattleServiceTest`：`可见性闸_` 开头 5 个（指定私密局无副作用、记录不在同样不清退且回包逐字节相同、残留成员只摘成员、随机跳过、漏填按私密）。
- `GlobalWatchServiceTest`：`可见性闸_` 开头 4 个。
- `WatchableListServiceTest`、`SpectateGatherHooksTest`、`GatherPipelineTest` 各 1 个。
- `GlobalMatchStoreIntegrationTest`：`可见性闸_SQL列表与随机选场…`（`MATCH_IT_TIDB` 门控）。
- `ObserverVisibilityQuotaTest`（第二道闸、节点配额、配额公式）。
- `EncodedFrameTest`（线上字节相同、引用计数、写失败、不活直连）。
- `BattleOutboundTest`：`观众帧扇出_` 开头 3 个。

**修改**
- 夹具补 PUBLIC：`RoomHarness`（另加 `observerSeatQuota`）、`E2eSupport`、`WatchBattleHandlerTest`、`WatchBattleConfigurationTest`、`ListWatchableHandlerTest`、`WatchableConfigurationTest`、`SpectateFlowRedisIntegrationTest`、`GlobalMatchStoreIntegrationTest.watchable`。
- 钉值同步：`MatchProtoTest`（字段 12）、`MatchMetricsTest`（新标签）、`LocalObserverRpcLoopbackTest`（构造器）。
- 原先依赖「1V1 可观战」，改用 PVE 组队凑单：`GatherHooksCallSiteTest` 6 个用例（`publicPopped`，「五个入口」改为按入口断言公开 / 私密）、`CrossZoneGatherTest` Z12。
- 改了既有断言：`WatchBattleServiceTest` 第 9 行用例，离线时指定场也会先读一次落点。

## 三、给 Codex 的命令（Java 仓根目录，按序）
1. `java tools/ContractSync.java --mmorpg ../xuanming-server-mmo-cpp --check`。不一致先协调再跑写入模式；地图覆盖报不一致时按 Java `AGENTS.md` §1 加 `--maps-source <地图源>`。
2. `./mvnw -B clean install -DskipTests`
3. `./mvnw -B -pl game-battle -am test`
4. `./mvnw -B -pl game-match -am test`
5. 可选真环境：加 `-Dgame.it.redis=redis://127.0.0.1:6379` 跑 `SpectateFlowRedisIntegrationTest`；设 `MATCH_IT_TIDB` 跑 `GlobalMatchStoreIntegrationTest`。

通过标准：全部 exit 0，上面列出的类全绿；另看 `ObserverTest`、`ObserverOperationRoomTest`、`LocalObserverRpcLoopbackTest`、`FinishAndVoidTest`、`RoundTimerTest`、e2e `BattleNodeEndToEndTest` 不回退。失败时保留 surefire 报告与用例全名。

## 四、未完成项、风险与待拍板
1. **待拍板：robot `BattleCrossZoneScenario` Z5 / Z6 观战跨区 1V1，运行时会被 16018 拒绝，本批未改**（不在任务范围）。Go 的跨区场景没有观战步。建议删掉这段，或改为断言 16018。
2. **待拍板：全球 SQL 没有可见性列。** 私密局很多时列表会变短，随机窗口全是私密局时回 16017；全球模式的 `game_match_watchable_battles` 含私密局。标准解法是 `match_global_formation` 加列和复合索引（要改 `GlobalMatchStore.saveIntent` 并做迁移），建议并入全服匹配第二批。
3. **与 Go 的差异**：全球路径 Java 在清退旧观战之前拒绝（任务说明要求），Go 是先清退后判定，两边私密局与不存在同口径，都没有探测旁路。Java 还在提交点复核，Go 没有。
4. **行为变化**：本地与全球的指定场，记录不在时都不再先清退旧观战；本地记录损坏同样不清退。
5. **PVP_TEAM** 按默认拒绝判私密（与 Go 同），请确认。
6. robot `BattleScenario` 第 16 步经 dev 口登记观众，本来就缺原操作三字段（账本回 1005），与本批无关；dev 建房也没带可见性。
7. 工作区已有 ContractSync 写入结果（`SOURCE.properties` 的 `mmorpg.commit=72017963…`，表数据 dirty），本批代码依赖它带来的 `eSpectateVisibility`、`MATCH_MODE_PVP_TEAM`。工作区还混有其他会话改动（`GlobalMatchStore`、login、scene 等），编译失败先确认是否本批文件。
8. mmorpg `battle-spectate-tiered-fanout.md` §10.1 / §15.8 与 `docs/PROGRESS.md` 未改，需主会话登记。

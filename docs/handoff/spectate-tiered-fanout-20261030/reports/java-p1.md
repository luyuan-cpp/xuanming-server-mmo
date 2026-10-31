# JAVA-P1 交付说明

**未编译、未运行，待 Codex 验证。** 路径相对 Java 仓，工作区混有其他会话改动。

## 改动
- game-common：新增 `token/SpectateTickets`、`SpectateSecretPolicy`。
- game-discovery：`RedisKeys`；新增 `spectate/SpectateFrameChannel`。
- game-battle（含 game-api `BattleNodeService`）：`BattleRoomServiceImpl`（`attachRelayGate`、`enableSpectateRelay`、`publishSpectateFrame`）；新增 `port/SpectateFrameSink`、`RedisSpectateFrameSink`；装配与指标。
- game-gate：新增 `spectate/SpectateHub`、`session/SpectateSubscriptions`；`ClientDispatcher.onSpectateRequest`、`MessageRoutes`、`ClientSession.writeEncodedBelow`、`GateNode`、`GateConfiguration`。
- game-match：新增 `spectate/SpectateRelay`、`SpectateRelayDialer`、`SpectateTierStore`、`RedissonSpectateTierStore`、`SpectateRelayConfiguration`、`SpectateFeaturedService`、`admin/SpectateFeaturedController`；`WatchBattleService`、`GlobalWatchService.replayLobby`、`WatchableListService`、`MatchTips`、`MatchProperties`。
- 修正 2：`global_match_tables.proto`（`spectate_public`）、`GlobalMatchStore.saveIntent`、`GlobalWatchAuthority.list / pick`。
- 修正 1：`BattleCrossZoneScenario`（Z5 断言 16018；`crossZoneWatch` 走 PVE 正向，步骤 `z5-public-watch`）、`game-robot/README.md`。
- **说明没列但必须改**（gate 缺省 prod，没密钥拒启）：`deploy/compose/*`、`deploy/ci/stack-secrets.sh`、`tools/local/{start-slice,battle-crash-window}.sh`、`ReleasePolicy.SECRETS`、`release-plan.example.json`。
- `PARITY.md` 加一行。

## 测试
- 新增：`SpectateTicketsTest`、`SpectateHubTest`、`ClientSpectateRequestTest`、`SpectateRelayRoomTest`、`RedisSpectateFrameSinkTest`、`WatchBattleLobbyTest`、`SpectateFeaturedServiceTest`；真 Redis：`SpectateFrameChannelIntegrationTest`、`RedissonSpectateTierStoreIntegrationTest`。
- 改：`MessageRoutesTest`、`RedisKeysMatchTest`、`MatchTipsTest`、`MatchMetricsTest`、`WatchBattleServiceTest`、`WatchBattleHandlerTest`、`WatchableConfigurationTest`、`GlobalWatchServiceTest`、`WatchableListServiceTest`、`GlobalMatchStoreIntegrationTest`、`MatchApplicationContextTest`、`BattleCrossZoneScenarioTest`、`SliceScriptsTest`。

## 给 Codex（Java 仓根目录，按序）
1. `java tools/ContractSync.java --mmorpg ../xuanming-server-mmo-cpp --check`：一致；不一致先同步。
2. `./mvnw -B clean install -DskipTests`：0 错误。
3. `./mvnw -B -pl game-common,game-discovery,game-data,game-battle,game-gate,game-match,game-robot -am test`：全绿。
4. 真 Redis 7.2 与 8.x 各一轮：`./mvnw -B -pl game-discovery,game-match -am test -Dgame.it.redis=redis://127.0.0.1:6379 -Dtest='SpectateFrameChannelIntegrationTest,RedissonSpectateTierStoreIntegrationTest' -Dsurefire.failIfNoSpecifiedTests=false`：无 SKIP。
5. 设 `MATCH_IT_TIDB` 跑 `GlobalMatchStoreIntegrationTest`：无 SKIP。

失败保留 surefire 报告与完整用例名，先判断是否来自本批。

## 未完成、风险、待拍板
1. robot 不会 `SubscribeSpectate`，大厅档没有端到端覆盖。
2. 全局库旧库缺新列要重建；compose 旧 `.env` 要补 `GAME_SPECTATE_TOKEN_SECRET`。
3. **待拍板**：`GAME_MATCH_SPECTATE_RELAY_ENABLED` 缺省 false；C++ K8s 写死 true，Java 整栈要不要默认开。
4. **待拍板**：Go / C++ 测试没有固定向量，`SpectateTicketsTest` 里的一组（Python 独立算出）可抄过去。
5. 写键与发布不原子，丢帧下一帧自愈。
6. 已结束的中继局与赛事局留在列表到过期分界，点进去收 166。
7. 未改：`spectate-spec.md` §10.8、`PROGRESS.md`、设计 §15.8；PARITY B 批行的「待拍板」由新行取代。

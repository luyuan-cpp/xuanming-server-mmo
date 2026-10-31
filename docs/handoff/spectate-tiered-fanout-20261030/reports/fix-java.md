# FIX-JAVA 交付说明

**未编译、未运行，待 Codex 验证。** 路径相对 Java 仓。评审各条动手前都核过，全部仍成立，没有跳过的。

## 1. 每条的处理

| 条目 | 处理 |
|---|---|
| 严重-1 | 房间 `spectateTail`：上一帧有结局（失败也算）才发下一帧，逻辑线程不等；`SpectateFrameSink.publish` 改回 `CompletionStage<Void>`；`SpectateFrameChannel.publish` 改成 SET 完成再 SPUBLISH，SET 失败照发 |
| 严重-2 | `RedisProperties.subscriptionsPerConnection`（缺省 5）；gate yaml 配 500（容量 25000 个频道）；§6.1 总账已同步。订阅失败计 `game.gate.spectate.channel.failures` 并当场 `SOURCE_LOST` |
| 严重-3 | `GlobalWatchAuthority.settled` 没证明时在同一笔 P 锁事务里判：成局 state 5、已过期限 + 宽限、目录同 node_id 已是别的实例（`SqlSpectateStore.settle` 在事务外查好传入，查不到不算）。原实例明确回 ACTIVE 不关。指标 `game.match.watch.unproven.close{reason}` |
| 一般-1 | battle：`ObserverOperationLedger.reject / firstRejection`，同 op 同请求重放回首次码；被 Remove 封闭的没有记录，维持 1003。match：`observedFull` 一笔事务转待取消并写 LOBBY 回执，之后 `admitLobby`（与 `replayLobby` 同路）；新增 `Admission.Unavailable`，没调通回 16004 不持久化 |
| 一般-2 | 新类 `BattleInstanceCheck`（缓存 3 s，上限 1024 条），`SpectateRelay.lobby` 签票前查。本地路径同时懒剔除，随机模式换下一场 |
| 一般-3 | `SpectateHub.sweep` 判失联前先读键：同实例 END 按真实终局收尾；读键期间来了新帧不收尾；其余 `SOURCE_LOST`。丢最后一回合不补，写在类注释 |
| 一般-4 | `purgeRequests`：一分钟一页、每页 1000 行，按 (created_ms, 主键) 翻页；跳过请求头、当前原件、有待退出准入的玩家（pending 票、未完结 Join、未释放的 Team 角色）。新增索引 `created_ms,player_id,request_id`，指标 `game.match.watch.requests.purged` |
| 一般-5 | `WatchBattleService.settle` 的 INFO 降 DEBUG |
| 一般-6 / 小项 | 测试见第 3 节；`finish` 清 `latest / firstFrame`；晚到的第二个 attach 句柄当场退掉 |
| 默认打开 | 评审表逐处改完。另有两处 prod 上下文用例要显式关开关，否则先撞密钥门禁 |
| 键 TTL、结束释放 | **不改**。全局与本地期限都是 300 s，TTL 与分界 360 s 盖得住；没有全服配额；档位键靠 TTL，赛事局 ZSET 读时过滤、标记时清理。以后移植模板时长，`RedissonSpectateTierStore.TTL`、`SpectateRules.staleCutoff`、`WATCHING_TTL_SECONDS` 要一起改 |

相对评审的取舍：
- 本地路径 Attach 没调通也回 16004（原 16018），与全局同口径。
- `doSubscribe` 把 `attachChannel` 挪到进订阅表之后，否则同步执行器上观众收不到失败收尾；生产是单线程排队，时序不变。
- 请求头与当前原件在 Java 里比，不写进 DELETE：这两列是 `unicode_ci`，请求行主键是 `0900_bin`，跨表比有排序规则混用风险。

## 2. 改动文件

- game-discovery：`spectate/SpectateFrameChannel`、`RedisProperties`、`RedisAutoConfiguration`
- game-battle：`port/{SpectateFrameSink,RedisSpectateFrameSink}`、`room/{BattleRoom,BattleRoomServiceImpl,ObserverOperationLedger}`
- game-gate：`spectate/SpectateHub`、`metrics/GateMetrics`、`application.yaml`
- game-match：`spectate/{SpectateRelay,SpectateRelayConfiguration,GlobalWatchService,WatchBattleService,SqlSpectateStore,SpectateStoreConfiguration}`、新增 `spectate/BattleInstanceCheck`、`global/GlobalWatchAuthority`、`metrics/MatchMetrics`、`MatchProperties`、`application.yaml`、`global_match_tables.proto`
- 其它：`deploy/compose/{stack.yaml,env.example}`、`tools/local/start-slice.sh`、`docs/design/architecture.md`、`docs/porting/battle-node-spec.md`、`PARITY.md`（只改 P1 行）

## 3. 测试

- 新增类：`SpectateFrameChannelTest`、`BattleInstanceCheckTest`、`SqlSpectateStoreDriveTest`、`GlobalWatchFormationEndedTest`
- 真 Redis `SpectateFrameChannelIntegrationTest`：`最后一个订阅者退订后立刻重订_…`、`连发TURN加END_循环多次_…`、`按gate的配置订300个频道都收得到帧`
- 真 TiDB `GlobalMatchStoreIntegrationTest` +10：不带证明关闭、ACTIVE、满员转 LOBBY、保留期 5 条
- `SpectateRelayRoomTest` +3、`RedisSpectateFrameSinkTest` +3、`ObserverOperationLedgerTest` +1、`ObserverOperationRoomTest` +1、`SpectateHubTest` +9（改 1）、`GatePropertiesTest` +1、`GlobalWatchServiceTest` +7（改 1）、`WatchBattleLobbyTest` +4（改 1）
- 随改：`SpectateFeaturedServiceTest`、`MatchApplicationContextTest`、`MatchMetricsTest`、`RedisPropertiesTest`、`RoomHarness`

## 4. 给 Codex（Java 仓根目录，按序）

1. `java tools/ContractSync.java --mmorpg ../xuanming-server-mmo-cpp --check`：一致；不一致先同步。
2. `./mvnw -B clean install -DskipTests`：0 错误。
3. `./mvnw -B -pl game-common,game-discovery,game-data,game-battle,game-gate,game-match,game-robot -am test`：全绿。
4. 真 Redis 7.2 与 8.x 各一轮：`./mvnw -B -pl game-discovery,game-match -am test -Dgame.it.redis=redis://127.0.0.1:6379 -Dtest='SpectateFrameChannelIntegrationTest,RedissonSpectateTierStoreIntegrationTest' -Dsurefire.failIfNoSpecifiedTests=false`：无 SKIP。
5. 设 `MATCH_IT_TIDB`：`./mvnw -B -pl game-match -am test -Dtest=GlobalMatchStoreIntegrationTest -Dsurefire.failIfNoSpecifiedTests=false`：无 SKIP。重点看 `请求历史保留期_*`（关联 NOT EXISTS 的 DELETE、元组翻页只有这里跑真 TiDB）。
6. 红绿（可选，改完还原）：去掉 `spectateTail` 直接发，`上一帧没有结局时…` 应失败；`SpectateFrameChannel.publish` 改回同时发，`SET完成之前不发布…` 应失败。

失败保留 surefire 报告与完整用例名，先判断是否来自本批。

## 5. 风险与需要主会话处理

1. **dev 库要重建**：normal 启动缺索引拒启。
2. **默认打开之后**：prod 的 match 没配 `GAME_SPECTATE_TOKEN_SECRET` 拒启；Redis 低于 7 时大厅档观众当场收 `SOURCE_LOST`。
3. **只有 LOBBY 结论是一笔事务**：中继关闭时的 16021、1004 仍是两笔，中间失败且恢复循环先关闭后，同 UUID 重试得 16018，换 request_id 可恢复。按最小修法没动。
4. 崩溃实例上已完成的局：Java 成局 COMPLETE 后 state 仍是 4，只能等期限 + 宽限或目录换实例，最长约 6 分钟。
5. Redis 故障时发布有积压：单帧结局最长约 8.4 s，一场至多约 150 帧，房间收尾后继续发完，有上界，没做丢帧。
6. 保留期：没有回执也没取消的旧行不删；有待退出准入的玩家整名跳过。
7. **未做**：robot 不会 `SubscribeSpectate`，大厅档仍无端到端（CODEX.md 第 6 节）。
8. **接手**：`reports/java-p1.md` 待拍板第 3 条的结论就是这里的「默认打开」，该文件没改；评审建议把「丢最后一回合」写进设计 §15.7，本批按任务说明只写了注释；`docs/PROGRESS.md` 未写。
9. 过程偏差：`SpectateFrameSink.java` 是用 Write 整体重写的，违反「只做外科式 Edit」。该文件是本批未跟踪文件，重写前刚读过，内容只多了本次改动。

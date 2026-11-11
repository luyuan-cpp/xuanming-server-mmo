# 观战分级扩容：给 Codex 的执行清单（2026-11-05 核对）

服务端实现及 FIX 三批已落盘；整批观战专项验收尚未闭合。`reports/` 中“未编译、未运行”是各代理交付时的状态，后续已有部分运行证据，见第 0 节。本清单取代 `README.md` 的历史执行顺序；各批细节与失败时要保留的内容见 `reports/`。
任一步失败即停，保留输出（gtest 全文、`go test -v` 全文、msbuild 错误段），不改断言、不跳过。
工作区混有其他会话的改动：编译失败先看失败点属于哪个文件，不是本任务的报告即可，不要顺手改。
C++ MSBuild 一律串行 `/m:1 /nr:false /p:CL_MPCount=1`。项目未上线：本轮专用 dev 库需清空重建以带上新列和索引；执行前核对实例、库名与其他正在运行的验证任务。本次文档核对没有清库或运行生成、构建、测试。

## 0. 重启后的状态核对（2026-11-05，America/New_York）

### 0.1 提交与 Java 修正落盘

- 主仓快照 `fc504300b` 是核对时 HEAD `993b6da9d` 的祖先；`reports/fix-java.md` 已包含在该快照中。Java 快照 `a6d2e137` 是当前 HEAD `0b578ea7` 的祖先。两个快照均非当前工作区的完整基线，后续验收必须记录实际 HEAD 与未提交输入。
- 按 `reports/fix-java.md` 的文件清单展开，核对了 **40 个 Java 源码/测试文件及 8 个配套文件**：两份 application.yaml、`game-match/src/main/proto/game/match/global_match_tables.proto`、compose 的 stack.yaml / env.example、start-slice.sh、architecture.md、battle-node-spec.md。磁盘内容经 Git 属性规范化后的 blob 均与 `a6d2e137` 一致，没有缺失或本批未提交差异。`PARITY.md` 的观战 B / P0 与 P1 / P2 两行仍在；该文件另有其他任务的更新，本次未改。
- 同时核对关键实现仍在：`BattleRoom.spectateTail` 串行发布、SET 完成后 SPUBLISH、gate 每连接 500 个订阅、无证明关闭的事务判据、满员转 LOBBY 的单事务回执、首次拒绝码重放、`BattleInstanceCheck`、失联前读取最新帧、7 天请求保留期与索引、relay 默认 true。以上是**落盘与静态核对**，不是第二轮语义评审或运行验收。
- Java 版本仍为 `0.1.0-SNAPSHOT`。Java 的 L1 / L2 合成按场 Redis 分片 pub/sub，没有 C++/Go 的全服 4 场配额；300 s 战斗期限与 360 s 档位 TTL 的既有差异见 `reports/fix-java.md`，不能把 Go 的 TTL / 配额修正要求机械套到 Java。

### 0.2 近两天其他会话与文件变动

检查了 11 月 4—5 日的 Git 提交、当前工作区，以及活跃/归档聊天的相关消息：

| 来源 | 与本批有关的变化 / 边界 |
|---|---|
| 「双区恢复与剩余 3 项真库联验」、其协调聊天「重跑玩家存盘第二轮验证」 | Gate 身份、重启认证和路由已变。主仓 `372caca25` / `ac6cc026c` 涉及 gate_event_handler、client_session_handler、session_details_builder、gate_service_handler、gate/main，以及 match/schema 和 battle_smoke_scenario；Java `83d57c8f` 改了 ClientDispatcher 及配套测试。观战经过的公共入口必须在新基线上回归。 |
| 「编译服务器并配置远程调试」 | 已完成 Linux Debug 三节点构建、本机隔离调试栈启动和 L0 双客户端观战冒烟，证据见下；不等于 L1 / L2 验收。 |
| 「改造队伍跟随 Redis 协程」 | 正在改 Scene 任务生命周期、共享构建接线和队伍机器人；其测试/部署会用到共享资源。 |
| 「推送所有改动并检查客户端」 | 已原样保存并推送主仓至 `993b6da9d`，该聊天明确没有运行编译测试；提交成功不是验收结果。 |
| 「修复 Go 和 Java 幂等查询故障」 | 正在处理公会与客户端恢复；与本批共用 PROGRESS / PARITY。它遇到的 shared/loginsession 缺失来自在途身份迁移，不能归因于观战修正。 |

核对期间另见未提交的 `cpp/nodes/gate/main.cpp`、`go/match/match_service.go`、`go/match/internal/logic/gather.go`、`proto/battle/battle_node.proto` 及公共 proto / 部署配置变动。`specs/85-local-mark-consistency.md` 也已有他人的作废说明改动。均保留；不能用旧快照覆盖，也不能仅凭文件时间认定改动归属。本次仅收尾本交接入口、设计状态和追加 PROGRESS。

### 0.3 已有证据与仍缺项目

| 项目 | 已核对的证据 | 结论边界 |
|---|---|---|
| C++/Go 本机调试栈与 L0 | `build/verification/docker-debug-refresh-20261104-r2/runtime-checks/local-acceptance-serial-r1/result.json` 为 passed；相邻 game-smoke.exit.json 退出 0；game-smoke.log 记录两名机器人、战斗/观战及两项直连计数各 9。发生于 2026-11-05 11:24（纽约时间）。 | 证明该次产物的 L0 直连观战；不证明当前在途改动、LOBBY 订阅、升档、广播配额或 Linux 容量上限。 |
| Java 已运行的观战单测 | Java 仓 `game-common/target/surefire-reports/TEST-com.game.common.token.SpectateTicketsTest.xml`：7/7；`game-discovery/target/surefire-reports/TEST-com.game.discovery.spectate.SpectateFrameChannelTest.xml`：3/3；`game-gate/target/surefire-reports/TEST-com.game.gate.spectate.SpectateHubTest.xml`：37/37。均 0 失败/错误/跳过，文件时间为 11 月 5 日。 | 是现存报告的范围，不是 game-battle / game-match 全套及真库验收；本次只读取报告。 |
| Java 真 Redis | 同目录 `TEST-com.game.discovery.spectate.SpectateFrameChannelIntegrationTest.xml`：4 项全部 skipped。 | **未通过真 Redis 验收**；Redis 7.2 / 8.x 两轮仍需执行并保留版本、环境和 0 SKIP 证据。 |
| FIX 三批完整回归 / 真 TiDB | 尚未核齐对应全部用例与输入版本的运行回执；Go 准入窗口用例 `TestSpectatorTiDBAdmitWindowLeavesClockSkewMargin` 仍在待验证记录中。 | 继续按下面顺序补齐；旧报告和其他功能的绿不能代替。 |
| L1 / L2、Unity、容量 | Go robot 有生成的 SubscribeSpectate RPC，业务冒烟仍只走直连；Java 报告也仍登记大厅档 robot 缺口。本次未读取/修改 Unity。 | 两版 LOBBY 端到端、客户端接入、Linux 帧大小与扇出上限仍待对应任务验收。 |

FIX-CPP / FIX-GO / FIX-JAVA 没有第二轮完整语义评审的记录，后续优先核对这三批。Java 另有 robot dev 观战原操作字段、本地 163 跨会话栅栏两项待办，保留 `PARITY.md`「观战 P0 缺陷」行的原口径，不因本次落盘确认而关闭。

## 1. 生成（以本次实际输入重跑；旧生成记录不能覆盖后续 proto / Tip / 生成器改动）

1. 导表（`MatchSpectateFull`、`MatchSpectateTicketInvalid` 已发号；Tip 可能还有其他会话的新行）：
   复制 `tools/data_table_exporter/exporter_config.yaml` 为同目录 `exporter_config.local.yaml`，把 `csharp.deploy` 设为 `[]`，
   `py -3 tools/data_table_exporter/run.py tools/data_table_exporter/exporter_config.local.yaml`，跑完删掉 local 配置。
2. 生成器源码变了（`gate_kafka_command_router.go` / `.tmpl`）：
   `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`
3. `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`
4. 核对：`cpp/nodes/gate/handler/event/gate_kafka_command_router.cpp` 里 9 个入口事件的信封分支都变成拒绝（`RejectEmptyPayload`）；
   `reports/cpp-gate.md` 写的「只有四个 case」已被 `reports/p1-gate.md` 取代（所有生产者都带 payload，评审已逐个核对）。
   `cpp/nodes/battle/handler/grpc/battle_node.cpp` 的 `HandleAttachRelayGate` / `HandlePromoteSpectateBroadcast` 守护段保留了委托；
   `SPECTATE_FEATURED_REJECT_DENIED`、`MatchSpectatorIntentRecord.created_ms` 已生成。

## 2. 生成器与 Go

工作目录都是各模块根。
1. `tools/proto_generator/protogen`：`go test ./... -count=1`
2. `go/shared`：`go vet ./kafkautil/...`、`go test ./kafkautil/... -count=1`（含 `TestEncodeBroadcastTargetsMatchesSharedVectors`）
3. `go/match`：`go build ./...`、`go vet ./...`、`go test ./... -count=1`；`go/friend`、`go/guild`、`go/login`、`go/mail`：`go build ./... && go test ./... -count=1`
4. 真 TiDB（`MATCH_IT_TIDB`、`MATCH_IT_REQUIRE_TIDB=1`、`MMORPG_SQL_FIXTURE_*`，见 `reports/fix-go.md`），`go/match` 下：
   `go test ./internal/authority/... -run 'TestSpectatorTiDB|TestSpectatorFormationEnded' -count=1 -v`、
   `go test ./internal/logic/... -run 'TestGlobalWatchTiDB|TestGlobalSpectator|TestCommitBattleOutcomeTiDB' -count=1 -v`、
   `go test ./internal/team/... -count=1`
5. dev 库建表：本轮专用 dev 库清空重建，核对 `close_tip`、`delivery`、`created_ms` 及索引。迁移器只 ADD COLUMN、不补普通索引，不能把单独执行 `match -migrate` 视为本批 schema 已齐；隔离实例/库的选择及重建结果写入回执。

## 3. C++

1. `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`（或依次 proto、table、gate、battle）；新文件已登记进工程：`cpp/nodes/gate/spectate/*`、`gate_push_delivery.*`、`cpp/nodes/battle/logic/battle_spectate_relay.*`。
2. 不在 sln 的测试工程：`msbuild cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj /p:Configuration=Debug /p:Platform=x64 /m:1`，以及 `routing_identity_test`、`kafka_command_test` 对应工程。
3. 运行：
   - `build/cpp/tests/routing_identity_test.exe --gtest_filter=SpectateHub.*:SpectateTicket*:SpectateFrameTargetFilter.*:GatePush*:BroadcastTargetCodec*`
   - `build/cpp/tests/rpc_controller_test.exe --gtest_filter=BattleObserverContract.*:BattleSpectateRelay.*:BattlePushPolicy*:BattleTicket*:BattleRoomTable*`
   - `build/cpp/tests/kafka_command_test.exe --gtest_filter=ValidateCommandTarget.*:BroadcastTargetVectors.*`
4. Linux 构建按现有流程编 gate 与 battle（新文件已进 CMakeLists）。

## 4. 部署脚本

- `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1`
- 本地：`docker compose -f deploy/docker-compose.yml up kafka-topic-init`，核对 `gate-spectate_g<N>`（分区 = 命令 topic 分区数，保留 10 分钟）与 `spectate-bcast_g<N>`（16 分区，保留 10 分钟）存在。
- 新配置：`bin/etc/base_deploy_config.yaml` 的 `SpectateTokenSecret`、`go/match/etc/match_service.yaml` 的 `Spectate` 段已有 dev 值；K8s 要注入 `MMORPG_SPECTATE_TOKEN_SECRET`（必填）与 `MMORPG_MATCH_OPS_CALLER_SECRET`（可空）。

## 5. Java（`D:\luyuan\xuanming-server-mmo` 根目录）

1. `java tools/ContractSync.java --mmorpg ../xuanming-server-mmo-cpp --check`；不一致先和改 game-proto 的会话协调，再跑写入模式。
2. `./mvnw -B clean install -DskipTests`
3. `./mvnw -B -pl game-common,game-discovery,game-data,game-battle,game-gate,game-match,game-robot -am test`
4. 真 Redis 7.2 与 8.x 各一轮（命令见 `reports/java-p1.md` 第 4 步与 `reports/fix-java.md`）。
5. 真 TiDB：设置 `MATCH_IT_TIDB` 后运行 `./mvnw -B -pl game-match -am test -Dtest=GlobalMatchStoreIntegrationTest -Dsurefire.failIfNoSpecifiedTests=false`。必须 0 SKIP，重点保留无证明关闭、满员转 LOBBY、请求历史保留期用例的完整结果；详见 `reports/fix-java.md` 第 4 节。

每步记录执行命令、输入版本/改动、退出码、测试总数与失败/错误/跳过数、日志位置；报告文件存在或生成的 class 存在都不能单独算本轮通过。ContractSync 写入前还须按 Java `AGENTS.md` 核对现有地图来源（需要时显式 `--maps-source`），避免覆盖其他会话同步的契约。

## 6. 冒烟（要先改 robot，这部分归 Codex）

- 现有 `robot/etc/battle_smoke.yaml`（B 随机观战 A 的 PVE_SOLO，走 L0 直连）已有第 0.3 节的历史通过记录，新输入应继续 `BATTLE_SMOKE_OK`。match 只有 SQL 路径，robot 的 WatchBattle 已带 `request_id`。先按 AGENTS §6.2 准备本轮专用环境，让 `spectate:battles:active` 里只剩这一场；SQL 的时钟不能比 battle 快 5s 以上。
- 要覆盖 LOBBY 档，robot 需要新增：收到 `WatchBattleResponse.delivery == LOBBY` 时发 `GateClientSpectate.SubscribeSpectate`（带 `spectate_ticket`），在大厅连接上断言依次收到 161、每回合 158、终局 166；把房间直连席注入小值或用 21 名观众触发满员转中继。客户端按同一流程接入（见设计文档 §6.1：去重时 158 只与 158 比较，161 不推进水位）。
- 客户端代码（Unity 仓）：按 `delivery` 分支、大厅连接挂 158 / 161 / 166、`SubscribeSpectate` / `UnsubscribeSpectate`、`SPECTATE_END_SOURCE_LOST`、新 tip 16021 / 16024、大厅重连后用原 request_id 重发 WatchBattle。未授权本次改客户端，归 Codex / 客户端任务。

## 7. 实测（§11 P0 的遗留，决定阈值）

帧大小、gate 单线程扇出上限还没有实测，`Spectate.BroadcastAutoGateThreshold`（默认 32）、`BroadcastMaxConcurrent`（默认 4）、软水位 256 KB、`kFanoutBatch` 512 都是保守默认值。这些是 C++/Go 配置与实现参数，不代表 Java 的频道容量。扇出上限必须在 Linux 上使用目标构建与部署参数实测；Windows 功能验证及已有 Linux Debug 冒烟均不能替代容量数据。压测按 AGENTS §6。

## 8. 第二轮评审修正的验证（2026-11-05 新增）

修正内容与逐条来源见设计文档 §15.10，评审原文是 `reports/review2-gate.md`、`review2-battle.md`、`review2-go.md`。第 1–3 步照常跑，下面是本批新增或改了期望的部分。任一步失败即停，保留完整输出。

**2026-11-10 核对**：第 3 步（C++）已由 2026-11-08 的全量测试覆盖并通过（`build/verification/naming-audit-20261108/cpp-tests/`，本批 C++ 用例全在其中）；第 2 步的 gofmt / 构建 / 纯单测已由 2026-11-06 的运行覆盖并通过（`build/verification/battle-handoff-20261106-r4/`）。**只剩第 2 步的真 TiDB 那一项**（7 条用例，那两次运行里都是 SKIP）与第 4 步冒烟。工作区里发布轨道会话还在改 `global_spectator.go` / `spectate.go`（给实例核对加 canary 镜像），提交之后跑真 TiDB 时一并覆盖。

1. **生成**：`proto/battle/battle_node.proto` 只改了注释，第 1 步照常重跑即可，生成物只有注释变化。
2. **Go**（`go/match`）：
   - `gofmt -l internal/authority/completion.go internal/authority/spectator.go internal/authority/schema.go internal/logic/global_spectator.go internal/logic/spectate.go internal/logic/listwatchablebattleslogic.go internal/logic/setspectatefeaturedlogic.go`，应无输出。
   - `go build ./...`、`go vet ./internal/logic/... ./internal/authority/...`
   - 单测：`go test ./internal/authority/... -run 'TestSpectatorFormationEnded|TestFormationOutcomeDecided' -count=1 -v`；`go test ./internal/logic/... -run 'TestGlobalFormationWatchable|TestListWatchableBattles|TestReleaseSpectateOnOutcome|TestLobby|TestSpectate' -count=1 -v`
   - 真 TiDB（环境变量同第 2 步第 4 项，必须 0 SKIP）：`go test ./internal/logic/... -run 'TestGlobalWatchTiDB|TestGlobalSpectator|TestCancelGlobalSpectatorTreatsAlreadyClosedAsDone|TestCommitBattleOutcomeTiDB' -count=1 -v`；`go test ./internal/authority/... -run 'TestTiDBCandidateIndexesRequireSpectatorIntentPurgeIndex|TestTiDBAuthorityStartupUsesDMLAccountAndRequiresCurrentSchema|TestSpectatorTiDB' -count=1 -v`
   - 期望变化：`TestGlobalSpectatorClosesWithoutProofOnlyWhenSourceGone` 多一个 case「终局已提交」（3 个入口），「新观战」入口的新场改落在节点 8。
3. **C++**（MSBuild 一律 `/m:1 /nr:false /p:CL_MPCount=1`，`/p:Configuration=Debug /p:Platform=x64`，串行）：
   - 公共头 `node_command_topic.h`（新增观战通道活性常量与静态断言）与 `session_identity_fence.h`（未绑定会话一律丢弃）都改了，测试工程链接预编好的库，必须先重编库再编测试：`cpp/libs/engine/core/core.vcxproj` → `cpp/libs/services/gate/gate.vcxproj` → `cpp/nodes/gate/gate.vcxproj` → `cpp/nodes/battle/battle.vcxproj` → `cpp/tests/routing_identity_test/routing_identity_test.vcxproj` → `cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj` → `cpp/tests/kafka_command_test/kafka_command_test.vcxproj`。
   - 运行：`build/cpp/tests/routing_identity_test.exe --gtest_filter=SpectateHub.*:SpectateTicket*:SpectateSessionIdentity.*:GateIdentityFence.*:KickFence.*:GatePush*`；`build/cpp/tests/rpc_controller_test.exe --gtest_filter=BattleObserverContract.*:BattleSpectateRelay.*`；全量再各跑一次 `pwsh tools/scripts/run_cpp_tests.ps1 -Filter routing_identity` 与 `-Filter rpc_controller`。
   - 期望变化：`BattleObserverContract.ShutdownSendsNoEndFrameWhileOutcomeIsUndecided` 已删除，由 `ShutdownTellsEveryChannelTheSourceIsLostForRoomsStillInProgress`、`ShutdownClosesSpectateAdmission`、`ShutdownKeepsUndecidedOutcomeRoomsOnKeepaliveUntilTeardown`（独立子进程）取代；新增 `ChannelKeepaliveRepublishesStateAfterTheKeepaliveInterval`、`OutcomeWaitKeepaliveCarriesFrozenStateAndPromotedCompleteOnlyBroadcasts`；`AssignmentFailureRemovesOriginalOperationAndNewOperationRecovers` 多了重放断言。
   - Linux 构建照现有流程编 gate 与 battle，确认 `node_command_topic.h` 的静态断言在两个平台都成立。
4. **冒烟**：第 6 步的 L0 冒烟照常；LOBBY 冒烟（robot 支持 `SubscribeSpectate` 之后）再加两条断言：一场手动回合等满 25 s 的中继观战，gate 不在最后一回合前合成 `SOURCE_LOST`；停机一台 battle 时，中继与广播观众立刻收到 166 `SOURCE_LOST`，不等 30 s。

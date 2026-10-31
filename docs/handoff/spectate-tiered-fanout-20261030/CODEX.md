# 观战分级扩容：给 Codex 的执行清单（2026-10-30）

Claude 没有跑任何构建、测试、生成或冒烟。本清单取代 `README.md` 第 3 节；各批细节与失败时要保留的内容见 `reports/`。
任一步失败即停，保留输出（gtest 全文、`go test -v` 全文、msbuild 错误段），不改断言、不跳过。
工作区混有其他会话的改动：编译失败先看失败点属于哪个文件，不是本任务的报告即可，不要顺手改。
C++ MSBuild 一律串行 `/m:1`。项目未上线：dev 库可以直接清空重建。

## 1. 生成（必须重跑一次：10-30 03:18 之后 proto、Tip.xlsx、生成器都改过）

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
5. dev 库建表：清库重建最省事（迁移器只 ADD COLUMN、不补普通索引）；否则 `MATCH_MIGRATION_USER=… MATCH_MIGRATION_PASSWORD=… match -f etc/match_service.yaml -migrate`（`close_tip`、`delivery`、`created_ms`）。

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

## 6. 冒烟（要先改 robot，这部分归 Codex）

- 现有 `robot/etc/battle_smoke.yaml`（B 随机观战 A 的 PVE_SOLO，走 L0 直连）应继续 `BATTLE_SMOKE_OK`。先清空 Redis，确认 match 模式（见 `specs/85-local-mark-consistency.md` 第 6 节）。
- 要覆盖 LOBBY 档，robot 需要新增：收到 `WatchBattleResponse.delivery == LOBBY` 时发 `GateClientSpectate.SubscribeSpectate`（带 `spectate_ticket`），在大厅连接上断言依次收到 161、每回合 158、终局 166；把房间直连席注入小值或用 21 名观众触发满员转中继。客户端按同一流程接入（见设计文档 §6.1：去重时 158 只与 158 比较，161 不推进水位）。
- 客户端代码（Unity 仓）：按 `delivery` 分支、大厅连接挂 158 / 161 / 166、`SubscribeSpectate` / `UnsubscribeSpectate`、`SPECTATE_END_SOURCE_LOST`、新 tip 16021 / 16024、大厅重连后用原 request_id 重发 WatchBattle。未授权本次改客户端，归 Codex / 客户端任务。

## 7. 实测（§11 P0 的遗留，决定阈值）

帧大小、gate 单线程扇出上限还没有实测，`Spectate.BroadcastAutoGateThreshold`（默认 32）、`BroadcastMaxConcurrent`（默认 4）、软水位 256 KB、`kFanoutBatch` 512 都是保守默认值。Windows 版 muduo 的 `wakeup()` 是空操作（每批最多等 100 ms），不要用 Windows 的结果定扇出上限，要在 Linux 上测。压测按 AGENTS §6。

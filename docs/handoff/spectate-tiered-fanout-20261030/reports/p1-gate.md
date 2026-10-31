# P1-GATE 交付说明(未编译、未运行,待 Codex 验证)

## 必须先补(不在本任务范围,未改)

`cpp/libs/engine/config/config.cpp` 按键名逐个读配置,没有 `SpectateTokenSecret`。不补则 prod gate 拒启、dev 订阅全拒。紧跟 `BattleTokenSecret` 那段加:

```cpp
if (root["SpectateTokenSecret"]) { baseConfig.set_spectate_token_secret(root["SpectateTokenSecret"].as<std::string>()); }
```

## 改动文件

- 新增 `cpp/nodes/gate/spectate/`:`gate_spectate_ticket` 验签与密钥强度(纯函数);`gate_spectate_hub` 订阅表与扇出(§15.5,发送、续发、时钟注入);`gate_spectate_client` 装配、真实发送(256 KB 软水位)、两条 RPC 回执、无捕获入口。
- `handler/rpc/client_session_handler.cpp`:GM 闸后拦 `GateNodeService`;断线通知 hub;修过时行号注释。
- `handler/event/gate_event_handler.cpp`:LeaseExpired erase 通知 hub;调用改传 `ContentOf`。
- `handler/rpc/gate_client_spectate_handler.cpp`:守护段只写注释。
- `main.cpp`:密钥门禁、`InstallHub`、L1 / L2 消费者、5 秒巡检。
- `node_kafka_command_handler.h`:`registerLegacyPerNodeTopic`、`assignAllPartitions`,默认值不改现有行为。
- `handler/gate_push_delivery.{h,cpp}`、`handler/rpc/gate_service_handler.cpp`:公开 `EncodedFrame`;单播 / 场景 / 全服缺 `message_content` 整条拒绝。
- 生成器 `gate_kafka_command_router.go`、模板与两个测试文件:`PayloadRequired` 改为「有字段映射不到信封」。
- 登记:`gate.vcxproj`、`.filters`、`CMakeLists.txt`(含 `gate_client_spectate_handler`)、`routing_identity_test.vcxproj`(只 Debug|x64)。

## 测试

- `cpp/nodes/gate/tests/gate_spectate_ticket_test.cpp`:`SpectateTicket.*` 11 条、`SpectateTicketSecret.*` 1 条。
- `cpp/nodes/gate/tests/gate_spectate_hub_test.cpp`:`SpectateHub.*` 36 条、`SpectateFrameTargetFilter.*` 2 条、`GatePushEncodedFrame.*` 与 `GatePushContentOf.*` 各 1 条。
- 生成器:新增 `TestGateKafkaRouterKickWithLoginGenerationRequiresPayload`;两条改名为 `...SameNameDifferentTypeFieldRequiresPayload`、`...EnvelopeOnlyEventKeepsEnvelopeMapping`;其余按新判据改夹具。

## 给 Codex(仓库根,按序)

1. 补上面的 `config.cpp`。
2. `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`,再 `-Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。通过:路由文件信封分支 9 个 case 全变 `RejectEmptyPayload`,载荷分支不变。
3. 目录 `tools/proto_generator/protogen`:`gofmt -l internal/generator/cpp` 无输出;`go test ./internal/generator/cpp/... -count=1` 全绿。
4. MSBuild `/p:Configuration=Debug /p:Platform=x64 /m:1`,串行:`cpp/generated/proto/proto.vcxproj` → `cpp/libs/engine/core/core.vcxproj` → `cpp/nodes/gate/gate.vcxproj` → `cpp/nodes/scene/scene.vcxproj` → `cpp/nodes/battle/battle.vcxproj` → `cpp/tests/routing_identity_test/routing_identity_test.vcxproj`。通过:0 error。
5. `build/cpp/tests/routing_identity_test.exe --gtest_filter=SpectateHub.*:SpectateTicket*:SpectateFrameTargetFilter.*:GatePush*`,再 `pwsh tools/scripts/run_cpp_tests.ps1 -Filter routing_identity -LogDir build/verification/cpp-tests`。全绿;失败保留 gtest 全文。
6. 起 gate 前 `docker compose -f deploy/docker-compose.yml up kafka-topic-init`:两条新 topic 分区数不符 gate 拒启(同 gate-cmd);已被 auto-create 成 1 分区的要先删。
7. robot 冒烟(robot 要先支持 `SubscribeSpectate`,不在本任务):LOBBY 订阅 → 回执 → 161 → 每回合 158 → 166;停 battle 约 30 秒收 `SOURCE_LOST`。日志每 30 秒一行 `metric=gate_spectate`,不应有 `Spectate frame rejected`。

## 需要拍板 / 风险

1. 新判据范围比例子大:9 个入口事件都有信封装不下的字段,信封分支实际只剩拒绝。已核对在树生产者全带 payload。`gate_event.proto` 的 Kick / Bind 信封注释因此过时,proto 未授权,未改。
2. 相对说明的补充,为保住 D69 与收尾:STATE 发给还没收过状态的会话(已在看的不发);首帧被软水位跳过时下一帧补 161;166 不受软水位限制。
3. `MMORPG_MAINTENANCE_CONTROL_DIR` 非空时不注册 L2:`node.cpp` 只接受单分区 manual consumer,否则 gate 拒启。该 gate 的 L2 观众靠空闲巡检收尾,prepare-only 开放后长期服务时同样如此。
4. dev / test 空密钥时不注册两个消费者。指标行多 `instance_mismatch`、`invalid_frames`。每台 gate 多两个 rdkafka 消费者及其线程。

## 未完成

未编译、未测试;客户端接线与 `main.cpp` 只能靠 robot 冒烟。`SECURITY.md`、PROGRESS、设计文档状态不在范围。Java 版 P1 / P2 待做。

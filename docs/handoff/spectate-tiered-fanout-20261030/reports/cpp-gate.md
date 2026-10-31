> **2026-10-30 注**：本报告里「只有四个事件拒绝空 payload」「单播缺内容仍发空帧」「Redirect 未纳入」已被 `p1-gate.md` 取代；以 `../CODEX.md` 为准。

# CPP-GATE 交付说明(未编译、未运行测试,待 Codex 验证)

## 改动文件
1. `cpp/libs/engine/core/network/broadcast_target_codec.h`(L1-184):`Encode` / `ForEach` 改为对消息类型的模板,同时服务 `BroadcastToPlayersRequest` 与 `contracts::kafka::BroadcastToPlayersEvent`,既有调用点不改;排序改 `std::stable_sort`(重复 session 保留首个);bitmap 跨度改按 uint64 算(与 Go 同口径,修 [0,UINT32_MAX] 回绕后越界写);新增公开 `kBitmapThreshold`、`TargetCount(msg)`、`PlayerListAligned(msg)`;保留 `gate_service.pb.h` 供既有调用方。
2. 新增 `cpp/nodes/gate/handler/gate_push_delivery.h/.cpp`:`gate_push::{Path, DeliverToSession, DeliverToTargets(RPC/Kafka 两个重载,共用 .cpp 内同一模板), DeliverToScene, DeliverToAll}`,头文件写明只在主 loop 调用。会话查找 → weak lock → `ClassifyPush` 三态(`LogFenceMismatch` / `LogFenceUnfencedOnce` 迁入,状态 thread_local、按入口各一份)。广播编码一次:`EncodedFrame` 用 `ProtobufCodec::fillEmptyBuffer` 编一份,逐连接 `send(data,len)`,不用 `send(Buffer*)`;单播仍走 `GetGateCodec().send`。Scene/All 只推 playerId 既非 0 也非 kInvalidGuid 的会话,scene_id=0 整条拒绝。7 项 thread_local 计数;丢弃类首条 + 每 1024 条打 `metric=gate_push_dropped path=<rpc|kafka> reason=<fence|invalid|scene0> total=N`,紧跟一行明细(定位 id + 全部计数,不进 metric 行);会话不存在 DEBUG + 计数;无连接采样 ERROR。
3. `cpp/nodes/gate/handler/rpc/gate_service_handler.cpp`(仅守护段):删 `LogFence*` 与变成无用的 include;新增 `DropKickFromUnboundScene`(踢线来源栅栏原逻辑搬出,在共享投递前判定);四个推送 RPC 各改为一次 `gate_push::*` 调用(`Path::kRpc`)。
4. `cpp/nodes/gate/handler/event/gate_event_handler.cpp`(仅守护段):include 段加新头;PushToPlayer(目标玩家 = `event.player_id()`)/ BroadcastToPlayers / BroadcastToScene / BroadcastToAll 改调 `gate_push::*`(`Path::kKafka`),原先为空的后两个由此补全;`LogKickUnfencedOnce` 注释的引用路径同步。
5. `cpp/nodes/gate/CMakeLists.txt`、`gate.vcxproj`、`gate.vcxproj.filters`:登记新 .h/.cpp。
6. 生成器 `gate_kafka_command_router.go`:case 加 `PayloadRequired`(`eventRequiresPayload`:有 repeated 或 message/group 字段),为真时不做信封映射;模板数据加方法 `AnyPayloadRequired()`。
7. 模板 `gate_kafka_command_router.cpp.tmpl`:有需要载荷的事件时渲染一份 `RejectEmptyPayload`(按 event_id 的 thread_local 计数,首条 + 每 1024 条 `metric=gate_kafka_empty_payload_rejected event_id= total=` + 明细行);信封分支对这类 case 渲染为 `RejectEmptyPayload(...); return;`,其余事件不变。
8. `cpp/tests/kafka_command_test/kafka_command_test.vcxproj`:登记新测试文件。

## 测试
- `gate_kafka_command_router_test.go` 新增:`TestGateKafkaRouterRejectsEmptyPayloadWhenEnvelopeCannotCarryEvent`、`TestGateKafkaRouterScalarOnlyEventKeepsEnvelopeMapping`、`TestEventRequiresPayloadOnlyForFieldsTheEnvelopeCannotCarry`。
- `routing_identity_test.cpp`:两种消息 5 个目标字段的字段号 static_assert;新增 `BroadcastTargetCodec.KafkaEventAndRpcRequestDecodeToTheSamePairs`(64 个密集目标强制 bitmap,两类型 wire 字节相同)、`BroadcastTargetCodec.DuplicateSessionKeepsTheFirstTargetInInputOrder`、`BroadcastTargetCodec.PlayerListLengthMismatchIsRejected`。
- 新增 `cpp/tests/kafka_command_test/broadcast_target_vectors_test.cpp`:`BroadcastTargetVectors.GateDecodesGoEncodedPairs`(另守覆盖面:bitmap / list / 重复 session 都要出现)、`BroadcastTargetVectors.SceneEncoderMatchesGoEncoder`(C++ Encode 逐字段等于 expect,即 GO-PUSH 报告的建议项)。GO-PUSH 已写出 7 条向量,已人工核对 bitmap/去重/阈值与 C++ 算法一致。
- gate handler 接线没有单测夹具,只能靠 robot 冒烟覆盖。

## 给 Codex 的命令(仓库根,按序)
1. `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`
2. `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`
   通过:重生成的 `cpp/nodes/gate/handler/event/gate_kafka_command_router.cpp` 的 diff 只有:多一份 `RejectEmptyPayload`;信封分支 PushToPlayer / BroadcastToPlayers / BroadcastToScene / BroadcastToAll 四个 case 变为拒绝,其余 case 不变。
3. 工作目录 `tools/proto_generator/protogen`:`gofmt -l internal/generator/cpp`(应无输出)→ `go test ./internal/generator/cpp/... -count=1`(建议再 `go test ./... -count=1`),全绿。
4. MSBuild,`/p:Configuration=Debug /p:Platform=x64 /m:1`,串行依次:`cpp/generated/proto/proto.vcxproj` → `cpp/libs/engine/core/core.vcxproj`(scene 的 client_message_sender.cpp 走模板 Encode)→ `cpp/nodes/gate/gate.vcxproj` → `cpp/nodes/scene/scene.vcxproj` → `cpp/tests/routing_identity_test/routing_identity_test.vcxproj` → `cpp/tests/kafka_command_test/kafka_command_test.vcxproj`。通过:0 error(gate 开 TreatWarningAsError,即 0 warning)。
5. 运行(cwd 仓库根):`build/cpp/tests/routing_identity_test.exe --gtest_filter=BroadcastTargetCodec.*:GateIdentityFence.*:NodeMessageHeaderFence.*`;`build/cpp/tests/kafka_command_test.exe --gtest_filter=BroadcastTargetVectors.*:CommandPartitionVectors.*:DbTaskPartitionContract.*`;再整体 `pwsh tools/scripts/run_cpp_tests.ps1 -Filter routing_identity -LogDir build/verification/cpp-tests` 与 `-Filter kafka_command`。全绿;失败保留 gtest 全文与 build log,向量不一致以机器结果回报、不改向量。
6. robot 冒烟(登录、进场景、聊天/公会/好友推送、AOI 广播、顶号踢线、跨 zone 重定向):gate 日志不应出现 `metric=gate_push_dropped`(fence/invalid)与 `metric=gate_kafka_empty_payload_rejected`;Go 侧未全部升级前出现一次 `Gate push without target player id ... path=kafka` 属预期。

## 未完成项 / 风险 / 需主会话拍板
1. 按说明只在指名广播校验 `has_message_content`;单播 / 场景 / 全服缺 message_content 时仍下发空帧(与旧行为同)。是否统一拒绝待拍板。
2. 计数未登记进节点 `/metrics`(要在 gate 装配处 main.cpp 登记 collector,不在本任务文件范围),目前只在采样明细行可见。
3. 场景 / 全服广播在主 loop 同步扇出、不分批(留给 P1 SpectateHub);当前没有生产者。
4. 行为变化:BroadcastToAll/Scene 不再推未绑定玩家的会话,scene_id=0 拒绝;踢线来源栅栏改在身份栅栏之前判定(投递结果不变,仅丢弃时打哪条日志不同)。
5. RedirectToGateEvent 的 string/bytes 字段信封同样装不下,按说明未纳入 PayloadRequired,可后续扩判据。
6. `client_session_handler.cpp:856` 引用的 `gate_service_handler.cpp:143` 行号已过时(非本任务文件,未改)。
7. 文档(battle-spectate-tiered-fanout.md §10.1 进展、PROGRESS.md、Java PARITY 备注)未改,由主会话补。
8. 未编译、未运行任何测试。

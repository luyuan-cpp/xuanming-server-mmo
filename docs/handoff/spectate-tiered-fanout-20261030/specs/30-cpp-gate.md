# 任务 CPP-GATE：gate 推送身份栅栏、两种广播 handler 补全、编码一次、空 payload 拒绝（C 批 C++ 侧）

先读 `00-common-rules.md`（同目录）。勘查参考：`..\wf\scout_gate.md`（落码方案与依据，行号已过时）。
Java gate 已有等价栅栏（每个推送目标带 player_id），本任务只改本仓。

## 允许改的文件
- `cpp/libs/engine/core/network/broadcast_target_codec.h`
- 新增 `cpp/nodes/gate/handler/gate_push_delivery.h`、`cpp/nodes/gate/handler/gate_push_delivery.cpp`，并登记进
  `cpp/nodes/gate/CMakeLists.txt`、`cpp/nodes/gate/gate.vcxproj`、`cpp/nodes/gate/gate.vcxproj.filters`
- `cpp/nodes/gate/handler/rpc/gate_service_handler.cpp`（只改 `///<<< BEGIN/END WRITING YOUR CODE` 守护段）
- `cpp/nodes/gate/handler/event/gate_event_handler.cpp`（只改守护段，含文件头 include 守护段）
- 生成器：`tools/proto_generator/protogen/internal/generator/cpp/gate_kafka_command_router.go`、
  `tools/proto_generator/protogen/internal/template/gate_kafka_command_router.cpp.tmpl`、同目录的
  `gate_kafka_command_router_test.go`（以实际文件名为准；生成产物 `gate_kafka_command_router.cpp` 不许手改）
- 测试：`cpp/tests/` 下现有的 `routing_identity_test.cpp`（BroadcastTargetCodec 组旁）；跨语言向量的 C++ 读取用例放进现有
  kafka 命令测试工程（勘查报告称 `kafka_command_test`，按 `kafka_partition_contract_test.cpp` 的 yaml-cpp 读法），新文件需登记进该测试工程的 vcxproj

## 要做的事
1. **编解码模板化**：`broadcast_target_codec.h` 的 Encode / ForEach 改成对消息类型的模板，同时服务 RPC 的
   `BroadcastToPlayersRequest` 与 Kafka 的 `contracts::kafka::BroadcastToPlayersEvent`（两者字段号、语义、顺序契约相同）；
   编码排序改 `std::stable_sort`，同一 session 重复时保留第一个；新增 `PlayerListAligned(msg)`：player_list 非空但长度
   不等于目标数 → false。现有调用点不改签名语义。
2. **共享投递模块 `gate_push`**（只在 gate 主 loop 线程调用，写进头文件契约）：
   ```cpp
   namespace gate_push {
   enum class Path : uint8_t { kRpc, kKafka, kCount };   // 低基数,计数与日志用
   void DeliverToSession(Path, uint32_t sessionId, uint64_t targetPlayerId, const MessageContent&);
   template <typename Msg> void DeliverToTargets(Path, const Msg&);   // 校验 has_message_content + PlayerListAligned
   void DeliverToScene(Path, uint64_t sceneId, const MessageContent&); // sceneId == 0 拒绝并计数
   void DeliverToAll(Path, const MessageContent&);                     // 只推已绑定玩家(playerId != 0)的会话
   }
   ```
   - 会话查找 → `weak_ptr` lock → `gate_session_fence::ClassifyPush(会话当前 playerId, targetPlayerId)` 三态
     （从 `gate_service_handler.cpp` 现有 RPC 实现整体搬过来，`LogFenceMismatch` / `LogFenceUnfencedOnce` 一起迁，
     `static` 状态改 `thread_local`）。BroadcastToAll / Scene 没有目标玩家，不过栅栏，但只推 playerId != 0 的会话
     （未登录连接不收游戏广播）。
   - **编码一次**：广播的同一条 MessageContent 只用 gate codec 编码成一份字节，再对每个会话 `conn->send(data, len)`；
     绝不对共享 Buffer 调 `send(Buffer*)`（muduo 会 `retrieveAll()` 清空它）。单播照常。
   - thread_local 计数：投递成功、unfenced、栅栏丢弃、会话不存在、无连接、无效消息；丢弃类第一条必打，此后每 1024 条打一行
     `metric=gate_push_dropped path=<rpc|kafka> reason=<fence|invalid|scene0> total=N`（口径参考 `client_request_lane.cpp`
     现有采样写法），不按 player_id 分维度；会话不存在改 DEBUG + 计数（替换 Kafka 版逐条 WARN）。
3. **两条入口都改调共享模块**：RPC 守护段（SendMessageToPlayer / BroadcastToPlayers / BroadcastToScene / BroadcastToAll）
   与 Kafka 守护段（PushToPlayer / BroadcastToPlayers / BroadcastToScene / BroadcastToAll）；Kafka 单播目标玩家取
   `event.player_id()`。两个原本为空的 Kafka handler（Scene / All）由此补全。不要保留两份实现（DRY）。
4. **空 payload 拒绝（生成器）**：生成器 case 结构加 `PayloadRequired`：事件含 message 型或 repeated 字段（信封装不下）
   即为 true（命中 PushToPlayer、BroadcastToPlayers、BroadcastToScene、BroadcastToAll）。模板对这类 case 渲染为
   「采样日志 + 计数 `metric=gate_kafka_empty_payload_rejected` + return」，不再构造空事件下发；其它事件的信封分支不变。
   生成器测试新增：需要 payload 的事件空 payload 被拒；只有标量字段的事件仍走信封映射。
5. **测试**：`routing_identity_test.cpp` 新增 Kafka 事件与 RPC 请求两种类型 ForEach 结果逐对一致（≥32 个密集目标强制
   bitmap）、重复 session 保留第一个、player_list 长度不齐被拒；跨语言向量用例 `BroadcastTargetVectors.GateDecodesGoEncodedPairs`
   读取 `go/shared/kafkautil/testdata/broadcast_target_vectors.json`（由 Go 侧任务生成，格式见下），对每条向量用 `expect`
   字段构造 `BroadcastToPlayersEvent`，ForEach 得到 (session, player) 对，断言等于 `targets` 按 session 升序稳定排序并去重
   （保留首个）后的结果。gate handler 接线没有单测夹具，交付说明写明只能靠 robot 冒烟覆盖。

## 跨语言向量文件格式（Go 任务写文件，你只读）
```json
{
  "vectors": [
    {
      "name": "dense_bitmap_64",
      "targets": [ { "session_id": 1, "player_id": 1001 } ],
      "expect": { "session_list": [1], "session_bitmap_base": 0, "session_bitmap_hex": "", "player_list": [1001] }
    }
  ]
}
```
`session_bitmap_hex` 是 bitmap 字节的小写十六进制；用 bitmap 时 `session_list` 为空，反之 `session_bitmap_hex` 为空。

6. 交付说明里给 Codex 的命令：`proto-gen-build`（生成器源码变了）→ `proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`
   → 生成器 `go test`（工作目录 `tools/proto_generator/protogen`）→ MSBuild `/m:1` 依次编 proto、gate、相关测试工程并运行。

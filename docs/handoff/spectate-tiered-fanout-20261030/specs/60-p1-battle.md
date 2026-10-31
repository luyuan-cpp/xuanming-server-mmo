# 任务 P1-BATTLE：battle 节点观战中继（L1）与全服广播（L2）出口

先读同目录 `00-common-rules.md`。权威契约：`docs/design/battle-spectate-tiered-fanout.md` §15（务必读 §15.2–§15.4），
proto 已由主会话改好：`battle_node.proto`（`AttachRelayGate` / `PromoteSpectateBroadcast` 及其消息）、
`battle_data.proto`（`eSpectateTier`）、`contracts/kafka/gate_event.proto`（`eSpectateFrameKind`、`SpectateFrameEvent`，
消息体直接就是该事件，不套 GateCommand 信封）、`player_battle.proto`（`SPECTATE_END_SOURCE_LOST`）。
topic 命名工具已加在 `cpp/libs/engine/core/node/system/node/node_command_topic.h`：`kSpectateRelayTopicBase`、
`kSpectateBroadcastTopicBase`、`kSpectateBroadcastTopicPartitions`、`SpectateBroadcastPartitionForBattle`。
B 批刚在 `battle_room_manager.{h,cpp}` 加了可见性第二道闸、观众席节点配额、观众帧编码一次、大厅推送填 player_id，
A 批加了 `RetireObserver` 等观众席回收；先读懂再改，不要破坏它们。

## 允许改的文件
- `cpp/nodes/battle/logic/battle_room_manager.h/.cpp`
- 可新增 `cpp/nodes/battle/logic/battle_spectate_relay.h/.cpp`（中继 / 广播的帧编码与投递，SRP），并登记进
  `cpp/nodes/battle/CMakeLists.txt`、`cpp/nodes/battle/battle.vcxproj`、`.vcxproj.filters`
- 测试：`cpp/nodes/battle/tests/battle_observer_contract_test.cpp` 或新增同目录测试文件（并登记进编它的测试工程，
  勘查称 `cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj`）
- **不改** `cpp/nodes/battle/handler/grpc/battle_node.{h,cpp}`（生成文件；新 RPC 的骨架要等 Codex 跑 proto-gen 才出现）。
  在交付说明里给出「生成后守护段清单」：`BattleNodeImpl::HandleAttachRelayGate` 守护段贴
  `BattleRoomManager::Instance().HandleAttachRelayGate(*request, *response);`，`HandlePromoteSpectateBroadcast` 同理。

## 要做的事
1. **房间状态**：`relayGates`（`std::map<uint32_t gateNodeId, std::string gateInstanceId>`，同 node_id 新实例覆盖）、
   `spectateBroadcast`（bool，只升不降）。
2. **`HandleAttachRelayGate` / `HandlePromoteSpectateBroadcast`**（`BattleRoomManager` 公开接口，头文件写完整契约）：
   校验顺序 `target_instance_id == 本节点实例 UUID`（否则 `kInvalidParameter`）→ 房间存在（否则 `kEntityIsNull`）→
   房间公开（否则 `kInvalidParameter`）。Attach：已是广播档回 `broadcast = true`、不挂；否则挂上并**只给这台 gate**补发
   一帧 STATE（当前快照 `BuildStateSnapshot` 编成 161 `SpectateStateS2C`，`observer_count = 0`）。Promote：置广播档，往广播
   topic 发一帧 STATE。两者幂等，回包带本实例 UUID 与 `deadline_ms`。
3. **帧编码**：`client_frame` = 客户端 `MessageContent{message_id, serialized_message}` 的序列化字节（不是直连的帧头编码；
   gate 会加自己的帧头）。同一帧只编一次，L0 直连（B 批的编码一次）与中继 / 广播共用同一份 S2C 消息。
   `round_index` 按 §15.4 取值；`deadline_ms` 取房间期限。
4. **投递**：
   - 每回合 `BroadcastTurnResult`：广播档 → 往 `spectate-bcast_g<N>` 发 1 条（分区 `SpectateBroadcastPartitionForBattle`，
     key = battle_id 十进制）；否则对每台中继 gate 往 `gate-spectate_g<N>` 发 1 条（分区 `CommandPartitionForNode(gateNodeId, 命令 topic 分区数)`，
     事件里填 `target_gate_id` / `target_instance_id`，key = battle_id）。topic 名用 `CommandTopicName(base, 命令 topic 代号)`，
     代号与分区数从现有配置读取口径（`node_command_route.h` 一带）取，与 battle 现有发 gate-cmd 的方式一致。
   - 结束 / 作废 / 销毁（与给 L0 观众发 SpectateEnd 同一处收口）：所有中继 gate 与广播（若已升档）各发一帧 END（编好的 166，
     reason 与 L0 同值）。停机且结果未落定的房间与 L0 同口径不发（见 §5.3），中继观众靠 gate 空闲超时收尾。
   - 复用 battle 现有的 Kafka 生产者；发送失败只采样日志 + 计数（`metric=battle_spectate_frame_send_failed` 一类），
     不重试、不阻塞回合推进（每帧带全量状态，下一帧自愈）。
   - **显式依赖**：把「往哪个 topic / 分区 / key 发什么字节」抽成可注入的出口（例如 `std::function` 或小接口），
     默认实现走真实 Kafka 生产者，测试注入捕获函数。
5. **测试**：Attach 的三种拒绝；Attach 幂等与同 node_id 新实例覆盖（只发一次 STATE 给新实例）；Promote 幂等且之后每回合
   只发广播 1 条、不再发中继；每回合中继条数 = 挂接 gate 数，事件目标字段正确、`client_frame` 可反序列化为 158 的
   MessageContent；结束时每个通道各一帧 END。现有观战用例保持通过（夹具已设 PUBLIC）。
6. 交付说明里给 Codex：proto-gen（统一跑）→ 按「生成后守护段清单」核对 / 粘贴 → MSBuild `/m:1` 依次 battle、测试工程 →
   运行相关 gtest filter。

## 2026-10-30 更新

- 生成骨架已存在：直接在 `cpp/nodes/battle/handler/grpc/battle_node.cpp` 的 `HandleAttachRelayGate`、`HandlePromoteSpectateBroadcast` 守护段里写一行委托（不再需要「生成后守护段清单」）；只改这两个守护段。
- 「全球经济系统代码审核」改过 `PumpSqlOutcomes`，先重读，别碰。
- 并入一条 B 批修正：「连接上限 0 = 硬上限」的规则现在 `battle_client_edge` 与 `BattleRoomManager` 各写了一份，改成直连面提供一个「有效连接上限」接口，管理器的节点观众席配额从它取（DRY）。

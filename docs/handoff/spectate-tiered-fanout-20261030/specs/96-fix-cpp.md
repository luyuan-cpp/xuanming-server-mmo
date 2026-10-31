# 任务 FIX-CPP：按评审修正 C++ gate 与 battle 的观战 / 推送问题

先读同目录 `00-common-rules.md`。评审原文（含位置、触发序列、修法）：`reports/review-cpp-relay.md`、`reports/review-c-batch.md`。动手前先核实每条仍成立（主会话已修掉 hub 的单水位问题：`lastTurnRound` / `cachedStateRound`），不成立的不改并说明。

## 允许改的文件
`cpp/nodes/gate/spectate/*`、`cpp/nodes/gate/handler/event/scene_entry_dispatch.cpp`（只加退订一行）、`cpp/nodes/gate/tests/gate_spectate_*`、新增 gate_push 测试文件（登记进 `routing_identity_test` 或现有 gate 测试工程）、`cpp/libs/engine/core/node/system/node/node_kafka_command_filter.h`、`node_kafka_command_handler.h`、`cpp/nodes/gate/main.cpp`、`cpp/nodes/battle/logic/battle_spectate_relay.*`、`battle_room_manager.{h,cpp}`（只动 `PublishSpectateEnd` 与注释）、`cpp/nodes/battle/tests/battle_observer_contract_test.cpp`、`cpp/nodes/gate/handler/gate_push_delivery.*`（只改注释或为测试加最小接缝）、`proto/contracts/kafka/gate_event.proto`（只改注释）、`cpp/libs/engine/core/network/client_message_sender.h`（只改注释）、`cpp/libs/services/gate/session/system/session_identity_fence.h`（只改注释）、`docs/design/scene-enter-chain-residuals.md`（只补一句）。

## 要修
来自 `review-cpp-relay.md`：
1. **严重-1 END 不抢占在途扇出**：`OnEndFrame` 置 kEnded、存 finalFrame；若当前还有没发完的目标就先返回；`FinishFanout` 发现已 ended 且 `fanoutFrame != finalFrame` 时再对全部订阅者开始 END 扇出。`DeliverToTarget` 的「必达」判据改成 `fanoutFrame == finalFrame`；`latestState` / `firstFrame` 的清理推迟到 END 开始扇出（否则尾部没收过状态的会话合成不出 161）。回归用例：批大小 2、订阅者 5，`TurnFrame(9)` 紧跟 `EndFrame(9)`，跑完续发后每个会话都恰好先 158 后 166。
2. **严重-2 升档后 END 只发广播**：`PublishSpectateEnd` 已升档时只发广播 topic（与最后一回合 TURN 同分区保序，消除跨 topic 赛跑）；同步头文件注释与用例 `PromoteIsIdempotentAndLaterRoundsOnlyBroadcast`。
3. **一般-1 合并时不饿死表尾**：TURN 合并在途扇出时，把新名单旋转到上一轮停下的位置（`std::rotate`，下标对 size 取模），过载时大家轮流丢帧；补用例。
4. **一般-3 回选角摘订阅**：`OnClientLeaveGame` 调 `gate_spectate::OnSessionClosed(sessionId)`（确认这个调用对「TCP 不断」的情形语义正确）；补用例或说明替代验证。
5. **一般-4 继任 gate 不逐帧刷 WARN**：`KafkaCommandTargetFilter` 加一个采样选项（默认关，现有调用方行为不变），只在两条观战 topic 的 options 上打开；实例不符的丢弃第一条必打，此后每 1024 条一行。
6. **一般-5 注释**：`gate_event.proto` 的观战帧注释与 `gate_spectate_hub.h` 文件头写清两条水位：158 只与 158 比较，161 不推进水位，TURN(r) 的 state 回合是 r + 1。

来自 `review-c-batch.md`：
7. **一般-4 协议注释**：按原文列的位置改注释（`gate_event.proto` 信封路径与空 payload 的旧说法、BroadcastToAll 只推已绑定玩家；`client_message_sender.h:33`；`session_identity_fence.h` 的「未绑定却收到指名推送不可能发生」改成：Bind 之前的登录失败提示以 player_id = 0 显式走兼容位，见 login `gate_command.go`；`scene-enter-chain-residuals.md` §2.5 补一句 2026-10-30 起空 payload 一律拒绝）。
8. **一般-5 gate_push 测试**：照 `battle_observer_contract_test.cpp` 的回环夹具写 gate_push 用例，向 `tlsSessionManager` 放会话，覆盖：栅栏三态（放行、丢弃、0 值兼容）、未绑定会话跳过场景 / 全服广播、scene 0 拒绝、player_list 不齐拒绝、缺 message_content 拒绝。接线到两条入口的那一层若测不到，说明替代验证。

不要改 Go（login 的 Bind 前提示由另一个任务改）。不要动 battle 的 outcome / SQL outcome 段。
完整交付说明写到 `docs/handoff/spectate-tiered-fanout-20261030/reports/fix-cpp.md`，最终回复三五行摘要。

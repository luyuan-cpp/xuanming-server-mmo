# 任务 P1-GATE：gate 观战订阅中心 SpectateHub（L1 中继 / L2 广播的边缘扇出）

先读同目录 `00-common-rules.md`。权威契约：`docs/design/battle-spectate-tiered-fanout.md` §15（务必读 §15.2、§15.3、§15.5），
以及 §7。proto 已由主会话改好：`proto/gate/gate_client_spectate.proto`（客户端可见服务 `GateClientSpectate`：
`SubscribeSpectate` / `UnsubscribeSpectate`，`cc_generic_services`、NODE_GATE）、`proto/gate/gate_spectate_ticket.proto`
（`SpectateTicketPayload` / `SpectateTicket`，签名 = 小写 hex 的 HMAC-SHA256）、`contracts/kafka/gate_event.proto` 的
`SpectateFrameEvent`（观战帧 topic 的消息体就是它，内含 `target_gate_id` / `target_instance_id`，不套 GateCommand）、
`common/base/config.proto` 的 `BaseDeployConfig.spectate_token_secret`（字段 24）。topic 命名工具在
`cpp/libs/engine/core/node/system/node/node_command_topic.h`（`kSpectateRelayTopicBase` / `kSpectateBroadcastTopicBase` /
`kSpectateBroadcastTopicPartitions`）。C 批刚加了 `cpp/nodes/gate/handler/gate_push_delivery.{h,cpp}`（共享投递、编码一次、
栅栏），本任务复用它的「编码一次」能力，不另写一份。

## 允许改的文件
- 新增 `cpp/nodes/gate/spectate/gate_spectate_hub.h/.cpp`（订阅表与扇出，核心逻辑可单测：发送函数与时钟显式注入）、
  `cpp/nodes/gate/spectate/gate_spectate_ticket.h/.cpp`（凭证验签，纯函数）、需要的话 `gate_spectate_client.h/.cpp`
  （客户端两条 RPC 的本地处理与回执）；登记进 `cpp/nodes/gate/CMakeLists.txt`、`gate.vcxproj`、`gate.vcxproj.filters`
- `cpp/nodes/gate/handler/rpc/client_session_handler.cpp`：客户端消息分发里，在 GM 闸之后、battle 拒绝与协议分派之前加一处：
  `messageInfo.targetNodeType == eNodeType::GateNodeService` → 交给本地处理并 return（不转发、不进 lane）。
  会话关闭处（与 `gate_lane::OnSessionClosing` 同一处）通知 hub 摘除该会话。
- `cpp/nodes/gate/main.cpp`：装配（读密钥、注册两个 Kafka 消费者、起空闲巡检定时器、prod 空密钥拒启）
- `cpp/libs/engine/core/node/system/node/node_kafka_command_handler.h`：只为「不注册旧的一节点一 topic 兼容路径」与
  「assign 全部分区」各加一个最小选项（不改现有调用方的行为）
- `cpp/nodes/gate/handler/event/gate_event_handler.cpp`：会话被顶号 / 踢出时若走这里的 erase 路径，同样通知 hub（只改守护段）
- 测试：新增 `gate_spectate_hub_test.cpp` / `gate_spectate_ticket_test.cpp`，放进现有能编 gate 代码的测试工程（先找：
  `cpp/nodes/gate/tests/` 下的工程或 `cpp/tests/` 下引用 gate 源的工程），并登记进其 vcxproj

## 要做的事
1. **凭证验签**（纯函数）：输入 secret、凭证字节、会话 playerId、请求 battle_id、gate 当前墙钟毫秒；输出「通过 +
   payload」或拒绝原因。签名比较用 `token_security::ConstantTimeEquals`，HMAC 用 `token_security::HmacSha256Hex`。
   secret 为空一律拒绝（fail-closed）。
2. **SpectateHub**（只在主 loop 线程调用，写进头文件契约；按 §15.5 实现）：
   - `Subscribe(sessionId, payload)`：一个会话同时只订一场，替换旧订阅；条目实例与凭证不符时以凭证为准并清缓存；
     有缓存立即给该会话发首帧 161（缓存是 161 原样发；是 158 则解析出 `state` 合成 `SpectateStateS2C{state, observer_count=0}`
     后编码成 161 的 MessageContent）。
   - `Unsubscribe(sessionId, battleId)`、`OnSessionClosed(sessionId)`。
   - `OnFrame(const SpectateFrameEvent&)`：实例不符丢弃；TURN 只收 `round_index > last_round`；STATE 收 `>=` 且只更新缓存；
     END 只收一次，扇出后标 ended，保留 10 分钟墓碑（墓碑期内新的 Subscribe 直接回一帧 166 并结束）；无订阅者的场次也建条目
     缓存，无人订阅且 30 s 无帧即删。
   - 扇出：同一帧只用 gate codec 编码一次（`client_frame` 是 MessageContent 序列化字节，反序列化一次后按 gate 现有客户端
     编码方式编成帧，复用 `gate_push` 的编码一次接口），每次最多 512 个会话，余下 `queueInLoop` 续发（闭包只捕获 weak /
     值，不捕获裸 this）；续发途中同场来了新帧，没发完的会话改发新帧（合并）；发送前检查
     `conn->outputBuffer()->readableBytes()`，超过 256 KB 跳过这一帧并计数。
   - `SweepIdle(nowSteady, nowWallMs)`：有订阅者的条目超过 30 s 无帧，或超过 `deadline_ms + 60 s`，给订阅者合成 166
     （`SPECTATE_END_SOURCE_LOST`，outcome ONGOING）并删除。定时器回调只绑 weak（§11.7）。
   - 每 30 s 一行 `metric=gate_spectate subscribers=… entries=… frames=… soft_watermark_skips=… coalesced=…
     ticket_rejects=… source_lost=…`；不带 battle_id / player_id。
3. **客户端两条 RPC**：解析 `ClientRequest.body` 为对应请求；Subscribe 验签失败回带请求 id 的错误信封
   （`gate_lane::SendEnvelope(..., kMatchSpectateTicketInvalid)`，tip 已在 Tip.xlsx，导表后生成）；成功回带请求 id 的
   `MessageContent{id, message_id, serialized_message=SubscribeSpectateResponse}`，然后再发首帧（若有缓存）。
   Unsubscribe 幂等成功。两条都要过现有的体积 / 限流闸（拦截点放在那些闸之后）。
4. **Kafka 消费**（`main.cpp`）：
   - L1：`RegisterKafkaCommandHandler<contracts::kafka::SpectateFrameEvent>`，commandTopicBase = `kSpectateRelayTopicBase`，
     分区按本节点 node_id（与 gate-cmd 同契约），目标过滤字段 `target_gate_id` / `target_instance_id`，不注册兼容旧 topic。
   - L2：同一类型、topic `kSpectateBroadcastTopicBase`，assign 全部 `kSpectateBroadcastTopicPartitions` 个分区，startAtEnd，
     不提交 offset；两个目标字段为空即放行（现有过滤规则）。
   - 两者回调都在主 loop 上调 `SpectateHub::OnFrame`。
5. **密钥**：从 `BaseDeployConfig.spectate_token_secret` 读；用 `token_security::ClassifyTokenSecret` 与 gate 现有的运行模式口径：
   prod 空 / 过短拒绝启动；dev/test 空值放行但 WARN 一次，并且所有 Subscribe 一律拒绝（fail-closed）。
6. **测试**（显式注入发送函数与时钟，不用真实墙钟、不依赖执行顺序）：凭证验签正反例（篡改、过期、换 player、换 battle、
   空 secret）；订阅替换；首帧从 158 缓存合成 161；TURN 单调去重、STATE 只更新缓存、END 只扇一次且墓碑期内新订阅直接收 166；
   实例不符丢帧；空闲 / 过期合成 SOURCE_LOST；扇出分批与合并（注入批大小）；软水位跳帧（注入「缓冲已满」的发送函数）。
7. 交付说明里给 Codex：proto-gen（统一跑）→ MSBuild `/m:1` 依次 proto、gate、测试工程 → 运行测试；并写明 robot 冒烟要覆盖的场景
   （LOBBY 订阅 → 收 161 → 每回合 158 → 166），这一步要 robot 先支持 SubscribeSpectate（不在本任务）。

## 2026-10-30 更新

- 生成器已产出 `cpp/nodes/gate/handler/rpc/gate_client_spectate_handler.{h,cpp}`（muduo RPC 形态，节点间入口）与 `cpp/generated/rpc/service_metadata/gate_client_spectate_service_metadata.h`。客户端的这两条消息在 `client_session_handler.cpp` 按 `targetNodeType == GateNodeService` 本地处理，走不到这个 RPC 入口；它的守护段里写一句注释说明原因并不做任何事（节点间没有调用方），不要把逻辑放这里。确认 `gate.vcxproj` / `CMakeLists.txt` 是否已包含它（生成器可能已加）。
- 并入三条 C 批修正：
  1. `gate_push`：单播、场景、全服广播缺 `message_content` 一律拒绝并计数（现在只有指名广播检查），不下发空帧。
  2. 生成器 `gate_kafka_command_router.go` 的 `PayloadRequired` 判据改为「事件里有任何 GateCommand 信封装不下的字段」（含 string / bytes，例如 `RedirectToGateEvent` 的 ip / token），不只看 message / repeated；先读清信封到事件的映射规则再改，补测试。生成产物不手改，Codex 跑 proto-gen-build + proto-gen-run 重新生成。
  3. `client_session_handler.cpp` 约 856 行注释引用的 `gate_service_handler.cpp:143` 行号已过时，改成函数名引用。

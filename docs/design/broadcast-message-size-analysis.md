# BroadcastToPlayers 消息体积分析

**日期**: 2026-04-19  
**背景**: uint64→uint32 优化后，单条广播仍观测到 2292 字节

2026-10-15 按 `6281e4d27` 核对传输：C++ Scene → Gate 使用 muduo TCP RPC。
本文体积对比是原测量记录；当前单玩家目标还带身份栅栏，不能直接把旧体积当作当前整包大小。

## 观测数据

```
method: BroadcastToPlayers, msg_id: 32, size: 2292, body(15925)
session_list: ~750 个 uint32 session ID (范围 262145–263149)
message_content: msg_id=70, serialized_message ~20 bytes
```

## 体积构成

| 部分 | 大小 | 占比 |
|------|------|------|
| `session_list` (~750 个 uint32 varint, 每个 ~3 字节) | ~2250 bytes | 98% |
| `message_content` (msg_id=70, payload ~20 bytes) | ~25 bytes | 1% |
| protobuf 开销 (tag, length prefix) | ~17 bytes | 1% |

## 根因

同一 gate 上有 ~750 个玩家在同一场景，广播时逐个列出 `session_id`。消息负载本身只有 25 字节，session 列表占据 98% 的体积。

## 已实现优化

### Phase 1: session_list uint64→uint32

| 优化 | 变更 | 效果 |
|------|------|------|
| session_list uint64→uint32 | proto field type 变更 + 两端适配 | ~50% 减小 (varint ~5 bytes → ~3 bytes) |

### Phase 2: 三级广播体系 (bitmap + scene + all)

#### 1. Bitmap 编码 (自动, count >= 32)
- `session_bitmap_base` (uint32) + `session_bitmap` (bytes)。Bit N 置位 = session (base+N) 在列表中。
- 当前 C++ 发送侧由 `broadcast_targets::Encode` 编码，底层 `EncodeBroadcastSessionList()` 在
  `count >= 32 && bitmapBytes + 6 < count * 3` 时走 bitmap。身份列表按同一目标顺序编码，供 Gate 校验。
- 750 players: 2292 → ~155 bytes (**-93%**)
- 密度盈亏平衡点 ~4.2%，AOI 广播总是远高于此。

#### 2. BroadcastToScene (场景级广播)
- 只发 `scene_id + message_content`，Gate 遍历已连接 session 按 `info.sceneId` 过滤分发。
- 用于场景全员广播（非 AOI 子集），~30 bytes，与玩家数无关。

#### 3. BroadcastToAll（目标 Gate 上全部客户端会话）
- 只发 `message_content`，Gate 向所有已连接 session 分发。
- 用于系统公告、维护通知等，~25 bytes。
- C++ 调用只覆盖当前节点 Gate 注册表中拥有 `RpcSession` 的 Gate；跨节点或跨 zone 的全服覆盖需要调用方另行编排。

## API 总览

### C++（Scene Node → Gate，muduo TCP RPC）

| 函数 | 传输 | 场景 |
|------|------|------|
| `BroadcastToPlayerClients(messageId, message, playerEntities)` | 每 Gate 一次 muduo TCP RPC | 指定玩家客户端，适用于 AOI 子集（自动 bitmap） |
| `BroadcastToSceneClients(messageId, message, sceneId, scenePlayerEntities)` | 每 Gate 一次 muduo TCP RPC | 玩家集合只用于发现 Gate，实际收件人由 Gate 按 `sceneId` 筛选 |
| `BroadcastToAllClientsOnKnownGates(messageId, message)` | 向当前线程注册表中拥有 `RpcSession` 的 Gate 发送 muduo TCP RPC | 这些 Gate 上的全部客户端会话，不保证全服覆盖 |

发送接口位于 `client_message_sender.h`，只在所属节点的 EventLoop 线程调用。单发按玩家 ID / 实体调用
`SendMessageToClientViaGate`；已经持有 Gate RPC 会话与客户端会话号的路径调用
`SendMessageToClientSessionViaGate`。这些接口均不等待客户端确认；此次改名不改变身份栅栏、广播编码或发送行为。

### Go (Service → Gate, Kafka)

| 函数 / Builder | 传输 | 场景 |
|-----------|-----------|----------|
| `kafkautil.BroadcastToPlayers()` + `BuildBroadcastCommand()` | Kafka per gate | 玩家子集 (自动 bitmap) |
| `kafkautil.BroadcastToScene()` + `BuildBroadcastToSceneCommand()` | Kafka per gate | 场景全员 |
| `kafkautil.BroadcastToAll()` + `BuildBroadcastToAllCommand()` | Kafka to all gates | 全服 |

## 关键文件

**Proto:**
- `proto/gate/gate_service.proto` — bitmap fields (3,4) + `BroadcastToSceneRequest` + `BroadcastToAllRequest` + RPCs
- `proto/contracts/kafka/gate_event.proto` — bitmap fields (3,4) + `BroadcastToSceneEvent` + `BroadcastToAllEvent`

**C++ 发送侧 (scene node):**
- `cpp/libs/engine/core/network/client_message_sender.cpp/h` — 单发与三种广播的组包及 `RpcSession::SendRequest`
- `cpp/libs/engine/core/network/broadcast_target_codec.h` — `broadcast_targets::Encode`，与 Gate 共用目标顺序和身份栅栏编码

**C++ Gate 接收侧:**
- `cpp/nodes/gate/handler/gate_push_delivery.{h,cpp}` — 共享投递模块 `gate_push`（2026-10-30 起）：身份栅栏（`ClassifyPush`）、广播只编码一次、场景 / 全服广播只推已绑定玩家的会话；RPC 与 Kafka 两条入口都走这里
- `cpp/nodes/gate/handler/rpc/gate_service_handler.cpp` — muduo TCP RPC 入口：`BroadcastToPlayers`、`BroadcastToScene`、`BroadcastToAll`
- `cpp/nodes/gate/handler/event/gate_event_handler.cpp` — Kafka 入口：同上 + `PushToPlayerEventHandler`（2026-10-30 之前 Kafka 版场景 / 全服广播 handler 是空实现，消息被静默吞掉）

**Go 共享工具:**
- `go/shared/kafkautil/gate_push.go` — `GateCommandBuilder` 接口, `BroadcastToScene()`, `BroadcastToAll()`, `EncodeBroadcastTargets()`（session 与 player 成对编码，与 C++ `broadcast_target_codec.h` 用 `testdata/broadcast_target_vectors.json` 对拍）

**Go 服务 Builder:**
- `go/guild/internal/kafka/gate_command_builder.go`
- `go/friend/internal/kafka/gate_command_builder.go`

## Session ID 布局

`SessionIdGenerator = TransientNodeCompositeIdGenerator<uint32_t, kNodeBits=17>`:
- 高 15 位 = gate node_id，低 17 位 = sequence（序列按 17 位回绕；分配器另查活跃会话避免复用在用 ID）
- 同一 gate 上所有 session 共享高 15 位 → bitmap span 受实际 seq 分配限制
- bitmap 最坏情况: 131071/8 = ~16KB, 但 `EncodeBroadcastSessionList` 会自动退回 list

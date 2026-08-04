# SceneManager跨节点切场景释放玩家方案选型

## 方案对比

### ① SceneManager 主动 RPC 通知旧 node（旧实现，生产默认禁用）
- SceneManager 检查 currentLoc.NodeId != nodeId 时，直接调用旧 node 的 ReleasePlayer（或 LeaveScene）RPC。
- 旧 node 的 RPC 只保证 `SavePlayerToRedis` 已入异步队列，**不保证 Redis 已落盘**。
- 新 node 可能先加载旧快照，再用从旧快照派生的新写覆盖旧 node 的较新状态；
  Kafka offset 不是业务因果版本，不能修复该窗口。

### ② 旧 node 监听 player_locator 变更
- 每个 scene node 订阅 player_locator 变更，发现玩家迁出后自行 Save + Destroy。
- 存在 watch 延迟，Save 可能晚于新 node 加载，导致数据覆盖。
- 扇出大，调试难，死节点不会收到通知。

## 当前安全策略

- 首次落点和同节点切场景正常执行。
- 已有位置的跨节点/跨 zone 交接在生产默认 **fail-closed**：回滚目标场景预留，
  不释放旧实体、不更新 location、不发送成功路由。
- 开发环境只能通过显式的不安全开关恢复旧行为，用于协议联调；不得作为生产配置。
- 开关名为 `AllowUnsafeCrossNodeHandoff`，默认与示例配置均为 `false`。
- Gate 路由只在 Kafka `RequireOne` broker ACK 后提交成功；失败时用 Lua
  exact-value CAS 恢复 location，避免覆盖并发推进的新位置。该 ACK 不等于存盘屏障。
- 完整恢复跨节点能力的发布门禁是 per-player handoff epoch：旧 node 成功落盘并
  发布 epoch，目标 node 在加载前验证同一 epoch，超时则保持旧 ownership 并失败。

## 适用范围
- 节点内切场景无需 Save/Destroy，直接复用 entity。
- 跨节点能力在 handoff epoch 协议完成前不可用于生产。

## 相关文件
- proto/scene_manager/scene_node_service.proto
- cpp/nodes/scene/handler/grpc/scene_node_service.{h,cpp}
- go/scene_manager/internal/logic/scene_node_client.go
- go/scene_manager/internal/logic/enterscenelogic.go

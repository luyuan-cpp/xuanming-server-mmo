# SceneManager跨节点切场景释放玩家方案选型

> **⚠️ 状态说明(2026-09-20):下文「当前安全策略」与「适用范围」里的「跨节点 / 跨 zone 交接生产默认 fail-closed、handoff epoch 协议完成前不可用于生产」已被取代,正文保留为历史。**
> - 文末所说的发布门禁——per-player handoff epoch——已于 2026-09-18 落码(代码已进 main,**尚未编译、未跑测试**),这道无条件拒绝的闸换成了**换手门**:`scene_manager.EnterScene` 对已有位置的跨节点 / 跨 zone 请求,要求源 scene 已为当前 `owner_epoch` 写出 `player:{id}:handoff` 落盘标记;有则放行并铸造新 epoch,没有则回可重试的 18(`ErrHandoffPending`),不改任何状态。旧的 14(`ErrUnsafeCrossNodeHandoff`)是历史码,不再发出。
> - 生产配置(`AllowUnsafeCrossNodeHandoff=false`)下客户端发起的跨节点换图因此**可达**:首个 EnterScene 被 18 暂拒 → 源 scene 冻结 → 存盘落地 → 写标记 → 重发同一个 EnterScene → 放行。与下文设想不同的一点:校验标记与 epoch 的是 scene_manager(唯一闸口),不是目标 node;旧持有者迟到的存盘由 C++ 存盘 Lua CAS 与 go/db 的 applied_epoch 守卫拒绝。
> - `AllowUnsafeCrossNodeHandoff` 的含义也变了:只是跳过「源已落盘」这道标记门的 dev 旁路,且旁路放行时**不铸造** epoch;生产仍必须为 `false`。
> - 现状以 [cross-zone-scene-travel.md](./cross-zone-scene-travel.md) 为准:CZ-4(两道换手门)、§10.2 R1 / R4 / R5 / R6(落码修正)、§11.2(生产配置下的跨节点换图与预期日志序列)。「方案对比」一节对 ①② 的否定理由仍然成立。

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

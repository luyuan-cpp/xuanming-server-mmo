# Centre 退役第一批次 - 代码变更点 (2026-03-15)

## 第一批次范围
- 在需要时保持线路兼容性，但从 SceneManager 入口点中移除 Centre 语义。
- 添加 fencing/ownership 元数据，确保控制平面无单点故障安全性。
- 不要编辑 generated/ 下的生成文件；通过现有 pbgen 流水线重新生成。

## 1) Proto 契约变更（唯一数据源）
- 文件：`proto/scene_manager/scene_manager_service.proto`
- 变更 RPC 名称（如需要，在 server 中保留瘦包装器）：
  - `EnterSceneByCentre` -> `RoutePlayerToScene`
  - `LeaveSceneByCentre` -> `ReleasePlayerFromScene`
- 消息重命名和字段迁移：
  - `EnterSceneByCentreRequest` -> `RoutePlayerToSceneRequest`
  - `LeaveSceneByCentreRequest` -> `ReleasePlayerFromSceneRequest`
  - 废弃 `centre_node_id`（字段编号 3），用 `request_source_node_id` 替代其语义（如需线路兼容则使用相同字段编号）。
- 在路由请求中添加 HA/fencing 字段：
  - `uint64 orchestrator_epoch`（新增）
  - `string orchestrator_instance_id`（新增）
  - 保留 `gate_id`、`gate_instance_id`、`request_id`。

## 2) Kafka 命令契约扩展
- 文件：`proto/contracts/kafka/gate_command.proto`
- 添加可选的控制平面 fencing 元数据：
  - `optional uint64 orchestrator_epoch`（新字段编号）
  - `optional string orchestrator_instance_id`（新字段编号）
- 保留现有 `target_instance_id` 作为 Gate 侧过期防护。

## 3) Go SceneManager 逻辑重构（瘦包装器模式）
- 文件：
  - `go/scene_manager/internal/server/scenemanagerserver.go`
  - `go/scene_manager/internal/logic/enterscenebycentrelogic.go`
  - `go/scene_manager/internal/logic/leavescenebycentrelogic.go`
- 操作：
  - 保持生成的方法名可调用，但委托给动词命名的内部函数：
    - `ProcessRoutePlayerToScene(...)`
    - `ProcessReleasePlayerFromScene(...)`
  - 在写路径之前验证 ownership/fencing：
    - 拒绝过期的 `orchestrator_epoch`
    - 拒绝同一 epoch 下不匹配的 orchestrator 实例
  - 通过 `request_id` 和当前位置检查保持幂等性。

## 4) Gate 消费者验证加固
- 文件：
  - `cpp/nodes/gate/main.cpp`
  - `cpp/nodes/gate/handler/event/gate_kafka_command_router.cpp`
- 当前状态：
  - 已验证 `target_gate_id`
  - 已验证 `target_instance_id`
- 第一批次新增：
  - 结构化日志字段包含 `player_id`、`session_id`、`event_id`、topic。
  - 可选的过期/不匹配丢弃指标计数器。

## 5) 重新生成 / 构建顺序
1. 仅编辑 `proto/` 下的源 proto 文件。
2. 运行 pbgen 生成（`pbgen: run` 任务或项目脚本）。
3. 对修改过的 Go 文件运行 `gofmt`。
4. 构建 Go scene_manager 服务。
5. 构建 C++ gate 节点目标。

## 6) 已知影响范围关注点
- Go 生成的 grpc 存根和调用点中所有 `*ByCentre` 引用。
- 现有 C++ 场景流程在多个 proto 域中仍包含 `centre_node_id`；第一批次不要批量重命名。
- 保持迁移渐进式推进：先迁移 SceneManager 契约，然后在第二批次中迁移 scene/player 生命周期契约。

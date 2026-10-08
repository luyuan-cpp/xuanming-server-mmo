# Centre 下线 + 高可用检查清单 (2026-03-15)

## 目标
- 将 Centre 从热路径中移除，同时避免任何单点故障。

## 控制面拆分
- 服务发现与存活检测：etcd 集群（3/5 节点）。
- 玩家位置真相源：player_locator（多实例 + 持久化存储）。
- 命令扇出：Kafka（RF>=3，min.insync.replicas 已配置）。
- 场景切换编排：scene_manager 集群（建议 >=3 实例）。

## 所有权与隔离
- 按 player_id（或 zone_id）通过一致性哈希划分分区所有权。
- 每个分区通过 lease/epoch 确保恰好一个拥有者。
- 每条控制命令携带 target_instance_id 和 epoch。
- 消费者拒绝过期的 epoch 或不匹配的 target_instance_id。

## 不变量（必须保持）
- 任一时刻每个玩家只有一个写入者。
- 场景交接必须串行化：旧场景释放+确认后，新场景才能加载。
- RoutePlayer/KickPlayer 必须是幂等的。

## 故障处理
- 拥有者丢失：通过 lease 过期进行分区重新选举。
- 重复/过期消息：通过 epoch + instance id 进行隔离。
- 路由过程中玩家已移动：
  - 重要消息：重新查询 locator 并重试。
  - 尽力投递消息：允许丢弃。

## 迁移计划
1. Centre 仅保留为可观测性/回退角色，不再作为路由权威。
2. 按区服/玩家批次灰度切换至 scene_manager+kafka 路径。
3. 运行双路径验证（旧路由结果 vs 新路由结果）。
4. 稳定窗口期后，将 Centre 从所有热路径流量中移除。
5. 保留紧急回滚开关，持续一个发布周期。

## 全面切换前必须具备的指标
- 路由命令延迟 p95/p99。
- 场景切换成功率和超时率。
- 过期命令拒绝计数（epoch/instance 不匹配）。
- 重复命令率和幂等命中率。
- 故障转移期间的玩家重连/踢线率。

## 实现状态 (2026-03-15)

### 已完成
- **Proto 变更**：SessionDetails 扩展了 gate_node_id+gate_instance_id；SceneManager RPC 重命名（EnterSceneByCentre→EnterScene，LeaveSceneByCentre→LeaveScene）；gate_event.proto + gate_command.proto 扩展（BindSession=3，LeaseExpired=4，event_id 字段）；gate_event.proto 新增消息（PlayerDisconnectedEvent、PlayerLeaseExpiredEvent、BindSessionEvent）；player_event.proto 已创建
- **Go Login**：sessionmanager（Redis 支撑，基于账号的重连）、gatenotifier（Kafka）、entergamelogic/leavegamelogic/disconnectlogic 重写以移除 Centre 依赖、ServiceContext.KickOldSession 通过 Kafka 实现、login.go 中 connectToCentreNodes 已移除、session_cleaner 中 CentreClient 接口已移除
- **Go SceneManager**：逻辑文件重命名 + 内容更新（EnterScene/LeaveScene），server + interface 已更新
- **C++ Gate**：CentreNodeService 已从 CanConnectNodeTypeList 中移除、Centre 断连 RPC 已移除、SessionDetails 现在携带 gate_node_id+gate_instance_id、CentreNodeService 已从 network_utils 单例中移除、gate_service_handler.cpp 中 centre_service_service_metadata.h 的 include 已移除
- **C++ Gate Kafka 事件**：gate_kafka_command_router 处理全部 5 种命令类型（RoutePlayer、KickPlayer、Broadcast、BindSession、LeaseExpired）；gate_event_handler 拥有全部 4 种事件的处理器，支持 session/version 检查
- **C++ 生成的 Proto**：session.pb.h/cc、gate_command.pb.h/cc、gate_event.pb.h/cc、scene_manager_service.pb.h/cc 和 .grpc.pb.h/cc 通过 protoc 重新生成；service_metadata.h/cpp 已更新
- **Go 生成的 Proto**：login/contracts/kafka/gate_command.pb.go、login/proto/common/session.pb.go 以正确的源路径重新生成

### 需要完整 pbgen 运行
- `cpp/generated/grpc_client/scene_manager/` 中 Send 函数仍有 ByCentre 命名
- `tools/scene_manager/`（冗余副本）仍有旧命名
- `go/generated/` 下所有 Go 生成的 proto 需要重新生成

### 已完成（第二阶段 — player_locator）
- **player_locator Go 服务**：完整实现，包含 7 个 RPC（SetLocation、GetLocation、MarkOffline、SetSession、GetSession、SetDisconnecting、Reconnect）
- **Proto 已重新生成**：player_locator_grpc.pb.go 现在包含全部 7 个会话管理 RPC
- **Kafka 契约已生成**：gate_command.pb.go、gate_event.pb.go、player_event.pb.go 用于 player_locator 模块
- **Redis 会话存储**：`player:session:{player_id}`（protobuf 二进制，断线状态有 TTL）
- **Lease 监控**：后台 goroutine 轮询 `player:leases` ZSET，原子 Lua ZPOPMIN，向 gate-{gate_id} Kafka topic 发布 GateCommand{LeaseExpired}
- **节点注册**：etcd lease + NodeAllocator 模式（与 Login 相同），NodeType=24（PlayerLocatorNodeService）
- **配置**：go-zero RPC 服务器、Redis、Kafka brokers、etcd 注册中心、lease 设置（默认 30s TTL、1s 轮询、批量 100）

### 已验证可下线 (2026-03-15)
- **CentreEnterGsSucceed**：所有回调桩都为空（发后即忘的空操作）— 不阻塞
- **session 中的 centre_node_id**：已从 session 快照初始化中移除 — 未填充
- **Login → player_locator**：sessionmanager 已委托给 player_locator gRPC — 已完成
- **Scene CanConnectNodeTypeList**：已移除 CentreNodeService（白名单，非必需）— 已完成
- **队伍跟随者崩溃修复**：将不安全的 `.at(CentreNodeService)` 替换为 LOG_WARN + skip（这是唯一的崩溃路径）
- **TryFallbackToCentreForImportantRoute**：代码库中不存在
- **RegisterAllSceneToCentre**：代码库中不存在
- **SendToCentrePlayerById**：死代码，生产环境无调用者

### 剩余死代码（无害，后续清理）
- Gate 和 Scene 的 rpc_replies 中的 centre_service_response_handler.cpp/h（空桩，从未触发）
- register_response_handler.cpp 中的 InitCentreReply() 调用（注册空操作回调）
- enterscenebycentrelogic.go（pbgen 后无引用）
- KickSessionByCentre 处理器（记录 "decommissioned" 日志后返回）
- 生成的 ByCentre 命名（需要 pbgen 运行）

### 尚未开始
- 队伍跟随者通过 SceneManager 切换场景（当前以 LOG_WARN 跳过）
- 跨节点消息通过 Kafka 实现（替换 Centre 的 RoutePlayerStringMsg/RouteNodeStringMsg）
- Session lease 过期处理的测试覆盖
- 双路径验证和灰度发布

## 规则
- 保持位置透明性：逻辑层不得根据跨服/本地进行分支判断。

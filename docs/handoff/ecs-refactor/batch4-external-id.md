# Codex 任务单：第 4 批：实体句柄不再跨出本进程（2026-10-15，第 1 版）

> **谁执行**：Codex。总规则、批次顺序、组件三类初始化口径、协调与回执格式见 [README.md](README.md)，本单不重复。
> **本版性质**：设计稿加挑战者意见的原样整理，还没有细化到「改前/改后代码片段」。Claude 的额度恢复后会出第 2 版（补丁级）。**先读 §8 挑战意见**：标为 high/medium 的问题，实施时必须按其中的修法修正设计。凡与 README §2 三类初始化规则冲突的地方，一律以 README 为准。
> **行号**：设计时取自当时的 HEAD，此后第 1 批和其他会话都改过代码，实施前请按函数名重新定位。

## 1. 目标

第 4 批的目标是让实体句柄不再离开本进程。具体分四个子批：
- 4a：Kafka 命令入口按 proto 上的显式标注做白名单。全仓盘点后，只有 3 个 scene 事件和 9 个 gate 事件确实有跨进程生产者，其余事件都不能再从 Kafka 打进来（F001/F002/F013/F098/F053/F081）。
- 4c：实体整数不再截成 uint32，也不再冒充 player_id / node_id 打日志；gate 会话里的节点句柄改成有类型的 entt::entity（F084/F085/F089/C001/F030/F033）。
- 4b：29 个纯进程内事件从 protobuf 改为普通 C++ 结构体，字段直接是 entt::entity；同时删除 game_registry.h 里自定义的 entt::to_entity、EventOwner、CreateEntityIfNotExists、RecreateEntity（F052/F063/F071/F086/F058/F039/F027/F097/F015/F016/F017/F026）。
- 4d：客户端可见的 actor 号与 entt 句柄解耦（F080），先建立唯一接缝，再按用户决定选实现。
F091 已在第 1 批处理，本批不碰。gRPC 资源相关条目归 muduo HTTP/2 迁移会话，本批也不碰。

## 2. 采用的标准做法

一、4a 白名单。由 proto 上的显式标注驱动生成，这是唯一事实源。
1. 新增 message option：`proto/common/options/event_ingress_option.proto`，写法沿用 `attribute_sync_option.proto`（无 package，扩展号 700001，紧挨 700000）。
   - 内容是 `enum EventIngress {EVENT_INGRESS_NONE=0; EVENT_INGRESS_SCENE_COMMAND=1; EVENT_INGRESS_GATE_COMMAND=2;}` 和 `extend MessageOptions { optional EventIngress OptionEventIngress = 700001; }`。
   - 未标注即只能在进程内 trigger。
2. 生成器从描述符集读标注，用新增的 `prototools.ReadEnumExtension`，按完整名加编号读取，与 `ReadBoolExtension` 同构。读取失败一律中止生成（fail-closed）。
3. 生成器做三项自检：
   - SCENE 标注只允许出现在 logic_event 目录，GATE 只允许出现在 contracts/kafka 的 `*_event.proto`；
   - 被标注的消息不得含名为 `entity` 或以 `_entity` 结尾的字段，这是「句柄不跨进程」的机械守卫；
   - proto 里 EventIngress 的枚举值必须与生成器里的常量一致。
4. 生成两个按入口分开的派发函数 `DispatchSceneCommandEvent` / `DispatchGateCommandEvent`，返回 `KafkaEventDispatchResult{kDispatched,kNotAllowed,kDecodeFailed}`。另外生成一个纯 constexpr 的白名单头文件，供单测直接引用。删除全量的 `DispatchProtoEvent` 和 `DefaultKafkaCommandDispatch`，节点 hook 必须显式声明自己的派发函数，从结构上杜绝退回全量派发。
5. 选这个方案的理由：
   - 白名单与契约同处一个文件，新增跨进程事件只需要在 proto 上加一行；
   - 不像生成器 yaml 那样与 proto 形成两份真相；
   - 不需要挪动事件所在目录，所以不会改变 package 和事件号，Go 与 battle 的生产者零改动；
   - 实例 UUID 防僵尸检查（ValidateCommandTarget）照旧位于派发之前；旧的每节点 topic 迁移窗口共用同一个回调，白名单同样生效。

二、4b 进程内事件改为普通结构体。这是 EnTT 的官方惯用法：dispatcher 事件是普通值类型，字段用 entt::entity，在注释里写明属于哪个 registry。
- 仓库已有先例：`OnConnected2TcpServerEvent`（rpc_connection_event.h）。
- 事件类型放在产生它的模块里（node / mission / scene / actor / combat / player 各一个头文件）。
- 订阅改为在 scene 组合根手写的接线文件里完成，handler 正文从原来生成文件的守护段原样搬过去。
- 跨进程契约（battle_event、team_event、gate_event）保持 proto，只携带业务 ID（player_id / session_id / scene_id / node_id），消费端在 handler 内自己解析成本地实体。
- 盘点结果：目前没有任何事件同时用于进程内和跨进程，不需要拆成两个类型。BattleSettlementEvent 进入 scene 之后，转成进程内的 ConditionEvent，本来就是两个类型。
- 规则写进文档：同一业务以后若既要跨进程又要进程内广播，就定义 proto 契约加 C++ 结构体两个类型，在入口 handler 里转换。
- 删除自定义 to_entity 之后，官方模板 `entt::to_entity(uint64_t)` 仍然可以编译，但语义是取 index，与原来相反。所以要加一道机械门禁：全仓 `entt::to_entity(` 必须为零。

三、4c 统一修法：
- 实体句柄只能以 `entity=<完整 64 位>` 的形式出现在日志里；
- 要标识玩家就打 Guid（player_id），要标识节点就打 NodeInfo.node_id；
- 不截 uint32，哈希用标准库的 `std::hash<entt::entity>`；
- 新增唯一的日志辅助 `ActorLog(entity)`，输出 `player_id=… entity=…`。Guid 组件类型将来改动（F032 所在批次）时只需改这一处；
- gate 会话里的节点指向改存 entt::entity，以 entt::null 作哨兵，消除各处的 `entt::entity{int}` 往返。

四、4d 先建唯一接缝 `ActorNetIds`，提供出站 `Of(entity)` 和入站 `Resolve(id)` 两个方向，所有下发与回传点都经过它。
- 默认实现方案 A，不改客户端契约：
  - 每个 actor 挂 `ActorNetIdComp{uint64 value}`；
  - 号由本进程单调计数从 1 发放，永不复用；
  - 反查表放在 `actorRegistry.ctx()`，由 `on_destroy<ActorNetIdComp>` 自动维护，这是 EnTT 维护私有索引的标准做法，不需要手工清理；
  - 语义与 Java 版现状一致：非 0 雪花号，在一次在线期间稳定。
- 选 A 的依据：AGENTS §11 的取舍顺序是「向后兼容 > 简单」。
- 方案 B 改变客户端可见语义，列为待决：玩家改用 player_id，NPC 用 bit63 打标的场景内号，与 66 号消息的 entity_id 以及 battle 的 actor_id 口径统一。

全程不手改生成产物：改模板和生成器，由 Codex 重新生成。

## 3. 覆盖的审计编号

F001、F002、F013、F098、F053、F081、F071、F058、F052、F063、F086、F039(仅 to_entity 往返部分)、F027(结构上杜绝复发)、F097、F015、F016、F017、F026、F079(随死事件 AfterEnterScene 一并删除)、F084、F085、F089、C001、F030、F033、F080

## 4. 实施步骤（预计改动约 115 个文件）

### 4.1 4a-1 新增事件入口 option，并按生产者盘点结果标注白名单

**文件**：
- `proto/common/options/event_ingress_option.proto(新)`
- `proto/common/event/battle_event.proto`
- `proto/common/event/team_event.proto`
- `proto/contracts/kafka/gate_event.proto`
- `cpp/generated/proto/CMakeLists.txt`
- `cpp/generated/proto/proto.vcxproj`
- `cpp/generated/proto/proto.vcxproj.filters`

1. 新建 option 文件：syntax=proto3，import google/protobuf/descriptor.proto，go_package 为 "proto/common/options;gameoptions"，不设 package（与 attribute_sync_option 一致）。内容为 enum EventIngress{EVENT_INGRESS_NONE=0;EVENT_INGRESS_SCENE_COMMAND=1;EVENT_INGRESS_GATE_COMMAND=2;}，以及 extend google.protobuf.MessageOptions{optional EventIngress OptionEventIngress=700001;}。注释写明：未标注 = 只能进程内 trigger；标注 = 允许经该入口从进程外投递，且载荷不得携带实体句柄。

2. 在消息内加 option (OptionEventIngress)，均为 message option，不影响线格式。

SCENE_COMMAND（3 个）：
- BattleSettlementEvent：生产者 C++ battle，cpp/nodes/battle/logic/battle_room_manager.cpp:255 SendSceneCommandTo；
- BattleConfirmedEvent：生产者同文件 :362 SendSceneEvent，经 :197 发出；
- PlayerTeamRefreshEvent：生产者 go/match/internal/team/notify.go:263-269。

GATE_COMMAND（9 个）：
- RoutePlayerEvent：go/scene_manager/internal/logic/enterscenelogic.go:1364-1371，带 payload；
- RedirectToGateEvent：同文件 :1172-1178；
- BindSessionEvent：go/login/internal/svc/gate_command.go:125-128；
- KickPlayerEvent：gate_command.go:137-140，payload 为空，走信封；
- PlayerLeaseExpiredEvent：go/player_locator/internal/logic/leasemonitor.go:220-252（事件号写死为 34）；
- PushToPlayerEvent：login gate_command.go:174-177；friend/guild/match 的 internal/kafka/gate_command_builder.go:44-45，经 go/shared/kafkautil/gate_push.go PushToPlayer 发出，调用方为 friend/internal/logic/push.go:140、guild/internal/logic/push.go:144、match/internal/playercontract/playercontract.go:149；C++ battle 在 battle_room_manager.cpp:118；
- BroadcastToPlayersEvent：三份 gate_command_builder.go:76-77，调用方 guild/internal/logic/push.go:146；
- BroadcastToSceneEvent、BroadcastToAllEvent：gate_command_builder.go:101/125 与 kafkautil.BroadcastToScene/BroadcastToAll。现在没有调用方，但生产代码存在，保留在白名单内。

不标注：
- PlayerDisconnectedEvent：没有 GateCommand 生产者；
- match_event.proto 的 BattleResultEvent / BattleResultTeam：走 match-results topic，后者只是子消息；
- player_event.proto 的 PlayerLifecycleCommand：走 player-events topic；
- PlayerMigrationEvent / PlayerMigrationAckEvent：没有生产者；
- common/event 其余全部事件。

Java 仓库没有 SceneCommand/GateCommand 生产者：Java 未接 Kafka，battle→scene 走 Dubbo SceneBattleService。

3. 照 attribute_sync_option 的先例，把 common/options/event_ingress_option.pb.cc/.pb.h 登记进 cpp/generated/proto 的 CMakeLists、vcxproj 和 filters。

### 4.2 4a-2 生成器读取标注并 fail-closed 自检

**文件**：
- `tools/proto_generator/protogen/internal/prototools/option/value.go`
- `tools/proto_generator/protogen/internal/prototools/option/value_test.go`
- `tools/proto_generator/protogen/internal/generator/cpp/event_ingress.go(新)`
- `tools/proto_generator/protogen/internal/generator/cpp/event_ingress_test.go(新)`
- `tools/proto_generator/protogen/internal/generator/cpp/event_id.go`

1. value.go 新增 `func ReadEnumExtension(message protoreflect.Message, expectedName protoreflect.FullName, expectedNumber protoreflect.FieldNumber) (value int32, found bool, err error)`，结构照 ReadBoolExtension：
- 已注册的扩展走反射，要求 Kind==EnumKind，取 fieldValue.Enum()；
- 未注册的扩展从 unknown fields 读 varint 再转成 int32；
- 编号被其他名字占用、类型不对时返回 error。

2. 新建 event_ingress.go：
- `type EventIngress int32`，常量 EventIngressNone=0、EventIngressSceneCommand=1、EventIngressGateCommand=2；
- `func checkEventIngressEnum(fdSet *descriptorpb.FileDescriptorSet) error`：在描述符集里找到 enum EventIngress，逐项核对名字和数值与上面的常量一致，不一致就中止，保证 proto 是唯一事实源；
- `func readEventIngress(msg *descriptorpb.DescriptorProto) (EventIngress, error)`：遇到未知数值报错；
- `func validateEventIngress(info *ProtoEventInfo, msg *descriptorpb.DescriptorProto, logicEventDir, kafkaDir string) error`。规则：SceneCommand 只允许出自 logic_event 目录；GateCommand 只允许出自 contracts_kafka 下的 gate_event.proto；被标注的消息不得有名为 entity 或以 _entity 结尾的字段。
以上都是纯函数，便于单测。

3. event_id.go：
- ProtoEventInfo 增加 `Ingress EventIngress` 字段；
- registerProtoEvents 在文本解析之外，到 internal.FdSet 里按 `strings.HasSuffix(fd.GetName(), filepath.ToSlash(filepath.Join(protoRelativeDir, file.Name())))` 找文件描述符，再按名字找消息，读出 Ingress 并校验；
- 找不到描述符或校验失败一律 logger.Global.Fatal；
- ReadAllProtoEvents 开头先调用一次 checkEventIngressEnum。

pipeline 的顺序本来就是 GenerateAllInOneDescriptor 之后才执行 ReadAllProtoEvents，两者都是 RunTaskWithWG 串行执行，所以描述符一定已经就绪。

### 4.3 4a-3 生成按入口拆分的派发函数，删除全量 DispatchProtoEvent

**文件**：
- `tools/proto_generator/protogen/internal/generator/cpp/service_register_info.go`
- `tools/proto_generator/protogen/internal/template/rpc_event_registry.cpp.tmpl`
- `tools/proto_generator/protogen/internal/template/service_header.tmpl`
- `tools/proto_generator/protogen/internal/template/kafka_command_event_whitelist.h.tmpl(新)`
- `tools/proto_generator/protogen/internal/generator/cpp/event_ingress.go`
- `tools/proto_generator/protogen/internal/generator/cpp/gate_kafka_command_router.go`
- `tools/proto_generator/protogen/internal/template/gate_kafka_command_router.cpp.tmpl`

1. service_header.tmpl：
- 声明 `enum class KafkaEventDispatchResult : uint8_t { kDispatched, kNotAllowed, kDecodeFailed };`
- 声明 `KafkaEventDispatchResult DispatchSceneCommandEvent(uint32_t eventId, const std::string& payload);`
- 声明 `KafkaEventDispatchResult DispatchGateCommandEvent(uint32_t eventId, const std::string& payload);`
- 删除 `DispatchProtoEvent` 的声明。

2. service_register_info.go 的 writeServiceInfoCppFile：把 eventDispatchCases 拆成 SceneCommandDispatchCases 和 GateCommandDispatchCases，只纳入 Ingress 对应的事件，按事件号排序。每个 case 的形状是：解析失败 return kDecodeFailed；成功则 tlsEcs.dispatcher.trigger 后 return kDispatched；default 分支 return kNotAllowed。rpc_event_registry.cpp.tmpl 相应渲染这两个函数，并删除 DispatchProtoEvent。仓内 DispatchProtoEvent 只有两个调用方，都是 Kafka 路由。

3. event_ingress.go 新增 writeKafkaCommandEventWhitelistHeader：
- 在 service_register_info.go:537 调用 writeEventIdHeaderFiles() 的同一处调用；
- 输出 cpp/generated/rpc/service_metadata/kafka_command_event_whitelist.h，内容为 `constexpr bool IsSceneCommandEvent(uint32_t)` 与 `constexpr bool IsGateCommandEvent(uint32_t)` 两个 switch；
- 只 include 用到的 *_event_id.h；
- 用 utils2.WriteFileIfChanged 写出。

4. gate_kafka_command_router.go 的 buildGateKafkaRouterCases 只为 readEventIngress(eventMsg)==GateCommand 的消息生成信封（空 payload）兜底 case。PlayerDisconnectedEvent 的兜底 case 因此消失。

5. gate 路由模板：payload 分支改调 DispatchGateCommandEvent。kNotAllowed 打 LOG_ERROR（带 event_id 和 topic，说明生产者违约）后丢弃；kDecodeFailed 打 LOG_WARN。

### 4.4 4a-4 节点接线：去掉默认全量派发，节点 hook 显式声明派发函数

**文件**：
- `cpp/libs/engine/core/node/system/node/node_kafka_command_handler.h`
- `cpp/libs/engine/core/node/system/node/node_entry.h`
- `cpp/nodes/scene/main.cpp`
- `cpp/nodes/gate/main.cpp`
- `cpp/nodes/scene/handler/event/scene_kafka_command_router.cpp`
- `cpp/nodes/scene/handler/event/battle_event_handler.cpp(仅守护段注释)`
- `cpp/nodes/scene/handler/event/team_event_handler.cpp(仅守护段注释)`
- `cpp/libs/services/scene/player/system/player_team.cpp(仅 :37 注释)`
- `go/login/internal/svc/gate_command.go(仅 :106 注释)`

1. node_kafka_command_handler.h：删除 DefaultKafkaCommandDispatch 和只带 Node& 的全自动重载，只保留显式传入 dispatchFn 的两个重载。

2. node_entry.h 的 ApplyPostConstructionHooks：当 THooks 有 KafkaCommandType 时，用 static_assert 要求存在 `static void THooks::DispatchKafkaCommand(const std::string&, const typename THooks::KafkaCommandType&)`，并以它注册 `RegisterKafkaCommandHandler<KafkaCommandType>(n, &THooks::DispatchKafkaCommand)`。

3. scene/main.cpp：SceneNodeHooks 增加 `static void DispatchKafkaCommand(const std::string& t, const contracts::kafka::SceneCommand& c){ DispatchSceneKafkaCommand(t, c); }`，并 include scene_kafka_command_router.h。DispatchSceneKafkaCommand 现在编译了但是没人用，这里接上。

4. gate/main.cpp：GateNodeHooks 同样转调 DispatchGateKafkaCommand；删除 :277-282 那段 SetKafkaHandlers 覆盖代码（已经冗余）。

5. scene_kafka_command_router.cpp 改调 DispatchSceneCommandEvent，结果映射同 gate：kNotAllowed 打 LOG_ERROR，kDecodeFailed 打 LOG_WARN。

6. 所有提到 DispatchProtoEvent 的注释改为新函数名。

不改的部分：目标过滤（ValidateCommandTarget 加 target_instance_id 防僵尸）仍在派发之前；旧的每节点 topic 迁移窗口共用 makeCallback，白名单同样生效。

### 4.5 4a-5 白名单单测与文档

**文件**：
- `cpp/tests/kafka_command_test/kafka_command_whitelist_test.cpp(新)`
- `cpp/tests/kafka_command_test/kafka_command_test.vcxproj`
- `docs/design/ecs-entt-misuse-audit-20261015.md`
- `docs/design/control-plane-topic-partitioning-20260908.md`
- `docs/ops/incident-scene-ecs-gameplay-20261014.md`
- `docs/PROGRESS.md`

1. 单测只 include 生成的 kafka_command_event_whitelist.h 和各 *_event_id.h，不链接 rpc_event_registry.cpp。
- IsSceneCommandEvent 应为 true：BattleSettlement / BattleConfirmed / PlayerTeamRefresh；
- IsSceneCommandEvent 应为 false：BeforeLeaveScene、InitializePlayerComps、ConditionEvent、CombatStateAdded、PlayerUpgrade、OnNodeRemove、墓碑号 43/44、≥kMaxEventCount 的号；
- IsGateCommandEvent 应为 true：上面列出的 9 个；
- IsGateCommandEvent 应为 false：PlayerDisconnected、PlayerLifecycleCommand、BattleResultEvent、OnNodeRemoveEvent（即 F081）。

2. 文档：
- 审计文档更新 F001/F002/F013/F098/F053/F081 的 HEAD 状态；
- 在控制面 topic 文档里补一节规则：新增跨进程事件 = 在 proto 上加标注，没有标注就只能进程内使用；
- 事故文档里 F001 的待办改为指向本方案；
- PROGRESS.md 追加一条（只追加，注明 Java 版本号）。

### 4.6 4c-1 日志身份统一：不冒充 player_id，不截断

**文件**：
- `cpp/libs/engine/thread_context/actor_log_id.h(新)`
- `cpp/libs/engine/thread_context/thread_context.vcxproj(+filters)`
- `cpp/libs/modules/mission/system/mission.cpp`
- `cpp/nodes/scene/handler/rpc/player/player_lifecycle_handler.cpp`
- `cpp/libs/services/scene/player/system/player_scene.cpp`

1. 新建 actor_log_id.h（header-only）：
```cpp
struct ActorLogTag { Guid playerId; entt::entity entity; };
[[nodiscard]] inline ActorLogTag ActorLog(entt::entity actor);
```
- ActorLog 先判 tlsEcs.actorRegistry.valid，再 try_get<Guid>，取不到时 playerId 记 0；
- 提供 `inline muduo::LogStream& operator<<(muduo::LogStream&, const ActorLogTag&)`，输出 "player_id=<guid> entity=<to_integral 完整 64 位>"；
- 注释写明这是日志里标识 actor 的唯一写法；以后 Guid 组件类型变化（F032 所在批次）只改这里。

2. 替换以下冒充玩家号的日志：
- mission.cpp:82、:111、:121（MAKE_ERROR_MSG 内）、:131，原文为 "playerId = " << to_integral → << ActorLog(...)（C001）；
- player_lifecycle_handler.cpp:18、:28，均在守护段内（F089）；
- player_scene.cpp:251。

3. 同类但可能落在第 1 批独占范围内的点，等第 1 批合入后再改：player_lifecycle.cpp:2782、bag_marshal.cpp:189（"player="）。

4. 已经标成 "entity=" 的日志（例如 currency_system.cpp）是合规写法，不改。

### 4.7 4c-2 gate 断线日志与节点 registry 命名（F084/F085）

**文件**：
- `cpp/nodes/gate/handler/rpc/client_message_processor.cpp`

改 HandleConnectionDisconnection（约 :428-437 与 :521）：
- 删除 `uint32_t sceneNodeId = sceneEntityId`（64 位实体整数被隐式截成 uint32，还被当作 node_id 打日志）；
- 改为通过 sceneNodeRegistry.try_get<NodeInfo>(sceneEntity) 取 info->node_id() 作为 scene_node_id，取不到记 0；
- 局部变量 sceneRegistry 改名为 sceneNodeRegistry（F085：避免与 tlsEcs.sceneRegistry 混淆）；
- 日志里的 scene_node_id 现在是真正的业务 node_id。

### 4.8 4c-3 AnomalyDetector 哈希截断（F030）

**文件**：
- `cpp/libs/modules/transaction_log/anomaly_detector.h`
- `cpp/libs/modules/transaction_log/anomaly_detector.cpp`

删除 EntityHash（它把 64 位实体截成 uint32 再求哈希），buckets_ 改为 `std::unordered_map<entt::entity, PlayerBuckets>`。标准库对枚举类型提供 std::hash，entt::entity 是 enum class，可以直接用。

F064（这张旁路表应改为玩家组件）不在本批范围，只在交付说明里注明。

### 4.9 4c-4 gate 会话节点指向改为有类型句柄（F033）

**文件**：
- `cpp/libs/services/gate/session/comp/session_info_comp.h`
- `cpp/nodes/gate/handler/event/scene_route_helper.h`
- `cpp/nodes/gate/handler/event/scene_entry_dispatch.cpp`
- `cpp/nodes/gate/handler/rpc/gate_service_handler.cpp`
- `cpp/nodes/gate/handler/rpc/client_message_processor.cpp`
- `cpp/nodes/gate/handler/rpc/client_request_lane.h`
- `cpp/nodes/gate/handler/rpc/client_request_lane.cpp`
- `cpp/tests/routing_identity_test/routing_identity_test.cpp`

1. SessionInfo：
- `std::unordered_map<uint32_t, uint64_t>` 改为 `std::unordered_map<uint32_t, entt::entity>`；
- 接口改为 `void SetNodeEntity(uint32_t nodeType, entt::entity)`、`entt::entity GetNodeEntity(uint32_t nodeType) const`（缺失时返回 entt::null）、`bool HasNodeEntity(uint32_t) const`；
- 删除 kInvalidEntityId；
- 注释写明：值是 tlsNodeContextManager.GetRegistry(nodeType) 里的句柄，只在本进程有效。

2. 调用点：
- scene_route_helper.h：CompareSceneNode、RebindSceneNode、AttemptPendingSceneEntry 的 resolve/forward 签名从 uint64_t 改为 entt::entity；
- scene_entry_dispatch.cpp：ResolveSceneNodeEntity 返回 std::optional<entt::entity>，ForwardPlayerToScene 的参数同步改；
- gate_service_handler.cpp:76：直接 SetNodeEntity(SceneNodeService, *sceneNodeEntity)；
- client_message_processor.cpp：GetEffectiveNodeId 返回 entt::entity，并删除 `entt::entity{int}` 往返（:229 :436 :707 等处）。

3. client_request_lane：同类问题一并处理。RouterSendFn 返回 std::optional<entt::entity>；InflightCall.routerEntity 改为 entt::entity，默认 entt::null（原来默认 0，而 0 在节点 registry 里是合法句柄）；OnRouterNodeRemoved 参数改为 entt::entity。

4. OnNodeRemoveEventHandler 在 4b 落地之前，用 `entt::entity{pb.entity()}` 与会话里存的句柄比较；4b 落地后改为 event.node。

5. routing_identity_test 按新接口更新，并补一个用例：同一 index、不同 version 的句柄必须判为不同节点。

### 4.10 4b-1 进程内事件定义为 C++ 结构体（第 1 批合入且 4a/4c 落地后执行）

**文件**：
- `cpp/libs/engine/core/node/system/node/node_events.h(新)`
- `cpp/libs/modules/mission/mission_events.h(新)`
- `cpp/libs/modules/scene/scene_events.h(新)`
- `cpp/libs/services/scene/actor/actor_events.h(新)`
- `cpp/libs/services/scene/combat/combat_events.h(新)`
- `cpp/libs/services/scene/player/player_events.h(新)`
- `对应库的 vcxproj/.filters(ClInclude)`

保留原事件名以减少改动面；所有字段给默认值，句柄默认 entt::null；注释写明句柄属于哪个 registry。

node_events.h（nodeRegistry 指 tlsNodeContextManager.GetRegistry(nodeType)）：
- `struct OnNodeRemoveEvent{entt::entity node; uint32_t nodeType; uint32_t sceneNodeType;};`
- `struct ConnectToNodeEvent{entt::entity node; uint32_t nodeType;};`（F015/F097：不再是 uint32）
- `struct OnServerStart{};`

mission_events.h（actorRegistry）：
- `AcceptMissionEvent{entt::entity player; uint32_t missionId;}`
- `OnAcceptedMissionEvent{player, missionId}`
- `OnMissionAwardEvent{player, missionId}`
- `ConditionEvent{entt::entity player; uint32_t conditionType; std::vector<uint32_t> conditionIds; uint32_t amount;}`

scene_events.h：
- `OnSceneCreated{entt::entity scene;}` 与 `OnSceneDestroyed{entt::entity scene;}`，属于 sceneRegistry；
- `BeforeLeaveScene{entt::entity actor;}`，属于 actorRegistry。

actor_events.h：
- `InitializeActorCompsEvent{actor}`
- `InterruptCurrentStateEvent{actor; uint32_t actorAction; uint32_t actorState;}`
- `CombatStateAddedEvent` / `CombatStateRemovedEvent{actor; uint64_t sourceBuffId; uint32_t stateType;}`

combat_events.h：
- `BeKillEvent{entt::entity caster; entt::entity target;}`
- `SkillExecutedEvent{uint64_t skillId; uint32_t skillTableId; entt::entity caster; entt::entity target;}`，这个类型由 skill.cpp 直接作为参数传给 BuffSystem::OnSkillExecuted，不经 dispatcher。

player_events.h：
- `RegisterPlayerEvent{player}`
- `PlayerUpgradeEvent{player; uint32_t newLevel;}`
- `InitializePlayerCompsEvent{player}`
- `PlayerLoginEvent{player; uint32_t enterGsType;}`

没有生产者的事件直接删除，不迁移（YAGNI）：BuffTestEvent、InitializeNpcCompsEvent、OnNodeAddEvent、OnNodeConnectedEvent、OnConnect2Login、BeforeEnterScene、AfterEnterScene、AfterLeaveScene、S2CEnterScene。随之删除 SceneCrowdSystem::AfterEnterSceneHandler（F079，跨 registry 的死代码）。

### 4.11 4b-2 删除 11 个进程内事件 proto 并重新生成（事件号转墓碑）

**文件**：
- `proto/common/event/{actor_combat_state_event,actor_event,buff_event,combat_event,mission_event,node_event,npc_event,player_event,scene_event,server_event,skill_event}.proto(删除)`
- `proto/event_id.txt(由生成器改写)`
- `cpp/generated/proto/CMakeLists.txt`
- `cpp/generated/proto/proto.vcxproj(+filters)`
- `陈旧生成物:cpp/generated/proto/common/event/<上述>.pb.{h,cc}、cpp/generated/rpc/service_metadata/common_event_<上述>_event_id.h、go/proto/common/event/<上述>.pb.go`

1. 删除上面 11 个 proto。保留 battle_event.proto、team_event.proto、player_migration_event.proto。

2. Codex 以 PROTOGEN_ALLOW_MASS_EVENT_TOMBSTONE=1 运行生成器：一次转出 29 个墓碑，超过 maxNewTombstonesPerRun=4 的上限，必须显式放行。事件号永久保留不复用；kMaxEventCount 仍为 49；事件号头文件里的数值不变，所以 player_locator 写死的 34 不受影响。

3. 提交前人工核对 event_id.txt：只能新增墓碑行，不能有任何活号被改号。

4. 生成器不会删除陈旧产物，需要手动删除上面列出的陈旧 .pb.h/.pb.cc/_event_id.h/.pb.go，并从 proto 的 CMakeLists 和 vcxproj 里摘掉。

5. 门禁：`git grep -n "proto/common/event/(actor_combat_state|actor|buff|combat|mission|node|npc|player|scene|server|skill)_event.pb.h"` 必须为零。否则同名的旧 proto 类会和新结构体在全局命名空间冲突。

### 4.12 4b-3 scene 进程内事件改为手写接线，搬运 handler 正文

**文件**：
- `cpp/nodes/scene/handler/local_event/scene_local_events.h(新)`
- `cpp/nodes/scene/handler/local_event/scene_local_events.cpp(新)`
- `cpp/nodes/scene/main.cpp`
- `cpp/nodes/scene/CMakeLists.txt`
- `cpp/nodes/scene/scene.vcxproj(+filters)`
- `cpp/nodes/scene/handler/event/{actor_combat_state,actor,buff,combat,mission,node,npc,player,scene,server,skill}_event_handler.{h,cpp}(删除)`
- `cpp/tests/bag_test/bag_test.vcxproj`
- `cpp/tests/missions_test/missions.vcxproj`

1. 新文件 scene_local_events 提供 `namespace scene_local_events { void RegisterAll(); void UnregisterAll(); void RegisterMission(); void UnregisterMission(); }`。按域分组是为了测试能单独装 mission 的订阅。全部用自由函数或静态函数 connect，不绑 *this。

2. 把原生成文件守护段里的 handler 正文原样搬过来，只把字段访问改为结构体字段：
- mission 的 3 个 handler；
- player 的 4 个；
- scene 的 OnSceneCreated（agones 计数 + AoiSystem::OnSceneCreatedHandler）、OnSceneDestroyed、BeforeLeaveScene（SceneCrowd + Aoi）；
- actor 的 InitializeActorComps：搬的是第 1 批修改之后的版本；
- combat_state 的两个。
空 handler 不搬：事件没人订阅时 trigger 是 no-op。

3. scene/main.cpp 在 ApplyPostConstructionHooks 之后显式调用 scene_local_events::RegisterAll()。

4. 生成的 event_handler.cpp 汇总文件经 regen 后只剩 battle、team、player_migration 三个类。*(2026-10-20 批注,10-22 补:汇总文件 `event_handler.{h,cpp}` 本身已在 `e76f6f747`(2026-10-16)删除,生成器现在只按 proto 逐个产出 `*_event_handler.{h,cpp}`。player_migration 的 handler 已在 `e7bb46070`(2026-10-18)删除,它属于已下线的跨 zone 搬数据链。现在 `cpp/nodes/scene/handler/event/` 下是 battle、player_inbox、team 三个 handler 加 `scene_kafka_command_router`。)*

5. 删除 11 对旧的生成 handler 文件，并从 scene 节点工程以及 bag_test.vcxproj:181、missions.vcxproj:160 里摘掉 mission_event_handler.cpp，换成新文件。

### 4.13 4b-4 改生产者与消费者的字段访问

**文件**：
- `cpp/libs/engine/core/node/system/node/node.cpp(:999、:1528-1532)`
- `cpp/libs/engine/core/node/system/registration/registration_manager.cpp(:204-214)`
- `cpp/libs/modules/mission/system/mission.{h,cpp}`
- `cpp/libs/modules/mission/comp/mission_comp.h`
- `cpp/libs/services/scene/player/system/player_mission.cpp`
- `cpp/libs/services/scene/player/system/player_lifecycle.cpp(:2735、:2854-2856、:3830-3845)`
- `cpp/libs/services/scene/player/system/player_scene.cpp(:176-178)`
- `cpp/libs/services/scene/combat/buff/system/buff_impl.cpp(:85-99)`
- `cpp/libs/services/scene/combat_state/system/combat_state.{h,cpp}`
- `cpp/libs/services/scene/spatial/system/scene_crowd.{h,cpp}`
- `cpp/libs/services/scene/spatial/system/aoi.{h,cpp}(仅两个事件 handler 的签名与取字段)`
- `cpp/libs/services/scene/combat/skill/system/skill.cpp(:714、:788)`
- `cpp/libs/services/scene/combat/buff/system/buff.{h,cpp}(OnSkillExecuted 签名)`
- `cpp/libs/services/scene/actor/action_state/system/actor_action_state.cpp(:38)`
- `cpp/libs/services/scene/player/system/player_attribute.cpp(:1070)`
- `cpp/libs/services/scene/battle/system/player_battle.cpp(:1802-1807)`
- `cpp/nodes/scene/handler/grpc/scene_node_service.cpp(:58-60、:131-133)`
- `cpp/nodes/scene/handler/rpc/scene_handler.cpp(:896、:959-961)`
- `cpp/nodes/gate/handler/rpc/client_message_processor.cpp(OnNodeRemoveEventHandler)`

1. 构造处统一改为结构体聚合初始化，例如 `tlsEcs.dispatcher.trigger(ConditionEvent{player, type, {ids}, amount})`，删除全部 set_xxx(entt::to_integral(...))。

2. 消费处：
- `entt::to_entity(event.entity())` 改为 event.player / event.actor / event.scene；
- mission.cpp 的 HandleConditionEvent 等函数里，condition_ids(i) / condition_ids_size() 改为遍历 std::vector；
- 头文件里的前置声明从 `class X;` 改为 `struct X;`，或直接 include 事件头文件，避免 MSVC C4099；
- agones 的 OnUnitCreated/OnUnitDestroyed 仍传 entt::to_integral(event.scene)。这是进程内去重键，语义不变。

3. gate 的 RpcClientSessionHandler::OnNodeRemoveEventHandler：比较 `session.GetNodeEntity(event.nodeType) != event.node`；gate_lane::OnRouterNodeRemoved(event.node)。

4. registration_manager.cpp:212 填 `ConnectToNodeEvent{entity, nodeType}`，完整 64 位，即 F015/F097。

5. 只改事件相关的行。上面多处文件属于第 1 批的独占范围（aoi、skill、buff、actor_action_state、player_attribute、player_battle、scene_node_service、scene_handler 的 CreateScene），所以本步必须等第 1 批合入之后基于它的结果改。node.cpp:1528-1532 紧挨 F010 的摘除代码，要与 gRPC→muduo 迁移会话协调。

### 4.14 4b-5 删除 game_registry.h 的自定义 to_entity、EventOwner 和按外部整数建实体的接口（F016/F017/F026）

**文件**：
- `cpp/libs/engine/core/utils/registry/game_registry.h`
- `cpp/libs/engine/core/utils/registry/game_registry.cpp`
- `cpp/libs/modules/mission/comp/mission_comp.h`
- `cpp/libs/services/scene/combat/skill/system/skill.cpp(:201、:406、:725)`

1. 删除以下内容，只保留 DestroyEntity（node.cpp:1534、player_lifecycle.cpp:2849 仍在用）：
- class EventOwner；
- namespace entt 里的 to_entity(uint64_t)；
- CreateEntityIfNotExists 和 RecreateEntity（声明与 .cpp 实现）。

2. MissionsComp 去掉 `: public EventOwner`。若删除后编译报出隐式转换的使用点，逐个改为显式传参。

3. skill.cpp 剩下的 3 处来自 protobuf 运行时组件（SkillContext 的 target、DamageEvent 的 target），这里存的仍是本进程实体整数，改为 `entt::entity{value}`。这些组件本身改造另算（F043/F052 剩余部分），不在本批范围。

4. 机械门禁：`git grep -n "entt::to_entity(" -- cpp tools/proto_generator` 必须为零。原因是删掉自定义版本后，官方模板对 uint64_t 照样能编译，但语义是取 index 部分，会静默改变行为。

### 4.15 4b-6 更新受影响的测试

**文件**：
- `cpp/tests/missions_test/missions_test.cpp`
- `cpp/tests/bag_test/player_mission_system_test.cpp`
- `cpp/tests/bag_test/player_feature_persistence_test.cpp`
- `cpp/tests/bag_test/player_feature_handler_test.cpp`
- `cpp/tests/bag_test/player_battle_settlement_test.cpp`
- `cpp/tests/aoi_test/aoi_system_test.cpp`
- `cpp/tests/aoi_delivery_test/aoi_delivery_test.cpp`
- `cpp/tests/routing_identity_test/routing_identity_test.cpp`

1. 构造事件的写法从 `set_entity(to_integral(x))` 改为结构体字段赋值。

2. MissionEventHandler::Register()（player_mission_system_test.cpp:69/72、missions_test.cpp:31）改为调用 scene_local_events::RegisterMission() / UnregisterMission()。

3. 新增用例：
- ConnectToNodeEvent 携带的节点句柄保留 version：创建、销毁、再创建同一 index 之后，监听者收到的句柄等于新句柄；
- ConditionEvent 的多个 conditionIds 能正确推进任务。

### 4.16 4d-1 actor 对外号接缝 ActorNetIds（默认方案 A，客户端契约不变；需先确认 decisions_needed 第 1 条）

**文件**：
- `cpp/libs/services/scene/actor/actor_net_id.h(新)`
- `cpp/libs/services/scene/actor/actor_net_id.cpp(新)`
- `cpp/libs/services/scene/CMakeLists.txt`
- `cpp/libs/services/scene/scene.vcxproj(+filters)`
- `cpp/libs/services/scene/world/world.cpp`
- `cpp/tests/aoi_test/aoi_system_test.cpp(夹具)`
- `cpp/tests/aoi_delivery_test/aoi_delivery_test.cpp(夹具)`
- `cpp/tests/skill_test/skill_test.cpp(夹具)`

1. 新建 actor_net_id.h/.cpp：
- `struct ActorNetIdComp{ uint64_t value{0}; };`
- `namespace actor_net_id { void Install(entt::registry& actors); uint64_t Assign(entt::registry&, entt::entity); uint64_t Of(const entt::registry&, entt::entity); entt::entity Resolve(const entt::registry&, uint64_t netId); }`

2. 状态放在 `actors.ctx()` 里的 `ActorNetIdIndex{ uint64_t next{1}; std::unordered_map<uint64_t, entt::entity> byNetId; }`。

3. Install 做两件事，且可重复调用：
- ctx().emplace 这个索引；
- 用 `on_destroy<ActorNetIdComp>().connect<&OnDestroyed>()` 在实体销毁时自动从索引里删除；registry.clear() 也会触发这个回调。

4. 各函数语义：
- Assign 幂等：实体已有号就返回原值；没有调用过 Install 时 LOG_ERROR 并返回 0，即 fail-closed；
- Of 取不到号时返回 0，调用方据此不下发；
- Resolve 查不到时返回 entt::null。

5. 号的语义：本进程内非 0、单调递增、永不复用；actor 实体存续期间稳定，同节点换场景也不变；与 entt 的 index/version 完全无关。

6. 在 World::InitializeSystemBeforeConnect 里调用 Install(tlsEcs.actorRegistry)。引擎层的 EcsContext 不认识 scene 组件，不放在那里。测试夹具里也调用 Install。

### 4.17 4d-2 所有下发与回传点改走接缝

**文件**：
- `cpp/libs/services/scene/player/system/player_lifecycle.cpp(建实体处,约 :3822 emplace<Guid> 旁)`
- `cpp/libs/services/scene/spatial/system/view.cpp(:90)`
- `cpp/libs/services/scene/spatial/system/aoi.cpp(:220、:369)`
- `cpp/libs/services/scene/spatial/system/movement.cpp(:163、:210)`
- `cpp/libs/services/scene/combat/skill/system/skill.cpp(ResolveSkillTarget 及 :166/:180/:216/:239/:498/:520/:613-614/:654)`
- `cpp/libs/services/scene/player/system/player_scene.cpp(:217 selfCreate)`
- `proto/scene/player_scene.proto(仅注释)`
- `proto/scene/player_skill.proto(仅注释)`
- `proto/scene/player_movement.proto(仅注释)`

1. 建号：玩家实体创建处（emplace<Guid> 旁边）调用 actor_net_id::Assign。生产代码目前没有建 NPC 的地方；Assign 的注释写明，以后 NPC 或怪物生成也必须调用它。

2. 出站：ActorCreateS2C.entity、ActorDestroyS2C、ActorListDestroyS2C、ActorMoveS2C、TeleportS2C、SkillUsedS2C、SkillInterruptedS2C 中原来用 entt::to_integral 的地方，一律改为 actor_net_id::Of(...)。值为 0 时不下发该项并打 LOG_ERROR，因为 0 在协议里表示「没有目标」。

3. 入站：ResolveSkillTarget 改为接收 entt::entity，校验逻辑（玩家或 NPC、同场景、未冻结）保持不变。
- 客户端路径先 `actor_net_id::Resolve(reg, request->target_id())`，再做校验；
- :498 的 `entt::entity{ request->target_id() }` 删除；
- 技能上下文里存的本进程整数仍用 `entt::entity{}` 还原；
- SkillUsedS2C 回显的 target_entity 改为解析后目标的 Of(target)，不再原样回显客户端上传的值。

4. 66 号消息 ActorBaseAttributesS2C.entity_id 保持 Guid，这是契约，不改。

5. 三个 proto 只改注释，写明：「actor 号：不透明、非 0、本进程内不复用，不是实体句柄，也不是 player_id」。字段类型不变。

6. aoi、movement、view、skill 都属于第 1 批的独占范围，本步必须在第 1 批和 4b 之后进行。

### 4.18 4d-3 双版本对账与文档

**文件**：
- `docs/design/ecs-entt-misuse-audit-20261015.md`
- `docs/PROGRESS.md`
- `D:/luyuan/xuanming-server-mmo/PARITY.md(Java 仓库)`
- `D:/luyuan/xuanming-server-mmo/docs/reference/mmorpg-client-contract-aoi.md(Java 仓库 §3.1 表格 entity 行与 §3.5)`

1. 方案 A 下，两版都使用 actor 存续期间稳定的不透明号。C++ 号为 node_id<<32|seq，同时在线节点之间唯一、本进程内不复用；进程重启或 node_id 回收后，同一 node_id 从 seq=1 重新发号。Java 用 SceneWorld.nextId 雪花号，跨重启也唯一。号源维持现状，Java 代码不需要改。

2. Java 契约文档 :146 现在写着「C++ 是 entt::to_integral，会复用」，需改为新语义。

3. 在 Java 仓库的 PARITY.md 登记一行，写明本仓库 commit。

4. 若用户选择方案 B，Java 必须同批修改：ScenePlayer.entity 改为 playerId，技能目标解析从 playersByEntity 改为 playersById，robot 断言同步调整。

## 5. 生成器模板与生成产物

- 模板 tools/proto_generator/protogen/internal/template/rpc_event_registry.cpp.tmpl：删除 DispatchProtoEvent，新增 DispatchSceneCommandEvent 与 DispatchGateCommandEvent，返回 KafkaEventDispatchResult。
- 模板 service_header.tmpl：声明 KafkaEventDispatchResult 与上述两个函数，删除 DispatchProtoEvent 的声明。
- 新模板 kafka_command_event_whitelist.h.tmpl：生成 cpp/generated/rpc/service_metadata/kafka_command_event_whitelist.h，内含 constexpr 的 IsSceneCommandEvent 与 IsGateCommandEvent。
- 模板 gate_kafka_command_router.cpp.tmpl：payload 分支改调 DispatchGateCommandEvent；信封兜底分支只生成带 GATE 标注的事件。
- 生成器 Go 代码：prototools/option/value.go 新增 ReadEnumExtension；generator/cpp 新增 event_ingress.go；event_id.go 的 ProtoEventInfo 增加 Ingress 字段并接入描述符查询与自检；service_register_info.go 拆分派发 case，并调用白名单头文件的写出函数；gate_kafka_command_router.go 增加按标注过滤。
- 由 Codex 重新生成的产物，不手改：
- cpp/generated/rpc/service_metadata/rpc_event_registry.{h,cpp} 与 kafka_command_event_whitelist.h；
- cpp/nodes/gate/handler/event/gate_kafka_command_router.cpp；
- cpp/generated/proto/common/options/event_ingress_option.pb.*；
- go/proto/common/options 下的 pb；
- 各服务的 go/*/generated/pb/game/event_id.go；
- 4b 之后：proto/event_id.txt 的墓碑行、cpp/nodes/scene/handler/event/event_handler.cpp 汇总文件（只剩 battle、team、player_migration 三个 handler）。*（2026-10-20 批注，10-22 订正：该汇总文件已在 `e76f6f747` 删除，player_migration handler 已在 `e7bb46070` 删除，现状见上面第 4 条的批注。）*
- 需要手动删除的陈旧生成物（生成器不删）：cpp/nodes/scene/handler/event 下 11 对 *_event_handler.{h,cpp}（守护段里的用户代码先搬到 scene_local_events.cpp）；cpp/generated/proto/common/event 下 11 对 .pb.{h,cc}；cpp/generated/rpc/service_metadata 下 11 个 common_event_*_event_id.h；go/proto/common/event 下 11 个 .pb.go。同时从 cpp/generated/proto 的 CMakeLists.txt、proto.vcxproj、.filters，以及 scene 节点工程、bag_test.vcxproj、missions.vcxproj 中摘掉对应条目。
- 生成式 handler 文件中守护段内的改动（属于允许手写的区域）：player_lifecycle_handler.cpp 的日志；battle_event_handler.cpp 与 team_event_handler.cpp 里提到 DispatchProtoEvent 的注释。

## 5a. proto 改动

- 新增 proto/common/options/event_ingress_option.proto：enum EventIngress{EVENT_INGRESS_NONE=0;EVENT_INGRESS_SCENE_COMMAND=1;EVENT_INGRESS_GATE_COMMAND=2;}，以及 extend google.protobuf.MessageOptions{optional EventIngress OptionEventIngress=700001;}。无 package，go_package 为 proto/common/options;gameoptions。扩展号 700001 目前未被占用。
- proto/common/event/battle_event.proto：BattleSettlementEvent、BattleConfirmedEvent 加 option (OptionEventIngress)=EVENT_INGRESS_SCENE_COMMAND，并 import 上面的新文件。只加 message option，线格式不变。
- proto/common/event/team_event.proto：PlayerTeamRefreshEvent 加 option (OptionEventIngress)=EVENT_INGRESS_SCENE_COMMAND。
- proto/contracts/kafka/gate_event.proto：RoutePlayerEvent、KickPlayerEvent、PlayerLeaseExpiredEvent、BindSessionEvent、RedirectToGateEvent、PushToPlayerEvent、BroadcastToPlayersEvent、BroadcastToSceneEvent、BroadcastToAllEvent 加 option (OptionEventIngress)=EVENT_INGRESS_GATE_COMMAND。PlayerDisconnectedEvent 不标。
- 4b：删除 proto/common/event 下的 actor_combat_state_event、actor_event、buff_event、combat_event、mission_event、node_event、npc_event、player_event、scene_event、server_event、skill_event 共 11 个文件（均为进程内事件，不是客户端契约）。event_id.txt 由生成器转出 29 个墓碑，事件号永不复用。
- 4d：proto/scene/player_scene.proto、player_skill.proto、player_movement.proto 只改 entity / target_entity / target_id 字段的注释，写明「actor 号：不透明、非 0、本进程内不复用，不是实体句柄」。字段编号与类型不变。如果选方案 B，注释语义改为「玩家 = player_id」，字段类型仍然不变。

## 6. 测试

- 生成器 Go 单测（Codex 执行）。工作目录 tools/proto_generator/protogen，命令 `go test ./internal/prototools/option/... ./internal/generator/cpp/...`。需覆盖：
- ReadEnumExtension 的已注册反射路径与 unknown-field 路径，以及编号被占用、类型错误两种报错；
- validateEventIngress：SCENE 标在 contracts 目录要报错，GATE 标在 common/event 要报错，被标注消息含 actor_entity 字段要报错，未标注事件不得进入白名单；
- checkEventIngressEnum：proto 枚举值与 Go 常量不一致时报错；
- 已有的 event_id_test.go 全部保持绿。
- 重新生成（Codex 执行，工作目录为仓库根）：先 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`，再 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`。4b 这一轮要带 `PROTOGEN_ALLOW_MASS_EVENT_TOMBSTONE=1` 执行。通过标准：
- event_id.txt 只新增 29 行 `N=reserved:` 墓碑，活号零变化；
- 生成的 rpc_event_registry.cpp 里不再出现 DispatchProtoEvent；
- DispatchSceneCommandEvent 恰好 3 个 case，DispatchGateCommandEvent 恰好 9 个 case；
- gate_kafka_command_router.cpp 的信封兜底 case 恰好 9 个，不含 PlayerDisconnected。
失败时保留生成器日志。
- 新单测 cpp/tests/kafka_command_test/kafka_command_whitelist_test.cpp：IsSceneCommandEvent / IsGateCommandEvent 的正例与反例。反例包括 BeforeLeaveScene、InitializePlayerComps、ConditionEvent、OnNodeRemoveEvent、PlayerDisconnected、墓碑号 43/44、≥kMaxEventCount 的号。已有的 ValidateCommandTarget 系列用例保持绿。
- 4c 测试：
- routing_identity_test 改用 SetNodeEntity/GetNodeEntity；
- 新增用例：同一 index、不同 version 的节点句柄判为不同节点；节点摘除后会话的指向变为 entt::null；
- AnomalyDetector：两个同 index、不同 version 的实体，计数互不串。
- 4b 测试：missions_test、player_mission_system_test、player_feature_persistence_test、player_feature_handler_test、player_battle_settlement_test、aoi_system_test、aoi_delivery_test 改为结构体事件后全部保持绿。新增两个用例：ConnectToNodeEvent 携带完整 64 位句柄（index 被复用后 version 仍正确）；ConditionEvent 的多个 conditionIds 能正确推进任务。
- 4d 测试（aoi_delivery_test / skill_test）：
- ActorCreateS2C.entity 等于 actor_net_id::Of(entrant)，非 0，且不等于 to_integral；
- 实体销毁后 Resolve(旧号) 返回 entt::null；
- 销毁再创建同一个 index，新号不同，旧号永不复用；
- ReleaseSkill 传入未知号、别的场景 actor 的号、冻结玩家的号都被拒绝；
- SkillUsedS2C.target_entity 等于目标的 net id；
- registry.clear() 之后索引为空。
- 机械门禁（Codex 执行 git grep）：
- `entt::to_entity(` 在 cpp 和 tools/proto_generator 里结果为零；
- `DispatchProtoEvent` 在 cpp、tools、go 里结果为零（注释也要改掉）；
- 已删除的 11 个 common/event 的 .pb.h 没有任何 include；
- `uint32_t .* = .*GetEntityId` 结果为零。
- C++ 编译（Codex 执行，MSBuild 必须串行 /m:1，否则会出现假的 C1041 / LNK1104）：game.sln 中依次编 proto、core、thread_context、modules、services/scene、nodes/scene、nodes/gate、nodes/battle，然后编 kafka_command_test、missions、bag_test、aoi_test、aoi_delivery_test、routing_identity_test、skill_test 并运行。Linux 走 tools/scripts/build_linux.sh。失败时保留首个错误及其上下文。
- Go（Codex 执行）：在 go 目录下对 match、login、scene_manager、player_locator、friend、guild、shared 跑 `go build ./...` 和 `go test ./...`。本批不改 Go 生产者，只需确认重新生成的 pb、event_id.go 不破坏编译。
- Java（按 Java 仓库 AGENTS.md 的口径，Codex 执行）：用 tools/ContractSync.java 重新同步后执行 `./mvnw clean install`。4a 需确认新的 option proto 能被 Java protoc 编译；4b 需确认删除 11 个 proto 后 Java 没有引用残留（只读核对过：Java 只用到 BattleConfirmedEvent 和 BattleSettlementEvent）。
- 冒烟（Codex 执行）：robot 的 battle_smoke（battle→scene 的 SceneCommand）、team_smoke（match→scene）、登录、踢人、推送（GateCommand 9 种）各跑一遍，日志中不得出现 "not allowed" 级别的 ERROR。

## 7. 并行拆分、风险与拍板项

**并行拆分**：四个子批之间的依赖与可并行关系如下。

现在可以并行开工：
- 轨道 A（4a-1 至 4a-5，白名单）：只动生成器、模板、option proto、三个被标注的 proto、node_kafka_command_handler.h、node_entry.h、scene/gate 的 main.cpp、scene_kafka_command_router.cpp、kafka_command_test，以及少量注释。
- 轨道 C（4c-1 至 4c-4，日志与 gate 会话类型化）：只动 actor_log_id.h、mission.cpp 里的日志行、player_lifecycle_handler.cpp、player_scene.cpp:251、client_message_processor.cpp、anomaly_detector.h/.cpp、session_info_comp.h、scene_route_helper.h、scene_entry_dispatch.cpp、gate_service_handler.cpp、client_request_lane.h/.cpp、routing_identity_test。
- A 与 C 的文件集合互不相交，可以同时开工。注意 gate/main.cpp 只归 A，client_message_processor.cpp 只归 C。

等前置条件满足后再做：
- 轨道 B（4b-1 至 4b-6）：必须等第 1 批合入、A 与 C 都落地之后。原因有三：大量文件与第 1 批重叠；mission.cpp 与 C 重叠；生成器的重新生成要建立在 A 的结果上。
  - 删除 proto、重新生成、搬运 handler、改生产者与消费者，必须在同一次提交里原子完成，否则新旧同名类型会冲突。
  - B 内部可以按文件分给三个实施者，但要合入同一个分支以后再统一编译：B1 负责 engine 与 gate（node_events.h、node.cpp、registration_manager.cpp、client_message_processor.cpp 的 OnNodeRemove）；B2 负责 mission 与 player（mission_events.h、player_events.h、mission.*、player_mission.cpp、player_lifecycle.cpp、player_attribute.cpp、player_battle.cpp、mission 相关测试）；B3 负责 scene、actor 与 combat（scene_events.h、actor_events.h、combat_events.h、aoi、scene_crowd、combat_state、buff_impl、buff、skill、actor_action_state、scene_node_service、scene_handler、aoi 相关测试）。
  - 接线文件 scene_local_events.*、proto 删除与 regen、game_registry 清理，由 B 的负责人收口。
- 轨道 D（4d）：等用户就 decisions_needed 第 1 条拍板，并且 B 合入之后再做。D 与 B 都会动 aoi.cpp、skill.cpp、player_lifecycle.cpp、player_scene.cpp，只能串行。

**风险**：
- 漏掉的生产者会被拒收。已全仓盘点 Go、C++ battle 和 Java，Java 没有生产者；被拒的事件会打 LOG_ERROR 而不是静默丢弃。但如果部署环境里有未纳入仓库的运维工具直接往 scene-cmd/gate-cmd 投事件，这类事件也会被拒。
- player_locator 用写死的 34 当 PlayerLeaseExpiredEvent 的事件号（leasemonitor.go:220）。本批不重排任何活号，所以不受影响；但这是一处脆弱点，建议另行改用生成常量。
- 4b 必须设 PROTOGEN_ALLOW_MASS_EVENT_TOMBSTONE=1，一次永久作废 29 个事件号。提交前必须人工比对 event_id.txt 的 diff，确认活号零变化。
- 删除自定义 entt::to_entity 之后，官方模板对 uint64_t 照样能编译，返回的是 index 部分。漏改的调用点（尤其是日志里的）会静默换语义，必须靠 grep 门禁兜住。
- 生成器不会删除陈旧的生成物。如果残留旧的 common/event/*.pb.h 被 include，同名的旧 proto 类会和新结构体在全局命名空间冲突，报 ODR 或重定义错误。陈旧文件必须删干净，并从 CMakeLists 和 vcxproj 中摘掉。
- MissionsComp 去掉 EventOwner 以后，原来依赖隐式转换成 entt::entity 或 id_type 的地方会编译失败。这是好事，但修改量不可预知。
- MSVC 下前置声明 class 与 struct 不一致会报 C4099（aoi.h、combat_state.h、buff.h、mission_comp.h、mission.h 都有 `class Xxx;` 形式的前置声明），需要统一。
- 4b 与 4d 有大量文件属于第 1 批的独占范围（aoi、skill、buff、actor_action_state、player_attribute、actor_event_handler、player_battle、scene_node_service、scene_handler 的 CreateScene），必须等第 1 批合入后再基于它的结果改。node.cpp:1528-1532 紧挨 F010 的摘除代码，需要与 gRPC→muduo HTTP/2 迁移会话协调；两者都在改 node.cpp。
- 4d 方案 A 依赖 Install 必须先被调用：生产环境在 World 初始化时调用，测试在夹具里调用。漏调时 Assign 返回 0，该 actor 对客户端不可见；这是 fail-closed，能被发现，但会影响测试。
- 4d 方案 A 的 C++ 号为 node_id<<32|seq，同时在线节点之间唯一、本进程内不复用；进程重启或 node_id 回收后，同一 node_id 从 seq=1 重新发号。Java 的 SceneWorld.nextId 雪花号跨重启也唯一。负责人已于 2026-10-16 确认客户端换场景、重连、换节点时清空 actor 表，因此保留现有号源，不加启动代次。
- 按改动文件数计：A 约 26、C 约 14、B 约 55 个手改文件外加约 60 个生成物删除、D 约 15，合计远超 AGENTS §10.2 规定的 30 个文件上限。需要用户明确授权后才能实施。

**需负责人拍板**（未拍板前不要实施相关步骤，其余步骤照常进行）：
- F080 的方案 A 已按第 6 批核心拍板 3 确认（2026-10-16）：负责人确认客户端在换场景、重连、换节点时清空 actor 表，号源维持现状，只补契约文档。三项问题中，是否只把 actor 号当不透明值使用、是否接受方案 B，负责人未单独确认；按方案 A 记录并维持现状。本会话未读取独立客户端仓。
- 改动规模授权。第 4 批合计约 115 个文件，其中 4b 单独约 55 个手改文件外加约 60 个陈旧生成物的删除；还要一次永久作废 29 个事件号，需设 PROTOGEN_ALLOW_MASS_EVENT_TOMBSTONE=1。按 AGENTS §10.2，超过 30 个文件必须经用户明确授权。建议按 4a、4c 先行，4b、4d 依次跟进的顺序分批授权。
- §12 双版本对账需要写 Java 仓库：在 D:/luyuan/xuanming-server-mmo 的 PARITY.md 登记，并更新 docs/reference/mmorpg-client-contract-aoi.md 中关于 C++ 实体号的描述。请确认实施代理是否被授权写 Java 仓库，还是改由 Java 侧会话处理。

**Java 版（AGENTS §12）**：4a（Kafka 白名单）：Java 版不适用。Java 没有接 Kafka 命令入口（Java 仓库 docs/porting/inventory/scene-core.md 的 scene-kafka-command-ingress 一节标为 missing），battle→scene 走 Dubbo SceneBattleService，每个方法本身就是白名单。Java 侧只需要用 ContractSync 重新同步新的 event_ingress_option.proto 和三个被标注的 proto，再编译一遍；PARITY 登记为「不适用 + 契约已同步」。

4b（进程内事件改为结构体）：Java 代码不使用任何进程内事件 proto。只读核对过，Java 只用到 common/event 里的 BattleConfirmedEvent 和 BattleSettlementEvent。ContractSync 会删除 11 个 proto 并更新 event_id.txt 的墓碑，Java 无代码改动，只需重新同步、编译、登记。

4c（日志与 gate 会话类型化）：属于 C++ 内部改动，Java 不适用。

4d（对外 actor 号）：
- Java 现状：ActorCreateS2C / ActorDestroyS2C / ActorMoveS2C / SkillUsedS2C 的 entity 是 ScenePlayer.entity，取自 SceneWorld.nextId()（节点雪花号，非 0，一次进场内稳定，同节点换场景不变）；66 号消息 ActorBaseAttributesS2C 的 entity_id 是 playerId（ScenePlayer.java:303）；技能目标通过 playersByEntity 解析。
- 方案 A：C++ 追平 Java 的语义，Java 无代码改动，只需修改契约文档里关于 C++ 的那一句，并在 PARITY 登记。
- 方案 B：两版必须同批修改，Java 侧 ScenePlayer.entity 改为 playerId，技能目标改由 playerById 解析，robot 断言同步调整。

## 8. 挑战者意见（实施时必须处理）

结论：**needs_changes**

- **[medium]** 4b-2 的机械门禁写成 `git grep -n "proto/common/event/(actor_combat_state|actor|...)_event.pb.h"`，没有加 -E。git grep 默认用基本正则（BRE），`(` 和 `|` 都按字面匹配，所以这条门禁永远命中 0 行，等于没有检查。4c 那条 `uint32_t .* = .*GetEntityId` 也一样：4c-4 已把 GetEntityId 改名为 GetNodeEntity，改名之后它必然为零，检查不到任何东西。
  - 修法：第一条改成 `git grep -nE 'common/event/(actor_combat_state|actor|buff|combat|mission|node|npc|player|scene|server|skill)_event\.pb\.h'`（去掉 proto/ 前缀，把各种 include 写法都覆盖到）。第二条改成检查真正要消灭的形状，例如 `git grep -nE 'uint32_t\s+sceneNodeId\s*=|entt::entity\{\s*(storedEntityInt|sceneEntityId|effectiveEntityInt)'`，或者直接 grep `kInvalidEntityId|SetEntityId|GetEntityId` 必须为零。
- **[medium]** 并行划分只考虑了第 1 批，漏掉了正在进行的 muduo/gRPC 迁移会话。规格称轨道 A 和轨道 C「互不相交，现在可以并行开工」，但工作区里下列文件都有该会话未提交的改动：gate/main.cpp（GATE_ROUTER_TRANSPORT、SetHandshakeSucceededHook）、client_message_processor.cpp:189-227（SendRawViaRouter 是 grpc-muduo-transport.md §8 的接缝，muduo_h2 分支也在这里）、client_request_lane.{h,cpp}（OnRouterNodeRemoved 改为 queueInLoop）、gate_service_handler.cpp（改了 145 行）、registration_manager.cpp（改了 229 行）。4a-4 要改 gate/main.cpp，4c-4 要改 RouterSendFn 的返回类型和这几个文件，会与该会话直接冲突。另外 player_lifecycle.cpp 的 InitPlayerFromAllData 和 SavePlayerToRedisImpl 正被第 1 批修改，就在 4b-4、4d-2 要动的位置旁边，但规格的第 1 批重叠清单里没有列这个文件。规格引用的行号也既对不上 HEAD 也对不上工作区：node.cpp 写 :999/:1528，HEAD 是 971/1501，工作区是 1001/1553；registration_manager.cpp 写 :204，工作区是 :160。
  - 修法：在 parallel_split 里补上与迁移会话的协调：4a-4 中 gate/main.cpp 的改动和 4c-4 整步，要等迁移第 1 批合入后再基于它的结果改，或者与该会话约定由谁统一修改 RouterSendFn、SendRawViaRouter、OnRouterNodeRemoved 的签名。第 1 批重叠清单补上 player_lifecycle.cpp。所有步骤按符号名定位，不按行号定位。
- **[medium]** 4d 方案 A 声称「语义与 Java 版现状一致：非 0 雪花号」，这与实现不符。规格的实现是每个进程从 1 开始单调计数，所有 scene 节点发出的号都会重叠。Java 实际用的是节点雪花号（SceneNode.java:351 传入 snowflake::nextId，SceneWorld.java:2988），跨节点也唯一。仓库里其实已有现成的做法：scene 的 tlsIdGeneratorManager 用 TransientNode32BitCompositeIdGenerator（node_id<<32|seq，见 id_generator.h、node_id_generator.h）给 buff_id、skill_id 发临时号，node_id 在 World::OnRoutingNodeIdAllocated 里设置。规格另起一个计数器，与 §11.2「优先沿用仓库现有模式」不符，跨节点换图时的碰撞风险也比必要的大。
  - 修法：在 ThreadLocalIdGeneratorManager 里加一个 actorNetIdGenerator（同样是 TransientNode32BitCompositeIdGenerator），由 SetNodeId 一并设置 node_id；Assign 用它发号。ctx 里的反查表和 on_destroy 维护保持不变。node_id 还未分配（为 0）时 Assign 按 fail-closed 返回 0。同时把 decisions_needed 里「是否需要全局唯一」一项改为「已做到同时在线的节点之间唯一，node_id 回收后可能重复」，并更正与 Java 的对比描述。
- **[medium]** 已定稿的 player_inbox 设计（docs/design/player-inbox-service.md，v1.0，D1–D4 已拍板，下一步就落码）在 §5.1（:582）计划新增 proto/common/event/player_inbox_event.proto，其中的 PlayerInboxNotifyEvent 经 SceneCommand 投到 scene，写法照抄 team_event.proto:1-15。4a 落地后，这个新事件如果没有标注会被 kNotAllowed 拒收，在线提醒会全部失效（只留 ERROR 日志）。规格的生产者盘点和文档清单都没有提到它。
  - 修法：4a-5 的文档改动加上 player-inbox-service.md §5.1：示例 proto 要带 `option (OptionEventIngress) = EVENT_INGRESS_SCENE_COMMAND;`，并加入它的协调项清单。控制面 topic 文档里新增的那条规则也应点名这个后续事件。
- **[low]** 4b 漏了两处必然编译失败的地方。(1) cpp/tests/cross_zone_test/cross_zone_test.cpp:4583 include 了 proto/common/event/player_event.pb.h，并在 :4790-4803 订阅 PlayerLoginEvent，但不在 4b-6 的测试清单里。(2) cpp/nodes/gate/handler/rpc/client_message_processor.h:14 写的是 `class OnNodeRemoveEvent;`，事件改成 struct 后 MSVC 会报 C4099；gate.vcxproj 开了 TreatWarningAsError，所以这里是编译错误。4b-4 只列了 .cpp，风险条目列出的前置声明文件里也没有它。aoi.h:5 的 `class AfterEnterScene;` 指向的是要删除的类型，会成为残留声明。
  - 修法：4b-6 加入 cross_zone_test.cpp，改为 include player_events.h 并按结构体使用。4b-4 加入 client_message_processor.h，改为 `struct OnNodeRemoveEvent;` 或直接 include node_events.h。同一步删掉 aoi.h 里 `class AfterEnterScene;` 这行前置声明。
- **[low]** 清理陈旧产物的清单不完整。generated/proto/db/proto/common/event、generated/proto/login/proto/common/event、generated/robot/proto/common/event 下的 *.proto 都受 git 跟踪，共 40 个文件。它们由 CopyProtoToGenDir 经 CopyLocalDir 只复制、不删除（prototools/builder.go:47-75），删掉 11 个源 proto 后旧副本会留在仓库里。只有 _unified 目录会先 RemoveAll（generator/go/unified.go），可以自动清干净。
  - 修法：在 4b-2 的「需要手动删除的陈旧生成物」里补上这三个目录下对应的 11 个 .proto，或者让生成器在复制前清掉目标目录（这属于改生成器）。
- **[low]** 4a 有更简单的写法。哪个入口由目录决定已经足够：logic_event 只能走 SCENE，gate_event.proto 只能走 GATE，规格的自检本来就这样约束。既然如此，enum 带来的信息完全冗余，还要为此新写 ReadEnumExtension，再在 Go 里镜像一份枚举常量并用 checkEventIngressEnum 对账，这本身又制造了第二份真相。另外，单测只验证生成的 constexpr 头文件，而真正放行的是另外生成的 switch。两者若被生成器生成得不一致，单测发现不了。
  - 修法：改用一个 bool option（例如 `optional bool OptionKafkaIngress = 700001;`），直接复用已有并已测过的 ReadBoolExtension，入口由所在目录推导；删掉 ReadEnumExtension 和 checkEventIngressEnum。生成的 Dispatch*CommandEvent 先调用白名单头文件里的 IsXxxCommandEvent，不通过就返回 kNotAllowed，这样单测覆盖的就是实际生效的那道门。
- **[low]** 4c-3 的 AnomalyDetector 测试无法区分修复前后。EntityHash 截成 32 位只影响分桶，std::unordered_map 判断键相等时仍比较完整的 entt::entity。所以「同 index、不同 version 的实体计数互不串」这个用例，修复前就能通过。F030 本质上只是哈希质量问题，真正的问题是 F064（旁路表）。
  - 修法：去掉这个用例，或改成测试 `std::hash<entt::entity>` 是否把 version 位也纳入哈希；交付说明里写明 F030 不影响正确性，修复只为去掉截断写法。
- **[low]** 4b 保留并专门测试的几个事件，实际上已经没有任何监听者。ConnectToNodeEvent、OnServerStart、BeKillEvent、InterruptCurrentStateEvent 都有生产者，但所有订阅处的 handler 都是空的：scene 的 NodeEventHandler、ServerEventHandler、CombatEventHandler，以及 ActorEventHandler 的 Interrupt 分支；gate 也没有订阅它们。4b-3 写明「空 handler 不搬」，所以 4b 之后 trigger 这些事件不会有任何效果。4b-6 新增的「ConnectToNodeEvent 保留 version」用例测的是一条死路径。
  - 修法：按规格自己的 YAGNI 口径，把这几个零监听者的事件连同 trigger 点一起删掉（node.cpp 的 OnServerStart、registration_manager 的 ConnectToNodeEvent、skill.cpp 的 BeKillEvent、actor_action_state 的 InterruptCurrentStateEvent）。若决定保留，至少去掉针对 ConnectToNodeEvent 的专项测试，F015/F097 记为「随事件删除」。
- **[low]** 4b-3 要求把 handler 正文「原样搬」，但搬过去的日志不符合 4c 的规则，而且改成结构体字段后编译不过。player_event_handler.cpp 的 :36/:49/:73/:87 写的是 `"Player Not Found :" << event.actor_entity()`，:92 写的是 `"PlayerLoginEvent: player=" << ...`。字段改成 entt::entity 之后，LogStream 没有 enum class 的 operator<<，这几行无法编译。风险描述也有一处不准：官方 entt::to_entity(uint64_t) 返回的是整数类型 entity_type，不是 entt::entity，所以传给 valid/try_get 的调用会直接编译失败。只有 `auto x = entt::to_entity(...)` 且 x 只用于日志或整数运算时，才会静默换成「取 index」的语义。grep 门禁仍然必要。
  - 修法：4b-3 写明：搬运时，日志一律改用 4c-1 的 ActorLog（或输出 `entity=` + 完整 64 位）。风险条目改成上面的准确说法。

**遗漏的调用点**：
- cpp/tests/cross_zone_test/cross_zone_test.cpp:4583（include proto/common/event/player_event.pb.h）与 :4790-4803（订阅 PlayerLoginEvent）
- cpp/nodes/gate/handler/rpc/client_message_processor.h:14 的 `class OnNodeRemoveEvent;`（gate 开了 /WX，会报 C4099）
- cpp/libs/services/scene/spatial/system/aoi.h:5 的 `class AfterEnterScene;`（指向已删除的类型）
- generated/proto/db/proto/common/event/*.proto、generated/proto/login/proto/common/event/*.proto、generated/robot/proto/common/event/*.proto（受 git 跟踪，CopyLocalDir 不会清理）
- docs/design/player-inbox-service.md:582（计划新增的 SceneCommand 事件 PlayerInboxNotifyEvent 必须带 SCENE 标注）
- cpp/libs/services/scene/combat/skill/system/skill.cpp:679、:872（ResolveSkillTarget(caster, skillContext->target()) 传入的是本进程实体整数，应改为 entt::entity{}，不能走 actor_net_id::Resolve）
- cpp/nodes/scene/handler/event/player_event_handler.cpp:36/:49/:73/:87/:92（搬运时日志需改用 ActorLog）
- cpp/nodes/gate/handler/rpc/client_message_processor.cpp:189-227 SendRawViaRouter（muduo 迁移会话正在修改的接缝；RouterSendFn 改返回类型会落在这里）
- cpp/libs/engine/core/node/system/registration/registration_manager.cpp（工作区有 muduo RPC 会话未提交的 229 行改动；ConnectToNodeEvent 在 :160）
- cpp/libs/services/scene/player/system/player_lifecycle.cpp:3805-3845（第 1 批正在修改 InitPlayerFromAllData；4b-4 和 4d-2 都要改这里）
- cpp/libs/services/scene/core/system/id_generator.h（4d 应在这里复用 TransientNode32BitCompositeIdGenerator，而不是另写计数器）
- docs/design/team-system.md:1603、:1932 和 docs/design/scene-enter-session-fence.md:126（文档仍引用 DispatchProtoEvent）

## 9. 回执

按 [README.md](README.md) §6 的格式交回。另外逐条说明 §8 每个 high/medium 问题是怎么处理的。

## 9. 2026-10-15 Codex 源码回执

4a–4d源码已实施，尚未统一生成。采用§8建议的 bool OptionKafkaIngress，目录决定scene/gate，避免enum两份真相；实际显式标注3+9，默认拒绝，载荷与gate空载荷回退用同一生成白名单。删除自动全量dispatch后备，hook必须指定派发函数；零case也覆盖旧输出。删除11源proto及22旧handler，模块C++结构体保留完整typed句柄，scene手写接线；源proto删除对应事件号须未来转29个墓碑，活号不改。

gate会话与lane改entt::entity，entt::null缺省，补同index不同version重绑回归；日志通过NodeInfo取业务node_id，玩家日志ActorLog读PlayerGuidComp。ActorNetIds选默认兼容A：node32+seq32、本进程不复用、耗尽拒绝、registry私有反查随组件移除清理；创建Assign失败沿既有拒入补偿。对外Of、回传Resolve，保留同场景/冻结校验；66号属性entity_id仍玩家GUID。内部SkillContext/Buff的uint64本地句柄与0哨兵兼容保留。

§8生成清理挑战收口在生成器源码：检查三份构建清单完整/可转换后才删旧产物，清单自身也提供候选以支持失败恢复；旧Go/staging副本同步清，生成阶段在输入完整性/墓碑门禁后。未手改生成树，当前cpp/generated和若干生成handler/attribute仍是旧接口；等待用户恢复生成后才能整编。新增白名单/入口校验、生成清理幂等/失败、ActorNetId生命周期和拒入测试源码。静态diff/XML及交叉读源通过，不替代构建；最新全部生成器与C++测试未运行。源码选型/待执行命令/Java边界见README §7。

### 2026-10-16 第6批更正（保留以上历史回执）

“补同index不同version重绑回归”应为“类型契约固定用例，F033 无行为缺陷，无修复前红”。保留用例守护将来把节点句柄收窄为 index/uint32 的回退，与 §8 对 AnomalyDetector 用例的定性一致。

<!-- 只读调查结果(2026-10-15,基线 HEAD 526780b4e 加当时工作区的未提交改动;第 3 批前半已落地,行号可能已漂移)。原样保留作交接附录,见同目录 README.md。 -->

# AREA call-sites
## summary
【调查范围】只读核对 HEAD 526780b4e 加主工作区里未提交的改动。cpp/libs、cpp/nodes 下手写代码对生成 gRPC 客户端的出站调用一共 9 处直接调用，另有 1 处通用 sender 和 1 处 muduo 手写路径。全部用全局处理器，没有一处生产代码用 GrpcDone 新重载；GrpcDone 只在 cpp/tests/rpc_controller_test/grpc_client_lifecycle_test.cpp 里用到。

【调用点总表】（全部为单次请求、单次应答的 unary 调用，etcd 除外）
1. gate → client_rpc_router.Forward
   - 官方路径：cpp/nodes/gate/handler/rpc/client_message_processor.cpp:236（SendRawViaRouter）。
   - muduo 路径：同文件 :216，调 gate_router_muduo::SendForward，实现在 cpp/nodes/gate/handler/rpc/router_muduo_forward.cpp:73-120，已经是每次调用回调。
2. gate → login.Disconnect（直连模式）：client_message_processor.cpp:568。
3. gate → login / match / chat / friend / guild / team / jubaozhai（直连模式的通用转发）：client_message_processor.cpp:925 调 rpcHandlerMeta.sender，它绑定的是生成客户端的 Message& 重载（cpp/generated/rpc/service_metadata/rpc_event_registry.cpp:826-830 为例）。
4. scene → scene_manager.EnterScene：cpp/libs/services/scene/player/system/player_lifecycle.cpp:480 是唯一出口 SendCorrelatedEnterScene。它有 3 个入口：
   - RequestSceneChange，第 4892 行；它又被 player_scene_handler.cpp:162、player_team.cpp:505、scene_manager_response_handler.cpp:174 调用；
   - SendEmergencyRelocateEnterScene，第 3869 行；
   - RequestTravelEnterScene，第 5440 行。
5. scene → scene_manager.CreateScene：player_scene.cpp:382。
6. scene → data_service.AllocateIdSegment：cpp/nodes/scene/id_segment_bootstrap.cpp:160。
7. scene → player_inbox.FetchItems：player_inbox_system.cpp:89。
8. scene → player_inbox.AckItems：player_inbox_system.cpp:101。
9. 各节点 → etcd：
   - 官方路径：cpp/libs/engine/core/node/system/etcd/grpc_etcd_transport.cpp:75-110，共 8 个 Send，含两条流 Watch 与 LeaseKeepAlive；
   - muduo 路径：muduo_etcd_transport.cpp:194（unary）和 :318（流）。
10. battle：没有业务出站 gRPC。白名单为空（cpp/nodes/battle/main.cpp:262），cpp/nodes/battle 下找不到任何 Send 调用。

【全局处理器安装】
- gate：
  - main.cpp:359 SetIfEmptyHandler；
  - main.cpp:371 SetIfEmptyFailedHandler。
  - 两者都铺到全部 19 个 gRPC 文件（etcd 也在内），见 cpp/generated/grpc_client/grpc_init_client.cpp:216-256 和 :300-340。
- scene：
  - InitSceneManagerReply（main.cpp:139）：scene_manager_response_handler.cpp:30、72、84、189；
  - InitPlayerInboxReply（main.cpp:142）：player_inbox_response_handler.cpp:13、17、24、28；
  - InitDataServiceReply：id_segment_bootstrap.cpp:264-272。
- core：
  - grpc_etcd_transport.cpp:53-70 安装 etcd 应答处理器，:118 在关机时清空；
  - etcd_service.cpp:743 也会清空 etcd 处理器。
- rpc_replies 目录：
  - 只有 scene 下两个 gRPC 适配文件（scene_manager_response_handler、player_inbox_response_handler）。
  - 没有 On<Domain><Method>Reply 形式的 gRPC 适配。
  - gate 与 battle 的 rpc_replies/register_handler.cpp 都只有一个空的 InitServiceHandler。

【第 3 批迁移结论】
- 生成器可以统一改：每个方法的 Start<Svc><Method>Call 是唯一发送入口（tools/proto_generator/protogen/internal/template/grpc_async_client.cpp.tmpl:186-211），可以在这里把传输换成 MuduoGrpcChannelPtr 加 StartUnaryCall。已经用 GrpcDone 的调用方不用改。
- 但今天的生产调用点全靠全局处理器，而全局处理器签名里带 grpc::ClientContext 和 grpc::Status（GrpcCallFailure 也带），所以调用点不可能“完全不动”。
- 选点函数与组件 view 都绑在官方组件上，必须与工厂安装同批改，否则 scene 拿不到可用节点：
  - GetSceneManagerEntity（cpp/libs/engine/core/network/node_utils.cpp:246）；
  - HasUsableGrpcClient（cpp/libs/engine/core/network/grpc_client_runtime.h:61）；
  - PickReadyDataServiceNode（id_segment_bootstrap.cpp:91）；
  - PickReadyPlayerInboxNode（player_inbox_system.cpp:63）。
## facts
### GrpcDone 新重载已生成，但生产代码一处都没用
【已生成】20 个生成头文件里每个 unary 方法都有 Send<Svc><Method>(registry, entity, req, GrpcDone<Resp>, const GrpcCallOptions& = {}) 重载，例如 scene_manager_service_grpc_client.h:51。muduo-rpc-channel.md §19.2 也写了“已随用户快照提交并 regen”。

【文档不一致】[历史设计 grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md) §11 开头仍写“未 regen”，与生成物不符。

【使用情况】在 cpp/ 下排除 generated 搜 GrpcDone / GrpcCallOutcome / GrpcCallOptions，只命中 grpc_client_lifecycle_test.cpp:464、495、556。

【唯一的在途迁移】镜像 CreateScene 改用每次调用回调，只存在于 worktree claude/blissful-morse-969d44 的未提交改动里：
- player_scene.cpp:417-423 用 GrpcDone 发送，回调按值捕获 playerId 与 token；
- player_scene.cpp:433 起是 HandleMirrorCreateDone；
- 该 worktree 的 scene_manager_response_handler.cpp:71 起不再安装 CreateScene 的全局处理器。

主工作区 player_scene.cpp:382 仍是三参 Send，仍走全局处理器。
files: D:\luyuan\xuanming-server-mmo-cpp\cpp\generated\grpc_client\scene_manager\scene_manager_service_grpc_client.h, D:\luyuan\xuanming-server-mmo-cpp\cpp\generated\grpc_client\grpc_call_tag.h, D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_lifecycle_test.cpp, D:\luyuan\xuanming-server-mmo-cpp\.claude\worktrees\blissful-morse-969d44\cpp\libs\services\scene\player\system\player_scene.cpp, [历史设计 D:\luyuan\xuanming-server-mmo-cpp\docs\design\grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md), D:\luyuan\xuanming-server-mmo-cpp\docs\design\muduo-rpc-channel.md
### 生成客户端的发送与完成流程（模板）
【发送】所有 unary Send 重载都汇到 Start<Svc><Method>Call（grpc_async_client.cpp.tmpl:186-211），步骤如下：
1. 对每个 metadata 原值调 context.AddMetadata(key, Base64Encode(value))。所有键都编码，不只是 -bin 键，例如 x-idseg-seq 也会编码。
2. 把请求拷进调用对象的 call->request。
3. 设截止时间：set_deadline(system_clock::now() + deadline 或文件默认值)。
4. 用 try_get 取 grpc::CompletionQueue、<Svc>StubPtr、GrpcClientRuntimePtr。
5. 缺任一组件或节点正在关闭：以 UNAVAILABLE "node transport missing or closing" 失败，经 QueueGrpcClientCompletion 延后投递（grpc_client_runtime.h:74-108）。
6. 否则走 PrepareAsync → StartCall → Finish。

【完成】Complete<Svc><Method>（模板 :140-174）：
- 有 done：只交给 done，不再走全局处理器；
- 成功：交 Async…Handler(ctx, reply)；没装处理器就每方法每线程报一次 ERROR；
- 失败：交 Async…FailedHandler(GrpcCallFailure, 请求副本)；没装就只记日志。

【全局处理器的类型】
- 应答处理器签名：std::function<void(const ClientContext&, const Resp&)>；
- GrpcCallFailure 含 const grpc::ClientContext& 和 const grpc::Status&（grpc_call_tag.h:64-80）；
- GrpcCallOutcome 只有 code 和 message，不带响应头 metadata（grpc_call_tag.h:31-36）。
files: D:\luyuan\xuanming-server-mmo-cpp\tools\proto_generator\protogen\internal\template\grpc_async_client.cpp.tmpl, D:\luyuan\xuanming-server-mmo-cpp\cpp\generated\grpc_client\grpc_call_tag.h, D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\network\grpc_client_runtime.h
### [gate] 调用点 G1：ClientRpcRouter.Forward（路由模式）
【官方路径】client_message_processor.cpp:236-241，SendClientRpcRouterForward(registry, routerNode, forwardRequest, {kSessionBinMetaKey}, {sessionDetailsBin})。
- 处理器：全局。
  - 成功：gate main.cpp:359-363 用 GetSessionDetailsByClientContext(ctx) 从响应头回显的 x-session-detail-bin 找会话，交 gate_rpc_reply_bridge::DeliverReply（rpc_reply_bridge.cpp:50-87）。DeliverReply 先让 gate_lane::OnRouterReply 认领，否则原样下发 MessageContent。
  - 失败：main.cpp:371-376 用 failure.FindSentMetadata(kSessionBinMetaKey) 取发出的会话，交 NotifyCallFailure（rpc_reply_bridge.cpp:89-126）：先 lane 认领回带请求 id 的 1003 信封，否则 SendTipToClient(1003)。
- metadata：x-session-detail-bin，值是 SessionDetails 序列化后的原始字节。
- 选点：PickRandomNode(ClientRpcRouterNodeService)（client_message_processor.cpp:101-134）。路由服是全局池，不比对 zone。候选条件是挂了 MuduoGrpcChannelPtr 或 HasUsableGrpcClient 为真，随机挑一个。
- 没有路由服或两种传输都缺：返回 nullopt，lane 回 1003（:191-201、:225-235）。
- deadline：文件默认值，ClientRpcRouter 配置 8000ms。

【调用方】
- 路由 lane：client_request_lane.cpp:137-138 调 gRouterSender，它在 client_message_processor.cpp:260 注入为 &SendRawViaRouter；
- gate 合成的断线通知：SendViaRouter（:246-249、:561），gate_call_seq 留 0，回包不归 lane。

【分叉点】:212-218 实体上挂了 MuduoGrpcChannelPtr 就只走 gate_router_muduo::SendForward（router_muduo_forward.cpp:73-120）：
- 截止时间：steady_clock::now() + grpc_call_deadline::Get(ClientRpcRouter)；
- metadata：值是 Base64Encode(sessionDetailsBin)，与生成客户端同一层编码；
- 成功：按响应头回显的会话 ParseSessionDetailsMetadataValue，再交 DeliverReply；
- 失败或解析失败：按捕获的 sentSessionDetails 交 NotifyCallFailure，解析失败记为 INTERNAL。

【其他分叉点】
- 通道工厂只在路由模式且 GATE_ROUTER_TRANSPORT=muduo_h2 时安装（gate main.cpp:276-299，router_muduo_forward.cpp:51-71）；
- muduo_h2 下关机要等路由通道空闲（main.cpp:321-344，router_muduo_forward.cpp:122-133 的 AllChannelsIdle）。
files: D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\gate\handler\rpc\client_message_processor.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\gate\handler\rpc\router_muduo_forward.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\gate\handler\rpc\rpc_reply_bridge.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\gate\handler\rpc\client_request_lane.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\gate\main.cpp
### [gate] 调用点 G2：Login.Disconnect（直连模式）
client_message_processor.cpp:568，loginpb::SendClientPlayerLoginDisconnect(registry, *loginNode, request, {kSessionBinMetaKey}, SerializeSessionDetails(sessionDetails))。
- 会话代次：sessionDetails 带会话代次（:546），gate_call_seq 留 0。
- 选点：PickRandomNode(LoginNodeService)。login 是按 zone 划分的类型，只挑 node.zone_id 等于本 gate zone 的实例。
- 没有 login 实例：:566 的 if 直接跳过，此处不记日志。
- 处理器：同 G1 的全局 IfEmpty 那一对。失败时会话已在 :579 被 erase，NotifyCallFailure 找不到会话就 return，只留采样 WARN（rpc_reply_bridge.cpp:103-109）。
- 路由模式下改走 SendViaRouter（:548-562）。
files: D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\gate\handler\rpc\client_message_processor.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\network\network_utils.cpp
### [gate] 调用点 G3：直连模式的通用转发（rpcHandlerMeta.sender）
位置：client_message_processor.cpp:925-929，在 HandleGrpcNodeMessage 里。

【怎么发】调 rpcHandlerMeta.sender(registry, *node, *rpcHandlerMeta.requestProto, {kSessionBinMetaKey}, SerializeSessionDetails(...))。
- sender 是 RpcMethodMeta 的 std::function，签名是 (registry, entity, const Message&, metaKeys, metaValues)，不带 done（rpc_event_registry.h:11-26）。
- 它由生成器绑到各方法的 Message& 重载，例如 rpc_event_registry.cpp:826-830 的 loginpb::SendClientPlayerLoginLogin。
- 发出的是 gRpcMethodRegistry 里的进程级共享原型，生成层会拷进 call->request。

【目标】由 IsClientMessageId（rpc_event_registry.cpp:1672 起）决定：
- ClientPlayerLogin*、MatchService*：在直连白名单里，能发出；
- ClientPlayerChat/Friend/Team/Jubaozhai*、GuildService*：不在直连白名单里，registry 为空，PickRandomNode 拿不到节点，:910-915 回带请求 id 的 1003；
- BattleClientPlayer*：在 :1078-1084 统一拒绝；
- Scene* 是 PROTOCOL_TCP，走 HandleTcpNodeMessage，不走这里。

【选点】会话已绑定该节点类型或目标是 Scene 时，用 ResolveSessionTargetNode 取绑定实体；否则用 PickRandomNode。

【处理器】同 G1 的全局 IfEmpty 那一对。非 MessageContent 的回包按 rpc_reply_bridge.cpp:30-46 的“响应类型 → message_id”表包一层再下发。

【前景】gate main.cpp:256 的日志说直连路径会随 D61 删除。
files: D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\gate\handler\rpc\client_message_processor.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\generated\rpc\service_metadata\rpc_event_registry.h, D:\luyuan\xuanming-server-mmo-cpp\cpp\generated\rpc\service_metadata\rpc_event_registry.cpp, D:\luyuan\xuanming-server-mmo-cpp\tools\proto_generator\protogen\internal\generator\cpp\service_register_info.go
### [scene] 调用点 S1：SceneManager.EnterScene（唯一出口）
位置：player_lifecycle.cpp:468-481 的 SendCorrelatedEnterScene，内部调 scene_manager::SendSceneManagerEnterScene(smRegistry, smEntity, req)。
- 不带 metadata。
- 关联号：每条都写入非 0 的 correlation_id（:478）。

【调用方】
- RequestSceneChange（:4873-4894，先在 PlayerSceneChangeInFlightComp 登记，再发），它又被以下三处调用：
  - player_scene_handler.cpp:162（玩家换图）；
  - player_team.cpp:505（队伍跟随，playerRequested=false）；
  - scene_manager_response_handler.cpp:174（镜像建好后自动进场）；
- SendEmergencyRelocateEnterScene（:3813-3874，先登记到改派待确认表，再发）；
- RequestTravelEnterScene（:5361-5446）。第 5441 行的注释依赖“发送不碰 registry、回调不在发送栈内同步执行”。

【选点】GetSceneManagerEntity(playerId)（node_utils.cpp:246-289）：
- 候选：只收 HasUsableGrpcClient 为真且挂了 std::shared_ptr<grpc::Channel> 的实体；
- 分级：按 GetState(true) 分成 READY > IDLE/CONNECTING > TRANSIENT_FAILURE/SHUTDOWN；
- 同级内：splitmix64 打散 playerId 后取模，不比对 zone；
- 注册表为空时返回 null。

【处理器】全局：
- scene_manager_response_handler.cpp:30-67：应答交 DispatchEnterSceneReply，按回显的 correlation_id 认领（player_lifecycle.cpp:4896）；
- :72-76：失败交 DispatchEnterSceneTransportFailure（按请求副本的 correlation_id，player_lifecycle.cpp:5013）。普通换图释放槽位并回 kEnterSceneServerBusy；交接只记日志，交给看门狗裁决。

【deadline】SceneManager 配置 10000ms；在途 TTL 与改派预算都从 grpc_call_deadline::Get 派生。
files: D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\services\scene\player\system\player_lifecycle.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\scene\rpc_replies\scene_manager_response_handler.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\scene\handler\rpc\player\player_scene_handler.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\services\scene\player\system\player_team.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\network\node_utils.cpp
### [scene] 调用点 S2：SceneManager.CreateScene（镜像）
位置：player_scene.cpp:382，SendSceneManagerCreateScene(smRegistry, smEntity, req)，三参版本，不带 metadata。
- 选点：GetSceneManagerEntity(player_id)（:365）。

【处理器】全局：
- scene_manager_response_handler.cpp:84-181：成功时按应答回显的 creator_ids 对每个创建者调 RequestSceneChange；
- :189-203：传输失败时按请求副本的 creator_ids 回 kEnterSceneServerBusy；
- 已知缺口（:187-188 注释）：业务失败的应答不回显 creator_ids，告诉不了创建者。

【分叉】blissful-morse worktree 未提交的版本已改成 GrpcDone：令牌放在 PlayerMirrorCreateInFlightComp 里，成功、业务失败、传输失败三路都交 HandleMirrorCreateDone。
files: D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\services\scene\player\system\player_scene.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\scene\rpc_replies\scene_manager_response_handler.cpp, D:\luyuan\xuanming-server-mmo-cpp\.claude\worktrees\blissful-morse-969d44\cpp\libs\services\scene\player\system\player_scene.cpp
### [scene] 调用点 S3：DataService.AllocateIdSegment（号段）
位置：id_segment_bootstrap.cpp:160-162，SendDataServiceAllocateIdSegment(registry, target, request, {"x-idseg-seq"}, {to_string(seq)})。
- metadata 键：x-idseg-seq（:32），值由生成层做 Base64。

【选点】PickReadyDataServiceNode（:91-108）：
- view<NodeInfo, data_service::DataServiceStubPtr, std::shared_ptr<grpc::Channel>>；
- 只认 GetState(true) == GRPC_CHANNEL_READY；
- 随机挑一个，不比对 zone；
- 挑不到返回 false，由号段客户端退避重试。

【共享路径单飞】同一时刻只放一个在途请求（:110-165），超时后进 10s 迟到窗口。

【处理器】全局，InitDataServiceReply（:264-272）：
- OnResponse(ctx, resp)：先查 ctx.GetServerInitialMetadata() 里有没有 x-idseg-seq 回显做精确归属，否则按 FIFO 归属（:178-226）；
- OnFailure：按 failure.FindSentMetadata(x-idseg-seq) 精确归属，交 GuidSegmentClient::OnTransportFailure（:229-254）。

【超时】fetchTimeoutSec = DataService deadline（4000ms）+ 1s（:286-287）。

【连带】号段是 scene DependencyGate 的启动硬依赖（scene main.cpp:381-386）。

【grpc 头】文件直接 #include <grpcpp/grpcpp.h>（:12）。
files: D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\scene\id_segment_bootstrap.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\scene\main.cpp
### [scene] 调用点 S4：PlayerInbox.FetchItems / AckItems
位置：player_inbox_system.cpp:89-91（DefaultSendFetch）、:101-103（DefaultSendAck），三参 Send，不带 metadata。
- 测试注入：通过函数指针 gSendFetchFn / gSendAckFn 发送，测试可用 SetTransportForTest 替换（:106-109、:1333-1336）。

【选点】PickReadyPlayerInboxNode（:63-80）：
- view<NodeInfo, PlayerInboxStubPtr, shared_ptr<grpc::Channel>>；
- 只认 GRPC_CHANNEL_READY；
- 随机挑，不比对 zone。

【处理器】全局（player_inbox_response_handler.cpp:11-34）：
- 应答交 OnFetchReply / OnAckReply，按回显的 player_id + domain + correlation_id 认领；
- 失败交 OnFetchFailed / OnAckFailed(request, uint32 code, message)，这一层签名已经与传输无关；
- UNIMPLEMENTED 单独处理：长退避、不计故障（:1166、:1305）。常量 kGrpcUnimplemented 取自 grpc::StatusCode（:44）。

【deadline】PlayerInbox 6000ms；在途兜底 = deadline + 5s（:45-47）。

【依赖性质】弱依赖：发现不到节点只退避，不阻断进场。
files: D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\services\scene\player\system\player_inbox_system.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\services\scene\player\system\player_inbox_system.h, D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\scene\rpc_replies\player_inbox_response_handler.cpp
### [各节点] etcd：官方 / muduo 两条路径的分叉点
【怎么选】三个节点的 main 都在 Node 构造前调 etcd_transport::InstallFromEnv()：gate main.cpp:234、scene main.cpp:89、battle main.cpp:257。它读 NODE_ETCD_TRANSPORT（muduo_etcd_transport.cpp:504-519）：
- 不设或设 grpc：GrpcEtcdTransport，这是默认；
- muduo_h2：通过 SetFactory 换成 muduo 传输。

【官方路径】GrpcEtcdTransport：
- 用 GrpcChannelCache 建通道，再 InitGrpcNode 到 EtcdNodeService 的全局实体上（grpc_etcd_transport.cpp:29-41）；
- 全局应答处理器在 :53-70，8 个 Send 在 :75-110，Shutdown 时 SetEtcdHandler 清空（:118）。

【muduo 路径】自建一条 muduo_grpc::Channel（muduo_etcd_transport.cpp:156-175），不经 node_connector，不挂到 registry：
- 周期 keepalive；
- unary 用 StartUnaryCall 加按次闭包（:177-216）；
- 流用 StartStream（:318）。

【5ms CQ 轮询】两种 etcd 传输下都在跑（etcd_service.cpp:34-39），它驱动的是进程内全部官方 CQ。

【gate 的全局 IfEmpty 处理器也会盖到 etcd】grpc_init_client.cpp:228 和 :312 把它铺到 etcd 文件：
- Revoke / TimeToLive / Leases 这几个方法没装专用处理器，应答会落进 gate 的成功桥；
- etcd 的 unary 失败会落进 NotifyCallFailure；因为找不到会话 metadata，只记一行 ERROR。
files: D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\node\system\etcd\grpc_etcd_transport.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\node\system\etcd\muduo_etcd_transport.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\node\system\etcd\etcd_service.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\generated\grpc_client\grpc_init_client.cpp
### [core] 建连 / 选点 / 关机 / 摘除里绑着官方 gRPC 的接缝
【建连】node_connector.cpp：
- :73-92：registry 上下文里有 MuduoGrpcChannelFactory，就只挂 MuduoGrpcChannelPtr 和 NodeInfo；
- :94-99：否则用 GrpcChannelCache 建通道，再 InitGrpcNode，挂 GrpcClientRuntimePtr、CompletionQueue、各 StubPtr（grpc_init_client.cpp:576-637）。
- 今天只有 gate 路由服的 registry 装了工厂。

【可用性判定】
- HasUsableGrpcClient（grpc_client_runtime.h:61-70）要求 runtime 存在且未关闭、并挂着 CompletionQueue；
- scene_manager_selector::Candidate.channelState 的类型是 std::optional<grpc_connectivity_state>（node_utils.h:33-39）。

【关机阶段 0】node.cpp:1215-1229：
- 对挂 CQ 的实体调 BeginShutdownGrpcNode，用 TryCancel 让在途调用以 CANCELLED 结束；
- 清掉所有 MuduoGrpcChannelPtr，~Channel 以 UNAVAILABLE 投递在途失败；
- 之后 DrainGrpcClientsInLoop 每 5ms 轮询排空（:1232-1280）。

【节点摘除】node.cpp:1601 先调 BeginShutdownGrpcNode；CompleteNodeRemoval（:1613-1637）在 PollGrpcNodeShutdown 排空之前每 5ms 重试，排空后才 DestroyEntity。

【deadline 生效】node.cpp:518 调 grpc_call_deadline::Apply，再在 :522 InitEtcdService。

【官方通道的环境变量】grpc_channel_cache.h:12-69：
- GRPC_MAX_THREADS = 2
- GRPC_CLIENT_CHANNEL_BACKUP_POLL_INTERVAL_MS = 5000
- GRPC_KEEPALIVE_TIME_MS = 30000
- GRPC_KEEPALIVE_TIMEOUT_MS = 10000
- GRPC_KEEPALIVE_PERMIT_WITHOUT_CALLS = 1

【其他 grpc 类型依赖】session.cpp:13-18 的 GetSessionDetailsByClientContext 依赖 grpc::ClientContext。
files: D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\node\system\node\node_connector.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\node\system\node\node.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\network\grpc_client_runtime.h, D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\network\node_utils.h, D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\node\system\grpc_channel_cache.h, D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\node\system\node\muduo_grpc_channel_slot.h, D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\session\system\session.cpp
### 各节点 main 的 CanConnectNodeTypeList 与 gRPC 相关配置
【gate】main.cpp:229-231
- 路由模式（GATE_CLIENT_RPC_ROUTER=1/true/on，gate_router_mode.h:29）：{SceneNodeService, ClientRpcRouterNodeService}；
- 直连模式：{SceneNodeService, LoginNodeService, SceneManagerNodeService, MatchNodeService}。代码中找不到 gate 对 SceneManager 的出站调用点，也没有指向它的客户端消息号。
- GATE_ROUTER_TRANSPORT（gate_router_transport.h:25）取 grpc（默认）或 muduo_h2，只在路由模式下生效（main.cpp:283-292）。
- 依赖门（:432-434）：路由模式等 {ClientRpcRouter, Scene}，直连模式等 {Login, Scene}。
- gate 没有 gRPC server。

【scene】main.cpp:102-104
- {SceneManagerNodeService, DataServiceNodeService, PlayerInboxNodeService}；
- 有入站 gRPC：RegisterGrpcService(SceneNodeGrpcImpl)（:120）；
- 依赖门（:381-386）：等 SceneManager 和“id segments ready”。

【battle】main.cpp:262
- 白名单为空 {}；
- 有入站 gRPC：BattleNodeImpl（:268-269）。

【deadline 配置】base_deploy_config.yaml:238-247 的 GrpcClient.CallDeadlineMs（毫秒）：
- SceneManager 10000
- DataService 4000
- ClientRpcRouter 8000
- Match 7000
- Login 12000
- Battle 5000
- Etcd 5000
- PlayerInbox 6000

【发现前缀】base_deploy_config.yaml:43-50 的 service_discovery_prefixes。
files: D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\gate\main.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\scene\main.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\battle\main.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\gate\gate_router_mode.h, D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\gate\gate_router_transport.h, D:\luyuan\xuanming-server-mmo-cpp\bin\etc\base_deploy_config.yaml
### muduo_grpc::Channel 的接口与线上约束（迁移要对齐的部分）
【调用】StartUnaryCall(UnaryCallRequest{method, message, metadata, steady_clock deadline}, cb(UnaryCallResult{status, initialMetadata, trailingMetadata, response}))（channel.h:89-105、:184）。

【状态】GetState(bool) 返回 ConnectivityState，五态与 gRPC 相同（:47-54、:191）；outstandingCalls()（:194）。

【行为】
- 回调恰好一次、不同步执行；通道析构时在途调用以 UNAVAILABLE 结束（:5-10）；
- 状态码数值与 grpc::StatusCode 一致（grpc-muduo-transport.md §4.3）。

【线上约束】
- -bin 键由传输层再加一层 base64，应用层值仍需调用方先 Base64Encode（§4.2）；
- 非 -bin 值必须是可打印 ASCII（§4.2）；
- :path 格式为 /<proto package>.<Service>/<Method>，没有 package 时为 /<Service>/<Method>（§4.1）。几个 package 名：loginpb、scene_manager、data_service、player_inbox；battle_node 没有 package。官方生成的 .grpc.pb.cc 里有现成的路径表，例如 scene_manager_service.grpc.pb.cc:26-28。
- 对端地址必须是 IPv4 字面量（§8）。
files: D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\muduo_grpc\channel.h, D:\luyuan\xuanming-server-mmo-cpp\docs\design\grpc-muduo-transport.md, D:\luyuan\xuanming-server-mmo-cpp\cpp\generated\proto\scene_manager\scene_manager_service.grpc.pb.cc
### 三份 muduo 树都缺“连接失败”回调（第 3 批前置条件）
以下三处都只有一行注释 // FIXME setConnectFailedCallback，没有实现：
- third_party/muduo/muduo/net/TcpClient.cc:68
- third_party/muduo-linux/muduo/net/TcpClient.cc:68
- cpp/libs/engine/muduo_windows/src/muduo/net/TcpClient.cc:68

后果（设计 §5）：被拒绝的连接会一直停在 CONNECTING，直到 connect_timeout（20s）。
files: D:\luyuan\xuanming-server-mmo-cpp\third_party\muduo\muduo\net\TcpClient.cc, D:\luyuan\xuanming-server-mmo-cpp\third_party\muduo-linux\muduo\net\TcpClient.cc, D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\muduo_windows\src\muduo\net\TcpClient.cc
### 依赖官方 gRPC 内部组件的测试
- grpc_client_lifecycle_test.cpp：:108 调 InitGrpcNode，覆盖 CQ 排空、Watch 流，以及 :464 起的 PerCallDone 三条用例。
- cross_zone_test.cpp：:1856-1857 挂 shared_ptr<grpc::Channel> 再 InitGrpcNode。
- scene_manager_selector_test.cpp：:76-164 直接用 GRPC_CHANNEL_* 枚举。
files: D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\rpc_controller_test\grpc_client_lifecycle_test.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\cross_zone_test\cross_zone_test.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\routing_identity_test\scene_manager_selector_test.cpp
### 迁移到“每次调用回调 + muduo_grpc::Channel”：各调用点要改什么
【生成器统一改，调用点不动】
- 改 Start<Svc><Method>Call 和 Init* 模板即可。
- 前提是该调用点已经用 GrpcDone，并且不需要响应头 metadata。今天主工作区没有这样的调用点。

【G1 路由 Forward】
- muduo 版已经是手写的每次调用回调（router_muduo_forward.cpp）。
- 成功分支要用“响应头回显的 x-session-detail-bin”，而 GrpcCallOutcome 不带 initial metadata。所以改用生成的 GrpcDone 时，要么扩展 GrpcCallOutcome，要么改成用捕获的“发出的会话”（推测：路由服按原值回写时两者相同，但这会改变“按回显认领”的语义）。
- 建议保留 SendForward，删除 :225-241 的官方分支（推测）。

【G2 Login.Disconnect】改成 GrpcDone，回调里只记采样日志（会话已经 erase，无处可回）。

【G3 通用 sender】
- RpcMethodMeta.sender 签名没有 done，要么由 service_register_info.go 生成带 done 的 sender，要么继续靠全局 IfEmpty 处理器。
- 直连路径计划随 D61 删除，可能不值得迁。

【S1 EnterScene】只改唯一出口 SendCorrelatedEnterScene 一处：
- 回调按值捕获请求；
- 成功：DispatchEnterSceneReply(*reply)，并搬上 scene_manager_response_handler.cpp:40-66 的日志；
- 失败：DispatchEnterSceneTransportFailure(请求副本, outcome)；
- 删除 :30-76 的两个全局安装。

【S2 CreateScene】合并 blissful-morse 的改动即可。

【S3 号段】
- 把 seq 和 kind 捕获进 done；
- OnResponse 不再需要 ctx.GetServerInitialMetadata()，metadata 仍可放在 GrpcCallOptions.metadata 里；
- PickReadyDataServiceNode 改成看通道组件与 kReady；
- 删除 :264-272 的全局安装。

【S4 收件箱】
- DefaultSendFetch / DefaultSendAck 改成 GrpcDone，回调里调现有的 OnFetchReply/OnFetchFailed(request, code, message)、OnAckReply/OnAckFailed；
- kGrpcUnimplemented 改成传输无关的常量 12；
- PickReadyPlayerInboxNode 同步改；
- 删除 player_inbox_response_handler.cpp。

【每个调用点都要同步改的选点】GetSceneManagerEntity、HasUsableGrpcClient、gate PickRandomNode 的候选条件。
files: D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\services\scene\player\system\player_lifecycle.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\scene\id_segment_bootstrap.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\services\scene\player\system\player_inbox_system.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\gate\handler\rpc\client_message_processor.cpp, D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\gate\handler\rpc\router_muduo_forward.cpp
## changeSurface
- [template] D:\luyuan\xuanming-server-mmo-cpp\tools\proto_generator\protogen\internal\template\grpc_async_client.cpp.tmpl: Start<Svc><Method>Call 要改走 MuduoGrpcChannelPtr 加 StartUnaryCall，需要的东西：
- 方法路径 :path；
- 保留 Base64Encode 那一层编码；
- steady_clock 截止时间；
- 响应解析失败记为 INTERNAL；
- 缺通道时异步返回 UNAVAILABLE。
另外 Init<File>GrpcNode 和流式分支也要处理。
- [template] D:\luyuan\xuanming-server-mmo-cpp\tools\proto_generator\protogen\internal\template\grpc_async_client.h.tmpl: 调用对象里的 ClientContext、Status、ClientAsyncResponseReader、StubPtr，以及全局处理器签名里的 grpc 类型，都要换成与传输无关的类型。
- [template] D:\luyuan\xuanming-server-mmo-cpp\tools\proto_generator\protogen\internal\template\grpc_init_total.cpp.tmpl: 它生成 InitGrpcNode、BeginShutdownGrpcNode、PollGrpcNodeShutdown、HandleCompletedQueueMessage 这些 CQ 驱动的函数，以及 SetIfEmpty* 系列。
- [template] D:\luyuan\xuanming-server-mmo-cpp\tools\proto_generator\protogen\internal\template\grpc_init_total.h.tmpl: 对外声明里带 grpc::ChannelInterface 和 grpc::CompletionQueue。
- [generator-source] D:\luyuan\xuanming-server-mmo-cpp\tools\proto_generator\protogen\internal\generator\cpp\grpc_process.go: 要给模板提供 proto package（用于拼 /<pkg>.<Service>/<Method>），今天只有 CppPackage。这是 Go 源码，改了要重编 proto-gen。
- [generator-source] D:\luyuan\xuanming-server-mmo-cpp\tools\proto_generator\protogen\internal\generator\cpp\service_register_info.go: RpcMethodMeta.sender 的签名没有 done。只有决定迁移 gate 直连模式的通用转发时才需要改。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\generated\grpc_client\grpc_call_tag.h: 它在生成目录里但是手工维护的。
- GrpcCallFailure 的 context 和 status 是 grpc 类型；
- 如果 G1 或号段要按回显认领，GrpcCallOutcome 可能要加 initial metadata；
- 要去掉 grpcpp 头。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\network\grpc_client_runtime.h: HasUsableGrpcClient 与 QueueGrpcClientCompletion 都绑着 CQ 和 GrpcClientRuntime。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\network\node_utils.cpp: GetSceneManagerEntity 用 grpc::Channel::GetState 和 GRPC_CHANNEL_* 分级，要改成 muduo_grpc::ConnectivityState。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\network\node_utils.h: Candidate.channelState 的类型是 std::optional<grpc_connectivity_state>。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\node\system\node\node_connector.cpp: 官方分支要删掉，或者给 SceneManager、DataService、PlayerInbox、Login、Match 默认建 muduo 通道。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\node\system\node\node.cpp: 以下位置都基于 CQ：
- 关机阶段 0 的 BeginShutdownGrpcNode 和 5ms 排空（:1215-1280）；
- 节点摘除的 BeginShutdownGrpcNode 和 CompleteNodeRemoval（:1601-1637）。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\node\system\etcd\etcd_service.cpp: 要删的 5ms CQ 轮询在 :35-39。前提是 etcd 的官方回退路径也不再用 CQ。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\node\system\etcd\grpc_etcd_transport.cpp: 官方 etcd 回退路径依赖 InitGrpcNode、CQ 和流式模板，删 CQ 前要决定保不保留它。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\core\node\system\grpc_channel_cache.h: 官方通道缓存（ResourceQuota、keepalive 环境变量），所有出站迁完后就没有用户了。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\services\scene\player\system\player_lifecycle.cpp: SendCorrelatedEnterScene（:468-481）改用 GrpcDone，直接调 DispatchEnterSceneReply 或 DispatchEnterSceneTransportFailure。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\scene\rpc_replies\scene_manager_response_handler.cpp: 删掉 EnterScene 和 CreateScene 的全局处理器安装，日志逻辑搬进每次调用回调。CreateScene 部分以 blissful-morse 的改动为准。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\services\scene\player\system\player_scene.cpp: 镜像 CreateScene 改用 GrpcDone。blissful-morse worktree 已实现但未提交。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\scene\id_segment_bootstrap.cpp: 要改的地方：
- PickReadyDataServiceNode 的 view 与 READY 判断；
- OnResponse 用到的 ClientContext；
- OnFailure 用到的 GrpcCallFailure.status；
- InitDataServiceReply 改成每次调用回调；
- 去掉 grpcpp 头。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\services\scene\player\system\player_inbox_system.cpp: 要改的地方：
- PickReadyPlayerInboxNode 的 view 与 READY 判断；
- DefaultSendFetch / DefaultSendAck 改用 GrpcDone；
- kGrpcUnimplemented 不再取自 grpc::StatusCode。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\scene\rpc_replies\player_inbox_response_handler.cpp: 全局处理器适配；改用每次调用回调后可以整个删掉，并删掉 scene main.cpp:142 的调用。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\scene\main.cpp: InitSceneManagerReply 和 InitPlayerInboxReply 的调用点（:139、:142），以及给 scene 侧各目标 registry 安装通道工厂。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\gate\main.cpp: SetIfEmptyHandler 与 SetIfEmptyFailedHandler 用了 ClientContext 和 failure.status（:359-376）；直连模式的工厂安装与传输开关（:276-299）。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\nodes\gate\handler\rpc\client_message_processor.cpp: 要处理的位置：
- SendRawViaRouter 的官方分支（:225-241）；
- 直连模式的 Login.Disconnect（:568）；
- 通用 sender（:925）；
- PickRandomNode 的候选条件（:112-116）。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\engine\session\system\session.cpp: GetSessionDetailsByClientContext 依赖 grpc::ClientContext。
- [third-party] D:\luyuan\xuanming-server-mmo-cpp\third_party\muduo\muduo\net\TcpClient.cc: 补连接失败回调（第 3 批前置）。另外两份树也要补：third_party/muduo-linux/muduo/net/TcpClient.cc，cpp/libs/engine/muduo_windows/src/muduo/net/TcpClient.cc。
- [generated-output] D:\luyuan\xuanming-server-mmo-cpp\cpp\generated\grpc_client: 模板改完要重新生成，不能手改。
- [handwritten] D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\routing_identity_test\scene_manager_selector_test.cpp: 用例直接用 GRPC_CHANNEL_* 构造输入，要跟着改。cross_zone_test.cpp:1856 和 grpc_client_lifecycle_test.cpp 依赖 InitGrpcNode 与 CQ，也要改。
- [doc] [历史设计 D:\luyuan\xuanming-server-mmo-cpp\docs\design\grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md): §11 开头仍写“未 regen”，与生成物不符。第 3 批落地后，§11.3 里关于 CANCELLED 与 UNAVAILABLE 的说明也要更新。
## risks
- 【全局处理器签名带 grpc 类型】ClientContext 与 GrpcCallFailure 的 context、status 都是 grpc 类型。生成器换了传输但签名不变，这些位置都会编译不过：gate main.cpp:361 和 :374-375、scene_manager_response_handler.cpp:21-25、player_inbox_response_handler.cpp:20-21 和 :31-32、id_segment_bootstrap.cpp:184 和 :232-233、grpc_etcd_transport.cpp:53-70。设计说“第 3 批调用方不改”，这只对已经改用 GrpcDone 的调用方成立，而主工作区目前一个都没有。
- 【GrpcCallOutcome 不带响应头】它拿不到 initial metadata，有两处做不到“按回显认领”：G1 路由 Forward 的成功桥（按回显的 x-session-detail-bin）、号段 OnResponse 的 x-idseg-seq 精确归属。
- 【工厂与选点必须同批改】给 SceneManager、DataService、PlayerInbox 的 registry 装 muduo 工厂后，如果选点函数没同时改，后果是：
- GetSceneManagerEntity 因 HasUsableGrpcClient 要求 CQ 而恒返回 null：换图回 kEnterSceneServerBusy，交接走 ConcludeHandoffAfterMarkSent 被踢下线；
- PickReadyDataServiceNode 恒为空：号段领不到，scene 的 DependencyGate “id segments ready” 永不满足，世界 tick 不启动；
- 收件箱无限退避。
- 【CQ 轮询暂时删不掉】NODE_ETCD_TRANSPORT 默认是 grpc（muduo_etcd_transport.cpp:504-519），官方 etcd 回退路径仍靠 CQ 和 5ms 轮询（etcd_service.cpp:35-39）。“删 CQ 轮询”与“保留 etcd 官方回退”互相冲突。
- 【缺连接失败回调】三份 muduo 树都没有实现（TcpClient.cc:68）。对端拒连时，muduo 通道会在 20s 内一直显示 CONNECTING，GetSceneManagerEntity 的 ③a 级（失败通道）几乎永远看不到。只认 READY 的选点函数（号段、收件箱）不受影响，它们本来就退避。
- 【停机状态码变化】同一停机场景，官方 CQ 给 CANCELLED，muduo 通道析构给 UNAVAILABLE（grpc_call_tag.h:43-44 已有说明）。另外 muduo 关机不经 PollGrpcClientsInLoop 计数，只靠阶段 0 先清通道、再按 FIFO 排队来保证顺序。推测：关机期间回调里如果重发（例如号段重试），可能落到拆除之后。
- 【节点摘除的时序变化】官方路径会先排空 CQ 再 DestroyEntity（node.cpp:1613-1637）。muduo 通道随实体销毁，失败回调在实体销毁之后才到，所以 done 闭包不得捕获实体句柄或组件指针。
- 【IPv4 字面量要求】muduo_h2 要求对端 NodeInfo.grpc_endpoint.ip 是 IPv4 字面量（设计 §8）。本次没有核实 scene_manager、data_service、player_inbox、login、match 的 Go 侧注册是否满足（推测它们用 POD_IP）。
- 【metadata 编码不能动】生成层对所有 metadata 键（含非 -bin 键 x-idseg-seq）都先做 Base64Encode。换传输时这一层必须保留，否则线上字节会变，Go 侧和号段的回显比对会失配。
- 【并行工作树会冲突】以下工作树有未提交改动，落第 3 批前要先合并或协调：
- blissful-morse-969d44 改了 player_lifecycle.cpp/.h、player_scene.cpp/.h、scene_manager_response_handler.cpp；
- fervent-northcutt-6c0c12 和 practical-bouman-1beb0d 改了 client_message_processor.cpp。
- 【第 5 批的路径表】删除 *.grpc.pb.cc 后，官方生成的方法路径表随之消失，生成器得自己产出 :path 常量。battle_node.proto 没有 package，路径必须是 /BattleNode/<Method>。
- 【muduo 定时器用墙钟】墙钟回拨 N 秒，截止时间就晚 N 秒触发（设计 §6）。依赖 deadline 派生的业务看门狗（换图 TTL、号段 fetchTimeout）会一起被推迟。
## openQuestions
- 第 3 批的路线选哪条：
(a) 先把所有生产调用点迁到 GrpcDone，再只换传输；
(b) 全局处理器 API 一起改成与传输无关的类型，作为过渡继续保留？
- GrpcCallOutcome 要不要增加只读的响应头 metadata？还是 G1 路由与号段继续保留手写的按次回调路径？
- gate 直连模式（D61 计划删除）：第 3 批要迁移它（Login.Disconnect 和通用 sender），还是保持官方路径直到删除？
- etcd 官方回退路径（NODE_ETCD_TRANSPORT=grpc，现在是默认）在第 3 批保留还是删除？这决定 5ms CQ 轮询能不能删。
- muduo 通道工厂按节点类型逐个安装（每类一个开关，比照 GATE_ROUTER_TRANSPORT），还是 node_connector 统一默认走 muduo？回退方案是什么？
- Go 侧 scene_manager、data_service、player_inbox、login、match 注册的 grpc_endpoint.ip 是否都是 IPv4 字面量？本次未核实。
- blissful-morse-969d44 的镜像 CreateScene 每次调用回调，计划什么时候合回 main？
- [历史设计 grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md) §11 写着“未 regen”，与生成物和 muduo-rpc-channel.md §19.2 不一致，以哪份为准？regen 产物是否已经编译验证过？


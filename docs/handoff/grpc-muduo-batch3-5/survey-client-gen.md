<!-- 只读调查结果(2026-10-15,基线 HEAD 526780b4e 加当时工作区的未提交改动;第 3 批前半已落地,行号可能已漂移)。原样保留作交接附录,见同目录 README.md。 -->

# AREA client-gen
## summary
【现状一句话】C++ 生成的 gRPC 客户端仍然完全建在官方 gRPC C++ 运行时上(Stub、CompletionQueue、ClientContext、Status),靠 etcd 服务里一个 5ms 定时器轮询所有完成队列来驱动。muduo_grpc 只在两处手写接入:gate→路由服(第 1 批)和 etcd(第 2 批),生成的 Send* 一次都没有经过 muduo_grpc。每次调用回调(GrpcDone / GrpcCallOptions)已经写进模板(tools/proto_generator/protogen/internal/template/grpc_async_client.{h,cpp}.tmpl),生成产物里也已有,但主工作区里还没有任何生产代码在用它。

【现状图】
```
业务调用点
  scene: player_lifecycle.cpp:480 EnterScene / player_scene.cpp:382 CreateScene
         id_segment_bootstrap.cpp:160 AllocateIdSegment / player_inbox_system.cpp:89,101(未跟踪的新文件)
  gate : client_message_processor.cpp:236 Forward(官方路径) / :568 Login.Disconnect
         / :925 gRpcMethodRegistry.sender(直连模式通用转发)
  etcd : grpc_etcd_transport.cpp:75-110(NODE_ETCD_TRANSPORT=grpc,默认)
        │ Send<Svc><Method>(registry, nodeEntity, req[, metaKeys/metaValues | GrpcDone, GrpcCallOptions])
        ▼
cpp/generated/grpc_client/<域>/<文件>_grpc_client.cpp(生成)
  Start<Svc><Method>Call:try_get<grpc::CompletionQueue> + <Svc>StubPtr + GrpcClientRuntimePtr
  → PrepareAsync<M>(ClientContext, cq) → Finish(tag);缺组件 → QueueGrpcClientCompletion(UNAVAILABLE,异步投递)
        │ 节点实体上的组件由 grpc_init_client.cpp InitGrpcNode 挂上(GrpcClientRuntimePtr + CQ + 各文件 Stub)
        ▼
NodeConnector::ConnectToGrpcNode(node_connector.cpp:40-106)
  ├─ registry.ctx() 里有 MuduoGrpcChannelFactory → 只挂 MuduoGrpcChannelPtr(目前只有 gate 路由服装了工厂)
  └─ 否则 GrpcChannelCache::GetOrCreateChannel → 挂 shared_ptr<grpc::Channel> → InitGrpcNode
驱动:EtcdService::Init 的 grpcHandlerTimer.RunEvery(0.005) → HandleCompletedQueueMessage(每个 registry)
      → PollGrpcNodeShutdown(AsyncNext 零超时)→ Handle<File>CompletedQueueMessage → Complete<Svc><Method>
      → done(有就只交给它)/ Async<Svc><Method>Handler / FailedHandler
关机:FinalizeShutdownInLoop → EtcdService::Shutdown 停掉 5ms 轮询 → 对有 CQ 的实体 BeginShutdownGrpcNode,
      clear<MuduoGrpcChannelPtr> → DrainGrpcClientsInLoop(5ms 定时器重试 PollGrpcClientsInLoop)→ FinalizeTeardownInLoop
摘节点:ExecuteNodeRemoval → BeginShutdownGrpcNode → CompleteNodeRemoval(5ms 重试)→ DestroyEntity
旁路:gate_router_muduo::SendForward(router_muduo_forward.cpp)与 MuduoEtcdTransport 手写方法路径,直接调 muduo_grpc::Channel
```

【关键文件】
- 生成器:tools/proto_generator/protogen/internal/generator/cpp/grpc_process.go,以及 4 个模板 grpc_async_client.{h,cpp}.tmpl、grpc_init_total.{h,cpp}.tmpl。
- 产物:cpp/generated/grpc_client/ 下 20 对 *_grpc_client.{h,cpp},外加 grpc_init_client.{h,cpp}。
- 手写:
  - grpc_call_tag.h:放在 generated 目录里,但文件自己注明是手工维护。
  - cpp/libs/engine/core/network/grpc_client_runtime.h
  - node_connector.cpp
  - muduo_grpc_channel_slot.h
  - grpc_channel_cache.h
  - node.cpp:1197-1300
  - etcd_service.cpp:34-39
  - node_utils.cpp:246-270
  - grpc_call_deadline.{h,cpp}

【须注意】
- 工作区里另有会话的未提交改动正落在这些文件上:grpc_call_tag.h、grpc_init_client.cpp、node.cpp/h、etcd_service.*、gate 若干文件,以及未跟踪的 player_inbox 全套。
- worktree .claude/worktrees/blissful-morse-969d44 里有尚未合并的 CreateScene 每次调用回调改造。
## facts
### 生成器入口与输出文件
cmd/pipeline.go:70 注册 cppGen.CppGrpcCallClient。grpc_process.go:119-178 对每个 proto 文件(跳过 cc_generic_services=true 的 muduo RPC 服务)用 grpc_async_client.h.tmpl / .cpp.tmpl 渲染出 cpp_gen_grpc_dir + LogicalPath + _grpc_client.{h,cpp}(proto_gen.yaml:53、111-113)。grpc_process.go:180-219 用 buildGrpcInitFileInfo 的结果(:43-87,跳过 cc_generic_services 与 NODE_DB 文件,按 proto 文件合并多个 service,并按路径、包名、方法 Id 排序)渲染 grpc_init_total.{cpp,h}.tmpl,输出 grpc_init_client.{cpp,h}(proto_gen.yaml:71-72)。因此 db_grpc_client 会生成,但不进 InitGrpcNode 和 Set*Handler 广播。grpc_async.go 只剩注释。.tmpl 在运行时按相对路径从磁盘读(utils.RenderTemplateToFile),只改模板不需要重编 proto-gen;改 Go 源码要先跑 proto-gen-build([历史设计 docs/design/grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md) §6 第 5 条、§9.1)。
files: tools/proto_generator/protogen/cmd/pipeline.go, tools/proto_generator/protogen/internal/generator/cpp/grpc_process.go, tools/proto_generator/protogen/internal/generator/cpp/grpc_async.go, tools/proto_generator/protogen/etc/proto_gen.yaml
### 模板数据与 grpc 头依赖
GrpcServiceTemplateData(grpc_process.go:20-27)有以下字段。(1) GrpcIncludeHeadName:产出 #include "proto/<路径>/<文件>.grpc.pb.h",即 grpc_cpp_plugin 生成的 Stub 头(model.go:57-59)。(2) GrpcCompleteQueueName = FileBaseNameCamel + "CompleteQueue"(proto_gen.yaml:135 complete_queue),模板据此生成一个从未被使用的 struct <File>CompleteQueue{grpc::CompletionQueue cq;}(cpp.tmpl:27-29,例:data_service_grpc_client.cpp:24;全仓 grep 无引用)。(3) Package / FileBaseNameCamel / GeneratorGrpcFileName。gRPC 全方法路径 "/pkg.Service/Method" 目前只有 Go 侧路由表的 internal/route_table.go:61-71 会拼,C++ 模板数据里没有这个字段。
files: tools/proto_generator/protogen/internal/generator/cpp/grpc_process.go, tools/proto_generator/protogen/internal/model.go, tools/proto_generator/protogen/internal/route_table.go, cpp/generated/grpc_client/data_service/data_service_grpc_client.cpp
### 每个 unary 方法生成的结构体 / 函数 / 全局 handler
头文件 h.tmpl:50-64 生成 struct Async<Svc><Method>GrpcClient,成员有 messageId、ClientContext context、Status status、reply、unique_ptr<ClientAsyncResponseReader> response_reader、request 副本、GrpcSentMetadata sentMetadata、GrpcDone<Resp> done、GrpcClientRuntimePtr runtime。h.tmpl:70-79 与 cpp.tmpl:35-40 生成全局 Async<Svc><Method>Handler(std::function<void(const ClientContext&, const Resp&)>)和 Async<Svc><Method>FailedHandler(std::function<void(const GrpcCallFailure&, const Req&)>)。cpp.tmpl:34 生成 boost::object_pool <Svc><Method>Pool。cpp.tmpl 依次生成:Complete<Svc><Method>(:140-174,先看 done;没有 done 再走全局处理器;成功但没装 handler 时每线程打一次 ERROR;失败没装 FailedHandler 时打 ERROR)、AsyncCompleteGrpc<Svc><Method>(:176-183,ok=false 且 status OK 时改成 CANCELLED)、Start<Svc><Method>Call(:186-211,唯一发送入口)。Send 有 4 个重载(h.tmpl:81-88、cpp.tmpl:213-253):(req)、(req, metaKeys, metaValues)、(const Message&, metaKeys, metaValues)、(req, GrpcDone, const GrpcCallOptions& = {})。三参版显式传 std::vector<std::string>{},避免与 GrpcDone 重载二义。
files: tools/proto_generator/protogen/internal/template/grpc_async_client.h.tmpl, tools/proto_generator/protogen/internal/template/grpc_async_client.cpp.tmpl
### 流式方法(只有 etcd 两个)
ClientStreaming=true 时,h.tmpl:29-46 生成带 in_place_delete 的结构体,内含 ClientContext、Status、reply、writingRequest、deque pendingWrites、unique_ptr<grpc::ClientAsyncReaderWriter> stream 和若干状态位。该结构体以 ECS 组件形式 emplace 在节点实体上(cpp.tmpl:345);流在 Init<File>GrpcNode 时就开(:339-356)。另有 TryFinish / TryWriteNextNext / AsyncCompleteGrpc(cpp.tmpl:43-138)。Send 在 pendingWrites ≥ 200 时拒绝(:232-236)。流式方法没有 FailedHandler、没有 GrpcDone 重载、不设 deadline。全仓只有 etcd 的 Watch.Watch 与 Lease.LeaseKeepAlive 两个(etcd_grpc_client.h:201、299)。模板只判断 ClientStreaming,纯服务端流的方法会被错生成成 unary([历史设计 grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md) §10 第 3 条;model.go:300-311 有 ServerStreaming,但模板没用)。
files: tools/proto_generator/protogen/internal/template/grpc_async_client.h.tmpl, tools/proto_generator/protogen/internal/template/grpc_async_client.cpp.tmpl, cpp/generated/grpc_client/etcd/etcd_grpc_client.h, tools/proto_generator/protogen/internal/model.go
### 每个文件一份的函数与文件级状态
h.tmpl:94-105 与 cpp.tmpl:258-359 生成:Set<File>Handler、Set<File>IfEmptyHandler(参数都是 ClientContext&)、Set<File>FailedHandler、Set<File>IfEmptyFailedHandler、Set<File>CallDeadline(chrono::milliseconds)、Handle<File>CompletedQueueMessage(registry, entity, grpc::CompletionQueue&, ok, GrpcTag*,按 messageId switch,unique_ptr 保证回收 tag)、Init<File>GrpcNode(shared_ptr<::grpc::ChannelInterface>,对每个 Svc 调 NewStub 并启动流)。文件级匿名命名空间(cpp.tmpl:14-24)里有:tagPool(object_pool<GrpcTag>)、atomic callDeadlineMs(默认 kDefaultGrpcCallDeadlineMs),以及用 system_clock 算截止时间的 NextCallDeadline(:20-23,交给 context.set_deadline)。
files: tools/proto_generator/protogen/internal/template/grpc_async_client.h.tmpl, tools/proto_generator/protogen/internal/template/grpc_async_client.cpp.tmpl
### 每次调用回调(GrpcDone / GrpcCallOptions)现状
模板已实现:Start...Call 接收 metadata、deadline、done(cpp.tmpl:186-211);Complete 有 done 时只交给它,传 GrpcCallOutcome{error_code, error_message},reply 只在 ok 时非空(:147-152)。生成产物已含这些(data_service_grpc_client.cpp:2006-2031,41 个文件共 1390 处匹配)。两份文档对此说法冲突:[历史设计 grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md):275 写「未 regen、未编译」,muduo-rpc-channel.md:642 写「已随用户快照提交并 regen」。主工作区里只有测试在用 GrpcDone(grpc_client_lifecycle_test.cpp:464-556)。player_scene.cpp:382 仍是三参 SendSceneManagerCreateScene,scene_manager_response_handler.cpp:84-190 仍装全局 CreateScene 处理器;每次调用回调版只在 worktree .claude/worktrees/blissful-morse-969d44 里,是未提交改动。
files: tools/proto_generator/protogen/internal/template/grpc_async_client.cpp.tmpl, cpp/generated/grpc_client/data_service/data_service_grpc_client.cpp, [历史设计 docs/design/grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md), docs/design/muduo-rpc-channel.md, cpp/tests/rpc_controller_test/grpc_client_lifecycle_test.cpp, cpp/libs/services/scene/player/system/player_scene.cpp, cpp/nodes/scene/rpc_replies/scene_manager_response_handler.cpp
### 模板对 grpc:: 类型的依赖点(逐个)
h.tmpl:6 include core/network/grpc_client_runtime.h(其中再 include grpcpp/client_context.h、completion_queue.h);h.tmpl:8 include grpc_call_tag.h(grpcpp/client_context.h、support/status.h);h.tmpl:9 include *.grpc.pb.h;h.tmpl:16-18 using grpc::ClientContext / Status / ClientAsyncResponseReader;h.tmpl:23 <Svc>StubPtr = unique_ptr<<Svc>::Stub>;h.tmpl:33-38 与 51-55 结构体成员 ClientContext / Status / ClientAsyncReaderWriter / ClientAsyncResponseReader;h.tmpl:70-71、96-97 handler 签名含 ClientContext&;h.tmpl:102 grpc::CompletionQueue&;h.tmpl:103 ::grpc::ChannelInterface。cpp.tmpl:27-29 grpc::CompletionQueue;:50、63、134 stream->Finish / Write / Read;:76、104 context.TryCancel;:124 status.error_message;:149、167 status.error_code 与 GrpcCallFailure 引用 context、status;:180、202 Status(grpc::StatusCode::CANCELLED / UNAVAILABLE);:191 context.AddMetadata(key, Base64Encode(value));:196 context.set_deadline;:197 try_get<grpc::CompletionQueue>;:207-210 PrepareAsync / StartCall / Finish;:336 NewStub;:351-353 Async<M>(&context, &registry.get<grpc::CompletionQueue>, tag)。grpc_init_total.h.tmpl:7 <grpcpp/grpcpp.h>、:11-13 using、:35 ChannelInterface。grpc_init_total.cpp.tmpl:9 grpcpp、:41 CompletionQueue&、:80-88 all_of<grpc::CompletionQueue> 与 TryCancel、:98-119 queue->Shutdown / AsyncNext(gpr_timespec)/ CompletionQueue::SHUTDOWN 与 GOT_EVENT、:142 view<grpc::CompletionQueue>、:154 emplace<grpc::CompletionQueue>。另:grpc_init_total.cpp.tmpl:1 在 .cpp 里写了 #pragma once。
files: tools/proto_generator/protogen/internal/template/grpc_async_client.h.tmpl, tools/proto_generator/protogen/internal/template/grpc_async_client.cpp.tmpl, tools/proto_generator/protogen/internal/template/grpc_init_total.h.tmpl, tools/proto_generator/protogen/internal/template/grpc_init_total.cpp.tmpl
### grpc_call_tag.h(位于生成目录、实为手写)
L13-14 注明「本文件不是生成器产物,手工维护」。L10-11 include grpcpp/client_context.h 与 support/status.h。内容:GrpcTag{messageId, valuePtr}(L16-19);kDefaultGrpcCallDeadlineMs = 10000(L24);GrpcSentMetadata = vector<pair<string,string>>(L27,存 Base64 之前的原值);GrpcCallOutcome{uint32 code, message, ok()}(L31-36,与传输无关);GrpcDone<Reply>(L45-46);GrpcCallOptions{deadline, metadata}(L50-53);GrpcCallFailure{messageId, method, const grpc::ClientContext& context, const grpc::Status& status, sentMetadata, FindSentMetadata}(L64-80,是唯一还带 grpc 类型的契约结构)。工作区有未提交的注释改动(L42-44:不要按 code 区分停机与传输失败)。
files: cpp/generated/grpc_client/grpc_call_tag.h
### grpc_client_runtime.h(手写 core)
GrpcClientRuntime{unordered_set<grpc::ClientContext*> calls, queuedCompletions, activeCompletions, closing, queueShutdown, drained}(L17-25),只在节点 loop 线程访问。thread_local localCompletions(L33)。GrpcClientLocalCompletionsDrained()(L36-43,不在 EventLoop 线程调用就 LOG_FATAL)。GrpcClientCompletionScope 给 activeCompletions 计数(L46-59)。HasUsableGrpcClient(L61-70)要求 runtime 存在、未 closing,且有 grpc::CompletionQueue 组件,所以对只挂了 MuduoGrpcChannelPtr 的实体返回 false。QueueGrpcClientCompletion(L74-108)用 queueInLoop 延后投递本地拒绝,计数覆盖没有 runtime 的情况。
files: cpp/libs/engine/core/network/grpc_client_runtime.h
### grpc_init_client 公开接口与语义
(1) SetHandler / SetIfEmptyHandler / SetFailedHandler / SetIfEmptyFailedHandler:广播到 19 个文件(不含 db)的 Set<File>*(grpc_init_client.cpp:216-382)。(2) SetGrpcCallDeadline(nodeType, ms):对每个 nodeType 匹配的文件各用一个独立的 if(:384-442),由 grpc_call_deadline::Apply 调用。(3) BeginShutdownGrpcNode:只作用于有 CQ 的实体;置 closing,对所有在途 context 调 TryCancel,不阻塞(:444-454)。(4) PollGrpcNodeShutdown:非阻塞循环 AsyncNext(零超时);closing 且 calls 为空、回调空闲时才 Shutdown CQ;没有 CQ 或已排空且空闲时返回 true;按 nodeType + messageId 列表分派到 Handle<File>CompletedQueueMessage(:458-566)。(5) HandleCompletedQueueMessage(registry):对每个有 CQ 的实体调一次 Poll(:568-574),这就是平时正常的轮询路径。(6) InitGrpcNode:emplace GrpcClientRuntimePtr 与 grpc::CompletionQueue,再对 nodeType 匹配的每个文件调 Init<File>GrpcNode(:576-637)。例:SceneManagerNodeService 实体同时挂 SceneManager 与 SceneNodeGrpc 两个 Stub(:622-627)。头文件见 grpc_init_client.h:15-35。
files: cpp/generated/grpc_client/grpc_init_client.cpp, cpp/generated/grpc_client/grpc_init_client.h, tools/proto_generator/protogen/internal/template/grpc_init_total.cpp.tmpl
### 生成产物清单(按服务)
cpp/generated/grpc_client/ 下(方法数 / 流式数):battle/battle_node(全局命名空间 BattleNode,5)、battle/player_battle(全局 BattleClientPlayer,12)、chat/chat(chatpb ClientPlayerChat,2)、client_rpc_router(client_rpc_router ClientRpcRouter,1)、data_service(data_service DataService,24)、db/db(全局 db,1,不进 InitGrpcNode)、etcd/etcd(etcdserverpb KV / Watch / Lease,11,其中流式 2)、friend(friendpb,11)、guild/guild(guildpb GuildService,28)、guild/guild_internal(guildpb GuildInternal,1)、login(loginpb ClientPlayerLogin / LoginPreGate / LoginAdmin,9)、match/match_internal(1)、match/match_service(10)、player_inbox/player_inbox(2,目录未跟踪)、player_inbox/player_inbox_admin(6,未跟踪)、scene_manager/scene_manager_service(scene_manager SceneManager,4)、scene_manager/scene_node_service(scene_node SceneNodeGrpc,8)、team(teampb,15)、trade/jubaozhai(4)、trade/trade_admin(1)。另有 grpc_init_client.{h,cpp}、grpc_call_tag.h(手写),以及手工维护的 CMakeLists.txt / grpc_client.vcxproj / .filters。生成器不写工程文件:local_event_cleanup.go:63 只碰 proto 工程。
files: cpp/generated/grpc_client/CMakeLists.txt, cpp/generated/grpc_client/grpc_client.vcxproj, cpp/generated/grpc_client/grpc_init_client.cpp, tools/proto_generator/protogen/internal/prototools/local_event_cleanup.go
### gRpcMethodRegistry 通用 sender(另一份生成物依赖 Send 签名)
service_register_info.go:339-355 为每个非 cc_generic 的方法生成一行声明 `Send<Svc><Method>(entt::registry&, entt::entity, const google::protobuf::Message&, const std::vector<std::string>&, const std::vector<std::string>&)`,并填进 gRpcMethodRegistry[...].sender(rpc_event_registry.h:19-25)。gate 直连模式用它转发任意客户端 gRPC 消息(client_message_processor.cpp:889-929)。第 3 批只要这个重载保持原签名,rpc_event_registry 就不受影响。
files: tools/proto_generator/protogen/internal/generator/cpp/service_register_info.go, cpp/generated/rpc/service_metadata/rpc_event_registry.h, cpp/nodes/gate/handler/rpc/client_message_processor.cpp
### NodeConnector::ConnectToGrpcNode 与 muduo 工厂接缝
node_connector.cpp:40-106 的流程:按 uuid 去重;要求 grpc_endpoint 非空;目标 registry.ctx() 里有 MuduoGrpcChannelFactory 时,只 emplace MuduoGrpcChannelPtr 与 NodeInfo,不调 InitGrpcNode(:73-92);否则 GrpcChannelCache::GetOrCreateChannel(FormatIpAndPort),emplace shared_ptr<grpc::Channel>,调 InitGrpcNode,最后 emplace NodeInfo(:94-99)。muduo_grpc_channel_slot.h:19-30 只前向声明 muduo_grpc::Channel,所以 core 不用链 muduo_grpc。全仓唯一装工厂的地方是 gate_router_muduo::InstallChannelFactory(router_muduo_forward.cpp:51-71),条件是 routerMode 且 GATE_ROUTER_TRANSPORT=muduo_h2(gate/main.cpp:276-299);工厂建好通道立即 GetState(true) 拨号。
files: cpp/libs/engine/core/node/system/node/node_connector.cpp, cpp/libs/engine/core/node/system/node/muduo_grpc_channel_slot.h, cpp/nodes/gate/handler/rpc/router_muduo_forward.cpp, cpp/nodes/gate/main.cpp
### gate 路由路径:两套传输并存
SendRawViaRouter 实体上挂了 MuduoGrpcChannelPtr 就调 gate_router_muduo::SendForward;否则要求同时有 grpc::CompletionQueue 和 ClientRpcRouterStubPtr,都没有时 fail-closed(client_message_processor.cpp:210-242)。PickRandomNode 接受挂 muduo 通道的实体,或 HasUsableGrpcClient 为真的实体(:101-122)。SendForward 手写方法路径 "/client_rpc_router.ClientRpcRouter/Forward"(router_muduo_forward.cpp:33);应用层先自己做一次 Base64Encode(:84),线上 -bin 那层交给传输层,共两层;deadline 取 grpc_call_deadline::Get(:79-80);回包走 rpc_reply_bridge。gate 的官方路径通过 main.cpp:359-376 的 SetIfEmptyHandler / SetIfEmptyFailedHandler 装全局桥,成功回包靠 GetSessionDetailsByClientContext(session.cpp:13-25,读 ctx.GetServerInitialMetadata)。
files: cpp/nodes/gate/handler/rpc/client_message_processor.cpp, cpp/nodes/gate/handler/rpc/router_muduo_forward.cpp, cpp/nodes/gate/main.cpp, cpp/libs/engine/session/system/session.cpp
### 文档与代码不一致:「两种传输都在 grpc::ChannelInterface 之下」
[历史设计 grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md):316 写「生成代码没有 muduo_grpc 分支:两种传输都在 grpc::ChannelInterface 之下」。代码事实:走 muduo 工厂的实体没有 Stub、没有 CQ、也没有 GrpcClientRuntimePtr(node_connector.cpp:73-92);对这种实体调生成的 Send,会走 cpp.tmpl:197-205 的本地 UNAVAILABLE("node transport missing or closing")。正因如此 gate 才单独写了 SendForward 旁路。
files: [历史设计 docs/design/grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md), cpp/libs/engine/core/node/system/node/node_connector.cpp, tools/proto_generator/protogen/internal/template/grpc_async_client.cpp.tmpl
### grpc_channel_cache
GrpcChannelCache(grpc_channel_cache.h:79-149):按 target 用 weak_ptr 缓存官方 Channel。参数:ResourceQuota 线程上限读 GRPC_MAX_THREADS(默认 2);backup poll 5000ms;keepalive 30s / 10s;PERMIT_WITHOUT_CALLS 默认 1;MAX_PINGS_WITHOUT_DATA=0。grpc-muduo-transport.md §4.4 指出这套「30s、空闲也 ping」的设置已触发过 too_many_pings。Node 持有它(node.h:94、278);使用方是 node_connector.cpp:95 和 grpc_etcd_transport.cpp:29-30;Node::Initialize 会打印它的配置(node.cpp:524-526)。
files: cpp/libs/engine/core/node/system/grpc_channel_cache.h, cpp/libs/engine/core/node/system/node/node.h, cpp/libs/engine/core/node/system/node/node.cpp, cpp/libs/engine/core/node/system/etcd/grpc_etcd_transport.cpp
### 5ms CQ 轮询与影子定时器
etcd_service.cpp:34-39:EtcdService::Init 无论 etcd 用哪种传输,都 RunEvery(0.005) 遍历所有 registry 调 HandleCompletedQueueMessage。全仓只有这里调用 HandleCompletedQueueMessage,它是平时驱动所有官方 CQ 的唯一入口。停止点在 EtcdService::Shutdown 第一行的 Cancel(etcd_service.cpp:726)。Node 自己的 grpcHandlerTimer(node.h:257)从未 RunEvery / RunAfter 过,只在 node.cpp:1205 被 Cancel,是影子成员。设计 §13.9 写明第 3 批删 CQ 时一并删这个轮询。
files: cpp/libs/engine/core/node/system/etcd/etcd_service.cpp, cpp/libs/engine/core/node/system/etcd/etcd_service.h, cpp/libs/engine/core/node/system/node/node.h, cpp/libs/engine/core/node/system/node/node.cpp, docs/design/grpc-muduo-transport.md
### Node 关机链
FinalizeShutdownInLoop(node.cpp:1197-1230)依次:ReleaseNodeId;serviceDiscoveryManager.Shutdown(内含 EtcdService::Shutdown,停 5ms 轮询并释放 etcd 传输);Cancel 各定时器;CloseAllRpcChannels;对每个有 CQ 的实体 BeginShutdownGrpcNode;nodeRegistry.clear<MuduoGrpcChannelPtr>()(~Channel 把在途调用以 UNAVAILABLE 排进队列);最后 DrainGrpcClientsInLoop。PollGrpcClientsInLoop(:1232-1264):先 Poll 每个 CQ 实体,再只读复核 runtime 的 drained / calls / queued / active,最后要求 GrpcClientLocalCompletionsDrained。DrainGrpcClientsInLoop(:1266-1280):没排空就用 grpcClientDrainTimer_ 隔 5ms 重试,排空后 queueInLoop(FinalizeTeardownInLoop)。FinalizeTeardownInLoop(:1282-):kafka 与 redis Shutdown,再 Poll 一次,没排空就回到 Drain;然后 tlsNodeContextManager.Clear()、tlsEcs.Clear()、停日志、两跳 quit。RunNodeMain 的兜底路径(node_entry.h:160-171)不清 tlsNodeContextManager,也不处理 CQ / Stub,对应设计 §10.1 的 F012 / F022。
files: cpp/libs/engine/core/node/system/node/node.cpp, cpp/libs/engine/core/node/system/node/node.h, cpp/libs/engine/core/node/system/node/node_entry.h
### 节点摘除链
ExecuteNodeRemoval 用 queueInLoop 找到 uuid 匹配的实体,依次调 BeginShutdownGrpcNode、触发 OnNodeRemoveEvent、调 CompleteNodeRemoval(node.cpp:1580-1609)。CompleteNodeRemoval(:1613-1637):PollGrpcNodeShutdown 返回 false 时,在实体上挂 TimerTaskComp 隔 5ms 重试,排空后 DestroyEntity。muduo 实体没有 CQ,Poll 直接返回 true,随即销毁,~Channel 以 UNAVAILABLE 结束在途调用。gate 的 OnNodeRemoveEventHandler 注释仍写「CompletionQueue 随后析构,其上的在途调用永远不会再有回调(F29)」(client_message_processor.cpp:1240-1241);推测:该注释已落后于现行的 Begin / Poll 排空实现。
files: cpp/libs/engine/core/node/system/node/node.cpp, cpp/nodes/gate/handler/rpc/client_message_processor.cpp
### grpc_call_deadline
Resolve / Apply / Get(grpc_call_deadline.h:18-36)。Apply 在 Node::Initialize 里、第一个 gRPC 请求之前调用(node.cpp:518),逐类型调生成的 SetGrpcCallDeadline(grpc_call_deadline.cpp:63),并把生效值记进 gEffectiveMs 供 Get 查询。该文件 include 了 grpc_call_tag.h 与 grpc_init_client.h,间接带进 grpcpp。Get 的使用方:muduo_etcd_transport.cpp:167、router_muduo_forward.cpp:80、client_request_lane.cpp:147、id_segment_bootstrap.cpp:287、player_inbox_system.cpp:159、player_lifecycle.cpp:272 / 3243 / 3664 / 3866。
files: cpp/libs/engine/core/node/system/grpc_call_deadline.h, cpp/libs/engine/core/node/system/grpc_call_deadline.cpp, cpp/libs/engine/core/node/system/node/node.cpp
### 按 GetState 选点的位置(第 3 批要改用新状态机)
(1) GetSceneManagerEntity(node_utils.cpp:246-270):候选必须 HasUsableGrpcClient,并挂 shared_ptr<grpc::Channel>,调 GetState(true),所以 muduo 实体会被排除;ClassifyCandidate 按 GRPC_CHANNEL_* 分级(:93-111);WarnLastResortPick(:190-207);node_utils.h:4 include <grpc/impl/connectivity_state.h>,Candidate.channelState 的类型是 optional<grpc_connectivity_state>(:38);注释 :52-64 依赖 pick_first 的「空闲 30min 回 IDLE / TRANSIENT_FAILURE 不回报 CONNECTING」语义。(2) PickReadyDataServiceNode(id_segment_bootstrap.cpp:91-108)。(3) PickReadyPlayerInboxNode(player_inbox_system.cpp:63-80,未跟踪的新文件)。(4) 测试 scene_manager_selector_test.cpp 用 GRPC_CHANNEL_*。新状态机:muduo_grpc::ConnectivityState(channel.h:47-54),Channel::GetState(bool)(:191)。
files: cpp/libs/engine/core/network/node_utils.cpp, cpp/libs/engine/core/network/node_utils.h, cpp/nodes/scene/id_segment_bootstrap.cpp, cpp/libs/services/scene/player/system/player_inbox_system.cpp, cpp/tests/routing_identity_test/scene_manager_selector_test.cpp, cpp/libs/engine/muduo_grpc/channel.h
### 手写代码对全局 handler 签名与 GrpcCallFailure grpc 字段的依赖
读 ClientContext 内容的:gate/main.cpp:359-363(GetSessionDetailsByClientContext)、id_segment_bootstrap.cpp:178-205(ctx.GetServerInitialMetadata 找 x-idseg-seq)。只在签名里带 ClientContext 但忽略它的:scene_manager_response_handler.cpp:31、85;player_inbox_response_handler.cpp:14、25;grpc_etcd_transport.cpp:53-70、118;etcd_service.cpp:742-743。用 failure.status.error_code / error_message 的:gate/main.cpp:374-375、id_segment_bootstrap.cpp:232-233、scene_manager_response_handler.cpp:23-24、player_inbox_response_handler.cpp:20-21 / 31-32。player_inbox_system.cpp:44 直接用 grpc::StatusCode::UNIMPLEMENTED。测试:grpc_client_lifecycle_test.cpp:128、178、217(failure.context)。session.{h,cpp}:11-13 的 GetSessionDetailsByClientContext 签名是 grpc::ClientContext。
files: cpp/nodes/gate/main.cpp, cpp/nodes/scene/id_segment_bootstrap.cpp, cpp/nodes/scene/rpc_replies/scene_manager_response_handler.cpp, cpp/nodes/scene/rpc_replies/player_inbox_response_handler.cpp, cpp/libs/engine/core/node/system/etcd/grpc_etcd_transport.cpp, cpp/libs/engine/core/node/system/etcd/etcd_service.cpp, cpp/libs/engine/session/system/session.cpp, cpp/libs/engine/session/system/session.h, cpp/libs/services/scene/player/system/player_inbox_system.cpp, cpp/tests/rpc_controller_test/grpc_client_lifecycle_test.cpp
### C++ 出站 gRPC 清单(按节点)
scene:白名单为 SceneManager / DataService / PlayerInbox(scene/main.cpp:102-104),另加 etcd。发送点:player_lifecycle.cpp:480(EnterScene)、player_scene.cpp:382(CreateScene)、id_segment_bootstrap.cpp:160(AllocateIdSegment)、player_inbox_system.cpp:89、101;应答处理器在 scene/main.cpp:139-142 装。gate:路由模式白名单为 Scene + ClientRpcRouter;直连模式为 Scene / Login / SceneManager / Match(gate/main.cpp:229-231),经 gRpcMethodRegistry.sender 转发,另有 Login.Disconnect(client_message_processor.cpp:568)。battle:无出站拨号(battle/main.cpp:259-262),只有 etcd。etcd 官方路径的 8 个 Send 在 grpc_etcd_transport.cpp:75-110。
files: cpp/nodes/scene/main.cpp, cpp/nodes/gate/main.cpp, cpp/nodes/battle/main.cpp, cpp/libs/services/scene/player/system/player_lifecycle.cpp, cpp/libs/services/scene/player/system/player_scene.cpp, cpp/nodes/scene/id_segment_bootstrap.cpp, cpp/libs/core/node/system/etcd/grpc_etcd_transport.cpp
### etcd 官方回退传输依赖 CQ
GrpcEtcdTransport 的构造用 GrpcChannelCache 建通道,并对 Etcd 全局实体调 InitGrpcNode(grpc_etcd_transport.cpp:24-42),handler 装在生成层的全局 std::function 上(:49-70)。它要靠 5ms CQ 轮询才能工作。NODE_ETCD_TRANSPORT 默认值是 grpc(设计 §13.1、§13.8)。MuduoEtcdTransport 手写方法路径(muduo_etcd_transport.cpp:35-42),直接调 Channel::StartUnaryCall / StartStream。
files: cpp/libs/engine/core/node/system/etcd/grpc_etcd_transport.cpp, cpp/libs/engine/core/node/system/etcd/muduo_etcd_transport.cpp, docs/design/grpc-muduo-transport.md
### muduo_grpc 客户端 API(第 3 批要对接的出口)
Channel::Create(loop, ip, port, ChannelOptions) 只接受 IPv4 字面量(channel.h:175-177)。StartUnaryCall(UnaryCallRequest{method :path、已序列化 message、Metadata、steady_clock deadline(必填)}, UnaryCallback(UnaryCallResult{status, initialMetadata, trailingMetadata, response}))(:89-105、184),回调恰好一次、从不同步执行,通道析构时以 UNAVAILABLE 结束在途调用(:5-13)。另有 StartStream(:188)、GetState(:191)、outstandingCalls(:194)。Status 的 code_value 与 grpc::StatusCode 数值一致(status.h:17-63)。Metadata 是有序的 vector<pair>,-bin 键只在线上编 / 解一层(metadata.h:5-19)。默认在途上限 65536,收发上限各 4MiB(channel.h:67-87)。
files: cpp/libs/engine/muduo_grpc/channel.h, cpp/libs/engine/muduo_grpc/status.h, cpp/libs/engine/muduo_grpc/metadata.h
### muduo 三份树、连接失败回调与 Windows wakeup
三处 TcpClient.cc:68 都是 `// FIXME setConnectFailedCallback`:third_party/muduo(仓内跟踪,muduo_grpc.vcxproj:110 在 Windows 上用它的头)、third_party/muduo-linux(子模块,上游 chenshuo/muduo,.gitmodules:13-15)、cpp/libs/engine/muduo_windows/src/muduo/net(子模块 luyuan-cpp/muduo-windows,.gitmodules:26-28;muduo.vcxproj:43、56 编它的 Connector.cc / TcpClient.cc)。Connector 连接失败只走 retry:Windows 树 Connector.cc:97-99、199-211;Linux 树 :95-97、197-209。Windows 的 EventLoop::wakeup() 在非 __linux__ 下是空操作(muduo_windows/src/muduo/net/EventLoop.cc:258-268)。Windows 上 kPollTimeMs=100,poll 超时取定时器队列的下一个到期时间(EventLoop.cc:36、128-133)。
files: third_party/muduo/muduo/net/TcpClient.cc, third_party/muduo-linux/muduo/net/TcpClient.cc, cpp/libs/engine/muduo_windows/src/muduo/net/TcpClient.cc, cpp/libs/engine/muduo_windows/src/muduo/net/Connector.cc, cpp/libs/engine/muduo_windows/src/muduo/net/EventLoop.cc, cpp/libs/engine/muduo_grpc/muduo_grpc.vcxproj, .gitmodules
### 构建链接面
grpc_client 的 CMakeLists.txt / vcxproj 是手工维护的,Windows include 路径含 third_party/grpc/include 与 third_party/muduo(grpc_client.vcxproj:169)。core 引用 generated/grpc_client 的头(core.vcxproj:95,core/CMakeLists.txt:41)。25 个测试 vcxproj 链 grpc_client.lib;链 muduo_grpc 的只有 gate / scene / battle 三个节点(vcxproj + CMakeLists)和 muduo_grpc_test,后者刻意不链 grpc_client(muduo_grpc_test.vcxproj:4、146)。tools/scripts/check_no_raw_pointer_member.ps1 会扫生成的头(模板注释 h.tmpl:68-69 提到它)。
files: cpp/generated/grpc_client/grpc_client.vcxproj, cpp/generated/grpc_client/CMakeLists.txt, cpp/libs/engine/core/core.vcxproj, cpp/libs/engine/core/CMakeLists.txt, cpp/tests/muduo_grpc_test/muduo_grpc_test.vcxproj, tools/scripts/check_no_raw_pointer_member.ps1
### 依赖 CQ 的测试
grpc_client_lifecycle_test.cpp 整个文件:用 InitGrpcNode、grpc::CreateChannel、手挂 CQ / Stub / Runtime,调 Begin / PollGrpcNodeShutdown,另用 grpc::ServerBuilder 起真实 Watch 服务测流;PerCallDone 三条在 :464-556。cross_zone_test.cpp:1855-1864(InitGrpcNode 后 Begin / Poll)。scene_manager_selector_test.cpp(GRPC_CHANNEL_*)。生成器单测 grpc_process_test.go:40-145 断言 HandleCompletedQueueMessage / PollGrpcNodeShutdown / Handle*CompletedQueueMessage 字符串,以及 InitGrpcNode、SetGrpcCallDeadline 不用 else if。
files: cpp/tests/rpc_controller_test/grpc_client_lifecycle_test.cpp, cpp/tests/cross_zone_test/cross_zone_test.cpp, cpp/tests/routing_identity_test/scene_manager_selector_test.cpp, tools/proto_generator/protogen/internal/generator/cpp/grpc_process_test.go
### 并行会话的在途改动(只读核对)
git status 显示以下文件有未提交改动(M):grpc_call_tag.h、grpc_init_client.cpp、data_service_grpc_client.{h,cpp}、grpc_client CMakeLists.txt / vcxproj / filters、node.{h,cpp}、node_util.{h,cpp}、etcd_service.{h,cpp}、muduo_etcd_transport.cpp、discovery_key_index.h、gate 的 CMakeLists / vcxproj / client_message_processor.cpp、生成器 config.go / proto_gen*.yaml 及多个 generator/cpp/*.go。未跟踪(??):cpp/generated/grpc_client/player_inbox/、player_inbox_system.cpp、player_inbox_response_handler.cpp、proto/player_inbox/。worktree .claude/worktrees/blissful-morse-969d44(与 main 同为 526780b4e)里未提交地改了 player_scene.{h,cpp}、player_lifecycle.{h,cpp}、player_scene_handler.cpp、scene_manager_response_handler.cpp、cross_zone_test.cpp 等。
files: cpp/generated/grpc_client/grpc_call_tag.h, cpp/libs/engine/core/node/system/node/node.cpp, cpp/libs/engine/core/node/system/etcd/etcd_service.cpp, cpp/libs/services/scene/player/system/player_inbox_system.cpp
## changeSurface
- [template] tools/proto_generator/protogen/internal/template/grpc_async_client.h.tmpl: 去掉 grpc.pb.h / ClientContext / Status / Stub / ClientAsyncResponseReader / ChannelInterface / CompletionQueue 依赖。调用对象改成持有传输无关的状态(请求副本、sentMetadata、done)。Init<File>GrpcNode 与 Handle<File>CompletedQueueMessage 的签名要换或删。全局 handler 签名里的 ClientContext 要换成传输无关的回包上下文(至少带 initial metadata)。
- [template] tools/proto_generator/protogen/internal/template/grpc_async_client.cpp.tmpl: Start<Svc><Method>Call 改为:序列化请求 → muduo_grpc::Channel::StartUnaryCall(:path、Metadata、steady_clock deadline)→ 回调里解析 response,交给 done 或全局处理器。保留应用层 Base64Encode(:191,两层 base64 不能省)。缺通道组件时仍异步 UNAVAILABLE。删 tagPool、CompleteQueue 结构、AsyncCompleteGrpc 与 messageId switch。流式分支(只有 etcd)要么删,要么改用 StartStream。Send 的 Message& 重载签名要保持,供 rpc_event_registry 使用。
- [template] tools/proto_generator/protogen/internal/template/grpc_init_total.h.tmpl: InitGrpcNode(ChannelInterface)、HandleCompletedQueueMessage、Begin / PollGrpcNodeShutdown 的签名与语义都绑在 CQ 上,要改成按通道组件的初始化与排空(或删除);去掉 <grpcpp/grpcpp.h> 和 using grpc::*。
- [template] tools/proto_generator/protogen/internal/template/grpc_init_total.cpp.tmpl: 去掉 CQ 轮询、AsyncNext、Shutdown 和按 nodeType+messageId 的分派(:79-160)。Set*Handler 广播和 SetGrpcCallDeadline 可以保留。顺带处理 :1 在 .cpp 里的 #pragma once。
- [generator-source] tools/proto_generator/protogen/internal/generator/cpp/grpc_process.go: 模板数据:去掉 GrpcCompleteQueueName,不再需要 GrpcIncludeHeadName(.grpc.pb.h),新增每个方法的 gRPC :path。改 Go 源码要 proto-gen-build。
- [generator-source] tools/proto_generator/protogen/internal/model.go: 推测:需要给 MethodInfo 加生成 "/pkg.Service/Method" 的 helper,复用 route_table.go:61-71 的拼法,避免规则写两份。
- [generator-source] tools/proto_generator/protogen/internal/generator/cpp/grpc_process_test.go: 现有断言(:107-142)检查 HandleCompletedQueueMessage / PollGrpcNodeShutdown / Handle*CompletedQueueMessage 字符串,模板改后要同步改。
- [generator-source] tools/proto_generator/protogen/internal/generator/cpp/service_register_info.go: 只有 Send(Message&, metaKeys, metaValues) 的签名变化时才要改(:343-354);保持原签名就不用动。
- [generator-source] tools/proto_generator/protogen/etc/proto_gen.yaml: 可选:删掉 complete_queue 命名项(:135);no-unity 副本和 config.go:158 同改。注意这三个文件已有他人的未提交改动。
- [handwritten] cpp/generated/grpc_client/grpc_call_tag.h: 放在 generated 目录但手工维护。GrpcCallFailure 里的 grpc::ClientContext& / grpc::Status& 要换成 code / message(或 GrpcCallOutcome)加 sentMetadata;去掉 grpcpp include;GrpcTag 不再需要。已有他人的未提交改动。
- [handwritten] cpp/libs/engine/core/network/grpc_client_runtime.h: GrpcClientRuntime.calls 存的是 grpc::ClientContext*;HasUsableGrpcClient 要求 grpc::CompletionQueue 组件,要改成判断通道组件(对应 F100 / F006 / F082 / F011)。
- [handwritten] cpp/libs/engine/core/node/system/node/node_connector.cpp: ConnectToGrpcNode 改成对所有 gRPC 节点类型建 muduo_grpc 通道(或按开关选择),不再走 GrpcChannelCache + InitGrpcNode。
- [handwritten] cpp/libs/engine/core/node/system/node/muduo_grpc_channel_slot.h: MuduoGrpcChannelFactory 目前要求每个节点类型的 registry.ctx() 各装一份,全部迁移后要改成进程级的工厂或默认通道创建方式。
- [handwritten] cpp/libs/engine/core/node/system/grpc_channel_cache.h: 客户端全部迁走后,只剩 GrpcEtcdTransport 回退路径在用;删除或缩小范围。
- [handwritten] cpp/libs/engine/core/node/system/node/node.cpp: FinalizeShutdownInLoop / PollGrpcClientsInLoop / DrainGrpcClientsInLoop / FinalizeTeardownInLoop(:1197-1300)、CompleteNodeRemoval 的 5ms 重试(:1613-1637)、ExecuteNodeRemoval 的 BeginShutdownGrpcNode(:1601)、影子成员 grpcHandlerTimer 的 Cancel(:1205)、grpc_channel_cache 配置日志(:524-526),都要改成按通道组件排空。已有他人的未提交改动。
- [handwritten] cpp/libs/engine/core/node/system/node/node.h: 删除影子成员 grpcHandlerTimer(:257),以及 grpcChannelCache / GetGrpcChannelCache(:94、278)。
- [handwritten] cpp/libs/engine/core/node/system/node/node_entry.h: RunNodeMain 兜底路径(:160-171)未处理节点 registry 上的传输组件(F012 / F022,§10.1 列为第 3 批)。
- [handwritten] cpp/libs/engine/core/node/system/etcd/etcd_service.cpp: 删除 5ms CQ 轮询(:34-39)及其 Cancel(:726);SetEtcdHandler 空 handler 的签名会随 handler 签名一起变(:742-743)。已有他人的未提交改动。
- [handwritten] cpp/libs/engine/core/node/system/etcd/etcd_service.h: 删除 grpcHandlerTimer 成员(:175)。
- [handwritten] cpp/libs/engine/core/node/system/etcd/grpc_etcd_transport.cpp: 官方 etcd 回退路径完全依赖 InitGrpcNode + CQ + 5ms 轮询(:24-42、49-118);删 CQ 时要决定去留,handler 签名也会变。
- [handwritten] cpp/libs/engine/core/node/system/etcd/etcd_transport.cpp: 推测:回退被删时,NODE_ETCD_TRANSPORT 默认值和 InstallFromEnv / 工厂要一起改。
- [handwritten] cpp/libs/engine/core/node/system/grpc_call_deadline.cpp: include 了 grpc_call_tag.h / grpc_init_client.h,随这两个头去 grpc 化;SetGrpcCallDeadline 接口保留时逻辑可以不变。
- [handwritten] cpp/libs/engine/core/network/node_utils.cpp: GetSceneManagerEntity(:246-270)从 shared_ptr<grpc::Channel>::GetState 改成 muduo_grpc::Channel::GetState;ClassifyCandidate / WarnLastResortPick 换用新枚举;注释 :52-64 里的 pick_first 语义也要重写。
- [handwritten] cpp/libs/engine/core/network/node_utils.h: 去掉 <grpc/impl/connectivity_state.h>,Candidate.channelState 改成传输无关的枚举(:4、38)。
- [handwritten] cpp/libs/engine/session/system/session.cpp: GetSessionDetailsByClientContext 读 grpc::ClientContext 的 initial metadata(:13-25),要换成按传输无关的回包 metadata 查找。
- [handwritten] cpp/libs/engine/session/system/session.h: 同上,签名(:11)。
- [handwritten] cpp/nodes/gate/main.cpp: SetIfEmptyHandler / SetIfEmptyFailedHandler 的 lambda 签名(:359-376);GATE_ROUTER_TRANSPORT 分支与 InstallChannelFactory 要并进统一机制(:274-344)。
- [handwritten] cpp/nodes/gate/handler/rpc/client_message_processor.cpp: SendRawViaRouter 的两套传输分支和对 CQ / Stub 的 all_of 守卫(:210-242)、PickRandomNode 的 HasUsableGrpcClient(:101-122)、:1240 的过期注释。已有他人的未提交改动。
- [handwritten] cpp/nodes/gate/handler/rpc/router_muduo_forward.cpp: 生成客户端走 muduo 后,这条手写旁路(硬编码路径、自建工厂、AllChannelsIdle)可以删或收缩;关机等待里的 AllChannelsIdle 要有替代。
- [handwritten] cpp/nodes/gate/gate_router_transport.h: 推测:路由服专用的传输开关要推广为全部出站共用的开关,或删除。
- [handwritten] cpp/nodes/scene/id_segment_bootstrap.cpp: PickReadyDataServiceNode 用 grpc::Channel::GetState(:91-108);OnResponse 读 ctx.GetServerInitialMetadata(:178-205);OnFailure 用 failure.status(:232-233);InitDataServiceReply 的 lambda 签名(:264-272)。
- [handwritten] cpp/nodes/scene/rpc_replies/scene_manager_response_handler.cpp: 全局 handler 签名用 grpc::ClientContext(:31、85);DescribeGrpcFailure 用 failure.status(:21-25)。worktree blissful-morse 里也在改这个文件。
- [handwritten] cpp/nodes/scene/rpc_replies/player_inbox_response_handler.cpp: 未跟踪的新文件(他人在写):用 grpc::ClientContext 与 failure.status(:13-32)。
- [handwritten] cpp/libs/services/scene/player/system/player_inbox_system.cpp: 未跟踪的新文件:PickReadyPlayerInboxNode 用 grpc::Channel::GetState(:63-80),还有 grpc::StatusCode::UNIMPLEMENTED(:44)。
- [handwritten] cpp/libs/engine/muduo_grpc/http2_transport.cpp: 接入新加的连接失败回调后立即进 TRANSIENT_FAILURE,不再等 connect_timeout(设计 §5「已知差异」)。
- [handwritten] cpp/libs/engine/muduo_grpc/channel.cpp: 推测:连接失败要能直接触发 EnterTransientFailure 与退避。
- [third-party] third_party/muduo/muduo/net/TcpClient.cc: 补 setConnectFailedCallback(:68 FIXME),同时要改 TcpClient.h、Connector.{h,cc}。这棵树仓内跟踪,Windows 编 muduo_grpc 时用它的头。
- [third-party] third_party/muduo-linux/muduo/net/TcpClient.cc: 同上,Linux 树。这是子模块(上游 chenshuo/muduo),改动要提交到子模块。
- [third-party] cpp/libs/engine/muduo_windows/src/muduo/net/TcpClient.cc: 同上,Windows 源码树(子模块 luyuan-cpp/muduo-windows),要和 third_party/muduo 的头保持一致。
- [third-party] cpp/libs/engine/muduo_windows/src/muduo/net/EventLoop.cc: wakeup() 在 Windows 上是空操作(:258-268)。设计 §10 把修它列为删 5ms 轮询的前提,§13.1 写明不在第 2 批做;推测随第 3 批处理。也是子模块。
- [build] cpp/generated/grpc_client/grpc_client.vcxproj: 手工维护。要加 muduo_grpc 的 include;不再需要 grpc include 时同步移除。
- [build] cpp/generated/grpc_client/CMakeLists.txt: 手工维护。Linux 的 include 与源码清单。
- [build] cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj: 推测:grpc_client 一旦依赖 muduo_grpc,链 grpc_client / core 的测试工程(25 个 vcxproj)都要加 muduo_grpc.lib 和 nghttp2.lib。这里列出代表项,其余见 risks。
- [handwritten] cpp/tests/rpc_controller_test/grpc_client_lifecycle_test.cpp: 整个文件建在官方 CQ / Stub / ServerBuilder 上,要按新传输重写,或改用 muduo_grpc_test 里的回环服务。
- [handwritten] cpp/tests/cross_zone_test/cross_zone_test.cpp: :1855-1864 用 grpc::CreateChannel + InitGrpcNode + Begin / Poll。worktree blissful-morse 里也在改这个文件。
- [handwritten] cpp/tests/routing_identity_test/scene_manager_selector_test.cpp: 用 GRPC_CHANNEL_* 喂选择器,要跟着 Candidate 的类型改。
- [generated-output] cpp/generated/grpc_client/data_service/data_service_grpc_client.cpp: 代表全部 20 对 *_grpc_client.{h,cpp}(battle×2、chat、client_rpc_router、data_service、db、etcd、friend、guild×2、login、match×2、player_inbox×2、scene_manager×2、team、trade×2),只能 regen。
- [generated-output] cpp/generated/grpc_client/grpc_init_client.cpp: 由 grpc_init_total.cpp.tmpl 生成,只能 regen。
- [generated-output] cpp/generated/grpc_client/grpc_init_client.h: 由 grpc_init_total.h.tmpl 生成,只能 regen。
- [generated-output] cpp/generated/rpc/service_metadata/rpc_event_registry.cpp: 只有 Send(Message&, metaKeys, metaValues) 的签名变化时才会变;保持原签名就不受影响。
- [doc] docs/design/grpc-muduo-transport.md: 写第 3 批的设计与落地记录。
- [doc] [历史设计 docs/design/grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md): §11 的「未 regen」与 :316「两种传输都在 grpc::ChannelInterface 之下」都和代码事实不符;另外 GrpcCallFailure 契约会变。
## risks
- 并行会话撞文件:grpc_call_tag.h、grpc_init_client.cpp、node.{h,cpp}、etcd_service.*、gate 的 client_message_processor.cpp 与工程文件、生成器 config 都有他人未提交的改动。player_inbox 全套是未跟踪的新文件。worktree blissful-morse-969d44 正在改 player_scene / scene_manager_response_handler / cross_zone_test。第 3 批动手前必须重新确认归属,不能覆盖。
- 前置条件的状态在文档里互相矛盾:grpc-muduo-transport.md:278-281 约定「每次调用回调」会话宣布完成前,本迁移不碰客户端模板、grpc_process.go 与 grpc_call_tag.h。[历史设计 grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md):275 写未 regen,muduo-rpc-channel.md:642 写已 regen(产物里确实已有)。
- AGENTS §3 禁止手改 generated/,但 grpc_call_tag.h 放在 cpp/generated/grpc_client/ 下,文件里又自称手工维护。第 3 批要改它,应当先确认这个例外,或把文件挪出 generated。
- 地址限制:muduo_grpc::Channel::Create 只认 IPv4 字面量(channel.h:175)。全部出站迁移后,SceneManager / DataService / PlayerInbox / Login / Match 任一实例在 grpc_endpoint 登记的是主机名时,通道建不起来、节点进不了候选;官方路径则支持 DNS。
- 链接面扩大:推测,生成代码一旦直接调 muduo_grpc,core(node_connector 调 InitGrpcNode)→ grpc_client → muduo_grpc → nghttp2 的依赖就是传递的,25 个链 grpc_client.lib 的测试工程和三个节点工程都要补 muduo_grpc.lib 与 nghttp2.lib,否则 LNK2019。设计 §9 里「core 不链 muduo_grpc」的前提随之失效。
- Windows 上 EventLoop::wakeup() 是空操作。推测:删掉 5ms 轮询后,在 doPendingFunctors 里再 queueInLoop 出来的完成回调要等下一次 poll,最长约 100ms(kPollTimeMs);今天被 5ms 定时器掩盖。
- 连接失败回调要改三棵 muduo 树,其中两棵是子模块(muduo-linux 上游、muduo-windows 自有仓),需要子模块提交。Windows 编 muduo_grpc 用 third_party/muduo 的头、链 muduo_windows 的实现,两边的 TcpClient / Connector 声明必须逐字一致,否则 ODR / ABI 不一致。
- 可观察行为会变:停机 / 摘节点时在途调用的状态码从 CANCELLED 变成 UNAVAILABLE(grpc_call_tag.h:42-44 已提示调用方不要依赖)。官方通道的 30s keepalive 加空闲 ping 没了,改为截止触发探测(§4.4)。官方通道「空闲 30 分钟回 IDLE」语义消失,node_utils.cpp:52-64 的分级理由要重写。建连失败要到补了回调才能立即进 TRANSIENT_FAILURE。
- 线上兼容:metadata 的两层 base64(cpp.tmpl:191 应用层 Base64Encode + 传输层 -bin)必须保留;非 -bin 键(如 x-idseg-seq)的值经应用层 Base64 后满足 ASCII 校验。deadline 从 system_clock 改成 steady_clock 加 grpc-timeout 编码,超过 8 位的值会被 Go 拒绝(§4.1)。
- etcd 回退:GrpcEtcdTransport(NODE_ETCD_TRANSPORT 的默认值 grpc)完全依赖 CQ 与 5ms 轮询。删 CQ 而不处理它,默认配置下 etcd 直接失效;保留它就删不掉 CQ。
- 全局 handler 签名里的 ClientContext 和 GrpcCallFailure 里的 grpc::Status / ClientContext 一变,gate main、id_segment、scene_manager / player_inbox 应答处理器、grpc_etcd_transport、etcd_service、两个测试都会编译失败;gate 与 id_segment 还实际读 initial metadata。rpc_event_registry 的 sender 依赖 Send(Message&, metaKeys, metaValues),这个签名要保持。
- 模板里流式方法的状态结构是节点实体上的 ECS 组件(ECS 审计 C008 的同类问题)。第 3 批若保留流式分支并改用 StartStream,应把流状态放进不可移动的调用对象,不要再做成组件。
- tools/scripts/check_no_raw_pointer_member.ps1 会扫生成的头,新调用对象里若出现裸指针成员(例如 Channel*)会被拦。
- 生成器改 Go 源码(grpc_process.go / model.go)后必须先 proto-gen-build 再 proto-gen-run。没有客户端授权时要用 proto_gen.no-unity.yaml(AGENTS §4)。只改 .tmpl 则不用重编。
- 模板遗留问题:纯服务端流的方法会被误生成成 unary;<File>CompleteQueue 结构是死代码;grpc_init_total.cpp.tmpl:1 在 .cpp 里写了 #pragma once。重写时一并处理,避免带进新模板。
## openQuestions
- 「每次调用回调」会话是否已宣布完成?grpc-muduo-transport.md:278-281 的门槛是否已满足?两份文档关于 regen 状态的矛盾以哪份为准?
- 第 3 批是否像第 1、2 批一样保留进程级传输开关(设计 §1 第 3 条要求迁移期新旧并存、启动时选定),还是对所有出站直接切到 muduo_grpc?如果保留开关,CQ 轮询就删不掉,和 §10 第 3 批的「删除 CQ 轮询」冲突。
- NODE_ETCD_TRANSPORT=grpc 的官方回退在第 3 批删掉,还是保留到第 5 批?默认值是否改成 muduo_h2?
- 生成代码是直接依赖 muduo_grpc::Channel(grpc_client 链 muduo_grpc / nghttp2),还是在 core 里定义一个传输无关的通道接口,由节点 main 注入?后者能保住「core / 测试不链 muduo_grpc」。
- 全局 handler 签名与 GrpcCallFailure 是一次性改成传输无关(破坏性,调用方同批改),还是先加兼容层?回包上下文要暴露哪些字段(initial metadata、trailing metadata、status)?
- 模板里的流式分支(只有 etcd 的 Watch / LeaseKeepAlive)直接删掉(etcd 已经手写在 MuduoEtcdTransport 里),还是改用 StartStream 生成?
- db_grpc_client 目前生成了但没接进 InitGrpcNode,第 3 批是否停止生成?
- K8s 和现网里各 Go 服务的 grpc_endpoint.ip 是否全是 IPv4 字面量?不是的话,是否需要在 loop 外做 DNS,或引入 c-ares?
- Windows wakeup 修复与 muduo 单调时钟定时器(设计 §6、§10)是否算进第 3 批?改两个 muduo 子模块是否已获授权?
- gate 直连模式(Login / SceneManager / Match 的通用 sender)是否还要在第 3 批迁移,还是按 D61 随直连路径一起删?
- Java 版:设计 §12 判定本迁移不属于双版本共享契约,第 3 批是否沿用这个结论,PARITY.md 不登记?


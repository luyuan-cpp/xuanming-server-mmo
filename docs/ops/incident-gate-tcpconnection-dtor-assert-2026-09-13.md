# 事故报告:gate 在 `~TcpConnection` 断言崩溃(thread_local 持有连接强引用)

> **日期**: 2026-09-13
> **发现方式**: 本机联调,VS 附加到运行中的 `bin\gate.exe`(Debug)后在 `muduo::net::TcpConnection::~TcpConnection()` 断住
> **影响**: Debug 下 gate 进程停在 assert(被调试器挂起后一区 gate 实际不可用);**Release 下 assert 被编掉,同一路径是 poller 里悬垂 `Channel*` 的 use-after-free**
> **触发条件**: gate 连出去的节点链路(TcpClient)被拆(节点停机 / 重连),且拆之前 gate 收到的**最后一条 RPC** 恰好来自这条链路
> **根因**: `tlsRpc.conn`(thread_local `TcpConnectionPtr` 强引用)只写不清,把 muduo `TcpClient::~TcpClient` 的 `use_count()==1` 唯一性判断带偏,`forceClose()` 被跳过,连接在 `kConnected` 态被析构
> **修复**: 引入 per-call 上下文对象 `RpcController`(继承 protobuf 同名基类),随 `Service::CallMethod` **按次显式传递**(Go `ctx` 语义);`thread_local RpcRequestContext tlsRpc` **整个删除**;生成代码零改动
> **状态**: 已落码;core / gate / scene / thread_context 四个工程定点编译(含被牵连重编的 8 个依赖 TU)**0 error / 0 warning**;5 个独立对抗视角**均未推翻**;回归测试 `rpc_controller_test` 已加入统一测试入口,**修复前红态 / 修复后绿态均已实测,最终 6/6**(§6.4);全部预制库重建后现有 gtest 套件 **22/22 全过**(过程中的 3 个异常项均证实为预制库漂移、非产品缺陷,见 §6.5);**运行时验收已通过**(§6.3,09-13 04:44 真机删 etcd 键复现,拿到 `forceClose` 确已执行的正向证据,而非仅"没崩")

---

## 1. 现象

VS 调试器断在:

```
ucrtbased.dll!...                                   ← _wassert / abort
> gate.exe!muduo::net::TcpConnection::~TcpConnection() 行 71        C++
  [外部代码]                                        ← std::shared_ptr 释放
  gate.exe!GameChannel::HandleRpcMessage(...) 行 314               C++   ← tlsRpc.conn = conn;
  ...
  gate.exe!muduo::net::TcpConnection::handleRead(...) 行 354
  gate.exe!muduo::net::EventLoop::loop() 行 146
  gate.exe!main(...)
```

- 断言点是 [third_party/muduo/muduo/net/TcpConnection.cc:71](../../third_party/muduo/muduo/net/TcpConnection.cc) `assert(state_ == kDisconnected);`
  —— **一个 `state_` 仍为 `kConnected` 的连接对象被销毁了**。不是"连接早断开",是反过来。
- 触发帧是 [cpp/libs/engine/core/network/game_channel.cpp](../../cpp/libs/engine/core/network/game_channel.cpp) 原 314 行 `tlsRpc.conn = conn;`:这次赋值放掉了**上一个**连接的最后一根强引用。
- 断住后进程被调试器挂起:CPU 5 秒内 0 ms、46 个线程 `Suspended`、`run/logs/cpp_nodes/z1_gate.stdout.log` 停在 23:45:46,而 scene/battle 日志仍在滚动。**调试期间一区 gate 事实上下线。**

## 2. 时间线

| 时间 | 事件 |
|---|---|
| 09-13 03:33 | `start_game.ps1` 拉起本机整栈(gate/scene/battle + 6 个 Go 服务),客户端进入游戏正常 |
| 09-13 03:35~ | gate 日志出现节点拆连事件 `Service node stop: entity not found ... node_type=4`(node.cpp:1353),scene_manager 出现 `[Rebalance] ... (node_gone)` |
| 09-13 23:45 | 用户 VS 附加 `bin\gate.exe`(pid 53696),gate 在 `~TcpConnection` 断住,进程从此挂起 |
| 09-13 | 排查:读栈 → 定位 `tlsRpc.conn` 生命周期 → 复核 muduo `TcpClient` 析构语义 → 复核 `RpcClient` 成员顺序 → 确认 TcpServer 侧免疫 → 定案 |
| 09-13 | 第一版修复(`GameRpcController`)落码、编译通过;5 视角对抗性复核启动 |
| 09-13 | 按评审意见定型:类名改 `RpcController`、`tlsRpc` 整体删除、`From()` 改 Release-safe;全部工程重新定点编译通过;复核结论回填 |
| 09-13 | 测试验证收口:新增回归测试 `rpc_controller_test`(红态实测 before=3/after=4 → 修复后 6/6);修好 runner 两处会造成假绿的缺陷(`zlibd.dll` 暂存目录、单个 exe 启动失败吞整张表);查实全套三个异常项均为 lib 漂移而非本次改动 —— 其中 `turn_battle_engine_test` 是漏建 `battle.lib` 导致的 `BattleActorState` ODR 违反,重建后 93/93;`§6.3` 运行时复现步骤修正(硬杀节点复现不出,须删 etcd 键) |

## 3. 根因(逐环,均有代码位置)

**前提**:gate 是单 IO 线程(全仓无 `setThreadNum`,见 [client_message_processor.cpp:536](../../cpp/nodes/gate/handler/rpc/client_message_processor.cpp) 注释),所有回调都在主 `EventLoop`,`tlsRpc` 只有一份。

1. **`tlsRpc.conn` 只写不清。**
   原 `rpc_request_context.h` 声明 `extern thread_local RpcRequestContext tlsRpc;`,其成员 `conn` 是 `std::shared_ptr<TcpConnection>`。全仓唯一写点是 `game_channel.cpp:314`,每条入站 RPC 都写一次,**从未 reset**。于是它永远攥着"上一条消息所在连接"的一个强引用,跨派发存活。

2. **节点停机 → 实体销毁 → `RpcClient` 析构。**
   gate 连出去的节点链路是 `muduo::net::TcpClient`,由 [rpc_client.h:176](../../cpp/libs/engine/core/network/rpc_client.h) 的 `RpcClient` 以值持有,`RpcClient` 作为 `RpcClientPtr` 组件挂在 entt 实体上。节点停机走 [node.cpp:1352](../../cpp/libs/engine/core/node/system/node/node.cpp) `DestroyEntity` → `~RpcClient`。

3. **成员析构顺序让 `tlsRpc.conn` 成为唯一"多余"引用。**
   `RpcClient` 的声明顺序是 `client_`(TcpClient,176 行)在前、`channel_`(GameChannel,177 行)在后,析构逆序:**先** `~GameChannel`(放掉 `GameChannel::connection_`),**再** `~TcpClient`。所以到 `~TcpClient` 时,这条连接的强引用只剩:

   | 持有者 | 到 `~TcpClient` 时还在? |
   |---|---|
   | `TcpClient::connection_` | ✔(它自己) |
   | `GameChannel::connection_` | ✘(已先析构) |
   | `tlsRpc.conn` | ✔ ← **多出来的这一个** |

4. **muduo 的 `unique` 判断被带偏,`forceClose` 被跳过。**
   [TcpClient.cc:81-93](../../third_party/muduo/muduo/net/TcpClient.cc):
   ```cpp
   unique = connection_.use_count() == 1;     // 期望 1,实际 2
   ...
   if (unique) conn->forceClose();            // unique==false → 跳过!
   ```
   非 unique 时它只把 closeCallback 换成 `detail::removeConnection`,**不关连接**。`TcpClient::connection_` 随对象析构释放,但这条连接**从没走过** `handleClose()` / `connectDestroyed()`:`state_` 仍是 `kConnected`,`Channel` 仍注册在 poller 里,唯一持有者变成 `tlsRpc.conn`。

5. **最后一根引用被下一条消息覆盖。**
   下一条**任意**连接上的消息到达 `game_channel.cpp:314`,`tlsRpc.conn = conn` 覆盖旧值 → 引用归零 → `~TcpConnection` → `assert(state_ == kDisconnected)` 在 `kConnected` 下触发。

**为什么客户端连接不会这样**:TcpServer 侧走 [TcpServer.cc:106-117](../../third_party/muduo/muduo/net/TcpServer.cc) `removeConnectionInLoop` → 无条件排队 `connectDestroyed`,而 [connectDestroyed](../../third_party/muduo/muduo/net/TcpConnection.cc) 会把 `kConnected` 强制翻成 `kDisconnected` 并 `channel_->remove()`。多持一个引用只是延迟释放,不会 assert。**唯一漏网的就是 TcpClient 的 unique-skip 路径**,因此崩溃必然发生在节点间链路上,与 "node stop" 时机吻合。

**Release 下的真实危害**:`assert` 与 [Channel.cc:42-43](../../third_party/muduo/muduo/net/Channel.cc) `assert(!addedToLoop_)` 都被编掉。连接在仍登记于 `PollPoller::channels_` 时被析构,下一次 [WindowsPollPoller.cc:67](../../third_party/muduo/muduo/net/poller/WindowsPollPoller.cc) `fillActiveChannels` 按 fd 查表取到的是已释放的 `Channel*` —— use-after-free。这不是 Debug 专属的小毛病。

## 4. 这行代码为什么会存在(设计层面的原因)

- 原 `RpcRequestContext` 的注释白纸黑字承诺 "Reset at each RPC dispatch entry point / valid only during a single RPC handler invocation",但 reset **从未被实现**;单 IO 线程让它"看起来能用"。
- 真正的缺口在派发层:`GameChannel::ProcessMessage` 调 `service->CallMethod(method, nullptr, request, response, nullptr)` —— protobuf 给 `Service::CallMethod` 预留的 per-call 上下文形参 `RpcController*` **被传成了 `nullptr`**。手写 handler(如 `OnNodeHandshake(req, resp)`)拿不到"本次调用在哪条连接上",于是用 thread_local 兜。
- 这违反 muduo 的基本规范:**`TcpConnectionPtr` 只应在回调期间持有;需要跨回调"记住"连接必须用 `weak_ptr` + `lock()`**。thread_local 里的强引用恰好是"跨回调持有"的最隐蔽形态。

## 5. 修复

### 5.1 设计

目标语义就是 Go 的 `context.Context`:**按次创建、随调用链显式传递、调用结束即销毁、绝不落 ambient 状态**。

C++ 标准库没有 `context` 等价物(`std::stop_token` 只管取消;`boost::context` 是协程栈切换,无关;`folly::RequestContext` / `brpc::Controller` 是整套第三方框架)。但本项目的 RPC 就是 protobuf `Service` / `CallMethod`,**protobuf 官方为它设计的 per-call 上下文对象正是 `google::protobuf::RpcController`**(brpc 的 `Controller` 就继承自它)。所以不引入任何依赖,把已经存在、一直被传成 `nullptr` 的槽位用起来:

```
ProcessMessage 栈上构造 RpcController(conn)
   → service->CallMethod(method, &rpcContext, ...)
      → 生成代码原样转发 controller                       // 生成代码零改动
         → GateHandler/SceneHandler::NodeHandshake(controller, ...)
            → OnNodeHandshake(RpcController::From(controller), req, resp)   // 显式收 ctx
   ← CallMethod 返回,rpcContext 析构                       // 生命周期到此为止
```

**命名**:类名就叫 `RpcController`,与基类 `::google::protobuf::RpcController` 同名是**有意的**(它就是那个槽位的实现,brpc 同法)。代价是全仓**不得** `using namespace google::protobuf` —— 已核实非生成代码与生成代码中均无一处;生成代码本来就全限定书写。

### 5.2 改动清单(11 改 / 1 增 / 2 删,+33 / −71)

| 文件 | 改动 |
|---|---|
| `cpp/libs/engine/core/network/rpc_controller.h` **(新增)** | `class RpcController final : ::google::protobuf::RpcController`:持本次调用的 `TcpConnectionPtr` 拷贝 + 会话 id / 下一跳路由三个标量;禁拷贝;`From(controller)` 用 `dynamic_cast + LOG_FATAL` 做 Release-safe 受检下转;7 个纯虚接口最小实现 |
| `cpp/libs/engine/core/network/game_channel.cpp` | `ProcessMessage`:`RpcController rpcContext(conn);` 以 `&rpcContext` 替代 `nullptr` 传入 `CallMethod`;**删除** `HandleRpcMessage` 里的 `tlsRpc.conn = conn;`;去掉对 `rpc_request_context.h` 的 include |
| `cpp/libs/engine/thread_context/rpc_request_context.h` **(删除)** | `RpcRequestContext` 整个移除。`conn` 由 `RpcController` 取代;`currentSessionId_ / nextRouteNodeType_ / nextRouteNodeId_` 折进 `RpcController`;`routeData_`(proto 对象)/ `routeMsgBody_` 全仓无读者且重量级,不再携带 |
| `cpp/libs/engine/thread_context/rpc_request_context.cpp` **(删除)** | 原只定义 `thread_local RpcRequestContext tlsRpc;` |
| `cpp/libs/engine/thread_context/thread_context.vcxproj` / `.filters` | 移除上述 .cpp / .h 条目 |
| `cpp/libs/engine/thread_context/trace_context_tls.h` | 注释:不再引用已删除的 `tlsRpc` 模式,改为指向 `RpcController` |
| `cpp/libs/engine/core/node/system/registration/registration_manager.h` | `OnNodeHandshake` 首参新增 `const RpcController& ctx`;前向声明 |
| `cpp/libs/engine/core/node/system/registration/registration_manager.cpp` | 从 `ctx.conn()` 取连接(局部名 `rpcConn`,避免与 `tryRegister` lambda 形参 `conn` 遮蔽);两处 `tlsRpc.conn` 读取删除;include 换新头 |
| `cpp/nodes/gate/handler/rpc/gate_service_handler.cpp` | `NodeHandshake` 调用改传 `RpcController::From(controller)`;两处"controller 恒为 nullptr"的旧注释改正 |
| `cpp/nodes/scene/handler/rpc/scene_handler.cpp` | `NodeHandshake` 调用改传 `RpcController::From(controller)` |
| `cpp/nodes/gate/rpc_replies/route_message_response_handler.cpp` | 删除 3 行 `defer(tlsRpc.Set…)` 死写(响应路径没有 CallMethod,函数已显式收到 `conn`) |

### 5.3 关键片段

```cpp
// game_channel.cpp  ProcessMessage
MessagePtr response(service->GetResponsePrototype(method).New());
RpcController rpcContext(conn);   // 栈上,CallMethod 返回即析构
service->CallMethod(method, &rpcContext, boost::get_pointer(request), boost::get_pointer(response), nullptr);
```

```cpp
// rpc_controller.h —— Release 下 NDEBUG 会抹掉 assert,所以用 LOG_FATAL 保证 fail-fast 在两种构建里都成立
static const RpcController& From(::google::protobuf::RpcController* controller)
{
    auto* self = dynamic_cast<RpcController*>(controller);   // nullptr 输入也返回 nullptr
    if (self == nullptr)
    {
        LOG_FATAL << "CallMethod dispatched without an RpcController (controller="
                  << static_cast<const void*>(controller) << ")";
    }
    return *self;
}
```

### 5.4 为什么它能修好

`~TcpClient` 时对这条连接的强引用回到 **1**(只剩 `TcpClient::connection_`;`GameChannel::connection_` 已先析构;栈上的 `rpcContext` 早在那次派发返回时就没了)→ `unique == true` → `forceClose()` 执行 → `handleClose()` 把状态翻成 `kDisconnected`、`channel_->disableAll()`、closeCallback 排队 `connectDestroyed` → 之后无论谁最后释放它,析构时都已是 `kDisconnected`。

派发期间 `rpcContext` 持一份拷贝是安全的:此时 muduo 自己的 `Channel::handleEventWithGuard` 正持有 `tie_` 守卫,连接在本次回调内本就不可能被析构。**由此引出一条必须遵守的规则(§8-4)**:handler 内不得同步释放本连接所属 `RpcClient` 的最后一个引用——回调期间 muduo 守卫与栈上 ctx 各持一份强引用,`~TcpClient` 必然看到 `use_count>1` 而跳过 `forceClose`;节点摘除必须像现在一样经 `queueInLoop` 延后(node.cpp 的 `DestroyEntity` 路径)。

### 5.5 讨论过但没采用的两个方案

| 方案 | 能不能修 | 为什么不选 |
|---|---|---|
| `tlsRpc.conn` 改 `std::weak_ptr`,读点 `lock()` | 能(永不进 `use_count`) | `tlsRpc.conn = conn;` 变成一次**静默的 shared→weak 转换**,写点看不出语义变了;而且连接本就不该"跨回调被记住",weak_ptr 是给真正跨回调的场景用的 |
| 保留 `shared_ptr`,在 `HandleRpcMessage` 入口加 RAII、退出时 `reset()` | 能修当前时间线 | 仍是 ambient 状态,反模式留着;它只是把"从未实现的 reset"补上,没解决"没有 per-call 载体"这个根子 |

### 5.6 生成代码零改动(证据)

- 生成的 `Gate::CallMethod`(`cpp/generated/proto/gate/gate_service.pb.cc`)与 `Scene::CallMethod` 把 `controller` **原样转发**给各方法虚函数;方法签名本来就是 `::google::protobuf::RpcController* controller`。我们的对象 IS-A 基类,穿过生成代码时生成代码无需知道。
- 全仓 repo-owned 代码里 **只有一个** `service->CallMethod(` 派发点(`GameChannel::ProcessMessage`),TcpServer 与 TcpClient 两条传输都汇入它。
- `scene_handler.cpp` 另有三处 `->CallMethod(` 是 `PlayerService::CallMethod`(entt 签名,另一套接口),够不到 `NodeHandshake`;`BattleHandler` 的自定义 `CallMethod` 是判空的终端 sink,不再派发。
- muduo vendored 的 protorpc `RpcChannel.cc:140` 确实以 `NULL` controller 派发,但仓库从未实例化 `muduo::net::RpcChannel`,不可达(见 §7.6)。
- 代码生成器模板(`tools/proto_generator`)不引用 `tlsRpc` / `rpc_request_context`,重新生成不会把它带回来。

### 5.7 语义不变点

其他 handler 以前就忽略 `controller`(它一直是 `nullptr`,没人敢用),现在传真对象对它们零影响;`done` 仍恒为 `nullptr`(相关注释已同步修正)。

## 6. 验证

### 6.1 编译(最终版本,已完成)

`Debug|x64`,对改动 TU 定点 `ClCompile`(链接不在本步:可执行文件被运行中的进程占用,见 §6.3):

```
MSBuild <proj>.vcxproj /t:ClCompile /p:Configuration=Debug /p:Platform=x64 /p:BuildProjectReferences=false /p:SelectedFiles=...
```

| 工程 | 实际编译的 TU(含因头文件变更被自动重编的依赖 TU) | 结果 |
|---|---|---|
| core | `game_channel.cpp`、`registration_manager.cpp`;此前一轮还牵连 `node.cpp`、`node_connector.cpp`、`node_allocator.cpp`、`etcd_manager.cpp`、`etcd_service.cpp`、`service_discovery_manager.cpp` | 0 error / 0 warning |
| gate | `gate_service_handler.cpp`、`route_message_response_handler.cpp`;牵连 `client_message_processor.cpp`、`main.cpp`、`scene_response_handler.cpp` | 0 error / 0 warning |
| scene | `scene_handler.cpp` | 0 error / 0 warning |
| thread_context | 全工程 `ClCompile`(移除 `rpc_request_context.cpp` 后);实际重编 `trace_context_tls.cpp` | 0 error / 0 warning |

三个工程均开启 `/WX`(TreatWarningAsError)与 RTTI,`dynamic_cast` 与 `LOG_FATAL` 均可用。

**注意 `ClCompile` 不产出 `lib/*.lib`。** 单元测试工程不带 ProjectReference,链接的是 `lib/` 下的预制静态库;要让测试真正吃到修复,必须以 Build 目标重建 `thread_context.vcxproj`、`core.vcxproj`(已做,03:06)。实际操作中还发现 `lib/rpc.lib`(09-09)与 `lib/proto.lib`(09-09)早已落后于 09-12 02:45 重新生成的代码(09-11 提交新增了 bag / mission 的 RPC 与消息):新 `core.lib` 引用的 `gRpcMethodRegistry` 是 `std::array<RpcMethodMeta,196>`,旧 `rpc.lib` 里不是这个长度;新 `rpc.lib` 又引用旧 `proto.lib` 里没有的 43 个新消息符号。两者一并重建后才能链接(§6.4、§7.8)。

### 6.2 对抗性复核(5 个独立视角,均未推翻;0 blocker,4 should-fix)

| # | 视角 | 结论 |
|---|---|---|
| 1 | 崩溃时间线下 `~TcpClient` 处 `use_count` 是否确为 1;是否还有别的强引用能活过派发 | **未推翻**。逐一核实了 `GameChannel::connection_`(成员顺序先释放)、`HandleIncomingMessage` 以 `get_pointer` 绑定(不加引用)、`GetConnection()` 只做指针比较、`OnConnected2TcpServerEvent` 为临时量、muduo 自身 pending functor;栈上 ctx 不可能是最后一根引用(`handleRead` 持 `shared_from_this`、`Channel` 持 `tie_` 守卫,`DestroyEntity` 经 `queueInLoop` 延后)。残留:握手重试定时器按值捕获(→ §7.1) |
| 2 | 是否存在任何以 `nullptr` / 异类 controller 触达 `NodeHandshake` 的路径 | **未推翻**。见 §5.6;唯一提示是 Release 下 assert 消失 → 已改为 `LOG_FATAL` |
| 3 | MSVC 编译与语义(签名一致、遮蔽告警、RTTI、include 路径、protobuf 版本纯虚集合) | **未推翻**。复核方自行用 MSBuild 18.7 在各工程自身设置下逐 TU 编译通过;`OnNodeHandshake` 声明 / 定义 / 两个调用点均为 3 参;无其他调用点 |
| 4 | 全仓同类反模式盘点(A = TcpServer 侧必达 `connectDestroyed`,免疫;B = 出站 TcpClient,可复现本事故) | **未推翻**。B 类残留两处(§7.1、§7.2)+ 一处待证实(§7.3);`RpcSession` 经证明恒为 A 类(§7.4);battle / scene 今天没有出站 TCP 节点链路,B 类只在 gate 可达 |
| 5 | 设计回归(间接读者、响应路径、派发内同步拆自身链路、拷贝 vs 引用) | **未推翻**。`tlsRpc.conn` 无任何间接读者(TLOG_* 只读 tracing 前缀);响应路径不依赖 thread_local;持拷贝是正确选择;派发内同步拆自身链路的路径今天不存在,但必须作为规则写明(§8-4) |

### 6.3 运行时验收(**已通过**,09-13 04:44)

**前置**(09-13 03:36 全部满足):调试器已退出(pid 53696 不存在)、`bin\gate.exe` 可写、无 mmorpg 进程残留;`proto/rpc/thread_context/core/table/modules/scene/grpc_client` 已重建,`cpp\nodes\{gate,scene,battle}` 三个 Application 工程重链后由各自的 `PostBuildEvent`(`copy $(TargetPath) ../../../bin/`)刷新 `bin\*.exe` —— 节点正是从 `bin\` 拉起(`cpp_nodes.ps1`:`$BinDir = RepoRoot\bin`),所以无需手工搬运。基础设施 mysql/redis/etcd/kafka/tidb 均在监听。用 `启动服务器.cmd` 拉栈。

> ⚠️ **必须走 `启动服务器.cmd` / `start_game.ps1`,不要图省事裸调 `dev_tools.ps1 -Command cpp-node-start`。** 09-13 本次验收里我这么干了一次,三个节点全部瞬死:退出码 `0xC0000135`(STATUS_DLL_NOT_FOUND),stdout/stderr **0 字节**、连 muduo 日志文件都没生成 —— 零线索。原因是 `start_game.ps1` 在启动 C++ 节点前做了两件裸调不会做的事:
> ```powershell
> $env:PATH = (Join-Path $serverRoot 'third_party/grpc/install_vs2026_dbg/bin') + ';' + $oldPath
> $env:RPC_PORT = '20010'   # 仅 battle:它与 scene 的自动端口基址相同,本机必须显式错开
> ```
> 节点依赖的 `zlibd.dll` 等 gRPC/protobuf 调试期 DLL **只存在于 `third_party/grpc/install_vs2026_dbg/bin`,不在 `bin/`**。这与 §6.5 里测试跑不起来的那条是**同一个根因**(同一个 DLL、同一个目录、同一个静默 `0xC0000135` signature)。
>
> 顺带排除两个当时看着很像的假线索,免得下次重走弯路:① 与日志级别无关(把 `LogLevel` 改回 2 后照样起不来);② 与 etcd 残键无关 —— `cpp-node-stop` 是 `Stop-Process -Force` 硬杀、不吊销租约,旧键会按 `NodeTTLSeconds: 180` 滞留三分钟,理论上会触发 node-id 抢占 FATAL,但等到残键自然过期后节点**依旧**起不来。真凶自始至终只有 PATH。

**复现原缺陷的最小步骤**(修复前必崩,修复后必不崩)。

> ⚠️ **本节步骤已于 09-13 修正,早先写的"`cpp-node-stop` 停掉节点"复现不出来。** `cpp_nodes.ps1` 的 `Invoke-Stop` 执行的是 `Stop-Process -Id $procId -Force`,即 TerminateProcess 硬杀:进程瞬间消失,不执行任何优雅停机逻辑。gate 侧随即从 **socket 断开**感知到对端消失,走 `handleClose()` → `setState(kDisconnected)` → `closeCallback_` → `TcpClient::removeConnection` → `connectDestroyed`。这条路**会**把状态正确置为 `kDisconnected`,此后再销毁 `RpcClient` 完全无害 —— 因此硬杀节点撞不出这个缺陷。
>
> 崩溃窗口的成立条件是:**实体销毁发生在 TCP 连接仍为 `kConnected` 时**。而销毁的触发源与 socket 无关 —— 它来自 **etcd watch 的 DELETE 事件**:`etcd_service.cpp:384` `HandleDeleteEvent(event.kv().key(), event.prev_kv().value())` → `:340` → `Node::HandleServiceNodeStop`(`node.cpp:1180`)→ 按 uuid 扫描 → `DestroyEntity` → `~RpcClient` → `~GameChannel`(先释放自己那份引用)→ `~TcpClient`。节点键是租约绑定的(`RequestNodeLease` + keepalive,无显式 delete-on-shutdown),所以键消失的正常路径是**租约到期**。要在连接仍活着时打开这个窗口,最确定的办法是**在节点存活且已连接的状态下直接删掉它的 etcd 键**。

1. 栈起来后,让 gate 与某个节点(scene 或 battle)至少交换一条 RPC,使 gate 收到的**最后一条**入站 RPC 来自该节点链路 —— 这一步决定了 `tlsRpc.conn`(修复前)恰好指向这条即将被销毁的连接。
2. 查出该节点的 etcd 键。键格式由 `EtcdManager::MakeNodeEtcdKey` 决定(`etcd_manager.cpp:55`):
   `<eNodeType_Name(node_type)>.rpc/zone/<zone_id>/node_type/<node_type>/node_id/<node_id>`
   直接列出实际键最稳妥:
   ```
   docker exec etcd etcdctl get --prefix "" --keys-only
   ```

   > ⚠️ **在 Git Bash 里执行 etcdctl 时,以 `/` 开头的键会被 MSYS 路径转换静默改写成 Windows 路径,加引号也拦不住。** 实测 `etcdctl put /_probe_selftest v1` 实际写入的键是 `D:/Program Files/Git/_probe_selftest`。节点键本身不以 `/` 开头(形如 `GateNodeService.rpc/zone/1/...`),所以本复现不受影响;但同一个 etcd 里的 `/login/...`、`/match/...`、`/scene_manager/snowflake_guard/...` 等键都以 `/` 开头,操作它们必须加 `MSYS_NO_PATHCONV=1` 前缀,或改用 PowerShell。
3. **在该节点进程仍在运行、连接仍是 kConnected 的前提下**删掉这个键:
   ```
   docker exec etcd etcdctl del "<上一步查到的完整键>"
   ```
   gate 的 watch 收到 DELETE → `HandleServiceNodeStop` → `DestroyEntity`。此时连接还活着,`~TcpClient` 就在这一刻发生。
4. 再向 gate 发任意一条消息(客户端登录一次即可),让派发层覆盖那个 thread_local 槽位(修复前),从而丢掉最后一个强引用。

**观察点**(gate 日志 / 调试器):

- 修复后 `~TcpClient` 应触发 `forceClose`,随后出现 muduo `TcpConnection::dtor[...] state=kDisconnected`(DEBUG 级别);Debug 版**不再**断在 `TcpConnection.cc:71`。
- 修复前同样步骤在第 3 步断在 `TcpConnection.cc:71`,`this->name_` 形如 `RpcClient:<ip:port>#N`(TcpClient 命名;TcpServer 侧是 `-` 连接),`state_ == kConnected(2)`。

> ⚠️ **默认配置下看不到 `TcpConnection::dtor[...] state=kDisconnected`**:那是 `LOG_DEBUG`,而 `LogLevel: 2` 实际是 muduo 的 INFO(见 §7.9,注释与实际语义错开一档)。不必为此改配置重启 —— INFO 级别有一条**更强**的正向判据,见下。

**实测结果(09-13 04:44,`LogLevel: 2` 默认配置,未开 DEBUG)**:靶子选 scene —— 实测 gate 唯一的出站 muduo `TcpClient` 就是连 scene(`Connecting to TCP node ... Port: 20000`),其余对端(login 5 / match 16 / scene_manager 25 / data_service 26 / client_rpc_router 29,**含 battle 28**)全是 `Connecting to GRPC node`,删它们的键不会触发 `~TcpClient`。删键时 scene 进程(pid 7956)与该 TCP 连接均存活。gate 日志新增:

```
16:44:35.332747 INFO  Service node stop, key: SceneNodeService.rpc/zone/1/node_type/3/node_id/1
16:44:35.333640 INFO  Service node stopped : node_type: 3
16:44:35.402969 INFO  TcpClient::~TcpClient[RpcClient] - connector 0x1D3B2139430
16:44:35.404122 WARN  Disconnected from server at 127.0.0.1:20000.        <- rpc_client.h:166
16:44:35.404175 INFO  Client disconnected: 127.0.0.1:20000
16:44:36.089269 INFO  Node added, type: 3, uuid: 819aa3e2-…(同 uuid,scene 自愈重注册)
16:44:36.090344 INFO  TcpClient::TcpClient[RpcClient] - connector 0x1D3B2DCDC50(新建)
```

**判据不是"没崩",而是 `.404122` 那条 `Disconnected from server` 出现在 `~TcpClient`(`.402969`)之后。** 它由 `RpcClient::onConnection` 在 `conn->connected()==false` 时打出,而该 `connectionCallback_` 只能由 `TcpConnection::handleClose()` 触发 —— 也就证明 `~TcpClient` 里 `connection_.use_count()==1` 成立、`unique` 为真、`forceClose()` **确实被调用**,连接走完了 `kConnected → kDisconnected`。修复前 `tlsRpc.conn` 多占一个引用会让 `unique` 为假、`forceClose` 被跳过,**这一行根本不会出现**,连接带着 `kConnected` 悬到下一次派发覆盖 thread_local 槽位时才在 `TcpConnection.cc:71` 炸开。

gate(pid 52724)与 scene(pid 7956)全程存活,新增日志中无 FATAL、无 assert;scene 于 0.75s 后以同一 uuid 自愈重注册,gate 重建了新的 `TcpClient`(connector 地址已换),链路自动恢复 —— 说明这条修复没有把正常的"节点摘除再上线"路径改坏。

### 6.4 回归测试(已加入统一测试入口,红态已实测)

新增 `cpp/tests/rpc_controller_test/`(已登记进 `tools/scripts/run_cpp_tests.ps1` 的工程表)。锁定的不变量:**一次 `GameChannel` 派发结束后,不得在任何长生命周期存储里留下这条连接的强引用。** `TcpConnection` 是具体类不可替身,且在 Debug 下必须走完 kConnected → kDisconnected 才能析构,所以用 `TcpServer + TcpClient` 在 `EventLoopThread` 上真连一次回环,server 侧连接即被测对象;消息用生产 codec 的 `fillEmptyBuffer` 按线上帧格式编码,从 `HandleIncomingMessage`(生产入口)喂入,比较派发前后 `use_count()`。

| 用例 | 断言 | 修复前(链 09-10 旧 `core.lib`) | 修复后(链重建后的 `core.lib`) |
|---|---|---|---|
| `LoopbackFixture.DispatchLeavesNoStrongRefBehind_UnknownType` | 派发前后 `use_count` 相等 | **FAIL:before=3, after=4** —— 正是 `tlsRpc.conn` 多出的那一个 | **PASS** |
| `LoopbackFixture.DispatchLeavesNoStrongRefBehind_RequestWithInvalidMessageId` | 同上,走 `ProcessMessage` → `SendErrorResponse(INVALID_REQUEST)` 真发一条 RPC_ERROR | —(见下注) | **PASS** |
| `LoopbackFixture.HarnessDetectsThreadLocalRetention` | 自证:窗口内复刻"thread_local 存连接",`use_count` 必须 +1 | —(未跑到) | **PASS**(+1 被捕获) |
| `RpcControllerFrom.ReturnsTheVeryObjectPassedToCallMethod` | `From(&ctx) == &ctx` | —(未跑到) | **PASS** |
| `RpcControllerFromDeathTest.NullControllerIsFatal` | `From(nullptr)` 触发 `LOG_FATAL` | PASS | **PASS** |
| `RpcControllerFromDeathTest.ForeignControllerTypeIsFatal` | 异类 controller 触发 `LOG_FATAL` | PASS | **PASS** |

最终 **6/6 全过**(`run_cpp_tests.ps1` 表内一行 `rpc_controller_test OK 6/6 全过`)。红态是**真实跑出来的**,不是推断:首次链接恰好吃到了尚未重建的 09-10 `core.lib`(内含 `tlsRpc.conn = conn`),测试当场抓出多出的那一个引用;换成重建后的库,同一条用例转绿。

> 注:REQUEST 用例最初用 `message_id = 0`,在新旧库上都让进程访问违例。原因与修复无关:`gRpcMethodRegistry` 由节点启动时的 `Init*ServiceMetadata` 填充,单测进程里它是全空默认数组,`RpcMethodMeta::serviceName / methodName` 是 `const char* nullptr`,合法 id 会走到 `services_->find(meta.serviceName)` 与 `SendErrorResponse` 里的 `std::string(nullptr)`。改用越界 id(`kMaxRpcMethodCount`)后,`IsValidMessageId / RecordRecv / RecordSend / LogMessageStatistics` 四处边界检查都命中,路径完整且不碰注册表内容。这也顺带说明:**任何在单测里驱动 `ProcessMessage` 合法 id 的测试都必须先填充注册表**,否则崩在 nullptr 而不是业务逻辑。

### 6.5 现有 gtest 套件

`tools/scripts/run_cpp_tests.ps1`(统一入口)在本机首次整体跑通前暴露了三个与本次改动无关、但会让"跑过测试"变成假话的问题,前两个已随本次修好:

1. **`zlibd.dll` 从不被暂存**:脚本只从 `bin/` 拷 DLL,而 `zlibd.dll` 只在 `third_party/grpc/install_vs2026_dbg/bin/`(节点静态链 zlib)。凡链进 `game_channel` / `ProtobufCodecLite`(帧尾 adler32)的测试都会以 `0xC0000135` 静默退出、零 gtest 输出。已加目录回退。
2. **一个 exe 启动失败会吞掉整张表**:`Start-Process` 报 `Access is denied`(本次是 `agones_lifecycle_test.exe`,疑为安全软件瞬时拦截,稍后重跑 24/24 正常)在 `$ErrorActionPreference='Stop'` 下直接中止脚本。已加 try/catch,记一条 FAIL 继续跑。
3. **`lib/*.lib` 漂移**(§6.1 注、§7.8):`-Build` 只重建测试工程自身,不重建它们链接的引擎库;库一旦落后于源码,测试要么链不上(本次 43 个未解析符号),要么**链着旧代码却显示全绿**。这是比前两条更危险的假绿来源。

基线(run-only;exe 均为修复前旧库所链,反映套件自身健康度):17 个工程全过;`cool_down_time_test` 9/10(`CoolDownTimeMillisecondUtilTest.Reset` 既有失败);`bag_test` / `missions_test` / `turn_battle_engine_test` 无 exe(构建失败)。这三项的根因已查实并全部归零,见上表与其后的分析 —— 均为 lib 漂移,其中 `turn_battle_engine_test` 是 ODR 违反(漏建 `battle.lib`),不是任何测试自身的缺陷。

重建 `proto / rpc / thread_context / core` 四个库后全套 `-Build`(22 个工程全部重链、重跑):

| 结果 | 工程 |
|---|---|
| 全过(19) | agones_lifecycle 24/24 · aoi 32/32 · buff 1/1 · configuration_table 39/39 · **cool_down_time 10/10**(基线里那条 `Reset` 失败随重链消失 —— 它本身就是 lib 漂移,不是产品缺陷)· cross_zone 7/7 · currency 24/24 · message_limiter 3/3 · node_sequence 2/2 · proto_field_checker 9/9 · readfile2string 4/4 · reward 5/5 · **rpc_controller 6/6** · skill 13/13 · snow_flake 27/27 · time_meter 7/7 · time_util 6/6 · timer_destroy 1/1 · timer_queue_unit 14/14 |
| 构建失败(2) | `bag_test`:`LNK2019 BagService::…` 未解析;`missions_test`:`LNK2019 Mission…` 未解析 —— 均为 09-11 提交改动过的 bag / mission 系统符号,对应 `lib/modules.lib`(09-10)/ `lib/scene.lib`(09-10)仍未重建 |
| 运行崩溃(1) | `turn_battle_engine_test`:退出码 `0xC0000374`(堆损坏),疑似崩在 `SameSeedSameCommandsProduceIdenticalEvents` —— 用新头编译、链旧 `modules.lib / scene.lib` 的 ABI 错位是典型成因 |

三个异常项全部指向同一根因 —— 测试直接链接的预制库落后于 09-11/09-12 的源码与生成代码 —— 并已逐一复测归零:

| 工程 | 修复前 | 重建相关库后 |
|---|---|---|
| `bag_test` | 链接期 LNK2019(`BagService::…` 未解析) | **214/214 全过** |
| `missions_test` | 链接期 LNK2019(`Mission…` 未解析) | **16/16 全过** |
| `turn_battle_engine_test` | 退出码 `-1073740940`(`0xC0000374`) | **93/93 全过** |

> 前两项在重建 `proto / rpc` 后即恢复。第三项**没有**这么简单,值得单独记一笔,因为它正是 §7.8 那条风险的实证,而且第一轮排查里我自己就踩了它:
>
> - 早先"三个异常项与本次改动没有任何符号关联"的判断**下得太早**。`turn_battle_engine_test` 确实链接了 `core.lib` / `thread_context.lib` / `rpc.lib`,即本次改动动过的库,所以"无符号关联"这个理由并不成立,不能据此免责。
> - 两个现场看似矛盾,其实同源。cdb 下栈顶是 `failwithmessage ← _RTC_StackFailure ← _RTC_CheckStackVars ← TurnBattleEngine::InitPlayers+0x4c7` —— 这是 MSVC `/RTC1` 的**栈变量越界**检查,不是堆损坏;无调试器直接跑则晚一步炸在 `battle_data.pb.cc:5188` `Check failed: this_.GetArena() == nullptr`(`BattleActorState::SharedDtor` 里的 `ABSL_DCHECK`)。
> - 真因:`turn_battle_engine.cpp` 只被 `cpp/libs/services/battle/battle.vcxproj` 编译进 `lib/battle.lib`(时间戳 **09-09 22:02**),而 `battle_data.pb.h` 在 **09-12 02:45** 重新生成、新增了 `pet_table_id_` 字段。**`battle` 从头到尾就不在我的重建名单里。** 于是 `InitPlayers` 里那个栈上的 `BattleActorState actor;` 按**旧的、更小的**布局开空间,而重建后的 `proto.lib` 里的构造/析构按**新布局**写 —— 写出了栈对象边界。ODR 违反,`/RTC1` 与 arena 断言只是它的两个下游表现。
> - 重建 `lib/battle.lib` 后重链,一次 93/93 通过。**与 RpcController 改动无关。**
>
> 另需注意连带影响:`cpp/nodes/scene` 与 `cpp/nodes/battle` 都链接 `battle.lib`(`cpp/nodes/gate` 不链),所以在 `battle.lib` 重建之前产出的 `bin/scene.exe`、`bin/battle.exe` 带着同一个错配,必须重链后才能用于运行时验收。
>
> 已核查这不是普遍问题:09-11 之后重新生成的 proto 头只有 6 个(`battle_data / bag_quest_mail_data / mysql_database_table / player_activity / player_bag / player_mission`),而仍陈旧的其余自有库(`muduo / proto_helpers / session / gate / config / infra`)一个都没有包含它们。

**最终整轮实测(09-13 04:23,退出码 0)**:全部库重建齐备后又完整跑了一遍 `run_cpp_tests.ps1 -Build`,**22 个测试工程全部通过,无一例外** —— `bag_test` 214/214、`turn_battle_engine_test` 93/93、`missions_test` 16/16、`rpc_controller_test` 6/6、`configuration_table_test` 39/39、`aoi_test` 32/32、`snow_flake_test` 27/27、`agones_lifecycle_test` 24/24、`currency_test` 24/24,余者见上表。至此本次改动的测试面全绿,三个异常项均已证实为 lib 漂移而非产品缺陷。

## 7. 遗留与后续(按真实风险排序)

### 7.1 【should-fix】握手重试定时器按值捕获出站连接 —— 与本事故同形的 B 类隐患

[registration_manager.cpp:43](../../cpp/libs/engine/core/node/system/registration/registration_manager.cpp) `RunAfter(0.5, [conn, nodeType, clientCopy]() { ... })` 把**出站** `TcpConnectionPtr` 和一个强 `RpcClientPtr` 都按值捕获进 muduo Timer 的闭包。在连接建立后的 0.5 s 窗口内,若 `RpcClient` 的最后一个持有者被释放而闭包里的 `conn` 仍活着,`~TcpClient` 看到 `use_count()==2` → 跳过 `forceClose` → 与本事故完全相同的 assert / UAF。今天不炸只因为 (a) EnTT 3.13.2 按池创建的逆序销毁组件(`TimerTaskComp` 池晚于 `RpcClientPtr` 池创建,所以闭包先被释放),(b) 闭包成员析构顺序恰好有利 —— 两者都是**未规定的**行为。`:148` 的 `[clientCopy, nodeType]` 同理(无 conn,但把 `RpcClient` 寿命拖过 `DestroyEntity`)。

**修法**:改弱捕获,回调内 `lock()`:`std::weak_ptr<TcpConnection> weakConn = conn; std::weak_ptr<RpcClient> weakClient = client;` → `RunAfter(0.5, [weakConn, weakClient, nodeType]{ auto c = weakConn.lock(); auto cl = weakClient.lock(); if (!c || !c->connected() || !cl || !cl->connected()) return; ... })`。或在回调里按 (registry, entity) 重新查 `RpcClientPtr`(定时器就挂在同一实体上)。改动局部,建议作为紧随本次的独立变更。

### 7.2 【should-fix】`GameChannel::connection_` 对出站连接是强引用,且 DOWN 时不清

[game_channel.h:87](../../cpp/libs/engine/core/network/game_channel.h) 在 UP 时由 [rpc_client.h:145](../../cpp/libs/engine/core/network/rpc_client.h) `channel_->SetConnection(conn)` 设置,DOWN 分支(rpc_client.h:153)只翻 `connected_`,从不清空。它今天不触发 `~TcpClient` 的 unique-skip,**只**因为 (a) `RpcClient` 把 `client_` 声明在 `channel_` 之前(析构时 channel 先释放引用),(b) `channel_` 是 `GameChannel` 的唯一持有者(消息回调绑的是裸指针)。把这两个成员调换顺序、或在别处拷一份 `channel_`,本事故原样重演;同时 DOWN 到下一次 UP 之间它钉住一条已死连接和其 fd。

**修法**(二选一):`connection_` 改 `std::weak_ptr<TcpConnection>`,`game_channel.cpp` 四处发送点改 `if (auto c = connection_.lock()) codec_.send(c, msg); else LOG_WARN ...`,`HandleRpcMessage` 首行改 `assert(conn == connection_.lock())`;`RpcSession` / `RpcServer` 用服务端连接构造 `GameChannel` 的路径同样适用且安全。**最小替代**:DOWN 分支加 `channel_->SetConnection(nullptr)`,并在 `rpc_client.h:176-177` 加注释 / `static_assert` 钉死 `client_` 先于 `channel_` 的顺序。

### 7.3 【should-fix / 待证实】`Node::disconnectedClientList` 只进不出

[node_connector.cpp:112](../../cpp/libs/engine/core/node/system/node/node_connector.cpp) 在 uuid 重注册时把旧 `RpcClientPtr` `push_back` 进 [node.h:252](../../cpp/libs/engine/core/node/system/node/node.h) 的列表,全仓只有一个 getter 和这一处 push,**从不 drain**。后果一:旧 `RpcClient`(`enableRetry` 仍开)及其连接泄漏到进程结束。后果二(复核方标注为**未证实**,需验证):`~Node` 时 `~TcpClient → forceClose → queueInLoop(forceCloseInLoop)` 排进一个已经退出 `loop()` 的 `EventLoop`,pending functor 随 `pendingFunctors_` 一起销毁时连接处于 `kDisconnecting`,可能在**进程退出**时命中同一个 `TcpConnection.cc:71` 断言。

**修法**:被替换客户端的连接报 DOWN 时(或短定时器后)从列表擦除;`Node` 关停时对每个存活 `RpcClient` 调 `disconnect()`/stop,让连接在 loop 仍运行时到达 `kDisconnected`。**验证方法**:Debug 下让 scene 做一次 uuid 重注册后停 gate,看退出路径。

### 7.4 【hygiene】`RpcSession::connection` 强引用 —— 已证明免疫,重新归类

[rpc_session.h:59](../../cpp/libs/engine/core/network/rpc_session.h) 由 `OnNodeHandshake` 存进 ECS 组件,是跨回调的强引用,形式上与本事故同一反模式。**但它恒为 TcpServer 侧入站连接**:其构造函数 `boost::any_cast<GameChannelPtr>(conn->getContext())`,而只有 `RpcServer::onConnection`(rpc_server.cc:67)会设置这个 context —— 出站连接进来会直接 `bad_any_cast`,根本注册不上。TcpServer 拆连无条件走 `connectDestroyed`,状态必先变 `kDisconnected`,因此**对本 assert 免疫**。修复第一版的头文件注释曾把它列为"剩余的 weak_ptr 待办",复核表明真正危险的残留是 §7.1,注释已改正。

仍值得做的是卫生问题:没有 DOWN 钩子移除 `RpcSession`,一条已死节点链路会把连接对象、fd 记录和 `GameChannel` 钉到重注册或 node-stop 事件为止。**建议**:`connection` 改 `std::weak_ptr` 并在 `IsConnected()` / 四个发送方法里 `lock()`;和/或在 `RpcServer` 的 DOWN 回调(rpc_server.cc:69)移除该组件。消费点集中在 `player_message_utils.cpp`、`node_message_utils.cpp`、`scene_handler.cpp:661`、`player_lifecycle.cpp:101`,均经 `RpcSession` 的方法访问,改动可控。优先级低于 §7.1 / §7.2。

### 7.5 【已完成】`tlsRpc` 的路由 / 会话字段

原 `route_message_response_handler.cpp:22-26` 写 `SetNextRouteNodeType / SetNextRouteNodeId / SetCurrentSessionId` 并 `defer` 复位,但全仓没有任何 getter 调用点 —— 死写。本次随 `tlsRpc` 一并移除;三个标量字段的**声明**保留在 `RpcController` 上作为 per-call 数据的归宿,重量级且无读者的 `routeData_` / `routeMsgBody_` 不再携带。

### 7.6 【note】muduo vendored protorpc 的 `NULL` 派发点

`cpp/libs/engine/muduo_windows/src/muduo/net/protorpc/RpcChannel.cc:140` 以 `service->CallMethod(method, NULL, ...)` 派发,该目录参与编译,但仓库从未实例化 `muduo::net::RpcChannel`、也未在其上注册本项目的 Service(`node.h` 里的 `muduo::net::RpcServer` 是本仓库自己的类,包装 `GameChannel`)。不可达;若要杜绝隐患,可把 protorpc 排除出构建。`RpcController::From` 的 `LOG_FATAL` 已能在它被误接入时立即暴露。

### 7.7 【已核实】各节点的暴露面

- **gate**:唯一持有出站 muduo TcpClient 节点链路(`SceneNodeService`)的节点,B 类隐患只在这里可达。
- **scene**:连接白名单只有 gRPC(SceneManager、DataService),没有出站 TcpClient,`RpcClientPtr` 视图为空。
- **battle**:`cpp/nodes/battle/main.cpp:183` `Node::CanConnectNodeTypeList{}` 为空,节点间只走 gRPC;其 `BattleRoom::directConnByPlayer` 是自身 TcpServer 的入站连接(A 类),且 `BattleClientEdge::DirectSession` 已经用 `weak_ptr` —— 这是应当推广的写法。

### 7.8 【should-fix / 基础设施】测试工程与 `lib/*.lib` 的隐性耦合

`cpp/tests/*` 全部无 ProjectReference,直接链接 `lib/` 下预制的 `core.lib / rpc.lib / proto.lib / thread_context.lib / battle.lib …`;`run_cpp_tests.ps1 -Build` 不会重建这些库。结果是:引擎源码改了、测试却可能链着旧库全绿(本次若不是恰好拿旧库做红态验证,不会发现)。

本次这条风险出现了**两种**形态,后一种比前一种危险得多:

1. **链不上** —— 符号对不上,LNK2019/LNK2001,至少会当场报错(`bag_test` / `missions_test`)。
2. **链得上但布局不一致** —— 同一个类在两个 TU 里 `sizeof` 不同(ODR 违反),链接器不报错,运行期表现为栈/堆被写坏,现场还会漂到离真因很远的地方(`turn_battle_engine_test`:`/RTC1` 报 `InitPlayers` 栈越界,或 protobuf 报 `GetArena() == nullptr`)。**一次 proto 头重新生成 + 一个漏掉的库,就足以造出这种问题。**

**建议**:要么给测试工程加 ProjectReference 让 MSBuild 顺依赖重建;要么在 `run_cpp_tests.ps1 -Build` 前置一步按固定顺序重建 `proto → rpc → thread_context → core → table → modules → battle → scene → …`(**`battle` 本次就是漏掉的那个**),并在汇总表里打印所链各库与最新源码/生成代码的时间戳对比,落后即标红 —— 尤其要拿 `cpp/generated/proto/**/*.pb.h` 的 mtime 作为基准,任何早于它的库都不可信。

### 7.9 【should-fix / 配置】`LogLevel` 的注释与实际语义相反

`bin/etc/base_deploy_config.yaml:25` 写着 `LogLevel: 2 # ... 0=DEBUG，1=INFO，2=WARN，3=ERROR，4=FATAL`,但 `Node::InitLogSystem`(`node.cpp:1092-1098`)是**裸转换、没有任何映射表**:

```cpp
auto logLevel = static_cast<muduo::Logger::LogLevel>(
    tlsNodeConfigManager.GetBaseDeployConfig().log_level());
muduo::Logger::setLogLevel(logLevel);
```

而 muduo 的枚举是 `TRACE=0, DEBUG=1, INFO=2, WARN=3, ERROR_=4, FATAL=5`(`Logging.h:20-28`)。所以配置值与注释**整体错开一档**:当前的 `2` 是 **INFO** 而不是注释说的 WARN;想要 ERROR 的人按注释填 `3`,实际拿到的是 WARN。09-13 实测佐证:运行中的 gate 日志 INFO 141 行、DEBUG 0 行、TRACE 0 行。

影响面不止于误解:本次运行时验收原本要观察的 `TcpConnection::dtor[...] state=kDisconnected` 是 **LOG_DEBUG**,在默认配置下根本不会打 —— 谁按 §6.3 去看这条,会以为"没打就是没走到",从而误判。**建议**:把注释改成 muduo 的真实枚举,或在 `InitLogSystem` 里加一张显式映射表把配置语义与 muduo 解耦(后者更好,配置不该泄漏第三方库的枚举顺序)。

## 8. 规则(建议写进 AGENTS.md / 代码规范)

1. **禁止**把 `TcpConnectionPtr` 强引用存进 thread_local、全局、定时器闭包或任何寿命长于单次回调的容器;需要跨回调引用连接,一律 `std::weak_ptr` + `lock()`。
2. 单次 RPC 调用需要的上下文(所在连接、会话、路由、peer 信息)一律放进 `RpcController`,随 `CallMethod` 显式传递;**不要**再开 thread_local 存连接类字段。
3. 新增任何 `service->CallMethod(` 派发点必须构造并传入 `RpcController`;`RpcController::From()` 对 `nullptr` / 异类 controller 在 Debug 与 Release 下都 `LOG_FATAL` —— 这是有意的 fail-fast,不要绕过。
4. **不要在一条消息的回调里同步释放该连接所属 `RpcClient` 的最后一个引用**;节点摘除必须经 `queueInLoop` 延后到派发之外(现状如此,保持)。
5. 全仓**不得** `using namespace google::protobuf`(本项目的 `RpcController` 与基类同名是有意的);需要基类时全限定写 `::google::protobuf::RpcController`。
6. 拆除持有 `TcpClient` 的对象前,显式 `forceClose()`,不依赖 muduo 的 unique 判断。
7. 改了 `cpp/libs/**` 或重新生成了 proto / 表之后,跑单元测试前先以 Build 目标重建对应的 `lib/*.lib`(至少 `proto`、`rpc`、`thread_context`、`core`、`table`、`modules`、`battle`、`scene`),否则测试链的是旧代码;`ClCompile` 定点编译只证明"能编",不更新库。**重新生成过 `*.pb.h` 时尤其不能只重建"我改过的"库** —— 任何在栈上或按值持有该 proto 消息的库都必须一起重建,否则是 ODR 违反:链接器不报错,运行期才以栈越界 / arena 断言的形式炸开,且现场离真因很远。

# 玩家级 handler 延后完成(PlayerCallContext)

> 状态(2026-10-15,Claude):**设计已定,暂不实施**。三个独立方案经评审选定(§2),但全仓**没有合理的首个调用方**(§1),
> 按 AGENTS.md §11.2 YAGNI / §11.5 第 2 条,机制与测试都不先写;满足 §7 触发条件时与首个调用方同批实施。
> 上游:`docs/design/muduo-rpc-channel.md` §2 非目标、§5.2(延后不保证次序)、§14 第 1 项;gRPC 下游见
> `docs/design/grpc-muduo-transport.md` §16.1。本文全部来自静态阅读,行号以 2026-10-15 工作区为准。

## 0. 一句话

让玩家级 handler 可以「先不回、等下游(gRPC 每次调用回调 / RPC0 CallMethod)完成再回」,不阻塞 EventLoop;
没选择延后的 handler 零变化。形状:每个生成的玩家 handler 显式多一个 `PlayerCallContext& callCtx`,
`callCtx.TryDefer(*response)` 返回可拷贝的延后句柄,入口 `ProcessClientPlayerMessage` 只在 handler 选择延后时才
`RpcDoneCallback::Adopt(done)`。

## 1. 为什么暂不做:没有合理的首个调用方

| 候选 | 结论 | 依据 |
|---|---|---|
| `GmQueryTransactionLog` / `GmTraceItem` 及其余 P1-B 桩(`player_rollback_handler.cpp:74-224`) | 不合理 | `SceneRollbackClientPlayer` 未标 `OptionIsClientProtocolService`(`player_rollback.proto:204-206`),gate 直接拒(`client_message_processor.cpp` 客户端消息白名单;`gate_gm_client_messages.h:34-37` 明确排除);生产两道 GM 闸都拒(`scene_handler.cpp` GM 闸、handler 内闸);请求无目标 player_id;字段与 data_service 契约对不上(`item_uuid` / 单个 `tx_type` 对 `item_config_id` / `repeated tx_types`),`GmTraceItem` 在 data_service 没有对应 RPC;线上运维面是带 HMAC 的 scene_admin / data_service。在一条按设计关闭的路径上填活代码等于给后门补功能 |
| `EnterScene` / `TravelToZone` | 不该延后 | 契约是「应答 = 已受理,结果走推送」(`player_scene_handler.cpp`);交接链要经暂拒、存盘、重发,SceneManager 一跳就 10000ms,突破 I5;TravelToZone 成功后客户端离开本 gate,延后应答送不到;改回包时机是客户端可见变更,需两版同批 |
| `SceneInfoC2S` / `GetSyncDigest` | 需先改客户端协议 | 有异步下游,但应答是本地 `Empty`,gate 单向发送 |
| 其余玩家 handler | 不等下游 | player 目录除上述外无 gRPC / Redis 调用 |

推迟几乎零代价:`RpcDoneCallback`、gRPC 每次调用回调 `GrpcDone`(如 `SendDataServiceQueryTransactionLog` 已有该重载)、
gate 的 12s 预算(`kGateToSceneClientCallTimeout`)都已就位,届时按本文实施是机械工作。现在实施的代价:接口 + 生成器 + 入口 +
28 个生成文件,且没有调用方就无从验证接口形状(要不要保活 request、identity 够不够、要不要单飞、Redis 截止怎么给)。

## 2. 选型

三个独立方案(评审 0-10 分):

- **A 显式上下文形参(8 分,选定)**:所有生成的玩家 handler 多一个 `PlayerCallContext& callCtx`。与
  `rpc_controller.h` 「调用上下文随调用链显式传递,不落任何 thread_local」同向,与节点级 handler 带 `done` 同构。
- B thread_local 当前调用 + 自由函数 `DeferPlayerReply()`(6 分,否决):生成器零改动,但把请求级 TLS 请回来,违反
  `rpc_controller.h` 与 `player_service_interface.h` 的规则和 AGENTS §11.2 显式依赖;签名上看不出能否延后。
- C proto 方法 option 按方法开启(5 分,否决):服务端内部机制进客户端可见 proto(客户端仓、Java 镜像都得带 options 文件,
  越过 AGENTS §12 的共享边界);生成的派发层要写错误码,违反「生成的 CallMethod 只派发、不搬运错误码」;打标即 Adopt,污染计数。

从 B 嫁接:可单测的生产派发函数 `DispatchClientPlayerCall`、续延按身份复核实体、无法延后时返回 `nullopt`。
从 C 嫁接:RPC0 绑定做成 header-only(少改有他人改动的 vcxproj / CMake)、续延错误码口径。

## 3. 接口

### 3.1 `cpp/libs/services/player/rpc/player_call_context.h`(新建,header-only)

只依赖 STL、protobuf 与 `core/type_define/type_define.h`;不依赖 network / scene / ECS(player 库不认识 `RpcDoneCallback`,依赖倒置)。

```cpp
class TipInfoMessage;

struct PlayerCallIdentity { Guid playerId{0}; SessionId sessionId{0}; };  // playerId 取入口 SessionMap 解析值,不取 gate 自报

class PlayerDeferredReplyState {            // 由分发入口实现
public:
    virtual ~PlayerDeferredReplyState() = default;
    virtual ::google::protobuf::Message& Response() = 0;
    virtual void Run() = 0;                 // 只有第一次装帧并发出,之后交外层计 reply_run_twice
    virtual bool IsCompleted() const = 0;
    virtual bool IsCallerConnected() const = 0;
};

template <class R> class PlayerDeferredReply {  // 可拷贝,副本共享 state;只在 EventLoop 线程使用
public:
    R& response() const;
    const PlayerCallIdentity& identity() const;
    void Run() const;
    bool IsCompleted() const;
    bool IsCallerConnected() const;
    // 不提供 Fail():玩家层失败一律写 error_message 再 Run;gate 对任何非 ok 都回带 id 的 1003,Fail 对客户端无区别。
    // 不暴露 request():续延只按值捕获需要的字段(同 RpcDoneCallback)。
};

class PlayerCallContext {                   // 栈上;禁拷贝 / 移动 / 存储(同 RpcController)
public:
    const PlayerCallIdentity& identity() const;
    // 只有带 TipInfoMessage error_message 的应答能延后(static_assert);nullopt = 本入口不能延后,handler 同步回码。
    template <class R> [[nodiscard]] std::optional<PlayerDeferredReply<R>> TryDefer(R& response);
protected:
    explicit PlayerCallContext(PlayerCallIdentity);
    virtual ~PlayerCallContext() = default;
    virtual std::shared_ptr<PlayerDeferredReplyState> DeferImpl(::google::protobuf::Message& response) = 0;
};

class SyncOnlyPlayerCallContext final : public PlayerCallContext { /* DeferImpl 恒返回 nullptr */ };
```

### 3.2 `player_service_interface.h`

- 纯虚改为 5 参 `CallMethod(method, player, request, response, PlayerCallContext& callCtx) = 0;`
- 新增非虚 4 参便捷重载:构造 `SyncOnlyPlayerCallContext` 后转调 5 参。节点路由入口(`SendMessageToPlayer`、
  `InvokePlayerService`)与 bag_test 等 29 处单测调用不用改。

> 2026-10-16 补(RpcChannel 批 7):`SendMessageToPlayer`(Scene 1)、`InvokePlayerService`(Scene 40)已退役并从 proto 删除
> (号留墓碑,见 `docs/design/muduo-rpc-channel.md` §20.3),不再调玩家 handler 的 `CallMethod`;实施时 4 参便捷重载的调用方只剩单测,
> 本节与 §3.5 里涉及这两个入口的说法只作留痕,不要去接这两个已退役的入口。

### 3.3 生成物(只改 `player_service_gen.go` 的模板字符串,不改 yaml)

- 类内紧跟 `using PlayerService::PlayerService;` 加 **`using PlayerService::CallMethod;`**(否则 5 参 override 隐藏 4 参重载,单测断编)。
- 静态声明 `static void M(entt::entity player, const Req* request, Resp* response, PlayerCallContext& callCtx);`,形参放末位
  (与节点级 `Closure* done` 末位同构,不影响契约测试的 `X(player,` 计数)。
- override 与 case 派发同步加 `callCtx`,保留原有 `static_assert`;cpp 定义签名同步。守护段按 `Class::Method(` 匹配
  (`code_parser.go`),改签名不丢守护段代码。

### 3.4 `cpp/nodes/scene/handler/rpc/client_player_call_context.h`(新建,header-only)

- `struct ClientPlayerReplyFraming { uint32_t messageId; uint64_t clientRequestId; SessionId sessionId; };`
- `FrameClientPlayerReply(framing, playerResponse, ProcessClientPlayerMessageResponse& out)`:唯一装帧点,从
  `ProcessClientPlayerMessage` 原样搬出(含「本地 `::Empty` 不装帧」与序列化失败日志),输出逐字节相同。
- `[[nodiscard]] bool DispatchClientPlayerCall(service, method, player, playerRequest, playerResponse, framing, identity, done)`:
  返回 true = 已接管,调用方立即 return、不得再碰两个 `unique_ptr`。
- `ClientPlayerCallContext::DeferImpl` 顺序:`done == nullptr` → nullptr(直接调用的测试,也避开 `Adopt(nullptr)` 的 LOG_FATAL);
  `&response != playerResponse.get()` → LOG_FATAL(代码错误);已接管 → 同一 state;首次 → `RpcDoneCallback::Adopt(done)`,
  `playerRequest` / `playerResponse` 的所有权转进 state 的 `shared_ptr`(对象不搬家,handler 手里的裸指针继续有效)。
- `ClientPlayerDeferredState::Run()`:首次装帧进 `outer.response<ProcessClientPlayerMessageResponse>()` 再 `outer.Run()`;析构为空,不碰 ECS / loop。

### 3.5 `scene_handler.cpp`(只改守护段)

`ProcessClientPlayerMessage`:两个 `MessageUniquePtr` 去 const;派发改为
`else if (DispatchClientPlayerCall(...)) { return; }`;末尾装帧改为 `FrameClientPlayerReply(framing, *playerResponse, *response)`。
退出闸、GM 闸、`SendMessageToPlayer`、`InvokePlayerService` 不变(后两者走 4 参,延后 handler 拿到 nullopt 同步回码)。
(2026-10-16 补:后两者已于批 7 退役并从 proto 删除,见 §3.2 末注。)

### 3.6 续延复核(随首个调用方加)

`FindLivePlayerForCall(identity)`:`GetPlayer` + `valid` + `GuidOf == playerId`(把 `player_landing.cpp` 的 `FindSamePlayer` 上提为共享
helper,不复制)、会话快照的 `gate_session_id == sessionId`、`!any_of<UnregisterPlayer>`。已离开写 1012、退出中写 1006 再 Run;
写类续延还须重查 `IsCrossZoneFrozen` 与 `InBattleComp`。

### 3.7 预算(随首个调用方加)

新增 `cpp/libs/engine/core/network/rpc_call_budget.h`:`kGateToSceneClientCallTimeout{12000}`(从 gate 迁来)、
`kSceneDownstreamMaxDeadline{10000}`、`static_assert(kSceneDownstreamMaxDeadline + 1000ms <= kGateToSceneClientCallTimeout)`(编译期守住 I5)。
延后 handler 必须**显式**传 `GrpcCallOptions{.deadline}` / `RpcCallOptions{.timeout}` 且不超过上限(`deadline <= 0` 落回部署配置默认值,可能被改大);
串行多跳之和也不得超预算。Hiredis 没有逐命令超时,不能单独作为延后的下游。

## 4. 所有权、生命周期与完成路径

- `callCtx` 只在 `CallMethod` 同步栈内有效;未延后时内层 request / response 由入口栈上 `unique_ptr` 持有(同今天),延后后转进 state。
- 外层 `RpcDoneCallback::State` 只持 `weak_ptr<RpcChannel>`;整条链不持强连接 / 强 channel(AGENTS §11.7)。state 不持实体,续延按 identity 重查并 `try_get`。
- 每个 `id != 0` 的请求恰好一帧:未延后 → 框架补发;延后后 Run → RESPONSE;下游非 OK → 续延写 1003 再 Run;
  句柄全丢未 Run → REPLY_DROPPED(gate 回带 id 的 1003,不必等 12s);TryDefer 后栈内同步 Run → 只一帧;
  超 12s → gate 判 kTimeout,迟到回包计 late_reply(所以首批只允许只读方法);断连 → Run 时 weak 锁不住,计 reply_channel_gone;
  停机 → 阶段 0 先排空 gRPC CQ,在途 `GrpcDone` 以非 OK 先完成,续延里禁止再 `queueInLoop`。

## 5. 次序

沿用 `muduo-rpc-channel.md` §5.2:被接管的消息,同会话后续同步消息先回包;`client-request-ordering.md` 的 O2 对延后方法不再成立。
规则:首批只允许只读或幂等方法;每个延后方法自带业务在途闸(`IsSceneChangeBusy` 写法)并限制单玩家在途数;写类续延必须重新校验状态;
客户端须按 `MessageContent.id` 对号(Q4,由客户端负责人确认)。框架不另设上限(每 channel 65536 已封顶)。

## 6. 测试(实施时)

- Go:新建 `player_service_call_context_test.go`(签名以 `PlayerCallContext& callCtx);` 结尾、override 带 callCtx、
  `using PlayerService::CallMethod;` 恰好一次、派发含 `, callCtx);`、旧签名 cpp 重新生成后守护段逐字节保留)。
- C++:bag_test 一行不改照常通过;新建 `cpp/tests/rpc_controller_test/player_deferred_reply_test.cpp`(登记 vcxproj 并补
  `../../libs/services/player/` include 路径):SyncOnly 返回 nullopt;done 为空返回 nullopt 且 `unique_ptr` 未转走;副本共享状态、Run 两次只装帧一次;
  真实回环(沿用 `rpc_chain_test` 夹具)覆盖同步金样逐字节一致、延后不阻塞 loop、句柄全丢 kReplyDropped、栈内同步 Run 只一帧、
  M1 延后 M2 同步时 M2 先回且各按 id 对上、Run 前断连不出帧且内层对象已释放、超时后迟到 Run 计 late_reply、channel 先 Close 仍出帧。
- 冒烟:`[RpcChannel] summary` 的 `adopted` 在没有延后 handler 时恒为 0(今天生产代码无 Adopt 调用方),作为「零变化」证据。

## 7. 触发条件(全部满足才与首个调用方同批实施)

1. 出现一个客户端可达(在 `IsClientMessageId` 里)、应答非 `Empty` 且带 `TipInfoMessage error_message` 的玩家方法;
2. 它只读或幂等;
3. 结果必须来自 `GrpcDone` 或 `RpcChannel::CallMethod` 下游,且能显式给出不超过 10000ms 的截止;
4. 「受理 + 推送」对它明显更差;
5. 客户端负责人确认按 `MessageContent.id` 对号;
6. Java 版对齐方案已定(纯服务端机制本身 Java 不适用;只有首个调用方改变客户端可见回包时机时才需两版同批)。

## 8. 与并发改动的共存(实施时核对)

- `player_service_gen.go` 有他会话的改动(`hasErrorResponse` 排除 `google::protobuf::Empty`,配套测试在
  `player_service_response_contract_test.go`);本设计只动模板字符串的另几段,原位叠加,不得 checkout / restore。
- 先等该改动提交再 regen:`player_lifecycle_handler.h` 与 `s2s_player_scene_handler.h` 现有的 diff 就是它的产物,改动不在位时 regen
  会把 `google::protobuf::Empty` 的 `static_assert` 生成回来、直接断编。

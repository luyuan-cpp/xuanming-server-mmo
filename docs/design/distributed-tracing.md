# 分布式日志关联与链路追踪（todo.md #152）

> **状态：** 2026-11-06 按代码现状重写，取代 2026-05-14 版的分片计划（那一版只落了 W3C 原语和线程局部上下文）。**阶段一第 1 批（§5.7、§4.1、§5.1）2026-11-06 已落码，11-08 Codex 全量运行 rpc_controller_test 时本批 20 条用例全部通过（证据 `build/verification/naming-audit-20261108/cpp-tests/rpc_controller_test/`）；第 2 批（§5.2）2026-11-10 已落码，未编译、未测试，待 Codex 验证；第 3 批（§5.5、§5.6，外加 `ScopedRoot` 与 `Bind`）2026-11-10 已落码，未编译、未测试，待 Codex 验证；第 4 批（§5.3、§5.5 路由 lane、§6.2 第 1 项）2026-11-10 已落码，未编译、未测试，待 Codex 验证，其中 client_rpc_router 的裸客户端与 scene_manager 的玩家拦截器留待对应文件空出来再接（§6.2）；第 5 批（§5.4、§6.2 的 Kafka 载体与第 2 项）2026-11-11 已落码，未编译、未测试，待 Codex 验证，其中 login 发往 gate 的调用方切换和另外几处 Go 生产、消费点留待对应文件空出来再接，几处不继承请求 ctx 的发送要另改（§6.2）；其余各批尚未落码。**
> **范围：** 本仓的 C++ 节点、Go 微服务、`java/gateway_node`。Java 版服务器守同一份约定、自己实现，见 §12。
> **相关：** 现有采集链路见 [grafana-loki-local-logs.md](../ops/grafana-loki-local-logs.md)；RPC0 信封见 [muduo-rpc-channel.md](muduo-rpc-channel.md) §4.1；muduo_grpc 见 [grpc-muduo-transport.md](grpc-muduo-transport.md)。

---

## 0. 一句话

一个请求从进入系统到落库，沿途每个进程的日志都带同一个 `trace_id`。传递格式用 W3C Trace Context（`traceparent`），三种语言通用；只在框架接缝处接一次，业务代码不改。日志照旧写本地文件或 stdout，由 Alloy 送进 Loki，不自建日志服务器。

## 1. 现状（2026-11-05 核对）

| | 已有 | 断点 |
|---|---|---|
| 采集与查询 | 本地 Alloy → Loki → Grafana，三种语言都采；K8s 上 C++ 由 sidecar 读 muduo 文件 | K8s 上 Go / Java 的 stdout 没有采集器，只在 `kubectl logs` 里，Pod 重建即丢；三种语言的 trace 字段都没提成结构化元数据 |
| Go | go-zero v1.9.2 在 `core/trace` 的 init 里设了 W3C TraceContext + Baggage 传播器；zrpc 服务端和客户端默认挂 tracing 拦截器（各服务 etc 都没关 `Middlewares.Trace`）；`ServiceConf.SetUp` 起 TracerProvider，Sampler 默认 1.0，没配 `Telemetry.Endpoint` 就不导出；带 ctx 的 logx 行已有 `"trace"` / `"span"`（本机 `run/logs/go_services/data_service.stdout.log` 可见） | 5 处裸 `grpc.NewClient`（Go 调 C++ scene 等）没挂拦截器；Kafka 不带 header；C++ 调进来不带 `traceparent`，于是每次都是新根 |
| C++ | `core/tracing/trace_context.h`：W3C 的 TraceId / SpanId、`traceparent` 解析与序列化、`NewRoot` / `ChildSpan`。`thread_context/trace_context_tls.{h,cpp}`：每线程一份 `tlsTrace`、`ScopedSpan`、`TLOG_*` 宏。RpcChannel 发起调用时捕获 `call.trace`，回包和超时回调在 `Deliver` 里用 `ScopedSpan` 恢复 | RPC0 每次派发都开新根（`ScopedDispatchTrace`，`rpc_channel.cpp:121`），信封不带 trace；muduo_grpc、Kafka 都不带；gate 收客户端消息不开根；协程经 `queueInLoop` 恢复后上下文丢失；只有 `TLOG_*` 会给日志加 8 位前缀，而 `TLOG_*` 全仓零调用，所以现在的 C++ 日志里一个 trace 都没有 |
| Java（gateway_node） | Spring Boot 3.4.3，`logging.structured.format.console: logstash`；grpc-java 客户端 | 没接 Micrometer Tracing：日志没有 traceId，出站 gRPC 不带 |

结论：Go 服务之间已经串得起来。断点在三处：C++ 不传、Go 调 C++ 的裸客户端不传、Kafka 不传。另外 K8s 上 Go / Java 的日志还没进 Loki。

## 2. 目标与非目标

**目标**

1. 同一请求跨进程的日志，用一个 `trace_id` 就能查全。覆盖 C++ / Go / Java，以及 RPC0、gRPC、Kafka 三种通道。
2. 三种语言的日志里，`trace_id` / `span_id` / `player_id` 在 Loki 里同名、同口径。
3. 只改框架接缝，业务代码零改动；新服务只要走现有框架，就自动带上。
4. 热路径成本有上限，并且能测出来（§8）。
5. K8s 上三种语言的日志都进 Loki。

**非目标**

- 本期不导出 span，不上 Tempo（阶段三，§10.3）。
- 不自建日志服务器，不把日志经游戏 RPC 发出去。日志写本地文件或 stdout，再由采集器送走；Loki 挂了，游戏不受影响。
- 不改客户端契约：客户端帧、`TipInfoMessage` 都不动。
- 运营、BI、审计流水不在本文范围，见 §2.1。

### 2.1 运营和审计流水为什么不走这套

诊断日志允许丢、允许采样：muduo 后端写不过来时会主动丢日志，并留一条 `Dropped log messages`（grafana-loki-local-logs.md §6.9）；Loki 也只留 7 天。登录、充值、道具与货币流水不行：不能丢、不能采样，要按玩家长期可查，还要能对账。它们走 Kafka → TiDB / ClickHouse 这类有主的存储；经济类已有 `asset_op` 台账和 `transaction_log`。两条管道可以共用 `trace_id` 做交叉索引，但不能互相替代。

## 3. 约定（三种语言共同遵守）

### 3.1 线上传递格式

- 用 W3C Trace Context level 1：`traceparent: 00-<32 位十六进制 trace-id>-<16 位十六进制 parent-id>-<2 位 flags>`。
- 只认小写十六进制。全零 id、版本 `ff`、长度或分隔符不对，一律当作没有。
- 不传 `tracestate`。go-zero 的传播器会顺带处理 `baggage`，但不靠它传任何业务值。

| 通道 | 载体 | 键或字段 |
|---|---|---|
| gRPC（C++ ↔ Go、Go ↔ Go、Java → Go） | metadata（HTTP/2 头） | `traceparent` |
| Kafka（三种语言） | record header | `traceparent`，值同上（ASCII） |
| RPC0（C++ 节点之间） | `GameRpcMessage` 字段 7–10，二进制（§5.2） | — |
| Java 网关的公网 HTTP 入口 | 请求头 | 不接受外部值（§3.3） |
| 客户端 ↔ gate | 无 | gate 开根（§3.3） |

### 3.2 日志字段

| Loki 结构化元数据 | 含义 | C++（行尾后缀） | Go（logx JSON） | Java（logstash JSON） |
|---|---|---|---|---|
| `trace_id` | 32 位小写十六进制 | `trace_id=` | `trace`（go-zero 固定键名） | `traceId`（Micrometer 的 MDC 键） |
| `span_id` | 16 位小写十六进制 | `span_id=` | `span` | `spanId` |
| `player_id` | 十进制 | `player_id=` | `player_id` | 不涉及 |

- 三种语言的原始键名不同，由 Alloy 统一改名（§9.1），不在各语言里硬改：go-zero 的键名改不了。
- 三个字段只进**结构化元数据**，不做 Loki 标签，也不做 Prometheus label（AGENTS §9）。

### 3.3 根从哪里开（信任边界）

- **gate**：每条客户端消息、每次建连和断线事件，各开一条根。客户端帧里没有、也不加 trace 字段，客户端指定不了 `trace_id`。
- **Java 网关的公网 HTTP 入口**：不接受外部 `traceparent`，一律开根。Micrometer 默认会接，要显式关掉（§7）。
- **内部入口**（RPC0 / gRPC / Kafka）：收到合法上下文就接着用（在它下面建子 span）；没有或不合法就开根。**绝不因为 trace 不合法而拒绝或丢弃请求**：追踪不能影响业务结果。
- **后台任务**（周期定时器、tick、巡检）：不挂 trace。

### 3.4 采样

- 三端头部采样一律「采」：C++ 新根的 flags 为 `01`；go-zero 的 Sampler 保持默认 1.0；Java 设 `management.tracing.sampling.probability: 1.0`（Spring Boot 默认是 0.1，要改）。
- 本期不导出 span，flags 只是一个标记，没有成本。真正的取舍放到阶段三的采集器尾部采样（§10.3）。

## 4. C++ 线程局部上下文（正确性的核心）

C++ 的业务逻辑跑在 muduo 的 EventLoop 线程上，一个线程交替处理成百上千个请求，所以上下文必须跟着「当前在处理谁」切换。这一节的规则决定了日志上的 trace 是对还是错。

### 4.1 内容

每线程一份**日志上下文**：`TraceContext` 加 `player_id`（0 表示无）。沿用 `thread_context/trace_context_tls.{h,cpp}`：

- 读：`Current()`、`CurrentPlayerId()`、`CurrentTraceHex()`（48 个十六进制字符，给日志后缀用，§5.1）。
- 装：`ScopedRoot`（不看当前、总是开新根，给入口用）、`ScopedSpan`（有 trace 建子 span，没有就开根）、`ScopedSpan(parent)`（恢复捕获值）、`ScopedPlayerId`；`Set()` 只给作用域类的实现用。
- 包：`Bind(fn)`，记下包装时的 trace，执行时用 `ScopedSpan(captured)` 装回；只给一次性续跑用（§4.3）。
- 线程局部变量放在 `detail` 里，不对外暴露。

### 4.2 安装规则

1. **只用 RAII 作用域安装，离开作用域就恢复进入前的值。** 禁止在入口裸调 `Set()` 而不恢复。2026-05 的 RpcChannel 就是这样，把上一个请求的 trace 留给了之后同一线程上的回调；`rpc_channel.cpp:119` 的注释记着这个坑。
2. 入口只有三种动作：
   - **开根**：§3.3 列出的入口。
   - **接续**：带上下文的入站（RPC0 / gRPC / Kafka），在收到的上下文下面建子 span；不合法就开根。
   - **恢复**：异步回调执行时，装回投递时捕获的上下文。沿用 `ScopedSpan(parent)`，在捕获的值下面建子 span。
3. **宁可丢，不可错。** 丢了上下文，只是这几行日志查不到；挂错上下文，会把人引到另一个请求上。所以只有一次性续跑才捕获，周期任务永不捕获。

### 4.3 异步原语逐个对齐

| 异步原语 | 现状 | 做法 |
|---|---|---|
| RPC0 回包、超时回调 | 已做：发起时捕获 `call.trace`，`Deliver` 里恢复 | 不变 |
| gRPC 客户端完成回调 | 无 | 出站钩子包一层完成回调（§5.3） |
| Kafka 消费（poll 线程 → loop 线程） | 无 | `Work` 带上解析出的上下文，`Run` 时接续（§5.4） |
| 协程（`LoopTaskScope` + boost.cobalt） | 第 3 批已做：`AwaitCallback` 挂起时记下、完成时装回；执行器的投递分支用 `Bind` | 见 §5.6。协程里经 `AwaitCallback` 等的 Redis、etcd 回包因此也接得上 |
| 业务直接调用的一次性 `queueInLoop` / `runAfter` | `Bind` 已有（第 3 批） | 要续上就用 `tracing::Bind(fn)` 包一层；不包就是无上下文（安全，只是查不到） |
| 周期定时器 `runEvery` | — | **永不捕获**：不然发起它的那个请求的 trace，会挂在之后每一次 tick 上 |
| 不经协程的 Redis 回包（muduo contrib `Hiredis`） | 无 | 在引擎封装处（`infra/storage` 下的 Redis 客户端）包一层；没有封装的调用点按 `Bind` 处理。尚未排批 |

## 5. C++ 接缝

### 5.1 日志出口：`Node::AsyncOutput` 统一追加后缀

- **位置**：`cpp/libs/engine/core/node/system/node/node.cpp:2165`。节点初始化之后，所有 `LOG_*`（包括 muduo 库自己打的日志）都从这里出去。muduo 的 `Logger` 析构时在**调用线程**上同步调用 `g_output`（`third_party/muduo/muduo/base/Logging.cc:197`），所以这里读得到线程局部上下文。不改 muduo，也不改任何 `LOG_*` 调用点。
- **格式**：在 muduo 的 ` - 文件:行` 之后、换行之前追加：
  ` [ctx trace_id=<32 位> span_id=<16 位> player_id=<十进制>]`
  没有 trace 就省掉前两项，没有玩家就省掉 `player_id`；两样都没有就原样输出，不拷贝。
- **放行尾，不放行首**：行首的时间、线程号、级别是 Alloy 解析和人工排查的锚点，一个字节都不动（`config.alloy` 的 C++ 规则只锚行首）。后缀带固定的 `[ctx ` 前缀，并且只在行尾匹配，不会跟正文里随手写的 `player_id=` 混淆。
- **必须拼好整行再一次 `append`**：`AsyncLogging::append` 每次调用单独加锁（`third_party/muduo/muduo/base/AsyncLogging.cc:34`），分两次写会和别的线程的行交错。拼接用线程局部缓冲，大小取 muduo 单行上限 `kSmallBuffer`（4000）加上后缀上限，不在堆上分配。
- **超长行**：muduo 的 `LogStream` 放不下就整段丢弃，连 `finish()` 追加的 ` - 文件:行\n` 也可能被丢，所以行尾可能没有换行。这种行照样追加后缀，并补一个换行。
- **十六进制按需生成**：第一次真打日志时才转换，结果缓存在线程局部；缓存记着生成它的 trace_id 与 span_id，每次打日志时比对，变了才重算。多数派发一行日志都不打，就省掉了转换；也不靠各修改路径置脏，所以 trace 无论经哪条路径改动，都读不到过期的缓存。原来的 `RefreshLogPrefix` 每次上下文变化都立即转换；`ToHex` 也改成查半字节表，不再每字节调一次 `snprintf`。
- **退役**：删掉 `TLOG_*` 宏和 8 位前缀缓存 `tlsTraceLogPrefix`（全仓零调用，只在注释里出现），`[trace=xxxxxxxx]` 格式作废。
- Windows 控制台那一路（`LogToConsole`）写同一份拼好的行。

### 5.2 RPC0：信封带二进制 trace

在 `proto/common/base/rpc_message.proto` 的 `GameRpcMessage` 里加 4 个字段：

```proto
  // 链路追踪(docs/design/distributed-tracing.md §5.2):W3C trace-context 的二进制形态。
  // 全 0 = 没有 trace(proto3 不编码,零开销),收方开新根。只在 REQUEST 上填。
  fixed64 trace_id_hi = 7;     // TraceId.bytes[0..7] 按大端拼成的整数
  fixed64 trace_id_lo = 8;     // TraceId.bytes[8..15]
  fixed64 parent_span_id = 9;  // 发送方当前 span;收方在它下面建子 span
  uint32 trace_flags = 10;     // W3C flags,只用最低位 sampled
```

- **为什么用 fixed64，不用 bytes 或 string**：16 字节超过 MSVC 和 libstdc++ 的短串优化上限（15），用 `bytes` 时每帧解析、序列化都要堆分配。随机 64 位值用 varint 编码要 9～10 字节，fixed64 恒为 8 字节。字符串形态的 `traceparent` 要 55 字节。
- **字节序**：按大端拼。这样 `%016llx%016llx` 打出来就和 `TraceId::ToHex()` 一致，抓包能直接对上日志。
- **合法性**：trace_id 全 0 或 parent_span_id 为 0，都当作没有。flags 只取低 8 位。
- **读写只有一处**：`cpp/libs/engine/core/network/rpc_trace_envelope.h` 的 `WriteTraceToRpcEnvelope` / `ReadTraceFromRpcEnvelope`。全仓只有 RpcChannel 构造 REQUEST 帧（2026-11-10 核对）。
- **发送**：`RpcChannel::CallMethodWithResponseType` 和 `RpcChannel::SendRequest` 建信封时写入当前上下文；上下文无效就什么都不写。RESPONSE、RPC_ERROR、HEARTBEAT 不写：回包由调用方自己恢复捕获的上下文。
- **接收**：`CallServiceMethod` 开头读出信封上的 trace，交给 `tracing::ScopedIncoming`：合法就 `ChildSpan`，否则 `NewRoot`，退出时恢复。原来的 `ScopedDispatchTrace` 第 4 批并进了这个通用作用域类，gRPC 入站用的也是它。派发前的拒绝（未知方法、解析失败等）也在这个作用域里，拒绝日志同样带 trace。
- **兼容**：旧收方忽略新字段；旧发方不填，新收方开根。项目未上线，混跑不阻塞。
- 改 proto 后按 AGENTS §4 重新生成（未获客户端任务授权，用 `proto_gen.no-unity.yaml`），并完整编译受影响的 C++ / Go。muduo-rpc-channel.md §4.1 的线格式已同步。
- **测试**：信封读写的纯函数在 `rpc_trace_envelope_test.cpp`；端到端在 `rpc_channel_loopback_test.cpp` 第五节，覆盖客户端帧、对端 handler、回包回调、不合法信封。

### 5.3 gRPC（muduo_grpc）：传输层开两个通用钩子，由 engine 注册

第 4 批已做。`muduo_grpc` 是独立工程（`muduo_grpc.vcxproj`，只依赖 muduo、boost、nghttp2），不认识线程局部上下文。所以不在里面读 TLS，而是加两个可选钩子，契约写在 [grpc-muduo-transport.md](grpc-muduo-transport.md) §18：

- **出站钩子** `ChannelOptions::unaryInterceptor`：在 `Channel::StartUnaryCall` 入口执行，紧接在判空 `done` 之后，运行在调用线程上。它早于停机、在途上限、消息大小、metadata 和截止这几项检查，所以本地拒绝也拿到包装后的回调。它可以往 metadata 追加键，返回值替换完成回调；返回空回调按编程错误 `LOG_FATAL`。为空表示不拦截。
- **派发钩子** `ServerOptions::dispatchInterceptor`：派发泵调用 handler 时包一层，参数是本次调用的 metadata 和一个 `invoke`。钩子多调 `invoke` 的被忽略；一次都没调的，服务端补调一次并打 ERROR。所以 handler 恰好执行一次。为空表示直接调用。

engine 侧的追踪实现在 `cpp/libs/engine/core/network/grpc_trace_hooks.{h,cpp}`：

- **出站** `grpc_tracing::InjectTraceIntoUnaryCall`：当前上下文有效、且调用方没写过 `traceparent` 时写入；完成回调用 `tracing::Bind` 包一层，在发起时的 trace 下执行，做法与 RpcChannel 的 `Deliver` 一致。完成契约不变：恰好一次、在 loop 线程上、不在 Send 栈内同步回调。
- **入站** `grpc_tracing::DispatchUnderIncomingTrace`：从 metadata 取 `traceparent`，用 `tracing::ScopedIncoming` 接续或开根（与 RPC0 派发同一条规则，同一个作用域类）；作用域覆盖整个同步 handler，包括守护段。守护段发起异步步骤、之后再 `Proceed()` 的情况，由那个异步原语自己的恢复传下去（§4.3）。
- **不经通道的本地完成**：生成客户端缺通道、序列化失败时走 `QueueGrpcLocalCompletion`（`grpc_client_runtime.h`），不经过出站钩子，那里另用 `tracing::Bind` 包一层。
- **注册点**：业务通道在 `node_connector.cpp`，节点服务端在 `node.cpp` 建 `ServerOptions` 处。etcd 通道（`muduo_etcd_transport.cpp`）不装。
- 生成的客户端桩（`cpp/generated/grpc_client/`）和各 `*Impl::RegisterMethods` 都不用改。双向流不经过钩子，目前没有业务流需要追踪。
- **验签不受影响**：callerauth 只签 caller、方法和 subject（`go/shared/callerauth/sign.go`），多一个 `traceparent` 头不改变签名内容。

### 5.4 Kafka（librdkafka）

第 5 批已做。键名和读写只在 `cpp/libs/engine/infra/messaging/kafka/kafka_trace_header.h` 一处：`kTraceparentHeader`、`TraceparentOf`、`TraceFromHeaders`。这个头和仓里别的包含 `rdkafkacpp.h` 的头一样，先定义 `LIBRDKAFKA_STATICLIB`（原因见 `kafka_consumer.h` 顶部注释），它可能先于 `kafka_producer.h` / `kafka_consumer.h` 被包含。

- **生产**：`KafkaProducer::send` 在调用线程上取一次 `tracing::Current()`，转成 traceparent 串传给 `produceOnce`；队列满后的重试、重建后的重试都带同一个值。`produceOnce` 只在串非空时 `RdKafka::Headers::create()` 并加 `traceparent`，交给 `produce`（原来就用这个重载，headers 传的是空指针）。没有 trace 时不建 headers，消息与改动前逐字节相同。所有权按 librdkafka 的约定：`produce` 成功后 headers 归库（C++ 包装层在成功时自己释放包装对象），所以只 `release()`；失败时仍归调用方，由 `unique_ptr` 释放。
- **消费**：`KafkaConsumer::consumeOnce` 在 poll 线程上用 `TraceFromHeaders(msg->headers())` 取 trace（没有、不合法 → 无效值；同名键取最后一个），按值交给 `dispatch`。`Work` 带 `trace` 字段，`Work::Run` 在 `ScopedIncoming(trace)` 里执行回调：合法就建子 span，否则开新根。就地执行与投递到 loop 线程两条路径一样，都不接执行线程上碰巧在场的上下文。`MessageCallback` 签名不变，各业务回调不用改。
- **测试**：`cpp/tests/kafka_command_test/kafka_trace_header_test.cpp` 6 条，只用 `RdKafka::Headers` 这个纯内存对象，不连 broker。生产、消费两处接点本身没有单测：`KafkaProducer`、`KafkaConsumer` 都要连 broker，靠冒烟核对。

### 5.5 入口：开根与玩家上下文

- **gate 开根**（第 3 批已做）：`GateClientSessionHandler` 的 `DispatchClientRpcMessage`、`DispatchTokenVerify`、`HandleConnectionEstablished`、`HandleConnectionDisconnection` 开头各放一个 `tracing::ScopedRoot`。这四个入口按定义不属于别的请求，所以用 `ScopedRoot` 而不是 `ScopedSpan()`：线程上万一残留了上下文，也不会接上它。
- **gate 装玩家**：取到会话之后装 `ScopedPlayerId`。会话的「没绑角色」有 `0` 和 `kInvalidGuid`（`UINT64_MAX`）两种写法，统一经 `PlayerForLogContext` 归成 0；直接传 `kInvalidGuid` 会在日志里打出一个假的 player_id。
- **scene 装玩家**：`SceneHandler::ProcessClientPlayerMessage`（写在守护段内）在 `CheckRoutedPlayerIdentity` 通过之后装上会话绑定的玩家（`kInvalidGuid` 同样归 0）。trace 已经由 RPC0 接续，这里不开根。
- 其他带玩家的入口（玩家 Kafka 命令、scene_manager 发来的 gRPC 等）按需补，不追求一次铺满。
- **gate 路由 lane**（第 4 批已做）：路由模式下同一会话的 gRPC 类请求单飞排队，下一条请求从上一条的完成回调（或过期扫描）里发出。gRPC 完成回调恢复上下文之后，不处理的话下一条请求会挂到上一条的 trace 上。现在 `RouterLaneItem` 带 `trace` 字段，`gate_lane::Admit` 入队时存当前 trace（与会话快照同样取入队时刻）；`Pump` 发出每一条前用 `ScopedSpan(item->trace)` 装回，回给客户端的拒绝信封也落在这条请求的 trace 上。没有单测：lane 的装配代码不在任何测试工程里，靠冒烟核对。
- **高频消息关闭名单**：暂不实现，开关和名单一起等压测数据说明值得省再加（AGENTS §11.3：性能改动要有数据）。第 3 批之后，gate 转给 scene 的每条客户端消息都多 29 字节，下一轮登录压测要对比 gate → scene 的 RPC0 带宽。

### 5.6 协程执行器

第 3 批已做，两处接点：

- **`loop_async::AwaitCallback`（`core/async/coroutine_callback.h`），主要的一处**：全仓协程只经它等回调式异步操作（RPC、Redis、etcd、延时），没有协程之间互相唤醒的通道（2026-11-10 核对）。`initiate` 时记下协程当前的 trace，`Finish` 里用 `ScopedSpan(captured)` 装回，再完成 awaitable。默认执行器允许就地执行，协程大多直接在 `Finish` 里继续，所以落在协程自己的 trace 上。完成回调本身是什么上下文都不影响协程：可能是别的请求的，也可能像 Redis、etcd 回包那样没有。停止（`nullopt`）也走 `Finish`，取消路径上协程的收尾日志同样挂回原请求，不挂到发起停止的那一方。
- **`SceneExecutor` 的投递分支（`core/async/loop_task_scope.cpp`），兜底**：要求「不阻塞」的执行会经 `queueInLoop` 投递，离开当前作用域。投递的函数用 `tracing::Bind` 包一层，记下投递时的 trace，执行时装回；就地执行的分支本来就在当前上下文里，不用管。
- 协程本来不在 trace 里时，恢复后也不在，不会接上唤醒方的 trace（宁可丢，不可错）。
- 协程的每次恢复都是一次性的，符合 §4.2 第 3 条。`LoopTaskScope::Start` 时的上下文就是任务的初始上下文：请求里起的协程挂请求的 trace，启动流程里起的后台协程不挂。现有任务都是一次性短操作（写或续期战斗锁、写会话结束凭据、发布迁移、创建场景或战斗的许可等待），没有从请求里起的常驻循环。以后真要在请求作用域里起常驻循环的协程，先用清空上下文的 RAII 作用域包住 `Start`，否则这个请求的 trace 会挂在它之后的每一轮上。
- **测试**：`cpp/tests/cross_zone_test/loop_task_scope_test.cpp` 末尾 3 条：恢复时回到挂起时的 trace、本来没有 trace 的协程不接唤醒方的、停止路径上收尾日志挂回原请求。投递分支靠 `Bind` 的单测覆盖（`trace_context_test.cpp`）。

### 5.7 修正 `trace_context.h`

- `ParseTraceparent` 只收小写十六进制（W3C 规定含大写的值必须忽略）。版本只认 `00`，原实现就是这样，`ff` 本来就被拒；解析失败时不改写输出参数（原实现会写坏一半）。
- `ToHex` 改成查表。
- 补单测：这个文件现在一条测试都没有。

## 6. Go

### 6.1 已经有的，不用动

- go-zero v1.9.2 在 init 里设了 W3C TraceContext + Baggage 传播器（`core/trace/propagation.go`）。
- `ServiceConf.SetUp` 起 TracerProvider；没配 `Telemetry.Endpoint` 时只建 span、不导出。
- zrpc 服务端和客户端的 tracing 拦截器默认开着。
- 用 `logx.WithContext(ctx)` 打的行，以及 goctl 生成的 logic 里用 `l.Logger` 打的行，都自带 `trace` / `span`。

### 6.2 要补的

1. **新包 `go/shared/tracing`**（第 4 批已建，`grpc.go` 与 `grpc_test.go`），把传播相关的代码收在一处：
   - `UnaryClientInterceptor()`：给裸 `grpc.NewClient` 用。它开一个 client span，按全局传播器写 metadata，做法照 go-zero zrpc 客户端的 tracing 拦截器（那份在 go-zero 的 internal 包里，引用不了）：先用 `metadata.FromOutgoingContext` 取出已有的出站 metadata 再追加，所以 callerauth 已经写好的签名头原样保留。拦截器在调用方建好 ctx 之后才执行，不会被签名那一步的整体替换冲掉。要接的 5 处，第 4 批接了 4 处：
     - `go/scene_manager/internal/logic/scene_node_client.go`（已接）
     - `go/shared/scenenode/conn.go`（已接）
     - `go/match/internal/discovery/node_watcher.go`（已接）
     - `go/match/internal/authority/rank_publisher.go`（已接）
     - `go/client_rpc_router/internal/discovery/node_watcher.go` 的 `DialEndpoint`：**未接**。2026-11-10 这个函数正被别的会话整段重写，等它落定后加一个 `grpc.WithChainUnaryInterceptor(tracing.UnaryClientInterceptor())` 即可。没接之前，路由模式下路由服往后端的调用不带 traceparent，链路在路由服断开。

     `go/mail/cmd/mailadmin` 和 `go/match/cmd/spectatefeatured` 是运维命令行，可以不接。
   - **Kafka 载体**（第 5 批已写）：按客户端库拆成两个子包，注入和提取都走 `otel.GetTextMapPropagator()`，同名键取最后一个（与 C++ 的 `get_last` 同口径）：
     - `go/shared/tracing/kafkagotrace`：`Headers(ctx)` 生成 kafka-go 消息头，`ContextFrom(ctx, headers)` 取回远端父 span，之后 `logx.WithContext` 打的行带同一个 `trace`；
     - `go/shared/tracing/saramatrace`：`Headers(ctx)` 生成 sarama 生产消息头。本仓只有 login 用 sarama，而且只生产，所以只有注入。

     不放进 `shared/tracing` 根包：根包被只做 gRPC 的模块引用（scene_manager 直接引，mail、player_inbox、trade 经 `shared/scenenode` 引），这几个模块的 go.mod 里没有 sarama，trade 连 kafka-go 都没有，放进根包会让它们编不过。拆开后每个模块只依赖自己本来就用的 Kafka 客户端。
   - `PlayerFieldInterceptor()`：服务端拦截器。请求实现了 `GetPlayerId() uint64` 且值非零时，用 `logx.ContextWithFields` 加上 `player_id`。第 4 批接了 login（排在会话拦截器之后、serverbase 之前）和 data_service（排在关停开关之后）。scene_manager 的 main（`scene_manager_service.go`）2026-11-10 有别的会话的大批未提交改动，**未接**，同样等它落定再接。
2. **Kafka 接点**（第 5 批按 `WriteMessages`、`ProducerMessage`、`FetchMessage`、`ReadMessage` 全量 grep 核对过，2026-11-11）：
   - **已接**：
     - `shared/kafkautil/gate_push.go`：`PushToPlayer`、`BroadcastToPlayers`、`BroadcastToScene`、`BroadcastToGateClients` 写的消息都带头，经这几个函数推 gate 的服务不用改就带上。
     - `player_inbox/internal/inbox/notify.go`：发给 scene 的在线提醒。入箱请求里发的带请求的 trace，补发循环里发的不带。
     - `login/internal/kafka/command_producer.go`：新增 `SendToTopicPartitionContext(ctx, …)`，原 `SendToTopicPartition` 委托它并传 `context.Background()`。ctx 只用来取 trace，不控制超时（超时仍由 sarama 的 `Net.*Timeout` 决定）。
     - data_service 消费：`snapshot_consumer.go` 的 `decode`（3 条）和 `txlog_consumer.go` 的 `decodeTxLog`（2 条）逐条日志，经 `message_log.go` 的 `messageLogger(msg)` 挂上这条消息自己的 trace。解码在剥掉消息头之前，取得到头。批里为省内存剥掉了 `Value`、`Key`、`Headers`，所以落库阶段的 STOPPED 日志不挂 trace，它们点名了 guid、玩家和 offset。
     - C++ 两端是通用接点（§5.4），gate、scene 消费的命令都在消息头的 trace 下处理。
   - **未接，等对应文件空出来再接**（2026-11-11 这些文件都有别的会话没提交的改动）：
     - login 的调用方：`svc/gate_command.go` 的 `PushTipToUnboundSession`，`svc/servicecontext.go` 的 `SendBindSessionToGate`、`KickSessionOnGate`、`PushTipToSession`。这几个函数都不收 ctx，补尾要给它们加 ctx 参数、改调 `SendToTopicPartitionContext`，并改 logic 层的调用方（`entergamelogic.go` 也在改）。**没接之前 login → Kafka → gate 这一跳接不上**，§10.1 第 5 批的这条验收要等补尾。`svc/gate_migration.go` 的 `SendGateDrainMigration` 是排空监视器发的整台 gate 命令，本来没有请求 trace，不接。
     - 生产：`mail/internal/notify/notify.go`（发 gate）、`mail/mail.go` 与 `player_inbox/internal/svc/servicecontext.go` 的转发 writer、`match/internal/svc/servicecontext.go` 的对局结果发布。
     - 消费：`guild/internal/kafka/trial_result_consumer.go`、`match/internal/kafka/result_consumer.go`。接法同 data_service：逐条日志用 `logx.WithContext(kafkagotrace.ContextFrom(…))`。
     - `chat/internal/svc/world_push.go` 的世界频道推送文件本身没被改，但 chat 的 go.mod、go.sum 现在没有 kafka-go 这个依赖，先不碰；要接时照 `gate_push.go` 加一行。
   - **不继承请求 ctx 的发送**：下面几处为了不被请求的取消带走，用 `context.Background()` 另起预算，trace 也一起丢了，只在写消息处加头没有用。补尾时预算改从 `context.WithoutCancel(请求 ctx)` 派生（Go 1.21 起的标准做法：去掉取消和截止时间，保留值），trace 和 logx 字段就一起带过去，再在写消息处加头：
     - `match/internal/team/notify.go` 的 `publish`、`publishOnline`、`publishRegroup`（组队副作用里的 scene 刷新与推送）；
     - `match/internal/logic/push.go` 的 `pushToPlayer`（不收请求 ctx，要改签名）；
     - `scene_manager/internal/logic/enterscenelogic.go` 两处、`enter_hosted_placement.go` 一处，直接传 `context.Background()`（文件有别的会话没提交的改动）。
   - **本来就没有 trace**：`player_locator/internal/logic/leasemonitor.go` 是周期扫描，不接。
   - 批量消费时按条取上下文；整批级别的日志不挂 trace。
3. **日志调用不做全量迁移**：不带 ctx 的 `logx.Xxx` 直接调用约 1600 处（含测试）。请求路径上的新代码用 `logx.WithContext(ctx)` 或 logic 的 `l.Logger`。

## 7. Java（本仓 `java/gateway_node`）

- 加 `micrometer-tracing-bridge-otel`，版本由 Spring Boot 3.4.3 的 BOM 管理；不加导出器。
- 设 `management.tracing.sampling.probability: 1.0`。
- logstash 结构化日志会自动带上 MDC 里的 `traceId` / `spanId`，日志代码不用改。
- grpc-java 客户端挂 Micrometer 的 `ObservationGrpcClientInterceptor`（micrometer-core 自带），出站写 `traceparent`。
- 公网 HTTP 入口不接受外部 `traceparent`。做法二选一：在观测过滤器之前加一个过滤器，剥掉 `traceparent`、`tracestate`、`baggage`；或者把传播配成只产出、不消费。落码时选一种，并写测试。
- `config_node` 同样处理，可以排在后面。

## 8. 成本与兼容

### 8.1 成本预算

| 项 | 成本 |
|---|---|
| RPC0 每帧 | 有 trace 时多 29 字节（3 个 fixed64 各 9 字节，flags 2 字节）；没有时为 0 |
| 每条 C++ 日志 | 无上下文：一次线程局部读取加一个分支。有上下文：一次不超过约 4.1KB 的拷贝；每个上下文第一次打日志时做一次查表转换（48 个字符） |
| gate 开根 | 3 次 `mt19937_64` |
| gRPC 每次调用 | 多一个约 60 字节的头，外加一次字符串拼接 |
| Kafka 每条 | 多约 70 字节 header |
| Go | go-zero 现在就在付这份开销，不新增 |
| Java | 多一层 Observation |

**验收以压测对比表为准**（AGENTS §6）：阶段一落码后跑一轮登录压测，对照 `prev-summary.txt` 看 gate、scene 的 CPU 和 RPC0 带宽。高频消息关闭名单，只在这轮数据说明需要时才加。

### 8.2 兼容

- RPC0 新字段、gRPC 头、Kafka header 都是可选的，旧版本会忽略。项目未上线，混跑不阻塞。
- C++ 日志行尾多了后缀，行首不变。落码时核对一遍按行尾匹配 C++ 日志的脚本和测试。
- 不改客户端契约，Java 版不需要同批改（§12）。

## 9. 采集与查询

### 9.1 Alloy 规则（本地 `deploy/observability/alloy/config.alloy`）

- **Go**：`stage.json` 加 `trace_id = "trace"`、`span_id = "span"`、`player_id = "player_id"`，并加进 `stage.structured_metadata`。
- **C++**：在 muduo 行的规则里再加一条锚定行尾的正则，提取的值进结构化元数据：
  `\[ctx(?: trace_id=(?P<trace_id>[0-9a-f]{32}) span_id=(?P<span_id>[0-9a-f]{16}))?(?: player_id=(?P<player_id>\d+))?\]\s*$`
- **Java**：`stage.json` 加 `trace_id = "traceId"`、`span_id = "spanId"`。
- 改完先按 grafana-loki-local-logs.md §5.1、§5.2 校验配置、不入库试跑。

### 9.2 K8s

- C++ sidecar 的规则由 `tools/scripts/k8s_deploy.ps1` 里的 `New-CppLogSidecar*` 生成，同样加上面那条正则。**改它会滚动重启 C++ 节点**（grafana-loki-local-logs.md §6.5），要挑时机。
- 新增一个读 `/var/log/pods` 的 Alloy DaemonSet，采 Go / Java。解析规则照搬本地的 §2.1、§2.3，标签从 pod label 取。

### 9.3 查询

- 一条请求的全链路：`{job=~".+"} | trace_id="<32 位>"`
- 一个玩家的足迹：`{job=~".+"} | player_id="<id>"`
- Grafana 看板「游戏服务日志总览」加 `trace_id`、`player_id` 两个文本框变量。
- 落码时同步更新文档：grafana-loki-local-logs.md 的 §3 标签表、§4 常用查询和 §6.4，以及 log-management.md 的「This Project」一节。

## 10. 分批落地

### 10.1 阶段一：串起全链路（本期）

按顺序做，每批都能单独验收：

| 批 | 内容 | 验收 |
|---|---|---|
| 1 | §5.7 原语修正，§4.1 上下文改造，§5.1 日志后缀 | 单测通过；本进程内一次 RPC0 派发的日志带完整 `trace_id` |
| 2 | §5.2 RPC0 信封（含 proto 生成） | 回环用例通过；scene 处理一条玩家消息时推给 gate 的消息，两边日志 `trace_id` 相同、`span_id` 不同（gate 收客户端消息时开根在第 3 批，此前 gate → scene 这一跳 scene 仍开新根） |
| 3 | §5.5 入口开根与玩家上下文，§5.6 协程（`AwaitCallback` 与执行器），`ScopedRoot`、`Bind` | 单测通过；客户端一条消息从 gate 到 scene 是同一个 trace，两边日志都带 `player_id`；co_await 之后 trace 不变；周期任务不带 trace |
| 4 | §5.3 gRPC 钩子，§6.2 第 1 项的客户端拦截器与玩家拦截器，§5.5 路由 lane 排队项带 trace | C++ → Go、Go → C++ 都接得上；login 的日志带 `player_id`；路由模式下排队发出的请求挂自己的 trace，不挂前一条的。补尾：client_rpc_router 的 `DialEndpoint` 与 scene_manager 的玩家拦截器（§6.2） |
| 5 | §5.4 与 §6.2 的 Kafka 载体和第 2 项 | scene → Kafka → data_service 接得上；login → Kafka → gate 接得上。补尾：login 调用方切换、mail / match / guild 等几处生产消费点、几处不继承请求 ctx 的发送（§6.2） |
| 6 | §7 Java gateway_node | 外部带 `traceparent` 的 HTTP 请求被开新根；出站 gRPC 接得上 |
| 7 | §9.1 本地 Alloy 规则，§9.3 看板变量与文档同步 | 本机登录一次，一条 `trace_id` 查询能看到 Java、gate、login、data_service、scene 的行 |

### 10.2 阶段二：补齐 K8s 采集

做 §9.2 的两项。sidecar 规则一改就会滚动重启 C++ 节点，要和其他需要滚动的变更放在同一个窗口。

### 10.3 阶段三（可选）：span 与 Tempo

- Go 配 `Telemetry.Endpoint` 和 `Batcher: otlpgrpc`，指向 Alloy 的 OTLP 接收端；Java 加 OTLP 导出器。Alloy 做尾部采样（错误和慢请求全留，其余按比例）后送进 Tempo；Grafana 里配上 Loki 与 Tempo 互相跳转。
- **C++ 不引入 OpenTelemetry C++ SDK**：它的批量导出要起后台线程，OTLP/gRPC 导出器还会拉起 gRPC 运行时线程，和「运行期不起 gRPC 线程」的目标冲突。候选做法：基于 muduo 写一个 OTLP/HTTP 导出器，或者写本地 OTLP JSON 文件，由采集器读取。到阶段三再定。

## 11. 测试与验收（Codex 执行，AGENTS §10.1）

**C++**（MSBuild 串行 `/m:1`；改了引擎库要先编库再跑测试，`run_cpp_tests.ps1 -Build` 不会重编库）：

- `trace_context`：合法值往返；大写、版本 `ff`、全零 trace 或 span、长度和分隔符错误都要拒绝；查表版 `ToHex` 与逐字节 `snprintf` 在 0–255 上全部一致。
- 日志后缀拼接（写成纯函数，脱离 Node 单测）：有无 trace、有无玩家的各种组合；行尾有换行和没有换行；接近 4000 字节的长行；无上下文时输出与输入逐字节相同。
- RPC0（`cpp/tests/rpc_controller_test/rpc_channel_loopback_test.cpp`）：
  - 带 trace 的调用：对端 handler 看到同一个 `trace_id`，`span_id` 不同；
  - 不带 trace 的调用：对端开根；
  - 回包回调在调用方原来的 trace 下执行；
  - trace 非零、span 为零的信封：开根，且请求照常派发；
  - RESPONSE、HEARTBEAT 帧不带这几个字段。
- gRPC（第 4 批已写）：
  - `grpc_client_loopback_test.cpp` 的 `GrpcTraceLoopbackTest` 4 条：出站带 `traceparent`，服务端接续、完成回调恢复；不在 trace 里的调用不写头、服务端开根；调用方已写的 `traceparent` 不重复；缺通道的本地完成同样恢复。
  - `muduo_grpc_test/server_test.cpp` 末尾 4 条：钩子本身的契约（追加 metadata 与包装顺序、通道本地拒绝也包装、多调 `invoke` 只执行一次、不调由服务端补调）。
  - `trace_context_test.cpp` 加 1 条 `ScopedIncoming`。
  - 路由 lane 排队项带 trace 没有单测，靠冒烟。
- 协程（第 3 批已写，`loop_task_scope_test.cpp` 3 条）：回包在别的 trace 里或没有 trace 时，协程恢复后仍是挂起时的 trace；本来没有 trace 的协程不接唤醒方的；停止路径同样挂回原请求。
- `ScopedRoot` 与 `Bind`（第 3 批已写，`trace_context_test.cpp` 4 条）：`ScopedRoot` 不接当前 trace；`Bind` 在包装时的 trace 下执行，包装时没有就不带，执行完恢复，参数和返回值原样透传。
- Kafka（第 5 批已写，`kafka_command_test/kafka_trace_header_test.cpp` 6 条）：生产写的值经 `RdKafka::Headers` 读回后逐位相同；读得出 Go 侧写的值（与 Go 用例同一个字面值）；无效 trace 不写值；没有 headers、没有这个键、值不合法（含空串、大写、全零 trace、未知版本）都得到无效值；同名键取最后一个。`KafkaProducer` / `KafkaConsumer` 里的两处接点要连 broker，靠冒烟。
- gate、scene 入口的开根与装玩家没有单测，靠冒烟核对日志。`runEvery` 不用 `Bind` 是使用约定，没有机械检查。

**Go**（`go test`）：

- `go/shared/tracing`（第 4 批已写 4 条）：拦截器写入 `traceparent` 并保留已有的签名 metadata；调用方没有 span 时开根；`PlayerFieldInterceptor` 只在 `GetPlayerId` 非零时加字段。
- Kafka 载体（第 5 批已写）：
  - `go/shared/tracing/kafkagotrace` 6 条：对固定 id 写出的值与 C++ 用例的字面值逐字相同；往返后消费端拿到同一个 trace、且标为远端；ctx 里没有 trace 时不写头；没有、坏值、全零、别的键都不产生 trace；同名键取最后一个；`logx.WithContext(ContextFrom(…))` 打的行带生产端的 trace_id。
  - `go/shared/tracing/saramatrace` 2 条：写出同一个字面值；没有 trace 时不写头。
  - 子包的用例自己设 W3C 传播器、结束时恢复，不依赖 go-zero 的 init。
- Go 客户端带 `traceparent` 调 muduo_grpc 服务端的跨语言互通：C++ 侧已由 `grpc_client_loopback_test` 覆盖解析，Go 侧由上面的拦截器用例覆盖写入；真跨进程的核对放在冒烟里（`go/shared/grpcinterop` 的互通用例没扩展）。

**Java**（`mvn -pl gateway_node test`）：外部 `traceparent` 不被采信；出站 gRPC 带 `traceparent`；JSON 日志带 `traceId`。

**采集**：按 grafana-loki-local-logs.md §5.2，拿三种语言新旧格式的样例行不入库试跑，核对结构化元数据；旧格式的行解析不受影响。

**端到端**：本机起服，机器人登录一次，用 `trace_id` 查询，核对 §10.1 第 7 批的验收；再按 AGENTS §6 跑一轮登录压测，出对比表。

## 12. 双版本（AGENTS §12）

- 本文不改客户端契约，两版不需要同批改。
- Java 版服务器（`xuanming-server-mmo`）守同一份约定（§3：`traceparent`、三个日志字段、开根规则与信任边界），在它自己的内部 RPC、Kafka 和日志上实现。状态：**待做**。本仓落码完成后，按 AGENTS §12 在 Java 仓的 `PARITY.md` 登记。

## 13. 待定

1. **trace_id 要不要回给客户端。** 回给客户端的话，玩家报错时能带一个「错误编号」给客服；但 `TipInfoMessage` 是客户端契约，两版要同批改。先不回，阶段一跑稳再议。
2. **阶段三 C++ 怎么导出 span**（§10.3）。
3. **尾部采样的比例**（§10.3）。
4. **`error_reporter`（#250）的事件要不要带 `trace_id`**，用来交叉定位。阶段一之后再看。

## 14. 参考

- W3C Trace Context：https://www.w3.org/TR/trace-context/
- OpenTelemetry：https://opentelemetry.io
- todo.md #152（本题）、#27（线上快速定位）、#250（错误聚合）
- 代码：`cpp/libs/engine/core/tracing/trace_context.h`、`cpp/libs/engine/thread_context/trace_context_tls.{h,cpp}`、`cpp/libs/engine/core/network/rpc_channel.cpp`、`cpp/libs/engine/muduo_grpc/{channel,server}.cpp`、`cpp/libs/engine/infra/messaging/kafka/`、`deploy/observability/alloy/config.alloy`

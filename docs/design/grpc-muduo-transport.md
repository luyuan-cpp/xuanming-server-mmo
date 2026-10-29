# C++ gRPC 传输改由 muduo 驱动(nghttp2 + 自有 gRPC 调用语义)

> **2026-10-26 后续决策：彻底移除官方 C++ SDK。** 负责人已批准完整依赖拆分与验证范围。
> C++ 保留本文的标准 gRPC 线上协议及 `muduo_grpc` 实现；删除官方初始化探针、官方插件、
> `*.grpc.pb.*` 与官方链接项，Protobuf、Abseil、GoogleTest、BoringSSL 按原提交独立供给。
> 本文早期“不换依赖、不删 grpc 子模块”和保留运行时守卫的阶段决策由本条取代。
> Protobuf 的 `utf8_range` 使用其自带源码；curl / librdkafka 继续共用同版本独立 BoringSSL，
> 不借此次迁移切换 TLS 实现或升级依赖。业务协议、字段号及 Go / Java 内部实现不变。
> 删除官方产物须由正规生成器校验受管路径后执行；工程和工具不得回退到旧 SDK。
> Windows Debug / Release 分别使用独立安装前缀，禁止混用 CRT；Linux 同样从独立来源构建。
> 完成证据须包含无旧 SDK 的正规生成、串行编译、相关回归及实际完成的平台范围；本决策本身不代表验证通过。

- 日期:2026-10-14
- 当前状态（2026-10-26）：官方 C++ SDK 已移除，独立依赖与正规生成、Windows Debug 完整构建通过；运行回归及 Linux 验证继续，见 §17。下文分批记录保留当时状态。
- 范围:C++ 节点(gate / scene / battle)作为 gRPC 客户端与服务端的传输层。Go / Java 服务与线上协议不变
- 授权:负责人 2026-10-14 拍板「分 5 批推进、本次做第 1 批」「引入 nghttp2 v1.70.0 子模块」「etcd 走原生 gRPC 接口」
- 关联:[client-rpc-router.md](client-rpc-router.md)(第 1 批接入点)、
  [§16 客户端调用契约](#grpc-client-contract)(deadline、完成回调与调用点归属)、
  [§3.1 节点双端口与发现](#node-grpc-endpoints)、[Scene 线程与停机](scene-node-threading-model.md)、
  [thread-count-monitoring.md](thread-count-monitoring.md)(现有 gRPC 线程构成)、
  AGENTS.md §11.7(连接与回调生命周期)

## 0. 一句话

**线上仍然是标准 gRPC(HTTP/2 h2c)**,Go 服务一行不改;C++ 侧把官方 gRPC C++ 运行时(EventEngine 线程池、
sync server poller、CompletionQueue 与 5ms 轮询)换成:muduo `TcpClient` / `TcpServer` 收发字节 →
nghttp2 做 HTTP/2 编解码 → 自有模块 `muduo_grpc` 实现 gRPC 调用语义 → 官方 Protobuf 序列化。
网络、定时器、完成回调全部在节点自己的 EventLoop 上执行,新模块**不创建任何线程、线程池或 CQ**。

## 1. 目标与非目标

目标:

1. C++ 节点的 gRPC 出站、入站都由节点 EventLoop 驱动;业务回调与今天一样只在 loop 线程上执行。
2. 线上字节与 Go(grpc-go v1.79.3 + go-zero)完全兼容:请求头、5 字节消息前缀、trailers、状态码、
   `-bin` 头、`grpc-timeout`、GOAWAY / PING 行为。
3. 迁移期新旧实现并存,按开关在启动时选定;**一次调用只走一种实现、只发一次**。
4. 最终移除官方 gRPC C++ 的运行时、`grpc_cpp_plugin` 产物与 28 个工程的 grpc 链接项。

非目标:

- 不改 `.proto`、不改 Go / Java 服务、不改客户端契约(AGENTS §12:Java 版不受影响,见 §12)。
- 不做 TLS(现网 C++ 与 Go 之间、与 etcd 之间都是明文 h2c;muduo 本身也没有 TLS)。
- 不做压缩、不做 retry policy / service config / 负载均衡策略(现网 C++ 一样都没开,见 §4)。

## 2. 选型

| 方案 | 结论 | 理由 |
|---|---|---|
| **nghttp2 核心库 + 自写 gRPC 语义(采用)** | ✅ | nghttp2 是 C 语言 HTTP/2 编解码的事实标准(curl、Apache httpd mod_http2、Node.js 内置),设计上**不做 I/O**:`nghttp2_session_mem_recv2` 吃字节、`nghttp2_session_mem_send2` 吐字节,天然由外部 EventLoop 驱动。gRPC over HTTP/2 的规则只有一份公开规范([PROTOCOL-HTTP2](https://grpc.github.io/grpc/core/md_doc__p_r_o_t_o_c_o_l-_h_t_t_p2.html)),Envoy 的 gRPC 客户端就是这个组合。MIT 许可。 |
| 官方 gRPC + 自定义 EventEngine | ❌ | EventEngine 仍是 `experimental` 接口、随版本变;要实现 Endpoint / Listener / DNS / 定时器整套语义;运行时、CQ 与构建依赖都还在,与「移除官方运行时」的目标相反。 |
| Boost.Beast | ❌ | 最新稳定版仍无 HTTP/2。 |
| cpp-httplib | ❌ | HTTP/1.1、阻塞 I/O,不能进 EventLoop,也连不了 gRPC。 |
| brpc 的 h2/gRPC 协议 | ❌ | 自带 bthread 调度,与 muduo 单 loop 模型冲突。 |
| Thrift / 自定义 TCP | ❌ | 改协议,Go 侧要跟着改,违背「线上协议不变」。 |
| etcd 走 HTTP/JSON 网关 | ❌(负责人 10-14 定) | 要另写流式 HTTP/1.1 客户端与 int64 / bytes 的 JSON 适配,等于多维护一套协议;原生 gRPC 接口与其他调用共用同一套传输。 |

依赖:`third_party/nghttp2` 子模块,钉 **v1.70.0**(2026-07-29 发布;annotated tag 对象 `b69da4c2656b…`,指向提交
`85e300c79fb6…`,与 GitHub API 核对一致;v1.68.1 修过 CVE-2026-27135,本版本已含)。只编静态核心库
(`ENABLE_LIB_ONLY=ON`、`BUILD_SHARED_LIBS=OFF`、`BUILD_STATIC_LIBS=ON`),不编 nghttp / nghttpx / h2load,
不引入 mruby、neverbleed 等嵌套子模块。构建方式与 grpc / hiredis / yaml-cpp 一致(上游 CMake),见 §9。

## 3. 分层

```
业务(gate SendViaRouter / 将来的生成客户端与服务端 handler)
        │  Channel::StartUnaryCall(method, 请求字节, metadata, 截止时间, 完成回调)
        ▼
muduo_grpc::Channel ── 连接状态机 / 排队 / 截止时间 / 透明重试 / 恰好一次完成
        │ 持有 1 个 READY 连接 + 若干 draining 连接
        ▼
muduo_grpc::Http2Transport ── 一条 TCP 连接:muduo TcpClient(按值持有)+ 握手超时 + 存活探测
        │ 字节进出
        ▼
muduo_grpc::ClientSession ── 纯协议引擎(无 I/O、无定时器):nghttp2 client session、请求头、
        │                       5 字节前缀、headers / trailers / RST / GOAWAY → gRPC Status
        ▼
nghttp2(HTTP/2 帧、HPACK、流控、SETTINGS、PING ACK)
```

- `ClientSession` 不碰 socket 与定时器,单测里与一个内存中的 nghttp2 服务端会话对接,完全确定性;
  `Http2Transport` 只是把它接到 muduo 上的薄适配器。
- 公共头文件不包含 `nghttp2.h`;使用方(gate)只需 `muduo_grpc` 的包含目录与两个 lib。
- 工具层:`status`(状态码与 HTTP / RST_STREAM 映射)、`metadata`(键校验、`-bin` 的 base64、`grpc-message`
  百分号编码)、`grpc_timeout`、`message_framing`(5 字节前缀的拼装与增量拆包)。
- 生成客户端与业务调用方共享的调用结果、回调、选项和 metadata 类型由手写公共头 `cpp/libs/engine/core/network/grpc_client_call.h` 定义，以 `core/network/grpc_client_call.h` 引用。2026-10-19 将其从 `cpp/generated/grpc_client/grpc_call_tag.h` 迁出：`GrpcTag` 已删除，文件名改为表达现有类型职责，公共头归属 core，生成模板只引用、不产出该文件。历史实施记录中的旧路径保留作历史；调用语义与线上协议不变。
- 节点的业务 `node_id` 与带版本的 EnTT 实体句柄是不同身份。`NodeConnector` 的 TCP / gRPC 路径均由 `registry.create()` 分配实体，按 `node_uuid` 识别同一进程，摘除也按 UUID 查找；业务查询使用 UUID 或 `FindNodeEntityByZoneAndNodeId`，不得把 `node_id` 强转成实体句柄。身份解析与跨 zone 回归见 [路由身份审计](routing-identity-audit-20260908.md)，句柄寿命见 [组件访问规则](ecs-component-access-rules.md#实体类型与生命周期边界)。旧故障中的“node_id 仅 zone 内唯一”和“TCP 仍直接使用 node_id 作句柄”均不作为当前约束。

<a id="node-grpc-endpoints"></a>

### 3.1 节点双端口与服务发现

有注册 gRPC 服务的 C++ 节点使用两个端口：`NodeInfo.endpoint` 承载 muduo RPC0，
`grpc_endpoint.port = endpoint.port + 30000` 承载标准 gRPC HTTP/2。两者不能混拨。
NodeAllocator 检查派生端口；端口对不可用则本轮分配失败、不发布，不脱离 RPC0 端口单独扫描 gRPC fallback。
该规则是当前分配契约，不保证任意环境无需路由或网络配置。

Go scene_manager 的 `resolveNodeEndpoint` 只接受发现 JSON 的 `grpcEndpoint`，缺失时拒绝，
不退回 RPC0 的 `endpoint`。客户端按 `(zoneId,nodeId)` 缓存；缓存命中前仍拒绝 watch 表中的已知重复注册，
需要解析时要求唯一登记。它不等于每次缓存命中都重新向 etcd 实时验证唯一活注册。
实现见 [node_allocator.cpp](../../cpp/libs/engine/core/node/system/node/node_allocator.cpp)
与 [scene_node_client.go](../../go/scene_manager/internal/logic/scene_node_client.go)。

CreateScene、DestroyScene 等需要明确应答与截止的控制调用走 unary gRPC；Gate 路由、踢线等异步通知可走 Kafka，
恢复与重投仍由各自业务契约负责。这个取舍不依赖旧稿未实测的 gRPC／Kafka 固定延迟比较。
节点控制端口保持内网职责，客户端接入另按入口设计；玩家交接安全仍须满足属主与存盘屏障。
服务端具体实现、背压与排空统一见本文 §15，整体业务线程与拆除顺序见[Scene 线程模型](scene-node-threading-model.md)。

## 4. 线上契约(必须逐字节保持)

全部来自 2026-10-14 对现网 C++(grpc 1.83.0)与 Go(grpc-go v1.79.3、go-zero v1.9.2 / v1.10.0)源码的核对。

### 4.1 请求

- h2c prior-knowledge:TCP 建好立即发 24 字节 magic + SETTINGS(nghttp2 自动发 magic)。不走 HTTP/1.1 Upgrade。
- 头部顺序:`:method POST`、`:scheme http`、`:path /<pkg>.<Service>/<Method>`(无 package 时 `/<Service>/<Method>`,
  如 `/BattleNode/CreateBattle`)、`:authority <ip:port>`、`te: trailers`、`grpc-timeout`、
  `content-type: application/grpc`、`user-agent`、自定义 metadata。键一律小写,不得带 `connection` 头。
- 不发 `grpc-encoding` / `grpc-accept-encoding`:Go 只在请求带了已注册编码时才压缩响应,不发就永远不压缩。
- `grpc-timeout`:≤8 位十进制 + 单位(H/M/S/m/u/n),取能放下的最细单位并向上取整(与 grpc-go `encodeDuration` 一致,
  例:800ms → `800000u`,10s → `10000000u`,12s → `12000000u`)。超过 8 位 Go 会以 400 + Internal 拒绝整个请求。
- 消息:1 字节压缩标志(恒 0)+ 4 字节大端长度 + protobuf;空消息也有 5 字节前缀。请求上限默认 4 MiB(Go 服务端
  `maxReceiveMessageSize` 默认值),超出本地直接 `RESOURCE_EXHAUSTED`,不发出。

### 4.2 metadata 的两层 base64(不要「优化」)

```
SessionDetails 原始字节 raw
 → 应用层值 B = base64_std_padded(raw)          // 生成客户端 grpc_async_client.cpp.tmpl:155 的 Base64Encode(boost beast)
 → 线上头值   = base64_std_unpadded(B)            // 因为键以 -bin 结尾;官方 grpc C++ 与 grpc-go 发送都不带 padding
 → Go 应用层拿到 B → 业务 base64.StdEncoding 解出 raw
回程:路由服 grpc.SetHeader(B)(初始 metadata,不是 trailer)→ 线上 RawStd(B) → 传输层解一层得 B → gate 再解一层得 raw
```

- 传输层对 `-bin` 键**只加 / 解一层**;解码同时接受有无 padding,并按逗号拆分重复值(规范要求)。
- 应用层值仍由调用方(gate)按旧客户端的方式编码,传输层不知道 SessionDetails 是什么。
- 非 `-bin` 键的值必须是可打印 ASCII(0x20–0x7E),否则本地以 `INTERNAL` 失败。
- 自定义键不得以 `grpc-` 开头、不得是伪头;路由服按 `x-` 前缀整体透传,新实现**不得私自添加 `x-` 键**。

### 4.3 响应与状态

- 正常:HEADERS(`:status 200`、`content-type: application/grpc…`,可能带初始 metadata)→ 恰好一条消息 →
  trailers(END_STREAM,`grpc-status`、`grpc-message`)。trailers 之后 Go 可能补一个 RST_STREAM(NO_ERROR),不是失败。
- trailers-only:单个 HEADERS 带 END_STREAM,含 `:status`、`content-type`、`grpc-status`、`grpc-message`;此时初始 metadata 为空。
- **HTTP 200 或 protobuf 能解析都不代表成功**,只认完整的 gRPC 完成状态:
  - `content-type` 不是 `application/grpc` 开头(后接结束、`+` 或 `;`)或 `:status != 200`:按 HTTP 状态合成
    (400→INTERNAL、401→UNAUTHENTICATED、403→PERMISSION_DENIED、404→UNIMPLEMENTED、429/502/503/504→UNAVAILABLE、其余 UNKNOWN)。
  - 缺 `grpc-status`:UNKNOWN;值不是 0–16 的十进制:UNKNOWN。
  - 状态 OK 但消息数 ≠ 1,或有残留半条消息:INTERNAL(与 grpc-go「cardinality violation」一致)。
  - 无 trailers 就结束流:INTERNAL。
  - RST_STREAM:REFUSED_STREAM→UNAVAILABLE(**未被处理,可透明重试**)、CANCEL→CANCELLED、
    ENHANCE_YOUR_CALM→RESOURCE_EXHAUSTED、INADEQUATE_SECURITY→PERMISSION_DENIED、其余→INTERNAL。
  - 压缩标志为 1:INTERNAL(我们没声明任何编码,出现即对端违约)。
  - 响应超过接收上限(默认 4 MiB,与 grpc C++ 默认一致):RST_STREAM(CANCEL) + RESOURCE_EXHAUSTED。
- `grpc-message` 按规范百分号解码,解码永不失败(坏序列原样保留)。
- 状态码数值与 `grpc::StatusCode` 完全一致(日志里按整数打印,两边要能对照)。
- 本地截止时间到期:RST_STREAM(CANCEL) + DEADLINE_EXCEEDED。**超时不等于对端没执行**(go-zero 超时拦截器先返回、
  handler 继续跑),所以上层一律按「结果未知」处理,与 `core/network/grpc_client_call.h` 的约定相同。

### 4.4 PING:必须服从 grpc-go 服务端默认的 keepalive 强制策略

Go 侧没有任何服务配置 `KeepaliveEnforcementPolicy`,即 grpc-go 默认:有活跃流时两次 PING 间隔 < 5min 记一次 strike,
无活跃流时 < 2h 记一次;超过 2 次 strike 即 `GOAWAY(ENHANCE_YOUR_CALM, "too_many_pings")`;连接上第一个 PING 不记;
服务端写过 HEADERS / DATA 之后到达的下一个 PING 清零 strike。现有 C++ 通道的 30s、空闲也 ping 的设置
已在日志里触发过 `too_many_pings`,新实现不照搬。

新实现的规则(对 Go 服务端**正常情况下不产生 strike**,例外与上限见下):

- 默认不做周期 keepalive(`keepalive_time = 0`)。
- 存活探测只在「某个调用的截止时间到期、而这条连接自该调用发出以来**一个帧都没收到**」时发一次 PING,且只在下列之一成立时发:
  1. 这条连接还没发过 PING;
  2. 在上一次 PING 的 ACK **之后**收到过 HEADERS / DATA(服务端在处理我们上个 PING 之后写过数据,按 grpc-go 逻辑下一个 PING 清零);
  3. 距上一次 PING 已超过 grpc-go 默认 MinTime 加余量(5min + 10s;探测发出时该调用的流仍活跃,不记 strike);
  4. 以上都不满足时,至多再放行 **1 次**「无法确认已清零」的探测。没有这一条,一次探测被 ACK 后紧接着网络黑洞,
     这条连接就再也探测不到,只能等内核 TCP 重传放弃(Linux 约 15 分钟)。
- 例外:grpc-go `handlePing` 先排 ACK、后做 CAS,与写协程有竞态;`writeEarlyAbort` 的 trailers-only HEADERS 也不清零 strike。
  这两种情况下第 2 条放行的探测可能记 1 次 strike。第 4 条只给一次额度,这条路径累计的 strike 最多到 1~2,低于 grpc-go
  `>2` 才 GOAWAY 的阈值;万一触发,后果也只是这条连接被优雅 GOAWAY、重连一次。
- 已知盲区:连续两次探测都被 ACK、却始终没有 HEADERS / DATA 时,静默断链仍要等 5min 后第 3 条放行的探测,或 TCP 重传放弃、
  etcd 摘除节点。
- 探测在 `keepalive_timeout`(默认 20s,与 grpc-go 服务端的 ACK 超时一致)内收不到 ACK:判连接已死,关连接、
  在途调用以 UNAVAILABLE 结束、通道重连。
- 对端的所有 PING(含 BDP ping、GOAWAY 后的 ping)由 nghttp2 自动 ACK。
- etcd(第 2 批)的长流需要周期 keepalive:那时按通道配置打开(etcd 默认 `--grpc-keepalive-min-time=5s`),不影响 Go 服务通道。

### 4.5 GOAWAY

- go-zero 服务端 `MaxConnectionIdle = 5min`,空闲连接会周期性收到两段式 GOAWAY(先 `last_stream_id = 2^31-1`
  再发真实值)。收到即把该连接标为 draining:不再开新流,新调用触发新连接(通道回到 IDLE→CONNECTING)。
- id 大于 `last_stream_id` 的流由 nghttp2 以 REFUSED_STREAM 关闭(`nghttp2_session.c` `session_close_stream_on_goaway`),
  按「未被处理」处理;HEADERS 因 GOAWAY 没发出去的请求同样以 REFUSED_STREAM 关闭(`nghttp2_session.c` 2885–2900)。
- 已接收的流照常等完;draining 连接上没有流之后由通道在 `queueInLoop` 里释放。

### 4.6 透明重试(只针对可证明未被处理的请求)

与官方 gRPC C++ / grpc-go 的 transparent retry 一致:流以 REFUSED_STREAM 关闭(含 GOAWAY 未处理、HEADERS 未发出)
且截止时间未到时,**在新实现内部**重发一次。其余任何失败(超时、断线、RST CANCEL、非 OK 状态)一律不重发,
更**不会**改走旧实现重发 —— 服务端可能已经执行。

### 4.7 并发与 SETTINGS

- Go 服务端不宣告 `MAX_CONCURRENT_STREAMS`(即不限),每个流一个 goroutine,背压只能由 C++ 自己做:
  通道在途上限(排队 + 活跃)默认 65536,超出本地以 RESOURCE_EXHAUSTED 失败。
- 本端 SETTINGS:`ENABLE_PUSH=0`、`INITIAL_WINDOW_SIZE=1 MiB`、`MAX_HEADER_LIST_SIZE=16 KiB`(与 grpc C++ 硬上限一致)
  ,连接级接收窗口放大到 16 MiB。收到的头部块超过 16 KiB:RST_STREAM + RESOURCE_EXHAUSTED。
- stream id 用尽(nghttp2 返回 `STREAM_ID_NOT_AVAILABLE`)时该连接转 draining,调用改走新连接。

## 5. 连接状态机与退避

对外暴露与 gRPC 相同的五态,供以后替换 `GetState`(`node_utils.cpp` 的 SceneManager / DataService 选点):

| 状态 | 含义 | 新调用 |
|---|---|---|
| IDLE | 没有可用连接 | 入队并开始连接 |
| CONNECTING | 正在建 TCP + 等服务端 SETTINGS | 入队(不是 wait_for_ready:各自截止时间到期即失败) |
| READY | 收到服务端首个 SETTINGS | 立即发出 |
| TRANSIENT_FAILURE | 本次尝试失败,退避中 | 立即 UNAVAILABLE(fail-fast,与现网未开 wait_for_ready 一致) |
| SHUTDOWN | 通道已拆 | 立即 UNAVAILABLE |

- READY 以「收到服务端 SETTINGS」为准,而不是 TCP 可写:TCP 连上只说明端口在监听,对端是不是可用的 HTTP/2 服务端要看它的 SETTINGS(gRPC 语义)。(2026-10-17 订正:原理由「Windows 的 `win_connect` 有假阳性」自 d27176e04 起已不成立,`rpc_client.h` 里据此设的启动宽限也已删除。)
- 一次连接尝试的上限 `connect_timeout` 默认 20s(gRPC MIN_CONNECT_TIMEOUT)。超时 → TRANSIENT_FAILURE,
  排队中的调用以 UNAVAILABLE 结束,按退避重连:初始 1s、倍数 1.6、抖动 ±20%、上限 120s;收到 SETTINGS 即重置
  ([connection-backoff](https://github.com/grpc/grpc/blob/master/doc/connection-backoff.md))。
- READY 连接断开(非 GOAWAY):在途流以 UNAVAILABLE 结束(REFUSED 的走 §4.6),通道回 IDLE,下次调用重连 —— 与
  pick_first 在 READY 断开后回 IDLE 的行为一致。
- **已知差异**:muduo `Connector` 没有「连接失败」回调(上游 `TcpClient.cc` 自己标了 `FIXME setConnectFailedCallback`),
  被拒绝的连接在我们看来一直是 CONNECTING,直到 `connect_timeout`;官方 gRPC 会立即进 TRANSIENT_FAILURE。
  后果只是失败来得晚(调用按自己的截止时间失败,gate 照样回 1003),不影响正确性。第 3 批要用连接状态做选点前,
  在三份 muduo 树里补这个回调(见 §10)。**2026-10-16 已补(未编译、Windows revents 未实测),见 §14.3。**

## 6. 线程、生命周期与关机(AGENTS §11.7)

- 所有 API 只能在通道所属 EventLoop 线程调用(断言);新模块不创建线程。
- 完成回调**恰好一次**,且**永不在 `StartUnaryCall` 内同步执行**:一律 `queueInLoop` 投递。成功、失败、截止、取消、
  断线、通道析构之间的竞争由每个调用自己的 completed 标志收口。`id_segment_bootstrap.cpp:160-163` 这类「先发再登记」
  的调用方依赖这一点。
- 连接所有权只在 muduo:`Http2Transport` 按值持有 `TcpClient`,只存 `weak_ptr<TcpConnection>`;绑进 `TcpClient`
  回调与定时器的一律是 `weak_ptr<自己>`。拆连接 = 显式 `forceClose` 后直接销毁,不在连接自己的回调栈上释放最后一个引用
  (通道在 `queueInLoop` 里释放 draining / 失败的连接)。
- nghttp2 回调里不调用业务回调、不销毁任何对象(只记状态),避免重入 nghttp2。
- 截止时间用 `steady_clock`;到点用 steady_clock 复核,未到期就重新 arm。
  ~~**已知限制**:muduo 定时器按墙钟到期,墙钟**回拨** N 秒时截止 / 建连 / 探测定时器都晚 N 秒触发。~~
  **已解决(2026-10-16,未编译)**:muduo `TimerQueue` 改按单调时钟(`steady_clock`)到期,`runAfter` / `runEvery`
  用单调 now 加时长,`runAt` 的墙钟时刻在调用时换算;Windows `nextTimeoutMs`、Linux timerfd 上弦时长同改
  (三份树落点与语义见 [muduo-timer-cancellation-hazards.md](muduo-timer-cancellation-hazards.md)「定时器时钟」)。
  墙钟前跳 / 回拨都不再影响定时器何时到期;steady 复核保留,只兜秒→微秒截断造成的微秒级早到。
- 节点摘除:通道是节点实体上的组件(`MuduoGrpcChannelPtr`),`ExecuteNodeRemoval` 的 `DestroyEntity` 析构它;
  析构把在途调用以 UNAVAILABLE 投递失败回调(gate 据此回客户端 1003 —— 旧路径这里是静默丢失)。
- 关机:关机分两段(2026-10-15 由「C++ RPC 异步回调链式调用设计」会话改成两段式,见 muduo-rpc-channel.md)。
  **阶段 0**(`Node::FinalizeShutdownInLoop`)取消定时器、关闭 RPC0 通道,并清掉所有 registry 上的 `MuduoGrpcChannelPtr`,
  然后 `queueInLoop(FinalizeTeardownInLoop)`。按 FIFO,`~Channel` 排进队列的 UNAVAILABLE 失败回调先于拆除段执行,
  执行时 ECS 与日志都还活着。拆除段(ReleaseNodeId …… `tlsEcs.Clear()` …… 两跳 quit)覆盖 `~TcpClient` 排进的
  `forceCloseInLoop` / `connectDestroyed`,理由与清 `RpcClientPtr` 相同(`~TcpClient` 要用 loop)。

## 7. 限额汇总

| 项 | 默认 | 依据 |
|---|---|---|
| 请求消息 | 4 MiB | grpc-go 服务端接收上限 |
| 响应消息 | 4 MiB | grpc C++ 默认接收上限 |
| 接收头部块 | 16 KiB | grpc C++ 元数据硬上限 |
| 通道在途调用(排队 + 活跃) | 65536 | 本地背压 |
| 输出缓冲 | 1 MiB 以上暂停从 nghttp2 取帧,写完回调后继续 | 防止慢对端把内存拖满 |
| 每次读事件 | muduo 一次 readv(≤ 可写空间 + 64 KiB),nghttp2 同步解完 | 单轮 CPU 有界 |

## 8. 第 1 批:Gate → client_rpc_router

- 只覆盖路由模式(`GATE_CLIENT_RPC_ROUTER=1`)下 gate 的唯一 gRPC 出站 `ClientRpcRouter.Forward`:业务请求与断线通知
  共用 `SendViaRouter` 这一个接缝(`client_message_processor.cpp`)。
- 开关 `GATE_ROUTER_TRANSPORT`,启动时读一次:`grpc`(默认,旧实现)| `muduo_h2`(新实现)。不认识的值按 `grpc`
  并 WARN。**第 3 批起失效**:官方客户端已删,任何非 `muduo_h2` 值只打 WARN 后照走 muduo_h2,本节下文的通道工厂、
  `InstallChannelFactory` 与回退方式都已删除(§14.1、§14.3)。它与 `GATE_CLIENT_RPC_ROUTER`(业务拓扑:直连还是经路由服)是两回事;直连模式下这个开关不生效。
- **地址要求**:`muduo_h2` 要求路由服在 `NodeInfo.grpc_endpoint.ip` 里登记 **IPv4 字面量**(K8s 注入的 POD_IP、
  或配置的具体 IP;仓库交付的配置都满足)。路由服 `ListenOn` 写主机名又没有 POD_IP 时不能用 `muduo_h2`:
  建通道失败、节点不进候选,服务发现每 2s 重试一次并记 ERROR。不在 loop 线程上做阻塞 DNS 解析,也不为这种节点
  回落官方通道(同一进程里同一类节点不混用两种实现)。IPv6 官方路径同样拨不通(`FormatIpAndPort` 不加方括号),不是新差异。
- `muduo_h2` 时 gate 在 `ClientRpcRouterNodeService` 的节点 registry 上下文里装一个通道工厂,`NodeConnector::ConnectToGrpcNode`
  看到工厂就建 `muduo_grpc::Channel`,**不再创建官方 `grpc::Channel`、Stub 与 CompletionQueue**。
  同一进程内其余 gRPC(etcd)仍走官方运行时 —— **不能据第 1 批宣称 gate 已经没有 gRPC 线程**。
- 回包 / 失败对客户端的可见行为与旧实现逐条相同(抽成 `rpc_reply_bridge` 供两条路径共用):
  成功按响应初始 metadata 里回显的 `x-session-detail-bin` 找会话、`MessageContent` 原样下发;
  失败按发出的 SessionDetails 找会话、回 `kServiceUnavailable`(1003);会话已不在只留采样日志;不重试。
- deadline 照旧取 `grpc_call_deadline::Get(ClientRpcRouterNodeService)`(配置 8000ms),线上编码成 `grpc-timeout`。
- 回退:改回 `GATE_ROUTER_TRANSPORT=grpc` 后滚动重启 gate。切换前先让该 gate 收敛在途调用(滚动重启本来就会断开客户端)。

## 9. 构建

- Windows:`tools/scripts/third_party/build_nghttp2.ps1` 用上游 CMake(Debug、`/MDd`、只编静态库、优先 Ninja)装到
  `third_party/nghttp2/install_vs2026_dbg`(子模块内,`.gitmodules` 为它设了 `ignore = dirty`,与 grpc 的安装目录同做法),
  产物 `lib/nghttp2.lib` 与 `include/nghttp2/{nghttp2.h,nghttp2ver.h}`。脚本先核对子模块提交号。
  使用 `nghttp2.h` 的翻译单元定义 `NGHTTP2_STATICLIB`(只有 `muduo_grpc` 库与它的测试)。
- 新库 `cpp/libs/engine/muduo_grpc/muduo_grpc.vcxproj`:只依赖 muduo 头(及 muduo 头间接要的 boost)与 nghttp2,
  **不依赖 protobuf / grpc / core / entt**(收发的都是字节,序列化由调用方做);`/utf-8`、`/WX`、`/MDd`。
  gate 链接 `muduo_grpc.lib` 与 `nghttp2.lib`(库目录加 `third_party/nghttp2/install_vs2026_dbg/lib/`);game.sln 里 gate 依赖它。
- 测试 `cpp/tests/muduo_grpc_test`:**不链接 grpc / grpc++ / core / session / proto**,只链 muduo、nghttp2。
- Linux:`setup_dependencies.sh` 加 nghttp2 段(核对同一提交 / 版本,源码 stamp 判定是否重编)、`Dockerfile.cpp` deps 阶段
  COPY 子模块、CI 子模块清单(不递归拉 nghttp2 的嵌套子模块)、`vcxproj2cmake.py` 的 `EXTERNAL_LIBS` 与工程清单、
  `build_linux.sh` 的 `LIB_PROJECTS`。运行镜像里 libcurl 依赖系统自带的旧版 `libnghttp2.so`,所以节点链接时加
  `-Wl,--exclude-libs,libnghttp2.a`,不把静态链入的 1.70 符号导出,避免 libcurl 运行期混用两个版本。

## 10. 分批计划

| 批 | 内容 | 前置 |
|---|---|---|
| **1(本批)** | nghttp2 依赖;`muduo_grpc` 客户端 unary;不链 grpc 的单测与对真实 Go 路由服的互通用例;gate 路由链路;开关默认旧实现 | — |
| 2 | 客户端双向流;etcd(KV / Txn / Lease / Watch / KeepAlive)迁到新传输:流断重建、按 revision 续 watch、compact 处理、关机 LeaseRevoke 先 flush;修 Windows `EventLoop::wakeup()` 空操作(删 5ms CQ 轮询的前提) | 1 验证通过 |
| 3 | 生成器:客户端模板改为生成传输无关的上下文 / 状态类型,scene→scene_manager / data_service 等全部出站迁移;`GetState` 选点改用新状态机;muduo 三份树补连接失败回调;删除 CQ 轮询 | 2 |
| 4 | 服务端:muduo `TcpServer` + nghttp2 server session;scene / battle handler 模板改为 loop 内异步完成;Agones 许可改异步;关机改 GOAWAY 排空,删关机线程 | 3 |
| 5 | 删除官方 gRPC:proto-gen 停用 `grpc_cpp_plugin`、删 `*.grpc.pb.cc`、28 个工程去掉 grpc 链接项、Linux 改独立构建 protobuf 35.1 / abseil、文档同步 | 4 |

每批完成后交 Codex 编译与验证,通过再开下一批。

第 3 批的前置(2026-10-15 与「C++ RPC 异步回调链式调用设计」会话约定):生成客户端的**每次调用回调**(`done` + 可选 deadline,
签名不含 grpc 类型,向后兼容现有全局 handler)由该会话先在官方 CQ 模板上落地,设计见 [muduo-rpc-channel.md](muduo-rpc-channel.md) §8.4 / §14;
第 3 批以它为唯一出口、只把底下的传输换成 `muduo_grpc::Channel::StartUnaryCall`,调用方不改。在那边宣布完成前,本迁移不碰
`tools/proto_generator` 的 gRPC 客户端模板、`grpc_process.go` 与 `grpc_call_tag.h`。

### 10.1 来自 entt 审计的验收条件

2026-10-15 并行会话「代码中的 entt 使用问题」移交,原文见 [ecs-entt-misuse-audit-20261015.md](ecs-entt-misuse-audit-20261015.md)
「仍未修」表与「修复时的注意事项」。旧模板不在那边修,这里把它们列为迁移的验收条件:

| 审计号 | 问题 | 新传输的要求 | 落在哪批 | 第 1 批状态 |
|---|---|---|---|---|
| F010 | 节点摘除直接 destroy 挂着 CQ 的实体,在途调用永不回调、调用对象不释放 | 摘除时先让在途调用以失败结束(走失败回调),再释放 | 1(muduo 通道)/ 3(删 CQ) | muduo 通道已满足:`~Channel` 把在途调用以 UNAVAILABLE 投递失败回调(§6);官方路径仍是旧行为,第 3 批随 CQ 一起删 |
| F100 / F006 / F082 / F011 | 生成的 Send* 对节点实体无守卫 `get<CompletionQueue>` / `get<StubPtr>`;选点只按 `view<NodeInfo>`,混进 TCP 握手建的、缺这两个组件的实体 | 新模板对通道组件 `try_get`,缺失走失败回调;选点改按通道组件的 view | 3 | gate 路由路径已对 `MuduoGrpcChannelPtr` 用 `try_get`;缺组件时仍落到官方 Send*(旧行为),第 3 批改模板时一并收口 |
| F012 / F022 | `tlsNodeContextManager` 从不 Clear;RunNodeMain 兜底路径与 CQ / stub 未处理 | 关机在两跳排空前清掉所有传输组件;兜底路径同样处理 | 1(部分)/ 2 / 3 | `FinalizeShutdownInLoop` 已清 `MuduoGrpcChannelPtr`;兜底路径与 CQ / stub 待第 2、3 批 |
| C008 | 流式客户端把 registry 里可移动的 protobuf 响应组件地址交给异步 Read(swap-and-pop 后悬空) | 流的响应缓冲放进不可移动的通道 / 调用对象,不作为 entt 组件 | 2(etcd 双向流) | 不涉及(第 1 批只有 unary;调用状态存在 Channel 内部的节点式容器里,地址稳定) |

2026-10-15 ECS 源码收尾补充：上表的“第 1 批状态”为迁移当时记录。文件释放后，Codex 已在官方 CQ 源模板补齐缺传输的异步拒绝、稳定调用缓冲、取消与非阻塞排空，并在节点最终清池前等待在途标签及本地业务回调（包括重入）。同时前置握手类型与发现前缀一致性校验；真实入口回归已加入现有 rpc_controller_test。按负责人“先别编译”要求，尚未重生成、编译或运行，因此不能将这些源码当作本迁移已验收，更不能据此关闭下节 etcd 重连/revision/compact 条目。最新执行范围见 [ECS 总索引 §8](../handoff/ecs-refactor/README.md#8-全部源码收尾补充2026-10-15仍暂缓构建)。

### 10.2 第 2 批(etcd)的验收条件:P0-D「etcd 双向流断开后不重建」

2026-10-15 并行会话「全局单例扩容交接」移交,不在旧 CQ 模板上修。证据与测试设计见
[global-singletons-p0-findings-20261014.md](../handoff/global-singletons-p0-findings-20261014.md)「## P0-D」及其「原报告:修复设计」。
以下结论均为静态分析(基线 `2f897bd07`),**未实测**:

- 生成的 `HandleCompletedQueueMessage` 遇 `ok=false` 只打 `RPC failed` 就 return:tag 不分发不回收、跳过本轮其余事件。
  Watch / LeaseKeepAlive 在任何一次断连后永久失效,约每 181s 本地判失租重注册一轮;推断从第 2 轮起本节点的键会先被 etcd
  删 1~2s 再写回,watch 正常的 gate 会立刻摘掉它。
- 每 2s 的全前缀 Range 只当 PUT 处理,补不回删除:流断期间下线的对端永久留成幽灵节点。
- DELETE 处理依赖 `prev_kv`,开 MVCC 压缩后可能拿不到;`ScheduleWatchReconnect` 只处理 canceled,不处理 `compact_revision`。
- 生成物 FINISH 分支是 `cq.Shutdown()`,真走到会关掉整条 etcd 完成队列。

第 2 批必须满足:

1. 流断(RST / GOAWAY / 靠 HTTP/2 keepalive 发现的静默断链)后按退避重建:0.2s 起、封顶 5s、±20% 抖动、稳定 10s 清零、**不设次数上限**。
   最后一项是对 AGENTS §11.3「重试有次数上限」的有意偏离:etcd 连接是节点身份的生命线,放弃重连等于主动失租;
   与 etcd clientv3 的做法一致。代价是 etcd 长时间不可用时持续重连(每 5s 一次,带抖动,不形成风暴)。
2. KeepAlive 流重建后立刻发一拍,不等 1s 定时器。
3. Watch 流重建后逐前缀做「Range → 对账删除本地多出的节点 → 从 R+1 watch」;快照不完整(`more=true`、值解析失败、键不属前缀)一律不删。
4. 处理 `compact_revision` 取消(重新 Range 后续 watch);DELETE 处理不依赖 `prev_kv`(按键反查本地快照)。
5. 不再有任何会关掉整条完成队列 / 整条连接的流结束分支;流结束只影响该流。
6. 验收口径:删除 / 重启一个 etcd 成员后**无节点 fence 且零重注册**(node-id-overhaul-plan 原口径只有前半句)。
7. 硬约束(移交方同时写进其 PROGRESS 与交接):任何 etcd 滚动(钉镜像版本、加压缩参数)都要等本项验证通过后再做。

回退开关:第 2 批沿用第 1 批的做法,新实现走开关、验证通过前默认仍是旧 CQ 版;**回退到旧实现时上述缺陷依然存在**,
运维手册与启动日志都要写明,回退只用于新实现自身出问题的场景。

## 11. 第 1 批验证(交 Codex,未执行)

见 `docs/PROGRESS.md`「2026-10-15 C++ gRPC 传输改由 muduo 驱动·第 1 批」条目的「交 Codex」一节:依赖构建 → 单测(不链 grpc、线程检查)→
对真实 Go 路由服的互通用例 → gate 两种传输分别跑登录 / 进场 / 断线通知 → 新旧实现**分别**用隔离输入,
不对同一有副作用请求双发。

## 12. 双版本(AGENTS §12)

本改动只换 C++ 服务端内部传输,线上协议、客户端帧格式、proto、tip 码、配置表都不变,不属于两版共享的客户端契约;
Java 版用 Java gRPC,**不适用**。PARITY.md 不登记功能行。

## 13. 第 2 批:etcd 迁到新传输(设计,2026-10-15)

负责人 10-15「做完之后开始落地代码」:第 1 批交付后继续落第 2 批。第 2 批**同样未编译**,与第 1 批一起交 Codex。
验收条件见 §10.1(C008)与 §10.2(P0-D 全部条目)。

### 13.1 范围

- `muduo_grpc` 增加客户端**双向流**与按通道配置的**周期 keepalive**。
- C++ 节点的 etcd 访问(KV.Range / Put / Txn / DeleteRange、Lease.Grant / Revoke、Watch 与 LeaseKeepAlive 两条双向流)
  可改走新传输;开关 `NODE_ETCD_TRANSPORT=grpc(默认)| muduo_h2`,进程启动时读一次,不认识的值按 grpc 并 WARN。
- 注册链(Range → Watch → Grant → 端口 CAS → 分配键 CAS → 起 RPC → 发布)、key 布局、CAS 形态、单槽 pending + 10s 看门狗
  **一律不变**;两种传输共用 `EtcdService` 的同一套应答处理(从生成层的全局 handler lambda 抽成 `EtcdService` 的方法)。
- 不在本批:Windows `wakeup()`、删除 5ms CQ 轮询(其余官方 gRPC 客户端仍依赖它,第 3 批);etcd 多 endpoint 切换。

### 13.2 双向流 API(`muduo_grpc::Channel::StartStream`)

- 返回 `std::shared_ptr<ClientStream>`;回调 `onHeaders`(可选)、`onMessage`(每条消息一次)、`onClose(Status, trailing)`
  **恰好一次**。全部经 `queueInLoop` 投递、保持到达顺序,永不在 `StartStream` / `Write` 内同步执行。
- `Write(message)`:未连上时先在本端缓冲;单流待发上限 1 MiB,超出以 RESOURCE_EXHAUSTED 结束该流(不丢消息、不覆盖旧消息——
  旧 CQ 实现的 circular_buffer(200) 满了会覆盖最旧的 create_request,静默丢 watch)。
- `WritesDone()` 半关闭;`Cancel()` 发 RST_STREAM(CANCEL) 且**之后不再回调**(重建流时用,免得旧流的结束事件再触发一次重建)。
  句柄析构 = Cancel。流没有截止时间。
- 通道 TRANSIENT_FAILURE / SHUTDOWN 时立即 `onClose(UNAVAILABLE)`;IDLE / CONNECTING 时排队,连上再开流。
- 连接断开、对端 RST / trailers、GOAWAY 未处理:`onClose` 给出对应状态;**流不透明重试**,由上层(etcd)按 §13.5 退避重建。
- 流的响应缓冲在流对象内部(不可移动,非 entt 组件),对应 §10.1 C008。

### 13.3 周期 keepalive

- `ChannelOptions::keepaliveTime`(0 = 关,Go 服务通道默认关)。>0 时:连接 READY、有活跃流、且距最近收到的帧已满
  `keepaliveTime` 时发 PING,`keepaliveTimeout` 内无 ACK 判连接死。
- 只给 etcd 通道开:30s / 10s。etcd 的 grpc-go 服务端默认 `--grpc-keepalive-min-time=5s`,Watch / KeepAlive 流常驻,
  30s 一次远高于下限,不会被 `too_many_pings`。Go 业务服务通道仍走 §4.4 的截止触发探测。

### 13.4 etcd 传输接缝

```
EtcdHelper(唯一出口,EtcdManager 的 keepalive 也收进来)
   │ etcd_transport::Get()  —— 进程级、Node 构造前装好
   ├── GrpcEtcdTransport(官方生成客户端 + CQ;回退路径,行为与改前完全一致)
   └── MuduoEtcdTransport(muduo_grpc 通道;本批新增)
            │ 每次调用的完成回调 / 流事件
            ▼
      EtcdResponseSink(由 EtcdService 实现):OnRange / OnPut / OnTxn / OnLeaseGrant / OnWatch / OnKeepAlive
                                         + OnWatchStreamOpened / OnKeepAliveStreamOpened(仅新传输)
```

- core 只认接口(`etcd/etcd_transport.h`)与官方实现;`MuduoEtcdTransport` 也在 core,但只被各节点 main 的
  `etcd_transport::InstallFromEnv()` 引用 —— 静态库按需取目标文件,不引用它的测试工程不需要链 muduo_grpc / nghttp2。
  gate / scene / battle 三个节点工程链 `muduo_grpc.lib` + `nghttp2.lib`。
- `OnRange` 带上原请求(新传输知道是哪个前缀;官方实现传空 —— 生成层的成功回调拿不到请求,所以对账只在新传输上做)。
- unary 失败(新传输能拿到,官方实现拿不到):只记日志,恢复仍靠既有的 txn / lease 10s 看门狗与 2s 全量 Range,语义不变。

### 13.5 流重建与对账(落实 §10.2)

- 流结束(任何原因)→ 按退避重建:0.2s 起、×2、封顶 5s、±20% 抖动、流稳定 10s 后清零、**不设次数上限**(偏离理由见 §10.2)。
- **KeepAlive 流**重开后立刻补发一拍(当前 lease 存在时),不等 1s 定时器。
- **Watch 流**重开后逐前缀「Range → 对账 → 从 R+1 watch」:
  - 本地索引 `key → {node_type, uuid, mod_revision}`(由 PUT 维护,DELETE / 对账删除时清理)。
  - 对账:前缀下本地有、Range 结果里没有、且本地 `mod_revision ≤ R`(Range 的 header.revision)的 key 视为已删除,
    走原有 `HandleServiceNodeStop`(含宽限期)。`mod_revision > R` 的是 Range 之后经 watch 才到的新键,不删。
  - 快照不完整一律不删:`more == true`、任一 kv 值解析失败、kv 的 key 不属于该前缀。
  - 每 2s 的全量 Range 走同一对账(新传输上;它本来就要推进 revision,顺带补回漏掉的删除)。
- watch `canceled`(含 `compact_revision != 0`):该前缀按上一条重新 Range → 对账 → 从 R+1 重建 watch,不再在同一流上
  重发全部前缀(旧实现会给每个前缀叠一个重复 watcher)。`created` 应答按发送顺序把 `watch_id` 对回前缀。
  (落地时细化:只有压缩取消立即重新 Range,其他取消等下一次 2s 全量 Range,见 §13.9。)
- DELETE 事件**不依赖 prev_kv**:先用 prev_kv;没有时按 key 查本地索引拿到 NodeInfo 再走 `HandleServiceNodeStop`。
- 注册链的触发点不变:第一个 watch `created` 应答触发 LeaseGrant;重建后的 `created` 不再触发。

### 13.6 地址解析

- `Etcd.Hosts` 第一个地址(与旧实现相同)。IPv4 字面量直接用;主机名(K8s 的 `etcd.<ns>:2379`)在 **Node 构造期**
  (loop 尚未运行)用 `getaddrinfo` 解析一次,失败每秒重试、最多 10 次,仍失败则 LOG_FATAL 拒绝启动。
- 解析结果在进程生命周期内固定:K8s Service 的 ClusterIP 在 Service 存续期间不变;Service 被删除重建需要滚动节点。
  运行中不做 DNS(loop 线程不得阻塞;无线程的异步 DNS 要引入 c-ares,不在本批)。

### 13.7 关机

- `FinalizeShutdownInLoop` 里 `ReleaseNodeId()` 发 LeaseRevoke 之后,新传输的通道随 `EtcdService::Shutdown` 释放。
  LeaseRevoke 只是**尽量送达**:通道 READY 且 muduo 输出缓冲为空时,提交那一刻 `send` 已把字节直接写进 socket;
  否则字节还在用户态输出缓冲里,随 `forceClose` 丢弃,lease 等 TTL 到期由 etcd 回收。应答不等(与旧实现一致)。
  (落地时核实:库里没有单独的「关前排空」步骤;要保证送达需让连接在关机两跳里多活到写完,收益不抵复杂度,不做。)
- 流句柄 Cancel、退避定时器取消;通道析构走 §6 的排队释放。

### 13.8 回退

`NODE_ETCD_TRANSPORT` 不设或设 `grpc` 即回到旧实现;**回退时 §10.2 列出的缺陷依然存在**(启动日志写明当前选择)。
任何 etcd 滚动(钉镜像版本、加压缩参数)要等新实现验证通过之后再做(§10.2 第 7 条)。

### 13.9 落地记录(2026-10-15,未编译)

代码与上面的设计一致,以下是实现时定下、设计里没写细或有调整的点:

- **文件**:接口 `core/node/system/etcd/etcd_transport.{h,cpp}`(含进程级工厂 `SetFactory` / `Create`);
  官方实现 `grpc_etcd_transport.{h,cpp}`;新实现 `muduo_etcd_transport.{h,cpp}`(`InstallFromEnv` 也在这里);
  纯函数头 `etcd_endpoint.h`(地址拆分)、`etcd_stream_backoff.h`(流重建退避)、`discovery/discovery_key_index.h`(发现键索引)。
- **5ms CQ 轮询留在 `EtcdService`**:它驱动的是进程里**全部**官方 gRPC 客户端的 CQ(不只 etcd),两种 etcd 传输下都要跑,
  第 3 批删 CQ 时一并删。
- **`OnWatchStreamRestarted` / `OnKeepAliveStreamRestarted` 在每次重建开流后立即回调**(首条流不回调)。etcd 的 watch
  流在收到第一个 create 之前不发响应头,无法以「对端已确认」为准;重建开流时通道若仍不可用,流会再次以 UNAVAILABLE 结束,
  重新 Range 失败只记日志,下一次重建再来一遍。
- **watch 取消**:`compact_revision != 0` 立即对该前缀重新 Range;其他原因的取消(含创建失败的 created+canceled)等每 2s 的
  全量 Range 重建,对端持续拒绝时不空转。`created` 按 create 的发送顺序对回前缀(etcd 在一条流上串行处理 create)。
- **发现键索引带墓碑**,比设计多挡了两类乱序(Range 与 watch 走不同的流):比已处理的删除更旧的快照条目不再把下线节点加回来;
  比已知 PUT 更旧的 DELETE 事件忽略。对账删除也在快照 revision 处记墓碑。墓碑由之后的快照清理(快照 revision ≥ 墓碑即清)。
- **只有发现键进索引**:`/allocated/` 占位键(值是裸 uuid)照旧交给发现层由它跳过,不参与「值解析失败 → 快照不完整」判定。
- **同一个键换了持有者**(评审补):对端重启按「最小空闲 id」拿回同一 node_id、以新 uuid 写回同一个键时,快照里键还在,
  只删「快照里没有的键」的对账摘不掉旧 uuid;它的 DELETE 又可能因比快照旧被判过时。索引的 PUT 返回被覆盖的旧值与旧持有者,
  新旧 uuid 都解析得出且不同就把旧持有者记为待摘。Range 与 watch 两条路径都做。
- **按 uuid 判「还在不在」**(回归评审补):发现层按 uuid 摘节点,而同一 uuid 可能同时挂在两个键上 —— Go 侧
  `ReallocateNewID`(friend / match)失租换号时保留 uuid、换 node_id,match 的注册表也可能把自己的旧键误判成被占而换号。
  所以三种摘除来源(换持有者、对账删除、watch DELETE)一律先记为待摘,**整批**(一个 Range 快照 / 一个 watch 应答)应用完后,
  只摘索引里已不再挂任何键的 uuid(索引按持有者计数)。同一批里先 DELETE 后重新 PUT 的节点也因此不再先摘再加。
  uuid 解析不了时无法判断,照旧交给发现层。跨应答的先后仍按到达顺序处理(先收到 DELETE、下一个应答才收到新键的 PUT 时,
  节点会被摘一下再加回,与旧实现相同)。
- **前提:etcd revision 单调递增**(etcd API 不变量)。水位、墓碑、`mod_revision` 判过时都依赖它。用 `etcdutl snapshot restore`
  恢复时必须带 `--bump-revision` / `--mark-compacted`;换成一套全新的 etcd(数据清空、revision 从 1 重计)时所有节点都要重启
  (deploy/k8s/README 已有此要求)。压测清库用 `etcdctl del --prefix`,不会让 revision 回退。
- **每前缀已应用快照水位**(评审补):同一前缀多个 Range 同时在途、乱序到达时,比已应用快照旧的应答整条丢弃(不应用 PUT、
  不对账);watch 一律从 `max(R, 水位) + 1` 建,重建前发出的旧 Range 晚到也不会从更旧的 revision 回放。水位跨流重建保留。
  索引把「本地无记录、墓碑不早于它」的 DELETE 判为重放,不再交给发现层(否则宽限期被重置一遍)。
- **etcd 通道接收上限 64MiB**(评审补,默认 4MiB):单类约 4000 个节点的 Range 应答就会超 4MiB,超了每次失败、该前缀永远建不了 watch。
- **双向流评审补**:对端带 END_STREAM 结束流而本端方向还开着时补 RST_STREAM(NO_ERROR)(否则 nghttp2 只置 half-closed,
  onClose 永远不来);待发缓冲里已交给 nghttp2 的前缀及时挪掉(否则内存随累计发送量增长);待发上限只在已有积压时生效
  (缓冲为空总放行一条);周期 keepalive 按「最近收到的帧 + keepaliveTime」上弦;拆包出错前已完整的消息先交付;
  onHeaders 只对 200 + application/grpc 回调;StartStream 拒绝缺 onClose;loop 线程外的 Cancel 只置标记不动表。
- **节点模板**:`cpp/nodes/_template/main.{simple,with_context}.cpp.example` 也在 main 开头调用 `etcd_transport::InstallFromEnv()`。
- **新传输的通道**:周期 keepalive 30s / 10s;连接级重连退避封顶 5s(默认 120s,etcd 恢复后要几秒内连回);unary 截止时间
  取 `grpc_call_deadline::Get(EtcdNodeService)`,与官方客户端同一配置;同一方法的失败日志 10s 限频。不发 `hasleader`
  元数据(与旧实现一致;成员失去 leader 的探测与多 endpoint 切换同属后续)。
- **`ClientStream` 弱持有流记录**:`Cancel()` 先就地置取消标记,通道已析构、`onClose` 已在队列里时也能压住(重建流时防止旧流的
  结束事件再触发一次重建)。
- **工程**:core 加 3 个 .cpp / 6 个头;scene / battle 的 Debug|x64 与 Linux CMake 链 `muduo_grpc` + `nghttp2`,`game.sln`
  给两者加对 muduo_grpc 的依赖。Release 配置与 gate 第 1 批一样没动(那几份 Release 链接清单本来就不全)。
  顺带补上 §9 写了、但手工维护的节点 CMakeLists 里一直缺的 `-Wl,--exclude-libs,libnghttp2.a`(gate / scene / battle 三处;
  第 1 批时它只进了已归档的 `vcxproj2cmake.py`)。
- **用例**:`muduo_grpc_test` 的 channel_test 末尾 6 条(双向流顺序 / 恰好一次、Cancel、通道析构、目标不可达、超限、
  周期 keepalive);新增 `etcd_transport_helpers_test.cpp`(索引、地址拆分、退避,共 14 条)。EtcdService 上的整合行为依赖
  Node 全局状态,按 PROGRESS 条目里的 etcd 故障注入步骤验证。

## 14. 第 3 批:生成客户端全部改走 muduo_grpc、删 CQ(设计,2026-10-15)

负责人 10-15「不用管验证,全部做完」:第 3–5 批不再等上一批验证,连续落地;全部**未编译、未重生成**,交负责人统一构建。
**当前状态**:第 3 批做到一半后按负责人要求转交其他会话,进度、剩余清单与五份现状调查见
[handoff/grpc-muduo-batch3-5/README.md](../handoff/grpc-muduo-batch3-5/README.md)。
2026-10-16 接手会话已补完第 3 批代码(仍未编译),落地记录见 §14.3。负责人同日澄清目标:**不是删 gRPC 库,而是运行期
不起任何 gRPC 线程、线程数可控**;第 5 批据此改为「无 gRPC 线程守卫 + 清理」,不换依赖、不删 grpc 子模块(见交接 README)。

### 14.1 决定

- **不再保留官方客户端分支**:生成的 `Send<Svc><Method>` 一律走节点实体上的 `MuduoGrpcChannelPtr`
  (`muduo_grpc::Channel::StartUnaryCall`)。第 1、2 批的进程级开关(`GATE_ROUTER_TRANSPORT`、`NODE_ETCD_TRANSPORT=grpc`)
  随之失效:留着开关就删不掉 CQ,而第 5 批本来就要删官方运行时。`NODE_ETCD_TRANSPORT=grpc` / `GATE_ROUTER_TRANSPORT=grpc`
  改为打 WARN 后仍走 muduo_h2。
- **全局处理器保留,但签名去 grpc 化**:`std::function<void(const GrpcReplyContext&, const Resp&)>`。
  `GrpcReplyContext` 是 `GrpcCallOutcome` 的别名,带响应头 / trailers 的 metadata(`-bin` 已解掉线上那层,与官方
  `GetServerInitialMetadata` 同义),所以按回显认领的调用方(gate 会话、号段 `x-idseg-seq`)只改一行。
  `GrpcCallFailure` 里的 `grpc::ClientContext& / grpc::Status&` 换成 `const GrpcCallOutcome& outcome`。

  > **2026-10-17 契约勘误**：以上是第 3 批的兼容阶段。删除全局处理器 A–C 落地后，`GrpcReplyContext / GrpcCallFailure` 与所有全局处理器、安装入口删除，done 必填且是唯一完成通道；详见 `grpc-muduo-transport.md` §16.1。

- **`GrpcDone` 不变**,`GrpcCallOutcome` 多带响应 metadata,纯增字段。
- **调用状态不进 ECS**:请求副本、发出的 metadata 原值、done 放在 `shared_ptr` 里,随完成回调走;不再有对象池、tag、
  `GrpcClientRuntime` 计数。本地拒绝(实体无通道 / 序列化失败)经 `queueInLoop` 延后投递,保持「永不在 Send 栈内回调」。
- **线上字节不变**:`:path = "/<proto package>.<Service>/<Method>"`(无 package 时 `/<Service>/<Method>`,battle_node)由模板直接拼;
  metadata 仍是应用层先 `Base64Encode` 一层、`-bin` 键再由传输层编一层(§4.2);截止时间按 steady_clock、线上 `grpc-timeout`。
  应答解析失败报 INTERNAL(与官方客户端一致)。
- **流式方法不生成客户端**:全仓只有 etcd 的 Watch / LeaseKeepAlive,已由 `MuduoEtcdTransport` 手写。为不打断
  `gRpcMethodRegistry` 的通用 sender,流式方法仍生成三个旧 Send 重载,调用即记 ERROR 拒绝;纯服务端流同样处理
  (修掉旧模板把它误生成成 unary 的问题)。

  > **2026-10-17 契约勘误**：流式拒绝桩与对应通用 sender 随后删除；生成客户端只输出 unary 的 typed / Erased done 入口，etcd 双向流继续使用手写传输。

- **CQ 与 5ms 轮询删除**:`InitGrpcNode`、`HandleCompletedQueueMessage`、`BeginShutdownGrpcNode`、`PollGrpcNodeShutdown`、
  `GrpcClientRuntime`、`GrpcChannelCache`、`EtcdService` 的 5ms 定时器、`GrpcEtcdTransport` 一并删除。
  关机阶段 0 清 `MuduoGrpcChannelPtr`(~Channel 把在途调用以 UNAVAILABLE 排队),拆除经 `queueInLoop` 排在它们之后;
  节点摘除直接销毁实体,不再 5ms 轮询排空。
- **建连**:`NodeConnector::ConnectToGrpcNode` 对所有 gRPC 节点类型直接建 `muduo_grpc::Channel`(IPv4 字面量,§8),
  建好即 `GetState(true)` 预连。`MuduoGrpcChannelFactory` 与 gate 的 `InstallChannelFactory` 删除。
- **选点**:`HasUsableGrpcClient` 改为「挂着通道且非 SHUTDOWN」;`GetSceneManagerEntity` / 号段 / 收件箱选点改按
  `muduo_grpc::ConnectivityState` 分级。
- **muduo**:补连接失败回调(Windows 编译树 `muduo_windows/src` + 使用方头文件副本 `third_party/muduo` 逐字同步 +
  Linux overlay),`Http2Transport` 收到即进 TRANSIENT_FAILURE、不再等 20s;修 Windows `EventLoop::wakeup()` 空操作
  (删 5ms 轮询后它不再被掩盖)。
- **依赖面**:core 与 grpc_client 从此链 muduo_grpc + nghttp2;所有链 core / grpc_client 的工程在第 5 批统一改链接项
  (同时去掉 grpc)。**2026-10-16 订正**:第 5 批改口径后不去掉 grpc 链接项 —— core 的 `grpc_runtime_probe.cpp` 调
  `grpc_is_initialized()`,链进 node.obj 的工程必须继续链 grpc.lib / gpr.lib;muduo_grpc + nghttp2 链接项已在第 3 批补齐(§14.3)。

### 14.2 行为变化(调用方须知)

- 停机 / 摘节点时在途调用一律 UNAVAILABLE(旧官方路径是 CANCELLED);`GrpcDone` 契约早已要求不按 code 区分。
- 摘节点时 done 在实体销毁**之后**才执行:回调不得捕获实体句柄或组件指针(现有调用方都按值捕获,已核对)。
- 官方通道「空闲 30 分钟回 IDLE」「空闲也 ping」消失,改为截止触发探测(§4.4)。
- 对端拒连立即进 TRANSIENT_FAILURE(连接失败回调落地后),退避 1s ×1.6 封顶 120s,期间调用立即失败(与官方 gRPC 一致)。

### 14.3 落地记录(2026-10-16,未编译)

10-15 22:27 负责人 / 另一会话已按新模板跑过 proto-gen,`cpp/generated/grpc_client/**` 不再有 CQ / `ClientContext` / grpcpp。
接手会话据此补完调用方与其余项,七组并行落码、每组一轮语义评审。全部**未编译、未运行**。

- 同一次 regen 把 mail / rank 两组客户端加进了 `grpc_init_client.cpp`。它们的 `*_grpc_client.cpp` 与 `*.pb.cc` 已由另一会话
  登记:Windows 在 `grpc_client.vcxproj` / `proto.vcxproj` 末尾 `Microsoft.Cpp.targets` 导入之后的 `ProtoGeneratorSources`
  段,Linux 在两份 CMakeLists。构建日志里要确认这几个文件确实被编译(项在导入之后,按 MSBuild 求值顺序应生效)。
- **调用方**:
  - session:`GetSessionDetailsByClientContext` 改名并改签名为 `GetSessionDetailsByReplyContext(const GrpcReplyContext&)`,
    用 `FindInitialMetadata("x-session-detail-bin")` 取值,值语义不变(§4.2)。

    > **2026-10-17 契约勘误**：`GetSessionDetailsByReplyContext` 已退场；`ParseSessionDetailsMetadataValue` 仅供路由 Forward 完成回调解析回显会话，直连按发送时捕获的原始会话字节下发。

  - gate main:全局处理器换新签名;`GATE_ROUTER_TRANSPORT` 不设或 `muduo_h2` 时静默,其余值(含 `grpc`)打一条 WARN 后照走
    muduo_h2;`gate_router_transport.h` 与 6 条 `GateRouterTransport.*` 用例删除。关机排空判定(beforeShutdown 关掉的会话
    走完断线处理 + 路由通道 `AllChannelsIdle`,上限 15s)改为**路由模式一律安装**,以前只在 muduo_h2 下;直连模式不变。

    > **2026-10-17 契约勘误**：gate main 不再安装任何全局应答 / 失败处理器；直连通用发送与 Disconnect 都在调用处显式提供 done，Forward 继续走自身完成回调。上述传输退役与关机排空规则不因此改变。

  - `router_muduo_forward`:删 `InstallChannelFactory`;`SendForward` 保留(第 1 批路径,本身无单测),线上字节按代码核对
    与生成的 `SendClientRpcRouterForward` 一致(`:path`、应用层一层 Base64、同一份截止配置、解析失败 INTERNAL)。
  - `client_message_processor`:`SendRawViaRouter` 只剩 muduo 通道一条路,缺通道守卫改为 `try_get<MuduoGrpcChannelPtr>`
    (采样 WARN 后返回空,与原守卫失败时一致);`PickRandomNode` 只看 `HasUsableGrpcClient`。
  - 号段 `id_segment_bootstrap`、收件箱、scene_manager 应答适配:只迁签名(没有改成每次调用回调);`x-idseg-seq` 回显比对
    同时认原文与 Base64,语义不变;收件箱用 `kGrpcStatusUnimplemented` 代替 grpc 枚举。

    > **2026-10-17 契约勘误**：号段、收件箱、EnterScene / CreateScene 均已迁到每次调用回调，scene 的 gRPC 回包适配文件及安装函数删除；号段捕获 seq，收件箱捕获三项，EnterScene 捕获 playerId / requestTag，不再以响应 metadata 查归属。

- **选点**:`Candidate.channelState` 改为 `std::optional<muduo_grpc::ConnectivityState>`;`node_utils.h` 只前向声明枚举
  (`enum class ConnectivityState : uint8_t`,该头经 `node.h` 进入大量翻译单元)。号段 / 收件箱选点改为
  `view<NodeInfo, MuduoGrpcChannelPtr>` 且只认 `GetState(true) == kReady`。
- **muduo 连接失败回调**(两份 Windows 树逐字一致 + Linux 新增 overlay,均标 LOCAL DELTA):
  - `Connector::setConnectFailedCallback`:`retry()` 关 socket、回到 kDisconnected 之后、排下一次重试之前回调;`connect()` 的
    致命分支与 default 分支也回调;只在 `connect_` 为真时回调,回调返回后重读 `connect_`。回调里**不得同步 start / restart**
    (`removeAndResetChannel` 排队的 `resetChannel` 尚未执行,当场重连会被它误删新的 `channel_`)。`stop()` 改绑 `shared_from_this()`;
    2026-10-17 起 `start()` 与 `resetChannel` 也改绑 `shared_from_this()`,`connecting()` 里绑进自有 `channel_` 的回调按 §11.7「同寿命成员」例外保留并注明理由。
  - `TcpClient` 只加 out-of-line 转发与一个嵌套 typedef,**无数据成员**,布局不变。
  - Windows `Channel.cc`:设了 errorCallback、没设 closeCallback 的 channel(本仓只有 Connector)遇 POLLHUP 不再提前返回,
    先投 error 再投 write;其余 channel 逐字不变。2026-10-18 Codex 本机原生 WSAPoll 实测回环非阻塞拒连报
    `POLLHUP|POLLERR|POLLOUT`(`0x0013`),随后读取 `SO_ERROR=10061`;只报 POLLWRNORM 时走 handleWrite 读 SO_ERROR,同样到失败回调。
  - `Http2Transport`:失败回调只绑 weak,先 `client_.stop()` 停掉 Connector 内部重试,再经 `queueInLoop`(绑 weak)
    `CloseWithError(UNAVAILABLE)`,不在 Connector 回调栈或 `CreateTransport` 栈上关闭;与 connect_timeout、Shutdown、`~Channel`
    由 `closed_` 收口,恰好关闭一次。§5「已知差异」随之消除。
- **Windows 唤醒**:`EventLoop.cc` 的 WIN32 分支改为自连接回环 UDP socket(`EventLoop.h` 不变),`wakeup()` 发 1 字节、
  缓冲满忽略;`handleRead()` 读到 EWOULDBLOCK 为止(WSAPoll 水平触发,不读干净会空转)。每个 EventLoop 占一个
  127.0.0.1 UDP 临时端口。修复后 Windows 上「pending functor 里再排」也立即唤醒,任何自我重排的写法会从每 100ms 一次变成忙等
  (生产代码只查到 `FinalizeTeardownInLoop` 自我重排,有界)。
- **测试**:
  - 删 `grpc_client_lifecycle_test.cpp`;新增 `grpc_client_local_reject_test.cpp`(无通道 → done 异步恰好一次 UNAVAILABLE、
    不触发全局处理器、`GrpcClientLocalCompletionsDrained` 判据)。PerCallDone 成功 / 停机两条与「快照 / 已关闭实体不能被选中」
    的适配层用例待第 4 批或选点组补。

    > **2026-10-17 契约勘误**：全局处理器删除后的本地拒绝测试只覆盖显式 done 的异步恰好一次与排空；旧的“不触发全局处理器”断言删除，生成器测试另守住无全局接口与同步序列化。实际测试结果另记。

  - `cross_zone_test`:假 SceneManager 节点改挂不连的 `MuduoGrpcChannelPtr`,夹具自带 EventLoop。
  - 选点用例换五态;muduo_grpc_test 新增 `RefusedConnectionEntersTransientFailureWithoutWaitingForConnectTimeout`
    (connectTimeout 20s、断言 5s 内失败;Windows 协议栈对回环拒连还会重试 SYN,经验值 1~2s)与 `event_loop_wakeup_test.cpp` 三条。
- **构建**:23 个测试工程与 rpc_controller_test、cross_zone_test 的 Debug|x64 加 `muduo_grpc.lib;nghttp2.lib` 与 nghttp2 库目录;
  `game.sln` 给链 core / grpc_client 的测试加对 muduo_grpc 的依赖。rpc_controller_test、aoi_delivery_test、
  consistent_hash_node_test 不在 sln 里,要先编 sln 再单编(consistent_hash_node_test 本来就是死工程,`run_cpp_tests.ps1` 已排除)。
  Release / Win32 配置未动(gate 的 Release 链接项第 1 批起就缺 muduo_grpc / nghttp2)。
- **行为变化补充**(在 §14.2 基础上):
  - 路由模式 gate 关机最多多等 15s,换来断线通知能发出、login 侧会话转离线。
  - 拒连后只剩 Channel 自己的退避(1s×1.6,业务通道封顶 120s),对端重启后的恢复可能比原来慢;是否下调业务通道
    `backoff.max` 待负责人定。
  - Windows 上 RPC0 的 `RpcClient` 拒连也会真正走 `Connector::handleError` 按 0.5s 起翻倍重试并打 ERROR(以前推断停在
    kConnecting 空转)。
  - Windows 本地压测数据与旧轮次不再可比(跨线程投递与 scene / battle 同步服务端的 runInLoop + future 等待都变快)。
- **未做**:~~muduo TimerQueue 单调时钟(可选项)~~(2026-10-16 已做,见 §6);~~Connector 其余绑裸 this 的上游 FIXME~~(2026-10-17 已做:start / resetChannel 改绑 `shared_from_this()`,connecting 按同寿命例外保留;
  TcpClient 自身的裸 this(closeCallback、交给 Connector 的 newConnectionCallback_)是上游设计、有状态守护,改要动 TcpClient.h 布局,按「第三方树最小增量」保留)。`client_request_lane.{h,cpp}` 的过时注释已由遗漏检查改准;随后 ECS 审查转交的 W1-45 删掉了 lane 里重复的摘节点机制(`OnRouterNodeRemoved` / `routerEntity` / `nodeRemoved` / `InflightIndex::ExtractIf`),路由服摘除改由 `~Channel` 的 UNAVAILABLE 经失败桥认领,见 client-request-ordering.md §4.5。

## 15. 第 4 批:服务端改走 muduo + nghttp2(设计,2026-10-16)

负责人 10-15 对第 4 批 11 个待定问题选「按最标准做法由你定」。本节由三份独立设计(正确性与生命周期 / 最简单可审 / 线上契约与运维)经裁决综合而成,裁决时逐条对照代码核实论据。**状态:2026-10-16 已按本节落码(七组并行 + 每组语义评审 + 全仓遗漏检查),未编译、未重生成;实现与本节的偏离和待拍板的取舍见 §15.16。**

### 15.1 目标、范围与验收

负责人原话:「grpc 库还是可以用,但是不要启动 grpc 任何线程……我需要的是线程数量可控」。本批据此做到以下几点:

- **不再构造官方服务端对象。** scene / battle 不再构造 `grpc::Server`、`ServerBuilder`、`ServerContext`、`CompletionQueue`。服务端的接入、HTTP/2 协议、派发、定时器、关机,全部跑在节点 EventLoop 上,**新增线程数为 0**。第 3 批之后客户端也不再构造官方对象,所以官方运行时不会被 `grpc_init` 拉起。
- **线上字节与 Go 依赖的状态码契约不变。** 三条手写文案逐字保留:
  - scene `CreateScene` 回 `UNAVAILABLE "agones allocate not confirmed"`;
  - battle `CreateBattle` 回 `UNAVAILABLE "battle_not_allocatable"`。这是跨语言契约,`go/match/internal/logic/gather.go:66/677` 按状态码加文案精确匹配;
  - battle `DestroyBattle` 回 `FAILED_PRECONDITION "placement_operation_mismatch"`。
  - 其余业务错误照旧放在应答字段里,status 为 OK。
- **不删依赖。** grpc 库和 `*.grpc.pb.*` 都保留,服务端只是不再 include 它们。
- **不设回退开关**(见 §15.3 Q1)。
- 本节只是静态设计,未编译;验证按 §15.14 交 Codex 执行。

### 15.2 现状核对(以 2026-10-16 工作区为准,survey 中过时的说法在此更正)

| # | survey / README 的说法 | 现状 | 影响 |
|---|---|---|---|
| 1 | 13 个 unary 方法 | **15 个**:scene 9 个(新增 `AssetAckReceipt`),battle 6 个(新增 `QueryBattleCreation`,排在第一)。全部 unary,全部是同步的 `Handle*` | 方法表和测试都按 15 个做 |
| 2 | 手写的非 OK 只有两处 UNAVAILABLE | battle 的 `grpc.DestroyBattle.in_loop` 已有手写的 FAILED_PRECONDITION(`battle_node.cpp:215-218`,另一会话未提交) | 非空手写段共 5 段(§15.8.4)。迁移以**工作区**为准,不得 `git show HEAD:` |
| 3 | 关机链里有 `DrainGrpcClientsInLoop` | 第 3 批已删。阶段 0 清 `MuduoGrpcChannelPtr`;拆除开头按 `GrpcClientLocalCompletionsDrained()` 让一轮(`node.cpp:1191-1241`) | 关机按 §15.10 |
| 4 | node.cpp 有并行改动 | node.{h,cpp} 当前没有未提交改动。行号:`~Node` :436,`RegisterGrpcService` :730,`StartGrpcServer` :740,`ShutdownGrpcServer` :816,`ShutdownInLoop` :1113,`MaybeFinalize` :1161,`Finalize` :1191,`Teardown` :1222 | — |
| 5 | Windows 的 wakeup 是空操作 | 第 3 批已改成自连接 UDP(未提交,§14.3) | 派发泵与 Agones 投递依赖它,算前置条件 |
| 6 | README §4 写的是 TcpServer | 本批改用 Acceptor 自管连接(§15.4.1) | README 要同步改 |
| 7 | 需求 12「回退开关」 | 与目标、§14.1 冲突 | 删除这条需求 |
| 8 | 需要配 nghttp2 防洪选项 | 1.70 默认已全部开启(`nghttp2_session.h:96-111`) | 本批把默认值显式写出,锁住将来的漂移 |
| 9 | 超过 MAX_CONCURRENT_STREAMS 会怎样 | 对端 ACK 我方 SETTINGS 之前:REFUSED_STREAM;ACK 之后:连接级 PROTOCOL_ERROR(`nghttp2_session.c:3891-3909`) | — |
| 10 | GOAWAY 之后的新流 | shutdown notice 不置 GOAWAY_SENT,新流照常收(`nghttp2_submit.c:205-215`)。最终 GOAWAY 之后新流被忽略(`:3897`),客户端按 last_stream_id 判为未处理 | 两段式见 §15.5.6 |
| 11 | 探针要静默处理 | Linux 上游 `TcpServer.cc:80/109` 每次建连 / 断连各打一行 INFO。服务端先写 SETTINGS、对端再 RST,会触发 `handleError` ERROR | Acceptor 加「推迟写 SETTINGS」 |
| 12 | 服务端要配 keepalive | `TcpConnection` 构造时已经 `setKeepAlive(true)`(两份树的 `TcpConnection.cc:63`) | 不做 HTTP/2 层 keepalive |
| 13 | — | 除 muduo_windows 与 third_party 外,所有 vcxproj 都跑 no-raw-pointer-member 检查(`Directory.Build.targets:57`,`fieldDecl(hasType(pointerType()))`,函数指针也算) | 新类型**不得有 `T*` 字段** |
| 14 | 要改 handlers.go / grpc_handler_target.go | 路由和输出文件都不变,这两个文件不用改。但通用管线是**先写 .h、再生成 .cpp**(`handler.go:41-75`) | 旧段检查必须在写 .h 之前做(§15.8.3) |
| 15 | — | Go 调用方都没配 grpc keepalive(已 grep `scene_manager` / `match` / `shared/scenenode`) | 服务端不做 PING strike |

### 15.3 决定(11 个待定问题,按「最标准做法」裁决)

| 题 | 决定 | 依据 | 代价 |
|---|---|---|---|
| Q1 回退开关 | **不设**。模板只生成新形态。回滚就是换回上一版镜像;灰度先发 1 个 scene 和 1 个 battle | 回到官方服务端就会重新拉起 poller 和 EventEngine 线程;§14.1 已定不保留官方分支;线上协议不变,新旧节点可以与同一批 Go 调用方并存 | 只能整版回滚 |
| Q2 在途上限 | 两层:<br>① 每连接 SETTINGS `MAX_CONCURRENT_STREAMS=256`;<br>② 全节点在途 **1024**,超限在业务之前回 trailers-only `RESOURCE_EXHAUSTED "server in-flight call limit (1024) reached"`。<br>派发泵每轮至多 64 个，在调用之间检查 2ms 预算，至少处理 1 个；单个 handler 不可抢占。每连接未收完的请求体 ≤16MiB。<br>Go `reconcile.go` 的 `Workers=8` 不动,只改注释 | MCS 是 HTTP/2 原生背压(RFC 9113 §6.5.2 建议 ≥100;Go net/http2 默认 250):grpc-go 在客户端排队,没发出的请求必定没执行。<br>RESOURCE_EXHAUSTED 是规范里的资源耗尽码,grpc C++ sync server 线程配额耗尽时也回它。<br>不用 REFUSED_STREAM:grpc-go 会立刻在同一连接透明重试。<br>2ms 预算占 50ms 固定步长的 4%，不代表硬执行上限 | 极端突发时出现 RESOURCE_EXHAUSTED(业务未执行,调用方按失败重试) |
| Q3 停接新连接 | 用 `Acceptor` 加自管 `TcpConnection`;停止监听就是经 queueInLoop 销毁 Acceptor;三份 muduo 树都不改。etcd 发现键**不提前撤**,仍在 `ReleaseNodeId` 随租约撤销 | grpc-go `GracefulStop` 和 grpc C++ `Shutdown` 都是先关监听。提前删键会让 scene_manager 改派,与本节点还在落地的存盘竞争(回档) | 排空窗口里 Go 仍能发现本节点,但拨号立刻 ECONNREFUSED、fail-fast,与今天一致 |
| Q4 排空 | `kGrpcDrainTimeout=2s`,从 BeginShutdown 起算(在 hook 之后)。<br>第二段 GOAWAY 在收到 PING ACK 或满 1s 时发,取先到者。<br>最终 GOAWAY 之前收到的流照常处理。<br>2s 到期:业务未开始的回 `RST_STREAM(REFUSED_STREAM)`;已开始的回 `UNAVAILABLE "server shutting down"`。<br>之后半关,宽限 1s 再 forceClose,总上限约 3s | hook 里的 `Stop()` 已在排空之前拒掉所有许可等待者,Handle* 又都是同步的,所以通常一个 RTT 就排空。<br>grpc-go 在第一段 GOAWAY 后照常接受新流(`http2_server.go:1404-1407`)。<br>REFUSED_STREAM 是 RFC 9113 §8.7 的「未处理」;UNAVAILABLE 属于 Go 的「结果未知」(`spectate.go:387-392`) | 旧路径到期多是 CANCELLED,现在是 UNAVAILABLE 或 REFUSED |
| Q5 截止 | 服务端用单一截止定时器(照 `Channel` 的 `deadlines_` 有序集合 + steady 复核):<br>投递前到期 → DEADLINE_EXCEEDED,不派发;<br>等许可时到期 → DEADLINE_EXCEEDED 完成调用,许可回来后不进业务;<br>业务已开始 → 立即回 DEADLINE_EXCEEDED,业务不打断,迟到的完成丢弃并计数 | grpc core 服务端的行为就是这样(今天线上跑的就是它);中途打断会制造半完成 | 多一个定时器加一个有序集合 |
| Q6 延后完成 | 框架支持(ServerCall 可拷贝、只持 weak);**`Handle*` 签名和函数体都不变** | 异步许可本来就需要延后完成;目前没有 handler 需要接管(YAGNI) | 将来再加带 call 参数的变体 |
| Q7 异步许可 | `GameServerLifecycle::AcquireAllocationPermitAsync(EventLoop&, cb)`:结果一律 queueInLoop 回 loop;超时由该 loop 的定时器兑现;`Stop()` 拒绝全部等待者;析构只丢弃、不投递。`SceneHandler::CreateScene`(RPC0)的 NonBlocking 不动 | 等待者能否拿到结果,必须与写 Allocated、晚到收口在同一把 mutex 里裁决;infra 本来就依赖 muduo,测试本来就链 `muduo.lib` | Blocking 版保留,只给测试和非 loop 线程用 |
| Q8 bind | 绑注册 IPv4(`grpc_endpoint.ip`),启动时校验必须是规范 IPv4 字面量,否则 `LOG_FATAL`;`reuseport=false`;不读 `BIND_IP` | 暴露面与今天官方服务端完全一样;资产 RPC 只靠请求体签名(`asset_op_auth.h:9-13`);muduo `InetAddress` 遇到非法 IP 会静默落成 0.0.0.0 | IPv6 的 POD_IP 会启动失败(与 §8 的客户端侧同一限制) |
| Q9 scene 停机准入 | **只给 CreateScene 补**。框架提供 `ServerCall::ServerDraining()`;守护段在取许可前、许可回到 loop 后各查一次,为真就回 `UNAVAILABLE "scene_node_shutting_down"` | 排空时拒绝新的变更型工作是标准做法(与 battle 准入闸同理);SceneManager 收到任何错误都会回滚(`createscenelogic.go:297-309`) | 多一条文案(不是跨语言契约) |
| Q10 metadata | `ServerCall::metadata()` 只读,只含自定义键,`-bin` 已解掉线上那层;不提供回写 | 等价于 `ServerContext::client_metadata()`;会话层本来就要解 `-bin` 来回 400 | 无 |
| Q11 CreateScene 零值校验 | 本批不改,另立任务 | 传输迁移不夹带业务语义变化(§11.2) | 暂时与 RPC0 口径不一致 |

另外两条约束:

- **关机顺序保持现状**:StopConsumers → hook → 看门狗 → BeginShutdown → MaybeFinalize。同一个 loop 任务里,hook 与 BeginShutdown 之间不可能派发任何调用,所以先后对语义等价;而保持现序可以让 hook 的阻塞时间不吃排空预算。
- **握手超时 120s**(grpc 两家的默认值)。**不做 PING strike**。

### 15.4 结构

#### 15.4.1 TcpServer 还是 Acceptor

| | muduo `TcpServer` | **`Acceptor` + 自管 `TcpConnection`(采用)** |
|---|---|---|
| 停止监听 | 没有接口。要么三份树逐字同步补一个,要么接入后立刻 GOAWAY 再关(排空期间探针仍报就绪,grpc-go 会空转重连) | 销毁 Acceptor:`~Acceptor` 先 `disableAll + remove`,监听 fd 随之关闭 |
| 改 muduo | 要改三份树,而第 3 批正在改它们 | 不改;只用公开的 `Acceptor.h`(三份树内容相同)、`TcpConnection`、`sockets::getLocalAddr / close` |
| 回调安全 | `closeCallback` 绑裸 this(上游标了 FIXME) | 一律绑 `weak_ptr<Server>` |
| 探针日志 | Linux 每次建连和断连各一行 INFO | 自己控制:握手前断开只打 DEBUG |
| 代价 | 几乎不用写新代码 | 约 60 行,逐行照抄 `TcpServer::newConnection / removeConnectionInLoop / ~TcpServer` 的时序,用 §15.14 的生命周期用例锁住 |

关于 AGENTS §11.7:`muduo_grpc::Server` 是 muduo 层的连接所有者(TcpServer 的等价物),是 `TcpConnectionPtr` 的唯一强持有者;应用层(ServerCall、生成代码、Node)只经 weak 触达。这一点已写入 `server.h` 头注释;经负责人授权,§11.7 的参考实现列表及生命周期测试引用已补齐,详见 §15.16 第 7 项。

#### 15.4.2 分层与文件

```
Go(grpc-go,h2c) ─TCP→ muduo::net::Acceptor ─(新连接,回调绑 weak<Server>)→ muduo_grpc::Server
  Server:连接表(唯一强持有 TcpConnectionPtr)/ 方法表 / 调用表 calls_(恰好一次的唯一裁决点)/
          派发泵 / 截止定时器 / 排空状态机 / 统计
  └ detail::ServerConnection(每连接,定义在 server.cpp;只存 weak<TcpConnection>、weak<Server>)
      └ ServerSession(纯协议的 nghttp2 server 会话:不碰 I/O,不碰定时器)
生成的 <Svc>Impl : muduo_grpc::Service  ── GrpcUnaryCall<Req,Resp>(core 里的 protobuf 胶水)
  grpc.<M>.guard 段(在 loop 上:同步拒绝,或发起异步许可后 Proceed)→ Handle<M>(同步,函数体不变)
agones::GameServerLifecycle::AcquireAllocationPermitAsync(worker 线程发 HTTP,结果 queueInLoop 回 loop)
```

| 文件 | 内容 |
|---|---|
| `muduo_grpc/address.{h,cpp}`(新) | `bool IsCanonicalIpv4(const std::string&)`。`channel.cpp:22` 里的私有副本等第 3 批落定后改调这里;本批不碰 channel.cpp |
| `muduo_grpc/method_table.{h,cpp}`(新) | `UnaryMethodHandler`、`SplitMethodPath`、`MethodTable`、`Service` |
| `muduo_grpc/server_call.h`(新) | `ServerCall`、`detail::CallToken`(实现放在 server.cpp) |
| `muduo_grpc/server_session.{h,cpp}`(新) | `ServerSession`;只有 .cpp include `nghttp2.h` |
| `muduo_grpc/server.{h,cpp}`(新) | `Server`、`ServerOptions`、`ServerShutdownOptions`、`ServerStatsSnapshot`、`FormatServerStats`;内部的 `detail::ServerConnection`、`CallRecord` |
| `muduo_grpc/message_framing.cpp`(改 1 行) | `body_.reserve(length)` 改为 `reserve(std::min<size_t>(length, 64 * 1024))`,防止对端只报长度不发数据造成预分配放大 |
| `core/network/grpc_unary_call.h`(新,只有头文件) | `GrpcUnaryCall<Req, Resp>` |
| `infra/agones/agones_gameserver_lifecycle.{h,cpp}`(改) | 新增 `AcquireAllocationPermitAsync` |

muduo_grpc 仍然不依赖 protobuf / grpc / core / entt。

#### 15.4.3 API 草稿

```cpp
// ===== method_table.h =====
namespace muduo_grpc {
class ServerCall;
// requestBytes 只在本次调用内有效。只在 loop 线程、Server 的派发泵里调用,绝不在 nghttp2 回调栈里;
// 调用前框架已确认调用仍有效、未到期。handler 必须让 call 恰好完成一次(同步 Finish*,或留着句柄稍后完成)。
using UnaryMethodHandler = std::function<void(ServerCall call, std::string_view requestBytes)>;
// "/pkg.Service/Method" -> {"pkg.Service","Method"};必须以 '/' 开头、恰好两段、两段都非空。
std::optional<std::pair<std::string_view, std::string_view>> SplitMethodPath(std::string_view path);
class MethodTable {
public:
    bool AddUnary(std::string path, UnaryMethodHandler handler);   // 非法或重复:LOG_ERROR,valid_ 置 false,返回 false
    const UnaryMethodHandler *Find(std::string_view path) const;   // Start 之后只读
    bool HasService(std::string_view fullServiceName) const;       // 用来区分两种 UNIMPLEMENTED 文案
    bool valid() const { return valid_; }
private:
    std::unordered_map<std::string, UnaryMethodHandler> methods_;
    std::unordered_set<std::string> services_;
    bool valid_{true};
};
class Service {
public:
    virtual ~Service() = default;
    virtual void RegisterMethods(MethodTable &table) = 0;
};
} // namespace muduo_grpc
```

```cpp
// ===== server_call.h =====
namespace muduo_grpc {
class Server;
namespace detail { struct CallToken; }
// 一次入站 unary 调用的完成句柄(形状照 RpcDoneCallback,见 muduo-rpc-channel.md §5)。
//  * 可拷贝:所有副本共享一个 CallToken。token 只持 weak_ptr<Server> + callId + loop 线程 id + 只读信息,
//    绝不持有 TcpConnectionPtr,也不持有 Server 的强引用(AGENTS §11.7)。
//  * 除只读访问外,只能在 loop 线程调用(断言)。
//  * 恰好一次:第一个完成源生效(handler、截止、对端 RST / 断连、排空到期、句柄全部丢弃、服务端析构);
//    之后的 Finish 一律吞掉,计入 late_finish。
//  * 最后一个副本析构时仍未完成:
//    - 在 loop 线程:回 UNAVAILABLE "server dropped the call without completing it"(业务可能已执行,
//      所以只能用「结果未知」码),计入 dropped,打限频 ERROR;
//    - 不在 loop 线程(与 loopThread 不符):只累加进程级原子计数 dropped_off_loop,绝不 lock Server。
class ServerCall {
public:
    ServerCall() = default;
    explicit operator bool() const;
    uint64_t id() const;
    const std::string &method() const;   // :path
    const std::string &peer() const;     // 只用于日志
    const Metadata &metadata() const;    // 只含自定义键;-bin 已解掉线上那一层
    std::optional<std::chrono::steady_clock::time_point> deadline() const;
    bool IsActive() const;               // 调用记录还在
    bool ServerDraining() const;         // 已 BeginShutdown,或服务端已销毁
    // 业务开始前调用(只给 GrpcUnaryCall::Proceed 用)。
    //   记录不在 → false;
    //   已到期(steady 复核)→ 以 DEADLINE_EXCEEDED 完成后返回 false;
    //   否则标记「业务已开始」,返回 true。
    // 不检查排空:排空期间已接受的调用照常处理。
    bool TryStartBusiness() const;
    void FinishOk(std::string responseBytes) const;   // 超过 maxSendMessageBytes → 改回 RESOURCE_EXHAUSTED
    void FinishError(Status status) const;            // status.ok() 视为编程错误 → 回 INTERNAL 并 LOG_ERROR
private:
    friend class Server;
    explicit ServerCall(std::shared_ptr<detail::CallToken> token);
    std::shared_ptr<detail::CallToken> token_;
};
namespace detail {
struct CallToken {   // 没有任何 T* 字段
    std::weak_ptr<Server> server;
    uint64_t callId{0};
    std::thread::id loopThread;
    std::string method, peer;
    Metadata metadata;
    std::optional<std::chrono::steady_clock::time_point> deadline;
    ~CallToken();    // 析构规则见上
};
} // namespace detail
} // namespace muduo_grpc
```

```cpp
// ===== server_session.h =====(纯协议;约定与 ClientSession 相同:单线程,Observer 回调里不得销毁本对象、不得调业务)
struct ServerSessionOptions {
    size_t maxReceiveMessageBytes{4 * 1024 * 1024};
    uint32_t maxHeaderListBytes{16 * 1024};
    uint32_t maxConcurrentStreams{256};
    uint32_t initialStreamWindowBytes{1024 * 1024};
    uint32_t connectionWindowBytes{16 * 1024 * 1024};
    size_t maxBufferedRequestBytes{16 * 1024 * 1024};   // 本连接所有未收完请求体之和
};
struct RequestHeaders {
    std::string path;                                 // :path 原值
    Metadata metadata;                                // 只含自定义键,-bin 已解一层
    std::optional<std::chrono::nanoseconds> timeout;  // 没有 grpc-timeout = 无截止
};
class ServerSession {
public:
    struct Verdict {
        enum class Kind : uint8_t { kAccept, kReject, kRefuse };
        Kind kind{Kind::kAccept};
        Status status;   // kReject:回 :status 200 + 该状态的 trailers-only;kRefuse:回 RST_STREAM(REFUSED_STREAM)
    };
    class Observer {     // 全部在 Receive / DrainOutput 的 nghttp2 回调栈里同步触发;实现里只查表、登记、入队
    public:
        virtual ~Observer() = default;
        virtual void OnClientSettings() = 0;                                                  // 收到首个非 ACK SETTINGS
        virtual Verdict OnRequestHeaders(int32_t streamId, const RequestHeaders &headers) = 0; // 路由 / 排空到期判定
        virtual Verdict OnRequestComplete(int32_t streamId, RequestHeaders headers, std::string message) = 0; // 在途上限检查 / 登记
        virtual void OnStreamAborted(int32_t streamId, uint32_t http2ErrorCode) = 0;          // 已登记、本端尚未回完时流被关闭
        virtual void OnShutdownPingAck() = 0;
    };
    ServerSession(ServerSessionOptions options, Observer &observer);
    ~ServerSession();
    Status Start();                              // 建会话、设 nghttp2 选项、提交 SETTINGS 与连接窗口;字节要等 DrainOutput 才写出
    Status Receive(const uint8_t *data, size_t len);   // 非 OK = 连接级错误(坏 magic / 协议错误 / 洪泛)
    Status DrainOutput(const std::function<bool(std::string_view chunk)> &write);
    bool SubmitResponse(int32_t streamId, std::string message);   // HEADERS(200) + DATA + trailers(grpc-status 0)
    bool SubmitStatus(int32_t streamId, const Status &status);    // 非 OK 的 trailers-only
    bool RefuseStream(int32_t streamId);                          // RST_STREAM(REFUSED_STREAM)
    void SubmitShutdownNotice();                                  // GOAWAY(2^31-1, NO_ERROR) + PING
    void SubmitFinalGoaway();                                     // GOAWAY(nghttp2_session_get_last_proc_stream_id, NO_ERROR)
    bool receivedAnyBytes() const;
    bool handshakeComplete() const;
    bool finalGoawaySubmitted() const;
    size_t OpenStreamCount() const;
    bool ShouldClose() const;                                     // 既不想读也不想写
};
```

```cpp
// ===== server.h =====
struct ServerOptions {
    std::string name{"grpc"};
    ServerSessionOptions session;
    size_t maxSendMessageBytes{4 * 1024 * 1024};
    size_t maxPendingOutputBytes{1024 * 1024};          // 输出缓冲超过就暂停从 nghttp2 取帧,writeComplete 后继续
    size_t maxInFlightCalls{1024};
    size_t maxDispatchPerTurn{64};
    std::chrono::microseconds dispatchTurnBudget{2000};
    std::chrono::milliseconds handshakeTimeout{120000};
    std::chrono::seconds statsLogInterval{30};          // 0 = 不打汇总(单测用)
};
struct ServerShutdownOptions {
    std::chrono::milliseconds drainTimeout{2000};
    std::chrono::milliseconds goawayPingTimeout{1000};
    std::chrono::milliseconds closeGrace{1000};
};
struct ServerStatsSnapshot { /* 纯值类型,字段见 §15.11 */ };
std::string FormatServerStats(const ServerStatsSnapshot &stats);   // 字段顺序由单测锁住

// 线程:所有 API 只在构造时给定的 loop 线程调用(断言);不创建线程,不加锁。
// 所有权:muduo 层的连接所有者(TcpServer 的等价物),连接表里按值持有 TcpConnectionPtr;
//   Acceptor / 连接 / 定时器 / queueInLoop 的回调一律绑 weak_ptr<Server>。
// 析构前置:在 loop 线程上,且不在 I/O 事件处理期间 —— 只在 queueInLoop 的任务里或 loop 外析构。
//   原因:Acceptor 的 channel 在 eventHandling_ 期间 remove 会撞上 EventLoop.cc:212-216 的断言。
class Server : public std::enable_shared_from_this<Server> {
    struct PrivateTag { explicit PrivateTag() = default; };
public:
    // ipv4 不是规范 IPv4 字面量,或 port == 0:LOG_ERROR 后返回 nullptr。
    static std::shared_ptr<Server> Create(muduo::net::EventLoop &loop, const std::string &ipv4, uint16_t port,
                                          ServerOptions options = {});
    Server(PrivateTag, muduo::net::EventLoop &loop, const muduo::net::InetAddress &listenAddress, ServerOptions options);
    ~Server();   // 取消所有定时器;剩余调用静默收口;剩余连接逐条 forceClose(排进的两趟由两跳 quit 覆盖);不调 onDrained
    Server(const Server &) = delete;
    Server &operator=(const Server &) = delete;
    bool RegisterService(Service &service);   // 只能在 Start 之前;路径非法或重复 → false
    void Start();                              // 建 Acceptor(bind 失败按 muduo LOG_SYSFATAL)并 listen;只调一次
    // 幂等。onDrained 恰好调用一次,一律经 queueInLoop 投递,永不在本调用栈内执行。可以从 handler 内调用。
    void BeginShutdown(ServerShutdownOptions options, std::function<void()> onDrained);
    bool draining() const;
    size_t inFlightCalls() const;
    size_t connectionCount() const;
    ServerStatsSnapshot Stats() const;
    const std::string &listenAddress() const;
private:
    friend class ServerCall;
    friend struct detail::CallToken;
    // 内部状态:loop_(引用);methods_(vector<UnaryMethodHandler> + path→下标);
    //   acceptor_(shared_ptr<Acceptor>,便于 move 进 queueInLoop 闭包销毁);
    //   connections_(id → {TcpConnectionPtr, shared_ptr<detail::ServerConnection>});
    //   calls_(callId → CallRecord);ready_(deque<callId>);deadlines_(set<{tp, callId}>);
    //   deadline / drain / stats 三个 TimerId;phase_;onDrained_;统计
};
```

```cpp
// ===== core/network/grpc_unary_call.h =====(只有头文件;生成代码使用;只在 loop 线程)
template <typename Request, typename Response>
class GrpcUnaryCall final {
    struct PrivateTag { explicit PrivateTag() = default; };
public:
    using Handler = std::function<void(const Request *request, Response *response)>;   // 不用函数指针字段(§15.2 #13)
    static std::shared_ptr<GrpcUnaryCall> Create(muduo_grpc::ServerCall call, Handler handler);
    GrpcUnaryCall(PrivateTag, muduo_grpc::ServerCall call, Handler handler);
    bool ParseRequest(std::string_view bytes);   // 失败时已回 INTERNAL "grpc: error unmarshalling request"
    const Request &request() const { return request_; }
    const muduo_grpc::ServerCall &server_call() const { return call_; }
    bool IsActive() const { return call_.IsActive(); }
    bool ServerDraining() const { return call_.ServerDraining(); }
    void Reject(muduo_grpc::Status status);      // 不进 Handle*;status 必须非 OK
    // TryStartBusiness 为真时:handler_(&request_, &response_) → SerializeToString → FinishOk
    //   (序列化失败回 INTERNAL "grpc: error while marshaling")。
    // 调用已被截止 / 取消 / 排空收口时什么都不做。重复调用没有副作用。
    void Proceed();
private:
    muduo_grpc::ServerCall call_;
    Handler handler_;
    Request request_;
    Response response_;
};
```

#### 15.4.4 所有权、线程与 §11.7 对照

| 对象 | 谁强持有 | 绑进回调的是 | 何时销毁 |
|---|---|---|---|
| `Acceptor` | `Server::acceptor_`(shared_ptr) | 新连接回调绑 `weak_ptr<Server>`;lock 失败或已在排空时 `sockets::close(fd)` | BeginShutdown 里移进 `queueInLoop` 闭包销毁 |
| `TcpConnection` | **只有** `Server::connections_`,以及 muduo 自己排进队列的闭包 | `connectionCallback` 必须设(`TcpConnection.cc:331` 无条件调用,空的 std::function 会抛异常),设成空 lambda;message / writeComplete 回调绑 `weak<ServerConnection>`,lock 失败就 `retrieveAll`;close 回调绑 `weak<Server>`:Server 还在就摘表,然后**无条件** `queueInLoop([conn]{ conn->connectDestroyed(); })` | 摘表后 ServerConnection 也移进同一个闭包一起释放;`~Server` 对剩余连接一律 `forceClose`,由 close 回调补 `connectDestroyed` |
| `detail::ServerConnection` | `Server::connections_` | 握手 / GOAWAY-PING / 关闭宽限定时器绑 `weak<ServerConnection>`;只存 `weak<TcpConnection>`、`weak<Server>` | 同上,不在它自己的回调栈上析构 |
| `CallRecord` | `Server::calls_`(按值) | — | 完成即删除,这是恰好一次的唯一裁决点 |
| `CallToken` / `ServerCall` | handler 侧的各个副本 | 不进 muduo 回调 | 最后一个副本析构,规则见 §15.4.3 |
| `GrpcUnaryCall` | 生成代码与守护段的异步回调(shared_ptr) | 只经 ServerCall 弱引用服务端 | 回调结束即释放 |
| 生成的 `<Svc>Impl` | scene `SceneRuntimeContext`(值成员)/ battle `unique_ptr`;**所有权不变** | 方法表闭包捕获 `this`:由 Server 同步调用,不是 muduo 回调;Server 在 Node 析构前已销毁,而 Impl 活过 Node(注册契约同今天)。**异步续跑不得捕获 this** | — |
| `Server` | `Node::grpcServer_`(shared_ptr) | 所有定时器 / 闭包只绑 weak | `Node::OnGrpcServerDrained`(排队任务)里 reset,早于两跳 quit |

### 15.5 线上契约(服务端方向)

#### 15.5.1 连接、握手与探针

- 只支持 h2c prior-knowledge。nghttp2 server 会话校验 24 字节 magic,坏 magic 时 `Receive` 返回错误:尽力冲出后 `forceClose`,计入 `protocol_errors`,打限频 WARN。
- **本端 SETTINGS 推迟到收到对端首字节才写出**。`Start()` 只把它提交进 nghttp2;收到首字节之前,`ServerConnection::FlushOutput` 直接返回。
  - RFC 9113 §3.4 只要求 SETTINGS 是服务端发出的第一帧;grpc-go 客户端连上就先发 preface,所以不增加往返。
  - 效果:kubelet tcpSocket 探针连上就关,本端一个字节都不写,不会出现 EPIPE / RST 引发的 `handleError` ERROR。断开时只打 DEBUG,计入 `closed_before_preface`。
- 握手超时 120s:accept 时上弦,收到首个非 ACK SETTINGS 时作废;到期 `forceClose`,打 DEBUG,计入 `handshake_timeout`。
- 不做 HTTP/2 层 keepalive,死对端由 SO_KEEPALIVE 发现。连接数不设上限:未握手的连接有握手超时兜底,已握手的空闲连接与 grpc 默认一致,不限。

#### 15.5.2 SETTINGS 与 nghttp2 选项(`ServerSession::Start`)

- 本端 SETTINGS:`MAX_CONCURRENT_STREAMS=256`、`INITIAL_WINDOW_SIZE=1MiB`、`MAX_HEADER_LIST_SIZE=16KiB`;连接级接收窗口用 `nghttp2_session_set_local_window_size` 放大到 16MiB。不发 `ENABLE_PUSH`(服务端只能省略或发 0)。
- 显式写出的选项(取值等于 1.70 的默认值,目的是锁住将来的漂移):

| 选项 | 值 |
|---|---|
| `max_outbound_ack` | 1000 |
| `max_settings` | 32 |
| `stream_reset_rate_limit` | 1000 / 33 每秒(rapid-reset) |
| `max_continuations` | 8 |
| `glitch_rate_limit` | 10000 / 330 每秒 |
| `no_rfc9113_leading_and_trailing_ws_validation` | 1(与 ClientSession 一致,不新增对 grpc-go metadata 的拒绝) |

- 自动 ACK PING / SETTINGS、自动 WINDOW_UPDATE、HTTP 语义校验:全部保持默认开启。

#### 15.5.3 请求校验(按顺序;全部在业务之前)

请求没收完时,回完紧跟一个 `RST_STREAM(NO_ERROR)`。依据 RFC 9113 §8.1、grpc-go `writeEarlyAbort(rst=!eos)`;nghttp2 不会自动补,由本层在 `on_frame_send` 里看到 END_STREAM 且对端未关闭时补发。

| 情况 | 回应 | 依据 |
|---|---|---|
| 伪头缺失 / 连接级头 / `te`≠trailers / 头名大写 | nghttp2 自动发 `RST_STREAM(PROTOCOL_ERROR)` | RFC 9113 §8.2 |
| 头部块 >16KiB(名+值+32 累计) | 200 + `RESOURCE_EXHAUSTED "received header list larger than max (16384)"`;超限后的头不再存储 | 与 §4.7 客户端同口径 |
| `:method`≠POST | 405 + INTERNAL `method must be POST` | grpc-go `http2_server.go:554` |
| content-type 不是 `application/grpc` 开头,或其后不是结束 / `+` / `;` | 415 + INVALID_ARGUMENT `invalid gRPC request content-type "<v>"` | grpc-go `:496` |
| `grpc-timeout` 非法(`ParseGrpcTimeout` 返回 nullopt) | 400 + INTERNAL `malformed grpc-timeout` | grpc-go `:482/:500` |
| `-bin` 值 base64 非法 | 400 + INTERNAL `malformed binary metadata "<key>"` | grpc-go `:464` |
| `grpc-encoding` 存在且不是 `identity` | 200 + UNIMPLEMENTED `unsupported grpc-encoding "<x>"`,并附 `grpc-accept-encoding: identity` | gRPC compression 规范 |
| `:path` 拆不成两段 | 200 + UNIMPLEMENTED `malformed method name: "<path>"` | grpc-go 同文案 |
| 服务未知 / 方法未知 | 200 + UNIMPLEMENTED `unknown service <svc>` / `unknown method <m> for service <svc>` | grpc-go 同文案 |
| 压缩标志为 1 或非法 | 200 + INTERNAL(沿用 MessageDeframer 的文案) | — |
| 声明长度 >4MiB | 200 + `RESOURCE_EXHAUSTED "received message larger than max (N vs. 4194304)"`,立即回 | — |
| 本连接未收完请求体之和 >16MiB | 200 + `RESOURCE_EXHAUSTED "request buffer limit exceeded"` | 防止 256 × 4MiB 的内存放大 |
| END_STREAM 时消息数≠1,或有残留的半条 | 200 + INTERNAL `unary request must contain exactly one message` | — |
| timeout ≤ 0 | 200 + `DEADLINE_EXCEEDED "deadline exceeded before dispatch"` | — |
| 全节点在途 ≥1024 | 200 + `RESOURCE_EXHAUSTED "server in-flight call limit (1024) reached"` | Q2 |
| 排空到期之后才开或才收完的流 | `RST_STREAM(REFUSED_STREAM)` | Q4 |

- 不校验 `:scheme`、`:authority`、`user-agent`;未知的 `grpc-*` 头一律忽略,不进 metadata。
- 派发之后可能出现的状态码:
  - protobuf 解析失败:INTERNAL `grpc: error unmarshalling request`;
  - 应答序列化失败:INTERNAL;
  - 应答 >4MiB:RESOURCE_EXHAUSTED `grpc: trying to send message larger than max`;
  - 句柄全部丢弃:UNAVAILABLE。
  - 后三种发生在业务之后,风险见 §15.13。

#### 15.5.4 路由、metadata、grpc-timeout

- **路由**:按 `:path` 精确匹配。scene 是 `/scene_node.SceneNodeGrpc/<M>`,battle 是 `/BattleNode/<M>`(无 package)。不提供反射和健康检查服务。
- **metadata**:自定义键收进 `RequestHeaders.metadata`。`-bin` 键用 `DecodeBinaryHeaderValue` 解掉线上那一层,有无 padding、逗号分隔的多值都接受。不收的头:伪头,以及 `te / content-type / user-agent / grpc-timeout / grpc-encoding / grpc-accept-encoding`,其余 `grpc-` 前缀的头也不收。
- **截止**:截止时间 = 头部收完时的 `steady_clock::now()` 加上 timeout,做饱和加法:timeout 大于 `time_point::max() - now` 就视为无截止。原因是 `99999999H` 会被钳到 `nanoseconds::max()`,直接相加会溢出。没有这个头就是无截止(scene_manager 的 DestroyScene 循环就不带)。

#### 15.5.5 应答

- **成功**:`nghttp2_submit_response2(:status 200, content-type: application/grpc)`。数据提供者产出「5 字节前缀 + 消息」,最后一块置 `EOF | NO_END_STREAM`,并在读回调里 `nghttp2_submit_trailer(grpc-status: 0)`。这是 nghttp2 规定的 trailers 写法,保证 trailers 不会插到 DATA 前面。成功时不发 `grpc-message`。
- **非 OK 一律 trailers-only**:单个 HEADERS 带 END_STREAM,含 `:status`、`content-type`、`grpc-status`,以及非空时的 `grpc-message`(用 `PercentEncodeGrpcMessage` 编码,逐字节透传,不加前缀)。
- **输出背压**:连接输出缓冲达到 1MiB 时暂停从 nghttp2 取帧,`writeComplete` 后继续。照抄 `Http2Transport::FlushOutput` 的 `sessionDepth_ / flushDeferred_`。派发泵里产生的应答在本轮泵结束时统一冲出。

#### 15.5.6 RST 与两段 GOAWAY

- **对端 RST**:请求还没交出的,直接丢弃;已登记的,触发 `OnStreamAborted`,调用被撤销,计数。业务已开始的,之后的 Finish 被丢弃。
- **GOAWAY 第一段**:`nghttp2_submit_shutdown_notice`(必须先于任何 GOAWAY 调用),外加一个 PING。新流照常接收和处理。
- **GOAWAY 第二段**:收到 PING ACK 或满 1s,`nghttp2_submit_goaway(last_proc_stream_id, NO_ERROR)`。之后的新流被 nghttp2 忽略,grpc-go 把它们标为未处理,透明重试到新连接,被拒后得到 UNAVAILABLE。grpc-go 自己是 5s 计时器,本端取 1s,以落在 2s 的排空预算之内。
- **没收到过字节的连接**:排空开始时直接 `forceClose`。
- **协议错误**:nghttp2 发 GOAWAY,本端尽力冲出后 `forceClose`,打限频 WARN。

#### 15.5.7 PING

对端的 PING(含 grpc-go 的 BDP ping)全部由 nghttp2 自动 ACK。本端只在 GOAWAY 第一段时发一次 PING。**不做** too_many_pings 强制:Go 调用方都没配 keepalive,洪泛由 `max_outbound_ack` 和 glitch 限额兜住。

#### 15.5.8 状态码与 Go 语义

| 场景 | 线上 | 业务是否执行 | gather(CreateBattle) | spectate(AddObserver) |
|---|---|---|---|---|
| 准入 / 许可拒绝 | `UNAVAILABLE "battle_not_allocatable"` | 否 | 跳过 DestroyBattle,换节点 | — |
| 协议 / 路由 / 解析错误、过载 | INTERNAL / UNIMPLEMENTED / RESOURCE_EXHAUSTED | 否 | 发 DestroyBattle(幂等) | 判为没挂上,正确 |
| 截止(投递前 / 等许可) | DEADLINE_EXCEEDED | 否 | 发 DestroyBattle | 结果未知,补发 RemoveObserver |
| 排空到期(未开始) | REFUSED → 透明重试 → UNAVAILABLE | 否 | 发 DestroyBattle | 结果未知 |
| 排空到期(已开始)、句柄丢弃 | UNAVAILABLE | **可能已执行** | 发 DestroyBattle ✓ | 结果未知 ✓ |

不变量(写进 server.h 注释,并由测试锁住):

- 服务端自己产生的 UNAVAILABLE,文案永远不等于 `battle_not_allocatable`。
- 「可能已执行」的失败只能用 DEADLINE_EXCEEDED / UNAVAILABLE / CANCELLED。

### 15.6 并发与背压

- **数据流**:读事件 → `ServerConnection::HandleMessage` → `session.Receive`。muduo 一次 readv;nghttp2 同步解完,这一步只做分帧和 HPACK。
  - `OnRequestComplete` 检查在途上限 → 登记 `CallRecord`、登记截止 → `ready_.push_back` → `ScheduleDispatch()`。泵任务经 `queueInLoop` 排队,同一时刻至多一个。
- **派发泵**:每轮最多 64 个，在调用之间检查 2ms 预算，有待处理项时至少处理 1 个（已截止项可能只被处理，不调用 handler）；同步 handler 不可抢占，单个调用可能超过预算，不能保证整轮累计 ≤2ms。剩余的再排一次:泵内再排时 `callingPendingFunctors_` 为真,会触发唤醒,落到下一轮,中间让出给定时器和 I/O。每个调用的处理:
  1. 记录已不在(被取消)就跳过;
  2. 已到期就回 DEADLINE_EXCEEDED;
  3. 否则建 token,**先把 message 移出记录**(handler 可能同步完成并删掉记录),再调 handler。
- **顺序**:全节点一个先进先出队列,按请求收完的先后派发。今天 8 个 poller 并发,没有顺序保证;没有调用方依赖那种乱序。这只是改进,不写成契约。
- **输入背压**由三层上限构成:每连接 256 条流(HTTP/2 层强制,客户端排队)、每连接未收完请求体 16MiB、全节点在途 1024。不另做 stopRead。
- **Go 侧**:`reconcile.go` 的 `Workers=8` 不变,Y-06 golden 不变。只改 `reconcile.go:259-271` 和 `:662`、`config_test.go:23` 的注释。新理由:每条资产 RPC 都在 scene 的 EventLoop 上同步执行(含 durable 探测和 Redis 写),后台补偿不能挤占逻辑帧;副本数 × Workers 才是 scene 看到的补偿并发,而且远小于服务端的 256 / 1024。

### 15.7 截止、取消与恰好一次

| 时刻 | 截止到期 | 对端 RST / 断连 | 排空到期 | `lifecycle.Stop()` |
|---|---|---|---|---|
| 排队中(kQueued) | DEADLINE_EXCEEDED,不派发 | 静默撤销,计 `cancelled_before_start` | REFUSED_STREAM | — |
| 已派发、等许可 | DEADLINE_EXCEEDED,完成调用;许可回来后 `Proceed` 里的 `TryStartBusiness` 为假,不进业务,permit 随回调析构,零单元时收口 | 撤销,不回应答;许可回来后同左 | REFUSED_STREAM;许可回来后同左 | 回调收到拒绝,由守护段 `Reject(...)` |
| 业务已开始 | 同步的 Handle* 不可能处在这一状态。延后的:立即回 DEADLINE_EXCEEDED,业务继续,计 `deadline_after_start` | 不回应答,之后的 Finish 计 `late_finish` | UNAVAILABLE `server shutting down` | 无影响 |

恰好一次靠以下几条保证:

1. 所有完成源都只在 loop 线程,汇到 `Server::CompleteCall(callId, outcome)`。完成源包括:handler、截止定时器、`OnStreamAborted`、连接移除、排空定时器、token 析构(在 loop 线程时)、`~Server`(静默)。
2. `CompleteCall` 的第一步是在 `calls_` 里找记录,找到即 erase,找不到就计 late 后返回。
3. 释放顺序:先把记录移出并从表中删除,再递减在途计数、提交应答、标记连接需要冲出;最后才释放可能进入其他锁的对象。
4. 业务只在 `TryStartBusiness()` 为真之后才开始;判定「有效且未到期」与标记「已开始」在同一次 loop 调用里完成。截止和取消只决定回什么,永不打断业务。
5. 连接移除时,遍历该连接的 `streamId → callId` 表,逐个撤销。所以连接表为空就意味着调用表为空。

### 15.8 生成代码形状与守护段迁移

#### 15.8.1 新的头文件(以 battle 为例)

```cpp
#pragma once

#include <memory>
#include <string_view>
#include <muduo/net/EventLoop.h>
#include "muduo_grpc/method_table.h"
#include "network/grpc_unary_call.h"
#include "proto/battle/battle_node.pb.h"
///<<< BEGIN WRITING YOUR CODE: grpc.header.includes
///<<< END WRITING YOUR CODE

// 由 proto 工具生成 BattleNode 的 gRPC 服务端骨架(muduo_grpc,docs/design/grpc-muduo-transport.md §15);自定义代码只写在守护段内。
// 全部在节点 EventLoop 线程执行,不创建线程。每个 RPC:解析请求 → <M>()(守护段 grpc.<M>.guard)→ Handle<M>(同步)→ 回包。
// 守护段:同步拒绝写 `call->Reject(status); return;`;异步决定时先发起异步操作再 `return;`,在回调里调
// `call->Proceed()` 或 `call->Reject(...)`。回调只捕获 call 与 weak 引用,不得捕获 this(AGENTS §11.7)。
// 要活到业务完成的对象(如 Agones 许可)作为回调参数,活到 Proceed() 返回即可(Handle* 同步)。不得阻塞;业务错误经响应字段返回。
class BattleNodeImpl final : public muduo_grpc::Service
{
public:
    explicit BattleNodeImpl(muduo::net::EventLoop& loop);
    ///<<< BEGIN WRITING YOUR CODE: grpc.header.public
    ///<<< END WRITING YOUR CODE

    // 方法表(按 :path 精确路由)。处理函数捕获 this:本对象必须活过注册它的 Node。
    void RegisterMethods(muduo_grpc::MethodTable& table) override;

    static constexpr std::string_view kQueryBattleCreationPath = "/BattleNode/QueryBattleCreation";
    static constexpr std::string_view kCreateBattlePath = "/BattleNode/CreateBattle";
    // ……每个方法一个

private:
    using QueryBattleCreationCall = GrpcUnaryCall<::QueryBattleCreationRequest, ::QueryBattleCreationResponse>;
    using CreateBattleCall = GrpcUnaryCall<::CreateBattleRequest, ::CreateBattleResponse>;
    using DestroyBattleCall = GrpcUnaryCall<::DestroyBattleRequest, ::Empty>;
    // ……
    void QueryBattleCreation(const std::shared_ptr<QueryBattleCreationCall>& call);
    void CreateBattle(const std::shared_ptr<CreateBattleCall>& call);
    // ……
    static void HandleCreateBattle(const ::CreateBattleRequest* request, ::CreateBattleResponse* response); // 签名不变
    static void HandleDestroyBattle(const ::DestroyBattleRequest* request);                                  // 签名不变
    // ……
    muduo::net::EventLoop& loop_;
    ///<<< BEGIN WRITING YOUR CODE: grpc.header.private
    ///<<< END WRITING YOUR CODE
};
```

#### 15.8.2 新的 cpp

文件中的顺序依次是:FirstCode → 构造函数 → `Handle*`(位置、格式、查找键都不变)→ 每个方法的 `<M>()` → `RegisterMethods`。

```cpp
#include "battle_node.h"
///<<< BEGIN WRITING YOUR CODE
// ……FirstCode……
///<<< END WRITING YOUR CODE
// ……构造函数、Handle* 原样……

void BattleNodeImpl::CreateBattle(const std::shared_ptr<CreateBattleCall>& call)
{
    [[maybe_unused]] const ::CreateBattleRequest* request = &call->request();
    ///<<< BEGIN WRITING YOUR CODE: grpc.CreateBattle.guard
    ///<<< END WRITING YOUR CODE
    call->Proceed();
}

void BattleNodeImpl::RegisterMethods(muduo_grpc::MethodTable& table)
{
    table.AddUnary(std::string(kCreateBattlePath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = CreateBattleCall::Create(std::move(serverCall), &HandleCreateBattle);
            if (call->ParseRequest(requestBytes))
            {
                CreateBattle(call);
            }
        });
    table.AddUnary(std::string(kDestroyBattlePath),
        [this](muduo_grpc::ServerCall serverCall, std::string_view requestBytes)
        {
            const auto call = DestroyBattleCall::Create(std::move(serverCall),
                [](const ::DestroyBattleRequest* request, ::Empty* /*response*/) { HandleDestroyBattle(request); });
            if (call->ParseRequest(requestBytes))
            {
                DestroyBattle(call);
            }
        });
    // ……
}
```

- **调用 Handle 不带类名前缀**:`Handle*` 体的解析键就是 `<Svc>Impl::Handle<M>(`,这个字面量在生成正文里只能出现在定义行,所以 `RegisterMethods` 里写 `&HandleCreateBattle` / `HandleDestroyBattle(request)`。
- **守护段末尾的 `return;`**:会让段后的 `call->Proceed()` 变成不可达代码。对应的 C4702 是 4 级警告,scene / battle 都是 Level3,GCC 的 `-Wall -Wextra` 也不报。
- **生成物里不得出现**:`grpc::`、`grpcpp`、`<future>`、`promise`、`.grpc.pb.h`、`runInLoop`。

#### 15.8.3 生成器改动(只动 `internal/` 下的模板、`grpc_handler_gen.go`、`grpc_code_sections.go` 和三个测试;`model.go` 有他人改动,一行不动)

- **数据结构**:`GrpcHandlerMethod` 删掉 `PreDispatchCode / InLoopCode / PostDispatchCode`,新增 `GuardCode`、`Path`。`GrpcHandlerHeadData` 把 `GrpcInclude` 换成 `PbInclude`。
- **本文件内的新 helper**:
  - `grpcServerMethodPath(m)`:有 package 时为 `"/"+pkg+"."+svc+"/"+method`,否则 `"/"+svc+"/"+method`,与 `grpc_async_client.h.tmpl` 的 `k<Svc><Method>GrpcPath` 同一规则;
  - `grpcHandlerPbInclude(m)`:与 `MethodInfo.IncludeName()` 的 cc_generic 分支同式,产出 `.pb.h`;
  - `isEmptyGrpcCodeSection(code)`;
  - `checkLegacyGrpcSections(cppPath, methods) error`。
- `grpcHandlerIncludeName` 去掉 `<future>`。
- 服务里有任何流式方法:头文件返回 error,cpp panic。服务端只支持 unary,fail-closed。
- **允许的命名段**:头文件仍是三个;cpp 每个方法 `grpc.<M>.guard`。`grpc.<M>.in_loop`、`grpc.<M>.post_dispatch` 只在**为空时**接受,然后丢弃。其余未识别的段照旧拒绝。
- **旧段检查**(`checkLegacyGrpcSections`):
  - **2026-10-17 改为全覆盖**:不再依赖当前 proto 的方法表与 `legacyGrpcWrapperKey`。扫描旧 .cpp 里所有旧式包装函数(`<Svc>Impl::<M>(` 后允许任意空白 / 换行,紧跟 `grpc::ServerContext`)与所有旧命名段;已从 proto 删掉的方法的非空 Handle* 体、函数外的非空无名段同样拒绝;报错按段计数,格式「文件:方法:段名」。共享 `codeParser` 修了查找键劫持的根因(守护段内部不做查找键匹配,段外只认顶格签名行,孤立的开始标记回放不吞代码);`.h` 里的非空无名段也 fail-closed。下面几条是首版描述,保留作历史。
  - 旧的无名投递前段,用 `legacyGrpcWrapperKey(m) = Svc+"Impl::"+M+"(grpc::ServerContext"` 读取(即原来的 `GenerateGrpcWrapperNameWrapper` 改名,只用于检测)。读到的段如果以命名标记开头,说明串到了下一个段,按「不存在」处理。
  - 「空」的判定:去掉标记行后不剩任何非空白字符。注释也算非空。
  - 只要有非空的旧段,就返回 error,列出 `文件:方法:旧段名`,并提示「按 grpc-muduo-transport.md §15.8.4 手工迁到 grpc.<M>.guard」。**宁可拒绝生成,也不吞代码**。
- **调用时机**:`GetGrpcServiceHandlerHeadFromFileStr` 在渲染头文件之前,先对同名 .cpp(把 `GrpcHandlerH` 后缀换成 `GrpcHandlerCpp`)跑这项检查,出错就返回 error。`handler.go` 据此 Fatal,两份文件都不写。`GetGrpcServiceHandlerCppStr` 里同样检查,出错 panic。
- `handlers.go`、`grpc_handler_target.go`、`proto_gen*.yaml` 都不改。

#### 15.8.4 手写段迁移(以工作区为准,一行不丢)

| 旧位置 | 去向 |
|---|---|
| scene FirstCode(`scene_node_service.cpp:3-12`) | 原样保留,末尾加 `#include <memory>` 和 `namespace { constexpr char kSceneNodeShuttingDownMessage[] = "scene_node_shutting_down"; }` |
| scene CreateScene 无名投递前段(`:269-289`) | 迁到 `grpc.CreateScene.guard`(代码见下),文案逐字不变,保留 6c4021ae5 的恢复说明 |
| battle FirstCode(`battle_node.cpp:3-37`) | 原有注释逐行保留。`NotAllocatable()` 改为返回 `muduo_grpc::Status(muduo_grpc::StatusCode::kUnavailable, kBattleNotAllocatableMessage)`;`Open/CloseAdmission` 改用 `admission_->`;新增 `bool AdmissionOpen(const std::weak_ptr<battle_admission::AdmissionGate>&)`(lock 失败或 Phase≠kOpen 返回 false) |
| battle `grpc.header.private`(`battle_node.h:67-69`) | 改为 `std::shared_ptr<battle_admission::AdmissionGate> admission_ = std::make_shared<battle_admission::AdmissionGate>();`,注释写明「许可回调只弱持有它」 |
| battle CreateBattle 无名段(`:126-167`) + `in_loop`(`:175-184`) + `post_dispatch`(`:192-198`) | 合并进 `grpc.CreateBattle.guard`:第 1 道闸 → 异步许可 → 回调里复核闸(原 in_loop),拒绝分支的日志用原 post_dispatch 那一行。原来的 `closedInLoop` 和 `auto &admission` 不再需要 |
| battle `grpc.DestroyBattle.in_loop`(`:215-218`) | 迁到 `grpc.DestroyBattle.guard`,执行时机仍是在 loop 内、`Handle*` 之前 |
| 15 个 `Handle*` 体 | **一字不改**。其中过时的注释(scene `:212`、`:231` 的「外层已 runInLoop」)在单独一步里改 |

scene 的 `grpc.CreateScene.guard`:

```cpp
    // 停机已开始(Node 在 beforeShutdown 之后进入 gRPC 排空):不再建新场景 —— 建出的场景会随进程退出丢失,
    // SceneManager 却以为它在。回 UNAVAILABLE 让 SceneManager 回滚(它对任何错误都回滚,不匹配文案)。
    if (call->ServerDraining())
    {
        LOG_WARN << "[gRPC] CreateScene rejected: scene node is shutting down, scene_id=" << request->scene_id();
        call->Reject(muduo_grpc::Status(muduo_grpc::StatusCode::kUnavailable, kSceneNodeShuttingDownMessage));
        return;
    }
    // Agones 高密度模式:进程必须先被 Allocate,才能创建第一个场景。
    // POST /allocate 在 lifecycle worker 线程,结果经 queueInLoop 回 loop,不阻塞逻辑帧;等待上限
    // LifecycleOptions::allocateWaitTimeout。失败一律 fail-closed:不建实体,回非 OK,让 SceneManager 回滚重试,
    // 而不是留下一个 Agones 不知道的房间。createPermit 是回调参数,活到 Proceed() 返回(Handle* 同步建完场景),
    // 在途创建才能阻止提前回到 Ready。回调只捕获 call,不捕获 this(AGENTS §11.7)。
    //
    // 2026-09-15 恢复:原块在 commit 6c4021ae5 被 proto 重生成吞掉(当时位于守护段外)。
    agones::GameServerLifecycle::Instance().AcquireAllocationPermitAsync(
        loop_,
        [call](agones::GameServerLifecycle::AllocationPermit createPermit)
        {
            if (!createPermit)
            {
                LOG_ERROR << "[gRPC] CreateScene rejected: Agones allocate not confirmed, scene_id="
                          << call->request().scene_id() << " state="
                          << agones::ToString(agones::GameServerLifecycle::Instance().State());
                call->Reject(muduo_grpc::Status(muduo_grpc::StatusCode::kUnavailable, "agones allocate not confirmed"));
                return;
            }
            if (call->ServerDraining())   // 等许可期间停机开始
            {
                LOG_WARN << "[gRPC] CreateScene rejected after permit: scene node is shutting down, scene_id="
                         << call->request().scene_id();
                call->Reject(muduo_grpc::Status(muduo_grpc::StatusCode::kUnavailable, kSceneNodeShuttingDownMessage));
                return;
            }
            call->Proceed();
        });
    return;
```

battle 的 `grpc.CreateBattle.guard`:

```cpp
    // 节点级准入分三道,全部在任何副作用之前;任一道不过即回 UNAVAILABLE battle_not_allocatable,
    // match 据此跳过 DestroyBattle、换节点重试一次。
    // 顺序:准入闸 → 分配许可(异步:HTTP 在 lifecycle worker,结果经 queueInLoop 回 loop)→ 许可回到 loop 后复核准入闸
    // → 预签票据(HandleCreateBattle 内 BuildAssignment)→ 插表(EmplaceRoom)。全部在 loop 线程。
    // 1) 建房准入闸(battle_admission_gate.h):启动完成之前与停机开始之后都不接新房间。闸没开就连许可都不取。
    if (const auto phase = admission_->Phase(); phase != battle_admission::AdmissionPhase::kOpen)
    {
        LOG_WARN << "CreateBattle rejected: this node is not accepting new rooms, battle_id=" << request->battle_id()
                 << " admission=" << battle_admission::ToString(phase)
                 << ",returning UNAVAILABLE " << kBattleNotAllocatableMessage;
        call->Reject(NotAllocatable());
        return;
    }
    // 2) 分配许可(集群外入口 D82)。拿不到(排空标签在、allocate 未确认、lifecycle 已停 / 仍在 Starting、等待超时)即拒绝:
    //    此刻还没进 Handle*,房间、定时器、推送、Kafka 事件一概没有发生。非 Agones 环境许可恒放行(启停窗口由第 1、3 道兜住)。
    //    回调只捕获 call 与 weak 闸,不捕获 this(AGENTS §11.7)。
    agones::GameServerLifecycle::Instance().AcquireAllocationPermitAsync(
        loop_,
        [call, gate = std::weak_ptr<battle_admission::AdmissionGate>(admission_)](
            agones::GameServerLifecycle::AllocationPermit permit)
        {
            auto &lifecycle = agones::GameServerLifecycle::Instance();
            if (!permit)
            {
                LOG_WARN << "CreateBattle rejected: this instance is not allocatable, battle_id=" << call->request().battle_id()
                         << " agones_state=" << agones::ToString(lifecycle.State())
                         << " draining=" << (lifecycle.IsDraining() ? 1 : 0)
                         << ",returning UNAVAILABLE " << kBattleNotAllocatableMessage;
                call->Reject(NotAllocatable());
                return;
            }
            // 3) 许可回到 loop 后复核准入闸:等许可期间 SetBeforeShutdown 可能已关闸并作废全部房间(关闸与 AbortAllRooms
            //    在同一个 loop 任务里,排在后面的本回调必然看到 kClosed);放行的话房间会在进程退出时无声丢失。
            if (!AdmissionOpen(gate))
            {
                LOG_WARN << "CreateBattle rejected: shutdown started during dispatch(rechecked in loop), battle_id="
                         << call->request().battle_id() << ",returning UNAVAILABLE " << kBattleNotAllocatableMessage;
                call->Reject(NotAllocatable());
                return;   // permit 随回调返回析构:零房间时自动收口回 Ready
            }
            // permit 活到 Proceed() 返回:在途创建计入 lifecycle,防止"最后一个旧房间刚移除"时 worker 抢先回 Ready;
            // 调用已失效 / 幂等命中而零房间时,析构自动收口回 Ready。
            call->Proceed();
        });
    return;
```

battle 的 `grpc.DestroyBattle.guard`:

```cpp
    if(!BattleRoomManager::Instance().AcceptsPlacementOperation(request->battle_id(),request->placement_operation_id(),request->target_instance_id()))
    {
        call->Reject(muduo_grpc::Status(muduo_grpc::StatusCode::kFailedPrecondition,"placement_operation_mismatch"));
        return;
    }
```

#### 15.8.5 迁移步骤

1. 新生成器的 go test 全绿。其中「旧段非空拒绝生成」用例的固件取自工作区真实内容:scene 1 段,battle 4 段,共 5 段。
2. 手改 4 个文件,改成与新模板逐字一致的骨架,把 §15.8.4 的内容填进 guard 段;`Handle*` 段**不动**。
3. 定向生成两个 proto(§15.14 第 2 步),要求以下检查全部通过:
   - 这 4 个文件零 diff,或只有骨架空白差异;
   - 每个 `Handle*` 段与迁移前逐字节相同;
   - 正向 grep 必须命中;
   - 负向 grep 不得命中。
4. 再生成一次,diff 必须为空(幂等)。
5. 单独一步改 `Handle*` 体里过时的注释。

### 15.9 Agones 异步许可

```cpp
namespace muduo { namespace net { class EventLoop; } }
// GameServerLifecycle 新增(public):
using AllocationPermitCallback = std::function<void(AllocationPermit permit)>;
// EventLoop 上的调用方(scene / battle 的 gRPC 守护段)用的有界异步许可。
//  * 只能在 loop 线程调用(断言);done 非空。
//  * done 恰好一次,永不在本函数栈内执行:
//    - 当场可判定的(Disabled / Allocated → 放行;已停 / 排空 / ShuttingDown → 拒绝)也经 loop.queueInLoop 投递;
//    - 有结果时由 worker 线程经 queueInLoop 投递;
//    - 等待超时由该 loop 上的定时器裁决并就地调用。
//  * 判定谓词与 Blocking 版相同,共用私有判定函数,日志原样。放行时已 ++pendingPermits_,permit 析构即释放。
//  * 等待期间计入 allocateWaiters_,晚到分配收口的语义不变。
//  * 等待上限 options.allocateWaitTimeout;到点按 steady 复核,未到就重新上弦(muduo 定时器已按单调时钟到期,见 §6)。
//  * Stop() 在锁内摘下全部等待者,锁外经各自的 loop 投递拒绝;之后的调用立即(仍经投递)拒绝。
//    Stop() 必须在等待者所属 loop 仍存活时调用:scene / battle 都在 beforeShutdown hook 里调用。
//    析构(静态析构)只丢弃等待者,不投递。
void AcquireAllocationPermitAsync(muduo::net::EventLoop &loop, AllocationPermitCallback done);
```

私有部分(受 `mutex_` 保护,没有 `T*` 字段):

```cpp
struct AsyncPermitWaiter { std::reference_wrapper<muduo::net::EventLoop> loop; std::uint64_t startGeneration;
                           std::chrono::steady_clock::time_point deadline; AllocationPermitCallback done; };
struct PermitDelivery { std::reference_wrapper<muduo::net::EventLoop> loop; bool granted; AllocationPermitCallback done; };
std::unordered_map<std::uint64_t, AsyncPermitWaiter> asyncWaiters_;
std::uint64_t nextAsyncWaiterId_ = 1;
std::shared_ptr<int> aliveToken_ = std::make_shared<int>(0);    // 超时定时器只弱持有
std::optional<bool> DecideWithoutWaitLocked(bool reservePermit); // 抽自 EnsureAllocatedBlockingImpl 前半,日志原样
bool WaitSatisfiedLocked(std::uint64_t startGeneration) const;   // 即 Blocking 的 wait_for 谓词  // 2026-10-17:ShuttingDown 视为成立(终态,挂起的 allocate 会被丢弃、不再有结果)
bool FinishWaitLocked(bool reservePermit);                       // 抽自后半:排空 / 非 Allocated → false,否则预占  // 2026-10-17:已停优先,stopRequested_ 时一律拒绝;其次 ShuttingDown 视为终态拒绝(WARN),在排空之前
void ResolveAsyncWaitersLocked(std::vector<PermitDelivery> &out, bool rejectAll); // 谓词成立者出表、--allocateWaiters_
void ExpireAsyncWaiter(std::uint64_t waiterId);                  // loop 定时器到点
void DeliverPermits(std::vector<PermitDelivery> deliveries);     // 只在锁外调用
void StopImpl(bool deliverRejections);                           // Stop() = StopImpl(true);~GameServerLifecycle = StopImpl(false)
```

**算法**:

- **Acquire**(loop 线程):
  - 持锁调用 `DecideWithoutWaitLocked(true)`。有结论就解锁并投递。
  - 否则:`allocateRequested_ = true`,登记等待者,`++allocateWaiters_`,`notify_all`;解锁后 `loop.runAfter(timeout, [alive = std::weak_ptr<int>(aliveToken_), this, id]{ if (alive.lock()) ExpireAsyncWaiter(id); })`。
- **结算点**:凡是改状态之后 `notify_all` 的地方,都先在同一临界区里调用 `ResolveAsyncWaitersLocked(out, false)`。包括:
  - `DoAllocate`:必须在 `++allocateResultGeneration_` 之后、晚到收口检查之前。放行者已经计入 `pendingPermits_`,`RequestReadyIfIdleLocked` 自然返回假;等待者全部超时离开时照常收口;
  - `DoBackToReady` 的两个分支;
  - `DoPollDrain` 中 `draining_` 翻转为真时;
  - worker 丢弃 allocate 时的 Allocated 分支;
  - (2026-10-17)worker 取走 /shutdown、写入 `ShuttingDown` 的同一临界区(发 HTTP 之前):等待者一律按 `ShuttingDown` 拒绝。此后挂起的 allocate 由 allocate 分支丢弃,不再有结果(§15.17.1 第 1 条)。
  - 解锁后再 `DeliverPermits`。
- **ExpireAsyncWaiter**(loop 线程):持锁查找。不在表里就什么都不做(结果已投递)。steady 时间还没到就重新上弦。否则出表、`--allocateWaiters_`;解锁后打 `LOG_ERROR "allocate not confirmed within Xms (async)"`,然后就地 `done(AllocationPermit(*this, false))`。
- **Stop / StopImpl**:锁内摘下全部等待者作为拒绝,worker 存在时置 `stopRequested_`;锁外投递拒绝,再 `notify` 和 `join`,其余与现有实现相同。worker 与 Stop 竞争时,谁先摘下等待者谁裁决。已放行、还在 loop 队列里的回调会在 hook 之后执行:battle 被复核闸拒绝,scene 被 `ServerDraining()` 拒绝。
- **不可拷贝的 permit 怎么随回调传递**:
  - 锁外先构造 `AllocationPermit p(*this, granted)`(私有构造函数只能在成员函数内调用),再 `std::make_shared<AllocationPermit>(std::move(p))`(走公开的移动构造),连同回调一起放进 `shared_ptr` 盒子;
  - `loop.queueInLoop([box = std::move(box)]{ AllocationPermitCallback done; done.swap(box->done); done(std::move(*box->permit)); })`;
  - worker 线程**从不持有**回调的最后一个引用。闭包没执行就随 `~EventLoop` 销毁时,permit 析构照常记账,ServerCall 走「服务端已不在」的分支。
  - 有结论的 permit 一律在锁外析构,避免与 `FinishAllocationPermit` 自锁。
- **Blocking 版**保留,改为调用同一组私有函数,行为逐字不变,现有用例照旧通过;注释改为「仅非 EventLoop 线程 / 测试使用」。`agones_gameserver_lifecycle.h` 头注释第 2 条硬约束与 `cpp/AGENTS.md:68` 同步改。
- **`SceneHandler::CreateScene`(RPC0)**:NonBlocking 保持不动,只改 `scene_handler.cpp:479-485` 守护段内那句「SceneManager 走 gRPC 用阻塞版」的注释。

### 15.10 关机(全部在 loop 内)

```
ShutdownInLoop(loop 任务 T0,顺序不变)
  1 kafkaManager.StopConsumers()
  2 beforeShutdownFn_
      battle:CloseAdmission → AbortAllRooms → DisconnectAll → lifecycle.Stop()
      scene :guid 号段停 → lifecycle.Stop() → 停定时器 → HandleExitGameNode(全部在线玩家)
      Stop() 把等许可的调用以「拒绝」投进队列(T0 之后执行)
  3 StartShutdownDrainWatchdog()(15s,100ms,不变)
  4 ShutdownGrpcServer() → grpcServer_->BeginShutdown({2s, 1s, 1s}, [this]{ OnGrpcServerDrained(); })
      a draining_ = true(此后 ServerDraining() 为真)
      b Acceptor 移进 queueInLoop 闭包销毁(停听);销毁前到达的接入一律 sockets::close
      c 每条连接:收到过字节 → SubmitShutdownNotice(GOAWAY 2^31-1 + PING),冲出,上弦 1s;没收到过 → forceClose
      d 上弦 2s 排空定时器(steady 复核)
      e 已满足排空条件 → queueInLoop(onDrained)(永不在本栈内回调)
  5 MaybeFinalizeShutdownInLoop()(gRPC 还没排空,直接返回)
之后的若干轮:
  - 已收完的调用照常派发:CreateBattle 被闸拒(battle_not_allocatable),CreateScene 被 ServerDraining 拒
    (scene_node_shutting_down),其余方法照常执行(与 grpc::Server::Shutdown「在途调用照常完成」同义)
  - 许可拒绝回调:CreateScene → "agones allocate not confirmed";CreateBattle → battle_not_allocatable
  - PING ACK 或满 1s → SubmitFinalGoaway;会话既不想读也不想写(nghttp2 want_read 与 want_write 同时为 0,即最终 GOAWAY 已写出、没有活跃流)
    → shutdown()(写半关;muduo 在输出缓冲写空后才发 FIN),closeGrace 从此刻起算作硬上限
  - 2s 到期(2026-10-17 改):未开始的调用回 REFUSED_STREAM,请求体还在上传的流也立即回 REFUSED_STREAM,已开始的回 UNAVAILABLE "server shutting down";
    所有连接补最终 GOAWAY,**不立即写半关**:连接继续读(WINDOW_UPDATE / RST / PING ACK)、继续按背压取帧写出,被对端流控卡住的已完成应答也写完,
    直到会话既不想读也不想写才写半关;closeGrace 从排空到期起算,是硬上限
  - 宽限到期:剩余连接 forceClose(排进 forceCloseInLoop,close 回调再排 connectDestroyed);对端始终不读或不给窗口时由它兜底
  - 连接表为空 → queueInLoop(onDrained),按 FIFO 排在最后一个 connectDestroyed 之后
OnGrpcServerDrained(排队任务;不在 Server 自己的栈上,也不在 I/O 事件处理期间)
  → 打 "gRPC server drained. drain_ms=… <FormatServerStats>" → grpcServer_.reset() → shutdownGrpcDrainComplete_ = true → MaybeFinalize
FinalizeShutdownInLoop(阶段 0)
  防御:grpcServer_ 仍在 → LOG_ERROR 后 queueInLoop([s = std::move(grpcServer_)]{})
       不能同步 reset:可能是从看门狗定时器进入的,Linux 的定时器回调就跑在事件处理期间
  然后照旧:ReleaseNodeId(撤销租约与发现键)→ … → queueInLoop(FinalizeTeardownInLoop)(FIFO 排在防御销毁之后)→ … → 两跳 quit
```

- **删除**:`grpcShutdownThread_`、`ShutdownGrpcServer` 里的线程、`~Node` 里的 join(`node.cpp:473-476`)、`node.h:27` 的 `<grpcpp/grpcpp.h>`、`node.h:20` 的 `<thread>`(先确认没有别的成员在用)。
- **保留** `~Node` 的兜底循环,注释改为「loop 异常退出时重新驱动 loop 内的 gRPC 排空与收尾」。
- **改注释**:`node.h:184-186`(RequestShutdown:handler 不能同步等待 drain,否则 drain 永远等不到 loop)、`node.h:202`(删掉「gRPC 线程上的许可阶段」)、`node_entry.h:144-147`、scene `main.cpp:202-203 / :287-288`(以 OnGrpcServerDrained 为界)、battle `main.cpp:341-353`(「框架随后关 gRPC」改为本节语义)。
- **与看门狗的关系**:并行运行;`MaybeFinalize` 仍要求「gRPC 已排空」且「业务已排空或已超时」。gRPC 排空完成之后不会再有 gRPC 来源的新工作。
- **拆除前钩子(2026-10-17 补,R7;已落码,未编译、未测试,待 Codex 验证)**:上图「queueInLoop(FinalizeTeardownInLoop)」之后,`FinalizeTeardownInLoop` 在 `kafkaManager.Shutdown()`(producer 最后一次有界 flush)之后、`tlsRedis.Shutdown()` 之前调用 `Node::SetBeforeTeardown` 登记的钩子,用成员 `beforeTeardownRan_` 保证每个 Node **只调一次**。`GrpcClientLocalCompletionsDrained()` 为假时本函数会 `queueInLoop` 重入自身让一轮(等 gRPC 本地延后完成在 ECS / 日志仍活着时跑完),重入时不再调钩子。钩子跑在 loop 线程,此时 Redis、ECS、节点注册表与异步日志都还活着;它不得再发起需要等待的异步工作,也不得同步阻塞。目前只有 scene 设置(`cpp/nodes/scene/main.cpp`:`tlsRedisSystem.StopDbTaskDispatch()`,停 DBTask 回执 Pump 并打未落定玩家清单,见 [db-task-delivery-and-retention.md](db-task-delivery-and-retention.md) §A.5「停机」),gate / battle 不设,两跳 quit 与本节其余顺序不变。

### 15.11 运维与可观测

- **启动日志**:`gRPC server (muduo_grpc) listening on <ip:port> services=[…] methods=N max_streams_per_conn=256 max_in_flight=1024 dispatch_per_turn=64/2ms threads=0`。
- **`GRPC_SERVER_MAX_POLLERS` 退役**(行号以动手时 grep 为准,这几个文件都有他人改动,只做局部替换):
  - `node.cpp`:删掉解析。设了这个变量就打**一次** `LOG_WARN "GRPC_SERVER_MAX_POLLERS 已退役(第 4 批起服务端无 poller 线程),忽略 value=…"`。
  - `tools/scripts/k8s_deploy.ps1`:
    - `:153` 保留参数,默认值改为 0,传入大于 0 的值时 `Write-Warning`;
    - 删掉 `:3439-3441` 与 `:4035-4041` / `:4112` 的注入;
    - `:4330` 的透传保留,值为 0;
    - tcpSocket 探针注释(`:3466` 附近)补一句「新服务端对不发 preface 的连接静默处理;排空期间监听已关,探针失败属预期」。
  - `tools/scripts/lib/k8s_client_entry.ps1`:参数 `:619-620` 保留并标注已退役(未跟踪的 `lobby_authority_render.tests.ps1` 还在传它);删掉 `:692-693` 的注入;`:13 / :60` 注释同步。
  - 文档:`deploy/k8s/README.md:348/371`、`.github/copilot-instructions.md:105`、`docs/design/thread-count-monitoring{,_en,_zh}.md`、`scene-node-threading-model.md`、`scene-grpc-server-design.md`(抬头注明已被本节取代)、`battle-transport-decision.md:60`。
- **低基数统计**:C++ 节点没有 Prometheus 出口,沿用 `[RpcChannel] summary` 的口径:Server 自己每 30s 检查一次,有变化才打一行。

```
[GrpcServer] summary listen=<ip:port> conns=<cur>/<peak> accepted= closed_before_preface= handshake_timeout= protocol_errors=
 inflight=<cur>/<peak> dispatched= max_backlog= max_dispatched_per_turn= rejected_overload= rejected_protocol= unimplemented=
 deadline_before_start= deadline_after_start= cancelled_before_start= aborted_after_start= refused_on_shutdown=
 unavailable_on_shutdown= late_finish= dropped= dropped_off_loop= codes=<code:n,…> methods={<Method>:<code>=<n>,…}
```

  维度只有方法(≤15 个)乘状态码(≤17 个);不出现 player_id 和 peer。
- **日志限频**:规则同 `ShouldLogRpcEvent`(前 8 条逐条打,之后每 1024 条打一条),在 server.cpp 本地实现,因为 muduo_grpc 不依赖 core。
  - ERROR:dropped(程序缺陷信号);
  - WARN:坏 magic、协议错误、过载、未知方法、排空到期强制收口(每次停机汇总一行);
  - DEBUG:探针断开、握手超时、late_finish。

### 15.12 行为变化(调用方须知)

- 节点上不再有 gRPC poller / EventEngine / 关机线程;`GRPC_SERVER_MAX_POLLERS` 不再生效。
- 同一节点上的请求按「收完」的顺序进入业务(以前没有顺序)。不写成契约。
- 过载时:每连接超过 256 条流,在 grpc-go 客户端排队(可能以没发出去的 DeadlineExceeded 结束);全节点超过 1024 条,回 RESOURCE_EXHAUSTED(以前是 poller 争抢加超时)。
- 服务端执行截止,到期回 DEADLINE_EXCEEDED,与 grpc C++ 一致。
- 排空到期:未开始的回 REFUSED,经透明重试后表现为 UNAVAILABLE;已开始的回 UNAVAILABLE。旧路径多为 CANCELLED。
- 停机期间 scene 的 CreateScene 回 `UNAVAILABLE "scene_node_shutting_down"`(新增)。
- UNIMPLEMENTED、415、405、400 的文案和状态码改为 grpc-go 口径,只有畸形客户端会遇到。
- 服务端不再做 too_many_pings 强制,对客户端更宽松。
- bind 不再开 SO_REUSEPORT,误重复 bind 时 fail-fast。
- K8s 探针连接不再产生日志。

### 15.13 风险

| 风险 | 缓解 |
|---|---|
| 守护段丢代码(6c4021ae5 同类事故) | 旧段非空即拒绝生成,且在写 .h 之前检查;固件用真实内容;正向和负向 grep;`Handle*` 段逐字节比对;二次生成幂等 |
| 自管连接的时序错误(`TcpConnection.cc:71` / `Channel.cc:43` 断言,`removeChannel` 断言) | 逐行照抄 TcpServer;必须设 `connectionCallback`;close 回调无条件排 `connectDestroyed`;Acceptor 和 Server 只在排队任务里销毁;回环用例覆盖「定时器回调里 BeginShutdown 同时有并发连入」和「带活连接析构」 |
| 恰好一次被破坏(重复完成 / 在途计数泄漏 / 排空收不拢) | 唯一裁决点 `CompleteCall`;各完成源组合的单测都断言在途计数归零、只发一次 trailers;排空有约 3s 的硬上限 |
| battle 准入语义被打破 | 闸 → 异步许可 → 复核 weak 闸;Stop 拒绝全部等待者;用例覆盖「放行回调排在关闸之后」 |
| 许可泄漏或重复回调 | 等待者在 mutex 内只摘一次;Agones 用例断言 `pendingPermits_` 最终为 0、没有 unbalanced 日志 |
| 业务之后才发生的非「未知」码(序列化失败 INTERNAL、应答超过 4MiB 的 RESOURCE_EXHAUSTED) | proto3 下前者实际不会发生;后者 Go 客户端收包时本来也报同一个码;打 ERROR |
| 删掉 node.h 里的 grpcpp / thread 之后,依赖传递 include 的翻译单元编不过 | 非生成代码里的 `grpc::` 只剩 node.{h,cpp} 和两对 handler(已 git grep);编不过就在该翻译单元补它自己需要的 include |
| Windows wakeup 修复(第 3 批)没落地 | 派发和许可投递会退化为最多 100ms 延迟;第 4 批编译与验证以第 3 批合入为前提 |
| 进程里仍有别的路径构造官方 grpc 对象,线程照样起来 | §15.14 第 8 步按线程名和 thread baseline 核对;第 5 批运行期守卫已落地(`grpc_runtime_probe` + ThreadMonitor 采样时核对 `grpc_is_initialized()`,见 thread-count-monitoring「gRPC Runtime Guard」) |
| 并行会话冲突(battle main.cpp、reconcile.go、k8s_deploy.ps1、muduo_grpc_test.vcxproj、rpc_controller_test.vcxproj、本文档) | 按组的文件清单操作;注释只做局部替换;vcxproj 等第 3 批提交后再追加 |
| 2ms 的每轮预算与 1024 的在途上限没有压测数据 | 统计里有 `max_dispatched_per_turn`、`max_backlog`;按 AGENTS §6 跑一轮 ReleasePlayer 突发压测并做对比表之后再调,没有对比表之前不宣称任何性能结论 |

### 15.14 验证(交 Codex;Claude 未执行,全部未编译)

1. **生成器**:在仓库根目录执行 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`;然后在 `tools/proto_generator/protogen` 下执行 `go test ./internal/... ./cmd`。要求全绿,包括 `LegacyNonEmptySectionsRefuseGeneration` 和头文件预检用例。
2. **迁移前的拒绝验证**:在 `tools/proto_generator/protogen` 下设 `$env:PROTO_GEN_CONFIG_PATH=(Resolve-Path etc/proto_gen.no-unity.yaml)`,执行 `./proto-gen.exe --grpc-handler proto/battle/battle_node.proto`。期望:非零退出,报错恰好列出 battle 的 4 段;4 个 handler 文件的 `git diff` 为空。保留完整报错文本。
3. **迁移后的定向生成**:按 §15.8.4 手改之后,对 `proto/scene_manager/scene_node_service.proto`、`proto/battle/battle_node.proto` 各跑一次 `--grpc-handler`。要求:
   - 4 个文件零 diff(或只有骨架空白差异);
   - 正向 grep 必须命中:`AcquireAllocationPermitAsync`(两份 cpp)、`battle_not_allocatable`、`agones allocate not confirmed`、`placement_operation_mismatch`、`scene_node_shutting_down`、`2026-09-15 恢复`;
   - 负向 grep `rg -n "grpc::|grpcpp|<future>|promise|\.grpc\.pb\.h|AcquireAllocationPermitBlocking" cpp/nodes/scene/handler/grpc cpp/nodes/battle/handler/grpc` 不得命中;
   - `runInLoop` 在改完 `Handle*` 注释之后也不得命中;
   - 再跑一次,diff 为空。
4. **Windows 构建**:`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`,退出码 0,no-raw-pointer 检查通过。rpc_controller_test 不在 sln 里,要单独编译。失败时保留第一个出错工程的名字和前 50 行输出。
5. **C++ 单测**:依次执行 `pwsh tools/scripts/run_cpp_tests.ps1 -Filter muduo_grpc`、`-Filter agones`、`-Filter rpc_controller`、`-Filter battle_admission`,全绿;muduo_grpc_test 的线程快照用例必须通过。
6. **Go**:在 `go/shared` 下执行 `go test ./assetop/... ./grpcinterop/...`(互通用例在没有环境变量时跳过)。互通步骤:先后台启动 `muduo_grpc_test.exe --gtest_filter=ServerInterop.*`,设 `MUDUO_GRPC_SERVER_INTEROP_PORT=50999`;再执行 `MUDUO_GRPC_SERVER_INTEROP_ADDR=127.0.0.1:50999 go test ./grpcinterop/ -run TestMuduoServerInterop -count=1 -v`。要求两边都通过,C++ 退出时打出的 `[GrpcServer]` 行里 `dropped=0`。
7. **Linux**:`tools/scripts/build_linux.sh`,确认 muduo_grpc 的 CMake 包含新增源文件,muduo_grpc、infra、core、scene、battle 都能编过。
8. **冒烟与线程核对**(环境:etcd + Go 服务 + gate + scene + battle;先 `redis-cli FLUSHALL`、清日志):
   - 业务流程:登录进场(CreateScene)、换图(ReleasePlayer)、资产 Debit / Credit / AckReceipt、匹配 PrepareBattle / CreateBattle / QueryBattleCreation / AddObserver / IssueBattleTicket / DestroyBattle、缩容 DestroyScene。
   - 日志里应有 `listening` 行和 `[GrpcServer] summary`,不应再有 `gRPC server config: max_pollers`。
   - 对比改前改后的 `thread baseline (stable window reached): total_threads=`。
   - Linux 上 `for t in /proc/<pid>/task/*; do cat $t/comm; done | sort | uniq -c` 不得出现 grpc 相关线程名(名单以改前采样为准)。不满足就不宣称「线程归零」。
9. **关机**:有流量时分别对 scene / battle 按 Ctrl+C(K8s 下为 SIGTERM)。
   - 无断言;
   - 日志依次出现 `gRPC server draining`、`gRPC server drained. drain_ms=`、`Before-shutdown drain complete`;
   - 停机窗口里 CreateBattle 只出现 `battle_not_allocatable` 或连接被拒,CreateScene 只出现 `scene_node_shutting_down` 或连接被拒。
10. **探针**:对 gRPC 端口执行 `nc -z` 100 次。不得出现 WARN / ERROR,`closed_before_preface` 递增,`protocol_errors` 为 0。

### 15.15 实施分组(文件互不重叠)

| 组 | 内容 | 依赖 |
|---|---|---|
| G1 | muduo_grpc 服务端库 + muduo_grpc_test 新用例 + Go 互通用例 | 无。测试工程的 vcxproj 等第 3 批提交后再追加 |
| G2 | Agones 异步许可 + agones_lifecycle_test | 无 |
| G3 | 生成器模板、`grpc_handler_gen.go`、`grpc_code_sections.go` 与三个测试 | 无(只约定接口名) |
| G4 | core 的 `grpc_unary_call.h`、Node 接线与关机、可选的 rpc_controller_test 回环用例 | 编译依赖 G1 |
| G5 | scene / battle 手写段迁移、相关注释、定向重生成 | G1–G4 |
| G6 | 部署脚本、文档、Go 注释、PROGRESS | 定稿跟随 G1–G5 |

先后:G1、G2、G3 并行 → G4 → G5 → G6 → Codex 按 §15.14 验证。

双版本(AGENTS §12):本批只替换 C++ 服务端的内部传输,客户端契约、proto、tip 码、配置表都不变。Java 版不适用,PARITY 不登记功能行;PROGRESS 条目里注明这一点。

### 15.16 落地记录(2026-10-16,未编译、未重生成)

七组并行落码(服务端协议层 / Agones 异步许可 / 生成器 / 服务端主体 / core 胶水与 Node / 守护段迁移 / 运维文档),第 5 批守卫另成一组;
每组一轮语义评审,最后一轮全仓遗漏检查。遗漏检查用只读模板渲染器把 4 个 handler 文件按新模板逐字节比对,全部一致,
15 个 `Handle*` 体与迁移前一致(HEAD 没有的方法以别的会话的工作区版本为准)。唯一一条 high(最终 GOAWAY 已提交、尚未写出时到达的
新流仍会被派发)已由评审当场修复。

**新增 / 主要改动文件**:`cpp/libs/engine/muduo_grpc/{address,method_table,server_session,server}.{h,cpp}`、`server_call.h`、
`message_framing.cpp`(预分配封顶);`cpp/libs/engine/core/network/grpc_unary_call.h`;`node.{h,cpp}`、`node_allocator.cpp`、
`node_entry.h`、scene / battle `main.cpp`;`infra/agones/agones_gameserver_lifecycle.{h,cpp}`;生成器 `grpc_service_handler.{h,cpp}.tmpl`、
`grpc_handler_gen.go`、`grpc_code_sections.go` 与测试;scene / battle 4 个 handler 文件;`core/node/system/node/grpc_runtime_probe.{h,cpp}`、
`thread_observability.h`;测试 `muduo_grpc_test` 的 `test_h2_client` / `server_session_test` / `server_test` / `server_interop_test`、
`go/shared/grpcinterop/muduo_server_interop_test.go`、`agones_lifecycle_test` 异步用例、`rpc_controller_test` 的 `grpc_client_loopback_test` /
`grpc_runtime_not_initialized_test`;部署脚本与文档见 PROGRESS 条目。

**与本节的偏离(已核实,均有理由)**:
- `ServerSession` 的帧级协议错误、rapid reset、glitch 由 `DrainOutput` 报告(nghttp2 对这些违约只排带错误码的 GOAWAY,`mem_recv2` 仍返回成功);
  `Receive` 只报坏 magic / FLOODED / 内部失败,且粘滞。
- 首字节之前不写任何字节,在会话层强制;shutdown notice 的 PING 推迟到 GOAWAY 写出之后才提交(先 GOAWAY 后 PING,ACK 才能证明对端处理过 GOAWAY)。
- `Observer` 多一个带默认空实现的 `OnRequestRejected`,供统计;`MethodTable` 多只读 `size()` / 透明哈希,空 handler 也算非法。
- `IsCanonicalIpv4` 逐字符校验,比 `channel.cpp` 的私有副本更严(那份会接受 `-1.2.3.4`);2026-10-17 `channel.cpp` 已改调 `address.h` 并删掉私有副本。
- `dropped_off_loop` 统计每一次 loop 外析构句柄(loop 外无法判断调用是否已完成),正常路径恒为 0。
- 启动日志 `services=` 打个数;`node.cpp` 用临时 `MethodTable` 数方法数。
- `ThreadMonitor` 每小时指标行的哨兵改为 `std::optional`(原 `time_point::min()` 相减溢出,这行从来没打出来过);现在启动后第一次采样就多打一行 INFO。
- Windows 上 muduo `Acceptor` 无条件 `SO_REUSEADDR`,§15.12「误重复 bind fail-fast」只在 Linux 成立。

**原「待负责人拍板」的 7 条取舍:负责人 10-16「除了 codex 其他的你按最标准的做」,2026-10-17 已全部落码(未编译)**:
1. Agones `Stop()` 在 beforeShutdown hook 里于 loop 线程上 join worker,最坏约 6.4s(allocate HTTP 重试),期间 loop 冻结、gRPC 排空推后。
  → 已做:可放弃请求(/ready、/health、/allocate、GET /gameserver)带 `std::stop_token`,经 libcurl 进度回调中止,重试与退避立即退出,Stop 通常毫秒级、最坏约 1s(libcurl 空闲期进度回调粒度,且不超过 timeouts.total);/shutdown 必须送达,挂起时 worker 退出前先送达(多等最多 timeouts.total);Stop 之后阻塞等待方也一律拒绝,不再签发许可。
2. 排空到期时连接输出被背压(>1MiB),排队的 REFUSED / UNAVAILABLE / 最终 GOAWAY 可能写不出去,对端只看到 EOF(UNAVAILABLE,结果未知,安全)。修法是「冲完再半关、宽限作硬上限」。
  → 已做:见 §15.10 新语义(写半关以 want_read / want_write 同时为 0 为准,closeGrace 从排空到期起算作硬上限,上传中的流立即 REFUSED;FlushFinalFrames 一步删除,优雅关闭期间照常守背压)。
3. Agones 形态停机时,`Stop()` 先于 BeginShutdown 拒掉等许可的 CreateScene,打 ERROR 且回 `agones allocate not confirmed` 而非 `scene_node_shutting_down`(SceneManager 都回滚,只是日志误导)。
  → 已做:`!createPermit` 分支先判 `ServerDraining()`,回 `scene_node_shutting_down` 并只打 WARN。
4. `StartGrpcServer` 不检查 `shutdownStarted`(沿用旧行为):etcd 端口分配晚于关机时,服务端会在排空之后才起,由 Finalize 的防御分支销毁。
  → 已做:`shutdownStarted` 为真时不启动服务端、不发布发现键,打一行 WARN。
5. `checkLegacyGrpcSections` 只检出「签名行含旧字面量、方法仍在 proto」的旧无名段;已删方法或折行签名会静默丢弃(当前工作区已迁完,无实际风险)。
  → 已做:见 §15.8.3「2026-10-17 改为全覆盖」。
6. 守卫是 fail-open(只记日志 / 指标);CI 或冒烟环境要不要做成 fail-closed(例如设环境变量即 LOG_FATAL)。
  → 已做:新增 `NODE_GRPC_RUNTIME_GUARD_FATAL=1/true/on`,CI 与冒烟环境设上即 fail-closed(发现即 LOG_FATAL),生产不设;见 thread-count-monitoring 文档。
7. AGENTS.md §11.7 参考实现列表补 `muduo_grpc::Server`(TcpServer 等价物)与 `ServerCall`。
  → 已做:§11.7 参考实现列表已补 `muduo_grpc::Server` / `ServerCall`,回归测试补 `server_test.cpp`。

**风险补记**:进入排空(GOAWAY_SUBMITTED)后 nghttp2 的 rapid-reset 令牌桶不再生效(约 3s 窗口,glitch 与 max_outbound_ack 仍有效);
`ServerCall` 最后一个副本若在 loop 外析构而调用未完成,记录要等截止 / 断连 / 排空到期才清,期间占着在途与流名额;
`rpc_controller_test` 工程内今后不得有用例构造官方 grpc 对象或调 `grpc_init`(否则 `GrpcRuntimeNotInitializedTest` 依执行顺序失败)。

### 15.17 收尾(2026-10-17,未编译)

负责人 10-16「除了 codex 其他的你按最标准的做」「全部做完给我」,在 §15.16 七条之外一并收口:

- **muduo 定时器改单调时钟**(三份树):到期判断与上弦一律按 steady_clock(Timestamp 承载、不是 epoch);`runAt` 调用时换算一次,之后不随墙钟变;
  §6 的「墙钟回拨推迟 N 秒」已消除,业务侧 steady 复核保留作纵深防御。详见 `muduo-timer-cancellation-hazards.md`「定时器时钟」一节。
  Linux 的 `TimerQueue.h` / `TimerQueue.cc` / `EventLoop.cc` 三份 overlay 必须同一个提交生效。
- **`RpcClient` 启动宽限删除**:`kStartupGraceSeconds` 只是日志降级开关,唯一依据是 d27176e04 已修掉的 `win_connect` 假阳性;删除后补回归用例。
- **`Connector` 裸 this**:`start()` / `resetChannel` 改绑 `shared_from_this()`,`connecting()` 按同寿命例外保留;TcpClient 自身的裸 this 是上游设计,不动布局。
- **测试缺陷**:ECS T8.9 加的 `ConnectionObserverReportsUpAndDownWithoutDispatcher` 原先「forceClose 后同一 functor 析构服务端」,Debug 下 `Channel::remove` 断言中止整个
  `rpc_controller_test`;已改为等服务端 accept 后直接析构服务端。
- **已知的既有边角问题(评审中发现,早于本批、生产代码到达不了,另起后续)**:单独调 `GameServerLifecycle::RequestShutdown`(不走 Stop)时
  `ShuttingDown` 守不住挂起的 allocate、`RequestShutdown` 不幂等;`codeParser` 不跟踪函数体(函数体内顶格的 `Impl::HandleX(` 或前置声明行会劫持)、
  `handler_gen` / `player_service_gen` 缺结构预检、`event.go` 的 `extractUserCodeBlocks` 有同类查找键劫持;服务端 `headersReceivedAt_` 超限修剪后,
  慢上传的流排空到期时不能立即 REFUSED(最坏等 closeGrace 硬上限);Windows `Channel.cc` 对只设读写回调的 channel(Hiredis)只有 POLLHUP 时仍提前返回,
  需先实测 WSAPoll 的 revents(交 Codex)。
  → 2026-10-17 已落码时尚未编译、未测试,Windows `Channel.cc` 当时留待实测。其余项见下面「边角问题落地记录」;2026-10-18 Codex 的 HUP 修复与红绿证据见下文及 §15.18。
  → 2026-10-18 Codex 已完成本机原生测量:纯 FIN 与数据读尽后的 FIN 均持续报告单独 `POLLHUP`(`0x0002`),`recv` 返回 0;
  数据尚未读尽时报告 `POLLRDNORM|POLLHUP`(`0x0102`)。随后链接真实 `muduo.lib` / Hiredis 的 8 例两次稳定出现同样 3 例失败,
  证实 Windows 纯 HUP 分支跳过 EOF 读取。最小修复是在没有 close / error 回调时派发一次 read 后立即返回,避免读回调清理后再调用 write;
  Connector 的 error 路径及已有 close 路径保留。修复后 8/8、0 跳过,真实 Hiredis 在析构前完成挂起命令并断开连接。
  8 例已接入正式 `muduo_grpc_test`,正式工程全套及重复运行仍按 §15.18 的后续回执判定。
  原生测量在 `build/verification/grpc-muduo-codex-20261018-005135/wsapoll-result.json`,红、重复红、绿及源码清单在同目录 `hup-prep/closure.json`。

#### 15.17.1 边角问题落地记录(2026-10-17,未编译、未测试)

按「最标准做法」逐项修,每项都补了能在修复前失败的回归用例:

1. **Agones `ShuttingDown` 是终态,`RequestShutdown` 幂等**(`infra/agones/agones_gameserver_lifecycle.{h,cpp}`):
   - worker 取走 /shutdown 时,在同一临界区里写入 `ShuttingDown`(与 Allocating / ReturningToReady 同理:状态在释放锁、发 HTTP 之前切换)并结算异步等待者、
     `notify_all`;`DoShutdown` 只发 HTTP。原先由 `DoShutdown` 另拿锁写状态、不结算、不唤醒。
   - allocate 分支遇 `ShuttingDown` 丢弃挂起请求并清 `nonBlockingAllocateKicked_`(打 WARN),不再把状态改回 Allocating / Allocated。
   - `WaitSatisfiedLocked` 把 `ShuttingDown` 视为成立,`FinishWaitLocked` 在「已停」之后、排空之前按 `ShuttingDown` 拒绝(WARN):
     阻塞 / 异步等待方即刻被拒,不再等满 `allocateWaitTimeout` 后误报「allocate 未确认」的 ERROR。
   - `RequestShutdown`:已有挂起请求或状态已是 `ShuttingDown`(在途 / 已发出)时忽略并打 WARN。取走请求与写入 `ShuttingDown` 同一临界区,两个条件之间没有空档,
     /shutdown 全程最多发一次;失败只打 ERROR,不靠重复调用重试。
   - 用例(`agones_lifecycle_test`):`AgonesShutdownTest.PendingAllocateIsDiscardedOnceShuttingDown`(借排空的那次同步 GET 卡住 worker,三类调用方挂上 allocate 后
     RequestShutdown;断言不发 /allocate、两类等待方都被拒且远早于上限、状态停在 ShuttingDown)、`AgonesShutdownTest.RepeatedRequestShutdownIssuesShutdownOnce`
     (挂起时重复调、在途时调、发出后调,最后 Stop 让任何被误收下的请求现形,/shutdown 恰好一次)。
2. **生成器取段规则统一并补结构预检**(`tools/proto_generator/protogen/internal`):
   - `codeParser` 改为按查找键取段(`ReadCodeSectionsByKeys`),函数边界与 `scanGrpcCppSections` 同一套签名累积规则:守护段外的函数起始行
     (`isCppFunctionStart`:签名行且含 `<类>::<名>(`;grpc 的 `<Svc>Impl::<名>` 是它的特例)开始签名,以 `{` 结尾才进入函数体,遇 `;` 放弃,
     新起始行重新匹配;函数体内不认起始行、不认查找键;FirstCode 是文件的第一个段、且出现在任何当前方法进入函数体或签名已命中之前,
     被放弃的前置声明不再让 FirstCode 落空(它若落在签名不对应当前方法的函数体内,见第 4 条)。
   - 解析同时把结构损坏(未闭合 / 嵌套 / 孤立结束标记)与「读到却在新生成里没有位置」的非空段(签名不对应当前方法的函数里的段、方法体里第一个段之后的段、
     FirstCode 之后函数外的段、同一方法重复定义时被覆盖的段)记进 `CodeSections.Problems`(带行号)。宽松入口 `ReadCodeSectionsFromFile` 照旧只报 I/O 错误,
     既有尽力而为的结果不变;`handler_gen` / `player_service_gen` 改走 `readCheckedCodeSections`,有问题即拒绝生成(.cpp panic、调用方不写文件),
     并新增 `GetServiceHandlerHeadFromFileStr` / `GetPlayerServiceHeadFromFileStr`(`handlers.go` 三处配置接上)在写 .h 之前对同名 .cpp 做同一预检,
     不留下「新 .h 配旧 .cpp」—— 与 gRPC 生成器同口径。预检就是解析本身,两者不会对「哪段带得走」各执一词。gRPC 生成器起初只补了读错误处理,
     仍走宽松入口;评审发现其扫描器与 codeParser 规则不一致会漏三种形状,已按第 4 条改为同样据 Problems 拒绝。
   - `generator/cpp/event.go` 的 `extractUserCodeBlocks` 改走同一套规则(查找键为去掉行尾换行的整条签名),Problems 非空或读失败时 `Fatal` 拒绝生成
     (原先打 WARN 照常渲染,会把自定义代码覆盖成空段);新事件文件(不存在)取默认空段对,原先首次生成连守护段标记都没有。
   - 用例:`TestReadCodeSections_FunctionBodiesAndDeclarationsDoNotHijack`、`TestReadCodeSectionsByKeys_ProblemsListBrokenAndLostSections`、
     `TestHandlerGenerators_RefuseToDropSectionsOfRemovedMethods`、`generator/cpp/event_user_code_test.go` 三例;「对现有生成物解析结果不变」由
     `TestReadCodeSections_WorkspaceHandlersParseUnchanged`(新增事件 handler 两个目录,非 gRPC 文件同时断言预检不误报、与宽松入口逐字一致)与
     `TestExtractUserCodeBlocks_WorkspaceEventHandlersUnchanged` 锁住。落码前用 awk 按新规则模拟扫过工作区 21 个 handler / 事件 handler 文件,无一误报。
3. **排空到期按会话状态拒上传中的流**(`muduo_grpc/server_session.{h,cpp}`、`server.cpp`):新增 `ServerSession::AcceptedUndispatchedStreams()`
   (头部已被接受、未派发、未应答的流,按 id 升序);`ServerConnection::RefuseUnfinishedRequests` 改用它。`headersReceivedAt_` 只剩截止起算一个用途,
   修剪不再影响停机;`Server::AcceptCall` 的排空到期分支降为防御。用例:`ServerSessionTest.AcceptedUndispatchedStreamsListsOnlyRequestsStillUploading`、
   `ServerLoopbackTest.DrainTimeoutRefusesUploadingStreamsEvenAfterHeaderTimesAreTrimmed`(`maxConcurrentStreams = 8` 使记录上限为 16,
   20 个收完头部即被对端 RST 的流把慢上传流的记录挤掉;修复前要等 20s 硬上限)。
4. **评审遗留(2026-10-17 对抗评审,均为既有问题;评审确认前三条无回归:23 个现有生成文件新旧解析逐字节一致,20 万次模糊测试 checked 路径无静默丢段)**:
   - **[medium] gRPC 生成器也按 codeParser 的 Problems 拒绝**:`Problems` 改为带类别的 `[]CodeSectionProblem`(`CodeSectionBroken` 结构损坏 /
     `CodeSectionUnplaced` 无处安放 / `CodeSectionOverwritten` 被覆盖;跳过的命名段带 `SectionName`)。`validateGrpcHandlerCppSections` 在原有三步
     (结构 → `checkLegacyGrpcSections` → 命名段)之后再按 Problems 拒绝(`refuseUnexpectedGrpcCodeSectionProblems`),只放过当前方法的 `grpc.<M>.guard`
     (落在 `<M>()` 里,codeParser 跳过是预期,由 `readNamedGrpcCodeSections` 按名字读走);并把这次解析交回 `GetGrpcServiceHandlerCppStr` 直接渲染,
     不再另走宽松入口重读。按段形状扫描放行、却会被 codeParser 静默丢掉的三种手改形状:(a) 文件第一个段落在一个没有段的现行 Handle* 之后、
     函数外(扫描器按「文件第一个段」当 FirstCode 放行,codeParser 已见过方法、整段跳过);(b) 同一 Handle* 定义两次(`#if` / `#else`,后段覆盖前段);
     (c) 段内某行夹带结束标记文本但不独占一行(codeParser 按行内含结束标记提前闭合,后半截落到段外,真正的结束标记成了孤立标记)。
     用例 `TestGrpcServiceHandler_RefusesShapesTheScannerAcceptsButCodeParserDrops`(三种形状 .cpp / .h 都拒绝、文件不改;对照:只填 guard 照常生成),
     `TestReadCodeSectionsByKeys_ProblemsListBrokenAndLostSections` 补「段内夹带结束标记文本」「跳过的命名段带名字」两例并校验类别。
   - **[low] FirstCode 落在别的函数体内要报**:缺 FirstCode 的文件里,第一个段落在签名不对应当前方法的函数体内(已删方法 / 手写辅助函数)时,
     宽松解析照旧把它当 FirstCode,但闭合时非空就记一条 `CodeSectionUnplaced`(重生成会把它搬到文件开头、函数随之消失);`codeParser` 文档注释与
     第 2 条的 FirstCode 说法已统一。用例 `TestReadCodeSectionsByKeys_FirstSectionInsideUnrecognizedFunctionIsReported`。
   - **[low] 函数体结束行放宽**:codeParser 与 `scanGrpcCppSections` 同批改用 `isFunctionBodyEnd`:顶格的 `}` 或 `};`,行尾空白不计;行首仍不许有空白
     (缩进的 `}` 是嵌套块的结束,若连行首一起去掉,旧包装函数体里的 `        }` 会提前结束函数体、把段归错)。修复前 `} ` / `};` 会让解析停在体内,
     其后方法的段被跳过或归错。用例 `TestReadCodeSections_FunctionBodyEndsOnTopLevelBraceVariants`、`TestScanGrpcCppSections_FunctionBodyEndsOnTopLevelBraceVariants`。
   - **[doc]** §15.9 结算点清单补「worker 取走 /shutdown、写入 `ShuttingDown` 的同一临界区」,`WaitSatisfiedLocked` / `FinishWaitLocked` 的说明补「`ShuttingDown` 视为终态拒绝」。
   - **已知、不改**(登记备查):`RequestShutdown` 以 worker 取走请求的时刻生效 —— 收下请求到 worker 取走之间(worker 正在一次 HTTP 里)仍可能签发许可,
     与 Allocating / ReturningToReady「worker 取走时切换状态」同一设计;/shutdown 只试一次、失败只打 ERROR,不重试(头文件 `Stop()` 注释已写明的设计选择,
     重复调 `RequestShutdown` 也不补发);`RunInitialReady` 重试 /ready 期间不看挂起的 shutdown(最长约 1 分钟,之后照常先发 /shutdown)—— 生产上只有进程决定自我终止时才调
     `RequestShutdown`,不会落在启动的 /ready 重试窗口里。
   - **(2026-10-17 核对补)第四种形状**:某个 `Handle*` 自己没有守护段、结束括号又带注释(如 `}  // ...`,不是函数体结束行)时,解析停在它的函数体里,
     其后 `<M>()` 里的 `grpc.<M>.guard` 被当成这个 `Handle*` 的体取走(缺 FirstCode 时 guard 也可能被当成 FirstCode),既不记 Problem 也过得了扫描器,
     生成物会出现 guard 错位、重名。`validateGrpcHandlerCppSections` 末尾新增 `refuseNamedSectionsTakenAsUnnamed`:FirstCode 或任一 Handle* 体以命名开始标记起头即拒绝,
     列出「文件:FirstCode|Handle<M> 体:段名」;用例补进 `grpc_code_section_consistency_test.go`。核对用只读推演跑遍工作区 23 个生成式 handler 文件:零误报,
     FirstCode 与 41 个方法段与 HEAD 逐字节一致;gRPC 预检约 3.4 万、handler 约 4 万个随机变体静默丢段为 0。已知且接受的保守拒绝:段内夹带结束标记文本、
     函数体内守护段外手写的顶格 `};`(均 fail-closed,不丢代码)。

### 15.18 Codex 验收回执(2026-10-18,获授权修复后续跑中)

- **生成器与 Go 已有证据**:生成器 `proto-gen-build`、定向单测与 `go test ./internal/... ./cmd -count=1` 均退出 0;`go/shared` 的 `assetop` 单测退出 0,`grpcinterop` 默认因缺真实端点跳过。Go 该轮日志没有独立输入 SHA 前后清单,不据此声称后续变化也已验证。实际端点互通仍待执行。
- **保留首轮失败**:`build/ecs-refactor/batch6/full-cpp-20261018T051013798Z/` 的串行 `game.sln /t:Rebuild /m:1 /nr:false Debug|x64 /p:CL_MPCount=1` 原生退出 1。首错为 `player_landing.h:51` 的 C2059,来自 `player_landing_test` Debug|x64 缺 `/utf-8`;另有 `open_file_limit_test.cpp` 的辅助函数 `Run` 被 `testing::Test::Run` 遮蔽。该轮还有 `config.cpp`、`core.vcxproj`、`node.cpp`、`open_file_limit.h`、`client_endpoint_test.vcxproj`、`open_file_limit_test.cpp` 共 6 项输入漂移,不能作为当前源码通过证据。
- **授权修复与复测**:负责人本轮「做完给我」授权后,已补测试工程 `/utf-8`,将辅助函数及调用改为 `EvaluateForTest`。随后共享增量 `game.sln` Build 原生退出 0,1813 个输入前后零漂移;它不是最终 Rebuild。复用本轮新 EXE 的完整用例为 `player_landing` 13/13、`client_endpoint` 64/64,均 0 失败、0 跳过,源及 21 个共享库无漂移。应用回执为本轮 `build-blockers/applied-receipt.json`;Build 原件为 `build/verification/landing-fencing-resume-20261018T053714Z/cpp/01-game.native-result.json`;复测为 `build/ecs-refactor/batch6/repair-checks-20261018T064437730Z/result.json`。
- **HUP 已完成红绿**:原生 WSAPoll 测量后,8 例真实 Channel / Hiredis 回归两次稳定出现精确 3 红、5 绿。最小 Windows Channel 读 EOF 修复后,单库构建和隔离回归均退出 0,8/8、0 跳过、输入零漂移;只更新 `muduo.lib`,其他库 SHA 不变。正式接入新增 `windows_channel_hup_test.cpp` 及测试工程/filters,正式工程运行尚待后续专项。两次绿色预检曾被短命链接进程阻挡,没有实际启动构建或测试,单独留有「未执行」回执,不作代码失败或通过证据。原件见本轮 `hup-prep/closure.json`。
- **最终完整重建已通过**:`build/ecs-refactor/batch6/full-cpp-20261018T065305567Z/` 的串行 Rebuild 原生退出 0,1814 个源码输入前后零变化,21 个库已封存;gate、scene、battle 三节点均重新链接。其 `incoming_contract_profile_pending` 指另一个尚未实现的双版本客户端契约入口,不代表编译失败,也不能据此声称双契约兼容完成。
- **正式 gRPC 工程构建通过,普通轮失败**:`build/verification/grpc-muduo-approved-20261018-014019/windows-tests-20261018T075842541Z-3331cf8f/` 中两工程以独立 IntDir、禁止构建引用工程的方式串行构建,均退出 0。muduo 普通轮计划 195 例,日志实际启动 173 例:166 例通过、5 例条件跳过、1 例已完成失败、1 例挂起。首个失败是 `ServerSessionTest.MalformedHttpRequestsAreResetByNghttp2` 的 `uppercase name` 子场景,期望 RST_STREAM(PROTOCOL_ERROR),实际收到应答并触发观察者。其后 `ServerLoopbackTest.DrainTimeoutUnderBackpressureForceClosesAfterCloseGrace` 挂起,内层运行器在 600 秒超时后终止测试,外层退出 1;不能用外层 `timed_out=false` 宣称没有超时。没有完整 XML 或整套通过结论。
- **失败收口与候选**:测试自有进程均已退出,最后只读终检退出 0,源码和 21 库零变化,报告为 `build/ecs-refactor/batch6/grpc-window-20261018T075842541Z-d25fda47/timeout-final-artifact-differences.json`。只读排查发现 Windows `accept4` 用 `FIONBIO=0` 把新连接设成阻塞,可能让 32MiB 应答堵住 EventLoop;这是高可信候选而非已确认根因。最小 `mode=1` 候选保存在本轮 `hup-prep/backpressure-fix-candidate/`,未应用、未重试;请求头子场景另行只读诊断。按 AGENTS.md §10.2 等待负责人决策,原冻结基线保持不变。构建队列已交回 ECS 执行不依赖该专项结论的既定验证。
- **请求头夹具只读定位**:`TestH2Client::MakeNva` 使用 `NGHTTP2_NV_FLAG_NONE`,其 `nghttp2_submit_headers` 经 `nghttp2_nv_array_copy` 自动把复制的头名转小写,实际发送的是 `x-upper`。因此上述失败没有证明服务端接受了非法大写头;后续应为恶意子场景构造真正的原始 HEADERS/HPACK,保留原有 RST 断言再验证。未修改正式夹具、未重新运行,不据静态定位宣称用例通过。
- **后续已获修正授权**:负责人在协调会话明确「继续做完,你可以先修复」,已核读原始人类 turn `01a14e1b-941f-7170-8a9c-c1cffb263341`。此前「待决策」「候选未应用」是失败停止时的状态。实际收件箱构建自然退出 0 后,再次核验原 1814 源/21 库均无变化,再 CAS 合入四个测试文件:恶意头名用显式 `NGHTTP2_NV_FLAG_NO_COPY_NAME` 且由夹具深拷贝持有到会话销毁后,保留全部 RST/观察者断言;新增真实 accepted socket 空读回归,既有 8 个 HUP 用例保留。当前生产 `accept4` 仍为旧 `mode=0`,尚未定向编译或红绿运行;先测试旧库取得短红测,再单独应用生产修正。原件为 `build/verification/global-handler-repair-20261018T082546455Z/repair-receipt.json` 与 `applied-FixTests-20261018T083356293Z/applied.json`。旧完整基线不改写,修正期间明确记录授权变化,不会伪称修后零漂移。
- **上述修正随后完成定向红绿**:四测试文件先链接旧库,大写头用例 1/1 通过;新增 accepted socket 空读单例实际触发外部 5 秒预算,wrapper=124、精确 RUN 后无 PASS,自有进程已退出,这是计划红测。再单独 CAS `accept4`:成功连接设置 `FIONBIO=1`,设置失败关闭新 socket 并恢复错误。单 muduo 工程实际重编该文件、退出 0,只有 `muduo.lib` 更新,其余 20 库不变;测试工程重链退出 0。同一新 EXE 下 socket/header/原背压排空三个单例均 1/1、0 跳过、退出 0,原挂起用例约 1.6 秒完成。前后审计均只出现已授权的五源/一库变化,没有额外变化。原件位于 `build/verification/grpc-muduo-approved-20261018-014019/repair-phase1-20261018T083805906Z-d6aa2996/phase1-result.json` 与 `repair-phase2-20261018T084836490Z-6213656d/phase2-result.json`;正式普通轮/20 轮仍待另行记录,定向通过不替代全套。
- **修复后的正式专项**:`windows-tests-20261018T090212825Z-b6ff7a42/` 中 muduo 普通轮 196 例为 191 PASS、5 个默认关闭的互通 SKIP;Agones 普通轮 69 PASS、0 SKIP。muduo 排空 26×20=520 PASS、0 SKIP。Agones 原 24×20=480 PASS,但最后约 3 秒与来源未知的主机 Go 编译重叠,不能称全程串行;随后在唯一窗口补跑 `agones-repeat-preparation-20261018T093715466Z/run-20261018T094050312Z-19d4caa8/`:20 轮逐轮各 24 PASS、共 480、0 SKIP,前后严格守卫和两次源/库审计均 0,EXE SHA 不变;旧重叠记录保留。原最后守卫 92 和后续只读终检 0 均保留,不改写旧失败回执。正式 HUP 8 例已随全套通过。
- **battle/router 与真实互通**:本轮 `cpp-prep/battle-runs/20261018T092020479Z-7197735f/result.json` 为 7/7,`router-lane-runs/20261018T092106696Z-d162aa33/result.json` 为 11/11,均 0 跳过。`interop-prep/artifact-runs/20261018T093213333Z-5045eb2d/` 中 Go `TestMuduoServerInterop` 及全部子例实际通过,C++ ServerInterop 1/1、0 跳过,两端退出 0;100 次零字节连接全部完成,最终 accepted=101、closed_before_preface=100、dispatched=312、protocol_errors/dropped/dropped_off_loop/inflight 均为 0,探针连接及时间窗无 WARN/ERROR/FATAL。三项前后审计均只含已授权五源/一库,额外变化 0。之前日志缓冲导致前置等待超时、两套 Go 路径导致启动参数为数组的执行脚本失败原件完整保留,未冒充服务端失败或通过。
- **当前节点 Build 超时停止**:`cpp-prep/current-build-runs/20261018T094748304Z-d6bd67f5/` 的 game Build 被运行器预算终止,native=-1、timedOut=true,日志未见编译错误,RPC 和 A0 未启动,没有成功 manifest。1814 源前后 SHA 相同;21 库仅已实际重编修复的 muduo.lib 更新,其他 20 库不变,最终进程守卫 0。Windows 电源事件证明机器在 09:49:19Z 至次日 01:57:18Z 休眠;旧运行器未记录清醒耗时,不把 16 小时挂钟跨度当作实际构建耗时,也不据此断言超时根因。按 AGENTS.md §10.2 保留原件并停止,独立准备排除休眠的清醒时钟版本,仍保留 game 3 小时/RPC 1 小时上限及全部编译检查,获新决策前不重跑。
- **v2 获批后被启动前源变化拦截**:负责人已直接回答“按 v2 方案继续验收”,候选 E2160B... 实际在新目录 `cpp-prep/current-build-runs/awake-v2-20261019T021620484Z-65828481/` 运行。02 源快照双扫描不一致而退出 1,未超时、未启动 MSBuild 或测试,两进程守卫 0。后续只读与旧基线比较为 1815 源、9 路径变化（3 处注释/日志及 6 处 gate 登录准入相关文件）,不把 old-to-now 列表冒称失败那次双扫描的精确变化集合;失败当时两份 map 未保存。`routing_identity_test.cpp` 又在 02:20:24Z 写入,写者尚待确认。新代码全部保留,原17评审不重跑,A–D/rename源窗未释放;正在请求其它 C++/协议/表代码会话停写状态确认,尚无成功当前节点 manifest。
- **尚未完成**:修复后节点重新链接及 RPC/A0、A–D 与命名迁移后的最终生成器/完整重建、Linux 构建、节点线程/探针/带流量关机及真实玩家资产/battle 链仍待实际执行。旧完整 Rebuild 是修正前基线,不能充当新节点产物证据。最新 Linux 上下文仍为修正前 `linux-prep-20261018T065409Z/freeze-receipt.json`,须在最终源码冻结后重建;隔离 smoke/phase2 已补停机前后请求分窗、gate 线程守卫与直连源 scene 的退出证据判据,仍未构建或真实运行。

### 15.19 代码优先收口（2026-10-18 纽约；编译、测试后置）

负责人最新明确指令为“我要你继续做完代码，不等编译”。该指令取代 §15.18 末尾等待其它会话停写、当前节点 Build/RPC/A0 通过后才写 A–D 的执行顺序；此前失败、通过和源码变化原件全部保留。各文件以最新内容合并、保存前像并校验后写入，不要求全仓停止开发。

- **原第 4、5 批**：当前源码已只读核对 §15.16 七项和 §15.17 收尾；Server/ServerSession/ServerCall、GrpcUnaryCall、Node 两段 GOAWAY、Agones 异步许可、运行时守卫、生成器旧段拒绝均在。最终 GOAWAY 提交后拒派新流、Windows HUP/非阻塞 accept4、大写头夹具及 agones 测试 Windows 包含路径修正均保留，没有新增待实现项。
- **全局应答处理器删除 A–D**：26/16/8/21 个阶段条目已实际写入主工作树，删除适配器及注册点，调用方改为显式 typed 或类型擦除完成回调，模板同步删除全局回退。每阶段前像与实际回执位于 `build/verification/global-handler-repair-20261018T082546455Z/applied-{A,B,C,D}-code-*/applied.json`；不能将旧 Temp 候选或未更新的生成物当作完成证据。
- **并行改动保留**：合并保留 `HandleSessionEndedRelease`、release fence、陈旧 owner 拒绝、新增 `NotifySceneExitGame`/LeaveGame，以及 Login 成功标记、实际发出前准入、`session_incarnation`。静态窄审确认 lane 先更新成功标记再 Pump，直连回包按捕获的代次核会话；这只是源码一致性检查，未冒称运行验证。
- **内部命名**：27 个手写条目已实际应用；两文件改为 `client_session_handler.{h,cpp}`，类名为 `GateClientSessionHandler`，成员为 `clientSessionHandler`。`NotifySceneExitGame` 新调用、登录准入和 A–D 回调均保留，原件及逐文件回执为 `build/verification/gate-session-rename-applied-20261019T025258341449Z/applied.json`。只读独立核对确认全部差异仅为三组命名替换，CMake 留给正式导出器同步。
- **Java 对应**：A–D 的四份源码与 architecture/PARITY 已实际同步，回执为全局删除目录的 `applied-Java-code-20261019T0256Z/applied.json`；ECS 第 6 批另外补齐零冷却唯一写入口、两条测试源码和四份契约/PARITY 说明。上述 Java 新改动均未编译、未测试，受管镜像未手改。
- **正式生成已完成**：导表退出 0，42 张表、10 项服务端部署成功；213 输入与 2220 产物内容前后均无变化。fresh `proto-gen-build`、`proto-gen-run -UseBinary -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml` 和官方 vcxproj2cmake 均退出 0，19 个 CMake 工程无跳过。生成前后 25 文件、173 守护段（131 非空）及两份编号表的全部原有编号/墓碑保留；203 个 typed 与 203 个 Erased 声明已同步，旧全局 API 命中为 0。gate/scene 的源码和工程清单无旧命名或已删适配器引用，未手改生成树、未触碰独立客户端。
- **同步回执**：`build/verification/grpc-muduo-approved-20261018-014019/generator-final-preparation-20261018T092706747Z/final-code-sync-20261019T025522520Z-6a3dd8fa/` 的 `fresh-build-result.json`、`full-generation-native-result.json`、`cmake-export-result.json` 为三步真实结果；`post-full-api-corrected-observation.json` 为静态结果。首份附加观察漏扫子目录并误把方法名字符串当作函数指针，其误报原件 `post-full-static-check.json` 保留，仅更正观察，未重跑成功生成或修改产品断言。
- **验证边界**：本节新增代码未编译、未运行测试。§15.18 的旧完整 Rebuild、专项红绿、gRPC/Agones 普通与重复轮、battle/router/互通结果仅证明各自当时输入。最终当前节点、独立 RPC/A0、生成器与各语言测试、Linux、线程/关机/真实业务冒烟仍属后置验证；没有新的成功当前构建 manifest。

### 15.20 函数与变量命名收尾（2026-10-18 纽约；业务编译、测试后置）

负责人追加要求“把剩下的函数、变量命名优化也做完，生成器和生成产物同步好，不用等编译，做完给我”。本次沿既有 gRPC 回调迁移收尾，只调整内部名称与对应引用、断言及说明，不扩展功能或修改公开 Send / GrpcDone / Erased 契约。

- **12 个手写文件已实际写入**：gate 会话处理器及桥接头、scene lifecycle 头/实现与 id_segment_bootstrap、生成器 Go 源码及两个模板和相应断言、两份当前设计入口说明与全局处理器删除交接清单。以当前 HEAD `8b9592de3` 的工作树为前像逐文件核对后写入，保留其它会话的新逻辑；实际回执为 `build/verification/global-handler-repair-20261018T082546455Z/applied-callback-naming-20261019T0322Z/applied.json`。
- **构造与声明用途明确**：`LocalFailure` 改为 `MakeLocalFailureOutcome`；`SenderFunctions`、局部收集列表及去重表改用 `SenderDeclarations` / `senderDeclarations` / `senderDeclarationSet`，模板字段同步；模板内 `rpc` 改为 `unaryRequest`，对应生成器断言同步。
- **请求及完成结果明确**：gate 私有完成函数改为 `ForwardSceneClientCallResult` / `ForwardDirectGrpcCallResult`；序列化会话值改为 `serializedSessionDetails`；scene 的请求标记改为 `requestCorrelationId`、gRPC 状态值改为 `grpcStatusCode`；号段申请局部序号改为 `requestSequence`。声明、定义、调用、捕获和注释一起更新。
- **行为边界**：控制流和日志字面量保留，Login 准入、session incarnation、session-ended 退出凭据与 release fence 均保留。仅静态核对名称和引用一致性，本轮业务工程未编译、未运行测试；Java 功能、客户端契约和编号规则不变。
- **最后一处关联号参数已补齐**：私有 `ClaimRelocateTransportFailure` 的参数及三处使用统一为 `requestCorrelationId`，旧 `requestTag` 在 lifecycle 头/实现中无残留。该项仅调整普通 C++ 参数名，不影响生成输入，未重跑已成功的生成。实际追加回执为同证据目录的 `applied-callback-naming-claim-final-20261019T0330Z/applied.json`，最终源码 SHA 为 `08935447e5cb3b30bbd822f868188bcca24e7e3d292391151d251160b788daa7`。
- **生成同步已完成**：按新源码执行 `proto-gen-build`，再用新工具执行 `proto-gen-run -UseBinary` 与绝对路径 no-unity 配置，两步均退出 0；工具 SHA 为 `28f7499b20231c293dd2cdda586c6716d06bc18e80d9109ca0c1a05cecaed315`。25 份客户端生成 cpp 仅出现预期标识符替换，注册表输出逐字节相同；25 文件中 131 个非空守护段、314 消息编号、50 事件编号及全部墓碑保留，工具输入无意外变化、指定旧名称无残留。未手改生成目录，未重新导表或业务编译；文件/工程清单未变，本轮不重复 CMake 导出。真实回执为 `build/verification/global-handler-repair-20261018T082546455Z/callback-naming-sync-20261019T0325Z/` 的 `generator-build-result.json`、`full-generation-result.json` 与 `final-static-result.json`。

### 15.21 当前协议与下游副本同步收口（2026-10-19 纽约；代码范围已完成）

- **晚到字段已正式同步**：命名收尾之后补入的 `ReleasePlayerRequest.covered_session_id = 6`、`covered_login_generation = 7` 已进入 C++ 和 Go 绑定。按顺序执行 fresh `proto-gen-build`、绝对 no-unity 配置的 `proto-gen-run -UseBinary`，原生均退出 0；生成期间 234 项输入不变，25 个 handler 文件中的 131 个非空守护段、原有编号与墓碑保留。未手改生成树、未触碰独立客户端。
- **命名补漏无新增缺口**：25 份生成客户端使用 `MakeLocalFailureOutcome`，203 处 `unaryRequest` 序列化与发送逐文件对应；`SenderDeclarations`、模板断言、gate 完成转发函数及 `ClaimRelocateTransportFailure` 的 `requestCorrelationId` 一致，指定旧名称无活动引用。此项是静态核对，不替代运行验证。
- **机器人副本已正规同步**：按 `robot/README.md` 先执行 `go mod vendor -o <本轮证据目录>/robot-vendor-preview`，再执行正式 `go mod vendor`，两步原生均退出 0。1299 个正式 vendor 文件与预览完全一致；仅 `shared/generated/pb/table/login_error_tip.pb.go` 一份过时副本更新，与 `go/shared` 源一致，`robot/go.mod`、`go.sum` 未变。
- **后续并行变更如实保留**：最终严格全文件比较发现 `tools/scripts/dev_tools.ps1` 在生成之后被其它会话追加 K8s 登录参数透传；其余 233 项协议/生成器输入不变，三个生成入口函数与生成前逐字相同。严格比较失败原件保留在 `final-code-generation-result.json`，分类核对见 `code-generation-closure-observation.json`，未重跑成功生成。vendor 与另一会话已经启动的 Windows C++ 构建有时间重叠，仅修改 robot/vendor，不涉及 C++ 输入，不宣称全部 native 串行。
- **实际原件与交付边界**：本轮目录为 `build/verification/grpc-muduo-codex-20261018-005135/release-fields-sync-20261019043741/`，含四步 `*-result.json`、`static-result.json`、vendor 差异和最终分类记录。根会话的代码、命名及生成同步任务已完成；本轮未执行业务编译或测试，不等待独立验证会话的构建，不将其运行中状态记为通过。此前失败和验证历史均保留，未提交。

### 15.22 独立构建发现的日志类型兼容问题（2026-10-19 纽约；新增待修）

§15.21 的代码生成收口之后，独立验证会话在 `grpc_client.vcxproj` 实际编译到 `battle_node_grpc_client.cpp:94` 时发现 C2679：protobuf 的 `Descriptor::full_name()` 返回 `absl::string_view`，不能直接写入 `muduo::LogStream`。权威来源为 `grpc_async_client.cpp.tmpl:99`。验证执行者保留首错及日志，核对进程身份后停止自有失败构建，native=-1、非超时；RPC、后续测试和 Linux 未启动。1812 项 C++ 输入前后相同，没有成功构建 manifest。原件为 `build/verification/grpc-muduo-codex-20261018-005135/cpp-prep/current-build-runs/v3-20261019T044004493Z/` 的 `result.json`、`game-first-error-50.txt`。

同目录已准备 `grpc-client-log-view-fix.patch`（SHA `a3d1e743c39cab349a926850ae79f260cbe8ee0cfe22c55f17c1bf834ddb74e1`）：只在模板中取 `requestTypeName`，用 `std::string(data, size)` 写日志，保留日志文字、错误回调和调用控制流；正规生成覆盖 25 cpp / 203 站点，不手改产物。根会话只读复核该候选，未应用、未重试。按 AGENTS.md §10.2，唯一修复 owner 的“执行 C++ 与生成器验证”会话已向负责人请求本次具体修复/续验决定，尚待回答，不重复提问。此前生成和 vendor 成功仍是真实记录，但当前代码交付新增这一项兼容修正，不能再将“代码范围已完成”解释为没有待修代码。

### 15.23 C2679 最小修正与正式生成完成（2026-10-19 纽约；业务编译后置）

负责人已在本聊天明确授权“应用最小补丁修掉 C2679，重新构建生成器并同步生成产物，不用等业务编译，做完给我”。统一主协调在唯一执行窗口实际应用 §15.22 的原候选，模板 SHA 从 `0e73226f...` 变为 `90ee601c293fb1753ba4e3f03015279d3636a8b34c1d58be3a690cd73920aa97`：只取 `requestTypeName`，再以 `std::string(data, size)` 写入日志流。日志文案、类型拒绝、错误码、回调与控制流不变，原命名及并行新代码保留。

- **生成器已重建**：`proto-gen-build` 实际退出 0，新鲜工具 SHA 为 `28f7499b20231c293dd2cdda586c6716d06bc18e80d9109ca0c1a05cecaed315`。模板由工具运行时读取，因此本次模板修改不要求工具二进制 SHA 改变。
- **正式生成已完成**：首次包装调用将程序路径误作额外参数，进入生成前退出 1，原件 `full-generation-result.json` 保留；执行者改用同一个 fresh 正规 CLI 的零额外参数启动方式，并显式使用绝对 no-unity 配置，`direct-full-generation-result.json` 实际退出 0。没有修改生成器参数校验或手写生成产物。生成期间 234 项输入、25 handler 文件内 131 个非空守护段及编号/墓碑保留，ReleasePlayer 字段 6/7 的 C++/Go 绑定保持原 SHA。
- **最终产物逐字核对**：根会话用独立保存的前像计算预期两行日志转换，与实际 25 份客户端 cpp 逐字节比对，203 个站点全部一致、额外差异 0；旧直接输出视图的站点已清零。文件/工程清单没有变化，不重复导表、CMake 导出或 vendor。
- **证据与边界**：实际执行原件在 `build/verification/c2679-root-takeover-20261019T061821334Z/`；根独立核对在 `build/verification/grpc-muduo-codex-20261018-005135/c2679-code-delivery-20261019T061726797Z/root-generated-diff-audit.json`。本次要求的最小代码修正与生成同步已完成，原 C2679 失败不改写为通过；本聊天未执行业务编译或测试，也不等待独立验收的后续构建。Java 不适用：仅 C++ 内部生成客户端的日志重载适配，未改变功能或共享客户端契约。未提交、未推送。

### 合并保留：2026-10-15 手写调用方适配历史

本段是早期第三批实施记录；全局处理器、CQ 以及旧公共类型均已由 §15 的现役实现取代，不作为当前接口或构建结论。

#### 原第三批适配说明

§14.1 落地时只改了模板与 core,手写调用方仍引用已删除的符号,重生成后编不过。本节补齐,**未编译、未测试**,
当前验证范围见 [§16.6](#grpc-client-validation)，本段保留当时的适配说明。

- **应答 / 失败处理器签名**:所有 `SetIfEmptyHandler` / `SetHandler` lambda 改收 `const GrpcReplyContext&`,
  失败侧一律用 `failure.code()` / `failure.message()`(gate 失败桥、号段、收件箱、scene_manager 应答)。
  `GetSessionDetailsByClientContext(const grpc::ClientContext&)` 改名为 `GetSessionDetailsByReplyContext(const GrpcReplyContext&)`
  (`session.{h,cpp}`),按 `initialMetadata` 找 `x-session-detail-bin`,`-bin` 层语义与旧 `GetServerInitialMetadata` 相同。
  号段的 `x-idseg-seq` 改用 `GrpcCallOutcome::FindInitialMetadata`。
- **选点**:`node_utils.{h,cpp}`(`GetSceneManagerEntity` 分级)、`id_segment_bootstrap.cpp`、`player_inbox_system.cpp`
  改按 `MuduoGrpcChannelPtr` + `muduo_grpc::ConnectivityState`,`GRPC_CHANNEL_*` 与新枚举一一对应,分级规则不变。
- **gate 路由**:删 `gate_router_muduo::InstallChannelFactory`(工厂已删,通道由 `NodeConnector` 建);
  `SendRawViaRouter` 去掉官方 CQ / Stub 回退,没挂通道按「没有路由服」fail-closed(返回 nullopt,lane 回 1003);
  `GATE_ROUTER_TRANSPORT=grpc` / 不认识的值打 WARN 后照旧走 muduo_h2(与 `NODE_ETCD_TRANSPORT` 同一处理)。
- **gate 关机排空门(行为变化)**:以前只在 `GATE_ROUTER_TRANSPORT=muduo_h2` 时装。CQ 删掉后若仍按开关判,
  默认部署(开关未设 = 旧默认 grpc)会不装这道门,`FinalizeShutdownInLoop` 同步清掉通道,被 forceClose 的会话下一轮才发
  `Login.Disconnect`,通知全部发不出去。现在两种拓扑都装:等 beforeShutdown 记下的会话都走完断线处理、**所有** node registry
  上的 gRPC 通道都没有在途调用,再放行 Finalize;上限仍是节点的 15s 排空预算(gate 调用 deadline 最长 12s)。
  直连模式(默认)因此关机时要等在途调用结束,换来断线通知能送达;关机期间若一直有新请求进来,会等满 15s 预算
  (k8s 宽限 30s 覆盖得住)。`gate_router_muduo::AllChannelsIdle` 并入 gate main 的 `AllGrpcChannelsIdle`。
  gate 的 `PickRandomNode`(各 gRPC 节点类型通用)改为只看 `HasUsableGrpcClient`(挂着通道且非 SHUTDOWN),`SendRawViaRouter` 的无通道分支是防御性守卫。
- **测试**:`scene_manager_selector_test` 换新枚举;`cross_zone_test` 的假 scene_manager 节点自带 EventLoop 建真通道
  (端口 1,不连);`grpc_client_lifecycle_test` 按新运行时改写,删掉 CQ 时代与 etcd 流用例(流由 `muduo_grpc_test` 覆盖)。
  其中一条成功用例的对端仍是测试内的官方 C++ gRPC 服务端桩,第 5 批删官方运行时时要换成 muduo_grpc 服务端或回环桩。
- **链接项**:本批改了源码的三个测试工程(`rpc_controller_test`、`cross_zone_test`、`routing_identity_test`)的 Debug|x64
  已补 `muduo_grpc.lib;nghttp2.lib` 与 nghttp2 库目录(照 gate.vcxproj)。其余链 core / grpc_client 的测试工程(如 `bag_test`)
  仍未补,按 §14.1 归第 5 批;在那之前编它们若报 LNK2019,按报错补同样两项即可。

<a id="grpc-client-contract"></a>

## 16. C++ 出站客户端调用契约（2026-10-23 按当前源码整理）

本节统一生成客户端的接口、截止预算、部署门禁及业务完成归属。§8、§14 与第三批适配说明保留实施历史；当前调用规则以本节和源码为准，不重新启用全局处理器、官方 CQ 或旧重载。配置值只反映本次核对的仓库输入，后续修改须同步核对配置、调用方与门禁，不以本节替代实际验证记录。

<a id="grpc-client-callback"></a>

### 16.1 接口与生命周期

- 手写公共头 `cpp/libs/engine/core/network/grpc_client_call.h` 是 `GrpcCallOutcome`、`GrpcDone<Resp>`、`GrpcMessageDone`、`GrpcCallOptions` 和 metadata 的权威定义；生成模板引用它，不生成第二份类型。模板位于 `tools/proto_generator/protogen/internal/template/grpc_async_client.{h,cpp}.tmpl`。
- 每个 unary 方法只有显式传入 `GrpcDone<Resp>` 的 typed 入口和供通用 sender 使用的 `Erased` 入口，两者汇入同一发送路径。`done` 必须非空，空值是调用方错误并触发 `LOG_FATAL`；没有全局应答 / 失败处理器、安装函数或无 done 的兼容重载。流式方法不生成这些入口，etcd Watch / LeaseKeepAlive 由手写 `MuduoEtcdTransport` 维护。
- 完成只经本次 `done`，恰好一次、只在所属 EventLoop 上、从不在 `Send` 栈内同步执行；缺通道、序列化失败、Erased 类型不符等本地拒绝也经 `QueueGrpcLocalCompletion` 延后。Erased 同时检查 protobuf descriptor 与真实 C++ 类型，不能把同 descriptor 的 `DynamicMessage` 强转为生成类型。
- 成功时响应指针非空且只在回调期间有效，需要留用必须拷贝；非 OK 时响应指针为空。`GrpcCallOutcome` 保存状态及收到的 initial / trailing metadata。`GrpcCallOptions.metadata` 填应用层编码前的原值，发送时按 §4.2 编码；`deadline > 0` 覆盖本次调用，否则使用该目标类型的默认配置。
- 请求在建立任何异步闭包前同步序列化，gate 的共享请求原型随后可被下一次调用覆盖。回调按值捕获本次业务上下文，不保留 request 的引用或指针，不长期捕获 `TcpConnectionPtr`；若将来延后序列化，必须先取得独立请求副本。
- 摘除节点时，销毁通道将未完成调用以 `UNAVAILABLE` 排队完成，回调可能在节点实体销毁后才执行，不能依赖实体、组件或连接仍存在。停机先清通道并排队完成，再拆 ECS / Redis / 日志；拆除还须等待 `GrpcClientLocalCompletionsDrained()`，顺序见 §6 与 §15.10。

<a id="grpc-client-outcome"></a>

### 16.2 失败表示结果未知

`DEADLINE_EXCEEDED`、`UNAVAILABLE`、`CANCELLED` 都不能证明对端没有执行，超时或断线后服务端仍可能提交。完成方只能释放本次本地在途状态、提示客户端，或按业务幂等契约做有界退避重试；不能把传输失败当业务拒绝去改权威状态。交接去留继续由 owner_epoch、权威查询与既有看门狗裁决，不因传输失败立即解冻或提前核实。

调用方不按状态码臆断“停机”与“传输故障”：通道实现变化可能改变返回码。生成客户端不添加重试；传输内部只允许 §4.6 中可证明未处理的透明重试。

<a id="grpc-client-deadline"></a>

### 16.3 配置与预算

配置真源为 `proto/common/base/config.proto` 的 `BaseDeployConfig.grpc_client = 21`（`GrpcClientConfig.call_deadline_ms`），部署值在 `bin/etc/base_deploy_config.yaml` 的 `GrpcClient.CallDeadlineMs`，键为目标 `eNodeType` 名称。未配置的类型使用 `kDefaultGrpcCallDeadlineMs = 10000`；未知名称或值为 0 时记 ERROR 并忽略该项，保留默认值。

`Node::Initialize` 在 `LoadConfigs()` 后、`InitEtcdService()` 前调用 `grpc_call_deadline::Apply`，将值写入生成客户端各文件的原子默认值，并留存供 `grpc_call_deadline::Get(nodeType)` 查询。业务调用的截止以 `steady_clock` 计算。

预算同时满足两条约束：C++ deadline 至少比目标 Go 服务端 Timeout 多 2000ms；依赖结果的本地在途 TTL / 看门狗又须比该 deadline 宽，并从 `Get` 派生。方法级 `MethodTimeouts` 也要核对，不能只看服务顶层 Timeout。

| 目标节点类型 | C++ deadline（ms） | 当前 Go 顶层 Timeout（ms） | 本地派生预算 |
|---|---:|---:|---|
| SceneManagerNodeService | 10000 | 8000 | 普通换图 / 镜像在途 TTL = deadline + 1000ms；交接另有原权威裁决预算 |
| DataServiceNodeService | 4000 | 未写，默认 2000 | 号段 fetchTimeout = deadline + 1s |
| ClientRpcRouterNodeService | 8000 | 6000 | 路由内部预算另见 [请求顺序契约](client-request-ordering.md) |
| MatchNodeService | 7000 | 4000 | gate 直连模式 |
| LoginNodeService | 12000 | 4000 | gate 直连模式 |
| PlayerInboxNodeService | 6000 | 3000 | Fetch / Ack 在途兜底 = deadline + 5000ms |
| BattleNodeService | 5000 | 不适用 | C++ 服务端截止见 §15 |
| EtcdNodeService | 5000 | 无对应配置 | 仍须小于 etcd 的 10s Txn 看门狗 / LeaseGrant 重试预算 |

服务端值分别核自 `go/{scene_manager,data_service,client_rpc_router,match,login,player_inbox}/etc/` 的服务 YAML。不要沿用迁移初期 login=10000、match=5000 的服务端旧值，也不要将跨服务后台总预算混作单次 unary 超时。

<a id="grpc-client-deploy-budget"></a>

### 16.4 部署门禁

`tools/scripts/k8s_deploy.ps1` 的 `Assert-GrpcClientDeadlineBudget` 在 zone-up / infra-up / all-up 的集群写入前核对上表六个 Go 目标。六项 C++ deadline 必须显式为正整数；服务顶层 Timeout 缺席按 2000ms，写出后必须为正整数。任一预算不成立即拒绝部署，down / status 不经此门禁。node 与 battle-node ConfigMap 从 `bin/etc` 原样镜像整个 `GrpcClient` 块，缺块在生成期拒绝；Go ConfigMap 的服务端超时必须与其服务 YAML 一致。

`Get-ZrpcMethodTimeouts` 逐条解析方法超时，`Get-GrpcClientDeadlineBudgetViolations` 同时核对顶层和方法预算。唯一免于与 C++ deadline 比大小的例外，由 `Get-GrpcClientMethodsNotCalledFromCpp` 显式登记：当前仅 DataService 的 `/data_service.DataService/RollbackPlayer`、`RollbackZone`、`RollbackAll`。例外仍须有合法方法名、可解析时长和对应 YAML 条目；data-service ConfigMap 现已镜像这些 MethodTimeouts，旧版“不镜像”的限制已失效。

登记“不由 C++ 调用”必须同时排除手写代码的 typed / Erased / 消息号调用，以及 gate 根据生成 `IsClientMessageId` 的直连转发。契约测试还拒绝已不在 YAML 中的过期登记；部署脚本自身不扫描 C++。当前 deploy-config-tests 的路径触发不覆盖仅 C++ 的改动，发版流程必跑；增加调用方时须主动运行相应契约测试，不能等部署入口发现。

解析只支持顶层不带引号的 `MethodTimeouts` 块式列表或显式 `[]`；每项仅含 `FullMethod` / `Timeout`。重复、缺键、未知键、无法辨认的写法、异常换行及两种解析的条数不一致均拒绝。时长仅接受正数 ms / s / m / h 的子集，不支持符号、`.5s` 或 `5.s`。这是防配置漂移的文本门禁，不是完整 YAML 校验器；转义拼接键名、顶层 Timeout 的合并键 / 多文档等仍有解析边界，不据此宣称完整语义覆盖。

<a id="grpc-client-call-sites"></a>

### 16.5 调用点与完成归属

| 调用 | 认领与校验 | 失败、迟到与客户端行为 |
|---|---|---|
| EnterScene | `SendCorrelatedEnterScene` 捕获 playerId + 非零关联号，交 `DispatchEnterSceneReply` / `DispatchEnterSceneTransportFailure`；响应非零 player_id / correlation_id 逐字段核对，不符记 ERROR、计 unmatched 后丢弃，零回显仍按捕获值认领 | 普通换图释放本次槽，玩家主动请求回 `kEnterSceneServerBusy`（3028），跟随只记日志；交接仍按 §16.2 处理。先分发再记录完成日志，不能按 player_id 误认另一请求 |
| 镜像 CreateScene | `HandleMirrorCreateDone` 按捕获 playerId + token 认领；只认当前 token，随后要求 `now >= sentAtMs && now - sentAtMs < TTL` | token 不符不动新槽；匹配但过期或完成时检测到时间回拨，只摘自己的槽并静默丢弃，不推 tip、不自动进场。有效槽内业务拒绝、零 scene_id 或已进战斗回 `kEnterSceneFailed`，传输失败 / 无可用 SceneManager 回 3028 |
| AllocateIdSegment | `IdSegmentTransport` 捕获 seq 认领 inflight / abandoned，不依赖 `x-idseg-seq` 响应 metadata | 保留单飞和迟到窗口；正常传输失败交 `OnTransportFailure`，已放弃请求不重复判失败 |
| 收件箱 Fetch / Ack | 捕获 playerId + domain + correlationId 及 confirmed admission；先核当前归属身份仍一致，再进入具名完成函数；非零回显逐字段核对后按捕获值认领 | 旧完成不清新在途；保留原退避、UNIMPLEMENTED 与 Ack 幂等语义，失败不阻断进场 |
| gate 直连通用转发 | 捕获 session_id / incarnation、发送时 SessionDetails 原始字节及原请求 id / message_id；成功失败都先核对会话代次 | 拒绝 session_id 复用后的旧完成；成功不依赖服务端回显，成功包装和失败 1003 信封均携带原请求 id / message_id；失败仍走既有桥和 lane 认领 |
| gate Login.Disconnect | 显式 typed done，只捕获 sessionId | 成功无后续；失败每 1024 次采样 WARN，不向已断开或复用该 session_id 的新会话推 tip |
| gate 路由 Forward | `router_muduo_forward.cpp` 的 `SendForward`；成功按响应头回显的原值会话交桥，失败按发送时捕获会话交失败桥 | 保留路由 lane 的队头、代次和信封契约，不自动重试；不能把直连“不依赖响应会话回显”套到 Forward |

镜像建场在途也占 `IsSceneChangeBusy`；客户端换图、队伍跟随与跨 zone 传送不能绕过。普通换图登记、交接全部预检通过、退出新挂 `UnregisterPlayer`、权威落点成功这四个提交点主动撤销旧镜像令牌，永久取消旧提示 / 自动进场资格；被撤销的旧请求即使在 UTC 回拨后恢复到原 TTL 内也不能复活。只读忙碌检查不删除组件，交接预检失败不取消仍有效的旧镜像。详情见 [场景创建架构](scene-creation-architecture.md) 与 [组件访问规则](ecs-component-access-rules.md)。

<a id="grpc-client-validation"></a>

### 16.6 验证范围与证据边界

修改生成规则时，按 AGENTS.md §4 先构建生成器、运行受影响的 Go 生成器测试，再用 no-unity 配置正规重生成，不手改产物。受影响 C++ 工程按 §15.14 串行构建（MSBuild `/m:1`），运行相应行为回归；配置与部署门禁变化还须运行 `tools/scripts/tests/k8s_deploy_contract.tests.ps1`。仅更新文档引用和注释无需重生成协议或重跑业务构建。

验证重点：typed / Erased 的唯一完成通道与同步序列化；本地拒绝异步恰好一次及排空；EnterScene 非零不一致和零回显认领；镜像过期、回拨、撤销和新令牌隔离；Inbox 归属变化与旧完成隔离；号段失败及迟到窗口；gate 会话复用和原请求 id 的成功 / 失败信封。已有本地拒绝用例在 `cpp/tests/rpc_controller_test/grpc_client_local_reject_test.cpp`，业务回归分布在 cross_zone / bag / currency / routing_identity 测试工程，具体结果以执行回执为准。

启动生效值日志 `[GrpcClient] unary call deadlines (ms)` 与正常排空完成日志均为 INFO；muduo 的 `LogLevel: 2` 即 INFO，可以看到，无需为此降低日志级别。缺 done 会触发 `requires a done callback`，不再检查已删除的全局处理器漏装日志。

本次合并仅依据当前源码和配置整理契约，不代表编译、单测、Linux、联机或压测已通过；历史未验计划和当时故障数量不作为现状。Java 内部传输不由本节约束，本次没有功能或共享客户端契约变更，无需同步 Java 实现或登记 PARITY。

## 17. 官方 C++ SDK 移除与独立依赖（2026-10-26）

C++ 控制面继续使用标准 gRPC 协议，传输由 `muduo_grpc` / nghttp2 实现，序列化由独立 Protobuf 实现。官方 gRPC 仓库不再是源码、构建、链接、生成或工具来源；Go gRPC 生成和服务实现保持现状。

### 17.1 独立源码与安装契约

四个子模块的精确提交由 `third_party/releases.lock.json` 固定，沿用拆分前版本，不在本次迁移升级：

| 源码目录 | 固定提交 | 职责 |
|---|---|---|
| `third_party/protobuf` | `35cd01f9fe9afbeea38cc7b979a3b6bfcde82c03` | 消息运行库、protoc、自带 utf8_range；libprotoc 依赖其内部 libupb |
| `third_party/abseil-cpp` | `76bb24329e8bf5f39704eb10d21b9a80befa7c81` | Protobuf 与业务代码共用的基础类型及日志 |
| `third_party/googletest` | `52eb8108c5bdec04579160ae17225d66034bd723` | C++ 测试框架 |
| `third_party/boringssl` | `2b44a3701a4788e1ef866ddc7f143060a3d196c9` | 已有 TLS 消费者，保留登记的 Windows X509_NAME 补丁 |

Windows 的 Protobuf / Abseil / utf8 共用 Protobuf 安装前缀，BoringSSL 与 GoogleTest 各用自身前缀。x64 Debug 为 `install_vs2026_dbg`（`/MDd`），x64 Release 为 `install_vs2026_rel`（`/MD`）；x86 对应 `install_vs2026_x86_dbg/rel`，不混用架构或 CRT。工程中的独立 SDK 目录优先于根 `lib/`，旧归档不得兜底。具体入口及 zlib 前置见 `tools/scripts/README.md` 的“独立 C++ 依赖构建”。构建只使用已有工具；本地化 MSVC 缺少英文诊断包时选择已有 VS 生成器，避免 Ninja 依赖前缀转码失真。

Linux 安装契约固定为 `/usr/local`：先构建固定 zlib，再由 `build_linux_sdk.sh` 依次构建独立 Abseil、Protobuf、BoringSSL。Protobuf 禁止 FetchContent 网络兜底。来源摘要包含源码、构建配方、上游依赖和安装产物，缺失或变化时重建。curl / librdkafka 明确使用同一 BoringSSL 头和归档，节点通过独立导出目标链接，不回退系统 OpenSSL。Docker 的 COPY 来自主仓子模块，不在镜像内拉取官方 gRPC。

### 17.2 生成与依赖边界

- 删除官方初始化探针及其守卫、环境开关、日志字段；正常 ThreadMonitor 继续生效。
- 生成器不再查找或运行官方 C++ 插件；普通 `.pb.*`、自有 `*_grpc_client.*` 与 muduo 服务骨架照常生成。服务 metadata 统一引用普通 `.pb.h`。
- 正规同步只删除受管 proto 根下、带官方生成标记的 `.grpc.pb.cc/.h`；先校验清单、目录链接、条件和自定义编译元数据，再写入。单文件采用既有原子替换，整批不宣称跨文件事务；中途 I/O 失败显式报错并保留现场。
- mail/rank 后处理与生成工程同步均不再登记官方桩。权威 CMake 导出器与静态契约拒绝重新引入官方 SDK 路径、库和产物，保留自有 `grpc_client` / `muduo_grpc`。
- 调试 SDK 来源清单使用 schema 2，完整核对独立依赖和现役构建配方；旧 schema 1 安装不能冒充新的独立 SDK。

### 17.3 实际验证与边界

本轮证据统一保存在 `build/verification/official-grpc-removal-20261026/`，首次失败原件保留，后继成功不改写原结果：

- 独立 Protobuf / Abseil、BoringSSL、GoogleTest 的 Windows Debug x64 构建安装实际退出 0，protoc 为 35.1。首次 Protobuf 构建误关其内部 libupb 的失败已修正并重验。
- 生成器完整 Go 回归、mail/rank 10 项回归及依赖边界 8 项检查通过。123 份协议源及消息号 / 事件号不变，137 个非空手写守护段核对一致；82 个官方桩由正规生成清除，二次生成 697 个 C++ 产物逐字节不变。
- 标准 `game.sln` Debug x64 串行 Build 实际退出 0，三节点均链接成功，1620 个源码与工程输入前后相同，适用 NoRaw 检查通过。原官方目录与根 `lib/` 的旧 SDK 归档在构建前已移至 ignored 备份，不能作为本轮构建兜底。该结果不称 clean Rebuild。
- `proto.lib` 成员没有官方桩对象，三节点直接 DLL 导入没有官方 gRPC；这项只读检查不宣称递归 DLL 闭包。
- 当前工作树的 8 项 SDK 边界和限定生成器三组回归再次通过，126 项生成器输入前后相同。四个独立子模块的远端与登记一致，无 alternates 或 gitdir 借用旧 gRPC 存储。
- Linux 正式 Dockerfile 的 `--target deps` 已实际构建成功，38,191 项依赖输入完整封存并在构建前后复核一致，镜像为 `local/xuanming-independent-sdk:grpc-removal-r2`（`sha256:4a84f833d9f882e7abf1916ad4f6bf5de86371b31f5e97347dc25bbf8cd08658`）。首次 r1 在新合入 Hiredis overlay 的同名参数处触发 `-Werror=shadow`，仅改参数名后 r2 通过；首红原件与上下文保留，不关闭警告检查。
- 新 Linux 镜像的一次性无网络消费者实际通过：独立 protoc 35.1 生成消息，CMake 使用安装导出目标编译运行 Protobuf 往返及 BoringSSL / nghttp2 链接；三项 SDK 的安装摘要及二次复用检查通过，动态导入无官方 gRPC、无缺失库。完整依赖层同时编译了 muduo、curl、librdkafka、yaml-cpp、nghttp2，zlib 上游 18 项测试通过。证据为 `linux-deps-r2/build-result.json` 与 `installed-sdk-result.json`；这不替代节点或业务回归。
- 本机调试 SDK 清单已从 schema 1 原子切换为 schema 2，原件逐字节备份，现役 `validate_sdk()` 再次通过。来源守卫覆盖 25,298 项一般依赖输入与三独立仓的 12,897 项完整文件：实际镜像输入为 38,191 项，另 4 项为固定提交中未参与本次构建的上游安装测试文件（Protobuf 的 `csharp/install_dotnet_sdk.ps1`，Abseil 的 `CMake/install_test_project/` 三文件）。它们因封存脚本过宽的 `install_*` 排除规则未进入镜像，已记录 Git blob、实际字节和未消费证据并额外守卫，未将其冒称已编译输入；后继封存规则已限定为依赖根安装目录。资格与切换记录位于 `sdk-schema2-candidate-r2/`，验证器本身未放宽。
- 运行回归、四个方案外测试工程补编、Go 互通及 Linux 三节点构建尚待接续。Windows Release / Win32 只有配置清理证据，不宣称已编译；后继实际追加本节或进度记录，不据 Windows Debug 或 Linux 依赖层成功扩大范围。

本次属于 C++ 内部依赖整理，没有功能或共享客户端契约变化，Java 无对应 SDK 移除任务，独立客户端未读取或修改。

2026-10-26 后续协作说明：上述 Windows 成功后，共享主仓合入了另一批架构源码。原构建证据仅绑定原 1620 项输入；当前新主线由其协调聊天先正规生成、构建，本任务的运行回归和 Linux 三节点验证须基于完整新来源接续，不沿用旧成功冒充新主线验收。Linux 依赖层已在独立快照中完成，未消费该主线中途生成的应用产物。

2026-10-27 备份清理：负责人明确要求不保留旧 gRPC 备份后，已物理删除旧源码及构建产物备份、127 个旧库归档和闲置的 `.git/modules/third_party/grpc`，并清除本地过期子模块配置。删除后目标路径及登记均不存在，四个独立依赖的关键文件和 Git HEAD/config 保持原字节；只保留删除证明及既有验证日志。首次两次文件占用中断原件与最终成功记录均在上述证据目录，不代表应用剩余验收已完成。

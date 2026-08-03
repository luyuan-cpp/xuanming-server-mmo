# Scene Node 线程模型决策

**日期**: 2025-04-05
**最近修订**: 2026-08-02

## 现状：业务逻辑单线程，进程并非单线程

Scene Node 的线程安全边界是单个 muduo `EventLoop`，不是“整个进程只有一条线程”。

必须在 EventLoop 上执行的内容包括：

- ECS、玩家、场景等 gameplay 状态变更；
- `World::Update()` 逻辑 tick（33ms 帧间隔）；
- muduo RPC handler 的业务部分；
- Kafka 后台 consumer 投递回来的业务回调；
- gRPC sync handler 通过 `runInLoop` 投递的 `Handle*` 业务部分。

进程中同时存在非业务线程：

- gRPC sync server poller；当前 `GRPC_SERVER_MAX_POLLERS` 默认上限为 8；
- gRPC v1.83 默认 EventEngine 线程池；
- Kafka/librdkafka、异步日志和 Agones lifecycle 等后台线程；
- 关机期间临时运行 `grpc::Server::Shutdown()` 的 worker。

gRPC v1.83 默认 EventEngine 会 eager 启动
`Clamp(gpr_cpu_num_cores(), 4, 16)` 条 reserve worker，再启动 1 条
Lifeguard。该池还能按任务积压扩展，16 不是硬上限。gRPC 没有公开的
reserve/max 环境变量来调整这个内置线程池。

入口为 `RunNodeMain`，最终由 `loop.loop()` 驱动业务主线程。

## 决策：现阶段保持业务状态单线程归属

### 理由

1. **无锁 ECS**：所有 gameplay 状态变更回到同一 EventLoop，访问 entt registry 无需跨线程加锁。
2. **连接数少**：Scene Node 对端是内部服务（Gate、SceneManager），不是客户端，业务侧无需为连接数引入并行状态修改。
3. **Kafka 消息轻量**：后台 consumer 只负责收取并投递，实际 control message 处理仍回到 EventLoop。
4. **水平扩容**：通过多个 Scene Node 实例分担不同场景/地图，而不是先把单个 ECS 拆成多线程共享状态。
5. **调试简单**：业务状态变更顺序由 EventLoop 串行化，竞态边界清晰。

“业务逻辑单线程”不表示 transport handler 也在 EventLoop 上阻塞执行。
gRPC sync handler 运行在自己的 poller 上，只把业务部分投递到 EventLoop，
随后用 promise/future 等待结果。gRPC 线程不得直接读写 EventLoop 所有的
ECS 或节点业务状态。

### 对比：Gate Node

Gate 面对大量客户端 TCP 连接，可以使用 `EventLoopThreadPool` 扩展 I/O
处理；这不改变业务状态必须按其明确所有权串行化或同步的原则。

## gRPC 关机约束

`grpc::Server::Shutdown(deadline)` 对 sync server 是阻塞 join 点。deadline
到期会取消传输层调用，但不会强制终止已经进入的 C++ sync handler，因此
deadline 不是 handler 执行时间的硬上限。

Scene gRPC handler 会等待 EventLoop 执行 `runInLoop` 任务，所以不得在
EventLoop 上同步调用 `Shutdown()`。关机状态机必须是：

```text
Running -> ClosingIngress -> (GrpcDraining || PersistenceDraining) -> Finalizing -> Done
```

1. EventLoop 先停止并 join Kafka consumer，封住 Kafka 入站；producer 继续
   存活，供玩家停机存盘发送 DBTask。随后执行关机前业务 hook。
2. 临时 worker 立即调用 `grpc::Server::Shutdown()` 封住 gRPC 入站；同时
   EventLoop 每 100ms 现场检查业务 barrier：玩家实体为空、退出存盘均已收到
   Redis ACK、Kafka producer 队列已真正 flush。两条 drain 并行推进。
3. 只有 gRPC worker 已返回，并且业务 barrier 当前为完成状态，EventLoop 才能
   Finalize；业务 barrier 最多等待 15 秒，超时必须明确记录未完成并继续退出。
   gRPC 完成前不得锁存一次短暂的业务完成结果，因为在途 handler 仍可能追加工作。
4. Finalize 时先有界 flush Kafka producer，再执行 `tlsRedis.Shutdown()`；该调用
   会在 `MessageAsyncClient` 仍存活时触发 Hiredis pending callback 并注销 Channel。
   `loop.loop()` 返回后再执行 `tlsRedisSystem.Shutdown()` 销毁
   `MessageAsyncClient`，最后才允许 `EventLoop` 析构。固定析构顺序为
   Hiredis → MessageAsyncClient → EventLoop。

不得在 EventLoop 上立即 join 仍在等待 handler 的 shutdown worker，也不得
detach 后让它越过 `Node` 或 gRPC service 的生命周期。

## 演进路径（未来如遇瓶颈）

```text
阶段 0（现在）: 单线程业务 EventLoop + transport/background workers
    ↓  当网络投递和业务回调持续挤压逻辑帧
阶段 1: 增加 muduo I/O worker，只负责收发和解码
         所有业务状态修改继续 runInLoop 投递回主线程
    ↓  当单场景玩家密度极高
阶段 2: 场景内按 AOI 区域分片（需要明确 ECS 分片和跨分片消息协议）
```

## 触发演进的信号

- 投递到 EventLoop 的网络/Kafka 回调单帧耗时持续超过 2ms；
- `World::Update` 耗时接近 33ms 帧间隔；
- EventLoop 待执行队列持续积压；
- AOI 同步或跨场景序列化成为稳定瓶颈。

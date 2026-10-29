# Scene Node 线程归属、帧循环与停机

> 源码核对：2026-10-23。本次只更新文档，不新增构建、测试或线程采样证据。
> gRPC 传输与排空细节以[gRPC 传输设计 §15](./grpc-muduo-transport.md)为准；
> Redis 底层回调与释放见[Redis 生命周期设计](./redis-async-client-retry-fix.md)。

## 1. 业务状态归单个 EventLoop

Scene 的边界是业务状态归同一个 muduo EventLoop，不是整个进程只有一条线程。
ECS、玩家与场景变更、World tick、RPC0 业务处理、Kafka 消费后投递的业务回调均在该 loop 上执行。
Kafka 后台线程只收取并投递，不能直接改 Scene 的 registry。

当前 gRPC 客户端与服务端也由节点 loop 驱动；服务端接入、HTTP/2、派发、截止与排空，
以及生成的 `grpc.<M>.guard` 和同步 `Handle*` 都在该 loop。
外部结果如 Agones 许可通过异步请求取得，再经 queueInLoop 续跑，handler 不得阻塞等待。

进程仍可有 librdkafka、异步日志与 Agones lifecycle worker 等后台线程。
当前设计不构造官方 gRPC poller、EventEngine、Lifeguard 或 gRPC 关机 worker；
这不代表运行进程的线程数已被本轮实测为零。
2026-10-26 起移除官方 C++ SDK、初始化探针及其运行期守卫。依赖边界由构建与源码契约检查；
ThreadMonitor 继续采样实际线程基线，不再查询官方库的初始化状态。
`GRPC_SERVER_MAX_POLLERS` 已退役，不能用它或旧线程池变量调当前节点线程数。

## 2. 当前帧与派发预算

`core/constants/fps.h` 的目标为 **20 FPS，固定模拟步长 50ms**，
World 初始化使用 `delta_time=1/target_fps`，Scene 据此安排定时驱动。
这不是墙钟调度保证：World 累计时间限制在 ±1 秒，每次 Update 最多执行 5 个模拟步，
并丢弃超过追赶上限的模拟时间。旧文的“33ms 帧间隔”不是当前实现。

gRPC 派发泵默认每轮最多处理 64 项，在调用之间检查 2000µs 预算，至少处理一项。
它不能抢占正在同步执行的 handler；单个 handler 就可能超过 2ms。
因此 2ms 是协作式派发预算，不能当作单次 handler 或整轮执行的硬上限。

容量文档中另行约定的 `p99≤33ms` 等验收目标不因此自动放宽；固定步长、实测帧耗时与目标 SLO 必须分开判断。

## 3. gRPC 排空与业务排空

gRPC 排空在 loop 内推进，没有该部分专用的关机线程或 join；
整体关机仍会停止并 join Kafka consumer、Agones worker 等，因此不能概括成“停机完全没有 join”。

状态可概括为：

```text
Running → ClosingIngress →（GrpcDraining 与 PersistenceDraining）→ Finalizing → Done
```

1. 停止并 join Kafka consumer，封住 Kafka 业务入站；producer 继续存活，供退出存盘使用。
   执行关机前业务 hook，Scene 停止 lifecycle，并让尚待许可的创建请求按拒绝路径收口。
2. 调用 `BeginShutdown({2s, 1s, 1s}, onDrained)`，设置排空状态、排队撤监听，发送两阶段 GOAWAY。
   最终 GOAWAY 前的在途请求仍可能追加业务工作。到期后拒绝未开始的调用、结束已开始的调用；
   已提交的 DATA / trailers 仍须处理写窗口与背压，帧写完且所有流关闭后半关；排空到期路径的 closeGrace 从该到期时刻起算，
   到期即 forceClose，不是半关后再额外等待一秒。
   这些预算依赖 loop 正常推进，不能解释为整个进程严格三秒退出。
3. 业务 barrier 每 100ms 现场重查：玩家实体为空、玩家存盘无在途、退出释放标记为零、
   改派确认表为空、Kafka producer 已 flush。它不是 MySQL 全部落地的证明，
   具体边界见[属主重入屏障](./scene-owner-reentry-barrier.md)。
4. gRPC 的 onDrained 经 queueInLoop 投递并销毁 Server；只有传输已排空，且业务 barrier 当前满足或其 15 秒预算耗尽才 Finalize。
   业务 drain 的 15 秒预算到期时记录未完成项并继续收尾，不能锁存较早一次短暂完成。

身份冲突路径还可能先经历独立的业务排空预算。外线程 Shutdown 的约 20 秒等待用于报告卡顿，
析构仍等待安全收尾，不能把任一单段预算当作整个进程的硬退出时间。

## 4. Finalize 的两阶段收尾

当前实现入口为
[Node::FinalizeShutdownInLoop](../../cpp/libs/engine/core/node/system/node/node.cpp)
与同文件的 FinalizeTeardownInLoop：

1. 残留 gRPC Server 只能经排队任务防御销毁。撤销节点租约、停止发现与 etcd，
   关闭 RPC0 在途调用并释放出站 MuduoGrpcChannelPtr，使失败完成先进入 loop 队列；
   再排队进入 teardown，不能直接拆业务状态。
2. 执行 KafkaManager 的最终有界 flush（此调用不关闭或销毁 producer），再执行一次 BeforeTeardown 业务 hook。
   Scene 在这里停止 DBTask Pump 并记录未落定事项；随后调用 `tlsRedis.Shutdown()`。
3. 释放 Hiredis 可能同步触发空 reply，业务客户端与回调捕获此时必须仍活着。
   本地 gRPC 完成尚未投递排尽时，让出一轮再检查；之后才清节点池、ECS 与日志资源。
4. 最后以两跳 queueInLoop 排空 forceClose 和 connectDestroyed，再 quit。
   完成标记及通知在最后一跳 quit 之后、完成锁内发出；解锁后不再访问 Node。
   不以定时器或固定睡眠代替该顺序。
5. loop 返回、Node 析构后，Scene 主入口显式调用 `tlsRedisSystem.Shutdown()`，由其 reset 客户端并销毁 MessageAsyncClient，最后析构 EventLoop。
   顺序保持 **Hiredis → MessageAsyncClient → EventLoop**。

muduo_grpc::Server 与 Acceptor 只能在排队任务或 loop 外析构，不能在 I/O 分发栈上同步 reset。
底层 Hiredis 自定义删除器还会把事件分发期间的最终释放推迟到 pending 阶段；
这不允许上层提前销毁 EventLoop 或业务回调依赖。连接所有权与回调要求遵守根 `AGENTS.md` §11.7。

## 5. 何时重新评估线程模型

当前选择通过多个 Scene 实例分担场景，不引入多个线程共享同一套可变 ECS。
若要增加 I/O worker 或拆分场景，需要另行明确状态所有权、消息投递、关闭和跨分片契约，不能只改线程数。

应结合实际 profiling 观察 World::Update 耗时、loop 待执行队列、网络／Kafka 回调耗时、
gRPC max_backlog / rejected_overload 及 AOI 热点。2ms 只能作为网络派发的观察信号，
不能直接指定玩法 System 的预算；固定步长也不能代替单实例容量测试。

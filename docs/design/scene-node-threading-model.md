# Scene Node 线程归属、帧循环与停机

> 线程与停机基线于 2026-10-23 核对。2026-11-05 补充队伍跟随及后续节点协程的接入契约。
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

### Scene 协程任务

2026-11-05 后续批次将角色授权、跨节点交接、战斗锁写入与续期、会话释放、存盘与认领、疏散改派、etcd 注册、号段预取及创建许可的单次异步操作收进协程。原有 SQL/Redis 围栏、未知结果确认表、重试身份、截止时间和持久状态机仍是权威。此次改变控制流表达，不构成吞吐或帧耗时结论，Java 内部实现无须照搬。

共享 `LoopTaskScope` 只把 Cobalt task 交给现有 muduo loop，`Close` 封住新增，`Stop` 取消本地等待，均不等待任务排空。`AwaitCallback` 的提交与回调必须在同一 loop，状态在提交前准备，立即拒绝、提交内重入及本地停止只交付一次。结果值在原回调期间复制或移动为自有数据，晚回调只持操作状态的弱引用。正常回调原栈恢复，保持现有事件顺序。`Stop` 只把 nullopt 交给当时正在等待的那一次；同一任务此后的 `co_await` 会在挂起前抛 `operation_aborted`（Cobalt 的 throw_if_cancelled），抛点之后的代码不再执行，所以拿到 nullopt 的业务直接 `co_return`。任务体的异常不吞：`Stop` 之后的 `operation_aborted` 只记一条警告，其余异常带任务名和 `what()` 经 `LOG_FATAL` 终止进程，与改造前回调里异常逸出 EventLoop 的结局一致。2026-11-11 之前只打一行不带原因的 ERROR、进程继续运行，半途更新的账本和计数会留在不一致的状态。

Scene 的任务所有者分为瞬态与持久排空两组。维护 P1、普通关机、身份冲突停止瞬态任务；瞬态组目前只有队伍跟随和镜像创建后自动进场。授权、存盘、会话释放，以及战斗锁的写入、续期与重建复核，在原业务排空期间仍允许必要工作，由 `TryStartDurable` 启动，只在 `BeforeTeardown` 停止本地等待。战斗锁归持久组，是因为排空期间备战和确认补发仍会到达，停写会让锁提前过期，玩家重登后重建不出冻结（2026-11-10 订正，11-05 的实现曾放在瞬态组）。维护 P3 路径不经过 `BeforeTeardown`：Node 关闭中继时只调中继停止钩子，随即置 `beforeTeardownRan_`、释放 Redis 与传输。所以 Scene 把同一段拆除前收尾（`StopAll`、关创建许可、停号段传输与归属对账、停定时器、打未落定存盘清单）同时挂在 `SetBeforeTeardown` 和 `SetMaintenanceRelayStop` 上，两条停机路径都在释放本地所有者之前停止持久任务（2026-11-11 订正，此前 P3 只停两个定时器，持久任务要到 `SceneRuntimeContext` 析构才停）。停止不减去未结束的远端写计数、不确认 SQL 提交、不解除围栏、不丢掉待确认记录。原传输关闭、Hiredis 空回复、客户端及 EventLoop 的销毁顺序不变。

etcd 注册、领段及创建许可由各自对象或节点作用域持有。etcd unary 每次调用有独立终态归属，旧阶段的迟到回复不能命中新阶段。号段超时后原有迟到认领窗口保留。Agones 继续调用现有异步许可接口，已存在的 worker 和队列投递规则不变，本批不增加线程或线程池。创建许可在维护 P1、普通关机、身份冲突入口与瞬态组一起关闸：此后新的 CreateScene 一律回 UNAVAILABLE，SceneManager 回滚后改派别的节点；等许可的请求按拒绝收口，已拿到许可的创建仍由许可覆盖到 `Proceed()` 返回。改造前非 Agones 模式下 P1 之后还能建场景、直到 gRPC 排空，Agones 模式下则因 lifecycle 已停而拒绝。2026-11-11 定案统一为从 P1 起拒绝：排空中的节点不接新放置，两种部署行为一致。

`SceneRuntimeContext` 持有 `SceneTaskScope`。队伍跟随和镜像创建后自动进场都在所属 Scene EventLoop 启动、恢复和停止。Cobalt 执行器的同线程 `dispatch` 内联，根任务的完成通知经 `queueInLoop` 回收。Boost 的 `execution_context` 提供执行器属性，实际工作由 muduo 驱动。适配器的 Asio `blocking.possibly` 属性允许同栈执行当前工作，不表示等待异步结果；等待仍由协程挂起表达。

等待回复期间协程挂起，Scene 可以执行其他排队工作。正常 Redis 和 gRPC 回包直接在原回调栈恢复。Redis 提交内即时拒绝或重入完成在 `command` 返回后交付。镜像沿用生成 gRPC 客户端的异步完成，连本地发送失败也按原规则排队交付。

维护 P1、普通关机和身份冲突入口关闭瞬态任务准入并发出本地取消，持久任务在拆除前才停止。本地取消在当前 loop 结束等待，根任务完成通知稍后清理停止信号。晚到的 Redis 和 gRPC 回包只持等待状态的弱引用。号段任务的 TLS 所有者还由 `SceneRuntimeContext` 析构兜底收掉，覆盖启动异常，先于局部 EventLoop 销毁。整个路径沿用已有线程，以及下文 Redis 与 `MessageAsyncClient` 的销毁顺序。

单次完成协议全仓只有 `loop_async::AwaitCallback` 一份（`core/async/coroutine_callback.h`）。队伍跟随的 Redis GET 与镜像的 CreateScene 原先各自手写了一份等价的 Cobalt `op`（取消槽、提交内重入延后交付、晚回包只持弱引用）。2026-11-11 起两处都改成接到公共等待体的薄接线：跟随在 hiredis 回调里把应答转成自有的 `GetResult`，镜像在 gRPC 回调里复制 `CreateSceneResponse`。生成的 gRPC 客户端恰好回调一次，连本地拒绝也延后交付，所以镜像的提交恒为接纳。行为不变，原有跟随与镜像用例照样覆盖。以后接新的单次异步接口一律用 `AwaitCallback`，不再手写 `op`。

2026-11-11 审查定案，以下几处保持现状，原因写在这里，免得下次重开：

- **认领应答整份复制保留。** `GrpcDone` 契约规定回包只在回调期间有效，零拷贝要改全仓生成客户端的完成签名。认领每次登录或交接只发生一次，不在帧路径上。没有剖析数据之前，不为它改全局契约（AGENTS.md §11.3）。
- **只等一次的存盘协程不退回裸回调。** 它们和多跳任务共用同一套停止语义：`StopAll` 之后不再执行业务。提交内同步完成要延后到提交返回之后才交付，由 `PlayerStoreClientTest.InlineReplyIsDeliveredAfterSendReturns` 钉住。改回回调要逐处重新证明这两条，换来的只是少一层包装。
- **换图与改派的 EnterScene 应答仍由协程等待。** `LifecycleCoroutineTest.TravelPublishesBeforeEnterAndLateReplyCannotMovePlayer` 钉住「拆除点之后的晚应答不能再移动玩家」。两条停机路径现在都在同一个拆除点 `StopAll`，这条保证在维护路径上同样成立。
- **注册冲突的晚回包仍记全服分配清单。** 预算过期后才到的冲突回包照样调用 `NodeAllocator::RecordAllocatedIds`。清单只是选号提示，号的归属以分配键 CAS 为准。旧清单最多让下一次选号多撞一轮 CAS，不会让两个节点拿到同一个号。
- **Cobalt 等待体的小缓冲用库默认值。** `BOOST_COBALT_SBO_BUFFER_SIZE` 保持 4096 字节。没有剖析数据说明协程帧分配或这块缓冲是瓶颈，不调这个值。

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
   执行关机前业务 hook，Scene 停止瞬态协程任务和 lifecycle，并让尚待许可的创建请求按拒绝路径收口；持久写入与归属释放继续参加原有排空。
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
   Scene 在这里停止全部本地协程等待及领段任务，记录未落定事项；随后调用 `tlsRedis.Shutdown()`。
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

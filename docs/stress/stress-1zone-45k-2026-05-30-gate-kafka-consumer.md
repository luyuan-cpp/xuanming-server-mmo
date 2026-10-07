# 45k Round: gate-group-1 consumer 失活 → 主瓶颈

**日期**: 2026-05-30 (复盘 2026-05-31)
**前置**: [stress-1zone-25k-2026-05-29-dispatcher-async-lpop.md](./stress-1zone-25k-2026-05-29-dispatcher-async-lpop.md)
**Run dir**: `robot/logs/stress-45k-z1-20260530-102927`

**结论**: 1zone 45k 压测主瓶颈不在 dataloader / db / scene_manager,而在 **Gate 的 Kafka command 消费链路**。`gate-group-1` 在 ramp 中段被 broker 踢出,`no active members`,`gate-1` topic lag 一度 17,299,玩家连上 TCP 但拿不到 BindSession/RoutePlayer,scene 永远不 ready。次级退化:dataloader `kafka_send avg` 从 4.8ms 涨到 322.7ms。

修复路径已落地(待 Round 9 验证):
1. **Kafka poll 移出 muduo EventLoop 主线程**,改为独立线程 + `queueInLoop` 派回主线程执行回调。
2. **加 librdkafka 关键调优**:`max.poll.interval.ms=15min`、`session.timeout.ms=45s`、`heartbeat.interval.ms=3s`、`partition.assignment.strategy=cooperative-sticky`。
3. **`KafkaManager::Init` 空 GroupID 短路**:不再启动失败的占位 consumer。
4. **service_discovery 过滤 `/allocated/` key**:避免对裸 uuid value 走 JSON parse,消除日志风暴。

---

## 1. 现场数字

### 1.1 Robot 视角

| 指标 | 值 |
|---|---|
| ramp 目标 | 45,000 robot |
| `entered game` | 26,961 |
| `EnterGame success` | 23,711 |
| `scene_ready_timeout` | 23,784 |
| `retrying login` | 20,721 |
| `gave up after retries` | 3,063 |
| `preload_failed` | 140 (≈0.6%) |

`entered game` 26k 是这一轮跨过 EnterGame ack 的玩家数,但有 23.7k 又在等 scene ready 时超时 —— **登入流程的 `gate↔scene 桥接`这一步基本崩了**。

### 1.2 stress_summarize 表

```
=== Robot per-minute stats =============================================

  time   conn        login_ok   enter_ok   enter_fail  recon_fb  max_login  msg/s
  ----   ----        --------   --------   ----------  --------  ---------  -----
  1m0s         2,104      2,076      2,076           0         0 336ms      145/s
  2m0s         6,078      6,030      6,030           0         0 791ms      332/s

=== EnterGame end-to-end (entergame_*) =================================

  snapshot                       success  preload_failed  apply_failed  fail%   preload{ok}avg  preload{fail}avg  apply avg
  --------                       -------  --------------  ------------  -----   --------------  ----------------  ---------
  t_final_before_stop              23,711             140             0 0.6%    114.2ms         5,460.7ms         285.3ms
  t00m_ramp                           762               0             0 0.0%     15.4ms         -                  42.0ms
  t02m_steady                       6,472               0             0 0.0%     14.0ms         -                 137.6ms
  t05m_steady                      18,747             126             0 0.7%     61.1ms         5,489.5ms         266.0ms

=== Dataloader per-stage avg (dataloader_preload_*) ====================

  snapshot                       cache_check  sub_cache  dispatcher  kafka_send  cb_wait{ok}  cb_wait{fail}
  --------                       -----------  ---------  ----------  ----------  -----------  -------------
  t_final_before_stop              2.3ms        4.5ms      0.0ms     322.7ms      86.9ms      5,447.6ms
  t00m_ramp                        1.6ms        3.2ms      0.0ms       4.8ms       8.7ms      -
  t02m_steady                      1.9ms        3.6ms      0.0ms      10.1ms      15.7ms      -
  t05m_steady                      2.5ms        4.6ms      0.0ms      93.6ms      98.4ms      5,447.6ms

=== Kafka consumer lag (live) ==========================================
  TOTAL LAG: 0   (机器人停掉后追上来了,运行期 gate-group-1 一度 17,299)
```

### 1.3 与 Round 8 (25k dispatcher-async-lpop, prev-summary.txt) 的对比

| 指标 | Round 8 (25k @ 18m steady) | Round 9 (45k @ 5m steady) | Δ |
|---|---|---|---|
| `entergame_success` | 205,224 | 18,747 (5m) → 23,711 (final) | — |
| `entergame fail%` | 17.8% | 0.7% (preload only) | preload 不再是瓶颈 |
| `cb_wait{ok}` avg | 2,289 ms | 98.4 ms (5m) / 86.9 ms (final) | Round 8 修复有效 |
| `cb_wait{fail}` avg | 5,492 ms (44k entries) | 5,447 ms (126 entries) | tail 一致,样本 -350x |
| `kafka_send` avg | 8 ms | 93.6 ms (5m) → 322.7 ms (final) | **退化 40x** |
| dataloader `preload{ok}` avg | 28.6 ms | 61.1 ms (5m) → 114.2 ms (final) | 退化 4x |
| Kafka backlog | 4k(全 partition 全压住) | 0(stop 后回收) | 不同根因 |

Round 8 修的 dispatcher 链路在 45k 下依然稳:`preload_failed` 从 17.8% 跌到 0.6%,fail tail 样本数从 44,444 跌到 126(-99.7%)。**这一轮的失败 23.7k 主要不是 preload 失败,是 scene_ready 超时**。

---

## 2. Root cause:Kafka consumer 跑在 EventLoop 主线程 + 配置缺关键调优

### 2.1 失活证据

`kafka-consumer-groups.sh --describe --group gate-group-1` 期间一度:
```
gate-group-1   gate-1   lag=17299
Consumer group 'gate-group-1' has no active members.
```

gate-2 进程还活着,客户端能连上 TCP,但 gate 日志(`bin/logs/cpp_nodes/gate.20260530-104739.luyuan.50892.log` 等三个 12MB 滚动)里:
- **零** Kafka / rdkafka / coordinator / rebalance 相关行(grep 三个文件全 0 命中)。
- 大量 `Client disconnected ... scene_notified=0 ... remaining_sessions=N` —— 玩家连上 gate 但 gate 从来没把 client 桥接给 scene。
- 大量 `Connection ID not found for PlayerMessage, session ID: ..., message ID: 79` —— scene 回包来时玩家已断,因为根本没桥起来。

`scene_notified=0` 是直接信号:**gate 收到 BindSession 后没拿到 Kafka 里的 RoutePlayer 命令把它路由到 scene**。

### 2.2 代码路径

1. `cpp/libs/engine/core/node/system/node/node.cpp:478` `Node::InitKafka()` 在 `Initialize()` 里被调,走 `KafkaManager::Init(config.kafka())` → `Subscribe(topics=["game-events"], groupId="")`。
2. `bin/etc/base_deploy_config.yaml:71` 是 `GroupID: ""`(注释里写得很清楚:留空让每节点自己生成 `<groupPrefix>-<node_id>`)。
3. `KafkaConsumer::init` 把 `""` 喂给 `conf_->set("group.id", "", ...)`,librdkafka 拒收 → `consumer_` 是 null → 早期 `KafkaConsumer initialized` log 不会出现。
4. 之后 `Node::StartRpcServer` 里 `RegisterKafkaHandlers()` → gate main.cpp 的 `SetKafkaHandlers` lambda → `RegisterKafkaCommandHandler<GateCommand>` → `RegisterKafkaMessageHandler({"gate-1"}, "gate-group-1", ...)` → **第二次** `KafkaConsumer::Instance().stop() + init() + start()` 用 `"gate-group-1"`,这个才是真正起来的 consumer。
5. 这一次 init() 里没有任何 `session.timeout.ms` / `max.poll.interval.ms` / `heartbeat.interval.ms` / `partition.assignment.strategy` 配置 —— librdkafka 全部走默认。
6. `Node::StartKafkaPolling` 用 `kafkaConsumerTimer.RunEvery(0.1, kafkaManager.Poll)` 把 `consume()` 钉在 muduo **主 EventLoop** 上。同一个 EventLoop 还跑:
   - RpcServer TCP accept / message dispatch
   - 客户端 TCP message callback(ProtobufCodec onMessage)
   - scene_response_handler 转发(scene 回包 → 客户端)
   - playerCountReportTimer 等所有 timer
7. 45k 压测中,client TCP connect 风暴 + RPC handler 占满 EventLoop tick,**100ms 的 kafka poll timer 错过 → consume() 几秒甚至几十秒不调 → broker 看不到任何消费动作(`max.poll.interval.ms` 默认 5min)→ 协调器把 consumer 踢出 group**。

被踢之后,`gate-group-1` 没有 active member,broker 不会再把 partition 分配给任何人,**topic 数据停在那里直到下一次 consumer rejoin**(rejoin 也得靠 consume() 被调到才会发生),恶性循环。

### 2.3 次级退化:dataloader kafka_send 322.7ms

`dataloader_preload_kafka_send` 是 login → db Kafka 写入耗时。从 ramp 的 4.8ms 退化到 322.7ms 不是 Kafka 自己慢,是 **login 端 Sarama SyncProducer.SendMessages 在 broker 负载升高时排队**。这一项不是阻塞瓶颈(`cb_wait{ok}` 仍然 87ms,preload_failed 仅 0.6%),Round 10 验证 P0 修复后再看是否需要单独优化。

---

## 3. 修复

### 3.1 KafkaConsumer 配置加 4 项 librdkafka 关键调优
`cpp/libs/engine/infra/messaging/kafka/kafka_consumer.cpp`:
```cpp
{"session.timeout.ms",             "45000"},
{"max.poll.interval.ms",          "900000"},   // 15min,容忍长 stall
{"heartbeat.interval.ms",           "3000"},
{"partition.assignment.strategy", "cooperative-sticky"},
{"fetch.min.bytes",                    "1"},
{"fetch.wait.max.ms",                 "50"},
```

`cooperative-sticky` 在 rebalance 时不会一次性 revoke 所有 partition,这对 45k 登入风暴下"反复 rebalance"特别重要 —— eager 模式每次 rebalance 都 revoke→重新 assign,期间 lag 一路涨。

### 3.2 Kafka poll 移到独立线程
`cpp/libs/engine/infra/messaging/kafka/kafka_consumer.{h,cpp}` 新增:
```cpp
void startBackgroundPolling(muduo::net::EventLoop* dispatchLoop);
```
- 独立 std::thread 跑 blocking `consumer_->consume(200ms)` 循环。
- 拿到 message 后通过 `dispatchLoop->queueInLoop([cb, topic, payload=move]{ cb(topic, payload); })` 把回调投回主线程执行。
- ECS / node 状态访问全部仍在主线程,**单线程不变量保留**。
- 旧的 `poll()` 接口保留为前向兼容,但在背景线程已启动时自检短路,避免双线程 `consume()` 同时跑同一个 `KafkaConsumer`(librdkafka 接口不允许)。

`cpp/libs/engine/core/node/system/node/node.cpp` 的 `StartKafkaPolling` 从 `kafkaConsumerTimer.RunEvery(0.1, Poll)` 改成 `kafkaManager.StartBackgroundPolling(eventLoop)`。

### 3.3 KafkaManager::Init 空 GroupID 短路
`cpp/libs/engine/infra/messaging/kafka/kafka_manager.cpp`:
```cpp
bool KafkaManager::Init(const KafkaConfig& config) {
    if (config.group_id().empty()) {
        // Producer 仍然需要 brokers
        KafkaProducer::Instance().setBrokers(JoinBrokers(config));
        return true;   // 真正的 subscribe 由 RegisterKafkaCommandHandler 接管
    }
    ...
}
```
和 `KafkaConsumer::init` 里的 `groupId.empty()` 早退保护配合,消除「先尝试空 group 失败,再被 SetKafkaHandlers 覆盖」的早期日志噪音。

### 3.4 service_discovery 过滤 allocated/ 路径
`cpp/libs/engine/core/node/system/discovery/service_discovery_manager.cpp::HandleServiceNodeStart`
+ `cpp/libs/engine/core/node/system/node/node.cpp::HandleServiceNodeStop`:
```cpp
if (key.find("/allocated/") != std::string::npos) {
    LOG_TRACE << "Skip allocation-slot key for service discovery: " << key;
    return;
}
```
`EtcdManager::RegisterNodeService` 写 `<NodeType>.rpc/allocated/...` 的 value 是裸 uuid(CAS 占位),不是 NodeInfo JSON。`service_discovery_prefixes` 让 RangeQuery 把这些 key 也带回来,旧代码统统送 `JsonStringToMessage` → "Parse node JSON failed ... JSON: <uuid>" 每节点每轮发现刷一行 ERROR。45k 启动期间几个 NodeType × 上百节点 × 多次 Range = 日志风暴 + 日志 IO/CPU 浪费 + 干扰真实故障定位。

---

## 4. ARCH §11 决策行

```
2026-05-31 | Round 9 45k 主瓶颈 = Gate Kafka consumer 失活
         | EventLoop 主线程 poll 在 45k connect 风暴下错过 max.poll.interval.ms
         | → 协调器踢出 gate-group-1,topic lag 17k,scene_notified=0
         | 修法:Kafka poll 移独立线程 + 加 librdkafka 4 项调优 + 空 GroupID 短路
         | + service_discovery 过滤 /allocated/ 消除日志风暴
         | 待 Round 10 验证;无 prev-summary 不开 Round 10
```

---

## 5. Round 10 验证要点

跑前清理(CLAUDE.md §9.2 全照办):
- 把这次 `stress_summarize` 输出存为 `docs/stress/stress-1zone-45k-2026-05-30-gate-kafka-consumer.prev-summary.txt`。
- redis FLUSHALL + kafka offset reset + 删 mysql/etcd 数据。
- 删 `bin/log/*`、`tools/scripts/.run/`、旧 robot/logs/stress-*(只留最近 1 个备查)。

跑测中要看的关键指标:
1. **gate-group-1 是否一直 active**:`kafka-consumer-groups.sh --describe --group gate-group-1` 在 ramp / 稳态末两次抓快照,members 数 = gate 实例数。
2. **gate-1 topic lag**:三时刻 snapshot,期望都 < 200。
3. **scene_notified=1 比例**:`grep -E "Client disconnected.*scene_notified" gate*.log | awk` 算 ratio,期望 >99%。
4. **dataloader kafka_send 退化**:看 final snapshot 是否回到 5m_steady 时的 ~94 ms 水平或更低。
5. **Parse node JSON failed**:`grep -c "Parse node JSON failed" bin/logs/cpp_nodes/*.log` 必须 = 0。

如果 (1)(2)(3) 通过 → Gate consumer 主瓶颈确认解除;再看 (4) 决定是否要并行修 login → db Kafka 写入的退化。

---

## 6. 没改但可能踩的下一颗雷

- **scene 节点也用同一个 `KafkaConsumer` 单例(thread_local)**:scene main.cpp `RegisterKafkaMessageHandler({"player_migrate", "player_migrate_ack"})` 走同样路径,scene 主线程一样跑 World::Update 帧逻辑 + RPC,如果稳态玩家多 + 跨 zone migrate 多,scene 的 `scene-cross-zone-<id>` group 会同样症状。本次修复在基础设施层,scene 自动受益,但要在 Round 10 一并看 scene-cross-zone-* group 的 active members。
- **librdkafka 后台线程数量**:每个 KafkaConsumer 实例创建后 librdkafka 会拉 N 个 background thread(broker thread + cgrp thread)。thread_local 单例 + 多 worker thread 的话,thread 数会乘倍。本次未压测期间 verify thread count,Round 10 用 `Get-Process | Select ... Threads.Count` 抓一下。

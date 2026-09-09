# 控制面命令 topic:从"一节点一 topic"改为"固定分区 + 按 node_id 取模寻址"

> 状态:已落码(2026-09-08)。迁移开关 `Kafka.DisableLegacyPerNodeTopic` 默认 `false`,
> 全部生产者切完后置 `true` 才算完成。

## 0. 一句话

控制面命令(`GateCommand` / `SceneCommand`)不再一个进程一个 Kafka topic,改成
**一个节点类型一个 topic + 固定不可变分区数 P**,`partition = node_id % P`;
生产端显式指定分区,消费端 `assign` 自己那一个分区(不进 consumer group)。

---

## 1. 两个问题

### P1(正确性,HIGH):僵尸前任把继任者饿死

老寻址是 `gate-<node_id>` / `scene-<node_id>`,consumer group 是 `<type>-group-<node_id>`
(`node_kafka_command_handler.h`)。而路由 `node_id` 由 `node_allocator.cpp` 用**最小空闲扫描**
立刻回收 —— 没有隔离期,而且最小空闲的那个槽恰好就是"前任最可能还活着"的槽。

```
T0    gate A 拿到 node_id=5,加入 gate-group-5
T1    A 被冻结(GC 停顿 / cgroup freeze / 宿主机 live-migration / 网络分区),
      但 librdkafka 的后台心跳线程仍可能维持成员资格,或它只是"看起来死了"
T1+   etcd lease 过期,node_id=5 被回收
T2    gate B 启动,拿到 node_id=5,也加入 gate-group-5
T3    Kafka 把 gate-5 的分区判给 group 里的**一个**成员
```

判给 B 时一切正常。**判给 A 时**,A 消费掉本该给 B 的命令,再按 `target_instance_id`
(进程 uuid,`node.cpp:506` 每次启动重新生成)发现不是自己的、丢弃;B 永远收不到 ——
登录绑定(BindSession)、进世界(RoutePlayer)、踢人(KickPlayer)、租约过期清理、
战斗绑定(BindBattle)与结算(BattleSettlement)全部**静默消失**。

关键区分:`target_instance_id` 这道过滤是对的、也是必要的,它防的是**错执行**
(A 拿 B 的会话去做事)。但它防不住**饿死**:消息被别人吃掉这件事,过滤器看不见。
问题出在"谁能拿到这个分区"这一层,不在"拿到之后做什么"这一层。

顺带确认:`target_node_id` / `target_gate_id` 这道更便宜的数字过滤,在多数 Go 生产者
(friend / guild / match)里根本没填(留 0),`ValidateCommandTarget` 遇到 0 直接放行 ——
也就是说当时真正生效的只有那一次字符串比较。本轮把它们补齐了(见 §5)。

### P2(规模,HIGH):10 万个 topic 撑不住

目标规模是 **~10 万个 scene 进程、每天重启**。一个进程一个 topic 意味着单集群 10 万个 topic:

- controller 的元数据是全量广播的,topic/partition 数直接决定 metadata 大小与传播成本;
- 每个分区在 broker 上都有独立的日志段、索引、内存与文件句柄;
- 每天重启 = 每天 10 万次 group 加入/退出,rebalance 时间随成员与分区数增长。

这三条里任何一条先塌方,表现都是"控制面整体变慢或停摆",而不是某一个玩家进不去。

---

## 2. 为什么不是"每个分片一个 Kafka 集群"

拆集群是把 topic 数摊薄的常规手段,这里**明确否决**,原因是产品形态:

> **全国同服:任意分片的玩家必须能和任意分片的玩家一起玩。**

全局池的 `match` 撮合出来的一局,参战者可能来自任意分片;`battle` 节点结算后要把
`BattleSettlement` 发回**任意** scene,把 S2C 推给**任意** gate。如果控制面按分片切成
互不相通的 Kafka 集群,`battle` 就必须先知道"目标 gate 在哪个集群"、再持有到每个集群的
producer 连接 —— 等于把分片边界重新引入到一条本来无关分片的链路上,而且每加一个分片
就要改一次全局服务的连接拓扑。这跟"全局池"的前提直接冲突。

结论:**控制面 Kafka 必须是一个可达的集群**,规模问题只能在这个集群内部解决 ——
也就是把"多 topic"换成"多分区"。分区是 Kafka 真正为规模设计的维度:P 个分区的元数据
是常数级的,和有多少个消费者进程无关。

---

## 3. 设计

### 3.1 topic 与分区

| 项 | 值 |
|---|---|
| topic 基名 | `<节点类型短名>-cmd`,即 `gate-cmd` / `scene-cmd`(沿用 `BuildDefaultKafkaOptions` 的按类型短名派生,新节点类型不用另配) |
| 有效名 | `<基名>_g<代号>`,当前 `gate-cmd_g1` / `scene-cmd_g1` |
| 分区数 P | 256(默认),**不可变契约** |
| 寻址 | `partition = node_id % P` |
| 保留期 | 1 小时(见 §3.3) |

代号(generation)带在第一代上就出现,与 `data_service` 的
`transaction_log_topic_g1` / `player_snapshot_topic_g1` 同形。这不是洁癖:broker 开着
`auto.create.topics.enable` 且 `num.partitions=1`,任何抢先发消息的生产者都会把裸名字的
topic 自动建成 1 分区;带代号的名字与那个裸名 topic 互不相干,第一次就能按契约建对。

用**取模**而不是哈希:两端实现必须逐位一致,取模是唯一不会因语言/库版本漂移的映射;
`node_id` 由分配器顺序发放,分布本来就均匀,哈希没有额外收益。

真源两份、由一份共享向量对拍:

- C++:`cpp/libs/engine/core/node/system/node/node_command_topic.h`(纯函数,零依赖)
- Go:`go/shared/kafkacmd/command_topic.go`
- 向量:`go/shared/kafkacmd/testdata/command_partition_vectors.json`,
  被 `go/shared/kafkacmd/command_topic_test.go` 与 `cpp/tests/kafka_command_test` 同时读取。

### 3.2 消费端:assign,不 subscribe

每个进程 `assign(topic, node_id % P)`,**不加入 consumer group**。

这一条就是 P1 的修复本身:librdkafka 在只 `assign()` 不 `subscribe()` 时走 simple consumer
模式,不发 JoinGroup、不参与任何协调。谁 assign 谁读,前任是死是活与本进程无关 ——
"分区被判给别人"这个状态不再存在。同时也把 10 万消费者规模下的 rebalance 成本一并去掉。

group id 仍然拼一个(`<基名>-<node_id>`)只为日志可读:不 subscribe 就不会 JoinGroup,
又关掉了 auto-commit,broker 上根本不会出现这个 group。

**分区契约门禁**(`KafkaConsumer::init`):assign 之前先向 broker 查该 topic 的分区数,
broker **答了一个不同的数字**就**拒绝启动**。理由是 assign 一个不存在的分区不会报任何错,
只会安静地永远收不到消息 —— 那正是"登录卡在分配场景"这类最难查的故障。宁可起不来。
(broker 冷启动期会先重试 5 次 × 1s,不把选举竞态当成契约违反。)

刻意留的一个口子:**查不到**分区数(broker 不可达、元数据还没广播开)时只记 ERROR 继续,
不拒绝启动。改造前的 `subscribe()` 在 broker 不可达时同样成功、由后台重连兜底;
把它变成启动致命等于让"Kafka 晚起一会儿"直接打死整个 gate/scene,那是本次改造引入的
新脆弱性,不是修复。真正危险的那一种(broker 答 1 分区)仍然 fail-closed。

### 3.3 offset 策略:每次启动从分区末尾开始,不提交 offset

`KafkaPartitionAssignPolicy{ startAtEnd = true, ownOffsets = true }`:
assign 时显式定位 `OFFSET_END`,并把 `enable.auto.commit` 关掉
(`auto.offset.reset` 同时设为 `latest` 作为第二道保险)。

两个理由,都不是偏好问题:

1. **offset 没有主人**。共享分区意味着同一个 `(group, topic, partition)` 会被几百个进程
   写同一个 offset。谁提交都不能代表别人的进度;提交上去只会互相覆盖,并让重启的进程
   按别人的位置起跳,**漏掉发给自己的命令**。这比不提交严重得多。
2. **回放没有收益**。发给某个进程的命令不可能早于这个进程的启动时刻 ——
   `target_instance_id` 是启动时新生成的 uuid,历史里带的都是别人的(或前世的)uuid,
   读回来只会被 `ValidateCommandTarget` 全部丢弃。从头读一遍等于白读几十万条。

代价是诚实的:**进程重启期间发给它的命令会丢**。这在旧方案里也一样成立(旧方案的
`auto.offset.reset=earliest` 看似能补,但补回来的命令带的是前世的 instance uuid,
同样会被丢弃),所以不是本次改造引入的退化。控制面命令本身也都有上层兜底:
登录/进场景由客户端重试,租约过期由 `player_locator` 的 claim 重投,
战斗结算由 scene 侧 reaper 按 `InBattleComp.deadline_ms` 兜底。

topic 保留期因此设成 1 小时:没有任何人会回放历史,长保留期只是白占磁盘。

### 3.4 生产端:显式分区

- C++:`KafkaProducer::send()` 本来就接受显式 partition 参数,加一个
  `node::kafka::ResolveCommandRoute(nodeType, targetNodeId)` 一次性给出 `{topic, partition}`。
  **不允许再自己拼 `"gate-" + id`**:topic 名与分区号是一对不可分的东西,分开写必然漂移,
  而漂移的表现是消息落到没人 assign 的分区上、Kafka 一个错都不报。
- Go:`kafka-go` 的 `Writer` 在写入路径上**忽略** `Message.Partition`,只问 `Balancer`
  (`writer.go`: `balancer.Balance(msg, partitions...)`)。所以新增
  `kafkacmd.CommandPartitionBalancer`:命令 topic 原样返回 `Message.Partition`,
  其它 topic 交给原来的 `&kafka.Hash{}`,行为不变。
  broker 分区集合里没有目标分区时(契约不符),记一行能直接定位的 ERROR 再走 fallback ——
  这条消息注定送不到,但沉默是最坏的结果。

---

## 4. 读放大预算

同一个分区上坐着多个节点,每个节点读**整个分区**再按
`target_node_id` → `target_instance_id` 两级过滤。这是刻意接受的成本,算一下:

| 项 | 数值 |
|---|---|
| 目标 scene 进程数 | ~100,000 |
| P | 256 |
| 每分区的节点数 | ~390 |
| 控制面总量(全系统) | 数百条/秒,取 500/s |
| 每分区消息速率 | 500 / 256 ≈ **2 条/秒** |
| 单节点读取速率 | ≈ 2 条/秒(其中约 1/390 是给自己的) |
| 单节点浪费的解码 | ≈ 2 次 protobuf 解码/秒 |

即:**每个进程每秒多解码约两条几百字节的消息,并对其中绝大多数只做一次整数比较**。
按每条 200B 算,单节点入向带宽约 400 B/s;10 万节点对 broker 的总出向是
100,000 × 400 B/s ≈ **40 MB/s**,分摊到 256 个分区、由多个 broker 承载。
这是 fan-out 的真实代价(每条消息被投递约 390 次),但绝对量在控制面这个数量级上可以接受。

两个刻意的设计跟这条预算绑在一起:

1. **两级过滤的顺序**:先 `target_gate_id` / `target_scene_id`(整数),再
   `target_instance_id`(字符串)。整数那级挡掉约 389/390 的消息。为此本轮把留 0 的
   Go 生产者全部补齐(§5)—— 留 0 会让第一级直接放行,每条消息都退化成一次字符串比较。
2. **日志级别**:不是给自己的消息走 `LOG_DEBUG`。一个分区几百个节点、每人对每条别人的
   命令打一行 WARN,日志量会是控制面消息量的几百倍。真正该报警的是
   "node_id 是我的、但 instance 对不上"(僵尸残留),那一条仍然是 WARN。

**P 的选取**:P 太小 → 读放大随节点数线性上升;P 太大 → 回到 P2 的元数据问题,而且
每个分区都要有独立的日志段。256 在 10 万进程下给出 ~390 的放大比,而 256 个分区对单个
Kafka 集群是完全常规的规模。P 必须能覆盖节点数的**分布**而不是数量:`node_id ∈ [1, 2^17)`,
`% 256` 把它们均匀铺开。

---

## 5. 被"补齐"的生产者(target_node_id / target_gate_id)

| 生产者 | 改动前 | 改动后 |
|---|---|---|
| `go/friend/internal/kafka/gate_command_builder.go` | `TargetGateId` 留 0 | 由 `gate_push` 传入 gate node id 并填上 |
| `go/guild/internal/kafka/gate_command_builder.go` | 同上 | 同上 |
| `go/match/internal/kafka/gate_command_builder.go` | 同上 | 同上 |
| `cpp/nodes/battle/logic/battle_room_manager.cpp` `SendGateCommand` | `target_gate_id` 未设 | 设为 `routing.gate_node_id()` |
| `cpp/libs/services/scene/battle/system/player_battle.cpp` BindBattle | 同上 | 设为 `gateRoute.gateNodeId` |
| `go/scene_manager` / `go/player_locator` | 本来就填了 | 不变 |

共享收口 `kafkautil.GateCommandBuilder` 的四个方法因此各多一个 `gateNodeID uint32` 参数。

另外,`gate_id` 解析不出数字时一律 **fail-closed**(返回错误),不再有"发到 0 号分区"这种
默认行为 —— 0 号分区上坐着 `node_id` 是 256 倍数的那些节点,命令会被它们读到再丢弃,
等于静默丢消息。

---

## 6. 分区数契约:怎么改

分区数是**寻址协议**的一部分,不是容量参数。与 `db_task`
(`docs/design/db-task-kafka-partition-contract.md`)同一条纪律:

> **绝不在同名 topic 上 `--alter --partitions`。** 扩分区会把 `node_id % P` 重映射,
> 一部分节点算出的分区号变了,而在场的消费者仍 assign 着老分区 —— 命令落到没人读的
> 分区上,静默全丢,Kafka 一个错都不报。

改分区数 = **代号 +1 换一批新 topic**。四处必须同拍(缺一处就是全丢):

1. `bin/etc/base_deploy_config.yaml` → `Kafka.CommandTopicPartitions` / `Kafka.CommandTopicGeneration`
2. `go/shared/kafkacmd/command_topic.go` 的 `DefaultCommandTopic*`,或注入环境变量
   `KAFKA_COMMAND_TOPIC_PARTITIONS` / `KAFKA_COMMAND_TOPIC_GENERATION`
3. `deploy/docker-compose.yml` 的 `KAFKA_INIT_COMMAND_PARTITIONS` / `KAFKA_INIT_COMMAND_TOPIC_GENERATION`
4. `deploy/k8s/manifests/infra/kafka-topic-init.yaml`(值由 `Apply-KafkaTopicInitJob` 从第 1 项读,
   所以改完 1 就够,但要重跑 `k8s_deploy.ps1 -Command infra-kafka-topics`)

同步向量文件 `command_partition_vectors.json` 里的 `default_partitions`,
两侧单测会因为"向量默认值与代码常量不一致"直接红。

换代号的顺序(与 db_task 的离线扩容流程同形):建新 `*_g<N+1>` → 先滚消费者(gate/scene)
到新代号 → 再滚生产者 → 老 topic 停写后按保留期过期或手工删。因为新旧 topic 名字不同,
两代可以并存,不存在"扩分区那一瞬间寻址错乱"的窗口。

---

## 7. 迁移与灰度顺序

**顺序不能换**,原因是:消费者先切、生产者后切 = 有一段时间两边都在跑;
如果反过来(生产者先切),命令会写进一个还没人 assign 的 topic,直接全丢。

1. **建 topic**:`infra-up`(或止血用的 `infra-kafka-topics`)/ compose 的 `kafka-topic-init`
   建出 `gate-cmd_g1` / `scene-cmd_g1`,各 256 分区。**必须早于任何 gate/scene 启动** ——
   否则 broker 的 auto-create 会把它建成 1 分区,消费端的契约门禁随即让节点起不来
   (这是有意的 fail-closed,不是退化)。
2. **切消费者**:发布新版 gate / scene。此时 `Kafka.DisableLegacyPerNodeTopic` 保持 `false`,
   两条路径同时消费:新 topic 走 assign,老的 `gate-{id}` / `scene-{id}` 仍走 group 订阅。
3. **切生产者**:发布 battle、scene(BindBattle)、scene_manager、player_locator、
   friend / guild / match。**`go/login` 由另一路发布切换**(见 §9),这一步没完成之前不能进 4。
4. **关老路径**:确认老 topic 的 lag 与写入速率归零后,把
   `Kafka.DisableLegacyPerNodeTopic` 置 `true` 并滚一次 gate/scene。
   **P1 到这一步才算彻底关闭** —— 老路径仍是 group 订阅,窗口期内它上面的僵尸争夺照旧存在。
5. **回收**:老 topic 停写后按 retention 自然过期;想早点回收磁盘再手工 `--delete`。

### 混版窗口里会发生什么

| 场景 | 结果 |
|---|---|
| 新消费者 + 老生产者 | 命令走老 topic,被步骤 2 保留的 legacy 路径消费,正常。**P1 在这条路径上仍存在。** |
| 新消费者 + 新生产者 | 走新 topic 的指定分区,正常,P1 不存在 |
| 老消费者 + 新生产者 | **命令丢失**。所以顺序不能倒过来:先滚完全部 gate/scene 再动生产者。 |
| 同一条命令被两条路径重复投递 | 不会:每个生产者只写一个 topic,切换是进程级的,不双写。 |
| 一个 gate 的两代实例同时在场(滚动重启) | 两代各自 assign 同一个分区(assign 允许重复消费),各自按 `target_instance_id` 过滤,只有正主执行。这正是新方案相对 group 模式的关键差别。 |

### 回滚

把 gate/scene 回滚到旧版本之前,**必须先把生产者回滚**(顺序与上线相反),
否则新生产者写的 `*-cmd_g1` 没人读。老 topic 在 retention 内仍然存在,回滚窗口就是它的保留期。

---

## 8. 落码清单

**C++**

- 新增 `node_command_topic.h`(纯寻址契约)、`node_command_route.h`(接配置的落地层)、
  `node_kafka_command_filter.h`(从 handler 拆出的目标过滤,便于单测)
- `node_kafka_command_handler.h`:注册新 topic 的 assign 消费者 + 迁移窗口的 legacy 消费者
- `kafka_partition_assign_policy.h` / `kafka_consumer.{h,cpp}`:分区契约核对、`OFFSET_END`、关 auto-commit
- `kafka_manager.{h,cpp}` / `node.{h,cpp}`:透传策略
- `config.cpp` + `proto/common/base/config.proto`:三个新配置键
- 生产者:`battle_room_manager.cpp`、`player_battle.cpp`

**Go**

- 新增 `shared/kafkacmd`(寻址 + Balancer + 向量对拍)。**没有放进 `shared/kafkautil`**:
  后者装着依赖 sarama ClusterAdmin 的 topic 预建,而 `scene_manager` / `player_locator`
  只需要"往哪个分区发",不该因此被拖进一个 admin 依赖。
- `shared/kafkautil/gate_push.go`、`scene_manager`、`player_locator`、`friend`、`guild`、`match`

**部署**

- `deploy/docker-compose.yml`(建命令 topic;删掉 per-node topic 的预建循环)
- `deploy/k8s/manifests/infra/kafka-topic-init.yaml` + `tools/scripts/k8s_deploy.ps1`
- `deploy/k8s/README.md`(per-node topic 停建的说明)

---

## 9. 已知边界 / 待办

- **`go/login` 未切**:本轮由另一路会话在编辑,`login` 仍然写 `gate-{id}`
  (`go/login/internal/svc/servicecontext.go` 的 BindSession / KickPlayer 两处)。
  legacy 消费路径就是为它留着的;它切完之前不能置 `DisableLegacyPerNodeTopic=true`。
- **`node_allocator` 的最小空闲扫描不变**(本轮刻意不动)。P1 的修复不依赖它改 ——
  即使 `node_id` 立刻被复用,assign 模式下也不会出现"分区被判给前任"。
  `docs/design/node-id-overhaul-plan-20260908.md` 的隔离期方案是另一条线,
  解决的是**发号器 worker id** 的撞号,与本文的路由 `node_id` 不是同一件事(该文 §改造 B
  正是要把两者解耦)。
- **进程重启窗口内的命令仍会丢**(§3.3),依赖各链路已有的上层重试兜底。
  真要做到不丢,需要的是"命令有确认与重投"的语义,那是另一张票。
- **`centre-{id}` 等其它节点类型**:`BuildDefaultKafkaOptions` 是按类型短名派生的,
  这些类型一旦启用 Kafka 命令通道就会自动落到 `<类型>-cmd_g<N>`,
  但对应的 topic 需要在 `kafka-topic-init` 里补一行预建。当前只有 gate/scene 在用。

# DB Task Kafka 分区不可变契约

> 2026-10-16 订正(① 读写分区对齐,S3):「原因」、代码门禁 3 / 6、离线扩容第 3 步、「已知边界」第 2 条按已落码的代码订正;
> 文末新增「分区函数契约」「golden 向量」「滚动对齐步骤与观测」「屏障覆盖范围与剩余缺口」四节。
> 状态:**已落码,未编译、未测试,待 Codex 验证**。与落码规格(`docs/handoff/godb-pipeline-impl-20261015/S3-partition-align-spec.md`)
> 不一致处以代码为准,差异集中写在「分区函数契约」末尾的「与规格的差异」。

## 结论

`db_task_zone_*` 的 partition 数是数据顺序协议的一部分，不是可在线调节的容量参数。
同一 topic generation 内禁止 `CreatePartitions`、生产者动态扩环或“配置追平 broker”。
需要扩容时，必须停写、排空旧 generation，并切到一个新 topic generation。

原因：同一玩家的读（login）与写（C++ scene）都按 CRC-32(player_id 十进制) mod N 选 partition（见下文
「分区函数契约」；原文「login 生产者按 `player_id` 一致性哈希」从来没生效过，2026-10-16 订正），db 成功写入后又把
`(topic, player_id, msg_type, origin_partition, offset+1)` 作为永久 applied cursor。
Kafka 扩分区会重映射一部分 key；新 partition 的 offset 与旧 partition 不可比较。
把它们直接比较会回档，不比较而继续写也无法证明顺序，因此只能 fail-closed。

## DBTask 操作枚举契约（2026-10-22）

`proto/db/db_task.proto` 的 `DBTask.op` 使用共享 `DBOp`：`DB_OP_UNSPECIFIED=0`、
`DB_OP_READ=1`、`DB_OP_WRITE=2`。C++ scene 存盘、Go login 读请求、DB 消费者、
重试和存盘补投均使用同一枚举。零值和未知编号不得进入排序锁、选库或 SQL；Kafka
非法任务和重试收据沿既有持久死信路径保全原始字节，持久隔离成功后才允许确认消费。
批合并不能跨过未知操作合并同一玩家、同一表的写。

这是上线前对原 `string op=3` 的统一替换，字段号仍为 3，wire type 从 LEN 改为 VARINT，
不支持新旧协议混跑。旧字符串任务在新绑定中不会成为 READ 或 WRITE，必须被拒绝；
开发环境切换前应排空旧 Kafka 任务、Redis 重试及父屏障内的原始补投意图，不能仅更新
消费者。未决写不能直接丢弃，本次源码改造不清理运行环境数据。上线状态以
[存盘围栏设计](db-write-fencing-mysql.md)文末为准。

操作字段单独的未压缩 protobuf 编码为 READ=`18 01`、WRITE=`18 02`，均 2 字节；
原字符串分别为 6、7 字节。此处只记录编码尺寸，不据此宣称吞吐或延迟改善。Go 的 handler
map 使用枚举键；日志和指标仍映射成原有 `read/write/unknown`，分区审计保持
`read/write/other`，避免改变监控契约。Java 没有该 Kafka 生产/消费路径，但其内部协议副本
须通过正式 ContractSync 同步并重生成编译。

同类字段只读排查：Kafka 的 SceneCommand、PlayerLifecycleCommand 已使用枚举，GateCommand
使用全局整数事件号。交易协议的 `TradeCommerceWorkflow.stage`、`TradeCommerceLeg.role`
是后续可单独评估的封闭集合，但属于持久化及恢复契约；人民币寄售 `asset_kind` 和市场/竞拍
视图 `state` 还涉及客户端或外部商城 JSON，需另批明确映射并同步两版与客户端。本次不改这些
合同。`DBTask.msg_type` 是 protobuf 全限定类型名，认证渠道是可扩展注册名，标识、文案和
开放文本不能仅因使用 string 就转成枚举。

## 代码门禁

1. `kafkautil.EnsureTopics` 只创建缺失 topic，绝不原地扩分区；已存在 topic 的
   broker/config partition 数不等时启动失败。
   创建成功或返回 AlreadyExists 后，metadata 可能延迟可见；仅对这种缺失状态按
   100ms 间隔等待，10 秒单调截止内复查，查询错误直接失败。出现后仍须验证精确
   分区数及 marker，超时不得继续更新 retention。单次 Sarama 查询受原 socket
   超时约束，10 秒是轮询预算，不是强制取消在途 RPC 的硬截止。
2. 首次成功校验时创建一个 broker 侧不可变 marker topic，名称包含 data topic
   的 SHA-256 和 partition 数。即使运维先在 broker 扩分区、再把配置改成相同数，
   旧 marker 仍会让启动失败。
3. login producer 构造时再次核对实际 partition 集合必须精确为 `[0,N)`；运行中
   发现 drift 后永久 fence 本进程的 DBTask 写入（`KeyOrderedKafkaProducer.routingFenced`，置位不可逆）。
   CRC-32 mod N 的映射随 N 变化，漂移只 fence，不做任何重映射（2026-10-16 订正：原文「不动态修改 hash ring」
   所说的哈希环从来没生效，已随 `pkg/consistent` 一起删除）。
4. db consumer 的 rebalance/claim 若出现配置范围外 partition，直接拒绝 claim，
   不按需创建“看起来能消费”的 worker。
5. retry payload 保留原始 partition/offset；db batch coalescer 只对同 partition、
   已知 seq 的写按最大 seq 合并。跨 partition 写进入 ordering guard/DLQ。
6. 分区函数本身也是契约（2026-10-16 起，见「分区函数契约」）：Go 侧唯一来源是 `go/shared/kafkautil`
   （`DBTaskPartition` / `NewDBTaskPartitioner`）；C++ 在 `KafkaProducer::createProducer` 显式钉
   `partitioner=consistent_random`；两侧共读同一份 golden 向量；go/db 在消费端只读地核对每条消息
   （`db_task_producer_partition_total`），写不符是 critical 告警。

## 离线扩容流程

假设从 generation 1 / 10 partitions 扩到 generation 2 / 20 partitions：

1. 备份配置，停止所有会产生 DBTask 的 login/业务写入入口。C++ scene 是 DBTask 的
   另一个生产者（`SavePlayerToRedis` 按 home_zone 写存盘），停写必须包含全部 scene
   节点，不能只停 login。
2. 保持旧 db consumer 运行，确认旧 topic consumer lag 为 0。
3. 确认旧 generation 的 Redis retry `ready`、`processing` 均为 0(2026-10 起 `processing` 按 db 实例拆开:
   旧版共享列表 `kafka:retry:processing:{topic}` 加上 `kafka:retry:instances:{topic}` 里每个实例名对应的
   `kafka:retry:processing:{topic}:{实例名}`,都要为 0)；逐条处置 dead
   queue，不能把未决任务遗留到旧 namespace。
   2026-10-16 起还有按原始分区拆开的 ready 列表 `kafka:retry:queue:{topic}:p{N}`（④ 重试只交给分区属主，
   `ServerConfig.Kafka.RetryRouting: owner` 时写入；键名见 `go/db/internal/kafka/retry_routing.go` 的
   `retryPartitionReadyKey`），同样都要为 0：用 `redis-cli --scan --pattern 'kafka:retry:queue:{topic}:p*'`
   列出后逐个 `LLEN`；或在 `go/db` 下跑 `go run ./cmd/retry_requeue -f etc/db.yaml -zone <zone> -check`（只读盘点，
   有非空的按分区 ready 列表时以退出码 3 结束）。已落码，未编译、未测试，待 Codex 验证。
   2026-10-17 更正（R7 复核）：`-zone` 必填：R7 之前缺省取配置文件里的 ZoneId，漏了会盘点别的 zone 的 topic 并以 0 退出（fail-open）；现在不给 `-zone` 直接以退出码 1 退出，一条不动（`resolveTarget`）；
   代次取 `-f` 配置的 `TopicGeneration`，这一步查的是旧 generation，配置已改成新代时加 `-topic-generation <旧代>`。
   只有退出码 0 才算通过：目标看起来没在用（登记表为空、所有重试列表都是 0，多半是参数指错）以 4 结束，
   登记表里还有登记为 owner 模式的实例以 5 结束，参数 / 配置 / Redis 出错以 1 结束（`go/db/cmd/retry_requeue/main.go` 的
   `exitTargetUnused` = 4、`exitOwnerInstances` = 5、`exitError` = 1，p* 非 0 是 `exitResidual` = 3）。已落码，未编译、未测试，待 Codex 验证。
4. 停止旧 db consumer。此时 MySQL 已包含旧 generation 的最终状态。
5. 在 login 与 db 配置中同时设置 `TopicGeneration: 2`、`PartitionCnt: 20`、
   `InitialPartition: 20`（login），并在同一次提交里把 C++ 的
   `bin/etc/base_deploy_config.yaml` 设为 `DbTaskTopicGeneration: 2`。不要 ALTER
   generation 1 的 topic。
   - 三方必须相等：C++ 与 Go 同一条命名规则（`cpp/libs/services/scene/player/constants/player.h`
     `GetDbTaskTopic` ↔ `DbTaskTopicForGeneration`）：`<= 1` 为 `db_task_zone_{zone}`，
     `>= 2` 为 `db_task_zone_{zone}_g{N}`；缺键 / 0 当 1。注意与审计 topic 第一代就带
     `_g1` 不同。
   - 漏改 C++ 不报任何错：scene 会继续写已排空、没人消费的旧 topic，存盘静默积压。
     本机 `start_game.ps1` 启动前核对三方，不一致即中止；K8s 的 node ConfigMap 由
     `k8s_deploy.ps1` 从 `go/db/etc/db.yaml` 的 `ServerConfig.Kafka.TopicGeneration` 生成
     `DbTaskTopicGeneration`（生成期同时核对 login），契约测试
     `tools/scripts/tests/k8s_deploy_contract.tests.ps1` 钉住三方相等。
   - `DbTaskTopicGeneration` 只在进程启动时读入；换代必须重启全部 scene 节点。
6. 先启动 db，再启动 login，最后启动 scene。任一侧启动门禁失败都不开放写流量。
   scene 必须晚于 db：broker 开着 auto.create，scene 抢先发消息会把新代 topic 自动建成
   1 分区，db 的分区契约校验随即永久失败。
7. 验证新 topic 名为 `db_task_zone_{zone}_g2`、20 partitions、marker 存在；执行
   单玩家连续写与故障重试 smoke，确认 applied cursor 使用新 topic namespace。
8. 恢复写入口并观察 lag、DLQ、producer fence 告警。旧 topic 与 marker 至少保留
   一个审计/回滚窗口，不立即删除。

回滚也必须先停写并排空新 generation，并把 login、db、C++ `DbTaskTopicGeneration`
三方一起切回。已有新写时直接把配置切回旧 topic，会形成
两条独立顺序历史，禁止这样操作。

## 已知边界

- 本协议关闭“同名 topic 在线扩分区”，不实现跨 generation 的在线双写/epoch 交接。
- Kafka 集群迁移同样必须使用停写/排空流程；MirrorMaker 复制 backlog 不等于保留
  同一分区映射与 applied cursor 的因果关系（2026-10-16 订正：原文写的「一致性哈希」从来没生效）。
- 删除 marker 是破坏正确性门禁的运维动作，不属于正常清理。

---

## 分区函数契约（2026-10-16，① 读写分区对齐）

> 状态：已落码，未编译、未测试，待 Codex 验证。Go 与 C++ 代码已随 `a7c510d65` / `592d7e718` / `0f6690604` 等快照提交入库；
> 告警文件 `deploy/k8s/db-task-partition-alerts.yaml` 随 `e76f6f747` 入库。验证与上线清单见 [上线待做](../ops/godb-pipeline-launch-todo.md) §5。

### 契约

- **key** = 玩家 id 的无符号十进制 ASCII，无前导零。Go：`kafkautil.DBTaskKey`（`strconv.FormatUint(id, 10)`）与免分配的
  `kafkautil.AppendDBTaskKey`；C++：`DbTaskKafkaKey`（`cpp/libs/services/scene/player/constants/player.h`，`std::to_string(uint64_t)`）。
  两者逐字节相同；key 的字节就是契约的一部分，改了它同一玩家的写就会换分区。
- **partition** = CRC-32/IEEE(key) mod N，按 uint32 无符号取模。这就是 librdkafka 默认分区器 `consistent_random` 对非空 key 的算法
  （`rd_crc32(key) % partition_cnt`）；`sarama.NewConsistentCRCHashPartitioner` 是它的 Go 对应物。
  Go 侧唯一实现：`go/shared/kafkautil/dbtask_partition.go` 的 `DBTaskPartition`（纯函数）与 `NewDBTaskPartitioner`
  （就是 `sarama.NewConsistentCRCHashPartitioner`，写成函数而不是可变包变量；编译期断言它满足 `sarama.PartitionerConstructor`）。
- **空 key 不在契约内**：librdkafka 对空 key 随机分区，sarama 对非 nil 的空 key 落 0 号分区，两边不一致。
  `DBTaskPartition` 对空 key 返回 `-1` 加 `ErrEmptyDBTaskKey`，对 N ≤ 0 返回 `-1` 加 `ErrInvalidDBTaskPartitionCount`；两侧生产者都不发空 key。
- **topic**：`kafkautil.DBTaskTopicPrefix = "db_task_zone_"`，`kafkautil.IsDBTaskTopic` 只用来识别 topic 类别。命名规则本身仍是
  go/db 与 go/login 各自的 `config.DbTaskTopicForGeneration` ↔ C++ 的 `GetDbTaskTopic`（`player.h`）；
  `go/login/internal/kafka/command_partitioner_test.go` 的 `TestDBTaskTopicNamingMatchesContractPrefix` 用真实命名函数把前缀绑住。
- **换函数 = 换 TopicGeneration**（走上面的离线扩容流程）。在线换函数（librdkafka 升级改了默认值、有人加了 partitioner 配置、
  Java 化之后想换 murmur2）会让同一玩家的写在新旧生产者之间换分区，go/db 比较游标里记录的分区时判 `orderingIncomparable`，写被隔离进死信。

### 两侧实现（截至 2026-10-16 工作区）

| 角色 | 位置 | 要点 |
|---|---|---|
| login 读（主生产者） | `go/login/internal/kafka/key_ordered_producer.go` 的 `newDBTaskProducerConfig` | 在 `sarama.NewClient` 之前设 `Producer.Partitioner = kafkautil.NewDBTaskPartitioner`（`NewSyncProducerFromClient` 用的是 client 上的配置）。旧代码没设，用的是 sarama 默认 `NewHashPartitioner`（FNV-1a），它还会覆盖 `ProducerMessage.Partition`，所以旧的一致性哈希环与「不可用分区重映射」从来没生效，已整段删除（含 `go/login/internal/logic/pkg/consistent`） |
| login 读（发送校验） | 同文件 `SendTask` / `SendTasks` | Kafka key 必须等于 `kafkautil.DBTaskKey(task.Key)`，否则直接返回 error、不发送（fail-closed）；不再填 `Partition`，分区由分区器算、sarama 回填 |
| login plainProducer | 同文件 `SendToTopic` / `SendToTopicPartition`；`command_partitioner.go` 的 `NewCommandAwarePartitioner` | 两个通用口子都拒绝 `IsDBTaskTopic` 为真的 topic（"refusing to send db_task topic … through the plain producer"）；万一有绕过的路径，`NewCommandAwarePartitioner` 对 `db_task_zone_` 前缀也返回契约分区器；其它非命令 topic 仍是 `sarama.NewHashPartitioner` |
| C++ scene 写 | `cpp/libs/engine/infra/messaging/kafka/kafka_producer.cpp` 的 `createProducer` | 显式 `conf->set("partitioner", "consistent_random", errstr)`，值等于 vendored librdkafka 2.14.0 的默认值，运行时零变化；设置失败只打 `LOG_ERROR`（"failed to pin partitioner=consistent_random"），不阻止建实例（否则本节点所有 Kafka 发送都会失败） |
| C++ scene 写（key） | `cpp/libs/services/scene/player/system/player_lifecycle.cpp` 的 `SavePlayerToRedisImpl` | `playerIdStr = DbTaskKafkaKey(playerId)`；发送经 S5a 的 `DbTaskDispatchSystem::SendDbTask`（`KafkaProducer::sendTracked`），不传分区 |
| 压测写 | `go/db/cmd/data_stress/main.go` | key 用 `kafkautil.DBTaskKey`，分区器用 `kafkautil.NewDBTaskPartitioner`（旧代码走 FNV，与 C++ 不同分区） |

### 生产者清单（2026-10-15 规格静态核对）

db_task 只有三个生产者：C++ scene（写，`SavePlayerToRedisImpl` 是唯一生产点）、login（读）、`go/db/cmd/data_stress`（写，压测工具）。
C++ 其余 Kafka 发送点（battle 的 gate / scene 命令 topic 显式传分区、对局结果、审计、异常告警 topic）都不是 db_task；
其余 Go 生产者、`tools/merge_zone`、robot、Java 都不发 DBTask。`KafkaProducer` 不读任何外部 librdkafka 配置，所以源码核对覆盖了全部
C++ 发送路径；**以后若加「从 yaml 透传 librdkafka 参数」的能力，必须同时禁止覆盖 `partitioner`。** 线上实际分布由下文「滚动对齐步骤」第 1 步的抽样证实。

### 消费端写分区审计（go/db，只观测）

- `go/db/internal/kafka/producer_partition_audit.go` 的 `observeProducerPartition`，在 `ConsumeClaim` 解码成功之后、`ensureWorker` 之前调用一次。
  判定 `classifyProducerPartition`：消息或任务为空、分区数非正 → `invalid`；Kafka key 不等于 `AppendDBTaskKey(task.Key)`（含 nil / 空 key）→
  `key_mismatch`；消息所在分区不等于 `DBTaskPartition(key, N)` → `partition_mismatch`；否则 `ok`。
- 指标 `db_task_producer_partition_total{op, verdict}`（`go/db/internal/metrics/metrics.go`，`CountProducerPartition`）：op 取
  `read | write | other`（`DBTask.op` 不是 read / write 的记 other），verdict 取 `ok | partition_mismatch | key_mismatch | invalid`；
  进程启动时以 0 建出全部 12 条序列，`increase` 能抓到第一次。label 是封闭集合，不含 player_id。
- 日志每进程每种 op 最多 20 条（`producerPartitionLogBudget`）：写 / other 不符是 ERROR
  「DATA-ORDERING: db_task producer violated the partition contract」，读不符是 INFO「db_task read outside the partition contract (old login?)」。
- **只计数、不拒绝**：不改 ack、游标、重试。拒写会丢存盘，拒读会打断滚动窗口里的旧 login。遗留重试载荷照旧按原规则 fail-closed；
  重试任务不经 `ConsumeClaim`，不在观测范围内；计数按每次投递算，重投递会重复计。
- **不变量**：N 取的是本消费者的 `partitionCount`，前提是「一个 handler 只消费一个 topic」。以后一个 handler 消费两代 topic 时必须按
  `msg.Topic` 取 N，否则写会被误判 `partition_mismatch` 并触发 critical 告警。

### 告警与 apply 时机

`deploy/k8s/db-task-partition-alerts.yaml`（PrometheusRule，组 `db-task.partition-contract`，interval 30s）：

| 告警 | 表达式 | 级别 | 含义 |
|---|---|---|---|
| `DbTaskWritePartitionContractBroken` | `sum by (namespace) (increase(db_task_producer_partition_total{op="write",verdict!="ok"}[10m])) > 0`，for 0m | critical | 有生产者破坏写的分区契约，同一玩家的写会被判 `orderingIncomparable` 隔离进死信；按 go/db 日志「DATA-ORDERING …」里的 topic / partition / kafkaKey 找写入方，同时看 `db_task_dead_letter_total{reason="ordering_incomparable"}` |
| `DbTaskReadPartitionMismatch` | `sum by (namespace) (increase(db_task_producer_partition_total{op="read",verdict!="ok"}[30m])) > 0`，for 30m | warning | 还有旧版 login 在跑，或 login 回滚到了旧镜像；这些读没有读后写屏障 |

- op="other" 不设告警（是否要设，待负责人定）。
- **apply 时机**：写规则在 go/db 观测上线之后即可 apply；读规则必须等 login 全部滚到按契约分区的版本之后——滚动期间旧版 login 的读按
  FNV 分区，必然不符，会误报。先上写规则时把读规则注释掉再 apply，login 滚完后放开、重 apply 同一文件。
  安装 `kubectl apply -n observability -f deploy/k8s/db-task-partition-alerts.yaml`，回滚 `kubectl delete -n observability -f` 同一文件；
  仓库不部署 Prometheus，由负责人手工 apply（AGENTS.md §9）。抓取前提：go/db 的 `:9160`（db ConfigMap 的 `MetricsListenAddr`）有
  ServiceMonitor / PodMonitor。登记见 `deploy/k8s/README.md`「go/db 存盘管线告警」与上线待做 2.5.8。

### 与规格的差异（以代码为准）

1. 上线抽样复核器（`go/shared/kafkautil/dbtask_partition_test.go`）不再按子串判 op：`parseConsoleConsumerLine` 先经 `unescapeKafkaBytes`
   把 `\xNN` 反转义回原始字节，再 `proto.Unmarshal` 成 `DBTask`，op 与 key 都取自解码结果；key 判定与 go/db observer 同一口径
   （`kafkaKey == DBTaskKey(task.Key)`，`auditDumpVerdict`）。`TestDBTaskPartitionAuditDump` 的失败条件在规格之外加了「一条写都没有」与
   「op 为 other 的行数 > 0」，文件是 UTF-16 时直接失败并提示改用 utf8NoBOM 抓取（复核意见）。
2. `TestDBTaskPartitionDistribution` 是信息性检查：打印分布表，只有偏离超过 30% 才失败（规格写的是 10% 硬断言；失败说明 CRC 对存量 id 有偏斜，写侧早已如此，回报负责人，不改契约）。
3. `command_partitioner_test.go` 里名为 `"db_task"` 的 topic 仍断言走默认 FNV（复核原文要求改断言 CRC），理由是全仓不存在叫 `db_task` 的 topic、放宽前缀会误伤无关 topic；**这条偏离待负责人确认**。
4. C++ 写侧的发送调用已由 S5a 换成 `DbTaskDispatchSystem::SendDbTask`（规格写的是 `KafkaProducer::send`），key 字节与分区不变。
5. C++ 用例 `VendoredDefaultPartitionerIsConsistentRandom` 额外断言 `rd_kafka_version() == RD_KAFKA_VERSION` 并用 `RecordProperty` 记下运行时版本串：
   测的是运行时加载的 `rdkafka.dll`，「运行时库就是 vendored 版本」因此成了被测条件（复核意见）。
6. 冷读冒烟删键前「确认 W 真的到了 broker」改为在 Kafka 侧确认（复核意见，见下文第 2 步）：scene 日志
   "[SavePlayerToRedis] Player … saved to Redis, DB write tasks enqueued" 在发送失败时也会打印，`send` 成功也只说明进了 librdkafka 本地队列。
   **2026-10-17 更正（R7 复核）**：「发送失败时也会打印」这处已修。`cpp/libs/services/scene/player/system/player_lifecycle.cpp` 的
   `SavePlayerToRedisImpl` 里的 `publishDbTasks` 按是否全部入队分两种文案：两张子表的 DBTask 都同步入队，才打原文 `saved to Redis, DB write tasks enqueued`；
   没全入队时先有逐条的 ERROR `[SavePlayerToRedis] Kafka send failed: …` 与汇总 ERROR `not every DB write task was queued to Kafka`，
   再打 INFO `saved to Redis, DB write tasks NOT all queued (see the ERROR above)`，不再出现 `DB write tasks enqueued`。
   全部入队也只说明进了 librdkafka 本地队列、不等于 acks=all，所以第 2 步仍以 Kafka 侧的 end offset 与抽样复核为准，不变。
   已落码，未编译、未测试，待 Codex 验证。
   **2026-10-20 更正（存盘日志降级）**：成功那行 `saved to Redis, DB write tasks enqueued` 与 `HandlePlayerAsyncSaved: Saving complete` 已从 INFO 降为 DEBUG
   （每次真写盘都打两条，会把 INFO 刷满）；没全入队时那行 INFO `DB write tasks NOT all queued (see the ERROR above)` 已删，只剩逐条 ERROR `Kafka send failed`
   与汇总 ERROR `not every DB write task was queued to Kafka (topic=…, home_zone=…, owner_epoch=…, save_seq=…)`，原 INFO 里的字段并进了汇总 ERROR。
   本地 `bin/etc/base_deploy_config.yaml` 默认 `LogLevel: 2`（muduo 枚举，2=INFO、1=DEBUG），要看成功那行须临时调到 `1` 并重启 scene。
   第 2 步本来就以 Kafka 侧 end offset 为准，判据不变。已落码，未编译、未测试，待 Codex 验证。

---

## golden 向量

- 文件：`go/shared/kafkautil/testdata/dbtask_partition_vectors.json`，Go 与 C++ 共读，结构 `schema` / `check` / `cases`。
  `check` 是 CRC-32/IEEE 标准自检值：`"123456789"` → 3421780262（0xCBF43926）。`cases` 共 45 条：11 个 key（`"0"`、`"1"`、`"42"`、`"1001"`、
  `"10001"`、`"123456789"`、`"7311864093319258112"` 等三个 19 位 id、`"9223372036854775807"`、`"18446744073709551615"`）× N ∈ {7, 10, 16, 20}，
  外加 `{"key":"123456789","n":2147483647,"expect_partition":1274296615}`：这一条区分三种取模写法——无符号取模得 1274296615，
  sarama referenceAbs 得 1274296614，有符号取模取反得 873187034。
- 读取方（每条都必须逐条相等）：
  - Go：`TestDBTaskPartitionCheckValue`；`TestDBTaskPartitionGoldenVectors` 用四种算法（`DBTaskPartition`、独立重算 `crc32.ChecksumIEEE % n`、
    `sarama.NewConsistentCRCHashPartitioner`、`NewDBTaskPartitioner`）对每条断言，并要求 ≥45 条；
  - C++：`cpp/tests/kafka_command_test/kafka_partition_contract_test.cpp` 的 `DbTaskPartitionContract.LibrdkafkaConsistentRandomMatchesSharedVectors`
    直接调 `rd_kafka_msg_partitioner_consistent_random`，并用 `check.crc32 % N` 间接核对自检值、要求覆盖上述五个 N。
- 数值来源：规格附录作者与复核者各用独立手写 CRC-32 算出且一致，**没有用机器算过**。必须由 Codex 用 Go `crc32`、sarama、librdkafka
  三方机械复核；不一致时以机器结果回报，**不直接改表**。
- 相关用例：`TestNewDBTaskPartitionerMatchesDBTaskPartition`（≥1e5 个 key、多个 N，分区器与契约函数逐个相等）、`TestDBTaskKeyEncoding`、
  `TestDBTaskPartitionRejectsEmptyKeyAndBadCount`、`TestIsDBTaskTopic`；C++ `DbTaskPartitionContract.KafkaKeyMatchesGoFormatUint`
  （`DbTaskKafkaKey` 与 Go `FormatUint` 逐字节相同）、`…VendoredDefaultPartitionerIsConsistentRandom`、`…ExplicitPartitionerOnGlobalConfReachesDefaultTopicConf`。

---

## 滚动对齐步骤与观测

写侧不改一行、不迁移分区、不换代；只把 login 的读搬到写所在的分区。各步之间没有硬依赖，login 与 go/db 谁先上都安全。
**执行人**：生产与预发上的抽样、kubectl apply 只由负责人执行；Codex 只在本机或负责人明确指定的 dev 环境执行（AGENTS.md §9）。
每一步的命令与通过标准见[上线待做](../ops/godb-pipeline-launch-todo.md) §5。

| 步 | 做什么 | 看什么 | 回滚 |
|---|---|---|---|
| 0 | Codex 本机编译与单测（Go 三个模块、C++ 串行 `/m:1`、golden 三方复核） | 全绿；golden 不一致时保留输出回报，不改表 | — |
| 1 | **只读抽样门槛**（每个 zone，在 login 切换之前）：console consumer 按分区抽最近 M 条，离线跑 `TestDBTaskPartitionAuditDump` | 写的非 ok 计数为 0、解析失败为 0、抽样里有写；读不符比例记作基线（旧 login 下推断约 1−1/N） | 无需；不满足就停在这一步，按输出的不符行找出不守契约的写入方 |
| 2 | staging 按生产顺序走一遍，做冷读冒烟（下文） | 同上；冒烟全部有结论且通过 | 回滚 staging |
| 3 | **生产 login 逐副本滚动**（核心切换） | 读不符比例单调降到 0、read ok 上升（go/db 观测已上线时）；EnterGame 成功率、`entergame_total{result="preload_failed"}` 速率、`dataloader_preload_callback_wait_seconds`、`db_task_stage_seconds{op="read"}` 与基线持平；按分区 lag 无持续堆积；login 日志没有 "producer routing fenced" | 回滚 login 镜像，读回到 FNV（即旧状），无数据要清理。**前置**：回滚目标提交必须包含 `d5cbb8a8e`（G0 自锁修复），用 `git merge-base --is-ancestor d5cbb8a8e <commit>` 核验 |
| 4 | go/db 观测随下一次例行 go/db 发版上，不单独安排（遵守[瓶颈文档](db-task-pipeline-bottlenecks.md) §4.5 的发版红线） | 写的 `verdict!="ok"` 恒为 0，write ok 随在线增长，至少看 2 个存盘周期（≥10 分钟） | 回滚 go/db 镜像 |
| 5 | 告警：写规则在第 4 步之后 apply，读规则在第 3 步全部滚完之后放开 | 写规则静默 | `kubectl delete -n observability -f deploy/k8s/db-task-partition-alerts.yaml` |
| 6 | C++ 改动随下次 scene / battle 例行发版 | 写不符恒为 0；scene 启动日志没有 "failed to pin partitioner" | 回滚 scene / battle 镜像，运行时零变化 |

**第 1 步的抽样**（以 zone 1、N=10、每分区最近 M=500 条为例；本机用 `docker exec kafka …`，K8s 用
`kubectl -n <infra> exec kafka-0 -c kafka -- /bin/sh -c "KAFKA_HEAP_OPTS='-Xms16m -Xmx64m' …"`——exec 进 broker 跑工具必须覆盖成小堆，
否则工具继承 broker 的大堆参数、与 broker 叠在同一个 memory limit 里会把 kafka-0 撑到 OOMKilled；工具报 OutOfMemoryError 时按需调大 -Xmx，但不得去掉覆盖）：

```bash
# end / begin 偏移,每行 topic:partition:offset;from = max(begin, end-M),count = end-from,count 为 0 就跳过
/opt/kafka/bin/kafka-get-offsets.sh --bootstrap-server <B> --topic db_task_zone_1 --time -1
/opt/kafka/bin/kafka-get-offsets.sh --bootstrap-server <B> --topic db_task_zone_1 --time -2
# 每个分区一次,只捕获 stdout;带 --partition 是 assign 模式:不入组、不提交位点、不触发再平衡
/opt/kafka/bin/kafka-console-consumer.sh --bootstrap-server <B> --topic db_task_zone_1 --partition <p> --offset <from> \
  --max-messages <count> --timeout-ms 15000 \
  --property print.partition=true --property print.offset=true --property print.key=true --property print.value=true \
  --value-deserializer org.apache.kafka.common.serialization.BytesDeserializer --consumer-property enable.auto.commit=false
```

- 只捕获 stdout（console consumer 的 stderr 摘要混进 dump 会让解析失败）：在容器内重定向落盘后 `kubectl cp` 拷出，或在 pwsh 7 里用
  `Out-File -Encoding utf8NoBOM` 写文件。Windows PowerShell 5.1 的 `>` 写 UTF-16，复核器会直接失败。
- Kafka 4.x 下 `--property` 是否提示弃用、需不需要改 `--formatter-property`，以钉定镜像 `apache/kafka:4.3.1` 上的实测为准（未实测）。
  kcat 的 `%s` 输出原始字节，会被载荷里的换行打断，不能用来判定 op；只有 kcat 时只打 `%p %o %k` 做分区核对。
- 离线复核：`$env:DBTASK_AUDIT_DUMP='<dump>'; $env:DBTASK_AUDIT_PARTITIONS='10'`，在 `go/shared` 下
  `go test ./kafkautil/ -run TestDBTaskPartitionAuditDump -count=1 -v`；用完删掉临时文件与两个环境变量。
  失败条件（任一即失败，fail-closed）：有解析失败的行；有效行数为 0；一条写都没有；有 op 为 other 的行；有写违反契约。
  **所以每个分区的抽样量要足以覆盖写**——只抽到读的分区证明不了写侧。

**冷读冒烟**（第 2 步，staging；本机演练同法。scene 开 `STRESS_TEST_PROBE`，10 个玩家）：

1. 优雅停掉 go/db；robot 登录、改状态、下线。
2. **在 Kafka 侧确认写 W 已到 broker**：登出前记下该玩家所在分区 p = `DBTaskPartition(key, N)` 的 end offset（`kafka-get-offsets.sh --time -1`）；
   登出、看到存盘日志后确认这个 end offset 增长了（2026-10-20 起存盘日志是 DEBUG，scene 不调到 `LogLevel: 1` 时不等日志，直接按 end offset 增长判断）；再用 console consumer `--partition p --offset <旧 end>` 抽到 key=<id>、op=write 的那条
   （复用上面的抽样复核器解析）；同时确认该玩家没有 "Kafka send failed" 日志。以上都满足才允许删键。
3. 从父键解出 W 的 `test_seq`；删掉父键与两个子表键（父键见 login dataloader `go/login/internal/logic/pkg/dataloader/data_loader.go` 的 `buildParentKey`，子表键见 `go/db/internal/kafka/key_ordered_consumer.go` 的 `buildCacheKey`，格式 `{MsgType}:{key}`），不要删 `consumer:applied*` 键。
4. robot 再次登录（login 日志出现 "LoadSync: sending … DB read tasks"），2 秒内拉起 go/db。
5. 判定：预加载超时（`entergame_total{result="preload_failed"}` 加 1、日志出现 "task result wait timed out"）判为不确定、重跑；进场成功后解码父键里的
   `player_database.stress_test_probe.test_seq`，等于 W 判通过，小于 W 判失败。至少 5 次有结论且全部通过。

**行为变化**（发布说明与 runbook 都要写；负责人 2026-10-13 已接受第 1 条）：

1. db 积压让冷读的排队超过 login 预加载等待时（`entergamelogic.go` 的 `dispatcherTaskTTL` = 8s，dispatcher 每 1s 扫一次、到期前再补一次 LPOP，
   实际约 8–9s），以前是静默读到旧档，现在是进场失败：gate 推 tip 3023（`kEnterSceneFailed`），EnterGame 的 gRPC 应答此前早已返回成功。
   观察 `entergame_total{result="preload_failed"}`、`entergame_failure_notify_total{stage="preload"}`、`dataloader_preload_callback_result_total{result="failed"}`；
   排障看日志 "task result wait timed out"、按分区的 lag 与 `db_task_stage_seconds{op="read"}`。不新增 timeout label。
2. `SendTask` / `SendTasks` 在 key 不等于 `DBTaskKey(task.Key)` 时直接拒发。
3. `SendToTopic` 与 `SendToTopicPartition` 都拒绝 `db_task_zone_*` topic。
4. 删掉 safego 点位 `login.kafka_check_unavailable_partitions`，stats 日志去掉 `unavailablePartitions` 字段。
5. 去掉「所有分区都被标不可用时立即报错」的快速失败：Kafka 全挂时每次发送都要等 sarama 跑完重试与网络超时，预加载池更容易打满
   （`entergame_total{result="pool_saturated"}`）。删掉的不可用分区代码里有 G0 已修过的自锁路径，之后失败分支里不再有二次加锁的代码；
   `key_ordered_producer_lock_test.go` 的 `TestSendTaskLeaderNotAvailableDoesNotDeadlock` 保留，防止有人再引入。

---

## 屏障覆盖范围与剩余缺口

go/db 的读后写屏障（`processTaskBatch`：反向那一遍里读遇到同 (key, msgType) 就切断写合并，正向那一遍按到达顺序串行 `handleTask`）只在
**同一分区**内成立。对齐之后，同一玩家的读与写进同一 worker、同一子 shard，按 offset 串行，这套代码一行不用改就生效。
`key_ordered_consumer.go` 里 `processTaskBatch` 的文档注释与本节同口径。

**前提（三条同时成立）**：

1. 两侧生产者都守分区契约（`kafkautil.DBTaskPartition`）。
2. 两侧 topic 名相同：login 的 `DbTaskTopicForGeneration(登录 zone)` 等于 C++ 的 `GetDbTaskTopic(home_zone)`；访客天然不满足。
3. 读发出时，本玩家的写已经进了 Kafka。旧会话还在断线租约内、尚未存盘，或写还滞留在 librdkafka 本地队列时，读会排在写之前。

**不在覆盖范围内的五个缺口**：

1. 访客或合服后跨 topic：读走登录 zone 的 topic，写走 home_zone 的 topic。
2. 写已转进 Redis 重试队列，读不会等它。
3. 重试被非属主实例执行。④（重试只交给分区属主）落码后，`RetryRouting: owner` 时收窄；缺省 `shared`（`go/db/etc/db.yaml`）下仍成立。
4. 父键缺失、子表缓存还在：login（`ensure_player_all_data_async.go`）逐张子表判断，命中的直接用缓存、不发读；**部分命中时拼出来的是新旧混合档**。
5. 再平衡排空超时后旧实例仍在执行的写（G1，见[瓶颈文档](db-task-pipeline-bottlenecks.md) §4.4）。

**放大效应**：`publishDBReadResult` 会把读结果回写子表缓存（`{MsgType}:{key}`，TTL 取 `ServerConfig.RedisClient.DefaultTTLSeconds`：`go/db/etc/db.yaml` 与 K8s db ConfigMap 均为 3600s；配置 ≤ 0 时 `cacheTTL` 才回落 24h），缺口 1 / 2 / 3 / 5 里读到的旧值
因此会延续到缓存过期，并经缺口 4 在后续冷登录里被直接复用。

**触发条件**：C++ 无 TTL 地 SET 父键；本机 Redis 是 `noeviction`（`deploy/docker-compose.yml`），K8s 的 `redis.yaml` 不设 `--maxmemory`，所以现网不存在驱逐。
父键全量丢失、FLUSH、运维删键会走冷读，屏障有效；AOF 部分回退时父键回到上一次 SET 的旧值，走 EXISTS 快路径不发读，与屏障无关，属于 Redis 持久化的范畴。
结论：这是同 zone 内的正确性补洞，现网触发频率低，**不能写成「读后写一致」**。

**与 MySQL 条件写的分工**：条件写（[db-write-fencing-mysql.md](db-write-fencing-mysql.md)）挡的是旧写覆盖新写，不校验读；本契约让同 topic 内的读排在写之后，
不覆盖跨 topic。两者互补，不能互相替代；条件写设计稿提到「读到旧档」时链接本节。

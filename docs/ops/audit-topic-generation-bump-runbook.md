# 审计 topic 加分区与离线换代 Runbook(player_snapshot / transaction_log)

> 状态:2026-10-13 随 G2a「快照审计管道扩容」落地;2026-11-06 改为「加分区原地做、减分区才换代」(缺口清单第 15 条,
> [审计流水链路的水平扩展](../design/audit-pipeline-horizontal-scale-20261106.md) §2.A)。代码与本文**未经运行验证**,验证项见 §9。
> 适用:`player_snapshot_topic` / `transaction_log_topic` 改分区数;以及与它们相关的积压、保留期、落库并发的运维判断。
> 换代的原则同 [db-task-kafka-partition-contract.md](../design/db-task-kafka-partition-contract.md) 的离线流程:**停写 → 排空 → 三方同改 → 消费者先起、生产者后起**。
> **不在 TiDB Phase 1 迁移窗口里做**(D5「借迁移之机误动 topic / partition」,[global-data-layer-tidb-decision.md](../design/global-data-layer-tidb-decision.md))。

## 0. 先读:加分区在线做,只有减分区要离线换代

- **加分区(2026-11-06 起)= 原地加,不停服、不换代**。两张审计表都按主键 / 唯一键去重(流水 `tx_id` 主键 `INSERT IGNORE`,
  快照 `uk_snapshot_guid_nz`),落库不依赖消息顺序,原地加分区重映射 key=player_id 的哈希没有害处。
  `kafkautil.EnsureTopics` 对这两个 topic 打开 `AllowPartitionGrowth`(`go/data_service/internal/kafka/start.go`):broker 上少于配置就
  `CreatePartitions` 加到配置值,多于配置照收,也不建、不核 marker topic。步骤:
  1. 按新分区数核 Kafka 盘(§1 的预算;K8s 的 `Get-KafkaDiskPlan` 按配置的分区数算,`zone-up` / `infra-up` 会打账本)。
  2. 调大 `go/data_service/etc/data_service.yaml` 的 `Kafka.TransactionLogPartitions` / `SnapshotPartitions`(同步 `config.go` 的 `default=` 标签)。
  3. K8s:`k8s_deploy.ps1 -Command infra-kafka-topics -WaitReady`(Job 带 `grow`,少了原地加),再 `zone-up` 滚动 data-service;
     本机:重启 data_service 即可(启动时 `EnsureTopics` 加)。日志见 `kafka topic ... partitions grown in place: 6 -> 12`。
  4. 再加 data-service 副本或调大 `Kafka.*MaxInflightFlushes`(§5)。C++ 生产者靠 librdkafka 定期刷新元数据(缺省 5 分钟)看到新分区,
     之前照旧写老分区,不丢不乱;新分区由消费组再均衡分给各副本,从头消费。
  - 回滚到 2026-11-06 以前的 data-service 二进制之前,先把配置的分区数调成 broker 上的实际值(旧版 `EnsureTopics` 要求相等),或者换代。
- **减分区 / 换名 / 换保留策略需要新 topic 时 = 换一代**:Kafka 不能减分区。有效名 `<基名>_g<N>` 里的 N +1,按下面 §1~§3 离线做。
  其余 topic(go/db、login 的有序 topic,命令 / 观战 / 世界频道 topic)照旧是**不可变契约**:原地 `--alter --partitions` 会重映射哈希,顺序或寻址就断了。
- 两个审计 topic **共用一个世代号**:`go/data_service/etc/data_service.yaml` 的 `Kafka.TopicGeneration` ↔ C++ 的
  `AuditTopicGeneration`(`bin/etc/base_deploy_config.yaml`)。只想扩快照,流水也会一起换到新 topic(流水分区数可以不变)。
- 生产者只有 C++ scene,消费者只有 data_service(所有 zone 共用一个消费组)。
- **旧文档的顺序会丢数(已修正)**:`deploy/k8s/README.md` 以前写的是「建出 `*_g2` → 滚 data-service 与 scene → 老 topic 排空后删」。
  data-service 先切到 g2 后就不再消费 g1,而 scene 没滚完之前仍在写 g1 —— g1 的尾部无人消费,保留期一到就丢。必须按本文的「停写 → 排空 → 切」。

## 1. 决策与预算(开工前,负责人拍板)

- **新分区数**:快照 S、流水 T(T 默认保持 6)。
  - G2a 之后全球并行落库路数 ≈ min(S, 2Z × M):Z = zone 数(每 zone 2 个 data-service 副本),
    M = `Kafka.SnapshotMaxInflightFlushes`(一个实例可同时写 M 个分区)。例:S=12、Z=2、M=2 → 8 路。
  - 每条落库循环的吞吐:逐条 ≈ 1 / t_row;批量 ≈ B / t_batch(B)。t_row、t_batch 看 `data_service_kafka_consumer_flush_seconds`。
  - 建议值 S=12(待拍板,G2a 规格 Q-5);S=24 时审计单项盘量就是 15GiB,不建议。
- **Kafka 盘预算必须按整台 broker 算,不能只算审计 topic**。副本数 3、broker 3 台时每台 broker 持有每个分区的一份副本,
  最坏占用 ≈ Σ_所有分区 min(retention.bytes, 该分区写速 × retention.ms)。
  (2026-11-11 更正:db_task 已随存盘直写退役,盘预算现在只算审计、命令与其余 topic 预留,由 `k8s_deploy.ps1` 的 `Get-KafkaDiskPlan` 按配置的分区数算,
  见 `deploy/k8s/README.md`「Kafka 磁盘预算」;审计 topic 每分区 4GiB,S=3、T=6 时 prod 盘预算 80Gi,每多 9 个审计分区约多 47Gi。)
  下面是 G2a 时的原始估算,数字已过时:按 prod 每分区 512MiB 的上界,
  审计 (S+T)×512MiB **加上** 每个 zone 的 db_task(10 分区)Z×10×512MiB,再加命令 / game-events 等。
  例:S=12、T=6、Z=2 → 9GiB + 10GiB = 19GiB,**已经顶到 20Gi PVC**(`deploy/k8s/manifests/infra/kafka.yaml`)。
  broker 盘满 = 所有 zone 的存盘与命令一起停,事故面远大于审计本身。所以 **S=12 的前提是同批扩 Kafka PVC**
  (或给审计 topic 单独设更小的 topic 级 `retention.bytes`,但那会缩短 §4 的 T_safe,与保留期决策冲突)。
- **时机**:开服前做,topic 里只有测试数据,排空是空操作,窗口最短。**开服后做 = 全服停服维护**(scene 是唯一生产者,停写 = 停所有 scene)。

## 2. 步骤

任何一步的核对不通过就停下,不要跳步。

### 2.1 同一次提交改配置(先不部署)

| 位置 | 改什么 |
|---|---|
| `go/data_service/etc/data_service.yaml` | `Kafka.TopicGeneration: N+1`、`SnapshotPartitions: S`(需要时 `TransactionLogPartitions: T`) |
| `go/data_service/internal/config/config.go` | `KafkaConfig` 里对应的 `default=` 标签与注释同步(缺键时不要落回旧数;marker 会让不一致的实例启动失败,但别依赖它) |
| `bin/etc/base_deploy_config.yaml` | `AuditTopicGeneration: N+1`(C++ 生产者;K8s 的 node ConfigMap 由 `k8s_deploy.ps1` 从 data_service.yaml 镜像,不抄这份) |
| `deploy/docker-compose.yml` | `KAFKA_INIT_AUDIT_TOPIC_GENERATION` 默认值;`kafka-topic-init` 里写死的最少分区数 6 / 3(`create_topic` / `ensure_min_partitions` 两处,只在要降到 6 / 3 以下时才改) |
| `tools/scripts/start_game.ps1` | 本机预建清单里审计主题的最少分区数(6 / 3,`GrowOnly`);主题名与保留期仍与 data_service.yaml 逐项核对 |
| 描述性文字 | `deploy/k8s/README.md`「Kafka 审计 topic 预建」、`deploy/k8s/manifests/go-svc/data-service.yaml` 头注、`deploy/k8s/manifests/infra/kafka-topic-init.yaml` 头注、`docs/design/data_service_role_and_scope.md` |
| `deploy/k8s/data-service-audit-alerts.yaml` | 换代本身**不用**改:`DataServiceAuditLagHigh` 比的是单分区最大积压,阈值与分区数无关(§4)。同批改了每分区 `retention.bytes` 时才按 §4 重算,并在注释里写明出处 |

契约测试 `tools/scripts/tests/k8s_deploy_contract.tests.ps1`(AuditTopicGeneration 与 TopicGeneration 相等、
data-service ConfigMap 的 Kafka 键与服务 yaml 相等)应自动跟随;改完先让它跑绿。

### 2.2 停写

停掉**所有 zone 的所有 scene**(K8s:按本集群的编排形态把 scene 工作负载缩到 0;本机:停掉全部 scene 进程)。

核对:用 `kafka-get-offsets.sh`(或 `kafka-run-class.sh kafka.tools.GetOffsetShell`)取两个 g N topic 各分区的 end offset,
间隔 2 分钟取两次,**完全不变**才算停写完成。

### 2.3 排空

g N 的 data-service 保持运行。

- `kafka-consumer-groups.sh --bootstrap-server <b> --describe --group data_service-player-snapshot`
  与 `--group data_service-transaction-log`:g N topic 的每个分区 `LAG` = 0。
- 同时看指标:`data_service_kafka_consumer_lag_messages` = 0、`data_service_kafka_consumer_retention_gap_messages` = 0、
  `data_service_kafka_consumer_up` = 1;data_service 日志里没有 `STOPPED:`。
- 有 `STOPPED:` 先按日志修 DB(日志点名了 guid 与 offset),**不许跳过**。
- `retention_gap_messages > 0` = 已经有审计数据被保留期删除:记录条数与时间段,上报负责人;它**不是**可以「清掉」的排空障碍。
  条数怎么取:日志 `AUDIT DATA LOST` 是探针的采样值,只是下界;精确条数看 `[kafka-go]` 的 `skipping from offset` 行,
  消费者期间重建过(停机冷却 / 重启 / rebalance)的看 `starting at offset` 与 `seeking to offset` 两行之差(口径见 §7)。

### 2.4 停 data-service

所有 zone。

### 2.5 预建新代 topic

- K8s:`pwsh -File tools/scripts/k8s_deploy.ps1 -Command infra-kafka-topics -WaitReady`(Job 从改过的 data_service.yaml 取基名 / 分区数 / 保留期 / 代号)。
- compose:`docker compose -f deploy/docker-compose.yml up kafka-topic-init`。

核对:`kafka-topics.sh --describe --topic player_snapshot_topic_g<N+1>`:`PartitionCount: S`、`Configs` 里 `retention.ms=2592000000`;
流水 topic 同样核对。

### 2.6 起 data-service(新配置)

- K8s 上「新配置」= 用改过的仓库重新生成并 apply **两份** ConfigMap:`go-svc-data-service-config`(`Kafka.TopicGeneration` / `*Partitions`)
  与 `node-config`(`AuditTopicGeneration`,由 data_service.yaml 镜像)。本仓的生成入口是 `k8s_deploy.ps1 -Command zone-up`
  (多 zone 用 `all-up`),参数与平时部署相同;之后 `kubectl -n <zone-ns> rollout restart statefulset/data-service`。
- **在起任何 scene 之前**核对:`kubectl -n <zone-ns> get cm node-config -o yaml` 里 `AuditTopicGeneration: N+1`,
  `go-svc-data-service-config` 里 `TopicGeneration: N+1`。不要只靠契约测试。
- 日志应见 `[kafka] consumers started: ... topic=player_snapshot_topic_g<N+1> ... p=S`(2026-11-06 起审计 topic 不再建 marker,没有 `kafka immutable partition marker ensured` 这一行);
  `data_service_kafka_consumer_up` = 1。

### 2.7 起 scene(新 AuditTopicGeneration)

**必须晚于 2.5**:broker 开着 `auto.create.topics.enable` + `num.partitions=1`,scene 抢先发消息会把新 topic 建成 1 分区、
保留期继承 broker 默认值(分区数能被 init / data-service 原地补上,但抢先写进去的消息落在哪个分区由不得你,排障多一个变量)。
最好也晚于 2.6(先看到消费者起来,排障时少一个变量)。
`zone-up` 会把 scene 与 data-service 一起滚动:只要 2.5 已完成就是安全的。

> 补注(2026-10-15,未经运行验证):data-service 的 lag 探针不再可能自动建 topic(sarama `Metadata.AllowAutoTopicCreation = false`,
> 负责人 2026-10-15 按「最标准」定)。它指着的 topic 不存在时只会探测失败:日志 `lag probe failed`(broker 回 UNKNOWN_TOPIC_OR_PARTITION
> 时错误文本含 `does not exist on this broker`;broker 侧回哪个码未实测)、`lag_probe_total{result="error"}`,
> 持续 15 分钟即 `DataServiceAuditLagProbeBlind`,不再可能把它误建成 1 分区。
> 探针在 data-service 启动的 `EnsureTopics` 成功之后才起,按本流程(2.5 预建在先)碰不到不存在的 topic;换代期间若见到这条失明,
> 先 `kafka-topics.sh --describe` 查该 topic 是否被删 / 未建。预建(2.5)仍是前置步骤:scene 生产者与 broker 的 auto.create 不受这项改动影响。

### 2.8 验证

- 一个账号登录 / 登出 → `player_snapshot` 表出现 2 行新 guid(source=1)。
- 新 topic 上消费组 offset 前进、`LAG` ≈ 0;`lag_messages` 指标同。
- Loki:`{job="cpp_nodes", service="scene"} |= "[SnapshotSystem] Kafka send failed"` 无新增。

### 2.9 收尾

旧 g N topic(以及 2026-11-06 以前的二进制建的 marker topic)至少保留一个审计窗口(30 天)再删;删除不属于本流程。

## 3. 回滚

同样「停写 → 排空 g N+1 → 三方切回 g N」。

- g N 的组 offset 若已超过 `offsets.retention.minutes`(Kafka 默认 7 天)被清,消费者会从 g N 最早位置重读
  (`StartOffset=FirstOffset`)—— 由 snapshot_guid / tx_id 去重保证幂等,只是多耗时间。
- **禁止**在已有新写入时直接把配置切回旧 topic(形成两段历史,与 db-task 契约同理)。

## 4. 保留期与积压(容量估算,开服前填数)

数字以压测为准(AGENTS §6;下文「示例」不是实测)。

- 快照速率 R = 全球(登录速率 + 登出速率);每分区 R/P(按 key 哈希,近似均匀)。
- 全球消费吞吐 ≈ min(P, 2Z × M) × 每循环吞吐,同时受全局库写入能力(fsync / 写带宽)封顶。
- **审计 topic 每分区的 `retention.bytes` 比 30 天先到**(2026-11-11 起 prod / prod-like 4GiB,由 `k8s_deploy.ps1 -AuditKafkaRetentionBytes`
  的档位缺省渲染进 data-service、启动时设到 topic 上;dev 与 compose 沿用 broker 缺省 128MiB):积压超过它的未消费消息会被删。
  - 积压不丢的上限按**单分区**计(retention 按分区执行,哪个分区先到线就先删哪个):每分区(条)≈ retention.bytes ÷ 平均快照字节;
    4GiB、300KB 时约 1.4 万条(15KB 时约 28 万条),与分区数无关。各分区积压均匀时(全局库故障、全部消费者同时停)合计约为它的 P 倍;
    积压集中在一个分区时(持有它的实例卡住、毒丸行卡住所在分区,Q-3)那个分区先过线、先丢 —— 所以不能拿合计去比删除线。
  - 换算成时间:**T_safe = P × retention.bytes ÷ (R × 平均快照字节)**(单分区进水速率是 R/P,按单分区算是同一个式子;
    积压只长在一个分区、其余分区照常消费时也是它)。示例 R=128/s、P=3、4GiB:15KB 约 1.9 小时;300KB 只有约 5.5 分钟。
    broker 按 `log.retention.check.interval.ms`(prod 2 分钟)成段删除,删除是阶梯式的。
  - T_safe 必须**大于**「最长可容忍的 DB 故障 + supervisor 冷却(`ResumeAfter`,60s)+ 积压追平时间」,否则任何一次 DB 抖动都会丢审计数据。
  - 反过来算盘:真正保留 30 天要 R × 平均字节 × 30d(示例 R=128/s、300KB 约 98TB;10KB 约 3.3TB),「30 天」在开服速率下只是名义值。
  - **保留策略 2026-11-11 已定(缺口清单第 3 条,原 G2a 规格 Q-1)**:目标是消费端中断 **2 小时**不丢;按容量模型一个 45k zone 峰时
    单分区进水约 0.5MB/s(快照 15KB),每分区 4GiB(约 2.4 小时),prod 盘预算随之 80Gi。多 zone 时按 zone 数等比加审计分区,单分区进水不变。
    推导、扩 zone 做法与前提见 `deploy/k8s/README.md`「审计 topic 的 retention.bytes」。前提是估计值:实测快照远大于 15KB 时 T_safe 按比例缩短
    (300KB 时只剩几分钟),先用 `audit_snapshot` 抽样或关掉一个触发点,再按实测放大 retention.bytes 并同批扩 PVC。
- `DataServiceAuditLagHigh` 比的是**单分区最大积压**(`data_service_kafka_consumer_lag_max_partition_messages`,探针每轮取各分区积压的最大值;
  每个 consumer 一条序列,不按分区出指标),阈值 = 单分区删除线条数的 30%(当前 4000 ≈ 30% × 4GiB ÷ 300KB,快照按保守的 300KB 算),
  与分区数无关,换代**不用**重算。拿到实测快照大小(`data_service_kafka_consumer_message_bytes`)或放宽 retention.bytes 之后,
  按上面的单分区公式重算并写进告警注释。
  合计积压仍看 `data_service_kafka_consumer_lag_messages`(`DataServiceTxLogLagHigh` 与排空核对用它):它与单分区最大值接近 =
  积压集中在一个分区;相差约 P 倍 = 各分区均匀积压。
- 全局库写带宽 ≈ R × 平均字节 ×(数据 + redo + binlog 约 3 倍)。示例 R=128/s、300KB → 约 38MB/s 数据、约 115MB/s 落盘。

## 5. 批量 / 并发旋钮(G2a,`Kafka.*`)

| 键 | 默认(= 2026-10-12 之前的行为) | 合法范围 | 建议开服值(待压测) |
|---|---|---|---|
| `SnapshotBatchMaxRows` | 1(逐条落、逐条提交) | [1, 256] | 16 |
| `SnapshotBatchMaxBytes` | 4194304(软上限,加入后检查,最多超出一条) | [65536, 33554432] | 4194304 |
| `SnapshotFlushIntervalMs` | 0(不等待,只拼已排队的) | [0, 5000] | 200 |
| `SnapshotMaxInflightFlushes` | 1(本实例同时落库的分区数) | [1, max(1, SnapshotMySQL.MaxOpenConn − 1)] | 2 |
| `TransactionLogMaxInflightFlushes`(2026-11-06) | 1(流水本实例同时落库的分区数;每分区各攒 200 行 / 200ms 一批) | [1, max(1, SnapshotMySQL.MaxOpenConn − 1)] | 一个副本分到 ≥ 2 个流水分区时调到 2(待压测) |
| `LagProbeIntervalMs` | 30000(yaml 刻意打开;0 = 关) | 0 或 [5000, 600000] | 30000 |

- 越界 = data_service 拒启(exit 1,日志 `invalid Kafka consumer config`)。K8s ConfigMap 从服务 yaml 镜像这六个键,改服务 yaml 即可。
- 流水消费者 2026-11-06 起和快照一样每个分区一个 worker(`go/data_service/internal/kafka/partition_workers.go`):各分区各攒各的批、各自先落库后提交,
  一个分区重试用尽就停下整个消费者;排空时各分区落完手里的批再退出。`data_service_kafka_consumer_flush_seconds{consumer="transaction_log"}`
  只算拿到落库名额之后的那段,名额排队不算。
- 并发上限留一条连接给 GM 快照 / 回档读;本旋钮**不新增任何 MySQL 连接**(连接预算已超)。
- 批量路径:先普通读剔除库里已有的 guid 与批内重复,剩下的按 guid 升序拼一条**普通**多行 INSERT,撞 1062(并发写者刚落了同一 guid)退回逐条,按 snapshot_guid 去重,语义与逐条相同(〔2026-10-16 更正:原写多行 `INSERT ... ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`,批内重复报 1869、撞已存在行时 rebalance 重叠会成环,已改,见 `snapshot_store.go` 的 `InsertSnapshotsIfGuidAbsent` 与事故文档 §9.6〕);
  批量失败自动逐条兜底(`isolation_total{result="recovered"}`),兜底也插不进去的那一行让消费者停在那里(与逐条消费同结局)。
- **打开批量(1 → 16 等)的前置**(G2a 规格 Q-7 / Q-9):
  1. 压测对比表:AGENTS §6 全流程,同一环境「逐条」与「批量」各跑一轮,用 `stress_summarize.ps1` 出审计段(lag、flush p50/p99、
     batch_rows 均值、message_bytes p50/p99/max、data_service RSS)。脚本的审计段是待做项(G2a-2);没有对比表不得声称吞吐提升。
  2. 内存护栏:批量后每实例在途 ≈ kafka-go 内部队列 + 拥有分区数 × (通道 + 批) × BatchMaxRows 条,单条最坏约 1MB,
     GC 余量下可顶破 512Mi(`deploy/k8s/manifests/go-svc/data-service.yaml` 的 limits);data-service 同进程跑 Load/Save,
     OOMKill 是玩家数据面中断。先实测 RSS 峰值,再由负责人定 `GOMEMLIMIT`(如 limits 的 80%)或提 limits。
- TiDB:今天 TiDB 上建不出 `uk_snapshot_guid_nz`,store 走退路逐行,批量不生效;将来 TiDB 形态的去重让批量生效时,
  批字节须 ≤ 4MiB(TiKV raft-entry-max-size 默认 8MB),并在 TiDB 上重跑批量的真库用例。

## 6. 全局库成瓶颈时

- 全局库(SnapshotMySQL)同时承载建角同步路径(名字登记、号段)与审计写(快照、流水),连接池分开,但在同一台实例的 CPU / IO 上。
  审计是唯一能「慢下来而不伤玩家」的一方:积压只要在保留期内就不丢。
- 节流旋钮:`SnapshotMaxInflightFlushes` 压到 1、批量调大(减少 fsync 次数:`sync_binlog=1` + `innodb_flush_log_at_trx_commit=1`
  下逐条自动提交是每条两次 fsync,攒 B 条是每 B 条两次)。lag 会上升,但同步路径受保护。
- **这个旋钮在保留策略拍板之前基本不能用**:按 §4,示例速率下删除线只有几十秒到几分钟,「让审计慢下来」几分钟内就会变成「审计被 broker 删掉」。
  所以顺序是:先确认保留策略已把 T_safe 拉到小时级 → 再压并发、加批。T_safe 不够时,能保同步路径的只剩开服前把审计表拆到独立库实例
  (G2b,待立项)或负责人拍板的开服期快照策略。
- 早期信号:`DataServiceAuditFlushSlow`(单次落库 p99 > 2s,15 分钟)。

## 7. 告警与值班查询

规则文件:`deploy/k8s/data-service-audit-alerts.yaml`(PrometheusRule,`kubectl apply -n observability -f ...`)。

| 告警 | 级别 | 先看什么 |
|---|---|---|
| `DataServiceAuditConsumerStopped` | warning | data_service 日志 `STOPPED:`(点名 guid / offset);修库后自动续消费。带 `for: 2m`,只为滤掉新 Pod 启动时首个样本为 0 的抖动 |
| `DataServiceAuditRetentionGap` | critical | **数据已丢**。探针在 15 分钟内采到过一次缺口就响(`max_over_time(...[15m]) > 0`、`for: 0m`),缺口消失后约 15 分钟才 resolve,resolve 不代表数据找回。日志 `AUDIT DATA LOST`(采样下界)与 `[kafka-go]` 的 `skipping from offset` 行;消费者重建过的看 `starting at offset` / `seeking to offset` 两行之差(见表后说明);上报负责人 |
| `DataServiceAuditLagHigh` | critical | 某个分区逼近删除线(比的是单分区最大积压,§4);先分清库慢(FlushSlow)还是消费者停了(ConsumerStopped);合计 `lag_messages` 与它接近 = 积压集中在一个分区 |
| `DataServiceTxLogLagHigh` | warning | 流水落后;`[transaction_log] STOPPED:` |
| `DataServiceAuditConsumerStalled` | warning | 某个分区上组内已提交位点停下之后又新到了消息,位点 15 分钟没再前进(`data_service_kafka_consumer_commit_stall_seconds` ≥ 900,lag 探针按分区算),且消费循环报 up:专抓「活着但不前进」(2026-11-05,r2-kc#1;2026-11-06 改判据)。只认新到的消息:reader 模式提交失败交还的已落库记录会在低流量 topic 上一直挂着积压,那不算卡住,旧判据「有积压 + 15 分钟零处理」在这里误报;代价是分区上一条新消息都不来时看不见。消费组 owner 遇再均衡与暂时失败不停,日志 `[kafka-bound]` 是它在原地重试(等不到代次 / 打开分区 / 拉取 / 提交),`kafka_bound_consumer_retry_total{stage}` 看卡在哪一步;Kafka 侧修好后自动追上。按分区判,单个实例卡住也会响 |
| `DataServiceAuditLagProbeBlind` | warning | 按实例触发:该实例的探针 15 分钟没成功过,它的 lag / retention_gap 是陈值(照样参与积压类告警的 max,先按 instance 展开指标排除);所有实例都在报时积压类告警都不可信;日志 `lag probe failed` |
| `DataServiceAuditFlushSlow` | warning | 全局库排队;见 §6 |
| `DataServiceAuditMessagesSkipped` | warning | 坏消息被跳过(未落库);日志 `undecodable` / `invalid snapshot` / `oversize snapshot`。计数在解码时记:`ConsumerStopped` 同时在响时会随每次重放重复累加,条数以日志里去重后的 partition / offset 为准 |
| `DataServiceAuditBatchIsolated` | info | 批量失败、逐条兜底成功;多半是包 / 事务大小,调小批字节或批行数 |

- C++ 生产端的发送失败不在 Prometheus(scene 没有指标端点)。Loki 值班查询:`{job="cpp_nodes", service="scene"} |= "[SnapshotSystem] Kafka send failed"`
  (LOG_ERROR,任何日志级别都在;C++ 日志流没有 `app` 标签,写成 `{app="scene"}` 恒为空,选择器写法见 `deploy/k8s/player-inbox-alerts.yaml`)。`Snapshot captured ... size=` 是 LOG_INFO、且在**入队**时打,不代表投递成功;
  用它统计大小分布时确认 scene 日志级别 ≤ INFO。
- 保留期删掉未消费的消息之后 kafka-go 会自动跳到日志起点。它留下什么日志分两种情形(kafka-go v0.4.47,已按 tag 源码核对,
  组模式与非组模式相同;data_service 把这些行接到了自己的日志,前缀 `[kafka-go] topic=... group=...`):
  - 消费者**运行中**被保留期追上:Error 级的 `... skipping from offset X to Y (N messages)`,N 是精确条数。
  - 分区 reader **(重新)初始化**时(进程重启 / `STOPPED` 冷却后重建 / rebalance / 断线重连)已提交位点早于日志起点:kafka-go
    **静默**跳到起点,没有上面那一行。看同一分区先后的两行 Info:`... starting at offset A`(传入位点;负数 = 这个组在该分区没提交过,
    不算丢)与 `... is seeking to offset B`(实际起读位点)。A ≥ 0 且 B > A 时,B − A 就是被跳过的条数。
    「DB 故障 → 停机 → 冷却 60s → 重建」这条主要的丢失路径走的正是这里:DB 持续故障期间每次重建都会打一对,
    位点没提交时 A 不变、B 随保留期推进逐次变大 —— 同一个 A 取 B 最大的那一对,A 变过(中途有过提交)的各段相加。
    两行是 Info 级,日志级别高于 info 时看不到。
  - 探针的 `AUDIT DATA LOST` 行是每轮(默认 30s)的采样值,只是下界,消费者越过缺口并提交后不再出现;
    消费者已读入未提交的消息所在段被删、其实没丢时也会出现(见下一条 ②),丢没丢以 `[kafka-go]` 行为准。
  - `DataServiceAuditRetentionGap` 对探针采到的任何一次缺口都立即呼叫(2026-10-15 按负责人「按最标准的去做」改定,取代原来的 `for: 2m`;
    窗口 15m 的取值依据写在规则注释里)。代价:gauge 为正而实际没丢(日志条数为 0)的情形也会呼叫。分两种,那一刻积压都已经在
    删除线上,一律按丢数据处置,用上面的日志确认条数:
    - ① 瞬时:探针先读 committed 后读 start,消费者正读着分区最老的段、该段恰在两次读之间被删(消费者其实已越过它),只有一轮探针为正。
    - ② 更常见:消费者已读入、尚未提交的消息所在的段被删。读入在前、落库后才提交,读入未提交的量 = kafka-go Reader 的预取队列
      (两个消费者共用的 `newReader` 未设 `QueueCapacity`,v0.4.47 默认 100 条,由本实例分到的各分区共用)+ 正在攒 / 落库的批
      (player_snapshot 另有分区 worker 通道)+ 当前 fetch 响应的剩余部分。gauge 一直为正,直到下一次提交越过日志起点,
      可持续多轮探针(库慢时数秒到数分钟);reader 位点在被删段之后,kafka-go 既不打 `skipping from offset` 行、
      也不重新定位(没有 `starting at offset` / `seeking to offset` 两行),日志条数为 0。消费者随后照常落库提交就没丢;
      提交之前停机 / 重建时,其中没落库的才真丢,看那次重建的两行位点日志。
  - 仍会漏的:缺口整段落在两次探针之间(短于一个探针间隔,默认 30s)时 gauge 从未为正,`DataServiceAuditRetentionGap` 无从触发。
    所以积压事故收尾时,不论告警响没响都按 `[kafka-go]` grep 一遍。
  - 窗口 15m 待故障演练确认(dev 环境,开服前做一次):① 核对 ServiceMonitor / PodMonitor 的抓取间隔 ≤ `LagProbeIntervalMs`
    (监控栈不在本仓,未核实),否则只保持一轮探针的正值可能一个样本都没被抓到;② 断库让 player_snapshot 消费者停机,
    等积压越过单分区删除线、日志出现 `AUDIT DATA LOST`,再恢复库让消费者越过缺口并提交(可临时调小该审计 topic 的
    `retention.bytes` / `retention.ms` 加快触发,演练后改回);③ 在 Prometheus 查 `ALERTS{alertname="DataServiceAuditRetentionGap"}`:
    第一条 `AUDIT DATA LOST` 之后约「抓取间隔 + 30s 评估间隔」内进入 firing,最后一条之后约 15 分钟才 resolve,其间 Alertmanager
    至少发出一次通知。不满足时先对照 ① 查抓取与探针间隔,确是窗口不够再按规则注释里的下限公式(评估间隔 + 探针间隔)重新取窗口;
    演练结果写进规则注释,替换「待压测 / 故障演练确认」。

## 8. 消息大小链路(核对结论,本次不改)

| 环节 | 上限 |
|---|---|
| C++ 生产者 `message.max.bytes` | **约 1,000,000 字节**(librdkafka 默认;本仓未设) |
| broker `message.max.bytes` / topic `max.message.bytes` | 1,048,588(Kafka 默认;未设) |
| kafka-go reader `MaxBytes` | 16MiB |
| 消费者 `MaxBlobBytes` | 16MiB − 1(MEDIUMBLOB) |
| MySQL `max_allowed_packet` | 64MiB(8.0 默认;未设) |
| TiDB `txn-entry-size-limit` | 默认 6MB(dev 已配 32MiB) |

**有效上限约 1MB,在生产端本地**:超过的快照在 `produce()` 里被拒(按 librdkafka 公开行为推断,本仓未实测),
`CaptureAndSend` 只打 `Kafka send failed` 并返回 0,调用方忽略返回值 —— 这个玩家这一次的快照就没有了,且 C++ 侧没有指标。
消费端的 16MiB 上限从来到不了。单条快照实际多大本仓没有数据:看消费端 `message_bytes` 直方图(只看得到发成功的)与 scene 日志。
将来调高上限到 > 6MB 时,TiDB 的 `txn-entry-size-limit` 必须同步,否则那条消息成为毒丸、卡住所在分区。

## 9. 待负责人拍板 / 待验证

- **拍板项**(G2a 规格 §15;凡涉及「快照是否必达 / 是否可丢」的都在这里,代码一律未改):
  Q-1 保留策略(2026-11-11 已定:扛住 2 小时消费中断、审计 topic 每分区 4GiB,见 §4「保留期与积压」)、Q-2 快照 > 约 1MB 在生产端丢弃、Q-3 毒丸行永久卡住消费者(维持 / 跳过)、Q-4 开服期快照策略、
  Q-5 新分区数与换代时机(前提:同批扩 PVC)、Q-6 在线换代、Q-7 何时打开批量、Q-8 单分区停机是否只停该分区、Q-9 内存护栏。
- **待 Codex 验证**:G2a 的单测、真库集成、部署生成器契约测试、`promtool check rules`、本机端到端冒烟与 L-14 压测,
  命令与通过标准见 G2a 交付说明;本文的步骤在第一次真实换代前没有被演练过。

## 修订历史

- 2026-10-13:初版(G2a)。同批修正 `deploy/k8s/README.md` 里会丢 g N 尾部的换代顺序。
- 2026-10-13(G2a 评审修复,未经运行验证):`DataServiceAuditLagHigh` 改按单分区最大积压比较(新指标
  `lag_max_partition_messages`;§4 按条数的公式去掉「× P」,§2.1 不再要求换代后重算阈值);`DataServiceAuditLagProbeBlind` 改为按实例触发;
  `DataServiceAuditConsumerStopped` 加 `for: 2m`;保留期丢失的日志口径按 kafka-go v0.4.47 源码核对后重写(§2.3 / §7,
  reader 重建路径是静默的,改看新接出的两行位点日志);补充坏消息计数在停机重放时会重复累加的说明(§7)。
- 2026-10-15(负责人「按最标准的去做」,未经运行验证):`DataServiceAuditRetentionGap` 改为
  `max by (consumer) (max_over_time(...[15m])) > 0`、`for: 0m`,对任何一次采到的缺口都呼叫;§7 告警表与表后说明同步,补窗口的演练确认步骤。
- 2026-10-15(上一条的评审修复,未经运行验证):§7 把「gauge 为正而实际没丢」拆成两种 —— 探针两次读之间删段的瞬时情形,
  与更常见的「消费者已读入未提交的消息所在段被删」(可持续多轮探针、日志条数为 0);规则文件头的抓取前提补「抓取间隔 ≤ LagProbeIntervalMs」。
- 2026-10-15(负责人按「最标准」定,未经运行验证):lag 探针关掉 sarama 的自动建 topic(`go/shared/kafkautil/group_lag.go`
  `newGroupLagProberConfig`),topic 不存在时报失明而不是可能把它建成 1 分区;§2.7 末补注。依据见 G2a 规格 §4.5 的「已定」注。

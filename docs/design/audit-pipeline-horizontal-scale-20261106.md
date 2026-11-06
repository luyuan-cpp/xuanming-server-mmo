# 审计流水链路的水平扩展(缺口清单第 15 条,2026-11-06)

状态:设计已定,分批落码,**未编译、未测试,待 Codex 验证**。验证步骤见 `docs/PROGRESS.md` 2026-11-06「审计流水链路水平扩展」条目。
上游:[服务器代码缺口清单](../handoff/server-gap-audit-20261101.md) 第 15 条;相关:[区服数据回档](./zone_data_rollback.md)(快照的使用方)、
[TiDB 全区全服数据层](./global-data-layer-tidb-decision.md)、运维手册 [审计 topic 换代](../ops/audit-topic-generation-bump-runbook.md)。

## 1. 问题(加机器不涨吞吐的四处)

审计流水(`transaction_log`)与玩家快照(`player_snapshot`)由 C++ scene 经 Kafka 发出、data_service 落进全局流水库(`<base>_ledger`)。

| # | 环节 | 现状 | 为什么不能靠加机器扩 |
|---|---|---|---|
| A | topic 分区 | 流水 6、快照 3,是「不可变契约」:`kafkautil.EnsureTopics` 拒绝与 broker 不一致的值,改分区只能离线换代(停写 → 排空 → 三方同改) | 一个分区只给一个消费者,data_service 开到第 7 个副本就没有分区可领 |
| B | 流水消费者 | 每个进程一条循环:取消息 → 攒 200 条 → 同步 INSERT → 提交,所有分到的分区排在一条线上 | 一个副本分到几个分区,吞吐也只有一条线 |
| C | 单调索引 | `timestamp_sec`、`created_at` 单列索引,以及 `(tx_type, timestamp_sec)`、`(item_config_id, timestamp_sec)`、`(zone_id, created_at)` 这类低基数前缀 + 时间的索引 | TiDB 的二级索引按键有序存放,时间单调增长时全服的追加写都落在每条索引最末尾的同一个 Region,加 TiKV 也只有那一个 leader 在写 |
| D | 清理 | 每个副本都跑 `DELETE … WHERE timestamp_sec < ? LIMIT ?` | 各副本删的是同一批最老的行,互相等锁、白干;删除吞吐不随副本数增长 |

## 2. 决定

### A. 审计 topic 只许加分区、可以原地加

- **为什么安全**:两张表的落库都不依赖消息顺序。
  - 流水:`tx_id` 是主键,一批按 `tx_id` 升序拼一条 `INSERT IGNORE`(`transaction_log_store.go` 的 `InsertBatchIgnore`),重放与乱序都只会被忽略;表上没有插入序语义。
  - 快照:按 `snapshot_guid` 的唯一键去重(`uk_snapshot_guid_nz`),每份快照是独立记录,回档按 `created_at` 选,与到达顺序无关。
  - 分区内顺序只用于「只提交已落库前缀」,扩分区不影响这一条(新分区从头消费)。
- **怎么做**:`kafkautil.TopicSpec` 加 `AllowPartitionGrowth`。打开时:broker 上分区少于配置 → `CreatePartitions` 加到配置值;多于配置 → 接受(Kafka 不能减分区,多出来的照样被消费组分走);不建、也不核「不可变分区标记」(标记只为挡住有序 topic 被原地扩)。**只有 data_service 的两个审计 topic 打开它**;db、login 等依赖按玩家有序的 topic 照旧是不可变契约。
- **生产者**:C++ 用 key=player_id 发;librdkafka 定期刷新元数据(`topic.metadata.refresh.interval.ms`,缺省 5 分钟),看到新分区后同一玩家的消息可能换到新分区。审计落库不依赖顺序,可以接受。
- **部署脚本**:compose 的 `kafka-topic-init`、`start_game.ps1` 的本机预建、K8s 的 `kafka-topic-init` Job 对审计 topic 改为核「分区数不少于配置」,不再要求相等;命令 topic、观战 topic 照旧要求相等。
- **扩容步骤**:把 `go/data_service/etc/data_service.yaml`(及 K8s ConfigMap)的 `Kafka.TransactionLogPartitions` / `SnapshotPartitions` 调大 → 滚动 data_service(首个起来的副本 `EnsureTopics` 加分区)→ 再加副本。减分区仍要换代(`TopicGeneration` +1),手册不变。
- Kafka 盘预算按配置的分区数算(`k8s_deploy.ps1` 的 `Get-KafkaDiskPlan`),加分区之前先按新分区数核一遍盘。

### B. 流水按分区并行落库

照快照消费者的 G2a 结构(`snapshot_worker.go`):dispatcher 按分区把消息分给各自的 worker,每个 worker 自己攒批、落库、提交本分区本批最高 offset;实例级信号量 `Kafka.TransactionLogMaxInflightFlushes` 限制同时落库的分区数(缺省 1 = 与改动前同吞吐;上限 = 流水池连接数 − 1)。
不变量与快照相同:任何分区的任何一次提交 V,都满足「该分区 offset < V 的消息都已落库」;一个分区落库重试用尽就停下整个消费者,由 supervisor 冷却后从断点续。
批内仍按 `tx_id` 升序拼语句,不同分区的批即使碰到同一个 `tx_id`(rebalance 重叠)也按同一顺序取锁,不会成环。

### C. 时间索引按桶打散

- 两张表各加一列 `shard`(`uint32`):流水 = `tx_id % 16`,快照 = `id % 16`(两者都是打散过的发号值,取模后均匀)。由 data_service 写入时算,规则只有一处(`store.AuditShard`)。16 = 两张表的 `SHARD_ROW_ID_BITS=4`。
- 索引改为(proto 的 `OptionIndex`):
  - 流水:`from_player,timestamp_sec; to_player,timestamp_sec; item_config_id,shard,timestamp_sec; tx_type,shard,timestamp_sec; shard,timestamp_sec; item_uuid`。
    同时补上 `item_uuid`:`QueryByItemUUID`(追物品流向)原来没有索引,对全服流水表做全表扫。
  - 快照:`player_id; snapshot_guid; zone_id,shard,created_at; shard,created_at`。`schema.go` 的 `extraIndexes` 里那条兜底的 `(zone_id, created_at)` 删掉,否则存量库上它会被补回来。
- 查询:按时间、`tx_type`、`item_config_id` 走的查询加 `shard IN (0..15)`,优化器按 16 段区间扫;按玩家、按物品 uuid 的查询不变。
- 每条按时间的写入从「一条索引的末尾」变成「16 段各自的末尾」,分到 16 个 Region 上。
- 没做:`snapshot_guid` 普通索引与 `uk_snapshot_guid_nz` 唯一键同样按 SnowFlake 单调增长。唯一键是去重的唯一实现、手写 DDL 有 MySQL / TiDB 两种形态,改它要连去重路径与回滚方案一起重做;快照写入量比流水小一个量级,先看压测的 TiDB 热点视图(KeyViz)再定(同第 20 条「先测」)。
- 项目未上线、玩家数据可以清空:新库按新索引建;已有开发库要重建流水库(proto2mysql 对已存在的表只补列、不删旧索引)。

### D. 清理由一个实例做,按桶并行删

- 每轮开始前在流水库上取会话级命名锁 `GET_LOCK('data_service.audit_retention.<库名>', 0)`(与 schema 迁移锁同一做法),拿不到就跳过这一轮;拿到的实例做完这一轮再放锁。进程崩溃或连接断开时服务端自动释放,不会卡死。
- 删除按 `shard` 分 16 段,每段 `DELETE … WHERE shard = ? AND timestamp_sec < ? LIMIT ?`,至多 4 段同时删;各段互不重叠,不再互相等锁。
- 没做:按天分区 + `DROP PARTITION`。它是这类追加表最省的清理方式(不逐行删、不给 TiDB 留 MVCC 垃圾),但 proto2mysql 表达不了分区表,而且分区表要求主键与唯一键都带分区列,`uk_snapshot_guid_nz` 这条全局去重在 MySQL 上做不到;等清理速度跟不上(看 `data_service_audit_retention_*` 指标)再整体换建表方式。

## 3. 兼容与回滚

- 不改 Kafka 消息格式与 C++ 生产者;流水与快照表只加列,旧二进制写入时不带 `shard`(缺省 0),新查询仍能查到(那些行全在桶 0)。新旧版本混跑只影响打散效果,不影响正确性。
- 打开 `AllowPartitionGrowth` 之后再回滚到旧二进制:旧版 `EnsureTopics` 看到分区数大于配置会拒绝启动消费者。回滚前要把配置的分区数调成 broker 上的实际值;项目未上线时可以直接删 topic 重建。
- 命名锁只在流水库上取,旧二进制不取锁、照旧每个副本都删,混跑期间回到改动前的竞争,不会更糟。

## 4. Java 版

审计流水在 Java 版的形态不同(Java 仓 `PARITY.md` 的审计行),按 11-05 起的约定本轮不写 Java 代码,`PARITY.md` 登记待做。

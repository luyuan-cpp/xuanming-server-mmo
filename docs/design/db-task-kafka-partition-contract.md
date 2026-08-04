# DB Task Kafka 分区不可变契约

## 结论

`db_task_zone_*` 的 partition 数是数据顺序协议的一部分，不是可在线调节的容量参数。
同一 topic generation 内禁止 `CreatePartitions`、生产者动态扩环或“配置追平 broker”。
需要扩容时，必须停写、排空旧 generation，并切到一个新 topic generation。

原因：login 生产者按 `player_id` 一致性哈希选 partition，db 成功写入后又把
`(topic, player_id, msg_type, origin_partition, offset+1)` 作为永久 applied cursor。
Kafka 扩分区会重映射一部分 key；新 partition 的 offset 与旧 partition 不可比较。
把它们直接比较会回档，不比较而继续写也无法证明顺序，因此只能 fail-closed。

## 代码门禁

1. `kafkautil.EnsureTopics` 只创建缺失 topic，绝不原地扩分区；已存在 topic 的
   broker/config partition 数不等时启动失败。
2. 首次成功校验时创建一个 broker 侧不可变 marker topic，名称包含 data topic
   的 SHA-256 和 partition 数。即使运维先在 broker 扩分区、再把配置改成相同数，
   旧 marker 仍会让启动失败。
3. login producer 构造时再次核对实际 partition 集合必须精确为 `[0,N)`；运行中
   发现 drift 后永久 fence 本进程的 DBTask 写入，不动态修改 hash ring。
4. db consumer 的 rebalance/claim 若出现配置范围外 partition，直接拒绝 claim，
   不按需创建“看起来能消费”的 worker。
5. retry payload 保留原始 partition/offset；db batch coalescer 只对同 partition、
   已知 seq 的写按最大 seq 合并。跨 partition 写进入 ordering guard/DLQ。

## 离线扩容流程

假设从 generation 1 / 10 partitions 扩到 generation 2 / 20 partitions：

1. 备份配置，停止所有会产生 DBTask 的 login/业务写入入口。
2. 保持旧 db consumer 运行，确认旧 topic consumer lag 为 0。
3. 确认旧 generation 的 Redis retry `ready`、`processing` 均为 0；逐条处置 dead
   queue，不能把未决任务遗留到旧 namespace。
4. 停止旧 db consumer。此时 MySQL 已包含旧 generation 的最终状态。
5. 在 login 与 db 配置中同时设置 `TopicGeneration: 2`、`PartitionCnt: 20`、
   `InitialPartition: 20`（login）。不要 ALTER generation 1 的 topic。
6. 先启动 db，再启动 login。任一侧启动门禁失败都不开放写流量。
7. 验证新 topic 名为 `db_task_zone_{zone}_g2`、20 partitions、marker 存在；执行
   单玩家连续写与故障重试 smoke，确认 applied cursor 使用新 topic namespace。
8. 恢复写入口并观察 lag、DLQ、producer fence 告警。旧 topic 与 marker 至少保留
   一个审计/回滚窗口，不立即删除。

回滚也必须先停写并排空新 generation。已有新写时直接把配置切回旧 topic，会形成
两条独立顺序历史，禁止这样操作。

## 已知边界

- 本协议关闭“同名 topic 在线扩分区”，不实现跨 generation 的在线双写/epoch 交接。
- Kafka 集群迁移同样必须使用停写/排空流程；MirrorMaker 复制 backlog 不等于保留
  一致性哈希与 applied cursor 因果关系。
- 删除 marker 是破坏正确性门禁的运维动作，不属于正常清理。

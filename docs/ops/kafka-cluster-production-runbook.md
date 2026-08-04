# Kafka 集群化生产部署 Runbook

> 创建日期: 2026-06-03
> 状态: **未实施**,生产部署时使用
> 配套: docs/notes/redis-oom-data-safety-qa-record.md(P1 决策记录)
> 作者: P1 数据安全加固阶段

---

## 为什么需要这个 Runbook

当前本地开发环境是 **1 个 Kafka broker + replication-factor=1**。
生产环境必须升到 **3+ broker + replication-factor=3 + min.insync.replicas=2**,
否则 broker 磁盘故障会丢失全部未消费消息。

本地不需要 3 broker(浪费内存,压测意义不大),所以代码层只调了 retention
(5min → 24h),broker 数量留给生产部署专门处理。

---

## 前置确认

部署前必须确认:

- [ ] 已完成 P0(Redis maxmemory + LFU)
- [ ] 已完成 P1.1 + P1.2(代码默认 RetentionMs=86400000,4 处全改)
- [ ] 生产服务器有 3+ 物理节点(或 3+ 独立可用区)
- [ ] 每个 Kafka 节点有独立磁盘(不共享 IOPS)
- [ ] 每节点至少 4 GB 内存给 Kafka heap(`-Xms2g -Xmx4g`)

**如果做不到 3 个独立节点**,部署单 broker + 严格的快照备份策略也比 3 broker 共享单磁盘强 ——
后者磁盘挂了 3 副本一起丢,毫无意义。

---

## 部署步骤(零停机迁移)

### Step 1: 起 3 broker 集群(并行运行)

新建 `deploy/docker-compose.kafka-cluster.yml`:

```yaml
services:
  kafka-1:
    image: apache/kafka:latest
    container_name: kafka-1
    hostname: kafka-1
    environment:
      - KAFKA_NODE_ID=1
      - KAFKA_PROCESS_ROLES=controller,broker
      - KAFKA_CONTROLLER_QUORUM_VOTERS=1@kafka-1:9093,2@kafka-2:9093,3@kafka-3:9093
      - KAFKA_LISTENERS=INTERNAL://:29092,EXTERNAL://:9092,CONTROLLER://:9093
      - KAFKA_ADVERTISED_LISTENERS=INTERNAL://kafka-1:29092,EXTERNAL://kafka-1.prod:9092
      - KAFKA_LISTENER_SECURITY_PROTOCOL_MAP=CONTROLLER:PLAINTEXT,INTERNAL:PLAINTEXT,EXTERNAL:PLAINTEXT
      - KAFKA_INTER_BROKER_LISTENER_NAME=INTERNAL
      - KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER
      - KAFKA_DEFAULT_REPLICATION_FACTOR=3            # 关键: 默认 3 副本
      - KAFKA_MIN_INSYNC_REPLICAS=2                   # 关键: 至少 2 个 ISR 才能写入
      - KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR=3      # __consumer_offsets 也要 3 副本
      - KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR=3
      - KAFKA_TRANSACTION_STATE_LOG_MIN_ISR=2
      - KAFKA_HEAP_OPTS=-Xms2g -Xmx4g
    ports:
      - "9092:9092"
    volumes:
      - kafka-1-data:/var/lib/kafka/data

  kafka-2:
    # ... 同上,NODE_ID=2,端口 9192,hostname kafka-2

  kafka-3:
    # ... 同上,NODE_ID=3,端口 9292,hostname kafka-3

volumes:
  kafka-1-data:
  kafka-2-data:
  kafka-3-data:
```

启动:
```bash
docker compose -f deploy/docker-compose.kafka-cluster.yml up -d
```

验证集群健康:
```bash
docker exec kafka-1 /opt/kafka/bin/kafka-broker-api-versions.sh \
  --bootstrap-server kafka-1:9092 | grep "id:"
# 应返回 3 个 broker
```

### Step 2: 调整应用配置指向新集群

修改生产 `login.yaml`:
```yaml
DBTaskKafka:
  Brokers:
    - kafka-1.prod:9092
    - kafka-2.prod:9092
    - kafka-3.prod:9092
  RetentionMs: 86400000  # 24h(已是默认)
```

### Step 3: 创建 topic 时指定 replication=3

修改 `go/shared/kafkautil/topic_init.go`:
```go
type TopicSpec struct {
    Name          string
    Partitions    int32
    RetentionMs   int64
    ReplicaFactor int16  // 生产传 3,本地传 1
}
```

login service 启动时传:
```go
kafkautil.EnsureTopics(brokers, []TopicSpec{{
    Name:          "db_task_zone_1",
    Partitions:    10,
    RetentionMs:   86400000,
    ReplicaFactor: 3,  // 生产
}})
```

> **分区数不可在线修改。** `db_task_zone_*` 已把 origin partition 写进永久
> applied cursor。`EnsureTopics` 现在只接受精确 partition 契约并创建不可变 marker，
> 不再调用 `CreatePartitions`。扩容须按
> [`db-task-kafka-partition-contract.md`](../design/db-task-kafka-partition-contract.md)
> 停写、排空，并同时切换 login/db 的 `TopicGeneration`。

### Step 4: 旧数据迁移(只在切换时做)

如果之前有单 broker 跑过的数据需要保留:
```bash
# 用 mirror-maker 把旧 broker 的消息复制到新集群
docker run --rm \
  apache/kafka:latest \
  /opt/kafka/bin/kafka-mirror-maker.sh \
    --consumer.config consumer.properties \
    --producer.config producer.properties \
    --whitelist 'db_task.*'
```

通常压测/开发环境不需要这一步。

### Step 5: 切流验证

1. 先把 1 个 zone 的 login service 指向新集群,观察 1 小时
2. 验证 `kafka-consumer-groups.sh --describe` 没有 lag
3. 验证 `db.stderr.log` 没有 send error
4. 全部 zone 切换

### Step 6: 下线旧 broker

```bash
docker compose stop kafka  # 旧的单 broker
```

---

## 回滚预案

**触发条件**: 任何一个出现:
- 集群 ISR 连续 5 分钟 < 2
- producer send 失败率 > 1%
- consumer lag 持续增长

**回滚步骤**:
1. 改 `login.yaml` Brokers 指回旧 kafka:9092
2. login service 滚动重启
3. 新集群保留 24h 用于事后分析,不立即删

---

## 监控指标(生产必加)

Prometheus 指标:
- `kafka_server_under_replicated_partitions`: 持续 > 0 = 集群病了
- `kafka_server_isr_shrinks_total`: 飙升 = 副本掉队
- `kafka_log_log_end_offset` vs `kafka_consumer_consumer_lag`: db service 消费速度
- `kafka_network_request_total{request="Produce"}` 失败率

告警阈值:
- ISR < 2 持续 1min → P0 告警
- consumer lag > 10000 持续 5min → P1 告警
- broker 磁盘 > 80% → P2 告警

---

## 容量规划

按"单 zone 100k 在线 + 多 zone 总和 1000 万"估算:

| 维度 | 单 zone 100k | 总和 1000 万(100 zone) |
|---|---|---|
| 每秒事件数 | ~100k(估算 1 事件/玩家/秒) | ~10M |
| 每事件大小 | ~200 字节(currency delta) | 同左 |
| 写入流量 | 20 MB/s | 2 GB/s |
| 24h 保留 | ~1.7 TB / zone | 170 TB |

**170 TB 不现实**,所以 **多 zone 部署必须每 zone 独立 Kafka 集群**:
- 每 zone 1 个 3-broker 集群,数据隔离
- 跨 zone 路由走 player_locator,不走 Kafka
- 每集群存储压力 ~1.7 TB / 24h,可控

如果实在要中心化 Kafka,retention 必须降到 1-2h,接受短窗口风险。

---

## 性能影响

replication=3 + acks=all 对比 replication=1:
- producer 写入延迟: +5-15ms(等待 2 个 ISR ack)
- 集群吞吐: 单 broker 1/3(因为每条消息要写 3 份)
- 网络流量: 3x(broker 间复制)

测试数据(行业典型):
- 1 broker 单分区: ~50 MB/s
- 3 broker replication=3 单分区: ~15-20 MB/s

我们当前用 10 个分区,对应:
- 1 broker: ~500 MB/s 上限(本地 docker 实际 ~50 MB/s)
- 3 broker: ~150-200 MB/s 单 zone(够用,单 zone 100k 在线只需 ~20 MB/s)

---

## 灾难恢复演练

每月做一次:
1. 杀掉 1 个 broker 容器: `docker stop kafka-2`
2. 验证服务无感知(producer 继续 send,consumer 继续消费)
3. 验证 `under_replicated_partitions` 告警触发
4. 启动 broker: `docker start kafka-2`
5. 验证 ISR 恢复到 3
6. 验证 lag 追平

---

## 相关文档

- `docs/notes/redis-oom-data-safety-qa-record.md` — P1 决策依据
- `docs/notes/player-data-loading-and-sharding-pain.md` — 数据分层架构
- `go/shared/kafkautil/topic_init.go` — topic 创建代码
- `go/login/etc/login.yaml` — 配置默认值

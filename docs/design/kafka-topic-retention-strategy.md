# Kafka topic 保留与耐久策略

> 更新：2026-10-23。本文是当前配置与专项设计的统一入口，替代 2026-04 的保留期说明。
> 以下是仓库配置事实，不代表线上部署或故障恢复已验收；项目上线状态见[存盘围栏设计](./db-write-fencing-mysql.md)文末。

## 1. 保留期、容量与耐久分别判断

消费完成不会立即删除 Kafka 消息，`retention.ms` 也不等于故障恢复窗口。还要核对每分区的
`retention.bytes`、日志段、磁盘容量与实际写入速率；时间和容量限制中先触发的限制可能删掉未消费积压。
长保留期不能代替持久卷、副本、生产者确认、消费提交及业务幂等。

节点命令、DBTask、审计与战斗结果具有不同恢复语义，不能统一按“即发即弃”处理。
收件箱等业务若已在 SQL 持久化，Kafka 可以只承担提醒；消息内容的可靠性由该业务的存储与补拉契约保证。

## 2. topic 分类与配置归属

`<N>` 表示配置中的世代号，不应照抄固定值；例如本机节点命令当前为 g2，而 Compose 初始化默认仍为 g1，启动前必须对齐。

| topic | 时间保留配置 | 配置与恢复语义 |
|---|---|---|
| `gate-cmd_g<N>`、`scene-cmd_g<N>` | 显式 1 小时 | 初始化脚本预建；节点 assign 固定分区、启动从末尾消费，不提交共享 offset。`target_instance_id` 必填；重启期间命令的恢复由上层负责。见[控制面分区契约](./control-plane-topic-partitioning-20260908.md)。 |
| `gate-<id>`、`scene-<id>` | 继承 broker 配置，除非另有 topic 覆盖 | 兼容消费路径，不再预建。本机 `DisableLegacyPerNodeTopic=false`，K8s 节点模板为 `true`；按目标环境核对。 |
| `game-events` | Compose 显式 1 小时 | 本机初始化保留的共享事件 topic；不能据此推断已有业务消费者。K8s 初始化 Job 只预建命令与审计 topic。 |
| `db_task_zone_<zone>`；世代 ≥2 带 `_g<N>` | 本机 Go 缺省 24 小时；K8s 按档位，见下节 | login 与 db 启动时共同声明相同的分区及保留配置。实际缓冲还受容量限制，投递、补投与盘预算见[DBTask 设计 §B](./db-task-delivery-and-retention.md)。 |
| `transaction_log_topic_g<N>`、`player_snapshot_topic_g<N>` | 配置缺省 30 天 | 初始化脚本预建，data_service 的 `EnsureTopics` 再核对；当前配置分区数分别为 6、3。未单独配置 `retention.bytes`，仍受 broker 容量缺省限制，不能保证实际保留 30 天。 |
| `match-results` | 显式 7 天 | match 与 guild 的结果消费入口声明；全球 SQL 模式还要求至少 3 副本、`min.insync.replicas≥2`、禁用非同步副本选主。见[战斗设计](./turn-based-battle-server.md)及[帮会试炼](./guild-phase2/06-activities.md)。 |
| `match-rating-snapshot` | 旧兼容评分链缺省 168 小时 | 仅 `RatingEnabled && !GlobalMatch.Enabled` 时声明，缺省 3 分区、发布关闭；不能当作当前全球排行榜的恢复链。配置见 `go/match/etc/match_service.yaml`。 |

player_inbox 没有独立的长保留 Kafka topic：内容在 SQL 中提交后，通过 `scene-cmd_g<N>` 提醒在线节点；
提醒可丢，落地拉取与在线补拉负责恢复。见[玩家收件箱设计](./player-inbox-service.md)。

## 3. 环境档位与存储

| 配置 | dev | prod-like | prod（K8s） |
|---|---|---|---|
| broker `log.retention.ms` | 1800000（30 分钟） | 3600000（1 小时） | 3600000（1 小时） |
| broker 每分区 `log.retention.bytes` | 134217728（128 MiB） | 536870912（512 MiB） | 536870912（512 MiB） |
| `log.segment.bytes` | 16777216（16 MiB） | 33554432（32 MiB） | 33554432（32 MiB） |
| `log.retention.check.interval.ms` | 120000（2 分钟） | 同左 | 同左 |
| K8s DBTask `retention.ms` | 3600000（1 小时） | 21600000（6 小时） | 86400000（24 小时） |
| K8s DBTask 容量缓冲目标 | 1 小时 | 2 小时 | 2 小时 |

Compose 的 dev / prod-like 参数来自 `deploy/env/kafka.dev.env`、`deploy/env/kafka.prod-like.env`；
它们只控制 broker 档位，不会自动修改本机 login / db 的 topic 配置。本机 DBTask 的
`RetentionBytes` 缺省为 0，表示不管理该项，已有 topic 覆盖不会自动被清除；没有覆盖时继承 broker。
K8s 参数由 `tools/scripts/k8s_deploy.ps1` 的 `Apply-KafkaProfileDefaults` 与盘预算共同渲染，显式参数可覆盖档位。

K8s 的 DBTask `RetentionMs`、`RetentionBytes` 从同一组变量写入 login 与 db 的 ConfigMap，避免两个服务启动时互相改值。
prod / prod-like 的容量按约 2 小时目标估算，盘不足还会进一步收缩；不能把 24 小时 / 6 小时当成恢复保证。
完整公式、设计速率、收缩告警与扩盘流程统一见 [DBTask 设计 §B](./db-task-delivery-and-retention.md)
和[部署手册](../../deploy/k8s/README.md)的“Kafka 磁盘预算与 db_task retention.bytes”。

- **Compose**：`kafka-data` 命名卷挂在 `/tmp/kraft-combined-logs`。另有 `/tmp/kafka-logs` 的 tmpfs 挂载，不能因此称 Kafka 日志“只在内存、零磁盘 I/O”。本机单 broker 不提供副本容错。
- **K8s**：StatefulSet + 每 broker PVC，`KAFKA_LOG_DIRS=/var/lib/kafka/data`；不是 emptyDir。broker 数、副本数与最小同步副本数由部署拓扑统一渲染，详见[部署手册](../../deploy/k8s/README.md)的“Kafka:多 broker”。

## 4. 初始化与变更纪律

1. 命令与审计 topic 应在生产者启动前预建。Compose 的 `kafka-topic-init`、K8s 的 `kafka-topic-init.yaml` 都核对分区数并重新声明、读回保留期；不能依赖 broker 自动建出的单分区 topic。
2. Go 入口使用 [`kafkautil.EnsureTopics`](../../go/shared/kafkautil/topic_init.go)：创建缺失 topic，核验精确分区数、不可变分区标记与副本数；用增量接口设置受管理的 `retention.ms` / `retention.bytes`，保留其它动态配置。`RetentionBytes<0` 拒绝，0 不管理，正值显式设置。
3. `RequireDurability` 路径必须显式要求至少 3 副本，逐分区核验副本分配，并设置 `min.insync.replicas≥2`、禁用非同步副本选主；设置后检查管理 API 返回，未另行读回核验。普通路径不自动获得这一保证，初始化不会替已有 topic 自动补副本。
4. 分区数是寻址与顺序契约，不在同名 topic 上扩分区。命令换代按[控制面契约](./control-plane-topic-partitioning-20260908.md)，DBTask 停写、排空与换代按[DBTask 分区契约](./db-task-kafka-partition-contract.md)执行。
5. 下调保留时间或容量可能删除尚未处理的积压。先核对消费进度、业务恢复要求与盘预算，再按对应手册调整；保留期到期只清理数据，不会自动删除 topic 元数据。

新增可靠消息链时，先明确内容权威存储、丢失后的补偿或重放方式，再指定 topic 的时间、容量、分区、副本与初始化归属。
本文登记配置入口，业务的提交点、确认与幂等规则留在各自设计中。

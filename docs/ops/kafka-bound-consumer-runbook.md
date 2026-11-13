# Kafka 消费组 owner 运维手册:死信重放、保留期丢失与排空 HOLD

> 适用于用 `go/shared/kafkautil` 的 `BoundConsumerGroup` 消费的组:guild 的 `guild-trial`、match 的 `match-rating`、
> data_service 的两个审计组。机制见 [kafka-bound-consumer-group.md](../design/kafka-bound-consumer-group.md)。
> 告警规则:`deploy/k8s/kafka-bound-consumer-alerts.yaml`、`guild-trial-result-consumer-alerts.yaml`、`match-result-consumer-alerts.yaml`、
> `data-service-audit-alerts.yaml`。2026-11-12 首版,工具与告警**未经运行验证**。

## 0. 告警对照

| 告警 | 先看 | 处置 |
|---|---|---|
| `KafkaBoundConsumerDeadLettered` | 日志 `[kafka-bound] ... record written to the group's dead-letter topic`(原位置、次数、原因) | §1 |
| `KafkaBoundConsumerDeadLetterWriteFailing` | 日志 `[kafka-bound]` 里 stage=dead_letter 的错误 | §1.6 |
| `KafkaBoundConsumerRetentionLoss` | 日志 `RECORDS DELETED BY RETENTION BEFORE THIS GROUP PROCESSED THEM` | §2 |
| `KafkaBoundConsumerHold` | 日志 `[kafka-bound]` 与服务自己的 HOLD 行 | §3、§4 |
| `GuildTrialResultConsumerLagging` / `MatchResultConsumerLagging` / `DataServiceAuditConsumerStalled` | 有积压的分区上位点 15 分钟没动 | 各自规则的 description;卡在毒丸上见 §1 |

## 1. 死信:查看与人工重放

### 1.1 先弄清

- 死信 topic 名是「消费组名-dlq」(例如 `guild-trial-dlq`),1 个分区,保留 90 天。原 topic 上的位点已越过死信里的记录:**它们没被处理**。
- `cause=exhausted`:回调失败到预算(guild-trial 20 次,约 7.5 分钟)。多半是依赖(TiDB、Redis、号段)长时间不可用,也可能是这一条触发了 bug。
- `cause=unprocessable`:记录本身处理不了(解不出、违反契约)。要先修生产者或解码,否则重放只会再撞一次。
- 重放把原件**原样写回原 topic 的原分区**:原 topic 上**所有**消费组都会再收到它。本仓的消费组都幂等
  (guild 结算有对局终态闸门、match 有 SQL 入账收据、data_service 按主键 / guid 去重),重复投递只会回「已处理」。
  但 `match-results` 上的 `match-rating` 不开死信、遇解码失败保留位点:**不要把解不出的原件写回 `match-results`**,会把评分消费组卡住。
- 死信 topic 只追加,重放过的条目不会消失:每次重放都要把死信 offset 区间记进处置记录,避免重复重放(重复也幂等,但没必要)。

### 1.2 修好原因

依赖故障先等它恢复;是 bug 先修代码并发布到全部副本。guild-trial:已登记的历练对局多半已由巡检器在结果超时(420s)后从 match 的
SQL 终局结算过,重放会走幂等闸门回「已结算」;没有登记行的只能靠重放。

### 1.3 列出

工具在 `go/shared/cmd/kafka_dlq`,需要能直连 broker 的机器(K8s 上在集群内的运维 Pod 里跑,或先把 broker 端口转发出来)。只读:

```
cd go/shared
go run ./cmd/kafka_dlq list -brokers <broker1:9092,broker2:9092> -group guild-trial
```

每行:`dlq_offset=<死信 offset> original=<原 topic>[<分区>]@<原 offset> group=... cause=... attempts=... dead_lettered_at=... key=... value_bytes=... reason=...`。
同一原位置出现多次(转死信后提交失败、新属主又转了一次)会标 `duplicate_of_dlq_offset=...`。可用 `-from` / `-to` 限定死信 offset 区间(含两端)。

### 1.4 演练与执行

```
go run ./cmd/kafka_dlq replay -brokers <...> -group guild-trial -from <起> -to <止>             # 演练:只打印将写回的原位置
go run ./cmd/kafka_dlq replay -brokers <...> -group guild-trial -from <起> -to <止> -execute    # 执行
```

- 必须显式给 `-from` / `-to`;区间里有别的组的死信、解不出的条目时整批拒绝,不写任何一条。
- `cause=unprocessable` 默认拒绝;确认原因已修、而且原 topic 上每个消费组都能解开它之后,才加 `-allow-unprocessable`。
- 同一原位置只写一次;写回原分区(分区已不存在就拒绝)、`acks=all`、逐条同步写。中途失败会报已写条数,从失败那条起重跑(已写的重复也幂等)。
- 执行后把区间与时间记进处置记录。

### 1.5 验证

- 消费者日志里这些对局 / 记录被处理(guild:`[GuildTrial]` 结算日志;match:评分与归档日志);
- `kafka_bound_consumer_dead_lettered_total` 没有因为它们再涨(再涨说明原因没修好);
- 积压回落,`*ConsumerLagging` / `*Stalled` 不再响。

### 1.6 死信写不进去(`KafkaBoundConsumerDeadLetterWriteFailing`)

记录留在原处、退避重写,所在分区停在这一条上,不越过、不跳过。查日志里 stage=dead_letter 的错误:

- 死信 topic 不存在:服务启动时 `EnsureTopics` 会建它(guild 在启动门禁与消费者启动时都确保);核对 `kafka-topics.sh --describe --topic guild-trial-dlq`;
- ISR 不足:`acks=all` 写不进,先修 broker;
- broker 不可达:Kafka 侧故障。

修好之后自动写入并继续消费,不用重启。

## 2. 保留期删掉了本组还没处理的记录(`KafkaBoundConsumerRetentionLoss`)

**数据已丢**。owner 不静默跳过,而是 HOLD 并永久停止取新;之后每次打开该分区都会因「提交点越过保留范围」再次 HOLD
(data_service 会反复退出)。处置:

1. 从日志 `RECORDS DELETED BY RETENTION BEFORE THIS GROUP PROCESSED THEM: expected offset A, log start B ...` 记下 topic、分区、`[A, B)`,上报负责人。
2. 按业务补:data_service 审计见 [audit-topic-generation-bump-runbook.md](audit-topic-generation-bump-runbook.md) §7(条数口径与上报);
   guild-trial 已登记的对局由巡检器兜底,没有登记行的按 match 终局行人工核对;match-rating 按
   [cross-zone-matchmaking.md](../design/cross-zone-matchmaking.md) §13.4 人工核对来源与入账。
3. 停掉该消费组的全部消费者(位点重置要求组内没有活跃成员):K8s 上把对应工作负载缩到 0(data_service 还承载 Load/Save 热路径,
   要在维护窗口做);本机停进程。
4. 先演练再执行,把该分区的组内位点重置到日志起点:
   ```
   kafka-consumer-groups.sh --bootstrap-server <b> --group <组> --topic <topic>:<分区> --reset-offsets --to-earliest --dry-run
   kafka-consumer-groups.sh --bootstrap-server <b> --group <组> --topic <topic>:<分区> --reset-offsets --to-earliest --execute
   ```
   只动报出的那个分区,不要整 topic 重置。
5. 起消费者,确认 `kafka_bound_consumer_retention_loss_total` 不再涨、积压回落。
6. 查根因:通常是消费者长时间停摆或卡住、积压越过了 `retention.bytes` / `retention.ms`(先看当时的停滞与积压告警)。

## 3. 维护排空时收据 HOLD:交还 / 恢复票据没核到

日志:`[kafka-bound] group=<组>: N ticket(s) not confirmed by the group's committed offset within the drain budget; receipt HOLD: handoff <topic>[<分区>]@<offset>, ...`。

含义:owner 终止时有没结清的记录(在途、退避中,或旧代次提交被拒)交给了新属主,或分区漂走的恢复票据没回来;
收尾约 30 秒内读组内已提交位点,一直没越过它们。常见原因是**同一服务的全部副本同时关闭 RELAY**,交出去的记录没有还在跑的副本接手;
也可能是还在跑的副本自己卡住(看它的积压与 `[kafka-bound]` 日志)。

处置:按排空流程把这次 run 记为 ABORTED_CLOSED,重试同一 run。重试时逐个副本关闭 RELAY:前一个副本的收据成立之后再关下一个,
最后一个副本的代次不会再被别人离组结束,它的在途记录用自己的代次提交。P4 仍以 broker 上 committed == end 核全组排空。

## 4. 排空之外的 HOLD(`KafkaBoundConsumerHold`)

owner 记了 HOLD 就永久停止取新(match、guild 进程继续跑其它功能,data_service 退出由编排重启)。按日志原因处置:

| 日志原因 | 处置 |
|---|---|
| 保留期删掉了没处理的记录 / 提交点越过保留范围 | §2 |
| reader 回退、分区或 offset 与代次不符、assignment 为负或重复 | 不该发生:保留日志与该时刻的 `kafka-consumer-groups.sh --describe` 输出,报开发;重启服务恢复消费 |
| 确定性恢复票据没被提交就被保留期删掉 | 同 §2(组在该分区从没提交过,丢失范围看日志) |
| 结清过程 panic | 保留栈(`goroutine_panic` 日志),报开发,重启服务 |

修复后重启该服务;重启前确认 §2 的情形已重置位点,否则会再次 HOLD。

## 5. 指标与日志速查

- `kafka_bound_consumer_retry_total{group,stage}`:原地重试,stage = join / open / fetch / handle / dead_letter / commit;
- `kafka_bound_consumer_released_total{group}`:交还重投(含排空时交给新属主);
- `kafka_bound_consumer_dead_lettered_total{group,cause}`:写进死信的记录;
- `kafka_bound_consumer_retention_loss_total{group}`:保留期丢失(已 HOLD);
- `kafka_bound_consumer_hold_total{group}`:HOLD;
- 积压:`guild_trial_result_consumer_*`、`match_result_consumer_*`、`data_service_kafka_consumer_*`(lag、单分区最大 lag、保留期缺口、提交停滞秒数)。
- 日志前缀 `[kafka-bound]`;服务自己的 HOLD 行:match `[rating] result consumer HOLD`、guild `[GuildTrial] Trial result consumer HOLD`。

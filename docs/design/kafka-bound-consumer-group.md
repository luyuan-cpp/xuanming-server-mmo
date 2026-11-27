# Kafka 消费组 owner(BoundConsumerGroup):再均衡、暂时失败、死信与排空收据

> **状态**:现行机制。2026-11-05 r2-kc#1 首版(owner 跨代次存活),2026-11-12 收口当时记下的残余(终止时交还给新属主、
> 收尾核组内位点、有界重试后转死信、保留期丢失不静默跳过、reader 模式交还后结束代次、guild-trial 的 lag 告警)。
> 2026-11-12 的改动**未编译、未测试**,待 Gemini 验证(命令见 `docs/PROGRESS.md` 同日条目)。
> **代码**:`go/shared/kafkautil/group_lag.go`(`BoundConsumerGroup`)、`bound_consumer_dead_letter.go`、`bound_consumer_tickets.go`、
> `bound_consumer_retention.go`、`bound_consumer_metrics.go`、`group_lag_probe.go`;死信工具 `go/shared/cmd/kafka_dlq`。
> **处置手册**:[kafka-bound-consumer-runbook.md](../ops/kafka-bound-consumer-runbook.md)。
> 本文取代 [db-write-fencing-mysql.md](db-write-fencing-mysql.md) 末尾「Kafka 消费组 owner 的再均衡与暂时失败」一节(那份文档已整体标为历史)。

## 1. 用途与调用方

`BoundConsumerGroup` 把一个 Kafka 消费组在本进程里的全部所有权收在一个 owner:组代次、分区 reader、已接纳记录、提交,
以及维护排空要的收据(经 `go/shared/drainproof` 汇入 RELAY)。两种用法:

| 模式 | 构造 | 调用方 | 回调 / 批处理 |
|---|---|---|---|
| handler | `NewBoundConsumerGroup` | guild 历练结果(组 `guild-trial`)、match 评分与结算来源归档(组 `match-rating`) | owner 逐条调回调,成功后逐条同步提交 |
| reader | `NewBoundMessageReader` | data_service 审计落库(流水、快照两个组) | 调用方 `FetchMessage` 取、自己攒批落库,`CommitMessages` 交「已持久前缀」 |

不变的前提:回调与批处理落库都**幂等**。所以「没提交」是确定结局(组内已提交位点停在它前面,之后一定重投),不是未知结局。

## 2. 所有权与代次

- 一个 owner 跨越本进程的全部代次。kafka-go 是 eager 再均衡:任一副本入组、离组、重启都结束全组代次;
  一个 `Generation.Start` 回调返回,该代次就结束(本副本随即重新入组,触发再均衡)。
- `Run` 等下一代次失败(协调者不可达、join / sync / 读位点失败)只计数、记日志、有界退避后继续等;分区打开失败(leader 不可读)
  与拉取失败退避后重试。都不结束 owner。
- 打开分区时核组内已提交位点在保留范围内:早于日志起点或晚于日志末端(保留期删掉了没消费的记录、topic 重建)→ HOLD(§7)。
  早于日志起点的那种同样计 `kafka_bound_consumer_retention_loss_total`、打 `RECORDS DELETED BY RETENTION ...`(2026-11-26),
  与代次中途发现的缺口走同一条 critical 告警;晚于日志末端不是保留期丢失,只 HOLD。
- 退避:1s 起翻倍、封顶 30s,生产等待带抖动(`sleepWithJitter`,[d/2, d])。

## 3. 一条已接纳记录的结局

handler 模式下 `FetchMessage` 返回、且代次还在,记录就被接纳(拿一张 relay 票据)。结局只有这几种:

| 结局 | 条件 | 提交 | 票据 | 收据计数 |
|---|---|---|---|---|
| 结清 Settled | 回调成功(或已写成死信)且以本代次提交成功 | 是 | 立即结束 | Settled(死信另计 DeadLettered) |
| 交还 Released | 没结清,owner 仍在运行而本代次已不再消费 | 否 | 立即结束 | Released |
| 交给新属主 HandedOff | 没结清而 owner 已终止(StopFetching 或 Run 的 ctx 结束) | 否 | 留到收尾核结局(§6) | Released 且 HandedOff |
| HOLD | 不变量破坏、保留期丢失、结清过程 panic、收尾核不到 | 否 | 先记 HOLD 再结束 | 收据 HOLD |

- 代次的 ctx 已结束也照常用旧代次提交一次:eager 再均衡准备期 broker 仍接受旧代次的提交,本代次的协调者连接要等全部 Start 回调返回才关。
  真被拒(IllegalGeneration、UnknownMemberId、RebalanceInProgress)才按交还处理。
- 代次已结束时刚拉到的记录不接纳;只是 StopFetching 而代次仍在时照常接纳(Fetch 已返回即归本 owner)。
- 收据逐代核 `Accepted == Settled + Replayed + Released`,滚出窗口的旧代次折叠进 `Compacted` 时同样核平衡。

### 3.1 从没提交过的分区:接纳第一条之前先提交起点(2026-11-19)

打开分区时 assignment 不带位点(组在这个分区从没提交过,`FirstOffset`),reader 从日志起点读。**接纳第一条记录之前,先用本代次把这条记录的 offset
提交成组内已提交位点**(`commitStartOffset`;handler 模式与 reader 模式都在把记录交出去之前做),提交成功才接纳。负责人 2026-11-19 定。

- 为什么:不提交的话,这条接纳之后没结清就换代或换属主,新属主又从日志起点读起;起点若已被保留期推过它,就静默越过、谁都没处理过,
  恢复票据(§8)和收尾核对(§6.2)也核不出来。提交之后它再被删掉,下一位属主打开时按「提交点越过保留范围」HOLD(§2、§7),丢失变成看得见的 HOLD。
- 起点取这条记录自己的 offset,不取打开时的 initial:代次还没读到任何记录就结束时不钉住起点(不会因为自己提交过、起点又被推过去而误 HOLD);
  读到之前起点被推过去(§7 第三条照常接纳)也不会把已删的那段算成本组欠下的。
- 提交失败按 §2 的退避重试,直到成功、代次结束或 owner 终止;后两种不接纳这条(没有票据),之后由组内位点(或日志起点)重投。
- 代价:每个新组(或新一代 topic)在每个分区上多一次提交。

## 4. 原地重试

代次还在、owner 没终止时,失败(回调返回错误、超过 HandlerTimeout、panic,或提交失败)按 §2 的退避原地重试同一条:

- 回调成功过(或死信写成功过)就只重试提交,不再调回调;
- 退避被代次结束或 owner 终止打断时,不再调回调、不再写死信(新属主可能已在处理同一条),按 §3 判交还 / 交给新属主;
- 回调 panic 由 safego 记栈与计数,折成这一次失败,不记 HOLD(guild 2026-11-12 起同样,见 §10);
- **没打开死信时不设次数上限**(match 结果消费的取舍,§5.5);打开了死信时按 §5 有界。

## 5. 有界重试后转死信

### 5.1 触发

`BoundConsumerGroupConfig.DeadLetter` 打开(只支持 handler 模式)。同一条在本 owner 的这次交付里:

- 回调失败累计到 `MaxAttempts` 次(缺省 20,上限 1000;guild-trial 取 20,退避带抖动,约 4–7.5 分钟);或
- 回调用 `%w` 返回 `kafkautil.ErrUnprocessable`(记录本身处理不了:解不出、违反契约),第一次就转。

预算只在本 owner 的这次交付里累计:代次结束交还重投后,新属主从 0 重计(不做跨副本的持久计数)。

### 5.2 写成功才越过

- 先写死信,写成功才以本代次提交越过它(收据 Settled,另记 DeadLettered);
- 写失败(死信 topic 不可用、ISR 不足、panic)就留在原处,按 §2 退避重写,不再调回调;代次结束 / owner 终止照 §3 交还;
- 死信写成功但随后提交失败、交还重投时,新属主可能再写一份:死信里会有同一原位置的两条,重放工具按原位置去重。

### 5.3 存储:新增死信 topic `<消费组>-dlq`

选新增死信 topic,不用各服务自己的库表,理由:

- 死信原件本来就是一条 Kafka 记录:key、value、headers 原样保存,重放就是原样写回原 topic 的原分区,不用给每个服务各设计一张表、
  各写一份迁移与写者闸门(逻辑库自治,AGENTS §7 第 6 条);
- 与原 topic 同一套耐久契约(`DeadLetterTopicSpec`:原 topic 要求严格耐久时三副本、minISR 2;1 个分区),
  不依赖消费者自己那个多半正在出故障的库;
- 单条上限 2MiB(topic 的 `max.message.bytes` 与写入端的单批上限,2026-11-26):死信比原件多 `dlq.*` 头(约 1.5KB),
  原 topic 沿用 broker 缺省上限(约 1MiB)时,贴着上限的原件加头之后写不进同样上限的死信 topic,分区会停在这一条;
- 保留 90 天,远长于原 topic 的 7 天:人工处置按周算;
- 按消费组隔离:一个 topic 可以被几个组读(`match-results` 有 `match-rating` 与 `guild-trial`),某条可能只对其中一个组是毒丸。

代价:依赖长时间不可用时,在途记录各自用完预算后也会转死信(guild-trial 约 4–7.5 分钟一条),恢复后要按手册重放。
告警 `KafkaBoundConsumerDeadLettered` 对第一条就响。

### 5.4 死信消息格式

key、value 与原 headers 原样(原件里旧的 `dlq.*` 头先去掉),再追加:

| 头 | 内容 |
|---|---|
| `dlq.original.topic` / `dlq.original.partition` / `dlq.original.offset` | 原位置 |
| `dlq.original.timestamp.ms` | 原件时间戳(原件没有时不写) |
| `dlq.group` | 原消费组 |
| `dlq.cause` | `unprocessable` / `exhausted` |
| `dlq.attempts` | 回调失败次数 |
| `dlq.reason` | 最后一次失败的原因(截到 1024 字节,不切开 UTF-8 字符) |
| `dlq.dead.lettered.at.ms` | 写死信的时刻(也是死信消息的时间戳) |

编解码:`kafkautil.DeadLetterMessage` / `kafkautil.ParseDeadLetter`。写入端 `KafkaDeadLetterSink`:同步、`RequiredAcks=all`、
不自动建 topic(调用方启动时与原 topic 一起 `EnsureTopics`)、单次只试一次(重试由 owner 做)。

### 5.5 各消费组的取舍

| 消费组 | 死信 | 理由 |
|---|---|---|
| `guild-trial` | 打开,20 次 | 毒丸以前会卡住本副本名下的分区;已登记的历练由巡检器在 ResultOverdue(420s)后从 match 的 SQL 终局兜底结算;20 次约 4–7.5 分钟(退避带抖动),与巡检器谁先谁后无关,重放走幂等闸门 |
| `match-rating` | **不开** | 负责人 2026-11-06 定([cross-zone-matchmaking.md](cross-zone-matchmaking.md) §13.4):解码失败与同局原字节冲突保留位点、告警、人工处置,未处置前分区不前进,不新增丢弃或毒丸表路径 |
| data_service 两个审计组 | 由批处理自己写(reader 模式的 owner 不支持死信,2026-11-26 起) | 坏消息(解不出、主键为 0、超列宽)不落库,原件先写进本组死信「消费组名-dlq」再提交,格式与重放工具同上,见 §15.6;记 ERROR 与 `decode_error / invalid / oversize` 计数,告警 `DataServiceAuditMessagesSkipped` |

### 5.6 人工重放

`go/shared/cmd/kafka_dlq`:`list` 只读列出;`replay` 默认演练,`-execute` 才把原件写回原 topic 原分区(acks=all),同一原位置只写一次。
`cause=unprocessable` 默认拒绝(写回只会再撞一次,而且同 topic 上不开死信的组会被卡住),修好原因后带 `-allow-unprocessable`。
步骤见手册。

## 6. 终止与排空收据

### 6.1 逐个离组

维护排空时各副本相继调 `CloseAndReport`(内部 StopFetching),相继离组;每次离组都结束其余副本的代次。owner 终止之后:

- 不再取新,也不再调回调;正在进行的那次回调 / 提交照常做完;
- 回调已成功、只差提交的记录,代次还在时先短重试几次提交(同 reader 模式:3 次、200ms 起翻倍),仍失败才交还(2026-11-26)。
  最后离组的副本没有新属主接手,瞬时提交失败直接交还会让收据 HOLD;代次已结束的提交被拒不会因重试变好,不试;
- 没结清的记录(在途、退避中,或回调成功后旧代次提交被拒)**交给新属主**:不提交、不记 HOLD,票据留着(§3 表);
- reader 模式同理:终止时 `CommitMessages` 照常有界重试(代次还在时),仍失败这批交给新属主,返回 `*UncommittedError`(调用方照旧摘掉在途集合)。

### 6.2 收尾核对

`CloseAndReport` 等 `Run` 返回(owner 已离组)之后,对两类票据读组内已提交位点(只读 coordinator 的 OffsetFetch):

- 交给新属主的记录(§6.1);
- 确定性恢复票据(reader 模式 `ProcessingNotCommitted`)所在分区漂到别的副本、不再回来的。

位点越过了它、**且组在该分区确实提交过**(位点 >= 0;-1 说明组从没提交过,越过它的只可能是保留期,同 R5 的 groupCommitted 判据),
**且票据产生时组在该分区已有真实的已提交位点**(打开时 assignment 就带着,或本 owner 在该代次提交过;`boundPartition.hasCommitFrontier`)
→ 别的属主处理并提交了它:结清,计入同一次排空的收据(交还的记作 `ResolvedHandoffs`,恢复票据记作 `GROUP_COMMITTED_PAST_AT_CLOSE`)。
最多读 30 轮、每轮间隔 1s(调用方 ctx 先到期以它为准);仍核不到的才 HOLD(先记 HOLD 再结束票据)。

第三个条件是 2026-11-19 评审补的:票据产生时组在该分区没有已提交位点,新属主就从日志起点读起;起点若已被保留期推过票据,
它提交的位点照样 >= 0 且越过票据,却从没处理过它。这种票据核不出结局,只能 HOLD。同日起接纳第一条之前先提交起点(§3.1),
已接纳记录的票据恒有位点,这一条留作兜底,正常走不到。

所以正常的逐个离组不让收据变 HOLD:被拒的那条由还在跑的副本重投、结清,结清经组内位点计入同一次排空。
所有副本同时终止、交还的记录没有新属主时,收据照旧 HOLD(fail-closed),按 ABORTED_CLOSED 重试同一 run;
建议逐个关闭 RELAY(手册 §3)。P4 仍以 broker 上 committed == end 核全组排空,本机收据不替代它。

### 6.3 仍然 HOLD 的情形

reader 返回的分区或 offset 与代次不符、reader 回退;assignment 为负或重复;分区 reader 在 worker 退出前被关闭;组被提前关闭;
组内已提交位点越过保留范围(打开时)或保留期删掉了本组还没处理的记录(代次中途,§7);确定性恢复票据没被提交就被保留期删掉;
relay 已关闭仍拉到新记录;结清过程本身 panic;收尾核不到交还 / 恢复票据的结局。

P8 / P9 的活代次复核不变:P8 之后发生再均衡,`ValidatePrepared` / `WithConsumerReadiness` 仍回 HOLD、拒绝开放,但消费继续。

## 7. 保留期删掉了本组还没处理的记录

kafka-go 的分区 reader 遇 OffsetOutOfRange(读的位点早于日志起点)会自己跳到日志起点,(重新)初始化时位点早于起点也会静默改到起点。
owner 每取到一条都核它是否紧接着上一条(应到位点从 initial 起逐条推进);有缺口就读一次日志起点:

- 起点 <= 应到位点:缺口不是保留期删的(事务控制记录、压缩),照常接纳;
- 起点 > 应到位点,且这段位点归本组(组在该分区提交过,或本 owner 已从这里取到过记录):中间那段没被本组处理就删了 ——
  HOLD、停止取新、计 `kafka_bound_consumer_retention_loss_total`、打 ERROR(`RECORDS DELETED BY RETENTION ...`),不接纳、不提交;
- 组在该分区从没提交过、本 owner 也还没取到过一条(新组从日志起点读,打开之后起点才被推过去):没有越过本组的已提交位点,只打 ERROR、照常接纳。
  接纳这一条之前照 §3.1 先把它的 offset 提交成起点。

之后每次打开该分区都会因「提交点越过保留范围」再次 HOLD,直到人工按手册记录丢失范围、重置位点。
§3.1 的起点提交之后,接纳过的记录在结清之前被删掉,下一位属主同样在打开时 HOLD(2026-11-19 之前这种情形会被下一代次从新起点读起静默越过)。

## 8. reader 模式(data_service)

- `CommitMessages` 只推进本代次实际交付、调用方确认已持久的前缀;代次的 ctx 已结束也照常用交付它们的代次提交(`waitDelivered` 让该代次等这些记录结清才关)。
- 代次还在时提交失败先有界重试 3 次(200ms 起翻倍,持 commitMu 所以要短;owner 已终止也照样重试)。仍失败:
  - owner 仍在运行 → 这批交还(`*UncommittedError`),**并结束本代次**(2026-11-12):本代次不会再投它们,不结束的话安静分区上
    已落库却没提交的几条会一直挂着积压,直到同分区来了新消息;结束本代次让下一代次立即从组内已提交位点重投(落库幂等);
    连续几个代次都因此被自己结束时(提交一直失败而入组正常),第二次起下一代次先退避再消费(1s 翻倍、封顶 30s,批提交成功清零,
    2026-11-26):不退避就会每轮重做同一批落库,本成员是 leader 时每轮还触发一次全组再均衡。代次已被再均衡结束时提交被拒不计;
  - owner 已终止 → 交给新属主(§6)。
- `ProcessingNotCommitted` 的确定性恢复票据:分区回到本 owner 时组内已提交位点已越过它(且组提交过)→ `GROUP_COMMITTED_PAST`;
  分区不回来 → 收尾核对(§6.2);组从没提交过而日志起点越过它 → HOLD(没被处理就被保留期删了)。`ProcessingOutcomeUnknown` 永久 HOLD。
- data_service 的 owner HOLD 经 `ConsumerOwner.Errors()` 让进程退出、由编排重启(fail-closed)。

## 9. 可观测

| 指标 | 类型 | 说明 |
|---|---|---|
| `kafka_bound_consumer_retry_total{group,stage}` | counter | 原地重试;stage:join / open / fetch / handle / dead_letter / commit |
| `kafka_bound_consumer_released_total{group}` | counter | 交还重投(含交给新属主) |
| `kafka_bound_consumer_dead_lettered_total{group,cause}` | counter | 写进死信的记录;cause:unprocessable / exhausted;只给开了死信的组预置 |
| `kafka_bound_consumer_retention_loss_total{group}` | counter | 发现保留期删掉了本组还没处理的记录(已 HOLD);代次中途发现缺口与打开分区时发现提交点落在日志起点之前都计 |
| `kafka_bound_consumer_hold_total{group}` | counter | owner 记 HOLD |

label 只有组名(配置常量)与固定的 stage / cause,不含分区、offset、玩家或对局号;构造 owner 时预置为 0。日志前缀 `[kafka-bound]`。
告警:`deploy/k8s/kafka-bound-consumer-alerts.yaml`(死信、死信写不进、保留期丢失、HOLD)。

消费组积压由各服务进程内的 lag 探针每 30s 从 broker 读出(`kafkautil.RunGroupLagProbe` / data_service 的 `runLagProbe`),与消费本身无关。
提交停滞 `CommitStall` 的判据是「有积压的分区上(组内已提交位点低于日志末端)已提交位点多久没动」,按分区算、不依赖新消息到达
(2026-11-12 去掉了 data_service 用过的 `RequireArrivals`:它只认位点停下之后新到的消息,收不到新消息的分区卡住看不见;
根因是 reader 模式交还后挂着的积压,§8 已修)。

| 消费组 | 告警文件 | 规则 |
|---|---|---|
| `match-rating` | `match-result-consumer-alerts.yaml` | `MatchResultConsumerLagging`(停滞 >= 15 分钟)、`...RetentionGap`、`...LagProbeBlind` |
| `guild-trial` | `guild-trial-result-consumer-alerts.yaml`(2026-11-12 新增) | `GuildTrialResultConsumerLagging`、`...RetentionGap`、`GuildTrialResultLagProbeBlind` |
| data_service 两个组 | `data-service-audit-alerts.yaml` | `DataServiceAuditConsumerStalled`(停滞 >= 15 分钟且 up)等 |

## 10. 调用方要点

- **guild**(`go/guild/internal/kafka/trial_result_consumer.go`):生产路径每次回调只试一次,重试交给 owner;解不出回 `ErrUnprocessable`;
  handler panic 不记 HOLD;死信 `guild-trial-dlq`、20 次;启动时与结果 topic 一起确保死信 topic(`TopicSpecs`,启动门禁 `ensureTrialResultTopic` 同样);
  lag 探针在 `runBound` 里起、返回前 join。
- **match**(`go/match/internal/kafka/result_consumer.go`):只有 owner 一条路径,缺 RelayAdmission / BindOwner 拒启(2026-11-12 删掉了不进关停 join 的自建 reader 循环);
  不开死信;一次回调内 `processResultRecord` 最多 3 次(写闸冻结不占次数),解码失败与原字节冲突立即返回、不做业务重试。
- **data_service**(`go/data_service/internal/kafka`):reader 模式;`ownedReader` 收到 `*UncommittedError` 只摘掉在途集合与 `ConsumerOwner` 票据,不 hold。

## 11. 回归用例

`go/shared/kafkautil/bound_consumer_test.go`、`bound_consumer_residual_test.go`(全部用替身:不连 Kafka,退避与收尾轮询经 sleep 替身,不等墙钟):
在途再均衡后继续消费、暂时失败原地恢复、入组与拉取失败不停、打开失败与越过保留期、历史窗口有界;排空时交给新属主并按组内位点核到 / 核不到;
逐个离组时旧代次提交被拒;reader 模式交还后结束代次、下一代次重投;漂走的恢复票据收尾核对(越过 / 没越过 / 组没提交过);
有界重试后转死信、无法处理直接转、死信写失败留在原处、panic 转死信不 HOLD、不开死信时原地重试;死信消息编解码;
保留期删掉没处理的记录 HOLD、非保留期缺口照常接纳、新组起点被推过去照常接纳、reader 回退 HOLD;提交停滞不依赖新消息;
从没提交过的分区接纳第一条之前先提交起点、起点提交不成功不接纳(handler 与 reader 两种模式);没有位点的票据不按组内位点结清;收尾核对有时间上限;
reader 模式连续自己结束代次时退避;owner 终止而代次还在时只差提交的记录先短重试提交(handler 与 reader 两种模式);死信单条上限。
另有 `go/shared/cmd/kafka_dlq/main_test.go`、guild `trial_result_bound_test.go`、match `result_consumer_test.go`,
以及 `tools/scripts/tests/k8s_deploy_contract.tests.ps1` 的三份告警契约。

## 12. 残余与取舍

- 死信预算只在本 owner 的一次交付里累计;每次再均衡重计。毒丸在频繁再均衡的组上转死信会更晚,不会更早。
- 依赖长时间不可用时会陆续转死信(§5.3 代价),恢复后人工重放;没有自动重放。
- 所有副本同时终止且交还的记录没有新属主时收据 HOLD,按 ABORTED_CLOSED 重试同一 run(§6.2)。
- match 的毒丸仍停在原位(负责人已定,§5.5),靠 `MatchResultConsumerLagging` 发现、人工处置。
- (2026-11-26 已收)data_service 的坏消息原来只跳过并提交,原字节只在原 topic 保留期内可取;现在先写死信再提交(§15.6)。原记:要保全原件需在两个 worker 的解码分支接死信写入端,未做。

## 13. Java 版

Java 版(`D:\luyuan\xuanming-server-mmo`)的 match、guild、data 消费者用 KafkaConsumer:提交失败只记日志,消费循环出异常时由监督逻辑重建消费者。
本文的机制两版各自实现,不共享代码。待做(登记在本仓 `docs/PROGRESS.md` 2026-11-12 条目,Java 仓 `PARITY.md` 按其口径登记):
历练结果消费组的 lag 告警、历练结果的有界重试后转死信(含 handler 异常)、保留期越过已提交位点时停取并告警、维护排空收尾按组内位点核交还记录。

## 14. 修订历史

- 2026-11-05:r2-kc#1 首版(写在 db-write-fencing-mysql.md 末尾):owner 跨代次存活、暂时失败原地重试、代次结束交还、HOLD 只剩终止与不变量破坏。
- 2026-11-06:评审修正,恢复票据按组内位点结清必须组确实提交过(R5 groupCommitted)。
- 2026-11-12:迁到本文;终止时交给新属主并收尾核组内位点(取代「终止时没结清即 HOLD」);恢复票据收尾核对;有界重试后转死信;
  保留期丢失检测;reader 模式交还后结束代次、提交停滞判据去掉 RequireArrivals;guild-trial 的 lag 探针与告警;match 删掉无 join 的旧循环。
- 2026-11-19:独立评审修正,交还与恢复票据只有在组于该分区已有已提交位点时才按组内位点结清(§6.2),收尾核对加 45 秒整体上限;
  负责人定从没提交过的分区接纳第一条之前先提交起点(§3.1)。
- 2026-11-26:收掉 11-19 评审的建议项。reader 模式连续因提交失败自己结束代次时退避(§8);owner 终止而代次还在时先短重试提交再交还(§6.1);
  死信 topic 与写入端的单条上限放到 2MiB(§5.3);死信写不进的告警改成连续失败才响;滚出窗口的代次结局不平计入 HOLD 指标;
  死信重放工具写到一半失败时报出死信 offset;打开分区时发现提交点落在日志起点之前也计入保留期丢失指标(§2、§9)。
  各项的问题、取舍与边界见 §15。

## 15. 设计记录:2026-11-26 收掉的评审建议项

每条写明问题、方案、为什么这样选、被否决的做法、代价与边界。

### 15.1 reader 模式连续因提交失败自己结束代次时退避

- **问题**:reader 模式批提交重试用尽后把这批交还并结束本代次,让下一代次立即重投(§8)。提交一直失败而入组正常时,
  每一轮都是「重投 → 重做同一批落库 → 提交失败 → 结束代次」,间隔只有提交自己的 0.6 秒;本成员是 leader 时每轮还触发一次全组再均衡。
  不丢数据,但持续打库、全组抖动。
- **方案**:owner 记「连续几个代次是自己因批提交失败结束的」。第一次不等;第二次起,下一代次打开分区之后、开始消费之前先等
  1s、2s…封顶 30s(带抖动);任何一次批提交成功清零。只有交还时代次还在(是本 owner 自己结束它)才计数。
- **为什么**:第一次不等,保住 11-12 那次改动的目的(安静分区上的积压立即重投),瞬时失败是常态,不该为它付等待。
  退避放在下一代次开头:调用方立即拿到 `*UncommittedError`、照旧摘在途集合;等待期间本成员仍在组里心跳,不消费也不占提交锁;
  排空或再均衡会打断等待。
- **被否决的做法**:
  - 在交还时先睡、再结束代次:持着提交锁睡,调用方被堵在提交里,排空要等它醒。
  - 把被再均衡结束的代次也计进去:滚动重启时每个成员都会连着几代被别人结束,平白多等。
  - 不结束代次、原地无限重试提交:回到 11-12 之前安静分区挂着积压的问题。
- **代价与边界**:提交持续失败期间同一批最多每 30 秒重做一次;提交停滞告警照常按分区计时。handler 模式不受影响
  (它的提交失败本来就原地退避)。

### 15.2 owner 终止而代次还在:只差提交的记录先短重试

- **问题**:排空时 owner 一终止就不再重试。回调已成功、只差提交的记录遇到一次瞬时提交失败就交还给新属主;最后一个离组的副本
  没有新属主,收尾核对读到的组内位点永远越不过它,45 秒后收据 HOLD,整次排空要重来。
- **方案**:终止之后提交仍按短重试(含第一次共 3 次,200ms 起翻倍),前提是代次还在;仍失败才交还。handler 模式在第一次提交失败、
  发现 owner 已终止且代次还在时进这段重试;reader 模式去掉「终止即停」。
- **为什么**:标准客户端关闭时也在关闭时限内重试同步提交。记录已经处理完,多试几次提交没有副作用。只在代次还在时试:
  逐个离组时旧代次的提交被拒是因为成员已被踢出,重试不会变好,照旧立即交还。
- **被否决的做法**:沿用运行期的长退避(1s 到 30s),终止之后不能无界地拖着排空;为它另设一个时间预算常量,已有的批提交短重试正是这个量级。
- **代价与边界**:协调者不可达时,终止最多多等三次提交各自的请求时限(组请求预算不超过 10 秒)加 0.6 秒,之后照旧交还、收尾核对。
  回调没成功的记录不重试回调(新属主可能已在处理同一条)。

### 15.3 死信的单条上限

- **问题**:死信 = 原件 + `dlq.*` 头(约 1.5KB)。死信 topic 沿用 broker 缺省上限(约 1MiB),写入端的单批上限缺省也是 1MiB:
  贴着上限的原件写死信必败,记录留在原处、分区停住,只剩「死信写不进」的告警。
- **方案**:死信 topic 的 `max.message.bytes` 与写入端的单批上限都取 2MiB;已有的死信 topic 在服务启动确保 topic 时被增量设置。
- **为什么是 2MiB**:本组消费者单次拉取上限是 1MiB,读得到的原件都装得下。不按原 topic 的上限推算:目前没有原 topic 设过上限,
  多一个参数没有调用方。
- **被否决的做法**:截断或拆开原件,重放要原样写回原分区,不能改原件;调 broker 级上限,会波及所有 topic。
- **边界**:生产者压缩过的批解开后超过 2MiB 仍写不进,告警照响,处置见手册 §1.6。

### 15.4 打开分区时发现的保留期丢失

- **问题**:保留期丢失有两个发现点。代次中途取到的记录前面有缺口,计丢失指标、critical;打开分区时提交点已落在日志起点之前,
  原来只记 HOLD、warning。§3.1 之后,接纳过没结清的记录被删正是在第二个点发现的,同样是数据已丢,却只有 warning。
- **方案**:打开时提交点早于日志起点,错误同时带「保留期丢失」的标记,owner 据此计同一个指标、打同一行日志。提交点晚于日志末端
  (topic 重建、截断)不带。
- **为什么**:一个事实一条告警、一个处置入口(手册 §2)。用错误上的标记区分,判据只在读水位的那一处,不在上层重算。
- **边界**:位点重置之前每次打开都再计一次,告警一直响。这是对的:消费者确实停着。

### 15.5 「死信写不进」告警的窗口

- **问题**:`increase[10m] > 0` 配 `for: 5m`。一次失败在 10 分钟窗口里一直算数,撑得过 5 分钟的 for,单次瞬时失败也响,
  与「持续失败」不符。
- **方案**:窗口改成 2 分钟,for 不变。重写死信的退避封顶 30 秒,持续失败时每个 2 分钟窗口里至少有几次。
- **可复用的规则**:用 increase 表达「持续」时,统计窗口必须短于 for。契约用例钉住这条规则的窗口与 for。

### 15.6 data_service 审计消费者的坏消息进死信

- **问题**:两个审计消费者(流水、快照)遇到解不出、主键为 0 或超过列宽的消息,只记日志与计数就提交越过。原件只在原 topic 的保留期内
  取得到,保留期一过,排查和补录都没有原件可用。
- **方案**:批处理自己写死信(`go/data_service/internal/kafka/dead_letter.go`)。进批时给坏消息留一份原件的深拷贝(reader 会复用缓冲),
  flush 的第一步把这一批的坏消息按顺序写进本组的死信「消费组名-dlq」(kafkautil 的死信格式,cause 记 unprocessable),写成功才落库、提交。
  写不进有界重试(5 次、1 秒),仍不行就整批不提交、消费者停下(`ErrConsumerStoppedOnDeadLetter`),与落库失败同样按「确定未提交」交还、
  冷却后续消费。
- **为什么死信写在最前面**:快照的批量模式失败时只提交失败行之前的前缀。死信先写完,不管之后提交的是整批还是哪一段前缀,
  前缀里的坏消息都已经在死信里,判据只有一条,不用逐段核对。
- **为什么由批处理写、不给 reader 模式的 owner 加死信**:reader 模式下 owner 只交付消息、由调用方按批提交,
  只有调用方知道哪条是坏消息、是哪种坏;让 owner 去判就得把业务解码搬进框架。
- **死信放得下快照**:快照 topic 的单条上限是 16MiB。`DeadLetterTopicSpecFor` 按原 topic 的上限加 64KiB 头部余量定死信的上限,
  写入端按同一份契约建(`NewKafkaDeadLetterSink(brokers, spec)`),两边不会对不上;重放工具读死信、写回原 topic 的单条上限一起放到 32MiB。
- **被否决的做法**:坏消息写进 data_service 自己的库表,要多一张表、一套迁移与写者闸门,而且坏消息常常出现在库正出故障的时候;
  只延长原 topic 的保留期,所有消息都多占盘,而要留的只是极少数坏消息。
- **代价与边界**:死信写不进时这一批的好消息也暂停落库,直到 Kafka 恢复(与库故障时一样宁可积压)。重放走 `cmd/kafka_dlq` 并要带
  `-allow-unprocessable`:原件本身处理不了,先修生产端或解码。停机重放时同一条坏消息可能写进死信两次,重放工具按原位置去重。

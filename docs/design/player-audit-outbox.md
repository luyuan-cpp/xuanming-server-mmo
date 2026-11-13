# 玩家审计发件箱:审计流水与快照随存盘提交(断线重连 R13)

> 状态:2026-11-11 定稿并落码,2026-11-12 补完(分笔存盘、删掉 C++ 直发 Kafka、scene 侧丢弃计数、资产变动通知挪到记流水的判定之后、
> 转发的分工互斥与轮次预算、TiDB 预切、K8s 渲染与暂停开关、告警、清库清单)。代码全部写完,**未生成协议、未编译、未测试**;
> 验证步骤见 `docs/PROGRESS.md` 2026-11-11、2026-11-12 标题带「审计发件箱」的各条。改动清单见 §11。
> 上游:[断线重连核对](infra-reconnect-overview.md) §4.1 R13(方案 2026-11-10 定);[存盘直接提交](player-save-direct-commit.md)(提交点、写权、save_seq 幂等);
> [逻辑库自治](horizontal-scale-logical-db.md)(每库一份闸门、不跨库);[审计链路水平扩展](audit-pipeline-horizontal-scale-20261106.md)(消费端去重)。

## 0. 结论

1. **审计记录随存盘提交。** scene 记下的审计记录(流水 `TransactionLogEntry`、快照请求)按玩家排队,随这名玩家的下一笔存盘交给 go/db;
   go/db 在提交这笔存档的同一个 TiDB 事务里把它们写进**这名玩家存档库**的发件箱表 `player_audit_outbox`。
   于是「发件箱里有这条」等于「它描述的改动已经提交」,两边要么一起在库里,要么一起不在。
2. **转发循环至少投一次。** go/db 按片分工(`shared/workshard`),把发件箱投到原来的两个审计 topic(`<基名>_g<世代>`,key = player_id),
   broker 全副本确认(acks=all)之后按主键删除。重投由 data_service 的消费者照旧吸收:流水按 `tx_id` 主键、快照按 `snapshot_guid` 去重。
3. **快照由 go/db 在提交事务里拼。** scene 只交一份快照请求(号、触发、zone),go/db 按这一笔写完之后的全部数据行拼出整档。
   快照因此就是库里某一笔提交的原样,存盘请求也不会因为带整档而变大。
4. **不跨库。** 发件箱建在每个存档逻辑库里,只有 go/db 读写,搬库时跟着库走(AGENTS §7 第 6 条)。
5. **审计不挡存盘。** go/db 不会因为审计内容拒绝一笔存盘;个别坏记录跳过并计数。

不选的两种(2026-11-10 定):本地落盘队列(K8s 上 Pod 一重建就丢);把 `message.timeout.ms` 调成无限(只撑得住进程活着时的 Kafka 中断)。

## 1. 问题

改之前(2026-11-05 起)的流水已经「跟着存盘提交走」:改动发生时排进玩家队列,存盘回 COMMITTED / EQUAL 之后由 `TransactionLogSystem::PublishThrough`
直接发 Kafka。剩下的漏洞都在「提交之后、Kafka 确认之前」:

| 场景 | 后果 |
|---|---|
| Kafka 不可用超过 `message.timeout.ms`(300 秒),或发送当场被拒(队列满、fatal) | 已提交的改动没有流水,只剩一行 ERROR |
| 存盘提交了、回包丢了、进程随后崩溃 | 同上,流水没发出去 |
| 快照当场直接发 Kafka,不等提交 | 发送失败就丢;发出去的快照可能是从来没提交过的内存状态 |

流水是 GM 回收按玩家汇总应扣金额的依据,也是审计与排查的记录(人工终结工具 2026-11-05 起改以已提交账本为证据,
[数据一致性保障地图](data-consistency-guarantee-map-20261020.md) §6.3 第 4 条);快照是回档的依据。负责人的标准是不丢。

## 2. 数据流

```
scene  改动 ──► AuditOutbox 队列(玩家, 水位 m)
       存盘取快照(记下水位 M) ──► 组请求:拷进水位 ≤ M 的审计记录 ──► go/db SavePlayer
go/db  一个事务:锁写权行 → 判定 → 写脏行 → 写发件箱(流水原样;快照按提交后的行拼) → COMMIT
       转发循环:按 (存档库, 片) 读发件箱 → 投 Kafka(acks=all) → 按主键删
data_service  消费 → 去重 → <基名>_ledger 的 transaction_log / player_snapshot
```

## 3. 发件箱表 `player_audit_outbox`

声明在 `proto/common/database/mysql_database_table.proto`,随 `mysql_database_table_list.json` 由 go/db 的 `cmd/migrate` 基线建在每个存档库里。

| 列 | 类型 | 含义 |
|---|---|---|
| `shard` | INT UNSIGNED | `player_id % 16`,转发按片分工、按片扫描 |
| `kind` | INT UNSIGNED | `playerstore.PlayerAuditKind`:1 流水、2 快照 |
| `created_ms` | BIGINT UNSIGNED | 写入它的 go/db 在提交事务里取的墙钟毫秒 |
| `player_id` | BIGINT UNSIGNED | Kafka key |
| `record_id` | BIGINT UNSIGNED | `tx_id` 或 `snapshot_id` |
| `save_seq` | BIGINT UNSIGNED | 带它提交的那一笔存盘的序号,只供排查 |
| `payload` | MEDIUMBLOB | Kafka 消息体原样:序列化的 `TransactionLogEntry` / `PlayerSnapshotEntry` |

主键 `(shard, kind, created_ms, player_id, record_id)`,聚簇。为什么这样排:

- **片内按时间排。** 发件箱是插入即删的队列。删掉的行在 TiDB 里留 MVCC 墓碑,直到 GC(缺省保留 10 分钟)。按 player_id 排的话,
  每轮扫描都要从片头跨过这一片近十几分钟的全部墓碑;按时间排,扫描从「上一轮最老的未投记录」附近开始,墓碑留在扫描起点之前(§7.2)。
- **16 片。** 每个存档库 16 段追加尾巴、16 个可分工的单位,K=64 个库时全地域 1024 个单位,够几十个 go/db 副本分。
  片数只决定分工和写入分散,改它要清表(项目未上线可以直接改)。
- **流水和快照分开排。** 快照一行可到几百 KB,流水一行一两百字节,转发按 kind 各读各的批(流水 500 行、快照 8 行)。
- 同一条记录被交了两次(§8)会是两行,都投出去,由消费端去重;同一笔请求里重复的记录撞主键时 `ON DUPLICATE KEY` 不报错。
- **TiDB 预切**(`go/db/cmd/migrate/presplit.go`,每次 `-command up` 之后发,幂等;MySQL 上不发):
  ``SPLIT TABLE `player_audit_outbox` BY (1, 0, 0, 0, 0), (2, 0, 0, 0, 0), …, (15, 0, 0, 0, 0)``。
  聚簇的复合主键按值切时,TiDB 要求给出主键每一列的值,只写片号 `(1)` 会被拒;后几列取 0,就是这一片的起点。
  切点就是片号边界,一片一个 Region:16 段按时间追加的尾巴从建表起就落在不同 Region、能分到不同 TiKV,不用等自动分裂;
  之后单片变大由 TiDB 按 Region 大小继续自动分裂。同一步也把 `player_store` 按 player_id 的 63 位取值空间等分成 16 段
  (边界 `(i × 2^59, 0)`)。段数 16 定案(2026-11-12),不等压测:预切只解决开服起步的单 Region 热点,之后的分布由自动分裂接管。
  发之前核 `TIDB_PK_TYPE = CLUSTERED`;切不成按 `NEEDS-REVIEW` 报、退出码 4,迁移本身照样算成功(只影响写入分散)。片数改了切点跟着改。

**大小**:流水一条一两百字节,单条上限 64 KiB(scene 的 `AuditOutbox::kMaxRecordBytes` 与 go/db 的 `playerstore.MaxAuditTransactionLogBytes` 同值,
流水 topic 沿用 broker 缺省约 1 MiB 的 `max.message.bytes`,装得下);快照一份上限 4 MiB(`playerstore.MaxAuditSnapshotBytes`),低于 TiDB 单 KV 6 MiB、
Kafka 快照 topic 的 `max.message.bytes` 16 MiB。超了不写发件箱、计数并告警(§6.3),存盘照常提交。

## 4. 协议(`proto/player_store/player_store.proto`)

```proto
enum PlayerAuditKind {
  PLAYER_AUDIT_KIND_UNSPECIFIED = 0;
  PLAYER_AUDIT_KIND_TRANSACTION_LOG = 1;
  PLAYER_AUDIT_KIND_PLAYER_SNAPSHOT = 2;
}
message SavePlayerRequest {
  …                                                   // 1~9 不变
  repeated TransactionLogEntry transaction_logs = 10; // 随这一笔提交的流水,原样进发件箱
  repeated PlayerSnapshotEntry audit_snapshots = 11;  // 快照请求:只用 snapshot_id、trigger、schema_version、zone_id
}
message SavePlayerResponse {
  …                                                   // 1~3 不变
  bool audit_outbox_written = 4;                      // 认识审计字段的 go/db 在 COMMITTED / EQUAL 时置真
}
```

- 两种审计消息沿用已有的 `TransactionLogEntry`、`PlayerSnapshotEntry`,不另造平行结构。快照请求里的 `player_database_blob`、
  `snapshot_time`、`total_bytes`、`player_id` 由 go/db 填,请求里带了也会被覆盖。
- `audit_outbox_written` 只为新旧混跑兜底:旧 go/db 不认识 10、11 字段,照样提交存档、静默丢掉审计。scene 看到带了审计却没置真,
  就不放行这些记录,留在队列里随下一笔再交,打 ERROR 并计数(§5.2、§5.6)。

## 5. scene 侧

### 5.1 队列 `AuditOutbox`(`cpp/libs/modules/audit/audit_outbox.{h,cpp}`)

- 线程局部,只在 scene 逻辑线程上用,与原来的流水闸门同一个线程模型。按玩家一条队列,每条记录带进程内单调的水位,
  全进程一条数轴:实体销毁重建之后水位也不会回头,旧快照的水位盖不到新记录。
- **开关**:scene 装配存盘状态机时 `Enable(true)`。没开(battle、单测、工具)时流水与快照请求一律拒收,打 ERROR 并按 `outbox_disabled` 计数(§5.6):
  这个进程没有存盘,审计记录没有提交的地方。C++ 不直接发审计 topic,没有第二条出路(§5.4)。
- **入队**:`EnqueueTransactionLog`、`EnqueueSnapshot`。单条超过 64 KiB(`kMaxRecordBytes`)拒收,打 ERROR 并按 `too_large` 计数(调用方的 bug)。
- **一笔的预算与分笔(2026-11-12)**:一笔存盘最多带 512 KiB 审计记录(`kMaxBytesPerSave`,按序列化大小加每条 4 字节的字段头计,
  与 go/db 的 `MaxAuditBytesPerSave` 同值)。行预算 3 MiB(go/db `MaxRequestBytes`)+ 审计 512 KiB + 余量 64 KiB 小于传输上限 4 MiB − 64 KiB,
  所以审计永远不会让一笔存盘超限被拒,go/db 启动时核这条不等式(§6.4)。到期记录超过一笔的预算时不丢:`AttachDue` 只带到预算为止并报 `more`,
  存盘状态机在这一笔提交之后紧接着发续笔,把剩下的带走(§5.2)。单条上限不超过一笔的预算(`static_assert`),所以每一笔至少带走一条,分笔一定交得完。
  11-11 的做法是每人只排 512 KiB、超了丢最旧的,一笔总能带完;代价是存盘中断稍久、记录稍多就开始丢已提交改动的流水,所以改成分笔。
- **内存上限**:每名玩家最多 65536 条、8 MiB(`kMaxRecordsPerPlayer` / `kMaxBytesPerPlayer`),只为封住内存。正常情况下流水一入队就请求约 1 秒内存盘(§5.4),
  队列里只有一两秒的量;到了上限说明这名玩家的存盘中断了很久(go/db 或 TiDB 长时间不可用)而他还在不停地产生记录,这时丢最旧的一条,
  打 ERROR(`metric=audit_outbox_overflow`,第一条和此后每 1000 条打一次)并按 `overflow` 计数。
- **交出**:`HasDue(玩家, M)`、`AttachDue(玩家, M, 两个输出)` —— 水位 ≤ M 的记录按记录顺序拷进请求,合计不超过一笔的预算,
  返回带到哪一条(`through`)、还有没有(`more`);队列里不删。
- **放行**:`AckThrough(玩家, M)` 删掉水位 ≤ M 的记录。只在这一笔 COMMITTED / EQUAL 且 `audit_outbox_written` 为真时调用:带完时 M 是那份快照的水位,
  没带完时是 `through`。或者快照与已确认内容一样、没有到期记录而不用写的时候调用(这时删不掉任何东西)。
- **丢弃**:`Discard(玩家, 原因)`,三个时机照旧:实体销毁且存盘落定、实体已没而存盘落定(被废黜)、本节点重新认领之前。
  丢的是没落库的改动的记录,`metric=audit_outbox_discarded_uncommitted`。

### 5.2 存盘状态机(`player_store_client.{h,cpp}`)

`Options` 新增两个可选钩子:`auditDue(玩家, M)`、`attachAudit(玩家, M, 请求)`(返回 `AuditAttachment{through, more}`)。

- `Start` 组请求时调 `attachAudit`。重试发的是同一份请求,审计记录和行一样同内容、同序号。
- 有到期记录就算没有脏行也写一笔(和 `force` 同一档):净变化为零的改动(先 +100 后 −100)同样有两条流水要交。
- `DeferPeriodicSave` 有到期记录时不推迟。
- **分笔**:`attachAudit` 报 `more` 时,这一笔不带 release(release 之后别的节点可以认领,剩下的记录就没有机会由本节点提交了),
  放行水位记成 `through`,并记下一份续笔(调用方原来的 release 与回调,`force` 清掉:行已经随这一笔写了)。
  这一笔 COMMITTED / EQUAL 之后续笔才排进待发:行就是刚确认的内容,带着原来的 release 与回调紧接着发,`attachAudit` 从 `through` 之后接着带;
  续笔再报 `more` 就再续,直到带完。对调用方来说这次存盘到最后一笔提交才算完成,最后一笔被拒,调用方看到的就是被拒。
  这一笔被拒或结果未知时不排续笔,回调照常报这一笔的结局,记录留在队列随下一笔再交(续笔只在提交之后排,确定性拒绝因此一定回到调用方)。
  排队位置上已经有更新的快照时不另排续笔:那一份的水位更大,会带走剩下的记录,release 也以它为准(release 本来就是「最后一份说了算」),这一笔照常回调。
- **go/db 不写发件箱时不空转**:这一笔带了审计、回 COMMITTED / EQUAL 而 `audit_outbox_written` 为假(旧 go/db)时,不调 `onCommittedThrough`,
  记录留在队列,打 ERROR(`metric=audit_outbox_store_too_old`,第一次和此后每 1000 次)并计数(§5.6);行照常确认。同时置 `auditStalled`:
  之后的笔即使报 `more` 也不分笔,免得对着一个永远不写发件箱的 go/db 一笔接一笔地续;哪一笔写了发件箱就清掉。
- 被拒、结果未知、被更新的快照取代都不放行,记录留在队列,随下一笔再交。

### 5.3 每种结局下审计记录的去向

| 这一笔的结局 | 存档 | 审计记录 |
|---|---|---|
| COMMITTED | 写入 | 同一事务写进发件箱,scene 放行 |
| EQUAL(同一请求之前已提交,回包丢了) | 之前已写入 | 之前同一事务已写入,scene 放行 |
| 结果未知 | 不确定 | 同内容重试,结局跟着存档走 |
| 写权拒绝(被接管) | 本次没写 | 留在队列;实体被废黜时随改动一起丢。若之前某次结果未知的尝试其实已提交,记录已在发件箱 |
| INVALID / TOO_LARGE / 世代拒绝 / OLDER_SEQ | 本次没写 | 留在队列,随下一笔再交 |
| 实体销毁、存盘落定、本节点重新认领之前 | — | `Discard`:改动没落库,记录随之作废 |

### 5.4 流水(`transaction_log_system.{h,cpp}`)

- `RecordEntry`:流水只有一条出路 —— 盖 zone 章、进这名玩家的审计队列(`AuditOutbox::EnqueueTransactionLog`),随他的存盘提交。
  拒收的情形各自打 ERROR、按原因计数(§5.6):`tx_id` 无效(txlog 号段没号,`no_id`)、不是恰好一名玩家(`from_player`、`to_player` 两边都填或都不填,
  `no_single_owner`)、进程没开发件箱(`outbox_disabled`)、单条超限(`too_large`,由 `AuditOutbox` 判)。
- **C++ 不直接发审计 topic(2026-11-12)**:原来「没开发件箱的进程、没有单一玩家的流水照旧直接发 Kafka」的旁路删掉 —— 那条路 Kafka 不可用时会丢,
  而且发出去的流水与存档不在一个事务里,正是 R13 要消掉的形态。随之删掉的:没有调用方的 `LogItemTransfer`(两名玩家之间的转移记成转出、转入两条,
  同一个 correlation_id,各随各的存盘);`TransactionLogSystem` 的 Kafka 发送与测试注入口;`cpp/libs/modules/audit/audit_topic.h`;
  C++ 配置键 `AuditTopicGeneration`(`config.proto` 字段 20 `reserved`,node ConfigMap 与 `bin/etc/base_deploy_config.yaml` 不再写它)。
  topic 世代号只剩 go/db 的 `AuditOutbox.TopicGeneration` 与 data_service 的 `Kafka.TopicGeneration` 两处,同源(§7.6、部署见 `deploy/k8s/README.md`)。
- **资产变动通知(2026-11-12)**:`TransactionLogSystem::SetAssetChangedHook`。每条流水只要指名了恰好一名玩家,判定完(收下或被拒)就按这名玩家调一次;
  scene 在装配存盘状态机时接上(`player_lifecycle.cpp` 的 `MakePlayerStoreOptions`),用它请求关键进度存盘(1 秒内合并),
  于是货币、物品的变动和描述它的流水约 1 秒内一起提交,不用等定时存盘([存盘规格 §5.3](./player-save-direct-commit.md))。
  通知跟着「资产变了」走,不跟着「流水收下了」走:流水被拒(号段断供、单条超限)时货币、物品照样已经在内存里变了,同样要尽快存盘,
  否则崩溃时多丢的正好是资产。最初放在 `AuditOutbox::SetTransactionLogQueuedHook`(流水入队成功之后),同日挪到这里。
  没有单一玩家的流水不通知(不知道该请谁存盘);快照请求不走这里,要不要尽快提交由请求方决定(§5.5)。钩子里只许登记稍后存盘:调用点在货币、背包的改动函数中间。
- 原来的闸门接口 `EnableCommitGating`、`CommitMark`、`PublishThrough`、`DiscardPending`、`PendingCount` 删掉,由 `AuditOutbox` 取代。

### 5.5 快照(`snapshot_system.{h,cpp}`)

- `CaptureAndSend` 改名 `RequestAuditSnapshot`:核实体与 Guid → 抽样(`audit_snapshot_policy`,不变)→ 发件箱没开就拒绝(打 ERROR、按 `outbox_disabled` 计数,
  这个进程没有存盘,快照没有提交的地方)→ 领 snapshot_id(号段没号照旧跳过,按 `no_id` 计数)→ 入队快照请求。不再在 scene 里序列化整档,
  也不再在这里核持久化组件:缺组件的玩家存盘会被拒(`MarshalPlayerForSave`),快照请求随实体一起作废,不会落成一份零值快照。
- 进场(`SNAPSHOT_LOGIN`):入队之后调 `RequestKeyProgressSave`,让快照在约 1 秒内随一笔存盘提交,内容是进场初始化之后的状态。
- 下线(`SNAPSHOT_LOGOUT`):随后就是最终存盘(带 release),快照内容就是最终提交的状态。

### 5.6 scene 侧丢弃计数与告警(2026-11-12)

审计不挡游戏:scene 一侧下面几种情形只丢这一条审计记录,它描述的改动照常生效、照常随存盘提交,所以每一次都是「已提交的改动少了一条流水 / 快照」。
原来只有日志,按 AGENTS §11.3(fail-open 的路径要配日志、指标和测试)补上指标与告警。计数在 `cpp/libs/modules/audit/audit_outbox_stats.h`
(进程级 relaxed atomic,与 `currency_clawback_stats` 同一套写法),由 scene 的 Prometheus 出口在 `player_save_metrics.cpp` 导出,启动即以 0 导出:

| 指标 | label | 计在哪里 |
|---|---|---|
| `scene_audit_outbox_records_dropped_total` | `reason` = `overflow` | 一名玩家排着的记录到了内存上限,丢最旧的一条(`AuditOutbox`,`metric=audit_outbox_overflow`) |
| | `too_large` | 单条超过 64 KiB,拒收(`AuditOutbox`,`metric=audit_outbox_record_too_large`) |
| | `no_single_owner` | 流水没有恰好一名玩家(`TransactionLogSystem::RecordEntry`,`metric=txlog_refused_no_single_owner`) |
| | `outbox_disabled` | 进程没开发件箱时记流水或请求快照(`metric=txlog_refused_outbox_disabled` / `snapshot_refused_outbox_disabled`) |
| | `no_id` | txlog / snapshot 号段没号(`metric=txlog_refused_no_id` / `snapshot_refused_no_id`) |
| `scene_audit_outbox_store_too_old_total` | — | 带审计的存盘提交了、go/db 没写发件箱(存盘状态机,`metric=audit_outbox_store_too_old`)。记录没丢,留在队列;不升级 go/db,队列到上限之后才按 `overflow` 丢 |

`reason` 的取值是枚举 `audit_outbox_stats::DropReason` 加一张平铺的名字表(`kDropReasonNames`,`static_assert` 钉住条数,AGENTS §11.2)。
玩家号只在同一时刻那一行 ERROR 里,不进 label(AGENTS §9)。`audit_outbox_discarded_uncommitted`(改动没落库,记录随之作废,两边一致)不是丢失,只在日志里。

告警在 `deploy/k8s/audit-outbox-alerts.yaml` 的 `scene.audit-outbox` 组:`SceneAuditRecordsDropped`(`increase(...[10m]) > 0`,按 namespace 与原因,warning,出现就报)、
`SceneAuditStoreTooOld`(warning,出现就报)。指标名、每种原因都有计数点、采集回调的登记由 `tools/scripts/tests/audit_outbox_alerts_contract.tests.ps1` 对照源码核对。

## 6. go/db 写入(`go/db/internal/playerstore`)

### 6.1 事务

```
进事务前校验(不变)
BEGIN;SELECT 写权行 FOR UPDATE;判定(不变)
COMMITTED 时:INSERT player_store …(不变)
  有审计记录时:
    有快照请求 → SELECT kind,payload FROM player_store WHERE player_id=? AND kind>0(看得到本事务刚写的行)→ 拼快照
    INSERT INTO player_audit_outbox (…) VALUES … ON DUPLICATE KEY UPDATE record_id=record_id(每条语句至多 1000 行)
COMMIT
```

- 只有 COMMITTED 才写发件箱。EQUAL 不写:同序号意味着同一份请求之前已经提交,那次提交在同一个事务里写过发件箱。
- 发件箱写失败按存档写失败处理:整笔回滚,返回错误(结果未知),scene 同内容同序号重试。
- `audit_outbox_written`:COMMITTED、EQUAL 一律置真。EQUAL 时 go/db 判断不了之前那次提交是不是旧 go/db 做的,按同版本部署处理(§12)。

### 6.2 快照怎么拼

把这名玩家全部数据行的 payload 按 kind 升序拼接,`proto.Unmarshal` 进 `player_database`(protobuf 拼接即合并,和 scene 加载是同一个做法),
再填 `player_id` 和 `persist_schema_gen`(取这一笔请求的 `schema_gen`),确定性序列化成 `player_database_blob`。
`PlayerSnapshotEntry` 的 `snapshot_time` 取 go/db 在事务里的当前秒(内容就是这一刻提交的状态),`total_bytes` 照 scene 原来的做法两遍序列化。
不认识的未知字段原样保留。

### 6.3 坏记录不挡存盘

审计不能让玩家存不了盘。下列情况只跳过那一条,存档照常提交,计数 `db_audit_outbox_records_skipped_total{reason}` 并打 ERROR:

| reason | 情形 |
|---|---|
| `invalid` | 流水 `tx_id` 为 0,或快照请求 `snapshot_id` 为 0(scene 不会发,消费端也会跳过) |
| `unknown_row_kind` | 这名玩家有本 go/db 不认识的行(更新的 scene 写的,或退役号残留):拼出来会缺字段,宁可少一份快照 |
| `too_large` | 拼出来的快照超过 4 MiB,或一条流水超过 64 KiB(scene 入队时已按同一上限拒收,这里再核一次:进了发件箱的行必须投得出去) |
| `undecodable` | 行拼接后解不开(行被写坏) |

### 6.4 预算核对

`playerstore.New` 拒绝 `MaxRequestBytes + MaxAuditBytesPerSave(512 KiB) + 64 KiB > 4 MiB − 64 KiB` 的配置:行预算调大到装不下审计时,
scene 会在本地按 TOO_LARGE 拒掉整笔存盘。`MaxAuditBytesPerSave` 与 C++ `AuditOutbox::kMaxBytesPerSave` 同值、`MaxAuditTransactionLogBytes` 与
`AuditOutbox::kMaxRecordBytes` 同值,Go 单测读头文件核对。

## 7. 转发循环(`go/db/internal/auditoutbox`)

代码:`relay.go`(循环、游标、分工、退避)、`sql.go`(探测、命名锁、读、删)、`kafka.go`(投递);装配在 `go/db/audit_outbox_wiring.go`,
`go/db/db.go` 在 etcd 注册之后起、停机时先于注销停。目标:**多副本水平扩展,同一行不会被两个副本同时投,确认之前不删,所以不漏**。

### 7.1 分工与互斥

- **工作单位** `(存档库, 片)`。存档库清单 = 库名白名单(`MMORPG_DB_ALLOWED_DATABASES`,部署保证恰好是本地域的 K 个库,
  `proto_sql.StoreRegistry.RelayStorageIDs`)。白名单为空只出现在本机 dev 的宽松档,这时退到本进程打开过的库。
- **分工**:归属键 `audit-outbox:<storage_id>:<片>`,用 `shared/workshard` 的最高随机权重哈希在全地域的 stable go/db 之间分
  (成员表 = etcd 前缀 `PlayerStoreNodeService.rpc/zone/` 下全部 zone 的注册,canary 由 `StableMembers` 排除)。哪个 go/db 都能写哪个存档库,
  所以可以跨 zone 分;副本增减时只有落在增减副本上的单位换主。只在本副本负责某个库至少一片时才打开那个库,不负责的库不占连接池。
- **互斥**:分工只省重复劳动,成员表变化的几秒里两个副本可能都认为自己负责同一片。所以每个单位处理时持一把会话级命名锁:
  1. 先不加锁探测:每种记录只读最老一行的 `created_ms`(`LIMIT 1`,不读 payload)。绝大多数单位绝大多数时候是空的,空就不取锁。
  2. 有行才在钉住的一条连接上 `GET_LOCK('db_audit_outbox.<两位片号>.<库名>', 0)`;拿不到 = 别的实例正在投,这一轮让给它。
     MySQL 上 0 是不等待;TiDB 的超时下限是 1 秒(0 会被抬到 1),撞锁时最多等 1 秒,只在分工切换时出现。锁在整个 TiDB 集群上生效。
  3. 读、投、删都在持锁的这条连接上做,做完 `RELEASE_LOCK`。放锁失败、回 0 或 NULL,取锁出错或回 NULL,一律丢掉这条物理连接
     (`Conn.Raw` 回 `driver.ErrBadConn`):会话一断服务端就放锁;还回池里的话,一条可能仍持锁的会话会被存盘反复借用,锁永远放不掉。
- **前提**:同一地域所有 go/db 的白名单相同(部署只有一份清单),并且注册在同一套 etcd 上。不在同一套时两边各自认为负责同一片,
  靠命名锁互斥,只是撞锁多;分工覆盖告警(§9)会显示合计负责的单位数不对。

### 7.2 扫描

每个 (库, 片, kind) 一个游标 `scanFrom`(毫秒),只在本副本负责这个单位期间保留:

```sql
SELECT created_ms, player_id, record_id, payload FROM player_audit_outbox
 WHERE shard=? AND kind=? AND created_ms>=? ORDER BY created_ms, player_id, record_id LIMIT ?
```

- 投完一批:`scanFrom = max(scanFrom, min(本批最后一行的 created_ms, 现在 − 2 分钟))`;读到空:`scanFrom = max(scanFrom, 现在 − 2 分钟)`。
  2 分钟的余量盖住「created_ms 取值到提交可见」的延迟(存盘事务受 30 秒读超时约束)和 go/db 之间的时钟偏差。
  等于游标的那些行下一次照样读:同一笔存盘的记录 `created_ms` 相同,已删的读不到。
- 有失败的批:`scanFrom` 停在第一条没投成的行上,它和它之后的行下一轮再投。
- 每 10 分钟对每个单位从 0 全扫一次,接住时钟跳变之类余量盖不住的情况。刚接手的单位(包括分工换主之后)也从 0 开始:
  那时它可能被别的副本推进过,也可能还有别的副本留下的行。
- 墓碑代价:每轮只跨过「现在 − 2 分钟」之后的墓碑,外加每 10 分钟一次全扫。
- 批量:流水 500 行、快照 8 行;一个单位一轮最多 20 批。

### 7.3 投递与删除

- 每个 topic 一个同步 kafka-go `Writer`:`RequiredAcks=RequireAll`、`CRC32Balancer`(与 librdkafka `consistent_random` 同一个分区算法,
  key = 玩家号十进制串,同一玩家落同一分区,AGENTS §7 第 3 条)、lz4、不自动建 topic(topic 由 data_service 的 `EnsureTopics` 与
  kafka-topic-init 按分区契约预建,裸名字被自动建成 1 分区会让消费端的分区契约永久失配)。
- 批上限:流水 topic 用 broker 缺省的约 1 MiB(`BatchBytes` 1 MiB、500 条),快照 topic 用它的 `max.message.bytes` 16 MiB(8 条)。
  超过「批上限 − 1 KiB」的行在本地判失败、不交给 kafka-go:否则它以 `MessageTooLargeError` 拒掉整次调用,一条坏行拖住整批。
  正常写入产生不了这种行(流水单条 64 KiB、快照 4 MiB 的上限在写入端,§6.3);真有了就留在发件箱,积压年龄告警会响。
- 确认之后才删,只按投成功的那些行的整主键删:`DELETE … WHERE shard=? AND kind=? AND (created_ms,player_id,record_id) IN (…)`。
  部分失败时只删成功的;删不存在的行不算错(重投之后另一个实例已经删过)。删除走存档库的写者闸门。
- topic 名:`kafkautil.AuditTopicName(基名, 世代)`,data_service 的消费端与它共用同一个函数。go/db 的 `AuditOutbox.TopicGeneration` 与两个基名必须和
  data_service 的 `Kafka` 段同值:本机 yaml 由 Go 单测核对,K8s 的 db ConfigMap 由部署脚本从 data_service 那份渲染、契约测试核对。

### 7.4 一轮的时间上限、失败与退避

- **一轮至多 30 秒**(`passBudget`):到点就不再开始新的单位,一个单位里也不再开始新的一批,剩下的下一轮从断点接着做、不等下一拍。
  断点记在本副本负责的单位清单上,清单靠后的单位不会因为前面的单位慢而一直轮不到。
- **超时**:每条 SQL 5 秒(`statementTimeout`);投一批 5 秒(`produceTimeout`,含 kafka-go 内部至多 3 次重试)。投的时候钉着存档库池的一条连接,
  K 个库时每库的池可能只有 2 条,所以投递超时取短:Kafka 不可用时每次只占这条连接 5 秒,随后整轮退避。健康时 acks=all 一批是几十毫秒。
- **退避**:一批一条都没投出去(多半是 Kafka 不可用或 topic 不存在)才整轮停下,1 秒起翻倍、封顶 30 秒、乘 0.5~1 的随机系数(几个副本不在同一刻一起重试)。
  部分失败(个别行被拒)不退避:别的行照常投,被拒的那行留在发件箱。读、删失败只跳过那个库这一轮,其余库照常。
  一轮 panic 被 `safego` 兜住(`safego_panic_total{point="db.audit_outbox_relay"}`),按投递失败退避,循环不死。
- **日志**:同一阶段(`read` / `produce` / `delete`)连续失败的第一次和此后每 30 次打 ERROR,恢复时打一行 INFO。日志不带玩家号以外的玩家数据,不进指标 label。

### 7.5 写者闸门、维护排空与停机

- **写者闸门冻结**(搬库切换窗口):读、删回 `authoritysql.ErrWriterFrozen`(`IsWriterFrozen`),不算错误,只记 INFO,这个库暂停 5 秒。
  这个 boot 还没有写事务过过闸门时,事务外的读在冻结时照样放行(闸门的启动阶段规则),所以窗口里可能把同一批投出去、删不掉,
  每 5 秒至多一批,由消费端去重。搬库时发件箱的行随库一起复制,解冻之后在新位置接着投。
- **维护排空**:`db.go` 建一个 RELAY 会话(`auditRelayAdmission`),每个单位处理前 `Enter`、做完退出。`internal/drain` 先关 SOURCE(RPC 入口)、等在途 RPC,
  再关 RELAY、等在途的单位做完,之后才封写资格、关连接池。关了之后循环不再开始新的单位;投出去还没删的行留给接手的 boot 重投。排空结局不明(HOLD)时 RELAY 也一起关。
  单位处理到一半 panic 时 `Hold`:说不清写到哪一步,排空不能把这个 boot 当成干净收尾(与 assetop 相同)。
- **停机**:停机时先停转发循环(等至多 20 秒,一批在途至多 5 秒投递加几条 SQL),再关 Kafka 写端,之后才注销 etcd、关连接池。
- **不起的情形**:canary 副本(发布轨道,不跑全局后台);没配 `AuditOutbox.Brokers`(本机没有 Kafka、或部署时 `-AuditOutboxRelay paused` 暂停转发,
  审计 topic 换代手册 §2.2)。都打日志、`db_audit_outbox_relay_enabled` 报 0;发件箱照常随存盘写入,只积不投、不丢,告警会响(§9)。

### 7.6 配置(go/db 顶层 `AuditOutbox`)

| 键 | 缺省 | 说明 |
|---|---|---|
| `Brokers` | 空 | 空 = 不起转发。本机 `etc/db.yaml` 写 `127.0.0.1:9092`;K8s 由部署脚本渲染集群里的 Kafka 地址 |
| `TopicGeneration` | 1(写 0 也按 1) | 与 data_service `Kafka.TopicGeneration` 同值 |
| `TransactionLogTopic` / `SnapshotTopic` | `transaction_log_topic` / `player_snapshot_topic` | 基名,有效名带 `_g<世代>`;两者不能同名 |
| `IntervalMs` | 1000 | 空闲时两轮之间的间隔,`[100, 30000]`,越界拒启。上限与退避上限相同,活性告警的阈值按它算(§9) |

`Config.Normalize` 回填缺省,`Config.Validate` 在回填之前也按回填之后的值核(`conf.Load` 在 Normalize 之前就调它)。

## 8. 幂等与重复

至少一次,重复来自:

1. 投出去之后删除失败、或进程崩溃 → 下一轮重投。
2. 成员表变化时两个副本同投一片。
3. 结果未知后的写权拒绝、OLDER_SEQ:之前的尝试可能已经提交,记录留在 scene 队列,随下一笔再交一次。

消费端:流水 `tx_id` 主键 + `INSERT IGNORE`;快照按 `snapshot_guid` 去重,保留先到的那份。所以重复只多一次落库尝试,不会多一条记录。

## 9. 指标与告警

指标都在 `go/db/internal/metrics/audit_outbox.go`,label 都是封闭集合,不含玩家号和存档库编号(AGENTS §9)。

| 指标(go/db) | 含义 |
|---|---|
| `db_audit_outbox_records_written_total{kind}` | 随一笔已提交的存盘写进发件箱的条数(提交之后才计,结果未知的重试不重复计) |
| `db_audit_outbox_records_skipped_total{reason}` | §6.3 跳过的条数,`reason` = `invalid` / `unknown_row_kind` / `too_large` / `undecodable`,启动时以 0 预置 |
| `db_audit_outbox_forwarded_total{kind}` | 投出(acks=all)并删除的条数 |
| `db_audit_outbox_relay_errors_total{stage}` | `read` / `produce` / `delete` 失败次数(闸门冻结不计) |
| `db_audit_outbox_oldest_pending_age_seconds{kind}` | 本副本负责的单位里,最近一轮探测到的最老未投记录的年龄;没有积压为 0。各副本只报自己那部分,规则取 max |
| `db_audit_outbox_relay_enabled` | 本副本转发循环是否在跑(canary、没配 Kafka 为 0) |
| `db_audit_outbox_relay_owned_units` / `db_audit_outbox_relay_units` | 本副本最近一轮负责的单位数 / 它知道的单位总数(存档库数 × 16),每轮写 |
| `db_audit_outbox_relay_last_run_unixtime` | 转发循环最近一次跑完一轮的时刻(`shared/looplive`):起来时写一次,每轮正常返回才写,panic 的那一轮不写,停机删序列。没起循环的副本没有序列 |

scene 侧两个进程级计数,由 scene 的 Prometheus 出口导出(口径见 §5.6):
- `scene_audit_outbox_records_dropped_total{reason}`:一条审计记录没能进队列、或进了又被挤掉,`reason` 取 `audit_outbox_stats::kDropReasonNames`
  (`overflow` 内存上限、`too_large` 单条超限、`no_single_owner` 没有单一玩家、`outbox_disabled` 进程没开发件箱、`no_id` 号段没号)。每计一次就是一笔改动没有流水或快照;
  同一时刻日志里有一行带 `metric=` 标签的 ERROR,玩家号只在那一行。
- `scene_audit_outbox_store_too_old_total`:存盘提交了、go/db 却没写发件箱(go/db 比 scene 旧),记录留在队列随下一笔再交。

只在日志里的:`audit_outbox_discarded_uncommitted`(改动没落库,记录随之作废,两边一致,不是故障)。

**告警**(`deploy/k8s/audit-outbox-alerts.yaml`,契约测试 `tools/scripts/tests/audit_outbox_alerts_contract.tests.ps1`;阈值 2026-11-12 定案,不等压测):

| 规则 | 条件 | 级别 | 依据 |
|---|---|---|---|
| `AuditOutboxForwardingLagging` | `max by (kind) (db_audit_outbox_oldest_pending_age_seconds) > 300`,for 5m | warning | 健康时一拍(1 秒)内投出,5 分钟说明转发或 Kafka 出了问题。不丢:行留在发件箱 |
| `AuditOutboxForwardingStalled` | 同上 `> 1800`,for 5m | critical | GM 回收按流水汇总应扣金额(§1),流水落后半小时以上,这段时间内的改动回收时会少扣;人工终结工具读已提交账本,不受影响 |
| `AuditOutboxRecordsSkipped` | `sum by (namespace, reason) (increase(db_audit_outbox_records_skipped_total[10m])) > 0` | warning | 跳过的记录永远不会进流水库;按 `reason` 查 go/db 日志 `[audit_outbox]` |
| `AuditOutboxRelayDisabled` | `max(db_audit_outbox_relay_enabled) == 0`(stable 副本全部不在转发),for 10m | critical | 发件箱只积不投;部署 `-AuditOutboxRelay paused` 暂停转发时也会响,换代手册里写明预期。转发跨 zone 分工,不按 namespace 分 |
| `AuditOutboxUnitsUnowned` | `sum(db_audit_outbox_relay_owned_units) < max(db_audit_outbox_relay_units)`,for 5m | warning | 有单位没人负责:某个 stable 副本登记在成员表里却没在转发(没配 Kafka、版本旧),它名下单位的积压年龄也没人报,上面的年龄规则看不见。成员表变化的几十秒里会短暂对不上,for 5m 滤掉。一个 Prometheus 只抓一个地域时成立 |
| `AuditOutboxRelayLoopStalled` | `time() - db_audit_outbox_relay_last_run_unixtime > 600`,按副本判、不聚合,for 5m | warning | 两次写时刻的最长间隔 = 一轮最长(`passBudget` 30 秒 + 在途单位的两次探测、取锁、读、删各一条 5 秒语句 + 投一批 5 秒 + 放锁 5 秒 = 65 秒)+ 两轮之间的最长等待(退避上限与 `IntervalMs` 上限,都是 30 秒)= 95 秒,阈值取它的 5 倍以上 |
| `SceneAuditRecordsDropped` | `sum by (namespace, reason) (increase(scene_audit_outbox_records_dropped_total[10m])) > 0` | warning | 健康时恒 0;每计一次就是一笔改动没有审计记录 |
| `SceneAuditStoreTooOld` | `sum by (namespace) (increase(scene_audit_outbox_store_too_old_total[10m])) > 0` | warning | 发布顺序错了(go/db 要先于 scene 升级,§10);记录不丢,go/db 升上来之后随下一笔写进发件箱 |

改 `passBudget`、`statementTimeout`、`produceTimeout`、`backoffMax`(`relay.go`)、`releaseTimeout`(`sql.go`)或 `MaxAuditOutboxIntervalMs`(`config/audit_outbox.go`)时,
同步改活性阈值:契约测试按源码里这些常量核「阈值不小于两次写时刻最长间隔的 5 倍」。

## 10. 上线前的迁移与发布顺序

- 项目未上线(负责人 2026-10-18),不做存量迁移,玩家数据可以清空:新表由 `cmd/migrate` 的基线建出;基线已经跑过的存档库建不出新表,
  按「清空重建」处理(与 `player_store` 当初同一口径)。go/db 第一次打开一个存档库时核表(`playerstore.VerifySchema` 连带核发件箱的列),缺表拒绝打开这个库,
  不会在缺表的库上一笔接一笔地以 1146 失败。压测与本机的清库脚本(`stress_round19.ps1`、`currency_crash_window.ps1`)一并清 `player_audit_outbox`,
  否则上一轮没投完的记录会投进下一轮的账本。
- 授权:写者的逐表 GRANT 按库里已有的表展开(`sql_boot_catalog_local.ps1` 的 `Own`),表建好之后 go/db 自动拿到 `SELECT,INSERT,UPDATE,DELETE`。
- 部署:K8s 的 db ConfigMap 由 `k8s_deploy.ps1` 渲染 `AuditOutbox` 段(Kafka 地址、世代与基名取 data_service 那份,§7.6);本机 `go/db/etc/db.yaml` 自带一份,
  docker 调试栈由 `prepare_docker_debug.py` 把地址改成容器内的 Kafka 并核三个键与 data_service 相等。审计 topic 换代时用 `-AuditOutboxRelay paused` 暂停转发、不停服
  (`docs/ops/audit-topic-generation-bump-runbook.md` §2.2)。
- 上线之后的发布顺序:**先 go/db、再 scene**。反过来的窗口里旧 go/db 会丢掉审计字段,scene 靠 `audit_outbox_written` 发现、保留记录并告警(§5.2、§5.6)。

## 11. 改动清单

2026-11-11 ~ 12,全部未生成协议、未编译、未测试。go/ 下的转发循环、接线、TiDB 预切与 Go 单测由另一个会话写,文件以 PROGRESS 那两条为准。

| 部分 | 文件 |
|---|---|
| 协议 | `proto/player_store/player_store.proto`(字段 10、11、响应 4、`PlayerAuditKind`);`proto/common/database/mysql_database_table.proto`(`player_audit_outbox`);`proto/common/base/config.proto`(字段 20 `audit_topic_generation` 改 `reserved`) |
| scene | 新 `cpp/libs/modules/audit/audit_outbox.{h,cpp}`、`audit_outbox_stats.h`(登记 `modules.vcxproj`);删 `audit_topic.h`;`transaction_log_system.{h,cpp}`(只有一条出路、拒收计数、`SetAssetChangedHook`,删 `LogItemTransfer` 与 Kafka 发送);`snapshot_system.{h,cpp}`;`player_store_client.{h,cpp}`(分笔、`auditStalled`、计数);`player_save_metrics.cpp`(导出两个计数);`player_lifecycle.cpp`(装配、两处快照、丢弃;钩子接线由「玩家数据存储策略优化」会话改);`cpp/libs/engine/config/config.cpp`(不再读 `AuditTopicGeneration`) |
| go/db | `internal/playerstore/{store.go,audit_outbox.go}`;新 `internal/auditoutbox/`;`internal/metrics/audit_outbox.go`;`internal/config/{config.go,audit_outbox.go}`;`player_store_wiring.go`(返回注册 uuid);新 `audit_outbox_wiring.go`;`db.go`;`etc/db.yaml`;`go.mod`(kafka-go 改直接依赖);`cmd/migrate/presplit.go`(TiDB 预切) |
| 共用 | 新 `go/shared/kafkautil/audit_topic.go`;`go/data_service/internal/config/config.go`(`topicForGeneration` 改调它) |
| 部署与脚本 | `tools/scripts/k8s_deploy.ps1`(db ConfigMap 的 `AuditOutbox` 段从 data_service 渲染、`-AuditOutboxRelay paused`,node ConfigMap 不再写 `AuditTopicGeneration`);`bin/etc/base_deploy_config.yaml`(删键);`tools/scripts/start_game.ps1`、`prepare_docker_debug.py`(核本机两份 yaml 相等);`deploy/docker-compose.yml`(注释);`stress_round19.ps1`、`currency_crash_window.ps1`(清 `player_audit_outbox`) |
| 告警 | 新 `deploy/k8s/audit-outbox-alerts.yaml`(§9、§5.6);`.github/workflows/deploy-config-tests.yml`(触发路径) |
| 测试 | `cpp/tests/currency_test/transaction_log_commit_gate_test.cpp`(`AuditOutbox`、分笔预算、内存上限、拒收计数、资产变动通知);`cpp/tests/bag_test/player_store_client_test.cpp` 末尾(随存盘交出与放行、分笔与续笔、`auditStalled`、store_too_old 计数);`bag_test.cpp`、`player_feature_snapshot_test.cpp` 的快照用例;`tools/scripts/tests/k8s_deploy_contract.tests.ps1`(同源、暂停开关);新 `tools/scripts/tests/audit_outbox_alerts_contract.tests.ps1`;Go 单测见 PROGRESS |
| 文档 | 本文;`docs/ops/audit-topic-generation-bump-runbook.md`(生产者改为 go/db、换代不停服);`deploy/k8s/README.md`;`infra-reconnect-overview.md` R13 行;`data_service_role_and_scope.md` 等几处旧说法 |

## 12. 残余

- **新旧混跑**:EQUAL 时 go/db 判断不了之前那次提交是不是旧 go/db 做的,按已写入回复;要「旧 go/db 提交、回包丢失、重试落到新 go/db」三件事
  同时发生才会少一批记录。发布顺序见 §10。
- **scene 崩溃**:队列在内存里,没随存盘提交的记录随进程一起丢 —— 它们描述的改动也没提交,两边仍一致。
  例外是分笔的窗口(§5.2):前一笔的改动已经提交、剩下的记录还在等续笔,这一两拍里 scene 崩溃,这些已提交改动的记录就丢了。
  只在一名玩家一次积了超过 512 KiB 记录时出现(存盘中断很久之后),不分笔的话这一笔要么超限被拒、要么只能丢记录,所以取分笔。
- **记录本身可能丢的情形都计数并告警**(§5.6 与 §6.3):scene 内存上限、单条超限、没有单一玩家、进程没开发件箱、号段没号,go/db 跳过的坏记录。
  每一种对应的改动照常提交,健康时全部恒 0。
- **快照内容是提交时刻的状态**,不是请求时刻的内存;进场快照约晚 1 秒,包含这 1 秒里的改动。
- **转发延迟**:正常一拍(1 秒)内投出;Kafka 恢复或暂停结束之后按时间顺序排空积压。积压期间 GM 回收按流水汇总会少扣这段时间的改动(告警描述里写了)。

## 13. Java 版(AGENTS §12)

Java 版的审计链路形态不同。按 11-05 起的约定本轮不写 Java 代码,在 Java 仓 `PARITY.md` 登记待做(2026-11-11「审计发件箱 R13」:
流水与快照随存档同事务写发件箱、转发至少一次、消费端去重、丢弃计数与告警)。不改客户端契约,也没有客户端改动。

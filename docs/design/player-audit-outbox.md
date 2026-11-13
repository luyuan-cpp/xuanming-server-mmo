# 玩家审计发件箱:审计流水与快照随存盘提交(断线重连 R13)

> 状态:2026-11-11 定稿并落码。A 批(协议、scene、go/db 写入与转发循环、单测)已写完,**未生成协议、未编译、未测试**,
> 验证步骤见 `docs/PROGRESS.md` 2026-11-11「审计发件箱」条目;B 批(K8s 渲染、告警、本机与压测脚本)见 §11。
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

流水是人工终结工具和 GM 回收的证据(「流水里有」= 已提交),快照是回档的依据,负责人的标准是不丢。

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
- TiDB 上和 `player_store` 一样纳入预切(`SPLIT TABLE player_audit_outbox BY (1),(2),…,(15)`),初值压测后定。

**大小**:流水一条一两百字节;快照一份上限 4 MiB(`playerstore.MaxAuditSnapshotBytes`),低于 TiDB 单 KV 6 MiB、
Kafka 快照 topic 的 `max.message.bytes` 16 MiB,超了不写发件箱、计数并告警(§6.3),存盘照常提交。

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
  就不放行这些记录,留在队列里随下一笔再交,并打 ERROR(§5.5)。

## 5. scene 侧

### 5.1 队列 `AuditOutbox`(`cpp/libs/modules/audit/audit_outbox.{h,cpp}`)

- 线程局部,只在 scene 逻辑线程上用,与原来的流水闸门同一个线程模型。按玩家一条队列,每条记录带进程内单调的水位,
  全进程一条数轴:实体销毁重建之后水位也不会回头,旧快照的水位盖不到新记录。
- **开关**:scene 装配存盘状态机时 `Enable(true)`。没开(battle、单测、工具)时,流水照旧直接发 Kafka(§5.4),快照请求被拒。
- **入队**:`EnqueueTransactionLog`、`EnqueueSnapshot`。单条超过 64 KiB 拒收并打 ERROR(属于调用方的 bug)。
- **流水入队通知(2026-11-12)**:`SetTransactionLogQueuedHook`。一条流水成功入队之后按玩家号调一次;scene 在装配存盘状态机时接上,
  用它请求关键进度存盘(1 秒内合并),于是货币、物品的变动和描述它的流水约 1 秒内一起提交,不用等定时存盘
  ([存盘规格 §5.3](./player-save-direct-commit.md))。钩子里只许登记稍后存盘:调用点在货币、背包的改动函数中间。
  拒收的记录、快照请求、没开发件箱时都不通知;快照请求要不要尽快提交由请求方决定(§5.5)。
- **上限**:每名玩家最多 4096 条、合计 512 KiB(按序列化大小加每条 4 字节的字段头计)。超了丢最旧的一条并打 ERROR
  (`metric=audit_outbox_overflow`,第一条和此后每 1000 条打一次)。正常一个定时存盘周期(60 秒)远到不了;到了说明这名玩家的存盘很久没提交。
  字节上限保证**一笔存盘总能带走全部到期记录**:行预算 3 MiB(go/db `MaxRequestBytes`)+ 审计 512 KiB + 余量,小于传输上限 4 MiB − 64 KiB,
  所以审计永远不会让一笔存盘超限被拒,也不需要拆成两笔(拆了就有「改动已提交、流水还在路上」的窗口)。go/db 启动时核这条不等式(§6.4)。
- **交出**:`HasDue(玩家, M)`、`AttachDue(玩家, M, 请求)` —— 水位 ≤ M 的记录拷进请求,队列里不删。
- **放行**:`AckThrough(玩家, M)` 删掉水位 ≤ M 的记录。只在这一笔 COMMITTED / EQUAL 且 `audit_outbox_written` 为真时调用,
  或者快照与已确认内容一样、没有到期记录而不用写的时候调用(这时删不掉任何东西)。
- **丢弃**:`Discard(玩家, 原因)`,三个时机照旧:实体销毁且存盘落定、实体已没而存盘落定(被废黜)、本节点重新认领之前。
  丢的是没落库的改动的记录,`metric=audit_outbox_discarded_uncommitted`。

### 5.2 存盘状态机(`player_store_client.{h,cpp}`)

`Options` 新增两个可选钩子:`auditDue(玩家, M)`、`attachAudit(玩家, M, 请求)`。

- `Start` 组请求时调 `attachAudit`。重试发的是同一份请求,审计记录和行一样同内容、同序号。
- 有到期记录就算没有脏行也写一笔(和 `force` 同一档):净变化为零的改动(先 +100 后 −100)同样有两条流水要交。
- `DeferPeriodicSave` 有到期记录时不推迟。
- 回包 COMMITTED / EQUAL:这一笔带了审计、`audit_outbox_written` 却为假(旧 go/db)时不调 `onCommittedThrough`,记录留在队列,
  打 ERROR(`metric=audit_outbox_store_too_old`,第一次和此后每 1000 次);行照常确认。其余与原来相同。
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

- `RecordEntry`:`tx_id` 无效照旧拒收;盖 zone 章;开了发件箱且只涉及一名玩家 → `AuditOutbox::EnqueueTransactionLog`;
  否则(没开发件箱的进程、两边都有玩家或都没有玩家的流水)照旧直接发 Kafka。后者在 scene 里没有调用方:`LogItemTransfer` 没人调,
  「两边都没有玩家」只在实体没有 Guid 时出现。它们仍会因 Kafka 不可用而丢,记为残余(§12)。
- 原来的闸门接口 `EnableCommitGating`、`CommitMark`、`PublishThrough`、`DiscardPending`、`PendingCount` 删掉,由 `AuditOutbox` 取代。

### 5.5 快照(`snapshot_system.{h,cpp}`)

- `CaptureAndSend` 改名 `RequestAuditSnapshot`:核实体与 Guid → 抽样(`audit_snapshot_policy`,不变)→ 发件箱没开就拒绝(打 ERROR,
  这个进程没有存盘,快照没有提交的地方)→ 领 snapshot_id(号段没号照旧跳过)→ 入队快照请求。不再在 scene 里序列化整档,
  也不再在这里核持久化组件:缺组件的玩家存盘会被拒(`MarshalPlayerForSave`),快照请求随实体一起作废,不会落成一份零值快照。
- 进场(`SNAPSHOT_LOGIN`):入队之后调 `RequestKeyProgressSave`,让快照在约 1 秒内随一笔存盘提交,内容是进场初始化之后的状态。
- 下线(`SNAPSHOT_LOGOUT`):随后就是最终存盘(带 release),快照内容就是最终提交的状态。

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
| `too_large` | 拼出来的快照超过 4 MiB |
| `undecodable` | 行拼接后解不开(行被写坏) |

### 6.4 预算核对

`playerstore.New` 拒绝 `MaxRequestBytes + MaxAuditBytesPerSave(512 KiB) + 64 KiB > 4 MiB − 64 KiB` 的配置:行预算调大到装不下审计时,
scene 会在本地按 TOO_LARGE 拒掉整笔存盘。`MaxAuditBytesPerSave` 与 C++ `AuditOutbox::kMaxBytesPerPlayer` 同值,Go 单测读头文件核对。

## 7. 转发循环(`go/db/internal/auditoutbox`)

### 7.1 分工(选主)

- 工作单位 `(存档库, 片)`,归属键 `audit-outbox:<storage_id>:<片>`,用 `shared/workshard` 的最高随机权重哈希在**全地域的 go/db** 之间分
  (成员表 = etcd 前缀 `PlayerStoreNodeService.rpc/zone/` 下全部 zone 的注册,canary 不算)。哪个 go/db 都能写哪个存档库,所以可以跨 zone 分。
- 分工只省重复劳动,不承担正确性:成员表变化的几秒里同一片可能两个副本都在投,结果是重投,由消费端去重;删除按主键,幂等。
- 只在 stable 轨道起;没配 Kafka、或者没有存档库清单时不起,`db_audit_outbox_relay_enabled` 报 0 并打 ERROR,发件箱照常写、等配好再投。
- 存档库清单 = 库名白名单(`MMORPG_DB_ALLOWED_DATABASES`,部署保证恰好是本地域的 K 个库)。白名单为空时(只有本机 `go run`)
  退到本进程打开过的库。前提:同一地域所有 go/db 的白名单相同(部署只有一份清单),否则某些片会落到打不开那个库的副本上,靠积压告警发现。

### 7.2 扫描

每个 (库, 片, kind) 一个游标 `scanFrom`(毫秒):

```sql
SELECT created_ms, player_id, record_id, payload FROM player_audit_outbox
 WHERE shard=? AND kind=? AND created_ms>=? ORDER BY created_ms, player_id, record_id LIMIT ?
```

- 投完一批:`scanFrom = max(scanFrom, min(本批最后一行的 created_ms, 现在 − 2 分钟))`;读到空:`scanFrom = max(scanFrom, 现在 − 2 分钟)`。
  2 分钟的余量盖住「created_ms 取值到提交可见」的延迟(存盘事务受 30 秒读超时约束)和 go/db 之间的时钟偏差。
- 每 10 分钟对每个单位从 0 全扫一次,接住时钟跳变之类余量盖不住的情况。刚接手的单位也从 0 开始。
- 有失败的批:`scanFrom` 停在第一条没投成的行上。
- 墓碑代价:每轮只跨过「现在 − 2 分钟」之后的墓碑,外加每 10 分钟一次全扫。
- 批量:流水 500 行、快照 8 行;一个单位一轮最多 20 批,整批读满就接着读,不等下一拍。空闲时按 `AuditOutbox.IntervalMs`(缺省 1000)一拍。

### 7.3 投递与删除

- kafka-go `Writer`:`RequiredAcks=RequireAll`、同步、`CRC32Balancer`(与 C++ 生产者的 librdkafka `consistent_random` 同一个分区算法,同一玩家照旧落同一分区)、
  lz4、`BatchBytes` 16 MiB(与快照 topic 的 `max.message.bytes` 一致)、不自动建 topic(topic 由 data_service `EnsureTopics` 与 kafka-topic-init 预建)。
- 整批确认之后按主键删:`DELETE … WHERE shard=? AND kind=? AND (created_ms,player_id,record_id) IN (…)`。部分失败时只删成功的那些。
- 删除失败(含写者闸门冻结):行留着,下一轮重投,消费端去重。闸门冻结(搬库窗口)只记 INFO,不算错误。
- topic 名:`kafkautil.AuditTopicName(基名, 世代)`,data_service 的消费端与它共用同一个函数;go/db 配置 `AuditOutbox.TopicGeneration` 与 data_service 的
  `Kafka.TopicGeneration` 必须同值,本机 yaml 由 Go 单测核对,K8s 由部署脚本从 data_service 那份渲染(B 批)。

### 7.4 失败与退避

- Kafka 写失败:本轮停,1 秒起翻倍、封顶 30 秒、乘 0.5~1 的随机系数。读、删失败只跳过那个库,其余库照常。
- 日志:连续失败第一次和此后每 30 次打 ERROR,恢复时打一行 INFO。不带 player_id 以外的玩家数据,不进指标 label。

## 8. 幂等与重复

至少一次,重复来自:

1. 投出去之后删除失败、或进程崩溃 → 下一轮重投。
2. 成员表变化时两个副本同投一片。
3. 结果未知后的写权拒绝、OLDER_SEQ:之前的尝试可能已经提交,记录留在 scene 队列,随下一笔再交一次。

消费端:流水 `tx_id` 主键 + `INSERT IGNORE`;快照按 `snapshot_guid` 去重,保留先到的那份。所以重复只多一次落库尝试,不会多一条记录。

## 9. 指标与告警

| 指标(go/db,label 都是封闭集合) | 含义 |
|---|---|
| `db_audit_outbox_records_written_total{kind}` | 随存盘写进发件箱的条数 |
| `db_audit_outbox_records_skipped_total{reason}` | §6.3 跳过的条数 |
| `db_audit_outbox_forwarded_total{kind}` | 投出并删除的条数 |
| `db_audit_outbox_relay_errors_total{stage}` | `read` / `produce` / `delete` 失败次数 |
| `db_audit_outbox_oldest_pending_age_seconds{kind}` | 本副本负责的单位里,最近一轮读到的最老未投记录的年龄;没有积压为 0 |
| `db_audit_outbox_relay_enabled` | 本副本转发循环是否在跑 |

scene 侧沿用日志标签:`audit_outbox_overflow`、`audit_outbox_discarded_uncommitted`、`audit_outbox_store_too_old`。

告警(B 批 `deploy/k8s/audit-outbox-alerts.yaml`):最老未投记录超过 5 分钟 warning、超过 30 分钟 critical(Kafka 保留期 30 天,积压本身不丢,
但 GM 工具查不到这段流水);`records_skipped_total` 增长 warning;stable 副本全部 `relay_enabled=0` 超过 10 分钟 critical。

## 10. 上线前的迁移与发布顺序

- 项目未上线(负责人 2026-10-18),不做存量迁移:新表由 `cmd/migrate` 的基线建出;基线已经跑过的存档库建不出新表,按「清空重建」处理
  (与 `player_store` 当初同一口径)。缺表时带审计记录的存盘每次都以 1146 失败、scene 一直重试,不会丢存档。
- 授权:写者的逐表 GRANT 按库里已有的表展开(`sql_boot_catalog_local.ps1` 的 `Own`),表建好之后 go/db 自动拿到 `SELECT,INSERT,UPDATE,DELETE`。
- 上线之后的发布顺序:**先 go/db、再 scene**。反过来的窗口里旧 go/db 会丢掉审计字段,scene 靠 `audit_outbox_written` 发现、保留记录并告警(§5.2)。

## 11. 改动清单

**A 批(2026-11-11,未编译)**

| 部分 | 文件 |
|---|---|
| 协议 | `proto/player_store/player_store.proto`(字段 10、11、响应 4、`PlayerAuditKind`);`proto/common/database/mysql_database_table.proto`(`player_audit_outbox`) |
| scene | 新 `cpp/libs/modules/audit/audit_outbox.{h,cpp}`(登记 `modules.vcxproj` 与 `.filters`);`transaction_log_system.{h,cpp}`;`snapshot_system.{h,cpp}`;`audit_topic.h` 注释;`player_store_client.{h,cpp}`;`player_lifecycle.cpp`(装配、两处快照、丢弃) |
| go/db | `internal/playerstore/{store.go,audit_outbox.go}`;新 `internal/auditoutbox/`;`internal/metrics/audit_outbox.go`;`internal/config/config.go`;`player_store_wiring.go`(返回注册 uuid);新 `audit_outbox_wiring.go`;`db.go`;`etc/db.yaml`;`go.mod`(kafka-go 改直接依赖) |
| 共用 | 新 `go/shared/kafkautil/audit_topic.go`;`go/data_service/internal/config/config.go`(`topicForGeneration` 改调它) |
| 测试 | `cpp/tests/currency_test/transaction_log_commit_gate_test.cpp`(改测 `AuditOutbox`);`cpp/tests/bag_test/player_store_client_test.cpp` 末尾;`bag_test.cpp`、`player_feature_snapshot_test.cpp` 的快照用例;Go 单测见 PROGRESS |

**B 批(未做)**:`tools/scripts/k8s_deploy.ps1` 的 db ConfigMap 渲染 `AuditOutbox` 段(brokers、世代与基名取 data_service 那份)及契约测试;
`deploy/k8s/audit-outbox-alerts.yaml` 与契约测试;docker 调试栈与 compose 的 go/db 配置;压测清库清单(`stress_round19.ps1` 等)加 `player_audit_outbox`;
TiDB 预切。B 批之前 K8s 上的 go/db 没有 `AuditOutbox.Brokers`,转发不起、`relay_enabled=0`,发件箱只积不投(不丢)。

## 12. 残余

- **没有单一玩家的流水**仍直接发 Kafka(§5.4),Kafka 不可用时会丢;scene 里目前没有这种调用方。
- **新旧混跑**:EQUAL 时 go/db 判断不了之前那次提交是不是旧 go/db 做的,按已写入回复;要「旧 go/db 提交、回包丢失、重试落到新 go/db」三件事
  同时发生才会少一批记录。发布顺序见 §10。
- **scene 崩溃**:队列在内存里,没随存盘提交的记录随进程一起丢 —— 它们描述的改动也没提交,两边仍一致。
- **快照内容是提交时刻的状态**,不是请求时刻的内存;进场快照约晚 1 秒,包含这 1 秒里的改动。
- **转发延迟**:正常一拍(1 秒)内投出;Kafka 恢复之后按时间顺序排空积压。
- 缺发件箱表的存档库:按需打开的只读闸在「基线已跑过」时只记 ERROR、不拒绝打开(A17c 的闸门口径),存盘会一直 1146 重试,见 §10。

## 13. Java 版(AGENTS §12)

Java 版的审计链路形态不同。按 11-05 起的约定本轮不写 Java 代码,在 Java 仓 `PARITY.md` 登记待做(2026-11-11「审计发件箱 R13」)。
不改客户端契约。

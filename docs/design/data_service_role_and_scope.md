# data_service 定位与边界

> 2026-10-15 按当前代码修订服务职责与 Login 依赖；本文区分目录/发号 RPC 与玩家存档的 Redis/Kafka 加载链。

## 定位
`go/data_service/` 承担全局玩家目录、永久 ID 号段、角色名登记，以及快照、流水与 GM 数据操作。它保留按 `player_id → home_zone_id → Redis` 路由的数据访问 RPC；当前 Scene 的普通载入与存盘仍直接使用共享 Redis，并经 Kafka DBTask 交给 go/db 落玩家库，跨 zone 游玩沿用这条加载链。

## 核心职责
1. **玩家目录** — `RegisterPlayerZone`, `GetPlayerHomeZone`, `BatchGetPlayerHomeZone`；维护逻辑归属与存储落点。映射持久化处于分批实施阶段，`RouteStore.Mode` 默认仍为 `redis`，详见 [映射持久化方案](./player-route-mapping-durability-proposal.md)。
2. **永久 ID 与角色名** — `AllocateIdSegment`, `ReservePlayerName`, `ReleasePlayerName`, `BatchGetPlayerName`。
3. **按归属路由的数据操作** — `LoadPlayerData`, `SavePlayerData`, `GetPlayerField`, `SetPlayerField`；这些 RPC 的存在不代表 Scene 的普通存盘经此中转。
4. **快照、流水与回档入口** — `CreatePlayerSnapshot`, `GetPlayerSnapshotDiff`, `CreateEventSnapshot`, `QueryTransactionLog`，以及两条审计 Kafka 消费者。`RollbackPlayer/Zone/All` 的生产 `RollbackFence` 尚未装配，真实执行保持 fail-closed，见 [zone_data_rollback.md](./zone_data_rollback.md)。
5. **GM 回收入口** — `BatchRecallItems` 的 dry-run 查询可用，真实回收尚未实现；勿把 RPC 骨架视为可执行回收能力。

## 与 Login 的关系

Login 已经依赖 data_service：建角前领取 PlayerId 号段、登记角色名与归属，角色列表和进场时查询权威 home_zone。`go/login/internal/svc/servicecontext.go` 装配共用的 `DataServiceClient`；`player_id_minter.go`、`home_zone.go` 与 `playernamereg` 分别封装调用。缺少归属/名字客户端会拒绝建角；号段失败按发号配置处理，默认禁止回退 Snowflake。

玩家 blob 走另一条链：EnterGame 先检查 Redis 父键，缺失时通过 Kafka 发 DBTask 读取子表，由 TaskResultDispatcher 的 Pub/Sub 回调拼装父键。EnterGame 的成功应答只表示已受理，真正进场由后续 Scene 通知确认。`sync_loader.go` 中的同步辅助路径不代表当前 EnterGame 在等待 BLPOP。目录与发号依赖的故障语义、预算及数据读路径应分别排查。

## 谁用 data_service
| 调用方 | 场景 |
|--------|------|
| Login | PlayerId 号段、角色名登记/释放、归属登记与查询 |
| Scene / guild 等永久 ID 使用方 | 领取各业务号段；需要名字/归属的调用方使用相应目录 RPC |
| GM/运维工具 | 快照、回档、批量回收、交易日志查询 |
| Kafka 审计生产者 | C++ Scene 发流水与快照，data_service 消费后写全局库 |

## 架构位置
```
Login / 号段与目录使用方 ── gRPC ──▶ data_service
                                     ├─ mapping Redis（归属/落点）
                                     └─ 全局 MySQL（号段、名字、快照、流水等）

Login / Scene ──▶ 共享 Redis（玩家父键/子表缓存）
       │
       └─ Kafka DBTask ──▶ go/db ──▶ 按当前存储落点选玩家库

Scene ── Kafka 流水/快照 ──▶ data_service ──▶ 全局 MySQL
```

## 关键文件
- `go/data_service/data_service.go` — 入口
- `go/data_service/internal/logic/data_logic.go` — Load/Save/Field 操作
- `go/data_service/internal/logic/rollback_logic.go` — 回档
- `go/data_service/internal/logic/snapshot_logic.go` — 快照
- `go/data_service/internal/logic/recall_logic.go` — GM 批量回收/交易日志
- `go/data_service/internal/routing/router.go` — player_id → zone → Redis 路由

## 全局库落库消费者与号段发号 (2026-09-08 追加)
落地 `docs/design/node-id-overhaul-plan-20260908.md` §2.0c 与 §6。全局库 = `SnapshotMySQL` 指向的库,表结构唯一真源是 `proto/common/database/rollback_database_table.proto`,由 `internal/store/schema.go` 用 proto2mysql 建表/补列(启动 `Schema.AutoMigrate`,或部署阶段 `data_service -f <yaml> -migrate`)。

`Schema.AutoMigrate` **没配 = true**(整段 `Schema:` 缺失、或 `Schema: {}` 都按建表处理),只有显式写 `false` 才关掉。它在代码里是 `*bool` 而不是带 `default=true` 的裸 bool:go-zero 的 mapping 不下钻一个"整段 optional 且未出现"的嵌套结构,里面的 default 标签一个都不会回填,裸 bool 会让一份没有 `Schema:` 段的 yaml 静默变成"启动不建表" —— 三个 store 全打在一张表都没有的库上,rollback / 流水查询 / `AllocateIdSegment` 每一次调用都在运行期炸(收口手写 DDL 之前,store 自带的 `CREATE TABLE IF NOT EXISTS` 还兜着这一层)。同一个坑在 go/db 的 `BlobGuardConfig` 与 login 的 `IdSegment.Enabled` 上各踩过一次。

### 两条 Kafka 消费者(`internal/kafka/`)
| 消费者 | topic → 表 | 落库方式 | 坏消息 | DB 故障 |
|--------|-----------|---------|--------|---------|
| `transaction_log` | `transaction_log_topic_g1` → `transaction_log` | 攒批(200 条 / 200ms)一条多行 `INSERT IGNORE`,**先插库后提交 offset** | 解不出 proto / `tx_id=0`:跳过并提交 | 有界重试后**停下不提交**(宁可积压不许丢),supervisor 60s 后从断点续消费 |
| `player_snapshot` | `player_snapshot_topic_g1` → `player_snapshot` | 每分区一个 worker(分区内 FIFO、先落库后提交本分区最高 offset);默认逐条,`Kafka.SnapshotBatchMaxRows > 1` 时多行批量(预读 + 普通多行 INSERT,见本格后文)。按 `snapshot_guid` 去重(唯一键 `uk_snapshot_guid_nz`:逐条用 ODKU `id = LAST_INSERT_ID(id)` 回带 id;多行批量先普通读剔除已存在的 guid 与批内重复,剩下的按 guid 升序拼**普通**多行 INSERT,撞 1062 退回逐条 —— 不用多行 ODKU:它撞已存在行时留下自增主键末尾的间隙锁直到语句结束、rebalance 重叠时成环,给自增列赋值还会在批内重复时报 1869,均为 2026-10-16 真库复现,见事故文档 docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md §9.6;仅 MySQL 缺键兼容窗口退回逐行 `NOT EXISTS`，TiDB 缺键拒绝建池),落 `source=1` | 解不出 / `snapshot_id=0` / `player_id=0` / 超 MEDIUMBLOB:跳过并随本批提交 | 同上;批量失败先逐条兜底,兜底也插不进去的那一行让整个消费者停在那里(与逐条同结局) |

生产者是 C++ scene(全局单 topic、裸 proto、key=player_id)。启动时 `kafkautil.EnsureTopics`(transaction_log 6 分区、snapshot 3 分区、保留 30 天;分区数是不可变契约)。Kafka 不可达**不拖死** data_service:第一次尝试就在后台 goroutine 里(EnsureTopics 对半死不活的 broker 要走完 sarama 30s 拨号 + 元数据重试,同步做会把 gRPC 启动和 Load/Save 一起按住),失败只记日志、每 30s 重试。指标:`kafka_consumer_up{consumer}`、`kafka_consumer_messages_total{consumer,outcome}`;两条 `kafka_consumer_up` 序列在第一次尝试**之前**就按 0 预注册,所以"消费者从来没起来"这个状态告警得到(不预注册的话那个 child series 根本不存在,`== 0` 的告警永远不触发,监控上和"服务没部署"长得一样)。

**G2a 快照审计管道扩容(2026-10-13,未经运行验证)**:`player_snapshot` 消费者改为「每分区一个 worker + 可配批量 + 实例级落库并发上限」,
旋钮是 `Kafka.SnapshotBatchMaxRows` / `SnapshotBatchMaxBytes` / `SnapshotFlushIntervalMs` / `SnapshotMaxInflightFlushes`,**默认值 = 之前的逐条串行**
(批 1 条、并发 1、不等待),越界拒启;打开批量要等压测对比表与内存护栏拍板。新增 `Kafka.LagProbeIntervalMs`(yaml 默认 30000):
sarama 只读探针按 broker 视角报消费组 lag(全部分区合计与单分区最大值)与「保留期已删掉的未消费条数」。新指标 `kafka_consumer_flush_seconds{consumer,result}`(单次 DB 尝试)、
`kafka_consumer_batch_rows`、`kafka_consumer_message_bytes`、`kafka_consumer_lag_messages`、
`kafka_consumer_lag_max_partition_messages`(单分区最大积压:broker 的 retention.bytes 按分区执行,快照积压告警比的是它;每个 consumer 一条序列,不按分区出指标)、
`kafka_consumer_retention_gap_messages`、
`kafka_consumer_lag_probe_total`、`kafka_consumer_isolation_total`,告警规则在 `deploy/k8s/data-service-audit-alerts.yaml`。
全球并行度上限仍是分区数(快照 3);要扩只能离线换代,保留期在开服速率下远比 30 天先到 —— 两件事的步骤与容量算式见
[audit-topic-generation-bump-runbook.md](../ops/audit-topic-generation-bump-runbook.md)。

关停顺序:`main` 先 cancel 消费者 ctx、有界等它们退出(8s,`kafkaShutdownGrace`),**之后**才 `svcCtx.Close()` 关连接池。反过来的话 txlog 的 `flushOnShutdown` 只会拿到一个已经关掉的池,那段"关停前尽力落最后一批"等于从来没写过。等超时就照常关池:未提交的 offset 会在下次启动时重放,`tx_id` 主键与 `snapshot_guid` 去重让重放幂等。

#### topic 名带代号(`Kafka.TopicGeneration`,默认 1)
有效名 = `<基名>_g<TopicGeneration>`,当前即 `transaction_log_topic_g1` / `player_snapshot_topic_g1`。C++ 生产者已由部署配置 `AuditTopicGeneration` 拼接代号，入口见 `cpp/libs/modules/transaction_log/transaction_log_system.h`、`cpp/libs/modules/snapshot/snapshot_system.h`；必须与 Go 的 `Kafka.TopicGeneration` 同步，不能再按“修改两处编译期常量”操作。

为什么连第一代也带后缀(与 go/db 的 `DbTaskTopicForGeneration` 在 `<=1` 时用裸名不同):broker 开着 `auto.create.topics.enable` 且 `num.partitions=1`(`deploy/docker-compose.yml` 的 `KAFKA_NUM_PARTITIONS=1`;k8s 清单没设,同样是默认 1),C++ 生产者一发消息就把**裸名字**的 topic 自动建成 1 分区。`EnsureTopics` 拿 6/3 的契约去比,只会永远返回 `partition contract mismatch`,两条消费者一条都起不来,唯一症状是每 30s 一条 Error 日志,而流水/快照全被保留期吃掉。带代号的名字与那两个自动建出来的裸 topic 互不相干。

分区数要改 = `TopicGeneration` +1(换一批新 topic,老 topic 排空后删),**绝不**原地扩分区:扩分区会重映射 `key=player_id` 的哈希,同一玩家的流水顺序就断了。部署侧应当预建这两个 topic 并给足分区(6 / 3),不要依赖 broker 自动创建 —— k8s 上 Kafka pod 一重启就会按 1 分区重建自动创建的 topic,下一次 data-service 重启就永久卡住。

### `player_snapshot.source` 列契约
- `source=0`:GM / data_service 自己写入。`snapshot_type` 是 data_service 的 `SnapshotType`,`data` 是 JSON 字段图 `{fields: map[string][]byte}`,`snapshot_guid=0`。
- `source=1`:C++ scene 经 Kafka 落库。`snapshot_type` 存 C++ `SnapshotTrigger` **原值**,`data` 存收到的整条 `PlayerSnapshotEntry` 序列化字节(含两个 player_database blob),`snapshot_guid` = C++ `snapshot_id`,`operator="scene-node"`,`zone_id`/`created_at` 取自载荷。
- **现有 GM 读路径一律只看 `source=0`**(`GetSnapshotByID` / `GetLatestSnapshotBefore` / `ListSnapshotsMeta` / `GetSnapshotPlayerIDsByZone`),直到回滚路径学会解 proto blob;`ListSceneSnapshotsByPlayer` 是 `source=1` 的专用读路径(尚未接 RPC)。`DeleteOldSnapshots` 两种 source 都删。

### `snapshot_guid` 去重规则
`snapshot_guid` 的普通索引用于查询；非零 GUID 的唯一性由额外的可空生成列 `snapshot_guid_nz` 和其单列完整唯一键 `uk_snapshot_guid_nz` 约束。生成列是可空 `BIGINT UNSIGNED`：零值映射为 `NULL`，其他值保持原 uint64 GUID。因此多个 GM 快照（`guid=0`）可以共存，同一非零 GUID 最多一行。生成列不进入 proto，列注释不使用 `pb:N`，避免与 proto2mysql 的列同步争夺所有权。

2026-10-17 起迁移和 `NewSnapshotStore` 共用服务端方言选择：MySQL 保留既有 `NULLIF(snapshot_guid, 0)` + `STORED`；TiDB 使用 `CASE WHEN snapshot_guid = 0 THEN NULL ELSE snapshot_guid END` + `VIRTUAL`。TiDB 不支持通过 `ALTER TABLE` 添加存储生成列，也不支持生成表达式中的 `NULLIF`，但支持虚拟生成列上的唯一索引。[TiDB 生成列文档](https://docs.pingcap.com/tidb/stable/generated-columns/)

迁移快路径、DDL 后回读和建池探测使用同一组形状判据：生成列的可空 uint64 类型、存储形态与表达式必须正确，指定唯一索引必须恰好覆盖该列全长。仅名字相同的普通索引、联合唯一索引、其他列的唯一索引、普通列或不同表达式都不能放行。坏结构返回明确错误，不自动删列或重建索引。存量列的纯校验先于历史去重的第一个 `DELETE`；原有停写、迁移锁、有界清重及保留每组最小 `id` 的流程不变，零 GUID 行不参与清重。大表仍应停写后执行 `data_service -f <yaml> -migrate`，同库可重入。

有真实唯一约束时，逐条入口使用 `INSERT ... ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`，重放返回原记录；多行入口沿用预读、普通多行插入和撞键后逐条兜底。`InsertSnapshotIfGuidAbsent` 拒绝零 GUID，GM 零 GUID 使用普通插入入口。`transaction_log` 继续依赖 `tx_id` 主键与 `INSERT IGNORE`，不增加另一套生成列。仅 MySQL 在缺键兼容窗口保留依赖 RR 间隙锁的 `NOT EXISTS` 退路；TiDB 没有此前提，约束缺失、形状错误或探测失败时必须关池拒启。`MigrateOptions.AllowMissingGuidUniqueKey` 只保留旧调用方源码兼容，不再提供绕过唯一约束的通道。

本轮证据在 `build/asset-arrival-20261017/fullstack/`：正式 CLI 和正式迁移测试先复现旧 TiDB 预拒绝；修复后同库迁移两遍、两个零 GUID 共存、MaxUint64 重复被真实 SQL 以 1062 拒绝、四个并发写者只有一个新增行，以及原流水主键去重均通过。七种缺失或错误结构先真实复现旧写池错误放行，再由同源 Linux 默认 store 测试验证拒绝或正确补键。该组 **84 个顶层测试通过、零失败、零跳过**，其中两个顶层测试使用真实 TiDB v8.5.2，七个坏结构场景是子测试，不另加成顶层总数。最终输入、二进制 SHA、镜像、日志与容器清理记录见 `data-service-guid-final.json` 和 `dataservice-store-linux02-results.json`。本轮未启用 `integration` 构建标签，也未重跑原生 MySQL 实库套件。

Windows 第一轮真实正例已通过；第二轮新测试程序在运行前被系统以 `AccessDenied` 拒绝，不能计作绿色结果。最终验证使用 Windows Go 交叉编译相同源码的静态 Linux 测试程序，在已有镜像创建的临时容器内连接同一个专用 TiDB 库；未改变 Windows 防护、旧容器或旧库全局参数，临时容器已清理。这组快照证据不代替完整服务启动或邮件、交易到账验收：`RouteStore` 还要求自己的分片 `CHECK` 真正执行；TiDB 的 `tidb_enable_check_constraint` 只有全局作用域，不能通过会话设置为共享旧实例上的新库单独启用。[TiDB 系统变量文档](https://docs.pingcap.com/tidb/stable/system-variables/#tidb_enable_check_constraint-new-in-v720)

历史说明：2026-10-08 之前的普通索引加 `NOT EXISTS` 方案曾依赖 InnoDB 间隙锁与单实例部署。可空唯一键落地后，这已不再是正常写入契约，也不再据此要求 `Recreate`；当前多副本部署见 [横向扩容设计](no-single-node-horizontal-scaling-20261001.md)。

### `AllocateIdSegment(biz_tag, step) → [lo, hi)`
- 半开区间,一经返回视为已发出、绝不重发;段内剩余号作废不回收。
- 行由迁移按 `IdSegment.BootstrapTags` 预建,从 **1** 起(0 保持非法);表里没有的 `biz_tag` 在生产(`IdSegment.AllowAutoSeed=false`,默认)→ `ErrCodeIdSegmentUnknownTag`(故障码,表零变更),只有 dev 显式 `AllowAutoSeed: true` 才运行期自动种行。`step=0` 沿用行上的 step,非 0 则覆盖并回写;`step` 上限 10,000,000。
- `biz_tag` 必须匹配 `^[a-z0-9_]{1,64}$`;非法 tag / 非法 step → `ErrCodeInvalidRequest`(不算故障,表零变更)。
- 值域上限 **2^55**(与存量 snowflake 号 ≥ 6.7e16 永不相交):`max_id + step ≥ 2^55` → `ErrCodeIdSegmentExhausted`,fail-closed、表零变更;表上的 `CHECK (max_id < 2^55)` 兜底手工 SQL。
- 并发:`SELECT ... FOR UPDATE` + `WHERE version=?` CAS,同 tag 串行;死锁/锁超时重试 4 次。store 不可用 → `ErrCodeIdSegmentDBError`(故障码,进拦截器告警)。指标 `id_segment_allocate_total{biz_tag,outcome}`。

### `id_segment` bootstrap 例外
本包唯一一段手写 DDL(`store/schema.go: idSegmentBootstrapDDL`):proto2mysql v0.1.0 把 string 主键渲染成 MEDIUMTEXT(MySQL 1170 建不了表),所以 `id_segment` 用 `biz_tag VARCHAR(64)` 预建,之后**不对它跑** `CreateOrUpdateTable`(会试图 MODIFY 回 MEDIUMTEXT),只做"proto 每个字段都有同名列"的漂移检查。其余三张表全部由 proto 驱动。

### `id_segment` 行的生命周期与恢复手册(设计 §7.5 第 7 条)
每一行都是一个永久身份的发号水位。行只会在**全局库被 drop 重建、或从旧备份恢复**时消失 / 回退,而消费表(`player_database` / `guild` / 玩家 blob 里的物品、Kafka 里的流水与快照)还留着已发出的号;此时从 1 重发,login 的 `INSERT ... ON DUPLICATE KEY UPDATE` 会**静默覆盖别人的角色行**,没有任何报错。所以计数器必须有一条"库被重置也活得下来"的底线:

- **运行期不补种**(`IdSegment.AllowAutoSeed`,没配 = false):缺行 → `ErrCodeIdSegmentUnknownTag`(20,进 `FaultCodeSet` 告警),不写任何行,Error 日志点名 tag 与补救步骤。dev yaml 显式 `true` 只为本地随手换 tag,k8s ConfigMap 按 profile 镜像(staging/prod 固定 false)。
- **行由迁移显式创建**(`IdSegment.BootstrapTags`,没配 = `player, guild, item, txlog, snapshot, trade_listing, guild_asset_op, friend_request, mail`;`friend_request` 是好友申请代次号;`mail` 是邮件 mail_id / sys_mail_id,邮件 M1a 加入,消费表在独占库 `mmorpg_mail`、不做地板校验;`trade_listing` 是聚宝斋商品号,2026-09-14 加入;`guild_asset_op` 是帮会资产指令 op_id,2026-09-20 帮会二期 B5a 加入,消费表在独占库 `mmorpg_guild`、不做地板校验):`-migrate` 与 AutoMigrate 都在四张表就位之后对每个 tag `INSERT IGNORE ... (max_id=1, step=100, version=0)`,幂等,**绝不降低**已有行的 max_id;每行创建 / 已存在各打一条 Info。清单里出现非法 tag(不匹配 `^[a-z0-9_]{1,64}$`)整次迁移失败。新增一种永久身份 = 加一个 tag。
- **地板校验**(`store/schema.go: idSegmentFloorSources` → `raiseIdSegmentFloor`):迁移接着对 `player` / `guild` 两行做 `max_id ← MAX(主键 < 2^55) + 1`(只在 `MAX ≥ max_id` 时;`< 2^55` 排除存量 snowflake 号;UPDATE 带 `max_id < ?` 守卫,与并发发号互斥安全)。消费表必须与 `id_segment` **同库**才查得到(`INFORMATION_SCHEMA.COLUMNS` 探测,不在就 Info 跳过);抬升时打 Error `id_segment counter was behind the consuming table — backup restore?`,出现即意味着库曾被重置,按下面的手册排查已被覆盖的行。`item` / `txlog` / `snapshot` **做不了**这种校验:item guid 在 zone 库的玩家 blob 里;tx_id / snapshot_id 所在的 `transaction_log` / `player_snapshot` 与 `id_segment` 同库、会被一起恢复到旧水位,而真正"已发出"的集合在 Kafka 积压与 C++ 进程手里的段里(重放的旧流水会与重发的新号撞 `tx_id` 主键,`INSERT IGNORE` 静默丢掉**新**的流水)。

**运维手册(恢复全局库 = ID 安全事件,不是普通库恢复)**:恢复 `id_segment` 所在的全局库之前,必须先核对每个 tag 的 `max_id ≥ 消费侧最大号 + 1` —— `player` 看各 zone 库 `MAX(player_database.player_id) WHERE player_id < 2^55`,`guild` 看 `MAX(guild.guild_id)`,`item` 看各 zone 库 blob 里 bag 的最大 guid,`txlog` / `snapshot` 看两个 Kafka topic 里的最大 `tx_id` / `snapshot_id`;不够就手工 `UPDATE id_segment SET max_id = <最大号 + 1>, version = version + 1` 抬上去,再启动 data_service / 跑 `-migrate`。消费表在各服务独占库里的 tag(`trade_listing` / `guild_asset_op` / `friend_request` / `mail`)同样要核对,各自查哪张表见 `deploy/k8s/README.md`「恢复全局库前须先核对」一条。迁移里的自动地板校验只是同库能查到时的兜底,"跳过"不代表安全。回归用例:`store/id_segment_integration_test.go`(缺行拒绝零写入 / bootstrap 幂等不降位 / 地板抬升与 snowflake 号排除 / dev 补种)、`logic/id_segment_integration_test.go`(错误码定性)。

### 存量库(老手写 DDL 建的表)升级路径
proto2mysql 的 `syncTableSchema` 只在表**不存在**时执行完整的 `CREATE TABLE`(索引在那条语句里);表已存在时它一条索引都不建,只 `ADD/MODIFY/CHANGE COLUMN`。所以任何一台跑过旧版 data_service 的机器,迁移之后是"列齐了、索引缺了":`player_snapshot` 少 `snapshot_guid` 索引 → 消费者每落一条快照全表扫一次,而且没有任何报错。

`store/schema.go: ensureIndexes` 在每张表 `CreateOrUpdateTable` 之后补索引,幂等:
- 想要的索引清单从 proto2mysql **生成的建表语句**里解出来(索引真源仍只有 proto 一处,名字与新库直接建出来的一致);
- 外加 `extraIndexes`:`player_snapshot(zone_id, created_at)` —— 手写 DDL 时代的 `idx_zone_created`,收口到 proto 时漏了,而 `GetSnapshotPlayerIDsByZone` / `RollbackZone` 正按这两列过滤。长期修法是改 proto(本服务不拥有 `proto/`):把 `rollback_database_table.proto:64` 的 `option(OptionIndex) = "player_id;snapshot_guid";` 改成 `"player_id;snapshot_guid;zone_id,created_at"`;改完后 `extraIndexes` 那条会自动被判为"已覆盖",不会建重复索引;
- 覆盖判据是**前缀**不是同名:存量表的索引名是手写 DDL 起的(`idx_zone_created`),只要某条现有索引的前若干列正好是想要的列序就跳过,不重复建。

存量表的列一个 `pb:N` 注释都没有,所以升级那一次会对**每一列**发 `MODIFY COLUMN`(顺带 `longblob→mediumblob`、`varchar→mediumtext`),等于整表重写。启动路径只给这件事 5 分钟(`svc.autoMigrateTimeout`),大表会超时(进程侧三个 store 全置 nil、服务降级,MySQL 侧那条 ALTER 还在跑)。`migrateSchemaOn` 因此在同步前先检测并打一条 Error 日志点名该表。**刻意没有**把启动路径改成"拒绝重写、只许 `-migrate`":那会把所有存量 dev 库上的 data_service 一次性锁死在 fail-closed 状态,代价比它防的问题大。存量库升级请走 `data_service -f <yaml> -migrate`(不设时限)。回归用例:`store/schema_legacy_integration_test.go`(老 DDL 建表 + 种数据 → 迁移 → 数据在、新列在、索引在、二次迁移零变更)。

### 配置键(`etc/data_service.yaml`)
```yaml
Schema:
  AutoMigrate: true            # 没配 = true;生产显式设 false,改跑 data_service -f <yaml> -migrate
IdSegment:
  AllowAutoSeed: true          # 没配 = false(生产必须 false):运行期遇到缺行是否自动种一行从 1 起
  BootstrapTags: [player, guild, item, txlog, snapshot, trade_listing, guild_asset_op, friend_request, mail]   # 没配 = 这份默认清单;迁移 INSERT IGNORE 预建,绝不降位
Kafka:
  Brokers: [127.0.0.1:9092]    # 留空 = 不消费(本地无 Kafka 合法)
  TopicGeneration: 1           # 有效 topic 名 = <基名>_g<N>;改分区数就 +1,绝不原地扩分区
  TransactionLogTopic: transaction_log_topic   # 基名 → 实际 transaction_log_topic_g1
  TransactionLogPartitions: 6
  TransactionLogConsumerGroup: data_service-transaction-log
  SnapshotTopic: player_snapshot_topic         # 基名 → 实际 player_snapshot_topic_g1
  SnapshotPartitions: 3
  SnapshotConsumerGroup: data_service-player-snapshot
  RetentionMs: 2592000000      # 30 天
```
两个 store 与迁移共用一条 DSN(`store/mysql.go: buildDSN`),会话级 `STRICT_TRANS_TABLES`,超长写入报错而不是静默截断。

### 集成测试
`go test -tags=integration ./internal/store/... ./internal/kafka/ ./internal/logic/`:接真实 MySQL,每个用例建 `data_service_it_<pid>_<n>` 一次性库、跑完 DROP,yaml 指向的库不被触碰;MySQL 不可达或无建库权限时 Skip。本地 docker 的 `appuser` 没有 CREATE DATABASE 权,用 `DATA_SERVICE_IT_MYSQL_USER=root DATA_SERVICE_IT_MYSQL_PASSWORD=<deploy/docker-compose.yml>` 跑。

### 已知缺口
- 表 proto `player_snapshot` 的 `OptionIndex` 还缺 `zone_id,created_at`,暂由 `store/schema.go: extraIndexes` 补(见上一节;`proto/` 归另一位负责)。
- 本地 yaml 的 `SnapshotMySQL.DBName` 仍为 `testdb`；目标库是否存在及账号是否获授权取决于环境，不能从 yaml 推断本机实时状态。联调前应预建目标库并授权，或显式选择已获授权的全局库；迁移失败会导致相应 store 不可用，不能把服务端口已监听视为数据能力就绪。

### 已澄清(不再是缺口)
- C++ 审计 topic 已按 `AuditTopicGeneration` 生成带代号的名字，部署渲染的 data-service 配置也包含 `Kafka.TopicGeneration`；旧版“生产者仍发裸 topic”的待办已完成。换代仍需同步配置并按审计 topic 换代 runbook 排空旧代。
- ~~表 proto `transaction_log` 没有 `zone_id` 字段,消费者收到的 `zone_id` 被丢弃~~ —— **已过时**:`rollback_database_table.proto` 的 `transaction_log` 已有 `uint32 zone_id = 15`,`store.txLogHasZoneColumn` 因此为 true,`InsertBatchIgnore` 的列序里带着 `zone_id`,`NewTransactionLogStore` 那条 Error 不会打。回归用例 `TestTransactionLogStore_TimestampSecAndZoneLand` 直接断言这一列真的落库。
- **玩家数据路由失败一律回 ErrCodeRedis(1),不另分码**(2026-10-19 裁决):LoadPlayerData / SavePlayerData / SetPlayerField / DeletePlayerData / GetPlayerField 与 CreatePlayerSnapshot 第 3 步,在路由失败(映射缺席、值畸形、mapping Redis 故障或熔断、ReadRequiresReady 回源失败、zone 未配集群)时都回 1。理由:in-band 数据路径上「没有映射」分不清是不存在的 id,还是「有数据、映射丢了」的事故,只能 fail-closed 当故障;4 不在 FaultCodeSet 里,改成 4 会让 rpc_inband_fault 与 inband_fault_total 哑掉,还会和快照的「无数据」4 混在一起;30 在当前部署(DevRedis + RouteStore.Mode: redis)下不可达。原因只在 `[DataRoute] <op> player=<id>:` 的 ERROR 日志里。账本读(GetPlayerAssetOpLedger / GetPlayerInboxLedger)路由失败回 Unavailable,message 带原因,绝不 NotFound(login 的 homezone.IsUnmapped 与 player_inbox 的 isHomeZoneUnmapped 会把 NotFound 当「无映射→回退」)。合服围栏是例外:写路径回 22(规则拒绝),见「写门禁契约」。等 Regions 或 ReadRequiresReady=true 上线、且这些 RPC 有了真实调用方时,再按调用方需要评估独立码;届时须同时改 FaultCodeSet,以及 snapshot_logic / rollback_logic 里把 err 再折回 1 的地方。

## 玩家路由 `ClientForPlayer` 的分布式审计(2026-10-18)

起因是一个问题:`LoadPlayerData` 里的 `svcCtx.Router.ClientForPlayer(ctx, req.PlayerID)` 有没有分布式问题。审计分 4 个视角并行(映射变更时序、映射存储一致性、Redis Cluster 命令语义、调用方影响),去重后 19 条发现,每条再由「代码事实」「生产可达性」两个视角对抗核验。结论与整改状态记在这里,整改流水见 `docs/PROGRESS.md` 2026-10-19 的两条 data_service 条目。

### 结论
- 当前部署下不会路由错:本地 `etc/data_service.yaml` 与 K8s ConfigMap(`tools/scripts/k8s_deploy.ps1` 的 go-svc-data-service-config)都配了 `DevRedis`,`ClientForPlayer` 第一行短路返回同一个 `devClient`,根本不读 `player:zone:{id}`。所有 zone 的 data_service 副本都连同一套 Redis,结果一致。映射被合服改写、mapping Redis 切主丢键等路由型问题今天都碰不到。
- `player:{id}:<field>` 键族只有 data_service 自己写(Save / SetField / 回档 / debug_import),不在游戏存档主链上(主链是 `PlayerAllData:{id}`)。`LoadPlayerData` 的生产调用方只有 GM 快照、快照 diff 和 debug_fetch,都不用返回的 Version。
- `ClientForPlayer` 真正的生产用途是账本读:`persisted_player_database.go` 读 `PlayerAllData:<id>`,`asset_sql_commit.go` 读 `player:{id}:asset_sql_commit`,供 guild / trade / mail / player_inbox 使用。它们读的是 scene 和 go/db 写的键,正确性完全依赖配置同址。

### 已整改(2026-10-19 三批,2026-10-22 第四批)
下表标「二」「三」「四」:第三批是对第二批的对抗评审修正,第四批是 10-19/20 只读核对中复核成立的遗留项。验证状态(2026-10-22 订正):
- 第一批 data_service 部分:Codex 在 `e611b5d8d` 上跑过定向与全量单测(PROGRESS 同日 Codex 条目)。之后第二、三批改了同一批文件,这份结果只作历史证据。
- 第一批 DevRedis 同址契约:在 `k8s_deploy_contract` 套件里实跑通过。
- 第一批 merge_zone `checkDataRedisBlobFlags`:从未编译、运行。第三批 merge_zone 集成用例同样从未运行。
- 第二、三批的 `internal/logic` 包:2026-10-20 在 `c35e1133d` 授权副本上编译、单测通过,真 Redis 两版也通过(见下文「字段 Lua 验证结果」)。
- routing 包、data_service 全量单测:当前提交上还没有运行证据。第四批未编译、未测试,待 Codex 验证。

| 批 | 问题 | 改法 | 位置 |
|----|------|------|------|
| 一 | `LoadPlayerData` 先读字段、再单独 GET 版本,两步之间有存盘就返回「旧数据 + 新版本」,调用方据此 CAS 会覆盖别人的写 | 指定字段:版本键与字段键同一条 MGET。全量读第二批改走字段索引(见下一行);存量玩家的 SCAN 回退保留「先读版本、版本变了回旧版本」 | `internal/logic/data_logic.go` |
| 一 | 锁释放沿用请求 ctx,请求超时/取消后释放命令发不出去,两把锁挂到 `PlayerLockTTLSec` | `lockReleaseContext`:`context.WithoutCancel` + 1s 超时;第二把锁失败时回滚第一把也走它 | `internal/routing/router.go` |
| 一 | 账本读的正确性只靠 DevRedis 与 C++ zoneredis、go/db RedisClient 同址同库,仓库里没有守卫;漂移后账本读全是 Nil → found=false,fail-closed 但只丢活性、很难发现 | 契约测试钉住三者同址且都是 DB 0 | `tools/scripts/tests/k8s_deploy_contract.tests.ps1` |
| 一 | 合服第 2 步拷 blob 只看 `-migrate-player-blobs`、与 pin/copy 模式无关;两端 data Redis 不同又不搬时,改映射后 `LoadPlayerData` 读到「成功、空数据、Version=0」 | merge_zone `checkDataRedisBlobFlags`:声明了不同的 `-source-data-redis` / `-target-data-redis`,却既没给 `-migrate-player-blobs` 也没给 `-skip-player-blob-migration` 时,写前拒跑;`player-storage-placement.md` 更正「copy 模式无条件拷 blob」的错误说法 | `tools/merge_zone/main.go` |
| 一 | Regions 表里同一 zone 出现两次时后写静默覆盖先写(debug_fetch 却取先写),空地址、空 zone 不报错 | `config.ValidateRegions`,`NewRouter` 在非 dev 分支建集群客户端之前校验,不合法拒启 | `internal/config/config.go`、`internal/routing/router.go` |
| 二 | 全量读与 `DeletePlayerData` 靠 `SCAN player:{id}:*` 找键,单机 DevRedis 上扫整个共享 Redis;删除连带删掉别的模块的 `save_barrier` / `asset_sql_commit`(HASH),Save / SetField 能把它们覆盖成 string(C++ 存盘随后对该玩家永久失败) | 字段索引 `player:{id}:__fields` + 保留名 + 写前 TYPE 校验 + 删除只删自有 string 键,见下文「字段索引与保留名」 | `internal/logic/data_logic.go`、`internal/metrics/metrics.go` |
| 二 | 写路径不看合服围栏、拿锁后不复核映射(Regions 跨集群合服时会丢写) | 写门禁 `Router.AcquirePlayerWriteLock` + merge_zone 拷 blob 前查数据锁,见下文「写门禁契约」 | `internal/routing/router.go`、`internal/logic/data_logic.go`、`internal/logic/rollback_logic.go`、`tools/merge_zone/player_blob_migrate.go` |
| 二 | 路由错误全部折叠成 `ErrCodeRedis`,server 层丢掉错误文本 | **码不改**(裁决见「已澄清」),只加原因日志:in-band 入口经 `clientForPlayer` / 写门禁记 `[DataRoute] <op> player=<id>:`,快照第 3 步同样记;账本 SQL 证据读的 Unavailable message 带原因 | `internal/logic/data_logic.go`、`snapshot_logic.go`、`asset_sql_commit.go` |
| 二 | merge_zone `scanPlayerKeys` 在 `ForEachMaster` 的并发回调里无锁读写同一个 map / slice | 加 `sync.Mutex` | `tools/merge_zone/player_blob_migrate.go` |
| 三 | `cmd/debug_import` 的 player 模式把版本重置为 `"1"` 却不删字段索引:玩家恰好只存过一次(戳 `"1"`)时,导入的字段被可信索引藏掉,下一次存盘还会固化 | `writeLegacyPlayerFields`:一个 MULTI 里先 DEL 索引、再写字段、最后重置版本 | `go/data_service/cmd/debug_import/main.go` |
| 三 | 写脚本的版本守卫放过 `"007"` 这类前导零串,真 Redis 的 INCR 会拒绝它,此时字段已 SET,留下半截写(miniredis 接受,单测测不出) | 守卫改成规范十进制;读路径 `parseVersionValue` 同一口径 fail-closed | `internal/logic/data_logic.go` |
| 三 | merge_zone 拷 blob 前查数据锁、且必须早于 SCAN,没有任何测试 | 真 Redis 集成测试 `TestIT_CopyPlayerStringKeys_RefusesWhileDataLockHeld`(tag `merge_integration`);字段索引键名在 data_service 与 merge_zone 两边各钉一次字面量 | `tools/merge_zone/blob_lock_integration_test.go`、`internal/logic/player_fields_test.go` |
| 四 | 读路径把「版本键存在、值为空串」当成无版本,回 Version=0 成功,与 LoadPlayerData 的 fail-closed 契约和写侧守卫不一致 | `parseVersionValue` 空串按损坏处理;「键不存在」只由 `parseVersionReply` 的 nil 分支表达 | `internal/logic/data_logic.go` |
| 四 | 拿锁时 SETNX 结果不明:回包丢失、或客户端重试撞上自己写入的 token。此时只回滚 mapping 锁,自己写下的数据锁挂到 `PlayerLockTTLSec`,期间该玩家的写全部 LockConflict | 任一把锁没拿到,都把可能已落地的锁按 token 释放:mapping 失败释放 mapping,数据失败两把都释放。只删自己的,代价是锁冲突时多一次释放往返 | `internal/routing/router.go` `AcquirePlayerLock`、`router_lock_test.go`(新) |
| 四 | 合服集成用例第一句不是 `requireIT(t)`,AST 守卫 `TestIntegrationTestsCallRequireIT` 必然 FAIL;它与本文件的真 Redis 用例都不认 REQUIRE 开关,SKIP 会被当成通过 | merge_zone 用例补 `requireIT(t)`,DB 13/14 非空改走 `itMissing`;真 Redis 用例加 `requireRealRedisIT`(`DATA_SERVICE_IT_REQUIRE=1` 时 SKIP 判 FAIL) | `tools/merge_zone/blob_lock_integration_test.go`、`internal/logic/data_logic_integration_test.go` |

### 字段索引与保留名(2026-10-20:c35 业务源码加授权生成绑定,字段 Lua 验证通过)
- **结构**:`player:{id}:__fields`(HASH,与 `__version` / `__lock` / 字段键同 `{id}` slot),成员 `<字段名>` = 空串,保留成员 `__version` = 戳(索引在哪个版本上完整)。
- **三种状态与可信判据**:版本、索引都不存在 = 无数据;TYPE 为 hash 且 `GET(__version)` 存在且 `HGET(__fields,'__version')` 与之逐字节相等 = 可信;其余(存量老玩家、戳失效、有索引无版本)= 未索引。
- **三段 Lua 的职责**:
  - `saveFieldsScript`:所有可失败检查(版本键类型与格式 ≤15 位、锁 token、期望版本、索引类型、每个字段键必须是 none / string)都在第一条 SET 之前,字段键类型不对回 -2(InvalidRequest,零副作用);新玩家或索引可信时把本次字段名加进索引并把戳盖成新版本,否则不加名字、索引存在就 DEL。
  - `loadAllFieldsScript`(只读):无数据回空;可信时一段脚本读完字段集合、值与版本(同一瞬间,返回的版本可直接 CAS);未索引回版本,Go 侧退回 SCAN。
  - `deletePlayerFieldsScript`:校验 token;可信时按索引删,否则 Go 侧 SCAN 收候选后回退模式删;两种模式都只删 TYPE 为 string 的键。
- **存量玩家**:永远走 SCAN 回退(成本等于以前),读路径不「收养」—— 这族键没有生产写者,存量集合不再增长;收养要在读路径写元数据,还有「删除后重存版本回到同一值」的 ABA 缺陷。收敛靠指标 `data_service_player_fields_enum_total{op,path}`:`path="legacy_scan"` 长期为 0 后,再另起一批把回退改成 fail-closed。
- **保留名**:`__` 前缀、`save_barrier`、`asset_sql_commit`(键名取自 `shared/savebarrier`、`shared/assetop` 的规范键函数)。Load(指定字段)/ Save / SetField 回 InvalidRequest,GetField 回错误(server 折成 1,既有口径)。今后新增 `player:{%d}:` 花括号键必须同时登记(见 `shared-redis-split-by-purpose.md` 2026-10-19 增补)。
- **删除边界**:`KeysDeleted` = 实际删掉的字段 string 键数 + 版本键(索引不计);别的模块的 HASH 与保留名不删;只由带外半截写造成的「有字段、无版本」残留在全量读里不可见、删除时也不删;`__` 前缀的老字段不读、不删。
- **带外写者**:任何绕过 `saveFieldsScript` 直接改 `__version` 的工具,必须在同一个 MULTI 里 `DEL player:{id}:__fields`,或改走 `SavePlayerData`。否则旧戳可能恰好等于被重置的版本:索引被当成可信,带外写的字段从全量读与删除里消失,下一次存盘还会把这个不完整的索引固化(`saveFieldsScript` 的「不维护就删索引」只兜得住戳与版本不等的情形)。`cmd/debug_import` 的 player 模式(唯一的这类工具,写 `"1"`)已改成 `writeLegacyPlayerFields`:一个 MULTI 里先删索引、再写字段、最后重置版本(2026-10-19 第三批)。
- **版本格式**:`__version` 必须是规范十进制(无前导零);写侧另限当前版本 ≤15 位。真 Redis 的 INCR 拒绝 `"007"` 这类前导零串,miniredis 却接受,所以写脚本的守卫按规范十进制检查;读路径(`parseVersionValue`)同样拒绝非规范值,空串也算损坏(2026-10-22;版本键不存在才读成 0),但不设位数上限(见下一条)。
- **版本精度上限(2026-10-19,由 ≤18 位收紧为 ≤15 位)**:版本经 `tonumber` 与 INCR 进入 Lua 都是 double,只在 2^53 以内精确。旧守卫放行到 18 位时,2^53+1 与过期一格的 `expected_version=2^53` 会被舍入成同一个数,CAS 误放行并写入,返回的新版本也不准。≤15 位时当前版本 < 10^15、INCR 后 ≤ 10^15,都在 2^53 以内;`expected_version` 超过 2^53 时虽也被舍入,但舍入后仍 ≥ 2^53,不会与当前版本相等。超出上限的版本只可能是带外写坏的,写侧按损坏拒写(fail-closed),单玩家存盘次数到不了 10^15。上限只在写侧:读路径不经 Lua 数字,版本以字符串回到 Go、`ParseUint` 对整个 uint64 都精确,所以 `parseVersionValue` 不设位数上限;读到超限版本的调用方去 CAS 只会撞上写侧拒绝,不会被误放行。守卫正好落在 15 位最大值时,存盘照常、新版本是 10^15,此后再写按超限拒绝。回归用例:`TestSave_VersionPrecisionCap`(miniredis,2026-10-20 Codex 运行 PASS;旧守卫下红为既有记录,本次未重放)、`TestRealRedisSaveRefusalsHaveNoSideEffects` 的 `version_9007199254740993` / `version_1000000000000000` 与 `TestRealRedisStampMatchesVersionBytes` 的上界段(2026-10-20 真 Redis 7.2.16 / 8.10.2 实测 PASS,两轮命令均退出 0)。本次通过范围为 `c35e1133d` 的业务源码加经授权补生成的 Go 绑定,详见下一节。
- **滚动升级**:旧副本只 INCR、不维护戳,索引自动失效、读方退回 SCAN,不会漏读;旧副本的 `DeletePlayerData` 仍会连带删掉别家 HASH(旧缺陷),升级完成后消失。

### 字段 Lua 验证结果(2026-10-20)

- **通过范围**:负责人明确选择的 `c35e1133d3e41bfaa2aee9f42110ffeb2a1bc954` 业务源码与测试,在隔离副本中按同提交协议源补生成 `go/proto/common/database/rollback_database_table.pb.go` 后验证通过。799 个导出输入仅该生成文件相对提交有变化,业务源码、测试和原始 proto 未变;生成后到全部测试结束,799 个输入哈希零变化。
- **构建与单测**:`gofmt -l internal/logic internal/routing` 无输出;`go build ./...`、`go vet ./internal/logic/ ./internal/routing/`、`go vet -tags=integration ./internal/logic/` 均退出 0;`go test -count=1 ./internal/logic/ -v` 退出 0,105 个顶层测试全部 PASS、零 SKIP,含版本精度回归。

| 真 Redis 版本(单实例 / 三主) | 顶层用例 | 拒绝子测试 | SKIP | 命令退出码 |
|---|---|---|---|---|
| 7.2.16 / 7.2.16 | 7/7 PASS | 8/8 PASS | 0 | 0 |
| 8.10.2 / 8.10.2 | 7/7 PASS | 8/8 PASS | 0 | 0 |

- **覆盖与 Cluster 更正**:`TestRealRedis*` 覆盖 TYPE 的 `.ok`、缺失值占位(RESP2 / RESP3)、删除计数、规范版本守卫、版本戳字节与三主 Cluster 未声明键/SCAN 回退。两版 Cluster 探针都确认:无 shebang 的兼容模式 EVAL 允许同节点跨 slot 的未声明键,跨节点才报 `non local key`;`loadAllFieldsScript` 与 `routing.getHomeZoneAndMergeFenceScript` 的 2026-10-19 注释更正保留。
- **生成与证据**:仅在副本使用现有 protoc 35.1 / protoc-gen-go 1.36.10,沿用提交生成器的暂存与 `go_package=proto/{目录}` 规则补生成一个 Go 文件,未手改生成物,未写原工作区或客户端。日志、两轮版本、输入哈希、生成参数及容器清理见本机 `build/verification/data-service-field-lua-c35-generated-20261020-1792484087475/summary.json`,详细步骤见交接 §10。四个一次性容器均已移除。
- **结论边界**:这是上述授权派生副本的字段 Lua 线验证,不表示 `c35e1133d` 原样提交可编译,也不表示最新工作区或 D1 路由线已验证。此前共享输入变化、D1 接口未齐及原样提交缺绑定的失败记录保留在交接 §7–9;本轮未运行其他包单测、MySQL 集成、race、全服务启动、压测或全仓协议同步。


### 写门禁契约(2026-10-19;未编译、未测试,待验证)
- **顺序**:`Router.AcquirePlayerWriteLock` = 预检(`GetPlayerHomeZoneAndMergeFence`,home 与围栏同一段 Lua / 同一条 SQL)→ 按 home 取数据客户端 → 两把锁 → **数据锁 SETNX 成功之后**复核。复核看到围栏、home 变了或读失败,先释放两把锁再拒绝。Save / SetField / Delete 唯一的拿锁入口;读路径(Load / GetField / 账本读)不拦,继续用 `ClientForPlayer`。
- **为什么复核必须晚于数据锁**:提交脚本校验数据锁 token,merge_zone 拷 blob 前 `EXISTS` 的也是数据锁。复核晚于数据锁时,立围栏之后才拿到锁的写者复核必然看见围栏;立围栏之前拿到锁的写者,要么在锁有效期内提交(锁在,合服拷贝中止),要么锁过期后提交被 token 拒绝。复核若早于数据锁,写者可能在 merge_zone 查锁之后才拿到锁、在拷贝之后提交。
- **码**:围栏 / home 变化 → `ErrCodeZoneMergeInProgress`(22,规则拒绝,零写入,不进 FaultCodeSet);锁冲突 → 2;其余(Regions 下映射缺席、Redis 故障、回源失败、zone 未配集群)→ 1。回档里 22 不算「走到写那一步」,不触发写后帮会复查。指标 `data_service_player_lock_total{outcome="merge_fenced"}`、`save_player_data_total{outcome="merge_fenced"|"type_conflict"}`;日志 `[PlayerWriteLock]` / `[DataRoute]`。
- **拿锁失败的回滚(2026-10-22)**:`AcquirePlayerLock` 里任一把锁没拿到,都按 token 释放可能已落地的锁。原因是 SETNX 报错不等于没写入,回 false 也可能是客户端重试撞上自己。只删自己的,别人持有的锁不动。
- **dev 与 Regions**:围栏两种模式同口径(合服窗口内 src / dst 两个 zone 的写入一律拒绝)。dev 模式下映射缺席照常可写:数据位置不依赖映射,这类玩家也不可能在合服清单里(清单按 `player:zone` 收集,登记受同一道围栏约束),行为与引入门禁之前一致;Regions 下映射缺席照旧是故障。
- **代价**:每次写多两次 mapping Redis 往返(ReadRequiresReady=true 时是两次就绪读,缓存不可信时两次库点查,`route_read_total{path="home_fence"}` 会混入写门禁的读);复核耗时计入 3s 锁 TTL,慢到锁过期时提交被拒成 LockConflict,只损活性。db_first 下 SQL 回源读 `zone_merge_fence`,merge_zone 目前只写 Redis 围栏,沿用「第 2 批落地前禁止 merge_zone 写模式」的冻结前提。
- **部署**:merge_zone 的拷贝前查锁只有在所有 data_service 副本都含写门禁时才成立;Regions 下合服(含 unmerge)之前必须确认副本已全部升级。

### 仍未处理
- `cmd/debug_import` 的 player 模式仍然不持玩家锁、先写数据后登记映射、把版本重置为 `"1"`(调试工具,只写本地目标;字段索引问题已由 `writeLegacyPlayerFields` 解决)。与正在运行的 data_service 写入并发时可能互相覆盖,不要对线上进程正在写的玩家导入。

### 启用 Regions(去掉 DevRedis)之前必须先处理
- zone → 集群表只在启动时建一次、不热更新:改 zone 归属必须停写、搬完 `player:{id}:*`、所有副本同时换配置后再起,不能滚动改(见 `RegionConfig` 注释)。
- 跨集群合服必须给全 `-source-data-redis` / `-target-data-redis`,并用 copy 模式加 `-migrate-player-blobs`;工具内拷贝遇到没有 `player:{id}:*` 键的清单玩家会整步中止,真实 zone 跑不通,目前只能带外搬,带外搬必须在立围栏之后、确认每名玩家的数据锁不在(或至少等 `PlayerLockTTLSec`)再搬。合服拷过去的玩家在目标端没有字段索引,走 SCAN 回退(Cluster 上按 slot 只扫单节点);要更快再做索引重建。
- 合服前确认所有 data_service 副本都已含写门禁,旧副本不拦写。
- 续跑时运维 DEL 围栏到重新立起之间写进源区的数据,第 2 步已标完成时不会被重拷。
- 同前缀的 HASH 键(`save_barrier`、`asset_sql_commit`)不随 blob 拷贝。
- `RouteStore.Mode=db_first` 之后,merge_zone 改映射仍只写 Redis、不写 `player_route`,目前只靠文档化的冻结窗口约束,工具里没有守卫。
- unmerge 改回映射用的是 MGET + SET 而不是 CAS,目标集群里合服后写入的 blob 也不拷回。
- 契约测试里 DevRedis 同址那条用例会先变红,要为 Regions 另写一条同址契约。

### 核验后排除的担心
- 多 key 命令(MGET、存盘 / 删除 Lua)的键都带 `{id}` hash tag,同一 slot,不会 CROSSSLOT。
- go-redis v9.18.0 的 `Scan` 在 pattern 带 hash tag 时 `SetFirstKeyPos(3)`,按 slot 发往对应主节点,不会随机挑节点;merge_zone 注释里「会打到随机节点」只对它用的 v9.7.3 成立。
- 映射缺失直接报错,不会回落到某个默认 zone。
- 一次 RPC 只解析一次 client 并全程复用,读写不会落在两个集群。
- 客户端不能经 client_rpc_router 调 DataService(路由表 `ClientProtocol=false`,转发被拒)。

### 其余低优先级发现(未整改,记录备查)
- `AcquirePlayerLock` 的 SET NX 与 `saveFieldsScript` 在回包丢失时会被 Redis 客户端自动重发,可能回假的 LockConflict(锁残留到 TTL)或假的 VersionMismatch(写入其实成功)。
- `__version` 被 `DeletePlayerData` 删掉后从 0 重计,有 ABA;`expected_version=0` 跳过校验,首次存盘不受乐观锁保护。
- redis 模式下映射写入没有 WAITAOF,节点级崩溃可能丢掉合服最后一批映射改写,续跑时不复核。
- 回档里 SavePlayerData 回 LockConflict(2) 也是零写入,但仍记成 writeAttempted=true,可能误触发写后复查、误报 29(本批只修了 22)。
- `GetPlayerField` 的参数错误(保留名)被 server 折成 1,业务拒绝被多报成故障(既有口径)。

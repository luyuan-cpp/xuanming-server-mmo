# data_service 定位与边界 (2026-04-11)

## 定位
`go/data_service/` 是**跨区(cross-zone)运行时数据代理**，服务于玩家游戏过程中的跨区场景数据访问和运维工具。

## 核心职责
1. **跨区数据透明访问** — Scene Server 调 gRPC 统一接口，data_service 按 `player_id → home_zone_id → Redis` 路由，Scene 无需知道玩家归属哪个区
2. **Load/Save** — `LoadPlayerData`, `SavePlayerData`, `GetPlayerField`, `SetPlayerField`
3. **玩家区归属** — `RegisterPlayerZone`, `GetPlayerHomeZone`
4. **快照与回档** — `CreateSnapshot`, `DiffSnapshot`, `RollbackPlayer/Zone/All`
5. **GM/运维** — `BatchRecallItems`, `QueryTransactionLog`, `CreateEventSnapshot`

## 与 Login 的关系：无关
- Login 使用 Kafka 直连 db 服务读写玩家数据（`sync_loader.go` → Redis check → Kafka DBTask → BLPop）
- Login 只操作玩家所在区的 Redis，不需要跨区路由
- 在登录时间敏感路径上加一层 gRPC 到 data_service 会增加延迟，无收益
- **结论：Login 不接 data_service，两者独立**

## 谁用 data_service
| 调用方 | 场景 |
|--------|------|
| Scene Server (C++) | 跨区玩家在非本区 Scene 上游戏时，通过 data_service 透明读写 home zone Redis |
| GM/运维工具 | 快照、回档、批量回收、交易日志查询 |
| 未来跨区功能 | 跨区邮件、拍卖行等需要访问玩家 home zone 数据的系统 |

## 架构位置
```
Player → Gate → Scene Server (任意区)
                    │
                    │ gRPC (区无关的统一接口)
                    ▼
              data_service (Go, go-zero)
                    │
                    │ 按 player_id → home_zone_id 路由
                    ▼
              Redis (玩家 home zone)
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
| `player_snapshot` | `player_snapshot_topic_g1` → `player_snapshot` | 逐条、按 `snapshot_guid` 单语句 `INSERT ... SELECT ... WHERE NOT EXISTS`,落 `source=1` | 解不出 / `snapshot_id=0` / `player_id=0` / 超 MEDIUMBLOB:跳过并提交 | 同上 |

生产者是 C++ scene(全局单 topic、裸 proto、key=player_id)。启动时 `kafkautil.EnsureTopics`(transaction_log 6 分区、snapshot 3 分区、保留 30 天;分区数是不可变契约)。Kafka 不可达**不拖死** data_service:第一次尝试就在后台 goroutine 里(EnsureTopics 对半死不活的 broker 要走完 sarama 30s 拨号 + 元数据重试,同步做会把 gRPC 启动和 Load/Save 一起按住),失败只记日志、每 30s 重试。指标:`kafka_consumer_up{consumer}`、`kafka_consumer_messages_total{consumer,outcome}`;两条 `kafka_consumer_up` 序列在第一次尝试**之前**就按 0 预注册,所以"消费者从来没起来"这个状态告警得到(不预注册的话那个 child series 根本不存在,`== 0` 的告警永远不触发,监控上和"服务没部署"长得一样)。

关停顺序:`main` 先 cancel 消费者 ctx、有界等它们退出(8s,`kafkaShutdownGrace`),**之后**才 `svcCtx.Close()` 关连接池。反过来的话 txlog 的 `flushOnShutdown` 只会拿到一个已经关掉的池,那段"关停前尽力落最后一批"等于从来没写过。等超时就照常关池:未提交的 offset 会在下次启动时重放,`tx_id` 主键与 `snapshot_guid` 去重让重放幂等。

#### topic 名带代号(`Kafka.TopicGeneration`,默认 1)
有效名 = `<基名>_g<TopicGeneration>`,当前即 `transaction_log_topic_g1` / `player_snapshot_topic_g1`;**C++ 生产者的 topic 常量必须同名**(`cpp/libs/modules/transaction_log/transaction_log_system.h`、`cpp/libs/modules/snapshot/snapshot_system.h`)。

为什么连第一代也带后缀(与 go/db 的 `DbTaskTopicForGeneration` 在 `<=1` 时用裸名不同):broker 开着 `auto.create.topics.enable` 且 `num.partitions=1`(`deploy/docker-compose.yml` 的 `KAFKA_NUM_PARTITIONS=1`;k8s 清单没设,同样是默认 1),C++ 生产者一发消息就把**裸名字**的 topic 自动建成 1 分区。`EnsureTopics` 拿 6/3 的契约去比,只会永远返回 `partition contract mismatch`,两条消费者一条都起不来,唯一症状是每 30s 一条 Error 日志,而流水/快照全被保留期吃掉。带代号的名字与那两个自动建出来的裸 topic 互不相干。

分区数要改 = `TopicGeneration` +1(换一批新 topic,老 topic 排空后删),**绝不**原地扩分区:扩分区会重映射 `key=player_id` 的哈希,同一玩家的流水顺序就断了。部署侧应当预建这两个 topic 并给足分区(6 / 3),不要依赖 broker 自动创建 —— k8s 上 Kafka pod 一重启就会按 1 分区重建自动创建的 topic,下一次 data-service 重启就永久卡住。

### `player_snapshot.source` 列契约
- `source=0`:GM / data_service 自己写入。`snapshot_type` 是 data_service 的 `SnapshotType`,`data` 是 JSON 字段图 `{fields: map[string][]byte}`,`snapshot_guid=0`。
- `source=1`:C++ scene 经 Kafka 落库。`snapshot_type` 存 C++ `SnapshotTrigger` **原值**,`data` 存收到的整条 `PlayerSnapshotEntry` 序列化字节(含两个 player_database blob),`snapshot_guid` = C++ `snapshot_id`,`operator="scene-node"`,`zone_id`/`created_at` 取自载荷。
- **现有 GM 读路径一律只看 `source=0`**(`GetSnapshotByID` / `GetLatestSnapshotBefore` / `ListSnapshotsMeta` / `GetSnapshotPlayerIDsByZone`),直到回滚路径学会解 proto blob;`ListSceneSnapshotsByPlayer` 是 `source=1` 的专用读路径(尚未接 RPC)。`DeleteOldSnapshots` 两种 source 都删。

### `snapshot_guid` 去重规则
`snapshot_guid` 是普通 INDEX **不是** UNIQUE(GM 行恒为 0,proto2mysql 做不出可空唯一列)。去重由 `InsertSnapshotIfGuidAbsent` 的**一条**语句完成:

```sql
INSERT INTO player_snapshot (...) SELECT ?,... FROM DUAL
 WHERE NOT EXISTS (SELECT 1 FROM player_snapshot AS existing WHERE existing.snapshot_guid = ?)
```

存在性判断与插入在同一条语句、同一个隐式事务里,InnoDB 会在 `snapshot_guid` 索引上对那个不存在的值加间隙锁,两个并发实例写同一个 guid 只会有一个成功(另一个 0 行受影响,按"重复"计数)。以前是"先 SELECT 再 INSERT",两次往返之间没有任何锁:滚动更新窗口里新旧两个 pod 同时在线,足以把同一条快照写成两行 `source=1`。
**deploy 侧仍应把 data-service 的 Deployment 策略设成 `Recreate`**(deploy/ 由另一位负责):UNIQUE 键用不了,DB 侧的保证止步于"同一条语句 + 间隙锁",真正的单实例语义还得靠部署策略。`guid=0` 走这条入口直接报错。`transaction_log` 不需要这套:`tx_id` 是主键,重放天然被 `INSERT IGNORE` 吃掉。

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
- **行由迁移显式创建**(`IdSegment.BootstrapTags`,没配 = `player, guild, item, txlog, snapshot`):`-migrate` 与 AutoMigrate 都在四张表就位之后对每个 tag `INSERT IGNORE ... (max_id=1, step=100, version=0)`,幂等,**绝不降低**已有行的 max_id;每行创建 / 已存在各打一条 Info。清单里出现非法 tag(不匹配 `^[a-z0-9_]{1,64}$`)整次迁移失败。新增一种永久身份 = 加一个 tag。
- **地板校验**(`store/schema.go: idSegmentFloorSources` → `raiseIdSegmentFloor`):迁移接着对 `player` / `guild` 两行做 `max_id ← MAX(主键 < 2^55) + 1`(只在 `MAX ≥ max_id` 时;`< 2^55` 排除存量 snowflake 号;UPDATE 带 `max_id < ?` 守卫,与并发发号互斥安全)。消费表必须与 `id_segment` **同库**才查得到(`INFORMATION_SCHEMA.COLUMNS` 探测,不在就 Info 跳过);抬升时打 Error `id_segment counter was behind the consuming table — backup restore?`,出现即意味着库曾被重置,按下面的手册排查已被覆盖的行。`item` / `txlog` / `snapshot` **做不了**这种校验:item guid 在 zone 库的玩家 blob 里;tx_id / snapshot_id 所在的 `transaction_log` / `player_snapshot` 与 `id_segment` 同库、会被一起恢复到旧水位,而真正"已发出"的集合在 Kafka 积压与 C++ 进程手里的段里(重放的旧流水会与重发的新号撞 `tx_id` 主键,`INSERT IGNORE` 静默丢掉**新**的流水)。

**运维手册(恢复全局库 = ID 安全事件,不是普通库恢复)**:恢复 `id_segment` 所在的全局库之前,必须先核对每个 tag 的 `max_id ≥ 消费侧最大号 + 1` —— `player` 看各 zone 库 `MAX(player_database.player_id) WHERE player_id < 2^55`,`guild` 看 `MAX(guild.guild_id)`,`item` 看各 zone 库 blob 里 bag 的最大 guid,`txlog` / `snapshot` 看两个 Kafka topic 里的最大 `tx_id` / `snapshot_id`;不够就手工 `UPDATE id_segment SET max_id = <最大号 + 1>, version = version + 1` 抬上去,再启动 data_service / 跑 `-migrate`。迁移里的自动地板校验只是同库能查到时的兜底,"跳过"不代表安全。回归用例:`store/id_segment_integration_test.go`(缺行拒绝零写入 / bootstrap 幂等不降位 / 地板抬升与 snowflake 号排除 / dev 补种)、`logic/id_segment_integration_test.go`(错误码定性)。

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
  BootstrapTags: [player, guild, item, txlog, snapshot]   # 没配 = 这五种;迁移 INSERT IGNORE 预建,绝不降位
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
- C++ 生产者的 topic 常量还是裸名(`transaction_log_topic` / `player_snapshot_topic`),需要改成 `transaction_log_topic_g1` / `player_snapshot_topic_g1` 才能与消费者对上;在那之前 Go 侧消费的是**空的**新 topic(不会卡住,只是没有数据)。同样地,`tools/scripts/k8s_deploy.ps1` 的 data-service ConfigMap 还没写 `Kafka.TopicGeneration`(缺省 1,与服务 yaml 一致,所以名字仍对得上;要改代号必须两边一起改)。
- 本地 docker MySQL 上不存在 `testdb`,`appuser` 也无权限;按当前 yaml 启动 data_service 会迁移失败、三个 store 置 nil、消费者不启动(非致命)。本地联调应把 `SnapshotMySQL.DBName` 指到有权限的库(如 `mmorpg`)。

### 已澄清(不再是缺口)
- ~~表 proto `transaction_log` 没有 `zone_id` 字段,消费者收到的 `zone_id` 被丢弃~~ —— **已过时**:`rollback_database_table.proto` 的 `transaction_log` 已有 `uint32 zone_id = 15`,`store.txLogHasZoneColumn` 因此为 true,`InsertBatchIgnore` 的列序里带着 `zone_id`,`NewTransactionLogStore` 那条 Error 不会打。回归用例 `TestTransactionLogStore_TimestampSecAndZoneLand` 直接断言这一列真的落库。

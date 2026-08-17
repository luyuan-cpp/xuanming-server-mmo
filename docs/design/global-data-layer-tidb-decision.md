# 全区全服数据层 + TiDB 架构决策

> **文档状态**: v1 — 2026-08-15
> **范围**: 玩家数据层从"每 zone 独立 MySQL 库(`zone_{N}_db`)"演进为"单一 TiDB 集群上的全局数据层(按 `player_id` 组织)";裁决 proto2mysql 升级路径、TiDB 建表方言、跨区/合服语义变化。
> **关联文档**: [db_zone_isolation.md](./db_zone_isolation.md)(被本文修订)、[cross_server_architecture_principle.md](./cross_server_architecture_principle.md)(存储前提被修订)、[server_merge_design.md](./server_merge_design.md)、[data_service_role_and_scope.md](./data_service_role_and_scope.md)、[zone_data_rollback.md](./zone_data_rollback.md)、[db-task-kafka-partition-contract.md](./db-task-kafka-partition-contract.md)、[cross-zone-readiness-audit.md](./cross-zone-readiness-audit.md)

---

## 结论(先读这节)

1. **目标形态 = 全区全服**。物理 zone(≈10 gate + 20 scene 的集群)只是**部署单位**;玩家归属用 **home_zone(逻辑区)** 表达(现状已存在: data_service 的 Redis `player:zone:{player_id}` 映射)。玩家可自由跨区;跨区动作 = **客户端重连目标区 gate**(复用现有 `handleCrossZoneRedirect` 的 HMAC token redirect 机制),**数据不搬家**。同 zone 内 gate↔scene 保持全连接 mesh(10×20=200 条内网连接,量级无压力)。
2. **玩家数据层收敛为一个 TiDB 集群**,表以 `player_id`(Snowflake uint64)为主键。"按 player_id 全局分片"由 TiDB region 自动切分承担,应用层不做分片路由。**合服 = `RemapHomeZoneForMerge` 改逻辑归属 + 业务数据合并(榜单/市场),零玩家主数据迁移**。
3. **proto2mysql 直接接 TiDB: 可行**。go-sql-driver 官方 Full 支持;库生成的 DML(`INSERT`/`INSERT ... ON DUPLICATE KEY UPDATE`/`REPLACE INTO`/`SELECT ... FOR UPDATE`)、`STRICT_TRANS_TABLES` 会话参数、`CREATE DATABASE IF NOT EXISTS`、多子句 `ALTER TABLE`(v6.2+)在 TiDB v8.5 LTS 上全部兼容。但**必须落三件事**,缺一不可:
   - **建表方言**: snowflake 主键时间戳在高位、单调递增,在 TiDB 默认聚簇表下是官方点名的写热点场景。玩家表必须建成 `PRIMARY KEY ... /*T![clustered_index] NONCLUSTERED */` + `/*T! SHARD_ROW_ID_BITS=N PRE_SPLIT_REGIONS=N */`(TiDB 扩展注释语法,MySQL 视为注释忽略,**同一份 DDL 双方言可用**)。proto2mysql 需新增对应表选项(见 §D6)。
   - **集群配置**: TiDB `txn-entry-size-limit` 默认 6MB,而玩家存档列是 MEDIUMBLOB(上限 16MB)——**默认配置下大存档写入直接报 `entry too large`**。上线配置须调到 ≥33554432(32MB,留一倍余量),并压测验证 TiKV `raft-entry-max-size`(默认 8MB)是否需同步调整。
   - **服务器升级 proto2mysql 新版**: go/db 现钉 `v0.0.18`,是旧世系;GitHub main 已重写(`NewPbMysqlDB`→`NewDB`、raw binary 存储、descriptor schema sync)。**迁移时必须逐表显式锁定表名**(新版默认表名 = proto 全名含点号,不锁表名 schema sync 会建出一套新表,旧表数据"消失")。
4. **分阶段实施,不一步到位**:
   - **Phase 1(纯搬迁)**: 单 TiDB 集群承载现有 `zone_{N}_db` 逻辑库。库名派生、Kafka topic、write-behind 管线、per-key LWW 不变量**全部不动**,只换存储后端 + 建表方言。用 L1-L4 一致性测试和压测基线验收。
   - **Phase 2(全局化)**: 玩家表收敛到单一全局库,DBTask 生产端从"按本 zone 选 topic"改为"按 home_zone 选 topic";跨区带状态游玩 = redirect 重连 + 全局数据层直读,**废弃 `player_migrate` 的数据搬运职责**(冻结/单写栅栏语义保留)。
   - 每阶段是独立可回退的发布单元。

## 1. 背景与问题

三个原本独立的问题在"全区全服"目标下收敛为同一个数据层决策:

| 问题 | 现状 | 症结 |
|---|---|---|
| 跨区游玩 | `enterscenelogic.go` 对带状态跨区 fail-closed(`ErrUnsafeCrossNodeHandoff`);C++ `player_migrate` 协议只迁 7 个 ECS 组件,bag/quest/mail 丢失([cross-zone-readiness-audit.md](./cross-zone-readiness-audit.md)) | 数据按 zone 分库,跨区必须"搬数据",搬不全 |
| 合服 | [server_merge_design.md](./server_merge_design.md): 跨库搬数据 + ID/名字冲突处理 | 数据物理归属 = 逻辑归属,合服被迫做物理迁移 |
| 存盘路由 | scene 写本 zone Redis + 本 zone topic(`GetDbTaskTopic(zone)`) | 玩家若在非 home zone 游玩,存盘会落错库 |

根因相同: **物理 zone 和逻辑归属没有解耦**。解法: 数据全局化(TiDB 单集群、player_id 主键)+ 归属字段化(home_zone)。gate↔scene 连接数疑虑经核算不成立(区内 mesh 是 O(gate×scene)=200/zone,全局跨区靠客户端重连,连接数随 zone 数线性增长)。

## 2. 目标拓扑

```
                 ┌────────────── zone A(部署单位) ──────────────┐
 client ──TCP──▶ │ gate ×10 ──mesh──▶ scene ×20(每进程多副本)   │
      ▲          └───────┬──────────────────┬───────────────────┘
      │ 跨区=redirect:   │ Redis(zone A 数据面,不变)             │
      │ 断开→重连 zone B │ Kafka db_task_*(不变)                 │
      ▼          ┌───────▼──────────────────▼───────────────────┐
 zone B(同构)    │ go/db consumer(每 zone 一实例,不变)           │
                 └───────────────────┬───────────────────────────┘
                                     ▼
                    ┌────────────────────────────────┐
                    │  TiDB 集群(全局唯一数据层)      │
                    │  Phase 1: zone_{N}_db 逻辑库    │
                    │  Phase 2: 全局玩家表            │
                    └────────────────────────────────┘
      全局服务(跨区不随玩家走): data_service(home_zone 路由)、friend、guild、chat
```

## 3. 决策明细

### D1: 逻辑区(home_zone)与物理 zone 解耦

- home_zone 是玩家数据上的归属标签,决定榜单/频道/市场分组;物理 zone 只决定"这批进程部署在哪"。
- 权威映射沿用 data_service: Redis `player:zone:{player_id}`(`router.go`),合服走 `RemapHomeZoneForMerge`。Phase 2 需给映射补 TiDB 持久化兜底(Redis 丢失可重建)。
- 建号三不变量(合服的地基,现状已满足,**不许回退**): ① player_id 全局唯一(Snowflake + etcd worker id + guard 水位,绝非按区自增);② 名字唯一性作用域在建号时定死(现状全局唯一,保持);③ 业务表以 home_zone 字段区分归属,不以"在哪个库"区分(Phase 2 落地)。

### D2: 跨区 = 客户端重连,不做数据搬运

- 复用既有 redirect 机制(`handleCrossZoneRedirect` → `AssignGateForZone` 签 5 分钟 HMAC token → Kafka 推 `RedirectToGateEvent` → 客户端断开重连目标区 gate)。
- Phase 2 解除 `ErrUnsafeCrossNodeHandoff` 的前提是: 目标区 scene 从**全局数据层**加载完整玩家数据(和本区登录同一条加载链),而不是靠 `player_migrate` 把 PlayerAllData 打包过去。7 组件 KNOWN GAP 因此整体消失——不需要"把 bag/quest/mail 补进迁移协议"这条路了。
- `player_migrate`/`CrossZoneReaper` 保留的职责只剩**单写栅栏**: 源区冻结+存盘+释放所有权,目标区确认后再加载。任何时刻只有一个 scene 进程持有玩家状态,顺序必须是"源存盘落地 → 释放 → 目标加载",违反即串档。

### D3: TiDB 建表方言(写热点是第一坑)

snowflake 主键(时间戳高 32 位,单节点严格单调)+ TiDB 默认聚簇表(`tidb_enable_clustered_index=ON`)= 官方点名的写热点场景。且两个常规工具都不直接适用: `AUTO_RANDOM` 只对 TiDB 自动赋值的主键生效,`SHARD_ROW_ID_BITS` 不支持聚簇表。**官方可行路径**(troubleshoot-hot-spot-issues / high-concurrency-best-practices):

```sql
CREATE TABLE IF NOT EXISTS `player_database` (
  `player_id` bigint unsigned NOT NULL COMMENT 'pb:1',
  ...,
  PRIMARY KEY (`player_id`) /*T![clustered_index] NONCLUSTERED */
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */;
```

- 行数据按打散的 `_tidb_rowid` 存储,主键退化为二级唯一索引(代价: 主键点查多一次回表,压测验收)。
- `/*T!...*/` 是 TiDB 扩展注释语法,MySQL 按普通注释忽略——**同一份 DDL 在开发 MySQL 和生产 TiDB 上都能跑**,不需要维护两套建表逻辑。
- shard bits 取 `log2(TiKV 节点数)` 数量级,初始集群 3 TiKV 取 4 已足够。
- 自增主键表(`player_snapshot`/`rollback_audit_log`): 同样 `NONCLUSTERED` + shard;TiDB AUTO_INCREMENT 多节点按批缓存分配(默认 3 万/批),全局唯一但**非连续、重启跳号**——两表均无连续性依赖,可接受;若未来有依赖,建表加 `AUTO_ID_CACHE=1`(v6.4+ 集中分配)。
- 落地方式: proto2mysql 新增 TiDB 表选项(§D6),不在业务代码里手写 DDL。

### D4: 集群配置硬要求

| 配置项 | 默认 | 要求 | 原因 |
|---|---|---|---|
| TiDB `performance.txn-entry-size-limit` | 6MB | **≥32MB** | 单行 KV 上限;MEDIUMBLOB 存档 16MB,默认必炸 |
| TiDB `performance.txn-total-size-limit` | 100MB | 默认可,批量工具需评估 | 全服离线迁移/批量导入分批提交 |
| TiKV `raft-entry-max-size` | 8MB | 压测验证,预计 ≥32MB | 大行单条 raft entry 超限风险 |
| 版本 | — | **v8.5 LTS**(9.0 仅 beta) | 当前推荐 LTS |

### D5: write-behind 管线哪些不动(Phase 1 全部不动)

以下不变量与 TiDB 替换**正交,明确不动**:

- `db_task_zone_{N}` topic 命名、**partition=10 不可变契约**([db-task-kafka-partition-contract.md](./db-task-kafka-partition-contract.md)): 换存储后端不是动 topic/partition 的理由;若 Phase 2 改 topic 路由,必须走"停写 drain + 新 TopicGeneration"流程。
- per-key 单调 last-write-wins 不变量与 L1-L4 验证体系([data-consistency-stress-testing.md](./data-consistency-stress-testing.md)): **这就是 TiDB 迁移的验收标准**,chaos_test 必须重跑。
- Redis 权威读缓存 + applied cursor + retry/dead 队列、SubShard 并行、写合并: 全不动。
- `sql_mode=STRICT_TRANS_TABLES` 连接参数: TiDB 完全支持且本就在其默认 sql_mode 里,保留现有代码不变(防 blob 静默截断的语义在 TiDB 下同样成立)。

### D6: proto2mysql 升级与 TiDB 方言支持(库是我们自己的)

服务器从 v0.0.18(旧世系)升到重写后的 main,业务侧改动点:

| 项 | 旧 | 新 | 风险 |
|---|---|---|---|
| 构造 | `NewPbMysqlDB()` / `*PbMysqlDB` | `NewDB()` / `*DB` | 纯改名 |
| `OpenDB(db, dbname)` | 同签名保留 | 不改 | — |
| `RegisterTable` | 手工传选项 | 自动读 proto option,代码选项优先级更高 | 兼容 |
| 查询 API | `FindOneByWhereClause` 等 | 保留兼容别名 | — |
| **默认表名** | 消息名 | **proto 全名(含点号)** | **高危**: 不逐表锁表名(proto option `table_name` 或 `WithTableName`),schema sync 会建新表,旧数据在业务视角消失 |

TiDB 方言在 proto2mysql 侧新增表选项(生成 §D3 的 DDL 形态):

- `WithTiDBNonclusteredPK()` / proto option → 主键子句追加 `/*T![clustered_index] NONCLUSTERED */`
- `WithTiDBShardRowIDBits(n)`、`WithTiDBPreSplitRegions(n)` / proto option → COMMENT 前追加 `/*T! ... */`(带 fail-safe: 有主键未声明 NONCLUSTERED 时忽略 shard 并告警;preSplit > shard 时收敛)
- `WithTiDBAutoIDCacheOne()` / proto option → `/*T![auto_id_cache] AUTO_ID_CACHE=1 */`
- 全部走注释语法,MySQL 双方言安全;选项输出顺序对齐 TiDB `SHOW CREATE TABLE` 规范导出,便于与线上表结构 diff;schema sync 的多子句 `ALTER TABLE` 保持单条(TiDB v6.2+ 原子支持,但"同语句引用新列"等写法禁止——库现有生成逻辑无此模式)。
- 选项读取带 unknown fields 兜底: 使用方的 pbopt 生成代码旧于新选项号时,扩展会落进 options 的 unknown fields,库按 wire 格式解出而不是静默丢弃;`pbopt/proto2mysql_option.pb.go` 仍应尽快用 protoc 重新生成。

### D7: 合服语义(Phase 2 后)

- 玩家主数据: **零迁移**(本就在全局层按 player_id 放着)。
- 逻辑归属: `RemapHomeZoneForMerge` 批量改映射;榜单/市场/公会合并是纯业务操作。
- 物理层面: 两个逻辑区指向同一套物理 zone 集群,旧集群退役(scene 从全局层加载数据,集群近乎无状态)。
- topic 路由随 home_zone 改变: 合服前必须 **drain 旧 topic**(合服本就停服操作,窗口天然存在);applied cursor 按 topic 隔离,新 topic 冷启动无冲突。
- [server_merge_design.md](./server_merge_design.md) 的跨库搬数据工具(`tools/merge_zone`)在 Phase 2 后按此收缩职责。

### D8: 回档与备份(诚实记录退化项)

- 灾难恢复级从 MySQL PITR 换成 **TiDB BR + PiTR**;[zone_data_rollback.md](./zone_data_rollback.md) 的七步脚本和 [mysql-backup-pitr-runbook.md](../ops/mysql-backup-pitr-runbook.md) 必须重写(**本决策落地前不许迁生产**)。
- **明确的退化**: Phase 2 单一全局库后,"只回档一个 zone"不再能靠"只恢复 zone_{N}_db"实现;zone 粒度回档改由**应用级**承担(`player_snapshot` + `RollbackZone` 按 home_zone 过滤)——而应用级回档现状是 fail-closed 未通电(缺离线 epoch 栅栏)。**因此 Phase 2 的前置条件包含: 应用级回档先通过验收**。Phase 1 无此退化(逻辑库仍按 zone 分,BR 可按库过滤恢复)。

### D9: 全局库(friend/guild/gateway zone_config)的归属

- friend/guild 本就是"全局单 MySQL 直写事务"先例([friend-persistence-architecture.md](./friend-persistence-architecture.md)),Phase 1 一并迁 TiDB(低频、直写、`BeginTx` 悲观事务,TiDB 兼容),独立验证,失败可单独回退留 MySQL。
- 迁移时顺带收敛口径: friend/guild/transaction_log 的 DSN 补 `STRICT_TRANS_TABLES`(现状缺失,与 go/db 口径不一);root 明文凭证问题([db-service-root-credentials.md](./db-service-root-credentials.md))在 TiDB 用户体系下重议,至少改环境变量注入(login 已有先例)。

## 4. 风险清单

| # | 风险 | 等级 | 缓解 |
|---|---|---|---|
| 1 | snowflake 主键写热点(默认聚簇表) | P0 | §D3 NONCLUSTERED+SHARD,压测看 region 热点面板 |
| 2 | 单行 6MB 默认上限,16MB 存档写失败 | P0 | §D4 配置;上线前用最大存档样本做写入冒烟 |
| 3 | 新版库默认表名变更 → 数据"消失" | P0 | 逐表锁表名;verifier 工具加表名断言 |
| 4 | 表名/DDL 漂移导致 schema sync 建错表 | P1 | 上线前 `GetCreateTableSQL` dump 与线上 `SHOW CREATE TABLE` diff |
| 5 | AUTO_INCREMENT 非连续/跳号 | P2 | 两张自增表无连续性依赖;有则 `AUTO_ID_CACHE=1` |
| 6 | go-sql-driver `multiStatements` 多结果集在 TiDB 的行为差异 | P1 | 库测试套在 TiDB 实例上全量跑一遍(验收项) |
| 7 | 主键点查多一次回表的延迟回归 | P1 | 压测对比基线(`stress_summarize.ps1`,dataloader/DB task 子阶段) |
| 8 | PITR 真空期(runbook 未改就迁移) | P0 | D8 前置条件卡死发布 |
| 9 | 借迁移之机误动 topic/partition | P0 | D5 明令禁止;code review 检查项 |
| 10 | 死信队列无消费者(既有问题) | P2 | 与本决策正交,单独立项,不因换 TiDB 消失 |

## 5. 验收标准(Phase 1)

1. proto2mysql 测试套在真实 TiDB v8.5 实例上全绿(含 16MB blob 写读、`ON DUPLICATE KEY`、多子句 ALTER、FOR UPDATE)。
2. [data-consistency-stress-testing.md](./data-consistency-stress-testing.md) 的 L1-L4 + chaos_test 全绿。
3. 压测对比: 以最近一轮 stress 基线(`prev-summary.txt`)为参照,DB task 子阶段延迟无回归、TiDB region 无持续热点。
4. `SHOW CREATE TABLE` 与 `GetCreateTableSQL` dump 全表 diff 为空(含表名)。

## 6. 实施清单

**Phase 1(按依赖顺序)**:

1. proto2mysql: TiDB 方言表选项 + 单测(库仓库,`main` 分支) — **已完成**(commit `2aca007` 已推送 main;`go test` 待 Codex)
2. proto2mysql: 打 tag(旧世系 v0.0.18 已断,新版从 v0.1.0 起) — **已完成**(Codex 测试绿后,`v0.1.0` 已打在 `2aca007` 并推送远端。注意:仓库实际已迁至 `github.com/luyuan-cpp/proto2mysql`,module 路径仍声明旧名 `luyuancpp`,靠 GitHub 重定向工作——一旦旧用户名被抢注,go get 链路即断,建议后续统一 module 路径)
3. go/db: 升级依赖 + `NewDB` 迁移 + 逐表 `table_name` 锁定 + 玩家表加 TiDB 方言 option(proto 侧) — **代码已落,待 Codex 编译**(2026-08-15 续二,详见 PROGRESS.md):`NewDB` 迁移 + 启动期表名守卫(`assertTableNameLocked`,风险 #3 兜底);表名锁定由既有 `OptionTableName`(与新版选项同号 500001,库按字段号读取)天然满足,守卫防回归;proto 侧 5 表加 TiDB 选项(player_database / player_database_1 / player_centre_database / player_snapshot / rollback_audit_log);另按 §D3 同理覆盖 data_service 裸 DDL 3 表(player_snapshot / rollback_audit_log / **transaction_log**——tx_id 雪花主键、全服最高频追加写,虽未在 §D3 点名但同属其处方范围)
4. deploy: TiDB 集群清单(docker-compose dev / k8s prod)+ §D4 配置;`CREATE DATABASE` 权限策略重议 — **dev 已落**(`deploy/docker-compose.tidb.yml` + `deploy/tidb-config/`,与 MySQL 并存可回退);k8s prod 清单与权限策略未做
5. 迁移工具: 开发环境直接重建;有存量数据的环境用 Dumpling + Lightning(逻辑库对逻辑库)
6. 验收: §5 全项
7. 文档: 回档 runbook TiDB 版(§D8)

**明确未做(本次)**: Phase 2 全部(全局表收敛、home_zone 字段落表、topic 按 home_zone 路由、解除跨区 fail-closed)、应用级回档通电、死信队列消费者。

## 7. 连带文档更新

- [x] 本文创建
- [x] ARCH.md §10 索引 + §11 决策表追加
- [x] CLAUDE.md §1 基础设施行
- [x] db_zone_isolation.md / cross_server_architecture_principle.md / mmo_cross_server_architecture.md / server_merge_design.md / zone_data_rollback.md / db-service-root-credentials.md 顶部修订标注
- [x] PROGRESS.md 当日条目
- [ ] docs/ops/mysql-backup-pitr-runbook.md 的 TiDB 版(Phase 1 第 7 步,迁生产前必须完成)
- [ ] data_service_role_and_scope.md(Phase 2 动 home_zone 持久化时再改)

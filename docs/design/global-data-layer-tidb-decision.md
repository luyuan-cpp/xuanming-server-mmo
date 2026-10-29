# 全区全服数据层 + TiDB 架构决策

> **文档状态**: v1 — 2026-08-15
> **范围**: 玩家数据层从"每 zone 独立 MySQL 库(`zone_{N}_db`)"演进为"单一 TiDB 集群上的全局数据层(按 `player_id` 组织)";裁决 proto2mysql 升级路径、TiDB 建表方言、跨区/合服语义变化。
> **关联文档**: [db_zone_isolation.md](./db_zone_isolation.md)(被本文修订)、[cross_server_architecture_principle.md](./cross_server_architecture_principle.md)(存储前提被修订)、[server_merge_design.md](./server_merge_design.md)、[data_service_role_and_scope.md](./data_service_role_and_scope.md)、[zone_data_rollback.md](./zone_data_rollback.md)、[db-task-kafka-partition-contract.md](./db-task-kafka-partition-contract.md)、[cross-zone-readiness-audit.md](./cross-zone-readiness-audit.md)

> **⚠️ 状态说明(2026-09-20):本文关于 `player_migrate` / `CrossZoneReaper` / `ErrUnsafeCrossNodeHandoff` 的表述是 2026-08-15 的现状,已被 [cross-zone-scene-travel.md](./cross-zone-scene-travel.md) 的落码改变;TiDB / proto2mysql / 分阶段的决策本身不受影响。** 受影响的是:结论 4 Phase 2 的「冻结/单写栅栏语义保留」、§1 表「跨区游玩」行的现状列、D2 的后两条。
> - `player_migrate` 搬数据链与 `CrossZoneReaper` **没有「保留职责」,而是在跨 zone 传送阶段 3(2026-09-18)整条删除**(该文 §11.3)。留下来的只有 `PlayerFrozenComp` 这个冻结标记和业务拦写闸。
> - **单写栅栏**现在由 per-player `owner_epoch` + `player:{id}:handoff` 落盘标记承担(该文 CZ-4 / §6 不变量 1):源 scene 冻结 → 存盘落地 → 写标记,`scene_manager.EnterScene` 校验标记属于当前 epoch 才放行并铸造新 epoch。D2 要求的顺序不变量「源存盘落地 → 释放 → 目标加载」不变,只是执行者换了。
> - `ErrUnsafeCrossNodeHandoff`(14)已是历史码、不再发出;同类拒绝改为可重试的 `ErrHandoffPending`(18)。带状态跨区不必等 Phase 2:存盘已按 home_zone 选 topic(CZ-2,即本文 Phase 2「DBTask 按 home_zone 选 topic」的先行一步),目标 zone 从共享 Redis 直接加载。
> - 上述代码已进 main 但**尚未编译、未跑测试**。

> **⚠️ 状态说明(2026-09-28):Phase 2 的落地方式由 [player-storage-placement.md](./player-storage-placement.md) 修订**。TiDB / proto2mysql / 建表方言 / D5「write-behind 管线不动」的决策不变,本文正文不重写。受影响的是:
> - **结论 4 Phase 2「玩家表收敛到单一全局库」**:改为**按落点逐玩家迁移**。
>   - 把「玩家属于哪个区」(home_zone)与「主数据在哪个库」(落点记录 `player:placement:{id}`)拆开,go/db 按当前记录选库。
>   - 全局库是一种非 zone 落点库(默认 `player_store_1000000_db`)。新号可直接钉到全局库,存量按 zone 冻结式搬库,任何时刻每个玩家只有一个真源,可逐玩家回退;旧 zone 库无人指向后退役。
>   - D7 的「合服零玩家主数据迁移」由合服 pin 模式提前兑现。
>   - 生产切换仍受 D8(应用级回档)卡住,D1 的映射持久化兜底表仍是遗留项。
> - **§6「明确未做」里的「topic 按 home_zone 路由」已由 [cross-zone-scene-travel.md](./cross-zone-scene-travel.md) CZ-2 落码**:C++ 存盘按 `PlayerHomeZoneComp` 选 `db_task_zone_{home}`。2026-09-28 起 topic 世代号也进了 C++ 配置(`DbTaskTopicGeneration`)。
> - **proto2mysql 核实结论**(2026-09-28,go/db 多库落码时):生成的 SQL 都是裸表名,库由连接池 DSN 的默认库决定。所以多库必须一库一池,不能靠 `USE` 切换。
> - 上述代码同样**未编译、未跑测试**。(2026-10-12 更正:落码时如此;2026-09-29 已按用户指示跑过一轮,`be6b4ab1d` 之后的 HEAD 待复验,见下一条状态说明。)

> **⚠️ 状态说明(2026-10-12):Phase 1、Phase 2 都还没上线,现网玩家数据全部在 MySQL。**
> - **数据存在哪**:逻辑归属 `player:zone:{id}` 与落点记录 `player:placement:{id}` 都在 data_service 的 mapping Redis(DB 0),没有 MySQL / TiDB 兜底表(D1 未做)。落点记录只选库名(`zone_{N}_db` / `player_store_{N}_db`),库所在的机器由各 go/db 的 `Database.Hosts` 决定,所以「上 TiDB」是集群级切换,不能按玩家切(详见 [player-storage-placement.md](./player-storage-placement.md) §0 第 7 条)。
> - **Phase 1**:TiDB 只有 dev 的 `deploy/docker-compose.tidb.yml`;K8s 是单个 `mysql:8.0`,§6 第 4 条的 K8s prod 清单与 §7 的 TiDB 版 PITR runbook 都没做;「Phase 1 是否继续」仍待拍板(handoff-backlog D-02)。
> - **Phase 2**:落点代码在 main(`4493b3c9f`),但开关全关(`PinOnCreate=false`、`Required=false`),`player_store_1000000_db` 没有在任何部署里建过,存量没搬。仍卡在 D8(`RollbackFence` 生产为 nil)与 D1(映射兜底表);真实集群演练、压测、落点告警规则也都没做。
> - **TiDB 兼容遗留**(迁库前必须处理):`player_snapshot` 的 `snapshot_guid_nz` 唯一键在 TiDB 上建不出(`go/data_service/internal/store/schema.go`,当前 fail-closed);friend 推荐查询的 `INL_JOIN` 提示已实测可用但未加([friend-handoff-20260920.md](../handoff/friend-handoff-20260920.md) §9.5 第 12 条);data_service 迁移互斥的 `GET_LOCK` 与 `player_name` 改主键都没在 TiDB 上实测。
> - **验证**:落点代码 2026-09-29 跑过一轮(Claude 按用户指示执行);之后 `be6b4ab1d` 改了 go/db 重试队列与 `tools/merge_zone`,当前 HEAD 的复验清单见 [placement-reverify-checklist-20261012.md](../handoff/placement-reverify-checklist-20261012.md)。

> **⚠️ 状态说明(2026-10-24):玩家数据不再用 Redis 存储,Redis 的其他用途照旧保留**,见本文 §9。
> - 只定了方向,存盘与加载的新路径没有设计、没有落码;现行代码仍按 D5 先写 Redis、再经 Kafka 由 go/db 写 TiDB。
> - §D5 的「Redis 权威读缓存」,以及 write-behind 管线里经 Redis 缓冲、中转玩家数据的部分,由 §9 修订。applied cursor、retry/dead 队列、SubShard、写合并与 topic / partition 契约在新设计里逐条裁定(同 §D5 末尾修订注);per-key LWW、L1–L4 验收与 `sql_mode` 照旧。

---

## 结论(先读这节)

1. **目标形态 = 全区全服**。物理 zone(≈10 gate + 20 scene 的集群)只是**部署单位**;玩家归属用 **home_zone(逻辑区)** 表达(现状已存在: data_service 的 Redis `player:zone:{player_id}` 映射)。玩家可自由跨区;跨区动作 = **客户端重连目标区 gate**(复用现有 `handleCrossZoneRedirect` 的 HMAC token redirect 机制),**数据不搬家**。同 zone 内 gate↔scene 保持全连接 mesh(10×20=200 条内网连接,量级无压力)。
2. **玩家数据层收敛为一个 TiDB 集群**,表以 `player_id`(Snowflake uint64)为主键。"按 player_id 全局分片"由 TiDB region 自动切分承担,应用层不做分片路由。**合服 = `RemapHomeZoneForMerge` 改逻辑归属 + 业务数据合并(榜单/市场),零玩家主数据迁移**。
3. **proto2mysql 直接接 TiDB: 可行**。go-sql-driver 官方 Full 支持;库生成的 DML(`INSERT`/`INSERT ... ON DUPLICATE KEY UPDATE`/`REPLACE INTO`/`SELECT ... FOR UPDATE`)、`STRICT_TRANS_TABLES` 会话参数、`CREATE DATABASE IF NOT EXISTS`、多子句 `ALTER TABLE`(v6.2+)在 TiDB v8.5 LTS 上全部兼容。但**必须落三件事**,缺一不可:
   - **建表方言**: snowflake 主键时间戳在高位、单调递增,在 TiDB 默认聚簇表下是官方点名的写热点场景。玩家表必须建成 `PRIMARY KEY ... /*T![clustered_index] NONCLUSTERED */` + `/*T! SHARD_ROW_ID_BITS=N PRE_SPLIT_REGIONS=N */`(TiDB 扩展注释语法,MySQL 视为注释忽略,**同一份 DDL 双方言可用**)。proto2mysql 需新增对应表选项(见 §D6)。
   - **集群配置**: TiDB `txn-entry-size-limit` 默认 6MB,而玩家存档列是 MEDIUMBLOB(上限 16MB)——**默认配置下大存档写入直接报 `entry too large`**。上线配置须调到 ≥33554432(32MB,留一倍余量),并压测验证 TiKV `raft-entry-max-size`(默认 8MB)是否需同步调整。
   - **服务器升级 proto2mysql 新版**: go/db 现钉 `v0.0.18`(2026-10-12 更正:已升级,`go/db/go.mod` 与 `go/data_service/go.mod` 现钉 `v0.2.0`,本条保留作历史),是旧世系;GitHub main 已重写(`NewPbMysqlDB`→`NewDB`、raw binary 存储、descriptor schema sync)。**迁移时必须逐表显式锁定表名**(新版默认表名 = proto 全名含点号,不锁表名 schema sync 会建出一套新表,旧表数据"消失")。
4. **分阶段实施,不一步到位**:
   - **Phase 1(纯搬迁)**: 单 TiDB 集群承载现有 `zone_{N}_db` 逻辑库。库名派生、Kafka topic、write-behind 管线、per-key LWW 不变量**全部不动**,只换存储后端 + 建表方言。用 L1-L4 一致性测试和压测基线验收。
   - **Phase 2(全局化)**: 玩家表收敛到单一全局库,DBTask 生产端从"按本 zone 选 topic"改为"按 home_zone 选 topic";跨区带状态游玩 = redirect 重连 + 全局数据层直读,**废弃 `player_migrate` 的数据搬运职责**(冻结/单写栅栏语义保留)。
   - 每阶段是独立可回退的发布单元。
5. **玩家数据仍可按职责纵向分表、分 PB**。TiDB 的 Region 拆分解决行之间的数据分布,不会替应用拆开一行中的玩家数据。拆分先确定原子保存范围,再看访问方式、大小与增长;容量预算、当前实现和公开实践见 §8。

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

> *(2026-09-20 标注)* 本节后两条里的 `ErrUnsafeCrossNodeHandoff` 与「`player_migrate` / `CrossZoneReaper` 保留单写栅栏职责」已不成立,见文首状态说明;D2 的决策与顺序不变量仍然有效,落地形态以 cross-zone-scene-travel.md CZ-1 / CZ-4 为准。

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

> **2026-10-12 更正与补充(只读核对 + docs.pingcap.com v8.5 官方文档,未在 TiDB 上实测)**
>
> **1. 本节「snowflake 主键」的前提已过时,但「单调」的结论不变。**
> - 现在新号由号段发号:`go/shared/idsegment`,从 1 开始,步长 100~1000 动态调整,login 配置 `FallbackToSnowflake:false`。
> - 每个 login 在自己的段内严格递增,所有 login 的写入点挤在一小段连续 id 里。
>
> **2. 本节的处方只打散了行数据,没打散索引。**
> - `NONCLUSTERED` + `SHARD_ROW_ID_BITS` 打散的是隐藏行 ID `_tidb_rowid`;「退化」出来的那条 `PRIMARY KEY(player_id)` 唯一索引仍按 player_id 有序存放,首日建角的写入集中在它最右的一个 Region。官方「高并发写入最佳实践」把单调递增的索引列为热点成因。
> - `PRE_SPLIT_REGIONS` 只预切行数据,每条索引建表时只有 1 个 Region。
> - Load Base Split 只按读流量触发,化解不了写热点。
> - 仓库里没有任何 `SPLIT TABLE … INDEX`,也没有对 player_id 做打散。
> - 同类单调索引还有:`player_name.uk_player_name_owner(player_id)`;`transaction_log`、`player_snapshot` 的时间类联合索引;以及 [player-route-mapping-durability-proposal.md](./player-route-mapping-durability-proposal.md) 里 `player_route` 的 PK / `idx_route_storage` / `idx_route_updated`。(2026-10-12 G1a:`player_route` 改为聚簇 `(route_shard, player_id)` + 16 片前缀索引(2026-10-14 起为 64 片,见提案 §11.2「行片 16 → 64」),`route_meta` 拆出 `route_reconcile`,`player_route` 与 `zone_merge_fence` 的预切与防合并由 `route_tool tidb-prepare` 执行,见提案 §11.2「第 1a 批热点修复」。)
>
> **3. Phase 2 会把热点放大。** 原来每个 zone 各有一组玩家表,也就是 Z 条索引尾部;合进全局库后只剩 1 条。在 MySQL 上单调追加是 B+ 树最友好的写法,不构成热点,瓶颈是整台实例;在 TiDB 上这是真实的扩展瓶颈,加机器解决不了。
>
> **4. `AUTO_RANDOM` 用不了。** 官方要求它只能用于 BIGINT 聚簇主键,不支持 `NONCLUSTERED`;显式插入的值按原样存放。而 player_id 必须在落库之前由 login 铸出。
>
> **5. 可选解法(未拍板):**
> - 发号时在 2^55 值域内做双射置换打散,再预切索引,写入真正散开;
> - 或者加大每个 login 的号段、在段边界预切,写入点数等于 login 实例数。
> - 两种都会让新号更早进入 [2^22, 2^55) 区间,可能与 2024-10-27 之前铸出的存量 Snowflake player_id 撞号。撞号不报错,会静默串档。所以先审计存量 id 或收紧号段上限,由负责人拍板。
> - 只做预切、不做打散,对单调键无效。
> - 选定前先在 TiDB staging 上按 AGENTS §6 压测「首日建角 + 存盘」,用 Key Visualizer 区分 index 与 record 流量。
>
> **6. 还有一项待核实。** proto2mysql v0.2.0 的 Save 生成的是 `REPLACE INTO` 还是 `INSERT … ON DUPLICATE KEY UPDATE`,仓库内说法矛盾、源码未核对。若是 REPLACE,非聚簇表每次存盘都会改写一次 PK 索引项,热点会从「首次插入」扩大到「所有首日玩家的存盘」。
>
> **2026-10-13 更正**:已核对 proto2mysql v0.2.0 源码,`Save` 按主键先 UPDATE,未命中再 INSERT,重复键再 UPDATE 并核对,既不是 REPLACE 也不是普通 ODKU。上段的 REPLACE 风险是假设,不能作为当前实现事实;具体 SQL 往返与条件写遗留见 [db-task-pipeline-bottlenecks.md](./db-task-pipeline-bottlenecks.md) §6.6 / §8。本次未编译或运行真库测试。
>
> 开服首日的完整瓶颈排序与开服前清单见 [tidb-phase1-and-launch-readiness.md](./tidb-phase1-and-launch-readiness.md)。

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

> **2026-10-15 修订(负责人 2026-10-13 拍板「④ 先做并修订 D5」;第 1 条措辞按 [S1 修订稿](../handoff/godb-pipeline-impl-20261015/S1-retry-owner-waitaof-addendum.md) §8 定稿,2026-10-17 写入本文)**
> 状态:④ 与 WAITAOF 已落码,未编译、未测试,待 Codex 验证。验证与上线项见 [上线待做](../ops/godb-pipeline-launch-todo.md) §9。
> 1. 「retry/dead 队列全不动」改为允许两项改动:④ 重试只交给分区属主(新增键族 `kafka:retry:queue:{topic}:p{N}`,开关 `ServerConfig.Kafka.RetryRouting`,见 [db-retry-owner-routing.md](./db-retry-owner-routing.md))与重试 / 死信 / 毒消息入队补 WAITAOF(`ServerConfig.RedisClient.WaitAOF*`)。两项随同一个 S1 镜像发布,S1 整体作为一个**独立发布单元**,不得与 TiDB Phase 1 任何迁移批次同窗口,也不得在同一次滚动里发布;两项的开关与指标各自独立,出问题时按 `db_retry_route_total` / `db_durable_enqueue_*` 归因。
>    - 开关:④ 是 `RetryRouting`(`shared` | `owner`,缺省 `shared`;环境变量 `DB_RETRY_ROUTING` 优先,只用于单 zone 金丝雀);WAITAOF 是 `WaitAOFNumLocal` / `WaitAOFNumReplicas` / `WaitAOFTimeoutMs`(代码缺省 1 / 0 / 1500,两个份数都写 0 才关闭)。取值只在启动时读:K8s 上只改 ConfigMap 不会滚动 go/db,改值后必须 `rollout restart deploy/db`。
>    - 不拆成「先 0/0 发镜像、再开 1/0」两步:ConfigMap 由契约测试强制镜像 `go/db/etc/db.yaml`,拆步要改两次配置、多一次发布;S2 之前每多一次滚动就多一轮全服 eager 再平衡;第 2 步 ④ 处于 `shared`,行为基本等于今天,WAITAOF 的影响能单独从 `db_durable_enqueue_*` 看出来。
> 2. 迁移窗口内冻结 RetryRouting:L1–L4 与 chaos_test 在迁移前后用同一个值跑;迁移的「停写 drain」核对清单加上 `kafka:retry:queue:{topic}:p*`。
> 3. topic 命名与 partition=10 不可变契约**不变**:④ 不改分区数、不换代;④B2(双消费在线换代)本轮不做。要做 B2 必须先落 MySQL 条件写,并另行修订本节第一条「改 topic 路由必须走停写 drain + 新 TopicGeneration」,不能以本修订为依据;双消费窗口内禁止 relocate / 合服,三方相等校验要同步放宽。
> 4. 风险表第 9 条照旧。

> **2026-10-24 修订**:玩家数据不再用 Redis 存储(§9)。本节的「Redis 权威读缓存」,以及 write-behind 管线里经 Redis 缓冲、中转玩家数据的部分不再保留。applied cursor、retry/dead 队列、SubShard、写合并以及 topic / partition 契约是否随新存盘路径调整,在新设计里逐条裁定,裁定之前照旧;per-key LWW、L1–L4 验收与 `sql_mode` 不受影响。

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
- 逻辑归属: `RemapHomeZoneForMerge` 批量改映射;榜单/市场/公会合并是纯业务操作。(**2026-10-12 注**:`RemapHomeZoneForMerge` 已停用,一律 `FailedPrecondition`;映射改写走 `tools/merge_zone`,映射持久化第 2 批起改为库先写,见 [player-route-mapping-durability-proposal.md](./player-route-mapping-durability-proposal.md) §4.3、§11.2。)
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
| 11 | 映射持久化路由表的热点(2026-10-12 登记,[player-route-mapping-durability-proposal.md](./player-route-mapping-durability-proposal.md) §11.2 风险第 1 条):`zone_merge_fence` 只有「zone 数 × 分片数」行,必落同一个 Region,悲观事务的 `FOR UPDATE` 让全服每次建角都在同一个 Region Leader 上加锁 / 解锁;`player_route` 的主键唯一索引与 `idx_route_updated` 单调递增,`idx_route_home(home_zone, player_id)` 在单个 zone 内也是尾部热点(NONCLUSTERED + SHARD_ROW_ID_BITS 只打散行数据);`route_meta` 单行是读热点 | P1 | `zone_merge_fence` 已建成 `CLUSTERED`,上线前按 zone `SPLIT TABLE`(§6 Phase 1 第 8 条);`player_route` 的热点纳入 TiDB 压测,用 Key Visualizer 区分 index 与 record 流量。2026-10-12 G1a:`player_route` 改为聚簇 `(route_shard, player_id)` + 16 片前缀索引(2026-10-14 起为 64 片,见提案 §11.2「行片 16 → 64」),`route_meta` 拆出 `route_reconcile`,`player_route` 与 `zone_merge_fence` 的预切与防合并由 `route_tool tidb-prepare` 执行,见提案 §11.2「第 1a 批热点修复」 |

## 5. 验收标准(Phase 1)

1. proto2mysql 测试套在真实 TiDB v8.5 实例上全绿(含 16MB blob 写读、`ON DUPLICATE KEY`、多子句 ALTER、FOR UPDATE)。
2. [data-consistency-stress-testing.md](./data-consistency-stress-testing.md) 的 L1-L4 + chaos_test 全绿。
3. 压测对比: 以最近一轮 stress 基线(`prev-summary.txt`)为参照,DB task 子阶段延迟无回归、TiDB region 无持续热点。
4. `SHOW CREATE TABLE` 与 `GetCreateTableSQL` dump 全表 diff 为空(含表名)。

## 6. 实施清单

**Phase 1(按依赖顺序)**:

1. proto2mysql: TiDB 方言表选项 + 单测(库仓库,`main` 分支) — **已完成**(commit `2aca007` 已推送 main;`go test` 待 Codex)
2. proto2mysql: 打 tag(旧世系 v0.0.18 已断,新版从 v0.1.0 起) — **已完成**(Codex 测试绿后,`v0.1.0` 已打在 `2aca007` 并推送远端。注意:仓库实际已迁至 `github.com/luyuan-cpp/proto2mysql`,module 路径仍声明旧名 `luyuancpp`,靠 GitHub 重定向工作——一旦旧用户名被抢注,go get 链路即断,建议后续统一 module 路径)。**2026-09-15 更正**:Go 代理与 sumdb 永久缓存的 `v0.1.0` 是 `9ad991c`(07-29 首次缓存),tag 之后才被移到 `2aca007`,缓存不可改——任何 require `v0.1.0` 的 module 都拿不到 TiDB 选项。版本口径改为不可变的 ≥ `v0.1.1`(D-14 第 7 条)
3. go/db: 升级依赖 + `NewDB` 迁移 + 逐表 `table_name` 锁定 + 玩家表加 TiDB 方言 option(proto 侧) — **代码已落,2026-08-16/17 编译、单测、起服冒烟通过;但 TiDB 选项在服务产出的 DDL 里并未生效**(2026-09-15 更正:当时 go/db 经代理拉到的 `v0.1.0` 实为 `9ad991c`,不含 TiDB 选项;08-17 的 TiDB 真集群验证用的是手写 DDL,只证明了方言语法,没有证明服务产出。现状:go/db 以 replace 指向本机 proto2mysql 分支 `f3b308f`(含 TiDB 选项 + 字符串主键 VARCHAR(191) 修复),data_service 仍在 `9ad991c`;收口 = 主键修复进不可变 tag 后各 module 统一 require,由人执行,见 xuanming-port-decisions-20260910.md D-14 遗留。原始记录见 PROGRESS.md 2026-08-15 续二):`NewDB` 迁移 + 启动期表名守卫(`assertTableNameLocked`,风险 #3 兜底);表名锁定由既有 `OptionTableName`(与新版选项同号 500001,库按字段号读取)天然满足,守卫防回归;proto 侧 5 表加 TiDB 选项(player_database / player_database_1 / player_centre_database / player_snapshot / rollback_audit_log);另按 §D3 同理覆盖 data_service 裸 DDL 3 表(player_snapshot / rollback_audit_log / **transaction_log**——tx_id 雪花主键、全服最高频追加写,虽未在 §D3 点名但同属其处方范围)
4. deploy: TiDB 集群清单(docker-compose dev / k8s prod)+ §D4 配置;`CREATE DATABASE` 权限策略重议 — **dev 已落**(`deploy/docker-compose.tidb.yml` + `deploy/tidb-config/`,与 MySQL 并存可回退);k8s prod 清单与权限策略未做
5. 迁移工具: 开发环境直接重建;有存量数据的环境用 Dumpling + Lightning(逻辑库对逻辑库)
6. 验收: §5 全项
7. 文档: 回档 runbook TiDB 版(§D8)
8. 映射持久化路由表(2026-10-12 追加,风险 #11):data_service 全局库随 Phase 1 迁 TiDB 之前,`SPLIT TABLE zone_merge_fence BY (2,0),(3,0),…` 按 zone 预切(表已是 `CLUSTERED`,建表后不能再改聚簇属性);`player_route` 的主键 / `idx_route_home` / `idx_route_updated` 尾部热点与围栏表的加锁热点纳入 Phase 1 压测。TiDB 上还要确认 `tidb_txn_mode=pessimistic`(route store 建池时探针会拒绝 optimistic)与 `tidb_enable_check_constraint`(默认关,CHECK 不生效,应用层校验兜底)。2026-10-12 G1a:`player_route` 改为聚簇 `(route_shard, player_id)` + 16 片前缀索引(2026-10-14 起为 64 片,见提案 §11.2「行片 16 → 64」),`route_meta` 拆出 `route_reconcile`,`player_route` 与 `zone_merge_fence` 的预切与防合并由 `route_tool tidb-prepare` 执行,见提案 §11.2「第 1a 批热点修复」;`tidb_enable_check_constraint` 从此是**必须**(`chk_route_shard` 关系到 `player_id` 唯一性,建池探针实测它生效,不生效就拒建 store),且要在建表前打开

**明确未做(本次)**: Phase 2 全部(全局表收敛、home_zone 字段落表、topic 按 home_zone 路由、解除跨区 fail-closed)、应用级回档通电、死信队列消费者。

## 7. 连带文档更新

- [x] 本文创建
- [x] ARCH.md §10 索引 + §11 决策表追加
- [x] CLAUDE.md §1 基础设施行
- [x] db_zone_isolation.md / cross_server_architecture_principle.md / mmo_cross_server_architecture.md / server_merge_design.md / zone_data_rollback.md / db-service-root-credentials.md 顶部修订标注
- [x] PROGRESS.md 当日条目
- [ ] docs/ops/mysql-backup-pitr-runbook.md 的 TiDB 版(Phase 1 第 7 步,迁生产前必须完成)
- [ ] data_service_role_and_scope.md(Phase 2 动 home_zone 持久化时再改)

## 8. 玩家数据分 PB / 分表原则(2026-10-13 补充)

> **范围与状态**:补充玩家数据模型的设计原则及当前源码事实,没有修改 proto、表结构或存盘管线,也没有新增压测结论。本文的 TiDB 迁移不自动触发玩家表重构。下述职责分组是后续设计参考,不表示对应的新表或按需加载能力已经实现。

### 8.1 三种拆分分别解决什么问题

| 概念 | 含义 | 是否缩小单行存档 |
|---|---|---|
| 业务 PB 拆分 | 背包、技能、属性等规则分别由子 message 表达 | 不一定;子 message 仍可落在同一 SQL 行的不同 BLOB 列 |
| 玩家数据纵向分表 | 同一玩家的不同职责分别落入不同表、独立记录 | 是,但要处理读取范围、原子提交和恢复 |
| 集合拆成多行 | 道具、邮件、历史等按实例或记录保存 | 是;同时支持局部更新、分页与生命周期管理 |

TiDB 将一条 SQL 行编码成一个 KV 记录,再按 Key 范围管理 Region。增加 TiKV 节点不会把一行中的背包、技能等字段自动拆开。把 PB 拆成多个子 message 也不会自动获得跨表事务或局部保存能力。依据:[TiDB 数据映射](https://docs.pingcap.com/tidb/stable/tidb-computing/)、[单行大小配置](https://docs.pingcap.com/tidb/stable/tidb-configuration-file/#txn-entry-size-limit-new-in-v4010-and-v500)。

采用固定、职责明确的存储模型。所有玩家按同一套契约保存数据;数据增长时评估该职责的表或集合模型,不为个别大玩家临时创建新表。新设计的 PB / 表名表达业务职责,不继续用 `_1`、`_2` 编号代替职责。存量编号与字段在完成兼容迁移之前保留。

### 8.2 当前实现与分表方案的关系

当前已具备「不同持久化 PB 对应不同表」的框架,但第二张玩家表尚未承载独立业务:

| 当前入口 | 源码事实 | 对设计的含义 |
|---|---|---|
| [玩家表契约](../../proto/common/database/mysql_database_table.proto) | `player_database` 包含核心状态、货币、背包、任务和账本;`player_database_1` 只有 `player_id` 与压测探针 | 业务数据仍主要集中在一张表,不能把空的第二张表视为已经完成冷热拆分 |
| [新号骨架](../../go/login/internal/logic/pkg/dataloader/new_player_seed.go) | `newPlayerAllDataSkeleton` 初始化两份子 message | 只构造当前读取契约的空数据,不负责选择分表策略 |
| [登录聚合加载](../../go/login/internal/logic/pkg/dataloader/ensure_player_all_data_async.go) | 父键未命中时遍历两份子 message,读子缓存,缺失的各自发 DBTask,全部完成后写父键 | 当前没有按重要 / 次要数据延迟加载的实现 |
| [玩家保存](../../cpp/libs/services/scene/player/system/player_lifecycle.cpp) | `SavePlayerToRedisImpl` 分别为两份子表发送写 DBTask | 每条任务独立保存一条记录,两张表之间没有共同原子提交 |
| [装卸代码生成](../../tools/proto_generator/protogen/internal/generator/cpp/player_data.go) | 根据 `OptionIsPlayerDatabase` 为已声明的持久化消息生成装卸代码 | 字段归属由 proto 源契约决定,生成器不会自动按大小拆 PB |

`player_database` 内的业务子 message 分别映射到 BLOB 列,不是把整份 `PlayerDatabase` 作为单个 BLOB。列分开仍属于同一 SQL 行,TiDB 单行预算须考虑整行大小。

因此现有思路可以沿用,后续新增分块需说明它的职责、读写者、增长上限和原子保存范围。是否退役空的 `player_database_1` 是独立迁移事项,不能只删除骨架中的一行:旧 DBTask、子缓存、父键、快照、装卸代码及迁移工具都引用它。

### 8.3 多大需要拆:区分数据库限制与项目预算

没有统一的行业标准规定「单玩家达到 N KiB 必须分表」。应量测每个持久化单元,不能只看玩家全部数据的合计。小而频繁更新的状态和大量低频数据放在一起,也可能需要拆;低频、有界的大记录则须结合延迟与成本判断。

| 约束 | 当前口径 | 用途 |
|---|---|---|
| TiDB 默认单行 KV 上限 | `6291456` 字节,即 6 MiB;可以配置 | 数据库保护限制,不是普通玩家存档的大小目标 |
| 本仓 dev TiDB 配置 | [tidb.toml](../../deploy/tidb-config/tidb.toml) 为 32 MiB;[tikv.toml](../../deploy/tidb-config/tikv.toml) 同步放宽写请求限制 | 迁库及大记录的容量兜底,不能据此宣称普通玩家写 32 MiB 有可接受性能;目标集群仍须核实有效配置 |
| 普通玩家单个 BLOB 列 | [db.yaml](../../go/db/etc/db.yaml) 的 `MaxColumnBytes=262144`,即 256 KiB | 业务组件的项目容量预算 |
| 普通玩家单行 BLOB 估算合计 | 同配置的 `MaxRowBytes=1048576`,即 1 MiB | BlobGuard 所量 BLOB 列估算合计的项目容量预算,不包含标量及 SQL / KV 编码开销 |
| 容量预警 / 拒写 | `WarnRatio=0.8`;`ReportOnly=false` | 达到预算 80% 预警,超过上限拒写;表级覆盖按实际配置生效 |

**计量口径必须标明**:[BlobGuard](../../go/db/internal/dbguard/blob.go) 当前量顶层 message / bytes 字段,跳过顶层 repeated / map;先取子 message 的 `proto.Size` 或 bytes 长度,再用 Base64 编码长度估算,各列累加。已核对 proto2mysql v0.2.0 的 `pbconv/convert.go`:嵌套 message / bytes 写裸二进制,不做 Base64;闸门仍保留旧估算,通常约为 raw payload 的 4/3。因此不能把这个估算值当作实测 SQL / TiKV 行大小,也不能直接把 `proto.Size(PlayerAllData)` 当作它。迁库或调整预算前应核对实际依赖的编码、存量格式与统计口径;本次只记录差异,不修改闸门或放宽上限。

容量预警触发调查,不自动移动字段或裁剪玩家资产。判定顺序:

1. 明确增长来源及业务上限:道具数量、历史保留期、任务集合与账本窗口等。无限增长的历史优先拆成记录并设置归档 / 保留策略。
2. 看列与行的大小分布(p95 / p99)、最大合法存档及增长趋势。分位数用于容量判断,最大合法数据用于验证不会拒写;不能只看平均值。
3. 对照每类数据的加载范围、修改频率和实际重写量,以及序列化耗时、存盘 p99、Kafka lag / 消息限制与登录耗时。必须一起检查 PB、Kafka、Redis 和数据库的整条路径。
4. 只有测量支持时才决定拆表、集合多行化或调整预算。声称收益须按 `AGENTS.md §6` 保存压测基线与对比,不以本节的预算代替性能证据。

观测沿用 `db_blob_bytes{table,column}`、`db_blob_row_bytes{table}` 与 `db_blob_guard_total{table,column,level}`;禁止以 `player_id` 作指标 label。超限任务进入失败处理并可能最终进入死信,不等于成功保存。应在到达拒写上限前处理增长来源。

### 8.4 合理的职责分组与原子保存范围

| 职责分组(设计参考) | 数据示例 | 保存与拆分要求 |
|---|---|---|
| 玩家核心状态 | 等级、属性、技能等有界状态 | 按实际共同加载、共同更新的范围分组;不是每个组件都必须一张表 |
| 玩家资产状态 | 货币、背包、奖励领取标记、资产通道账本、战斗结算幂等账本 | 相关资产与处理标记共同提交、共同恢复;当前保留同记录同 DBTask |
| 普通成长进度 | 任务、成就、剧情进度 | 独立更新才有独立保存价值;改变资产的领取标记不能未经原子提交设计单独移出 |
| 邮件与历史记录 | 邮件、战斗记录、操作历史 | 按独立记录保存,支持分页、期限与归档;不持续塞进登录必读的大存档 |

例如,背包从玩家主表移到背包表只能缩小主表,背包本身仍会增长。若规模与访问方式确实需要多行模型,可按 `(player_id, item_id)` 保存道具实例,布局独立表达槽位、占位与排列,遵循 [实例 / 布局分层设计](./bag-instance-layout-split.md) 与 `AGENTS.md §11.6`。这是建模层次的分离,不要求实例与布局一定分成两张 SQL 表或两条 DBTask;两份子 message 可以同记录共同保存。布局源码不反查道具配置;由桥接层传入所需信息。旧持久化字段按兼容迁移方案处理,不能据规范推断当前 wire 结构已完全分离。该例是设计参考,没有授权或实现新的背包存储模型。

**同一玩家、同一个 Kafka key、按顺序执行,均不等于跨表原子提交。** 购买、交易、领奖涉及的资产增减与幂等 / 领取标记必须要么共同生效,要么共同不生效。当前契约已在 [玩家表 proto](../../proto/common/database/mysql_database_table.proto) 明确要求 currency、bag、资产账本及结算账本同记录同 DBTask;本节不放宽该不变量。

以后若因数据规模必须拆开这一原子单元,需要先设计完整的多表保存契约:同一数据库事务中复核所有权 / 版本并提交相关变更,明确重试幂等、故障回滚、Redis 发布与加载、回档恢复的一致性。现有每表独立 DBTask 不能直接提供这一能力。独立保存的分组也须明确一致性读取要求,不能把不同版本拼成一个假定为同版本的快照。相关同 epoch 旧写防护的遗留事项仍见 [存盘管线瓶颈](./db-task-pipeline-bottlenecks.md),本次没有实现条件写。

### 8.5 与公开游戏存储实践的对照

以下资料核对日期为 2026-10-13。它们展示不同存储产品中的设计取舍,不能解释为所有大厂都采用相同表结构、相同 PB 或统一大小阈值:

- [AWS 公布的 EA 等游戏案例](https://aws.amazon.com/blogs/database/amazon-dynamodb-gaming-use-cases-and-design-patterns/)(2019):EA 将游戏状态、用户与道具数据放在多张表中;文章同时说明,需要细粒度访问时可读取 / 更新玩家数据的子集,并用事务处理关联记录。与本仓「持久化 PB 分组、分别落表」属于同一类建模思路,不证明 EA 使用本仓的 PB、DBTask 或缓存实现。
- [AWS 官方垂直拆分建议](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/data-modeling-blocks.html#data-modeling-blocks-vertical-partitioning):按逻辑实体拆成可独立访问的小记录。DynamoDB 的 400 KB item 限制是该产品的约束,不能作为 TiDB 分表阈值;同一物理表的多条 item 也不等于本仓多张 SQL 表。
- [Roblox 官方玩家数据建议](https://create.roblox.com/docs/cloud-services/data-stores/player-data-purchasing):相关会话数据放在同一存储单元可以减少请求、保证多值原子更新并简化回滚;购买验证、发放、处理标记须原子完成。其示例明确尚未经过热门游戏长期验证,本文引用的是官方设计建议,不称其为 Roblox 内部生产模型。

本仓已有分表框架和资产共同保存的契约;实质业务分组、按需加载与保存仍须按真实需求设计和验证。当前空的第二张表不能作为这些能力已经完成的证据。

### 8.6 后续落地的兼容与验证要求

本次只补文档。实际拆分属于独立变更,须先明确字段真源、读写所有者、原子范围、数据量上限与验收标准,再决定表 / PB 形状。迁移设计覆盖 proto 源与生成器、建表、装卸、DBTask、缓存、快照 / 回档、合服与落点迁移工具、新旧版本并存和回退。上线字段不复用编号,删除使用 `reserved`;不得手改生成产物。验证按风险覆盖冷 / 热加载、失败、重放、幂等、并发旧写、原子恢复与兼容。

Java 版:本节只记录共享设计原则及 C++/Go 当前源码,对应 Java 版本号为无,没有功能或客户端契约变更。不能推断 Java 当前已有相同表或 PB;以后实施业务拆分时按 `AGENTS.md §12` 核对另一版、同步客户端可见契约并登记 `PARITY.md`,内部存储按各自实现设计。

## 9. 玩家数据不再用 Redis 存储(2026-10-24 方向,未设计、未落码)

> **来源**:2026-10-24 用户在「玩家数据存储 Redis vs TiDB」会话里明确:「我不是撤掉 redis 啊,就是玩家数据不要 redis 存储而已,其他改用的地方还是要用的哦……该用还是用」。
> **状态**:只定方向。存盘与加载的新路径没有设计、没有落码,现行代码仍按 §D5 先写 Redis、再经 Kafka 由 go/db 写 TiDB。项目尚未上线,按[存盘围栏](./db-write-fencing-mysql.md)文末「上线状态」,落码时直接以新形态起步,不做双写与迁移。
> **证据口径**:§9.3 的键族由本会话 6 路只读盘点、1 路对抗核查和 3 路文档核查(事实、分类完整性、跨文档一致)得出,以 2026-10-24 主工作区为准(多个会话在改,引用以文件名、函数名为主);[C] 读代码确认,[D] 文档记载,[I] 推断。没有编译、运行或压测。

### 9.1 决定的内容

1. 玩家数据(持久存档,以及它的整份或部分副本、中转载荷)以 TiDB 为唯一存储。Redis 不再保存、缓存或中转玩家数据本体。
2. **Redis 不撤。** 会话、锁、归属与位置投影、排队与限流、队列、Pub/Sub 等协调用途照旧用 Redis;以后的功能该用 Redis 的地方继续用,不因本条改走 TiDB。
3. 本条修订 §D5「Redis 权威读缓存 + applied cursor + retry/dead 队列、SubShard 并行、写合并: 全不动」里与玩家数据有关的部分(读缓存,以及经 Redis 缓冲、中转玩家数据的部分)。applied cursor、retry/dead 队列、SubShard、写合并与 topic / partition 契约随新存盘路径逐条裁定,裁定之前照旧;per-key LWW、L1–L4 验收与 `sql_mode` 不受影响。

### 9.2 判据

一个 Redis 键算不算「玩家数据」,看它的值,不看键名里有没有 player_id:

- 值是玩家持久数据的内容,或内容的副本、中转载荷,丢了只能从 TiDB 找回 → **玩家数据,移出 Redis**;
- 值是代次、租约、锁、位置、标记、计数、队列成员一类的协调信息,丢了可以按协议重建或按 TTL 自然失效 → **协调态,留在 Redis**。

两者兼有的(例如同一个 hash 里既有载荷又有版本号),拆开后各归各类。协调态里只服务于 Redis 父快照或 outbox 的键(例如补投索引),不因为它是协调态就保留:载荷移出后它失去用途,随新设计一起去留。

只作加速、真源在 TiDB、可随时丢弃、正确性从不依赖它的读缓存,按值算是副本,但不是存储。这类缓存是否保留逐项裁定(看有没有读者、能不能接受丢),默认不新增。

### 9.3 现行键族分类

**A. 移出 Redis**

| 键族 | 现在的作用 | 去向 |
|---|---|---|
| `PlayerAllData:{<pid>}` 父快照(第 2b 批已改为带 tag 的键名,未编译) | 写方:scene 每次存盘在严格屏障 Lua 里写;login 的进场预加载(SQL 读回后发布)、核验采纳、新号种子(`NewPlayerSeed`)、首次入场身份补齐(整字节 CAS)也写。读方:scene 进场只从它加载,不读 SQL [C]:`redis_client.h` 严格存盘脚本与 `AsyncLoad`、`role_admission_system.cpp`,login `ensure_player_all_data_async.go`、`parent_commit_barrier.go`、`new_player_seed.go`、`player_class_backfill.go` | TiDB 两张玩家表为唯一来源;存盘与加载路径重新设计(§9.4 第 1、2 条),scene 与 login 的写方同批去掉 |
| `{MsgType}:<pid>` 子表缓存(`player_database:<pid>`、`player_database_1:<pid>`) | go/db 写任务 SQL 提交后(applied / equal)在发布资产证据与 ACK 的同一段令牌 Lua 里 SET 整行;读任务只在键不存在时 SETNX 回填;TTL 3600s。login 冷加载已不读,生产路径没有读者 [C]:`player_cache_publish.go`、`key_ordered_consumer.go`、`ensure_player_all_data_async.go` | 删除;令牌 Lua 随之改形 |
| `player:{<pid>}:save_barrier` 里的 outbox 载荷 `outbox_player_database`、`outbox_player_database_1`(两条 DBTask 原字节) | 「写 Redis」与「投 Kafka」不能原子完成,所以意图要和父快照在同一段 Lua 里留存,供 go/db saveoutbox 补投 [C]:`redis_client.h` 严格存盘脚本、`go/db/internal/saveoutbox` | 父快照不进 Redis 后不再需要;新存盘路径若保留异步投递,意图留在哪里另行设计 |
| go/db 重试与死信队列的成员:`kafka:retry:queue:*`(含 `:p<N>`)、`kafka:retry:processing:*`、`kafka:dead:queue:*` | 写失败、隔离、毒消息时把整条 DBTask 压进列表(`body` 是玩家行整行字节),重试协程认领后重放 [C]:`key_ordered_consumer.go` 的 `encodeRetryPayload`、`retry_routing.go` | 不再把玩家行字节留在 Redis 队列;若保留 Kafka → go/db,重试载荷改存 TiDB 或 Kafka 重试 topic,Redis 只留认领收据与计数 |
| `player:{<pid>}:asset_sql_commit` 资产 SQL 提交证据(含账本 `asset_op_ledger`) | go/db 在 SQL 提交后发布,data_service 资产通道据此判定已落盘 [C]:`player_cache_publish.go`、`go/data_service/internal/logic/asset_sql_commit.go` | 改为强读 TiDB 玩家行 |
| data_service 读 `PlayerAllData:{<pid>}` 解账本(`GetPlayerAssetOpLedger`、`GetPlayerInboxLedger`) | guild / trade / mail 资产通道与 player_inbox 对账判断「已见 / 未见」[C]:`persisted_player_database.go` | 同上;data_service 已有在 TiDB 里跨 schema 读两张玩家行的代码 [C]:`account_role_sql_proof.go` |
| `task:result:<id>` 读结果 | 玩家读 DBTask 的回包,载荷是整条消息,TTL 5 分钟 [C]:`key_ordered_consumer.go` | 加载改直读后不再承载玩家数据;其他读任务是否还用它另定 |
| `battle:settlement:pending:<pid>` 结算原件(伴生的 `battle:settlement:pending:id:<pid>`、积压索引 `battle:settlement:pending:index`、落库标记 `battle:settlement:staged:<battle_id>` 是协调键,随原件一起去留) | battle 先写再投递,scene 应用并按 battle_id 销账后才删,不过期;投递整条丢失时,下次登录靠它补发。值是要并入玩家存档的奖励原件 [C]:`cpp/libs/services/battle/settlement/settlement_outbox.h`、`player_battle.cpp` | 去向是 match 写 SQL 原件 + player_inbox(TiDB)入箱的新路径(工作区未提交)。两条路径各覆盖哪些对局核实之前,旧路径不删 |
| `match:rating:<pid>`(及入账标记 `match:rating:applied:<battle_id>`) | `GlobalMatch.Enabled=false` 时玩家评分只存在 MatchRedis,无 TTL;这个分支只有 dev / test 档允许,非 dev / test 档强制开启,评分走 TiDB `match_rating` / `match_rating_applied` [C]:`go/match/internal/logic/keys.go`、`rating.go`,`go/match/internal/authority/rating.go`,`go/match/internal/svc/servicecontext.go` | 删除 Redis 分支,评分只走 TiDB |
| data_service 字段 KV `player:{<id>}:<field>`、`__version`、`__fields`、`__lock` | 只存在于 Redis,没有生产写者;GM 快照、回档建在它上面 [C]:`data_logic.go`、`snapshot_logic.go`、`rollback_logic.go` | 删除,或把 GM 能力重建在 TiDB 玩家行上 |

**B. 新设计时逐项裁定**

| 键族 | 为什么待定 |
|---|---|
| `save_barrier` 的其余字段:`pending`、两表 ACK、`handoff_fence`、`schema_*`、`verified_identity`、outbox 元数据与补投重试态(`outbox_version`、`outbox_topic`、`outbox_attempt*`、`outbox_terminal*`、`outbox_retry_ms`);补投索引 `player:save_pending:*`(成员只是 pid);barrierrestore 维护键 `player:save_barrier:maintenance:<zone>` / `player:save_barrier:maintenance_release:*` | 本身是协调态,但几乎都只服务于 Redis 父快照与 outbox。ACK 镜像、`verified_identity`(证明 Redis 父快照与资产证据没回退)、补投索引与补投状态、恢复维护键在载荷移出后失去用途;`pending` 与 `handoff_fence` 是否保留取决于新的交接设计(§9.4 第 4 条) |
| go/db 管线控制:排序锁 `distributed:lock:kafka:ordering:*`、游标 `consumer:applied:*` / `consumer:applied_epoch:*`、重试认领守卫与实例登记 `kafka:retry:guard:*` / `kafka:retry:instances:*` / `kafka:retry:instance_mode:*`、存盘协议世代 `db:save_protocol:zone:*` | 本身是协调态,该在 Redis;但只在保留 Kafka → go/db 写管线时才有用,随 §9.4 第 1 条一起定。重试与死信队列的成员是玩家行载荷,已归表 A |
| `battle:activity_result:<battle_id>` | battle 对局结果发件箱(帮会同道历练):值是对局事件,TTL 7 天,guild 结算提交后 DEL,巡检器直接读它补结算 [C]:`cpp/libs/services/battle/system/battle_result_activity.h`、`go/guild/internal/data/trial_result_record.go`。不是玩家存档,但结算前是唯一原件、丢了不能重建,§9.2 两条都套不上,与结算原件同批裁定 |
| `player_merge_notice:<pid>`、`player_force_rename:<pid>` | merge_zone 按玩家写的合服公告与强制改名标记,无 TTL,Redis 是唯一记录 [C]:`tools/merge_zone/post_merge_stamp.go`、login `entergamelogic.go`。设计落点是玩家行 `merge_state`(`PlayerMergeStateComp`);force_rename 管线目前未启用 |
| 只读加速缓存:`player:name:<pid>`(全局 `player_name` 表的读缓存与负缓存,唯一性判定从不读它),以及各服务私有 Redis 里的非权威读缓存 | 真源在 SQL、可随时丢弃,按 §9.2 末段规则裁定是否保留 |
| 父快照的展示读:friend 好友档案与在线推荐(`go/friend/internal/data/friend_profiles.go`、`online_directory.go`)、match(`go/match/internal/team/presence.go`)、login 选角列表补外观(`go/login/internal/logic/clientplayerlogin/role_appearance_backfill.go`) | 改读 TiDB,或改为专门的展示投影。投影若放 Redis,只能是可从 TiDB 重算的副本,不得成为唯一来源。`online_directory.go` 在父快照缺失时直接跳过候选,改造时不能让推荐列表悄悄变空 |

**C. 继续用 Redis(不受本条影响)**

- 归属与交接:`player:{<pid>}:owner_epoch` 的铸造、`player:<pid>:handoff`、`location` / `location_revision` / `location_revision_floor`、离场释放等待。
- 会话与登录:`player:session:*`、`player:leases*`、`player_locker:*`、`login_generation`、进场去重。
- 战斗:`battle:lock:*`、`battle:ctx:*`(值是 `InBattleComp` 的对局状态,不在玩家表里)。
- 落地恢复摘要 `player:<pid>:sync_digest`:代次加各域变更计数,只是提示,丢失或过期时客户端全量拉一次。
- 排队与限流、场景路由与节点负载、选主锁。
- Pub/Sub:`assetop:wake`、`task:result:notify*`。
- 各服务私有 Redis 的协调用途:chat / friend / guild / match 的限流、配额、排队、选主锁等(rank 已全部走 SQL,不用 Redis;match 评分见表 A,非权威读缓存见表 B)。

**不在本条范围**:归属与落点映射 `player:zone` / `player:placement` / `player:maprev`,以及 go/db 能力标记 `db:capability:zone:*`。负责人 2026-10-23 已另行定为移出 Redis、直读 TiDB(设计见 [映射持久化提案](./player-route-mapping-durability-proposal.md) §15,未落码),本条不改变那个结论。

### 9.4 新设计必须回答的问题(都未定)

1. **存盘提交路径**:二选一或另提。
   - scene 经已有的异步 gRPC → data_service 通道直接提交 TiDB 事务。这条通道 `RegisterRoleAdmission` 已在用,接在 EventLoop 上、带签名鉴权,见 `role_admission_system.cpp`。
   - 保留 Kafka → go/db 异步写,但不在 Redis 留存意图。

   两种都要重新论证丢失窗口、退出与停机排空、同 key 写合并。scene tick 不得同步等 SQL([scene 线程模型](./scene-node-threading-model.md))。
2. **加载路径**:scene 进场从哪里拿数据,是 login 读 TiDB 后经 RPC 下发,还是 scene 经 RPC 读 TiDB。重连、同节点重登这类今天走 Redis 的热加载,代价要算清。
3. **单写围栏怎么闭合**:owner_epoch 仍在 Redis 铸造,存盘却落 TiDB,两者不再在同一段 Lua 里比对。只比 TiDB 行内已落库的 `(fence_owner_epoch, fence_save_seq)` 不够 [C]:
   - 行围栏只拒 epoch 更小的写(`player_row_fence.go`);go/db 的 applied_epoch 守卫有意只拦新持有者落库之后的老写(`key_ordered_consumer.go` 归属 epoch 守卫)。
   - 今天新持有者第一次存盘之前,挡住被废黜持有者迟到写的只有 C++ 严格存盘 Lua 的 owner 比对,外加靠时间的再入屏障(`enterscenelogic.go` 的 `playerLocationOwnerDead` 分支)。存盘改为直接提交 TiDB 后,Lua 这一层没有了,迟到写会被接受。
   - 入场登记已在同一笔 TiDB 悲观事务里把 owner_epoch 写进 `account_role_admission.identity_proto`(按 `(player_id, scene_instance_id)` 一行,`account_role_admission_store.go`)。但它不是可比较的列:登记时不比 owner_epoch、不跨实例取最大,也不抬玩家行围栏。
   - 可选方向:把它提成代次地板,或在入场登记的同一事务里抬两张玩家行的 `fence_owner_epoch`,存盘事务在同一事务里比对。Java 版把 owner_epoch 放在玩家行里,是现成先例。
4. **交接、交易、结算门禁**:今天经 Redis 里的两表 ACK 镜像判断「SQL 已追平」(`handoff_sql_barrier.h` 的发布标记脚本、`role_admission_system.cpp` 的 `kReadSqlAck`)。存盘直接提交 TiDB 后,判据改为事务提交结果或 TiDB 强读。
5. **两张玩家表的原子性**:今天两表是两条独立 DBTask,跨表不原子。直接提交 TiDB 时可以在同一事务里提交,须明确是否这样做;§8.4 的资产原子范围不得放宽。
6. **容量与延迟**:存盘与加载的 TiDB 往返按[容量模型](./capacity-model-hok-scale.md)的 N_save 口径压测,按 AGENTS §6 出对比表;TiDB 抖动、切主时的表现单独测。没有数据之前不宣称性能结论。
7. **读方与工具**:friend(好友档案、在线推荐)、match、login 的选角外观补齐与首次入场身份补齐(`role_appearance_backfill.go`、`player_class_backfill.go`)、data_service 账本、`go/db/internal/barrierrestore`、`tools/merge_zone`、`debug_fetch` / `debug_import`,以及压测工具 `go/db/cmd/verifier`,凡读父快照或子表缓存的地方逐个改。同批改不完的按 fail-closed 处理,不能带着旧键静默空过。(`tools/data_consistency_check` 只读映射 Redis,不在此列。)
8. **TiDB 部署**:仓库没有 TiDB 的 K8s 清单,只有本地 `deploy/docker-compose.tidb.yml`。`k8s_deploy.ps1` 只接收外部 TiDB 入口(`-GlobalAuthorityTiDBHost`,或必须与之相同的 `-FriendTiDBHost` / `-TradeTiDBHost` / `-MatchTiDBHost`),go/db 的玩家表与目录、好友、交易、匹配共用这一个入口。玩家数据只存 TiDB 之后,TiDB 是存档读写的唯一依赖,部署、备份与回档方案要先落(§D8、[TiDB Phase 1 与开服准备](./tidb-phase1-and-launch-readiness.md))。

### 9.5 对其他文档的影响

- [玩家态 Redis 集群化](./redis-player-store-cluster.md):
  - 父快照、子表缓存、资产证据、结算原件属于玩家数据,不再进 Redis Cluster。补投索引与补投器随父快照移出失去现有用途;新存盘路径若保留异步投递,另行设计。
  - 第 2b 批(父快照改名)在本方向记录之前已落码(未编译),保留还是回退待用户定。2c(补投索引按 slot)、2d(补投器分片)已暂停(该稿 §13)。第 4 批里子表缓存与排序锁 / 游标的改名、第 8 批战斗结算按 slot,等新设计定稿后再定。
  - owner_epoch(2a,已落码)、归属与位置族(3)、player_locator(7)等协调键的分片不受影响,但集群规模要按剩下的键重算。
- [共享 Redis 按用途拆分](./shared-redis-split-by-purpose.md):§19 redis-cache 的主要内容(子表缓存、玩家读结果)移出 Redis,「必做」结论待重评。go/db 控制面按组分实例(§18、§24 第 2 批)与 §18.7 令牌发布脚本的硬依赖,随 §9.4 第 1 条重评。§16 的相关推算要重算;§23 的冷卸载以父键为对象,父快照移出后失去对象。
- [异步存盘丢失窗口](./player-async-save-loss-windows.md)、[Redis 回退守卫](./zone-redis-regression-guard-20261022.md):以 Redis 父快照为对象的丢尾、回退与实例身份核验,随新存盘路径重写。前者 §7 的待实现修复、后者里只服务于父快照与资产证据的部分,开工前先按本条向用户确认是否还做。
- [存盘围栏](./db-write-fencing-mysql.md)文末 2026-10-17「C++ 父快照提交屏障」「登录冷恢复」附注,以及[跨 zone 传送](./cross-zone-scene-travel.md) CZ-2「`PlayerAllData` Redis 物理共享」契约:都以 Redis 父快照为提交与加载对象,随 §9.4 第 1~4 条重写。
- §D5 的修订见 §9.1 第 3 条与 §D5 末尾的 2026-10-24 修订注。

### 9.6 Java 版

Java 版的存档与归属代次在 MySQL,与归属相关的 Redis 键只有位置投影([Redis 回退守卫](./zone-redis-regression-guard-20261022.md)「Java 版不同构」一段与 §12 [D])。Java 是否还在 Redis 里缓存或中转别的玩家数据没有核过(同文 §12 注明 G5「资产放弃是否读 Redis 副本」未核)。本仓落码时按 AGENTS §12 核对,确认后再判 Java 是否已是目标形态;结论为不适用或已有等价时,按约定不往 Java `PARITY.md` 加行。本条只改文档,Java 版无对应改动(对应 Java 版本 0.1.0-SNAPSHOT)。

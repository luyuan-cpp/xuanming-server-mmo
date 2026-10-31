# 玩家归属与落点映射持久化:库为真源 + Redis 完整副本 + 就绪标记(已拍板;第 0 ~ 3 批已落码,未验证;2026-10-19 起读方按身份绑定的就绪标记判可信,见 §13)

> **2026-10-23 方向变更**：负责人拍板映射改为直读 TiDB、撤掉 Redis 副本与就绪标记，见文末 §15（设计已定，未落码）。本文 §2 / §5.1 / §5.4 / §13 的副本与就绪标记设计、§14 里依赖 ready 的首次启动步骤随之作废，落码以 §15 为准。

> **状态**:**已拍板(2026-10-12,负责人),分批落码中**。2026-10-12 起草;拍板结果与落码进度见文末「§11 拍板与落码记录」。
> **起因**:用户提出「逻辑服 id(home_zone)和 storage_id 应该存到 TiDB,Redis 只做缓存,不然 Redis 丢了映射无法恢复」。这正是 [TiDB 决策](./global-data-layer-tidb-decision.md) D1「映射持久化兜底」与 [player-storage-placement.md](./player-storage-placement.md) §4.4 / §11.4 的遗留项。
> **怎么来的**:多 agent 只读设计。先盘点两份映射的全部读写点与不变量,再并行出三种方案:A 库为真源 + Redis 缓存;B Redis 为真源 + 数据库副本;C 把归属写进玩家主表。之后从「数据一致性」「运维上线」两个视角做对抗评审,最后汇总成本稿。只读代码、grep、git 得出,没有编译、测试或压测。按 AGENTS.md §10.1,实现后交 Codex 验证,在那之前不能说「已验证」。
> **前置**:当前 HEAD 的落点代码先按 [placement-reverify-checklist-20261012.md](../handoff/placement-reverify-checklist-20261012.md) 复验,本方案不叠在未验证的基线上。行号以 2026-10-12 HEAD `73b852f32` 为准。


## 1. 一句话结论

**要落库。但 Redis 不是"未命中就回源"的普通缓存，而是一份带版本号、保证完整、有就绪标记的只读副本。**

对用户提法的四处修正：

1. **现在就落，不等 TiDB。** 放进 data_service 已有的 MySQL 全局库（dev 是 `testdb`，K8s 是 `mmorpg_global`），建表语句按 TiDB 兼容写，TiDB Phase 1 上线时随全局库一起迁过去。
   - 依据：`go/data_service/etc/data_service.yaml:92-98`，`tools/scripts/k8s_deploy.ps1:997`、`:3511-3516`。
   - TiDB Phase 1 本身"是否继续"还没拍板（`docs/design/player-storage-placement.md:374`），映射的风险今天就在。
2. **提交点从 Redis Lua 挪到数据库事务。** 合服围栏、`merged_into`、冻结态要**一起落库**，否则 Redis 一丢，"防回填""围栏互斥"会跟着失效。
3. **能力标记 `db:capability:zone:{z}` 必须留在 Redis、保持临时，不落库。** 落库会替已回退的旧版 go/db 作证，见 `go/db/internal/kafka/placement_route.go:369-379`。
4. **Redis 丢失依然要全服维护。** 同一个实例里还有会话、锁、存盘游标、`PlayerAllData`（`tools/scripts/k8s_deploy.ps1:3460-3465`），映射落库只解决两件事：
   - 映射能自动恢复；
   - 恢复期间不会静默写错库。今天的行为是：scene_manager 把 gate zone 当 home（`go/scene_manager/internal/logic/home_zone.go:144-151`），go/db 走 legacy 分支写进本 zone 库（`go/db/internal/kafka/placement_route.go:171-172`）。

**紧迫度**：今天的实际风险偏低。所有环境都是 `PinOnCreate=false`，从没跑过 pin、relocate，归属与物理库仍一一对应（`player-storage-placement.md:5`）。但两件事必须排在本方案之后：
- 生产上执行 §13 第 4 步（pin 合服、relocate、`PinOnCreate`）；
- 打开 `PinOnCreate` 并指向全局库。这时会出现"无法恢复的新号"缺口（`player-storage-placement.md:116`、`:384`），本方案是硬前置。

---

## 2. 推荐方案总览

以方案 A 为骨架，嫁接 B、C 和两份评审的修补。

| 维度 | 做法 | 来源 |
|---|---|---|
| 真源 | 全局库 4 张表：`player_route`、`zone_merge_fence`、`zone_merged_into`、`route_meta` | A |
| 提交点 | RC 悲观事务，加锁复核（围栏行 → 玩家行），COMMIT 即提交 | A |
| Redis 键格式 | **不改**，仍是 `player:zone:{id}` 和 `player:placement:{id}`；只新增一个版本键 `player:maprev:{id}` | C + 评审 2 |
| 发布 | 提交后用 set-if-newer Lua（按 maprev）写三把键；与 **WAITAOF** 走同一条连接，确认后才推进依赖步骤 | A + 评审 1 修补 |
| "未命中"的判定 | 整份副本级别：全局就绪标记 `mapping:route:ready` 存在 = 副本完整，缺键就是权威缺席；标记不在 = 不可信 | B/C 的标记 + A 的真源 |
| 热路径 go/db | 不连全局库；标记不在时写延后（不耗重试预算）、读照常耗预算 | C 的论证，取代 A 的 RouteDB 只读池 |
| data_service 读 | 标记不在时按主键回源数据库 | A |
| 部分丢失检测 | 对账 leader 每拍比对 Redis 的 `run_id` 和 `master_replid`，变化就先撤标记、再按 `updated_at_ms` 修补、再挂回标记 | B，改成"库为真源"，方向不会反 |
| 库回档检测 | `route_meta.watermark_ms` 回退 → 全体写者只读，从缓存反向修库 | 评审 1 的"库可信闸" |
| 不需要的东西 | 新值格式 `player:route`、MirrorLegacy、P6 停写旧键、负缓存、outbox、guardian、R5a/R5b、行戳 | 见 §8 |

**提交即安全**（全方案的核心纪律，来自 A）：每个数据库事务提交后的状态，被读方提前看到或延后看到都必须安全。依赖步骤（login 落账号 blob、合服开服、搬库 R2/R3）只能在"发布 + WAITAOF 确认"之后开始。

---

## 3. 表结构与存放位置

### 3.1 位置

- **库**：data_service 的 `SnapshotMySQL.DBName`。data_service 已在这里存 `id_segment`、`player_name`（`go/data_service/internal/store/schema.go:20-30`）。
- **建表方式**：
  - 照 `id_segment`、`player_name` 的先例，走手写 DDL，放进 `bootstrapTables`（`schema.go:47-68`、`:153-161`）。原因是需要 VARCHAR 的 run_id、可空列、二级索引和 CHECK，而 proto2mysql 会把 string 渲染成 MEDIUMTEXT（`schema.go:47-49`）。
  - 同时在 `proto/common/database/rollback_database_table.proto` 追加对应 message，加进 `TableMessages()`（`schema.go:1195-1206`），只用来做列漂移守卫。
  - 这份 proto 也编进 C++（`cpp/generated/proto/CMakeLists.txt:81`），所以要按 AGENTS.md §4 重新生成，并重编 C++ 的 proto 库。
- **建表时机**：dev 启动时 `AutoMigrate` 自动建（`data_service.yaml:173`）；prod 由 data-service-migrate Job 建。
- **TiDB 适配**：
  - 沿用 `NONCLUSTERED` 加 `SHARD_ROW_ID_BITS`（`schema.go:65-68`）；（2026-10-12 G1a 起 `player_route` 改为聚簇 `(route_shard, player_id)`、不再用 SHARD_ROW_ID_BITS，见 §11.2「第 1a 批热点修复」）
  - 锁定读以 `tidb_txn_mode=pessimistic` 为前提，同 A15（`player-storage-placement.md:405`、`:428`）；
  - CHECK 只在 MySQL ≥ 8.0.16、TiDB ≥ 7.2 生效（`schema.go` 的 id_segment 注释），应用层校验不能省。TiDB 还要**建表前**打开 `tidb_enable_check_constraint`；且 G1a 之后 `chk_route_shard` 关系到 `player_id` 唯一性，route store 建池探针会拒绝 CHECK 不生效的库。

### 3.2 建表语句（字段类型按 AGENTS.md §4：GUID 与时间戳用 uint64）

> 2026-10-12 G1a(§11.2「第 1a 批热点修复」)起,下面是**五张表**的定稿形状(代码真源是 `go/data_service/internal/store/schema.go` 的 bootstrap DDL;本块只为可读,两者不一致时以代码为准)。改动:`player_route` 主键改聚簇 `(route_shard, player_id)` 并加片前缀索引;`route_meta` 拆出常写的 `route_reconcile`;全部显式排序规则。
> 2026-10-13:`route_shard` 的模数由 16 改为 **64**(容量模型 [capacity-model-hok-scale.md](./capacity-model-hok-scale.md) §4.1),见 §11.2「行片 16 → 64」。

```sql
-- 每个玩家一行:home 与落点同行,原「两键要么一起写、要么都不写」(router.go:140-146) 退化成单行原子
CREATE TABLE IF NOT EXISTS `player_route` (
  `route_shard`       tinyint unsigned NOT NULL COMMENT 'pb:10', -- = player_id % 64,应用按 placement.RouteRowShard 算,CHECK 钉住
  `player_id`         bigint unsigned NOT NULL COMMENT 'pb:1',
  `home_zone`         int unsigned    NULL     COMMENT 'pb:2',  -- NULL = 无归属(删号墓碑 / 只有落点)
  `storage_id`        int unsigned    NULL     COMMENT 'pb:3',  -- NULL = 无落点记录
  `placement_version` bigint unsigned NULL     COMMENT 'pb:4',
  `frozen_run_id`     varchar(64) CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT 'pb:5',
  `rev`               bigint unsigned NOT NULL COMMENT 'pb:6',  -- 本行任何列变化 +1;仅供缓存排序
  `updated_by`        varchar(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' COMMENT 'pb:7',
  `created_at_ms`     bigint unsigned NOT NULL COMMENT 'pb:8',  -- 取库时钟
  `updated_at_ms`     bigint unsigned NOT NULL COMMENT 'pb:9',  -- 取库时钟,对账增量扫描用
  PRIMARY KEY (`route_shard`, `player_id`) /*T![clustered_index] CLUSTERED */,
  KEY `idx_route_home`    (`home_zone`, `route_shard`, `player_id`),
  KEY `idx_route_storage` (`storage_id`, `route_shard`, `player_id`),
  KEY `idx_route_updated` (`route_shard`, `updated_at_ms`, `player_id`),
  CONSTRAINT `chk_route_shard`  CHECK (`route_shard` = `player_id` % 64),  -- player_id 唯一性靠它,见下
  CONSTRAINT `chk_route_home`   CHECK (`home_zone` IS NULL OR `home_zone` BETWEEN 1 AND 999999),
  CONSTRAINT `chk_route_place`  CHECK ((`storage_id` IS NULL) = (`placement_version` IS NULL)),
  CONSTRAINT `chk_route_ver`    CHECK (`placement_version` IS NULL OR `placement_version` >= 1),
  CONSTRAINT `chk_route_frozen` CHECK (`frozen_run_id` IS NULL OR `storage_id` IS NOT NULL),
  CONSTRAINT `chk_route_rev`    CHECK (`rev` >= 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='player_route';

-- 合服围栏真源。按 (zone, shard) 存 N 份相同内容:建角只锁一个分片,获取围栏锁全部分片
CREATE TABLE IF NOT EXISTS `zone_merge_fence` (
  `zone_id`       int unsigned NOT NULL,
  `shard`         tinyint unsigned NOT NULL,                  -- 0..N-1,N 由配置给,默认 8
  `run_id`        varchar(64) CHARACTER SET ascii COLLATE ascii_bin NULL,  -- NULL = 未封锁
  `purpose`       varchar(16) CHARACTER SET ascii COLLATE ascii_bin NULL,  -- merge / unmerge
  `src_zone`      int unsigned NULL,
  `dst_zone`      int unsigned NULL,
  `expires_at_ms` bigint unsigned NULL,                       -- 库时钟;生效判据 run_id 非空且未过期
  `holder_json`   varchar(1024) NULL,                         -- 原样存 mergeInProgressValue,排障用
  `fence_epoch`   bigint unsigned NOT NULL DEFAULT 0,
  PRIMARY KEY (`zone_id`, `shard`) /*T![clustered_index] CLUSTERED */,
  CONSTRAINT `chk_fence_pair` CHECK ((`run_id` IS NULL) = (`expires_at_ms` IS NULL))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='zone_merge_fence';

-- 合走标记,永久;只有 unmerge 按 (src, dst, run_id) 删。契约同 tools/merge_zone/merged_into.go:11-20
CREATE TABLE IF NOT EXISTS `zone_merged_into` (
  `src_zone` int unsigned NOT NULL, `dst_zone` int unsigned NOT NULL,
  `run_id` varchar(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `merged_at_ms` bigint unsigned NOT NULL,
  PRIMARY KEY (`src_zone`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='zone_merged_into';

-- 开关表,单行(id=1):库世代、可写开关。很少写(1b 的冻结 / 回档恢复),每次 W1 普通读 writable
CREATE TABLE IF NOT EXISTS `route_meta` (
  `id` tinyint unsigned NOT NULL,
  `generation` varchar(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,  -- 建表时随机生成;回档恢复流程换新
  `writable` tinyint unsigned NOT NULL DEFAULT 1,         -- 0 = 全体映射写者拒写
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='route_meta';

-- 对账表,单行(id=1):1b 对账 leader 的租约与水位,每拍写;第 1a 批只由迁移种行
CREATE TABLE IF NOT EXISTS `route_reconcile` (
  `id` tinyint unsigned NOT NULL,
  `leader` varchar(96) CHARACTER SET ascii COLLATE ascii_bin NULL,  -- 与 updated_by 同源的 ASCII 标识
  `lease_until_ms` bigint unsigned NULL,
  `watermark_ms` bigint unsigned NOT NULL DEFAULT 0,      -- leader 每拍写库时钟,用来发现回档
  `cache_verified_ms` bigint unsigned NOT NULL DEFAULT 0, -- 最近一次确认 Redis 身份未变的时刻
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='route_reconcile';
```

**为什么 route_shard(G1a)**:号段发的 `player_id` 在每个 login 段内严格递增,只按 `player_id` 排序的主键 / 索引在 TiDB 上开服首日始终只写同一个尾部 Region(首日数据量到不了按大小分裂的阈值)。聚簇 `(route_shard, player_id)` 加片前缀索引,把每条索引拆成 64 条尾部,再由 `route_tool tidb-prepare` 预切到不同 Region。代价:`player_id` 的唯一性改由「主键 + `chk_route_shard`」共同保证,所以 route store 建池时实测这条 CHECK 生效(探针 P4),不生效就拒建 store。细节见 §11.2「第 1a 批热点修复」。

**为什么拆 route_meta(G1a)**:每次 W1 都普通读 `writable`,而 1b leader 每 1s 写水位、每 3s 续租;两者同在一行时,TiDB 的读会被更早的未提交写锁间歇拖住,常写的表也没法设成缓存表。拆开后开关表一年写不了几次。

**排序规则(G1a)**:只写 CHARSET 时 MySQL 默认 `utf8mb4_0900_ai_ci` / `ascii_general_ci`,TiDB 默认 `utf8mb4_bin` / `ascii_bin`,比较语义会分叉;这些列都是标识符,显式写 `utf8mb4_bin` / `ascii_bin`。

**"真的没有记录"在库里的定义**：没有这一行，或者 `home_zone` 与 `storage_id` 都是 NULL。

**为什么拆 rev 和 placement_version**：`placement_version` 仍然只在搬库切换时 +1（P-2，`player-storage-placement.md:60`）。但冻结、解冻、合服改 home 也必须让缓存看出变化，所以另设 `rev`。

**围栏为什么分片**：两份评审都指出，一行围栏会把同一 zone 的建角串行化。TiDB 没有真正的共享锁，所以用分片代替 `FOR SHARE`。N=1 就退化成 A 的单行设计。表结构从第一天就带 `shard` 列，以后调 N 不用改表。TiDB 上另按 zone 预切(`route_tool tidb-prepare`,G1a),否则整表「zone 数 × N」行落在一个 Region 上。

### 3.3 Redis 键（副本，仍在 mapping Redis DB 0）

| 键 | 值 / TTL | 说明 |
|---|---|---|
| `player:zone:{id}` | 格式不变，无 TTL | 由数据库派生；旧读方照常可用 |
| `player:placement:{id}` | `"{s}:{v}[:frozen:{run}]"` 不变，无 TTL | 编解码仍以 `go/shared/placement` 为唯一出处，merge_zone 镜像一份（`player-storage-placement.md:429`） |
| `player:maprev:{id}` 新增 | 十进制 rev，无 TTL | 只用于 set-if-newer 排序和审计，旧读方不认识也不受影响 |
| `merge:in_progress:{zone}` | 数据库围栏的镜像；值是 holder_json（含 run_id、expires_at_ms）；**改为无 TTL** | 读方判据不变，仍是"键存在即封锁"（`router.go:85`）。过期由对账循环按库时钟删。镜像只可能多封锁，不会少封锁 |
| `merge:merged_into:{src}` | 镜像，永久，**不停写** | 评审 1：P6 之后只剩库里一份会被库回档带走 |
| `mapping:route:ready` 新增 | JSON `{generation, redis_run_id, master_replid, set_at_ms, watermark_ms}`，无 TTL | 存在即"副本完整可信"。只由对账 leader 在全量预热或修补完成后写 |
| `db:capability:zone:{z}` | 不变，90s 心跳 | **不落库、不进副本、恢复时不手写**（`placement_route.go:369-389`） |

**硬前提：mapping 实例必须是 noeviction。** 否则会出现"标记还在、单键被淘汰"，缺键被误当成缺席。对账 leader 用 `CONFIG GET maxmemory-policy`（被禁用时改用显式配置项）和 `INFO stats` 里的 `evicted_keys` 自检，任一不满足就拒绝挂标记（fail-closed）。
- K8s 单实例已经要求 noeviction（`deploy/k8s/manifests/infra/redis.yaml:137-139`）。
- dev 的 compose 是 `allkeys-lfu`（`deploy/docker-compose.yml:229-231`），这正是 mapping Redis `127.0.0.1:6379`（`data_service.yaml:61-63`）。必须先改，见 §9 第 0 批。

---

## 4. 写入点改造：顺序与原子性

### 4.1 公共规则

1. **事务**：route store 用独立连接池，DSN 设 RC，与 A15 同口径（`player-storage-placement.md:428`）。
2. **全局加锁顺序**：
   1. 普通读 `route_meta.writable`，为 0 就拒写；
   2. `zone_merge_fence`（zone 升序，再 shard 升序）；
   3. `zone_merged_into`；
   4. `player_route`（player_id 升序）。

   错误 1213/1205 有界重试 3 次加抖动，必须落在调用方预算内。
3. **判定只用加锁读（`FOR UPDATE`）的结果。** 新行用 `INSERT … ON DUPLICATE KEY UPDATE player_id=player_id`，**不用 `INSERT IGNORE`**：它会把 CHECK 违例降级成 warning（评审 1）。影响 0 行之后再加锁读查不到行，按内部错误处理。
4. **发布**：
   - COMMIT 后，在**同一条连接**上用 pipeline 发 `EVALSHA 发布脚本` 和 `WAITAOF numlocal numreplicas timeout`。
   - 取值：dev、kind 单实例是 `WAITAOF 1 0`；redis-ha 是 `WAITAOF 1 1`。镜像是 `redis:7.2`（`redis.yaml:83`、`redis-ha.yaml:684`），支持 WAITAOF。
   - 为什么是 WAITAOF 而不是 WAIT：评审都指出 WAIT 在 0 副本的单实例上无效；WAITAOF 能确认本地 AOF 已刷盘。
   - 为什么必须同一连接：两份评审都指出，WAIT 类命令只等本连接此前的写。
   - **待核实**：go-zero 的 `redis.Redis` 能否同连接 pipeline 发 WAITAOF。
5. **发布失败**：重试 3 次后，调用方按"未完成"失败。缓存这时只是落后，由对账增量推送补齐；按"提交即安全"，晚看到是安全的。
6. **防绕过**：所有写都收口在一个 `commitAndPublish` helper（Go 侧放 `go/shared`，merge_zone 镜像一份），另加 grep 测试，禁止在 helper 之外出现 `UPDATE player_route` 或直接 SET 映射键。

### 4.2 发布脚本（set-if-newer）

```lua
-- KEYS: 1=player:zone:{id} 2=player:placement:{id} 3=player:maprev:{id}
-- ARGV: 1=rev(十进制,无前导零) 2=home(''=DEL) 3=placement(''=DEL)
local cur = redis.call('GET', KEYS[3])
if cur and (#cur > #ARGV[1] or (#cur == #ARGV[1] and cur >= ARGV[1])) then
  return {0, cur, redis.call('GET', KEYS[1]) or '', redis.call('GET', KEYS[2]) or ''}
end
if ARGV[2] == '' then redis.call('DEL', KEYS[1]) else redis.call('SET', KEYS[1], ARGV[2]) end
if ARGV[3] == '' then redis.call('DEL', KEYS[2]) else redis.call('SET', KEYS[2], ARGV[3]) end
redis.call('SET', KEYS[3], ARGV[1])
return {1, ARGV[1]}
```

- **rev 比较**：先比长度、再比字典序，不经 Lua 的 double，与 `player-storage-placement.md:289` 担心的精度问题同源。
- **返回 0 时**（采纳评审 2 的修补）：
  - 缓存 rev 大于本次：已被更新的值取代，正常；
  - 缓存 rev 等于本次、值也相同：已确认；
  - 缓存 rev 等于本次、值不同：说明有绕过 helper 的写者，或库回档后 rev 被复用。报 `route_publish_total{result="conflict"}`，写者按失败返回。
- **多键脚本成立的前提**：mapping Redis 是单实例（`router.go:311-314`）；三把键都显式写进 KEYS。

### 4.3 逐写入点

| 写入点 | 改造后的顺序 | 与现状相比 |
|---|---|---|
| **W1 RegisterPlayerZone**（`router.go:152-237`），含 debug_import | 1) 确保围栏分片行存在（ODKU no-op）。2) BEGIN。3) 读 writable。4) `SELECT … FROM zone_merge_fence WHERE zone_id=? AND shard=player_id%N FOR UPDATE`，生效就回 3（`ErrZoneMergeInProgress`）。5) 按 ODKU 插入 rev=1；影响 0 行就加锁读后分支：同 home 幂等；异 home 冲突；home 为 NULL 时补 home，已有落点保留并打 ERROR，等价于原返回 2（`router.go:195-201`）。6) COMMIT。7) 发布 + WAITAOF；幂等分支也要重发，用来修补上次失败的发布 | 围栏复核与写入同事务。获取围栏要锁全部分片，必须等登记事务释放分片锁，`router.go:165-172` 防的窗口仍然关着。删掉 Redis 的 EXISTS 预检（`router.go:155-161`）。发布失败回 Unavailable，login 照旧 fail-closed 并释放名字（`createplayerlogic.go:257-275`）。**不新增可用性依赖**：6c 名字登记本来就同步写同一个库（`createplayerlogic.go:231-236`） |
| **W2 RemapHomeZoneForMerge**（`router.go:488-534`） | **停用**，回 FailedPrecondition("use tools/merge_zone") | 仓库内无调用方；无条件 SET 的危害见 `tools/merge_zone/merge_run.go:836-837` |
| **W3 DeletePlayerZoneIfLocked / W4 DeletePlayerZone** | W3 的 `DeleteZoneMapping` 分支在删号设计定下来之前回 FailedPrecondition；W4 降为测试专用 | 锁 token 在 Redis、删除在库里，两者无法原子；生产无调用方（`rollback_logic.go:1064-1071`）；删号与落点的关系本来就待定（`player-storage-placement.md:383`） |
| **W12 合服围栏**（`fence.go:103-210`，unmerge 同样使用） | **获取**：一个事务，按升序锁 src、dst 的全部分片 → 任一生效且 run_id 不是自己就拒绝 → UPDATE 写入 run_id、`expires_at_ms = 库时钟 + max(timeout+30m, 1h)`（`fence.go:111-114`）→ COMMIT → 写镜像（无 TTL）+ WAITAOF → **之后**才收集清单（`merge_run.go:171-174`）。**续期**：`UPDATE … WHERE run_id=?`，影响 0 行就把围栏标成"已丢失"，之后每批事务的围栏校验都失败、中止（原来只打 WARN，`fence.go:176-180`）。**释放**：先清库，再按 run_id 用 Lua 删镜像；删失败由对账删 | 过期只看库时钟，不看工具本机时钟 |
| **W6 markMergedInto**（`merged_into.go:93-119`） | 一个事务：校验围栏属于本次运行 → `SELECT … FROM zone_merged_into WHERE src_zone=? FOR UPDATE`：无行就插入；同一 dst 就更新 run_id；其它 dst 就拒绝 → COMMIT → 写镜像 + WAITAOF。必须先于 W5 | 原来"先 GET 再 SET、只靠围栏兜住"的非原子写由此关闭 |
| **W5 合服步骤 5**（`merge_run.go:586-617`、`:839-951`） | 每 500 人一个事务：锁 src、dst 围栏并要求 run_id 等于清单值（**新增 fencing token**，挡住 TTL 过期后被人接手的僵尸工具；原 Lua 不校验，`merge_run.go:839-848`）→ 玩家行按升序 FOR UPDATE → 按 `classifyMappingValue` 同口径分类 → `UPDATE … SET home_zone=dst, rev=rev+1 WHERE … AND home_zone=src`，影响行数必须等于要改的人数 → COMMIT → 整批发布 + WAITAOF | 完成判据：原 `complete()`（`merge_run.go:894-901`）**加上**全员发布已确认 |
| **W7 pin 合服钉落点**（`placement_ops.go:661-728`） | 每批一个事务：围栏属于本次运行 → 玩家行 FOR UPDATE → `decidePin` → `UPDATE storage_id=src, placement_version=1 … WHERE storage_id IS NULL AND home_zone=src` → COMMIT → 发布 | 原来不传围栏键，现在正向校验"围栏属于我"，更严格 |
| **W8 unmerge 5' / 5'a**（`unmerge.go:391-433`、`merged_into.go:142-170`） | 每批一个事务：围栏属于本次撤销 → 条件 UPDATE，home 从 dst 改回 src → COMMIT → 发布；5'a 在持有围栏的事务里 `DELETE … WHERE src_zone=? AND dst_zone=? AND run_id=?`，再删镜像 | 原来是 pipeline 普通 SET，不是 CAS（`unmerge.go:426`）。"键不存在就跳过"现在是权威缺席，不可能是丢键 |
| **W9 backfill_home_zone**（`backfill_home_zone.go:185-245`） | 每批一个事务：锁任一围栏分片，要求未生效（**新增**，原实现不看围栏，`:229-230`）→ `zone_merged_into` 有该区就拒绝（这个标记现在持久，Redis 丢了也拦得住）→ 只给"无行"的人 ODKU 插入，给"home 为 NULL"的人补 home → COMMIT → 发布 | 定位降级：只给"从未登记过的存量号"补 home，不再是 Redis 丢失后的恢复手段 |
| **W10 -mode pin-placement**（`pin_placement.go:64-121`） | 每批不超过 200 人一个事务：围栏分片未生效 → 无 `merged_into` → `SELECT … WHERE home_zone=N AND storage_id IS NULL … FOR UPDATE`（走 `idx_route_home`）→ UPDATE → COMMIT → 发布 | 钉的值等于 home，有效落点不变，提前或延后可见都安全 |
| **W11 relocate**（R1、R5、解冻、abort） | **只换 `relocatePlacements` 接口的实现**（`relocate.go:38-43` 这条现成接缝），`decideFreeze`、引擎都不改。`cas` 每批一个事务：锁相关 home 的围栏分片（生效回结局 3）→ 玩家行 FOR UPDATE → 比对"编码后的当前值 vs 期望值、当前 home vs 期望 home"→ 写列、rev+1 → **新增断言**：稳定态的新 version 不得小于当前 version（守 P-2）→ COMMIT → 发布 + WAITAOF → 只有确认了的人才回 Written。R1 必须确认之后才进 R2 | **不需要 B 的 R5a/R5b 拆分**：库先提交，缓存只可能落后。R5 的发布丢了，缓存停在 R1 已确认的冻结态，go/db 延后写，安全；对账再把 `T:v+1` 推上去，R4 已比对过，也安全 |
| **W13 能力标记**（`go/db/db.go:143-157`） | 不改 | 见 §1 第 3 条 |

---

## 5. 读路径，以及"缓存未命中 ≠ 无记录"

### 5.1 统一判据（取代"键不存在 = 无记录"）

| 同一次 MGET 或同一段 Lua 读到的 | 含义 | 处理 |
|---|---|---|
| 就绪标记在，映射键在 | 权威值 | 直接用；畸形值照旧 fail-closed（`player-storage-placement.md:77`） |
| 就绪标记在，映射键缺 | **权威缺席** | 沿用今天的缺席语义 |
| **就绪标记不在** | 副本不可信，即"未命中" | **绝不当缺席**：data_service 回源数据库；go/db 和 guild 按故障 fail-closed |

**为什么"标记在 + 键缺"可以当真缺席**：
- 所有写都是"库先提交，再发布并 WAITAOF 确认"，依赖步骤才推进；
- 实例 noeviction，有自检；
- Redis 整体丢失时标记跟着丢；
- 部分丢失由对账在一拍之内撤掉标记；
- 只有全量预热或修补完成后才挂回标记。

**残留窗口**（写进风险）：
- 已提交但还没发布的状态，对读方暂时不可见。对 W1 而言调用方还没成功、玩家进不了场；对工具而言依赖步骤还没开始。
- Redis 重启到 leader 发现之间（不超过一拍），读到的只会是"没确认过的发布被回退"，按"提交即安全"等价于"晚看到"。

### 5.2 逐读方

| 读方 | 改造 | 标记不在时 |
|---|---|---|
| **R1 go/db 选库与复核**（`placement_route.go:89-99`、`:242-254`） | MGET 从 2 键改为 3 键（placement、zone、ready）；`decideWritePlacement`、`decideReadPlacement`、`recheckPasses` 这些纯函数**一格不改**（`:158-226`）；由开关 `Placement.RequireRouteReady` 控制，缺省 false | 新 outcome 为 `route_not_ready`。**写任务**延后，加入 `retryBudgetExempt`（`:357-367`）不耗预算，避免存盘进死信。**读任务**照常耗预算，理由同 `:352-356` 的"落点库打不开只豁免写"。复核遇到不就绪按可重试处理。**不连全局库**：Redis 整体丢失时游标、排序锁、重试队列也在同一实例一起丢，回源换不来可用性（方案 C 的论证）；同时避开 strict 库名白名单（`k8s_deploy.ps1:3452-3455`）和第三份 SQL |
| **R2 scene_manager → GetPlayerHomeZoneAndMergeFence**（`router.go:299-324`） | Lua 开头加 `EXISTS mapping:route:ready`，不在就回 `{'', -2}` | 回源用一条 SQL：`player_route` LEFT JOIN `zone_merge_fence`（shard 0），home 与围栏仍是同一快照（`router.go:286-289` 的要求），然后用发布脚本回填。查询失败回 Unavailable，**绝不回 NotFound**；NotFound 的文案契约不变（`router.go:247-256`）。scene_manager 收到 Unavailable 走既有的拒绝分支（`home_zone.go:152-157`），不会把 gate zone 当 home |
| **R3 login**、**R5 trade / match / friend**：都经 `BatchGetPlayerHomeZone`（`router.go:366-390`） | MGET 里并入 ready；顺带修掉畸形值被静默跳过（`:383-386`），改为整批报错 | 一条 `WHERE player_id IN (…)` 回源；失败整批回错，各调用方照现有故障分支处理 |
| **R4 guild 直读围栏**（`go/guild/internal/logic/merge_fence.go:62-73`，经济、活动也经由它） | 改 MGET `[merge:in_progress:{z}, mapping:route:ready]` | 标记不在就返回错误，调用方按接口契约当封锁处理。这也覆盖了评审 1 指出的"guild 内部调用不经单查"那条路径 |
| **R6 data_service 内部**（`ClientForPlayer`、snapshot、debug_fetch） | 单 GET 改成 MGET zone + ready | 回源；配了 DevRedis 时今天实际 0 次（`player-storage-placement.md:335`） |
| **R7 全量枚举**：`GetAllPlayerIDsInZone`（`router.go:440-472`）、`collectPlayerIDsWithHomeZone`、`collectStoragePlayers`、`data_consistency_check` | **一律读库**，走 `idx_route_home` / `idx_route_storage` 键集翻页；修掉吞 GET 错误（`router.go:452-454`） | 不受缓存影响。完整性：登记事务持有分片锁直到提交，围栏提交之后的扫描一定能看到此前所有登记 |
| **R8 merge_zone 其它读**：清单、`placementsExisting`、`manifestPlayersBackAt`、`redisMappingPresence`（`merge_run.go:741`）、`verify:manifest_mapping`（`audit_checks.go:261`）、`data_consistency_check/main.go:462` | 全部读库（评审 1 补全的清单）。新增 `verify:route_cache`：开服前逐人比对清单玩家的"缓存 rev = 库 rev，且值相等" | 满足 P-6：缓存必须在 dst 开服之前反映新 home |

### 5.3 围栏、merged_into、冻结态：都落库

| 对象 | 真源 | Redis | 理由 |
|---|---|---|---|
| 合服围栏 | `zone_merge_fence`，过期看库时钟 | 镜像，无 TTL，对账负责删 | 围栏与登记必须同一事务域复核（`router.go:77-79`）；Redis 丢失时围栏不能跟着消失 |
| merged_into | `zone_merged_into` | 永久镜像 | Redis 整体丢失时它是唯一能挡住"回填钉回源区"的东西（`merged_into.go:11-20`、runbook `:734`） |
| 冻结态 | `player_route.frozen_run_id` | 落点值里的 `:frozen:{run}` | relocate 崩溃后续跑靠它认领（`relocate.go:253-266`） |
| 能力标记 | **只在 Redis** | 90s 心跳 | 必须保持临时（`placement_route.go:369-379`）；Redis 丢失后 30s 内自动补回 |

### 5.4 新增：data_service 对账 leader

leader 由 `route_meta` 租约选出（10s 租期，3s 续一次）。每拍 1s：

1. 读 `INFO server` 的 `run_id` 和 `INFO replication` 的 `master_replid`，再读就绪标记。
2. **标记在，但身份变了**（Redis 重启或切主）：
   - 先 DEL 标记（关门）；
   - 推送 `updated_at_ms >= cache_verified_ms - 60s` 的行（set-if-newer）。60s 余量覆盖 `min-replicas-max-lag 10`（`redis-ha.yaml:252-254`）和时钟偏差；
   - 全量同步围栏和 merged_into 镜像（只有寥寥几个 zone）；
   - 写回标记。
3. **标记不在**：
   - 自检 noeviction；
   - 全量预热：按主键键集翻页、pipeline 写入，set-if-newer 保证与在线写者并发时收敛；
   - 同步围栏和 merged_into；
   - 核对 run_id 在预热期间没变，再写标记。
4. **每拍还做**：
   - 围栏镜像对账：补齐缺失的生效围栏；删除库里已失效的镜像，先查库确认、再按 run_id 删；
   - 写 `watermark_ms`；
   - **回档检测**：库里的 watermark 小于标记里记下的 watermark 减容差，就把 `writable` 置 0、告警，停止库到缓存的推送（缓存才是较新的一边）。
5. **每 30s** 推送最近 N 分钟变化的行，自动补齐发布失败的那部分。

> 2026-10-18 注(第 1b-1 批):租约与水位已拆到 `route_reconcile`(G1a);就绪标记的值格式、CAS / CAD 脚本与冻结 / 反向修库 / 解冻的库原语见 §11.2「第 1b-1 批」,以那里为准。leader 本身属于第 1b-2 批,未开始。本节原文不回改。

---

## 6. 故障行为与恢复

| 故障 | 行为 | 恢复步骤 |
|---|---|---|
| **Redis 整体丢失**（FLUSHALL、换空实例、PVC 丢失） | 标记随之消失。go/db 写延后、不丢盘；data_service 的读回源，进场判定照常；guild 的建帮、经济、活动按封锁拒绝；建角照常（库先提交，再发布到新实例）。**已提交的映射零丢失** | **自动**：leader 全量预热后挂标记（RTO = 全量预热时长，100 万行级估计几十秒到分钟，未测）。运维只需：照现有流程处理会话、锁、游标的丢失（全服维护）；确认标记已挂、`-route-audit` 为 0。原 runbook §9.2 的手工重放（`merge-zone-runbook.md:732-750`）不再需要。**过渡期例外**：读方开关打开（第 3 批）之前，旧读方仍会把缺键当缺席，必须保留 §9.2 第 0 步"先停 go/db"，等预热完成再起 |
| **Redis 部分丢失**（AOF everysec 丢约 1s，`redis.yaml:95-97`；切主丢尾部，`redis-ha.yaml:24-27`） | 经 WAITAOF 确认的发布不丢；丢的只是没确认的发布，按"提交即安全"无害。leader 在一拍内发现身份变化，撤标记、修补、挂回 | 自动。残留：哨兵提升了落后的副本时，已确认的写也可能丢，由第 2 步按 `updated_at_ms` 修补 |
| **数据库不可用** | 稳态读全在 Redis，热路径（存盘、进场、登录列表）不受影响。写入 fail-closed：建角失败，但 6c 名字登记本来就依赖同一个库；合服、搬库、钉落点工具拒绝启动，或带着围栏中止。标记保持（没有写能绕过库，副本仍然正确）；对账暂停 | 库恢复后自动续上；数据库不可用期间禁止开始合服或搬库（写进 runbook） |
| **数据库回档**（PITR；K8s 下全局库与 zone 库同一个 mysql 实例，`k8s_deploy.ps1:3443`、`:3511-3512`，zone 回档会连带回档全局库） | leader 发现 watermark 回退：`writable=0`，全体写者拒写；**不往缓存推旧值**；缓存与标记保持不动，读照常 | 1) `data_service -route-reconcile-from-cache`：缓存 maprev 大于库 rev 或库里无行时，把缓存值连同 rev 写回库；围栏和 merged_into 从镜像补回。2) `-route-audit` 两个方向都为 0。3) 换新 `generation`，`writable=1`。`k8s_zone_rollback.ps1` 的流程里要加入这一步 |
| **Redis 与数据库同时丢失** | 全服 fail-closed（不会写错库） | 先恢复数据库备份；按 runbook §9.2 的清单重放，补齐备份之后的合服和搬库（重放目标改为写库）；跑 backfill；最后预热。清单"永久归档"的纪律保留（`merge-zone-runbook.md:720`） |

> 2026-10-18 注(第 1b-1 批):「数据库回档」一行的恢复步骤以 §11.2「第 1b-1 批」与 runbook §9.4 为准:命令是 `data_service -f <yaml> -route-tool freeze` → `reconcile-from-cache` → `audit -frozen-recovery` → `unfreeze -confirm-audited`;**不换 generation**(偏离 B1);第 1b-2 批之前没有就绪标记,解冻要显式 `-cache-was-not-verified-acknowledged` 并先按 §9.2 清单核对。leader 自动发现回档(写 `writable=0`)属于第 1b-2 批,在那之前回档要由运维发现并手工 freeze。本表原文不回改。

---

## 7. 存量回填、上线顺序与回退

**前置条件**：
1. 先按 `docs/handoff/placement-reverify-checklist-20261012.md` 复验当前 HEAD 的落点代码（`player-storage-placement.md:4`），本方案不能叠在一个未验证的基线上。
2. 完成第 0 批（§9）。

| 阶段 | 内容 | 新旧并存的说明 |
|---|---|---|
| **P0 建表** | migrate 建 4 张表，写入 `route_meta(id=1)` | 只新增，行为不变 |
| **P1 冻结窗口开始，导入存量** | 禁止合服、unmerge、relocate、pin-placement、backfill、W2，代价为零：从未执行过（`player-storage-placement.md:5`）。`data_service -route-import-from-redis`：SCAN `player:zone:*`、`player:placement:*`、`merge:merged_into:*`，逐批 upsert；发现生效的 `merge:in_progress` 就中止；畸形值报告并跳过，交人工。之后跑 `-route-audit -against-redis` | Redis 仍是唯一提交点 |
| **P2 data_service 切到库先写**（滚动） | W1 改为库先写，W2、W3 停用 | 混跑期间旧副本只写 Redis。登记总是新 id，不会出现异值并发；窗口内没有围栏，两种副本的围栏口径不同也无妨 |
| **P3 二次导入，挂标记** | 全部副本切换后，**再跑一遍导入 + 审计，直到为 0**，补上旧副本在滚动期间写的登记（评审 2 的修补，取代 A 的 shadow 方案）。然后启动对账 leader：全量预热 maprev，挂标记 | 读方还不要求标记，行为不变 |
| **P4 新版 merge_zone** | 工具全部改为库先写（新参数 `-route-db-dsn`）；runbook §9.2 改写；解除冻结窗口 | dev_tools 每次从 HEAD 编译（`player-storage-placement.md:427`）；旧二进制禁用，写进 runbook |
| **P5 读方要求标记** | go/db 逐 zone 打开 `RequireRouteReady`；data_service 读路径和回源；guild | 先有标记、后要求；反过来会全服 fail-closed |
| **压测** | P2 之后测建角（注册阶段 p99、1213/1205 计数，以此定 N）；P5 之后测存盘（go/db MGET 多一个键）。按 AGENTS.md §6 出对比表 | 落点 MGET 本身也从没压测过（`player-storage-placement.md:379`），合并成一轮 |

**与 §13 的衔接**（`player-storage-placement.md:431-450`）：
- P0–P5 是生产环境执行 §13 第 4 步（pin 合服、relocate、`PinOnCreate`）的前置；
- 是 `PinOnCreate` 指向全局库的**硬前置**（§4.4 的缺口，`:116`）；
- 第 5 步（`Required=true`）照原顺序，放在其后。

**回退**：
- **顺序固定为先撤读方、后撤写方**（评审 2）：先关 `RequireRouteReady`，再回退写者。
- data_service 退回只写 Redis 的旧版：Redis 键格式没变，一直是完整副本，旧版照常工作；库会落后，重新前进前必须**重新冻结工具，再跑一次 P3**。
- P4 之后旧版 merge_zone 一律禁用；需要回退就同时回到冻结窗口。
- 回退时不删表。
- §13 的 go/db 回退禁令照旧有效，与本方案正交。

**双版本**（AGENTS.md §12）：映射存储属于服务端内部实现，客户端契约不变，两版也不共用 MySQL。Java 版按自己的方式做等价的"库为真源 + 完整副本"，并在 `PARITY.md` 登记；本仓库的 PROGRESS 条目注明 Java 状态（目前待做）。

---

## 8. 被否决的方案、已吸收的修补、仍未解决的风险

### 8.1 被否决或部分否决

| 方案 | 结论 | 理由 |
|---|---|---|
| **B：Redis 仍为真源 + outbox + applier + guardian** | 否决 | 1) 评审 2 指出致命问题：guardian 以"epoch 不一致"为触发条件，整体丢失也命中，只回写尾部行就重新开库，大量玩家被当成真缺席。2) 评审 1：全局库回档时修尾方向反了，会把旧值写回 Redis。3) 单一有序流有队头阻塞：别的 zone 合服产生约 20 万条 outbox，会让建角超时。4) 机制最多，没有回应"Redis 只做缓存"。**吸收**：Redis 身份检测、就绪标记、库租约 leader、FLUSHDB 防护 |
| **C：行戳下界重建** | 否决（包括 A+C） | 证明不了"不存在"：首存之前的玩家、冻结态、围栏、merged_into 都无法重建。要改最热的表 `player_database`，牵动 C++、所有库的 migrate 和 `alignedPlayerColumns` 门禁（`player_rows.go:463-483`），属于 YAGNI。RTO 未知。**吸收**：不改键格式、加 maprev；"go/db 不回源"的论证；"行为什么不能当真源"的分析 |
| **A 原文** | 采用骨架，四处改动 | 1) 新键 `player:route` + MirrorLegacy + P6 → 保持原格式加 maprev，上线和回退矩阵简单得多。2) 每键负缓存 → 整副本就绪标记，稳态没有未命中。3) go/db 的 RouteDB 只读池 → 去掉，与 strict 白名单冲突，回源也换不来可用性。4) shadow 写库 → 库先写，加二次导入 |
| **Kafka outbox** | 否决 | Lua 无法原子地写 Kafka，写者崩溃会永久丢失副本登记（B 自己的分析） |
| **等 TiDB 再做** | 否决 | Phase 1 未定（`player-storage-placement.md:374`）；风险与玩家表放在哪无关；今天的 MySQL 全局库已有同类先例（`player_name`） |

### 8.2 已吸收的评审修补

- **回档闸门**：`route_meta.watermark_ms` / `generation` / `writable` 加从缓存反向修库，对应评审 1 的"库可信闸"。
- **切读前缺口**：二次导入 + 审计为 0 才挂标记。
- **重启和切主**：自检不依赖哨兵告警；`updated_at_ms` 建了索引。
- **围栏镜像**：去掉 TTL，用库时钟加对账清理；noeviction 写进部署契约，删掉 A 里"volatile-* 也可以"的错误说法。
- **WAITAOF**：与发布脚本同连接。
- **W1**：用 ODKU，不用 `INSERT IGNORE`。
- **建角吞吐**：围栏分片，先压测再定 N。
- **merged_into**：Redis 镜像永久保留。
- **读方清单**：补全 `merge_run.go:741`、`audit_checks.go:261`、`data_consistency_check/main.go:462`。
- **发布返回 0**：返回当前值，写者比较。
- **清库脚本**：`stress_round19.ps1:80`、`:117-124` 和 `currency_crash_window.ps1:111-130` 只清 `zone_N_db` 不清全局库，要把新表加进清单，同步改 AGENTS.md §6.2；否则 FLUSHALL 之后，残留的生效围栏会被对账重新镜像回去。
- **预算豁免**：只豁免写，读照常耗预算。
- **回退顺序**：先读方、后写方。

### 8.3 仍未解决的风险（需知情或拍板）

1. **写者纪律是正确性的根基。** 绕过 `commitAndPublish` 的写者（旧版 merge_zone、手工 `redis-cli SET`、将来的新代码）会造成"rev 相同、值不同"。只能靠 helper、grep 测试、审计告警事后发现，无法事前拦截。
2. **WAITAOF 的延迟。** 在 `appendfsync everysec` 下，本地确认的唤醒时机没有核实，最坏按 1s 估算，落在 W1 3s 预算内（`homezone.go:52-58`）。不可接受时，mapping 实例改为 `appendfsync always`，或单独起一个实例。go-zero 能否同连接 pipeline 发 WAITAOF 也待核实。
3. **WAITAOF 不等于强一致。** 哨兵提升了落后的副本时，已确认的写仍可能丢，只能靠身份检测加 60s 回看修补。
4. **noeviction 是硬前提。** dev 改成 noeviction 之后，内存打满就拒写（fail-closed）。这是与当前 OOM 防御（`docker-compose.yml:222-226` 的注释）的取舍，见 §10。
5. **Redis 重启后 go/db 要等 data_service 的 leader 挂回标记。** data_service 全挂期间，go/db 的写会一直延后（不丢盘）。这是一条新的可用性耦合，但只在 Redis 刚重启之后出现。
6. **库回档检测依赖 watermark 容差。** 回退幅度小于容差（秒级）时检测不到，靠 runbook 硬性要求"凡对全局库所在实例做过 PITR，都要跑反向修库"。
7. **同 zone 建角吞吐、分片数 N、TiDB 悲观事务下的表现都没测。**
8. **删号语义仍未定**（W3 停用，`player-storage-placement.md:383`）。
9. **Redis 整体丢失时，会话、锁、游标、`PlayerAllData` 的恢复不在本方案范围。**
10. **改动超过 30 个文件，需要按 §10.2 授权；HEAD 上的落点代码也还没复验。**

> 2026-10-18 注(第 1b-1 批):第 6 条「凡对全局库所在实例做过 PITR,都要跑反向修库」的工具与步骤已落码(未编译、未测试):`data_service -f <yaml> -route-tool freeze` → `reconcile-from-cache` → `audit -frozen-recovery` → `unfreeze -confirm-audited`,以 §11.2「第 1b-1 批」与 runbook §9.4 为准。第 1b-2 批(leader 的 watermark 检测)落地之前连容差检测都没有,这条 runbook 纪律是唯一防线。本节原文不回改。

---

## 9. 改动面清单与分批

估计合计约 45–55 个手改文件，不含生成产物，超过 AGENTS.md §10.2 的 30 文件门槛。**开工前必须取得授权**，并按 §11.5 先在 `player-storage-placement.md` 新增"映射持久化"一节，改写 §4.4、§11.4。建议分 4 批，每批可以独立审阅、独立回退。

| 批 | 内容 | 主要文件 |
|---|---|---|
| **0 先修现存隐患**（约 5 个文件，可以立即做，与方案无关） | dev 的 mapping Redis 改 noeviction（或单独起一个实例）；zone 回档脚本的 FLUSHDB 加防护：目标实例存在 `player:zone:*` 或 `merge:*` 键就拒绝，除非显式确认（默认 DB 0，`k8s_zone_rollback.ps1:107-111`、`:803-813`）；BatchGet 畸形值改为报错；`GetAllPlayerIDsInZone` 不再吞错误 | `deploy/docker-compose.yml`、`tools/scripts/k8s_zone_rollback.ps1`、`go/data_service/internal/routing/router.go` 及其测试 |
| **1 存储底座 + data_service 库先写**（约 15–18 个文件，覆盖 P0–P3） | 4 个 proto message 并重新生成（Go 与 C++ proto 库）；bootstrap DDL 和 `TableMessages`；`store/route_store.go`；`go/shared/placement` 增加 maprev 键、就绪标记键、发布脚本和测试向量；router 的 W1 库先写、W2/W3 停用；`commitAndPublish`；对账 leader；四个 cmd：`-route-import-from-redis`、`-route-audit`、`-route-rewarm`、`-route-reconcile-from-cache`；config、yaml、K8s ConfigMap 与契约测试（钉住 noeviction、DBName）；两份清库脚本 | `proto/common/database/rollback_database_table.proto`、`go/data_service/internal/{store,routing,config,svc}/…`、`go/data_service/data_service.go`、`go/shared/placement/*`、`tools/scripts/{k8s_deploy.ps1,stress_round19.ps1,currency_crash_window.ps1}`、契约测试 |
| **2 merge_zone 写入点**（约 15 个文件，覆盖 P4） | 新增 `route_store.go` 作 SQL 镜像；`fence.go`、`merged_into.go`、`merge_run.go`、`placement_ops.go`、`pin_placement.go`、`relocate.go`（只换 `relocatePlacements` 的实现）、`unmerge.go`、`backfill_home_zone.go`、`player_blob_migrate.go`、`audit_checks.go`（新增 `verify:route_cache`）、`main.go`、`placement_codec.go`；集成测试；dev_tools 转发与契约测试；runbook §9 重写 | `tools/merge_zone/*`、`tools/scripts/dev_tools.ps1`、`tools/scripts/tests/dev_tools_merge_zone_contract.tests.ps1`、`docs/ops/merge-zone-runbook.md` |
| **3 读方要求标记**（约 10 个文件，覆盖 P5） | go/db 的三键 MGET、`route_not_ready`、预算豁免、开关、指标；data_service 读路径与回源；guild 围栏读；`data_consistency_check`；告警 yaml；文档（落点设计新增一节、TiDB 决策 D1 标为本方案、PROGRESS、`docs/README.md`） | `go/db/internal/kafka/placement_route.go`、`go/db/internal/config/config.go`、`go/guild/internal/logic/merge_fence.go`、`tools/data_consistency_check/main.go`、`deploy/k8s/*-alerts.yaml`、`docs/design/*` |
| **Java 版** | 等价实现，在 `PARITY.md` 登记 | Java 仓库 |

**每批交给 Codex 的验证要点**：
- 受影响 Go 模块的 build、vet、test；
- data_service 和 merge_zone 的 MySQL + Redis 集成测试：发布 set-if-newer 乱序、WAITAOF 同连接、围栏分片互斥、二次导入收敛、预热与并发写收敛、回档检测；
- 至少一组 TiDB 悲观 RC 用例；
- 第 1 批要重编 C++ proto 库（`/m:1` 串行）；
- 第 1 批之后压测建角，第 3 批之后压测存盘，按 §6 出对比表。

---

## 10. 待负责人拍板

1. **方向**：采用"库为真源 + 完整副本 + 就绪标记"，即本稿方案，并按 4 批推进（超过 30 个文件，需要授权）。
2. **dev 的 Redis 淘汰策略**：改成 noeviction，内存满即拒写；还是给映射单独起一个 noeviction 实例。
3. **W2 停用、W3 的删映射分支停用**，等删号设计定下来再说。
4. **围栏分片数 N**：先按 8 落码，压测之后定。
5. **对 `appendfsync everysec` 下 WAITAOF 延迟的接受度**：不接受就给 mapping 实例单独配 `appendfsync always`。
6. **时间点**：第 0 批立即做；第 1–3 批放在生产执行 §13 第 4 步之前完成；在落点复验清单完成之前不开工。

---

## 11. 拍板与落码记录

### 11.1 负责人拍板(2026-10-12)

| 第 10 节待拍板项 | 结论 |
|---|---|
| 1. 方向 | 采用:库为真源 + 完整副本 + 就绪标记,按批推进。超过 30 个文件的改动面已获授权 |
| 2. dev Redis 淘汰策略 | 改 noeviction,与 K8s 一致,不另起实例 |
| 3. W2 / W3 | 按本稿默认:停用,等删号设计定下来 |
| 4. 围栏分片数 N | 按本稿默认:先按 8 落码,压测后再定 |
| 5. WAITAOF 延迟 | 未单独表态,按本稿默认:`appendfsync everysec` 下先用 WAITAOF,压测后再定是否改 always |
| 6. 时间点 | 「两条都做,D1 先行」:先写 D1 代码,同时出 TiDB Phase 1 的设计与实施清单 |

前置提醒:当前 HEAD 的落点代码还没按 [placement-reverify-checklist-20261012.md](../handoff/placement-reverify-checklist-20261012.md) 复验。D1 代码可以先写,但交 Codex 验证时要先跑那份清单,再跑本方案各批的验证。

### 11.2 落码进度

| 批 | 状态 | 说明 |
|---|---|---|
| 第 0 批 | 已落码(2026-10-12),**未编译、未测试** | 见下 |
| 第 1a 批 | 已落码(2026-10-12),**未编译、未测试** | 存储底座(四张表 + route store)、W1 的 db_first 开关(所有档位默认 redis)、`route_tool` 导入 / 审计、W2 / W3 停用。见下「第 1a 批」 |
| 第 1a 批热点修复(G1a) | 已落码(2026-10-12),**未编译、未测试** | TiDB 写热点:`player_route` 聚簇 `(route_shard, player_id)` + 片前缀索引、`route_meta` 拆出 `route_reconcile`、建池探针 P4、`route_tool tidb-prepare`。见下「第 1a 批热点修复」 |
| 第 1b 批 | 1b-1 已落码(2026-10-18);1b-2、1b-3 已落码(2026-10-19);**全部未编译、未测试** | (2026-10-19 补)1b-2 / 1b-3 的设计修订 M1–M5 与落码见 §13 | §9 第 1 批的剩余项:对账 leader、全量预热与 `mapping:route:ready`、回档检测与反向修库(`-route-rewarm` / `-route-reconcile-from-cache`)、`route_tool` 并进 data_service 主程序入口(K8s 上可跑)。范围以 1b 开工时的规格为准。2026-10-18 起按调研规格(`docs/handoff/launch-readiness-20261013/spec_D1_batch1b_scout.md`)拆成三步:1b-1 工具、入口、库原语(见下「第 1b-1 批」);1b-2 对账 leader、就绪标记写者、rewarm、audit `-require-maprev`;1b-3 K8s 配置载体(FenceShards / Reconcile / WaitAOF)、route-tool Job 清单、告警、清库脚本防护 |
| 第 2、3 批 | 已落码(2026-10-19),**未编译、未测试**,见 §13 | 第 2 批另含:四张表补 proto 守卫(替换 `routeTableShapes`,见第 1a 批偏离第 1 条)——**仍未做**,登记在 §13 未完成项 |

**第 0 批(2026-10-12)改动**:
- `deploy/docker-compose.yml`:redis 的 `maxmemory-policy` 由 `allkeys-lfu` 改为 `noeviction`(保留 `maxmemory 8gb`),注释写明原因。
- `go/data_service/internal/routing/router.go`:
  - `BatchGetPlayerHomeZone` 遇到畸形值整批报错,与单查 `parseHomeZoneValue` 同口径(此前静默跳过,被当成缺席);
  - `GetAllPlayerIDsInZone` 单键 GET 失败时整体返回错误(此前吞掉,名单会静默缺人)。
  - 新增单测:`TestBatchGetPlayerHomeZone_MalformedValueFailsTheBatch`、`TestGetAllPlayerIDsInZone`、`TestGetAllPlayerIDsInZone_GetErrorFailsTheScan`。
- `go/data_service/internal/server/dataserviceserver.go`:`BatchGetPlayerHomeZone` 注释补上畸形值口径。
- `tools/scripts/k8s_zone_rollback.ps1`:Step 4 FLUSHDB 防护。目标 DB 里有 `player:zone:*` / `player:placement:*` / `merge:*` / `consumer:applied*` / `kafka:retry*` 任一类键就拒绝,查询失败同样拒绝;核对在第 0 步预检(停服之前)与 Step 4 各做一次;新开关 `-ConfirmFlushSharedRedis` 放行;Step 4 打印的命令行遮住口令(此前明文打印)。探测用 `redis-cli SCAN` 逐页、找到第一个命中即停,不用 `--scan`(会把百万级键名全打出来)。
- `tools/scripts/dev_tools.ps1`:新开关 `-RollbackConfirmFlushSharedRedis` 转给回滚脚本。
- `tools/scripts/tests/k8s_zone_rollback_flush_guard.tests.ps1`(新):防护判定、fail-closed、确认开关、dry-run 不读 Redis、口令遮蔽、dev_tools 转发。
- `docs/design/zone_data_rollback.md`:步骤 3 的 FLUSHDB 补警告。

**第 0 批交 Codex 的验证**(仓库根):
1. `cd go/data_service`:`gofmt -l ./internal/routing ./internal/server`(无输出);`go vet ./internal/routing/... ./internal/server/...`;`go test ./internal/routing/... ./internal/server/... -count=1 -v -run "BatchGetPlayerHomeZone|GetAllPlayerIDsInZone"`,上面 3 个新用例与既有 `TestBatchGetPlayerHomeZone` PASS;再 `go test ./... -count=1` 全绿(既有失败按复验清单第 4 步归因)。
2. `pwsh -NoProfile -NonInteractive -File tools/scripts/tests/k8s_zone_rollback_flush_guard.tests.ps1`:fail=0(共 8 个用例)。
3. 回归:`pwsh -NoProfile -NonInteractive -File tools/scripts/tests/k8s_zone_rollback_gate_router_mode.tests.ps1` 与 `dev_tools_merge_zone_contract.tests.ps1` 均 fail=0(dev_tools 参数表新增了一个 switch)。
4. 本机 compose:`docker compose -f deploy/docker-compose.yml up -d redis` 后 `docker exec redis redis-cli CONFIG GET maxmemory-policy` 应为 `noeviction`。重建容器会不会丢本机数据取决于 volume,执行前先确认。

**已知未改**:chat、friend、match、scene_manager 共 9 处代码注释仍写「共享库是 allkeys-lfu」(如 `go/chat/etc/chat.yaml:44`、`go/friend/internal/svc/servicecontext.go:157`、`go/scene_manager/internal/logic/enterscenelogic.go:596`),现已过时。注释本身的结论(staging/prod 要用独立的 noeviction 实例)仍然成立,留作后续统一更正。

*(2026-10-16 更正:实际 go 下 10 处,另有 tools/scripts/k8s_deploy.ps1、deploy/k8s/README.md 各 1 处,共 12 处。chat、friend、match 的 internal/logic/keys.go、scene_manager 共 9 处已更正(只改注释,未编译);go/match/etc/match_service.yaml、k8s_deploy.ps1、README 3 处待对应会话收尾。见 docs/PROGRESS.md 同日「共享 Redis 淘汰策略过时注释更正」条目。)*

*(2026-10-19 结案:剩下的 3 处(`go/match/etc/match_service.yaml`、`tools/scripts/k8s_deploy.ps1` 的 friend here-string、`deploy/k8s/README.md` 的 friend 段)已更正,12 处全部改完,只改注释,未编译。结论不变。见 docs/PROGRESS.md「2026-10-19 G4a 收尾:剩余 3 处过时注释与其他文档指向注」条目。)*

---

**第 1a 批(2026-10-12):存储底座 + W1 的 db_first 开关 + 导入 / 审计工具 + W2 / W3 停用**

**未编译、未测试**(AGENTS.md §10.1)。Codex 跑完下面「第 1a 批交 Codex 的验证」之前,不能说「编译通过」「测试绿」「已验证」。

- **改动面**:代码与脚本 38 个(新增 15、修改 23),文档 10 个,超过 §10.2 的 30 文件门槛,已由 §11.1 第 1 行授权。
- **本批不改 proto,不重编 C++**:四张表走手写 DDL,列漂移只守列名与主键(`routeTableShapes`),补 proto 留到第 2 批(偏离第 1 条)。
- **所有档位固定 `RouteStore.Mode: redis`**:本地 `etc/data_service.yaml` 显式写 redis;K8s ConfigMap 所有档位固定 redis、不镜像服务 yaml(契约测试守住)。redis 模式下 W1 与 2026-10-12 之前逐字相同;切 db_first 是 P2 的显式运维动作,门禁见下。例外:新表的 DDL、形状守卫、meta 种行进了 `MigrateSchema` 的关键路径,redis 模式也受影响(风险第 8 条)。
- **代码注释里的「规格 x 节 / 第 N 条」**指第 1a 批定稿实现规格。那是会话工作稿,没有入库。与决策相关的部分收录在本节,编号保持一致:
  - 「规格 g.2 第 N 条」= 下面「相对本提案的偏离」第 N 行;
  - 「规格 g.3 第 N 条」= 「仍未解决的风险」第 N 条;
  - 「规格 h 节第 N 步」= 「第 1a 批交 Codex 的验证」第 N 步;
  - 「规格 c.3」= 「语义对照」表;「规格 Q1–Q16」= 「待定问题」表;
  - 其余各节(b 类型、SQL 与 Lua,c.1 / c.2 的步骤,d 配置,e 工具,f 用例清单,以及评审编号 B1 / B2 / M1–M6 / m1–m12)已落在代码与注释里,以代码为准。

**改动清单**

新增(15 个):

| 文件 | 要点 |
|---|---|
| `go/shared/placement/route.go`、`route_test.go` | 新键 `player:maprev:{id}`、`mapping:route:ready`(本批只定义,不读不写);围栏分片 `DefaultRouteFenceShards=8`、`MaxRouteFenceShards=64` 与 `RouteFenceShard`。64 同时是围栏持有者的契约:持有者必须写满 0..63 全部分片行。rev 的 `FormatRev` / `ValidRev` / `CompareRev`:先比长度,再逐字节比。发布脚本 `PublishRouteScript` 全文、`PublishRouteKeys`;`ClassifyPublishReply` 只认四种回复形状,其余一律 `ErrUnexpectedPublishReply`。仍只依赖标准库 |
| `go/data_service/internal/store/route_store.go` | 独立连接池,DSN 钉会话级 `READ-COMMITTED` 与 `innodb_lock_wait_timeout=1`。建 store 时跑探针:隔离级别、锁等待、TiDB 须为 `pessimistic`、`route_meta(id=1)` 存在,任一不符就关池拒建。W1 事务 `RegisterHome`;导入 `ImportRoutes` / `ImportMergedInto`;只读 `ScanRoutes` / `GetRoutes` / `ListMergedInto`;`decideImport`;`NewRouteWriterID`;`bootstrapRouteMetaRow`。它是四张表**唯一的写入方**,player_route 由 AST 守卫钉住 |
| `store/route_store_test.go`、`route_store_contract_test.go`、`route_store_integration_test.go` | 不连库的单测:DSN、DDL 契约、禁止 INSERT IGNORE、AST 写入守卫、`decideImport`、`RouteRow`、writer 清洗。假驱动契约测试(默认运行,BEGIN / COMMIT / ROLLBACK 记进语句序列):钉住「库应答 → 结局 / 语句 / 参数」和探针 fail-closed。真库集成测试(`integration` tag,storetest 一次性库):迁移幂等、CHECK、形状守卫、各分支、并发、锁等待封顶、围栏生效与过期、导入幂等、键集翻页 |
| `go/data_service/internal/routing/route_publish.go`、`route_publish_test.go` | db_first 的 W1 编排 `registerPlayerZoneDBFirst`;发布 `publishRoute`,同一个 pipeline 里发 EVAL 和 `Do("WAITAOF")`;纯函数 `checkWaitAOFReply`、`waitAOFBudgetMs`;消费方接口 `RouteStore`、选项 `WithRouteStore`、五个哨兵错误。单测 28 个顶层用例(miniredis + 假 store) |
| `go/data_service/internal/routetool/redisread.go`、`importer.go`、`auditor.go`、`routetool_test.go`、`routetool_integration_test.go` | `import-from-redis` 与 `audit` 的逻辑;键名与值的规范形式校验;import 开头 / 结尾各快照一次 `merge:merged_into:*`,有变化即退出码 3(窗口内跑完的合服也抓得到);audit 方向 B 补比方向 A 没比过的 id(含方向 A 之后才提交的行)。单测用 miniredis + 假 routeDB,集成测试用真库 + miniredis |
| `go/data_service/cmd/route_tool/main.go`、`main_test.go` | 薄入口:解析参数、装配、映射退出码(0 / 1 / 2 / 3);yaml 是 db_first 时真导入要显式 `-allow-db-first`,否则退出码 2 |

修改(23 个):

| 文件 | 要点 |
|---|---|
| `store/schema.go`、`store/mysql.go`、`store/export_test.go` | `bootstrapTables` 加四段 DDL,共 6 项;`zone_merge_fence` 主键 `CLUSTERED`(偏离第 16 条)。新增 `routeTableShapes` 与 `assertRouteTableShapes`;从 `assertColumnsPresent` 抽出 `assertColumnNamesPresent`;迁移末尾用 ODKU 种 `route_meta(id=1)`。改了表数注释。`mysql.go` 拆出 `openMySQLWithDSN`,新增 `buildRouteDSN`。`export_test.go` 导出测试钩子 |
| `routing/router.go`、`router_test.go`、`router_integration_test.go` | `NewRouter(c, opts...)` 先 Normalize + Validate RouteStore 段,非法就 panic,在建任何连接之前。`RegisterPlayerZone` 第一行按 Mode 分流,redis 分支逐字不动。删除 `RemapHomeZoneForMerge` 和 `ErrMergeFenceMissing`。`DeletePlayerZone` / `DeletePlayerZoneIfLocked` 注释写明生产路径已停用。单测删掉 3 个 remap 用例。集成测试 `itKeyPatterns` 加 `player:maprev:*`;remap 用例去掉 remap 步骤,改名为 `TestMergeFence_RealRedisBlocksRegister`;新增 4 个用例 |
| `server/dataserviceserver.go`、`dataserviceserver_test.go` | W1 错误映射加两支:`ErrInvalidRegistration` → Unavailable + 14,`ErrRouteStore` → Unavailable + 30,契约注释同步。W2 在鉴权之前恒回 FailedPrecondition + 16(`ErrCodeNotImplemented`),零变更。W3(`DeletePlayerData` 带 `delete_zone_mapping=true`)回 FailedPrecondition + 16。5 个 remap 用例换成 `TestRemapHomeZoneForMerge_AlwaysFailedPrecondition`;新增 `TestDBFirstRegister_ErrorMapping`、`TestDeletePlayerData_DeleteZoneMappingIsFailedPrecondition` 和辅助函数 `newDBFirstTestServer` |
| `logic/data_logic.go`、`data_logic_test.go` | `DeletePlayerData` 入口零副作用拒绝(哨兵 `ErrZoneMappingDeletionDisabled`),删掉不可达的删映射分支;用例改写为 `TestDeletePlayerData_ZoneMappingBranchDisabled` |
| `constants/error_codes.go` | 新码 `ErrCodeRouteStoreError = 30`;24(`ErrCodeMergeFenceMissing`)注释改为「已停用,号不复用」 |
| `config/config.go`、`config_test.go` | `RouteStoreConfig` 及 `Normalize` / `Validate` / `DBFirst` / `WaitsForAOF` / `NumLocal` / `NumReplicas`;AdminToken 与表数注释。用例:缺省值、拒绝表、显式写 0 关掉 WAITAOF、仓库 yaml 为 redis 且合法 |
| `svc/servicecontext.go` | 接口 `PlayerRouteStore`(`routing.RouteStore` 加 `Close`)。只在 db_first、迁移成功、且 mapping Redis 过了 WAITAOF 能力探针(`routing.ProbeWaitAOF`)时装配,失败打 ERROR、保持 nil;只在非 nil 时注入 Router,避开「非 nil 接口包着 nil 指针」。`MissingStores` 在 db_first 时带 `player_route`(`servicecontext_missing_stores_test.go` 表驱动钉住 db_first / redis / 空 Mode 三种);Close 时关闭 |
| `data_service.go` | `-migrate` 之后校验 RouteStore 段,非法就拒启(exit 1)。db_first 时:打参数 Info 和冻结窗口 WARN,横幅加 route 行,调 `metrics.PrimeRoute()`。flag 注释、迁移输出文案、故障码注释(补码 30) |
| `cmd/debugutil/debugutil.go` | db_first 时装 route store(writer `data_service_debug@{host}`),cleanup 时关闭;打不开就报错,不退化成 redis |
| `metrics/metrics.go`、`metrics_test.go` | 两个计数器与 `PrimeRoute`,见下「运行时要点」 |
| `etc/data_service.yaml` | 新增 `RouteStore:` 段:显式 `Mode: redis`,写满全部键,注释里写明切换前提;改 AdminToken 与表数注释 |
| `tools/scripts/k8s_deploy.ps1`、`tests/k8s_deploy_contract.tests.ps1` | data-service ConfigMap 在 `Store:` 之后加 `RouteStore: Mode: redis`;「四张表」注释改成不写数量(只改注释)。契约测试加 1 个用例:dev / prod 两档 ConfigMap 与服务 yaml 都是 redis |
| `tools/scripts/stress_round19.ps1`、`currency_crash_window.ps1` | 新参数 `$GlobalDbName='testdb'`。清库时三张路由数据表 TRUNCATE,`route_meta` 原地复位(不换 generation),只处理已存在的表。SQL 失败就 throw,比 zone 表那段更严格,理由写在脚本注释里(规格 Q7) |

文档(10 个):本节;`docs/PROGRESS.md`;`docs/README.md`;[player-storage-placement.md](./player-storage-placement.md) §4.4 / §11.4;[merge-zone-runbook.md](../ops/merge-zone-runbook.md) §3 / §9.3 / §12;[server_merge_design.md](./server_merge_design.md);[enter-scene-zone-routing.md](./enter-scene-zone-routing.md);[global-data-layer-tidb-decision.md](./global-data-layer-tidb-decision.md) D7 / §4 / §6;`ARCH.md` 决策 21 的行尾注;[复验清单](../handoff/placement-reverify-checklist-20261012.md)第 4 步附录。

**运行时要点(db_first)**

- **W1 的顺序**(`route_publish.go`):
  1. 参数校验:player_id ≠ 0,home ∈ [1, 999999];
  2. store 是否装配;
  3. Redis 预检,两次往返:先 EXISTS `merge:in_progress:{home}`(与 redis 模式同一个 `IsMergeInProgress`);再一次 MGET 取 zone / placement / maprev。zone 是别的值就判冲突、不进库;三键任一在,就设 `ExistingRowOnly`,库里必须已有这一行;
  4. `RegisterHome`:普通读 `route_meta.writable` → 围栏分片行 `FOR UPDATE`(分片行不存在时在事务外懒建)→ ODKU 插入 rev=1,或 `FOR UPDATE` 加锁读之后补 home → COMMIT;
  5. 发布:脚本 + WAITAOF,同一条连接。

  事务里不做任何 Redis I/O。
- **`RegisterHome` 的结局**:inserted / filled_home / filled_home_placement_kept / already_same / conflict / fenced / row_missing。只有故障才返回 err。1213 / 9007 有界重试(共 4 次,10–50ms 抖动);1205 不重试(偏离第 15 条)。
- **发布脚本**(KEYS 第 4 把是围栏键):
  - `{1, rev}`:三键已写入;
  - `{0, cur, h, p}`:缓存已是这一版或更新,把 maprev 原值写回一次(touch),让随后的 WAITAOF 覆盖缓存现状;
  - `{2, '', h, p}`:缓存没有 maprev,遗留值又与本次不同,零写入;
  - `{3}`:围栏存在,零写入。

  Redis 错误回复、回复形状不对,都不重试。网络错误和 WAITAOF 份数不足,退避后整条 pipeline 重发,最多 `PublishAttempts` 次。WAITAOF 的等待上限 = min(`WaitAOFTimeoutMs`, ctx 剩余 − 100ms),截断成整毫秒后不足 1ms 就不发(timeout 为 0 会无限阻塞)。
- **配置 `RouteStore`**:

  | 键 | 默认 | 合法范围 |
  |---|---|---|
  | `Mode` | redis | `redis` 或 `db_first`,大小写敏感 |
  | `FenceShards` | 8 | [1, 64] |
  | `MaxOpenConn` / `MaxIdleConn` | 20 / 5 | `MaxOpenConn > 0`,`0 ≤ MaxIdleConn ≤ MaxOpenConn` |
  | `PublishAttempts` | 3 | [1, 5] |
  | `WaitAOFNumLocal` | 1 | {0, 1} |
  | `WaitAOFNumReplicas` | 0 | [0, 16] |
  | `WaitAOFTimeoutMs` | 1500 | [1, 1500] |

  任一非法就拒启,与 Mode 无关。WAITAOF 两项都显式写 0 = 不发 WAITAOF,只给单测和没有 AOF 的环境用。
- **指标**:只在 db_first 时有数,启动时预置为 0。
  - `data_service_route_register_total{outcome}`,12 个取值:inserted、filled_home、filled_home_placement_kept、already_same、conflict、fenced、refused_unknown_row、precheck_conflict、precheck_fenced、precheck_error、store_error、invalid。每次 db_first 的 W1 恰好计一次,合计 = 请求数;precheck_error 是 Redis 预检本身出错(EXISTS / MGET 失败、MGET 长度不对、缓存 zone 解析不了),评审后补上(规格原为 11 个,不含它);
  - `data_service_route_publish_total{result}`,8 个取值:applied、confirmed、superseded、conflict、legacy_conflict、fenced、not_durable、error。WAITAOF 回复形状不对(不是两个非负整数)是确定性错误、不重试,计 `error` 而不是 `not_durable`。

  player_id 和 rev 只进 ERROR 日志,不做 label。
- **错误码**:新增 30 `ErrCodeRouteStoreError`;24 `ErrCodeMergeFenceMissing` 不再产生,号不复用。

**语义对照(规格 c.3)**

gRPC code 的集合与今天相同:OK、AlreadyExists、FailedPrecondition、Unavailable。新码 30 和 Unavailable 下的码 14 只出现在 message 里;login 只看有没有 err。

| 场景 | redis 模式(今天) | db_first | gRPC 结果 |
|---|---|---|---|
| 新玩家 | Lua SETNX | 预检无键 → 插入 rev=1 → 发布 `{1}` + WAITAOF | OK |
| 同 home 重试 | 不写 | 预检见到键 → 加锁读同 home → 重新发布(补齐上次失败的发布) | OK |
| 异 home | Conflict | 预检或加锁读发现异值 | AlreadyExists + 21 |
| 既有值畸形 | unparsable | 预检用同一文案 | Unavailable + 1 |
| 围栏:预检命中 | EXISTS | 同一个函数 | FailedPrecondition + 22 |
| 围栏:预检之后才立 | 写入脚本复核,两键都不写 | 发布脚本 KEYS[4] 复核,三键都不写;**库里已提交的行留下(id 已烧掉)** | FailedPrecondition + 22 |
| 围栏:库分片行 | 不存在 | `FOR UPDATE`;**本批恒不生效**,见下 | FailedPrecondition + 22 |
| home 缺、落点在 | 写 home,保留落点,打 ERROR | 补 home(R6a),打同一条 ERROR | OK |
| 缓存无 maprev 且遗留值不同 | 不存在 | 发布脚本回 `{2}`,零写入,打 ERROR | Unavailable + 1 |
| Redis 认得、库里无行 | 不存在 | 拒绝(row_missing),提示先导入 | Unavailable + 30 |
| 库故障、store 没装配、meta 缺失、writable≠1、1205 | 不存在 | `ErrRouteStore` | Unavailable + 30 |
| 已提交,但发布失败或未确认落盘 | 不存在 | 发布错误。login 照常 fail-closed 并释放名字,重试走幂等分支补发 | Unavailable + 1 |
| player_id = 0 或 home 越界 | 照写 | `ErrInvalidRegistration` | Unavailable + 14 |

**P2 切换门禁**(切 `RouteStore.Mode=db_first` 之前,缺一不可)

1. **冻结窗口**:从任一 data_service 副本切到 db_first 起,到第 2 批落地为止,禁止 merge_zone 的一切写模式:merge、unmerge、relocate、pin-placement、backfill。这与 §7 P1 是同一个窗口,原因见下一段。
2. **P1 导入完成且审计为 0**:`route_tool import-from-redis -dry-run=false` 退出码 0,随后 `route_tool audit` 退出码 0。步骤见 runbook §9.3。K8s 镜像只构建单一入口,生产上要等第 1b 批把 route_tool 并进 data_service 主程序才能跑(偏离第 8 条)。
3. **mapping Redis**:`noeviction` + `appendonly yes`,版本 ≥ 7.2(WAITAOF)。按拓扑填 `WaitAOFNumLocal` / `WaitAOFNumReplicas`,redis-ha 档 NumReplicas 写 1。db_first 装配 route store 之前会按这两项发一次 WAITAOF 做能力探针(`routing.ProbeWaitAOF`,timeout = `WaitAOFTimeoutMs`):报错或份数不足就不装配,K8s(`Store.Required=true`)拒启。理由:这类配置错误是确定性的,留到请求期就是每次 W1 都失败,而 go-zero 给 mapping Redis 套的熔断器把 Redis 错误回复都记成失败,熔断一开,同一客户端上的 GetPlayerHomeZone、BatchGet、玩家锁也被按概率丢弃。
4. **建角压测**:按 AGENTS.md §6 出 W1 p99 与「发布未确认率」的对比表,阈值由负责人看表拍板(规格 Q16)。发布未确认率 = `route_publish_total{result=~"not_durable|error"}` 占总数的比例:WAITAOF 回复形状不对计在 `error`(确定性、不重试),只看 `not_durable` 会漏掉它们。不可接受时二选一:给 mapping 实例配 `appendfsync always`(§10 第 5 条),或调整服务端标量 Timeout,并同步更新 `Assert-GrpcClientDeadlineBudget` 的登记。
5. **切换本身**:改 yaml(K8s 改 ConfigMap 生成器里那一行),滚动重启,启动日志里应能看到那条 `WARN RouteStore.Mode=db_first: ...`。全部副本切完之后,按 §7 P3 再导入一次(yaml 已是 db_first,真导入要显式 `-allow-db-first`,建议小批量)、再审计一次。除 P3 这一次之外,切 db_first 之后不再跑真导入:那时库是真源,从缓存回填库方向是反的。

**db_first 下,库内围栏在本批恒不生效**

`zone_merge_fence` 已经建表,W1 每次也都 `FOR UPDATE` 锁一个分片行。但第 1a 批没有任何写者写 `run_id`:merge_zone 仍只写 Redis 的 `merge:in_progress:*`,从不写这张表。所以库内复核的结果恒为「未封锁」。

提交点的合服围栏复核,靠的是发布脚本的第 4 把键 `merge:in_progress:{home}`:命中时三键零写入、回 22,但**库里已提交的行会留下**(见下一段)。第 2 批 merge_zone 改成库先写之后,库内复核才成为真正的提交点复核。在那之前,db_first 的安全前提就是上面第 1 条的冻结窗口:在 db_first 下跑 relocate、pin、backfill,会改出一份库不知道的 Redis 值。遗留值守卫能挡住「之后的 W1 重发把它改回旧值」,挡不住「库与缓存长期分叉」,这类分叉只能等 audit 发现。导入工具也只能检测 merge / unmerge 立的围栏。

**已提交未发布的孤儿行**

发布失败、WAITAOF 超时、发布时撞上围栏,都会在库里留下一行 id 已烧掉的映射,而 W1 对调用方返回失败。`route_tool audit` 把三族键全缺的计为 `db_only_unpublished`,部分在的计为 `missing_in_redis` / `value_mismatch`,退出码都是 1。WAITAOF 超时那种情况,缓存其实已经写入、只是没确认落盘,audit 看不出差异,直到 Redis 丢了这次写才变成 `db_only_unpublished`。本批**没有**自动补发,要等第 1b 批对账 leader 的全量预热推送。因此:

- db_first 下,P3 的「audit 为 0」门禁要等第 1b 批落地之后才可达;
- 第 1b 批推送之前,先按 audit 清单人工核对:哪些行的 home 指向了已合走的 zone(只有违反冻结纪律时才会出现);
- 发布未确认的那种情况,还会同时留下 Redis 键,即 login `createplayerlogic.go:229-236` 说的「幽灵映射」。redis 模式下响应丢失也会出现同类问题,只是频率更低。

**相对本提案的偏离(规格 g.2,编号一致)**

| # | 偏离 | 理由 | 代价 |
|---|---|---|---|
| 1 | 不新增 proto message(§3.1) | 避免依赖 proto 重新生成和 C++ 重编 | 只守列名和主键,不守类型。第 2 批补 proto 时删掉 `routeTableShapes`;`RouteRow` 要么改用生成类型,要么明确作为领域类型保留并写清转换(AGENTS.md §3 不许有与 proto 重复的并行 struct)。DDL 列注释已预留 `pb:1..9`,补 proto 时沿用同样的编号 |
| 2 | 用 EVAL,不用 EVALSHA(§4.1 第 4 条;**任务原文写的是 EVALSHA**) | pipeline 里遇到 NOSCRIPT 无法透明回退;与仓库现状一致(router.go 全部用 `EvalCtx`);W1 的频率等于建角频率 | 每次多发约 1.5KB。第 2 批 merge_zone 批量发布时重新评估。**2026-10-13 补(容量模型 §4.1「连带修正」):改回 EVALSHA 列为 S2 档的前置**,本批代码仍是 EVAL;改的时候要一并解决本行「理由」里的 NOSCRIPT 回退 |
| 3 | 发布脚本(§4.2)加了 rev 校验、可选的围栏键、遗留值守卫、touch、逐字节比较 | 提交点复核围栏;不静默覆盖遗留值;touch 让幂等重发也能用 WAITAOF 确认落盘;逐字节比较不依赖 locale | 返回 0 时多一次 SET;每次多两次 GET |
| 4 | 发布的 I/O 放在 data_service routing,`go/shared/placement` 只放脚本、键和回复分类;没有 `commitAndPublish` helper(§4.1 第 6 条) | `shared/placement` 约定只依赖标准库 | 第 2 批 merge_zone 要自己写 go-redis 的 I/O,并镜像脚本 |
| 5 | **db_first 保留 Redis 预检**(EXISTS 围栏 + MGET 三键),没有删掉 EXISTS 预检(§4.3 W1) | 第 2 批之前,围栏和清单都在 Redis;库可能缺行(未导入或回档) | 多两次 Redis 往返 |
| 6 | 分片行「确保存在」改成懒执行:加锁读读不到才建 | 避免每次 W1 都在事务外做一次 autocommit 写 | 语义不变 |
| 7 | 新增契约 `MaxRouteFenceShards = 64`:围栏持有者必须锁住并写满全部 64 行 | 各进程 N 配置不同时,N 大的写者会落到持有者没锁的分片上,绕过围栏 | 获取一次围栏要锁 128 行 |
| 8 | route_tool 是独立的 cmd(§7 P1 写的是 `data_service -route-import-from-redis`) | 按任务要求 | **K8s 镜像只构建单一入口**(`deploy/k8s/Dockerfile.go-svc:10`),生产上跑 P1 要等第 1b 批补主程序入口 |
| 9 | W3 改为在 logic 入口零副作用拒绝,映射成 gRPC FailedPrecondition | 不能先删数据键、再拒删映射,留下半截操作 | DeletePlayerData 原本只回 in-band 码,这是唯一的例外;仓库内没有调用方 |
| 10 | 用 `Do("WAITAOF")`,不用 go-redis 的 `WaitAOF` | go-redis v9.18.0 会把数组回复错解析成 int | 无 |
| 11 | 新增码 30 `ErrCodeRouteStoreError` | 告警能分清「库坏」和「Redis 坏」 | 只出现在 message 里 |
| 12 | W2 在鉴权之前就恒拒,并删除 `Router.RemapHomeZoneForMerge` | 无条件 SET 的危害见 `merge_run.go:836-837`;仓库内只有注释提到它 | 原来的 5 个服务端用例和 3 个路由用例要改写;guild、login、match、trade、merge_zone 里提到它的注释暂不改(记入「已知未改」) |
| 13 | 清库脚本对 `route_meta` 原地复位,不 TRUNCATE | `currency_crash_window.ps1` 在服务运行中清库;TRUNCATE 会让 db_first 的 W1 一直 fail-closed,直到下次迁移 | 第 4 张表不是「清空」而是「复位」;generation 不换 |
| 14 | 参数非法时 db_first 回 **Unavailable + 码 14**,不回 InvalidArgument | 与今天的 RPC 契约一致:gRPC 码集合保持 OK、AlreadyExists、FailedPrecondition、Unavailable | 语义上「参数错」落进了 Unavailable,靠码 14 区分;只有 db_first 可达;redis 模式下仍会写入垃圾,与今天逐字相同 |
| 15 | 会话 `innodb_lock_wait_timeout=1`;**1205 不重试**,只重试 1213 和 9007(§4.1 第 2 条写的是「1213/1205 重试 3 次」) | 默认 50s 的锁等待在 2s 预算里永远等不到 1205,ctx 到期后服务端还会继续占着分片锁;guild 先例 | 锁争用超过 1s 时直接 Unavailable;导入遇到 1205 时整体失败,靠重跑(幂等)恢复 |
| 16 | `zone_merge_fence` 主键加 `/*T![clustered_index] CLUSTERED */`(§3.2) | TiDB 建表后不能改聚簇属性;聚簇才能按 zone 值 SPLIT | MySQL 上没有影响 |
| 17 | 导入写入 `rev = maprev`;库 rev 小于 maprev 时不写 | 防止导入把「库被回档」洗白成 rev 倒退,导致之后的发布被静默判成 Superseded | 导入多读一族键 |
| 18 | `WaitAOFTimeoutMs` 默认 1500ms;「发布未确认率」列为 P2 门禁 | `everysec` 下本地确认最长约 1s 以上,1000ms 正压在边界上 | W1 最坏耗时接近 2s;切换之前要先压测 |

仓库内没有 W2 / W3 生产调用方的证据:grep `RemapHomeZoneForMerge`,除 data_service 自身外只出现在 guild、login、match、trade、merge_zone 的注释里,robot 里只有生成的消息 id,cpp、java、ps1 中都没有;`DeleteZoneMapping` 只在 data_service 内部和测试里出现。

**落码时相对定稿规格的取舍**(都朝 fail-closed 或更可测的方向,逐条可在代码注释里找到理由)

- **store**:
  - 导入的两个事务也先读 `route_meta.writable`(§4.1 第 2 条:所有映射写者先读它)。
  - 入参校验比规格严:`ImportRoutes` 拒绝 player_id 为 0、home 大于 999999、home 与落点都缺、落点不合法;`ImportMergedInto` 要求 src / dst 非零且不同、run_id 合法;`RouteStoreOptions` 要求分片数在 [1, 64]、writer 非空、不超过 96 字节、只含允许的字符。
  - `GetRoutes` 复用 dry-run 的批量读语句,多一个 `ORDER BY player_id`,结果相同。
- **routing / server**:
  - 发布前拒绝 PlayerID、HomeZone 为 0 或 rev 非法的行;编排层核对 `RegisterHome` 回来的行与请求是同一名玩家、同一个 home,对不上返回 `ErrRouteStore`。
  - WAITAOF 回复形状不对不重试,只有份数不足才重试(规格 c.1 第 4 步原写「不满足就记 not_durable 并重试」;形状错误是确定性的,重试无益,改为 fail-closed 返回,计 `result="error"` —— P2 门禁的「发布未确认率」因此要把 `error` 一并算上,见上)。
  - 指标口径:Superseded 且 home 不同计 `result="conflict"`;Redis 预检本身出错(EXISTS / MGET 失败、MGET 长度不对、缓存值解析不了)计 `route_register_total{outcome="precheck_error"}`(评审后新增的第 12 个取值;起初不计,但 W1 的 error_code 只在 gRPC message 里,grpcstats 分不出码 1 与码 30,不计这类拒绝在指标上就看不见)。
  - AdminToken 注释写成「守三个 Rollback* 与 ReleasePlayerName」,因为代码里 ReleasePlayerName 也用这个 token。
  - debugutil 在 db_first 下打不开 route store 就报错,不退化成 redis。W2、W3 被拒时打带调用方身份的 ERROR。
  - 测试手法:命令计数用 miniredis 的 `SetPreHook`;真 Redis 对照向量放在集成测试文件里,为的是让 routing 单测数保持 54;`TestDBFirstRegister_RealStores` 遇到 appendonly 关闭时关掉 WAITAOF、只记日志,真实的 WAITAOF 路径由 `TestPublishRoute_RealRedisWaitAOF` 负责。
- **route_tool / 脚本**:
  - 配置用 `conf.Load` 而不是 `MustLoad`:后者出错时直接退出码 1,破坏「失败一律 2」。logx 改写到 stderr、只留 Error 级别,stdout 只有一行统计。装配时显式 PING mapping Redis,上限 5s。
  - import 的 merged_into 冲突与畸形也算退出码 1;结尾命中围栏(退出码 3)时仍输出统计行。
  - import 开头(围栏检查之后)与结尾各原样快照一次 `merge:merged_into:*`,有新增 / 消失 / 改值即退出码 3(规格 e 节只要求结尾复查围栏;一次在两次围栏检查之间开始并跑完的合服 / 撤销合服会删掉围栏,只查围栏抓不到)。结尾退出码 3 之后**不能原样重跑**:窗口内被合服改写的玩家,库里是旧 home、缓存里是新 home,导入从不覆盖非 NULL 值,重跑只会判 conflict、永远收敛不了。恢复办法写在 runbook §9.3「重跑与恢复」:只限 P1(库还不是真源),窗口外清空 `player_route` / `zone_merged_into`、`route_meta` 原地复位,再从头导入。
  - yaml 是 db_first 时,`import-from-redis -dry-run=false` 默认拒绝(退出码 2),要显式 `-allow-db-first`:那时库是真源,从缓存回填库方向是反的,导入事务的 `FOR UPDATE` 还会让并发 W1 吃 1205。唯一合法的用法是 §7 P3 那一次。
  - audit 方向 B 对方向 A 没比过的 id 都补比一次(规格原写「库里有行的已在 A 遍比过,跳过」):方向 A 翻页扫过之后才提交的行(db_first 下审计期间新登记的玩家)否则在本次审计里完全没比对过。
  - 只有 maprev、没有 zone 和落点的玩家判 MALFORMED。audit 里 merged_into 畸形和键名不规范计入 `redis_malformed`;maprev 缺席不算差异(第 1a 批 redis 模式下没有写者写它);按「玩家 × 差异种类」计数,复核取两次读取的交集。
  - 清库 SQL 连接时不指定默认库:先查 information_schema,写入前 `USE <db>`;`GlobalDbName` 只接受普通标识符。两份脚本各有一份同样的函数,注释写明「改一处两处同改」。
  - 规格里可选的 `tools/scripts/tests/wipe_global_route_tables.tests.ps1` 没有做。

**评审修复(2026-10-12,同日;同样未编译、未测试)**:除上面几条已并入正文的(precheck_error、WAITAOF 启动探针、merged_into 快照、`-allow-db-first`、audit 方向 B),还有:

- 测试:`svc/servicecontext_missing_stores_test.go` 补 db_first 缺 store 必报 `player_route`、redis / 空 Mode 不报的表驱动用例,以及 WAITAOF 探针失败不装配的用例(放在 svc 包,routing 单测数仍是 54);`route_store_contract_test.go` 删掉「`sql.Null[uint32]` 当参数必被驱动拒绝」的反向断言 —— Go 1.24 起 `Null[T].Value()` 先过 `driver.DefaultParameterConverter`,uint32 会被转成 int64 放行,那条断言在 go 1.26 上会红;绑定继续用 `nullU32` / `nullU64`,理由改写为「`Null[T]` 的转换随 Go 版本变化,统一走 0 = NULL 的领域约定」;`route_publish_test.go` 的「发布失败后重试补齐」改为在新地址的 miniredis + 新 Router 上重试(go-zero 按地址缓存连接池,同地址 Restart 会拿到旧连接、在熔断器上攒失败,结果取决于墙钟与平台);真库用例 `LockWaitBoundedAfterClientCancel` 把 p 的客户端超时从 300ms 放宽到 700ms(余量 ≈ p 的超时 − p 走到 R4 前的往返,超时越长余量越大;仍 < 1s),q 遇 1205 在用例里重试一次以吸收 InnoDB 锁等待超时的检查节拍。
- 脚本:两份清库脚本的 `Invoke-GlobalDbSql` 在函数内局部 `$ErrorActionPreference='Continue'`,成败只看 `$LASTEXITCODE`(否则 Windows PowerShell 5.1 / pwsh < 7.2 会把 mysql 的口令警告当 NativeCommandError 抛出);`k8s_zone_rollback.ps1` 的 FLUSHDB 共享键清单(第 0 批)补 `player:maprev:*`,`k8s_zone_rollback_flush_guard.tests.ps1` 期望同步。
- 注释:`constants/error_codes.go` 码 23 与分节标题、`config/config_test.go` 的 AdminToken 分节与用例注释,写明 remap 已停用、token 现在只守三个 Rollback* 与 ReleasePlayerName 的运维路径;`mysqlLikeConvert` 注释写明只复刻本 store 用得到的子集。
- 文档:本文 H1 带上状态,与 `docs/README.md` 索引行逐字一致;`tools/scripts/gen_docs_index.py` 的「玩家数据与存储」规则补 `player-route-mapping-*`,索引行按字母序挪到 `player-storage-placement.md` 之前。`gen_docs_index.py --check` 仍可能因工作区里别人新增 / 改动的其它条目报过期(例如 `tidb-phase1-and-launch-readiness.md` 没登记分类、是手工插的行),那些不归本批。runbook §9.3 同步上述行为与恢复步骤。

**仍未解决的风险(规格 g.3,编号一致)**

1. **TiDB 热点**(MySQL 上不存在):
   - `player_route` 的主键唯一索引和 `idx_route_updated` 都单调递增,NONCLUSTERED + SHARD_ROW_ID_BITS 只打散行数据、打散不了索引尾部;`idx_route_home(home_zone, player_id)` 在单个 zone 内同样是尾部热点。
   - **`zone_merge_fence` 整张表只有「zone 数 × N」行,必然落在同一个 Region。** TiDB 悲观事务的 `FOR UPDATE` 要往 TiKV 写锁记录,全服每次建角都在同一个 Region Leader 上加锁、解锁;分片只拆开了行锁排队,拆不开 Region 热点。TiDB Phase 1 上线前必须 `SPLIT TABLE zone_merge_fence BY (2,0),(3,0),…` 按 zone 预切并纳入压测(已登记进 TiDB 决策 §4 第 11 行、§6 第 8 条)。
   - `route_meta` 是单行,每次 W1 都要读,是读热点。`idx_route_updated` 本批没有读方,保留它是因为 bootstrap 只执行 `CREATE TABLE IF NOT EXISTS`,以后补索引要另写收敛代码。
2. **TiDB 的 CHECK 约束**要求 ≥ 7.2 并开启 `tidb_enable_check_constraint`(默认关),所以应用层校验不能省。
3. **`writable` 是普通读**:第 1b 批把它置 0 时,正在进行的事务仍会提交,1b 的流程要先等这些事务结束。
4. **db_first 依赖冻结纪律**:见上面「库内围栏在本批恒不生效」。
5. **P2 混跑窗口**:同一个 player_id 被新旧副本同时用不同 home 注册时,有遗留值守卫兜着,最多一边成功,另一边回 Unavailable 或 AlreadyExists;残留问题是「库是 B、Redis 是 A」的分叉,由 audit 发现。player_id 每次建角都新发,实际几乎不可能发生。
6. **已提交未发布的孤儿行**:见上面同名一段。
7. **库被重置后重新种的 `route_meta` 也是 `writable=1`**:靠预检的「缓存认得 ⇒ 只认已有行」、发布时「Superseded 且 home 不同就失败」、导入的 rev_regression 三处兜底;回档的自动检测要到第 1b 批。
8. **新的启动依赖**:新表的 DDL、形状守卫、meta 种行进了 `MigrateSchema` 的关键路径,**redis 模式也受影响**。任一步失败,快照、流水、号段、名字四个 store 都置 nil,本地禁止建角;K8s 下 `Store.Required=true` 直接拒启。所以「redis 模式与今天逐字相同」只适用于 W1 的行为。本机 MySQL 是 `mysql:latest`、K8s 是 `mysql:8.0`,两边都要跑一次迁移。
9. **WAITAOF 的延迟与 go-zero 的细节没有实测**:`PipelinedCtx` 是否就是 go-redis 的同连接 pipeline、ReadTimeout 与 MaxRetries 的取值、redis 熔断器怎么计数脚本的错误回复。由验证第 4 步的 CLIENT ID 夹心用例给出部分证据;本机 compose 的 mapping Redis 是 `redis:latest`,WAITAOF 要求 ≥ 7.2。熔断器把 pipeline 回的 Redis 错误回复都记成失败(go-zero 的 acceptable 只放过 nil / redis.Nil / context.Canceled):AOF 配错这类**确定性**错误已由启动期 WAITAOF 探针挡住;运行期仍会计入熔断的只剩个别玩家的畸形 maprev(脚本拒绝)与 WRONGTYPE,影响面取决于它们的频率。彻底隔离要按 Q3 的备选给发布单独建一个不经 go-zero 熔断器的 go-redis 客户端,本批不做。

**待定问题(规格 Q;本批按推荐答案做)**

| # | 问题 | 本批做法 |
|---|---|---|
| Q1 | route_tool 在 K8s 上怎么跑 | 本批是独立 cmd;第 1b 批在 `data_service.go` 加 `-route-tool <sub>`,复用 `internal/routetool`,与 `-migrate` Job 同形 |
| Q2 | EVAL 还是 EVALSHA | EVAL(偏离第 2 条),向负责人点明。2026-10-13:容量模型 §4.1 把改 EVALSHA 列为 S2 前置,S1 仍可用 EVAL |
| Q3 | go-zero `PipelinedCtx` 还是独立 go-redis 客户端 | go-zero。由 `TestPublishRoute_RealRedisSameConnection` 定;FAIL 就改用独立 `goredis.NewClient`(DB 0,ReadTimeout = WAITAOF 上限 + 1s)专做发布 |
| Q4 | miniredis 是否支持 WAITAOF | 未核实;单测一律显式配 0 / 0,真实 WAITAOF 只在真 Redis 集成测试里验 |
| Q5 | db_first 是否保留 Redis 预检 | 保留到第 3 批;发布脚本的 KEYS[4] 要等库内围栏真正生效之后再评估 |
| Q6 | `DeletePlayerZone` / `DeletePlayerZoneIfLocked` 删不删 | 保留,注释写明「生产路径已停用」,等删号设计 |
| Q7 | 清库时 route_meta 怎么处理 | 原地复位;三张数据表 TRUNCATE;只处理存在的表;SQL 失败即抛错 |
| Q8 | AGENTS.md §6.2 是否加「全局库路由表一并清」 | 推荐加(§8.2),但 AGENTS.md 工作区有别人未提交的改动,**本批没改,待负责人确认** |
| Q9 | 给 RegisterPlayerZone 加 MethodTimeouts | 本批不加;P2 压测之后再定,要加就同步登记 `Get-GrpcClientMethodsNotCalledFromCpp`,并注意 K8s 只镜像标量 Timeout |
| Q10 | 新码 30 | 采用 |
| Q11 | 导入遇到「库非 NULL、与 Redis 不同」 | 报告并跳过,退出码 1,不覆盖;rev_regression 同样处理 |
| Q12 | 导入时顺带写 maprev 或发布 | 不做,留给第 1b 批全量预热;遗留值守卫保证不写 maprev 也安全 |
| Q13 | W1 拒绝注册进已被合走的 zone(查 `zone_merged_into`) | 本批不做,记为后续加固项 |
| Q14 | Java 的 PARITY.md 何时登记 | 第 1 批(1a + 1b)全部完成时登记一行;本批只在 PROGRESS 写「Java 待做」 |
| Q15 | 锁冲突重试次数 | 1213 / 9007 共 4 次(1 + 3),最多多睡 150ms;1205 不重试 |
| Q16 | P2 门禁的阈值 | 负责人看 Codex 的压测对比表之后拍板,本批不预设数字 |

**第 1a 批交 Codex 的验证(规格 h 节,步骤编号一致)**

先后顺序:

- 先按 [placement-reverify-checklist-20261012.md](../handoff/placement-reverify-checklist-20261012.md) 复验,**以第 4 步之后追加的附录为准**:计数与用例名已因第 0 批和第 1a 批改变;
- 再跑上面「第 0 批交 Codex 的验证」;
- 最后跑下面第 1–9 步。全部在仓库根目录、用 PowerShell 执行。

第 1–9 步:

1. **shared**:

   ```powershell
   cd go/shared
   gofmt -l ./placement           # 无输出
   go vet ./placement/...
   go test ./placement/... -count=1 -v -run "Route|Rev|Publish|MapRev|Keys"
   go test ./... -count=1
   ```

   placement 包顶层用例由 9 个变为 15 个(新增 `route_test.go` 6 个)。
2. **data_service 单元测试**:

   ```powershell
   cd go/data_service
   gofmt -l ./internal ./cmd ./data_service.go   # 无输出
   go vet ./...
   go build ./...
   go test ./... -count=1
   go test ./internal/routing/... ./internal/server/... ./internal/logic/... ./internal/config/... ./internal/store/... ./internal/routetool/... ./cmd/route_tool/... -count=1 -v -run "Route|Publish|DBFirst|Remap|DeletePlayerData|ShippedYaml|WaitAOF|NoPlayerRouteWrites|DecideImport|RegisterHome|ImportRoutes|NewRouteStore"
   go test ./internal/svc/... ./internal/metrics/... -count=1 -v -run "MissingStores|PlayerRouteStore|PrimeRoute"   # 评审修复补的 svc 用例与 12 个 outcome
   go test ./internal/routing/... -list . | Measure-Object -Line   # 期望 54 个顶层用例 + 1 行 ok
   ```

   通过标准:全绿。既有失败按复验清单第 4 步归因,单独列出。`TestNewPlayerRouteStoreRefusedWhenWaitAOFProbeFails` 连一个刚关掉的本机端口,要几秒(go-zero PING 超时),属正常。
3. **MySQL 集成**。本机 compose 的 appuser 没有建库权限,用 root;口令取 `deploy/docker-compose.yml` 里的 `MYSQL_ROOT_PASSWORD`,不要写进日志。

   ```powershell
   $env:DATA_SERVICE_IT_MYSQL_HOST = '127.0.0.1:3306'
   $env:DATA_SERVICE_IT_MYSQL_USER = 'root'
   $env:DATA_SERVICE_IT_MYSQL_PASSWORD = '<见 compose>'
   go test -tags=integration ./internal/store/... -count=1 -v -run "Route|MigrateSchema"
   ```

   通过标准:没有 FAIL。`TestMigrateSchema_UpgradesLegacyHandWrittenTables` 从 09-29 起就红(复验清单 4c),它会被 `MigrateSchema` 选中,单独列出,不计入本批结论。出现 SKIP 如实报成「未验证」。保留 `TestRouteStore_ConcurrentManyPlayersSameZone` 打印的 lockRetries,以及 `TestRouteStore_LockWaitBoundedAfterClientCancel` 实测的耗时。
4. **Redis 集成**。必须用一次性 Redis,不能依赖开发库恰好为空:本机 compose 的 Redis 就是服务在用的 mapping Redis,用例在 `itKeyPatterns` 非空时会 Skip。

   ```powershell
   docker run -d --rm --name ds-it-redis -p 127.0.0.1:6390:6379 redis:7.2 redis-server --appendonly yes   # 本地没有该镜像时要拉取,先征得同意
   $env:DATA_SERVICE_IT_REDIS_HOST = '127.0.0.1:6390'
   go test -tags=integration ./internal/routing/... -count=1 -v
   docker rm -f ds-it-redis
   ```

   通过标准:
   - `TestPublishRoute_RealRedisSameConnection` 和 `TestPublishRoute_RealRedisWaitAOF` 都必须 **PASS,不能 Skip**。前者 FAIL 就按 Q3 改用独立 go-redis 客户端;
   - 第 3 步的 MySQL 环境变量还在时,`TestDBFirstRegister_RealStores` 也必须 PASS;
   - 带 integration tag 时顶层用例共 63 个(单测 54 + 集成 9)。
5. **工具与全量集成**:

   ```powershell
   go test -tags=integration ./internal/routetool/... -count=1 -v
   go test -tags=integration ./... -count=1
   ```

6. **PowerShell 契约测试**:`pwsh -NoProfile -NonInteractive -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`,fail=0,total 由 87 变为 88。评审修复动了第 0 批的 FLUSHDB 共享键清单(补 `player:maprev:*`),再跑一次 `pwsh -NoProfile -NonInteractive -File tools/scripts/tests/k8s_zone_rollback_flush_guard.tests.ps1`,fail=0(用例数不变,仍是 8)。
7. **清库脚本:语法检查与计划输出**:

   ```powershell
   pwsh -NoProfile -Command "foreach(`$f in 'tools/scripts/stress_round19.ps1','tools/scripts/currency_crash_window.ps1'){`$e=`$null;[void][System.Management.Automation.Language.Parser]::ParseFile((Resolve-Path `$f),[ref]`$null,[ref]`$e);if(`$e){`$e;exit 1}}"
   pwsh -NoProfile -File tools/scripts/stress_round19.ps1 -Phase wipe   # 不带 -Execute,只打 plan
   ```

   通过标准:语法零错误;plan 输出里出现 `testdb` 的三张路由表与 `route_meta` 复位。(plan 不连库;`Invoke-GlobalDbSql` 在函数内局部改回 `Continue`、只认 `$LASTEXITCODE` 这一点,只有真清库时才走到。)
8. **可选冒烟(本机全栈)**。开始之前先确认这段时间不跑 dev_tools 的任何 merge-zone 命令(冻结窗口)。

   ```powershell
   cd go/data_service
   go run ./cmd/route_tool -f etc/data_service.yaml import-from-redis                  # dry-run,看统计
   go run ./cmd/route_tool -f etc/data_service.yaml import-from-redis -dry-run=false   # 真导入,期望退出码 0
   go run ./cmd/route_tool -f etc/data_service.yaml audit                              # 期望 0
   ```

   然后临时把本地 yaml 改成 `Mode: db_first`,重启 data_service,确认启动日志里有那条 WARN;用 robot 建一个角色,再跑 audit,应为 0;做完还原 yaml。也可以先按 AGENTS.md §6.2 清库再做。注意不要在 dry-run 之后直接期望 audit 为 0:Redis 里有历史玩家时,dry-run 不写库,audit 必然返回 1。
9. **不需要重编 C++**:本批没有 proto 变更。

**已知未改**:

- guild、login、match、trade、merge_zone 里提到 `RemapHomeZoneForMerge` 的代码注释(`go/guild/internal/logic/home_zone.go`、`go/login/internal/logic/pkg/homezone/homezone.go`、`go/match/internal/team/homezone.go`、`go/trade/internal/logic/home_zone.go`、`tools/merge_zone/{backfill_home_zone,merge_run,player_rows}.go`),留作后续统一更正。
- AGENTS.md §6.2 没有加「全局库路由表一并清」(Q8,待负责人确认)。
- 本提案 §4.1 / §4.2 / §4.3 / §7 的正文没有改写:EVALSHA、1213/1205 重试、删 EXISTS 预检、`data_service -route-import-from-redis` 等说法以上面的偏离表为准。
- [global-data-layer-tidb-decision.md](./global-data-layer-tidb-decision.md) 第 35、82 行(「结论」第 2 条与 D1)、[server_merge_design.md](./server_merge_design.md) §5.5 与 v2 Changelog 的历史叙述仍提到 `RemapHomeZoneForMerge`。它们是历史决策,不改写;前者只在 D7 加注,后者在文首修订提示、§3.5、§3.3 加注。
- [tidb-phase1-and-launch-readiness.md](./tidb-phase1-and-launch-readiness.md)(工作区里别人在途的提案)没有同步 `zone_merge_fence` 的 SPLIT 项,TiDB 决策 §6 第 8 条已登记。

**Java 版(AGENTS.md §12)**:待做。映射存储是服务端内部实现,不涉及客户端契约;Java `PARITY.md` 推迟到第 1 批(1a + 1b)全部完成时登记一行。

---

**第 1a 批热点修复(G1a,2026-10-12):`player_route` 聚簇片前缀 + `route_meta` 拆表 + 建池探针 P4 + `route_tool tidb-prepare`**

> 状态:**已落码,未编译、未测试**(AGENTS.md §10.1,待 Codex 按下面「验证增量」执行)。与第 1a 批一起交验证,任何红都要区分归属(第 0 批 / 第 1a 批 / G1a)。
> 2026-10-13:行片模数由 16 改为 **64**,本段凡涉及片数、尾部条数、预切段数之处已同步;起因、预切密度与「按 16 建过表的库怎么办」见本段之后的「行片 16 → 64」。

**起因**。上面「仍未解决的风险」第 1 条:`player_route` 主键 `player_id NONCLUSTERED + SHARD_ROW_ID_BITS` 只打散行数据,主键退化成的唯一索引、`idx_route_updated`、单 zone 内的 `idx_route_home` 都是单调尾部;`PRE_SPLIT_REGIONS` 只切行数据,每条索引建表时只有 1 个 Region;号段发的 `player_id` 在每个 login 段内严格递增,首日数据量到不了按大小分裂的阈值,新插入始终打同一个尾部 Region(Load Base Split 只看读)。`zone_merge_fence` 整表一个 Region;`route_meta` 单行同时放「每次 W1 都读的 writable」和「1b leader 常写的水位 / 租约」。开服首日瓶颈核对把它列为 TiDB 档的首要问题之一([tidb-phase1-and-launch-readiness.md](./tidb-phase1-and-launch-readiness.md) §5.1)。**为什么现在做**:这几张表还没在任何环境部署,第 1a 批代码也未编译;TiDB 建表后不能改聚簇属性 —— 现在改只是改常量,上线后再改等于整表重写 + 双写迁移。

**改动清单**

| 位置 | 要点 |
|---|---|
| `go/shared/placement/route.go` | `RouteRowShards uint32 = 64`(写死、不可配;2026-10-13 由 16 改为 64)、`RouteRowShard(id) = id % 64`;写者与读者都只能经它算 |
| `store/schema.go` | `player_route` 聚簇 `(route_shard, player_id)` + `chk_route_shard` + 三条带片前缀的二级索引,去掉 `SHARD_ROW_ID_BITS / PRE_SPLIT_REGIONS`;`route_meta` 只留 `id / generation / writable`,新表 `route_reconcile`;五张表显式 `COLLATE=utf8mb4_bin`、ascii 列 `COLLATE ascii_bin`;`bootstrapTables` 7 项;形状守卫对 `player_route` 的任一失败附 G1a 处理提示;`ensureRouteTables` = 形状守卫 + 种 `route_meta` + 种 `route_reconcile` |
| `store/route_store.go` | 按 `player_id` 的全部 SQL 带 `route_shard`(R4–R9、R12、R13);R8 / R13 用行构造器 `(route_shard, player_id) IN ((?, ?), …)`,按聚簇键升序;R12 键集游标改 `RouteScanCursor{Shard, PlayerID}`;导入的 R7 / R9 按聚簇键升序执行;扫描读到错片行 → `ErrRouteShardMismatch`;建池探针 P4;R16 种对账行;W1 里围栏片改名 `fenceShard`、行片 `rowShard` 在事务函数内算 |
| `store/route_tidb.go`(新) | `PrepareTiDB` 与纯函数 `routeTiDBPrepareStatements`:判引擎 → 核对 `TIDB_PK_TYPE` → `ATTRIBUTES 'merge_option=deny'` ×2 → `SPLIT` ×(3 + zones + storages) |
| `routetool/{importer,auditor}.go` | `routeDB.ScanRoutes` 签名改游标类型;audit 方向 A 沿聚簇键翻页 |
| `routetool/tidb_prepare.go`(新)、`cmd/route_tool/main.go` | 子命令 `tidb-prepare -zones=… [-storages=…] [-dry-run=true]`;不连 mapping Redis;参数按子命令登记 |
| 两份清库脚本 | `route_meta` 只复位 `writable`;`route_reconcile` 存在时复位 leader / 租约 / 水位;注释写明 TiDB 清库后要重跑 `tidb-prepare` |
| 测试 | placement `TestRouteRowShard`;store 的 DDL 契约 / SQL 清单 / 游标序 / 语句生成;假驱动的行片 ≠ 围栏片(4250)、导入键序、错片行、P4、`PrepareTiDB`;真库的 CHECK、旧形状守卫、P4、错片行、EXPLAIN;可选真 TiDB 用例;routetool 跨片翻页与 `tidb-prepare` 输出;cmd 参数与「不连 Redis」 |

**新表形状与读方怎么用**

| 索引 | 形状 | 写入尾部(新号) | 读方 |
|---|---|---|---|
| PRIMARY | `(route_shard, player_id)` CLUSTERED | 64 条 | 1a:W1、导入、`GetRoutes`、audit;1b:全量预热;第 3 批:回源 |
| `idx_route_home` | `(home_zone, route_shard, player_id)` | 每 zone 64 条 | 第 2 批 W10 pin-placement;第 3 批 R7 `GetAllPlayerIDsInZone` 等 |
| `idx_route_storage` | `(storage_id, route_shard, player_id)` | 每个落点值 64 条 | 第 2 / 3 批 relocate 清单等 |
| `idx_route_updated` | `(route_shard, updated_at_ms, player_id)` | 64 条 | 1b 增量推送 |

后续批次的读方照抄下面的模板(**结果不再按 `player_id` 排序**,游标从 `player_id` 变成 `(shard, id)`):

| 用途 | SQL | 游标 / 起点 |
|---|---|---|
| 按 zone 枚举(R7、W10) | `SELECT route_shard, player_id[, …] FROM player_route WHERE home_zone = ? AND (route_shard > ? OR (route_shard = ? AND player_id > ?)) ORDER BY route_shard, player_id LIMIT ?`(W10 再加 `AND storage_id IS NULL` 与 `FOR UPDATE`,锁序分析留给第 2 批规格) | `(shard, id)`,起点 `{0, 0}`;走 `idx_route_home`、无 filesort |
| 按落点枚举 | 同上,`home_zone` 换 `storage_id` | 同上;走 `idx_route_storage` |
| 1b 增量推送 | 按片循环 `s = 0..63`(上界用 `placement.RouteRowShards`,不写字面量;每拍 64 条查询):`SELECT route_shard, player_id, <cols> FROM player_route WHERE route_shard = ? AND (updated_at_ms > ? OR (updated_at_ms = ? AND player_id > ?)) ORDER BY updated_at_ms, player_id LIMIT ?` | 每片一个 `(updated_at_ms, player_id)` 游标(共 64 个);「`>= since`」的起点写成 `updated_at_ms > since OR (updated_at_ms = since AND player_id > 0)` |
| 1b 全量预热 | 即 R12(`ScanRoutes`) | `RouteScanCursor` |
| 按 id 回源(第 3 批) | R5 的不加锁形态;批量即 R13 | — |

**`player_id` 唯一性与探针 P4**。主键 `(route_shard, player_id)` 只在 `route_shard` 由 `player_id` 唯一决定时才等价于「`player_id` 唯一」,所以唯一性现在靠 `chk_route_shard`。CHECK 不生效(TiDB 默认 `tidb_enable_check_constraint=OFF`、MySQL < 8.0.16)时,一个绕过 store 或算错片的写者能给同一 `player_id` 写出两行,而 R5 / R8 / R13 都按「正确片 + id」点查,**错片行对它们不可见、表现为缺席**,导入还会再插一条正确片的行。读路径兜不住,所以:建池探针第 4 步 P4 在必然回滚的事务里插一行只违反 `chk_route_shard` 的行,必须收到 3819 且报错点名该约束,否则 `ErrRouteCheckNotEnforced` 拒建 store(db_first 的 data_service、debugutil、route_tool 都经过它);全表键集扫描(`ScanRoutes`,即 audit 方向 A / 1b 全量预热)校验每行的片,错片即 `ErrRouteShardMismatch`;不另加 `UNIQUE(player_id)`(那正是要消掉的单调尾部)。P4 只证明 CHECK 生效,不证明模数是 64(由 DDL 契约测试钉住;探针行对 `% 16` 与 `% 64` 同样违例,按旧模数 16 建的表也过得了 P4,见下「行片 16 → 64」的前置)。2026-10-14 起建池探针第 5 步 P5 在同一个回滚事务里再插两行按 64 合法的行核对模数,见下「正向探针 P5」。

**对 §5.4 / 第 1b 批的约束**

- leader 租约、`watermark_ms`、`cache_verified_ms` 的读写改在 `route_reconcile`;`writable`、`generation` 留在 `route_meta`。`mapping:route:ready` 里的水位来自 `route_reconcile`。
- **回档检测跨两表**:「水位倒退 → 置 `writable=0`」原来可以是同一行上的条件更新,拆表后要在一个事务里读 `route_reconcile`、写 `route_meta`(或先置 `writable=0` 再处理水位),1b 规格要写明原子范围。
- leader 标识必须是 ASCII(`route_reconcile.leader` 是 `ascii`,与 `NewRouteWriterID` 同源),非 ASCII 字节在严格模式下会被库拒绝。
- `route_meta` 设成 TiDB 缓存表之后(S2 起必选,见「TiDB 运维步骤」第 6 条),冻结写 `UPDATE route_meta SET writable=0` 最长阻塞一个读租约(`tidb_table_cache_lease`,默认 3s【未核实】),1b 的冻结步骤超时要 ≥ 租约 + 余量;在途事务仍按「仍未解决的风险」第 3 条处理。
- 增量推送按片循环 `s = 0..63`(见上表;每拍 64 条查询、64 个游标,上界取 `placement.RouteRowShards`);全量预热即 `ScanRoutes`。
- `route_tool tidb-prepare` 要随第 1b 批并进主程序入口(同偏离第 8 条)。
- **新的 `player_route` 写者**(第 2 批 merge_zone 等):直接调 `placement.RouteRowShard`(merge_zone 已 import `shared/placement`),并在开写前过与 P4 / P5 同等的 CHECK 实测与模数核对(或复用 `RouteStore`)。CHECK 不生效的库上,任何绕过探针的写者都能给同一 `player_id` 写出两行,点读看不到。

**TiDB 运维步骤**

1. 建表**之前**:`SET GLOBAL tidb_enable_check_constraint=ON`(TiDB ≥ 7.2),否则 P4 拒建 store。
2. 建表之后(Phase 1 预建表之后)跑 `route_tool -f <yaml> tidb-prepare -zones=<全部 zone> -dry-run=false`;它先核对两张表 `TIDB_PK_TYPE=CLUSTERED`,不是就退出码 2(趁表空 DROP 后用 `-migrate` 重建)。默认 `-dry-run=true` 只列语句。
3. 开新 zone:在该 zone 对外开放(第一次建角)之前、低峰跑 `tidb-prepare -zones=<新 zone>`(带上全部 zone 也行,幂等)。两条 `ALTER … ATTRIBUTES` 是 DDL,在线重跑可能让在途建角以 8028 失败一次(HR9)。
4. BR / PITR 恢复之后、TRUNCATE 之后重跑(换表 ID,预切与 table attributes 不跟过去;BR / TiCDC 也跳过 `ATTRIBUTES` DDL)。
5. Phase 2 打开 `PinOnCreate` 指向 1000000 时加 `-storages=1000000`。
6. `ALTER TABLE route_meta CACHE`(撤销 `NOCACHE`):**S2 档起必选**,S1 可选(2026-10-13 按容量模型 §4.1「连带修正」,由「可选、默认不开」改定。依据是容量模型 §2.6 的「route_meta 的 R1 单行读」一行:每次 W1 一次点读,压在单个 Region leader 上,S2 上沿约 2.5 万/s,设成缓存表后为 0;数字是推算,没有压测)。目前只是运维步骤,`tidb-prepare` 不发这条语句,要不要并进去留给第 1b 批。缓存表上不能直接做 DDL、BR / TiCDC 是否保留 CACHE 属性都【未核实】,按「不保留、恢复后重放」处理。
   > 2026-10-18 更正(第 1b-1 批):「要不要并进去」已定为**不并进**,见下「第 1b-1 批」运行时要点第 8 条:这条语句由 TiDB Phase 1 的部署步骤执行(建表之后、开服之前;恢复之后重放),`tidb-prepare` 不发;设成缓存表之后 `route_tool freeze` / `unfreeze` 的 `-freeze-timeout` 要 ≥ `tidb_table_cache_lease` + 2s。Phase 1 一侧的归属写在 [tidb-phase1-and-launch-readiness.md](./tidb-phase1-and-launch-readiness.md) §9.3 第 21 项与 §6.2「恢复后必做」的同日更正。原文不删。
7. MySQL 档:`tidb-prepare` 打印 `SKIP` 并退出 0;G1a 在 MySQL 上不改变容量(InnoDB 主键本来就聚簇,插入点从 1 个变 64 个,推断无可见影响)。

**相对本提案与第 1a 批的偏离(H)**

| # | 偏离 | 理由 | 代价 |
|---|---|---|---|
| H1 | `player_route` 主键 `player_id NONCLUSTERED + SHARD` → 聚簇 `(route_shard, player_id)`(§3.2、§3.1) | 消掉主键唯一索引的单调尾部;TiDB 建表后不能再改 | `player_id` 唯一性改由 PK + CHECK 共同保证,多一个建池探针;所有按 id 的 SQL 都要带片 |
| H2 | `RouteRowShards = 64` 写死、不可配(2026-10-13 由 16 改为 64,见下「行片 16 → 64」) | 改它 = 整表重写 | 以后想要别的片数(后备 256)只能新表迁移;列是 `tinyint unsigned`,上限 256 |
| H3 | 二级索引全部带片前缀,`idx_route_updated` 加 `player_id` 尾列 | 每条索引 64 尾;键集游标有确定的平局决胜 | 按 zone / storage 枚举的游标从 `player_id` 变成 `(shard, id)`,结果不再按 `player_id` 排序 |
| H4 | `route_meta` 拆成 `route_meta` + `route_reconcile` | TiDB 上读不被 leader 写锁拖住;开关表才可能设缓存表 | 五张表;1b 读写两张;清库脚本与 runbook 要改 |
| H5 | 建池探针 P4(回滚事务里一次必失败的 INSERT) | 唯一性依赖 CHECK,元数据查询在两种引擎上口径不一,实测最可靠 | 每次建池多 3 条语句;只读用途(`route_tool audit`)也要 `player_route` 的 INSERT 权限;真库套件(含 routing 的 `TestDBFirstRegister_RealStores`)要求 MySQL ≥ 8.0.16(更老是 FAIL 不是 SKIP) |
| H6 | `ScanRoutes` 的游标与顺序改为聚簇键(`routeDB` 接口签名变) | 键集翻页必须沿聚簇键 | audit 明细不再按 `player_id` 升序;审计窗口内新提交、且缓存三键全无的行若落在已扫过的片里,本次方向 A 看不到、方向 B 也不查它,要到下一次 audit 才报 |
| H7 | 新增 `route_tool tidb-prepare`(§7 与偏离第 8 条之外的新入口) | 预切要真实 zone 清单,迁移拿不到;运维可重放 | 多一个要记得跑的步骤;K8s 上依赖第 1b 批并进主程序;漏跑只影响性能 |
| H8 | 对 `player_route` 也预切(原任务只点了围栏) | 不预切,片前缀在首日不生效 | Region 数 ≈ 63 + 63 + 8·Z + 8·S + Z(2026-10-13 行片改 64 之后:行数据与 `idx_route_updated` 每片一段;`idx_route_home` / `idx_route_storage` 每个 zone / 落点值只切 8 个点,即每 8 片一段,见下「行片 16 → 64」。16 片时是 15 + 15 + 16·Z + 16·S + Z),空 Region 靠 `merge_option=deny` 永久留住;Z 上百时 `idx_route_home` 的空 Region 仍以千计,届时可把语句生成改成每 zone 只切 `(z,0,0)`(只改 `route_tidb.go`) |
| H9 | 显式排序规则(`utf8mb4_bin` / `ascii_bin`;`route_reconcile.leader` 由 utf8mb4 改 ascii) | 两种引擎默认不同,建表前改最便宜 | 与 MySQL 现状默认的 `utf8mb4_0900_ai_ci` 不同;这些列都是标识符,逐字节比较是正确语义 |
| H10 | 建池探针 P5(2026-10-14,P4 的同一个回滚事务里再插两行按当前模数合法的行) | 模数建表后改不了,按另一个模数建的表能一路通过迁移、形状守卫与 P4 | 每次建池再多 2 条 `INSERT`;正确的库上这两行真的插入(未提交)、持锁到回滚,63 / 64 号玩家的 W1 与并发建池会短暂排队;探针键缺席时三方以上并发建池可能 1213,建池整事务有界重跑(2026-10-15 补);redis 模式不建 store、认不出。详见下「正向探针 P5」 |

**风险(HR)**

1. **HR1 TiDB 细节大量未核实**(本批无法联网对照官方文档):SPLIT 元组写法、重复 SPLIT 是否幂等、`ATTRIBUTES` 是否覆盖索引 Region、所需权限、`TIDB_PK_TYPE` 列、缓存表语义、TiDB 的 3819 文案、行构造器 IN 的计划。缓解:真 TiDB 用例 `TestRouteStore_TiDBClusteredShardsAndPresplit` 与验证增量第 8 步;任何一项不成立,改 `route_tidb.go` 的语句生成或探针判据,DDL 不动。
2. **HR2 CHECK 是唯一性的一部分**:P4 只在建池时证一次;运行期有人 `ALTER … NOT ENFORCED`、DROP CHECK 或把 `tidb_enable_check_constraint` 改回 OFF,要到下次重启才发现;其间写进来的错片行只有 `ScanRoutes`(audit 方向 A)看得见。runbook 写明不许关这条 CHECK,改过 DDL / 全局变量之后必跑 audit。
3. **HR3 存量 snowflake 号偏向片 0**:bwmarrin 布局最低 9 位是同毫秒序号,低速铸号时多为 0(AGENTS §7.1),`% 64` 几乎都落片 0(同毫秒序号占最低 9 位,模 64 只看最低 6 位)。只影响一次性导入与冷读的分布,不影响新号(号段稠密、均匀)。
4. **HR4 本机旧形状表**:G1a 之前跑过第 1a 批迁移的库,`player_route` 会让迁移失败(fail-closed,带处理提示)。确认路由表里没有要保留的数据后手工 `DROP TABLE player_route, zone_merge_fence, zone_merged_into, route_meta;`(`route_reconcile` 若存在一并 DROP)再 `-migrate`;**只 DROP `player_route` 不够**(旧 `route_meta` 与旧排序规则守卫查不出来)。代码不自动 DROP。**2026-10-13 补:按模数 16 建过表的库(跑过 HEAD `d5cbb8a8e` 的迁移或启动过那一版 data_service)同样要这样 DROP,而且这一种迁移不会报错**,见下「行片 16 → 64」的前置。2026-10-14 补:db_first 的 data_service 与 route_tool 建池时由 P5 认出(`ErrRouteShardModulusMismatch`),redis 模式认不出(见下「正向探针 P5」)。
5. **HR5 `idx_route_storage` 的 NULL 段没切**:`PinOnCreate=false` 时新号 storage 为 NULL,这 64 条尾部仍在一个 Region;`SPLIT … BY (NULL, …)` 的写法未核实。打开 `PinOnCreate` 后按 `-storages=1000000` 切。
6. **HR6 围栏仍是每 zone 一个 Region**:单 zone 开服尖峰时,该 zone 的全部 W1 在一个 Region leader 上加锁 / 提交。缓解梯子见 §12「万一目录还是成了瓶颈」第 ③ 步。
7. **HR7 TRUNCATE / BR / TiCDC 丢预切与属性**:两份清库脚本只连本机 MySQL;TiDB 环境每个恢复 / 清库步骤后重跑 `tidb-prepare`。漏跑 = 回到单尾,只有性能后果。
8. **HR8 叠在未验证的 1a 上**:G1a 改的就是 1a 未编译的代码,两者一起交 Codex。1a 自己的风险(上面「仍未解决的风险」)全部仍在,G1a 只处理第 1 条里的 TiDB 热点部分;MySQL 单机连接预算(L-03)与 Redis 单主(L-06)不在本批。
9. **HR9 在线跑 `tidb-prepare` 的 DDL 副作用**:`ALTER TABLE … ATTRIBUTES` 可能让在途 W1 悲观事务以 8028 失败一次【未核实】,W1 只重试 1213 / 9007 → 那一次建角 Unavailable,由客户端重试。缓解:开新 zone 时在开放前、低峰执行;真 TiDB 用例在二次预切期间并发建角并原样记录。若实测确有,再评估「属性已在就跳过 ALTER」或给 W1 加 8028 重试(改变既有重试口径,另议)。

**验证增量**(插在上面「第 1a 批交 Codex 的验证」里执行:先复验清单 → 第 0 批 → 第 1a 批第 1–9 步,各步按下面替换 / 追加)

- 前置:本机 `testdb` 若跑过 G1a 之前的第 1a 批迁移,按 HR4 处理;若跑过模数还是 16 的那一版(`d5cbb8a8e`)的迁移,按「行片 16 → 64」的前置处理(这一种迁移不报错,要先 `SHOW CREATE TABLE player_route` 看);不确定时停下来问负责人。P4 让所有建 route store 的真库用例要求 MySQL ≥ 8.0.16,跑第 3 步前先 `SELECT VERSION()` 记下版本。
- 第 1 步:placement 顶层用例 15 → **16**(新增 `TestRouteRowShard`);另跑 `go test ./placement/... -list . | Measure-Object -Line`(期望 16 行用例 + 1 行 ok)。
- 第 2 步:`-run` 正则追加 `TiDBPrepare|ScanCursor|RouteRowShard|ShardMismatch`,并跑 `go test ./internal/store/... ./internal/routetool/... ./cmd/route_tool/... -count=1 -v -run "Route|TiDBPrepare|ScanCursor|DecideImport|RegisterHome|ImportRoutes|NewRouteStore|ShardMismatch"`;routing 顶层用例**不变**(54)。
- 第 3 步:`go test -tags=integration ./internal/store/... -count=1 -v -run "Route|MigrateSchema"` 与 `go test -tags=integration ./internal/routetool/... -count=1 -v`;`TestRouteStore_KeyedReadsUsePrimaryRange` 打印的 EXPLAIN 计划**原样保留**(若 `type=ALL` 如实回报,不改 SQL);`TestRouteStore_TiDBClusteredShardsAndPresplit` 在 MySQL 上 SKIP 属预期。
- 第 4 步:按原样跑;`TestDBFirstRegister_RealStores` 必须 PASS(它会经过 P4)。routing 带 integration 仍是 63。
- 第 7 步:plan 里应出现三张数据表的 TRUNCATE、`route_meta(id=1) writable reset`、`route_reconcile(id=1) reset`。
- 新增 route_tool 冒烟(本机 MySQL;**不要用 `go run` 断言退出码**,它对子进程非 0 退出一律自己退 1):

  ```powershell
  cd go/data_service
  go build -o "$env:TEMP\route_tool_g1a.exe" ./cmd/route_tool
  & "$env:TEMP\route_tool_g1a.exe" -f etc/data_service.yaml tidb-prepare -zones=1,2; "exit=$LASTEXITCODE"   # 期望 exit=0,stdout 含 engine=mysql skipped=true
  & "$env:TEMP\route_tool_g1a.exe" -f etc/data_service.yaml tidb-prepare; "exit=$LASTEXITCODE"            # 期望 exit=2,stderr 含 -zones
  Remove-Item "$env:TEMP\route_tool_g1a.exe"
  ```

- 可选真 TiDB(拉取 `pingcap/*` 镜像属于下载,先征得用户同意;只在本机 dev TiDB 上做):`docker compose -f deploy/docker-compose.tidb.yml up -d`,用 root 先 `SET GLOBAL tidb_enable_check_constraint=ON` 并建一个随机口令的测试账号(口令不进日志不进 git),`DATA_SERVICE_IT_MYSQL_HOST=127.0.0.1:4000` 后只跑 `go test -tags=integration ./internal/store/... -count=1 -v -run "TiDB"`(**不要**对 TiDB 跑 `-run "Route"` 全集:全局库迁移在 TiDB 上会因 `player_snapshot` 的可空唯一键失败)。回报每条 SPLIT 的 `TOTAL_SPLIT_REGION`、三次 `SHOW TABLE … REGIONS` 的行数、二次 `PrepareTiDB` 是否报错、期间并发 `RegisterHome` 的结果(有无 8028)、R13 的计划、P4 在 TiDB 上的原始报错文本。
- 不需要重编 C++:无 proto 变更。

**已知未改**:§2 总览表「4 张表」与「`route_meta.watermark_ms` 回退」、§4.3 W10 / §5.2 R7 正文、§5.4 正文、§8.2「`route_meta.watermark_ms` / `generation` / `writable`」仍按旧口径叙述,以本段为准(水位在 `route_reconcile`,枚举游标是 `(shard, id)`);AGENTS.md §6.2(Q8)仍未改。

**Java 版(AGENTS.md §12)**:本项是服务端内部存储的表形状,不涉及客户端契约(帧、客户端可见 proto、tip 码、配表都不变),不需要与 Java 同批改。Java 版的映射持久化(D1)整体仍是「待做」,登记时机按 Q14。给 Java 的建议:做等价的「库为真源」目录时,若后端是 TiDB,同样避免以单调 `player_id` 作主键 / 唯一索引的唯一前缀(按本仓同口径的 64 片前缀,或 Java 选型的等价方案),并在 `PARITY.md` 写明取舍。

---

**行片 16 → 64(2026-10-13,容量模型 [capacity-model-hok-scale.md](./capacity-model-hok-scale.md) §4.1)**

> 状态:**已落码,未编译、未测试**(AGENTS.md §10.1,待 Codex 验证)。16 的版本已随 `d5cbb8a8e` 提交并推送,所以这不是「建表前改个常量」那么干净:任何跑过那一版迁移的库都要先处理(见「前置」)。

**为什么改**。单 Region 的写入上限 B 没测过,而模数建表后改不了(改它 = 整表换主键重写)。S3 开服上沿时每条尾部 = C_p / R:R=16 是 3,125/s,R=64 是 781/s;取 64 是保险,理由不是「16 已经证明不够」。后备:V5 实测 B 低于 800/s 时取 256(列是 `tinyint unsigned`,上限 256)。数字都是容量模型的推算,没有压测。

**改了什么**

| 位置 | 要点 |
|---|---|
| `go/shared/placement/route.go` | `RouteRowShards` 16 → 64;注释写明模数建表后不可改、按 16 建过表的库要先 DROP 五张路由表 |
| `store/schema.go` | `chk_route_shard` 的 `% 16` → `% 64` |
| `store/route_store.go`、`routetool/auditor.go`、`cmd/route_tool/main.go` | 只改注释与一条错误文案(`ErrRouteShardMismatch` 不再写模数字面量);逻辑一律经 `placement.RouteRowShard`,没有别处写死模数 |
| `store/route_tidb.go` | 预切密度见下;新增具名常量 `routeTiDBPrefixSplitPoints = 8`、`routeTiDBPrefixSplitStride = RouteRowShards / 8`,以及「切点数整除行片数」的编译期断言(2026-10-14 由「间隔整除行片数」收紧:后者在切点数取 24 时间隔为 2、照样整除,循环却切出 32 个点) |
| 测试 | 钉 16 的向量全部换成 64 下仍测得到原意图的值:「同片」的 id 从相差 16 换成相差 64(17 / 33 → 65 / 129 等);导入契约用例的 id 换成 100..300(行片 36 / 22 / 8 / 58 / 44,键序 ≠ id 序);`TestRegisterHomeBindsRowShardNotFenceShard` 的行片 10 → 26;`TestRouteTiDBPrepareStatements` 的 8 条语句按新密度重写 |
| `etc/data_service.yaml` | `RouteStore.FenceShards` 8 → 16(MySQL 档;TiDB 档 64)。只是配置,代码默认值仍是 8;K8s ConfigMap 不镜像这个键,仍取默认 8,切 db_first 前按档位补 |

**预切密度(`tidb-prepare`)**

| 对象 | 切点 | 段数 | 依据 |
|---|---|---|---|
| 行数据、`idx_route_updated` | 片 1..63 的起点,各 63 个 | 各 64 段,每片一段 | 切点数是常数、不乘 zone 数;沿用 16 片时「每条尾部独占一个 Region」的做法 |
| `idx_route_home`、`idx_route_storage` | 每个 zone / 落点值 8 个点:片 0、8、16、…、56 | 每个 zone / 落点值 8 段,每段 8 片 | 容量模型 §4.1 理由 3「把每 zone 预切改成 8 个点」;H8 预留的改法。`s=0` 那个点仍然隔开相邻 zone |
| `zone_merge_fence` | 每 zone 一个点 | 每 zone 一个 Region | 本步不改(容量模型建议每 zone 4~8 个点,另议) |

Region 总数 ≈ 63 + 63 + 8·Z + 8·S + Z。行数据与 `idx_route_updated` 该不该也降到 8 段,容量模型与 H8 都没有给结论,这里取每片一段;若压测显示空 Region 数成问题,只改 `route_tidb.go`,不动 DDL。按 zone 的两条索引每个 Region 承 8 条尾部,单 zone 开服尖峰时每个 Region 的写入 = 该 zone 建角速率 / 8,是否够用由 V5 压测定。

**前置(改之前建过表的库)**。迁移是 `CREATE TABLE IF NOT EXISTS`,不会改已有的表;形状守卫只查列名与主键,探针 P4 的违例行对 `% 16` 与 `% 64` 同样违例 —— **按 16 建的表会一路通过迁移与建池**,之后 db_first 下的 W1 与 route_tool 导入遇到 `player_id % 64 ≥ 16` 的玩家(号段发的新号里占四分之三)全部撞 3819 失败(fail-closed,不写坏数据)。所以对任何可能跑过 `d5cbb8a8e` 的迁移、或用那一版启动过 data_service 的库(redis 模式也会建表):

1. `SHOW CREATE TABLE player_route`,看 `chk_route_shard` 里是 `% 16` 还是 `% 64`;
2. 是 `% 16`:确认五张路由表里没有要保留的数据后,按 HR4 手工 `DROP TABLE player_route, zone_merge_fence, zone_merged_into, route_meta, route_reconcile;`,再 `data_service -f <yaml> -migrate`;不确定就停下来问负责人;
3. TiDB 上 DROP 重建之后重跑 `route_tool tidb-prepare`(预切与 `merge_option` 不跟着新表)。

代码不自动 DROP;本步只做机械改动,还没有「建池时认出旧模数」的正向探针(留作下一步),在那之前只能靠上面的人工核对。**2026-10-14 补**:正向探针 P5 已落码(见下)。上文「按 16 建的表会一路通过迁移与建池」里的「建池」从此不成立(建池时 P5 认得出),「迁移」仍成立 —— P5 只在建 route store 时跑,redis 模式的 data_service 不建 store,所以上面的人工核对保留。

**连带修正(容量模型 §4.1,本步只改文档与配置)**

- `RouteStore.FenceShards`:MySQL 档 16(已写进 `etc/data_service.yaml`),TiDB 档 64。任何时候可改 —— 围栏持有者总是锁满 0..63 全部分片行。
- `route_meta CACHE`:由「可选」改为 **S2 起必选**(上面「TiDB 运维步骤」第 6 条)。
- 发布脚本 EVAL → EVALSHA:列为 **S2 前置**(偏离第 2 条与 Q2 已加注);代码仍是 EVAL。
- `zone_merge_fence` 每 zone 预切 4~8 个点、WAITAOF 的 `appendfsync always` 或调大连接池:未做,仍在容量模型的清单里。
- 1b 增量推送每拍的查询数随行片数从 16 条变成 64 条(上面读方模板已改)。

**交 Codex 的验证**(工作目录以仓库根为起点;不写死 Go 版本,按各模块 go.mod;串行,前一步红了就停下归因)

1. 前置:对本机 / kind / staging 里任何可能建过路由表的库执行上面「前置」第 1~2 步,记录每个库的 `SHOW CREATE TABLE player_route` 结果。
2. `cd go/shared`:`gofmt -l ./placement`(无输出)、`go vet ./placement/...`、`go test -count=1 ./placement/...`;`TestRouteRowShard` 必须 PASS。
3. `cd go/data_service`:`gofmt -l ./internal/store ./internal/routetool ./internal/config ./cmd/route_tool`(无输出)、`go build ./...`、`go vet ./...`、`go test -count=1 ./internal/store/... ./internal/routetool/... ./internal/config/... ./cmd/route_tool/...`。`go build` / `go vet` 是整个模块,会带上工作区里别的在途改动,报出问题按文件归因。
4. 真库(需要 `DATA_SERVICE_IT_MYSQL_*`,MySQL ≥ 8.0.16):`go test -tags=integration -count=1 -v ./internal/store/... -run "Route|MigrateSchema"` 与 `go test -tags=integration -count=1 -v ./internal/routetool/...`;`TestRouteStore_KeyedReadsUsePrimaryRange` 的五份 EXPLAIN 原样保留。
5. 可选真 TiDB:同上面「验证增量」的做法,只跑 `-run "TiDB"`;回报 `SHOW TABLE player_route REGIONS`(期望 ≥ 64)与 `SHOW TABLE player_route INDEX idx_route_home REGIONS`(1 个 zone,期望 ≥ 8)的行数。

(2026-10-15 补)上面各步失败时保留什么,见下「正向探针 P5」段末的「2026-10-15 补登」;命令不变。

**Java 版(AGENTS.md §12)**:不涉及客户端契约,不需要同批改;Java 版的 D1 仍是「待做」。

---

**正向探针 P5:建池时认出按另一个行片模数建的表(2026-10-14)**

> 状态:**已落码,未编译、未测试**(AGENTS.md §10.1,待 Codex 按下面的验证执行)。负责人 2026-10-13 拍板加机械守卫;评审曾建议把模数写进约束名(`chk_route_shard_mod64`),未采纳,DDL 契约不变。

**起因**。上面「前置」:按 16 建过的表,迁移(`CREATE TABLE IF NOT EXISTS`)、形状守卫(只查列名与主键)、P4(违例行 `(1, 0)` 对任何模数都违例)全都通过,要到运行期 W1 / 导入对 `player_id % 64 ≥ 16` 的新号大面积撞 3819、建角 Unavailable 才暴露,启动期没有任何信号。

**做法**(`store/route_store.go` 的 `probeRouteShardModulus`)。P4 收到「3819 且点名 `chk_route_shard`」之后,在**同一个必然回滚的事务**里按聚簇键升序再插两行「按 N = `placement.RouteRowShards` = 64 合法」的行。其余列与 P4 相同:home / 落点 / 冻结全 NULL、`rev=1`、`updated_by='route-probe'`、两个时间戳 0。已逐条对过 `player_route` 的六条 CHECK 与全部 NOT NULL,正确的库上不会误报。表的模数为 M 时:

| 探针行 `(route_shard, player_id)` | 被接受的条件 |
|---|---|
| `(0, N)` = `(0, 64)` | `N % M = 0`,即 M 整除 N(于是 M ≤ N) |
| `(N−1, N−1)` = `(63, 63)` | `(N−1) % M = N−1`,即 M ≥ N |

两行都被接受 ⇔ M = N,对任何 M ≥ 1 成立。N = 64 的判别表(`TestRouteShardModulusProbeDiscriminates` 钉住,并对 M = 1..256 穷举):

| M | `(0, 64)` | `(63, 63)` | 结论 |
|---|---|---|---|
| 16 | 收 | 拒 | 不一致 |
| 32 | 收 | 拒 | 不一致 |
| 64 | 收 | 收 | 一致 |
| 128 | 拒 | 收 | 不一致 |
| 192 | 拒 | 收 | 不一致 |
| 256 | 拒 | 收 | 不一致 |

设计时设想过的第三行 `(1, N+1)` 不需要:它要补的 M = 128 已经被 `(0, 64)` 拒(64 % 128 = 64 ≠ 0),上面两行对全部 M 已经完备。

逐行判定(所有分支都回滚):

- 成功或 1062 → 该行被接受。1062 = 库里已有这一行。CHECK 生效的表上已有的行都满足它,所以撞键同样说明该行在表的模数下合法,与引擎先判 CHECK 还是先判撞键无关(CHECK 被关掉期间写进来的行除外,属 HR2)。号段从 1 发号,63 / 64 号是开服后最早的真实玩家,撞键在生产上是常态;
- 3819 且点名 `chk_route_shard` → `ErrRouteShardModulusMismatch`,拒建 store。文案:表的行片模数与二进制不一致;确认无数据后按 HR4 DROP 五张路由表再 `-migrate`;不要改常量迁就旧表;
- 1142 → 与 P4 同一条权限提示(两步都是回滚的 INSERT,不新增权限要求);
- 3819 点名别的约束、1205、其它任何错误 → 原样包装,拒建 store(fail-closed)。其中 1213 / 9007 由建池整事务有界重跑(2026-10-15 补,见下「风险」的锁一条)。

**作用范围**。只在建 route store 时跑:db_first 的 data_service(失败只记 ERROR、store 不装配,`Store.Required=true` 时拒启)、debugutil、route_tool(退出码 2,stderr 提示「只重跑 -migrate 修不好」)。**redis 模式的 data_service 不建这个 store**,它启动期的迁移照样会建出(或留着)旧模数的表而 P5 挡不住,所以上面「前置」的人工 `SHOW CREATE TABLE player_route` 核对保留,runbook §9.3 前置第 1 条同步。

**风险与未核实项**

- 锁:模数一致时两行真的插入(未提交),撞键时在已有行上取共享锁(评审按 mysql-server 8.0.36 源码 `row0ins.cc` 核对,未实跑;真库用例 `TestRouteStore_ProbeKeysHeldByRealPlayers` 取证),都持到回滚(毫秒级)。63 / 64 号玩家的 W1 可能短暂排队;反过来他们的 W1 持锁超过会话锁等待 1s 时,这次建池以 1205 失败(不重试),重启即可。**2026-10-15 更正**(原句「多副本同时建池……不成环」不成立):各副本按同一顺序插这两行,两行之间不成环;但三个及以上会话并发插同一个**缺席**的探针键时(空表、首次部署,或 63 / 64 号还没建角),先插入者回滚后,其余等待者查重取的 S 锁会被继承成 gap 锁(MySQL 8.0 在 RC 下同样继承,评审按源码 `lock0lock.cc` 推断、未实跑),它们重新插入要插入意向锁,互等成 1213。现已按 1213 / 9007 对 P4 + P5 整个事务有界重跑(与写事务同一预算 `routeTxAttempts`、同一退避,受探针总时限 5s 约束;1205 仍不重试),见下「2026-10-15 评审修复」。
- TiDB:P4 的语句失败之后同一事务能否继续用(语句级回滚)【未核实】,`TestRouteStore_TiDBClusteredShardsAndPresplit` 的建池就是取证。不成立时建池会以 P5 原样包装的错误失败(fail-closed),届时改成 P5 另开一个回滚事务,判别表不变。
- 1062 的语义依赖「已有行都满足当前 CHECK」:CHECK 关掉期间写进来的错片行恰好占了探针键时会漏判,检测仍靠 audit 方向 A(HR2)。

**改了什么**

| 位置 | 要点 |
|---|---|
| `store/route_store.go` | 新 SQL `routeProbeShardModulusSQL`、纯函数 `routeShardModulusProbeKeys`、`probeRouteShardModulus`(P4 通过的分支接着调);新哨兵 `ErrRouteShardModulusMismatch`;P4 / P5 共用的约束名判据 `namesRouteShardCheck`,1142 提示 `routeProbeInsertDenied`(文案改为覆盖两步) |
| `cmd/route_tool/main.go` | 建池失败的提示按原因分流(`openRouteStoreHint`):模数不一致时不再提示「先跑迁移」;退出码仍是 2;探针步骤注释补全为五步 |
| 测试 | 纯函数判别表 `TestRouteShardModulusProbeDiscriminates`;契约 `TestNewRouteStoreShardModulusProbeContract`(全接受 / 撞键 / 每一行各自 3819 / 3819 点名别的约束 / 1205 / 1142 / 非 MySQL 错误);`TestNewRouteStoreProbeFailClosed` 的三个通过用例跟上 P5 的两条语句,另补 P4 的 1142 / 1062 文案用例;真库 `TestRouteStore_ProbeRejectsTableBuiltWithAnotherShardModulus`(按 `% 16` / `% 128` 手工重建 → 新哨兵,换回 `% 64` 建池通过);`TestOpenRouteStoreHintByCause` |
| 文档 | 本段、H10、HR4 补注;runbook §9.3 前置、退出码表与 tidb-prepare 小节 |

**交 Codex 的验证**(工作目录以仓库根为起点;不写死 Go 版本;串行,前一步红了就停下归因)

1. `cd go/data_service`:`gofmt -l ./internal/store ./cmd/route_tool`(期望无输出)、`go vet ./internal/store/... ./cmd/route_tool/...`、`go test -count=1 -v ./internal/store/... ./cmd/route_tool/... -run "ShardModulus|NewRouteStore|RouteSQL|RouteBootstrapDDL|NoPlayerRouteWrites|OpenRouteStoreHint"`,再不带 `-run` 跑一次 `go test -count=1 ./internal/store/... ./internal/routetool/... ./cmd/route_tool/...`。
2. 真库(MySQL ≥ 8.0.16,环境变量同上面第 4 步):`go test -tags=integration -count=1 -v ./internal/store/... -run "Route"`,`TestRouteStore_ProbeRejectsTableBuiltWithAnotherShardModulus` 必须 PASS,其余建 route store 的真库用例(都经过 P5)不许因 P5 变红;第 1a 批验证里 routing 带 integration 的那一步按原样跑,`TestDBFirstRegister_RealStores` 必须 PASS(它经过 P5)。
3. 可选真 TiDB:同「验证增量」,只跑 `-run "TiDB"`;建池失败时原样回报报错文本(P4 失败后事务是否可用未核实)。
4. 前置纪律不变:任何可能建过路由表的库仍先 `SHOW CREATE TABLE player_route`。

**2026-10-15 评审修复**(未编译、未测试,待 Codex 验证;AGENTS.md §10.1)

| 问题 | 修法 | 位置 |
|---|---|---|
| 探针键缺席时三方以上并发建池可能 1213,P5 原样包装、不重试,这次建池直接失败(`Store.Required=false` 时该副本带着 nil 的 route store 跑到重启) | `runRouteTx` 的循环体抽成自由函数 `retryRouteTx`(计数器可传 nil),`newRouteStoreFromDB` 用它包住 P4 + P5:只认 1213 / 9007,每次重开事务,退避与写事务相同,受探针总时限约束;1205 与其它错误立即返回 | `store/route_store.go` |
| 模数不一致时只有 route_tool 改了提示,db_first 的 data_service 与 debugutil 仍无条件提示「重跑 -migrate」 | 分流收进 store 包的 `OpenRouteStoreHint`,三处共用;route_tool 私有的 `openRouteStoreHint` 删除,纯函数用例 `TestOpenRouteStoreHintByCause` 移到 store 包(名字不变) | `store/route_store.go`、`svc/servicecontext.go`、`cmd/debugutil/debugutil.go`、`cmd/route_tool/main.go` 与 `main_test.go` |
| 生产常态(63 / 64 号已在库,P5 撞 1062 后继续)与持锁时的 1205 没有真库取证 | 按 `% 16` 建的表 seed 64 号后仍被认出、seed 行不变;新真库用例 `TestRouteStore_ProbeKeysHeldByRealPlayers`(两行都在 → 建池通过、行不变;另一事务 FOR UPDATE 持 (0, 64) → 1205 原样包装、不重试,放锁后建池通过);真 TiDB 用例登记 63 / 64 号后再建一个 store | `store/route_store_integration_test.go` |
| `placement/route.go` 常量注释与容量模型状态注描述的仍是「没有 P5 时」的失败形态 | 注释改为「建池由 P5 拒绝、redis 模式认不出」;容量模型 §4.1 状态注追加 2026-10-15 补注(原句不回改) | `shared/placement/route.go`、`docs/design/capacity-model-hok-scale.md` |

另补契约用例 `TestNewRouteStoreProbeRetriesLockConflict`:首行 / 次行 1213 重跑后建池,语句序列与实参逐条钉住;连续 `routeTxAttempts` 次 1213 建池失败、关池,`errors.As` 取得出 1213;退避途中 ctx 结束即停、不再开事务。

验证增量(接在上面「交 Codex 的验证」之后,口径不变):第 1 步的 `-run` 已覆盖新用例(`NewRouteStore`、`OpenRouteStoreHint`);不带 `-run` 的那一次加上 `./internal/svc/... ./cmd/debugutil/...`,`go vet` 也加上这两处(它们改了调用)。第 2 步 `TestRouteStore_ProbeKeysHeldByRealPlayers` 必须 PASS,原样回报它日志里的耗时。第 3 步真 TiDB 上建第二个 store 失败时,原样回报报错文本。

**2026-10-15 补登:两段验证的「失败时保留」,与 FenceShards TiDB 档的配置载体**(只补文档;上面所有命令、期望与顺序都不变;AGENTS.md §10.1)

*失败时保留*。适用于「行片 16 → 64」的「交 Codex 的验证」第 1~5 步,以及本段「交 Codex 的验证」第 1~4 步与其「验证增量」。任何一步红了就停下归因,按下表原样回报;不连续重试,不改代码或用例迁就,不 `gofmt -w`。回报里不得出现 `DATA_SERVICE_IT_MYSQL_PASSWORD` 或 TiDB 测试账号的口令(AGENTS.md §9)。

| 哪一步红 | 保留什么 |
|---|---|
| `gofmt -l` 有输出 | 列出的文件清单,以及对每个文件 `gofmt -d <文件>` 的完整输出(只看差异,不改文件) |
| `go build` / `go vet` | 第一条错误的原文(`文件:行:列` 与报错文本,不改写、不摘要),并注明该文件在不在本步改动范围内(模块级命令会带上工作区别的在途改动,见「行片 16 → 64」验证第 3 步) |
| 单测 `go test` | 失败用例的完整 `go test -v` 输出:不带 `-v` 的那一次红了,对失败的包用 `go test -count=1 -v -run '^<失败用例名>$' <包>` 重跑一次,保留从 `=== RUN` 到 `--- FAIL` 与包尾 `FAIL` 行的全部输出,不截断 |
| 真库用例(`-tags=integration`) | 同上的完整 `-v` 输出;报错原文(含 MySQL 错误号、约束名,以及 `ErrRouteCheckNotEnforced` / `ErrRouteShardModulusMismatch` 等哨兵的文案);`SELECT VERSION()` 的结果;`SHOW CREATE TABLE player_route` 的完整输出。真库用例的临时库在 `t.Cleanup` 里 DROP(`storetest/mysql.go`),失败后看不到它的表,所以这份 `SHOW CREATE TABLE` 取自本机常驻库(`etc/data_service.yaml` 指向的、「前置」核对过的那个库),写明库名。`TestRouteStore_KeyedReadsUsePrimaryRange` 的 EXPLAIN 与 `TestRouteStore_ProbeKeysHeldByRealPlayers` 的耗时照原要求保留 |
| 「前置」核对 | 每个库的 `SHOW CREATE TABLE player_route` 完整输出;是 `% 16` 就停下等负责人,不自行 DROP |
| 可选真 TiDB 一步 | 报错原文(建池时 P4 / P5 的原始报错、SPLIT / ATTRIBUTES 的报错、8028 / 1213 等,不改写);原要求回报的 Region 行数、`TOTAL_SPLIT_REGION` 与建第二个 store 的结果照常保留。拉 `pingcap/*` 镜像属于下载,仍先征得用户同意 |

*FenceShards 的 TiDB 档 64 没有配置载体*。2026-10-15 按仓库 grep 核对:

- `go/data_service/etc/` 下只有 `data_service.yaml` 一份,即 MySQL 档,`RouteStore.FenceShards: 16`(:179,注释 :172-177)。仓库里没有 TiDB 档的服务 yaml,64 只写在文档里(上面「改了什么」表 `etc/data_service.yaml` 一行、「连带修正」第 1 条;容量模型 §4.1「连带修正」)。
- `tools/scripts/k8s_deploy.ps1` 生成 data-service ConfigMap 的 `RouteStore:` 段(2026-10-15 在 :4294-4297)只写固定的 `Mode: redis` 与从服务 yaml 镜像来的 `MaxOpenConn` / `MaxIdleConn`;`tools/`、`deploy/` 下没有任何 `FenceShards`。所以 K8s 上的 data-service 取代码默认 8(`placement.DefaultRouteFenceShards`;`internal/config/config.go:321-322` 在键缺省为 0 时回落),MySQL 档的 16 也没有到 K8s 上。
- 契约测试 `tools/scripts/tests/k8s_deploy_contract.tests.ps1` 没有钉这个键;`internal/config/config_test.go` 只钉了本地 yaml 的 16。
- 眼下没有运行期后果:K8s 各档位的 `Mode` 固定 redis,route store 不装配,FenceShards 用不上。切 db_first 之后,K8s 上的建角按 8 把围栏行锁分摊,低于两档的设计值(16 / 64);只影响建角吞吐,不影响正确性(围栏持有者总是锁满 0..63)。

性质:这是实现方在 2026-10-13「行片 16 → 64」那一步、在文档与 yaml 注释里登记的**延后**(上面「改了什么」表 `etc/data_service.yaml` 一行写的「切 db_first 前按档位补」、yaml :175-177),**不是负责人拍板**;负责人没有对「K8s 上先取默认 8」或「TiDB 档 64 暂不落配置」做过决定。

前置条件:切 db_first(§7 P2)之前必须做完,与 `k8s_deploy.ps1` 注释里切 db_first 时要补的 WaitAOF 两项、镜像里要有 route_tool 并列。未做完不切。

1. 按档位把 `RouteStore.FenceShards` 补进 `k8s_deploy.ps1` 的 data-service ConfigMap:MySQL 档 16,TiDB 档 64。取值来源(镜像服务 yaml,还是按后端档位单独给)由改脚本的人定,并在脚本注释里写明;
2. 在 `k8s_deploy_contract.tests.ps1` 加契约用例,钉住生成的 ConfigMap 里 `RouteStore.FenceShards` 等于该档位的值,且落在 [1, `placement.MaxRouteFenceShards`];
3. 若为 TiDB 档另起服务 yaml,`config_test.go` 同样钉住 64。

tidb-phase1-and-launch-readiness.md §5.1 单行热点一行与 §7 L-12 已加同口径的更正注。

**Java 版(AGENTS.md §12)**:服务端内部存储的启动探针,不涉及客户端契约,不需要同批改;Java 版的 D1 仍是「待做」。做等价的「库为真源」目录时若同样用片前缀 + CHECK,建议在启动时同样实测模数。

---

**第 1b-1 批(2026-10-18):route_tool 并进主程序入口 + freeze / reconcile-from-cache / unfreeze + 就绪标记的值格式与 CAS / CAD 脚本**

> 状态:**已落码,未编译、未测试**(AGENTS.md §10.1,待 Codex 按下面「交 Codex 的验证」执行)。**不改服务的运行行为**:新的库原语只被工具调用,发布路径只改了「pipeline 没执行」的分类(见运行时要点第 6 条)。唯一可见的入口变化:没有 `-route-tool` 却带位置参数时 data_service 以退出码 2 拒启(K8s 的 args 只有 `-f`,本地 `go_services.ps1` 也只传 `-f`,都不受影响)。
> 依据:调研规格 [spec_D1_batch1b_scout.md](../handoff/launch-readiness-20261013/spec_D1_batch1b_scout.md) 中标为 1b-1 的部分,吸收了其「# VERDICT」的 must / should,以及主会话 2026-10-18 定下的 10 条修法(与规格冲突处以后者为准)。没有另起 `spec_D1_batch1b.md`(核实意见 nit (d):两个事实源),结论以本段为准。

**改动清单**

| 文件 | 要点 |
|---|---|
| `go/shared/placement/route.go`、`route_test.go` | `RouteReadyKey` 注释改为 1b 的读写规则;新增 `RouteReadyFormatVersion = 1`、`RouteReadyMarker`(string JSON `{v, generation, redis_run_id, master_replid, set_at_ms, watermark_ms, leader}`)、`EncodeRouteReadyMarker` / `DecodeRouteReadyMarker`(未知字段忽略;v 大于已知值回 `ErrRouteReadyMarkerNewerVersion`,与 `ErrMalformed` 分开;手写字符校验、不引 regexp)、`RouteReadyCompareAndSetScript` / `RouteReadyCompareAndDeleteScript`。新增用例 3 个:`TestRouteReadyMarkerRoundTrip`、`TestRouteReadyMarkerRejects`、`TestRouteReadyScriptsShape`,placement 顶层用例 16 → **19** |
| `go/data_service/internal/store/route_store.go` | SQL R17(修库整行覆盖,rev 置为 maprev)、R18 / R18x(读 / 加锁读 route_meta)、R19(冻结)、R20 / R21(锁对账行、GREATEST 抬水位)、R22(解冻);哨兵 `ErrRouteNotFrozen`、`ErrRouteReconcileMissing`;常量 `RouteFreezeTimeoutMin = 5s`;`RouteMeta` / `ReadRouteMeta`、`FreezeRouteWriters`、`UnfreezeRouteWriters`、`RouteRepairOptions` / `RouteRepairStats` / `RepairRoutesFromCache` / 纯函数 `decideRepair`、`RepairMergedIntoFromCache`。R7 / R9 的执行从 `importRoutesTx` 抽成 `insertRouteRows` / `fillRouteRows`,merged_into 的循环抽成 `addMergedInto`(带开关参数),导入的语句、顺序与报错文案逐字不变 |
| `store/route_store_test.go`、`route_store_contract_test.go`、`route_store_integration_test.go`、`export_test.go` | 单测 `TestDecideRepair`、`TestRouteRepairOverwriteSQLBindsMapRevNotIncrement`、`TestRouteMetaAndReconcileWritesOnlyInStoreFiles`(AST 守卫,允许集合本批只有 `route_store.go`,命中至少 5 条);假驱动契约 `TestRepairRoutesFromCacheRefusedWhenWritable`、`TestRepairRoutesFromCacheStatementSequence`、`TestRepairRoutesFromCacheDryRunOpensNoTx`、`TestRepairMergedIntoRequiresFrozen`、`TestFreezeRouteWritersIdempotent`、`TestUnfreezeRaisesWatermarkBeforeFlippingWritable`、`TestUnfreezeAlreadyWritableIsNoop`、`TestReadRouteMetaMissingRow`;真库 `TestRouteStore_RepairFromCacheRestoresRolledBackRows`、`TestRouteStore_RepairTieReportedUnlessResolved`、`TestRouteStore_FreezeUnfreezeRoundTrip`、`TestRouteStore_UnfreezeWaitsForRepairMetaLock`;`RouteLockMetaSQLForTest` 出口 |
| `go/data_service/internal/routetool/repair.go`(新)、`freeze.go`(新)、`auditor.go` | `ReconcileFromCache` / `RepairExitCode`(三遍 SCAN 无状态去重、页内先去重、开头 / 结尾查围栏、`-freeze-grace` 等待可注入);`Freeze` / `Unfreeze`(就绪标记判定、身份比对、generation 对齐)、`ParseRedisIdentity`;audit 新增 `AuditOptions.FrozenRecovery` 与 `AuditReport.BlockingDifferences`,统计行格式不变 |
| `routetool/repair_test.go`(新)、`freeze_test.go`(新)、`routetool_integration_test.go` | `TestReconcileFromCache_*` 9 个、`TestRouteAudit_FrozenRecoveryIgnoresDBOnlyUnpublished`;`TestUnfreeze_*` 5 个、`TestFreeze_OutputAndRedisModeWarning`、`TestParseRedisIdentity`、`TestReadyMarkerScriptsOnMiniredis`(CAS / CAD 在 miniredis 上的执行语义);真库 + miniredis 的 `TestReconcileFromCache_RealDBEndToEnd` |
| `go/data_service/internal/routetoolcli/cli.go`(新)、`cli_test.go`(新) | `cmd/route_tool/main.go` 的 run / parseArgs / parseIDList / connectMappingRedis / openRouteStore / runTiDBPrepare 原样迁来,导出 `Run`;新子命令 `freeze`、`reconcile-from-cache`、`unfreeze` 与 audit `-frozen-recovery`;模式门禁 `refuseByMode`;装配依赖显式注入(`deps`);INFO 身份读取 `readRedisIdentity`。用例:迁入的 5 个 `TestRouteTool*`(**名字不变**,见下「更正」)与新增 7 个 `TestRun*` |
| `go/data_service/cmd/route_tool/main.go`、`main_test.go`(删) | 只剩薄包装 `os.Exit(routetoolcli.Run(...))`,命令行逐字兼容;`main_test.go` 的用例整体迁到 routetoolcli |
| `go/data_service/data_service.go`、`data_service_entry_test.go`(新) | flag `-route-tool`;纯函数 `selectEntry`(`-migrate` 与 `-route-tool` 互斥、`-route-tool` 必须带子命令、没有 `-route-tool` 却带位置参数拒绝,退出码 2);route-tool 模式下版本行改打 stderr、在 `conf.MustLoad` 之前交给 `routetoolcli.Run`。`selectEntry` 写在 `data_service.go` 里而不是另起文件:`tools/scripts/go_services.ps1` 用 `go run data_service.go` 只编这一个文件。`TestSelectEntry` 7 种组合 |
| `go/data_service/internal/routing/route_publish.go`、`route_publish_test.go` | 发布「pipeline 没执行」改按可重试的不可用处理(主会话第 9 条,见运行时要点第 6 条);`publishPipelineNotExecuted`;`ProbeWaitAOF` 同一判据并更正注释。新增 `TestPublishPipelineNotExecuted`、`TestDBFirstRegister_PublishRetriesWhenPipelineNotExecuted`,routing 顶层用例 54 → **56**(带 integration tag 63 → **65**) |
| 文档 | 本段;§5.4 / §6 加「以本段为准」的注;[merge-zone-runbook.md](../ops/merge-zone-runbook.md) §9.3 加注、新增 §9.4、修订历史一行;[zone_data_rollback.md](./zone_data_rollback.md) 步骤 3 加注。PROGRESS、PARITY 的条目由主会话合并 |

**运行时要点**

1. **入口**:`data_service -f <yaml> -route-tool <子命令> [参数]`(K8s 是 `/app/service -f /app/etc/data_service.yaml -route-tool …`);`go run ./cmd/route_tool -f <yaml> <子命令>` 照旧。stdout 只有一行统计,版本行、logx 日志与明细都在 stderr。K8s 上默认用一次性 Job / Pod(同镜像、同 ConfigMap,资源单独设),exec 进在役 Pod 只限 dry-run 或小库(data-service Pod 内存 limit 512Mi,import / audit 的已见集合是 O(N)),见 runbook §9.4。Job 清单本身属于 1b-3。
2. **freeze**:`UPDATE route_meta SET writable = 0 WHERE id = 1 AND writable <> 0`(R19);只碰全局库,不连 Redis(冻结是止血动作,Redis 坏了也要能冻);redis 模式下允许但打 WARN(那时 W1 不读 writable)。`-reason` 只进 stderr。
3. **reconcile-from-cache**(只许 db_first):ReadRouteMeta 可写 → 退出码 2;开头有围栏 → 3;等 `-freeze-grace`(缺省 10s,覆盖冻结之前已过开关检查的在途 W1);三遍 SCAN `player:zone:*` → `player:placement:*`(zone 非空跳过)→ `player:maprev:*`(zone 或落点非空跳过,只剩 maprev 的记 MALFORMED),内存只占一页,不保留 O(N) 的已见集合;每页先去重;merged_into 只增;结尾再查围栏。逐行判定(`decideRepair`):库无行 → 插入(rev = maprev);maprev > 库 rev → 用缓存整行覆盖、rev 置为 maprev(R17);maprev < 库 rev → 不动;同 rev 同值 → match;同 rev 异值 → 默认只报告(TIE),`-resolve-ties=cache` 才覆盖(rev 不变);无 maprev → 退回导入语义(只补 NULL 列,冲突报告);库行本身无效 → 冲突。重跑幂等。
4. **提交点复核冻结态**(主会话第 1 条):每批修库事务的第一句是 `SELECT generation, writable FROM route_meta WHERE id = 1 FOR UPDATE`(R18x),持锁到提交;解冻的 R22 要同一行的 X 锁,只能排在修库提交之后(真库用例 `TestRouteStore_UnfreezeWaitsForRepairMetaLock` 取证)。锁序:修库 route_meta → player_route;解冻 route_reconcile → route_meta;W1 与导入对 route_meta 只普通读;建池探针守卫 route_meta → player_route,与修库同向 —— 没有任何一方持 route_meta 再等 route_reconcile,不成环。
5. **unfreeze**(只许 db_first,必须 `-confirm-audited`):已可写 → 退出码 0、什么都不做;否则读就绪标记,只有「可解码、且 run_id / master_replid 等于当前 Redis(INFO server / replication)」才算可信,最低水位取标记的 `watermark_ms`;generation 与库不等时先 CAS 成库值(CAS 输了退出码 1,重跑)。**确认不了一律退出码 2**(主会话第 4 条),只有 `-cache-was-not-verified-acknowledged` 放行(最低水位 0):此前要按 runbook §9.2 清单核对 / 重放,补上两边都可能缺的合服与搬库。畸形标记按原值撤掉;版本号更新的只记日志、不删也不信任(主会话第 7 条);不是 string 类型的不动(读方 MGET 视同缺席);身份不符的在放行时按原值撤掉(读方只看「存在」)。**第 1b-2 批之前没有任何写者挂标记,所以本批的 unfreeze 一律要走显式确认。** 解冻事务 R20 → R18 → R21(要求的水位大于当前值时)→ R22,generation 不换(偏离 B1)。MappingRedis.Type 不是 node 时拒绝(身份按单实例定义)。连 mapping Redis 用 go-zero `redis.NewRedis`,RedisConf 的 User(ACL 用户名)/ Pass / Tls 原样透传。
6. **发布「pipeline 没执行」的分类**(主会话第 9 条,核实结论:疑点成立)。按模块缓存核对 go-zero v1.10.0 `core/stores/redis/breakerhook.go`(熔断打开时 `ProcessPipelineHook` 直接返回 `breaker.ErrServiceUnavailable`,不调下一层;`core/breaker/breaker.go` 的 `DoWithAcceptableCtx` 在 ctx 已结束时先回 `ctx.Err()`)、`redis.go` 的 `PipelinedCtx`(回调照样被调用,`conn.Pipelined` 的错误原样返回)、go-redis v9.18.0 `pipeline.go` 的 `Exec`(钩子不往下走时不回填任何命令的错误;`setCmdsErr` 只在 `generalProcessPipeline` 里):熔断打开时 `evalCmd` 不为 nil、`Err()` 为 nil、`Val()` 为 nil,旧代码会落到 `ClassifyPublishReply(nil)` → `ErrUnexpectedPublishReply`,当成确定性错误、不再重试。现在 `evalCmd == nil` 或 `publishPipelineNotExecuted`(判据同 scene_manager `load_refresh.go` 的 `pipelineBatchRejected`)都按可重试的不可用处理,计 `route_publish_total{result="error"}` 不变。顺带核实出 `ProbeWaitAOF` 注释里「熔断器与 Router 的互不相干」不成立:go-zero 按地址缓存底层客户端(`redisclientmanager.go` `getClient`),熔断钩子在第一次建客户端时挂上,同地址的 *redis.Redis 共用一个熔断器;注释已加更正注,行为不改,风险见下。
7. **audit -frozen-recovery**(主会话第 6 条):冻结期反向修库只有「缓存 → 库」一个方向,库独有的行一定计成 `db_only_unpublished`;这个口径下它照常计数、打明细,但不计入退出码,其余类别必须为 0。只许 db_first 且已冻结时用(入口先查 ReadRouteMeta,可写就退出码 2)。stdout 统计行格式不变,口径写在 stderr 的 NOTE 行。1b-1 里这些行解冻之后**仍留在库独有状态**,要等 1b-2 leader 的增量推送;在那之前默认口径的 audit 会一直报它们。
8. **超时**(主会话第 5 条):含冻结写的调用(R19 / R22)时限至少 `max(5s, tidb_table_cache_lease + 2s)`;CLI 参数 `-freeze-timeout`(缺省 5s,低于 5s 直接拒绝)。`tidb_table_cache_lease` 缺省 3s【未核实】。`ALTER TABLE route_meta CACHE` **不并进 tidb-prepare**(回应本段之上「TiDB 运维步骤」第 6 条留给 1b 的问题),留给 TiDB Phase 1 的部署步骤;届时缓存表的租约若调大,`-freeze-timeout` 同步放大。

> 2026-10-18 更正(同日修复评审,原文不删):第 3 条「maprev > 库 rev → 用缓存整行覆盖」多了一个例外(库里某列有值而缓存同列缺席 → 冲突);第 5 条「只许 db_first」「MappingRedis.Type 不是 node 时拒绝」改为:redis 模式下也允许 unfreeze(要显式 `-cache-was-not-verified-acknowledged`,不读标记、不连 mapping Redis),node 的要求只对 db_first 成立;就绪标记「版本号更新」的判定改为先只解 v。见下「修复评审」。

**相对规格(spec_D1_batch1b_scout.md 的 1b-1)的偏离**

| # | 偏离 | 理由 | 代价 |
|---|---|---|---|
| 1b1-1 | 修库事务用 R18x(`SELECT generation, writable … FOR UPDATE`)在提交点复核,不是规格的普通读 `requireRouteFrozen` | 主会话第 1 条 / 核实意见 should:普通读挡不住「修库进行中有人解冻」 | 每批多持一把 route_meta 行锁;见风险 1 |
| 1b1-2 | R18x 多取一列 generation,文本与建池守卫(`SELECT writable … FOR UPDATE`)不同 | 契约用例按 SQL 文本区分语句,同文本会让两类语句的应答混在一起;generation 进错误信息 | 无 |
| 1b1-3 | R19 加 `AND writable <> 0`,受影响 0 行再用 R18 区分已冻结 / 缺行 | 不依赖引擎对「值没变」的受影响行数口径 | 无 |
| 1b1-4 | 解冻在事务内先锁 route_reconcile(R20)再普通读 meta,已可写时整笔回滚(连水位也不抬) | 「已可写」的判定与写同一事务,并发的两次解冻在 R20 上串行 | 无 |
| 1b1-5 | unfreeze 读不到可信标记一律退出码 2,要显式 `-cache-was-not-verified-acknowledged`(规格原为「畸形就撤掉、当作不存在、照常解冻」) | 主会话第 4 条 / 核实意见 should:冻结期 Redis 身份变化后缓存可能不是超集,audit 看不见两边都缺的行 | 1b-2 之前每次解冻都要显式确认 |
| 1b1-6 | 标记 v 大于已知值:只记日志,不删也不信任(规格原为当畸形撤掉) | 主会话第 7 条 / 核实意见 nit (c):滚动升级到 v2 时新旧写者来回打架 | 无 |
| 1b1-7 | audit `-frozen-recovery`(规格无) | 主会话第 6 条 / 核实意见 should:冻结期 audit 到不了 0,门禁形同虚设 | 多一个口径;只在 db_first 且冻结时可用 |
| 1b1-8 | `RouteRepairStats.TiesResolved` 是清单不是计数 | 被覆盖的平局要逐个留痕(stderr `TIE_RESOLVED` 明细) | 无 |
| 1b1-9 | AST 守卫允许集合本批只有 `route_store.go`(规格写的是加上 `route_reconcile.go`),命中数要求 ≥ 5 而不是 > 0 | 不登记一个还不存在的文件;5 条是 R15 / R16 / R19 / R21 / R22,正则失效时更早红 | 1b-2 新增文件时要改允许集合 |
| 1b1-10 | 迁移的用例保留 `TestRouteTool*` 原名(规格写改成 `TestRun*`) | 主会话第 2 条 / 核实意见 should:前几批验证命令按 `-run "Route|…"` 选用例,改名会静默丢覆盖 | 见下「更正」 |
| 1b1-11 | 修库按页先去重再提交 | 主会话第 3 条 / 核实意见 nit (e):同一页 SCAN 回重复键会撞 `validateRouteImportRows` 的重复检查、整批失败 | 跨页重复仍会修两次(第二次 Matched),计数略多 |
| 1b1-12 | reconcile-from-cache 的 dry-run 同样要求冻结并等宽限 | 可写时的 dry-run 结论没有意义(W1 在写);与真跑同一前提 | dry-run 之前必须先 freeze |
| 1b1-13 | `freeze` 不连 mapping Redis;unfreeze 要求 MappingRedis.Type=node | 冻结是止血动作;身份按单实例定义 | 无 |
| 1b1-14 | 不另起 `spec_D1_batch1b.md` | 核实意见 nit (d):避免两个事实源 | 无 |
| 1b1-15 | 顺带修了发布的「pipeline 没执行」分类,并更正 `ProbeWaitAOF` 注释 | 主会话第 9 条,核实成立(见运行时要点第 6 条) | routing 顶层用例数 54 → 56 |

**风险与未核实项**

1. 修库每批持 route_meta(id=1) 的 X 锁到提交:这期间新起的 data_service 副本(db_first)建 route store 时探针守卫最多等 1s,等满即 1205、这次建池失败(`Store.Required=true` 时拒启)。修库期间不要滚动重启 data_service;每批事务通常毫秒级到百毫秒级(≤ 1000 行),撞上的概率低,但没有压测。
2. TiDB 缓存表上的写要等读租约、`FOR UPDATE` 加锁读在缓存表上的行为都【未核实】;本批 tidb-prepare 不设缓存表,所以眼下不触发。
3. 熔断器共用(运行时要点第 6 条):`ProbeWaitAOF` 与 Router、route_tool 的 mapping Redis 客户端同地址时共用第一次建客户端那个 *redis.Redis 的熔断器;探针失败会计进 Router 的熔断。行为本批不改,彻底隔离仍按第 1a 批「仍未解决的风险」第 9 条的备选(独立 go-redis 客户端),留给 1b-2 的 leader 一并处理。
4. 无状态去重的前提是冻结期间没有人写 mapping Redis(W1 在发布前就被 writable=0 拦下,合服被冻结窗口禁止);有人违反时 B 遍可能漏修一名玩家,只有随后的 audit 能发现。
5. 1b-1 的解冻之后,库独有的行(孤儿行)仍不在缓存里,要等 1b-2 的增量推送;默认口径的 audit 会继续报 `db_only_unpublished`。
6. 真库用例 `TestRouteStore_UnfreezeWaitsForRepairMetaLock` 依赖会话锁等待 1s(buildRouteDSN 钉死);耗时约 1s。

**更正(2026-10-18,主会话第 2 条):前几批验证命令里 `./cmd/route_tool/...` 的用例已迁到 `./internal/routetoolcli/...`**。原文的命令不改;照原文跑时 `./cmd/route_tool/...` 现在只有 `main.go`、没有测试文件(`no test files`,仍能编译),五个用例要在新位置跑。名字一个都没改:

| 原位置(`go/data_service/cmd/route_tool/main_test.go`) | 新位置(`go/data_service/internal/routetoolcli/cli_test.go`) |
|---|---|
| `TestRouteToolBadArgsExitTwo` | `TestRouteToolBadArgsExitTwo`(用例表不变) |
| `TestRouteToolMissingConfigExitsTwo` | `TestRouteToolMissingConfigExitsTwo` |
| `TestRouteToolUnreachableRedisExitsTwoWithoutPanic` | `TestRouteToolUnreachableRedisExitsTwoWithoutPanic` |
| `TestRouteToolRealImportUnderDBFirstNeedsExplicitFlag` | `TestRouteToolRealImportUnderDBFirstNeedsExplicitFlag` |
| `TestRouteToolTiDBPrepareDoesNotTouchRedis` | `TestRouteToolTiDBPrepareDoesNotTouchRedis` |

涉及的旧命令(第 1a 批第 2 步、G1a「验证增量」第 2 步、「行片 16 → 64」第 3 步、「正向探针 P5」第 1 步):凡是出现 `./cmd/route_tool/...` 的地方改跑 `./internal/routetoolcli/... ./cmd/route_tool/...`,`-run` 正则不变(`Route` 仍能选中这五个)。第 1a 批第 2 步「routing 顶层用例 54 个」与第 4 步「带 integration tag 共 63 个」从本批起分别是 56 与 65(新增两个发布用例)。G1a 的 route_tool 冒烟 `go build ./cmd/route_tool` 照旧可用。

**交 Codex 的验证**(工作目录以仓库根为起点,PowerShell 串行;前一步红了就停下归因,不连续重试、不 `gofmt -w`、不改用例迁就;Go 设 `GOTOOLCHAIN=local`、`GOPROXY=off`,不改 go.mod / go.sum)

前置:先按复验清单、第 0 / 1a / G1a / P5 各段把已落码部分跑完(其中 `./cmd/route_tool/...` 按上面「更正」换位置),再跑本段。

0. **修复前红(基线 `360d56ae5`,临时 worktree,跑完删除)**:
   ```powershell
   git worktree add "$env:TEMP\ds-1b1-base" 360d56ae5
   # 入口:基线把位置参数静默忽略,-version 照常退出 0;新代码退出 2
   cd "$env:TEMP\ds-1b1-base\go\data_service"; go build -o "$env:TEMP\ds_base.exe" .
   & "$env:TEMP\ds_base.exe" -version audit; "exit=$LASTEXITCODE"          # 期望 exit=0(红:位置参数被吞)
   # 发布分类:把本仓 internal/routing/route_publish_test.go 拷进基线 worktree,删掉其中 TestPublishPipelineNotExecuted 整个函数(基线没有被测函数)
   go test -count=1 -v -run "TestDBFirstRegister_PublishRetriesWhenPipelineNotExecuted" ./internal/routing/   # 期望 FAIL,错误含 unexpected route publish reply
   cd <仓库根>; git worktree remove --force "$env:TEMP\ds-1b1-base"; Remove-Item "$env:TEMP\ds_base.exe"
   ```
   其余新用例在基线上是「符号不存在、编译失败」,不单独取证。
   > 2026-10-18 更正(同日修复评审):第一版测试钩子不分命令拦下所有 pipeline,连新连接初始化时的 CLIENT SETINFO 也挡,`MustNewRedis` 的 PING 随之失败、`logx.Must` 直接 `os.Exit(1)`,整个 routing 测试进程退出 —— 那不是有效的红。钩子已改成只拦发布脚本那条 EVAL 的批(见下「修复评审」第 1 条),本步要**重做**:有效的红必须是 `--- FAIL: TestDBFirstRegister_PublishRetriesWhenPipelineNotExecuted`,且子用例「第一次被挡_重试成功」的错误含 `unexpected route publish reply`;只看到 `exit status 1` / `FAIL data_service/internal/routing` 而没有 `--- FAIL` 行的,判为无效、停下归因,不要当成红。
1. `cd go/shared`:`gofmt -l ./placement`(无输出);`go vet ./placement/...`;`go test -count=1 -v ./placement/... -run "RouteReady|Route|Rev|Publish"`(全部 PASS);`go test ./placement/... -list . | Measure-Object -Line`(期望 19 行用例 + 1 行 ok)。
2. `cd go/data_service`:
   ```powershell
   gofmt -l ./internal/store ./internal/routetool ./internal/routetoolcli ./internal/routing ./cmd/route_tool ./data_service.go ./data_service_entry_test.go   # 无输出
   go build ./...
   go vet ./internal/store/... ./internal/routetool/... ./internal/routetoolcli/... ./internal/routing/... ./cmd/route_tool/... .
   go test -count=1 -v ./internal/store/... -run "DecideRepair|Repair|Freeze|Unfreeze|RouteMeta|NoPlayerRouteWrites|WritesOnlyInStoreFiles|RouteSQL|RouteODKU|ImportRoutes|ImportMergedInto"
   go test -count=1 -v ./internal/routetool/... -run "ReconcileFromCache|Unfreeze|Freeze|ParseRedisIdentity|ReadyMarkerScripts|FrozenRecovery|RouteAudit|RouteImport"
   go test -count=1 -v ./internal/routetoolcli/... .
   go test -count=1 -v ./internal/routing/... -run "PipelineNotExecuted|DBFirstRegister|CheckWaitAOF|WaitAOFBudget"
   go test ./internal/routing/... -list . | Measure-Object -Line      # 期望 56 行用例 + 1 行 ok
   go test -count=1 ./internal/store/... ./internal/routetool/... ./internal/routetoolcli/... ./internal/routing/... ./internal/svc/... ./cmd/route_tool/... .
   go test -count=1 ./...
   ```
   通过标准:全绿;模块级 build / vet / test 带进的他人在途改动按文件归因、单独列出。`TestRouteToolUnreachableRedisExitsTwoWithoutPanic` 等连刚关掉端口的用例要几秒,属正常。
3. **MySQL 真库**(`$env:DATA_SERVICE_IT_MYSQL_HOST='127.0.0.1:3306'`、USER=root、PASSWORD 取 compose,不进日志;MySQL ≥ 8.0.16,先记 `SELECT VERSION()`):
   ```powershell
   go test -tags=integration -count=1 -v ./internal/store/... -run "RouteStore_Repair|RouteStore_FreezeUnfreeze|RouteStore_UnfreezeWaits|RouteStore_ImportClassifies|RouteStore_MetaMissing"
   go test -tags=integration -count=1 -v ./internal/routetool/... -run "RealDB|RouteToolImportThenAudit"
   ```
   通过标准:没有 FAIL;出现 SKIP 如实报「未验证」。`TestRouteStore_UnfreezeWaitsForRepairMetaLock` 应在约 1s 后以 1205 证明解冻排在修库锁之后,原样回报它的耗时。已知红项 `TestMigrateSchema_UpgradesLegacyHandWrittenTables` 不在这个正则里。
4. **Redis 回归**(发布路径改了分类):一次性 `redis:7.2 --appendonly yes`(本地没有镜像要拉取时先征得用户同意),`$env:DATA_SERVICE_IT_REDIS_HOST='127.0.0.1:6390'`,`go test -tags=integration ./internal/routing/... -count=1 -v`;`TestPublishRoute_RealRedisSameConnection`、`TestPublishRoute_RealRedisWaitAOF` 必须 PASS、不能 Skip;顶层用例 65 个。跑完 `docker rm -f ds-it-redis`。
5. **入口冒烟**(本机;不要用 `go run` 断言退出码):
   ```powershell
   cd go/data_service; go build -o "$env:TEMP\ds1b1.exe" .
   & "$env:TEMP\ds1b1.exe" -f etc/data_service.yaml audit; "exit=$LASTEXITCODE"                          # 期望 exit=2,stderr 含 unexpected positional arguments,没有起服务
   & "$env:TEMP\ds1b1.exe" -version audit; "exit=$LASTEXITCODE"                                           # 期望 exit=2(与第 0 步的基线对照)
   & "$env:TEMP\ds1b1.exe" -f etc/data_service.yaml -migrate -route-tool audit; "exit=$LASTEXITCODE"      # 期望 exit=2
   & "$env:TEMP\ds1b1.exe" -f etc/data_service.yaml -route-tool; "exit=$LASTEXITCODE"                     # 期望 exit=2
   & "$env:TEMP\ds1b1.exe" -f etc/data_service.yaml -route-tool reconcile-from-cache; "exit=$LASTEXITCODE" # 本地 yaml 是 redis 模式:期望 exit=2,stderr 含 RouteStore.Mode=redis
   & "$env:TEMP\ds1b1.exe" -f etc/data_service.yaml -route-tool unfreeze; "exit=$LASTEXITCODE"            # 期望 exit=2,stderr 含 -confirm-audited
   & "$env:TEMP\ds1b1.exe" -f etc/data_service.yaml -route-tool audit 1>"$env:TEMP\ds1b1_out.txt"; "exit=$LASTEXITCODE"
   Get-Content "$env:TEMP\ds1b1_out.txt"   # 本机 MySQL / Redis 在且跑过迁移时:恰好一行 route_audit …(exit 0 或 1 取决于数据),版本行在 stderr;依赖不在时 exit=2、文件为空,如实报「未验证」
   Remove-Item "$env:TEMP\ds1b1.exe", "$env:TEMP\ds1b1_out.txt"
   ```
6. 不需要重编 C++(没有 proto 变更),不跑压测。

失败时保留:同上面「2026-10-15 补登」那张表(gofmt -d 输出、第一条编译错误原文并注明是否在本段改动范围内、失败用例完整 -v 输出、MySQL 错误号、`SELECT VERSION()`、`SHOW CREATE TABLE player_route`);口令不进回报。

**修复评审(2026-10-18,同日;同样未编译、未测试,待 Codex 验证)**

1. **发布用例的测试钩子(must)**。`route_publish_test.go` 第一版的 `rejectPipelinesHook` 不分命令拦下前 n+1 个 pipeline。已在模块缓存逐行核实:go-zero v1.10.0 `getClient` 把 `r.hooks` 挂在 breakerHook 之后;go-redis v9.18.0 每条新连接在 `initConn` 里经同一条钩子链用 pipeline 发两条 CLIENT SETINFO(`DisableIdentity` 缺省 false);结构体字面量的 `RedisConf` 是 `NonBlock=false`,`NewRedis` 先 PING;被挡下的 SETINFO 让 PING 失败,`MustNewRedis` → `logx.Must` → `os.Exit(1)`。整个 routing 测试进程退出,两个子用例都跑不到,第 0 步拿到的「红」无效。改为 `publishBreakerHook`:只拦含 `PublishRouteScript` 那条 EVAL 的批(`Name()=="eval"` 且 `Args()[1]` 等于脚本,同 login `createplayer_seed_test.go` 的 `probePipelineHook` 按批内命令认人),其余一律放行;自检改为发一条 `ECHO <哨兵>` 的 pipeline、只置「已挂上」标记,不消耗名额;断言改为「被挡下的发布批 / 放行的发布批」两个计数(子用例 1 恰好 1 / 1,子用例 2 恰好 3 / 0),不再断言总计数。go-zero 熔断器在 3 次失败下不会自己打开(`protection = 5`),计数不受真熔断干扰。被测代码不变。
2. **redis 模式下撤销 freeze(should,两条评审意见合并修一次)**。原先 redis 模式下 freeze 放行、unfreeze 退出码 2,而 `requireRouteWritable` 与模式无关:误跑 / 演练一次 freeze(或从 db_first 回退时带着冻结),P1 / P3 的 `import-from-redis` 就一直报 `ErrRouteNotWritable`,工具里没有解冻路径,运维只能手写 `UPDATE route_meta`(绕过「先抬水位再解冻」与 store 的写入守卫)。取方案 A(保留 freeze 的止血语义):redis 模式下允许 `unfreeze -confirm-audited -cache-was-not-verified-acknowledged`,不连 mapping Redis、不读就绪标记(规格「redis 模式不碰标记」)、最低水位 0,照样走 `store.UnfreezeRouteWriters`,stderr 打 WARN,统计行 `marker=not_read_redis_mode`;没加确认开关在连任何东西之前退出码 2(`refuseByMode`),`routetool.Unfreeze` 的 `RedisMode` 再兜一次;MappingRedis 必须是 node 的要求只对 db_first 成立。新增 `TestRunFreezeThenUnfreezeInRedisMode`(routetoolcli)、`TestUnfreeze_RedisModeNeedsAckAndSkipsMarker`(routetool);`TestRunReconcileAndUnfreezeRefusedInRedisMode` 用例表不变(没加确认的 unfreeze 仍在拒绝之列),只改注释。usage、包注释、runbook §9.4 表后加更正注。
3. **就绪标记的版本判定(nit)**。`DecodeRouteReadyMarker` 原先一步整体解,v2 若改了已知字段的类型(如 `set_at_ms` 改成字符串),json 类型错先于 v 的判断,判成 `ErrMalformed`,unfreeze 随即按原值 compare-and-delete,删掉新版本写者的标记(与主会话第 7 条相反)。改为先只解 v,v 大于已知值直接 `ErrRouteReadyMarkerNewerVersion`,v=1 再整体解与校验。`TestRouteReadyMarkerRejects` 加子用例 `{"v":2,"set_at_ms":"x"}` → 新版本、`{"v":1,"set_at_ms":"x"}` → 畸形;`TestUnfreeze_RefusesWithoutVerifiedMarker` 加一行「版本更新且字段改了类型_不动」。placement 顶层用例数仍是 19。
4. **反向修库不清掉库里有值的列(nit)**。`decideRepair` 在 maprev > 库 rev 时原先直接用缓存整行覆盖:缓存缺 `player:zone` 或 `player:placement` 而库里有值时,R17 会把这一列写成 NULL,之后两边都没有这一列,audit 判 match、静默放过。db_first 下这两列只会从 NULL 变成有值,「rev 更高却少一列」只可能是脚本之外的 DEL,所以改判 conflict(不覆盖、`CONFLICT` 明细交人看)。`TestDecideRepair` 原用例「缓存没有落点_库的落点也清掉」改判 conflict(带日期注),另加「缓存没有 home 而库有」与「库缺的列由缓存补上_照常覆盖」两格。契约 / 真库用例的输入不触及这一分支,不变。将来启用删号墓碑(W3)时要同步调整这条判据。
5. **审计口径的已知缺口(nit,只改文档)**。`-frozen-recovery` 只豁免 `db_only_unpublished`;库里原本只有落点的行,W1 补上 home、rev+1 之后发布失败,缓存仍只有 `player:placement`,audit 计成 `missing_in_redis`(阻断类),冻结期内修不掉。按评审建议 (b) 不放宽门禁,在 runbook §9.4 第 3 步写明这类行的来历、核对形状与处置。
6. **db_first 下 zone 回档的缓存清理(should,只改文档)**。`zone_data_rollback.md` 步骤 3 与 runbook §9.4 第 0 步只说「不要整库 FLUSHDB」,没说步骤 3 本来的目的(清本 zone 回档点之后的玩家缓存)怎么完成,照做就是只回了一半。两处追加更正注:db_first 下仍必须选择性清本 zone 的玩家缓存、保留全部全局键;能安全执行它的载体登记为第 1b-3 批的前置,**在它落地并验证之前,db_first 下 K8s 的 zone 回档不可执行**。注:工作区里 `tools/scripts/k8s_zone_rollback.ps1` 正由另一项改动重写(不再 FLUSHDB,改由尚未落地的 `zone_rollback_discard` 按 PITR 之后的库处置受影响玩家的缓存),1b-3 动手前先以它的最终形态为准对齐。
7. **文档归属(should / nit)**:「TiDB 运维步骤」第 6 条加回指注;[tidb-phase1-and-launch-readiness.md](./tidb-phase1-and-launch-readiness.md) §9.3 第 21 项与 §6.2「恢复后必做」⑤ 各加同日更正(CACHE 由 Phase 1 部署步骤执行、`-freeze-timeout` ≥ 租约 + 2s、1b-2 之前没有自动回档检测);§8.3 加注;调研规格文首加「以本段为准、本文件只作交接快照」;`repair.go` 注释里编译期断言的文件名改正(在 `repair_test.go`)。

**相对规格的偏离(修复评审补登,接上表编号)**

| # | 偏离 | 理由 | 代价 |
|---|---|---|---|
| 1b1-16 | 规格要求 stderr 明细有 `TIE` / `CONFLICT` / `MALFORMED` / `DB_INVALID` 四类;实现里库行本身无效时打成 `CONFLICT`(`db_placement` 字段带原因),没有单独的 `DB_INVALID` | `decideRepair` 对库行无效与同列异值给同一结论(都不写、交人看),明细里库一侧的值足以区分 | 按明细类别 grep 时要看 `db_placement` 的内容 |
| 1b1-17 | redis 模式下允许 unfreeze(要显式确认,不读标记),规格原为退出码 2 | 见上「修复评审」第 2 条:否则 redis 模式下的冻结没有工具撤销路径 | 确认开关在 redis 模式下的含义是「确认所有副本都在 redis 模式」,靠运维判断 |
| 1b1-18 | 反向修库在 maprev > 库 rev 但缓存少一列时判冲突,规格原为整行覆盖 | 见上「修复评审」第 4 条 | 带外 DEL 造成的这类行要人工处理 |

**修复评审的验证增量**(插进上面「交 Codex 的验证」,命令不变的步骤照旧):

- 第 0 步:按该步的同日更正**重做**;期望是用例级 FAIL(含 `unexpected route publish reply`),进程退出不算红。修复评审第 2–4 条修的是本批新增的代码(`RouteReadyMarker`、`decideRepair`、`refuseByMode` / `Unfreeze` 在基线 `360d56ae5` 里都不存在),基线上只会是编译失败,不单独取证;它们的回归由新增 / 改判的用例在修复后的代码上钉住。
- 第 2 步 `-run` 正则不变即可选中新用例:store 的 `DecideRepair`、routetool 的 `Unfreeze`(含 `TestUnfreeze_RedisModeNeedsAckAndSkipsMarker`)、routetoolcli 全量(含 `TestRunFreezeThenUnfreezeInRedisMode`)。routing 顶层用例数仍是 56(带 integration 65);routetool 新增 1 个顶层用例,routetoolcli 新增 1 个(`TestRun*` 7 → 8)。
- 第 2 步额外跑一遍 routing 发布用例并核对没有进程级退出:`go test -count=1 -v -run "TestDBFirstRegister_PublishRetriesWhenPipelineNotExecuted" ./internal/routing/`,期望两个子用例都 PASS;输出里出现 `exit status 1` 而没有 `--- FAIL` 行即为钩子问题复发,保留完整输出。
- 第 5 步加一条:`& "$env:TEMP\ds1b1.exe" -f etc/data_service.yaml -route-tool unfreeze -confirm-audited; "exit=$LASTEXITCODE"` —— 本地 yaml 是 redis 模式,期望 exit=2,stderr 含 `RouteStore.Mode=redis` 与 `-cache-was-not-verified-acknowledged`,且不出现 `unreachable`(不连 mapping Redis)。**不要**在本机加 `-cache-was-not-verified-acknowledged` 冒烟:那会真的改本机全局库的 `route_meta`。

**已知未改(属于 1b-2 / 1b-3)**:对账 leader、就绪标记的写者、rewarm、audit `-require-maprev`、config 的 Reconcile 段、metrics;`k8s_deploy.ps1` 的 ConfigMap(FenceShards / Reconcile / WaitAOF)与契约测试、route-tool 的 K8s Job 清单、告警;两份清库脚本在有在任 leader 时拒绝执行的防护;`k8s_zone_rollback.ps1` 加入 freeze / reconcile-from-cache / audit / unfreeze 四步。import / audit 仍在内存里保留 O(N) 的已见集合(规格风险条目,未改)。(2026-10-18 修复评审补:1b-3 再加一项 —— db_first 下 zone 回档步骤 3 的选择性缓存清理载体,只清本 zone 玩家缓存、保留全局键,在它落地并验证之前 db_first 下 K8s 的 zone 回档不可执行,见上「修复评审」第 6 条。)

**Java 版(AGENTS.md §12)**:**不适用**。映射存储是服务端内部实现,不涉及客户端契约(帧、客户端可见 proto、tip 码、配表都不变);Java 版的归属区本来就是库内列 `game.player.zone_id`(`PlayerHomeZones` 按主键读库),没有 Redis 完整副本,冻结、反向修库、就绪标记都没有作用对象。按 Q14 在 Java 仓 `PARITY.md` 登记一行,由主会话合并。

---

## 12. 全局目录本身是不是瓶颈(2026-10-12 补,回应负责人追问)

「玩家属于哪个逻辑服、数据在哪个库」这份目录全服只有一份,本质上是全局单点。要分清它和 Phase 2 全局玩家库是两类负载。数字都是推算,没有压测:

| | 玩家主数据全局库(Phase 2) | 归属 / 落点目录(本方案) |
|---|---|---|
| 每玩家数据量 | 整份存档,KB~MB 级 | 一行约 100 字节(`player_route`) |
| 写频率 | 每次存盘:在线人数 / 300 每秒,再加事件存盘 | **一生一次**(建角),加上极少的合服 / 搬库运维 |
| 读路径 | 冷读 / 预加载,走 go/db | 热路径全部读 **Redis 副本**:go/db 每条存盘一次 MGET(2026-10-18 更正:每条读写任务 2 次、一次存盘 4 次,另有 scene_manager 每次换图也读,见 §12.1)、scene_manager 每次进场、login 每次登录。**不读库**(go/db 设计上不连全局库,§5.2) |

**写:只在建角时发生。**
- 每次建角在全局库做一个短事务:读 `route_meta`,`FOR UPDATE` 一个围栏分片行,插入一行,提交。
- 发布到 Redis 和 WAITAOF 都在**提交之后**,不持锁。所以 WAITAOF 只拉长单次建角的延迟,不降吞吐。(2026-10-18 更正:db_first 下不成立,见 §12.1 更正第 2 条)
- 吞吐上限约为「每 zone 分片数 N ÷ 事务时长」。
  - MySQL 上事务几毫秒:N=8 时约每 zone 千级建角/秒(推算)。
  - TiDB 悲观事务更慢。G1a(§11.2「第 1a 批热点修复」)之后:`player_route` 主键与三条二级索引各 64 条写入尾部(`idx_route_home` 每 zone 64 条;2026-10-13 由 16 改为 64),`route_tool tidb-prepare` 预切后落在不同 Region(按 zone / 落点值的两条索引每 8 片一个 Region);围栏每 zone 一个 Region;`route_meta` 只剩极少写的开关行,S2 起必须设成缓存表(S1 可选)。剩下的上限是「每 zone 围栏分片行锁」与 TiDB 悲观事务时长。
- 它和今天已经在建角同步路径上的「名字登记」(同一个全局库、每次建角一条写)是同一类负载。所以本方案没有引入新的瓶颈种类,只是把这一类加倍。

**读:真正的压力在 mapping Redis,不在库。**
- 这个 Redis 现在和会话、锁、`PlayerAllData`、存盘游标共用一个单主实例。开服首日它是比库更早的单点(tidb-phase1-and-launch-readiness.md §7)。
- 缓解:mapping 独立成实例,保持 noeviction;压测里单独看它的 ops 与 CPU。
- 不建议在 go/db 进程内再缓存一层映射:搬库冻结要求「按当前记录选库」(player-storage-placement.md P-3),本地缓存会读到旧落点。

**能不能不要这份全局目录?**
- 只要 player_id 全局唯一、不编码 zone,而且合服只改归属、不重新发号(AGENTS §7、合服设计 §2),就必须有一处能回答「这个 id 属于哪个区、数据在哪个库」。
- 业界通常的做法也是这样一个「低写入、可缓存读」的全局目录。账号、名字唯一性、号段本来就是同类的全局薄层。
- 全球多地域时,目录留在单一写入地域,远端玩家建角多付 1~2 个跨洋往返。这是一次性的,见 Phase 1 文档 §8。

**万一目录还是成了瓶颈(2026-10-12 G1a 补)**

先看信号,再按代价从低到高往上爬。下面的数字都是推算,以 [tidb-phase1-and-launch-readiness.md](./tidb-phase1-and-launch-readiness.md) §7 的 L-12 / L-14 压测为准。

- **信号**(任一持续抬头就该看):
  - `login_create_player_stage_seconds{stage="register"}` 的 p99(建角注册段,`stress_summarize.ps1` 的 CreatePlayer stages 表);
  - `data_service_route_register_total{outcome="store_error"}` 占 db_first W1 总数的比例;
  - data_service 日志里 `[RouteStore] … retry … after lock conflict`(1213 / 9007 重来)与 1205 的频率;
  - TiDB 档:PD / Dashboard 的热点视图里 `player_route`、`zone_merge_fence` 所在 Region 的写热度与 leader 分布。
- **梯子**(代价从低到高):
  1. 调大 `RouteStore.FenceShards`(只改配置;上限 64 已是围栏持有者的契约,`placement.MaxRouteFenceShards`):同一 zone 的建角分摊到更多围栏行锁上。
  2. TiDB:`ALTER TABLE route_meta CACHE`(S2 起是必选项,不再是梯子上的一级;S1 档在开关行读热时打开。冻结写会多等一个读租约,见 §11.2 G1a 段)。
  3. TiDB:围栏改按 `(zone, shard)` 预切 —— 只改 `route_tidb.go` 的语句生成,表已聚簇,不动 DDL(G1a HR6)。
  4. TiDB:加 TiKV / tidb-server。热点已打散,扩容才近似线性;打散之前扩容没用。
  5. W1 的围栏复核改为不持锁的版本比对(提交点只比 `fence_epoch` 而不 `FOR UPDATE` 围栏行),去掉每次建角都要的行锁。改变提交点语义,需要单独设计与评审,不在任何已排批次里。
  6. 全球多地域:目录留在单一写入地域、各地域放只读薄层(Phase 1 文档 §8),远端玩家建角多付跨地域往返,是一次性的。
- **MySQL 档**:目录与 zone 库目前在同一个 MySQL 实例上(§6 引的 `k8s_deploy.ps1:3443`、`:3511-3512`),这时目录的瓶颈其实是实例本身(连接预算、IO),先把全局库拆到独立实例;G1a 在 MySQL 上不改变容量。
- **读路径始终是 Redis 副本,不读库**(上面「读」一节):目录库成为瓶颈只会发生在建角写入上,不会拖慢登录、进场、存盘。(2026-10-18 更正:db_first 下不成立,见 §12.1 更正第 2 条)

**开服前要做的(都已在别处登记):**
1. 压测建角:P2 切 db_first 的门禁本来就要求,出注册阶段 p99 与 1213 / 1205 计数,据此定 N。
2. TiDB 上跑 `route_tool tidb-prepare -zones=<全部>`(G1a;预切 `player_route` 与 `zone_merge_fence`),并把路由表热点纳入压测。
3. mapping Redis 独立成实例。
4. 号段开服档 Step=1000。

### 12.1 2026-10-18 复核：登录到底读了什么、按吞吐看先撞哪面墙（回应负责人「登录拿逻辑服和物理服是不是瓶颈、能否水平扩展，吞吐要高」）

> 怎么来的：多代理只读核对代码与文档（login、data_service、进场 / 存盘消费者、Redis 部署、文档与压测证据、Java 版各一路，每路一名质疑者复核），再经两轮评审订正（2026-10-19）；没有编译、测试或压测。代码位置写函数名，不写易漂移的行号；初稿核对的是第 3 批读方（`8b9592de3`）落码之前的树。本节订正上面 §12 的两条说法（第 2 条一次推翻两句；第 3 条是附加条件，不是更正）。改动与验证命令见 `docs/PROGRESS.md`「2026-10-19 登录吞吐第二轮」与「2026-10-19 scene_manager 跨 zone 重定向选 gate 改随机二选一……」两条。

**一句话**：登录只取逻辑服（home_zone），不取物理落点（storage_id）；这一步本身很轻（1 次 RPC + 1 次 MGET，R 个键，每个值只是几字节的 zone 号十进制串），不是瓶颈。卡水平扩展的是它所在的那台 Redis：账号目录、会话、锁、映射、PlayerAllData、存盘控制面全挤在全服唯一的单实例上（2026-10-19 补：切到已接入部署路径的高可用形态后也只是同一个主，见下表「共享 Redis」行）。数据面进程（login / data_service / scene_manager 的请求处理）无状态、能加副本，存储层今天不能扩。

**登录链路上对归属 / 落点的实际读写**

| 环节 | 读写 | 存在哪 | 次数 | 出处 |
|---|---|---|---|---|
| 账号登录 Login | 读账号目录 `account:{account}`（一段 Lua：GET，键不存在就 SET 空壳，带 PERSIST） | 核心 Redis（login 的 `Node.RedisClient`），没有 DB 副本、没有 TTL | 每次 Login 1 次 | `loginlogic.go` 的 `getOrInitUserAccountScript` |
| 账号登录 Login | 角色列表经 data_service `BatchGetPlayerHomeZone` 改写成当前归属：mapping Redis 上对 `player:zone:{id}` 一次 MGET（R 个键）；500ms 预算，失败保留建角 zone（fail-open） | mapping Redis | 每次 Login 1 次 | `loginlogic.go` 的 `roleListWithCurrentHomeZone`；`homezone.go` 的 `RefreshRoleZones`；`router.go` 的 `Router.BatchGetPlayerHomeZone` |
| 一次完整登录 | 新路径会执行**两次** Login（HTTP `/api/login`，再在 gate 上用 access_token 登录），上面两行各 2 次 | — | ×2 | `robot/main.go` 的 `loginAndEnterViaHttpGateway` → `loginAndEnterAccessToken` |
| EnterGame | 默认**不**查归属（`HomeZone.RedirectOnEnterEnabled=false`，K8s 不写该段） | — | 0 | `entergamelogic.go` 的 RedirectOnEnter 分支；`go/login/internal/config/config.go` |
| scene_manager EnterScene | 每次带 GateId 的进场（含**每次换图**）经 data_service `GetPlayerHomeZone` 在 mapping Redis 上跑一段 EVAL（读 home + 合服围栏）；跨 zone 传送第一条腿另查 1 次 | mapping Redis | 每次进场 / 换图 1 次 | `enterscenelogic.go` 第 3b 步；`home_zone.go`；`router.go` 的 `GetPlayerHomeZoneAndMergeFence` |
| 物理落点 `player:placement` | **login 全程不读**；go/db 每条读写任务 2 次 MGET（选库 + 落库后复核，两键 placement + zone） | mapping Redis | 每条 DBTask 2 次；一次存盘（2 条写任务）4 次；一次冷登录（父键缺失时 2 条读任务）4 次 | `go/db/internal/kafka/placement_route.go`；`ensure_player_all_data_async.go` |
| 目录库 `player_route` | 不在在线路径上：K8s 固定 `RouteStore.Mode=redis`，提交点是 mapping Redis 上的 SETNX Lua | — | 0 | `k8s_deploy.ps1` 的 data-service ConfigMap `RouteStore` 段；`router.go` 的 `setPlayerZoneIfAbsentScript` |

**gate 上 access_token 那次 Login 的角色列表不能省**（2026-10-19 经负责人授权只读核对 `../mmorpg-client` HEAD `09c6ccea`）：Unity 客户端的选角、建角、本地名牌与外观缓存只用这份列表（`Assets/Scripts/Game/GameClient.cs` 的 `ConnectAndEnter` → `CacheRoleMetadata` / `PlayerChooser`；列表为空时选角页直接跳建角页，不挂界面的自动路径会给已有角色的账号静默再建一个号）；跨 gate 重定向不走 HTTP，这份列表是唯一来源。robot（`robot/login.go` 的 `loginAndEnterAccessToken`）同样只读它。Java 版这次回包同样带列表，契约文档（Java `docs/reference/mmorpg-client-contract-login.md`）写明登录成功时带 `players`。反过来，HTTP `/api/login` 那份列表 Unity 客户端只打日志、字段也不全（只有 id / 名字 / 等级），robot 不解析它；要省一份只能省 HTTP 那份，但它是 Java gateway 的 HTTP 契约，要两版同批改，未做。（2026-10-19 负责人定：两份都保持现状，按业界常规流程——HTTP 认证取 token、连 gate 后由 gate 登录回包给角色列表——不再改。）若要降低 gate 那次的开销，方向是削减 `roleListWithCurrentHomeZone` 的补齐成本（缺外观时整份读 PlayerAllData），而不是去掉列表。

稳态下 mapping 读量的大头**不是登录**：是存盘（每次存盘 4 次 MGET）与换图（容量模型 S3 档换图速率约为登录速率的 6 倍）。容量模型 §2.5 `redis-mapping` 行的「2E」与 G4a 稿的「E + 列表数/s」都漏了换图那一项。

**按吞吐看的撞墙顺序与状态**（倍数全是推算，见下「证据边界」）

| 环节 | 现状 | 加机器能扩吗 | 要做的事 / 状态（2026-10-19） |
|---|---|---|---|
| 共享 Redis | K8s 上 login、data_service（MappingRedis）、scene_manager、player_locator 全指向 `redis.${InfraNamespace}:6379`：全服同一个主。（2026-10-19 订正）原写「单实例、replicas 1；`redis-ha.yaml` 未接入任何部署路径」已过时：形态由 `k8s_deploy.ps1 -RedisReplicas` 决定，dev 档缺省单实例（`redis.yaml`，replicas 1），staging / prod 档缺省 3 = `redis-ha.yaml`（1 主 2 从 + 每 Pod 一个哨兵 + 2 个 haproxy 代理，Service redis 指向代理，客户端地址不变）；2026-10-16 接入、10-19 补齐连接上限与 WAITAOF 副本份数按拓扑渲染，均未运行、待 Codex 验证，未在任何真实集群切换过。高可用只消除单点故障，读写仍压在一个主上，不改变右列「不能」 | **不能** | 第一步 mapping 拆独立实例（G4a，设计稿未落码）并上 HA；上 Cluster 的阻塞见下「映射链路的 Cluster 阻塞点」；10-17 已拍板「单元化 + 单元内 Cluster」，修订稿与代码都未做 |
| AssignGate 读 gate 目录 | 原为每请求一次 etcd 线性一致 Range | 部分 | **已落码、未验证**：list-watch 进程内镜像，镜像不新鲜回落直读（`gate-load-balancing-design.md`「gate 目录怎么读、怎么挑」） |
| PickGate | 原为按 10s 刷新的人数确定性取最少，开新区一个窗口可把 4.5k~5 万连接压到同一台 | 部分 | **已落码、未验证**：随机二选一（`go/shared/gatepick`，login 快路径与 scene_manager 跨 zone 重定向共用，重定向并补排空过滤；Java `GatePicker` 同步）。K8s 默认每 zone 2 台时：两台人数不等与旧规则相同；全体同人数（开新区第一个上报窗口）时从全落 NodeID 最小的一台变为两台各半；之后的窗口里羊群依旧，充分摊开要每 zone ≥3 台。单台连接上限感知、死 gate 新鲜度过滤未做 |
| task:result | 全服一个频道广播 | 部分 | B0-3 按 login 实例定向两批已落码，待 Codex 验证 |
| 账号目录 `account:{a}` | 只在核心 Redis，没有 DB 副本 | 不能 | 落库位置待拍板；现成的 `user_accounts.simple_players` 列（`go/db/model/mysql_database_table.sql`）没有任何代码写它 |
| 新号首登冷读 | 父键缺失时固定发 2 条读任务 | 部分 | 新号免冷读已落码、默认关，打开要拍板 |
| db_first 之后的建角 | WAITAOF 与登录角色列表、进场查询共用 data_service 的 mapping 连接池（go-zero 按地址共用一个客户端），建角上限约 40Z/s，尖峰时饿住读 | — | 见下「对 §12 的更正」第 2 条；修法登记在本节，D1 后续批次处理 |
| login / data_service / scene_manager 进程 | 请求处理无状态，每 zone 2 副本 | 数据面**能** | 直接加副本（目前没有 HPA）。例外：scene_manager 编排 leader 全服唯一（`scene_manager:leader:lock`，不分 zone）；login 排队 dispatcher 每 zone 一个 leader；data_service 每副本 +35 条全局 MySQL 连接，受 `k8s_deploy.ps1` 连接总账约束 |

**映射链路的 Cluster 阻塞点**（2026-10-18 首次写入设计文档；共享库整体的清单见 [cross-zone-matchmaking.md](./cross-zone-matchmaking.md) §10.0）

1. 三把映射键（`player:zone` / `player:placement` / `player:maprev`）没有共同 hash tag；改键与 `db-task-pipeline-bottlenecks.md` §6.5「不为 Cluster 改 hash tag 键名」冲突，待负责人裁决（容量模型 §4.2）。
2. **读脚本在脚本里拼键**：`GetPlayerHomeZoneAndMergeFence` 读出 home 后拼出 `merge:in_progress:{home}` 再 EXISTS，这个键不在 KEYS 里，Cluster 下会跨 slot；`decodeHomeZoneAndMergeFenceReply` 把无法解读的回复按合服处理，结果是已映射的玩家进场一律被拒（fail-closed）。`router.go` 的代码注释早有说明，但此前没有任何文档登记。改法不能简单拆成两次读（读 home 与读围栏之间合服完成会读到过时 home），要和 D1「围栏挪进库」一起设计。
3. 建角 W1 的 `setPlayerZoneIfAbsentScript` 同时碰 `player:zone` 与 `merge:in_progress`，跨 slot。
4. go/db 的两键 MGET（`placement_route.go` 选库与复核）跨 slot。
5. **第 3 批就绪标记**（`8b9592de3` 已落码）：`go/shared/placement/route_ready.go` 的 `RouteReadyReadScript` 把全局单键 `mapping:route:ready` 与逐玩家映射键放进同一段脚本，脚本里还调 INFO 取 `run_id`；data_service 的 `getHomeZoneFenceReadyScript` 在此之上还拼围栏键，go/db 的 `route_ready_gate.go` 同理。标记绑定的是单实例身份，hash tag 修不了，Cluster 下要按分片重新设计（或不用标记）。
6. 2026-10-18 新增的多键脚本：`healAccountRoleScript`（4 个 KEYS：账号目录、玩家锁、账号锁、反向索引）、`removeOrphansScript`（2+N 个 KEYS）、scene_manager 排空查询对多台 gate 的 MGET（读失败放行）。它们在核心 / 共享实例上，归共享库那份清单。
7. C++ hiredis 每进程一条连接、不处理 MOVED/ASK（容量模型 §2.5 末行）。

**对 §12 的更正**

1. §12 表里「go/db 每条存盘一次 MGET」不对：每条读写任务 2 次（选库一次、落库后复核一次），一次存盘 4 次；另有 scene_manager 每次换图也读。`shared-redis-split-by-purpose.md` 里的 2 次是对的。
2. §12「WAITAOF 只拉长单次建角的延迟、不降吞吐」「目录库成为瓶颈不会拖慢登录、进场、存盘」在 db_first 下不成立：发布与 WAITAOF 走 `r.mappingRedis.PipelinedCtx`（`route_publish.go`），与 `GetPlayerHomeZone`、`BatchGetPlayerHomeZone` 是同一个句柄；go-zero v1.10.0 按地址只建一个客户端（`redisclientmanager.go` 按 Addr 缓存），共用连接池（data_service Pod 500m 时约 10 条）与熔断器。每次 WAITAOF 占住一条连接约 0.5s，建角尖峰会饿住登录角色列表与进场查询。修法方向：db_first 的发布 + WAITAOF 改用独立的 go-redis 客户端（独立连接池，顺带隔离熔断器，可与 §11.2 第 9 条的熔断隔离备选合并）；不能再用同地址的 `MustNewRedis`，go-zero 会把它合并回同一个池。属于 D1 后续批次；本节只登记，不改 `go/data_service`（D1 会话在途）。
3. §12「读路径始终是 Redis 副本，不读库」今天仍成立（Mode=redis）。附加条件：第 3 批读方已落码（`8b9592de3`，`RouteStore.ReadRequiresReady` 缺省关、只许 db_first）；打开之后，在就绪标记挂上之前（1b-2 对账 leader 落地前恒不挂），每次登录、进场都会各打一条 SQL 且不回填缓存，读放大与限流尚无评估（规格 `spec_D1_batch1b2_2_3.md`）。

**证据边界**：最近一次压测是 R19（2026-06-18，1 zone 45k，`docs/stress/prev-summary.txt`），EnterGame 约 73/s 是 t15m→t18m 的爬坡斜率，不是饱和点。它早于角色列表查 home_zone（`11ba15a0f`，2026-09-08）与 scene_manager 进场查归属（`b8f56c58d`，2026-09-17），**当前登录路径没有任何压测覆盖**；mapping Redis 的 ops / CPU 也没有实测。上面所有倍数都是推算，以 tidb-phase1 §7 的 L-12 / L-14 压测为准。

**顺带发现并已处理的问题**（已落码，未编译、未测试）：

- K8s 与 compose 栈的 login `DataServiceRpc` 熔断与本地对齐为开；
- EnterGame 账号目录自愈原为无锁 GET + 整块 SET，改为账号锁 + 锁内重读 + 整字节 CAS，提交点复核反向索引；
- 运维 `RemovePlayersFromAccounts` 的实现改为同一把账号锁内原子摘孤儿并删反向索引（脚本可重发）。**它目前没有生产入口**：gRPC `LoginAdmin.RemovePlayersFromAccounts` 原是返回空成功的生成桩，已改为明确返回 Unimplemented，接线另开任务。

**开服前复核**：本节结论不能直接沿用到开服，开服前由 AI 按 §12.2 对着当时的代码、部署与压测重做一遍（L-18）。（2026-10-20 补：静态部分已按 §12.2 第 1~4、6 步复核，结论不变，订正与补漏见 §12.3；第 5 步压测未做。）

**仍待负责人决定**：拆 Redis 与 HA 的排期；hash tag 改键与 §6.5 的冲突；读脚本拼键与就绪标记在 Cluster 下的改法（随 D1 围栏挪库）；账号目录落库位置；打开新号免冷读；单台 gate 连接上限如何让 login 知道；运维摘孤儿要不要接上 gRPC 入口。

**Java 版**：没有映射层，归属区就是 `game.player.zone_id` 列，登录时按主键点查一次单库（`PlayerStore.findPlayer`），另读一次可降级的 Redis `game:location:{player_id}`；进程能多开，存储同样是单点，这是 Java 单库的既定设计（扩展要换分布式库，不在本轮）。选 gate 已同步为随机二选一（Java `PARITY.md` 末行）。对应 Java 版本号 0.1.0-SNAPSHOT。

### 12.2 上线前复核步骤（负责人 2026-10-19 要求：开服前由 AI 再查一遍「登录取逻辑服 / 物理服是不是瓶颈、能不能水平扩展」）

登记在 [tidb-phase1-and-launch-readiness.md](./tidb-phase1-and-launch-readiness.md) §7 L-18 与 `docs/ops/release-checklist.md` A.1。复核要对着**当时**的代码、部署和压测重做，不能照抄 §12.1：§12.1 写于开服前很久，当时当前登录路径没有任何压测覆盖，倍数全是推算。按 AGENTS §10.1，Claude 做静态核对与文档，压测与汇总交 Codex。

1. **读路径有没有变**（静态核对，按函数名搜，不按行号）：
   - login：`Login` 两条路径（HTTP 与 gate access_token）各自对 mapping Redis、核心 Redis、data_service、库的调用次数；`roleListWithCurrentHomeZone` 的补齐成本（缺外观时整份读 PlayerAllData、缺名时回源注册表）有没有被削减或变重；EnterGame 的 `RedirectOnEnterEnabled` 现网取值。
   - data_service：`BatchGetPlayerHomeZone`、`GetPlayerHomeZoneAndMergeFence` 是否仍只读 mapping Redis；`RouteStore.Mode` 与 `ReadRequiresReady` 的现网取值（打开回源后，每次登录 / 进场在标记挂上前各打一条 SQL）。
   - scene_manager：每次进场 / 换图的归属查询是否仍是 1 次 EVAL；go/db：每条读写任务的映射 MGET 次数。
   - 新增的任何「每次登录都要做」的跨服务调用，都按「全服单份 / 每 zone / 每实例」三类归档。
2. **存储拓扑有没有变**：`k8s_deploy.ps1` 里 login `Node.RedisClient`、data_service `MappingRedis`、scene_manager、go/db `Placement.Redis` 实际指向哪些实例；G4a（mapping 独立实例）、`redis-ha.yaml`、单元化 / Cluster 是否已落地（`redis-ha.yaml` 2026-10-16 起已接入部署路径、staging / prod 档缺省，但接入不等于线上已切：以线上 `sts/redis` 的副本数为准，1 = 单实例、3 = 高可用）；单实例的 `maxclients`、`maxmemory`、淘汰策略与 PVC。仍是全服单实例就仍然「不能水平扩」，结论不变；切到高可用也只是一个主，结论同样不变。
   - （2026-10-19 补）核对各 zone 的 data-service ConfigMap 的 `RouteStore.WaitAOFNumReplicas` 与 go/db ConfigMap 的 `ServerConfig.RedisClient.WaitAOFNumReplicas` 与线上 Redis 形态一致：单实例 0、高可用 1（两处由 `k8s_deploy.ps1` 的同一个 `Get-RouteStoreWaitAOFNumReplicas` 按拓扑渲染，对照部署输出的 `Redis topology: ... waitaof_replicas=` 行）；运行中的 db / data-service Pod 也要已在该 ConfigMap 之后重启过（只改 ConfigMap 不会滚动它们）。高可用下仍是 0 = 切主时已答复的写可能不在新主上；单实例下写成 1 = WAITAOF 启动探针等不到副本，go/db 拒启（data-service 在 db_first 下同样不装配 route store、拒启）。部署脚本这部分未运行、待 Codex 验证。
3. **db_first 是否已开**：开了的话核对 §12.1 更正第 2 条（WAITAOF 与读共用连接池）是否已改为独立连接池，没改就把「建角尖峰饿住登录 / 进场」列为开服风险。
4. **gate 侧**：每 zone gate 台数（≥3 台随机二选一才有实际收益）；`login_gate_directory_reads_total{source}` 在压测里是否以 `cache` 为主；单台连接上限感知、死 gate 新鲜度过滤是否已做。
5. **压测证据**（交 Codex，按 AGENTS §6 全流程，即 L-14）：
   - 前置：`stress_summarize.ps1` 目前不汇总 data_service 归属查询、scene_manager 归属查询的耗时，也不汇总 mapping Redis 的 ops / CPU / 延迟；开服前要先补这几项，否则「是不是瓶颈」没有数据回答。
   - 看：登录与进场速率的饱和点（不是爬坡斜率）、各阶段 p99、mapping 与核心 Redis 的 ops 与 CPU、`login_gate_directory_reads_total` 的 source 分布、gate 落点分布。
   - 没有与 `prev-summary.txt` 的对比表，不下「是 / 不是瓶颈」的结论。
6. **写回**：结论更新到 §12.1 两张表（状态列写复核日期），追加一条 PROGRESS（写明复核依据的 commit 与压测轮次），L-18 标为已复核；仍不能扩的环节列出要拍板的事项。Java 版同步核对（`game.player.zone_id` 单库点查、`GatePicker`），在 Java `PARITY.md` 记录结论。

### 12.3 2026-10-20 L-18 静态复核（§12.2 第 1~4、6 步；第 5 步压测未做）

> 怎么来的：负责人 10-20 再问「登录拿逻辑服和物理服还是不是瓶颈、能不能水平扩展」。本节由四路只读核对完成：login 路径；归属读方（data_service / scene_manager / go/db）；存储拓扑与进程扩展；选 gate、压测证据与 Java。每路再由一名质疑者逐条复核，并与开服线交接会话 10-19 晚的静态结果（未经复核）交叉比对。没有编译、测试或压测。
> 基线：核对开始时 HEAD 为 `c63b22dca` 加工作区，核对期间前进到 `49b1aa889`。期间 `c35e1133d` 把下文提到的对账 leader 接线、`isZoneGoneFromRegistry`、router.go 注释订正、k8s_deploy 拓扑函数去重等改动入了库。`10fa5d939` 之后的两次提交只改了 go/db 就绪闸的认领细节与注释，不改变下面的计数。代码位置写函数名。§12.1 原文不改，订正与补漏都写在本节。

**一句话**：结论与 §12.1 相同。
- 登录只取逻辑服（home_zone），不取物理落点（storage_id），也不读路由库 `player_route`。
- 取逻辑服这一步很轻：每次 Login 1 次 data_service RPC，落到 mapping Redis 是 1 次 MGET（R 个键）。
- 卡水平扩展的仍是存储。K8s 上下面这些全部连 `redis.${InfraNamespace}:6379`，是全服同一个主：login、data_service、scene_manager、player_locator、go/db（含落点句柄）、C++ gate / scene / battle（`zoneredis` 名字像每 zone 一份，其实也是它）、Java gateway 限流。G4a、单元化、Cluster 都没有任何代码。
- 进程层能加副本。
- L-18 本次只算「静态部分已复核」。第 5 步没做，而且做之前要先补观测（见下「压测前置」）。所以「是不是瓶颈」仍然没有数据回答。

**每次操作打到共享 Redis 的命令数**

前提是 K8s 缺省开关：`RouteStore.Mode=redis`，`ReadRequiresReady`、`Placement.RequireRouteReady` 都关，登录排队被合入门槛强制关。

| 操作 | 映射键（mapping） | 同一实例上的其他往返（login 直接发起） | 说明 |
|---|---|---|---|
| 一次完整热登录：AssignGate + HTTP Login + gate Login + EnterGame | 2 次 MGET（R 键）+ 1 次 EVAL（scene_manager 第 3b 步，GET + EXISTS，不调 INFO） | AssignGate 1 次排空标记 MGET；HTTP Login 约 5 次；gate Login 约 7+D 次（D = 设备集成员数，K8s 上限 3）；EnterGame 约 15 次（同步段与预加载、异步链合计） | R=0（未建角）时不发 MGET。staging / prod 另有 Java gateway 限流：`/api/assign-gate` 与 `/api/login` 各过一次 zone 桶 + IP 桶，Bucket4j CAS 模式每个桶至少 2 次往返，合计至少 8 次 |
| 冷登录（父键缺失）再加 | go/db 4 次 MGET（2 条读任务 × 选库 + 复核，两键） | 每条读回执 LPUSH + EXPIRE + PUBLISH，login 侧各 LPOP 1 次 | 这 2 条读任务本身各打 1 次 zone 库 SQL |
| 进场 / 换图 | 1 次 EVAL | — | 跨 zone 传送 2 次 EVAL，另有 1 次 etcd 前缀直读和 1 次排空标记 MGET |
| 一次存盘 | 4 次 MGET（2 条写任务 × 2，两键） | — | 2 条写任务本身是 zone 库 SQL |

映射读只是这个实例负载的一小部分。会话、锁、PlayerAllData、存盘游标、epoch 守卫、缓存回写都打在同一个主上，本节没有逐条去数。

**开关打开后计数怎么变**
- 只开 `Mode=db_first`：三列的读不变。变的有三处：
  - 建角 W1 变成 EXISTS + 三键 MGET + SQL + EVAL + WAITAOF。
  - 全服多出一个对账 leader，见下「新增的全服单点」。
  - §12.1 更正第 2 条**仍未修**：发布与 WAITAOF 仍走 `r.mappingRedis.PipelinedCtx`，和读路径共用 go-zero 同地址客户端的池与熔断器。K8s 只开 `--appendonly yes`，appendfsync 用缺省 everysec，每次 WAITAOF 最长约 1s。500m 限额下 automaxprocs 把 GOMAXPROCS 定为 1，池约 10 条（推断，待 Pod 实测）。同一副本约 10 个并发建角就能占满这个池。
- 打开 `ReadRequiresReady`：登录的 2 次 MGET 和进场的 EVAL，都换成含 2 次 INFO 的就绪读脚本（`EvalCtx`，每次发完整脚本正文）。标记不可信时，每次再回源 1 条 SQL，不回填缓存，也没有限流。
- 打开 `Placement.RequireRouteReady`：存盘 / 冷登录的 4 次 MGET 换成 4 次 EVALSHA，共 8 次 INFO；每个 go/db 进程另加每秒 1 次闸刷新。不可信时停止消费，不回源。

**相对 §12.1 的订正**
1. 补外观的成本被低估了：
   - `CreatePlayer` 接受空 `appearance_id`，robot 建角恒为空。这类角色每次 Login（一次完整登录 ×2）都在核心 Redis 上整份 MGET PlayerAllData 父键，永远修不好。
   - `roleListWithCurrentHomeZone` 实际是三段串行：归属 500ms、外观 1s、名字 500ms。注释里的「两次串行」已过时。
   - 这些预算只限制 ctx，限制不住单次 socket 读：login 的核心 Redis 客户端（`servicecontext.go` 的 `redis.NewClient`）只传了 Addr / Password / DB / PoolSize。ConfigMap 里的 Dial / Read / WriteTimeout 3s 没有传进去，实际用的是 go-redis 缺省值（读超时 3s、重试 3 次）。核心 Redis 变慢时，这一段最坏约 4s，不是 2s（推断）。
   - Unity 客户端建角带不带外观，本次未核（需客户端授权）。
2. EnterGame 不只「首登」读整份存档。凡是判为 FirstLogin 的进场，即 player_locator 里没有在线会话，包括老号每天的正常登录，都会读两份整存档加一份账号目录：`beginParentLoad` 脚本带回整份，`backfillPlayerIdentity` 再 GET 一次。
3. 「K8s 固定 `Mode=redis`」已过时。`-RouteStoreMode` 现在是带粘滞核对的可切换开关，缺省 redis。`data_service.yaml` 与 `k8s_deploy.ps1` 各留了一句旧注释。
4. task:result 的「现状」列已过时：已按 login 实例定向，但仍在同一个 Redis 上，用的是 PUBLISH 而不是 SPUBLISH。
5. Cluster 阻塞点第 2 条的后果要细化：读脚本按 EVAL 兼容模式执行，拼出的围栏键在本节点时静默放行，在别的节点才报错并按合服拒绝。N 个主节点时约 (N−1)/N 的已映射玩家被拒，不是「一律」被拒。用例 `TestRealRedisClusterUndeclaredKeys` 已入库，但没有设集群地址就 Skip，至今没有运行证据。`route_read.go` 的 `getHomeZoneFenceReadyScript` 注释仍是旧口径。
6. 「login 排队 dispatcher 每 zone 一个 leader」今天不存在：排队被 `$script:LoginQueueMergeGateCleared=$false` 强制关闭。
7. Java 那段的表述订正：
   - 「登录时主键点查」实际发生在 EnterGame，同时读一次 `game:location`，送回原实例时读 2 次；每次 EnterGame 还至少跑一次单库 `claimOwnership` 写事务。
   - 账号登录是 `INSERT IGNORE account` + 按 `idx_player_account` 查角色列表，一次完整登录做两遍。
   - Java 的 gate 目录每次 AssignGate 整表读 Redis RMapCache，没有进程内镜像，TTL 15s。
   - PARITY「选 gate 随机二选一」行的 mmorpg commit 列仍写「未提交」，实际已随 `e611b5d8d` 入库。

**§12.1 漏列的环节**

| 环节 | 现状 | 加机器能扩吗 | 备注 |
|---|---|---|---|
| Java gateway 限流（staging / prod 缺省开） | 每 zone 一个 `rl:zone:{id}` 桶，被该 zone 全部 gateway 副本 CAS 争用；单次桶操作时限 500ms，超时或出错时放行（fail-open） | 不能（每 zone 单键写热点） | 开服洪峰时冲突重试会放大往返，超过时限就不扣桶直接放行 |
| gate Login 经 client-rpc-router（K8s 缺省 `-GateRouterMode 1`） | 多一跳 gRPC；全局池，不连 Redis，用 list-watch 镜像 | 能 | — |
| player_locator 会话对账 leader | 全服一个（`player_locator:session_reconciler:leader`），后台任务 | 不能 | 不在登录同步路径上 |
| data_service 路由对账 leader | 全服一个，租约在全局库 `route_reconcile` id=1；只在 db_first 下装配（已在 HEAD 接线，未编译、未测试） | 不能 | 每拍对共享 DB 0 做一次全库 `SCAN merge:in_progress:*`。这一步不看 Arm，从 P2 起就跑。三把映射键都没有 TTL，所以扫描量随累计注册人数增长；一拍扫不完 3s，标记就一直挂不上。预热 / 修补峰值约 20000 EVALSHA/s，外加约 6 万键/s 的 MGET，正好和读方回源、go/db 闸关着的窗口重叠。成本均未评估 |
| data_service / scene_manager / player_locator 的发现键 | `dataservice.rpc` / `scenemanagerservice.rpc` / `playerlocator.rpc` 不带 zone，实际是 2×Z 副本的全服共用池 | RPC 数据面能 | 「每 zone 2 副本」不能按 zone 独立算容量。data_service 的 Kafka 消费组不带 zone，并行度被分区数封顶（交易流水 6、快照 3）。go/db 每 zone 副本被 `PartitionCnt=10` 封顶，不能配 HPA |
| 其他经 data_service 读映射的服务 | friend（好友列表、推荐）、match、trade 走 BatchGet；guild、player_inbox 每次操作 1 次 EVAL | — | 客户端进游戏就拉好友列表的话，每次登录还要多 MGET。本次未核客户端行为 |

**Cluster 阻塞点补 4 条**（接 §12.1 的 7 条）

8. 登录在线路径本身：`BatchGetPlayerHomeZone` 对 R 个**不同玩家**的键一次 MGET，即使加上 `{player_id}` hash tag 也跨 slot，要改成按 slot / 节点拆分的 pipeline。就绪读路径 `batchGetPlayerHomeZoneReady` 同样如此。
9. db_first 的发布脚本 `PublishRouteKeys` 有 4 个 KEYS，含 `merge:in_progress:{zone}`。hash tag 能把前三把放进同一个 slot，第四把放不进去。
10. `routereconcile.NewRedisCache` 拒绝非 node 类型的 MappingRedis，所以 mapping 一上 Cluster，对账 leader 就整体不可用。guild 也是就绪标记读方（`MergeMarkerRedis.RequireRouteReady`），与第 5 条同病。
11. 登录路径的多键脚本：`backfillPlayerClassScript`、`commitLoginScript`、`beginParentLoadScript` / `publishParentLoadScript`、`accountRecordCASScript`。它们把 login、player_locator、scene_manager 的键绑在同一实例上。共享库那份清单（`shared-redis-split-by-purpose.md`）已登记，§12.1 漏列。

**存储层的数值与新事实**（§12.1 没写）
- Redis 单实例与高可用两种形态：
  - 两份清单都是 `maxclients 40000`、AOF 开。
  - 都不设 `maxmemory`，淘汰策略取缺省 noeviction（刻意为之，上界待 G4a §23 与压测再定）。
  - 只设 requests、不设 limits，PVC 8Gi。
- 高可用形态的代理比主节点更早到线：
  - 业务连接全部经 2 个 haproxy，每台 `PROXY_MAXCONN=13000`。稳态上限 26000，一台不在时只剩 13000，告警线 10400。
  - 按规划档每 zone 约 264 条推算，Z=36 触发代理告警；单实例形态 Z=118 才触发。
  - `13000×(2+1)+1000` 恰好等于 40000，没有余量。要加代理副本，必须同时调小 `PROXY_MAXCONN` 或调大 `MAXCLIENTS`，否则 `Get-RedisHaLimits` 会 throw。
  - 代理吞吐与多出这一跳的延迟都没有数据。
- 部署脚本拓扑函数重复定义：旧 HEAD `c63b22dca` 的 `k8s_deploy.ps1` 有 7 个 Redis 拓扑函数各定义两遍，后定义的旧版生效，「zone 写路径跟随线上 `sts/redis`」因此不生效。已由 `c35e1133d` 去重入库，未运行，部署前要交 Codex 验证。
- G4a 落码时有两个耦合要同批解开：`k8s_zone_rollback.ps1` 硬性要求 MappingRedis 与 go/db RedisClient 同一实例同一 DB，契约测试也钉住了这一点。
- 共享 MySQL 是单个 `mysql:8.0` Deployment（replicas 1，`max_connections 2100`），全局库与全部 zone 库都在上面。按连接总账公式推算（最坏假设每个 go/db 常驻其余全部 zone 库）：规划档（db_first）Z=5 峰值约 2292，超限；今天的 redis 模式 Z=6 约 2428 才超。超限只告警，不阻断。以 DryRun 的「MySQL connection ledger」行为准。
- 全部 zone 共用 infra 里一套 3 节点 etcd。登录在线路径只读、gate 目录已改为镜像，推断今天不会先撞到它。
- （推断，未运行）go/db 的 K8s ConfigMap 原样镜像 `go/db/etc/db.yaml` 的 `ServerConfig.Database.RequireTiDB: true`，但 `Database.Hosts` 固定是 `mysql.${InfraNamespace}:3306`，k8s_deploy 也没有 go/db 的 TiDB 主机参数。按 ConfigMap 注释与 `proto_sql/db.go` 的 `authoritysql.AssertCurrentSession`，K8s 上 go/db 会在启动断言处拒启。zone 库在 K8s 上的后端是 MySQL 还是 TiDB 要先定；上面以 MySQL 连接数为墙的推算，前提也取决于这一点。

**gate 侧（第 4 步）**
- 每 zone 仍是 2 台，此时随机二选一基本等于取最少。镜像与 `gatepick.TwoChoices` 已入库（`e611b5d8d`），未编译、未测试。
- 单台连接上限感知没做，死 gate 新鲜度过滤也没做：没在原地址重启的死 gate 会在候选里留满 etcd 租约 180s。
- scene_manager 跨 zone 重定向每次直读 etcd，没有镜像。断线租约内从别区 gate 重登，或打开 `RedirectOnEnterEnabled` 后，登录也会走这条路。
- 每个 login 副本另有一条后台直读：排空监视每 5s 一次 etcd 前缀 Range。

**压测前置（第 5 步做之前）**
- 光改 `stress_summarize.ps1` 不够：
  - scene_manager 的 `resolveHomeZone` 不在任何 `enter_scene_stage` 计时段里。
  - data_service 没有归属读耗时直方图（`route_read_total` 只在 ReadRequiresReady=true 时计数），它的 prometheus 开关也是关的，go-zero 内置的 `rpc_server_*` 替代不了。
  - 这两侧要先在代码里加指标。
- login 侧本机裸进程跑时不用改代码：go-zero 客户端拦截器缺省就记 `rpc_client_requests_duration_ms` / `rpc_client_requests_code_total{method=".../BatchGetPlayerHomeZone"}`，超时与熔断拒绝按 code 计入，改汇总脚本解析 :9101 即可。
- **K8s 上 login 没有任何指标端点**：ConfigMap 没有 Prometheus / DevServer 段，manifest 只开 50000。`login_gate_directory_reads_total`、`entergame_*`、`dataloader_*`、`rpc_client_*` 全部不计数。L-14 若在 K8s 专用压测环境跑（开服线拍板清单 T-13 的推荐），login 侧一张表都出不来。
- `stress_snap.ps1` 不抓 data_service 的 :9260；mapping Redis 服务端的 ops / CPU / 延迟也没有采集入口。
- 以上已登记为开服线拍板清单 X-3（`docs/handoff/launch-readiness-20261013/decisions_launch_line_20261020.md`）。R19（2026-06-18）之后没有任何压测，本工作区也没有 `robot/logs`。

**仍待负责人决定**（§12.1 原列各项仍有效，下面是新增）
- 补外观是否改成只取需要的字段，或者要求建角必带外观（降核心 Redis 带宽）；
- K8s 每 zone gate 是否提到 ≥3 台；
- X-3 指派：加指标、开 K8s login 指标端口、改汇总脚本；
- 对账 leader 的围栏镜像对账是否从全库 SCAN 改为按库里的 zone 清单逐把读；
- 全局库何时拆到独立 MySQL 实例，Z≥5 时连接预算超限；
- go/db 在 K8s 上 `RequireTiDB` 与 MySQL 主机的矛盾怎么定；
- login 核心 Redis 客户端是否接上配置的超时。

**Java 版**：结论不变。进程能多开，存储是单库加单 Redis（`useSingleServer`），这是 Java 单库的既定设计。没有新的 Java 待做项，按「Java 仓只记待做」的约定，不往 `PARITY.md` 加行。上面第 7 条的 commit 列过时属于账本卫生，留给维护 Java 仓的会话顺手回填。对应 Java 版本号 0.1.0-SNAPSHOT。


---

## 13. 第 1b-2 / 1b-3 / 第 2 / 第 3 批:最小设计修订与落码(2026-10-18 ~ 19)

> 状态:**已落码,未编译、未测试**(AGENTS.md §10.1,待 Codex 按 PROGRESS 2026-10-19「D1 剩余批次落码完成」条目执行)。开关全部保持安全缺省(Mode=redis、ArmReadyMarker=false、三个读方开关 false),**代码完成不等于启用**,任何环境的切换按 §13.4 逐阶段由负责人决定。
> 落码规格(逐条细节、评审吸收、偏离与用例清单)是 [spec_D1_batch1b2_2_3.md](../handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md);本节只记结论,两者冲突以本节为准。

### 13.1 为什么要修订:已批准方案会写错库

§5.1 的判据「标记在 ⇒ 缺键是权威缺席」要求标记与数据同生同灭。实际上下面几种情况,标记会和回退了的数据一起留下来,要等 leader 下一拍(无上界)才撤得掉,期间读方把缺键当「没有归属」、go/db 走 legacy 写本进程 zone 库:Redis 从旧备份恢复;哨兵提升落后副本(`WAITAOF 1 1` 的确认可能不在被提升的那台);全量预热途中 FLUSH;旧 leader 迟到挂标记;映射实例被部分拷贝。「每秒对账最终修好」消除不了这些窗口。

### 13.2 修订(不改 DDL、不改已有键格式、不改客户端契约)

| # | 修订 | 关掉的窗口 |
|---|---|---|
| M1 | 读方用同一段只读 Lua(`placement.RouteReadyReadScript`)取标记、本实例身份(INFO run_id / role / master_replid)与映射键;`VerifyRouteReady` 只有「标记可解码且身份等于本次读所在的主实例」才 Trusted。不可信时值与缺键都不用:go/db 写延后不耗预算并暂停取消息(就绪闸),读耗预算;data_service 回源 SQL(SQL 故障回 Unavailable,绝不 NotFound);guild 当封锁 | 重启、切主、旧备份、拷贝、多实例 |
| M2 | 预热令牌 `mapping:route:warm`:挂标记用 `RouteReadyArmScript`(令牌未变 + 标记不在 + 脚本内身份复核)+ 同连接 WAITAOF | 预热途中 FLUSH、旧 leader 迟到挂标记、rewarm |
| M3 | 水位键 `mapping:route:watermark`:库回档判据改为「水位键世代 ≠ 库世代 或 水位键水位 > 库水位」(容差 0),不依赖标记;unfreeze / rewarm 有显式出口 | ArmReadyMarker=false 期间的库回档 |
| M4 | ArmReadyMarker=false 时 leader 每拍撤标记 | 回退时忘了 rewarm |
| M5 | 标记不在一律全量预热;回看修补只用于「标记在、身份不符」(同一份数据的延续);部分拷贝迁移必须 rewarm | 空实例重启、部分拷贝 |
| — | 围栏镜像按 `fence_epoch` set-if-newer、TTL = 库 expires_at_ms + 10 分钟,由 leader 双向对账;旧格式镜像不删只告警(foreign_fence) | 失权旧工具覆盖新持有者的镜像;镜像残留永久封区 |

**不换 generation**(沿用 1b-1 的 B1,也是对任务原文「新 generation」的取舍):M1 + 水位键已能认出旧快照与再次回档;换代会让标记失效,S3 下读方要 fail-closed 到全量预热完成(数小时)。

### 13.3 各批落码

- **1b-2**:`internal/store/route_reconcile.go`(R23–R30,库时钟租约、AdvanceReconcile、FreezeByLeader、按片增量扫描);`internal/routereconcile/`(leader:每拍顺序、M2–M5、围栏镜像对账、armingBlocked 闩锁、cvm 持久化语义、关停有界);config `RouteStore.Reconcile`;metrics;svc 装配与 MissingStores;`data_service.go` 启停;`routetool/rewarm.go`、audit `-require-maprev`、unfreeze 水位键出口。
- **1b-3**:`k8s_deploy.ps1` 五个转发开关 + `-ConfirmRouteSwitchChange` 粘滞防翻转 + 非法组合拒绝 + Redis 连接总账计入 leader 池,契约测试;`deploy/k8s/data-service-route-alerts.yaml`;`tools/scripts/k8s_route_tool.ps1` 与 Job 模板;清库脚本在任 leader 防护;mapping Redis `--enable-debug-command no`。`k8s_zone_rollback.ps1` 未改(另一项改动在重写):db_first 下 K8s zone 回档在 zone_rollback_discard 落地前不可执行。
- **第 2 批**:`tools/merge_zone/route_store.go`(merge_zone 唯一写路由表的文件,开写前 P1–P5 探针、围栏 64 片获取 / 续期 / 释放、批事务锁 63 号片且先确保行存在、全部批原语)、`route_publish.go`(镜像 shared 脚本,单连接 pipeline + WAITAOF、catchUp)与源码比对守卫;W5–W12 与 relocate(只换 `relocatePlacements` 实现)、R8 读方、`verify:route_cache`;`-route-source db|redis` 必填并用水位键核对库与 Redis 成对;dev_tools 必填转发;集成测试 harness 区分 PASS / FAIL / SKIP。W2 / W3 保持停用。
- **第 3 批**:go/db(`route_ready_gate.go`、`placement_route.go`、`Placement.RequireRouteReady`)、data_service(`store/route_reads.go`、`routing/route_read.go`、`RouteStore.ReadRequiresReady`、R7 db_first 读库)、guild(`MergeMarkerRedis.RequireRouteReady`)、`tools/data_consistency_check`(`-route-db-dsn`、不再吞错)。

### 13.4 上线顺序与回退

前进:P1 冻结窗口 + 导入 + 审计 → P2 data_service 切 db_first(leader 运行、Arm=false,补发孤儿行、做回档检测)→ P3 二次导入 + `audit -require-maprev` 为 0 → 全服 Arm=true → leader 预热挂标记 → P4 新版 merge_zone(`-route-source=db`)、禁用旧二进制、解除冻结窗口 → P5 读方逐个打开(go/db 逐 zone → data_service → guild),门禁看 `data_service_route_ready_selfcheck`。
回退:**先关全部读方开关 → Arm=false(看到 `marker_dropped_disarmed`)→ Mode=redis → `rewarm -wait=0`**;不删表。项目尚未上线(AGENTS.md 启动门禁「上线状态」),混跑窗口类残余上线前不阻塞,首次上线按上面顺序整批起步。

### 13.5 仍未完成 / 残余风险

1. ~~四张路由表的 proto 列漂移守卫(第 1a 批偏离第 1 条)仍未做。~~ **已做(2026-10-20 D1 收尾,未编译,待 Codex 重生成 proto 并验证)**:实为五张(player_route、zone_merge_fence、zone_merged_into、route_meta、route_reconcile)。`proto/common/database/rollback_database_table.proto` 追加五个 message(player_route 沿用 DDL 预留的 pb:1..10,另四张按列序新定字段号;可空列用零值表达,与 `store.RouteRow` 同约定);`schema.go` 的 `routeTableShapes` 删除,`routeTableMessages()` 进 `TableMessages()`(只为表名守卫,同步循环跳过),迁移期按 proto 字段查列名、按 `OptionPrimaryKey` 查主键列序;`route_store_test` 双向比对 proto 字段与 DDL 列、player_route 的 pb 注释与字段号、`RouteRow` 与 `dbpb.PlayerRoute` 逐字段对应(`RouteRow` 保留为领域类型,写明映射,AGENTS §3 的并行 struct 由该用例机械守住)。DDL 不改。
2. INFO 在 Lua 里的可用性(Redis 7.2)与每次读多两次 INFO 的开销未实测;mapping Redis 若用 ACL,账号要放行 INFO(@dangerous)。
3. `DEBUG RELOAD NOSAVE` / `LOADAOF` 在进程内换数据不换身份:靠 `enable-debug-command no` 与 leader 自检;淘汰策略被改到下一拍之间的键丢失只能靠 leader 每拍自检。
4. redis-ha 网络分区时被隔离的旧主仍自报 master 的情形【推断】;WAITAOF 按全局偏移覆盖此前写是推断。
5. S3 规模全量预热数小时(先单标记,压测后再定是否按片)。
6. 第 1b-2 / 1b-3 / 2 / 3 批叠在未验证的 1a / G1a / P5 / 1b-1 之上,验证必须按顺序。
7. Java:副本机制不适用;合服围栏、撤销合服、归属变更审计随 7.3 **待做**(PARITY D1 行已改)。

## 14. 2026-10-21 全球正式默认与首次启动

项目尚未上线，全球逻辑服务不再把P阶段旧redis/关闭proof当正式默认。DS正式配置固定RequireTiDB、db_first、64围栏片、reconcile/Arm/ReadRequiresReady、RoleTradeSQLProof与Store.Required；普通池和路由池共用authoritysql物理强读会话，清除snapshot/staleness/replica/low-TSO暗回档入口。DB正式配置RequireTiDB、Placement.Required及RequireRouteReady；Login正式PinOnCreate且必须具备DS目标和独立目录签名能力。服务显式LegacyNonGlobal仅允许dev/test隔离历史工具夹具，Mode=dev本身不降级。正式K8s各档位均要求同一显式TiDB入口，不部署Redis权威/关闭ready；旧开关关闭组合仅保留工具与拒绝回归。

角色SQL存盘证明的storage_id白名单仅从既有独立玩家库白名单反解：本机DB_ALLOWED_DATABASES（或启动器显式MMORPG_DB_ALLOWED_DATABASES），K8s现MMORPG_DB_ALLOWED_DATABASES同时渲染DB AllowedDatabases与DS AllowedStorageIDs。库名只接受正规zone_1..999999_db或player_store_1000000..uint32max_db，最多1024、拒重复及双来源分叉；不得从Regions、当前ZoneId、请求或缓存猜白名单。实际证据仍按同事务SQL player_route原落点读取正规两张玩家表，白名单不能改落点。

全新空环境的可执行顺序：先预建独立清单内的数据库并用现役DS -migrate迁移global；用go/db既有cmd/migrate -command up对每个白名单storage执行正式玩家表迁移；然后起DS。DS仅在SQL无任何route、account_role无历史、每个已配置正规落点的主双表与player_centre_database确实为空、旧user_accounts.simple_players无非空角色原blob，且Mapping Redis无历史zone/placement/maprev/merged_into时认作新空环境。centre与旧账号表归属来自GoDB同一TablesFromJSON正式迁移清单；只查白名单落点库，不猜查独立LOGIN_PASSWORD_AUTH_DSN。仅有密码账户且角色目录NULL/空不算玩家历史，非空未知角色blob不能证明为空。每个落点一条查询包含四个LIMIT1 EXISTS，不COUNT；共享3秒启动探针预算，缺正规表/列、权限、未知或超时均拒绝。对账leader沿既有全量预热/水位/实例身份协议自然挂ready，不手写marker；随后DB现有消费/重试ready闸放行，最后启动Login和客户端入口。

存量环境先停止旧映射写者，保留原Redis/SQL原件，运行既有route-tool import-from-redis -allow-db-first -dry-run=false及audit；缺placement者还须沿正规merge_zone pin-placement流程补齐，不能在正常入场时偷偷创建默认home。然后起DS自然reconcile，严格audit -require-maprev为0并核真实ready后再开放DB/Login。SQL路由为空但Redis为空也不能证明全新：目录或任一白名单存盘有记录必须拒启并要求正规恢复。SQL已有route时不做每副本全表startup audit，继续原逐行maprev/水位防回档协议。恢复/回档不得使用第13节旧“切回Redis”的服务回退；只许保持全球强一致合同的正规工具恢复。

本批不新增表或proto，不伪造导入/ready凭证；新增配置、空库/存量负例、真实TiDB玩家历史和caller权限回归均仍待根代理实际执行。原DS/DB/Login/SM运行证据仅代表此前字节，不代表本批当前源。


---

## 15. 2026-10-23 负责人拍板：映射改为直读 TiDB，撤掉 Redis 副本（设计已定，未落码）

> **状态**：方向已拍板，本节是落码依据；**一行代码都还没改**。取代 §2「Redis 完整副本 + 就绪标记」、§5.1 / §5.4、§13 的 M1–M5、第 1b-2 批对账 leader 与第 3 批读方。保留：库为真源、四张（含 proto 守卫共五张）路由表、合服围栏落库、提交点在 TiDB 事务。
> **来源**：负责人在会话「账号登录逻辑/物理服瓶颈」里，面对三个选项（直读 TiDB / Redis 分片副本 / 这次不动）选了「直读 TiDB（推荐）」。选项原文：去掉 Redis 映射副本，`player_route` 只在 TiDB 一份，登录、进场、go/db 直接 SQL 点查，go/db 在同一 TiDB 事务里核落点；删掉就绪标记、对账 leader、预热、发布等待。负责人同日说明：项目未上线，玩家数据可完全清空。
> **与 §14 的关系**：§14（2026-10-21 全球正式默认与首次启动）里依赖副本的部分随本节作废：对账 leader 自然挂 ready、go/db 消费 / 重试就绪闸放行、「Mapping Redis 无历史 zone/placement/maprev/merged_into」这一条空环境判据、存量环境的 `route-tool import-from-redis` 与 `audit -require-maprev`。首次启动改为：预建并迁移全局库与白名单落点库 → 起 data_service（§14 的 SQL 空环境判据保留）→ 直接起 go/db 与 login，不再等待 ready。§14 其余内容（RequireTiDB、Placement.Required、PinOnCreate、storage_id 白名单反解、强读会话）保留。
> **依据**：§12.3 与 2026-10-22~23 的两轮多代理静态核对（基线 HEAD `940ef9746` / `2c07f2c28`）；D1 移交 [d1-review-fix-handoff-20261023.md](../handoff/d1-review-fix-handoff-20261023.md)；数据服务路由审计会话给出的写门禁契约（[data_service_role_and_scope.md](./data_service_role_and_scope.md)「写门禁契约」「已澄清」）。本节的删除清单是汇总结果，**不是穷举**；落码第一步先按 §15.7 B0 做全仓穷举盘点。

### 15.1 为什么改

- 2026-10-21 全球权威改动（`1b41d8f1e`）之后，全部 SQL（全局库、全部 zone / player_store 库）已在同一个外部 TiDB 集群上。登录链路上唯一不能水平扩展的环节，就是挂在单个共享 Redis 主上的映射副本。
- 副本要靠「绑定实例身份的就绪标记」才能判可信（§13 M1）。这个设计天然只能单实例，上不了 Cluster（§12.3 阻塞点 5、10）；Redis 重启或切主后要全量预热数小时，期间 go/db 全服停消费、读方全部回源（§13.5 第 5 条，R2 审计 r2-rr#5）。
- 直读 TiDB 只有一份数据，一致性由 TiDB 事务直接给出；`player_route` 主键已按 `(route_shard, player_id)` 聚簇打散（§11.2 G1a），点查随 TiKV / tidb-server 加节点扩展。代价是每次读从约 0.2ms（Redis）变成约 1~2ms（同地域 TiDB 点查，推断，待压测）。
- 未上线、数据可清空：不写迁移、混跑、回退兼容，首次上线整批起步。

### 15.2 读路径（改造后）

| 读方 | 改造前 | 改造后 | 失败语义 |
|---|---|---|---|
| login 角色列表 `BatchGetPlayerHomeZone`（每次 Login 1 次） | data_service 在 mapping Redis 上跑就绪读脚本（GET 标记 + 2 次 INFO + MGET），不可信回源 | data_service 一条 `SELECT player_id, home_zone FROM player_route WHERE (route_shard, player_id) IN (…)`（≤ `RouteBatchLimit`，TiDB BatchPointGet） | 不变：login 失败放行、保留建角 zone；data_service 侧 SQL 故障回 Unavailable，不回 NotFound；调用方取消不计 sql_error（M1-2 口径保留） |
| scene_manager 进场 / 换图 `GetPlayerHomeZone` | 一段 EVAL（GET home + EXISTS 围栏键） | 现有 `RouteStore.GetHomeAndFence`：**同一条 SQL** 读出 home 与 `zone_merge_fence`（A16：预检的 home 与围栏必须同一读） | 无记录回 NotFound；SQL 故障回 Unavailable（故障码 1，不降级、不回落别的源） |
| data_service 写门禁（`AcquirePlayerWriteLock`） | 两次 EVAL 读 home + 围栏 | 预检与提交点复核都改为 `GetHomeAndFence`；顺序保持「预检 → 两把玩家锁 → 数据锁 SETNX 之后复核」，复核看到围栏 / home 变化 / 读失败先放锁再拒绝（回 22） | 同上 |
| go/db 每条读写任务（选库 + 落库后复核，现为 2 次两键 MGET 或就绪脚本） | Redis | 见 §15.3「go/db 在写事务里锁读路由」 | 三态，见 §15.5 |
| guild 合服围栏（`MergeMarkerRedis.RequireRouteReady` 就绪读） | Redis 围栏镜像 | 在 guild 现有的 TiDB 会话里用库名限定读 `<全局库>.zone_merge_fence` | 读失败按封锁处理（与现行 fail-closed 一致） |
| friend / match / trade / player_inbox | 经 data_service RPC | 接口不变，随 data_service 内部改读 SQL | 不变 |
| saveoutbox 补投 | 跨 zone 迁移前直读 `player:zone` | 由「Redis 物理水平扩展设计」会话的 saveoutbox 改造删掉这处映射读，home 是否匹配改由消费端 go/db 判（[redis-player-store-cluster.md](./redis-player-store-cluster.md) §5.13） | 见 §15.5 第 1 条 |

### 15.3 写路径与 go/db 的一致性

- **建角 W1**：data_service `RegisterPlayerZone` 只剩一个 TiDB 事务（读 `route_meta`、`FOR UPDATE` 围栏分片行、INSERT `player_route`），提交即完成。删掉发布脚本与 WAITAOF，`publish_redis.go` 整个文件删除。
- **合服 / 搬库（merge_zone）**：`tools/merge_zone/route_store.go` 只写 SQL；删除 Redis 镜像发布（`route_publish.go`、catchUp、源码比对守卫）、`-route-source`（只剩 db）、`verify:route_cache` 审计与 WAITAOF 参数。D1 移交 §2.1 里仍成立的围栏接手 / 续期 / 换源拒绝 / 失权中止 / 时限释放全部保留。合服围栏只在 TiDB `zone_merge_fence`，删掉 `merge:in_progress:{zone}` 镜像。
- **go/db 在写事务里锁读路由**（取代「选库 MGET + 落库后复核 MGET」）：
  1. 先无锁点查 `player_route` 拿到 `storage_id` / `home_zone` / 冻结状态 / 行版本，用来选目标库连接池（go/db 仍按库开池，不改池结构）；
  2. 在目标库连接上开写事务，第一条语句用库名限定 `SELECT … FROM <全局库>.player_route WHERE … FOR UPDATE` 重读并核对行版本与第 1 步一致（不一致：本次放弃、按最新路由重试），然后写两张玩家表、提交。同一 TiDB 集群内跨库事务原子；
  3. 搬库 / 合服写者改路由行时同样先锁路由行。**锁序固定为「路由行 → 玩家数据行」**，两边一致，不会互锁；
  4. TiDB 对 `FOR SHARE` / `LOCK IN SHARE MODE` 默认不支持（开 `tidb_enable_noop_functions` 后也只是空操作，推断，落码时用真 TiDB 用例确认），所以用 `FOR UPDATE`。同一玩家的写本来就被排序锁串行，额外的行锁只与搬库 / 合服竞争；
  5. 冷登录读任务：在目标库连接上开读事务，先 `FOR UPDATE` 锁读路由行核对未冻结、版本一致，再读两张玩家表、提交。只发生在冷登录，锁持有时间短。
  这样「搬库要等在途写完成」（§13 R2）由 TiDB 行锁直接保证，不再需要 SQL 之后的二次复核与就绪闸。go/db 需要新配置项：全局库名（`Placement.RouteSchema`），以及该账号对全局库 `player_route` 的读与加锁权限。

### 15.4 删除与保留清单（汇总，B0 穷举后补全）

> 2026-10-30：B0 已完成，逐文件清单、对本节的更正（`route_meta` 整表保留、`MappingRedis` 还承载名字缓存等）与漏项见 §15.9。

- **data_service**：删 `routing/route_read.go` 的就绪读、`routing/publish_redis.go`、`route_publish.go` 的发布与 WAITAOF（W1 的 SQL 部分保留）、`internal/routereconcile/` 整包、routetool 的 rewarm / 水位键出口 / `audit -require-maprev`；配置删 `RouteStore.Mode`（只剩 SQL，不再有 redis / db_first 之分）/ `ReadRequiresReady` / `Reconcile.*` / `ArmReadyMarker` / `PublishRedisPoolSize` / `WaitAOF*`，`global_authority.go` 的强制组合随之简化；指标删 `route_ready_*`，`route_read_total` 改为只计 SQL 读结局（ok / not_found / error）。D1 会话留下的未提交读方门禁改动（M1-5 / M1-6 / `PrimeRouteEnumerate` 及两份测试）随之删改。
- **`go/shared/placement`**：删 `route_ready.go`（就绪脚本、Arm 脚本、`VerifyRouteReady`、水位键）与 `route.go` 的发布脚本、`PublishRouteKeys`、`MapRevKey`；保留按 player_id 计算路由分片等纯函数。
- **go/db**：删 `route_ready_gate.go`、`placement_route.go` 的 Redis 读（改为 §15.3）、配置 `Placement.Redis` / `RequireRouteReady` / `RouteReadyGateIntervalMs`、`ValidateGlobalAuthority` 里对应的组合、就绪指标；`placement_lookup` / `placement_recheck` 两个 Redis 步骤指标改为 SQL 步骤。`db:capability:zone:{z}` 是给旧版 go/db 作证的临时标记，未上线、整批起步时不再需要，一并删除。**保留** go/db 核心 Redis 上 persistBeforeAck 用的 WAITAOF（与映射无关）。
- **guild**：删 `MergeMarkerRedis` 句柄与就绪读，改读 SQL 围栏。
- **tools**：merge_zone 见 §15.3；`data_consistency_check` 删 redis 口径，保留 D1 移交里已落码的读失败判 fail；`dev_tools.ps1` 删 `-MergeZoneRouteSource` 与 WAITAOF 转发；`k8s_route_tool.ps1` 删 rewarm。
- **部署**：`k8s_deploy.ps1` 删 `-RouteStoreMode` / `-RouteReconcileArm` / `-RouteReadRequiresReady` / `-DbPlacementRequireRouteReady` / `-GuildRequireRouteReady` 五个开关、粘滞核对与组合门禁；Redis 连接总账删对账 leader 池与发布池；`deploy/k8s/data-service-route-alerts.yaml` 删副本 / 就绪 / 发布类告警；`redis.yaml` 的 `--enable-debug-command no` 保留（加固，无副作用）。`Get-RouteStoreWaitAOFNumReplicas` 若只剩 go/db 核心 Redis 一个用途，改名并注明。
- **表**：删 `route_reconcile`（leader 租约）及 `route_meta` 里只为副本服务的水位 / 世代列；`player_route` 的行版本列保留，作为 §15.3 的乐观核对版本。同步改 proto 守卫（`rollback_database_table.proto` 末尾 message、`schema.go` 的 `routeTableMessages`），按 AGENTS §4 重生成。
- **保留 data_service 的 `MappingRedis` 句柄**：写门禁的玩家锁（`AcquirePlayerLock`、数据锁 SETNX）仍在这台实例上（路由审计会话 10-23 刚改过，`router_lock_test.go` 依赖它）。句柄只剩锁这一个用途，注释与配置说明改成「玩家写锁」；锁是否挪走另议。

### 15.5 错误语义（必须保持）

1. **go/db 路由读三态**（与「Redis 物理水平扩展设计」会话的约定）：读失败（超时、断连、TiDB 不可用）→ 可重试延后，不 ACK、进重试，**不进 stale_topic 死信，也不经 save_outbox_note 触发 markTerminal**；确定无记录 → `Placement.Required` 语义（写进死信、读回失败）；读到且 home≠本 zone → 才判 stale。补回归用例「读错不进 stale、不终结补投意图」。
2. data_service 路由读失败一律 fail-closed，按故障回码 1，日志 `[DataRoute] <op> player=<id>:<原因>`，指标 label 不放 player_id；映射缺席不另造新码（4 不在 FaultCodeSet，会把故障哑掉）。
3. login 角色列表补齐仍失败放行；scene_manager 进场归属读失败可重试拒绝。
4. merge_zone「结果不明不释放围栏」原则保留（D1 移交 B1-1）。

### 15.6 容量与扩展边界（推断，以 L-14 压测为准）

- 每次热登录：2 次批量点查（R 个 id）+ 账号目录已有的 SQL；每次进场 / 换图 1 次带围栏的点查；每条 DBTask 1 次无锁点查 + 写事务内 1 次锁读。存盘量按容量模型口径（在线 / 300 × 2 条任务）估算，S3 档是每秒十万级点查，需要相应数量的 tidb-server / TiKV，**不再有单主上限**。
- 热点：`player_route` 已按分片聚簇；号段 `player_id` 单调导致的索引尾部热点由 G1a 的分片前缀消除。`zone_merge_fence` 只在合服时写。
- TiDB 不可用时：登录（账号目录本来就已依赖 TiDB）与存盘（消息留在 Kafka）一起停，爆炸半径与账号目录相同，不新增依赖。
- data_service route store 池（`RouteStore.MaxOpenConn`，缺省 20）与账号目录池（`AccountDirectory.MaxOpenConn`）都计入 TiDB 连接预算；TiDB 侧连接总账仍缺（§12.3、开服线拍板清单），一并补。

### 15.7 分批实现计划（每批：实现 → 对抗复核 → 修复；编译测试交 Codex）

- **B0 穷举盘点**（只读）：按键字面量、配置键、指标名、K8s 参数、测试文件、文档引用多路扫描，产出逐文件清单，补全 §15.4。
- **B1 data_service + `go/shared/placement`**：读写路径改 SQL、删副本相关包与配置、改指标与测试。保留 `router.go` 的玩家锁段。
- **B2 go/db**：§15.3 写事务锁读、三态语义、配置与测试。`servicecontext.go` / `config.go` 与「Redis 物理水平扩展设计」会话第 4 批同改，按「先落的通知后落的」。
- **B3 guild、merge_zone、data_consistency_check、dev_tools**。
- **B4 部署**：k8s_deploy 开关与总账、告警、route tool、契约测试。
- **B5 文档**：merge-zone runbook、release-checklist（加「写过落点记录之后禁止回退到不认落点的 go/db」）、§12.3 复核结论更新、G4a 稿指向注（由 G4a 会话加）、PROGRESS。
- 验证（Codex）：data_service / go/db / guild / merge_zone / data_consistency_check 的 build、vet、单测与真 TiDB 用例（MySQL 通过不能代替 TiDB）、PowerShell 契约测试；L-14 压测看 D1 / D2 / M1 表。

### 15.8 对其他线的边界

- 「Redis 物理水平扩展设计」：映射域已从其分片方案撤出（[redis-player-store-cluster.md](./redis-player-store-cluster.md) §5.13）；saveoutbox 归它。
- G4a（[shared-redis-split-by-purpose.md](./shared-redis-split-by-purpose.md)）：不再需要 redis-mapping 实例；`MappingRedis` 只留给 data_service 玩家写锁。§18 / §21 / §25.2 第 7 条由 G4a 会话加「被取代」指向注。
- D1：[d1-review-fix-handoff-20261023.md](../handoff/d1-review-fix-handoff-20261023.md) §2 仍成立的项并入本节各批；§3 作废项不再修。
- **Java 版**：不适用。Java 没有映射层，归属区就是单库 `game.player.zone_id` 列，按约定不往 `PARITY.md` 加行。


### 2026-10-26：全 SQL 写资格与路由维护接缝（候选，未验证）

本批采用 [存盘围栏设计的 v4 实施段](db-write-fencing-mysql.md) 中的 writer 目录、维护阶段和永久票据。route_meta.writable 不再接受无正式请求的独立翻转；route_tool 的冻结和解冻通过显式 Operator 与 Catalog 操作，normal data_service 保持自己的 WriterConfig。对账检测到危险时只能关闭本 boot 的入口、保留危险事实，再由独立维护 owner 申请紧急 HOLD；normal 服务不能暗持 Operator 凭据。

冻结、解冻均保留原世代与业务行语义。正常释放所需水位下限固定到 ReleaseReadiness.route_watermark_floor_ms，必须覆盖审核时同世代可信 ReadyMarker 与 Redis watermark 下限；Catalog 在 slots→reconcile→meta→run→ticket 顺序下，于同一事务抬 route_reconcile.watermark_ms 并推进 E/writable。请求或审核原件一旦封装，不因当前观测变化而原地重封。已有 Redis CAS 后处理继续按原结果判断，UNKNOWN 保持维护。

当前生产主入口、真实发行者关闭/新 boot prepare-open 仍在逐项接线；相应遗漏路径单列待范围决策。候选只读形状检查、独立 fresh-prestart 与测试夹具的 Schema 能力已分开；未执行生成、构建、真库或压力测试。Java 版由同批独立实现对齐，不复制 R 的 route_reconcile 结构。

### 15.9 B0 穷举盘点结果（2026-10-30，只读，未改代码）

> **基线**：HEAD `720179638` 加当日工作区。D1 收尾会话用三路只读代理扫描了键字面量、配置键、指标名、K8s 参数、测试与文档引用，又亲自抽查了以下几处：`RequireReconcile` 调用点、`route_meta` / `route_reconcile` 建表语句、名字缓存所在实例、home 读错误码、`savePlayerRow` 事务边界、proto2mysql v0.2.0 的事务接口。
> **落码归属**：会话「映射直读 TiDB 落码（提案 §15，B0~B5）」，10-30 起受「账号登录逻辑/物理服瓶颈」委派。本节是它的 B0 输入，可直接修订。
> 行号随改动漂移，下文只写文件与符号。三路原始报告（含行号）暂存在该收尾会话的 scratchpad，以本节为准。

**结论**

1. **§15 仍未落码。** `go/shared/placement/route_ready.go`、`go/db/internal/kafka/route_ready_gate.go`、`go/data_service/internal/routing/publish_redis.go`、`tools/merge_zone/route_publish.go`、`go/data_service/internal/routereconcile/` 都还在。
2. **规模（推断，按下文清单计）。**
   - data_service 加 placement 约 105 个文件。
   - go/db、guild、merge_zone、data_consistency_check、脚本、部署、CI，以及 scene_manager / login 的过时注释，约 160 个。
   - 写资格框架、proto、其余测试夹具约 16 个。
   - 合计约 280 个手改文件，不含 proto 生成物和 Java 仓 3 处，远超 AGENTS §10.2 的 30 文件门槛。
3. **与本节 10-26 补段正面冲突。**
   - 补段把 v4 维护协议的释放水位建在 `route_reconcile.watermark_ms`、ReadyMarker 和 Redis watermark 上：锁序是 slots→reconcile→meta→run→ticket，释放事务内用 `GREATEST` 抬水位。这三样恰是 §15 的删除对象。
   - R 侧 6 个 `authoritysql.CatalogConfig` 调用点全部写的是 `RequireReconcile: true`。
   - 代码里已有 `RequireReconcile=false` 分支：不锁 reconcile，floor 必须为 0。这就是 J 的形状。§15 落地后 R 收敛到这一形状即可，不需要另起协议。
4. **待负责人决定。**
   - (a) 10-23 拍板的 §15 与 10-26 补段，以哪一份为准。
   - (b) §15 与 Codex 全球同服验收谁先谁后。§15 会改 Codex 正在验收的 authoritysql、barrierrestore、drainproof、proto 和维护权限脚本，受影响的验收项见文末。
   - (c) 约 280 文件的改动授权。
   - (d) B2 的 go/db 写路径怎么做（落码会话 10-30 补）。[TiDB 决策](./global-data-layer-tidb-decision.md) §9.3 已写明映射不在 §9 范围，§15 的结论不变。但 §9.4 第 1 条还没定：存盘改为 scene 经 data_service 直接提交 TiDB，还是保留 Kafka → go/db。若选前者，go/db 写路径整体退场，§15.3 的写事务锁读和 T1（proto2mysql 事务出口）都白做。候选：
     - 按 §15.3 做；
     - 先把 go/db 现有的「选库读 + 落库后复核读」从 Redis 一比一换成 SQL 点查，保持现在的正确性；写事务内锁读等 §9.4 定了，再并入新的存盘路径。这一种偏离了负责人所选选项的原文「go/db 在同一 TiDB 事务里核落点」，所以要负责人定。
     - **（2026-10-30 补，上面原文不改）§9.4 第 1 条已定**：用户在会话「玩家数据存储方案」选了存盘直接提交 TiDB，存盘服务放 go/db（原话「放 go/db」），见[总体方案](./player-data-tidb-scaling-plan.md) §3。所以上面「go/db 写路径整体退场」的前提不成立：退场的只是 go/db 的 Kafka 消费路径，go/db 改成接 RPC 的存盘服务，仍在一个 TiDB 事务里写玩家行。「go/db 在同一 TiDB 事务里核落点」和 T1（proto2mysql 事务出口）在新路径上仍然需要；Kafka 写路径本身会退役，所以不必为它单独做一遍。(d) 的具体做法落码前与该总体方案的第 1 批规格对齐。其余 (a)(b)(c) 仍待负责人定。
   - 盘点人建议在独立分支上落码：分支编译、测试通过后整批合入主线。这样多批次改动期间主工作区不会处于半改状态，也不打断其他会话和 Codex 的验收。

**对 §15.4 的更正**

- **`route_meta` 整表保留。** 它没有水位列，水位和租约都在 `route_reconcile`。唯一的世代列 `generation` 是写资格框架的 writer generation：NORMAL 写者每个事务都核它，merge_zone 的冻结证明、drainproof、J 侧初始化也核它。它不属于「只为副本服务」的列。
- **`MappingRedis` 句柄保留，说明改为「玩家写锁与名字缓存」。** 它除了两把玩家写锁，还承载名字缓存 `player:name:{id}`（见 `routing/player_name_cache.go`）。
- **另外两个 Redis 句柄也不能整撤：**
  - merge_zone 的映射 Redis 句柄：preflight 查 `lock:player` 要用。
  - data_consistency_check 的 `-redis-addr`：扫 `guild_rank:zone:*` 要用。
- **D1 读方门禁改动已入库。** §15.4 写的「未提交」改动（M1-5 / M1-6 / `PrimeRouteEnumerate`）已经提交，仍按删改处理。
- **错误码按现有分层口径理解，不另造新码。** gRPC 层 home 读遇 SQL 故障现回 `ErrCodeRouteStoreError`（30），in-band 回 1。§15.5 第 2 条「回码 1」按这一分层口径读。
- **k8s_deploy 有两处与原文不符：**
  - `Get-RouteStoreWaitAOFNumReplicas` 去掉路由用途后还剩 go/db 和 scene-manager OwnerEpochMint 两处用途，不是只剩一处。
  - Redis 总账里本来就没有发布池，只有一条过时注释，`PublishRedisPoolSize` 从未进过总账。
- **`tools/scripts/k8s_zone_rollback.ps1` 不在本批改动范围。** 它只有 `merge:in_progress` 判据的注释过时，按 D1 交接约定不碰，交该脚本的负责会话处理。

**§15.4 漏项（必须同批改）**

| 编号 | 位置 | 说明 |
|---|---|---|
| L1 | `data_service.go`、`svc/servicecontext.go` | SOURCE 排空闭合（`closeSource` 用 `RouteReconciler.SettledDigest`）、`onDanger` / `RouteEmergencyError` 的危险原件与 Hold、prepare-only boot 推迟 route owner，这几处都挂在对账 leader 上。删对账包时要同时重新定义 SOURCE 闭合 |
| L2 | `routetoolcli/cli.go`、`cmd/writer_authority/main.go`、`routetool/freeze.go` | v4 维护 P7 导出 `RouteRecoveryAudit`（内含 ReadyMarker、水位键、INFO 原件、floor、mapping 目标）；P8 解冻时核 floor；`RequireReconcile: true` |
| L3 | `store/account_role_sql_proof.go`、`account_role_admission_tidb_test.go` | 角色交易的维护恢复证明用 `FOR UPDATE` 读 `route_reconcile.watermark_ms`，再与 `PromotionPlan.RouteWatermark` 比对；测试夹具往 mapping Redis 预置路由键 |
| L4 | `routetool/*`、`store/route_store.go` | 除 rewarm 外，还有 import-from-redis、audit、reconcile-from-cache 三个子命令；store 侧还有 `Import*` / `Repair*` / `MaintenanceFreeze` 和 Freeze/Unfreeze 存根 |
| L5 | `svc/route_bootstrap.go` | 首启空库判据仍在 SCAN 四族 Redis 键 |
| L6 | `routing/router.go` | redis 模式的整套读写：注册、home、围栏、批量、按 zone 枚举、删除 |
| L7 | `route_store.go`、`route_publish.go`、`metrics.go`、`dataserviceserver.go` | W1 Redis 预检的残留：`ExistingRowOnly`、`RouteRegisterRowMissing`、`ErrRouteUnknownToStore`、`precheck_*` 与 `refused_unknown_row` 两类结局、default 分支回 `ErrCodeRedis` |
| L8 | data_service `metrics/metrics.go` | 除 `route_ready_*` 外，还有 `route_publish_total`、`route_register_total` 的预检结局、`route_reconcile_*` 全族、`route_writable`、`route_cache_verified_age_seconds`、SQL 池的 `includeRoute` |
| L9 | `config/config.go`、`global_authority.go`、`etc/data_service.yaml` | `RouteStore.PublishAttempts`；散落各处的 `DBFirst()` 调用点；`LegacyNonGlobal` 的「历史 Redis 夹具」语义会随 redis 模式一起消失 |
| L11 | `cmd/debugutil`、`cmd/debug_fetch`、`cmd/debug_import` | 选库、映射登记、写目标检查全部依赖 mapping Redis 的 `player:zone` / `player:placement`，以及 go/db `Placement.Redis` 的镜像，要改成读全局库 `player_route` |
| L12 | `shared/placement/placement.go` | `Key` / `HomeZoneKey` / `MergeFenceKey`、`CapabilityKey*`、Redis 值格式，外部调用方在 go/db、barrierrestore、guild。等这些调用方改完，最后一批再删 |
| L13 | `store/schema.go`、`store/route_tidb.go` | `idx_route_updated` 及其预切只服务对账增量扫描（推断），随对账一起删 |
| L14 | `store/route_reads.go` | RD4 `ListActiveMergeFences` 只服务对账与 rewarm，删 |
| L15 | `logic/data_logic_test.go`、`server/dataserviceserver_test.go` 等 | 同包共享的测试 helper 依赖 redis 模式和 miniredis，需要新增内存版 RouteStore / RouteReader |
| L16 | `routing/router.go` 的 `readWriteRoute` | dev 模式把 `ErrHomeZoneNotMapped` 当作「未登记、可写」；改读 SQL 后这个口径要重新定 |
| L19 | `store/route_reconcile.go` 及其 3 份测试 | 与 `routereconcile` 是两个包，§15.4 没有点名 |
| F1 | `go/shared/authoritysql`（`catalog.go`、`maintenance.go`、`sqltest/fixture.go`） | Advance / GrantTicket / RevokeTicket / Freeze / Release / publicationReleased 里的 reconcile 行锁；`route_reconcile` 建表与种子行（`CatalogBootstrapStatements` 是 data_service 建表和各服务测试夹具的唯一事实源）；floor 校验与 `GREATEST` 写入 |
| F2 | `go/shared/drainproof/report.go` | `RouteRecoveryReadScript` / `ParseRouteRecoverySnapshot` 读 ReadyMarker 与水位键，依赖 `placement/route_ready.go` |
| F3 | `go/db/internal/barrierrestore`（`route.go`、`restore.go`、`scripts.go`；`drain.go` 间接）、`go/db/cmd/save_barrier_restore/main.go` | redis / split / db_first 三种路由模式；reconcile 锁；mapping 键检查；`merge:in_progress` 广播围栏（§15 删镜像后，这把 Redis guard 挡不住任何写入，要改成 SQL 围栏）；整套路由 P7 审核；75 键分支；清单里 `route_source` 缺省为 redis |
| F4 | `tools/merge_zone`（`main.go`、`current_tidb.go`、`team_authority.go`、`audit_checks.go`、`audit_resources.go`） | `RequireReconcile` 两处；`-route-source` 判断；P7 必需检查 `verify:route_cache` / `verify:merge_fence`，要与 `restore.go` 的必需检查表一起改 |
| F5 | `proto/common/database/rollback_database_table.proto` | `route_reconcile`、`RouteRecoveryAudit` / `RouteRecoveryStats`、`ReleaseReadiness.route_watermark_floor_ms`、`PromotionPlan.route_watermark` 与 `mapping_addr` / `mapping_db`。删字段写 `reserved`，按 AGENTS §4 重新生成 |
| F6 | `deploy/sql/authority_operator_role.sql`、`authority_runtime_role.sql`、`tools/scripts/maintenance_drain.ps1` 及其测试 | 删 `route_reconcile` 的授权，包括 10-28 新增的列级 UPDATE 特例和 schema 账号的 CREATE / INSERT；runtime 角色补 `player_route` 授权（见 T8） |
| F7 | go/chat、go/guild、go/player_inbox 的 TiDB 测试夹具 | 自建 `route_reconcile` 表和种子行，或者解析 `routeReconcileBootstrapDDL` |
| F8 | Java 仓 | `InitializeRouteAuthority.java` 的 floor==0 校验、game-player-store `route_authority.proto` 的 floor 与 `RouteRecoveryAudit`、game-proto 副本（经 ContractSync）。§15.8「Java 不适用」对映射层成立，但这三处要随 proto 同批对齐 |
| G1 | `go/db/internal/saveoutbox`（`replay.go`、`service.go`）、`internal/drain/session.go` | RouteGate 接缝与维护排空的签名。另外 `db.go` 交给 saveoutbox 的 mapping 客户端会随 `Placement.Redis` 一起消失，必须先和「Redis 物理水平扩展设计」会话的 saveoutbox 改造约好先后 |
| G2 | `go/db/internal/kafka`（`key_ordered_consumer.go`、`retry_routing.go`、`store_failure.go`） | 进程级就绪闸接在 Start、重试循环、shared 认领、ConsumeClaim、owner 认领循环这几处；`route_not_ready` 延后原因 |
| G3 | `tools/merge_zone`（`capability_check.go`、`main.go`、`merge_run.go`、`placement_ops.go`、`placement_codec.go`、`unmerge.go`）、`dev_tools.ps1` | 能力标记机制：`-mode capability-check`、`-db-capability-zones`、`-MergeDbCapabilityZones` 及对应测试。§15.4 只写了 go/db 一侧 |
| G4 | `dev_tools.ps1`、`k8s_deploy.ps1` 及契约测试 | 六个 D1 开关的透传与 `dev_tools_route_switch_forwarding.tests.ps1`；`k8s_client_entry_contract` 的开关键；第六个开关 `-ConfirmRouteSwitchChange`（粘滞核对） |
| G5 | `currency_crash_window.ps1`、`stress_round19.ps1` | 清库脚本里的 `route_reconcile` 复位，以及对账 leader 探测 |
| G6 | go/db `metrics/pipeline.go`、`stress_summarize.ps1`、`test_stress_summarize.ps1` | `db_redis_pool_*{redis_client="placement"}`；`placement_lookup` / `placement_recheck` 两个步骤名要改成 SQL 步骤；`db_save_outbox_total` 的 `route_not_ready` 结局 |
| G7 | `deploy/k8s/player-placement-alerts.yaml`、`data-service-route-alerts.yaml`、`deploy/k8s/README.md` | route alerts 删 15 条、改 3 条，其中 WritesFrozen 依赖只有 leader 才导出的 `route_writable`。placement alerts 的 RecheckMoved 随落库后复核一起消失 |
| G8 | `tools/merge_zone`（`relocate.go`、`relocate_run.go`、`main.go`） | R2 原先成立的前提是「go/db 持锁直到复核完成」。§15.3 去掉复核后，这个前提改由 TiDB 行锁保证，R2 / `-relocate-lock-wait` / `redisOrderingLocks` 可能变成冗余（推断） |
| G9 | `.github/workflows/deploy-config-tests.yml`；go/db `durable_enqueue.go` | CI 路径过滤；一条镜像注释指向将被删除的同名函数 |
| G10 | scene_manager、login | 两者只经 data_service RPC 读，代码不用改，只改过时注释：§14 的 reconcile→ready 顺序、「同一段 Lua」、`merge:in_progress` 键 |
| G11 | `tools/scripts/stress_summarize.ps1`（D1 表与文件头说明）、`stress_snap.ps1` 注释、`test_stress_summarize.ps1` | D1 表的 `rr_cache` / `rr_sql` / `rr_sqlerr` 三列按 `data_service_route_read_total{result=cache\|sql\|not_found_sql\|sql_error}` 求和；`route_read_total` 改为只计 ok / not_found / error 后，三列改为 `rr_ok` / `rr_nf` / `rr_err`，夹具里的 `data_service_route_ready_verdict_total` 一并删，与 B1 同批。`go/data_service/internal/metrics/login_reads_test.go` 的 `TestLoginReadNamesMatchStressSummarize` 从脚本读 `$homeZoneReadResults` / `$accountDirectoryRpcs` 钉名；rr_* 的结局若也提成脚本变量，钉名用例同批加一项。L-18 观测批（PROGRESS 10-24）至今没有 Codex 验证记录（落码会话 10-30 补） |
| G12 | `go/db/internal/saveoutbox/{replay,service}.go`、`db.go` | 归属更正：§15.2 / §15.8 写的「由『Redis 物理水平扩展设计』会话的 saveoutbox 改造删掉这处映射读」已不成立。那项改造属于第 2c / 2d 批，已作废（PROGRESS 10-30），该会话 10-24 之后也没再运行。R2 会话的记录是「映射直读 TiDB 后 RouteGate 接缝同批删除，home_zone 的 GET 改读 TiDB，归新方向会话」。所以由 B2 做：删 RouteGate 接缝和 `db.go` 注入的 mapping 客户端，映射读的去向见 T10（落码会话 10-30 补） |

**落码前要定的技术点**

这些点由落码会话按 §11 原则自定，定后写回本节，不需要负责人拍板。

- **T1 go/db 的事务结构。** 现状有三处缺口：
  - `savePlayerRow` 的 `ExistsByPK` 和 `Insert` 在事务外，事务第一条语句是 `FindOneByPKForUpdate`。
  - 未围栏表的 Save 和读路径都不在事务里。
  - `dbOpHandler` 拿不到路由信息。

  proto2mysql v0.2.0 的 `RunInTransaction` 只交出绑了事务的 `*DB`，`tx` 字段不导出，也没有导出的原生查询方法，所以没法在同一事务里先发 `SELECT … FROM <全局库>.player_route … FOR UPDATE`。候选做法：
  - (a) 给 luyuan-cpp 分叉加一个事务内原生查询出口，需要发新版本；
  - (b) 在本仓内解决，不跨仓发版。

  无论选哪种，都要保持「路由行 → 玩家数据行」的锁序，INSERT 分支也要挪进事务。
- **T2 路由读错的重试预算。** 现在 `lookup_error` 会消耗重试预算，写任务 3 次后进 `budget_exhausted` 死信。按 §15.5 第 1 条，以及 §15.6「TiDB 不可用时存盘停、消息留在 Kafka」，SQL 路由读错应当按存储故障处理、豁免重试预算。
- **T3** `player_route.home_zone` 允许 NULL，stale 判定要写明这种行怎么算。
- **T4** barrierrestore 的广播围栏改用 SQL `zone_merge_fence`，或者写资格冻结本身。
- **T5** P7 路由恢复审核在 §15 后失去比对对象，删掉 `RouteRecoveryAudit` 的 Redis 部分，以及 `ReleaseReadiness` 里对应的审核类型。
- **T6（推断）** go/db 的 `Placement.RouteSchema` 可以直接取 writer 配置的 `AuthoritySchema`：路由表就在这个库里，`restore.go` 也要求两者相等。这样避免两个来源对不上。
- **T7** relocate R2 是否保留，见 G8。
- **T8 权限。** runtime 角色模板目前没有 `player_route`，Plan.tables 要加上。`FOR UPDATE` 需要什么权限取决于锁原语是 SELECT_ONLY 还是 LOCK_TOUCH，要用真 TiDB 确认。
- **T9 容量估算。** go/db 每个落点库池的每个事务都会读 `route_meta`、锁 `writer_slot`，所以 §15.3 第 1 步说的「无锁点查」经这些池走并不是无锁的。§15.6 的容量估算要把这部分算进去。
- **T10 saveoutbox 跨 zone 迁移前的 home_zone 读（见 G12）。** 两种做法：
  - (a) 把 `replay.go` 的 `GET player:zone:{id}` 换成全局库 `player_route` 点查，读错按 R2 已定的口径不计次；
  - (b) 按 [cluster 稿](./redis-player-store-cluster.md) §5.4 不再读映射，由消费端 go/db 的 stale 判定作权威。§5.4 是作废的 2c / 2d 分片设计的一部分，单独采用要重新核对补投索引的跨 zone 迁移语义。

  两种都依赖 §15.5 第 1 条的三态。按 §11「先做最小完整方案」，倾向 (a)。

**逐文件清单**

路径相对于仓库根目录。

- **data_service + placement**
  - 整删（非测试）：
    - `go/data_service/internal/routereconcile/{cache,reconciler}.go`
    - `internal/routing/publish_redis.go`
    - `internal/routetool/{rewarm,auditor,importer,redisread,repair}.go`：删之前先把 `importer.go` 的退出码常量和 `redisread.go` 的包注释挪走
    - `internal/store/route_reconcile.go`
    - `go/shared/placement/route_ready.go`
  - 整删（测试）：
    - routereconcile 包 4 份
    - `routing/{publish_redis_test,route_read_probe_test}.go`
    - `store/route_reconcile_{test,contract_test,integration_test}.go`
    - `svc/route_read_gate_test.go`
    - `metrics/route_enumerate_prime_test.go`
    - `routetool/{repair_test,rewarm_test,routetool_integration_test,routetool_test}.go`：先挪走 `TestRouteTiDBPrepareOutputAndExitCodes`
    - `placement/route_ready_test.go`
  - 改写（非测试）：
    - `data_service.go`、`etc/data_service.yaml`
    - `cmd/writer_authority/main.go`
    - `cmd/debugutil/{debugutil,bundle}.go`、`cmd/debug_fetch/bundle.go`、`cmd/debug_import/{main,bundle_import,target_identity}.go`
    - `config/{config,global_authority}.go`
    - `metrics/metrics.go`
    - `routetool/freeze.go`、`routetoolcli/cli.go`
    - `routing/{route_read,route_publish,router}.go`
    - `server/dataserviceserver.go`
    - `store/{route_store,route_reads,schema,account_role_sql_proof}.go`
    - `svc/{servicecontext,route_bootstrap}.go`
    - `placement/{route,placement}.go`
  - 只改注释：
    - `cmd/route_tool/main.go`、`cmd/debug_fetch/main.go`
    - `constants/error_codes.go`
    - `logic/{data_logic,persisted_player_database,asset_op_ledger_logic,player_inbox_ledger_logic}.go`
    - `routing/player_name_cache.go`
    - `store/{account_role_trade_store,route_tidb}.go`
    - `routetool/tidb_prepare.go`
  - 改写（测试）：
    - routing：`route_publish_test`、`route_read_test`、`route_read_abandon_test`、`router_test`、`write_lock_test`、`router_integration_test`
    - store：`route_store_*` 3 份、`route_reads_*` 3 份、`export_test`、`account_role_admission_tidb_test`
    - svc：`route_bootstrap_test`、`servicecontext_missing_stores_test`
    - config：`config_test`、`global_authority_test`
    - `metrics_test`
    - server：`dataserviceserver_test`、`data_rpc_route_test`、`home_zone_ctx_test`、`login_read_metrics_test`
    - logic：`data_logic_test`、`route_failure_test`、`rollback_merge_fence_test`、`player_fields_test`、`asset_op_ledger_logic_test`、`player_inbox_ledger_logic_test`、`rollback_recall_test`
    - `routetool/freeze_test`、`routetoolcli/cli_test`
    - debug 三个工具的测试
    - `placement/{route_test,placement_test}`
  - 保留不动：
    - `routing/router_lock_test.go`
    - `logic/rollback_logic.go`
    - `store/{route_bootstrap,account_role_initial_commit,mysql}.go`
    - `placement/role_route_sql.go`
    - `logic/player_name_logic_test.go`
  - 新增：
    - routetool 退出码与包注释的落点
    - `tidb_prepare` 用例
    - 内存版 RouteStore / RouteReader，共 2 个
    - 可选：SQL 读路径用例
- **go/db**
  - 整删：
    - `internal/kafka/route_ready_gate.go`
    - 测试 `route_ready_gate_test`、`route_ready_gate_shutdown_test`、`route_gate_open_test`、`route_ready_integration_test`
  - 改写：
    - `internal/kafka/{placement_route,key_ordered_consumer,player_row_fence,store_failure,retry_routing}.go`
    - `internal/config/config.go`：新增 `RouteSchema`
    - `internal/svc/servicecontext.go`
    - `db.go`
    - `internal/metrics/{metrics,pipeline}.go`
    - `internal/saveoutbox/{replay,service}.go`
    - `internal/drain/session.go`
    - `etc/db.yaml`
    - `internal/barrierrestore/{route,restore,scripts}.go`
    - `cmd/save_barrier_restore/main.go`
  - 只核对：`save_outbox_note.go`，确认 SQL 读错不判终态。
  - 只改注释：`durable_enqueue.go`
  - 改写（测试）：
    - kafka：`placement_route_test`、`key_ordered_consumer_test`、`run_db_op_test`、`l04_deferral_test`、`ordering_lease_test`、`consumer_shutdown_test`、`retry_routing_test`、`group_handoff_test`、`instrumentation_test`、`player_row_fence_mode_test`、`player_row_fence_integration_test`、`player_read_missing_integration_test`、`save_outbox_note_test`
    - `metrics_test`
    - config：`config_test`、`global_authority_test`
    - saveoutbox：`replay_route_gate_test`（只删闸用例）、`replay_test`、`replay_integration_test`、`replay_bounds_test`、`lifecycle_test`
    - barrierrestore：`route_test`、`route_integration_test`、`restore_test`、`restore_integration_test`、`home_fallback_test`、`drain_test`、`schema_gen_test`
    - `cmd/save_barrier_restore/{main_test,home_fallback_test}`
  - 新增：
    - SQL 路由读单测
    - 真 TiDB 用例：写事务锁读、版本冲突、冻结、跨库权限
    - 「读错不进 stale、不终结补投意图」回归
- **go/guild**
  - 改写：
    - `internal/logic/merge_fence.go`：改成 SQL 围栏
    - `internal/config/config.go`、`internal/svc/servicecontext.go`
    - `guild.go`、`etc/guild.yaml`
    - `internal/data/`：SQL 围栏读，可以复用 roleAuthority
  - 改日志和注释：
    - `internal/logic/{guild_logic,economy_logic,home_zone}.go`
    - `internal/constants/constants.go`
  - 测试：
    - `merge_fence_test`、`config_test`、`data/{route_authority_fixture_test,role_identity_tidb_test}`
    - 新增 SQL 围栏的真 TiDB 用例
- **tools/merge_zone**
  - 整删：
    - `route_publish.go`、`capability_check.go`
    - 测试 `route_publish_test`、`capability_check_test`、`manifest_route_source_test`（推断整删）
  - 改写：
    - `main.go`：`-route-db-dsn` 改必填，`-take-over-fence` 保留
    - `merge_run.go`、`fence.go`、`merged_into.go`
    - `placement_ops.go`、`placement_codec.go`、`pin_placement.go`、`backfill_home_zone.go`
    - `relocate_run.go`、`relocate.go`、`relocate_manifest.go`、`unmerge.go`
    - `storage_audit.go`、`player_blob_migrate.go`、`manifest.go`
    - `current_tidb.go`、`team_authority.go`、`route_store.go`
    - `audit_checks.go`、`audit_resources.go`
  - 只改注释：`preflight.go`、`trade_step.go`、`player_rows.go`
  - 测试：
    - 约 26 份要改，以 `*_db_test` / `*_integration_test` 为主
    - `route_mirror_test` 只删脚本比对项，镜像常量保留
- **tools/data_consistency_check**
  - `route_source.go`：删 redis 口径。
  - `main.go`：`-route-db-dsn` 改必填，`-redis-addr` 保留。
  - `route_source_test.go`：删 Redis 用例。
- **tools/scripts 与契约测试**
  - 脚本：
    - `dev_tools.ps1`：`-MergeZoneRouteDbDsn` 保留并改必填
    - `k8s_deploy.ps1`：新增 `Placement.RouteSchema` 的渲染
    - `k8s_route_tool.ps1`、`maintenance_drain.ps1`
    - `currency_crash_window.ps1`、`stress_round19.ps1`、`stress_summarize.ps1`
  - 测试：
    - `dev_tools_route_switch_forwarding.tests.ps1`：整删
    - `dev_tools_merge_zone_contract`、`k8s_deploy_contract`、`k8s_route_tool`、`k8s_client_entry_contract`、`maintenance_drain` 的 tests
    - `test_stress_summarize.ps1`
- **部署与 CI**
  - 改写：
    - `deploy/k8s/{data-service-route-alerts,player-placement-alerts}.yaml`、`deploy/k8s/README.md`
    - `deploy/sql/{authority_runtime_role,authority_operator_role}.sql`
    - `.github/workflows/deploy-config-tests.yml`
  - 只改注释：
    - `deploy/k8s/manifests/infra/{redis,redis-ha}.yaml`：`--enable-debug-command no` 保留
    - `manifests/go-svc/{data-service,data-service-route-tool}.yaml`
    - `deploy/k8s/{mysql-connection-alerts,scene-manager-alerts}.yaml`、`scene-manager-dashboard.json`
    - `deploy/login-stack.linux/login.yaml`、`deploy/docker-compose.yml`
- **写资格框架、proto、其余夹具**
  - `go/shared/authoritysql/{catalog,maintenance}.go`、`sqltest/fixture.go` 及相关测试
  - `go/shared/drainproof/{report,report_test}.go`
  - `proto/common/database/rollback_database_table.proto`，并重新生成
  - `go/chat/internal/authority/fixture_test.go`
  - `go/player_inbox/internal/inbox/{route_authority_integration_test,store_integration_test}.go`
  - Java 仓 3 处见 F8
- **工作区注意**（10-30 当时）：以下文件有别的会话的未提交改动，落码时只做路由相关的局部编辑：
  - data_service：`logic/data_logic.go`、`data_logic_test.go`、`data_logic_integration_test.go`，以及新增的 `asset_op_ledger_verified_test.go`
  - `tools/scripts/k8s_deploy.ps1`：match 线
  - `tools/merge_zone/blob_lock_integration_test.go`

**对 Codex 全球同服验收的影响（推断，按依赖推出）**

- **要重跑：**
  - 完整协议生成与 After 快照；
  - C++ 构建（生成的 pb 会变）与 29 项测试；
  - J 的 ContractSync 与全量构建；
  - Go 的 data_service、db、shared、merge_zone、chat、guild、player_inbox；
  - barrierrestore 专项；
  - maintenance_drain 合同，以及部署 / 排空合同里涉及授权的部分；
  - 外部 R14 fixture（migrate、render-roles、writer_authority）；
  - I3 对 `authoritysql/catalog.go` 的「权威 DDL」SHA 绑定；
  - 涉及路由表的真库项，以及 Main36 双区。
- **基本不受影响：**
  - schemamigrate：该模块禁止引用 shared / proto；
  - R 的 `authority_jdbc`：只读 `route_meta`；
  - 纯环境类工作：容器恢复、Raw、旧库存核账。

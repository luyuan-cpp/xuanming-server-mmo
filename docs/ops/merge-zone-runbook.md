# Merge-Zone Runbook(合服操作手册)

> **状态**: v3.2 — 2026-09-28(v3.2:`-MergeDbCapabilityZones` 去缺省 + `merge-zone-capability-check`,见修订历史)。按 [`player-storage-placement.md`](../design/player-storage-placement.md)(v2.2)同批落码的 `tools/merge_zone/` 与 `tools/scripts/dev_tools.ps1` 重写了步骤顺序、清单、验证与回滚,并新增落点运维(§14)。**这批代码落码时未编译、未测试**(AGENTS.md §10.1);2026-09-29 按用户指示跑过一轮(merge_zone 集成测试 83 条 PASS(67 个顶层 + 16 个子用例)、dev_tools 契约 16/16,PROGRESS.md「2026-09-28 玩家存储落点」条目的「运行验证」),但 `be6b4ab1d`(10-08)之后 `tools/merge_zone` 的 P4 预检与 Kafka 队列审计又改过、未编译。**首次按本版执行前,先确认 [placement-reverify-checklist-20261012.md](../handoff/placement-reverify-checklist-20261012.md) 已由 Codex 在当前 HEAD 上跑绿**。截至 2026-10-12,真实集群上 pin 合服、copy 合服、搬库都没有演练过(§12)。
> **范围**: K8s 部署形态下,把 source zone 合并进 target zone 的端到端 ops SOP,以及玩家存储落点的钉落点 / 搬库 / 退役审计。**逐字可执行**。
> **关联**:
> - 设计:[`player-storage-placement.md`](../design/player-storage-placement.md)(落点、pin / copy 模式、搬库;§12 为本批修的工具缺口)/ [`server_merge_design.md`](../design/server_merge_design.md)(历史设计,§5.5 回滚已作废)/ [`server-merge-gap-fixes.md`](../design/server-merge-gap-fixes.md)(还开着哪些口子)
> - 工具:`tools/merge_zone/`(独立 go module)+ `tools/scripts/dev_tools.ps1`
> - 备份:[`mysql-backup-pitr-runbook.md`](mysql-backup-pitr-runbook.md)
> - 灾难回滚(整 zone 回档,**不是**合服撤销):[`tools/scripts/k8s_zone_rollback.ps1`](../../tools/scripts/k8s_zone_rollback.ps1)
>
> **谁该读**: 运维 + 主程值班。**首次合服前必须读完 §2(库地图)、§3(围栏)、§4(回填前置)、§8 Step 2(排空)**。这几节里任何一条搞错,合服都会「报告成功但什么都没做」,或者把玩家最后一次存盘丢掉。

---

## 0. TL;DR — 十件不能搞错的事

1. **第一次合服之前,`-MergeBackfillZone` 必须对 source 和 target 各跑一遍**(§4)。存量玩家历史上没有 `player:zone:{id}` 映射,没有映射的玩家**不会被合过去**。工具的守卫会拒绝(源库任何一行没有映射即拒),但拒绝发生在维护窗口里就是浪费停服时间。
2. **Redis 库号不能猜**(§2)。mapping = **DB 0**,guild = DB 2,login / shared / scene = DB 0。friend 已不占 Redis 库。写错库不会报错,只会静默无效。
   - **(2026-10-23 更正,上句原文保留)** guild 已改为 DB 0(`go/guild/etc/guild.yaml` 的 `RedisClient.DB`;merge_zone `-redis-db` 与 dev_tools `-MergeRedisDB` 的缺省同步改为 0)。现在全部 Redis 句柄都是 DB 0。
3. **N 取自 T-0 zone-down 之后的第一次 dry-run**,不是 T-1。T-1 彩排时两个区还在线、还在建号,只能做与在线无关的检查(§7)。把 T-0 dry-run 打出的 `players_in_source=N` 用 `-MergeExpectedSrcPlayers N` 回传给正式跑;数字不一致工具直接拒绝。
4. **zone-down 之前先排空源区 db_task**(§8 Step 2):先停 scene,等 go/db 把最后一批存盘消费完,再删 namespace。先删 namespace,go/db 跟着没了,P3 / P4 门禁就过不去。
5. **清单(manifest)是唯一凭据**(§9)。它在第一个字节写出去**之前**落盘;续跑、撤销、`-VerifyMerged`、映射丢失后的重放都要它。dry-run 只写 `<path>.dryrun.json` 预览,**预览不能当清单用**。
6. **默认是 pin 模式**(§5.1):合服只改归属,玩家行留在源区库。**pin 合服之后源区库仍是这批玩家的真源,绝不能下线**。要退役源区库,先 `merge-zone-relocate` 把人搬走,再 `merge-zone-storage-audit` 确认无人指向(§14)。
7. **回滚的第一手段是 `merge-zone-unmerge -MergeManifestPath`**(§10),**不是**备份还原。`RollbackPlayer` 当前**不可用**(§10.2)。
8. **`k8s-zone-down` 会删掉整个 zone namespace**(`k8s_deploy.ps1::Remove-Zone` → `kubectl delete namespace mmorpg-zone-<name>`)。「保留 source namespace 7 天以备回滚」**做不到**;回滚靠清单 + infra 命名空间里的 MySQL / Redis 备份。
9. **合服窗口内,home 在 src 或 dst 的玩家从任何 zone 都进不了场**(scene_manager 回可重试码 21,A16)。窗口外 `scene_manager_enter_scene_rejected_total{reason="home_zone_merging"}` 持续非 0 = 围栏没清干净(§3)。
10. **映射来源必填,不推断**(§15):除 `merge-zone-capability-check` 外每个 merge-zone 命令都要带 `<ROUTE_ARGS>` —— data_service 已是 db_first 时 `-MergeZoneRouteSource db -MergeZoneRouteDbDsn $env:MERGE_ZONE_ROUTE_DB_DSN`,否则 `-MergeZoneRouteSource redis`。选错的一边会写错库;工具开写前核对库与 mapping Redis 是否成对,不成对即拒绝。P4 之后**禁用旧版 merge_zone 二进制**。

---

## 1. 资产清单(全部实地核对存在)

| 资源 | 在哪 | 用途 |
|---|---|---|
| `tools/merge_zone/`(独立 go module) | repo | 合服、撤销、审计、落点运维。`main.go` 顶部注释是步骤顺序的真源 |
| `tools/merge_zone/merge_run.go` | repo | 合服主流程(§5) |
| `tools/merge_zone/audit_checks.go` / `audit_resources.go` | repo | 合服**前**审计 + 合服**后** `-verify-merged` 断言 |
| `tools/merge_zone/unmerge.go` | repo | `-mode unmerge`:按清单逐对象撤销 |
| `tools/merge_zone/backfill_home_zone.go` / `merged_into.go` | repo | `-backfill-home-zone`:存量 `player:zone` 回填;`merge:merged_into:{src}` 标记挡住对已合走 zone 的回填 |
| `tools/merge_zone/pin_placement.go` / `relocate*.go` / `storage_audit.go` | repo | `-mode pin-placement` / `relocate` / `relocate-abort` / `storage-audit`(§14) |
| `tools/merge_zone/placement_codec.go` | repo | `go/shared/placement` 的镜像(键名、值编解码),两边用同一组测试向量 |
| `tools/scripts/dev_tools.ps1` | repo | ops 入口:`merge-zone` / `merge-zone-audit` / `merge-zone-unmerge` / `merge-zone-pin-placement` / `merge-zone-relocate` / `merge-zone-relocate-abort` / `merge-zone-storage-audit` |
| `go/db/cmd/migrate`(`-storage-id`) | repo | 预建落点库(含 Phase 2 全局库 `player_store_<id>_db`) |
| `deploy/k8s/manifests/infra/mysql-backup-cronjob.yaml` | k8s(**infra namespace**) | 每日 03:17 UTC 的 mysqldump + binlog 归档;手动触发见 §8 Step 3 |
| `tools/scripts/k8s_zone_rollback.ps1` | repo | 整 zone PITR 回档(与合服撤销**不是**一回事) |
| `dev-robot-zones` | dev_tools.ps1 | 合服后烟雾测试入口 |

> **不存在的东西,别去找**:
> - `tools/scripts/merge_zone.ps1`:从来没有过,dev_tools.ps1 直接在 `tools/merge_zone/` 里编译并执行工具(退出码透传,§7.1);
> - 网关的 `POST /admin/maintenance`:真接口见 §8 Step 1;
> - per-zone 的 `mysql-backup` CronJob:只在 infra namespace 有一份;
> - topic `db_task_topic`:真名见 §2.3;
> - 合服 / 搬库清单的「重放工具」:映射丢失时按 §9.2 手工重放;
> - 单玩家回档:`RollbackPlayer` 在生产未通电(§10.2)。

---

## 2. 权威库地图(2026-09-28 核对)

合服跨了多个 Redis 逻辑库、多类 MySQL 库和 Kafka topic。**库号猜错不报错,只静默无效**,这是合服最危险的失败形态。

### 2.1 Redis

| 逻辑库 | DB | 键 | 配置来源 | dev_tools.ps1 参数 |
|---|---|---|---|---|
| **mapping** | **0** | `player:zone:{id}` / `player:placement:{id}` / `lock:player:{id}` / `merge:in_progress:{zone}` / `merge:merged_into:{src}` / `db:capability:zone:{z}` | `go/data_service/etc/data_service.yaml` → `MappingRedis`;go/db 的 `Placement.Redis`(缺省复用 `RedisClient`)必须指向同一实例的**主节点** | `-MergeMappingRedisAddr` / `-MergeMappingRedisDB`(默认 `-1` = 自动,兜底 0) |
| **guild** | 2 | `guild_rank:zone:{z}` / `guild:v2:{id}` / `guild:v2:cache_generation:{id}` / `guild_rank:maintenance_lock` | `go/guild/etc/guild.yaml` → `RedisClient.DB` | `-MergeRedisAddr` / `-MergeRedisDB` |
| **login / shared** | 0 | `player_merge_notice:{pid}` / `player_force_rename:{pid}` / `player:session:{pid}` / `kafka:{retry,processing,dead}:queue:*` / `distributed:lock:kafka:ordering:*`(go/db 排序锁)/ `team:player:*` / `team:rec:*` / `PlayerAllData:{pid}` / `{MsgType}:{pid}` | `go/login/etc/login.yaml` → `Node.RedisClient.DB`;`go/db` / `go/player_locator` / match(组队)同为 0 | `-MergeNoticeRedisAddr` / `-MergeNoticeRedisDB` |
| **scene_manager** | 0 | `scene_nodes:zone:{z}:load` / `player:{id}:location` / `scene:*:zone` / `world_channels:*` / `world_channel_lineno:*` / `world_channel_directory:*` / `node:zone:{z}:*` | `scene_manager_service.yaml` → `Redis` | `-MergeSceneRedisAddr` / `-MergeSceneRedisDB` |
| **player data** | 独立 | `player:{id}:*` | data_service 按 region 分的 Redis Cluster(**按 home_zone 选集群**) | `-MergeSourceDataRedis` / `-MergeTargetDataRedis`(仅多集群部署) |

> **(2026-10-23 更正,上表原文保留)** guild 一行的 DB 已由 2 改为 **0**(`guild.yaml`、merge_zone `-redis-db` 缺省、dev_tools `-MergeRedisDB` 缺省同批改);上表现在每个逻辑库都是 DB 0。

> ⚠️ **mapping 恒为 DB 0,没有例外。** data_service 用 go-zero 的 `redis.MustNewRedis(config.MappingRedis)`,而 go-zero `RedisConf` 没有 `DB` 字段,yaml 里写的 `DB` 会被静默忽略。历史文档与脚本里的「DB 15」一直是错的,已全部清理:
> - `data_service.yaml` 那行删了;
> - K8s data-service ConfigMap 的 `DB: 15` 删了;
> - `dev_tools.ps1` 的注释改了,并由 `dev_tools_merge_zone_contract.tests.ps1` 钉住。
>
> **K8s 上 go/db 读落点记录的 Redis**:go-svc-db ConfigMap 不写 `Placement` 段,go/db 用 `RedisClient` 读落点记录。它与 data-service 的 MappingRedis 同实例(`redis.<infra-ns>:6379`)、同为 DB 0,由 `k8s_deploy_contract.tests.ps1` 钉住。**以后把 mapping Redis 拆到别的实例,必须同时给 go/db 显式写 `Placement.Redis`**,否则 go/db 按错的记录选库。
>
> friend 的 Redis 逻辑库(原 DB 3)与 `-MergeFriendRedisAddr` / `-MergeFriendRedisDB` 已于 2026-09-18 删除:唯一的键 `friend:online:{pid}` 全仓没有写者,在线门禁只看 `player:session`。旧脚本继续传这两个参数会让 merge_zone 以「flag provided but not defined」直接退出。这是刻意的,静默忽略会让人以为自己配的库真的被查过。

### 2.2 MySQL

| 库 | 内容 | 谁读 |
|---|---|---|
| `zone_<N>_db` | `player_database` 等**玩家主数据表**(表清单由 `generated/data/mysql_database_table_list.json` 发现) | go/db(按玩家的**有效落点**选库,不再只读本 zone 库) |
| `player_store_<id>_db`(id ≥ 1000000) | 与 zone 库同结构的落点库(Phase 2 全局库默认 `player_store_1000000_db`) | go/db(按需打开) |
| 全局库(DSN 里的 schema,默认 `mmorpg`) | data_service 的全局表(`player_name` 等) | data_service |
| `mmorpg_friend`(`-friend-schema`) | `friend` / `friend_request` | go/friend;合服只读审计 |
| `mmorpg_guild`(`-guild-schema`) | `guild` / `guild_member` / `guild_player_state` / `guild_application` | go/guild |
| `mmorpg_trade`(`-trade-schema`) | `trade_listing` | go/trade |
| `mmorpg_player_inbox` | `player_inbox_stream` / `player_inbox_item` / `player_inbox_audit`(按 player_id 存,无 zone 列) | go/player_inbox;合服只读审计(PENDING 量级),不搬不改 |

- **有效落点** = `player:placement:{id}` 记录的库 ?? home_zone 的 zone 库 ?? 处理任务的 go/db 所在 zone 库(设计 §1)。没有记录的玩家与以前完全一样;pin 合服、钉落点、搬库才写记录。
- 一个 `-MergeMySqlDsn` 必须**同时**够得着上面这些库:工具按「库名.表名」访问帮会 / 聚宝斋 / 好友的表,并写 `zone_<src>_db.x → zone_<dst>_db.x` 的全限定名。所有 zone 库与落点库也必须和各 zone go/db 的 `Database.Hosts` 在**同一个 MySQL 实例 / TiDB 集群**,因为 go/db 会按需打开别区的库。
- 帮会 / 聚宝斋的库或表不在,合服**在任何写之前拒绝**,报错会点名迁移命令与对应的跳过开关,不会静默漏搬。
- **K8s 上没有部署 guild**:`deploy/k8s/manifests/go-svc/` 没有 guild 的 manifest / ConfigMap(见 `deploy/k8s/README.md`),`mmorpg_guild` 通常不存在。这类环境里:
  - `merge-zone` / `merge-zone-unmerge` / `merge-zone-audit` 三处都要加 `-MergeSkipGuildMySql -MergeSkipGuildRank`,两个一起给,三处口径一致。只给一个不算「跳过帮会」:合服 P1 仍会去查帮会表;审计的 `guild_member` 与 `-VerifyMerged` 的 `verify:guild_zone` / `verify:guild_rank` 也不会报 SKIPPED,照常断言。
  - 两个一起给时,审计的 `guild_member` 与 `-VerifyMerged` 的 `verify:guild_zone` / `verify:guild_rank` 三行都报 **SKIPPED(warn)**,不再是 INFRA(2026-09-28 修复,未经运行验证)。SKIPPED 表示「没查」,不是「通过」。
  - `go/guild` 的 `MergeMarkerRedis`(§12)在这类环境不适用。

### 2.3 Kafka

- 存盘 topic 是 **`db_task_zone_{zone}`**,世代号 ≥ 2 时带后缀 `_g{gen}`(`tools/merge_zone/preflight.go::dbTaskTopic`,镜像 `go/db/internal/config.DbTaskTopicForGeneration`;C++ 侧读 `bin/etc/base_deploy_config.yaml` 的 `DbTaskTopicGeneration`)。
- 世代号由三方共同决定,必须相等:C++ `DbTaskTopicGeneration`、`go/db/etc/db.yaml` 的 `ServerConfig.Kafka.TopicGeneration`、`go/login/etc/login.yaml` 的 `Kafka.TopicGeneration`。`start_game.ps1`、`k8s_deploy.ps1` 与契约测试会在不一致时拒绝。
- 消费组(2026-10-15 起按 zone 推导;已落码,未编译、未测试,待 Codex 验证;设计见 [db-consumer-group-per-zone.md](../design/db-consumer-group-per-zone.md) §7):
  - go/db 的存盘消费组正逐 zone 从共用的 legacy 组 `db_rpc_consumer_group` 交接到 `db_rpc_consumer_group_z{zone}`。P3 的候选组是按 zone 推导的 `db_rpc_consumer_group_z{zone}`
    (合服取 src,撤销取 dst;`tools/merge_zone/preflight.go::dbConsumerGroupForZone`,基名 `defaultKafkaGroup` 与 `go/db/etc/db.yaml` 的 `ServerConfig.Kafka.GroupID` 相同),
    加上 legacy 组(`-kafka-legacy-group`,缺省 `db_rpc_consumer_group`,传空值则只看 zone 组)。
  - 门禁只认在本 topic 上至少有一行数值 CURRENT-OFFSET 的**唯一权威组**,只对它算 LAG(`resolveAuthoritativeLag`)。两组都有位点就拒绝(`more than one candidate group holds committed offsets on topic …`),
    先用 `tools/scripts/kafka_offset_reset.ps1 -FinalizeHandoff` 收尾;两组都没有位点(`no candidate group holds committed offsets on topic …`)或 CLI 读不出,同样拒绝。
    不取「各组 LAG 最小值」:冻结在高位的旧组 LAG 可以是 0,会替真正在消费的组担保(fail-open)。
  - 〔2026-10-17 补,R7 复核 scripts [3] / [8];已落码,未编译、未测试,待 Codex 验证〕
    - 权威组的 LAG 合计为 0 之后,工具再用 `kafka-get-offsets --time -2 / -1` 取 topic 每个分区的日志边界,逐分区证明:每个非空分区(earliest ≠ latest)都有数值 CURRENT-OFFSET 的行,且该分区 LAG = 0;
      earliest 与 latest 的分区集合要相同,describe 里出现的分区 get-offsets 里都要有(`tools/merge_zone/preflight.go::verifyAuthoritativeCoverage`)。不过时报 `cannot prove topic <T> is drained (<n> problem(s)): …`。
      起因:源区 db 停了、权威组是 Empty 时,describe 只列出提交过位点的分区,从没提交过却已有消息的分区不出现,只看合计会放行。
    - 「组不存在」文案只在 describe 没有表头时才认定组不存在;文案与本 topic 的数值位点同时出现,按矛盾输出拒绝(`groupMissingInDescribe`)。
  - `-kafka-group` 显式给值时只看这一个组。本机单 zone 默认启动是裸名 `db_rpc_consumer_group`,`-Zone N` 启动是 `db_rpc_consumer_group_zN`,两种都在候选里;
    本机混用过两种启动方式、两组都有位点时,用 `-MergeKafkaGroup` 显式指定实际在跑的组。
- `dev_tools.ps1` 从 `go/db/etc/db.yaml` 现读 TopicGeneration 并转发 `-kafka-topic-generation`(`-MergeKafkaTopicGeneration` 覆盖)。**组名不再转发**(2026-10-15,`Get-MergeZoneArgs`):
  由工具按 zone 推导并认定唯一权威组;`-MergeKafkaGroup` 只用于显式覆盖,非空才转成 `-kafka-group`。`dev_tools.ps1` 不转发 `-kafka-legacy-group`。
  以前「必须绕过 ps1 手敲 `go run`」的变通已经不需要了。
- 注意:`dev_tools.ps1` 里 `-RollbackKafkaTopic` 的默认值仍是旧名 `db_task_topic`,但那是 `k8s-zone-rollback` 的参数,与合服无关。
  > 2026-10-16 更正:`-RollbackKafkaTopic` 缺省已是空串(`dev_tools.ps1` 参数块),由 `k8s_zone_rollback.ps1` 按 `-ZoneId` 推导为 `db_task_zone_<id>`。
  > 该脚本推不出世代后缀:`go/db/etc/db.yaml` 的 `ServerConfig.Kafka.TopicGeneration` > 1 时真实名字是 `db_task_zone_<id>_g<gen>`,必须显式传 `-RollbackKafkaTopic`(脚本不读、也不校验世代号;漏传时 Step 5 重置的不是 go/db 实际消费的 topic)。
  > 2026-10-17 再更正(R7 复核 scripts [6];已落码,未编译、未测试,待 Codex 验证):上一条的第二行已过时。`k8s_zone_rollback.ps1` 入口现在读 `ServerConfig.Kafka.TopicGeneration`(必须是非负整数),
  > 用 lib 的 `Get-DbTaskTopicName` 推导出带世代后缀的名字;`-RollbackKafkaTopic` 留空即可,显式给了却与推导值不同会在入口拒绝。详见 [zone_data_rollback.md](../design/zone_data_rollback.md)「2026-10-15 修订」。

---

## 3. 围栏(fence)生命周期 — `merge:in_progress:{zone}`

合服(和撤销)跑的这段时间里,别人不许往被合的两个 zone 里塞新对象,这两个 zone 的玩家也不许进场。围栏就是那面旗。

| 项 | 内容 |
|---|---|
| **键** | `merge:in_progress:{zone_id}`(十进制,无 hash tag) |
| **在哪** | **mapping Redis(DB 0)**,必须和它守护的 `player:zone:*` 同库 |
| **打几把** | **两把**:source 与 target 各一。目标区在窗口里同样不能进新对象 |
| **值** | JSON(`tool` / `run_id` / `source_zone` / `target_zone` / `started_at` / `expires_at_unix_ms` / `operator=host/pid` / `manifest_path`)。**读者从不解析**,纯排障用 |
| **判据** | **键存在即封锁**。不看值、不看剩余 TTL。Redis 报错一律按「封锁」处理(fail-closed) |
| **谁写** | 只有 `tools/merge_zone`(merge 与 unmerge 两种模式都写)。`SETNX`:已经有别人的围栏就**拒绝启动**,并把对方的 JSON 打出来 |
| **什么时候写** | 合服在 P1 / R / C 之后、**收集玩家之前**立围栏(A2),所以收集与预检都在围栏之下 |
| **TTL** | `-timeout + 30m`,下限 1 小时。默认 `-timeout=2h` ⇒ **TTL 2h30m**。后台 goroutine 每 `ttl/3` 续期一次(redis 源;db 源的库围栏每 `min(ttl/3, 30s)` 续期一次,见 §15.4) |
| **谁清** | 分三种情况:<br>① **正常结束**:显式 DEL。Lua 先比对 `run_id` 再删,不会误删接手者的围栏。<br>② **首跑时,立围栏之后、清单落盘之前被拒绝**(守卫、P2~P7、公会重名、落点扫描、合走标记等):本次什么都没写,工具**正常释放围栏**后 exit 1,日志 `this run wrote nothing and its merge fence was released`。修好原因后直接重跑。<br>③ **清单落盘之后失败**(步骤 1~6、清单写盘),以及**续跑时在清单落盘之前被拒绝**(见下方说明):**围栏保留**在本次 run_id 上,文案带 `LEFT IN PLACE`,并打印续跑指引:确认没有存活的 merge_zone 进程 → `GET merge:in_progress:<SRC>` 核对 run_id 与中止日志一致 → `DEL merge:in_progress:<SRC> merge:in_progress:<DST>` → 立即用原命令(同一个 `-MergeManifestPath`)重跑。<br>**以上 GET / DEL 只适用于 `-MergeZoneRouteSource redis`**。db 源(§15)下围栏的真源是全局库 `zone_merge_fence`,`merge:in_progress:*` 只是镜像:DEL 镜像只会让 scene_manager / guild 提前放行,重跑照样被库拒绝(`merge fence is held by another run`)。db 源的 ③ 改为:确认没有存活进程 → 原命令加 `-MergeZoneTakeOverFence <中止日志里的 run_id>` 重跑,工具在一个库事务里把围栏接到新 run_id,两区一刻不解封(§15.4)。<br>TTL 是进程被 kill 时的兜底,不是正常清除手段 |
| **dry-run** | **不写**围栏,但会检查是否撞上别人的;撞上就拒绝 |

> **续跑与半撤销时的拒绝保留围栏**(2026-09-28 修复,未经运行验证):
> - **合服续跑**:清单已存在、且清单里步骤 7 `post_merge_flag` 还没标记完成时,上一次运行可能已写到一半。此时在「围栏之下、清单落盘之前」被拒绝(续跑校验 R2、守卫、P2~P7、公会重名、落点扫描、合走标记、清单写盘),工具**不释放围栏**,文案以「续跑在清单更新之前被拒」开头,带 `LEFT IN PLACE` 与本次 run_id。按 ③ 处理:修好原因 → 核对 run_id → DEL → 原命令重跑(db 源不 DEL:原命令加 `-MergeZoneTakeOverFence <本次 run_id>`,§15.4;db 源续跑在获取围栏之后镜像确认不了时同样保留围栏)。清单已标记步骤 7 完成时照常释放(上一次已跑完全程)。
> - **撤销重跑**:立围栏后先看清单玩家里是否已有人 `player:zone == src`(上一次撤销在 5' 之后中止,半撤销)。是则 P、S 段的拒绝同样保留围栏;读映射失败也按半撤销处理。偏保守的副作用:合服当初带了 `-skip-player-mapping`,或合服在步骤 5 之前停下后改跑撤销,清单玩家都还是 src,撤销的门禁拒绝也会保留围栏,照 ③ 处理即可。
> - 判断拿到的是哪一种:看最后几行有没有 `LEFT IN PLACE`。有 = 围栏还在,按 ③;没有、且有 `merge fence was released` = 按 ②。

**谁在看这面旗**(四个消费者,契约必须字字一致;其中 remap 自 2026-10-12 起停用,见表中那一行):

| 消费者 | 位置 | 行为 |
|---|---|---|
| data_service `RegisterPlayerZone` | `go/data_service/internal/routing/router.go` | 目标 zone 被围栏 ⇒ 拒绝建映射(`ErrZoneMergeInProgress`);查询报错也拒绝。写入脚本开头再原子复核一次围栏(提交点复核),命中时 `player:zone` 与 `player:placement` 都不写。`RouteStore.Mode=db_first` 时(映射持久化第 1a 批,`routing/route_publish.go`;所有档位默认仍是 redis),提交点复核由发布脚本的 `merge:in_progress:{home}`(KEYS[4])完成,命中时三键(`player:zone` / `player:placement` / `player:maprev`)零写入,但全局库 `player_route` 里已提交的行会留下(§9.3 的 `db_only_unpublished`) |
| data_service `GetPlayerHomeZone` → scene_manager `EnterScene` | `router.go::GetPlayerHomeZoneAndMergeFence` + `go/scene_manager/internal/logic/home_zone.go` | 玩家 home 被围栏 ⇒ 响应 `home_zone_merging=true`,scene_manager 回 `ErrHomeZoneMerging = 21`(可重试),在写 location / owner_epoch 之前拒绝。围栏读失败也按「合服中」处理。指标 `scene_manager_enter_scene_rejected_total{reason="home_zone_merging"}`(A16) |
| data_service `RemapHomeZoneForMerge` | `internal/server/dataserviceserver.go` | **已停用(2026-10-12,[提案](../design/player-route-mapping-durability-proposal.md) §4.3)**:不论 token、不论围栏,一律 `FailedPrecondition` + `ErrCodeNotImplemented`(16),零变更;Router 上的实现已删除,`ErrCodeMergeFenceMissing`(24)不再产生。改映射只走 `tools/merge_zone`。(停用前:要求 source 围栏必须在,另需 `x-admin-token`。) |
| guild `CreateGuild` | `go/guild/internal/logic/merge_fence.go` + `guild_logic.go::checkMergeFence` | 被围栏 ⇒ 拒绝建帮(`FailedPrecondition`);读不到围栏也拒绝(fail-closed)。**但 `guild.yaml` 的 `MergeMarkerRedis` 整段缺失 = 闸门不生效**,启动时只打一条 Info |

另外,搬库(§14.2)的冻结 CAS 与钉落点(§14.1)都会检查围栏,与合服互斥。

**开服前必查**:`merge-zone-audit -VerifyMerged` 的 `verify:merge_fence`。围栏没清干净,data_service 会永久拒绝建号,guild 会永久拒绝建帮,**这两个 zone 的玩家会永久进不了场**。
**手动清理**(仅当确认没有任何 merge_zone 进程存活;**只适用于 `-MergeZoneRouteSource redis`**):

```bash
redis-cli -h <mapping-redis> -n 0 GET merge:in_progress:<SRC>   # 先看是谁的、什么时候起的
redis-cli -h <mapping-redis> -n 0 DEL merge:in_progress:<SRC> merge:in_progress:<DST>
```

db 源(§15)下**不要手工 DEL 镜像,也不要手工 `UPDATE zone_merge_fence`**:库里的围栏才是真源,镜像由工具与对账 leader 按库维护(库里不生效的镜像 leader 会按完整值删掉)。先只读查库:`SELECT zone_id, run_id, expires_at_ms, fence_epoch, holder_json FROM zone_merge_fence WHERE zone_id IN (<SRC>, <DST>) AND shard = 63`,然后:要续跑 → 原命令加 `-MergeZoneTakeOverFence <run_id>`(§15.4);要撤掉这次合服 → `merge-zone-unmerge` 同一份清单、加 `-MergeZoneTakeOverFence <run_id>`(§10.1);两者都不做 → 等库时钟过了 `expires_at_ms` 自然解封(默认 TTL 2h30m),期间两个 zone 保持封锁。

---

## 4. 前置条件:`player:zone` 回填(**第一次合服前必做,两个 zone 都要**)

**背景**:生产在 2026-09-08 之前从未在建号时写 `player:zone:{id}`(login 的 `CreatePlayer` 已修复,但只对**新号**生效)。存量玩家没有映射 ⇒ 合服收集时**根本扫不到他们** ⇒ 合服后仍留在已下线的源区。

回填的真源是 `zone_<N>_db.player_database`:有行 = 这个玩家的归属 zone 就是 N。写入用 `SET NX`,**绝不覆盖**已有值(已有值可能是上一次合服的结果,覆盖回去等于把玩家送回坟场)。

```powershell
# source 与 target 各跑一遍;先 -DryRun 看 would_create,再去掉 -DryRun 落地
# <ROUTE_ARGS> = 映射来源,必填、没有缺省值(§15.1):db_first 环境写 -MergeZoneRouteSource db -MergeZoneRouteDbDsn $env:MERGE_ZONE_ROUTE_DB_DSN,
#                仍是 redis 模式的环境写 -MergeZoneRouteSource redis。下文所有 merge-zone 命令(capability-check 除外)同此。
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeBackfillZone <SRC_ID> <ROUTE_ARGS> -DryRun
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeBackfillZone <SRC_ID> <ROUTE_ARGS>
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeBackfillZone <DST_ID> <ROUTE_ARGS> -DryRun
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeBackfillZone <DST_ID> <ROUTE_ARGS>
```

期望输出:

```
=== Backfill player:zone for zone <N> from zone_<N>_db.player_database (dry-run=false, mapping=<addr> db=0) ===
=== Backfill done (migrated): rows_scanned=N would_create=K created=K already_mapped=M mismatched=0 ===
```

- `mismatched > 0` = 有玩家已经映射到**别的** zone,原样保留并打 WARN。对「已经被合出去过的玩家」这是正常的;其他情况要查清楚再继续。
- **target zone 也必须跑**:目标区玩家没有映射的话,合服后 data_service 一样路由不到他们。
- **已合走的 zone 不能回填**:合服步骤 5 改映射之前会写 `merge:merged_into:{src}`,回填看到它就拒绝(dry-run 同样拒绝),报错里带 `was merged away`。pin 合服后源区库的行仍是真源,copy 合服后是冷副本,两种情况下回填都会把人钉回已下线的源区。映射丢失的正确恢复是按清单重放(§9.2),不是回填。

**没跑回填会怎样**:`merge-zone` 的守卫 G 会拒绝执行,报文里直接点名。守卫做的是**集合比较**:源库任何一行完全没有 `player:zone` 映射即拒绝;映射指向别区的行算冷副本,不算。

```
zone_<S>_db.player_database has 1200 rows and 370 of them have NO player:zone mapping at all (first ids: [...]).
Those players would be left behind pointing at a dead zone.
Run `-backfill-home-zone -zone <S> -apply` first, then re-run this merge
```

守卫在围栏之下执行,首跑时拒绝会正常释放围栏(§3 ②);续跑时拒绝保留围栏(§3 ③)。`-MergeAllowEmptySource` 只在「真的要合一个零玩家的空区」时才加,**不要用它绕过这条守卫**。

### 4.4 角色名口径与 `profile_component` 列前置检查(帮会二期 B3a)

> 本节编号沿用代码里早已存在的「runbook §4.4」引用(`proto/common/component/player_comp.proto`、
> `tools/merge_zone/audit_resources.go`、`go/login/.../entergamelogic.go`),所以 §4 下没有 4.1–4.3。
> 设计出处:`docs/design/guild-phase2/03-names.md` §3.15a / §3.16。

**名字口径:全服唯一,合服不改名。**

- 角色名的真源是 data_service **全局库**的 `player_name` 表(`uk_player_name` 唯一键,login 建角时先 `ReservePlayerName` 占名再建角)。唯一性**不按 zone**,两个 zone 里不可能各有一个同名角色,所以合服**没有重名可撞**,也**不存在改名步骤**。
- 注册表在全局库,合服**不搬、不改、不释放**。名字的两份副本走的是两条不同的路,排障时别找错地方:
  - `player_database.profile_component` 里的副本在玩家的**有效落点库**。copy 模式随玩家行由步骤 1 带到目标库;pin 模式不搬,留在源区库(它仍是真源)。
  - 账号 blob 里的 `AccountSimplePlayer.name` **不在 zone 库**:它在 login 共享 Redis 的 `account:<acct>` 键里,按账号存放、不分 zone。合服工具**不读也不写**这个键(步骤 1 只搬带 `player_id` 列的表,缓存失效也只删 `<消息名>:<player_id>` 形状的键),其中的名字原样保留。
- 审计里对应的那一行是 `player.name (global)`,级别 info(§7.1)。它不查库:唯一性由写入时的唯一键保证,不是合服期能靠扫描发现的问题。
- `force_rename` 全链路(`force_rename_required` 字段 + `player_force_rename:{pid}` 键)**保持未启用**,合服工具不写它,原样保留。
- 名字孤儿(建角失败后名字仍被占住,login 指标 `login_create_player_name_orphan_total` + ERROR 日志 `[player-name] orphan reservation`)与合服无关:由运维按日志里的 player_id + 名字,带 `x-admin-token` 调 data_service 的 `ReleasePlayerName` 释放,**不要**挂在合服、回档或摘角色的流程上——释放不可逆。

**前置检查:源、目标两个 zone 库都必须已有 `profile_component` 列。** pin 模式同样要查:合服后目标区的 go/db 会按需打开源区库,按需打开有只读 schema 闸,缺列会让这批玩家的存盘一直延后重试。

go/db 的启动期 DDL 默认关闭(`go/db/etc/db.yaml` 的 `AutoMigrateSchema: false`),新列只会由 `cmd/migrate` 加上,而迁移是**逐 zone 库分别跑**的。T-7 就查,不要留到窗口里:

```powershell
# 每个 zone 库各查一次,期望恰好 1 行
#   SHOW COLUMNS FROM zone_<SRC>_db.player_database LIKE 'profile_component';
#   SHOW COLUMNS FROM zone_<DST>_db.player_database LIKE 'profile_component';

# 缺列就在 go/db 目录里对那个 zone 的配置跑迁移:先 plan 再 up
cd go/db
go run ./cmd/migrate -f <该 zone 的 db.yaml> -command plan   # 只允许出现 ALTER TABLE ... ADD COLUMN;出现其它语句先停下核对
go run ./cmd/migrate -f <该 zone 的 db.yaml> -command up
# 库名白名单拒绝时按 cmd/migrate/main.go 的注释设置 DB_ALLOWED_DATABASES
# 非 zone 落点库(player_store_<id>_db)用 -storage-id <id> 代替按 ZoneId 推导库名,见 §14.5
```

顺序是 migrate → 重启 go/db → 重启 scene。同一条检查也适用于之后每一个加在 `player_database` 上的新列(如资产通道的 `asset_op_ledger`、回合制战斗的 `settlement_ledger`、MySQL 行内围栏的 `fence_owner_epoch` / `fence_save_seq`(`player_database_1` 上也有这两列,一并查)、玩家收件箱的 `inbox_ledger`(20 号,2026-10-15 起);完整清单以 `docs/design/guild-phase2/90-consistency.md` G-03 为准)。

漏跑迁移的兜底(2026-09-28 起):启动期 DDL 关闭时,go/db 在 `InitDB` 里对本 zone 库做一次**只读**核对(`proto_sql.assertSchemaUpToDate`,复用 `internal/migrate` 的 LoadColumns / Drift,不下发任何 DDL)。
- **直接拒启的两种情况**,照提示对**同一份配置**跑上面的 `-command up` 后重启即可:
  - 缺 proto 声明的列,panic 信息形如 `库 zone_<id>_db 缺 N 个 proto 声明的列 ... : player_database.settlement_ledger`;
  - 缺表且台账 `schema_migrations` 里没有干净的基线(预建的**空库**就是这种),信息形如 `库 zone_<id>_db 缺 N 张 proto 声明的表 ...`。
- **只打错误日志、不阻断的情况**:
  - 基线已应用后才缺的表,up 建不出来,打 `SCHEMA-DRIFT: ... 缺表`,需人工建表;
  - 类型漂移 / 多余列 / 缺主键,同样打 `SCHEMA-DRIFT:`。本地已知的 `user_oauth.provider_id`、`user_phone.phone` 每次启动各打一行。
- 按需打开别的落点库时复用同一道只读闸;库不对或缺列会被拒,错误提示里会给出要加的 `-storage-id`。

`-command up` 的退出码别读错:
- **exit 4 = 迁移已成功执行,只是有 NEEDS-REVIEW 待人工过目**。上面那两条已知类型漂移在本地库上一直在,所以本地补列后几乎总是 4,照常重启 go/db;
- exit 1 = 迁移失败(含台账 dirty);
- exit 3 = 另一个进程正持有同库的迁移锁。
- 只有 1 和 3 不能重启。脚本里别写 `migrate up && 启动`,会把 4 当失败。

K8s 上 db Pod 因此 CrashLoop 时,集群里没有 db-migrate Job,db 镜像也不带源码和 Go 工具链,只能从工作站跑:
1. `kubectl port-forward -n mmorpg-infra svc/mysql 13306:3306`。
2. 把 `go/db/etc/db.yaml` 复制到仓库**外**,改 `ZoneId` 为该 zone、`Database.Hosts` 为 `127.0.0.1:13306`,账号密码填集群 MySQL 的(从 Secret 取,不进 git)。
3. 设 `DB_ALLOWED_DATABASES=zone_<id>_db`,在 go/db 目录跑 `go run ./cmd/migrate -f <那份副本> -command plan`,核对只有 `CREATE TABLE IF NOT EXISTS` / `ADD COLUMN`。
4. 再跑 `-command up`,然后让 Pod 重启。

**不要**用 `DB_AUTO_MIGRATE_SCHEMA=1` 绕过:proto2mysql 的 CreateOrUpdateTable 会顺手对类型漂移列执行无人批准的 MODIFY COLUMN。

**工具自己的兜底(copy 模式)**:
- 步骤 1 **按列名**拷贝(`INSERT INTO dst (列…) SELECT 列… FROM src`),不依赖两库列序一致。
- 两库**列名集合**不一致时,在**任何一张表开始写之前**以 `SCHEMA MISMATCH ... only_in_src=[...] only_in_dst=[...]` 中止,一行都不写。
- 撞号预检也在任何写之前对全部表做完。
- 这些检查发生在步骤 1,在线的 T-1 彩排走不到那里(§7.2),所以列检查要按上面的 `SHOW COLUMNS` 人工做。

---

## 5. 工具实际执行的步骤顺序

顺序本身是正确性的一部分(源码:`tools/merge_zone/main.go` 顶部 + `merge_run.go`):

| # | 阶段 | 做什么 | 失败处置 |
|---|---|---|---|
| P1 | preflight | `zone_<src>_db` / `zone_<dst>_db` 都存在;从表清单 JSON 发现玩家表;trade / guild 库表就绪(或显式跳过) | 拒绝,exit 1(还没有围栏) |
| R | resume | 读既有清单:dry-run 预览一律拒读;清单的 src / dst 必须与本次一致(拿错清单时不碰围栏就退出) | 同上 |
| C | capability | **首跑的 pin 模式**:`-db-capability-zones` 列出的每个 zone 都有 go/db 能力标记 `db:capability:zone:{z}=placement-routing-v1`。参数**没有缺省值**:pin 模式不给即在入口拒绝;写 `none` 表示「此刻没有别的 zone 在跑」,不查(标记是 90s 心跳,只证明「此刻在跑」,T-0 怎么填见 §5.1) | 同上 |
| F | fence | 给 src 与 dst 各打 `merge:in_progress:{zone}`(§3) | 拒绝 |
| R2 | resume check | **只在续跑时**:玩家行模式必须与清单一致;copy 模式下玩家表集合变了即拒绝;pin 模式核对能力标记。在围栏之下做,拒绝时保留围栏 | 见下方「F~M 之间的拒绝」 |
| 0 | collect | 扫一次 mapping 拿源区玩家 id(续跑从清单读,日志 `RESUME: ...`) | 首跑:释放围栏,exit 1;续跑:保留围栏(§3 ③) |
| G | guard | 0 个玩家 ⇒ 拒绝(除非 `-MergeAllowEmptySource`);源库有行却没有任何映射 ⇒ 拒绝;`-MergeExpectedSrcPlayers` 对不上 ⇒ 拒绝 | 同上 |
| P2 | preflight | etcd 场景节点注册表里源区**一条注册都没有**(正面证据,`-MergeEtcdEndpoints` 必填,经 etcd v3 JSON 网关线性一致读)**且** `scene_nodes:zone:{src}:load` 为空。负载集为空不能单独证明下线:负载键丢失 / 被淘汰时也读作 0(2026-10-23 起,`tools/merge_zone/scene_registry.go`)。撤销按目标区口径同样要它 | 同上 |
| P3 | preflight | `db_task_zone_{src}[_g{gen}]` 在**唯一权威组**上的 LAG = 0(2026-10-15 起):候选是 `db_rpc_consumer_group_z{src}` 加 legacy 组 `db_rpc_consumer_group`(或 `-kafka-group` 显式指定的一个),恰好一个组在本 topic 上有已提交位点才只对它算 LAG;两组都有 / 都没有 / CLI 读不出一律拒绝,见 §2.3。通过日志 `preflight P3 OK: authoritative group <g> lag=0 on <topic> [...]` | 同上 |
| P4 | preflight | `kafka:retry/processing/dead:queue:db_task_zone_{src}` 三个列表都空,且每个 db 实例各自的 `kafka:retry:processing:{topic}:{实例}` 也空,以及按分区的 ready 列表 `kafka:retry:queue:db_task_zone_{src}:p*` 都空(2026-10-15 起,经 SCAN MATCH 发现,topic 先做 glob 转义,任一页出错整体拒绝) | 同上 |
| P5 | preflight | 源区玩家没有在持的 `lock:player:{id}` | 同上 |
| P6 | preflight | 源区玩家没有 `player:session:{id}` | 同上 |
| P7 | preflight | 源区玩家**不在任何活队伍里**(`team:player:{id}` 的 tid 非 0 且 `team:rec:{tid}` 仍在)。队伍记录写着每个成员的 zone_id,合服不迁移 | 同上;处置见 §8 Step 1 |
| N | guild name | 公会重名断言(冲突时一个字节都不写) | 同上 |
| S | placement | 扫清单玩家的落点记录:有冻结(搬库中)或畸形记录即拒绝;已有记录记进清单 `placement_existing`;续跑时逐人比对;**copy 模式下清单里有记录者时同样要求 `-db-capability-zones`(可 `none`)并照 C 的口径核对** | 同上 |
| X | merged_into | 查 `merge:merged_into`:源区已合进别的 zone、目标区自己已被合走、标记读失败或读不懂 ⇒ 在第一次写之前拒绝(A10)。步骤 5 已完成或带 `-skip-player-mapping` 时不查 | 同上 |
| M | manifest | **在任何写之前**落盘清单(玩家 / 公会 / ZSET 成员 / 表 / 商品 / 模式 / 已有落点)。dry-run 只写 `<path>.dryrun.json`,到此为止 | 同上 |
| 1 | pin_placement(pin) | 清单里无落点记录的玩家,逐人 CAS 钉 `player:placement = "{src}:1"`。**不拷行、不拷 blob、不删缓存** | **保留围栏**中止(§3 ③) |
| 1 | player_rows(copy) | 只对**无落点记录**的玩家:`zone_src_db.<表> → zone_dst_db.<表>` 逐表**按列名**拷贝 + 共享 DB 0 上的玩家缓存失效(不含 `save_barrier`,2026-10-20 起保留,由 Step 5 的 `verify:save_barrier` 核对)。有记录者计入 `placed_skipped`。续跑时与源行逐列相同、且没有目标独有行的表视为已拷;目标行被改过或目标独有行按 ID SAFETY 拒绝 | 保留围栏中止 |
| 2 | player_blobs | 跨 data Redis 拷 `player:{id}:*`(仅 `-MergeMigratePlayerBlobs`,**仅 copy 模式**)。拷到的玩家数 ≠ 清单数 ⇒ 中止 | 保留围栏中止 |
| 3 | guild_mysql | `guild.zone_id` 按清单逐条改写(每个 id 一个显式短事务:`SELECT … FOR UPDATE` → 带 zone 守卫的 UPDATE → COMMIT)+ `guild:v2` 缓存失效 + 复查源区 | 保留围栏中止 |
| 3b | trade_mysql | 聚宝斋 `mmorpg_trade.trade_listing.market_zone` 由 src 改写为 dst,**只改清单里的 listing_id**,事务形状同步骤 3;`seller_zone_at_listing` 不改。实写后复查源区计数,仍 > 0 ⇒ **中止且不标完成**(清单落盘后又有商品进源区)。续跑:停上架入口 → 按 §3 ③ 核对并 DEL 围栏 → 原命令重跑(新商品并入清单续搬)。库 / 表 / 列缺失在 P1 即拒绝,只有显式 `-MergeSkipTradeMySql` 才跳过 | 保留围栏中止 |
| 4 | guild_rank | `guild_rank:zone` ZSET 合并:在 `guild_rank:maintenance_lock` 内**重读源榜**,以重读结果为准;写之前先把撤销依据落进清单,再 MULTI/EXEC(目标榜 `ZADD NX`,不覆盖 guild 服在步骤 3 之后写进目标榜的新分数)。重读为空(续跑时上一次已并走,或公会全部解散)时**不写**,只 DEL 源键空壳,清单快照不会被写回。清单详情 `members=N source_gone=bool` | 保留围栏中止 |
| 5 | player_mapping | 先写 `merge:merged_into:{src} = {"dst":…,"run_id":…}`(已有标记指向别的区即中止;X 段已先查过一次,这里是最后一道防线),再**按清单逐键 CAS** `player:zone` src→dst。要求「本次改成 + 已是 dst == 清单人数」;清单外仍指向源区的只打 WARN | 保留围栏中止 |
| 6 | hot_state | 清源区 scene_manager 热状态(仅 `-MergeClearSourceHotState`)。删场景键之前,先对清单外玩家补扫 `zone_id==0` 的 location(报告 `zone0_sweep(...)`) | 保留围栏中止 |
| 7 | post_merge_flag | `player_merge_notice:{pid}` 打进 **login Redis(DB 0)** | 只 WARN |

**F~M 之间的拒绝**(R2 / 0 / G / P2~P7 / N / S / X / M 写盘)统一按是否半合服分流(§3):
- 首跑(`-MergeManifestPath` 还不存在):释放围栏,exit 1,修好后直接重跑;
- 续跑(清单已存在,且步骤 7 未标记完成):保留围栏,按 §3 ③ 处理;
- 清单已标记步骤 7 完成:照常释放。

**顺序红线**:

- **1 必须在 5 之前**。mapping 一改,玩家就被路由到目标区。copy 模式下他的行此刻还在源库;pin 模式下他的落点记录还没钉上。
- **5 一旦跑完,源区玩家在 mapping 里就消失了**。重新扫描会得到空集合,这正是清单存在的理由(§9)。

**P3 的取证方式**:这条检查保护的是「玩家最后一次存盘有没有落库」,不能省。三选一:
- `-MergeKafkaConsumerGroupsCmd <kafka-consumer-groups.sh|.bat>` + `-MergeKafkaBootstrap <host:port>`:**生产用法,真查**;
  - 〔2026-10-17 补,已落码,未编译、未测试,待 Codex 验证〕执行机上还必须有**同目录、同扩展名**的 `kafka-get-offsets`(`…/kafka-consumer-groups.sh` → `…/kafka-get-offsets.sh`,`.bat` 同理;
    由 `tools/merge_zone/preflight.go::kafkaGetOffsetsCmdFor` 推导)。P3 在 LAG 合计为 0 之后用它做全覆盖检查(§2.3),推导不出或执行失败 P3 即拒绝。传的路径文件名里必须含 `kafka-consumer-groups`。
- `-MergeAssumeKafkaDrained`:运维显式声明「我用别的手段确认排空了」,日志里会留一条刺眼记录,**这条声明进事故复盘**;
- 两个都不给 ⇒ **拒绝执行**(默认绝不是「查不到就当 0」)。

撤销(`merge-zone-unmerge`)同样要其中一种(§10.1)。

### 5.1 pin 与 copy(`-MergePlayerRowsMode`)

| | **pin(默认)** | **copy(兼容旧 go/db)** |
|---|---|---|
| 玩家行 | 不拷,留在 `zone_<src>_db`,由落点记录 `"{src}:1"` 指过去 | 无落点记录者拷到 `zone_<dst>_db`;有记录者不拷 |
| 前置 | `-MergeDbCapabilityZones` 必填(无缺省,可 `none`),列出的 zone 都有能力标记(C 阶段) | 清单里有已钉落点的玩家时同样要求这个参数与能力标记(S 阶段) |
| data Redis blob | **拒绝 `-MergeMigratePlayerBlobs`**(见下) | 为全部清单玩家拷 blob(blob 跟 home 走) |
| 共享缓存 | 不删 | 无记录者删 |
| 源区库 | **仍是真源,不能下线**(退役见 §14) | 冷副本,可按备份口径归档 |
| 撤销 | 只改回 home,不动落点、不删行 | 1' 删目标库逐字节相同的行,跳过此刻有落点记录的玩家 |

- **能力标记**:go/db 新版启动时写 `db:capability:zone:{Z} = placement-routing-v1`,并每 30s 心跳续写(TTL 90s);进程退出不 DEL,由 TTL 收尾。有标记 = 最近 90s 内这个 zone 有新版 go/db 在续写,它会按落点选库,能处理被钉在别处的玩家。回退到旧版或停掉 go/db 后,标记在 90s 内自然消失。
- **`-MergeDbCapabilityZones` 没有缺省值**(2026-09-28 负责人决定,工具与 dev_tools 同步修改,未经运行验证):值是逗号分隔的 zone 号,或字面量 `none`(「此刻没有别的 zone 的 go/db 在跑,不检查」)。pin 合服、pin 模式清单的撤销不给就在入口拒绝(dev_tools 在 pin 合服时先点名拒绝,提示与工具一致);旧的 `src` / `dst` 记号已删除,给了直接报错。列出的 zone 要写**全部仍在跑的 zone**(例 `1,3,5`):合服后这批玩家可能以访客身份从任何 zone 登录,登录 zone 的 go/db 负责给他预加载读库。
- **T-0 怎么填**:
  - §8 Step 2 对 src、dst 都跑了 zone-down,两区 go/db 停掉 90s 后标记就消失。T-0 的 dry-run / apply **只列此刻 go/db 仍在跑的其他 zone**。列了 src 或 dst 工具照常检查,已下线就在 C 阶段被拒(`missing in [...]`),报文会说明「src/dst went down at T-0 ... list only the zones still running ... -mode capability-check」。copy 模式在清单里有已钉落点的玩家时,S 阶段用的也是这份列表,同样适用。
  - **除 src、dst 外没有别的 zone 在跑**(例如只有两个 zone 的环境):写 `-MergeDbCapabilityZones none`。工具不查任何标记,打一行 `preflight C: -db-capability-zones none — no capability marker checked ...`。这是运维声明,写进维护工单。
  - dst 的能力改在 §8 Step 6 zone-up 之后、Step 7 烟雾测试与开服**之前**,用 `merge-zone-capability-check -MergeDbCapabilityZones <DST_ID>` 核对,**必须 exit 0**。非 0 = dst 拉起的 go/db 不是新版(或没查成),**不许开服**,也不许跑机器人,先把 dst 的 go/db 换成新版。
  - src 在合服之后不再有 go/db,不需要它的标记。
  - T-1 彩排时各区都在跑,照常写全部在跑的 zone(含 src、dst)。
- **`merge-zone-capability-check`**(只读):逐 zone 报告 `present` / `missing` / `unreadable`。值不是 `placement-routing-v1` 算 missing 并打印实际值,读失败算 unreadable。退出码与审计同口径:全部 present 为 0,有 missing 为 1,有 unreadable 为 2(2 优先,表示没查成,不是通过)。不接受 `none`;`-db-capability-zones` 没给 / 写 `none` / 写错时工具打印 `ERROR: ...` 并按 2 退出(一个标记都没查,属于没查成,不与 missing 的 1 混用)。
- **go/db 刚发布或刚回退时不要动手**:发布完成(新 Pod 全部 Ready、旧 Pod 全部退出)之前,新旧 Pod 重叠,标记已经存在但证明不了所有副本都是新版;回退后要等满 90s 或手工 `DEL db:capability:zone:<Z>`,标记才不再替旧版作证。这两个窗口心跳管不住,只能靠这条纪律。
- **没升级 go/db、没有标记的环境,必须显式 `-MergePlayerRowsMode copy`**,否则在 C 阶段被拒,日志提示先升级重启 go/db。
- **写过落点记录之后禁止把 go/db 回退到旧版**:旧版按本进程 zone 选库,会把被钉到别处的玩家写错库。
- **多 data Redis 集群部署一律用 copy 模式**。data_service 按 home_zone 选 data Redis 集群,src 与 dst 不在同一集群时,pin 合服后 `player:{id}:*` 会读不到。所以工具在 pin 模式下拒绝 `-migrate-player-blobs`,除非同时给 `-skip-player-blob-migration`,ps1 不转发它,只在确认同集群时直接用 `go run`。这条已登记为待设计拍板。
- **pin 合服后,目标区 go/db 会按需打开 `zone_<src>_db`**:要求它与目标区 go/db 在同一 MySQL 实例,且 `Placement.AllowStoreFamilies` 为缺省的 true(或白名单里有 `zone_<src>_db`)。`§4.4` 的列检查对源区库同样适用。
- 续跑必须与首跑同模式:没有 `player_rows_mode` 字段的旧清单按 copy 处理,用默认 pin 续跑会被拒,提示加 `-MergePlayerRowsMode copy`。

---

## 6. T-7 天(预备期)

### 6.1 公告

- 客户端弹窗 + 官网公告 + 社区通知
- **必须包含**:精确合服时间(到分钟)、合服后的服名、预计停服时长,以及「**维护开始前请离开队伍**」(§8 Step 1 的 P7 门禁)
- 避开节假日 / 大版本窗口

### 6.2 容量评估

```bash
# 按有效落点库数玩家:没有落点记录的玩家就在 zone_<N>_db.player_database
kubectl exec -n mmorpg-infra deploy/mysql -- \
  mysql -uroot -p<pwd> -e "SELECT COUNT(*) FROM zone_<SRC>_db.player_database;"
kubectl exec -n mmorpg-infra deploy/mysql -- \
  mysql -uroot -p<pwd> -e "SELECT COUNT(*) FROM zone_<DST>_db.player_database;"
# 已有落点记录(做过搬库)的 zone,用 §14.4 的 storage-audit 看 effective 人数更准
```

判断标准:合并后总数 < target zone scene 单节点上限 × 节点数 × 0.7。**到 0.7 就先扩容 target zone**。

### 6.3 回填、列检查、能力标记 + 排期登记

- **跑 §4 的回填**(两个 zone)。放在 T-7 而不是窗口里,是因为它可能扫出一批「有行没映射」的存量玩家,需要时间核对。
- **做 §4.4 的列前置检查**(两个 zone 库都有 `profile_component`)。缺列要跑 go/db 迁移并重启 go/db、scene,同样不该留到窗口里。
- **pin 模式:确认全部在跑的 zone 都有能力标记**:
  ```powershell
  pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-capability-check -MergeDbCapabilityZones <全部在跑的 zone>   # 期望 exit 0
  ```
  需要看剩余 TTL 时再手查:
  ```bash
  redis-cli -h <mapping-redis> -n 0 GET db:capability:zone:<Z>   # 每个 zone 期望 placement-routing-v1
  redis-cli -h <mapping-redis> -n 0 TTL db:capability:zone:<Z>   # 剩余秒数,正常在 60~90 之间(心跳每 30s 续到 90)
  ```
  - 缺标记 = 那个 zone 此刻没有新版 go/db 在跑:还是旧版、已停掉,或写标记一直失败(go/db 日志 `[placement]` 前缀的 ERROR)。
  - Redis 故障恢复后,心跳在 30s 内自动补写,不需要重启 go/db。旧版要先升级。做不到就改用 copy 模式。
  - 同时核对各 zone go/db 的镜像版本,并确认发布已经完成(新 Pod 全部 Ready、旧 Pod 全部退出),理由见 §5.1 最后一条。
  - 这一步只证明 T-7 时刻的状态。T-0 还要按 §5.1 再核一次,口径不同(src、dst 已停)。
- **K8s 未部署 guild**:确认本次所有命令都带 `-MergeSkipGuildMySql -MergeSkipGuildRank`(§2.2)。
- 把窗口写进 `docs/ops/release-checklist.md`,确认不撞 release / hotfix,ops 主备与主程都在线。

---

## 7. T-1 天(彩排日)— 只做与在线无关的检查

T-1 时两个区都在线:有活节点、有会话、有队伍、还在建号。凡是依赖「已停服」的门禁在 T-1 **必然不过**,人数也还会变。所以 T-1 只用来发现**与在线无关**的问题,例如库表、列、回填完整性、能力标记、帮会重名、配置参数。**正式跑用的 N 一律取自 T-0 zone-down 之后的第一次 dry-run**(§8 Step 4.1)。

### 7.1 pre-merge 审计

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-audit `
  -MergeSourceZone <SRC_ID> -MergeTargetZone <DST_ID> `
  <ROUTE_ARGS>
# K8s 未部署 guild 再加 -MergeSkipGuildMySql -MergeSkipGuildRank
# <ROUTE_ARGS> 见 §4 / §15.1;db 源下枚举源区玩家读全局库 player_route,库与 mapping Redis 不成对即 INFRA(exit 2)
```

输出是一张 markdown 表:`sev | resource | src_count | dst_count | unique_scope | conflicts | notes`。

**退出码三个值意义不同**:

| exit | 含义 | 怎么办 |
|---|---|---|
| 0 | 干净,无 block | 继续 |
| 1 | 至少一条 block 级发现 | **不许合服**,先解决 |
| 2 | 审计**自己没跑成**(某个库连不上 ⇒ INFRA 级) | 结论**不可信**,不要当成 1 处理;修连通性后重跑 |

> 经 `dev_tools.ps1` 跑时,退出码**原样透传** merge_zone 的 0 / 1 / 2:`Invoke-MergeZoneGo` 先 `go build` 再直接执行产物(不用 `go run`,它会把非零码压成 1),最后显式 `exit`(否则 `pwsh -File` 正常结束恒为 0)。merge_zone 编译失败时工具一行都没跑,退出码为 2。契约测试 `tools/scripts/tests/dev_tools_merge_zone_contract.tests.ps1` 用 `pwsh -File` 子进程钉住这一点(未运行,待 Codex 验证)。

**当前跑的检查项**:

| 名称 | 判据 | 级别 | T-1 预期 |
|---|---|---|---|
| `source_scene_nodes` | etcd 注册表里源区没有场景节点,且 `scene_nodes:zone:{src}:load` 为空(与 P2 同一判据;没给 `-MergeEtcdEndpoints` 报 INFRA) | block | **必然 block**(在线),T-1 忽略 |
| `online_presence` | `player:session:{pid}`(**DB 0**)必须为 0 | block | **必然 block**,T-1 忽略 |
| `player_locks` | `lock:player:{id}`(mapping DB)+ `player:{id}:__lock`(data Redis,给了 `-MergeSourceDataRedis` 才查) | block | 可能 block,T-1 忽略 |
| `kafka_db_task_queues` | `kafka:retry/processing/dead:queue:db_task_zone_{src}` 三个都空,且每个 db 实例各自的 processing 列表也空,以及按分区的 ready 列表 `kafka:retry:queue:db_task_zone_{src}:p*`(2026-10-15 起,经 SCAN 发现)也空 | block | 在线时有也正常;**死信队列非空要在 T-1 查清** |
| `friend` / `friend_request` | 住独占库 `mmorpg_friend`,按 player_id 索引,合服后自然存活;只看量级。库 / 表查不到算 INFRA | info | 必须不是 INFRA |
| `guild_member` | 按 player_id 索引,合服后自然存活;只看量级。库 / 表查不成或孤儿查询失败算 **INFRA**;同时给 `-MergeSkipGuildMySql -MergeSkipGuildRank` 时报 **SKIPPED(warn)** | info | 必须不是 INFRA |
| `player.name (global)` | 角色名在 data_service 全局库 `player_name` 全服唯一,合服无冲突、不改名;不查库,只出说明行(§4.4) | info | — |
| `player_inbox` | 源区与目标区 zone 库都有 `player_database.inbox_ledger` 列(缺 = **block**,先对该 zone 跑 go/db `cmd/migrate up`,否则拷贝步骤 SCHEMA MISMATCH);再报 `mmorpg_player_inbox.player_inbox_item` 的 PENDING 量。收件箱库 / 表不在报 NOT VERIFIED(warn),其余查询失败算 INFRA。见 `docs/ops/player-inbox-runbook.md` | info | 必须不是 block / INFRA |

T-1 的通过口径:除了上表标「T-1 忽略」的在线类 block 之外,**没有 INFRA、没有别的 block**。完整的 exit 0 要等 T-0 停服之后(§8 Step 4.1 之前可再跑一次)。

### 7.2 dry-run 合服(预期停在 P2)

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone `
  -MergeSourceZone <SRC_ID> -MergeTargetZone <DST_ID> `
  -MergeDbCapabilityZones <全部在跑的 zone,例 1,2,3> `
  -MergeKafkaConsumerGroupsCmd <path\to\kafka-consumer-groups.bat> -MergeKafkaBootstrap <broker:9092> `
  -MergeEtcdEndpoints <http://etcd:2379[,...]> `
  <ROUTE_ARGS> `
  -DryRun
# copy 模式加 -MergePlayerRowsMode copy;K8s 未部署 guild 加 -MergeSkipGuildMySql -MergeSkipGuildRank
```

T-1 各区都在跑,能力标记列表可以含 src、dst;T-0 不行(§5.1)。pin 模式不给 `-MergeDbCapabilityZones` 会在入口就被拒(没有缺省值)。

T-1 能走完的是 **P1 → R → C → 0 → G**,在 **P2 被拒是预期的**(源区还有活节点)。dry-run 不写围栏,所以这次拒绝不会挡住在线的建号与进场。要确认的行:

```
=== Merge zone <SRC> → <DST> (dry-run=true apply=false run_id=...) ===
    player rows: mode=pin capability_zones=[...]
    redis: mapping=<addr>/db0 guild=<addr>/db2 login=<addr>/db0 scene=<addr>/db0
preflight P1 OK: zone_<SRC>_db and zone_<DST>_db both exist
preflight P1 OK: player tables in zone_<SRC>_db = [player_database ...]
preflight C OK: placement-routing-v1 confirmed in zones [...]        ← pin 模式
Mapping: M players with home_zone=<SRC> (<addr> db=0)
guard OK: all R rows of zone_<SRC>_db.player_database have a player:zone mapping (M map to home_zone=<SRC>)
preflight P2 (source zone <SRC>): zone still has live scene nodes: etcd still registers K scene node(s) of zone <SRC> — finish zone-down first   ← T-1 预期的拒绝
```

- 这里的 `M` **不要**记成 N:在线期间还会变。
- 在 P2 之前被拒的任何原因都要在 T-1 解决:P1 缺库缺表、C 缺能力标记、G 有行没映射。
- 公会重名(N)、落点扫描(S)、合走标记(X)、列对齐 / 撞号(步骤 1)在 T-1 走不到,由 §4.4 的人工列检查和 T-0 第一次 dry-run 覆盖。

### 7.3 演练备份恢复(强烈建议)

在 staging 跑一次完整 §8 + §10,确认备份能跑通、`merge-zone-unmerge` 能撤回、审计能出报告。
**截至 2026-09-28,真实集群的合服演练一次都没有跑过,pin 模式与搬库也没有**(见 §12)。首次生产合服之前应当补一次 staging 全流程,pin / copy 各一次。

---

## 8. T-0:维护窗口流程(建议 75 分钟)

### Step 1 [10 min] 公告 + 停止入口 + 踢人 + 解散队伍

网关的真实接口是 **`POST /admin/zones/{zoneId}/maintenance`**(`java/gateway_node/.../AdminZoneController.java`),鉴权头是 **`X-Admin-Key`**(不是 `Authorization: Bearer`),**一次一个 zone**:

```bash
# 把 source 与 target 都标成维护(manualStatus=1)
curl -X POST https://<gateway>/admin/zones/<SRC_ID>/maintenance \
  -H "X-Admin-Key: <admin-key>" -H "Content-Type: application/json" \
  -d '{"maintenanceMsg":"合服维护中,预计 06:00 恢复"}'
curl -X POST https://<gateway>/admin/zones/<DST_ID>/maintenance \
  -H "X-Admin-Key: <admin-key>" -H "Content-Type: application/json" \
  -d '{"maintenanceMsg":"合服维护中,预计 06:00 恢复"}'
```

等 5 分钟让在线玩家自然下线,再强踢剩余。

**组队门禁 P7 的前置**:队伍只存在共享 Redis(`team:player:*` / `team:rec:*`),记录里写着成员与队伍的 zone_id,合服不迁移。清单玩家里只要有人还在活队伍里,合服(和撤销)就在 P7 拒绝。
- 可行的解散手段只有两个:
  - **玩家自己**离队或由队长解散(`DisbandTeam` 只接受队长本人调用,没有管理员解散入口);
  - 等队伍 **24h 空闲过期**(`TeamIdleTTL`)。
- 所以离队要求必须写进 T-7 公告,并在踢人之前提醒一次。停服后才发现有队伍,只能等过期或改期。
- **不要手工 DEL `team:*`**:C++ scene 与组队服务靠它们的 epoch 判定成员关系。
- 查剩余人数:T-0 第一次 dry-run 在 P7 的拒绝文案会打出人数。

### Step 2 [15 min] 排空源区存盘,再 zone-down(source + target 都要)

**先排空、再删 namespace。** 玩家下线时 scene 发出的最后一次存盘在 `db_task_zone_<src>` 上,由**源区自己的** go/db 消费落库。`k8s-zone-down` 连 go/db 一起删,删早了这些存盘就留在 topic 里,T-0 的 P3 / P4 必然不过。
> 〔2026-10-17 补,R7 复核 scripts [3];已落码,未编译、未测试,待 Codex 验证〕「P3 必然不过」以前有一个漏洞:积压落在权威组**从没提交过位点**的分区上时(例如新 zone 或新世代 topic 刚收到第一批存盘,
> 或 db 卡在某分区首条消息上 CrashLoop 到 zone-down),Empty 组的 describe 根本不列出这个分区,其余分区 LAG 都是 0,合计为 0,P3 会放行,那批存盘就此留在源 topic、再没人消费。
> 现在 P3 在合计为 0 之后再用 `kafka-get-offsets` 逐个非空分区核对(§2.3),这句话才成立。执行机必须有与 `-MergeKafkaConsumerGroupsCmd` **同目录、同扩展名**的 `kafka-get-offsets`(§5「P3 的取证方式」)。

1. **停源区的 gate 与所有 scene**,go 服务(尤其 db)保持运行:
   ```bash
   kubectl get deploy -n mmorpg-zone-<src>                       # 以实际列出的名字为准(scene / scene-world / scene-instance / gate…)
   kubectl scale deploy -n mmorpg-zone-<src> <gate 与各 scene deploy> --replicas=0
   ```
2. **等 go/db 把 `db_task_zone_<src>` 消费完**:
   ```bash
   # 2026-10-15 起组名按 zone:切组收尾后是 db_rpc_consumer_group_z<src>。尚未切组的 zone(线上 go-svc-db-config 仍是
   # GroupID: "db_rpc_consumer_group")查 legacy 组;交接没收尾时两组都看,以 ConfigMap 的有效组为准(§2.3)。
   <kafka-consumer-groups.sh> --bootstrap-server <broker:9092> --describe --group db_rpc_consumer_group_z<src> \
     | grep db_task_zone_<src>                                   # 每个分区 LAG == 0
   redis-cli -h <shared-redis> -n 0 LLEN kafka:retry:queue:db_task_zone_<src>        # 下面每一个都 == 0
   redis-cli -h <shared-redis> -n 0 LLEN kafka:retry:processing:db_task_zone_<src>   # 旧版共享 processing(新版只回收不写入)
   redis-cli -h <shared-redis> -n 0 LLEN kafka:dead:queue:db_task_zone_<src>
   # 2026-10 起 db 可以多实例,每个实例有自己的 processing 列表。先列出登记过的实例名,再逐个查:
   redis-cli -h <shared-redis> -n 0 ZRANGE kafka:retry:instances:db_task_zone_<src> 0 -1
   redis-cli -h <shared-redis> -n 0 LLEN kafka:retry:processing:db_task_zone_<src>:<实例名>
   # 2026-10-15 起(④ 重试只交给分区属主)还有按原始分区拆开的 ready 列表 kafka:retry:queue:<topic>:p<N>。先 SCAN 列出,再逐个 LLEN == 0:
   redis-cli -h <shared-redis> -n 0 --scan --pattern 'kafka:retry:queue:db_task_zone_<src>:p*'
   redis-cli -h <shared-redis> -n 0 LLEN kafka:retry:queue:db_task_zone_<src>:p<N>
   ```
   - 〔2026-10-08 更正〕本节以前把第二个键写成了 `kafka:processing:queue:…`,那个键不存在,照着查恒为 0。
   - merge_zone 的 P4 门禁与 `kafka_db_task_queues` 审计已经会把每个实例的列表都查到(`dbTaskQueueKeys`)。
   - 〔2026-10-15 补,已落码,未编译、未测试,待 Codex 验证〕P4 与 `kafka_db_task_queues` 也已用 SCAN MATCH 查到按分区的 ready 列表 p*
     (`tools/merge_zone/preflight.go::dbRetryPartitionQueuePattern`,topic 先经 `escapeRedisGlob` 转义,`db_task_zone_1` 的模式不会扫到 `db_task_zone_11` / `db_task_zone_1_g2`;
     不按 p0..p{N-1} 枚举;SCAN 任一页出错整体拒绝)。P4 通过日志变为 `preflight P4 OK: kafka retry/processing/dead queues for <topic> are empty (<n> keys incl. per-instance processing lists and partitioned ready lists)`。
     键名与 go/db 侧 `go/db/internal/kafka/retry_routing.go::retryPartitionReadyKey` 一致。
   - 世代号 ≥ 2 的环境,topic 名带 `_g<gen>`。
   - **死信队列非空不会自己变空**:死信没有消费者。按其内容排查(常见是 `stale_topic` 或 `missing_required`,见 go/db 的 `db_placement_guard_total`),处理清楚再继续。
   - 冻结中的玩家(`frozen_deferred`)会让重试队列一直不空:合服前不应有进行中的搬库(§14.2)。
3. 两个 zone 各跑一次 zone-down:
   ```powershell
   pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName <SRC_NAME>
   pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName <DST_NAME>
   ```
   target 的 topic 不强制排空:target 会重新拉起,Kafka 的消费位点仍在。但撤销(§10.1)要求 target topic 排空,所以习惯上也等它清零。

> **这条命令会 `kubectl delete namespace mmorpg-zone-<name>`**(`k8s_deploy.ps1::Remove-Zone`),即整个 zone 的 gate / scene / go 服务全部销毁。
> **共享 infra 不受影响**:MySQL / Redis / Kafka / etcd 都在 `mmorpg-infra` namespace(`deploy/k8s/manifests/infra/`),merge_zone 工具要读写它们。
> **推论**:source zone 的 namespace 在这一步就没了,§8 Step 6 之后没有「保留 7 天」这回事。

**已经删了 namespace、P3 / P4 才发现不过怎么办**:临时拉起一个 **ZoneId=src 的 go/db** 把积压消费完,再停掉。**不要**为此 `k8s-zone-up` 源区,那会连 gate / scene 一起拉起。
- 用与集群同版本的 go/db,配置取 go-svc-db-config 的副本,只改 `ZoneId` 为 src。
- 它必须能直连 infra 的 MySQL、Redis(DB 0)、Kafka。Kafka 的 advertised listener 若只在集群内可达,就放在集群内的临时 Pod 里跑,不要从工作站 port-forward Kafka。
- 排空后停掉它,再跑一次 T-0 dry-run 确认 P3 / P4 通过。
- 此时这批玩家的 home 仍是 src(映射还没改),新版 go/db 会照常落库;新版会顺手写 `db:capability:zone:<src>`,停掉后 90s 内自然消失,无害。
- 这条路径**未在真实集群演练过**。

### Step 3 [5 min] 备份 MySQL + Redis

```bash
# MySQL:CronJob 在 infra namespace,只有一份,手动触发一次覆盖性备份
kubectl create job -n mmorpg-infra --from=cronjob/mysql-backup \
  mysql-backup-pre-merge-$(date +%s)
kubectl wait --for=condition=complete job/mysql-backup-pre-merge-<id> \
  -n mmorpg-infra --timeout=10m

# Redis:同步落 RDB(mapping / guild / login 通常是同一实例的不同 DB)
redis-cli -h <redis> SAVE
redis-cli -h <src-data-redis> SAVE     # 多集群部署才有
redis-cli -h <dst-data-redis> SAVE
```

**这一步失败,立刻终止合服。**

### Step 4 [15 min] 第一次 dry-run(取 N)+ apply

#### 4.1 第一次 dry-run(必做,N 从这里取)

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone `
  -MergeSourceZone <SRC_ID> -MergeTargetZone <DST_ID> `
  -MergeDbCapabilityZones <此刻 go/db 仍在跑的其他 zone,不含 SRC / DST;没有就写 none(§5.1)> `
  -MergeManifestPath <repo>\merge_<SRC>_to_<DST>.json `
  -MergeKafkaConsumerGroupsCmd <path> -MergeKafkaBootstrap <broker:9092> `
  -MergeEtcdEndpoints <http://etcd:2379[,...]> `
  <ROUTE_ARGS> `
  -DryRun
# copy 模式加 -MergePlayerRowsMode copy;K8s 未部署 guild 加 -MergeSkipGuildMySql -MergeSkipGuildRank
```

- 这一次必须走完 P1~P7、N、S、X,并在最后打出:
  ```
  DRY-RUN: nothing was written to MySQL or Redis (only the preview <path>.dryrun.json). Record players_in_source=N and pass it back as -expected-src-players on the T-0 run.
  ```
- **把 `players_in_source=N` 记进维护工单。**
- 预览写在 `<-MergeManifestPath>.dryrun.json`,只给人看,`-apply` / 撤销 / 验证一律拒读它。**`-MergeManifestPath` 本身此时并不存在**。
- 任何拒绝都按报错处理后重跑 dry-run。P7 拒绝见 Step 1;P3 / P4 拒绝见 Step 2 末尾。

> **升级提示**:旧版工具的 dry-run 直接写在 `-manifest-path` 上,新版认不出那种文件(没有 `dry_run` 字段),会把它当续跑清单。升级后**第一次 `-apply` 之前,先删掉旧版彩排留在同一路径上的文件**,或者换一个新的 `-MergeManifestPath`。

#### 4.2 apply

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone `
  -MergeSourceZone <SRC_ID> -MergeTargetZone <DST_ID> `
  -MergeExpectedSrcPlayers <N_FROM_4.1> `
  -MergeDbCapabilityZones <与 4.1 同一份:仍在跑的其他 zone,不含 SRC / DST;没有就写 none> `
  -MergeManifestPath <repo>\merge_<SRC>_to_<DST>.json `
  -MergeKafkaConsumerGroupsCmd <path> -MergeKafkaBootstrap <broker:9092> `
  -MergeEtcdEndpoints <http://etcd:2379[,...]> `
  <ROUTE_ARGS> `
  -MergeClearSourceHotState
# 不带 -DryRun 即 -apply(dev_tools.ps1 二选一转发,不存在「两个都不给」)
# <ROUTE_ARGS> 必须与 4.1 同一份;db 源下每批「库先提交 → 发布 + WAITAOF」,任一批确认不了即中止、退出非 0(§15.3)
# copy 模式加 -MergePlayerRowsMode copy;多 data Redis 集群部署只能 copy,并再加:
#   -MergeMigratePlayerBlobs -MergeSourceDataRedis <addrs> -MergeTargetDataRedis <addrs>
# K8s 未部署 guild 加 -MergeSkipGuildMySql -MergeSkipGuildRank
```

N 对不上时工具在 G 直接拒绝(首跑释放围栏,exit 1;续跑保留围栏,见 §3):

```
expected <N> source players (-expected-src-players, recorded at the T-1 rehearsal) but found <M>.
The source zone changed since the rehearsal — zone-down is not complete. Abort the merge
```

报文里的「T-1 rehearsal」是旧措辞,实际指 4.1 的那次 dry-run。先排查是否漏停了某个服务、是否还有 cron / robot 在写,**排查清楚再决策**。

**watch 这几行**(pin 模式;copy 模式把 Placement pin 一行换成 `Player rows (migrated): ...`):

```
preflight C OK: placement-routing-v1 confirmed in zones [...]      ← -MergeDbCapabilityZones none 时为 preflight C: -db-capability-zones none — no capability marker checked ...
merge fence acquired: merge:in_progress:<SRC> ttl=2h30m0s run_id=...
merge fence acquired: merge:in_progress:<DST> ttl=2h30m0s run_id=...
preflight P2 OK ... / P3 OK ... / P4 OK ... / P5 OK ... / P6 OK ... / P7 OK: none of the N players in scope is in a live team
placement scan OK: no frozen / malformed record among N manifest players; K already have a record (mode=pin)
Manifest: N players, G guilds, R rank members, tables=[...]
Manifest written to <path> BEFORE any write. Keep it: -mode unmerge and -verify-merged need it.
Placement pin (migrated): ... — player rows stay in zone_<SRC>_db, nothing is copied
MySQL: G guild rows migrated for zone <SRC> → <DST> (G guilds in manifest); G guild:v2 cache entries invalidated
MySQL: T trade_listing rows migrated (market_zone <SRC> → <DST>; seller_zone_at_listing untouched)
Redis rank: R ZSET members migrated guild_rank:zone <SRC> → <DST> (re-read under the maintenance lock; ZADD NX)
Mapping Redis: N manifest players migrated to home_zone=<DST>, 0 already there; 0 outside the manifest still at <SRC>  (<addr> db=0)
Scene hot state (migrated): locations(...) zone0_sweep(...) scenes(...) ...
Post-merge flags: N notice keys migrated to <addr> db=0, 0 force-rename keys
merge fence released: merge:in_progress:<SRC>
merge fence released: merge:in_progress:<DST>
=== Done (players_in_source=N ...) ===
Manifest: <path> — keep it: -verify-merged -manifest-path <path> (-expected-src-players N) and -mode unmerge need it
```

- `outside the manifest still at <SRC>` 大于 0:有人在清单之外仍映射到源区(紧前一行 WARN 会列样本),开服前 `verify:mapping_src` 会拦住。查清来源:通常是绕过围栏的写入。
- **把清单文件路径记进维护工单。** 任何 `ERROR` / 中止 → 先读中止文案:
  - 文案里有 `LEFT IN PLACE`:围栏保留(清单落盘之后的中止,或续跑时在清单更新之前被拒),按 §3 ③ 续跑或进 §10;
  - 文案里有 `this run wrote nothing and its merge fence was released`:首跑时清单落盘之前的拒绝,围栏已释放,修好后重跑。
- 源榜在步骤 4 时已为空(续跑时上一次已并走,或公会全部解散),那一行会是 `Redis rank: guild_rank:zone:<SRC> is already gone — the target rank is left as the guild service keeps it ...`:目标榜不动,只清源键空壳,正常。

### Step 5 [10 min] 合服后验证(`-VerifyMerged`,**必须带清单**)

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-audit `
  -MergeSourceZone <SRC_ID> -MergeTargetZone <DST_ID> `
  -MergeExpectedSrcPlayers <N> -VerifyMerged `
  -MergeManifestPath <repo>\merge_<SRC>_to_<DST>.json `
  <ROUTE_ARGS>
# K8s 未部署 guild 加 -MergeSkipGuildMySql -MergeSkipGuildRank(guild_zone / guild_rank 两行报 SKIPPED,见下)
# db 源下多一行 verify:route_cache(清单玩家缓存与库逐人一致),不过不许进 Step 6(§15.2)
```

没有 `-MergeManifestPath` 时 ps1 直接报错;清单缺失或是 dry-run 预览时,工具把相关断言报 INFRA(exit 2)。

| 断言 | 判据 | 不满足 |
|---|---|---|
| `verify:mapping_src` | mapping 里 `home_zone==src` 的玩家数 == 0(兜清单外的人) | block |
| `verify:manifest_mapping` | 清单里**每一个**玩家的 `player:zone == dst`;清单人数 ≠ `-MergeExpectedSrcPlayers` 时直接 block(多半拿错了清单) | block |
| `verify:manifest_rows` | 每个清单玩家在「落点记录所指的库,无记录则 `zone_<dst>_db`」的 `player_database` 里有行。pin 模式下被钉的人查源区库,copy 模式下无记录的人查目标库。清单没记玩家行那一步完成时报 warn `NOT VERIFIED`(不算通过):copy 看 `player_rows`,pin 看 `pin_placement` | block |
| `verify:save_barrier` | 清单里在 Redis 有存盘历史的玩家(owner_epoch≠0、owner 或屏障键畸形、或屏障里有 pending / ACK),按 login 冷加载(`savebarrier.Reconcile`)逐人预判:`player:{id}:save_barrier` 的 pending 合法且不超过 owner_epoch、两表 ACK 都等于 pending、有效落点库(记录 ?? `zone_<dst>_db`)两张表的行内围栏都等于 pending —— 全部成立即 V1 一致路径。玩家行那一步没记完成时报 warn `NOT VERIFIED`;读失败或时间预算用完报 INFRA | 会拒登的人 block;只会被 login 采纳 SQL 放行的人(`barrier_missing_sql_adoptable` / `sql_rows_ahead_of_barrier`)warn,不算通过。逐个结论的处置见 §10.1 末段 |
| `verify:guild_zone` | `SELECT COUNT(*) FROM mmorpg_guild.guild WHERE zone_id=src` == 0 | block(同时加 `-MergeSkipGuildMySql -MergeSkipGuildRank` 时为 warn `SKIPPED`,不算通过) |
| `verify:guild_rank` | `guild_rank:zone:{src}` 不存在;且 `ZCARD guild_rank:zone:{dst}` == `COUNT(guild WHERE zone_id=dst)` | block(同上) |
| `verify:trade_listing` | `SELECT COUNT(*) FROM mmorpg_trade.trade_listing WHERE market_zone=src` == 0 | block(加 `-MergeSkipTradeMySql` 时为 warn "NOT VERIFIED",不算通过) |
| `verify:source_hot_state` | 源区 scene_manager 键已清空 | warn(不影响数据正确性) |
| `verify:merge_fence` | `merge:in_progress:{src|dst}` **都不存在**;db 源下另要求库里 `zone_merge_fence` 的 src / dst 围栏都已释放(库是真源,镜像只是副本) | block |
| `verify:route_cache` | **只在 `-MergeZoneRouteSource db` 时出现**:清单里每一个玩家在 mapping Redis 里的 `player:maprev` 等于库里 `player_route.rev`,`player:zone` / `player:placement` 等于库值的编码(库里为 NULL 的缓存里也必须不存在);缓存 rev 比库新同样算不一致 | block(不一致不许开服,§15.2) |

db 源下 `verify:mapping_src` / `verify:manifest_mapping` / `verify:manifest_rows` 读的是全局库 `player_route`(真源),不是 mapping Redis;两者是否一致由 `verify:route_cache` 单独回答。

旧版的 `verify:target_zone_rows`(目标库总行数 >= N)已删除:总行数里有目标区原住民,漏拷几个照样过线。它由 `verify:manifest_rows` 逐 id 取代。

**任何 block → 进 §10。** exit 2(INFRA)不是「通过」,是「没查成」。
**K8s 未部署 guild**:同时给 `-MergeSkipGuildMySql -MergeSkipGuildRank` 时,`verify:guild_zone` 与 `verify:guild_rank` 报 warn `SKIPPED`,不再是 INFRA(2026-09-28 修复,未经运行验证)。只给一个开关时照常断言,帮会库不存在就是 INFRA。此后出现任何 INFRA 都一律当「没查成」,不再需要人工剔除这两行。

> 聚宝斋前置:合服与 `-VerifyMerged` 之前,trade 必须已对 `mmorpg_trade` 跑过 `trade -f etc/trade.yaml -migrate`(或 K8s `trade-migrate` Job Complete),否则前置检查 P1 / `verify:trade_listing` 会以缺表拒绝。合服窗口内 trade 的上架入口必须停(P3 起由合服围栏 `merge:in_progress:{zone}` 保证;P1 只有 dev 种子入口)。

### Step 6 [10 min] zone-up(只 up target)

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up `
  -ZoneName <DST_NAME> -ZoneId <DST_ID> -WaitReady
```

- **(2026-09-29 更正)上面的命令只适用于"dst 一直以默认参数部署"的情况**。部署参数不粘滞:`k8s-zone-up` 每次按本次参数重新生成 gate 等工作负载,
  不从集群读回旧值,而 dst 的 namespace 在 Step 2 已被删掉,`k8s_deploy.ps1` 的集群现状预检面对的是空 namespace,**拦不住漏传**。所以必须照 dst 上一次部署的记录补齐:
  - dst 以 `"0"` 回退运行(gate 直连):**必须补 `-GateRouterMode 0`**,否则静默落回 K8s 默认的 `"1"`(turn-based §22 D75);若回退原因(如路由服不可用)仍在,gate 卡在依赖门,登录 / 匹配全部 `no_target`。
    路由模式(`"1"`)的前置:infra 里 `client-rpc-router` 已部署就绪。
  - dst 以集群外入口运行:补 `-ClientEntryMode external`、`-GateServiceType`、`-GateNodePortBase`、`-ClientPublicHost` 或 `-GateClientHostTemplate`、`-GateExternalTrafficPolicy`
    (切 external 的窗口期还有 `-RequireClientEndpoint`);漏传会把 dst 重建成 podip,客户端拿到 PodIP,集群外玩家全部连不上且不报错。
  - dst 经 Ingress 对外:补 `-GatewayIngressHost` 与 `-GatewayTrustedProxies`(及 ClassName / TlsSecret);留空不会重建 Ingress。
  - staging / prod:补 `-ReleaseProfile`(`dev_tools.ps1` 默认 dev 档),并设好 `MMORPG_GATEWAY_ADMIN_API_KEY` 等注入密钥的环境变量;dev 依赖开发口令登录时补 `-LoginDevPasswordAuth`。staging / prod 还需要 `MMORPG_LOGIN_QUEUE_TOKEN_SECRET`(排队缺省开;见 `deploy/k8s/README.md`)。(2026-10-15 更正:登录排队合入门槛未清时各档排队都关,不要求该变量;见 `deploy/k8s/README.md`「登录排队的合入门槛」。)
  - 指定集群:`-KubeContext` / `-KubeConfig`。
  - 先加 `-DryRun` 跑一遍同一条命令核对渲染结果(DryRun 只本地渲染、不调 kubectl),再去掉 `-DryRun` 执行。参数口径见 `deploy/k8s/README.md` Optional Flags。
  以上补充未经运行验证,待 Codex / 下一次合服彩排(§7)核对。
- **核对 dst 的能力标记(pin 模式必做;copy 模式在 Step 4.2 的 `placement scan OK: ... K already have a record` 里 K > 0 时也必做)**。T-0 的 C 阶段没有查 dst(§5.1),zone-up 之后、Step 7 的机器人与 `/open` **之前**执行:
  ```powershell
  pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-capability-check -MergeDbCapabilityZones <DST_ID>
  ```
  **必须 exit 0**(输出 `zone <DST_ID>: present ...` 与 `capability-check: 1 present, 0 missing, 0 unreadable`),**否则不开服**:
  - exit 1(missing):输出里有 `zone <N>: missing ...`。dst 拉起的 go/db 不是新版,或写标记一直失败(看 `[placement]` 前缀的 ERROR)。**不许进入 Step 7**:旧版 go/db 会把被钉在源区库的玩家写进 `zone_<dst>_db`。先把 dst 的 go/db 换成新版,等它启动写上标记后重跑本命令。
    例外:`-MergeDbCapabilityZones` 留空时,ps1 在工具运行前就抛错(`merge-zone-capability-check requires -MergeDbCapabilityZones ...`),pwsh 同样 exit 1。这是参数没填,一个标记都没查,不要去换 go/db;补上 `<DST_ID>` 重跑。
  - exit 2(unreadable):没查成,不是通过。两种情况:输出里有 `zone <N>: unreadable ...`(mapping Redis 连不上等),修好连通性后重跑;或者输出是 `ERROR: ... -db-capability-zones ...`(参数写错,如写了 `none` / `dst` / 非数字),一个标记都没查,改对参数后重跑。
  - go/db 刚滚动发布完时,先确认发布已经完成(新 Pod 全部 Ready、旧 Pod 全部退出),理由见 §5.1。
- **pin 模式**:target 起来后,目标区 go/db 会在第一次处理这批玩家的任务时按需打开 `zone_<src>_db`,日志 `[placement] store opened on demand: storage=<src> db=zone_<src>_db admitted_by=family`,`db_placement_open_stores` 随之 +1。打不开时写任务会不耗预算地延后,`db_placement_store_open_total{result="error"|"rejected"}` 上涨。rejected 是放行配置问题,检查 `AllowStoreFamilies` 或白名单。
- source zone 不再 zone-up。它的 namespace 在 Step 2 已经删掉了,**回滚不依赖它**,依赖 §9 的清单与 Step 3 的备份。pin 模式下源区**库**仍在用,被删的只是源区的进程。

### Step 7 [5 min] 烟雾测试 + 开服

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command dev-robot-zones -Zones <DST_ID>
kubectl logs -n mmorpg-zone-<dst> deploy/scene-0 --tail=200 | grep -E "ERROR|FATAL"
```

人工验证:

- [ ] 选一个**原 source zone 的玩家**登录,确认进得去 target,背包 / 等级等数据完整
- [ ] `redis-cli -n 0 GET player:zone:<pid>` 返回 `<DST_ID>`
- [ ] pin 模式:`redis-cli -n 0 GET player:placement:<pid>` 返回 `<SRC_ID>:1`(合服前已有记录的保持原值)
- [ ] 该玩家的公会数据在(名字、等级、成员)
- [ ] 公会排行榜上能看到原 source 的公会
- [ ] 好友数据在
- [ ] `redis-cli -n 0 EXISTS merge:in_progress:<SRC>` / `<DST>` 都返回 0
- [ ] go/db 指标 `db_placement_guard_total{outcome="stale_topic"}` 为 0,`lookup_error` 不持续上涨
- [ ] pin 模式:`merge-zone-capability-check -MergeDbCapabilityZones <DST_ID>` 仍 exit 0(Step 6 已核对,`/open` 之前再确认一次)
- [ ] scene_manager 指标 `scene_manager_enter_scene_rejected_total{reason="home_zone_merging"}` 不再增长

开服(**Step 6 的 `merge-zone-capability-check` 没有 exit 0 时不许执行**):

```bash
curl -X POST https://<gateway>/admin/zones/<DST_ID>/open -H "X-Admin-Key: <admin-key>"
```

> source zone 保持维护态(或从服务器列表下架)。**不要**对 `<SRC_ID>` 调 `/open`。

---

## 9. 清单(manifest)、重跑与恢复

**清单是什么**:合服的写入面(落点记录或 zone 库玩家行 / guild MySQL / trade MySQL / guild_rank ZSET / mapping)分布在多个 MySQL 库和多个 Redis 库上,**没有任何跨面事务**。清单是「这次到底动了谁」的唯一结构化记录。

| 项 | 内容 |
|---|---|
| **落在哪** | `dev_tools.ps1` 强制算成**绝对路径**:不给 `-MergeManifestPath` 时是 `<repo>\merge_<src>_to_<dst>_<UTCts>.json`;给了相对路径按**调用者的当前目录**解析 |
| **什么时候写** | **第一次写之前**(阶段 M)。之后每完成一步 `markStep` + 落盘一次 |
| **怎么写** | 同目录 `.tmp` + rename(原子),进程被 kill 不会留下半个 JSON |
| **dry-run** | 只写 `<path>.dryrun.json`(内容带 `dry_run: true`),**不写 `<path>`**。续跑、`-mode unmerge`、`-verify-merged` 按后缀和内容两道拒读预览 |
| **内容** | `version` / `run_id`(与围栏里的同一个值)/ `source_zone` / `target_zone` / `operator` / `player_ids` / `guild_ids` / `rank_members`(成员 + 分数,是撤销 4' 的依据;步骤 4 以锁内重读结果回写,步骤 4 做完后不再改)/ `rank_members_unwritten`(为 true = 这份只是清单阶段的快照,还没交给过任何一次写;步骤 4 发现源榜已空时据此把撤销依据记为空,撤销不会把已解散的公会加回源区。旧清单没有这个字段,按「可能写过」处理)/ `trade_listing_ids` / `tables`(copy 模式)/ `player_rows_mode` / `placement_scanned` / `placement_existing`(合服前已有的落点记录)/ `steps` |
| **重跑** | 同一个 `-MergeManifestPath` 再跑一次:已完成的步骤按 `steps` 跳过,玩家 id **从清单读而不重新扫描**(日志 `RESUME: ...`)。必须与清单同模式;copy 模式下玩家表集合变了即拒绝;`placement_existing` 与现状不一致即拒绝。这些续跑校验在围栏之下做,拒绝时围栏保留(清单已标记步骤 7 完成的除外,§3 ③) |
| **不匹配** | 清单的 src/dst 与本次调用不一致 ⇒ 硬失败(复制粘贴错路径的保护);拿搬库清单(`kind=relocate`)当合服清单 ⇒ 拒读 |

> **为什么重跑不能重新扫描**:步骤 5 跑完后,源区玩家的 `player:zone` 已经是目标区,重新收集会返回**空集合**,第二次跑会「成功」地什么都不做,把「合了一半」当成「合完了」。

**清单丢了怎么办**:`-mode unmerge` 与 `-VerifyMerged` 都不可用,只能走备份还原(§10.3),映射丢失时也无从重放(§9.2)。所以清单必须跟着维护工单归档,**永久保留**,不按 90 天备份口径清理。

### 9.1 `merge:merged_into:{src}` 标记

| 项 | 内容 |
|---|---|
| **键 / 库** | `merge:merged_into:{src}`,mapping Redis DB 0,无 TTL |
| **值** | JSON `{"dst": <zone>, "run_id": "<清单 run_id>"}` |
| **谁写** | 合服步骤 5 改映射**之前**。已有标记指向别的 dst ⇒ 中止(保留围栏)。正常情况下走不到这里,X 段已先拒绝 |
| **谁读** | `-backfill-home-zone`:存在即拒绝(§4)。合服 X 段(清单落盘之前,§5):源区标记指向别的 zone、目标区自己有标记(已被合走)、标记读失败或读不懂 ⇒ 在第一次写之前拒绝(首跑释放围栏) |
| **谁删** | `-mode unmerge` 在 5' 之后删,只删 dst 与 run_id 都对得上的那一把;对不上只打 WARN 不删 |

### 9.2 映射丢失的恢复(按清单重放)

mapping Redis 整体丢失或被误删时(`player:zone:*`、`player:placement:*`、`merge:merged_into:*`、`db:capability:zone:*` 一起没了),**不要先跑回填**:没有 `merged_into` 标记挡着,回填会把已合走的玩家钉回源区。目前**没有重放工具**,按下面的规则手工执行,每一步都用 `redis-cli` 在 mapping Redis DB 0 上做。

0. **全服维护**,停掉所有 go/db(或确认 `Placement.Required=false`)。go/db 在映射缺失时会按 legacy 口径把写落进本进程 zone 库。
1. **收齐清单**:所有合服清单(不含已被 unmerge 撤销的)与所有搬库清单(`relocate_*.json`,**包括** `aborted` 的:放弃之前已切换的人仍然有效;dry-run 预览 `*.dryrun.json` 除外),按 `started_at_unix_ms` **从早到晚**排序。
2. **逐份重放**:
   - **合服清单**:
     - `SET merge:merged_into:<source_zone> '{"dst":<target_zone>,"run_id":"<run_id>"}'`;
     - 对 `player_ids` 里每个 id,`SET player:zone:<id> <target_zone>`;
     - 落点:`placement_existing` 里有这个人的,`SET player:placement:<id> <value>`;没有的,pin 模式(`player_rows_mode` 为 `pin`)`SET player:placement:<id> <source_zone>:1`,copy 模式(或字段缺失)不写。
   - **搬库清单**:对 `players[]` 中 `state` 为 `switched` 的人,`SET player:placement:<id> <final>`,并 `SET player:zone:<id> <home_zone>`。`unfrozen` 的人 `SET player:placement:<id> <final>`(解冻后的稳定值)。
   - 按时间顺序用**覆盖式 SET**,后发生的操作盖掉先发生的。
   - player_id 是大整数,生成命令时逐字拷贝清单里的数字,**抽查几行与清单原文逐字相同**。不得出现科学计数法或尾数截断。
3. **回填其余玩家**:只对**没有被合走**的 zone 跑 §4 的回填。回填用 `SET NX`,不会覆盖第 2 步的结果;已合走的 zone 此时已由第 2 步恢复了 `merged_into` 标记,回填会拒绝。
4. **重新钉落点**:如果 go/db 开了 `Placement.Required=true`,对每个 zone 跑一次 §14.1 的 `merge-zone-pin-placement`。批量钉落点写的 `"{N}:1"` 不在任何清单里,丢了就要重钉。
5. **启动全部 go/db**(第 0 步停掉的),能力标记由启动时的写入与 30s 心跳自动补上,不需要手工写;再按 §14.4 对每个在用的落点库跑 `storage-audit`,要求 `unmapped` 与 `unreadable` 都为 0。第 0 步没停 go/db 的环境,心跳会在 30s 内自己把 `db:capability:zone:*` 补回来,不需要重启。

**恢复不了的一类**:login `Placement.PinOnCreate=true` 且 `NewPlayerStorageId` 指向**非本 zone 库**(Phase 2 全局库)建出的新号。任何 zone 库里都没有他们的行,也不在任何清单里,上面的步骤覆盖不到。设计 §4.4 / §11.4 把它登记为 D1「映射持久化兜底表」的遗留项。

### 9.3 映射落库:`route_tool` 导入与审计(映射持久化第 1a 批,2026-10-12)

> **代码未编译、未测试**(AGENTS.md §10.1)。首次执行前,先确认 [player-route-mapping-durability-proposal.md](../design/player-route-mapping-durability-proposal.md) §11.2「第 1a 批交 Codex 的验证」已跑绿。设计与门禁的完整说明在提案 §11.2,这里只写操作。

**是什么**:`go/data_service/cmd/route_tool`。它 SCAN mapping Redis 里的 `player:zone:*` / `player:placement:*`,对扫到的这些玩家用 MGET 一并读 `player:maprev`(作为导入行的 rev),再导入 `merge:merged_into:*`,写进全局库(`SnapshotMySQL.DBName`,dev 是 `testdb`)的 `player_route` 与 `zone_merged_into`,并审计两边是否一致。`player:maprev` 不是单独扫描的一族:**只有 maprev、没有 zone 也没有落点的玩家不会被导入**,两遍 SCAN 都看不到他(发布脚本写不出这种形状;若是 SCAN 之后、MGET 之前 zone 与落点被删、只剩 maprev,这名玩家计为 `MALFORMED`)。这类残留只有 audit 的方向 B(它也 SCAN `player:maprev:*`)会报成 `missing_in_db`。与 data_service 读同一份 yaml;mapping Redis 恒为 DB 0。输出只打 host 与库名,不打 DSN 与口令。

**在哪跑**:本批只能在有 Go 工具链与仓库检出的机器上 `go run`(dev / 本机)。**K8s 镜像只构建单一入口**,生产上跑要等第 1b 批把它并进 data_service 主程序(提案 §11.2 偏离第 8 条)。

> 2026-10-18 更正(映射持久化第 1b-1 批,代码未编译、未测试):入口已并进主程序,K8s 上用 `/app/service -f /app/etc/data_service.yaml -route-tool <子命令> [参数]`,子命令与参数和下面的 `route_tool …` 逐字相同;怎么在 K8s 上跑(默认一次性 Job / Pod,不 exec 进在役 Pod)见 §9.4。dev 上 `go run ./cmd/route_tool …` 照旧可用。

**前置**:

1. 全局库跑过迁移(`data_service -f <yaml> -migrate`,或 `Schema.AutoMigrate` 启动过),`route_meta(id=1)` 在。否则 route_tool 探针不过,退出码 2,提示先跑迁移。**若这个库跑过 2026-10-13 之前那一版(提交 `d5cbb8a8e`,行片模数还是 16)的迁移**:先 `SHOW CREATE TABLE player_route`,`chk_route_shard` 里是 `% 16` 就要在确认无数据后 DROP 全部五张路由表再迁移(提案 §11.2「行片 16 → 64」的前置)。这种旧表迁移不报错。2026-10-14 起 route_tool 与 db_first 的 data_service 建池时由探针 P5 认出(退出码 2,报错含 `route_shard modulus in CHECK chk_route_shard differs`),但 **redis 模式的 data_service 不建 route store、认不出**,所以这条人工核对仍要做。认出之后的处理同上(确认无数据后 DROP 五张路由表再迁移),不要改代码里的模数去迁就旧表;只重跑迁移修不好(`CREATE TABLE IF NOT EXISTS` 不改已有的表)。
2. **冻结窗口已经开始**:不跑 merge / unmerge / relocate / pin-placement / backfill 任何写模式(提案 §7 P1)。工具只能检测 merge / unmerge 立的围栏(`merge:in_progress:*`);relocate、pin-placement、backfill 不立围栏,只能靠人保证。import 在 stderr 第一行打的 NOTE 就是提醒这一点。
3. 账号对 `player_route` 有 INSERT 权限,且库上 `chk_route_shard` 生效(MySQL ≥ 8.0.16;TiDB ≥ 7.2 且建表前 `SET GLOBAL tidb_enable_check_constraint=ON`)。route_tool 的每个子命令(含 dry-run 与 audit)建池时都会跑探针 P4 与 P5:同一个必然回滚的事务里三次 `INSERT`(P4 一行必被 `chk_route_shard` 拒;P5 两行 `(0, 64)`、`(63, 63)` 核对模数),不留数据。P5 不需要额外权限;正确的库上它的两行会真的插入(未提交)到回滚为止,63 / 64 号玩家的建角可能短暂排队。反过来 63 / 64 号玩家的行被持锁超过 1s 时,这次建池以 1205 失败(退出码 2),重跑即可;多个建池方同时撞上还没有行的探针键时偶发的 1213 由建池自动有界重试(2026-10-15 补)。因此不能用只读账号(报错里写明 `INSERT privilege on player_route`),也不要连只读副本(只读实例拒绝这次 INSERT 时的报错未核实);不满足时同样是探针不过,退出码 2。

**P1 操作**(工作目录 `go/data_service`):

```powershell
# 1. dry-run(默认):只做 SELECT 判定,不写库,看统计
go run ./cmd/route_tool -f etc/data_service.yaml import-from-redis
# 2. 真导入。退出码 1 时先按 stderr 明细人工处理冲突 / 畸形 / rev 倒退,再重跑
go run ./cmd/route_tool -f etc/data_service.yaml import-from-redis -dry-run=false
# 3. 审计,期望退出码 0
go run ./cmd/route_tool -f etc/data_service.yaml audit
```

可选参数:`-batch=500`(1..1000)、`-timeout=30m`、`-max-report=200`(stderr 明细行上限,计数始终精确);import 另有 `-allow-db-first`(见下面「db_first 之后」);audit 另有 `-recheck-delay=2s`、`-max-recheck=10000`。

**重跑与恢复**(按退出码分两种,不能一概「原样重跑」):

- **退出码 2(中断、超时、连接 / 查询 / 写入出错)、开头就退出码 3(一行没写)**:与合服无关,导入幂等,窗口内原样重跑即可。
- **结尾退出码 3**(结尾还有围栏,或 `merge:merged_into` 与开头不同 = 导入窗口内有合服 / 撤销合服跑过):**不能原样重跑**。窗口内被合服改写的玩家,库里已导入的是旧 home(src)、缓存里是新 home(dst),而导入从不覆盖库里的非 NULL 值,重跑只会把他们判成 `conflict`(退出码 1)且永远收敛不了。恢复办法(只限 P1:全部 data_service 副本仍是 redis 模式,库还不是真源、没有任何读方):等合服窗口结束,在全局库上清空两张数据表、`route_meta` 原地复位(与两份清库脚本 `stress_round19.ps1` / `currency_crash_window.ps1` 的 `Clear-GlobalRouteTables` 同一口径),再从头 dry-run → 真导入 → audit:

  ```sql
  USE <SnapshotMySQL.DBName>;
  TRUNCATE TABLE player_route;
  TRUNCATE TABLE zone_merged_into;
  UPDATE route_meta SET writable=1 WHERE id=1;
  UPDATE route_reconcile SET leader=NULL, lease_until_ms=NULL, watermark_ms=0, cache_verified_ms=0 WHERE id=1;
  ```

  (2026-10-12 G1a:水位 / 租约从 `route_meta` 拆到了 `route_reconcile`,两张单行表都原地复位、不 TRUNCATE。)TiDB 上 TRUNCATE 会换表 ID、丢掉预切与 table attributes,清完要重跑下面的 `route_tool tidb-prepare`(或改用分批 `DELETE`)。

  任一副本已经切到 db_first 之后,库就是真源,**不许**这样清;那时合服本来就被冻结窗口禁止(§12),出现结尾退出码 3 说明冻结纪律被打破,停下来找负责人。

**import-from-redis 做什么**:

1. 开头 SCAN `merge:in_progress:*`,有一把就退出码 3,一行都不写;没有就原样快照一次 `merge:merged_into:*`(键 → 值);
2. A 遍:SCAN `player:zone:*`,每页一次 MGET 取这些玩家的 zone / placement / maprev。键名或任一值不规范(如 `007`、`0012`、`01:1`),整名玩家跳过并记 `MALFORMED`,不导半截;
3. B 遍:SCAN `player:placement:*`,跳过 A 遍处理过的 id;
4. `merge:merged_into:*` 导入 `zone_merged_into`。`merged_at_ms` 取导入时刻的库时钟,原始时刻已无从得知;同 src 而 dst 或 run_id 不同记冲突,不覆盖;
5. 结尾再查一次围栏,并把 `merge:merged_into:*` 与开头的快照比对(新增 / 消失 / 改值任一项):命中任一项就退出码 3,stderr 打 `ABORT` 行列出看到的围栏与变化。比对抓的是「在两次围栏检查之间开始并跑完」的合服 / 撤销合服 —— 它们跑完就删了围栏,结尾的围栏 SCAN 看不到,但 merge 一定先写 merged_into 再改映射、unmerge 一定先改回映射再删它。已写入的部分保留(统计行照出),恢复见上面「重跑与恢复」。relocate / pin-placement / backfill 既不立围栏也不写 merged_into,工具检测不到,只能靠冻结窗口。

**db_first 之后**:yaml 是 `RouteStore.Mode=db_first` 时,真导入(`-dry-run=false`)默认拒绝,退出码 2 —— 那时库是真源,「从缓存回填库」方向是反的(绕过发布脚本的写者留下的值可能被补进库),导入事务还会对一批玩家行持 `FOR UPDATE`、逐行 CAS 更新,并发的建角在同一玩家行上最多等满 1s 后以 1205 失败。唯一合法的用法是提案 §7 P3:全部副本切完之后,补一次滚动期间旧副本只写 Redis 的登记,这时显式加 `-allow-db-first`,并建议用小批量(如 `-batch=100`)缩短持锁时间;除此之外切 db_first 之后不再跑真导入。dry-run 与 audit 只读,不受影响。

逐名玩家的判定:库里没有行就插入,`rev = maprev`(没有 maprev 取 1);库 rev < maprev 记 `rev_regression`,不写(疑似库被回档);两边都有值且不同记 `conflict`,**不覆盖**;库有值而 Redis 无键记 `db_ahead`,不写、不算冲突;库为 NULL 而 Redis 有值则补列(`filled`)。

stdout 只有一行:`route_import dry_run=… players_scanned=… inserted=… matched=… filled=… db_ahead=… rev_regression=… conflicts=… malformed=… merged_into_inserted=… merged_into_matched=… merged_into_conflicts=… merged_into_malformed=…`。stderr 是 NOTE、连接目标,以及 `MALFORMED` / `CONFLICT` / `REV_REGRESSION` / `MERGED_INTO_CONFLICT` 明细。

**audit 做什么**(只读;建池探针 P4 / P5 除外,见前置第 3 条):方向 A 是库对 Redis(按主键键集翻页),方向 B 是 Redis 对库(SCAN 三族键;方向 A 没比过的 id 都在这里补比一次 —— 既包括库里没有行的,也包括方向 A 翻页扫过之后才提交的行,例如 db_first 下审计期间新登记的玩家),再全量对比 merged_into。有差异的玩家不超过 `-max-recheck` 时,等 `-recheck-delay` 后重读一次,只计两次都在的差异,被清掉的计入 `transient_cleared`;超过上限就全部计入并打 WARN。有生效围栏时只提示(`redis_fences_active` + WARN)。墓碑行(home 与落点都是 NULL)视为没有记录;maprev 缺席不算差异(本批 redis 模式下没有人写它)。stdout 一行 `route_audit …`,stderr 是 `DIFF kind=… player=… db=… redis=…` 明细。

| 计数 | 含义 |
|---|---|
| `db_only_unpublished` | 库里这行不是墓碑,三族键**全部**不在。可能是 db_first 下已提交未发布的孤儿行(发布失败、WAITAOF 超时、发布时撞上围栏),也可能是 Redis 丢了已确认的发布,本批分不开。**本批无法自动消除**,要等第 1b 批的全量预热推送;推送之前,先人工核对其中 home 指向已合走 zone 的行 |
| `missing_in_redis` | 库里这一列有值,Redis 缺这把键(三族键不是全缺) |
| `missing_in_db` | 库里这一列是 NULL 或根本没有这一行,Redis 有值 |
| `value_mismatch` | 两边都有,值不同(落点按编码后的字符串比,冻结态与稳定态算不同) |
| `redis_malformed` | Redis 的键名或值不规范(含 maprev、merged_into) |
| `db_invalid` | 库里这一行的落点过不了校验 |
| `redis_rev_ahead` | maprev 合法且大于库里的 rev:库被回档的信号,停下来找负责人 |
| `merged_into_missing_in_redis` / `merged_into_missing_in_db` / `merged_into_mismatch` | 合走标记两边不一致 |

**退出码**:

| 码 | import-from-redis | audit |
|---|---|---|
| 0 | 完成,冲突、畸形、rev 倒退、merged_into 冲突与畸形都是 0 | 所有差异计数都是 0(`redis_fences_active`、`transient_cleared` 不算) |
| 1 | 完成,但上面任一项非 0,需要人工处理 | 有差异,**`db_only_unpublished` 也算** |
| 2 | 失败:参数、配置、连接、查询或写入出错(含锁等待超时 1205)、超时;探针不过(没跑迁移、会话变量不对、TiDB 不是悲观事务、`chk_route_shard` 不生效、账号没有 `player_route` 的 INSERT 权限、表是按另一个行片模数建的 —— 后者 stderr 带 `another route_shard modulus`,只重跑迁移修不好,按前置第 1 条处理);yaml 是 db_first 而真导入没加 `-allow-db-first` | 同左(`-allow-db-first` 除外) |
| 3 | 开头或结尾 SCAN 到生效的 `merge:in_progress:*`,或结尾的 `merge:merged_into:*` 与开头不同,已中止。结尾的 3 不能原样重跑,见「重跑与恢复」 | 不用 |

**P2 切换门禁**(切 `RouteStore.Mode=db_first` 之前,缺一不可;完整说明见提案 §11.2):

1. **冻结窗口**:从任一 data_service 副本切到 db_first 起,到映射持久化第 2 批落地为止,禁止 merge_zone 一切写模式(§12 同一条);
2. 上面的 P1 做完:真导入退出码 0,audit 退出码 0;
3. mapping Redis 是 `noeviction` + `appendonly yes`、版本 ≥ 7.2;按拓扑填 `RouteStore.WaitAOFNumLocal` / `WaitAOFNumReplicas`(redis-ha 档 NumReplicas 写 1)。(2026-10-19 补,未运行、待 Codex 验证)K8s 上这两项不手填:`k8s_deploy.ps1` 生成 data-service ConfigMap 时 NumLocal 固定 1,NumReplicas 按共享 Redis 的形态渲染 —— 高可用档(`-RedisReplicas 3`,`redis-ha.yaml`;staging / prod 档缺省)为 1,单实例(`redis.yaml`)为 0;go/db ConfigMap 的 `ServerConfig.RedisClient.WaitAOFNumReplicas` 同源同值(`Get-RouteStoreWaitAOFNumReplicas`)。以写路径入口那行 `Redis topology: ... waitaof_replicas=<0|1>` 为准;zone-up 没给 `-RedisReplicas` 时跟随线上 `sts/redis` 的副本数。本机 compose 与单实例一律 0(仓库 `go/data_service/etc/data_service.yaml`、`go/db/etc/db.yaml` 保持 0,写 1 会因等不到副本被启动探针拒启)。单实例切到高可用之后,要对每个 zone 重跑 zone-up 并 `rollout restart deploy/db` 与 `deploy/data-service`(只改 ConfigMap 不会滚动它们),NumReplicas 才变成 1。db_first 装配 route store 之前,data_service 会按这两项发一次 `WAITAOF`(timeout = `WaitAOFTimeoutMs`)做能力探针:报错(appendonly 关着、Redis < 7.2)或确认份数不足(NumReplicas 写了 1 而没有副本)就不装配,日志里有 `route store not assembled ... cannot confirm AOF durability`,K8s(`Store.Required=true`)直接拒启 —— 不把配置错误留到每一次建角;
4. 建角压测已出 W1 p99 与「发布未确认率」的对比表(AGENTS.md §6),负责人拍板。「发布未确认率」看 `data_service_route_publish_total` 的 `not_durable` **加上** `error`:WAITAOF 回复形状不对是确定性错误、不重试,计在 `error` 而不是 `not_durable`;
5. 切换后看启动日志里那条 `WARN RouteStore.Mode=db_first: ...`;全部副本切完,再导入一次(`-dry-run=false -allow-db-first`,小批量)、再审计一次(提案 §7 P3)。db_first 下 audit 可能长期因 `db_only_unpublished` 非 0,P3 的「audit 为 0」要等第 1b 批。

**db_first 下 Redis 整体丢失**:本批还没有把库推回 Redis 的工具(第 1b 批的预热),仍按 §9.2 手工重放。全局库的路由表这时是最完整的一份记录,重放之后可以用 audit 核对结果。

> 2026-10-18 注:第 1b-1 批仍没有「库 → Redis」的推送(全量预热与就绪标记属于第 1b-2 批),上一段照旧有效。反方向(**全局库被回档、Redis 较新**)的恢复见 §9.4。

**`tidb-prepare`(TiDB-only,2026-10-12 G1a,代码未编译、未测试)**:对全局库的路由表设防合并、按片 / 按 zone 预切 Region。`player_route` 改成聚簇 `(route_shard, player_id)` 之后,不预切的话 64 条写入尾部在开服首日仍压在同一个 Region 上(提案 §11.2「第 1a 批热点修复」)。行片数 2026-10-13 由 16 改为 64(提案 §11.2「行片 16 → 64」):行数据与 `idx_route_updated` 每片一段;`idx_route_home` / `idx_route_storage` 每个 zone / 落点值切 8 个点(每 8 片一段)。

```powershell
# 默认 dry-run:判引擎、核对两张表是聚簇主键、列出将执行的语句(stderr 每条一行 PLAN)
go run ./cmd/route_tool -f etc/data_service.yaml tidb-prepare -zones=1,2
# 真执行(stderr 每条一行 DONE ... split_regions=N);-storages 只在 Phase 2 PinOnCreate 指向 1000000 时加
go run ./cmd/route_tool -f etc/data_service.yaml tidb-prepare -zones=1,2 -storages=1000000 -dry-run=false
```

- **何时跑**:TiDB 上建表之后;**开新 zone 时,在该 zone 开放(第一次建角)之前、低峰**跑一次(带上全部 zone 也行,幂等);BR / PITR 恢复之后;TRUNCATE 之后。两条 `ALTER … ATTRIBUTES` 是 DDL,在线重跑可能让在途建角以 8028 失败一次(提案 G1a HR9,未核实),所以挑开放前、低峰。
- **MySQL 上自动跳过**:stderr 打 `SKIP: … is not TiDB`,退出码 0。
- **只碰全局库**,不连 mapping Redis。它照样经过 route store 的建池探针:没跑迁移、会话变量不对、TiDB 不是悲观事务、**`chk_route_shard` 不生效**(MySQL < 8.0.16,或 TiDB 建表前没 `SET GLOBAL tidb_enable_check_constraint=ON`)、表是按另一个行片模数建的(P5,处理见前置第 1 条)都直接退出码 2。探针先于判引擎,所以 MySQL < 8.0.16 上是退出码 2,不是上一条的跳过。
- **前置核对**:`player_route`、`zone_merge_fence` 在 TiDB 上必须是 `CLUSTERED`(`information_schema.TABLES.TIDB_PK_TYPE`),不是就退出码 2、一条 ALTER / SPLIT 都不发 —— 只能趁表空 DROP 后用 `data_service -migrate` 重建。
- **权限**:SPLIT / ATTRIBUTES 需要的权限【未核实】;应用账号不够时用运维账号的 yaml 副本跑(副本不进 git、口令不进日志)。另:建池探针 P4 / P5 是同一个必然回滚的事务里的 `INSERT`,所以 route_tool 的任何子命令(含只读的 audit)都要 `player_route` 的 INSERT 权限。
- **退出码**:0 完成或 MySQL 跳过;2 参数非法(`-zones` 必填,每项 1..999999;`-storages` 每项 ≥ 1)、连接 / 探针失败、核对失败、某条语句失败(stderr 的 `FAILED` 行带第几步与语句;已执行的保留,重跑即可)。stdout 一行 `route_tidb_prepare engine=… skipped=… dry_run=… zones=… storages=… statements=… executed=… split_regions=…`。
- **不许**为了「清干净」去关 `chk_route_shard`(`ALTER … NOT ENFORCED`)或把 `tidb_enable_check_constraint` 改回 OFF:`player_id` 的唯一性靠它。改过 DDL 或全局变量之后必跑一次 audit(方向 A 是唯一看得见错片行的检测器)。
- K8s 上同样要等第 1b 批把 route_tool 并进主程序入口。
  > 2026-10-18 更正:已并进(第 1b-1 批,未编译、未测试),K8s 上是 `/app/service -f /app/etc/data_service.yaml -route-tool tidb-prepare -zones=… [-dry-run=false]`,跑法见 §9.4。

> 2026-10-19 补(映射持久化第 1b-2 批收尾,代码未编译、未测试):**`audit -require-maprev`(严格 maprev 口径,提案 §7 P3 的门禁)**。上面「maprev 缺席不算差异」只对默认口径成立。
>
> - 用法:`go run ./cmd/route_tool -f etc/data_service.yaml audit -require-maprev`(K8s:`/app/service -f /app/etc/data_service.yaml -route-tool audit -require-maprev`)。只在 `RouteStore.Mode=db_first` 下可用;redis 模式在连任何东西之前退出码 2(那时没有写者维护 `player:maprev`,每一行都会判落后)。
> - 多一类差异 `redis_rev_behind`:库行不是墓碑、缓存里至少有它的一把键,而 `player:maprev` 缺席或小于库 rev(值相同也算 —— 缺 maprev 的键会被发布脚本的遗留值守卫挡住,maprev 落后会让下一次发布判错新旧)。三族键全无的行仍只计 `db_only_unpublished`,不重复计;墓碑、库行无效(`db_invalid`)、缓存值畸形(`redis_malformed`)不计。复核与首读同一口径。
> - 输出:统计行在末尾多一段 ` redis_rev_behind=N`(默认口径的统计行逐字不变,脚本照旧解析);stderr 是 `DIFF kind=redis_rev_behind player=… db=… redis=…` 明细与一行 `NOTE: -require-maprev`。
> - 退出码:0 = 含 `redis_rev_behind` 在内全部为 0;1 = 任一类非 0;2 = 失败,或 yaml 是 redis 模式。
> - P3:二次导入(`-dry-run=false -allow-db-first`)之后、全部副本打开 `RouteStore.Reconcile.ArmReadyMarker` 之前,`audit -require-maprev` 必须退出码 0;非 0 时等对账 leader 的增量推送补齐 maprev(看 `data_service_route_reconcile_push_total{phase,result}`)后重跑,**不要**手工 `SET player:maprev:*`(绕过发布脚本的写者,提案 §8.3 第 1 条)。
> - 与 `-frozen-recovery` 同用:`-frozen-recovery` 只豁免 `db_only_unpublished`,`redis_rev_behind` 照常计入退出码。冻结期反向修库之后「库 rev 领先缓存」的行要等解冻后 leader 推送补 maprev,冻结期内一般到不了 0,所以冻结期的门禁仍是 §9.4 第 3 步的 `audit -frozen-recovery`(不加 `-require-maprev`);两个开关同用只作核对。

### 9.4 映射冻结、反向修库与 K8s 上的 route-tool(映射持久化第 1b-1 批,2026-10-18)

> **代码未编译、未测试**(AGENTS.md §10.1)。首次执行前先确认 [player-route-mapping-durability-proposal.md](../design/player-route-mapping-durability-proposal.md) §11.2「第 1b-1 批」的「交 Codex 的验证」已跑绿。设计、偏离与风险都在那里,这里只写操作。

**入口**

- dev / 本机:`go run ./cmd/route_tool -f etc/data_service.yaml <子命令> …`(与 §9.3 逐字相同),或编出 data_service 后 `data_service -f etc/data_service.yaml -route-tool <子命令> …`。要看退出码就先 `go build` 再跑可执行文件,`go run` 对子进程非 0 一律自己退 1。
- K8s:`/app/service -f /app/etc/data_service.yaml -route-tool <子命令> [参数]`。stdout 只有一行统计,版本行与明细都在 stderr。
  - **默认用一次性 Job / Pod 跑**:照 `deploy/k8s/manifests/go-svc/data-service-migrate.yaml` 的形状(同一镜像、同一 ConfigMap `go-svc-data-service-config` 挂到 `/app/etc`、`restartPolicy: Never`),改 `metadata.name`、`labels`(不要用 `app: data-service`,否则会混进 Service)、`args: ["-f", "/app/etc/data_service.yaml", "-route-tool", "<子命令>", …]`,`backoffLimit: 0`(退出码 1 / 3 要人看,不能自动重跑),内存按全局库行数单独设。仓库里还没有现成的 Job 清单,它登记为第 1b-3 批的必做项。
  - **exec 进在役 Pod 只限 dry-run 或小库**(`kubectl -n <ns> exec deploy/data-service -- /app/service -f /app/etc/data_service.yaml -route-tool audit`):data-service 容器的内存 limit 是 512Mi(`data-service.yaml`),import / audit 在内存里保留 O(N) 的已见 id 集合(100 万行约几十 MB,千万行就是几百 MB),在役容器里 OOM 会把正在服务的 data_service 一起杀掉;CPU 也与服务共享 500m 的 limit。reconcile-from-cache 不保留已见集合(内存只占一页),但它会持锁写全局库,同样建议走 Job。
  - `-timeout`(缺省 30m)按行数设;超大表的耗时没有测过。

**新子命令与退出码**

| 子命令 | 做什么 | 0 | 1 | 2 | 3 |
|---|---|---|---|---|---|
| `freeze [-reason=…] [-freeze-timeout=5s]` | `route_meta.writable = 0`:之后的 W1(db_first)与导入一律拒写。只碰全局库,不连 Redis。redis 模式也能跑,但那时 W1 不读这个开关,stderr 打 WARN | 已冻结(含本来就冻结) | — | 失败 | — |
| `reconcile-from-cache [-dry-run=true] [-resolve-ties=report\|cache] [-freeze-grace=10s] [-batch=500] [-max-report=200]` | 冻结态下把缓存里较新的映射修回库(缓存 maprev > 库 rev 或库里没有这一行) | 干净 | 有未覆盖的平局(TIE)、冲突、畸形 | 失败;没冻结;yaml 是 redis 模式 | 开头或结尾有生效的 `merge:in_progress:*` |
| `audit -frozen-recovery` | 冻结期恢复口径:`db_only_unpublished` 照常计数、打明细,但不计入退出码,其余类别必须为 0 | 除 `db_only_unpublished` 外全为 0 | 其余任一类非 0 | 失败;没冻结;redis 模式 | — |
| `unfreeze -confirm-audited [-cache-was-not-verified-acknowledged] [-freeze-timeout=5s]` | 解冻(先把 `route_reconcile.watermark_ms` 抬到可信标记的水位,再 `writable = 1`;generation 不换) | 已解冻(含本来就可写) | 就绪标记在途中被改过,重跑 | 失败;没加 `-confirm-audited`;确认不了缓存且没加 `-cache-was-not-verified-acknowledged`;redis 模式;MappingRedis 不是 node | — |

> 2026-10-18 更正(修复评审,代码未编译、未测试):上表 `freeze` 行「redis 模式也能跑」与 `unfreeze` 行退出码 2 里的「redis 模式」原先不对称 —— `import-from-redis` 的写开关检查与模式无关,redis 模式下误跑 / 演练过一次 `freeze`(或从 db_first 回退到 redis 模式时带着冻结),P1 / P3 的导入就一直报 `route_meta: writable!=1, route writers are frozen`(`ErrRouteNotWritable`),切到 db_first 之后所有建角也从拒写开始,而工具里没有撤销路径。现在 redis 模式下这样撤销:
>
> `unfreeze -confirm-audited -cache-was-not-verified-acknowledged`
>
> 不连 mapping Redis、不读就绪标记(redis 模式没有标记语义,也跑不了 `audit -frozen-recovery`),最低水位取 0,stderr 打 `WARN: RouteStore.Mode=redis`,stdout 是 `marker=not_read_redis_mode`;没加 `-cache-was-not-verified-acknowledged` 仍是退出码 2(在连任何东西之前判),MappingRedis 不是 node 不再挡这条路径。所以 `unfreeze` 行退出码 2 的「redis 模式」应读作「redis 模式且没加 `-cache-was-not-verified-acknowledged`」,「MappingRedis 不是 node」只对 db_first 成立。**只在所有 data_service 副本都确实是 redis 模式时这样做**;手里的 yaml 若是旧的、实际已有副本在 db_first,改用那份 yaml 走下面「全局库被回档之后的恢复」第 4 步。**不要**手写 `UPDATE route_meta SET writable = 1`:它绕过「先抬水位再解冻」的顺序和 store 的写入守卫。

`-freeze-timeout` 不得低于 5s;TiDB 把 `route_meta` 设成缓存表之后(TiDB Phase 1 的部署步骤,`tidb-prepare` 不做),冻结 / 解冻写要等一个读租约,时限要 ≥ `tidb_table_cache_lease` + 2s(缺省租约 3s,未核实)。

**全局库被回档之后的恢复(`RouteStore.Mode=db_first`)**

K8s 下全局库与 zone 库在同一个 mysql 实例,zone 回档(`docs/design/zone_data_rollback.md` 步骤 2)会连带回档全局库:库丢了回档点之后的映射写,而 mapping Redis 里还是较新的一份。第 1b-2 批之前**没有自动检测**,凡是对全局库所在实例做过 PITR / 备份恢复,都按下面做。所有档位当前仍是 redis 模式,本节在切 db_first(提案 §7 P2)之后才适用。

0. **前提**:合服冻结窗口在(§12,db_first 下本来就禁止 merge_zone 写模式);**不要清 mapping Redis**(回档脚本的 FLUSHDB 防护不要用 `-ConfirmFlushSharedRedis` 放行):这时缓存才是较新的一边,清掉就只剩回档后的库。
   > 2026-10-18 更正(修复评审):上句只说了「不要整库清」,没说回档步骤 3 本来要做的事该怎么完成 —— 清掉**本 zone** 回档点之后的玩家缓存,让 C++ 从回档后的 MySQL 重建。跳过它就是只回了一半:回档点之后的玩家缓存活过 PITR,节点按比 MySQL 更新的缓存继续运行、回写。db_first 下这一步仍必须做,只是要选择性地做:只清本 zone 的玩家缓存(键名以 go/db 与 C++ 的键名函数为准,本文不列,禁止按印象拼 pattern),保留全部全局键 —— `player:zone:*`、`player:placement:*`、`player:maprev:*`、`merge:*`、`mapping:route:ready`、go/db 的 `consumer:applied*` 游标与 `kafka:retry*` 重试收据、账号目录。K8s 上能安全执行这种选择性清理的载体(`k8s_zone_rollback.ps1` 不整库 FLUSHDB、按本 zone 处置玩家缓存)登记为第 1b-3 批的前置;**在它落地并验证之前,db_first 下 K8s 的 zone 回档不可执行**。所有档位当前仍是 redis 模式,本条在 P2 之前不影响现行回档。
1. `freeze -reason="PITR <时间点>"`:建角从此 fail-closed(Unavailable + 30)。冻结之前已经过开关检查的建角仍可能提交并发布,第 2 步开头会等 `-freeze-grace`(缺省 10s)。
2. `reconcile-from-cache`(默认 dry-run)看统计;再 `reconcile-from-cache -dry-run=false`。
   - stdout:`route_reconcile_from_cache dry_run=… players_scanned=… inserted=… restored=… filled=… matched=… db_ahead=… ties=… ties_resolved=… conflicts=… malformed=… merged_into_inserted=… merged_into_matched=… merged_into_conflicts=… merged_into_malformed=…`;stderr 是 `TIE` / `TIE_RESOLVED` / `CONFLICT` / `MALFORMED` / `MERGED_INTO_CONFLICT` 明细。
   - `restored` 是库被回档丢掉、由缓存补回的行(rev 置为缓存的 maprev);`db_ahead` 是库更新的行(不动);`TIE` 是同 rev 两份值,先人工判断哪边对,确认缓存对再加 `-resolve-ties=cache` 重跑;冲突与畸形按明细逐条处理。可以原样重跑(幂等,已修的行再判就是 matched)。
   - 修库期间每批事务持 `route_meta` 的行锁:**不要滚动重启 db_first 的 data_service**,新副本建 route store 的探针最多等 1s,撞上就 1205、`Store.Required=true` 时拒启。
3. `audit -frozen-recovery` 必须退出码 0。`db_only_unpublished` 是库独有的行(回档前就已提交但发布失败的孤儿行等),这个口径下不计入退出码;它们在第 1b-2 批 leader 落地之前解冻后仍不在缓存里,默认口径的 audit 会一直报。
   > 2026-10-18 补(修复评审):还有一类同样「已提交、未发布」的孤儿行**不**被这个口径豁免,会让退出码停在 1:库里原本只有落点的行(P1 从 Redis 导入的「只有落点」玩家),建角时 W1 补上 home、rev + 1 之后发布失败。这时缓存里仍只有 `player:placement`、没有 `player:zone` 也没有 `player:maprev`,audit 把它计成 `missing_in_redis`(明细里 Redis 一侧只有落点、落点与库相同、`redis_maprev` 为空,库 rev ≥ 2)。`reconcile-from-cache` 修不了它(缓存没有 maprev 时走导入语义,库已有值就判 `db_ahead`),冻结期 W1 又被挡住,1b-1 也没有库到缓存的推送,所以冻结期内修不掉。逐条核对明细确认是这种形状(且回档时间点之后没有合服 / 搬库动过这名玩家)之后,记下这些 player_id,当作已知差异继续第 4 步(此时 `audit -frozen-recovery` 的退出码是 1,只因这些行);解冻后同一 player_id、同一 home 再走一次 W1 时,幂等分支会把它补发布,否则等第 1b-2 批的库到缓存推送补齐,补齐后再跑默认口径的 audit 核对。**不要**手工 `SET player:zone:*` 去「补」:那是绕过发布脚本的写者(提案 §8.3 第 1 条)。形状对不上的一律按真丢失处理,不要解冻。
4. `unfreeze -confirm-audited`。
   - 就绪标记(`mapping:route:ready`)可信 = 存在、可解码、标记里的 run_id / master_replid 等于当前 Redis:直接解冻。
   - **第 1b-2 批之前没有任何写者挂这个标记**,所以 unfreeze 一律退出码 2,stderr 说明原因。这时冻结期间(或回档前后)Redis 可能也丢过东西(重启 / 切主 / 换实例),库与缓存可能同时缺同一批登记,audit 看不见两边都缺的行。先按 §9.2 的清单核对:收齐回档时间点之后的全部合服 / 搬库清单,确认它们改的映射在缓存与库里都在(db_first 期间合服本来就被冻结窗口禁止,通常这份清单是空的);有缺的按 §9.2 补齐后重跑第 2、3 步。确认完再 `unfreeze -confirm-audited -cache-was-not-verified-acknowledged`。
   - 退出码 1 = 标记在途中被改过,原样重跑。
5. 解冻后建角恢复。回档时间点之后、冻结之前提交的映射如果也不在缓存里(两边都丢),只能按清单与业务日志另行追查,本流程覆盖不到。

**Redis 丢失(第 1b-2 批之后)**:db_first 且就绪标记打开时,leader 会自动全量预热并挂标记,§9.2 的手工重放只用于 redis 模式或库与 Redis 同时丢失。**第 1b-1 批还没有这一步**,仍按 §9.3 末尾「db_first 下 Redis 整体丢失」做。

**在线清库与 db_first**:`stress_round19.ps1` / `currency_crash_window.ps1` 在服务运行中先 FLUSHALL 再清路由表、复位 `route_meta` / `route_reconcile`。第 1b-2 批的 leader 落地之后,db_first 下这样清库会与 leader 互相干扰(把将被清掉的行推回刚清空的 Redis,或把水位复位误判成回档)。脚本防护属于第 1b-3 批;在那之前,db_first 下清库前先停 data_service,或保持 redis 模式。

> 2026-10-19 补(映射持久化第 1b-2 批收尾,代码未编译、未测试):**`rewarm` 与 `unfreeze` 的水位键出口**。设计见 [spec_D1_batch1b2_2_3.md](../handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md) §2 M2 / M3 / M5、§13.1 第 4 / 6 条、§13.5 第 1 条(并入提案 §13 之前以它为准)。

**`rewarm [-wait=0] [-poll=5s] [-timeout=30m]`**(撤掉三把映射控制键,让对账 leader 从头全量预热、重挂就绪标记)

- 做什么:**一条** `DEL mapping:route:ready mapping:route:warm mapping:route:watermark`(就绪标记、预热令牌、水位键;单条命令是原子的,不会留下半撤的状态)。只碰这三把键:不碰 `player:*` 映射键与合服围栏,不推送,不挂标记(标记只由 leader 写)。stdout 一行 `route_rewarm deleted=… marker_was=… waited=… rearmed=… baseline_set_at_ms=… new_set_at_ms=…`;stderr 是 NOTE、被撤标记的原值(`MARKER …`,诊断用)与 `DROPPED …`。
- `-wait=0`(缺省):只撤,不开全局库;两种模式都允许。**回退 redis 模式**时按这个顺序(规格 §13.5 第 1 条,紧急回退也不能跳过前两步):先关全部读方开关(`RouteStore.ReadRequiresReady`、`Placement.RequireRouteReady`、`MergeMarkerRedis.RequireRouteReady`)→ 全部副本 `RouteStore.Reconcile.ArmReadyMarker=false` 滚动,确认现任 leader 计了 `data_service_route_reconcile_event_total{event="marker_dropped_disarmed"}` → `RouteStore.Mode=redis` 滚动 → `rewarm -wait=0`(一并删令牌与水位键)。关 go/db 的 `Placement.RequireRouteReady` 有前提:**映射缓存完整**(标记已挂、现任 leader 的 `data_service_route_ready_selfcheck{verdict="trusted"}` 为 1)。缓存不完整时(标记不在 / 身份不符 / leader 正在全量预热,包括下面「撤掉标记之后」的预热期)关它,go/db 退回两键 MGET,缺键被当成「没有落点」落到 home / 本 zone 的库,已搬库或已合服 pin、键还没预热到的玩家会被静默写进错库。这时 go/db 的就绪闸关着就是安全状态(存盘留在 Kafka),要止损只能停 go/db,等预热完、标记挂上再按上面的顺序回退;告警 `GoDbRouteGateClosed` 的处置说明同口径。
- `-wait=<时长>`:撤完等 leader 重挂,只许 db_first(redis 模式在连任何东西之前退出码 2:没有 leader)、映射写者没冻结(冻结期 leader 不预热,退出码 2,三把键都不动)、`-wait` 必须小于 `-timeout`。判据:出现可解码、且 `set_at_ms` 大于基线的新标记;基线 = 被撤标记的 `set_at_ms`,被撤时没有可解码的标记就取撤之前读的库时钟(挡住从旧 AOF / RDB 恢复回来的旧标记)。被撤标记是更新格式版本时 `-wait` 直接退出码 2、什么都不撤(换新版二进制,或用 `-wait=0`)。
- **撤掉标记之后,打开了读方开关的服务全部 fail-closed,直到 leader 全量预热完重挂**:go/db 暂停消费(`db_route_ready_gate` 为 0,Kafka lag 增长)、data_service 回源 SQL、guild 当作合服封锁。全服规模下全量预热要几个小时(未压测),**低峰执行,事先确认能承受**。
- 什么时候用:用拷贝方式(SCAN + DUMP / RESTORE 等)迁移过 mapping 实例之后(规格 M5:迁移时不得拷贝这三把控制键;拷了、或拿不准有没有拷,迁移后必须 rewarm);怀疑缓存不完整而 leader 没察觉时;leader 因淘汰类原因闩住不挂标记、原因已排除时(rewarm 之后 leader 重新取淘汰基线)。
- 退出码:

| 码 | 含义 |
|---|---|
| 0 | 已撤(`-wait` 时:已等到新标记,`rearmed=true`) |
| 1 | 已撤,但 `-wait` 内没等到新标记(`rearmed=false`,统计行照出)。stderr 的 `TIMEOUT` 行列出要查的项:每个副本 `RouteStore.Mode=db_first`、`RouteStore.Reconcile.Enabled`、`RouteStore.Reconcile.ArmReadyMarker`;`data_service_route_reconcile_leader`(恰好一个副本为 1)与 `data_service_route_reconcile_arm_enabled`;`data_service_route_reconcile_event_total` 的 `arming_refused_eviction` / `arming_refused_unresolved` / `arming_refused_debug_enabled` 与 `task_aborted`;`data_service_route_reconcile_push_total` 的冲突类 result。原因排除后用更长的 `-wait` 重跑(重跑只会再撤一次,leader 从头预热) |
| 2 | 失败:参数(`-wait` 为负、`-poll` 不为正、`-wait` 不小于 `-timeout`)、redis 模式下 `-wait`、冻结中 `-wait`、`MappingRedis.Type` 不是 node、连接 / 读 / DEL 出错、`-timeout` 先到。DEL 之后才出的错 stderr 写明 `keys ARE dropped`:leader 会自己重挂,看现任 leader 的 `data_service_route_ready_marker` |
| 3 | 不用 |

**`unfreeze` 的水位键出口**(db_first;redis 模式的 unfreeze 不读不碰水位键)

- 解冻之前先读 `mapping:route:watermark`;交给解冻事务的最低水位取 max(可信标记的 `watermark_ms`, 世代与库相同的水位键的 `watermark_ms`),`-cache-was-not-verified-acknowledged` 放行时同样取水位键那一项。不这样抬,解冻之后 leader 下一拍就会按「水位键 > 库水位」再冻结(`freeze_watermark`)。别的世代的水位键不约束本世代。
- `writable = 1` 之后(同一次 unfreeze 里),把水位键改成 `{库世代, 上面的最低水位}`:不在就 SET NX,在就按原值 compare-and-set;最低水位为 0 时,不在就留给 leader 下一拍建,是别的世代 / 畸形就按原值删掉(同样留给 leader 重建)。stderr 一行 `WATERMARK_KEY <处置> before=<原状态> generation=… watermark_ms=…`,处置取 `set_nx` / `compare_and_set` / `kept` / `left_absent` / `dropped` / `superseded` / `not_reset`;stdout 统计行格式不变(`min_watermark_ms` 已含水位键给的下限)。
- 写进水位键的是交给解冻事务的下限,不是解冻后的精确库水位(解冻按 GREATEST 抬水位,解冻后库水位 ≥ 它,所以不会触发再冻结;leader 之后每拍按库水位推进它)。
- 退出码(在上面 §9.4 表的 `unfreeze` 行之上补):水位键是更新格式版本(`v` 更大)或不是 string 类型 → **2**,在解冻之前、任何写之前拒绝,不动它(版本更新:换新版二进制;不是 string:人工确认后只 `DEL mapping:route:watermark` 再重跑);解冻之后水位键没改成(按原值写输了且复读到别的世代 / 畸形,或 Redis 出错)→ **1**:writable 已经是 1,原样重跑 —— 已可写时重跑什么都不做;若 leader 因 `freeze_generation` / `freeze_watermark` 又冻结了,重跑会再走一遍并重置水位键。写输了但复读到本世代的值(leader 已推进它)或键已不在,算完成(`superseded`,退出码 0)。

---

## 10. 回滚

### 10.1 第一手段:`merge-zone-unmerge`(**默认路径**)

适用面:**合服刚跑完、还没开服**,发现搞错了对象。撤销前**目标区也必须处于停服态**:撤销按目标区口径跑 P2~P7,要求目标区无活节点、目标 topic 与重试队列排空、清单玩家无锁无会话无活队伍。所以 Step 6 zone-up 之后要撤销,得先把 target 按 §8 Step 2 重新排空并 zone-down。

```powershell
# 先 dry-run 看会动多少
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-unmerge `
  -MergeManifestPath <repo>\merge_<SRC>_to_<DST>.json `
  -MergeDbCapabilityZones <此刻 go/db 仍在跑的其他 zone,不含 SRC / DST;没有就写 none> `
  -MergeKafkaConsumerGroupsCmd <path> -MergeKafkaBootstrap <broker:9092> `
  -MergeEtcdEndpoints <http://etcd:2379[,...]> `
  <ROUTE_ARGS> -DryRun

pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-unmerge `
  -MergeManifestPath <repo>\merge_<SRC>_to_<DST>.json `
  -MergeDbCapabilityZones <与 dry-run 同一份> `
  -MergeKafkaConsumerGroupsCmd <path> -MergeKafkaBootstrap <broker:9092> `
  -MergeEtcdEndpoints <http://etcd:2379[,...]> `
  <ROUTE_ARGS>
# <ROUTE_ARGS> 必须与当初合服时同一份(§15.1);P4 之后用旧二进制撤销 = 直写 Redis,禁止(§15.5)
# db 源下撤销写到一半中止(LEFT IN PLACE):同一命令加 -MergeZoneTakeOverFence <中止日志里的 run_id> 重跑(§15.4),不要 DEL 镜像
# Kafka 门禁也可用 -MergeAssumeKafkaDrained(进复盘);两者都不给会在 P3 被拒
# -MergeDbCapabilityZones:清单是 pin 模式时必填(没有缺省值,可 none),不给在围栏之前就被拒;copy 模式可不给,给了就查
# K8s 未部署 guild 加 -MergeSkipGuildMySql -MergeSkipGuildRank(与合服时同一组开关)
```

撤销顺序与合服**完全相反**:先把路由改回去,再往回搬数据。这样任何一步失败时,玩家的归属都指向一个**确实有他数据**的 zone:

```
(撤销期间同样上围栏;P / S 拒绝时本次什么都没写:首次撤销释放围栏,半撤销时保留围栏,见下)
P   前置门禁(目标区口径):P2~P7
S   清单玩家不得有冻结的落点记录(与搬库互斥);有「落点记录 ≠ src」的人时要求 src 的能力标记;
    -MergeDbCapabilityZones 列出的 zone 逐个核对(none 不查)
7'  清掉 player_merge_notice:{pid} / player_force_rename:{pid}
5'  player:zone:{id} 改回 src(只改当前值确实是 dst 的那些)
5'a 删 merge:merged_into:{src}(dst 与 run_id 都对得上才删)
5'b 共享玩家缓存失效(copy 模式;连 player:{id}:save_barrier 一起删,开服前必须重建,见下文「屏障重建」)
4'  guild_rank ZSET 成员按原分数 ZADD 回 src、从 dst ZREM
3b' trade_listing.market_zone 改回 src(只改清单里、当前确实在 dst 的 listing_id)
3'  guild.zone_id 改回 src(只改清单里的 guild_id)+ guild:v2 缓存失效
1'  copy 模式:删目标库里那些**与源库逐字节相同**的玩家行;跳过此刻有落点记录的玩家
```

**模式按清单走**(`-MergePlayerRowsMode` 对撤销不起作用):
- **pin 模式**:只改回 home,**不动落点记录**(`"{src}:1"` 与改回后的 home 一致,有效落点不变),不删缓存,不删行。
- **copy 模式**:照旧,但 1' 跳过此刻有落点记录的玩家。他的有效落点由记录决定,目标库那一行可能正是他的真源。

**S 段的能力标记要求**(两种模式都查,2026-09-28 起,未经运行验证):清单玩家里有人的落点记录**不指向 src**(含畸形记录;例如合服后被 `merge-zone-pin-placement` 钉在了 dst,或被搬库搬走)时,撤销要求 **src** 的能力标记,缺了就在任何写之前拒绝(`unmerge refused: N manifest players have a placement record that does not point at zone <SRC> ...`)。这些人的有效落点不随 home 改回,撤销之后处理他们存盘的是 src 的 go/db,旧版会写进 `zone_<src>_db` 里的旧副本。
- 能力标记是 90s 心跳(§5.1),而 src 的 namespace 在 §8 Step 2 已删除,所以遇到这条拒绝时,要仿照 §8 Step 2 末尾的做法临时拉起一个同版本、`ZoneId=src` 的新版 go/db,等 `GET db:capability:zone:<SRC>` 出现再撤销。撤销完成后 src 反正要 zone-up。
- 合服时 pin 模式钉的 `"{src}:1"` 与无记录的人不计入,所以一般的 pin / copy 撤销碰不到这条。

**`-MergeDbCapabilityZones` 的口径**(与合服相同,2026-09-28 起,未经运行验证):清单是 pin 模式时必填、没有缺省值,不给在立围栏之前就被拒;copy 模式可不给,给了就查。撤销时 src、dst 都已停,只列此刻仍在跑的其他 zone,没有就写 `none`;列了 src 或 dst 照常检查,已下线就在 S 段被拒(`unmerge refused: -db-capability-zones: ...`,首次撤销释放围栏)。这条与上面「落点记录 ≠ src 时要求 src 标记」是两条独立检查。

**半撤销时的拒绝保留围栏**:撤销不记步骤进度。立围栏后工具先看清单玩家里是否已有人 `player:zone == src`(上一次撤销在 5' 之后中止),有就打 `RESUME: some manifest players already map to zone <SRC> ...`,之后 P / S 段的拒绝不释放围栏(文案带 `LEFT IN PLACE`),按 §3 ③ 处理;读映射失败也按半撤销处理。没有则照常释放。

**硬保证**:

- **只碰清单里的对象**。目标区原住民按构造安全。
- **1' 只删逐字节相同的行**。任何一行在合服后被改过(玩家登录过、打过一场、领过邮件)就**拒绝删除并报出来**:那时删掉的不是「拷贝」,而是合服后产生的唯一一份数据。源库的行从来没删过,所以留一份多余副本是安全的(mapping 已经指回源区,不会被读到)。
- `player:{id}:*` blob **不自动删**。mapping 指回源区后没人读它们;确认源副本无误后再手动清。

**屏障重建:copy 模式撤销之后、zone-up 之前必做**(2026-10-20 修复 r2-mz#1,未经运行验证)

5'b 对**全部清单玩家**连父快照提交屏障 `player:{id}:save_barrier` 一起删:改回源区后玩家读源库,合服后在目标库提交的 pending / 两表 ACK 不再对应他要读的行。删完之后,`owner_epoch≠0`(进过场景)的人冷登录按源库两行分流(2026-10-22 修订:login 冷加载已改由 `savebarrier.Reconcile` 判定):两行同版非零的,login 采纳 SQL 自愈(设计 V7,每人一条 `PARENT-LOAD-ADOPT-SQL`);零围栏、缺行或两行不同版的,以「历史玩家父快照与提交屏障缺失」等原因拒登 —— fail-closed、不丢数据。重建仍然必做,**完成之前不许开服**,这是保守做法:逐人回执在开服前就把零围栏、缺行、两行不同版的人暴露出来,而不是开服后才在登录时拒绝;重建之后冷加载走 V1 一致路径,不产生采纳告警。pin 模式撤销不删缓存也不删屏障,跳过本段。前向 copy 合服从 2026-10-20 起保留屏障,不需要本段;它的 `verify:save_barrier` 不是 info 时见本段末尾。

1. **保持维护闸**:source / target 保持 §8 Step 1 的维护标记与 Step 2 的 zone-down,不 `k8s-zone-up`、不 `/open`;scene / login / go/db 与合服、pin、relocate、回填工具都不运行。`save_barrier_restore` 的 `-writers-stopped -queues-drained` 只是声明,工具查不了这一条。
2. **列出要重建的人**:清单 `player_ids` 里 `player:<id>:owner_epoch` 存在且不为 `0` 的玩家。owner_epoch、屏障与父快照都在 §2.1 表 login / shared 那一行的实例(`-MergeNoticeRedisAddr` / `-MergeNoticeRedisDB`,DB 0)。owner_epoch 不存在或为 `0` 的人从没进过场景,不需要屏障,也不能放进计划(工具以「归属计数器缺失」拒绝)。再按源库两行先分出两类单独处理(2026-10-22 补;用 `SELECT player_id, fence_owner_epoch, fence_save_seq FROM zone_<SRC>_db.player_database WHERE player_id IN (<名单>)` 与 `player_database_1` 的同一查询比对名单):
   - **源库缺行**(两张表任一张没有这名玩家):多半是 r2-login#1 的受害者(owner 已铸、首次存盘之前失败),合服前就登录不了。恢复工具对他报「SQL 玩家行缺失」,放进计划会让整份计划在他这里退出 —— **排除出计划**,名单单独登记进维护工单,等 r2-login#1 的恢复手段。
   - **两行都是零围栏**(`fence_owner_epoch = 0 AND fence_save_seq = 0`):围栏之前的历史行。先查清 owner 为何已铸而行从未经围栏写入;确认后单独做计划,plan 加 `-allow-legacy`(落点分组与下面相同;只在 plan 给;工具先原子提交两表围栏元数据,重锁复核后才发布屏障)。两行一张零围栏、一张不是的,恢复工具按「两表 SQL 围栏不同版」拒绝,按 §10.2 查清,不放进计划。

   余下的人(两行同版非零)再按此刻的落点记录 `player:placement:<id>` 分组:
   - **没有记录**(copy 模式被拷的人都是):home 已改回 `<SRC>`,行在 `zone_<SRC>_db`。用下面的命令,plan 带 `-allow-home-fallback`。
   - **有记录**:记录是 `<SRC>:<版本>` 的用同一条命令、不带 `-allow-home-fallback`;指向别的库(钉在 dst、搬过库)的按 [mysql-backup-pitr-runbook.md](mysql-backup-pitr-runbook.md)「2026-10-18 第二批」走 v2(要独立审定的 `-audited-owner-upper-bound`)。

   每份计划最多 1000 人,超出就分批,每批一个计划文件。
3. **建维护标记**:在同一实例 DB 0 上 `SET player:save_barrier:maintenance:<SRC> <维护工单号>`,**不设 TTL**。它只供工具校验,不会让任何进程停写。合服围栏 `merge:in_progress:<SRC>` 必须已由撤销释放,工具见到它就拒绝。映射(`-MergeRedisAddr` 的 DB 0)与共享 Redis 不是同一实例时,改用 `-routing-mode redis-split -mapping-redis <映射实例>`(v2,先 `-mode guard`);`-MergeZoneRouteSource db` 的环境按 PITR 手册第二批走 `db_first`,那条路径本来就接受本区空落点,不需要 `-allow-home-fallback`。
4. **plan → 审查 → apply**。工作目录 `go/db`;`SAVE_BARRIER_RESTORE_DSN` / `SAVE_BARRIER_RESTORE_REDIS_PASSWORD` 由受控环境注入,不打印:

```powershell
go run ./cmd/save_barrier_restore -mode plan -schema zone_<SRC>_db -zone <SRC> `
  -players <逗号分隔的玩家号> -redis <共享 Redis host:port> -redis-db 0 -routing-mode redis-same-instance `
  -maintenance-id '<维护工单号>' -plan '<受控目录>/barrier-plan-<SRC>-<批号>.json' `
  -allow-home-fallback -writers-stopped -queues-drained

go run ./cmd/save_barrier_restore -mode apply -schema zone_<SRC>_db -zone <SRC> `
  -redis <同上> -redis-db 0 -routing-mode redis-same-instance `
  -maintenance-id '<同一维护工单号>' -plan '<同一计划文件>' `
  -plan-sha256 '<plan 输出的 SHA-256>' -writers-stopped -queues-drained
```

   `-allow-home-fallback` 只在 plan 给,写进计划、计入 SHA-256;apply 不接受它,以审定计划为准。它只放宽一件事:`player:placement:<id>` **不存在**且 `player:zone:<id>` 恰为 `-zone` 的人,按该区自己的 zone 库(`-schema` 必须就是它)恢复。以下照旧拒绝:home≠zone;落点键存在(空串、他库、冻结、畸形);两表 SQL 围栏不同版;父 / 子缓存、资产证据或交接标记仍在;补投桶非空;合服围栏仍在。「落点记录仍不存在」在写屏障的同一段 Lua 里原子复核。屏障按 SQL 两行的当前围栏重建,不会把任何旧屏障恢复回来。任一人失败立刻退出、已完成前缀不回退,修复原因后按**同一计划**重跑。
5. **验证后再开服**:按 PITR 手册「整批成功后」那段,用受控测试号验证缺父键冷登录读到同版两行;再按原值删掉 `player:save_barrier:maintenance:<SRC>`,然后才按下一段 zone-up。

**合服 Step 5 的 `verify:save_barrier` 不是 info**(2026-10-20 新增;2026-10-22 按 login 冷加载重写):前向 copy 合服保留屏障、删掉父快照,开服后每人第一次登录都是冷加载。这一行按 login 冷加载(`savebarrier.Reconcile`)逐人预判清单里在 Redis 有存盘历史的玩家:info = V1 一致路径;warn = login 会采纳 SQL 放行;block = login 会拒登。Notes 按结论分组,带前几个 id。只有一格有意比 login 严:`ack_not_equal_pending`(实例没重启时 login 只等补投,而 P4 之后等不来)。不进 Step 6,保持维护闸,按结论处置。**屏障重建一律按上面第 1~5 步,把 `<SRC>` 换成 `<DST>`**(`-zone <DST> -schema zone_<DST>_db`,维护标记 `player:save_barrier:maintenance:<DST>`,无记录者带 `-allow-home-fallback`),重建后重跑 `-VerifyMerged`:

- warn `barrier_missing_sql_adoptable`(屏障缺失或只剩 ACK,两行同版非零)/ `sql_rows_ahead_of_barrier`(两行或 ACK 新于 pending):不丢数据,但不是正确合服留下的状态,先查成因 —— 前者多半是用 2026-10-20 之前的二进制跑了 copy 合服(旧版删屏障),后者说明合服前 Redis 丢过已确认的写。成因清楚、记进工单后可以进 Step 6:开服后这些人第一次登录各有一条 `PARENT-LOAD-ADOPT-SQL`。采纳时 login 还要过 C4(enforce 模式下 schema 世代倒退照样拒),开服前用受控测试号冷登录一次。屏障整把缺失的也可以按上面的方法就地重建,结果与采纳相同;屏障 hash 还在的(只剩 ACK,或 `sql_rows_ahead_of_barrier`),恢复工具要求已有屏障的 pending 与两表 ACK 都等于 SQL 版本,会以 `barrier or outbox conflicts` 拒绝,只能交给 login 采纳,不要为了重建去删屏障。
- block `barrier_missing_zero_fence`(owner≠0、无屏障、两行都是零围栏):先查清 owner 为何已铸而行从未经围栏写入,确认后就地重建,plan 加 `-allow-legacy`。
- block `barrier_missing_no_sql_row`(owner≠0、无屏障、目标库缺行):copy 是逐字节拷贝,先用 `SELECT` 核对源库是否同样缺行。源库也缺 —— 合服前他就登录不了(r2-login#1:owner 已铸、首次存盘之前失败),不是合服造成的;合服既不能也不该替他补行,`save_barrier_restore` 对他报「SQL 玩家行缺失」,不要放进任何计划。名单登记进维护工单。这一类 block 重跑不会消失,是否带着这批人开服由负责人书面决定(开服后他们仍登录不了,与合服前相同),或等 r2-login#1 的恢复手段就绪。源库有行而目标库没有,是拷行出了问题,按 `verify:manifest_rows` 的 block 处理。
- block 其余结论(`barrier_missing_rows_disagree`、`ack_not_equal_pending`、`sql_row_fence_not_equal_pending`、`sql_row_missing`、`owner_epoch_behind_pending`、`barrier_without_owner`、各类 `*_malformed`):屏障或行与登录要求不一致,说明 P4 之后有写入或数据损坏。**不要就地重建,也不要为了让这一行变绿而手工删屏障** —— 屏障里可能还有未提交的存盘意图(`outbox_*`),按目标库旧行重建可能丢掉更新的数据。按 §10.2 的单玩家事故查清,或整体撤销。
- 读失败是 INFRA(没查成),不是通过。Notes 写着时间预算用完(`time budget ... ran out`)时重跑 `-VerifyMerged`;同样人数下持续超时,说明 `runAuditMode` 给每项检查的预算不够,要改代码放宽,不能跳过这一行。

撤销完成后(copy 模式先完成上面的屏障重建):`k8s-zone-up` 把 source 与 target 都拉起来(2026-09-29:两区各自照传上一次部署的 `-GateRouterMode` / 集群外入口 / Ingress / `-ReleaseProfile` 参数,口径同 §8 Step 6);pin 模式清单的撤销,在两区 `/open` 之前跑 `merge-zone-capability-check -MergeDbCapabilityZones <SRC_ID>,<DST_ID>`,**必须 exit 0**,否则不开服、先把对应 zone 的 go/db 换成新版。之后再发「合服延期」公告。

### 10.2 什么时候 unmerge 不够

- **已经开服、玩家已经在 target 里玩过**:copy 模式的 1' 会大面积 REFUSED,得到的是一个半撤销状态。这时应当**不要**撤销,转为单玩家修复。
  - **`RollbackPlayer` 当前不可用**:data_service 的应用级回档要求跨服务离线 epoch 栅栏 `RollbackFence`,而生产里它是 nil,`RollbackPlayer` / `RollbackZone` 一律返回 `ErrCodeNotImplemented` 并打 ERROR `cross-service offline epoch fence is not configured`(`go/data_service/internal/logic/rollback_logic.go`)。见 [`single_player_rollback.md`](../design/single_player_rollback.md) 与 TiDB 决策 §D8。
  - **实际可行的替代**是 DBA 手工单玩家修复,只在玩家**离线**时做(无 `player:session:{pid}`、无 `lock:player:{pid}`、他 home 的 db_task topic 无积压):
    1. 按 [`mysql-backup-pitr-runbook.md`](mysql-backup-pitr-runbook.md) §4.1 把 §8 Step 3 的备份恢复到**影子 MySQL**,不要碰生产库。
    2. 用 `GET player:placement:<pid>` / `GET player:zone:<pid>` 确认该玩家的**有效落点库**。
    3. 在有效落点库里按主键点更新他的各玩家表行(整行 REPLACE 自影子库),一次一个玩家。
    4. 删共享 Redis 上他的缓存 `PlayerAllData:<pid>` 与 `<消息名>:<pid>`,否则下次登录读到的仍是旧缓存。
    5. 登记进事故单。
  - **pin 模式**下源区库一直是真源,合服没有动过玩家行。「合服后数据不对」多半与合服无关,先按单玩家问题排查。
- **清单丢失 / 损坏**:见 10.3。

### 10.3 最后手段:备份还原

按 [`mysql-backup-pitr-runbook.md`](mysql-backup-pitr-runbook.md) §4 把 target 完整还原到 §8 Step 3 的 dump 时间点,Redis 用同一时刻的 RDB。
**代价**:target zone 在窗口开始之后的所有写入一起丢。pin 模式下还要一并还原 `zone_<src>_db`(这批玩家的真源在那里)。所以这是最后手段,不是默认路径。

### 10.4 复盘

合服失败必须在 24h 内出复盘文档 `docs/design/server-merge-postmortem-<date>.md`。

---

## 11. T+1 ~ T+7 观察期

- [ ] 每天跑一次 `merge-zone-audit -VerifyMerged -MergeExpectedSrcPlayers <N> -MergeManifestPath <清单>`,确认持续一致
- [ ] 客服工单打 `merge-<src>-<dst>` tag,主程 4h 响应 SLA
- [ ] 关注「东西不见了 / 公会没了 / 好友丢了」类投诉
- [ ] 单玩家数据问题按 §10.2 的手工修复走(`RollbackPlayer` 未通电)
- [ ] 盯 go/db:`db_placement_guard_total{outcome="stale_topic"}` 恒 0;`lookup_error`、`db_placement_store_open_total{result="error"}` 不持续上涨
- [ ] **清单文件与审计报告归档进维护工单**,清单永久保留(§9)。备份保留按 [`db-retention`](../design/) 现行 90 天口径

> source zone 的 K8s namespace 在 §8 Step 2 就已删除,**没有「T+7 删 namespace」这一步**。要清的是:服务器列表里下架 source、备份归档、`zones.json` 里摘掉 source。
> **pin 模式下不要清 `zone_<src>_db`**:它仍是这批玩家的真源。要退役它,按 §14.2 搬走再 §14.4 审计。

---

## 12. 已知限制与仍然开着的口子(2026-09-28)

| 项 | 影响 | 应对 |
|---|---|---|
| **本批代码全部未编译、未测试** | pin 模式、搬库、A1–A16 修复、dev_tools 新转发都只有代码审查 | 以 PROGRESS 本批条目的 Codex 验证结果为准;首次生产使用前先在 staging 跑通 |
| **Unity 客户端不处理 `RedirectToGate`** | 登录期跨区重定向客户端接不住 | **`HomeZone.RedirectOnEnterEnabled` 必须在生产保持 `false`**(`go/login/etc/login.yaml` 默认就是 false)。robot 已经能跟随重定向,客户端还不能 |
| **真实集群合服演练从未跑过** | 全部结论来自代码审查 + 单测 / 集成测试 | 首次生产合服前先在 staging 跑一次完整 §8 + §10,pin / copy 各一次;搬库另演练一次正向 + 反向 |
| **pin 合服的源区库不能退役** | 源区库仍是被钉玩家的真源 | 退役前 `merge-zone-relocate` 搬走 + `merge-zone-storage-audit` 结论为可退役(§14) |
| **pin 模式与多 data Redis 集群不兼容** | data_service 按 home 选 data Redis 集群,pin 不拷 blob | 多集群部署用 copy 模式;是否在 pin 模式下补 blob 拷贝待设计拍板 |
| **dev_tools 转发后的退出码** | 已修:先 build 再执行 + 显式 `exit`,0 / 1 / 2 原样透传;编译失败为 2(§7.1) | 修复未经运行验证前,仍以报告表里的 `INFRA:` 行交叉核对 |
| **能力标记是 90s 心跳,T-0 的 src / dst 标记已消失** | 已修(未经运行验证):`-MergeDbCapabilityZones` 不再有缺省值,T-0 只列仍在跑的 zone 或写 `none`;dst 在 zone-up 之后用 `merge-zone-capability-check` 核对 | 按 §5.1 填列表,§8 Step 6 的 capability-check 非 0 不开服 |
| **能力标记管不住的两个窗口** | go/db 滚动发布新旧 Pod 重叠期间标记已存在;回退后 90s 内标记仍残留 | 发布完成之前、回退后等满 90s 或手工 DEL 之前,不做 pin 合服 / 搬库 / 开 `PinOnCreate`(§5.1) |
| **映射丢失无重放工具** | 只能按 §9.2 手工重放;钉到全局库的新号不可恢复 | D1 映射持久化兜底表仍是遗留项 |
| **组队没有管理员解散入口** | P7 只能靠玩家离队或 24h 过期 | T-7 公告要求离队(§6.1、§8 Step 1) |
| **guild 的 `MergeMarkerRedis` 可缺失**(部署了 guild 的环境) | 整段没配 = 建帮闸门不生效,窗口内仍能在源 zone 建帮,那个公会不会被搬走 | 合服前确认 `go/guild/etc/guild.yaml` 的 `MergeMarkerRedis.Host` 指向 mapping Redis 且 `DB: 0` |
| **合服不做昵称冲突检测(刻意如此)** | 角色名由 data_service 全局库 `player_name` 保证**全服唯一**(§4.4),合服期**无冲突可查** | `force_rename` 全链路保持未启用、原样保留。哪天名字改成按 zone 唯一,才需要在这里补检测并启用它 |
| **`mail` / `auction` / `chat_history` / `guild_application` 表不存在** | audit 不扫它们(也无须扫) | 对应服务上线后再补 auditor |
| **合服公告 UI 靠客户端** | 服务端已把 `player_merge_notice:{pid}` 写进 login Redis(DB 0),login 首登时消费并下发 | 客户端接上即可生效 |
| **player blob 跨集群拷贝是逐玩家流式** | 大 zone(>10w)分钟级 | source / target 同 Redis 后端时工具自动跳过这一步 |
| **go/db 每条任务多两次 MGET** | 选库一次、落库后复核一次 | 未压测;按 AGENTS.md §6 出对比表之前不下性能结论 |
| **映射持久化 db_first 与 merge_zone 不能共存**(2026-10-12,第 1a 批) | merge_zone 在映射持久化第 2 批之前仍只写 Redis、从不写全局库的 `zone_merge_fence` / `player_route`。db_first 下库内围栏恒不生效,merge / unmerge / relocate / pin-placement / backfill 改出的 Redis 值库里不知道,库与缓存长期分叉 | **任一 data_service 副本为 `RouteStore.Mode=db_first` 起,到第 2 批落地为止,禁止 merge_zone 任何写模式**(merge / unmerge / relocate / pin-placement / backfill)。所有档位默认 redis;切换门禁见 §9.3 |

已闭合、从本表移除的旧项:
- C++ 审计 topic 世代号改为配置(2026-09-09,`audit_topic.h`);
- C++ db_task topic 世代号改为配置(本批,`DbTaskTopicGeneration`);
- `dev_tools.ps1` 已转发 `-kafka-topic-generation`(2026-10-15 更正:组名不再缺省转发,由工具按 zone 推导并认定唯一权威组;`-MergeKafkaGroup` 只用于显式覆盖,非空才转成 `-kafka-group`;世代号仍从 `go/db/etc/db.yaml` 现读。见 §2.3);
- `dev_tools.ps1` 已转发帮会跳过开关与撤销的 Kafka 门禁。
- 以下三项 2026-09-28 修复,未经运行验证,修复结论以 Codex 验证为准:
  - K8s 未部署 guild 时 `-VerifyMerged` 恒 exit 2:两个跳过开关同时给出时,`verify:guild_zone` / `verify:guild_rank` 改报 SKIPPED(warn);
  - 续跑在清单落盘前被拒绝会释放围栏:改为保留围栏(§3);
  - 撤销重跑在半撤销状态下被门禁拒绝会释放围栏:同上。

---

## 13. 命令快参

| 操作 | 命令 |
|---|---|
| **映射来源 `<ROUTE_ARGS>`(下列 merge-zone 命令都要带,capability-check 除外;§15.1)** | db_first:`-MergeZoneRouteSource db -MergeZoneRouteDbDsn $env:MERGE_ZONE_ROUTE_DB_DSN [-MergeZoneWaitAofReplicas 1(redis-ha)]`;仍是 redis 模式:`-MergeZoneRouteSource redis` |
| **回填(前置,每 zone 一次)** | `dev_tools.ps1 -Command merge-zone -MergeBackfillZone <id> <ROUTE_ARGS> [-DryRun]` |
| pre-merge 审计 | `dev_tools.ps1 -Command merge-zone-audit -MergeSourceZone <s> -MergeTargetZone <d> <ROUTE_ARGS>` |
| dry-run 合服 | `dev_tools.ps1 -Command merge-zone -MergeSourceZone <s> -MergeTargetZone <d> -MergeDbCapabilityZones <zones\|none> -MergeManifestPath <file> <ROUTE_ARGS> -DryRun` |
| 正式合服 | `dev_tools.ps1 -Command merge-zone -MergeSourceZone <s> -MergeTargetZone <d> -MergeExpectedSrcPlayers <n> -MergeDbCapabilityZones <zones\|none> -MergeManifestPath <file> -MergeKafkaConsumerGroupsCmd <p> -MergeKafkaBootstrap <b> <ROUTE_ARGS> [-MergePlayerRowsMode copy]` |
| **能力标记核对(dst zone-up 之后、开服之前)** | `dev_tools.ps1 -Command merge-zone-capability-check -MergeDbCapabilityZones <zones>`(exit 0 / 1 / 2 = present / missing / unreadable;不读写映射,不带 `<ROUTE_ARGS>`) |
| 合服后验证 | `dev_tools.ps1 -Command merge-zone-audit -MergeSourceZone <s> -MergeTargetZone <d> -MergeExpectedSrcPlayers <n> -VerifyMerged -MergeManifestPath <file> <ROUTE_ARGS>`(db 源多一行 `verify:route_cache`) |
| **撤销一次合服** | `dev_tools.ps1 -Command merge-zone-unmerge -MergeManifestPath <file> (-MergeKafkaConsumerGroupsCmd <p> -MergeKafkaBootstrap <b> \| -MergeAssumeKafkaDrained) -MergeDbCapabilityZones <zones\|none>(pin 清单必填) <ROUTE_ARGS> [-DryRun]` |
| **db 源中止后续跑(围栏 LEFT IN PLACE)** | 原合服 / 撤销命令(同一清单、同一 `<ROUTE_ARGS>`)加 `-MergeZoneTakeOverFence <中止日志里的 run_id>`(先确认那次运行的进程已不在;§15.4)。redis 源仍按 §3 ③ GET / DEL |
| 批量钉落点 | `dev_tools.ps1 -Command merge-zone-pin-placement -MergePinPlacementZone <z> <ROUTE_ARGS> [-DryRun]` |
| 搬库 | `dev_tools.ps1 -Command merge-zone-relocate -MergeRelocateSourceStorage <S> -MergeRelocateTargetStorage <T> -MergeDbCapabilityZones <全部在跑 zone,不接受 none> <ROUTE_ARGS> [-MergeRelocatePlayerIds a,b] [-MergeRelocateBatchSize n] [-MergeRelocateLockWait 150s] [-MergeManifestPath f] [-DryRun]` |
| 放弃搬库 | `dev_tools.ps1 -Command merge-zone-relocate-abort -MergeManifestPath <f> <ROUTE_ARGS> [-DryRun]` |
| 落点库审计 | `dev_tools.ps1 -Command merge-zone-storage-audit -MergeAuditStorage <id> <ROUTE_ARGS>` |
| 映射运维(K8s 一次性 Job) | `pwsh -NoProfile -Command "& ./tools/scripts/dev_tools.ps1 -Command k8s-route-tool -ZoneName <name> -RouteToolArgs @('audit') -KubeContext <ctx>"`(转 `tools/scripts/k8s_route_tool.ps1`,退出码原样:0 / 1 / 2 / 3;用法见 §9.4) |
| 预建落点库 | `go/db` 目录:`go run ./cmd/migrate -f <db.yaml> -storage-id <id> -command up -create-database`(§14.5) |
| zone-down(**会删 namespace**) | `dev_tools.ps1 -Command k8s-zone-down -ZoneName <name>` |
| zone-up | `dev_tools.ps1 -Command k8s-zone-up -ZoneName <name> -ZoneId <id> -WaitReady`(2026-09-29:照传该 zone 上一次部署的 `-GateRouterMode` / 集群外入口 / Ingress / `-ReleaseProfile` 参数,见 §8 Step 6) |
| 触发 MySQL 备份 | `kubectl create job -n mmorpg-infra --from=cronjob/mysql-backup mysql-backup-manual-$(date +%s)` |
| 网关置维护 / 开服 | `POST /admin/zones/<id>/maintenance` / `POST /admin/zones/<id>/open`(头 `X-Admin-Key`) |
| 整 zone PITR 回档 | `tools/scripts/k8s_zone_rollback.ps1 -ZoneName <n> -ZoneId <i> -TargetTime <iso>`(2026-09-29:回滚脚本新增第 0 步停服前预检;以 `"0"` 回退、external、经 Ingress 对外或 staging / prod 档运行的 zone 须照传 `-GateRouterMode` / `-ClientEntryMode` / `-GatewayIngressHost` / `-ReleaseProfile` 等,清单见 `docs/design/zone_data_rollback.md`「## 3. 整 Zone 灾难恢复级回档」下的「2026-09-29 修订」) |
| 查围栏 / 标记 / 落点 | `redis-cli -h <mapping-redis> -n 0 GET merge:in_progress:<zone>` / `GET merge:merged_into:<zone>` / `GET db:capability:zone:<zone>`(`TTL` 同键,正常 60~90)/ `GET player:placement:<pid>` |

**merge-zone 系列的全部 dev_tools.ps1 参数**(`Get-MergeZoneArgs` + `Get-MergeZoneCommandArgs`;转发契约由 `tools/scripts/tests/dev_tools_merge_zone_contract.tests.ps1` 钉住):

```
公共(全部 merge-zone 系列命令都转发):
  -MergeMySqlDsn <dsn>                  必须同时够着 zone_<N>_db / player_store_*_db 与各独占库
  -MergeRedisAddr / -MergeRedisPassword / -MergeRedisDB          guild 库,默认 2
  -MergeMappingRedisAddr / -MergeMappingRedisPassword
  -MergeMappingRedisDB <n>              默认 -1 = 从 data_service.yaml 现读,兜底 0
  -MergeNoticeRedisAddr / -MergeNoticeRedisDB                    login/shared,默认 0(搬库 R2 的排序锁也在这里)
  (-MergeFriendRedisAddr / -MergeFriendRedisDB 已于 2026-09-18 删除,见 §2.1)
  -MergeSceneRedisAddr  / -MergeSceneRedisDB                     默认 0
  -MergeTableListJson <path>            默认 generated/data/mysql_database_table_list.json
  -MergeKafkaGroup <g>                  留空 = 不转发,工具按 zone 推导候选组(db_rpc_consumer_group_z<zone> + legacy)并认定唯一权威组;
                                        非空才转发 -kafka-group,P3 只看这一个组(2026-10-15,§2.3)
  -MergeKafkaTopicGeneration <n>        默认 -1 = 从 go/db/etc/db.yaml 现读
  -MergeSourceDataRedis / -MergeTargetDataRedis / -MergeDataRedisPassword
  -MergeSourceDataRedisDB / -MergeTargetDataRedisDB
  -MergeExpectedSrcPlayers <n>          -1 = 不校验
  -MergeSkipTradeMySql                  三处(合服 / 撤销 / 审计)同一口径
  -MergeSkipGuildMySql / -MergeSkipGuildRank    同上;两个一起给才算「跳过帮会」

仅 merge-zone:
  -MergeSourceZone / -MergeTargetZone / -MergeBackfillZone
  -MergeManifestPath <file>             不给则自动生成到仓库根;dry-run 写 <file>.dryrun.json
  -MergePlayerRowsMode pin|copy         留空 = 工具默认 pin
  -MergeDbCapabilityZones <z1,z2,...|none>  没有缺省值:pin 必填(ps1 先点名拒绝);copy 只在清单玩家已有落点记录时由工具要求。
                                        填全部仍在跑的 zone(T-0 不含 src / dst),没有就写 none(§5.1)
  -MergeMigratePlayerBlobs              多 Redis 集群部署才需要,仅 copy 模式
  -MergeClearSourceHotState
  -MergeAllowEmptySource                只在真的要合空区时用

merge-zone 与 merge-zone-unmerge:
  -MergeAssumeKafkaDrained              与 -MergeKafkaConsumerGroupsCmd 二选一,必须给其一
  -MergeKafkaConsumerGroupsCmd <path> / -MergeKafkaBootstrap <host:port>

仅 merge-zone-unmerge:
  -MergeDbCapabilityZones <z1,z2,...|none>  清单是 pin 模式时由工具要求(ps1 不读清单,没给只提示);copy 撤销给了才查

仅 merge-zone-capability-check:
  -MergeDbCapabilityZones <z1,z2,...>   必填,不接受 none;只读,不转发写意图,-DryRun 不起作用
                                        退出码 0 = 全部 present / 1 = 有 missing / 2 = 有 unreadable

仅 merge-zone-audit:
  -VerifyMerged                         必须同时给 -MergeManifestPath

仅 merge-zone-unmerge / merge-zone-relocate-abort:
  -MergeManifestPath <file>             必填,文件必须存在
  (relocate-abort 另外可选转发 -MergeRelocateSourceStorage / -MergeRelocateTargetStorage / -MergeRelocateBatchSize,给了才转发)

仅 merge-zone-pin-placement:
  -MergePinPlacementZone <z>

仅 merge-zone-relocate:
  -MergeRelocateSourceStorage <S> / -MergeRelocateTargetStorage <T>    必填
  -MergeDbCapabilityZones <显式 zone 号>                               必填,不接受 none(搬库在线进行)
  -MergeRelocatePlayerIds a,b,c / -MergeRelocateBatchSize n(默认 100)/ -MergeRelocateLockWait 150s
  -MergeManifestPath <file>             可选;不给则自动生成 <repo>\relocate_<S>_to_<T>_<UTCts>.json

仅 merge-zone-storage-audit:
  -MergeAuditStorage <id>               只读,不转发写意图,-DryRun 不起作用

除 audit / storage-audit / capability-check 外通用:
  -DryRun                               给了 = -dry-run,不给 = -apply(工具拒绝两者都不给)

退出码:全部 merge-zone* 命令都以 merge_zone 自己的退出码结束(先 go build 再直接执行产物);merge_zone 编译失败时工具不运行,退出码为 2(§7.1)。
```

---

## 14. 玩家存储落点运维(pin-placement / relocate / storage-audit)

设计见 [`player-storage-placement.md`](../design/player-storage-placement.md) §9–§11。落点记录 `player:placement:{id}` 在 mapping Redis DB 0,值为 `"{storage_id}:{version}"`(稳定)或 `"{storage_id}:{version}:frozen:{run_id}"`(搬库中)。`storage_id` 为 `1..999999` 对应 `zone_{id}_db`,`≥ 1000000` 对应 `player_store_{id}_db`。

**上线顺序红线**(设计 §13):
1. data_service、scene_manager、go/db(`Required=false`)新版全部就绪;
2. 各 zone 能力标记齐全(90s 心跳,`merge-zone-capability-check -MergeDbCapabilityZones <全部 zone>` exit 0,或 `GET` / `TTL db:capability:zone:<Z>` 手查);
3. 之后才允许 pin 合服、relocate、login `PinOnCreate`;
4. `Placement.Required=true` 必须在**全部 login 开了 `PinOnCreate`、全部 zone 跑完 §14.1** 之后;
5. 写过落点记录之后**禁止回退 go/db**;
6. go/db **发布完成**(新 Pod 全部 Ready、旧 Pod 全部退出)之前,不执行 pin 合服、relocate,也不打开 `PinOnCreate`:新旧 Pod 重叠期间标记已经存在,心跳管不住;
7. 第 3 条之前回退了某个 zone 的 go/db:等满 90s,或在 mapping Redis DB 0 上 `DEL db:capability:zone:<Z>`,之后才能做第 3 条里的事;
8. 任一 go/db 开了 `Required=true` 之后:不许把任何 login 的 `PinOnCreate` 改回 false,不许把 `NewPlayerStorageId` 改成没有 go/db 能打开的库,不许把 login 或 data_service 回退到不支持 storage_id 的旧版。违反时建角照常成功、login 零报错,但新号只有 `player:zone` 没有 `player:placement`,预加载秒级失败、存盘全部进死信(`missing_required`)。要回退这些组件,先关掉全部 go/db 的 `Required`。

**互斥矩阵**:
- 合服与撤销在围栏之下拒绝冻结玩家;
- 搬库 R1 在 CAS 里检查玩家 home 所在 zone 的围栏;
- pin-placement 拒绝有围栏或已合走的 zone,且逐人 CAS 时再检查围栏。

### 14.1 批量钉落点 `merge-zone-pin-placement`

给 home==N 且没有落点记录的玩家钉 `"{N}:1"`。有效落点不变,**在线执行安全**,幂等,不要求能力标记。

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-pin-placement -MergePinPlacementZone <N> <ROUTE_ARGS> -DryRun
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-pin-placement -MergePinPlacementZone <N> <ROUTE_ARGS>
# <ROUTE_ARGS> 见 §4 / §15.1
```

期望输出:

```
=== Pin placement "N:1" for players with home_zone=N and no record (dry-run=false, mapping=<addr> db=0) ===
=== Pin placement done (migrated): zone N: candidates=... pinned=... already_pinned=... kept_existing=... home_moved=... anomalies=... ===
```

- N 有合服围栏或 `merge:merged_into:N` 标记时拒绝;运行中出现围栏会立即停下,稍后重跑即可(幂等)。
- `home_moved > 0`:扫描与钉之间有人被合走,没有钉他,正常。
- 用途:打开 `Required=true` 的前置;§9.2 映射丢失恢复后的重钉。

### 14.2 搬库 `merge-zone-relocate`

把一批玩家的主数据从落点库 S 搬到 T:冻结 → 等在途写 → 拷贝 → 比对 → 切换。**不要求玩家离线**:冻结期间 go/db 把他们的写延后(不耗重试预算),切换后按游标落到 T。

**前置**:
- 全部在跑的 zone 都有能力标记,用 `-MergeDbCapabilityZones` **显式列出**(没有缺省值;**不接受 `none`**:搬库在线进行,所有在跑的 zone 都必须列出并有标记)。工具还会自动检查清单玩家的 home zone:列表外且无标记的 zone,其玩家在 R1 跳过。
- S、T 两个库都存在,且都在 `-MergeMySqlDsn` 所在的同一个 MySQL 实例上;表按列名对齐。T 是全新落点库时先按 §14.5 预建。
- 两库都过了 §4.4 的列检查;T 的列必须齐,否则 go/db 按需打开会被 schema 闸拒绝。
- 没有进行中的合服涉及这批玩家的 home zone。
- `-MergeKafkaTopicGeneration` 与线上一致(dev_tools 默认从 db.yaml 现读):R2 等的排序锁键里带 topic 名。
- 配好告警:`db_placement_guard_total{outcome="frozen_deferred"}` 持续上涨 = 有玩家一直冻结。go/db 对冻结期间的重试重排只打 DEBUG(`retry task rescheduled (placement frozen)`),ERROR 日志里看不到冻结,只能靠这个指标;落点库不可用(写)仍打 ERROR `retry task rescheduled`。

```powershell
# 先 dry-run:只写 <manifest>.dryrun.json 预览,列出会冻结 / 会跳过的人
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-relocate `
  -MergeRelocateSourceStorage <S> -MergeRelocateTargetStorage <T> `
  -MergeDbCapabilityZones <全部在跑的 zone,例 1,2,3> `
  -MergeManifestPath <repo>\relocate_<S>_to_<T>.json <ROUTE_ARGS> -DryRun

pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-relocate `
  -MergeRelocateSourceStorage <S> -MergeRelocateTargetStorage <T> `
  -MergeDbCapabilityZones <全部在跑的 zone> `
  -MergeManifestPath <repo>\relocate_<S>_to_<T>.json <ROUTE_ARGS>
# 只搬部分人:-MergeRelocatePlayerIds a,b,c;批大小 -MergeRelocateBatchSize(默认 100);
# 等锁上限 -MergeRelocateLockWait(默认 150s,略大于 go/db 排序锁 TTL 2m)
# <ROUTE_ARGS> 见 §4 / §15.1;续跑与 relocate-abort 必须用同一份(db 源下冻结 / 切换先写库,再发布 + WAITAOF,见 §15.3)
```

不给 `-MergeRelocatePlayerIds` = 整库搬:落点记录指向 S 的人,加上 S 为 zone 库时无记录且 home==S 的人。枚举的是扫描那一刻的集合,期间新钉到 S 的人要再跑一次。

| 步骤 | 内容 | 失败处置 |
|---|---|---|
| R0 | S≠T、两库存在、列名对齐、能力标记;**清单在任何写之前落盘** | 拒绝,exit 1,什么都没写 |
| R1 | 冻结:有效落点 == S、非冻结、home 可读且其 zone 已确认能力、home 的 zone 无合服围栏 → CAS 写 `"{S}:{v}:frozen:{run}"`(无记录按 v=1) | 不满足的人记为 `skipped` |
| R2 | 等在途写:每人每张表的 `distributed:lock:kafka:ordering:{db_task_zone_<home>[_g<gen>]}:{pid}:{表名}`(共享 DB 0)至少观察到一次不存在 | 超过 `-MergeRelocateLockWait` → 本批解冻(`unfrozen`) |
| R3 | 每人一个 RC 事务:每张表先删 T 里的旧行(冷副本,计数进清单),再从 S `INSERT…SELECT`,两条语句都是主键等值 | 回滚,解冻该人 |
| R4 | 逐表逐列 `CAST AS BINARY` 比对 S 与 T | 不一致或比对出错 → 解冻该人 |
| R5 | CAS 切换 `"{S}:{v}:frozen:{run}"` → `"{T}:{v+1}"` | 记录被人动过 → `cas_failed`,保持原状,**需要人工处理** |

**结果与退出码**:
- 全部 `switched` 时 exit 0,日志 `All N players now live in storage T. The rows left in storage S are cold copies ...`;
- 否则 exit 1,列出 `cas_failed` / `unfrozen` / `skipped` 的人数与样本。修好原因后,用**新的** `-MergeManifestPath` 另起一次(终态不会被续跑重做)。
- 经 dev_tools 跑时退出码原样透传(§7.1)。

**工具中途崩溃**:冻结值带 run_id 留在 Redis(db 源下同时留在库里 `player_route.frozen_run_id`),这些玩家的存盘持续延后、不会丢,`frozen_deferred` 上涨。二选一:
- **续跑**:用**同一条命令、同一个 `-MergeManifestPath`** 重跑。清单每批在 R1 后、R5 后各落盘一次,可能落后于 Redis,续跑会从 Redis 认领本 run 的冻结值与已切换值;db 源下从**库**认领(mapping Redis 丢了也认得出),认领之前先把库值发布到 mapping Redis 并 WAITAOF 确认(§15.3)。
- **放弃**:见 §14.3。

**搬完之后**:
- S 里的行成为冷副本(P-1),在线路径不读不写。
- 撤回 = 反向搬库(S、T 互换),R3 会覆盖对面的冷副本。
- 缓存不删:T 的内容就是 S 在冻结时刻的内容。
- 搬库清单(`kind=relocate`)与合服清单一样**永久保留**:逐人 `final` 是映射丢失时重放的依据(§9.2)。

### 14.3 放弃搬库 `merge-zone-relocate-abort`

把一次搬库里仍处于冻结的玩家 CAS 回稳定态 `"{S}:{v}"`。原本无记录的玩家解冻后留下 `"{S}:1"`,有效落点与冻结前相同。放弃之后该清单**不能再续跑**。

```powershell
# 先确认那次 relocate 进程已经退出:两者会改写同一份清单
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-relocate-abort -MergeManifestPath <relocate 清单> <ROUTE_ARGS> -DryRun
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-relocate-abort -MergeManifestPath <relocate 清单> <ROUTE_ARGS>
# <ROUTE_ARGS> 与那次 relocate 同一份;db 源下 dry-run 只读库、不发布
```

- 中途失败直接重跑,解冻是幂等的。
- 有人的记录已不是本次的冻结值时,不动他,报数量并 exit 1,需要人工看。

### 14.4 落点库审计 / 退役检查 `merge-zone-storage-audit`

只读。报告有效落点仍为 N 的玩家,以及 N 库里的冷副本、无主行、畸形行,并给出能否退役的结论。

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-storage-audit -MergeAuditStorage <N> <ROUTE_ARGS>
# db 源下映射一律读全局库 player_route(决定库能否退役,不按缓存下结论);库与 mapping Redis 不成对 = exit 2
```

```
=== Storage audit for storage N (route-source=<db|redis> mapping=<addr> db=0) — read-only ===
route mapping is read from <db|redis>
storage N: effective=E (by_record=… incl. frozen=…, by_home=…) | player_database rows=… (live=… cold_copies=… unmapped=… unreadable=…) | malformed_records=…
RETIREMENT CHECK: nothing routes to storage N any more; its R rows are cold copies.
```

- 退出码:查成为 0(结论看最后一行),基础设施失败为 2(`Storage audit could not complete`)。
- **退役 `zone_N_db` / 落点库的前提**:结论为 `RETIREMENT CHECK: nothing routes to storage N`,且 `unmapped` 与 `unreadable` 都为 0,且全库 `malformed_records` 为 0。任何一条不满足都 `NOT SAFE`,逐条列出原因。
- db 源(`-MergeZoneRouteSource db`)下映射一律读全局库 `player_route`,`malformed_records` 的范围是「`storage_id = N`、或 `home_zone = N` 且无 `storage_id`、但落点列不自洽」的行(库里 `storage_id` 是一列,指向别处的行与 N 无关);redis 源下仍是全 mapping 里解不出的记录。**db_first 环境退役一个库只认 db 源的结论**:缓存可能落后或丢键,按它说「没人指向」不可信。
- 只看锚表 `player_database`。

### 14.5 预建落点库(含 Phase 2 全局库)

按需打开**从不建库**,新落点库必须预建:

```powershell
cd go/db
$env:DB_ALLOWED_DATABASES = "player_store_1000000_db"     # -create-database 只认外部白名单
go run ./cmd/migrate -f <db.yaml 副本> -storage-id 1000000 -command plan
go run ./cmd/migrate -f <db.yaml 副本> -storage-id 1000000 -command up -create-database
```

- `-storage-id N`:库名 = `placement.StoreDBName(N)`,覆盖按 ZoneId 推导。`N=0` 保持旧行为。
- 放行规则与 go/db 业务服务相同(白名单或家族);但**建库只认外部白名单**。外部白名单为空且不是 warn 档时拒绝。
- dev(warn 档)下期望日志 `target database=player_store_1000000_db storage=1000000 admitted_by=family`。
- 退出码口径同 §4.4(exit 4 = 成功但有 NEEDS-REVIEW)。

**Phase 2 的运维路径**:
1. 建全局库;
2. login `PinOnCreate=true`、`NewPlayerStorageId=1000000`,注意 §9.2 末尾那类玩家的不可恢复风险;
3. 按 zone 分批 `merge-zone-relocate -MergeRelocateSourceStorage <N> -MergeRelocateTargetStorage 1000000`;
4. `merge-zone-storage-audit -MergeAuditStorage <N>` 为可退役后下线 `zone_N_db`。

**生产切换仍受 TiDB 决策 §D8 卡住**(应用级回档未通电)。

---

## 15. 第 2 批之后的合服 / 搬库(全局库为映射真源)

> **状态**:D1 第 2 批(规格 [spec_D1_batch1b2_2_3.md](../handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md) §6、§13.3),**代码已落码,未编译、未测试**(AGENTS.md §10.1,待 Codex 验证)。适用于 data_service 已切 `RouteStore.Mode=db_first` 的环境,即提案 §13.4 的 **P4 之后**。P4 之前(全部 data_service 仍是 `Mode=redis`)照旧走 redis 源,本节只有 15.1 的必填参数与 15.5 的禁令适用。

### 15.1 映射来源 `<ROUTE_ARGS>`:必填、没有缺省值、不从 yaml 推断

除 `merge-zone-capability-check`(只读能力标记)之外,**每个** merge-zone 命令都必须带:

| 环境 | `<ROUTE_ARGS>` |
|---|---|
| data_service 已是 `RouteStore.Mode=db_first`(P2 之后) | `-MergeZoneRouteSource db -MergeZoneRouteDbDsn $env:MERGE_ZONE_ROUTE_DB_DSN`;redis-ha 档再加 `-MergeZoneWaitAofReplicas 1` |
| 全部 data_service 仍是 `Mode=redis` | `-MergeZoneRouteSource redis`(不带 DSN,带了工具拒绝) |

- 缺了 dev_tools 在调用工具之前点名拒绝;直接跑二进制时工具同样拒绝(`-route-source is required and has no default`)。**不推断**:本机 `data_service.yaml` 与线上部署可以是两种模式,猜错的一边会写错库 —— db_first 环境里直写 Redis 会写出库不知道的缓存值,redis 环境里写库则缓存与读方根本不认。
- DSN 指全局库(dev `testdb`,K8s `mmorpg_global`),含口令:放环境变量再引用,不写进命令历史与工单。工具把会话钉成 READ-COMMITTED、`innodb_lock_wait_timeout=1`,开写前跑与 data_service 同判据的探针(隔离级别、TiDB 悲观事务、`route_meta` 行、`chk_route_shard` 生效且模数 64;探针在必然回滚的事务里试插行,不留数据)。
- WAITAOF:`-MergeZoneWaitAofLocal`(缺省 1)、`-MergeZoneWaitAofReplicas`(缺省 0,redis-ha 写 1)、`-MergeZoneWaitAofTimeoutMs`(缺省 1500,上限 2000)。mapping Redis 必须 `appendonly yes` 且 ≥ 7.2,否则第一批发布就以 `WAITAOF … rejected` 失败。
- **成对核对**:开写前工具在同一个 mapping Redis 上读 `mapping:route:watermark`(对账 leader 维护的水位键)。db 源要求它存在、可解码、世代等于 `-MergeZoneRouteDbDsn` 所指库的 `route_meta.generation`;redis 源见到它就拒绝。只读的 audit / storage-audit 同样核对,不成对报 INFRA / exit 2。

### 15.2 成对核对或 `verify:route_cache` 不过怎么办

| 报错 | 含义 | 怎么办 |
|---|---|---|
| `mapping:route:watermark is missing`(db 源) | 这个 mapping Redis 上没有 db_first 的对账 leader 在维护:data_service 还没切 db_first,或 `-MergeMappingRedisAddr` 指到了别的实例,或 leader 刚起 | 核对 data_service 的 `RouteStore.Mode` 与 `data_service_route_reconcile_leader`(全服恰好一个 1);在**同一个**地址上 `GET mapping:route:watermark`;等 leader 一拍再试。**不要手工 SET 这把键** |
| `generation … != route_meta.generation` | Redis 与 DSN 不是一对:DSN 指错库,或 mapping 地址指错实例 | 改参数。**不要手工改水位键**:它是库回档检测(M3)的依据 |
| `… exists on this mapping Redis`(redis 源) | 这个 Redis 已归 db_first 管 | 改用 db 源。**不要 DEL 水位键**(那等于关掉回档检测) |
| `has a newer format version` | 水位键是更新版本写的 | 换新版 merge_zone |
| `verify:route_cache` block | 清单玩家的缓存与库不一致(发布丢了、Redis 切主 / 从旧快照恢复、或有绕过脚本的写者) | **不开 dst**。等 leader 增量推送一轮后重跑 `-VerifyMerged`;仍不一致就用同一份清单重跑写这批的那一步(发布幂等,已提交的行走「已是目标」分支重新发布),再验。查 leader 的推送冲突计数;**不要手改 Redis** |

### 15.3 写入顺序与「发布确认不了」

- 每批:库事务(先确认 `route_meta.writable=1`,再锁本次相关 zone 的 63 号围栏片并核对 run_id 与库时钟,再按聚簇键锁玩家行)→ COMMIT → 同一连接 pipeline 发布 + `WAITAOF` → **全员确认之后**才算这一批完成、才推进依赖步骤。
- 任一批发布确认不了(WAITAOF 份数不足、连不上、缓存冲突):工具中止、退出非 0,报文写明「committed … NOT confirmed」。库里已提交的状态被读方晚看到是安全的(提交即安全,规格 §6.3)。处置:修好 mapping Redis 后**用同一条命令、同一份清单重跑**;已提交的行走幂等分支重新发布。
- `route_meta.writable != 1`:映射写者被冻结(全局库回档恢复流程,§9.4),所有写都被拒;等 unfreeze 之后再跑,不要绕过。
- 搬库(§14.2):冻结 / 切换 / 解冻都是「库先提交 → 发布确认 → 才算写成」。
  - R1 冻结已提交、发布没确认:引擎停在 R1、**不进 R2**(R2 等的是 go/db 看到冻结,而 go/db 看的是缓存)。续跑先把库里的冻结发布并 WAITAOF 确认 —— 缓存显示已冻结也照样再确认一次 —— 之后才等锁、拷贝。
  - mapping Redis 整体丢失(FLUSHALL、空实例重启):续跑按库里 `player_route.frozen_run_id` 认领本次冻结的人(清单落后也认得出),先补发布再继续;不需要先等 leader 预热。
  - R5 切换已提交、发布没确认:缓存停在 R1 已确认的冻结态,go/db 延后写,安全;续跑认领已切换并补发布。
  - dry-run(含 relocate-abort 的 dry-run)只读库、不发布、不写任何东西。

### 15.4 中止之后怎么续跑:围栏保留(`-MergeZoneTakeOverFence`)与围栏失权

先看中止报文属于哪一种:有 `LEFT IN PLACE` 且没有 `merge fence lost` = **围栏保留**(下面第一段);有 `本次运行已失去合服围栏` / `merge fence lost` = **围栏失权**(第二段)。两种都不要手工 DEL `merge:in_progress:*`(§3)。

**围栏保留(写到一半中止,库围栏故意留在原 run_id 上)**:每次运行的 run_id 都是新生成的,原样重跑会被库拒绝(`merge fence is held by another run`),直到库时钟过了 `expires_at_ms`。按报文里的续跑步骤:

1. 按报文原因排除故障(锁冲突查 `SHOW ENGINE INNODB STATUS` 与 `information_schema.innodb_trx` 的长事务;Redis 失败先恢复 mapping Redis 连通)。
2. 确认那次运行的 merge_zone 进程已经不在(本机 / 跳板机 / Job 都查)。**对一个还在运行的 run_id 接手是错的**:旧进程的下一批映射写入会被库拒绝,但它不碰映射的写(拷行、blob、帮会、交易、榜单、热状态、公告、清单文件)只看本地判据,要到它下一次续期失败才停 —— db 源续期间隔是 `min(ttl/3, 30s)`,所以最长约 30 秒与新进程交错写、互相覆盖同一份清单文件。
3. 只读核对库里的持有者与报文一致:`SELECT zone_id, run_id, expires_at_ms, fence_epoch FROM zone_merge_fence WHERE zone_id IN (<SRC>, <DST>) AND shard = 63`。
4. 用**原命令**(同一份 `-MergeManifestPath`、同一份 `<ROUTE_ARGS>`)加 `-MergeZoneTakeOverFence <报文里的 run_id>` 重跑;撤销中止的同理,`merge-zone-unmerge` 加同一个开关:

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeSourceZone <SRC> -MergeTargetZone <DST> `
  -MergeManifestPath <原清单> -MergeDbCapabilityZones <同原命令> <ROUTE_ARGS> `
  -MergeZoneTakeOverFence <run_id>
```

- 工具在**一个**库事务里把两区全部 64 片从旧 run_id 接到本次 run_id(`fence_epoch + 1`),两区在库里一刻不解封;镜像按更大的 `fence_epoch` 覆盖旧值并 WAITAOF 确认之后才继续。日志 `merge fence taken over in zone_merge_fence from run_id=…`。
- 第三个 run_id 持有任何一片(有人在跑另一次合服 / 撤销)仍拒绝,**停下来查清楚**。库里那次围栏已过期 / 已释放时,这个开关不起作用,按普通获取处理。
- **接手核对**(2026-10-22 补记,未经运行验证):接手事务在库里逐片核对被接手的分片 —— `src_zone` / `dst_zone` 必须与本次相同(写错 `-MergeSourceZone` / `-MergeTargetZone` 时若照样接手,只有点对的那一区被接走,另一区挂在已死的运行上、过期后在半截状态下解封);`holder_json` 里记着的 `manifest_path` 必须与本次 `-MergeManifestPath` 相同(规整后比较;续跑落到另一份清单会被当成首跑,收集出漏人的清单);每个 zone 要么 64 片全在那个 run_id 名下、要么一片都不在(已过期,按普通获取)。任一不满足报 `route store: invalid argument` 并写明库里与本次的值,事务不写,旧围栏原样保留:改正参数(多半是补上原来的 `-MergeManifestPath`)再接手。不比 purpose:合服中止后改跑 `merge-zone-unmerge` 照样能接手。
- **接手本身失败时围栏不释放**:接手事务结果不明(提交回包丢了),或接手之后镜像 `WAITAOF` 确认不了(上一次正是 Redis 故障中止时很常见),工具**不释放**库围栏、非零退出 —— 释放就等于在半合服 / 半撤销状态下解封。报文写明围栏此刻归谁:镜像没确认时归**本次**的新 run_id(报文带 `fence_epoch`),修好 mapping Redis 后对这个新 run_id 再接手一次;结果不明时是旧 / 新两个之一,先按第 3 步只读查 `shard = 63` 再对库里那个 run_id 接手。不要等它过期,也不要 DEL 镜像。
- **合服续跑不带这个开关时同一口径**(2026-10-22 补记,未经运行验证):上一次的围栏已过期 / 已释放、本次以新 run_id 普通获取,但清单已存在且步骤 7 未标记完成(两区可能处在半合服状态)—— 获取之后镜像确认不了、获取事务结果不明,同样**不释放**、非零退出,报文以「续跑」说明并给出本次 run_id 与 `-take-over-fence <本次 run_id>`;结果不明时查 `shard = 63`:是本次就接手,没有生效的围栏就用原命令直接重跑。首跑(没有清单)照旧先清库、报 `this run wrote nothing`。撤销在立围栏之前判断不了半撤销,不走这条(只有带开关时才保留)。
- 带 `-MergeZoneTakeOverFence` 的运行,在清单落盘之前的任何拒绝(续跑校验、守卫、P2~P7、公会重名、落点扫描、合走标记、清单写盘)一律保留围栏(`LEFT IN PLACE`),与清单在不在、是否标记了步骤 7 都无关;撤销同理按半撤销处理。
- db 源撤销的 7'(删公告标记)、4'(拿 guild_rank 维护锁、还原 ZSET)、1'(删目标库行)失败同样打 `LEFT IN PLACE` 报文并给出接手命令(之前是一行原始错误、围栏挂到过期)。
- 不带这个开关重跑撞上残留围栏时,报错(`merge fence is held by another run`)会直接提示 `-take-over-fence <run_id>` 与同一份 `-manifest-path`:仍须先做第 2、3 步,不要见到提示就接手。
- 映射来源:清单记录 `route_source=db` 的合服,续跑与撤销都必须 `-MergeZoneRouteSource db`;用 redis 源续跑或撤销在立围栏之前就被拒(库里留着的围栏不受影响)。
- 适用范围由工具判:只 `merge-zone` / `merge-zone-unmerge`、只 `-MergeZoneRouteSource db`、值必须是合法 run_id 且不同于本次;其余命令给了即拒绝(`-take-over-fence only applies to …`)。redis 源仍按 §3 ③ 的 GET / DEL 流程。
- `-DryRun` 加这个开关只读预判,日志 `[DRY-RUN] would take over the merge fence of zone …`,不写库、不写镜像。

**围栏失权(续期确定失败或本地截止已过)**:

- 续期判据(规格 §13.3 第 4 条):本地截止 = 上次续期成功 + TTL − 60s;续期改到的行数少于「64 × zone 数」= **确定失权**。失权或超过本地截止后,工具停止**一切**写步骤(含不碰映射的 player_blob / guild / trade),退出非 0,报文含 `merge fence lost`。续期遇到 1205 / 网络错误只 WARN 并退避重试,不算失权;每批事务里的「run_id 是本次 ∧ 库时钟未过期」复核才是权威判据。
- 处置(以工具实际报文为准):
  1. 确认那个 merge_zone 进程已经退出。
  2. 查库里的围栏(只读):`SELECT zone_id, run_id, expires_at_ms, fence_epoch FROM zone_merge_fence WHERE zone_id IN (<SRC>, <DST>) AND shard = 63`。
  3. 被**另一个** run_id 持有且未过期:有人在跑另一次合服 / 撤销,**停下来查清楚**,不要续跑,**也不要对它用 `-MergeZoneTakeOverFence`**(失权报文因此不给接手指引)。
  4. 已过期或已释放、且没有别人持有:用**同一条命令、同一份 `-MergeManifestPath`**(同一份 `<ROUTE_ARGS>`,不加 `-MergeZoneTakeOverFence`)重跑。工具以新的 run_id 重新拿围栏(`fence_epoch + 1`),已提交的批次走幂等分支重新发布,未完成的批次接着做。
  5. 失权期间 mapping Redis 里的围栏镜像可能已过期消失:续跑重新拿围栏后先写镜像并 WAITAOF,才收集 / 推进,读镜像的 scene_manager / guild 随之重新封锁。

### 15.5 P4 之后禁用旧二进制

- 第 2 批之前的 merge_zone 直写 mapping Redis、不碰 `player:maprev`、不写库;对账 leader 按 `updated_at_ms` 扫库,**看不见**它写的东西,缓存与库会静默分叉(提案 §8.3 第 1 条)。它写的围栏镜像不带 `fence_epoch`,leader 记 `foreign_fence` 并告警、但不删。
- 规矩:P4 起**只经 `dev_tools.ps1` 跑当前 HEAD 编出的 merge_zone**;清掉本机与跳板机上的旧编译产物。经新 dev_tools 跑旧二进制会以 `flag provided but not defined: -route-source` 失败 —— 这是 dev_tools 这条路上唯一的机械防线;直接手跑旧二进制没有防线,只能靠纪律与 `foreign_fence` 告警。
- 误跑了旧二进制:立刻停,不开服;用新二进制跑 `merge-zone-audit -VerifyMerged <ROUTE_ARGS>`,`verify:route_cache` 会列出分叉的玩家。库仍是旧二进制运行之前的状态(它没写库):**不要手工改 Redis 去「对齐」**,升级给负责人,按提案 §8.3 决定是用新二进制按库重做,还是把旧二进制的结论补进库。

## 修订历史

- **2026-10-19(D1 收尾:库围栏续跑接手;代码未编译、未测试,待 Codex 验证)**:dev_tools 新增 `-MergeZoneTakeOverFence <run_id>`(转 merge_zone 的 `-take-over-fence`)。§3「谁清」③ 与「手动清理」写明 GET / DEL 镜像只适用于 redis 源,db 源改用该开关续跑(或撤销、或等库时钟过期),不手工改镜像与 `zone_merge_fence`;§15.4 改为「围栏保留 / 围栏失权」两段,前者给出接手步骤与命令,后者更正「以清单里的 run_id 重新拿围栏」为「以新的 run_id」;§10.1 补 db 源撤销中止的续跑一行,并把「旧二进制撤销」的引用改指 §15.5;§13 加一行。
- **2026-10-19(映射持久化第 2 批:搬库库接缝、R8 读方、`verify:route_cache`、dev_tools 必填转发;代码未编译、未测试,待 Codex 验证)**:merge-zone 系列(capability-check 除外)的命令一律补 `<ROUTE_ARGS>`(§4、§7.1、§7.2、§8 Step 4 / 5、§10.1、§13、§14.1–§14.4);§0 加第 10 条;§8 Step 5 断言表加 `verify:route_cache` 与库里围栏复核;§14.2 续跑说明补「db 源从库认领」;§14.4 补 db 源的 `malformed_records` 口径;新增 §15(必填映射来源与成对核对、核对失败处置、写入顺序与发布确认不了、续期失权中止后的续跑、P4 之后禁用旧二进制);§13 加 `k8s-route-tool` 一行。
- **2026-10-18(映射持久化第 1b-1 批修复评审,代码未编译、未测试)**:§9.4 退出码表之后加更正注:redis 模式下误跑 / 演练的 `freeze` 用 `unfreeze -confirm-audited -cache-was-not-verified-acknowledged` 撤销(不连 mapping Redis、最低水位 0),不要手写 `UPDATE route_meta`;恢复流程第 0 步加更正注:db_first 下回档步骤 3 仍须清掉本 zone 的玩家缓存,只能选择性清、保留全局键,载体登记为第 1b-3 批前置,在那之前 db_first 下 K8s 的 zone 回档不可执行;第 3 步补「只有落点」的孤儿行(`missing_in_redis`,不被 `-frozen-recovery` 豁免)的来历与处置。
- **2026-10-18(映射持久化第 1b-1 批同步,代码未编译、未测试)**:§9.3「在哪跑」、「db_first 下 Redis 整体丢失」与 tidb-prepare 的 K8s 一条加更正注(入口已并进主程序 `-route-tool`);新增 §9.4:K8s 上默认用一次性 Job / Pod 跑 route-tool(exec 只限 dry-run 或小库,512Mi 内存风险)、`freeze` / `reconcile-from-cache` / `audit -frozen-recovery` / `unfreeze` 的退出码、全局库回档后的恢复流程(没有可信就绪标记时 unfreeze 拒绝,先按 §9.2 清单核对再显式确认)。设计见 [player-route-mapping-durability-proposal.md](../design/player-route-mapping-durability-proposal.md) §11.2「第 1b-1 批」。
- **2026-10-16(存盘管线 ③ 消费组按 zone 拆分与 ④ 按分区重试键同步;代码已落码,未编译、未测试,待 Codex 验证)**:§2.3 消费组改为「zone 组 `db_rpc_consumer_group_z{zone}` + legacy 候选,只认唯一权威组,两组都有位点先 `-FinalizeHandoff`」,dev_tools 不再缺省转发 `-kafka-group`(`-MergeKafkaGroup` 只用于显式覆盖,世代号仍现读);§5 P3 / P4 两行、§7.1 `kafka_db_task_queues` 行、§8 Step 2 命令块(describe 改 zone 组,补 `kafka:retry:queue:db_task_zone_<src>:p*` 的 SCAN 与 LLEN)、§12「已闭合」一行、§13 `-MergeKafkaGroup` / `-MergeKafkaTopicGeneration` 两行同步。设计见 [db-consumer-group-per-zone.md](../design/db-consumer-group-per-zone.md) §7。
- **2026-10-12(映射持久化第 1a 批热点修复 G1a 同步,代码未编译、未测试)**:§9.3「重跑与恢复」的恢复 SQL 改为 `route_meta` 只复位 `writable`、另复位 `route_reconcile`(水位 / 租约从 `route_meta` 拆出),并注明 TiDB 上 TRUNCATE 后要重跑 `tidb-prepare`;§9.3 末尾新增 `route_tool tidb-prepare`(TiDB-only 预切与防合并,MySQL 自动跳过;开新 zone 时在开放前、低峰跑)。设计见 [player-route-mapping-durability-proposal.md](../design/player-route-mapping-durability-proposal.md) §11.2「第 1a 批热点修复」。
- **2026-10-12(映射持久化第 1a 批同步,代码未编译、未测试)**:§3 围栏消费者表:`RegisterPlayerZone` 补 db_first 下由发布脚本复核围栏、库里已提交的行会留下;`RemapHomeZoneForMerge` 改为「已停用」。新增 §9.3 `route_tool` 导入与审计(P1 步骤、退出码、只能检测合服围栏、`db_only_unpublished` 的含义、P2 切换门禁)。§12 加一行:db_first 与 merge_zone 写模式不能共存。设计见 [player-route-mapping-durability-proposal.md](../design/player-route-mapping-durability-proposal.md) §11.2。同日评审修复:§9.3 写清 maprev 不是单独扫描的一族;import 结尾加 `merge:merged_into` 快照比对(窗口内跑完的合服也退出码 3),「原样重跑」改为按退出码分的「重跑与恢复」(结尾退出码 3 要在 P1 清表后从头导入);db_first 下真导入要显式 `-allow-db-first`;audit 方向 B 补比方向 A 之后提交的行;P2 门禁补 WAITAOF 启动探针与「发布未确认率」要连 `error` 一起看。
- **2026-09-28 v3.2(`-MergeDbCapabilityZones` 去缺省,负责人决定;代码未编译、未测试)**:
  - `-db-capability-zones` / `-MergeDbCapabilityZones` 不再有 `src,dst` 缺省值,`src` / `dst` 记号删除。pin 合服、pin 清单的撤销必填(可 `none`);copy 合服只在清单玩家已有落点记录时要求;搬库必填且不接受 `none`(§5、§5.1、§10.1、§13、§14.2)。
  - 新增只读命令 `merge-zone-capability-check`(exit 0 / 1 / 2)。§8 Step 6 zone-up dst 之后、Step 7 `/open` 之前必须 exit 0,否则不开服、先换 dst 的 go/db;撤销后两区开服前同样核对。
  - §8 Step 4 的 dry-run / apply 与 §10.1 撤销命令写明「仍在运行的 zone 或 none」;删掉「只有两个 zone 时临时拉起 dst 的 go/db」的变通(改写 `none`);§12 对应一行标为已修。
- **2026-09-28 v3.1(同批复核修复同步,代码仍未编译、未测试)**:
  - **围栏**:续跑在清单落盘前被拒、撤销在半撤销状态下被 P / S 拒,都改为保留围栏(§3、§5、§10.1);§5 步骤表加 R2(续跑校验挪到围栏之下)与 X(合走标记前置检查)。
  - **能力标记**:改为 90s TTL + 30s 心跳。§5.1 写明 T-0 的 `-MergeDbCapabilityZones` 不含 src / dst,§8 Step 6 在开服前核对 dst 标记;§6.3 加 `TTL` 核对与发布完成要求;§9.2 第 5 步不再需要重启补写;§14 红线补发布 / 回退纪律与 `Required` 打开后的回退禁令。
  - **验证**:`-VerifyMerged` 的帮会两行在两个跳过开关同时给出时报 SKIPPED,删掉 §2.2 / §8 Step 5 的 INFRA 人工判读与 §12 那一行。
  - **步骤 4**:源榜为空不写、目标榜 `ZADD NX`、清单加 `rank_members_unwritten`,日志与清单详情改为 `source_gone`。
  - **撤销**:S 段对「落点记录 ≠ src」的人要求 src 能力标记(§10.1)。
  - **其他**:搬库冻结的重排日志降为 DEBUG(§14.2);§13 参数表补 relocate-abort 的可选转发与退出码说明。
- **2026-09-28 v3(玩家存储落点 + 合服工具缺口修复,同批代码未编译)**:
  - **步骤顺序**:§5 按新顺序重写:P1 → R → C → F → 0 → G → P2~P7 → N → S → M → 1~7。围栏提前到收集之前;清单落盘前的拒绝释放围栏,落盘后的失败保留围栏(§3 表改「谁清」);新增 P7 组队门禁与解散前置(§8 Step 1);新增第四个围栏消费者 scene_manager(A16,合服窗口拒绝进场)。
  - **pin / copy 模式**:新增 §5.1,默认 pin;能力标记前置;pin 源区库不能退役;多 data Redis 集群只能 copy。
  - **T-1 / T-0 口径**:§7 改为「T-1 只做与在线无关的检查,dry-run 预期停在 P2」;N 改取自 T-0 zone-down 之后的第一次 dry-run(§8 Step 4.1)。§8 Step 2 改为先停 scene、等 go/db 排空 db_task 再删 namespace,并写明 P3 / P4 不过时临时拉起 ZoneId=src 的 go/db。
  - **清单**:dry-run 只写 `<path>.dryrun.json` 并注明升级前清理旧文件;§9 补新字段与续跑规则;新增 §9.1 `merge:merged_into` 与 §9.2 映射丢失按清单重放。
  - **验证**:`-VerifyMerged` 必须带清单;断言表删 `verify:target_zone_rows`,加 `verify:manifest_mapping` / `verify:manifest_rows`。
  - **回滚**:§10.1 撤销补目标区门禁、Kafka 参数与 pin / copy 差异;§10.2 写明 `RollbackPlayer` 不可用(`RollbackFence` 为 nil)与手工替代。
  - **K8s 无 guild**:§2.2 写明 `-skip-guild-*` 用法与 `-VerifyMerged` 的 INFRA 判读。
  - **新章节**:§14 pin-placement / relocate / relocate-abort / storage-audit / 预建落点库。
  - **清理过时内容**:friend DB 3、DB 15 说明收拢、「dev_tools 不转发 kafka 参数」、C++ 审计 topic 常量两条旧限制、main.go 顶部旧顺序、输出样例里的 `friend=.../db3`。
- **2026-09-20(帮会二期 B3a-2)**: 新增 **§4.4 角色名口径与 `profile_component` 列前置检查**(名字全服唯一、合服不改名;两个 zone 库先跑齐 go/db 迁移)。§5 步骤 1 写明按列名拷贝与 `SCHEMA MISMATCH` 中止;§6.3 加列检查;§7.1 审计行 `player.name (n/a)` 改为 `player.name (global)`;§12 昵称一行按现状重写。
- **2026-09-08 v2**: 按实现逐条重写,使手册可**逐字执行**。相对 v1.1 的实质变化:
  - **删掉不存在的东西**:`tools/scripts/merge_zone.ps1`(从未存在,dev_tools.ps1 直接 `go run`,且必须在 `tools/merge_zone/` 目录内跑——它是独立 go module)、网关 `POST /admin/maintenance`(真接口是 `POST /admin/zones/{zoneId}/maintenance` + `X-Admin-Key`,一次一个 zone)、per-zone 的 `mysql-backup` CronJob(只在 `mmorpg-infra` 有一份)、topic `db_task_topic`(真名 `db_task_zone_{zone}[_g{gen}]`)、`mmorpg.player` 表(真表是 `zone_<N>_db.player_database`)。
  - **纠正「source namespace 保留 7 天」**:`k8s-zone-down` 直接 `kubectl delete namespace`,namespace 在 Step 2 就没了;回滚不依赖它。
  - **纠正 Redis 库地图**:mapping 恒为 **DB 0**(go-zero `RedisConf` 无 `DB` 字段,yaml 里的 `DB` 会被静默忽略;`data_service.yaml` 那行 inert 的 `DB` 已删)。历史文档里的「DB 15」一直是错的。
  - **新增 §3 围栏生命周期**、**§4 回填前置**、**§5 真实步骤顺序**、**§9 清单**、**§10 以 `merge-zone-unmerge` 为默认回滚路径**。
  - **§7.1 / §8 Step 5** 改为按真实实现列检查项:pre-merge 四条 block 级门禁现在真的能拦住;`-VerifyMerged` 现在是六条真断言(以前只换了个标题)。
  - **§13** 补全 dev_tools.ps1 的全部 merge 参数名。
- **2026-05-23 v1.1**: 现状对齐(删 5 个空跑 auditor,`player.name` 改为显式「不适用」提示)。
- **2026-05-23 v1**: 初版。


### 2026-10-26：v4 写资格与严格排空接入（实现候选，未验收）

本段记录本轮候选的接口变更及尚未通过的门禁；不是已经可以执行的发布说明。正式状态机见 [存盘围栏设计](../design/db-write-fencing-mysql.md) 的 v4 实施段。当前文件仍为隔离候选，没有覆盖仓库工作区，也没有运行生成、构建、SQL 或排空验收。

- P0 固定完整计划与初始 ACTIVE 集。P1 关闭真实 SOURCE，P3 关闭真实 RELAY；每个物理 boot 的全部既有角色完成后，才能用同 run 的永久 RETIRE 收据退役。目录版本逐次递增，E 保持不变；后继阶段按连续 RETIRE 原件折叠 P0。无 ACTIVE 写者仍可正常冻结。不能原地改 P0，不能以进程退出、队列空或取消代替完成证明。
- freeze 请求绑定已经存在的 NON_SQL_CLOSED 原收据。GrantTicket 将完整 freeze 摘要写入永久票据，冻结时经票据核对 grant 与其前序原件，避免摘要循环。超期、写入 UNKNOWN、不同 ticket 参数或不同原请求一律保持维护。旧收据认重仍核票据参数。
- merge_zone 的 apply/unmerge 必须带 maintenance-run-id、freeze-ticket-id、freeze-request-id、freeze-receipt-sha256、frozen-writer-epoch；它们绑定同一 NORMAL 已消费票据与永久 SQL_FROZEN 原件。工具不把 writable 改回 1，不接受人工 attestedLag 代替传输闭包。路由写事务和已接入的公会/交易事务在自身提交事务里锁 meta 并核原冻结。玩家行复制/删除的同事务守卫仍在追加路径提案，未经纳入与验证不能声称全部写面闭合。
- Redis 等非 SQL 维护动作还必须有本 run 的真实停写、在途完成与不可重入原件。单次 preflight 或另外一条连接长持 meta 锁不能覆盖其它连接或 Redis 的提交结果；SQL_RELEASED 前所需维护工具 owner 接线仍待明确落地。这个缺口不能用超时或运维约定冒认完成。
- RELEASE_READY 保留原恢复审计与冻结收据；R 的 route_watermark_floor_ms 固定到正式协议，在既有 reconcile 锁内以 GREATEST 与 E/writable 同事务提交。正常恢复后只接纳同 run 的新 boot，消费者真实准备完成且 SOURCE 保持关闭，最后实际 open 产生 ACK；旧 boot 不复活。
- Windows 私有文件必须由实际本地固定 NTFS、ACL 与祖先句柄校验支持。Go/Java 共用一个无特权文件 I/O owner 的提案正在等待追加范围确认；Java NIO 的 NTFS 名称或日志不能替代真实平台证明。

本轮验收源码增加了 RR 旧快照、真实 slot 锁等待和 E+2 交错后的业务 SQL 零发送断言；LOCK_TOUCH 与 SELECT_ONLY 各自需要独立真实最小权限资格原件。普通 meta/registry SELECT 可能读取事务快照，安全判断依赖锁内 CURRENT slot 状态与 E，不能把这些 SELECT 统称 CURRENT。所有运行结论须等正式生成、构建及独立夹具执行后填写。

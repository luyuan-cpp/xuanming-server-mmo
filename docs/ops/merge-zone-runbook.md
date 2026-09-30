# Merge-Zone Runbook(合服操作手册)

> **状态**: v3.2 — 2026-09-28(v3.2:`-MergeDbCapabilityZones` 去缺省 + `merge-zone-capability-check`,见修订历史)。按 [`player-storage-placement.md`](../design/player-storage-placement.md)(v2.2)同批落码的 `tools/merge_zone/` 与 `tools/scripts/dev_tools.ps1` 重写了步骤顺序、清单、验证与回滚,并新增落点运维(§14)。**这批代码全部未编译、未测试**(AGENTS.md §10.1),首次按本版执行前,先确认 PROGRESS.md「2026-09-28 玩家存储落点」条目里的验证清单已由 Codex 跑绿。
> **范围**: K8s 部署形态下,把 source zone 合并进 target zone 的端到端 ops SOP,以及玩家存储落点的钉落点 / 搬库 / 退役审计。**逐字可执行**。
> **关联**:
> - 设计:[`player-storage-placement.md`](../design/player-storage-placement.md)(落点、pin / copy 模式、搬库;§12 为本批修的工具缺口)/ [`server_merge_design.md`](../design/server_merge_design.md)(历史设计,§5.5 回滚已作废)/ [`server-merge-gap-fixes.md`](../design/server-merge-gap-fixes.md)(还开着哪些口子)
> - 工具:`tools/merge_zone/`(独立 go module)+ `tools/scripts/dev_tools.ps1`
> - 备份:[`mysql-backup-pitr-runbook.md`](mysql-backup-pitr-runbook.md)
> - 灾难回滚(整 zone 回档,**不是**合服撤销):[`tools/scripts/k8s_zone_rollback.ps1`](../../tools/scripts/k8s_zone_rollback.ps1)
>
> **谁该读**: 运维 + 主程值班。**首次合服前必须读完 §2(库地图)、§3(围栏)、§4(回填前置)、§8 Step 2(排空)**。这几节里任何一条搞错,合服都会「报告成功但什么都没做」,或者把玩家最后一次存盘丢掉。

---

## 0. TL;DR — 九件不能搞错的事

1. **第一次合服之前,`-MergeBackfillZone` 必须对 source 和 target 各跑一遍**(§4)。存量玩家历史上没有 `player:zone:{id}` 映射,没有映射的玩家**不会被合过去**。工具的守卫会拒绝(源库任何一行没有映射即拒),但拒绝发生在维护窗口里就是浪费停服时间。
2. **Redis 库号不能猜**(§2)。mapping = **DB 0**,guild = DB 2,login / shared / scene = DB 0。friend 已不占 Redis 库。写错库不会报错,只会静默无效。
3. **N 取自 T-0 zone-down 之后的第一次 dry-run**,不是 T-1。T-1 彩排时两个区还在线、还在建号,只能做与在线无关的检查(§7)。把 T-0 dry-run 打出的 `players_in_source=N` 用 `-MergeExpectedSrcPlayers N` 回传给正式跑;数字不一致工具直接拒绝。
4. **zone-down 之前先排空源区 db_task**(§8 Step 2):先停 scene,等 go/db 把最后一批存盘消费完,再删 namespace。先删 namespace,go/db 跟着没了,P3 / P4 门禁就过不去。
5. **清单(manifest)是唯一凭据**(§9)。它在第一个字节写出去**之前**落盘;续跑、撤销、`-VerifyMerged`、映射丢失后的重放都要它。dry-run 只写 `<path>.dryrun.json` 预览,**预览不能当清单用**。
6. **默认是 pin 模式**(§5.1):合服只改归属,玩家行留在源区库。**pin 合服之后源区库仍是这批玩家的真源,绝不能下线**。要退役源区库,先 `merge-zone-relocate` 把人搬走,再 `merge-zone-storage-audit` 确认无人指向(§14)。
7. **回滚的第一手段是 `merge-zone-unmerge -MergeManifestPath`**(§10),**不是**备份还原。`RollbackPlayer` 当前**不可用**(§10.2)。
8. **`k8s-zone-down` 会删掉整个 zone namespace**(`k8s_deploy.ps1::Remove-Zone` → `kubectl delete namespace mmorpg-zone-<name>`)。「保留 source namespace 7 天以备回滚」**做不到**;回滚靠清单 + infra 命名空间里的 MySQL / Redis 备份。
9. **合服窗口内,home 在 src 或 dst 的玩家从任何 zone 都进不了场**(scene_manager 回可重试码 21,A16)。窗口外 `scene_manager_enter_scene_rejected_total{reason="home_zone_merging"}` 持续非 0 = 围栏没清干净(§3)。

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
| **scene_manager** | 0 | `scene_nodes:zone:{z}:load` / `player:{id}:location` / `scene:*:zone` / `world_channels:*` / `node:zone:{z}:*` | `scene_manager_service.yaml` → `Redis` | `-MergeSceneRedisAddr` / `-MergeSceneRedisDB` |
| **player data** | 独立 | `player:{id}:*` | data_service 按 region 分的 Redis Cluster(**按 home_zone 选集群**) | `-MergeSourceDataRedis` / `-MergeTargetDataRedis`(仅多集群部署) |

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
- 消费组是 `db_rpc_consumer_group`(`go/db/etc/db.yaml` → `Kafka.GroupID`)。
- `dev_tools.ps1` 会从 `go/db/etc/db.yaml` 现读 GroupID 与 TopicGeneration,并转发 `-kafka-group` / `-kafka-topic-generation`;需要覆盖时用 `-MergeKafkaGroup` / `-MergeKafkaTopicGeneration`。**以前「必须绕过 ps1 手敲 `go run`」的变通已经不需要了。**
- 注意:`dev_tools.ps1` 里 `-RollbackKafkaTopic` 的默认值仍是旧名 `db_task_topic`,但那是 `k8s-zone-rollback` 的参数,与合服无关。

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
| **TTL** | `-timeout + 30m`,下限 1 小时。默认 `-timeout=2h` ⇒ **TTL 2h30m**。后台 goroutine 每 `ttl/3` 续期一次 |
| **谁清** | 分三种情况:<br>① **正常结束**:显式 DEL。Lua 先比对 `run_id` 再删,不会误删接手者的围栏。<br>② **首跑时,立围栏之后、清单落盘之前被拒绝**(守卫、P2~P7、公会重名、落点扫描、合走标记等):本次什么都没写,工具**正常释放围栏**后 exit 1,日志 `this run wrote nothing and its merge fence was released`。修好原因后直接重跑。<br>③ **清单落盘之后失败**(步骤 1~6、清单写盘),以及**续跑时在清单落盘之前被拒绝**(见下方说明):**围栏保留**在本次 run_id 上,文案带 `LEFT IN PLACE`,并打印续跑指引:确认没有存活的 merge_zone 进程 → `GET merge:in_progress:<SRC>` 核对 run_id 与中止日志一致 → `DEL merge:in_progress:<SRC> merge:in_progress:<DST>` → 立即用原命令(同一个 `-MergeManifestPath`)重跑。<br>TTL 是进程被 kill 时的兜底,不是正常清除手段 |
| **dry-run** | **不写**围栏,但会检查是否撞上别人的;撞上就拒绝 |

> **续跑与半撤销时的拒绝保留围栏**(2026-09-28 修复,未经运行验证):
> - **合服续跑**:清单已存在、且清单里步骤 7 `post_merge_flag` 还没标记完成时,上一次运行可能已写到一半。此时在「围栏之下、清单落盘之前」被拒绝(续跑校验 R2、守卫、P2~P7、公会重名、落点扫描、合走标记、清单写盘),工具**不释放围栏**,文案以「续跑在清单更新之前被拒」开头,带 `LEFT IN PLACE` 与本次 run_id。按 ③ 处理:修好原因 → 核对 run_id → DEL → 原命令重跑。清单已标记步骤 7 完成时照常释放(上一次已跑完全程)。
> - **撤销重跑**:立围栏后先看清单玩家里是否已有人 `player:zone == src`(上一次撤销在 5' 之后中止,半撤销)。是则 P、S 段的拒绝同样保留围栏;读映射失败也按半撤销处理。偏保守的副作用:合服当初带了 `-skip-player-mapping`,或合服在步骤 5 之前停下后改跑撤销,清单玩家都还是 src,撤销的门禁拒绝也会保留围栏,照 ③ 处理即可。
> - 判断拿到的是哪一种:看最后几行有没有 `LEFT IN PLACE`。有 = 围栏还在,按 ③;没有、且有 `merge fence was released` = 按 ②。

**谁在看这面旗**(四个消费者,契约必须字字一致):

| 消费者 | 位置 | 行为 |
|---|---|---|
| data_service `RegisterPlayerZone` | `go/data_service/internal/routing/router.go` | 目标 zone 被围栏 ⇒ 拒绝建映射(`ErrZoneMergeInProgress`);查询报错也拒绝。写入脚本开头再原子复核一次围栏(提交点复核),命中时 `player:zone` 与 `player:placement` 都不写 |
| data_service `GetPlayerHomeZone` → scene_manager `EnterScene` | `router.go::GetPlayerHomeZoneAndMergeFence` + `go/scene_manager/internal/logic/home_zone.go` | 玩家 home 被围栏 ⇒ 响应 `home_zone_merging=true`,scene_manager 回 `ErrHomeZoneMerging = 21`(可重试),在写 location / owner_epoch 之前拒绝。围栏读失败也按「合服中」处理。指标 `scene_manager_enter_scene_rejected_total{reason="home_zone_merging"}`(A16) |
| data_service `RemapHomeZoneForMerge` | 同上 + `internal/server/dataserviceserver.go` | **反过来要求围栏必须在**:source 没围栏 ⇒ `FailedPrecondition` + `ErrCodeMergeFenceMissing`,零变更。另需 `x-admin-token` 与配置的 `AdminToken` 相符(**没配 = 该 RPC 停用,不是免鉴权**) |
| guild `CreateGuild` | `go/guild/internal/logic/merge_fence.go` + `guild_logic.go::checkMergeFence` | 被围栏 ⇒ 拒绝建帮(`FailedPrecondition`);读不到围栏也拒绝(fail-closed)。**但 `guild.yaml` 的 `MergeMarkerRedis` 整段缺失 = 闸门不生效**,启动时只打一条 Info |

另外,搬库(§14.2)的冻结 CAS 与钉落点(§14.1)都会检查围栏,与合服互斥。

**开服前必查**:`merge-zone-audit -VerifyMerged` 的 `verify:merge_fence`。围栏没清干净,data_service 会永久拒绝建号,guild 会永久拒绝建帮,**这两个 zone 的玩家会永久进不了场**。
**手动清理**(仅当确认没有任何 merge_zone 进程存活):

```bash
redis-cli -h <mapping-redis> -n 0 GET merge:in_progress:<SRC>   # 先看是谁的、什么时候起的
redis-cli -h <mapping-redis> -n 0 DEL merge:in_progress:<SRC> merge:in_progress:<DST>
```

---

## 4. 前置条件:`player:zone` 回填(**第一次合服前必做,两个 zone 都要**)

**背景**:生产在 2026-09-08 之前从未在建号时写 `player:zone:{id}`(login 的 `CreatePlayer` 已修复,但只对**新号**生效)。存量玩家没有映射 ⇒ 合服收集时**根本扫不到他们** ⇒ 合服后仍留在已下线的源区。

回填的真源是 `zone_<N>_db.player_database`:有行 = 这个玩家的归属 zone 就是 N。写入用 `SET NX`,**绝不覆盖**已有值(已有值可能是上一次合服的结果,覆盖回去等于把玩家送回坟场)。

```powershell
# source 与 target 各跑一遍;先 -DryRun 看 would_create,再去掉 -DryRun 落地
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeBackfillZone <SRC_ID> -DryRun
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeBackfillZone <SRC_ID>
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeBackfillZone <DST_ID> -DryRun
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeBackfillZone <DST_ID>
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

顺序是 migrate → 重启 go/db → 重启 scene。同一条检查也适用于之后每一个加在 `player_database` 上的新列(如资产通道的 `asset_op_ledger`)。

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
| P2 | preflight | `scene_nodes:zone:{src}:load` 必须空(源区无活节点) | 同上 |
| P3 | preflight | `db_task_zone_{src}[_g{gen}]` 在 `db_rpc_consumer_group` 上的 LAG = 0 | 同上 |
| P4 | preflight | `kafka:retry/processing/dead:queue:db_task_zone_{src}` 三个列表都空 | 同上 |
| P5 | preflight | 源区玩家没有在持的 `lock:player:{id}` | 同上 |
| P6 | preflight | 源区玩家没有 `player:session:{id}` | 同上 |
| P7 | preflight | 源区玩家**不在任何活队伍里**(`team:player:{id}` 的 tid 非 0 且 `team:rec:{tid}` 仍在)。队伍记录写着每个成员的 zone_id,合服不迁移 | 同上;处置见 §8 Step 1 |
| N | guild name | 公会重名断言(冲突时一个字节都不写) | 同上 |
| S | placement | 扫清单玩家的落点记录:有冻结(搬库中)或畸形记录即拒绝;已有记录记进清单 `placement_existing`;续跑时逐人比对;**copy 模式下清单里有记录者时同样要求 `-db-capability-zones`(可 `none`)并照 C 的口径核对** | 同上 |
| X | merged_into | 查 `merge:merged_into`:源区已合进别的 zone、目标区自己已被合走、标记读失败或读不懂 ⇒ 在第一次写之前拒绝(A10)。步骤 5 已完成或带 `-skip-player-mapping` 时不查 | 同上 |
| M | manifest | **在任何写之前**落盘清单(玩家 / 公会 / ZSET 成员 / 表 / 商品 / 模式 / 已有落点)。dry-run 只写 `<path>.dryrun.json`,到此为止 | 同上 |
| 1 | pin_placement(pin) | 清单里无落点记录的玩家,逐人 CAS 钉 `player:placement = "{src}:1"`。**不拷行、不拷 blob、不删缓存** | **保留围栏**中止(§3 ③) |
| 1 | player_rows(copy) | 只对**无落点记录**的玩家:`zone_src_db.<表> → zone_dst_db.<表>` 逐表**按列名**拷贝 + 共享 DB 0 上的玩家缓存失效。有记录者计入 `placed_skipped`。续跑时与源行逐列相同、且没有目标独有行的表视为已拷;目标行被改过或目标独有行按 ID SAFETY 拒绝 | 保留围栏中止 |
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
  -MergeSourceZone <SRC_ID> -MergeTargetZone <DST_ID>
# K8s 未部署 guild 再加 -MergeSkipGuildMySql -MergeSkipGuildRank
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
| `source_scene_nodes` | `scene_nodes:zone:{src}:load` 必须空 | block | **必然 block**(在线),T-1 忽略 |
| `online_presence` | `player:session:{pid}`(**DB 0**)必须为 0 | block | **必然 block**,T-1 忽略 |
| `player_locks` | `lock:player:{id}`(mapping DB)+ `player:{id}:__lock`(data Redis,给了 `-MergeSourceDataRedis` 才查) | block | 可能 block,T-1 忽略 |
| `kafka_db_task_queues` | `kafka:retry/processing/dead:queue:db_task_zone_{src}` 三个都空 | block | 在线时有也正常;**死信队列非空要在 T-1 查清** |
| `friend` / `friend_request` | 住独占库 `mmorpg_friend`,按 player_id 索引,合服后自然存活;只看量级。库 / 表查不到算 INFRA | info | 必须不是 INFRA |
| `guild_member` | 按 player_id 索引,合服后自然存活;只看量级。库 / 表查不成或孤儿查询失败算 **INFRA**;同时给 `-MergeSkipGuildMySql -MergeSkipGuildRank` 时报 **SKIPPED(warn)** | info | 必须不是 INFRA |
| `player.name (global)` | 角色名在 data_service 全局库 `player_name` 全服唯一,合服无冲突、不改名;不查库,只出说明行(§4.4) | info | — |

T-1 的通过口径:除了上表标「T-1 忽略」的在线类 block 之外,**没有 INFRA、没有别的 block**。完整的 exit 0 要等 T-0 停服之后(§8 Step 4.1 之前可再跑一次)。

### 7.2 dry-run 合服(预期停在 P2)

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone `
  -MergeSourceZone <SRC_ID> -MergeTargetZone <DST_ID> `
  -MergeDbCapabilityZones <全部在跑的 zone,例 1,2,3> `
  -MergeKafkaConsumerGroupsCmd <path\to\kafka-consumer-groups.bat> -MergeKafkaBootstrap <broker:9092> `
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
preflight P2: ... still has ... live scene node(s) ...                ← T-1 预期的拒绝
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

1. **停源区的 gate 与所有 scene**,go 服务(尤其 db)保持运行:
   ```bash
   kubectl get deploy -n mmorpg-zone-<src>                       # 以实际列出的名字为准(scene / scene-world / scene-instance / gate…)
   kubectl scale deploy -n mmorpg-zone-<src> <gate 与各 scene deploy> --replicas=0
   ```
2. **等 go/db 把 `db_task_zone_<src>` 消费完**:
   ```bash
   <kafka-consumer-groups.sh> --bootstrap-server <broker:9092> --describe --group db_rpc_consumer_group \
     | grep db_task_zone_<src>                                   # 每个分区 LAG == 0
   redis-cli -h <shared-redis> -n 0 LLEN kafka:retry:queue:db_task_zone_<src>        # 三个都 == 0
   redis-cli -h <shared-redis> -n 0 LLEN kafka:processing:queue:db_task_zone_<src>
   redis-cli -h <shared-redis> -n 0 LLEN kafka:dead:queue:db_task_zone_<src>
   ```
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
  -MergeClearSourceHotState
# 不带 -DryRun 即 -apply(dev_tools.ps1 二选一转发,不存在「两个都不给」)
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
  -MergeManifestPath <repo>\merge_<SRC>_to_<DST>.json
# K8s 未部署 guild 加 -MergeSkipGuildMySql -MergeSkipGuildRank(guild_zone / guild_rank 两行报 SKIPPED,见下)
```

没有 `-MergeManifestPath` 时 ps1 直接报错;清单缺失或是 dry-run 预览时,工具把相关断言报 INFRA(exit 2)。

| 断言 | 判据 | 不满足 |
|---|---|---|
| `verify:mapping_src` | mapping 里 `home_zone==src` 的玩家数 == 0(兜清单外的人) | block |
| `verify:manifest_mapping` | 清单里**每一个**玩家的 `player:zone == dst`;清单人数 ≠ `-MergeExpectedSrcPlayers` 时直接 block(多半拿错了清单) | block |
| `verify:manifest_rows` | 每个清单玩家在「落点记录所指的库,无记录则 `zone_<dst>_db`」的 `player_database` 里有行。pin 模式下被钉的人查源区库,copy 模式下无记录的人查目标库。清单没记玩家行那一步完成时报 warn `NOT VERIFIED`(不算通过):copy 看 `player_rows`,pin 看 `pin_placement` | block |
| `verify:guild_zone` | `SELECT COUNT(*) FROM mmorpg_guild.guild WHERE zone_id=src` == 0 | block(同时加 `-MergeSkipGuildMySql -MergeSkipGuildRank` 时为 warn `SKIPPED`,不算通过) |
| `verify:guild_rank` | `guild_rank:zone:{src}` 不存在;且 `ZCARD guild_rank:zone:{dst}` == `COUNT(guild WHERE zone_id=dst)` | block(同上) |
| `verify:trade_listing` | `SELECT COUNT(*) FROM mmorpg_trade.trade_listing WHERE market_zone=src` == 0 | block(加 `-MergeSkipTradeMySql` 时为 warn "NOT VERIFIED",不算通过) |
| `verify:source_hot_state` | 源区 scene_manager 键已清空 | warn(不影响数据正确性) |
| `verify:merge_fence` | `merge:in_progress:{src|dst}` **都不存在** | block |

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
  - staging / prod:补 `-ReleaseProfile`(`dev_tools.ps1` 默认 dev 档),并设好 `MMORPG_GATEWAY_ADMIN_API_KEY` 等注入密钥的环境变量;dev 依赖开发口令登录时补 `-LoginDevPasswordAuth`。
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

---

## 10. 回滚

### 10.1 第一手段:`merge-zone-unmerge`(**默认路径**)

适用面:**合服刚跑完、还没开服**,发现搞错了对象。撤销前**目标区也必须处于停服态**:撤销按目标区口径跑 P2~P7,要求目标区无活节点、目标 topic 与重试队列排空、清单玩家无锁无会话无活队伍。所以 Step 6 zone-up 之后要撤销,得先把 target 按 §8 Step 2 重新排空并 zone-down。

```powershell
# 先 dry-run 看会动多少
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-unmerge `
  -MergeManifestPath <repo>\merge_<SRC>_to_<DST>.json `
  -MergeDbCapabilityZones <此刻 go/db 仍在跑的其他 zone,不含 SRC / DST;没有就写 none> `
  -MergeKafkaConsumerGroupsCmd <path> -MergeKafkaBootstrap <broker:9092> -DryRun

pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-unmerge `
  -MergeManifestPath <repo>\merge_<SRC>_to_<DST>.json `
  -MergeDbCapabilityZones <与 dry-run 同一份> `
  -MergeKafkaConsumerGroupsCmd <path> -MergeKafkaBootstrap <broker:9092>
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
5'b 共享玩家缓存失效(copy 模式)
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

撤销完成后:`k8s-zone-up` 把 source 与 target 都拉起来(2026-09-29:两区各自照传上一次部署的 `-GateRouterMode` / 集群外入口 / Ingress / `-ReleaseProfile` 参数,口径同 §8 Step 6);pin 模式清单的撤销,在两区 `/open` 之前跑 `merge-zone-capability-check -MergeDbCapabilityZones <SRC_ID>,<DST_ID>`,**必须 exit 0**,否则不开服、先把对应 zone 的 go/db 换成新版。之后再发「合服延期」公告。

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

已闭合、从本表移除的旧项:
- C++ 审计 topic 世代号改为配置(2026-09-09,`audit_topic.h`);
- C++ db_task topic 世代号改为配置(本批,`DbTaskTopicGeneration`);
- `dev_tools.ps1` 已转发 `-kafka-group` / `-kafka-topic-generation`;
- `dev_tools.ps1` 已转发帮会跳过开关与撤销的 Kafka 门禁。
- 以下三项 2026-09-28 修复,未经运行验证,修复结论以 Codex 验证为准:
  - K8s 未部署 guild 时 `-VerifyMerged` 恒 exit 2:两个跳过开关同时给出时,`verify:guild_zone` / `verify:guild_rank` 改报 SKIPPED(warn);
  - 续跑在清单落盘前被拒绝会释放围栏:改为保留围栏(§3);
  - 撤销重跑在半撤销状态下被门禁拒绝会释放围栏:同上。

---

## 13. 命令快参

| 操作 | 命令 |
|---|---|
| **回填(前置,每 zone 一次)** | `dev_tools.ps1 -Command merge-zone -MergeBackfillZone <id> [-DryRun]` |
| pre-merge 审计 | `dev_tools.ps1 -Command merge-zone-audit -MergeSourceZone <s> -MergeTargetZone <d>` |
| dry-run 合服 | `dev_tools.ps1 -Command merge-zone -MergeSourceZone <s> -MergeTargetZone <d> -MergeDbCapabilityZones <zones\|none> -MergeManifestPath <file> -DryRun` |
| 正式合服 | `dev_tools.ps1 -Command merge-zone -MergeSourceZone <s> -MergeTargetZone <d> -MergeExpectedSrcPlayers <n> -MergeDbCapabilityZones <zones\|none> -MergeManifestPath <file> -MergeKafkaConsumerGroupsCmd <p> -MergeKafkaBootstrap <b> [-MergePlayerRowsMode copy]` |
| **能力标记核对(dst zone-up 之后、开服之前)** | `dev_tools.ps1 -Command merge-zone-capability-check -MergeDbCapabilityZones <zones>`(exit 0 / 1 / 2 = present / missing / unreadable) |
| 合服后验证 | `dev_tools.ps1 -Command merge-zone-audit -MergeSourceZone <s> -MergeTargetZone <d> -MergeExpectedSrcPlayers <n> -VerifyMerged -MergeManifestPath <file>` |
| **撤销一次合服** | `dev_tools.ps1 -Command merge-zone-unmerge -MergeManifestPath <file> (-MergeKafkaConsumerGroupsCmd <p> -MergeKafkaBootstrap <b> \| -MergeAssumeKafkaDrained) -MergeDbCapabilityZones <zones\|none>(pin 清单必填) [-DryRun]` |
| 批量钉落点 | `dev_tools.ps1 -Command merge-zone-pin-placement -MergePinPlacementZone <z> [-DryRun]` |
| 搬库 | `dev_tools.ps1 -Command merge-zone-relocate -MergeRelocateSourceStorage <S> -MergeRelocateTargetStorage <T> -MergeDbCapabilityZones <全部在跑 zone,不接受 none> [-MergeRelocatePlayerIds a,b] [-MergeRelocateBatchSize n] [-MergeRelocateLockWait 150s] [-MergeManifestPath f] [-DryRun]` |
| 放弃搬库 | `dev_tools.ps1 -Command merge-zone-relocate-abort -MergeManifestPath <f> [-DryRun]` |
| 落点库审计 | `dev_tools.ps1 -Command merge-zone-storage-audit -MergeAuditStorage <id>` |
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
  -MergeKafkaGroup / -MergeKafkaTopicGeneration                  默认从 go/db/etc/db.yaml 现读
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
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-pin-placement -MergePinPlacementZone <N> -DryRun
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-pin-placement -MergePinPlacementZone <N>
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
  -MergeManifestPath <repo>\relocate_<S>_to_<T>.json -DryRun

pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-relocate `
  -MergeRelocateSourceStorage <S> -MergeRelocateTargetStorage <T> `
  -MergeDbCapabilityZones <全部在跑的 zone> `
  -MergeManifestPath <repo>\relocate_<S>_to_<T>.json
# 只搬部分人:-MergeRelocatePlayerIds a,b,c;批大小 -MergeRelocateBatchSize(默认 100);
# 等锁上限 -MergeRelocateLockWait(默认 150s,略大于 go/db 排序锁 TTL 2m)
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

**工具中途崩溃**:冻结值带 run_id 留在 Redis,这些玩家的存盘持续延后、不会丢,`frozen_deferred` 上涨。二选一:
- **续跑**:用**同一条命令、同一个 `-MergeManifestPath`** 重跑。清单每批在 R1 后、R5 后各落盘一次,可能落后于 Redis,续跑会从 Redis 认领本 run 的冻结值与已切换值。
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
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-relocate-abort -MergeManifestPath <relocate 清单> -DryRun
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-relocate-abort -MergeManifestPath <relocate 清单>
```

- 中途失败直接重跑,解冻是幂等的。
- 有人的记录已不是本次的冻结值时,不动他,报数量并 exit 1,需要人工看。

### 14.4 落点库审计 / 退役检查 `merge-zone-storage-audit`

只读。报告有效落点仍为 N 的玩家,以及 N 库里的冷副本、无主行、畸形行,并给出能否退役的结论。

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-storage-audit -MergeAuditStorage <N>
```

```
=== Storage audit for storage N (mapping=<addr> db=0) — read-only ===
storage N: effective=E (by_record=… incl. frozen=…, by_home=…) | player_database rows=… (live=… cold_copies=… unmapped=… unreadable=…) | malformed_records=…
RETIREMENT CHECK: nothing routes to storage N any more; its R rows are cold copies.
```

- 退出码:查成为 0(结论看最后一行),基础设施失败为 2(`Storage audit could not complete`)。
- **退役 `zone_N_db` / 落点库的前提**:结论为 `RETIREMENT CHECK: nothing routes to storage N`,且 `unmapped` 与 `unreadable` 都为 0,且全库 `malformed_records` 为 0。任何一条不满足都 `NOT SAFE`,逐条列出原因。
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

## 修订历史

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

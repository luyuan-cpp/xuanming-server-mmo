# Merge-Zone Runbook(合服操作手册)

> **状态**: v2 — 2026-09-08。**本版按 `tools/merge_zone/` 与 `tools/scripts/dev_tools.ps1` 的实际实现逐条核对重写**,v1/v1.1 里指向不存在的脚本 / 接口 / topic 的步骤已全部删除(删了什么见文末「修订历史」)。
> **范围**: K8s 部署形态下,把 source zone 合并进 target zone 的端到端 ops SOP。**逐字可执行**。
> **关联**:
> - 设计:[`server_merge_design.md`](../design/server_merge_design.md)(历史设计,§5.5 回滚已作废)/ [`server-merge-gap-fixes.md`](../design/server-merge-gap-fixes.md)(还开着哪些口子)
> - 工具:`tools/merge_zone/`(独立 go module)+ `tools/scripts/dev_tools.ps1`
> - 备份:[`mysql-backup-pitr-runbook.md`](mysql-backup-pitr-runbook.md)
> - 灾难回滚(整 zone 回档,**不是**合服撤销):[`tools/scripts/k8s_zone_rollback.ps1`](../../tools/scripts/k8s_zone_rollback.ps1)
>
> **谁该读**: 运维 + 主程值班。**首次合服前必须读完 §2(库地图)、§3(围栏)、§4(回填前置)**——这三节里任何一条搞错,合服都会「报告成功但什么都没做」。

---

## 0. TL;DR — 六件不能搞错的事

1. **第一次合服之前,`-MergeBackfillZone` 必须对 source 和 target 各跑一遍**(§4)。存量玩家历史上没有 `player:zone:{id}` 映射;没有映射的玩家**不会被合过去**,合服后留在一个已下线的 zone 里,登不上而且没有任何报错。工具现在会拒绝跑(空映射 / 映射不全都是硬拒绝),但拒绝发生在维护窗口里就是浪费停服时间。
2. **Redis 库号不能猜**(§2)。mapping = **DB 0**,guild = DB 2,friend = DB 3,login/shared/scene = DB 0。写错库不会报错,只会静默无效。
3. **dry-run 跑两遍**:T-1 彩排一次 + T-0 窗口最开始一次。两次的 `players_in_source` 必须**完全相同**,并且用 `-MergeExpectedSrcPlayers` 把这个数字回传给正式跑——不一致工具会直接拒绝执行(说明 zone-down 没做干净)。
4. **清单(manifest)是唯一凭据**(§9)。它在第一个字节写出去**之前**落盘。丢了它:重跑会扫到空集合并「成功」地什么都不做,撤销也无从谈起。
5. **回滚的第一手段是 `merge-zone-unmerge -MergeManifestPath`**(§10),**不是**备份还原。unmerge 只碰清单里列的对象,目标区原住民一根汗毛不碰;备份还原会把目标区在窗口后发生的一切一起抹掉。
6. **`k8s-zone-down` 会删掉整个 zone namespace**(`k8s_deploy.ps1::Remove-Zone` → `kubectl delete namespace mmorpg-zone-<name>`)。所以「保留 source namespace 7 天以备回滚」这件事**做不到**——回滚靠的是清单 + infra 命名空间里的 MySQL/Redis 备份,不是那个 namespace。

---

## 1. 资产清单(全部实地核对存在)

| 资源 | 在哪 | 用途 |
|---|---|---|
| `tools/merge_zone/`(独立 go module) | repo | 合服核心工具。`main.go` 顶部注释是步骤顺序的真源 |
| `tools/merge_zone/merge_run.go` | repo | 合服主流程(preflight → 围栏 → 清单 → 7 步写入) |
| `tools/merge_zone/audit_checks.go` | repo | 合服**前**门禁 + 合服**后** `-verify-merged` 断言 |
| `tools/merge_zone/unmerge.go` | repo | `-mode unmerge`:按清单逐对象撤销 |
| `tools/merge_zone/backfill_home_zone.go` | repo | `-backfill-home-zone`:存量 `player:zone` 回填 |
| `tools/scripts/dev_tools.ps1` | repo | ops 入口:`merge-zone` / `merge-zone-audit` / `merge-zone-unmerge` |
| `deploy/k8s/manifests/infra/mysql-backup-cronjob.yaml` | k8s(**infra namespace**) | 每日 03:17 UTC 的 mysqldump + binlog 归档;手动触发见 §8 Step 3 |
| `tools/scripts/k8s_zone_rollback.ps1` | repo | 整 zone PITR 回档(与合服撤销**不是**一回事) |
| `dev-robot-zones` | dev_tools.ps1 | 合服后烟雾测试入口 |

> **不存在的东西,别去找**:`tools/scripts/merge_zone.ps1`(从来没有过;dev_tools.ps1 直接 `go run`)、网关的 `POST /admin/maintenance`(真接口见 §8 Step 1)、per-zone 的 `mysql-backup` CronJob(它只在 infra namespace 有一份)、topic `db_task_topic`(真名见 §2)。

---

## 2. 权威库地图(2026-09-08 实地核对)

合服跨了 4 个 Redis 逻辑库 + 2 类 MySQL 库 + 1 个 Kafka topic。**库号猜错不报错,只静默无效**——这是合服最危险的失败形态。

### 2.1 Redis

| 逻辑库 | DB | 键 | 配置来源 | dev_tools.ps1 参数 |
|---|---|---|---|---|
| **mapping** | **0** | `player:zone:{id}` / `lock:player:{id}` / `merge:in_progress:{zone}` | `go/data_service/etc/data_service.yaml` → `MappingRedis` | `-MergeMappingRedisAddr` / `-MergeMappingRedisDB`(默认 `-1` = 自动) |
| **guild** | 2 | `guild_rank:zone:{z}` / `guild:v2:{id}` / `guild:v2:cache_generation:{id}` / `guild_rank:maintenance_lock` | `go/guild/etc/guild.yaml` → `RedisClient.DB` | `-MergeRedisAddr` / `-MergeRedisDB` |
| **friend** | 3 | `friend:online:{pid}` | `go/friend/etc/friend.yaml` → `RedisClient.DB` | `-MergeFriendRedisAddr` / `-MergeFriendRedisDB` |
| **login / shared** | 0 | `player_merge_notice:{pid}` / `player_force_rename:{pid}` / `player:session:{pid}` / `kafka:{retry,processing,dead}:queue:*` / `PlayerAllData:{pid}` / `{MsgType}:{pid}` | `go/login/etc/login.yaml` → `Node.RedisClient.DB`;`go/db` / `go/player_locator` 同为 0 | `-MergeNoticeRedisAddr` / `-MergeNoticeRedisDB` |
| **scene_manager** | 0 | `scene_nodes:zone:{z}:load` / `player:{id}:location` / `scene:*:zone` / `world_channels:*` / `node:zone:{z}:*` | `scene_manager_service.yaml` → `Redis` | `-MergeSceneRedisAddr` / `-MergeSceneRedisDB` |
| **player data** | 独立 | `player:{id}:*` | data_service 按 region 分的 Redis Cluster | `-MergeSourceDataRedis` / `-MergeTargetDataRedis`(仅多集群部署) |

> ⚠️ **mapping 恒为 DB 0,没有例外。** data_service 用 go-zero 的 `redis.MustNewRedis(config.MappingRedis)`,而 go-zero v1.10.0 的 `RedisConf`(`core/stores/redis/conf.go`)只有 `Host/Type/User/Pass/Tls/NonBlock/PingTimeout` ——**没有 `DB` 字段**。yaml 里写 `DB: 15` 会被反序列化直接忽略。`data_service.yaml` 里那行 inert 的 `DB` 已经删掉了,`Get-MergeMappingRedis` 的解析因此恒返回 `-1`,兜底值是 **0**。
> 历史文档里的「mapping 在 DB 15」**从来就是错的**:按 15 跑,围栏与 remap 会一起指向 data_service 从不碰的库——审计恒绿、合服报告成功却一个 key 都没改。

### 2.2 MySQL

| 库 | 内容 | 谁读 |
|---|---|---|
| `zone_<N>_db` | `player_database` 等**按 zone 分的玩家主数据表**(表清单由 `generated/data/mysql_database_table_list.json` 发现) | go/db |
| 全局库(DSN 里的 schema,默认 `mmorpg`) | `guild` / `guild_member` / `friend` / `friend_request` | go/guild、go/friend |

一个 `-MergeMySqlDsn` 必须**同时**够得着这两类库(工具会写 `zone_<src>_db.x → zone_<dst>_db.x` 的全限定名)。

### 2.3 Kafka

存盘 topic 是 **`db_task_zone_{zone}`**,`Kafka.TopicGeneration > 1` 时带后缀 `_g{gen}`(`tools/merge_zone/preflight.go::dbTaskTopic`,镜像 `go/db/internal/config.DbTaskTopicForGeneration`)。
消费组是 `db_rpc_consumer_group`(`go/db/etc/db.yaml` → `Kafka.GroupID`)。

> **没有名为 `db_task_topic` 的 topic**。`dev_tools.ps1` 里 `-RollbackKafkaTopic` 的默认值仍是那个旧名字,但那是 `k8s-zone-rollback` 的参数,与合服无关。
>
> **已知缺口**:`dev_tools.ps1` **不转发** `-kafka-group` / `-kafka-topic-generation`。如果你的环境 `TopicGeneration ≠ 1` 或改过 GroupID,`merge-zone` 会查错 topic → P3 门禁失效。这种环境必须绕过 ps1,直接在 `tools/merge_zone/` 里 `go run . -kafka-topic-generation <n> ...`。

---

## 3. 围栏(fence)生命周期 — `merge:in_progress:{zone}`

合服跑的这段时间里,别人不许往被合的两个 zone 里塞新对象。围栏就是那面旗。

| 项 | 内容 |
|---|---|
| **键** | `merge:in_progress:{zone_id}`(十进制,无 hash tag) |
| **在哪** | **mapping Redis(DB 0)**——必须和它守护的 `player:zone:*` 同库 |
| **打几把** | **两把**:source 与 target 各一。目标区在窗口里同样不能进新对象,否则 ZSET 合并与行拷贝的计数对不上 |
| **值** | JSON(`tool` / `run_id` / `source_zone` / `target_zone` / `started_at` / `expires_at_unix_ms` / `operator=host/pid` / `manifest_path`)。**读者从不解析**,纯排障用 |
| **判据** | **键存在即封锁**。不看值、不看剩余 TTL。Redis 报错一律按「封锁」处理(fail-closed) |
| **谁写** | 只有 `tools/merge_zone`(merge 与 unmerge 两种模式都写)。`SETNX`——已经有别人的围栏就**拒绝启动**并把对方的 JSON 打出来 |
| **TTL** | `-timeout + 30m`,下限 1 小时。默认 `-timeout=2h` ⇒ **TTL 2h30m**。后台 goroutine 每 `ttl/3` 续期一次 |
| **谁清** | `tools/merge_zone` 退出时(成功或失败)显式 DEL,Lua 先比对 `run_id` 再删——不会误删接手者的围栏。**TTL 只是进程被 kill 时的兜底,不是正常清除手段** |
| **dry-run** | **不写**围栏,但会检查是否撞上别人的;撞上就拒绝 |

**谁在看这面旗**(三个消费者,契约必须字字一致):

| 消费者 | 位置 | 行为 |
|---|---|---|
| data_service `RegisterPlayerZone` | `go/data_service/internal/routing/router.go` | 目标 zone 被围栏 ⇒ 拒绝建映射(`ErrZoneMergeInProgress`);查询报错也拒绝 |
| data_service `RemapHomeZoneForMerge` | 同上 + `internal/server/dataserviceserver.go` | **反过来要求围栏必须在**:source 没围栏 ⇒ `FailedPrecondition` + `ErrCodeMergeFenceMissing`,零变更。另需 `x-admin-token` 与配置的 `AdminToken` 相符(**没配 = 该 RPC 停用,不是免鉴权**) |
| guild `CreateGuild` | `go/guild/internal/logic/merge_fence.go` + `guild_logic.go::checkMergeFence` | 被围栏 ⇒ 拒绝建帮(`FailedPrecondition`);读不到围栏也拒绝(fail-closed)。**但 `guild.yaml` 的 `MergeMarkerRedis` 整段缺失 = 闸门不生效**,启动时只打一条 Info |

**开服前必查**:`merge-zone-audit -VerifyMerged` 的 `verify:merge_fence` 一条就是干这个的——围栏没清干净,data_service 会永久拒绝建号、guild 会永久拒绝建帮。
**手动清理**(仅当确认没有任何 merge_zone 进程存活):

```bash
redis-cli -h <mapping-redis> -n 0 GET merge:in_progress:<SRC>   # 先看是谁的、什么时候起的
redis-cli -h <mapping-redis> -n 0 DEL merge:in_progress:<SRC> merge:in_progress:<DST>
```

---

## 4. 前置条件:`player:zone` 回填(**第一次合服前必做,两个 zone 都要**)

**背景**:生产在 2026-09-08 之前从未在建号时写 `player:zone:{id}`(login 的 `CreatePlayer` 已修复,但只对**新号**生效)。存量玩家没有映射 ⇒ 合服按「值 == source」改写时**根本扫不到他们** ⇒ 合服后仍留在已下线的源区。

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

**没跑回填会怎样**:`merge-zone` 的守卫会拒绝执行,报文里直接点名——

```
zone_<S>_db.player_database has 1200 rows but only 830 players map to home_zone=<S>.
The 370 players without a mapping would be left behind pointing at a dead zone.
Run `-backfill-home-zone -zone <S> -apply` first, then re-run this merge
```

`-MergeAllowEmptySource` 只在「真的要合一个零玩家的空区」时才加,**不要用它绕过这条守卫**。

---

## 5. 工具实际执行的步骤顺序

顺序本身是正确性的一部分(源码:`tools/merge_zone/main.go` 顶部 + `merge_run.go`):

| # | 阶段 | 做什么 | 失败即中止? |
|---|---|---|---|
| P1 | preflight | `zone_<src>_db` / `zone_<dst>_db` 都存在;从表清单 JSON 发现玩家表 | 是 |
| 0 | collect | 扫一次 mapping 拿源区玩家 id(**或从既有清单 RESUME**)。之后所有步骤复用这一份 | 是 |
| G | guard | 0 个玩家 ⇒ 拒绝(除非 `-MergeAllowEmptySource`);源库行数 > 映射数 ⇒ 拒绝;`-MergeExpectedSrcPlayers` 对不上 ⇒ 拒绝 | 是 |
| P2 | preflight | `scene_nodes:zone:{src}:load` 必须空(源区无活节点) | 是 |
| P3 | preflight | `db_task_zone_{src}` 在 `db_rpc_consumer_group` 上的 LAG = 0 | 是 |
| P4 | preflight | `kafka:retry/processing/dead:queue:db_task_zone_{src}` 三个列表都空 | 是 |
| P5 | preflight | 源区玩家没有在持的 `lock:player:{id}` | 是 |
| P6 | preflight | 源区玩家没有 `player:session:{id}` | 是 |
| F | fence | 给 src 与 dst 各打 `merge:in_progress:{zone}`(§3) | 是 |
| M | manifest | **在任何写之前**落盘清单(玩家 id / 公会 id / ZSET 成员+分数 / 表名) | 是 |
| 1 | player_rows | `zone_src_db.<表> → zone_dst_db.<表>` 逐表拷贝 **+ 共享 DB 0 上的玩家缓存失效** | 是 |
| 2 | player_blobs | 跨 data Redis 拷 `player:{id}:*`(仅 `-MergeMigratePlayerBlobs`)。拷到的玩家数 ≠ 清单数 ⇒ **中止** | 是 |
| 3 | guild_mysql | `guild.zone_id` 改写 + `guild:v2` 缓存失效(INCR generation + DEL) | 是 |
| 4 | guild_rank | `guild_rank:zone` ZSET 合并,在 `guild_rank:maintenance_lock` 下用 MULTI/EXEC | 是 |
| 5 | player_mapping | `player:zone:{id}` 由 src 改写为 dst | 是 |
| 6 | hot_state | 清源区 scene_manager 热状态(仅 `-MergeClearSourceHotState`) | 是 |
| 7 | post_merge_flag | `player_merge_notice:{pid}` 打进 **login Redis(DB 0)** | 否,只 WARN |

**两条顺序红线**:

- **1 必须在 5 之前**。mapping 一改,玩家就被路由到目标区,而他的行还在源库。
- **5 一旦跑完,源区玩家在 mapping 里就消失了**。重新扫描会得到空集合——这正是清单存在的理由(§9)。

**P3 的可注入性**:Kafka 在开发机上不跑,但这条检查保护的是「玩家最后一次存盘有没有落库」,不能省。三选一:
- `-MergeKafkaConsumerGroupsCmd <kafka-consumer-groups.sh|.bat>` + `-MergeKafkaBootstrap <host:port>` —— **生产用法,真查**
- `-MergeAssumeKafkaDrained` —— 运维显式声明「我用别的手段确认排空了」,日志里会留一条刺眼记录,**这条声明进事故复盘**
- 两个都不给 ⇒ **拒绝执行**(默认绝不是「查不到就当 0」)

---

## 6. T-7 天(预备期)

### 6.1 公告

- 客户端弹窗 + 官网公告 + 社区通知
- **必须包含**:精确合服时间(到分钟)、合服后的服名、预计停服时长
- 避开节假日 / 大版本窗口

### 6.2 容量评估

```bash
# 按 zone 库数玩家(注意:玩家主数据是 zone_<N>_db.player_database,不是 mmorpg.player)
kubectl exec -n mmorpg-infra deploy/mysql -- \
  mysql -uroot -p<pwd> -e "SELECT COUNT(*) FROM zone_<SRC>_db.player_database;"
kubectl exec -n mmorpg-infra deploy/mysql -- \
  mysql -uroot -p<pwd> -e "SELECT COUNT(*) FROM zone_<DST>_db.player_database;"
```

判断标准:合并后总数 < target zone scene 单节点上限 × 节点数 × 0.7。**到 0.7 就先扩容 target zone**。

### 6.3 回填 + 排期登记

- **跑 §4 的回填**(两个 zone)。这一步放在 T-7 而不是窗口里,因为它可能扫出一批「有行没映射」的存量玩家,需要时间核对。
- 把窗口写进 `docs/ops/release-checklist.md`,确认不撞 release / hotfix,ops 主备与主程都在线。

---

## 7. T-1 天(彩排日)

### 7.1 pre-merge 审计

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-audit `
  -MergeSourceZone <SRC_ID> -MergeTargetZone <DST_ID>
```

输出是一张 markdown 表:`sev | resource | src_count | dst_count | unique_scope | conflicts | notes`。

**退出码是给脚本用的,三个值意义不同**:

| exit | 含义 | 怎么办 |
|---|---|---|
| 0 | 干净,无 block | 继续 |
| 1 | 至少一条 block 级发现 | **不许合服**,先解决 |
| 2 | 审计**自己没跑成**(某个库连不上 ⇒ INFRA 级) | 结论**不可信**,不要当成 1 处理;修连通性后重跑 |

**当前跑的检查项**:

| 名称 | 判据 | 级别 |
|---|---|---|
| `source_scene_nodes` | `scene_nodes:zone:{src}:load` 必须空 | block |
| `online_presence` | `friend:online:{pid}`(**DB 3**)+ `player:session:{pid}`(**DB 0**)都必须为 0 | block |
| `player_locks` | `lock:player:{id}`(mapping DB)+ `player:{id}:__lock`(data Redis,给了 `-MergeSourceDataRedis` 才查) | block |
| `kafka_db_task_queues` | `kafka:retry/processing/dead:queue:db_task_zone_{src}` 三个都空 | block |
| `friend` / `friend_request` / `guild_member` | 全局表(无 zone_id 列),按 player_id 索引,合服后自然存活;只看量级 | info |
| `player.name (n/a)` | 项目当前没有玩家昵称字段,重名冲突不存在(详见 §12) | info |

> `online_presence` 是修好的那一条:旧实现在 **mapping Redis** 上查 `friend:online:{pid}`,而这把键由 go/friend 写在 **DB 3** ⇒ 恒查不到 ⇒ 恒「全部离线 ✅」;pipeline 错误还被吞掉。**一个从设计上就不可能返回 block 的 block 级门禁,比没有门禁更危险**——现在扫描失败一律 block(INFRA)。

### 7.2 dry-run 合服

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone `
  -MergeSourceZone <SRC_ID> -MergeTargetZone <DST_ID> `
  -MergeKafkaConsumerGroupsCmd <path\to\kafka-consumer-groups.bat> -MergeKafkaBootstrap <broker:9092> `
  -DryRun
```

关键输出:

```
=== Merge zone <SRC> → <DST> (dry-run=true apply=false run_id=...) ===
    manifest=<repo>\merge_<SRC>_to_<DST>_<ts>.json timeout=2h0m0s
    redis: mapping=<addr>/db0 guild=<addr>/db2 login=<addr>/db0 friend=<addr>/db3 scene=<addr>/db0
preflight P1 OK: zone_<SRC>_db and zone_<DST>_db both exist
preflight P1 OK: player tables in zone_<SRC>_db = [player_database ...]
Mapping: N players with home_zone=<SRC> (<addr> db=0)
guard OK: zone_<SRC>_db.player_database has N rows <= N mapped players
preflight P2..P6 OK
DRY-RUN: nothing was written. Record players_in_source=N and pass it back as
-expected-src-players on the T-0 run and on -verify-merged.
```

**把 `players_in_source=N` 记下来**。T-0 正式跑要用 `-MergeExpectedSrcPlayers N`。

> dry-run **不写围栏、不写清单**——它只读。日志里那行 `manifest=...` 是「将会写到哪」,文件此刻并不存在。

### 7.3 演练备份恢复(强烈建议)

在 staging 跑一次完整 §8 + §10,确认备份能跑通、`merge-zone-unmerge` 能撤回、审计能出报告。
**注意:截至 2026-09-08,真实集群的合服演练一次都没有跑过**(见 §12)。首次生产合服之前应当补一次 staging 全流程。

---

## 8. T-0:维护窗口流程(建议 60 分钟)

### Step 1 [10 min] 公告 + 停止入口 + 踢人

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

等 5 分钟让在线玩家自然下线,再强踢剩余。检查:

```bash
kubectl get pods -n mmorpg-zone-<src>   # 仍 Running,但 active connection ≈ 0
```

### Step 2 [5 min] zone-down(source + target 都要)

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName <SRC_NAME>
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName <DST_NAME>
```

> **这条命令会 `kubectl delete namespace mmorpg-zone-<name>`**(`k8s_deploy.ps1::Remove-Zone`),即整个 zone 的 gate / scene / go 服务全部销毁。
> **共享 infra 不受影响**:MySQL / Redis / Kafka / etcd 都在 `mmorpg-infra` namespace(`deploy/k8s/manifests/infra/`),merge_zone 工具要读写它们。
> **推论**:source zone 的 namespace 在这一步就没了,§8 Step 6 之后没有「保留 7 天」这回事。

### Step 3 [5 min] 备份 MySQL + Redis

```bash
# MySQL:CronJob 在 infra namespace,只有一份,手动触发一次覆盖性备份
kubectl create job -n mmorpg-infra --from=cronjob/mysql-backup \
  mysql-backup-pre-merge-$(date +%s)
kubectl wait --for=condition=complete job/mysql-backup-pre-merge-<id> \
  -n mmorpg-infra --timeout=10m

# Redis:同步落 RDB(mapping / guild / friend / login 通常是同一实例的不同 DB)
redis-cli -h <redis> SAVE
redis-cli -h <src-data-redis> SAVE     # 多集群部署才有
redis-cli -h <dst-data-redis> SAVE
```

**这一步失败,立刻终止合服。**

### Step 4 [15 min] 第二次 dry-run + apply

#### 4.1 第二次 dry-run(必做)

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone `
  -MergeSourceZone <SRC_ID> -MergeTargetZone <DST_ID> `
  -MergeExpectedSrcPlayers <N_FROM_§7.2> `
  -MergeKafkaConsumerGroupsCmd <path> -MergeKafkaBootstrap <broker:9092> `
  -DryRun
```

`players_in_source` 与 §7.2 不一致时工具会**直接拒绝**:

```
expected <N> source players (-expected-src-players, recorded at the T-1 rehearsal)
but found <M>. The source zone changed since the rehearsal — zone-down is not complete. Abort the merge
```

排查:是否漏了某个 go 服务?是否还有 cron / robot 在写?**排查清楚再决策**。

#### 4.2 apply

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone `
  -MergeSourceZone <SRC_ID> -MergeTargetZone <DST_ID> `
  -MergeExpectedSrcPlayers <N> `
  -MergeManifestPath <repo>\merge_<SRC>_to_<DST>.json `
  -MergeKafkaConsumerGroupsCmd <path> -MergeKafkaBootstrap <broker:9092> `
  -MergeClearSourceHotState
# 不带 -DryRun 即 -apply(dev_tools.ps1 二选一转发,不存在「两个都不给」)
# 多 Redis 集群部署再加:
#   -MergeMigratePlayerBlobs -MergeSourceDataRedis <addrs> -MergeTargetDataRedis <addrs>
```

**watch 这几行**:

```
merge fence acquired: merge:in_progress:<SRC> ttl=2h30m0s run_id=...
merge fence acquired: merge:in_progress:<DST> ttl=2h30m0s run_id=...
Manifest: N players, G guilds, R rank members, tables=[...]
Manifest written to <path> BEFORE any write. Keep it: -mode unmerge needs it.
Player rows (migrated): ...
MySQL: G guild rows migrated for zone <SRC> → <DST>; G guild:v2 cache entries invalidated
Redis rank: R ZSET members migrated guild_rank:zone <SRC> → <DST>
Mapping Redis: N players matched, N home_zone values migrated  (<addr> db=0)
Post-merge flags: N notice keys migrated to <addr> db=0, 0 force-rename keys
merge fence released: merge:in_progress:<SRC>
merge fence released: merge:in_progress:<DST>
=== Done (players_in_source=N ...) ===
```

**把清单文件路径记进维护工单。** 任何 `ERROR` / `log.Fatal` → 进 §10。

### Step 5 [10 min] 合服后验证(`-VerifyMerged`)

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-audit `
  -MergeSourceZone <SRC_ID> -MergeTargetZone <DST_ID> `
  -MergeExpectedSrcPlayers <N> -VerifyMerged
```

`-VerifyMerged` 现在是**真断言**(以前只是把标题换成 "POST-MERGE VERIFICATION",一条断言都没有):

| 断言 | 判据 | 不满足 |
|---|---|---|
| `verify:mapping_src` | mapping 里 `home_zone==src` 的玩家数 == 0;且 `home_zone==dst` 的数 >= `-MergeExpectedSrcPlayers` | block |
| `verify:guild_zone` | `SELECT COUNT(*) FROM guild WHERE zone_id=src` == 0 | block |
| `verify:guild_rank` | `guild_rank:zone:{src}` 不存在;且 `ZCARD guild_rank:zone:{dst}` == `COUNT(guild WHERE zone_id=dst)` | block |
| `verify:target_zone_rows` | `zone_<dst>_db.player_database` 行数 >= `-MergeExpectedSrcPlayers` | block(没给期望值时降级为 warn,并明说这是弱断言) |
| `verify:source_hot_state` | 源区 scene_manager 键已清空 | warn(不影响数据正确性) |
| `verify:merge_fence` | `merge:in_progress:{src|dst}` **都不存在** | block |

**任何 block → 进 §10。** exit 2(INFRA)不是「通过」,是「没查成」。

### Step 6 [10 min] zone-up(只 up target)

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up `
  -ZoneName <DST_NAME> -ZoneId <DST_ID> -WaitReady
```

source zone 不再 zone-up。它的 namespace 在 Step 2 已经被删掉了(§8 Step 2),**回滚不依赖它**——回滚依赖 §9 的清单与 Step 3 的备份。

### Step 7 [5 min] 烟雾测试 + 开服

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command dev-robot-zones -Zones <DST_ID>
kubectl logs -n mmorpg-zone-<dst> deploy/scene-0 --tail=200 | grep -E "ERROR|FATAL"
```

人工验证:

- [ ] 选一个**原 source zone 的玩家**登录,确认进得去 target
- [ ] `redis-cli -n 0 GET player:zone:<pid>` 返回 `<DST_ID>`
- [ ] 该玩家的公会数据在(名字、等级、成员)
- [ ] 公会排行榜上能看到原 source 的公会
- [ ] 好友数据在
- [ ] `redis-cli -n 0 EXISTS merge:in_progress:<SRC>` / `<DST>` 都返回 0

开服:

```bash
curl -X POST https://<gateway>/admin/zones/<DST_ID>/open -H "X-Admin-Key: <admin-key>"
```

> source zone 保持维护态(或从服务器列表下架)。**不要**对 `<SRC_ID>` 调 `/open`。

---

## 9. 清单(manifest)、重跑与恢复

**清单是什么**:合服的四个写入面(zone 库玩家行 / guild MySQL / guild_rank ZSET / mapping)分布在两个 MySQL 库和三个 Redis 库上,**没有任何跨面事务**。清单是「这次到底动了谁」的唯一结构化记录。

| 项 | 内容 |
|---|---|
| **落在哪** | `dev_tools.ps1` 强制算成**仓库根的绝对路径**:`<repo>\merge_<src>_to_<dst>_<UTCts>.json`;`-MergeManifestPath` 可指定 |
| **什么时候写** | **第一次写之前**。之后每完成一步 `markStep` + 落盘一次 |
| **怎么写** | 同目录 `.tmp` + rename(原子),进程被 kill 不会留下半个 JSON |
| **内容** | `version` / `run_id`(与围栏里的同一个值)/ `source_zone` / `target_zone` / `operator` / `player_ids` / `guild_ids` / `rank_members`(成员+分数)/ `tables` / `steps` |
| **重跑** | 同一个 `-MergeManifestPath` 再跑一次:已完成的步骤按 `steps` 跳过,玩家 id **从清单读而不重新扫描**(日志打 `RESUME: ...`) |
| **不匹配** | 清单的 src/dst 与本次调用不一致 ⇒ 硬失败(复制粘贴错路径的保护) |

> **为什么重跑不能重新扫描**:remap(步骤 5)跑完后,源区玩家的 `player:zone` 已经是目标区,`collectPlayerIDsWithHomeZone` 会返回**空集合**,第二次跑会「成功」地什么都不做——把「合了一半」当成「合完了」。

**清单丢了怎么办**:`-mode unmerge` 不可用,只能走备份还原(§10.3)。所以清单必须跟着维护工单归档。

---

## 10. 回滚

### 10.1 第一手段:`merge-zone-unmerge`(**默认路径**)

适用面:**合服刚跑完、还没开服**,发现搞错了对象。

```powershell
# 先 dry-run 看会动多少
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-unmerge `
  -MergeManifestPath <repo>\merge_<SRC>_to_<DST>_<ts>.json -DryRun

pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-unmerge `
  -MergeManifestPath <repo>\merge_<SRC>_to_<DST>_<ts>.json
```

撤销顺序与合服**完全相反**——先把路由改回去,再往回搬数据,这样任何一步失败时玩家的归属都指向一个**确实有他数据**的 zone:

```
(撤销期间同样上围栏)
7'  清掉 player_merge_notice:{pid} / player_force_rename:{pid}
5'  player:zone:{id} 改回 src(只改当前值确实是 dst 的那些)
4'  guild_rank ZSET 成员按原分数 ZADD 回 src、从 dst ZREM
3'  guild.zone_id 改回 src(只改清单里的 guild_id)+ guild:v2 缓存失效
1'  删目标库里那些**与源库逐字节相同**的玩家行
```

**两条硬保证**:

- **只碰清单里的对象**。目标区原住民按构造安全。
- **1' 只删逐字节相同的行**。任何一行在合服后被改过(玩家登录过、打过一场、领过邮件)就**拒绝删除并报出来**——那时删掉的不是「拷贝」,而是合服后产生的唯一一份数据。源库的行从来没删过,所以留一份多余副本是安全的(mapping 已经指回源区,不会被读到)。
- `player:{id}:*` blob **不自动删**。mapping 指回源区后没人读它们;确认源副本无误后再手动清。

撤销完成后:`k8s-zone-up` 把 source 与 target 都拉起来,再发「合服延期」公告。

### 10.2 什么时候 unmerge 不够

- **已经开服、玩家已经在 target 里玩过**:1' 会大面积「REFUSED」,得到的是一个半撤销状态。这时应当**不要**撤销,转为单玩家修复(`RollbackPlayer`,见 [`single_player_rollback.md`](../design/single_player_rollback.md))。
- **清单丢失 / 损坏**:见 10.3。

### 10.3 最后手段:备份还原

按 [`mysql-backup-pitr-runbook.md`](mysql-backup-pitr-runbook.md) §4 把 target 完整还原到 §8 Step 3 的 dump 时间点,Redis 用同一时刻的 RDB。
**代价**:target zone 在窗口开始之后的所有写入一起丢。所以这是最后手段,不是默认路径。

### 10.4 复盘

合服失败必须在 24h 内出复盘文档 `docs/design/server-merge-postmortem-<date>.md`。

---

## 11. T+1 ~ T+7 观察期

- [ ] 每天跑一次 `merge-zone-audit -VerifyMerged -MergeExpectedSrcPlayers <N>`,确认持续一致
- [ ] 客服工单打 `merge-<src>-<dst>` tag,主程 4h 响应 SLA
- [ ] 关注「东西不见了 / 公会没了 / 好友丢了」类投诉
- [ ] 单玩家数据问题走 `RollbackPlayer` RPC
- [ ] **清单文件与审计报告归档进维护工单**(备份保留按 [`db-retention`](../design/) 现行 90 天口径)

> source zone 的 K8s namespace 在 §8 Step 2 就已删除,**没有「T+7 删 namespace」这一步**。要清的是:服务器列表里下架 source、备份归档、`zones.json` 里摘掉 source。

---

## 12. 已知限制与仍然开着的口子(2026-09-08)

| 项 | 影响 | 应对 |
|---|---|---|
| **Unity 客户端不处理 `RedirectToGate`** | 登录期跨区重定向客户端接不住 | **`HomeZone.RedirectOnEnterEnabled` 必须在生产保持 `false`**(`go/login/etc/login.yaml` 默认就是 false)。robot 已经能跟随重定向,客户端还不能 |
| **C++ 侧 Kafka 审计 topic 的代号是编译期常量** | `cpp/libs/modules/transaction_log/transaction_log_system.h` 的 `transaction_log_topic_g1` 与 `snapshot/snapshot_system.h` 的 `player_snapshot_topic_g1` 写死;Go 侧由 `Kafka.TopicGeneration` 组合 | **换代必须改 C++ 常量 + 重建镜像 + 同步 Go 配置,三件一起做**。只改 yaml 会让两端写进不同 topic |
| **`dev_tools.ps1` 不转发 `-kafka-group` / `-kafka-topic-generation`** | `TopicGeneration ≠ 1` 的环境里 P3 门禁查错 topic | 绕过 ps1,在 `tools/merge_zone/` 里直接 `go run . -kafka-topic-generation <n> ...` |
| **真实集群合服演练从未跑过** | 全部结论来自代码审查 + 单测 / 集成测试 | 首次生产合服前先在 staging 跑一次完整 §8 + §10 |
| **guild 的 `MergeMarkerRedis` 可缺失** | 整段没配 = 建帮闸门不生效,窗口内仍能在源 zone 建帮,那个公会不会被搬走 | 合服前确认 `go/guild/etc/guild.yaml` 的 `MergeMarkerRedis.Host` 指向 mapping Redis 且 `DB: 0` |
| **玩家昵称冲突检测未实现** | 项目当前**没有玩家昵称字段**(`CreatePlayerRequest` 是空 message,`AccountSimplePlayer` 只有 `player_id`,`user.display_name` 零读写),所以重名问题**不存在** | `force_rename` 全链路(proto 字段 + Redis flag + login 消费 + `stampPostMergeFlags` 的参数 seam)是 future-proof 预留,不是死代码。哪天加了昵称再实现检测 |
| **`mail` / `auction` / `chat_history` / `guild_application` 表不存在** | audit 不扫它们(也无须扫) | 对应服务上线后再补 auditor |
| **合服公告 UI 靠客户端** | 服务端已把 `player_merge_notice:{pid}` 写进 login Redis(DB 0),login 首登时消费并下发 | 客户端接上即可生效 |
| **player blob 跨集群拷贝是逐玩家流式** | 大 zone(>10w)分钟级 | source/target 同 Redis 后端时工具自动跳过这一步 |

---

## 13. 命令快参

| 操作 | 命令 |
|---|---|
| **回填(前置,每 zone 一次)** | `dev_tools.ps1 -Command merge-zone -MergeBackfillZone <id> [-DryRun]` |
| pre-merge 审计 | `dev_tools.ps1 -Command merge-zone-audit -MergeSourceZone <s> -MergeTargetZone <d>` |
| dry-run 合服 | `dev_tools.ps1 -Command merge-zone -MergeSourceZone <s> -MergeTargetZone <d> -DryRun` |
| 正式合服 | `dev_tools.ps1 -Command merge-zone -MergeSourceZone <s> -MergeTargetZone <d> -MergeExpectedSrcPlayers <n> -MergeManifestPath <file>` |
| 合服后验证 | `dev_tools.ps1 -Command merge-zone-audit -MergeSourceZone <s> -MergeTargetZone <d> -MergeExpectedSrcPlayers <n> -VerifyMerged` |
| **撤销一次合服** | `dev_tools.ps1 -Command merge-zone-unmerge -MergeManifestPath <file> [-DryRun]` |
| zone-down(**会删 namespace**) | `dev_tools.ps1 -Command k8s-zone-down -ZoneName <name>` |
| zone-up | `dev_tools.ps1 -Command k8s-zone-up -ZoneName <name> -ZoneId <id> -WaitReady` |
| 触发 MySQL 备份 | `kubectl create job -n mmorpg-infra --from=cronjob/mysql-backup mysql-backup-manual-$(date +%s)` |
| 网关置维护 / 开服 | `POST /admin/zones/<id>/maintenance` / `POST /admin/zones/<id>/open`(头 `X-Admin-Key`) |
| 整 zone PITR 回档 | `tools/scripts/k8s_zone_rollback.ps1 -ZoneName <n> -ZoneId <i> -TargetTime <iso>` |
| 查围栏 | `redis-cli -h <mapping-redis> -n 0 GET merge:in_progress:<zone>` |

**merge-zone 系列的全部 dev_tools.ps1 参数**(`Get-MergeZoneArgs` + 各分支):

```
公共(三个命令都转发):
  -MergeMySqlDsn <dsn>                  必须同时够着 zone_<N>_db 与全局库
  -MergeRedisAddr / -MergeRedisPassword / -MergeRedisDB          guild 库,默认 2
  -MergeMappingRedisAddr / -MergeMappingRedisPassword
  -MergeMappingRedisDB <n>              默认 -1 = 从 data_service.yaml 现读,兜底 0
  -MergeNoticeRedisAddr / -MergeNoticeRedisDB                    login/shared,默认 0
  -MergeFriendRedisAddr / -MergeFriendRedisDB                    默认 3
  -MergeSceneRedisAddr  / -MergeSceneRedisDB                     默认 0
  -MergeTableListJson <path>            默认 generated/data/mysql_database_table_list.json
  -MergeSourceDataRedis / -MergeTargetDataRedis / -MergeDataRedisPassword
  -MergeSourceDataRedisDB / -MergeTargetDataRedisDB
  -MergeExpectedSrcPlayers <n>          -1 = 不校验

仅 merge-zone:
  -MergeSourceZone / -MergeTargetZone / -MergeBackfillZone
  -MergeManifestPath <file>             不给则自动生成到仓库根
  -MergeMigratePlayerBlobs              多 Redis 集群部署才需要
  -MergeClearSourceHotState
  -MergeAllowEmptySource                只在真的要合空区时用
  -MergeAssumeKafkaDrained              与 -MergeKafkaConsumerGroupsCmd 二选一,必须给其一
  -MergeKafkaConsumerGroupsCmd <path> / -MergeKafkaBootstrap <host:port>

仅 merge-zone-audit:
  -VerifyMerged

仅 merge-zone-unmerge:
  -MergeManifestPath <file>             必填,文件必须存在

三个命令通用:
  -DryRun                               给了 = -dry-run,不给 = -apply(工具拒绝两者都不给)
```

---

## 修订历史

- **2026-09-08 v2**: 按实现逐条重写,使手册可**逐字执行**。相对 v1.1 的实质变化:
  - **删掉不存在的东西**:`tools/scripts/merge_zone.ps1`(从未存在,dev_tools.ps1 直接 `go run`,且必须在 `tools/merge_zone/` 目录内跑——它是独立 go module)、网关 `POST /admin/maintenance`(真接口是 `POST /admin/zones/{zoneId}/maintenance` + `X-Admin-Key`,一次一个 zone)、per-zone 的 `mysql-backup` CronJob(只在 `mmorpg-infra` 有一份)、topic `db_task_topic`(真名 `db_task_zone_{zone}[_g{gen}]`)、`mmorpg.player` 表(真表是 `zone_<N>_db.player_database`)。
  - **纠正「source namespace 保留 7 天」**:`k8s-zone-down` 直接 `kubectl delete namespace`,namespace 在 Step 2 就没了;回滚不依赖它。
  - **纠正 Redis 库地图**:mapping 恒为 **DB 0**(go-zero `RedisConf` 无 `DB` 字段,yaml 里的 `DB` 会被静默忽略;`data_service.yaml` 那行 inert 的 `DB` 已删)。历史文档里的「DB 15」一直是错的。
  - **新增 §3 围栏生命周期**、**§4 回填前置**、**§5 真实步骤顺序**、**§9 清单**、**§10 以 `merge-zone-unmerge` 为默认回滚路径**。
  - **§7.1 / §8 Step 5** 改为按真实实现列检查项:pre-merge 四条 block 级门禁现在真的能拦住;`-VerifyMerged` 现在是六条真断言(以前只换了个标题)。
  - **§13** 补全 dev_tools.ps1 的全部 merge 参数名。
- **2026-05-23 v1.1**: 现状对齐(删 5 个空跑 auditor,`player.name` 改为显式「不适用」提示)。
- **2026-05-23 v1**: 初版。

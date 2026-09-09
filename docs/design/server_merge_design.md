# 合服(Server Merge)设计文档

> **文档状态**: v2 — 2026-09-08(v1 2026-05-15)。**这是设计与历史决策文档;可逐字执行的 SOP 在 [`docs/ops/merge-zone-runbook.md`](../ops/merge-zone-runbook.md)。** 作废段落一律就地标注,不悄悄改写历史。
> **范围**: 把散落在 `tools/merge_zone/main.go`、`mmo_cross_server_architecture.md §9`、`guild_ranking_architecture.md §合服工具`、`enter-scene-zone-routing.md §50` 的合服知识收口为单一权威来源。
> **读者**: 运维、客服总监、新接手的 AI / 工程师。
> **修订(2026-08-15)**: 全区全服数据层落地后(见 [global-data-layer-tidb-decision.md](./global-data-layer-tidb-decision.md) §D7),合服从"跨库搬数据"收敛为"`RemapHomeZoneForMerge` 改逻辑归属 + 业务数据合并",`tools/merge_zone` 职责相应收缩;Phase 2 之前本文流程仍是权威。

---

## 1. 合服是什么

把 zone X(source)的所有玩家、公会、邮件、排行榜数据**逻辑上**合并到 zone Y(target),使 source 玩家从此在 target 服里游戏。**player_id 不变,角色不丢失,公会不解散**(除非重名冲突且选择解散策略)。

合服适用场景:
- 老服在线人数衰减,运营成本不划算
- 跨服活动后期收口
- 测试服 / S1/S2 服并入正式服

合服**不**适用:
- 两服版本号不同 → 应先升级到同版本再合
- 经济模型严重不一致(物价差 10x+)→ 需要先做物价校准

---

## 2. 设计前提(已经在架构里固化的)

这些是历史决策,不要再问「能不能改」:

| 前提 | 出处 | 含义 |
|---|---|---|
| **player_id 全局唯一,不编码 zone_id** | `mmo_cross_server_architecture.md §6.1`(Snowflake `[time:32][node_id:17][step:15]`)| 合服不需要重写 ID,这是一切的基础 |
| **数据持久化在 home zone 的存储** | `mmo_cross_server_architecture.md §2` 位置透明 | 合服的本质是**改 mapping**(player → home zone)+ **数据搬家**(若两 zone 不共享 Redis) |
| **`player_id → home_zone_id` 是全局 mapping** | `data_service` Router(key prefix `player:zone:`)| 改这个 mapping 就完成了路由切换 |
| **Plan A:Data Service 是数据代理层** | `mmo_cross_server_architecture.md §3` | 业务层不感知合服,只看到 player_id;改 mapping 后业务自然路由到新 zone |
| **业务层不分支「is_cross_server」** | `mmo_cross_server_architecture.md §2` 红线 | 合服后**不需要**改业务代码,只在数据层做迁移 |

**推论**:合服 = mapping 改写 + (可选)blob 跨集群拷贝 + 业务表 zone_id 改写 + 重名冲突处理。**没有业务代码迁移**。

---

## 3. 已有工具(本节内容来自 `tools/merge_zone/main.go` 实测)

> ⚠️ **2026-09-08 修订**:本节 v1(2026-05-15)描述的「3 步流程」在 2026-09 的大修中被整体替换。
> 下面 §3.1 保留 v1 原文作为**历史记录**(它解释了当年为什么这么设计),
> **当前权威的步骤顺序见 §3.1a**,可执行的 SOP 见 [`docs/ops/merge-zone-runbook.md`](../ops/merge-zone-runbook.md)。

### 3.1 Go CLI:`tools/merge_zone/main.go`(**v1 原文,已被 §3.1a 取代**)

独立 Go module(`tools/merge_zone/go.mod`),不依赖主项目。功能:

```
合服 source-zone → target-zone:

  [可选 0]  player blob 跨集群拷贝     (-migrate-player-blobs)
   └→ 仅在 source/target 不共享 player data Redis 时需要
   └→ 必须在 mapping 重映射**之前**做(否则 DataService 读新 zone 撞空)

  [步骤 1] guild MySQL zone_id 改写  (除非 -skip-guild-mysql)
   └→ UPDATE guild SET zone_id = target WHERE zone_id = source
   └→ 内置重名冲突检测:JOIN guild s ↔ guild d ON name + d.zone_id=target
   └→ 冲突的 source 公会跳过迁移,打印名单要求人工处理

  [步骤 2] guild_rank:zone:* ZSET 合并  (除非 -skip-guild-rank)
   └→ ZRANGE source key → ZADD target key + DEL source key
   └→ 同名公会的分数会按 target 既有分数保留(ZAdd 默认 NX/XX 取决于实现,详见 mergeRankingZSET)

  [步骤 3] player:zone:* mapping 重映射  (除非 -skip-player-mapping)
   └→ SCAN player:zone:* → GET → 若值=source 则 SET 为 target
```

**这段原文里已经作废的三点**(留在这里是为了让读到旧 commit 的人对得上):

1. **「玩家主数据不用搬」是错的**。玩家主数据在 `zone_<N>_db`(按 zone 分库),合服**必须**逐表拷贝
   `zone_src_db → zone_dst_db`,否则 mapping 一改,玩家在目标库里什么都没有。这是现在的步骤 1,
   也是整个流程里最关键的一步(`-skip-player-rows` 需要 `-i-know-global-player-table` 才允许,
   而那个前提要等 TiDB 全局玩家层落地才成立)。
2. **公会重名「跳过冲突继续写」已改成「中止」**。`deploy/mysql-init/guild_friend_tables.sql` 的
   `UNIQUE KEY uk_name (name)` 是**全局**唯一(不带 zone_id),所以跨 zone 同名在库层面根本不可能,
   旧的 JOIN 永远返回空。真要哪天改成 `(zone_id, name)`,正确做法是**中止**——跳过会把源区公会
   连同成员一起留在一个已下线的 zone 里。
3. **ZSET 合并从普通 pipeline 改成了 MULTI/EXEC**,并且要先拿 `guild_rank:maintenance_lock`。

### 3.1a 当前实际步骤顺序(2026-09-08,真源 = `tools/merge_zone/main.go` 顶部注释)

```
P1  preflight   zone_<src>_db / zone_<dst>_db 都存在;从表清单 JSON 发现玩家表
0   collect     扫一次 mapping 拿源区玩家 id(或从既有清单 RESUME);之后所有步骤复用这一份
G   guard       0 个玩家 / 源库行数 > 映射数 / 与 -expected-src-players 不符 → 拒绝
P2  preflight   scene_nodes:zone:{src}:load 必须空(源区无活节点)
P3  preflight   db_task_zone_{src}[_g{gen}] 在 db_rpc_consumer_group 上 LAG=0
P4  preflight   kafka:{retry,processing,dead}:queue:<topic> 三个列表都空
P5  preflight   源区玩家没有在持的 lock:player:{id}
P6  preflight   源区玩家没有 player:session:{id}
F   fence       merge:in_progress:{src} 与 {dst} 同时打标(见 §3.5)
M   manifest    **在任何写之前**落盘清单(玩家 id / 公会 id / ZSET 成员+分数 / 表名)
1   player_rows zone_src_db → zone_dst_db 逐表拷贝 + 共享 DB 0 上的玩家缓存失效   ← 最关键
2   player_blobs 跨 data Redis 拷 player:{id}:*(仅多集群;拷到的人数对不上就中止)
3   guild_mysql  guild.zone_id 改写 + guild:v2 缓存失效(INCR generation + DEL)
4   guild_rank   guild_rank:zone ZSET 合并(guild_rank:maintenance_lock + MULTI/EXEC)
5   player_mapping player:zone:{id} 改写                                        ← 必须在 1/2 之后
6   hot_state    scene_manager 源区热状态清理(可选,-clear-source-hot-state)
7   post_merge_flag player_merge_notice:{pid} 打进 **login Redis(DB 0)**
```

两条顺序红线:**1 必须在 5 之前**(mapping 一改玩家就被路由到目标区,而他的行还在源库);
**5 一旦跑完源区玩家在 mapping 里就消失了**,重新扫描会得到空集合——这正是清单存在的理由。

三种模式:`-mode merge`(默认)、`-mode audit`(只读;`-verify-merged` 切换到合服后断言)、
`-mode unmerge -manifest-path <file>`(按清单逐对象撤销)。外加独立的
`-backfill-home-zone -zone <id>`(存量 `player:zone` 回填,**第一次合服前每个 zone 各跑一遍**)。

### 3.2 PowerShell 包装

> ⚠️ **2026-09-08 更正**:v1 这里写的 `tools/scripts/merge_zone.ps1` **在仓库里从来不存在**。
> 之前的 `dev_tools.ps1` 也确实没走通过——它在仓库根跑 `go run ./tools/merge_zone`,
> 而 `tools/merge_zone` 是**独立 go module**,那条命令恒为 `go: cannot find main module`。
> 换句话说:**这个入口在 2026-09 修复之前从来没有真正执行过一次合服。**

真实入口是 `tools/scripts/dev_tools.ps1` 的三个命令,它们 `Push-Location tools\merge_zone` 之后
直接 `go run .`,并由 `Get-MergeZoneArgs` 统一转发 MySQL DSN 与**四个 Redis 库**的地址 / 库号 / 密码:

| 命令 | 作用 |
|---|---|
| `-Command merge-zone -MergeBackfillZone <id>` | 存量 `player:zone` 回填(合服前置) |
| `-Command merge-zone -MergeSourceZone <s> -MergeTargetZone <d>` | 合服本体(`-DryRun` = 预演,不给 = `-apply`) |
| `-Command merge-zone-audit ... [-VerifyMerged]` | 只读审计 / 合服后断言 |
| `-Command merge-zone-unmerge -MergeManifestPath <f>` | 按清单撤销一次合服 |

参数全表见 [`merge-zone-runbook.md §13`](../ops/merge-zone-runbook.md)。

### 3.5 Redis 库地图与合服围栏(2026-09-08 新增)

**库地图**(写错库不报错,只静默无效——这是合服最危险的失败形态):

| 逻辑库 | DB | 键 | 配置来源 |
|---|---|---|---|
| mapping | **0** | `player:zone:{id}` / `lock:player:{id}` / `merge:in_progress:{zone}` | `data_service.yaml` → `MappingRedis` |
| guild | 2 | `guild_rank:zone:{z}` / `guild:v2:{id}` / `guild_rank:maintenance_lock` | `guild.yaml` → `RedisClient.DB` |
| friend | 3 | `friend:online:{pid}` | `friend.yaml` → `RedisClient.DB` |
| login / shared | 0 | `player_merge_notice:{pid}` / `player:session:{pid}` / `kafka:*:queue:*` / `PlayerAllData:{pid}` | `login.yaml` → `Node.RedisClient.DB`;go/db、player_locator、scene_manager 同为 0 |
| player data | 独立 | `player:{id}:*` | data_service 按 region 分的 Redis Cluster |

> **mapping 恒为 DB 0,没有例外。** data_service 用 go-zero 的 `redis.MustNewRedis(MappingRedis)`,
> 而 go-zero v1.10.0 的 `RedisConf` **没有 `DB` 字段** —— yaml 里写 `DB: 15` 会被反序列化直接忽略。
> `data_service.yaml` 里那行 inert 的 `DB` 已经删除。**历史文档里的「mapping 在 DB 15」一直是错的**:
> 按 15 跑,围栏与 remap 会一起指向 data_service 从不碰的库——审计恒绿、合服报告成功却一个 key 都没改。

**围栏契约** `merge:in_progress:{zone}`(三方共享,改一处必须同步另外两处):

- **在哪**:mapping Redis(DB 0)——必须和它守护的 `player:zone:*` 同库
- **判据**:**键存在即封锁**,不解析值、不看 TTL;Redis 报错按封锁处理(fail-closed)
- **谁写**:只有 `tools/merge_zone`(`SETNX`,src 与 dst 各一把),TTL = `-timeout + 30m`(下限 1h),后台每 `ttl/3` 续期
- **谁清**:`tools/merge_zone` 退出时按 `run_id` 校验后 DEL;**TTL 只是进程被 kill 的兜底**
- **谁看**:`data_service.RegisterPlayerZone`(被围栏 ⇒ 拒绝建映射)、`guild.CreateGuild`(被围栏 ⇒ 拒绝建帮;`MergeMarkerRedis` 整段缺失 = 闸门不生效)、
  `data_service.RemapHomeZoneForMerge`(**反过来要求围栏必须在**,否则 `FailedPrecondition`)

### 3.3 联机版 RPC:`DataService/RemapHomeZoneForMerge`

`proto/data_service/data_service.proto:25` 定义的 RPC,**只做步骤 5**(mapping 重映射)。Go 实现在 `dataserviceserver.go`。

> ⚠️ **2026-09-08 修订:这个 RPC 现在有两道硬闸,v1 描述的「服务在线时也能执行」已经不成立。**
>
> 1. **必须带 `x-admin-token`**,且与 data_service 配置的 `AdminToken` 相符;
>    **`AdminToken` 没配 = 该 RPC 停用**(不是免鉴权)。不满足 ⇒ `PermissionDenied`。
> 2. **源 zone 必须已经立起 `merge:in_progress:{source}` 围栏**,否则 `FailedPrecondition` +
>    `ErrCodeMergeFenceMissing`,零变更。理由不是形式主义:那个围栏同时在挡 `RegisterPlayerZone`,
>    没有它就意味着源 zone 仍在接受新映射,本函数的 SCAN 游标扫过之后新写进来的玩家会永远留在源 zone。
>    **`dry_run` 也要求围栏**——在没有封锁的库上数出来的数不作数。
>
> **用途因此收窄为**:合服窗口内由 `tools/merge_zone` 之外的路径做 mapping 修正(排障 / 客服热处理),
> 而**不是**「不停服合服」。真正的合服走 CLI:它还要搬玩家主数据行,那件事这个 RPC 根本不做。

### 3.4 命令示例

```powershell
# 0. 前置:存量 player:zone 回填(第一次合服前,source 与 target 各跑一遍)
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeBackfillZone 102 -DryRun
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeBackfillZone 102

# 1. 合服预演 / 正式执行(不给 -DryRun 就是 -apply;两者都不给会被工具拒绝)
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone `
  -MergeSourceZone 102 -MergeTargetZone 101 -MergeAssumeKafkaDrained -DryRun
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone `
  -MergeSourceZone 102 -MergeTargetZone 101 -MergeExpectedSrcPlayers <N> `
  -MergeManifestPath <repo>\merge_102_to_101.json -MergeAssumeKafkaDrained

# 2. 多 Redis 集群(生产)再加 blob 拷贝
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone `
  -MergeSourceZone 102 -MergeTargetZone 101 -MergeMigratePlayerBlobs `
  -MergeSourceDataRedis 10.1.2.3:6379,10.1.2.4:6379 `
  -MergeTargetDataRedis 10.1.5.6:6379,10.1.5.7:6379

# 3. 审计 / 合服后验证 / 撤销
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-audit -MergeSourceZone 102 -MergeTargetZone 101
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-audit -MergeSourceZone 102 -MergeTargetZone 101 `
  -MergeExpectedSrcPlayers <N> -VerifyMerged
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-unmerge -MergeManifestPath <file> -DryRun
```

```bash
# 直接跑 CLI 时:tools/merge_zone 是独立 go module,必须在**那个目录里**跑。
# 在仓库根跑 `go run ./tools/merge_zone` 恒为 "go: cannot find main module"。
cd tools/merge_zone && go run . -source-zone=102 -target-zone=101 -dry-run -assume-kafka-drained
```

---

## 4. 重名 / 冲突处理

### 4.1 公会重名 — 已实现(CLI 自动检测)

`tools/merge_zone/main.go:240-260` 的 `checkNameConflicts`:

```sql
SELECT s.guild_id, d.guild_id, s.name
FROM guild s
JOIN guild d ON s.name = d.name AND d.zone_id = TARGET
WHERE s.zone_id = SOURCE
```

**当前策略**:**冲突的 source 公会跳过迁移**(`UPDATE ... AND guild_id NOT IN (conflicts)`),打印名单要求**人工处理**(改名 / 解散 / 合并)后再次执行。

**保留这个策略的理由**:
- 公会名是社交资产,自动改名(如加 `_z102` 后缀)会激怒玩家
- 公会合并涉及成员、银行、权限、贡献值等多张表,不是一句 UPDATE 能解决的
- 人工处理频次极低(同名公会通常 < 10 个 / 万人服)

**未实现的进阶选项**(如运营要求自动化,后续再加):
- `-conflict-strategy=rename-source` —— source 改 `{name}_z{srcZone}`
- `-conflict-strategy=disband-source` —— 解散 source 公会,成员转为散人,银行物品邮件回收
- `-conflict-strategy=merge-by-master-level` —— 保留高等级会长那边,另一边解散

### 4.2 玩家重名 — **状态:已确认无冲突,无需处理**(任务 #15 已结)

**调研结论(2026-05-15 v2)**:**当前数据模型下,合服 player 级别没有任何重名冲突可能**。任务 #15 工作量为 **0**,不需要写改名逻辑、不需要改名券、不需要补偿邮件。

**调研证据**:

| 字段 | 出处 | 唯一性约束 | 合服时是否冲突 |
|---|---|---|---|
| `user.id` | `mysql_database_table.proto:19` | PK,Snowflake 全局唯一 | ❌ 不撞 |
| `user.display_name` | `mysql_database_table.proto:20` | 未声明 UNIQUE — 允许同名 | ❌ **同名也能共存,本来就不是 ID** |
| `user_accounts.account` (Redis) | `login_constants.go:14` `account:{account}` 全服共享前缀 | **login 服务层强制全服唯一** | ❌ **物理上不允许两个 zone 注册同 account** |
| `user_accounts.account` (MySQL) | `go/login/model/mysql_database_table.sql:1` `VARCHAR(191)` | 新库为 PK；存量库须先跑 `model/migrations/20260803_password_auth.sql` 的非空/重复/超长 fail-closed 门禁 | ❌ 门禁通过后不撞 |
| `AccountSimplePlayer` | `proto/common/base/user_accounts.proto:6-9` | 只有 `player_id` 一个字段 | N/A —— **没有 name 字段可撞** |
| `player_database` | `mysql_database_table.proto:90-107` | `player_id` PK,无 name 字段 | N/A |
| `player_id` | Snowflake `[time:32][node_id:17][step:15]` | 全局唯一 | ❌ 不撞 |
| 公会名 | `tools/merge_zone/main.go:240-260` `checkNameConflicts` | UNIQUE per (zone_id, name) | ✅ **CLI 已自动检测并跳过** |

**决定性证据**(在 worktree 调研中找到):
1. **Redis key 不带 zone scoping**:`go/login/internal/constants/login_constants.go:14` 的 `account:{account}` 完全没有 zone_id 前缀。意味着 `account="smoke"` 在 login 服务全局是同一个 key —— 谁先注册谁占用,系统物理上不允许两个 zone 都注册 `smoke`
2. **`AccountSimplePlayer` 只有 player_id**:`proto/common/base/user_accounts.proto:6-9`
   ```proto
   message AccountSimplePlayer {
     uint64 player_id = 1;
   }
   ```
   没有 name 字段,客户端展示玩家时只能用 player_id(或动态查 user.display_name)
3. **`createplayerlogic.go:105-107`** 创建玩家时**只赋值 player_id**,完全没有 name:
   ```go
   newPlayerId := uint64(l.svcCtx.SnowFlake.Generate())
   newPlayer := &login_proto_common.AccountSimplePlayer{PlayerId: newPlayerId}
   ```
4. **HTTP login API 把 zone_id 当请求参数**:`{"zone_id":1,"account":"smoke","password":"x"}` —— zone_id 只是路由参数(决定去哪个 zone),不是 account 命名空间分割维度

**为什么这是好的**:`mmo_cross_server_architecture.md §6.1` 的「player_id 不编码 zone_id」原则被严格执行 —— 整套数据模型都不依赖 zone 做唯一性,合服天然安全。

**合服 CLI 当前已处理的冲突**(只剩这一个,无需改动):
- 公会名:`merge_zone/main.go:240-260` 自动检测并跳过冲突的 source 公会,要求人工解决(改名 / 解散)

**未来如果引入玩家级 name 字段(如自定义昵称),需要重新评估**:
- 加 `player_database.nickname` 或独立 `player_nickname` 表时,必须先决定唯一性范围(zone-scoped 还是全服)
- 若选 zone-scoped,合服时需在此处补改名逻辑(回到本文档之前版本的方案矩阵)
- 若选全服唯一,则在 SetPlayerNickname 流程加全服 SETNX 校验,合服时同样无冲突

**任务 #15 状态**:✅ **已结**(2026-05-15)。任何引入 player-level name 的 PR 都应作为新任务重新评估本节。

---

## 5. 完整停服合服 SOP

> 这是真正生产合服时执行的步骤,**所有操作有先后,不能乱**。

### 5.1 准备阶段(T-7 天)

1. **公告**:游戏内 + 官网 + 社群,提前 7 天告知合服时间、source/target 服编号、补偿礼包内容
2. **数据备份**(强制,不可省):
   - source zone MySQL `mysqldump` → 异地存储,标 `pre-merge-{src}-{dst}-{date}.sql`
   - target zone MySQL 同样备份(目标也是受影响方)
   - source/target Redis BGSAVE → 异地存储 `.rdb`
3. **重名冲突预扫**:`-dry-run` 跑一遍,记录公会冲突 + 玩家重名(若已实现 #15)
4. **客服培训**:准备投诉 FAQ、改名券发放流程、公会冲突处理预案
5. **回滚预案**:如果合服中失败,走 §5.5 —— **`merge-zone-unmerge -MergeManifestPath <清单>`**,
   备份还原只是清单丢失时的兜底
6. **存量映射回填**(2026-09-08 新增,**第一次合服前必做**):`-MergeBackfillZone` 对 source 与
   target 各跑一遍。存量玩家历史上没有 `player:zone:{id}`,没有映射的玩家不会被合过去

### 5.2 停服窗口(T+0,通常凌晨 3-6 点 3 小时窗口)

> **2026-09-08 重写**。v1 这一段有三处硬错误:引用了不存在的 `merge_zone.ps1`、topic 名写成
> `db_task_topic`(真名 `db_task_zone_{zone}[_g{gen}]`)、把 blob 拷贝与主流程拆成「5a/5b 两步」
> (工具是一条流水线,拆不开)。**可逐字执行的版本见 [`merge-zone-runbook.md §8`](../ops/merge-zone-runbook.md)**,
> 本节只保留骨架供设计层对照。

```
Step 0:前置(可以在 T-7 就做完,不占窗口)
──────────────────────────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeBackfillZone <src>
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeBackfillZone <dst>
# 存量玩家没有 player:zone 映射就不会被合过去;两个 zone 都要跑

Step 1:网关置维护 + 踢人
─────────────────────────
# POST /admin/zones/<zoneId>/maintenance,头 X-Admin-Key,一次一个 zone
# (v1 写的 POST /admin/maintenance + Bearer token 不存在)

Step 2:关 source + target
──────────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName <source>
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName <target>
# ⚠️ 这条命令 = kubectl delete namespace mmorpg-zone-<name>,整个 zone 的负载被销毁。
#    共享 infra(MySQL/Redis/Kafka/etcd)在 mmorpg-infra,不受影响。

Step 3:备份 MySQL + Redis
──────────────────────────
kubectl create job -n mmorpg-infra --from=cronjob/mysql-backup mysql-backup-pre-merge-$(date +%s)
# mysql-backup CronJob 只在 infra namespace 有一份(不是 per-zone)

Step 4:第二次 dry-run(与 T-1 彩排的 players_in_source 必须一致)
────────────────────────────────────────────────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone `
  -MergeSourceZone <src> -MergeTargetZone <dst> -MergeExpectedSrcPlayers <N> -DryRun
# Kafka 积压门禁必须给来源:-MergeKafkaConsumerGroupsCmd(真查)或 -MergeAssumeKafkaDrained(声明)
# 工具查的 topic 是 db_task_zone_<src>[_g<gen>],消费组 db_rpc_consumer_group

Step 5:正式执行(一条命令跑完 §3.1a 的全部步骤,顺序由工具保证)
─────────────────────────────────────────────────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone `
  -MergeSourceZone <src> -MergeTargetZone <dst> -MergeExpectedSrcPlayers <N> `
  -MergeManifestPath <repo>\merge_<src>_to_<dst>.json -MergeClearSourceHotState
# 多 Redis 集群再加 -MergeMigratePlayerBlobs -MergeSourceDataRedis/-MergeTargetDataRedis
# **清单路径记进维护工单** —— 它是重跑与撤销的唯一凭据

Step 6:合服后断言
──────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-audit `
  -MergeSourceZone <src> -MergeTargetZone <dst> -MergeExpectedSrcPlayers <N> -VerifyMerged
# 六条 block 级断言,含 verify:merge_fence(围栏没清干净 = 建号建帮被永久拒绝)
# 退出码 0=干净 / 1=有 block / 2=审计自己没跑成(结论不可信,不要当 1 处理)

Step 7:启动 target zone
────────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up `
  -ZoneName <target> -ZoneId <dst-id> -WaitReady

Step 8:source zone 永久关停
───────────────────────────
# 它的 namespace 在 Step 2 就已经被删掉了,没有「保留 N 天」这一步。
# 要保的是 MySQL / Redis 备份(现行 90 天口径)+ 那份清单文件。

Step 9:开服公告
───────────────
# POST /admin/zones/<dst>/open;source 保持维护态 / 从服务器列表下架
```

### 5.3 验证阶段(开服后第一小时)

| 检查项 | 预期 | 出问题怎么办 |
|---|---|---|
| source 玩家能登录 target | login 路由到 target gate | 查 `player:zone:{pid}` 是否真的改成了 target |
| 公会数据完整(成员 / 银行 / 等级) | 同合服前 | 查 guild 表 zone_id 是否改了 |
| 排行榜含两服合并数据 | ZSET 大小 ≈ srcSize + dstSize | 查 `guild_rank:zone:{dst}` |
| 邮件可读 | 历史邮件全在 | 邮件按 player_id 索引,不受合服影响 |
| 改名玩家收到补偿邮件 | 收到改名券 | 查邮件发送日志 |

### 5.4 善后(T+1 → T+30 天)

- **D+1**:统计客诉,对补偿不满的玩家追加礼包
- **D+7**:确认 source zone K8s 资源已下线,云成本降下来
- **D+30**:source zone 数据备份归档冷存储(S3 Glacier 等);本地 MySQL 备份可删除
- **D+90**:source zone 备份彻底删除(GDPR / 数据生命周期合规)

### 5.5 回滚

> 🚫 **v1 的手工六步回滚(R1~R6)已于 2026-09-08 整体作废并删除。**
>
> 作废的核心理由是 **R2 那一步会造成数据事故**:它让运维「临时跑
> `RemapHomeZoneForMerge(target → src)`」把 mapping 改回去。但那个 RPC 是**按值匹配**的
> (`SCAN player:zone:* → 值 == target ⇒ 改成 source`),它分不清「这次合过来的玩家」和
> **「目标区原住民」**——一跑下去,target 自己的玩家会被一起送进那个已经下线的 source zone。
> 原文那句「但要小心:target 原本的玩家不能被改」根本没有可执行的落实手段。
>
> 而且这条路今天已经走不通了:该 RPC 现在**要求 `x-admin-token`**(没配 = 停用)
> **且要求源 zone 的 `merge:in_progress:{source}` 围栏存在**——合服跑完围栏已经被释放,
> 调用只会拿到 `FailedPrecondition`。
>
> R3/R4 的「从备份 SQL 里 grep 出来 UPDATE 回去」同样已被逐对象撤销取代;R5 的结论
> (blob 不删也安全)被保留进了新实现的说明里。

**当前唯一正确的回滚路径:按清单逐对象撤销。**

```powershell
# 先预演,看会动多少、有多少行会被拒绝删除
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-unmerge `
  -MergeManifestPath <repo>\merge_<src>_to_<dst>_<ts>.json -DryRun

pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-unmerge `
  -MergeManifestPath <repo>\merge_<src>_to_<dst>_<ts>.json
```

撤销顺序与合服**完全相反**(先改路由,再往回搬数据,这样任何一步失败时玩家的归属都指向
一个**确实有他数据**的 zone):`7' 清公告 flag → 5' mapping 回 src → 4' ZSET 回 src →
3' guild.zone_id 回 src + 缓存失效 → 1' 删目标库里与源库逐字节相同的玩家行`。

两条硬保证,正是 R2 缺的那两条:

- **只碰清单里列出的玩家 / 公会 / ZSET 成员**——目标区原住民按构造安全,不靠人「小心」。
- **1' 只删与源库逐字节相同的行**。任何一行在合服后被改过(玩家登录过、领过邮件)就
  **拒绝删除并报出来**——那时删掉的不是「拷贝」,而是合服后产生的唯一一份数据。
  源库的行从来没删过,所以留一份多余副本是安全的(mapping 已经指回源区,没人会读)。
  `player:{id}:*` blob 同理不自动删。

**边界**:

- 适用面 = **合服刚跑完、还没开服**。已经开服且玩家在 target 玩过 ⇒ 1' 会大面积「REFUSED」,
  得到一个半撤销状态;这时应当**不要**撤销,转为单玩家修复(`RollbackPlayer`)。
- **清单丢了 = `-mode unmerge` 不可用**,只能退到备份还原(代价:target 在窗口后的写入全丢)。
  所以清单必须跟着维护工单归档。

完整 SOP 见 [`merge-zone-runbook.md §10`](../ops/merge-zone-runbook.md)。

**关键提醒**:**回滚比合服难 10 倍**。所以 §5.1 备份必须严格做,§5.2 Step 5 的清单必须留档。

---

## 6. 已知未覆盖项(留给运营 / 后续迭代)

下列**不在 CLI 范围**,需要人工或单独工具处理:

### 6.1 玩家排行榜(战力 / 等级 / 充值等)
当前 `merge_zone` CLI 只处理 `guild_rank:zone:*` 公会榜。**玩家个人榜(若有)需要类似的 ZSET 合并**。后续如有玩家榜需求,在 CLI 里加 `-merge-player-rank` 开关,逻辑同 `mergeRankingZSET`。

### 6.2 跨服活动状态
如有「跨服竞技场」「跨服 BOSS」之类的活动数据,合服后需要单独清理 / 重建。**当前架构未实现真正的跨服活动**(参见 `mmo_cross_server_architecture.md`),所以 N/A。

### 6.3 友情链 / 黑名单
按位置透明原则,friend service 不感知 zone(只看 player_id),合服后**自动一致**,无需迁移。`zone_data_rollback.md §2.1` 已确认这一点。

### 6.4 拍卖行 / 跨服市场
当前架构未实现拍卖行。如未来加入,合服时需要类似 guild 的处理(zone_id 改写 + 重名冲突)。

### 6.5 玩家在线计数 / 服务器排名
这些是 ops 视图,合服后 source 服从列表里下架即可。不涉及数据迁移。

### 6.6 充值 / 客服订单的 zone_id 历史记录
**保留原 zone_id**(审计需求)。merge_zone CLI 不动 `recharge_log` / `gm_audit_log` 这类历史表 —— 历史就是历史。

---

## 7. 测试 SOP

### 7.1 开发自测

```bash
# 1. 起本地 K8s 单集群,创建两个 zone
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up -ZoneName z101 -ZoneId 101
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up -ZoneName z102 -ZoneId 102

# 2. 用 robot 在两服各创建 100 个角色 + 10 个公会 + 写一些数据
go run -C robot/clients/cmd/multi_zone_seeder . -zone=101 -count=100
go run -C robot/clients/cmd/multi_zone_seeder . -zone=102 -count=100

# 2b. 回填 player:zone(存量玩家没有映射就不会被合过去)
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeBackfillZone 101
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeBackfillZone 102

# 3. dry-run 合服
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone `
  -MergeSourceZone 102 -MergeTargetZone 101 -MergeAssumeKafkaDrained -DryRun

# 4. 正式合服
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName z101
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName z102
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone `
  -MergeSourceZone 102 -MergeTargetZone 101 -MergeExpectedSrcPlayers <N> `
  -MergeManifestPath <repo>\merge_102_to_101.json -MergeAssumeKafkaDrained
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up -ZoneName z101 -ZoneId 101

# 5. 验证 source 角色能登录 target
go run -C robot/clients/cmd/login_smoke . -gate=<gate-addr> -accounts=<src-account-list>
```

### 7.2 staging 演练

每次大版本上线前在 staging 跑一次合服演练(从生产快照恢复出两个 zone,执行合服,记录耗时和缺陷)。**生产合服前最少 1 次成功演练**。

---

## 8. 文档关系图

```
本文件(server_merge_design.md) — 设计与历史决策
├─ 可执行 SOP:docs/ops/merge-zone-runbook.md ← **运维按它逐字执行,不要按本文**
├─ 仍开着的口子:docs/design/server-merge-gap-fixes.md
├─ 实现:tools/merge_zone/(独立 go module;main.go 顶部注释 = 步骤顺序真源)
├─ 实现:tools/scripts/dev_tools.ps1 的 merge-zone / merge-zone-audit / merge-zone-unmerge
├─ 实现:proto/data_service/data_service.proto:25(联机 RPC,现需 admin token + 围栏)
├─ 上游约束:mmo_cross_server_architecture.md §6/§9(player_id 设计 / 合服策略)
├─ 上游约束:enter-scene-zone-routing.md(home zone 路由约束)
├─ 引用:guild_ranking_architecture.md §合服工具(公会榜合并细节)
├─ 引用:zone_data_rollback.md §3(灾难回档 SOP — 与合服共享 K8s zone-up/down 命令)
└─ 已知缺口:见 AUDIT.md §3.2(玩家重名 / SOP 完善)
```

---

## 9. Changelog

- **2026-09-08 v2**: 与 2026-09 的合服大修对齐。**历史叙述保留,作废段落就地标注而不是悄悄改写**:
  - §3.1 保留 v1 原文并标「已被 §3.1a 取代」,另列出其中三条已作废的说法(玩家主数据必须搬 /
    公会重名从「跳过」改「中止」/ ZSET 合并改 MULTI/EXEC + 维护锁)。
  - **新增 §3.1a**(当前 P1→G→P2..P6→F→M→1..7 的真实步骤顺序)与 **§3.5**(Redis 库地图 + 围栏契约)。
  - §3.2 更正:`tools/scripts/merge_zone.ps1` **从来不存在**;旧 `dev_tools.ps1` 在仓库根跑
    `go run ./tools/merge_zone` 恒失败(独立 module)——**这个入口在修复前从没真正合过一次服**。
  - §3.3 补上 `RemapHomeZoneForMerge` 现在的两道闸(admin token + 围栏必须在),用途收窄。
  - §3.4 / §5.2 / §7.1 命令改为真实入口;topic 名从 `db_task_topic` 更正为 `db_task_zone_{zone}[_g{gen}]`;
    网关维护接口更正为 `POST /admin/zones/{zoneId}/maintenance` + `X-Admin-Key`;
    `mysql-backup` CronJob 只在 `mmorpg-infra` 有一份;`k8s-zone-down` = 删 namespace。
  - **§5.5 删除 v1 的手工六步回滚**。R2 让运维跑 `RemapHomeZoneForMerge(target → src)`,而该 RPC
    **按值匹配**,会把目标区原住民一起送进已下线的源区;今天它还要 admin token + 源区围栏,合服后
    根本调不通。回滚改为指向 `merge-zone-unmerge -MergeManifestPath`(逐对象、只碰清单里的对象、
    只删与源库逐字节相同的行)。
  - §5.1 增加「存量 `player:zone` 回填」为强制前置。
- **2026-05-15 v1**: 初版,从 `merge_zone/main.go`、`mmo_cross_server_architecture.md §9`、`guild_ranking_architecture.md §合服工具`、`enter-scene-zone-routing.md` 收口而成。明确 SOP、回滚流程、未覆盖项。

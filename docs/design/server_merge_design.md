# 合服(Server Merge)设计文档

> **文档状态**: v1 — 2026-05-15
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

### 3.1 Go CLI:`tools/merge_zone/main.go`

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

### 3.2 PowerShell 包装

`tools/scripts/merge_zone.ps1` —— 自动 `go build` 并执行 CLI。`dev_tools.ps1 -Command merge-zone` 也走它。

### 3.3 联机版 RPC:`DataService/RemapHomeZoneForMerge`

`proto/data_service/data_service.proto:25` 定义的 RPC,**只做步骤 3**(mapping 重映射,服务在线时也能执行)。MessageId = 129。Go 实现在 `dataserviceserver.go`。

**用途**:小规模合服 / 客服热处理;不需要停服窗口的 mapping 修正。

### 3.4 命令示例

```bash
# 1. 同 Redis(开发 / 单机部署)— 最简单
go run -C tools/merge_zone . -source-zone=102 -target-zone=101 -dry-run
go run -C tools/merge_zone . -source-zone=102 -target-zone=101 -apply

# 2. 多 Redis 集群(生产)— 需要 blob 拷贝
go run -C tools/merge_zone . -source-zone=102 -target-zone=101 -dry-run \
   -migrate-player-blobs \
   -source-data-redis=10.1.2.3:6379,10.1.2.4:6379 \
   -target-data-redis=10.1.5.6:6379,10.1.5.7:6379

# 3. PowerShell 包装(更省心)
pwsh -File tools/scripts/merge_zone.ps1 -SourceZone 102 -TargetZone 101 -DryRun
pwsh -File tools/scripts/merge_zone.ps1 -SourceZone 102 -TargetZone 101 -Apply

# 4. 联机版(只改 mapping,不停服)
grpcurl -plaintext -d '{"source_zone_id":102,"target_zone_id":101,"dry_run":true}' \
   data-service:8080 data_service.DataService/RemapHomeZoneForMerge
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
5. **回滚预案**:如果合服中失败,执行流程 §5.5 — 把 mapping 改回去 + 恢复备份

### 5.2 停服窗口(T+0,通常凌晨 3-6 点 3 小时窗口)

```
Step 1:关 source zone(玩家踢下线 + 阻止重连)
─────────────────────────────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName <source>
# 等待 5 分钟让 in-flight 写入落库

Step 2:关 target zone(同样下线)
─────────────────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName <target>
# 两边都下,避免合服中途有玩家在 target 里继续写数据

Step 3:刷干 Kafka in-flight
────────────────────────────
# 让 db_task_topic 的 consumer 消费完积压
kafka-consumer-groups.sh --describe --group db_rpc_consumer_group \
  --bootstrap-server <broker>
# 等 LAG 全部 = 0

Step 4:执行合服 dry-run(再确认一次)
─────────────────────────────────────
pwsh -File tools/scripts/merge_zone.ps1 -SourceZone <src> -TargetZone <dst> -DryRun
# 检查输出:players_in_source / guild_rows / map_matched / map_updated 是否合理

Step 5:正式执行(顺序敏感 ⚠️)
──────────────────────────────
# 5a. 先拷 player blob(若多 Redis 集群)
pwsh -File tools/scripts/merge_zone.ps1 -SourceZone <src> -TargetZone <dst> -Apply \
  -MigratePlayerBlobs -SourceDataRedis <src-addrs> -TargetDataRedis <dst-addrs>

# 5b. 再改 guild MySQL + ZSET + mapping(默认行为)
# 注意:5a 命令已经包含了所有步骤;若分两步执行需要显式 -SkipMapping 等开关

Step 6:玩家重名处理(若 #15 已实现)
────────────────────────────────────
# 工具会自动改名 + 写补偿邮件;无需人工

Step 7:启动 target zone(承接所有玩家)
──────────────────────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up \
  -ZoneName <target> -ZoneId <dst-id> -NodeImage <image> -WaitReady

Step 8:source zone 永久关停
───────────────────────────
# 不再启动 source zone 的 K8s namespace
# 但保留 MySQL 备份至少 90 天(应对客诉)

Step 9:开服公告
───────────────
# 「合服已完成,补偿礼包请到邮箱领取」
# 客服值班 24 小时应对客诉
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

### 5.5 回滚(只在 step 5-7 失败时执行)

```
Step R1:停 target(刚启动的)
─────────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName <target>

Step R2:恢复 mapping
────────────────────
# 把 player:zone:* 中值=target 但**应该**是 source 的玩家改回 source
# 工具未提供反向 CLI,手动 SCAN+SET 或临时跑 RemapHomeZoneForMerge(target → src)
# 但要小心:target 原本的玩家不能被改

Step R3:恢复 guild zone_id
──────────────────────────
# 从备份 SQL 里 grep 出 guild WHERE zone_id=src 的原始记录,UPDATE 回去
# 或直接 LOAD DATA INFILE 覆盖(更暴力)

Step R4:恢复 ZSET
─────────────────
# 从 RDB 备份里把 guild_rank:zone:{src} 还原

Step R5:恢复 player blob(若做过 5a 跨集群拷贝)
─────────────────────────────────────────────
# target data Redis 上把多出来的 player:{src-pids}:* 删除
# (或不删,反正 mapping 改回去后没人会读)

Step R6:重启两个 zone
──────────────────────
# source 和 target 都重新拉起
```

**关键提醒**:**回滚比合服难 10 倍**。所以 §5.1 备份必须严格做。

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

# 3. dry-run 合服
pwsh -File tools/scripts/merge_zone.ps1 -SourceZone 102 -TargetZone 101 -DryRun

# 4. 正式合服
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName z101
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName z102
pwsh -File tools/scripts/merge_zone.ps1 -SourceZone 102 -TargetZone 101 -Apply
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up -ZoneName z101 -ZoneId 101

# 5. 验证 source 角色能登录 target
go run -C robot/clients/cmd/login_smoke . -gate=<gate-addr> -accounts=<src-account-list>
```

### 7.2 staging 演练

每次大版本上线前在 staging 跑一次合服演练(从生产快照恢复出两个 zone,执行合服,记录耗时和缺陷)。**生产合服前最少 1 次成功演练**。

---

## 8. 文档关系图

```
本文件(server_merge_design.md) — 单一权威 SOP
├─ 实现:tools/merge_zone/main.go(CLI 源码)
├─ 实现:tools/scripts/merge_zone.ps1(包装)
├─ 实现:proto/data_service/data_service.proto:25(联机 RPC)
├─ 上游约束:mmo_cross_server_architecture.md §6/§9(player_id 设计 / 合服策略)
├─ 上游约束:enter-scene-zone-routing.md(home zone 路由约束)
├─ 引用:guild_ranking_architecture.md §合服工具(公会榜合并细节)
├─ 引用:zone_data_rollback.md §3(灾难回档 SOP — 与合服共享 K8s zone-up/down 命令)
└─ 已知缺口:见 AUDIT.md §3.2(玩家重名 / SOP 完善)
```

---

## 9. Changelog

- **2026-05-15 v1**: 初版,从 `merge_zone/main.go`、`mmo_cross_server_architecture.md §9`、`guild_ranking_architecture.md §合服工具`、`enter-scene-zone-routing.md` 收口而成。明确 SOP、回滚流程、未覆盖项。

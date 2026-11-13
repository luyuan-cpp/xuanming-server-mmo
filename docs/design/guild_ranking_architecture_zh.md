# 帮会排行榜架构

> 现行实现。2026-11-10 按删除全服榜之后的代码重写；2026-11-11 写分改为带版本的条件写入，不再抢全服的榜维护锁（[缺口清单](../handoff/server-gap-audit-20261101.md)第 23 条，[消除单节点总纲](no-single-node-horizontal-scaling-20261001.md) §9 的 C3）。两处代码改动都未编译：删全服榜的验收见[执行清单](../handoff/gate-online-identity-simplify-20261105/CODEX.md)第 8 节，带版本写入的验收见 [PROGRESS](../PROGRESS.md) 2026-11-11「帮会积分写入不再抢全服锁」。2025-03-25 的初稿是「全服 + 本服」双榜，全服榜由负责人 2026-11-10 定删除，原因见 §6。
> 本文与 [guild_ranking_architecture.md](guild_ranking_architecture.md) 内容相同，英文版是 [guild_ranking_architecture_en.md](guild_ranking_architecture_en.md)；改一份要三份同步。

## 1. 结论

- 帮会榜只有本服榜：每个最终区一张 ZSET，玩家只看自己归属区的榜。没有全服榜，也不需要聚合服务。
- 排行分的权威副本是 MySQL `guild.score`，每次改分在同一事务里把 `guild.score_version` 加 1。ZSET 是读加速用的派生投影，丢了、错了都由 `RebuildRanks` 按 MySQL 对齐。
- 榜上每一项都记着自己的版本，所有写榜都是条件写入：版本不比榜上已记的旧才写。所以写分不需要全服锁：几个副本并发推分、提交之后乱序到达 Redis，榜上留下的总是最后提交的那个分数。
- 帮会进哪张榜只看 MySQL 里帮会行的 `zone_id` 经合服映射解析出的最终区。`zone_id` 是建帮时的原始区，合服不改写。调用方传进来的区从不决定进哪张榜。

## 2. 数据与 Redis 键

| 名称 | 类型 | 内容 |
|---|---|---|
| MySQL `guild.score` | 列 | 排行分的权威副本 |
| MySQL `guild.score_version` | 列 | 分数版本。建帮写 0，`UpdateGuildScore` 在写 `score` 的同一事务里加 1。同一帮会的写分在 guild 行锁上串行，所以版本随提交顺序严格递增，一个版本只对应一个已提交的分数 |
| MySQL `guild.zone_id` | 列 | 建帮时的原始区。建帮只在请求者的归属区建，所以恒非 0；合服不改写 |
| `guild_rank:zone:{gr:<最终区>}` | ZSET | member = guild_id，score = 排行分 |
| `guild_rank:version:{gr:<最终区>}` | HASH | field = guild_id，value = 榜上那个分数的 `score_version`。与同区的 ZSET 只经脚本一起改，两者的成员始终一致 |
| `guild_rank:tombstone:{gr:<区>}` | ZSET | 本区的解散墓碑，member = guild_id，score = 到期时刻（Redis 服务器时间，毫秒，立碑后 24 小时）。挡住解散前提交、解散后才到的写入。每次立碑把整个键的 TTL 续到这一条的到期时刻 |
| `guild_rank:maintenance_lock:{gr:<区>}` | 字符串 | 榜维护锁，每区一把，只让重建与合服折叠互斥（重建按升序拿涉及的各区，折叠拿各源区）。SETNX，TTL 5 分钟，最多等 5 秒，令牌相符才释放。写分、建帮上榜、解散摘榜都不拿它 |

键在控制集群上（guild 的 `ControlRedis` 段，`go/guild/etc/guild.yaml`；2026-11-12 全部 Redis 上 Cluster 第 5a 批改形）。同区的榜、版本 HASH、墓碑与维护锁共用一个 hash tag `{gr:<区>}`，写榜的每段脚本只碰一个区、一个槽。以 `guild_rank` 开头的键都归公会榜，别的服务的键不得用这个前缀（[排行榜设计](leaderboard-system.md) §3.1）。哪些区有榜键从 SQL 推出（`rankZones`：guild 表出现过的原始区与它们的最终区，加合服映射两端），重建与摘榜都不扫 Redis；盖不到的只有「某区帮会全部解散、摘榜又失败」留下的幽灵，那个区再有帮会后下一次重建摘掉。

## 3. 写入

所有写榜都走同一个条件写入脚本（`rankUpsertScript`）。它只碰一个区的三个键（ZSET、版本 HASH、墓碑）：

1. 本区墓碑里有这个帮会且没到期：帮会已解散，不写。
2. 版本 HASH 里已记的版本比这次的新：不写。
3. 否则写 ZSET、记版本。版本相等也写：同一版本只对应一个分数，重写是幂等的，还能修好被手工改过的分数。

各写入方：

- **改分**：data 层的 `UpdateGuildScore` 在一个 READ COMMITTED 事务里 `SELECT zone_id, score_version ... FOR UPDATE` 锁住帮会行，再 `UPDATE guild SET score = ?, score_version = ?`（旧版本加 1）。死锁和写冲突有界重试，提交结果不明一律归成 `ErrWriteConflict`。提交后失效帮会缓存，再按这次的版本对最终区的榜做条件写入。不拿维护锁。
  - 先 MySQL 后 Redis 是刻意的：ZSET 丢了能重建；反过来先写 ZSET，Redis 一出故障分数就永久丢了。
  - 进哪张榜只看 MySQL 的 `zone_id`，调用方传的区只用于打不一致日志。
  - `zone_id=0` 的行是坏数据：分数照常落库，但不进任何榜，并打 ERROR 日志。
  - 条件写入没写（榜上已是更新的版本、帮会已解散）是并发下的正常结果，打 INFO 日志，不算失败。
- **改分 RPC**：同名 RPC `UpdateGuildScore` 标了 `CLIENT_RPC_SERVER_ONLY`，客户端发上来会被 gate 当非法包丢弃。全仓现在没有正常调用方，robot 只用它验证客户端发不上来。
- **建帮**：logic 层建帮成功后调 `AddNewGuildToRank`，按建帮事务写入的初始分数 0、版本 0 上榜，只写一次 Redis，不再开第二个事务。版本 0 输给之后任何一次推分，晚到也盖不掉新分数。失败只记日志：权威分在 MySQL，缺的榜项由下次推分或启动重建补上。
- **解散与摘榜**：`RemoveGuildFromRank` 先定区。帮会行还在时以 MySQL 的 `zone_id` 为准；行已删（解散的正常路径）时，用解散事务里 `FOR UPDATE` 读到的区。然后对 zone 清单里的每张榜（guild 表出现过的区及其最终区、合服映射两端，从 SQL 推出，不扫 Redis），以及该帮合服组（解析到同一最终区的全部原始区，含最终区）的每个区，逐个把这一项连同版本摘掉。解散时，合服组里的区在摘的同一段脚本里立墓碑（`rankBuryScript`）。不拿维护锁。
  - 为什么不只摘一张：合服折叠之前，条目可能还在源区的旧键上；撤销合服之后，条目可能还在目标区的键上。只摘一张会留下查不到名字的幽灵条目。
  - 为什么不用和写分互斥：一个区的立墓碑与摘榜是同一段脚本。在它之前执行的写入留下的项被它摘掉，在它之后的写入被墓碑拒绝。
  - 为什么墓碑立在合服组就够：写榜只写进 `Final(zone_id)`。映射缓存滞后的副本算出来的也在合服组里：还不知道合服的写原始区，知道的写最终区。剩一个窗口：刚合服时摘榜的副本还没刷新到新映射，推分的副本已经刷新了，迟到的推分会写进摘榜副本不知道的新最终区，留下一个幽灵条目，到下次重建才摘掉。只影响榜上的显示。
  - 墓碑保留多久：迟到的写入只有三个来源，即解散前已提交、还没写榜的推分（同一请求内，秒级），重建读完快照到合并之间（分钟级），折叠读出源区条目到并进目标区之间（秒级）。24 小时远大于这三者。到期按 Redis 服务器时间算，各副本的时钟差不进来。
  - 墓碑怎么清理：不要清理任务。每次立墓碑先删掉本区已到期的，再把整个墓碑键的 TTL 续到这一条的到期时刻；一个区一天没人解散，墓碑键由 Redis 自己过期删除。
  - 帮会行还在（降级、手工清榜）时只摘不立墓碑，之后的推分照常让它回到榜上。
  - 解散后清榜失败只记日志，由启动重建自愈。

## 4. 读取

- **入口**：客户端方法 `GetGuildRank`（分页）和 `GetGuildRankByGuild`（某个帮会的名次）只受理带会话的请求，无会话调用回 `PermissionDenied`。
- **区**：一律取 data_service 给的归属区。查不到归属回 `kGuildHomeZoneUnknown`，查询故障按错误返回。服务端不读请求体里的区，字段删除见 [Gate 在线身份](gate-online-identity.md) §9.6 第 2 条。
- **分页**：页码从 1 开始，缺省 1；页长缺省 20，夹到 `MaxRankPageSize`（50）。每条补帮名、等级、人数（读帮会缓存），帮主名整页一次批量查。
- **名次**：`GetGuildRankByGuild` 查的是请求者归属区榜上的名次。别区的帮会不在本区榜上，回 `kGuildNotRanked`。
- **data 层**：`GetGuildRankPage` / `GetGuildRank` 的 zoneID 必须非 0，传 0 回 `ErrRankZoneRequired`。传 0 只可能是调用方写错了，所以不回空榜，免得冒充「榜上没人」。合走的区读最终区的榜。
  - 一致性：总数与本页（`ZCARD` + `ZREVRANGE`）、名次与分数（`ZREVRANK` + `ZSCORE`）各自在一个 Lua 脚本里读，来自同一快照。读路径只读 ZSET，不碰版本 HASH。
  - 越界：页起点按 64 位算，越界直接回空页，不会环绕回榜首。

## 5. 重建与自愈

`RebuildRanks` 在 guild 启动时执行，只由 stable 副本做，canary 副本跳过（[发布轨道设计](release-track-gray-canary.md) §2.5）。重建失败就拒绝启动，不对外提供不完整的榜。

重建不再建临时键、整键 RENAME 覆盖：那样会把重建期间新写进来的分数盖回快照里的旧值，写分因此要和重建抢同一把锁。现在逐项做条件写，流程：

1. 持维护锁，只为与折叠、别的副本的重建互斥。
2. 全量读 `SELECT guild_id, zone_id, score, score_version FROM guild`。
3. 每行按版本合并进当前合服映射下的最终区。榜上已是更新的版本（快照之后又推过分）就不动；帮会已解散（墓碑在）就不写。
4. 按 zone 清单（从快照推出，同上）逐张读榜，找出不在快照里、或快照说不归这个区的项。这些项先回库复核当前权威，不凭快照下结论：
   - 库里已没有：解散后没摘干净的幽灵。在这个区立墓碑并摘掉；
   - 在库里、属于这个区：快照之后才建的帮会。按当前权威合并，不删；
   - 在库里、属于别的区（映射变了）或 `zone_id=0`：权威分并进它该在的区，再按读到的版本从这里摘，期间新写进来的版本不会被误删。

能修复的情况：缺项、旧分、整张榜丢失、解散后的幽灵条目、放错区的条目；合服或撤销合服之后重启一次，榜也回到与映射一致。`zone_id=0` 的行不进任何榜，只计数并打 ERROR 日志；不是 guild_id 的成员（只可能是手工写进来的）会被摘掉。

正确性不靠维护锁：所有写都是条件写，所有搬走都按版本比较后删除。锁 TTL 5 分钟、不续期，重建超时导致锁过期、折叠同时在跑，也写不坏榜。

## 6. 为什么没有全服榜

2025-03-25 的初稿要的是「全服 + 本服」双榜：推分同时写 `guild_rank` 和本服榜，`zone_id=0` 读全服榜。2026-11-10 负责人定运维和 GM 都不需要全服榜，删掉。依据：

- **没有读者**：客户端只看本区榜。唯一读全服榜的是 guild 读方法的无会话分支，生产里入口准入早就拒绝这类调用，2026-11-06 已删（[Gate 在线身份](gate-online-identity.md) §9.6 第 1 条）。合服工具、数据一致性检查、合服运维手册、robot、网关后台也都只用本服榜。
- **只有代价**：每次推分多一次 ZADD；所有帮会挤在一个键上，上 Redis Cluster 就是单槽热点。
- **两版一致**：Java 版本来就不提供全服榜。

旧环境里残留的 `guild_rank` 键不再有人读写，`RebuildRanks` 只按 zone 清单读 `guild_rank:zone:{gr:<区>}`，也不删它。项目未上线，不写迁移代码，有两种清法：

- 压测前清库时一并清掉：`pwsh -File tools/scripts/dev_tools.ps1 -Command redis-wipe-local -ConfirmWipeAllRedis`，K8s 用 `-Command k8s-redis-wipe`。
- 只删这一个键：`redis-cli -h <guild 的 ControlRedis 地址> -n 0 DEL guild_rank`。

## 7. 合服工具与公会榜

合服工具 `tools/merge_zone` 只登记一条合服映射，即全局库 `zone_merged_into` 里的「源区 → 目标区」，不改公会行，也不碰公会榜的键（见[合服运维手册](../ops/merge-zone-runbook.md)）。公会榜跟着映射走：

- **新分数**：按 `Final(guild.zone_id)` 写进最终区的榜；读源区读到的也是最终区的榜。
- **旧条目**：合服前写进源区旧键的条目，由 `FoldMergedZoneRanks` 折进最终区。
  - 调度：每个副本都跑，挂在帮会历练的待入队循环上，默认 10 秒一轮。稳态下每个源区只做一次 `EXISTS`。
  - 折叠：有旧键时持维护锁，每次从源区榜头原子读出一页（分数与版本成对），逐项按版本条件写入最终区。目标区已是更新的版本就不动；源区的版本更新（映射缓存还没刷新的副本合服之后又往源区写过）就盖掉目标区的旧分；帮会已解散就不写。并完一页，再按读到的版本从源区摘：读完之后又写进源区的新版本不会被误删，留到下一页或下一轮再并。源区的榜空了，键由 Redis 自己删掉。
  - 不用 `ZUNIONSTORE`：Cluster 下两个键不在同一个槽。不用 `ZADD NX`：它会让目标区更旧的分数挡住源区更新的分数。
  - 失败：中途失败下一轮从头再并，条件写入是幂等的。
- **撤销合服**：工具会提示重启一次 guild，由启动重建按当前映射把榜归位。

## 8. 扩展约束

- **写分可以并发**：写分、建帮上榜、解散摘榜都不拿维护锁，不同帮会、不同副本的推分互不等待。同一帮会的写分在 MySQL 的 guild 行锁上串行：这是版本单调的来源，也是单个帮会写分吞吐的上限。维护锁只让重建与折叠互斥，两者都不在请求路径上。
- **已按 Cluster 改形（2026-11-12，第 5a 批，未编译）**：榜键加 `{gr:<区>}`，缓存代次脚本的两把键改成 `guild:v2:cache_generation:{g:<id>}` / `guild:v2:{g:<id>}`（玩家所在帮会是 `{pg:<id>}`），按 zone 清单处理取代 `SCAN guild_rank:zone:*`，维护锁按区。写榜脚本不再进 pipeline（控制集群句柄的守卫拒绝 pipeline 里的 EVAL），重建与折叠逐条发。`tools/data_consistency_check` 的孤儿检查 2026-11-12 已改成逐主节点 SCAN（`-redis-cluster`，单实例与 Cluster 都支持），按 `guild_rank:zone:{gr:<区>}` 解析（与 guild 同用 `redisslot.ZoneControlTag`），改形前的旧键报「形状不认识」，未编译，见 [全部 Redis 上 Cluster 设计稿](redis-cluster-all-domains-20261110.md) §4.8。
- **键的大小**：按区拆键之后，单个键的大小只是一个区的帮会数，不随全服帮会数增长。墓碑键只装本区合服组最近 24 小时解散的帮会。

## 9. 代码位置

- **数据层**：`go/guild/internal/data/guild_repo.go`，即「写榜协议」一节的脚本、`UpdateGuildScore`、`AddNewGuildToRank`、`RebuildRanks`、`RemoveGuildFromRank`、`FoldMergedZoneRanks`、`GetGuildRankPage`、`GetGuildRank`、`zoneRankKey`。
- **表结构**：`proto/guild/guild_db.proto` 的 `GuildRecord`，`score_version` 是字段 12；建表、加列经 `go/schemamigrate`。
- **逻辑层**：`go/guild/internal/logic/guild_logic.go`，即 `GetGuildRank`、`GetGuildRankByGuild`，以及建帮、解散里的榜维护；折叠任务的调度在 `trial_background.go`。
- **启动重建**：`go/guild/guild.go`。
- **协议**：`proto/guild/guild.proto`，即 `UpdateGuildScore`、`GetGuildRank`、`GetGuildRankByGuild`。
- **测试**：`go/guild/internal/data/rank_version_test.go`（不连库：乱序旧写入、写分不等锁、重建与并发写入、快照之后新建的帮会、解散后迟到的写入）、`merged_zone_rank_test.go`（折叠按版本合并），以及 `rank_zone_integration_test.go`（`integration` 标签，连真库）。

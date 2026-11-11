# 帮会排行榜架构

> 现行实现。2026-11-10 按删除全服榜之后的代码重写；代码改动未编译，验收见[执行清单](../handoff/gate-online-identity-simplify-20261105/CODEX.md)第 8 节。2025-03-25 的初稿是「全服 + 本服」双榜，全服榜由负责人 2026-11-10 定删除，原因见 §6。
> 本文与 [guild_ranking_architecture_zh.md](guild_ranking_architecture_zh.md) 内容相同，英文版是 [guild_ranking_architecture_en.md](guild_ranking_architecture_en.md)；改一份要三份同步。

## 1. 结论

- 帮会榜只有本服榜：每个最终区一张 ZSET，玩家只看自己归属区的榜。没有全服榜，也不需要聚合服务。
- 排行分的权威副本是 MySQL `guild.score`。ZSET 是读加速用的派生投影，丢了、错了都由 `RebuildRanks` 从 MySQL 重建。
- 帮会进哪张榜只看 MySQL 里帮会行的 `zone_id` 经合服映射解析出的最终区。`zone_id` 是建帮时的原始区，合服不改写。调用方传进来的区从不决定进哪张榜。

## 2. 数据与 Redis 键

| 名称 | 类型 | 内容 |
|---|---|---|
| MySQL `guild.score` | 列 | 排行分的权威副本 |
| MySQL `guild.zone_id` | 列 | 建帮时的原始区。建帮只在请求者的归属区建，所以恒非 0；合服不改写 |
| `guild_rank:zone:<最终区>` | ZSET | member = guild_id，score = 排行分 |
| `guild_rank:maintenance_lock` | 字符串 | 榜维护锁，让改分、摘榜、重建、折叠互斥。SETNX，TTL 5 分钟，最多等 5 秒，令牌相符才释放 |
| `guild_rank:rebuild:<token>:zone:<区>` | ZSET | 重建时的临时键。切换时 RENAME 成正式键，没切换成功就在退出时删掉 |

键里没有 hash tag。guild 用自己的 `RedisClient`（`go/guild/etc/guild.yaml`，DB 0）。以 `guild_rank` 开头的键都归公会榜：`RebuildRanks` 会删光现有的 `guild_rank:zone:*`，所以别的服务的键不得用这个前缀（[排行榜设计](leaderboard-system.md) §3.1）。

## 3. 写入

- **改分**：data 层的 `UpdateGuildScore` 先持维护锁，然后在一个 READ COMMITTED 事务里 `SELECT zone_id ... FOR UPDATE` 锁住帮会行，再 `UPDATE guild SET score`。死锁和写冲突有界重试，提交结果不明一律归成 `ErrWriteConflict`。提交后失效帮会缓存，再对最终区的榜 `ZADD`。
  - 先 MySQL 后 Redis 是刻意的：ZSET 丢了能重建；反过来先写 ZSET，Redis 一出故障分数就永久丢了。
  - 进哪张榜只看 MySQL 的 `zone_id`，调用方传的区只用于打不一致日志。
  - `zone_id=0` 的行是坏数据：分数照常落库，但不进任何榜，并打 ERROR 日志。
- **改分 RPC**：同名 RPC `UpdateGuildScore` 标了 `CLIENT_RPC_SERVER_ONLY`，客户端发上来会被 gate 当非法包丢弃。全仓现在没有正常调用方，robot 只用它验证客户端发不上来。
- **建帮**：logic 层建帮成功后写一次 0 分进榜。失败只记日志：权威分在 MySQL，缺的榜项由下次推分或启动重建补上。
- **解散与摘榜**：`RemoveGuildFromRank` 先持维护锁，再定区。帮会行还在时以 MySQL 的 `zone_id` 为准；行已删（解散的正常路径）时，用解散事务里 `FOR UPDATE` 读到的区。然后对 `SCAN guild_rank:zone:*` 找到的每张榜，以及最终区那张榜，逐个 `ZREM`。
  - 为什么不只摘一张：合服折叠之前，条目可能还在源区的旧键上；撤销合服之后，条目可能还在目标区的键上。只摘一张会留下查不到名字的幽灵条目。
  - 解散后清榜失败只记日志，由启动重建自愈。

## 4. 读取

- **入口**：客户端方法 `GetGuildRank`（分页）和 `GetGuildRankByGuild`（某个帮会的名次）只受理带会话的请求，无会话调用回 `PermissionDenied`。
- **区**：一律取 data_service 给的归属区。查不到归属回 `kGuildHomeZoneUnknown`，查询故障按错误返回。服务端不读请求体里的区，字段删除见 [Gate 在线身份](gate-online-identity.md) §9.6 第 2 条。
- **分页**：页码从 1 开始，缺省 1；页长缺省 20，夹到 `MaxRankPageSize`（50）。每条补帮名、等级、人数（读帮会缓存），帮主名整页一次批量查。
- **名次**：`GetGuildRankByGuild` 查的是请求者归属区榜上的名次。别区的帮会不在本区榜上，回 `kGuildNotRanked`。
- **data 层**：`GetGuildRankPage` / `GetGuildRank` 的 zoneID 必须非 0，传 0 回 `ErrRankZoneRequired`。传 0 只可能是调用方写错了，所以不回空榜，免得冒充「榜上没人」。合走的区读最终区的榜。
  - 一致性：总数与本页（`ZCARD` + `ZREVRANGE`）、名次与分数（`ZREVRANK` + `ZSCORE`）各自在一个 Lua 脚本里读，来自同一快照。
  - 越界：页起点按 64 位算，越界直接回空页，不会环绕回榜首。

## 5. 重建与自愈

`RebuildRanks` 在 guild 启动时执行，只由 stable 副本做，canary 副本跳过（[发布轨道设计](release-track-gray-canary.md) §2.5）。重建失败就拒绝启动，不对外提供不完整的榜。流程：

1. 持维护锁。
2. 全量读 `SELECT guild_id, zone_id, score FROM guild`。
3. 按当前合服映射的最终区写进临时键。
4. 在一个 MULTI/EXEC 里删掉现有全部 `guild_rank:zone:*`，把临时键 RENAME 成正式键。

能修复的情况：缺项、旧分、整张榜丢失、解散后的幽灵条目；合服或撤销合服之后重启一次，榜也回到与映射一致。`zone_id=0` 的行不进任何榜，只计数并打 ERROR 日志。

## 6. 为什么没有全服榜

2025-03-25 的初稿要的是「全服 + 本服」双榜：推分同时写 `guild_rank` 和本服榜，`zone_id=0` 读全服榜。2026-11-10 负责人定运维和 GM 都不需要全服榜，删掉。依据：

- **没有读者**：客户端只看本区榜。唯一读全服榜的是 guild 读方法的无会话分支，生产里入口准入早就拒绝这类调用，2026-11-06 已删（[Gate 在线身份](gate-online-identity.md) §9.6 第 1 条）。合服工具、数据一致性检查、合服运维手册、robot、网关后台也都只用本服榜。
- **只有代价**：每次推分多一次 ZADD；所有帮会挤在一个键上，上 Redis Cluster 就是单槽热点。
- **两版一致**：Java 版本来就不提供全服榜。

旧环境里残留的 `guild_rank` 键不再有人读写，`RebuildRanks` 也不删它。项目未上线，不写迁移代码，有两种清法：

- 压测前清库时一并清掉：`pwsh -File tools/scripts/dev_tools.ps1 -Command redis-wipe-local -ConfirmWipeAllRedis`，K8s 用 `-Command k8s-redis-wipe`。
- 只删这一个键：`redis-cli -h <guild 的 RedisClient 地址> -n 0 DEL guild_rank`。

## 7. 合服工具与公会榜

合服工具 `tools/merge_zone` 只登记一条合服映射，即全局库 `zone_merged_into` 里的「源区 → 目标区」，不改公会行，也不碰公会榜的键（见[合服运维手册](../ops/merge-zone-runbook.md)）。公会榜跟着映射走：

- **新分数**：按 `Final(guild.zone_id)` 写进最终区的榜；读源区读到的也是最终区的榜。
- **旧条目**：合服前写进源区旧键的条目，由 `FoldMergedZoneRanks` 折进最终区。
  - 调度：每个副本都跑，挂在帮会历练的待入队循环上，默认 10 秒一轮。稳态下每个源区只做一次 `EXISTS`。
  - 折叠：有旧键时持维护锁，`ZSCAN` 逐成员 `ZADD NX` 到最终区，全部写完再删旧键。`NX` 不覆盖合服之后写的新分数；不用 `ZUNIONSTORE`，因为 Cluster 下两个键不在同一个槽。
  - 失败：中途失败下一轮从头再并，`NX` 保证幂等。
- **撤销合服**：工具会提示重启一次 guild，由启动重建按当前映射把榜归位。

## 8. 扩展约束

- **改分串行**：改分、摘榜、重建、折叠都抢同一把维护锁，所以改分是串行的。现在只有建帮时写一次 0 分，不是瓶颈；接入高频推分之前要先改，方案见[缺口清单](../handoff/server-gap-audit-20261101.md)第 23 条。
- **还不能上 Cluster**：guild 现在用单实例 Redis 客户端。`RebuildRanks` 的切换在一个 MULTI/EXEC 里同时动多个键（删各区旧榜、RENAME 临时键），临时键和正式键也不在同一个槽，上 Redis Cluster 之前必须先改。
- **键的大小**：按区拆键之后，单个键的大小只是一个区的帮会数，不随全服帮会数增长。

## 9. 代码位置

- **数据层**：`go/guild/internal/data/guild_repo.go`，即 `UpdateGuildScore`、`RebuildRanks`、`RemoveGuildFromRank`、`FoldMergedZoneRanks`、`GetGuildRankPage`、`GetGuildRank`、`zoneRankKey`。
- **逻辑层**：`go/guild/internal/logic/guild_logic.go`，即 `GetGuildRank`、`GetGuildRankByGuild`，以及建帮、解散里的榜维护；折叠任务的调度在 `trial_background.go`。
- **启动重建**：`go/guild/guild.go`。
- **协议**：`proto/guild/guild.proto`，即 `UpdateGuildScore`、`GetGuildRank`、`GetGuildRankByGuild`。

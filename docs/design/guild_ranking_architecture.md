# Guild Ranking Architecture (2025-03-25)

## Problem
需要同时支持**全服排行榜**和**本服（per-zone）排行榜**，且在锁区/不锁区场景下都正确工作。

## Solution: Dual Redis ZSET Strategy

### Redis Keys
- **全服排行**: `guild_rank` — 所有公会，单个 ZSET
- **本服排行**: `guild_rank:zone:{zoneId}` — 每 zone 一个 ZSET

### 写入 (Pipeline 双写)
- `UpdateGuildScore`: Pipeline 同时 `ZADD guild_rank` + `ZADD guild_rank:zone:{zoneId}`
- `RemoveGuildFromRank`: Pipeline 同时 `ZREM` 两个 key
- `CreateGuild`: 初始化 score=0 写入两个 ZSET
- `DisbandGuild`: 从两个 ZSET 移除

### 读取 (zone_id 路由)
- `zone_id=0` → 读 `guild_rank` (全服)
- `zone_id>0` → 读 `guild_rank:zone:{zoneId}` (本服)
- 分页: `ZCARD` 得 total, `ZREVRANGE start stop` 取当前页, client 用 `ceil(total/page_size)` 算总页数

### 锁区/不锁区 无影响
- 排行榜数据隔离靠 `zone_id` 字段，不依赖锁区机制
- 全服排行是只读展示，不涉及跨区操作
- 不锁区时公会 `zone_id` 在创建时固定，本服排行依然准确

### Redis 架构
- 所有 guild 实例连同一个 **global Redis** (DB:2)，不是 per-zone Redis
- Redis Cluster 下单个 ZSET key 落在一个 slot/节点上，不存在跨分片问题
- 不需要额外的聚合/排行服务

## Data Model Changes
- `GuildData` 新增 `ZoneID uint32`
- MySQL `guild` 表新增 `zone_id INT UNSIGNED` + `KEY idx_zone`
- Proto: `CreateGuildRequest`, `GuildInfo`, `UpdateGuildScoreRequest`, `GetGuildRankRequest`, `GetGuildRankByGuildRequest` 均添加 `zone_id`

## When Would You Need a Separate Ranking Service?
- 只有当 guild 数据分散在 per-zone 独立 Redis 实例（不共享）时才需要聚合服务
- 当前架构用 global Redis，**不需要**

## Merge-Zone Tool (合服工具)

### Location
- Go CLI: `tools/merge_zone/main.go` (standalone Go module)
- PowerShell wrapper: `tools/scripts/merge_zone.ps1`
- Registered in `dev_tools.ps1` as `-Command merge-zone`

### What It Does
1. **Name conflict detection**: SQL JOIN to find guilds with same name in source and target zones
2. **MySQL migration**: primary-key point updates over the manifest written before any write — `UPDATE guild SET zone_id = <target> WHERE guild_id = ? AND zone_id = <source>`, ascending `guild_id`, one autocommit statement per guild — then invalidate `guild:v2` caches and re-count the source zone (still > 0 → abort, step not marked done). The session runs at READ COMMITTED; a row hitting 1213 / 1205 is retried a bounded number of times (capped exponential backoff, cancellable); any failure aborts with the merge fences **left in place** and logs the resume steps (GET to check run_id → DEL both fences → re-run). Why not a zone-wide `UPDATE ... WHERE zone_id = <source>`: it walks `idx_guild_0` (secondary entry first, then the primary key), the reverse of the online `DisbandGuild` (primary key `FOR UPDATE` → DELETE removes the secondary entry), so the two deadlock (1213) — same lock-order class as [`docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md`](../ops/incident-friend-lock-order-deadlock-2026-09-21.md)
3. **Redis ZSET merge**: `ZRANGEWITHSCORES` source → `ZADD` target → `DEL` source key

### Usage
```powershell
# Preview (safe)
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeSourceZone 102 -MergeTargetZone 101 -DryRun

# Execute (requires typing 'merge' to confirm)
pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeSourceZone 102 -MergeTargetZone 101
```

### Properties
- **Idempotent**: safe to re-run (MySQL UPDATE is no-op for already-migrated rows, Redis ZADD overwrites)
- **Conflict-safe**: name-conflict guilds are logged and skipped (resolve manually)
- **Global ZSET untouched**: only per-zone ZSETs are merged; `guild_rank` (global) already has all guilds

# 公会排行榜架构 (2025-03-25)

## 问题
需要同时支持**全服排行榜**和**本服（per-zone）排行榜**，且在锁区/不锁区场景下都正确工作。

## 方案：双 Redis ZSET 策略

### Redis 键
- **全服排行**: `guild_rank` — 所有公会，单个 ZSET
- **本服排行**: `guild_rank:zone:{zoneId}` — 每个区一个 ZSET

### 写入（Pipeline 双写）
- `UpdateGuildScore`：Pipeline 同时 `ZADD guild_rank` + `ZADD guild_rank:zone:{zoneId}`
- `RemoveGuildFromRank`：Pipeline 同时 `ZREM` 两个 key
- `CreateGuild`：初始化 score=0，写入两个 ZSET
- `DisbandGuild`：从两个 ZSET 移除

### 读取（zone_id 路由）
- `zone_id=0` → 读 `guild_rank`（全服）
- `zone_id>0` → 读 `guild_rank:zone:{zoneId}`（本服）
- 分页：`ZCARD` 获取总数，`ZREVRANGE start stop` 取当前页，客户端用 `ceil(total/page_size)` 计算总页数

### 锁区/不锁区无影响
- 排行榜数据隔离靠 `zone_id` 字段，不依赖锁区机制
- 全服排行是只读展示，不涉及跨区操作
- 不锁区时公会 `zone_id` 在创建时固定，本服排行依然准确

### Redis 架构
- 所有公会实例连同一个 **全局 Redis**（DB:2），不是 per-zone Redis
- Redis Cluster 下单个 ZSET key 落在一个 slot/节点上，不存在跨分片问题
- 不需要额外的聚合/排行服务

## 数据模型变更
- `GuildData` 新增 `ZoneID uint32`
- MySQL `guild` 表新增 `zone_id INT UNSIGNED` + `KEY idx_zone`
- Proto：`CreateGuildRequest`、`GuildInfo`、`UpdateGuildScoreRequest`、`GetGuildRankRequest`、`GetGuildRankByGuildRequest` 均添加 `zone_id`

## 什么时候需要独立的排行服务？
- 只有当公会数据分散在各区独立的 Redis 实例（不共享）时才需要聚合服务
- 当前架构使用全局 Redis，**不需要**

## 合服工具

### 位置
- Go CLI：`tools/merge_zone`（`go mod` 独立小模块，见同目录 `go.mod`）
- PowerShell 封装：`tools/scripts/merge_zone.ps1`（会 `go build` 出 `merge_zone[.exe]`）
- 玩家归属重映射的 **RPC 路径**：`DataService/RemapHomeZoneForMerge`（与 Data Service 联机时可用 `grpcurl` 调用，逻辑与下款 CLI 的 mapping 步骤一致）

### 功能
1. **名称冲突检测**：SQL JOIN 查找源区和目标区中同名的公会
2. **MySQL 迁移**：按写前落盘的清单，以 `guild_id` 升序逐条主键点更新 `UPDATE guild SET zone_id = <target> WHERE guild_id = ? AND zone_id = <source>`（每条自动提交），再失效 `guild:v2` 缓存并复查源区公会数（仍 > 0 则中止、不标记步骤完成）；MySQL 会话固定 READ COMMITTED，单行遇 1213 / 1205 有界重试（指数退避封顶、可取消），任何失败都**保留合服围栏**中止，并在日志给出续跑指引（GET 核对 run_id → DEL 两把围栏 → 原命令重跑）。不用整区 `UPDATE ... WHERE zone_id = <source>` 的原因：它经 `idx_guild_0` 先锁二级项、再回表锁主键，与在线 `DisbandGuild`「主键 FOR UPDATE → DELETE 删二级项」反序，两者会死锁（1213），同类锁序问题见 [`docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md`](../ops/incident-friend-lock-order-deadlock-2026-09-21.md)
3. **Redis 本服 ZSET 合并**：`ZRANGE` 源区 `guild_rank:zone:*` → `ZADD` 目标区 → `DEL` 源区 key
4. **（多集群必做）玩家存档串键复制**：按 `data_service` 的 `player:{playerId}:*` 字符串键，从**源区数据 Redis** 拷到**目标区数据 Redis**，**必须在改 `player:zone` 之前**完成；若源/目标 endpoint+DB 完全相同则工具自动跳过（等同单 Redis 开发环境）
5. **玩家 `home_zone` 映射**：`player:zone:{playerId}` 在 **mapping Redis**（`-mapping-redis-*`）

### 用法
```powershell
# 只预览（不写库、不写 Redis）
go run -C tools/merge_zone . -source-zone=102 -target-zone=101 -dry-run

# 真正执行前务必 dry-run 通过；再显式 -apply
go run -C tools/merge_zone . -source-zone=102 -target-zone=101 -apply

# 分区数据在不同 Redis 集群时：先拷存档串键，再改归属（与工具内顺序一致）
go run -C tools/merge_zone . -source-zone=102 -target-zone=101 -dry-run -migrate-player-blobs `
  -source-data-redis=127.0.0.1:7001 -target-data-redis=127.0.0.1:7004 -data-redis-password=xxx

# 或 PowerShell
pwsh -File tools/scripts/merge_zone.ps1 -SourceZone 102 -TargetZone 101 -DryRun
pwsh -File tools/scripts/merge_zone.ps1 -SourceZone 102 -TargetZone 101 -Apply
```

可跳过任一步：`-skip-guild-mysql`、`-skip-guild-rank`、`-skip-player-mapping`、`-skip-player-blob-migration`（便于分阶段执行或只补跑某步）。

### 特性
- **幂等**：可安全重复执行（MySQL UPDATE 对已迁移行无操作，Redis ZADD 会覆盖，mapping 为覆盖写）
- **冲突安全**：同名冲突公会会被记录并跳过（需手动处理）
- **全服 ZSET 不受影响**：只合并本服 ZSET；`guild_rank`（全服）已包含所有公会
- **非 dry-run 必须带 `-apply`**，避免误跑生产

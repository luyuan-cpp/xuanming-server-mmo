# 公会与好友服务实现笔记 (2025-03-25)

## 已创建的服务

### 公会服务 (`go/guild/`)
- **端口**: 50300
- **Redis**: 全局 Redis DB:2（跨区）
- **节点类型**: GuildNodeService (node.proto)
- **功能**: 创建/获取/加入/离开/解散/设置公告 + 排行榜 (UpdateScore/GetRank/GetRankByGuild)
- **模式**: cache-aside + singleflight (Redis → MySQL → 回写)
- **排行榜**: 双 ZSET（全局 + 按区），详见 `guild_ranking_architecture.md`
- **ID 生成**: bwmarrin/snowflake

### 好友服务 (`go/friend/`)
- **端口**: 50400
- **Redis**: 全局 Redis（跨区）
- **节点类型**: FriendNodeService=27 (node.proto)
- **功能**: 添加好友/接受好友/拒绝好友/删除好友/获取好友列表/获取待处理请求
- **模式**: cache-aside + singleflight
- **校验**: 不能加自己、已是好友检查、好友列表已满检查、重复申请检查

## Proto 集成
- 两个服务均已添加到 `tools/proto_generator/protogen/etc/proto_gen.yaml` 的 `domain_meta` 中，`rpc.type: grpc`
- Proto-gen 每个服务生成约 96 个 .pb.go 文件
- **关键注意点**: `tip.proto` 没有 package 声明——在 proto 文件中必须使用 `TipInfoMessage`（而非 `common.base.TipInfoMessage`）
- **Go 导入路径**: `proto/common/base`（而非 `proto/common`），因为生成的代码位于 `proto/common/base/`

## MySQL 表结构 (deploy/mysql-init/guild_friend_tables.sql)
- `guild`（主键 guild_id，唯一键 name，zone_id）
- `guild_member`（主键 guild_id+player_id）
- `friend`（主键 player_id+friend_player_id）
- `friend_request`（主键 from_player_id+to_player_id，索引 to_player_id+status）
- `friend_capacity`（好友硬上限的权威计数与显式锁行）
- `guild_schema_migration`（数据迁移 durable readiness gate）

## 一致性与授权门禁（2026-08-03）

- `friend_capacity_backfill_v1` 必须经历 `pending → ready`。全表归零、从
  `friend` 权威边回填以及 ready 标记在同一事务提交；服务启动和每个容量写事务
  都检查 gate。表存在但 gate 缺失/pending 时 fail-closed，缺失容量行也按
  `COUNT(friend)` 初始化，绝不猜 0。
- Redis 公会快照仅用于展示。`SetAnnouncement` 在一个 MySQL 事务内依次锁
  `guild` 与操作者的 `guild_member`，只允许权威 role 为 officer/leader 时更新；
  降权、退会后的陈旧 Redis 成员缓存不能继续授权。

## 架构决策：不单独拆分排行榜服务
- 公会使用全局 Redis → 单个 `guild_rank` ZSET 天然就是全服的
- 按区排行通过 `guild_rank:zone:{zoneId}` ZSET 实现
- Redis Cluster：单个 key = 单个 slot，不存在跨分片问题
- 只有当公会数据分散在按区 Redis 实例中时，才需要独立的排行榜服务

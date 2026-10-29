# 公会与好友服务实现笔记

> 创建于 2025-03-25；2026-10-23 按当前源码修订。本文说明现有存储与调用路径，不表示已核验某个运行环境的部署状态。详细契约见 [好友持久化架构](./friend-persistence-architecture.md)、[帮会二期交接](./guild-phase2/92-handoff.md)与[公会架构说明](./guild-actor-architecture.md)。

**当前两者都直接通过 SQL 事务维护权威数据，生产要求连接 TiDB。Redis 仍在使用，但不是好友关系、公会成员或公会资产的持久化事实源；进程内也没有以 GuildActor 为权威、定时异步刷盘的实现。**

| 项目 | 公会服务 `go/guild/` | 好友服务 `go/friend/` |
|---|---|---|
| 默认 RPC 端口 | 50300 | 50400 |
| 节点类型 | `GuildNodeService=15` | `FriendNodeService=27` |
| 独占逻辑库 | `mmorpg_guild` | `mmorpg_friend` |
| 权威写入 | SQL 事务直接提交；生产 TiDB | SQL 事务直接提交；生产 TiDB |
| 列表与展示读取 | `GetGuild` / `GetPlayerGuildID` 使用 Redis cache-aside，未命中读 SQL，再按缓存代次回填 | 好友列表与待处理申请直接读 SQL，跳过旧关系缓存 |
| Redis 主要职责 | 公会展示快照、排行榜投影，以及会话定位、同步摘要等辅助状态 | 会话与在线投影、频率配额、资料等派生数据；旧关系缓存仅保留失效兼容 |
| 进程内存职责 | 请求处理、连接池、缓存加载互斥等运行时状态 | 请求处理、连接池等运行时状态 |

## 公会服务当前路径

- 已实现管理、入帮审批、资产与商店、活动和排行榜等功能；RPC 契约见 [guild.proto](../../proto/guild/guild.proto)。公会 ID 默认通过 data_service 的 `AllocateIdSegment` 领取 `guild` 号段；当前 `IdSegment.Enabled=true`、`FallbackToSnowflake=false`，号段不可用时拒绝建帮，策略见 [guild_id_minter.go](../../go/guild/internal/svc/guild_id_minter.go)。
- **写路径**：业务校验与授权在 SQL 事务里复核，只写发生变化的字段或关系；提交后失效相关 Redis 展示缓存。例如公告更新在同一事务锁公会与操作者成员行，只允许权威角色为 officer/leader 时修改；陈旧成员缓存不能继续授权。禁止把 Redis 快照改完后整份写回 SQL。建帮、公告等管理路径见 [guild_repo.go](../../go/guild/internal/data/guild_repo.go) 与 [guild_manage_repo.go](../../go/guild/internal/data/guild_manage_repo.go)。
- **展示读路径**：Redis → 未命中 → SQL → 校验缓存代次后回填 Redis。加载互斥用于合并冷读；这不等于每个公会有一个常驻 Actor，也不提供展示快照的强一致保证。权限、成员与资产判定仍须使用事务内的权威状态。
- **Redis 配置**：本地 `RedisClient.DB=2` 承载公会私有缓存与排行；`PlayerLocatorRedis` 单独读取跨运行时共享状态。DB 编号是配置约定，不能据此判断数据权威性；见 [guild.yaml](../../go/guild/etc/guild.yaml) 与 [servicecontext.go](../../go/guild/internal/svc/servicecontext.go)。
- **历练邀请也有 SQL 权威记录**：当前启动装配使用 `NewSQLTrialLobbyRepo`，邀请、同意、占位与终局写 SQL。旧 `NewTrialLobbyRepo` 仅保留给 Redis 隔离夹具，不能把它当生产入口；见 [guild.go](../../go/guild/guild.go)、[trial_lobby_sql.go](../../go/guild/internal/data/trial_lobby_sql.go)。

## 好友服务当前路径

- 已实现申请、接受、拒绝、删除、拉黑、解除拉黑、列表与推荐；RPC 契约见 [friend.proto](../../proto/friend/friend.proto)。
- **写路径**：在关系写事务前按玩家 ID 升序补齐容量行，缺行从 `COUNT(friend)` 的权威边数初始化；事务内按相同顺序锁双方容量行，再校验申请代次、双向拉黑、好友边和硬上限，申请状态、双向边与容量同事务提交。默认好友上限为 200；见 [friend_repo.go](../../go/friend/internal/data/friend_repo.go)。
- **读路径**：`GetFriendList` 与 `GetPendingRequests` 直接调用 SQL 加载函数，旧好友 / 申请列表 Redis 键不参与权威读。代码仍保留 cache-aside helper 和提交后失效逻辑，不能据此把当前列表描述成“Redis → SQL → 回填”。资料和在线状态在关系读取后补全，不能反过来授权关系变更。
- **Redis 句柄分工**：`SharedRedis` 读取 `player:session:{id}` 等跨运行时契约并维护同步摘要；`FriendRedis` 承载私有配额与派生数据，未配置时回落共享实例并告警；`LoginRedis` 用于全局登录会话校验。权威关系的 SQL 提交、Redis 失效和推送不在同一事务；失效或推送失败不能把已提交关系改报为未提交，也不能返回旧关系缓存充当成功读取。装配见 [servicecontext.go](../../go/friend/internal/svc/servicecontext.go)。
- `friend_capacity_backfill_v1` / `guild_schema_migration` 的旧共享库 readiness 门禁已于 2026-09-18 退役。friend 只在独占业务库维护关系，不再查询旧共享库 readiness 标记；当前容量守卫与锁序仍须保留。完整约束见[好友持久化架构](./friend-persistence-architecture.md)。

## TiDB 门禁与 schema 事实源

TiDB 使用 MySQL 协议，因此 `MySQL` 配置名、`OpenMySQL` / `load…FromMySQL` 方法名及 `database/sql` 驱动名不代表当前生产仍依赖单机 MySQL。

- **guild**：`MySQL.RequireTiDB` 缺省开启，本地 [guild.yaml](../../go/guild/etc/guild.yaml) 也显式为 `true`。业务池通过 [authoritysql](../../go/shared/authoritysql/pool.go) 逐物理连接固定 `READ-COMMITTED`、悲观事务与当前强读会话，后端或会话不符合约束时拒绝启动。仅 dev/test 可显式关闭 TiDB 门禁。
- **friend**：生产配置由 [config.Validate](../../go/friend/internal/config/config.go) 强制 `RequireTiDB=true`；[servicecontext.go](../../go/friend/internal/svc/servicecontext.go) 对每条物理连接初始化并复核悲观事务、RC、strict、leader，关闭历史快照、陈旧读与低精度 TSO。本地 [friend.yaml](../../go/friend/etc/friend.yaml) 仍保留 `Mode: dev`、`RequireTiDB: false` 的 MySQL 开发兼容配置，不能据此宣称所有本地实例都已连接 TiDB。
- **事务域**：全服各副本连接同一个 TiDB 权威事务域；涉及全局角色原件校验时，显式配置的 `account_role` 目录也须在同一事务域内。独占逻辑库不等于按 zone 分散到多个独立数据库实例。见 [guild 角色守卫](../../go/guild/internal/data/role_trade_guard.go)与 [friend 角色守卫](../../go/friend/internal/data/role_identity.go)。
- **guild schema**：事实源为 [guild_db.proto](../../proto/guild/guild_db.proto)，完整表清单由 [Tables](../../go/guild/internal/data/tables.go) 声明，涵盖关系、管理、资产与活动等数据。
- **friend schema**：事实源为 [friend_table.proto](../../proto/friend/friend_table.proto)，四张业务表为 `friend`、`friend_request`、`friend_capacity`、`friend_block`，清单见 [Tables](../../go/friend/internal/data/tables.go)。
- 两者均经 `go/schemamigrate` 管理结构；旧共享库初始化脚本 `deploy/mysql-init/guild_friend_tables.sql` 不是当前 schema 事实源。结构变更按各服务的 `-migrate` 入口与配置操作，不手改生成产物。

## 排行榜当前实现与一致性边界

排行榜目前由公会服务提供，不能把旧文档里关于拆分服务或 Redis Cluster 的选型推论当作已落地架构。

- `UpdateGuildScore` 先在 SQL 事务里更新 `guild.score`，再更新 Redis 全局 `guild_rank` 与按区 `guild_rank:zone:{zoneId}` 双 ZSET；归属区以 SQL 为准，不相信调用方传入的 zone。
- `GetGuildRankPage` / `GetGuildRank` 查询 Redis ZSET，排行展示仍是派生投影，不是每次直接用 SQL 排序。
- SQL 提交与 ZSET 同步不原子。Redis 更新失败时 SQL 分数可能已经提交，不能以 RPC 错误推断回滚；投影可能暂时缺项或陈旧。`RebuildRanks` 从 SQL 权威快照重建全局与各区榜，当前在启动时执行。
- 代码依据：[guild_repo.go](../../go/guild/internal/data/guild_repo.go) 的 `UpdateGuildScore` / `RebuildRanks` / `GetGuildRankPage` / `GetGuildRank`，以及 [guild.go](../../go/guild/guild.go) 的启动装配。

## Proto 集成

- 两个服务均在 [proto_gen.yaml](../../tools/proto_generator/protogen/etc/proto_gen.yaml) 的 `domain_meta` 中配置 `rpc.type: grpc`。
- 协议生成物统一位于 `go/proto/`，通过仓库 proto-gen 管线生成，不按服务手写另一套 `.pb.go`。未获客户端任务授权时按 `AGENTS.md §4` 使用 no-unity 生成配置。
- [tip.proto](../../proto/common/base/tip.proto) 没有 package 声明，proto 中使用 `TipInfoMessage`；共享 Go 导入路径为 `proto/common/base`。tip 错误码仍只从 `data/tip/Tip.xlsx` 导表生成。

## 本次核对范围

本次仅按 2026-10-23 工作区源码、配置和现有设计契约修正文档；未改代码或配置，未启动服务、编译、运行测试或核验实际数据库后端，未新增性能结论。

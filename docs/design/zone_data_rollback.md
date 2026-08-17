# Zone 数据回档方案

> **修订(2026-08-15)**: 数据层迁 TiDB 后,灾难恢复级的"MySQL PITR"须改为 TiDB BR/PiTR(runbook 未改写前**不许迁生产**);Phase 2 单一全局库后 zone 粒度回档改由应用级按 home_zone 承担。见 [global-data-layer-tidb-decision.md](./global-data-layer-tidb-decision.md) §D8。

## 概述

Zone 数据回档分两个层面：
1. **应用级回档**（RPC/逻辑骨架已实现，生产执行暂停）：通过 `RollbackZone` RPC，从快照恢复玩家数据
2. **灾难恢复级**（Ops 操作）：MySQL PITR + Redis flush + Kafka offset reset

> **当前安全状态（2026-08-03）**：应用级 `RollbackPlayer` / `RollbackZone` /
> `RollbackAll` 在生产 `ServiceContext` 中没有可用的跨服务离线 epoch 栅栏，
> 因此会以 `error_code=16 (NotImplemented)` **fail-closed**，不修改任何玩家
> 数据。这不是“先查一次玩家是否在线”能代替的问题。

---

## 1. 单玩家回档（应用级）

详见 [single_player_rollback.md](single_player_rollback.md)。

- 踢玩家下线 → 找目标时间点之前最近的快照 → 调用各服务 `ImportPlayerData()` 恢复
- 自动校验交易日志防止道具复制（有正常转移记录的物品不恢复）
- 本质是**补偿式**回档，不是时间倒流

---

## 2. 整 Zone 应用级回档（`RollbackZone` RPC）

### 必须先闭合的在线写栅栏

当前 Scene 的后续存盘会经 Kafka 进入 `go/db` 并落 MySQL，而
`data_service` 的应用级回档修改 Redis。如果只在回档前查一次
“当前离线”，会同时存在两个窗口：

- 查完后玩家可以重新登录/Scene 激活（TOCTOU）；
- 旧 Scene epoch 在回档前已发出、但尚未落库的存盘，会在回档后
  再次覆盖结果。

因此 `RollbackFence` 的契约必须是原子的跨服务离线 epoch：

1. 阻止目标 player/zone 的新登录和 Scene 激活；
2. 让当前 Scene 下线并排空它已发出的 Kafka/DB 写；
3. 存盘消息携带 epoch，DB 落库端拒绝旧 epoch；
4. 栅栏覆盖“意图审计 → pre-rollback 安全快照 → 恢复 → 结果审计”
   全过程，持久化完成后才释放。

当前仓库尚没有 login / player_locator / Scene / db 共同实现这个协议。
`data_service` 仅保留了可注入的契约接口；生产不注入实现，所以执行路径
默认禁用。单元测试可注入 fake fence，验证缺栅栏、安全快照失败、
审计失败时均不会静默继续。

### 设计决策：Guild/Friend 不回档

**结论：Guild/Friend 数据不需要回档。**

**原因**：
- Player Redis blob（`player:{id}:player_database`）**不包含** guild_id 或 friend 引用
- Guild 通过 `GuildService` gRPC 独立查询，Friend 通过 `FriendService` gRPC 独立查询
- 两个系统的数据完全解耦，回档 player 数据不会与 guild/friend 产生不一致
- 即使存在时间差异（如玩家回档到加入公会前，但 guild_member 表仍有记录），
  这类不一致通过正常的 guild/friend 操作即可自愈（退出重进、重新申请等）

### 无快照角色处理（fail-closed）

`RollbackZone` **不会再自动删除任何无快照角色**。快照只在 GM、事件和回档
安全点产生，仓库内没有周期性快照任务；因此“目标时间前没有快照”不能证明
“角色是在目标时间后创建的”。把两者等同会删除大量正常老玩家。

| 情况 | 处理 |
|------|------|
| T 之前创建的角色 | 从 player_snapshot 恢复数据 ✅ |
| 目标时间前没有快照的角色 | 仅列入 `orphan_player_ids` 候选名单，不恢复、不删除 |
| 候选角色的 Redis / zone mapping | 保持原样 |
| 候选角色的账号 / guild / friend | 保持原样 |

字段名 `orphan_player_ids` 为保持 wire compatibility 暂不改名；它的当前语义是
“需要人工核对的无快照候选”，**不是已确认的新建角色，更不是已清理结果**。
`orphans_cleaned` 恒为 0。若以后恢复自动清理，必须先引入权威角色创建时间，
并走独立审批、pre-delete 快照和可恢复删除流程。

### 执行流程（栅栏落地后，2 阶段）

```
Preflight: 获取跨服务离线 epoch 栅栏 + 写 STARTED 审计
→ 任一前置失败则停止，零玩家数据变更

Phase 1: 恢复 player 数据
────────────────────────
遍历 zone 内所有有快照的 player
→ 对每个 player: 找 target_time 之前最近的 player_snapshot
→ 创建 pre-rollback 安全快照
→ 用 snapshot 数据覆盖 Redis

Phase 2: 报告无快照候选
──────────────────────
SCAN mapping Redis, 找到该 zone 下所有有 zone mapping 的 player
→ 对比 snapshot 中的 player 列表，得到差集（候选）
→ 将候选写入响应，所有数据和映射保持不变

Finalize: 写 RESULT 审计 → 释放 epoch 栅栏
```

### 代码位置

| 文件 | 作用 |
|------|------|
| `go/data_service/internal/logic/rollback_logic.go` | RollbackZone（player 回档 + 无快照候选报告） |
| `go/data_service/internal/routing/router.go` | GetAllPlayerIDsInZone（SCAN mapping Redis） |
| `go/data_service/internal/store/snapshot_store.go` | player_snapshot + rollback_audit_log CRUD |
| `proto/data_service/data_service.proto` | RollbackZone RPC 定义 |

### 跨服务调用链

```
RollbackZone (data_service)
  ├── RollbackFence: 阻断新会话 + 排空/拒绝旧 epoch 存盘（尚未落地）
  ├── Phase 1: 恢复每个 player 的 Redis 数据（从 snapshot）
  └── Phase 2: 返回无快照候选名单（零删除、零账号变更）
```

> **安全边界**：候选名单只能用于人工核对，调用方不得把它解释成“已经清理”。

---

## 3. 整 Zone 灾难恢复级回档（Ops 操作）

### 架构前提

每个 zone 在 K8s 中是独立 namespace（`mmorpg-zone-{zoneName}`），拥有独立的：
- MySQL 实例（数据库名 `game`）
- Redis 实例（`--appendonly yes`）
- Kafka broker / topic（`db_task_topic`）

写路径：`C++ node → Kafka(db_task_topic) → Go db service → MySQL`，Redis 仅为缓存加速层。

### 操作步骤

```
步骤 1: 关闭 zone（停止所有业务写入）
─────────────────────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName <zone>

步骤 2: MySQL Point-in-Time Recovery (PITR)
────────────────────────────────────────────
# 先恢复到影子库验证
mysqlbinlog --stop-datetime="2026-03-26 10:00:00" binlog.000xxx | mysql -h shadow-host -u root -p game

# 确认无误后替换正式库
# 方式 A: 云 RDS 控制台一键 PITR
# 方式 B: 自建 MySQL 用 binlog 手动恢复

步骤 3: Redis 处理
──────────────────
# Redis 是缓存层，C++ 节点启动时会从 MySQL 重建，直接清空即可
redis-cli -h <redis-host> FLUSHDB

# 或从 AOF/RDB 备份恢复到对应时间点（如果有归档的话）

步骤 4: Kafka 处理
──────────────────
# 清空 db_task_topic，防止回档时间点之后的写入消息被重放
# 方式 A: 删除并重建 topic
kafka-topics.sh --delete --topic db_task_topic --bootstrap-server <broker>
kafka-topics.sh --create --topic db_task_topic --partitions 5 --bootstrap-server <broker>

# 方式 B: 重置 consumer group offset 到指定时间戳
kafka-consumer-groups.sh --reset-offsets --to-datetime 2026-03-26T10:00:00.000 \
  --group db_rpc_consumer_group --topic db_task_topic --bootstrap-server <broker> --execute

步骤 5: 重新拉起 zone
─────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up \
  -ZoneName <zone> -ZoneId <id> -NodeImage <image> -WaitReady
```

### 关键安全点

| 要点 | 说明 |
|------|------|
| Redis 可安全清空 | 它是缓存层，节点启动后从 MySQL 重建 |
| Kafka 必须处理 | 不清空 topic/offset 会导致旧消息重放覆盖回档数据 |
| MySQL 必须先恢复 | 它是唯一的持久化真相源（source of truth） |
| 串行保证 | Kafka partition key = `player_id`，同玩家写入串行执行 |

---

## 3. 当前缺失基建

| 缺失项 | 状态 | 说明 | 优先级 |
|--------|------|------|--------|
| MySQL 定时备份 + binlog 归档 | ✅ **已落地 (2026-05-15)** | `deploy/k8s/manifests/infra/mysql.yaml`(PVC + binlog on)+ `mysql-backup-cronjob.yaml`(每天 03:17 UTC dump + binlog 复制),SOP 见 [`docs/ops/mysql-backup-pitr-runbook.md`](../ops/mysql-backup-pitr-runbook.md) | ~~高~~ |
| Redis RDB/AOF 定期快照归档 | 未做 | 当前 `--appendonly yes` 保证持久化，但无历史时间点恢复能力 | 低（可 FLUSHDB） |
| Kafka offset 回档工具 | ✅ **已落地 (2026-05-15)** | `tools/scripts/kafka_offset_reset.ps1`(支持 to-datetime / to-earliest / to-latest / delete-and-recreate-topic 4 种模式,默认 dry-run),注册为 `dev_tools.ps1 -Command kafka-offset-reset` | ~~中~~ |
| 一键 zone 回档脚本 | ✅ **已落地 (2026-05-15)** | `tools/scripts/k8s_zone_rollback.ps1`(7 步:zone-down / Kafka drain / MySQL PITR 提示暂停 / Redis FLUSHDB / kafka-offset-reset / zone-up / 验证清单),注册为 `dev_tools.ps1 -Command k8s-zone-rollback`,默认 dry-run | ~~中~~ |
| 备份异地归档(S3/OSS) | 未做 | PVC 只防 pod 重启,不防整个集群 / 机房挂 | 高(生产前必做) |
| 跨服务离线 epoch 回档栅栏 | 未做（应用级回档 fail-closed） | login/player_locator 阻断新会话，Scene 排空存盘，Kafka/db 携带并校验 epoch | P0（重启应用级回档前） |

---

## 4. 生产环境建议

- **云 MySQL（RDS）**：自带自动备份和 PITR，回档操作在控制台点几下即可，**强烈推荐**
- **自建 MySQL**：必须配置 `log-bin`、定期 `mysqldump` CronJob、binlog 归档到对象存储
- **Redis**：zone 回档时直接 FLUSHDB，无需额外备份
- **Kafka**：回档后最安全的做法是清空 `db_task_topic`，避免旧消息重放

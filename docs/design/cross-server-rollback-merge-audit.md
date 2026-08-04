# 跨服 / 回档 / 合服 — 现状盘点与差距分析

> **生成日期**: 2026-05-17
> **目的**: 在动手"跨服回档"和"合服"两件 L 级事之前,先把已有代码和设计文档摸清楚,识别真正缺什么,避免重造轮子。
>
> **结论**: 这三块基础设施做得比预想完整很多。**不是"从零开始"**,而是补完最后的几条缝。
>
> **2026-08-03 安全更正**: 本文 §1.3 记录的是当时实现，不能作为当前
> `RollbackZone` 契约。由于仓库没有周期快照，“无目标时间前快照”无法证明
> “角色在目标时间后创建”；当前实现只返回候选名单，绝不删除 Redis、zone
> mapping 或账号关系。权威说明见 `zone_data_rollback.md`。

---

## 一、跨服架构 — 已有

### 1.1 核心原则已固化

`docs/design/cross_server_architecture_principle.md`(121 行)+ `mmo_cross_server_architecture.md`(358 行)定下了红线:

- **位置透明** — 业务层永远不分支"是否跨服"
- **数据不迁移** — 玩家数据始终在 home zone,跨服时通过 Data Service 路由
- **Single Writer** — 任意时刻只有一个 Scene 写入某玩家
- **player_id 永不编码 zone** — 合服时 ID 不变

这些原则**已经被代码强制执行**:

| 检查项 | 落地证据 |
|---|---|
| Data Service 是唯一路由层 | `go/data_service/internal/routing/router.go` |
| 跨服时 Scene 切换序列化 | `cross_scene_player_messaging.md` §14 + `route_message_response_handler.cpp` |
| 整 Zone 数据隔离 | `db_zone_isolation.md`: Kafka topic `db_task_zone_{id}` / MySQL `zone_{id}_db` 已派生 |
| player_id 不含 zone 信息 | `cpp/libs/engine/thread_context/snow_flake_manager.cpp` — node_id 是物理节点,非 zone |

### 1.2 已有的 Data Service RPC 面

`proto/data_service/data_service.proto` 已经定义:

- `RollbackPlayer(player_id, snapshot_id, target_time, scope, fields)` — 单玩家回档
- `RollbackZone(zone_id, target_time, reason, operator)` — 整 Zone 回档
- `RollbackAll(...)` — 全服回档
- `BatchRecallItems` / `QueryTransactionLog` — 配套追缴/审计

### 1.3 已有的回档代码(394 行)

`go/data_service/internal/logic/rollback_logic.go`:

- `RollbackPlayer` — 找快照 → 创建 pre-rollback 安全快照 → ImportPlayerData → 审计
- `RollbackZone` — 两阶段:
  - Phase 1: 遍历有快照的玩家,覆盖 Redis
  - Phase 2: SCAN mapping Redis 找孤儿(target_time 之后创建的角色) → 删 zone mapping + Redis 数据
  - Phase 2b: 通过 gRPC `RemovePlayersFromAccounts` 让 login 端从账号反向索引里删除孤儿 player_id

### 1.4 已有的合服工具(538 行)

`tools/merge_zone/`:

- `main.go`(363 行) — 完整 CLI,带 `-dry-run` / `-apply` 双重保护
- `player_blob_migrate.go`(147 行) — `player:{id}:*` 字符串 key 跨 Redis 集群迁移
- `merge_zone_test.go`(28 行) — 测试占位

**已做的 4 步合服流程**:

| 步骤 | 实现 |
|---|---|
| 1. 列出 source zone 的所有玩家 | `collectPlayerIDsWithHomeZone` |
| 2. 跨 Redis 集群拷贝 player blob(可选) | `copyPlayerStringKeys` — **顺序很关键**:**必须在 remap 之前**做,否则 DataService 读新 zone 会读空 |
| 3. MySQL guild 表 zone_id 改写 + 名字冲突检测 | `migrateGuildZone` + `checkNameConflicts` |
| 4. Redis 公会排行榜 ZSET 跨 zone 合并 | `mergeRankingZSET` |
| 5. `player:zone:*` mapping 改写 | `remapPlayerMapping` |

**入口**: `pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone -MergeSourceZone <id> -MergeTargetZone <id>`(`dev_tools.ps1:91-95` 已有参数,`:588` 有用法说明)

### 1.5 已有的灾难恢复级回档(`zone_data_rollback.md`)

完整的 ops 操作流程,包含:
- K8s `k8s-zone-down` / `k8s-zone-up` 已有
- MySQL PITR 操作步骤
- Redis FLUSHDB 步骤(注释明确说 Redis 是缓存层,可安全清空)
- Kafka topic 删重建 / consumer group offset 重置

---

## 二、真正的差距 — 没做完的部分

诚实评估,把 todo 里相关的事项和已有实现交叉比对,**真正缺的不多**:

### 2.1 跨服回档 — 缺什么

| 缺口 | 工作量 | 优先级 |
|---|---|---|
| **A. 跨 zone 的 `RollbackPlayer`**(玩家 home_zone 在 zone X,但需在 zone Y 上发起回档) | M | 中 |
| **B. 跨服节点崩溃后的"未刷写状态"回滚** — 玩家在 zone Y 战斗中,zone Y scene 节点崩溃,玩家最后的状态没存到 home zone Redis | L | 高 |
| **C. 整 Zone 回档的 Kafka 一致性** — `zone_data_rollback.md` 已写步骤,但**没自动化脚本** | S | 中 |
| **D. 一键 zone 回档脚本** `dev_tools.ps1 -Command k8s-zone-rollback` | S | 中 |
| **E. MySQL 定时备份 + binlog 归档 CronJob** | S(K8s YAML) | **高** |
| **F. 回档审计仪表盘** — 已有 `rollback_audit_log` 存储,缺 Grafana 面板 | S | 低 |

**B 是真正的硬骨头** — 涉及到"玩家临死前最后一刻状态丢失"的灾难恢复,需要更高频的 incremental snapshot 或 transaction log replay。

### 2.2 合服 — 缺什么

| 缺口 | 工作量 | 优先级 |
|---|---|---|
| **G. 玩家昵称冲突解决** — 合服后两个 zone 同名玩家,代码没显式处理 | M | **高** |
| **H. 玩家通知机制** — 合服后玩家登录要看到"你的 zone 已合并到 X"提示 | S | 中 |
| **I. 合服前的灰度演练流程** — `-dry-run` 已有,缺端到端的"合服前 checklist" runbook | S | 高 |
| **J. 邮件 / 帮派申请 / 拍卖等"指向 player_id"的资源** — `merge_zone` 主要处理 guild + player mapping,**其他资源**(邮件附件、好友、聊天历史等)是否完整覆盖未审计 | M | **高** |
| **K. 跨 zone 数据不一致检测** — 合服前后跑一遍校验脚本 | M | 中 |
| **L. 回滚合服** — 合服执行后发现错了,能不能撤回?目前看是不能 | L | 中 |

**G + J 是最大风险** — 玩家投诉最严重的就是"我合服后名字不能用了"和"我合服后邮件不见了"。

### 2.3 优先级排序(按 ROI)

| Tier | 项 | 工作量 | 依赖 |
|---|---|---|---|
| **P0** | E. MySQL 备份 CronJob | S | 无,纯 K8s YAML |
| **P0** | G. 玩家昵称冲突解决 | M | 无 |
| **P0** | J. 资源全量审计(邮件/好友/拍卖等) | M | 无 |
| **P1** | B. 跨服节点崩溃状态回滚 | L | 现有 snapshot 频率 |
| **P1** | I. 合服 runbook + checklist | S | G + J 先完成 |
| **P2** | A. 跨 zone 主动 RollbackPlayer | M | 无 |
| **P2** | C. Kafka offset 重置脚本化 | S | 无 |
| **P2** | D. k8s-zone-rollback 一键 | S | C 先完成 |
| **P2** | K. 不一致检测 | M | 无 |
| **P3** | H. 合服通知提示 | S | 客户端配合 |
| **P3** | F. 回档审计 Grafana 面板 | S | 无 |
| **P3** | L. 撤回合服 | L | 极低 |

---

## 三、给你的建议

**不要"动手写跨服回档系统"**, 它**已经写了** — 看 `rollback_logic.go::RollbackZone`(394 行)。

**也不要"动手写合服工具"**, 它**已经写了** — 看 `tools/merge_zone/main.go`(363 行)。

**真正缺的事**:

1. **审计**: 跑一遍 `tools/merge_zone/` 看看玩家邮件/好友/拍卖/聊天历史等资源是否完整迁移。这是 P0,半天工作量。
2. **昵称冲突**: 设计合服时的重名玩家强制改名策略(后缀 `_zone{id}` / 让玩家登录时选)。
3. **CronJob 化 MySQL 备份**: 真正生产部署前的必备。

**这三件,我可以一次性出"差距修复设计文档"+ 必要时改代码**。但都需要你点头我才动 — 因为每件都涉及决策(改名策略、备份频率、审计范围)。

---

## 参考索引

| 已有文档 | 关键内容 |
|---|---|
| `cross_server_architecture_principle.md` | 跨服红线 + 路由原则 |
| `mmo_cross_server_architecture.md` | 完整跨服设计基线(358 行) |
| `single_player_rollback.md` | 单玩家回档 + 防复制 + 补缴系统 |
| `zone_data_rollback.md` | 整 Zone 回档应用级 + ops 灾难恢复级 |
| `db_zone_isolation.md` | Kafka topic / MySQL DB 按 zone 隔离 |
| `enter-scene-zone-routing.md` | 场景切换的 zone 路由 |

| 已有代码 | 行数 / 主要符号 |
|---|---|
| `go/data_service/internal/logic/rollback_logic.go` | 394 行;`RollbackPlayer` / `RollbackZone` / `cleanupOrphanCharacters` |
| `go/data_service/internal/routing/router.go` | `RemapHomeZoneForMerge` + `AcquirePlayerLock` |
| `tools/merge_zone/main.go` | 363 行;5 步合服流程 |
| `tools/merge_zone/player_blob_migrate.go` | 147 行;跨 cluster blob 拷贝 |
| `proto/data_service/data_service.proto` | `RollbackPlayer/Zone/All` + `BatchRecallItems/QueryTransactionLog` |

| 现存运维入口 | 命令 |
|---|---|
| 合服 | `dev_tools.ps1 -Command merge-zone -MergeSourceZone <id> -MergeTargetZone <id>` |
| Zone 下线 | `dev_tools.ps1 -Command k8s-zone-down -ZoneName <zone>` |
| Zone 上线 | `dev_tools.ps1 -Command k8s-zone-up -ZoneName <zone> -ZoneId <id>` |

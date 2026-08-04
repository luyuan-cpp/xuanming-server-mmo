# Single-Player Rollback (单人回档) — MMO/MOBA Microservice Architecture

> **Discussion Date**: 2025-03-25
> **Context**: MMO/MOBA microservice architecture rollback design discussion

## ⚠️ 实施现状(Reality Check, 2026-05-23)

> 这份文档讨论了完整的回档设计,**包括许多尚未在本仓库实现的子系统**。
> 阅读时请把**已实现**和**规划中**分开看,避免照着文档以为有的功能其实并不存在。
>
> | 子系统 | 状态 | 落地位置 |
> |---|---|---|
> | `RollbackPlayer` / `RollbackZone` / `RollbackAll` RPC | ⚠️ RPC/逻辑骨架存在；生产执行 fail-closed | `go/data_service/internal/logic/rollback_logic.go`；缺跨服务离线 epoch 栅栏时返回 `error_code=16` |
> | `transaction_log` 协议 + 双向操作类型 | ✅ enum 已加完(含 TX_ITEM_AWARD 等 4 个 replay 用 op) | `proto/common/rollback/transaction_log.proto` |
> | `transaction_log` 业务侧 hook 点 | 🟡 部分 — currency 已 hook;**bag/quest/mail/skill 未 hook** | `cpp/libs/modules/currency/system/currency_system.cpp` |
> | `BatchRecallItems` GM 工具 | ⚠️ dry-run 查询可用；真实回收未实现 | 非 dry-run 返回 `error_code=16`；候选数超过 10000 返回 `error_code=17` 且零变更 |
> | 补缴 (Deferred Clawback) | ✅ 已实现并防旁路审计 PASS | `cpp/libs/modules/currency/system/currency_system.cpp` |
> | 物品 UUID (SnowFlake) | ✅ 已实现 | `cpp/libs/engine/core/utils/id/snow_flake.h` |
> | 跨服节点崩溃后的 PostCrashReplay 工具 | ❌ **未实现** — 设计文档级,工具骨架未写 | (待) |
> | **mail 服务** | ❌ **服务级别整体未实现** — 既无 MySQL 表也无 Go 服务 | (待) |
> | **auction 服务** | ❌ **服务级别整体未实现** | (待) |
> | **chat 历史持久化** | 🟡 proto 有,DB 表不存在 | `proto/chat/chat.proto` 仅协议 |
> | **bag 持久化(跨 zone 安全)** | ❌ proto 有,Marshal/Unmarshal 未接 | 详见 `cross-zone-readiness-audit.md §3.2 件 1` |
> | quest 系统 | ❌ proto 占位,系统未实现 | 同上 |
>
> **影响阅读这份文档时的判断**:
>
> - 文档第 91 行写"Applies to ALL bidirectional ops: trade, mail attachments, guild bank, auction house, party loot" — 这是**设计意图**,实际只 currency 接了
> - 文档第 134 行"On auction house → delist and delete" — 假设 auction 服务存在;**当前不存在**
> - 文档第 100 行的 transaction_log 字段都已经在 proto 里 ✅
>
> **如果你正在排期这份文档相关的工作**,先核对上面的状态表;表里 ❌ 的项是真正的工作量。

### 当前应用级回档安全门（2026-08-03）

`data_service` 现在要求回档全程持有 `RollbackFence`。该栅栏必须原子地
阻止新登录/Scene 激活，排空目标玩家旧 Scene epoch 已发出的 Kafka/DB
存盘，并让落库端拒绝迟到的旧 epoch 写。单次 `IsOnline`/location 查询
会有 TOCTOU 窗口，不满足契约。

这个跨 login / player_locator / Scene / Kafka / db 的协议尚未落地，所以
生产 `ServiceContext.RollbackFence` 故意保持 `nil`：三个回档 RPC 明确拒绝，
而不是冒险让在线 Scene 的后续存盘覆盖回档结果。即使将来注入栅栏，
下列前置也都是 fail-closed：

- SnapshotStore / Router 缺失；
- `STARTED` 意图审计写入失败；
- pre-rollback 安全快照创建失败或返回空 ID；
- `RESULT` 结果审计失败（不再吞错，调用方会收到失败）。

详细协议边界见 [zone_data_rollback.md](zone_data_rollback.md)。

---

## Conversation Summary (完整对话记录)

### Q1: MMO/MOBA 微服务中单人回档怎么做？
- 微服务架构下玩家数据分散在多个服务（背包、任务、邮件、货币、公会等）
- **方案**：每个服务暴露 Export/Import RPC → 快照服务定期采集 → 回档时踢下线 → 按服务依次 Import → 审计日志
- 兜底：MySQL binlog PITR 到影子库提取单人数据
- 混合策略：常规用应用层快照，灾难恢复用 PITR

### Q2: 两人交易后只回档一人，物品会复制怎么办？
- 非对称回档是最经典的物品复制漏洞
- **核心结论**：单人回档不是「时光倒流」而是「异常损失补偿」
- 依赖交易流水日志（transaction_log）判断物品是正常转移还是异常丢失
- 有正常转移记录的一律不恢复 → 从根本上杜绝复制
- 也可用物品全局 UUID 做存在性检查（物品还在世界上就不恢复）
- 级联回档（连带回档交易方）风险太大，几乎无人采用

### Q3: "只有严重 Bug 才用真正的数据恢复"是什么意思？
- 日常客服工单（90%+）用「软回档」：查流水 → GM 邮件补发道具/货币 → 数据库不动
- 只有数据结构性损坏（服务器崩溃、代码Bug批量吞物品、数据库迁移错误）才用快照/PITR 真正恢复
- 原因：真正回档操作复杂、有复制风险、会覆盖正常游戏进度；补偿发放则零风险

### Q4: 玩家利用 Bug 刷东西怎么处理？
- 封号止损 → transaction_log 追踪污染链（多级扩散 A→B→C）→ 按 item_uuid 精确回收
- 无辜玩家处理：合谋者扣+封，不知情者扣物品但退还支付金币
- 极端情况（全服经济污染）→ 全服回档（代价最大，窗口越小损失越小）
- **一切追踪能力都依赖 item_uuid + transaction_log**

### Q5: 对于小额 Bug 刷取，有没有更温和的补缴机制？
- **补缴系统（Deferred Clawback）**：GM 挂一笔欠款，后续玩家每次获得该货币时自动扣入补缴池
- 优点：不封号、不突然扣款、永远不会扣成负数、玩家感知弱
- 关键实现：必须 hook 到统一的 AddCurrency 入口
- 适合纯货币类小额 Bug；不适合唯一性物品或大额/恶意利用

### Q6: 货币系统是独立 Go 服务还是放 C++ 玩家实体上？
- **结论：放 C++ 玩家实体上（CurrencyComp），Go 只管持久化和 GM 操作**
- 原因：战斗中消耗/获得货币（买药、技能代价、修理费等）需要零延迟，RPC 到 Go 服务 2-20ms 不可接受
- 架构：登录时从 DB 加载到 CurrencyComp → 游戏中纯内存读写 → 定期/下线 flush 到 Kafka → go/db → MySQL
- 补缴 hook 也在 C++ 的 AddCurrency 统一入口里
- Go 服务只处理离线/外围场景：充值到账、Web 商城、邮件附件、GM 工具 → 最终都通知 C++ 节点执行

## Problem
In microservice MMO, player data is scattered across many services (bag, quest, mail, currency, guild, friend, ranking, etc.). Rolling back a single player to a previous state requires coordinated restoration across all services.

## Recommended: Application-Level Snapshot + On-Demand Rollback

### Each service exposes Export/Import RPC
- `ExportPlayerData`: export full player state for that service (protobuf bytes)
- `ImportPlayerData`: overwrite player state with provided blob (idempotent)

### Snapshot Service
- Periodically (and on key events: login, large transactions, pre-maintenance) calls each service's ExportPlayerData
- Stores composite snapshots: `player_id | snapshot_time | service_name | data_blob`
- Retention: last N days of login snapshots + daily periodic snapshots
- Storage: dedicated table/database, isolated from live data

### Rollback Procedure
1. Kick player offline (mandatory — no rollback while online)
2. Find nearest snapshot ≤ target_timestamp
3. Call ImportPlayerData on each service (respecting dependency order)
4. Write audit log (operator, reason, target time, affected services)
5. Return result to GM/CS tool

### Key Design Points
- **Must be offline**: kick before rollback to avoid write conflicts
- **Idempotency**: ImportPlayerData must be retry-safe
- **External effects are NOT reversed**: mails sent to others, guild donations, trades — only the player's own state rolls back
- **Granularity**: support full rollback AND per-service rollback (e.g., only bag, not quests)
- **Audit trail**: every rollback logged with who/when/why/scope
- **Snapshot isolation**: separate from live DB to prevent corruption cascading
- **Service ordering**: restore in dependency order (generally no circular deps)

### Fallback: Database-Level PITR
- Use MySQL binlog point-in-time recovery to a shadow DB
- Extract target player's rows at target timestamp from shadow
- Import back to live DB
- Heavier operationally; best as disaster-recovery last resort

### Best Practice: Hybrid
- Routine rollbacks → application-level snapshots (fast, precise, automatable)
- Disaster recovery → MySQL PITR (broad coverage, high operational cost)

## Anti-Duplication: Asymmetric Rollback Problem

### The Problem
Player A trades item to Player B. If only A is rolled back, both A and B have the item → duplication.
Applies to ALL bidirectional ops: trade, mail attachments, guild bank, auction house, party loot.

### Solution: Rollback ≠ Time Travel, Rollback = Abnormal Loss Compensation

**Core principle: items/currency with normal transfer records are NOT restored.**

### Required Infrastructure
1. **Item UUID**: every item instance has a global unique ID (SnowFlake). Enables precise tracking.
2. **Transaction Log**: all bidirectional operations write to `transaction_log` via Kafka → DB:
   - tx_id, time, type (trade/mail/auction/guild_bank), from_player, to_player, item_uuid, quantity, currency_delta
3. **Global Item Index**: item_uuid → current_owner lookup (only queried for rollback items)

### Rollback Procedure (Revised)
1. Kick player offline
2. Load snapshot at target time
3. Diff snapshot vs current state
4. For each "missing" item/currency: check transaction_log
   - Has normal transfer record → DO NOT restore (recipient still has it)
   - No transfer record (bug/anomaly) → restore
5. Generate "recoverable items" report for GM review
6. GM approves → execute selective restore
7. Audit log

### Alternative Strategies (Trade-offs)
- **Cascading rollback** (roll back all trade partners): too dangerous, uncontrollable blast radius
- **Soft rollback** (mail compensation only, no DB overwrite): safest, used for routine CS requests
- **Item existence check** (skip restore if UUID still exists in world): simpler but less precise than full tx-log analysis

### Critical Rule
> Single-player rollback is NOT "time reversal" — it is "abnormal loss compensation."
> Any transfer with a valid transaction log entry is excluded from restoration.

## Bug Exploit / Item Duplication — Precision Clawback

### Scenario
Player discovers a bug, exploits it to generate items/currency, then spreads them via trade/mail/auction.

### Handling Layers
1. **Freeze**: ban account immediately to stop exploit
2. **Trace pollution chain**: from transaction_log, follow every dirty item_uuid and currency flow (multi-level: A→B→C→...)
3. **Precision clawback**:
   - Item still on someone → remove by UUID
   - Sold to NPC → deduct equivalent currency
   - On auction house → delist and delete
   - Dirty currency spent on other items → deduct those items or equivalent
4. **Innocent player handling**:
   - Knowing accomplice → clawback + possible ban
   - Unknowing buyer via normal trade → remove item BUT refund what they paid
   - Spread too wide to trace → accept loss if small; emergency maintenance + full server rollback if massive
5. **Nuclear option**: if economy is too polluted → full server rollback (MySQL PITR)

### Four Scenarios Summary
| Scenario | Action | Infrastructure |
|----------|--------|----------------|
| Normal item loss (accidental) | Soft rollback (mail compensation) | Transaction log |
| Structural data corruption | Snapshot restore (real rollback) | Periodic snapshots + Export/Import RPC |
| Bug exploit duplication | Precision clawback (reverse trace) | Transaction log + Item UUID + pollution chain |
| Server-wide economic collapse | Full server rollback | MySQL PITR / full server snapshot |

**All four + deferred clawback (below) depend on: Item UUID (SnowFlake) + Transaction Log.**

## Deferred Clawback / 补缴系统 (Soft Recovery for Minor Exploits)

### When to Use
- Minor bug exploits where the amount is small, no mass spread, player not malicious
- Pure currency/token duplication (not unique items)
- You want to recover without banning or disrupting the player's experience

### How It Works
1. GM determines player gained X currency via bug
2. Create a `player_debt` record: player owes X of currency_type
3. Every time the player receives that currency (quest reward, trade income, system grant...):
   → System intercepts at the unified `AddCurrency` entry point
   → Deducts min(incoming_amount, remaining_debt) into the debt pool
   → Player receives the remainder
4. Continues until debt is fully repaid

### Data Model
```
player_debt:
  player_id | currency_type | owed_amount | paid_amount | reason | gm_operator | created_at | completed_at
```

### Implementation Key Point
- **Must hook into the single unified currency-add function** — if currency can be added through multiple code paths that bypass the hook, the deduction is easily circumvented
- Pseudocode:
```
AddCurrency(player, type, amount):
  debt = GetPlayerDebt(player, type)
  if debt > 0:
    deduct = min(amount, debt)
    actual_gain = amount - deduct
    ReduceDebt(player, type, deduct)
  else:
    actual_gain = amount
  player.currency[type] += actual_gain
```

### Suitable vs Not Suitable
- **Suitable**: fungible currency (gold, gems, tokens), small amounts, non-malicious players
- **Not suitable**: unique items (must use UUID-based precision clawback), large amounts (debt too large to ever repay naturally), repeat/malicious exploiters (should ban)

### Advantages over Hard Clawback
| | Hard clawback (direct deduction) | Deferred clawback (补缴) |
|-|----------------------------------|--------------------------|
| Player experience | Sudden loss, likely complaints | Gradual, low-friction |
| Negative balance risk | Can go negative if already spent | Never negative, only deducts from gains |
| Ban required? | Usually no, but may need if ongoing | No, just fix the bug and attach debt |
| Implementation | Simple | Medium — requires unified currency entry point |

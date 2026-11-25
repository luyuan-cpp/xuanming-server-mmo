# 线性一致 SQL 玩家归属权威与重连状态机设计（`go/shared/ownerauthority`）

> 日期：2026-11-24  
> 对标来源：`xuanming-server/services/runtime/owner` 与 `CLAUDE.md §9.22 / §9.23`  
> 状态：核心事务引擎、DDL、重连状态决策机、`scene_manager` `ServiceContext` 强读装配，以及 `EnterScene` / `LeaveScene` 的 `shadow` 双写审计与 `enforce` fail-closed 入口闸门均已落地并通过单测。  
> Java 独立版（`xuanming-server-mmo`）状态：本阶段未变更任何客户端可见 proto、帧或配表契约，暂无需改动 `xuanming-server-mmo`；阶段 2 切换真实进场主链并引入客户端重连状态字段时须同步更新 `xuanming-server-mmo` 与 `PARITY.md`。

---

## 1. 需求分析

当前 `xuanming-server-mmo-cpp` 的玩家实时归属由 `scene_manager` 结合 Redis 键族（`player:{id}:owner_epoch`、`handoff`、`holder_lease`、`owner_epoch_confirmed` + `WAITAOF`）以及 `data_service` / `go/db` 的 SQL 写权行共同维护：
1. Redis 即使配合 `WAITAOF`，仍属于异步复制后的落盘近似，网络分区切换期间存在主从脑裂与回退窗口，不得不维护复杂的两段回执（`pending:`）、墙钟地板与自愈补偿逻辑；
2. 缺乏统一、原子的 `owner_record` + `instance_lease` + `admit_not_before` 线性化屏障事务，旧节点最晚停止可玩时刻与新节点最早准入时刻难以靠单一强一致提交点严格排序；
3. 客户端断线重连与切场恢复缺少统一的最小状态集决策（`TARGET` / `WAIT` / `ROLE_REQUIRED` + `retry_after_ms`）。

本方案将 `xuanming-server` 经多轮压测与事故收敛的**线性一致 SQL `owner` 权威模型**移植为 `go/shared/ownerauthority`，并复用本仓 `go/shared/authoritysql` 强一致连接池。

---

## 2. 设计分析

### 2.1 三张核心表（同事务域，TiDB / MySQL 同构）

1. **`owner_record`**（每玩家 1 行，串行化锚点）：
   - `player_id BIGINT UNSIGNED PRIMARY KEY`（TiDB `NONCLUSTERED + SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4`）
   - `owner_epoch BIGINT UNSIGNED NOT NULL`（单调递增代次）
   - `owner_type TINYINT NOT NULL`（`0=NONE, 1=HUB/WORLD, 2=INSTANCE/BATTLE`）
   - `phase TINYINT NOT NULL`（`0=NONE, 1=PENDING, 2=ADMITTED`）
   - `pod_name`, `instance_uid`, `instance_epoch`, `assignment_id`, `release_track`（exact 实例五元组）
   - `operation_id CHAR(36)`（权威铸造或调用方显式 UUIDv4 幂等键）
   - `admit_not_before_ms BIGINT NOT NULL`（安全准入屏障时间戳）
   - `source_revision BIGINT UNSIGNED NOT NULL DEFAULT 0`（单调来源版本围栏，防旧分配覆盖新分配）
   - `updated_at_ms BIGINT NOT NULL`
2. **`owner_instance_lease`**（每活跃物理节点 1 行）：
   - `pod_name VARCHAR(128) PRIMARY KEY`, `instance_uid VARCHAR(128)`, `instance_epoch INT UNSIGNED`, `lease_deadline_ms BIGINT`, `updated_at_ms BIGINT`
3. **`owner_transition_log`**（有界审计流水，`idx_created_at (created_at_ms)` 供保留期清理）。

### 2.2 核心原语与固定锁序（无死锁）

固定加锁顺序：**先 `owner_record FOR UPDATE`，再 `owner_instance_lease FOR UPDATE`**；`RenewInstanceLease` 仅锁 `owner_instance_lease`，无环无死锁：
- **`BeginTransition`**：
  - 校验 `player_id`、UUIDv4 `operation_id`（空则权威铸造）、完整 `OwnerTarget`；
  - 同事务 `FOR UPDATE` 锁 `owner_record`：若 `operation_id` 或 `(owner_type, exact target)` 已匹配则幂等 no-op 返回既有记录；否则校验 `expect_epoch`（不匹配返回 `ErrEpochConflict` 附最新记录）与 `source_revision` 单调性（回退或 legacy 0 回落返回 `ErrSourceRevisionStale`）；
  - 同事务读旧实例 `owner_instance_lease` 计算屏障 `admit_not_before = max(now, old_lease_deadline + skew_margin)`（跨实例迁移必设屏障，确保 `旧节点最晚可玩 < 新节点最早准入`），推进 `owner_epoch+1` 与 `PENDING` 并写审计行。
- **`Admit`**：同事务校验 exact 五元组 + `owner_epoch` + `operation_id`；若 `now < admit_not_before_ms` 返回 `ErrBarrierNotOpen` 与 `retry_after_ms`；已 `ADMITTED` 幂等返回成功；否则推进到 `ADMITTED`。
- **`RenewInstanceLease`**：按协议硬上限 `MaxLeaseSeconds=30s` 钳制防放大脑裂窗；同 `pod_name` 换新 `instance_uid` 时允许单调纪元接管，拒绝旧 `instance_epoch` 回写（`ErrLeaseRegressed`）。
- **`Release`**：精确匹配 `(owner_epoch, operation_id)` 后释放为 `NONE`；迟到/失配调用幂等 no-op 返回当前权威记录，绝不伤新归属。
- **`ResolveResume`（重连状态机）**：纯函数，依据权威查询错误、是否已选角、当前 `OwnerRecord`、`nowMs` 与 `skewMarginMs` 确定性收敛到 `EntryTarget`、`EntryRoleRequired` 或 `EntryWait`（含 `WaitReason` 与 `RetryAfterMs`），禁止任何本地猜路由。

---

## 3. 思想分析

- **单一强一致事务域取代跨存储拼接**：把 `owner_epoch`、旧租约读取、屏障计算与准入 CAS 全部装进同一 TiDB/MySQL 事务，从根源消除“Redis 已改、SQL 未改”的半提交状态。
- **阶段渐进、不破坏现网**：阶段 1 提供完整可用的 `go/shared/ownerauthority` 引擎与 `scene_manager` 可选装配（`OwnerAuthorityDSN` 留空时不启用，零破坏），后续按 `expand → shadow compare → cutover` 切替换掉 Redis `ownerepoch`。

---

## 4. 漏洞分析

| 隐患 | 防护机制 |
|---|---|
| 同名 Pod 重建后复用了 `pod_name` | 身份比较强制五元组 `pod_name + instance_uid + instance_epoch + assignment_id + release_track` |
| 调用方配置过大租约秒数放大脑裂等待窗 | `RenewInstanceLease` 强制钳制到 `MaxLeaseSeconds = 30s` |
| `owner_transition_log.detail` 超列宽在严格 `sql_mode` 下打挂合法迁移 | Go 侧按 `maxDetailLen = 512` 显式截断 |
| 审计表无界增长 | `owner_transition_log` 带 `idx_created_at (created_at_ms)` 索引，纳入 `tools/dbcheck` 与保留期清理 |

---

## 5. 步骤分析

1. 落地 `go/shared/ownerauthority/{ownerauthority.go,resume.go,ownerauthority_test.go}`；
2. 在 `go/scene_manager/internal/config` 与 `svc/servicecontext.go` 预接可选强一致 `OwnerAuthority` 句柄（复用 `shared/authoritysql`，默认空不启用，开启时校验强一致会话并在 `Stop()` 关闭）；
3. 阶段 2（后续）：在 `scene_manager` EnterScene/LeaveScene 走双写影子对账，核对零分歧后切换权威读，最后下线 Redis `ownerepoch`/`WAITAOF` 并同步 C++ scene 节点与 Java 独立版 `PARITY.md`。

---

## 6. 执行步骤与验收口径

- 运行 `go test -count=1 ./ownerauthority/...` 与 `go vet ./ownerauthority/...`（覆盖 CAS 冲突、屏障等待、准入幂等、旧 `source_revision` 拒绝、租约钳制与纪元回退、迟到 `Release` no-op 与 `ResolveResume` 全部五态）；
- 运行 `go test -count=1 ./...` 与 `go vet ./...` 验证 `go/scene_manager` 零回归；
- 运行 `python tools/scripts/check_log_text_english.py` 确认 0 中文运行日志。

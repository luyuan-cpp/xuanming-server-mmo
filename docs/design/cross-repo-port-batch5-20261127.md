# 双仓双向方案对比与移植设计 Batch 5（`xuanming-server` ↔ `xuanming-server-mmo-cpp`）

> 日期：2026-11-27  
> 范围：`D:\luyuan\xuanming-server`（Pandora Go/Kratos/UE DS）↔ `D:\luyuan\xuanming-server-mmo-cpp`（MMORPG C++/go-zero/Java）  
> 前序基线：Batch 1–3（B1–B16、A1–A11）见 [cross-repo-port-comparison-20261123.md](./cross-repo-port-comparison-20261123.md)；Batch 4（B17–B22、A12–A16）见 [cross-repo-port-batch4-20261127.md](./cross-repo-port-batch4-20261127.md)。  
> Java 独立版（`xuanming-server-mmo` / `PARITY.md`）对账状态：**零改动需求**。本批 B23–B25 与 A17–A20 均属服务端存储/发号/观测基础设施，未变更任何客户端可见 `.proto`、帧格式或策划配表契约。

---

## 1. 需求分析

在 Batch 4 完成后，继续逐包审计 `xuanming-server/pkg` 与 `xuanming-server-mmo-cpp/go/shared` 及现役服务调用点，识别出 7 项高确定性、可闭环接线的优势差异与真实隐患：

1. **`xuanming-server-mmo-cpp` → `xuanming-server`（B23–B25）**：
   - **Redis Cluster 非幂等管道半接线漏洞 + `ds_allocator/gm.go` 真实重放隐患**：`xuanming-server/pkg/redisx/noreplay.go` 已定义 `NonIdempotentPipelineHook` 但从未挂载进 `pkg/redisx/client.go` 的 `ClusterClient`；同时实查全仓 8 处 `.Pipeline()` / `.TxPipeline()` 后发现，唯独 `services/battle/ds_allocator/internal/gm/gm.go:220` 在 `TxPipeline` 中包含非幂等 `LPush`，在 Redis Cluster 网络中断时会被 go-redis 管道层整批重放导致 GM `add_item` 指令重复入队。
   - **Snowflake VM 挂起漏判漏洞**：`xuanming-server/pkg/snowflake/etcdnode/etcdnode.go` 仅靠 Go 单调 `time.NewTimer` 做自 fencing；当宿主机休眠 / VM 挂起导致 Linux `CLOCK_MONOTONIC` 停走时，服务端 etcd lease 早已过期并被其他副本领走同 `nodeID`，旧进程恢复后单调 timer 仍未到期，存在双活撞号窗口；且 `pkg/snowflake.Node` 缺少 `Fence`/`SetFenceClock`/`TryGenerate`/`SetGuardTime`。
   - **配置表热更版本漂移观测缺口**：`xuanming-server/pkg/configtable/store.go` 尚未暴露类似 `mmo-cpp/go/shared/tablebundle/metrics.go` 的单活跃序列 `bundle_info`、热更结果计数与校验耗时直方图。

2. **`xuanming-server` → `xuanming-server-mmo-cpp`（A17–A20）**：
   - **Snowflake 批量预留与范围清理下界**：`mmo-cpp/go/shared/snowflake` 仅有单条 `Generate()`，缺少一次加锁批量切段 `GenerateInto(dst []uint64) error` 与按时间截止转主键下界的 `MinIDAt(unixSec int64) uint64`。
   - **通用启动期表/列/形状 fail-fast 守卫**：`mmo-cpp/go/shared/authoritysql` 缺少通用的 `CheckTables`/`CheckColumns`/`CheckColumnSpecs`，`scene_manager` 初始化 `ownerauthority` SQL 连接池后未在启动期核对 `owner_record`/`owner_instance_lease`/`owner_transition_log` 及其关键列形状。
   - **慢 RPC 告警缺口**：`mmo-cpp/go/shared/serverbase.UnaryInterceptor` 仅观测带内故障码与未知码，缺少 `xuanming-server/pkg/middleware/logging.go` 的慢 RPC `rpc_slow`（默认 500ms / `LOG_SLOW_RPC_MS`）结构化告警。
   - **自建 Redis 7.x 握手噪音日志**：`mmo-cpp/go/shared/clusterredis` 使用 `go-redis/v9 v9.17.3` 但未默认关闭 `MaintNotifications`，连接自建 Redis 7.x 时会产生 `maintnotifications disabled due to handshake error` 噪音日志。

---

## 2. 设计分析

### 2.1 Batch 5 双向移植清单

#### 方向一：`xuanming-server-mmo-cpp`（B）→ `xuanming-server`（A）

| # | 移植方案 | 源实现（B） | 落点与接线（A） | 核心收益 / 修复的真实问题 |
|---|---|---|---|---|
| **B23** | 挂载 Cluster `NonIdempotentPipelineHook` + 修复 `ds_allocator/gm.go` 管道非幂等 `LPush` | `go/shared/clusterredis/guard.go` | `services/battle/ds_allocator/internal/gm/gm.go`（`enqueueCommandScript` 同 slot Lua 替换 `TxPipeline`）；`pkg/redisx/client.go` 挂载 `NonIdempotentPipelineHook{}`；`pkg/redisx/noreplay_test.go` 补 Cluster 管道拦截回归测试 | 消除 `gm.go` 在 Cluster 网络抖动下重复 `LPUSH` 发放道具隐患；正式激活全仓 Cluster 管道非幂等拦截，且经逐一审计确认其余 7 处管道全为幂等操作 |
| **B24** | 双时钟自栅栏 `FenceClock` + `Node` 栅栏/地板原语 + `etcdnode` 防 VM 挂起升级 | `go/shared/snowflake/{fenceclock.go,snowflake.go}` | 新增 `pkg/snowflake/{fenceclock.go,fenceclock_test.go}`；扩展 `pkg/snowflake/snowflake.go`（`Fence`/`IsFenced`/`SetFenceClock`/`TryGenerate`/`SetGuardTime`/`HighWaterEpochSec`）；升级 `pkg/snowflake/etcdnode/{etcdnode.go,keepalive_test.go}` | 单调钟挡 NTP 回拨 + `Round(0)` 墙钟挡 VM 挂起/宿主机休眠；`etcdnode` 周期复核 `FenceClock.Stale()` 并在 `signalLost()` 时立即永久 `Fence()` 节点，保持现有 `Generate() uint64` 签名 100% 兼容 |
| **B25** | 低基数配置表热更 Prometheus 指标 | `go/shared/tablebundle/metrics.go` | 新增 `pkg/configtable/{metrics.go,metrics_test.go}`；接线 `pkg/configtable/store.go`（`Store.Load`） | 暴露 `pandora_configtable_bundle_info{version,source_rev}`（切换前删旧标签，严格每进程 1 序列）、`pandora_configtable_reload_total{result}` 与 `pandora_configtable_reload_prepare_seconds` |

#### 方向二：`xuanming-server`（A）→ `xuanming-server-mmo-cpp`（B）

| # | 移植方案 | 源实现（A） | 落点与接线（B） | 核心收益 / 修复的真实问题 |
|---|---|---|---|---|
| **A17** | Snowflake 批量发号 `GenerateInto` + 清理下界 `MinIDAt` | `pkg/snowflake/snowflake.go` | `go/shared/snowflake/{snowflake.go,snowflake_test.go}` | 一次加锁连续预留批量 ID（严格保留 `fenced`/`fenceClock`/`bootGuardPending`/`maxBorrowAheadSec` 全部不变量，出错清零 `dst`）；提供 `MinIDAt` 支撑主键范围清理 |
| **A18** | 通用启动期表/列/形状 fail-fast 守卫 + `ownerauthority` 接线 | `pkg/mysqlx/schema.go` | 新增 `go/shared/authoritysql/{schema_check.go,schema_check_test.go}`；`go/shared/ownerauthority/ownerauthority.go` 新增 `VerifySchema`；接线 `go/scene_manager/internal/svc/servicecontext.go` `initOwnerAuthority` | 既有数据库升级缺表/缺列/类型或可空性漂移时在启动期 fail-fast 拒启并指明 DDL，避免带病进流量 |
| **A19** | `serverbase` 慢 RPC 结构化告警 `rpc_slow` | `pkg/middleware/logging.go` | `go/shared/serverbase/{interceptor.go,interceptor_test.go}` | 新增 `EventSlowRPC="rpc_slow"` 与 `Options.SlowThreshold`（0 读 `LOG_SLOW_RPC_MS` 默认 500ms，负值关闭），仅打低基数方法名与耗时（严守不含 `player_id`），不与故障码双打 |
| **A20** | `clusterredis` 默认禁用 `MaintNotifications` | `pkg/redisx/client.go` | `go/shared/clusterredis/{clusterredis.go,clusterredis_test.go}` | 统一在单实例与 Cluster 客户端默认设 `maintnotifications.ModeDisabled`（支持 `Config.MaintNotifications` 显式覆盖），消除自建 Redis 7.x 握手噪音日志 |

### 2.2 本轮明确排除项（核对源码与单测后放弃）

1. **`gatepick.TwoChoices` → `xuanming-server/hub_allocator`**：经查 `hub_allocator` 每次 `AssignHub` 均同步原子更新 Redis `player_count`（不存在 `mmo-cpp` Gate 10s 心跳滞后导致的新区惊群），且 `hub_test.go`（`TestAssignHub_LazySeedAndLeastLoaded`、`TestAssignHub_SpreadAcrossShards`）明确锁定最小 `ShardId` 决胜规则，强改会破坏既有契约。
2. **`xuanming-server/pkg/accountdeletion/apple_revoke.go` → `mmo-cpp`**：实查 `mmo-cpp/go/login` 已完整实现 `auth.NewAppleRevoker` 与 `sandbox_mock` 闭环测试，无需重复移植。
3. **`xuanming-server/pkg/sellercredit` / `pkg/redisx/ratelimit.go` → `mmo-cpp`**：实查 `mmo-cpp` 已在 `go/trade/internal/commerce/seller_credit.go` 与 `go/shared/ratelimit` 落地，无需重复移植。
4. **`xuanming-server/pkg/snowflake` 借位逻辑**：`xuanming-server/pkg/snowflake/snowflake.go:153` 明确说明因 `MinIDAt` 保留期清理语义禁止借位超前墙钟，尊重该架构差异不强行对齐。

---

## 3. 思想分析

1. **先审计调用点再收紧安全门禁**：B23 先遍历全仓所有 8 处 Redis Pipeline，定位并修复 `ds_allocator/gm.go` 这唯一一处非幂等管道后，再正式开启 `ClusterClient` 的 `NonIdempotentPipelineHook`，避免“先挂钩子后炸生产”。
2. **向后兼容 + Fail-Closed 并行**：B24 在不破坏 `xuanming-server` 全仓 `Generate() uint64` 调用的前提下补齐 `FenceClock`、`TryGenerate()` 与 `etcdnode` 双时钟自栅栏；A17 的 `GenerateInto` 失败即 `clear(dst)` 防止调用方误用半截数组。
3. **继续零新依赖**：全部 7 项仅复用既有依赖（含 `mmo-cpp` 已锁定的 `go-redis/v9 v9.17.3` 内置 `maintnotifications`），未改动任何 `go.mod` / `go.sum`。

---

## 4. 漏洞分析与防线设计

| 风险点 | 触发场景 | 本轮防护措施 |
|---|---|---|
| **GM 道具命令在 Redis Cluster 管道中被重放（B23）** | `TxPipeline` 含 `LPUSH`，遇网络 EOF/超时 go-redis Cluster 整批重试 | 改为单条同 slot（`{<match_id>}`）Lua `enqueueCommandScript` + Cluster 挂载 `NonIdempotentPipelineHook` |
| **VM 挂起/休眠导致 `etcdnode` 失租漏判双活撞号（B24）** | Linux `CLOCK_MONOTONIC` 在挂起期停走，`time.NewTimer` 未到点但 etcd lease 已过期 | `FenceClock` 双时钟（单调钟 + `Round(0)` 墙钟）周期复核，唤醒首个节拍立即 `node.Fence()` + 关闭 `Lost()` |
| **热更版本切换造成 Prometheus 指标基数缓慢增长（B25）** | 每次配置表热更都新建 `{version,source_rev}` gauge 序列而不删旧序列 | `setActiveBundleMetric` 在加锁下先 `DeleteLabelValues` 旧标签再 `Set(1)` 新标签，单测断言活跃序列数恒为 1 |
| **批量发号中途跨秒失租或产生数据竞争（A17）** | 一次要 N 个 ID 跨秒等待期间被 `Fence()` 或水位过期 | 每段加锁前后均复核 `fenced` 与 `checkWatermarkAge()`，出错 `clear(dst)` 并返错；跨秒测试用 `atomic.Uint64` 保证零竞态 |
| **`ownerauthority` 缺表/半旧表带病启动（A18）** | 升级部署后 PVC 未重放 `DDLTiDB`/`DDLMySQL` | `scene_manager` 的 `initOwnerAuthority` 打开池后立即调用 `ownerauthority.VerifySchema`，缺表或关键列形状不符立即关闭连接池并拒启 |
| **慢 RPC 告警泄露高基数字段或与故障码重复刷屏（A19）** | 慢请求同时带玩家 ID 或带内故障码 | 仅在 `VerdictOK`/`VerdictBizReject` 分支记 `rpc_slow`，只含 `method`/`latency_ms`/`slow_threshold_ms` |

---

## 5. 步骤分析

1. 盘点双仓 `pkg/`、`go/shared/` 与现役服务调用点，核对前序 Batch 1–4 与既有测试契约，剔除重复/冲突项。
2. 实施方向一（B23–B25）：修复 `ds_allocator/gm.go` 并挂载 `NonIdempotentPipelineHook`；落地 `pkg/snowflake/fenceclock.go` 与 `etcdnode` 双时钟升级；落地 `pkg/configtable/metrics.go` 并接入 `Store.Load`。
3. 实施方向二（A17–A20）：补齐 `shared/snowflake` 的 `GenerateInto`/`MinIDAt`；落地 `shared/authoritysql/schema_check.go` 并经 `ownerauthority.VerifySchema` 接入 `scene_manager`；补齐 `shared/serverbase` 慢 RPC 告警；在 `shared/clusterredis` 默认禁用 `MaintNotifications`。
4. 在双仓跑通全部受影响包/服务单测、编译与 `check_log_text_english.py`，修复单测编译笔误（`&struct{}{}`）与测试原子并发细节。
5. 双仓落盘 Batch 5 设计文档并复查 `git status -sb`。

---

## 6. 执行步骤与实测验证记录

### 6.1 `xuanming-server`（临时 `GOWORK` 剔除 `services/data/data_service` + `-tags=http2legacy`）
- `pkg/redisx`：`go test -tags=http2legacy -count=1 ./redisx/...` → **PASS**（6.171s，含 `TestNewUniversalClientClusterMountsNonIdempotentPipelineHook`）
- `pkg/snowflake`：`go test -tags=http2legacy -count=1 ./snowflake/...` → **PASS**（5.647s，含 `TestFenceClock_DualClockHandlesRollbackAndVMSuspend`、`TestNode_FenceGuardAndTryGenerate`）
- `pkg/configtable`：`go test -tags=http2legacy -count=1 ./configtable/...` → **PASS**（6.251s，含 `TestBundleInfoMetricKeepsSingleActiveSeries`）
- `pkg/snowflake/etcdnode`：`go test -tags=http2legacy -v -count=1 ./...` → **PASS**（6.848s，含 `TestKeepAliveLoop_WallClockJumpTriggersSelfFenceOnSuspend`）
- `services/battle/ds_allocator`：`go test -tags=http2legacy -count=1 ./internal/gm/...` → **PASS**（6.276s）+ `go build -tags=http2legacy ./...` → **EXIT 0**

### 6.2 `xuanming-server-mmo-cpp`
- `go/shared`：`go test -count=1`：
  - `shared/snowflake`：**PASS**（14.413s，含 `TestMinIDAt`、`TestGenerateInto_BatchMonotonicCrossSecondAndFence`）
  - `shared/authoritysql`：**PASS**（5.680s，含 `TestSchemaChecksFailClosedAndPass`）
  - `shared/ownerauthority`：**PASS**（6.159s）
  - `shared/serverbase`：**PASS**（2.603s，含 `TestUnaryInterceptorSlowRPCWarning`）
  - `shared/clusterredis`：**PASS**（2.730s，含 `TestMaintNotificationsModeDefaultAndOverride`）
- `go/scene_manager`：`go test -count=1 ./internal/svc/...` → **PASS**（9.786s）+ `go build ./...` → **EXIT 0**
- 英文日志检查：`python tools/scripts/check_log_text_english.py` → `log text check passed: no CJK literals in log/console calls`（**EXIT 0**）

### 6.3 未验证项
1. 本机 Windows `CGO_ENABLED=0` 未执行 `go test -race`（需在 Linux CI 补跑）。
2. 未连真实 etcd / Redis Cluster / TiDB 做真实 VM 挂起与真实 `information_schema` 联调（均通过双时钟注入、fake SQL driver 与 `miniredis` 单测覆盖）。

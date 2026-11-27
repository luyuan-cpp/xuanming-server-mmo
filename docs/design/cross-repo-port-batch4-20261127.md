# 双仓双向方案对比与移植设计 Batch 4（`xuanming-server` ↔ `xuanming-server-mmo-cpp`）

> 日期：2026-11-27  
> 范围：`D:\luyuan\xuanming-server`（Pandora Go/Kratos/UE DS）↔ `D:\luyuan\xuanming-server-mmo-cpp`（MMORPG C++/go-zero/Java）  
> 前序基线：Tier 1 / Batch 2 / Batch 3（B1–B16、A1–A11）已归档于 [cross-repo-port-comparison-20261123.md](./cross-repo-port-comparison-20261123.md)；并行会话已提前落地的未提交跨仓能力（如 `pkg/redisx/slot.go`、`pkg/playername`、`pkg/releasetrack`、`go/shared/internalrpcauth`、`go/shared/releasetrack`）本轮完整保留、零覆盖。  
> Java 独立版（`xuanming-server-mmo` / `PARITY.md`）对账状态：**零改动需求**。本批 B17–B22 与 A12–A16 均属服务端基础设施、连接安全、日志降噪与运维护栏，未触碰任何客户端可见 `.proto`、帧协议或配表契约。

---

## 1. 需求分析

在前三批（B1–B16、A1–A11）完成基础设施与内容安全对齐后，深入核对两仓剩余源码仍发现 11 项经过生产故障或压测打磨、且对方存在真实缺口的方案：

1. **`xuanming-server-mmo-cpp` 明显更优、应移植到 `xuanming-server`（B17–B22）**：
   - **事务重试与结局未知 fail-closed**：`xuanming-server` 各服务（`login`/`guild`）仍手工分散枚举 MySQL `1213`/`1205`，漏掉 TiDB `9007`（Write Conflict），且未建立 `ErrTxOutcomeUnknown` 压制盲目重试的统一判据。
   - **Redis Cluster 跨分片批量读与跨主节点分页扫描**：`xuanming-server/services/matchmaking/team/internal/data/team.go` 直接对 `UniversalClient` 调 `Scan`，在 Redis Cluster 下只会扫描单个分片、永久遗漏其余分片离线成员；且缺少按 slot 分组的跨槽 `MGetBySlot` 与带拓扑指纹的 `ScanEachMaster` / `ScanPage`。
   - **租约心跳数学化自栅栏（Self-Fencing）**：`auction` 的 `RedisMarketLocker` 使用固定 `ttl/3` + 首次瞬时错误即 fail-stop，缺少 `mmo-cpp/go/shared/leader/fence.go` 的单调节拍与 `CallBudget`/`FenceAfter` 精确超时预算推导（在保证永不越过 TTL 过期的前提下吸收单次网络毛刺）。
   - **Kafka Topic 分区契约守卫与只读 Lag 探针**：`xuanming-server` 缺乏生产级幂等 `EnsureTopics`（拒绝带 key topic 原地扩容破坏哈希分区映射、拒绝下调 `min.insync.replicas`、创建后等待全 broker 元数据可见）以及只读消费组 Lag 探针。
   - **优雅停机预算推导**：Kratos `App` 停机超时未按 `Grpc.Timeout` / `MaxConnAgeGrace` + 后台排空时长自动对齐，长 RPC 存在停机被过早强杀风险。
   - **按逻辑库/分片隔离的三态熔断器与建连门**：`xuanming-server/pkg/mysqlx` 仅有请求级 BBR 限流，缺少 `mmo-cpp/go/shared/circuitbreaker` + `authoritysql.WithConnectGate` 的单分片建连熔断与故障隔离能力。

2. **`xuanming-server` 明显更优、应移植到 `xuanming-server-mmo-cpp`（A12–A16）**：
   - **无锁常量内存依赖降级日志限流窗**：`xuanming-server/pkg/log/window.go` 用单个 `atomic.Int64` + CAS 实现窗口首条放行、窗内压制计数、恢复摘要，而 `mmo-cpp/go/scene_manager/internal/logic/owner_authority_bridge.go` 的 shadow 模式在 SQL 异常时会逐请求刷屏打爆日志。
   - **全序单调 `SourceRevision` 编码器与发号器**：`xuanming-server/pkg/placement/source_revision.go` 将 40 位 `writerTerm` + 24 位单调 `seq` 合成 64 位无符号全序版本（含溢出/零值 fail-closed 与并发 `SourceRevisionMint`），而 `mmo-cpp/go/shared/ownerauthority` 虽然已有 `SourceRevision` 字段校验，却缺少标准合成与并发铸造原语。
   - **严格 Bundle-Only MySQL/TiDB TLS 根证书加载与不安全模式拒绝**：`xuanming-server/pkg/mysqlx` 拒绝 `skip-verify`/`preferred`/`AllowFallbackToPlaintext`/TLS<1.2，且仅信任显式 CA bundle（不混入 OS 默认根证书），拒绝符号链接/超大证书文件；`mmo-cpp/go/shared/authoritysql` 尚未在 `Open` 入口统一拦截弱 TLS 配置。
   - **双检锁按需拨号 Kafka SyncProducer**：`xuanming-server/pkg/kafkax/lazy.go` 支持构造期仅做配置校验、首次发送按需拨号并在断联恢复后自动自愈；`mmo-cpp/go/shared/kafkautil` 与 `go/login/internal/kafka` 尚无按需拨号封装。
   - **白名单剥离异步上下文 `tracing.Detach`**：`xuanming-server/pkg/log.Detach` 明确禁止用 `context.WithoutCancel(ctx)` 承接异步后台任务（避免请求级 gRPC metadata / server transport 泄漏到后台协程），而 `mmo-cpp/go/guild/internal/logic/push.go` 仍在两处异步推送中直接使用 `context.WithoutCancel(ctx)`。

---

## 2. 设计分析

### 2.1 Batch 4 双向移植全景对比矩阵

#### 方向一：`xuanming-server-mmo-cpp`（B）→ `xuanming-server`（A）

| # | 移植方案 | 源实现（B） | 落点与生产接线（A） | 核心收益 / 修复的真实缺陷 |
|---|---|---|---|---|
| **B17** | 统一 SQL 事务可重试判定（含 TiDB `9007` 与 `ErrTxOutcomeUnknown`） | `go/shared/authoritysql/tx_outcome.go` | 新增 `pkg/mysqlx/{tx_retry.go,tx_retry_test.go}`；接线 `services/account/login/internal/data/session_generation.go` 与 `services/social/guild/internal/data/guild_repo.go`（同步覆盖 `group_repo.go`） | 补齐 TiDB `9007` Write Conflict 重试；`ErrTxOutcomeUnknown` 压制盲目重试防止双写；消除多服务重复 1213/1205 常量 |
| **B18** | Redis Cluster 跨槽 `MGetBySlot`、拓扑指纹 `ScanEachMaster` 与分页 `ScanPage` | `go/shared/clusterredis/{batch.go,scanpage.go}` | 新增 `pkg/redisx/{batch.go,scanpage.go,cluster_scan_test.go}`（复用既有 `pkg/redisx/slot.go`）；接线 `services/matchmaking/team/internal/{data/team.go,biz/offline_leave.go}`（补 `TestScanPlayerIndexPage`） | **修复真实线上隐患**：`team.go` 原直接调用 `r.rdb.Scan` 在 Cluster 下仅扫 1 个分片；升级后跨主节点增量分页扫描并兼容单机零额外往返 |
| **B19** | 租约心跳数学化自栅栏（`CallBudget` / `FenceAfter`） | `go/shared/leader/fence.go` | 新增 `pkg/redislock/{fence.go,fence_test.go}`；接线 `services/economy/auction/internal/data/market_locker.go` | 以 `TryLock` 发起前时间点起算租约寿命，单调节拍续租 + 数学推导连续容错上限；30s 生产 TTL 可吸收 1 次瞬时毛刺且严格在过期前自停，确定失主（`err==nil && !extended`）仍立即 fail-stop |
| **B20** | Kafka Topic 契约守卫 `EnsureTopics` 与只读 `GroupLagProber` | `go/shared/kafkautil/{topic_init.go,group_lag.go}` | 新增 `pkg/kafkax/{topic_init.go,topic_init_test.go,group_lag.go,group_lag_test.go}`；更新 `pkg/kafkax/topics.go`（`DefaultPushTopicSpecs`）；接线 `services/runtime/push/cmd/push/main.go` 单二进制 CLI 运维旗标 | 支持 `-ensure-topics` / `-check-kafka-lag` 一次性运维探针；默认禁止带 key topic 原地扩容（防破坏分区映射）、禁止调低 `min.insync.replicas`、校验全 broker 元数据收敛 |
| **B21** | 优雅停机预算守卫 `StopTimeout` / `AppStopTimeoutOption` | `go/shared/shutdownbudget` | 新增 `pkg/grpcserver/{shutdown_budget.go,shutdown_budget_test.go}`；接线 `login` / `owner` / `auction` / `inventory` / `ds_allocator` / `hub_allocator` / `push` 共 7 个核心服务 `cmd/*/main.go` | 按 `max(Grpc.Timeout, MaxConnAgeGrace) + backgroundDrain + 余量` 推导 Kratos `StopTimeout`（不低于 10s），防止停机阶段误杀在途长 RPC |
| **B22** | 三态按目标隔离熔断器与 `mysqlx.ConnectGate` | `go/shared/circuitbreaker` + `authoritysql.WithConnectGate` | 新增 `pkg/circuitbreaker/{breaker.go,breaker_test.go}`、`pkg/mysqlx/{connect_gate.go,connect_gate_test.go}`；扩展 `pkg/mysqlx/mysqlx.go`（`NewClientWithOptions`）；接线 `services/economy/auction/cmd/auction/main.go` | 单分片 MySQL/TiDB 故障触发按分片独立熔断（Closed → Open → Half-Open 单探针），避免慢拨号拖垮其他健康分片或放大连接风暴 |

#### 方向二：`xuanming-server`（A）→ `xuanming-server-mmo-cpp`（B）

| # | 移植方案 | 源实现（A） | 落点与生产接线（B） | 核心收益 / 修复的真实缺陷 |
|---|---|---|---|---|
| **A12** | 无锁常量内存降级日志窗口 `logwindow.Window` | `pkg/log/window.go` | 新增 `go/shared/logwindow/{window.go,window_test.go}`；接线 `go/scene_manager/internal/logic/owner_authority_bridge.go` | Shadow 模式 SQL 连续报错按 5s 窗口限流（首错即报、窗内计数压制、恢复吐 `shadow_error_streak` 汇总），防止数据库抖动引发日志风暴（全英文日志，符合 `AGENTS.md §2`） |
| **A13** | 单调全序 `SourceRevision` 编码器与并发 `SourceRevisionMint` | `pkg/placement/source_revision.go` | 新增 `go/shared/ownerauthority/{source_revision.go,source_revision_test.go}` | 为 `ownerauthority.OwnerRecord.SourceRevision` 补齐 40-bit `writerTerm` + 24-bit `seq` 全序编码、解码与并发原子铸造原语，零值与溢出全部 fail-closed |
| **A14** | 严格 Bundle-Only MySQL/TiDB TLS CA 加载与弱 TLS 拦截 | `pkg/mysqlx/mysqlx.go` | 新增 `go/shared/authoritysql/{tls.go,tls_test.go}`；接线 `go/shared/authoritysql/pool.go`（`WithStrictTLS` + 全 `Open*` 路径自动 `applyStrictTLS`） | 自动拒绝 `skip-verify` / `preferred` / `AllowFallbackToPlaintext` / TLS<1.2；严格模式只信显式 CA bundle、拒符号链接/大文件，且对 `WithStrictTLS` + 动态 `WithPlacement` 组合显式 fail-closed |
| **A15** | 双检锁按需拨号 `LazySyncProducer` | `pkg/kafkax/lazy.go` | 新增 `go/shared/kafkautil/{lazy_producer.go,lazy_producer_test.go}`；接线 `go/login/internal/kafka/{command_producer.go,command_producer_test.go}`（新增 `NewLazyCommandProducer`，保留 `NewCommandProducer` 默认 fail-fast） | 提供无启动阻塞、首次发送按需拨号、故障恢复自动自愈且支持优雅 `Close` 的同步生产者封装 |
| **A16** | 白名单异步上下文剥离 `tracing.Detach` | `pkg/log/log.go` `Detach` | 新增 `go/shared/tracing/{detach.go,detach_test.go}`；接线 `go/guild/internal/logic/push.go`（替换 `Notify` 与 `NotifyWarAlert` 两处 `context.WithoutCancel(ctx)`） | 仅保留 OTel `SpanContext` 与 `Baggage`，彻底剥离请求取消、截止时间、gRPC incoming/outgoing metadata 与 server transport，阻断请求上下文逃逸到后台协程 |

### 2.2 刻意排除项与不移植理由（严守 YAGNI、D-3 禁令与接线完整性）

1. **D-3 反向移植禁令（`xuanming-port-decisions-20260910.md`）**：严禁将 `xuanming-server` 早期从 `mmo-cpp` 抽出的旧副本（`pkg/cache`、`pkg/redislock`、`pkg/kafkax/consumer.go`、`pkg/configtable/manifest.go`）反向覆盖 `mmo-cpp`。
2. **`xuanming-server/pkg/configtable/shop.go`（NPC 买卖倒挂校验）**：实查 `mmo-cpp/data/schema/item_table.proto` 尚无 `sell_price` 字段，当前不存在商店买入价低于回收价套利路径，不提前造空校验。
3. **`mmo-cpp/go/shared/dualcontrol`（高危运维双人审批）**：`xuanming-server` 暂无 GM 管理后台审批 RPC，按 `xuanming-server/CLAUDE.md §14` 禁止落无引用的孤儿包，登记至后续路线图。
4. **`xuanming-server/pkg/rewardclaim`**：`mmo-cpp` 的奖励位图与防重领实现在 C++ 节点，Go 侧无需重复维护平行实现。
5. **gRPC Reflection 门禁**：经核对，`mmo-cpp` 各 Go 服务已普遍按 `c.Mode == service.DevMode || c.Mode == service.TestMode` 保护 `reflection.Register`，无需再移植。
6. **`xuanming-server/services/battle/ds_allocator/internal/data/battle_active_reconciler.go`**：单测依赖 `miniredis`（不支持 `INFO server` 的 `run_id:`），本轮未盲切到 `ScanEachMaster`，避免破坏既有单测稳定性。

### 2.3 剩余 Tier 3 大架构级路线图（待负责人拍板）

- **B → A**：63 位打散永久号段发号器（`idsegment` + 双独立 etcd）、持久收件箱全服务（`player_inbox`）、`Tip.xlsx` 单轴错误码自动生成、`dbplacement` 在线搬库、`dualcontrol` GM 双人审批（待 GM 服务立项）。
- **A → B**：线性一致独立 `owner` 服务全面替代 Redis `ownerepoch`、RS256/Ed25519 JWKS 票据信任域隔离、`cellroute` 三层确定性路由、免 Docker 策划本地一键栈。

---

## 3. 思想分析

1. **只搬“已被线上事故或压测打磨过”的硬核原语，绝不造无人调用的样板包**：每一项 B17–B22 / A12–A16 都绑定真实生产调用点与回归测试，不做“只拷贝一个 `pkg/` 目录完事”的表面移植。
2. **Fail-Closed 优先于宽松放行**：
   - `ErrTxOutcomeUnknown` 优先级高于任何可重试 SQL 错误码，宁可向上暴露未知结局走幂等对账，也绝不盲目重试导致双扣/双发；
   - `EnsureTopics` 默认禁止带 key topic 原地加分区、禁止调低 `min.insync.replicas`；
   - `authoritysql` 在 `WithStrictTLS` 与动态 `WithPlacement` 混用时显式报错拒绝，避免 SNI `ServerName` 与动态目标地址不一致带来静默降级或证书校验误判；
   - `tracing.Detach` 采用白名单复制而非黑名单剔除，从根源阻断未来新增的请求级 context Value 泄漏。
3. **零新外部依赖**：全部 11 项均完全复用两仓既有依赖（`sarama`、`go-sql-driver/mysql`、`go-redis/v9`、`kratos/v2`、`otel/trace`、`otel/baggage`），未改动任何 `go.mod` / `go.sum`。

---

## 4. 漏洞分析与防线设计

| 风险点 | 触发场景 | 本轮防护措施 |
|---|---|---|
| **未知提交结局被误判为可重试（B17）** | TiDB `COMMIT` 阶段断网或返回不确定状态时若外层包装了 `1213`/`9007`，重试会导致重复发货/双记账 | `IsRetryableTxError` 先查 `IsTxOutcomeUnknown(err)`，命中直接返回 `false`；单测覆盖包裹组合场景 |
| **Redis Cluster 漏扫与拓扑变更游标漂移（B18）** | `team` 离线兜底在 Cluster 下只扫单分片，或中途主从切换/扩容导致游标失效 | `ScanPage` 将 8 字节拓扑指纹编入游标，拓扑变化自动从首主节点重启并置 `Restarted=true`；兼容单机常量指纹零额外开销 |
| **续租超时反而推迟 fail-stop 触发脑裂（B19）** | 单次 `Extend` 阻塞超过预期，若只按“连续失败次数”计时可能越过锁 TTL 才停机 | `market_locker.go` 记录 `TryLock` 发起前时间 `issuedAt` 并按单调节拍调度，`FenceAfter` 以最坏 RTT 预算确保必然在 TTL 到期前触发 `failStop` |
| **Topic 自动扩容打散有序消息（B20）** | 运维误把带 `player_id` key 的 push topic 原地调大分区数，导致同玩家新旧消息乱序 | `EnsureTopics` 默认 `AllowPartitionGrowth=false`，遇分区扩大直接拒绝并要求走新一代 topic 命名 |
| **停机超时早于 gRPC 宽限（B21）** | `MaxConnAgeGrace=30s` 但 Kratos `App` 仍用默认 10s 停机超时，滚更强杀在途交易 RPC | `StopTimeout` 取 `max(Grpc.Timeout, MaxConnAgeGrace) + backgroundDrain + 3s` 余量并钳底 10s |
| **全仓 TLS 拦截误伤本地/测试 DSN（A14）** | `authoritysql` 默认对全部 `Open*` 启用 `applyStrictTLS`，若误拒明文开发 DSN 会炸掉全部本地单测 | `applyStrictTLS` 仅在 DSN 显式配置不安全 TLS 模式（`skip-verify`/`preferred`/`AllowFallbackToPlaintext`）或开启 `WithStrictTLS` 时严格拦截，本地明文开发 DSN 零破坏（全量 `shared/authoritysql` 单测已验证） |
| **日志中混入中文触犯 `mmo-cpp` 门禁（A12–A16）** | `mmo-cpp` 强制全部 Go 服务与脚本日志全英文 | 全量执行 `python tools/scripts/check_log_text_english.py` 通过（0 违规） |

---

## 5. 步骤分析

1. **基线盘点与去重**：检查双仓 `git status` 与既有 Batch 1–3 文档，锁定并行会话未提交文件（严格只读不动），剔除已落地与不适配项。
2. **方向一落地（B17–B22）**：在 `xuanming-server/pkg` 实现 `tx_retry`、`redisx/batch+scanpage`、`redislock/fence`、`kafkax/topic_init+group_lag`、`grpcserver/shutdown_budget`、`circuitbreaker` + `mysqlx/connect_gate`，并接入 `login`/`guild`/`team`/`auction`/`push`/`owner`/`inventory`/`ds_allocator`/`hub_allocator`。
3. **方向二落地（A12–A16）**：在 `xuanming-server-mmo-cpp/go/shared` 实现 `logwindow`、`ownerauthority/source_revision`、`authoritysql/tls`、`kafkautil/lazy_producer`、`tracing/detach`，并接入 `scene_manager`/`authoritysql/pool`/`login`/`guild`。
4. **双仓实测与修复**：跑通全部受影响包与服务单测/编译、`check_log_text_english.py`，补齐发现的符号与测试缺口（`authoritysql.Reader()`、`config.Grpc`、`TestScanPlayerIndexPage`、`TestNewLazyCommandProducer_NoDialOnConstructAndCleanClose`）。
5. **文档落盘与终检**：双仓同步落盘本文档，复查 `git status -sb` 确认无多余二进制产物、未覆盖任何既有未提交改动、未擅自 `git commit`/`git push`。

---

## 6. 执行步骤与实测验证记录

### 6.1 `xuanming-server` 验证结果

> **环境说明**：仓库根 `go.work` 包含的无关模块 `services/data/data_service` 锁定了已在上游被删除的标签 `github.com/luyuancpp/proto2mysql@v0.0.28`（上游现存标签为 `v0.0.1`–`v0.2.0`），导致默认 `go.work` 无法构建整个工作区；同时本机工具链为 `go1.27.2`，`google.golang.org/grpc@v1.79.3` 搭配 `golang.org/x/net@v0.54.0` 在 Go 1.27 下需要 `-tags=http2legacy`（`x/net@v0.54.0/http2/server.go:5`）。本次在不修改仓库 `go.work`/`go.mod` 的前提下，使用临时 `GOWORK`（剔除 `services/data/data_service`，沿用仓库原 `go.work.sum`）并加 `-tags=http2legacy` 对全部改动模块与 9 个受影响服务完成实跑：

- `pkg`（`go test -count=1`）：
  - `github.com/luyuancpp/pandora/pkg/mysqlx`：PASS（6.530s，含 `TestIsRetryableTxError`、`TestConnectGateChainingAndCircuitBreakerIsolation`）
  - `github.com/luyuancpp/pandora/pkg/redisx`：PASS（5.456s，含 `MGetBySlot` / `ScanEachMaster` / `ScanPage`）
  - `github.com/luyuancpp/pandora/pkg/redislock`：PASS（1.143s，含 `TestFenceAfter`、`TestCallBudget`）
  - `github.com/luyuancpp/pandora/pkg/kafkax`：PASS（17.214s，含 `TestEnsureTopics*`、`TestGroupLag*`）
  - `github.com/luyuancpp/pandora/pkg/grpcserver`：PASS（1.972s，含 `TestStopTimeout`）
  - `github.com/luyuancpp/pandora/pkg/circuitbreaker`：PASS（7.527s）
- 业务服务（`go test -tags=http2legacy -count=1` + `go build -tags=http2legacy ./...`）：
  - `services/account/login`：`./internal/data/...` PASS（7.403s）+ `go build ./...` EXIT 0
  - `services/social/guild`：`./internal/data/...` PASS（2.839s）+ `go build ./...` EXIT 0
  - `services/matchmaking/team`：`./internal/data/...` PASS（4.111s，含新增 `TestScanPlayerIndexPage`）+ `./internal/biz/...` PASS（10.980s）+ `go build ./...` EXIT 0
  - `services/economy/auction`：`./internal/data/...` PASS（6.304s）+ `go build ./...` EXIT 0
  - `services/runtime/push` / `services/runtime/owner` / `services/economy/inventory` / `services/battle/ds_allocator` / `services/battle/hub_allocator`：`go build ./...` 均 EXIT 0

### 6.2 `xuanming-server-mmo-cpp` 验证结果

- `go/shared`（`go test -v -count=1`）：
  - `shared/logwindow`：PASS（2.811s）
  - `shared/ownerauthority`：PASS（2.523s，含 `TestComposeAndSplitSourceRevision`、`TestSourceRevisionMintMonotonicAndExhaustion`）
  - `shared/authoritysql`：PASS（3.137s，含新增 `tls_test.go` 全量通过；真实 TiDB 集成用例按预期 skip）
  - `shared/kafkautil`：PASS（含 `lazy_producer_test.go`）
  - `shared/tracing` + `shared/tracing/kafkagotrace` + `shared/tracing/saramatrace`：PASS（5.027s / 6.723s / 3.092s，含 `detach_test.go`）
- 受影响业务服务：
  - `go/scene_manager`：`go test -v -count=1 -run 'OwnerAuthority|Bridge|Shadow' ./internal/logic/...` PASS（3.055s，`TestOwnerAuthorityBridge_ShadowAndEnforce`）+ `go build ./...` EXIT 0（注：该包其余 5 个既有基线用例 `TestEnterScene_ZeroZoneReconnectReturnsToPreviousScene` 等因并行未提交 `proto/` 变更失败，与本轮 `owner_authority_bridge.go` 无关）
  - `go/login`：`go test -v -count=1 ./internal/kafka/...` PASS（2.292s，含新增 `TestNewLazyCommandProducer_NoDialOnConstructAndCleanClose`）+ `go build ./...` EXIT 0
  - `go/guild`：`go test ./internal/logic/...` PASS（11.299s；注：未改动的 `go/guild/guild.go:899` 存在既有基线 `undefined: matchpb` 编译错误）
- 英文日志检查：`python tools/scripts/check_log_text_english.py` → `log text check passed: no CJK literals in log/console calls`（EXIT 0）。

### 6.3 未验证项（客观标注）

1. 本机 Windows 环境 `CGO_ENABLED=0`，未运行 `go test -race`（需在 Linux / CI 环境补跑）。
2. 未连接真实多节点 Redis Cluster / 真实 TiDB / 真实 Kafka Broker 做端到端故障注入（全部逻辑通过单元/fake/miniredis 测试覆盖）。
3. `xuanming-server/services/data/data_service` 因上游 `proto2mysql@v0.0.28` 缺失未纳入本轮构建；`mmo-cpp/go/guild/guild.go:899` 与 `scene_manager` 5 个非 bridge 既有基线失败保持原状未改动。

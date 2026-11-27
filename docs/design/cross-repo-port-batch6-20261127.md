# 双仓双向方案对比与移植设计 Batch 6（`xuanming-server` ↔ `xuanming-server-mmo-cpp`）

> 日期：2026-11-27  
> 范围：`D:\luyuan\xuanming-server`（Pandora Go/Kratos/UE DS）↔ `D:\luyuan\xuanming-server-mmo-cpp`（MMORPG C++/go-zero/Java）  
> 前序基线：Batch 1–3（[cross-repo-port-comparison-20261123.md](./cross-repo-port-comparison-20261123.md)）、Batch 4（[cross-repo-port-batch4-20261127.md](./cross-repo-port-batch4-20261127.md)）、Batch 5（[cross-repo-port-batch5-20261127.md](./cross-repo-port-batch5-20261127.md)）。  
> Java 独立版（`xuanming-server-mmo` / `PARITY.md`）对账状态：**零改动需求**。本批 B26–B28 与 A21–A23 均属服务端停机收尾、积压探测、建连熔断、异步上下文隔离、启动 schema 核对与本地 `-race` 工程脚本，未改动任何客户端可见 `.proto`、帧协议或策划配表契约。

---

## 1. 需求分析

在完成 Batch 1–5 的核心原语移植后，针对双仓“已落地原语的全面覆面扫尾 + 剩余工具链/运维探测差距”做了全仓扫描，锁定 6 项可立即闭环的收口项：

1. **`xuanming-server-mmo-cpp` → `xuanming-server`（B26–B28）**：
   - **停机预算全服覆盖缺口**：Batch 4 的 `AppStopTimeoutOption` 仅接入 7 个核心服务，其余 13 个可构建服务仍沿用 Kratos 默认停机超时，存在滚更强杀在途长 RPC 的不一致性。
   - **持续消费组积压与提交停滞（`CommitStall`）探测缺口**：B20 仅移植了单次探针 `GroupLagProber.Probe`（CLI 旗标），缺少 `mmo-cpp/go/shared/kafkautil/group_lag_probe.go` 的持续后台探测循环、`CommitStall` 分区级停滞检测（捕获安静分区毒丸卡死）及低基数 Prometheus 指标导出。
   - **账号库建连熔断缺口**：`services/account/login` 仍直调 `mysqlx.NewClient(mysqlCfg)`，尚未接入 B22 的 `mysqlx` 建连三态熔断门。

2. **`xuanming-server` → `xuanming-server-mmo-cpp`（A21–A23）**：
   - **Windows 本地执行 `go test -race` 工具缺口**：`xuanming-server/tools/scripts/go_test_race.ps1` 通过挂载宿主机 `GOMODCACHE` 到 Linux `golang` 容器，在无 gcc 的 Windows 开发机上即可直接跑 `-race`；`mmo-cpp` 缺少等价脚本。
   - **后台 goroutine `context.WithoutCancel` 扫尾缺口**：`go/chat/internal/authority/world_window.go`（共享快照后台重读）与 `go/login/internal/logic/clientplayerlogin/entergamelogic.go`（EnterGame 异步后置链）仍在后台 goroutine 中直接使用 `context.WithoutCancel(ctx)`，会把触发请求的 `ctxkeys.SessionDetails` / gRPC incoming metadata 带入后台协程；同时 `shared/tracing` 根包因 `kafka.go` 拖入 `sarama`，导致非 `sarama` 服务（如 `chat`）无法直接导入 `shared/tracing`。
   - **核心落库服务启动表/列校验扩面**：`go/player_inbox` 启动时已校验 `role_fence` 表，但尚未在启动期 fail-fast 核对自身三张核心表（`player_inbox_stream` / `player_inbox_item` / `player_inbox_audit`）及关键列。

---

## 2. 设计分析

### 2.1 Batch 6 双向移植清单

#### 方向一：`xuanming-server-mmo-cpp`（B）→ `xuanming-server`（A）

| # | 移植方案 | 源实现（B） | 落点与接线（A） | 核心收益 |
|---|---|---|---|---|
| **B26** | 全服 100% 补齐 `AppStopTimeoutOption` | `go/shared/shutdownbudget` | 全部剩余 13 个可构建服务 `cmd/*/main.go`（`player`/`battle_result`/`trade`/`matchmaker`/`team`/`leaderboard`/`player_locator`/`chat`/`dialogue`/`friend`/`guild`/`mail`/`mission`） | 达成 20/20 可构建服务优雅停机预算全覆盖（仅 `data_service` 因上游 `proto2mysql@v0.0.28` 暂不可构建未改） |
| **B27** | 持续 Kafka 积压 + `CommitStall` 后台探针与低基数指标 | `go/shared/kafkautil/group_lag_probe.go` | 新增 `pkg/kafkax/{group_lag_probe.go,group_lag_probe_test.go}`；扩展 `pkg/kafkax/group_lag.go`（`GroupLag.CommitStall`）；接线 `services/runtime/push/cmd/push/main.go`（30s 周期探测 push 消费组） | 实时暴露 `pandora_kafka_group_{lag_total,lag_max_partition,retention_gap,commit_stall_seconds,probe_errors_total}`（仅 `group`/`topic` 两维低基数）；单轮 panic/错误自动关连重建并联动 `safego` 活性心跳 |
| **B28** | `login` 账号库接入 `mysqlx` 建连三态熔断 | `go/shared/circuitbreaker` + `authoritysql.WithConnectGate` | `pkg/mysqlx/connect_gate.go` 新增 `NewClientWithCircuitBreaker`；接线 `services/account/login/cmd/login/main.go`（5 次连败熔断 10s） | 账号库短时不可达时快速 fail-fast 保护登录节点协程与连接槽位，半开单探针平滑自愈 |

#### 方向二：`xuanming-server`（A）→ `xuanming-server-mmo-cpp`（B）

| # | 移植方案 | 源实现（A） | 落点与接线（B） | 核心收益 |
|---|---|---|---|---|
| **A21** | Windows 容器化 `go test -race` 脚本 | `tools/scripts/go_test_race.ps1` | 新增 `tools/scripts/go_test_race.ps1`（适配 `go/<module>` 与 `-Module all`，镜像对齐 `golang:1.27.2`，全英文输出） | 在 Windows 开发机挂载宿主 `GOMODCACHE` 秒跑真 Linux CGO `-race` 检测，消除“Windows 不支持 `-race`”的常态盲区 |
| **A22** | 零 `sarama` 依赖子包 `ctxdetach.Detach` + `chat`/`login` 后台协程上下文扫尾 | `pkg/log/log.go` `Detach` | 新增 `go/shared/tracing/ctxdetach/detach.go`（`shared/tracing/detach.go` 委托给它）；接线 `go/chat/internal/authority/world_window.go`（`ctxdetach.Detach`）与 `go/login/internal/logic/clientplayerlogin/entergamelogic.go`（`logx.ContextWithFields(tracing.Detach(ctx), logx.Field("player_id", playerID))`） | 既彻底剥离 `ctxkeys.SessionDetails` 与 gRPC metadata 防止泄漏进后台协程，又让非 `sarama` 服务（`chat`/`mail`/`friend` 等）零新增依赖复用同一份白名单剥离实现 |
| **A23** | `player_inbox` 启动期表/列 fail-fast 守卫 | `pkg/mysqlx/schema.go` | `go/player_inbox/internal/inbox/store.go` 新增 `VerifySchema`（核对 `player_inbox_stream`/`player_inbox_item`/`player_inbox_audit` 及核心列）；接线 `go/player_inbox/internal/svc/servicecontext.go`（`productionDeps.checkInboxSchema`）并补 `servicecontext_test.go` 失败回收单测 | 与既有 `checkRoleFence` 形成双启动守卫，缺表/缺列拒启并回收已开连接 |

---

## 3. 思想分析

1. **避免“半覆盖”和“依赖传染”**：
   - B26 把停机超时覆盖率从 7/20 拉齐到 20/20（全部可构建服务），消灭跨服务行为漂移；
   - A22 遇到 `go/chat` 不依赖 `sarama` 时，绝不往 `chat/go.mod` 塞多余依赖，而是按 `shared/tracing/kafkagotrace` 同一分层思想抽出零外部 broker 依赖的 `shared/tracing/ctxdetach` 子包。
2. **只改真正的后台逃逸点，不动有意的同步值透传**：A22 逐一审查 `mmo-cpp` 全部 `context.WithoutCancel` 后，明确跳过依赖请求内 `authoritysql` 观察位 / `withSyncDelivery` 标记的同步收尾路径，只改造真正启动独立后台协程的 `chat.world_window` 与 `login.EnterGame`。
3. **收敛判定**：至此，Batch 1–6（B1–B28、A1–A23）已把两仓全部**非破坏性、零新依赖、不跨端改契约**的基础设施/安全/观测/工具链差距系统性收敛完毕；剩余差异均属需要 ADR 与跨端协同拍板的 Tier 3 大架构项。

---

## 4. 漏洞分析与防线设计

| 风险点 | 触发场景 | 本轮防护措施 |
|---|---|---|
| **安静分区毒丸卡死但传统总 Lag 告警不响（B27）** | 某分区停在 1 条坏消息且无新流量，总 Lag=1 远低于阈值 | `CommitStall` 在进程内按分区跟踪 `Backlog > 0` 且 `Committed` 未变的最长时长，对外只聚合为单标量 `pandora_kafka_group_commit_stall_seconds` |
| **探针观测回调 panic 或 broker 重置导致连接泄漏/失明（B27）** | broker 断连或 observer panic 后 sarama 连接停在半坏态 | 单轮用 `safego.Recovered` 兜底，遇到任何探测 error 或 panic 立刻 `closeConn` 并在下一轮重建连接；正常轮推进 `safego.MarkLoopLiveness` |
| **剥离 `EnterGame` 后台上下文后丢失 `player_id` 日志字段（A22）** | 直接 `tracing.Detach(ctx)` 丢掉请求级所有 Value | 显式白名单补回 `logx.ContextWithFields(tracing.Detach(ctx), logx.Field("player_id", playerID))`，保留 trace + baggage + `player_id`，剔除 `SessionDetails` 与 gRPC metadata |
| **`player_inbox` 单测 stub 未传 `checkInboxSchema` 导致空指针（A23）** | 现有单测自定义 `externalDeps` 仅填 `checkRoleFence` | `newServiceContextWithAdmission` 判 `if err == nil && deps.checkInboxSchema != nil`，`productionDeps()` 必挂 `inbox.VerifySchema`，并在 `servicecontext_test.go` 补 `failAt="inbox_schema"` 资源回收回归 |

---

## 5. 步骤分析

1. 扫描双仓剩余未覆面调用点与脚本目录，确认 B26–B28 与 A21–A23 范围及精确锚点。
2. 落地 B27（`kafkax` `CommitStall` + `RunGroupLagProbe` + `ObserveGroupLagMetrics` + `push` 接线）、B28（`mysqlx.NewClientWithCircuitBreaker` + `login` 接线）与 B26（13 个服务 `AppStopTimeoutOption`）。
3. 落地 A21（`tools/scripts/go_test_race.ps1`）、A22（`shared/tracing/ctxdetach` + `chat`/`login` 接线）与 A23（`player_inbox` `VerifySchema` + 失败回收单测）。
4. 双仓全量执行受影响包单测、15 个 `xuanming-server` 服务编译、3 个 `mmo-cpp` 服务编译、PowerShell AST 语法检查与 `check_log_text_english.py`，修复 `group_lag.go` 重复方法与 `chat` `sarama` 传递依赖问题。
5. 落盘 Batch 6 设计文档并复核 `git status -sb`。

---

## 6. 执行步骤与实测验证记录

### 6.1 `xuanming-server`（临时 `GOWORK` 剔除 `services/data/data_service` + `-tags=http2legacy`）
- `pkg/kafkax`：`go test -tags=http2legacy -count=1 ./kafkax/...` → **PASS**（11.052s，含 `TestCommitStallTracksFrozenBacklogAcrossPartitions`、`TestRunGroupLagProbeRebuildsOnErrorAndPanic`）
- `pkg/mysqlx`：`go test -tags=http2legacy -count=1 ./mysqlx/...` → **PASS**（1.389s）
- 全部 15 个改动服务 `go build -tags=http2legacy ./...` 全部 **EXIT 0**：`login`、`player`、`battle_result`、`trade`、`matchmaker`、`team`、`leaderboard`、`player_locator`、`push`、`chat`、`dialogue`、`friend`、`guild`、`mail`、`mission`。

### 6.2 `xuanming-server-mmo-cpp`
- `go/shared/tracing/...`：`go test -count=1 ./tracing/...` → **PASS**（`shared/tracing` 3.825s、`kafkagotrace` 9.639s、`saramatrace` 6.677s）
- `go/chat`：`go test -count=1 ./internal/authority/...` → **PASS**（5.468s）+ `go build ./...` → **EXIT 0**
- `go/login`：`go test -count=1 -run 'EnterGame' ./internal/logic/clientplayerlogin/...` → **PASS**（6.207s）+ `go build ./...` → **EXIT 0**
- `go/player_inbox`：`go test -count=1 ./internal/inbox/... ./internal/svc/...` → **PASS**（5.047s / 12.572s，含新增 `inbox_schema` 失败资源关闭回归）+ `go build ./...` → **EXIT 0**
- `tools/scripts/go_test_race.ps1`：PowerShell AST `Parser::ParseFile` → **0 syntax errors**
- `python tools/scripts/check_log_text_english.py` → `log text check passed: no CJK literals in log/console calls`（**EXIT 0**）

### 6.3 未验证项
1. `tools/scripts/go_test_race.ps1` 仅做了 PowerShell 语法解析检查，未实际调用本机 Docker Daemon 拉起 `golang:1.27.2` 容器执行 `-race`。
2. `xuanming-server/services/data/data_service` 因上游缺失 `proto2mysql@v0.0.28` 未补 `AppStopTimeoutOption`（避免改动无法编译验证的文件）。

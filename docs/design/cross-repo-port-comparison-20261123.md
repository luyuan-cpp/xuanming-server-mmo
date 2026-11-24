# 双仓双向方案对比与移植设计（`xuanming-server` ↔ `xuanming-server-mmo-cpp`）

> 日期：2026-11-23（Batch 2 & Batch 3 非战斗全量对齐更新：2026-11-24）  
> 范围：`D:\luyuan\xuanming-server`（Pandora Go/Kratos/UE DS）↔ `D:\luyuan\xuanming-server-mmo-cpp`（MMO C++/Go-zero/Java）  
> 状态：Tier 1、Batch 2 与 Batch 3（除战斗域外全量非战斗对齐）全部非破坏性、可闭环基础设施/内容安全/观测/认证能力已完成双向代码接线与单测验证；跨服务重架构项保留在 Tier 3 待拍板。  
> Java 独立版（`xuanming-server-mmo`）状态：Tier 1 与 Batch 2 未改动任何客户端可见 proto / 帧 / 配表契约；Batch 3 的 `go/content_guard` 增强（西里尔/希腊同形字折叠、叠字绕过、Zalgo 拒绝）保持既有 `testdata/vectors.json` 20 条共享向量 100% 兼容，若后续将新反绕过向量纳入共享向量表须同步 `xuanming-server-mmo` 与 `PARITY.md`。

---

## 1. 需求分析

两个服务端仓库由同一作者并行演进，沉淀了各自在不同故障与压测中打磨出的生产级机制：
- **`xuanming-server`（A 仓）** 在数据库广度/深度容量守卫（`dbguard`/`dbcheck`）、TiDB 排序规则真实行为探针、gRPC 服务端 `MaxConnectionAge` 滚更连接轮换、线性一致独立 `owner` 归属权威、RS256 JWKS 信任域隔离、蜂窝路由 `cellroute`、P0 事故独立建档关闭门禁等方向更成熟；
- **`xuanming-server-mmo-cpp`（B 仓）** 在 K8s 启动期时钟偏移门禁（`clockcheck`）、gRPC 客户端 `TCP_USER_TIMEOUT` Keepalive（`grpckeepalive`）、etcd Watch 三道防卡死防线（`etcdwatch`）、周期后台循环活性监控（`looplive`）、权威 SQL 物理连接级强读与主库守护（`authoritysql`）、低基数聚合连接池指标（`sqlpoolstats`）、无符号截止点防溢出护栏、HRW 无状态后台分片（`workshard`）+ 存活集合理性闸门（`liveview`）、63 位打散永久号段发号器（`idsegment`）、可靠玩家收件箱（`player_inbox`）、W3C `traceparent` 跨 Kafka 传播、`Tip.xlsx` 单轴导表错误码、实例与布局分层规范等方向更完备。

目标是在**不破坏两仓各自架构边界、不推翻未拍板 ADR、不触碰并行会话未提交改动、不留未接线半成品**的前提下，把各自更优且可闭环接线的方案双向移植落地，并把跨服务大架构级候选形成可执行路线图。

---

## 2. 设计分析

### 2.1 分层取舍策略（Tier 1 立即闭环落地 vs Tier 2 待拍板分批实施）

| 分层 | 准入标准 | 处理方式 |
|---|---|---|
| **Tier 1（本批已移植并实测）** | 经源码核对确为对方显著更优/修补真实生产缺口；不推翻既有 ADR；可立即闭环接入现役生产链路；不改客户端可见 proto/表；总改动远低于 30 文件门禁；严守 D-3 反向移植禁令 | 本批直接落地代码、接线与单测，并同步通用工程规则与 P0 事故模板 |
| **Tier 2（本批落盘待拍板）** | 涉及推翻既有决策（需 `decision-revisit`）、新增微服务/独立库、超过 30 文件改造、或联动 UE/Unity 客户端及 Java 独立版 | 形成完整对比与分批验收方案落盘，由负责人逐项拍板后排期实施 |

### 2.2 Tier 1 已落地双向移植清单

#### 方向一：`xuanming-server-mmo-cpp`（B）→ `xuanming-server`（A）

| # | 移植方案 | 源实现（B） | 落点与接线（A） | 解决的具体问题 |
|---|---|---|---|---|
| B1 | **K8s 启动期时钟偏移门禁** | `go/shared/clockcheck` | `pkg/clockcheck` + 接入 `pkg/grpcserver.MustNewServer`（`MustPassOnce`） | 全仓 20 个 Go 服务在 K8s 启动时自动校对本机与 API server 时钟，偏差 >30s fail-fast（`PANDORA_CLOCK_CHECK_MODE=warn` 应急放行，本地开发自动跳过） |
| B2 | **gRPC 服务间客户端 Keepalive** | `go/shared/grpckeepalive` | `pkg/grpcclient/grpcclient.go` + 单测 | 统一加 `Time=5m, Timeout=20s, PermitWithoutStream=false`，激活 `TCP_USER_TIMEOUT=20s`，避免 K8s ClusterIP 后端节点掉电后半开连接挂死 ~15 分钟 |
| B3 | **周期后台循环活性监控** | `go/shared/looplive` | `pkg/safego/liveness.go` + 直接融入 `safego.Loop` / `safego.Run` | 暴露 `pandora_loop_last_round_unix{name}`；启动标记、仅无 panic 轮推进、仅 `ctx.Done()` 有意退出删序列，全仓现有 `safego` 后台循环零样板获得卡死/连续 panic 告警能力 |
| B4 | **etcd 客户端 KeepAlive + Watch RequireLeader + 周期对账（并修真 bug）** | `go/shared/etcdwatch` | `pkg/killswitch/etcdkv`、`pkg/cellroute/etcdtable`、`pkg/leader/etcdleader`、`pkg/snowflake/etcdnode`、`pkg/dsauthfence` | 5 个 etcd 模块全补 `DialKeepAliveTime=10s / Timeout=5s`；watch 全加 `WithRequireLeader` + 5m 周期全量对账；**修复 `cellroute/etcdtable` watch 通道关闭即 `return` 导致热更永久死亡的 bug**；`etcdkv` 退避改 ctx 感知 |
| B5 | **Kafka 链路 `trace_id` 跨消息头透传** | `go/shared/tracing/saramatrace` 思路 | `pkg/kafkax/{trace.go,producer.go,consumer.go}` | 生产侧自动从 `ctx` 注入安全 `x-pandora-trace-id` header（不覆盖显式头），消费侧清洗校验后还原到 `ctx`，打通异步 Kafka 链路日志关联 |
| B6 | **权威 SQL 连接加固 + 低基数连接池指标 + 清理截止点护栏** | `go/shared/authoritysql`、`go/shared/sqlpoolstats`、`go/friend/sweep_repo.go` | `pkg/mysqlx/{mysqlx.go,backend_check.go,poolstats.go}`、`pkg/dbguard/sweep.go` | `mysqlx.NewClient` 补默认 `ConnMaxIdleTime=3m` 防云 LB 静默断连；补 `IsTiDBServer` / `AssertMySQLPrimary` / `AssertTiDBStrongReadSession`；自动导出 `pandora_sql_pool_*` 低基数聚合指标（整数纳秒求和防浮点 counter reset）；`dbguard` 补 `SweepCutoffMs` 防止负截止点在无符号比较下清空整表 |
| B7 | **工程规范补齐** | `docs/rules/engineering-principles.md §11.6`、`ai-workflow.md §10.4` | `docs/rules/engineering-and-storage.md` §5–§6 | 补入「实例与布局分层」与「设计方案强制落盘 `.md`」条款 |

#### 方向二：`xuanming-server`（A）→ `xuanming-server-mmo-cpp`（B）

| # | 移植方案 | 源实现（A） | 落点与接线（B） | 解决的具体问题 |
|---|---|---|---|---|
| A1 | **TiDB 最低版本与排序规则真实行为探针** | `pkg/mysqlx/backend_check.go` | `go/shared/authoritysql/collation.go` + `collation_test.go` | 补齐 `AssertTiDBVersionAtLeast`（兼容 `tidb_version()` 与 `VERSION()`）与 `AssertColumnCollationSemantics`（实时探针 `'A' COLLATE = 'a'` 与 `'a ' COLLATE = 'a'`），拦截 TiDB 老版本或旧 collation 框架静默回退 |
| A2 | **gRPC 服务端 `MaxConnectionAge` 连接轮换** | `pkg/grpcserver/grpcserver.go` | `go/shared/grpckeepalive/grpckeepalive.go` + 接入 `go/shared/domainhost/host.go` | 补 `ServerOption(maxAge, grace)`（默认 15m/30s 优雅 GOAWAY），`mail`/`rank` 等 `domainhost` 服务在 K8s 滚动更新与扩容时自动均衡 HTTP/2 长连接 |
| A3 | **统一数据库容量与大字段守卫 `dbguard`** | `pkg/dbguard/{dbguard,payload,sweep}.go` | `go/shared/dbguard/` + `CheckPayload` 接入 `go/player_inbox/internal/inbox/registry.go` | 补齐 `information_schema.TABLES` 行数/字节/`AVG_ROW_LENGTH` 估算监控、写入侧 80% 预警/100% fail-closed 三档 `CheckPayload` 分布直方图、`SweepTable` 单 WHERE `report_only` 封装与 `SweepCutoffMs` 护栏（全英文日志符合 `AGENTS.md §2`） |
| A4 | **崩溃与 P0 事故独立建档体系 + 工程红线补齐** | `docs/incidents/{index,template}.md`、`CLAUDE.md §9.24/§16.9/§16.10`、`protocol-ordering-rules.md` | `docs/incidents/{index,template}.md`、`docs/rules/engineering-principles.md` | 建立独立 P0 事故模板、索引与关闭审核清单；补齐“禁止用定时器掩盖时序问题”“推送不承担正确性”“数据库广度/深度双守卫”条款 |

### 2.2.1 Batch 2 新增落地双向移植清单（2026-11-24，单仓严格控制在 <30 文件）

#### B → A 新增落地（`xuanming-server`）
- **B8 存活集合理性闸门（`pkg/liveview`）**：接入 `hub_allocator`（`HubConf.LiveViewHold` + `reconcileShardTopology`），防御控制面空列表或 ≥4 分片骤降过半导致整区批量误标 draining。
- **B9 K8s Secret 环境变量单一来源（`pkg/secretenv`）**：接入 `pkg/config`（`DSNEnv`/`PasswordEnv`/`SecretEnv`）、`pkg/mysqlx`、`pkg/redisx`，杜绝双源冲突与密钥泄漏。
- **B10 物理连接级强读会话固定（`pkg/mysqlx/pin.go`）**：每条物理连接建连固定 TiDB 7 项强读变量 + 500ms 空闲复用探活 + MySQL 主库降从隔离，并在 `owner`/`login` 的 `RequireTiDB=true` 路径自动开启。
- **B11 K8s 真集群 NetworkPolicy 实测探针（`tools/scripts/k8s_netpol_probe.ps1`）**：配套契约测试并登记至 `ci_backend.ps1`。

#### A → B 新增落地（`xuanming-server-mmo-cpp`）
- **A5 `authoritysql.AssertCurrentSession` 零额外往返强启 TiDB `≥ v7.4` 门禁**：直接校验已查出的 `SELECT tidb_version()`，全仓权威库服务自动获益（补回归单测）。
- **A6 补齐全部剩余 13 个直连 `zrpc` Go 服务 `MaxConnectionAge=15m` 轮换**：覆盖 `chat` / `client_rpc_router` / `content_guard` / `data_service` / `db` / `friend` / `guild` / `login` / `match` / `player_inbox` / `player_locator` / `scene_manager` / `trade`，达成 Go 服务 16/16 全覆盖。
- **A7 `player_inbox` 维护主循环接入 `dbguard.Guard.Check`**：仅在维护领导者任期内每小时只读巡检 `player_inbox_stream` / `player_inbox_item` / `player_inbox_audit` 容量与行宽预算。
- **A8 宿主秒级交叉编译镜像构建（`deploy/k8s/Dockerfile.go-svc.prebuilt` + `go_svc_image.ps1 -BuildMode host`）**：复用同一 alpine digest 与 `10001` 非 root 运行层；正式 `-Version` 发布强制走 `incontainer` 保可复现，本地/快照可用 `host` 并校验 `go1.27.2`（已补契约单测）。
- **A9 只读全库容量/索引/大字段/压测快照门禁 CLI（`tools/dbcheck`）**：支持表登记校验、清理索引前导列校验、outbox 精确计数、`-snapshot/-compare`、`-size-check/-top-rows` 与只读 `-pending`（fake driver 单测强制断言零写操作）。

### 2.2.2 Batch 3 非战斗全量双向对齐清单（2026-11-24，严格排除战斗）

#### B → A 新增落地（`xuanming-server`）
- **B12 繁简折叠 + 变体联系方式/外链引流检测 + `chat` 真实接线**：将 `go/content_guard/internal/{textnorm/t2s.txt,contact/detector.go}` 移植至 `xuanming-server/pkg/namecheck`（繁简只用于敏感词/引流匹配，不污染唯一键 `Fold`），并替换 `services/social/chat` 的朴素 `strings.ReplaceAll` 为 `pkg/namecheck` 自动机 + `LexiconDir` fail-closed 加载 + `RejectContactAd` 开关。
- **B13 带内服务端故障指标修复（`pkg/middleware/metrics.go`）**：对齐 `go/shared/serverbase`，`server hop` 的带内 `ERR_INTERNAL`/`ERR_UNAVAILABLE`/`ERR_UNKNOWN`/`ERR_TIMEOUT` 归入 `server_err`/`timeout` 桶而非误记为 `ok`。
- **B14 按需按方法 RPC 流量画像（`pkg/grpcstats`）**：移植 `go/shared/grpcstats` 为 Kratos 中间件并接入 `pkg/grpcserver.MustNewServer`（关闭时仅一次 `atomic.Bool` Load）。
- **B15 口令 Argon2id PHC + KDF 并发信号量 + 防枚举伪校验（`pkg/passwd` + `login`）**：对齐 `go/login` `PasswordProvider`，支持 bcrypt/Argon2id 双校验与参数上下界校验、分算法 KDF 并发限流（满载返 `ErrUnavailable` 不误扣失败配额），并在未知账号分支接入 `DummyVerify`。
- **B16 构建版本 VCS BuildInfo 回落（`pkg/version`）**：对齐 `go/shared/buildinfo`，未注入 `-ldflags -X` 时自动读取 `vcs.revision`(12 位)/`vcs.time`/`-dirty` 并归一空值。

#### A → B 新增落地（`xuanming-server-mmo-cpp`）
- **A10 `go/content_guard` 三项反绕过加固**：移植 `xuanming-server/pkg/namecheck` 的**西里尔/希腊视觉同形字折叠**（`textnorm.go`，刻意保留数字不破坏 `contact` 引流检测）、**连续叠字绕过折叠扫描**（`wordlist/matcher.go`，破 `管理理员`/`加微微微信`/`笨笨蛋`/`aavv`，词库不折叠保双字母词语义）与 **Zalgo 连续组合记号上限（`>2` 拒收）**（`engine/engine.go`）；20 条共享向量全过。
- **A11 `go/shared/safego` 后台循环存活心跳指标**：合入 `xuanming-server/pkg/safego/liveness.go` 能力（`safego_loop_last_round_unix{point}` + `TouchLoop`），全仓 `safego.Loop` 自动获得后台静默卡死可观测性。

### 2.3 Tier 3 大架构级待拍板候选清单

#### B → A（需 `decision-revisit` 或联动 UE 客户端）

1. **63 位打散永久号段发号器（`idsegment` + 双独立 etcd + 全局号源登记处 mTLS）**：推翻 `CLAUDE.md §9.11`，须先写 `decision-revisit`。
2. **可靠玩家持久收件箱全服务（`go/player_inbox`）**：需新服务端口/库与 UE 客户端补拉协议。
3. **`Tip.xlsx` 单轴错误码自动生成 + `dbplacement` 在线搬库**：涉及全服 proto/表与客户端联动。
4. **`gameday`（05:00 游戏日/周）与 `workshard`（无状态 HRW 分片）**：待 `xuanming-server` 出现真实 05:00 重置或高争用多副本后台任务时再按需接线，不提前落空包。

#### A → B（需跨 C++/Go/Java/Unity 协同拍板）

1. **线性一致独立 `owner` 归属权威服务（TiDB 单事务 `owner_epoch` + `ds_instance_lease` + `admit_not_before` + `PENDING/ADMITTED`）**：替代 B 仓 Redis `ownerepoch`/`handofffence`。
2. **非对称 RS256/Ed25519 JWKS 票据与编译期信任域类型隔离 + 有界 JTI 防重放**：替代 Gate / Battle 对称 HMAC 共享密钥。
3. **Region → Cell → Shard 三层确定性算点路由（`cellroute` + `AtomicTable`）**：服务 D-15 百万级 CCU 演进。
4. **免 Docker 策划一键栈（`local_infra.ps1`）**：独立工具体系，按本地开发需要另行立项。

---

## 3. 思想分析

1. **尊重差异，拒绝一刀切照搬**：A 仓是 Kratos + Envoy + UE DS，B 仓是 go-zero + C++ muduo Gate/Scene/Battle + Java Gateway；同时 B 仓 `xuanming-port-decisions-20260910.md` D-3 明确禁止把 A 仓早期从 B 仓抽出的旧副本（`kafkax`/`redislock`/`cache`）反向覆盖回 B 仓。本次只移植各自**经过真实事故与压测验证、且目标仓明确缺失**的能力。
2. **接线闭环优于堆砌空包**：遵守 A 仓 `CLAUDE.md §14` 与通用简洁原则，凡新增能力均直接挂入现役公共入口（如 `clockcheck` 挂 `grpcserver.MustNewServer`、`looplive` 融入 `safego.Loop/Run`、keepalive 融入 `grpcclient.mustDial` / `domainhost.Serve`、`poolstats` 融入 `mysqlx.NewClient`、`dbguard.CheckPayload` 接入 `player_inbox`），避免只落“孤儿包”。
3. **先防静默故障，再做大架构迁移**：Tier 1 聚焦“静默失败但后果致命”的底层缝隙（watch 静默停摆、半开 TCP 15 分钟卡死、TiDB collation 静默降级、无符号截止点清空全表、后台循环静默停摆），以最小 diff 换取最高生产安全性。

---

## 4. 漏洞分析

本次逐文件源码审计发现的具体隐患与修复状态：

| 隐患编号 | 所在仓库 | 位置与根因 | 风险后果 | 本批处置 |
|---|---|---|---|---|
| V1 | `xuanming-server` | `pkg/cellroute/etcdtable/etcdtable.go` `watchLoop` 在 `!ok` 时直接 `return` 且无 `WithRequireLeader`/keepalive | etcd 任一次断连/重启后，该 Pod 的蜂窝路由表热更新永久死亡直到重启 | **已修复并补回归单测** |
| V2 | `xuanming-server` | `pkg/killswitch/etcdkv`、`pkg/dsauthfence`、`pkg/leader/etcdleader`、`pkg/snowflake/etcdnode` 缺 `DialKeepAliveTime`；`etcdkv`/`dsauthfence` 缺 `WithRequireLeader` | 分区 etcd 成员或半开连接导致 killswitch 关停规则与 DS 授权 epoch 栅栏静默失效 | **已修复** |
| V3 | `xuanming-server` | `pkg/grpcclient/grpcclient.go` 未配置客户端 keepalive（未激活 `TCP_USER_TIMEOUT`） | K8s ClusterIP 后端节点掉电无 FIN 时，长连接卡满 ~15 分钟内核重传超时 | **已修复并补契约单测** |
| V4 | `xuanming-server` | `pkg/mysqlx/mysqlx.go` 空闲连接默认不回收；`pkg/kafkax` 不透传 `trace_id`；`pkg/safego` 无循环活性时间戳 | 云 LB 静默回收引发首次查询未知结局；Kafka 异步链日志断链；后台循环持续 panic 难定位副本 | **已修复并补单测** |
| V5 | `xuanming-server-mmo-cpp` | 全仓 Go gRPC 服务端均未设置 `MaxConnectionAge` | K8s 滚动更新/扩容时老连接粘滞在旧 Pod，新副本难分到流量 | **已补 `grpckeepalive.ServerOption` 并接入 `domainhost`**（其余独立 zrpc 服务可按同一方式一键接入） |
| V6 | `xuanming-server-mmo-cpp` | `go/shared/authoritysql` 缺 TiDB 最低版本与 collation 实时行为探针；全仓缺统一的 `AVG_ROW_LENGTH` / 写侧 80% 预警 `dbguard` | TiDB 旧集群或关闭新 collation 框架时唯一键语义静默漂移；blob 字段膨胀无前置预警 | **已落地 `authoritysql/collation.go` 与 `go/shared/dbguard` 并接入 `player_inbox`** |

---

## 5. 步骤分析

1. **只读基线核查**：读取双仓根规则与分章文档，核对 `git status` 隔离并行会话未提交改动，扫描 `pkg/` 与 `go/shared/` 全部实现。
2. **B → A 移植落地**：按最小侵入原则把 `clockcheck`、gRPC client keepalive、`safego` 活性监控、Kafka trace 透传、5 处 etcd keepalive/require-leader/resync（含 `etcdtable` 断连修复）、`mysqlx` 空闲回收/主库与 TiDB 强读校验/连接池指标、`dbguard.SweepCutoffMs` 逐项落地并补单测。
3. **A → B 移植落地**：把 TiDB 版本与 collation 行为探针写入 `authoritysql`、把 `MaxConnectionAge` 写入 `grpckeepalive` 并接入 `domainhost`、把 `dbguard` 写入 `go/shared/dbguard` 并接入 `player_inbox`、落地 `docs/incidents/` 体系并同步双方规则文档。
4. **验证与合规复核**：在两仓运行受影响 Go 模块单测、在 `mmo-cpp` 运行日志英文检查脚本 `check_log_text_english.py`，并核对 `git diff` 零夹带他人未提交文件。
5. **Tier 2 后续分批（需拍板）**：按 `decision-revisit`（A 仓）与 `docs/design/` 评审（B 仓），每次挑 1 项独立闭环推进（建议优先：① B→A `player_inbox` 与 `workshard`+`liveview`；② A→B `dbcheck` CLI 与 `owner` 线性一致权威）。

---

## 6. 执行步骤与验收口径

- **A 仓（`xuanming-server`）验证命令**：
  ```powershell
  Set-Location D:\luyuan\xuanming-server\pkg
  go test ./clockcheck/... ./grpcclient/... ./grpcserver/... ./safego/... ./kafkax/... ./mysqlx/... ./dbguard/...
  Set-Location D:\luyuan\xuanming-server\pkg\cellroute\etcdtable; go test ./...
  Set-Location D:\luyuan\xuanming-server\pkg\killswitch\etcdkv; go test ./...
  Set-Location D:\luyuan\xuanming-server\pkg\leader\etcdleader; go test ./...
  Set-Location D:\luyuan\xuanming-server\pkg\snowflake\etcdnode; go test ./...
  Set-Location D:\luyuan\xuanming-server\pkg\dsauthfence; go test ./...
  ```
- **B 仓（`xuanming-server-mmo-cpp`）验证命令**：
  ```powershell
  Set-Location D:\luyuan\xuanming-server-mmo-cpp\go\shared
  go test ./authoritysql/... ./grpckeepalive/... ./domainhost/... ./dbguard/...
  Set-Location D:\luyuan\xuanming-server-mmo-cpp\go\player_inbox
  go test ./internal/inbox/...
  Set-Location D:\luyuan\xuanming-server-mmo-cpp
  python tools/scripts/check_log_text_english.py
  ```
- **通过标准**：所有新增与回归单测 0 失败；`check_log_text_english.py` 对本次新增 Go 文件 0 违例；不执行 `git commit` / `git push`。

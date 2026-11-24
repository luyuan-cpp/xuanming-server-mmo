# 双仓双向方案对比与移植设计（`xuanming-server` ↔ `xuanming-server-mmo-cpp`）

> 日期：2026-11-23  
> 范围：`D:\luyuan\xuanming-server`（Pandora Go/Kratos/UE DS）↔ `D:\luyuan\xuanming-server-mmo-cpp`（MMO C++/Go-zero/Java）  
> 状态：Tier 1 基础设施/防护补丁已完成双仓代码移植与单测验证；Tier 2 大架构方案已落盘待负责人拍板后分批实施。  
> Java 独立版（`xuanming-server-mmo`）状态：本批未改动任何客户端可见 proto / 帧 / 配表契约，Java 版无需契约同步；Tier 2 涉及跨服契约项须同步更新 `PARITY.md`。

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

### 2.3 Tier 2 大架构级待拍板候选清单

#### B → A（`mmo-cpp` 优于 `xuanming-server`，需 `decision-revisit` 后实施）

1. **63 位打散永久号段发号器（`idsegment` + 双独立 etcd + 全局号源登记处 mTLS）**：替代 `xuanming-server` 依赖墙钟 Epoch 与单 etcd 的 `pkg/snowflake/etcdnode`，根除时钟回拨与重启号段隔离窗口问题。
2. **可靠玩家持久收件箱服务落地（`go/player_inbox` + `inboxcontract` + `inboxshard`）**：`xuanming-server` 目前仅有调研文档 `reliable-player-inbox-20261014.md`，好友/交易/系统通知仍存在“DB 提交后发 Kafka 丢窗”，可按 B 仓现役契约落盘。
3. **HRW 无状态后台分片（`workshard`）+ 存活集合理性闸门（`liveview`）**：替代多处单 Leader 后台循环瓶颈，并在 `hub_allocator` / `ds_allocator` 防御 etcd 瞬时空视图/过半失联误判。
4. **物理连接级 `authoritysql` 会话固定 + `dbplacement` 逻辑库在线搬迁**：升级 `xuanming-server` 从启动期一次性校验到每条物理连接固定强读变量、500ms 空闲活性探测与不停服逻辑库迁移。
5. **K8s Secret 环境变量单一来源注入（`secretenv`）+ `gameday` 全服统一 05:00 游戏日/周 + `Tip.xlsx` 单轴错误码自动生成 + `k8s_netpol_probe.ps1` 真实网络策略探针**。

#### A → B（`xuanming-server` 优于 `mmo-cpp`，需跨服务/双端评估后实施）

1. **线性一致独立 `owner` 归属权威服务（TiDB 单事务 `owner_epoch` + `ds_instance_lease` 实例级聚合续租 + `admit_not_before` 屏障 + `PENDING/ADMITTED`）**：替代 B 仓当前依赖 Redis 非线性一致的 `ownerepoch` / `handofffence`。
2. **全库无界增长与清理索引发布前置机械门禁 CLI（`dbcheck`）**：扫描全部 `mmorpg_*` 逻辑库，未登记表、缺前缀清理索引、outbox 堆积、大字段体检及压测前后行数 `-snapshot/-compare` 直接 FAIL（只读永不 DELETE）。
3. **非对称 RS256/Ed25519 JWKS 票据与编译期信任域类型隔离（`pkg/auth/domains.go` + `dsticket.go` + 有界 JTI 防重放）**：替代 B 仓 Gate / Battle 的对称 HMAC 共享密钥模式，支持三段式无停服密钥轮换。
4. **Region → Cell → Cell 内分片三层确定性算点路由（`cellroute` + `AtomicTable`）**：服务 B 仓 D-15 的 DAU 1 亿 / CCU 2000 万扩展目标。
5. **宿主 Go 交叉编译 + `Dockerfile.prebuilt` scratch 镜像秒级构建（`-BuildMode host`）与免 Docker 策划一键栈（`local_infra.ps1`）**。

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

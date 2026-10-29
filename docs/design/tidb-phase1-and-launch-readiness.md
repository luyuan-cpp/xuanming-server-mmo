# TiDB Phase 1 设计与实施清单（含开服准备）

> **文档状态**：v0.2 提案。2026-10-12 起草，同日吸收了两份对抗评审（一份从运维看，一份从正确性看），处理记录在文末。等负责人按 §9.3 拍板。
> **依据**：只读核对。读了代码、配置、文档和 git，也查了 docs.pingcap.com 的官方文档（TiDB v8.5 / TiDB Operator v1.6，2026-10-12 抓取，页面清单在文末）。**本文不含代码。没有编译、测试、压测，也没有改部署**。按 AGENTS.md §10.1，所有验证交给 Codex。文中的容量数字都由「公式 + 参数出处」推出，不是运行结果。
> **范围**：TiDB Phase 1，即把数据层从 K8s 上的单副本 `mysql:8.0` 换成 TiDB 集群。包括目标形态、部署脚本改造、迁移与回退、兼容阻塞、写热点、备份恢复和验收。另附开服首日的准备清单（§7）和全球同服的地理形态建议（§8）。
> **拍板背景**：负责人 2026-10-12 定了「两条都做，D1 先行」。D1 指 [player-route-mapping-durability-proposal.md](./player-route-mapping-durability-proposal.md)，正在落码。这等于对 [handoff-backlog-2026-09-05.md](../handoff/handoff-backlog-2026-09-05.md) 的 D-02（:87）和 P1-D2（:789-826）选了 **B，即继续 Phase 1**。决策落档有两处：决策文档 §6 首段，以及 PROGRESS.md 一条（P1-D2 验收标准 :810）。
> **关联文档**：[global-data-layer-tidb-decision.md](./global-data-layer-tidb-decision.md)（本文细化它的 Phase 1，它 §D3 的 10-12 更正已链接到本文）、[player-storage-placement.md](./player-storage-placement.md)、[no-single-node-horizontal-scaling-20261001.md](./no-single-node-horizontal-scaling-20261001.md)、[zone_data_rollback.md](./zone_data_rollback.md)、[mysql-backup-pitr-runbook.md](../ops/mysql-backup-pitr-runbook.md)、[merge-zone-runbook.md](../ops/merge-zone-runbook.md)、[player-route-mapping-durability-proposal.md](./player-route-mapping-durability-proposal.md)。
> **行号基准**：HEAD `73b852f32`。
> - 下列文件在工作区有未提交改动，按**工作区**行号引用：`tools/scripts/k8s_zone_rollback.ps1`（例如 Step 3 在工作区 :903-915，在 HEAD 上约 :786-798）、`docs/design/zone_data_rollback.md`、`global-data-layer-tidb-decision.md`、`no-single-node-horizontal-scaling-20261001.md`、`player-storage-placement.md`、`docs/ops/merge-zone-runbook.md`、`docs/PROGRESS.md`、`go/data_service/internal/routing/router.go`。
> - `player-route-mapping-durability-proposal.md` 还没入库。
> - 其余文件工作区与 HEAD 相同。
> - Java 仓库按本机 `D:\luyuan\xuanming-server-mmo` 引用（remote `luyuan-java/xuanming-server-mmo`）。AGENTS §12 已于 2026-10-12 把本机路径更正为它(旧路径 `D:\luyuan\wuxingqitan\xuanming-server-mmo-java` 不存在)。

---

## 0. 状态与范围

| 项 | 说明 |
|---|---|
| 本文是什么 | Phase 1 的设计与实施清单，外加开服首日准备清单（§7）。只给方案、出处和验收口径 |
| 本文不是什么 | 不是代码或部署变更。没有任何一项「已验证」 |
| 与 D1 的先后 | **D1 先行**。D1 第 1 批把 `player_route` 等 4 张表建在 data_service 的全局库里，DDL 带 TiDB 注释（提案 §3.1 :53-64）。Phase 1 迁库时，这 4 张表跟 `mmorpg_global` 一起迁。两边有文件重叠：本文 P1-1 的快照去重要改 `go/data_service/internal/store/schema.go`（`bootstrapTables` 与 `nullableUniqueKeys`）；P1-2 要改 `tools/scripts/k8s_deploy.ps1`；D1 第 1 批也改这两个文件（提案 :373）。所以本文这两批**排在 D1 第 1 批合入之后**。D1 要求的「TiDB 悲观 RC 用例」（提案 :381）纳入 §9.1 #12。（2026-10-12 G1a）路由表现为 5 张（多 `route_reconcile`），`player_route` 改聚簇片前缀，预切由 `route_tool tidb-prepare` 做（§5.2 第 7 条） |
| Phase 2 | **不在本文**。全局玩家库、`PinOnCreate`、relocate 仍按 player-storage-placement.md §11 / §13 推进，前置条件仍是 D8 与 D1 |
| 授权 | D1 拿到的「超 30 个文件」授权（提案 §11.1 :404）**不覆盖本文**。本文估计要手改 45–60 个文件（§9.2），还要安装 helm / TiDB Operator / CRD / tiup。按 AGENTS §10.2，这些都要单独授权（§9.3 #14、#15） |
| 证据标注 | 「【文】」= 官方文档；「（推断）」= 读代码推出；「【未核实】」= 需要 Codex 或负责人确认 |

---

## 1. 结论（先读这节）

### 1.1 一句话

**Phase 1 只解决一个单点：数据层是一台 1 CPU / 1Gi 的单副本 MySQL。开服首日的其余瓶颈不归它管。**

- TiDB 能水平扩的：**打散后的行数据、读、容量、连接入口**。
- 扩不了的有四类：**单调索引的尾部 Region、单行热点、大事务上限、跨地域延迟**。
- 库外还有更早撞上的单点：
  - 共享 Redis 单主；
  - 每个 zone 固定 10 分区的存盘管线；
  - 默认全关的削峰；
  - C++ 快照发送端约 1MB 的 Kafka 消息上限（§4 #5，L-07）。

### 1.2 Phase 1 做什么、不做什么

| 做 | 不做 |
|---|---|
| K8s 上用 TiDB Operator，在**独立 namespace** 部署 TiDB 集群，替换 `mysql.yaml`（§2.1、§2.2） | Phase 2 的全局玩家库、`PinOnCreate`、搬库 |
| **全部库同批迁到同一个 TiDB 集群**：`zone_{N}_db`、`mmorpg_global`、`mmorpg_friend`、`mmorpg_trade`、`mmorpg_guild`、`mmorpg`（gateway）。理由是 §2.7 的两条硬约束 | 改 Kafka topic 或分区（决策 D5 明令禁止） |
| 处理兼容阻塞（§4），落实 §D4 配置（§2.5） | 改发号方式（打散或段边界预切待拍板，§5.3） |
| 部署脚本的 `-Database` 开关：必填、有预检、migrate Job 不再写死 MySQL（§2.8） | 多地域部署（§8，需要新设计） |
| 迁移与回退：Dumpling / Lightning / sync-diff / TiCDC 反向通道，以及种子行与迁移台账的处理（§3） | 拆 Redis、开削峰、调 Kafka 消息上限（§7，都不依赖 Phase 1） |
| 写热点的基础处置：全局打散、随机分布索引预切、防合并（§5.2） | 应用级回档 D8（仍是 Phase 2 前置） |
| BR + PITR runbook，备份 CR 在目标环境真实运行，重写 zone 灾难回档（§6） | |
| MySQL 退役步骤（R1 窗口结束后，§3.4） | |

> （2026-10-20 指向注，原文不删）表中「全部库同批迁到同一个 TiDB 集群」只是针对现网 2 个 zone 量级的结论。[容量模型](./capacity-model-hok-scale.md) §4.5 的口径是：S1 以上 OLTP（zone 库加全局薄层）与审计至少分两套集群，单元化后每个单元一套。两者的冲突尚未拍板，见 §2.7 末尾的注与 §9.3 第 12 条。

### 1.3 TiDB 能扩什么、扩不了什么

| 维度 | 加机器能不能扩 | 依据 / 本仓对应物 |
|---|---|---|
| 打散后的行数据写入 | 能。Region 自动分裂，调度到多台 TiKV | `NONCLUSTERED` + `SHARD_ROW_ID_BITS=4` 打散的是 `_tidb_rowid`。三张主表等已声明（`mysql_database_table.proto:90-92`、`:106-108`、`:164-166`） |
| 读 | 能。加 tidb-server 与 TiKV，Load Base Split 按读流量分裂 | — |
| 连接入口 | 能。tidb-server 无状态 | 客户端池仍要按总账配额（§2.4） |
| **单调递增索引的尾部** | **不能**。新值都比旧值大，只写最右一个 Region；分裂后，新的尾部仍承接全部写入 | `PRIMARY KEY(player_id)` 退化成的二级唯一索引、`uk_player_name_owner`、快照与流水的时间类联合索引、D1 `player_route` 的 4 条索引（§5.1）。（2026-10-12 G1a）D1 四条索引都带 16 片前缀，预切后不再是单尾，见 §5.1（2026-10-14 更正：行片已改为 64，见 D1 提案 §11.2「行片 16 → 64」） |
| **单行热点** | **不能**。一行只在一个 Region leader 上，悲观锁要排队 | `id_segment` 的 `FOR UPDATE` 行（`id_segment_store.go:184`）；D1 的 `zone_merge_fence` 分片行与 `route_meta` 单行（提案 :93-124）。（2026-10-12 G1a）`route_meta` 已拆出常写的 `route_reconcile`，开关行可设缓存表；`zone_merge_fence` 由 `route_tool tidb-prepare` 按 zone 预切。（2026-10-15 更正：「可设缓存表」已改定为 `route_meta CACHE` **S2 档起必选、S1 可选**；围栏分片数按档位取 MySQL 16 / TiDB 64，不再是 8。见 §5.1 单行热点一行与 §9.3 第 21 项的更正注） |
| **大事务 / 大行** | 有硬上限。单行 KV ≤ `txn-entry-size-limit`（默认 6MB，上限 120MB）；单事务 ≤ `txn-total-size-limit`（默认 100MB）【文 tidb-configuration-file】；TiKV 拒绝大于 `raft-entry-max-size`（默认 8MB）的写请求 | 快照行最大 16MiB−1（`snapshot_consumer.go:24`）；批量工具必须分批 |
| **跨地域延迟** | **不能**。每个事务都要取 TSO、走 2PC，写延迟至少是跨 AZ RTT 的两倍【文 multi-data-centers-in-one-city】。官方要求单集群跨地域延迟 <30ms（dr-solution-introduction） | §8 |

### 1.4 为什么「全局库在 TiDB 上不是单机瓶颈，但单调索引尾部是」

- **库只是命名空间。**
  - TiDB 按「表 ID + 键」区间把数据切成 Region，分散在多台 TiKV 上，跟库名无关。所以 `mmorpg_global`，或将来的 `player_store_1000000_db`，不会因为「是一个库」就落在一台机器上。
  - MySQL 正好相反，整台实例就是上限。K8s 上 zone 库和全局库都在同一台 `mysql:8.0` 上（`mysql.yaml:74`、`:82`、`:156-160`），拆不拆出全局库，上限都一样。
- **单调键的索引分散不了。**
  - 号段发的 `player_id` 从 1 开始，在每个 login 的段内严格递增（`go/shared/idsegment/client.go:1-24`）。所以任一时刻，所有新插入都落在该索引最右的一个 Region，由一台 TiKV 的 Raft leader 独自承接。
  - Load Base Split 只看读流量。
  - `PRE_SPLIT_REGIONS` 只预切行数据，每条索引建表时只有 1 个 Region【文 sql-statement-split-region：例子里是 4 个行 Region + 1 个 idx1 Region】。
- **Phase 1 与 Phase 2 的差别。**
  - Phase 1 仍按 zone 分库，每个 zone 库的表 ID 不同，所以尾部有 **Z 条**，散在不同 Region 上。
  - Phase 2 把各 zone 的玩家表收成一组表，Z 条尾部变成 **1 条**。这是 Phase 2 真正的扩展瓶颈，加机器解决不了（决策文档 §D3 的 10-12 更正第 3 条）。

### 1.5 首日瓶颈里，哪些靠 Phase 1

按 launch_verdict 的排序，依赖关系如下，详见 §7：

| 首日瓶颈 | 靠 Phase 1 吗 |
|---|---|
| 削峰默认全关；排队的 leader 锁跨 zone 不出队 | 否 |
| 单 zone 的 EnterGame 预加载链：新号首读冷读、go/db 80 个 worker、10 分区 | 否。Phase 1 只把库换成能扩的，管线上限不变 |
| 共享 Redis 单主 | 否 |
| **按现有清单开服时的 MySQL**：1 CPU / 1Gi，`max_connections=500`，2 个 zone 时零余量（2026-10-14 注：G2b 已把清单改为 `max_connections=1800`、内存 limits 5Gi，并加了部署期连接总账，见 §2.4 的状态注；CPU 仍是 1 核） | **是**。这是 Phase 1 唯一直接消除的一项 |
| 跨 zone 的全局耦合（所有 zone 的 go/db 共用一个消费组） | 否 |
| data_service 全局库上的同步写（名字登记、D1 事务） | 部分。库能扩了，但单行锁和单调尾部还在 |
| 全球审计管道：快照 >约 1MB 在发送端失败；积压超过每分区 512MiB 会被删 | 否（L-07） |
| TiDB 专属热点 | 这一项是 Phase 1 **引入**的，要按 §5 处置 |

---

## 2. 目标形态

### 2.1 部署方式：TiDB Operator 还是手写 StatefulSet

| 维度 | TiDB Operator v1.6 | 手写 StatefulSet |
|---|---|---|
| 版本匹配 | TiDB ≥ 8.0 推荐 Operator 1.6【文 tidb-operator-overview 兼容表】 | 自己维护 |
| 生命周期 | 官方称覆盖「deployment, upgrades, scaling, backup, fail-over」【文】 | 难以编排（推断）。TiKV 缩容要先在 PD 下线 store、等 tombstone，再删 Pod 和 PVC；PD 成员变更、按 PD → TiKV → TiDB 顺序滚动升级都要手工 |
| 配置 | `spec.tidb.config` / `spec.tikv.config`，推荐 `configUpdateStrategy: RollingUpdate`【文 configure-a-tidb-cluster】 | 照搬 compose 的 toml |
| 初始化 | `TidbInitializer` CR 加 `passwordSecret` 设置 root 口令【文 initialize-a-cluster】 | 自写 Job |
| 备份 | `Backup` / `BackupSchedule` / `Restore` CR，支持 S3 兼容存储、GCS、Azure Blob、PV【文 backup-restore-overview】 | 自写 CronJob 调 br |
| 高可用调度 | 支持 `topologySpreadConstraints` 和反亲和，zone 标签取 `topology.kubernetes.io/zone`【文】 | 自写 |
| 代价 | 多一个 operator 和一组 CRD，CRD 是集群级资源 | 生产不可接受 |

**推荐**：staging、prod 和压测环境用 **TiDB Operator v1.6.x**。本机仍用 `deploy/docker-compose.tidb.yml`。kind 上用 Operator 起最小拓扑（1 PD / 1 TiKV / 1 TiDB），这一步可选，因为 D-02 记过「本机 kind 资源紧」。手写 StatefulSet 不进生产。

**前置与纪律：**
- 安装 helm、TiDB Operator、CRD、tiup，属于 AGENTS §10.2 说的「需要安装或升级工具」，先取得授权（§9.3 #15）。
- 生产 apply 由人执行（AGENTS §9）。
- kind 上每条 helm / kubectl / `k8s_deploy.ps1` 命令都要显式点名集群：`--kube-context kind-mmorpg --kubeconfig …`，或 `-KubeContext` / `-KubeConfig`（`deploy/k8s/kind-config.yaml:4-21`，D93）。原因是本机默认 context 指向一个严禁触碰的集群，CRD 又是集群级资源，一旦落错集群，影响面是整个集群。

### 2.2 拓扑、namespace 与版本

| 档 | PD | TiKV | TiDB | 其它 | 说明 |
|---|---|---|---|---|---|
| prod | 3（每 AZ 1） | ≥3（每 AZ ≥1，副本数 3） | ≥2（跨 AZ） | TiCDC ×2（反向回退通道或异地灾备时用）、监控 | 同城三 AZ。一个 AZ 故障时 RPO=0，多数情况 20s 内恢复【文 multi-data-centers-in-one-city】。PD 配 `replication.location-labels`。`pvReclaimPolicy: Retain`。规格按压测定，官方硬件建议本次没核对 |
| staging | 3 | 3 | 2 | 形态同 prod，缩配 | 可能有要保留的数据（§3.0），所以**不做**登录压测。Retain |
| 压测环境（新增） | 3 | 3 | 2 | 与 staging 同拓扑 | 可以随时清空，开服尖峰压测在这一档做（§3.5、L-14）。`pvReclaimPolicy: Delete` |
| kind / 本机 | 1 | 1 | 1 | — | PD `replication.max-replicas=1`，否则只有 1 个 TiKV 会长期欠副本。只做功能验证，不出性能结论 |

> （2026-10-20 补注，原文不删）上表 prod 档是现网 2 个 zone 的量级。按 S 档定规格时，用[容量模型](./capacity-model-hok-scale.md) §4.5 的公式，以下数字都是推算 [I]：
> - **tidb-server 台数**：N ≥ 1 + 1.5·T_peak / max_connections，max_connections 取 2000 时，S1 需 4~8 台，S3 需 28~61 台。
> - **逻辑写**：S1 为 24~183 MB/s，S3 为 0.24~1.83 GB/s；Raft 写是它的 3 倍，还要再乘 LSM 放大（未测）。
> - **MVCC 旧版本**：S1 为 44~330 GB，S3 为 0.44~3.3 TB。
> - **玩家主数据存量**（3 副本）：S1 约 6 TB，S3 约 60 TB。
> - **TiKV 台数**：待 V5 实测。
>
> 「一套集群还是多套」见 §2.7 末尾的注与 §9.3 第 3、12 条。

- **namespace**：TiDB 放在独立 namespace，暂定 `mmorpg-tidb`，**不放** `${InfraNamespace}`。
  - 原因：`infra-down` 走的 `Remove-Infra` 会直接 `delete namespace $InfraNamespace`（`k8s_deploy.ps1:5888-5890`）。TiDB 放在里面，infra-down / all-down 就等于删集群；Retain 策略下 PV 还会变成孤儿。
  - 要删一个含备份 CR 的 namespace，先删 `Backup` / `BackupSchedule` CR，否则 namespace 会卡在 Terminating【文 backup-restore-overview】。
  - P1-2 规定：staging / prod 档在检测到 TidbCluster 存在时，infra-down 拒绝执行。
- Operator 默认拓扑就是 3 PD / 3 TiKV / 2 TiDB【文 configure-a-tidb-cluster】；`limits = requests`（Guaranteed QoS）【文】。
- **时钟**：所有节点都要 NTP 同步。
  - D1 的 `created_at_ms`、`updated_at_ms`、`route_meta.watermark_ms` 都取「库时钟」（提案 :78-79、:120）。（2026-10-12 G1a）水位已移到 `route_reconcile.watermark_ms`。
  - 换成 TiDB 后，库时钟就是各 tidb-server 的本机时钟（推断）。
  - 节点间的时钟偏差必须远小于 D1 回档检测的容差（提案 :274、:358），否则检测会误判为回档、把 `writable` 置 0，建角全停。切换时的核对见 §3.2 第 16 步。
- **版本**：钉 **v8.5.x，且 ≥ v8.5.5**。
  - 有两项能力从 v8.5.5 才开始有：PITR 按库表过滤，以及日志备份运行中也能做 PITR【文 br-pitr-manual】。§6.3 的单 zone 灾难回档依赖这两项。
  - dev compose 现为 v8.5.2（`docker-compose.tidb.yml:15/30/47`），要同步升级。
  - 具体 patch 号由负责人在部署时定。
- TiKV block-cache 与容器内存上限的关系【未核实】，规格压测时一起定。

### 2.3 接入与 LB

- **入口**：客户端全在集群内，用 Operator 生成的 ClusterIP Service 即可，形如 `<集群名>-tidb.mmorpg-tidb:4000`（命名以 Operator 实际生成为准【未核实】）。不需要外部 LB，运维访问用 port-forward。
- **连接不会自动重新均衡**：kube-proxy 是四层、按连接分流。tidb-server 扩容或滚动重启后，旧连接不会自动回到新实例上。
  - go/db 已设 `ConnMaxLifetime 5m`（`proto_sql/db.go:264`、`store_registry.go:316`），全仓也只有这两处 `SetConnMaxLifetime`。
  - P1-1 给下面这些池补上：data_service 的快照 / 号段 / 流水池、名字池、D1 route 池；friend；trade；guild；login PasswordAuth。
- **下线排空**：
  - TiDB 侧配 `graceful-wait-before-shutdown`，它的默认值是 0【文 tidb-configuration-file】，要设成 >0。
  - Pod 的 `terminationGracePeriodSeconds` 必须大于「graceful-wait + 在途语句排空时间」，否则 kubelet 会先 SIGKILL。

### 2.4 连接上限

**总账**：Phase 1 不减少连接数。

| 来源 | 连接数 | 出处 |
|---|---|---|
| go/db | 每 zone 2×(60+8E) | PROGRESS.md:7178；no-single-node :154-167 |
| data_service | 每 zone 2×35（三个 store 各 5，名字池 20） | `deploy/k8s/manifests/go-svc/data-service.yaml:34` |
| Java gateway | 每 zone 2×10（Hikari 默认值，推断） | PROGRESS.md:7178 |
| friend、trade | 全局各 2×20 | 同上 |
| **D1 route store 独立池（新增）** | 每 zone 2×R，R 由 D1 第 1 批定 | 提案 :155 |
| **login PasswordAuth（条件性）** | 每 zone 2×20，只在开启时有 | `go/login/etc/login.yaml:336-345`（默认 `Enabled: false`） |
| 滚动余量 | maxSurge 1 时，每 zone 临时多出 (60+8E)+35 | no-single-node :160 |
| 工具 | migrate Job、merge_zone、data_consistency_check、Dumpling / Lightning / TiCDC | — |

- 名义值 T = Z·(210+16E) + 80，不含新增两项。Z=2、E=0 时 T=500；Z=4 时 T=920。
- T_peak = T + D1 池 + PasswordAuth + 滚动余量 + 工具。
- **（2026-10-13 G2b 状态注，已落码，未编译、未测试；数值均待压测确认）** 上表是 2026-10-12 的核算口径，原表不改。MySQL 档的现行口径以可执行总账为准：`tools/scripts/k8s_deploy.ps1` 的 `Get-MysqlConnectionPlan` / `Get-MysqlConnectionLedger`，说明见 `deploy/k8s/README.md`「MySQL 连接总账」，规格见 `docs/handoff/launch-readiness-20261013/spec_G2b.md`。与上表不同的地方：
  - **Java gateway 行**：现为显式 2×5。`java/gateway_node/src/main/resources/application.yaml` 配了 Hikari `maximum-pool-size: 5`、`minimum-idle: 2`，K8s ConfigMap 镜像这两个值，不再是按默认值推断的 2×10。
  - **滚动余量行**：按 R=2 计，即滚动期间每个 Deployment 多 2 个实例（maxSurge 的 1 个新 Pod，加 1 个仍在优雅退出、连接还没释放的旧 Pod）。范围是一个 zone 的 db / data-service / gateway 三个 Deployment，加全局的 friend、trade；不是上表的「每 zone 临时多出 (60+8E)+35」。这里的 R 是多出的实例数，与「D1 route store」行里表示池大小的 R 不是同一个量。
  - **D1 route store 行**：池大小已定为 20（`go/data_service/etc/data_service.yaml` 的 `RouteStore.MaxOpenConn`）。K8s 上 `RouteStore.Mode` 现在固定 `redis`、不装配这个池，规划档按 db_first 提前计入。
  - **取值**：规划档（4 个 zone、每个 go/db 实例打开其余 3 个 zone 的库、Phase 2 未开、运维余量 20）峰值 1620；`deploy/k8s/manifests/infra/mysql.yaml` 的 `max_connections` 已从 500 改为 1800，内存 limits 从 1Gi 改为 5Gi。login PasswordAuth 仍不计入（K8s ConfigMap 不写该段）。
  - 下面「两种后端都要做的」三条里，后两条已由 G2b 落码：池指标见 §7 L-08 的状态注；旋钮拆成 `Placement.ZoneStore*` / `NonZoneStore*`，`ExtraStoreMaxOpenConn` 只作回落。第一条（L-04）没有做。
  - 换成 TiDB 后，总账里各项客户端连接数不变，变的只是上限参数，取值口径见下面「TiDB 侧」。

**TiDB 侧：**
- 单实例上限是 `instance.max_connections`，默认 0（不限），范围 0–100000；同时设了 `max-server-connections` 时以前者为准。并发执行的会话数由 `token-limit` 控制，默认 1000【文 tidb-configuration-file】。
- 推论：在 TiDB 上，只有显式设了上限，才会出现 1040。不设上限时，连接风暴的后果会从 1040 变成 tidb-server 的内存压力（推断）。真正起约束作用的是 tidb-server 的内存和 `token-limit`。
- **取值口径：每实例上限是保护闸，不是配额。**
  - 设有 N 台 tidb-server。一台故障时，剩下的每台平均承接 T_peak/(N−1)。kube-proxy 按连接随机分流，还要再乘一个不均衡系数 k（先取 1.5，压测时测定）。所以 `max_connections ≥ k·T_peak/(N−1)`。
  - tidb-server 的内存规格，按这个连接数加上 `token-limit` 的并发去压测确定。
  - v0.1 写的「每实例 ≥ T×(1+余量)」过于保守：上限一旦高到不可能触发，就失去了保护作用，等于不设。

**两种后端都要做的**（不会随 Phase 1 消失，见 §7 L-04、L-08）：
- go/db 遇到「拿不到连接 / 1040」时改为延后，不消耗重试预算。问题陈述见 PROGRESS.md:7181，建议见 :7188。
- 导出 `sql.DBStats`，label 只用池类别。
- 把全局库池和访客池拆成两个旋钮。`ExtraStoreMaxOpenConn` 现在是全局单旋钮（PROGRESS.md:7183）。

### 2.5 配置（§D4 更新版）

| 位置 | 项 | 默认 | 取值 | 依据 |
|---|---|---|---|---|
| TiDB 配置 | `performance.txn-entry-size-limit` | 6291456 | **33554432**（dev 已配，`tidb.toml:7`） | 快照行最大 16MiB−1（`snapshot_consumer.go:24`）；配置上限 120MB；v7.6 起可用 `tidb_txn_entry_size_limit` 动态改【文】。现状下大于约 1MB 的快照到不了库（§4 #5），这一项作为纵深防御保留 |
| TiKV 配置 | `raftstore.raft-entry-max-size` | 8MB | **32MB**（dev 已配，`tikv.toml:7`） | TiKV 拒绝大于它的单个写请求【文】 |
| TiDB 配置 | `performance.txn-total-size-limit` | 100MB | 默认 | 迁移、合服、D1 导入等批量工具必须分批 |
| TiDB 配置 | `instance.max_connections` | 0 | 按 §2.4 | 【文】 |
| TiDB 配置 | `token-limit` | 1000 | 默认，压测时观察 | 【文】 |
| TiDB 配置 | `graceful-wait-before-shutdown` | 0 | >0，具体值待定，配合 §2.3 | 【文】 |
| TiDB 配置 | `pessimistic-txn.constraint-check-in-place-pessimistic` | true | **保持 true，在配置文件里钉住** | 对应的系统变量 `tidb_constraint_check_in_place_pessimistic` 只有 SESSION 作用域，默认值由这个配置项决定。关掉后，唯一性检查会推迟到后续加锁语句或提交时【文 system-variables、constraints】。名字登记依赖立即判重（`player_name_store.go:355-388`） |
| TiDB 配置 | Global Kill | 自 v6.1 起默认开 | 保持 | 迁移超时时从旁路发的 KILL 要靠它跨实例生效（§4 #2）【文 sql-statement-kill】 |
| 全局变量 | `tidb_enable_check_constraint` | **OFF**（GLOBAL） | **ON，必须在建表之前设** | CHECK 默认不生效【文 constraints、system-variables】。依赖它的有：`id_segment` 的 `max_id < 2^55`（`schema.go:66`）、D1 `player_route` 的 5 条 CHECK（2026-10-12 G1a 起 6 条：新增的 `chk_route_shard` 关系到 `player_id` 唯一性 —— 主键是 `(route_shard, player_id)`；route store 建池探针 P4 实测它生效，不生效即拒绝建 store）、`zone_merge_fence` 的 1 条 CHECK。D1 提案 :64 写的「TiDB ≥ 7.2 生效」要补上这个前提（§9.5）。开关关闭期间建的表，打开后会不会补上约束【未核实】，所以先开后建 |
| 全局变量 | `tidb_scatter_region` | `""`（SESSION \| GLOBAL） | **`global`，建表前以 GLOBAL 设** | 可选值 `""` / `table` / `global`。代价是 `CREATE TABLE` 要等打散完成，可能慢好几倍【文 system-variables】 |
| 全局变量 | `default_collation_for_utf8mb4`（v7.4+） | `utf8mb4_bin` | **`utf8mb4_0900_ai_ci`**（兜底，见 §4 #10） | 只能取 `utf8mb4_bin` / `utf8mb4_general_ci` / `utf8mb4_0900_ai_ci`【文 system-variables】，**设不成 `utf8mb4_unicode_ci`**，所以建库时仍要显式写排序规则 |
| 全局变量 | `tidb_enable_clustered_index` | ON | 保持 | 没声明 `NONCLUSTERED` 的表会建成聚簇（§4 #8） |
| 全局变量 | `tidb_txn_mode` | pessimistic | 保持 | 两处依赖：D1 的锁定读（提案 :63）；guild 的全连接 RC，其注释写明「TiDB 只在悲观事务模式下支持 RC」（`guild_manage_repo.go:329`）。本次在 v8.5 系统变量页没检出这一条目【未核实】，P1-0 用 `SHOW GLOBAL VARIABLES` 核对 |
| 全局变量 | `innodb_lock_wait_timeout` | 50（范围 1–3600） | 由各应用在会话里设 | TiDB 支持【文 system-variables】。guild 设为 1（`guild_manage_repo.go:315`）；go/db 与 schemamigrate 的迁移会话默认 5（`go/db/internal/config/config.go:231-232`）。都在范围内 |
| 首次初始化 | `new_collations_enabled_on_first_bootstrap` | true | 保持 true | 只在集群首次初始化时生效【文】 |
| 建库 | 库默认排序规则 | TiDB 是 `utf8mb4_bin` | 建库时显式写 `utf8mb4_unicode_ci` | 与 MySQL 的 `collation-server` 一致（`mysql.yaml:71`），见 §4 #10 |
| PD | `replication.location-labels`、`max-replicas` | — | prod 按 zone / host 标签 3 副本；kind 为 1 | 【文 multi-data-centers-in-one-city】 |

### 2.6 账号、权限与初始化

**root 口令：**
- 新建的集群里 root 是空口令。
- 用 `TidbInitializer` CR 加 `passwordSecret` 设置口令，Secret 要在建集群之前就存在。
- 口令**不要**写进 `initSql`：那里写的口令会以明文存在初始化 Job 对象里【文 initialize-a-cluster】。
- `TidbInitializer` 只在集群首次创建时生效，之后改它没有作用【文】。

**幂等初始化 Job（本仓自写，可以反复跑）**：代替现在的 `mysql-init-sql` ConfigMap（`New-MysqlInitConfigMapYaml`，`k8s_deploy.ps1:5494`）。
- 应用口令从 Secret 经 env 注入（`secretKeyRef`），不进 Job spec，不进 git（AGENTS §9）。
- 用 `CREATE USER IF NOT EXISTS` 建应用账号。
- 按库 GRANT，口径与现在 `:5516-5535` 的逐库授权一致。TiDB 上能否用通配库名授权【未核实】。
- 设置 §2.5 的全部全局变量。
- 显式写排序规则，建这些库：`zone_{N}_db`、`mmorpg_global`、三个服务库、`mmorpg`。
- **把 mysql 镜像 entrypoint 隐式做的三件事显式补上**（`mysql.yaml:113-128` 的注释）：
  1. 按 `MYSQL_USER` 建出 appuser；
  2. 对 `MYSQL_DATABASE=mmorpg` 自动授权；
  3. 以 `--database=mmorpg` 执行 init 脚本，所以没写 `USE` 的脚本也落在 `mmorpg`。
  `gateway_tables.sql` 要在 `mmorpg` 里执行。
- 在这些时候重跑：§3.5 清库之后、建影子集群之后、BR 恢复之后（§6.2。BR 会不会恢复全局变量【未核实】，一律按「不会」处理）。

**权限边界：**
- 应用账号不给 `CREATE DATABASE`。`player_store_*_db` 由 `go/db/cmd/migrate -storage-id … -create-database` 用运维账号建（player-storage-placement.md §11.1）。
- 应用账号不得有 `SUPER` / `CONNECTION_ADMIN`。这一条在 R1 窗口里对 MySQL 侧同样关键（§3.2 第 10 步）。
- 沿用 `k8s_deploy.ps1` 的凭证注入口径：`MMORPG_MYSQL_USER` / `MMORPG_MYSQL_PASSWORD`（`:446-451`）；Java 网关仍用 `MMORPG_GATEWAY_DB_*`。`mysql.yaml` 与备份 CronJob 的明文口令问题，不要带进 TiDB 清单（runbook §7）。

### 2.7 各服务 DSN 怎么切

**两条硬约束决定了「全部库同批、同集群」：**

1. **go/db 只有一个 `ServerConfig.Database.Hosts`**（`proto_sql/db.go:112`），按需打开落点库时只改 DSN 里的库名（`store_registry.go:307-316`）。访客预加载会打开别 zone 的库，所以所有 zone 库和落点库，必须与各个 go/db 在同一个 MySQL 实例或 TiDB 集群上（player-storage-placement.md:28、:455）。
   推论：**不能按 zone 分批切**，所有 zone 的 go/db 必须同时切。
2. **merge_zone 只有一个 `-mysql-dsn`**。它要求在同一实例上同时访问账号表、各 `zone_<N>_db`，以及 guild、trade、friend 三个服务库（`tools/merge_zone/main.go:246-249`）。D1 第 2 批还会给它加一个 `-route-db-dsn`，用来读写全局库的映射表（提案 :303）。
   推论：决策 D9 写的「friend / guild 失败可单独回退留 MySQL」与此冲突。**本文改为同批迁、同批回退**。如果确实有某个服务单独回退，合服就冻结，直到重新合一。

> （2026-10-20 补注：回写容量模型 §4.5，原文不删）
> - [容量模型](./capacity-model-hok-scale.md) §4.5 / §6 #11 的拓扑建议是：OLTP（zone 库加全局薄层）与审计分开，快照 blob 不进 TiDB、只迁元数据，流水用独立集群或列存；「全部库同批迁到同一个集群」在 S1 以上改成至少两套；单元化后每个单元一套同城三 AZ 集群，全球薄层在单一写入地域一套（与 §8 第 1 条同义）。
> - 上面两条硬约束只约束 OLTP 侧：go/db 与 merge_zone 要能在同一集群里访问所有 zone 库、落点库、账号表和三个服务库。审计表（`player_snapshot`、`transaction_log`）不在其中，可以另起集群（推断）。
> - 但拆出审计之前，要先给 data_service 加独立的审计 DSN（推断）。原因：data_service 目前只有一个 `SnapshotMySQL` DSN（`go/data_service/internal/config/config.go` 的 SnapshotMySQLConfig），同时承载 id_segment、player_name、transaction_log、player_snapshot 和 D1 路由表。
> - merge_zone、回档、data_consistency_check 是否读审计表，未核实。
> - 两份文档的结论冲突，尚未拍板，列为 §9.3 第 12 条；开服线拍板清单 [decisions_launch_line_20261020.md](../handoff/launch-readiness-20261013/decisions_launch_line_20261020.md) 记为 T-12。§9.2 的分批与文件数估算可能要随之上调（推断）。

| 服务 | 连的库 | 本地配置 | K8s 生成段（`tools/scripts/k8s_deploy.ps1`） | 切法 |
|---|---|---|---|---|
| go/db | `zone_{N}_db`、`player_store_*_db`、访客时的他区库 | `go/db/etc/db.yaml:36-37`（`Database.Hosts`） | `:3442-3443` | Hosts 改为 TiDB Service:4000，所有 zone 同时切 |
| data_service（含 data-service-migrate Job） | `mmorpg_global`（本地叫 `testdb`）：`id_segment`、`player_name`、`transaction_log`、`player_snapshot`、`rollback_audit_log`，以及 D1 的 4 张表 | `data_service.yaml:92-98`（`SnapshotMySQL`）；名字池 `:109-110` 共用这个 Host | `:3511-3517`（库名用 `$GlobalDbName`，`:997`） | 同上。DSN 由 `store/mysql.go:33` 拼 |
| trade（含 trade-migrate Job） | `mmorpg_trade` | `go/trade/etc/trade.yaml:50-51` | `:3958` | 同上 |
| friend（含 friend-migrate Job） | `mmorpg_friend` | `go/friend/etc/friend.yaml:92-93` | `:4073` | 同上 |
| guild | `mmorpg_guild` | `go/guild/etc/guild.yaml:44-45`（整串 DataSource） | **没有 K8s 清单**（no-single-node §7） | 补清单时直接写 TiDB |
| Java gateway | `mmorpg`（`zone_config` 等） | `java/gateway_node/src/main/resources/application.yaml:10-11` | `:4925-4926` | `jdbc:mysql://<tidb>:4000/mmorpg?...` |
| **login（条件性）** | `user_accounts` 所在的库（zone 库） | `go/login/etc/login.yaml:336-345`（`PasswordAuth`，DSN 取自环境变量 `LOGIN_PASSWORD_AUTH_DSN`，默认关）；每次口令登录都读 `user_accounts.password`（`password_provider.go:80`） | K8s 现在只用 `-LoginDevPasswordAuth`（`:230-234`），不连库 | 正式服若开启，DSN 同批切；连接数计入 §2.4 |
| 工具 | — | merge_zone `-mysql-dsn`（`main.go:246-249`）及 D1 第 2 批的 `-route-db-dsn`；data_consistency_check `-mysql-dsn` / `-friend-mysql-dsn`（`main.go:92`、`:99`）；`go/login/cmd/password_admin`（`main.go:61`）；`go/guild/cmd/assetopfix`；`go/data_service/cmd/debugutil` | — | runbook 和命令行一起改 |
| 脚本 | — | `stress_round19.ps1`（TRUNCATE，`:59-60`、`:103`）；`currency_crash_window.ps1:118-130`（TRUNCATE）；`k8s_zone_rollback.ps1` Step 3（工作区 `:903-915`）；`start_game.ps1:446-474`（本地写死 compose 的 `mysql` 容器）；`mysql-backup-cronjob.yaml`（**不在** infra 清单列表 `:5740` 里，见 §6.1） | infra 清单列表 `:5740` | §3.5、§6 |

C++ 节点不直连数据库，`cpp/` 全仓没有 MySQL 客户端调用。

login 只在 `PasswordAuth` 开启时连库。它的存量迁移脚本是 MySQL 存储过程（§4 #13）。

### 2.8 `k8s_deploy.ps1` 改造点（P1-2 批）

1. **开关**：新增 `-Database mysql|tidb`，backlog P1-D2 第 1 步（:802）已有此建议。tidb 档要做的：
   - 改五处 DSN：go/db、data_service、trade、friend、Java gateway；guild 补清单时直接写 TiDB；login PasswordAuth 如果部署，也一并改。
   - infra 清单用 TiDB CR（放在 `mmorpg-tidb`）代替 `mysql.yaml`。
   - 初始化 ConfigMap 换成 TidbInitializer 加幂等初始化 Job（§2.6）。

2. **开关不粘滞，所以改为必填并加预检，防止静默分库。**
   - **问题**：部署参数不粘滞。回滚 Step 6、合服后的 zone-up、release-zone，以及各包装入口，都会按「本次」传入的参数重新生成 ConfigMap（同类教训见 `zone_data_rollback.md:206-240`）。切换后只要漏传一次，该 zone 的 go/db 和 data_service 就会指回 R1 窗口里仍在运行的 MySQL。backlog 风险段的原话是「DSN 指错会让 zone 数据分裂」（:823）。按 §2.7 约束 1，访客读还会跨库混跑。
   - **改法**：
     - staging / prod 档 `-Database` 必填，不给默认值。
     - 预检先读集群现状：`mmorpg-tidb` 里有没有 TidbCluster，或 infra namespace 里的一个 `data-backend` 标记 ConfigMap。与参数不一致就拒绝。`k8s_zone_rollback.ps1` 的第 0 步做同样的核对。
     - `dev_tools.ps1`、`k8s_image.ps1` 等包装入口必须透传这个参数（`deploy/k8s/AGENTS.md` 的「包装入口透传」纪律）。
     - 兜底：R1 窗口内 MySQL 保持 `read_only=ON`，应用账号没有 SUPER（§3.2 第 10 步），这样指错库的写会报错，而不是写进旧库。

3. **migrate Job 与发布门禁不再写死 MySQL。**
   - **问题**：
     - 三个 migrate Job 的 initContainer `wait-mysql` 写死了 `mysql.__INFRA_NAMESPACE__` 和 3306（`data-service-migrate.yaml:60-88`、`trade-migrate.yaml:73-102`、`friend-migrate.yaml:81-110`）。
     - staging / prod 档在 apply Job 之前，会先 `Wait-ForDeploymentReady … "mysql"`（`k8s_deploy.ps1:4203-4211`）。
     - 后果：MySQL 退役后，这一步会一直等到超时，发布中断；R1 窗口里 MySQL 还在时，门禁等的是错的库，形同虚设。
   - **改法**：
     - 地址和端口改成占位符 `__DB_ADDR__` / `__DB_PORT__`，由 `Apply-GoSvcMigrateJob` 按 `-Database` 替换。
     - tidb 档改为等 TidbCluster Ready。
     - 同步核对 `$GoSvcMigrateJobMinWaitSeconds`。initContainer 的注释写明两者联动（`trade-migrate.yaml:79`）。

4. **go/db 迁移的执行方式**：K8s 上没有 go/db 的 migrate Job。推荐 P1-2 新增一个 db-migrate Job（§3.2 第 5 步，§9.3 #16）。

5. **infra-down 保护**：TiDB 放在独立 namespace，不受 `Remove-Infra` 影响；staging / prod 档的 all-down 也不碰它（§2.2）。

6. **契约测试**（`tools/scripts/tests/k8s_deploy_contract.tests.ps1`）钉住以下几点：
   - 同一部署里所有 DB Host 完全相同；
   - tidb 档的渲染结果里没有 `mysql.`，也没有 `:3306`；
   - initContainer 的地址、端口与 DSN 一致；
   - 初始化 Job 先于任何 migrate Job 完成；
   - **mysql 档的渲染结果与现在逐字一致**，保证两档并存、可以切回；
   - TiDB 清单和备份 CR 确实被 apply（§6.1）。

7. staging / prod 档保持 `AutoCreateDatabase` / `AutoMigrateSchema` / data_service `Schema.AutoMigrate` 为 false，与现状一致（`:3449-3451`、`:3520`）。

---

## 3. 迁移方案

### 3.0 先回答：要不要迁

**开服前切换时，大部分环境不需要迁数据。** 新环境直接在 TiDB 上起，用官方迁移工具建表，再按 §5.2 预切即可。下面的迁移流程只服务于需要保留数据的环境，比如 staging、内测服。

**哪些环境有要保留的数据，还没核实**，列为 §9.3 #5 的拍板项。P1-0 要用 `information_schema.TABLES` 统计各环境、各库的数据量，作为选迁移方式和估算窗口时长的输入。

### 3.1 方式对比

| 方式 | 适用 | 停服 | 组件 | 推荐 |
|---|---|---|---|---|
| 全新建库 | 没有要保留的数据 | 不涉及 | TiDB | **首选** |
| 停服窗口：Dumpling + Lightning | 数据量小、可以停服 | 是，全服停（§2.7 第 1 条约束） | Dumpling、Lightning、sync-diff-inspector | **有存量时推荐** |
| 在线：DM 全量 + 增量，短窗口切换 | 数据量大、窗口受限 | 只停切换窗口 | DM 集群。官方定位：<1TiB 用 DM，速度 30–50GB/h，`task-mode: all`【文 migrate-small-mysql-to-tidb】 | 开服前不推荐，多一套组件 |

### 3.2 停服窗口流程（推荐）

#### 前置（不停服，提前做）

1. **计时**：在影子环境把本节完整演练一遍并计时。窗口时长以演练结果为准。

2. **工具放在哪里跑**：
   - Dumpling、Lightning、sync-diff-inspector 用 PingCAP 官方镜像，版本与集群一致，在 `mmorpg-tidb` 里起临时 Pod 跑；也可以在运维机上用 tiup。
   - 导出文件放 PVC 或 S3，容量至少按数据量 ×2 预留。
   - Lightning 物理导入的排序目录（`sorted-kv-dir`）要单独用一块本地盘。
   - TiCDC 用 Operator 的 TiCDC 组件。
   - 镜像和 CR 的具体写法【未核实】，在 P1-3 定。

3. **TiDB 集群就绪**：TidbInitializer 和幂等初始化 Job（§2.6）都跑完。

4. **源库先对齐版本**：把 MySQL 侧的所有迁移跑到与即将部署的代码同一版本，包括 go/db `cmd/migrate`、`data_service -migrate`、friend / trade / guild 的 `-migrate`。两侧台账的 version 集合要一致，否则导入的数据与 TiDB 上的表结构对不上。

5. **用官方迁移工具在 TiDB 上预建全部表结构，不用 Dumpling 导出的表结构**（原因见 §4 #11）。依次是：
   - **每个 zone 跑 go/db `cmd/migrate -command up`**，有 `player_store_*` 时加 `-storage-id`。
     - K8s 上**没有** go/db 的 migrate Job：`k8s_deploy.ps1:937` 里没有 `MigrateJob`；staging / prod 靠人工执行 `go run ./cmd/migrate`（`:3369-3370`）；手工步骤见 `merge-zone-runbook.md:235-238`。
     - 二选一：继续手工跑，port-forward 到 TiDB Service，用一份指向它、不进 git 的 `db.yaml` 副本；或者在 P1-2 新增 db-migrate Job（推荐）。
     - 选哪种都要注意：go/db 的启动闸 `assertSchemaUpToDate` 会核对台账（`proto_sql/db.go:305-323`），所以要和第 7 步的台账处理一起考虑。
   - **跑 `data_service -migrate`**。前提是 P1-1 的快照去重已经合入，否则它在 TiDB 上一定失败（§4 #1）。
   - **跑 friend / trade / guild 的 `-migrate`**（`go/schemamigrate`）。
   - **跑 gateway 的 `gateway_tables.sql`**。

6. **预切与核对**：跑 §5.2 的预切；`GetCreateTableSQL` 与 TiDB 的 `SHOW CREATE TABLE` 做全表 diff（§9.1 #4）。
   - （2026-10-12 G1a）D1 路由表的预切用 `route_tool -f <yaml> tidb-prepare -zones=<全部 zone> [-storages=1000000] -dry-run=false`（§5.2 第 7 条）。它会先核对 `player_route`、`zone_merge_fence` 的 `TIDB_PK_TYPE=CLUSTERED`，不是就停（退出码 2）。

7. **清掉预建时写进去的行**（§4 #14）。不清的话，第 12 步导入必然撞主键：

   | 表 | 谁写的 | 处理 |
   |---|---|---|
   | `id_segment` | `data_service -migrate` 用 `INSERT IGNORE` 预种 7 个 biz_tag，`max_id=1`（`id_segment_store.go:253-265`；种子清单 `k8s_deploy.ps1:3533`） | 导入前 `DELETE`，然后导入 MySQL 的行 |
   | `route_meta`（D1） | migrate 写入 `id=1`（提案 :299） | 同上。导入源库的行，保留源库的 `generation` |
   | `route_reconcile`（D1，2026-10-12 G1a 从 `route_meta` 拆出） | migrate 写入 `id=1` | 同上。导入源库的行（水位、租约随之过来） |
   | `zone_config` | `gateway_tables.sql:37-39` 的 `INSERT IGNORE` 种子 | 同上 |
   | 各库的 `schema_migrations` | go/db `cmd/migrate`（`go/db/internal/migrate/runner.go:324-333`）与 schemamigrate（`go/schemamigrate/runner.go:33-44`）建台账并写入 version 行 | **不导**。Dumpling 用 `--filter` 排除；以 TiDB 侧 migrate 的结果为准，核对两侧的 version 与 checksum 一致 |

   - 清行用 `DELETE`，不用 `TRUNCATE`。TRUNCATE 会换表 ID，预切的 Region 和 table attributes 不会跟过去（§3.5，推断）。
   - P1-3 的脚本在导入前断言目标表全部为空（`schema_migrations` 除外），不空就停。
   - **不允许为了让导入通过而把冲突策略改成 `ignore` 或 `replace`。** 以 `id_segment` 为例：它会停在 `max_id=1`，号段倒回，下一次领段就重发旧号，静默覆盖已有的角色行（runbook :138-148）。

#### 窗口内

8. **停入口**：
   - 发公告；把全部 zone 置为维护态（`POST /admin/zones/{id}/maintenance`，见 `merge-zone-runbook.md:454-462`）；等在线玩家下线，再强踢剩余的人。
   - **维护态挡不住自动选区**：`zoneId<=0` 会直接放行（`AssignGateService.java:174-177`），白名单也还没接上（`:166-167`）。所以还要在入口层断开，即 Ingress 或 gateway 的对外入口。

9. **停写，分两段**。模板是 `merge-zone-runbook.md` §8 Step 2。
   - **不能一上来就 `k8s-zone-down`**：它会删整个 zone namespace（`k8s_deploy.ps1:5426`），而 db 和 data-service 都是 zone 级服务（`:937-938`，没有 `Global`），会被一起删掉。存盘和审计的积压就再也没有消费者了。
   - (a) 各 zone 用 `kubectl scale` 把 gate 和全部 scene 缩到 0，**不删 namespace**，go/db 和 data_service 保持运行。全局的 trade、friend、guild，以及其它直连库的服务，也缩到 0。
   - (b) 等以下几项全部排空：
     - 各 `db_task_zone_*` 在 `db_rpc_consumer_group` 里的 LAG 为 0；
       - **（2026-10-16 更正，上一条不删；已落码，未编译、未测试，待 Codex 验证）** 按 zone 拆组后，以本 zone 线上 `go-svc-db-config` 的有效组为准查 LAG：切组收尾后是 `db_rpc_consumer_group_z<ZoneId>`，尚未切组的 zone 仍是 legacy 组 `db_rpc_consumer_group`，交接没收尾时两组都看。已切组的 zone 只查 legacy 组看到的是冻结或已删除的位点，证明不了已排空。查法见 `merge-zone-runbook.md` §8 Step 2。
     - `kafka:retry:*`、`kafka:dead:queue:*` 以及每个实例的 processing 列表都为 0（键名和查法见 `merge-zone-runbook.md` §8 Step 2）；
     - `transaction_log` 和 `player_snapshot` 两个全局消费组（`data_service.yaml:216-220`）的 LAG 为 0。
     死信怎么处置由人决定（决策文档风险 #10）。
   - (c) 然后对全部 zone 执行 `k8s-zone-down`。
   - 本文**不采用**「残留 lag 留到切换后再落进 TiDB」。那样 MySQL 基线和 TiDB 就无法逐行比对（§3.3），R0 回退时也会丢掉这部分写入。

10. **MySQL 置只读**：设 `read_only=ON`。
    - 前提：应用账号没有 `SUPER` 或 `CONNECTION_ADMIN`，否则 `read_only` 拦不住它（MySQL 8.0 的语义，推断级）。
    - dev 档的应用账号会回落为 root（`k8s_deploy.ps1:448-449`），所以 **dev 档不进 R1**（§3.4）。
    - 再做最后一次全量备份，作为回退基线。

11. **Dumpling 导出**：
    - 按库只导数据：`-B <库> --no-schemas`，用 `--filter` 排除 `schema_migrations`（filter 的写法以 P1-3 实测为准）。
    - 文件大小 `-F ≤256MiB`【文 dumpling-overview】。
    - MySQL 源的 `--consistency` 缺省是 `flush`，即 FTWRL，需要 RELOAD 权限（推断）。写已经停了、库也只读，也可以用 `none`。用运维账号执行。

12. **Lightning 导入**：
    - 数据量小用逻辑导入（`backend = "tidb"`），导入期间 TiDB 可用；数据量大用物理导入（`local`），导入期间 TiDB 不对外服务。
    - `conflict.strategy = "error"`。逻辑导入时，空字符串 `""` 也按 error 处理【文 tidb-lightning-configuration】。
    - v8.5 的配置页里没有「不建表、只导数据」这个选项。数据目录里没有 schema 文件时，Lightning 会不会直接用目标侧已有的表【未核实】，P1-3 实测。如果不行，就把 TiDB 上 `SHOW CREATE TABLE` 的输出写成 schema 文件，保证与预建结果逐字一致。

#### 校验

13. 见 §3.3，全部通过才切换。

#### 切换

14. **记下 TSO，开反向通道**：
    - 时机：校验通过之后，任何应用连上 TiDB 之前。
    - 取当前 TSO 作为 start-ts，建 TiCDC changefeed（TiDB → 原 MySQL）。导入写进去的数据不会被回放，因为它早于 start-ts。
    - MySQL 保持 `read_only=ON`。TiCDC 的 sink 账号授 `CONNECTION_ADMIN` 才能写入。

15. **部署**：
    - 用 `k8s_deploy.ps1 -Database tidb` 重新生成 ConfigMap（staging / prod 必填，并与集群标记一致，§2.8）。
    - 先起 data_service（migrate Job 这时应当无事可做）和 go/db，再起其余服务。
    - 入口仍然断开。

16. **放流量前的冒烟**：
    - robot 走集群内地址冒烟：登录、建角、存盘、进退场快照、好友推荐、交易下单。
    - robot 能不能绕过维护态【未核实】。绕不过的话，临时把一个测试 zone 置为 OPEN，入口只对内开放。
    - **D1 的三项检查**：
      - `route_meta.writable = 1`；
      - `data_service -route-audit` 两个方向都为 0；
      - `watermark_ms` 没有倒退。库时钟已经从 MySQL 换成了 tidb-server，提案 :274 的回档检测会把倒退判成回档，置 `writable=0`，建角全停。
      - （2026-10-12 G1a）`writable` 在 `route_meta`；`watermark_ms` 在 `route_reconcile`。

17. **放流量**：把 zone 置为 OPEN，恢复入口。

### 3.3 校验

| 层 | 做法 | 依据 |
|---|---|---|
| 行数 | 逐库逐表 `COUNT(*)`，MySQL 与 TiDB 对比 | — |
| 内容 | 用 sync-diff-inspector 跑 MySQL 对 TiDB 的分块校验和 | 【文 migrate-from-tidb-to-mysql 中的用法】 |
| 导入自检 | 物理导入时设 Lightning `post-restore.checksum = "required"` | 逻辑导入模式下，checksum 与 ANALYZE 都会**被跳过**【文】 |
| 统计信息 | **导入后对全部表手动 `ANALYZE TABLE`** | 逻辑导入不做 ANALYZE；而统计信息低估，正是 friend 推荐查询退化成全表扫描的触发条件（§4 #3） |
| 种子行与台账 | 导入前目标表为空（§3.2 第 7 步）；两侧 `schema_migrations` 的 version 与 checksum 一致 | §4 #14 |
| 号段 | `id_segment` 各 biz_tag 的 `max_id` 两边完全相同 | runbook :138-148 |
| D1 | `route_meta` 与源库一致；`-route-audit` 为 0 | 提案 §5.4、§6 |
| 应用级 | `tools/data_consistency_check`，L1–L4 抽样 | 决策 D5 |

### 3.4 回退到 MySQL：路径、时限与退役

| 阶段 | 条件 | 路径 | 数据损失 | 时长 |
|---|---|---|---|---|
| R0 | 已切换、未放流量 | 用 `-Database mysql` 重新生成 ConfigMap，改回集群标记，MySQL 解除只读 | 0 | 分钟级 |
| R1 | 已放流量，反向 changefeed 在跑；**只在应用账号不是 root / SUPER 的档位开放**（staging / prod） | 见下面的「R1 切回步骤」 | 0（前提是 changefeed 健康） | 一个维护窗口 |
| R2 | 反向通道已拆，或 R1 失效 | 不再算「回退」，按一次新的 TiDB → MySQL 迁移处理（Dumpling + 导入） | — | 一个维护窗口 |

**R1 切回步骤：**
1. 停掉 TiDB 侧的全部写入口，并排空 Kafka（同 §3.2 第 8–9 步）。
2. 等 changefeed 的 checkpoint 晚于停写时刻（口径见【文 migrate-from-tidb-to-mysql】第 4 步）。
3. 停掉 changefeed。
4. sync-diff 干净。
5. 核对 `id_segment` 水位和 D1 的 `route_meta`。
6. MySQL 设 `read_only=OFF`。
7. 用 `-Database mysql` 重新生成 ConfigMap，改回集群标记。
8. 放流量。

**R1 窗口内的纪律：**
- **冻结 schema 变更。** TiCDC 会把 DDL 同步到 MySQL。
- **禁止对生产集群做 BR 恢复。** BR 恢复不走 SQL 层，TiCDC 大概率捕获不到这些写入（推断。官方 ticdc-compatibility 页只写明 Lightning 物理导入不兼容，没提 BR）。一旦做了，就视为 R1 失效，转入 R2。
- **反向通道保留多久**由负责人定（§9.3 #6）。TiCDC 运行期间会占住 GC safepoint，需要监控。
- TiCDC 只同步有合法索引的表：要么有主键，要么有「各列显式非空、且不含虚拟生成列」的唯一索引【文 ticdc-overview】。本仓所有表都有主键。

**为什么 R1 必须有反向通道：**
- go/db 的已落库游标、applied epoch 都在 Redis 里（决策 D5）。切到 TiDB 之后发生的写，如果没有同步回 MySQL，改回 MySQL 后 Redis 游标会认为这些写「已落库」，实际 MySQL 里没有，形成静默回档。
- `id_segment` 也一样：MySQL 的水位落后，会导致重发号码（runbook :138-148）。

**生成列：**
- MySQL 侧的 `snapshot_guid_nz` 是 STORED 生成列，TiCDC 写入时由 MySQL 自己计算。
- 选 §4 #1 的方案 B，就是在迁移前两侧都去掉这一列，没有兼容问题。
- 选方案 A 的话，回退前要验证两侧表结构兼容。

**MySQL 退役**（单独成批 P1-6，R1 窗口结束、负责人同意后执行）：
1. 删 changefeed。
2. 删 mysql 的 Deployment、PVC，以及备份 CronJob。
3. 从 infra 清单列表里去掉 `mysql.yaml`（`k8s_deploy.ps1:5740`）。
4. 去掉 `Wait-ForDeploymentReady … "mysql"`（`:4211`）。如果 P1-2 已经把它参数化，就只删 mysql 档。

退役前保留最后一次全量备份，保存多久由负责人定。

### 3.5 AGENTS §6 压测纪律在 TiDB 上怎么执行

**环境**
- 设一个专用、可以随时清空的压测环境，拓扑与 staging 相同（§2.2）。
- 原因：AGENTS §6.2 第 6 条要求每次登录压测前删光 redis / mysql / etcd，所以压测环境不能是要保留数据的 staging（§3.0）。
- AGENTS §9 只允许 AI 操作本地环境，或用户明确指定的 dev 环境。Codex 要在这个环境压测，需要负责人点名（§9.3 #13）。

**基线**
- 先在**同一个环境**用 MySQL 跑一轮基线，存下 `prev-summary.txt`；再切 TiDB，跑同一个场景。
- R18 不能直接当基线。它是单机 compose、单 zone（`stress-1zone-45k-2026-06-04-round18.md:9`），环境、拓扑、后端三样都和新环境不同，对比结果无法归因到数据库，只能作参考。

**「清空 mysql」对应的 TiDB 做法**，二选一：
- DROP DATABASE 全部应用库（不碰系统库），然后重跑幂等初始化 Job、全部 migrate，以及 §5.2 的预切。TidbInitializer 只在首次建集群时运行，之后不需要再设 root 口令。
- 删掉 TidbCluster 重建。Retain 策略下 PV 还要另外删，所以压测环境用 Delete 回收策略。

Redis `FLUSHALL`、etcd、Kafka offset reset 照旧执行。AGENTS §6.2 第 6 条里「mysql」一词的 TiDB 口径要改，由负责人修改（§9.5）。

**TRUNCATE 的坑（推断）**
- `currency_crash_window.ps1` 和 `stress_round19.ps1` 用 TRUNCATE 清表。
- TiDB 执行 TRUNCATE 会换新的表 ID，之前手工预切的索引 Region 和 table attributes 不会跟到新表上。
- 所以清表之后要重跑预切脚本，P1-3 把预切做成脚本。
- （2026-10-12 G1a）D1 路由表 TRUNCATE 之后重跑 `route_tool -f <yaml> tidb-prepare -zones=<全部 zone> -dry-run=false`（§5.2 第 7 条）。两份清库脚本只连本机 MySQL 容器，注释里已写明 TiDB 环境按本节做。

**备份**：压测环境不开日志备份，或者压测前停掉，免得把压测产生的垃圾数据备份进去。

**采集**
- `stress_snap.ps1` 目前只抓 9101、9150、9160 三个端口（`:8-10`）。P1-3 要给它和 `stress_summarize.ps1` 加 TiDB 段：
  - tidb-server 状态端口 10080（`docker-compose.tidb.yml:51`）只有 tidb-server 自己的指标；热点数据要从 PD 的 hotspot API 抓。
  - K8s 上 port-forward 只能连到单个 Pod。多副本时要逐个 Pod 抓，或者从 Prometheus 拉，方式在 P1-3 定。
- AGENTS §6.1 要求复盘只读脚本输出。所以在 P1-3 完成之前，Key Visualizer 只能作为截图附件，不能当表格数据用。

---

## 4. 必须先解决的兼容阻塞

| # | 阻塞 | 出处 | 不处理的后果 | 建议解法 | 状态 |
|---|---|---|---|---|---|
| 1 | `player_snapshot` 的可空唯一键 `uk_snapshot_guid_nz` 在 TiDB 上建不出 | `schema.go:803-825`；`snapshot_store.go:119`（ODKU）、`:132`（NOT EXISTS 退路）、`:215-225` | migrate Job 失败。AutoMigrate 路径把**四个** store（快照、流水、号段、名字）都置为 nil（`svc/servicecontext.go:157-221`），**建角全停**。**没有放行通道**：`AllowMissingGuidUniqueKey` 没接到 yaml，只有零值一条路（`schema.go:1242-1249`），要放行本身就得改代码。还有一条静默路径，见下文 | 见下文 #1 | **待拍板，阻塞** |
| 2 | `GET_LOCK`、迁移会话设置、旁路 KILL | 见下文 #2 | `SET SESSION lock_wait_timeout` 如果被拒，go/db `cmd/migrate` 和三个 schemamigrate 在建会话时就失败，§3.2 第 5 步整体走不下去 | 先实测（P1-0 第一项），按结果再定是否加方言分支 | **可能阻塞，待实测** |
| 3 | friend 推荐查询的计划 | `friend-handoff-20260920.md:1241` | 统计信息低估时，查询退化成整表扫描 | 见下文 #3，(c) 和 (a) 至少落一个 | 待选型并实测 |
| 4 | `player_name` 的主键迁移 | `schema.go:352-366`（TiDB 上跳过「主键改为 name_norm」）；目标形态 `:143-151` | 存量旧形态的表在 TiDB 上永远不会被改成新形态 | 见下文 #4 | 解法明确 |
| 5 | 大快照：Kafka 消息上限与 TiDB 单行上限 | 见下文 #5 | 现状下，大于约 1MB 的快照在 C++ 发送端就失败，根本到不了 TiDB。将来调高 Kafka 上限后，如果 TiDB 仍是 6MB，一条大快照会卡住它所在的分区 | §2.5 的两项配置；Kafka 上限另立项（L-07） | 配置已在 dev |
| 6 | proto2mysql 的 Save 生成什么 SQL | 见下文 #6 | 决定存盘会不会写到主键索引的尾部 | 见下文 #6 | 有新证据，仍需看源码确认 |
| 7 | `AUTO_INCREMENT` 选项 | 见下文 #7 | 见下文 | 见下文 | **待拍板，必须在 TiDB 首次建表前定** |
| 8 | 聚簇 / 非聚簇的默认值 | 见下文 #8 | 没写选项的表在 TiDB 上是聚簇主键 | 见下文 | 不阻塞，须写明 |
| 9 | CHECK 默认不生效 | `schema.go:66`；D1 提案 :84-88 | `id_segment` 上限和 D1 映射约束在 TiDB 上只剩应用层一道防线 | §2.5：建表前把 `tidb_enable_check_constraint` 设为 ON；验收时插入越界值应被拒 | 解法明确 |
| 10 | 库级和表级默认排序规则 | 见下文 #10 | 只写了 CHARSET 的表，两边的排序规则**一定**不同 | 见下文 | 解法明确 |
| 11 | Dumpling 导出的 MySQL 表结构不含 TiDB 选项 | 推断：MySQL 只识别 `/*! */` 版本注释，`/*T! … */` 当普通注释丢弃，所以 MySQL 的 `SHOW CREATE TABLE` 里没有 `NONCLUSTERED` / `SHARD_ROW_ID_BITS` | 如果让 Lightning 按 Dumpling 的表结构建表，TiDB 上的表会全部变成聚簇单调主键，§D3 的处方整体失效 | Dumpling `--no-schemas` 只导数据【文】；表结构由官方迁移工具预建（§3.2 第 5 步） | 解法明确 |
| 12 | affected rows 与 ODKU 的语义 | 见下文 #12 | 多处业务判断依赖「ODKU 返回 0/1/2、无变化的 UPDATE 返回 0」，一旦错位会**静默**出错，例如多发奖励、撞名被当成插入成功 | 每处补一条 TiDB 用例（§9.1 #14） | 未实测 |
| 13 | login 口令认证的门禁脚本是 MySQL 存储过程 | `go/login/model/migrations/20260803_password_auth.sql:7-9`（`DELIMITER` + `CREATE PROCEDURE` + `SIGNAL`） | TiDB 不支持存储过程（官方 mysql-compatibility 页写明；本次没有重新抓取该页），脚本在 TiDB 上跑不了 | 迁库前先在 MySQL 上跑完；或改写成应用侧检查（§9.3 #17） | 待拍板，只在开启 PasswordAuth 时阻塞 |
| 14 | 预建表时，迁移工具会写入行 | `id_segment_store.go:253-265`；`go/db/internal/migrate/runner.go:324-333`；`go/schemamigrate/runner.go:33-44`；`gateway_tables.sql:37-39`；D1 提案 :299 | 第 12 步导入必然撞主键；若改用 `ignore` 绕过，号段会倒回 | §3.2 第 7 步 | 解法明确 |

**#1 解法（负责人从 A1 / A2 / B 中选一个）**

- **还有一条静默路径。** 只有迁移路径是 fail-closed 的。`NewSnapshotStore` 遇到缺键的表时，只打一条 Error，然后退回 NOT EXISTS 去重（`snapshot_store.go:215-225`）。在 TiDB 上，这条退路依赖的间隙锁并不存在，等于去重静默失效。
  - 一旦有人在 TiDB 上跑过一次失败的 `data_service -migrate`，表就已经建好了，只是缺这个键。之后再起一个 `AutoMigrate=false` 的 data_service，就会走进这条路径。
  - 所以 P1-1 要同时做两件事：一是 TiDB 上缺这个键时 `NewSnapshotStore` 拒绝装配；二是在 P1-1 合入之前，禁止在 TiDB 上起 data_service。
- **方案 A：方言分叉，改动小。** MySQL 保持 `STORED` + `NULLIF`。TiDB 上换一种写法，表达式都用 `CASE WHEN snapshot_guid=0 THEN NULL ELSE snapshot_guid END`，再加 UNIQUE。有两个变体：
  - **A1**：建表时就在 `CREATE TABLE` 里写 STORED 生成列。官方只禁止「用 ALTER 添加 STORED 生成列」，`schema.go:824` 的报错文案也把它列为候选；
  - **A2**：改用 `VIRTUAL` 生成列。
  - 依据：TiDB 生成列不支持 `NULLIF()`；虚拟和存储生成列上都能建索引【文 generated-columns】。
  - 生成列上能否建**唯一**索引，文档没写【未核实】。Dumpling / Lightning 怎么处理生成列【未核实】。
  - 代价：`ensureNullableUniqueKey` 要加 TiDB 分支；`hasGeneratedColumn` 要按方言比较表达式；两种方言各维护一份 DDL。
- **方案 B（推荐）：去掉「可空」，两种方言共用一份 DDL。**
  - GM 和 data_service 写快照时，也从 `snapshot` 号段领 guid。C++ 已经这么做了（`cpp/libs/modules/snapshot/snapshot_system.cpp:53-71`，`GuidKind::kSnapshot`），data_service 的号段种子清单里也有 `snapshot`（`k8s_deploy.ps1:3533`）。
  - 回填存量中 `snapshot_guid=0` 的行，然后建普通的 `UNIQUE KEY(snapshot_guid)`，配合 ODKU 去重。
  - 这与 Java 版一致：Java 的「去重键就是主键快照号、ODKU 幂等」（Java `PARITY.md:69`）。
  - 好处：业务知识只有一个权威来源（AGENTS §11.2），对 TiCDC 和 Dumpling 也友好。
  - 代价：GM 快照写入路径要改；MySQL 侧要按 expand / contract 迁移（§9.2 P1-1）；要补测试。
- **更正 v0.1 的一处误读。** v0.1 说 `data-service.yaml:26-27`「多副本的前提就是这个键」，这是误读。实际上：
  - 清单 :26 只是把这个键列为「不再需要单副本」的理由之一；
  - 多副本的两个服务侧前提写在 :31-32：迁移锁和 `Store.Required`。

**#2 细节**

- **TiDB v8.5 对锁函数的支持**【文 locking-functions】：
  - 支持 `GET_LOCK`、`RELEASE_LOCK`、`IS_FREE_LOCK`、`IS_USED_LOCK`、`RELEASE_ALL_LOCKS`；
  - 超时被限制在 [1, 3600] 秒，锁名不超过 64 字符；
  - 锁对集群里所有 tidb-server 都生效；
  - **不自动检测用户锁之间的死锁**。
  - 文档**没写**两件事：连接断开或 tidb-server 宕机时锁会不会释放。而代码依赖「断开即释放」（`go/schemamigrate/runner.go:112`）。
- **仓库里的 GET_LOCK 调用点**：
  - data_service：`schema.go:1391`，等待下限 1 秒（`:1364-1378`）；
  - `go/schemamigrate/runner.go:148`；
  - go/db 迁移：`internal/migrate/runner.go:295`；
  - guild 启动期：`go/guild/guild.go:58`、`:660`；
  - guild 业务路径：`guild_manage_repo.go:911`，等待 5 秒（`:855`）。
  - 锁名形如 `mmorpg_db_migrate:<库名>`，在 64 字符以内（推算）。
- **`lock_wait_timeout`**：
  - 两个迁移器都用一条语句同时设两个变量：`SET SESSION lock_wait_timeout=…, innodb_lock_wait_timeout=…`（`go/db/internal/migrate/runner.go:264-270`、`go/schemamigrate/runner.go:98-104`）。
  - v8.5 系统变量页里没有 `lock_wait_timeout` 这个条目（本次分四段读完了全页），`innodb_lock_wait_timeout` 有。
  - TiDB 如果不认这个变量，会报 1193，两个迁移器都在建会话时失败。如果认、但不生效，「DDL 撞上长事务时快速失败」就不成立：TiDB 的元数据锁会一直等旧事务结束，只能靠语句硬超时加 KILL 兜底。
  - 结论：**放进 P1-0 第一项实测。**
- **旁路 KILL**：
  - go/db（`runner.go:496-503`）和 schemamigrate（`runner.go:310-318`）都会在超时后，从另一条连接发 `KILL QUERY`。
  - 官方文档：Global Kill 自 v6.1 起默认开启，经过代理也会转发到目标实例；自 v6.2 起 KILL 可以终止进行中的 DDL job【文 sql-statement-kill】。
  - go/db 用 uint64 读取 `CONNECTION_ID()`（`runner.go:252-256`），装得下 64 位连接 ID。
  - v0.1 写的「跨 tidb-server 可能杀不到、要用 `ADMIN CANCEL DDL JOBS` 兜底」与文档相反，已删除。仍然保留实测：经过 LB 的跨实例 KILL 能终止语句和 DDL。

**#3 细节**

- 2026-10-09 在 TiDB v8.5.2 上实测：统计信息新鲜时，两条推荐查询读的行数不随玩家总数增长；用 `LOCK STATS` 模拟统计信息低估后，会退化为整表扫描。
- 已实测有效的钉法：每个排除子查询加 `QB_NAME`，外层加 `INL_JOIN` 提示。代价是 MySQL 上每条语句多 1–2 条 1064 警告。
- 三种做法：
  - (a) 直接在 SQL 里加提示；
  - (b) 按方言选两份 SQL 文本；
  - (c) TiDB 全局绑定（`CREATE GLOBAL BINDING`），代码不动、MySQL 不受影响，但没测过。
- **(c) 和 (a) 至少要落一个，不能当成可选项。** TiDB 自动 ANALYZE 默认在修改行占比达到 0.5 时触发（`tidb_auto_analyze_ratio`【文 system-variables】），统计信息可能比 InnoDB 的更旧（推断）。建议先测 (c)，不行再退到 (a)。不管选哪种，导入后都必须 ANALYZE（§3.3）。

**#4 细节**

- 新建库：bootstrap DDL 直接建出目标形态，即 `PRIMARY KEY(name_norm) NONCLUSTERED` + `UNIQUE(player_id)`，不需要迁移。
- 有存量：按 §3.2 第 5 步在 TiDB 上预建表、只导数据即可；也可以迁移前先在 MySQL 上跑一遍 `data_service -migrate`，让源库先变成目标形态。导入后，`bootstrapUniqueColumns` 的守卫会核对两条唯一性是否都在。
- 可选优化（不阻塞）：TiDB 上把 `name_norm` 改成聚簇主键，能省一次回表。名字是随机分布的，没有单调尾部问题。但聚簇表不能再用 `SHARD_ROW_ID_BITS`（`schema.go:352-366` 的注释），要写成 TiDB 专用形态。

**#5 细节**

- **现状：快照先被 Kafka 的约 1MB 默认上限挡住。**
  - 全仓没有配置消息大小上限，只在 `go/shared/kafkautil/topic_init.go:191` 的注释里提到过。
  - C++ 生产者只设了 bootstrap、dr_cb、sparse、幂等四项（`cpp/libs/engine/infra/messaging/kafka/kafka_producer.cpp:59-73`），broker 清单也没设。
  - 按 librdkafka 和 Kafka broker 的默认值，上限都在 1MB 左右（推断，本仓没有实测）。
  - 超过上限的快照会在 `snapshot_system.cpp:101-112` 发送失败，只打一条 LOG_ERROR，审计静默缺档。
  - 消费端配了 `MaxBytes: 16<<20`（`go/data_service/internal/kafka/consumer.go:133`），说明设计意图是支持 16MB。
  - 这是一个**与 Phase 1 无关的既有问题**，放进 L-07。
- **TiDB 侧。**
  - 消费者的单条上限是 16MiB−1（`snapshot_consumer.go:24`）。DB 写入重试用完后，**整个消费者停住、不提交 offset**（`:97-103`，即「lag 优先于丢失」）。
  - 将来调高 Kafka 上限后，TiDB 如果还是默认的 6MB，一条大快照就会卡住它所在的分区（全球共 3 个分区）。
  - 所以 §2.5 的两项 32MB 配置要保留，作为纵深防御。它们同时防住 GM 直写路径和 Lightning 导入存量大行。
- **go/db 存盘路径不受影响。** 它有大字段闸：默认每列 256KiB、每行 1MiB（`go/db/internal/config/config.go:245-246`），K8s 没有覆盖，超限就拒写。默认的 6MB 对它够用。
- **验收。** §9.1 #13 改成两条：
  - 对 store 直写一条 16MiB−1 的快照做冒烟；
  - 经 Kafka 的大快照，只在 L-07 调高消息上限之后作为单独用例。调高 Kafka 消息上限不违反 D5，D5 只禁止改 topic 和分区。

**#6 细节**

- **仓库内说法矛盾**：`go/db/internal/logic/pkg/proto_sql/store_registry.go:294` 的注释写 `REPLACE INTO`；`go/db/internal/migrate/plan.go:177/249/299` 等处写的是 ODKU。
- **新证据**：Java 版 `PbMysql` 自称「语义逐项对齐 Go v0.2.0」，注释里写了两点：
  - Go 版 `Save / InsertOnDupUpdate` 的流程是：先按主键 UPDATE 全部非主键列；没命中再 INSERT；撞 1062 再 UPDATE；仍为 0 就 `SELECT … FOR UPDATE` 核对（Java `game-pbmysql/src/main/java/com/game/pbmysql/PbMysql.java:187-196`）。
  - `GetSaveSQLWithArgs` 是带主键守卫的 ODKU（`:176-179`）。go/db 调用的是 `store.Save(msg)`（`key_ordered_consumer.go:829`）。
- **如果属实：**
  - 存盘既不是 REPLACE，也不是 ODKU。稳态存盘是按主键 UPDATE，在非聚簇表上只改已经打散的行 KV；主键索引的尾部只在首次 INSERT 时才写到。所以 launch_verdict 里「REPLACE 让每次存盘都改写主键索引」的最坏情况**不成立**。
  - 但有一个副作用（推断）：go-sql-driver 默认按「实际改动行数」计 affected rows。值完全相同的存盘会走满四条语句（UPDATE → INSERT(1062) → UPDATE → SELECT FOR UPDATE），在 TiDB 上每条都多一次取 TSO 和提交的往返。
- **仍需 Codex 确认**：读 Go 源码（`go mod download github.com/luyuan-cpp/proto2mysql@v0.2.0`），然后更正 `store_registry.go:294` 的注释。affected rows 口径一并纳入 #12。

**#7 细节**

- 三张主表把 `player_id` 声明为 `OptionAutoIncrementKey`（`mysql_database_table.proto:88`、`:103`、`:161`）。
- Java 镜像把这个选项渲染成 `… AUTO_INCREMENT`（Java `TableSchema.java:241-244`），并自称与 Go 的 `GetCreateTableSQL` 逐字节相同。所以 Go 版大概率也这样渲染，但本次没读 Go 源码。
- `player_id` 永远由 login 号段显式铸出，AUTO_INCREMENT 在这三张表上没有用处。在 TiDB 上有三个问题：
  - 官方不建议显式写入与隐式分配混用【文 auto-increment】；
  - `SHARD_ROW_ID_BITS=4` 时，行 ID 只剩 59 位的自增空间，用尽后报 `Failed to read auto-increment value from storage engine`【文 shard-row-id-bits】；
  - AUTO_INCREMENT 是否与 `_tidb_rowid` 共用分配器、显式写入的存量大号会不会推高分配器，文档都没写【未核实】。就算共用、又被存量约 2.8e17（`client.go:43`）推高，2^59（约 5.76e17）也还有约 2 倍余量，用尽不是近期风险（推算）。
- TiDB 不能给已有的列追加 AUTO_INCREMENT（Java `SchemaSync.java:151` 的注释）。
- **建议**：在 TiDB 首次建表之前，去掉三张主表的 `OptionAutoIncrementKey`。
  - MySQL 侧配一条 `MODIFY` 迁移。对旧二进制没有影响，因为 `player_id` 本来就显式写入。
  - proto2mysql 的漂移校验会把 auto_increment 不一致判为漂移（Java 镜像 `SchemaPlanner.java:301`），所以要和 proto 改动同批发布。
  - `player_snapshot` / `rollback_audit_log` 的自增 id 保留。它们非连续、有跳号，没有影响（决策文档 §D3 :109）。

**#8 细节**

- `tidb_enable_clustered_index` 默认 ON【文】。
- 没写 TiDB 选项的表：
  - zone 库：`user`、`user_oauth`、`user_phone`、`user_password`、`user_accounts`、`account_share_database`；
  - proto 里的 `player_debt`、`rollback_audit`（data_service 的 `TableMessages` 不建它们，`schema.go:1197-1204`）；
  - gateway 的三张表（`gateway_tables.sql:6-35`）。
- 这些表在 TiDB 上都会建成聚簇主键。它们要么不是单调键，要么不在首日写路径上，所以**不阻塞**。
- 但必须写明：**禁止用手写 SQL 副本在 TiDB 上预建表。** `go/db/model/mysql_database_table.sql:12-50`，以及 login、player_locator 的两份同类副本，都没有 TiDB 注释，用它们建出来的就是聚簇单调主键。

**#10 细节**

- **建库。** 这两处的 `CREATE DATABASE` 不带排序规则：`k8s_deploy.ps1:5516` 生成的，以及 `00_init_zone_dbs.sql:5/9/22`。
  - MySQL 上会继承 server 的 `utf8mb4_unicode_ci`（`mysql.yaml:71`）。
  - TiDB 上默认是 `utf8mb4_bin`【文 character-set-and-collation】。
  - trade、guild、friend 三个库的初始化 SQL 已显式写了排序规则（`00_init_zone_dbs.sql:36/47/68`）。
- **只写 CHARSET 的表：两边确定不同。**
  - 涉及的表：`gateway_tables.sql` 的三张；D1 的 `zone_merge_fence`、`zone_merged_into`、`route_meta`（提案 :105、:113、:124）。
  - MySQL 8.0 规定，只写 CHARSET 时用该字符集的默认排序规则，即 `utf8mb4_0900_ai_ci`，与库的排序规则无关（推断，按 MySQL 规则，P1-0 用 `SHOW CREATE TABLE` 核实）。
  - TiDB 默认是 `utf8mb4_bin`。所以两边**确定不同**，不是「可能不同」。
- proto2mysql 建的表都显式写了 COLLATE，不受影响。
- **处理：**
  - TiDB 初始化建库时，显式写 `utf8mb4_unicode_ci`。
  - gateway 表和 D1 三张表补上 COLLATE，取值与 MySQL 现状一致，即 `utf8mb4_0900_ai_ci`（D1 可以另定）。
  - TiDB 全局设 `default_collation_for_utf8mb4=utf8mb4_0900_ai_ci`，作为兜底（§2.5）。
  - TiDB v8.5 支持 `utf8mb4_unicode_ci`、`utf8mb4_0900_ai_ci`、`utf8mb4_0900_bin`【文 character-set-and-collation】。

**#12 细节（新增）**

下列判断依赖 `ClientFoundRows=false`：ODKU 返回 0（无变化）/ 1（新插入）/ 2（已更新），无变化的 UPDATE 返回 0。在 TiDB 上都还没实测。

| 位置 | 依赖 | 错位的后果 |
|---|---|---|
| `go/guild/internal/data/economy_repo.go:90` | 计数 upsert 的 1 = 新插入、2 = 已累加、0 = 达上限 | 多发奖励 |
| `go/guild/internal/data/guild_manage_repo.go:335-336` | `RowsAffected==1` 写入自检 | 写丢了也看不见 |
| `go/data_service/internal/store/player_name_store.go:355-388` | no-op ODKU 回 0 行，以及表上有两个唯一键时 ODKU 更新哪一行（注释 :388 自己写着「迁 TiDB 时要回归」） | 撞名被当成插入成功，名字归属错 |
| D1 W1（提案 :163） | 「影响 0 行之后再加锁读」 | 映射判定错 |
| proto2mysql Save（#6） | UPDATE 先行路径 | 存盘多走语句，或误判 |

**#13 细节（新增）**

- `PasswordAuth` 默认关闭，K8s 上也没开（§2.7）。所以这一项**只在正式服启用口令登录时**阻塞。
- 门禁脚本会对 `user_accounts` 做空值、重复、长度检查，然后才执行 ALTER。
- 两种出路：
  - 迁库前先在 MySQL 上跑完。迁移后的 TiDB 表就已经是目标形态；
  - 改写成应用侧检查（`password_admin` 或一次性 Go 工具）。

---

## 5. 写热点处理

### 5.1 现状

| 表 | 单调键 / 索引 | 现有处方 | Phase 1 尾部数 | Phase 2 后 |
|---|---|---|---|---|
| `player_database`、`player_database_1`、`player_centre_database` | `PK(player_id)`，号段单调 | `NONCLUSTERED` + `SHARD 4` + `PRE_SPLIT 4`，只打散了行数据 | Z 条（每个 zone 库一条） | **1 条** |
| `player_name`（全局库） | `uk_player_name_owner(player_id)` 单调；`PRIMARY(name_norm)` 随机 | 同上 | 1 条 | 1 条 |
| `transaction_log`（全局库） | `PK tx_id`；4 条「X + timestamp_sec」联合索引（`rollback_database_table.proto:19`），其中 `(tx_type, ts)` 是低基数前缀加单调时间 | 同上 | 少数几条 | 同左 |
| `player_snapshot`（全局库） | 自增 `PK id`（每台 tidb-server 缓存一批 id，各自形成一条尾部）；`player_id`、`snapshot_guid` 单调；`(zone_id, created_at)` 有 Z 条尾部（`:68`） | 同上 | 若干条 | 同左 |
| D1 `player_route`（全局库） | 4 条索引都有单调尾部：<br>• PK 1 条；<br>• `idx_route_home(home_zone, player_id)` Z 条；<br>• `idx_route_storage(storage_id, player_id)` 1 条，新号的 storage 相同或同为 NULL；<br>• `idx_route_updated(updated_at_ms)` 1 条，每次更新都改写到尾部<br>**（2026-10-12 G1a）**改为聚簇 `(route_shard, player_id)`，`route_shard = player_id % 16`，三条二级索引都带片前缀：PK / `idx_route_updated` 各 16 尾，`idx_route_home` 每 zone 16 尾，`idx_route_storage` 每个落点值 16 尾<br>**（2026-10-14 更正）**行片已改为 64（`player_id % 64`），各条尾数随之为 64；`tidb-prepare` 对 PK 与 `idx_route_updated` 每片一段，对 `idx_route_home` / `idx_route_storage` 每个前缀值切 8 个点，见 D1 提案 §11.2「行片 16 → 64」 | NONCLUSTERED + SHARD（提案 :80、:90）。**G1a 起**：聚簇 + 片前缀 + `route_tool tidb-prepare` 预切与 `merge_option=deny`，预切后分散 | 1+Z+1+1（G1a 后为 16·(3+Z) 条，分散在预切出的 Region 上；`idx_route_storage` 的 NULL 段 16 条尾部在一个 Region，见提案 G1a HR5） | 同左 |
| trade、guild、friend | 号段单调主键：`trade_listing.listing_id`（`trade_table.proto:30`）、`trade_asset_op.op_id`（`:108`）、`guild_asset_op.op_id`（`guild_db.proto:143`）。时间类索引：`(state, created_ms)`（`:272`）、`guild_trial_reward_owed(created_ms)`（`:302`）、friend 的 `(friend_count, created_ms)`（`friend_table.proto:108`） | 已声明 NONCLUSTERED + SHARD + PRE_SPLIT（评审已核对） | 写入频率低，不阻塞 | 同左 |
| 单行热点 | `id_segment` 行（每次领段做一次 `FOR UPDATE`）；D1 的 `zone_merge_fence`（每 zone N=8 行，每次建角锁其中一行）；`route_meta` 单行。**（2026-10-12 G1a）**`zone_merge_fence` 按 zone 预切（每 zone 一个 Region）；`route_meta` 读多写极少，可选设缓存表；常写的 leader / 水位在 `route_reconcile`（只有 1 个写者）<br>**（2026-10-15 更正，原文不删）**① 「每 zone N=8 行」已过时：围栏分片数 `RouteStore.FenceShards` 按容量模型 §4.1「连带修正」取 **MySQL 档 16、TiDB 档 64**（D1 提案 §11.2「行片 16 → 64」的「改了什么」与「连带修正」，:908、:930）。它只是配置、任何时候可改（上限 64，围栏持有者总是锁满 0..63 全部分片行）；MySQL 档的 16 已写进 `go/data_service/etc/data_service.yaml`，代码默认仍是 8。TiDB 档 64 目前**没有配置载体**（仓库里没有 TiDB 档的服务 yaml；`k8s_deploy.ps1` 的 data-service ConfigMap 不镜像这个键，K8s 上取默认 8），切 db_first 之前按档位补进 ConfigMap 并加契约用例，见 D1 提案 §11.2「正向探针 P5」段末的「2026-10-15 补登」。② 「可选设缓存表」已改定：`ALTER TABLE route_meta CACHE` **S2 档起必选，S1 可选**（D1 提案 §11.2「TiDB 运维步骤」第 6 条 :836、「连带修正」:931、§12 梯子第 2 级 :1093；容量模型 §4.1「连带修正」、§2.6 route_meta R1 单行读一行），见 §9.3 第 21 项的更正注 | — | TiDB 扩不了 | 同左 |

- 号段发号只打散了行数据。仓库里没有任何 `SPLIT TABLE … INDEX`，也没有对 id 做打散（决策文档 §D3 更正第 2 条）。
- launch_verdict 里「player_route 4 条索引（其中 3 条单调）」的说法要更正：漏算了 `idx_route_home`，4 条都单调。（2026-10-12 G1a 更正：D1 路由表已在建表前改为聚簇 + 16 片前缀，四条索引都不再是单尾；前提是 TiDB 上跑过 `route_tool tidb-prepare`，见 §5.2 第 7 条与 D1 提案 §11.2「第 1a 批热点修复」。2026-10-14 起行片为 64，见同节「行片 16 → 64」。）
- 主表的首行由 go/db 经 Kafka 异步写入，所以尾部热点首先表现为 Kafka lag，不会直接导致建角失败。`player_name` 和 D1 的事务在建角的同步路径上，热点会直接拉高建角 p99。

### 5.2 Phase 1 必做

1. **建表前把 `tidb_scatter_region` 设为 `global`**【文 system-variables、split-region】。它在初始化 Job 里以 GLOBAL 设置。
2. **对随机分布的索引做预切。**
   - 重点是 `player_name` 的 `PRIMARY(name_norm)`：用 `SPLIT TABLE player_name INDEX ... BY (...)` 按 UTF-8 字节区间切。
   - 中文名的首字节集中在 0xE4–0xE9，具体切分点按名字规则包的实际分布定。
   - 字符串上界的写法参照官方例子，用 `{` 而不是 `z`【文 split-region】。
   - 对非聚簇主键要写成 ``INDEX `PRIMARY` ``，这一语法【未核实】。
3. **防止 PD 把预切的 Region 合并回去**：执行 `ALTER TABLE … ATTRIBUTES 'merge_option=deny'`。BR 和 TiCDC 都**会跳过**这条 DDL，所以恢复或同步之后要手动重放【文 table-attributes】。
4. **号段开服档**：login 的 `IdSegment.Step` 直接配成 1000（现为 100，`login.yaml:186`），只改配置。代价是每次重启最多浪费 2×step 个号，可以忽略。
5. **写进脚本和 runbook**：新建表、TRUNCATE 之后、BR / PITR 恢复之后，都要重跑第 2、3 步（§3.5、§6）。（2026-10-12 G1a）D1 路由表的第 2、3 步就是第 7 条的 `route_tool tidb-prepare`，同样在这些时机重跑。
6. **验证**：在 TiDB 压测环境里，用 PD 的热点数据和 Key Visualizer 区分 index 流量与 record 流量。在 `stress_snap` 支持 TiDB 之前（§3.5），这部分只作为附件。
7. **（2026-10-12 G1a）D1 路由表：`route_tool -f <yaml> tidb-prepare -zones=<全部 zone> [-storages=1000000] -dry-run=false`**（D1 提案 §11.2「第 1a 批热点修复」）。
   - 做什么：判引擎（MySQL 打印 `SKIP`、退出 0）→ 核对 `player_route`、`zone_merge_fence` 的 `TIDB_PK_TYPE=CLUSTERED`（不是就退出 2，一条都不发）→ 两表 `ATTRIBUTES 'merge_option=deny'` → `SPLIT TABLE player_route BY (1,0)…(15,0)`、`INDEX idx_route_updated BY (1,0,0)…(15,0,0)`、每个 zone 一条 `INDEX idx_route_home BY (z,0,0)…(z,15,0)`、每个 `-storages` 值一条 `INDEX idx_route_storage`、`SPLIT TABLE zone_merge_fence BY (z1,0),(z2,0),…`。
   - （2026-10-15 更正，上一条原文不删）上一条的切点是行片 16 时的写法。2026-10-13 起行片为 64（`placement.RouteRowShards`）：`player_route` 行数据与 `idx_route_updated` 切片 1..63 的起点（各 63 个点，每片一段）；`idx_route_home` / `idx_route_storage` 每个 zone / 落点值只切 8 个点（片 0、8、…、56，每段 8 片）；`zone_merge_fence` 仍是每 zone 一个点。见 D1 提案 §11.2「行片 16 → 64」的「预切密度」。`tidb-prepare` 仍不发 `ALTER TABLE route_meta CACHE`，S2 档起要另行执行（§9.3 第 21 项更正注）。
   - 默认 `-dry-run=true` 只列语句；全部语句幂等，中途失败重跑即可。只碰全局库，不连 mapping Redis；会经过 route store 的建池探针（含 `chk_route_shard` 实测），CHECK 不生效的库直接退出 2。
   - 何时跑：Phase 1 预建表之后（§3.2 第 6 步）；开新 zone 时（见下）；BR / PITR 恢复之后；TRUNCATE 之后（§3.5）。
   - 权限：SPLIT / ATTRIBUTES 需要的权限【未核实】，应用账号不够时用运维账号的 yaml 副本跑（副本不进 git）。
   - K8s 上要等 D1 第 1b 批把 route_tool 并进 data_service 主程序入口；TiDB Phase 1 本来就排在 D1 第 1 批之后（§0）。

**（2026-10-12 G1a）开新 zone 的 TiDB 步骤**

1. 在该 zone 对外开放（第一次建角）**之前**、低峰跑 `route_tool -f <yaml> tidb-prepare -zones=<新 zone> -dry-run=false`（也可以带上全部 zone，幂等）。两条 `ALTER … ATTRIBUTES` 是 DDL，在线重跑可能让在途建角以 8028 失败一次（提案 G1a HR9，未核实）。
2. 核对 `SHOW TABLE zone_merge_fence REGIONS`、`SHOW TABLE player_route INDEX idx_route_home REGIONS` 里出现以新 zone 开头的 Region。
3. MySQL 档跳过（工具打印 `SKIP`、退出 0）。
4. 第 1b 批之前 K8s 上 route_tool 不在镜像里；TiDB Phase 1 本来就排在 D1 第 1 批之后。

### 5.3 需要拍板的

| 选项 | 做法 | 写入点数 | 代价与风险 |
|---|---|---|---|
| 0. Phase 1 不动发号 | 保持现状。Phase 1 是 Z 条尾部，再加 Kafka 削峰 | Z | 打开 Phase 2 的 `PinOnCreate` 之前**必须**重新评估 |
| 1. 发号打散 | 在 2^55 值域内做双射置换，再预切索引 | 真正分散 | 新号会更早进入 [2^22, 2^55)，可能与 2024-10-27 之前铸出的存量 snowflake `player_id` 撞号（`client.go:36-51`）。**撞号不报错，会静默串档**。`player_id` 的分布对客户端可见，按 AGENTS §12 要同批评估 Java |
| 2. 大号段 + 段边界预切 | 加大每个 login 的段长，在段边界预切 | 等于 login 实例数 | 改动小，上限低；撞号风险同上 |

- **建议**：Phase 1 选 0。在 TiDB 压测环境量化之后，于 Phase 2 之前在 1 和 2 之间定案。
- **不管选哪个**，动手前都要先在所有环境审计 `player_id < 2^55` 的存量行，或者由负责人拍板收紧 `MaxIDExclusive`（`client.go:48-51` 已把这个决定留给负责人）。
- **只预切、不打散，对单调键无效。**

---

## 6. 备份与恢复（TiDB 版 runbook 要点）

### 6.1 现状与差距

**现状：**
- 清单 `mysql-backup-cronjob.yaml` 写的是：每天 03:17 UTC 做一次 `mysqldump --all-databases`，并把 binlog 复制到 RWX PVC（`:39`、`:81-88`、`:98-102`）。dump 保留 30 天，binlog 保留 14 天（`:106-107`）。
- **这份清单不在部署脚本里**：infra 清单列表（`k8s_deploy.ps1:5740`）不含它，全脚本 grep 也是 0 命中，只能按 runbook §2.1 手工 apply（`mysql-backup-pitr-runbook.md:28`）。**各环境是否真在跑，未核实。**
- 没有异地归档（runbook §7 :205）。
- PITR 的做法是在影子 Pod 里手工跑 `mysqlbinlog`（runbook §4.1）。
- 同类问题：`redis-ha.yaml:3` 声称 `k8s_deploy.ps1` 会按 `-RedisReplicas 3` apply 它，但脚本里没有这个参数（grep 0 命中）。这一条与 L-06 相关，列入 §9.5。
  - **（2026-10-19 更正，上一条原文保留）已接入**：`k8s_deploy.ps1 -RedisReplicas`（0 / 1 / 3，0 = 按档位，staging / prod 档缺省 3）取 3 时 apply `redis-ha.yaml` 代替 `redis.yaml`，`redis-ha.yaml` 文件头已改写为接入说明。origin `43284eb0b`（10-16）接入、本地 10-19 补齐；已落码，契约测试未运行、未在 kind 复测，待 Codex 验证。详见 L-06 同日更正。本节 MySQL 备份 CronJob 不在部署脚本里的问题不受影响。

**决策 D8**：runbook 没改完之前不许迁生产。本文把门禁从「runbook 已写」**收紧为**同时满足下面三条：
- `docs/ops/tidb-br-pitr-runbook.md` 已写；
- 目标环境里的 `BackupSchedule` 和日志备份任务**确实在运行**，checkpoint 告警正常；
- 契约测试钉住「TiDB 清单与备份 CR 确实被 apply」。

三条都满足，才是 Phase 1 上生产的**硬前置**。

### 6.2 TiDB 版 runbook 要点

| 主题 | 要点 | 依据 |
|---|---|---|
| 备份存储 | 生产用 S3 兼容对象存储，顺带补上 MySQL 版缺的异地归档。**kind 上用 MinIO（S3 兼容）**：Operator 文档没写日志备份能否存到 PV【未核实】，用 PV 的话验收 #9 在 kind 上可能跑不了 | Operator 支持 S3 / GCS / Azure Blob / PV【文 backup-restore-overview】 |
| 快照备份 | 用 `BackupSchedule` 定期做 BR 全量备份，官方例子是「每 2 天」。合服或重大变更前，手动做一次（对应 MySQL runbook §3.2、§5.1） | 【文 br-pitr-guide】 |
| 日志备份 | 常驻 log backup 任务，**每个集群同一时刻只能有一个**。`checkpoint[global]` 就是可恢复到的最新时间点 | 【文】 |
| RPO | BR 日志备份的 RPO <5min | dr-solution-introduction |
| 保留与截断 | `br log truncate` 只删早于基线快照的日志。保留期至少与 MySQL 现在的口径相同，由负责人定 | 【文】 |
| PITR 恢复 | `br restore point --full-backup-storage … --restored-ts …`。恢复期间目标表处于 restore 模式，不可读写。第一次恢复必须带上全量备份 | 【文】 |
| 按库过滤 | **v8.5.5 起** PITR 支持 `--filter 'db.*'`。限制：目标集群里不能已有匹配的库表；系统表不支持过滤 | 【文 br-pitr-manual】，这是 §2.2 钉版本的原因 |
| 与日志备份并行 | v8.5.5 起，日志备份任务运行期间默认可以做 PITR。代价是恢复区间内「可能出现数据不一致」：若受影响区间是 [t1, t2)，就不能直接恢复到这个区间内，只能恢复到 t1，或在 t2 之后重新做一次快照备份 | 【文 br-pitr-manual】 |
| 执行载体 | Operator 的 Restore CR 有 `restoreMode: pitr`，但文档没写 PITR 模式下能否带表过滤（评审轮查过 restore-from-aws-s3-using-br）【未核实】。runbook 先按「临时 Pod 跑 br 二进制（版本 ≥ v8.5.5）」写，实测 CR 可用后再换 | — |
| 性能参考 | 快照恢复约 2TiB/h/TiKV，日志恢复约 30GiB/h/TiKV，日志截断约 600GB/h | 【文】，数字来自 6–21 个 TiKV 的测试集群 |
| 影子集群 | 沿用 MySQL runbook「先恢复到影子库」的做法：另起一个 TidbCluster 做恢复，核对之后再决定是整库切换，还是只抽数据（用 Dumpling 加 `--where` 抽单个玩家的行再回灌） | runbook §4.2 |
| 恢复后必做 | ① 重跑幂等初始化 Job（全局变量，§2.6；BR 会不会恢复它们【未核实】）；② 重放 table attributes 和预切（§5.2）；③ `ANALYZE`；④ 恢复范围包含 `mmorpg_global` 时，按「核对 `id_segment.max_id`」三步做完（runbook :138-148）；⑤ D1 的 `route_meta.watermark_ms` 回退会把映射写者置为只读，按 D1 §6 反向修库。（2026-10-12 G1a）② 含 `route_tool tidb-prepare`；若 `route_meta` 设过 CACHE，一并重放；⑤ 的 `watermark_ms` 现在在 `route_reconcile`。（2026-10-18 更正，D1 第 1b-1 批，原文不删）⑤ 读起来像有自动回档检测，其实在 D1 第 1b-2 批（对账 leader）落地之前**没有**：恢复范围包含 `mmorpg_global` 且 `RouteStore.Mode=db_first` 时，要由运维按 `docs/ops/merge-zone-runbook.md` §9.4 手工 `freeze` → `reconcile-from-cache` → `audit -frozen-recovery` → `unfreeze`（`data_service -f <yaml> -route-tool <子命令>`）；② 里 `route_meta` 的 CACHE 由 Phase 1 部署步骤重放，`tidb-prepare` 不发这条语句（见 §9.3 第 21 项的 2026-10-18 更正） | 【文 table-attributes】；D1 提案 :274、:286 |
| namespace 删除 | 先删 `Backup` / `BackupSchedule` CR，再删 namespace，否则 namespace 会卡在 Terminating | 【文 backup-restore-overview】 |
| 演练与告警 | 每月一次自动恢复到影子集群，核对行数和校验和。告警项：日志备份 checkpoint 延迟、BackupSchedule 失败 | 承接 runbook §7 未做项 |

### 6.3 Phase 1 的 zone 灾难回档

改写对象：`zone_data_rollback.md` §3 和 `k8s_zone_rollback.ps1`。脚本 Step 3 打印的是 `mysqlbinlog` 命令（工作区 `:903-915`），TiDB 版改为下面 9 步：

1. **第 0 步预检**：沿用 `k8s_zone_rollback.ps1` 现有的第 0 步，并加上 `-Database` 核对（§2.8）。

2. **停入口并排空**：做法同 §3.2 第 8–9 步，只是范围缩到单个 zone。顺序是：先把 gate 和 scene 缩到 0，等本 zone 的 `db_task` 与重试队列排空，最后再执行 zone-down。
   - 现有脚本的顺序反了：Step 1 先 zone-down，Step 2 才等 lag（`k8s_zone_rollback.ps1:857-901`）。被删 zone 的 go/db 已经不在，本 zone topic 的 lag 排不掉（推断，原理同 §3.2 第 9 步）。TiDB 版顺带改正。

3. **PITR 到影子集群**：带 `--filter 'zone_N_db.*'`，br 版本 ≥ v8.5.5，执行载体见 §6.2。

4. **核对**影子集群上的恢复结果。

5. **导回生产**：在生产集群执行 `DROP DATABASE zone_N_db`，再从影子集群导回（Dumpling + Lightning 逻辑导入）。
   - 也可以把 PITR 过滤恢复直接打到生产集群：v8.5.5 起可以与运行中的日志备份并行，但要接受 §6.2 说的「恢复区间内可能不一致」，而且目标集群里不能已有匹配的库。

6. **Redis：不执行 FLUSHDB。**
   - 原因：K8s 上 FLUSHDB 会清掉**所有 zone** 的 `player:zone`、`player:placement`、`merge:*`，以及 go/db 的游标和重试收据（`zone_data_rollback.md:163-171`）。D1 第 0 批的防护本来就会拒绝这一步。
   - TiDB 版改为按 zone 范围清理：只清本 zone 的玩家缓存、`consumer:applied*` 中本 zone topic 的游标、`kafka:retry*` 中本 zone 的键。
   - 具体键清单和清理脚本还没定，是待决项（P1-4，§9.3 #18）。

7. **Kafka offset reset。**
   - K8s 上所有 zone 的 go/db 共用 `db_rpc_consumer_group`（`k8s_deploy.ps1:3437`）。重置一个 topic 的位点，要求整个组没有活跃成员（Kafka 的行为，推断），等于要停掉所有 zone 的 go/db。
   - L-09 把消费组按 zone 拆开之后，这一步才真正只影响单个 zone。在那之前，二选一：接受全服短停，或者把这项操作排在 L-09 之后。
   - **（2026-10-16 更正，上两条不删；已落码，未编译、未测试，待 Codex 验证）** L-09 已落码，见 [db-consumer-group-per-zone.md](db-consumer-group-per-zone.md) §6 与 `zone_data_rollback.md`「2026-10-15 修订」。切组之后回档只重置本 zone 组：`k8s_zone_rollback.ps1` 的 Step 5 把 `db_rpc_consumer_group_z<ZoneId>` 重置到 TargetTime（zone-down 之后这个组为 Empty 或不存在），逐分区复核通过后，再用 `-RetireGroup` 删除 legacy 组 `db_rpc_consumer_group` 在本 topic 上的位点。legacy 组即使还有别的 zone 的成员，只要没有成员订阅本 topic，broker 就允许删除（推断，待 Codex 在 Kafka 4.3.1 上实测）。所以不再需要停掉其它 zone 的 go/db。还没切组的 zone 回档时也一样：Step 6 显式 `-DbConsumerGroup zone`，落到最终形状 `GroupID: "db_rpc_consumer_group_z<ZoneId>"`，回档之后该 zone 改用 zone 组。附带：第 2 步所说「Step 2 才等 lag」的那段 Step 2 已于 2026-10-15 删除，改为停服前的只读 LAG 快照（第 0 步 c）；「先缩 gate / scene、排空、再 zone-down」的 TiDB 版顺序仍未落地。

8. **跨库孤儿对账（新增）。**
   - 按库过滤恢复只回档 `zone_N_db`。下面这些不会跟着回档，仍停在「现在」：
     - 全局库的 `player_name`、D1 的 `player_route`、`transaction_log`、`player_snapshot`；
     - `mmorpg_trade`、`mmorpg_guild`、`mmorpg_friend`；
     - mapping Redis。
   - 后果：T 时刻之后新建的角色，有名字、有归属记录，但 zone 库里没有它的数据。
   - 补救：复用 data_service 的 LoginAdmin 孤儿清理（`svc/servicecontext.go:223` 起），以及 `zone_data_rollback.md` 里「Guild/Friend 不回档」的口径。孤儿边的提醒见 `deploy/k8s/README.md:632-635`。

9. **zone-up，然后验证。**

**与 MySQL 现状比，这是一个取舍，不是单纯变好。**
- K8s 上全局库与 zone 库在同一个 MySQL 实例里，整实例 PITR 的结果在各库之间是一致的；代价是 `mmorpg_global` 跟着回档、号段水位倒回（D1 提案 :286）。
- TiDB 按库过滤恢复正好反过来：号段水位安全了，D1 的 watermark 也不会回退；代价是要做第 8 步的孤儿对账。
- D1 §6「数据库回档」一行的前提，在 TiDB 下就此改变。对全局库做 PITR 时，仍按 D1 §6 反向修库。

### 6.4 D8 仍是 Phase 2 的前置

Phase 1 的逻辑库仍按 zone 分开，用按库过滤恢复就能表达「只回档一个 zone」，没有退化。到 Phase 2 混库之后，这一点才需要应用级回档来承担（决策 §D8）。

---

## 7. 开服准备清单

排序原则：
- P0 = 不做就可能出首日事故；
- P1 = 首日大概率会撞上；
- P2 = 条件性。

「依赖 Phase 1」指这一项是否必须在 TiDB 上做或验证。数字的出处见 launch_verdict，下表只列关键行号。

| # | 级 | 做什么 | 模块 | 依赖 Phase 1 | 谁拍板 | 怎么验证 |
|---|---|---|---|---|---|---|
| L-01 | P0 | 修 login 排队的 leader 锁。锁键是全局的（`config.go:253`、`login.yaml:436`），但 `activeZones` 只读本 zone 的 GateWatcher（`servicecontext.go:152-161`、`:453-466`），结果只有 leader 所在 zone 的队列会出队。二选一：锁键带上 zone，或者让 `activeZones` 覆盖所有 zone | go/login | 否 | login 负责人 | 多 zone 单测或集成测试：每个 zone 的队列都能出队 |
| L-02 | P0 | 打开削峰：<br>• Bucket4j（`application.yaml:38` 现为 false；zone 阈值从设计稿开服档 2000/5000 起）；<br>• login `Queue`（`login.yaml:429` 现为 false），配合 `ZoneCapacityOverride`；<br>• 客户端做指数退避加抖动 | Java gateway、go/login、客户端 | 否 | 负责人；阈值用压测定 | L-14 尖峰压测 |
| L-03 | P0 | 数据层：完成 Phase 1（本文）。若未完成就开服，按连接总账重配 MySQL 规格和 `max_connections` | 本文 | **是** | 已拍板（B） | §9.1 |
| L-04 | P0 | go/db 遇到 1040 或拿不到连接时改为延后，不消耗重试预算（上限 3 次）；死信加消费者和告警。问题见 PROGRESS.md:7181，建议见 :7188<br>**（2026-10-14 第 1 批已落码，未编译、未测试）** 延后不耗预算、库级冷却、读任务快速失败、业务池读写超时、延后 / 死信计数与告警已写，设计 [db-task-store-unavailable-deferral.md](db-task-store-unavailable-deferral.md)；死信的查看与重放工具（第 2 批）未做，本条仍未关闭 | go/db | 否，两种后端都要 | 设计评审 | 单测 + 故障注入 |
| L-05 | P0 | 设计评审「新号首次进场免 DB 读」：建角时写一个空的 `PlayerAllData`，或打新号标记。号段 id 不复用，读到空就是真值。现状是新号首读一定冷读：每张子表一条 Kafka 读任务，玩家同步等待 8s（`entergamelogic.go:380`）<br>**（2026-10-14）已落码，未编译、未验证**：G3 建角后写新号种子，开关 `NewPlayerSeed.Enabled` 默认关，见 `new-player-first-enter-no-cold-read.md` §13 | login、go/db | 否 | 负责人 | 压测首次进场 p99 与 db_task 读量 |
| L-06 | P0/P1 | 共享 Redis 按用途拆实例：排队与限流 / 会话与锁 / PlayerAllData / mapping。`task:result` 改为按 login 实例分频道。压测后再定 `maxmemory` 和 limits，保持 noeviction（`redis.yaml:137-143` 现在只设了 requests；`redis-ha.yaml:9-13` 说明为什么上不了 Cluster）。注意：`redis-ha.yaml` 的 apply 路径在部署脚本里不存在（§6.1）<br>**（2026-10-18 B0-3 第 1 批已落码，未编译、未验证）** `task:result` 按 login 实例分频道：DBTask 加 `reply_instance = 10`，频道名与校验收口到 `go/shared/taskresult`，login 一条 SUBSCRIBE 同订旧频道与本实例频道并给读任务填回复目标，新增 `login_task_result_delivery_total{outcome}`。先用 PUBLISH，SPUBLISH 随上 Redis Cluster 那批切。go/db 按回复目标定向发布是第 2 批，未开工；在它上线前 go/db 仍只发旧频道，投递量不变。规格与验证命令见 `docs/handoff/launch-readiness-20261013/spec_B0-3_task_result_channel.md` 文末「第 1 批交付」，进度见 PROGRESS 对应条目。拆实例部分不受影响<br>**（2026-10-18 B0-3 第 2 批已落码，未编译、未验证）** go/db 按 reply_instance 定向发布：只发一次，空或不合法回落旧频道。另有 `db_task_result_notify_total{target,result}` 与独立告警 `deploy/k8s/task-result-notify-alerts.yaml`。上一句「第 2 批未开工」同此更正，见规格文末「第 2 批交付」。<br>**（2026-10-19 更正，上文「`redis-ha.yaml` 的 apply 路径在部署脚本里不存在」原文保留；已落码，契约测试未运行、未在 kind 复测，待 Codex 验证）** 核心实例的高可用形态已接入：`k8s_deploy.ps1 -RedisReplicas`（0 / 1 / 3，0 = 按档位，staging / prod 档缺省 3）取 3 时 apply `redis-ha.yaml`（1 主 2 从 + 哨兵 + 2 个 haproxy 入口代理）代替 `redis.yaml`；origin `43284eb0b`（10-16）接入，本地 10-19 补齐。同一个拓扑决定 WAITAOF 副本份数：data-service 的 `RouteStore.WaitAOFNumReplicas` 与 go/db 的 `ServerConfig.RedisClient.WaitAOFNumReplicas` 两处 ConfigMap 都按拓扑渲染（单实例 0、高可用 1，服务 yaml 里保持 0 供本机单实例用）；zone-up 没给 `-RedisReplicas` 时跟随线上 `sts/redis`，显式值与线上不同即拒绝。单实例切到高可用后要对每个 zone 重跑 zone-up 并滚动 db / data-service。高可用只解决可用性，核心仍是全服单主，本行的按用途拆实例与 maxmemory 不受影响。见 `no-single-node-horizontal-scaling-20261001.md` §5.1<br>**（2026-10-19 指向注，本行原文保留）** 本行开头的四路拆法照字面做不了：「会话与锁」不能单独成实例，「PlayerAllData」只有子表缓存和读结果能挪走，以 [shared-redis-split-by-purpose.md](shared-redis-split-by-purpose.md) §1 第 2 条为准。拆分设计已按 2000 万口径修订（该稿 2026-10-18 修订稿 §16~§26；第 1~4 批是设计、未落码，第 0 批 B0-1~B0-5 已落码、未编译未验证，见该稿 §17，本句 10-22 订正）：控制面按 zone 组分实例（§18）、redis-cache 必做（§19）、核心走负责人 2026-10-17 拍板的「单元化 + 单元内 Redis Cluster 按 `{player_id}` 分片」（§20），新分批见 §24，核心 `maxmemory` 与冷卸载见 §23（S1 前拍板），待拍板项见 §25.2 | deploy、go/login | 否 | 负责人 | 压测出 Redis 主的 CPU / ops / 内存曲线 |
| L-07 | P1 | 全球审计管道：消息上限、积压保留、容量预算，见表后展开<br>**（2026-10-14）积压与观测部分已落码，未编译、未验证**：G2a 按分区并发消费、批量落库旋钮（默认逐条）、lag 探针（含单分区最大积压）与告警 `deploy/k8s/data-service-audit-alerts.yaml`；消息上限与容量预算未做 | data_service、C++ snapshot、Kafka 部署 | 部分：落库耗时要在 TiDB 上重测 | 回档负责人 | consumer lag 指标；`snapshot_system` 发送失败计数 |
| L-08 | P1 | 连接预算：<br>• 导出 `sql.DBStats`，label 只用池类别；<br>• TiDB 每实例 `max_connections` 按 §2.4 取值（TiDB 默认不限，1040 只在设了上限时出现）；<br>• 拆开全局库池与访客池两个旋钮<br>**（2026-10-13 G2b，已落码，未编译、未测试）** 第 1、3 条已落码，第 2 条随 Phase 1：<br>• 池指标 `mmorpg_sql_pool_*{sql_client,pool}`（`go/shared/sqlpoolstats`，label 只有池类别）：go/db 在 K8s 上打开 `:9160`，data_service 在 `:9260`；告警文件 `deploy/k8s/mysql-connection-alerts.yaml`；friend / trade / Java gateway 没有导出；<br>• 额外落点库池拆成 `Placement.ZoneStore*` / `NonZoneStore*` 两组旋钮，另加 `ExtraStoreConnMaxIdleSeconds`（默认 60），旧键 `ExtraStore*` 只作回落；<br>• MySQL 档的 `max_connections` 500 → 1800，滚动余量按 R=2，总账写成部署脚本里的可执行函数并有契约测试（§2.4 状态注）；数值均待压测确认 | go/db、data_service | 是，上限参数随后端变 | — | DBStats 指标 |
| L-09 | P1 | go/db 消费组按 zone 命名。K8s 上全部 zone 共用 `db_rpc_consumer_group`（`k8s_deploy.ps1:3437`），任何一个 db Pod 变动，全球存盘一起停几秒；它还让单 zone 的 offset reset 变成全服操作（§6.3 第 7 步）。要同步改 merge_zone 等依赖方（no-single-node §3.4）。go/db 不配 HPA；首日冻结 db 和 data_service 的发布与扩缩容<br>**（2026-10-16）已落码，未编译、未测试，待 Codex 验证**（[db-consumer-group-per-zone.md](db-consumer-group-per-zone.md)）：组名 `db_rpc_consumer_group_z<ZoneId>`；go/db 入组前的交接闸（`go/db/internal/kafka/group_handoff.go`）；`k8s_deploy.ps1 -DbConsumerGroup keep\|zone\|legacy` 按线上 ConfigMap 与 Kafka 位点渲染最终形状（只写 `GroupID`）或交接形状（`ConsumerGroup` + `GroupHandoffFrom`）；`kafka_offset_reset.ps1 -FinalizeHandoff` 收尾；回档 Step 5 只重置本 zone 组、Step 6 落到 zone 组（§6.3 第 7 步更正行）；merge_zone P3 只认唯一权威组。切组要逐 zone 执行，没切的 zone 仍在共享组上，本条在全部 zone 切完并收尾之前不关闭；「kind 上删一个 Pod，只影响本 zone」尚未验证 | go/db、脚本 | 否 | 负责人 | kind 上删一个 Pod，只影响本 zone |
| L-10 | P1 | 号段开服档 Step=1000（§5.2 第 4 条） | login 配置 | 否 | 只改配置 | `login_create_player_stage_seconds` |
| L-11 | P1 | 名字表：<br>• `PRIMARY(name_norm)` 预切并设 `merge_option=deny`；<br>• `uk_player_name_owner` 的单调尾部与 §5.3 联动；<br>• 名字池 Z×2×20（`data_service.yaml:109-110`）计入连接总账；<br>• ODKU 行数语义在 TiDB 上回归（§4 #12） | data_service、runbook | 是 | — | PD 热点数据；`name` 阶段耗时 |
| L-12 | P1 | D1 的 `player_route` 同步事务：<br>• 压测定分片数 N 和 WAITAOF 的实际延迟；<br>• 评估去掉单调的 `idx_route_updated`；<br>• 4 条索引的尾部见 §5.1<br>**（2026-10-12 G1a）**`idx_route_updated` 已改为 `(route_shard, updated_at_ms, player_id)` 并保留（1b 增量推送要用，以后补索引要另写收敛代码）；四条索引都带 16 片前缀，TiDB 上由 `route_tool tidb-prepare` 预切（§5.2 第 7 条）<br>**（2026-10-14）**分片数 N 已按容量模型 §4.1 定为 64（建表前），不再由压测定；WAITAOF 延迟仍待压测<br>**（2026-10-15 更正，上一条不删）**上一条说的「定为 64（建表前）」是 `player_route` 的**行片** `RouteRowShards`（建表后不可改）。本行原文「压测定分片数 N」写于 G1a 引入行片之前，N 指的是 `zone_merge_fence` 的**围栏分片数**（D1 提案 :376、:409、§12 :1067），二者不是一个参数。围栏分片数是配置 `RouteStore.FenceShards`，按容量模型 §4.1「连带修正」取 MySQL 档 16、TiDB 档 64，任何时候可改；建角压测的 `register` p99 与 1213 / 1205 计数仍用来复核它够不够。TiDB 档 64 目前没有配置载体，见 §5.1 单行热点一行的更正<br>**（2026-10-19）** K8s 上 WAITAOF 副本份数按共享 Redis 形态渲染（单实例 0、高可用 1；data-service 与 go/db 两处同源，见 L-06 同日更正；已落码，未运行，待 Codex 验证）。staging / prod 档缺省是高可用，WAITAOF 要等 1 个副本的 AOF 落盘确认，所以 WAITAOF 的实际延迟要在高可用形态上压测，单实例上测出的数字不能代替 | data_service（D1） | 部分 | D1 负责人 | 建角压测：`register` 阶段 p99、1213 / 1205 计数；PD 热点里路由表（`player_route`、`zone_merge_fence`）的 Region |
| L-13 | P1 | TiDB 写热点：`tidb_scatter_region=global`；发号方案（§5.3）；存量 id 审计 | 本文 §5 | 是 | 负责人 | PD 热点数据 / Key Visualizer |
| L-14 | P1 | 交给 Codex 跑一轮开服尖峰压测，按 AGENTS §6 全流程，**环境是 §3.5 的专用压测环境**，同环境先跑 MySQL 基线。<br>场景：多 zone，0–30s 尖峰，同时覆盖建角、首次进场、周期存盘、进退场快照。用 `stress_summarize` 出对比表；没有这张表，不下容量结论 | Codex | 是 | 负责人点名环境（§9.3 #13） | 对比表 |
| L-15 | P2 | 首日按地域多开 zone，按负载分流选区。db_task 的 10 个分区不能在线扩，要扩只能走离线换代（`db-task-kafka-partition-contract.md`） | 运营、部署 | 否 | 负责人 | — |
| L-16 | P2 | 如果首日要开 `PinOnCreate`：遵守 placement §13 的顺序，先满足 D8、D1 两项前置 | Phase 2 | 是 | 负责人 | — |
| L-17 | P2 | 条件性的两项：<br>• gate 单 IO 线程；<br>• 正式服若用口令登录：Argon2id 每进程只允许 2 个并发（`login.yaml:346-348`），而且 login 会连库（§2.7），存量门禁脚本要先处理（§4 #13） | gate、login | 否 | — | 实测 t_kdf |
| L-18 | P1 | **上线前由 AI 复核（负责人 2026-10-19 要求）**：账号登录取逻辑服（home_zone）/ 物理服（storage_id）这条链路，届时是不是瓶颈、能不能水平扩展。按 [player-route-mapping-durability-proposal.md](./player-route-mapping-durability-proposal.md) §12.2「上线前复核步骤」逐条对照当时的代码、部署与压测重做一遍，不能沿用 2026-10-18 的结论（那时当前登录路径没有任何压测覆盖，倍数全是推算） | login、data_service、scene_manager、共享 Redis | 否 | 负责人 | §12.2 的结论写回 §12.1 表格并追加 PROGRESS；性能结论只认 L-14 的 `stress_summarize.ps1` 对比表。（2026-10-20：静态部分已复核，结论不变，见提案 §12.3；第 5 步压测未做，前置观测见开服线拍板清单 X-3） |

**L-07 展开：全球审计管道**

1. **消息上限。**
   - 全仓没有配置 Kafka 消息大小（只在 `topic_init.go:191` 的注释里提到），C++ 生产者只设了四项（`kafka_producer.cpp:59-73`），按默认值上限约为 1MB（推断）。
   - 超过上限的快照发送失败，只打 LOG_ERROR（`snapshot_system.cpp:101-112`），审计数据静默缺失。
   - 二选一：调高生产者、topic、broker 三处的上限（这不违反 D5）；或者限制快照大小并打指标。
2. **积压可能被删。**
   - prod 和 prod-like 档的 broker `log.retention.bytes` 是每分区 512MiB（`k8s_deploy.ps1:1186`、`:1199`；`-KafkaProfile` 缺省为 prod，`:62`），dev 档是 128MiB（`:1176`），通过 `kafka.yaml:276-277` 注入。
   - 审计 topic 只覆盖了 `retention.ms`（`topic_init.go:122-125`）。
   - 因此积压超过 512MiB/分区时，还没消费的快照可能直接被删（推断，与 segment 滚动的交互未核实）。「保留 30 天」（`data_service.yaml:221`）不是先到的那个上限。PVC 为 20Gi（`kafka.yaml:363`）。
3. **容量。**
   - 每次进场和退场各写一整份快照（`player_lifecycle.cpp:3555`、`:2737`）。
   - 全球只有 3 个分区，逐条串行落库（`snapshot_consumer.go:31`、`:73-116`；`start.go:123-141`）。
   - 按「(登录 + 登出速率) × 快照字节」算出每分区字节量和磁盘需求，并实测单条落库耗时。
   - 不够用就换代扩分区、改为按分区并发消费，或者定开服期的快照策略。加 lag 告警。
   - **（2026-10-13 G2a，未经运行验证）** 消费者已改为按分区并发、可配批量（默认仍逐条；打开批量要等压测对比表与内存护栏拍板），并加了 broker 视角的 lag / 保留期丢失探针与 8 条告警（`deploy/k8s/data-service-audit-alerts.yaml`）。离线换代步骤、T_safe 与盘量算式见 `docs/ops/audit-topic-generation-bump-runbook.md`。第 1 条消息上限与第 2 条保留策略**没有改**，仍是开服前拍板项（保留策略是阻塞项）。

**证据边界**：规模最大的一次压测是 R18。条件是单机、单 zone、45k 机器人、18 分钟爬坡，平均约 64 次进场/秒，后端是 MySQL（`stress-1zone-45k-2026-06-04-round18.md:9`、`:32`）。开服尖峰、TiDB、Phase 2、名字登记、RegisterPlayerZone 都从未压过。

---

## 8. 全球同服的地理形态

**推荐「区域集群 + 全球薄层」。不推荐「一个库让全球直连」，也不推荐把单个 TiDB 集群拉伸到多个大洲。**

1. **每个地域部署一套完整的 zone 集群**：包括 gate、scene、login、go/db、自己的 Redis 和 Kafka，以及一个按「同城三 AZ」部署的 TiDB。
   - 新号的 home_zone 等于建角时所在的地域，落点库也必须在同一地域。
   - 原因有两条：go/db 按 home_zone 消费 topic；每次写入都要对 mapping Redis 主节点做一次 MGET（`go/db/internal/kafka/placement_route.go:28`、`:90`）。
2. **全球只保留一个低频的薄层，由单一地域写入**：存放账号、名字（要全服唯一，省不掉）、号段、归属目录、好友与公会。
   - 远端玩家建角时多付 1–2 个跨洋 RTT，但这是一次性代价。
   - 号段靠双 buffer 预取，对地域基本不敏感（`client.go:11-24`）。
3. **部署契约（硬约束）**：带连接池或持锁的 SQL 客户端（go/db、data_service、tools/merge_zone），必须与它连接的 TiDB 在同一地域。跨洋通信只走 gRPC 或 Kafka。
   - 连接池吞吐上限约为 C/(k×RTT)。跨洋之后，8 条或 20 条连接的池会直接坍缩。
   - D1 围栏分片锁的持有时间也会乘上 RTT。
4. **为什么不拉伸单集群**：
   - 官方的延迟要求：单集群跨地域多副本，地域间延迟要小于 30ms；TiCDC 主备要求小于 100ms，对应 RPO <10s、RTO <5min；BR 没有延迟要求，RPO <5min（dr-solution-introduction）。
   - 每个事务都要向唯一的 PD leader 取 TSO。
   - Placement Rules 的 `MAJORITY_IN_PRIMARY` 在主地域故障时不会自动切换。
   - Follower Read 仍要和 leader 交互一次；Stale Read 只适合展示类的读。
   - 异地灾备用 TiCDC 或 BR。
5. **今天的代码配不出多地域**：
   - go/db 只有一个 `Database.Hosts`（`db.go:112`），落点只选库名（`store_registry.go:307-308`）。
   - relocate 只支持同一实例内。
   - SharedRedis 和 CZ-2 都是「全服单实例」契约（`cross-zone-matchmaking.md:103`、`cross-zone-scene-travel.md:31`）。
   - 要做多地域，先写设计文档，定下两条不变量：「`storage_id` 段 ↔ 地域集群」与「落点地域 = home_zone 地域」。然后补三项：go/db 按地域部署（或新增 storage → DSN 映射）、跨实例 relocate、跨地域转区的停服排空流程。Java 版要同步规划（AGENTS §12）。
   - **这些都不在 Phase 1 范围内。**
6. **首日按地域多开 zone**：就近分流加按负载分流，横向分摊按 zone 计的管线（L-15）。
7. **数据缺口**：跨洲 RTT 150ms 只是示例值，目标云厂商候选地域之间的实测 RTT 还没有。

---

## 9. 验收、分批、拍板事项、未核实项

### 9.1 验收标准（沿用决策文档 §5，并更新）

| # | 项 | 口径 |
|---|---|---|
| 1 | proto2mysql 测试套在真实 TiDB（≥ v8.5.5）上全绿 | 覆盖 16MiB−1 blob 写读、ODKU、多子句 ALTER、FOR UPDATE。同时确认 Save 实际生成的 SQL（§4 #6） |
| 2 | L1–L4 + chaos_test 全绿 | 决策 D5。在本地跑时，`start_game.ps1:446-474` 写死了 compose 的 `mysql` 容器，所以要么加一个本地切换开关，要么用一份指向 4000 端口、不进 git 的 yaml 覆盖；否则这一项只在 kind 或压测环境上跑。由 P1-2 定 |
| 3 | 压测对比 | 在同一个压测环境，以 MySQL 基线的 `prev-summary.txt` 为参照（§3.5）。DB task 子阶段延迟不能回归；PD 热点数据里没有持续热点，并区分 index 与 record 流量 |
| 4 | 表结构 diff 为空 | `SHOW CREATE TABLE` 与 `GetCreateTableSQL` 全表 diff 为空。包括表名、`NONCLUSTERED`、`SHARD_ROW_ID_BITS`，以及 AUTO_INCREMENT 拍板后的形态 |
| 5 | 迁移工具在 TiDB 上跑通 | 先验证 `SET SESSION lock_wait_timeout` 的行为（§4 #2）。<br>再跑通：go/db `cmd/migrate`（每个 zone 加 `-storage-id`）、P1-1 之后的 `data_service -migrate`（`AllowMissingGuidUniqueKey` 没接线，不存在「打开开关」这条路）、schemamigrate（friend / trade / guild）。<br>还要验证三件事：两个副本并发迁移会被 `GET_LOCK` 串行化；持锁连接断开、或 tidb-server 被杀后锁会释放；经 LB 的跨实例 `KILL QUERY` 能终止语句和 DDL |
| 6 | 快照去重 | 同一个 guid 并发重放，只落一行。在 TiDB 上，缺键的表会让 `NewSnapshotStore` 拒绝装配（§4 #1） |
| 7 | friend 推荐查询 | 用 `LOCK STATS` 模拟统计信息低估后，EXPLAIN ANALYZE 中没有全表扫描（§4 #3 的 12 种库与统计状态） |
| 8 | CHECK 生效 | 插入 `id_segment.max_id ≥ 2^55` 被拒；D1 的 CHECK 同样会拒绝 |
| 9 | BR + PITR 演练 | 全量备份 + 日志备份 → 恢复到影子集群的 T 时刻 → 按库过滤恢复一个 zone 库 → 走完 `id_segment` 水位流程和孤儿对账（§6.3 第 8 步）。记录耗时。kind 上用 MinIO |
| 10 | 回退通道演练 | TiCDC TiDB → MySQL，按 §3.4 的「R1 切回步骤」走完，sync-diff 干净 |
| 11 | 连接 | 导出 DBStats。记录 tidb-server 达到 `max_connections` 时客户端的行为（1040 链路，§2.4） |
| 12 | D1 | 提案 :381 要求的 TiDB 悲观 RC 用例全绿 |
| 13 | 大快照 | 对 store 直写一条 16MiB−1 的快照，落库成功（验证 TiDB 的 32MB 配置生效）。经 Kafka 的大快照，等 L-07 调高消息上限后，单列为一个用例 |
| 14 | 新增：affected rows 与 ODKU 语义 | §4 #12 表中的每一处各一条用例。其中 player_name 的 `TestReserveOutcomeContract` 要在 TiDB 上跑 |
| 15 | 新增：RC + 悲观事务 | guild 全连接 RC 下的锁序用例（`guild_lock_order_mysql_test.go` 在 TiDB 上的对应版本，范围由 Codex 评估） |
| 16 | 新增：部署契约 | §2.8 第 6 条全部满足；`-Database` 与集群标记不一致时拒绝部署 |
| 17 | 新增：导入断言 | 目标表非空就停；两侧 `schema_migrations` 一致；`id_segment` 水位相等 |
| 18 | 新增：P1-D2 原验收的替换口径 | TiDB 集群（kind 或压测环境）中 pd / tikv / tidb 全部 Ready。<br>`SHOW CONFIG` 核对 `performance.txn-entry-size-limit=33554432` 与 TiKV `raftstore.raft-entry-max-size=32MB`；`SHOW GLOBAL VARIABLES` 核对 §2.5 的全局变量。<br>runbook 存在且被 D8 引用；备份 CR 在目标环境中运行（§6.1） |

### 9.2 实施分批与工作量（量级估计，不是承诺）

| 批 | 内容 | 主要文件 | 估计文件数 | 依赖 |
|---|---|---|---|---|
| P1-0 核实与拍板 | 第一项先验证 `lock_wait_timeout` 与 GET_LOCK 的行为（§4 #2）。<br>其余：Codex 读 proto2mysql Go 源码，确认 Save 与 DDL；导出 `GetCreateTableSQL`；在各环境审计 `player_id < 2^55` 的存量；量出各环境数据量，确认哪些环境要保留数据；核对 `tidb_txn_mode`；核对 MySQL 只写 CHARSET 的表实际用的排序规则；完成 §9.3 拍板 | — | 0 | — |
| P1-1 代码兼容 | 快照去重的 TiDB 形态（§4 #1），并让 `NewSnapshotStore` 在 TiDB 上缺键时拒绝装配；如果拍板，去掉主表的 AUTO_INCREMENT 并配 MySQL 迁移；friend 查询钉住计划；data_service、friend、trade、guild、login PasswordAuth 补 `ConnMaxLifetime`；更正 `store_registry.go:294` 的注释；导出 `sql.DBStats`（与 L-08 合并）**（2026-10-13 G2b：这一子项由 G2b 批 B 承担（`go/shared/sqlpoolstats`，已落码，未编译、未测试），P1-1 不再另注册收集器；本行其余子项不变，新增的池接指标时复用这个包）**；gateway 表补 COLLATE | `go/data_service/internal/store/*`、`proto/common/database/*.proto`、`go/friend/...`、各服务 `svc`、`deploy/mysql-init/gateway_tables.sql` | 13–19 | **D1 第 1 批合入之后** |
| P1-2 部署 | Operator 安装说明，以及 dev / staging / 压测 / prod 四档的 TidbCluster CR（放在 `mmorpg-tidb`）；TidbInitializer + 幂等初始化 Job；`k8s_deploy.ps1 -Database`（必填、带预检、生成集群标记）；三个 migrate Job 参数化；`Wait mysql` 门禁参数化；db-migrate Job（如果拍板）；infra-down 保护；`dev_tools.ps1` / `k8s_image.ps1` 透传参数；`k8s_zone_rollback.ps1` 第 0 步核对；契约测试；告警 | `deploy/k8s/manifests/tidb/*`、`deploy/k8s/manifests/go-svc/*-migrate.yaml`、`deploy/tidb-config/*`、`tools/scripts/{k8s_deploy,dev_tools,k8s_image,k8s_zone_rollback}.ps1`、`tools/scripts/tests/*`、`deploy/k8s/*-alerts.yaml` | 14–20 | P1-0；D1 第 1 批之后（同改 `k8s_deploy.ps1`） |
| P1-3 迁移与预切工具 | `tidb_migrate.ps1`：Dumpling `--no-schemas` 并排除台账 → 导入前断言 → Lightning → sync-diff → ANALYZE；`tidb_presplit.ps1`；TiCDC 反向 changefeed；清库脚本支持 TiDB；`stress_snap` / `stress_summarize` 增加 TiDB 与 PD 段 | `tools/scripts/*` | 6–8 | P1-2 |
| P1-4 备份恢复 | `docs/ops/tidb-br-pitr-runbook.md`；Backup / BackupSchedule / 日志备份 CR，**并在目标环境运行**；`zone_data_rollback.md` §3；`k8s_zone_rollback.ps1` Step 2/3/4/5 的 TiDB 版，包括 Redis 按 zone 范围清理 | — | 5–7 | P1-2 |
| P1-5 验收 | §9.1 全部，由 Codex 执行；包括压测与修复轮次 | — | 0 | 全部 |
| P1-6 MySQL 退役 | §3.4 末尾的步骤 | `deploy/k8s/manifests/infra/*`、`tools/scripts/k8s_deploy.ps1`、契约测试 | 3–5 | R1 窗口结束 + 负责人同意 |
| 文档 | 见 §9.5 | `docs/*`、`deploy/k8s/{README,AGENTS}.md` | 8–10 | 随批更新 |

- **合计约 45–60 个手改文件**，超过 AGENTS §10.2 的 30 个文件门槛，开工前要单独授权。v0.1 估的 35–50 偏低，因为漏了 migrate Job、包装入口、回滚脚本和运维文档。
- **expand / contract（AGENTS §11.3）**：P1-1 会改 MySQL 表结构。以方案 B 为例，分三步：
  1. expand：GM 和 data_service 改为从号段领 guid，旧表结构保持不变，旧二进制仍可运行；
  2. 回填：用离线、幂等的脚本，回填存量中 `snapshot_guid=0` 的行；
  3. contract：建普通唯一键，删掉 `uk_snapshot_guid_nz` 和生成列。此后旧二进制写入 guid=0 的快照会报重复键，结果是 fail-closed，不会静默。
- **两档并存**：P1-2 的 mysql 档行为保持不变，由契约测试钉住，可以随时切回。
- **时间量级**：P1-0 约 1–2 天；P1-1 约 3–5 天；P1-2 约 4–6 天；P1-3、P1-4 各约 2–3 天；P1-5 约 3–5 天。日历时间约 4–5 周，取决于 Codex 的验证轮次和拍板速度。

### 9.3 需要负责人拍板的事项

1. 部署方式：TiDB Operator v1.6（推荐），还是手写 StatefulSet（只限 dev）。
2. 版本钉到 v8.5.x 的哪个 patch（必须 ≥ v8.5.5）。
3. prod 的拓扑与规格（同城三 AZ，规格用压测定）；kind 上要不要跑 Operator。
4. TiDB 的 namespace 名（暂定 `mmorpg-tidb`），以及各档的 `pvReclaimPolicy`。
5. 迁移方式：全新建库 / 停服窗口 Dumpling + Lightning / DM 在线；以及**哪些环境有必须保留的数据**。
6. TiCDC 反向通道保留多少天；MySQL 什么时候退役；最后一次备份保存多久。
7. 快照去重在 TiDB 上的形态：A1 / A2 / B（推荐 B）。
8. 三张主表是否去掉 `player_id` 的 AUTO_INCREMENT（建议去掉，必须在 TiDB 首次建表前定）。
9. 发号：Phase 1 不动（推荐）/ 打散 / 段边界预切；存量 id 审计，或收紧 `MaxIDExclusive`。
10. friend 推荐查询：全局绑定 / SQL 提示 / 方言 SQL，至少落一个。
11. 备份存储（S3 / OSS / MinIO）、保留期、PITR 的 RPO 目标。
12. 确认「全部库同批迁、同批回退」，并修订决策 D9 里「单独回退」的表述（理由见 §2.7）。
    - （2026-10-20 补，原文不删）容量模型 §4.5 / §6 #11 主张 S1 以上 OLTP 与审计至少分两套集群、单元化后每单元一套，与本条的「同一集群」冲突，拍板时一并定，见 §2.7 末尾的注。第 3 条的 prod 规格同样改按 §4.5 的公式定（§2.2 表后注）。
13. **点名 Codex 可以操作的环境**（AGENTS §9）：专用压测环境、kind、staging，逐个授权。
14. Phase 1 超过 30 个文件的改动面授权。
15. **安装 helm、TiDB Operator、CRD、tiup 的授权**（AGENTS §10.2）。
16. go/db 迁移方式：新增 db-migrate Job（推荐），还是沿用手工。
17. login 口令认证的门禁脚本：迁库前在 MySQL 上跑完，还是改写成应用侧检查（§4 #13）。
18. zone 回档时 Redis 按 zone 范围清理的键清单，由谁定（§6.3 第 6 步）。
19. §7 各项 P0 的负责人与时间点。
20. Java 版：登记「不适用」，还是同步评估（§10）。
21. （2026-10-12 G1a）D1 `route_meta` 是否设为 TiDB 缓存表（`ALTER TABLE route_meta CACHE`）：默认不设，看压测里它所在 Region 的读热度再定；设了之后冻结写最长阻塞一个读租约（D1 提案 §11.2 G1a 段）。
    - **（2026-10-15 更正：本项已不是待拍板项，原文不删）**2026-10-13 起按容量模型 §4.1「连带修正」改定为 **S2 档起必选，S1 可选**，「默认不设、看压测再定」作废。D1 提案已同步：§11.2「TiDB 运维步骤」第 6 条（:836）、「对 §5.4 / 第 1b 批的约束」（:824）、「行片 16 → 64」的「连带修正」（:931）、§12 梯子第 2 级（:1093）。依据是容量模型 §2.6 的「route_meta 的 R1 单行读」：每次 W1 一次点读，压在单个 Region leader 上，S2 档 5k~2.5 万/s，设成缓存表后为 0（推算，没有压测）。从拍板清单角度视为已定；剩下的是执行项：`tidb-prepare` 目前不发这条语句（是否并入留给 D1 第 1b 批），BR / TiCDC 是否保留 CACHE 属性【未核实】、按不保留处理、恢复后重放（§6.2「恢复后必做」②）；冻结写最长阻塞一个读租约的代价不变，1b 的冻结超时要 ≥ 租约 + 余量。
    - **（2026-10-18 更正，D1 第 1b-1 批已落码、未编译、未测试；原文不删）**上一条「是否并入留给 D1 第 1b 批」已定为**不并入**：`ALTER TABLE route_meta CACHE` 归本文 Phase 1 的部署步骤执行（建表之后、开服之前；BR / TiCDC 恢复之后按 §6.2「恢复后必做」② 重放），`route_tool tidb-prepare` 不发这条语句（D1 提案 §11.2「第 1b-1 批」运行时要点第 8 条）。设成缓存表之后，`route_tool freeze` / `unfreeze` 的 `-freeze-timeout`（缺省 5s，下限 5s）要设成 ≥ `tidb_table_cache_lease` + 2s（租约缺省 3s【未核实】），否则冻结 / 解冻写可能等不到租约到期就超时、退出码 2（重跑即可，两者都幂等）。另：D1 第 1b-2 批（对账 leader）落地之前没有自动回档检测，全局库回档之后要按 `docs/ops/merge-zone-runbook.md` §9.4 手工 `freeze`（见 §6.2「恢复后必做」⑤ 的同日更正）。

### 9.4 未核实项

- **proto2mysql**：v0.2.0 的 Save 实际生成什么 SQL；DDL 会不会给 `player_id` 带上 AUTO_INCREMENT。目前的依据只有 Java 镜像的注释，本机没有 Go 模块源码。
- **AUTO_INCREMENT**：在 TiDB 上是否与 `_tidb_rowid` 共用分配器；显式写入大号的影响。
- **生成列**：能否在生成列上建唯一索引（§4 #1 方案 A）；Dumpling / Lightning 怎么处理生成列。
- **会话变量与锁**：
  - `SET SESSION lock_wait_timeout` 在 TiDB 上的行为（v8.5 系统变量页没有这个条目）；
  - `GET_LOCK` 在连接断开或 tidb-server 宕机时是否释放；
  - `tidb_txn_mode` 的文档条目，本次没在页面里检出。
- **预切**：
  - ``SPLIT TABLE … INDEX `PRIMARY` `` 对非聚簇主键的写法；
  - `tidb_enable_check_constraint` 关闭期间建的表，打开后会不会补上约束；
  - TRUNCATE 之后，预切的 Region 和 table attributes 会不会丢（推断会丢）。
  - （2026-10-12 G1a，D1 路由表，由 D1 的真 TiDB 用例 `TestRouteStore_TiDBClusteredShardsAndPresplit` 取证）：组合聚簇主键与多列索引的 `SPLIT … BY` 元组写法；对已存在的边界重复 SPLIT 是否幂等；`ATTRIBUTES` 是否覆盖索引 Region；SPLIT / ATTRIBUTES 所需权限；`information_schema.TABLES.TIDB_PK_TYPE` 列；带 NULL 的索引值能否作为切分点；缓存表的写阻塞时长与事务内读是否走缓存；TiDB 的 3819 报错文案（route store 探针 P4 按「3819 + 约束名」判）；行构造器 `(a, b) IN ((…), …)` 是否走 Batch_Point_Get；SPLIT 语句是否自动打散、`tidb_scatter_region=global` 对 SPLIT 是否起作用；`ALTER TABLE … ATTRIBUTES` 是否推进 schema 版本、在途悲观事务是否因此 8028；运行期把 `tidb_enable_check_constraint` 改回 OFF 是否停止执行已建表上的约束。
- **备份恢复与 Operator**：
  - Lightning 在没有 schema 文件时是否直接用目标侧已有的表；
  - Operator 的 Restore CR 在 PITR 模式下能否带表过滤；
  - 日志备份能否存到 PV；
  - BR 恢复会不会带回全局变量；
  - TiCDC 能不能捕获 BR 恢复写入的数据（推断不能）。
- **时钟**：TiDB 上 `NOW()` 取的是哪台机器的时间，各 tidb-server 之间的偏差有多大（影响 D1 的 watermark 容差）。
- **TiKV**：
  - 单个 Region 的写入吞吐上限（文档没有给数字）；
  - 悲观事务下按非聚簇主键点更新时，会不会在主键索引 key 上写锁记录；
  - block-cache 与容器内存上限的关系。
- **MySQL 侧**：
  - `read_only` 对 `CONNECTION_ADMIN` / `SUPER` 无效（MySQL 8.0 语义，推断级）；
  - 只写 CHARSET 的表实际用的排序规则；
  - Dumpling 用 `flush` 需要 RELOAD 权限。
- **Kafka**：
  - librdkafka 和 broker 默认的消息上限，本仓没有实测；
  - `log.retention.bytes` 与 segment 滚动的交互；
  - 共享消费组重置单个 topic 的前置条件。
- **gateway / robot**：robot 能否绕过维护态；Operator 生成的 Service 命名；TiDB 上能否用通配库名授权。
- **容量数据**：
  - 单条快照的实际大小、单条落库耗时、首日全球登录加登出的速率；
  - 非聚簇主键点查在 TiDB 上的实际耗时（50ms 那个数来自 dev 机 MySQL）；
  - 共享 Redis 单主的 ops/s 上限；
  - 排队 leader 锁跨 zone 不出队（目前只是静态阅读的结论）；
  - D1 的分片数 N、WAITAOF 的实际延迟、TiDB 悲观事务下的建角吞吐；
  - 跨洲 RTT 的实测值；login 单实例约 1.5 万 QPS 这个数的出处；Argon2id 的 t_kdf。

### 9.5 连带文档更新（随各批执行，不在本文里改）

- **决策文档**：
  - §6 首段写明 D-02 = B（日期、决策人）；
  - §5 改为本文 §9.1，§D4 改为本文 §2.5，§D8 指向新 runbook 并采用 §6.1 收紧后的门禁；
  - §D9 修订「单独回退」，并顺带写明一点：TiDB 的默认 sql_mode 已经包含 `STRICT_TRANS_TABLES`，而 `guild.yaml:45` 的 DataSource 没带 sql_mode，所以切到 TiDB 后这一项不受影响；
  - §7 勾选 runbook。
- **PROGRESS.md** 记一条，注明 Java 状态；**`docs/README.md`** 登记本文和新 runbook。
- **backlog**：
  - D-02 和 P1-D2 改状态；
  - P1-D2 的验收（:810-812，要求 kind 上 pd / tikv / tidb 都 Ready，并能 `SHOW CONFIG` 核对）改为本文 §9.1 #18；
  - P1-D2 第 6 步（:807，`cmd/migrate` 阻断）标为已失效：go/db 里已经找不到 `PbMysqlDB` 的引用。
- **no-single-node §6**：写「方向已定 → 进行中，见本文」。**placement §11.4**：「上游 Phase 1 本身也没上线」那条改为指向本文。
- **D1 提案 :64**：「TiDB ≥ 7.2 生效」补上前提 `tidb_enable_check_constraint=ON`；§6「数据库回档」一行补上 TiDB 下按库过滤恢复的口径（本文 §6.3）。
- **`docs/ops/merge-zone-runbook.md`**：`:237` 的 `svc/mysql` port-forward、`:343-345` 的 `deploy/mysql` exec，都改成 TiDB 口径。
- **`deploy/k8s/README.md`**：
  - 存量 PVC 补库的 GRANT 命令（`:507-510`、`:560`、`:617`）；
  - 「恢复全局库」（`:513` 起）；
  - `:632-635` 的 TiDB BR 按库恢复清单，要求列上 `mmorpg_friend`。
- **`deploy/k8s/AGENTS.md`**：`:24`、`:82`「redis / kafka / mysql 单副本 Deployment」。
- **AGENTS.md §6.2 第 6 条**：清空数据库的 TiDB 口径，由负责人修改。
- **`redis-ha.yaml:3`**：说明它的 apply 路径在部署脚本里不存在。
  - **（2026-10-19 更正，上一条原文保留）不再需要**：apply 路径已接入（`-RedisReplicas`，见 §6.1、L-06 同日更正），`redis-ha.yaml` 文件头已改写为接入说明；已落码，未运行，待 Codex 验证。
- **Java `PARITY.md`**：登记本文的状态（§10）。

---

## 10. Java 版（AGENTS §12）

**结论：Java 版不需要同步迁 TiDB，在 `PARITY.md` 登记「不适用（Java 单库，存储方案另行设计）」即可。但有三件跟本文相关的事要同批评估。**

**依据：**
- AGENTS §12 规定，两版共享的只有客户端契约，存储访问由 Java 按自己的惯用方式实现。
- Java 仓库已经把「存储落点多库路由 / TiDB 分库」列为不移植（`docs/porting/roadmap.md:104`），TiDB compose 也标为「与 Java 无关」（`docs/porting/deploy-ci-spec.md:116`）。
- Java 是单库 MySQL 8.4（`docs/design/architecture.md:1282`）。
- 本文 §4 #5 的 Kafka 消息上限、§2.4 的连接预算，都属于服务端内部的事，各版本自己核算。

**要同批评估的三件事：**
1. §5.3 如果选「发号打散」，`player_id` 的分布会变（格式不变）。它对客户端可见，要同批评估 Java 是否受影响，以及 PARITY 怎么登记。
2. §7 的 L-01 / L-02（排队、限流、客户端退避）涉及客户端看得到的排队和限流行为。Java 有对应实现（architecture.md §8「登录排队与开服限流」），两版要对齐阈值和客户端退避的口径。
3. Java 将来如果也上 TiDB，本文 §4 的阻塞有一部分自然不存在：
   - Java 快照去重用的是主键快照号加 ODKU，没有可空唯一键（`PARITY.md:69`），与本文推荐的方案 B 同形；
   - `PbMysql` 已经输出 TiDB 注释块，也处理了 TiDB 的异步 DDL（`PARITY.md:101`）。

   但下面几项对 Java 同样适用：
   - `GET_LOCK` 的超时范围；
   - CHECK 默认不生效；
   - 只写 CHARSET 的表在两个库上排序规则不同；
   - affected rows 的语义；
   - `createDatabaseIfNotExist=true` 需要建库权限。

---

### 官方文档出处（docs.pingcap.com，2026-10-12 抓取）

- **本轮（TiDB v8.5，`/tidb/stable/`）**：`sql-statement-kill`、`br-pitr-manual`、`tidb-lightning-configuration`、`system-variables`（全页分四段读完）。
- **本轮（TiDB Operator v1.6，`/tidb-in-kubernetes/stable/`）**：`initialize-a-cluster`（链接指向 v1.6.6）、`backup-restore-overview`。
- **起草轮（TiDB v8.5）**：`locking-functions`、`tidb-configuration-file`、`generated-columns`、`auto-increment`、`shard-row-id-bits`、`table-attributes`、`sql-statement-split-region`、`br-pitr-guide`、`migrate-from-tidb-to-mysql`、`migrate-small-mysql-to-tidb`、`dumpling-overview`、`ticdc-overview`、`multi-data-centers-in-one-city-deployment`、`character-set-and-collation`、`constraints`。
- **起草轮（Operator v1.6）**：`tidb-operator-overview`、`configure-a-tidb-cluster`。
- **评审轮（评审人抓取，本文转引）**：`ticdc-compatibility`、`information-functions`、`restore-from-aws-s3-using-br`（Operator v1.6）。
- **更早一轮（launch_verdict）已复核**：`clustered-indexes`、`troubleshoot-hot-spot-issues`、`configure-load-base-split`、`auto-random`、`dr-solution-introduction`、`tikv-configuration-file`。
- **没有复核的两页**：`high-concurrency-best-practices` 两轮抓取都是空页；`mysql-compatibility`（存储过程不支持）本次没有重新抓取。

---

## 评审处理记录

v0.1 草稿收到两份评审：A（运维视角）和 B（正确性视角）。下面逐条记录处理结果。

「复核」指本文起草人重新读过所引的文件和行号，或重新抓取过官方文档。

### 评审 A（运维视角）

| 编号 | 意见 | 处理 | 落点 |
|---|---|---|---|
| A-M1 | §3.2 停写顺序自相矛盾：zone-down 会删掉 db 和 data-service | **采纳**。复核 `k8s_deploy.ps1:5426`、`:937-938`、`data_service.yaml:216-220`；改为先缩容 gate 和 scene、排空，再 zone-down，模板取自 `merge-zone-runbook.md` §8 Step 2。两种做法里只选「先排空」 | §3.2 第 9 步 |
| A-M2 | migrate Job 和发布门禁写死 MySQL | **采纳**。复核三份 migrate yaml 与 `:4203-4211` | §2.8 第 3 条、§3.4 退役 |
| A-M3 | `-Database` 不粘滞，可能导致静默分库 | **采纳**。改为必填，加集群标记预检，包装入口透传，回滚脚本第 0 步核对，并以只读作兜底 | §2.8 第 2 条 |
| A-M4 | 预建表时写入的行与导入撞主键 | **采纳**，与 B-M3 合并。补齐 `zone_config` 和 `route_meta`；清行用 DELETE，不用 TRUNCATE | §3.2 第 7 步、§4 #14 |
| A-M5 | MySQL 只读的机制写错了 | **采纳**。保留 `read_only`，TiCDC 账号用 CONNECTION_ADMIN，应用账号不得有 SUPER；dev 档不进 R1 | §3.2 第 10、14 步，§3.4 |
| A-M6 | 初始化没处理 root 口令，也没用 Operator 自带机制 | **采纳**。复核 initialize-a-cluster（v1.6）与 `mysql.yaml:113-128`。用 TidbInitializer 处理 root，加一个幂等 Job，补齐 entrypoint 的三件事 | §2.6 |
| A-M7 | 压测环境与 AGENTS §6、§9 冲突，基线不可比 | **采纳**。新增专用压测环境，在同一环境跑 MySQL 基线；Codex 能操作哪些环境由负责人点名；补上多副本采集方案 | §2.2、§3.5、L-14、§9.3 #13 |
| A-M8 | `infra-down` 会带走 TiDB | **采纳**。复核 `:5888-5890`。TiDB 放进独立 namespace；删除顺序先删 CR；压测档用 Delete 回收策略 | §2.2、§2.8 第 5 条 |
| A-重要1 | Operator PITR 能力没有证实 | **采纳**。执行载体先用 br 二进制；kind 上用 MinIO | §6.2 |
| A-重要2 | 备份 CronJob 不在部署脚本里；redis-ha 的 apply 路径不存在 | **采纳**。复核 `:5740` 与 runbook :28；D8 门禁收紧 | §6.1、§9.5 |
| A-重要3 | 按库恢复会造成跨存储不一致；BR 与 TiCDC 的关系 | **采纳**，与 B-M5 合并 | §6.3 第 8 步、§3.4 R1 纪律 |
| A-重要4 | 迁移在哪里跑、窗口多长、装工具要授权、kind 的 context | **采纳** | §3.2 第 1–2 步、§2.1、§9.3 #15 |
| A-重要5 | go/db 在 K8s 上没有 migrate Job | **采纳**。复核 `:937`、`:3369-3370`；新增拍板项 | §3.2 第 5 步、§9.3 #16 |
| A-重要6 | 「切换后、放流量前」的状态没有落地机制 | **采纳并加强**。复核时发现维护态挡不住 `zoneId<=0`（`AssignGateService.java:174-177`），白名单也没接上，所以在入口层加了一道断开 | §3.2 第 8、16 步 |
| A-重要7 | D1 watermark 与库时钟 | **采纳**，升级为切换时的检查项，并补 NTP 要求 | §2.2、§3.2 第 16 步 |
| A-重要8 | 恢复之后漏了重设全局变量 | **采纳**（BR 是否恢复全局变量仍列为未核实） | §6.2、§2.6 |
| A-重要9 | 各批次不能独立回退；缺少退役步骤 | **采纳**。写入 expand/contract、两档并存、P1-6 退役 | §9.2、§3.4 |
| A-重要10 | §9.5 漏项；文件数估低了 | **采纳**。估计改为 45–60 | §9.5、§9.2 |
| A-重要11 | 本地验证路径缺失 | **采纳** | §9.1 #2 |
| A-小1 | 10080 端口在 `:51` | **采纳** | §3.5 |
| A-小2 | KILL 的【未核实】可以改写 | **采纳**。复核 sql-statement-kill（v8.5） | §4 #2 |
| A-小3 | PITR 与日志备份共存的【未核实】可以删 | **采纳**，同时写上官方的不一致代价 | §6.2、§6.3 |
| A-小4 | 回滚脚本是工作区行号 | **采纳**。文首注明以工作区为准的文件 | 文首 |
| A-小5 | Dumpling 的 flush 需要 RELOAD；建议改用 `snapshot` 或 `none` | **部分采纳**。改用 `none`；**不采用** `snapshot`，因为它按 TSO 取快照，面向 TiDB 源（推断） | §3.2 第 11 步 |
| A-小6 | Lightning 只导数据的配置项 | **采纳**。复核配置页确实没有这个选项，列为未核实，并给出退路 | §3.2 第 12 步 |
| A-小7 | kind 的 max-replicas 与 block-cache | **采纳** | §2.2 |
| A-小8 | graceful-wait 要配合 terminationGracePeriodSeconds | **采纳** | §2.3 |
| A-小9 | `stress_snap` 应抓 PD 的 hotspot | **采纳** | §3.5 |
| A-小10 | R1 缺收尾步骤，也缺 schema 冻结 | **采纳** | §3.4 |
| A-小11 | offset reset 与共享消费组的关系 | **采纳**，与 L-09 交叉引用 | §6.3 第 7 步、L-09 |
| A-小12 | D9 的 `STRICT_TRANS_TABLES` | **采纳** | §9.5 |

### 评审 B（正确性视角）

| 编号 | 意见 | 处理 | 落点 |
|---|---|---|---|
| B-M1 | 「login 运行期不连库」不对；存储过程脚本；工具漏项 | **采纳**。复核 `login.yaml:336-345`、`password_provider.go:80`、`password_admin/main.go:61`、`20260803_password_auth.sql:7-9` | §2.4、§2.7、§4 #13、L-17 |
| B-M2 | KILL 的推断与官方文档相反 | **采纳**。复核文档；补上 schemamigrate 的 KILL 调用点 | §4 #2、§9.1 #5 |
| B-M3 | 停服迁移一定会在 Lightning 阶段失败 | **采纳**，与 A-M4 合并 | §3.2 第 7 步 |
| B-M4 | 大快照的前提不成立：Kafka 的约 1MB 上限先截住了 | **采纳**。复核 `kafka_producer.cpp:59-73`、`snapshot_system.cpp:101-112`、`consumer.go:133`，全仓确实没有配置消息上限；改写 #5，验收 #13 拆成两条 | §4 #5、L-07、§9.1 #13 |
| B-M5 | 回档第 5 步不能照写 FLUSHDB；「更好」要加限定 | **采纳**。复核 `zone_data_rollback.md:163-171`；改为按 zone 范围清理，并写成取舍 | §6.3 |
| B-M6 | 「只有打开开关才能放行」不准确；是四个 store | **采纳**。复核 `schema.go:1242-1249`、`servicecontext.go:157-221`；顺带补上 `NewSnapshotStore` 的静默路径 | §4 #1、§9.1 #5/#6 |
| B-重要1 | `lock_wait_timeout` 的后果写轻了 | **采纳**。复核系统变量全页确实没有这个变量；状态改为「可能阻塞」，放在 P1-0 第一项 | §4 #2、§9.2 |
| B-重要2 | 漏了 affected rows 语义 | **采纳**。复核 economy、guild、player_name 三处 | §4 #12、§9.1 #14 |
| B-重要3 | guild 依赖 RC 和悲观事务 | **采纳** | §2.5、§9.1 #15 |
| B-重要4 | migrate Job 写死了地址和端口 | **采纳**，与 A-M2 合并 | §2.8 第 3 条 |
| B-重要5 | D1 的单调索引数写少了 | **采纳**。复核提案 :81-83 | §1.3、§5.1 |
| B-重要6 | L-07 的 Kafka 磁盘口径不对 | **采纳**。复核 `:1176/:1186/:1199`、`kafka.yaml:276-277/:363` | L-07 |
| B-重要7 | 「每实例 ≥ T」的推理不成立 | **采纳**，改用保护闸公式 | §2.4 |
| B-重要8 | P1-D2 验收要明确修订；第 6 步已失效 | **采纳**。复核 go/db 里 `PbMysqlDB` 的引用数为 0 | §9.1 #18、§9.5 |
| B-小1 | 两个变量的作用域 | **采纳**。约束检查改在配置文件里钉住 | §2.5 |
| B-小2 | 排序规则是确定不同；D1 的表；`default_collation_for_utf8mb4` | **采纳**。复核时补充一点：该变量取不到 `unicode_ci`，所以建库时仍要显式写排序规则 | §4 #10、§2.5 |
| B-小3 | 方案 A 漏了 STORED 候选；多副本前提误读 | **采纳**。复核清单 :26 与 :31-32 | §4 #1 |
| B-小4 | dev 档的去重会静默失效 | **部分采纳**。复核后认为，dev 的 AutoMigrate=true 路径仍是 fail-closed，真正的静默路径是「表已建好但缺键，又以 AutoMigrate=false 启动」。按这个口径写入，并让 P1-1 改为拒绝装配 | §4 #1 |
| B-小5 | GET_LOCK 断连后是否释放；guild 启动期也用了它 | **采纳** | §4 #2、§9.1 #5 |
| B-小6 | 行号偏差 | **采纳**。逐条更正：compose `:51`、名字池 `:109-110`、PROGRESS `:7181`/`:7188`、guild `:335-336`、merge_zone `:246-249`、`snapshot_consumer.go:97-103`。复核时另外更正 `schema.go:64` → `:66`（CHECK 所在行） | 全文 |
| B-小7 | §5.1 表不完整 | **采纳**。复核 trade 和 guild 的 proto | §5.1 |
| B-小8 | AUTO_INCREMENT 用尽不是近期风险 | **采纳**（推算） | §4 #7 |
| B-小9 | MySQL 只允许 TiCDC 写入的实现方式 | **采纳**，与 A-M5 合并 | §3.2 第 14 步 |
| B-小10 | watermark 检查要升级 | **采纳**，与 A-重要7 合并 | §3.2 第 16 步 |
| B-小11 | 自动 ANALYZE 比例不同；查询钉计划不能是可选项 | **采纳**。复核 `tidb_auto_analyze_ratio` 默认 0.5 | §4 #3 |

### 复核时新增或修正的内容（两份评审都没提）

- **维护态挡不住自动选区。** `AssignGateService.java:174-177` 对 `zoneId<=0` 直接放行；`:166-167` 写明白名单还没接上。所以维护态不能单独作为放流量前的闸门（§3.2 第 8、16 步）。
- **现有 zone 回档脚本的排空顺序有问题。** `k8s_zone_rollback.ps1` 是先 zone-down、再等 lag，被删 zone 的 lag 排不掉（推断，§6.3 第 2 步）。
- **D1 新增连接池。** D1 的 route store 会单独开一个连接池（提案 :155），要计入连接总账（§2.4）。
- **两批改动有文件重叠。** D1 第 1 批同样要改 `k8s_deploy.ps1`（提案 :373），所以 P1-2 要排在它之后（§0、§9.2）。
- **Java 仓库路径**:AGENTS §12 记的本机路径已于 2026-10-12 更正为 `D:\luyuan\xuanming-server-mmo`,本条不再是未决项。

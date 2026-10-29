# 容量模型：王者荣耀量级（S1 / S2 / S3）推算

> **2026-10-15 目标修订（负责人确认）**：当前容量目标为 **DAU 1 亿、峰值同时在线 2000 万**，覆盖本次 login / friend / chat / guild / mail / leaderboard / auction / trade 的比较与迁移。下面旧 S3 的 1000 万 PCU 和派生数字保留为历史推算，不能作为新目标的验收预算；新口径及比较要求见文末 §7。目标尚无达标实测证据。

> **目标档已确认(2026-10-13,负责人)**:S3 —— 同时在线 1000 万、日活 1 亿。下文三档中以 S3 为容量决策基准,S1 / S2 作为分阶段压测里程碑。交接与后续工作见 [handoff-launch-readiness-and-d1-20261013.md](../handoff/handoff-launch-readiness-and-d1-20261013.md)。

> **状态**：草稿，2026-10-13。本文只读推算：读了代码、配置、文档和 git，没有改动仓库里的任何文件，也没有运行编译、测试或压测（AGENTS §10.1）。**文中所有数字都是「公式 + 参数出处」推出来的，不是运行证据。三档场景是假设，目标数字要负责人确认（§6）。**
> **起因**：负责人说「我的体量和王者荣耀一样」。本文不引用王者荣耀的内部架构或未公开数字。唯一用到的公开信息是「日活过亿」的公开报道，其余参数都是本推算的假设。
> **依据**：HEAD `73b852f32` 加上工作区未提交的改动。`go/data_service`、`go/shared/placement/route.go`（未跟踪文件）、`go/db/internal/kafka/key_ordered_consumer.go`、`tools/scripts/k8s_deploy.ps1` 等文件按工作区行号引用，他人的未提交改动可能让行号漂移。本文由五路组件推算和一轮对抗复核合成，已经吸收复核的更正并统一了参数（§0.6）。
> **关联**：[tidb-phase1-and-launch-readiness.md](./tidb-phase1-and-launch-readiness.md)（§7 开服清单 L-01~L-17、§8 地理形态）、[shared-redis-split-by-purpose.md](./shared-redis-split-by-purpose.md)（G4a）、[new-player-first-enter-no-cold-read.md](./new-player-first-enter-no-cold-read.md)（L-05 / G3）、[player-route-mapping-durability-proposal.md](./player-route-mapping-durability-proposal.md)（D1）、[db-task-pipeline-bottlenecks.md](./db-task-pipeline-bottlenecks.md)、[control-plane-topic-partitioning-20260908.md](./control-plane-topic-partitioning-20260908.md)、[gate-connection-admission-control.md](./gate-connection-admission-control.md)、[global-data-layer-tidb-decision.md](./global-data-layer-tidb-decision.md)。
> **Java 版（AGENTS §12）**：容量模型本身不涉及实现。各项改造的 Java 同步要求见 §3 每条的「契约 / Java」。

---

## 0. 状态与口径

### 0.1 证据等级

- **[C]**：代码或配置事实，标了文件:行号。
- **[M]**：压测实测，来自 stress 文档或 prev-summary。
- **[D]**：转引自文档，本次没有回到代码重新核对。
- **[I]**：推断或假设。
- **量级参照**：外部常识，或仓库文档里的估算，不是本仓实测，只用来判断差几个数量级。
- **差距倍数** = 需求 ÷ 现有上限。上限没测过时，写成「对量级参照」。

### 0.2 三档场景（假设）

| | S1 | S2 | S3 |
|---|---|---|---|
| 同时在线 PCU | 100 万 | 500 万 | 1000 万 |
| 日活 DAU | 1000 万 | 5000 万 | 1 亿 |
| 出处 | 假设 | 假设 | DAU 过亿有公开报道；PCU 是假设 |

### 0.3 统一参数表

各组件初稿各用一套参数，Z² 一类的项因此彼此差 4~25 倍。本文统一用下表。

| 符号 | 含义 | 取值 | 出处与性质 |
|---|---|---|---|
| n_s | 每个 DAU 每天的会话数 | 2 | [I] |
| 峰均比 | 峰时速率 ÷ 日均速率 | 2.4 | [I] |
| T_sess | 平均会话时长 | 1800 s | [I]。和上两项自洽：DAU×n_s×2.4/86400 = PCU/1800 |
| E | 峰时进场速率（等于退场速率） | PCU/1800 | 派生 |
| T_sw | 每人换一次场景的间隔 | 300 s | [I] |
| f_x | 换场景时跨 scene 节点的比例 | 0~0.5 | [I]。跨节点会走 `HandleExitGameNode(kReleasedByTransfer)`：`scene_node_service.cpp:182` → `player_lifecycle.cpp:2737`（LOGOUT 快照）、`:2740`（退出存盘），目标节点再走 `:3555`（LOGIN 快照）[C] |
| t | 每次会话跨节点的次数 | T_sess·f_x/T_sw = 0~3 | 派生 |
| C_z | 每个 zone 的在线人数 | 45k 或 100k | 45k 是 R18/R19 的爬坡终点，不是持续容量（`stress-1zone-45k-2026-06-04-round18.md:9`）[M]。100k 是 2026-06-03 定的目标（`docs/notes/redis-oom-data-safety-qa-record.md:267`）[D] |
| Z | zone 数 | PCU/C_z | 派生 |
| c_gate | 每台 gate 的在线人数 | 4.5k~20k | 4.5k 来自设计「每 zone 10 gate」（`global-data-layer-tidb-decision.md:34`）；20k 是 `GateMaxConnections`（`bin/etc/base_deploy_config.yaml:126`）[C] |
| c_scene | 每个 scene 进程的在线人数 | 2.25k~11.25k | 2.25k 来自设计「每 zone 20 scene」；11.25k 是 R18 不移动机器人的口径（45k ÷ 4）[M] |
| 1−s | 周期存盘中真正写盘的比例 | 0.5~1.0 | 机器人实测跳过率 s=0.49~0.60（prev-summary 脏存盘段）[M]。`player_database` 的第 2 个字段是 `Transform`（`proto/common/database/mysql_database_table.proto`），脏比较是整份 `MessageDifferencer`（`cpp/libs/engine/core/utils/proto/proto_dirty_compare.h`），所以真人只要走动过就算脏 [I] |
| B | `player_database` 序列化大小 | 8~25 KB，中值 15 KB；入库 11~33 KB | [I]。入库按 base64 放大 4/3（`go/db/internal/dbguard/blob.go:73-74`）。这和 `character-appearance-identity.md:21` 说的「存原始字节」矛盾，未核实 |
| B_snap | 一份快照的大小 | 15 KB（约等于 B）~300 KB | [I]。300 KB 取自代码注释「几百 KB」（`go/data_service/internal/config/config.go:466`），没实测 |
| C_n | 常态建角速率 | DAU×2%/86400×3 | [I] |
| C_p | 开服或大活动的建角峰值 | S1 0.1~0.5 万/s、S2 0.5~2.5 万/s、S3 1~5 万/s | [I]（题设） |
| K | 开新区时新号集中进入的 zone 数 | 0.1·Z | [I]。「开新区」（新号集中到 K 个区）和「大活动」（摊到全部 Z 个区）分开算 |
| N_acc / N_reg | 累计账号数 / 累计角色数 | 7×DAU / 10×DAU | [I] |
| P_cum | 累计进过场景的玩家数 | 不低于 DAU；30 天按 2~3×DAU | [I] |
| Q | 开服时排队人数 | 0.1×PCU | [I] |
| e | 每个 DAU 每天的流水条数 | 100~1000 | [I] |

### 0.4 派生规模

| 量 | S1 | S2 | S3 |
|---|---|---|---|
| E（进场 = 退场） | 556/s | 2,778/s | 5,556/s |
| λ_sw = PCU/T_sw | 3,333/s | 16,667/s | 33,333/s |
| λ_x = f_x·λ_sw | 0~1,667/s | 0~8,333/s | 0~16,667/s |
| Z（C_z=45k / 100k） | 23 / 10 | 112 / 50 | 223 / 100 |
| N_g = PCU/c_gate | 50~222 | 250~1,111 | 500~2,222 |
| N_s = PCU/c_scene | 89~444 | 444~2,222 | 889~4,444 |
| N_cpp = N_g + N_s（不含 battle） | 139~666 | 694~3,333 | 1,389~6,666 |
| N_save（每秒真存盘次数，§2.3） | 2,222~5,556 | 1.1~2.8 万 | 2.2~5.6 万 |
| N_snap = 2E(1+t) | 1,111~4,444 | 5,556~22,222 | 1.1~4.4 万 |
| C_n / C_p | 6.9/s / 0.1~0.5 万/s | 35/s / 0.5~2.5 万/s | 69/s / 1~5 万/s |
| C_zone（开新区时单 zone 建角） | 450~5,000/s（三档同量级） | 同左 | 同左 |
| N_reg | 1 亿 | 5 亿 | 10 亿 |

### 0.5 口径注意

- **45k 不是持续容量**：
  - R18/R19 都是 18 分钟线性爬坡。汇总里只有累计计数：R18 的 conn 78,660、enter_ok 68,888，都大于 45,000 个账号（`round18.md:27`、`:105`）。
  - 没有同时在线峰值，也没有满员稳态段。
  - 单 zone 只在单机 2 gate + 4 scene 上测过（`docs/stress/stress-1zone-45k-2026-05-partition-10.md:21`）。
- **机器人负载偏轻**：
  - 只有 CastSkill 85 和 SwitchScene 15 两种动作，每 5 秒一个，没有移动，也没有 AOI（`robot/logic/ai/action.go:111-116`；`robot/etc/robot.stress-3zone-z1.yaml:20-37`）。
- **拓扑和容量分开给**：
  - etcd、gRPC 网状连接这类随进程数增长的项，按 N_cpp / N_s 算。
  - 不能拿「每 zone 10 gate + 20 scene」去套实测的 45k，否则进程数被放大约 5 倍，平方项被放大约 25 倍。
- **量级参照（都没在本仓测过）**：
  - 单主 Redis 简单命令约 10 万 ops/s（`shared-redis-split-by-purpose.md:769`）；Lua 脚本和 10~20 KB 的大 value 更低。
  - etcd 官方 benchmark 单 key 读约 10^5 QPS。那不是 range，也不是线性一致读，不能和 KV/s 直接比。
  - TiKV 单个 Region 的写入预算 B ≈ 1k~3k 事务/s。这是假设，官方没给数字（launch_verdict 未核实项）。
  - Kafka 单集群 150~200 MB/s，是行业典型值（`docs/ops/kafka-cluster-production-runbook.md:219-224`）。
  - 1 核 MySQL 做 20 KB 整行 UPDATE，约每秒几百到 2k 次 [I]。

### 0.6 相对组件初稿的更正（复核吸收）

1. **etcd 的规模变量**：全前缀 Range 和 watch 扇出的成本随**全服 C++ 进程总数 N_cpp 的平方**增长，不随 Z² 增长（发现前缀不带 zone：`etcd_helper.cpp:33-41`、`service_discovery_manager.cpp:16`、`:27-33`）。所以「拉大单 zone 容量、少开 zone」对这两项无效。
2. **SM 数据面速率**：「R19 单实例约 73/s」是 EnterGame 的速率，不是 EnterScene 的。机器人切场景只带 `scene_config_id`（`robot_ai.go:207-209`），scene_id=0 不会触发「已在当前场景」的拒绝（`player_scene_handler.cpp:106-115`）。推断 R19 时 SM 的 EnterScene 约为 45k×3%/s ≈ 1,350/s，待 Codex 从 `t*m_sm.txt` 核实。
3. **Redis 核心合计漏了两项**：
   - 切场景：每次约 10 条命令，即 10×λ_sw。
   - SM 负载刷新：每个副本都读，`load_reporter.go:231-233` 的读不门控，`:240` 只对写按 `isLeader` 门控（本次抽查）。
4. **存盘与快照漏了跨节点换场景**，即参数 t。
5. **task:result 稳态不能按「每次登录 1 次冷读」算**：父键首存后不带 TTL（`redis_client.h:20-24`、`:42-52`；2026-10-16：落码前行号，同日该文件删 SADD、加注释，行号整体后移，现按 `kSaveLuaScript` / `kSaveIfGuardLuaScript` 定位），老玩家几乎不冷读。
6. **K8s 的 Kafka broker 数**：只要不是 dev+custom 档，缺省就是 3 个（`k8s_deploy.ps1:1076-1082`）。`deploy/k8s/AGENTS.md:86` 的「刻意 replicas 1」已过时。
7. **Kafka 的 512 MiB retention.bytes 是生效的**：segment.bytes 是 32 MiB（`k8s_deploy.ps1:1188`、`:1200`）。
8. **快照消费的现状**：工作区有未提交的 G2a 改动，改成按分区并发、可配批量，但默认仍逐条（`data_service.yaml:248-251`）。并行度仍被 3 个分区封顶。
9. **route_shard 取 64 的理由**是「B 未知，且建表后不可改」，不是「16 已经证明不够」。
10. **新增遗漏项（M 类）**：
    - etcd 写入与版本压缩；
    - 登录口令哈希；
    - HTTP 入口；
    - 下行带宽；
    - NodePort 端口段；
    - battle 全局池；
    - 日志管线；
    - 跨 zone 旅行；
    - 停服窗口；
    - Kafka 与数据库连接的全局总账。

---

## 1. 结论先读

**一句话**：按现状实现，到 S1 撑不住的是「全服只有一份」的控制面和共享件：

- etcd（不自动压缩，每 2 秒全前缀 Range）；
- 共享单主 Redis（会话对账全键 SCAN，内存无上界）；
- 一套 3 broker 的 Kafka；
- 单副本 MySQL；
- 全球 3 分区串行的快照审计。

单 zone 容量不是首要矛盾。

**归属目录不是最先的瓶颈，但参数要按规模定**：

- player_route 一个角色一生只写一次。按 route_shard 打散后，S3 开服时每条尾部只有 156~781/s（R=64），排在 etcd、共享 Redis、审计之后。
- 但 route_shard、围栏契约、号段 Step、db_task / 快照分区数这类参数，建表或建 topic 之后就改不了，必须现在按目标档定。

### 1.1 最先撞墙排序（按现状实现）

排序依据：差距首次超过 1 倍的档位。同一档内，先列由代码事实决定、与假设无关的项。

| 序 | 组件 | 首次撞墙 | 差距（S1 / S2 / S3） | 性质 | 改造 |
|---|---|---|---|---|---|
| 0a | etcd 不自动压缩，配额 2 GB | **现网就有** | 推算达到配额的时间：现网 2 zone 约 2 周，S1 1~2.5 天，S2 5~12 小时，S3 2.5~6 小时。一旦 NOSPACE，发号、节点注册、gate 计数同时停 | 写入随 N_g + Z 线性增长，版本无人回收 | A0-1 |
| 0b | gate / scene 的 RLIMIT_NOFILE | **现网就有** | 每台 gate 4.5k~20k 人，对 soft 1024 是 4.4~19.5 倍 | 部署缺口（G4 未修；2026-10-18 进程侧自抬与拒启已落码、未验证，硬限待实测） | A0-2 |
| 0c | K8s MySQL：单副本，1C / 1Gi（2026-10-18 更正：requests 1Gi、limits 5Gi，2026-10-12 G2b 改） / 20Gi，max_connections 500（2026-10-18 更正：现为 1800，`deploy/k8s/manifests/infra/mysql.yaml:90`，2026-10-12 G2b 改） | **现网就有** | 2 个 zone 时连接已零余量（按旧上限 500 算，待按 1800 与连接总账重核）；S1 存盘 1.1~28 倍 | 单点 | A5（Phase 1） |
| 0d | Kafka 每台 broker 20Gi、每分区 512 MiB | **现网就有** | 保留上限折算的盘量已超出 PVC；S1 2.5~5.8 倍 | 静默删数据 | A0-3 / A4 |
| 1 | 共享 Redis：会话对账 SCAN | S1 | 3.3~7.7 / 83~187 / 333~743 倍 | O(Z·N) | A0-4 |
| 2 | 快照审计 | S1 | 1.9~22 / 9~111 / 19~222 倍；积压只能扛 0.1~96 秒；库内没有保留期 | 全球 3 分区串行 | A3 |
| 3 | 共享 Redis：核心稳态与内存 | S1 | 命令量 1.1~1.9 / 5.6~11 / 12~26 倍；常驻内存至少 0.1~0.2 TB，现有 PVC 8Gi | 单主，没有上界 | A6 |
| 4 | etcd 全前缀 Range | S1 存疑，S2 确定 | 对 10^5 参照：0.3~5.4 / 6.9~134 / 28~534 倍 | N_cpp² | A1 |
| 5 | transaction_log | S1 | 1.4~56 / 7~278 / 14~556 倍 | 全球 6 分区写单台 MySQL，无界增长 | A3 / A5 |
| 6 | Kafka 一套 3 broker | S1 | 盘写 0.12~0.74 GB/s（快照按 15 KB 计）；保留上限折算的盘量是 PVC 的 2.5~5.8 倍 | 全服共享 | A4 |
| 7 | 开服尖峰链 | S1 | task:result 扇出 0.4~4.6 倍；PickGate 一个 10 秒窗口把 4,500~50,000 个连接全落一台 gate；排队轮询 2.5 倍；WAITAOF 建角上限 1.1~12.5 倍（db_first 模式） | 确定性羊群效应、单频道广播 | A0-5/6/7、§4.1 |
| 8 | 全局服务 | S1 | match 5 倍，friend 推荐 5.6~11 倍，世界频道 2~4 倍，trade 1.3~4 倍，guild 在 K8s 上没部署 | 单 key、单 slot、无缓存 | A8 |
| 9 | 薄层存储 | S1 上沿 | mapping Redis 对单实例 25 GB 是 0.4~3.2 倍；账号目录 20~36 GB 只存在核心 Redis | 单实例全量常驻 | §4.2、A6 |
| 10 | scene_manager | S2 | 全服一个 leader 编排 Z 个 zone；负载刷新 0.8·Z·N_s GET/s，对 10 万参照是 0.01~0.08 / 0.2~2 / 0.7~7.9 倍；Prometheus 序列 13 万~149 万（S2） | Z·N_s | A1 |
| 11 | 薄层单调尾部 | S1 临界，S2 确定 | player_name owner 唯一键、idx_route_storage 的 NULL 段：0.3~5 / 1.7~25 / 3.3~50 倍 | 单尾写入 = C_p | A5 |
| 12 | 按用途拆分后的 Redis | S2 | 核心 0.65~0.82 / 3.4~5.7 / 7.1~15 倍；mapping 并入控制面后 0.46~1.1 / 2.3~5.6 / 4.6~11 倍；连接数超过 maxclients | 单主 | A6、3.0 |
| 13 | HTTP 入口、Kafka 控制面消息率 | S2 | TLS 握手；控制面消息是设计预算 500 条/s 的 9 / 44 / 89 倍 | 预算本身是假设值 | A4 |
| 14 | 规模尾项 | S3 | etcd watch 出向 0.9 GB/s；scene 主线程 JSON 解析占 2~25%；gRPC 网状 396 万条连接，每个 scene 892 个 fd，逼近 1024；NodePort 端口段占用 80%；route_shard=16 时尾部 3,125/s；Loki 摄入 0.7~1.5 倍 | 平方项和固定上限 | A1、§4.1 |

### 1.2 为什么单 zone 与归属目录不是首要矛盾

- **单 zone**：
  - 每 zone 稳态进场 25~56/s（C_z/1800），低于 R19 已验证的约 73/s。
  - 每 zone 写任务 200~1,111/s，对 80 个 worker 的纸面能力（约 2.3k~8.5k/s）只用到 5%~50%，对已证的 55~62 task/s 差 3~20 倍。
  - zone 内的管线随 Z 横向分摊，差距在于缺测量，不在结构。
- **改 C_z 只是在两组成本之间转移**：
  - 拉大 C_z，能降下 scene_manager 刷新、gRPC 网状、task:result 里的 Z 因子。
  - 但 etcd 两项随 N_cpp 增长，降不下来。
  - 单 zone 的硬上限会变紧：db_task 10 个分区不能在线扩、80 个 worker、Phase 1 每 zone 一条主键尾部、围栏 N=8、每 zone 只有 2 个 login。
- **归属目录**：建角是一生一次的写。库侧靠 route_shard 和预切就能扛住；Redis 侧的单实例全量常驻要在 §4.2 一起改。

### 1.3 不依赖任何拍板、现在就该做的五件事

1. etcd：自动压缩、配额、定期 defrag，并钉住镜像版本（A0-1）。
2. gate / scene / battle 容器补 RLIMIT_NOFILE，并加启动断言（A0-2）。（2026-10-18：已落码，未编译、未验证。进程在 `Node::Initialize` 自抬软限到 min(硬限, 1048576)，阈值是客户端面准入上限 + 4096，不够缺省拒启，运维豁免为 `NODE_OPEN_FILE_LIMIT_MODE=warn`；各节点池的硬限仍待实测，见 PROGRESS 2026-10-18 B0-4 条目。）
3. SessionReconciler 去掉全键 SCAN；删除 `dirty_keys_set` 的写入（A0-4）。（2026-10-16：`dirty_keys_set` 的写入已在代码中删除（已落码，未编译、未测试；全部 scene 换成新镜像前生产仍在写）；存量键一次性清理见 [release-checklist.md](../ops/release-checklist.md) §6.1。SessionReconciler 一项不在该次改动内。）（2026-10-18 状态，B0-1：SessionReconciler 去掉全键 SCAN 已落码，未编译、未测试：改为全服单 leader + 按 gate 实例建的会话索引，见 [player-locator-gate-session-index.md](player-locator-gate-session-index.md)；K8s 上告警生效要先接 player_locator 的 /metrics。）
4. 每个 Kafka topic 显式设 retention.bytes，按公式定 PVC；关掉旧的「一节点一 topic」路径（A0-3）。
5. 补测量（§5 的 V0）。没有测量就不下容量结论。

---

## 2. 逐组件容量表

列说明：「负载公式」里写参数出处；「现有上限」没测过时写「未测」，并给量级判断与依据；「差距」三个数依次是 S1 / S2 / S3。

### 2.1 接入层与 zone 拓扑

| 项 | 负载公式（参数出处） | S1 | S2 | S3 | 现有上限（出处） | 差距 S1 / S2 / S3 |
|---|---|---|---|---|---|---|
| zone 数 | Z = PCU/C_z | 23（10） | 112（50） | 223（100） | 多 zone 最多压过 3 zone × 15k，那轮有失败（`stress-3zone-2026-05-23-postmortem.md`）[M]；K8s 清单只有 2 个 zone（`deploy/k8s/zones.json`）[C] | 对 3 个 zone：3.3~7.7 / 17~37 / 33~74 倍 |
| gate 连接 | 每台 c_gate；N_g = PCU/c_gate | 每台 4.5k~20k，共 50~222 台 | 250~1,111 台 | 500~2,222 台 | 配置上限 20,000（`base_deploy_config.yaml:126`）；K8s 上进程从不 setrlimit，fd soft 很可能是 1024（`gate-connection-admission-control.md:143` G4，未修）[D]（2026-10-18：进程启动时已自抬软限到硬限、不够就拒启，已落码未验证；硬限仍取决于节点容器运行时，未实测）；只有一个 IO 线程（`client_message_processor.cpp:542-545`），吞吐未测 | 对 1024：4.4~19.5 倍（三档相同）；对配置上限不超过 1 倍 |
| K8s 每 zone 的 gate 台数 | C_z ÷ 台数 | — | — | — | 清单每 zone 2 台，按 20,000 一台，一个 zone 封顶 40k（`zones.json`） | C_z=45k 至少要 3 台 |
| scene 进程 | 每进程 c_scene；业务单线程，33 ms 一帧（`scene-node-threading-model.md:13`） | 89~444 个 | 444~2,222 | 889~4,444 | 不移动机器人口径 11,250 人/进程 [M]；移动 + AOI 未测 | 未测，按 `scene-node-threading-model.md:95-96` 的帧耗时判据补测 |
| 每 zone 进场速率 | 稳态 C_z/T_sess；大活动 (C_p+E)/Z；开新区 C_p/K | 稳态 25（56）/s；大活动 43~500/s；开新区 450~5,000/s | 同 | 同 | R19 约 73/s/zone，preload_failed 为 0（`docs/stress/prev-summary.txt:19-20`），是爬坡速率，不是饱和点 [M] | 稳态 0.34~0.77 倍；大活动 0.6~6.8 倍；开新区 6~68 倍，只能靠排队和限流兜 |
| PickGate 集中度 | 按 10 秒才刷新一次的 player_count 确定性地取最小那台（`gatetoken.go:84-101`；gate 每 10 秒写 etcd，`cpp/nodes/gate/main.cpp:407-411`），所以每个 10 秒窗口有 λ_zone×10 个新连接落到同一台 | 稳态每窗口 250~560 个；开新区 4,500~50,000 个 | 同 | 同 | 本应 10 台均摊。候选集只做地址去重和排空过滤，没有存活或新鲜度过滤（`servicecontext.go:310-317`）；gate 键 TTL 180 秒（`base_deploy_config.yaml:6`），死掉的 gate 最长 180 秒内仍会被选中（「黑洞」）[C][I] | 集中度 10 倍；开新区一个窗口就能超过单台 20,000 / 1024 |
| login 的 AssignGate | 每次 1 次 etcd Range，不缓存（`servicecontext.go:292-297`）+ 1 次 Redis MGET（`gatedrain.go:77`） | 稳态 556/s；开服 1k~5k/s | 2,778/s；5k~2.5 万/s | 5,556/s；1~5 万/s | 未测 | 计入 etcd |
| 口令哈希 Argon2id（PasswordAuth，缺省关） | 每实例需求 E·f_pwd/(2Z)；能力 = min(2 并发 / t_kdf, 1 核 / t_cpu)（`password_provider.go:24-35`；`login.yaml:356-373`） | 稳态每实例 12~28·f_pwd 次/s | 同 | 同 | t_kdf 未测；按单次 50~150 ms 假设，约 7~20 次/s/实例；每个 login 1 CPU（`login.yaml:89`） | f_pwd≈1 时：稳态 0.6~4 倍，开新区 10 倍以上 |
| HTTP 入口（Ingress + Java gateway，每 zone 2 副本 × 500m，`gateway.yaml:24`、`:70`） | 2E + Q/2s（排队轮询间隔 2 秒，`go/login/internal/config/config.go:276`） | 稳态 1.1k/s；开服再加 5 万/s | 5.6k；再加 25 万 | 1.1 万；再加 50 万 | 未测；TLS 可选（`k8s-client-entry.md:174`、`:311`） | 开 TLS 且不复用连接：按每次握手 0.5~2 ms CPU 假设，S3 开服要 250~1,000 核 [I] |
| 下行带宽 | PCU×b_down，b_down 取 2~10 KB/s/人 [I] | 16~80 Gbps | 80~400 Gbps | 160~800 Gbps | 未测：R18 的机器人不移动，也没有 AOI | 未测；scene→gate 的内网流量再加一份 |
| NodePort 端口段 | N_g（另加 battle）对比 30000-32767，共 2,768 个（`k8s_deploy.ps1:140-144`、`:203-207`） | 2~8% | 9~40% | 18~80% | 2,768 | S3 逼近上限；改用 LoadBalancer 就是每台 gate 一个云 LB |

### 2.2 控制面：etcd、scene_manager、gRPC 网状连接、Kafka 控制面

| 项 | 负载公式（参数出处） | S1 | S2 | S3 | 现有上限（出处） | 差距 S1 / S2 / S3 |
|---|---|---|---|---|---|---|
| etcd 全前缀 Range | 每个 C++ 节点每 2 秒（`service_discovery_manager.cpp:16`）对 10 个前缀（`base_deploy_config.yaml:39-49`，含 BattleNodeService）各做一次全前缀线性一致 Range（`etcd_helper.cpp:33-41`，前缀不带 zone；分配键也在同一前缀下，`etcd_manager.cpp:60-69`）。KV/s ≈ N_cpp·K/2，其中 K ≈ 2(N_cpp + 6Z)；Range 次数 = 5·N_cpp | 2.8 万~54 万 KV/s；695~3,330 次/s | 69 万~1,335 万；3,470~16,665 | 276 万~5,336 万；6,945~33,330。SceneNodeService 前缀单次响应在 S3 上沿约 2.7 MB，接近 gRPC 默认的 4 MiB 接收上限 [I] | 未测。etcd 3 副本，requests 100m、没有 limits（`deploy/k8s/manifests/infra/etcd.yaml:181-183`）；线性一致读每次都要 leader 做 ReadIndex，3 个成员不能按 3 倍分摊；现网 2 zone 约 288 KV/s | 对 10^5 参照：0.3~5.4（存疑）/ 6.9~134 / 28~534 倍 |
| etcd watch 扇出 | gate 每 10 秒 Put 一次 NodeInfo；所有 C++ 节点都 watch 全服前缀，并带 prev_kv（`etcd_service.cpp:278-284`），每个事件两份 KV。事件/s = (N_g/10)·N_cpp；每节点每秒解析 JSON 约 (N_cpp + 6Z) + N_g/10 次 | 700~1.5 万事件/s | 1.7 万~37 万 | 6.9 万~148 万；出向上沿约 0.9 GB/s；每节点每秒解析 2.0k~8.2k 次 | 未测；现网 2 zone 约 4.8 事件/s | 若单次解析 10~30 µs [I]，S3 吃掉 scene 主线程 2~25% 的时间 |
| etcd 写入与版本压缩（新增） | 写入/s ≈ N_g/10 + 4Z：gate NodeInfo，加上 login 和 scene_manager 每进程每秒一次 snowflake 水位 Txn（`go/shared/snowflakealloc/allocator.go:926`、`:1427`） | 45~114/s；每天 390 万~985 万个版本，0.8~2 GB（按每个版本约 200 B [I]） | 225~559/s；3.9~9.7 GB/天 | 450~1,114/s；7.8~19 GB/天 | `etcd.yaml` 和 `docker-compose.yml` 都没设 `ETCD_AUTO_COMPACTION_*`（本次抽查）。etcd 缺省不自动压缩；bitnamilegacy 镜像有没有预置缺省值未核实。配额 2 GB（`etcd.yaml:190` 注释）；镜像是 `:latest`（`etcd.yaml:114`，本次抽查） | 推算达到 2 GB 的时间：现网约 2 周 / 1~2.5 天 / 5~12 小时 / 2.5~6 小时 |
| etcd 注册规模与 node_id | 每进程约 2~3 个键；node_id 17 bit（`node_util.h:124-131`）；续约每秒 N_cpp 次 | 键 0.3k~2k | 1.4k~10k | 2.8k~20k | 键数和号空间远低于上限；续约吞吐未测；NodeTTL 180 秒 | 存储和号空间没有缺口 |
| scene_manager 编排：全服只有一个 leader | 全服共用一把锁（`go/scene_manager/internal/config/config.go:78-80`）；fullSync 对所有 zone 跑一遍（`load_reporter.go:703-712`）；每 300 秒全 zone rebalance（`scene_manager_service.yaml:209`） | 一个进程编排 10~23 个 zone | 50~112 | 100~223 | 只在 3 个 zone 以内跑过 | 3.3~7.7 / 17~37 / 33~74 倍；单点 |
| scene_manager 负载刷新 | 每个副本每 5 秒对全服每个 scene 节点各做 2 次串行 GET（`load_reporter.go:31-32`、`:231-233`）→ 0.8·Z·N_s GET/s；Prometheus 序列 6·Z·N_s（`metrics.go:37-51`） | 712~8.2k GET/s；序列 0.5 万~6.1 万 | 1.8 万~19.9 万；13 万~149 万 | 7.1 万~79.3 万；53 万~595 万；每副本每拍串行 1,778~8,888 次，按 0.2~0.5 ms 往返要 0.4~4.4 秒，逼近 5 秒周期 | 共享单主（`k8s_deploy.ps1:3731-3735`） | 对 10 万参照：0.01~0.08 / 0.2~2 / 0.7~7.9 倍 |
| scene_manager 数据面（EnterScene） | 每实例 (E + λ_sw)/(2Z)：按 playerId 在全服 SM 里挑一个（`node_utils.cpp:239-256`） | 85~194/s | 同 | 同 | R19 推断约 1,350/s，待核实（§0.6 第 2 条）；K8s CPU limit 500m（`scene-manager.yaml:81`） | 推断成立时约 0.06~0.14 倍 |
| scene 到 SM / DS 的 gRPC 网状连接 | scene 连全服所有 SM 和 DS 实例（`cpp/nodes/scene/main.cpp:86`；二者不是 zone-scoped，`node_util.cpp:112-127`），两类每 zone 各 2 副本。连接总数 4Z·N_s；每个 scene 出连 4Z 条 | 3.6k~4.1 万条；每个 scene 40~92 条 | 8.9 万~99.5 万；200~448 | 35.6 万~396 万；400~892 | 代码注释假设「实例数是个位数」（`node_utils.cpp:246`）；fd soft 可能 1024（推断与 G4 同） | 每个 scene 的 fd：0.05~0.1 / 0.2~0.45 / 0.4~0.88 倍 |
| Kafka 控制面 gate-cmd / scene-cmd（P=256，不可变，`control-plane-topic-partitioning-20260908.md:82`） | 速率 ≈ 2E + λ_sw：每次 EnterGame 一条 BindSession（`entergamelogic.go:712`），每次 EnterScene 一条 RoutePlayer（`enterscenelogic.go:1338`）；读放大 = N_g/256、N_s/256 | 4,444 条/s；读放大 0.2~0.9 / 0.3~1.7 | 2.2 万；1~4.3 / 1.7~8.7 | 4.4 万；2~8.7 / 3.5~17.4 | 设计预算 500 条/s、读放大约 390（同文 :167-178），预算本身是假设；broker 数见 §0.6 第 6 条 | 读放大不到 0.05 倍，P=256 够用；消息速率对预算 9 / 44 / 89 倍 |
| Kafka 旧的一节点一 topic | `DisableLegacyPerNodeTopic=false`（`base_deploy_config.yaml:104`），每个 gate / scene 各一个 topic 和一个消费组，共 N_cpp 个 | 139~666 | 694~3,333 | 1,389~6,666 | 设计文档自己判定撑不住（同文 :44-52） | 目标是 0 |

> 状态注（2026-10-16，B0-5）：上表「scene_manager 负载刷新」一行已用「领导者按 zone 批量刷新 + 登记集合替代 SCAN」缓解，已落码，未编译、未测试。按 §7 的 2000 万在线口径（N_s 取旧 S3 的两倍，1,778~8,888）推算、未实测：刷新读降到 0.4·N_s GET/s，约 711~3,556；跟随者为 0；领导者每拍约 8~29 次 pipeline 往返（读 ⌈N_s/512⌉ 批 + 写 ⌈(N_s+Z)/1024⌉ 批 + 1 条 SADD；有读失败时每 zone 多一条 ZADD NX）；每节点序列 3·N_s，只由领导者导出；nodes_by_role 约 12·Z。字面的「每个副本只读本 zone」、按 zone 选主与 watch 收窄仍归 A1。详见 scene-creation-architecture.md「负载刷新与 zone 登记集合(2026-10-16)」。

### 2.3 存盘管线

| 项 | 负载公式（参数出处） | S1 | S2 | S3 | 现有上限（出处） | 差距 S1 / S2 / S3 |
|---|---|---|---|---|---|---|
| 每秒真存盘次数 N_save | PCU(1−s)/300：周期 300 秒，按 playerId%300 每秒摊开（`redis.cpp:121`）；+ E：退场强制存盘（`player_lifecycle.cpp:2740`）；+ λ_x(1−s)：跨节点退出存盘。不含战斗结算和资产指令触发的存盘（`player_battle.cpp:378`；`asset_op_system.cpp:222-227`） | 2,222~5,556 | 1.1 万~2.8 万 | 2.2 万~5.6 万 | — | — |
| 每 zone 写任务 | 2·N_save/Z | C_z=45k：200~500/s；100k：444~1,111/s（三档相同） | 同 | 同 | 已证 55~62 task/s（R19，lag=0，不是饱和值）[M]；纸面 80 个 worker（`go/db/etc/db.yaml:19`、`:22`）约 2.3k~8.5k/s：2.3k 按 R6 单任务 35 ms 推，8.5k 没有推导（`db-task-pipeline-bottlenecks.md:211`） | 对已证 3~20 倍；对纸面 0.05~0.5 倍 |
| DBTask 与 SQL 条数 | DBTask = 2·N_save（两张子表各一条，`player_lifecycle.cpp:3740-3741`）。`player_database_1` 正常对局里只有 player_id（本次抽查 proto），生产环境不开压测探针（`:3649`），每次仍照发。Save 链可能 1~4 条语句（未核实，db-task-pipeline-bottlenecks §6.1）。SQL ≈ (2~5)·N_save | 写任务 4,444~11,111/s；SQL 0.4 万~2.8 万/s | 2.2 万~5.6 万；2.2 万~13.9 万 | 4.4 万~11.1 万；4.4 万~27.8 万 | — | 一半写任务几乎是空的 |
| go/db 写路径打到 Redis | 11×DBTask（Kafka 来源、owner_epoch>0 的写；重试来源是 13 次，db-task-pipeline-bottlenecks §6.2）；其中一次是整行 SET 子表缓存（`key_ordered_consumer.go:846-857`） | 4.9 万~12.2 万 ops/s；整行 SET 18~139 MB/s | 24 万~61 万；89~694 MB/s | 49 万~122 万；178 MB/s~1.39 GB/s | 共享单主（`k8s_deploy.ps1:3456`）；10 万参照 | 0.5~1.2 / 2.4~6.1 / 4.9~12 倍 |
| Kafka db_task 入流量 | N_save×B。两端都没开压缩（`kafka_producer.cpp:59-76`；login CompressionType 是 0，`login.yaml:496`）；RF=3（`kafka.yaml:25`），所以盘写是 3 倍 | 18~139 MB/s；盘写 53~417 MB/s | 89~694 MB/s；0.27~2.1 GB/s | 178 MB/s~1.39 GB/s；0.53~4.2 GB/s | 所有 zone 共用一套 3 broker，每台 2 CPU / 2Gi（`kafka.yaml:345-349`）；已证约 0.6 MB/s（`PROGRESS.md:7120`）；参照 150~200 MB/s | 入流量对参照：0.1~0.9 / 0.4~4.6 / 0.9~9.3 倍 |
| db_task 实际保留时长 | topic 只设了 retention.ms（`topic_init.go:114-125`），实际受 broker 每分区 512 MiB 封顶（`k8s_deploy.ps1:1199` → `kafka.yaml:277`）。T_buf = 10×512 MiB ÷ 每 zone 字节率 | C_z=100k：6 分钟~50 分钟；45k：14 分钟~1.9 小时（三档相同） | 同 | 同 | 名义保留 24 小时（`k8s_deploy.ps1:1197`） | 名义比实际长 13~240 倍。消费从 OffsetOldest 开始（`key_ordered_consumer.go:889`），被删的是中间的整行写，后续写会覆盖。真正受影响的是「最后一次存盘恰好落在被删段、之后再没存过盘」的玩家：DB 落后于 Redis，Redis 再丢才真丢档 |
| Kafka 盘封顶 | Z×10 个分区×512 MiB×RF 3 | 150~345 GiB | 750 GiB~1.6 TiB | 1.5~3.3 TiB | 3 台 × 20Gi = 60 GiB（`kafka.yaml:363`），快照、流水、控制面也占这块盘 | 2.5~5.8 / 12.5~28 / 25~56 倍 |
| MySQL（K8s 现状） | 存盘 N_save；提交 (2~5)·N_save；binlog ≈ 2×B_row×N_save。配置：单副本 1C / 1Gi（2026-10-18 更正：requests 1Gi、limits 5Gi，2026-10-12 G2b 改），数据和 binlog 同在一块 20Gi 盘，sync_binlog=1 且 innodb_flush_log_at_trx_commit=1，binlog FULL，保留 7 天（`mysql.yaml:25`、`:56-67`、`:74`、`:82`、`:159-160`） | 存盘 2,222~5,556/s | ×5 | ×10 | 约每秒几百到 2k 次 [I]；max_connections 500（2026-10-18 更正：现为 1800，`deploy/k8s/manifests/infra/mysql.yaml:90`，2026-10-12 G2b 改） | 存盘 1.1~28 / 5.6~139 / 11~278 倍；单个 10 万人的 zone 约 10~70 分钟写满 20Gi 的 binlog 盘 |
| TiDB（Phase 1 之后） | 逻辑写 W = N_save×B_row（11~33 KB）；Raft 3 副本；MVCC 旧版本按 gc_life_time 10 分钟保留，约 W×600×3；新号首存是 INSERT，写主键索引尾部（Phase 1 有 Z 条尾部，Phase 2 只有 1 条） | W 24~183 MB/s；×3 后 73~550 MB/s；MVCC 44~330 GB | ×5 | W 0.24~1.83 GB/s；×3 后 0.73~5.5 GB/s；MVCC 0.44~3.3 TB | 还没部署；TiKV 可以加；单 Region 写上限未测 | 规格见 §4.5 |

### 2.4 读档与 task:result

| 项 | 负载公式（参数出处） | S1 | S2 | S3 | 现有上限（出处） | 差距 S1 / S2 / S3 |
|---|---|---|---|---|---|---|
| 新号首次进场冷读 | 2·C：每张子表一条 Kafka 读任务（`ensure_player_all_data_async.go:157-207`），玩家同步等待，每任务上限 8 秒（`entergamelogic.go:380`）。打开 NewPlayerSeed 后归零；它默认关，K8s 也没渲染这个键（`new-player-first-enter-no-cold-read.md` §1、§13，本次抽查） | 常态 14/s；开服 2k~1 万/s | 70；1 万~5 万 | 138；2 万~10 万 | 每 zone 约 80/t_r = 1.6k~2.5k 读/s（t_r 取 31.8~50 ms，R19 / deep-dive），和写任务共用 worker；Phase 2 走额外池 16 条，约 320 读/s | 开新区单 zone 需求 2·C_zone = 0.9k~1 万/s：对纸面 0.4~6 倍，Phase 2 时 3~31 倍 |
| task:result 扇出 | 所有结果 PUBLISH 到同一个频道 `task:result:notify`（`key_ordered_consumer.go:42`），每个 login 都订阅（`task_result_dispatcher.go:32`），login 每 zone 2 副本。投递数 = 读任务/s × 2Z；稳态读任务 ≈ 2C_n | 稳态 276~635/s；开服 4 万~46 万/s | 0.7 万~1.6 万；100 万~1,120 万 | 2.8 万~6.2 万；400 万~4,460 万 | 单主 10 万参照。载荷只有 task_id（`key_ordered_consumer.go:404-406`），每个 1 CPU 的 login 都要处理全部通知 | 开服：0.4~4.6 / 10~112 / 40~446 倍；login 的 CPU 是第二个瓶颈 |
| 老玩家冷读 | 2·E·f_miss。现状 f_miss≈0，代价是父键常驻内存（§2.5）；一旦加 TTL，或 Redis 故障切换，f_miss 趋近 1 | f_miss=1 时 1,111/s | 5,556 | 1.1 万 | 同上 | 均摊到每 zone 48~111/s，能吸收；故障切换后的全量冷读加重试雪崩，目前没有预算 |

### 2.5 共享 Redis

**核心实例的命令量分解（拆分前，稳态）**

| 来源 | 公式（出处） | S1 | S2 | S3 |
|---|---|---|---|---|
| C++ 存盘 EVALSHA | N_save（`player_lifecycle.cpp:3672-3683`） | 0.2~0.6 万 | 1.1~2.8 万 | 2.2~5.6 万 |
| go/db 写路径 | 11×2×N_save（§2.3） | 4.9~12.2 万 | 24~61 万 | 49~122 万 |
| 进退场链路 | ≈43×E（静态盘点 login / SM / C++，未逐条计数） | 2.4 万 | 11.9 万 | 23.9 万 |
| 切场景 | ≈10×λ_sw（SM 每次 EnterScene） | 3.3 万 | 16.7 万 | 33.3 万 |
| SM 负载刷新 | 0.8·Z·N_s（§2.2） | 0.07~0.82 万 | 1.8~19.9 万 | 7.1~79.3 万 |
| 建角常态 | ≈29×C_n | 0.02 万 | 0.1 万 | 0.2 万 |
| **合计** | | **11~19 万** | **56~112 万** | **116~264 万** |
| 对 10 万参照 | | 1.1~1.9 倍 | 5.6~11 倍 | 12~26 倍 |

以上不含：会话对账、friend 登录时的 MGET（§2.8）、task:result（§2.4）、排队、浪涌。

**其余各项**

| 项 | 负载公式（参数出处） | S1 | S2 | S3 | 现有上限（出处） | 差距 S1 / S2 / S3 |
|---|---|---|---|---|---|---|
| 会话对账 SessionReconciler | 每个 zone 的每个 locator 实例每 60 秒 SCAN 一遍 `player:session:*` 全键空间，再对每个键 GET 一次（`session_reconciler.go:42-44`、`:123`，本次抽查；`:196`）。2Z 个实例，GET = 2Z·N/60 | 33 万~77 万 GET/s | 833 万~1,870 万 | 3,333 万~7,430 万 | 实际跑不完，会退化成 2Z 条串行循环，长期占住执行线程。一轮跑不完时，「连续两轮缺席才清理」被拉长到小时级，gate 崩溃后的 ONLINE 会话清理随之失效；存活集用的是全服 Range（`:155`） | 3.3~7.7 / 83~187 / 333~743 倍 |
| 按用途拆分（G4a）后的核心 | 存盘 EVALSHA + 进退场 + 切场景 + SM 刷新 + 建角 + 子表缓存 SET（第 3 批不做时） | 6.5~8.2 万 | 34~57 万 | 71~153 万 | 单主 | 0.65~0.82 / 3.4~5.7 / 7.1~15 倍；S1 没有给浪涌留出余量 |
| 拆分后 redis-mapping（并入控制面） | 10×DBTask + 2E + 3C_n | 4.6~11.2 万 | 23~56 万 | 46~112 万 | 单主；go/db 必须读主（`go/db/internal/config/config.go:57-58`） | 0.46~1.1 / 2.3~5.6 / 4.6~11 倍 |
| 排队轮询 | 2.5·Q：每次轮询 5 条命令（`queue.go` 的 `Lookup`），客户端间隔 2 秒 | 25 万 | 125 万 | 250 万 | redis-queue 单主；排队默认关 | 2.5 / 12.5 / 25 倍 |
| 全服重连浪涌 | N/120×35：假设全服在 120 秒内重新进场，只算进场链路 | 29 万 | 146 万 | 292 万 | 同核心 | 2.9 / 15 / 29 倍 |
| WAITAOF 建角上限（D1 db_first） | ≈ 映射连接池总数 ÷ 0.5 秒 = 2Z×10/0.5 = 40Z。每 Pod 池 10 的来历：data-service CPU limit 500m（`data-service.yaml:110`），automaxprocs（`go.mod:94`）使 GOMAXPROCS=1，go-redis 缺省 PoolSize=10；go-zero 是否另行覆盖未核实 | 400~920/s | 2,000~4,480 | 4,000~8,920 | appendfsync everysec 下平均等约 0.5 秒 | 开服 C_p 对此上限 1.1~12.5 倍；WAITAOF 阻塞的是同一个池，同实例的路由 MGET 会一起被饿住 |
| 连接数 | C++ 每进程 1 条，加上 Go 各服务的连接池（仓库里没设 PoolSize） | 2.5k~6.2k | 1.2 万~3 万 | 2.5 万~6 万 | maxclients 没配，缺省 10000（2026-10-18 更正：已显式设 40000，`deploy/k8s/manifests/infra/redis.yaml:121`，实际生效值受容器 nofile 硬限约束，未实测） | 0.25~0.6 / 1.2~3 / 2.5~6 倍（按旧上限 10000 算；按 40000 约 0.06~0.16 / 0.3~0.75 / 0.6~1.5 倍，生效值受 nofile 硬限约束、未实测） |
| 核心常驻内存 | P_cum×(V+0.3 KB)：父键首次存盘后不再过期、owner_epoch 永不删除、dirty_keys_set 只写不读（`redis_client.h:20-24`、`:42-52`；2026-10-16：写入已在代码中删除（已落码，未编译、未测试；全部 scene 换成新镜像前生产仍在写），存量待按 release-checklist §6.1 清理；行号为落码前快照，现按 `kSaveLuaScript` / `kSaveIfGuardLuaScript` 定位） | 至少 0.1~0.2 TB；30 天 0.2~0.6 TB | 至少 0.5~1 TB | 至少 1~2 TB | 没设 maxmemory；requests 512Mi，PVC 8Gi（`redis.yaml:136-155`）；单实例经验上限是几十 GB | 对 8Gi：13~25 / 64~127 / 129~254 倍 |
| 子表缓存（1 小时 TTL，`k8s_deploy.ps1:3458`） | ≈3N×V | 30~60 GB | 150~300 GB | 300~600 GB | 与核心同实例 | 叠加在核心之上 |
| AOF 重放 | 数据集 ÷ 重放速率（按 0.2~0.5 GB/s 假设） | 3~17 分钟 | 更长 | 更长 | startupProbe 只给 300 秒（`redis.yaml:111-120`） | S1 已超过探针时间，有重启循环风险 |
| C++ 客户端 | 每进程 1 条 muduo hiredis 异步连接（`node.cpp:557`；`redis_manager.h:25-28`）。不处理 MOVED/ASK，不发 AUTH，地址只在启动时解析一次；业务依赖同一连接上的执行顺序：存盘后紧跟一条 PING，PING 回来就判定已落盘（`player_battle.cpp:385-393`） | — | — | — | — | 集群化和托管 Redis 的硬阻塞 |

> （2026-10-18 状态，B0-1，上表原文保留）「会话对账 SessionReconciler」一行描述的是旧实现。新实现已落码，未编译、未测试：每拍约 3 条 Redis 命令 + N_g 条 SCARD（pipeline）+ 2 次 etcd 读；补建 / 审计是限速的全键扫描，目标规模约 9 小时一遍，平均约 380 条命令/s（推算）；大面积 gate 故障时持续转换约 500/s，丢一个可用区约 3.7 小时转完（推算）；索引另占核心常驻内存约 1~1.6 GB（推算），应计入本节核心内存账。见 [player-locator-gate-session-index.md](player-locator-gate-session-index.md) §7。

### 2.6 全球薄层（建角路径）

| 项 | 负载公式（参数出处） | S1 | S2 | S3 | 现有上限（出处） | 差距 S1 / S2 / S3 |
|---|---|---|---|---|---|---|
| 号段 player 行 FOR UPDATE | C_p/step（`id_segment_store.go:183-229`）；初始 Step=100，按 Leaf 翻倍到 MaxStep=1000（`login.yaml:189-191`） | step 100：10~50 次/s；step 1000：1~5 | 50~250；5~25 | 100~500；10~50 | 单行串行；TiDB 约 100~300 tx/s [I] | step 100 冷启动时 S3 1.7~5 倍；step 1000 没有差距 |
| 热点 login 的预取裕量 | PrefetchAt(0.1)×step/c，其中 c = C_zone/2 = 225~2,500/s（`idsegment/client.go:240`、`:253-257`） | step 1000：40~440 ms | 同 | 同 | 领段 p99 未测；FetchTimeout 3 秒 | step 100 时只有 4~44 ms，Next 会阻塞；MaxStep 改成 10000 后为 0.4~4.4 秒 |
| player_name 主键 PK(name_norm) | W = C_p×(1.1~1.5)，含重名重试。建表只预切了行数据，PK 索引在新表上只有 1 个 Region（`schema.go:147-155`；预切是 L-11 待办） | 1,100~7,500/s | 5,500~3.75 万 | 1.1 万~7.5 万 | B 1k~3k [I] | 不预切时：0.4~7.5 / 1.8~38 / 3.7~75 倍 |
| uk_player_name_owner(player_id) 单调单尾 | 尾部写入 = C_p（`schema.go:153`），在建角同步路径上 | 1k~5k | 5k~2.5 万 | 1 万~5 万 | B 1k~3k [I] | 0.3~5 / 1.7~25 / 3.3~50 倍 |
| player_route 的 PK 与 idx_route_updated | 每条尾部 = C_p/R（`route.go:60`，现 R=16） | R16：63~313/s；R64：16~78 | R16：313~1,563；R64：78~391 | R16：625~3,125；R64：156~781；R256：39~195 | B 1k~3k [I]；建表后不可改 | R16 时 S3 上沿是 B 的 1.04~3.1 倍；R64 时三档都不超过 0.78 倍 |
| idx_route_storage 的 NULL 段（PinOnCreate=false） | 新号 storage_id 为 NULL，(NULL, s, id) 的 16 条尾部没预切，同一个 Region 承接全部写入，即 C_p（`route_tidb.go:157-158`；提案 HR5） | 1k~5k | 5k~2.5 万 | 1 万~5 万 | B | 和 owner 尾部同级；改 R 无效 |
| 合服围栏 zone_merge_fence（W1 的 R2 FOR UPDATE） | 每 zone 能力 = N/T_lock（N 缺省 8、上限 64，`route.go:32-41`；锁等待封顶 1 秒、1205 不重试，`route_store.go:86`、`:880-892`） | 开新区单 zone 需求 450~5,000/s | 同 | 同 | TiDB 上 N=8 约 800~1,600/s（T_lock 取 5~10 ms [I]） | 最高 3~6 倍；N=64 时不超过 0.8 倍 |
| route_meta 的 R1 单行读 | 每次 W1 一次普通点读，即 C_p（`route_store.go:43-47`） | 1k~5k | 5k~2.5 万 | 1 万~5 万 | 单 Region leader 点读，量级约每秒万级 [I]；没设 CACHE | S3 上沿 1~5 倍；设成缓存表后为 0 |
| mapping Redis 内存 | 第 1a 批三键 N_reg×0.1~0.3 KB；并入控制面后 N_reg×0.3~0.8 KB。契约是单实例、全量常驻、noeviction（`route.go:124`） | 10~30 GB；并入后 30~80 GB | 50~150 GB；150~400 GB | 100~300 GB；300~800 GB | 单实例宜在几十 GB 以内（按 25 GB 参照）[I] | 0.4~3.2 / 2~16 / 4~32 倍 |
| 账号角色目录 | N_acc×0.15~0.3 KB + N_reg×0.1~0.15 KB。只在核心 Redis，没有 TTL，也没有 DB 副本（`loginlogic.go:321-349`；`createplayerlogic.go:307-316`） | 20~36 GB | 100~180 GB | 205~360 GB | 与 PlayerAllData 同实例 | 内存叠加到核心；持久性没有任何冗余 |
| 全局库连接 | 2Z×(3×5 + 20 + 20) = 110Z（db_first）；redis 模式 70Z（`data_service.yaml:97`、`:110`、`:175`） | 1,100~2,530 | 5,500~1.2 万 | 1.1 万~2.5 万 | MySQL 500（2026-10-18 更正：现为 1800）；TiDB 见 §4.5 | 2.2~5 / 11~25 / 22~49 倍（按旧上限 500 算；按 1800 约 0.6~1.4 / 3.1~6.7 / 6.1~13.9 倍） |
| 单调 player_id 索引族 | 每条没打散的单调索引，尾部写入 = C_p | 1k~5k | 5k~2.5 万 | 1 万~5 万 | 已处理：player_route。未处理：name owner、Phase 2 三张玩家主表的主键、player_snapshot 的 player_id 索引等，至少 5 条 | Phase 2 合表后尾部从 Z 条变成 1 条 |

### 2.7 审计（快照与流水）

| 项 | 负载公式（参数出处） | S1 | S2 | S3 | 现有上限（出处） | 差距 S1 / S2 / S3 |
|---|---|---|---|---|---|---|
| 快照条数 | N_snap = 2E(1+t) + C。新号首登也会发一份空档 LOGIN 快照（`player_lifecycle.cpp:2040-2054` → `:3555`）；停服时一次发出 PCU 份 | 1,111~4,444/s；开服再加 1k~5k | 5,556~22,222；再加 5k~2.5 万 | 1.1 万~4.4 万；再加 1 万~5 万；停服时 1000 万份 | 全球 3 个分区、一个消费组（`data_service.yaml:241-242`）。默认逐条 INSERT 后同步提交（`snapshot_consumer.go:31`、`:73-113`）；工作区未提交的 G2a 按分区并发、批量可配，默认仍为 1（`data_service.yaml:248-251`）。按每条 5~15 ms 推断，全球约 200~600 条/s | 1.9~22 / 9~111 / 19~222 倍 |
| 快照日量与存量 | 每日条数 = 2(1+t)×n_s×DAU；字节 = 条数×B_snap | 4,000 万~1.6 亿条/日；0.6~2.4 TB/日（15 KB），12~48 TB/日（300 KB） | 2 亿~8 亿；3~12 TB，60~240 TB | 4 亿~16 亿；6~24 TB，120~480 TB | 库内没有保留期：DeleteOldSnapshots 没有生产调用方（`snapshot_store.go:854`）；「30 天」只是 Kafka 的 retention.ms（`data_service.yaml:243`）；source=1 的行没有生产读路径（`rollback_logic.go:1061`；`data_service_role_and_scope.md:72`） | 写进单副本 20Gi（含同盘 binlog），S1 下限约半小时写满 |
| 快照 topic 的实际积压窗口 | 3×512 MiB ÷ (N_snap×B_snap) | 1.2~96 秒 | 0.24~19 秒 | 0.12~9.6 秒 | 名义 30 天 | 差 4~6 个数量级；超过约 1 MB 的快照发送失败，只打 LOG_ERROR（`snapshot_system.cpp:101-112`） |
| transaction_log 速率 | 峰值 = DAU×e×2.4/86400 | 2.8 万~27.8 万行/s | 13.9 万~139 万 | 27.8 万~278 万 | 全球 6 个分区、6 条串行循环，每批 200 行（`txlog_consumer.go:41-56`），写同一台单副本 MySQL；推断约 0.5~2 万行/s | 1.4~56 / 7~278 / 14~556 倍 |
| transaction_log 存量与查询 | 每日行数 DAU×e，每行约 300 B | 0.3~3 TB/日 | 1.5~15 TB | 3~30 TB | 没有清理，也没有分区表；QueryByItemUUID 没有对应索引（`transaction_log_store.go:219-230`）；QueryLog 每次先跑 COUNT(*)（`:180`） | 无界增长。换 TiDB 后，(tx_type, ts) 和 (item_config_id, ts) 都是「低基数前缀 + 单调时间」的尾部，S3 单条尾部可达 10^5/s 量级（`rollback_database_table.proto:19-23`） |
| 审计和同步路径在同一个库 | player_snapshot、transaction_log 与 player_name、id_segment、player_route 都在 SnapshotMySQL（`data_service.yaml:91-176`） | — | — | — | — | db_first 模式下 W1 的 p99 会被审计写拖高 |

### 2.8 全局服务

| 项 | 负载公式（参数出处） | S1 | S2 | S3 | 现有上限（出处） | 差距 S1 / S2 / S3 |
|---|---|---|---|---|---|---|
| friend 登录拉在线态 | E×F 个键，对 player:session 做 MGET（F 取 50~200，`session_reader.go:101`） | 2.8 万~11.1 万键/s | 13.9 万~55.6 万 | 27.8 万~111 万 | 在核心 Redis 闭包内 | 要计入核心预算 |
| friend 推荐 | 0.1×E 次/s；每次 0.1~0.2 秒 MySQL 时间（`recommend_repo.go:200-216`，本机 F=300 实测） | 56/s | 278 | 556 | 1 核 MySQL 推断约 5~10 次/s；全局 2 副本 × 池 20 | 5.6~11 / 28~56 / 56~111 倍 |
| chat 世界频道 | 写 PCU×0.5/3600；读 PCU/10 秒（纯拉模型），读写都打同一把 key `chat:{world}:log`（`chat_logic.go:8-15`、`:40`、`:88-90`） | 写 139/s；读 10 万/s | 694；50 万 | 1,389；100 万 | redis-match-cluster 每个 Pod 500m、maxmemory 512mb、volatile-lru（`redis-match-cluster.yaml:116-120`），与 match 共用；单 key 约 2.5~5 万 ops/s [I] | 2~4 / 10~20 / 20~40 倍；这是产品形态问题 |
| match | 入队 PCU×0.5/600；所有队列压在 {mq} 一个 slot 上（`cross-zone-matchmaking.md:92`） | 833 人/s | 4,167 | 8,333 | 设计预算 1 万人/分钟（`:208-209`）；单队列串行弹组，10 人组按 RTT 0.5~1 ms 推断约 450~900 人/s（`matcher.go:590-690`） | 对设计 5 / 25 / 50 倍；最热队列在 S3 差 4.6~9 倍 |
| guild | DAU×0.6×5/86400×2.4 | 833 次写/s | 4,167 | 8,333 | K8s 上没部署（`k8s_deploy.ps1:149`、`:936-975`）；连接池没设上限（`go/guild/internal/svc/servicecontext.go:90-100`） | 现有实例数为 0 |
| trade 浏览 | PCU×2%×0.2；每次 COUNT(*) 加 LIMIT/OFFSET（`listing_repo.go:106`、`:123`） | 4,000 次/s | 2 万 | 4 万 | 没有缓存；1 核 MySQL 推断约 1~3k 次/s | 1.3~4 / 7~20 / 13~40 倍 |
| battle 全局池 | 未建模：BattleMaxConnections=4096（`base_deploy_config.yaml:136`），K8s 上是 Agones Fleet（`k8s_deploy.ps1:77`、`:211`） | — | — | — | — | 要计入 N_cpp、fd、端口、证书；缺参数「同时在战人数」 |

### 2.9 运维面

| 项 | 负载公式（参数出处） | S1 | S2 | S3 | 现有上限（出处） | 差距 / 说明 |
|---|---|---|---|---|---|---|
| 日志 | N_save + N_snap + 4E 行/s：每次存盘一行（`player_lifecycle.cpp:3743`）、每份快照一行（`snapshot_system.cpp:109`）、每条连接两行，都在 INFO 级；LogLevel 2 实际是 INFO（G5） | 0.6 万~1.2 万行/s | 2.8 万~6.1 万 | 5.6 万~12.2 万行/s，11~24 MB/s | Loki 1 个副本，ingestion_rate_mb 16，盘 10Gi，保留 168 小时（`loki.yaml:34`、`:89`、`:93`、`:133`） | S3 0.7~1.5 倍；保留 7 天要 6.7~14.5 TB |
| 监控 | 抓取目标 ≈ N_cpp + Go 实例；SM 序列见 §2.2 | 0.2k~0.8k | 1k~4.5k | 2k~9k | 仓库里没有 Prometheus 清单；gate 容器没有 resources，是 BestEffort（G11） | 节点数无法从清单推算 |
| 停服维护 | 全服 kNodeShutdown：每人一次退出存盘加一份 LOGOUT 快照（`cpp/nodes/scene/main.cpp:168`、`:199`） | 100 万份，15~300 GB | 500 万份 | 1000 万份，150 GB~3 TB | 快照 topic 共 1.5 GiB | 99% 以上会被按大小删除 |
| 停服后全服重登 | PCU/T_return，T_return 取 600 秒 [I] | 1,667/s | 8,333 | 1.67 万 | — | 相当于开服峰值的下沿；叠加口令哈希和 HTTP 入口；Redis 若也重启，f_miss=1 |
| gate 滚动发布 | StatefulSet OnDelete，每重启一台踢掉 c_gate 人；每批 10 台、每批 2 分钟 [I] | 10~44 分钟 | 50 分钟~3.7 小时 | 1.7~7.4 小时 | — | — |
| 不可变参数 | db_task 每代 10 个分区（`db.yaml:19`）、快照 3 / 流水 6 个分区、控制面 256、RouteRowShards 16、围栏契约上限 64、node_id 17 bit | — | — | — | — | 开服前按目标档定好，事后只能换代、排空或停写 |
| 跨 zone 旅行 | 未建模。每次跨 zone 包括：1 次 etcd Range（SM 不缓存 gate 列表，`gate_redirect.go:169-180`）、gate 重连、一次退出存盘、两份快照、会话 CAS | — | — | — | — | 缺参数 f_cz |
| 多地域 | 共享 Redis、CZ-2、单个 TiDB 都是「全服单实例」契约 | — | — | — | — | 全部推算都隐含单地域（tidb-phase1 §8） |

### 2.10 全局总账

**Kafka（同一套 3 broker）**
- 集群盘写，快照按 15 KB 计：
  - S1：db_task 0.05~0.42 + 快照 0.05~0.2 + 流水 0.01~0.13，约 0.12~0.74 GB/s。
  - S3：0.53~4.2 + 0.5~2.0 + 0.13~1.25，约 1.2~7.5 GB/s。
  - 快照若按 300 KB 计，S3 可达约 45 GB/s。
- 分区数 = db_task 10Z + 旧路径 N_cpp + 控制面 512 + 审计 9：
  - S1 约 760~1,417 个。
  - S3 约 2.9k~9.4k 个，即 0.9 万~2.8 万个副本。
- C++ 的存盘、快照、流水共用同一个 KafkaProducer 单例（`snapshot_system.cpp:101`、`transaction_log_system.cpp:83`、`player_lifecycle.cpp:3731`）。审计积压会拖慢存盘。

**数据库连接**
- 构成：
  - zone 库：Z(210+16·E_x)+80（`PROGRESS.md:7178`；E_x 是额外落点库数，这里取 0）。
  - 全局库：110Z。
  - PasswordAuth：40Z。
  - friend、trade：共 80。
  - guild：没有上限。
- 合计：S1 约 0.37~0.84 万，S3 约 3.6~8.0 万。

**Redis 连接**：见 §2.5「连接数」。

---

## 3. 必须的架构改造（按依赖排序）

依赖关系：

- **3.0 拓扑决策**：先拍板单元规模，它决定 A6 走哪条路线，以及 A4、A5 的规格。
- **A0 止血**：不依赖任何拍板。
  - A1 控制面按 zone 化：依赖 A0-1。
  - A2 存盘降量：依赖条件写（proto2mysql v0.3.0）和 V0 测量。
  - A3 审计改造：依赖回档 RPO 拍板。
  - A4 Kafka 分区与世代：依赖 3.0、A2、A3。
  - A5 TiDB 规模与发号打散：依赖 Phase 1。发号打散必须在生产铸出第一个 player_id 之前，route_shard 必须在任何环境建 player_route 之前。
  - A6 Redis 按用途拆，再走单元内单主或集群化：依赖 A0-4、C++ Redis 客户端替换、3.0。
  - A7 go/db 与 data_service 横向扩：依赖 A3、A4、A5。
  - A8 全局服务分片：依赖 A6，以及产品决策。
- **3.0 的实现**：依赖 A1、A4、A5、A6 完成，并改 CZ-2 / D12 契约。

### 3.0 区域集群 + 全球薄层（先拍板单元规模，实现排在最后）

- **做什么**：
  - 把全服切成若干「区域单元」。每个单元自带 etcd、按用途拆分的核心 Redis、Kafka、同城三 AZ 的 TiDB、go/db、data_service（zone 侧）、scene_manager，以及 login / gate / scene。
  - 全球薄层只留低频、单一地域写入的部分：账号目录、名字、号段、归属目录（player_route 记 home_zone 和所属单元）、好友与公会、审计冷存储的索引。
  - 跨单元旅行按「转区」处理，即停写、排空后迁移，不走实时的跨单元 CAS。
- **为什么**：
  - 现在所有共享件都是「全服一份」：SharedRedis、CZ-2、单个 TiDB、一套 Kafka、全服一个 SM leader、etcd 全前缀。§1.1 里每一项都在 S1~S2 撞墙。
  - 切成单元后，平方项和单主上限都被钉在单元规模以内。
  - tidb-phase1 §8 和 launch_verdict 出于地理原因推荐了同样的形态；本文从容量角度得出同一结论。
- **量级**：
  - 建议单元规模不超过 **50 万 PCU**（S1 的一半）。这是初值，要等 V2 实测单主上限后再定。
  - 理由：按用途拆分后，核心 Redis 在 S1 是 6.5~8.2 万 ops/s，对 10 万参照没有浪涌余量；减半到 3.3~4.1 万，才能给浪涌留出约 2.5 倍空间，前提是排队和限流已经封住重连浪涌。
  - S1 需要 2 个单元，S2 需要 10 个，S3 需要 20 个以上。
  - 另一条路线是单元内上 Redis Cluster，单元可以做到 S1~S2 规模（见 A6）。
- **依赖**：
  - A1：单元内的 Z 也有 10~23 个，控制面同样要按 zone 化。
  - A5：发号和归属目录要能表达单元。
  - A6：单元内 Redis 的形态。
  - 设计文档：改 CZ-2 / D12 契约（AGENTS §11.5）。
  - go/db 按单元部署，或补 storage→DSN 映射（现在只有一个 Database.Hosts，`db.go:112`）；跨实例 relocate；跨单元转区的排空流程。
- **契约 / Java**：
  - 帧格式不变。
  - 区服选择和跨单元旅行的体验会变（用转区代替无缝跨 zone），属于客户端可见行为。
  - 必须和客户端、Java 版同批评审（AGENTS §12）。

### A0 止血（不依赖任何拍板，现网也需要）

1. **etcd**：
   - 设自动压缩（periodic 或 revision 模式）、quota-backend-bytes，定期 defrag，告警 DB size。
   - 钉住镜像版本（现在是 `:latest`）。
   - 核实 C++ watch 带 start_revision（`etcd_helper.cpp:45-58`）撞上「已压缩」时怎么处理。
   - 现网 2 个 zone 也需要做，推算约 2 周就会 NOSPACE。
2. **gate / scene / battle 补 RLIMIT_NOFILE**：照 `kafka.yaml:157-176` 的 `ulimit -n` 薄壳做，或在进程启动时 setrlimit。加启动断言：soft 小于 GateMaxConnections 加余量就拒绝启动（G4 P0-1）。（2026-10-18：已落码，未验证。没用 ulimit 薄壳，做法是进程启动时 setrlimit 自抬软限；余量取 4096，覆盖 gate 直连模式约 2Z 条到全服 scene_manager、scene 约 4Z 条到全服 SM / DS（推算）；另加运维豁免开关 `NODE_OPEN_FILE_LIMIT_MODE=warn`，但 warn 下 fd 表可能被客户端连接占满，gate / battle 的应急首选是调低准入上限。数字不改。）
3. **Kafka**：
   - db_task、快照、流水、控制面四类 topic 显式设 topic 级 retention.bytes。
   - PVC ≥ Σ(分区数 × retention.bytes × RF) ÷ broker 数 × 1.3；部署脚本加预算门禁。
   - lag 告警以秒计，和 T_buf 对比。
   - 确认旧 topic 积压归零后，置 `DisableLegacyPerNodeTopic=true`。
   - 生产端打开压缩（C++ librdkafka 的 compression.type，login 的 CompressionType，用 lz4 或 zstd），压缩比实测后再写进容量公式。
   - message.max.bytes 在生产者、topic、broker 三处对齐。
4. **Redis**：
   - SessionReconciler 改成全服单例（etcd 选主），并在 SetOnline / SetDisconnecting 的 Lua 里维护 `gate:{uuid}:sessions` 索引，对账只读死 gate 的那个集合，存活集限定范围。
     - （2026-10-18 状态，B0-1）已落码，未编译、未测试。与本条的差异：选主用会话 Redis 上的锁（shared/leader），不是 etcd；索引键是 `player:gate_index:sessions:<uuid>`，留在核心闭包，不用会随排队族搬走的 `gate:` 前缀；存活集仍是每拍一次 gate 前缀全量 Range（量级 N_g），没有限定范围。更正：本条的 SetOnline 不存在，加入索引的是 SetSession / Reconnect，摘除的是 SetDisconnecting / MarkOffline / 对账转换。
   - 删除 `dirty_keys_set` 的 SADD（没有读者，`redis_client.h:22`、`:50`）。（2026-10-16：写入已在代码中删除（已落码，未编译、未测试；全部 scene 换成新镜像前生产仍在写）。两段存盘脚本都删了 SADD，普通脚本改名 `kSaveLuaScript`，脚本只访问 KEYS[] 声明的键；上面的行号是落码前快照。写频口径：周期存盘 + 进退场约 7.8 万/s（推算），另加战斗结算、资产提交、收件箱、跨节点交接触发的事件驱动存盘（未量化）。存量清理见 [release-checklist.md](../ops/release-checklist.md) §6.1。）
   - scene_manager fullSync 的 `SCAN scene_nodes:zone:*:load`（`load_reporter.go:750`）改成显式的 zone 注册集合。
     - （2026-10-16 状态，B0-5）已落码，未编译、未测试：fullSync 里两处全库 SCAN（另一处是孤儿清理的 `SCAN world_channels:zone:*`）都换成了登记集合 `scene_manager:zones` + 期望数 Hash 点查；有历史数据的长期环境升级时按 docs/ops/scene-node-role-split.md §6.1 一次性补登记；见 PROGRESS 同日条目。
   - friend 在线目录的 SCAN（`online_directory.go:23-26`）改走索引。
   - 显式设置 go-redis / go-zero 的 PoolSize 和 Redis 的 maxclients，按 Z 记总账。
     - （2026-10-15 补，P0-E）maxclients 与总账已落码、未经 Codex 验证：`redis.yaml` 显式 `MAXCLIENTS=40000`，总账在 `tools/scripts/k8s_deploy.ps1`「Redis 连接总账」（C++ 每进程一条，G 按 2 记，待实测），§2.5「连接数」一行的现有上限以它为准；PoolSize 仍未做。按用途拆分后，每个拆出的实例都要显式 `--maxclients`，并按用途重算总账。
5. **task:result 按 login 实例分频道**：直接用 SPUBLISH 形态，便于日后上集群。
   - **2026-10-18 更正（负责人 10-17 认可的偏离）**：分频道先用 PUBLISH/SUBSCRIBE，SPUBLISH/SSUBSCRIBE 推迟到上 Redis Cluster 那批，即 A6 路线 C 的「Pub/Sub 改成 SPUBLISH」。
     - 理由：① 现在 K8s 上是 Redis 7.2 单主，单主下两者的投递语义和成本等价；② miniredis（v2.35~v2.37）不支持 S 系命令，现在就用 SPUBLISH 的话，go/db 和 login 的发布、订阅用例都得改用 hook 拦截或真 Redis；③ 切集群本来就要 go/db 与 login 全服同一拍切换（G4a §5.6），届时一起改命令即可。
     - 代价：切集群那一拍，两端要再改一次实例频道的命令。如果届时做不到同拍，就沿用本项「新 login 两边都订、发布方每条只发一次」的过渡办法。
     - 状态：第 1 批（proto `reply_instance = 10`、go/shared/taskresult、login 双订并填回复目标、login 送达计数）已落码，未编译、未验证。go/db 按目标发布是第 2 批。交付与验证命令见 `docs/handoff/launch-readiness-20261013/spec_B0-3_task_result_channel.md` 文末。
     - 状态（2026-10-18 第 2 批）：go/db 按 reply_instance 定向发布已落码，未编译、未验证，同时新增 `db_task_result_notify_total{target,result}` 和独立告警文件。发布规则：只发一次；空或不合法回落旧频道；先 LPUSH 再 PUBLISH。两批都全量上线后，投递量由 R × L 变为 R × 1。上线顺序：login 仍是旧版的 zone 先 go/db、再按 zone 金丝雀 login；login 已是第 1 批的 zone 按 zone 金丝雀 go/db。在压测对比表出来之前，不宣称有收益。
6. **新号**：NewPlayerSeed 默认打开，K8s ConfigMap 渲染这个键；同一批让新号首登不再发 LOGIN 快照（§4.4）。
7. **gate 分配**：
   - login 的 GateWatcher 改成 watch 本 zone 前缀加进程内缓存，不再每次 AssignGate 都打一次 etcd Range。
   - PickGate 在本地累加已分配数，改用「两个随机挑一个」，并加新鲜度过滤，一次修掉羊群效应和「黑洞」。
   - **2026-10-18 状态（负责人当日定：先做 watch 缓存与随机二选一，取代全局单点交接 §3 P2「list-watch 有信号再做」的建议）**：
     - 已落码，未编译、未测试：GateWatcher 的 list-watch 镜像（按每台 gate 计新鲜度，有一台超过 15s 没刷新或镜像未同步时回落直读）；快路径 PickGate 改为随机二选一（同人数取第一次抽中的，不按 NodeID）；
     - gate 排空监视仍直读 etcd（要 fail-closed），不读镜像；排空脚本清标记前多等 20s，等各 login 的镜像跟上；
     - **当前 K8s 默认每 zone 2 台 gate**：两台人数不等时与旧规则相同；全体同人数（开新区第一个上报窗口）时从全落 NodeID 最小的一台变为两台各半；之后的窗口里羊群依旧，充分摊开要每 zone 至少 3 台，滞后窗口里最闲那台最多拿 2/n；
     - 没做：本地累加已分配数、新鲜度过滤（「黑洞」仍在），dispatcher 跨拍仍从同一份滞后人数起算，快路径不感知单台连接上限；scene_manager 跨 zone 重定向的选 gate 已于同日改为随机二选一并补排空过滤（共用 `go/shared/gatepick`，Java `GatePicker` 同步；未编译、未测试）；
     - 细节见 [gate-load-balancing-design.md](./gate-load-balancing-design.md)「gate 目录怎么读、怎么挑」。在压测对比表出来之前，不宣称有收益。
8. **日志**：存盘和快照的逐条 INFO 改成采样或 DEBUG 级；Loki 的摄入限额按公式定。
9. **guild**：补进 K8s 部署清单，设 SetMaxOpenConns，计入连接总账。
10. **补测量**：§5 的 V0。

- **依赖**：第 4 条的 SessionReconciler 改造要过设计评审；第 6 条要拍板打开开关。其余没有依赖。
- **契约 / Java**：都不改客户端。Java 版的 etcd / Redis / Kafka 部署是否同构未核实，需按 PARITY 自查同类配置。

### A1 控制面按 zone 化

- **做什么**：
  - **C++ 发现**：Gate、Scene、Login、PlayerLocator 这些 zone-scoped 类型只 watch `<Svc>.rpc/zone/<本 zone>/`。去掉每 2 秒一次的全前缀 Range，改为依赖 watch revision，只在断流或压缩后重拉；兜底周期拉长到 60 秒以上并加抖动。分配键移出发现前缀。
  - **gate 计数**：player_count 不再写 etcd，由 login 本地累加，或走 SM 已有的负载路径。
  - **scene_manager**：按 zone 选主（`scene_manager:leader:lock:zone:{z}`）；每个副本只 watch、只刷新本 zone 的节点，用 MGET 或 pipeline；每个节点的序列只由本 zone 的副本导出。
    - （2026-10-16 状态，B0-5）负载刷新已改为领导者按 zone 批量 pipeline，保留单写者，跟随者不再读，每节点序列只由领导者导出（已落码，未编译、未测试）。按 zone 选主、只 watch 本 zone、每节点序列由本 zone 副本导出仍待做；到时可直接复用 `load_refresh.go` 的按 zone 分批形状。
  - **scene 只连本 zone 的 SM / DS**，或用一致性哈希固定连 k=2~4 个实例。
  - 跨 zone 资源（MainWorldCross / InstanceCross，`load_reporter.go:28`）保留一个全局协调者。
- **为什么**：§2.2 里五处平方项。
- **量级**：
  - etcd 读量从 N_cpp·K/2 降到约 N_cpp × 本 zone 键数 ÷ 60：S3 从 276 万~5,336 万 KV/s 降到约万级 KV/s。
  - SM 刷新从 0.8·Z·N_s 降到 0.8·N_s；序列从 6·Z·N_s 降到 6·N_s；网状连接从 4Z·N_s 降到 4·N_s。
- **依赖**：
  - 设计文档：要推翻 `config.go:78-80` 的「服务 zone-agnostic」前提（AGENTS §11.5）。
  - 先确认 SM 没有进程内的按玩家状态：改成本 zone 后，同一个玩家跨 zone 会由两个 SM 处理（`node_utils.cpp:239-256`）。
  - Go 侧 noderegistry（`registry.go:201-207`）同批评估。
  - A0-1 先行。
- **契约 / Java**：不改客户端。Java 版的注册发现是自有实现，需评估有没有同类的全量拉取（未核实）。

### A2 存盘降量：按列组判脏、去掉近空的第二条 DBTask、压缩

- **做什么**：
  1. **pd1 的 DBTask**：按子表做脏比较，没变就不发；或者把 player_database_1 并入 player_database。需要评审 G5「两张子表不原子」和首存建行的语义。
  2. **按列组判脏**：
     - player_database 有 16 个业务列：transform、背包、任务、账本、宠物、技能、货币等，每个子消息一列。把它们分成列组，存盘时逐组比较，只把脏列组放进 DBTask。
     - go/db 基于条件写原语（proto2mysql `SaveConditional`，已推分支，未验证；`PROGRESS.md` 2026-10-13 条）做列级条件 UPDATE。
     - Transform 单独成一组：只走动的玩家只写几十字节，不再整行。
  3. **生产端压缩**（同 A0-3）。
  4. **评估 go/db 写任务只 DEL、不 SET 子表缓存**：`key_ordered_consumer.go:836` 有「回写失败不 ack」的语义，要一并处理。
  5. **写路径 Redis 往返从 11 次降到 2~4 次**：db-task-pipeline-bottlenecks §6.3 的 3a / 3b，按其立项判据执行。
- **为什么**：N_save 和 B 是存盘、Kafka、Redis、TiDB、binlog 五处带宽的共同乘数。
- **量级**：
  - 第 1 条：写任务减半，生产环境每次存盘少 1~4 条 SQL。
  - 第 2 条：字节量从 N_save×B 降到 N_save×Σ(脏列组字节)。真实的脏列分布没测，必须先由 V1 实测，再写进公式。
  - 第 5 条：S3 的 go/db Redis 从 49 万~122 万降到 8.9 万~44 万 ops/s；和第 1 条叠加后，约 4.4 万~22 万。
- **依赖**：
  - 正确性轨的条件写（db-task-pipeline-bottlenecks §6.3 第 2 步），先于删守卫和列级写。
  - V0 和 V1 的测量。
  - 注意：Redis 父键仍然是整份写入，因为 C++ 的 guard Lua 把父键和 owner_epoch 原子绑在一起。列组判脏只能减 Kafka 和 DB 的量，减不了 Redis 父键的写。要减 Redis，得另立「父键拆成 hash 字段」的设计，不建议在开服前做。
- **契约 / Java**：不改客户端。Java 版没有 Kafka 存盘链，不适用；在 PARITY 登记不适用的原因。

### A3 快照审计改造

- **做什么**：
  1. **开关与采样率**：加显式开关和采样率配置。现在唯一的关法是把号段的 snapshot Enabled 置 false，结果是每次调用都打 ERROR，不可取。
  2. **去重**：LOGIN 快照和跨节点换场景的快照只发内容哈希，哈希变了才发整档；新号首登不发。
     - 前提：「LOGIN 快照等于上一份 LOGOUT」只在两次之间没有离线写入时成立。GM 离线改档、回档、合服之后要强制发整档；哈希要把 schema_version 一起覆盖进去。
  3. **限频**：每个玩家每 24 小时（或 6 小时）最多一份整档，两份之间的增量由 transaction_log 承担。
  4. **冷存储**：blob 用 zstd 压缩后写对象存储，数据库只留元数据行（player_id、zone、created_at、对象键、大小、哈希），30 天生命周期。
  5. **库内保留期**：接入分批、限速的清理任务，或改成按天分区加 DROP PARTITION。TiDB 的 TTL 需要 datetime 列，而现在是 uint64 秒。
  6. **和存盘隔离**：审计 producer 和存盘 producer 拆开，各有自己的队列上限，队列满时允许丢审计、不挡存盘；审计 topic 单独设 retention.bytes。
  7. **消费独立部署**：审计消费不再跟着 zone 扩缩。现在是 2Z 个 data_service 成员抢 3 / 6 个分区，任何一个 Pod 变动，全球审计一起重平衡。
  8. **transaction_log**：
     - 按 tx_type 分级：高频低价值的掉落和小额货币按会话聚合成一条；物品实例的增删转、交易、邮件、充值保持逐条。
     - 补 item_uuid 索引，或下线 QueryByItemUUID；QueryLog 不再每次先跑 COUNT(*)。
     - TiDB 下二级索引加分片前缀，或迁到列存 / 日志分析库。
- **为什么**：§2.7。快照在 S1 就差 1.9~22 倍，库内无界增长；source=1 的行没有任何生产读路径，全部容量都在为一条还没实现的恢复路径付费。
- **量级**：
  - 第 2、3 条做完后，S3 每天不超过 1 亿份整档（日均约 1,160/s，峰值约 2,800/s），压缩前 1.5~30 TB/日。
  - 落到对象存储后，库内只剩元数据，约 1 亿行/日 × 200 B ≈ 20 GB/日。
- **依赖**：
  - 回档负责人先拍板 RPO 粒度，并写设计文档。
  - 对象存储是新依赖，要按 AGENTS §11.2 写清现有能力为什么不够。
  - A0-6。
  - 和 A4 的审计 topic 换代一起做（AuditTopicGeneration：`snapshot_system.h:6-17`；`data_service.yaml:221-236`）。
- **契约 / Java**：不改客户端。回档 RPO 是两版共享的运营契约，Java 版同批决策并登记 PARITY。

### A4 Kafka 分区与世代

- **做什么**：
  1. **集群拓扑**：按区域单元或 zone 组拆 Kafka 集群。
     - 这里有两份文档冲突：`kafka-cluster-production-runbook.md:202-205` 写「多 zone 必须每 zone 独立集群」，`control-plane-topic-partitioning-20260908.md` §2 写「控制面必须是一个集群」，要由负责人定。
     - 单元化后可以统一为「每单元一个集群，控制面在单元内唯一」。
  2. **db_task 分区**：每代分区数 P 要满足「P × 每分区消费能力 ≥ 2 × 每 zone 峰值写任务」。
     - C_z 不超过 10 万时，现在的 10 个分区纸面上够。
     - C_z 要到 50 万以上，必须先完成条件写，再换代扩分区。
  3. **快照和流水的分区**：按 A3 改造后的量定，分区数 ≥ 2 × 峰值条数 × t_ins。
  4. **控制面 P=256 保持不变**；流量预算从 500 条/s 更正为 0.4 万~4.4 万条/s（开服再加 2C_p）。
  5. **go/db 消费组按 zone 命名**（L-09）。
  6. **不可变参数**：§2.9 里的不可变参数开服前按目标档定好，列成表。
- **为什么**：分区数不能在线扩（`db-task-kafka-partition-contract.md`），而保留上限折算的盘量已经超过 PVC。
- **量级**：S1 现状集群盘写 0.12~0.74 GB/s。A2、A3 做完后按实测重算 broker 数，每单元至少 3 个。
- **依赖**：3.0（单元划分）、A3（审计的量）、A2 的条件写（换代扩分区时需要）。
- **契约 / Java**：不改客户端；Java 版自查 Kafka 用法。

### A5 TiDB 集群规模与发号打散

- **做什么**：
  1. **规格**：Phase 1 按 §4.5 的公式定 TiKV / TiDB / PD 规格。
  2. **审计不进 OLTP 集群**：player_snapshot 的 blob 去对象存储；transaction_log 去独立集群或列存。
  3. **热点修复**：
     - route_shard 改为 64（§4.1）。
     - player_name 的 PK 至少预切 64 段；owner 唯一键改为 (owner_shard, player_id) 加 CHECK，必须在 TiDB 建表前定。
     - idx_route_storage 的 NULL 段：要么打开 PinOnCreate，要么先在真 TiDB 上核实 `SPLIT … BY (NULL, s, 0)` 的写法再补切。
     - `ALTER TABLE route_meta CACHE`。
     - FenceShards 的 TiDB 档从 8 改为 64；zone_merge_fence 每 zone 预切 4~8 段。
     - 打开 `tidb_scatter_region=global`。
  4. **发号打散决策**：
     - S2/S3 且要上 Phase 2 全局玩家库时，在「发号层打散」和「至少 5 条单调索引逐表加片前缀」之间二选一。
     - 推荐发号层打散，而且**只能在生产铸出第一个 player_id 之前定案**；之后再切，新号会和已发的连续号相撞。
     - 前置条件：
       - 审计各环境 [2^22, 2^55) 里的存量 id（`idsegment/client.go:36-51`）。
       - 置换函数必须是 [1, 2^55) 上的双射，常量永久钉死，并有回归测试。
       - raiseIdSegmentFloor 不能再用 MAX(player_id) 当水位（`id_segment_store.go:302-356`；`schema.go:1544`），否则打散后会把号段当场抬爆。
       - friend 推荐的随机 pivot 依赖 id 稠密（`recommend_repo.go:335-349`、`:475`），要同步改。
  5. **号段**：开服即用 Step=1000（L-10）；S2/S3 档 MaxStep 从 1000 改到 10000，PrefetchAt 从 0.1 改到 0.3。Conf 现在不暴露 PrefetchAt，要在 `player_id_minter.go` 的 Options 里传。
- **为什么**：§2.3，单副本 MySQL 在 S1 就不可行；§2.6，单调尾部。
- **量级**：见 §4.5。
- **依赖**：Phase 1（P1-0 到 P1-6）；发号打散在首个生产 player_id 之前；route_shard 在任何环境建 player_route 之前。
- **契约 / Java**：发号打散改变了客户端看得见的 player_id 分布，按 AGENTS §12 与 Java 版同批评估。其余不改客户端。

### A6 Redis：按用途拆，再走单元内单主或集群化

> **（2026-10-19 指向注，本节原文不改）** 下面「在此之上二选一」已由负责人 2026-10-17 拍板，两条路线叠加，不再二选一：核心走「单元化 + 单元内 Redis Cluster 按 `{player_id}` 分片」。路线 R、C 的展开与 2000 万复算见 [shared-redis-split-by-purpose.md](shared-redis-split-by-purpose.md) §16、§20（纯设计，未落码），新分批见该稿 §24。**（10-22 补）** 下面路线 C 的「键名迁移要支持滚动升级的双读窗口」已由该稿 §20.7 作废：双读会让新旧二进制各按各的键名 CAS、绕过围栏；首次上线前改名直接以新键名起步，上线后走停写切换或版本门禁。「冷玩家卸载」的条件见该稿 §23.2（候选，待评审）。

- **做什么**：
  - G4a 拆分照做，但按 §4.2 调整。
  - 在此之上二选一：
    - **路线 R**：把区域单元的规模压到单主能承受（3.0），单元内维持「按用途各一个单主」。
    - **路线 C**：核心上 Redis Cluster。需要：
      - 闭包内的键统一加 `{player_id}` hash tag：PlayerAllData、`player:{id}:owner_epoch` / handoff / location、player:session、player_locker。
      - Lua 里去掉全局键：dirty_keys_set 删掉，`player:leases*` 按 slot 分桶。（2026-10-16：dirty_keys_set 的写入已在代码中删除（已落码，未编译、未测试；全部 scene 换成新镜像前生产仍在写）；`player:leases*` 分桶未做。删它只是必要条件之一：守卫脚本的父键与 `owner_epoch` 键仍无共享 hash tag，C++ 也没有 MOVED/ASK 处理，不能据此称「已可上 Cluster」。）
      - 拆开跨实体的 Lua：账号外观修复、组队 S_COMMIT、离场计数；场景生命周期改按 `{zone}` 打 tag。
      - 围栏键不能在脚本里拼（`router.go:299-324`）。
      - SCAN 逐节点执行。
      - 只用 DB 0：guild 的 DB 2 要迁出（`guild.yaml:14-38`）。
      - Pub/Sub 改成 SPUBLISH。
      - C++ 换成 hiredis-cluster 或 redis-plus-plus，补 AUTH 和 DNS 重解析，并去掉「同连接存盘后 PING 判落盘」的顺序依赖。
      - 键名迁移要支持滚动升级的双读窗口。
  - 两条路线都要做的：
    - 冷玩家卸载：玩家离线、且 go/db 的落库游标已经越过最后一次存盘后，给父键加 TTL（需设计评审）。
    - 核心显式设 maxmemory + noeviction，并配内存告警。
    - AOF 的 startupProbe 和重写阈值按数据集大小定。
- **为什么**：§2.5。核心拆分后 S1 是 0.65~0.82 倍且没有浪涌余量，S2/S3 是 3.4~15 倍；内存从 0.1 TB 到 2 TB 没有上界。
- **量级**：
  - 路线 C：核心随分片数线性扩。
  - 路线 R：每单元不超过 50 万 PCU 时，拆分后核心约 3.3~4.1 万 ops/s。
- **依赖**：
  - A0-4 先行：去掉 SCAN、删 dirty_keys_set。（2026-10-16：dirty_keys_set 的写入已在代码中删除（已落码，未编译、未测试；全部 scene 换成新镜像前生产仍在写）；去掉 SCAN 未做。）
  - C++ 客户端替换：scene 侧任何多实例改动都以它为前置（`redis_manager.h:25-28`）。
  - 和 `db-task-pipeline-bottlenecks.md:341`「不为 Redis Cluster 改 hash tag 键名」冲突，由负责人裁决。
  - `shared-redis-split-by-purpose.md` §3.3 否决的「核心上 Cluster」要在 S2/S3 重开讨论。
- **契约 / Java**：不改客户端。键名变更会影响 merge_zone、回档、清库脚本；Java 版自查 Redis 用法。

### A7 go/db 与 data_service 横向扩

- **做什么**：
  - **go/db**：每 zone 80 个 worker 纸面上够，扩展靠 A4 的分区和按 zone 命名的消费组。遇到 1040 或拿不到连接时改为延后、不消耗重试预算（L-04）。导出 sql.DBStats 和 PoolStats。
  - **data_service 拆成三类部署**：
    - zone 侧：路由读，随 zone 部署。
    - 全球薄层实例组：名字、号段、D1，按建角峰值独立扩缩，不再随 2Z 线性放大连接。
    - 审计消费：独立部署，按分区数定副本。
  - 连接总账按 §2.10 重算；tidb-server 台数按 §4.5 反算。
- **为什么**：全局库连接是 110Z，S3 达 1.1 万~2.5 万；审计消费组全球只有一个，会随 zone 变动抖动。
- **量级**：全局库连接从 110Z 降到「薄层实例数 × 55」，和 Z 无关。
- **依赖**：A3、A4、A5。
- **契约 / Java**：不改客户端。Java 是单库，连接预算各自核算。

### A8 全局服务分片

- **chat**：
  - 世界频道按 zone、单元或房间分片。
  - chat 实例在内存里缓存频道尾部，约 200 ms 刷新一次，Redis 读量从 PCU/10 降到「实例数 × 5/s」。
  - 和 match 拆成两个 Redis 集群。
  - 「全服一个世界频道」在 S1 就已经读不过来，产品形态要负责人和策划定，属于客户端可见行为。
- **match**：
  - 「1 万人/分钟」的预算按 §2.8 的公式重算。
  - {mq} 单 slot 改成按 (mode, config) 分 hash tag。这要先重新设计 D3 的「注册集 ⊆ 队列集合」不变量。
  - 弹组时的 n 次 loadTicket 和 n 次 CAS 合成一段 Lua。
  - 单元化后匹配默认在单元内，跨单元匹配另做设计。
- **friend**：推荐加结果缓存或离线预计算，并限频；登录 MGET 计入核心预算。
- **trade**：浏览加短 TTL 缓存；总数用缓存值或近似值，不再每次 COUNT(*)。
- **guild**：先部署、封顶连接池、压测单帮热点（活动结算、灯会重置时刻），再谈 actor 化。
- **battle**：补「同时在战人数 / 每节点容量」的模型，计入 N_cpp、fd 和端口。
- **依赖**：A6（Redis 集群拆分），以及产品决策。
- **契约 / Java**：chat 分频道、match 分单元会改变客户端可见行为，Java 同批。其余是服务端内部改动。

---

## 4. 对在途工作的参数修正

### 4.1 D1 player_route 的 route_shard（建表前必须定）

**推荐值：64。**

理由：

1. **尾部写入量**：每条尾部 = C_p/R。S3 开服上沿时，R=16 是 3,125/s，R=64 是 781/s，R=256 是 195/s。B 没测过：官方没给数字，本文假设 1k~3k，而 TiKV 的 Raft 批量可能让真实值更高。
2. **不可逆**：改模数就是整表换主键重写（`route.go` 的 RouteRowShards 注释）。在 B 未知、建表后又改不了的情况下，取 64 是保险。理由不是「16 已经证明不够」。
3. **代价小**：
   - 按 home_zone 枚举，始终是 idx_route_home 上的一段连续区间，不随 R 变化。
   - 随 R 增加的只有两项：1b 增量推送每拍要发 R 条查询；预切 Region 数。后者把每 zone 预切改成 8 个点就能抵消，提案 H8 已经预留了这个改法。
4. **上限**：route_shard 列是 tinyint unsigned（`schema.go:197`、`:208`），R 最多 256。否决 1024 的理由只有一个：列会溢出，要改 smallint。TiKV 有 hibernate region，10^5 量级的 Region 数是常规规模，不构成否决理由。
5. **和发号打散无关**：以后就算采纳发号层打散，64 也没有副作用；反过来，如果打散不落地，16 在 S2 上沿已经到 B 的 0.5~1.6 倍。
6. **后备**：V5 实测 B 低于 800/s 时取 256。

**现在仍然可以改**：

- `go/shared/placement/route.go`、`route_store*.go` 在工作区是未跟踪文件（本次 `git status` 抽查）。第 1a 批没提交、没编译（`PROGRESS.md` 2026-10-12「第 1a 批落码」条）。
- 改之前必须确认：没有任何环境（包括 Codex 本机、kind、staging）用工作区代码跑过 data_service 的 `-migrate`。原因是 MigrateSchema 在 redis 模式下也会建表，一旦建出 player_route 就改不了了。

> **状态注（2026-10-13）**：已按 64 落码，**未编译、未测试**（AGENTS.md §10.1，待 Codex 验证）。
> - 上面「工作区未跟踪、没提交」的说法已过时：R=16 的版本已随提交 `d5cbb8a8e` 提交并推送。所以任何跑过那一版迁移、或用那一版启动过 data_service 的库，都可能已经按 `% 16` 建了 player_route。这种表迁移和建池探针都不会报错（形状守卫只查列名与主键；探针行对 `% 16` 与 `% 64` 同样违例），要人工 `SHOW CREATE TABLE player_route` 核对，是 `% 16` 就确认无数据后 DROP 全部五张路由表再迁移。
> - 落码内容：`RouteRowShards` 与 `chk_route_shard` 改为 64；`route_tidb.go` 里 idx_route_home / idx_route_storage 每个 zone / 落点值切 8 个点（片 0、8、…、56），行数据与 idx_route_updated 仍是每片一段（各 63 个切点）；服务 yaml 的 `RouteStore.FenceShards` 改为 16（MySQL 档）。
> - 下面「连带修正」里只做了 FenceShards 的 MySQL 档，以及把 `route_meta CACHE`、EVALSHA 两条写进提案；zone_merge_fence 细切、WAITAOF 两条没动。
> - 细节、前置步骤与验证命令见 [player-route-mapping-durability-proposal.md](./player-route-mapping-durability-proposal.md) §11.2「行片 16 → 64」。本节正文与 §1.1、§2.6、§2.9、附录里的 R=16 是当时的抽查结果，不回改。
> - **2026-10-15 补**（上面第一条的「建池探针都不会报错」已过时，原句不回改）：2026-10-14 起建池探针第 5 步 P5 能认出按另一个行片模数建的表（`ErrRouteShardModulusMismatch`）。db_first 的 data_service 不装配 route store（`Store.Required=true` 时拒启），route_tool 退出码 2，debugutil 返回错误。迁移仍然不报错；redis 模式的 data_service 不建 route store，认不出旧模数，所以人工 `SHOW CREATE TABLE player_route` 核对保留。见提案 §11.2「正向探针 P5」。

**要改的地方**：

- `route.go:60`；
- `schema.go` 里 chk_route_shard 的 `% 16`（:188 一带）；
- route_test.go 和 route_store_test.go 里钉住的用例；
- 提案 §11.2 和 1b「按片循环 s=0..15」的描述；
- `route_tidb.go` 的每 zone 预切点改成 8 个。

**连带修正**：

- RouteStore.FenceShards：TiDB 档从 8 改为 64，MySQL 档改为 16，只改配置。
- zone_merge_fence 每 zone 预切 4~8 个点。
- `route_meta CACHE` 从可选改为必选（S2 起）。
- 发布脚本 EVAL 改 EVALSHA，列为 S2 的前置。
- WAITAOF：redis-mapping 考虑 appendfsync always（组提交），或调大连接池，并纳入 L-12 压测。

### 4.2 拆 Redis（G4a）改为集群化或单元化

> **（2026-10-18 指向注，本节原文与数字不改）** 本节对 G4a 的修订要求已并入 [shared-redis-split-by-purpose.md](shared-redis-split-by-purpose.md) 2026-10-18 修订稿的 §16~§26（纯设计，未落码）。核心路线按负责人 2026-10-17 拍板，走「单元化 + 单元内 Redis Cluster 按 `{player_id}` 分片」，两者叠加。2000 万口径的重算以本文 §7.2 为准，见该稿 §16。下面「先删 expand 死代码再拆」与负责人 10-13「保留」的决定冲突，该稿 §18 改用 home_zone 作用域来论证。第 2、3 批另有 db-write-fencing 令牌发布脚本的硬依赖，见该稿 §18、§25。

**复算结论**：按用途拆分只够撑到约 S1，核心是 0.65~0.82 倍，没有浪涌余量。S2/S3 拆完后，核心仍有 3.4~15 倍，mapping 并入控制面后是 2.3~11 倍。原文 §6.1 的例子（全服 10 万）和 §9「单实例余量远大于首日推算值」，要按本文复算并加注。

**批次调整**：

- **新增第 0 批**：
  - SessionReconciler 去掉全键 SCAN；
    - （2026-10-18 状态，B0-1）已落码，未编译、未测试，见 [player-locator-gate-session-index.md](player-locator-gate-session-index.md)。
  - 删除 dirty_keys_set（2026-10-16：写入已在代码中删除（已落码，未编译、未测试；全部 scene 换成新镜像前生产仍在写）；存量清理见 release-checklist §6.1）；
  - task:result 分频道（从第 1 批提前）；
    - （2026-10-18 状态，B0-3）第 1 批（proto + go/shared + login）已落码，未编译、未测试。go/db 侧（按回复目标定向发布、指标与告警）是第 2 批，尚未开工。第 2 批上线之前，投递量不变，仍是全服广播。
    - （2026-10-18 第 2 批状态，B0-3）go/db 侧（定向发布、`db_task_result_notify_total`、`deploy/k8s/task-result-notify-alerts.yaml`）已落码，未编译、未测试。上一行的「尚未开工」同此更正。两批全量上线之前，仍是全服广播。
  - SM 负载刷新只读本 zone（属于 A1）。
    - （2026-10-16 状态，B0-5）已用「领导者按 zone 批量刷新 + 登记集合替代 SCAN」缓解，已落码，未编译、未测试；字面的「每个副本只读本 zone」、按 zone 选主和 watch 收窄仍归 A1。
- **第 2 批**：控制面的键都带 topic，也就是带 zone，应该按 zone 组分实例，不并进全服唯一的 redis-mapping。在线扩分区的锁是全服共享键，先删 expand 死代码再拆。
- **第 3 批 redis-cache**：从「可选」改为必做，子表缓存要 30~600 GB。
- **第 1c 批的映射拷贝**：这是唯一一次全量搬键的停写窗口。
  - 建议同时把 player:zone / placement / maprev 改成同一个 hash tag。这需要负责人裁决，因为和 db-task-pipeline-bottlenecks §6.5 冲突；不改，将来集群化还要再停写搬一次。
  - 冻结窗口按键数复算：映射三族约 3×N_reg 个键。开服前做，键很少；若在 S1 规模之后做，约 3 亿个键，按 5 万键/s 约 100 分钟，不是原文的 10~30 分钟。
- **§13 第 5 条（核心内存）**：必须在 S1 开服前拍板，做冷玩家卸载并设 maxmemory。
- **§3.3 否决的「核心上 Cluster」**：在 S2/S3 重新讨论，或者用 3.0 的单元化代替。
- **账号目录**：不能只留在核心 Redis。在全局库建账号目录表，主键用 account 或它的哈希（随机键，没有尾部热点），Redis 降为缓存。集群化时，account:{a} 和 account_lock:create:{a} 必须同一个 hash tag。
- **redis-match-cluster**：原文 :128 列为「本稿不处理」，建议纳入。chat 和 match 拆成两个集群；match 的票据和锁不能被 volatile-lru 淘汰，淘汰策略要重定。
- **friend 登录的 MGET player:session**：计入核心预算。核心闭包不能拆（原文 :160）。

### 4.3 快照管道扩容的目标

- **方向调整**：从「扩分区」改为 A3 的「去重 + 时间桶采样 + 对象存储 + 库内保留期」。
- **只扩分区为什么不可行**：
  - 如果不改语义只扩分区，分区数要 ≥ 2×N_snap×t_ins。t_ins 取 5~15 ms 时，S1 要 11~133 个，S2 要 56~667 个，S3 要 111~1,333 个。
  - S3 每天新增 6~480 TB。
- **改造后的目标（S3）**：
  - 每天不超过 1 亿份整档，峰值约 2,800/s。
  - 按逐条口径，分区数是 28~84 个；G2a 打开批量后更少，实测后再定。
  - G2a 默认逐条；打开批量要等压测对比表和内存护栏拍板（tidb-phase1 L-07 注）。
- **L-07 第 2 条**：改为审计 topic 单独设 retention.bytes = 峰值字节/s × 能容忍的积压时长（建议至少 2 小时），PVC 按 RF 计入。
- **L-07 第 3 条的容量公式要补上 (1+t)**：跨节点换场景、停机、排空、疏散都会发快照。

### 4.4 新号免冷读的必要性

- **结论**：S1 开服前必须打开。从「可选优化」改为开服前置条件，K8s ConfigMap 要渲染 NewPlayerSeed。
- **理由**：
  - 不打开时，开服 task:result 投递从 S1 的 4 万~46 万/s 到 S3 的 400 万~4,460 万/s。
  - 开新区时单 zone 冷读 0.9k~1 万/s，是 go/db 纸面读能力的 0.4~6 倍，Phase 2 下是 3~31 倍。
  - 每个新号还要约 18 条 Redis 命令。
  - 打开后这些都归零，代价是每次建角多一次核心 Redis 脚本写（S3 开服 1~5 万/s），要计入核心的尖峰预算。
- **连带**：
  - **PreloadPool.Size=256 要复核**：开种子后，新号的入场链在池 worker 里同步跑完。开新区时单个 login 要处理 225~2,500 个新号/s，按 apply 40~100 ms 算，需要 9~250 个并发，已经逼近 256。
  - **hash tag**：种子 Lua 同时碰父键和 owner_epoch，将来集群化时两者必须同一个 `{id}` tag。
  - **快照**：同一批去掉新号首登的 LOGIN 快照。
  - **开服分流**：种子失效会退回冷读，所以新号不要集中到太少的新区，K ≥ 2×C_p ÷ 单 zone 读能力。

### 4.5 TiDB Phase 1 拓扑规格

现文 §2.2 的 prod 档「3 PD / 至少 3 TiKV / 至少 2 TiDB，规格按压测定」是现网 2 个 zone 的量级。S 档按下面的公式定。

| 量 | 公式 | S1 | S3 |
|---|---|---|---|
| 逻辑写 W | N_save×B_row（11~33 KB） | 24~183 MB/s | 0.24~1.83 GB/s |
| Raft 写（3 副本） | 3W，还要再乘 LSM 放大（未测） | 73~550 MB/s | 0.73~5.5 GB/s |
| MVCC 旧版本 | W × gc_life_time 600 秒 × 3 | 44~330 GB | 0.44~3.3 TB |
| 玩家主数据存量（3 副本，不含审计） | N_reg×20 KB×3 | 约 6 TB | 约 60 TB |
| 事务数（每个事务取一次 TSO） | (2~5)×N_save + 建角 + 其它 | 0.4 万~2.8 万/s | 4.4 万~27.8 万/s |
| 连接 T_peak | §2.10 | 0.37 万~0.84 万 | 3.6 万~8 万 |
| tidb-server 台数 | N ≥ 1 + k·T_peak/max_connections（k=1.5，见 tidb-phase1 §2.4）；示例 max_connections 取 2000 | 4~8 台 | 28~61 台 |
| TiKV 台数 | max(存量 ÷ 单盘容量, Raft 写 × LSM 放大 ÷ 单盘写带宽, 热点 Region 数) | 待 V5 | 待 V5 |

- **拓扑建议**：
  - OLTP（zone 库加全局薄层）和审计分开：快照 blob 不进 TiDB，只迁元数据；流水用独立集群或列存。
  - 「全部库同批迁到同一个集群」（Phase 1 原结论）在 S1 以上要改成至少两套。
  - 单元化后，每个单元一套同城三 AZ 集群；全球薄层在单一写入地域一套。
- **待测**：LSM 放大、单 Region 写入预算 B、tidb-server 内存与连接数的关系、PD TSO 吞吐、Key Visualizer 里的尾部 Region。

---

## 5. 验证路径（按 AGENTS §6 分阶段，由 Codex 执行）

**每个阶段都按 §6.2 执行**：

- 跑前把上一轮的 `stress_summarize.ps1` 输出存为 `prev-summary.txt`。
- 清理：`robot/logs/stress-*` 旧目录、`bin/log/*`、go 服务的日志与 pid、`redis-cli FLUSHALL`、Kafka offset reset，以及 Redis / MySQL / etcd / 全局库路由表全部数据。
- 用 `stress_snap.ps1` 在 ramp 完成、稳态中段、稳态末三个时刻拉 snapshot。
- 跑后用 `stress_summarize.ps1` 出表，并与 prev-summary 二维对比。

Claude 不执行任何构建、测试或压测。

| 阶段 | 目的 | 环境规模 | 前置 | 通过标准 | 产出（替换本文的哪个假设） |
|---|---|---|---|---|---|
| V0 观测补齐 | 让汇总脚本能看见 Redis、etcd、Kafka、go/db、审计 | 本机单 zone，用 R17~R19 的快照回放 | 无 | 新增各段用回放数据出表无报错，并有回归用例。新增段：Redis INFO / commandstats（或接 redis_exporter）；etcd 的 DB size、revision、leader CPU、Range/Watch 速率；`db_blob_row_bytes` 的 p50/p99；Kafka 每 topic 的 bytes-in；go/db 分段（`db_task_redis_step_seconds`）；快照/流水的 lag 与 retention_gap；SM 的 `enter_scene_stage_seconds_count`；sql.DBStats / PoolStats | 测量能力；同时从 R19 的 `t*m_sm.txt` 核实 §0.6 第 2 条 |
| V1 单 zone 真实负载稳态 | 测出真实脏率、载荷、带宽和单进程负载 | 45k 机器人，profile 增加 Move 和 AOI；爬坡后满员稳态至少 30 分钟；「2 gate + 4 scene」和「10 gate + 20 scene」各跑一组 | V0、A0-2 | scene 帧耗时 p99 ≤ 33 ms（`scene-node-threading-model.md:95-96`）；preload_failed=0；稳态 Kafka lag 回到 0；gate IO 线程 CPU < 70%；没有断连风暴 | 1−s、B 的分布、b_down、c_scene / c_gate 的下沿 |
| V2 单组件饱和点 | 把每个「未测」变成实测值 | K8s 专用压测环境（tidb-phase1 §3.5），每项单独打到饱和：gate 单 IO 线程；scene 单进程；SM 单实例 EnterScene；go/db 先人为积压再看排空速率；快照落库 t_ins（MySQL 与 TiDB 各一次，批量 1 与 16 各一次）；Argon2id 的 t_kdf；单主 Redis 在本仓命令混合下的 ops/CPU 曲线；login 单实例 | V0 | 每项给出饱和值和拐点（p99 劣化到 2 倍处），回填本文 §2 的「现有上限」列 | c_gate、c_scene、单主 Redis 上限、t_ins、t_kdf；同时决定 3.0 的单元规模 |
| V3 合成注册压测 etcd | 验证 etcd 平方项和压缩 | 3 节点 etcd，生产规格。假节点只做注册、续约、每 10 秒 Put NodeInfo、每 2 秒全前缀 Range（现状行为），规模 N_cpp = 700 / 3,300 / 6,700（分别对应 S1 / S2 / S3 上沿）。A1 做完后复测 | A0-1 | A1 之后：etcd leader CPU < 50%；Range/Txn p99 < 50 ms；DB size 在 24 小时内稳定；不出现 NOSPACE。A1 之前的结果作对比基线 | etcd 真实上限，替换 10^5 参照 |
| V4 K8s 多 zone 控制面 | 证明 A1 之后没有超线性项 | 先 10 个 zone × 每 zone 1 万机器人（含移动），再 20 个 zone。每 zone 至少 3 台 gate，scene 数按 V1 定；Kafka 至少 3 个 broker | A0、A1 | SM 的共享 Redis GET/s、Prometheus 序列数、gRPC 连接数随 Z 线性增长（10→20 个 zone 的比值不超过 2.2）；PickGate 各 gate 新连接分布的变异系数 < 0.3；SessionReconciler 一轮 < 60 秒 | 多 zone 下的 C_z，替换 45k / 100k 两种口径 |
| V5 开服尖峰（L-14） | 测建角链路的热点与上限 | 专用压测环境上的 TiDB，同环境先跑 MySQL 基线。0–30 秒尖峰，覆盖建角、首次进场、周期存盘、进退场；单 zone 2,500 建角/s 持续 60 秒；同时打开审计流量；NewPlayerSeed 开、关各一组 | A0、A5 第 3 条、route_shard 定值 | 建角 p99 在预算内（负责人定）；W1 的 1205/1213 比例 < 0.1%；记录 WAITAOF 实际时延和发布未确认率；Key Visualizer 里没有单个 Region 超过实测 B；没有全局 task:result 扇出；种子命中率约 100%；快照和流水的 retention_gap 为 0 | B、T_lock、WAITAOF 延迟、单 zone 建角上限 |
| V6 停服与全服重登 | 验证停服窗口和重登浪涌 | 在 V4 规模上做一次 kNodeShutdown 全服停机，然后在 600 秒内全部重登 | V4、A0-3、A3 第 6 条 | 停机存盘全部落库，数据一致性检查 0 差异；快照 topic 的 retention_gap 为 0；重登期间 Redis、HTTP 入口、口令哈希都不过载；没有雪崩式重试 | T_return 下的入口与 Redis 余量 |
| V7 区域单元演练 | 唯一能下「能撑 S1」结论的一步 | 机器人从 50 万加到 100 万，分布在 1~2 个区域单元。机器人主机按每台不超过 45k（R18 单机口径）约 12~23 台；服务端 N_g 和 N_s 按 V1/V2 测出的 c_gate、c_scene 定；etcd、Redis、Kafka、TiDB 每个单元一套 | A0~A7 中完成的各项 | 所有组件不超过 V2 实测饱和值的 70%；稳态 30 分钟 lag=0；开服尖峰和停服重登各一次，分别满足 V5、V6 的判据；对比表齐全 | S1 容量结论 |

S2/S3 不做全量演练，按「单元容量 × 单元数」线性外推，前提是 V4 和 V7 已经证明单元内、单元间都没有超线性项。全球薄层另做一次 C_p=5 万/s 的建角专项压测。

---

## 6. 待负责人确认的数字与拍板项

1. **目标档**：开服目标是 S1、S2 还是 S3，一年目标是哪一档；具体的 PCU 和 DAU。
2. **建角峰值**：开服或大活动的 C_p；开新区时新号是集中到 K 个新区，还是摊到全部 zone。
3. **会话参数**：n_s、峰均比、T_sess；换场景间隔 T_sw 和跨节点比例 f_x，或者交给 V1 实测。
4. **单 zone 目标 C_z**：45k（现测）还是 10 万（2026-06-03 的目标）。它决定 Z，以及 db_task 的 10 个分区够不够。
5. **区域集群 + 全球薄层**：是否采纳；单元规模（建议不超过 50 万 PCU，待 V2 定）；是否同意改 CZ-2 / D12 契约并立设计文档；跨单元旅行改为「转区」的体验能否接受。
6. **核心 Redis 路线**：单元内单主（路线 R）还是 Redis Cluster（路线 C）。同时裁决两处冲突：`db-task-pipeline-bottlenecks.md:341`「不为 Cluster 改 hash tag」，以及 `shared-redis-split-by-purpose.md` §3.3 的否决；还有第 1c 批映射拷贝时是否同时改 hash tag。
   - **（2026-10-19 指向注，本条原文保留）** 路线已于 2026-10-17 拍板：单元化 + 单元内 Redis Cluster 按 `{player_id}` 分片，两者叠加。`shared-redis-split-by-purpose.md` §3.3 的否决由该稿 §20 取代；hash tag 原则 10-13 已定为按 player_id 分片。剩下的「第 1c 批是否同时改 hash tag」见该稿 §21，列在 §25.2 待拍板。
7. **route_shard**：取 64（后备 256），并确认没有任何环境建过 player_route。
8. **发号**：发号层打散还是逐表加片前缀。S2/S3 且上 Phase 2 时必须二选一，并且在生产铸出第一个 player_id 之前定；客户端和 Java 版同批。
9. **快照审计 RPO**：每个玩家每 24 小时（或 6 小时）一份整档、哈希去重、对象存储（新依赖）、保留期；source=1 的恢复路径什么时候实现。
10. **Kafka 拓扑**：每单元一个集群，还是每 zone 一个集群（runbook 和控制面文档冲突）；各 topic 的 retention.bytes 和能容忍的积压时长（建议 db_task 不短于能容忍的 DB 故障时长，快照至少 2 小时）。
11. **开服数据层**：明确以 TiDB Phase 1 为开服前提，因为单副本 MySQL 在 S1 不可行；Phase 1 的「全部库同一集群」改为 OLTP 与审计分离。
12. **新号**：NewPlayerSeed 作为开服前置条件默认打开；新号首登不发 LOGIN 快照。
13. **核心内存**：冷玩家卸载和 maxmemory（shared-redis-split §13 第 5 条），S1 开服前必须拍板。
14. **产品形态**：世界频道怎么分（按 zone、单元还是房间）；match 的预算从 1 万人/分钟上调到多少。
15. **口令登录**：正式服是否开口令登录（f_pwd），决定 Argon2id 的配置和 login 的规格。
16. **号段**：S2/S3 档 MaxStep 改 10000、PrefetchAt 改 0.3。
17. **入口**：下行带宽 b_down 的目标；入口形态是 NodePort 端口段、每台 gate 一个 LB，还是统一的入口代理。
18. **Java 版**：哪些改造要进 PARITY。发号打散、区域单元、回档 RPO、chat / match 的产品形态必须同批。

---

## 附：证据清单

标「本次抽查」的条目，是写本文时回到文件核对过的；其余条目来自五路组件推算和复核，按工作区行号转引。

**场景与基线**
- [M] `docs/stress/stress-1zone-45k-2026-06-04-round18.md:9`、`:27`、`:32`、`:105`
- [M] `docs/stress/prev-summary.txt:13-21`、`:35-40`（R19）
- [M] `docs/stress/stress-1zone-45k-2026-05-partition-10.md:21`（2 gate + 4 scene）
- [M] `docs/stress/stress-3zone-2026-05-23-postmortem.md`
- [C] `robot/logic/ai/action.go:111-116`；`robot/etc/robot.stress-3zone-z1.yaml:20-37`；`robot/logic/ai/robot_ai.go:207-209`
- [D] `docs/notes/redis-oom-data-safety-qa-record.md:21`、`:267`
- [D] `docs/design/global-data-layer-tidb-decision.md:34`

**接入层**
- [C] `bin/etc/base_deploy_config.yaml:2-11`、`:39-49`、`:96-104`、`:126`、`:136`、`:197-206`
- [C] `cpp/nodes/gate/main.cpp:96-125`、`:405-411`
- [C] `cpp/nodes/gate/handler/rpc/client_message_processor.cpp:542-545`
- [D] `docs/design/gate-connection-admission-control.md:143`、`:207`（G4、G5、G11）
- [C] `go/login/internal/svc/servicecontext.go:151-160`、`:292-297`、`:310-317`
- [C] `go/login/internal/logic/pkg/loginqueue/gatetoken.go:84-101`；`gatedrain.go:67-90`；`queue.go` 的 `Lookup`
- [C] `go/login/internal/logic/pkg/auth/password_provider.go:24-35`；`go/login/etc/login.yaml:89`、`:189-191`、`:356-373`、`:496`；`go/login/internal/config/config.go:276`
- [C] `deploy/k8s/manifests/go-svc/gateway.yaml:24`、`:70`；[D] `docs/design/k8s-client-entry.md:174`、`:311`
- [C] `tools/scripts/k8s_deploy.ps1:77`、`:140-144`、`:203-207`、`:211`

**控制面**
- [C] `cpp/libs/engine/core/node/system/discovery/service_discovery_manager.cpp:12-33`、`:44-55`、`:75-83`
- [C] `cpp/libs/engine/core/node/system/etcd/etcd_helper.cpp:33-62`；`etcd_service.cpp:50-74`、`:278-357`；`etcd_manager.cpp:47-79`
- [C] `cpp/libs/engine/core/node/system/node/node_util.h:103-131`；`node_util.cpp:112-142`；`cpp/libs/engine/core/network/node_utils.cpp:239-256`
- [C] `deploy/k8s/manifests/infra/etcd.yaml:75`、`:114`（`:latest`，本次抽查）、`:181-183`、`:190`（2 GB 注释，本次抽查；全文没有 AUTO_COMPACTION，本次抽查）
- [C] `go/shared/snowflakealloc/allocator.go:926`、`:1427`；`go/shared/noderegistry/registry.go:201-207`
- [C] `go/scene_manager/internal/config/config.go:65-89`
- [C] `go/scene_manager/internal/logic/load_reporter.go:28`、`:31-32`、`:225-245`（本次抽查：读不门控，写按 isLeader）、`:686-712`、`:750`、`:806-810`、`:1001-1020`
- [C] `go/scene_manager/internal/metrics/metrics.go:37-51`；`go/scene_manager/etc/scene_manager_service.yaml:125`、`:192`、`:209`
- [C] `deploy/k8s/manifests/go-svc/scene-manager.yaml:24`、`:81`；`data-service.yaml:34-35`、`:110`；`login.yaml:24`
- [D] `docs/design/control-plane-topic-partitioning-20260908.md:44-52`、`:76-84`、`:160-192`
- [C] `tools/scripts/k8s_deploy.ps1:1076-1082`；`deploy/k8s/AGENTS.md:86`（已过时）

**存盘管线**
- [C] `cpp/libs/services/scene/core/system/redis.cpp:120-160`
- [C] `cpp/libs/services/scene/player/system/player_lifecycle.cpp:2040-2054`、`:2737`、`:2740`、`:2957`、`:3555`、`:3633-3745`
- [C] `cpp/nodes/scene/handler/grpc/scene_node_service.cpp:182`；`cpp/nodes/scene/main.cpp:86`、`:168`、`:199`
- [C] `proto/common/database/mysql_database_table.proto`（player_database 16 个业务列，player_database_1 只有 player_id 和压测探针；本次抽查）；`cpp/libs/engine/core/utils/proto/proto_dirty_compare.h`
- [C] `go/db/etc/db.yaml:19`、`:22`、`:68`、`:124`；`go/db/internal/kafka/key_ordered_consumer.go:42`、`:369-411`、`:836-857`、`:889`
- [D] `docs/design/db-task-pipeline-bottlenecks.md:211`、§6.1~§6.5（本次抽查 `:283-342`）
- [C] `cpp/libs/engine/infra/messaging/kafka/kafka_producer.cpp:59-76`；`deploy/k8s/manifests/infra/kafka.yaml:25`、`:157-176`、`:277`、`:345-349`、`:363`
- [C] `go/shared/kafkautil/topic_init.go:114-125`；`tools/scripts/k8s_deploy.ps1:1186-1200`、`:3437`、`:3456`、`:3458`
- [C] `deploy/k8s/manifests/infra/mysql.yaml:25`、`:56-67`、`:74`、`:82`、`:159-160`
- [D] `docs/PROGRESS.md:7120`、`:7171-7192`（连接总账，本次抽查）、`:7193-7232`（第 1a 批未编译，本次抽查）、`:7321` 起（SaveConditional）

**读档**
- [C] `go/login/internal/logic/pkg/dataloader/ensure_player_all_data_async.go:56-93`、`:157-207`；`go/login/internal/logic/clientplayerlogin/entergamelogic.go:380`、`:712`
- [C] `go/login/internal/dispatcher/task_result_dispatcher.go:26-32`、`:226-232`
- [C] `cpp/libs/engine/infra/storage/redis_client/redis_client.h:20-24`、`:42-52`（2026-10-16：落码前行号；同日该文件删 SADD、加注释，行号整体后移，现按 `kSaveLuaScript` / `kSaveIfGuardLuaScript` 定位）
- [D] `docs/design/new-player-first-enter-no-cold-read.md` §1、§13（本次抽查：开关默认关，已按默认值落码，未验证）

**共享 Redis**
- [C] `go/player_locator/internal/logic/session_reconciler.go:42-44`、`:123`（本次抽查）、`:155`、`:196`
- [C] `deploy/k8s/manifests/infra/redis.yaml:70`、`:93-97`、`:111-120`、`:136-155`；`redis-ha.yaml:9-13`
- [D] `docs/design/shared-redis-split-by-purpose.md:12-25`、`:123`、`:128`、`:160`、`:207-214`、`:302-322`（本次抽查）、`:769`、`:776`、`:889-902`（本次抽查）
- [C] `cpp/libs/engine/thread_context/redis_manager.h:25-28`；`cpp/libs/engine/core/node/system/node/node.cpp:541-557`；`cpp/libs/services/scene/battle/system/player_battle.cpp:385-393`
- [C] `go/data_service/go.mod:94`（automaxprocs）

**全球薄层**
- [C] `go/shared/placement/route.go:32-41`、`:45-67`（本次抽查：RouteRowShards=16；文件未跟踪）
- [C] `go/data_service/internal/store/schema.go:147-155`、`:162-217`（本次抽查 chk_route_shard 注释）、`:1544`
- [C] `go/data_service/internal/store/route_tidb.go:157-191`；`route_store.go:43-47`、`:86`、`:880-892`
- [C] `go/data_service/internal/store/id_segment_store.go:183-229`、`:302-356`；`go/shared/idsegment/client.go:11-51`、`:240-257`
- [D] `docs/design/player-route-mapping-durability-proposal.md` §11.2、§12、H8、HR5
- [C] `go/login/internal/logic/clientplayerlogin/loginlogic.go:321-349`；`createplayerlogic.go:307-316`
- [C] `go/friend/internal/data/recommend_repo.go:200-216`、`:335-349`、`:475`

**审计**
- [C] `cpp/libs/modules/snapshot/snapshot_system.cpp:33-50`、`:75-76`、`:101-112`；`snapshot_system.h:6-17`
- [C] `cpp/libs/modules/transaction_log/transaction_log_system.cpp:48-90`
- [C] `go/data_service/internal/kafka/snapshot_consumer.go:31`、`:73-116`；`txlog_consumer.go:41-56`；`start.go:123-141`
- [C] `go/data_service/internal/store/snapshot_store.go:24-25`、`:813`、`:852-861`（工作区改动）；`transaction_log_store.go:94-130`、`:180`、`:219-230`
- [C] `proto/common/database/rollback_database_table.proto:19-23`
- [C] `go/data_service/etc/data_service.yaml:91-110`、`:160-176`、`:219-254`（工作区改动）
- [D] `docs/design/tidb-phase1-and-launch-readiness.md` §2.2、§2.4、§7（L-07 的 G2a 注）、§8（本次抽查）

**全局服务**
- [C] `go/chat/internal/logic/chat_logic.go:8-15`、`:40`、`:88-90`；`go/chat/etc/chat.yaml:67-73`；`deploy/k8s/manifests/infra/redis-match-cluster.yaml:116-120`
- [C] `go/match/internal/logic/matcher.go:46-53`、`:590-690`；[D] `docs/design/cross-zone-matchmaking.md:92`、`:208-209`
- [C] `go/trade/internal/data/listing_repo.go:106`、`:123`
- [C] `go/guild/internal/svc/servicecontext.go:90-100`；`go/guild/etc/guild.yaml:14-38`；`tools/scripts/k8s_deploy.ps1:149`、`:936-975`

**运维面**
- [C] `deploy/k8s/manifests/infra/loki.yaml:34`、`:89`、`:93`、`:133`；`tools/scripts/k8s_deploy.ps1:2355-2372`
- [C] `go/scene_manager/internal/logic/gate_redirect.go:169-180`

**背景**
- [D] 会话 scratchpad 中的 `launch_verdict.txt`（geo 段、unverified 段，本次抽查）
- [D] `docs/design/tidb-phase1-and-launch-readiness.md` §7（L-01~L-17）

---

> **本文落盘后的待办**（文档纪律，AGENTS §5）：
> - 在 `docs/README.md` 对应分类登记一行。
> - 在 `docs/PROGRESS.md` 追加一条，注明「只读推算，未编译、未测试、未压测；Java 版：容量模型不涉及实现，各改造项的状态见 §3」。
> - 在 `tidb-phase1-and-launch-readiness.md` §7 的 L-07、L-14、L-15，`shared-redis-split-by-purpose.md` §6、§9、§13，以及 `new-player-first-enter-no-cold-read.md` §1，各加一行指向本文的注记。

## 7. 2026-10-15：2000 万同时在线与两仓容量比较

### 7.1 本次范围和证据边界

负责人本次确认 DAU 1 亿、峰值同时在线 2000 万，并要求比较本仓与
`D:/luyuan/xuanming-server`，在正确性、数据一致性与既有必需能力均满足的前提下，选择更可扩展的实现。
不再以“全部照搬原文件”或“本仓已有实现就只取几个增量”作为选型规则。

范围为 login、friend、chat、guild、mail、runtime/leaderboard、economy/auction、economy/trade。
不迁移 dialogue、player、inventory、mission、scene_manager；玩家资产和任务继续由既有 C++ 系统持有权威。
客户端同步已经授权。trade 是原仓双方确认的玩家交易，不能用本仓同名聚宝斋寄售服务代替。
原仓审计基线为 `b1f6c1ebf4583b45a9370ad0522c64196dc792b2`（检查时工作区干净）；
本仓以当时工作区源码为准，保留其他会话已有修改。

本节是源码、配置和既有证据的对照，没有运行新的性能比较。**两套系统目前都没有本次目标的同口径达标证据。**
多副本 API、TiDB DDL、Redis Cluster 配置和 cellroute 纯函数均不能单独证明承载人数。
少做持久化、少发通知、漏发邮件、减少功能或放宽资产一致性，也不能算性能更好。

### 7.2 新目标不能直接把旧 S3 所有数字乘二

DAU 仍为 1 亿，PCU 从 1000 万增加到 2000 万；按在线人数增长的负载与按日活增长的负载要分开。
沿用 §0.3 的会话次数、时长和换场景间隔作为假设时，有以下推导：

| 量 | 新目标下的条件推导 | 性质 |
|---|---|---|
| 日均会话开始速率 | `1 亿 × 2 / 86400 ≈ 2315/s` | 假设每 DAU 每天 2 次会话，不是登录实测 |
| 日均在线人数 | `2315 × 1800 ≈ 417 万` | 假设平均会话 1800 秒 |
| 峰均在线比 | `2000 万 / 417 万 ≈ 4.8` | 因此旧表 2.4 峰均比不能同时沿用 |
| 峰时进退场速率 | 稳态近似下 `2000 万 / 1800 ≈ 11111/s` | 依赖会话分布及稳态假设；不代表开服洪峰 |
| 换场景速率 | `2000 万 / 300 ≈ 66667/s` | 场景管理不迁移，但登录与恢复接缝仍须考虑这项外部负载 |
| 全服广播投递量 | 每秒 1 条全服消息即 `2000 万次投递/s` | 发送端限流和消费者扩容都不能消掉收件人数乘数 |
| task:result 全局广播投递量（2026-10-18 补，B0-3） | 投递数 = R × L。R 是读回执速率；L 是 login 订阅者总数，每 zone 2 副本，Z=200~445 时 L=400~890。冷读场景下 R 与 L 都正比于在线人数，所以投递量随 PCU² 增长，不能拿 S3 的数乘二。2000 万口径下：稳态（父键常驻、新号种子关）R≈139/s，投递 5.6 万~12.4 万/s；冷玩家卸载上线或核心 Redis 切主后（f_miss→1）R≈2.2 万/s，投递 889 万~1,976 万/s；开服且新号种子关时 R=2 万~10 万/s，投递 800 万~8,890 万/s。合服回流另算：合服工具（`tools/merge_zone/player_rows.go`）会 DEL 父键，回流玩家全部冷读。头 5 分钟若有 2~3k/s 冷进场，即 4~6k 读回执/s，L=400~890 时投递约 160 万~534 万条/s（全局单点交接 M4 在 1000 万口径下算的是 0.8~1.2e6）。按实例定向投递（B0-3）后变为 R × 1。第 1 批已落码、未验证；go/db 定向发布（第 2 批）上线前仍是 R × L（2026-10-18 更正：第 2 批 go/db 定向发布也已落码，未验证；两批都全量上线之前，仍按 R × L 估） | 推算（[I]），未压测。数字取自 B0-3 规格 summary 和核实意见的复算；单主参照 10 万/s |

好友读写、邮件收取、榜单更新、拍卖下单和交易确认的峰值，不能仅由 PCU 推出。
每域必须写清活跃比例、每活跃玩家操作频率、热点集中度、消息大小、保留期和失败重试率，
再计算 SQL / Redis / Kafka / 下行带宽预算；未知参数保持未知，不用来源不明的“单机可承载人数”补齐。

### 7.3 已确认的比较方向

| 域 | 本次源码证据与比较方向 | 尚不能宣称的结论 |
|---|---|---|
| friend | 本仓 `recommend_repo.go` 的候选窗口、`recommend_sql.go` 的按方言提示与服务器语句超时，以及 `friend_repo.go` 的升序主键点锁应保留；原仓补充申请代次、推送恢复与 TiDB 运行接线。原仓 `friend_sharding.go` 仅做落点观测，真正跨节点事务由 TiDB 提供 | 不能根据一段 `database/sql` 事务判定单机，也不能把原仓旧推荐 SQL 覆盖本仓已有的有界修复；无同口径新实测，不能报提升百分比 |
| chat | 本仓有 Redis Cluster 安全的单键操作，但世界历史固定 `chat:{world}:log`；原仓有多频道、持久私聊与推送。原仓 `chat_routing.go` 明确区域桥仅观测、未改变单总线投递，`sendWorld` 仍全服广播 | 双方都不能仅靠增加 chat 副本解决世界频道热点与全服扇出；本仓只做部分频道、历史尽力保留，不能直接与原仓完整推送业务比 QPS |
| guild | 必须比较本仓 `guild_player_state(0)` 全局插入守卫、`guild_rank:maintenance_lock` 与原仓按 guild 父行锁的冲突范围；优先消除不相关帮会之间的串行化，同时保留本仓已有经济和活动的一致性门禁 | 不能直接删守卫以换吞吐，也不能把原仓少做的业务当作更高容量的证明 |
| mail | 原仓有完整业务，可作为移植起点；其递增 Snowflake 游标与提交乱序的多副本漏信风险、系统 / 帮会邮件无查询上限须先修复，再接现有 C++ 资产接口 | 本仓没有可直接比较的完整 mail 实现；原仓代码存在不等于可安全横向扩容，不能照搬后宣布可用 |
| leaderboard | 以原仓现成服务为业务起点，另核单榜热 key、更新频率、排序口径、结算写放大及多副本结算幂等；本仓帮会内部榜不能代替通用榜 | Redis Cluster 可以分散不同榜，不会自动拆开同一个榜；无热点榜测试不能推断亿级玩家榜容量 |
| login | 保留本仓已接 C++ 的 gate 会话栅栏、KDF 资源上限、开服队列；比较原仓账号 SQL 代际、失败配额和统一恢复行为。原仓 owner / allocator 不在本次迁移范围，恢复出口须适配现有场景管理 | 不得删掉 owner 依赖后直接启动原仓恢复链，造成全员 WAIT；不能把本仓当前保证冒充原仓 exact owner / lease 的完整证明 |
| auction | 原仓有市场分片与撮合业务，但现有启动硬限两片，HRW 选主不是已接好的跨实例请求转发；需验证热点市场串行上限、改片数迁移与真实资产接线 | 原仓分片数量不能随意改 N，单个热门 market 也不会因多起实例自动变成并行撮合 |
| trade | 原仓是双方确认的玩家交易，本仓是聚宝斋市场；按相同订单状态、托管资产与提交语义准备对比入口，复用现有资产通道，inventory 权威不迁移 | 两个不同业务不能直接拿吞吐排输赢；路由日志不等于已将订单或结算分片 |

### 7.4 同口径验证与选择门禁

1. 固定源版本、目标工作区快照、硬件、数据库版本 / 拓扑、Redis / Kafka 拓扑与配置。
   每项先对齐可观察语义、数据规模、好友度数 / 帮会大小 / 热点榜和市场分布、消息保留期、缓存冷热和副作用。
2. 先跑正确性与并发不变量，再比较吞吐和资源；记录完成的业务操作数，而非仅 API 接收数。
   超时后后台仍在做、漏投递、重复发奖或订单未结算，都不能记成成功。
3. 输出吞吐、p50 / p95 / p99、错误 / 超时、CPU / 内存 / 网络、SQL 扫描与锁等待、
   Redis 热 key / 槽负载、Kafka lag / 最老积压时间、恢复时间及单位完成操作的资源成本。
   延迟 SLO 和故障恢复期限须在测试前冻结，不能看完结果再改通过线。
4. 覆盖单副本、多副本扩容、热点倾斜、冷缓存、消费者重启和依赖短暂不可用。
   重点检验增加副本后完成吞吐是否增长，以及错误率 / 一致性 / 队列最老年龄是否恶化。
5. 压测执行仍逐项遵守 AGENTS.md §6：保存上一轮 `prev-summary.txt`、使用隔离测试数据和规定清理、
   三阶段 snapshot、只用 `stress_summarize.ps1` 汇总及前后对比。没有授权不得清理现有业务环境。
   任何新域若尚无可用采集与汇总入口，先补齐口径，再运行比较，不绕过现有压测纪律。
6. 首先完成单域可重复验证，再验证八域混合负载与共用基础设施的总预算。
   单域通过、短时间爬坡或静态公式均不能替代 2000 万同时在线的完整验收。

Java 版当前版本 `0.1.0-SNAPSHOT`，本节没有新增实现或测量；选择落地时逐域登记 PARITY，
各域未完成对齐、未执行的验证继续如实保留。

# 消除单节点:所有服务都能水平扩展(2026-10-01)

> 用户要求(2026-10-01):**不允许任何服务只有一个节点;同一种服务要能开很多个**。
> 本文是这件事的总纲:盘点、顺序、每一项的设计与验证口径。每做完一项就在对应小节更新状态。
> 全部改动遵守 AGENTS.md §10.1:Claude 不跑编译 / 测试,验证命令交给 Codex。

> **2026-10-23 需求扩展**：负责人要求所有实际部署节点具备平滑停机／缩容、水平扩展及灰度／金丝雀发布能力。统一目标、源码现状、差距与验收见 [全类型节点生命周期与发布需求](node-lifecycle-elasticity-release-20261023.md)。本文的“可多开”不等于已通过在线无感维护或渐进发布验收；两版目前均未完成全类型验收。

> **2026-11-05 第二轮**：负责人要求把剩下的单点修完。逐项现状、修法与分批见 §8～§11，MySQL 一节（§6）同日改写。

## 1. 盘点(以 2026-10-01 的 `main` 为准)

「有备份、挂一个不停服」和「能多开、一起分担压力」是两件事,本文按后者判断。

| 组件 | K8s 副本 | 现状 | 本文的处理 |
|---|---|---|---|
| C++ gate / scene | 每区 ≥2 / ≥4 | 可多开 | — |
| C++ battle | 全局池,staging/prod 档 ≥2 | 可多开 | — |
| login / player-locator / match / chat / client-rpc-router / trade / friend | 2 | 可多开 | — |
| scene-manager | 2 | 请求由所有副本处理;后台定时任务(均衡 / 扩缩容 / 实例回收)由选出的主节点做 | — |
| Java gateway | 2 | 可多开 | — |
| etcd | 3 | 集群 | — |
| match 的 Redis | 6 | Redis Cluster | — |
| **Kafka** | 1 | 单 broker,所有 topic 一份副本 | **§2,已落码** |
| **db**(存档写库) | 每区 1 | 代码假定单实例 | §3 |
| **data_service**(发号段 + 审计落库) | 每区 1(Recreate) | 两个实例同时消费互相等锁 | §4 |
| **共享 Redis** | 1 | 单实例;C++ 侧客户端不支持集群 | §5 |
| **MySQL** | 1 | 无主从 | §6(方向已定:TiDB) |
| **guild**(帮会) | 无清单 | K8s 上没有部署 | §7 |

本机(docker-compose / `start_game.ps1`)不在范围内:开发环境保持单实例,省资源。

> **2026-10-18 增补(B0-1,上表原文保留;已落码,未编译、未测试,待 Codex 验证)**:上表漏了「player_locator 会话对账 leader」(全局单点交接 `docs/handoff/handoff-global-singletons-10m-ccu-20261013.md` 的「相关设计」一节已指出 §1 盘点漏了对账),B0-1 又把它改成了全服单例:
> - **形态**:player-locator 照常 2 副本、请求由所有副本处理;只有后台的会话对账(处理 gate 整机崩溃留下的 ONLINE 会话)由选出的 leader 做。选主锁是共用会话 Redis 上的 `player_locator:session_reconciler:leader`(`shared/leader`,TTL 30s),今天所有 zone 的 locator 一起竞选,所以**全服一个**;按本文口径属于「有备份、挂一个不停服」,不是「能多开、一起分担」。
> - **失效处理**:leader 连续 3 拍失败或 3 拍内没有一次成功就主动让位,本进程多等一个 TTL 再竞选,健康副本接手;两个 leader 短暂重叠无害(所有动作走整字节 CAS 或字节守卫 Lua)。
> - **告警**:`deploy/k8s/player-locator-alerts.yaml`(NoLeader / Stalled 为 critical)。K8s 上要先接 player_locator 的 /metrics 才生效。
> - **设计**:[player-locator-gate-session-index.md](player-locator-gate-session-index.md) §4.1(选主与让位)、§8(失败模式)。
> - **为什么可以是单例**:它只做兜底对账,每拍只读死 gate 的索引,每拍的命令数只与 gate 数有关;补建 / 日审计是限速的全键扫描(目标规模平均约 380 条命令/s,推算 [I])。多开分担没有收益,反而要重新解决多副本重复扫描的问题(B0-1 之前正是每副本各扫一遍全键空间)。将来多 cell 时自然变成每 cell 一个。

## 2. Kafka:1 个 → 可配置 ≥3 个 broker(已落码,未跑测试、未上集群)

### 2.1 做法

- **broker 数是一个部署参数**:`k8s_deploy.ps1 -KafkaBrokers <n>`。缺省 0 = 按档位:`-ReleaseProfile` 不是 dev,
  或 `-OpsProfile` 不是 custom → 3;其余 → 1。只接受 1 或 ≥3(两票的选举组挂一个就没有多数派)。
- **所有派生值只在 `Get-KafkaTopologyFor` 里算一次**,三处消费:
  - `manifests/infra/kafka.yaml`:副本数、选举组成员表、broker 默认副本数、`min.insync.replicas`、PDB、`podManagementPolicy`;
  - `manifests/infra/kafka-topic-init.yaml`:预建 topic 的副本数(建完读回核对,不够即失败);
  - 每个 go-svc 的环境变量 `KAFKA_TOPIC_REPLICATION_FACTOR`(`go/shared/kafkautil.EnsureTopics` 建 topic 时用)。

| broker 数 | 兼任 controller | 选举组 | topic 副本数 | min.insync.replicas | PDB minAvailable | podManagementPolicy |
|---|---|---|---|---|---|---|
| 1 | kafka-0 | 1 票 | 1 | 1 | 1 | OrderedReady |
| 3 | kafka-0..2 | 3 票 | 3 | 2 | 2 | Parallel |
| N > 3 | kafka-0..2 | 3 票 | 3 | 2 | N − 1 | Parallel |

- **前 3 个 Pod 兼任 controller,第 4 个起只当 broker**。选举组与 broker 总数无关,所以 3 → N 只改 `replicas`:
  不动成员表、不改 Pod 模板、不触发全体滚动重启。
- **每个 Pod 的身份由启动脚本按 StatefulSet 序号导出**(`node.id = 序号 + 1`、角色、监听、广播地址),不写死在模板里。
  单 broker 时算出来的值与改造前逐字相同(`kafka-0`、`node.id=1`、广播 Service 名),既有 PVC 可以直接沿用。
- **多 broker 时每个 broker 广播自己的 Pod DNS**(`kafka-<n>.kafka-headless.<ns>`);`kafka.<ns>:9092` 这个 Service 只做
  bootstrap 入口,所以所有服务的 broker 地址配置都不用改。

### 2.2 为什么不让 broker 的默认副本数说了算

最干净的做法是建 topic 时副本数传 −1(用 broker 的 `default.replication.factor`),那样连环境变量都不需要。
但它要求 CreateTopics 协议 v4(KIP-464),而仓库钉的 sarama v1.43.1 最高只发 v3,−1 会被 broker 拒掉。
升级依赖需要编译验证,不在本次范围,所以走「部署脚本注入环境变量」这条路 —— 与命令 topic 代号的注入同一机制。

### 2.3 fail-closed 的三道闸

1. **1 ↔ ≥3 不能原地切换**(`Assert-KafkaTopologyChangeIsSafe`)。单 broker 的选举组只有 kafka-0 一票;直接把成员表改成三票
   并扩容,两个空白的新节点可以先凑成多数派、用一份空的元数据日志当选,kafka-0 回来时被截断,全部 topic 元数据消失。
   脚本发现线上副本数与目标跨了这条线就拒绝 apply;缩容(摘掉仍持有副本的 broker)同样拒绝。放行的只有「不变」与「≥3 再加」。
2. **副本数不够的 topic 起不来**。`kafka-topic-init` 与 `kafkautil.EnsureTopics` 都读回已存在 topic 的副本数,
   低于部署要求即失败(高于不算错)。否则一个早先按 1 份建出来的 topic 会带着假冗余一直跑。
3. **broker 级 `min.insync.replicas=2`**。多 broker 集群里若仍有人建出 1 份副本的 topic,`acks=all` 的写入
   (C++ 幂等生产者、login 的 db_task 生产者)会被 `NOT_ENOUGH_REPLICAS` 拒掉 —— 有意如此,不让假冗余悄悄跑着。

### 2.4 已知限制

- **combined 模式**:前 3 个 Pod 同时是 controller 和 broker。Apache 的建议是关键环境用独立的 controller 节点;
  拆出独立 controller 是下一步(需要另一个 StatefulSet),本次不做。
- **加 broker 不会自动搬分区**:新 broker 起来后已有 topic 的副本仍在老 broker 上,要用 `kafka-reassign-partitions.sh` 挪。
- **镜像是浮动 tag `apache/kafka:latest`**(既有问题):多 broker 滚动重启时可能拉到不同版本,生产上应钉版本。
  > **2026-10-16 更正**:已钉 `apache/kafka:4.3.1@sha256:77e3df9054047a88b520d0cc46e16696d3b22022e1d580aeccd2632df6532837`,
  > 四处同值(`deploy/docker-compose.yml` 的 kafka 与 kafka-topic-init、`deploy/k8s/manifests/infra/kafka.yaml`、`deploy/k8s/manifests/infra/kafka-topic-init.yaml`),
  > 核对与升级步骤见 `deploy/k8s/README.md`「镜像钉版本」。已落码,未编译、未测试,待 Codex 验证。
- **就绪探针**沿用 `kafka-broker-api-versions.sh`:它会逐个询问集群成员,别的 broker 不在时对那个节点报错但整体以 0 退出。
  这一点依赖工具实现,**上线前必须在 kind 上实测**(见 §2.5 第 3 步)。
- 本机 docker-compose 仍是单 broker,`KAFKA_TOPIC_REPLICATION_FACTOR` 不设,取默认 1。

### 2.5 验证(交 Codex;工作目录仓库根)

1. 不连集群:
   ```
   pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
   pwsh -NoProfile -File tools/scripts/tests/k8s_migrate_gate.tests.ps1
   cd go/shared && go test ./kafkautil/...
   ```
   期望全部 PASS。`go/shared` 被 login / db / data_service / match / friend / guild 依赖,这些模块要一并重编。
2. kind 单 broker 回归(dev 档):`infra-up -WaitReady`,kafka-0 滚动重启后以 `node.id=1` 回来,日志有
   `kafka identity: node.id=1 roles=controller,broker advertised=PLAINTEXT://kafka.<ns>.svc.cluster.local:9092`;
   `kafka-topic-init` Job Complete,日志每行 `replication-factor=1`;跑一轮 robot login-test。
3. kind 三 broker(全新 namespace,`-KafkaBrokers 3`):三个 Pod 都 Ready;`kafka-topic-init` 日志每行 `replication-factor=3`;
   login 启动日志有 `kafkautil: topic replication contract replication_factor=3`。然后 `kubectl delete pod kafka-1`:
   **另外两个必须保持 Ready**,期间 robot login-test 仍能通过;kafka-1 回来后重新加入。
4. 原地切换的闸:对一个单 broker 集群执行 `infra-up -KafkaBrokers 3`,必须报「拒绝变更 Kafka broker 数」且集群无任何变化。

## 3. db(存档写库):每区 1 → 2 副本(已落码,未编译、未跑测试)

### 3.1 结论:只差重试收据的归属

通读 `go/db/internal/kafka/key_ordered_consumer.go` 后的判定(2026-10-08):

- **已经是跨实例安全的**:Kafka 消费是 sarama consumer group(分区在实例间独占分配,rebalance 时旧属主的在途任务被 claim context 拦住);
  写任务靠 ordering lock(Redis 分布式锁,带租约)互斥、applied 游标保证不回退、owner epoch 守卫归属;读结果发布、能力标记都不依赖单实例。
- **唯一的阻塞点**:重试队列的 processing 列表 `kafka:retry:processing:{topic}` 全 zone 共用,进程启动时把它**整表**搬回 ready。
  第二个实例一启动,第一个实例在途的收据全部被搬走重做。后果是重复执行(被游标挡住,不会写坏库),但「每条收据恰有一个认领者」不成立,
  而且孤儿回收只发生在进程启动 —— 单纯删掉那段恢复,崩溃实例的收据就永远留在 processing 里。
- 顺带:K8s 上 db 的清单没写 `strategy`,默认滚动策略在 1 副本时是先起新 Pod 再停旧 Pod,所以「单实例」在每次发布的重叠期本来就不成立。

### 3.2 做法:processing 按实例隔离 + 租约 + 孤儿回收(`retry_ownership.go`)

| 键 | 类型 | 说明 |
|---|---|---|
| `kafka:retry:queue:{topic}` | LIST | ready,所有实例共享(不变) |
| `kafka:dead:queue:{topic}` | LIST | 死信(不变) |
| `kafka:retry:processing:{topic}:{instanceID}` | LIST | 本实例认领的收据(新) |
| `kafka:retry:instances:{topic}` | ZSET | 实例登记表,score = 租约到期毫秒,统一取 Redis 的 `TIME`(新) |
| `kafka:retry:processing:{topic}` | LIST | 旧版共享列表:新版只回收、不写入 |

> **2026-10-17 更正**:上表 `kafka:retry:queue:{topic}`「ready,所有实例共享(不变)」已过时。④ 重试只交给分区属主起,ready 拆成共享「未分拣收件箱」`kafka:retry:queue:{topic}` 与按原始分区的 `kafka:retry:queue:{topic}:p{N}`(LIST,`RetryRouting=owner` 时写入;owner 实例只认领自己持有 claim 的分区,shared 实例每拍兜底排空全部非空的 p*(本 zone 主库冷却时整拍跳过),从 owner 回滚就靠这一点);另增实例模式 HASH `kafka:retry:instance_mode:{topic}` 与认领 / ack 防重记录 `kafka:retry:guard:*`(TTL 5 分钟),认领脚本也改为带 nonce。键表、两种模式、混跑与回滚以 [db-retry-owner-routing.md](db-retry-owner-routing.md) 为准。已落码,未编译、未测试,待 Codex 验证。

- `instanceID` 默认 `<主机名>:<ListenOn 端口>`(K8s 上主机名即 Pod 名;本机多开时端口不同),可用 `Kafka.RetryInstanceId` 覆盖。
  租约 `Kafka.RetryLeaseSeconds` 默认 30(下限 10),续租与回收周期是它的三分之一。
- **认领**:一个 Lua 脚本里先续租、再 `RPOPLPUSH ready → processing:{me}`。
- **启动**:收回同名实例(上一次的自己)遗留的收据 → 登记租约(失败即拒启) → 回收一遍孤儿 → 开始消费。
- **回收**(每拍):旧版共享列表直接搬回 ready;对租约过期的其它实例,由脚本**再确认一次仍然过期**后分批搬回 ready,搬空才把它摘出登记表。
- **停机**:worker 全停之后,用新的有时限 context 把没做完的收据还回 ready 并摘掉租约(失败则靠租约过期兜底)。
- **不变量**:一个实例的 processing 列表只在它持有未过期租约的那一刻才会增长(续租与认领同一个脚本)。
  所以不存在「已被摘出登记表、却还持有收据」的列表 —— 那样的收据没人会来回收。
- **语义**:仍是 at-least-once。误回收(实例活着但租约断了)= 重复执行,被游标挡住。
- **滚动升级**:旧 → 新,旧版实例仍写旧列表、新版每拍回收它,重叠期旧实例的在途收据会被重复执行(与以前每次发布的行为相同)。
  新 → 旧(回退):旧版看不到按实例的列表;正常停机时收据已还回 ready,若新版是崩溃退出,回退前要先起一个新版实例让它回收,
  或手工把 `kafka:retry:processing:{topic}:*` 搬回 ready。

同批修掉的两处既有问题(多实例会把它们放大):

- **停机时 ordering lock 放不掉**:`Stop()` 先取消 worker 的 context,在途任务用它去释放锁必然失败,锁残留到 TTL(2 分钟),
  期间这个 key 的存盘只能在别的实例上排队。改为脱离取消信号、带 2 秒上限释放。
- rebalance 的 Cleanup 日志把切片下标当成了分区号。

`tools/merge_zone` 同批改:P4 门禁与 `kafka_db_task_queues` 审计原来只查旧的共享 processing 键,键一拆它们会恒为 0、静默放行;
现在读登记表,把每个实例的列表都查到(`dbTaskQueueKeys`)。运行手册里的手查命令同步更新(原来还把键名写错了)。

### 3.3 部署

- `db.yaml`:2 副本、显式 `RollingUpdate`(maxSurge 1 / maxUnavailable 0)、preferred 反亲和、同文件 PDB、`terminationGracePeriodSeconds: 45`。
- 本机 `go_services.ps1`:db 的 `AllowMultiInstance` 放开(默认仍起 1 个,`-Counts db=N` 可多开)。

### 3.4 限制与未做

- **K8s 上所有 zone 的 db 共用一个消费组**(`db_rpc_consumer_group`,ConfigMap 里写死;本机脚本会加 `_z<Zone>` 后缀)。
  每个 zone 只订阅自己的 topic,但同组成员的任何进出都会让**全组** rebalance:任一 zone 的 db Pod 重启,所有 zone 的存档消费一起暂停几秒。
  zone 多了这是个明显的耦合。改成按 zone 命名需要同步 `tools/merge_zone` 的 `-kafka-group` 默认值、`k8s_zone_rollback.ps1`、
  `dev_tools.ps1`、`stress_summarize.ps1`、`kafka_offset_reset.ps1` 与运行手册,而且要先确认 merge_zone 对「组里没有该 topic 位点」的判定是
  fail-closed 的(否则门禁会静默放行)。**本次没做,单列为后续项。**
  > **2026-10-16 更新**:已落码,未编译、未测试,待 Codex 验证,见 [db-consumer-group-per-zone.md](db-consumer-group-per-zone.md)。
- 有效并行度上限 = 分区数(`Kafka.PartitionCnt`,现为 10);不要配 HPA(eager rebalance,成员变化全员暂停)。
- 连接预算:每实例最多 60 条 MySQL 连接 + 每个额外落点库 8 条;2 副本 × 4 个 zone = 480,已贴近 `max_connections = 500`(见 §6)。
  > **2026-10-12 更正(只读核算,未实测)**:480 = 2 × 4 × 60,只算了 go/db 主池。同一台 MySQL 上的全部客户端要加总:
  > - go/db 每实例 60 + 8E(E 是该实例打开的额外落点库数);
  > - data_service 每实例 35(§4 / :205);
  > - java gateway 每实例约 10(未配 Hikari,按默认值推断,只在部署时计入);
  > - friend、trade 全局各 20 × 2。
  >
  > 名义上限 T = Z·(210 + 16E) + 80(Z 为 zone 数,每 zone 2 副本)。Z=4、E=0 时为 920,是 500 的 1.84 倍;默认 2 个 zone(`deploy/k8s/zones.json`)时恰好 500,零余量;滚动发布(maxSurge 1)每个 zone 还会临时再多 (60+8E)+35。
  >
  > 超限的后果不只是变慢:go/db 不特判 1040(Too many connections),按普通错误处理、消耗重试预算(上限 3),用完进 `kafka:dead:queue:*`,而死信没有自动消费者,存盘不会自动落库。
  >
  > 现在 go/db、data_service 都没导出 `sql.DBStats`,实际峰值不知道。`go/db/etc/db.yaml` 与 `internal/config/config.go` 里「MySQL 默认 max_connections=151 留得出余量」的注释也已过时。
  >
  > 修正方向(未落码):先把总账算对,配 `max_connections` 并同步 MySQL Pod 内存(现为 limits 1Gi);给 gateway 显式配 Hikari 池;导出连接池指标;额外落点库池加 `ConnMaxIdleTime`,并把 zone 库池与全局库池的大小拆成两个旋钮。详见 PROGRESS.md 2026-10-12「落点多库连接拓扑」条目。
  >
  > **2026-10-13 落码说明(G2b;已落码,未编译、未测试,待 Codex 验证;数值均待压测确认)**:上一条的修正方向已按 `docs/handoff/launch-readiness-20261013/spec_G2b.md` 落到代码与部署清单。原文与上面的更正注都不改,口径不一致处以本条为准:
  > - `max_connections` 500 → **1800**,MySQL Pod 内存 requests 512Mi → 1Gi、limits 1Gi → 5Gi(`deploy/k8s/manifests/infra/mysql.yaml`);
  > - Java gateway 显式配 Hikari 池 `maximum-pool-size: 5` / `minimum-idle: 2`(以前没配,按默认值推断每实例恒开约 10 条),K8s ConfigMap 镜像这两个值;
  > - 滚动余量按**每个 Deployment 多 2 个实例**计(maxSurge 的 1 个新 Pod + 1 个仍在优雅退出、连接还没释放的旧 Pod),范围是一个 zone 的 db / data-service / gateway 加全局的 friend、trade。上面写的「每个 zone 还会临时再多 (60+8E)+35」少算了退出中的旧 Pod,也漏了 gateway 与两个全局服务;
  > - 总账写成可执行函数 `Get-MysqlConnectionPlan` / `Get-MysqlConnectionLedger`(`tools/scripts/k8s_deploy.ps1`),输入从服务 yaml 与 manifest 读,部署写路径打印一行 `MySQL connection ledger: …`,超限打 `[MySQLConnectionLedger]OverLimit:` 开头的英文警告(开头这一段不含空格,按它 grep;只警告、不阻断)。
  >   规划档(4 个 zone、每个 go/db 实例打开其余 3 个 zone 的库、Phase 2 未开、route store 按 db_first 计入、运维余量 20)峰值 1620,对 1800 留 180;
  > - 契约测试 `tools/scripts/tests/k8s_deploy_contract.tests.ps1` 新增 9 例(批 A 7、批 B 2),当前文件 89 → 98:钉住「规划峰值 ≤ `max_connections`」、公式自检(复现上面的 920 / 500)、ConfigMap 镜像值与服务 yaml 一致等;
  >   (2026-10-15 更正注,原句不改)「89 → 98」只是 2026-10-13 落码当时的用例数,之后该文件又加了用例,总数还会变。复验时不按用例数比对,判据以套件末尾汇总行 `total=… pass=… fail=0`(退出码 0)为准;
  > - 池指标 `mmorpg_sql_pool_*{sql_client,pool}`(`go/shared/sqlpoolstats`,label 只有池类别):go/db 在 K8s 上打开 `:9160`,data_service 在 `:9260`;告警文件 `deploy/k8s/mysql-connection-alerts.yaml`;
  > - 额外落点库池拆成 `Placement.ZoneStore*` / `NonZoneStore*` 两组旋钮,加 `ExtraStoreConnMaxIdleSeconds`(默认 60),旧键 `ExtraStore*` 只作回落;`db.yaml` / `config.go` 里「151」的过时注释已更正。
  >
  > 没有做的:go/db 遇 1040 仍按普通错误消耗重试预算、进死信(tidb-phase1 文档的 L-04);MySQL 服务端连接数仍无从观测(仓库里没有 mysqld_exporter,部署方式待拍板)。
  > 运维口径(那一行怎么看、超限的两条出路、只改 my.cnf 不会重启 mysqld、apply `mysql.yaml` 会重启唯一的 MySQL)见 `deploy/k8s/README.md`「MySQL 连接总账」。§4.4 那条「现为 500」同样以本条为准。
- 读任务的缓存回写是无条件 `SET`,不持 ordering lock:在非属主实例上重试的读可能把旧值盖到新缓存上。这是既有问题
  (读与写本来就多半不在同一个分区),多实例只是多了一个入口。改成只补缺不覆盖(`SETNX`)要先确认 login 的缓存修复路径与回档工具不依赖覆盖,没在本次做。
- 持锁实例若在拿锁与落库之间停顿超过锁 TTL 且续租失败,醒来仍会把旧版本写进库(随后复核失败,游标不回退)。既有设计,彻底封死需要 MySQL 侧的版本条件写。

### 3.5 验证(交 Codex)

```
cd go/db && go build ./... && go vet ./... && go test -count=1 ./internal/kafka/... ./internal/config/...
cd tools/merge_zone && go build ./... && go vet ./... && go test ./...
pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/go_services_ports.tests.ps1
```

重点用例在 `go/db/internal/kafka/retry_ownership_test.go`(miniredis,用 `SetTime` 推进 Redis 时钟):只收回自己的、过期才回收、
脚本内复核租约、不自己回收自己、旧列表被排空、被回收后认领会重新登记、停机归还、大列表分批搬完、逐拍驱动的续租循环、消费者接线。

本机:`go_services.ps1 -Counts db=2` 起两个 db,跑 robot login-test;杀掉其中一个(`taskkill /F`),另一个应在约 40 秒内打出
`reclaimed abandoned retry receipts`(如果被杀的那个当时有在途重试),存盘继续。kind 上:两个 db Pod Ready,`kubectl delete pod` 一个,robot 存盘不中断。

## 4. data_service:每区 1 → 2 副本(已落码,未编译、未跑测试)

### 4.1 结论:它在数据正确性上本来就能多开

通读代码后的判定(2026-10-08):

- **RPC 面没有进程内业务状态**。发号段是单事务 `SELECT ... FOR UPDATE` + `UPDATE ... WHERE version=?`;名字登记靠库上的唯一键;
  存盘 / 删除是 Redis 带 token 的分布式锁 + Lua;归属映射全是 Lua 原子脚本。
- **两条落库消费者是 kafka-go 的 consumer group**:每个分区同一时刻只属于一个实例;顺序恒为先落库后提交 offset,落库幂等
  (流水 `INSERT IGNORE`、快照唯一键 + ON DUPLICATE KEY)。重叠只发生在 rebalance 瞬间,结果是「至少一次 + 幂等」,不丢不重。
- **它早就是多实例的**:data-service 每个 zone 一份,但全局库、topic、消费组都不分 zone —— 多个 zone 的实例一直在共用它们。
- manifest 里「两个实例互相等锁」的理由只在快照唯一键 `uk_snapshot_guid_nz` 还没建出来的库上成立(那时走 NOT EXISTS + 间隙锁的退路)。

### 4.2 补的两处代码

| 问题 | 改动 |
|---|---|
| 启动期迁移没有互斥:补列 / 补索引是「先查后改」,首次建库或有结构变更时两个实例会撞在同一条 ALTER 上;输的一方四个 store 全不装配,且不重试 | `store.MigrateSchema` 先取库级命名锁 `GET_LOCK('data_service.schema_migrate.<库名>')`。启动路径等满迁移时限(后到的等先到的跑完);`-migrate` 等 60s,锁忙返回 `ErrMigrateLockBusy` → 退出码 3(可重试) |
| store 装配失败的实例照样进发现池:gRPC 健康恒为 SERVING、两条 etcd 注册照常。单实例时是全挂(显眼),多实例时是**按比例失败**(隐蔽),滚动更新还会把健康的旧 Pod 换成带病的新 Pod | 新配置 `Store.Required`(缺省 false,本地行为不变)。为 true 时四个 store 任一缺失即拒启,发生在任何 etcd 注册之前。K8s 的 ConfigMap 所有档位写 true |

### 4.3 部署

- `data-service.yaml`:2 副本、`RollingUpdate`(maxSurge 1 / maxUnavailable 0)、preferred 反亲和、同文件 PDB(`minAvailable: 1`)、metrics 端口 9260。
- 新增 `data-service-migrate.yaml`(照 `trade-migrate.yaml`),目录条目登记 `MigrateJob`:staging / prod 的启动路径不建表、不种号段行,
  以前靠人手工跑 `-migrate`;现在由发布流程在 Deployment 之前跑,且恒等它 Complete。每个 zone 一份 Job,迁同一个全局库,靠迁移锁串行。
- ConfigMap 加 `Store.Required: true`、`MetricsListenAddr: ":9260"`。
- 本机脚本不用改:`go_services.ps1` 对 data_service 本来就允许多开。

### 4.4 限制与未做

- 消费并行度上限 = 分区数(transaction_log 6 / player_snapshot 3);副本超过它只增加 RPC 容量。要更高只能换 topic 代号。
- 连接预算:每实例最多 35 条 MySQL 连接;「zone 数 × 2 × 35」要算进 `max_connections`(现为 500,见 §6)。
- 未做:唯一键缺失时拒绝启动快照消费者的守卫;C++ 约定的 etcd 键提前到 wrap-up 阶段注销(现在滚动更新有一个靠 READY 过滤兜住的短窗口)。
- `GET_LOCK` 在 TiDB 上的行为没有实测(迁移在 TiDB 上本来就停在可空唯一键那一步,见 §6)。

### 4.5 验证(交 Codex)

```
cd go/data_service && go build ./... && go vet ./... && go test ./...
cd go/data_service && go test -tags=integration ./internal/store/...      # 需要本地 MySQL;新用例 TestMigrateSchema_ConcurrentFirstBootIsSerialized / _LockBusyReturnsSentinel
pwsh -NoProfile -File tools/scripts/tests/k8s_migrate_gate.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
```

kind 上:`zone-up` 后 `data-service-migrate` Job Complete、两个 Pod Ready;`kubectl delete pod` 其中一个,期间 robot 建角(要领 PlayerId 号段)
与 scene 重启(要领首个 GUID 号段)都不受影响;把 MySQL 停掉再重启一个 data-service Pod,它应当 CrashLoop(拒启)而不是带病 Ready。

## 5. 共享 Redis:单实例 → 高可用(哨兵主从 + 入口代理;清单已在 kind 实测,脚本接入已落码、契约测试未运行)

约束:C++ 侧 hiredis 没有集群客户端;`player:session:{id}` 等跨运行时契约 key 多方共写。

> 2026-10-14 注(别的会话):「按用途拆」的后续设计在 [shared-redis-split-by-purpose.md](./shared-redis-split-by-purpose.md)(G4a,只出设计、未落码)。[容量模型](./capacity-model-hok-scale.md) §4.2 判定「按用途拆」只够到 S1,在 S3 目标下要改为集群化或单元化,修订项见 [交接](../handoff/handoff-launch-readiness-and-d1-20261013.md) §4 第 5 项。
>
> **两件事的关系(2026-10-16)**:本节解决的是**可用性**(主挂了几秒内自动切换,不再是单点),「按用途拆」解决的是**容量与爆炸半径**,两者正交。
> G4a 设计稿第 8 行写明「不做哨兵 HA,HA 是后续项(§13)」,§13 第 13 条把「新实例是否也用 redis-ha 形态、补上 `-RedisReplicas` 的 apply 路径」列为后续 ——
> 本节补的正是这条 apply 路径。将来按用途拆出的每个实例可以各自选单实例或高可用形态(清单按同样写法复制一份,对象名换掉即可)。
>
> **(2026-10-19 指向注,上两段原文保留)** 上面「要改为集群化或单元化、修订项见交接 §4 第 5 项」已有结论:G4a 修订已完成,见 [shared-redis-split-by-purpose.md](./shared-redis-split-by-purpose.md) 2026-10-18 修订稿 §16~§26(第 1~4 批是设计、未落码;第 0 批 B0-1~B0-5 已落码、未编译未验证,见该稿 §17;本括注 10-22 订正)。核心路线由负责人 2026-10-17 拍板为「单元化 + 单元内 Redis Cluster 按 `{player_id}` 分片」,两者叠加;新分批见该稿 §24。本节开头的约束「C++ 侧 hiredis 没有集群客户端」在第 4 批之前仍成立,第 4 批要先接 C++ 集群客户端或支持 MOVED / ASK(该稿 §20.6)。

### 5.1 做法

- 清单 `deploy/k8s/manifests/infra/redis-ha.yaml`(2026-10-08 写成并实测):StatefulSet `redis` 3 个 Pod(每个 = redis 容器 + 哨兵容器),Deployment `redis-proxy`(haproxy 3.2 ×2),
  Service `redis` 名字与 ClusterIP 不变、selector 改指代理。代理只把连接交给「自己答 `role:master` 且至少 2 个哨兵认它是主」的节点,节点失格时掐断它上面的已有连接。
  客户端零改动。设计取舍(为什么不让客户端找哨兵、不上 Cluster、为什么要代理先到线、停机钩子的无损交接、崩溃后不以主身份重放 AOF)写在清单文件头与
  [deploy/k8s/README.md](../../deploy/k8s/README.md)「共享 Redis:高可用形态」。
- 部署脚本接入(2026-10-16):`k8s_deploy.ps1 -RedisReplicas`,0 = 按档位(与 `-KafkaBrokers` 同一判据:非 dev / 非 custom → 3,其余 1),只接受 0 / 1 / 3。
  `Apply-Infra` 在任何写操作之前:拓扑变更闸(`Get-RedisTopologyChangeVerdict`:1 → 3 放行,3 → 1 与线上 0 / 2 拒绝)→ 高可用前置(口令字符集 `Assert-RedisPasswordSentinelSafe`、
  连接上限不等式 `Get-RedisHaLimits`)→ P0-E 的重启闸(摘要按所选清单算)。1 → 3 时先 `delete statefulset redis --cascade=orphan`(`podManagementPolicy` 不可改),再 apply;
  代理 Pod 模板带脚本 ConfigMap 的摘要(脚本一改代理就滚动);`-WaitReady` 额外等 `redis-proxy`。
- 连接上限(P0-E 的接入前置):`MAXCLIENTS` 写进 redis 的 Pod 模板且与 `redis.yaml` 同值(连接总账只读那一份);代理每副本 `PROXY_MAXCONN=13000`,
  生成期核对 `PROXY_MAXCONN ×(2 + 1)+ 1000 ≤ 40000` —— 让代理先到线,Redis 到线会让代理的健康检查失败、踢掉全部会话。
- **2026-10-19 补齐**(在 origin `43284eb0b` 的 10-16 接入之上;已落码,**未运行**:契约测试未跑、未在 kind 复测,待 Codex 验证):
  - **WAITAOF 副本份数按拓扑,两处同源**:常量 `$script:MappingRedisTopology` 删掉,改为函数 `Get-MappingRedisTopology`(每次从 `Get-RedisTopology` 现取)。
    data-service 的 `RouteStore.WaitAOFNumReplicas` 与 go/db 的 `ServerConfig.RedisClient.WaitAOFNumReplicas` 两处 ConfigMap 都经同一个 `Get-RouteStoreWaitAOFNumReplicas` 渲染:单实例 0、高可用 1。
    go/db 的 `WaitAOFNumLocal` / `WaitAOFTimeoutMs` 仍镜像 `go/db/etc/db.yaml`;`db.yaml` 与 `go/data_service/etc/data_service.yaml` 的 `WaitAOFNumReplicas` 保持 0
    (本机 compose 单实例用;写 1 会让本机 go/db 的 WAITAOF 启动探针等不到副本而拒启),K8s 上由部署脚本按拓扑写。
  - **zone 写路径跟随线上**:`zone-up`、`all-up -SkipInfra` 部署 zone Go 服务且非 DryRun 时,`Resolve-RedisTopologyFromLive` 先读线上 `sts/redis` 的副本数(早于任何写操作):
    没给 `-RedisReplicas` 就跟随线上(打一行 `Redis topology: following live statefulset/redis replicas=...`),显式值与线上不同即拒绝,线上 0 / 2 拒绝,线上没有 sts 照本次解析;DryRun 不查集群。
    目的:staging / prod 还在单实例时,`zone-up` 与 zone 回滚的 Step 6 照常可用,WAITAOF 渲染始终与线上一致。
    脚本入口(早于写路径门禁)打一行 `Redis topology: replicas=<1|3> manifest=<redis.yaml|redis-ha.yaml> high_availability=<true|false> waitaof_replicas=<0|1>`。
  - **重启闸**:闸在 orphan 删除之前读线上摘要,所以 1 → 3 照样要求 `-ConfirmRedisCoreRestart`;线上与本次都是高可用时传 `-RollingHandover`,
    拒绝 / 告警文本改为「逐个滚动重启 3 个 redis Pod,每个先交接主」(单实例与 1 → 3 仍按「重启 redis-0、重放 AOF」措辞)。
  - **代理总账**:高可用形态的 Redis 连接总账额外打一行 `Redis proxy ledger: proxy_maxconn=13000 proxies=2+1 warn_at=10400 ...`;
    规划档或本次峰值超过 80% × 单个代理的 `PROXY_MAXCONN` 时打 `[RedisConnectionLedger]OverProxyWarnRatio` 告警,只告警不阻断(一个代理不在时,它的客户端全部重连到另一个)。
  - **逐节点核对**:`-WaitReady` 部署后的 maxclients 核对改为 `Test-RedisLiveMaxClients -Nodes <副本数>`,高可用形态逐个核 redis-0..2。
  - **包装入口透传**:`dev_tools.ps1` / `k8s_image.ps1` 新增 `-RedisReplicas`,非 0 才透传(0 = 下游按档位,zone 写路径跟随线上)。
  - **gate 排空的 exec 目标**:`k8s_gate_drain.ps1` 的 Redis 命令不再 `exec svc/redis`(高可用形态下 Service redis 指向 haproxy,代理镜像没有 redis-cli),
    改为 `exec statefulset/redis -c redis`(任一 redis 节点 Pod 的 redis 容器);`-h` 仍是 login 配置里的 Service 地址,经代理落到主。
  - **切换后的手工步骤**:单实例 → 高可用之后,对每个 zone 重跑 `zone-up`(WAITAOF 副本份数变 1),再在各 zone namespace 里
    `kubectl rollout restart deploy/db` 与 `deploy/data-service`(只改 ConfigMap 不会滚动它们)。

### 5.2 实测(2026-10-08,本机 kind 单节点,隔离命名空间)

滚动重启全部节点:1~3 次写超时、零丢失;正常删主:约 2.5 秒挂起、零丢失;SIGKILL 主进程:约 6.7 秒、4 次零丢失;防火墙黑洞主的网络:约 6 秒、零丢失;
整体停机再拉起(上一任主不是 redis-0):约 6 秒、数据完整;从单实例原地切换:约 25 秒、ClusterIP 不变、数据完整;带口令全部复测通过。
实测里修掉的问题(都已进清单、都有契约测试钉住):haproxy 3.0.29 的僵尸会话(换 3.2)、`tcp-check expect` 缺 `min-recv`、停机钩子先暂停写入再交接、
冷启动多等哨兵多数意见、哨兵不加引号回写口令(口令字符集限制)。**2026-10-16 加的连接上限改动(Pod 模板里的 `MAXCLIENTS` / `PROXY_MAXCONN`)没有在 kind 上复测**:
当时本机 Docker Desktop 没在运行;配置生成在本机用同一份脚本跑过、语法与产物核对过。

### 5.3 限制与未做

- 主从复制是异步的,主崩溃可能丢最后毫秒级的写;三个 Pod 同时坏两个时只读、不能自动切主;代理多一跳(未压测)。
- WAITAOF 的代价(2026-10-19,高可用形态下 db / data-service 等 1 个副本确认;推导自配置,未运行):上一条「坏两个时只读」由 `min-replicas-to-write 1` 拒写;
  此外切主后几秒内新主身边还没有从时 WAITAOF 确认失败(go/db 消息重投、data-service 提交失败重试),这段时间里刚启动的 go/db 会被 WAITAOF 启动探针拒启。
  只有走 WAITAOF 的写有副本确认,C++ 存盘、会话等其余写仍是异步复制。
- 3 → 1 只能手工回退(README 有步骤)。`-RedisReplicas` 2026-10-19 起经 `dev_tools.ps1` / `k8s_image.ps1` 透传,非 0 才透传(0 = 下游按档位,zone 写路径跟随线上)。
- 没在真实多节点集群上做过网络分区与整机故障;大数据量下 1 → 3 的全量同步耗时未知(`rollout status` 等待上限 180 秒)。
- C++ 侧:口令认证已由别的会话补上(P0-A,`redis_manager.cpp` 每条新连接先 AUTH、失败 fail-closed);高可用形态下主切换由代理掐断连接、C++ 走既有重连。
  **残余风险**:代理 Pod 所在机器整机消失时,C++ 那条连接收不到 FIN / RST,原先会一直挂着(该进程的 Redis 读写全部卡住)直到进程重启。
  Redis 节点所在机器消失不受影响(代理会掐断客户端侧)。**2026-10-16 已补,10-17 Codex 在合并后的 main 上编译 + 单次测试通过**(Windows Debug|x64;
  `RedisManagerLiveness` 5 例与 `RedisManagerAuth` 10 例全过,未跑 `--gtest_repeat` 稳定性、未在真实多节点集群上演练整机消失):`RedisManager` 加了应用层存活探测 ——
  每 5 秒对已移交给业务的连接发一条 PING,15 秒无应答就摘下连接、走既有重连,最迟约 20 秒判定;契约见 `redis_manager.h` Connect 注释「存活契约」。
  没开 TCP keepalive:连接上有未确认的数据时内核不发 keepalive 探测,恰好覆盖不到这个场景(理由见 `docs/PROGRESS.md` 同日条目)。
  摘下时在途命令以空应答结束,与主切换掐连接相同;把空应答当成业务结论的那几处调用方已由后续修复(PROGRESS「zone Redis 空应答 / ERROR 不再被当成业务结论」条)
  改掉,同样在 10-17 那轮回归里编译测试通过。

### 5.4 验证(交 Codex)

```
pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1      # 新增 6 条 Redis 高可用用例,原有 Redis 用例不应受影响
pwsh -NoProfile -File tools/scripts/tests/redis_maxclients_contract.tests.ps1
```

2026-10-19 补齐后(均**未运行**,待 Codex):

```
pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/redis_maxclients_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_gate_drain.tests.ps1
pwsh -NoProfile -File tools/scripts/k8s_deploy.ps1 -Command infra-up -DryRun -SkipPreflight -SkipGoSvc -SkipJavaSvc -RedisReplicas 3 -InfraNamespace contract-ha -NodeImage registry.invalid/test/mmorpg-node:0123456789ab
```

- `k8s_deploy_contract.tests.ps1`:origin 的 6 条高可用用例之外,新增 zone 写路径拓扑判定、跟随线上(`Resolve-RedisTopologyFromLive`)、`-RollingHandover` 文本、
  `Apply-Infra` 的共享 Redis 顺序(AST)、写路径门禁顺序(AST)、prod 档代理总账行、包装入口透传 `-RedisReplicas`;改了 db / data-service 的 WAITAOF 用例(prod 档期望 1)与 mapping 拓扑用例。
- `redis_maxclients_contract.tests.ps1`:新增 T6(高可用 `MAXCLIENTS` 与 `redis.yaml` 同值)、T7(`PROXY_MAXCONN`)。
- `k8s_gate_drain.tests.ps1`:exec 目标断言改为 `statefulset/redis -c redis`。

真实集群:staging 上按 README「切换」步骤做一次 1 → 3,再做删主 Pod、杀主进程两项,用 robot 登录压测看中断时长与丢写。

## 6. MySQL：单实例 → TiDB（2026-11-05 设计，11-06～10 批 A 落码，未编译、未在集群上验证）

方向见 [全局数据层 TiDB 决策](global-data-layer-tidb-decision.md)。10-01 时 deploy 里没有任何 TiDB 清单；此后 tidb 档（`-DatabaseBackend tidb`，缺省值）已让 db、data_service、guild、trade、friend、player_inbox、match、mail、rank、chat 的逻辑库全部连外部 TiDB（脚本不建 TiDB 集群，`-GlobalAuthorityTiDBHost` 指向入口）。还留在 infra 单实例 MySQL（`manifests/infra/mysql.yaml`，1 副本，无主从）上的只剩 `mmorpg` 逻辑库，负责人 11-05 拍板把它迁到 TiDB。批 A 做完之后，tidb 档的部署里不再有这台 MySQL；它只剩 dev 的 mysql 档用作夹具。

### 6.1 `mmorpg` 上有什么、谁依赖它

- Java 网关（`java/gateway_node`）的区目录、公告、白名单，以及这个库自己的写者闸门表。区目录读不到，选服和分配 gate 就都失败，所以它在登录链路上。
- login 作为 SQL 写者登记在这个库的闸门上（`Apply-OneGoSvc` 注释：「login 登记在网关的 mmorpg 库上」）；login 的常驻进程不写业务表。
- login 的 PasswordAuth（读 `user_accounts`）在 K8s 上没有部署，ConfigMap 里只有 DevPasswordAuth。`user_accounts` 归哪个库，文档互相矛盾：[TiDB 一期](tidb-phase1-and-launch-readiness.md) §2.7 写 zone 库，`docs/ops/db-move-runbook.md` §9 写 `mmorpg`，`gateway_tables.sql` 里没有这张表。这件事不在批 A，见 §6.5。

### 6.2 现在被固定在 MySQL 的地方（11-05 只读核对）

| 位置 | 11-05 的现状 | 批 A 的改法（已落码） |
|---|---|---|
| `k8s_deploy.ps1` 的 `New-JavaSvcConfigMapYaml` | JDBC 主机写死 `mysql.<infra>:3306/mmorpg`；不写 `mmorpg.sql.require-tidb`，而网关这一项缺省 true、prod 档不许关。按当时的配置，prod 档网关连 MySQL 本来就起不来（读代码得出，未实测） | 规则收在 `Resolve-GatewaySqlMode` 一处，写路径入口和 ConfigMap 生成都调它。主机就是 `-GlobalAuthorityTiDBHost`，所有档位都要显式给，不回落 infra MySQL（理由见下）。tidb 档写 require-tidb true、run-mode prod；mysql 档只在 dev，写 false 与 dev；staging、prod 的 mysql 档部署网关在写之前拒绝 |
| `Add-SqlWriterBootManifest` | login、gateway 的 registrar 档位写死 mysql | 一律跟 `-DatabaseBackend` |
| `Apply-OneGoSvc` 与 Java 的部署路径 | login 不加 `MMORPG_DB_PLACEMENT`，Java 路径从不加 | 两处都加，`mmorpg` 按库映射连 |
| `Apply-Infra` | `$infraManifests` 无条件 apply `mysql.yaml` | 由 `Test-InfraMysqlInUse` 决定：只在 dev 的 mysql 档、入口就是 `mysql.<infra>:3306` 时部署。staging、prod 把入口写成它，或者入口是它却选了 tidb 档，都在写之前拒绝 |
| 5 个 `*-migrate.yaml` 与 `Apply-GoSvcMigrateJob` | init 容器等待写死 `mysql.<infra>:3306`；非 dev 档先等 `deploy/mysql` 就绪 | init 容器改名 `wait-sql`，等 `Get-SqlEntryEndpoint` 给出的实际入口（`SQL_HOST`、`SQL_PORT`）；只在入口是集群内 MySQL 时先等 `deploy/mysql` |
| MySQL 连接总账 | gateway 计入 `mmorpg`；tidb 档在 K=1 时仍对照 `mysql.yaml` 的上限 | tidb 档一律对照 TiDB 预算，mysql 档对照 `mysql.yaml` |
| `mmorpg` 的建库建表 | 只靠 `mysql.yaml` 的 `MYSQL_DATABASE=mmorpg` 和 mysql-init 里的 `gateway_tables.sql`，TiDB 上没有任何入口 | 新增建表 Job `gateway-schema`，见 §6.3 |

网关不回落 infra MySQL 的理由：login 也登记在 `mmorpg` 上，而部署 Go 服务早已要求所有档位显式给入口。网关单独回落的话，网关写 infra MySQL 上的 `mmorpg`，login 写入口上的 `mmorpg`，同一个库分成两份，两个写者闸门各管一份。

MySQL 省钱档（staging、prod 的 mysql 档）现在部署不了网关：`AuthorityDatabaseConfiguration` 只在 run-mode 为 dev、test 时允许关掉 TiDB 强会话核验。要支持，得先给 `java/authority_jdbc` 加 mysql 档（同 Go 的 `authoritysql`：逐连接核对不是只读实例），这是后续项。Go 服务的 mysql 档不受影响。

### 6.3 TiDB 上的建库建表

- 库：与其余逻辑库同一口径，由运维预建，显式写排序规则（TiDB 一期 §2.6）。
- 闸门表：与其余逻辑库走同一份 `authoritysql.EnsureCatalogSchema`，由 `writer_authority -command prepare-schema -schema mmorpg` 执行，不再复制第三份闸门 DDL。本机脚本给商城建库就是这条路。
- 业务表（zone_config、zone_whitelist、announcement）和种子行：同一次调用加 `-ddl-file`，在闸门表之后执行，DDL 以网关的 `schema.sql` 为唯一来源（`GatewaySchemaContractTest` 已在真 TiDB 上校验过它）。`-ddl-file` 只认 `CREATE TABLE IF NOT EXISTS` 与 `INSERT IGNORE INTO`，表名不许带库名；先校验整份再连库，有一句不合格一句都不执行。部署脚本把 `schema.sql` 原样渲染进 ConfigMap `gateway-schema-sql`。
- Job（`manifests/java-svc/gateway-schema.yaml`）用 schema 权限账号，与其余 `*-migrate` Job 同法；网关的常驻账号不给 DDL 权限。镜像取 boot 目录 gateway 条目的 `registrarImage`（`writer_authority` 就在里面，按摘要固定），所以 gateway 条目在名册之前就要有它。排在网关 Deployment 之前；staging、prod 与 `-SqlSchemaOnly` 恒等它完成；`zone-up` 开头先渲染一次，目录条目不对在任何写操作之前拒绝。
- mysql 档（只在 dev）不跑这个 Job：集群内 MySQL 由 mysql-init 的 `gateway_tables.sql` 建这个库。
- 三张业务表补 `COLLATE=utf8mb4_0900_ai_ci`（TiDB 一期 §4 第 10 项：只写 CHARSET 的表，两边的排序规则一定不同）。这个值就是 MySQL 上这些表现在实际用的排序规则，MySQL 档的行为不变。

### 6.4 网关代码在 TiDB 上要改的一处

`AdminZoneController` 的 update 与改维护状态：「区服不存在回 404」原来靠 MySQL 默认 RR 隔离级别下 UPDATE 取的间隙锁。TiDB 没有间隙锁，RC 也没有，于是并发 create 能在 UPDATE 和回读之间插进来，出现「UPDATE 一行没改，却回 200」。

改成同一个事务里先按主键 `SELECT … FOR UPDATE`：行不在就直接回 404，不执行 UPDATE；行在就已经持有这一行的 X 锁，再 UPDATE、再回读。这样在 MySQL 的 RR、RC 和 TiDB 上都成立。create（ODKU 加回读）与 delete（按主键 DELETE、看删除行数）的论证不依赖间隙锁，不用改。

### 6.5 不在批 A 的

- login 的 PasswordAuth：`user_accounts` 归哪个库、K8s 上怎么注入、`20260803_password_auth.sql` 里的存储过程改成应用侧检查（TiDB 不支持存储过程），三件事要一起定。只在启用 PasswordAuth 时才阻塞。
- 运维侧要按 TiDB 重新产出：runtimeConfigSecret、registrar DSN、名册（新 generation，锁原语从 LOCK_TOUCH 换成 SELECT_ONLY）、物理身份摘要。写进开服手册，脚本不代办。
- 存量数据：项目未上线，不写 `mmorpg` 从 MySQL 到 TiDB 的数据迁移；已有的环境按开服手册重建。

### 6.6 批 A 的落码顺序与状态（11-10）

1. A1：网关代码与 DDL（§6.3 的 COLLATE、§6.4）。11-06 已落码，未编译。
2. A2：建表 Job（含 `writer_authority -ddl-file`），以及 `k8s_deploy.ps1` 里网关的 JDBC 与档位、两个写者的档位与库映射、连接总账。11-06～10 已落码，未编译。
3. A3：migrate Job 的等待目标改为实际 SQL 入口，`mysql.yaml` 只在 dev 的 mysql 档、入口就是它时部署。11-10 已落码，未编译。
4. A4：契约测试随 A2、A3 一起改了，没有单独成批：`logical_db_deploy_contract`（翻转 login、gateway 的两条，新增网关入口、建表 Job、`-SqlSchemaOnly`、集群内 MySQL 四条）、`k8s_deploy_contract`（总账三条换上限或加 `-DatabaseBackend mysql`）、`k8s_migrate_gate`（等实际入口一条）、`k8s_client_entry_contract`（假 kubectl 夹具补入口）。原计划的「mysql 档产物逐字不变」不做：mysql 档本身也变了（网关只剩 dev、集群内 MySQL 只剩 dev），钉逐字不变等于钉住要改掉的行为。
5. A5：`deploy/k8s/README.md`（新增「`mmorpg` 库在全球权威入口上」一节）、开服手册 §1、§5、`docs/ops/db-move-runbook.md` §9 已改。

验证命令在 `docs/PROGRESS.md` 的「批 A2、A3」条目。

## 7. guild 的 K8s 清单(第一、二块已落码:部署本体、资产通道与 gRPC 口准入;BK8s 其余块按批推进)

> 2026-10-16:帮会二期全部批次 10-09 已进主干,用户同日拍板「BK8s 全部做完」(`docs/design/guild-phase2/90-consistency.md` G-05 原写「单独授权」)。
> 第一块已落码:`manifests/go-svc/guild.yaml`(全局池 2 副本)+ `guild-migrate.yaml` + ConfigMap 生成 + 镜像登记 + 两本连接总账 + data-service 的回档帮会闸,
> 并修了两处 go/guild 代码问题(注册地址取 `ListenOn` 在 K8s 上是 0.0.0.0;MySQL 池不设上限)。说明见 `deploy/k8s/README.md`「帮会 guild」。
> 第二块已落码:`-AssetOps`(签名密钥注入 guild / trade / scene,`AssetOp.Enabled` 跟随开关,跨命令指纹核对)与 `-NetworkPolicies`(scene / guild / match 的 gRPC 口准入),
> 两者默认 off,见 `deploy/k8s/README.md`「gRPC 口准入与资产通道」。
> 后续块:告警与 Loki ruler、db migrate Job。以下为 10-01 的原约束,保留备查。


帮会二期会话 2026-10-01 答复:清单归本线补;ConfigMap 要等它通知 B6b-srv2 落完再照最终的 `guild.yaml` 写。
约束:全局服务(Etcd 显式 `Key: ""`)、2 副本标注「未验证」、`PlayerLocatorRedis` 指共享库、AssetOp 密钥走 Secret、
data_service 侧要配 `GuildInternalRpc`、NetworkPolicy 收口内部 RPC、首次起服前跑 `-migrate`。

## 8. 2026-11-05 第二轮：剩余单点的现状与处理

负责人 2026-11-05 要求把剩下的单点「修复完毕」，并拍板三件事：`mmorpg` 库迁到 TiDB；Loki 的对象存储，外部 S3 兼容存储和集群内存储两种都要支持；按项分批，在同一个会话里依次做（每批不超过 30 个文件，先写设计再落码，编译与测试交 Codex）。

| 项 | 现状（11-05 核对） | 处理 | 状态 |
|---|---|---|---|
| `mmorpg` 库（Java 网关的区目录、公告、白名单与写者闸门，login 的口令表） | 只在 infra 的单实例 MySQL 上。`k8s_deploy.ps1` 把 login、gateway 的写者档位写死为 mysql，网关的 JDBC 地址写死 `mysql.<infra>` | 迁到 TiDB，见 §6 | 批 A 11-10 已落码，未编译 |
| 控制域 Redis 全服单主 | 所有 zone 的场景路由、世界频道和选主锁在一个主节点上 | 按 zone 组分实例，见[控制面按 zone 化](control-plane-zone-scoping-20261105.md)第 5 条 | 另一会话做，5a～5c 11-05 已落码未编译 |
| scene_manager 全服一个 leader，C++ 每 2 秒全量读 etcd | 同上 | 按 zone 选主、只发现本 zone，同一设计第 4 条 | 另一会话做，4a～4c 11-05 已落码未编译 |
| 选主的后台任务 | 见 §9 | 两处改分片（C1、C2），其余保持单 leader；帮会榜维护锁核对后暂不改（§9） | C1、C2 已落码未编译 |
| 排行榜 Redis | rank 自 10-19 起以 SQL 为权威，不连 Redis。`redis-rank.yaml` 只作历史保留，部署脚本不 apply 它，契约测试「R2b Redis清单保留历史，现役SQL排行不自动部署旧写榜存储」守着 | 不是单点，不用改 | — |
| Loki | 单副本加本地盘 | 单体模式 3 副本加共享对象存储，见 §10 | 批 B 11-10 已落码，未编译 |

## 9. 选主的后台任务：哪些要分片（2026-11-05 只读核对）

判据：同一时刻只有一个副本在干活，只有当这份活随在线人数或业务量增长时，才是容量问题。只随异常或节点数增长的维护任务，单 leader 加快速接任就够；硬拆成分片只会多出跨副本聚合和重复扫描的问题。

| 选主点 | 作用域 | 每拍的活 | 负载随什么增长 | 处理 |
|---|---|---|---|---|
| `guild:assetop:maintenance:leader` | 全服 | 日对账只读 PENDING、刷 gauge；每小时最多自动放弃 200 行。重投 Tick 每个副本都跑，不归这把锁管 | 异常积压 | 保持 |
| `trade:assetop:maintenance:leader` | 全服 | 同上，只放弃 TRADE_DEBIT；`AssetOp.Enabled` 现为 false | 异常积压 | 保持 |
| `player_inbox:maintenance:leader` 下的清理、补发、gauge | 全服 | 清理已空转；补发只扫到期未通知的行；gauge 是两条聚合查询 | 近零、异常、在途量 | 保持 |
| 同一把锁下的定时对账 | 全服 | 每小时读 24 小时内由消费方终结的行，每轮最多 2000 名玩家，超出就截断 | 业务量（战斗结算也进对账） | **改分片**（11-06 已落码，未编译）：64 个逻辑片用 HRW 分给 etcd 里注册的 stable 副本（与 match 的 poolowner 同法），每个副本只对账自己的分片，gauge 按本副本的分片求和，告警仍取 max() > 0。做法见[收件箱设计](player-inbox-service.md) §9.3「分片归属」 |
| `player_locator:session_reconciler:leader` 下的写者检查 | 全服 | 每 60 秒一次 etcd 只读 Txn | 节点数 | 保持 |
| 同一把锁下的补建与日审计 | 全服 | 全键 SCAN 加逐 slot SSCAN，限速 200 页/秒 | 在线会话数；2000 万在线时一遍约 19 小时（[会话索引设计](player-locator-gate-session-index.md) §7） | **改分片**（11-10 已落码，未编译）：玩家域是 Cluster 时复用死 gate 清扫的 256 段租约，每个副本逐 slot 用 `CLUSTER GETKEYSINSLOT` 列键补成员、清脏成员，进度按段记在控制域；leader 只发布一遍、等它做完，就绪与审计的判据不变。单实例玩家域（dev）仍由 leader 自己扫。做法见会话索引设计 §4.5 |
| `guild_rank:maintenance_lock` | 全服互斥，挂在请求路径上 | 帮会积分写入、解散和启动重建都抢它；不续期，持锁者崩溃最长堵 5 分钟 | 积分写入量 | **暂不改**（11-10 核对后定）：积分写入现在只有建帮时写一次初始 0 分，`UpdateGuildScore` 这个内部 RPC 在 Go、C++ 里都没有调用方，锁上的流量就是建帮、解散的量，不是容量问题。11-10 全服榜已整体删除，剩下的只有本区榜。等有了真实的积分来源（活跃度、战绩）再改，做法见表下 |

`guild_rank:maintenance_lock` 以后要改时的做法（有了真实的积分来源时）：`guild` 行加 `score_version`，在写分的同一个事务里递增；榜上用 Lua 条件写入，只接受比已记版本更新的分数（版本记在同一区的 HASH 里），乱序到达的旧写入被丢弃，积分写入就不用抢锁。整表重建按快照里的版本做同样的条件合并，不再整键 RENAME 覆盖；只有合区折叠和重建之间仍要互斥。现在就做等于给一个不存在的负载加一套版本协议，按 AGENTS.md §11.2 的 YAGNI 不做。
| login 出队 `dispatcher:lock:login_queue:zone:<z>` | 按 zone | 每秒放行一批，受空位封顶 | 排队人数 | 保持按 zone |
| guild 历练巡检 `guild:trial:sweep:lease` | 全服 | 每 60 秒只扫超时的对局，每轮最多 200 条 | 异常 | 保持 |

另外两点：

- friend 的 `sweep.go` 没有选主，每个副本每 5 分钟跑同一批 `DELETE … LIMIT 1000`，是重复执行而不是分摊。它缺省 report_only，不删数据，本轮不改，记为后续项。
- data_service、match、mail、rank、chat、db 没有全服单例：靠消费组、HRW 或行级租约分摊。

§1 那条 2026-10-18 增补说 player_locator 对账「多开分担没有收益」，写于死 gate 清扫拆出 leader 之前，对补建与日审计已不成立，以本节为准。

## 10. Loki：单副本 → 单体模式 3 副本加共享对象存储（2026-11-05 定，11-10 落码，未编译、未在集群上验证）

落码结果：`-LokiReplicas` 与 `-LokiS3*` 参数、`Get-LokiTopology` / `Get-LokiS3Settings` / `Assert-LokiTopologyChangeIsSafe`（`k8s_deploy.ps1`），清单 `manifests/infra/loki-ha.yaml` 与 dev 夹具 `manifests/infra/seaweedfs-dev.yaml`，契约测试 `loki_ha_deploy_contract.tests.ps1`；用法见 `deploy/k8s/README.md`「Loki:单副本与单体模式 3 副本」。官方文档写明单体模式的高可用就是这种形态（多个实例、memberlist、共享对象存储、`replication_factor: 3`），并把它列为读写分离（SSD）模式在 Loki 4.0 退役后的去处之一。compactor 靠 Loki 自己的环在几个实例里选一个在做，这一点没在集群上验证。

- 形态由 `-LokiReplicas` 选，只接受 1 或 3，缺省按档位（与 `-KafkaBrokers`、`-RedisReplicas` 同一判据：非 dev 或非 custom 取 3，其余取 1）：
  - 1：现状，单副本 Deployment 加 PVC 和本地盘，只给 dev、kind 用；
  - 3：单体模式 3 副本 StatefulSet。每个 Pod 都跑全部组件（`-target=all`），既收日志也答查询；memberlist 组环，`replication_factor: 3`，写入要 2 份确认，挂一个照常写；日志块与 TSDB 索引放共享对象存储，每个 Pod 的 WAL 放自己的 PVC；PDB `minAvailable: 2`，加反亲和。
- 不用读写分离的三组（write、read、backend）：单体多副本的 Service 名和推送地址不变（`loki.<infra>:3100`），C++ 日志 sidecar 和 Grafana 都不用改；Loki 官方文档写明单体模式可以靠共享对象存储和 memberlist 横向扩展。读写分离要多一个按路径转发的网关和三套工作负载，等日志量证明单体不够再拆。
- 对象存储（负责人 11-06 定）：**生产只接外部 S3 兼容存储，本仓库不部署生产级对象存储。**
  - 3 副本形态只认 S3 接口：部署时传 endpoint、bucket、region、是否 path-style、是否 TLS；访问密钥经环境变量注入成 Secret，不进 ConfigMap，也不进日志。
  - 云上用云厂商的对象存储（S3、OSS、COS 等），可以和 TiDB BR 备份共用一套、另开一个桶。
  - 自建机房由平台层提供，标准做法是 Rook-Ceph 的 RGW。自建 K8s 本来就要一套分布式存储给 MySQL、TiDB、Kafka 的 PVC 用，Ceph 同时给块存储和 S3 接口。对本仓库来说它也只是一个外部 S3 地址，与「TiDB 由外部提供、脚本不建」同一口径。
  - dev 和 kind 上验证 3 副本时，用单进程的 SeaweedFS 当测试夹具提供 S3 接口，只在 dev 档可选，不进 staging、prod。
  - 为什么不在集群里自建生产级对象存储：负责人原定的 MinIO 社区版 2025-10 起不再发布镜像（Docker Hub 上已查不到 `minio/minio`），2025-12 进入维护模式，2026-02 仓库归档，不再出安全修复；SeaweedFS 做高可用要 3 个 master、多台 volume 和带外部元数据库的 filer，等于在仓库里再养一套分布式存储；Garage 是 AGPL，社区小，缺 bucket 策略和版本；RustFS 尚不成熟；MinIO 的社区分叉由第三方维护，安全修复没有保障。
- 拓扑变更闸：1 → 3 要显式确认（旧 PVC 里的日志不迁移，只保留 PVC 供手工导出）；3 → 1 拒绝。

## 11. 分批（2026-11-05）

| 批 | 内容 | 规模估计 | 状态 |
|---|---|---|---|
| A | `mmorpg` 迁 TiDB（§6） | 实际约 20 个文件 | A1～A5 11-06～10 已落码，未编译 |
| B | Loki 3 副本加外部 S3；dev 档可选单进程 SeaweedFS 做测试夹具 | 8 个文件 | 11-10 已落码，未编译 |
| C | §9 的三处：player_inbox 对账分片（C1）、player_locator 补建与审计分片（C2）、guild 排行锁缩小范围（C3，要加分数版本号，先确认有没有真实调用方再定） | 10～15 个文件 | C1 11-06、C2 11-10 已落码未编译；C3 11-10 核对后定暂不改，理由与以后的做法见 §9 |

`k8s_deploy.ps1` 和它的契约测试同时有发布轨道（批 2b）、控制面按 zone 化等会话在改。动之前先给对方发消息，每次改前重读要改的那一段。Java 版（0.1.0-SNAPSHOT）只在 `PARITY.md` 登记待做。

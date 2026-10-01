# 消除单节点:所有服务都能水平扩展(2026-10-01)

> 用户要求(2026-10-01):**不允许任何服务只有一个节点;同一种服务要能开很多个**。
> 本文是这件事的总纲:盘点、顺序、每一项的设计与验证口径。每做完一项就在对应小节更新状态。
> 全部改动遵守 AGENTS.md §10.1:Claude 不跑编译 / 测试,验证命令交给 Codex。

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

## 3. db(存档写库):每区 1 → 多实例(待做)

现状:`go/db/internal/kafka/key_ordered_consumer.go` 启动时 `recoverRetryProcessing` 把**全部** processing 收据搬回 ready,
两个实例会互相抢走对方正在处理的任务;部署与本机脚本因此把它钉成单实例。
方向:让每个实例只认领自己的在途任务(收据带实例标识 + 租约),分区由消费组在实例之间分配。先读代码再定稿。

## 4. data_service:每区 1 → 多实例(待做)

现状:同一个进程既是发号段 / 名字登记的 RPC 服务,又跑两条审计落库消费者;多实例时消费者互相等锁,所以钉成 1 + Recreate。
方向:RPC 部分本来就靠 MySQL 行锁保证原子,可以多实例;消费者按分区在实例间分工。先读代码再定稿。

## 5. 共享 Redis:单实例 → 高可用(待做)

约束:C++ 侧 hiredis 没有集群客户端;`player:session:{id}` 等跨运行时契约 key 多方共写。
方向待定(Sentinel 主从切换 vs. 按用途拆库后分别集群化),需要先盘点所有客户端的连接方式。

## 6. MySQL:单实例 → TiDB(方向已定,待做)

见 `docs/design/global-data-layer-tidb-decision.md`。deploy 里目前没有任何 TiDB 清单。

## 7. guild 的 K8s 清单(待做,等帮会二期最后一批落定)

帮会二期会话 2026-10-01 答复:清单归本线补;ConfigMap 要等它通知 B6b-srv2 落完再照最终的 `guild.yaml` 写。
约束:全局服务(Etcd 显式 `Key: ""`)、2 副本标注「未验证」、`PlayerLocatorRedis` 指共享库、AssetOp 密钥走 Secret、
data_service 侧要配 `GuildInternalRpc`、NetworkPolicy 收口内部 RPC、首次起服前跑 `-migrate`。

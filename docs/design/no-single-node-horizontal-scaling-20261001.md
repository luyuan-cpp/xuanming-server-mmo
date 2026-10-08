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
- 有效并行度上限 = 分区数(`Kafka.PartitionCnt`,现为 10);不要配 HPA(eager rebalance,成员变化全员暂停)。
- 连接预算:每实例最多 60 条 MySQL 连接 + 每个额外落点库 8 条;2 副本 × 4 个 zone = 480,已贴近 `max_connections = 500`(见 §6)。
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

## 5. 共享 Redis:单实例 → 高可用(待做)

约束:C++ 侧 hiredis 没有集群客户端;`player:session:{id}` 等跨运行时契约 key 多方共写。
方向待定(Sentinel 主从切换 vs. 按用途拆库后分别集群化),需要先盘点所有客户端的连接方式。

## 6. MySQL:单实例 → TiDB(方向已定,待做)

见 `docs/design/global-data-layer-tidb-decision.md`。deploy 里目前没有任何 TiDB 清单。

## 7. guild 的 K8s 清单(待做,等帮会二期最后一批落定)

帮会二期会话 2026-10-01 答复:清单归本线补;ConfigMap 要等它通知 B6b-srv2 落完再照最终的 `guild.yaml` 写。
约束:全局服务(Etcd 显式 `Key: ""`)、2 副本标注「未验证」、`PlayerLocatorRedis` 指共享库、AssetOp 密钥走 Secret、
data_service 侧要配 `GuildInternalRpc`、NetworkPolicy 收口内部 RPC、首次起服前跑 `-migrate`。

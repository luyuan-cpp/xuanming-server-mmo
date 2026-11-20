# 交接:全局单点盘点、解法与 1000 万在线容量核算(2026-10-13)

> **2026-11-19 现状(先读)**:§3 与 §4.4 逐条对 11-19 工作区的核对、当天补的两项(etcd / Kafka 成员反亲和按档位、`k8s_image.ps1` 透传 `-InfraNamespace`)与决定不改的项,见 [handoff-global-singletons-p0-20261015.md](handoff-global-singletons-p0-20261015.md) §10。
>
> **2026-10-16 状态订正(先读)**:
> - **目标已改**:负责人 10-15 确认的新目标是日活 1 亿、**峰值同时在线 2000 万**(`docs/design/capacity-model-hok-scale.md` §7)。本文 §5 按 1000 万推算,只作历史参考,不能当新目标的预算;按在线人数增长的项不能简单乘二,以该文 §7.2 为准。
> - **本文的后续工作已全部分给其他工作线,不要再按本文 §0 重新开一条**:
>   - §3 的 P0 → [handoff-global-singletons-p0-20261015.md](handoff-global-singletons-p0-20261015.md)(P0-D 并入 [grpc-muduo-transport.md](../design/grpc-muduo-transport.md) §10.2 第 2 批);
>   - 会话对账按 gate 建索引、删 `dirty_keys_set`、task:result 分频道、gate nofile、scene_manager 只读本 zone → [handoff-launch-readiness-and-d1-20261013.md](handoff-launch-readiness-and-d1-20261013.md) 与 [handoff-launch-readiness-decisions-20261015.md](handoff-launch-readiness-decisions-20261015.md);
>   - (2026-10-18 状态,B0-1)会话对账按 gate 建索引已落码,未编译、未测试。与本文 §4.3 / §5 建议的差异有三点:leader 是会话 Redis 域内全服一个(Redis 锁),不是每 zone 一个;仍是 60s 轮询 etcd、连续两拍缺席去抖,没有改成监听 gate 键的 DELETE 事件(取舍见 docs/design/player-locator-gate-session-index.md §4.1);防「leader 卡死而 is_leader 之和仍为 1」靠 Stalled —— last_success 启动预置 0,Stalled 对全体副本取 max、不按 is_leader 过滤,并带 absent(§9)。本文指出的 no-single-node §1 单点盘点漏登对账,已在该文件 §1 表后追加 2026-10-18 增补注。
>   - WAITAOF 的 go/db 部分、db 消费组按 zone、L-04、clientFoundRows、快照保留期、Kafka retention.bytes → [godb-pipeline-handoff-20261013.md](godb-pipeline-handoff-20261013.md) 与 `godb-pipeline-impl-20261015/`。注意 DBTask 9 号字段 `reply_instance` 与 `save_seq` 撞号,按 godb 交接 §5 第 13 项定;
>   - `trade_asset_op` 号段 tag 已补进四份清单。
> - **截至 10-16 没有找到归属的小项**(本文 §3 / §4):refresh token 每账号最多 32 枚不作废;Java gateway 单 zone 准入容量默认 5000;scene_manager `reassignSceneNode` 无 CAS(`go/scene_manager/internal/logic/world_init.go:528`);match `migrateLegacyQueues` 仍在;每次存盘一条 LOG_INFO。要做时另开任务,先核对当时代码。

> **性质**:只读调查与设计结论。本会话**没有改任何代码**,没有编译、测试或压测(AGENTS.md §10.1)。
> 新增的只有本文件,以及 `docs/README.md` 一行索引、`docs/PROGRESS.md` 一条记录。
> **证据级别**:「已亲核」= 本会话主线程打开代码逐行确认过;其余条目来自多路只读调查 + 对抗审查(每条结论都有第二个代理回到代码行号核对过),但仍是**静态推断**。
> 所有容量数字都是推算。按 AGENTS.md §6,没有压测对比表之前,不得据此宣称任何性能结论。
> 行号以 HEAD `73b852f32` 加 2026-10-13 的工作区为准。工作区里有他人未提交的改动(见 §9),落码前按函数名重新定位。

---

## 0. 给接手会话的开场指令(可直接复制)

```
请接手 docs/handoff/handoff-global-singletons-10m-ccu-20261013.md 的工作。
先按 AGENTS.md 会话启动门禁读文档,再读这份交接。目标规模:同时在线 1000 万、日活 1 亿(多 zone 合计,单 zone 10 万,约 100 个 zone)。
优先顺序见交接 §7。第一步只做 §3 的「与规模无关的缺陷」里标 P0 的几项,每项先让 Codex 实测核实,再落码。
注意 §9 列出的他人未提交改动不要碰;AGENTS.md §10.1:Claude 不编译不测试,改完给 Codex 可执行的命令。
每做完一项在 docs/PROGRESS.md 追加条目,并按 AGENTS.md §12 写明 Java 版状态。
```

---

## 1. 背景:用户的问题链

1. 自运营只有一个账号服务器,会不会是瓶颈?
2. 「只有一份」的组件是全游戏一份还是每区一份?怎么改成能水平扩展?
3. 全局一份的东西能不能用分布式集群扩?能不能有多份?Redis 不是每区一份吗?
4. 「加机器解决不了」的几类情况,具体怎么解决?
5. 目标规模:同时在线 1000 万、日活 1 亿(口径见 `docs/notes/redis-oom-data-safety-qa-record.md` Q8:多 zone 合计,单 zone 10 万做扎实)。

已给用户的结论要点:

- 登录服本身无状态、每区 2 副本,可以多开;真正只有一份的是它背后的**共享 Redis、MySQL**。自运营时还要加上**密码校验(Argon2id)的算力**。
- 共享 Redis、MySQL、`task:result:notify` 频道、各个 leader 锁**都是全游戏一份**。C++ 配置项叫 `zoneredis`,但所有 zone 都指向 `redis.${InfraNamespace}:6379`。
- 共享 Redis 物理全服一份是**有意的契约**,不是疏忽:D12(`docs/design/cross-zone-matchmaking.md:103`)、CZ-2(`docs/design/cross-zone-scene-travel.md:21,88,162`)。原因:scene 读不到 `PlayerAllData:{id}` 会直接当新号加载。
- 「全局一份」指的是**每条数据同一时刻只有一个写者**,物理上可以是「分片 × 每片多副本」。核心数据不能多主。
- **在 1000 万在线下,D12 / CZ-2「共享 Redis 物理全服单实例」不成立**,远在目标规模之前就会失效(见 §5)。

---

## 2. 作用域总表(今天的部署)

| 组件 | 物理作用域 | 逻辑分区 | 能否多开 | 证据 |
|---|---|---|---|---|
| 共享 Redis | 全局 1 个实例 | 键大多按玩家,少数按 zone | 否:replicas 1;redis-ha 未接入 | `deploy/k8s/manifests/infra/redis.yaml:70`;`tools/scripts/k8s_deploy.ps1:5746`(已亲核) |
| redis-match-cluster | 全局 1 套,3 主 3 从 | match / chat 私有 | 是;512mb volatile-lru 有淘汰风险 | `redis-match-cluster.yaml:27-28,59` |
| MySQL | 全局 1 个实例,1 CPU / 1Gi(2026-10-18 更正:requests 1Gi、limits 5Gi),max_connections 500(2026-10-18 更正:现为 1800,`mysql.yaml:90`) | `zone_N_db` + 全局库 + 服务库 | 否;go/db 只有一个 Hosts,不能靠配置拆 | `mysql.yaml:74,82`;`go/db/internal/config/config.go:79`;`proto_sql/store_registry.go:299-316` |
| `task:result:notify` | 全局 1 条 Pub/Sub 频道 | 逻辑上按 zone | 订阅端能多开,但扇出 = 通知数 × login 副本数 | `go/db/internal/kafka/key_ordered_consumer.go:42`;`go/login/internal/dispatcher/task_result_dispatcher.go:32,281-286`(已亲核) |
| 登录排队 leader 锁 | HEAD 全局 1 把(L-01 bug);工作区已改成每 zone 1 把(未提交) | 按 zone | 改完后每 zone 一主多备 | 工作区 `go/login/internal/logic/pkg/loginqueue/dispatcher.go:120-130`(已亲核) |
| scene_manager 选主 | 全局 1 个 leader,管所有 zone | 无 | 单写者 | `go/scene_manager/scene_manager_service.go:94-102` |
| player_locator 租约 | 全局 1 组 ZSET `player:leases*` | 无 | 所有 zone 的副本一起抢 | `go/player_locator/internal/logic/keys.go:5-13` |
| player_locator 会话对账 | 无主,每副本每 60s 全库 SCAN | 无 | 加副本更糟,O(Z²) | `session_reconciler.go:41-51,107-149,196` |
| 号段 / 角色名 / D1 路由元数据 | 全局库里的单行或单表 | 无 | 单行热点,但负载很低 | `go/data_service/internal/store/id_segment_store.go:183-185` |
| go/db 消费组 | K8s 上全局 1 组 `db_rpc_consumer_group` | topic 按 zone | 有多实例,但跨 zone 一起 rebalance | `k8s_deploy.ps1:3437` |
| 审计消费组 | 全局 1 组;transaction_log 6 分区 / player_snapshot 3 分区 | 无 | 并行上限 6 / 3 | `go/data_service/etc/data_service.yaml:236-241` |
| etcd | 全局 3 节点 | 注册前缀按 zone(C++ 发现不按) | 有 HA,写不能横向扩 | `deploy/k8s/manifests/infra/etcd.yaml:75` |
| Kafka | 全局 1 套;dev 1 broker,staging/prod 3 broker | topic 按 zone | broker 可加;分区数是契约,不能在线扩 | `no-single-node-horizontal-scaling-20261001.md` §2 |
| 每 zone 部署的服务(login / db / data_service / player_locator / scene_manager / gate / scene / Java gateway) | 每 zone 一套 | 按 zone | 是,默认 2 副本 | `deploy/k8s/manifests/go-svc/*.yaml` |
| 全局池(match / chat / trade / friend / client_rpc_router / battle) | 全局 | 无 | 是 | `k8s_deploy.ps1:944-972` |

---

## 3. 与规模无关的缺陷(先修;多数要先由 Codex 实测核实)

| 优先级 | 问题 | 证据 | 后果 | 建议 | 状态 |
|---|---|---|---|---|---|
| **P0** | **C++ 节点连 Redis 不发 AUTH**,而 release 档强制给 Redis 开密码 | `k8s_deploy.ps1:452-453`(强制注入 ≥12 位密码)、`:1862-1872`(C++ `zoneredis.password: ""`);`redis.yaml:93-96`(requirepass);`cpp/libs/engine/core/node/system/node/node.cpp:540-555` 只取 host/port | release 档 gate / scene / battle 全部 NOAUTH | 补 AUTH(重连后先 AUTH 再补发);密码走 Secret | **已亲核**;已开任务卡片 |
| **P0** | **K8s 上 battle 的 Kafka 生产者拿不到 broker** | `kafka_manager.cpp:45-52,56-63,76-79,90`:GroupID 非空就跳过 setBrokers;`node.cpp:634-637` 不检查返回值;K8s 模板 GroupID 为 `game-consumer-group`(`k8s_deploy.ps1:1830`) | battle 发往 match-rating、scene/gate 的结算和确认命令全部被拒,而且静默 | 先在 kind 上实测;成立后 Init 无条件设置 broker,模板 GroupID 改空,同时加 `DisableLegacyPerNodeTopic: true` | 静态推断(对抗审查逐行核对通过);已开任务卡片 |
| **P0** | **etcd 没有 MVCC 历史压缩** | `etcd.yaml` 未见 auto-compaction;仓库内无 Compact 调用;镜像是 `latest`(`:114`) | 发号水位每秒写 + gate 每 10s 写;10 区约 1~3 天、100 区几小时写满 2GB 配额 → NOSPACE,全服注册和发号失败 | 先测 revision/s 与 dbSize 增速,并核实镜像默认值;首选不重启 etcd 的定时 `etcdctl compact` | 静态推断;已开任务卡片 |
| **P0** | **C++ etcd 双向流 ok=false 后不重建** | `cpp/generated/grpc_client/grpc_init_client.cpp:358-368`;`etcd/etcd_grpc_client.cpp:397-405` | 任一 etcd 成员重启后,连在它上面的 C++ 节点 watch 失效、续租失效,每 180s 判失租并重注册 | 修生成器模板或在非生成的 EtcdService 里重建流(§3 不许手改 generated);**这是任何 etcd 滚动操作的前置条件** | 静态推断,待实测;与上一项同一张任务卡片 |
| P0 | Redis 高可用会丢已确认的存盘 | Kafka ack 之后,重试队列是这笔存盘的唯一副本(`key_ordered_consumer.go:2114-2124,2196-2198`);redis-ha 是异步复制(`redis-ha.yaml:24-25`) | 切主时已 ack 的存盘可能静默丢失(推断) | 重试和死信入队、owner_epoch 铸造、`account:{a}` 在**同一连接**上补 WAITAOF(参考 `go/data_service/internal/routing/route_publish.go` 的写法);确认不足时不 ack | 接 redis-ha 之前必须做 |
| P0 | go/db 连接类错误消耗重试预算,3 次后进**没有消费者的死信** | `placement_route.go:357-366`;`key_ordered_consumer.go:932-933,1135-1139` | MySQL 切主、TiDB 滚动、连接数超限都会让存盘静默不落库 | 连接类错误不耗预算、延后重试;给死信长度加告警(L-04) | — |
| P0 | 共享 Redis 没设 `maxclients`(默认 10000)(2026-10-18 更正:已显式设 40000,`redis.yaml:121`,2026-10-15 落码,生效值待实测) | `redis.yaml:95-97` 只带 `--appendonly` 和 `--requirepass` | 估每 zone 100~250 条连接,约 40~100 个 zone 就撞线(按旧缺省 10000 推,待按 40000 重核),早于命令量上限(推断) | 显式设 maxclients,按各客户端池上限算总账;由 Codex 用 `CLIENT LIST` 实测现有连接数 | 终审新增 |
| P1 | gate 没设 RLIMIT_NOFILE,全仓也 grep 不到 setrlimit | `gate-connection-admission-control.md:143`;kafka.yaml:157-176 在启动脚本里手动抬高了,gate 没有 | 容器默认软限取决于运行时(较新的 containerd / Docker 是 1024,旧版是 1048576);若是 1024,gate 台数要乘 20 | 先实测 `/proc/<pid>/limits`;再在镜像或 Pod 设 `ulimit -n ≥ 65536`(**2026-10-18 更正**:不要在镜像或启动壳里加 ulimit,那会成为第二份实现。已改为 gate / scene / battle 在 `Node::Initialize` 启动时自抬软限到 min(硬限, 1048576),抬完仍低于「客户端面准入上限 + 4096」缺省拒启,运维豁免为 `NODE_OPEN_FILE_LIMIT_MODE=warn`;硬限由节点容器运行时决定,读法、上线前置与回退见 `docs/ops/gate-kernel-tuning-runbook.md` §3.1。已落码,未编译、未验证) | 有条件 |
| P1 | Java gateway 的 zone 准入容量默认 5000 | `java/gateway_node/src/main/resources/schema.sql:8`;`ZoneConfig.java:24` | 与「单 zone 10 万」的口径冲突;AssignGate 是否按它硬拒未逐行核实 | 开服前改配置并核实拒绝路径 | 终审新增 |
| P1 | 共享 Redis 的 AOF 放在 8Gi PVC 上 | `redis.yaml:136-155` | Redis 7 多段 AOF 重写期间磁盘峰值约为数据集的 3 倍,8Gi 只撑约 2.7GB,约 13 万累计玩家(按 21KB/人) | 按数据集 × 3 预留磁盘,并显式设 maxmemory | 终审新增 |
| P1 | Kafka `retention.bytes` 每分区 512MiB 先于 `retention.ms` 生效(default 档和 prod-like 档都是) | `k8s_deploy.ps1:1186,1199`;`go/shared/kafkautil/topic_init.go:115-126` 只覆盖 retention.ms | 目标规模下 db_task 每 zone 只能缓冲约 24 分钟,审计快照约 19 秒(声明 30 天) | db_task 和审计 topic 在 topic 层覆盖 retention.bytes,保证缓冲 ≥ 2h(broker 层 segment.bytes 已是 32MiB,不用改);审计以落库为准 | — |
| P2 | 每次存盘打一条 LOG_INFO | `player_lifecycle.cpp:3743-3744` | 目标规模下全服约 5 万行/s,日志管道要单独核算 | 降为 DEBUG 或采样 | 终审新增 |
| P1 | db 消费组全 zone 共用(eager rebalance) | `k8s_deploy.ps1:3437`;`go/db/etc/db.yaml:14` | 任一 zone 的 db Pod 进出,全服存盘消费暂停;K8s 上单 zone 回档 Step 5 必然被拒 | 组名改成 `db_rpc_consumer_group_z${ZoneId}`,merge_zone / dev_tools / k8s_zone_rollback / stress_summarize 默认值同步;另起一项修回档流程顺序 | 见 `docs/design/db-task-pipeline-bottlenecks.md` S2 |
| P1 | 每次存盘 5 条 SQL,其中 4 条无效 | go/db 没开 `clientFoundRows`;proto2mysql Save 走 UPDATE→INSERT→UPDATE→SELECT FOR UPDATE(`D:/luyuan/proto2mysql/proto2mysql.go:3732-3804`,仓库外依赖 v0.2.0);`player_database_1` 生产中恒不变 | MySQL 写 SQL 放大约 4 倍 | DSN 开 `clientFoundRows=true`,或 C++ 对未变化的子表不发 DBTask | 推断,需 Codex 用真库核对 affected rows 语义 |
| P1 | `player_snapshot` 永久保留,无清理 | `DeleteOldSnapshots` 没有调用者(`snapshot_store.go:83-93`) | 目标规模下每天约 3.5TB 永久增长 | 定保留期并接定时清理,或转对象存储 / 列存 | — |
| P1 | refresh token 每次 HTTP 登录新签一枚,不作废旧枚,每账号最多 32 枚 | `go/login/internal/logic/pkg/token/token.go:40,104-119` | 目标规模下常驻约 0.26~1.3TB,现有容量文档没计入 | 每设备 1 枚、签发时作废旧枚,或落库只存哈希 | — |
| P1 | `trade_asset_op` 号段 biz_tag 不在四份 bootstrap 清单里 | `go/trade/internal/svc/assetchannel.go:54`;清单 `config.go:224`、`id_segment_store.go:46`、`data_service.yaml:216`、`k8s_deploy.ps1:3539` | staging/prod(AllowAutoSeed=false)下聚宝斋上架托管 fail-closed | 补进四份清单 | `PROGRESS.md:5504` 已登记未处理 |
| P2 | scene_manager `reassignSceneNode` 无 CAS,存在第二写者 | `world_init.go:528-543`(无条件 SET + SREM/SADD,不持 zone 锁),被 leader rebalance 与任意副本的 `initWorldScenesForZone` 共同调用(`:276-278,287,317`) | 现成的双写竞态;双 leader 时计数会双减 | 改成 Lua CAS(仅当 `scene:{id}:node == oldNode`) | — |
| P2 | login AssignGate 每次请求都对 etcd 做 range Get,超时 30s | `go/login/internal/logic/pkg/node/watcher.go:27-37`;`servicecontext.go:292-293` 丢弃 ctx | 洪峰下 goroutine 堆积;etcd 抖动直接放大成登录延迟 | ctx 透传,超时降到 ≤3s;dispatcher 一拍只取一次候选;PickGate 改成「在最低几台里随机」 | 「list-watch 本地缓存」降为有信号再做;注意 gate 排空监视必须保持 fail-closed。(2026-10-18 负责人改定:list-watch 镜像加 ctx 透传的回落直读、快路径 PickGate 随机二选一已落码,未编译未测试;排空监视仍直读 etcd。见 `docs/design/gate-load-balancing-design.md`「gate 目录怎么读、怎么挑」) |
| P2 | match `migrateLegacyQueues` 每副本每 60s 对核心做全库 SCAN | `cross-zone-matchmaking.md` §10 已写「下一版本删除」 | 白白消耗核心 CPU | 删除 | XS |
| P2 | `dirty_keys_set`:C++ 存盘 Lua 里未声明、只写不读的全服单键 | `cpp/libs/engine/infra/storage/redis_client/redis_client.h:22,50` | 内存随累计玩家增长;集群化时 CROSSSLOT | 单独小改动删掉两行 SADD,改 `currency_test`;全部升级后用 `UNLINK`(**不要 DEL**,会阻塞) | 不要在功能改动里顺手夹带(AGENTS.md §11.2) |

**注意一条与早先建议冲突的发现**:容量建模代理指出,新登录路径要靠旧的 gate Login(access_token)写入 `login_session`;按 `LegacyGateLoginEnabled=false` 关掉旧路径,建角和 EnterGame 可能都会断(`go/login/internal/logic/clientplayerlogin/loginlogic.go:102-115`,推断)。早先给用户的建议是「关掉旧 gate 登录路径,并把 kill switch 挪到 resolveAccount 之前」。**关掉之前必须先核实这条依赖。** kill switch 位于 `resolveAccount`(`:46`)之后这一点已亲核。

---

## 4. 「加机器解决不了」的五类问题与解法(按今天规模判定)

通用判据:

- 集群扩的是「很多互不相干的小东西」,扩不了「一个热点」。
- 前提是数据能按某个 key 切开,并且绝大部分原子操作只碰一片。
- 单写者先问:写入能否在提交点原子复核(CAS / 令牌)?能,双 leader 就是有界的,可以按天然分片拆锁后直接滚动(G1b 就是这样);不能(例如有外部 RPC 副作用),才需要作用域开关加 tombstone 的两阶段上线。

### 4.1 单行 / 单 key 热点

| 项 | 今天 | 解法 |
|---|---|---|
| 号段 `id_segment` | **保留**。动态 step 把每段寿命拉到 ≥15 分钟,上界约「持有者数 ÷ 900s」;100 zone 时每行约 0.44 次/s | 开服前可选:login `IdSegment.Step` 100 → 1000,改 `go/login/etc/login.yaml:189`、`deploy/login-stack.linux/login.yaml:132`,并同步 `go/login/internal/config/idsegment_conf_test.go:22-23`(不改这条测试必红)。出现信号再调门限;多行号源只在门限不够时评审 |
| 角色名 `player_name` | 保留 | 迁 TiDB 时按 L-11 预切 `name_norm` 索引:中文首字节只落在 0xE4~0xE9,要切到前两字节才能切出 16 片以上;加 `merge_option=deny` |
| D1 `route_meta` / `zone_merge_fence` | 保留 | 第 2 批规格要补:获取围栏时对 0..63 全部分片做 upsert,不能只 UPDATE 已有行(否则调大 FenceShards 后新分片可绕过围栏);TiDB 上单 zone 上限是一个 Region,要按 (zone, shard) 细切 |
| TiDB 单调主键索引尾部 | Phase 1 不动 | Phase 2(全局玩家库 PinOnCreate)之前定案。首选 **E:新号分到 K 个 `player_store_*_db` 落点库**(纯配置先在 staging 验证);次选 A:号段做双射置换(必须用大奇数乘子,2^31 是硬上限,好友推荐 pivot 要同步改) |
| player_locator `player:leases*` | 保留。单实例里全服单键和分散的键花的 CPU 一样;负载随登录/断线速率增长,不随在线人数增长 | 分片时让会话与租约族独立成实例,9 段 Lua 不动;前置是先修订 D12 |

### 4.2 跨实体多 key 原子

- 今天不分片,**组 B 的脚本全部不需要为分片而改**。
- 将来按 player_id 分片时,各类的去向:
  - 同一玩家的多键(归属铸造、CAS、C++ 存盘守卫、新号种子、身份补齐、战斗锁):收进 `{player_id}` hash tag;
  - 账号族(`account:{a}` + `account_lock:create:{a}`):收进 `{account}` tag;
  - 跨玩家、跨场景的计数(离场、选线):拆成「玩家 CAS + 计数幂等递减」,以 Java 的 resv ZSET 为模板;拆分方向偏向多计,再靠 C++ 上报对账回收(少计会让 destroy-on-empty 销毁有人的场景);
  - 组队 S_COMMIT:统一 `{team}` tag(只让脚本合法,不分散负载)。
- 依赖 SCAN 的地方要改成注册集(match 已有范本:`go/match/internal/logic/keys.go:29-65`,`{mq}` tag)。
- `owner_epoch` 永不删(`redis_client.h:31-37`),将来做冷卸载也必须保留它。
- 文档补登记:G4a §2.3 加新号种子脚本和 claim 脚本里拼键;`cross-zone-matchmaking.md` §10.0 写「player_locator 9 段 Lua 全部跨槽」。

### 4.3 全局单写者

| leader | 今天 | 解法 |
|---|---|---|
| 登录排队 | 工作区 G1b 已改成每 zone 一把锁 | 打开排队之前唯一的硬门槛是 Codex 编译加测试。建议一并做:`proc.AddShutdownListener` 注册在 `Start()` **之后**(否则 Stop 会永久阻塞);AssignGate 对 ZoneId==0 早拒;Java 版是「全局锁 + 遍历全部 OPEN zone」,语义正确,PARITY 记「已做,实现不同」 |
| scene_manager 编排 | 全局 1 个 leader | 现在只加告警和单拍耗时 histogram,并修 `reassignSceneNode` CAS;出现拍长信号时在 leader 内按 zone 并行;出现故障域信号(或打开 Agones)时再拆成每 zone 一个 leader。注意 C6:不能只改锁键,循环和 watch 也要按 zone 收窄,否则变成 100 个 leader 各自跑全部 zone |
| player_locator 会话对账 | 无主,每副本全量扫描 | 单 leader + 每页一次 MGET;补 `last_success_unix` gauge 与告警(防止 leader 卡死而 is_leader 之和仍为 1)。**目标规模下必须改成按 gate 建索引的事件驱动**(见 §5) |
| data_service 1b 路由对账 | 只有设计 | 保持全局单 leader + DB 租约;§5.4 文字要改成 `route_reconcile` 租约;跟随者先读、过期才条件 UPDATE |
| Agones 对账 | 潜伏问题 | 根因是 namespace 按调用方而不是按目标 zone 解析(`agones_binding.go:63-71,120,226,347`),分配、预占、释放、迁移都有这个问题;打开 Agones 前要和数据面一起修 |

### 4.4 共识系统(etcd)

- 写入扩不了,也不需要扩:只放低频元数据。约束要写进文档:**不许按玩家、会话、房间这类随 CCU 增长的对象建 lease**。
- **C++ 每个节点每 2s 把全部 zone 的 10 个发现前缀整段 Range 一遍**(`service_discovery_manager.cpp:16-19,28-34`),返回的 KV 数按节点数平方增长。修法:前缀按 zone 加全局池,启动 1 次 Range,之后只消费 watch。
- C++ 节点号分配从 1 开始线性 CAS 探测,每次失败等 1s(`node_allocator.cpp:68-93`)。全服冷启动时第 k 个同类节点约要 k 秒,可能超过 startupProbe 的 300s。修法:先发一次 keys_only Range 拿到已占集合再选空位,CAS 仍是唯一提交点。
- 3 成员保留;staging/prod 用 required 反亲和(dev 保持 preferred,否则单节点 kind 丢 quorum);钉镜像版本。所有需要滚动 etcd 的改动合并到一个窗口,并以「C++ 流重建验证通过」为前置。

### 4.5 广播

- `task:result:notify` 统一方案:
  - DBTask 加 `string reply_instance = 9;`(9 号空闲,`proto/db/db_task.proto:10-21`);
    - **2026-10-18 更正**:字段号最终为 **10**(9 号已给 `save_seq`,godb 交接 §5 第 13 项)。第 1 批(proto、`go/shared/taskresult`、login 双订与读任务填回复目标)已落码,未编译、待 Codex 验证;go/db 按回复目标发布与计数是第 2 批。校验规则以 `go/shared/taskresult.ValidReplyInstance` 为准(与下一条的正则同义)。
    - **2026-10-18 第 2 批**:go/db 侧已按下面两条落码(未编译,待 Codex 验证)。校验和频道名取自 `go/shared/taskresult`(规则与 `^[A-Za-z0-9-]{1,64}$` 等价);不合法时回落旧频道,计 `db_task_result_notify_total{target="legacy_invalid"}`;只单发。
  - go/db 按 `^[A-Za-z0-9-]{1,64}$` 校验后发 `task:result:notify:<id>`,不合法就回落旧频道并计数;**只单发,不双发**;
  - 新 login 永久同时订阅新旧两条频道;
  - C++ scene 也生产 DBTask(`player_lifecycle.cpp:3713`),pb 要重新生成并全量编译;
  - `sync_loader.go:77` 的调用点要一起改;
  - 结果 LIST 必须和频道在同一实例上。
- 今天的规模下它不急(父键常驻,冷读少)。但**冷玩家卸载一上线,冷读会放大约 20 倍,两件事必须一起排期**。合服(`tools/merge_zone/player_rows.go:685-750` 会 DEL 父键)后的回流也是冷读高峰。

---

## 5. 1000 万在线 / 1 亿日活容量核算

**假设**:

- Z=100(单 zone 10 万,从未压过;最大一次是 R18 单 zone 4.5 万,做不到 10 万时 Z≈220,所有 O(Z²) 项再放大约 4.8 倍);
- 每 DAU 每天 2.5 次会话,峰均比 2;晚高峰进场与离场各约 6,000/s,与「R18 每 zone 约 64/s × 100」交叉吻合;
- 断线重连约 2,000/s;密码登录约 1,200/s;
- 累计账号约 3 亿(DAU 的 2~4 倍);
- B_all≈10KB(区间 5~21KB,**仓库内没有实测值**);真人会移动,存盘跳过率按 0 算(f=1;机器人实测约 0.5);
- 存盘约 5 万次/s,其中周期存盘 N/300 ≈ 3.3 万/s,事件触发约 1.6 万/s;每次存盘 2 条 DBTask,共 1e5/s;
- 单实例能力大多是外部常识,未核实。例如 Redis 单主小命令按 5 万 ops/s 计(取保守值)。

下表已按 §10 的终审修正改过。

| 组件 | 目标规模负载 | 需要 | 判定 |
|---|---|---|---|
| 共享 Redis 核心:命令 | 约 1.73e6 命令/s(每在线玩家约 0.17),**约 60% 来自 go/db 每条写任务约 11.5 条控制面命令**;另有 Pub/Sub 投递 0.5~1.2e5/s;写带宽约 0.85GB/s(B_all 未实测,可能更大);10 分钟全服重连约 2.6e6/s | **今天的契约下只能有 1 个主**(多键 Lua 没有 hash tag)。改契约分片后约 35~52 个主;做完命令合并和对账改造后约 20~30 个,另配从库 | 必须改架构(改写 D12/CZ-2) |
| 共享 Redis 核心:常驻内存 | 父键无 TTL,随累计玩家增长:约 4.9TB(1.6TB 起,**上沿不封顶**:21KB 是机器人新号的占用,真实玩家只会更大);现网 8Gi AOF PVC 考虑重写峰值只撑约 2.7GB | 约 100~200 个主;做完冷卸载、refresh token 收口、游标迁出、删 dirty_keys_set 后 26~52 个 | 必须改架构 |
| **单实例失效拐点** | 命令约 27 万在线(约 3 个满载 zone);对账约 4 个满载 zone;内存累计约 160~310 万玩家;AOF PVC 约 13 万累计玩家;Redis 连接数约 40~100 个 zone(默认 maxclients 10000) | — | **远早于目标规模** |
| player_locator 会话对账 | GET 2Z×N/60≈3.3e7/s,外加约 4.3e7 次 SCAN/s(总键数 K≈6.5e9) | 扩容无解 | **必须改代码,优先级第一**:SetSession 时 `SADD gate_sessions:{gate}`,改成监听 etcd 上 gate 键的 DELETE 事件驱动;保留每 zone 一个 leader 的低频兜底 |
| Redis 连接数(终审新增) | 估每 zone 100~250 条(go-zero 每客户端至少常驻 8 条空闲连接,加 C++ 每线程一条,加 Lettuce),合计 1~2.5 万;默认 maxclients 10000(`redis.yaml:95-97` 未设) | 显式设 maxclients,按池上限算总账;分片后每片重算 | 规模无关的配置缺陷,立即修 |
| mapping Redis | 第 1 批约 2.2e5 ops/s;第 2 批(游标并入)约 9.2e5 ops/s;内存 30~270GB。**今天和核心是同一实例,已算在第 1 行里,不能相加** | 第 1 批 6~8 片,第 2 批 24~32 片(命令合并后 8~12 片) | 必须分片 |
| MySQL / TiDB | 约 2.65e5 SQL/s(75% 是 player_database_1 的无效 SQL,已核实到 proto2mysql v0.2.0 代码);修 clientFoundRows 后约 6.5e4;连接名义约 2.1 万(上限 500);快照每天 3.5TB、流水每天 1.1TB,都永久保留 | MySQL 方案 18 个主(修后约 5 个);TiDB 方案 20~30 TiKV、8~14 tidb-server、3 PD,存储由审计保留期决定 | 必须分片(或改 TiDB) |
| Kafka | 约 2.3e5 条/s,入流量约 470MB/s(RF3 复制约 940MB/s);约 3,200 个分区;db_task 每 zone 只能缓冲约 24 分钟,快照约 19 秒 | 单集群 14~16 个 broker;db_task 只需在 topic 上覆盖 retention.bytes(broker 层 segment.bytes 已是 32MiB) | 扩容 + 修配置(cell 化是控制爆炸半径的选择,不是容量上的必需) |
| etcd | C++ 全量 Range 约 4.4e6 KV/s、约 1.7GB/s;scene 每 2s 在主循环解析约 5,000 条 JSON(约 100ms,超过帧预算);写约 670/s,没有压缩 | 改成 zone 前缀 + 只消费 watch 增量后,3~5 成员即可 | 必须改代码;压缩配置立即修 |
| scene_manager leader | 分线目录单拍 1.6~4.7s(周期 2s);闲置回收 25~75s(周期 30s);换主后 fullSync 要 SCAN 数十亿键,约 4~10 小时;负载刷新 ∝ Z×节点数 | 每 zone 一个 leader,循环和 watch 都只处理本 zone | 必须改代码 |
| task:result 广播 | 稳态投递 0.5~1.2e5/s;冷卸载上线后约 2.4e6/s;开服或合服回流时单 zone 可达约 1e6/s | 按实例定向投递后单实例够 | 必须改代码,与冷卸载同批排期 |
| 审计管道 | 快照 1.2e4 条/s、约 85MB/s,而 3 个分区、逐条落库只能撑 600~1,500 行/s | 快照 topic 24~32 分区,`SnapshotBatchMaxRows` ≥100,定保留期并接清理任务 | 改配置 + 补清理任务 |
| login(Argon2id) | 稳态约 1,200 次 KDF/s,约 240 核;开服单 zone 只有 13~40 次/s;全服 Redis 丢数据后要在 10~30 分钟内重输密码,需 450~1,340 核 | 现有 200 副本 × 4 核即够稳态;开服 zone 要打开排队加 HPA,KDFConcurrency 调到 4~8 | 高可用即可 |
| Java gateway | 约 1.35e4 req/s,尖峰约 5e4;按 zone 部署 200 个实例,周期轮询开销 ∝ Z×节点数 | 7~25 个实例即够(改全局池);**zone 准入容量默认 5000 必须改**(`java/gateway_node/src/main/resources/schema.sql:8`) | 高可用即可 |
| gate | 约 600 台(若单 IO 线程在真实流量下只能撑 1 万人,则约 1,000 台) | 先实测容器内 `/proc/<pid>/limits` 的 nofile:默认软限取决于运行时,可能是 1024 也可能是 1048576 | 规模无关缺陷(有条件) |
| Prometheus | 约 3.4e6 条活跃序列;SM per-node 指标 ∝ Z×节点数,Z=220 时约 1.3e6 | 2~4 分片;修 SM 重复导出 | 必须分片 |
| 号段 / 角色名 | 号段 0.12~3 次/s(开服头几分钟约 17 次/s);角色名 150~4k 次写/s | 1 行 / 1 张表 | 单份够 |

### 5.1 架构结论

> **(2026-10-18 指向注,本节原文保留)** 负责人 2026-10-17 已拍板第 1、3 条的路线:核心 Redis 走「单元化 + 单元内 Redis Cluster 按 `{player_id}` 分片」,两者叠加;D12 / CZ-2 改写为「逻辑单一、物理按 `{player_id}` 分片」,C++ 要接集群客户端或支持 MOVED / ASK。第 1、3 条的展开设计已并入 [G4a 2026-10-18 修订稿](../design/shared-redis-split-by-purpose.md) §20,第 2 条的全局键见同稿 §17、§20,第 4 条账号权威落库见同稿 §22,不另起设计;以上都是纯设计,未落码。本节数字按 1000 万推算,只作历史参考,2000 万口径见同稿 §16。

1. **改写 D12 / CZ-2 契约**。真正要保住的不变量是:「同一玩家的契约键(location、session、battle:lock、PlayerAllData、owner_epoch、handoff)只有一个权威存储,任何 zone 都能寻址到它」。应改写为「逻辑单一、物理按 player_id 分片,同一玩家的键用 `{player_id}` hash tag 落在同一 slot」,底层用 Redis Cluster 或固定 slot 表做客户端分片。这是破坏性的键名变更,必须按 AGENTS.md §11.3 走双读迁移和回滚窗口;C++ 要接入集群客户端(或支持 MOVED 与重新解析地址)。
2. **全局键要么删除,要么按 zone 或分片拆开**:`player:leases*`、`dirty_keys_set`、`scene_manager:leader:lock`、`task:result:notify`,以及依赖 SCAN MATCH 的 `scene_nodes` / `world_channels`。
3. **部署形态是两者叠加**:
   - 先在单地域内按 player_id 分片。这一步必须做:一个 100 万在线的 cell 也要约 1.85e5 ops/s、约 0.5TB 常驻,仍然超出单实例。
   - 再叠加多 cell:每个 cell 10~25 个 zone、100~250 万在线,各自拥有分片 Redis、Kafka(4~6 broker)、etcd、Prometheus。服务发现、选主、对账、消费组都按 zone 或 cell 限域。
   - 跨 cell 传送需要一套新的玩家数据迁移协议,仓库内没有。
   - 跨洲时做「多个区域 cell + 全球薄层」(账号、角色名、ID、映射),不要把一个集群拉长(TiDB 跨地域要求延迟 <30ms,`tidb-phase1-and-launch-readiness.md:66`)。
4. **账号权威落库**:`account:{account}` 账号→角色目录目前只有 Redis 一份,没有可回源的库(`loginlogic.go:319` 注释,已亲核)。要落到 TiDB 全局薄层,Redis 只当缓存。

---

## 6. 自运营账号体系(问题 1 的结论与缺口)

- 密码登录每次算一次 Argon2id(m=64MiB, t=3, p=2):每进程默认 2 并发、上限 8,等槽 500ms 超时即拒;login Pod 限额 1 核 / 512Mi(`password_provider.go:24-35`、`deploy/k8s/manifests/go-svc/login.yaml:24,88`,已亲核)。估算每副本每秒 3~10 次(t_kdf 未测)。
- access_token 重连和 RefreshToken 都不跑 KDF,也不查 MySQL;压力集中在首次登录、换设备和开服。
- 会放大问题的几处:
  - 未知账号同样占 KDF 槽;
  - KDF 过载被映射成「账号/密码错误」,客户端会重试放大;
  - 网关限流默认关,打开后默认 500/s,远高于 KDF 能力;
  - 校验时用的是库内哈希自带的参数,最高放行 m=256MiB,2 并发 × 256MiB 正好顶满 512Mi(推断)。
- 功能缺口:
  - 没有注册、改密、找回密码(`user_accounts` 运行时只读,只有离线 `password_admin` 写);
  - 密码表放 zone 库还是全局库没定:各 zone 库都有 `user_accounts`,文档示例又是单库 `/game`;
  - 账号目录只有 Redis 一份。
- 建议:建全局账号库(密码表 + 账号目录持久化);`/api/login` 单独设一个按 KDF 能力折算的限流桶;加「服务繁忙」错误码(tip 码按 AGENTS.md §4 走导表器);加 KDF 指标;由 Codex 实测 t_kdf。注册和找回在 Java 版要同批做(AGENTS.md §12)。

---

## 7. 推荐顺序

| 阶段 | 内容 | 前置 / 验证 |
|---|---|---|
| **0 止血**(可并行) | §3 的 P0:C++ AUTH;battle Kafka;etcd 压缩;C++ etcd 流重建;Redis maxclients;go/db 连接错误不耗预算 + 死信告警;落定 G1b 每 zone 排队锁。另外,player_locator 会话对账改成按 gate 建索引,是目标规模下**最先爆**的一项,应排在第 3 阶段的最前面 | 每项先 Codex 实测;C++ 编译 MSBuild `/m:1` 串行 |
| **1 可用性** | 存盘关键写补 WAITAOF;接入 redis-ha(新增 `-RedisReplicas`,做删主 Pod 与网络隔离两次演练);db 消费组按 zone 命名;gate nofile;Kafka retention.bytes;clientFoundRows | redis-ha 前置:C++ AUTH + WAITAOF |
| **2 压测基线** | 按 AGENTS.md §6.2 跑 L-14;采核心 `INFO commandstats` / cpu / memory / DBSIZE;实测 B_all、B_db、t_kdf、真人存盘跳过率、单 gate / 单 scene 在真实消息率下的上限 | 先排除 player_locator 的 SceneManagerRpc 配置缺口,否则基线是脏的 |
| **3 降负载**(目标规模的硬前置) | go/db 每任务 11.5 条命令合并成 1~2 次 EVAL;对账改按 gate 建索引;refresh token 收口;删 dirty_keys_set;冷玩家卸载设计稿(与 task:result 定向推送**同批**);审计保留期与快照批量 | — |
| **4 拆分** | G4a 第 1 批(`redis-queue` + `redis-mapping`,gate 排空标记三处同批搬)、第 2 批(存盘控制面) | 设计稿 `docs/design/shared-redis-split-by-purpose.md`(他人未提交) |
| **5 数据层** | TiDB Phase 1,所有库同批切 | 单独授权(45~60 个文件,还要装 Operator) |
| **6 分片与 cell** | 写一份设计:改写 D12 / CZ-2;`{player_id}` hash tag 双读迁移;C++ 集群客户端;全局键拆分;scene_manager / 服务发现 / 消费组按 zone 限域;cell 划分与跨 cell 迁移协议 | 预计 30 个以上文件,需要授权;按 AGENTS.md §12 Java 版同批评估 |

---

## 8. 待负责人拍板

1. 是否接受「D12 / CZ-2 改写为逻辑单一、物理按 player_id 分片」。
   - **(2026-10-18 指向注)** 已拍板(2026-10-17):接受,并与单元化叠加,即「单元化 + 单元内 Redis Cluster 按 `{player_id}` 分片」。设计并入 [G4a 修订稿](../design/shared-redis-split-by-purpose.md) §20。第 3 条冷卸载见同稿 §23,第 5 条账号落库见同稿 §22;第 2 条的单元规模仍待定,列在同稿 §25。
2. cell 划分口径(每 cell 多少 zone 和在线人数),以及跨 cell 传送是否在范围内。
3. 冷玩家卸载的阈值(离线 24h?),以及它与 noeviction 口径的关系(G4a §13 第 5 条)。
4. 审计快照与流水的保留期,以及是否转对象存储或列存。
5. 账号权威落库的位置(全局薄层),以及密码表放全局库还是 zone 库。
6. 开服时是否支持单 zone 回档或合服:如果支持,db 消费组按 zone 拆就是开服门槛。
7. 关掉旧 gate 登录路径之前,先核实它对 `login_session` 的依赖(§3 末尾)。

---

## 9. 相关文档与他人在途改动(不要碰)

- 工作区里他人未提交的改动:
  - `docs/design/shared-redis-split-by-purpose.md`(G4a 设计稿,未跟踪);
  - `go/login/internal/logic/pkg/loginqueue/{dispatcher,dispatcher_test,metrics}.go`(G1b 每 zone 排队锁);
  - D1 映射持久化第 0 / 1a 批(`go/data_service/**`,见 `docs/PROGRESS.md` 2026-10-12 条目);
  - `tools/scripts/k8s_deploy.ps1`、`deploy/docker-compose.yml`(noeviction)、`tools/scripts/kafka_offset_reset.ps1`、data_service 审计 G2a 指标与 `snapshot_worker.go`(未跟踪)。
  - 完整清单以 `git status` 为准。
- 相关设计:
  - `docs/design/no-single-node-horizontal-scaling-20261001.md`:§1 盘点表漏了 scene_manager 选主、player_locator 租约与对账、审计消费组、etcd、Kafka 分区契约,可按 §2 补;
  - `docs/design/tidb-phase1-and-launch-readiness.md`:§7 L-01 / L-06 / L-14 / L-15、§8;L-06 的四路拆法与 C++ 原子域冲突,以 G4a §1.2 为准,L-06 原文需要回写更正;
  - `docs/design/global-data-layer-tidb-decision.md`;
  - `docs/design/db-task-pipeline-bottlenecks.md`;
  - `docs/design/cross-zone-scene-travel.md`(CZ-2)、`docs/design/cross-zone-matchmaking.md`(D12);
  - `docs/design/auth-provider-framework.md`、`docs/design/dual-token-authentication.md`;
  - `docs/notes/redis-oom-data-safety-qa-record.md`(千万在线口径)。
- 已开的任务卡片(用户可一键另开会话):
  - C++ 节点补 Redis AUTH;
  - 核实并修复 K8s 上 battle 的 Kafka 生产者拿不到 broker;
  - etcd 开历史压缩 + C++ etcd 长连接断线重建。
- Java 版(`D:\luyuan\xuanming-server-mmo`):以上大多是服务端内部实现,不涉及客户端契约。
  - Java 没有号段库、etcd、Redis 热缓存层,相关项登记「不适用」;
  - 排队锁登记「已做,实现不同」;
  - Java 的 game-login 是否多实例、账号与玩家表的连接池上限,本次未核实。

## 10. 终审修正(容量模型)

终审代理抽查了 13 处参数,都与模型一致。其中「player_database_1 每次存盘 4 条 SQL」已从推断升为代码事实:proto2mysql v0.2.0 的 `proto2mysql.go:3744-3805`,go/db 依赖的正是这个版本(`go/db/go.mod:10,132`),go/db 下 grep 不到 clientFoundRows。

修正(§5 的表已按这些改过):

| 编号 | 原说法 | 修正 |
|---|---|---|
| E1 | B_all 取 10KB,区间 5~21KB,21KB 是上界 | 21KB 是 R16 机器人新号的整机占用(旧的 17 键布局),更接近新号的**下界**;真实玩家的背包、任务、宠物都会增长,唯一硬上限是每行 1MiB。内存和写带宽的上沿应写「不封顶」,结论只会更坏 |
| E2 | SCAN 总键数 K≈4.7e9 | 算术错:2.7e9 + 3.75e9 ≈ 6.45e9,再加每账号一个 account_refresh ZSET 约 6.5~6.8e9;对账 SCAN 约 4.3e7 次/s;Z=1 时单副本扫一轮要 22~108 分钟 |
| E3 | 全服重连时另加「首存必写」 | 基线已取 f=1,首存落在同一槽位,不是额外负载;全服重连时核心约 2.6e6 ops/s,不是 3.4e6 |
| E4 | SM 负载刷新、per-node 指标、Java etcd 轮询 ∝ Z² | 在 CCU 固定时它们 ∝ Z×节点数,Z 从 100 到 220 时乘 2.2,不是 4.8;只有 nodes_by_role 真正 ∝ Z² |
| E5 | 进场速率与 R18 的 64~68/s/zone 交叉吻合 | R18 是机器人 18 分钟爬坡加重连的速率,不是真人稳态,吻合只是巧合。真正说明的是:每 zone 稳态约 60/s 恰好就是测过的最高值,开服时每 zone 300~500/s 是实测的 5~8 倍,完全没有证据 |
| E6 | Pub/Sub 投递算进命令 ops;冷读 300/s | 投递是输出扇出,应分开计;稳态冷读只来自新号(NewPlayerSeed 默认关,`login.yaml:255`)和合服玩家,约 120~150/s;核心命令约 1.73e6/s,另有投递 0.5~1.2e5/s |
| E7 | 「不改代码约 56 个主」 | 不改契约只能有 1 个主;56 是「改契约、不做命令合并」时的数 |
| E8 | 8Gi AOF PVC 能撑 4~8GB | 重写峰值约为数据集的 3 倍,只撑约 2.7GB;写入每 zone 约 8.5MB/s 时约每 5 分钟就要 fork 一次重写 |
| E9 | Kafka 要设 segment.bytes;「prod 档」 | broker 层 segment.bytes 已是 32MiB;档位名是 dev / prod-like / default |
| E10 | 消费组改 cooperative-sticky | sarama 可能没实现 KIP-429(外部常识,未核实);按 zone 拆 GroupID 本身就够,可加 static membership |
| E11 | gate nofile 确定是 1024 | 取决于容器运行时,要实测 |
| E12 | mapping 与核心分别计实例数 | 今天是同一实例,核心的数已包含 placement MGET,不能相加 |
| E13 | etcd / scene_manager / task:result / 审计 / Kafka「必须改架构」 | 降为「必须改代码或配置,改完单集群就能承载」;Kafka 是「扩到 14~16 broker + 修配置」。维持「必须改架构 / 必须分片」的只有核心 Redis、mapping、MySQL(或 TiDB)、Prometheus |

遗漏补充(已并入 §3 / §5):

- M1:Redis maxclients 默认 10000;
- M2:zone 准入容量默认 5000;
- M3:开服 0~30s 的尖峰。热门新区单 zone 每秒可能数千次登录,KDF 只有 13~40 次/s、排队默认关,密码登录会大面积失败;每次冷进场扇出 2×200 条投递,单 zone 1.7k/s 冷进场就约 6.8e5 条/s;
- M4:合服后回流。父键被 DEL,回流玩家全部冷读;头 5 分钟若 2~3k/s 冷进场,投递约 0.8~1.2e6 条/s,目标 zone 库约 4~6k 次 SELECT/s;
- M5:每次存盘一条 LOG_INFO;
- M6:battle 全局池没有建模(仓库内没有战斗并发率);
- M7:累计账号可能是 DAU 的 5~20 倍(模型取 2~4 倍),内存项线性放大。

终审结论:

1. 主判断成立。共享 Redis 物理单实例按命令约 27 万在线、按内存约 160~310 万累计玩家就失效;B_all 被低估,所以只会更坏。
2. **最先出事的是三项与业务量无关的问题**:player_locator 会话对账全量扫描(约 4 个满载 zone)、Redis 默认 maxclients 10000(约 40~100 个 zone)、etcd 没配压缩(今天的规模也可能几天内 NOSPACE)。三项都只要改代码或配置,不必等架构改造。
3. 真正需要改契约的只有核心 Redis 与 mapping(按 player_id 加 hash tag 分片),以及 MySQL 换 TiDB。
4. 交 Codex 实测的证据缺口:
   - 真实存档的 B_all / B_db 字节分布;
   - 真人移动下的写盘比例 f;
   - go/db 每条任务实际的 Redis 命令数和 SQL 条数(Redis commandstats、MySQL general log);
   - Argon2id 的 t_kdf;
   - 单 gate、单 scene 在真实消息率下的上限;
   - 容器里 gate 实际的 nofile;
   - 核心 Redis 实际连接数(CLIENT LIST)。

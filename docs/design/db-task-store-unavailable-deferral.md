# go/db 存盘任务：存储不可用时延后、不进死信（L-04）设计评审稿

> **已过期(2026-10-30)**:玩家存盘改成 scene 直接提交 TiDB 的 `player_store`([player-save-direct-commit.md](./player-save-direct-commit.md)),go/db 不再消费存盘任务,存储不可用时由 scene 的状态机退避重试。本文只作历史保留,不再描述现行行为。

> **状态**：v0.3.1（2026-10-14，Claude）。第 1 批已获负责人授权（2026-10-13，「L-04 第 1 批 + 读写超时」），**代码已按本稿落码，未编译、未测试**（AGENTS.md §10.1）。负责人同时要求「先实现代码，先不管测试，测试记录到上线待做里面」，未执行的验证列在 [godb-pipeline-launch-todo.md](../ops/godb-pipeline-launch-todo.md)。
> **来源**：`tidb-phase1-and-launch-readiness.md` 的 L-04（「go/db 遇到 1040 或拿不到连接时改为延后，不消耗重试预算；死信加消费者和告警」）；[交接](../handoff/godb-pipeline-handoff-20261013.md) §4.1 第 1 项；[瓶颈文档](db-task-pipeline-bottlenecks.md) §4.4 的缺口 G2；全局单点交接的同名 P0。附录 A「mysql-connection-budget」批 A 是起点，按 HEAD 重新核对后改写。
> **归属**：负责人 2026-10-13 定为本交接做（交接 §5 第 4 项）。
> **证据口径**：[C] 读代码确认；[D] 文档记载；[I] 推断。全部是静态阅读，没有实测。
> **缩写**：KOC = `go/db/internal/kafka/key_ordered_consumer.go`；PR = `go/db/internal/kafka/placement_route.go`；SR = `go/db/internal/logic/pkg/proto_sql/store_registry.go`。行号按改动前的 HEAD `0aa1027bc`。

---

## 0. 结论速览

1. **问题比「3 秒内耗尽重试」更紧。** 重试循环同一拍内可以反复认领同一条任务，MySQL 抖动不到 1 秒就可能把一笔存盘推进没有消费者的死信（§1）。
2. **第 1 批（已落码）**：
   - SQL 调用点给驱动错误打来源标记（新包 `go/db/internal/storeerr`），汇总处只认标记；
   - 写任务遇到存储侧故障延后，不耗重试预算；
   - 已打开的库只在**探活确认整库不可达**时才进入冷却（连接数满不算），冷却 5 秒 ±20%，到期只放一次探活；
   - 读任务：库打不开或在冷却时立即回失败回执；其余情况照常重试，预算用完丢弃，读永远不进死信；
   - 重试循环每实例每拍对每条至多认领一次，主库冷却时整拍跳过；
   - 业务连接池补拨号 / 读 / 写超时，会话级锁等待短于读超时；
   - 延后、死信、读放弃三个计数，三个队列长度，冷却库个数；五条告警。
3. **第 1 批之后 L-04 仍未关闭**：死信的查看与重新入队工具在第 2 批（§4.1），未获授权。
4. **管不到的窗口**：客户端超时撤不回已经发出的语句。会话级锁等待让服务端先中止排队中的语句，挡住了最常见的形态（排在锁后面）；网络在语句发出之后断开这类形态仍可能留下事后生效的写，条件写落地之前它是发版红线（§8）。

---

## 1. 现状（改动前的 HEAD 事实）

### 1.1 一笔写失败之后

- handler 返回的是字符串：`handleDBWriteOp` 把驱动错误 `fmt.Sprintf` 成文本（KOC:829-830），`runDBOp` 再 `errors.New`（KOC:2040-2042）。上层无法用 `errors.Is` / `errors.As` 判断是什么错 [C]。proto2mysql v0.2.0 自己全程 `%w` 包装，类型丢在 go/db 这三行。
- 失败的任务进 `deferFailedTask`（KOC:2072-2125）：Kafka 来源的 LPUSH 到共享的 ready 列表后 ack；重试来源的把收据搬回 ready。Kafka 位点照常前进，ready 里那一份是唯一副本。
- 重试预算在认领时判定（KOC:1177-1191）：`RetryCount >= 3` 进死信，否则加一。一笔写总共执行 4 次。
- 死信 `kafka:dead:queue:{topic}` 没有消费者、没有重放工具、没有指标、没有告警；全仓唯一的读者是合服预检的 LLEN [C]。

### 1.2 哪些失败不耗预算

`retryBudgetExempt`（PR:357-367）只豁免四种：排序锁被占、落点冻结、复核发现落点变了、落点库打不开（只对写）。SQL 返回的任何错误都耗预算。「落点库打不开」只覆盖从没打开过的额外库：`StoreRegistry.Store` 对已打开的库直接返回缓存的句柄，不探活（SR:281-287），所以主库任何时候都不在豁免里 [C]。

### 1.3 重试没有最小间隔

重试循环 1 秒一拍、每拍最多认领 200 条（KOC:49、:1141-1154）。搬回 ready 是 LPUSH 到队头、认领从队尾取，循环只在「队列空」或「满 200 次」时结束，同一条任务可以在同一拍里被反复认领 [C]。

### 1.4 MySQL 卡住时什么都不发生

`newMysqlConfig`（`proto_sql/db.go:108-121`）只设了 `sql_mode`，没有任何超时；handler 也不传 ctx。MySQL 卡住时 SQL 既不返回也不报错：子分片协程阻塞 → 通道堵到全实例唯一的重试协程 → `Stop()` 挂在等待上 [C]。

### 1.5 后果

连接数超限、主从切换、MySQL 重建（`mysql.yaml` 是 `strategy: Recreate`）、TiDB 滚动，都会让在跑的 go/db 把存盘推进死信。在线玩家不受影响（在线数据以 Redis 父键为准），但 MySQL 落后；父键一丢就是回档。

---

## 2. 原则

**重试预算只花在任务自身的问题上。** 数据超限、表结构不符、消息解不出来，重试多少次都不会好。存储此刻连不上、连接数满、只读、忙，是环境问题，会随外部状态自己消失；为它耗预算，就是把一笔完好的存盘丢掉。这与现有豁免的理由一致（PR:349-356）。

**分类只在 SQL 调用点做。** Redis 侧的错误同样可能是 `net.Error`、`io.EOF`、超时。如果在重试豁免、延后计数这些汇总处对任意错误跑类型判断，一次 Redis 抖动就会被算成 MySQL 不可用（评审 F2）。

**读任务不同。** 读不改数据，等待它的 login 最多等 8 秒（`entergamelogic.go:380`）。读没有「不能丢」的东西。

---

## 3. 第 1 批设计（已落码）

### 3.1 handler 返回 `error`

- `dbOpHandler` 改为返回 `error`；`handleDBWriteOp`、`handleDBReadOp` 用 `%w` 包住经 `storeerr.Tag` 打过标记的驱动错误；`runDBOp`、`publishDBOpResult` 一并改为 `error`。
- 测试替身 `recordingHarness` 同步改签名。
- 最终签名：

  ```go
  type dbOpHandler func(ctx context.Context, store *proto2mysql.DB, task *db_proto.DBTask, msg proto.Message) error
  ```

  条件写之后会在这个基础上再扩展（见 [db-write-fencing-mysql.md](db-write-fencing-mysql.md)；据「玩家消息投递与位置管理」会话 2026-10-14 转告，负责人已在那边确认由它落码）。

### 3.2 错误分类（`go/db/internal/storeerr`）

| 类 | 判据 | 指标 reason | 写任务 | 库级冷却 |
|---|---|---|---|---|
| 连接数满 | MySQL 错误号 1040、1203、1226 | `conn_refused` | 延后，不耗预算 | **不进**：服务端在应答，池里已建立的连接照常可用 |
| 连接断开 / 连不上 | 1053（服务端正在关闭）；`driver.ErrBadConn`；`mysql.ErrInvalidConn`；`sql.ErrConnDone`；`io.EOF` / `io.ErrUnexpectedEOF`；`context.DeadlineExceeded`；任何 `net.Error`（拨号被拒、拨号超时、连接重置、读写超时）| `conn_lost` | 延后，不耗预算 | 触发一次探活，探活也连不上才进 |
| 只读 | 1290、1836 | `store_readonly` | 延后，不耗预算 | 不进 |
| 忙 | 1205（锁等待超时，含会话级锁等待触发的）、1213（死锁）、TiDB 9007、8002 | `store_busy` | 延后，不耗预算 | 不进 |
| 其它 | 以上都不是 | `other` 等 | 照旧耗预算 | — |

- `context.Canceled`（分区被收回、进程退出）不归入任何一类，走原有路径。
- 1226 是账号级资源上限：当前部署没有按账号限连接，不会出现，为附录选项 G「每个服务独立账号 + MAX_USER_CONNECTIONS」预留（评审 F9）。
- 1129（来源被 `max_connect_errors` 拉黑）不纳入：K8s 上的 MySQL 开了 `skip-name-resolve`（`deploy/k8s/manifests/infra/mysql.yaml`），不走主机缓存，不会出现；TiDB 没有这个机制。
- TiDB 错误号只收了 proto2mysql 集成用例里的可重试集合；其余 TiDB 瞬时错误（9001~9005 一类）等 TiDB 阶段按官方文档核对后再加。
- **未知 MsgType**（发版顺序错位时旧 go/db 不认识新消息类型，瓶颈文档 G2 点名过）本批不处理：仍耗预算进死信，依赖瓶颈文档 §4.5 的发版顺序，重放等第 2 批工具（评审 F5）。

### 3.3 写任务：延后，不耗预算

- `retryBudgetExempt` 增加一个分支：带存储侧标记的错误且 `op == "write"` 时豁免。只认标记。
- 载荷格式不变，新旧实例可以互相认领对方的收据；旧实例遇到连接错误照旧耗预算，混跑时最坏退回今天的行为。
- 先后顺序仍然靠排序锁和游标：延后的旧写回来时，如果同一玩家更新的写已经落库，它会被游标判过期丢弃（KOC:1579-1588）。超时引起的例外见 §8。

### 3.4 库级冷却：只在确认整库不可达时进入

`StoreRegistry` 原有一套「打开失败后冷却」（SR:49-54、:317-319），只管没打开过的库。本批给已打开的库补上同类机制（`proto_sql/store_health.go`），但进入条件更严（评审 F1，原稿的「任意一次连接类错误即冷却」会把 1040 这种局部故障放大成整个实例停写）：

- **进入**：`runDBOp` 只把 handler（SQL 这一步）的结果报给注册表。错误属于「连接断开 / 连不上」时，注册表对该库做一次有时限的探活（2 秒，同一个库并发只做一次）；**探活本身也属于「连接断开 / 连不上」才进入冷却**。探活拿到 1040 一类的服务端应答说明库活着，不冷却。
- **探活不经过连接池**：用建池时的连接器新建一条连接、Ping、关掉（`GameDB.Connector`）。经过连接池会误判：池里的空闲连接可能早已失效（Windows 上 go-sql-driver 不做空闲连接活性检查，本地重启 MySQL 后每次探活都会拿到一条死连接），池满时取连接还要排队到超时。握手阶段被 1040 拒绝时驱动返回 `*mysql.MySQLError`，照样识别成「服务端在应答」（复核意见 1）。
- **期间**：`Store()` 对该库返回同时包着 `ErrStoreUnavailable` 与 `ErrStoreCoolingDown` 的错误，之后的路径全部是现成的：`routeTask` 包成 `errPlacementStoreUnavailable`，写延后且不耗预算。
- **退出**：冷却到期后只放一次探活（并发请求共享结果），成功才放行，失败就续上一个冷却期；冷却期内任何一条 SQL 成功也立即解除。进入 SingleFlight 之后先按最新状态复查：别的请求刚续上冷却或刚解除，就直接用那个结论，不再探活（复核意见 2）。
- **时长**：5 秒 ±20% 抖动。短于登录的 8 秒等待（冷却期内的冷登录会立即失败）；抖动是为了所有 zone、所有副本不在同一刻到期、同时重连（评审 F7）。
- 「忙」「只读」「连接数满」都不进冷却：它们是某条语句或某条新连接的事。
- 注册表接口没有改，冷却通过 `StoreResolver` 的可选接口 `storeHealth` 接入（`internal/kafka/store_failure.go`）；测试替身不实现它就没有冷却。

### 3.5 重试循环

- 每拍开始时读一次 ready 的长度 `n`，本拍最多认领 `min(n, 200)` 条。**每实例每拍对每条至多认领一次**；R 个实例共用 ready 时，3 次预算最短约 3/R 秒（K8s 上 2 副本，滚动发布时 3 个）。要真正保证全局间隔得把名额做成队列的属性，属于另一批（评审 F4）。LLEN 失败时回落到固定的 200。
- 本 zone 主库在冷却期内时整拍跳过：此时每条任务都只会在选库处被挡回，认领只是白花 Redis 往返。**代价：主库整个不可用期间，落到其它库的重试任务也不排空**。冷却只在整库不可达时出现，所以这个耦合只在真正的停机里生效。
- 每拍顺便采样三个列表的长度（ready、本实例的 processing、dead），喂给 §3.7 的指标。

### 3.6 读任务

| 情形 | 处理 | 理由 |
|---|---|---|
| 选库时库打不开或在冷却期内 | 立即 `failReadTask` 回失败回执并 ack，不进重试队列；计 `db_task_read_abandoned_total{reason="store_unavailable"}` | 库已知不可用，login 等满 8 秒也等不来结果 |
| 已打开的库上第一次撞到连接类错误 | 照常重试（耗预算）| 秒级抖动仍能被 1 秒一拍的重试吸收（评审 F6）|
| 重试次数用完 | 丢弃收据，计 `{reason="budget_exhausted"}`，**不进死信** | 读没有可保全的东西；读进死信会让死信告警失去判别力 |

对登录的影响：库已确认不可达期间，需要读库的冷登录（Redis 父键缺失的玩家）立即失败，而不是等满 8 秒后失败；当前 Unity 客户端收到失败后直接断开回选服（`entergamelogic.go:439-440`）。父键存在的玩家不读库，不受影响。这改变了 `player-storage-placement.md` §6.1 实现口径里「读照常耗预算，几次后进死信」的说法，那份文档已同步。

### 3.7 指标

都是低基数，不带 player_id。计数器在启动时以 0 建出全部 reason 的序列，`increase` 能抓到第一次。

| 指标 | label | 计数点 |
|---|---|---|
| `db_task_deferred_total` | `op`；`reason` ∈ conn_refused / conn_lost / store_readonly / store_busy / store_cooldown / store_open_failed / ordering_lock_busy / lease_lost / placement_frozen / placement_moved / expand_lock_busy / redis_unavailable / other | `deferFailedTask` 一处 |
| `db_task_dead_letter_total` | `reason` ∈ budget_exhausted / malformed_payload / legacy_payload / invalid_partition / ordering_incomparable / stale_topic / missing_required / poison | 每个死信写入点写入成功之后；毒消息计在 `persistPoisonMessage` 里（`ConsumeClaim` 在分发前解码失败时直接调用它，评审 F10）|
| `db_task_read_abandoned_total` | `reason` ∈ store_unavailable / budget_exhausted | §3.6 |
| `db_retry_queue_length` | `queue` ∈ ready / processing / dead | 重试循环每拍采样。ready 与 dead 是 topic 共用的列表，每个副本报同一个数 |
| `db_store_cooldown_stores` | `pool` ∈ home / extra_zone / extra_non_zone | 每次抓取时按注册表现算，只数没到期的冷却（自定义收集器）。不在进出冷却时设值：冷却到期后要等下一次选库探活才删记录，某个库到期后再也没有流量时设值式的指标会一直挂着（复核意见 3）|

- `redis_unavailable` 的判定：没有存储侧标记、但错误链上有 `net.Error` 或 `context.DeadlineExceeded`。SQL 调用点的网络错误都打过标记，剩下的只可能来自 Redis（守卫、选库、复核、缓存）。这些失败本批仍耗预算（§4.3），只先数出来。
- 连接池指标已由 G2b 导出（`mmorpg_sql_pool_*{sql_client="db", pool}`），本批没有注册第二个收集器。

### 3.8 告警

新文件 [`deploy/k8s/db-task-retry-alerts.yaml`](../../deploy/k8s/db-task-retry-alerts.yaml)，写法同 `owner-epoch-alerts.yaml`（`PrometheusRule`，手工 `kubectl apply`），按 namespace（即 zone）聚合：

| 告警 | 条件 | 级别 |
|---|---|---|
| `DbTaskDeferredByStore` | 写任务因存储类原因延后的速率大于 0，持续 2 分钟 | critical |
| `DbStoreCoolingDown` | 有库在冷却，持续 1 分钟 | critical |
| `DbTaskDeadLetterAdded` | 10 分钟内有新死信 | warning（工具落地前）|
| `DbTaskDeadLetterNonEmpty` | 死信列表非空，持续 1 小时 | info |
| `DbTaskRetryQueueGrowing` | ready 超过 1000 条持续 10 分钟 | warning，阈值待压测确认 |

- **死信告警的处置**写在告警描述里：保留现场，不要清空；按日志归因；上报。**不要用 redis-cli 把条目搬回 ready**：预算耗尽的条目里重试次数已满，下一拍认领时会被立刻送回死信（评审 F5）。
- 存量环境上线前先记录各 zone 的死信长度，否则 `DbTaskDeadLetterNonEmpty` 部署后常亮。
- **前提**：K8s 上 go/db 的指标端口能抓到。HEAD 上 `k8s_deploy.ps1` 的 db ConfigMap 已写 `MetricsListenAddr: ":9160"`（G2b 补完，2026-10-14 的 HEAD），但仓库不部署 Prometheus，规则要手工 apply；前提不满足时告警是哑的。不依赖指标的等价观察办法：直接 LLEN 三个列表，加上 grep 冷却与延后日志。
- 另一会话的 `deploy/k8s/mysql-connection-alerts.yaml` 在描述里写着「go/db 把 1040 当普通失败 → 进死信」。本批部署后那段描述会过时；分工上连接数与连接池归那份，重试、延后、死信归本批。那份文件归另一会话，本批没有改它。

### 3.9 日志

- 存储不可用引起的延后：每个原因每 10 秒只打一行 ERROR，带上间隔内被压掉的条数（go-zero 的 logx 没有 WARN 级别）。原来是每条每轮一行 ERROR。
- 落点库**打开失败**沿用原来的逐条 ERROR（`player-storage-placement.md` §6.1 v2.2 的口径，有用例钉住）。
- 进入、续上、解除冷却各打一行。
- 读任务因存储不可用回失败回执：逐条只打 DEBUG，同样限速打一行 ERROR。

### 3.10 业务连接池的超时与会话级锁等待

新增配置（`ServerConfig.Database`，写 0 或不写取默认，写负数表示不设）：

| 键 | 默认 | 作用 |
|---|---|---|
| `DialTimeoutSeconds` | 5 | 拨号超时。对端不回包时（节点宕机而端点没摘、丢包），没有它拨号要等操作系统重试耗尽（评审 F8）|
| `ReadTimeoutSeconds` | 30 | 读超时：MySQL 卡住时语句不再无限挂住 |
| `WriteTimeoutSeconds` | 30 | 写超时 |
| `SessionLockWaitSeconds` | 10 | 会话级 `lock_wait_timeout`（元数据锁等待，MySQL 默认一年）|
| `SessionInnodbLockWaitSeconds` | 10 | 会话级 `innodb_lock_wait_timeout`（行锁等待，MySQL 默认 50 秒）|

- 只用于业务连接池（本 zone 库与按需打开的落点库，`newServiceMysqlConfig`）。迁移入口（`cmd/migrate`）与建库仍用不带超时的配置：DDL 可能合法地跑几分钟，迁移 runner 在自己的会话上另设锁等待。
- **会话级锁等待必须短于读超时**（评审 F3）：客户端读超时只关掉自己这一侧的连接，已经发给 MySQL 的语句不会被撤回。让服务端先以 1205 中止排在锁后面的语句，超时之后就不会留下事后生效的在途写。启动时打印一行生效值，并检查这个关系，不满足打 ERROR（不拒启：它削弱的是窗口保护，拒启等于停掉整个 zone 的存盘）。
- TiDB 是否接受 `lock_wait_timeout` 这个会话变量没有核实；切 TiDB 时若连接因它失败，配置写 -1 关掉。
- 本地打开 `AutoMigrateSchema` 时，启动期建表 / 补列也走业务池，大表上可能撞读超时（只影响 dev）。
- K8s 的 db ConfigMap 不写这几项，走默认值；没有改 `k8s_deploy.ps1`。

---

## 4. 第 2 批（未授权，只列方向）

### 4.1 死信工具

不建议做自动消费者：任务进死信都有原因，自动重放等于把分类白做。建议一个命令行工具（例如 `go/db/cmd/deadletter`）：

- 列出：按格式、表、原因汇总。死信里混着四种格式（带分区的重试载荷、不带分区的旧载荷、裸 DBTask、带 `0x03` 前缀的毒消息 JSON）[C]。
- 查看：解出单条。
- 重新入队：搬回 ready，重试次数清零；默认只演练。顺序安全靠游标（以及将来的围栏）。
- 新进死信的条目带上原因和时间。死信没有程序读者，加格式不影响兼容。

条件写上线后 `stale_owner` / `legacy_after_seq` 也进死信，那份设计的处置依赖这个工具。

### 4.2 主库不可用时原地等待

第 1 批把失败的写搬进 Redis 的 ready 列表：库不可用期间，新到的写以存盘速率持续流进这个列表（每条是整行数据），Redis 是共享的单实例。另一种做法是 Kafka 来源的写在主库冷却期间不搬走、不 ack，在 worker 上等冷却结束，积压留在 Kafka。是否做取决于「存盘速率 × 单条大小 × 可容忍的不可用时长」与 Redis 内存余量，需要 R20 的数据。今天这些载荷同样堆在 Redis（堆在死信里），第 1 批不比现在差；但 1040 这类局部故障下，第 1 批堆进 Redis 的也只是失败的那几条（冷却不因 1040 进入）。

### 4.3 Redis 侧的环境故障

同一条原则也适用于这些现在耗预算的失败：落点查询失败、复核读失败、游标读失败、epoch 守卫读失败、游标写失败、缓存发布失败。它们多数只在「落点 Redis 与主 Redis 分开部署、且只有其中一个不可用」时才会真的耗到预算。本批只用 `redis_unavailable` 数出来。

---

## 5. 兼容与上线

- 不改 proto、不改载荷格式、不改 Redis 键、不做 DDL。
- 新旧实例并存：新实例对存储不可用的写不耗预算，旧实例照旧耗。最坏退回今天的行为。
- 逐 zone 滚动，一次一个 Pod；每个 zone 观察 `db_task_deferred_total` 平时为 0、死信长度不变、`db_task_stage_seconds` 没有回归。
- 回滚：换回旧镜像即可，没有需要清理的状态。新加的配置键旧版本会忽略。
- 发布纪律照瓶颈文档 §4.5。
- 故障注入只在 kind 或 staging 做。

**对交接 §2.4 的影响**：本批在某个环境的所有 zone 部署之后，在那个环境 apply 会重建 MySQL 的清单不再丢盘，但仍是一次存储中断：期间的写堆在 Redis 的 ready 列表里，**这些存盘只在那里有一份，同一窗口不得重启或清空 Redis**。要在低峰做，并先确认 Redis 内存余量。部署之前，§2.4 的约束原样有效。

---

## 6. 验证（未执行，见上线待做）

负责人要求测试记入上线待做，本节只列要验证什么；命令与通过标准在 [godb-pipeline-launch-todo.md](../ops/godb-pipeline-launch-todo.md)。

**已随代码写的单测**：

- `go/db/internal/storeerr/storeerr_test.go`：分类表（每个判据及包一层之后）、`Tag` / `KindOf`、Redis 侧网络错误不被当成存储错误。
- `go/db/internal/logic/pkg/proto_sql/store_health_test.go`：探活拿到服务端应答不冷却；探活连不上进入冷却、期内不再探活；到期单次探活决定放行或续期；语句成功解除冷却；没打开过的库不探活；冷却计数只数没到期的；到期时晚到的放行请求不再探活。
- `placement_route_test.go` 的 `TestStoreUnavailable_WriteKeepsBudgetReadFailsFast`：读任务改为立即回失败回执（原用例断言读耗预算，已按新口径改写）。

**还没写的单测**（上线前补）：`retryBudgetExempt` 新分支；`deferReason` 全表；重试循环每拍上限（含两个消费者共用一个 miniredis）；读任务预算用完丢弃；冷却期整拍跳过；死信与延后计数。

**故障注入**：暂停 MySQL 30 秒；压低 `max_connections` 逼出 1040（通过标准加一条：池内已有连接的任务照常落库，ready 不按全量入流增长）；DDL 持元数据锁时的写（核对库内的行等于最后一次写入）。

---

## 7. 改动清单（第 1 批，已落码）

| 文件 | 改动 |
|---|---|
| `go/db/internal/storeerr/storeerr.go`（新）+ `_test.go`（新）| 错误分类 |
| `go/db/internal/kafka/store_failure.go`（新）| `storeHealth` 可选接口、冷却上报、延后原因、日志限速、队列采样、读任务丢弃 |
| `go/db/internal/kafka/key_ordered_consumer.go` | handler 签名；`runDBOp` 上报；`deferFailedTask` 计数与日志；重试循环每拍上限与冷却跳过；读任务预算用完丢弃；死信计数；失租哨兵 |
| `go/db/internal/kafka/placement_route.go` | 豁免分支；读任务快速失败；`failRead` 安静版；死信标签 |
| `go/db/internal/kafka/key_ordered_consumer_test.go`、`placement_route_test.go` | 替身签名；读任务用例改口径 |
| `go/db/internal/logic/pkg/proto_sql/store_health.go`（新）+ `_test.go`（新）| 已打开库的探活与冷却 |
| `go/db/internal/logic/pkg/proto_sql/store_registry.go` | 冷却字段、选项、`Store()` 接入；落点库用业务池配置 |
| `go/db/internal/logic/pkg/proto_sql/db.go` | `newServiceMysqlConfig`、启动时打印与检查；`GameDB.Connector`（探活用）|
| `go/db/db.go` | 注册冷却计数收集器 `metrics.RegisterStoreCooldown` |
| `go/db/internal/config/config.go`、`go/db/etc/db.yaml` | 五个新配置键与默认值 |
| `go/db/internal/metrics/metrics.go` | 五个新指标 |
| `deploy/k8s/db-task-retry-alerts.yaml`（新）| 五条告警 |

不动 proto、不动 DDL、不跨语言。

---

## 8. 管不到的：超时撤不回已发出的语句

- 读超时之后驱动返回连接类错误，任务按 §3.3 延后、排序锁释放、游标不动；但那条语句可能仍在 MySQL 里排队，之后才生效。如果同一玩家更新的写先提交、这条旧写后提交，库里会停在旧版本，且游标已推进、旧写的重试会被丢掉（评审 F3 给出了完整序列）。
- 本批的处置：会话级锁等待（10 秒）短于读超时（30 秒），排在锁后面的语句会被服务端先以 1205 中止，不留在途语句。这是最常见的形态（DDL 元数据锁、长事务行锁）。
- 残余：语句已在执行或网络在语句发出后断开时，客户端超时后语句仍可能完成。条件写落地之前，这是发版红线：**同一窗口不跑会阻塞 DML 的 DDL，不暂停 MySQL**（瓶颈文档 §4.5 已有这两条）。
- 改动前没有超时，这类语句会一直占着协程和排序锁，顺序是保住的，但协程会永久卡死、拖死整个实例的重试（§1.4）。两害取其轻，加超时。
- 另一个残余（复核意见 6）：如果某条写本身就会撞上 30 秒读超时（例如极大的一行在慢链路上），它每次都被归为「连接断开」、不耗预算，会无限期重试，每轮占一条连接最长 30 秒，并触发一次探活（结果是库可达）。本批不改：为它设上限会在服务端短暂卡顿时丢盘。可观察到的信号是 `DbTaskDeferredByStore`（reason=conn_lost）持续在响而 `DbStoreCoolingDown` 不响；遇到时按单条任务排查。

---

## 9. 与其它工作的关系

| 工作 | 关系 |
|---|---|
| 条件写（[db-write-fencing-mysql.md](db-write-fencing-mysql.md)，据「玩家消息投递与位置管理」会话 2026-10-14 转告，负责人已在那边确认由它落码）| 本批先落地。条件写在 `error` 签名上扩展；它的 `stale_owner` / `legacy_after_seq` 进死信，依赖本批的死信告警与第 2 批的工具 |
| G1 治标小 PR（交接 §4.1 第 2 项）| 读写超时已并入本批；其余几条（handler 接 ctx、缓存回写挪到核锁之后、续租退避、失租计数）不在本批 |
| G2b（另一会话）| 连接池指标与 db ConfigMap 的指标端口已由它补齐；它的 `mysql-connection-alerts.yaml` 与本批的告警分工见 §3.8 |
| WAITAOF | 负责人 2026-10-13 定为归本交接（交接 §5 第 17 项）。改的是同一段代码（`pushPayloadDurably`、`moveRetryReceipt` 与各死信写入点）；本批让更多写在 ready 里停留更久，Redis 接高可用之前更值得做。未授权开工 |
| 重试只交给分区属主（④）| 负责人定为先做并修订 TiDB 决策 D5。同一段代码，本批不改重试的归属 |

---

## 10. Java 版（AGENTS.md §12）

**不适用**：Java 版没有 go/db 这样的消费进程，没有重试队列和死信；存盘由 scene 进程内的存储线程池直接执行。等价的缺口是 Java 的最终写回在 5 秒内重试 3 次后放弃、最新状态丢弃（`StoragePlayerRepository.java:459-465`），已在 Java 仓库 `PARITY.md` 第 60 行登记为「Java 待做」。本批提交后在 `PARITY.md` 追加一行「不适用（Java 没有 Kafka 存盘链）」，备注指向第 60 行。

---

## 11. 待拍板与未核实

**已拍板**（负责人 2026-10-13）：第 1 批开工；L-04 归本交接；读写超时与本批同批。

**待拍板**：

| # | 事项 | 建议 |
|---|---|---|
| 1 | 「只读」「忙」两类与「连接数满 / 断开」一起豁免（本批已按此落码）| 维持；不接受时删掉对应两行即可 |
| 2 | 读任务在库已知不可用时立即回失败回执（本批已按此落码）| 维持 |
| 3 | 告警文件与 `mysql-connection-alerts.yaml` 的分工（§3.8）| 连接数、连接池归那份；重试、延后、死信归本批 |
| 4 | 第 2 批三项各自是否立项（§4）| 死信工具立项；原地等待等 R20 数据；Redis 侧故障等落点 Redis 独立部署时再做 |

**未核实**：

- go-sql-driver v1.9.3 把断连表现成哪种错误，以及 `database/sql` 对 `ErrBadConn` 的内部重试。分类把三种都列了，不依赖这一点。
- 各 MySQL / TiDB 错误号在真库上的实际取值（1226 可用 `ALTER USER … WITH MAX_USER_CONNECTIONS` 触发）。
- TiDB 是否接受会话变量 `lock_wait_timeout`。
- 1040 → 死信这条链路本身没有实测（PROGRESS.md:7190）。
- go-redis 对已取消上下文的行为：停机时在途任务失败会打 `retry reschedule failed` / `DATA-LOSS PREVENTED`，属于现状。

---

## 12. 考虑过而不采用的方案

| 方案 | 不采用的原因 |
|---|---|
| 按错误文本匹配 | 脆弱。login 的生产者就因为文本匹配误中而自锁过（瓶颈文档 G0）|
| 在重试豁免处对任意错误跑类型判断 | Redis 侧的超时也是 `net.Error`，会被算成 MySQL 不可用（评审 F2）|
| 任意一次连接类错误即进入库级冷却 | 1040 时服务端活着、池里已有连接可用，这样会让整个实例停写、写全量堆进 Redis（评审 F1）|
| 只把预算从 3 调大 | 治不了根：重建一次 MySQL 是分钟级 |
| 所有 SQL 错误都豁免 | 表结构不符、数据超限这类错误会无限重试，永远不会被人看到 |
| 给载荷加「最早重试时刻」字段 | 要改载荷格式；每拍上限加冷却已经够用 |
| 指数退避 | 固定冷却加抖动已经把拨号压到每个冷却期一轮 |
| 自动重放死信 | 任务进死信都有原因，自动重放会把分类白做 |

**对 AGENTS.md §11.3 的偏离**（「重试必须有超时、次数上限、退避与必要的抖动」）：存储不可用类的写**不设次数上限、固定间隔**。理由见 §2：为环境问题设上限就是丢盘。代价是库长期不可用时写会一直留在 Redis 的 ready 里，靠 `DbTaskDeferredByStore`、`DbStoreCoolingDown`、`DbTaskRetryQueueGrowing` 三条告警兜住。冷却本身有抖动。

---

## 13. 修订历史

- v0.1（2026-10-13）：初稿。
- v0.2（2026-10-13）：标注已评审（实际评审结论未并入）。
- v0.3（2026-10-14）：并入对抗评审（L-04 一路 10 条，核实后成立 6 条、部分成立 4 条），按此落码。主要变化：冷却只在探活确认整库不可达时进入，1040 不冷却（F1）；分类只在 SQL 调用点做、汇总处只认标记（F2）；会话级锁等待短于读超时，并写明超时撤不回语句的残余窗口（F3）；重试间隔结论改为「每实例每拍一次」、冷却跳过的代价改为「整个不可用期间」（F4）；死信告警降为 warning 并写处置步骤、写明未知 MsgType 不处理（F5）；读任务只在库已知不可用时快速失败，首次连接错误照常重试（F6）；冷却加抖动、到期单次探活、补记对 §11.3 的偏离（F7）；补拨号超时、上线前置与「ready 是唯一副本」（F8）；加 1226、说明 1129 不纳入（F9）；毒消息计数点改到 `persistPoisonMessage`（F10）。
- v0.3.1（2026-10-14）：落码后一轮只读代码复核（未发现编译错误与 gofmt 问题），按意见改：探活改为新建连接、不经过连接池；到期探活进入 SingleFlight 后复查最新状态；`db_store_cooldown_stores` 改为抓取时现算、只数没到期的；读放弃计数挪到回执发出之后；锁等待写负数时只提示不报错；超时引起的写可能无限期豁免记为残余（§8）。

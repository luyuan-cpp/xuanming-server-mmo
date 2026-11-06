# 全层水平扩展:逻辑库自治、角色护栏、目录分片、不停服迁移、多地域

> 状态:**2026-10-31 全部写完,三路独立评审的问题已修,全部未编译,待 Codex 验证。** 验证清单在 `docs/handoff/player-save-direct-commit-20261030/progress.md` §13。
> 来源:负责人 2026-10-31「全部按最标准的修复,我不怕改动,而且我确实会有不停服迁移物理库的」「角色护栏、过户逐服务冻结、目录按哈希分集群,这个开始先做,优先写代码」。
> 前提:项目未上线,数据可清空,不写存量迁移和新旧混跑。Java 版只在 `PARITY.md` 记待做。
> 取代:[总体方案](./player-data-tidb-scaling-plan.md) §8.2、§13 阶段 C 的做法。基线快照 `refs/snapshots/before-stage-c`(tree)。

## 0. 结论

三条规则,其余都是推论:

1. **一个逻辑库只归一个服务所有。** 服务之间不写跨库 SQL,只走 RPC。
2. **每个逻辑库自带一份写者闸门。** 逻辑库是搬迁的最小单位,整库搬到别的物理集群时闸门跟着走。
3. **逻辑库到物理集群的映射有版本。** 进程按映射连库,映射翻转时不用重启。

做完之后:

- 任何服务的库都能放到独立集群,加集群、搬库都不停服。
- 目录按账号、玩家、名字哈希分片,开服建角不再压一个集群。
- 每个地域一整套,不让集群跨洋。

## 1. 逻辑库清单

| 逻辑库 | 所有者 | 分片 | 内容 |
|---|---|---|---|
| `mmorpg_global`(K=1)或 `mmorpg_global_00..K-1` | data_service | 按玩家、账号、名字哈希,见 §5 | 角色目录、路由、入场登记、过户收据、名字、账号目录 |
| `mmorpg_global_ledger` | data_service | 不分片 | `transaction_log`、`player_snapshot`、`rollback_audit_log`。写入量大,不和目录抢闸门和连接。GM 回收的 `gm_recall_*` 和护栏副本 `role_fence` 也在这里 |
| `player_store_<id>_db`、`zone_<n>_db` | go/db(写)、data_service(建角初始行、证明与判死,各自连接池) | `storage_id`。2026-11-05 定改为每地域固定 K 个(初值 64),`storage_id = 1000000 + 地域码 × 1000 + 片号`,和区脱钩、`zone_<n>_db` 退役([总体方案 §7.2](./player-data-tidb-scaling-plan.md),未落码) | `player_store` |
| `mmorpg_guild`、`mmorpg_match`、`mmorpg_trade`、`mmorpg_mail`、`mmorpg_friend`、`mmorpg_player_inbox`、`mmorpg_chat`、`mmorpg_rank` | 同名服务 | 不分片,以后按业务键拆 | 业务表、`role_fence`、闸门表 |
| R的Mall逻辑库（本机计划显式命名） | `java/mall_game` | 全区共用，不按zone拆 | 商城订单、支付/退款/寄售原件与本库写者闸门；只经RPC/HTTP访问其他服务 |
| R的Gateway逻辑库（缺省`mmorpg`，隔离联验可显式命名） | `java/gateway_node` | 全区共用，不按zone拆 | 区目录、公告、白名单与本库写者闸门 |

### R双区的四个Java进程（2026-11-02补齐，待Codex实测）

四角色为`mall`、`mall-replica`、`gateway`、`gateway-replica`：两个Mall写同一个商城逻辑库，两个Gateway写同一个网关逻辑库。每个进程各有独立NORMAL账号、writer/instance/deployment身份、单次grant与消费目录；不是每个zone新建一套商城。Gateway的HTTP入口仍通过gRPC调用R的Go login，公开实例计划必须按两个zone分别固定本轮login地址。独立Java仓的Dubbo网关不参与此运行栈。

本地`sql_boot_catalog_local.ps1`在一次调用中合并两区Go计划和上述四角色，先检查键、账号名及全部端口，再迁移和注册，最后只发布一份目录。每个Java服务的两个副本必须指定同一个逻辑库，Mall与Gateway两库不同；不允许接管Go或玩家库。Mall无Go业务迁移入口，其闸门由独立Schema进程执行`writer_authority -command prepare-schema -schema <库>`，仅调用共享`authoritysql.EnsureCatalogSchema`，不复制第三份闸门DDL，不给NORMAL主体DDL能力。

四Java联验要求显式绑定独立物理authority与真实I3原件及SHA，不把普通local声明当成I3证明。公开计划固定R的运行包和Java程序SHA、本机监听端口及Go login/协调地址，不含凭据；每boot完整私有配置由正规producer写出，启动仍经`Get-SqlWriterBootEnvironment`。已存在的账号、旧boot与UNKNOWN原件在此模式保留，不按进程消失删除。启动端口就绪只是启动观察，不是双区业务或故障恢复通过。

`zone_merged_into` 留在目录第 0 片,只有 data_service 和 `tools/merge_zone` 读写,其余服务经 `GetMergedZones`。

**机械约束**:新增契约测试 `go/shared/crossdb_contract_test.go`(或同等位置),扫描非测试 Go 源码,出现 `` `"+<schema 变量>+"`. `` 形式的跨库 SQL 就失败。白名单只有数据迁移工具。

## 2. 写者闸门:每个逻辑库一份

> 开销(2026-11-05 按代码数过):一笔事务约 20 次非业务往返,连接复用时的会话重设占一半,收敛方向与顺序见[存盘规格 §9 第 1 条](./player-save-direct-commit.md)。本节的语义(锁 slot 先于读 meta、冻结拿全部 slot 锁)是收敛时不能动的部分。

现在的闸门把 `writer_slot`、`route_meta`、`writer_registry`、`writer_lifecycle_receipt` 放在全局库,每笔事务都要锁全局库一行,所以所有服务绑死在一个集群上。

改成:

- `AuthoritySchema` 等于这个逻辑库自己。`CatalogBootstrapStatements` 在每个逻辑库建这几张表:各服务的 `-migrate` 调 `authoritysql.EnsureCatalogSchema`(可重复执行,已有的行不动);data_service 走自己的 `bootstrapTables`;go/db 由 `cmd/migrate` 建;`mmorpg` 没有 Go 迁移入口,写在 `deploy/mysql-init/gateway_tables.sql`,`TestGatewayTablesCarryWriterGate` 钉住 DDL 相同。
- 一个进程可以持有多份 `WriterConfig`:`LoadWriterConfigs(getenv) (map[逻辑库]WriterConfig, error)`。只写一个库的进程用 `LoadWriterConfig`,多给或少给一份都拒绝。写者启动目录(`sql_boot_catalog_local.ps1`、K8s `<服务>-sql-boot`、`cmd/writer_authority`)按进程要写的每个逻辑库各注册一份。
- `authoritysql.Open(...)` 传对应逻辑库的 `Writer(config)`。建连时核 `DATABASE() == AuthoritySchema`:slot 和业务行必须在同一个库,才在同一个事务里。
- `ProtectedSchemas` 只有这个逻辑库自己。`NewCatalog`、`LoadWriterConfigs`、`writer_authority`、排空计划校验都拒绝别的形状。
- 维护排空(`maintenance_drain.ps1`、`go/db/internal/maintenancephase`)逐逻辑库做:一个 run 只管一个库。进程持多份写资格时,run 绑 Plan 指的那一份;进程退出后,其余库各按非 run 的 RETIRE 退休(`Catalog.Retire` 在那些库没有 run 时允许)。
- 闸门被冻结时返回可区分的错误 `authoritysql.ErrWriterFrozen`(包住 `ErrWriterDenied`),RPC 层映射成可重试的 `Unavailable`。资格不符仍是 `ErrWriterDenied`。

**启动形状**:`MMORPG_SQL_WRITER_GRANT_FILE` 指向的文件是一个 boot 的写资格集合(`sql_writer_boot_grant_set`,每个逻辑库一份),环境变量和挂载路径不变。名册的一条 entry 是一个 boot:`requests` 里每个逻辑库一份 Request,writer 身份和账号相同,任一份被别的 boot 消费过,整条就算用掉。`writer_authority` 一次注册写完全部份。注册、维护账号的连接串按 Request / Plan 的逻辑库改库名(`DSNForSchema`),同集群的库共用一条;库放到别的集群时地址按映射给(环境变量 `MMORPG_DB_PLACEMENT`,§8.1),连接串里的账号在目标集群提前建好。物理身份摘要 `MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256` 按部署一份,不按集群,搬库不换。

**冻结映射**:

- 连接层在闸门上看到 `writable=0` 或 epoch 变了就回 `ErrWriterFrozen`,并置上请求 ctx 的观察位(`ObserveWriterFrozen`)。主体、世代不符或写者已退休仍回 `ErrWriterDenied`。
- `serverbase.UnaryInterceptor`(match 用 `WriterFrozenInterceptor`)看观察位:handler 报告失败时(返回错误或带内错误码非 0)回 `Unavailable`。handler 常把存储错误翻成带内错误码,哨兵到不了拦截器,所以不靠错误链。handler 吞掉冻结错误、照常回成功时不改。
- go/db 存盘冻结时回 `Unavailable`,不记未知结局。scene 的 `PlayerStoreClient` 对 `Unavailable` 本来就按原序号退避重试,不用改。
- **读写都暂停**。写者池上的读也开自动事务、也过闸门,窗口里回 `ErrWriterFrozen`。不改这个语义:窗口是秒级,回可重试错误最简单,放开读就要回答读到的是不是冻结点之后的数据。不经闸门的只读连接池(`Reader` 权限)照常读,读到冻结时的数据。
- **结局分类**:`authoritysql.IsWriterFrozen(err)` 判「冻结,这次确定没写」,要求错误树的每个叶子都是冻结,混进别的失败(尤其 `ErrOutcomeUnknown`)就不算。`ClassifyOutcome(err)` 分成 `OutcomeOK`、`OutcomeFrozen`、`OutcomeUnknown`、`OutcomeFailed`,结局未知优先。后台循环对冻结不打排空 HOLD:trade 恢复循环和清理、`shared/assetop` 重投、player_inbox 清理和提醒、rank 发奖、mail、friend、guild 的清理和历练巡检、match 的 relay 工作。match 结果消费者撞冻结不占重试次数,否则窗口稍长消费者就停了。RPC 路径的判定在 serverbase。
- **trade 恢复循环的判定更宽**:`commerce.OutcomeKnown(err)` 在冻结之外还放过事务回滚之后的拒绝(`errRolePreparing`、`errRoleConflict`、`Rejection`、`rolefence.ErrUnavailable`、`ErrRoleNotFound`)和只读步骤的失败(拉商城待办、读合同、读目录)。这些在产生错误的地方标记(`rolledBack`、`noWrite`),判定走错误树,每个分支都要确定才算。没标的都算未知:回报商城失败、提交失败。只读步骤的语句被连接层报成 `ErrOutcomeUnknown` 也标成确定:连接层不分读写,这条语句本来没写。合同推进的单行失败照旧由 `deferCommerce` 推迟,不回给调用方,所以单行提交结局未知在这里也不 Hold:连接层把事务里被取消的语句也报成结局未知,按它 Hold 的话,行锁等待超过子预算就会打 Hold。
- **观测**:`writer_frozen_seconds{logical_db}`,这个进程观察到闸门连续冻结了多久,解冻后第一笔过闸归零,15 秒没再撞到也归零(没有写流量的进程不受影响,也不上报)。第一次撞到才注册序列。告警 `WriterGateFrozenTooLong`:超过 2 分钟,多半是工具被杀或翻转结局未知,处置见手册 §6。

**写资格什么时候消费**:写资格是一次性的,因为进程写过之后崩了,重启时内存里的 boot 状态没了,排空证明也接不上。进程还没写过就退出没有东西丢,所以 `LoadWriterConfigs` 只核消费标记不存在,**第一条写语句之前才写标记**(`grantMarker`,纯读不写)。启动时依赖没就绪(合服映射、Redis、Kafka)、读到一半退出,同一个 Pod 重启接着用这份,不烧名册。Go 服务都是这样;Java 的 `WriterAuthority` 还是读的时候就消费,比这严。

写过之后崩溃的进程,容器重启读到标记以退出码 78 拒启,只有删 Pod 换名册下一条。这件事由 `sql-writer-pod-reaper` 自动做(`deploy/k8s/README.md`)。

**启动路径扛冻结**:启动撞上窗口也不能退出,不然 Pod 反复重启,窗口过去才恢复:

- `authoritysql.Open` 自己的核对(`SELECT DATABASE()`、强读会话、闸门表形状)不过闸门。这几条不碰业务数据。
- 这个 boot 在这个库上还没有事务过过闸门时,写者池上不带事务的读语句放行(`guardedConn.readPassesFrozenGateLocked`)。各服务开池之后的只读核对(表形状、片号、`rolefence.VerifySchema`、go/db 的 schema 闸)都走这条,不用逐个服务改。第一笔事务过闸之后回到常规语义。放行的只有读,拿到的是冻结时的数据,和只读连接池一样;显式事务和写任何时候都不放。
- 启动期的写改成先读:chat `ensurePolicy`、match `EnsureGlobalPolicy`。策略行写过一次就不再变,重启只读。只有第一次部署(行还不存在)撞上窗口才失败,而第一次部署不可能在搬这个库。
- data_service、go/db 按需开池撞上冻结不进 10 秒冷却,错误照样包着 `ErrWriterFrozen`。

**REGISTER 不绑 epoch**:名册是预先渲染的,每次 cutover 或回滚 epoch 加 2,绑了整份名册就废了,Pod 永久起不来,多库 entry 还留下半套注册。所以 REGISTER 的 `expected_epoch` 必须是 0,只核 generation、`writable`、`freeze_run_id`,收据记落地时的实际 epoch,新写者的 slot 按当前 epoch 建。RETIRE 继续绑,它要和同 epoch 的维护 run 对账。

安全性论证:

- **闸门靠 slot,不靠请求**:写者事务持自己的 slot 行锁,核 slot epoch 等于 meta epoch、`writable=1`。新写者的 slot 在持 meta 行锁的事务里按读到的 epoch 建,和现有 ACTIVE slot 同 epoch,所以它和别的写者走同一道判定。请求里的 epoch 从来不参与这个判定。
- **和冻结串行**:`changeGate` 锁全部 slot → meta,再读 ACTIVE 全集,和预读不一致就拒绝,工具在预算内重试。REGISTER 持 meta 行锁插入。所以 REGISTER 要么排在冻结前,它的 slot 被冻结一起推进,写者全集不漏;要么排在冻结后,读到 `writable=0` 回 `ErrWriterFrozen`,什么都没写。不存在 slot 留在旧 epoch 又被放行的状态。
- **和排空串行**:run 开着时 `freeze_run_id` 非空,只有同 run、`SQL_RELEASED` 的请求能注册,这一支继续核 release 收据的 `AfterEpoch`。排空的证明不靠请求里的 epoch。
- **放宽的是什么**:排空彻底结束之后,排空之前渲染的、还没用过的名册条目还能注册。原来 epoch 挡着,现在靠部署:旧 Deployment 的名册随它下线。要强制作废一批名册就换 generation。

**多库 entry 的结局**(一个 boot 每个逻辑库一份 Request,不在一个事务里):

| 情形 | 结局 |
|---|---|
| 注册第一份之前的预检 | 逐库核 `RegistrationReady`。有一个库不行就一份都不写,不留尝试记录,这条 entry 不算用掉 |
| 提交时撞上冻结 | 事务已回滚,确定没写。撤掉这一份的尝试记录,否则重试按「结局未定」处理。回可重试 |
| 部分库已注册,进程还活着 | init 重启,deployment-id 不变,不算别的 boot 消费。已注册的按原请求查收据,没注册的接着注册 |
| 部分库已注册,Pod 没了 | 新 Pod 看到这条 entry 被别的 boot 消费,换下一条。已注册的几份是没有进程的写者,和注册完就崩的 Pod 一样,按 §2 的非 run RETIRE 退休 |
| 结局未知 | 留尝试记录,不重发,只查原请求,和原来一样 |

**`route_meta` 的两层意思**:这张表的 `writable` 同时是闸门开关和路由写者开关,是同一列,不是两个开关。

- 闸门:`writer_slot` + `route_meta.writer_epoch` + `writable`,每个逻辑库一份,事务进入时核。`Catalog.Freeze` / `FreezeWriters` 改它。
- `requireRouteWritable`(data_service 的 `RegisterHome`、建角初始提交)只是把同一列翻译成 `ErrRouteNotWritable`:经闸门的池在 BEGIN 就回 `ErrWriterFrozen` 了,走不到这条读;它只在不经闸门的池(测试)上起作用。server 层两种都回 `Unavailable`。
- `route_tool freeze` / `unfreeze` 是 `Catalog.Freeze` / `Release`,走带票据的排空流程。目录库的路由写者就是目录库的全部写者,所以冻结路由等于冻结目录库的闸门,没有更细的开关。
- 别的逻辑库的 `route_meta` 只有闸门的意思,表名不改(`RouteMeta` proto 和 Java 都按这个名字)。

**搬库用的冻结接口**(批 M1):`Catalog.FreezeWriters(ctx, runID)`、`Catalog.ReleaseWriters(ctx, runID)`。和排空的 `Catalog.Freeze` 不同,这里写者不退休,身份和 boot 授权都不变,解冻之后接着写。锁序同排空:全部 ACTIVE 写者的全部 slot → meta。同 `runID` 重放幂等,排空 run 开着时拒绝。

单集群部署行为不变,只是闸门表换了位置。

## 3. 角色护栏

### 3.1 要保留的语义

今天靠所有服务同事务锁 `account_role`(P 锁)得到两条保证,必须原样保留:

- **互斥**:过户、删角和服务的资产写入互斥。「票先赢则冻结拒绝,冻结先赢则新参与者拒绝」。
- **原身份**:会话的 `(account, owner_revision)` 必须等于当前归属,修订号只增不减,旧主不能写。

### 3.2 版本化的护栏状态

目录是唯一权威。`account_role` 加两列:

- `fence_version BIGINT UNSIGNED NOT NULL DEFAULT 1`:每次改 `account`、`owner_revision`、`trade_listing_id`、`prepare_token` 的事务都加 1。
- `prepare_token BIGINT UNSIGNED NOT NULL DEFAULT 0`:**准备令牌**,全部参与者投票期间写。过户是 listing id,删角是 `RemovalToken(player) = 1<<63 | player_id`(发号器只发 63 位,不会撞)。

`trade_listing_id` 是**最终冻结令牌**,全部参与者通过之后才写,值是 listing id。两列不会同时非零,墓碑不带令牌,读到别的组合按原件损坏拒绝。删角不写最终令牌,全通过之后直接进墓碑事务。

护栏状态 `RoleFenceState{player_id, account, owner_revision, freeze_token, freeze_final, version, home_zone}`。`freeze_token` 在 `trade_listing_id` 非 0 时取它,否则取 `prepare_token`;`freeze_final = trade_listing_id != 0`。

投票可能被撤销,所以两种令牌的读者不一样:

| 判定 | 看什么 | 理由 |
|---|---|---|
| `RegisterRoleAdmission`、`Lookup`、EnterGame、scene_manager | 只看 `trade_listing_id`,`Lookup` 不回准备令牌 | 投票期间玩家照常进场,论证见 §3.5 |
| 建角重放、`Repair`、`Remove`、新的 `Begin`、还没有收据的 `Cancel` | 两个都看,有令牌就拒绝 | 投票期间不能改身份,也不能开第二轮 |
| 参与者的业务写入 | `freeze_token != 0` 就拒绝,不分是否 final | 投票期间也要拦,否则 Prepare 读完挂起之后还能新增 |
| chat | 只看 `freeze_final` | 不写资产,不是参与者,不替投票拦写入 |
| 不可逆的判定(退款) | 只认 final,`freeze_token != 0 && !freeze_final` 回可重试 | 撤销了角色就回来了,不能按冻结下结论,清单见 §3.5 |

每个参与者的逻辑库有一张副本表:

```sql
CREATE TABLE role_fence (
  player_id      BIGINT UNSIGNED NOT NULL,
  account        VARBINARY(191)  NOT NULL,  -- '' = 已删除
  owner_revision BIGINT UNSIGNED NOT NULL,
  freeze_token   BIGINT UNSIGNED NOT NULL,  -- 0 = 未冻结
  freeze_final   TINYINT(1)      NOT NULL,  -- 1 = 最终冻结,0 且令牌非 0 = 还在投票
  version        BIGINT UNSIGNED NOT NULL,
  home_zone      INT UNSIGNED    NOT NULL,  -- 原始区,不解析
  updated_ms     BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (player_id) /*T![clustered_index] NONCLUSTERED */
) ENGINE=InnoDB /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */
```

**唯一的合并规则:版本大的赢。** 所有写入(冻结、解冻、过户推送、自愈)都是「`version` 更大才覆盖」。这样推送乱序、重复、丢失都没有影响,不需要 ABA 判断。

### 3.3 `shared/rolefence`

```go
type Fence struct { PlayerID uint64; Account string; OwnerRevision, FreezeToken uint64; FreezeFinal bool; Version uint64; HomeZone uint32 }

var ErrMissing = errors.New(...)                  // 副本行不存在
func Lock(ctx, tx, player uint64) (Fence, error)  // SELECT ... FOR UPDATE,取代所有 account_role FOR UPDATE
func Read(ctx, db, player uint64) (Fence, error)  // 不加锁,只给事务外的预检用,提交点仍然要 Lock
func Apply(ctx, execer, state *pb.RoleFenceState) (applied bool, err error) // 版本大才写
type Expect struct { PlayerID uint64; Account string; OwnerRevision, AllowToken uint64; Reject error }
func Check(fence Fence, lockErr error, expect Expect) error // nil 放行;*StaleError 该自愈;其余是拒绝或 SQL 错误
func LockAll(ctx, tx, expects ...Expect) (map[uint64]Fence, error) // 按 player_id 升序 Lock + Check,要自愈的一次收齐
type StateReader interface { GetRoleFenceState(ctx, player) (*pb.RoleFenceState, error) } // data_service RPC
func NewDirectoryReader(rpc, caller, secret string) (StateReader, error) // 签名见 §3.6
type AdmissionVerifier interface { VerifyRoleAdmission(ctx, admission) (bool, error) } // §3.7
func NewAdmissionVerifier(rpc, caller, secret string) (AdmissionVerifier, error) // 调用方和密钥同 NewDirectoryReader
type Guard struct { ... }                         // 持 *sql.DB、StateReader
func NewGuard(db, reader) *Guard
func (g *Guard) Heal(ctx, players ...uint64) error // 读权威状态并 Apply
func (g *Guard) RunTx(ctx, action func() error) error
func RemovalToken(player uint64) uint64
const DDL; func EnsureSchema(ctx, db) error; func VerifySchema(ctx, db) error // 迁移入口建表并校验;启动只读校验
```

- `RunTx`:`action` 返回 `*StaleError{Players}`(副本缺行、冻结、或会话修订号大于副本修订号)时,事务已回滚,调 `Heal` 后重试一次。第二次还不过就是真实拒绝,回各服务原来的拒绝错误(`Expect.Reject`)。
- **事务里不调 RPC。** 所有 RPC 都在 `RunTx` 的重试间隙。
- 一个事务锁多名角色时用 `LockAll`:要自愈的角色一次收齐,否则第二名角色的自愈会被 `RunTx` 当成真实拒绝。`Expect.OwnerRevision` 为 0 表示没有原身份(对端、目标),只判冻结和删除。`Expect.AllowToken` 是这笔业务自己持有的令牌(trade 的人民币寄售)。
- 目录说角色不存在(`found=false`)时不写副本,原样交给调用方:`Heal` 回 `*RoleNotFoundError{Players}`(`errors.Is(err, ErrRoleNotFound)`),其余角色照常写好。好友的对端不存在按原逻辑跳过,其余拒绝。
- **自愈读不到目录不是故障**。`RunTx` 回 `ErrUnavailable` 时事务已回滚,这一笔确定没写。回故障 tip 或 `Unavailable` 的话排空证明被打成 Hold(§3.6),所以各服务映射成非故障的「稍后重试」:guild `ErrWriteConflict`,player_inbox `kPlayerInboxStaleOwner`,trade `kTradeOrderStateConflict`(`ErrRoleNotFound` 回 `kTradeCounterpartyUnknown`),mail 和 friend `kFeatureUnavailable`,match 发起切磋 `kMatchChallengeExpired`。其余调用方没映射、照旧报故障的,`RunTx` 置的观察位由拦截器兜底,改回可重试的 `Unavailable`,不 Hold:match 有 51 处 `Fenced`,逐处映射兜不全。这种情况客户端经路由服看到的是 `kServiceUnavailable`。
- 过户之后旧主的会话:副本修订号大于会话修订号,直接拒绝,不自愈。新主的会话:修订号小于会话的,自愈后通过。

**服务里的替换规则**:每处 `SELECT account,owner_revision,trade_listing_id FROM <schema>.account_role WHERE player_id=? FOR UPDATE` 改成 `rolefence.Lock`,判据不变。`LockRoleRoutes` 删除:路由只有 `home_zone`、`storage_id`,前者不变、后者不再由这些服务关心;`frozen_run_id` 删除(§7 说明)。要读 `home_zone` 的(组队)读 `Fence.HomeZone`。事务外的读(JoinQueue、组队查询)走 `GetRoleFenceState`,多一跳 data_service。

### 3.4 参与者接口

新 proto `proto/role_fence/role_fence.proto`,每个参与者的 gRPC 服务器注册同一个 service:

```proto
service RoleFenceParticipant {
  rpc PrepareRoleFreeze(PrepareRoleFreezeRequest) returns (PrepareRoleFreezeResponse);
  rpc ApplyRoleFence(ApplyRoleFenceRequest) returns (ApplyRoleFenceResponse);
}
message PrepareRoleFreezeRequest  { RoleFenceState state = 1; common.base.ServiceCallAuth auth = 2; bool dry_run = 3; }
message PrepareRoleFreezeResponse { bool clean = 1; string blocker = 2; bool stale = 3; bool retry = 4; }
message ApplyRoleFenceRequest     { RoleFenceState state = 1; common.base.ServiceCallAuth auth = 2; }
message ApplyRoleFenceResponse    { bool retry = 1; }
```

- 鉴权:`ServiceCallAuth`,协调者密钥 `MMORPG_ROLE_FENCE_SECRET_COORDINATOR`,caller 是 `coordinator`。方法名由 `rolefence.Methods()` 给出。trade、mail、friend、player_inbox 的方法清单都是客户端白名单,不加这两个方法:协调者的调用不带会话,按内部调用放行,带会话的调用被白名单挡住。proto 包名是 `rolefence`,方法路径 `/rolefence.RoleFenceParticipant/...`。
- 这个 service 没有自己的节点类型(`ENodeType` 里没有对应值):六个参与者各自注册,协调者按 `RoleFence.Participants` 拨号。所以 protogen 不把它收进客户端路由表,也不收进 C++ 按节点类型的方法注册和 deadline 分发(`RPCServiceInfo.HasNodeType`)。客户端协议对不上节点类型时生成直接失败。
- `shared/rolefence.NewParticipantServer(db, pending, secret)` 提供实现,服务只注入 `pending func(ctx, tx, state) (blocker string, err error)` 和协调者密钥。friend 注入 `rolefence.NoPending`,没注入算装配失败,不算没有挂起。
- `PrepareRoleFreeze` 在一个事务里:`Apply`;副本版本更大回 `stale=true`;再 `FOR UPDATE` 读本库的挂起全集。有挂起就**回滚**(本库不冻结)回 `clean=false`。没有就提交。这和今天 `verify` 的判据逐条相同,只是拆到各服务。
- `dry_run=true` 不 `Apply`,不锁副本,只跑 `pending`,事务回滚。结论只用来挡住注定被拒的 Begin,提交点仍是真正的 Prepare。带最终令牌的状态不接受:最终令牌是全通过之后才写的。
- `blocker` 是低基数枚举字符串,进指标 label,不含玩家信息。
- 查询失败或未知状态一律不通过,和今天相同。确定性的结论用 `blocker` 回,不能回失败,否则协调者按不可达处理、令牌留着等重放,而重放还是同样的结果:trade 名下未终局合同超过 256 条回 `too_many_contracts`。
- **内部失败带内回**。SQL 失败、超时、取消回 `retry=true`,协调者按不可达处理。两个方法幂等、有版本保护,没有结局未知,而回 `Unavailable` 的话 `PublicationAdmission` 按结局未知算,要 Hold(§3.6)。gRPC 错误只留给鉴权失败和非法参数。

各服务的挂起全集(从 `account_role_asset_guard.go`、`account_role_guild_guard.go` 原样搬):

| 参与者 | 挂起条件 |
|---|---|
| guild | 只对过户:`guild_player_state`、`guild_member` 还在帮(`requireGuildExit`,旧的删角就不要求离帮)。过户和删角都判:`guild_asset_op` 未终态;`guild_trial_reward_owed`;`guild_trial_launch_role`;`guild_trial_lobby_slot` |
| trade | `trade_commerce` 未终态(含 `official_refund` 和本 listing 的 `rmb_freezing` 例外);`trade_asset_op` 未终态 |
| mail | `mail_asset_op` 未终态 |
| match | `match_authority_ticket` 占用;`team_membership`;`team_invite_index`、`team_application_index` 未过期;`team_launch_role`;`match_spectator`;`match_challenge_invitation`;`match_role_occupancy` 未到期(§4) |
| player_inbox | `player_inbox_item` 未终态 |
| data_service(流水库,进程内) | `gm_recall_op` 的 `PLANNED`、`PENDING`。不在 `rolefence.Participants` 里:那张表是要配地址的远端服务,这个参与者由 `Coordinator.AddLocalParticipant` 接上,接不上就不装配协调者(2026-11-01,[回收方案](./exploit_loss_prevention_zh.md)) |
| friend | 无,只冻结 |

chat 不是参与者:它不写资产,沿用 TTL 校验,校验改成调 `GetRoleFenceState`,只拒绝最终冻结。配置 `DataServiceRpc` 必填,运行角色没有全局库权限。

两处判据随拆库变了,不是原样搬:

- **player_inbox 的入场登记**:`FetchItems`、`AckItems` 在事务外调 `VerifyRoleAdmission`,事务内只比修订号。不再和 `CloseRoleAdmissions` 串行,校验之后世代可能刚好关闭。不影响资产:归属变更先要冻结副本,事务在副本行锁上被拒;scene 关闭世代前已结束自己的实体和回调,迟到的 Fetch 只多记一次投递,迟到的 Ack 报的是它已落盘的结局。这两个方法要写库,所以校验放在事务之前,读路径可以放在读之后。身份核不过(冻结、归属或登记变了)和目录读不到都回非故障 tip `kPlayerInboxStaleOwner`,`fence` 留空,scene 走退避重试。这两种情况要么没开事务,要么事务已回滚,没有结局未知,回故障 tip 会把排空证明打成 Hold。真正的存储故障还是 `kPlayerInboxStoreUnavailable`。没有加新 tip:scene 对非 0 tip 都是退避重试,不看具体的码,而加码要跑导表器、动客户端仓。
- **trade 的官方退款**:`official_refund` 不可逆,副本冻结或修订落后时先自愈,第二遍才按它判。副本可能是旧的,不能凭旧副本退款;自愈之后还是准备令牌也不判,回可重试,见 §3.5。

**player_inbox 的提醒簿记不过护栏**:`ClearNotifyDue`、`DeferNotifyDue` 是一条按 `next_seq` 比较的 `UPDATE`,不锁副本。`notify_due_ms` 只决定补发循环什么时候再提醒一次,不碰条目和资产,条目的消费都在持护栏的事务里。过护栏的话,条目终结之后角色被冻结(挂寄售),这一行就永远清不掉:每轮补发回 `ErrOriginalRole` 打成 Hold,还占着扫描名额。一条语句只锁流行,和入箱的锁序(副本 → 流行)成不了环。写失败(非冻结)照旧 Hold,提醒本身发 Kafka 失败也照旧。

**player_inbox 的运维写入**:GM 处罚(`GmSanctionPlayer`)、作废(`VoidItem`)撞上角色冻结或目录读不到,和消费面一样回非故障的 `kPlayerInboxStaleOwner`,同一 `request_id` 重试幂等。

**mail 读帮会**:帮会文本邮件的可见性原来跨库读 `mmorpg_guild.guild_member`,这里漏列了。改成事务外调 guild 的 `GetPlayerGuild`(配置 `GuildRpc`,单次 300ms)。读失败按没有帮算:这类邮件不带资产,只是这一次看不到,不能让帮会服务抖动拖垮邮箱。失败记错误日志和 `mail_guild_lookup_failures_total`。

### 3.5 协调者(data_service)

新增状态 `ACCOUNT_ROLE_TRADE_STATE_PREPARING`,排在现有枚举之后。目录事务不再跨库读,也不再锁账号 guard(§5 之后 guard 在别的分片)。

**Begin**:

0. 试探,新建时才做,无事务:读不到收据、读到角色归属相符且没有令牌,才并行调所有参与者 `PrepareRoleFreeze(dry_run=true)`,上限 0.8 秒。有挂起回 `ErrConflict`,问不齐回 `ErrRetryLater`,目录一个字都不写。注定被拒的 Begin 写了令牌就要让全部参与者拦这名玩家的业务写入,而 trade 的对账循环会一直重放它,玩家每 10 秒被打断一次。
1. 目录事务(玩家所在分片):插入或读收据。已有且摘要相同:`PREPARING` 走第 2 步,其余按今天重放。新建:P 锁,核归属、两个令牌都空,写 `prepare_token=L`、`fence_version+1`、收据 `PREPARING`。`trade_listing_id` 不动。
2. 无事务:并行调所有参与者 `PrepareRoleFreeze(state)`,有限时。
3. 结果:
   - **有不通过**:目录事务清令牌、`fence_version+1`、删收据(与今天被拒的 Begin 一样不留痕),尽力推 `ApplyRoleFence`,回 `ErrConflict`。
   - **有不可达或 stale**:不改状态,回 `ErrRetryLater`。令牌留着,调用方用同一 listing 重试;trade 的对账循环本来就重放 Begin。第 1 步已提交,结局已知,不回 `ErrUnavailable`:那是结局未知,会打成 Hold(§3.6)。收据超过 `rolePrepareDeadline`(30 秒)还是这样就撤销,和「有不通过」一样清令牌、删收据、推送,回 `ErrRetryLater`:原因是问不齐,下一次从试探重来,不再写令牌。
   - **全通过**:目录事务写 `trade_listing_id=L`、`prepare_token=0`、`fence_version+1`、收据 `FREEZING`,按 P 锁 `snapshotRoleTradeHolders`,提交后推 `ApplyRoleFence`,再 `fenceDeadTradeHolders`,回收据。holder 全集不合规(登记属于旧修订、超上限)要到这一步才判,回 `ErrConflict` 时按「有不通过」撤销,否则令牌清不掉。

第 3 步的两个目录事务都核版本等于第 1 步的版本:版本变了说明又开始了一轮,这一轮的结论不算数,事务回滚,回 `ErrRetryLater`。离开 `PREPARING` 的(别的重放做完了,或取消赢了)只重放收据。

**没人重放的 `PREPARING`**:调用方挂了或合同走了别的路,令牌会一直拦业务写入。沿用删角收尾的后台循环(每分钟一轮,每轮 64 行,走 RELAY 门禁,排空前停),`AbortStalePreparing` 撤销超过 `rolePrepareDeadline` 的收据。收据表只增不减,`account_role_trade` 加索引 `(state, created_ms)`。撤销总是安全的:只放开限制,不需要协调者;版本加 1,在途的 Prepare 作废;多副本同时跑靠收据行锁串行。

**trade 的处理**:`rmb_freezing` 收到 `ErrConflict` 标成 `role_conflict`,复用 `tickRoleConflictBackoff`(5 分钟)。合同的 `ExpiresMs`(创建后 15 分钟)到期就走取消,这时还没进商城,不牵涉任何人的钱;取消回 `ErrConflict` 同样分钟级复查。`ErrRetryLater`、`ErrUnavailable` 还是 10 秒。

**Cancel**:`PREPARING` 与 `FREEZING` 同路径,区别只是令牌在哪一列,终态提交后推 `ApplyRoleFence`。判死之前先普通读 holder,没有未排空的就跳过判死,所以 Cancel 不依赖存档库:`PREPARING` 没有 holder,一定不碰存档库。这一读过期了也没事,漏判一次只是持有者接着等 ACK。**Confirm**:不变,证明读见 §6;还在 `PREPARING` 回 `ErrRetryLater`。**Transfer**:见 §5.4,提交后推 `ApplyRoleFence`。

**锁序**:Begin、Cancel、Confirm 都是 listing 行 → 角色行 → 入场登记 → holder。Cancel 的墓碑候选先插入 listing 行,和 Begin 走同一个顺序;先锁角色行再插入的话,不再有账号 guard 串行,两边会成环。后面任何拒绝都回滚,不留墓碑。

**Remove**(`RemoveAccountRoles`):同样两阶段,准备令牌是 `RemovalToken`。第 1 步写 `prepare_token` 和小表 `account_role_removal(player_id, account, owner_revision, started_ms)`;全通过后才进墓碑事务:持角色行锁再核一次登记全部关闭,墓碑、清令牌、删小表、推送。投票期间有实例进场就放弃,和有不通过一样清令牌,回 `ErrConflict`。问不齐回 `ErrRetryLater`,令牌留着。data_service 后台把超过 5 分钟的 `account_role_removal` 行按同一流程继续或放弃(每分钟一轮,每轮 64 行,走 RELAY 门禁,排空前停)。多名角色逐个走,不再是一个事务:中途失败时前面的已经删了,原请求重放幂等。全程只在玩家分片上开事务,不碰账号 guard;墓碑提交之后删账号分片的 ref,失败无害,见 §5.2。删角不试探:没有重放循环,不会反复打断玩家。

**装配**:协调者在 `go/data_service/internal/rolefencecoord`,配置 `RoleFence.Participants`(`{Name, Endpoints 或 Etcd}`),必须逐个对上 `rolefence.Participants` 这六个。缺配置或缺密钥时不装配,`Begin`、`Transfer`、`Remove` 回 `ErrRetryLater`,不能跳过参与者;`Cancel` 不拒绝,它只放开限制,推送丢了也会自愈。Prepare 一轮上限 1.5 秒,试探 0.8 秒。Push 异步发,不等结果,上限 2 秒。`home_zone` 取自 `account_role.role_proto`,建角时已核等于路由的 `home_zone`。

**解冻丢推送不要紧**:副本还是冻结态,下一次访问 `RunTx` 自愈。**冻结丢推送也不要紧**:Prepare 没成功,Begin 就过不去。**转最终令牌的推送丢了也不要紧**:令牌不变,业务写入照样拒绝;认 final 的判定都先自愈。

**安全性论证**(评审时逐条核):

- 过户提交前,所有参与者的副本都已是冻结态,且冻结事务读到了之前提交的全部挂起。冻结之后的业务事务在 `Lock` 上被拒。这和同事务 P 锁等价,串行点从一行目录行变成每个服务各一行副本。
- 副本只会被版本更大的状态解冻。版本更大的解冻状态只有目录在终态事务或撤销事务里才产生。
- 延迟到达的旧 Prepare 版本小,`Apply` 不生效,回 `stale`。撤销之后才到、副本还没收到解冻的旧 Prepare 会把副本冻住,结论不算数(版本已变),下一次访问自愈。
- **准备期间放行入场不丢 holder**。`RegisterRoleAdmission` 持 P 锁看 `trade_listing_id`。转 `FREEZING` 的事务持同一把 P 锁,在写最终令牌的同一个事务里快照全部登记。所以每个 Register 要么排在它前面,进快照;要么排在它后面,读到最终令牌被拒。和「令牌写下之后才快照」相比,holder 全集一样完整,只是挡入场的时刻从投票开始挪到投票结束。投票期间进场的实例对参与者的业务写入同样被副本拦住,投票撤销之后才放开。
- **准备期间放行入场不会删掉在线角色**。删角没有 `FREEZING`,墓碑事务持同一把 P 锁核登记全部关闭,投票期间进场的实例让这笔删角放弃。墓碑之后修订号变了,旧世代登记不进来。
- `Lookup` 不回准备令牌只影响入场判定。改归属的操作都在自己的事务里锁角色行,看得到两个令牌。

**把冻结当最终结论的判定**(全仓核过,改这里要同步):

| 位置 | 结论 | 准备令牌时 |
|---|---|---|
| trade `Mall.accept` | 持久退款 | 回 `errRolePreparing`,不留合同,商城的待办下一轮再判。换主是最终结论,不等投票 |
| trade `RmbConsignments.deliver` | 持久退款 | 预检用 `Lookup`,只回最终令牌,走不到退款;事务里 `lockRoles` 拒绝,下一轮再判 |

核过不算的:trade `acceptBid`、`counterpartyDefect` 的退款和移出盘口只看换主,不看令牌;match `CommitBattleOutcome` 把 `ErrOriginalPlayer` 记成永久冲突,但 battle 只认回包里的决定,一直重放,不丢结果;match `repairCandidateRegions` 拒绝时什么都不做;player_inbox `EnqueueBattleSettlement` 冻结回 `Unavailable`;guild、mail、friend 的拒绝都不改状态。

### 3.6 目录 RPC

```proto
rpc GetRoleFenceState(GetRoleFenceStateRequest) returns (GetRoleFenceStateResponse) {}
message GetRoleFenceStateResponse { bool found = 1; RoleFenceState state = 2; bool retry = 3; }
```

强读玩家所在分片。鉴权:调用方密钥 `MMORPG_ROLE_FENCE_SECRET_<GUILD|TRADE|MAIL|MATCH|PLAYER_INBOX|FRIEND|CHAT>`。已删除的角色回 `found=true`、`account` 为空的状态,这样副本能自愈成墓碑;目录里没有回 `found=false`;读不到回 `retry=true`。

都带内回,不走 gRPC 错误:只读,没有结局未知,而读失败回 `Unavailable` 的话 `PublicationAdmission` 按结局未知算,要 Hold,Hold 不会自己清零。gRPC 错误只留给没装配、鉴权失败和调用方缺陷。`VerifyRoleAdmission`、`GetPlayerRoute` 同样。

没配密钥的调用方验不过,其余照常,不拒启。配了就必须互异,也不能和目录的其它调用方共用,否则只读调用方能冒充 login。

**排空证明按状态码判**(`serverbase.PublicationAdmission`,判定在 `publicationOutcomeUnknown`、`definiteRejection`)。这是安全边界:判成「确定」的请求不再挡排空,所以拿不准的都算未知。

| 结果 | 算 | 依据 |
|---|---|---|
| 带内业务拒绝 | 确定 | tip 表、服务私有码表 |
| 带内故障码、码表之外的码 | 未知,Hold | 同上 |
| `NotFound`、`InvalidArgument`、`FailedPrecondition`、`AlreadyExists`、`PermissionDenied`、`Unauthenticated`、`OutOfRange` | 确定 | 服务端明确没有写入,下面逐个核过 |
| `Aborted` | 确定 | 这次请求没有结局未知的写。目录的 `ErrNotCommitted`(§5.3)、`ErrRetryLater`(下面)、guild 的入口归属预检(只读)回它。handler 回它之前要能证明这一点 |
| `ResourceExhausted` | 确定 | 全仓只有目录的「账号角色数已满」回它,建角和过户都在提交点之前拒绝。别的 handler 不要拿它表示过载 |
| 闸门冻结(`errWriterFrozen`,内层拦截器确认这次请求没有结局未知的 SQL 才回它) | 确定 | 冻结的闸门上没有写入;之前提交成功的写结局已知 |
| 自愈读不到目录(`errNoWriteRetry`,同样由内层拦截器确认) | 确定 | 事务已回滚,见 §3.3。映射成非故障 tip 的不改写 |
| 其余 `Unavailable`、`DeadlineExceeded`、`Canceled`、`Internal`、`Unknown` | 未知,Hold | 提交可能已经成了 |
| `Unimplemented`、`DataLoss` | 未知,Hold | 没有逐个证明,拿不准 |
| 非 gRPC 错误(SQL、context) | 未知,Hold | handler 没下结论 |
| 包了一层的 gRPC 错误(`fmt.Errorf("...: %w", rpcErr)`、`errors.Join`) | 未知,Hold | 转发的是下游的结论,说不准这边写没写。只认错误本身就是 status |

**`ErrRetryLater`**(目录的结局已知、同参数重放,RPC 回 `Aborted`):投票问不齐、试探问不齐、这一轮结论作废、前面的角色已删后面的被拒、过户已提交但第 3 步没做完、只读步骤失败、协调者没装配。这次调用可能什么都没写,也可能已经提交过东西,所以它不等于「没提交」。两个码共用 `Aborted`,客户端按方法解读:只有 `CreateAccountRole` 的 `Aborted` 转成 `ErrNotCommitted`,login 凭它释放名字;别的方法都转成 `ErrRetryLater`,调用方只能重放,不可逆的判定要走自己的串行点(退款走 `Cancel`,§5.4)。错误链上有 `ErrUnavailable`,只认识后者的调用方照旧重试。写的步骤失败不用它:提交可能已经成了,照旧回 `Unavailable`、Hold。

**全仓核对结论**(2026-10-31,data_service、trade、guild、friend、chat、match、player_inbox、mail、rank,这些是挂发布准入的全部服务)。问题是有没有 handler 在事务已提交或结局未知时回上面判成确定的码:

- **拦截器和入口校验**:会话、鉴权、身份(在线域共用的入口准入 `roleidentity.EntryInterceptor`,guild 的入口归属预检 `OriginalIdentityInterceptor`,`domainhost.Guard`,`RoleFenceParticipant.admit`,mail、rank 的 `caller`)都在 handler 写之前,一个字都没写。入口准入只回 `PermissionDenied` / `Unauthenticated`。guild 的预检只有角色确实变了才回 `PermissionDenied`,闸门冻结、目录读不到、库故障回 `Aborted` 让客户端重试:这一层在 serverbase 外面,冻结不会被拦截器改写,回 `Unavailable` 又会被外层打成 Hold。friend 入口 2026-11-06 起不再预检([gate 在线身份](gate-online-identity.md) §9.3):角色变了、目录读不到由业务事务带内回 `ErrRoleNotVerified`,闸门冻结由业务链里的 serverbase 改写成确定没写的可重试状态。
- **friend、chat、trade、mail、rank**:业务结论全部带内回,gRPC 错误只有上一条那些,加 trade `SeedListing` 在入口拒绝。
- **guild**:`PermissionDenied` 都在写之前(`clientWrite`、`activityPrelude`、`economyCaller`)。`ListAppliedAssetOpsSince` 只读。转发的下游错误是包了一层的,算未知。调 match 的结果全部映射成带内结论。
- **player_inbox**:`FailedPrecondition`(`ErrBattleSettlementOriginal`)出在 `validateBattleOriginal` 或玩家事务里,事务回滚;同局内容冲突时读到已有条目,没有写。
- **match**:`CommitBattleOutcome` 的 `FailedPrecondition`(`ErrBattleOutcomeOriginal`)都在 `tx.Commit` 之前,提交失败回的是 `Unavailable`。`CancelActivityBattle` 的 `FailedPrecondition`(`ErrDirectConflict`)同样在提交之前。原样转发下游错误的只有 `RequestBattleTicket`(`authorizeBattleTicketOnce` 只读)和结算回执查询(`LookupBattleSettlement` 只读,本地的 `UPDATE` 排在它成功之后),转发时本地都没有写。观战和整队开局的下游错误都映射成带内结论。
- **data_service**:`RegisterPlayerZone` 的 `AlreadyExists` 在事务里读到别的 home_zone,回滚。`GetPlayerHomeZone`、`LookupAccountRole` 的 `NotFound` 只读。`RegisterRoleAdmission`、`CloseRoleAdmissions`、`AckRoleTradeDrain`、`RepairAccountRole`、`CancelAccountRoleTrade`、`TransferAccountRole` 的 `ErrConflict`、`ErrMissing` 都在单个事务里,回滚。`Begin` 撤销之后回 `ErrConflict`:写过令牌又清了,结局已知。判死抬代次、记排空是只增的事实,不是这笔请求要的结局,`Confirm`、`Cancel` 判死之后才回拒绝时,收据没动,调用方看到的结论还是对的。

**核对时改的 handler**(都是写了东西还回确定性的码,不改的话是假证据):

| 位置 | 原来 | 现在 |
|---|---|---|
| `RemoveAccountRoles` | 前面的角色已经写了墓碑,后面的被拒回 `ErrConflict` | 不是第一名角色时回 `ErrRetryLater`,调用方重放。第一名就被拒照旧 |
| `BeginAccountRoleTrade` | FREEZING 提交之后判死回哨兵,原样变成确定性的码 | 这次调用提交过 FREEZING 时回 `ErrRetryLater`,重放拿收据 |
| `fenceDeadTradeHolders` | 抬过代次之后记 holder 回哨兵 | `ErrRetryLater` |
| `ReservePlayerName` | 撤销名字行失败还回 `ReserveConflict`(`FailedPrecondition`),login 当成确定没写 | 回错误,走结局未知,日志照记,由运维释放 |

**还会 Hold 的确定性拒绝**(保守方向,不影响安全,要放开先按上面的办法核):

- 只读 RPC 失败回 `Unavailable`、`Canceled`,比如 `GetPlayerHomeZone`、`BatchGetPlayerHomeZone`;好友、邮箱这类读路径库故障回故障 tip。
- handler 用裸错误回的确定性拒绝,比如 match 的地域探测(`errors.New("会话已经失效")`);`RegisterPlayerZone` 的非法参数刻意回 `Unavailable`。
- 目录写事务的语句失败、`BEGIN` 失败:事务其实没提交,这里不区分,回 `Unavailable`。
- trade 回报商城失败(幂等,新 boot 会重报);收据 ACK 依赖失败;trade 读登录态、读目录身份失败回故障 tip。
- player_inbox `EnqueueBattleSettlement` 撞上角色冻结回 `Unavailable`(目录读不到由 §3.3 的拦截器兜底)。
- chat 的身份校验读不到目录回故障 tip(`readActor`,只读)。
- match 后台(`HoldRelay`、outbox)撞 `rolefence.ErrUnavailable`:这里只放过闸门冻结,§3.3 的观察位只管 RPC 路径。

新 handler 回判成确定的码之前,要保证调用方不会把它当成「没写」做不可逆的判定;提交之后的失败用 `notRejection` 改成 `ErrRetryLater`,结局未知的不改。

### 3.7 入场登记校验

`account_role_admission` 在目录里,只有 data_service 连。match 的 `ReadTeamAdmission`(scene 读队伍)原来同事务读它,现在走目录 RPC,鉴权同 §3.6:

```proto
rpc VerifyRoleAdmission(VerifyRoleAdmissionRequest) returns (VerifyRoleAdmissionResponse) {}
message VerifyRoleAdmissionRequest  { common.base.RoleAdmissionIdentity admission = 1; common.base.ServiceCallAuth auth = 2; }
message VerifyRoleAdmissionResponse { bool valid = 1; bool retry = 2; }
```

- **防什么**:过期的入场原件读到这名角色的队伍。旧主的会话由同事务锁副本拒绝;这里挡的是同一修订下已关闭、或被新世代顶掉的世代。
- **窗口多大**:世代和关闭水位只增不减,而 scene 登记成功之后才会来读。所以校验放在读事务**之后**:读之后还有效,就说明读的那一刻也有效。读事务提交到 scene 收到回复之间登记照样能关,这一段原来也有(原来的行锁在提交时也放了),靠 scene 回调再比 `RoleAdmissionSystem::ConfirmedIdentity` 兜住。窗口和原同事务校验相同。
- **为什么不在副本里记最大已关闭世代**:这条路径只读,放在读之后已经和同事务一样严。关闭世代也不是护栏状态,不带 `fence_version`,推送丢了没法自愈,加了只多一份会落后的数据。
- **写路径不同**:player_inbox 的 `FetchItems`、`AckItems` 要写库,校验只能放在事务之前,见 §3.4。
- 目录读不到回 `retry=true`,不放行。match 的 `ReadMembership` 同样带内:登记失效、角色冻结、目录读不到都回 `ReadTeamMembershipResponse.retry=true`,回包照样签名,队伍字段是空的。scene 验过签之后这一轮不更新队伍,和调用失败一样;不能把空字段当成没有队伍。投票期间也回它:投票能通过说明这名角色没有队伍(`team_membership` 是挂起),不缺什么。

## 4. 公会历练与匹配互斥

今天 guild 跨库读 match 的票和 launch,match 跨库读 guild 的 launch 和大厅,靠同一把 P 锁串行。护栏分开之后 P 不再串行它们,要换成单一仲裁:**match 是战斗入口占用的唯一权威**(历练本来就要经 match 开局)。

match 库新表 `match_role_occupancy(player_id PK, owner_kind, owner_id, account, owner_revision, expires_ms)`,索引 `owner_kind,owner_id,player_id`。

新 RPC(guild 调 match,`ServiceCallAuth`,caller 是 `guild`,密钥 `MMORPG_MATCH_INTERNAL_SECRET_GUILD`,和取消共用这一把):

- `AcquireTrialOccupancy(owner_id, roster, ttl_ms)`:一个事务里按 player_id 升序 `rolefence.LockAll`,核票空闲、无 team launch、占用空闲或同一 owner,写占用。全成或全不成。同 owner 重放幂等,只延长不缩短。roster 里修订号为 0 的角色从本库副本捕获身份:没有冻结的副本行不会落后于目录,过户、删角之前全部参与者都先冻结了。
- `ReleaseTrialOccupancy(owner_id, player_ids)`:只删 owner 相同的行。幂等。
- `GetBattleCompletion(battle_id)`:取代 guild 对 `match_battle_completion` 的跨库读。
- `StartActivityBattle` 加 `auth`,guild 签名。验签在参数校验之后、任何 SQL 之前,失败回 `INTERNAL`。持久化的意图正文不含 `auth`:每次重试的时间戳不同,正文要逐字节相等。

三个新 RPC 的结论都在响应的 `reject` 里(`OCCUPIED`、`ROLE`、`DENIED`、`RETRY`),不回 gRPC 错误:写入失败回 `Unavailable` 的话 `PublicationAdmission` 按结局未知算,要 Hold(§3.6)。Release、GetBattleCompletion 进 relay 方法清单,写者排空之后还能放开、能结算;Acquire 只在源写者上走。

规则:

- guild 在建房、同意、首次 Prepare 的事务之前 Acquire,房间没建成就 Release。Release 的 `defer` 挂在 Acquire 之前:Acquire 超时时 match 可能已经写了占用,不放的话这批人要等到租约到期;确定拒绝时放开是空操作。`requireTrialEntryFree` 里读 match 的两条删除,读 guild 自己的留着。本库能答的(冷却、别的大厅占着)先答,不走到 match。match 拒绝回 `ErrWriteConflict`;没问到结论回 `ErrTrialMatchUnavailable`,logic 回「服务繁忙」。已有 launch 原件的 Prepare 重放不再 Acquire:Start 可能已经写了票,这时 match 一定拒绝。
- **占用有租约**,过期视为空闲。不单独起续租循环:建房租到邀请截止 + 30 秒,同意时按邀请时长再续,Prepare 续 30 秒盖住开战窗口,开局提交时 match 延到战斗期限 + `WorkLeaseMs`。guild 停摆时续不了租,也发不了 Start,到期不会造成重复占用。
- **match 的 `StartActivityBattle` 是提交点**:按 `LaunchRequestId` 核占用未到期、由这个 owner 持有、roster 身份逐项相等,名单两个方向都相等,然后写 match 自己的票。租约丢了就拒绝,guild 按开局失败收尾。提交点之后接着成局时(`matchEntryOriginal.admitted`)不再要求占用还在:全员票已写,Acquire 见到票就拒绝。
- **占用行开局之后不删,也不转成票**。票终局到 guild 结算之间,原判据靠 launch 未终结拒绝新入场,这一段由这一行接着拒。行留到 guild 释放(结算、毒消息、取消终局、大厅结束)或到期。Release 丢了不要紧,有租约。经 `withSQLLobby` 的事务只要看到大厅已结束就补一次 Release,不只是这一次结束的:同意之前的续租可能排在别人结束大厅之后,又把占用写回去。Release 幂等,只删这个 owner 的行。
- match 自己入场(`requireEntryOriginalsTx`)读 `match_role_occupancy` 代替跨库读 guild。`requireOriginalActivityTx` 对 guild 原件的逐项核对换成:请求由 guild 密钥签名,加上占用行的 roster 核对。
- 没配 `MatchRpc` 时历练本来就不开放:建房、开局回 `ErrTrialMatchUnavailable`,巡检器没有 SQL 终局兜底,只靠 Kafka。

### 4.1 判据对照

原来能拒的情况,现在要么在 match 的占用行上拒,要么在 guild 自己的事务里拒。

| 原判据 | 原来在哪 | 现在在哪 |
|---|---|---|
| 角色有未释放的票 | guild `requireTrialEntryFree` 跨库读 `match_authority_ticket` | match `AcquireTrialOccupancy`,语句原样。调用点仍是建房、同意、首次 Prepare |
| 角色有未终结的整队开战 | guild `requireTrialEntryFree` 跨库读 `team_launch_role` | 同上 |
| 角色有未终结的历练 launch,或别的大厅占着 | guild `requireTrialEntryFree` | 不变,留在 guild 事务 |
| 大厅之外的入场(排队、整队、切磋):历练 launch 未终结 | match `requireEntryOriginalsTx` 跨库读 `guild_trial_launch_role` | match 读未到期的占用行 |
| 大厅之外的入场:大厅未结束 | match `requireTrialLobbyEntryTx` 跨库读 `guild_trial_lobby_slot` | 同上 |
| 活动开局:launch 原件身份等于票 | match 跨库读 `guild_trial_launch_role` | 占用行 owner 等于请求,`account`、`owner_revision` 相等 |
| 活动开局:缺大厅占位 | match 跨库读 `guild_trial_lobby_slot` | 占用行缺失即拒 |
| 活动开局:大厅 state=2、battle=0、全员同意、视图一致 | match `requireTrialLobbyEntryTx` | guild 的 Prepare 事务把关(`lockTrialLobbyForLaunch`、`lobby.State==2`、同意齐全),Start 只有 Prepare 成功才发,由签名担保 |
| 大厅行缺失 | 同上 | guild 的挂起判定和 `lobbyView` 拒,match 不再看 guild 的行 |
| 活动开局:launch state=1、battle=0 | match `requireOriginalActivityTx` | 取消墓碑(`current.Cancelled`)和 `FindDirect` 重放 |
| 活动开局:摘要、原件推导的请求等于收到的请求 | 同上 | guild 签名 |
| 活动开局:名单等于原件 | 同上 | 占用行名单双向相等(`requireActivityRosterTx`) |

语义确实变了的地方:

- 篡改活动上下文的拒绝从 store 层移到 logic 验签:store 没有 guild 的原件可比了。
- guild 卡过战斗期限 + `WorkLeaseMs` 之后,这批角色不再被占用行挡新入场。原来 launch 未终结没有 TTL,会一直挡。票未终局时票本身还挡;guild 的挂起判定不受影响,这批角色照样不能过户、删除。
- 两个大厅同时建、名单重叠时,输家可能回 `ErrWriteConflict`(match 先判)而不是 `MemberBusy`,重试一次就是 `MemberBusy`。
- Release 丢了,这批人到期之前入场被拒。

## 5. 目录分片

### 5.1 槽

`Directory.LogicalShards = K`,K 是 64 的约数。生产首次上线定 64,开发定 1。**上线后 K 不能改**,扩容靠把逻辑库搬到新集群(§8)。

```go
func SlotOfPlayer(id uint64, k int) int   // placement.RouteRowShard(id) % k
func SlotOfAccount(a string, k int) int   // crc32(a) % 64 % k,账号按原字节算
func SlotOfName(norm string, k int) int   // crc32(norm) % 64 % k
func DirectoryDBName(base string, slot, k int) string // K=1 是 base,否则 <base>_00
```

在 `shared/dirshard`,金样钉在 `dirshard_test.go`,上线后这些值不能变。

| 按玩家 | 按账号 | 按名字 | 只在第 0 片 | 每片一份 |
|---|---|---|---|---|
| `account_role`、`account_role_admission`、`account_role_trade`、`account_role_trade_holder`、`account_role_transfer_receipt`、`account_role_removal`、`player_route`、`player_name_owner` | `account_directory`、`account_role_ref` | `player_name` | `zone_merged_into` | `route_meta`、写者闸门表、`db_move_sentinel`、`directory_shard` |

过户收据跟玩家走,请求里都有 `player_id`。listing、tx_id 的主键只在一片内唯一:两个玩家用同一个 listing 不再互相挡,它们都由发号器发,本来就不会撞。

**`directory_shard`**:每片一行 `(id=1, logical_shards, slot)`,`-migrate` 种下,每个 store 开池时核对。配错 K 会把玩家路由到别的片,读到「角色不存在」而且没有报错,所以对不上拒绝启动。

**连接池**:每片一个池、一份写资格。`AccountDirectory.MaxOpenConn`、`PlayerName.MaxOpenConn`、`RouteStore.MaxOpenConn` 都是每片的上限,连接总账按片算。写者启动目录给的写资格必须恰好是片库 + 流水库 + 全部存档库(`Config.WriterSchemas`),多一份少一份都拒绝启动。

**`-migrate`**:逐库开池、拿迁移锁,先核完全部库都是空的再动手,只要有一个库已有表就一张都不建。中途失败时已经建好的库不回退,人工清掉那批库再跑(未上线,没有存量)。库要提前建好,`-migrate` 不建库。

目录只有 data_service 连。go/db 选库改调 `GetPlayerRoute`(强读,可重试),match 的 `home_zone` 读护栏副本或 RPC。

### 5.2 账号到角色

新表 `account_role_ref(account, player_id, state, tx_id, listing_id, home_zone, nonce, created_ms)`,主键 `(account, player_id)`,索引 `(state, created_ms)`,放账号所在分片。`tx_id`、`listing_id` 只有过户的 PENDING 才有,`home_zone` 只有建角的 PENDING 才有,都是判陈旧要用的。

**不变量:ref 集合 ⊇ 归属真相。** 真相永远是 `account_role.account`,ref 只是索引。写 ref 先于提交点,删 ref 要能证明这个账号不会再因为在途的写拥有这名角色。读角色列表:读 ref,按玩家分片批量读 `account_role`,只留 `account == 这个账号` 的。

- 陈旧 ref(卖家没删掉、建角失败)读的时候被过滤,交给后台。
- 角色上限按 ref 计,所以真实角色数 ≤ ref 数 ≤ 上限。到上限时先剔除被证明陈旧的 ref,再判。证明读失败时回错误,不说是满了。

**陈旧的证明**,都在读完 ref 之后读真相。真相就是这个账号时 PENDING 补成 ACTIVE,不删:

| ref | 真相不是这个账号时 |
|---|---|
| ACTIVE | ACTIVE 只在提交点之后写,所以已易主或删除,删。没有角色行不删,只记错误日志:角色行从不删除,库被人动过 |
| PENDING、`tx_id≠0`(过户) | 提交要求 listing 是 FROZEN,而 FROZEN 只会走到 TRANSFERRED 或 CANCELLED。到了这两个终态才删;还在 FROZEN 不算陈旧。listing 缺失不删,只记日志:第 1 步之前核过它是 FROZEN,FROZEN 的收据不会删 |
| PENDING、`tx_id=0`(建角) | 角色行已存在且不是这个账号:严格插入不会再成功,删。没有角色行:未超过 5 分钟不动(可能在途);超过之后在玩家分片的事务里写一条墓碑挡住迟到的建角,然后才删。写墓碑时发现建角先提交了,补 ACTIVE |

**过户的 PENDING 先读 listing、后读真相**。反过来的话:读到真相是卖家,这时过户提交,再读到 listing 已是 TRANSFERRED,就把买家的 ref 当陈旧删了;第 3 步补 ACTIVE 又失败的话,买家拥有角色却没有 ref,没有任何路径能再发现。先读 listing 时,读到终态说明这笔过户的提交已经在这之前,接着读到的真相一定盖住了它;读到 FROZEN 就不删。

**重新占用的竞态**:证明之后过户第 1 步可能又占了这条 ref。ref 的每次写都换 `nonce`,删除和补 ACTIVE 都按读到的 nonce 比较,对不上什么都不做。

**写 ref 的两个窗口**:

- 过户第 1 步用 `INSERT ... ON DUPLICATE KEY UPDATE`。后台的 `dropRef` 不持目录行锁,可能抢在读完 ref 之后删掉它;`UPDATE` 这时影响 0 行还回成功,过户就在没有 ref 的情况下提交了。
- 第 3 步写 ACTIVE 带 `tx_id` 比较,只在 ref 属于本笔过户或者没有在途过户时才改。覆盖别的过户的话,那条 ref 变成 ACTIVE,后台按「ACTIVE 且真相不是这个账号」把它删掉,那笔过户提交之后买家就没有 ref 了。

**退还名额**:建角在提交点之前失败时,这次尝试不会再提交,它写的 ref 不用证明,按 nonce 直接删(`releaseOwnRef`)。nonce 由调用方在写之前生成,所以第 1 步结局未知时也删得掉;ref 是上一次尝试留下的话 nonce 对不上,留给后台。删除失败无害。login 对一个 `player_id` 只发一次建角,没有并发的同 id 重放。

**后台清理**:`SweepStaleRefs` 和删角收尾同一个循环,每分钟一轮,每轮 64 条,走 RELAY 门禁,结果未知时 Hold。清两类:读路径发现的(内存队列,满了丢,下次读还会发现),超过 5 分钟还是 PENDING 的(逐片扫,逐轮换起点)。多副本同时跑靠 nonce 互斥。没有协调者时也跑。

### 5.3 建角(分步,都幂等)

先预读玩家分片。已有角色行:同账号同内容是重放,补第 4 步,回列表,不再碰存档(玩家可能已经存过盘);归属不同、已挂寄售、已删除(墓碑)一律 `ErrConflict`,删除后的角色不能复活。然后读路由选存档库:没有路由 `ErrUnavailable`,`home_zone` 不符或没有落点 `ErrConflict`,落点不在白名单 `ErrUnavailable`。

1. 账号分片事务:锁 `account_directory` 行;核上限;写 ref(PENDING)。已有这条 ref 不占新名额。
2. 存档库事务:写初始行(`player_store`),`ON DUPLICATE KEY` 不改已有行,事务内读回比较:已有行不是建角写的那几行回 `ErrConflict`,事务回滚,不覆盖。目录没提交时没人读得到,失败只留下孤儿行,重放时读回相同就过。
3. 玩家分片事务:核路由行(含 `storage_id` 等于第 2 步写的库);严格插入 `account_role`。**这是提交点。** 提交之后在名字片把名字行从预留置为已绑定(先重读角色行复核,见 §5.5),失败不改结局。
4. 账号分片:ref 改 ACTIVE。失败无害,列表按真相过滤,后台会补。

| 失败在 | 留下什么 | 结局 |
|---|---|---|
| 第 1 步之前(预读、读路由) | 无 | `ErrNotCommitted` |
| 第 1 步 | 无,或者结局未知时的目录行 | `ErrNotCommitted`,按这次的 nonce 删 ref。满位回 `ErrFull` |
| 第 2 步 | 目录行、可能有孤儿初始行 | `ErrNotCommitted`,删 ref。已有行不是建角写的回 `ErrConflict` |
| 第 3 步 BEGIN 失败(闸门拒绝、连不上) | 目录行、初始行 | `ErrNotCommitted`,删 ref。一条业务语句都没发 |
| 第 3 步其余失败 | 目录行、PENDING ref、初始行 | 结局未知,回错误,login 按 player_id 强读消歧;已提交的话预读走重放。事务内语句失败其实也没提交,这里不区分,按未知算 |
| 第 3 步被拒 | 同上 | `ErrConflict`。别人先建了或已删除时当场删 ref;路由不符证明不了,留给后台 |
| 第 4 步 | PENDING ref | 回成功 |
| 读回列表 | 已提交 | 回错误,重放回列表 |

**`ErrNotCommitted`**:这次调用确定没有写角色行。存档库独立成故障域、搬库又是计划内操作,这条路径是常态,不能和结局未知混。

- RPC 回 `Aborted`,客户端转回 `ErrNotCommitted`。闸门冻结的拦截器不改写它(`writerFrozenFailure`),否则 login 只能按未知处理。排空证明按确定算(§3.6)。
- login 收到它释放名字,回「稍后重试」的 tip,不强读,不记孤儿。玩家重试时铸新 id,不会撞上自己占住的名字。
- login 对一个 `player_id` 只发一次,所以收到它就能确定角色没建成。同 id 重放方不能这样认为:上一次尝试可能已经提交。

和同事务相比变了的:建角失败之后 `account_directory` 行已经提交,不回退,目录行本来就只增不删。结局未知时 PENDING ref 也留着,在清掉之前占一个名额。

名字在建角之前已由 `ReservePlayerName` 占住(预留态),提交之后绑定;login 收到 `ErrNotCommitted` 释放名字时,data_service 先在玩家片证明这名角色永远不会提交(必要时写墓碑),再删预留态的名字行(§5.5)。

**多集群时间点恢复之后的半截建角(2026-11-05,未编译)。** 这几步分在不同的库,库又可能分在不同集群;各集群各自恢复到同一个 T 时实际恢复点错开,提交点(第 3 步)在 T 之前、前面某一步所在的库却恢复到了那一步之前,就留下三种半截:ref 缺失(角色从列表消失)、名字缺失(名字能被别人再登记)、初始行缺失(进不了场)。data_service 的 `cmd/create_residual` 以角色行为准按玩家片分页核账,默认只报告,显式选种类才修:补 ACTIVE ref、按原名重新登记、重写初始行;名字已归别人时只报告不覆盖。判据、修复的原子范围和操作步骤见 [PITR 手册](../ops/mysql-backup-pitr-runbook.md) §4.1a,实现在 `go/data_service/internal/store/create_residual.go`。

### 5.4 过户

先读 `tx_id` 收据。有:摘要相同就补第 3 步,回原收据;摘要不同 `ErrConflict`。再核 listing 是 FROZEN、原件相符,不是就再读一次收据(同一笔过户可能就在两次读之间提交了),还没有就 `ErrConflict`:注定被拒的请求不该占买家的名额,带 listing 的 PENDING ref 的陈旧证明也靠这一条。

1. 买家账号分片事务:锁 guard;核上限;写 ref(PENDING,带 tx_id、listing_id)。已有这条 ref 不占新名额,改写成这笔过户并换 nonce,挡住在途的清理。满位回 `ErrFull`,不写收据,trade 走下面的退款出口。
2. 玩家分片事务:写收据、改 `account`、`owner_revision+1`、清令牌、`fence_version+1`、收据 `TRANSFERRED`。**这是提交点。** 上限在第 1 步按 ref 判过了,这里不再判;事务内再核 listing 是 FROZEN、归属和令牌没变。
3. 读护栏状态。真相还是买家才把 ref 改 ACTIVE(这笔过户之后角色可能又卖了,重放不能把陈旧的 ref 写回去);删卖家 ref;推 `ApplyRoleFence`。读护栏状态或写 ACTIVE 失败回 `ErrRetryLater`,trade 用同一 `tx_id` 重放补上:买家的列表靠这条 ref,这一步要么确认写成,要么让调用方再来一次。过户已提交,结局已知,不打 Hold(§3.6);只有写 ACTIVE 这条语句本身结局未知时才回 `ErrUnavailable`。写 ACTIVE 失败时删卖家 ref 和推送照做。删卖家 ref、推送失败无害,只记日志。

第 2 步被拒(`ErrConflict`、`ErrMissing`)时走一遍判陈旧:listing 已到终态就删第 1 步的 ref,还开着留给后台。第 2 步结局未知时什么都不删,同 `tx_id` 重放。

**买家满位的退款出口**(trade `RmbConsignments.refundRole`)。买家已付款、合同在 `rmb_delivery`,`Transfer` 回 `ErrFull`,或者回 `ErrConflict`(listing 已经不是 FROZEN)。重试不会自己好,没有出口的话钱和角色都卡住。退款不可逆,不能退了款过户又成了:超时的上一次调用可能还在途。所以先和提交点串行,`Cancel` 和第 2 步抢同一个 listing 行锁,提交点要求 listing 是 FROZEN。

| `Cancel` 的结局 | 说明 | 接下来 |
|---|---|---|
| `CANCELLED` | 过户再也提交不了 | 落 `rmb_refund`,合同状态 8。`deliver` 下一轮回报 `REFUND_PENDING`,商城退款,再发回退意图,`cancelRole` 重放原收据,合同到 `rmb_returned` |
| `ErrConflict` | 可能过户已经提交了 | 同一 `tx_id` 重放 `Transfer`。拿到收据就落 `rmb_delivered`。还被拒就是别的冲突,不退款,按 `role_conflict` 分钟级复查 |
| 别的失败 | 没有结论 | 不退款,下一轮再来 |

崩溃在 `Cancel` 提交之后、落退款之前也没关系:下一轮 `Transfer` 因为 listing 已取消回 `ErrConflict`,走同一条路,`Cancel` 重放原收据。`Transfer` 回 `ErrUnavailable`、`ErrRetryLater` 不算被拒,只重放,不取消。不等买家腾位子:没有通知通道,等不到。`Cancel` 之后角色就回到卖家手里,合同未终结期间它还是挂起,挡着新的 Begin 和 Remove。`rmb_refund` 期间卖家不能取消合同:状态改走的话 `deliver` 不再回报,钱退不出去。

**提前挡一下**(`CheckAccountRoleSlot`,只读,带内回,§3.6 的办法;只算真相是这个账号的角色,在途和陈旧的 ref 不算,所以不算数)。两处:买家下单之前,满了回 `kLoginAccountPlayerFull`,读不到放行;`deliver` 进 `rmb_delivery` 之前,满了直接落 `rmb_refund`,没有过户原件,不用先取消,读不到这条意图下一轮再判。

### 5.5 名字

`player_name` 按 `name_norm` 分片,主键 `name_norm`,这张表是名字归属的真相,唯一性由主键保证。表上不再有 `uk_player_name_owner`:它和名字不在一个库就保不住「一个角色只登记一个名字」。

`player_name_owner(player_id, name_norm, name)` 放玩家分片,主键 `player_id`。`BatchGetPlayerName(ids)` 不做全分片扫描,按玩家分片拆组读它;任一片失败整批报错。

**`ReservePlayerName`**(每步一条自动提交语句):

0. 读 owner 行。指着别的名字时去名字分片核:确实是自己的回 `ReserveConflict`(ID 复用事故),什么都不写;已经没了或归了别人,这条 owner 行是上次释放没清掉的,按名字删掉。
1. 写名字行(ODKU)。**这是提交点。** 撞键时读这一行:是自己 `AlreadyOwned`;是别人 `Taken`,不写 owner 行,第 0 步读到 owner 行指着这个名字的话顺手删(失败只记日志);读不到(并发释放)重插。
2. 写 owner 行(ODKU)。撞键时读出来比名字。

| 情形 | 结局 |
|---|---|
| 第 2 步失败 | 回错误,名字行留着。login 按结局未知处理:同参数重试走 `AlreadyOwned` 把 owner 行补上,或者 Release 把两边都清掉 |
| 第 2 步发现 owner 行已是别的名字 | 第 0 步之后又登记了别的名字,只有 ID 复用事故到得了。名字行是这一次插的就撤销,回 `ReserveConflict`;撤销失败回错误(名字行留下了,不能回确定性的拒绝,§3.6),日志照记,由运维释放。不是这一次插的不撤:它可能属于在役角色 |
| 第 2 步发现 owner 行没了 | 并发释放删的,回错误 |
| 同参数重放 | 每步幂等,`AlreadyOwned` |

**名字行「预留 → 绑定」**(2026-11-06,未编译;状态机全文见 [名字设计](guild-phase2/03-names.md) §3.4a):`player_name.bound_ms` 0 = 预留,非 0 = 已绑定。名字片和玩家片没有跨片事务,按下面的先后串起来,每步只写一个逻辑库、各自幂等:

- **绑定**:建角第 3 步提交之后,目录先在玩家片重读角色行(在役、名字就是这个),再在名字片 `UPDATE … SET bound_ms=? WHERE name_norm=? AND player_id=? AND bound_ms=0`。名字行不在就按原名重新登记再绑。失败不改建角结局,名字停在预留态,同一建角重放或 `cmd/create_residual`(`name_unbound`)补上。
- **删角释放**:墓碑提交之后先核角色行确是墓碑,再按墓碑里的原名删名字行(不看状态),最后删 `account_role_removal` 那一行。那一行在墓碑事务里留着,收尾没做完后台 `ResumeRemovals` 接着做。

**`ReleasePlayerName`**(只释放预留态):不带 token 时先核 login 的调用证明;再在名字片读一次名字行,没有这一行、已绑定、登记早于窗口都在证明之前就回,两边零变更(证明可能写墓碑,不可逆);有窗口内的预留态才在玩家片的一个事务里证明角色永远不会提交:先锁路由行、再锁角色行(和提交点同一个锁序),在役拒绝,墓碑放行,没有角色行而有路由行就写墓碑挡住迟到的建角,没有路由行不写;然后条件删名字行(`Deleted`、`Absent`、`OutsideWindow` 的判定不变,另加 `bound_ms = 0`,已绑定回 `Bound`),`OutsideWindow`、`Bound` 两边都不动,其余接着按名字删 owner 行。删 owner 行失败回错误,重放时名字行已不在,走 `Absent` 接着删。

**两边不一致时以 `player_name` 为准**,判定归属的路径(`Reserve`)都读它。`BatchGet` 不回名字分片核对:不一致只发生在建角失败的角色上(释放没清完 owner 行),没有人读它们的名字。

名字抢注的死锁结论(聚簇主键 + ODKU,真库 0/15)是在表上还有 `UNIQUE(player_id)` 时测的,去掉之后没有重测;少一个索引只少一条取锁路径,原来那两形同一 player_id 上的并发写随之消失。真库用例照旧断言零重试。

### 5.6 路由

`RouteStore` 按玩家分片,每片一个池;批量读按分片拆组,任一片失败整批报错。`route_meta` 每片一份。`frozen_run_id` 列和 `chk_route_frozen` 删除,proto 里 `reserved 5`。`zone_merged_into` 只在第 0 片,`GetMergedZones`、`tools/merge_zone`(`-route-db-dsn` 指第 0 片)都读写那里。

`route_tool tidb-prepare` 逐片做,每片只切落在自己身上的行分片,前缀切点数不变(K=64 时每片一个行分片,行数据不用切,每个前缀一个切点)。K=1 时语句和分片之前逐字相同。输出带片库名。`route_tool freeze`、`unfreeze` 一次一个库,库名由维护原件给,只认目录片和流水库。

## 6. 存档与目录分开

`player_store` 在存档逻辑库,和目录可能不在一个集群。三处同事务改分步:

| 流程 | 现在 | 改成 |
|---|---|---|
| 建角初始行 | 目录事务里写 | §5.3 第 2 步 |
| 过户证明 `VerifyFrozen` | 目录事务里读两行 | `ReadFrozenEvidence`:目录事务外用存档库连接读出证据,再在目录事务里核收据与下限并落证据 |
| 判死 `fenceDeadTradeHolders`、`ReleaseDeadPlayerStoreHolder` | 目录事务里抬代次 | 先在存档库事务抬代次(只增,幂等),再回目录事务记 holder。中途崩溃重跑 |

data_service 按 `storage_id` 持有存档库连接池(`Directory.StoragePools`,按需打开,打开失败冷却 10 秒,与 go/db 同口径)。白名单是 `AllowedStorageIDs`,不在白名单的落点回 `ErrUnavailable`。每个池用这个库自己的写资格,闸门跟着库走。账本读、展示读同样走它。展示读按落点分组,一个落点打不开、查询失败或读到一半断了都只记日志,跳过这个落点的全部玩家,不留半份资料:好友列表这类调用方只缺这些人。账本读不跳过,读不到回错误:调用方拿它判账本有没有见过一笔资产。

**判死的顺序**(`fenceDeadTradeHolders`):

1. 存档库普通读写权行,记下 `(E, H, claim_seq)`。
2. 目录事务:trade 还开着、角色还锁在这个 listing 上,取待判持有者和它们登记的最大代次。令牌挡住了新的 Register,待判集合不会再变。
3. 判定(纯函数):有待判持有者还是现任就停,等真实 ACK。写权行已经挡住了全部待判持有者(没有现任且代次大于登记的最大代次)就不再抬。否则代次抬到 `max(E, 最大登记代次)+1`。
4. 存档库事务:写权行还是第 1 步看到的 `(E, H, claim_seq)` 才抬。先抬代次,持有者才真的写不了存档。
5. 目录事务:角色还锁在这个 listing 上、护栏版本没变才记 holder 排空、关登记。

第 1 步排在第 2 步前面是第 4 步比较并写的依据:第 2 步看到角色还锁着,说明第 1 步读到的写权行还在锁定期内,不是解锁之后新持有者的。之后取消赢了、新的持有者认领了,写权行就变了,第 4 步什么都不做,所以一条挂住的判死不会踢掉解锁之后的新持有者。第 4 步之后崩溃重跑:第 3 步看到已经挡住,不再抬,接着走第 5 步。第 5 步之前崩溃不留下「记了排空但没抬代次」的状态。判死都在目录事务外做,Begin、Confirm、Cancel 都先判死,再开目录事务。

**`claim_seq`**:写权行 `claim_seq BIGINT UNSIGNED NOT NULL DEFAULT 0`,`LoadAndClaim` 每次加 1,同实例同代次也加。只比 `(E, H)` 比不出同实例重新认领:判死读到 `(E0, X)` 后挂住,令牌清除,玩家同节点重登,`LoadAndClaim(E0, X)` 把写权行写回同样的值,比较成立,判死把在线玩家踢掉,进度丢失。存盘和判死都不动这一列:它回答的是「有没有人重新认领过」。不进 scene 协议,也不进冻结证据的摘要:冻结期间没有人能认领。

**Confirm 的证据**:

1. 判死。判死抬代次只发生在读证据之前。
2. 目录事务核这一轮的持有者全部排空,取持有者数。holder 集合在进 FREEZING 时已固定。
3. 目录事务外读证据:一条语句读这名玩家的全部存档行,比较下限、世代、初始形态,算摘要。排空之后存档不再变,所以这份证据到提交时仍然成立。
4. 目录事务:收据还是 FREEZING、归属和令牌没变、持有者还是这一批且全部排空、证据不低于下限;初始提交的证据还要 `initial_player_commit=1`、修订号还是 1、没有持有者。都对才落 FROZEN。

判据逐条和同事务时相同,只是读的位置变了。证明读不到或读到不一致一律不落证据,收据停在 FREEZING。

## 7. go/db 选库

- 认领前调 `GetPlayerRoute` 读落点,存盘带认领时返回的 `storage_id`。go/db 不连目录,配置 `DataServiceRpc` 必填,`Placement.RouteSchema` 删除。
- `GetPlayerRoute` 强读玩家所在分片,回 `found`、`storage_id`、原始 `home_zone`。没有行或没有落点回 `found=false`,go/db 拒绝认领;目录读不到回 `retry=true`,go/db 回暂不可用,scene 退避重试,不能把读失败当成没有落点。都带内回,不打成 Hold,见 §3.6。走 RELAY 门禁。
- 鉴权:`ServiceCallAuth`,caller 是 `db`,密钥 `MMORPG_ROLE_FENCE_SECRET_DB`,和角色护栏的调用方同一命名。data_service 没配密钥时这个 RPC 回 `Unavailable`,其余照常;配了就不能和别的调用方共用。go/db 缺配置或缺密钥拒绝启动。go/db 不在启动时读一次:data_service 可能还没起来。
- `storage_id` 建角后不变,所以不在写事务里核路由。物理位置由 §8 的映射决定,闸门在同一事务里拦住旧位置的写。
- **不做按玩家搬库**。负责人要的是搬物理库,§8 覆盖。要做按玩家搬库时,必须在写权行加「已搬走」墓碑并在 `decideSave`、`decideClaim` 里判,这条记在这里。`player_route.frozen_run_id` 没有用处,删除。

一个进程要写多个存档库时,各库一份闸门(§2),库可以在不同集群。

## 8. 不停服迁移物理库

### 8.1 映射

不换 `*sql.DB`,在连接层按映射拨号。业务代码、repo 都不用改。

`shared/dbplacement`:

- etcd 键 `/mmorpg/db_placement/<逻辑库>`,值 `{cluster, version, state}`,外加工具的搬迁记录 `move`(进程不读)。没有键时是配置里的默认集群、版本 0。版本只在翻转时加 1,`prepare`、`rollback` 不动它,连接池不用换连接。
- 配置 `DbPlacement: {DefaultCluster, SqlClusters: {<名字>: {Hosts}}, EtcdHosts}`。账号口令沿用写者启动目录给的连接串,不进配置文件,账号在目标集群提前建好。整段不写就不按映射拨号,行为和原来完全一样。段的位置:guild、trade、friend、player_inbox 在 `MySQL` 下,match 在 `GlobalMatch` 下,data_service 在 `SnapshotMySQL` 下,chat、mail、rank、go/db 在顶层。一次性工具(`writer_authority`、`maintenance_phase`)读环境变量 `MMORPG_DB_PLACEMENT`,值是同形状的 JSON。
- **默认集群持久化**:etcd 键 `/mmorpg/db_placement_default`(不在映射前缀下),首次部署写入,之后配置不一致拒启(`Start`、`ResolveOnce`、`dbmove plan|prepare`)。没有映射键的库都在这个集群上,配置里换一个默认集群等于让它们的连接全指向别处,而且没有人冻结过它们。
- `Watcher`:启动时读一次全量,读不到就拒绝启动(分不清「没有映射」和「读不到」的进程可能连到已搬走的库)。之后跟 Watch,断了全量重读,另外每 30 秒全量读一次兜底。`Current(逻辑库)` 回答当前映射。etcd 故障时继续用最后读到的映射。坏值、版本回退、搬过的库的键被删都不接受:留在原映射上最坏是写不进去,跟着走可能回到一个没人冻结的旧集群。
- **etcd 回档**:`applyPut` 先比 ModRevision,不比已有的大就跳过,不计数(全量读和 Watch 的事件会重叠)。所以 etcd 从备份恢复又没有 `--bump-revision` 时,之后写的每个键都比进程记下的小,全部被跳过,进程跟不上翻转(写不进去,不会写错地方)。全量读读到更小的 ModRevision 只可能是这种情况,计 `db_placement_rejected_total{reason="revision_regress"}`。用了 `--bump-revision`,revision 更大,回退的值走到版本判定,计 `version_regress`。
- 指标 `db_placement_version{logical_db}`:只要进程配了映射就上报 etcd 里每个有键的库,不等它连过(按需开池的进程没连过时 audit 不能报它落后);没有键的库连过才报 0。映射指向本进程不认识的集群时停在旧版本,`audit` 才看得出。
- 指标 `db_placement_known_cluster{cluster}`:进程 `SqlClusters` 里的每个集群,恒为 1。`cutover` 冻结之前抓,任何列出的进程缺目标集群就拒绝:配置改了但 Pod 用的还是旧 Secret 时,翻转之后这个库读写全停,还回不去。

`authoritysql`:

- `Open`、`OpenForMigration` 加选项 `WithPlacement(解析器)`;go/db 自己管连接池,用 `PinPlacedSession`。解析器接口 `PlacementResolver` 定义在这个包,这个包不碰 etcd。没有全局变量:解析器由各服务启动时显式传进打开连接池的地方。
- `Connect` 时按当前映射选集群、建连接,连接记下映射版本(版本在拨号之前读,拨号途中翻转时这条连接已算过期)。
- 版本落后时 `IsValid` 回 false(归还时不进连接池),`ResetSession` 回 `driver.ErrBadConn`(空闲连接取出时丢弃,database/sql 换一条重试)。这时一条语句都没发,不存在结局未知。
- 在途事务:连接还在旧集群,旧集群已冻结,闸门拒绝,RPC 回可重试,重试时拿到新集群的连接。
- 不经闸门的只读连接池在映射生效之前可能读到冻结时的数据,窗口是 etcd Watch 的延迟。

`-migrate` 和启动期的短连接池用 `dbplacement.ForMigration`、`ForOneShot`:读一次映射,不起 Watch;前者看到 `state=MOVING` 拒绝。schema Job 的配置来自运维预置的 Secret,漏了 `DbPlacement` 段就看不到 `MOVING`:staging、prod 的 schema Job 由 `k8s_deploy.ps1` 注入 `MMORPG_DB_PLACEMENT_REQUIRED=1`,这时没有这一段直接拒绝。go/db 的 `cmd/migrate` 不是 Job,手工跑时自己设。

### 8.2 迁移步骤(`tools/dbmove`)

子命令 `plan`、`prepare`、`verify`、`cutover`、`rollback`、`audit`、`cleanup`。每步幂等,可重跑,带 `-run-id`,状态写进映射值和本地日志。操作和每种故障的结局见[手册](../ops/db-move-runbook.md)。

1. `plan`:只读。核源、目标后端相同,目标上没有这个库,维护账号能控制源库闸门,写者账号(ACTIVE 加 `-roster` 名册里的)和维护账号在目标集群已存在,没有别的逻辑库在搬,默认集群和 etcd 记的一致。
2. `prepare`:写 `state=MOVING`,**在目标上把全部写者账号 `ACCOUNT LOCK`**,再建空库。全量加增量复制由运维按手册起(TiDB:BR 加 TiCDC;MySQL:GTID 复制)。
   - 为什么锁账号:闸门表是复制过去的,目标上 `writable=1`,翻转之前连到目标的写者能直接过闸,两边分叉。账号不随复制走,TiDB 和 MySQL 都成立。要写进映射(`Move.LockedAccounts`),cutover 按它解锁,rollback 按它再锁一遍。
   - `ACCOUNT LOCK` 按账号生效,不按库。账号还在写目标集群上别的库时(go/db、data_service 一个 boot 写全部存档库),锁了就把那些库一起断了。这种账号默认拒绝,`-allow-shared-writer-accounts` 确认之后不锁,记进 `Move.SharedAccounts`。没锁的账号只靠翻转之前查连接挡,Go 进程按映射拨号,翻转之前不会连过来。判据是 `SHOW GRANTS` 里有没有别的业务库。
3. `verify`:复制运行中用一致快照比校验和。TiDB 用 TiCDC syncpoint 给的一对 TSO 设 `tidb_snapshot`;MySQL 用 `CHECKSUM TABLE`,要读锁,所以在停在同一 GTID 的副本上做。成功时把位置的时刻和复制流的身份记进映射值:
   - **复制连续性**:流断过之后用较新的位置重建,中间一段写没复制,延迟回到 0,光看时间和延迟 cutover 全绿,目标缺数据就开放写入。所以记身份(`Move.VerifiedFeed`):TiDB 档是 changefeed id、`start_ts`、`create_time`,加 `primary_ts`;MySQL 档是通道、`source_uuid`、来源地址,且要求 `SOURCE_AUTO_POSITION=1`(按文件位置复制能跳事务);两档都带目标库 `route_meta` 的建表时间,删库重建之后身份就变了。cutover 在预检和冻结之后各比一次,预检时变了拒绝,窗口里变了或看到复制断开就回滚并清掉记录。
   - **etcd 回档**:映射只在 etcd 里,回档之后它说没翻转不算数。cutover、重跑 prepare、rollback 都先读目标上的翻转标记。标记没来得及写的情况,靠解冻之前核「目标闸门被这个 run 打开过且写者连着」,成立就不解冻,退出码 3。
   - TiDB 档要求 `primary_ts >= start_ts`(否则是同名旧 changefeed 留下的行),且物理时间不早于源库当前 TSO 往前 4 个 sync point 间隔。`VerifiedMs` 取 `primary_ts` 的物理时间,不取工具开始的时刻。
   - 工具看不出的:`--overwrite-checkpoint-ts`、手工注入 GTID 这类不换身份的跳过。手册 §3 明确禁止,做过就从全量重来。
   - **TiDB 不比 `ADMIN CHECKSUM TABLE` 的校验和本身**:它把 `table_id` 算进去,官方文档明说同样的数据、不同的 `table_id` 校验和不同,而 BR 和 TiCDC 建的表 `table_id` 都和源不同。比 `Total_kvs`(和 `table_id` 无关,能发现索引和行对不上)、行数,再加逐行摘要 `BIT_XOR(MD5(CONCAT_WS(...)))`(和 sync-diff-inspector 同一个做法)。
4. `cutover`。要求有一次足够新的 `verify` 成功记录(默认 6 小时内),否则拒绝。切换窗口里只做快操作:
   - 冻结之前:核复制流身份没变、延迟低于阈值,在回滚掉的事务里试一遍两侧闸门的权限;目标上重查全部账号(ACTIVE、`-roster` 名册、维护、注册、schema)存在,两侧 `SHOW GRANTS` 比这个库和全局的授权,缺一个就拒绝;抓 `-metrics-targets`,任何进程不认识目标集群就拒绝。不过就拒绝,什么都没变。
   - 源库 `FreezeWriters`。它拿到所有写者的 slot 行锁才推进 epoch,所以在途事务都已提交,之后的事务在闸门上被拒,业务收到可重试错误。
   - 在源库写哨兵行(`db_move_sentinel`,和闸门表一起预建,切换时不做 DDL)。
   - 等复制追上:TiCDC 的 checkpoint 不小于哨兵之后取的 TSO,MySQL 用 `WAIT_FOR_EXECUTED_GTID_SET`,而且目标能读到哨兵行。超过预算(默认 30 秒)自动回滚。
   - 把目标位置 CAS 写进映射(`Move.TargetPoint`)。窗口崩在打开目标闸门之后时,重跑复用它,不重读:重读的位置已经包含那次打开,audit 会报不一致。
   - 查目标 processlist(TiDB `cluster_processlist`,MySQL `performance_schema.threads`):锁定的账号连着就回滚,`ACCOUNT LOCK` 不踢已有连接。没锁的账号只看连到这个库的。
   - `ACCOUNT UNLOCK`,再打开目标库闸门(`ReleaseWriters`,闸门表是复制过去的冻结点)。解锁排在前面:解锁之后、闸门打开之前连上来的写者还是过不了闸。两步都幂等,重跑接着做。
   - etcd CAS 翻转映射。**这是提交点。**
   - 在目标库写翻转标记(`db_move_sentinel` 里按 run id 算的一行)。映射只在 etcd 里,etcd 回档之后 `rollback` 读到的是翻转之前的状态,解冻源库就分叉了。标记和目标的数据在一起,`rollback` 先读它。写不成回退出码 2,重跑 cutover 补上,翻转不受影响。
   - 各进程 Watch 到后丢弃旧连接,重试的请求落到新位置。源库保持冻结,旧进程写不进去。
   - 回滚的顺序不能反:**先写一次映射让还在路上的翻转失效,再把目标账号锁回去,再解冻源库**。解冻之后源库的闸门表复制过去,目标又是 `writable=1`。没记过目标位置就没解锁过,不碰目标:回滚常常就是因为目标连不上。翻转结局未知、etcd 又不通时绝不解冻,退出码 3,等 etcd 恢复后重跑。
5. `rollback`:翻转之前可用,先读目标上的翻转标记(读到就拒绝),把账号再锁一遍,作废映射,解冻源库。翻转之后拒绝,只能反向再搬一次。目标连不上时什么都核不了,默认拒绝,确认它没收过写入之后加 `-target-lost`。
6. `audit`:翻转后对冻结的源库和目标库跑全量校验和(目标站在翻转之前记下的位置上),核各进程上报的映射版本。不一致退出码非 0。`route_meta`、`writer_slot` 单独比,允许目标正好多一次 release;`db_move_sentinel` 不比,窗口重跑时源库多一行新哨兵,和业务数据无关。
7. `cleanup`:保留期(默认 7 天)过后才允许删源库,必须显式 `-confirm-drop`,要求 `audit` 成功过,删之前核闸门确实是这个 run 冻结的。

玩家感知:存盘在 scene 内存里按原序号退避重试,不丢;RPC 回可重试错误。窗口预期是秒级,压测确认。

### 8.3 约束

- 写者账号要在目标集群提前建好,闸门表随复制过去,boot 授权继续有效。
- 迁移中 DDL 禁止,`-migrate` 看到 `state=MOVING` 拒绝。
- 不过闸门的工具在迁移期间禁用,手册 §2 有清单。`admission_recover` 同事务锁 `route_meta` 判 `writable`,冻结就拒绝,而且必须配 `MMORPG_DB_PLACEMENT`。
- 一次只搬一个逻辑库。`plan`、`prepare` 看到别的库在搬就拒绝。
- Java 进程(网关的 `mmorpg` 库)不做动态拨号,翻转后滚动重启,手册 §9。`java/authority_jdbc` 把闸门冻结映射成可重试的 `WriterFrozenException`(`SQLTransientException`,SQLSTATE 40001)。

## 9. 多地域

每个地域一整套:目录、业务库、存档库、Redis、Kafka、两套 etcd。集群不跨洋,30 毫秒限制不适用。

- **ID**:每个地域配不同的 `IdSegment.Issuer.Region`(部署参数 `-Region`,生产和 staging 用 1~63,0 只给 dev、test,上线后不能改)。2026-11-05 起地域码由全局号源登记处把关(部署参数 `-IdRegistryEtcd`,[总体方案](./player-data-tidb-scaling-plan.md) §6.3):一个码只绑一对本地 etcd、一生只认领一次,重复部署、克隆快照、复用退役码在发号前被拒绝。登记处是全世界共用的一套小 etcd,这是「每个地域一整套」唯一的例外:跨环境的唯一性只能靠共用的仲裁者。它只在预留新块时访问,不可达时各地域手里的块照常发完。
- **账号**:每个地域一份角色列表,和现在一个账号多区多角色同一个模型。**不做全局账号目录**:它要跨地域强一致写,只为了「提示你在别的地域有角色」,不值。客户端的地域列表由网关配置给出。
- **名字**:按地域唯一。公会、好友、聊天不跨地域。
- **同地域**所有 zone 互打不变。
- **跨地域对战**:没有确定玩法之前不写。要做时走独立战场单元:带战斗快照进去,结果经各自地域的收件箱幂等落地,不跨地域写存档。

## 10. 四项决定(2026-10-31 定)

| 项 | 决定 |
|---|---|
| 第二套 etcd | **强规定**,写进 AGENTS §7。配置比地址,运行期比 etcd 集群号,`AllowAutoSeed` 只在 `Mode=dev/test` 生效 |
| ID 低 63 位 | 保持。类型仍是 64 位 |
| mysql 档 3 个 Kafka broker | 保持,不放开 RF3 门禁 |
| go/db 选库 | 见 §7。物理位置由闸门在同一事务里拦住 |

## 11. 落码批次

每批由 Codex 验证,没有运行证据前都写「未编译」。

| 批 | 内容 | 状态 |
|---|---|---|
| F1 | `shared/rolefence`、proto、data_service 协调者、`fence_version`、删跨库守卫 | 已写完,未编译,待 Codex 验证。过渡状态见下 |
| F2 | guild、match 接护栏,§4 占用仲裁 | 已写完,未编译,待 Codex 验证。过渡状态见下 |
| F3 | trade、mail、friend、player_inbox 接护栏,chat 改 RPC | 已写完,未编译,待 Codex 验证。过渡状态见下 |
| D1 | 目录分片 §5,存档分步 §6,go/db 选库 §7,流水库 §1 | 已写完,未编译,待 Codex 验证。部署渲染归 K1,见下 |
| G1 | 闸门每库一份 §2,工具链 | 已写完,未编译,待 Codex 验证。过渡状态见下 |
| M1 | `shared/dbplacement`、连接层按映射拨号、`tools/dbmove`、手册 §8 | 已写完,未编译,待 Codex 验证。部署渲染(`DbPlacement` 段、`MMORPG_DB_PLACEMENT`)归 K1 |
| K1 | 部署:配置渲染、密钥、`-Region`、契约测试、跨库契约测试 §1 | 已写完,未编译,待 Codex 验证。闸门冻结时长的指标和告警由 FX3 补上 |
| FX1 | 一轮独立评审的修复:准备令牌和最终冻结分开、试探、准备时限、确定性结局不打成 HOLD、Cancel 不依赖存档库、占用多占、单库回档手册 | 已写完,未编译,待 Codex 验证 |
| FX2 | 一轮独立评审的修复:判陈旧先读 listing 后读真相,过户第 3 步失败回 `Unavailable`,写 ref 的两个窗口,建角确定失败 `ErrNotCommitted`,`claim_seq`,展示读跳过坏落点,排空证明按状态码判 | 已写完,未编译,待 Codex 验证 |
| FX3 | 一轮独立评审的修复:复制流身份、目标账号锁定、REGISTER 不绑 epoch、`IsWriterFrozen` 和后台循环不打 HOLD、账号授权和目标集群预检、`admission_recover` 守闸门、启动路径扛冻结、`writer_frozen_seconds`、audit 复用目标位置、翻转标记、默认集群持久化、admin 连接串核对 | 已写完,未编译,待 Codex 验证。serverbase 的排空判定不在这批 |
| R1 | 评审,文档,验证清单 | 已完成。三路评审 26 条全部修复,排空判定的最后一处(闸门冻结确定没写不 Hold、`ResourceExhausted`)在主会话改完。修复本身没有再评审 |

G1 落地后、F1~D1 落地前的过渡状态(D1 已写完,这段结束):闸门已按库拆开,但跨库 SQL 还在。data_service 的建角初始行和判死还在目录事务里写存档库,这些写只受目录库闸门保护,不受落点库闸门保护;go/db 选库还跨库读 `player_route`,库名来自配置 `Placement.RouteSchema`。这些都要求全部库还在一个集群,所以 F1~D1 落地之前不能真的搬库。

F1 落地后、F2、F3 落地前的过渡状态:协调者已经两阶段,挂起判据已从目录删掉,参与者还没注册 `RoleFenceParticipant`。调它们回 `Unimplemented`,算不可达,所以 `Begin`、`Remove` 回 `ErrUnavailable`,fail-closed。各服务的提交点还锁 `account_role`,不看副本。这个中间状态不能部署。部署配置(`RoleFence.Participants`、`MMORPG_ROLE_FENCE_SECRET_*`)和 `k8s_deploy.ps1` 里还提 `ConfigureAssetContracts` 的注释归 K1。

F3 落地后的过渡状态:trade、mail、friend、player_inbox 的提交点已锁本库 `role_fence`,也注册了 `RoleFenceParticipant`;chat 读 `GetRoleFenceState`。`placement.CheckRoleRoute`、`CheckRoleRouteContract` 已删。`LockRoleRoutes` 已随 D1 删除。go 代码已不再读 `AccountRoleSchema`。

F2 落地后的过渡状态:guild、match 的提交点已锁本库 `role_fence`,注册了 `RoleFenceParticipant`,服务之间不再有跨库 SQL。`AccountRoleSchema`、`MatchAuthoritySchema`、`GuildTrialSchema` 已从两边配置删除。

K1 落地前这些服务起不来,这是有意的,缺依赖不能跑。K1 要补:

- 密钥:`MMORPG_ROLE_FENCE_SECRET_{TRADE,MAIL,FRIEND,PLAYER_INBOX,CHAT,GUILD,MATCH}`,缺了拒启。除 chat 都是参与者,还要 `MMORPG_ROLE_FENCE_SECRET_COORDINATOR`。
- `MMORPG_MATCH_INTERNAL_SECRET_GUILD` 不再只给取消用:开局、占用、读终局都验它,guild 和 match 两边都要配,配了 `MatchRpc` 没配它 guild 拒启。
- 脚本还认 F2 删掉的键:`k8s_deploy.ps1` 按键读 `MatchAuthoritySchema`,读不到会抛错;`go_services.ps1`、`lib/sql_boot_catalog_local.ps1`、`prepare_docker_debug.py` 及其用例同类。`sql_boot_catalog_local.ps1` 给 guild 的 `mmorpg_match` 跨库授权要删。
- 配置渲染:mail 的 `GuildRpc`,chat 的 `DataServiceRpc`。mail 把 `GuildRpc` 配成 `NonBlock`,guild 没起时邮件照常启动。
- 脚本和用例还认 `AccountRoleSchema`。多渲染一个键 go-zero 会忽略,但 `tools/scripts/go_services.ps1` 的 `Get-LocalChatAuthoritySettings` 按键读 `go/chat/etc/chat.yaml`,读不到会抛错,本地起 chat 要等它改。同类的还有 `k8s_deploy.ps1`、`prepare_docker_debug.py`、`lib/sql_boot_catalog_local.ps1`,以及 `tools/scripts/tests` 下七个用例。`chat_authority_deploy.tests.ps1` 里的授权断言已随 `chat_runtime_role.sql` 改好,渲染断言还没改。

D1 落地后 K1 还要补:

- 建库:片库 `<base>_NN`(K=1 时是 `<base>`)、`<base>_ledger`,`-migrate` 之前建好,要是空库。
- 写者启动目录:data_service 的 boot 名册 = 片库 + 流水库 + 全部 `AllowedStorageIDs`,多一份少一份都拒启。授权:新表 `account_role_ref`、`player_name_owner`、`directory_shard`;存档库写 `player_store`,读 `player_centre_database`、`user_accounts`(空路由启动判据)。
- 配置:`Directory.LogicalShards`(生产 64,开发 1)、`Directory.StoragePools`。连接总账按片算:`AccountDirectory`、`PlayerName`、`RouteStore` 的池都是每片一个,`K=64` 时要按新总账调小每片上限;`Get-MysqlConnectionLedger` 要改。
- go/db:`DataServiceRpc`,data_service 和 go/db 两边的 `MMORPG_ROLE_FENCE_SECRET_DB`。删 `k8s_deploy.ps1`、`lib/sql_boot_catalog_local.ps1` 里的 `Placement.RouteSchema` 及其 `player_route` 授权,以及 `tools/scripts/tests` 里的断言。
- 排空和 reset 脚本逐库:`maintenance_drain.ps1 render-roles` 目录有 K+1 份计划;本地 reset 清单加 `<base>_ledger` 和各片。
- 工具参数:`debug_fetch`、`debug_import`、`admission_recover` 的 `-logical-shards`,`data_consistency_check` 的 `-route-logical-shards`,`merge_zone` 的 `-route-db-dsn` 指第 0 片,`dev_tools.ps1` 的包装跟着改。

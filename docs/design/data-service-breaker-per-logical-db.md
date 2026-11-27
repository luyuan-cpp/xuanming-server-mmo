# data_service 的熔断:服务端从按方法改成按逻辑库,调用方关掉按方法的客户端熔断

> **状态(2026-11-20,Antigravity)**:已获负责人授权并**已按 §6 落码,未编译、未运行**(AGENTS.md §10.3),验证命令见 `docs/PROGRESS.md` 2026-11-20 条目。2026-11-26 复核:全部在 main,11-24 的 Docker 重建编过;同日把「调用方取消」的结算从记成功改成成功失败都不记(§5.1、§8),这一处连同全部单测待 Gemini 跑,命令见 PROGRESS 2026-11-26「data_service 逻辑库熔断:调用方取消不再记成成功」。
> **来源**:2026-11-20 在 go/db 上确认并修掉同一类问题(`docs/design/db-task-store-unavailable-deferral.md` 文首「2026-11-20 补」),当时记下 data_service 另开任务。
> **证据口径**:设计与实现基于静态阅读——go-zero v1.10.0 源码与本仓代码,已做 `gofmt` 与英文日志静态检查;编译、单测交 Gemini。
> **Java 版(AGENTS §12)**:不涉及(Java 版没有 data_service 与 go-zero),不加 PARITY 行。

## 0. 结论

1. **问题属实,而且有两层。** data_service 用 `zrpc.MustNewServer` 建服务,go-zero 的服务端熔断缺省开、键是 gRPC 方法名;各 Go 调用方的 zrpc 客户端熔断缺省开、键是「目标 + 方法」。两层都不分后端。目录按账号 / 玩家 / 名字哈希分成 K 个逻辑库,存档库按 `storage_id` 各一个,它们可以经 `tools/dbmove` 落在不同集群。一个集群失联、而落在它上面的流量占某个方法的三分之一以上时,这两层都会把同一方法上指向健康库的调用也挡掉(§1.3)。
2. **服务端**:关掉 zrpc 按方法的服务端熔断(代码里写死);在连接池建物理连接的地方给每个逻辑库一个 go-zero 熔断。规则只有一处:`shared/authoritysql` 新增一个可选的建连钩子,data_service 装配连接池时挂上;八十多处选池的 store 代码一行不改(§5.1)。
3. **调用方**:建 data_service 客户端时一律关掉 zrpc 的客户端熔断,规则收在 `shared/dsclient` 一个函数里,14 个构造点各改一行,yaml 写开也不认(§5.2)。「保留客户端熔断、让被熔断的分片回一个不计失败的码」做不到隔离,原因在 §4.2。
4. **数据一致性不受影响。** 这次改的只是「一次调用要不要被提前挡回」。被逻辑库熔断挡回的那一次没有建连,一条语句都没发;调用方拿到的仍是原来那几种 gRPC 码,处理路径不变(§3.2、§5.4)。
5. **不加的东西**:发号器的 etcd 不加熔断(它自己有失败退避);不加新指标;不改任何 RPC 的错误码(§5.3)。

## 1. 框架这一层怎么工作(go-zero v1.10.0)

### 1.1 服务端

`zrpc/server.go` 的 `setupUnaryInterceptors` 按固定顺序装内置拦截器(先装的在外层):Trace → Recover → Stat → Prometheus → **Breaker** → Shedding(`CpuThreshold`,缺省 900)→ **Timeout**(缺省 2000ms,`MethodTimeouts` 可按方法改)。`AddUnaryInterceptors` 加的业务拦截器在它们里面。

- 熔断的键是 `info.FullMethod`(`zrpc/internal/serverinterceptors/breakerinterceptor.go`)。
- 计成失败的结局(`serverSideAcceptable` + `zrpc/internal/codes/accept.go`):gRPC 码 `DeadlineExceeded`、`Internal`、`Unavailable`、`DataLoss`、`Unimplemented`、`ResourceExhausted`,以及 `context.DeadlineExceeded`、handler panic。其余(含 `Aborted`、`FailedPrecondition`、`NotFound`、`InvalidArgument`、`PermissionDenied`、`Canceled`、`Unknown`)算成功。
- Timeout 拦截器在熔断里面:handler 没在时限内返回,它直接回 `DeadlineExceeded`,这一次被熔断计成失败,不管 handler 之后回什么。调用方自己的截止时间经 `grpc-timeout` 传过来,也走这条路。
- 被熔断丢掉的调用回 `Unavailable`「circuit breaker is open」(`convertError`)。

data_service 的 yaml 与 K8s 的 ConfigMap 都没写 `Timeout`,即 2000ms;GM 回收的三个方法有 `MethodTimeouts`(60s / 10s / 30s)。

### 1.2 客户端

`zrpc/internal/client.go` 的 `buildUnaryInterceptors`:Trace → Duration → Prometheus → **Breaker** → **Timeout**。

- 熔断的键是 `path.Join(cc.Target(), method)`:一个调用方进程里,一个目标的一个方法一个熔断,不分对端副本,更不分分片。
- 计失败的码同上(`codes.Acceptable`)。Timeout 拦截器在熔断里面,所以**调用方自己等超时**的那一次(本地生成的 `DeadlineExceeded`)也计失败。
- 被丢掉的调用拿到的是 `breaker.ErrServiceUnavailable` 本身,不是 gRPC status:`status.Code(err)` 是 `Unknown`,`status.FromError` 的 `ok` 为假。
- 开关是 `RpcClientConf.Middlewares.Breaker`(json 缺省 `true`)。在代码里用结构体字面量构造 `RpcClientConf` 时拿不到 json 缺省值,五个中间件全是关的(go/friend 现在就是这样)。

### 1.3 算法与放大条件

`core/breaker/googlebreaker.go`(Google SRE 的客户端自适应限流):10 秒滑动窗口、40 个桶;

```
dropRatio = (total − 5 − 1.5 × accepts) / (total + 1)
```

`total` 含被丢掉的调用。`dropRatio > 0` 时按它的概率丢,每秒强制放行一次。

这个算法假定「被限流得越多,后端越轻松,成功率越高」。分片后端不满足这个假定:落在失联集群上的那部分调用必然失败,失败占比 f 是流量结构决定的常数。设放行比例为 p,一个窗口里 `accepts ≈ p(1−f)·total`,下一个窗口的放行比例是 `1.5·p·(1−f)`:

- f < 1/3:`1.5(1−f) > 1`,不丢。
- f > 1/3:放行比例每个窗口乘以一个小于 1 的数,衰减到只剩每秒强制放行的一次。**同一方法上指向健康库的调用和指向坏库的调用被同样的概率丢掉。**

f 超过三分之一并不难:目录 64 片放在两个集群上,一个集群失联就是一半;存档库不超过 3 个时任何一个都是。而且登录的 `ReadAccountDirectory` 一次要读账号片和这个账号每个角色的玩家片(§2.2),一半的片失联时它的失败占比远高于一半。

还有两个与分片无关、同样会被算进去的来源(现状):

- 业务拒绝被当成失败:账号角色数到上限回 `ResourceExhausted`(`directoryError` 的 `ErrFull`);`RegisterPlayerZone` 的参数非法有意回 `Unavailable` + 码 14。
- 一行热锁、一片连接池排满造成的超时:只有那一个账号、那一片慢,却按方法计。

### 1.4 两层叠在一起

服务端按方法的熔断挡回的调用回 `Unavailable`,调用方的客户端熔断又把它计成失败。所以即使只修服务端,调用方那一层仍会因为坏分片的失败(`Unavailable`,或者调用方等到自己的截止时间)把健康分片的调用挡掉。两层要一起处置。

## 2. data_service 的后端与各 RPC

### 2.1 后端

| 后端 | 连接池 | 说明 |
|---|---|---|
| 目录逻辑库 `<DBName>` 或 `<DBName>_00..`(K 个,生产 64) | 每片三个池:账号目录(`AccountDirectoryStore.shards`)、名字(`PlayerNameStore.pools`)、路由(`RouteStore.pools`,RC + 锁等待 1s) | `internal/store/directory_shards.go`;按账号 / 玩家 / 名字哈希选片,事务只开在一个逻辑库上,跨片分步 |
| 流水库 `<DBName>_ledger`(1 个) | 快照、流水、回收各一个池 | `player_snapshot`、`transaction_log`、回收批次 |
| 存档库 `player_store_<storage_id>_db`(每个 `storage_id` 一个) | `PlayerStorePools`,第一次用到才打开 | `internal/store/player_store_pools.go`;打开失败有 10 秒冷却 |
| 发号器:两套本地 etcd + 全局登记处 | — | `internal/idissuer`;手里的块没用完时不碰 etcd |
| 控制集群 Redis(名字读缓存、回收唤醒)、玩家域 Redis(位置键) | — | 只放可丢弃的读缓存与协调 |
| 角色护栏的六个参与者(guild、trade、mail、match、player_inbox、friend) | zrpc 客户端 | 过户、删角之前由协调者调用 |
| Kafka(流水、快照两个消费者) | — | 后台,不在 RPC 路径上 |

所有 SQL 连接池都经 `authoritysql.Open` 建,建连超时 1 秒、读写超时 5 秒(`shared/authoritysql/pool.go`);按映射拨号时每条新连接读一次 `dbplacement` 的本地缓存决定连哪个集群。`tools/dbmove` 搬的单位是逻辑库,所以**能各自失联的最小单位是逻辑库**,同一逻辑库上的几个池同进退。

### 2.2 各 RPC

「键」是选片依据。「存储失败」指选到的库读写不成(连不上、超时、语句失败);表里只列它,鉴权失败一律 `PermissionDenied`、对应 store 没装配一律 `Unavailable`,不逐行重复。

| RPC | 碰的后端 | 存储失败时回什么 | 转码位置 |
|---|---|---|---|
| `ReadAccountDirectory` | 账号片;再按角色读各自的玩家片(最多 `MaxPlayersPerAccount` 片) | `Unavailable` | `server/account_directory.go` `directoryError` |
| `CreateAccountRole` | 账号片、玩家片、存档库(初始行)、名字片(绑定) | 提交点之前确定没写回 `Aborted`,其余 `Unavailable` | 同上 |
| `LookupAccountRole` | 玩家片;`include_account_state` 时再读账号片 | `Unavailable` | 同上 |
| `RepairAccountRole` | 账号片、玩家片 | `Unavailable` | 同上 |
| `RemoveAccountRoles` | 账号片、各角色的玩家片、名字片、存档库;之前经协调者调六个参与者 | `Unavailable`;投票问不齐、收尾没做完回 `Aborted` | 同上 |
| `SetAccountRealName`、`ResetAccountRealName` | 账号片 | `Unavailable` | 同上 |
| `GetAccountRealName` | 玩家片(归属)→ 账号片 | `Unavailable` | 同上 |
| `BeginAccountRoleTrade`、`CancelAccountRoleTrade`、`TransferAccountRole` | 玩家片、买卖双方的账号片、六个参与者 | `Unavailable` / `Aborted` | `server/account_role_trade.go` → `directoryError` |
| `ConfirmAccountRoleTradeFreeze` | 同上,另读存档库的冻结证据 | 同上 | 同上 |
| `CheckAccountRoleSlot` | 账号片 | 带内 `retry=true`,gRPC OK | `server/account_role_trade.go` |
| `GetRoleFenceState`、`VerifyRoleAdmission` | 玩家片 | 带内 `retry=true`,gRPC OK | `server/role_fence.go` |
| `RegisterRoleAdmission`、`CloseRoleAdmissions`、`AckRoleTradeDrain` | 玩家片 | `Unavailable` | `server/account_role_admission.go` → `directoryError` |
| `RequestAccountDeletion`、`CancelAccountDeletion`、`FinishAccountDeletion` | 账号片(收尾时另清各玩家片的陈旧索引) | `Unavailable` | `server/account_deletion.go` → `directoryError` |
| `ClaimAccountDeletions` | 全部 K 片逐片扫 | 一片失败不影响别的片;一条都没认领到才回 `Unavailable` | 同上 |
| `ReadPlayerStoreHolder`、`ReleaseDeadPlayerStoreHolder` | 玩家片(读路由)→ 存档库 | `Unavailable`;读之后有人认领过回 `FailedPrecondition` | `server/account_directory.go` |
| `RegisterPlayerZone` | 玩家片 | `Unavailable` + `error_code=30`;参数非法也是 `Unavailable` + 码 14;已有不同归属回 `AlreadyExists` | `server/dataserviceserver.go` |
| `GetPlayerHomeZone` | 玩家片 + 进程内合服缓存 | `Unavailable` + 码 30;没有映射 `NotFound`;调用方已放弃回它的取消原因 | 同上 |
| `BatchGetPlayerHomeZone` | 多个玩家片 | 任一片失败整批 `Unavailable` | 同上 |
| `GetMergedZones` | 进程内缓存(定时从第 0 片重读,失败保留旧值) | 不读库 | 同上 |
| `CountZoneRoles` | 全部 K 片 | 任一片失败 `Unavailable` | 同上 |
| `GetPlayerRoute` | 玩家片 | 带内 `retry=true`,gRPC OK | 同上 |
| `ReservePlayerName`、`ReleasePlayerName` | 名字片、玩家片(名字归属行);释放前在玩家片证明角色不会提交 | `Unavailable` + 名字库故障码 | `playerNameStatus` |
| `BatchGetPlayerName` | Redis 读缓存 → 多个玩家片 | 任一片失败整批 `Unavailable` | 同上 |
| `GetPlayerAssetOpLedger`、`GetPlayerInboxLedger`、`GetPlayerItemGainBlock` | 玩家片(读路由)→ 存档库 | `Unavailable`;调用方已放弃回它的取消原因 | `logic/persisted_player_database.go` |
| `BatchGetPlayerDisplay` | 多个玩家片 → 多个存档库 | 玩家片失败整批 `Unavailable`;存档库失败只跳过那个库的玩家 | `server/account_directory.go` |
| `ListPlayerSnapshots`、`QueryTransactionLog` | 流水库 | 带内 `error_code=11`,gRPC OK | `server/dataserviceserver.go` |
| `BatchRecallItems`、`GetRecallBatch`、`CancelRecallBatch`、`ResolveRecallOp`、`RegisterRecallReviewHandled` | 流水库、本进程发号器、控制集群 Redis | 带内 `error_code`,gRPC OK | 同上 |
| `AllocateIdSegment` | 两套本地 etcd + 登记处(只在预留新块时) | 带内 `error_code=18`,gRPC OK | `logic/id_segment_logic.go` |

三点结论:

- **多数 RPC 一次要碰不止一个逻辑库**(账号片 + 玩家片 + 名字片 + 存档库)。所以熔断不能放在 RPC 入口按请求里的一个键选:入口不知道失败的是哪一个库。
- 带内回错的那些 RPC(gRPC 恒 OK)在快速失败时不被熔断计入,但它们等满服务端 2 秒时限时照样被 Timeout 拦截器计成失败。
- 回 `Unavailable` 的那些,存储失败每一次都计入。

## 3. 调用方

### 3.1 构造点与现在的开关

`dspb.NewDataServiceClient` 的非测试调用点一共 14 个:

| 服务 | 构造点 | 本地 yaml 的 `DataServiceRpc.Middlewares.Breaker` | 用到的 RPC |
|---|---|---|---|
| login | `internal/svc/home_zone.go`、`internal/svc/player_id_minter.go` | **true**(注释写了理由,见下) | 账号目录全套、实名、注销、`RegisterPlayerZone`、归属读、`CountZoneRoles`、名字三个、`GetPlayerAssetOpLedger`、`AllocateIdSegment` |
| scene_manager | `internal/svc/servicecontext.go` | **true** | `GetPlayerHomeZone`、`LookupAccountRole`、`ReadPlayerStoreHolder`、`ReleaseDeadPlayerStoreHolder` |
| db | `player_store_wiring.go` | 没写(缺省 true) | `GetPlayerRoute` |
| payment | `channels.go` | 没写(缺省 true) | `GetAccountRealName` |
| chat | `internal/svc/authority.go` | 没写(缺省 true) | `GetRoleFenceState` |
| mail、rank | `shared/domainhost/dependencies.go` | 没写(缺省 true) | `AllocateIdSegment`;mail 另有护栏读 |
| guild | `internal/svc/guild_id_minter.go`、`cmd/assetopfix/main.go` | false | 归属读、合服映射、名字、号段、账本、护栏读 |
| trade | `internal/svc/servicecontext.go`、`cmd/assetopfix/main.go` | false | 归属读、合服映射、号段、角色交易、账本、护栏读 |
| match | `internal/svc/servicecontext.go` | false | 归属读、名字、展示资料、合服映射、号段、护栏读 |
| player_inbox | `internal/svc/servicecontext.go` | false | `GetPlayerHomeZone`、两本账本读、护栏读 |
| friend | `internal/svc/servicecontext.go` | 代码里用字面量构造,已经是关 | 归属读、名字、展示资料、护栏读 |

K8s 这边 `tools/scripts/k8s_deploy.ps1` 里显式写了的:login true,scene_manager、match、trade、player_inbox false。**scene_manager 本地 yaml 是 true、K8s 是 false,两处已经不一致。** 其余服务的 ConfigMap 没有显式的 `Breaker` 行,本稿没有逐个核它们是镜像本地 yaml 还是取缺省。

C++ scene 也调 data_service(`AllocateIdSegment`、入场登记三个),走 muduo_grpc,没有客户端熔断。

现有的两种理由:

- login、scene_manager 开着:data_service 一个副本「僵住但 etcd 键还在」时,让调用快速失败,不让每次登录、每次进场白等一个超时(`go/login/etc/login.yaml`、`go/scene_manager/etc/scene_manager_service.yaml` 的注释)。
- guild 关着:这条链路不重试,一次失败就整体拒绝,熔断帮不上忙,只会把一次抖动在日志里变成 breaker 错误(`docs/design/guild-phase2/07-rollback-fail-closed.md`)。

### 3.2 拿到错误之后怎么处理

逐个读过,**没有只认 `codes.Unavailable` 的分支**。认的都是「确定」的那几个码,其余一律按不可用 / 结局未知:

| 调用点 | 认的码 | 其余错误(`Unavailable`、`DeadlineExceeded`、`Unknown`、客户端熔断的错误…) |
|---|---|---|
| `shared/accountdirectory` 的 `classify`(login、trade 的目录与交易客户端) | `NotFound`、`AlreadyExists` / `FailedPrecondition`、`Aborted`、`ResourceExhausted`、`InvalidArgument`;建角的 `Aborted` 是确定没提交 | `ErrUnavailable`:结局未知,调用方按 `player_id` 强读消歧后重放 |
| `shared/accountdirectory` 的 `RouteClient`(go/db) | 带内 `found` / `retry` | `ErrUnavailable`:存盘接口回 `Unavailable`,scene 按原序号重试 |
| `shared/rolefence` 的状态读、入场校验(六个参与者、chat) | 带内 `found` / `retry` / `valid` | `ErrUnavailable`:这一笔确定没写,可重试 |
| `shared/assetop` 的 `DataServiceLedger`(guild、trade、mail 的重投) | 带内 `found` | 原样返回,重投循环计 error、继续退避,不终结 |
| `shared/idsegment` 的 `Adapt`(各服务领号段) | 带内 `error_code` | 原样返回,领段失败,调用方后台重试 |
| `shared/mergedzone`(guild、trade、match 启动与定时重读) | — | 启动时在预算内重试,之后保留旧值 |
| login `homezone`、guild / player_inbox / scene_manager 的归属读 | `NotFound`(以及旧文案)= 没有映射 | 故障:读路径保留存量值或拒绝本次请求,写路径建角失败 |
| login `playernamereg` | `FailedPrecondition` = 确定没写 | 结果未知,按名字登记的补偿流程处理 |
| login `GetZoneRoleCount` | — | 回 `Unavailable` 给网关,下一拍再问 |
| login 摘孤儿(`remove_players_from_accounts.go`) | `NotFound` | 回可重试 |

所以:客户端熔断关掉之后调用方看到的错误种类只会变少(不再有 breaker 自己的那个非 status 错误),不需要改任何一处错误处理。

## 4. 方案比较

### 4.1 服务端:按方法的熔断换成什么

| 方案 | 做法 | 结论 |
|---|---|---|
| S0 保持按方法 | — | 就是要修的问题 |
| S1 只关掉,不加别的 | `Middlewares.Breaker = false` | 隔离已经成立(每个逻辑库本来就各有连接池)。坏库上的调用每次自己等到建连超时(1 秒)或 RPC 时限(2 秒)才失败,调用方陪着等 |
| S2 在 RPC 入口按请求的键选一个熔断 | 一个拦截器,方法 → 取键函数 | 不可行:多数 RPC 一次碰几个逻辑库(§2.2),批量读跨很多片,入口不知道失败的是哪个库 |
| S3 在每个 store 方法里包一层(go/db 的 `enterStore` 做法) | 选池处 `Allow`,结束时结算 | go/db 一个 RPC 只碰一个库、只有两个入口,所以合适。这里选池的地方有八十多处、分在十几个 store 文件里,等于每个 store 方法各写一遍 |
| S4 在 SQL 守卫的语句层做 | 改 `authoritysql` 的 `guardedConn`,按每条语句的结局计 | 覆盖最全,但要在所有 Go 服务共用的守卫里加状态和错误分类。守卫有意不保留底层错误;go/db 那一轮已经否掉过这个方向(同上文档第 3 点) |
| **S5 在建物理连接的地方做** | `authoritysql` 加一个可选钩子,包住一次建连(拨号 + 握手 + 会话核对);data_service 给每个逻辑库一个 go-zero 熔断 | **选它**。一处规则,store 代码不动;守卫只多一个缺省为空的钩子,别的服务不传就和现在一样 |

S5 为什么够用:

- **集群不可达**(拒绝连接、黑洞):池里旧连接用完之后,每个请求都要新建连接,建连失败被计入;熔断打开后直接挡回,不再各等 1 秒。
- **TiDB 进程在、存储(TiKV / PD)不可用**:语句等到时限,驱动把超时的连接关掉;下一个请求新建连接时,写者池的会话核对要读 `route_meta` 与本 boot 的注册行,同样读不出来,建连失败被计入。
- **闸门冻结**(搬库窗口)不在建连阶段判:它是每笔事务 `BEGIN` 前后、事务外读的读闸门上判的,报错时连接是好的。所以冻结天然不计入,不需要按错误类型去排除。
- **业务错误**(唯一键冲突、行不存在、锁等待)都发生在已经建好的连接上,不计入。
- **调用方自己取消**时正在建的那条连接:结算时看 `ctx.Err()`,是 `Canceled` 就成功和失败都不记(2026-11-26 改,原来记成成功,原因见 §8)。

它不覆盖的:连接都还在、语句却持续失败而又不超时的情形(例如服务端对每条语句立刻回同一个错误)。这时没有快速失败,每次调用照常拿到那个错误;隔离不受影响。记在 §8。

### 4.2 调用方:客户端熔断怎么处置

| 方案 | 做法 | data_service 整个不可达时靠什么 | 结论 |
|---|---|---|---|
| C0 保留 | — | 熔断 | 就是要修的问题的另一半 |
| C1 保留,让服务端对「这个库被熔断 / 这个库失败」回一个不计失败的码 | 把这类失败从 `Unavailable` 换成 `codes.Acceptable` 放过的码 | 熔断 | 不成立,见下 |
| C2 保留,换成认「服务端答过没有」的拦截器 | 服务端在回包里带标记,调用方自己写一个熔断拦截器,带标记的错误不计 | 熔断 | 不成立,原因同 C1 的第二条;还要多一套自己的拦截器 |
| C3 让客户端熔断也按分片 | 调用方在发请求前算出它落在哪个库 | 熔断 | 做不到:调用方不知道 K、不知道玩家在哪个存档库,多数 RPC 也不止一个库 |
| **C4 关掉** | 建客户端时 `Middlewares.Breaker = false`,规则收在共享的一个函数里 | 见下 | **选它** |

C1、C2 为什么不成立:

1. **没有合适的码。** `codes.Acceptable` 放过的只有 `Canceled`、`Unknown`、`InvalidArgument`、`NotFound`、`AlreadyExists`、`PermissionDenied`、`FailedPrecondition`、`Aborted`、`OutOfRange`、`Unauthenticated`。「后端失联、结局可能未知」不属于其中任何一个;调用方按码分支的地方(§3.2)恰好把这些码中的多数当成确定结论——比如换成 `Aborted`,login 会把一次结局未知的建角当成确定没提交去释放名字。
2. **调用方自己等超时的那些次不经过服务端的码。** 坏库上的调用在服务端熔断打开之前、以及每秒被强制放行的那一次,要等建连超时(1 秒)才有回包;调用方的截止时间常常更短(guild 读账本 300ms,login 角色列表 500ms,建角登记 800ms)。这些次在调用方本地变成 `DeadlineExceeded`,计入客户端熔断,服务端回什么码、带不带标记都管不到。每个逻辑库各自的流量不大时(64 片摊开之后),服务端的熔断可能根本攒不够样本打开,坏库上的每一次调用都是这样超时的。也就是说:保留客户端熔断,隔离就取决于流量大小和各处超时的相对长短,而不是由结构保证。

C4 之后调用方还靠什么:

- **data_service 全部副本不在**(进程退出、Pod 没了):etcd 键随租约摘掉,没有可用地址或子连接全部处于 `TRANSIENT_FAILURE`,gRPC 对非 wait-for-ready 的调用立刻回 `Unavailable`。这一条本来就不靠熔断。
- **每次调用都有截止时间**:zrpc 客户端的 Timeout 拦截器不受这次改动影响(各服务 yaml 的 `DataServiceRpc.Timeout`,1500~3000ms),各封装还套了更短的单次上限(300ms~2.5s)。
- **一个副本僵住但键还在**(login 的 yaml 里写的那种情形):窗口不超过 go-zero 注册租约的 10 秒(`core/discov/clients.go` 的 `TimeToLive`)。期间 `p2c_ewma` 按延迟与在途数选副本,僵住的那个在途数堆高之后基本不再被选中,只有每秒一次的强制探测落到它上面。按「目标 + 方法」的熔断在这里帮的是倒忙:两个副本坏一个时失败占比过三分之一,它把发往好副本的调用也挡掉。
- **data_service 过载**:服务端的自适应降载(`CpuThreshold`)照旧,回 `ResourceExhausted`。
- **代价(明确接受)**:data_service 整体「活着、租约也在、但就是慢」而且不是 CPU 打满时,调用方每次等到自己的截止时间,不再提前快速失败。这是 guild、trade、match、player_inbox 以及 K8s 上的 scene_manager 现在就有的行为。

规则放在代码里,不放在 yaml 里。只改各服务 yaml 的 `Breaker` 也能关掉(改的文件少十来个),但开关的缺省值是开:新调用方的 yaml 少写一行、K8s 的 ConfigMap 和本地 yaml 写得不一样,熔断就回来了,而且没有任何报错——scene_manager 现在两处不一致、db / mail / rank / payment / chat 没写,就是这么来的。go/db 那一轮也是在代码里写死的。

## 5. 定案

### 5.1 服务端

1. `go/data_service/data_service.go`:建 gRPC 服务之前 `c.Middlewares.Breaker = false`,yaml 写开也不认(同 go/db)。Shedding、Timeout 不动。
2. `go/shared/authoritysql/pool.go`:新增

   ```go
   // ConnectGate 包住一次建物理连接。回错误时不建连,Connect 直接回这个错误;
   // 否则回 settle,建连结束后带着结局调一次。
   type ConnectGate func(ctx context.Context, logicalDB string) (settle func(error), err error)

   func WithConnectGate(gate ConnectGate) OpenOption
   ```

   `connector.Connect` 在有钩子时前后各调一次;没有钩子时与现在逐字相同。本包不引入任何熔断实现,也不改错误的不透明性(钩子只知道成没成)。`PinCurrentSession` / `PinPlacedSession`(go/db 用)不带钩子,不受影响。
3. `go/data_service/internal/svc/db_breaker.go`(新):钩子的实现,约 20 行。

   - 熔断名 `logical_db/<逻辑库名>`,用 `breaker.GetBreaker(name).Allow()`;同一逻辑库的几个池共用一个。
   - 结算:建连成功 → `Accept`;失败而 `ctx.Err()` 是 `context.Canceled` → 不结算,成功和失败都不记;其余失败 → `Reject("connect failed")`。
   - 打开时回 `logical database <名>: circuit breaker is open`(包着 `breaker.ErrServiceUnavailable`)。各 handler 现有的 ERROR 日志会带上这句,从日志能看出是哪个库。
4. `go/data_service/internal/svc/servicecontext.go`:装配 `mysqlCfg.PoolOptions` 的那一行把钩子加进去。目录各片、流水库、存档库的池都从这里拿选项(`MySQLConfig.ForSchema` 原样带下去),所以全部覆盖。`-migrate`、`route_tool`、`cmd/` 下的工具各有自己的装配,不带钩子。

被熔断挡回的调用在 store 里表现为「拿不到连接」:事务走 `shardTx` 的没开始分支(确定没写),其余读写拿到一个普通错误,handler 照原来的映射回 `Unavailable` 或带内的重试 / 故障码。和今天建连失败时走的是同一条路。

### 5.2 调用方

1. `go/shared/dsclient/dsclient.go`(新):

   ```go
   // Conf 返回建 data_service 客户端用的配置:关掉 zrpc 按「目标 + 方法」的客户端熔断。
   func Conf(c zrpc.RpcClientConf) zrpc.RpcClientConf {
       c.Middlewares.Breaker = false
       return c
   }
   ```

   原因写在这一个函数的注释里,指向本稿。
2. 14 个构造点各改一行:`zrpc.NewClient(c.DataServiceRpc)` → `zrpc.NewClient(dsclient.Conf(c.DataServiceRpc))`(`MustNewClient` 同理)。
3. `go/shared/dsclient/dsclient_test.go`:除了核 `Conf` 本身,再扫一遍 `go/` 下的非测试源码——出现 `NewDataServiceClient(` 的文件必须同时出现 `dsclient.Conf(`(做法照 `shared/authoritysql/crossdb_contract_test.go`)。新加的调用方漏掉这一行时,它自己的熔断会悄悄回来而且零报错,所以要机械地守住。
4. 配置里已经失效的说法改掉:`go/login/etc/login.yaml`、`go/scene_manager/etc/scene_manager_service.yaml`、`tools/scripts/k8s_deploy.ps1` 的 login 段、`deploy/login-stack.linux/login.yaml` 里 `DataServiceRpc` 的 `Breaker: true` 与那段理由,改成「客户端熔断在代码里关掉,见本稿」。写着 `Breaker: false` 的几处(guild、trade、match、player_inbox 及其 K8s 段)与新行为一致,不动。

### 5.3 不做的

- **发号器的 etcd 不加熔断。** `idissuer.Issuer.Allocate` 手里有号时不碰 etcd;预留失败后进入自己的退避期,期间直接回错误(「no block available, retrying after backoff」),一次调用最多等一次预留结果。它是单一后端,没有分片之间互相牵连的问题,快速失败也已经有了。
- **Redis 不加。** 名字读缓存写失败不影响结果,读失败回源。
- **不加指标、不加告警规则。** 被挡回的调用仍计入各 RPC 现有的结局指标(账号目录 `result="error"`、归属读、路由等),ERROR 日志带逻辑库名。要单独的「某库熔断打开」指标时再加,现在没有数据说明需要。
- **不改任何 RPC 的错误码**,包括 §1.3 里那两个把业务拒绝回成 `ResourceExhausted` / `Unavailable` 的地方:两层按方法的熔断都关掉之后,它们不再被任何熔断计数;改码会动调用方契约,不在这次范围。
- **不动 go/db** 的 `db.go` 与 `internal/playerstore/server.go`。go/db 作为 data_service 调用方的那个构造点(`player_store_wiring.go`)在 §5.2 的 14 个里。

### 5.4 行为变化一览

| 情形 | 现在 | 改后 |
|---|---|---|
| 一个集群失联,落在它上面的流量占某方法三分之一以上 | 两层熔断把这个方法上指向健康库的调用也挡掉,只剩每秒一次 | 只有碰到坏库的调用失败;流量够大时这些调用在建连处被直接挡回 |
| 一个集群失联,占比不到三分之一 | 坏库上的调用各自等超时 | 同上一行 |
| 搬库冻结窗口 | 写入回 `Unavailable`,计入按方法的熔断(占比够大时会连带别的库) | 不计入任何熔断 |
| 账号角色到上限、建角登记参数非法 | 计入按方法的熔断 | 不计入 |
| data_service 全部副本不在 | gRPC 立刻回 `Unavailable`,熔断随后也打开 | gRPC 立刻回 `Unavailable` |
| data_service 整体慢(不是 CPU 打满) | login 等开着熔断的调用方在失败够多之后快速失败 | 每次等到各自的截止时间 |
| 调用方看到的错误 | gRPC 码,外加熔断自己的非 status 错误 | 只有 gRPC 码 |
| Kafka 消费者落库重试 | 流水库不可达时每次尝试等 1 秒建连 + 1 秒退避 | 熔断打开后每次尝试立刻失败 + 1 秒退避,更早用完 `DBMaxAttempts`;结局不变 |

## 6. 影响的文件(32 个)

服务端(6):

1. `go/shared/authoritysql/pool.go`
2. `go/shared/authoritysql/connect_gate_test.go`(新)
3. `go/data_service/internal/svc/db_breaker.go`(新)
4. `go/data_service/internal/svc/db_breaker_test.go`(新)
5. `go/data_service/internal/svc/servicecontext.go`
6. `go/data_service/data_service.go`

调用方(16):

7. `go/shared/dsclient/dsclient.go`(新)
8. `go/shared/dsclient/dsclient_test.go`(新)
9. `go/chat/internal/svc/authority.go`
10. `go/db/player_store_wiring.go`
11. `go/friend/internal/svc/servicecontext.go`
12. `go/guild/internal/svc/guild_id_minter.go`
13. `go/guild/cmd/assetopfix/main.go`
14. `go/login/internal/svc/home_zone.go`
15. `go/login/internal/svc/player_id_minter.go`
16. `go/match/internal/svc/servicecontext.go`
17. `go/payment/channels.go`
18. `go/player_inbox/internal/svc/servicecontext.go`
19. `go/scene_manager/internal/svc/servicecontext.go`
20. `go/shared/domainhost/dependencies.go`
21. `go/trade/internal/svc/servicecontext.go`
22. `go/trade/cmd/assetopfix/main.go`

配置里的说明(4):

23. `go/login/etc/login.yaml`
24. `go/scene_manager/etc/scene_manager_service.yaml`
25. `tools/scripts/k8s_deploy.ps1`
26. `deploy/login-stack.linux/login.yaml`

文档(6):

27. `docs/design/data-service-breaker-per-logical-db.md`(本稿)
28. `docs/README.md`
29. `docs/PROGRESS.md`
30. `docs/design/horizontal-scale-logical-db.md`
31. `docs/design/data-consistency-guarantee-map-20261020.md`
32. `docs/design/db-task-store-unavailable-deferral.md`(文首有一句「目录路由不可达由 zrpc 客户端自带的熔断管」,改后不再成立)

不涉及:proto、生成产物、C++、Java、数据库表、部署清单的结构。

## 7. 单测(落码时写,交 Gemini 跑)

- `shared/authoritysql`(用包里现成的假拨号器与假驱动):
  - 钩子回错误时不拨号,`Connect` 的错误就是钩子的错误;
  - 建连成功结算 `nil`,落点解析失败 / 拨号失败结算非 `nil`;
  - **冻结不计失败**:闸门冻结时事务回 `ErrWriterFrozen`,钩子收到的建连结局仍是成功;
  - 不传钩子时行为不变(现有用例不改)。
- `data_service/internal/svc`:
  - 一个逻辑库一直建连失败:若干次之后这个库的调用被挡回,错误里有库名、`errors.Is(err, breaker.ErrServiceUnavailable)`;同时另一个逻辑库每一次都放行;
  - 调用方取消(`context.Canceled`)结算多少次都不打开;
  - 调用方取消也不算成功:每 3 次取消夹 1 次真实的建连失败,熔断照样打开(记成成功的话永远不开);
  - 熔断表是进程级的,用例用别处不会出现的库名。
- `shared/dsclient`:`Conf` 只改 `Breaker` 一项;构造点守卫(§5.2 第 3 条)。
- 可选冒烟(调试栈,目录 `LogicalShards` > 1 或两个存档库,分在两个 SQL 实例上):停掉其中一个实例,落在另一个实例上的账号照常登录、建角、进场;data_service 日志里只有坏库的 `logical database … circuit breaker is open`。单实例的调试栈做不了这一步,如实报未运行。

## 8. 残余与后续

以下各条 2026-11-26 复核时都已定案,不留待以后:

- **语句层不再加熔断(S4 不做)。** 建连处的熔断看不到「连接都在、语句持续失败又不超时」的情形(§4.1 末),但这种情形不需要它:语句立刻回错,这次调用本身就是快速失败,熔断只能再省一次往返;语句挂住的,由读写超时(5 秒)关掉连接,下一次请求变成建连,由建连处的熔断接住。
- 一个逻辑库流量很小时(每 10 秒不到 6 次建连失败),熔断不会打开,坏库上的调用各自等到建连超时。流量小的时候这没有代价;隔离不依赖它。
- **调用方取消:成功和失败都不记(2026-11-26 改,原来记成成功)。** 调用方的截止时间短于建连超时(1 秒)时——guild 读账本 300ms、login 角色列表 500ms、建角登记 800ms——坏库上的那次建连是被调用方的截止时间打断的。这时服务端有两个信号几乎同时到:自己按 `grpc-timeout` 设的截止定时器(`DeadlineExceeded`),和调用方到点后发来的 RST(grpc 收到后把请求的 ctx 取消成 `Canceled`)。按 grpc-go v1.79.3 的 `internal/transport/http2_server.go` 核对过:带 `grpc-timeout` 的请求用 `context.WithTimeout` 建 ctx,收到 `RST_STREAM` 走 `closeStream` 里的 `s.cancel()`;两者谁先生效只看到达的先后。谁先到不定,所以同一种「坏库上等到超时」有一部分表现成 `Canceled`。
  - 记成成功:这部分每一次都在给坏库加成功数,按 §1.3 的算法,占比过三分之二熔断就永远不开,不到三分之二也开得慢。
  - 记成失败:调用方正常挂断(玩家断线、上游取消)会冤枉好库。
  - 都不记:go-zero 的 `Allow()` 返回的 promise 不占名额,不结算就是这一次不进统计。熔断只靠确定的信号打开:建连自己超时、服务端时限先到。调用方截止时间都很短、RST 又总是先到的库,熔断可能攒不够样本,那些调用本来也只等自己那几百毫秒。
  - go/db 的 `enterStore` 同日也改成都不记(原来记成成功)。它没有上面的竞争——scene 调它的截止时间(14 秒)长于 go/db 自己的方法时限(12 秒),先到的总是服务端的 `DeadlineExceeded`——但 scene 崩溃或重启时在途的存盘仍以取消收场,而且两处同一条规则不该各说各的。
- **启动期不在隔离范围内。** 上面说的隔离是运行期的。data_service 启动时要把目录全部片、流水库的池开起来并逐片核对(`directory_shard`、写资格、表形状),任何一个逻辑库连不上,store 装配不起来,`Store.Required` 让进程拒绝启动。所以一个集群失联期间:已经在跑的副本照常服务健康库;新起或重启的副本起不来,要等那个集群恢复。这是「缺权威 store 不带病注册」的既定规则(配错分片数会把玩家路由到别的片而且不报错,所以启动核对不能跳过),不在这次改动里放宽。运维上的含义:单集群故障期间不要滚动重启 data_service。
  - 要放宽是一项单独的设计,不是一处小改:各片要改成第一次用到才打开、打开时做同样的核对(存档库的 `PlayerStorePools` 已经是这个做法),八十多处选池的代码要能拿到「这片还没打开」;合服映射(只在第 0 片)、路由的空环境判据这些要读全部片的启动判定也要逐个重定。逻辑库都在同一个集群时它没有收益,要等逻辑库分到第二个集群之前做。2026-11-26 已作为后续任务提给负责人定夺。
- **建连钩子不区分「连不上」和「连上了但会话核对不过」(写资格不符、后端档位不符),一律计成建连失败。** 不能分开:会话核对里读 `route_meta` 失败和资格不符,在守卫里本来就是同一个错误(`connector.pin` 把读失败也回成 `ErrWriterDenied`),而「TiDB 进程在、存储不可用」正是靠这一步读失败计入熔断(§4.1);把资格不符排除出去,这一类故障的快速失败就一起没了。计进去的后果有界:data_service 里挂钩子的池在同一个逻辑库上用的是同一份写资格,不存在「写者被拒、把同库健康的读池一起挡掉」;资格恢复之后,熔断最多滞后一个 10 秒窗口(每秒强制放行一次)才全部放开。
- **其余 Go 服务不用改**(2026-11-26 逐个核对)。判据:一个 zrpc 服务背后有几个可以各自失联的后端,而且失败回的是 §1.1 列的那几个 gRPC 码。
  - chat、friend、guild、login、mail、match、payment、player_inbox、rank、trade 各只有一个逻辑库(各一处 `authoritysql.Open`,没有按请求选库),库挂了就是整个服务不可用,按方法的熔断对它们合适。
  - client_rpc_router 背后是多个服务,但下游失败是随包回的提示码(gRPC 状态仍是 OK),不计入按方法的熔断;它另有按实例的熔断(`shared/circuitbreaker`)。
  - go/db 已处理(按存档库)。以后新加的服务如果符合判据,照 go/db 或本文的做法做。

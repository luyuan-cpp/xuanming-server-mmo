# data_service 启动期故障隔离:一个 SQL 集群连不上时照样启动

> **状态(2026-11-27,Claude Code)**:设计定稿。**批 1(常驻进程)已落码,未编译、未运行、未提交**,验证命令见 `docs/PROGRESS.md` 2026-11-27「data_service 启动期故障隔离批 1」条目。**批 2(写者注册)只有方案,等负责人对范围和两个选项的同意**(§7)。
> **来源**:[data_service 按逻辑库熔断](data-service-breaker-per-logical-db.md) §8「启动期不在隔离范围内」,负责人 11-26 要求现在做。
> **证据口径**:静态阅读本仓代码(`go/data_service`、`go/shared/authoritysql`、`go/shared/dbplacement`、`cmd/writer_authority`、K8s 清单),没有运行证据。
> **Java 版(AGENTS §12)**:不涉及(Java 版没有 data_service),不加 PARITY 行。

## 0. 结论

1. **启动期的耦合有两层,任务原先只看到一层。**
   - **常驻进程**:`NewServiceContextWithAdmission` 把目录 K 片(每片三个池)和流水库(三个池)一次开齐、逐个核对,任何一个逻辑库连不上,store 为 nil,`Store.Required` 拒启。
   - **写者注册**(init 容器 `writer_authority register`):一代写者要在进程配置的**每个**逻辑库各注册一份,注册器先逐库预检,有一个库连不上就一份都不注册、init 原地重试(`cmd/writer_authority/main.go` 的 `slotRegistration.register`)。主容器根本起不来。
   - 新 Pod(发布、扩容、写过之后崩溃被回收重建)都要重新注册,所以**只改常驻进程,新 Pod 在集群失联期间照样卡在 init**。只改这一层能救的只有两种:init 注册完之后集群才失联;主容器还没写过任何库就崩了、原地重启(同一代)。
2. **两层都要改,分两批。** 批 1 是常驻进程(本稿 §2~§6,已落码,未编译);批 2 是注册层(§7),它动所有写者服务共用的注册器和部署清单,还有两个要负责人定的选项,没有同意不动。**批 2 落地之前,「单集群故障期间不要滚动重启 data_service」这条运维口径不变。**
3. **批 1 的做法**:每个池从一开始就有 `*sql.DB` 句柄,原来开池时的那一串核对整体挪到「这个池第一条物理连接建立之前」,挂在建连钩子上(和逻辑库熔断同一个接缝)。九十多处选池代码一行不改;核对一项不删,只换时刻(§3)。
4. **启动时仍然每个池都试一次。** 结局分三种:核对通过 → 打开;**连不上** → 这个池保持关闭,进程照常启动,以后用到时再核对(失败冷却 10 秒);**连上了但核对不过**(片号对不上、表形状不对、会话不符)→ 照旧拒启。
5. **仍然是启动硬依赖的**:配置、写资格、映射(etcd)、目录第 0 片(合服映射只在这一片)、路由的空环境判据。原因和逐项判定在 §4、§5。所以批 1 之后的准确说法是:**第 0 片以外的目录片、流水库所在的集群失联,不再挡 data_service 启动。**
6. **数据一致性不受影响。** 一个池在核对通过之前拿不到任何连接,对这个库一条语句都发不出去;调用方看到的是「拿不到连接」,和今天建连失败走同一条路(事务按没开始算,确定没写)。
7. **文件**:批 1 共 26 个(§8.1,不到 30);批 2 预计 20~25 个(§8.2)。

## 1. 现状

### 1.1 常驻进程启动时做了什么

| 步骤 | 位置 | 失败时 |
|---|---|---|
| 读映射(etcd) | `dbplacement.Start` | panic |
| 读写资格、核对恰好覆盖要写的库 | `authoritysql.LoadWriterConfigs`、`checkWriterSchemas` | panic |
| 流水库:快照、流水两个池 | `store.NewSnapshotStore`、`NewTransactionLogStore` | store 为 nil |
| 名字:每片一个池 | `store.NewPlayerNameStore` → `openShardPools` | store 为 nil |
| 账号目录:每片一个池 | `store.NewAccountDirectoryStore` → `openShardPools` | store 为 nil |
| 路由:每片一个池,再跑空环境判据 | `store.NewRouteStore`、`checkEmptyRouteBootstrap` | store 为 nil |
| 读一次合服映射(第 0 片) | `newMergedZoneCache` | 缓存为 nil |
| 流水库:回收池 | `configureRecall` → `store.NewRecallStore` | Recall 为 nil,角色护栏协调者也不装配 |
| 存档库 | `PlayerStorePools`,不在启动时打开 | — |

之后 `data_service.go`:`MissingStores()` 非空且 `Store.Required` → 拒启;`MergedZonesUnassembled()` → 无条件拒启。

每个池打开时的核对分两段:`authoritysql.Open` 自己的(Ping、实际库名、强读会话、TiDB 版本、闸门表形状),和 store 的(片号 `directory_shard`、表形状、路由会话变量与 CHECK、快照唯一键探测、护栏副本表)。每条新物理连接另有 `pin`(主体、库名、世代、本 boot 的注册全文),这一段本来就是逐连接的。

### 1.2 写者注册

- Pod 的 init 容器 `sql-writer-registrar` 跑 `writer_authority -command register`:取各库 `LatestBoot` 定新一代的 `boot_seq` → 逐库 `RegistrationReady` 预检 → 写启动记录 → 逐库 `Register` → 写出 grant 集合。任何一步连不上某个库就整体失败([写者名册换代](writer-roster-exhaustion-20261106.md) §5.3、§5.6)。
- 主容器读 grant 集合。写资格按代一次性:这一代写过任何一个库之后进程退出,容器重启读到消费标记以 78 退出,回收任务删 Pod,同序号新 Pod 重新注册([水平扩展](horizontal-scale-logical-db.md) §2「写资格什么时候消费」)。
- init 容器只在 Pod 建立时跑,容器原地重启不重跑。

所以一个集群失联时:

| 情形 | 现在 | 只做批 1 | 批 1 + 批 2 |
|---|---|---|---|
| 已经在跑的副本 | 照常服务健康库(运行期隔离,11-20 已做) | 同左 | 同左 |
| init 已注册完,主容器启动时集群失联 | 拒启,反复重启 | **启动** | 启动 |
| 主容器没写过就崩溃,原地重启 | 拒启 | **启动** | 启动 |
| 写过之后崩溃,Pod 被回收重建 | 卡在 init | 卡在 init | 启动 |
| 滚动发布、扩容 | 卡在 init | 卡在 init | 启动 |

go/db 同样写多个库(全部存档库),连接池本来就按需打开,但它的注册也是这样全有或全无。批 2 改的是共用的注册器,go/db 一起受益。

## 2. 「这个池还没打开」怎么传到选池的地方

选池的代码有九十多处,分在 `internal/store` 的二十来个文件里,都直接拿 `*sql.DB`(`forPlayer` / `forAccount` / `forName` / `first` / `eachPlayerGroup` / `pools[i]`)。

| 方案 | 做法 | 结论 |
|---|---|---|
| A 选池函数回错误 | `forPlayer(id) (*sql.DB, error)`,每处调用加一段错误处理 | 九十多处各改一遍。`shardTx` 靠 `txNotBegunError` 区分「事务没开始、确定没写」,每处新加的错误出口都要保证落在这一类里,漏一处就把确定没写报成结局未知(或反过来) |
| B 每片的池指针可以为空 | 没打开时 `pools[i]` 是 nil | 同 A,而且漏判就是空指针 |
| **C 句柄始终存在,核对挪到第一条连接之前** | `sql.OpenDB` 本来就不拨号。给每个池一个建连钩子:没核对过就先核对,不过就拒绝建连 | **选它**。选池代码不动。没打开的池在调用方看来就是「拿不到连接」,和集群失联、熔断打开是同一种失败,现有的错误映射已经全部覆盖(熔断那一轮按同一个理由选了建连接缝) |

C 成立靠一条性质:**池在核对通过之前一条连接都没有**。`database/sql` 只有经 `Connector.Connect` 才能得到连接,钩子在 `Connect` 最前面,不放行就不拨号;所以不存在「业务从池里借到一条没核对过的连接」。

核对本身不在这个池上跑,而是**另开一个只用一次的小池**(2 条连接),把原来「急切打开 + store 核对」那段代码原样跑一遍,跑完就关。这样:

- 原来的核对函数都按 `*sql.DB` 写,一个签名都不用改;
- 核对用的连接不进业务池,核对到一半时业务借不到它;
- 「核对」和「工具、迁移当场打开」是同一段代码(`store.openCheckedPool` 的非按需分支),不会两份规则各走各的。

多出来的代价是每个池第一次核对时多建一条连接、用完即关。

**片号核对不交给调用方**:原来 `openShardPools` 开完每一片自己核 `directory_shard`。改成按需之后核对要跟着池走,所以三个分片 store 只描述「这一片的 DSN 和表形状核对」,`openShardPools` 把片号核对接在表形状核对后面,一起交给 `openCheckedPool`。新加一个分片 store 不会漏掉这道,这是这次改动里最要紧的一道核对(§3)。

## 3. 每项启动核对挡什么,换了时刻之后谁来挡

核对一项没删。下面的「之后」都是指:这个池第一条物理连接建立之前,在一次性的小池上跑;通过才放行,不过就一条连接都不给。

| 核对 | 挡的场景 | 之后 |
|---|---|---|
| `Ping` | 连不上、账号口令不对 | 小池的急切打开里照做 |
| `SELECT DATABASE()` 等于期望库名 | 连接串指错库 | 同上;写者池每条连接的 `pin` 另核一次 |
| 强读会话、TiDB 版本、档位 | 接到从库、接到版本过低或不是声明档位的库 | 同上;档位每条连接的 `pin` 另核 |
| 闸门表形状(`VerifyCatalogSchema`) | 迁移没跑、闸门表被改过 | 同上 |
| 本 boot 的注册全文(`pin` 里的 `verifyBootRegistration`) | 写资格和库里的注册对不上、这一代已被换代 | 不变:本来就是每条新物理连接都核,晚打开的库在它第一条连接上核 |
| `directory_shard` 的 K 与片号 | **配错 K 或连错片**:玩家被路由到别的片,读到「角色不存在」而且不报错 | 小池上照做。对不上时这个池永远不放行(每次重试都重核,错的还是错的);启动时连得上而对不上,照旧拒启 |
| 账号目录、名字、路由各表的形状与主键 | 迁移没跑或跑了旧版:ODKU 撞不到键,唯一性静默失效 | 小池上照做 |
| 路由池的会话变量(RC、锁等待 1 秒)、`chk_route_shard` 生效且模数正确、TiDB 的 CHECK 总开关 | 锁推演不成立;算错片的写者能给同一个玩家写出两行 | 小池用同一条连接串,照做 |
| 快照唯一键探测(`uk_snapshot_guid_nz`) | TiDB 上没有唯一键时去重不成立 | 小池上照做。探测结果决定去重语句,所以读它的方法先确认探测做完(`SnapshotStore.ready`),不会在探测之前按缺省值选语句 |
| 护栏副本表形状(`rolefence.VerifySchema`) | 流水库缺 `role_fence` | 小池上照做 |
| 存档库 `SELECT 1 FROM player_store` | 缺表的库早点报 | 不变,`PlayerStorePools` 本来就是第一次用到才核 |

两点补充:

- **搬库翻转之后不重核**,和现在一样:现在也只在启动时核一次,之后映射翻到别的集群不再核;目标集群的等价性由 `tools/dbmove` 的 `plan` / `verify` / 静止比对保证。
- **核对撞上闸门冻结**(搬库窗口)不进冷却,窗口一过下一次使用就重核。`authoritysql.Open` 自己的核对不过闸门;store 的只读核对在这个 boot 还没在这个库上过过写事务时放行,过过之后会被冻结挡回(`readPassesFrozenGateLocked`),所以晚打开的池可能撞上,规则和存档库相同。

## 4. 要读全部片或固定某一片的判定

| 判定 | 现在 | 定案 | 理由 |
|---|---|---|---|
| **合服映射**(`zone_merged_into`,只在第 0 片) | 启动时读不到无条件拒启 | **不变**。第 0 片的路由池打不开 → 读不到 → 拒启 | 映射是全局的:归属读(`GetPlayerHomeZone`、`BatchGetPlayerHomeZone`、`CountZoneRoles`、`GetMergedZones`)不管玩家在哪一片都要它。已经在跑的副本靠缓存照常回答;一个没读到映射的新副本进了发现池,会把别的副本答得了的归属读按比例答成 `Unavailable`,等于让健康片上的玩家变差。所以读不到就不进池 |
| **路由的空环境判据**(`checkEmptyRouteBootstrap`) | 逐片问有没有路由行,遇到读不了的片就报错 | 任一片有行就通过,读不了的片跳过;**一行都没找到而且有片读不了**才报错拒启。之后的角色历史核对不变 | 这道判据防的是「整张路由表被清空或回档」,只要有一片有行就不是。生产上每片都有行,第一片就返回。读不全又一行没找到时,分不清「全空」和「没读到」,保持拒绝;这只会出现在新空环境加集群失联 |
| `ClaimAccountDeletions` | 全片逐片扫,一片失败不影响别的片 | 不变 | 没打开的片就是一片失败 |
| `CountZoneRoles`、`BatchGetPlayerHomeZone`、`BatchGetPlayerName` | 任一片失败整批 `Unavailable` | 不变 | 半份结果会被当成「这些人没有」「这个区远没到上限」 |
| 后台清理(陈旧 ref、删角续做、超时 PREPARING、改名续做) | 逐片、逐轮换起点 | 不变 | 运行期本来就会遇到某一片失败 |
| Kafka 两个消费者(落流水库) | store 缺席时不启动消费 | store 始终装配,消费照常启动;流水库没打开时落库失败,按现有的重试、停在位点处理 | 和运行期流水库失联是同一种情形,不丢不重的保证不变 |
| 审计清理 | 读写流水库 | 不变,失败只记日志 | 同上 |
| 回收通道、角色护栏协调者的本地参与者(流水库) | 回收池打不开 → 协调者不装配,过户、删角在这个进程的整个生命期都被拒 | 回收池句柄始终在,协调者照常装配;流水库没打开时本地参与者回 `retry`,协调者按问不齐处理(`ErrRetryLater`),流水库恢复后自己好 | 少问一个参与者是漏冻结,所以不能跳过;问不到就拒绝是原来的规则,没有放宽 |
| 存档库 | 按需打开 | 不变 | — |

**第 0 片是批 1 留下的限制。** 要去掉它,合服映射得放到不依赖第 0 片的地方(每片一份并带版本,或者别的存储),那是数据模型的改动,合服工具也要跟着改,不在这两批里(§10)。部署上的含义:第 0 片所在的集群是 data_service 的主集群;搬库扩容时把别的片、流水库、存档库往外搬,这些集群失联不挡启动。

## 5. `Store.Required` 与拒启清单

`Store.Required=true`(生产恒为真)的语义改成:

- **缺任何一个 store 仍然拒启。** store 缺席的原因只剩:配置非法,或者某个池**连上了但核对不过**。
- **某个逻辑库连不上不算缺 store。** store 照常装配,那个库的池保持关闭,用到时再核对。

启动时仍然拒启的完整清单:

1. 配置、调用方密钥、发号器地域码等静态校验(不变)。
2. 映射读不到(`dbplacement.Start`,不变);**任何要写的逻辑库解析不出落点**(映射是坏值,或指向本进程 `SqlClusters` 里没有的集群)。后一条原来是开池时才失败,现在在连任何库之前逐库问一次解析器,当场拒启:它是配置错误,不是集群失联。
3. 写资格缺失、已消费、和要写的逻辑库不是恰好对应(不变)。
4. 任何一个池连上了但核对不过(§3 各项)。
5. 合服映射读不到(含第 0 片的路由池打不开)。
6. 路由的空环境判据不过,或判不了(§4)。
7. `Recall.Enabled=true` 而资产通道装配不起来(不变)。

**「连不上」的判据只有一个**:`authoritysql.Open` 拿不到第一条可用连接(新哨兵 `authoritysql.ErrUnreachable`),即拨号、握手、会话初始化、写者身份核对任一步没过。档位不符(`ErrBackendMismatch`)是确定的部署错误,不算在内。拿到连接之后的任何失败(包括核对到一半读失败)都按「核对不过」处理,启动时拒启、容器重启再试,和现在集群抖动时的行为相同。不去拆「集群失联」和「账号口令不对、注册不在」:守卫有意不暴露底层错误,拆不出来;这几种都是「对这个库一无所知」,处理也相同——这个库不给用。

**为什么不干脆全部容忍**:连得上而核对不过是确定的部署错误(K 配错、迁移没跑)。带着它启动,发布会一个副本一个副本地把好的换成坏的;当场拒启,滚动发布停在第一个副本。

**容忍「连不上」放宽了什么**(明确接受):运行账号在某个集群上口令不对、被锁,新副本现在会带着那几个关闭的库启动,而不是拒启。两条限制着它:注册器用的是另一个账号但同一组地址和映射,地址、网络策略这类错误仍然先卡在 init;第 0 片所在的集群有这种错误时照旧拒启。存档库从 D1 起就是这个行为。批 2 把注册器也放宽之后,这条保护要另外补上,见 §7.3。

**写资格**不变:`LoadWriterConfigs` 一次读齐。注册的核对本来就在每条新物理连接的 `pin` 里,晚打开的库在它第一条连接上核,不需要另找时刻。消费标记在第一条写语句之前写,核对全是只读,不消费。

## 6. 和建连熔断、按映射拨号的配合

- **顺序**:一次建连先过「首次核对」钩子,再过逻辑库熔断,再解析落点、拨号。`authoritysql.WithConnectGate` 现在可以传多次,按传入顺序执行;后一个拒绝时,前一个已经拿到的结算按这个错误结算。
- **没核对过的池不喂熔断**:核对钩子在前,拒绝时熔断的 `Allow` 没被调用。核对用的小池带着同一份选项,它的建连成败照常计入这个逻辑库的熔断;集群失联时熔断打开,之后每次重新核对在建连处被直接挡回,不再各等 1 秒。
- **冷却与熔断各管一段**:核对失败后 10 秒内的调用直接拿到上次的错误(不建连);10 秒后放一次重新核对。两者叠加的最坏情况是集群恢复后最多再等一个冷却期加熔断每秒放行一次的间隔才打开。
- **映射**:小池和业务池用同一个解析器,每次拨号现读。启动时逐库问一次解析器只为把「落点未知」提前成拒启,不改连接行为。
- **规则在一处**:「第一次用到才打开、成功后常驻、失败冷却 10 秒、闸门冻结不进冷却、并发共用一次结果」原来只在 `PlayerStorePools` 里,现在抽成 `openOnce`,存档库连接池和各池的首次核对共用,不再有第二份。
- **等核对时不看调用方的 ctx**(和 `PlayerStorePools.DB` 原来的规则相同):核对的结果被并发请求共享,不能随第一个调用方的取消一起失败。代价是触发核对的那几个请求要等它做完:集群失联时是拨号超时(1 秒),连得上时是几条只读查询;每个池每个冷却期最多一次,冷却期内的请求直接拿上次的错误,不等。不另起协程去「先回错误、核对接着做」:多一个异步结局,换来的只是这一秒。
- **启动多花的时间**:每个连不上的库每个池多等一次拨号超时。分片按 8 路并行开,三个分片 store 依次开:64 片里有 32 片连不上时约多 12 秒,流水库三个池再多 3 秒,在存活探针的宽限(15 秒起、20 秒一次、失败 3 次)之内。

## 7. 批 2:写者注册层(方案,未落码,等同意)

### 7.1 要改什么

1. **注册器容忍连不上的库**(`cmd/writer_authority`):定 `boot_seq`、预检、注册三步都只对连得上的库做;连不上的库在本次运行的预算内重试几次,仍连不上就跳过。grant 集合只含注册成功的库。「连不上」用同一个哨兵 `authoritysql.ErrUnreachable`;连上之后的冻结、资格不符照旧整体失败、原地重试。只写一个库的服务不受影响:它唯一的库连不上就没有 grant,照旧起不来。
2. **常驻进程容忍 grant 是子集**(data_service、go/db):grant 不许多(多出来仍拒启),可以少;没有 grant 的库在这一代的整个生命期关闭,调用回不可用。第 0 片没有 grant 时 data_service 仍拒启(§4)。
3. **恢复**:集群恢复后,带着不完整 grant 的 Pod 要重建才能拿到完整的一代。

### 7.2 安全性

- 被跳过的库一个字都没写;这一代在那个库没有注册行,`pin` 每条连接都核注册,所以即使 grant 发错了也连不上。授权的真源是库里的注册行,不是 grant 文件。
- 被跳过的库上,上一代仍是 ACTIVE。上一代的进程已经不在(同一序号同一时刻只有一个 Pod);强删之后旧节点其实还活着的那种僵尸,在连得上的库上被新一代当场围栏,在被跳过的库上要等下一代注册——现在这种情况下新一代根本注册不了,僵尸在所有库上都没被围栏,所以只会更好。
- `boot_seq` 只要求在每个库内递增。取连得上的各库最大号加一;被跳过的库以后由更晚的一代注册,那时取的是全部库的最大号。
- 排空 run 在被跳过的库上会把没有进程的上一代算进 ACTIVE 全集,要人工退休或等 Pod 重建(开服手册「没有进程的写者」,和缩容留下的末代同一处理)。

### 7.3 两个要负责人定的选项

**选项一:集群恢复之后怎么补上。**

| | 做法 | 代价 |
|---|---|---|
| **A(推荐)重建 Pod** | 不完整的一代不补注册;集群恢复后滚动重启(`kubectl rollout restart`),新一代在全部库注册 | 恢复要一步人工操作;在那之前这些 Pod 上该集群的库一直不可用 |
| B 常驻的注册容器补注册 | 注册器改成 Pod 里常驻的 sidecar,对没注册上的库一直重试;注册落地后常驻进程下一次核对就打开 | 全自动。但注册账号(能登记、退休写者)从「只在 init 里出现」变成「每个写者 Pod 里常驻」;依赖 K8s 原生 sidecar(1.29 起默认开);十二个写者服务的清单都要动 |

推荐 A:不多出常驻的高权限容器,不引入平台版本依赖;集群级故障的恢复本来就有人在场。

**选项二:降级启动的副本怎么不让滚动发布一路换下去。**

注册器放宽之后,「注册账号在某个集群上连不上」既可能是集群失联,也可能是这次发布把地址、口令、网络策略配错了。后一种在现在是 Pod 卡在 init、发布停在第一个副本;放宽之后新 Pod 会带着缺口启动,发布继续,直到所有副本都缺这个集群。必须补一道:

| | 做法 | 代价 |
|---|---|---|
| **A(推荐)缺 grant 的副本不算 Ready** | data_service、go/db 加 `/readyz`(指标端口),有库没有 grant 时回 503;就绪探针改指它。StatefulSet 滚动发布等不到 Ready 就停 | 降级的副本照常经 etcd 被发现、服务健康库(Go 与 C++ 调用方都不看 K8s 就绪);但 `data-service` 这个 ClusterIP 只剩 Ready 的副本,目前只有 gm-admin 的邮件工具走它。PDB 把降级副本算成不可用,节点排空更保守 |
| B 运维显式声明 | etcd 里记「某集群已声明不可用」,注册器和常驻进程只对声明过的集群容忍,没声明的照旧严格 | 不用猜错误原因,发布保护原样保留。但不是自动的:声明之前重启的 Pod 照旧起不来;要加一个声明工具、一条手册、一条「声明挂得太久」的告警 |
| C 只加告警 | 不拦发布 | 不推荐:一次配错就能把一个集群上的库全部做成不可用 |

推荐 A。它同时就是「哪些 Pod 该重建」的标记(选项一 A 的操作对象)。

### 7.4 范围

预计 20~25 个文件(§8.2),动所有写者服务共用的注册器、两个服务的清单与探针、告警、手册。和正在改 `k8s_deploy.ps1` 的会话要先分工。

## 8. 文件清单

### 8.1 批 1(26 个,已落码,未编译)

共享(2):

1. `go/shared/authoritysql/pool.go` —— `OpenUnverified`(只做静态校验、不连库的句柄)、`ErrUnreachable`、`WithConnectGate` 可叠加
2. `go/shared/authoritysql/connect_gate_test.go`

data_service 存储层(16):

3. `go/data_service/internal/store/lazy_open.go`(新)—— `openOnce`、`poolCheck`、`lazyPool`
4. `go/data_service/internal/store/lazy_open_test.go`(新)
5. `go/data_service/internal/store/mysql.go` —— `MySQLConfig.Lazy`、`openCheckedPool`;第二次 Ping 失败也归进 `ErrUnreachable`
6. `go/data_service/internal/store/directory_shards.go` —— `openShardPools` 只收 DSN 与表形状核对,片号核对由它自己接上(§2)
7. `go/data_service/internal/store/directory_shards_test.go`
8. `go/data_service/internal/store/account_directory_store.go`
9. `go/data_service/internal/store/player_name_store.go`
10. `go/data_service/internal/store/route_store.go`
11. `go/data_service/internal/store/route_bootstrap.go` —— `HasAnyRoute` 跳过读不了的片
12. `go/data_service/internal/store/route_bootstrap_test.go`(新)
13. `go/data_service/internal/store/snapshot_store.go` —— 探测挪进核对,`probePending` 与 `ready`
14. `go/data_service/internal/store/transaction_log_store.go`
15. `go/data_service/internal/store/recall_store.go`
16. `go/data_service/internal/store/player_store_pools.go` —— 改用 `openOnce`
17. `go/data_service/internal/store/player_store_access_test.go`、18. `release_holder_gate_test.go` —— 替身跟着换字段

data_service 装配(3):

19. `go/data_service/internal/svc/servicecontext.go` —— 常驻进程开 `Lazy`、落点逐库预检(`requirePlacements`)、`MissingStores` 的说明;顺带 gofmt 了一处原来就没对齐的结构体字段
20. `go/data_service/internal/svc/servicecontext_placement_test.go`(新)
21. `go/data_service/internal/config/config.go` —— `StoreConfig.Required` 的说明

文档(5):

22. `docs/design/data-service-startup-isolation.md`(本稿)
23. `docs/design/data-service-breaker-per-logical-db.md` —— §8 那一条
24. `docs/design/horizontal-scale-logical-db.md` —— §2「启动路径扛冻结」、§5.1「连接池」、§11 批次表
25. `docs/README.md`(本稿的索引行已由别的会话跑 `gen_docs_index.py` 时带上,本批没手改)
26. `docs/PROGRESS.md`

不涉及:proto、生成产物、C++、Java、表结构、部署清单、其余 Go 服务(工具、迁移、测试不开 `Lazy`,行为和原来相同)。

### 8.2 批 2(预计 20~25 个,未动)

`cmd/writer_authority` 的 `main.go` 与用例;data_service 的 `servicecontext.go`、关闭池的句柄、用例;go/db 的启动装配与用例;两个服务的 `/readyz` 与清单探针;`deploy/k8s/logical-db-alerts.yaml`;部署契约用例;[写者名册换代](writer-roster-exhaustion-20261106.md) §5.6、[水平扩展](horizontal-scale-logical-db.md) §2、开服手册、搬库手册。

## 9. 单测(批 1,交 Gemini 跑)

- `shared/authoritysql`(`connect_gate_test.go`):
  - `TestWithConnectGateChainsInOrder`:两个钩子按顺序执行;前一个拒绝时不问后一个;后一个拒绝时前一个按这个错误结算;都放行时各结算一次;传 nil 不清掉已有的钩子。
  - `TestOpenReportsUnreachableWhenNoConnectionCanBeMade`:拨号失败的 `Open` 回 `ErrUnreachable`。
  - `TestOpenUnverifiedDoesNotDialAndTheGateGuardsTheFirstConnection`:`OpenUnverified` 不拨号,钩子拒绝时一条连接都拿不到。
- `store` 的 `openOnce`(`lazy_open_test.go`):`TestOpenOnceKeepsTheFirstSuccess`、`TestOpenOnceBacksOffAfterAFailure`、`TestOpenOnceSharesOneOpenAcrossConcurrentCallers`;闸门冻结不进冷却由原有的 `TestPlayerStorePoolOpenFrozenDoesNotBackOff` 经存档库连接池覆盖(补了冷却过后重开成功、之后常驻)。
- `store` 的按需分片(`lazy_open_test.go`,替身照 `openCheckedPool` 的按需分支走):
  - `TestOpenShardPoolsKeepsAnUnreachableShardClosedUntilItVerifies`:一片连不上时装配成功,别的片照常给连接,这一片借连接回 `ErrUnreachable`、一条连接都不建;恢复后冷却期内仍关着,冷却过后先核对、通过才放行;放行之后新建连接不再核对。
  - `TestOpenShardPoolsNeverAdmitsAShardWithTheWrongSlot`:K 对不上时,启动时连得上就装配失败(而且不算「连不上」);启动时连不上、恢复后才对不上的,每个冷却期重核一次、每次都拒,一条连接都不给。
- `store` 的分片开池(`directory_shards_test.go`):原有用例改走 `openShardPoolsWith`;新增 `TestOpenShardPoolsRunsTheShapeCheckFirst`(表形状核对在片号核对前面,不过就拒绝)。
- `store` 的 `HasAnyRoute`(`route_bootstrap_test.go` 的 `TestHasAnyRouteSkipsUnreadableShards`):前一片读不了、后一片有行,回有;全部读得了且没有行,回没有;没有行且有片读不了,回错误并点名那一片。
- `svc`(`servicecontext_placement_test.go` 的 `TestRequirePlacementsRefusesAnUnresolvableDatabase`):有逻辑库解析不出落点时回错误,点名那个库。
- 存档库连接池原有用例(冷却、冻结不进冷却、开失败的落点跳过)改用新字段后语义不变。
- 可选冒烟(目录 `LogicalShards` > 1,片库分在两个 SQL 实例上):停掉不含第 0 片的那个实例再启动 data_service,进程起来,落在另一个实例上的账号照常登录、建角;日志里只有被停实例上的库 `stays closed`;把实例启回来,十来秒后那些库上的调用恢复,不用重启。单实例的调试栈做不了,如实报未运行。

## 10. 残余与后续

- **批 2 没做之前,新 Pod 在集群失联期间仍起不来**(§0 第 1 条)。运维口径不变:单集群故障期间不要滚动重启、不要扩容 data_service 与 go/db。
- **第 0 片所在集群是硬依赖**(§4)。去掉它要把 `zone_merged_into` 放到不依赖单一片的地方。没有第二个集群之前没有收益;要做时另开设计。
- **第 0 片失联期间,新起的 guild / trade / match 副本读不到合服映射**:它们启动时经 `GetMergedZones` 取映射,已经在跑的 data_service 副本靠缓存照答。不变。
- **不加指标**。关闭的池每次核对失败记一条 ERROR(带逻辑库名,10 秒一条),启动时逐个记;连接池指标里它的打开连接数是 0。批 2 做就绪判定时再加「缺口数」这一个指标。
- **运行账号口令类的配置错误在非主集群上不再挡启动**(§5 末)。批 2 的就绪判定(§7.3 选项二 A)可以顺带把「还有池没核对过」算成不 Ready,一并补上;批 1 不改探针。
- **缩容收尾任务**在集群失联期间整轮失败(它要读每个逻辑库),健康库上被缩掉的末代要等恢复之后才结束。只是推迟,不写坏数据;批 2 一并看要不要按库容忍。

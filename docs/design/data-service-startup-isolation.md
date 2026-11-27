# data_service 启动期故障隔离:一个 SQL 集群连不上时照样启动

> **状态(2026-11-27,Claude Code)**:设计定稿,两批与第二轮修复(§6.1、§7.5)都已落码,**未编译、未运行**。两批已随别的会话的批量提交入库(`3eb57a09a`、`76e2b59bb`、`a0b3d195e`);第二轮的文件(§11)在工作区未提交。验证命令见 `docs/PROGRESS.md` 2026-11-27 的三条:「data_service 启动期故障隔离批 1」(常驻进程)、「…批 2」(写者注册,负责人 11-27 同意 §7.3 两个推荐选项)、「…第二轮」。
> **后续(2026-11-27)**:最后一个启动硬依赖「目录第 0 片」由 [合服映射的 etcd 副本](merged-zone-etcd-copy.md) 去掉,已落码,未编译、未运行;本稿 §0、§4、§5、§7、§10 已按它改写。
> **来源**:[data_service 按逻辑库熔断](data-service-breaker-per-logical-db.md) §8「启动期不在隔离范围内」,负责人 11-26 要求现在做。
> **证据口径**:静态阅读本仓代码(`go/data_service`、`go/shared/authoritysql`、`go/shared/dbplacement`、`cmd/writer_authority`、K8s 清单),没有运行证据。
> **Java 版(AGENTS §12)**:不涉及(Java 版没有 data_service),不加 PARITY 行。

## 0. 结论

1. **启动期的耦合有两层,任务原先只看到一层。**
   - **常驻进程**:`NewServiceContextWithAdmission` 把目录 K 片(每片三个池)和流水库(三个池)一次开齐、逐个核对,任何一个逻辑库连不上,store 为 nil,`Store.Required` 拒启。
   - **写者注册**(init 容器 `writer_authority register`):一代写者要在进程配置的**每个**逻辑库各注册一份,注册器先逐库预检,有一个库连不上就一份都不注册、init 原地重试(`cmd/writer_authority/main.go` 的 `slotRegistration.register`)。主容器根本起不来。
   - 新 Pod(发布、扩容、写过之后崩溃被回收重建)都要重新注册,所以**只改常驻进程,新 Pod 在集群失联期间照样卡在 init**。只改这一层能救的只有两种:init 注册完之后集群才失联;主容器还没写过任何库就崩了、原地重启(同一代)。
2. **两层都要改,分两批。** 批 1 是常驻进程(本稿 §2~§6);批 2 是注册层(§7):注册器跳过连不上的库,data_service、go/db 接受写资格的子集,缺的库这一代关着、Pod 不就绪,集群恢复之后重建 Pod 补齐。两批都已落码,未编译。运维口径从「故障期间不要滚动重启」改成:故障期间被回收重建、扩出来的 Pod 照样起得来(缺那几个库、不就绪);发布仍然别在故障期间做(会停在第一个不就绪的 Pod);恢复之后逐个重建不就绪的 Pod(开服手册 §9)。
3. **批 1 的做法**:每个池从一开始就有 `*sql.DB` 句柄,原来开池时的那一串核对整体挪到「这个池第一条物理连接建立之前」,挂在建连钩子上(和逻辑库熔断同一个接缝)。九十多处选池代码一行不改;核对一项不删,只换时刻(§3)。
4. **启动时仍然每个池都试一次。** 结局分三种:核对通过 → 打开;**连不上** → 这个池保持关闭,进程照常启动,以后用到时再核对(失败冷却 10 秒);**连上了但核对不过**(片号对不上、表形状不对、会话不符)→ 照旧拒启。
5. **仍然是启动硬依赖的**:配置、写资格、映射(etcd)、路由的空环境判据。原因和逐项判定在 §4、§5。目录第 0 片原来也是(合服映射只在这一片),2026-11-27 由 [合服映射的 etcd 副本](merged-zone-etcd-copy.md) 去掉:第 0 片读不到时从 etcd 里的副本起。所以现在的准确说法是:**任何一个目录片、流水库所在的集群失联,都不再挡 data_service 启动**;例外只剩 etcd 里还没有副本(全新环境第一次起服就失联)和全新空环境的空环境判据读不全。
6. **数据一致性不受影响。** 一个池在核对通过之前拿不到任何连接,对这个库一条语句都发不出去;调用方看到的是「拿不到连接」,和今天建连失败走同一条路(事务按没开始算,确定没写)。
7. **文件**:批 1 共 26 个(§8.1),批 2 共 28 个(§8.2,其中 6 个批 1 也改过),各自不到 30。

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
| **合服映射**(`zone_merged_into`,只在第 0 片) | 启动时读不到无条件拒启 | 第 0 片读不到时按 etcd 副本起;第 0 片和副本都给不出能用的映射才拒启(2026-11-27,[合服映射的 etcd 副本](merged-zone-etcd-copy.md) §4.4) | 映射是全局的:归属读(`GetPlayerHomeZone`、`BatchGetPlayerHomeZone`、`CountZoneRoles`、`GetMergedZones`)不管玩家在哪一片都要它。一个没有映射的新副本进了发现池,会把别的副本答得了的归属读按比例答成 `Unavailable`,所以没有映射就不进池。副本是全体副本回答过的最高版本(先发布后使用),从它起不会比任何在跑的副本旧,所以可以进 |
| **路由的空环境判据**(`checkEmptyRouteBootstrap`) | 逐片问有没有路由行,遇到读不了的片就报错 | 任一片有行就通过,读不了的片跳过;**一行都没找到而且有片读不了**才报错拒启。之后的角色历史核对不变 | 这道判据防的是「整张路由表被清空或回档」,只要有一片有行就不是。生产上每片都有行,第一片就返回。读不全又一行没找到时,分不清「全空」和「没读到」,保持拒绝;这只会出现在新空环境加集群失联 |
| `ClaimAccountDeletions` | 全片逐片扫,一片失败不影响别的片 | 不变 | 没打开的片就是一片失败 |
| `CountZoneRoles`、`BatchGetPlayerHomeZone`、`BatchGetPlayerName` | 任一片失败整批 `Unavailable` | 不变 | 半份结果会被当成「这些人没有」「这个区远没到上限」 |
| 后台清理(陈旧 ref、删角续做、超时 PREPARING、改名续做) | 逐片、逐轮换起点 | 不变 | 运行期本来就会遇到某一片失败 |
| Kafka 两个消费者(落流水库) | store 缺席时不启动消费 | store 始终装配,消费照常启动;流水库没打开时落库失败,按现有的重试、停在位点处理 | 和运行期流水库失联是同一种情形,不丢不重的保证不变 |
| 审计清理 | 读写流水库 | 不变,失败只记日志 | 同上 |
| 回收通道、角色护栏协调者的本地参与者(流水库) | 回收池打不开 → 协调者不装配,过户、删角在这个进程的整个生命期都被拒 | 回收池句柄始终在,协调者照常装配;流水库没打开时本地参与者回 `retry`,协调者按问不齐处理(`ErrRetryLater`),流水库恢复后自己好 | 少问一个参与者是漏冻结,所以不能跳过;问不到就拒绝是原来的规则,没有放宽 |
| 存档库 | 按需打开 | 不变 | — |

**第 0 片原是批 1 留下的限制,2026-11-27 已去掉。** 比较过每片一份带版本、整张映射搬进 etcd、从同伴取快照三种做法之后,选的是「第 0 片仍是真源、data_service 先把每个新版本发布进 etcd 副本再用、读不到第 0 片时用副本」:合服工具和写路径一个字没改,版本号取合服历史的行数(对比与论证见 [合服映射的 etcd 副本](merged-zone-etcd-copy.md) §3、§5)。部署上第 0 片也不必再留在主集群。

## 5. `Store.Required` 与拒启清单

`Store.Required=true`(生产恒为真)的语义改成:

- **缺任何一个 store 仍然拒启。** store 缺席的原因只剩:配置非法,或者某个池**连上了但核对不过**。
- **某个逻辑库连不上不算缺 store。** store 照常装配,那个库的池保持关闭,用到时再核对。

启动时仍然拒启的完整清单:

1. 配置、调用方密钥、发号器地域码等静态校验(不变)。
2. 映射读不到(`dbplacement.Start`,不变);**任何要写的逻辑库解析不出落点**(映射是坏值,或指向本进程 `SqlClusters` 里没有的集群)。后一条原来是开池时才失败,现在在连任何库之前逐库问一次解析器,当场拒启:它是配置错误,不是集群失联。
3. 写资格缺失、已消费、和要写的逻辑库不是恰好对应(不变)。
4. 任何一个池连上了但核对不过(§3 各项)。
5. 合服映射给不出能用的一份:第 0 片连不上而 etcd 里还没有副本;第 0 片连得上却读不对(权限、缺表,有副本也不顶替);第 0 片与副本对不上(第 0 片的版本更小、同版本内容不同、世代不同);etcd 读不到([合服映射的 etcd 副本](merged-zone-etcd-copy.md) §4.4)。第 0 片连不上而副本在时不拒启。
6. 路由的空环境判据不过,或判不了(§4)。
7. `Recall.Enabled=true` 而资产通道装配不起来(不变)。

拒启时用哪个退出码(§7.5):这一代缺某个目录片的写资格时退出 78,让 Pod 重建、重新注册;别的拒启退出 1,原地重启再试。

**「连不上」的判据只有一个**:`authoritysql.Open` 拿不到第一条可用连接(新哨兵 `authoritysql.ErrUnreachable`),即拨号、握手、会话初始化、写者身份核对任一步没过。档位不符(`ErrBackendMismatch`)是确定的部署错误,不算在内。拿到连接之后的任何失败(包括核对到一半读失败)都按「核对不过」处理,启动时拒启、容器重启再试,和现在集群抖动时的行为相同。不去拆「集群失联」和「账号口令不对、注册不在」:守卫有意不暴露底层错误,拆不出来;这几种都是「对这个库一无所知」,处理也相同——这个库不给用。

**为什么不干脆全部容忍**:连得上而核对不过是确定的部署错误(K 配错、迁移没跑)。带着它启动,发布会一个副本一个副本地把好的换成坏的;当场拒启,滚动发布停在第一个副本。

**容忍「连不上」放宽了什么**(明确接受):运行账号在某个集群上口令不对、被锁,新副本现在会带着那几个关闭的库启动,而不是拒启。两条限制着它:注册器用的是另一个账号但同一组地址和映射,地址、网络策略这类错误仍然先卡在 init;关着的池让副本不就绪,滚动发布停在第一个副本(§6.1,第 0 片也一样)。存档库从 D1 起就是这个行为。批 2 把注册器也放宽之后,这条保护要另外补上,见 §7.3。

**写资格**不变:`LoadWriterConfigs` 一次读齐。注册的核对本来就在每条新物理连接的 `pin` 里,晚打开的库在它第一条连接上核,不需要另找时刻。消费标记在第一条写语句之前写,核对全是只读,不消费。

## 6. 和建连熔断、按映射拨号的配合

- **顺序**:一次建连先过「首次核对」钩子,再过逻辑库熔断,再解析落点、拨号。`authoritysql.WithConnectGate` 现在可以传多次,按传入顺序执行;后一个拒绝时,前一个已经拿到的结算按这个错误结算。
- **没核对过的池不喂熔断**:核对钩子在前,拒绝时熔断的 `Allow` 没被调用。核对用的小池带着同一份选项,它的建连成败照常计入这个逻辑库的熔断;集群失联时熔断打开,之后每次重新核对在建连处被直接挡回,不再各等 1 秒。
- **冷却与熔断各管一段**:核对失败后 10 秒内的调用直接拿到上次的错误(不建连);10 秒后放一次重新核对。两者叠加的最坏情况是集群恢复后最多再等一个冷却期加熔断每秒放行一次的间隔才打开。
- **映射**:小池和业务池用同一个解析器,每次拨号现读。启动时逐库问一次解析器只为把「落点未知」提前成拒启,不改连接行为。
- **规则在一处**:「第一次用到才打开、成功后常驻、失败冷却 10 秒、闸门冻结不进冷却、并发共用一次结果」原来只在 `PlayerStorePools` 里,现在抽成 `openOnce`,存档库连接池和各池的首次核对共用,不再有第二份。
- **等核对时不看调用方的 ctx**(和 `PlayerStorePools.DB` 原来的规则相同):核对的结果被并发请求共享,不能随第一个调用方的取消一起失败。代价是触发核对的那几个请求要等它做完:集群失联时是拨号超时(1 秒),连得上时是几条只读查询;每个池每个冷却期最多一次,冷却期内的请求直接拿上次的错误,不等。不另起协程去「先回错误、核对接着做」:多一个异步结局,换来的只是这一秒。
- **启动多花的时间**:每个连不上的库每个池多等一次拨号超时。分片按 8 路并行开,三个分片 store 依次开:64 片里有 32 片连不上时约多 12 秒,流水库三个池再多 3 秒,在存活探针的宽限(15 秒起、20 秒一次、失败 3 次)之内。

### 6.1 关着的池:后台重核,还关着就不就绪(2026-11-27 第二轮)

**问题**:原先关着的池只在第一次用到时重核。所以运行账号在某个非主集群上配错(口令不对、被锁、授权缺了)而注册账号是好的时,写资格齐全、那几个库的池却一直关着,`/readyz` 照样就绪,滚动发布一路换下去,只能靠那几个库的 ERROR 日志发现。没有流量的库还会一直关着,集群恢复了也不打开。

**做法**:
- 按需核对的池登记进一份名单(`store.LazyPools`,经 `MySQLConfig.LazyPools` 传下去,只有常驻进程给)。
- 后台每 15 秒把还关着的池重核一次(`ServiceContext.StartLazyPoolRetry`,间隔是 `store.LazyPoolRetryInterval`)。每个关着的池每个冷却期最多拨一次号。
  - 间隔比冷却(10 秒)长一半,因为失败的时刻记在核对做完时,比那一拍晚。间隔与冷却一样长的话,每一拍都还差几毫秒没出冷却,实际要两拍才核一次。
  - 多出的 5 秒盖得住拨号超时;核对慢过 5 秒时跳过一拍。
- `Ready` 看名单:有池还关着就回 503,写明是哪几个库;重核通过之后自己变回就绪。
- 关停时先停这个循环,再封写资格、关池(和合服映射的定时重读同一个顺序)。

**为什么判据可以是「关着」**:启动时每个池都先试过一次,连得上的当场打开。所以「关着」只会是启动时连不上;正常环境里不会有池一直关着,不会冤枉好副本。

**和缺写资格的区别**:
- 缺写资格的库(`closedPool`)这一代不会再有写资格,不进名单,不重核;不就绪靠 `MissingWriterGrants`,只能重建 Pod 补齐。
- 关着的池能自己恢复。
- 存档库连接池本来就是第一次用到才打开,没打开不代表有问题,不进名单。

**探针为什么不同步核对**:K8s 探针缺省 1 秒超时,上百个库每个连不上都要等 1 秒拨号,所以探针只读后台核对的结果。

## 7. 批 2:写者注册层(负责人 11-27 同意按 §7.3 两个推荐选项做,已落码,未编译)

### 7.1 改了什么

1. **注册器容忍连不上的库**(`cmd/writer_authority`):
   - 先并发探测各库连不连得上(`slotRegistration.splitReachable`,同时 16 个),连不上的停 2 秒再试一轮。两轮都连不上的库这一代跳过,每个打一行 `writer_authority command=register skipped=<库> reason=unreachable`。
   - 「连不上」只认 `authoritysql.ErrUnreachable`。别的打开错误(映射读不到、配置非法)和连上之后的冻结、资格不符,照旧整体失败、原地重试。
   - 现场生成一代时,跳过的库不算进 `boot_seq`、不进启动记录;启动记录因此可以是模板的有序子集(`recordMatchesTemplate`)。init 重跑时这一代仍只有原来那些库,刚恢复的库不在这一代里注册。
   - 预检时才连不上的库(生成时连得上)同样跳过。它还在启动记录里,init 重跑时连得上就接着注册,号是生成时算好的。
   - 一个库都连不上时什么都不写:不留启动记录,不问工作负载身份,回 `errNothingReachable`,init 原地重试。只写一个库的服务只有这一种结局,行为不变。
   - 目录连接的缓存(`catalogOpener`)加了锁,供并发探测用。探测本来就要逐库打开连接,所有库都连得上时只是并行了,不比原来慢。
2. **常驻进程接受写资格的子集**:
   - data_service:`checkWriterSchemas` 多出来的照旧拒启,少的列出来。缺写资格的库 `PermissionFor` 回 `store.ErrNoWriterGrant`,`MySQLConfig.ForSchema` 在常驻进程里把它装配成一直关着的池(`MySQLConfig.Closed` → `store.closedPool`),存档库连接池对它同样打不开。第 0 片缺写资格时从 etcd 里的合服映射副本起(没有副本时照旧拒启,见 §7.5)。
   - go/db:原来就接受任意存档库的写资格、按需打开时查不到写资格就打不开,行为不用改;只按白名单(`MMORPG_DB_ALLOWED_DATABASES`)算出缺哪几个库(`StoreRegistry.MissingWriterGrants`)。
   - 两边共用 `authoritysql.MissingWriterGrants`,并上报 `writer_grants_missing`。
3. **就绪**:data_service、go/db 的指标端口加 `/readyz`,两个清单的就绪探针改指它。
   - 缺写资格,或者 RPC 还没对外服务(节点注册回调之前),都回 503,正文写明原因。
   - 存活探针仍是 gRPC:不就绪的副本照常服务落在健康库上的请求,不能被当成坏了重启。
4. **告警**:`SqlWriterGrantsIncomplete`(`writer_grants_missing > 0` 持续 5 分钟,warning),说明里写明怎么查缺哪几个库、恢复之后怎么重建。
5. **恢复**:集群恢复后,带着不完整写资格的 Pod 要重建才能拿到完整的一代(开服手册 §9「某个 SQL 集群失联」)。

### 7.2 安全性

- 被跳过的库一个字都没写;这一代在那个库没有注册行,`pin` 每条连接都核注册,所以即使 grant 发错了也连不上。授权的真源是库里的注册行,不是 grant 文件。
- 被跳过的库上,上一代仍是 ACTIVE。上一代的进程已经不在(同一序号同一时刻只有一个 Pod);强删之后旧节点其实还活着的那种僵尸,在连得上的库上被新一代当场围栏,在被跳过的库上要等下一代注册——现在这种情况下新一代根本注册不了,僵尸在所有库上都没被围栏,所以只会更好。
- `boot_seq` 只要求在每个库内递增。取连得上的各库最大号加一;被跳过的库以后由更晚的一代注册,那时取的是全部库的最大号。
- 排空 run 在被跳过的库上会把没有进程的上一代算进 ACTIVE 全集,要人工退休或等 Pod 重建(开服手册「没有进程的写者」,和缩容留下的末代同一处理)。

### 7.3 两个选项(负责人 11-27 定:都选 A)

**选项一:集群恢复之后怎么补上。** 定 A。手册里写的做法是逐个删不就绪的 Pod;`rollout restart` 也行,只是会把本来完整的 Pod 一起换掉。

| | 做法 | 代价 |
|---|---|---|
| **A(推荐)重建 Pod** | 不完整的一代不补注册;集群恢复后滚动重启(`kubectl rollout restart`),新一代在全部库注册 | 恢复要一步人工操作;在那之前这些 Pod 上该集群的库一直不可用 |
| B 常驻的注册容器补注册 | 注册器改成 Pod 里常驻的 sidecar,对没注册上的库一直重试;注册落地后常驻进程下一次核对就打开 | 全自动。但注册账号(能登记、退休写者)从「只在 init 里出现」变成「每个写者 Pod 里常驻」;依赖 K8s 原生 sidecar(1.29 起默认开);十二个写者服务的清单都要动 |

推荐 A:不多出常驻的高权限容器,不引入平台版本依赖;集群级故障的恢复本来就有人在场。

**选项二:降级启动的副本怎么不让滚动发布一路换下去。** 定 A。

注册器放宽之后,「注册账号在某个集群上连不上」既可能是集群失联,也可能是这次发布把地址、口令、网络策略配错了。后一种在现在是 Pod 卡在 init、发布停在第一个副本;放宽之后新 Pod 会带着缺口启动,发布继续,直到所有副本都缺这个集群。必须补一道:

| | 做法 | 代价 |
|---|---|---|
| **A(推荐)缺 grant 的副本不算 Ready** | data_service、go/db 加 `/readyz`(指标端口),有库没有 grant 时回 503;就绪探针改指它。StatefulSet 滚动发布等不到 Ready 就停 | 降级的副本照常经 etcd 被发现、服务健康库(Go 与 C++ 调用方都不看 K8s 就绪);但 `data-service` 这个 ClusterIP 只剩 Ready 的副本,目前只有 gm-admin 的邮件工具走它。PDB 把降级副本算成不可用,节点排空更保守 |
| B 运维显式声明 | etcd 里记「某集群已声明不可用」,注册器和常驻进程只对声明过的集群容忍,没声明的照旧严格 | 不用猜错误原因,发布保护原样保留。但不是自动的:声明之前重启的 Pod 照旧起不来;要加一个声明工具、一条手册、一条「声明挂得太久」的告警 |
| C 只加告警 | 不拦发布 | 不推荐:一次配错就能把一个集群上的库全部做成不可用 |

推荐 A。它同时就是「哪些 Pod 该重建」的标记(选项一 A 的操作对象)。

### 7.4 范围

28 个文件(§8.2):所有写者服务共用的注册器、两个服务的装配与清单探针、共享库的缺口计算与指标、告警及其契约用例、手册。没有动 `k8s_deploy.ps1`:两个服务的指标端口(9260、9160)在它渲染的 ConfigMap 里原本就开着,探针在清单里改。

### 7.5 缺目录片写资格时以 78 退出、重建 Pod(2026-11-27 第二轮)

> **适用范围已收窄(2026-11-27 后续)**:有了 [合服映射的 etcd 副本](merged-zone-etcd-copy.md),缺第 0 片写资格的 Pod 从副本起、不就绪,不再走 78。78 只剩两种:etcd 里还没有副本时缺第 0 片(全新环境第一次起服),以及全新空环境的空环境判据读不全。下面是第二轮当时的分析,判据和代码没变。

**问题**(批 2 带进来的回归):
- 注册器跳过了第 0 片,data_service 读不到合服映射,拒启。
- 拒启之后容器原地重启,但 init 不重跑,拿到的还是同一份缺第 0 片的写资格。集群恢复了也永远起不来,只能人工删 Pod。
- 批 2 之前是 init 反复失败,集群恢复之后自己就过去了,所以这是变差了。
- 空环境判据(全新环境)读不全时也是同一种情形。

**做法**:启动自检没过(`MissingStores`、`MergedZonesUnassembled`)、而这一代又缺某个目录片的写资格时,data_service 以 78 退出(`errRebuildPod`、`exitRebuildPod`)。`sql-writer-pod-reaper` 删掉主容器以 78 退出的写者 Pod,StatefulSet 按同序号重建,新 Pod 的注册器再试,连上了就补齐。

- 78 原本是入口脚本「写资格已消费」用的码,意思都是「原地重启没用,要重建」。reaper 只认这个码,不用改判据,只改了说明和日志。
- 只在缺目录片的写资格时用。目录片是启动时必须读的;缺流水库、存档库的写资格不挡启动(§7.1),别的拒启照旧退出 1,原地重启。
- **代价**:集群一直不恢复时,这个 Pod 大约每分钟重建一次,每次在连得上的库上换一代:每库多一行登记、两份收据,约 2 KB,一个小时约几 MB,永久保留(写者名册换代 §5.8)。有了 etcd 副本之后,只有上面那两种情形才会这样。
- **缺第 0 片写资格时为什么现在可以照常启动**:第二轮当时的顾虑是,第 0 片的池这一代永远打不开,合服映射的定时重读永远失败,集群恢复之后再合服,这个副本会一直按旧映射回答归属。有了 etcd 副本,它每拍跟着副本走:读得到第 0 片的副本先把新版本发布进去,它下一拍采用,所以新合服最多晚一个周期(那份设计稿 §5.3),不会一直旧下去;它也不比任何在跑的副本旧。它仍然不就绪(缺写资格),恢复之后照开服手册 §9 重建。

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

### 8.2 批 2(28 个,已落码,未编译;其中 6 个批 1 也改过)

注册器(2):

1. `go/data_service/cmd/writer_authority/main.go` —— `splitReachable`、`boot` 与 `register` 跳过连不上的库、`recordMatchesTemplate` 接受有序子集、`catalogOpener` 加锁、跳过的库打一行
2. `go/data_service/cmd/writer_authority/main_test.go`

共享(3):

3. `go/shared/authoritysql/config.go` —— `MissingWriterGrants`
4. `go/shared/authoritysql/metrics.go` —— `writer_grants_missing`
5. `go/shared/authoritysql/config_consume_test.go`

data_service(8):

6. `go/data_service/internal/store/mysql.go` —— `ErrNoWriterGrant`、`MySQLConfig.Closed`
7. `go/data_service/internal/store/lazy_open.go` —— `closedPool`
8. `go/data_service/internal/store/lazy_open_test.go`
9. `go/data_service/internal/store/player_store_pools.go` —— 没有写资格的存档库打不开
10. `go/data_service/internal/svc/servicecontext.go` —— `checkWriterSchemas` 接受子集、`Ready`、`MarkServing`
11. `go/data_service/internal/svc/global_caller_secrets_test.go`
12. `go/data_service/internal/metrics/metrics.go` —— `/readyz`
13. `go/data_service/data_service.go` —— 接 `/readyz`,节点注册之后 `MarkServing`

go/db(4):

14. `go/db/db.go` —— 缺口、`/readyz` 判定、节点注册之后才算对外服务;顺带 gofmt 了一处 HEAD 里就没排好的 import
15. `go/db/internal/metrics/metrics.go` —— `/readyz`
16. `go/db/internal/logic/pkg/proto_sql/store_registry.go` —— `MissingWriterGrants`
17. `go/db/internal/logic/pkg/proto_sql/store_registry_test.go`

部署(4):

18. `deploy/k8s/manifests/go-svc/data-service.yaml` —— 就绪探针改 `/readyz`(9260)
19. `deploy/k8s/manifests/go-svc/db.yaml` —— 就绪探针改 `/readyz`(9160)
20. `deploy/k8s/logical-db-alerts.yaml` —— `SqlWriterGrantsIncomplete`
21. `tools/scripts/tests/logical_db_deploy_contract.tests.ps1` —— 规则名单与指标核对

文档(7):

22. `docs/design/data-service-startup-isolation.md`(本稿)
23. `docs/design/data-service-breaker-per-logical-db.md` —— §8
24. `docs/design/horizontal-scale-logical-db.md` —— §2「启动形状」、§11
25. `docs/design/writer-roster-exhaustion-20261106.md` —— §5.6
26. `docs/ops/k8s-open-server-runbook.md` —— §9「某个 SQL 集群失联」
27. `docs/ops/db-move-runbook.md` —— §1「什么时候能用」
28. `docs/PROGRESS.md`

不涉及:proto、生成产物、C++、Java、表结构、`k8s_deploy.ps1`。只写一个库的服务(chat、guild、login 等)只经过注册器那一处改动,行为不变。

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

### 9.1 批 2 的用例

- `shared/authoritysql`:`TestMissingWriterGrantsListsDatabasesWithoutAGrant`。
- `cmd/writer_authority`:
  - `TestRegisterSkipsUnreachableDatabases`:跳过的库不写、不算号、不进启动记录,试了两轮;init 重跑仍不注册它;重建 Pod 之后按全部库取号,在它上面退休上一代。
  - `TestRegisterRetriesAnUnreachableDatabaseOnce`:第二轮连上就照常注册。
  - `TestRegisterWithNothingReachableRegistersNothing`:什么都不写,不问工作负载身份。
  - `TestRegisterFailsWholeOnOtherOpenErrors`:连不上以外的打开错误整体失败。
  - `TestBootRecordMayHoldOnlyPartOfTheTemplateInOrder`:有序子集认,换序、重复、空的不认。
  - `TestSplitReachableProbesConcurrentlyAndKeepsOrder`:并发、不超过上限、保持顺序,要在 `-race` 下跑。
  - 原有的注册用例只改了返回值个数。
- `internal/store`:`TestDatabaseWithoutAWriterGrantStaysClosed`(目录片与存档库;工具照旧当场回错误)。
- `internal/svc`:`TestCheckWriterSchemas`(改写:少的列出来,多的拒绝)、`TestReadyNeedsEveryWriterGrantAndServing`。
- go/db:`TestStoreRegistryListsPlayerStoresWithoutAWriterGrant`。
- 部署契约:`logical_db_deploy_contract.tests.ps1` 的规则名单与指标核对。
- 可选冒烟(kind,两个 SQL 实例,目录 K > 1):停掉不含第 0 片的实例,删一个 data-service Pod;新 Pod 起来、`READY 0/1`、`/readyz` 503 点名缺的库,init 日志有 `skipped=`;落在另一个实例上的账号照常登录。把实例启回来,删这个 Pod,新 Pod 就绪、告警消失。单实例的调试栈做不了。

## 10. 残余与后续

- **故障期间的发布**:新 Pod 一样缺那些库,发布会停在第一个不就绪的 Pod 上(有意的保护)。故障期间被回收重建、扩出来的 Pod 起得来,不就绪;恢复之后逐个重建(开服手册 §9)。
- **第 0 片的硬依赖已去掉**(2026-11-27,[合服映射的 etcd 副本](merged-zone-etcd-copy.md)):第 0 片的集群失联期间,新起的 data_service 副本从 etcd 副本起,和别的库一样关着、不就绪;guild / trade / match 经 `GetMergedZones` 照常拿到映射。
  - 仍然起不来的只剩:etcd 里还没有副本(全新环境第一次起服就失联)、第 0 片读得到却和副本对不上(第 0 片回档、清过表、手改过映射,有意的 fail-closed)、etcd 读不到(节点注册本来也会失败)。
  - 读不到第 0 片的副本,新合服最多晚一个周期;PITR、清库、库重建之后要先停掉全部 data_service 再删副本键,etcd 从快照恢复之后也要删。残余见那份设计稿 §10。
  - 启动时只有第 0 片「连不上」(池关着、缺写资格)才从副本起;连得上却读不对(权限、缺表)照旧拒启,和 §5「连上了而核对不过」同一口径。
- **指标只加了一个**:批 2 的 `writer_grants_missing`。关着的池不单独出指标:每次核对失败记一条 ERROR(带逻辑库名,后台约 15 秒一条),`/readyz` 的正文点名,连接池指标里它的打开连接数是 0。
- **运行账号在非主集群上配错**:2026-11-27 第二轮已修(§6.1)。关着的池进了就绪判定,滚动发布会停在第一个副本上。
- **缩容收尾任务看过了,不改**:集群失联期间它整轮失败(要读每个逻辑库),健康库上被缩掉的末代要等恢复之后才结束。只是推迟,不写坏数据;改成按库容忍要逐库记账,换来的只是早几分钟。
- **go/db 的缺口按白名单算**:名册和 `MMORPG_DB_ALLOWED_DATABASES` 按设计是同一份清单。两者对不上本身就是缺陷(落在没有写资格的存档库上的玩家存不了盘),现在会表现成不就绪。

## 11. 第二轮(2026-11-27)的文件与用例

文件(14 个):
- `go/data_service/internal/store/lazy_open.go`:`LazyPools`、`LazyPoolRetryInterval`;`lazyPool` 登记。
- `go/data_service/internal/store/mysql.go`:`MySQLConfig.LazyPools`。
- `go/data_service/internal/store/lazy_open_test.go`。
- `go/data_service/internal/svc/servicecontext.go`:`StartLazyPoolRetry`、`Ready` 纳入关着的池、`MissingDirectoryShardGrants`、关停顺序。
- `go/data_service/internal/svc/servicecontext_placement_test.go`。
- `go/data_service/data_service.go`:`errRebuildPod`、`exitRebuildPod`、起后台重核。
- `deploy/k8s/manifests/go-svc/sql-writer-pod-reaper.yaml`:说明与日志。
- `deploy/k8s/manifests/go-svc/data-service.yaml`:就绪探针的注释(探针本身不变)。
- `docs/ops/k8s-open-server-runbook.md` §9。
- 关联文档同步第二轮:`horizontal-scale-logical-db.md` §11 的 S1 行、`data-service-breaker-per-logical-db.md` §8、`writer-roster-exhaustion-20261106.md` §5.6、`docs/ops/db-move-runbook.md` §1。
- 本稿。

用例:
- `TestLazyPoolsRetryClosedPoolsInTheBackground`:关着的池在名单里;冷却期内重核不拨号;核对做了 1 秒时下一拍照样重核(间隔改回与冷却同长会红);恢复之后重核打开它,不占业务池的连接;打开之后用到时不再核对;nil 名单安全。
- `TestMissingDirectoryShardGrants`:缺目录片才要重建,缺流水库、存档库不算。

## 12. 这次用到的、值得复用的做法

- **核对挪到「第一条连接之前」,挂在建连钩子上**(§2):句柄从一开始就有,没核对过的池拿不到连接。调用方看到的「没打开」和集群失联是同一种失败,已有的错误处理全部适用,九十多处选池代码一行不改。凡是「启动时要打开很多后端、其中一个坏了不该拖垮全部」的服务都能照搬。
- **核对用一次性的小池,跑的就是当场打开那段代码**(§2):核对规则只有一份;核对到一半,业务也借不到那条连接。
- **结构性的核对不交给调用方**(§2):片号核对收回 `openShardPools` 自己接上,调用方只描述 DSN 和表形状。新加一个分片 store 不会漏。
- **「连不上」与「连上了但不对」分开处理**(§5):前者先关着、过后再试;后者当场拒启。确定的部署错误不会被当成网络问题放过去,滚动发布停在第一个副本。
- **一代写资格可以只覆盖一部分库**(§7.1):授权的真源是库里的注册行,不是 grant 文件。所以少注册几个库不影响安全,只影响可用;缺的库关着、Pod 不就绪,靠重建补齐。
- **启动记录是模板的有序子集**(§7.1):init 重跑时按记录走,不会拿这一代去注册生成时没算号的库。`boot_seq` 只要求每个库内递增,不用全局一致。
- **探测连通性要并发**(§7.1):上百个库一半连不上时,逐个拨号会用完 init 的时限;所有库都连得上时,并发探测顶替了原来逐个打开那一步,不比原来慢。
- **就绪 ≠ 存活,不就绪的副本照常服务**(§7.3):调用方按 etcd 发现、不看 K8s 就绪,所以「不就绪」只用来让滚动发布停下、标记要重建的 Pod,不会把降级副本踢出流量;存活探针仍看 gRPC,不会把它当成坏了重启。
- **探针只读后台结果**(§6.1):要拨号的核对放进后台循环,探针只读结论。探针缺省 1 秒超时,同步去核上百个库会误判;读结论不会。
- **后台循环的间隔要比冷却长一截**(§6.1):冷却从失败的时刻算起,而失败总比那一拍晚。两者一样长时每一拍都差一点没出冷却,重核频率悄悄减半。凡是「定时器 + 失败冷却」叠在一起的地方都有这个坑。
- **退出码表达「原地重启没用」**(§7.5):init 里定下的状态(写资格)只能靠重建 Pod 刷新。用已有的 reaper 和 78 退出码表达这一点,不另造控制器。

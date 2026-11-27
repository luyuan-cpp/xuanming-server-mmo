# 合服映射的 etcd 副本:目录第 0 片失联时 data_service 照样启动

> **状态(2026-11-27,Claude Code)**:设计定稿,两批都已落码,**未编译、未运行**。验证命令见 `docs/PROGRESS.md` 2026-11-27 的两条:「合服映射的 etcd 副本」(第一批:data_service 读侧)、「合服映射的 etcd 副本第二批」(合服工具提交后发布、审计比对副本、共享包)。第二批按 PROGRESS 那一条的命令跑就覆盖第一批。
> **提交情况**:别的会话的快照提交 `24c13a311` 带进了第一批写到一半的 `route_store.go` 和本稿初稿,其余文件在工作区未提交;在它们一起提交之前 HEAD 上 `go/data_service` 编不过(PROGRESS 两条的「提交情况」)。
> **来源**:[启动期故障隔离](data-service-startup-isolation.md) §10 的最后一个启动硬依赖:合服映射 `zone_merged_into` 只在目录第 0 片。
> **证据口径**:静态阅读本仓代码(`go/data_service`、`go/shared/placement`、`go/shared/mergedzone`、`go/shared/dbplacement`、`tools/merge_zone`、`tools/scripts`、`deploy`),没有运行证据。
> **Java 版(AGENTS §12)**:不涉及。Java 版没有 data_service,归属区是单库列,没有映射副本,不加 PARITY 行。

## 0. 结论

1. **推荐方案 D**(§3):目录第 0 片仍是合服映射唯一的真源;etcd 里放一份**副本**,读不到第 0 片时按副本回答,新起的 data_service 读不到第 0 片就从副本起。
   - data_service 每读到一个新版本,**先把它发布进副本,再拿来回答**;
   - 合服工具 `tools/merge_zone` 每次真写提交之后,马上把第 0 片现在的映射发布进副本(第二批)。
2. **写路径的判定一个字没改**:锁、判定、`zone_merge_history`、操作员授权、PITR 语义全部照旧。合服工具只在提交之后多一步「发布」,发布失败也不改变已提交的映射。所以「合服映射的强一致保证不变」是字面意义上的不变。
3. **版本号是白拿的**:合服工具每改一次映射,都在同一个事务里给 `zone_merge_history` 追加恰好一行,历史只增不改(操作员只有 SELECT、INSERT)。所以**历史的行数就是映射的版本号**,一条语句连映射一起读出来就是同一个快照。不加列、不改表、不改 proto。
4. **副本是各写者读到过的最高版本**(先发布后使用,§5)。读不到第 0 片的新副本从它起,不会比任何在跑的副本旧;每个副本回答的版本只增不减。
5. **两个周期照旧成立,而且对所有副本都成立**(§5.3):合服工具提交即发布,读不到第 0 片的副本也在自己的下一个刷新周期内看到;guild、trade、match 再一个周期。今天读不到第 0 片的副本永远拿不到新映射。
6. **顺带得到的保护**(负责人没要求、今天没有的):
   - 第 0 片的版本比副本小(PITR、清表、主从切换丢了提交)→ 拒绝,今天会悄悄退回旧映射;
   - 有人绕过工具手改 `zone_merged_into`(不留历史)→ 同版本内容不同 → 拒绝,今天会被直接采用;
   - 副本是别的目录库的(指错库、库重建过)→ 拒绝;
   - 每个进程记住自己回答过的那一份,版本变小、同版本换了内容、第 0 片换了一个库,也都拒绝,要重启才接受(§5.1 I3);
   - 启动时只有「第 0 片连不上」才从副本起;连得上却读不对(权限、缺表、语句错)照旧拒启,滚动发布停在第一个副本(§4.4);
   - 合服审计直接比对副本和第 0 片,对不上按拒绝退出(§4.5)。
7. **一份规则,两个写者**:读第 0 片的语句、版本的算法、副本的编码、「发不发布」的判定都在 `go/shared/mergedzonecopy`,data_service 和合服工具共用,不会两边各算各的。
8. **取舍**(§6):
   - 合服生效多了一个前提:etcd 可达。etcd 本来就是 data_service 的启动硬依赖(节点注册失败即拒启);合服工具真写也要 `-etcd-hosts`,手册前置条件加 etcd 健康。
   - PITR、清库、库重建之后要**先停掉全部 data_service,再删副本键,再起**;etcd 从快照恢复之后删副本键(data_service 不用停)。不删的话 data_service 拒启并在错误里点名这个键(有意的 fail-closed)。
9. **文件**:第一批 24 个(§8.1),第二批 31 个(§8.2,其中 21 个第一批改过,另有 4 个只是 gofmt);各自不到 30 或是负责人点名「现在修」的那两项。

## 1. 需求分析

| 要求 | 来源 | 本稿 |
|---|---|---|
| 第 0 片所在集群失联时,新起的 data_service 副本能启动、服务健康库 | 任务 | §4:读不到第 0 片时从 etcd 副本起 |
| 合服映射的强一致保证不变 | 任务 | 写路径的判定不改;读侧的不变量见 §5 |
| 合服之后「两个周期内全部生效」不能破 | 任务 | 所有副本都是 2 个周期(§5.3) |
| 拿不准时保持拒启,不为了能启动跳过核对 | 任务 | 副本与第 0 片对不上一律拒绝(§4.3);副本缺席、etcd 读不到都拒启 |
| 不丢数据、强一致、性能高、可水平扩展、代码易懂 | 负责人 | 每拍多一次 etcd 读;不依赖任何一个 SQL 片;规则只有一份 |
| 查出的问题现在修,不等以后 | 负责人 11-27 | 第一批留下的「工具也发布」「审计比对副本」都在第二批做掉 |
| 游戏未上线,不写旧数据迁移或兼容 | 任务 | 副本键第一次读到第 0 片时自动建 |

**原来怎么读映射**:data_service 的 `placement.MergedZoneCache` 启动时读一次第 0 片,之后每 `RouteStore.MergedZoneRefreshSec`(默认 5 秒)重读,失败保留旧值。读不到就拒启(`MergedZonesUnassembled`)。guild、trade、match 每 5 秒调 data_service 的 `GetMergedZones`(`shared/mergedzone`),不连目录。所以只要 data_service 起得来,它们就拿得到。

## 2. 影响面(`zone_merged_into` 全仓 63 个文件命中)

| 类 | 文件 | 方案 D 动不动 |
|---|---|---|
| 唯一的写者 | `tools/merge_zone/{main,merge,route_store}.go` 及测试 | 判定不动;提交之后发布副本、审计比对副本、加 `-etcd-hosts`(第二批) |
| 唯一的 SQL 读者 | `go/data_service/internal/store/route_store.go`、`internal/svc/servicecontext.go`(缓存装配) | 改 |
| 表结构 | `internal/store/schema.go`、`proto/common/database/rollback_database_table.proto` 及各语言生成物 | 不动 |
| 授权 | `tools/scripts/maintenance_drain.ps1`(`MERGE_ZONE_GRANT`)、`deploy/sql/authority_operator_role.sql`、三个授权契约用例 | 不动;开服手册补运行账号读历史表(§6 V5) |
| 清库脚本 | `tools/scripts/currency_crash_window.ps1`、`stress_round19.ps1` | 前者同时删副本键;后者本来清全部 etcd |
| 合服命令包装 | `tools/scripts/dev_tools.ps1` 及契约用例 | 加 `-MergeZoneEtcdHosts`(第二批) |
| 经 RPC 读的 | `go/shared/mergedzone`、guild、trade、match、`proto/data_service` | 不动 |
| 只在注释或文档里提到 | config、metrics、error_codes、server、`k8s_deploy.ps1`、`deploy/k8s/README.md`、`data_consistency_check`、各设计稿与手册 | 不准确的改(§8) |

C++ 不读映射。Java 没有这张表。

## 3. 方案对比

| | A 每片一份带版本的映射 | B 整体搬进 etcd | C 从在跑的同伴取快照 | **D 第 0 片为真源,etcd 放副本** |
|---|---|---|---|---|
| 第 0 片失联时新副本能起 | 能(任何一片可读、没有在途合服) | 能 | 只在还有同伴在跑时 | 能(etcd 有副本) |
| 全部副本一起重启(发布、集群级故障) | 能 | 能 | **不能** | 能 |
| 写一半失败 | 要两阶段提交才不分叉 | 单键 CAS,原子 | 不涉及 | 不涉及:提交点仍是那一个 SQL 事务,副本落后只是还没发布 |
| 历史与映射同事务、只增不改、库侧 `CURRENT_USER()` | 保留(历史只在第 0 片) | 历史进 etcd 就失去库侧授权和身份(本部署 etcd 没开鉴权);留在 SQL 就跨存储、做不成原子 | 保留 | 保留 |
| PITR 后映射与历史仍成对 | 保留 | 改变:SQL 回档不再带回映射 | 保留 | 保留;副本键要删 |
| 合服工具 | 重写:开 K 个片、准备 / 提交 / 补齐、恢复 | 重写成 etcd | 不动 | 判定不动,提交之后发布 |
| 运维变化 | 合服要全部目录片可达;每片授权 | 合服要能连 etcd;删表、改授权、改脚本 | 无 | 合服要能连 etcd;PITR / etcd 恢复后删副本键 |
| 文件数 | 约 35~40,含 proto | 约 45 以上,含 proto 与生成物 | 约 10 | 两批共约 34 个不重复的文件,不含 proto |
| 结论 | 不选 | 不选(记为后续选项) | 不选 | **选** |

### 3.1 A:每个目录片一份带版本的映射

合服工具写全部 K 片,读方取可读片里的最大版本。

- **写到一半就分叉**:工具在第 0 片写成 v7 后崩溃,第 3 片还是 v6。只读得到第 3 片的副本会按 v6 回答,而别的副本按 v7;一个按 v7 回答过的副本重启后读到 v6,还会退回去。所以不能「写全部、读最大」了事。
- **要强一致只能两阶段**:先在每一片记「待提交 v7」,再在第 0 片提交(提交点),最后逐片标已提交;读方取已提交的最大版本,任何一片有比它大的待提交就拒绝。这样读到任何一片都能判断,第 0 片失联也能起。代价:
  - 合服时 K 片必须全部可达;
  - 工具要按 db_placement 解析每一片的集群、同时持 K 个连接;
  - 每次运行都要先按第 0 片补齐、清掉没提交的待提交标记,这些都在第 0 片的锁里做;
  - 每一片多一张表、多一套授权,proto、清库脚本、契约用例都要改。
  这是为一张百来行的表手写一个跨 K 个库的分布式提交协议。
- **用多数派也不行**:两个并发的工具可能各在不相交的少数片上写出同号不同内容的版本,读多数派时取到哪个看运气;要靠回写(ABD)解决,而回写可能让一次已经报成功的合服被后写的覆盖掉。
- 不选:复杂度和出错面都和收益不成比例,不符合「代码易懂」。

### 3.2 B:像 db_placement 那样整体放进 etcd

一个键存整张映射,合服工具按 ModRevision 做 CAS,读方读 etcd。

- **一致性本身最简单**:etcd 线性一致,单键 CAS 就是原子提交,版本就是 ModRevision。etcd 也已经是 data_service 的启动硬依赖,不多依赖。
- **etcd 与 SQL 之间**:映射只引用区号,没有任何 SQL 事务把它和别的表放在一起读(唯一的读者是 data_service 的缓存),所以把它搬出 SQL 不会让业务数据不一致。但有三处改了语义:
  1. **历史**:放进 etcd(和映射同一个 Txn)能保住原子性,却丢了两样 11-11 特意做的东西:操作员账号改不了、删不了历史(本部署 etcd 没开鉴权,`dbplacement.Dial` 和 data_service 的 etcd 客户端都只给地址),以及库侧的 `CURRENT_USER()` 与库时钟(工具只能自报)。留在 SQL,映射和历史就是跨存储的两次写,做不成原子,要再写一套补记、对账的逻辑。
  2. **PITR**:今天整体回档时映射和历史随目录回到同一个 T、仍然成对(PITR 手册 §4.0 第 4 步靠这一点);搬进 etcd 之后 SQL 回档不再带回映射。业务上不致命(映射只放宽同区判定、不决定资产,任意「回档时刻的数据 + 现在的映射」都是合法操作序列能到达的状态),但手册口径要改。
  3. **迁移与回滚**:未上线,不用搬数据;但要删两张表、两个 proto message 及各语言生成物、授权与契约用例、清库脚本,工具整个重写成 etcd。回滚只能 revert;上线之后再回 SQL 要写一次 etcd → SQL 的拷贝。
- 不选:改动最大,而且为了一个只关于「启动」的需求,倒退了历史的可信度和 PITR 的成对语义。**记为后续选项**:等控制面 etcd 开了鉴权,历史可以带着可核验的身份一起搬过去,那时 B 是自然的终态。

### 3.3 C:从在跑的同伴副本取快照

新副本读不到第 0 片时,调一个在跑的 data_service 的 `GetMergedZones`。

- 全部副本一起重启时没有同伴(滚动发布换完最后一个、集群级故障、从 0 扩容),解决不了。
- 同伴的刷新相位不同,可能一个已经是 v7、一个还是 v6,新副本分不清谁旧;同伴自己读不到第 0 片时也在用旧值。
- data_service 依赖 data_service,发现路径成环。
- 不选。但它的思路是对的:新副本只要「不比在跑的旧」就是一致的。D 把这一点落到 etcd 里,全部重启也还在。

### 3.4 D:第 0 片为真源,etcd 里放副本(推荐)

设计在 §4,一致性论证在 §5。选它的理由:
- 提交点和判定不动(锁、历史、授权、PITR),写者只在提交之后多发布一份;
- 版本号来自已有的只增历史,不改表、不改 proto;
- 副本是「各写者读到过的最高版本」,读不到第 0 片的副本从它起不会比在跑的旧,这正是 C 想要、却做不到全部重启的那一点;
- 顺带探测第 0 片回档、丢提交和手改映射;
- 规则集中在一个共享包,可以逐条单测。

## 4. 方案 D 的设计

### 4.1 数据

- **真源**(不变):目录第 0 片的 `zone_merged_into`、`zone_merge_history`、`route_meta`。
- **一次一致读**(`mergedzonecopy.ReadSQL` / `Read`,data_service 的 `RouteStore.ReadMergedZoneMapping`(R14)和合服工具的 `routeStore.Committed` 都调它):一条语句读出 `route_meta.generation`(库世代)、`zone_merge_history` 的行数(版本)和全部映射行。一条语句在 MySQL 上共用一个读视图(RC 下按语句取),在 TiDB 上共用语句的时间戳,所以三样是同一个快照;合服工具的映射与历史同一个事务提交,要么都看到,要么都看不到。
  - **世代**:迁移种 `route_meta` 时随机生成,之后不改;PITR、搬库都带着它走,只有库重建才会变。它用来认出「副本是另一个库的」。
  - **版本**:每次映射变化恰好追加一行历史,历史只增不改,所以同一个版本只对应一份映射。
- **副本**:etcd 键 `/mmorpg/merged_zone_copy/<第 0 片库名>`(`mergedzonecopy.Key`)。第 0 片库名在 `Directory.LogicalShards=1` 时就是目录基名(dev `testdb`),否则是 `<基名>_00`(K8s `mmorpg_global_00`)。按第 0 片库名取,是因为合服工具从 `-route-db-dsn` 拿到的正是这个库名,和 data_service 算出来的(`dirshard.DirectoryDBName(base, 0, K)`)天然一致;两个部署误用同一套 etcd 时也不撞键。
  - 值是 JSON `{"generation":..., "version":N, "merges":[{"src":S,"dst":D},...]}`,按源区升序,同一份映射编出来的字节相同;多余字段不拒绝,区号 0、源区重复、没有世代都拒绝(`zone_merged_into` 没有「区号 ≥ 1」的 CHECK,区号 0 只可能是手写的)。
- **etcd**:data_service 用自己 `Etcd.Hosts` 那一套,就是节点注册用的那一套(K8s 上和 db_placement 是同一套 `etcd.<infra>:2379`);节点注册失败本来就拒启,所以不多一个启动依赖。合服工具用 `-etcd-hosts` 指同一套。
- **写副本的有两方**:data_service(先发布后使用)和合服工具(提交之后发布),都按 ModRevision 做 CAS,都用 `mergedzonecopy.Decide` 判定发不发布。

### 4.2 data_service 每一拍怎么读(启动时一次,之后每个刷新周期一次)

1. **先读副本,再读第 0 片。** 副本里只有第 0 片已经提交过的版本,先读它,后读到的第 0 片就不会比它旧,除非第 0 片真的回档了。反过来读,一次合服夹在两次读之间就会误判成「第 0 片落后于副本」。
2. 副本读不到(etcd 故障、值坏了):这一拍失败。
3. 第 0 片读到了:先和本进程已回答的那一份比(I3,§5.1),再按 §4.3 的表核对。要发布的,按读副本时的 ModRevision 做 CAS;CAS 没成立(别的写者刚发布过)就从第 1 步重来,最多 3 次。发布成功或不用发布,才用第 0 片的映射回答。
4. 第 0 片读不到:按 §4.3 的表决定用不用副本。
   - 启动那一次只认「连不上」:池启动时就连不上、至今关着(`authoritysql.ErrUnreachable`),或这一代缺它的写资格(`store.ErrNoWriterGrant`)。这和按需打开的池同一口径([启动期故障隔离](data-service-startup-isolation.md) §5)。连得上却读不对(1142 权限、1146 缺表、语句错、`route_meta` 缺行)是确定的部署错误,照旧拒启。
   - 运行中的副本任何错误都可以采用副本里更新的版本:那是别的写者从第 0 片读到并发布的,采用它不会比谁旧。照旧回答的各拍都记失败。

「这一拍失败」在启动时就是拒启;运行中是这一次刷新失败、保留旧映射、计 `data_service_merged_zone_refresh_total{result="error"}`。

### 4.3 判定表

| 第 0 片 | 副本 | 结局 |
|---|---|---|
| 读到 T,映射解析不通(成环、超长),或写不进副本(区号 0) | 任意 | 失败,不发布 |
| 读到 T,与本进程已回答的对不上(世代不同、版本更小、同版本内容不同) | 任意 | 失败,不发布;要重启这个副本(只在 data_service 里) |
| 读到 T | 没有 | 发布 T(键必须不存在),用 T |
| 读到 T | 世代不同 | 失败:这个进程指错了库,或目录库重建过。重建过就停掉全部 data_service、删副本键、再起 |
| 读到 T | 版本 < T | 发布 T,用 T |
| 读到 T | 版本 = T,内容相同 | 不写,用 T |
| 读到 T | 版本 = T,内容不同 | 失败:有人绕过工具改了映射(不留历史),或副本被改过 |
| 读到 T | 版本 > T | 失败:第 0 片回档了、清过表,或丢了已提交的合服。错误里点名副本键 |
| 读不到(启动时,不是「连不上」) | 任意 | 失败:连得上却读不对是确定的部署错误 |
| 读不到 | 没有 | 失败 |
| 读不到 | 有,这个进程还没回答过 | 用副本(启动),打一行 ERROR |
| 读不到 | 与已回答的对不上(世代不同、版本更小、同版本内容不同) | 失败(照旧回答已有的) |
| 读不到 | 版本 > 已回答 | 用副本 |
| 读不到 | 版本 = 已回答 | 失败(照旧回答已有的),计一次刷新失败 |

上半张(第 0 片读到了)除「本进程已回答」那一行外就是 `mergedzonecopy.Decide`,合服工具用的是同一个函数。「已回答」指这个进程上一次交给缓存的那一份(世代、版本和映射)。发布之前先用 `placement.NewMergedZones` 和副本的编码规则校验,所以交出去的缓存一定收下,「已回答」与缓存一致。

读不到第 0 片又照旧回答时记成失败,是为了让 `DataServiceMergedZoneRefreshFailing` 照常响:这个副本确认不了真源。只有「副本里有更新的版本」那一拍算成功,失败为主的窗口里告警不会因此消失。

### 4.4 data_service 启动的结局

| 情形 | 原来 | 现在 |
|---|---|---|
| 第 0 片连不上,或这一代缺它的写资格 | 拒启(缺写资格的以 78 退出、重建) | 从副本起;那几个池照旧关着,`/readyz` 不就绪 |
| 第 0 片连得上却读不对(运行账号没有两张表的读权限、表不在) | 拒启 | 拒启(有副本也不顶替):滚动发布停在第一个副本 |
| 第 0 片读不到,etcd 里还没有副本(全新环境第一次起服就失联) | 拒启 | 拒启 |
| 第 0 片读得到,比副本旧 / 同版本内容不同 / 世代不同 | 照第 0 片起 | 拒启 |
| etcd 读不到 | 节点注册失败,拒启 | 拒启 |

所以从副本起的副本一定不就绪:它的第 0 片要么关着(`LazyPools.Closed`),要么缺写资格(`MissingWriterGrants`)。

### 4.5 合服工具:提交之后发布,审计比对副本(第二批)

- **参数**:`-etcd-hosts`(逗号分隔的 host:port,data_service 节点注册那一套 etcd)。真合服、真撤销必须给,缺了在写任何东西之前拒绝(退出码 1);审计给了就比对;dry-run 不用。`dev_tools.ps1` 对应 `-MergeZoneEtcdHosts`,没有缺省值:指错 etcd 时发布会落到别处。
- **发布**(`publishCommitted`):每次真写在锁内提交之后都跑,幂等重跑(「already merged」「nothing to delete」)也跑。先读副本、后读第 0 片(`Committed`,事务外的同一条一致读),按 `Decide` 判定,CAS 冲突重读,最多 3 轮。
  - 映射已经提交,所以这一步的任何失败都按「没做成或结局未知」报(退出码 2),错误写明「the mapping is committed」:data_service 读得到第 0 片的副本会在一个刷新周期内自己发布,重跑同一条命令也能补上。
  - 发布不进入判定:合服的提交点仍是那个事务;发布失败只让读不到第 0 片的副本晚一点看到,不会让谁看到没提交的东西。
  - 为什么不在事务里、提交之前发布:副本里只能有已提交的版本(§5.1 I2)。提交前发布、提交又失败的话,副本里就有一个第 0 片从来没有过的版本。
- **审计**(`auditCopy`):在原有输出之后打第 0 片的世代与版本、副本的世代与版本;副本缺席或落后照常退出(还没发布);对不上(领先、同版本内容不同、世代不同)按拒绝退出,这时 data_service 会拒启或刷新失败;读不到副本是退出码 2。审计从不写副本。

## 5. 一致性论证

### 5.1 三个不变量

- **I1(副本是上界)**:同一个世代里,任何副本回答过的版本都不超过副本键的版本。
  - data_service 读第 0 片的路径:只有副本已经 ≥ T(相等,或者刚 CAS 发布成功)才用 T;
  - data_service 读副本的路径:回答的就是副本本身;
  - 副本键只会被改大:两个写者都只在「键不存在」或「T 比它新」时写(`Decide`),世代不同一律拒绝。
- **I2(副本内容等于真源的对应版本)**:两个写者发布的都是一条语句读出的、已经提交的 T;同一个版本只对应一份映射。
- **I3(一个进程只走一条历史)**:一个 data_service 进程回答的版本只增不减,同一个版本只回答一份映射,世代不变。两条路径都和「已回答」比:第 0 片或副本读到的版本更小、同版本内容不同、世代不同,一律拒绝。这三种只会在回档、删过副本键、指错库之后出现,手里那一份和读到的不是同一条历史,只能重启。I1、I2 在副本键被删过、第 0 片被回档过之后不再成立,I3 让在跑的进程不跟着退,而是报错等重启。

### 5.2 推出来的保证

1. **新副本不比在跑的旧**:读不到第 0 片的新副本从副本起,由 I1,不比任何在跑的副本回答的版本旧;读得到第 0 片的,T ≥ 副本,否则拒启。
2. **每个副本回答的版本只增不减**:I3。
3. **同一个版本处处一样**:由 I2,读第 0 片和读副本的两个副本在同一个版本上回答同一份映射。
4. **滞后窗口和原来一样**:在跑的副本各自按自己的相位刷新,相邻两个副本最多差一个周期;这和原来两个副本相位不同时完全一样。

### 5.3 两个周期还成立吗

合服工具在 t0 提交,随即发布进副本(t0 之后几毫秒;发布失败时退出码 2,见下):
- 读得到第 0 片的副本:t0 之后一个周期内读到第 0 片、回答。
- 读不到第 0 片的副本:t0 之后一个周期内读到副本里的新版本、回答。
- guild、trade、match 经任何一个副本再一个周期。所以**所有副本都是 2 个周期**。
- 工具发布失败(退出码 2,映射已提交):读得到第 0 片的副本照常一个周期内看到并发布;读不到的要等那次发布,最多晚一个周期,即 3 个周期。工具没有报成功,手册要求重跑,重跑就补上发布。
- 没有任何副本读得到第 0 片、工具也发布不了:谁都不回答新版本,全体停在旧版本上,一致;第 0 片恢复后一个周期内补上。

合服手册的两条前置条件还是保留(多一道保险):「data_service 全部就绪」排除从副本起的副本(它们一定不就绪,§4.4);「`DataServiceMergedZoneRefreshFailing` 没在响」排除运行中才和第 0 片断开的副本(它们仍然就绪,但每一拍都记刷新失败)。

### 5.4 和写路径的关系

合服的提交点仍是 `tools/merge_zone` 那个事务;判定仍在 `route_meta` 的行锁里;历史仍和映射同事务。提交之后的发布不参与任何判定:先读副本、后读第 0 片,发布的是那一刻已提交的状态。两个写者(工具与 data_service)同时发布时由 CAS 排队,输的重读;因为两边发布的都是第 0 片的已提交状态,谁赢都一样。

## 6. 漏洞分析

| # | 情形 | 会发生什么 | 处理 |
|---|---|---|---|
| V1 | 整体 PITR,第 0 片回到 T | 第 0 片版本比副本小,data_service 拒启,错误点名副本键;合服工具发布失败、审计按拒绝退出 | PITR 手册 §4.0:data_service 都停着的时候删副本键,再起服 |
| V2 | etcd 从快照恢复 | 副本版本回退。在跑的副本发现「副本落后于已回答」、刷新失败;读不到第 0 片的新副本会按回退的版本起 | etcd 手册 §7a:恢复之后删副本键,读不到第 0 片的新副本于是拒启。没做回写,理由见 §10 |
| V3 | 清库脚本截断历史表 | 同 V1;在跑的副本若回答过大于 0 的版本,按 I3 拒绝、要重启 | `currency_crash_window.ps1` 同时删副本键;`stress_round19.ps1` 本来清全部 etcd |
| V4 | 绕过工具手改 `zone_merged_into` | 同版本内容不同,拒绝。原来会被直接采用 | 合服手册 §5:用工具撤销再合(版本加 2) |
| V5 | K8s 上运行账号没有 `zone_merge_history` 的 SELECT | 读第 0 片报 1142。启动时这是确定的部署错误,有副本也拒启,发布停在第一个副本;已经在跑的副本照旧跟着副本走、告警响 | 开服手册 §1:第 0 片的计划把 `zone_merged_into`、`zone_merge_history` 列成 SELECT。本机启动按表全授,不受影响 |
| V6 | etcd 没开鉴权,副本可被任意改写 | 读得到第 0 片的一方发现「副本领先 / 内容不同 / 世代不同」并拒绝;读不到第 0 片的 data_service 会照着回答 | 和 db_placement、节点注册、热关停同一信任边界,记为残余(§10) |
| V7 | 两个部署共用一套 etcd,或一个副本指错了库 | 键带第 0 片库名;库名也一样时世代不同,读得到第 0 片的一方拒绝(不覆盖),读不到的拒绝(世代与已回答不同);只有读不到第 0 片又刚启动的副本可能用了对方的副本 | 共用 etcd 本来就是错误部署(节点注册也会串)。记为残余 |
| V8 | 多个写者同时发布 | CAS 按 ModRevision,输的重读重判,最多 3 次 | — |
| V9 | 发布的结局未知(etcd 超时) | data_service 这一拍失败、不采用;合服工具退出码 2。下一拍或重跑时读到「版本相同、内容相同」就算发布过了 | — |
| V10 | 第 0 片读得到但很慢 | 3 秒时限内读不完按读不到处理,走副本(启动时这不是「连不上」,拒启) | — |
| V11 | 缓存拒收一份映射 | 发布前同一规则先校验,坏映射既不发布也不交给缓存 | — |
| V12 | 合服工具发布失败 | 读不到第 0 片的副本最多晚一个周期(§5.3) | 退出码 2、文案写明已提交;手册要求重跑同一条命令 |
| V13 | etcd 故障期间合服 | 工具在提交之后发布失败(退出码 2);data_service 也发布不了,就不采用新版本:合服在 etcd 恢复之前不生效 | 合服手册前置条件「etcd 健康」 |
| V14 | 副本键在 data_service 还在跑的时候被删(或第 0 片在它跑着的时候被回档) | 读第 0 片的副本按 I3 拒绝退回去的版本、要重启;不拦的话这个进程会从 v9 退到 v8,再合两次服又会出现同号不同内容的 v9 | 手册里删副本键一律写成「先停掉全部 data_service,删键,再起」;etcd 恢复那一种第 0 片没回退,不用停 |
| V15 | 撤销一行之后映射表仍然解析不通(表里有不止一处坏行) | 撤销已提交,发布按「坏映射不发布」失败,退出码 2 | 合服手册 §5:按审计把坏行都删掉,最后一次撤销就会发布 |

## 7. 运维变化

- **开服手册 §1**:目录第 0 片的计划给 data_service 运行账号列 `zone_merged_into`、`zone_merge_history` 的 SELECT。§9「某个 SQL 集群失联」:第 0 片失联时新 Pod 从副本起,例外只剩「etcd 里还没有副本」「连得上却读不对」「和副本对不上」和「全新空环境的空环境判据读不全」。
- **合服手册**:§3 环境变量加 etcd 地址,前置条件加 etcd 健康、data_service 全部就绪;§4 每条命令加 `-MergeZoneEtcdHosts`,第 4 步判据看工具打的 `published version N` 和审计里副本的版本;§5 加副本相关的故障处置;§11 加场景。
- **PITR 手册 §4.0**:data_service 停着的时候删副本键,再审计、再起服。
- **etcd 手册 §7a**:从快照恢复之后删副本键。
- **搬库手册 §1**:第 0 片不必再留在主集群。
- 看副本:`kubectl -n <infra> exec etcd-0 -- etcdctl get /mmorpg/merged_zone_copy/<第 0 片库名>`,或者合服审计带 `-MergeZoneEtcdHosts`;删副本:同一处 `etcdctl del`。删副本等于声明「以第 0 片为准」,下一个读得到第 0 片的写者会重新发布。
- **第 0 片回档、清表、库重建之后删副本键,要先停掉全部 data_service**:在跑的进程手里是旧历史上的那一份,按 I3 不会跟着退回去,只会一直报错。etcd 从快照恢复那一种第 0 片没有回退,不用停。

## 8. 执行步骤与文件清单

### 8.1 第一批:data_service 读侧(24 个)

1. store:原来的 `ListMergedInto` 换成 `ReadMergedZoneMapping`(R14 一条语句读世代、版本、映射)。
2. svc:新文件 `merged_zone_copy.go`,写 §4.2 的读法;`servicecontext.go` 装配(etcd 客户端随 `upstreamClosers` 关)。
3. 测试、告警说明、配置注释、清库脚本、文档。

代码(11):`go/data_service/internal/store/{route_store.go,route_store_contract_test.go,route_store_test.go,account_directory_shard_tidb_test.go}`,`internal/svc/{merged_zone_copy.go(新),merged_zone_copy_test.go(新),merged_zones_test.go,servicecontext.go}`,`internal/config/config.go`(注释),`etc/data_service.yaml`(注释),`deploy/k8s/data-service-route-alerts.yaml`。
脚本(1):`tools/scripts/currency_crash_window.ps1`。
文档(12):本稿(新)、`data-service-startup-isolation.md`、`horizontal-scale-logical-db.md`、`data-service-breaker-per-logical-db.md`、`player-data-tidb-scaling-plan.md`,开服、合服、PITR、etcd、搬库五份手册,`docs/PROGRESS.md`,`docs/README.md`(索引行由别的会话跑 `gen_docs_index.py` 时带上)。

### 8.2 第二批:共享规则、合服工具发布与审计、键改按第 0 片库名(31 个)

负责人 11-27 要求「查出的问题以后修的现在修」,第一批留下的两项(工具也发布、审计比对副本)在这一批做掉。

共享(2,新):
1. `go/shared/mergedzonecopy/copy.go`:键、`Mapping`、`ReadSQL` / `Read`、编码、`Decide`、`Copies` / `EtcdCopies`。
2. `go/shared/mergedzonecopy/copy_test.go`:`Decide` 判定表、编码、按 ModRevision 的 CAS(替身检查比较的是 ModRevision 相等)、真 etcd 用例(`MERGEDZONECOPY_IT_ETCD`)。

data_service(10):
3. `internal/svc/merged_zone_copy.go`:改用共享包;副本键按第 0 片库名。
4. `internal/svc/merged_zone_copy_test.go`:编码与真 etcd 用例挪到共享包。
5. `internal/svc/merged_zones_test.go`。
6. `internal/svc/servicecontext.go`。
7. `internal/store/route_store.go`:`MergedZoneMapping` 是 `mergedzonecopy.Mapping` 的别名,R14 用共享语句。
8. `etc/data_service.yaml`(注释)。
9~12. 只做 gofmt(查出来的老问题,这次一起修):`internal/svc/directory_events.go`、`internal/logic/recall_review_handled_test.go`、`internal/store/audit_retention_test.go`、`internal/store/schema.go`。

合服工具(6):
13. `tools/merge_zone/main.go`:`-etcd-hosts`、`openCopiesFor`。
14. `tools/merge_zone/merge.go`:`publishCommitted`、`auditCopy`、`execute` 多一个副本参数。
15. `tools/merge_zone/route_store.go`:`Committed`、记下第 0 片库名。
16. `tools/merge_zone/merge_test.go`。
17. `tools/merge_zone/merge_integration_test.go`:真库上核对发布的版本等于历史行数。
18. `tools/merge_zone/go.mod`:加 etcd 客户端(`go mod tidy` 补 go.sum)。

脚本与部署(5):
19. `tools/scripts/dev_tools.ps1`:`-MergeZoneEtcdHosts`。
20. `tools/scripts/tests/dev_tools_merge_zone_contract.tests.ps1`。
21. `tools/scripts/currency_crash_window.ps1`:副本键按第 0 片库名。
22. `deploy/k8s/data-service-route-alerts.yaml`(说明)。
23. `deploy/k8s/README.md`(合服权限那一条)。

文档(8):
24. 本稿。
25. `docs/ops/merge-zone-runbook.md`。
26. `docs/ops/k8s-open-server-runbook.md`。
27. `docs/ops/mysql-backup-pitr-runbook.md`。
28. `docs/ops/etcd-maintenance-runbook.md`。
29. `docs/design/data-service-startup-isolation.md`。
30. `tools/README.md`(merge_zone 那一行)。
31. `docs/PROGRESS.md`。

不涉及:proto、生成产物、表结构、授权渲染、C++、Java、guild / trade / match。

## 9. 单测与验证

- `shared/mergedzonecopy`(`copy_test.go`):`TestDecide`(缺席、落后发布;相同不写;同号不同内容、领先、别的世代、成环、区号 0、没有世代拒绝;文案点名键)、`TestCodec`、`TestEtcdCopiesCompareAndPut`、`TestEtcdCopiesAgainstRealEtcd`(`MERGEDZONECOPY_IT_ETCD` 设了才跑)。
- `data_service/internal/svc`(`merged_zone_copy_test.go`):
  - `TestMergedZoneSourcePublishesBeforeAnswering`:没有副本时先建再回答;版本相同不再写;版本变大按读到的 ModRevision 覆盖;发布失败这一拍不回答新版本。
  - `TestMergedZoneSourceHealsAPublishWhoseAnswerWasLost`:写进去了、应答丢了,这一拍失败,下一拍按已落地的那一版回答、不再写。
  - `TestMergedZoneSourceRefusesWhenShardZeroDisagreesWithTheCopy`:第 0 片落后于副本(错误点名副本键)、同版本内容不同、副本是别的目录库的,都失败且不写;删键之后新进程按第 0 片重新发布。
  - `TestMergedZoneSourceNeverGoesBackWithinAProcess`(I3):删键之后第 0 片退回去、同号不同内容、换了一个库,在跑的进程都拒绝、不发布;读副本那条路上同号不同内容也拒绝。
  - `TestMergedZoneSourceStartsFromTheCopyWhenShardZeroIsUnreadable`:池关着、缺写资格两种都从副本起;之后同版本失败并带出第 0 片的错误;更新的版本采用;更旧的、世代不同的失败;副本缺席失败。
  - `TestMergedZoneSourceDoesNotStartFromTheCopyOnADeterministicError`:启动时 1142 之类的错误不拿副本顶替;在跑的副本照旧采用更新的版本。
  - `TestMergedZoneSourceRetriesAPublishRace`、`TestMergedZoneSourceNeverPublishesABrokenMapping`(成环、区号 0)、`TestMergedZoneSourceFailsWithoutTheCopy`(etcd 读不到时即使第 0 片读得到也失败,而且第 0 片一次都不读)。
  - `merged_zones_test.go`:`TestMergedZonesUnassembled`、`TestNewMergedZoneCacheNeedsTheRouteStoreAndTheEtcdCopy`、`TestLoadMergedZoneCache`(含「连得上却读不对」有副本也不装配)。
- `data_service/internal/store`:`TestRouteReadQueriesShapes` 测 R14(有映射、空映射的 NULL 行、`route_meta` 缺行、半边 NULL);TiDB 用例 `TestTiDBShardedRouteStore` 读 `ReadMergedZoneMapping`。
- `tools/merge_zone`(`merge_test.go`):`TestRealWritesPublishTheCommittedMapping`、`TestPublishFailureAfterCommitIsNotARefusalAndARerunRepairs`、`TestPublishRefusesACopyThatDisagreesWithShardZero`、`TestPublishRetriesWhenAnotherWriterWins`、`TestRealWritesNeedTheEtcdCopy`、`TestRunAudit_ComparesTheEtcdCopy`;`TestParseOptions`、`TestRun_NeverEchoesTheDSN` 跟着新参数改。真库用例 `TestIT_MergeWritesOneRowWithAuditColumns` 核发布的版本等于真库上的历史行数。
- 部署与脚本:`dev_tools_merge_zone_contract.tests.ps1`(新增 etcd 参数用例)、`k8s_deploy_contract.tests.ps1`(告警文件)。
- 冒烟(可选,kind,两个 SQL 实例,目录 K > 1):合服手册 §11「第 0 片失联与 etcd 副本」。单实例调试栈做不了。

## 10. 残余与后续

- **etcd 没开鉴权**(V6、V7):副本和 db_placement、节点注册、热关停一样,信任整个集群网络。开 etcd 鉴权是全控制面的事,另行评估;开了之后 §3.2 的 B 可以作为终态重新考虑。
- **etcd 回档之后不回写**(V2,复核提议过,没采用):让在跑的副本发现「副本比自己回答过的旧」时,把自己那一份按 CAS 写回去,能补上「etcd 从快照恢复、第 0 片又整片失联、忘了删键」这种情形。但同一条规则也会让 PITR 时漏停的旧进程,把回档之前那条历史写回副本,恢复出来的新副本因此按旧历史起。两种都要先违反手册(etcd 恢复要删键;PITR 要先停掉全部服务)。选了不回写:规则少一条,写副本的仍然只有「刚从第 0 片读到的那一份」。
- 第一批记的两项(合服工具发布副本、审计显示副本)已在第二批做掉。

## 11. 这次值得复用的做法

- **把已有的只增日志当版本号**:每次变化都在同一个事务里恰好追加一行、只增不改的表,它的行数就是一个免费的单调版本。和数据一条语句读出来,就是同一个快照。
- **副本是各写者读到过的上界**:读者把自己要用的版本先 CAS 进共享副本再用,写者提交之后也 CAS 进去,副本就成了「已经有人读到过的最高版本」;新读者从它起不会比任何人旧。比「问一个在跑的同伴」强在全部重启也还在。
- **先读副本后读真源**:副本只含真源已提交的版本,这个顺序下「真源落后于副本」只可能是真源回档,不会是两次读的先后造成的误报。写者在提交之后发布时也按这个顺序,判定就能和读者共用一份。
- **副本顺带当真源的回档探测器**:真源版本比副本小,或同版本内容不同,一律拒绝。原来这两种都会被悄悄接受。
- **提交之后再发布**:副本里只能有已提交的版本,所以写者在提交之后发布,失败只按「结局未知、重跑补上」报,不影响已经提交的东西。
- **兜底只兜「连不上」**:启动时拿副本顶替真源,只限真源连不上;连得上却读不对是部署错误,照旧拒启。否则一次配错的发布会一路换完全部副本,只剩告警在响。
- **进程记住自己回答过的整份**:只记版本号挡不住「删键之后另一条历史又走到同一个号」;连内容一起记,同号不同内容才拦得住。
- **规则放共享包,两个写者调同一个函数**:版本的算法(那条 SQL)、编码、判定只有一份,工具和服务不会各算各的。

## 12. 独立复核(2026-11-27)

### 12.1 第一批

写完之后让一个只读子代理只拿代码和需求、不拿本稿的论证去找破绽。它的结论和处理:

| # | 发现 | 处理 |
|---|---|---|
| 1 | 启动时任何 SQL 错误都改走副本:运行账号缺读权限、表不在时新 Pod 照样起来而且就绪,发布一路换完,之后的合服没人发布 | 改:启动只认 `ErrUnreachable`、`ErrNoWriterGrant`(§4.2 第 4 步、§4.4);`TestMergedZoneSourceDoesNotStartFromTheCopyOnADeterministicError` |
| 2 | 真源路径不和本进程已回答的比:在跑时删副本键,进程会从 v9 退到 v8,再合两次服就出现同号不同内容 | 改:I3,两条路径都比世代、版本、内容;删键一律先停 data_service(§5.1、§7、V14);`TestMergedZoneSourceNeverGoesBackWithinAProcess` |
| 3 | etcd 回档之后,新副本可能比在跑的旧 | 不改代码,手册删键;回写的取舍记在 §10 |
| 4 | 世代不同时无条件覆盖:指错库的副本会把别的库的映射写进来 | 改:世代不同一律拒绝,库重建过要停服删键(§4.3、V7) |
| 5 | 区号 0 被报成「发布结局未知」 | 改:发布前按副本的编码规则先校验,报「cannot be published」 |
| 6 | 「写进去了、应答丢了」没有用例;两处替身改值不改 revision | 补 `TestMergedZoneSourceHealsAPublishWhoseAnswerWasLost`;替身加 `set`,改值时 revision 跟着变 |
| — | 「全部就绪 ⇒ 没有走副本的副本」不成立:运行中才断开的副本仍就绪 | §5.3 改成靠两条前置条件一起排除;第二批之后工具提交即发布,这类副本也是 2 个周期 |

### 12.2 第二批

见 PROGRESS 2026-11-27「合服映射的 etcd 副本第二批」的「复核」一项。

# 资产指令重投与终局发件箱按玩家分片(2026-11-10)

> 状态:**已落码,未编译、未运行**,待 Codex 按 §8 验证。对应[服务器代码缺口清单](../handoff/server-gap-audit-20261101.md)第 25 条。
> 第二批(2026-11-11,「G4a Redis 设计稿修订应用」会话):mail 与 GM 回收的重投分片(§10)、guild / trade 终态清理分片(§11)、分片缺口告警(§12),**同样已落码,未编译、未运行**,待 Codex 按 §13.3 验证。
> 复用 `go/shared/workshard`(先例:trade 合同推进,[聚宝斋设计](jubaozhai-market.md) §15.8)与 match 的 `svc.OwnsWork`([全服匹配设计](global-match-hierarchical-pools.md) §5.2、§12.7)。
> 不改表、不改协议、不改 proto,不需要重新生成。

## 1. 问题

| 工作 | 位置 | 现状 |
|---|---|---|
| guild / trade 的资产指令重投 | `go/shared/assetop/reconcile.go` 的 `Loop.Tick` → 各服务 `Store.ListDue` | 每个 stable 副本每 2 秒按 `(next_attempt_ms, op_id)` 取全表前 100 个到期 op_id,再逐行 `Claim`(租约 CAS)。所有副本读到的是同一个队头 |
| match 终局发件箱 | `go/match/internal/authority/outcome_outbox.go` 的 `dueOutcomeResults` / `dueSettlementSources` | 每个 stable 副本每 2 秒读整局结果 8 行、个人首投 4 行、个人等待 4 行,也是同一个队头,逐行 `next_retry_ms` CAS 认领 |

K 个副本每轮共发 K × 100 次领取,真正领到的仍是那 100 行,其余 (K − 1) × 100 次都是空 CAS(`assetop_claim_total{result="lost"}`,match 记 `claim_lost`)。积压超过一批时,每轮处理量封顶在一批:加副本吞吐不涨,只多出写放大。

## 2. 做法

### 2.1 assetop:可选能力 `OwnedDueLister` 与 `Loop.BindWorkOwner`

- **`assetop.OwnedDueLister`**(Store 的可选能力):`ListDueOwned(ctx, nowMs, limit, owned workshard.Filter)`。契约与 `ListDue` 相同(两段读、只回主键、不加锁、按 op_id 去重、总条数 ≤ limit),只多一个条件:`owned` 用 AND 接进**两段** SQL 的 WHERE,放在 `ORDER BY … LIMIT` 之前。零值 `owned` 不过滤。guild 的 `GuildAssetStore`、trade 的 `AssetOpRepo` 实现它;两边的 `ListDue` 改为委托 `ListDueOwned(…, workshard.Filter{})`,SQL 只多一个恒真的 `1=1`,不另写一份。
- **`Loop.BindWorkOwner(kind, owns)`**:`Run` 之前调用一次。Store 没实现 `OwnedDueLister`、`kind` 为空或 `owns` 为 nil 时回错误,装配方拒启:绑了分工却悄悄扫全部行,扩副本时看不出分工没生效。绑上之后 `Tick` 每轮用 `workshard.SQLFilter(owns, kind, "player_id")` 生成条件交给 `ListDueOwned`;没绑照旧调 `ListDue`。
- **只影响 `Tick`**。同步投递(`ProcessOne`)、落地唤醒(`WakePlayer`)、日对账与自动放弃(`RunMaintenance`,本来就选主)、最老未决年龄 gauge 都不看归属。落地唤醒由抢到去重键的任一副本处理某个玩家的全部未决行,与分片无关,保持不变。
- **为什么做成可选能力,而不是改 `Store.ListDue` 的签名**:`assetop.Store` 的生产实现除 guild、trade 外还有 mail(`mail_asset_op`)与 data_service 的 GM 回收(`gm_recall_op`),另有 4 个测试替身(assetop 的 `fakeStore`、assetoptest 的 `singleRowStore`、guild 的 `countingAssetStore` 与 `stubAssetStore`)。改签名要同批改 8 处;第一批只分 guild、trade,第二批给 mail 与回收也实现了它(§10),测试替身仍不需要分工。本包已有同样做法的可选能力(`PlayerPendingLister`、`PendingAgeReader` 等)。代价是 Store 上有两个列到期行的入口,四个生产实现里 `ListDue` 都只是一行委托。
- **新指标 `assetop_owned_shards`**(gauge,只带 `service` 常量 label):本副本本轮负责的片数(0..64)。只有绑了分工的循环才报,没绑 = 没有序列。各 stable 副本加起来应为 64;成员表变化的几秒里会多或少几片;长期少于 64 说明有登记着却不跑循环的副本(§3)。
- assetop 包头「不引 etcd 客户端」的边界不变:本包只用 `workshard` 的纯函数拼条件,成员表由装配方经 `owns` 注入,本包不读 etcd。

### 2.2 片号:`player_id % 64`

- 第 n 片的归属键是 `<kind>:<n>`(`workshard.ShardKey`)。guild 的 kind 是 `guild-assetop`,trade 是 `trade-assetop`。trade 的成员表还给合同推进(`trade-commerce`)和收据 ACK(`trade-receipt-ack`)用,三类键各不相同,各自独立分工。
- 为什么按玩家分:同一玩家同一条流的指令按 seq 递增,按玩家分之后同一玩家的未决行只归一个副本,不会两个副本各推进一部分;落地唤醒也是按玩家处理的。`guild_asset_op`、`trade_asset_op` 都有 `player_id` 列。player_id 是发号器发的打散值(AGENTS §7 不变量 1),取模足够均匀。
- 片数只决定分工,不进主键或持久数据,改它不用迁移。

### 2.3 成员表

**trade**:沿用合同推进那一份 `workshard.Membership`,即 etcd 里本 zone 注册的 stable trade 实例(`TradeNodeService.rpc/zone/<z>/`),每 5 秒整表读一次。`svc.NewServiceContext` 建好成员表之后,把 `members.Owns` 绑到重投循环(`reconcile.Pipeline.BindWorkOwner`)。本副本的身份照旧在 etcd 注册之后由 `BindWorkMember` 补上。成员表的整表读原先放在「有合同引擎」的分支里,现在只要重投循环起就起(`AssetChannel.Start`)。

**guild**:新建一份 `workshard.Membership`,前缀 `GuildNodeService.rpc/zone/<Node.ZoneId>/`。由 `AssetPipeline.AttachWorkShards` 建,在 `Start` 里随重投循环一起起;canary 副本不起重投循环,也不起成员表。guild 在建资产管线之前已经注册了 etcd,绑定时就有本副本的身份(`n.Info.NodeUuid`)。动手前核对过:

- guild 在 etcd 注册(`go/guild/internal/node`)。值是 protojson 的 NodeInfo,带 `nodeUuid` 与 `releaseTrack`;路径是 `GuildNodeService.rpc/zone/<z>/node_type/15/node_id/<n>`,与 `noderegistry.RpcZonePrefix` 同形;`/allocated/` 占位键不在 zone 前缀下。重注册沿用同一个 NodeInfo,uuid 不变。
- K8s:guild 是全局服务,infra namespace 里只有一个 Deployment(`k8s_deploy.ps1` 的 `$GoSvcCatalogue` 里 `Global = $true`)。全部副本用同一份 ConfigMap(同一个 `Node.ZoneId`,即 infra-up 的 `-ZoneId`)、同一个 DSN(Secret 注入的 `MMORPG_GUILD_SQL_DSN`,库名校验固定为 `mmorpg_guild`)、同一套 etcd(`etcd.<infra>:2379`)。
- 本机:`go_services.ps1` 每个 zone 起一份 guild。各 zone 的派生配置只改 `ZoneId`、端口和 `.rpc` 键后缀,MySQL 连接串不改,所以多 zone 时几份 guild 连同一个 `mmorpg_guild`,却注册在不同的 zone 前缀下。每份的成员表里只有自己,各自做全部片,与分片前相同,不会有片没人做。

**成员表的圈法(不变量)**:成员表只能圈住连同一个库、且都在跑重投循环的副本。

- 圈少了(连同一个库的副本注册在别的前缀下):各自做全部片或多做几片,只是重复,正确性仍由 CAS 保证;
- 圈多了(表里有连别的库的副本):落在它名下的片在本库上没人做,行一直 PENDING,直到成员表变化。

所以按 zone 圈:同一个 zone 前缀下的副本出自同一份部署配置。将来若出现「同一 zone 前缀下连不同的库」的部署(例如多个地域共用一套 etcd、ZoneId 又相同),必须先改圈法。

### 2.4 SQL 与索引

- 两段(新行 `attempts < 3`、老行 `attempts >= 3`)都接分片条件,形如 `… AND attempts < ? AND MOD(player_id,64) IN(?,…) ORDER BY next_attempt_ms, op_id LIMIT ?`。列名写死,片号只走绑定参数。全部片都归本副本时条件是 `1=1`;一片都不归时是 `1=0`,MySQL 判 Impossible WHERE、TiDB 出 TableDual,不扫表。
- 索引:扫描走 `idx_0 (status, next_attempt_ms)`,不含 player_id。但 `lease_until_ms`、`attempts` 原本就要回表判,分片条件跟它们在同一步判,每检查一行的代价不变。变的是检查的行数:积压超过 K 个批量时,本副本要跳过别的副本的行才凑满自己的一批,最坏约检查 K × Batch 行;积压不足一批时与分片前一样,把全部到期行看一遍。换来的是写:分片前 K 个副本每轮共 K × Batch 次领取 CAS,分片后每个副本约 Batch 次,几乎都能领到。读比空写便宜,先这样;压测若显示扫描成本可见,再给 `idx_0` 加 `player_id`(要改表定义、重新生成、整批重建表),不在本条做。
- scene 看到的在途上限仍是「副本数 × Workers」(`LoopConfig.Workers` 的注释),不变。积压时总吞吐随副本数增长,scene 承受的总投递量也跟着增长,这正是本条要的效果。

### 2.5 match 终局发件箱

- `authority/work_owner.go` 新增两个类别键。归属照旧由 `svc.OwnsWork` 判,即 etcd 注册的 stable match 实例上的最高随机权重哈希(watch 镜像;成员表为空时全部副本都做):
  - `outcome-result`:整局结果 outbox `match_battle_completion`,片号 = `battle_id % 64`。这张表一局一行,**没有 player_id 列**,所以按 battle_id 分。Kafka 发布的 key 本来也是 battle_id,同一局只由一个副本发;一局只有一行,也不存在「同一实体的多行被两个副本各推一部分」的问题,效果与按玩家分相同。
  - `outcome-settlement`:个人结算 outbox `match_battle_settlement_source`,片号 = `player_id % 64`。首投(`inbox_seq=0`)与等待 APPLIED(`inbox_seq>0`)两页共用一个键:同一玩家的首投和之后的 Lookup 归同一个副本。
- 条件都在 `ORDER BY … LIMIT` 之前;个人那一类一轮只算一次,两页共用。两张表的扫描索引分别是 `(decision, result_published, next_retry_ms, battle_id)` 与 `(dispatch_state, next_retry_ms, battle_id, player_id)`,分片列都在索引里,条件可以在索引上判,不多回表。
- 不变的部分:认领仍是 `next_retry_ms` CAS 改成本轮租约,回写以同一租约为条件;积压读数 `ReadOutcomeOutboxBacklog` 仍读全表(只给 gauge,各副本各读一份,看板取 max);`SettlementLookupDue` 与按需刷新 `RefreshBattleSettlementReceipt` 按单张票查,不扫队头;发件箱只在 stable 副本上跑(`globalBackground`),`MatchNodes` 是 `StableOnly()`,canary 不进成员表。

## 3. 失败与一致性推演

正确性不靠分工:

- 资产指令:领取是 `Claim` 的租约 CAS(`status=PENDING AND lease_until_ms<now`),重排带领取令牌,终结是 `status=PENDING` 的 CAS;scene 对同一 seq 只读答复(不变量 I2)。两个副本同时扫到同一行,只有一方领到。
- 发件箱:认领是 `next_retry_ms` CAS,回写带同一租约;下游按 battle_id / (player, battle) 去重。

| 情形 | 结果 |
|---|---|
| 成员表变化的几秒里各副本看法不一 | 某一片可能两个副本都扫(多几次空 CAS,与分片前相同),也可能暂时没人扫(下一拍补上) |
| 副本崩溃 | 它名下的片停到 etcd 租约过期(guild、trade 的 `LeaseTTL` 都是 60 秒),再加一次整表读(5 秒);match 用 watch,租约过期即换主。只是延迟,不丢 |
| 副本正常停机 | trade 先注销 etcd 再排空(`trade.go`);guild 先停资产管线、后关注册(`guild.go` 的 defer 顺序),停机窗口里它名下的片要等注销之后的一次整表读 |
| 登记着却不跑重投循环的副本(滚动修改 `AssetOp.Enabled` / `Recall.Enabled` 的窗口,或循环卡死) | 它名下的片停到它退出或恢复。兜底:提交后的同步投递照常;落地唤醒任一副本都能处理;`sum by (namespace, service) (assetop_owned_shards)` 低于 64 有告警(§12);卡死的副本还在报旧值、和仍为 64,只能看 `assetop_pending_oldest_age_seconds` 是否持续上涨 |
| etcd 连续 30 秒读不到 | 成员表判过期,本副本做全部片(保活优先) |
| 本副本还没注册完(trade 的循环起得比注册早) | 成员表不空时什么都不做,交给表里的副本;表为空时做全部片 |
| canary 副本 | 不跑重投循环和发件箱,也不进成员表(`workshard.StableMembers` 与 `StableOnly()` 过滤) |
| 落地唤醒落在别的副本上 | 与片的负责者同时处理同一玩家,和分片前一样由 Claim CAS 互斥 |
| 同步投递(提交后的 `ProcessOne`) | 不经 `Tick`,不看归属;没走完的行过了插行租约,由片的负责者接手 |
| 新老版本混跑 | 见 §4 |

## 4. 迁移与混跑

- 不改表、不改协议、不改 proto,不需要重新生成;归属不写进任何持久数据。项目未上线,整批重建部署即可,不做数据迁移;片数、类别键以后要改,也只影响分工。
- 滚动升级时老版本(不分片)扫全部片,新版本只扫自己的片。老副本仍覆盖全部片,不会有片没人做,只是重复;全部换成新版本后分工才生效。回滚同理。

## 5. 不做的部分与原因

1. mail(`mail_asset_op`)与 data_service 的 GM 回收(`gm_recall_op`)的重投循环:第一批没做,第二批已分片,圈法与理由见 §10。
2. 不给 `idx_0` 加 `player_id`:理由见 §2.4,等压测证据。
3. 不缓存「本副本没有片」,也不跳过 `1=0` 的查询:成员表随时会变,一次空查询的代价可以忽略。
4. 落地唤醒、同步投递、日对账与自动放弃、最老未决年龄 gauge 都不改。
5. 已建房票巡检(`ReadyAfter`)按 2026-11-06 的约定由数据覆盖审计会话接分片([全服匹配设计](global-match-hierarchical-pools.md) §5.2),本条不碰。
6. trade `asset_op_repo.go` 的 `Finalize`、`Abandon`、`ResolveManually` 由另一项改,本条只动 `ListDue` 一段。
7. 告警规则:第一批没加,第二批已加,见 §12。式子按 data_service 随 zone 部署改成 `sum by (namespace, service) (assetop_owned_shards) < 64`。
8. 终态行清理每个 stable 副本都跑(guild `guild.go` 的 `startAssetOpCleanup`、trade `svc.StartAssetOpCleanup`):第二批已按 `player_id` 分片,不改单领导者,理由见 §11。

## 6. 改动清单

| 文件 | 改动 |
|---|---|
| `go/shared/assetop/reconcile.go` | `OwnedDueLister`、`Loop.BindWorkOwner`、`Tick` 经 `listDue` 选入口、`ownedShardCount` |
| `go/shared/assetop/metrics.go` | `assetop_owned_shards` |
| `go/shared/assetop/types.go` | 包头边界补一句:分片只用 workshard 的纯函数 |
| `go/guild/internal/data/asset_store.go` | `ListDueOwned`;到期扫描的 SQL 拆成两段头加公共尾;`ListDue` 委托 |
| `go/guild/internal/svc/asset_op.go` | `AttachWorkShards`、成员表随 `Start` 起 |
| `go/guild/guild.go` | 建管线之后调 `AttachWorkShards` |
| `go/trade/internal/data/asset_op_repo.go` | `ListDueOwned`;`ListDue` 委托(只动这一段) |
| `go/trade/internal/reconcile/pipeline.go` | `Pipeline.BindWorkOwner`,类别键 `trade-assetop` |
| `go/trade/internal/svc/servicecontext.go` | 成员表绑到重投循环 |
| `go/trade/internal/svc/assetchannel.go` | 成员表整表读不再只在合同引擎分支里起 |
| `go/match/internal/authority/work_owner.go` | 类别键 `outcome-result`、`outcome-settlement` |
| `go/match/internal/authority/outcome_outbox.go` | 两个到期读接分片条件 |
| `docs/design/global-match-hierarchical-pools.md` §5.2 | 终局发件箱从「不在这条规则里」改为按片分工的一项 |
| `docs/design/jubaozhai-market.md` §15.8 | 成员表一节补一句:资产指令重投复用同一份成员表 |

测试文件见 §7。

## 7. 测试清单

全部没有运行。

- `go/shared/assetop/reconcile_shard_test.go`(纯单测):没绑分工走 `ListDue`、不报 gauge;绑了只走 `ListDueOwned`,条件是 `MOD(player_id,64) IN(?,?)`、片号按升序走绑定参数,gauge 等于负责的片数;全部片 / 一片都不归时条件分别是 `1=1` / `1=0`;Store 不支持、kind 为空、owns 为 nil 时拒绝绑定,拒绝之后仍按 `ListDue` 扫;落地唤醒不看归属。
- `go/shared/workshard/workshard_test.go`:分散回归的前缀清单加上四个新类别键。
- `go/guild/internal/data/economy_repo_test.go`:纯单测 `TestListDueQueryPutsShardFilterBeforeLimit`(两段的条件都在 `ORDER BY` 之前、占位符个数);真库 `TestListDue_OwnedShardsOnly`(别的片更早到期也不占名额、老行那一段同样过滤、零值条件与 `ListDue` 相同、一片都不归时回空)。
- `go/guild/internal/svc/asset_op_test.go`:`TestAttachWorkShards`(成员表前缀、Store 不支持 / 没有身份 / Start 之后都拒绝、绑上之后 Start 与 Stop 正常、nil 管线安全)。
- `go/trade/internal/data/asset_op_due_shard_test.go`(录制驱动,不连库):两段 SQL 的文本与实参顺序;`ListDue` 等于不带条件的 `ListDueOwned`。
- `go/trade/internal/data/asset_op_due_shard_integration_test.go`(`integration` 标签,真库):两个重投循环按玩家片互补分工,各自只投自己片里的行,合起来每行恰好投一次、全部终结;一片都不归的循环一行都不领。
- `go/trade/internal/reconcile/pipeline_test.go`:`TestBindWorkOwner`(owns 为 nil 拒绝、nil 管线拒绝、正常绑上)。
- `go/match/internal/authority/work_owner_test.go`:真 TiDB `TestWorkTiDBOutcomeOutboxDeliversOnlyOwnedShards`(结果只投本副本的 battle 片,个人首投与等待两页只投本副本的玩家片;一片都不归时什么都不投;不分工时其余行投出,已投的不重投)。
- 既有用例不改:`TestOutcomeTiDBConcurrentReplicasDeliverEachRowOnce` 两个副本都没绑归属,仍是「全部片 + CAS 只投一次」。

## 8. 给 Codex 的验证命令

工作目录都是仓库根目录下对应的 Go 模块,PowerShell 7;每条命令前先 `$env:GOTOOLCHAIN='local'; $env:GOPROXY='off'`。按顺序跑,失败时保留编译错误全文、失败用例名与断言原文,不改弱断言,不连续重试。

1. `cd go/shared; gofmt -l assetop workshard; go vet ./assetop/... ./workshard/...; go test ./assetop/... ./workshard/... -count=1`
   通过标准:gofmt 无输出,vet 无报错,全部 PASS,其中有 `TestTickWithoutWorkOwnerListsEveryShard`、`TestTickWithWorkOwnerListsOnlyOwnedShards`、`TestTickWorkOwnerEdgeShards`、`TestBindWorkOwnerRejectsWhatCannotShard`、`TestWakePlayerIgnoresWorkOwner`。
2. 依赖 `shared/assetop` 的其余模块都要能编(assetop 新引了 `shared/workshard`):对 `go/guild`、`go/trade`、`go/mail`、`go/data_service`、`go/player_inbox` 各跑 `go build ./...; go vet ./...`。通过标准:全部无报错;vet 若报的是别的会话正在改的文件,写明文件名,不算本条失败。
3. `cd go/guild; gofmt -l guild.go internal/svc internal/data; go test ./internal/svc/... ./internal/data/... ./internal/logic/... -count=1`
   通过标准:全部 PASS(没设 `GUILD_TEST_MYSQL_DSN` 时真库用例 Skip,另算),`TestAttachWorkShards`、`TestListDueQueryPutsShardFilterBeforeLimit` 在列。
4. guild 真库(一次性 MySQL 8.0.29+ 或 TiDB,库名只许 `guild_test` / `guild_it_<pid>_<n>`):`$env:GUILD_TEST_MYSQL_DSN='<DSN>'; go test ./internal/data/ -count=1 -run 'ListDue'`
   通过标准:`TestListDue_FreshRowsFirst` 与 `TestListDue_OwnedShardsOnly` 都 PASS、没有 Skip。
5. `cd go/trade; gofmt -l internal/data internal/reconcile internal/svc; go test ./internal/data/... ./internal/reconcile/... ./internal/svc/... -count=1`
   通过标准:全部 PASS,`TestListDueOwnedPutsShardFilterBeforeLimitInBothSegments`、`TestListDueIsUnshardedListDueOwned`、`TestBindWorkOwner` 在列。
6. trade 真库(一次性库):`$env:TRADE_TEST_MYSQL_DSN='<DSN>'; $env:TRADE_REQUIRE_MYSQL_TESTS='1'; go test -tags integration ./internal/data/ -count=1 -run 'Tick_ReplicasSplit|WakePlayer_EndToEnd'`
   通过标准:两个用例都 PASS、没有 Skip。
7. `cd go/match; gofmt -l internal/authority; go vet ./...; go test ./internal/authority/... ./internal/svc/... -count=1`
   通过标准:全部 PASS(没设 `MATCH_IT_TIDB` 时 TiDB 用例 Skip,另算)。
8. match 真 TiDB(一次性 TiDB,清库后由用例自己建表):`$env:MATCH_IT_TIDB='127.0.0.1:<端口>'; $env:MATCH_IT_REQUIRE_TIDB='1'; go test ./internal/authority/ -count=1 -run 'Work|Outcome'`
   通过标准:`TestWorkTiDBOutcomeOutboxDeliversOnlyOwnedShards` 与既有的 `TestOutcomeTiDB*`、`TestWorkTiDB*` 全部 PASS。
9. 多副本冒烟(本机,清空 Redis / MySQL / etcd 之后;trade 要 `AssetOp.Enabled=true`):guild、trade 各起 2 个副本(同一 zone),造一批资产指令积压(例如让 scene 暂时拒绝投递),观察:
   - 每个服务两个副本的 `assetop_owned_shards` 之和为 64;
   - 积压排空期间 `assetop_claim_total{result="lost"}` 基本不涨(分片前每轮约涨一个批量);
   - 停掉一个副本,约 65 秒内另一个副本的 `assetop_owned_shards` 涨到 64,积压继续下降;
   - match 起 2 个副本,造几局结束的对局,两边日志里投递的 battle_id 不重复。

## 9. Java 版

服务端内部改动,没有客户端契约。Java 版只登记待做(负责人 2026-11-05 起定 Java 留作 todo):核对 Java 的帮会 / 交易资产重投与匹配终局发件箱是否每个副本都扫同一个到期队头,是的话按同一口径分片(成员表为空时全做、本实例不在表里时不做、片号按玩家,结果行没有玩家列时按局号),条件放在 SQL 的 LIMIT 之前。第二批同样只登记待做:邮件附件与 GM 回收的重投、帮会与交易的终态清理按同一口径分片(成员表只圈连同一个库、都在跑这项工作的实例,圈不准宁可不分),以及「各实例负责片数之和应为 64」的告警。

## 10. 第二批:mail 与 GM 回收的重投分片

### 10.1 动手前的核对(两个服务的副本连不连同一个库)

**mail**:

- K8s 上是全局服务(`k8s_deploy.ps1` 的 `$GoSvcCatalogue` 里 `Global = $true`),infra namespace 一个 Deployment。全部副本同一份 ConfigMap(同一个 `ZoneId`)、同一个独占库 `mmorpg_mail`;`DbPlacement` 搬库时全部副本跟着同一份映射走,连的仍是同一个逻辑库。
- 注册由 `shared/domainhost` 做:路径 `MailNodeService.rpc/zone/<ZoneId>/node_type/<t>/node_id/<n>`,值是 protojson 的 NodeInfo,`releaseTrack` 由 `shared/noderegistry` 补上,所以 canary 能被 `workshard.StableMembers` 过滤掉。
- stable 副本都跑重投循环(没有开关);canary 不跑重投、清理,第二批起也不跑成员表。
- 本机:`go_services.ps1` 每个 zone 起一份,连同一个库、注册在不同的 zone 前缀下,与 guild 相同:每份的成员表里只有自己,各自做全部片,只是重复。

**data_service 的 GM 回收**:

- `gm_recall_op` 在流水库 `<SnapshotMySQL.DBName>_ledger`(`dirshard.LedgerDBName`)。K8s 上它是 `mmorpg_global_ledger`,全集群一份,**各 zone 的 data_service 都连它**。
- 但 data_service 随 zone 部署:每个 zone namespace 一个 Deployment、一份 ConfigMap,注册在 `DataServiceNodeService.rpc/zone/<z>/`。`Recall.Enabled`(K8s 上取部署开关 `AssetOpsOn`)与库名都是各 zone 自己的配置。
- 回收通道在每个副本上都起,不看发布轨道(canary 也跑);`Store.Required=true`,回收存储装配不起来的副本拒启,所以同一 zone 里「登记着却没有回收循环」只会出现在滚动修改 `Recall.Enabled` 的窗口。

### 10.2 圈法

- **mail 按 zone 圈**:`MailNodeService.rpc/zone/<ZoneId>/`,理由同 guild(§2.3)。
- **回收按 zone 圈,不圈全服**:全服各 zone 连的是同一个流水库,圈全服在今天的 K8s 部署里是准的(每一片只有一个副本扫);但它要假定所有 zone 的配置一致,哪个 zone 关了回收、晚升级或连了别的库,落在它名下的片就没人做,这正是 §2.3 说的「圈多了」。按 zone 圈,同一前缀下的副本出自同一份配置,只会「圈少了」:每一片由每个 zone 各一个副本扫,Z 个 zone 就是 Z 份,比分片前的「全部 Z × R 个副本都扫全部行」少,且不会漏片。正确性仍由 Claim 的租约 CAS 保证。
- **回收的 canary**:跑循环但不进成员表(`StableMembers` 过滤)。成员表里有 stable 副本时 canary 一片都不扫,成员表为空时扫全部片。这与发布轨道「canary 不跑全局后台」一致,但回收的派发与落地唤醒仍在 canary 上跑(第二批不改)。

### 10.3 做法

- **mail**:`Repo.ListDueOwned`(`internal/data/asset_store.go`),两段 SQL 都由 `listDueQuery` 拼,分片条件在 `ORDER BY … LIMIT` 之前;`ListDue` 委托它。重投循环的 Store 是值类型的 `StoreAdapter`(内嵌 `*Repo`),方法经内嵌指针提升,`BindWorkOwner` 的类型断言能过。类别键 `mail-assetop`。`mail.go` 建成员表、绑到循环;stable 上成员表先于循环起,停机时 `closeRelay` 与节点镜像、循环一起等它退出。
- **mail 的身份**:注册在 `domainhost` 里,mail 原来拿不到 node_uuid。`domainhost.ServeWithReady` 的回调改为 `func(nodeUUID string)`,注册成功后把 uuid 交给调用方(进程内不变,失租换号也不变);mail 在回调里 `members.SetSelf`,rank 忽略参数。注册之前成员表若已有别的副本,本副本一片都不做,与 trade 相同。
- **mail 的指标**:mail 原来给 `assetop.NewLoop` 和 `Caller` 传的 metrics 是 nil,assetop_* 一条序列都没有(包括最老未决年龄)。第二批改为 `assetop.NewMetrics(prometheus.DefaultRegisterer, "mail")`,同一个句柄交给 Loop 与 Caller;domainhost 的 `/metrics` 暴露的正是默认注册表。从此 `AssetOpUnknownOutcome` 等既有规则也覆盖 mail。
- **回收**:`RecallStore.ListDueOwned`(`internal/store/recall_store.go`,`recallListDueQuery` 拼两段);`ListDue` 委托它,limit 非正时不发 SQL。类别键 `gm-recall-assetop`。`newRecallChannel` 建成员表并 `BindWorkOwner`,绑不上就回错误(`Recall.Enabled=true` 时 `configureRecall` panic,拒启)。`Start` 里成员表先于循环起(点位 `data_service.recall.members`,panic 时 Hold)。本副本的 uuid 由 `startNodeRegistration` 的新参数 `onRegistered` 在注册成功后交给 `ServiceContext.BindRecallWorkMember`;没装配回收时是空操作。
- **不做**:回收的派发循环(`ListPlanned` → `Dispatch`,PLANNED → PENDING)每个副本仍读同一个队头,靠 `Dispatch` 的状态 CAS 互斥;mail 的过期邮件清理(`maintenance.Run`)每个 stable 副本都跑。两者都不在第 25 条里,量都按周期跑;要分时照本节同一写法。

## 11. 第二批:guild / trade 终态清理去重

### 11.1 分片还是单领导者

先看清理 SQL 能不能把片条件放在 `LIMIT` 之前:

- guild 一轮清三类,候选读都是「普通读 + 若干 AND 条件 + `ORDER BY … LIMIT`」,三张表都有 `player_id`:终态指令(`guild_asset_op`)、客户端幂等键去重记录(`guild_client_request`)、过期日键 / 周键计数行(`guild_daily_counter`);
- trade 只有终态指令一类(`trade_asset_op`),形状相同。

都能放,所以**分片**,复用重投那份成员表按 `player_id` 分,不改单领导者。不选单领导者的理由:

- 清理与资产通道开关独立(通道关着时历史终态行照样要清)。guild 的维护选主只在开了对账或放弃时才竞选;trade 的清理连共享 Redis 都不拨。改成单领导者,两边都要新增一把锁和一条通道关着时也要起的选主。
- 单领导者把清理吞吐封顶在一个副本上(每轮至多 20 批 × 500 行);分片后每个副本只读、只删自己那份,总吞吐随副本数增长。

### 11.2 做法

- `data.CleanupConf` 多一个 `Owns func(key string) bool`(guild、trade 各一份),nil 表示不分工、清全部行。类别键另起:`guild-assetop-cleanup`、`trade-assetop-cleanup`(`data.CleanupWorkKind`),与重投、合同推进、收据 ACK 都不同名,同一片的重投与清理不必落在同一个副本上。
- 片的归属每轮开头算一次(`workshard.SQLFilter(c.Owns, CleanupWorkKind, "player_id")`),一轮里不变。候选读拆成「头 + 分片条件 + 尾」,条件在 `ORDER BY … LIMIT` 之前;不带后缀的常量(`sqlListCleanupTerminalOps` 等)= 头 + `1=1` + 尾,仍是不分片的形状,给「候选读不加锁」的静态检查与既有用例。
- 成员表来源:guild `AssetPipeline.CleanupOwner()`,trade `ServiceContext.AssetOpCleanupOwner()`,都返回重投那份 `workshard.Membership` 的 `Owns`。成员表只在资产通道开着时才有;通道关着时回 nil,清理与分片前相同。trade 的 `StartAssetOpCleanup` 多一个 `owns` 参数,`data.AssetOpRepo` 新增 `CleanupTerminalOpsOwned`,`CleanupTerminalOps` 委托它。
- 点锁 + 带原条件复核的点删、分批、批间停顿、每轮上限都不变。

### 11.3 不变量与失败推演

- 圈法:清理只在 `CleanupEnabled` 且 stable 的副本上跑,成员表是本 zone 注册的 stable 副本,与重投同一份配置。滚动修改 `CleanupEnabled` 的窗口里,还没开清理的副本名下的片这几轮没人清,只是终态行多留,不会误删:删除仍在主键点锁之后按状态与判龄条件复核。
- guild 的清理水位(`asset_op_cleanup_watermark.go`)仍由每个副本在删本副本的片之前推进:它是只增的「这以下可能已删」,哪个副本先删都先写它,分片不改变 write-ahead 的论证;某些片暂时没删只让水位偏保守。trade 没有回档检查消费本表,不写水位(不变)。
- 成员表变化的几秒里某片可能两个副本都清(与分片前相同,点锁排队、点删复核落空)或暂时没人清(下一轮补上)。

## 12. 第二批:分片缺口告警

规则在 `deploy/k8s/asset-channel-alerts.yaml` 的 `asset-channel.work-shards` 组,契约测试 `tools/scripts/tests/assetop_work_shard_alerts_contract.tests.ps1`。

| 规则 | 式子 | for | 级别 |
|---|---|---|---|
| `AssetOpOwnedShardsMissing` | `sum by (namespace, service) (assetop_owned_shards) < 64` | 5m | warning |
| `AssetOpOwnedShardsMissingBacklogAging` | 上式 `and on (namespace, service) (max by (namespace, service) (assetop_pending_oldest_age_seconds) > 1800)` | 30m | critical |

- **按 (namespace, service) 聚合**:§5 第 7 条原式只按 service。data_service 每个 zone namespace 一份、成员表只圈本 zone,全服加起来是 Z × 64;guild、trade、mail 只在 infra namespace,加上 namespace 结果不变。
- **与最老未决年龄配合**:片没人扫时,在线玩家没走完的行停在那里,最老年龄会持续上涨。离线停放行本来就让最老年龄常年偏高,所以第二条在实践中等于「缺片持续 35 分钟以上」的升级:成员表不会自己变,缺口不会自愈,trade 的合同推进与收据 ACK 用同一份成员表,托管资产也会停在合同里,要人处置。
- **高于 64 不告警**:只是重复扫(某副本连续 30 秒读不到成员表就做全部片),正确性由 CAS 保证。
- **盲区**:循环卡死的副本 gauge 停在旧值,和仍为 64,两条都不响,只能看最老未决年龄;要补需要一个每轮推进的活性指标,本批不加。
- **mail 的序列**:第二批之前 mail 没有任何 assetop_* 序列(§10.3),规则对 mail 永远不响;契约测试钉住四个绑了分工的循环都拿到了 metrics。

## 13. 第二批改动清单、测试与验证命令

### 13.1 改动

| 文件 | 改动 |
|---|---|
| `go/shared/assetop/reconcile.go`、`metrics.go` | 注释:四个生产 Store 都实现了 `OwnedDueLister`;gauge 的告警口径 |
| `go/shared/domainhost/host.go` | `ServeWithReady` 回调带上 node_uuid |
| `go/rank/rank.go` | 回调签名跟着改,忽略参数 |
| `go/mail/internal/data/asset_store.go` | `ListDueOwned`、`listDueQuery`;`ListDue` 委托 |
| `go/mail/mail.go` | 成员表、`BindWorkOwner(mail-assetop)`、assetop 指标、身份回调、停机等成员表退出 |
| `go/data_service/internal/store/recall_store.go` | `ListDueOwned`、`recallListDueQuery`;`ListDue` 委托 |
| `go/data_service/internal/svc/recall_channel.go` | 成员表、`BindWorkOwner(gm-recall-assetop)`、`BindRecallWorkMember` |
| `go/data_service/data_service.go` | `startNodeRegistration` 注册成功后回调 uuid |
| `go/guild/internal/data/asset_store.go` | `CleanupConf.Owns`、`CleanupWorkKind`;三条候选读拆头尾,分片条件在 LIMIT 之前 |
| `go/guild/internal/svc/asset_op.go`、`go/guild/guild.go` | `AssetPipeline.CleanupOwner`;清理接上它 |
| `go/trade/internal/data/asset_op_repo.go` | `CleanupConf.Owns`、`CleanupWorkKind`、`CleanupTerminalOpsOwned`;候选读拆头尾(只动清理一段,`Finalize` / `Abandon` / `resolveManually` 没碰) |
| `go/trade/internal/svc/assetchannel.go`、`servicecontext.go`、`go/trade/trade.go` | `StartAssetOpCleanup` 多 `owns` 参数;`AssetOpCleanupOwner` |
| `deploy/k8s/asset-channel-alerts.yaml` | `asset-channel.work-shards` 组两条规则;文件头 service 取值 |

### 13.2 测试(都没有运行)

- `go/shared/workshard/workshard_test.go`:分散回归加 4 个类别键(`mail-assetop`、`gm-recall-assetop`、`guild-assetop-cleanup`、`trade-assetop-cleanup`)。
- `go/mail/internal/data/asset_store_test.go`(新):`TestListDueQueryPutsShardFilterBeforeLimit`、`TestListDueOwnedRejectsLimitBeforeSQL`、`TestStoreAdapterBindsWorkOwner`。
- `go/data_service/internal/store/recall_store_test.go`:`TestRecallListDueQueryPutsShardFilterBeforeLimit`、`TestRecallListDueOwned_NonPositiveLimitSkipsSQL`;`recall_store_integration_test.go`(`integration` 标签,真库):`TestRecallStore_ListDueOwnedReadsOnlyOwnedShards`。
- `go/data_service/internal/svc/recall_channel_test.go`:`TestRecallShardMembersPrefix_IsTheZoneRegistrationPrefix`、`TestRecallLoopBindsWorkOwner`。
- `go/guild/internal/data/economy_repo_test.go`:纯单测 `TestCleanupCandidateQueriesPutShardFilterBeforeLimit`;真库 `TestCleanupOnce_OnlyOwnedShards`。`go/guild/internal/svc/asset_op_test.go`:`TestCleanupOwner`。
- `go/trade/internal/data/asset_op_due_shard_test.go`:`TestCleanupTerminalOpsOwnedPutsShardFilterBeforeLimit`(录制驱动)。`go/trade/internal/svc/assetchannel_recovery_test.go`:`TestAssetOpCleanupOwner`,两处 `StartAssetOpCleanup` 调用补 `nil`。
- `tools/scripts/tests/assetop_work_shard_alerts_contract.tests.ps1`(新):规则名单、级别、for;指标由 assetop 注册、service 是常量 label;阈值等于 `workshard.Shards`;两条规则同一组聚合 label;四个循环都拿到 metrics;告警名不重名。

### 13.3 给 Codex 的验证命令

工作目录同 §8,PowerShell 7,Go 命令前先 `$env:GOTOOLCHAIN='local'; $env:GOPROXY='off'`。按顺序跑,失败保留编译错误全文、失败用例名与断言原文,不改弱断言,不连续重试。§8 第 1~8 步若还没跑,先跑完它们。

1. `cd go/shared; gofmt -l assetop workshard domainhost; go vet ./assetop/... ./workshard/... ./domainhost/...; go test ./assetop/... ./workshard/... -count=1`。通过:gofmt 无输出;全部 PASS,`TestOwnerSpreadsShortShardKeysAcrossMembers` 的 4 个新前缀子用例在列。
2. `cd go/rank; go build ./...; go vet ./...`。通过:无报错(只改了回调签名)。
3. `cd go/mail; gofmt -l mail.go internal/data; go build ./...; go vet ./...; go test ./internal/data/ -count=1 -run 'TestListDueQueryPutsShardFilterBeforeLimit|TestListDueOwnedRejectsLimitBeforeSQL|TestStoreAdapterBindsWorkOwner'`。通过:3 个用例 PASS。
4. `cd go/data_service; gofmt -l data_service.go internal/store internal/svc; go build ./...; go vet ./...; go test ./internal/store/ ./internal/svc/ -count=1`。通过:全部 PASS,`TestRecallListDueQueryPutsShardFilterBeforeLimit`、`TestRecallListDueOwned_NonPositiveLimitSkipsSQL`、`TestRecallShardMembersPrefix_IsTheZoneRegistrationPrefix`、`TestRecallLoopBindsWorkOwner` 在列。
5. data_service 真库(一次性 MySQL / TiDB,按 `internal/store/storetest` 的 `DATA_SERVICE_IT_MYSQL_HOST` / `_USER` / `_PASSWORD`):`go test -tags=integration ./internal/store/ -count=1 -run 'TestRecallStore_'`。通过:`TestRecallStore_ListDueOwnedReadsOnlyOwnedShards` 与既有 `TestRecallStore_*` 全部 PASS。
6. `cd go/guild; gofmt -l guild.go internal/svc internal/data; go build ./...; go vet ./...; go test ./internal/svc/... ./internal/data/... -count=1`。通过:全部 PASS,`TestCleanupOwner`、`TestCleanupCandidateQueriesPutShardFilterBeforeLimit`、既有 `TestEconomyCandidateReadsTakeNoLocks` 在列。
7. guild 真库(同 §8 第 4 步的 DSN):`go test ./internal/data/ -count=1 -run 'Cleanup'`。通过:`TestCleanupOnce_OnlyOwnedShards`、`TestCleanupOnce_KeepsPendingAndPartial` 与清理水位用例 PASS、不 Skip。
8. `cd go/trade; gofmt -l trade.go internal/data internal/svc; go build ./...; go vet ./...; go test ./internal/data/... ./internal/svc/... -count=1`。通过:全部 PASS,`TestCleanupTerminalOpsOwnedPutsShardFilterBeforeLimit`、既有 `TestCleanupTerminalOps*`、`TestAssetOpCleanupOwner`、`TestStartAssetOpCleanup*` 在列。
9. 仓库根目录:`pwsh -NoProfile -File tools/scripts/tests/assetop_work_shard_alerts_contract.tests.ps1`,通过:`fail=0`;`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`,通过:LogQL 契约那条(含 asset-channel-alerts.yaml)照旧 PASS。
10. 规则语法:按 PROGRESS 2026-11-10「死节点收尾告警」条目的做法,用固定摘要的 `prom/prometheus` 容器(`--network none --rm`,只读挂载)对 `deploy/k8s/asset-channel-alerts.yaml` 提取的 `.spec` 跑 `promtool check rules`,期望 `SUCCESS`,规则数等于文件里 `- alert:` 的条数(写本条时 16)。
11. 多副本冒烟(同 §8 第 9 步的环境):mail、data_service(同一 zone,`Recall.Enabled=true`)各起 2 个副本,`assetop_owned_shards{service="mail"}` 与 `{service="data_service"}` 两副本之和各为 64;guild / trade 开 `CleanupEnabled` 时,两副本启动日志都带 `sharded_by_player=true`,造一批保留期外的终态行后两边的 `*_cleanup_deleted_total` 合计等于造的行数、各自只删自己片里的行。

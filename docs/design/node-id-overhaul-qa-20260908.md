# Node ID 改造 —— 问答全记录与改动细节(2026-09-08)

> 本文是这一轮工作的**完整记录**:用户提出的每一个问题、给出的判断、以及每一处改动落在哪个文件。
> 配套文档:
> - [node-id-overhaul-plan-20260908.md](./node-id-overhaul-plan-20260908.md) —— 方案与推导
> - [node-id-overhaul-changelog-20260908.md](./node-id-overhaul-changelog-20260908.md) —— 逐文件改动表
> - [snowflake-id-allocation.md](./snowflake-id-allocation.md) —— 现状速查(已重写)
>
> 状态:全部改动**未提交**,在 git 工作树里。

---

## 目录

1. [问:scene node id 从 etcd 拿,大厂最标准的方式是这样吗?为什么我觉得有问题](#q1)
2. [问:线上是 k8s,本地不想让策划手改配置;C++ 和 Go 的 node id 分开;将来可能全球同服](#q2)
3. [问:这些问题都修复掉可以吗](#q3)
4. [问:player id 有必要弄成 uint128 么](#q4)
5. [问:一个 node 的所有对象 node id 位都一样,player id / guild id / item guid 可能重复,只是类型不同 —— 我的设计](#q5)
6. [问:为什么要 5 位集群 + 12 位节点?C++/Go 服务端不应该知道这个吧](#q6)
7. [问:只改逻辑,不改 snowflake,上层拼好节点位直接设置不行吗](#q7)
8. [问:现在就是有单独 worker 了吗?还会出现原来的问题吗](#q8)
9. [问:"最小空闲位回收是最坏策略"这条也修复了对吗](#q9)
10. [完整改动细节(按子系统)](#detail)
11. [验证记录](#verify)
12. [未做与残余风险](#residual)

---

<a name="q1"></a>
## 1. 问:scene node id 从 etcd 拿,大厂最标准的方式是这样吗?为什么我觉得有问题

### 答:etcd 这一步没选错,坏在拿到号之后的三件事

**从协调器拿 worker id 本身就是标准做法**:Twitter 原版用 ZooKeeper 临时节点占位;美团 Leaf 用 ZooKeeper 持久顺序节点;百度 UidGenerator 用数据库自增行。
而且在本仓库的约束下没有别的零配置路:k8s 原生只提供两种零配置身份 —— StatefulSet 序号和 Pod UID。本仓 Go 服务全是 Deployment,scene 是 Deployment 或 Agones Fleet,都拿不到序号;Pod UID 是 128 位,塞不进 17 位 worker 字段。所以必须有个注册中心发小整数,etcd 就是那个注册中心。
"策划零配置"这条现状也成立:全仓没有任何手填的 node id(已 grep 验证:`bin/etc/*.yaml`、`go/*/etc/*.yaml`、`run/etc`、`robot/etc`、`deploy/**` 都没有 `NodeId:`/`node_id:` 键,cpp/ 与 go/ 都不读 `NODE_ID` 环境变量)。

**你的直觉对,但问题不在"从 etcd 拿",而在拿到之后做的三件事,大厂的标准做法恰好和当时的实现相反。**

#### (1) 大厂不复用号,当时两侧都是"最小空闲位回收"

- Leaf 用 ZooKeeper 持久顺序节点(单调递增)+ 本地文件缓存;UidGenerator 每次启动烧一个新的数据库自增号。
- 当时:C++ `node_allocator.cpp` 从 1 起扫最小空闲位;Go `snowflakealloc` 从 0 起扫最小空闲位。
- 为什么这是最坏的策略:活着的节点都占着低位号,唯一空出来的低位一定是刚死或刚被冻结的那个。
- Chubby 的 lock-delay(持有者失联后一分钟内不许别人拿)和 Leaf 的回收 PR(某个 workId 超过最大时间没更新时间戳才放回池子)都是为了避开这一点。

#### (2) 大厂把协调器当弱依赖,当时是硬依赖

- Leaf 本地缓存 workerID,ZooKeeper 挂了照样能启动;Leaf-segment 的号段按"10 分钟峰值发号量"配,数据库宕机仍能发 10~20 分钟。
- 当时:etcd 不通则 C++ 起不来(没 node_id 不启 RPC),Go 全部 panic 或 `logx.Must` 退出;运行期失租则全员 fence + `os.Exit(1)`。而生产 etcd 是**单副本 emptyDir 的 Deployment**,一次 Pod 重启就是全集群重新抢号。

#### (3) 大厂只用 lease 做活性,不拿 lease 证明唯一性

- Jepsen 对 etcd 锁的结论是"多个客户端可能同时持有";etcd 自己的 issue #13294 记录了 leader 切换会把所有 lease 续满 `extend + previous TTL`;clientv3 的失租感知恒晚于服务端过期点(`ka.deadline = 收到响应时刻 + TTL`,deadlineLoop 每 1 秒才扫)。
- 当时 `bin/etc/base_deploy_config.yaml` 把 `NodeTTLSeconds` 从 60 调到 180,注释写着"开服洪峰 keepalive 被抢占导致误自杀"——这就是把正确性参数当活性旋钮在拧。

### 当时仓库里让人不安的四个具体点

| # | 问题 | 位置 |
|---|---|---|
| 1 | **C++ 侧一个 node_id 三用,没解耦**:同时是路由身份(Kafka 主题名 `scene-N`、gate 会话反查)、snowflake 的 17 位 worker、buff/skill/session 复合 id 的 node 段 | `etcd_service.cpp:602` `ActivateSnowFlakeAfterGuard` |
| 2 | **64 位布局零空余**:`[time32][node17][step15]` 一位不剩,ID 里放不下 fencing epoch(只能事后 fence 发号器,做不到落库侧拒绝旧持有者),也放不下区域/集群位 | `snow_flake.h` |
| 3 | **C++ guard 写墙钟不是逻辑高水位,且落在分 zone 的 Redis**:node_id 唯一域是全局的,跨 zone 回收时新持有者读不到水位(代码注释自己承认) | `etcd_manager.cpp:192` `WriteSnowFlakeGuard` |
| 4 | **两套实现漂移**:C++ TTL 180 秒 vs Go 60 秒;guard 一个在 Redis 一个在 etcd;guild/friend/player_locator 的 node 分配器是逐字拷贝的三份;`snowflake-id-allocation.md` 里写的 `mustAllocNodeID()` 和 `snowflake_counter` 在代码里已不存在 | 多处 |

### 顺带发现的三件更急的事

1. **k8s 生成的 scene-manager ConfigMap 没有 ZoneId**,而 Go 的 `ZoneId` 默认 1 → 所有 zone 的 SceneManager 都注册成 zone 1。
2. **C++ 发往 Kafka 的 tx_id 和 snapshot_id 两个主题全仓没有消费者**,且两个 topic 都没进 `EnsureTopics`(按 broker 默认自动建:1 分区,dev 保留 60 秒 / prod-like 300 秒)→ 回滚审计链路实际没落地,每条消息几分钟内被丢掉。
3. **Go 分配器的复用分支不清理 `released` 标记**:A 优雅退出写标记(挂在 A 的 lease 上,存活 ≤ TTL)→ A' 同主机重启复用成功但标记还在 → A'' 第二个进程看到标记就跳过 leaseAlive 检查,抢走活着的 A' 的 worker id。guild/login 用裸 hostname 亲和,本地多 zone 或裸金属会踩。

---

<a name="q2"></a>
## 2. 问:线上是 k8s,本地不想让策划手改配置;C++ 和 Go 的 node id 分开;将来可能像王者荣耀、枪战王者、LoL 手游那样全球同服

### 答:全球同服游戏没有一个是"一个全球集群"

| 游戏 | 实际架构 |
|---|---|
| 王者荣耀 | 4 个平台大区(微信/QQ × iOS/Android)× 每个几百个小区;PVP 跨小区**不跨平台**;战队、排行榜按区;约 4600 台机器 / 4 万进程,有 Proxy 层隐藏进程分布 |
| 穿越火线手游 | 5 个大区(安卓微信/安卓手Q/iOS微信/iOS手Q/游客),数据不互通 |
| LoL 手游(Wild Rift) | 美洲与其余地区两套基础设施,注册时锁区 |
| PUBG Mobile | 多个独立账号体系的区域版本;Global 版内 6 个匹配服务器,每 60 天可换一次 |
| Riot | 只有 PUUID 全球唯一且跨区转移后不变;召唤师 id / 账号 id 仍按区 |
| Clash of Clans | 最接近单一全球后端,2022 年 2 月仍把中国切出去 |

**模式是"全球账号层 + 区域分片 + 区域内本地 id",不是"全球一个发号器"。**
本仓的 TiDB 决策文档已经把"player_id 全球唯一"定为不变量,兑现它靠的是 **worker id 空间从第一天起就是全局的**,而不是将来搞一个全球 etcd。

零配置这条:k8s 原生只有 StatefulSet 序号和 Pod UID 两种零配置身份,scene 走 Agones 拿不到序号,而且强删 Pod 同样会破坏 at-most-one,所以**不建议为了拿序号改 StatefulSet**。

---

<a name="q3"></a>
## 3. 问:这些问题都修复掉可以吗

### 答:五条拍板项(用户全部接受)

1. 隔离期 Q = 4 小时、自 fence 期限 F = 2 小时 —— **接受**
2. item guid 也走号段 —— **接受**
3. 号段续段失败允许回退 snowflake —— **接受**(默认关,值域不相交所以安全)
4. `transaction_log` / `player_snapshot` 定在全局库一处,从 go/db 的 zone 库表清单摘掉 —— **接受**
5. etcd 改 3 副本 StatefulSet + PVC 本轮一起做 —— **接受**

按这五条执行,分 6 轮并行落码。详见[第 10 节](#detail)。

---

<a name="q4"></a>
## 4. 问:player id 有必要弄成 uint128 么

### 答:没必要,而且代价很大

- uint128 的收益是"不需要协调器也能保证唯一"(UUIDv7 靠 74 个随机位)。但本仓已经决定 player_id 走**号段**(Leaf-segment),号段本身就没有 worker、没有时钟、没有 lease,唯一性由数据库的一次 CAS 保证,不需要靠位宽换。
- 代价:`player_database` 主键、`guild_member`、`friend_request`、`transaction_log.from_player/to_player`、Redis `player:{id}:*` 全部要改;C++ 的 `Guid` 是 uint64;协议里 `uint64 player_id` 遍布;客户端、Lua、JSON 一律要改字符串传输(现有 id 已经超过 2^53,JS/Lua double 本来就有精度问题,但至少 uint64 在 protobuf/MySQL 里是原生类型)。
- 号段的 2^55 上限:每天 10 亿个号也够用 1 万年。

**结论:保持 uint64,永久身份走号段。**

---

<a name="q5"></a>
## 5. 问:一个 node 的 snowflake node_id,所有对象的 node id 位应该都是一样的,所以 player id 和 guild id、item guid 都是可能重复的,只是类型不一样 —— 我的设计

### 答:实现与此完全一致,没有改这条语义

- **一个进程只有一个槽位**:C++ scene 进程拿一个 `scene-item` 槽,item guid(过渡期)、tx_id、snapshot_id 共用这一个 worker 位;Go 每个服务进程一个槽,进程内所有 snowflake 发号器共用它。
- **唯一性只在同一种 ID 内保证**:player_id、guild_id、scene_id、battle_id、item guid 数值上可以相等,只是类型不同。槽池按 kind 分开(`/snowflake/<kind>/...`)只是为了让不同服务互不干扰地拿槽,**不是**为了跨类型唯一。
- **号段模式下同样如此**:`player`、`guild`、`item` 三个 biz_tag 各自从 1 起计数,彼此数值可以重合,永远不混进同一个键空间。

原设计文档里"跨类型可能同值"曾被列为一条提醒,现在按用户确认它是**既定语义**;评审中若有人把它报成缺陷,直接驳回。

---

<a name="q6"></a>
## 6. 问:为什么要 5 位集群 + 12 位节点?这个 C++/Go 服务端不应该知道这个吧

### 答:为了第二套 etcd;而且服务端确实不知道

**为什么要切**:前提是将来会有第二套独立的 etcd —— 另一个区域、容灾集群、或合服前的另一个大区。两套 etcd 互相看不见,都会从 0 号槽开始发,同一秒两边发出的号逐位相同。17 位 worker 是 ID 里**唯一**能把两套集群区分开的地方,所以要从里面划出集群段。

**为什么是 5 + 12**:
- 12 位 = 4096 个槽,远大于任何一种节点的数量,即便隔离期里压着几百个刚释放的槽也够用。
- 5 位 = 32 个集群/区域,够全球分片用。
- login 的 bwmarrin 13 位按同样比例切成 3 + 10(8 个集群 × 1024 个槽)。

**存量不作废**(此前口头说过"会作废",是错的):旧号的 17 位 worker 数值上等于新布局 cluster=0 的 slot,时间位又只增不减,所以只要现有集群是 0 号就永远不撞。

**服务端不知道这件事**:业务代码拿到的只是一个 17 位的 worker 值。拼装与拆解只存在于分配器库里。服务层唯一多出来的是一个 `ClusterId` 配置键,和 `ZoneId` 一样是运维部署时填一次的常量,进程启动读一下就交给分配器,业务逻辑碰不到,策划更碰不到。

**可选的进一步做法**(尚未实施,待用户决定):让每套 etcd 自己存一个 `/snowflake/cluster_id`,运维建集群时写一次,服务启动时从 etcd 读,服务配置里一个字都没有。

---

<a name="q7"></a>
## 7. 问:只改逻辑就行,不改 snowflake;命名空间出现之前先切 17 位,意思是上层拼好这个节点位直接设置不行吗

### 答:可以,而且更干净 —— 已按此调整

snowflake 本身保持 `[time32][node17][step15]`、只认一个 17 位的 worker 值;"5 位集群 + 12 位槽"只是**分配器层**拼这个值时的内部约定,拼好直接 `set_node_id` 即可。

第一轮落码时为了方便,把拼装/拆解常量和 `ParseGuid` 的 cluster/slot 字段放进了 snowflake 类里。按用户要求挪回分配器层:

| 语言 | 挪什么 | 从哪 | 到哪 |
|---|---|---|---|
| Go | `ClusterBits`/`SlotBits`/`MaxCluster`/`MaxSlot`、`ComposeWorkerID`、`DecodeWorkerID`、`ComposeWorkerIDWithBits`、`DecodeWorkerIDWithBits`、`WorkerOf`、`DecodeID` 及其测试 | `go/shared/snowflake/snowflake.go`、`layout_test.go` | `go/shared/snowflakealloc/layout.go` 及其测试 |
| C++ | `kClusterBits`/`kSlotBits`、`ComposeWorkerId`、`DecodeWorkerId`、`ParseGuid` 的 cluster/slot 字段;按水位年龄的自 fence 判定 | `snow_flake.h`、`SnowFlake::Generate` | `snowflake_slot_client`;`SnowFlakeManager::GenerateItemGuid` 包装层 |

**Go 侧保留在 `Node` 里的**:FenceClock 判定。理由 —— 它是自有发号器包,`Fence()` 七月就已经在里面,且 Go 消费方直接调 `Generate()`,没有 C++ 那样的包装层。若也要拿出去,代价是给 match 和 scene_manager 换一个包装类型。

**C++ 侧 `snow_flake.h` 最终只剩一个新增项**:只读的 `high_water_epoch()` 访问器(分配器写水位需要读逻辑高水位,无法从外部获得)。

---

<a name="q8"></a>
## 8. 问:现在就是有单独 worker 了吗?我没懂,那还会出现原来的问题吗

### 答:有了。C++ scene 启动后是两条互不相干的链

```
路由身份链:
  NodeAllocator::AcquireNode
    → CAS  <Svc>.rpc/allocated/node_type/T/node_id/N
    → EtcdService::OnTxnSucceeded → StartRpcServer()
  这个 N 只用于:Kafka 主题名 scene-N / gate 会话反查 / buff、skill 的临时复合 id

发号 worker 链(新增,独立):
  SnowflakeSlotClient
    → 扫 /snowflake/scene-item/c<cluster>/{slots,watermark}
    → 选槽(最久未用 + 隔离期)
    → CAS 建 slots/<slot> 并在同一事务写第一笔水位
    → SnowFlakeManager::OnNodeStart(worker = cluster<<12 | slot)
```

以前是 `etcd_service.cpp` 拿路由 N 直接喂给发号器(`ActivateSnowFlakeAfterGuard`),那段代码已删。Go 侧本来就是两套,这轮只是把发号那套换了协议。

### 原来的每个问题,现在为什么不会发生

| 原问题 | 改在哪 | 现在的机制 |
|---|---|---|
| 最小空闲位把刚死的槽发给新节点 | C++ `snowflake_slot_selection.h`;Go `snowflakealloc/selection.go` | 每个槽有一个**不挂 lease** 的水位 key。申领时跳过 4 小时内有人写过水位的槽,剩下的挑最久没用的。A 冻结时它的槽水位是 1 秒前写的,B 在 4 小时内根本不会选它 |
| 冻结的 A 醒来照发号 | C++ `SetFenceDeadline` + `GenerateItemGuid` 包装层判定;Go `snowflake/fenceclock.go` | 每次水位写成功把期限推到 2 小时后。醒来时超过 2 小时就拒发;没超过 2 小时也没关系,因为它的槽 4 小时内没人碰 |
| 同秒重启重号 | C++ 申领事务里第一笔水位与槽 key 同时落地;Go `allocator.go` 亲和复用只在 `released` 存在时允许且事务里删掉它 | 继任者把发号起点抬到前任水位之上,并等真实时钟越过那一秒(不借位) |
| 跨机时钟偏差 | 水位值 = `max(墙钟, 逻辑高水位) + 2 秒` | 偏差被 4 小时隔离期吃掉,而不是靠 lease 的 60 秒 |
| lease 不是互斥、TTL 是正确性参数 | `etcd_service.cpp` 的 `OnKeepAliveResponse`;Go 的 reclaim 循环 | 唯一性完全不看 lease。lease 过期只重挂(`CreateRev==0 或 Value==uuid`),只有槽被别的 uuid 挂走才永久 fence。TTL 从此只影响服务发现多快摘掉死节点 |
| 硬依赖 etcd | Go `snowflakealloc/cache.go`;C++ 失租不自杀;`deploy/k8s/manifests/infra/etcd.yaml` | etcd 抖动不会让任何节点退出;Go 可用本地缓存离线启动;etcd 3 副本 + PVC |
| C++ guard 写墙钟、落分 zone Redis | `etcd_manager.cpp` 的 Redis guard 整条删除 | 水位统一在 etcd 全局命名空间,两侧同一语义 |
| 两套实现漂移 | 两端读同一份 `selection_vectors.json` | 15 个选号用例两端逐一相符 |
| released 标记让第三个进程抢活槽 | `allocator.go` 复用事务 | 复用要求槽、亲和键、released 三者在**同一个 lease** 上,且事务里删 released;有回归测试 |
| 第二套 etcd 撞号 | 分配器层拼 `cluster<<12 \| slot` | 运维给每套集群填一个 ClusterId;snowflake 本身不知道 |
| 永久 ID 依赖 lease 和时钟 | `go/shared/idsegment`;login `player_id_minter.go`;guild `guild_id_minter.go`;data_service `id_segment_store.go` | player_id、guild_id 从数据库领号段,没有 worker、没有时钟;item guid 正在改成同样方式 |

### 还会不会出现原来的问题

模型内不会,但有前提和残余,不应说成零风险 —— 详见[第 12 节](#residual)。

---

<a name="q9"></a>
## 9. 问:"最小空闲位回收是最坏的策略 …… 这就是 Chubby 的 lock-delay",这些也修复了对吗

### 答:两侧都修了,就是这套机制

**水位(持久、不挂 lease)**
- Go:`snowflakealloc/allocator.go` 的 `advanceGuard`,每秒一个事务
  `If Value(slots/<slot>) == 本进程 uuid Then Put watermark = max(墙钟, 逻辑高水位) + 2`
- C++:`snowflake_slot_client.cpp` 每个 keepalive tick 同样的事务;申领时第一笔水位与槽 key 在**同一个事务**里落地

**申领跳过 4 小时内活跃过的槽、挑最久没用的**
- Go:`snowflakealloc/selection.go` 的 `selectSlot`,Q 默认 14400 秒
- C++:`snowflake_slot_selection.h` 的 `SelectSnowflakeSlot`,同一个 Q
- 两端跑同一份 `selection_vectors.json`,含"全部被隔离则拒绝"、"从没用过的优先"、"并列取最小"、"只有旧格式水位"等 15 例,逐一相符

**A 自己 2 小时内停发**
- 每次水位写成功把期限推到 2 小时后。A 被冻住期间写不了水位,解冻后第一次发号先看期限:超过 2 小时拒发。没超过 2 小时它还能发,但这正好没关系 —— 它的槽被隔离 4 小时,没有第二个人在用
- Go:`snowflake/fenceclock.go`,判定在 `Node.Generate` 入口和等秒之后各一次,**单调钟和墙钟任一超期**都算(VM 挂起会停单调钟)
- C++:`SetFenceDeadline(steady, wall)`,判定挪到 `SnowFlakeManager::GenerateItemGuid` 包装层

**对应的验证用例**
- Go 真 etcd 集成:`TestQuarantine_FailsClosedThenReleasesLeastRecentlyUsed`、`TestReclaimAfterLeaseBlip`、`TestLegacyKeysStillQuarantineAndOccupy`、`TestGuardWatermarkCoversMintingFromTheFirstID`
- Go 单测:`TestFenceClock_MonotonicAgeTrips`、`TestFenceClock_WallAgeTripsWhenMonotonicStalls`、`TestGenerate_RefusesWhenWatermarkStale`
- C++:`SnowFlakeSelection.SharedVectorsAgreeWithGo`、`SnowFlakeFence.SteadyClockPastDeadlineRefusesToMint`、`SnowFlakeFence.WallClockPastDeadlineRefusesEvenIfSteadyClockStopped`、`SnowFlakeGuard.FirstIdLandsPastGuardSecondWithoutBorrowing`

**没做的**:真集群上"冻结进程再解冻"的故障注入(要在 k8s 里跑)。

---

<a name="q9b"></a>
## 9b. 晚间修订(用户按 10 万台 scene 的目标规模重新拍板;以此为准,覆盖上文冲突之处)

完整条文见 plan 文档 §7.5。要点:

1. **scene 彻底退出 snowflake 槽位协议。** item guid、tx_id、snapshot_id 三种全部走号段(biz_tag `item` / `txlog` / `snapshot`)。理由:10 万节点每秒 10 万次水位写 + 10 万次 lease 续约超出单个 etcd 一个数量级,任何"每节点一个 etcd 槽"在这个规模都不成立;号段对节点数不敏感。tx_id / snapshot_id 只是去重键与主键,不需要时间序(时间在 `timestamp_sec` / `created_at` 列里)。**因此第 8 节里描述的 C++ `SnowflakeSlotClient`、`snow_flake.h` 的集群/槽位常量与期限 fence、缓存文件、灰度双向兼容全部作废删除,`snow_flake.h` / `snow_flake_manager.h` 回到改造前形态**;`FallbackToSnowflake` 随之删除(没有槽位就没有可回退的发号器,只保留 fail-closed)。
2. **槽位协议只服务 Go 的四种**(login-player、guild、scene-manager、match,合计几十个实例),4096 槽无容量压力;不再追加按槽位的通用 released 标记。
3. **一种 GUID 一个号段客户端实例**:C++ 改成通用 `GuidSegmentClient` + 按类型注册表,启动时按配置实例化 item / txlog / snapshot,以后 pet、guild 等只加一行注册;各实例独立缓冲、预取、退避、指标,共享一条到 data_service 的 gRPC 连接,响应按请求编号分发。Go 的 `idsegment.Client` 本来就是通用的。
4. **动态 step(Leaf 口径)**:不到 15 分钟用完则下次翻倍,超过 30 分钟减半,钉在 [MinStep, MaxStep](item 默认 [1000, 1,000,000] 初值 20,000;player/guild [10, 1000] 初值 100)。每天重启一次浪费 ≤ 2×step,2^55 在 10 万台每天重启下仍可撑两万年以上。
5. **不做的**:优雅关服把没用完的尾巴还给数据库(破坏"每段必比上一段大"的不变量);本地 json 缓存改 Redis(另一个协调器,崩了就没了;缓存唯一意义是 etcd 不通还能起,只能靠本机磁盘);Go 侧缓存改为**故障期才写**(etcd 水位写失败时才把本进程高水位落盘)。
6. **号段客户端不持久化游标**:一段在 `AllocateIdSegment` 提交时就已被数据库判定为用掉,进程崩溃只产生空洞不产生重号。
7. **`id_segment` 行被重置/从备份恢复是 ID 安全事件**:生产不自动补种(`AllowAutoSeed` 仅 dev),行由迁移显式创建(`BootstrapTags`);运维手册记录"恢复全局库前须先核对 max_id ≥ 消费表最大号"。
8. **Kafka 审计主题带代际后缀** `transaction_log_topic_g1` / `player_snapshot_topic_g1`:broker 自动建的 1 分区裸名主题会让 `EnsureTopics` 的分区契约永远失败、消费者永远起不来;infra 必须预建;C++ 生产者通过 `AuditTopicGeneration` 拼同一个名字。
9. **合服与账号角色列表缺口**(第 8 节末尾)已拍板修复:登录时 `BatchGetPlayerHomeZone` 批量解析、EnterGame 按 home_zone 走既有跨区重定向。

### 对第 10 节"完整改动细节"的影响
- 10.1 的 key 协议、10.2 选号、10.3 水位、10.4 自 fence、10.5 弱依赖、10.6 观测:**只对 Go 四种 kind 成立**,C++ scene 不再参与。
- 10.7 号段:C++ 侧的 `item` 客户端升级为通用 `GuidSegmentClient` + 注册表,并新增 `txlog` / `snapshot` 两种;`FallbackToSnowflake` 删除。
- 10.8 第 3 条:消费者监听的主题名带 `_g<N>` 后缀。

<a name="q10"></a>
## 9c. 问:修复到合服没有任何 bug 可以吗

### 答:能承诺的是过程,不是"零"

穷举合服触及的每条数据路径、逐条对抗验证、发现的缺陷全部修复并配回归测试、剩余风险明说。为此启动了一轮全链路审计(3 路普查:文档运行手册与不变量 / 合服工具实现 / 全系统带区状态普查 → 7 个维度找缺陷:数据归属与路由、账号登录进入、社交与经济、场景热状态、基础设施与持久化、合服工具本身、客户端契约 → 每条中高严重度发现独立反驳验证)。结果落在本文后续追加节。

### 第一批已确认并已派修的缺陷(来自"角色列表 + EnterGame 重定向"修复的对抗验证)

| # | 严重度 | 缺陷 | 修法 | 落点 |
|---|---|---|---|---|
| 1 | P0 | **客户端从不响应 RedirectToGate**:机器人 handler 只记日志,Unity 的 handler 是未实现的 partial。开着"进入时重定向"反而让走错区的玩家卡死(login 已返回成功并清掉登录会话,永远等不到 RoutePlayer) | 进入时重定向改为**显式开启**(`HomeZone.RedirectOnEnterEnabled`,默认关),角色列表刷新成为主要机制;机器人实现参考 handler(断开 → 连目标 gate → token 校验 → 重新 Login + EnterGame);Unity 端需同样实现 msg 124 后才可开启 | login、robot |
| 2 | P0 | **源区残留的 `player:{id}:location` 让两条腿都被 `ErrUnsafeCrossNodeHandoff` 拒掉**(区整体下线时 gate 没来得及发 Disconnect,key 不清;合服工具不碰它) | scene_manager:定位所在节点不存活且该区 node_load 为空时把定位视作不存在(Redis 出错保持拒绝);合服工具加"清源区热状态"步骤(location / scene zone / world channel / instance count / node_load),仅在源区已下线后允许 | scene_manager、tools/merge_zone |
| 3 | P1 | scene_manager 在场景解析**之后**才判重定向,目标区过渡期没有频道就直接 ErrNoAvailableNode;而 **login 把 EnterScene 的 ErrorCode≠0 当成功**(只记日志,照样 SetIdempotency 并清会话,客户端挂死)——后者是与合服无关的既有 bug | scene_manager:ZoneId≠GateZoneId 时先走重定向再解析场景;login:ErrorCode≠0 返回错误 tip、保留登录会话让客户端重试 | scene_manager、login |
| 4 | 中 | 重连/顶号时也触发了重定向,而 scene_manager 对有活定位的跨区请求必拒 | 只在 FirstLogin 且无活场景定位时才按 home_zone 覆盖 | login |
| 5 | 低 | 未建映射的玩家每次 EnterGame 打一条 ERROR(data_service 对缺 key 返回 gRPC 错误而非 0) | data_service 缺映射返回 0;login 对未映射记 Info,只对传输错误记 Error | data_service、login |
| 6 | 低 | data_service 不可达时每次登录串行多等 1.5 秒且无熔断 | DataServiceRpc 开 breaker;角色列表查询超时 500ms,EnterGame 1.5s,均可配 | login |
| 7 | **缺口** | **生产代码从未在建角时写 `player:zone` 映射**(只有导入工具和合服 remap 写),home_zone 模型此前只覆盖导入玩家 | login CreatePlayer 在持久化前调 RegisterPlayerZone,失败则建角 fail-closed;合服工具加 `-backfill-home-zone`,按 zone 库的 player_database 行补齐缺失映射(只补不覆盖),首次合服前每个区都要跑 | login、tools/merge_zone |
| 8 | 低 | 重定向后源区 gate 仍保持会话绑定,玩家在源区显示在线直到客户端断开 | gate 发完重定向后延迟关连接(C++,排在下一批) | gate |

<a name="detail"></a>
## 10. 完整改动细节

> 逐文件表格见 [node-id-overhaul-changelog-20260908.md](./node-id-overhaul-changelog-20260908.md)。此处按子系统给**机制说明**。

### 10.1 共用的 etcd key 协议(两种语言字节级一致)

```
/snowflake/<kind>/c<cluster>/slots/<slot>        = 持有者 uuid    挂 lease(只表活性)
/snowflake/<kind>/c<cluster>/watermark/<slot>    = epoch 秒        不挂 lease(持久;既是 guard 地板又是隔离期墓碑)
/snowflake/<kind>/c<cluster>/affinity/<host>     = slot           挂 lease(仅 Go;同主机优雅重启复用)
/snowflake/<kind>/c<cluster>/released/<host>     = "released"     挂 lease(仅 Go)
/snowflake/<kind>/c<cluster>/watermark_ms/<slot> = unix 毫秒       不挂 lease(仅 login-player)
```

kind:`scene-item`(C++ scene)、`login-player`(3/10 位)、`guild`、`scene-manager`、`match`。

### 10.2 选号算法(替代"最小空闲位")

```
used     = slots/ 下存在的 slot
wm[i]    = watermark/i(缺省 0)
now      = 墙钟秒(snowflake epoch 口径)
Q        = 14400 秒
候选      = { i ∉ used, i ≤ maxSlot, wm[i] == 0 或 now − wm[i] ≥ Q }
选择      = 候选中 wm 最小者;并列取最小 i
CAS      = If CreateRevision(slots/<i>) == 0 Then Put slots/<i>=uuid(lease) + Put watermark/<i>
候选为空  = fail-closed 报错(13 万/4096 槽对几十个节点不可能)
```

### 10.3 水位契约

- 值 = `max(墙钟, 发号器逻辑高水位) + 2 秒`;每 1 秒写一次;不挂 lease;单调不回退
- 申领成功后第一笔水位与槽 key 同事务落地(C++)/ 同步写一次(Go)
- 启动地板 `SetGuardTime(max(now, wm))`,并**等真实时钟越过那一秒**(不借位 —— 借位会把号发到墙钟前面,继任者的地板罩不住)
- 优雅退出:先 Fence 发号器 → 同步写最终水位 → 写 released → 取消 keepalive(**不 Revoke**)

### 10.4 自 fence(按水位年龄)

- 持有者记录 `lastAck` = 最近一次水位写成功时的单调钟 + 墙钟
- 发号前判定:`单调now − lastAckMono > F` **或** `墙钟now − lastAckWall > F` → 拒发
- `F = Q/2 = 7200 秒`。下界推导:`Q ≥ 2 × (TTL_max 180s + 最大时钟偏差 + drain 预算 15s + 冻结感知延迟)`,4 小时对每一项都有两个数量级余量
- Go 侧是**可恢复**的(`ErrWatermarkStale`,下次水位写成功即恢复);永久 `Fence()` 只在确认被接管时

### 10.5 弱依赖

- 发号从不等 etcd(水位异步写)
- lease 过期但槽没被别人挂走 → 重新挂 lease + 重挂槽,**不 fence 不退出**
- 启动时 etcd 不通:Go 用本地缓存文件(`snowflake-<kind>-<affinity>.json`,原子写)且 `now − lastAckWall < F` 时可用缓存槽起,后台每 2 秒重试注册,注册成功前 lastAck 不推进
- C++ 不实现离线启动(服务发现本来就要 etcd),缓存只用于抬地板与关联日志
- etcd 本身改 3 副本 StatefulSet + PVC + PDB(minAvailable 2)+ 反亲和

### 10.6 观测三字段

每条相关日志带 `node=<hostname/pod>`、`worker=c<cluster>:<slot>`、`inc=<slots key 的 mod_revision>`。id 会随重启变,`inc` 全局单调唯一;dashboard 按 node 聚合,排查撞号按 inc。

### 10.7 号段(Leaf-segment)

```sql
CREATE TABLE id_segment (
  biz_tag  VARCHAR(64) NOT NULL,
  max_id   BIGINT UNSIGNED NOT NULL,      -- 已分配到(不含)
  step     INT UNSIGNED NOT NULL,
  version  BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (biz_tag),
  CHECK (max_id < 36028797018963968)      -- 2^55
);
```

- 服务端 `data_service.AllocateIdSegment(biz_tag, step) → [lo, hi)`:一个事务里 `SELECT … FOR UPDATE` + 版本号 CAS;首次使用 `INSERT IGNORE` 建行(从 1 起);`hi ≥ 2^55` 直接拒绝
- 客户端双 buffer,剩 10% 预取(单飞);每段校验 `lo≥1、hi>lo、hi≤2^55、lo≥上一段 hi`,违反即整段拒收
- **与存量 snowflake 号值域不相交**:player_id(bwmarrin,epoch 2024-07,毫秒<<22)最小 ≈ 2.8×10^17;guild_id / item guid(epoch 2026-03,秒<<32)最小 ≈ 6.7×10^16;号段值域 [1, 2^55 ≈ 3.6×10^16)。**新旧号可在同一张表共存,不迁一条数据**
- `id_segment` 是全仓唯一允许用专门 bootstrap 语句预建的表(proto2mysql 把 string 主键渲染成 MEDIUMTEXT,MySQL 报 1170),与 go/db 预建 `user_accounts` 同一模式
- 接线:login `biz_tag="player"` step 100;guild `biz_tag="guild"` step 100;C++ scene `biz_tag="item"` step 1,000,000(进行中)
- 继续用 snowflake 的:snapshot_id、tx_id(需要时间序)、scene_id、battle_id/challenge_id、session_id

### 10.8 三个独立修复

1. **scene-manager ConfigMap 补 ZoneId**:`tools/scripts/k8s_deploy.ps1` 的 scene-manager 模板加 `ZoneId: ${CurrentZoneId}` 与 `LeaseTTL`,删掉已废弃的 `NodeID: "node-1"`
2. **released 标记**:复用事务里 `OpDelete(releasedKey)`;`Close()` 顺序改为先 Fence 再写 released;回归测试"A Close → A' reuse → A'' 在 TTL 内不得拿到 A 的槽"
3. **tx_id / snapshot_id 消费者**:五步 —— proto 补 `zone_id` → `EnsureTopics`(6 分区 / 3 分区,保留 30 天)→ 表结构收回 proto 驱动(`player_snapshot` 加 `snapshot_guid`、`source`;`rollback_audit_log` 加 `orphans_cleaned`;`transaction_log` 列名统一 + TiDB 方言)→ transaction_log 消费者(批量 `INSERT IGNORE`,tx_id 主键天然去重,插入成功才 commit,DB 失败停下不 commit)→ snapshot 消费者最后,以 `source=1` 落库且所有 GM 读路径先过滤 `source=0`(否则会弄坏今天能用的 GM 回滚)

### 10.9 部署

- `-ClusterId`(0..31)参数贯穿 zone-up / infra-up / all-up,写进 C++ node 与 login/scene-manager/match 的 ConfigMap
- `SNOWFLAKE_CACHE_DIR=/tmp/snowflake` + emptyDir 挂载(C++ Deployment、Agones Fleet、三个 Go Deployment)
- login ConfigMap 与 `deploy/login-stack.linux/login.yaml` 加 `DataServiceRpc`、`IdSegment`
- data-service ConfigMap 加 `Schema`、`Kafka` 块;全局库 `mmorpg_global` 已由 infra-up 预建;本地 compose 补建 `testdb` 并授权
- etcd:Deployment → 3 副本 StatefulSet(apply 前删旧 Deployment,否则 Service selector 会同时命中两者造成脑裂)

---

<a name="verify"></a>
## 11. 验证记录

| 项 | 结果 |
|---|---|
| `msbuild game.sln Debug\|x64` | 0 错误(两次全量:协议重生成后、C++ 解耦后) |
| C++ `snow_flake_test`(筛选套件) | 24/24 |
| C++ `bag_test` | 124/124 |
| Go `go/shared/snowflake`、`snowflakealloc` | 单测全过;`-tags=integration` **真 etcd 34/34** |
| Go `go/shared/idsegment` | 12/12(时序敏感用例重复 15 次) |
| Go `go/login`、`go/guild` | 全过 |
| Go `go/data_service` | 单测全过;`-tags=integration` **真 MySQL 12/12**(临时库用完即删) |
| `go build` go/proto、data_service、login、guild、match、scene_manager、friend、player_locator | 全过 |
| k8s 脚本 | 解析 0 错误;zone-up / infra-up 干跑渲染 **32 项断言全过** |
| 跨语言一致性 | 15 例选号向量,C++ 与 Go 逐一相符 |

**已知本机红灯(与本次改动无关)**:`go/db` 的 `go.mod` 里 proto2mysql 的 replace 指向不存在的目录;`go/shared` 整模块 `go build ./...` 因 `generated/bit_index` 一个目录两个 package 而红(只能按子包编)。

---

<a name="residual"></a>
## 12. 未做与残余风险

### 前提假设
- 主机间时钟偏差 < 2 小时(NTP 正常远远满足)
- 运维不会给两套独立集群填同一个 `ClusterId`

### 过渡期逻辑(一个版本后必须删)
- Go cluster 0 读旧 `<oldprefix>/snowflake_guard/<slot>`、`/login/guard_ms/<slot>` 取 max,并把旧 `snowflake_ids`/`snowflake_nodes` 算作占用
- C++ 把 `SceneNodeService.rpc/allocated/node_type/<T>/node_id/<N>` 被持有视为 slot N 占用,并读旧 Redis `snowflake_guard:<type>:<N>` 取 max
- 原因:灰度期间旧二进制仍用"路由 node_id = worker"发号,而新命名空间里对应槽位的水位是 0,会被当成"从没用过"优先选中

### 未验证
- **真集群故障注入未跑**:冻结进程再解冻、`etcdctl lease revoke`、强删 Pod。目前是本机真 etcd(3.6.1)与真 MySQL 上的集成测试
- Kafka 本机没起,两个消费者只在假 reader + 真 MySQL 上验过
- Agones 路径从未在真集群跑过(既有状况)
- C++ item guid 号段仍在编译验证中

### 顺带发现、未处理
- 协议生成器会往 `D:\luyuan\wuxingqitan\mmorpg-client`(Unity)写生成代码:那边有 32 个文件只差行尾、26 个未提交的 handler 桩,是之前就有的漂移
- `tools/scripts/dev_tools.ps1` 的 k8s 包装器不转发 `-ClusterId`(只影响 cluster≠0)
- guild / friend / chat 没有 k8s ConfigMap 与清单(既有缺口),将来需要 `ClusterId`、`SnowflakeCacheDir`、`DataServiceRpc`、`IdSegment`
- data_service 按 zone namespace 部署但库与 go-zero key 是全局的(既有事实;号段表靠 `FOR UPDATE` 串行化所以安全)
- `transaction_log` 表的 proto 尚缺 `zone_id` 列(消息已有),消费者已写好自动探测,列补上即自动落库

### 待用户决定
- 是否把 `ClusterId` 也从服务配置里去掉,改为每套 etcd 自存 `/snowflake/cluster_id`,服务启动时读
- Go 侧 FenceClock 是否也从 `snowflake.Node` 挪到包装层(代价:match 和 scene_manager 要换包装类型)

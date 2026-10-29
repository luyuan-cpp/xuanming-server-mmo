# 新号首次进场免冷读:建角时写入 PlayerAllData 种子

> **2026-10-23 安全合同后继（实现与验证中）**：下文保留原缓存种子方案的历史依据。当前以本节为准：正式建角必须先在同一 TiDB 事务中提交账号角色目录及权威落点的两张玩家 SQL 初始行；缓存种子只能在该事务成功后，由本次严格新建的提交回执产生。尚未取得当前编译、完整测试与真实 TiDB 证据，不表示本节已验证完成。

## 建角初始持久提交

- 初始版本 `(owner_epoch=1, save_seq=1)` 是每个严格全新 ID 的无 actor 持久提交，不能由普通存盘、导入、修复、恢复或转让补发。它不授予场景写权限。建角的目录首次 INSERT 与两张玩家行的严格 INSERT 共用全局权威池的同一个 `sql.Tx`；任一行已存在、权威路由缺失或冻结、落点越出白名单、权限或提交失败，全部回滚，不覆盖原件。
- 锁顺序为账号目录、路由可写保护、正式中性合服分片与完整主键的权威路由、角色唯一归属、第一张玩家行、第二张玩家行。落点只由被锁的 SQL 路由和 `placement.StoreDBName` 派生；跨 schema 仍用此事务所在的同一物理连接，禁止另开落点连接拼成两阶段提交。
- `account_role.initial_player_commit=1` 只与本次目录和双行严格新建在同一事务产生，旧行正式迁移默认 0；不从任意旧/导入/恢复的 `1:1` 数字倒推资格。正常 global 装配固定要求初始提交，`RequireTiDB` 探针、`RoleTradeSQLProof` 和 dev/test 模式均不关闭此安全语义；只有显式 `LegacyNonGlobal` 构造器不发此回执。
- 无 actor 的角色交易使用独立 `INITIAL_COMMIT` 证据：必须完整 P、0 required floor、0 holders、目录标记 1 / owner revision 1 及双 SQL 完整初始 payload；不能伪成 `NO_ADMISSION(0:0)` 或 SCENE。已有 closed admissions 的历史下限仍保留。
- 目录事务只有一次 Begin/Commit，没有自动重试；每次 action 清空临时初始回执，Commit 未确认时不返回。Login 只能经原 Lookup 恢复目录成功，不能由此补发缓存种子。
- 默认关闭 `NewPlayerSeed` 时，首次登录强读两张实际 SQL 行，沿现有 `Reconcile/AdoptSQL` 写完整父快照、`LastSaveSeq=1`、pending 和两表 ACK，并将 owner 地板抬到 1。正式首任 actor 随后仍只由 SceneManager 的既有 INCR 铸为 2；Login 和 C++ 不另行 INCR。恢复只抬地板，不降低已铸出的较高 owner。
- 打开 `NewPlayerSeed` 时保留首次进场免 Kafka 冷读的优化，但种子移到正式 SQL 提交之后。仅本次新建成功回执可写包含完整初始版本的父快照、屏障和 owner 地板；已有目录幂等、未知提交结果及目录回读均不补发该回执。迟到种子遇到非初始父快照、较高 owner 或已推进屏障时不写，不能补偿删除已经提交的角色或释放其在役名字。禁止继续使用 `LastSaveSeq=0` 的已核验快路径冒充持久基底。
- 任一玩家 SQL 回包的 `Data` 真正缺失均拒绝，不能先填 `player_id` 骨架再称它是已有初始行。目录重放、Import、Repair、Restore、转让及墓碑均不重建缺行。首存前失败可以从仍完整的初始提交恢复；历史任一行丢失仍 fail-closed。该方案不声称检测全套 SQL 原件同时回档至同一旧快照。
- 验证必须覆盖事务全回滚、并发同 ID、响应丢失、迟到 seed、开关两种模式、初始提交到首任 actor 的衔接、首存前路由失败后保持较高 owner，以及首存后删除任一行仍拒绝。原 §7 的 L-14 开关对照和 p99/读量观测要求保留；未运行时不宣称性能改善。

> **状态**:提案(开服准备 L-05 / G3),2026-10-12 起草,等负责人按 §10 拍板。2026-10-13 已按 §10 的默认值落码(开关默认关,见 §13);同日三视角评审的修复见 §13.1。没有编译、测试或压测,按 AGENTS.md §10.1 交 Codex 验证。
> **依据**:只读核对代码、配置与文档,HEAD `73b852f32`。`go/db/internal/kafka/key_ordered_consumer.go`、`tools/scripts/k8s_deploy.ps1`、`docs/design/player-storage-placement.md`、`docs/design/zone_data_rollback.md` 有他人未提交的改动,按工作区行号引用;其余文件工作区与 HEAD 相同。
> **2026-10-16 更正**:同日 `redis_client.h` 删去两段存盘脚本里的 SADD、普通脚本改名并加注释,该文件行号整体下移;本文对 `redis_client.h` 的行号引用改为按符号(常量 / 函数名)引用,其余文件的行号仍是上述 HEAD 快照。
> **起因**:[tidb-phase1-and-launch-readiness.md](./tidb-phase1-and-launch-readiness.md) §7 的 L-05(:816)。新号第一次 EnterGame 一定冷读数据库,玩家在加载屏上同步等待,每个读任务的等待上限是 8s。首日这是 Phase 2 全局玩家库唯一的同步路径,也是单 zone 预加载链的第一个瓶颈。
> **关联**:[player-storage-placement.md](./player-storage-placement.md) §13、[player-route-mapping-durability-proposal.md](./player-route-mapping-durability-proposal.md) §12、[cross-zone-scene-travel.md](./cross-zone-scene-travel.md) CZ-2 / CZ-4、[scene-owner-reentry-barrier.md](./scene-owner-reentry-barrier.md)、[no-single-node-horizontal-scaling-20261001.md](./no-single-node-horizontal-scaling-20261001.md) §5。

## 1. 结论

- **做法(方案 A)**:CreatePlayer 在写账号 blob 之前,先往 `PlayerAllData:{id}` 写一份「种子」。
  - 种子的内容和今天冷读读空后 login 组装出的父键完全一样:两张子表各只带 `player_id`。
  - TTL 也一样,都是 24h。
  - 新号第一次 EnterGame 时,预加载命中缓存快路径,不再发 Kafka 读任务,不碰 go/db,也不用等那 8s。
- **为什么正确**:
  1. 号段 id 不复用。刚铸出的 id 在任何库里都没有行,「读到空」本来就是真值,种子只是把这个结论提前写进缓存。
  2. 种子和冷读结果出自同一个构造函数。之后的身份补齐、scene 的新号初始化、首次存盘都走今天的老路,C++ 和 go/db 一行不改。
  3. 写入脚本只在「父键不存在,且 `player:{id}:owner_epoch` 也不存在」时才落地,覆盖不了任何真实存档。
- **种子不是真源,只是缓存优化**。写失败、过期、被删、Redis 丢键,首次进场都会退回今天的冷读,结果相同。
- **开关**:`NewPlayerSeed.Enabled`,默认关。关着时和今天逐字节相同。
- **不选「新号标记」(方案 B)**。它有两种做法:要么退化成「推迟到首次进场才写」的 A;要么让 scene 凭一个可能已过期的标记把空档当真,出错时空档会覆盖真实进度。
- **代价**:
  - 建角时多一次 Redis 往返,落在账号锁内。
  - 未进场角色的种子各占约百字节 Redis,24h 后自动过期。
  - **少了一个早期信号**。`player-storage-placement.md` §13 第 3 条禁止的两种违规配置:go/db 开了 `Placement.Required=true` 而 login 没钉落点;`NewPlayerStorageId` 指向 go/db 打不开的库。今天两者都表现为「新号预加载失败、进不了游戏」;打开种子后变成「能进场,但存盘进死信或无限重试,进度只在 Redis、不进 MySQL」,更隐蔽。打开种子之前两类告警必须先就位(§9、§10)。
  - **新号的入场链改在 PreloadPool 的 worker 上跑完**。快路径在池 worker 里同步跑完整段 apply;冷读路径发完 Kafka 就把 worker 还回去。开种子后,每个新号占用池 worker 的时间变长,池满回 `kLoginInProgress`(§6 第 21 行)。老玩家命中缓存时今天就是这样。
- **Java 版不适用**:Java 建角时同步插入玩家行,进场按主键读本库,没有这条冷读链(§11)。

## 2. 现状链路(逐步核对)

### 2.1 建角:写名字、归属映射和账号目录,不写 PlayerAllData

`go/login/internal/logic/clientplayerlogin/createplayerlogic.go`:

| 步 | 做什么 | 出处 |
|---|---|---|
| 3 | 拿账号建角锁 `account_lock:create:{account}`,TTL 20s | :116-130;`login.yaml:83` |
| 4 | 读账号 blob | :132-154 |
| 6b | 号段铸 `player_id`,失败就整体拒绝 | :219-229 |
| 6c | 名字登记到 data_service 全局库,fail-closed | :231-255 |
| 6d | 登记 `player:zone`,fail-closed;`PinOnCreate` 时同一次 RPC 原子钉落点 | :257-274、:707-717 |
| 7 | 以建角锁为围栏写账号 blob;结果不确定时回读 | :287-301、:54-73、:621-656 |
| 7b | 写反查索引 `player_id → account` | :303-308 |

> **2026-10-15 更正注**(负责人按「最标准」定 §10 P2 / P7 之后):上表是 G3 落码前的现状。现行链路在 6b 与 6c 之间多一步 **6b'**:对新铸 id 只读探测 `PlayerAllData:{id}` 父键与 `player:{id}:owner_epoch`(两条 `EXISTS` 走一个 pipeline,一次往返),命中或探测出错都拒绝建角(`kLoginDataSerializeFailed`,与 6c / 6d 故障同码),此时名字、`player:zone` 都还没登记,只烧掉这一个 id;不看种子开关,每次建角都探。开着种子时,6d' 种子脚本报 `parent_exists` / `owner_epoch_exists`(探测与种子之间的窗口)也改为拒绝:先释放 6c 的名字,6d 已登记的 `player:zone` 撤不掉、留下一条,与第 7 步失败同类。详见 §10 P2 / P7 的结论注与 §13.2。

- 没有任何一步写 `PlayerAllData:{id}`。
- `player_class_backfill.go:18-20` 的注释原话就是:「PlayerAllData 在建角时还不存在,首次 EnterGame 才由预加载建出」。

### 2.2 首次 EnterGame:异步链里的预加载

`entergamelogic.go`:

- **入口**:拿玩家锁(:97-114),校验归属(账号 blob,或反查索引 + self-heal,:139-219)。
- **RPC 立即回「已受理」**,真正进场走异步链(:221-244)。
- **预加载**:放进 PreloadPool 执行 `EnsurePlayerAllDataInRedisAsync`(:381-391)。
  - 池容量 256,池满直接回 `kLoginInProgress`(:393-400;`login.yaml:332-334`)。
  - 每个读任务等结果的上限是 `dispatcherTaskTTL = 8s`(:356-380)。
  - **回调在哪个 goroutine 上跑**:
    - 快路径命中时,`onComplete` 在池 worker 里同步执行(`ensure_player_all_data_async.go:65-67`)。于是整段 `applyLoadedPlayerSession`(GetSession、身份补齐、SetSession、BindSession、EnterScene,:333、:685 起)都占着这个 worker。
    - 冷读路径的池 worker 发完 Kafka 就返回;apply 跑在 dispatcher 的 16 个 worker 或溢出 goroutine 上(`dispatcher/task_result_dispatcher.go:82`、:288-300)。

`go/login/internal/logic/pkg/dataloader/ensure_player_all_data_async.go`:

1. 先构造骨架 `{player_database{player_id}, player_database_1{player_id}}`(:49-52)。
2. 父键 `EXISTS` 命中,直接 `onComplete(nil)`。这就是快路径(:55-68)。
3. 父键不在,逐张子表 `GET` 子表缓存;全部命中就组装父键并写入(:80-109)。
4. 缺哪张子表,就为它发一条 Kafka 读任务(:157-207;`sync_loader.go:81-110`,`Op=read`,`WhereCase player_id='…'`)。
   - 回执经 `task:result` 的 Pub/Sub 送回。频道全局只有一个(`dispatcher/task_result_dispatcher.go:32`)。
   - **2026-10-18 更正**:上一句的行号指针已失效(`TaskResultNotifyChannel` 常量已删除),「频道全局只有一个」也不再成立。B0-3 分频道两批落码后(未编译、未验证):读任务带 `DBTask.reply_instance`(发起方 login 进程启动时生成的 UUID,`sync_loader.go` 的 `newDBReadTask`);login 在同一条 Pub/Sub 连接上同时订全服旧频道 `task:result:notify` 与本实例频道 `task:result:notify:<reply_instance>`(`dispatcher/task_result_dispatcher.go` 的 `subscribeLoop`);频道名与校验规则只来自 `go/shared/taskresult`。go/db 侧见 §2.3「结果回写」的更正。规格:`docs/handoff/launch-readiness-20261013/spec_B0-3_task_result_channel.md`。
5. 回执 `Data` 为空表示没有这一行,子表保持骨架(:180-190)。
6. 全部回齐后 `SET` 父键,TTL 24h(:128-155;`sync_loader.go:121-125`)。

对新号来说:父键不在(建角没写),子表缓存也不在,所以**必然**发 2 条读任务,并同步等它们回来。

### 2.3 go/db 的读任务

- 按 `MsgType` 把请求体反序列化成 msg(`go/db/internal/kafka/key_ordered_consumer.go:1994-2001`,工作区行号)。
- `handleDBReadOp` 在选中的落点库上执行 `FindOneByWhereClause`。查不到行(`ErrNoRowsFound`)不算错,msg 保持请求体原样(:339-348)。
- **选库顺序**:落点记录 → home_zone → 本进程 zone(`placement_route.go:178-189`)。
  - `Required=true` 且没有落点记录时,读回执直接失败(:297-305)。
- **结果回写**:
  - 写子表缓存 `{MsgType}:{id}`,TTL 取 `DefaultTTLSeconds`,K8s 上是 3600s(`key_ordered_consumer.go:54-64`、:360-373;`k8s_deploy.ps1:3458`)。
  - 再 `LPUSH task:result:{taskID}` + `EXPIRE` + `PUBLISH task:result:notify`(:391-414)。
  - **2026-10-18 更正**:B0-3 第 2 批起(未编译、未验证),`LPUSH` + `EXPIRE` 之后的通知改为按 `DBTask.reply_instance` **只 PUBLISH 一次**:合法时发到发起方 login 的实例频道 `task:result:notify:<reply_instance>`,为空(旧版 login)或不合法时回落全服旧频道 `task:result:notify`;不双发。通知失败或无人接收仍不影响结果(结果已在列表里,由 login 的 TTL 兜底取回),按 `db_task_result_notify_total{target,result}` 计数。代码:`key_ordered_consumer.go` 的 `publishTaskResult`,目标判定在 `go/db/internal/kafka/task_result_notify.go`;上一句的行号是更早的快照,按函数名定位。
- **新号冷读的最终产物就是「请求体原样」,也就是骨架**。
- **资源共享**:
  - 读任务和存盘写任务共用本 zone 10 分区 × 8 子分片的 worker(`go/db/etc/db.yaml:19`、:22),以及每实例 60 条的主池(:68)。
  - Phase 2 打开 `PinOnCreate` 后,读的是全局落点库,走每实例 8 条的额外池(`db.yaml:122-124`。注释自己写了这个池「会成为全局库的瓶颈」)。

### 2.4 身份补齐

`player_class_backfill.go`:

- `applyLoadedPlayerSession` 先 `GetSession`,再调 `backfillPlayerIdentity`(`entergamelogic.go:685-695`)。
- 先读父键(:35-47)。读不到就报错,这条链失败,客户端重试。
- 职业、名字、外观、性别,缺哪个补哪个(:57-83)。
- 用 Lua 做整字节 CAS 写回(:88-106、:179-191):
  - 校验玩家锁令牌;
  - `player:session:{id}` 或 `player:{id}:location` 存在就跳过;
  - `SET … KEEPTTL`,保留原有 TTL。

### 2.5 scene 加载与新号初始化(C++)

1. **铸 epoch**:scene_manager 的 EnterScene 用 INCR 铸 `owner_epoch`(`go/scene_manager/internal/logic/owner_epoch.go:55-119`),再把玩家路由到 scene。
2. **登记与加载**:scene 的 `PlayerEnterGameNode` 先登记待入场,再清继承来的交接标记,最后对父键发 `AsyncLoad`(`cpp/nodes/scene/handler/rpc/scene_handler.cpp:229-277`)。
3. **NIL 重试**:读到 NIL 时按 500ms / 1s / 2s / 4s / 8s / 16s 有界重试,合计约 31.5s(`redis_client.h` 的 `BackoffForRetry`)。
   - 同文件 `kMaxLoadRetries` 上方的注释写的是「2,4,…60s,约 2 分钟」,已经过时。
   - 重试用完仍是 NIL,就按 `DataNotFound` 喂一份空 `PlayerAllData`,当新号处理(`player_lifecycle.cpp:1911-1930`)。
   - 零长度值和解析失败一律按 `RedisError` 处理,fail-closed(`redis_client.h` 的 `OnLoaded`)。
4. **`HandlePlayerAsyncLoaded`**:`player_id==0` 时先补上 id(`player_lifecycle.cpp:2037-2052`),再进 `InitPlayerFromAllData`(:3495-3556)。
5. **首次注册**:`registration_timestamp <= 0` 就算首次注册。写注册时间、等级置 1、触发 `RegisterPlayerEvent`(:3529-3537),然后进场景并发出 `SNAPSHOT_LOGIN` 快照(:3555)。
6. **结论**:scene 判断「新号」看的是 `registration_timestamp`,不是父键在不在。冷读骨架、`DataNotFound` 兜底、种子,这三种输入走的是同一个分支。

### 2.6 首次存盘

- **写 Redis**:`SavePlayerToRedisImpl`(`player_lifecycle.cpp:3565` 起)。
  - `owner_epoch≠0` 时走带守卫的 Lua。KEYS 是父键和 `player:{id}:owner_epoch`(`redis_client.h` 的 `kSaveIfGuardLuaScript`)。
  - 否则走只 `SET` 父键的普通脚本(`redis_client.h` 的 `kSaveLuaScript`)。
  - *2026-10-16 更正*:上两条原按行号引用(`redis_client.h:42-52`、`:20-24`),普通路径原写「`SET` + `SADD dirty_keys_set`」。两段脚本里的 SADD 已删、普通脚本改名,行号随之漂移,改为按常量名引用。
  - 两条路都是不带 `KEEPTTL` 的 `SET`,父键从此不再过期。
- **写库**:两张子表各发一条 DBTask(`op=write`,带 `owner_epoch`,topic 按 home_zone 选)(`player_lifecycle.cpp:3693-3737`)。go/db 按落点路由、比对 epoch 后落库。
- **含义**:新号**第一次碰数据库**本来就可以是这条异步写。冷读给不了写路径任何它需要的东西。

### 2.7 每个新号首次进场的代价(推算,未计数)

- **login**:2 条 Kafka 读任务、dispatcher 两次登记、等回执(上限 8s)、一次父键 `SET`。
- **go/db**,每条读任务:
  - 选库和复核各一次落点 `MGET`(`placement_route.go:19-30`);
  - 一次点查;
  - 一次子表缓存回写;
  - 一组 `LPUSH` / `EXPIRE` / `PUBLISH`。`PUBLISH` 走单频道,会扇出到所有 login 实例。
  - **2026-10-18 更正**:上一条的「单频道、扇出到所有 login 实例」只在 B0-3 分频道上线前成立。两批(go/db 与 login)全量上线后(未编译、未验证),每条读任务仍是先 `LPUSH` + `EXPIRE`,再**只 `PUBLISH` 一次**,发到发起方 login 的实例频道 `task:result:notify:<reply_instance>`,只有发起方收到;`reply_instance` 为空(旧版 login)或不合法时回落全服旧频道 `task:result:notify`,这部分仍扇出到所有 login 实例。按 `db_task_result_notify_total{target,result}` 计数,见 §2.3「结果回写」的更正。两批全量之前按旧口径估。
- **两点后果**:
  - 这些都发生在玩家的加载屏上,是同步等待。
  - 它们和存盘写共用同一组 worker 和连接池。

## 3. 方案比选

| 方案 | 做法 | 改动面 | 出错时的形态 | 结论 |
|---|---|---|---|---|
| **A 建角写种子(只带 player_id)** | 写账号 blob 前用 Lua 写 `PlayerAllData:{id}` = 冷读骨架,`PX` 24h;只在父键和 owner_epoch 键都不存在时写 | login 3 个文件 + 配置;C++、go/db 零改动 | 种子缺席就走冷读老路,结果相同 | **选定** |
| A1 种子带身份 | 种子里直接写职业 / 名字 / 外观 / 性别 | 同 A | 同 A | 不选。身份写法会有建角、backfill 两份(违反 DRY);能省的只是首次进场一次 Redis CAS |
| A2 并进账号 blob 的围栏脚本 | 在 `writeAccountBlobScript` 里原子地顺手写种子 | 改围栏脚本,要两个版本 | 同 A | 不选。把缓存优化和账号目录这份权威写绑死;L-06 拆 Redis 后两把键不再同实例,原子性保不住 |
| B1 新号标记,由 scene 按新号初始化 | 建角写 `player:{id}:new`(短 TTL);预加载看到标记就放行,不写父键 | login,加上 scene_manager 路由事件加字段,加上 C++ | scene 读父键得 NIL,要等约 31.5s 有界重试才判新号(`redis_client.h` 的 `BackoffForRetry`)。想跳过,就得给 `RoutePlayerEvent` / `PlayerEnterGameNode` 加「新号」位,proto、Go、C++ 三处同改。标记过期前若已有存盘、父键又丢了(合服失效缓存、Redis 丢键),scene 会用空档覆盖真实进度 | 否决 |
| B2 新号标记,login 看到标记就写空父键 | 首次进场时,login 原子地执行「标记在、父键不在 → 写骨架、删标记」 | login | 要安全,必须再加「owner_epoch 不存在」这个判据;加上之后标记本身就多余了 | 否决。等于把 A 推迟到首次进场,还多一个键、多一个状态源 |
| C1 账号记录加 `first_enter_pending` | 在账号 blob 里标「未进场」 | login | 和 B 一样有「标记已过时却仍被当真」的问题;清标记要再拿一次账号锁写 blob | 否决 |
| C2 建角后异步预热 | 建角后 fire-and-forget 发读任务(`TriggerPlayerDataPreload`,`sync_loader.go:35-78`,目前没有调用方) | login | 等待挪出了玩家路径,但每个新号的 2 次 DB 读一次不少,全局库首日的读负载不变;子表缓存 1h 过期后,首登仍要冷读 | 否决,解决不了负载 |
| C3 go/db 负缓存 | 把「无行」结果缓存起来 | go/db | 第一次读仍然要打库 | 否决 |
| C4 scene 读到 NIL 立刻当新号 | 去掉 NIL 重试 | C++ | scene 分不清「新号」和「缓存丢了的老号」,老号会被空档覆盖 | 否决 |

## 4. 选定方案

### 4.1 种子内容

- 种子 = `newPlayerAllDataSkeleton(id)` = `{player_database{player_id}, player_database_1{player_id}}`。
- **不带身份,也不带 `registration_timestamp`**。带了注册时间,scene 就会跳过首次注册(`player_lifecycle.cpp:3529`)。
- 冷读骨架(`ensure_player_all_data_async.go:49-52`)也改成调用这同一个函数,两边始终同源。

### 4.2 写入点和顺序

写在 CreatePlayer 的 6d(归属登记)成功之后、6e/7(账号 blob)之前。

- **为什么在 6d 之后**:6c、6d 都是 fail-closed。建角失败时,种子根本不会写。
- **为什么在 7 之前**:
  - EnterGame 的归属校验只认账号 blob 或反查索引(`entergamelogic.go:155-219`),这两样在 7 / 7b 才写。
  - 所以种子落地之前,任何 EnterGame 都用不上这个 id。首次进场看到的父键只有两种可能:种子,或者缺席。
- **崩溃恢复**:AOF 和复制都是按序的,恢复后的数据是一个前缀。账号 blob 还在,种子就一定也在(除非已过期)。
- **代价**:多一次 Redis 往返,落在账号锁内(§9 第 1 条)。

### 4.3 写入脚本的语义

脚本是 Lua。`KEYS` = 父键、`player:{id}:owner_epoch`;`ARGV` = 种子字节、TTL 毫秒。按顺序判断:

1. owner_epoch 键已存在:返回 -1,不写。
2. 父键不存在:`SET … PX`,返回 1。
3. 父键的值正好就是本次种子:返回 2,算成功(计 `idempotent`)。成因有两种:
   - go-redis 把已经在服务端执行过的 EVAL 又重发了一次。建角调用点套了 1s 的 ctx 预算,读超时(3s)之后的重发到不了,只剩连接层快速失败(EOF、连接被重置)后的重发。
   - 这个 id 以前铸出过,当时写的种子还没过期。这是 id 复用。

   所以 `idempotent` 在稳态应接近 0。每次出现都打一条带 `player_id` 的 INFO 日志;持续出现要按 id 复用排查。
4. 其余情况:返回 0,不写。

**为什么要看 owner_epoch**:

- scene_manager 每次把玩家派进场景都会 INCR 这个键,而且从不删除(`go/shared/ownerepoch/ownerepoch.go:16-22`;`redis_client.h` 中 `kSaveIfGuardLuaScript` 上方注释)。
- 键存在,说明玩家进过场景。这时 scene 的存档才是真相,种子必须让路。
- 它兜住的是一种极端交错:种子请求在网络里滞留,等它落地时,玩家已经进场、存过盘,父键又恰好被删了(§6 第 5 行)。

### 4.4 读侧完全不改

以下都不动:

- `EnsurePlayerAllDataInRedisAsync` 的 `EXISTS` 快路径;
- 身份补齐;
- scene 加载;
- 存盘;
- go/db。

唯一的例外是 2026-10-13 评审并入的一个计数:EnterGame 在 PreloadPool 满的分支里多记一次 `entergame_total{result="pool_saturated"}`。它不改任何行为(§13.1)。

### 4.5 开关、指标、日志

| 项 | 内容 |
|---|---|
| 开关 | `login.yaml` 的 `NewPlayerSeed.Enabled`,bool,缺省 false;整块不写也是关 |
| 指标 | `login_create_player_seed_total{result}`,result ∈ written / idempotent / parent_exists / owner_epoch_exists / error |
| 指标 | `login_create_player_stage_seconds{stage="seed"}` |
| 指标 | `login_create_player_id_probe_total{result}`,result ∈ clear / parent_exists / owner_epoch_exists / error;`login_create_player_stage_seconds{stage="id_probe"}`。2026-10-15 并入(§10 P7、§13.2),不看种子开关,每次建角都有。2026-10-16 补:`error` 不含请求在探测途中结束的情形(只打 INFO、不计) |
| 指标 | `entergame_total{result="pool_saturated"}`:PreloadPool 满、入场链没启动的次数。只计数,不进 `entergame_total_seconds` 直方图;起服时预置为 0。2026-10-13 评审并入(§6 第 21 行、R8) |
| 启动日志 | `[new-player-seed] enabled=… (pin_on_create=…)` |
| 看效果 | `dataloader_preload_cache_check_seconds_count{hit="miss"}` 下降;go/db `db_task_result_total{op="read"}` 下降 |

## 5. 不变量

| # | 不变量 | 依据 |
|---|---|---|
| I1 | **新铸 id 从未被使用过** | 号段由 data_service 用 CAS 领段;客户端拒绝重叠或回退的范围(`go/shared/idsegment/client.go:26-34`)。默认不回退到 snowflake(`login.yaml:184-189`;`player_id_minter.go:22-29`);即使回退,snowflake 的值域 ≥2^55,和号段不相交(`client.go:36-51`)。全系统本来就依赖这一条:`player_database` 撞号不报错,而是静默串档(`createplayerlogic.go:219-222`) |
| I2 | **种子 ≡ 冷读读空后的组装结果** | 两边用同一个构造函数;§8 有等价性用例 |
| I3 | **种子永不覆盖已有数据** | 脚本只在父键和 owner_epoch 键都不存在时写 |
| I4 | **种子先于 id 对外可见** | §4.2 |
| I5 | **种子 TTL 24h,和冷读组装的父键相同;scene 首存后父键不再过期** | `sync_loader.go:121-125`;`redis_client.h` 的 `kSaveLuaScript`、`kSaveIfGuardLuaScript` |
| I6 | **父键和 owner_epoch 在同一个 Redis 实例** | 这是既有契约:C++ 守卫 Lua 在同一个脚本里访问这两把键(`redis_client.h` 的 `kSaveIfGuardLuaScript`);CZ-2 写明「PlayerAllData Redis 物理共享」(`cross-zone-scene-travel.md:31`)。种子没有引入新的同址约束 |
| I7 | **种子不是真源** | 任何失败都不改变建角结果,首次进场退回冷读 |

## 6. 失败与并发分析

| # | 场景 | 会发生什么 | 结论 |
|---|---|---|---|
| 1 | 号段 id 不复用(万一复用了) | 父键或 owner_epoch 已存在:不写,计 `parent_exists` / `owner_epoch_exists`,打 ERROR;首次进场读到的是已有缓存,和今天一样。都不存在、但库里有旧行(要求旧玩家的父键和 owner_epoch 在 Redis 里也都没了):种子让新角色从空档起步,首存**覆盖**旧行;今天则是冷读把旧行读给新角色,两个账号共用一份档。两者都是 ID 安全事故,但形态不同:今天是旧角色的资产串给新账号,旧行还在;开种子后资产不外泄,但旧玩家在库里的进度被覆盖,只能靠快照或备份找回 | 两种都不可接受,只能靠 I1 兜底。种子在 Redis 侧多给一个检测信号。是否升级为拒绝建角见 §10 P2。**2026-10-15 更正注**(§10 P2 / P7 已定):「会发生什么」一栏的前半句不再成立。父键或 owner_epoch 在建角之前就已存在时,建角在 6b' 只读探测处拒绝(计 `login_create_player_id_probe_total` 的 `parent_exists` / `owner_epoch_exists`,打 ERROR),名字、`player:zone` 不登记,种子不写,新账号拿不到这个 id,也就不会读到那份已有缓存;关着种子同样拒。探测与种子之间的窗口里才出现的,由种子脚本报出后同样拒绝(名字释放,`player:zone` 留下一条)。后半句「都不存在、但库里有旧行」那一形态 Redis 侧没有任何信号,探测拦不住,结论不变,仍只能靠 I1 兜底。**2026-10-16 补注**:写时原子复核只在开种子时存在,且只覆盖「探测 → 种子」一段;关种子(默认)时探测之后、开种子时种子之后,到第 7 步都没有复核,接受理由见 §13.2 的 2026-10-16 补注。6c 名字登记的 `FailedPrecondition`(该 id 名下已有另一个名字)是与时序无关的另一道拦截 |
| 2 | 建角在种子之前失败(发号 / 名字 / 归属) | 种子没写 | 无残留 |
| 3 | 建角在种子之后失败:账号 blob 围栏写回 -1 或报错,回读确认不含新角色(`createplayerlogic.go:621-656`) | 留下一个只带 player_id、24h 后过期的孤儿种子。这个 id 已经烧掉,没人能用它进场。约百字节 | 无害 |
| 3' | 回读本身失败(`accountBlobUnknown`) | 角色可能已经落盘,而种子恰好就是它正确的初始值 | 无害 |
| 4 | login 在种子前后崩溃 | 和第 2、3 行相同。崩在 EVAL 执行中途:要么写了,要么没写 | 都安全 |
| 5 | 种子写入结果未知,或请求滞留之后才落地 | 落地那一刻:父键 = 骨架(刚被冷读写入)→ 返回 2,不写;父键已被身份补齐改过 → 返回 0;玩家已经进场 → owner_epoch 存在,返回 -1 | 滞留的种子什么时候落地,都覆盖不了进度 |
| 6 | 种子和首次存盘的竞态 | 首存是无条件 `SET`,会覆盖种子,这是应该的;种子是条件写,永远覆盖不了首存 | 无论谁先谁后,都收敛到 scene 的数据 |
| 7 | 种子和冷读并发 | 只会在种子写失败或结果未知时出现。冷读写的也是骨架 | 结果一样 |
| 8 | 跨 zone 首登 | 父键在共享 Redis 里。K8s 上相关组件全部同址:C++ zoneredis(`k8s_deploy.ps1:1866-1868`)、login RedisClient(:3583-3586)、go/db(:3456)、scene-manager(:3720 起)。所以任何 zone 的 login 都能命中。归属重定向和访客照今天的规则走(`entergamelogic.go:646-683`);存盘按 home_zone 选 topic(CZ-2)。zone_redis 物理分开的部署本来就不支持(`cross-zone-scene-travel.md:31`) | 不变 |
| 9 | Phase 2 `PinOnCreate` | 种子和落点无关。打开 PinOnCreate 后,新号第一次碰全局落点库的是异步首存,不再有同步读 | 去掉了全局库首日唯一的同步读;误配置后的症状变化见 §9 第 2 条 |
| 10 | owner_epoch | 种子不铸 epoch,也不读它的值,只看键在不在。scene 首存的守卫补种和 CAS 照旧(`redis_client.h` 中 `kSaveIfGuardLuaScript` 及其注释)。旧版 scene_manager 不铸 epoch 的兼容窗口里,第 5 行的「玩家已进场」判据退化为只看父键 | 兼容窗口内只剩第 5 行的极端交错,且需要父键被删才会触发 |
| 11 | C++ 对空数据的处理 | 种子非空、`player_id≠0`、`registration_timestamp=0`,进首次注册分支,和冷读骨架同一路径(§2.5)。零长度和解析失败照旧 fail-closed | 不变 |
| 12 | Redis 丢键(重启丢最后 1s 的 AOF、人工删、合服失效) | 首次进场冷读,读到空 | 结果相同 |
| 13 | 合服 | copy 模式按清单删玩家缓存键,含 PlayerAllData(`tools/merge_zone/player_rows.go:676-723`;`unmerge.go:251-263`)。种子被删后在 dst 冷读,读到空。pin 模式保留缓存,种子仍是真值。合服在停服期间进行,窗口内没有在途建角。owner_epoch 不会被删,滞留的种子仍被挡住 | 安全 |
| 14 | zone 回档 | 灾难回档要 FLUSHDB,带共享键防护(`zone_data_rollback.md:163-170`),种子随之消失,首次进场冷读。应用级回档不碰没有快照的玩家:快照要进场后才有(`player_lifecycle.cpp:3555`)。孤儿只报告、不删除(`go/data_service/internal/logic/rollback_logic.go:1050-1090`) | 安全 |
| 15 | TTL 边界 | 种子恰好在「预加载 EXISTS 命中」和「scene GET」之间到期。① 到期发生在身份补齐之前:补齐读不到父键,链失败,客户端重试,改走冷读。② 发生在补齐之后、scene GET 之前:scene 先 NIL 重试约 31.5s,再按 DataNotFound 初始化空档;这一次会话里职业和名字为空,下一次干净登录时由身份补齐修正。窗口是一次进场链的耗时(百毫秒级)比上 24h。今天的首次进场碰不到这个窗口,因为冷读刚写的父键带满 24h;只有「预加载过、但还没被 scene 存过盘」的父键在下一次进场时会碰到。种子把它扩到「建角后约 24h 才首次进场」的新号 | 残余风险;可选加固见 §10 P4 |
| 16 | `player-storage-placement.md` §13 第 3 条的违规配置:`Required` 开着而新号没落点;`NewPlayerStorageId` 指向打不开的库 | 见 §9 第 2 条 | 要求两类告警先就位 |
| 17 | go/db 或 Kafka 不可用 | 新号仍能进场,存盘在 Kafka 里排队,和老玩家一样。今天新号会卡 8s 后失败 | 可用性变好 |
| 18 | 滚动升级 / 新旧混跑 | 新旧 login 的读路径相同,旧 login 只是不写种子。开关随时可关;已写的种子 ≡ 冷读结果,24h 自然过期 | 无需清理 |
| 19 | 展示类读者 | 种子不含身份,首次进场照旧由身份补齐写入。好友资料、外观回填读到种子时,按「缺字段」处理(`go/friend/internal/data/friend_profiles.go:44-62`;`role_appearance_backfill.go:50-52`),展示结果和读不到 blob 时一样 | 不变 |
| 20 | 资产账本 | 对未进场的新号,data_service `GetPlayerAssetOpLedger` 的结果从 absent 变成 found + 空账本(`asset_op_ledger_logic.go:36-44`、:117-123)。guild / trade 两种情况都判 UNKNOWN,继续等(`go/shared/assetop/classify.go:144-162`) | 语义不变,只是计数 label 变了 |
| 21 | 入场链占用哪个池(§2.2) | 开种子后,新号也走快路径:`applyLoadedPlayerSession` 整段在 PreloadPool 的 worker 里同步跑完,不再在 dispatcher worker 上跑。每个新号占用池 worker 的时间,从「发完 Kafka」变成「整段 apply」,其中含 player_locator、scene_manager 两次 gRPC 和一次 Kafka 同步发送。池是非阻塞提交,Size 256(`login.yaml:332-334`),池满回 `kLoginInProgress`,客户端重试。另一面,dispatcher 不再替新号跑 apply,`cb_wait` 的排队压力变小。老玩家命中缓存今天就是这条路 | 背压形态变了,不是正确性问题。L-14 开、关两轮都要看池掉单指标 `entergame_total{result="pool_saturated"}`(`stress_summarize.ps1` 的 pool_saturated 列;`[preload-pool] dropped_delta` 日志作辅证)和 `entergame_apply_*` 各阶段;出现掉单就调大 `PreloadPool.Size`(只改配置) |

## 7. 上线与回退

**上线步骤**

1. 落码,默认关。Codex 跑单测(§8)和 §13 列的验证命令。
2. 本地冒烟:临时打开 `login.yaml` 的开关,用 robot 建 N 个新号进场,核对指标。
3. 告警先就位:
   - go/db `db_placement_guard_total{op="write",outcome="missing_required"}`(违规配置一:Required 开着而新号没落点);
   - go/db `db_placement_store_open_total{result=~"error|rejected"}`,以及 go/db 的 ERROR 日志 `retry task rescheduled`,其 cause 含 `placement store is unavailable`(违规配置二:`NewPlayerStorageId` 指向打不开的库。写任务按 `errPlacementStoreUnavailable` 延后、不耗重试预算,会一直在重试队列里转,`placement_route.go:36-37`、`key_ordered_consumer.go:2072-2107`);
   - `login_create_player_seed_total{result=~"parent_exists|owner_epoch_exists"}`。
4. 开服尖峰压测(L-14)里开、关各跑一轮,按 AGENTS §6 出对比表:首次进场 p99、db_task 读量、Redis ops,以及 PreloadPool 掉单、`kLoginInProgress` 次数、`entergame_apply_*` 各阶段(§6 第 21 行)。
   - PreloadPool 掉单看指标 `entergame_total{result="pool_saturated"}`,即 `stress_summarize.ps1` EnterGame 表的 pool_saturated 列。AGENTS §6.1 规定复盘只读这个脚本的输出,日志进不了对比表。
   - login 日志 `[preload-pool] dropped_delta` 只作辅证。
   - 建角锁内新增的那一步看同一脚本 CreatePlayer stages 表的 seed 列;开关关着时这一列是 n/a,可以用来核对本轮的开关状态。
5. K8s 打开:单独一批做,契约测试同步改。除 `NewPlayerSeed.Enabled` 外,同批必须补齐 login 在 K8s 上的观测面(同一缺口见 `docs/handoff/launch-readiness-20261013/spec_G4b.md` 的 R6):
   - login ConfigMap 加 `Prometheus`(Host / Port / Path),`deploy/k8s/manifests/go-svc/login.yaml` 加抓取端口与 prometheus.io 注解;
   - login ConfigMap 加 `PreloadPool`(Size、StatsInterval)。整块缺失时 go-zero 不填内层默认值,`StatsInterval` 为 0,`[preload-pool]` 日志不打。
   - 缺这两项时,第 3 步的 login 告警和第 4 步的池掉单指标、日志在集群里都没有数据源,只剩 ERROR 日志。
   - L-14 若在 K8s 上跑,这两项要排在压测之前。

**回退**

- 改回 false 并滚动重启 login(go-zero 配置不热加载),或回滚 login 镜像即可。
- 已写的种子和冷读结果相同,24h 后过期;不需要任何数据迁移。
- 打开和关闭都不要求和 C++ / go/db / scene_manager 同批发布。

## 8. 测试

| 层 | 用例(文件见 §13) | 外部依赖 |
|---|---|---|
| dataloader | 种子内容与 TTL;重发幂等;不覆盖已有父键;owner_epoch 存在就拒写;零 id / nil 客户端;Redis 不可用;**种子 ≡ 冷读骨架**(子表缓存给出「无行」时,`EnsurePlayerAllDataInRedisAsync` 写出的父键和种子 `proto.Equal`);种子命中后快路径不碰 Kafka 和 dispatcher | 无(miniredis) |
| CreatePlayer | 开关关 → 不写;开关开 → 写骨架并带 TTL;归属登记失败 → 不写;账号 blob 围栏失败 → 只留会过期的种子;已有父键 → 不覆盖并计数;owner_epoch 已存在 → 不写并计数;种子脚本报错 → 建角照常成功并计 `error`;请求 ctx 在持锁之后被取消 → 建角失败,但建角锁已释放(§13.1) | 无(miniredis + 假 data_service) |
| EnterGame | 种子 → EnterGame:走快路径,身份补齐写进种子,TTL 保留,没有子表缓存键;PreloadPool 满 → 回 `kLoginInProgress`、释放玩家锁、计 `pool_saturated` 且不进耗时直方图;起服预置后 `pool_saturated` 序列存在(§13.1) | 无(miniredis + ants 池 + 假 player_locator) |
| 配置 | 整块缺失 = 关;两份样例 yaml 显式写出且为关;探针拒绝拼写错误 | 无 |
| C++ | 不改代码,不新增用例。scene 的新号初始化分支今天也被冷读路径走着,没有单测是既有缺口(§10 R7) | — |
| 端到端 | 本地全栈冒烟(可选),开服尖峰压测(L-14) | 本地 MySQL / Redis / Kafka / etcd / C++ 节点 |

## 9. 与现有设计的偏离与代价

1. **建角持锁时间多一次 Redis 往返**。
   - ctx 预算 1s,只能掐掉 go-redis 的重发;单次最坏仍是 3s 读超时。
   - `login.yaml` `Locker` 段的锁 TTL 预算注释要同步更新,而且要写出真实的数:
     - 原注释的最坏和是 gRPC 3×3s + Redis 3×3s(读 blob、围栏写、回读)= 18s,「20 盖住」成立;
     - 开种子后 Redis 变成 4 次,各步预算直接相加是 21s,数值上超过 `AccountLockTTL: 20`。
   - **但 21s 在现行配置下到不了**(2026-10-13 评审更正;初稿写的是「开着种子时 20 不再盖得住」):
     - zrpc 服务端 `Timeout: 10000`(`login.yaml:7-13`)给 handler 的 ctx 套了 10s 截止时间。除回读、名字补偿释放和建角锁释放外,建角每一步的 ctx 都派生自它。
     - 10s 一到,gRPC 立即失败;go-redis 在取连接之前先看 ctx,不再发新命令。
     - 所以围栏写最晚在 handler 起算 10s 内发出。Redis 还在 3s 内回包时,它约在 13s 前执行,20 仍盖得住。有没有种子都一样。
     - 把 zrpc `Timeout` 调到约 17s 以上时,21s 才可达,届时要重算(§10 P6)。
   - 种子在慢 Redis 下的真实代价是另一件事:它多吃最多 3s 的 handler 预算,围栏写可能来不及发出,本次建角失败。失败时名字会释放,`player:zone` 映射和孤儿种子留下。
   - 原注释的口径本来就是「20 不是硬上界」,正确性靠围栏 + 回读。本批不改 TTL,只把注释改成如实的数。
   - zrpc `Timeout` 的注释按「常态 Redis 毫秒级」估算。种子在常态下也是毫秒级,所以不用改。
   - **建角锁的释放**(2026-10-13 评审并入,既有缺陷):原先用请求 ctx 释放。handler ctx 一过期,释放命令发不出去,锁要等 `AccountLockTTL` 自然过期,期间同账号重试建角一律 `kLoginInProgress`。开种子后建角越过 10s 的机会略增,所以一并修:改用脱离请求 ctx 的 1s 预算释放(`createLockReleaseTimeout`)。释放按锁令牌比对,删不到别人的锁。`loginlogic.go` 里同类的 `accountLock.Release(l.ctx)` 不在本项范围,没有改。
   - **2026-10-15 补(§10 P7 已定)**:6b' 只读探测让持锁期间的 Redis 再多一次往返(两条 `EXISTS` 走一个 pipeline;1s ctx 预算同样只掐重发,单次最坏仍是 3s),而且不看种子开关:Redis 变成 4 次(关种子)/ 5 次(开种子),各步预算直接相加是 21s / 24s。探测的 ctx 派生自 handler ctx、排在围栏写之前,上面「围栏写最晚在 handler 截止时发出」的推导不变。现行 zrpc `Timeout` 已是 4000(`login.yaml` 顶部,2026-10-14 起,不归本项),`login.yaml` `Locker` 注释按 4s 算出的「合计约 10s、20 仍盖得住」同样不受影响,已在那里追加一句。上调 `Timeout` 按 P6 重算时把这一次算进去。
   - **2026-10-16 更正注**(上一条):「1s ctx 预算同样只掐重发」不完整。login 的 Redis 客户端没开 `ContextTimeoutEnabled`,go-redis 不按 ctx 截断 socket 读写,但取连接(连接池排队 `waitTurn`)与拨号认 ctx,所以 1s 子预算会让连接池等满超过 1s 的探测以 `DeadlineExceeded` 失败;探测又是 fail-closed,它就成了唯一一个预算短于 handler、又会拒绝建角的 Redis 步骤(读 blob、围栏写都直接用 handler ctx),并被记成一次 `id_probe_total{error}`。现已改为直接用 handler ctx(删去 `mintedIDProbeTimeout`),对池等待的容忍度与读 blob、围栏写一致;本条「围栏写最晚在 handler 截止时发出」的推导不变(6c 之后的每次写之前都查请求 ctx)。请求 ctx 在探测途中结束(取连接 / 拨号时返回 ctx 错误)的,仍拒绝,但只打 INFO、不计探测指标(§13.2 行为表 2026-10-16 补行)。同类说法「ctx 预算只掐重发」也出现在本条第 1 小条与代码里的 `newPlayerSeedTimeout`、`accountBlobReadBackTimeout`、`createLockReleaseTimeout`、`reverseMappingWriteTimeout` 注释上:它们同样会截断连接池等待,但这几步要么尽力而为(种子池等满超过 1s 记 `error`、建角照常),要么不决定建角拒不拒,行为上无害,本次没有改那几处代码注释。
2. **失去一个早期信号:`player-storage-placement.md` §13 第 3 条的两种违规配置**。
   - `login.yaml:229-231`、`deploy/login-stack.linux/login.yaml:150`、`config.go:163-166` 和 `player-storage-placement.md:448` 写的症状都是「新号进游戏预加载秒级失败、存盘全进死信」。
   - 开种子后,新号不再走预加载读,症状只剩存盘这一半:
     - Required 开着而新号没落点:存盘全进死信(`placement_route.go:297-303`)。
     - `NewPlayerStorageId` 指向打不开的库:写任务按 `errPlacementStoreUnavailable` 延后,不耗重试预算,一直在重试队列里转(`placement_route.go:36-37`、`key_ordered_consumer.go:2072-2107`)。今天读任务会耗尽重试,新号进不去,所以很响亮。
   - 两种情况下玩家都能进场。进度只在 Redis 父键里(首存后不过期),不进 MySQL;Redis 丢键或合服 copy 模式失效缓存时,进度就真的丢了。
   - 所以要求:打开种子之前,§7 第 3 步的 `missing_required` 和 `db_placement_store_open_total{result=~"error|rejected"}` 两类告警必须到位;§13 第 2、3 条的打开顺序不变。上面四处症状描述都补一句「开着 NewPlayerSeed 时只剩存盘这一半」。
3. **Redis 内存**。未进场角色的种子和孤儿种子各约百字节,24h 后过期。进过场的玩家,父键今天就常驻。
4. **压测口径**。开关前后,dataloader 各阶段表和 entergame preload 分布都会变。压测对比时必须注明开关状态。
5. **注释过时**。
   - `player_class_backfill.go:18-20`:本批一起改。
   - `player_lifecycle.cpp:1916-1922`「CreatePlayer only writes account meta」、`player_lifecycle.cpp:2037-2041`「CreatePlayer only creates an account entry」:C++ 注释,随下一个 C++ 批次改,不为一句注释单独跑 MSBuild。
6. **和 L-05 原文一致**。L-05 写的是「建角时写一个空的 PlayerAllData」,本方案就是这个,不是「新号标记」。
7. **新号入场链的背压形态变了**(§6 第 21 行)。
   - 今天新号的 apply 跑在 dispatcher worker 上,开种子后跑在 PreloadPool worker 上。
   - 后者容量有上限,而且是非阻塞提交,池满回 `kLoginInProgress`。
   - 这条路老玩家命中缓存时今天就在走,不是新机制;但开服首日新号占大头,池的占用时长会明显变长。
   - L-14 压测必须观察池掉单(指标 `entergame_total{result="pool_saturated"}`,§7 第 4 步);出现掉单就调大 `PreloadPool.Size`。

## 10. 风险与待负责人拍板

### 风险

| # | 风险 | 处置 |
|---|---|---|
| R1 | 整个方案建立在「号段 id 不复用」上,这也是全系统的前提。万一复用、而 Redis 侧旧状态也没了,串档形态会从「旧行被新账号继承」变成「旧行被新角色首存覆盖」(§6 第 1 行) | 种子顺带提供了检测信号:`parent_exists` / `owner_epoch_exists` 计数 + ERROR 日志。**2026-10-15 补**:信号命中即拒绝建角(P2 / P7 结论注);建角前就在的状态由 6b' 探测拦下,另计 `login_create_player_id_probe_total`,不看种子开关 |
| R2 | TTL 边界(§6 第 15 行) | 默认接受;可选加固见 P4 |
| R3 | §13 第 3 条的两种违规配置变隐蔽(§9 第 2 条) | 两类告警作为打开前置 |
| R4 | L-06 拆 Redis 时,父键、owner_epoch、身份补齐 Lua 里的会话 / 位置 / 玩家锁键必须仍在同一实例 | 这是既有约束,种子没有新增。拆分方案里要点名这一组 |
| R5 | 账号锁内多一次 Redis;Redis 卡顿时最坏多 3s | 正确性靠围栏,不受影响;观察 `stage="seed"` |
| R6 | 收益没有实测 | 走 L-14 对比表;拿不出表不下性能结论 |
| R7 | C++ 新号初始化分支没有单测 | 既有缺口,种子没新增风险;可在后续 C++ 批次补 |
| R8 | 新号入场链改占 PreloadPool,池满回 `kLoginInProgress`(§6 第 21 行、§9 第 7 条) | L-14 看池掉单(`entergame_total{result="pool_saturated"}`);按需调大 `PreloadPool.Size` |

### 待拍板

| # | 事项 | 本稿默认 |
|---|---|---|
| P1 | 采用方案 A(种子只带 player_id,不带身份) | 采用 |
| P2 | id 复用信号是否升级为拒绝建角 | **只告警**(仍待拍板;依据在 2026-10-13 评审后更正)。种子报 `parent_exists` / `owner_epoch_exists` 时,这个 id 上已有既有状态。旧持有者若经现行建角流程创建,它的 `player:zone` 映射已存在:同 zone 时 6d 是幂等空操作,拒绝不新增映射;不同 zone 在 6d 已被拒,到不了这里。只有旧持有者没有映射时才会留下一条,与第 7 步失败同类。只告警的代价:已确知 id 上有既有状态,仍把它交给新账号,首次入场读到那份缓存(与关着种子时相同,但此刻本可拦下)。若改为拒绝,要像 6d 失败分支一样先释放 6c 登记的名字,`TestCreatePlayer_SeedNeverOverwritesExistingParent` 与 `TestCreatePlayer_SeedRefusedAfterOwnerEpochMinted` 的期望同步改。可选做法见 P7。**负责人 2026-10-15 按『最标准』定为:采用 P7 且 P2 升级为拒绝**(上面「只告警(仍待拍板)」不再适用)。种子脚本报 `parent_exists` / `owner_epoch_exists` 时拒绝建角,tip 复用 6c / 6d 故障时的 `kLoginDataSerializeFailed`(不新增 tip 码);先按「确定已登记」释放 6c 的名字;6d 已登记的 `player:zone` 映射撤不掉 —— login 没有删除入口,data_service 的 `DeletePlayerData` 会连同这个 id 的玩家数据一起删,而 id 复用时删的正是旧持有者的数据,不能用 —— 留下一条并记 ERROR,与第 7 步失败口径一致。走到这一步的只剩探测与种子写之间窗口里冒出的状态,建角前就在的由 P7 的探测拦下。两条用例改为在这个窗口里注入状态、期望改为拒绝 + 名字释放(§13.2) |
| P3 | 何时在 K8s / 开服档打开 | L-14 对比表出来、告警就位之后;另起一批改 `k8s_deploy.ps1`,同批补齐 login 在 K8s 上的 `Prometheus` 与 `PreloadPool`(§7 第 5 步) |
| P4 | TTL 边界加固:快路径命中、且父键剩余 TTL 低于阈值时续期 | **不做**。窗口极小,且能自愈 |
| P5 | `missing_required` 与 `db_placement_store_open_total{result=~"error|rejected"}` 两类告警是否作为硬门禁 | 是 |
| P6 | 开种子后 `AccountLockTTL` 是否从 20 上调到 ≥22,让各步预算直接相加的 21s 仍被盖住(§9 第 1 条) | **不调**。仅当 zrpc `Timeout` 上调到约 17s 以上时才需要考虑;现行 10s 下 21s 到不了,20 仍盖得住(§9 第 1 条)。20 本来也不是硬上界,正确性靠围栏;上调的代价是 login 崩溃后同账号建角被挡得更久。`k8s_deploy.ps1` 从 `login.yaml` 读这个值,要调只改 `login.yaml` 一处,K8s 自动跟随 |
| P7 | P2 的可选做法:在 6c(名字登记)之前加一次只读探测(`EXISTS` 父键与 `player:{id}:owner_epoch`),命中即拒绝建角 | **未采用,待拍板**。好处:拒绝发生在名字和 `player:zone` 登记之前,只烧掉一个 id,不留任何持久状态,也不用补偿释放名字。代价:开种子时每次建角多一次 Redis 往返,落在账号锁内;探测与 6d' 的种子写之间仍有窗口,种子脚本的条件判断要保留。若采用,P2 提到的两条用例期望同步改。**负责人 2026-10-15 按『最标准』定为:采用 P7 且 P2 升级为拒绝**(上面「未采用,待拍板」不再适用)。落点 6b':发号之后、名字登记之前。探测两条 `EXISTS`(键名与种子脚本共用 `newPlayerStateKeys`)走一个 pipeline,一次往返;命中或探测本身出错都拒绝建角(fail-closed),不登记名字、不登记 `player:zone`、不写种子,打带 `player_id` 的 ERROR,计 `login_create_player_id_probe_total{result}`(clear / parent_exists / owner_epoch_exists / error,后三个起服预置为 0),阶段耗时记 `stage="id_probe"`。**不看种子开关**:login 的 `RedisClient` 就是 EnterGame 预加载读父键的那个实例,关着种子也拿得到,而 id 复用在关着种子时同样会让新账号读到旧缓存;所以代价一栏的「开种子时每次建角多一次 Redis 往返」改为「每次建角都多一次」。请求 ctx 已截止时不发探测、直接拒绝(不计探测指标)。种子脚本的条件判断保留为写时的原子复核。**2026-10-16 补注**:该复核只在开种子时存在,且只覆盖探测 → 种子一段;关种子(默认)时探测之后、开种子时种子之后,到第 7 步都没有复核,接受理由见 §13.2 的 2026-10-16 补注。请求 ctx 在探测途中结束的(go-redis 取连接 / 拨号时返回 ctx 错误)同样不计探测指标、打 INFO,仍拒绝;探测不再另设 1s 子预算,直接用 handler ctx(§9 第 1 条 2026-10-16 更正注) |

## 11. Java 版(AGENTS §12)

**状态:不适用**(服务端内部实现不同,客户端契约不变)。

- Java 建角在事务里直接插入玩家行(`game-login/.../CreatePlayerHandler.java:222` `createPlayerWithinCap`)。
- 进场时按主键读本库:`EnterGameHandler.java:273` `findPlayer`;scene 侧 `StoragePlayerRepository.java:183-195`。
- 所以 Java 没有「读一行不存在的数据、还要经队列同步等待」这条链,也就没有要优化的冷读。
- 本功能不改任何客户端可见的消息、tip 码或配表。按 §12,在 `PARITY.md` 登记一行「不适用」即可。

## 12. 和「全服库成了瓶颈怎么办」的关系

- **「全局库」有两个,要分开看**:
  - data_service 全局库:放名字、号段、流水、快照,以及 D1 的 `player_route`;
  - Phase 2 的玩家主数据全局落点库 `player_store_1000000_db`。
- **G3 处理的是后者首日唯一的同步读路径**(tidb-phase1 L-05;`db.yaml:122-124`)。打开种子后:
  - 新号首日对全局落点库只剩异步写。它有 Kafka 削峰,积压表现为 lag,不会让玩家卡在加载屏。
  - 老玩家的父键常驻 Redis(scene 存盘会去掉 TTL),也不冷读。
  - 于是全局落点库从「玩家同步等待的依赖」变成了「异步落库的下游」。
- **写侧瓶颈仍在,不归 G3 解决**:单调主键的尾部 Region、每实例 8 条的额外池、每 zone 固定 10 分区。见 tidb-phase1 §5、L-08、L-13。
- **归属目录(D1 `player_route`)**每个玩家一生只写一次,见提案 §12。

## 13. 落码清单与验证(2026-10-13)

按 §10 的默认值落码:采用 A;id 复用只告警;K8s 不打开;不做 TTL 续期;两类告警作为打开前置;`AccountLockTTL` 不调。**未编译、未测试**,待 Codex 验证。

**新增**

| 文件 | 内容 |
|---|---|
| `go/login/internal/logic/pkg/dataloader/new_player_seed.go` | `newPlayerAllDataSkeleton`(冷读与种子共用)、`SeedResult`、`seedNewPlayerScript`、`SeedNewPlayerAllData` |
| `go/login/internal/logic/pkg/dataloader/new_player_seed_test.go` | 8 个 miniredis 用例,含「种子 ≡ 冷读骨架」等价性用例 |
| `go/login/internal/logic/clientplayerlogin/createplayer_seed.go` | `newPlayerSeedTimeout`、`(*CreatePlayerLogic).seedNewPlayer`:尽力而为,只计数和打日志 |
| `go/login/internal/logic/clientplayerlogin/createplayer_seed_test.go` | 7 个 CreatePlayer 用例 + 1 个 EnterGame 快路径用例 |
| `go/login/internal/config/new_player_seed_conf_test.go` | 4 个配置用例 |

**修改**:`ensure_player_all_data_async.go`(骨架改调共享构造,删掉不再使用的 import)、`createplayerlogic.go`(6d' 步)、`clientplayerlogin/metrics.go`(`createStageSeed`、`login_create_player_seed_total`、预置两个 id 复用结局)、`player_class_backfill.go`(只改注释)、`config/config.go`(`NewPlayerSeed` 字段与 `NewPlayerSeedConf`)、`svc/home_zone.go`(启动日志)、`go/login/etc/login.yaml` 与 `deploy/login-stack.linux/login.yaml`(显式关闭的块、锁预算注释、Placement 第 3 条补句)、`player-storage-placement.md:448`(补句)。

**不改**:C++、go/db、scene_manager、data_service、proto、`tools/scripts/k8s_deploy.ps1`。

**交 Codex 的验证**(工作目录 `go/login`,串行,不需要外部服务):

```powershell
gofmt -l internal/logic/pkg/dataloader/new_player_seed.go internal/logic/pkg/dataloader/new_player_seed_test.go internal/logic/clientplayerlogin/createplayer_seed.go internal/logic/clientplayerlogin/createplayer_seed_test.go internal/config/new_player_seed_conf_test.go
gofmt -d internal/logic/pkg/dataloader/ensure_player_all_data_async.go internal/logic/clientplayerlogin/createplayerlogic.go internal/logic/clientplayerlogin/metrics.go internal/logic/clientplayerlogin/player_class_backfill.go internal/config/config.go internal/svc/home_zone.go
go vet ./internal/logic/pkg/dataloader/... ./internal/logic/clientplayerlogin/... ./internal/config/... ./internal/svc/...
go build ./...
go test ./internal/logic/pkg/dataloader/... -run "Seed|Skeleton|SeededParent" -count=1 -v
go test ./internal/config/... -run "NewPlayerSeed|Placement|EtcYaml|LoginYaml" -count=1 -v
go test ./internal/logic/clientplayerlogin/... -run "Seed|CreatePlayer|RegisterHomeZone|EnterGame|BackfillPlayerIdentity" -count=1 -v
go test ./... -count=1
```

通过标准:第一条 gofmt 零输出,第二条的 diff 里没有本批改动的行;vet / build 零报错;20 个新用例全部 PASS(不得 SKIP),已有用例全绿。`-race` 本机无 gcc 时跳过并如实报告。仓库根再跑 `python tools/scripts/gen_docs_index.py --check`,退出码 0。

**后续批(不在本批)**:

- `k8s_deploy.ps1` 的 login ConfigMap 加这一键(待 P3)。同批必须补齐 login 在 K8s 上的观测面:ConfigMap 加 `Prometheus` 与 `PreloadPool`,`deploy/k8s/manifests/go-svc/login.yaml` 加抓取端口与 prometheus.io 注解(§7 第 5 步)。缺这两项时,§7 第 3 步的 login 告警和第 4 步的池掉单在集群里没有数据源。L-14 若在 K8s 上跑,排在压测之前。
- C++ 三处过时注释(`player_lifecycle.cpp:1916-1922`、`:2037-2041`,`redis_client.h` 中 `kMaxLoadRetries` 上方注释)。
- `stress_summarize.ps1` 的 dataloader 表加 hit/miss 计数列(可选);再加一行 `login_create_player_seed_total` 各 result 的计数(可选)。CreatePlayer stages 表的 seed 列和 EnterGame 表的 pool_saturated 列已在 §13.1 加上。
- `loginlogic.go` 的 `accountLock.Release(l.ctx)` 与建角锁修复前是同一种写法(§9 第 1 条),待单独处理。
- `docs/design/guild-phase2/03-names.md:456` 与 `92-handoff.md:408` 仍把建角阶段列成四个,没有 seed。这两份是帮会二期的文档,本批没有改。

### 13.1 三视角评审修复(2026-10-13)

三视角评审加对抗核实确认了十条问题,按负责人当天的拍板全部处置。**未编译、未测试**,待 Codex 验证。

| 条目 | 处置 | 文件 |
|---|---|---|
| G3-compile-1 | `chainBudget` 改回 5 分钟。它是被误带进提交的改动,与 `login.yaml:9` 的注释、[历史客户端设计](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md) 重新一致 | `entergamelogic.go` |
| G3-consistency-3 | 池满(PreloadPool 满、链未启动)计 `entergame_total{result="pool_saturated"}`,只计数,不进耗时直方图;起服由 `PrimeEnterGameMetrics` 预置为 0;汇总脚本 EnterGame 表加 pool_saturated 列,缺序列打印 n/a | `entergamelogic.go`、`clientplayerlogin/metrics.go`、`login.go`、`stress_summarize.ps1`、`test_stress_summarize.ps1`、`login.yaml`、本文 §6 / §7 |
| G3-consistency-4 | 建角锁的释放改用脱离请求 ctx 的 1s 预算(`createLockReleaseTimeout`) | `createplayerlogic.go` |
| G3-consistency-1 | 汇总脚本 CreatePlayer stages 表加 seed 列,五个阶段 | `stress_summarize.ps1`、`test_stress_summarize.ps1` |
| G3-consistency-5 | 种子 `error` 的日志与注释改为「写入未确认」;`idempotent` 单独打 INFO,成因说明更正 | `createplayer_seed.go`、`clientplayerlogin/metrics.go`、`dataloader/new_player_seed.go`(只改注释)、本文 §4.3 |
| G3-consistency-6 | P2 的依据更正,只改文字,不改行为;可选做法列为 P7 | `createplayer_seed.go`(只改注释)、本文 §10 |
| G3-spec-2 | K8s 打开步骤补上 login 的观测面缺口 | 本文 §7 第 5 步、§13 后续批 |
| G3-spec-3 | 锁预算:21s 在现行 zrpc `Timeout` 10s 下到不了;P6 的前提改写 | `login.yaml`、本文 §9 第 1 条、§10 P6 |
| G3-spec-4 | 配置用例对两份样例 yaml 都断言 `Enabled=false` | `config/new_player_seed_conf_test.go` |
| G3-spec-5 | `PreloadPool` 的旧注释(「池满时丢任务,scene 侧重试补偿」)更正为与代码一致 | `login.yaml`、`svc/servicecontext.go`(只改 `SubmitPreload` 的函数注释) |

**新增用例**(3 个 Go 用例,1 个新文件):

- `createplayer_name_test.go` 的 `TestCreatePlayer_ReleasesLockAfterRequestCancelledMidCreate`:请求 ctx 在持锁之后才取消,建角返回后锁必须已删。**修复前应红**。
- `entergame_pool_saturated_test.go`(新文件)的 `TestEnterGame_PoolSaturatedCountsWithoutLatencySample`:池满回 `kLoginInProgress`、玩家锁已释放、`pool_saturated` 计数 +1、耗时直方图里没有这个取值。
- 同一文件的 `TestPrimeEnterGameMetrics_ExposesPoolSaturatedSeries`:预置之后序列存在。
- `test_stress_summarize.ps1` 新造两份 login 快照(有序列 / 缺序列),断言两张表的表头与四行数据。

**没有做的**:`deploy/login-stack.linux/login.yaml` 没有改动(它没有那段旧的 PreloadPool 注释,锁预算注释也只在 etc 那份)。

**交 Codex 的验证**(先在 `go/login` 下串行跑,不需要外部服务;最后一条在仓库根):

```powershell
gofmt -l internal/logic/clientplayerlogin/entergame_pool_saturated_test.go internal/logic/clientplayerlogin/createplayer_name_test.go internal/logic/clientplayerlogin/createplayer_seed.go internal/config/new_player_seed_conf_test.go internal/logic/pkg/dataloader/new_player_seed.go
gofmt -d internal/logic/clientplayerlogin/createplayerlogic.go internal/logic/clientplayerlogin/metrics.go internal/logic/clientplayerlogin/entergamelogic.go internal/svc/servicecontext.go login.go
go vet ./internal/logic/clientplayerlogin/... ./internal/logic/pkg/dataloader/... ./internal/config/... ./internal/svc/... .
go build ./...
go test ./internal/logic/clientplayerlogin/... -run "TestCreatePlayer_ReleasesLockAfterRequestCancelledMidCreate|TestEnterGame_PoolSaturated|TestPrimeEnterGameMetrics" -count=10 -v
go test ./internal/config/... -run "NewPlayerSeed|EtcYaml|LoginYaml" -count=1 -v
go test ./internal/logic/clientplayerlogin/... ./internal/logic/pkg/dataloader/... ./internal/config/... ./internal/svc/... -count=1
pwsh -NoProfile -File tools/scripts/test_stress_summarize.ps1
```

通过标准:

- 第一条 gofmt 零输出;第二条的 diff 里没有 §13.1 改动的行(这些文件里别的会话的改动不归本批)。
- vet / build 零报错。
- 三个新用例各 10 遍全部 PASS,不得 SKIP;`TestLoginYamlSpellsOutNewPlayerSeedKeys` 的两个子用例 PASS;其余已有用例全绿。
- `test_stress_summarize.ps1` 末行是 `PASS`,退出码 0。它会临时挪走并还原 `run/logs/cpp_nodes/scene*.log`,跑的时候不要同时起 scene。
- 「修复前应红」的核对(可选):在 HEAD 的临时 worktree 里只带上 `createplayer_name_test.go` 的新用例跑一遍,应在 `create lock still held` 处失败。
  - *2026-10-16 更正*:同 §13.2 的更正,这段修复与本文字已随 `e4ff1d165` 进入历史,之后的 HEAD 都含修复;基线改为 `e4ff1d165^`,跑不起来就记「不可行、未做」,不要退回 HEAD 上跑。

失败时保留:失败用例的完整 `-v` 输出;`test_stress_summarize.ps1` 失败时它打印的整段 stress_summarize 输出,必要时加 `-KeepArtifacts` 保留临时目录。前一步红了就停下归因,不连续重试(AGENTS §10.2)。

### 13.2 P2 / P7 定案后的落码(2026-10-15)

负责人 2026-10-15 指示「按最标准修复」,§10 P2 / P7 定为:采用 P7(6c 之前只读探测),P2 升级为拒绝。取舍依据 AGENTS §11.3:玩家资产与数据完整性路径默认 fail-closed;能在副作用前完成的纯校验先完成(探测),提交点仍由同一机制原子复核(种子脚本的条件判断)。**未编译、未测试**,待 Codex 验证。

> **2026-10-16 补注**(更正上段「提交点仍由同一机制原子复核」):这句不成立。建角的提交点是第 7 步围栏写(`writeAccountBlobScript`),它只比对建角锁令牌,不复核 `PlayerAllData:{id}` / `player:{id}:owner_epoch`。种子脚本只在 `NewPlayerSeed.Enabled`(默认关)时运行,而且只覆盖「6b' 探测 → 6d' 种子写」这一段。因此关种子时「6b' → 第 7 步」、开种子时「6d' → 第 7 步」都没有复核,与 G3 落码前相同。这是对 AGENTS §11.3「提交点原子复核、预检不代替提交校验」的记录在案的偏离,作为已知残余窗口接受。理由与代价:(1) 这段窗口(秒级)只是「探测时旧持有者在 Redis 里没有状态」这一类的子集:旧持有者在第 7 步之后任何时候首次进场,同样串档,建角期的任何检查都拦不住,在第 7 步补复核只能把窗口再缩短几秒;(2) 旧持有者若经现行建角流程创建,6c 名字登记已按 `FailedPrecondition`(该 id 名下已有另一个名字)拒绝,与时序无关;漏网的只剩没有名字登记行的旧持有者(名字表上线前的老号,或 `player_name` 与 `id_segment` 一并丢失),以及新账号恰好请求了与旧持有者完全相同的名字(同 id 同名幂等成功)。兜底仍是 I1。若要在提交点真正复核(例如在 `writeAccountBlobScript` 里对这两把键 `EXISTS`),超出 2026-10-15 第 4 项决定,另行拍板。

**行为**

| 步 | 情形 | 结果 |
|---|---|---|
| 6b'(新增) | 请求 ctx 已截止 | 不发探测,直接拒绝(`kLoginDataSerializeFailed`),不计探测指标(没有发生探测,也不是 Redis 故障;截止语义见 client-request-ordering.md §6.1) |
| 6b' | 父键或 `player:{id}:owner_epoch` 已在 | 拒绝,ERROR `[minted-id-probe] freshly minted player_id=… already has …`,`id_probe_total{parent_exists 或 owner_epoch_exists}` +1;名字、`player:zone`、种子一个都不写 |
| 6b' | 探测出错 | 拒绝(fail-closed),ERROR `[minted-id-probe] probe failed …`,`id_probe_total{error}` +1 |
| 6b'(2026-10-16 补) | 请求 ctx 在探测途中结束(go-redis 取连接 / 拨号时返回的就是请求 ctx 的错误) | 拒绝(`kLoginDataSerializeFailed`),INFO `[minted-id-probe] request ctx done while probing …`,不计探测指标,口径同第一行。Redis 本身的错误即使恰好在截止之后才返回,仍按上一行计 `error`。边界:前一次尝试读超时、go-redis 重发前的退避等待被截止打断时,返回的也是 ctx 错误,按本行处理;这种慢 Redis 只体现在 `stage_seconds{stage="id_probe"}` 的耗时里(INFO 日志带的只是 ctx 错误,前一次的读超时已被 go-redis 丢掉) |
| 6b' | 两把键都不在 | `id_probe_total{clear}` +1,继续 |
| 6d'(开种子) | 种子脚本报 `parent_exists` / `owner_epoch_exists` | 拒绝(`kLoginDataSerializeFailed`);名字按「确定已登记」释放一次;`player:zone` 留下一条(撤不掉,理由见 §10 P2 结论注),ERROR 日志带 `player_id` |
| 6d' | `written` / `idempotent` / `error` | 不变,尽力而为 |

探测的键名与种子脚本共用 `dataloader.newPlayerStateKeys`;两把都在时归 `owner_epoch_exists`,与种子脚本先判 owner_epoch 的顺序一致。tip 一律复用 6c / 6d 故障时的 `kLoginDataSerializeFailed`,不新增 tip 码。

**改动文件**

| 文件 | 内容 |
|---|---|
| `go/login/internal/logic/pkg/dataloader/new_player_seed.go` | 新增 `newPlayerStateKeys`(种子与探测共用)、`MintedIDProbe` 及其 `String()`、`ProbeMintedPlayerState`;`SeedNewPlayerAllData` 改用 `newPlayerStateKeys`(键名不变) |
| `go/login/internal/logic/clientplayerlogin/createplayer_seed.go` | 新增 `mintedIDProbeTimeout`(1s)与 `probeMintedID`;`seedNewPlayer` 改为返回 tip,对 `parent_exists` / `owner_epoch_exists` 返回拒绝,注释改为现行行为。2026-10-16 更正:`mintedIDProbeTimeout` 已删除,探测直接用请求 ctx;探测途中请求结束不计 `error`(见 §9 第 1 条 2026-10-16 更正注) |
| `go/login/internal/logic/clientplayerlogin/createplayerlogic.go` | 6b' 调用点;6d' 拿到 tip 后释放名字并返回;6c、6d' 注释补顺序与例外 |
| `go/login/internal/logic/clientplayerlogin/metrics.go` | `createStageIDProbe = "id_probe"`;`login_create_player_id_probe_total{result}`;`PrimeCreatePlayerMetrics` 预置它的 parent_exists / owner_epoch_exists / error;`seed_total` 的注释与 Help 补「建角被拒」 |
| `go/login/etc/login.yaml` | 只改注释:`Locker` 锁预算补一句探测的那次 Redis |

**用例**(`createplayer_seed_test.go`、`dataloader/new_player_seed_test.go`)

| 用例 | 断言 | 修复前为什么红 |
|---|---|---|
| `TestCreatePlayer_IDProbeRefusesMintedIDWithExistingState`(新,4 个子用例:父键 / owner_epoch × 种子开 / 关) | 拒绝;data_service 零调用(名字、`player:zone` 都没登记,也没有释放);`id_probe_total` 对应结果 +1;`seed_total` 五个结果都不变;父键原样、无 TTL;账号 blob 无角色、反查映射不写、建角锁已释放 | 没有探测:关种子时建角直接成功,开种子时种子只告警、建角成功,`requireTip` 处即红 |
| `TestCreatePlayer_IDProbeErrorRefusesCreate`(新) | 发号之后 Redis 整个不可用(`mintHookSegment` 里 `SetError`):拒绝;data_service 零调用;`id_probe_total{error}` +1;不记孤儿;账号 blob 无角色 | 没有探测:名字与 `player:zone` 照常登记,围栏写与回读都失败后保留登记、记孤儿,tip 是 `kLoginRedisSetFailed`,`requireTip` 处即红 |
| `TestCreatePlayer_SeedNeverOverwritesExistingParent`(改写) | 父键改在 Reserve 回调里(探测之后、种子之前)注入;先断言 `id_probe_total{clear}` +1(拒绝确实来自种子那步);拒绝,调用序列 `reserve,register,release`,释放的正是登记的 id / 名字;父键字节不变、无 TTL;`seed_total{parent_exists}` +1 | 前提断言处即红(没有探测指标);去掉前提断言则在 `requireTip` 处红(种子只告警,建角成功) |
| `TestCreatePlayer_SeedRefusedAfterOwnerEpochMinted`(改写) | owner_epoch 改在窗口里注入;同上的拒绝与补偿断言;父键不存在;`seed_total{owner_epoch_exists}` +1 | 同上 |
| `TestCreatePlayer_SeedErrorDoesNotFailCreate`(只改注入时机) | list 型父键改在窗口里注入(原写法会被探测当成 parent_exists 拒在 6c 之前,种子跑不到);期望不变 | 不是回归用例,修复前后都应绿 |
| `TestProbeMintedPlayerState_Verdicts`(新,dataloader) | 四种结论与 label 字面量;探测前后键集合不变、父键无 TTL | 新 API,修复前不存在 |
| `TestProbeMintedPlayerState_SeesTheSeedsParentKey`(新) | 种子写下之后探测报 parent_exists(两边判同一组键) | 同上 |
| `TestProbeMintedPlayerState_ErrorsAreNotClear`(新) | 零 id、nil 客户端、Redis 不可用都报 error 且带 err,不写键 | 同上 |
| `TestCreatePlayer_IDProbeUsesTheRequestDeadline`(新,2026-10-16 补) | 请求 ctx 带 1h 截止;pipeline 钩子记下交给 go-redis 的探测 ctx 截止,必须等于请求截止;建角成功,`id_probe_total{clear}` +1 | 修复前探测套 1s 子预算,截止约为发探测时刻 + 1s,在 probe ctx deadline 断言处即红 |
| `TestCreatePlayer_IDProbeRequestDoneMidProbeIsNotProbeError`(新,2026-10-16 补) | pipeline 钩子在交给 go-redis 之前取消请求 ctx(探测已越过前置检查):tip `kLoginDataSerializeFailed`;`id_probe_total{error}`、`{clear}` 都不变;data_service 零调用;不记孤儿;账号 blob 无角色;反查映射、种子不写;建角锁已释放 | 修复前一律计 `error`,在 `id_probe_total{error} delta` 断言处即红 |
| `TestCreatePlayer_IDProbeClearThenDeadlineSendsNoReserve`(新,2026-10-16 补) | pipeline 钩子在探测返回 clear 之后取消请求 ctx:`id_probe_total{clear}` +1;tip `kLoginDataSerializeFailed`;data_service 零调用;无释放、无延迟释放;账号 blob 不变;建角锁已释放 | 守护用例(预检早已存在):去掉 `tryReserveName` 开头的 ctx 预检,调用序列变成 `reserve,release`,即红 |

> **2026-10-16 补注**(上表两条「改写」用例):`TestCreatePlayer_SeedNeverOverwritesExistingParent` / `TestCreatePlayer_SeedRefusedAfterOwnerEpochMinted` 原先用 `newCreatePlayerHarnessWithNames`,包变量 `afterFunc` 仍是真定时器,`requireRefusedAtSeedAndCompensated` 只比对同步那一刻的调用序列,看不到排到 10s 之后的延迟释放,「释放恰好一次」实际只钉住了同步那一半。现改用 `newNameHarness`(发号源换成同 id 的 `countingSegment`,其余期望不变),共同断言补上 `n.scheduled` 为空。反证:把种子拒绝分支的 `releaseName(..., false)` 改成 `true`,两条用例即红(`newNameHarness` 同步执行延迟释放,调用序列多出一次 `release`,先在 `data_service call order` 处失败;`n.scheduled` 同样不再为空)。

两条被点名的旧用例,期望逐条怎么改写在各自的函数注释里。

**与他人未提交改动的关系**:`createplayer_deadline_test.go`(另一会话的截止语义用例)没有改。它的「发号之后」子用例要求截止之后不再有命令碰种子父键,6b' 在发探测之前先查请求 ctx,满足这一条;「种子写之后」子用例的 `cancelWhen` 匹配所有参数里带种子父键的命令,但只挂在 `ProcessHook` 上,而探测走 pipeline(`ProcessPipelineHook`),所以只会在种子那条 EVAL 之后取消。以后若把探测改成单条命令,那个谓词要收窄到种子脚本。

**补注(2026-10-16)**:上段说「发号之后」子用例由 6b' 的 ctx 预检满足,这只说对了一半。6b' 落地之后,该子用例停在 `probeMintedID` 开头的预检,走不到 6c `tryReserveName` 开头的 ctx 预检;全包也没有别的用例在 ctx 已结束时进入 `tryReserveName`(`TestReservePlayerName_BudgetBelowOneReserveRPC` 进入时 ctx 未过期,空名支被 `hasBudget` 先拦)。这条预检生产上仍然走得到:login 的 go-redis 未开 `ContextTimeoutEnabled`,请求 ctx 在探测往返期间截止时 pipeline 照样返回 clear。现由 `TestCreatePlayer_IDProbeClearThenDeadlineSendsNoReserve` 钉住;「发号之后」子用例钉住的是 6b' 的预检。2026-10-16 探测改用请求 ctx 之后,「种子写之后」子用例的交互不变(探测仍走 pipeline,`commandProbe` 的 `ProcessPipelineHook` 只观察、不取消)。`createplayer_deadline_test.go` 属另一会话,本次仍未改;其 `TestCreatePlayer_RequestDoneMidCreateCommitsNothing` 注释里「修复前:发号之后的那一例会照发 Reserve」对该子用例已不成立,留给那边处理。

**没有做的 / 后续**

- `tools/scripts/stress_summarize.ps1` 的 CreatePlayer stages 表还没有 `id_probe` 列(本批只许改 login 下的文件和两份文档);持锁时间对比要看这一列时另起一批加。
- 告警:`login_create_player_id_probe_total{result=~"parent_exists|owner_epoch_exists|error"}` 出现即告警。它不看种子开关,不属于 §7 第 3 步的「打开种子前置」,随 login 观测面一起上(K8s 上的前提同 §7 第 5 步)。
  - *2026-10-16 补*:建议拆成两条。`result=~"parent_exists|owner_epoch_exists"` 出现即告警(ID 安全事件);`result="error"` 按速率告警(Redis 可用性,与 id 复用分开处置)。请求在探测途中结束的已不计 `error`(§13.2 行为表 2026-10-16 补行)。
- `deploy/login-stack.linux/login.yaml` 没有 `Locker` 锁预算注释(§13.1 已记),这次也不用改。
- Java 版:§11 判定种子这条链不适用;id 复用时拒绝建角在 Java 版有没有对等项(例如主键冲突天然拒绝),由主会话核对后登记 `PARITY.md`。`docs/PROGRESS.md` 也由主会话统一记。

**交 Codex 的验证**(工作目录 `go/login`,串行,不需要外部服务):

```powershell
gofmt -l internal/logic/pkg/dataloader/new_player_seed.go internal/logic/pkg/dataloader/new_player_seed_test.go internal/logic/clientplayerlogin/createplayer_seed.go internal/logic/clientplayerlogin/createplayer_seed_test.go internal/logic/clientplayerlogin/metrics.go internal/logic/clientplayerlogin/createplayerlogic.go
go vet ./internal/logic/pkg/dataloader/... ./internal/logic/clientplayerlogin/...
go build ./...
go test ./internal/logic/pkg/dataloader/... -run "ProbeMintedPlayerState|Seed" -count=1 -v
go test ./internal/logic/clientplayerlogin/... -run "TestCreatePlayer_IDProbe|TestCreatePlayer_Seed|TestCreatePlayer_RequestDoneMidCreateCommitsNothing|TestCreatePlayer_ReleasesLock|TestCreatePlayer_HomeZoneRegistrationFailsClosed" -count=10 -v
go test ./internal/config/... -run "NewPlayerSeed|EtcYaml|LoginYaml" -count=1 -v
go test ./internal/logic/clientplayerlogin/... ./internal/logic/pkg/dataloader/... ./internal/config/... ./internal/svc/... -count=1
```

通过标准:

- gofmt 零输出。若列出 `createplayerlogic.go` 或 `metrics.go`,先用 `gofmt -d` 看差异是否落在本批改动的行上;落在别的会话的改动上不归本批,如实报告。
- vet / build 零报错。
- 新增与改写的用例全部 PASS(clientplayerlogin 那条各 10 遍),不得 SKIP;`TestCreatePlayer_RequestDoneMidCreateCommitsNothing` 的 5 个子用例 PASS;其余已有用例全绿。
- 「修复前应红」的核对(可选):在 HEAD 的临时 worktree 里只换上新版 `createplayer_seed_test.go` 跑 `-run "TestCreatePlayer_IDProbe|TestCreatePlayer_SeedNeverOverwritesExistingParent|TestCreatePlayer_SeedRefusedAfterOwnerEpochMinted"`:两个 `IDProbe` 用例应在 `create succeeded, want tip` / `tip = …` 处失败,两条改写用例应在 `id_probe_total{clear} delta = 0, want 1` 处失败。dataloader 的三个新用例引用新 API,在 HEAD 上编译不过,不做这项核对。
  - *2026-10-16 更正*:上一条写于改动未提交时,那时的「HEAD」指修复前的实现。6b' 探测与 P2 拒绝,连同新版 `createplayer_seed_test.go`、`dataloader/new_player_seed_test.go` 和本节文字,已随整区快照提交 `de4960bee` 进入历史;之后的 HEAD 都已含 `probeMintedID`,在这些提交上只换测试文件等于原样跑修复后的代码,四条用例会是绿的,证明不了「修复前应红」。这项核对改在 `de4960bee^`(即 `526780b4e`,引入探测之前的提交)的临时 worktree 里做:只把当前版 `go/login/internal/logic/clientplayerlogin/createplayer_seed_test.go` 覆盖进去,其余文件不动,`-run` 与期望的失败位置同上一条;上一条「dataloader 的三个新用例……在 HEAD 上编译不过」同样改读为「在 `de4960bee^` 上」。该基线若因同一快照带进的其他依赖跑不起来,就把这项核对记为「不可行、未做」如实报告,不要退回 HEAD 上跑,也不要把 HEAD 上的全绿当作修复前的证据。临时 worktree 用完即删,不提交。
  - *2026-10-16 补*:同日新增的 `TestCreatePlayer_IDProbeUsesTheRequestDeadline` / `TestCreatePlayer_IDProbeRequestDoneMidProbeIsNotProbeError` 修的是 1s 子预算与归类,修复前的基线是本轮改动提交之前的提交(写这条时为 HEAD `a105c3efd`,改动未提交)。在该基线的临时 worktree 里只换上当前版 `createplayer_seed_test.go` 跑这两条,应分别在 `probe ctx deadline` 与 `id_probe_total{error} delta` 处失败。`TestCreatePlayer_IDProbeClearThenDeadlineSendsNoReserve` 与两条改写用例补上的 `n.scheduled` 断言是守护,不做「修复前应红」,改做反证(各改一处、确认变红后还原,不提交):注释掉 `tryReserveName` 开头的 ctx 预检,前者在 `data_service calls` 处红;把 6d' 种子拒绝分支的 `releaseName(..., false)` 改成 `true`,后两条在 `data_service call order` 处红(多出一次同步执行的延迟 `release`)。
  - *2026-10-16 补(续)*:在 `de4960bee^` 上按上上条的 `-run "TestCreatePlayer_IDProbe|…"` 跑时,同日新增的三条 `IDProbe` 用例也会被选中;那个基线上没有探测,它们分别停在各自的前提断言(探测 pipeline 命中数为 0 / `id_probe_total{clear}` 增量为 0),同样算红,不在上上条列出的失败位置;也可以在 `-run` 里排除它们。
- *2026-10-16 补(告警口径)*:`id_probe_total{result="error"}` 只统计不会被 go-redis 重发、或在截止前已重发到上限的 Redis 错误。读超时、连接池超时在请求截止之后会被重发前的退避换成 ctx 错误,按「探测途中请求结束」处理、不计数,所以 Redis 卡顿不会体现在 `error` 上,要看 `login_create_player_stage_seconds{stage="id_probe"}` 或建角锁获取失败的 ERROR。上面「`result="error"` 按速率告警」只能覆盖 Redis 明确报错的情形。

失败时保留:失败用例的完整 `-v` 输出,以及 `gofmt -d` 的输出。前一步红了就停下归因,不连续重试(AGENTS §10.2)。

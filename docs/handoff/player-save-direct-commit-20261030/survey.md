# 存盘直接提交 TiDB:现状盘点(2026-10-30,三路只读)

> 只读 10-30 工作区(含他人未提交改动),行号会漂,以函数名为准。未编译、未运行。是[总体方案](../../design/player-data-tidb-scaling-plan.md)第 1 批规格的输入。
> 缩写:S=`cpp/libs/services/scene/player/system/`;PL=`S/player_lifecycle.cpp`;RA=`S/role_admission_system.cpp`;HB=`S/handoff_sql_barrier.h`;RC=`cpp/libs/engine/infra/storage/redis_client/redis_client.h`;DL=`go/login/internal/logic/pkg/dataloader/`;ES、OE=`go/scene_manager/internal/logic/{enterscenelogic,owner_epoch}.go`;AS=`go/data_service/internal/store/account_role_admission_store.go`;IC=`.../account_role_initial_commit.go`;PF=`.../account_role_sql_proof.go`;KD=`go/db/internal/kafka/`;KOC=`KD/key_ordered_consumer.go`;FENCE=`KD/player_row_fence.go`;PS=`go/db/internal/logic/pkg/proto_sql/db.go`;P2M=proto2mysql v0.2.0 `proto2mysql.go`;AUTH=`go/shared/authoritysql/`;K8S=`tools/scripts/k8s_deploy.ps1`。

## 一、scene 存盘侧

### 1.1 存盘主流程(`PL:4559 SavePlayerToRedisImpl`)

1. 取 Guid,`MarshalPlayerForSave` 拼整份 PlayerAllData 并盖 `persist_schema_gen`。缺组件回 kRejected。
2. `requestedAtMs!=0` 回 kHandoffAlreadyRequested。
3. 脏比较:`IsEqual`(MessageDifferencer)比整份;还要求快照的 committed E 等于当前 E 且 q≠0,且 `DbTaskDispatchSystem::NeedsResave` 为假;满足才回 kAlreadyPersisted。
4. 发 q,写 last_save_seq。
5. 序列化两条 DBTask,捕获提交钩子。
6. E≠0 走 `SaveWithCommitBarrier`;E=0 走无 guard 的 `Save`。

在途合并(`RC:852 EnqueueSave`):同 key 至多 1 在途、1 排队,新值覆盖排队值,被覆盖的钩子不执行;同一 element 重试时字节和 E/q 都不变;Redis 错误重试无上限,退避封顶 16s。

回包(`RC:1171 OnSaved`):空回包 / ERROR 重试;0、{4,owner} 走 `RejectSaveByOwner`;3 是 schema 拒写;1、2 执行钩子,有排队值发新值且不回调,否则进 `HandlePlayerAsyncSaved`(PL:2365),末尾 `snap.Replace`。只有最新值落地才更新快照。

- **E**:scene_manager Lua 铸造(OE:353,值 max(观察+1,当前毫秒)),经 `scene_handler.cpp:129` 传入,PL:2987 取 max。C++ 不递增;唯一例外是 B5 回滚采纳 Redis 值(PL:7044)。
- **q**:进程级发号,`max(last+1, nowMs<<10)`(`S/save_seq.h:59`)。只有确定要写时才发。加载时用 `max(last_save_seq, 两表 fence_save_seq)` 播种(PL:4488),超界种子挂 `PlayerSaveSeqFloorComp`。
- **周期入口**:`cpp/libs/services/scene/core/system/redis.cpp:294-338`,每秒一拍,按 `playerId % 周期` 分槽,缺省 300s,0 关闭。

### 1.2 被拒与废黜

- `HandlePlayerSaveRejected`(PL:4853):被拒的 E≠当前缓存 E,只是旧代写,用当前 E 重存;相等时按计数器落后或被改派销毁。
- `DestroyDeposedPlayer`(PL:5010):作废疏散票据,摘交接和冻结,摘场景、摘会话、销毁。不存盘、不改派、不发客户端消息。
- `DbTaskDispatchSystem`(`S/db_task_dispatch_ledger.h:27`):只记 Kafka 投递,不是 SQL。

### 1.3 触发点与等待条件

| 触发点 | 入口 | 等什么 |
|---|---|---|
| 下线 | PL:3289 → 重存≤5轮 → `FinishExitOrAwaitDbTaskAck` PL:3615 | Redis 落地 + Kafka acks=all,不等 SQL |
| 交接 | `StartTravelHandoff` PL:5172 先冻结再存;落地后 `BeginTravelHandoff` PL:5944 发 `kPublishMarkLua` | 要求 pending 等于两表 ACK;250ms 重试到 35s;`ResolveTravelOutcome` PL:6292 只信 Redis 证据。预算:存盘 30s、晚发 35s、应答 30s、冻结上限 70s |
| 交易冻结 | `FreezeRoleForTrade` PL:3225(RA:613 Seal 调用) | 排队、实体销毁、加载排空,再 `CheckSql` RA:338 才 `AckRoleTradeDrain` |
| 资产 | `SavePlayerForAssetCommit` | 不等;`IsAssetOpDurable` `S/asset_op_system.cpp:231` 只查快照账本;500ms 限频 |
| 结算 | `player_battle.cpp:617 RequestSettlementPersist` + PING | :560 探测 SQL 确认才销账放锁;0.5/1/2/4s 后退回兜底 |
| 收件箱 | `S/player_inbox_system.cpp:508 RequestPersist`(500ms 节流)+ PING | :621 探测确认后才 SendAck |
| 活动 | `S/player_activity.cpp:176`(1s 节流) | 不等 |
| 停机 | `cpp/nodes/scene/main.cpp:145/184/218` kNodeShutdown | `node.cpp:447` 预算 15s,到期强拆;K8s 宽限 60s |

ACK 判据:`kReadSqlAck`(RA:303)只被 `ReadSqlAck` 用;其调用方是 `CheckSql`(交易)和 `ConfirmSnapshotSqlCommit`(RA:536);后者的调用方是结算和收件箱;`SqlReady`(RA:308)只在这两处判定。

### 1.4 C++ 调 Go 的异步 gRPC

- 路径(`RegisterRoleAdmission`,RA:240):`ReadyNode` RA:91 取第一个 READY(`id_segment_bootstrap.cpp:84` 在 READY 里随机)→ `Sign` RA:99(HMAC-SHA256,方法 + caller + 时间戳 + 去 auth 后的确定性 PB 摘要,±300s,密钥≥32 字节,环境变量 `MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE`,Go 侧 `go/shared/serviceauth`)→ 生成的 `SendDataServiceRegisterRoleAdmission` → `Channel::StartUnaryCall`。
- 回调恰好一次,一律 `queueInLoop` 回到 scene 线程;非 OK 等于结果未知(`grpc_client_call.h:45`);通道只对 REFUSED_STREAM 透明重试一次。缺省 deadline 4s;单消息上限 4MiB,在途上限 65536(`muduo_grpc/channel.h:101-107`)。回调不得捕获实体句柄。
- 新增 scene → go/db 要动:`proto/db/db.proto`(只有占位 `db.Test`,NODE_DB 被生成器跳过:`grpc_process.go:51`、`route_table.go:53`、`service_register_info.go:508,527`);`eNodeType` 无 db(`proto/common/base/node.proto`);`node_util.cpp:11` 的 `nodeTypeNameMap`;发现 `service_discovery_prefixes`、scene 白名单 `cpp/nodes/scene/main.cpp:104`;go/db 只注册 go-zero `db.rpc`,没有 C++ 认的 NodeInfo 注册(参照 `go/shared/noderegistry`);预算门禁 `GrpcClient.CallDeadlineMs`、`Assert-GrpcClientDeadlineBudget`;go/db 没有接鉴权校验和密钥;生成配置 `proto_gen.no-unity.yaml` 里 data_service 域 rpc.type: grpc,db 域是 rpc。

### 1.5 持久数据装卸

- 生成器 `tools/proto_generator/protogen/internal/generator/cpp/player_data.go:61` 对标 `OptionIsPlayerDatabase` 的消息生成 `S/player_data_loader.h` 和 `*_loader.cpp`(后者只写到 tools/generated/temp)。源码树 `S/player_database_loader.cpp`、`S/player_database_1_loader.cpp` 是手写。
- PlayerAllData = 两张子表 + `bag_data`/`quest_data`/`mail_data`(未携带)+ `last_save_seq`,标了 `OptionPersistFrozen`。
- 拆行时生成器不能自动适配:`.h.tmpl` 假定 `<msg>_data`,写死 `player_database_data().bag_component()`;PlayerAllData 冻结,加字段生成期失败(`persistschema/apply.go:79`);没有按组的 Marshal;手写 Unmarshal 跨组有顺序依赖:技能、属性、宠物初始化依赖等级和属性组件(`player_database_loader.cpp:191-201`)。
- 账本不变量 I3:currency、bag、mission 领取标记、三本账本、限购必须同一次落盘(`mysql_database_table.proto:136-184`)。
- 手写引用点:PL:930-938、2299-2329、4433-4506、4694、4772、4833;`S/player_persist_marshal.h`;`S/player_persist_carry_comp.h`;RA:305、516-518、545-553;`S/save_seq_floor.h`;`S/asset_op_system.cpp:239,1185`;`player_battle.cpp:540`;`S/player_inbox_system.cpp:547`;`S/player_activity.cpp:153`;`cpp/libs/modules/snapshot/snapshot_system.cpp:48-85`;`S/stress_test_probe.*`;RC 的 Lua 和 `RedisSaveCommitIntent`;HB:350。

### 1.6 从 Redis 加载

`scene_handler.cpp:103` → `RequestEnter` RA:429 → SQL 登记 → `kRegisterFenceLua` → `Activate` RA:79 → `HandleEnterGameNode` PL:2738。本节点有实体就直接 EnterScene,否则登记 pending 并发 A2′,然后 `AsyncLoad` RA:82 → RC:512。同 key 有未落定存盘就挂起(`loads_awaiting_saves_`)。`OnLoaded` RC:1320:NIL 按 0.5/1/2/4/8/16s 重试 6 次(等 login 预加载)。失败(PL:2226)推 `kEnterSceneFailed`=3023;成功(PL:2285)校验后 `InitPlayerFromAllData` PL:4431。RPC 读的插入点:RA:82(唯一生产调用)、回调接线 `redis.cpp:23-24`、`HasPendingLoad`(RA:698、PL:3218)、`LoadFailureReason`。

### 1.7 坑

1. ACK 门禁要整体换判据(HB:350、`kReadSqlAck`),否则交接、结算放锁、收件箱确认、交易 Ack 都卡住。
2. 部分行破坏「新值可取代旧值」:只留最新排队值只对整份快照成立,脏行合并时丢排队项等于丢行。
3. 快照兼任两职:脏比较基准 + 业务 durable 证据(资产、结算、收件箱、活动、RA:536)。
4. 结果未知:gRPC 超时后旧请求可能晚到,go/db 必须幂等,deadline 要与服务端 Timeout 对齐。
5. 交接解冻前提会失效:最终存盘和释放同事务后,超时可能已释放,看门狗和 Abort 要重判(`S/travel_freeze_cap.h:34-36`)。
6. FIFO 依赖:PING 快路径、A2′ 先于 GET、重连补发顺序都假定同一 hiredis 连接。
7. 退出和停机:闸门等 Kafka 不等 SQL;15s 预算下全员提交 TiDB 未测。
8. 旁路与 Lua 判定:E=0 无 guard 存盘(PL:4845)、C3 世代校验和写方令牌(RC:210-234)、home_zone 拒存(PL:4713);废黜 E+1/E+2 规则(PL:2813-2825)依赖现有铸造语义。
9. 序号种子:`last_save_seq` 在顶层、不入 SQL;RA:517 断言 C++ 载荷 fence 为 0。
10. 成本:整份 Marshal 和 Differencer 都在 loop 线程(PL:4587、4661),缩短周期成倍增加;PlayerAllData 还复制 bag/quest;单请求限 4MiB;文本契约测试(`cpp/tests/currency_test/currency_test.cpp:1715,1771`)要同批改。

## 二、加载与写权

### 2.1 进场流程

1. `EnterGame`(`entergamelogic.go:81`):读 `login_session:*`、`login:sessfence:*`,抢 `player_locker:<pid>`,RPC 强读 `account_role`。RPC 立即返回,之后失败经 Kafka 推 3023。
2. 预加载 `DL/ensure_player_all_data_async.go:63` → `beginParentLoad`(`DL/parent_commit_barrier.go:176`)→ `route`:268 分快路径 / 屏障在途失败 / 核验。核验发两条 READ DBTask,go/db 各 SELECT 一次,无事务,LPUSH `task:result:<id>` 加 PUBLISH,单任务等 8s;`settle`:416 写父键或采纳 SQL;缺行、两行异版都失败。
3. `applyLoadedPlayerSession`:再查目录 → GetSession → `backfillPlayerIdentity` → 铸 `login_generation` → SetSession/Reconnect → Kafka BindSession → 再查目录 → EnterScene。
4. `EnterScene`(ES:137):强读角色,写 `location_revision_floor`,去重;读 location 和 owner_epoch;身份裁决;判 Gone/Dead/Replaced;选场景;换手门;`GetPlayerHomeZone`;预占人数;落点 Lua;Kafka RoutePlayerEvent,broker ACK 是提交点。
5. gate 汇齐 Bind 与 Route,muduo 发 `PlayerEnterGameNode`。
6. scene 见 1.6。

### 2.2 写权代次

- 铸造:`changesceneutil.go:196` 加 OE:157,同一 Lua 核 epoch、location 原文、handoff 原文、修订 tag 再写三键。持有者换了才铸;同节点不铸。回滚 OE:336 只 INCR。
- go/db:`guardOwnerEpoch` KOC:940;`savePlayerRow` 行锁比围栏。跨代都只拒更小。
- handoff(`E:ms`,EX300):写 `kPublishMarkLua` HB:340、`kReleaseMarkLua` HB:288(A1′、疏散,不看 ACK)、`kCredentialMarkLua` HB:309;SM 回滚时转写。读 OE:742、铸造/回滚/离场 Lua、`leavescenelogic.go:357`。
- 再入屏障 ES:1080:etcd 注册表消失、观察满屏障、`death_at` 过默认 20s,放行后按无持有者铸造。
- `location_revision`(`location_revision.go:27`):`<owner_revision>:<sha256(location)>`。

### 2.3 入场登记

- 表(AS:33):主键 (player_id, scene_instance_id);highest_generation、closed_through、identity_proto(含 owner_epoch、session、login_generation 等)、request_digest、required_owner_epoch/save_seq、close_digest。
- `Register`(AS:212):`account_role FOR UPDATE` 核修订与无交易 → 登记行 FOR UPDATE → 同代比摘要、高代 UPDATE、首次 INSERT。不比 owner_epoch,不跨实例,不碰玩家行。
- 调用:只有 scene(签名),早于会话、实体、载入;generation 进程内自增。实体销毁后 `CloseOrAck` RA:356 写 closed_through 与存盘下限。
- 建角(`account_directory_store.go:262`)同一事务:锁 account_directory → route_meta → `LockRoleRoutes` → account_role → IC:161 严格 INSERT 两表(fence 1/1、当前 schema_gen)→ `initial_player_commit=1`。`NewPlayerSeed.Enabled` 时 login 再写 Redis 种子。
- PF:44 `VerifyFrozen`:交易冻结证据,锁路由,`SELECT * FOR UPDATE` 两行求摘要,要求 E/Q/gen 相等且不低于下限;PF:240 是崩溃 holder 恢复证明。

### 2.4 交接路径

同节点不铸不载;同 zone 跨节点走交接,不经 login;疏散 PL:3933 退出存盘加 release 标记,不经 login;跨 zone ES:1241 签票 → 等待落点铸造 → Kafka Redirect,目标 login 预加载后再铸一次;判死接管靠客户端重登,经 login。失败 `ResolveTravelOutcome` PL:6292 裁决(未变解冻 / 回执指回本节点采纳不低于 E+2 / 回执校验不过踢线 / 其余视为放行销毁 / 判不清保持冻结上限 70s)。

### 2.5 非加载读者

| 读者 | 字段 | 频率 | 缺键 |
|---|---|---|---|
| `friend_profiles.go:17` | 名、级、职、性、外观 | 每次好友列表,64 一批 | 留空,名回源 |
| `online_directory.go:160` | 同上 | 每页最多 4 轮×64,60 页/分 | 跳过候选 |
| match `presence.go:82` | 同上 | 队伍视图、通知 | 零值 |
| `role_appearance_backfill.go:18` | 外观、职、性 | 每次 Login,外观空才读 | 留空 |
| `player_class_backfill.go:24` | 读写职、名、外观、性 | 无会话入场 | 链失败 |
| `persisted_player_database.go:31` | 资产、收件箱账本 | assetop 2s/批 100,inbox 60 分钟 | found=false |
| `asset_sql_commit.go:19` | SQL 提交证据账本 | 同上;mail 恒开,guild、trade yaml 关 | found=false,等待 |

### 2.6 映射提案 §15 现状

未落码。`player_route` 已是写真源(RouteStore.Mode=db_first),读仍经 Redis 副本;选库 `KD/placement_route.go:98` MGET `player:placement`、`player:zone`。落码会话 B0 完成,约 280 文件,等负责人决定(提案 §15.9)。

### 2.7 坑

1. 行内围栏只拒更小,新持有者首存前老写能通过,认领必须抬写权行。
2. PF:95、`Reconcile`、`kPublishMarkLua` 都假定两表同版;`hashPlayerSQLRow` 逐行读 fence 列。
3. 疏散、A1′、会话结束标记今天不等 SQL。
4. 同 zone 和疏散不经 login,scene 必须能自己读 TiDB。
5. 崩溃接管现靠 saveoutbox 补投与屏障等待,撤掉后预加载分支要重定义。
6. 同节点不铸造,顶号同 E,与「认领代次加一」冲突;`kRegisterFenceLua` 拒同 E 跨实例。
7. Redis 铸造是墙钟值,与 TiDB「加一」不同号,所有比 E 的脚本要定谁为准;`IsDeposedOnReentry` 差不小于 2 即销毁重载。
8. 身份补齐只写父键;新号初行只有 id、围栏、世代,首次注册在 scene 内存,改 SQL 也要过写权。
9. proto2mysql 事务无原生 SQL,`savePlayerRow` 的 Insert 在事务外;跨全局库与 zone 库的事务今天只有 data_service 在做;authoritysql 每事务核写资格。
10. 读者缺键语义不一;`last_save_seq` 不在 SQL,save_seq 是进程级墙钟号。

## 三、go/db 与玩家表

### 3.1 进程形态

- 一进程一 zone:ZoneId 必填,派生 `db_task_zone_Z`、`zone_Z_db`(`go/db/db.go:96-101`)。已有 zrpc :6000,只有 `db.Test`,业务走 Kafka。依赖 Kafka、TiDB、Redis、etcd。
- 后台:消费 worker、重试协程与收据租约、durable_enqueue、saveoutbox、WatchLegacy、WatchPersistBootstrap、KeepPlacementCapability、route_ready_gate、group_handoff、库探活、dbdrain。
- 保留:zrpc 与拦截器、authoritysql、StoreRegistry、dbguard、persist schema 判定、drainproof。退役:消费全套、saveoutbox、子表缓存、task:result 发布、`cmd/retry_requeue`。待裁定:barrierrestore、能力标记。
- data_service 范式:zrpc + 手写 server(`data_service.go:424`),拦截器 grpcstats → killswitch → admission → serverbase;C++ 发现靠 etcd `DataServiceNodeService.rpc/zone/Z/node_type/26/node_id/N`,端口通后注册;新服务用 `go/shared/noderegistry`。
- 鉴权:请求体 `ServiceCallAuth auth`,HMAC,±300s(`go/shared/serviceauth/auth.go`、`cpp/libs/engine/core/security/service_call_auth.h`);handler 内 Verify(`go/data_service/internal/server/account_role_admission.go:19`);每 caller 一把 env 密钥。
- 生成:`proto/data_service/` + proto-gen-run,C++ 产物 `cpp/generated/grpc_client/`;已有 `grpc_client/db/` 空桩;`dbserver.go` 是 goctl 产物。
- K8s:每 zone 一 Deployment,replicas 2、500m/512Mi、grace 45s、禁 HPA(`deploy/k8s/manifests/go-svc/db.yaml`),init 容器注册 writer;门禁 replicas≤PartitionCnt(K8S:2359)。

### 3.2 行围栏

- `savePlayerRow`(FENCE:101):事务外 ExistsByPK,无行事务外 INSERT(1062 落下);事务内 FOR UPDATE 后:epoch=0 且行有围栏 → legacy_after_seq;epoch 小 → stale_owner;同代 seq 小 → older;同代同 seq → equal(不比内容);否则 C1b 世代倒退 → schema_regression;整行 Save。代次更大不看 seq。列:`fence_owner_epoch`、`fence_save_seq`(uint64)、`persist_schema_gen`(uint32)。
- KOC 前后:排序锁 → 游标 → epoch 守卫 → 选库 → C0/C1 → BlobGuard(超限进重试/死信)→ SQL → 落点复核 → 令牌 Lua → advanceApplied。
- proto2mysql:同事务多行多表可以(`RunInTransaction` P2M:417,无 TxOptions),限同一 DSN 库;复合主键可以,`FindAllByPKIn` 拒绝;**一表一 message,同表多 message 报错,真 oneof 拒绝,所以「(player_id, 组)」只能一个通用 bytes/子消息列,或按职责分表**;部分列 `UpdateFieldsByPK`;`Save` 写全部非主键列,同内容走四条语句(P2M:3744);`SaveConditional` 只在未发布提交 e81a1e7。

### 3.3 表定义

- player_database 字段:1 player_id、2 transform、3 uint64_pb_component、4 skill_list、5 uint32_pb_component、6 derived_attributes_component、7 level_component、8 currency、9 stress_test_probe、10 merge_state、11 attribute_component、12 pet_component、13 bag_component、14 mission_component、15 profile_component、16 asset_op_ledger、17 settlement_ledger、18/19 fence、20 inbox_ledger、21 persist_schema_gen、22 mystery_merchant。
- 迁移:启动禁 DDL,只读闸缺列拒启(PS:415、547);`cmd/migrate up` 基线 version=1 全 CREATE,漂移只 ADD COLUMN;基线跑过后新表建不出(PS:406),`go/schemamigrate` 已修、go/db 未切。
- 加表要改:proto → `mysql_database_table_list.json`(go/db、merge_zone 读);PlayerAllData 字段(冻结)与 loader 模板;手写 loader;世代台账 `proto/persist_schema_gen.txt`;FENCE:32、KOC:541、`KD/player_cache_publish.go:135` 硬编码;store、barrierrestore、debug 工具硬编码;`tools/scripts/maintenance_drain.ps1:513` 按表名授权。
- 预算:256KiB/列、1MiB/行(base64 估算),仅 asset_op_ledger 列 1MiB,按表名写死(`go/db/internal/config/config.go:299`)。

### 3.4 直接读写玩家行的入口

`account_role_initial_commit.go:185`(INSERT 两表四列)、`:38`(SELECT * FOR UPDATE);`account_role_sql_proof.go:149`(两表整行摘要,:95 要求同版);`route_bootstrap.go:37`、`schema.go:1640`;`tools/merge_zone/player_rows.go:197,575,681`、`relocate_run.go:676`、`save_barrier_verify.go:307`、`backfill_home_zone`、`storage_audit`;`go/db/internal/barrierrestore/restore.go:1156,747,1077`;`debug_fetch/main.go:321`、`debug_import/main.go:611`、`debugutil/rows.go:123`;快照 `snapshot_system.cpp:48`(无恢复路径);回档 `rollback_logic.go:776`(只写 Redis 字段);`go/db/cmd/verifier/main.go:316`。

### 3.5 读路径

login `EnsurePlayerAllDataInRedisAsync` 按两子消息发 DBTask → go/db `handleDBReadOp`(KOC:529)→ TaskResult{success,data,error},data 为单行 PB。scene 建实体需要:两表同 player_id、全部业务列、各表 persist_schema_gen、顶层未知字段(Carry 组件)、save_seq 种子、fence、owner_epoch;新号判定 `registration_timestamp≤0`(PL:4506)。

### 3.6 连接与容量

池 60/20、寿命 5 分钟、读写 30s、锁等待 10s(PS:145、351);额外库 8/2。总账 K8S:2076。会话:autocommit、RC、严格、pessimistic、leader、strict(`AUTH/pool.go:16-23`)。写池每次取连接 ResetSession 重放 13 条(`AUTH/driver.go:370`);每事务门禁 5~6 条(route_meta、BEGIN、slot、meta、registry,`AUTH/writer.go:67-131`)+ COMMIT。现有单行存盘 2 个事务、业务 3 条,合计 41~43 条,两表约 85 条;沿用写池时 N 脏行约 22+N 条 [I]。未开 InterpolateParams。

### 3.7 坑

1. 写池门禁不改,每笔 19~20 条非业务语句;RC 下语句各取 TSO。
2. writer_slot 每 boot 64 行轮转,行锁持到提交(`AUTH/writer.go:78,89-101`),撞槽排队;与玩家行跨 Region,预计无 1PC [I]。
3. 写池业务语句发出后,除直接 1062 外都变 ErrOutcomeUnknown 并隔离连接(`AUTH/driver.go:73,215`);1205/1213 也落未知。
4. zrpc 默认 Timeout 2000ms(db.yaml 未配),取消即未知(`AUTH/driver.go:429`),短于 10s 锁等待。
5. 两表同版是硬约束:PF:95、`readPair`(`restore.go:1140`)、login 世代取 min。
6. 旁路写:debug_import、merge_zone、barrierrestore、初始行都不经写权行。
7. 选库复核靠 Redis(`KD/placement_route.go:255`),home≠zone 进死信;须与 §15 对齐。
8. 副作用有读者:资产证据、两表 ACK 被 data_service、scene 交接读。
9. 多副本受限:roster≤1024 且副本须等于计划(K8S:11835、`writer_authority/main.go:386`),每 Pod 永久占一个 writer。
10. `FindMultiByWhereClauses` 依赖多语句,写池拒绝(`AUTH/statement.go:57`);gRPC 单消息 4MiB。

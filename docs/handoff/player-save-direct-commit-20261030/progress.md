# 玩家存储水平扩展:落码进度与验证清单

新窗口接手时先读这份,再读 [总体方案](../../design/player-data-tidb-scaling-plan.md) 和 [存盘规格](../../design/player-save-direct-commit.md)。

**全部改动都没有编译。** 用户 10-30 把当时的工作区提交并推送为 `main` 的 `fc504300b`(不是 Claude 提交的)。之后的全部改动(下面「第二轮」)只在主工作区,未提交。验证要在工作区做。

## 2026-10-30 用户新指令:全部写完再交 Codex

用户原话:「直接写完所有 然后再让codex 做」。所以不再等 Codex 逐批验证,按下面的顺序写到底,最后整批验证。上下文满了开新窗口接着做,先读这一节。

总清单(做完一项就在这里打勾,写清改了什么、留了什么):

- [x] A7c 与评审修正
> B8 动手前的工作区快照:tree `398af9dab7f2484a906c3d20a73e236471e0eae5`,ref `refs/snapshots/before-b8`。被删的文件用 `git show refs/snapshots/before-b8:<路径>` 取回。

- [x] B8-1 go/db:只留存盘 gRPC,删 Kafka 消费、saveoutbox、retry 工具(`barrierrestore` 留着:`save_barrier_restore -authority-phase` 和 go/db 排空都在用)
- [x] B8-2 go/login:删预加载、父快照、DBTask 生产者与回包分发
- [x] B8-3 go/data_service:账本、证据只读 player_store,删 Redis 字段 KV、回档、路由器里的数据 Redis 与读不了新表的调试工具
- [x] B8-4 go/friend、go/match、go/scene_manager、go/shared 及其余服务:去掉开关和旧分支
- [x] B8-5 tools、robot(merge_zone 见 A6b;`robot/data_stress.go` 已删)
- [x] B8-6 C++ scene:删旧存盘与加载、DBTask 派发、严格存盘脚本、ACK 屏障;`ConfirmSnapshotSqlCommit` 改成同步的 `IsSnapshotCommitted`,交接标记不再等 ACK
- [x] B8-7 proto(db_task、旧 RPC、开关字段删除,`player_database*` 不再建表,生成器按 OptionTableName 过滤);部署脚本、k8s、告警(新增 `deploy/k8s/player-store-alerts.yaml`,删 `persist-schema-alerts.yaml`、`task-result-notify-alerts.yaml`、`db-task-*-alerts.yaml`)、ops 文档
- [x] A6b 别名合服:`placement.ResolveMergedZone`、`routing.MergedZoneCache`,`merge_zone` 重写(合服手册新增「别名合服工具现状」)。**工具仍要求源区先下线,不是不停服**
- [x] A7b Kafka 清理:删旧单节点路径、`DisableLegacyPerNodeTopic`、`match-rating-snapshot`、`game-events`,重算盘预算
- [x] A8 单行热点:聊天校验移出事务(TTL 缓存)、`match_rank_source` 拆 64 片、guild / trade / mail 哨兵行、登录读目录先无锁、`route_meta` 移出提交路径。结果见总体方案 §8.1
- [x] A9 后端档位:`Backend: tidb | mysql`,部署参数 `-DatabaseBackend`,工具用 `MMORPG_SQL_BACKEND`。详情见总体方案 §9.1~§9.4
- [x] etcd 运维(P0-C 阶段 2 的清单和合同测试已写;镜像没钉,部署前先过 M0、M3、探针 A、P0-D)
- [x] 崩溃持有者判死(规格 §9 第 11 条,`admission_recover` 重写)
- [x] 总验证清单(下面)
- [x] 独立评审:结果见下面「独立评审」

阶段 B(压测)由 Codex 跑;阶段 C 是上线后的事,不在这一轮。

## 2026-10-30~31 第二轮:服务器代码收尾

用户原话:「没做完的话你就按最标准的方式去做,数据强一致性,代码可读可以易懂,可水平扩展」「不停服合服现在接着做吗? 继续」。全部未编译。

- [x] **死锁可重试**:`authoritysql/driver.go` 把业务语句上的 1213、1205、9007 判成事务已中止(COMMIT 没发,回滚后结局确定),`retryOnDeadlock`、`WithTxRetry` 真正生效。COMMIT 错误仍是结果未知。
- [x] **战斗结算只剩协议 1**(总体方案 §5.4):battle 的协议 0、Kafka 结果提交、两个发件箱,scene 的 `battle:settlement:pending` 读写、销账、补读,`BattleSettlementEvent` 全部删除。放锁:`CompleteInboxSettlement` 在 SQL 确认后条件删;备战取消、FIGHTING 到期也删,其余随 TTL。`settlement_outbox.h` 改成 `battle_lock_rules.h`。
- [x] **匹配本地模式删除**:`GlobalMatch.Enabled`、Redis 队列删除,`go/match` 只剩 SQL 权威路径(-19629 行)。guild 的历练超时巡检改读 `match_battle_completion`。详情见 `docs/PROGRESS.md`。
- [x] **映射直读 TiDB**(映射提案 §15.9):`route_reconcile`、就绪标记、水位、Redis 副本全部删除。data_service 直读全局库,新增 `GetMergedZones`。go/db 认领前强读一次 `player_route`,不再用 Redis。写者框架只剩一种形状。
- [x] **不停服合服**(总体方案 §7.1、`docs/ops/merge-zone-runbook.md`):合服只写 `zone_merged_into`。`tools/merge_zone` 只留 merge、unmerge、audit。guild、trade 经 RPC,match 直读表;比较用 `Final()`,查询用 `IN Members()`;公会榜周期折叠。
- [x] **建角落点一种语义**:login 的 `PinOnCreate`、`LegacyNonGlobal` 删除,建角恒写落点;`RegisterPlayerZone` 拒绝 `storage_id=0`;data_service、go/db 都不回落 home_zone。data_service 的 `LegacyNonGlobal` 也删了。
- [x] **C++ 清理**:`SavePlayerToRedis*`、`SavePlayerDirect` 改名 `SavePlayer*`、`SubmitPlayerSave`;`fence_*`、`save_seq_floor.h`、`BattleSettlementLedgerComp`、`PlayerMergeStateComp`、`login.proto` 两个合服提示字段、Kafka 逐条回执(`sendTracked`、`DeliveryTracker`)、scene 上的 `team:player` 回复解析、`TeamInfo` 删除。
- [x] **开关和命名**:`RequireSQLCommit` 恒开;`ImportRole` 和 `account_role_import_receipt` 删除;`player:{id}:save_barrier` 改名 `player:{id}:handoff_fence`;`shared/savebarrier` 改名 `shared/handofffence`;`save_barrier_restore` 改名 `go/db/cmd/maintenance_phase`,环境变量 `MAINTENANCE_PHASE_DSN`。死包 `shared/businessdigest` 删除。
- [x] **线上拉数工具**:按 `player_store` 重写 `debug_fetch`、`debug_import`(只把存档盖到本地已有角色上,不动路由和账号),手册 `docs/ops/online-debug-data-fetch.md`。
- [x] **本机写者授权**:`tools/scripts/lib/sql_boot_catalog_local.ps1`,`start_game.ps1` 每次启动自动生成,只收回环库。之前本地全套根本起不来。用法见 `tools/scripts/README.md`。
- [x] **robot**:`robot/network_probe.go`,战斗、组队、历练场景进匹配前先走地域探测,观战带 `request_id`。
- [x] **部署与告警**:`k8s_deploy.ps1` 删路由开关和 Redis 总账;全局服务排到首个 zone 的 data-service 之后(全新集群分开跑是 `infra-up` → `zone-up` → 再 `infra-up`);`data-service-route-alerts.yaml` 只留读失败、注册失败和 4 条映射刷新告警;压测脚本同步。db、match 的镜像入口改成整包构建。
- [x] **Redis 协调键集群化**(`docs/design/redis-player-store-cluster.md` §13,第 3、5、6、7、9、10、11 批;第 4、8 批随存盘直提交、结算走收件箱作废):玩家域键全带 `{id}`,进 Redis Cluster。C++ `RedisClusterClient`、`tlsPlayerRedis`:同 slot 停等,「未执行」错误按预算重试,结果未知的命令不重放。Go `shared/playerredis`:`UniversalClient`、守卫、防重放、按 slot 拆 MGET、逐主节点 SCAN。scene_manager 人数改成员集合,领导者后台对账。player_locator 按 slot 分桶、256 段段租约。登录域独立 `LoginRedis`:令牌 `<tag>.<secret>`、账号锁、连接栅栏,登录提交拆两步。部署:`redis-player-cluster.yaml`、`-PlayerRedisMasters`、连接总账、告警、手册、`compose.redis-cluster.yml`、`redis_cluster_capacity.ps1`。
- [x] **观战与调试栈**:`spectate:battle:{id}` 删除,观战记录从 SQL 成局组出;docker 调试栈接上写者授权和 player_inbox。
- [x] **`LoginRedis` 一种写法**:读登录态的六个服务都写 `Addrs`、`PoolSizePerNode`,`Host`、`PoolSize` 删除。

## 已写完的

### 发号器(总体方案 A7,不依赖存盘路径)

- `go/data_service/internal/idissuer`:内存发段、两份预留记录、时间下限。
- `go/data_service`:接线到 svc、config、logic、metrics;新增 `-id-issuer-init`;删除 `id_segment` 表、`store/id_segment_store.go`、号段地板。
- `go/shared/idsegment`:发号打散(`mix.go`);删除 snowflake 回退;范围校验从「必须递增」改成「不能重叠」。
- `cpp/libs/modules/id_segment`:`guid_mix.h`;`TryNext` 打散;上限 2^63;校验改成查重叠。测试里断言计数器值的地方都设了 `mixIds=false`。
- 依赖单调的修正:邮件收件箱、卖家货架、默认浏览、玩家合同与挂单列表、帮会资产唤醒顺序。
- Lua `tonumber` 约 96 处逐处看过,没有处理 ID 的,不用改。
- 部署:`deploy/k8s/manifests/infra/etcd-id-secondary.yaml`、`k8s_deploy.ps1` 接线、`tools/scripts/tests/id_issuer_deploy_contract.tests.ps1`;手册 `docs/ops/id-issuer-runbook.md`。
- `AGENTS.md` §7 第 1 条已修订。

未做,记在总体方案 §13 的 A7c:自增主键改由应用发号;battle、challenge、team 的主键迁到号段;etcd 运维补齐。

### A7c:自增主键和 battle_id 迁到发号器(不在 `claude/player-store-direct-commit` 那个提交里,只在工作区)

- `player_snapshot.id`、`rollback_audit_log.id`:`go/data_service/internal/store/snapshot_store.go` 的 `UseRowIDs`、`nextRowID`,`internal/svc/servicecontext.go` 的 `useSnapshotRowIDs`。没接来源或发不出号时插入失败。
- `battle_id`、`challenge_id`、`team_id`:`go/match/internal/svc/battle_id.go`,biz_tag = battle。match 不再申领 snowflake 槽位,删了失租强退、`ClusterId`、`SnowflakeCacheDir`、K8s 的 `snowflake-cache` 卷。配置必须写 `IdSegment.Enabled: true`,没写起服即拒绝。
- Java 网关 `zone_whitelist`、`announcement`:`entity/RowIds.java`,按时间递增,**不能换成打散的号**,理由在 `ZoneWhitelistRepository` 的类注释。
- biz_tag 清单加了 `battle`、`snapshot_row`、`rollback_audit`(代码、服务 yaml、K8s ConfigMap 三处)。
- 没做:etcd 钉镜像、配额、碎片整理,见总体方案 §13 的 A7c 行。

### 存盘新路径

- 规格 v2:`docs/design/player-save-direct-commit.md`,按 `spec-review-1.md` 修正。
- B1:`proto/player_store/player_store.proto`、`mysql_database_table.proto` 的 `message player_store`、`go/db/internal/playerstore`(判定纯函数、`Save`、`LoadAndClaim`、`VerifySchema`,单测与真库用例)、`go/db/internal/dbtest` 支持事务。
- B2:proto 加 `service PlayerStore`、`PlayerStoreNodeService = 31`、两份生成配置加 `player_store` 域;`go/db/internal/playerstore/server.go`(鉴权、选库、指标)、`go/db/player_store_wiring.go`(装配、VerifySchema、C++ 约定注册)、`kafka.NewPlayerStorageResolver`、`db.go` 接线、`ServerConfig.PlayerStore.Path`(缺省 redis)、`db_player_store_*` 指标、`stress_summarize.ps1` 新增一段。K8s 的 `MMORPG_PLAYER_STORE_SECRET_SCENE` 注入(db 和 scene)和 `Path=direct` **没有接**,放到切换批。
- B7(读方与工具,规格 §5.6):`BatchGetPlayerDisplay`;friend、match 改读它;`HasHistoricalPlayers`;GM 回档拒绝;调试工具、`save_barrier_restore`、`merge_zone` 拒绝;`verifier` 读 kProfile 行。
- B6(判据替换,规格 §5.5):`ConfirmSnapshotSqlCommit`、`CheckSql` 在新路径上不读 Redis ACK;`PlayerStoreClient::RunWhenSettled` 取代 PING 快路径(结算、收件箱);`data_service` 的 `player_store_access.go`(账本读、判死释放)、`account_role_store_proof.go`(交易冻结证据);`assetop.Loop.judgeWithPlayerStore`;`LeaveScene` 判死后释放写权;`scene_manager` 也读开关。
- B5(加载接入,规格 §5.4):`PlayerStoreClient` 的 `RequestClaim` 与 `HasPendingClaim`;`player_lifecycle` 的 `RequestPlayerLoad`、`HasPendingPlayerLoad`、`HandlePlayerStoreClaimed`;`role_admission_system` 改调它们;拼接时丢退役字段;`go/shared/playerstorepath`;login 跳过预加载和身份补齐;`data_service` 建角写 `player_store` 初始行(`account_role_initial_store.go`);七份 yaml 同一开关,部署与启动都核对。
- B4(主流程接入,规格 §5.3):`PlayerStorePath` 开关(`config.proto`、`engine/config/player_store_path.h`、`config.cpp`、`base_deploy_config.yaml`、`k8s_deploy.ps1` 镜像并核对与 go/db 相同、`start_game.ps1` 启动前核对);`PlayerStoreClient` 补 `SubmitOptions`、被取代的快照、代次落后、`ReleaseDetached`;`player_lifecycle` 的 `SubmitPlayerSave`、`OnPlayerStoreSaveDone`、`HandlePlayerSaveRefused`、`DisposeRefusedSave`、`RequestKeyProgressSave`、`LogUnsettledPlayerStoreSaves`;`handoff_sql_barrier.h` 的 `kPublishMarkDirectLua`;重连与退出被取代时清 `released`;会话结束凭证先释放;启动校验;定时存盘 60 秒;任务、等级接关键进度;go/db 接受不带落点的空行释放。
- B3:`OptionPersistGroup` 与 `player_database` 全部字段的分组;`cpp/libs/services/scene/player/system/player_store_projection.*`(投影、拼接、启动校验)、`player_store_client.*`(单在途、重试、未决行、全局完成);节点发现白名单、deadline 配置与门禁;测试 `cpp/tests/bag_test/player_store_client_test.cpp`。**没有接主流程**,行为不变。

### 独立评审

B4、B5 和 B6、B7 各评审过一轮。第一轮没有找到丢档、回档、串档、一物两得的序列;第二轮有一条回档序列(判死释放围掉新持有者),已修。修的项和接受不修的残余见规格 §5.5~§5.7、§9 第 9、10 条。

阶段 A 写完之后(10-30)又做了一轮只读评审,四路:C++ 存盘与生命周期、Go 存盘与权威数据层、榜单拆片与聊天和哨兵行、别名合服。全部读代码,没有编译。修掉的:

- **崩溃持有者判死挡不住在途认领**(高):只抬一次代次,而认领带的是登记时的墙钟代次,照样成功,冻结证据作废。改成抬到所有待判持有者登记的代次之上(规格 §9 第 11 条)。
- **已释放的活现任被判死**:释放不是终态,`released=1` 的现任也算现任,等真实 ACK。迟到的真实 Close 按成功重放。
- **交易排空证据**:封存时在途的提交、局中结算的存盘都不再算(规格 §5.7)。
- **`data_service.proto` 被我的补丁写坏**(39 个消息重名),已还原并核对没有重名。
- **合服工具**:连续合服时门禁漏掉之前别名进来的玩家;换清单重跑抢走映射的 `run_id`,撤销不再是逆操作;撤销半途失败后用同一清单重跑会空转打 Done;`NOSCRIPT` 把工具打成 HOLD。都已修。
- **投榜**:积压时一轮超预算被当成结局未知、永久挡住排空,高号片饿死;多副本投得更远时被当成故障。改成预算用完收手、积压多的片先投、榜确认得更远就推游标。
- **同分决胜**:`achieved_ms` 改由 match 定(`match_rating.score_changed_ms`),不看批次到达顺序。
- **聊天**:并发重发可能被回成限流,限流前再查一次原件。
- **TiDB**:固定 `tidb_constraint_check_in_place_pessimistic=ON`,1062 不会拖到 COMMIT。
- **编译和门禁**:`AppendToString` 丢弃 `[[nodiscard]]`(Debug 是 /WX);`SkippedSettlementItem::side` 裸指针成员过不了 `NoRawPointerMemberCheck`,这一处不是这轮写的,顺手改成 `std::string`;`scene.vcxproj` 的残留。
- **测试本身的错**:`rec.Contains("UPDATE")` 会撞上 `FOR UPDATE`;`NO_ADMISSION` 断言过时;证据夹具的白名单缺 proof 库。
- **死锁重试永远不触发**:写者闸门把业务语句上的 1213、1205、9007 一律变成不可重试的结果未知,`retryOnDeadlock`、`WithTxRetry` 认不出。已修:COMMIT 还没发,回滚之后结局确定,判为确定回滚并保留原错误(`go/shared/authoritysql/driver.go`)。

第二轮写完之后又做了两轮只读评审(主体一轮,Redis 分 C++、Go 两路)。全部读代码,没有编译。修掉的:

- **公会审批**:判成不同区时把合法的入帮申请删了(`ErrGuildZoneMismatch`)。
- **中止的事务**:死锁之后调用方吞错继续用同一个 `sql.Tx`,后面的语句会脱离事务执行。写者闸门加 `abortedTx`,一律回原错误。
- **排空下限**:代次取错了来源,改取认领响应(`HandlePlayerStoreClaimed`)。
- **home 回落残留**:`account_role_sql_proof.go`、`playerbundle` 不再回落 home_zone。
- **集群下重放**:`ResultUnknownError` 带 `Unwrap`,go-redis 9.17 的 `shouldRetry` 能解开,EOF 之后仍会重发已执行的脚本。改成 `Cause()`;上下文错误不包。
- **维护排空卡死**:节点退场时在途的 AUTH、探测 PING、CLUSTER SLOTS 拿到空应答,永久留下 UNKNOWN。新增 `Hiredis::hasNoSideEffect`。
- **拓扑变化露给业务**:新主握手期间,空闲 slot 的命令回 `REDIS_ERR`。改成排队,在预算内退避。
- **强制销毁**:残留成员直接 SREM 节点集合,会把同节点别的场景的玩家摘掉。改成逐个按 location 判。
- **对账撞在途预占**:一看到 location 不指向这里就摘,可能让空闲回收销毁玩家正要落点的实例。加 2 分钟宽限。
- **配置**:`player_redis.seeds` 写成标量时静默回落 zone redis,改成拒启;scene_manager、player_locator 启动时校验 `PlayerRedis` 段。
- **编译错误**:`go/trade/internal/config/config.go` 缺 import;`playercontract_test.go`、`drain/session_test.go` 的字段和参数。

## 给 Codex 的总验证清单(2026-10-31,替换旧清单)

工作目录除注明外都是仓库根目录。按顺序跑,一步失败先报告,不要改断言、不要跳测试、不要连续自动重试。C++ MSBuild 必须串行 `/m:1`。失败时保留:命令输出、`go test -v` 的失败用例、C++ 编译错误前 50 行。跳过的用例按没跑报告。

**工作区**:基线是 `main` 的 `fc504300b`,第二轮的改动都在主工作区,未提交(约 880 个文件,净减约 6 万行)。不要整理、不要回退。更早的快照:`refs/snapshots/before-b8`、`refs/snapshots/phase-a-written`(都是 tree,`git show <ref>:<路径>` 取回)。

**前置清理**:项目没上线,所有库清掉重建,不写迁移。主键变了的表:`match_rank_source`、`match_rank_outbox`、`rank_input_receipt`;`rank_score` 多一列。不再建的表:`player_database`、`player_database_1`、`id_segment`、`zone_merge_fence`、`save_barrier_maintenance_receipt`、`account_role_admission_recovery`、`route_reconcile`、`account_role_import_receipt`。按 AGENTS §6.2 第 6 条删掉 redis、mysql、etcd 数据。

### 1. 协议生成

生成器改过(`db_model.go` 跳过没有 `E_OptionTableName` 的消息),要先编译生成器:

```bash
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build
```

```bash
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml
```

通过标准:
- `generated/data/mysql_database_table_list.json` 和 `go/db/data/` 的同名文件有 `player_store`,没有 `player_database`。
- `go/proto/player_store/`、`cpp/generated/grpc_client/player_store/` 存在。
- 旧生成物要没有:`db_task.pb.*`、`battle_settlement_ledger_comp.pb.*`(`cpp/generated/proto/`、`go/proto/`、`robot/vendor/proto/` 三处),以及 `generated/proto/**` 下的 `db_task.proto`、`battle_settlement_ledger_comp.proto` 副本。**生成器对没有 proto 的产物只跳过、不删,工程清单也只增不删**(`tools/proto_generator/protogen/internal/prototools/cpp_build_projects.go`),所以这里是手工删的一个例外:删这些文件,并从 `cpp/generated/proto/CMakeLists.txt`、`proto.vcxproj`、`proto.vcxproj.filters` 删掉对应条目。只删文件不改清单,proto 工程找不到源文件。
- `proto/persist_schema_gen.txt` 多出 `retired=player_database#10`、`#17`、`#18`、`#19`、`retired=player_database_1#3`、`#4`。`BattleSettlementLedgerComp`、`PlayerMergeStateComp` 不再可达,生成器只出提示,台账行留着。
- `cpp/nodes/scene/handler/event/scene_kafka_command_router.cpp` 是生成物,这一轮手改过(去掉 `BattleSettlementEvent`),生成后 `git diff` 应该没有别的变化。
- `go/proto` 里没有 `AccountRoleImportReceipt`、`TeamInfo`、`RouteRecoveryAudit`;`login.proto` 的 3、4 号字段是 `reserved`。**客户端要重新生成协议**(只是删了两个一直没人写的字段,线上格式不变)。
- `grep -r RemapHomeZoneForMerge go/proto cpp/generated` 没有命中。`proto/event_id.txt`、`proto/message_id.txt` 由生成器管,旧行留着不算错。

### 2. Go 依赖

每个模块跑一次 `go mod tidy`:`go/shared`、`go/schemamigrate`、`go/data_service`、`go/db`、`go/login`、`go/guild`、`go/trade`、`go/mail`、`go/rank`、`go/friend`、`go/match`、`go/chat`、`go/player_inbox`、`go/player_locator`、`go/scene_manager`、`go/client_rpc_router`、`tools/merge_zone`、`tools/data_consistency_check`、`robot`。然后 `cd robot && go mod vendor`。

### 3. Go 编译、静态检查、单测

上面每个模块都跑:

```bash
go build ./... && go vet ./... && go test ./...
```

再跑:

```bash
pwsh -NoProfile -File go/test.ps1
```

```bash
cd go/data_service && go test -race ./internal/idissuer/...
```

```bash
cd go/chat && go test -race ./internal/authority -run TestActorVerify
```

通过标准:全绿。这一步预计会有编译错误,全部改动没有编译过。修编译错误不算改断言;如果断言和新行为冲突,保留输出报告。go/db 不再依赖 go-redis,`go mod tidy` 之后它的 `go.mod` 应该变短。

重点:`go/shared/playerredis` 的 `TestWrapResultUnknown`、`TestLoginReaderConfig`;`go/scene_manager/internal/logic` 的 `TestSceneMemberReconcile*`、`TestForceDestroyRemovesResidualMembersByLocation`、`scene_keys_contract_test.go`;`go/login` 的 `login_commit_two_step_test.go`、`internal/logic/pkg/token`;`go/player_locator` 的 `TestAlertRulesReferenceRegisteredMetrics`;`go/db/internal/maintenancephase` 的 `drain_test.go`。

### 4. 真库

自起一次性 MySQL 和 TiDB,别借 `codex-*` 容器。`go test` 报 Access denied 时改用 `go test -c` 再运行。真 Redis 用例 7.2 和 8.x 各跑一轮,结果记版本。

| 模块 | 环境变量 | 命令 |
|---|---|---|
| go/db | `GODB_MYSQL_ITEST_DSN`、`GODB_IT_REQUIRE=1`(MySQL、TiDB 各一遍) | `go test -tags=integration -race ./internal/playerstore/...` |
| go/data_service | `ACCOUNT_DIRECTORY_TEST_DSN`、`ACCOUNT_DIRECTORY_REQUIRE_TIDB=1` | `go test ./internal/store/... -run 'TestTiDB'` |
| go/data_service | 见 `internal/store/storetest/mysql.go` 包注释 | `go test -tags=integration ./internal/store/... ./internal/logic/... ./internal/kafka/...` |
| go/chat | `CHAT_IT_TIDB` | `go test ./internal/authority -run TestChatTiDB` |
| go/match | `MATCH_IT_TIDB`、`MATCH_IT_REQUIRE_TIDB=1` | `go test ./internal/authority ./internal/team/...`(只有 TiDB 档,含 `sql_store_integration_test.go` 的同区用例) |
| go/match | `RANK_MAIL_CHAIN_IT_TIDB` | `go test ./internal/authority -run TestTiDBRatingRankMailChainRecoversAfterCommittedReplyLoss` |
| go/rank、go/mail | `MAIL_RANK_TEST_TIDB` | `go test ./... -run TestTiDB` |
| go/guild | `GUILD_TEST_MYSQL_DSN` | `go test ./internal/data -run 'TestGuildLockOrder\|TestGuildLockingStatement' -count=3`,以及 `./internal/logic` 的 economy flow |
| go/guild | `GUILD_ROLE_TEST_DSN`、`GUILD_ROLE_REQUIRE_TIDB=1` | `go test ./internal/data -run TestTiDBTrialResultRecordsReadMatchTerminal` |
| go/trade | `TRADE_TEST_MYSQL_DSN` | `go test -tags integration ./internal/data -run 'TestLegacyEnsureSeqRow\|TestEnsureSeqRows\|TestFirstTimeCreation\|TestSeqRowProbe'` |
| go/player_inbox | `PLAYER_INBOX_TEST_MYSQL_DSN` | `go test -tags integration ./internal/inbox/...` |

重点用例:`TestTiDBRoleTradeDeadHolderIsFencedWithoutAck`、`TestTiDBRoleTradeCurrentHolderBlocksUntilReleased`、`TestTiDBRoleTradeReleasedCurrentHolderStillWaitsForAck`(崩溃持有者)、`TestRatingTiDBRoundBudgetExhaustedStopsWithoutError`、`TestRatingTiDBCursorAheadFromOtherReplicaIsNotAFailure`、`TestRatingTiDBAchievedMsFollowsScoreChangeNotGames`、`TestTiDBVersionedAchievedMsComesFromProducer`(投榜)、`TestTiDBReleaseDeadPlayerStoreHolder`、`TestTiDBCreateRoleOnPlayerStoreWritesOnlyTheNewRows`、`TestTiDBReadPlayerDisplays`、`TestSnapshotStore_RowIDCollisionIsNotSwallowed`。

### 5. merge_zone

工具已重写,只有 merge、unmerge、audit。按 `docs/ops/merge-zone-runbook.md` §11 跑:单测、真库(`MERGE_ZONE_IT_REQUIRE=1`、`MERGE_ZONE_IT_DSN`、`MMORPG_SQL_BACKEND`,TiDB 和 MySQL 各一遍,`go test -tags merge_integration -count=1 -v ./...`),以及两区在线合服、并发写入、撤销等场景表。

### 6. C++

```bash
msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64
```

期望产物:scene、gate、battle 节点和全部测试 exe。跑 `cross_zone_test`、`currency_test`、`bag_test`、`kafka_command_test`、`rpc_controller_test`、`turn_battle_engine_test`、`player_landing_test`、`aoi_test`、battle 节点的 `battle_observer_contract_test`。`currency_test`、`bag_test` 里的真 Redis 用例要配专用 `MMORPG_TEST_REDIS_ADDR`,没配会跳过,要配上跑一次(7.2 和 8.x 各一轮)。

重点:`PlayerStoreDirectEcs.*`(含这轮新增的 `CommitSubmittedBeforeTradeFreezeIsNotDrainEvidence`、`SaveDuringBattleDoesNotReplaceTradeFreeze`)、`PlayerStoreClientTest.*`、`PlayerStoreProjectionTest.*`、`RoleAdmissionTest.*`、`GuidSegmentClientTest.*`、`KafkaPartitionerContract.*`。`git show refs/snapshots/before-b8:<路径>` 能取回被删的旧测试,对照用,不要恢复。

Redis 这批在 `rpc_controller_test`:`RedisClusterClientTest.*`(含 `IdleSlotOnHandshakingMasterQueuesInsteadOfRejecting`)、`HiredisCommandLifecycle.*`(含 `NoSideEffectCommandNullReplyKeepsFrontierKnown`)、`redis_cluster_logic_test.cpp`。`Hiredis` 改了 `muduo_windows` 和 `tools/archived/muduo_linux_overlay` 两份实现,Linux 的 C++ 镜像(`deploy/k8s/Dockerfile.cpp`)也要编一次。

### 7. 部署合同测试

`tools/scripts/tests/` 下的 `*.tests.ps1` 全部跑,一个失败不中断其余的:

```bash
pwsh -NoProfile -Command "$failed=@(); Get-ChildItem tools/scripts/tests/*.tests.ps1 | ForEach-Object { pwsh -NoProfile -File $_.FullName; if ($LASTEXITCODE -ne 0) { $failed += $_.Name } }; $failed; exit $failed.Count"
```

再跑 `pwsh -File tools/scripts/test_stress_summarize.ps1`。

重点:`k8s_deploy_contract`(全局服务顺序、`Find-ReadyDataServiceNamespace`、startupProbe 预算)、`k8s_client_entry_contract`、`k8s_redis_split_contract`、`k8s_migrate_gate`、`k8s_route_tool`、`k8s_zone_rollback_discard`、`lobby_authority_render`、`global_authority_deploy`、`chat_authority_deploy`、`go_services_local_mysql`、`go_services_ports`、`go_services_stop`、`maintenance_drain`、`sql_boot_catalog_local`、`id_issuer_deploy_contract`、`etcd_maintenance_contract`、`dev_tools_merge_zone_contract`、`redis_player_cluster_contract`、`stress_snap_redis`、`docker_debug_sql_boot`、`k8s_zone_rollback_discard`。再跑 `python tools/scripts/gen_docs_index.py --check`。`go_services_ports` 在基线就缺写者函数桩,chat、trade 启动类用例失败先和基线对比。

### 8. etcd

按 `docs/ops/etcd-maintenance-runbook.md`:M0 取镜像摘要,`etcd.yaml`、`etcd-maintenance.yaml`、compose 三处同批钉(`bitnamilegacy/etcd:latest` 现在没钉);M3;探针 A;P0-D 实跑。没有环境就如实报告没跑。首次部署之前必须跑完。

### 9. Java 网关(A7c)

`GATEWAY_TEST_MYSQL_URL` 指向库名以 `_it` 或 `_test` 结尾的专用 MySQL,设 `GATEWAY_REQUIRE_MYSQL_TESTS=1`,跳过即失败:

```bash
cd java/gateway_node && mvn test -Dtest='RowIdsTest,GatewaySchemaContractTest,ZoneWhitelistRepositoryWiringTest,ZoneWhitelistUpsertLockOrderMySqlTest,AdminWhitelistControllerTest'
```

`ZoneWhitelistUpsertLockOrderMySqlTest#upsertBehindPendingDeleteOnlyQueues` 失败时不要改断言,按 `ZoneWhitelistRepository` 的类注释判断,主键方案要重新定。

### 10. 全链路冒烟

清库后本地起全套:`start_game.ps1` 每次启动自动跑 `sql-boot-catalog-local`,在 `run/secrets/sql-boot-local/` 生成写者授权和账号,只收回环库(用法和重来的办法见 `tools/scripts/README.md`)。本机卷里有旧 `mmorpg_global` 会报 `has tables but lacks the authority set`,按 README 清卷。docker 调试栈已接写者授权和 player_inbox,没有运行过,步骤见 `tools/scripts/README.md`。

K8s 上要先预置 `domain-service-secrets` 的 `player-store-scene`、`match-probe`、`match-outcome-battle`、`player-inbox-match`、`rank-producer-match`(至少 32 字节);按表授权的环境把 `player_store` 加进授权计划,go/db 账号加全局库 `player_route` 的 SELECT,match 账号加 `zone_merged_into` 的 SELECT,操作员加 `zone_merged_into` 的 `SELECT,INSERT,DELETE`(`docs/ops/k8s-open-server-runbook.md` §1)。全新集群分开跑是 `infra-up` → `zone-up` → 再 `infra-up`,`all-up` 自动按这个顺序。staging、prod 必须显式传 `-PlayerRedisMasters`(至少 3),口令 Secret 是 `redis-player-auth`(来自 `MMORPG_PLAYER_REDIS_PASSWORD`)。

场景:建角、进场、存盘、下线、重连、跨节点交接、交易冻结(含从未进场的角色直接上架)、崩溃持有者(`admission_recover` plan 和 apply)、结算、收件箱、自动放弃、判死(含节点崩溃后重登和 LeaveScene 重试撞在一起)、发号(etcd 里有 `/id_issuer/counter/primary` 和 `secondary`,玩家 ID 大于 2^53,Lua 和 Java 网关正常)、聊天发送、榜单(单局、封榜)、战斗(`robot` 的 battle-smoke、跨区版、team-smoke,先走地域探测再进匹配,结算只经收件箱,`battle:settlement:pending:*` 键不存在)、线上拉数(`debug_fetch` 导出,`debug_import` 预览并写进本地角色后能登录)、不停服合服(手册 §11 的场景表)。

mysql 档按总体方案 §9.4 的 7 项单独跑一轮。

### 11. 压测(阶段 B)

按 AGENTS §6 全部流程:先存 `prev-summary.txt`,清库,跑完用 `stress_summarize.ps1` 出对比表。这是新路径第一次压测,旧基线的存盘段全部不可比,要在复盘里写明。看:TiDB 写入与事务数、PD 时间戳、冷读和热加载 P99、`db_player_store_seconds`、`entergame_total{result="pool_saturated"}`,以及总体方案 §8.1 的四类热点是否消失。没有对比表不要声明性能结论。

### 12. Redis 集群(集群化文档 §14)

每步的细节在 `docs/PROGRESS.md` 的「玩家态 Redis 集群化第 9、10 批」条目。按这个顺序:

1. 开发集群:`docker --context desktop-linux compose -f deploy/compose.redis-cluster.yml up -d`,`docker --context desktop-linux wait redis-player-cluster-init` 打印 0。按 `tools/scripts/README.md`「compose.redis-cluster.yml」把本机服务指过去,跑 robot 的 login-test 和 travel-smoke。这几份配置改动不要提交。
2. 故障与迁移:设计 §14.2。存盘闭包、补投、go/db 发布和游标的用例已作废,其余照跑。
3. 容量:`redis_cluster_capacity.ps1`。压测端同机时退出码 2 是预期;结论要在独占主机的环境跑(§14.3)。
4. kind:`dev_tools.ps1 -Command k8s-infra-up -PlayerRedisMasters 3 -WaitReady`,再 `k8s-zone-up -PlayerRedisMasters 3`,走手册 §3~§5。
5. 告警:`deploy/k8s/redis-player-alerts.yaml` 的 exporter 指标名是按 v1.62.0 凭记忆写的,阈值是占位值,部署后核对。

## 没写的

- 阶段 B(压测)由 Codex 跑;阶段 C(拆跨服务同事务耦合、目录跨集群、多集群、分区搬迁、多地域、SLG 拆图)是上线后的事。
- docker 调试栈的写者授权和 player_inbox 10-31 已接(未运行,待 Codex 验证,见 `tools/scripts/README.md`)。
- 客户端(`../mmorpg-client/`)要跟进:重新生成协议;进匹配前走 `GetNetworkProbePlan` 和 `CommitNetworkProbe`,观战带 `request_id`(这两条是协议 1 本来就有的要求,robot 已改)。

## 已知残余

- 回档 RPC 已删,恢复玩家数据只走 PITR(`docs/ops/mysql-backup-pitr-runbook.md` §4.2)。
- 函数名还带 Redis 味道的只剩 `HandlePlayerAsyncSaved` 这类泛称,不影响理解,没改。
- 冻结存盘被拒(TOO_LARGE、SCHEMA_REGRESSED)时交易停住,不发 AckDrain,要运维摘掉。过户之后迟到的 Close 带旧修订,scene 每秒重发一次直到玩家再进这个实例或进程重启,只是空转。
- 跨集群搬迁没有重写,落点建角后不变(映射提案 §15.9(d))。
- 合服映射只靠定时重读(默认 5 秒,各服务两个周期内生效),没做通知加版本号。撤销合服后,合服期间跨原始区组成的队伍走完在途的,新开战回 `kTeamCrossZoneDenied`(手册 §7);期间在目标区建的公会和商品留在目标区。
- 本机写者授权:锁资格是本机声明,不是真实 I3 报告(文件正文写明);跨库授权清单来自读码,漏表会在服务日志里报 1142;data_service 对 guild、mail、match、inbox、trade 按库授 `SELECT,UPDATE`,比实际用到的宽;只影响本机。
- `k8s_zone_rollback.ps1` 删了 `-BarrierProtocol`,`-Apply` 只靠工具未落地和预检拦。
- `stress_round19.ps1`、`currency_crash_window.ps1` 的 `$GlobalDbName` 默认改成了 `mmorpg_global`。
- mysql 档:历练结果 topic 的 RF3 门禁不放开,省钱档也要 3 个 Kafka broker。
- 聊天:角色挂牌或过户后,旧会话最多再发和拉 `ActorVerifyTTLSeconds`(默认 30)秒;限流从精确变成近似。
- 榜单:投递途中榜分不保证全局顺序,只保证每人最终落在最新版本。
- MySQL 档的 guild、trade、mail 路径,死锁从 0 变成偶发,靠重试吸收,回归里要量重试率。
- 榜单 `RemoveRankEntry` 删行后版本丢失,迟到的旧批次能把人带回榜上(仓内没有生产调用方)。`StopRankSource`、`FreezeRankGeneration` 也没有生产调用方,「停写 → 投完 → 封榜」的编排要运营工具来做。
- Redis 控制域(选线、场景索引、成员集合、排队)按 zone 组分实例,一个组内是单主,加机器不能扩这一组的写。登录域和玩家域已进 Cluster,加主节点就扩。控制域的实际压力要看压测。
- 部署脚本只渲染登录域和玩家域共用一个集群,要拆开得再加参数。
- `zone_rollback_discard` 工具没落地,`k8s_zone_rollback.ps1` 只改了契约和预检。
- 玩家域镜像摘要沿用 Java 版的 `redis:8.10.2@sha256:6f81e891…`。Docker Hub 上这个标签 10-22 重建过,要换就两版一起换。
- 离场第二步失败、强制销毁之后才离场,都会留下成员,靠对账收敛(集群化文档 §5.10)。收敛前实例不销毁、选线躲着它,不影响数据。
- `LegacyGateLoginEnabled` 留着:它管的是经 gate 的凭据登录,与代次和存盘无关。

## 待用户确认

- 第二套 etcd(多一个 3 节点小集群,总体方案 §12 第 9 项)。代码已按要做写好。
- ID 值只用低 63 位(默认按此写,发第一个正式号之前还能改)。
- mysql 省钱档要不要放开历练结果 topic 的 RF3 门禁(现在生产 mysql 档仍要 3 个 Kafka broker)。
- go/db 选库是认领前强读一次全局库 `player_route`,之后存盘带着认领时返回的落点直接写,没有放进写事务里核落点,偏离映射提案选项的原文「同一 TiDB 事务里核落点」。理由:落点建角后不变,认领时读到的不会过期;写权行有代次,旧持有者写不进去;再核一次核不出不同的结果,只是给最热的写路径多加一次跨库读。前提:以后做玩家搬库时这条不成立,搬库要先让玩家下线并抬代次,或者回来改成事务内核对。

## 欠的评审

多集群部分(总体方案 §7、§8.2)的正确性评审,落码前补。

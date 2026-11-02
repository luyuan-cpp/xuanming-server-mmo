# 玩家存储水平扩展:落码进度与验证清单

新窗口接手时先读这份,再读 [总体方案](../../design/player-data-tidb-scaling-plan.md) 和 [存盘规格](../../design/player-save-direct-commit.md)。

用户 11-01 把整批提交并推送了,`main` 到 `2b8184428`,包括第三阶段(`docs/design/horizontal-scale-logical-db.md`)、第二轮修复、GM 回收和摘孤儿。之后修的代码还在工作区,所以验证要在工作区做。

**Codex 第一轮(11-01 03:44 到 04:46)的结果**:Go 除 `client_rpc_router` 外都编过了;C++ 被 protobuf 版本不一致挡住,一行业务代码都没编到;约 640 个真库用例因环境没配齐没碰到业务代码;没有跑过全栈。回执在 `build/player-store-verify-20261101/codex-results-round1.md`。第一轮的失败都按回执改了,改的内容见 `docs/PROGRESS.md`「按 Codex 第一轮回执修复」,**这些修复没有编译过**。下一轮从 §1 重新开始:第三阶段按 §13,GM 回收和摘孤儿按 §14,并回的修复按 §15,其后是 §16~§18。

## Codex 怎么跑、怎么回执(2026-11-01)

这一节是这一轮的口径,和下面总验证清单开头的说法冲突时以这一节为准。

**在哪跑**:只在 `D:\luyuan\xuanming-server-mmo-cpp` 这个目录里跑,不开 worktree,不切分支。整批已提交,但之后修的代码可能还在工作区,从 HEAD 新开的 worktree 看不到它们,验证的就成了旧代码。

**开跑前核对**,对不上就停下,写进回执:分支是 `main`;`git merge-base --is-ancestor 2b8184428 HEAD` 退出码是 0;`go/data_service/internal/logic/recall_plan.go`、`go/scene_manager/internal/logic/read_unknown.go`、`cpp/tests/rpc_controller_test/hiredis_event_batch_windows_test.cpp` 都存在。工作区条数不算门禁,用户随时可能提交。

**只跑不改**:不改代码、断言和配置,不提交,不 stash、reset、checkout。只有生成器、`go mod tidy`、`go mod vendor` 可以改文件。错误都写进回执,由 Claude 改,改完再跑下一轮。

**跑什么**:总验证清单 §1 到 §13,然后 §14、§15。例外:

- §8 etcd、§10 的 K8s 部分:没有环境就记「没跑」。
- §10 冒烟:§3、§6 全绿才跑,否则记「跳过」,写明依赖哪一步。
- §11 压测:这一轮不跑,其余全绿之后由用户决定。
- `gen_docs_index.py --check` 在 `2b20d351b` 就是红的,记下来,不算回归,也不要重新生成:索引里有手写条目,生成器会覆盖。

**失败怎么办**:不重试。协议生成失败就停,后面都依赖它。其余步骤失败不中断:一个 Go 模块编不过,接着跑下一个模块;C++ 编不过,接着跑部署合同测试。

**错误要拿全**,一次改完才省轮次:

- Go:`go build -gcflags=-e ./...`,默认每个包只报 10 条。编过了再跑 vet 和 test。
- C++:`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64 "/flp1:logfile=<日志目录>\msbuild-errors.log;errorsonly"`。

**日志**放 `build/player-store-verify-<日期>/`(已被 `.gitignore` 忽略),按步骤号命名。

**回执**写本目录的 `codex-results.md`,每一轮覆盖重写:

- 开头:基线提交、`git status --short` 条数,go、msbuild、pwsh、MySQL、TiDB、Redis 的版本。
- 每一步一节,标题是 `## <步骤号> <名字>:通过`(或 `失败`、`跳过`、`没跑`),下面写命令、退出码、完整日志路径。失败写错误摘要:Go 写全部编译错误,C++ 写前 50 行,测试写失败用例名和断言输出。
- 回执控制在 200KB 以内(11-01 第一轮有 1MB,读不完):同一个错误的用例合并成一条,写错误原文和用例名列表;通过的用例不写;跳过的用例写进日志目录的 `<步骤号>-skipped.txt`,回执只写条数和路径,按没跑算。
- 最后一行写 `状态:本轮跑完`。这一行写完就不再改这个文件,Claude 靠它判断可以开始改代码了。

不要写性能结论,不要写「基本通过」。

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

阶段 B(压测)由 Codex 跑。阶段 C 里的拆跨服务事务、目录跨集群、多集群、多地域已在第三阶段提前做完(见文末);分区搬迁和 SLG 拆图不在这一轮。

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

**工作区**:基线是 `main` 的 `2b8184428`,之后修的代码可能还在工作区,未提交。不要整理、不要回退。快照:`refs/snapshots/before-stage-c`(第三阶段之前,tree)、`refs/snapshots/before-b8`、`refs/snapshots/phase-a-written`,`git show <ref>:<路径>` 取回。

**前置清理**:项目没上线,所有库清掉重建,不写迁移。主键变了的表:`match_rank_source`、`match_rank_outbox`、`rank_input_receipt`;`rank_score` 多一列。不再建的表:`player_database`、`player_database_1`、`id_segment`、`zone_merge_fence`、`save_barrier_maintenance_receipt`、`account_role_admission_recovery`、`route_reconcile`、`account_role_import_receipt`。按 AGENTS §6.2 第 6 条删掉 redis、mysql、etcd 数据。 第三阶段新增的库:目录片库 `<base>_NN`(K=1 时就是 `<base>`)、流水库 `<base>_ledger`,各服务库新增 `role_fence`、闸门表、`db_move_sentinel`,都由部署脚本建。

### 1. 协议生成

生成器改过(`db_model.go` 跳过没有 `E_OptionTableName` 的消息、建表清单同时写 `go/db/data`,新增退役产物清退),要先编译生成器:

```bash
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build
```

```bash
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml
```

生成器现在只用和 C++ 运行库同版本的 protoc:优先 `third_party/protobuf/install_vs2026_dbg/bin/protoc.exe`(35.1),其次 PATH 上的,版本都对不上就报错退出。11-01 第一轮用的是 PATH 上的 35.0,全部 C++ 工程编不过。

通过标准:
- `cpp/generated/proto/common/base/node.pb.h` 里是 `#if PROTOBUF_VERSION != 7035001`。
- `generated/data/mysql_database_table_list.json` 和 `go/db/data/` 的同名文件有 `player_store`,没有 `player_database`。两份由生成器同一次写出,字节相同:本机 go/db 读前一份,容器里读后一份(`deploy/k8s/Dockerfile.go-svc` 拷到 `/app/data/`)。
- `go/proto/player_store/`、`cpp/generated/grpc_client/player_store/` 存在。
- 旧生成物要没有:`db_task.pb.*`、`battle_settlement_ledger_comp.pb.*`(`cpp/generated/proto/`、`go/proto/`、`robot/vendor/proto/` 三处),以及 `generated/proto/**` 下的 `db_task.proto`、`battle_settlement_ledger_comp.proto` 副本;`cpp/generated/proto/CMakeLists.txt`、`proto.vcxproj`、`proto.vcxproj.filters` 也不再引用它们。**这些由生成器自动清退**(`tools/proto_generator/protogen/internal/prototools/retired_outputs.go`,流水线最后一步,工程清单同步成功后才删):只删源 proto 已不存在、且文件头能证明出自生成流程的产物,删之前打印整张清单(文件的日志是 `Retiring generated outputs`,工程条目的日志是 `Removing retired proto entries`)。不要手工删文件或改工程清单,清不掉就按生成器问题报告。按当前工作区推算,第一次运行删 27 个文件、15 条工程条目,比上面列的多出更早留下的 `player_migration_event` 和 `generated/proto/**`、`generated/robot/proto/` 下的 `centre/*`、`player/player.proto`、`contracts/kafka/centre_command.proto`,都是源已删除的残留,没有代码引用;第二次运行不再删任何东西。
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

**环境要求**(11-01 第一轮缺这些,约 640 个用例没碰到业务代码就失败了,每条对应一类报错):

- **写者资格夹具**(报「缺少fixture物理authority原件摘要」):跑真库用例之前 dot-source 一次,TiDB、MySQL 各一份,切库之前再跑一遍。它同时设 `MMORPG_SQL_FIXTURE_ADMIN_DSN`、`MMORPG_SQL_BACKEND`,并按要求的 ACL 建好私有目录。

  ```powershell
  . tools/scripts/sql_fixture_env.ps1 -Backend tidb -Endpoint 127.0.0.1:14000 -ReceiptDir <日志目录的绝对路径>\sql-fixture-receipts-tidb
  ```

- **TiDB 开 CHECK**(报 `CHECK constraint chk_route_shard is not enforced`):建表之前执行 `SET GLOBAL tidb_enable_check_constraint=ON`。
- **库名白名单**(用例会 DROP 表,所以有白名单):`GUILD_TEST_MYSQL_DSN` 的库只能是 `guild_test` 或 `guild_it_<pid>_<n>`;`PLAYER_INBOX_TEST_MYSQL_DSN` 的库只能是 `player_inbox_it` 或 `player_inbox_it_<pid>_<n>`。
- **DSN 要带库名**(报 `Error 1046 No database selected`、`权威SQL配置无效`):`FRIEND_TEST_TIDB_DSN` 指向建好的专用库,比如 `root@tcp(127.0.0.1:14000)/friend_it?parseTime=true`。
- **榜单邮件整链**(报 `RANK_MAIL_CHAIN_MAIL_EXE 必须指定预编译夹具绝对路径`):先编两个夹具,再把 `RANK_MAIL_CHAIN_RANK_EXE`、`RANK_MAIL_CHAIN_MAIL_EXE` 设成它们的绝对路径。

  ```bash
  cd go/rank && go test -c -o <日志目录的绝对路径>/bin/rank_chain.test.exe .
  ```

  ```bash
  cd go/mail && go test -c -o <日志目录的绝对路径>/bin/mail_chain.test.exe .
  ```

- **`-race`** 要 cgo 和 gcc(`CGO_ENABLED=1`,gcc 在 PATH)。本机没有 gcc 就记「没跑」,再跑一遍不带 `-race` 的。
- **TiDB 端口随意**:聊天用例以前写死 `127.0.0.1:24000`,已改成只要求数值 loopback。24000 被别的 `codex-*` 容器占着,不要动它。

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
| go/guild | `GUILD_ROLE_TEST_DSN`、`GUILD_ROLE_REQUIRE_TIDB=1` | `go test ./internal/data -run TestTiDBTrial` |
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

Redis 这批在 `rpc_controller_test`:`RedisClusterClientTest.*`(含 `IdleSlotOnHandshakingMasterQueuesInsteadOfRejecting`)、`HiredisCommandLifecycle.*`(含 `NoSideEffectCommandNullReplyKeepsFrontierKnown`)、`redis_cluster_logic_test.cpp`。`Hiredis` 改了 `muduo_windows` 和 `tools/archived/muduo_linux_overlay` 两份实现,Linux 的 C++ 镜像(`deploy/k8s/Dockerfile.cpp`)也要编一次。`BUILD_JOBS=1` 要跑很久,放后台,不要设短超时:11-01 第一轮退出码 130、`context canceled` 是构建被中断,还没进入编译。

### 7. 部署合同测试

`tools/scripts/tests/` 下的 `*.tests.ps1` 全部跑,一个失败不中断其余的:

```bash
pwsh -NoProfile -Command "$failed=@(); Get-ChildItem tools/scripts/tests/*.tests.ps1 | ForEach-Object { pwsh -NoProfile -File $_.FullName; if ($LASTEXITCODE -ne 0) { $failed += $_.Name } }; $failed; exit $failed.Count"
```

再跑 `pwsh -File tools/scripts/test_stress_summarize.ps1`。

重点:`k8s_deploy_contract`(全局服务顺序、`Find-ReadyDataServiceNamespace`、startupProbe 预算)、`k8s_client_entry_contract`、`k8s_redis_split_contract`、`k8s_migrate_gate`、`k8s_route_tool`、`lobby_authority_render`、`global_authority_deploy`、`chat_authority_deploy`、`go_services_local_mysql`、`go_services_ports`、`go_services_stop`、`maintenance_drain`、`sql_boot_catalog_local`、`id_issuer_deploy_contract`、`etcd_maintenance_contract`、`dev_tools_merge_zone_contract`、`redis_player_cluster_contract`、`stress_snap_redis`、`docker_debug_sql_boot`。再跑 `python tools/scripts/gen_docs_index.py --check`。`go_services_ports` 在基线就缺写者函数桩,chat、trade 启动类用例失败先和基线对比。

两份裸指针合同不能不带参数跑:`no_raw_pointer_check` 要先有 `cpp/plugin/build/Release/no_raw_ptr_check.exe`(`pwsh -File tools/scripts/dev_tools.ps1 -Command no-raw-pointer-setup`,没有 LLVM 环境就记「没跑」);`no_raw_pointer_project` 要传参数:

```bash
pwsh -NoProfile -File tools/scripts/tests/no_raw_pointer_project.tests.ps1 -MSBuildPath "<MSBuild.exe 的绝对路径>" -EvidenceDir "<日志目录的绝对路径>/07-no-raw-pointer-project"
```

### 8. etcd

按 `docs/ops/etcd-maintenance-runbook.md`:M0 取镜像摘要,`etcd.yaml`、`etcd-maintenance.yaml`、compose 三处同批钉(`bitnamilegacy/etcd:latest` 现在没钉);M3;探针 A;P0-D 实跑。没有环境就如实报告没跑。首次部署之前必须跑完。

### 9. Java 网关(A7c)

`GATEWAY_TEST_MYSQL_URL` 指向库名以 `_it` 或 `_test` 结尾的专用 MySQL,设 `GATEWAY_REQUIRE_MYSQL_TESTS=1`,跳过即失败。`gateway_node` 依赖 `authority_jdbc`,要从 `java/` 走 reactor,不要依赖本地 m2;`authority_jdbc` 要显式给 protoc,用 SDK 里 35.1 那个:

```bash
cd java && mvn -pl gateway_node -am test "-Dtest=RowIdsTest,GatewaySchemaContractTest,ZoneWhitelistRepositoryWiringTest,ZoneWhitelistUpsertLockOrderMySqlTest,AdminWhitelistControllerTest" -Dsurefire.failIfNoSpecifiedTests=false "-Dprotoc.spec=file:/D:/luyuan/xuanming-server-mmo-cpp/third_party/protobuf/install_vs2026_dbg/bin/protoc.exe"
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
5. 告警:`deploy/k8s/redis-alerts.yaml`、`redis-player-alerts.yaml` 的 exporter 指标名 10-31 已对照 v1.62.0 源码核对(`redis_errorstat_total` 不存在,改成了 `redis_errors_total`),部署后再对着 `/metrics` 看一次。阈值是占位值。

### 13. 第三阶段:全层水平扩展

规格 `docs/design/horizontal-scale-logical-db.md`,手册 `docs/ops/db-move-runbook.md`、`docs/ops/open-new-region-runbook.md`。§2、§3 的模块列表再加 `tools/dbmove`。这批比前两批风险高,按这个顺序跑,一步失败先报告。

**1. 生成器和协议**(先跑,后面都依赖它):

```bash
cd tools/proto_generator/protogen && go test ./internal/prototools/... ./internal/generator/go/... ./internal/utils/...
```

```bash
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build
```

```bash
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml
```

通过标准:生成器测试绿;首跑清退退役产物(只读推演约 27 个文件、15 条工程条目),**再跑一次零删除**;`generated/data` 和 `go/db/data` 两份建表清单字节相同;`go/proto/role_fence/` 存在;`git status` 里只有生成物变化。消息号保护可能拦下批量退役,按上一轮 Codex 的做法核对后显式放行。

**2. 单测**(都在对应模块目录下跑 `go build ./... && go vet ./... && go test ./...`):`go/shared`(重点 `rolefence`、`dirshard`、`dbplacement`、`authoritysql`、`serverbase`,含跨库 SQL 契约扫描 `-run CrossDatabase`)、`go/data_service`、`go/db`、`go/login`、`go/guild`、`go/match`、`go/trade`、`go/mail`、`go/friend`、`go/player_inbox`、`go/chat`、`go/rank`、`go/scene_manager`、`tools/dbmove`、`tools/merge_zone`、`tools/data_consistency_check`、`go/schemamigrate`。`tools/dbmove`、`tools/merge_zone` 先 `go mod tidy`。

**3. 真库**:

| 模块 | 环境变量 |
|---|---|
| go/data_service | `ACCOUNT_DIRECTORY_TEST_DSN`、`MMORPG_SQL_FIXTURE_ADMIN_DSN`;`-tags=integration` 加 `DATA_SERVICE_IT_MYSQL_*` |
| go/shared | `MMORPG_SQL_FIXTURE_*`,在 `./authoritysql` 跑 `TestGate\|TestWriterGrant` |
| go/db | `GODB_MYSQL_ITEST_DSN` |
| go/match | `MATCH_IT_TIDB`、`MATCH_IT_REQUIRE_TIDB=1` |
| go/guild | `GUILD_ROLE_TEST_DSN`、`GUILD_ROLE_REQUIRE_TIDB=1` |
| go/trade | `COMMERCE_TEST_DSN`、`COMMERCE_REQUIRE_TIDB=1` |
| go/mail | `MAIL_RANK_TEST_TIDB`、`MAIL_RANK_REQUIRE_TESTS=1` |
| go/friend | `FRIEND_TEST_TIDB_DSN`、`FRIEND_REQUIRE_TIDB_TESTS=1` |
| go/player_inbox | `PLAYER_INBOX_TEST_MYSQL_DSN`、`PLAYER_INBOX_REQUIRE_MYSQL_TESTS=1`,`-tags integration` |
| go/chat | `CHAT_IT_TIDB`、`CHAT_IT_REQUIRE_TIDB=1` |
| tools/dbmove | `DBMOVE_IT_REQUIRE=1`、`DBMOVE_IT_SOURCE_DSN`、`DBMOVE_IT_TARGET_DSN` |

**4. 部署契约**:`logical_db_deploy_contract`、`id_issuer_deploy_contract`、`k8s_*`、`redis_player_*`、`lobby_*`、`chat_*`、`go_services_*`、`docker_debug_*`、`sql_boot_*`、`maintenance_drain`、`dev_tools_db_move_contract`。另外 `java/authority_jdbc` 跑 `mvn -q test -Dtest=AuthorityConnectionTest "-Dprotoc.spec=file:/D:/luyuan/xuanming-server-mmo-cpp/third_party/protobuf/install_vs2026_dbg/bin/protoc.exe"`,scene 改了 `player_team.cpp`,C++ MSBuild `/m:1` 再编一次。

**5. 全链路**:开发档 K=1 起全套;再用 `-DirectoryLogicalShards 4`(或本机配置 `Directory.LogicalShards: 4`)起一遍,跑:

- **建角和分片**:建角、名字撞名、满位拒绝、建角途中停存档库(回 `ErrNotCommitted`,名字释放,名额退还)、登录角色列表。
- **过户全链**:寄售、Begin、准备期间玩家照常进场、有挂起被拒且目录无写入、FREEZING 后被踢、Confirm、Transfer、新主进场、旧主被拒、卖家列表看不到、Cancel。每一步杀 data_service 或参与者后重放。
- **各服务冻结**:冻结期间 guild、match、trade、mail、friend、收件箱写入被拒;解冻后恢复;手工清空某服务的 `role_fence` 后自愈。
- **历练和匹配互斥**:有票不能建房,大厅里不能排队,开局占用,取消和租约到期。
- **维护排空**:逐库排空,确定性拒绝和闸门冻结不打成 HOLD。

**6. 不停服搬库**:手册 §10 全部(含 TiDB 和 MySQL 两档,TiCDC 字段 10-31 已对照官方源码核对,演练时对着真实版本再看一次),重点:搬 `player_store` 库时玩家在线且存盘不丢;搬目录分片;搬业务库;杀工具、断复制、etcd 不可用、翻转前连目标写者。冻结窗口时长记下来。

**7. 第二套 etcd 强规定**:生产配置两套地址相同时拒绝;地址不同但指向同一集群时,预留和 `-id-issuer-init` 回 `same etcd cluster`;`Mode: pro` 配 `AllowAutoSeed: true` 发号器不装配。地域码 0 只给 dev、test:prod、staging 部署 `-Region 0` 拒绝,`Mode` 不是 dev、test 且没配地域码时,data_service 启动和 `-id-issuer-init` 拒绝。

**8. 压测**:按 AGENTS §6。这是第一次按分片压,重点看:目录每片连接和 QPS、`GetRoleFenceState` 和 `GetPlayerRoute` 多出来的一跳、`role_fence` 行锁、闸门 slot、连接总账是否够。**没有对比表不要声明性能结论。**

**9. 灾备演练**:在隔离环境按 `docs/ops/mysql-backup-pitr-runbook.md` §4.0 走一遍整个地域回档:TiDB 档用 §4.1b,MySQL 档用 §4.1。重点看三件事:跳过 `match-results` 积压之后消费组不卡;T 时刻在打的对局到期判过期;起服后闸门可写,建角、登录、存盘正常。单 zone 回档脚本已退役,不用跑。

**10. 第二轮评审之后补的回归**(10-31 到 11-01,跟着 §2、§3 跑,都没有运行过):

- **协议要先生成**:`proto/rank/rank_table.proto` 加了 `RankRemovedEntryRecord`(新表 `rank_removed_entry`);`proto/data_service/data_service.proto` 加了 RPC `CheckAccountRoleSlot`。
- `go/shared`:`./authoritysql`(写资格第一条写语句之前才消费、`IsCommitOutcomeUnknown`)、`./accountdirectory`(`ErrRetryLater`)、`./serverbase`、`./rolefence`、`./domainhost`(发号器预领失败只告警)、`./dbplacement`。
- `go/data_service`:`./internal/store`(`-migrate` 幂等、`route_meta` 先读后 INSERT、迟到的 Close)、`./internal/server`、`./internal/svc`(地域码 0、`MMORPG_ID_ISSUER_EXPECTED_REGION`)、`.`。
- `go/trade`:`./internal/commerce`(买家满位退款、`OutcomeKnown`、`Engine.Tick` 的提交结局未知)、`./internal/logic`、`./internal/svc`。
- `go/guild`:`./internal/session`、`.`。`go/friend`:`./internal/session`、`./internal/constants`、`./internal/logic`、`./internal/data`、`.`。`go/mail`:`./internal/server`、`.`。`go/rank`:`./internal/data`、`.`。`go/match`:`./internal/logic`、`./internal/authority`、`./internal/team`、`.`。`go/player_inbox`:`./internal/inbox`、`./internal/logic`、`./internal/server`。`go/login`:`./internal/logic/clientplayerlogin`、`./internal/logic/admin`(目录客户端对 `Aborted` 的解读变了)。
- `tools/dbmove`:`go test ./...`,新用例在 `mover_safety_test.go` 末尾和 `main_test.go`。真库用例(`DBMOVE_IT_*`)里加了 `GateState`、`AccountLocked`、`CheckSeesAllConnections`。
- 改过既有断言的地方要留意,和新行为冲突时报告,不要改断言:`account_directory_test.go` 的 `{"full", ErrFull, true}` 改成 false;`account_role_cross_domain_tidb_test.go` 改成 `ErrRetryLater`;`TestTransferFailureBeforeCommitThenReplay` 加了 readOnly;`role_authority_integration_test.go` 的冻结动作表删了 ClearNotify、DeferNotify;`TestLoadWriterConfigsRejectsBadSetBeforeConsuming` 第二次读改成成功。

**11. 部署**。先跑契约测试(不连集群,逐个串行,`pwsh -NoProfile -File tools/scripts/tests/<名字>.tests.ps1`,全绿才算过):`k8s_migrate_gate`、`logical_db_deploy_contract`、`k8s_deploy_contract`、`id_issuer_deploy_contract`、`lobby_authority_render`、`maintenance_drain`、`go_svc_image_authority`、`k8s_route_tool`、`chat_authority_deploy`、`k8s_redis_split_contract`、`redis_player_cluster_contract`、`sql_boot_catalog_local`、`docker_debug_sql_boot`、`global_authority_deploy`、`dev_tools_merge_zone_contract`、`k8s_login_auth_provider_guard`。`k8s_client_entry_contract` 也跑,读代码判断可能有已有的红测(部分 DryRun 没给 `-GlobalAuthorityTiDBHost`),和基线对比着看。

再在 kind 上按开服手册 §5 走全新集群:`all-up -SqlSchemaOnly` → 读 generation、写名册、执行 `operator` 和 `runtime` 两段授权 → `all-up` → `-id-issuer-init` → §7 验收。重点核:

- 再跑一遍 `-SqlSchemaOnly`,Job 都回 0,generation 不变。
- `go_svc_image.ps1 -Command build-all -Services db-migrate` 出得来镜像,`db-migrate` Job 能跑。
- **只有真实集群才能确认的事实**,这批改动都依赖它们:emptyDir 根是不是 `drwxrwsrwt`,`private` 子目录过不过写者的私有目录校验(`stat -c '%a %U %n'`),Java 的属主判断和 Spring 从 `/app/config/private` 加载配置;带 CREATE 的表级 GRANT 在 MySQL、TiDB 空库上是否都不报 1146;K8s 版本 ≥ 1.26(`podFailurePolicy`)。
- **回收 CronJob**:让一个写过的写者主容器退出,入口脚本以 78 退出,2 分钟内 Pod 被换掉,新 Pod 领名册下一条,`logs job/<job> -c select-restarted-writers` 和 `-c delete-restarted-writers` 都有 namespace 和 Pod 名。让一个还没写过的进程退出(停掉 data_service 再起 guild),容器原地重启,Pod 不被删,依赖好了自己起来。名册用完时 Pod 停在 Init 不被删,`SqlWriterRegistrarFailing` 响。另外核 go-runner 写文件的行为、go-template 的 `printf`、`eq` 在 v1.35.10 的结果、空文件 `kubectl delete` 退出 0、uid 10001 能不能读 SA token。

## 没写的

- 阶段 B(压测)由 Codex 跑。分区搬迁、SLG 拆图不在这一轮。
- docker 调试栈的写者授权和 player_inbox 10-31 已接(未运行,待 Codex 验证,见 `tools/scripts/README.md`)。
- 客户端(`../mmorpg-client/`)要跟进:重新生成协议;进匹配前走 `GetNetworkProbePlan` 和 `CommitNetworkProbe`,观战带 `request_id`(这两条是协议 1 本来就有的要求,robot 已改)。

## 2026-11-01 查出的遗留:别的会话写完但没收尾

- **两份停在 worktree 的修复已并回主工作区**(11-01,未提交,未编译,验证见 §15):`.claude/worktrees/sleepy-dhawan-974040`(Hiredis 三份副本一致和 Windows 回归)、`.claude/worktrees/musing-jang-65ade6`(scene_manager 读 Redis 出错不再当成键不存在)。这两个会话可以归档了,用户说了再归档。
- (2026-11-01 更正)下一条已收尾:模板和主路径没发现拉不起来的问题,补了 `redis-split-status` 查实例、代理连接总账、21 条契约用例和手册,未运行,见 §16。
- **排队 Redis 高可用断在半路**(会话 `local_84c4ba6b`,额度耗尽):`redis-purpose-ha.yaml` 和 `k8s_deploy.ps1` 的函数都在,没有人复核;`k8s_redis_split_contract.tests.ps1`、`deploy/k8s/README.md` 都没提它。
- **(2026-11-01 更正)下面 4 个 worktree 已核对,要紧的改动都已并回主工作区**,见 §16;排队 Redis 高可用的复核也在 §16。完整缺口清单在 [server-gap-audit-20261101.md](../server-gap-audit-20261101.md)。
- **另外 4 个 worktree 有未提交改动,没核对是否进了 `main`**:`relaxed-cerf-d5da4d`(Refresh token)、`compassionate-dirac-4d87e1`(C++ 日志级别)、`laughing-wright-5dc98a`(gate D60)、`peaceful-haibt-6a19a3`(帮会资产恢复)。

## 已知残余

- 回档 RPC 已删,恢复玩家数据只走 PITR(`docs/ops/mysql-backup-pitr-runbook.md` §4.2)。
- 函数名还带 Redis 味道的只剩 `HandlePlayerAsyncSaved` 这类泛称,不影响理解,没改。
- 冻结存盘被拒(TOO_LARGE、SCHEMA_REGRESSED)时交易停住,不发 AckDrain,要运维摘掉。
- 跨集群搬迁没有重写,落点建角后不变(映射提案 §15.9(d))。
- 合服映射只靠定时重读(默认 5 秒,各服务两个周期内生效),没做通知加版本号。撤销合服后,合服期间跨原始区组成的队伍走完在途的,新开战回 `kTeamCrossZoneDenied`(手册 §7);期间在目标区建的公会和商品留在目标区。
- 本机写者授权:锁资格是本机声明,不是真实 I3 报告(文件正文写明);跨库授权清单来自读码,漏表会在服务日志里报 1142;data_service 对 guild、mail、match、inbox、trade 按库授 `SELECT,UPDATE`,比实际用到的宽;只影响本机。
- `stress_round19.ps1`、`currency_crash_window.ps1` 的 `$GlobalDbName` 默认改成了 `mmorpg_global`。
- mysql 档:历练结果 topic 的 RF3 门禁不放开,省钱档也要 3 个 Kafka broker。
- 聊天:角色挂牌或过户后,旧会话最多再发和拉 `ActorVerifyTTLSeconds`(默认 30)秒;限流从精确变成近似。
- 榜单:投递途中榜分不保证全局顺序,只保证每人最终落在最新版本。
- MySQL 档的 guild、trade、mail 路径,死锁从 0 变成偶发,靠重试吸收,回归里要量重试率。
- 榜单 `StopRankSource`、`FreezeRankGeneration` 没有生产调用方:赛季首发不做(全球匹配设计 D7),要做赛季时再写「停写 → 投完 → 封榜」的编排。
- (2026-11-01 更正:下一条与代码不符。只有排队拆了,其余控制域全服连一个主节点,见 [server-gap-audit-20261101.md](../server-gap-audit-20261101.md) 第 5 条。)
- Redis 控制域(选线、场景索引、成员集合、排队)按 zone 组分实例,一个组内是单主,加机器不能扩这一组的写。登录域和玩家域已进 Cluster,加主节点就扩。控制域的实际压力要看压测。
- 部署脚本只渲染登录域和玩家域共用一个集群,要拆开得再加参数。
- 玩家域镜像摘要沿用 Java 版的 `redis:8.10.2@sha256:6f81e891…`。Docker Hub 上这个标签 10-22 重建过,要换就两版一起换。
- 离场第二步失败、强制销毁之后才离场,都会留下成员,靠对账收敛(集群化文档 §5.10)。收敛前实例不销毁、选线躲着它,不影响数据。
- `LegacyGateLoginEnabled` 留着:它管的是经 gate 的凭据登录,与代次和存盘无关。

- **第三阶段**:连接上限现在是每片每域,`K=64` 时总账手算约 5682 条,没有压测。`MigrateSchema` 崩在建表当中的那一个库是半成品,要人工删掉再跑,其余库接着建。K=64 时名册一条约 40KB(估算),Secret 上限 1MiB,约 25 条,副本多时要算。写者主容器崩溃后要等回收 CronJob 换 Pod,最多约 2 分钟,这段时间少一个副本。`BatchGetPlayerName` 读 `player_name_owner`,不核 `player_name`,不一致时以后者为准但读不到。去掉 `UNIQUE(player_id)` 后名字的死锁没有重测。match 的 `home_zone` 事务外走 RPC,`JoinQueue` 多一跳。目录的只读 RPC 失败回 `Unavailable` 仍算未知,会 Hold。
- **搬库**:冻结窗口读写都暂停,回可重试错误。Java 网关的 `mmorpg` 库翻转后要滚动重启。写者账号共用时(`-allow-shared-writer-accounts`)目标不锁账号,只靠拨号和查连接。exporter 和闸门告警的阈值是占位值。
- **不做**:按玩家搬库(逻辑库整库搬已覆盖,要做时必须在写权行加墓碑,规格 §7);全局账号目录(每个地域一份角色列表,规格 §9);跨地域实时对战(没有确定玩法,规格 §9 留了做法)。

## 已定的决定(2026-10-31)

负责人 10-31:「全部按最标准的修复,我不怕改动,而且我确实会有不停服迁移物理库的」。详情见[全层水平扩展规格](../../design/horizontal-scale-logical-db.md) §10。

- **第二套 etcd**:强规定,写进 AGENTS §7。
- **ID**:类型 64 位,值只用低 63 位。
- **mysql 档**:不放开历练结果 topic 的 RF3 门禁,要 3 个 Kafka broker。
- **go/db 选库**:落点 `storage_id` 建角后不变,认领时读一次路由。物理位置由规格 §8 的映射决定,闸门在同一事务里拦住旧位置的写。不做按玩家搬库。

## 第三阶段:全层水平扩展(2026-10-31,已写完,未编译)

做法和批次见规格 §11。写完之后做了三路只读评审,共 26 条问题(护栏 8 条、目录和存档 6 条、闸门和搬库 12 条),全部修了(FX1~FX3 和最后的排空判定)。评审没有发现旧主能写、复制资产或一号两主的路径。修复本身没有再评审,部署脚本也没有单独评审,靠契约测试和 Codex 实跑。

## §14 GM 批量回收与摘孤儿(2026-11-01,未编译)

方案:[exploit_loss_prevention_zh.md](../../design/exploit_loss_prevention_zh.md)「BatchRecallItems 执行方案」、[account-role-authority.md](../../design/account-role-authority.md)「摘孤儿角色」。

Codex 验证,都在仓库根下,串行,失败留完整输出,不要重试:

1. 协议生成:`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。这批新增 `gm_recall_*` 四张表、`AssetOpStream` 第 6 条流、`AssetRecallLine`、三个 DataService RPC、`RecallKind`、`RemovePlayerOutcome`。删了 `RecallResult.item_uuid`(`reserved 2`)。
2. Go:

   ```bash
   cd go/shared && go build ./... && go test ./assetop/...
   cd go/data_service && go build ./... && go vet ./internal/... && go test ./internal/logic/ ./internal/store/ ./internal/svc/ ./internal/server/ ./internal/config/ ./internal/metrics/ ./internal/rolefencecoord/
   cd go/login && go build ./... && go test ./internal/logic/admin/ ./internal/server/loginadmin/
   ```

3. 真库:`cd go/data_service && go test -tags=integration ./internal/store/ -run TestRecallStore`。`DATA_SERVICE_IT_REQUIRE=1` 时 SKIP 判失败。覆盖状态机、跨批次去重、冻结和删除的角色、取消、取消和派发竞争、流水放回、护栏挂起。真实写者账号要有流水库 `gm_recall_*`、`role_fence` 的权限,这条用例用管理连接,覆盖不到授权。
4. C++(串行 `/m:1`):编 `cpp/tests/currency_test`、`cpp/tests/bag_test`。先跑 `currency_test --gtest_filter=AssetOpRecallTest.*:AssetOpAuthTest.GmRecallOnlyDataServiceAllowed:CurrencyTest.AttachDebt*` 和 `bag_test --gtest_filter=GuidSourcePolicyTest.*`。再全量跑一遍两个目标:`AttachDebt` 合并语义改了(已过期的旧欠款先作废,到期时间取较晚的),`BagService` 三个发放入口对不可叠加物品改成一件一条流水。`NamedInstanceIsRecalledFromAnyBagIncludingWornGear` 依赖配置 1 能放进装备栏,放不进去先看夹具。
5. 部署契约:逐个 `pwsh -NoProfile -File tools/scripts/tests/<名字>.tests.ps1`:`k8s_deploy_contract`、`k8s_redis_split_contract`、`redis_player_cluster_contract`、`redis_maxclients_contract`、`id_issuer_deploy_contract`、`logical_db_deploy_contract`、`docker_debug_sql_boot`、`sql_boot_catalog_local`。这批给 data-service 加了 Redis 连接(总账多一个池、一条 Pub/Sub、玩家域一个池),写死总数的断言会红,先看是夹具还是渲染。`Get-GrpcClientMethodsNotCalledFromCpp` 登记了 GM 回收三个方法,核这张表的用例要在 `proto-gen-run` 之后跑。
6. 端到端(本机栈):玩家在线,预览 → 执行 → `GetRecallBatch` 到 `error_code=0`,存档和 `transaction_log`(`tx_type=19`、`correlation_id=op_id`)都对得上。再做余额不足(差额进欠款,之后加钱先抵扣)、玩家离线(行停在 `PENDING`,上线几秒内扣到)、道具用掉(`shortfall`,`error_code=36`)、重放同一 `request_id`(不再扣)、取消之后改对条件重新预览(流水放回去了,还能回收)、过户被挡。`LoginAdmin`:没配口令回 `PermissionDenied`;配了之后在役角色回 `NOT_ORPHAN`,长名单分几次跑完。

没编译、没运行。只做过 `gofmt -l`、PowerShell 语法解析、YAML 解析。

已知残余:

- 流水早于落盘:scene 崩溃窗口内的货币收益可能多挂一笔欠款,窗口是单次存盘间隔。已人审 `plan_digest`,接受。
- 回收不留实例原件,要恢复按 `player_snapshot` 和 `transaction_log` 人工补发。
- 有缺口的道具没有自动折算。不可叠加物品只销毁点名的实例,同一件来回交易时缺口会多报。
- 摘孤儿分不出「路由行在、落点列为空」,这种行只可能是人改过库。
- 没配告警规则:`data_service_recall_dispatch_total{result="error"}`、`assetop_pending_oldest_age_seconds{stream="gm_recall"}` 还没进 `deploy/k8s`。离线玩家的行正常就会很老,阈值要等真实数据。

## §15 并回的两份修复(2026-11-01,未编译)

改动说明见 `docs/PROGRESS.md`「并回两份停在 worktree 的修复」。这两份在工作区,未提交,所以只能在主目录验证。日志放 `build/player-store-verify-<日期>/`,下面的 `<日志目录>` 就是它。

### 1. scene_manager:读 Redis 出错

miniredis 在进程内跑,不要外部 Redis、MySQL。都在 `go/scene_manager` 下:

```bash
go build -gcflags=-e ./...
```

```bash
go vet ./internal/...
```

```bash
go test -count=1 ./internal/logic/... ./internal/metrics/...
```

通过标准:build、vet 无输出;test 全部 `ok`,0 FAIL。尤其要过 `read_unknown_fail_closed_test.go` 全部用例,以及既有的 `TestAutoscale_*`、`TestOrphanCleanup_*`、`TestGetBestWorldChannel_*`、`TestInitWorldScenes_*`、`TestPlanRebalance_*`、`TestReconcileDeadNode_*`、`TestDesiredWorldChannelCount_*`、`TestWorldChannel_*`、`TestAtomicDestroyForce_*`、`TestOwnerReadInterleave_*`、`TestResolveScene*`。

失败时保留:编译、vet 的全部错误;失败用例用 `go test -count=1 -v -run '^用例名$' ./internal/logic/` 单跑的完整输出。失败落在这批没改的文件里,写明文件名。

### 2. Hiredis 收尾

MSBuild 一律串行 `/m:1`。测试工程只经 AdditionalDependencies 链接 `muduo.lib`,不会顺带重编它,所以先编 muduo。总验证清单 §6 的整解构建已经覆盖第 1 步时,不用重复。

```bash
msbuild cpp\libs\engine\muduo_windows\muduo.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64
```

```bash
pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter rpc_controller -LogDir <日志目录>/15-hiredis-rpc
```

```bash
build\cpp\tests\rpc_controller_test.exe --gtest_filter=HiredisEventBatchWindows.* --gtest_repeat=20
```

```bash
pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter muduo_grpc -LogDir <日志目录>/15-hiredis-grpc
```

`muduo_grpc` 覆盖 `HiredisPeerFin`,必须带 `-Build`,否则跑的是链接旧 `muduo.lib` 的旧 exe。

Linux:按 `docs/design/redis-async-client-retry-fix.md` 文末的 cmake 命令重跑 `redis_lifecycle_test`,`MMORPG_SDK_ROOT` 下的 muduo-linux 用 gitlink 当前记录的 SHA(现在是 `b55b0ac`),回执写明 SHA。再在 `f1fc77e` 上跑一轮,证明端口写法两版都能编。没有 Linux 环境就记「没跑」。

通过标准:

- muduo 构建退出 0。
- `rpc_controller` 0 失败,`HiredisEventBatchWindows` 两条实际运行、没有跳过,重复 20 轮共 40/40。
- `muduo_grpc` 0 失败。`rpc_controller_test.exe`、`muduo_grpc_test.exe` 的修改时间晚于第 1 步产出的 `muduo.lib`。
- Linux `redis_lifecycle_test` 全部通过,ASan、UBSan 无报告。
- 三份 `Hiredis.cc`(`cpp/libs/engine/muduo_windows/src/contrib/hiredis/`、`tools/archived/muduo_linux_overlay/contrib/hiredis/`、`third_party/muduo/contrib/hiredis/`)两两 `cmp` 无输出,三份 `Hiredis.h` 同样。

失败时保留:msbuild 首个 error 和完整日志;`-LogDir` 下的 stdout、stderr;崩溃时的退出码、最后一个 `[ RUN ]` 和 stderr 断言原文;Linux 所用 muduo-linux 的 SHA。

## Codex 第 4 项双区联验补齐要求（2026-11-01）

负责人已明确：**下列源码/测试接线由原改码窗口补齐，当前 Codex 只验证。** 本节不替代本目录 codex-results.md；该总验证回执仍由“执行玩家存盘总验证清单”聊天维护。

1. **恢复新 SQL 路径故障控制器**：旧 TestFullstackOwnershipFaults 已随 go/db 旧 Kafka 存盘链删除，run_ownership_fault_fullstack.ps1 仍调用它。保留真实进程身份、租约等待、恢复看门狗与机器人正式协议，改按 player_store 原子提交及写权行判断提交受阻/旧主恢复拒写。完整边界和通过标准：[ownership-fault-verification-blockers.json](../../../build/verification/stage4-resume-20261101-r1/ownership-fault-verification-blockers.json)。
2. **补齐当前双区写者启动接线**：本机工具当前仅单区，需一次合并实例计划再迁移/注册/发布 catalog；校准顶层 ZoneId/Node.ZoneId、语义区号与真实端口、data_service 参与者最终地址，J 的四个角色继续由 J 正规 producer 生成。不得循环生成两份互相覆盖的 catalog 或降低写资格门禁。完整边界：[multi-zone-boot-repair-plan.json](../../../build/verification/stage4-resume-20261101-r1/multi-zone-boot-repair-plan.json)。

已有失败的额外诊断供本轮统一修正：

- C++ 本轮生成器实际取环境中的 protoc 35.0，而 Debug SDK 为35.1，生成宏7035000与runtime7035001不符。不要手改生成头或删除版本检查；需正规生成工具来源对齐再构建。[cpp-protobuf-version-diagnosis.json](../../../build/verification/stage4-resume-20261101-r1/cpp-protobuf-version-diagnosis.json)。
- data_service 三项失败已区分为错误包装重新暴露地址、GetPlayerRoute 把取消误分为权限错误，以及假驱动行多一个nil；第三项不要改生产SQL/Scan。[data-service-three-failures-diagnosis.json](../../../build/verification/stage4-resume-20261101-r1/data-service-three-failures-diagnosis.json)。均只读定位，尚未修复或重跑。

本聊天的独立实证：Java官方ContractSync和check退出0，地图/内部协议保持；当前playerstore 12项真库TestIT_全部通过、0跳过（未race），原件在同目录playerstore-tidb-r2/。以上均不能代替双区真实节点联验。原自有PD随后因1GiB上限OOM，已按既有内存授权恢复同CID到4GiB/swap8GiB且health=true；后续A9接库前另做当前世代复核，旧资格不直接继承。

## §16 遗留修复与假成功的桩(2026-11-01,未编译)

清单和未做的项见 [server-gap-audit-20261101.md](../server-gap-audit-20261101.md)。Codex 下一轮验证时带上,都在仓库根下,串行,失败留完整输出:

```bash
cd go/shared && go test ./redisslot/...
cd go/guild && go build ./... && go test ./internal/logic -run 'TestWithClientRequestLookupFailure|TestClientRequestReplayBindsOriginalGuild' -count=1
cd go/guild && go test -tags integration ./internal/logic -run 'EconomyFlowClientRequest' -count=1
cd go/login && go build ./... && go vet ./internal/logic/pkg/token/... && go test ./internal/logic/pkg/token/... ./internal/config/... ./internal/svc/... ./internal/logic/clientplayerlogin/... -count=1
cd go/player_locator && go build ./... && go test ./internal/logic/... -count=1
cd go/scene_manager && go build ./... && go test ./internal/logic/... -count=1
cd robot && go vet ./... && go build ./...
```

- 真 Redis:`LOGIN_TOKEN_REQUIRE_REAL_REDIS=1 LOGIN_TOKEN_TEST_REDIS_ADDR=<数值地址> LOGIN_TOKEN_TEST_REDIS_DB=9 go test ./internal/logic/pkg/token/ -run TestTokenFamily_RealRedis -count=1 -v`,7.2 与 8.x 各一轮。
- C++(串行 `/m:1`):编 `gate`、`scene`、`routing_identity_test`、`cross_zone_test`。跑 `routing_identity_test --gtest_filter=SessionDetailsBuilder.*`、`cross_zone_test --gtest_filter=EnterFenceEcs.*`。
- Java 网关:`HotfixCheckServiceTest` 和网关全量单测。
- 部署契约:`k8s_deploy_contract`、`k8s_redis_split_contract`、`redis_exporter_contract`、`k8s_client_entry_contract`。`Get-GrpcClientMethodsNotCalledFromCpp` 现在登记了 GM 回收三个方法。`k8s_redis_split_contract` 第 8 节是新写的,最可能因夹具红的是 prod 档和 all-up 两条 DryRun。

player_locator 用例的假存活集多放了一个没登记玩家的活 gate(`fakeClusterView.LiveGateInstances`),因为闸门不接受全空。

11-01 那一轮 Codex 跑在这批改动之前,§14 的结果:`data_service`、`login` 编译和 vet 通过,摘孤儿测试通过;`TestRecallStore_*` 全部卡在夹具「缺少fixture物理authority原件摘要」,不是回收逻辑的断言;C++ 因依赖和生成版本不一致没编起来,`AssetOpRecallTest` 等都没跑。

Codex 同时要做客户端(`../mmorpg-client`):
- 帮会捐献和兑换恢复记录的两个出口,参考补丁 `docs/handoff/guild-economy-recovery-client-20261023.patch`,说明在同目录的交接文档。
- RefreshToken 结果未知时,只在宽限窗口内拿原 token 重试;收到拒绝或超预算就丢掉旧 refresh,绝不在窗口外再发它,否则会吊销整次登录。

没编译、没运行。只做过 `gofmt`、PowerShell 语法解析、YAML 解析。

**下一步先做的**:清单第 6、1 条和第 2 条的终结工具已落码(未编译),见下面「清单第 1、2、6 条」。接着做第 2 条的 Manual 合同恢复入口,然后第 3~5 条。

### 清单第 1、2、6 条(2026-11-01,未编译)

都在仓库根下,串行。第 6 条要先重新生成协议:proto 删了 `player_rollback.proto`,`message_id.txt` 的 12 个号已手写墓碑,所以不用设 `PROTOGEN_ALLOW_MASS_MESSAGE_TOMBSTONE`。

```bash
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml
```

```bash
cd tools/proto_generator/protogen && go test ./internal/... -run TestClientRpcKinds -count=1
```

```bash
cd go/scene_manager && go build -gcflags=-e ./... && go vet ./internal/... && go test -count=1 ./internal/logic/... ./internal/svc/...
```

```bash
cd go/trade && go build -gcflags=-e ./... && go vet ./cmd/... ./internal/data/... && go test -count=1 ./cmd/assetopfix/...
```

C++(串行 `/m:1`):编 `scene`、`gate`、`currency_test`,跑 `currency_test --gtest_filter=ClientGmGateTest.*`。

通过标准:

- 生成后 `go/proto/scene/player_rollback*.pb.go`、`cpp/generated/proto/scene/player_rollback.pb.*`、`robot/vendor/proto/scene/player_rollback*.pb.go` 都不在,`proto/message_id.txt` 没有新增活行。
- `cpp/generated/rpc/service_metadata/player_rollback_service_metadata.h` 会留着,是已知残留,不用手删,回执写一句;Claude 下一轮删。
- `scene_claim_atomic_test.go` 全部 PASS,重点看 `cjson` 在 miniredis 下的往返。既有的 `TestAutoscale_*`、`TestOrphanCleanup_*`、`TestReconcileDeadNode_*`、`TestAtomicDestroy*`、`TestLocateWorldChannel_*`、`scene_keys_contract_test.go` 不能变红。
- `go/trade/cmd/assetopfix` 全部 PASS。`GetOp`、`ListStuck` 只有单测的假实现覆盖,真库演练见 `docs/ops/trade-commerce-quarantine-runbook.md` §4.2。

失败时保留:编译全部错误;失败用例 `go test -count=1 -v -run '^用例名$' <包>` 的完整输出。不改断言。

### 本聊天后续 A9 实际结果（2026-11-01，交原窗口处理）

新PD世代真实Raw及SELECT_ONLY/LOCK_TOUCH资格均已通过；随后A9默认clean install真实退出1：711项=573通过+3失败+1错误+134条件跳过，617输入前后相同。game-discovery三项是观战协调键新增四金样遗漏/测试CRC16未取hash-tag；game-gateway-store的真实fixture在publish初始化SELECT DISTINCT zone_id FROM player超时，回滚通讯失败留下UNKNOWN，尚未进入失败轮业务竞争，根因未定。mall、gateway依赖跳过，运行三包本轮0个。详情：[本轮实际结果](../../../build/verification/stage4-resume-20261101-r1/java-a9-r3/result.json)、[失败诊断](../../../build/verification/stage4-resume-20261101-r1/java-a9-failures-diagnosis.json)。跳过清单、新XML及完整日志同结果目录，未改断言/未重跑。当前source/测试配套修正仍归原窗口；UNKNOWN私有原件与现场保持，不自动清理，不恢复第5旧资源核账。原R网关另有缺本仓authority-jdbc依赖的前置，未进入编译/测试：[R网关前置诊断](../../../build/verification/stage4-resume-20261101-r1/r-java-gateway-failure-diagnosis.json)。

## §18 双区写者启动接线(2026-11-01,未运行)

全部未运行、未提交。需求原文:`build/verification/stage4-resume-20261101-r1/multi-zone-boot-repair-plan.json`。

### 18.1 已改

- `lib/release_common.ps1` 新增 `Get-InstanceConfigZoneId`:只接受唯一出现的 `ZoneId` 或 `Node.ZoneId`,缺失或两处都有就拒绝。`go_services.ps1` 的计划和启动都用它,login、guild 不再因读不到顶层 `ZoneId` 拒启。
- `go_services.ps1` 新增 `ConvertTo-PlannedRoleFenceConfig`、`Update-PlannedRoleFenceConfigs`:`-Zone` 大于 0 时,`plan-instances` 在全部实例端口定下后,按配置自己的 `Participants` 把直连端口改成同区计划的实际端口。缺参与者、多副本、一个参与者多个地址都拒绝,Etcd 形式保持不动。`-Zone 0` 不绑,行为不变,所以单区端口被 Windows 保留区挪走时,协调者仍连旧端口,这个旧问题没动。
- `go_services.ps1` 新增 `-ZonePortOffset`(缺省 -1 = 原公式)和 `Get-ZonePortOffset`,`Get-InstancePort`、`Resolve-InstanceConfig` 共用。决定加它的依据:仓库跟踪文件里没有能产出 990017/990018 全部实例配置的 producer;`build/asset-arrival-20261017/fullstack/` 下只有 2026-10-17 的历史产物,只覆盖 db、login,早于写者目录,不能复用。
- `lib/sql_boot_catalog_local.ps1`:`New-LocalSqlBootCatalog` 新增 `-Zones`、`-Counts`、`-PortStride`、`-ZonePortShift`、`-ZonePortOffsets`。多区时内部逐区调 `plan-instances`(`Get-LocalSqlBootMultiZonePlan`),合并后先过 `Assert-LocalSqlBootMergedPlan`(实例键、配置路径、`ListenOn`、区号、监听和 metrics 端口、账号名碰撞),通过了才建库。白名单必须来自 `DB_ALLOWED_DATABASES` 或 `MMORPG_DB_ALLOWED_DATABASES`,每个目标区的 `zone_<区>_db` 都要在内。存档库迁移每个都带 `-storage-id`。任何服务建表失败都不发布。单区不带 `-Zones` 时流程不变,只多了一个拒绝:没有 `-Zones` 却给了 `-Counts`、`-PortStride` 或 `-ZonePortOffsets`。
- 目录发布改为先写 `catalog.candidate.json`,`Get-SqlWriterBootEnvironment` 自检通过才改名成 `catalog.json`,单区也走这条。
- `dev_tools.ps1`:`sql-boot-catalog-local` 透传 `-Zones`、`-GoCounts`、`-GoPortStride`、`-ZonePortShift`、`-ZonePortOffsets`;`go-svc-start`、`go-svc-start-exe` 透传 `-ZonePortOffset`。`dev-start-zones` 没改,仍按原公式。
- `tools/scripts/README.md` 原地改掉只支持单区的说法,写了多区用法,写明 Java 版的 4 个角色由 Java 仓的 producer 生成。

### 18.2 验证方法(全部由 Codex 执行,本窗口只做过语法解析)

合同测试,工作目录是仓库根:

```powershell
pwsh -NoProfile -File tools/scripts/tests/sql_boot_catalog_local.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/go_services_ports.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/go_services_local_mysql.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/docker_debug_sql_boot.tests.ps1
```

通过标准:每个脚本退出码 0,`fail=0`。后两个没有改,跑它们是确认 `Get-ZonePortOffset` 和 `New-LocalSqlBootSettings` 没带坏既有行为。

双区生成和启动,前置是当前源码的 `go-svc-build`(不带 `-GoServices`)和本地独立数据库。下面的命令用 MySQL 档;用 TiDB 时先设 `MMORPG_GLOBAL_AUTHORITY_TIDB_HOST`,并去掉 `-LocalMySQL`。

```powershell
$env:MMORPG_DB_ALLOWED_DATABASES='zone_990017_db,zone_990018_db'
& tools/scripts/dev_tools.ps1 -Command sql-boot-catalog-local -Zones 990017,990018 -ZonePortOffsets '990017=0,990018=1000'
$env:MMORPG_SQL_BOOT_CATALOG=(Resolve-Path run/secrets/sql-boot-local/catalog.json).Path
pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-start-exe -Zone 990017 -ZonePortOffset 0 -LocalMySQL
pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-start-exe -Zone 990018 -ZonePortOffset 1000 -LocalMySQL
```

通过标准:

- `catalog.json` 恰好 22 个条目,键是 `z990017_`、`z990018_` 加 data_service、db、login、trade、guild、match、mail、player_inbox、friend、chat、rank。
- `last-run.json` 的 `registered` 与这 22 个相同,`skipped` 为空,`zones` 是两个区。
- 每个 `z<区>_data_service` 的 `runtimeConfigFile` 里,`RoleFence` 六个参与者的端口等于同区计划里该服务的实际端口;`z<区>_login`、`z<区>_guild` 的 `Node.ZoneId` 等于目标区。
- 两次 `go-svc-start-exe` 都没有因 `ListenOn` 或 `ZoneId` 拒启,28 个 Go 进程在 LISTEN。LISTEN 和 ready 只算前置,不能据此宣称 Main36 业务通过;Java 版的 4 个角色要另走 Java 仓的 producer。
- 负例:不设白名单时拒绝且不起子进程;`-ZonePortOffsets 990017=0,990018=0` 在建库前因端口冲突拒绝。

失败时保留命令、退出码、完整日志和失败实例,不重试,不放宽闸门。

### 18.3 没做和已知风险

- 合同测试已补,未运行。`sql_boot_catalog_local.tests.ps1`:双区 22 个条目、一次账号清理、白名单缺失和少一个区、第二区失败不发布、账号碰撞和两区端口冲突在 DDL 前拒绝、单区兼容、`dev_tools` 透传。`go_services_ports.tests.ps1`:`Node.ZoneId`、两处区号拒绝、启动时区号和端口不一致拒绝、端口偏移、`RoleFence` 跟实际端口、`plan-instances` 缺参与者拒绝。两区端口冲突放在生成器测试里,因为检查在合并校验,不在 `go_services`。
- 测试没覆盖:合并计划里有 `Running` 实例的路径、`Etcd` 形式走完整 `plan-instances`、真实 `pwsh` 子进程调 `plan-instances`(生成器测试走进程桩)。真实路径只能靠 18.2 的双区生成验证。
- `go_services_ports.tests.ps1` 的 AST 检查会看到两个 `Get-SqlWriterBootEnvironment`:其中一个是用例内的局部遮蔽,是有意的,不影响顶层加载。
- `pwsh -File` 下 `-Zones 990017,990018` 能否绑定到 `[int[]]` 没有验证,所以生成命令写成会话内 `&` 调用。`docs/ops` 里有 `pwsh -File ... -Zones 1,2` 的写法,同样未核对。
- Java 版没有改,这个工具也不生成它的 4 个角色;Java 仓的 producer 是否已补齐没有核对。
- `-Zone 0` 没有回填 `RoleFence`:单区计划常只点名部分服务,回填会改变单区行为,所以单区端口被 Windows 保留区挪走时,协调者仍连旧端口。
- 计划按服务清单顺序解析端口,启动按 Tier 顺序。两个服务都被挪出保留区时,两边可能得到不同端口;这时启动会因 `ListenOn` 不符拒绝,不会连错,重跑或调整偏移即可。
- 计划逐区独立解析,第二区不知道第一区挪动后的端口,冲突由合并校验拒绝,不会自动避让。
- 多副本账号名可能截断碰撞(如 `player_inbox=2`),目前是拒绝,没有放宽账号命名。

## §17 SQL 路径故障控制器(2026-11-01,未编译)

**状态:没有编译、没有运行。** 只跑过 `gofmt -l`(无差异)和 PowerShell `ParseFile`(0 错误)。由 Codex 编译验证。

### 改了什么

- 新增 `go/data_service/internal/faultdrill/ownership_fault_fullstack_test.go`,包里只有测试文件。入口 `TestFullstackOwnershipFaults`,子测试 `commit_blocked_handoff_waits`、`stale_scene_resume_write_rejected`。不设 `OWNERSHIP_FAULT_REQUIRE=1` 时跳过,普通 `go test` 不受影响。
- `tools/scripts/tests/run_ownership_fault_fullstack.ps1`:删掉 `-DbTestBinary`、`-RobotTestBinary`,改成自己从当前源码 `go test -c` 编进 RunDir,旧二进制进不来;编完用 `-test.list` 核目标测试在二进制里(零用例也会退出 0);周期读回执不再要求 30 秒;截止按周期算;结束时查 stdout 里的 `=== RUN`、`--- PASS`、两个子测试和 SKIP,再查三份场景证据。进程句柄身份校验、独立恢复看门狗、finally 恢复都没动。编译失败发生在 `try` 之前,没有进程被碰,也不写 `runner-result.json`,看 `*.build.stderr.log`。
- `robot/ownership_fault_fullstack_test.go`:截止从写死 390 秒改读 `OWNERSHIP_FAULT_ROBOT_BUDGET_SECONDS`,其余不变。

### 构建命令

runner 会自己编。想先单独验证编译:

```powershell
$env:GOTOOLCHAIN='local'; $env:GOPROXY='off'
# 工作目录 go/data_service
go vet ./internal/faultdrill
go test -c -o <任一 build 下临时路径>/controller.test.exe ./internal/faultdrill
# 工作目录 robot
go test -c -o <任一 build 下临时路径>/robot.test.exe .
```

### 运行命令

```powershell
pwsh -NoProfile -File tools/scripts/tests/run_ownership_fault_fullstack.ps1 `
  -RunDir <repo>/build/...(已建好的空目录) -RunId <32位小写hex> `
  -SourceReceipt <scene 回执> -RunStartedUtc <本轮开始时间> `
  -RobotConfig <无凭据机器人配置> -DelayAccount robot_assetarrival_fault_<a> -ZombieAccount robot_assetarrival_fault_<b>
```

### 环境变量

由启动者注入,不写进文件:

| 变量 | 内容 |
|---|---|
| `OWNERSHIP_FAULT_PASSWORD` | 机器人账号密码 |
| `OWNERSHIP_FAULT_PLAYER_DSN` | 库名必须是 `zone_990017_db`、tcp、回环地址。专用本机 dev 观测账号,要有 `player_store` 的 `SELECT`、`SELECT ... FOR UPDATE` 所需权限,以及全局 `PROCESS`。不要改生产账号 |
| `OWNERSHIP_FAULT_SM_CONFIG` | 本轮 z990017 实际 scene_manager 配置的绝对路径。etcd、Redis 控制域(`Redis` 段)、玩家域(`PlayerRedis`,没写 `Addrs` 时指向控制域实例,单实例和 Cluster 都按配置走)、再入屏障都从这里读。地址全部要求回环 |
| `OWNERSHIP_FAULT_REENTRY_BARRIER_SECONDS` | 仅当上面配置没写 `SceneReentryBarrierSeconds` 时必填,填实际生效值(当前内部常量 `constants.SceneReentryBarrier` 是 20)。两处都没有就失败,不补缺省 |

由 runner 设置,不用手填:`OWNERSHIP_FAULT_REQUIRE/MODE/DIR/RUN/CONFIG/ROLE/ACCOUNT`、`OWNERSHIP_FAULT_SOURCE_SAVE_INTERVAL_SECONDS`、`OWNERSHIP_FAULT_SOURCE_NODE_UUID`、`OWNERSHIP_FAULT_ROBOT_BUDGET_SECONDS`。

### 真实运行前置

1. 990017、990018 两区真实进程已起,TiDB、Redis、etcd 在本机。依赖另一窗口的多区启动接线(`multi-zone-boot-repair-plan.json`)。
2. source scene 回执新增必填 **`node_uuid`**(本轮实际值,可从该进程的 etcd 注册取);`source_save_interval_seconds` 必填,1~120 的整数,缺了就拒绝,不假定 60。其余字段不变:`service=scene`、`zone=990017`、`fault_run=RunId`、`pid`、`binary`、`binary_sha256`、`cwd`、`started_utc`、`process_creation_filetime_utc`。scene 二进制要是 build 下的显式副本,日志在 `<cwd>/logs/cpp_nodes`。
3. 账号全新且两个不同;机器人配置 `TravelSmoke.HomeZone=990017`、`VisitZone=990018`、密码认证、网关回环。
4. 可用内存不少于 2 GiB。

### 通过标准

`runner-result.json` 的 `actual_pass=true`,且 `scene_still_suspended=false`、`cleanup_errors` 为空。runner 判定它之前已经核过:控制器和 3 个机器人进程退出 0;stdout 实际进入目标测试、两个子测试都 PASS、没有 SKIP;`save-blocked-evidence`、`delay-proof`、`zombie-proof`、`old-write-evidence` 都属于本轮。

- **场景 1 证据**:控制器在本轮玩家 kind=2 行持 `FOR UPDATE`(上限 8 秒,短于 go/db 行锁等待 10 秒),取 `@@tidb_current_ts`。`DATA_LOCK_WAITS` 里 `CURRENT_HOLDING_TRX_ID` 等于这个事务、`KEY_INFO` 是本库 `player_store` 的 `(player_id, 2)`,才开始负向观察。只有 Save 的批量写会锁这一行,所以等待方是一笔带资产行的真实 Save。观察 4 轮,每轮要求:仍在等锁、资产行字节和金额是基线、写权行 epoch、Redis owner、location 不变、handoff 标记不存在。释放后 SQL 金额为基线 +18,背包和资产账本不变。证据写在 `save-blocked-evidence.json`(两个事务号和等待键)。
- **场景 2 证据**:写权行 `holder_instance`、etcd 注册的 `nodeUuid`、回执 `node_uuid` 三者相同;等真实周期存盘把基线写进 SQL(截止 = 回执周期 + 15 秒);+11 只在内存,暂停后全部行摘要不变;etcd 注册自然消失且 `death_at` 过屏障;目标设备新登录代次更高、位置在 990018、SQL 写权行 epoch 和 holder 已换、金额为基线 +7;恢复后在本轮新增日志里匹配到 `HandlePlayerSaveRejected: owner_epoch CAS rejected save for player <id> key=... epoch=<旧 epoch>`(截止 = 周期 + 15 秒);之后连续 3 秒全部行摘要不变。

没有 `DATA_LOCK_WAITS` 权限、等待键不符、窗口没命中都是失败,不降级。

### 预算

控制器总预算 = 360 + 2×周期 秒(默认周期 60 时 480 秒);runner 循环 +10、`-test.timeout` +20、机器人 -30,runner 与控制器各写一份同公式,注释里标了。暂停上限和看门狗 240 秒没变,暂停时长不依赖存盘周期。

### 没做 / 未核实

- **没有编译和运行**,类型只靠逐行核对。`shared/ownerepoch`、`proto/player_locator` 是 data_service 第一次直接引用,`go.sum` 如缺条目会在 `go test -c` 报出,不要改 `go.sum` 绕过,报回来。
- `KEY_INFO` 的 `handle_value` 形如 `{id, kind}` 是按 TiDB 实现推断的,没在真库看过。形状不符时控制器立刻失败并打印原文,照原文改 `ownershipFaultKeyMatches`。
- 假定两区共用同一套 Redis 和 etcd,且 `player:session` 在玩家域(配置来自 z990017)。不成立会明确失败,不会误判通过。
- 没核 C++ `CallDeadlineMs` 与 8 秒的关系。短于 8 秒只会让 scene 按结果未知用同序号重试,交接照样等,不影响结论,但日志里会多出重试告警。
- `docs/ops/mysql-backup-pitr-runbook.md` 468~474 行还写着 30 秒周期和第二表锁,没改。
- Java 版不涉及:这是本仓测试工具,不改客户端契约。

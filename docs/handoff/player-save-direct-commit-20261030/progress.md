# 玩家存储水平扩展:落码进度与验证清单

新窗口接手时先读这份,再读 [总体方案](../../design/player-data-tidb-scaling-plan.md) 和 [存盘规格](../../design/player-save-direct-commit.md)。

**全部改动都没有编译。** A7、B1~B7 提交在分支 `claude/player-store-direct-commit`(基于 `720179638`,单独编译不了);A7c 之后的全部改动只在主工作区,未提交,还混着别的会话的未提交改动。验证要在工作区做。

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
- B4(主流程接入,规格 §5.3):`PlayerStorePath` 开关(`config.proto`、`engine/config/player_store_path.h`、`config.cpp`、`base_deploy_config.yaml`、`k8s_deploy.ps1` 镜像并核对与 go/db 相同、`start_game.ps1` 启动前核对);`PlayerStoreClient` 补 `SubmitOptions`、被取代的快照、代次落后、`ReleaseDetached`;`player_lifecycle` 的 `SavePlayerDirect`、`OnPlayerStoreSaveDone`、`HandlePlayerSaveRefused`、`DisposeRefusedSave`、`RequestKeyProgressSave`、`LogUnsettledPlayerStoreSaves`;`handoff_sql_barrier.h` 的 `kPublishMarkDirectLua`;重连与退出被取代时清 `released`;会话结束凭证先释放;启动校验;定时存盘 60 秒;任务、等级接关键进度;go/db 接受不带落点的空行释放。
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
- **「死锁被重试吸收」的说法不成立**:写者闸门把 1213 变成不可重试的结果未知,注释和方案 §8.1 已改正,写进残余。

## 给 Codex 的总验证清单(2026-10-30,替换旧清单)

工作目录除注明外都是仓库根目录。按顺序跑,一步失败先报告,不要改断言、不要跳测试、不要连续自动重试。C++ MSBuild 必须串行 `/m:1`。失败时保留:命令输出、`go test -v` 的失败用例、C++ 编译错误前 50 行。跳过的用例按没跑报告。

**工作区**:改动都在主工作区,未提交。`claude/player-store-direct-commit` 只有 B1~B7 和 A7。B8 之前的快照:`refs/snapshots/before-b8`;写完时的工作区快照:`refs/snapshots/phase-a-written`(都是 tree,`git show <ref>:<路径>` 取回)。工作区里混着别的会话的未提交改动,不要整理、不要回退。

**前置清理**:项目没上线,所有库清掉重建,不写迁移。主键变了的表:`match_rank_source`、`match_rank_outbox`、`rank_input_receipt`;`rank_score` 多一列。不再建的表:`player_database`、`player_database_1`、`id_segment`、`zone_merge_fence`、`save_barrier_maintenance_receipt`、`account_role_admission_recovery`。按 AGENTS §6.2 第 6 条删掉 redis、mysql、etcd 数据。

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
- 旧生成物要没有:`cpp/generated/proto/db/db_task.pb.*`、`go/proto/db/db_task.pb.go`、`robot/vendor/proto/db/db_task.pb.go`。**生成器对没有 proto 的产物只跳过、不删,工程清单也只增不删**(`tools/proto_generator/protogen/internal/prototools/cpp_build_projects.go`),所以这里是手工删的一个例外:删这几个文件,并从 `cpp/generated/proto/CMakeLists.txt`、`proto.vcxproj`、`proto.vcxproj.filters` 删掉 `db/db_task.pb.*` 的条目。只删文件不改清单,proto 工程找不到源文件。
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

通过标准:全绿。这一步预计会有编译错误,全部改动没有编译过。修编译错误不算改断言;如果断言和新行为冲突,保留输出报告。

### 4. 真库

自起一次性 MySQL 和 TiDB,别借 `codex-*` 容器。`go test` 报 Access denied 时改用 `go test -c` 再运行。真 Redis 用例 7.2 和 8.x 各跑一轮,结果记版本。

| 模块 | 环境变量 | 命令 |
|---|---|---|
| go/db | `GODB_MYSQL_ITEST_DSN`、`GODB_IT_REQUIRE=1`(MySQL、TiDB 各一遍) | `go test -tags=integration -race ./internal/playerstore/...` |
| go/data_service | `ACCOUNT_DIRECTORY_TEST_DSN`、`ACCOUNT_DIRECTORY_REQUIRE_TIDB=1` | `go test ./internal/store/... -run 'TestTiDB'` |
| go/data_service | 见 `internal/store/storetest/mysql.go` 包注释 | `go test -tags=integration ./internal/store/... ./internal/logic/... ./internal/kafka/...` |
| go/chat | `CHAT_IT_TIDB` | `go test ./internal/authority -run TestChatTiDB` |
| go/match | `MATCH_IT_TIDB` | `go test ./internal/authority -run TestRatingTiDB` |
| go/match | `RANK_MAIL_CHAIN_IT_TIDB` | `go test ./internal/authority -run TestTiDBRatingRankMailChainRecoversAfterCommittedReplyLoss` |
| go/rank、go/mail | `MAIL_RANK_TEST_TIDB` | `go test ./... -run TestTiDB` |
| go/guild | `GUILD_TEST_MYSQL_DSN` | `go test ./internal/data -run 'TestGuildLockOrder\|TestGuildLockingStatement' -count=3`,以及 `./internal/logic` 的 economy flow |
| go/trade | `TRADE_TEST_MYSQL_DSN` | `go test -tags integration ./internal/data -run 'TestLegacyEnsureSeqRow\|TestEnsureSeqRows\|TestFirstTimeCreation\|TestSeqRowProbe'` |
| go/player_inbox | `PLAYER_INBOX_TEST_MYSQL_DSN` | `go test -tags integration ./internal/inbox/...` |

重点用例:`TestTiDBRoleTradeDeadHolderIsFencedWithoutAck`、`TestTiDBRoleTradeCurrentHolderBlocksUntilReleased`、`TestTiDBRoleTradeReleasedCurrentHolderStillWaitsForAck`(崩溃持有者)、`TestRatingTiDBRoundBudgetExhaustedStopsWithoutError`、`TestRatingTiDBCursorAheadFromOtherReplicaIsNotAFailure`、`TestRatingTiDBAchievedMsFollowsScoreChangeNotGames`、`TestTiDBVersionedAchievedMsComesFromProducer`(投榜)、`TestTiDBReleaseDeadPlayerStoreHolder`、`TestTiDBCreateRoleOnPlayerStoreWritesOnlyTheNewRows`、`TestTiDBReadPlayerDisplays`、`TestSnapshotStore_RowIDCollisionIsNotSwallowed`。

### 5. merge_zone

工作目录 `tools/merge_zone`,按序:

```bash
go vet ./... && go vet -tags merge_integration ./... && go test -count=1 ./...
```

```bash
MERGE_ZONE_IT_REQUIRE=1 MERGE_ZONE_IT_SQL_DOMAIN_ONLY=1 MERGE_ZONE_IT_TIDB_DSN=<本机TiDB基址> go test -tags merge_integration -count=1 -run TestIT_SQLDomain ./...
```

全量:再加 `MERGE_ZONE_IT_REDIS_ADDR`,去掉 `MERGE_ZONE_IT_SQL_DOMAIN_ONLY` 和 `-run`。真二进制 `-apply` 没有用例,如实报告没覆盖。

### 6. C++

```bash
msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64
```

期望产物:scene、gate、battle 节点和全部测试 exe。跑 `cross_zone_test`、`currency_test`、`bag_test`、`kafka_command_test`、`rpc_controller_test`、`turn_battle_engine_test`、`player_landing_test`。`currency_test`、`bag_test` 里的真 Redis 用例要配专用 `MMORPG_TEST_REDIS_ADDR`,没配会跳过,要配上跑一次(7.2 和 8.x 各一轮)。

重点:`PlayerStoreDirectEcs.*`(含这轮新增的 `CommitSubmittedBeforeTradeFreezeIsNotDrainEvidence`、`SaveDuringBattleDoesNotReplaceTradeFreeze`)、`PlayerStoreClientTest.*`、`PlayerStoreProjectionTest.*`、`RoleAdmissionTest.*`、`GuidSegmentClientTest.*`、`KafkaPartitionerContract.*`。`git show refs/snapshots/before-b8:<路径>` 能取回被删的旧测试,对照用,不要恢复。

### 7. 部署合同测试

`tools/scripts/tests/` 下 40 个 `*.tests.ps1` 全部跑,一个失败不中断其余的:

```bash
pwsh -NoProfile -Command "$failed=@(); Get-ChildItem tools/scripts/tests/*.tests.ps1 | ForEach-Object { pwsh -NoProfile -File $_.FullName; if ($LASTEXITCODE -ne 0) { $failed += $_.Name } }; $failed; exit $failed.Count"
```

重点:`k8s_deploy_contract`(新增存盘告警、`-DatabaseBackend` 两条)、`lobby_authority_render`、`global_authority_deploy`、`chat_authority_deploy`、`go_services_local_mysql`、`id_issuer_deploy_contract`、`etcd_maintenance_contract`、`dev_tools_merge_zone_contract`。

### 8. etcd

按 `docs/ops/etcd-maintenance-runbook.md`:M0 取镜像摘要,`etcd.yaml`、`etcd-maintenance.yaml`、compose 三处同批钉(`bitnamilegacy/etcd:latest` 现在没钉);M3;探针 A;P0-D 实跑。没有环境就如实报告没跑。首次部署之前必须跑完。

### 9. Java 网关(A7c)

`GATEWAY_TEST_MYSQL_URL` 指向库名以 `_it` 或 `_test` 结尾的专用 MySQL,设 `GATEWAY_REQUIRE_MYSQL_TESTS=1`,跳过即失败:

```bash
cd java/gateway_node && mvn test -Dtest='RowIdsTest,GatewaySchemaContractTest,ZoneWhitelistRepositoryWiringTest,ZoneWhitelistUpsertLockOrderMySqlTest,AdminWhitelistControllerTest'
```

`ZoneWhitelistUpsertLockOrderMySqlTest#upsertBehindPendingDeleteOnlyQueues` 失败时不要改断言,按 `ZoneWhitelistRepository` 的类注释判断,主键方案要重新定。

### 10. 全链路冒烟

清库后本地起全套(`go_services.ps1` 给 go/db 设 `DB_AUTO_MIGRATE_SCHEMA=1`,缺 `player_store` 就拒启)。K8s 上要先预置 `domain-service-secrets` 的 `player-store-scene`(至少 32 字节);按表授权的环境把 `player_store` 加进授权计划,match 的账号加 `zone_merged_into` 的 SELECT。

场景:建角、进场、存盘、下线、重连、跨节点交接、交易冻结(含从未进场的角色直接上架)、崩溃持有者(`admission_recover` plan 和 apply)、结算、收件箱、自动放弃、判死(含节点崩溃后重登和 LeaveScene 重试撞在一起)、发号(etcd 里有 `/id_issuer/counter/primary` 和 `secondary`,玩家 ID 大于 2^53,Lua 和 Java 网关正常)、聊天发送、榜单(单局、封榜)、别名合服(`merge-zone-audit`、`merge-zone -DryRun`、`-VerifyMerged`)。

mysql 档按总体方案 §9.4 的 7 项单独跑一轮。

### 11. 压测(阶段 B)

按 AGENTS §6 全部流程:先存 `prev-summary.txt`,清库,跑完用 `stress_summarize.ps1` 出对比表。这是新路径第一次压测,旧基线的存盘段全部不可比,要在复盘里写明。看:TiDB 写入与事务数、PD 时间戳、冷读和热加载 P99、`db_player_store_seconds`、`entergame_total{result="pool_saturated"}`,以及总体方案 §8.1 的四类热点是否消失。没有对比表不要声明性能结论。

## 没写的

- 阶段 B(压测)由 Codex 跑;阶段 C(拆跨服务同事务耦合、目录跨集群、多集群、分区搬迁、多地域、SLG 拆图)是上线后的事。
- 不停服合服:`docs/ops/merge-zone-runbook.md` 开头写着这个目标,这一轮没做。别名合服方向一致,但工具仍要求源区先下线。
- 映射直读 TiDB(映射提案 §15):未落码。

## 已知残余

- **战斗结算原件还在 Redis**(`battle:settlement:pending:*`),和「玩家数据不进 Redis」的终态不符。读代码 [I]:每局结果都带 `settlement_format_version=1` 提交 match SQL 并进收件箱,同时仍写 Redis 待结算记录,scene 按 battle_id 幂等,两条路径并存。删 Redis 这条要单独一批。
- 线上拉数工具 `debug_fetch`、`debug_import` 已删,要按 `player_store` 重写才有。
- 回档 RPC 已删,恢复玩家数据只走 PITR(`docs/ops/mysql-backup-pitr-runbook.md` §4.2)。
- 函数名还带 Redis:`SavePlayerToRedis`、`HandlePlayerAsyncSaved`。`RequireSQLCommit` 字段只剩单测能关。
- `restore.go`、`maintenance_drain.ps1` 还在扫已退役的 `kafka:retry:*`、`player:save_pending:*`,恒空,改动风险大于收益。
- `merge_zone` 过程中出现过可恢复错误时,打完 Done 仍退出 1,要用同一清单重跑。撤销窗口里跑 `route_tool reconcile-from-cache` 可能把已撤销的映射写回库。人民币寄售的 `market_zone` 不改写,Java 商城是否按区过滤没有核对。
- 冻结存盘被拒(TOO_LARGE、SCHEMA_REGRESSED)时交易停住,不发 AckDrain,要运维摘掉。过户之后迟到的 Close 带旧修订,scene 每秒重发一次直到玩家再进这个实例或进程重启,只是空转。
- 协议里留着恒默认值的字段:`post_merge_notice_ts`、`force_rename_required`、`fence_*`、`merge_state`。`ImportRole` 没有生产调用。
- 跨集群搬迁没有重写。别名映射的「通知加版本号」没做,只靠定时重读(默认 5 秒)。
- `dev_tools.ps1` 不转发 `merge_zone` 的维护证据旗标,不带 `-DryRun` 会被工具拒绝。
- `k8s_zone_rollback.ps1` 删了 `-BarrierProtocol`,`-Apply` 只靠工具未落地和预检拦。
- `stress_round19.ps1`、`currency_crash_window.ps1` 的 `$GlobalDbName` 默认改成了 `mmorpg_global`。
- mysql 档:历练结果 topic 的 RF3 门禁不放开,省钱档也要 3 个 Kafka broker;写者门把 1213、1205 变成不可重试的结果未知,MySQL 上更常见,压测后再定(总体方案 §9.3)。
- `data_service` 的 `serverIsTiDB` 只看 `VERSION()`,TiDB 配了 `server-version` 时判成 MySQL(原有问题,§9.2)。
- 聊天:角色挂牌或过户后,旧会话最多再发和拉 `ActorVerifyTTLSeconds`(默认 30)秒;限流从精确变成近似。
- 榜单:投递途中榜分不保证全局顺序,只保证每人最终落在最新版本。
- MySQL 档的 guild、trade、mail 路径,从没有死锁变成偶发失败:写者闸门把 1213 变成不可重试的结果未知,`retryOnDeadlock`、`WithTxRetry` 认不出,事务已回滚,玩家重试。测试用裸 `sql.Open` 时重跑会绿,生产路径没有被测到。
- 榜单 `RemoveRankEntry` 删行后版本丢失,迟到的旧批次能把人带回榜上(仓内没有生产调用方)。`StopRankSource`、`FreezeRankGeneration` 也没有生产调用方,「停写 → 投完 → 封榜」的编排要运营工具来做。

## 待用户确认

- 第二套 etcd(多一个 3 节点小集群,总体方案 §12 第 9 项)。代码已按要做写好。
- ID 值只用低 63 位(默认按此写,发第一个正式号之前还能改)。
- 映射提案 §15.9 的 (a)(b)(c)。
- mysql 省钱档要不要放开历练结果 topic 的 RF3 门禁(现在生产 mysql 档仍要 3 个 Kafka broker)。
- `docs/ops/merge-zone-runbook.md` 开头写的不停服合服要不要接着做。

## 欠的评审

多集群部分(总体方案 §7、§8.2)的正确性评审,落码前补。

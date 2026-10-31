# 整 zone 回档清理工具 zone_rollback_discard 设计（R9）

> **已过期(2026-10-30)**:玩家存盘改成 scene 直接提交 TiDB 的 `player_store`([player-save-direct-commit.md](./player-save-direct-commit.md)),这个工具依赖的 Kafka 存盘位点、Redis 父快照都不存在了,设计作废。本文只作历史保留,不再描述现行行为。

> **状态**:设计 v3(2026-10-19)。v2 已并入两面对抗复核 28 条(§6.1、§6.2,原文见附录 A)与「维护围栏」「三个小项」两路代码核实(落点见附录 B);v3 先按「上线前范围」收窄(§6.3,删掉的内容连同补做的触发条件移入 §8「上线后再补」),再逐条处理复查轮 11 条意见(§6.4)。**未落码**。
> **规模**:收窄并处理复查意见之后,总文件数 **109**(新增 58、修改 51、删除 0;另有 Java 仓 `PARITY.md` 1 个不计入本仓),比 v2 的 125 少 16,比负责人 2026-10-19 授权的 73 多 36。减少:merge_zone 围栏配套整组移出(−10)、guildcheck 改为 shared 实现加 data_service 薄转发包(−5)、去掉没有第二个使用方的 shared/playercache(−2)、拓扑清单样例内联进测试(−2);增加:P3 与 save_barrier_restore 共用 schema_gen 规则(+2)、混版本持久字段保护设计登记本工具这个写入方(+1)。批次仍为 B1→B6 九批。规模仍超授权,按 AGENTS §10.2 须负责人确认后再开 B1(§7)。
> 全文为静态阅读结论:没有编译、没有运行任何测试,待 Codex 验证。代码与文档冲突时以代码为准;引用行号以 2026-10-19 工作区(HEAD c63b22dca)为准,落码时按锚点文字定位。
>
> **适用范围(上线前收窄)**:负责人 2026-10-18 确认项目尚未上线(见 [db-write-fencing-mysql.md](db-write-fencing-mysql.md) 文末「上线状态」):没有存量 zone,没有在跑的旧版 go/db / scene / login,首次上线以「go/db dual + scene / login 严格提交屏障」整批起步。所以本工具**只支持 strict 协议**:legacy 在 ps1 入口与工具入口都拒绝;只为新旧版本并存、旧镜像、旧二进制服务的校验不做;防并发运维只保留回档自身的 fail-closed 复核,不改 merge_zone。删掉的内容与补做的触发条件见 §8。
>
> 上游:回档语义裁决与脚本侧落码见 [zone_data_rollback.md](zone_data_rollback.md)「2026-10-18 修订」;工具契约见 `tools/scripts/k8s_zone_rollback.ps1` 文件头「zone_rollback_discard 契约」(B3 同批改写);交接见 [godb-pipeline-impl-handoff-20261015.md](../handoff/godb-pipeline-impl-handoff-20261015.md) §7.4;存盘围栏见 [db-write-fencing-mysql.md](db-write-fencing-mysql.md)(v0.4);合服见 [server_merge_design.md](server_merge_design.md) 与 `tools/merge_zone`。
>
> 调研缺口(已补):v1 时「维护围栏」与「三个小项」两路专项调研因认证错误(403)未跑完;2026-10-19 已补跑两路只读代码核实,v1 中被证实的、需改的与漏掉的部分全部并入本文。

## 1. 决定

| # | 决定 | 一句话理由 |
|---|---|---|
| #1 维护围栏 | K1 = `placement.MergeFenceKey(z)`(`merge:in_progress:<z>`),值 `zone_rollback_discard:<rid>`,永久不设 TTL;K2 = `barrierrestore.MaintenanceKey(z)`(`player:save_barrier:maintenance:<z>`),值同为 `zone_rollback_discard:<rid>`,永久。K2 只用于与 save_barrier_restore 互斥,不算挡写手段(全仓只有 barrierrestore 读它)。工具新增 `fence`(第 0 步 g)、`release`(Step 6b)、`quarantine-drop` 三个模式。立栏后沉降 30s,按 `shared/scenenode` 的在线判据(Redis + etcd 注册表)有界排空他区在线的本 zone 玩家,不踢;超时或立栏后复核不过以 5 退出,围栏保留。**生效前提**(第 0 步 f、0g 前、stopped 前、inventory 前由 ps1 对全服复核配置):全部 data-service 的 `RouteStore.Mode=redis`(`ReadRequiresReady=false`);全部 scene-manager 的 `AllowGateZoneAsHomeZone=false` 且配了 `DataServiceRpc`;runbook 写明回档窗口内冻结全服 data-service / scene-manager 的发布与配置变更,并禁止运行 merge_zone 的合服、撤销、relocate、pin-placement、backfill-home-zone。镜像版本不核对:首发版本晚于 4493b3c9f,上线前不存在不填 `home_zone_merging` 的在跑镜像(§8 第 4 条)。**不挡的面**:他区 login 的 EnterGame 预加载冷读(任何版本 login 都不读 K1)、不带 GateId 的 EnterScene、没有 `player:zone` 映射的玩家、trade / mail / 收件箱对他区在线实体的资产投递、立栏瞬间的在途 EnterScene(靠沉降)。 | 所有读方只看键是否存在 / 值是否为 nil,不解析值,值改成 `zone_rollback_discard:<rid>` 既不误判也不放行(核实 1);早于 4493b3c9f 的镜像下 EnterScene 是 fail-open(核实需改 2),上线前所有可部署镜像都晚于它,镜像核对移到 §8,与版本无关的配置核对保留(db_first 回源与 dev 旁路照样让 K1 失效)。PITR 要几个小时,中间没有常驻进程续期,TTL 到期即 fail-open,所以用永久键;barrierrestore 的永久键 + 非 JSON 值只是**格式先例**,它的 Guard 对 `redis-same-instance` 直接拒绝(route.go:197),不能当同实例下行为已验证的证据(核实需改 3)。不踢:仓库没有按归属 zone 踢人的通道,踢下线产生的退出存盘本来就要丢弃。 |
| #2 纪元抬升与不可变计划 | 做。范围是 purge 处理的玩家(rollback / torn / post_t_candidate)里停服时 owner_epoch>0 的。`player:<id>:owner_epoch` 与当前世代 topic 下两张围栏表的 `consumer:applied_epoch:<topic>:<id>:player_database(_1)` 抬到 C'=owner_stop+1,键缺失也建;在每个玩家删除的同一段 Lua 里先抬后删,只升不降。U(p)、C'、V 与各键预期值只在 **inventory** 计算一次,写进不可变的逐人计划(`plan-s<NN>.ndjson`,SHA 记进清单);purge 与 verify 只判「当前状态 ∈ {计划前态, 计划后态}」,不再重算 U(p)。MySQL 行内围栏不抬。 | fencing token 的「撤销全部租约」:PITR 只退回了行围栏,C++ 父快照的 owner 等值比较与 go/db 的 guardOwnerEpoch 要同时抬,在途写才会判 stale。重算 U(p) 会把本工具自己写的 C' 当成外来证据,续跑与 verify 必然误判 owner_behind(复核 6.1.1)。 |
| #3 屏障协议 | **只支持 strict**:现网 `Fencing.Mode=dual`(deploy-facts)、`saveoutbox.GenerationKey(z)` 恰为 `strict`、本机 `go/db/etc/db.yaml` 的有效 `Fencing.Mode` 为 dual(Step 6 按工作树渲染 ConfigMap),任一不满足即第 0 步拒绝;legacy 的文案写明「上线后如需 legacy 回档再补(§8 第 1 条)」。purge 先把屏障归档后删除,P3 按 PITR 之后的 SQL 逐人重建屏障(三个版本字段加 `schema_gen`,规则与 save_barrier_restore 共用导出函数);「有 pending、无 ACK」的丢弃并逐人记报告,不重放。B2d 接线 P3 之前,工具对 release / quarantine-drop / `-version` 之外的模式一律以 3 拒绝,ps1 入口对 strict 的拒绝也到 B2d 才撤(复查 [1])。 | login 无开关地检查屏障(parent_commit_barrier.go:110-111),只删不建会全员拒登。Step 6 目标镜像的协议核对(复核 6.1.4)只防「回滚到不说严格屏障的镜像」,上线前不存在这种镜像,移到 §8 第 3 条。 |
| #4 blocked 与 T 之后建角 | 归类穷举,兜底 unclassified;新增 torn_cross_table。foreign_home_storage_here、legacy_owned、frozen 与在途搬库**在停服之前**(preflight、fence、stopped)就硬拒,inventory 仍按 blocked 兜底。归档收窄到证据:屏障、资产提交证据全量归档,父快照只归档带 pending / outbox 的;其余父快照只在 Lua 里算 `redis.sha1hex(GET)` 回传 40 字节,删除 Lua 复核,不用 DUMP。T 之后建的角色以空骨架继续玩;owner_stop>0 的 post_t_candidate 逐人列出并须确认文件放行。`-AcceptBlockedPlayers` 继续在入口拒绝。 | 这类玩家的写者不在本 zone 停服与围栏范围内,PITR 之后才发现时影子库已替换(复核 6.1.3、6.2.7);整 zone 父快照常驻无 TTL,全量 DUMP 等于整区导出(复核 6.2.2)。 |
| #5 有效回档点 | 接受「回退点比 T 早一个消费延迟」为语义 A 的 RPO,不重放。按 `fence_save_seq>>10`(存盘的**生产**时刻)逐行度量:晚于 T+60s 的行 >20 即 3,1–20 行须确认文件逐行放行。见证行 = 停服基线里最后存盘的生产时刻落在 (T−15m, T−Δ] 的行,Δ = `-witness-lag`(缺省 5m,上限 10m,写进确认模板):恢复后行围栏大于基线即 3(库不对);过半见证行不一致即 3(PITR 过早);少数不一致逐行进确认模板;(T−Δ, T] 的行只计数、不判。窗口里没有见证行须确认文件声明「T 前 15 分钟无可判存盘」。 | save_seq 不是时钟,单行离群不应卡死整区;只看上界发现不了 PITR 恢复过早(复核 6.1.6);生产之后还要一个消费延迟才提交进 MySQL,(T−lag, T] 里生产、T 之后才提交的行在正确的 PITR 下本来就是旧版本,零容忍会把正确的 PITR 判成过早(复查 [2])。 |
| #6 T 之后合入 / 迁入 | 证明不了早于 T 一律 fail-closed。读 `-topology-manifest-dir` 下所有 src 或 dst 含 Z 的合服 / 撤销 / 搬库清单:清单 `player_ids` 与 post_t_candidate 求交,命中即 moved_in_after_t;`merge:merged_into` 的 run_id 必须对上一份清单,否则 topology_unproven;合服开始时刻 ≥T 即 3,早于 T 要写步骤全部 Done 且 At≤T−1s;迁入本库要搬库清单 switched 且 At≤T−1s。 | copy 模式合入的玩家在键形状上与原住民无法区分,merged_into 被撤销或人工清理后只靠比例阈值会把他们静默清成空骨架(复核 6.1.5)。 |
| #7 全部冷读 | 放弃 unchanged 优化,范围内玩家一律删缓存、首登冷读。停服基线 = 全部玩家表的分块校验和(块边界由 stopped 自适应确定并写进基线,inventory 按同一边界重算,各算一次)+ 本 zone 归属玩家的 owner 基线。inventory 要求恢复库 schema 通过 go/db 启动期同一份校验(`migrate.ProtoSource.Drift` 为空),否则先在恢复库执行 `go run ./cmd/migrate -f <db 配置> -command up` 再重跑 4a。冷读洪峰没有专门节流,只在 Step 7 观察(§5)。 | 「行没变」不等于「缓存不晚于 T」;Step 6 的 go/db 在缺列时本来就拒启(proto_sql/db.go assertSchemaUpToDate),骨架行只能按完整 proto 写(复核 6.1.7)。 |
| #8 帮会资产分歧闸 | 新增。inventory 与 verify 对 purge 范围内全部玩家以 since=T_ms−300000 问 guild `GuildInternal.ListAppliedAssetOpsSince`(复用迁到 `go/shared/guildcheck` 的同一实现,与 data_service 回档闸同口径);工具走流式接口 `StreamDivergences`,分歧逐行写进 `asset-divergence.ndjson`,**不受**单人回档闸 10000 行上限约束(复查 [5]);有分歧或不可证明即 blocked,确认文件记该文件的 SHA-256、行数与按 kind 汇总,按 SHA 匹配才放行,文件本身作为补偿依据;查询失败按拒绝。trade 不在本批覆盖范围(§5、§7)。 | 仓库现行契约要求「任何回档在写玩家数据之前先问 guild」(rollback_logic.go:128-141);整区回档把钱包退回 T,guild 不回退,捐献类操作会凭空复制资产(复核 6.1.2)。 |
| (a) 死信去重 | 去重字段 = 原始 v2 头 12 字节 (seq 8B BE‖partition 4B BE) 的 24 位小写 hex,毒消息按 (Offset+1, Partition) 编成同一形状;值 = 跨副本稳定指纹。HASH `kafka:dead:index:<topic>`。纯函数 `kafka.DeadDedupField(entry)` 是唯一推导入口,七个死信写入点里五个经去重脚本(判重命中也做一次会传播的 HSET),forward / requeue 两个 Lua 分支计 undeduplicable。重复副本不计 `db_task_dead_letter_total`。死信工具与人工删除必须在同一段 Lua 里按指纹 HDEL。上界限定为「带 (原始分区, seq) 的条目,每条 Kafka 消息至多一份」。 | HSETNX 判重是 O(1);整载荷 SHA1 不可行(捕获时刻、错误串、RetryCount 各副本不同,核实 A7);判重命中不传播写会让 WAITAOF 在首份未 fsync 时提前确认(核实 A3)。 |
| (b) IsEqual 与探针 | 剥离。`PlayerLastPersistedSnapshotComp::Replace` 在 `clear_last_save_seq()` 之后,对**已存在**的子消息(`has_player_database_data()` / `has_player_database_1_data()`)各自 `clear_stress_test_probe()`;快路径仍用 `dirty_save::IsEqual`,不深拷贝。 | 开探针时快照侧带探针、新消息侧不带,快路径永不命中(核实 (b)3);`mutable_*()` 会在快照侧单边制造 presence,改变 MessageDifferencer 结果(核实 B3)。 |
| (c) op="other" 告警 | 新增 `DbTaskUnknownOp`:`sum by (namespace) (increase(db_task_producer_partition_total{op="other"}[10m])) > 0`,for 0m,warning,放进 `db-task.partition-contract` 组;`observeProducerPartition` 对 op=other 不论 verdict 都限量打一条带 topic / partition / offset / kafkaKey / taskID / msgType 的日志。 | op 是封闭集合,other 即未知生产者或载荷损坏;现有日志定位不到生产者(核实 C1)。 |

## 2. 设计

路径都相对 `D:/luyuan/xuanming-server-mmo-cpp`。

### 2.0 全流程(一条时序)

```
入口 E1–E4(新增 E5:rid 不得含合服 run_id 形状的子串;E6:-BarrierProtocol 只认 strict,B2d 之前 strict 也拒;E7:-Apply 必须给 -ArchiveDir)
→ 0a–0e(不变)
→ 0f:deploy-facts Schema 3 → 全服围栏读方配置核对 → 现网协议核对(dual + 本机 db.yaml dual)→ run_id → 工具版本 → <tool> -mode preflight
→ 写 entry-state
→ 【新】0g:全服配置复核 → <tool> -mode fence(ps1 起 etcd 端口转发)
→ 1:zone-down
→ 1b:(i) 消费者;(ii) 静默;(iii) 全服复核 → <tool> -mode stopped(etcd;基线同时写进持久副本目录);(iv) LAG
→ 3:PITR(恢复库 schema 落后时执行 go/db cmd/migrate -command up)
→ 4a:全服复核 → <tool> -mode inventory(ps1 起 guild 端口转发;计划等同时写进持久副本目录)
→ 4a′:停服复核
→ 5:-ToLatest,记切点
→ 4b:<tool> -mode purge(从持久副本加载计划,逐文件按清单 SHA 校验)
     P1 区级重试 / 死信族:逐条解码,范围内进隔离区,其余回原处
     P2 逐人一段 Lua:复核围栏与租约 → 状态∈{计划前态,计划后态} → 纪元抬升 → 删各族键 → ZREM
     P3 逐人 SQL 规范化并重建屏障
→ 5b:切点复核 → <tool> -mode verify(guild;从持久副本加载计划)
→ 6:zone-up
→ 【新】6b:<tool> -mode release
→ 7:<tool> -mode release 复跑作断言(必须 0)→ 清单
中止 / 放弃路径:原有提示末尾加 <tool> -mode release
回档完结、人工复核隔离区之后:<tool> -mode quarantine-drop -confirm
```

衔接关系:

- 围栏(#1)在 0g 立起,保持到 6b。stopped / inventory / purge / verify 开头都复核「K1、K2 恰为本 rid 的值且 PTTL=-1」;purge 的逐人 Lua 与 strict 的重建 Lua 把两把键放进 KEYS 原子复核。
- 纪元抬升(#2)只能在 Step 5 之后、该玩家删除之前:Step 5 之前还有中止路径,中止要保留 Kafka 积压;先删后抬会留下残留写者写回的窗口。所以放进 P2 的同一段 Lua,先抬后删。
- strict 的屏障重建(#3)用 C' 做 owner,放在 P2 全部结束之后(P3)。
- 帮会分歧闸(#8)在 inventory 判一次、verify 再判一次;purge 不调 guild。
- 需要全键空间信息的模式各做**一趟**不带 MATCH 的 SCAN,客户端按前缀分发;purge 不做全量扫描(§2.7)。
- Step 5 之后只能向前,所以 Step 5 之后要用的计划、基线与确认文件都在 inventory 结束时已写进持久副本目录,purge / verify 从那里加载(§2.7)。

### 2.1 #1 zone 维护围栏

**读方清单与判据(代码证实,订正 v1 的证据定位)**

| 读方 | 位置 | 判据 |
|---|---|---|
| data_service 建号登记(redis 模式) | `routing/router.go`:211-225 `IsMergeInProgress`(:158)EXISTS 预检,读失败按封锁;提交点 `setPlayerZoneIfAbsentScript`(组键 :238,脚本 :289-299)EXISTS KEYS[2] 命中回 3 → `ErrZoneMergeInProgress`(码 22),`player:zone` 与 `player:placement` 都不写 | EXISTS |
| data_service 建号登记(db_first) | `route_publish.go`:134-144 同一个 `IsMergeInProgress`;发布脚本 KEYS[4] 复核(`shared/placement/route.go`:294-317) | EXISTS |
| data_service `GetPlayerHomeZone` RPC | `server/dataserviceserver.go`:159-172 → `Router.GetPlayerHomeZoneAndMergeFence`(router.go:371-432;Lua :390-399 用 ARGV 传前缀再拼 home,不经 `MergeFenceKey`;读失败回 -1 按封锁);`ReadRequiresReady=true` 时走 `route_read.go`:120-145,副本不可信时回源 SQL 会绕过 K1。`Router.GetPlayerHomeZone`(:326-335)不读围栏 | EXISTS |
| scene_manager | `home_zone.go` `resolveHomeZone`(:110-179)→ `rejectHomeZoneMerging`(:194-200),回 `ErrHomeZoneMerging`=21,`reason=home_zone_merging`;调用点 `enterscenelogic.go`:441-445(跨区第一条腿)与 :600-614(第 3b 步:同区落点、重连、第二条腿),都在铸 epoch、写 location 之前 | 应答字段 |
| guild | `internal/logic/merge_fence.go`:74-86 `RedisMergeFence`(EXISTS);:152-178 `RouteReadyMergeFence`(值非 nil 即封锁,nil 再 EXISTS,副本不可信当封锁)。被拒的是 Z 归属公会的全部写:建帮 guild_logic.go:236、退帮 :463、解散 :518、改公告 :588、经济 economy_logic.go:303-313、活动结算 activity_logic.go:420、历练开局 activity_trial.go:268-270 | EXISTS / 非 nil |
| route_tool | `routetool/redisread.go`:74-93 `listActiveFences`(SCAN `merge:in_progress:*`);importer.go:18/:54、repair.go:22 只要有一把就退出 3 | 存在 |
| merge_zone | `acquireMergeFence` 对 src、dst 各 SetNX(fence.go:103-166);`placementCASScript` EXISTS 命中回 3(placement_ops.go:501-515、:530-531);pin_placement.go:64-67 与 :98;relocate.go:278 与 :354;审计 `verifyFenceReleased`(audit_checks.go:531-557)EXISTS | EXISTS / SetNX |
| barrierrestore | `recoveryKeys` 把 `MergeFenceKey` 放进 KEYS(restore.go:425-435),`restoreScript` 遇到即拒(scripts.go:20);`guardScript` / `mappingInspectScript`(route.go:210-241)要求 K1 恰为 `save_barrier_restore:<id>` 且 PTTL=-1 | 存在 / 精确值 |
| db_first 对账 leader | `placement.MergeFenceMirrorSetScript`(route_ready.go:424-434)遇到非 JSON 值回 2,不覆盖 | 不覆盖 |

不读 K1 的组件:`java/` 下无匹配;C++ 只认 scene_manager 错误码 21(player_lifecycle.h:1135、:1168);`go/login` 无匹配;trade 自己写明不读(tools/merge_zone/trade_step.go:29)。「值的格式不是读方契约」有代码原文为证:router.go:127-143、merge_fence.go:15-27、fence.go:23、route_ready.go:407-408。

**与合服、barrierrestore 互斥**:merge_zone 对 src、dst 都 SetNX,以 Z 为源或目标的合服 / 撤销都撞上 K1 被拒;`releaseFenceScript` / `refreshFenceScript`(fence.go:72-85)只处理值里含本次 run_id 子串的键,正常情况下碰不到 K1;为杜绝子串巧合,E5 与工具入口拒绝含 `[0-9]+-[0-9]+-[0-9]{8}T[0-9]{6}Z-` 子串的 rid(`newRunID` 形状,fence.go:316-318)。barrierrestore 在 Z 上同样被挡,互斥双向。

**同实例前提**:k8s_deploy.ps1:7415-7419 MappingRedis 固定 DB0,go/db 的 `Placement.Redis` 复用 RedisClient;`Get-DeployFactsProblems`(k8s_zone_rollback.ps1:1245、:1319)要求 MappingRedis.Host == RedisClient.Hosts 且 DB 为 0。Lua 里调 INFO 有在产先例:route_read.go:54-76 `getHomeZoneFenceReadyScript` 与 `placement.RouteReadyReadScript`;merge_fence.go:95-96 写明 miniredis 不支持 INFO 的 server / replication 段,相关用例只能在真 Redis 上测。

**生效前提(ps1 全服配置核对)**

上线前所有可部署的镜像都晚于 4493b3c9f(2026-09-29:`home_zone_merging` 应答字段、`rejectHomeZoneMerging` 与建号脚本内的原子复核都由它或更早的提交引入),v2 的「最低提交」表与镜像祖先核对不做,移到 §8 第 4 条。只核对与版本无关、会让 K1 失效的配置。发现键 `dataservice.rpc` 全局共用(k8s_deploy.ps1:7964-7966),任一 zone 的 data-service 实例都可能应答,所以逐 namespace 核对。

ps1 新增 `Read-GlobalFenceReaders`(只读 kubectl:`get configmap -A --field-selector metadata.name=go-svc-data-service-config` 与同法 `go-svc-scene-manager-config`)与纯函数 `Get-GlobalFenceReaderProblems`,逐 namespace 判:

1. data-service 的 `RouteStore.Mode` 缺省或为 `redis`,`RouteStore.ReadRequiresReady` 缺省或为 false(config.go:384 的 Validate 已保证 redis 模式下它恒为 false,仍显式读出并要求为 false;db_first 且要求就绪时副本不可信会回源 SQL,绕过只写在 Redis 里的 K1);
2. scene-manager 的 `AllowGateZoneAsHomeZone` 缺省或为 false,且配了 `DataServiceRpc`(Etcd.Key 或 Endpoints 非空;home_zone.go:111-133 的 dev 旁路下 scene_manager 不经 data_service,围栏无效)。

结果写 `<报告目录>/global-fence-readers-<阶段>.json`;第 0 步 f、0g 之前、stopped 之前、inventory 之前各读一次,任一不过即拒(防回档期间别的 zone 改了配置)。guild 的拦截只是纵深防御(回档正确性由 #8 资产分歧闸保证),不核对;login / C++ / trade 任何版本都不读 K1,列为不挡的面。

**能挡住的面**(前提成立时):home=Z 玩家所有带 GateId 的 EnterScene 在第一次写之前被拒;新建 home=Z 角色的映射登记被拒(redis 与 db_first 两档都原子拒绝);pin-placement 落在 Z 上被拒;relocate 跳过 home=Z 玩家(冻结 CAS 查 home 所在 zone 的围栏,relocate.go:278);Z 归属公会的写被拒;涉及 Z 的合服、撤销、barrierrestore 被拒;全服 route_tool import / repair / reconcile-from-cache 暂停。他区归属玩家被搬进 / 搬出 Z 库、`-backfill-home-zone` 给 Z 补映射不被 K1 挡住,见下表。

**挡不住的面与补法**

| 面 | 现状 | 补法 |
|---|---|---|
| 他区 login 的 EnterGame | login 全程不读 K1。`entergamelogic.go`:425-435 先 `EnsurePlayerAllDataInRedisAsync`(读任务发往 login 所在 zone 的 topic,他区 go/db 用 `decideReadPlacement` 读 zone_Z_db),login 再用 `publishParentLoadScript` 写 `PlayerAllData:<pid>`(PX 24h,sync_loader.go:138);之后 `applyLoadedPlayerSession` 才写会话、绑 gate(:748-800),EnterScene 被 21 拒时会话照旧保留(:835-849);go/db 读路径 `publishDBReadResult` 用 SetNX + cacheTTL 回写子表缓存 | 冷读产物一律带 PTTL>0,scene 首次存盘的裸 SET 会去掉 TTL(new_player_seed.go:70)。P2 删除时 PTTL>0 的父快照 / 子表缓存不论内容一律归为「冷读产物」删除并计数;verify 对已处理玩家接受与计划后态一致的冷读产物(C'>0:父快照 `last_save_seq`==seq(V)、子表缓存行围栏==V;c_prime 为空的未铸造玩家:PTTL>0 即可,P2 之后的冷读只可能读到换库之后的数据),PTTL=-1 仍按违例。`player:session` 不作在线证明。login 侧加闸属于新代码,不是正确性前提(冷读产物已由 P2 / verify 识别),不在 R9 内(§7) |
| 他区在线实体的跨服务资产写 | trade 资产通道按 `player:{id}:location` 直调玩家所在 scene(trade/internal/svc/assetchannel.go:33-37),mail / player_inbox 同类;guild 只按 `guild.zone_id` 判围栏 | 排空期间落到将被丢弃的存盘上。guild 由 #8 兜住;trade、mail、收件箱登记为残余风险(§5),排空时长计入窗口 |
| foreign_home_storage_here;relocate 把他区归属玩家搬进 / 搬出 Z 库;`-backfill-home-zone` 给 Z 补映射 | 写者是他区 go/db;relocate 冻结 CAS 只查 home 的围栏(relocate.go:278),backfill 的 SetNX 不查围栏(backfill_home_zone.go:230) | 不改 merge_zone(§8 第 5 条)。preflight、fence、stopped 硬拒;stopped 之后才出现的,由 inventory(Step 5 之前,可走中止路径换回原库)与 verify 在各自那一趟全量扫描里按同一套规则复核,发现即 3(复查 [3]);runbook 禁止回档窗口内运行这些模式 |
| 不带 GateId 的 EnterScene | 不查归属、不过围栏(enterscenelogic.go:592-599 自认的已知缺口) | runbook 声明前提「所有生产调用方都带 GateId」;登记残余风险 |
| 立栏瞬间的在途 EnterScene | home_zone.go:142-178 先 RPC 读 merging,之后才铸 epoch、CAS location,location 的 Lua 不含 K1 | 立栏后沉降 30s(≥ HomeZoneLookupTimeoutMs 缺省 1500ms + EnterScene 处理上界;先例 rollback_logic.go:143-150 的 `guildSettleDelay`=30s);阻塞集合为空后再隔 30s 全量复扫 |
| 没有 `player:zone` 映射的玩家 | home_zone.go:172-175 未映射按 gate zone 回落,不受围栏约束 | 本库有行而无映射 = legacy_owned,preflight 与 stopped 用只读 SQL 硬拒 |

**与 data_service `RollbackFence` 契约的差距**:`svc/servicecontext.go`:92-105 规定 zone 级回档围栏须「阻断登录 / Scene 激活 / 旧 epoch 存盘」,并注明仓库尚无实现、生产注入 nil。K1 只覆盖 Scene 激活(带 GateId、前提成立时);不挡登录(冷读产物见上表);旧 epoch 存盘由 #2 纪元抬升挡住。本工具不实现、也不声称满足该接口。

**键与值**

- K1 = `placement.MergeFenceKey(zone)`,值 `zone_rollback_discard:<rid>`,PTTL=-1。
- K2 = `barrierrestore.MaintenanceKey(zone)`(barrierrestore 已于 8b9592de3 入库,工具与它同属 go/db module,直接引用,另加一条钉死字面量 `player:save_barrier:maintenance:<z>` 的测试),值 `zone_rollback_discard:<rid>`,PTTL=-1。
- 两把键与 MappingRedis / OwnerEpoch / Placement 同实例同 DB(deploy-facts 已核对)。

**fence 模式(第 0 步 g;只依赖 Redis、etcd 与 preflight 落盘结果,不连 MySQL、不读 deploy-facts)**

1. INFO 核对 run_id 与 cluster_enabled:0;取写者租约(§2.7)。
2. 读 `<报告目录>/preflight.json`:pass=true、rid / zone / run_id 一致、扫描完成时刻(Redis TIME)不早于 15 分钟,否则 3。
3. 获取前只读预检:K1、K2 状态;准入取 preflight.json 的计数(那一趟扫描里 `merge:in_progress:*` 值以 `zone_rollback_discard:` 开头的 zone 数,含本次,超过 `-max-concurrent-rollbacks` 即 3)。不过以 3 退出(什么都没写)。
4. `fenceAcquireScript` 一段 Lua 获取(§2.4 Lua 契约):已持有即幂等;遇外人值或带 TTL 的同值以 3 退出。
5. 沉降 30s(单调时钟)。
6. 第一趟全量扫描(§2.7:一趟不带 MATCH 的 SCAN,客户端分发 H(`player:zone:*` 取 home=Z)/ P / merged_into / in_progress)。立栏后复核:foreign_home_storage_here、frozen 落点规则、合服标记规则(同 preflight),准入按本趟重算。不过 → 写 `fence-drain.json`(reason=post_acquire_recheck 或 admission)以 5 退出,围栏保留。
7. 排空:对 H 按批读 location、handoff、load 集、death_at、scene zone,每轮取一次 etcd 注册表快照 → 按下面的判据得到阻塞集合 B(Online ∪ Undecided ∪ handoff 存在)。
8. B 非空:每 15s 只重读 B;B 为空 → 再沉降 30s → 第二趟全量扫描(同第 6 步,含复核)→ 新的 B 仍为空即排空完成,写 `fence-drain.json`,以 0 退出;不为空回到本步。
9. 截止 `-drain-wait`(缺省 30m,上限 6h,单调时钟):只计第 8 步的等待与沉降,**不计**两趟全量扫描(扫描耗时由 `-timeout` 约束,preflight 已按键数估过,§2.7;复查 [4])。到期写 `fence-drain.json`(reason=drain_timeout)以 5 退出,围栏保留。同一 rid 整脚本重跑会从 preflight(K1 恰为本 rid 视为续跑)走到 0g 接着等;放弃则 `-mode release` 后走中止路径。月卡租约可顺延、挂机会话可以无限期在线,协议上没有自然上界,所以超时退出是必需的。

`fence-drain.json`:`{rid, zone, reason, started_at, deadline, rounds, registry:{revision, cluster_id, trusted}, online:[…], stale_ignored:[…], undecided:[…], counts:{no_location, own_zone, awaiting_expired}}`,三类名单逐人附 `location_hex`、zone、node、update_time、load_present、load_card、death_at_present、registered_count、handoff_present 与判定依据。

**他区在线判据(`go/shared/scenenode/liveness.go`,纯函数 `ClassifyLocation`,工具专用)**

输入:location 原值、scene zone 反查值、Redis TIME 秒、目标 zone Z、`ZSCORE scene_nodes:zone:<zl>:load <node>`、`ZCARD` 同键、`EXISTS node:zone:<zl>:<node>:death_at`、etcd 注册表快照(`RegistrySnapshot`)。

| 序 | 条件 | 判定 | 阻塞 |
|---|---|---|---|
| 1 | location 不存在 | NoLocation | 否 |
| 2 | protobuf 解析失败 | Undecided | 是 |
| 3 | zone_id≠0 取它;zone_id==0 按 `scene:<scene_id>:zone` 反查,查无或非法 | Undecided | 是 |
| 4 | 解析出的 zone == Z(含 Z 自己 zone-down 留下的) | OwnZone | 否(由 stopped 的「Z 的 load 集为空」覆盖) |
| 5 | node_id 为空(等待落点):now−update_time>300(`awaitingPlacementExpired` / `redirectTokenTTLSeconds`=300)| AwaitingExpired | 否 |
| 5′ | node_id 为空且未过期 | Undecided | 是(围栏下第二条腿必被拒,300s 内必过期) |
| 6 | 节点在 `scene_nodes:zone:<zl>:load` 里 | Online | 是 |
| 7 | 该 zone 的 load 集 ZCARD==0,且 death_at 不存在 | StaleZoneGone | 否 |
| 8 | 快照可信、该 (zone,node) 注册条数为 0,且 death_at 不存在 | StaleNodeGone | 否 |
| 9 | 其余(death_at 仍在、快照不可信、Redis 读失败) | Undecided | 是 |

- 「death_at 存在即判不出」:`node:zone:<z>:<n>:death_at` 只在节点死亡后 `NodeDeathMarkTTL`=10 分钟内存在;按 GO-6,节点摘出 load 集的前提是 death_at 已落地或已满一个屏障,所以「不在 load 集 + death_at 不存在」蕴含再入屏障已过,工具不必读 `SceneReentryBarrierSeconds`。
- `RegistrySnapshot`:对 etcd 前缀 `scenenode.SceneNodeRpcPrefix`(`SceneNodeService.rpc/`)做一次 Get;跳过含 `/allocated/` 的键;其余每个值只解析 `zoneId` / `nodeId`,任一解析失败即整份快照 `Trusted=false`。与 `Watcher.parse` 不同,缺 grpcEndpoint 的注册也计入(死亡证明不能漏数);同一 (zone,node) 多条注册按存活。
- scene_manager 不改用这个函数:它的 `playerLocationOwnerDead`(enterscenelogic.go:1005-1026)依赖进程内 etcd 视图与本地观察时刻(`isNodeGoneFromRegistry` / `localGoneBarrierBlocks`),语义更强、读不到 Redis 之外的东西也照样成立。两者共享的只有键名(§3 B1 把 `NodeLoadKeyFmt`、`NodeDeathAtKeyFmt`、`SceneZoneKeyFmt` 收成 shared 单一来源)。
- `player:session` 不读:被 21 拒的玩家会话照样保留、Z 的 gate 还活着,会话证明不了「人在场景里」,键名也属 player_locator 私有。

**release 模式(Step 6b;只依赖 Redis,参数只要 -redis / -redis-db / -redis-password-env / -expect-run-id / -zone / -rollback-id / -report-dir)**

- 不读 deploy-facts、不连 MySQL、不比镜像:中止路径上影子库没换回、DSN 环境变量丢了、Step 6 换了 go/db 镜像,都不能卡住撤围栏(复核 6.2.9)。
- 一段 `fenceReleaseScript`:K1、K2 按**整值相等**判属本 rid(不照抄 merge_zone 的 `string.find` 子串匹配),属我方的删除(带 TTL 的我方值同样删除,并记 `ttl_tampered`);外人值不碰。清单写 `fence_released_at`。
- 返回:两把都恰为我方永久值并已删除 → 0;清单已记 released 且两把都不是我方值 → 0(幂等);其余(K1 或 K2 中途缺失、被改成外人值、被加了 TTL)→ 删掉仍属我方的那把,以 **7** 退出,报告写「围栏中途丢失」并列出各键现值;后续模式会以 fence_lost 拒绝。
- 只在三个时机执行:Step 6 rollout 成功之后(6b);中止路径的 zone-up 之后;Step 7 复跑作断言。

**复核**

- stopped / inventory 用 GET + PTTL 只读复核;purge 与 strict 的写 Lua 把 K1、K2 放进 KEYS 原子复核;缺失或被改 → 3(写 Lua 返回 `fence_lost`)。

**政策与副作用(写进 runbook)**

- 等他区在线的本 zone 玩家自然下线,不踢。「他区在线集合只减不增」只在前提成立时、对带 GateId 的请求、过了沉降期之后成立。
- 围栏期间与忘撤的影响:home=Z 玩家全部进不了场景(登录预加载照常执行、会话照样保留);Z 不能建号;Z 公会的全部写被拒,**guild 历练结果消费会全服卡住**:`trial_result_consumer.go`:20-37 的纪律 2 规定暂时性失败无限重调,Z 公会的一条结果撞上 ErrZoneMerging 后整个消费协程停在这条消息上(单副本时即全服),已登记的对局靠巡检器 420s 后兜底结算,未登记的要等到 release;全服 route_tool import / repair / reconcile-from-cache 暂停(merge-zone-runbook.md:917);涉及 Z 的合服 / 撤销、在 Z 上钉落点、对 home=Z 玩家的搬库、barrierrestore 全部被拒;他区归属玩家搬进 / 搬出 Z 库与给 Z 回填映射不被 K1 挡住,由 runbook 禁止、回档侧复核兜底(发现即拒)。
- 0g 沉降期 ps1 打印两条 PromQL 供操作者核对:`sum(increase(guild_trial_result_total{result="handler_retry"}[5m]))` 与 guild 结果消费组 lag;持续上涨即提示「历练结算已卡住,回档窗口越短越好」。只提示,不作闸门。
- 告警分层:新规则 `SceneManagerHomeZoneFenceHeldTooLong`(§2.8);ps1 Step 7 复跑 release 必须 0;k8s_deploy zone-up 结束时只读 GET `merge:in_progress:<zoneId>`,值以 `zone_rollback_discard:` 开头即打印醒目 WARN 与 release 命令(Step 6 期间这条 WARN 属预期,文案写明);`guild_trial_result_total{result="handler_retry"}` 持续上涨作旁证。

### 2.2 #2 纪元抬升与不可变计划

**停服基线(stopped)**

- 本 zone 归属玩家(H 遍,加上 M 遍里有行且 home=Z 的)按 `pid%64` 分片流式写 `stopped-players-s<NN>.ndjson`,每行 `{pid, owner, home_raw, placement_raw}`;`fingerprint-baseline.json` 记每个分片的 SHA-256。基线同时写进报告目录与持久副本目录(§2.7),SHA 记进清单。
- 同一 rid 已有 phase=stopped 的基线不覆盖。

**U(p)、C'、V(raise.go,纯函数;只在 inventory 首次计算)**

- U(p) = max(owner_stop;当前世代 topic(`config.DbTaskTopicForGeneration`)两表的 applied_epoch(`kafka.AppliedEpochKey`);屏障 pending / player_database / player_database_1 / outbox_version 的 epoch(`savebarrier.Parse`);资产证据 asset_sql_commit 的版本;PITR 之后两表的 fence_owner_epoch)。这些 Redis 来源停服之后不变(围栏加停服),由「owner 未变」间接证明。
- 判定:owner_stop==0 且 U>0 → owner_behind;owner_stop<U → owner_behind;owner_stop==MaxUint64 → unclassified;owner_stop==0 且 U==0 → 未铸造,不抬、不建键;其余 C'=owner_stop+1。

**计划(plan.go)**

- inventory 为**全部已归类玩家**各写一行 `plan-s<NN>.ndjson`:H 遍的全部 home=Z 玩家,加上 M 遍里本库有行的他区归属玩家(复查 [0])。字段:`{pid, class, action: purge|skip, owner_stop, c_prime("" = 不抬), home_raw, placement_raw, location_raw, applied:{player_database, player_database_1}, parent:{state: absent|scene|login_product, sha1}, asset_sql_commit:{sha1}, save_barrier:{sha1, fields}, row_fences:{表: [epoch, seq, schema_gen]}, p3:{v, g, s, sql:{player_database: keep|cas|insert, player_database_1: keep|cas|insert}}, bucket}`。action=purge 的是 rollback / torn / post_t_candidate;action=skip 的是 storage_elsewhere / home_elsewhere,只填 home_raw / placement_raw 供 verify 复核(storage_elsewhere 要求逐字节未变;home_elsewhere 只要求不变成 home=Z、有效落点仍≠Z)。p3 由 inventory 的 strict 预检算出(§2.3),c_prime 为空的玩家没有 p3。
- `plan.json` 记各分片 SHA-256、各类人数,以及每张围栏表的期望插入行数 `expected_inserts`(由各行 p3.sql 的 insert 汇总;复查 [8]);清单字段 `plan_sha256` = sha256(plan.json)。计划同时写进报告目录与持久副本目录(§2.7)。
- purge 第一轮开始后计划冻结:清单 `pass_count`>0 时 inventory 重跑一律 3(错误文本指向 purge / verify 续跑);P2 之后 owner 已抬到 C',计划无法重新生成,只能从持久副本加载。
- U(p) 不再在 purge / verify 重算,本工具写入的 applied==C'、屏障三字段==V、行围栏==V 不会被当成外来证据(复核 6.1.1)。

**inventory 的 owner 复核**:现读 owner 必须逐人等于停服基线;不在基线里却在范围内的玩家也算违例 → 3(围栏漏了,此时中止路径还在)。

**状态判定(purge 的逐人 Lua 与 verify 共用一张表)**

| 状态 | owner | 两个 applied | 父快照 / 缓存 / 证据 | 屏障 |
|---|---|---|---|---|
| 计划前态 | == owner_stop | == 计划值(缺失即仍缺) | sha1 == 计划值,或已不存在,或 PTTL>0 冷读产物 | 整 HASH 的 sha1 == 计划值(不论有哪些字段) |
| P2 后态 | == C' | == C' | 不存在或 PTTL>0 冷读产物 | 不存在 |
| P3 后态 | == C' | == C' | 不存在或与 V 一致的冷读产物 | 字段 ⊆ {pending, player_database, player_database_1, schema_gen},PTTL=-1,三个版本字段 == V,schema_gen == 计划的 S(**不按 HLEN 判**,复查 [10]) |

c_prime 为空(未铸造)的玩家:owner 与 applied 恒为缺失,终态是 P2 后态,不做 P3。

**抬升(P2 逐人 Lua 的前半段)**:比较一律用导出的 `kafka.LuaDecimalGreaterFn` 按十进制精确比较,不用 tonumber;前置检查:类型 string、规范十进制、PTTL=-1;owner ∈ {owner_stop, C'},否则 `owner_advanced`;每个 applied ∈ {计划值, C'},否则 `applied_ahead`;通过后 SET 三键为 C'。计数 raised / already。

**不抬的**:MySQL 行围栏(dual 下 Redis 守卫在 SQL 之前拒;P3 只按 §2.3 规范化形状,不抬);他区归属玩家;storage_elsewhere(它们的库不回退,saveoutbox 要用旧 epoch 补投被 Step 5 丢掉的写)。

### 2.3 #3 屏障与协议世代(只支持 strict)

**判定(target.go + ps1)**

- 现网:deploy-facts 的 FencingMode 必须为 dual,`GET saveoutbox.GenerationKey(zone)` 必须恰为 `saveoutbox.GenerationStrict`,否则 3。legacy(Fencing.Mode=legacy、没有世代标记)的错误文本写明「本工具只支持严格协议;上线后如需 legacy 回档,按本设计 §8 第 1 条补」。不再读 64 个补投桶的 ZCARD 判世代(那只是区分 legacy 的依据;strict 下桶里本来就可能有 storage_elsewhere 的合法成员)。
- Step 6 的 go/db:ps1 读本机 `go/db/etc/db.yaml` 的有效 `Fencing.Mode`(k8s_deploy.ps1 按工作树渲染 ConfigMap,缺省 dual),不是 dual 即第 0 步拒绝(否则 Step 6 渲染出 legacy go/db,遇到 strict 世代标记拒启,问题出在 Step 5 之后)。Step 6 的 node / login 镜像是否说严格屏障不核对:上线前不存在不说严格屏障的镜像(§8 第 3 条)。
- `-barrier-protocol` 只接受 `strict`,其它值 3。
- **接线闸**(复查 [1]):B2a 起 CLI 带一道闸,除 release / quarantine-drop / `-version` 外的模式一律以 3 拒绝(文案「strict 屏障重建(P3)尚未接线,本构建不可用于回档」);包内函数不受影响,单测与真库用例经包内接口驱动。B2d 接线 P3 时删掉这道闸,同批删掉 ps1 入口对 strict 的拒绝,两边各补一条测试。这样 B2a–B2c 与 B3 任意先后合入,现网 `-Apply` 都在第 0 步之前被拒,不会出现「P2 归档删除了屏障、却没有 P3 重建」的半截回档。

**P2**:屏障与资产证据「归档 + sha1 复核 + UNLINK」,ZREM 本人在桶里的成员;带 pending / outbox 的父快照另行归档;「有 pending、无 ACK」或带 outbox 字段的,在 `purge-players-p<N>.ndjson` 逐人记 pending / 两个 ACK / outbox_version / outbox_topic / schema_gen / 处置,计 `discarded_uncommitted_intents`。屏障按整 HASH 的 sha1(字段名排序后拼接)判定,不按 HLEN:`schema_gen` 字段由混版本持久字段保护设计引入(mixed-version-persist-field-protection.md §5.2)。不重放:按裁决 (A),T 时刻不在 MySQL 里的意图一律丢弃;归档满足 db-write-fencing-mysql.md 的「先保存、逐笔裁决」。

**P3,对每个 action=purge 且 C'>0 的玩家(strict.go)**

形状、V、G(两表的 `persist_schema_gen`)、S(屏障的 `schema_gen`)与每张表的 SQL 动作(keep / cas / insert)都由 inventory 的 strict 预检按只读结果算好写进计划(§2.2 的 `p3`);P3 只执行计划,并在行锁之下复核形状未变,不重新决定。

1. TiDB 强会话(`authoritysql.Open(strong=true)`;本机 MySQL 显式 `-local-mysql`)。
2. 一个事务里对 player_database / player_database_1 做 FOR UPDATE 读,形状必须与计划一致,否则 `shape_changed`、整体停:
   - 两行都在、围栏相等且非零、两行戳相等 → V = 该围栏,G = 该戳,两表 keep,不写 SQL;
   - torn(缺一行,或两行围栏都非零但不同):落后的那张表(缺行或围栏较小)先断言除「非业务列」外没有任何非默认值,否则判 **torn_cross_table**(blocked,inventory 预检即判出)。非业务列不写死列名,由编译进二进制的 proto 推导:主键 `player_id`、标了 `OptionPersistUncarried` 的字段(现为两表的 `stress_test_probe` 与 player_database 的 `merge_state`)、`fence_owner_epoch` / `fence_save_seq`、`persist_schema_gen`。通过后 V = 较新那行的围栏,G = 两行戳的最大值(缺行按 0);落后的行以「围栏与戳都等于预检读到的值」为条件 CAS 成 (V, G)(RowsAffected==1),缺的表用 proto2mysql 的 `ExistsByPK`+`Insert` 插入只有 player_id、围栏 V、`persist_schema_gen`=G 的空消息;
   - 两行都缺(post_t_candidate 且 owner_stop>0)→ V = (C',1),G = 0,两表都插骨架行(`persist_schema_gen`=0,与 login 新号种子「不盖戳」同义,mixed-version-persist-field-protection.md §5.4);
   - 其余一律 unclassified(blocked):两行围栏都是 (0,0)(只有 legacy 时代写入的行才会这样,上线前不存在,处置移到 §8 第 2 条)、围栏一半为零、围栏相等而戳不同、主键不唯一、player_id 不符、围栏或戳非规范、V 的 epoch 大于 C'。
3. 提交之后另开事务重锁重读,两行围栏必须等于 V、戳必须等于 G。
4. 一段 `strictPublishScript`:前置检查租约、K1/K2、owner==C'、handoff 不存在、本人不在自己的桶里、屏障不存在,或字段 ⊆ {pending, player_database, player_database_1, schema_gen} 且三个版本字段 == V、schema_gen == S、PTTL=-1;父快照与子表缓存不存在,或 PTTL>0(冷读产物)→ 同段 UNLINK,PTTL=-1 → `cache_present`;通过后 HSET pending / player_database / player_database_1 = V、schema_gen = S。S = `barrierrestore.SchemaGenFloor(G, G)` = max(1, G):B2d 把 save_barrier_restore 现有的 `schemaFloor`(restore.go,取两行戳最大值、0 按 1)导出为这个函数,两边共用一条规则(复查 [10])。
5. 回滚读事务。

- 先提交 SQL,后发布 Redis:SQL 失败绝不发布 ACK。重跑幂等:CAS 与 Insert 遇到「已是 (V, G)」即重读收敛,Lua 接受同值四字段。结果写 `strict-barrier-p<N>.ndjson`,只作审计:verify 的期望插入行数取自计划,不读它(复查 [8])。
- 骨架行按完整 proto 写:inventory 已要求恢复库 schema 通过 `migrate.ProtoSource.Drift`(§2.4)。骨架行与对齐后的戳要过 go/db 的 C0–C2 判据(读:行世代 ≤ 本二进制世代;scene 首存:输出世代 = max(本二进制世代, 两表戳最小值) ≥ 屏障 schema_gen),由 B2d 真库用例验证。

**为什么两表戳要对齐**:加载世代取两表戳的最小值,屏障 schema_gen 取最大值(mixed-version-persist-field-protection.md §3.3、§5.2);torn 时两行戳可能不同,只对齐围栏会让首存的倒退比较误判。落后那张表已断言没有非默认业务列,把它的戳抬到较新那行不会掩盖任何截断。

**为什么骨架行**:严格协议的不变量是 owner≠0 ⇒ 要么「父快照加屏障」,要么「SQL 行围栏 == pending」(parent_commit_barrier.go:38-44、:110-111);只写父快照骨架不可行(父快照有 TTL,过期后冷读因 SQL 缺行被拒)。骨架行加 (C',1) 等价于替玩家做一次「空角色首存」,scene 按 registration_timestamp==0 走新号分支,身份由 login 的 backfillPlayerIdentity 补齐,与冷读缺行得到空骨架的语义一致。

**inventory 的 strict 预检**:只读地对 action=purge 且 C'>0 的玩家做一遍上面的形状判定,结果写进计划的 `p3`(V、G、S、两表的 keep / cas / insert);判不出的归 unclassified(strict_*),torn 落后表带业务列 → torn_cross_table,都归 blocked。

**不改 barrierrestore 的流程**(只导出 `SchemaGenFloor`,行为不变):它的 restoreScript 要求合服围栏不存在、要求 64 桶全空(storage_elsewhere 的合法成员会留在桶里)、Plan 是 ≤1000 人人工审定流程,且 readPair 拒绝两行围栏不同版的 torn 形状。K2 让 save_barrier_restore 无法与回档并发,这是有意的。

### 2.4 回档工具:八个模式

**结构**

- 薄 CLI:`go/db/cmd/zone_rollback_discard/main.go`(flag、`-version`、退出码映射、B2d 之前的接线闸)。逻辑全部在 `go/db/internal/zonerollback`。
- 依赖:go-redis(`MaxRetries=-1`,`PoolSize=workers+2`)、etcd clientv3(go/db 已直接依赖)、`shared/authoritysql`、proto2mysql、`db/internal/migrate`(`LoadColumns`、`ProtoSource.Drift`)、`config.DbTaskTopicForGeneration`、`shared/guildcheck`、`shared/scenenode`、`shared/placement`、`barrierrestore.MaintenanceKey` / `SchemaGenFloor`,以及 §3 B1 的键名导出。

**公共参数**(Get-DiscardToolArgs 拼,顺序固定)

- 原有:`-deploy-facts -zone -topic-generation -redis -redis-db [-redis-password-env] -expect-run-id -mysql-dsn-env -rollback-id -barrier-protocol -report-dir -mode`;`-barrier-protocol` 只接受 `strict`。
- 新增:`-topology-manifest-dir`、`-archive-dir`(持久副本目录,stopped / inventory / purge / verify 必填,§2.7)、`-workers`(缺省 4,上限 8)、`-timeout`(缺省 8h)、`-local-mysql`、`-redis-keys-per-sec`(缺省 20000,上限 100000)、`-scan-count`(缺省 500)、`-max-inflight-bytes`(缺省 8MiB)、`-max-concurrent-rollbacks`(缺省 2,上限 4,含本次)。
- 口令与 DSN 只从环境变量读,不进报告。rid 入口校验:`^[A-Za-z0-9_-]{1,96}$`,且不含 `[0-9]+-[0-9]+-[0-9]{8}T[0-9]{6}Z-` 子串。

**每个模式开头**(release、quarantine-drop、fence 除外,见各自说明)

- deploy-facts Schema 3:ZoneId / 世代 / 分区数一致,四路 Redis 同实例同 DB,RouteStoreMode=redis,协议判定见 §2.3。
- Redis:INFO server 的 run_id == `-expect-run-id`,INFO cluster 的 cluster_enabled:0。
- MySQL:`SELECT DATABASE()` == `placement.StoreDBName(zone)`。
- 玩家表 = 编译进二进制的 mysql_database_table.proto 里 OptionPrimaryKey=player_id 的消息 ∩ information_schema 实际存在的表(与 merge_zone 同口径);围栏表 = 带 fence_owner_epoch / fence_save_seq 列的表。
- 除 preflight 外复核 K1/K2;写模式先取写者租约。
- 需要全键空间信息的模式只做一趟全量扫描并按前缀分发(§2.7);下文各模式写的 H / P / merged_into 等都取自这一趟。

**-version**:恰好两行 `commit=<40hex>` 与 `dirty=<true|false>`;读 `-X main.buildCommit` / `main.buildDirty`,未注入时回落 `debug.ReadBuildInfo`。

**preflight(停服前,只读;新增 `-target-time`)**

- 原契约的检查全部保留(Redis 身份、各族键 TYPE、隔离区按续跑规则、zone 库可连、玩家表可读、退出码 4 规则)。
- K1、K2 不存在,或恰为 `zone_rollback_discard:<本 rid>` 且 PTTL=-1(视为续跑)都放行;其它值以 3 拒绝。
- 只读 Redis 即可判出的硬拒(下称「停服前硬拒规则」,stopped、inventory、verify 原样复用):foreign_home_storage_here(P 遍 `player:placement:*`,`placement.Parse` + `EffectiveStorage`,有效落点=Z 且 `player:zone`≠Z 或缺失);frozen 落点规则(frozen 且 storage=Z → 拒;frozen 的 run_id 在拓扑清单目录里找不到清单,或清单 src / dst 含 Z → 拒);合服标记规则(`merge:merged_into:*` 里 dst=Z 的:run_id 开始时刻 ≥T → 拒;早于 T 的必须对上清单且写步骤全部 Done、At≤T−1s);拓扑清单目录里有 src 或 dst 含 Z、尚未走到 switched / aborted 的 relocate run → 拒;准入。
- 只读 SQL:legacy_owned(本库有行而 `player:zone` 缺失)→ 拒。
- 他区在线只报告(按 §2.1 判据,不连 etcd,需 etcd 才能定的一律计 undecided),不拒绝。
- 扫描预算(复查 [4]):`DBSIZE` 除以 `-redis-keys-per-sec` 得单趟扫描时长 t;fence 的 2t 加两次沉降,或任一模式的单趟 t,超过 `-timeout` 即 3;六趟合计与停服关键路径上的三趟(stopped / inventory / verify)写进 preflight.json 供排期。
- 写 `preflight.json`(含扫描完成时刻、准入计数、扫描预算)。

**fence / release**:见 §2.1。

**stopped(停服后,`-lease-wait / -handoff-wait / -fingerprint-baseline / -target-time / -etcd / -archive-dir`)**

1. 原契约检查:`scene_nodes:zone:<z>:load` 为空(`scenenode.NodeLoadKey`);本 topic 没有未过期的重试租约与 `distributed:lock:kafka:ordering:<topic>:` 前缀下的存活锁(`-lease-wait`,单调时钟截止);handoff 另按 `-handoff-wait`(≥300s)等待。
2. K1/K2 复核;全服读方复核结果由 ps1 保证。一趟全量扫描分发 H / P / merged_into / 排序锁前缀(第 1 步的锁检查也取自这一趟)。
3. 他区在线:按 §2.1 判据(带 etcd),任一 Online / Undecided → 3。
4. 停服前硬拒规则与 legacy_owned 复核(同 preflight)。
5. 最后写基线(报告目录与 `-archive-dir` 双写):
   - 每张玩家表按主键切块,块大小自适应(初始 1000 行,目标每块 0.5s,范围 100–100000 行),块边界覆盖整个 uint64 空间并写进基线;
   - 每块在服务端算 `COUNT(*)` 与两段 128 位摘要:`BIT_XOR(CAST(CONV(SUBSTRING(r,1,16),16,10) AS UNSIGNED))`、`BIT_XOR(CAST(CONV(SUBSTRING(r,17,16),16,10) AS UNSIGNED))`,其中行摘要 `r = MD5(CONCAT(<逐列:IF(c IS NULL,'1','0'), MD5(CAST(c AS BINARY))>))`,列按 ORDINAL_POSITION 排、反引号转义;逐列定长 MD5 拼接,没有分隔符歧义;
   - `stopped-players-s<NN>.ndjson`(§2.2);
   - 最后存盘的生产时刻(`fence_save_seq>>10`)落在 (T−15m, T] 的行写 `seq-witness.ndjson`(`{table, pid, fence_owner_epoch, fence_save_seq}`);按 Δ 切成判定窗口与只计数窗口是 inventory 的事,基线不预先切;
   - 先写 .partial,写完 rename;文件记 phase=stopped / rid / zone / Redis TIME / UTC / 列清单。

**inventory(PITR 之后,只读;判定参数 + `-report -guild-rpc -guild-check-budget`(缺省 30m))**

1. 读基线(先报告目录、缺失则持久副本,SHA 与清单一致;缺失或 phase 不对 → 3)。清单 `pass_count`>0(purge 已开始)→ 3。
2. schema:恢复库对编译进二进制的 proto 跑 `migrate.LoadColumns` + `ProtoSource.Drift`,非空 → 3,错误文本给出 `go run ./cmd/migrate -f <db 配置> -command up`(只 ADD COLUMN)。
3. 按基线块边界重算,列 = 基线列清单 ∩ 现列清单;列差异写进确认模板(`column_diff`)。全部块相同 → 3,除非 `-acknowledge-no-post-t-writes`。
4. owner 复核(§2.2)。
5. 盘点(scan.go):一趟全量扫描分发 H / P / merged_into / 孤儿 processing。H 与停服快照比对:映射变了 → route_changed → 3,多出或缺少 home=Z 玩家 → 3;P 与 merged_into 按停服前硬拒规则复核,新出现的 foreign_home_storage_here、涉及 Z 的 frozen、清单对不上的 frozen 或合服标记 → 3(复查 [3]:此时还在 Step 5 之前,中止路径能换回原库,搬进旧库的行随原库保留);M 遍扫 Z 库全部玩家表主键,对 M 遍里不在 H 的玩家逐人 MGET 映射与落点。孤儿 processing 键名写进计划,purge 不再扫描。
6. 归类(classify.go,§2.6)与拓扑证明(topology.go,§1 #6)。
7. #5 度量(Δ = `-witness-lag`,复查 [2]):离群行(>T+60s)>20 → 3,1–20 → 确认模板 `seq_outliers`。见证行:恢复后围栏大于基线(epoch 或 seq 更大)→ 3(库不对);判定窗口 (T−15m, T−Δ] 内不一致(恢复后更旧或缺行)的行数超过该窗口见证行数的一半且该窗口不少于 20 行 → 3(PITR 过早);其余不一致逐行进确认模板 `witness_mismatch`(超过 1000 行 → 3);(T−Δ, T] 的行只把总数与不一致数写进 inventory.json;判定窗口为空 → 确认模板 `quiet_before_t`(注明只计数窗口的行数)。确认模板另记 `witness_window`(Δ 与两个窗口的边界)。
8. post_t_candidate 或 home_elsewhere 占比 > `-max-post-t-ratio`(缺省 0.05,且人数 >20)→ 3;owner_stop>0 的 post_t_candidate → 确认模板 `post_t_players`。
9. strict 预检(§2.3),结果写进计划的 `p3`,汇总出 `expected_inserts`。
10. 帮会资产分歧闸(asset_gate.go,复查 [5]):对 action=purge 的全部玩家以 since=T_ms−300000(与 data_service `GuildClockSkewMarginMs` 缺省同值)调 `guildcheck.NewStreamer(guildpb.NewGuildInternalClient(conn)).StreamDivergences`,总预算 `-guild-check-budget`;分歧逐行流式写 `asset-divergence.ndjson`(按 player_id 分块升序、块内 op_id 升序,顺序确定,不设行数上限,不逐行打 ERROR 日志),不可证明的玩家写 `asset-unprovable.ndjson`;error(不可达、预算耗尽、guild 答复自相矛盾)→ 3。确认模板 `asset_divergences` 只记 `{sha256, rows, by_kind:{kind: {rows, funds_delta, contribution_delta}}}`,`asset_unprovable` 只记 `{sha256, players}`。
11. 归档字节估算(屏障、资产证据与带 pending / outbox 的父快照的 STRLEN / HSTRLEN 累加)> `-max-archive-bytes`(缺省 1GiB)或报告目录、持久副本目录任一的可用空间 → 3。
12. 输出 `inventory.json`(各类人数、阈值、blocked 前 1000 名、见证窗口计数)、`inventory-<class>.ndjson`、`plan.json` 与 `plan-s<NN>.ndjson`、`asset-divergence.ndjson`、`asset-unprovable.ndjson`、`acknowledge-template.json`;报告目录与持久副本目录双写,各文件 SHA 记进清单(§2.7,复查 [9])。
13. 有 blocked → 6;确认模板非空而 `-acknowledge-file` 缺失或任一节与模板不逐字相同(规范 JSON)、或带了模板之外的节 → 6。确认文件通过后同样复制进持久副本目录,SHA 记进清单。

**purge(判定参数 + `-confirm -report`)**

1. 不再重跑全部判定:从 `-archive-dir` 加载计划,逐分片 SHA、`plan_sha256`、确认文件 SHA 与清单一致(持久副本缺文件或不符 → 3,复查 [9]);复核 K1/K2、租约、deploy-facts、`-acknowledge-file` 与 inventory 时一致;任一不过 → 3,什么都不写。逐人状态在写 Lua 里原子判定。
2. HINCRBY 清单 `pass_count` 开新一轮 pass=N。
3. **P1 区级重试 / 死信族(quarantine.go,逐条分流)**
   - 源键按已知键名枚举,不全量扫描:`RetryReadyKey(topic)`、`RetryPartitionReadyKeys(topic, PartitionCnt)`、`RetryLegacyProcessingKey(topic)`、`ZRANGE RetryInstancesKey(topic)` 登记的每个实例的 `RetryInstanceProcessingKey`、`RetryDeadKey(topic)`,以及计划里记下的孤儿实例 processing 键(inventory 那一趟扫描列出;停服之后本 topic 没有消费者,不会新增,verify 再复核)。
   - 每个源键 L:一段 `stageSourceScript` 先在清单登记 `staging:<N>:<L>` 再 `RENAMENX L → rollback:quarantine:<z>:<rid>:<N>:staging:<L>`(原子);再按批(≤100 条)从暂存键尾部取条目,在 Go 里解码玩家:v2 / 毒消息 / v1 / 裸载荷分别按 `unwrapRetryPayload` / 毒消息 JSON 的 Key / proto 解码取 task.Key。
   - 计划里 action=purge 的玩家 → `rollback:quarantine:<z>:<rid>:<N>:<L>`(建键时同段 PEXPIRE `-quarantine-ttl`,缺省 720h);其余(action=skip、计划之外、解不出)→ 重试族回 `RetryReadyKey(topic)`(由 owner 实例按 v2 头重新分拣),死信回 `RetryDeadKey(topic)`;逐条写 `p1-routed-p<N>.ndjson`。
   - 死信条目进隔离区时同段 Lua 把它在 `kafka:dead:index:<topic>` 里的字段(按 `kafka.DeadDedupField` 推导,指纹相同才动)HDEL 并 HSET 到 `rollback:quarantine:<z>:<rid>:<N>:kafka:dead:index:<topic>`;源死信列表为空时同段 DEL 源索引。
   - `RetryInstancesKey` / `RetryInstanceModesKey` 整键 RENAMENX 进隔离区(实例已全部停止)。
   - 暂存键在 quarantine 命名空间里,进程中断后同 rid 重跑先按清单登记的 `staging:*` 字段逐个排空再开新键,不扫描。目标键已存在(清单与隔离区不一致)→ 1。
   - guard 键不碰(TTL 5 分钟,停服加 PITR 早已过期)。
4. **P2 逐人(worker 池按 pid%64 分片)**
   - pipeline 读三族证据现值:屏障、资产证据全量值(HGETALL),带 pending / outbox 的父快照全量值;其余父快照只在 Lua 里比 sha1。
   - 追加写 `purge-archive-p<N>-s<NN>.ndjson`(`{pid, key, type, pttl, value_b64 | fields_b64, sha1}`),报告目录与持久副本目录双写,两边每批 fsync 成功之后才执行这一批的删除 Lua(任一边失败 → 1,这一批不删);文件 0600、目录 0700。
   - 执行 `purgePlayerScript`(§2.4 Lua 契约);计划后态返回 already。
   - 追加 `purge-players-p<N>.ndjson`;清单 `shard_done:<N>:<NN>` 只作同一 pass 内进程重启的跳过提示,新 pass 全量重扫。
5. P3:P2 全部分片结束之后执行 §2.3。
6. `purge.json` 由清单与各 pass 的 ndjson 汇总生成(不依赖上一个进程写的文件);停止时与正常结束时都打印分族计数。

**verify(判定参数 + `-kafka-cut -report -guild-rpc`,只读;计划从 `-archive-dir` 加载并按清单 SHA 校验)**

- 逐人对象 = 计划里的全部玩家(不再读 purge.json):action=purge 且 C'>0 的必须是 P3 后态,c_prime 为空的必须是 P2 后态;冷读产物按 §2.1 规则接受并计数。storage_elsewhere 的映射与落点逐字节等于计划值(有效落点仍≠Z,复查 [0]);home_elsewhere 只要求不进入 home=Z 集合、有效落点仍≠Z(由下面的 H 比对与 P 复核覆盖),它们的映射与落点可以因他区自己的运维变化,不按字节比。
- 一趟全量扫描(§2.7)分发 H / P / merged_into / `consumer:applied:<topic>:*` / guard / 本 rid 隔离区:
  - home=Z 玩家集合必须与计划里 home=Z 的行(**全部类别**,含 action=skip 的 storage_elsewhere)逐人相同,多出或缺少 → 3(复查 [0]);
  - P 与 merged_into 按停服前硬拒规则复核,不过 → 3(复查 [3]);此时已过 Step 5,报告列出涉及的玩家与运维 run,处置是先撤销那次运维、把落点 / 映射恢复到计划值再重跑 verify(§5 第 19 条);
  - 区级:本 topic 各重试族与死信里只剩 `p1-routed` 记为保留的条目(逐条解码均为范围外);暂存键为空;孤儿 processing 键不存在;实例登记与模式键不存在;guard 键两种显式模式 `kafka:retry:guard:claim:kafka:retry:processing:<topic>:*` 与 `…:ack:…`(前缀逐字比较)计数为 0;`consumer:applied:<topic>:*` 用 `kafka.ParseAppliedCursor` 解析,v2 游标逐分区 seq≤切点,旧格式与各分区切点的最大值比较。
- 补投桶里没有任何 action=purge 的玩家;两表行围栏等于 V、戳等于 G。
- 各玩家表 `COUNT(*)` == inventory 计数 + 计划的 `expected_inserts`(只由计划推导,不读运行记录,复查 [8])。
- K1/K2 仍在;帮会分歧闸流式重判一次,`asset-divergence.ndjson` 与 `asset-unprovable.ndjson` 的 SHA-256 与行数必须等于确认文件所记。
- 任一项不过 → 3;写 `verify.json`,清单记 `verify_passed_at`。

**quarantine-drop(人工复核之后;`-confirm`,只依赖 Redis)**

- 前提:清单记有 `fence_released_at` 与 `verify_passed_at`;否则 3。
- 按清单列出的隔离键逐个 UNLINK(大键异步释放),清单记 `quarantine_dropped_at`;清单本身保留(小 HASH,审计用)。PEXPIRE 720h 只作远期兜底。

**Lua 契约(scripts.go / purge_scripts.go / quarantine.go / strict_scripts.go;KEYS 顺序由测试钉死)**

- `leaseScript`:KEYS[1]=`rollback:quarantine:<z>:<rid>:lease`;ARGV[1]=token,ARGV[2]=期望 run_id,ARGV[3]=PX(60000)。先 INFO server 核对 run_id;键不存在则 `SET NX PX`,等于 token 则 PEXPIRE,否则 `lease_held`。每 20s 续期,失败即整体中止(1)。逐人 Lua 只核对 token,不再调 INFO。
- `fenceAcquireScript`:KEYS = K1、K2、清单、租约;ARGV = 值、token、rid、zone、topic、世代。租约不符 → `lease_lost`;清单存在而 rid / zone / topic / 世代不一致或已有 `fence_released_at` → `foreign_fence`;K1、K2 各自:不存在 → SET(不带过期);恰为我方值且 PTTL=-1 → 已持有;否则 → `foreign_fence`。首次获取 HSET 清单字段并 `HSETNX fence_acquired_at <TIME>`。返回 1 获取 / 2 已持有。
- `fenceReleaseScript`:语义见 §2.1;返回 0 已释放 / 1 刚释放 / 2 中途丢失。
- `purgePlayerScript`:KEYS[1] K1,[2] K2,[3] 租约,[4] `player:zone:<pid>`,[5] `player:placement:<pid>`,[6] `player:<pid>:owner_epoch`,[7] `consumer:applied_epoch:<topic>:<pid>:player_database`,[8] 同 `player_database_1`,[9] `player:<pid>:location`,[10] `player:<pid>:handoff`,[11] `PlayerAllData:<pid>`,[12] `player:{<pid>}:asset_sql_commit`,[13] `player:{<pid>}:save_barrier`,[14] `player:save_pending:<z>:<pid%64>`,[15] `player:<pid>:sync_digest`,[16…] 各玩家表缓存 `<表>:<pid>` 与 `BagAllData` / `QuestAllData` / `MailAllData:<pid>`。ARGV:K1 值、token、home 原值、落点原值(空 = 必须不存在)、owner_stop、C'(空 = 不抬)、两个 applied 计划值、location 计划值、父快照 sha1 / 状态、资产证据 sha1、屏障 sha1、V、pid 十进制。全部检查先于第一次写:租约;K1/K2;映射与落点逐字节等于计划值;location 等于计划值或已不存在;handoff 不存在;状态 ∈ {计划前态, 计划后态};证据 sha1(字符串 `redis.sha1hex(GET)`,HASH 按字段名排序后 `长度:字段长度:值` 拼接再 sha1,不论屏障有几个字段)等于计划值,PTTL=-1 而不等 → `archive_mismatch`;抬升前置检查。写:抬升;UNLINK 各族(含 PTTL>0 冷读产物);ZREM 本人;返回分族计数。错误字:`lease_lost`、`fence_lost`、`route_changed`、`owner_advanced`、`applied_ahead`、`archive_mismatch`。
- `stageSourceScript`:KEYS = 源键、暂存键、清单、租约;ARGV = token、清单字段名 `staging:<N>:<L>`。租约不符 → `lease_lost`;源键不存在 → 0(已改名或本来没有);暂存键已存在 → `staging_exists`;否则 HSET 清单字段后 RENAMENX,返回 1。先登记后改名在同一段 Lua 里,中断后重跑只按清单找暂存键。
- `quarantineRouteScript`:KEYS = 暂存键、隔离目标、保留目标、租约,死信时另加源索引与隔离索引;ARGV = token,然后每条 `(期望条目字节, 去向 q|k, 字段, 指纹)`。逐条 `LINDEX 暂存 -1` 必须等于期望字节(否则 `staging_changed`),`RPOP` → `LPUSH` 去向;死信去 q 时迁移索引字段;最后源死信 LLEN==0 则 DEL 源索引。
- `strictPublishScript`:见 §2.3 第 4 步。

### 2.5 键族处置表(键名全部出自导出函数,工具里不写字面量)

| 键族 | 定义处 | 处置 |
|---|---|---|
| `PlayerAllData:<pid>` | `kafka.PlayerTableCacheKey`(go/db `buildCacheKey` 的导出包装)+ proto 描述符全名,与 login `buildParentKey`、data_service、match 同一拼法 | sha1 复核后 UNLINK;带 pending / outbox 的先归档 |
| `<玩家表>:<pid>`、`BagAllData` / `QuestAllData` / `MailAllData:<pid>` | 同上;三个辅助缓存名取自 proto 描述符,列表与 merge_zone `defaultCacheMsgTypes` 一致,由 purge_test 钉死(不新建 shared 包:没有第二个使用方) | UNLINK,不归档 |
| `player:{pid}:asset_sql_commit` | `assetop.SQLCommitKey` | 归档后 UNLINK |
| `player:{pid}:save_barrier` | `savebarrier.Key` | 归档后 UNLINK;P3 按 §2.3 重建(三个版本字段加 schema_gen) |
| `player:save_pending:<z>:<b>` | 新 `saveoutbox.PendingIndexKey` / `BucketOf` | ZREM 本人 |
| `player:<pid>:sync_digest` | `playersync` 的键函数 | UNLINK |
| `player:<pid>:owner_epoch` | `ownerepoch.OwnerEpochKey` | 只按 §2.2 抬升 |
| `consumer:applied_epoch:<topic>:<pid>:<表>` | 新 `kafka.AppliedEpochKey` | 只按 §2.2 抬升 |
| `consumer:applied:<topic>:*` | `kafka.AppliedSeqKeyPrefix` / `ParseAppliedCursor` | 不碰,verify 与切点比较 |
| `player:<pid>:handoff` | `ownerepoch.HandoffKey` | 不碰,等待过期 |
| `player:<pid>:location` | `scenenode.LocationKey` | 不碰,判在线 |
| ready、`:p<n>`、processing[`:inst`] | 新 `kafka.Retry*Key` | P1 逐条分流:范围内进隔离区,其余回共享收件箱 |
| `kafka:dead:queue:<topic>`、`kafka:dead:index:<topic>` | `kafka.RetryDeadKey`、新 `kafka.RetryDeadIndexKey` | P1 逐条分流,索引字段随条目迁移 |
| `kafka:retry:instances:<topic>`、`kafka:retry:instance_mode:<topic>` | `kafka.RetryInstancesKey` / `RetryInstanceModesKey` | 整键进隔离区 |
| `kafka:retry:guard:*` | `kafka.RetryGuardScanPatterns(topic)` | 不碰;verify 断言两种显式模式计数为 0 |
| `distributed:lock:kafka:ordering:<topic>:*` | 新 `kafka.OrderingLockKeyPrefix` | 只读,stopped 要求没有存活的锁 |
| `player:zone` / `placement` / `maprev`、`merge:merged_into:*`、`account:*`、`player_to_account:*`、data_service blob、会话与锁 | `placement.*` 等 | 只读 |
| K1 `merge:in_progress:<z>`、K2 `player:save_barrier:maintenance:<z>` | `placement.MergeFenceKey`、`barrierrestore.MaintenanceKey` | 只由 fence / release 写 |
| `db:save_protocol:zone:<z>` | `saveoutbox.GenerationKey` | 只读 |
| `scene_nodes:zone:<z>:load`、`node:zone:<z>:<n>:death_at`、`scene:<id>:zone` | 新 `scenenode.NodeLoadKey` / `NodeDeathAtKey` / `SceneZoneKey` | 只读 |
| `rollback:quarantine:<z>:<rid>:{manifest,lease,<N>:…,<N>:staging:…}` | zonerollback | 本工具写;暂存键先登记进清单再改名;quarantine-drop 释放数据键,清单保留 |

全量扫描一律不带 MATCH(MATCH 在服务端逐键过滤,不降成本),客户端按前缀分发:只采纳前缀逐字相同、且紧跟族边界的键(按实例或按键是 `:`,按分区 ready 是 `:p`;db_task_zone_1 不命中 db_task_zone_11 / db_task_zone_1_g2);cursor 走到 0;页出错按 §2.7 有界重试同一 cursor,仍失败即 1。P1 的源键按已知键名枚举,不扫描。

### 2.6 归类真值表(classify.go,纯函数 switch;`Class` 是带 `classCount` 哨兵的枚举加名字表,测试断言名字表长度 == classCount)

前置:映射与落点可解析;落点 frozen → frozen。

| home | 有效落点 | 本库有行 | 落点版本 | 类别 |
|---|---|---|---|---|
| Z | Z | 齐且围栏一致 | ≤1 或无 | rollback |
| Z | Z | 齐且围栏一致 | ≥2 | rollback,要求搬库证明,否则 topology_unproven |
| Z | Z | 缺一张或围栏不一致 | — | torn(版本≥2 时同样要证明;落后表带业务列 → torn_cross_table) |
| Z | Z | 无 | ≤1 或无 | post_t_candidate(命中拓扑清单 player_ids → moved_in_after_t) |
| Z | Z | 无 | ≥2 | moved_in_after_t |
| Z | ≠Z | 有或无 | — | storage_elsewhere(跳过;计划 action=skip,verify 复核映射与落点未变) |
| ≠Z | Z | 有或无 | — | foreign_home_storage_here(blocked;停服前已硬拒,此处兜底) |
| ≠Z | ≠Z | 有 | — | home_elsewhere(跳过,冷副本;计划 action=skip) |
| 缺 | — | 有 | — | legacy_owned(blocked;停服前已硬拒,修复入口 merge_zone `-backfill-home-zone` 须在立栏之前跑) |

- 归类后对 rollback / torn / post_t_candidate 做 U(p) 判定,不过 → owner_behind(blocked,修复入口 fencing §9.1 epoch_raise)。
- 其余畸形与 strict 预检判不出的形状(§2.3,含两行 (0,0) 的未围栏行、两行戳不同而围栏相等)一律 unclassified。blocked = legacy_owned、owner_behind、frozen、moved_in_after_t、topology_unproven、foreign_home_storage_here、torn_cross_table、unclassified。

### 2.7 水平扩展、并发、限速与断点续跑

- **无全局单点**:状态都带 zone 与 rid 前缀(隔离键、清单、租约、报告目录),多个 zone 可以并行回档;同一 zone 与合服互斥靠 K1 的 SetNX 语义;同一 rid 的写模式互斥靠写者租约(复核 6.1.11)。共享 Redis 是单实例,`-max-concurrent-rollbacks`(缺省 2)是容量准入,不是正确性锁。
- **分片**:固定 shard=pid%64(与补投桶同模),写进计划与清单;以后加 `-shard i/n` 多进程只需按分片取租约,格式不变。
- **Redis 限速**(复核 6.2.1):全局令牌桶按本次调用触及的键数计(MGET 的键数、Lua 的 #KEYS、SCAN 的 COUNT),缺省 20000/s;在途字节 ≤ `-max-inflight-bytes`;每秒一次 PING 测 RTT,滑动 p99>20ms 即暂停 200ms 起、翻倍至 5s,恢复后清零;SCAN COUNT 500、页间 ≥2ms;worker 缺省 4、上限 8。
- **全量扫描**(复查 [4]):需要全键空间信息的模式各做**一趟**不带 MATCH 的 SCAN,客户端按前缀分发给各自的消费者,遍数如实如下:
  | 模式 | 趟数 | 本趟分发的族 |
  |---|---|---|
  | preflight | 1 | H、P、merged_into、in_progress(准入)、孤儿 processing |
  | fence | 2 | 立栏沉降后一趟、排空完成前一趟,各含 H、P、merged_into、in_progress |
  | stopped | 1 | H、P、merged_into、排序锁前缀 |
  | inventory | 1 | H、P、merged_into、孤儿 processing |
  | purge | 0 | 源键按已知键名;孤儿 processing 取自计划;暂存键取自清单登记 |
  | verify | 1 | H、P、merged_into、`consumer:applied:<topic>:*`、guard 两种显式模式、本 rid 隔离区 |

  合计 6 趟。每趟耗时 ≈ DBSIZE / `-redis-keys-per-sec`:全服 3e7 个键、缺省 20000/s 时约 25 分钟,stopped / inventory / verify 三趟在停服关键路径上(§5 第 11 条)。v2 写的「7 次」漏数了按 MATCH 分开的扫描,按正文实际约 18 趟。成本随全服键数而非本 zone 规模增长,这是共享单实例 Redis 的固有代价;preflight 按 DBSIZE 估算,超预算即拒(§2.4)。
- **不复用 fence 的快照**:K1 挡住新的 home=Z,但不改 merge_zone 之后,他区归属玩家搬进 Z 库会让 P 集合增长,所以 stopped 与 inventory 各自重扫一趟,并按停服前硬拒规则复核;这是收窄的代价(§6.3)。
- **扫描续跑**:扫描的部分结果按批追加进报告目录的 `scan-<mode>-<趟>.ndjson` 并 fsync,之后才把 `{cursor, run_id, mode, rid, 趟}` 写进同目录的 `scan-<mode>-<趟>.ckpt`。同一 rid 重跑时,INFO 的 run_id 与检查点一致即从 cursor 续扫(SCAN 的 cursor 不在服务端留状态,同一实例上续扫仍满足「全程存在的键至少返回一次」);不一致或检查点缺失即从头扫。检查点只在本机:丢了只是重扫,不影响正确性,只读模式也因此不必写 Redis。`-drain-wait` 只计排空等待,不计扫描(§2.1 fence 第 9 步)。
- **持久副本**(复查 [9]):停服基线(stopped)、计划分片与 plan.json、inventory.json、`asset-divergence.ndjson` / `asset-unprovable.ndjson`、确认文件(inventory)以及 P2 的证据归档(purge)都同时写进报告目录与 `-archive-dir/<rid>/`,两边 fsync 之后才算写成;各文件 SHA-256 记进 Redis 清单(每个文件一个字段,小 HASH)。purge 与 verify 一律从 `-archive-dir` 加载计划并逐文件校验 SHA,缺文件或不符 → 3,所以 purge 写第一笔之前持久副本必然已核对。操作者本机丢失后,换一台机器给同一个 `-archive-dir` 与 rid 即可续跑 purge / verify(报告目录里的扫描检查点与日志丢了只是重扫)。`-archive-dir` 必须是操作者保证持久、可从其它机器访问的位置(网络共享或对象存储挂载点;仓库 infra 里没有对象存储,§7 第 2 条);ps1 核对它存在、可写且不在报告目录之下。
- **MySQL 限载**(复核 6.2.5):`SetMaxOpenConns(2)`;校验和单连接串行、块大小自适应;每块之前 `SHOW GLOBAL STATUS LIKE 'Threads_running'`>16 即暂停 1s 再查;TiDB 会话 `SET SESSION tidb_distsql_scan_concurrency = 1`;校验和只在 stopped 与 inventory 各算一次,块摘要写进 inventory.json,purge / verify 不重算。
- **超时**:Redis 单次 = 2s + 5ms×本批键数,上限 10s;SQL 单块 30s、P3 事务 5s;整体 `-timeout`;一律单调时钟截止。
- **有界重试**(复核 6.2.6):网络类错误(连接断开、超时、EOF;不含 Lua error_reply)对 SCAN(同一 cursor)、只读 SQL、逐人「读 → 归档 → Lua」最多重试 3 次,首次 100ms、×2、±20% 抖动;逻辑拒绝字即整体停。
- **断点续跑**:写入靠幂等重放加计划状态判定;扫描靠本机检查点续扫,run_id 变了即从头(上一条)。每次 purge 开新一轮 pass:UNLINK 已删的键是空操作,RENAMENX 源键不在就跳过,暂存键按清单登记先排空,逐人 Lua 接受计划后态,P3 接受同值;基线有 .partial 可续算;NDJSON 追加并 fsync,重复条目无害;进程内存不保存任何关乎正确性的状态(范围成员集合由持久副本里的计划推出)。

### 2.8 失败语义、退出码与告警

| 码 | 含义 |
|---|---|
| 0 | 通过 |
| 1 | 出错,可能已做了一部分;修好后用同一 rid 重跑,幂等 |
| 3 | 拒绝,本次调用什么都没写 |
| 4 | 目标看起来没在用(只 preflight,`-allow-empty-target` 放行) |
| 5 | 只 fence:围栏已立但未就绪(排空超时,或立栏后复核未过),围栏保留;同 rid 重跑接着等,或 release 后走中止路径 |
| 6 | inventory 有 blocked,或确认文件缺失 / 不符;purge 遇到时返回 3 |
| 7 | 只 release:围栏中途丢失(K1 / K2 在 release 之前已缺失、被改或被加 TTL);仍属我方的已删除,后续模式以 fence_lost 拒绝 |

- 其它值按出错处理。写 Lua 的错误字原样打印并写进报告。逐人 Lua 出现逻辑拒绝字即停止整次运行:本次已写过任何东西 → 1,否则 → 3。Step 5 之后的失败只能向前,ps1 打印单步命令。
- 新告警(scene-manager-alerts.yaml,`scene-manager.node-role-split` 组,紧随 `SceneManagerHomeZoneUnmappedTravel`):`SceneManagerHomeZoneFenceHeldTooLong`,`sum by (zone_id) (increase(scene_manager_enter_scene_rejected_total{reason="home_zone_merging"}[30m])) > 0`,for 2h,warning。描述写明:zone_id 是 **gate zone**,用 `GET merge:in_progress:*` 定位被封的是哪个 zone;值以 `zone_rollback_discard:` 开头 → 回档已结束则执行 `zone_rollback_discard -mode release`,值以 `save_barrier_restore:` 开头 → 由 barrierrestore 撤除,**都不得手工 DEL**;只有 JSON 值(merge_zone)按合服 runbook 处理。没有可复用的旧规则:deploy/ 下没有 home_zone_merging 或 merge fence 规则,也没有 redis_exporter(deploy/k8s/README.md:1151 标为未做);能复用的只有指标本身(metrics.go:121-124 注释写明「窗口外持续非 0 = 围栏键残留」)。

### 2.9 与 k8s_zone_rollback.ps1 契约对齐(B3 同批改文件头与实现)

- **C1 模式与退出码**:`Get-DiscardToolArgs` 的 ValidateSet 加 `fence`、`release`、`quarantine-drop`,并按模式取公共参数子集(fence:`-redis -redis-db [-redis-password-env] -expect-run-id -zone -topic-generation -rollback-id -report-dir -topology-manifest-dir -max-concurrent-rollbacks`,外加 `-drain-wait -etcd`;release 与 quarantine-drop:`-redis -redis-db [-redis-password-env] -expect-run-id -zone -rollback-id -report-dir`,quarantine-drop 另加 `-confirm`;其余模式取全集);`Get-DiscardToolExitMeaning` 加 5、7;:172-174 现把 5 当出错,改为按上表解释。
- **C2 preflight(:183-187)**:K1/K2 不存在,或恰为 `zone_rollback_discard:<本 rid>` 且 PTTL=-1(续跑)都放行,其它值以 3 拒绝;他区在线只报告、不拒绝;删除 :187「维护围栏落地之前必须以 3 拒绝」;新增停服前硬拒项(foreign_home_storage_here、frozen、合服标记、在途搬库、legacy_owned、准入)、扫描预算与 `-target-time`。
- **C3 stopped(:188-193)**:「按 scene_hot_state.go 判据区分陈旧与他区在线」改为 §2.1 的存活判据(带 etcd);新增围栏复核、排序锁检查、停服前硬拒规则与 legacy_owned 复核;基线改为分块校验和(自适应块、记边界)+ 停服玩家快照 + 见证行,写进报告目录与持久副本目录;新增 `-target-time -etcd -archive-dir`。
- **C4 inventory(:194-196)**:比较对象改为分块校验和;新增 schema 校验、owner 复核、H / P 复核、新类别与 blocked 列表、#5 度量(含见证窗口 Δ)、#6 证明、`-max-post-t-ratio`、strict 预检、帮会分歧闸(流式)、计划与确认模板、持久副本;`-guild-rpc`。
- **C5 purge(:197-207)**:「先原样重跑 inventory 的全部判定」改为「从持久副本加载并校验计划与确认文件,逐人状态在写 Lua 里原子判定」;P1 由整键改名改为逐条分流;「绝不碰 consumer:applied*」改为「游标 `consumer:applied:<topic>:*` 不碰;owner_epoch 与本世代两表 applied_epoch 按计划只升不降抬到 C'」;删除前只归档证据(双写);工具写 SQL(torn 对齐与骨架行)并重建屏障(三个版本字段加 schema_gen);purge.json 由清单与 ndjson 生成;删去「-barrier-protocol legacy 时绝不写 save_barrier」。
- **C6 verify(:208-214)**:按计划后态逐人复核;一趟全量扫描(home=Z 集合与计划全部类别比对、P 复核、区级只剩保留条目、guard 键显式模式);行数按计划的期望插入核对;帮会分歧闸按 SHA 复判;去掉 `-purge-report` 输入与 legacy 的「补投桶为空」;新增 `-guild-rpc -archive-dir`。
- **C7 协议与 deploy-facts**:`-BarrierProtocol` 只认 strict。legacy 在入口拒绝,文案「本工具只支持严格协议;上线后如需 legacy 回档,按 zone_rollback_discard_tool.md §8 第 1 条补」;:1914 对 strict 的入口拒绝在 B3 **保留**,文案改为「回档工具的 strict 屏障重建尚未落码验证(R9 B2d)」,由 B2d 删除并改契约测试(复查 [1])。`Get-DeployFactsProblems` 改为 strict ⇔ dual(legacy 即拒)。Schema 3 在 Schema 2 之上只增加 `RouteStoreMode`、`RouteReadRequiresReady`(读自 go-svc-data-service-config,缺省 redis / false);scene-manager 的两项由 C8 全服核对覆盖,不进 deploy-facts;不加镜像字段(§8 第 4 条)。E4 增加:本机 db.yaml 的有效 Fencing.Mode 必须为 dual。不加 `Get-TargetBarrierProtocol` 的 git 核对与 `-GoSvcTag`(§8 第 3 条)。
- **C8 全服围栏读方核对**:`Read-GlobalFenceReaders` / `Get-GlobalFenceReaderProblems`(§2.1,只读两类 ConfigMap,不读镜像),第 0 步 f、0g 之前、stopped 之前、inventory 之前各一次。
- **C9 端口转发**:新增 `Start-InfraPortForward -Service <etcd|guild> -RemotePort <2379|50300>`(`kubectl port-forward -n <infra> svc/<名> :<端口>`,同一组 -KubeContext / -KubeConfig,等到 `Forwarding from 127.0.0.1:<端口>` 行,15s 超时)与 `Stop-InfraPortForward`(finally 中停止);fence、stopped 用 etcd,inventory、verify 用 guild。端口转发由脚本自己起,集群身份由 kubectl 上下文绑定,工具侧不再另做身份核对。
- **C10 新参数**:`-TopologyManifestDir`、`-MaxPostTRatio`、`-DrainWait`、`-RollbackAcknowledgeFile`、`-MaxConcurrentZoneRollbacks`、`-ArchiveDir`(-Apply 必填:存在、可写、不在报告目录之下)、`-WitnessLag`(缺省 5m,上限 10m);dev_tools.ps1 透传 `-RollbackTopologyManifestDir`、`-RollbackMaxPostTRatio`、`-RollbackDrainWait`、`-RollbackAcknowledgeFile`、`-RollbackMaxConcurrentZoneRollbacks`、`-RollbackArchiveDir`、`-RollbackWitnessLag`。
- **C11 流程**:新增 0g(preflight 通过、写 entry-state 之后,Step 1 之前)与 6b(Step 6 成功之后);Step 7 先复跑 `-mode release`(必须 0)再打印清单(围栏已撤、staging / prod 的 `-GatewayRateLimit` 生效、MySQL 负载与 Kafka lag、`quarantine-drop` 的时机);中止与放弃路径提示末尾加 `<tool> -mode release`;Step 1b (ii) 失败文案改指 0g;0g 打印 guild 历练 PromQL;inventory 之后打印持久副本目录与 plan_sha256,Step 5 之后的单步命令一律带 `-archive-dir`;T 之后消息条数估计(`kafka_offset_reset.ps1 -ToDatetime <T>` dry-run 与日志末端相减,失败只告警);:415 / :1875 / :2498 一带的 blocked 文案改为新类别与各自修复入口,`-AcceptBlockedPlayers` 继续在入口拒绝;文件头 :17-18「owner_epoch 保留不动」改写;「两张玩家表」改为「OptionPrimaryKey=player_id 的全部表 ∩ 实际存在的表」;入口 E5 校验 rid、E6 协议、E7 `-ArchiveDir`。
- **C12 判定参数**(`$judgeArgs`):`-target-time -fingerprint-baseline [-acknowledge-no-post-t-writes] -max-post-t-ratio -witness-lag [-acknowledge-file]`,inventory / purge / verify 同一份。
- **C13 k8s_deploy.ps1**:zone-up 结束时(非 DryRun)`Show-ZoneRollbackFenceWarning` 经 kubectl exec 进 redis-0 只读 GET(写法同 `Test-RedisLiveMaxClients`),读不到只告警。k8s_deploy.ps1 与其契约测试当前有他人未提交改动(§3),动手前与持有方协调;C13 可单独顺延,不阻塞 B3 其余文件。
- 不改的契约:`-version` 的 40 位 `main.buildCommit`;rid 格式(只加子串禁令);隔离键名格式;续跑规则;MATCH 规则。

### 2.10 三个小项的实现

**(a) 死信去重**

- 去重字段与指纹(`go/db/internal/kafka/dead_dedup.go`,B1 先落纯函数,B4 接线):
  - `DeadDedupField(entry []byte) (field, fingerprint string, ok bool)`:v2(首字节 0x02,长度≥13)→ field = `hex(entry[1:13])`,fingerprint = 对 `entry[13:]` 解出的 DBTask 置 `RetryCount=0` 后 `proto.MarshalOptions{Deterministic:true}` 的 sha1hex;毒消息(0x03)→ field = hex(BE64(Offset+1)‖BE32(Partition)),fingerprint = sha1hex(BE32(len(Key))‖Key‖Value);v1、裸载荷、解不开 → ok=false。代码里 seq 一律是 offset+1(key_ordered_consumer.go:2667-2669、wrapRetryPayload :995-1002)。
  - 写入点、P1 隔离、死信工具共用这一个函数,保证「写进去的字段」与「从条目反推删除的字段」一致;L-04 计划给新条目加「原因 + 时间」信封之后,字段仍必须能从条目字节推出(db-task-store-unavailable-deferral.md §4.1 同批写明)。
  - `RetryDeadIndexKey(topic)` = `kafka:dead:index:<topic>`,权威定义在 dead_dedup.go。
- 去重脚本:
  - `deadEnqueueScript`:KEYS[1] 死信列表,KEYS[2] 索引;ARGV[1] 条目,ARGV[2] 字段,ARGV[3] 指纹。先 TYPE 预检(列表 none|list、索引 none|hash,否则报错一条不动);LLEN==0 而索引存在 → DEL 索引(孤儿);`HGET`:等于指纹 → `HSET` 同值(判重命中也产生一次会传播的写,WAITAOF 才覆盖到首份)返回 2;否则 LPUSH,原值存在(字段被别的指纹占用)→ 不动标记返回 3(collision),不存在 → HSET 返回 1。
  - `deadMoveScript`(收据搬进死信):KEYS[1] processing,KEYS[2] 死信,KEYS[3] 索引;ARGV 收据、替换载荷、字段('' = 不可去重)、指纹。TYPE 预检;收据不在 processing → 0;可去重且指纹命中 → HSET 同值、LREM 收据,返回 2;否则 LPUSH 替换载荷、LREM 收据,按有无旧值返回 3 / 1(先 LPUSH 后 LREM,保持现有安全性)。
- 七个死信写入点(核实 A1):

| # | 位置 | 处置 |
|---|---|---|
| ① | `persistPoisonMessage`(key_ordered_consumer.go:3205)入队层 | 入队层对 `DurableTargetDead` 用 `DeadDedupField(payload)` 自己推导字段,走 `deadEnqueueScript` |
| ② | `quarantineOrderingConflict` Kafka 分支(:3180,`persistBeforeAck`)| 同 ① |
| ③ | `deadLetterRetry`(:3130-3155) | 改走 `deadMoveScript`;返回 2 视为成功(现 `!moved` 当错误并打 DATA-LOSS,:3148-3150) |
| ④ | `moveClaimedRetryToDead`(:1569;`parseClaimedRetry` 的畸形、预算用完、旧格式、分区越界四支) | 同 ③;这里没有 workerTask,字段只从替换载荷字节推导 |
| ⑤ | `retryStrandedReceipts`(retry_stranded.go:121) | `strandedRetry` 增加 `dead bool`,死信目的地的重搬走 `deadMoveScript`;返回 2 时摘掉并计 duplicate_dropped,不调用 onMoved 里的死信计数 |
| ⑥ | `forwardRetryScript`(retry_routing.go:459-500)legacy / invalid 两支 LPUSH KEYS[2] | 不去重,Go 侧按脚本回报的条数计 undeduplicable |
| ⑦ | `requeuePartitionScript`(retry_routing.go:809-830,cmd/retry_requeue)非 v2 载荷进死信 | 同 ⑥ |

- `directEnqueue`(WAITAOF 关闭时)对死信目标同样走 `deadEnqueueScript`;go-redis 内部重发 EVAL 幂等。
- `aofGroupEnqueue`:批内死信目标改 EVAL,与其余 LPUSH 同一条粘连连接、同一个 pipeline,后面照旧 WAITAOF;份数不足走 `needPush=false` 分支只重发 WAITAOF(durable_enqueue.go:566-574),EVAL 只在 needPush 时发出;网络错误整批重推时判重命中分支照样产生传播写,WAITAOF 在新连接 / 新实例上也覆盖得到首份(核实 A3)。
- 回调带结果码(核实 A6、A8):`durableEnqueue.submit` 签名不加 dedupField 参数,回调改为 `done func(enqueueOutcome, error)`,`enqueueOutcome` 取 `enqueueStored` / `enqueueDuplicate`;实现 `directEnqueue`、`aofGroupEnqueue`、`confirmingEnqueue`(rejoin_backoff.go:137)与测试里的 `holdingEnqueue`(durable_enqueue_test.go:276)同改;调用方只在 `enqueueStored` 时计 `db_task_dead_letter_total`(否则 `DbTaskDeadLetterAdded` 每个重新入组周期都会再响一次)。
- 指标:`db_dead_letter_dedup_total{target,result}`,target ∈ {kafka, retry, route},result ∈ {enqueued, duplicate_dropped, collision, undeduplicable},启动时以 0 预建 12 条;`db_retry_queue_length{queue="dead_index"}`(`sampleRetryQueues`,store_failure.go:171-196,同一 pipeline 多一次 HLEN;`RetryQueueDeadIndex` 常量,标签集合保持封闭;stress_summarize 按精确字符串读其余四个值,不受影响)。
- 告警(db-task-retry-alerts.yaml):新增 `DbTaskDeadIndexOrphans`:`max by (namespace) (db_retry_queue_length{queue="dead_index"}) > max by (namespace) (db_retry_queue_length{queue="dead"})`,for 30m,warning;新增 `DbTaskDeadLetterCollision`:`sum by (namespace) (increase(db_dead_letter_dedup_total{result="collision"}[10m])) > 0`,for 0m,warning;`DbTaskDeadLetterAdded` 描述(:109)加「不许单独 DEL / LREM 死信,否则标记残留、同一消息以后再也进不了死信;删除必须用同段 Lua 按指纹 HDEL」。
- 文案:durable_enqueue.go:25-27 与 rejoin_backoff.go:19、:113 的「死信里的重复条目人工去重」改为「带 (原始分区, seq) 的条目每条 Kafka 消息至多一份;v1、裸载荷与补投器重发的不同 offset 不去重」。
- offset 复用的主防线:`kafka_offset_reset.ps1`:432-437 已拒绝同名重建 db_task topic;剩下手工删 topic 或清 broker 而不 FLUSHALL 的路径由 collision 计数与告警暴露。
- 合服后退役的源 topic:merge-zone-runbook 加一步——P4 确认源 topic 死信 LLEN==0 之后,用 runbook 给出的一句 EVAL(`if LLEN(dead)==0 then DEL index end`)原子删除残留索引。

**(b) 剥离探针**

- `last_persisted_snapshot_comp.h` 的 `Replace`:在 `snapshot->clear_last_save_seq();` 之后加 `if (snapshot->has_player_database_data()) snapshot->mutable_player_database_data()->clear_stress_test_probe();` 与 `if (snapshot->has_player_database_1_data()) snapshot->mutable_player_database_1_data()->clear_stress_test_probe();`;不得用 `mutable_stress_test_probe()->Clear()`(保留 presence)。生产中两个子消息都存在(MarshalPlayerForSave 两边都 set_player_id),目前不触发单边 presence。
- 同步改注释:player_lifecycle.cpp 的 R2 注释(锚点「MUST run BEFORE stresstest_probe::Stamp*」,前提「只在写盘侧有探针」不完整,快照侧也带)与锚点「快路径是否剥离 stress_test_probe 待负责人定」一行(改为「快照侧在 Replace 里剥离」);last_persisted_snapshot_comp.h 文件头「holds the exact bytes」与 Replace 注释;player_lifecycle.h 锚点「player_database_1 的 stress_test_probe 剔掉」一段(:650-656)。
- 快路径(锚点 `dirty_save::IsEqual(*message, *snap->snapshot)`)不改。新消息在比较时永远不带探针(Stamp 在快路径之后),剥离只会让「仅探针不同」判相等;全仓 C++ 没有代码读快照里的 stress_test_probe;`IsPersistedPayloadCurrent` 本来两侧都剥。探针仍起作用:NextSeq 只在真正盖章时推进,verifier 只比 MySQL 与缓存的 test_seq。
- 影响面:deploy、tools/scripts、robot 配置都没有设置 `STRESS_TEST_PROBE`,常规压测(含 R20)关探针跑,跳过率与 DBTask 量不受影响;只影响手工开探针的 L3 数据一致性压测(data-consistency-stress-testing.md:252-263、375),复盘口径说明只针对这类压测。

**(c) op="other" 告警**

- db-task-partition-alerts.yaml 的 `db-task.partition-contract` 组新增 `DbTaskUnknownOp`(表达式见 §1),文件头 :5-8 补一句「op=other 由 DbTaskUnknownOp 报」。描述:按 go/db 日志 `[DATA-ORDERING] unknown producer op` 一行(带 topic / partition / offset / kafkaKey / taskID / msgType / op)找生产者;并行信号 `db_task_deferred_total{op="unknown"}`(store_failure.go:118-123)与约 3 次重试后的 `DbTaskDeadLetterAdded{reason="budget_exhausted"}`(处理路径:`runDBOp` 报 unsupported op,key_ordered_consumer.go:2865 → deferFailedTask → `parseClaimedRetry` 只放弃读,其余进死信)。
- `observeProducerPartition`(producer_partition_audit.go):`op == ProducerPartitionOpOther` 时不论 verdict 都调新函数 `logProducerPartitionUnknownOp`,共用现有每进程 20 条的额度(下标 2)。合法生产者只发 read / write(C++ player_lifecycle.cpp 的 `set_op("write")`、login sync_loader.go:100 的 "read"、data_stress 的 "write")。

### 2.11 Java 版

不适用:Java 版没有 Kafka 存盘链与 go/db 键族。B6 在 Java 仓 `D:\luyuan\xuanming-server-mmo\PARITY.md` 追加一行「R9 zone_rollback_discard(含死信去重、探针剥离、op=other 告警):不适用」,本仓 PROGRESS 条目注明。

## 3. 落码批次(共 109 个文件:新增 58、修改 51、删除 0;9 批)

| 批 | 计入总数 | 新增 | 修改 |
|---|---|---|---|
| B1 | 14 | 10 | 4 |
| B2a | 26 | 26 | 0 |
| B2b | 10 | 10 | 0 |
| B2c | 7 | 7 | 0(另改 B2a 的 ledger.go、integration_test.go) |
| B2d | 6 | 4 | 2(另改 B2b / B2c 的 inventory.go、verify.go,B2a 的 main.go、main_test.go,B3 的 k8s_zone_rollback.ps1、k8s_zone_rollback_discard.tests.ps1) |
| B3 | 6 | 0 | 6 |
| B4 | 15 | 1 | 14(另改 B1 的 dead_dedup.go、dead_dedup_test.go) |
| B5 | 8 | 0 | 8 |
| B6 | 17 | 0 | 17 |
| 合计 | **109** | 58 | 51 |

依赖:B1 → B2a → B2b → B2c;B3 依赖 B2a 定稿的 CLI 契约(参数名、模式、退出码),可与 B2b、B2c 并行;B2d 依赖 B2c 与 B3,它同时删掉工具的接线闸与 ps1 入口对 strict 的拒绝,是回档 `-Apply` 唯一的放行批(复查 [1]);B4 依赖 B1(`DeadDedupField`、`RetryDeadIndexKey`),与 B2 无文件重叠;B5 无依赖;B6 在 B2d、B4、B5 落码并验证之后(文档以代码为准)。各批表头的「修改」若是本次前面批次新增或已计入的文件(见上表括号),不重复计入总数。

**文件归属**(复查 [6]):截至 2026-10-19 HEAD c63b22dca(v2 写的 e611b5d8d 与复查意见写的 85a457a55 都已过时),本设计要改的文件里有他人未提交改动的是:`tools/scripts/k8s_deploy.ps1`、`tools/scripts/tests/k8s_deploy_contract.tests.ps1`(B3 C13),`docs/design/db-retry-owner-routing.md`、`docs/design/shared-redis-split-by-purpose.md`、`docs/design/mixed-version-persist-field-protection.md`、`docs/ops/godb-pipeline-launch-todo.md`、`docs/ops/merge-zone-runbook.md`、`docs/PROGRESS.md`(B6;PROGRESS 是 union 合并,只追加)。这些文件动手前先与持有方协调,持有方未合入时只做本项最小增量、不覆盖对方改动;B3 的 C13 可单独顺延。其余文件工作区干净。每批动手前仍按当时 HEAD 重新 `git status` / `git diff` 核对,只做本项最小增量。

Codex 公共环境:`GOTOOLCHAIN=local`、`GOFLAGS=-mod=readonly`、`GOPROXY=https://goproxy.cn,direct`;真库用例自起一次性容器(Redis ≥7.2 且 `appendonly yes`、MySQL 8.x、TiDB 用 `deploy/docker-compose.tidb.yml`),不借用 codex-* 容器;任何 SKIP 不算通过;失败保留 `go test -v` 首个失败用例输出、ps1 的全部 [FAIL] 行、MSBuild 首个错误与真库容器日志,不改断言、不跳过测试、不连续自动重试。

### B1 共享导出、纯函数与 guildcheck 迁移(行为不变;依赖:无;14 个:新增 10、修改 4)

| 文件 | 类型 | 模块 | 内容 |
|---|---|---|---|
| go/shared/scenenode/liveness.go | 新增 | shared | `NodeLoadKeyFmt`/`NodeLoadKey`、`NodeDeathAtKeyFmt`/`NodeDeathAtKey`、`SceneZoneKeyFmt`/`SceneZoneKey`;`LocationVerdict` 枚举与名字表;`ClassifyLocation`;`RegistrySnapshot` 与 `ReadRegistrySnapshot(ctx, *clientv3.Client)` |
| go/shared/scenenode/liveness_test.go | 新增 | shared | §2.1 判据表逐行向量;快照解析 |
| go/shared/guildcheck/guild_divergence.go | 新增 | shared | 自 data_service 迁入实现(包名不变);导出原先未导出的跨服务契约常量 `RetentionRejectedMessagePrefix`、`RetentionClampSlackMs`;新增 `NewStreamer` 与 `StreamDivergences(ctx, sinceMsByPlayer, emit func(GuildDivergence) error) (StreamSummary, error)`:按 player_id 分块升序、块内 op_id 升序逐行回调,不设行数上限(复查 [5]);`ListDivergences` 改为在它之上收集后按 op_id 排序,保留 `MaxDivergenceRows`=10000 与 `ErrTooManyDivergences`,对外行为不变 |
| go/shared/guildcheck/guild_divergence_test.go | 新增 | shared | 只用标准库 `testing`(shared 不引入 testify,go.mod / go.sum 不动,复查 [6]):流式与 ListDivergences 逐行一致;超过 10000 行时流式照常、ListDivergences 报 ErrTooManyDivergences;emit 返回错误即停、不再发 RPC |
| go/data_service/internal/guildcheck/guild_divergence.go | 修改 | data_service | 改为薄转发包:类型别名(`GuildDivergence`、`GuildCheckResult`、`GuildDivergenceChecker`)、常量与错误变量转发(含原测试用到的未导出 `retentionRejectedMessagePrefix`、`retentionClampSlackMs`)、`New` / `ParseRetentionCutoffMs` 转调;包注释写明转发原因与撤除条件(§7 后续)。原测试 `guild_divergence_test.go` 与五个调用方(rollback_logic.go、rollback_recall_test.go、rollback_merge_fence_test.go、dataserviceserver.go、servicecontext.go)一个不改,原用例经转发包跑在 shared 实现上 |
| go/scene_manager/internal/logic/load_reporter.go | 修改 | scene_manager | `NodeLoadKeyFmt = scenenode.NodeLoadKeyFmt` |
| go/scene_manager/internal/logic/reentry_barrier.go | 修改 | scene_manager | `NodeDeathAtKeyFmt = scenenode.NodeDeathAtKeyFmt` |
| go/scene_manager/internal/logic/createscenelogic.go | 修改 | scene_manager | `SceneZoneKeyFmt = scenenode.SceneZoneKeyFmt` |
| go/db/internal/kafka/keys_export.go | 新增 | go/db | `RetryReadyKey`、`RetryPartitionReadyKeyPrefix`、`RetryPartitionReadyKeys`、`RetryLegacyProcessingKey`、`RetryInstanceProcessingKey`、`RetryInstancesKey`、`RetryInstanceModesKey`、`RetryDeadKey`、`RetryGuardScanPatterns`、`AppliedSeqKeyPrefix`、`ParseAppliedCursor`(返回 partition / seq / hasPartition)、`AppliedEpochKey`、`PlayerTableCacheKey`(`buildCacheKey` 的包装,`<消息全名>:<pid>`,父快照与辅助缓存同样用它)、`OrderingLockKeyPrefix`、`EscapeRedisGlob`、`LuaDecimalGreaterFn`;只包一层,不改原函数 |
| go/db/internal/kafka/keys_export_test.go | 新增 | go/db | 钉字面量(含 `PlayerAllData:<id>`);topic 边界 |
| go/db/internal/kafka/dead_dedup.go | 新增 | go/db | `DeadDedupField`、`RetryDeadIndexKey`(纯函数) |
| go/db/internal/kafka/dead_dedup_test.go | 新增 | go/db | 字段与指纹推导 |
| go/db/internal/saveoutbox/keys_export.go | 新增 | go/db | `PendingIndexKey`、`BucketOf` |
| go/db/internal/saveoutbox/keys_export_test.go | 新增 | go/db | 钉字面量与 `pid%BucketCount` |

**交 Codex 的验证**

- 工作目录 `go/shared`:`go vet ./scenenode/ ./guildcheck/`;`go test -count=1 ./scenenode/ ./guildcheck/`(`GOFLAGS=-mod=readonly` 下必须不报缺依赖)。
- `go/data_service`:`go vet ./...`;`go test -count=1 ./internal/guildcheck/ ./internal/logic/... ./internal/server/... ./internal/svc/...`(guildcheck 原有用例经转发包原样全绿)。
- `go/scene_manager`:`go vet ./internal/logic/`;`go test -count=1 ./internal/logic/`。
- `go/db`:`go vet ./internal/kafka/ ./internal/saveoutbox/`;`go test -count=1 -run 'KeysExport|DeadDedupField' ./internal/kafka/ ./internal/saveoutbox/`;再整包 `go test -count=1 ./internal/kafka/ ./internal/saveoutbox/`。
- 仓库根:`rg -n "func \(c \*rpcChecker\)" go` 只命中 `go/shared/guildcheck/`(实现只剩一份);`git diff --stat go/shared/go.mod go/shared/go.sum` 无输出。
- 通过标准:vet 零告警;全部用例绿;原有用例无回归;rg 与 git diff 符合上条。

### B2a 工具骨架、围栏、拓扑、清单与只读模式(依赖:B1;26 个,全部新增,模块 go/db)

| 文件 | 内容 |
|---|---|
| go/db/cmd/zone_rollback_discard/main.go | flag、`-version`、退出码映射、模式分派;**接线闸**:除 release / quarantine-drop / `-version` 外一律 3(B2d 删除,复查 [1]) |
| go/db/cmd/zone_rollback_discard/main_test.go | `-version` 两行格式;未知模式;release 参数最小集;接线闸拒绝其余六个模式 |
| go/db/internal/zonerollback/target.go | deploy-facts Schema 3、Redis / MySQL 身份、现网协议判定(只认 dual + strict 标记)、`-barrier-protocol` 只认 strict、玩家表发现、rid 校验 |
| go/db/internal/zonerollback/target_test.go | |
| go/db/internal/zonerollback/preflight.go | preflight 模式;停服前硬拒规则(foreign / frozen / 合服标记 / 在途搬库 / legacy_owned / 准入);扫描预算 |
| go/db/internal/zonerollback/preflight_test.go | |
| go/db/internal/zonerollback/fence.go | fence / release 模式、两趟扫描与排空循环、`fence-drain.json` |
| go/db/internal/zonerollback/fence_test.go | |
| go/db/internal/zonerollback/stopped.go | stopped 模式 |
| go/db/internal/zonerollback/stopped_test.go | |
| go/db/internal/zonerollback/scan.go | 一趟不带 MATCH 的全量扫描与前缀分发、本机检查点续扫、限速、location 批量读与判据调用 |
| go/db/internal/zonerollback/scan_test.go | |
| go/db/internal/zonerollback/fingerprint.go | 自适应分块校验和、停服玩家快照、见证行 |
| go/db/internal/zonerollback/fingerprint_test.go | |
| go/db/internal/zonerollback/report.go | 原子落盘、NDJSON fsync、权限、规范 JSON 比较(确认文件)、报告目录与持久副本目录双写 |
| go/db/internal/zonerollback/report_test.go | |
| go/db/internal/zonerollback/scripts.go | `leaseScript`、`fenceAcquireScript`、`fenceReleaseScript` |
| go/db/internal/zonerollback/throttle.go | 令牌桶、在途字节、PING 自适应退避、有界重试、按批超时 |
| go/db/internal/zonerollback/throttle_test.go | |
| go/db/internal/zonerollback/lease.go | 写者租约取得 / 续期 / 丢失中止 |
| go/db/internal/zonerollback/lease_test.go | |
| go/db/internal/zonerollback/topology.go | (自 v2 的 B2b 前移,复查 [7])合服 / 撤销 / 搬库清单最小只读镜像、run_id 解析、player_ids 求交、At≤T−1s;preflight / fence / stopped 的硬拒与 inventory 的 #6 证明共用 |
| go/db/internal/zonerollback/topology_test.go | 与 merge_zone `mergeManifest` v1、搬库清单同形的样例以字符串常量内联(不另设 testdata 文件) |
| go/db/internal/zonerollback/ledger.go | (自 v2 的 B2c 前移,复查 [7])清单 HASH 的键名、全部字段常量与读写函数:rid / zone / topic / 世代、fence_acquired_at、fence_released_at、verify_passed_at、quarantine_dropped_at、各持久副本文件的 SHA;B2c 只补 pass、shard_done、staging 登记、分族计数与 purge.json 汇总 |
| go/db/internal/zonerollback/ledger_test.go | |
| go/db/internal/zonerollback/integration_test.go | 真 Redis(INFO in Lua、租约、围栏)+ 真 MySQL(校验和) |

本批实现 preflight、fence、release、stopped 四个模式(经包内接口;CLI 有接线闸)。清单的键名与字段只在 ledger.go 定义一份,fenceAcquireScript / fenceReleaseScript 与 preflight 的续跑判断都经它读写。

**交 Codex 的验证**(工作目录 `go/db`)

- `gofmt -l ./internal/zonerollback ./cmd/zone_rollback_discard` 无输出;`go vet ./internal/zonerollback/... ./cmd/zone_rollback_discard/...`。
- `go test -count=1 -race ./internal/zonerollback/... ./cmd/zone_rollback_discard/...`。
- 真库:`$env:ZONE_ROLLBACK_IT_REQUIRE='1'; $env:ZONE_ROLLBACK_IT_REDIS='127.0.0.1:<端口>'; $env:ZONE_ROLLBACK_IT_DSN='<一次性库 DSN,库名 zone_<id>_db>'; go test -count=1 -v -run 'Integration' ./internal/zonerollback/`(Redis 必须 DBSIZE=0)。
- 通过标准:全部绿;`-v` 输出里没有 SKIP;gofmt 无输出。

### B2b 盘点:归类、纪元、计划与帮会分歧闸(依赖:B2a;10 个,全部新增,模块 go/db)

| 文件 | 内容 |
|---|---|
| go/db/internal/zonerollback/classify.go | §2.6 真值表,`Class` 枚举加 `classCount` 与名字表 |
| go/db/internal/zonerollback/classify_test.go | |
| go/db/internal/zonerollback/raise.go | U(p)、C'、owner_behind 纯函数 |
| go/db/internal/zonerollback/raise_test.go | |
| go/db/internal/zonerollback/plan.go | 计划写入 / 读取(从 `-archive-dir` 加载并逐文件按清单 SHA 校验)/ 冻结校验、action=purge 与 action=skip 行、`expected_inserts` 汇总、状态判定表 |
| go/db/internal/zonerollback/plan_test.go | |
| go/db/internal/zonerollback/asset_gate.go | `guildcheck.StreamDivergences` 调用、since 计算、`asset-divergence.ndjson` / `asset-unprovable.ndjson` 与确认模板节(SHA、行数、按 kind 汇总) |
| go/db/internal/zonerollback/asset_gate_test.go | bufconn 假 GuildInternal |
| go/db/internal/zonerollback/inventory.go | inventory 模式(含 schema 校验、H / P 复核、#5 见证窗口、确认模板、计划与持久副本;strict 预检在 B2d 补) |
| go/db/internal/zonerollback/inventory_test.go | |

**交 Codex 的验证**(工作目录 `go/db`)

- `gofmt -l ./internal/zonerollback` 无输出;`go vet ./internal/zonerollback/...`。
- `go test -count=1 -race -run 'Classify|Raise|Plan|AssetGate|Inventory' ./internal/zonerollback/`;再整包 `go test -count=1 ./internal/zonerollback/... ./cmd/zone_rollback_discard/...`。
- 真库:同 B2a 的环境变量,`go test -count=1 -v -run 'Integration' ./internal/zonerollback/`。
- 通过标准:全部绿、无 SKIP。

### B2c purge、verify 与隔离区(P3 之前的部分;依赖:B2b;7 个,全部新增;另改 B2a 的 ledger.go、integration_test.go)

| 文件 | 类型 | 内容 |
|---|---|---|
| go/db/internal/zonerollback/ledger.go | 修改(B2a 已计) | 补 pass、shard_done、staging 登记、分族计数、purge.json 汇总 |
| go/db/internal/zonerollback/quarantine.go | 新增 | P1 逐条分流、`stageSourceScript`、`quarantineRouteScript`、quarantine-drop 模式 |
| go/db/internal/zonerollback/quarantine_test.go | 新增 | |
| go/db/internal/zonerollback/purge.go | 新增 | purge 模式(P1、P2;证据归档双写) |
| go/db/internal/zonerollback/purge_scripts.go | 新增 | `purgePlayerScript`;辅助缓存名列表(取自 proto 描述符) |
| go/db/internal/zonerollback/purge_test.go | 新增 | |
| go/db/internal/zonerollback/verify.go | 新增 | verify 模式(P3 之前的部分:逐人 P2 状态、home=Z 集合与计划全部类别比对、P 复核、区级、期望行数、帮会复判) |
| go/db/internal/zonerollback/verify_test.go | 新增 | |
| go/db/internal/zonerollback/integration_test.go | 修改(B2a 已计) | 加 purge(P1、P2)/ verify / 隔离区真库用例 |

完成后 CLI 仍被接线闸拒绝,不可用于回档;本批只经包内接口与真库用例验证。

**交 Codex 的验证**(工作目录 `go/db`)

- `gofmt -l ./internal/zonerollback` 无输出;`go vet ./internal/zonerollback/...`。
- `go test -count=1 -race -run 'Ledger|Quarantine|Purge|Verify' ./internal/zonerollback/`;再整包。
- 真库:同 B2a,`go test -count=1 -v -run 'Integration' ./internal/zonerollback/`。
- 通过标准:单测与真库全绿、无 SKIP。

### B2d strict 屏障重建与放行(依赖:B2c、B3;6 个:新增 4、修改 2;另改 B2a / B2b / B2c / B3 已计入的 6 个文件)

| 文件 | 类型 | 内容 |
|---|---|---|
| go/db/internal/zonerollback/strict.go | 新增 | P3:按计划执行 SQL 规范化(torn 对齐围栏与戳、骨架行)、行锁下复核形状、重锁复核 |
| go/db/internal/zonerollback/strict_scripts.go | 新增 | `strictPublishScript`(三个版本字段加 schema_gen) |
| go/db/internal/zonerollback/strict_test.go | 新增 | |
| go/db/internal/zonerollback/strict_integration_test.go | 新增 | 真 TiDB / MySQL + 真 Redis;内联 login 的 `beginParentLoadScript` 文本做冷读对照;整条链路经 CLI |
| go/db/internal/barrierrestore/restore.go | 修改 | 导出 `SchemaGenFloor(gens ...uint32) uint32`(0 按 1、取最大值),原 `schemaFloor` 改为调用它,行为不变(复查 [10]) |
| go/db/internal/barrierrestore/schema_gen_test.go | 修改 | 导出函数的用例 |
| go/db/internal/zonerollback/inventory.go | 修改(B2b 已计) | strict 只读预检,写计划的 `p3` 与 `expected_inserts` |
| go/db/internal/zonerollback/verify.go | 修改(B2c 已计) | P3 后态(字段白名单)、两表行围栏与戳 |
| go/db/cmd/zone_rollback_discard/main.go | 修改(B2a 已计) | 删接线闸 |
| go/db/cmd/zone_rollback_discard/main_test.go | 修改(B2a 已计) | 接线闸已删:六个模式不再因闸被拒 |
| tools/scripts/k8s_zone_rollback.ps1 | 修改(B3 已计) | 删 :1914 一带 B3 保留的 strict 入口拒绝 |
| tools/scripts/tests/k8s_zone_rollback_discard.tests.ps1 | 修改(B3 已计) | 断言改为 strict 不在入口被拒、legacy 仍被拒 |

**交 Codex 的验证**

- 工作目录 `go/db`:`gofmt -l`、`go vet` 同上;`go test -count=1 -race -run 'Strict' ./internal/zonerollback/`;`go test -count=1 -run 'SchemaGen' ./internal/barrierrestore/`;再整包 `go test -count=1 ./internal/zonerollback/... ./internal/barrierrestore/... ./cmd/zone_rollback_discard/...`。
- 真库:MySQL 一遍(`ZONE_ROLLBACK_IT_DSN` 指 MySQL),TiDB 一遍(另加 `$env:ZONE_ROLLBACK_IT_REQUIRE_TIDB='1'`,DSN 指 docker-compose.tidb.yml 起的 TiDB),`go test -count=1 -v -run 'StrictIntegration|Integration' ./internal/zonerollback/`;barrierrestore 的真库用例照其现有环境变量再跑一遍,确认导出后无回归。
- 仓库根:`pwsh -NoProfile -NonInteractive -File tools/scripts/tests/k8s_zone_rollback_discard.tests.ps1` 期望 fail=0;dry-run 冒烟加 `-BarrierProtocol strict` 不在入口被拒。
- 负载验收(自 B2c 移来:接线闸删掉之后才能经 CLI 跑;压测环境,AGENTS §6 流程):在一个 ≥10 万玩家的 zone 上跑 purge,同时 robot 对其它 zone 持续登录;以 `stress_summarize.ps1` 输出对比 purge 前后其它 zone 的 entergame p99,上升不超过 20%。超过即把 `-redis-keys-per-sec` 缺省值减半后重测,结论写进 `docs/stress/`。
- 通过标准:两种后端全绿、无 SKIP;ps1 fail=0;负载验收达标且有对比表。**B2d 通过之前,ps1 与工具都拒绝 strict,`-Apply` 不可用**。

### B3 编排脚本与部署脚本(依赖:B2a 的 CLI 契约;6 个,全部修改)

| 文件 | 模块 | 内容 |
|---|---|---|
| tools/scripts/k8s_zone_rollback.ps1 | 运维脚本 | §2.9 C1–C12 与文件头契约;strict 入口拒绝保留(B2d 撤),legacy 入口拒绝 |
| tools/scripts/tests/k8s_zone_rollback_discard.tests.ps1 | 运维脚本测试 | 见 §4 B3 |
| tools/scripts/tests/lib/k8s_zone_rollback_sandbox.ps1 | 运维脚本测试 | 假工具认识新模式、退出码 5 / 7、新参数;假 kubectl 认识 port-forward 与全服 ConfigMap field-selector 查询 |
| tools/scripts/dev_tools.ps1 | 运维脚本 | 透传 C10 新参数 |
| tools/scripts/k8s_deploy.ps1 | 部署脚本 | C13 `Show-ZoneRollbackFenceWarning`(动手前与持有方协调,§3 文件归属) |
| tools/scripts/tests/k8s_deploy_contract.tests.ps1 | 部署脚本测试 | 钉 zone-up 后的只读 GET 与 WARN 文案;DryRun 不执行 |

merge_zone 不改(v2 的 10 个文件移到 §8 第 5 条)。

**交 Codex 的验证**

- 仓库根逐个执行 `pwsh -NoProfile -NonInteractive -File tools/scripts/tests/<名>.tests.ps1`,名字为 `k8s_zone_rollback_discard`、`k8s_zone_rollback_gate_router_mode`(PATH 上要有 git)、`kafka_offset_reset_output_guard`、`dev_tools_kafka_offset_reset`、`k8s_deploy_contract`、`dev_tools_merge_zone_contract`;期望 fail=0。
- dry-run 冒烟:`pwsh -File tools/scripts/k8s_zone_rollback.ps1 -ZoneName test1 -ZoneId 101 -TargetTime "2026-05-15T14:23:00Z" -NodeImage ghcr.io/luyuancpp/mmorpg-node:<40 位 sha>`(不带 -BarrierProtocol);期望输出含 0g 的 `-mode fence`、6b 的 `-mode release`、Step 7 的 `-mode release`,打印的每一次工具调用都带 `-expect-run-id`,Step 5 之后的单步命令带 `-archive-dir`,打印的 deploy-facts 为 Schema 3。再分别加 `-BarrierProtocol strict` 与 `-BarrierProtocol legacy` 各跑一次,期望都在入口拒绝(strict 的文案指向 R9 B2d,legacy 的文案指向本设计 §8)。
- 通过标准:ps1 fail=0;dry-run 期望全部出现。

### B4 小项 a:死信去重(依赖:B1;15 个:新增 1、修改 14;另再次修改 B1 的 dead_dedup.go / dead_dedup_test.go)

| 文件 | 类型 | 模块 | 内容 |
|---|---|---|---|
| go/db/internal/kafka/durable_enqueue.go | 修改 | go/db | 死信目标走 `deadEnqueueScript`;回调结果码;文件头 :25-27 |
| go/db/internal/kafka/durable_enqueue_test.go | 修改 | go/db | 11 处调用点改签名;判重用例 |
| go/db/internal/kafka/key_ordered_consumer.go | 修改 | go/db | 写入点 ①–④;`deadMoveScript` 接线 |
| go/db/internal/kafka/key_ordered_consumer_test.go | 修改 | go/db | `deadLetterRetry` 与 `moveClaimedRetryToDead` 判重 |
| go/db/internal/kafka/rejoin_backoff.go | 修改 | go/db | `confirmingEnqueue` 回调签名;:19、:113 文案 |
| go/db/internal/kafka/rejoin_backoff_test.go | 修改 | go/db | 2 处调用点 |
| go/db/internal/kafka/retry_stranded.go | 修改 | go/db | `strandedRetry.dead`;死信重搬走去重脚本 |
| go/db/internal/kafka/retry_stranded_test.go | 修改 | go/db | 返回 2 时摘除且不计死信 |
| go/db/internal/kafka/retry_routing.go | 修改 | go/db | ⑥⑦ 计 undeduplicable |
| go/db/internal/kafka/retry_routing_test.go | 修改 | go/db | 计数用例 |
| go/db/internal/kafka/store_failure.go | 修改 | go/db | `sampleRetryQueues` 多一次 HLEN |
| go/db/internal/kafka/dead_dedup_integration_test.go | 新增 | go/db | 真 Redis 7.2:WAITAOF 与传播 |
| go/db/internal/metrics/metrics.go | 修改 | go/db | `db_dead_letter_dedup_total`、`RetryQueueDeadIndex` |
| go/db/internal/metrics/metrics_test.go | 修改 | go/db | 预建序列(照 `TestRetryRoutingSeriesPrecreated`) |
| deploy/k8s/db-task-retry-alerts.yaml | 修改 | 部署 | 两条新规则与 `DbTaskDeadLetterAdded` 描述 |

**交 Codex 的验证**(工作目录 `go/db`)

- `go vet ./internal/kafka/ ./internal/metrics/`。
- `go test -count=1 -race -run 'DeadDedup|DurableEnqueue|PoisonMessage|OrderingConflict|DeadLetterRetry|MoveClaimedRetryToDead|Stranded|ForwardRetry|RequeuePartition|RejoinBackoff' ./internal/kafka/`;再整包 `go test -count=1 ./internal/kafka/ ./internal/metrics/`。
- 真库:`$env:GODB_IT_REQUIRE='1'; $env:GODB_REDIS_ITEST_ADDR='127.0.0.1:<端口>'; go test -count=1 -v -run 'DeadDedupIntegration' ./internal/kafka/`(Redis ≥7.2,`appendonly yes`、`appendfsync everysec`)。
- 有 promtool 时 `promtool check rules deploy/k8s/db-task-retry-alerts.yaml`;没有则如实记「未跑」。
- 通过标准:全部绿、无 SKIP;promtool 0 错误。

### B5 小项 b、c:探针剥离与 op=other(依赖:无;8 个,全部修改)

| 文件 | 模块 | 内容 |
|---|---|---|
| cpp/libs/services/scene/player/comp/last_persisted_snapshot_comp.h | C++ scene | `Replace` 带 has_ 守卫剥离;文件头与 Replace 注释 |
| cpp/libs/services/scene/player/system/player_lifecycle.cpp | C++ scene | 只改注释(R2 一段与「快路径是否剥离」一行,按锚点) |
| cpp/libs/services/scene/player/system/player_lifecycle.h | C++ scene | 注释 :650-656 |
| cpp/tests/cross_zone_test/save_seq_test.cpp | C++ 测试 | `LastPersistedSnapshot.*` 单元用例 |
| cpp/tests/cross_zone_test/db_task_dispatch_test.cpp | C++ 测试 | `DbTaskDispatchEcs` 夹具下的快路径用例 |
| go/db/internal/kafka/producer_partition_audit.go | go/db | `logProducerPartitionUnknownOp` |
| go/db/internal/kafka/producer_partition_audit_test.go | go/db | op=other 且 verdict=ok 也打一条;额度共用 |
| deploy/k8s/db-task-partition-alerts.yaml | 部署 | `DbTaskUnknownOp`;文件头 :5-8 |

**交 Codex 的验证**

- 仓库根:`pwsh -File tools/scripts/dev_tools.ps1 -Command cpp-node-stop`;`msbuild cpp\tests\cross_zone_test\cross_zone_test.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`(必须 /m:1,否则报假的 C1041 / LNK1104);`.\build\cpp\tests\cross_zone_test.exe --gtest_filter=SaveSeq*:LastPersistedSnapshot*:DbTaskDispatch*`。
- `go/db`:`go test -count=1 -run 'ProducerPartition' ./internal/kafka/`。
- 有 promtool 时 `promtool check rules deploy/k8s/db-task-partition-alerts.yaml`。
- 通过标准:编译零错误;gtest PASS 汇总无失败;Go 绿;promtool 0 错误。

### B6 文档、告警与登记(依赖:B2d、B3、B4、B5;17 个,全部修改;另 Java 仓 1 个)

| 文件 | 内容 |
|---|---|
| docs/design/zone_data_rollback.md | 新增「2026-10-19 修订:R9 回档清理工具落地」:决定、流程表、键族表、挡不住的面、忘撤影响、发布与配置冻结、回档窗口内禁用的 merge_zone 模式、持久副本目录、只支持 strict 与 §8 上线后再补清单、RollbackFence 差距、残余风险 |
| docs/design/zone_data_rollback_en.md | 同上(英文) |
| docs/design/zone_data_rollback_zh.md | 同上 |
| docs/design/db-write-fencing-mysql.md | §9.1 / §9.2 写回档抬升与删除证据;§7.3 与 §14 第 22 条改为「已定:剥离」;第 11 条核实结论改为「开探针时快照侧带探针、新消息侧不带,快路径永不命中」;§14 第 9 条死信工具由 go/db 线做;文末「上线状态」里「回档 `-Apply` 不可用」一条补记「R9 B2d 起 strict 可用,legacy 见 zone_rollback_discard_tool.md §8」 |
| docs/design/mixed-version-persist-field-protection.md | §5.4 写入方表加一行 `zone_rollback_discard` P3:骨架行不盖戳(0);torn 对齐把落后表的戳抬到较新那行;屏障 schema_gen 用 `barrierrestore.SchemaGenFloor`;§5.3「唯一的合法例外是回档」补一句整 zone 回档的处置落在哪 |
| docs/design/scene-owner-reentry-barrier.md | 登记维护期只升不降的运维写入方 |
| docs/design/shared-redis-split-by-purpose.md | 登记 `rollback:quarantine:*`、`kafka:dead:index:*`、维护围栏值、盘上归档 |
| docs/design/db-retry-owner-routing.md | §7.10 死信去重(字段、指纹、七个写入点、结果码) |
| docs/design/db-task-kafka-partition-contract.md | :166 改为「已设 DbTaskUnknownOp」 |
| docs/design/db-task-store-unavailable-deferral.md | §4.1 死信工具契约:LREM / 重新入队时同段 Lua 按指纹 HDEL;信封后字段仍可推导 |
| docs/ops/godb-pipeline-launch-todo.md | 6.5、5.3.9、:522;:133「未授权」改为 2026-10-16 已定由 go/db 线做 |
| docs/ops/merge-zone-runbook.md | §3 消费者表(:150-157)补全 guild 全部写、route_tool、relocate(只挡 home 所在 zone)、pin、barrierrestore;手工清理段(:162-167)加「值以 `zone_rollback_discard:` 或 `save_barrier_restore:` 开头的不得 DEL,由持有方工具撤除;merge_zone 撞围栏时的报错文案仍会提示手工 DEL,以本 runbook 为准」;新增「任一 zone 回档期间(`merge:in_progress:*` 有 `zone_rollback_discard:` 值)禁止运行合服、撤销、relocate、pin-placement、backfill-home-zone;回档 verify 因此拒绝时须先撤销那次操作」;:677「trade 上架入口由合服围栏保证」更正为 trade 不读 K1(trade_step.go:29-31);:534 一带加死信不得单独删除;退役源 topic 的索引清理 EVAL |
| deploy/k8s/scene-manager-alerts.yaml | `SceneManagerHomeZoneFenceHeldTooLong` |
| deploy/k8s/owner-epoch-alerts.yaml | 注明 `DbStaleOwnerWriteRejected` 在回档后出现属于预期 |
| go/shared/ownerepoch/ownerepoch.go | 包注释登记维护期运维写入方(只改注释) |
| docs/design/zone_data_rollback_discard_tool.md | 文首状态改为「已落码」与各批验证结论 |
| docs/PROGRESS.md | 追加条目,注明 Java 不适用 |
| (Java 仓)D:/luyuan/xuanming-server-mmo/PARITY.md | 追加「不适用」一行 |

**交 Codex 的验证**

- 仓库根:`python tools/scripts/gen_docs_index.py --check`(只比对、不整段重写);人工核对 zone_data_rollback 三语版「2026-10-19 修订」一节的条目一一对应。
- 有 promtool 时 `promtool check rules deploy/k8s/scene-manager-alerts.yaml deploy/k8s/owner-epoch-alerts.yaml`。
- `go/shared`:`go vet ./ownerepoch/`。
- 通过标准:索引检查无差异;三语版一致;promtool 0 错误;vet 零告警。

## 4. 测试用例(按批)

Claude 不执行任何构建或测试(AGENTS §10.1),以下用例由落码批实现、交 Codex 运行。

**B1**

- keys_export:`PlayerTableCacheKey("PlayerAllData", id)` == `PlayerAllData:<id>`(与 login `buildParentKey` 同拼法);topic 边界(db_task_zone_1 不命中 db_task_zone_11 / db_task_zone_1_g2);glob 元字符按字面;`ParseAppliedCursor` 的 v2 与旧格式;guard 两种显式模式;`OrderingLockKeyPrefix` = `distributed:lock:kafka:ordering:<topic>:`。
- saveoutbox:`PendingIndexKey` 与 `BucketOf`。
- scenenode liveness:§2.1 判据表每一行至少一个向量;zone_id=0 反查成功 / 查无;death_at 存在时 StaleZoneGone 与 StaleNodeGone 都降为 Undecided;快照里一条值解析失败 → `Trusted=false`;缺 grpcEndpoint 的注册计入;同身份两条注册按存活;`NodeLoadKey` 等与 scene_manager 原格式逐字相同(scene_manager 侧由常量别名保证)。
- dead_dedup:v2 字段 = 头 12 字节 hex;RetryCount 不同的两份副本指纹相同;毒消息 Offset=41、Partition=3 与 v2 (seq=42, p3) 字段相同、指纹不同;v1 / 裸载荷 / 截断载荷 ok=false;CapturedAt 与错误串不同的两份毒消息指纹相同。
- guildcheck:data_service 原有用例经转发包原样通过(复查 [6]);shared 新用例(标准库断言):流式与 ListDivergences 逐行一致;超过 10000 行时流式不报错、ListDivergences 报 ErrTooManyDivergences;emit 出错即停、不再发 RPC;流式输出顺序在同一份 guild 答复下逐字节稳定(复查 [5])。

**B2a**

- 契约原有用例:三种 topic 并存;glob 元字符;SCAN 第 2 页失败(有界重试后仍失败 → 退出 1、分族计数正确、同 rid 重跑结果一致);`-expect-run-id` 不一致;`-version` 格式;stopped 遇到非 stopped 阶段的基线。
- 接线闸(复查 [1]):CLI 对 preflight / fence / stopped / inventory / purge / verify 以 3 拒绝且零写入,release、quarantine-drop、`-version` 正常。
- 协议:`-barrier-protocol legacy` → 3 且文案指向 §8;deploy-facts Fencing.Mode=legacy → 3;世代标记缺失或不是 strict → 3。
- rid:合服 run_id 形状子串被拒;合法 rid 放行。
- scan:一趟扫描按前缀分发到 H / P / merged_into / in_progress / applied / guard / 隔离区 / 排序锁,族边界正确(db_task_zone_1 不命中 db_task_zone_11);检查点续扫:run_id 相同从 cursor 续、不同或检查点缺失从头;部分结果先 fsync 后写检查点(中途杀进程重跑不漏不重)。
- preflight:DBSIZE 估算出的扫描时长超过 `-timeout` → 3;preflight.json 记扫描完成时刻与准入计数。
- fence:获取幂等;外人值(merge_zone 的 JSON、`save_barrier_restore:<id>`、他 rid)→ 3 且零写入;带 TTL 的同值也算外人值;准入超限 → 3,立栏后重算超限 → 5(reason=admission);preflight.json 过旧 / rid 不符 → 3;立栏后复核失败 → 5 且围栏保留;排空:集合只减、超时 → 5 且围栏保留、同 rid 重跑接着等;`-drain-wait` 不计扫描耗时(假时钟下扫描耗时超过 drain-wait 仍能排空完成);沉降:围栏写入前 1ms 通过检查、沉降期内才 CAS location 的玩家被第一趟扫描捕获;fence-drain.json 三类名单。
- topology(自 B2b 前移):run_id 正则与 src / dst 必须与 merged_into 一致;S≥T;清单缺失、`dry_run=true`、同一 run_id 两份;At 按秒截断时减 1 秒比较;relocate switched 的 At 晚于 T;清单 player_ids 与 post_t_candidate 求交命中 → moved_in_after_t。
- ledger(自 B2c 前移):字段常量与 fenceAcquireScript / fenceReleaseScript 读写的字段逐字一致;fence_acquired_at 只写一次;各持久副本文件 SHA 的读写。
- report:双写任一边 fsync 失败即整体失败;持久副本目录在报告目录之下 → 拒绝。
- release:按整值 CAS;外人值不碰;K1 已缺失而 K2 为我方 → 删除 K2、返回 7、报告 fence_lost;我方值被加 TTL → 删除并返回 7;清单已 released 且两键都不在 → 0;MySQL 不可达、deploy-facts 缺失时 release 仍成功。
- 租约:同 rid 第二个写进程拿不到租约 → 3;续期时 run_id 变了 → 中止 1;逐人 Lua 校验 token。
- preflight:foreign_home_storage_here、frozen(storage=Z / 清单缺失 / 清单含 Z)、merged_into(≥T、无清单、步骤未 Done)、在途 relocate、legacy_owned 各自 → 3;他区在线只报告。
- stopped:Z 的 load 集非空 → 3;Online / Undecided → 3;陈旧 location(zone 已空 / 节点已注销且 death_at 已过期)放行;存活排序锁等到截止 → 3;基线写见证行。
- fingerprint:块边界覆盖整个 uint64;NULL 与空串、`a#b` 与 `a`+`#b` 摘要不同;自适应块大小在 0.5s 目标附近收敛;.partial 可续算。
- throttle:令牌桶速率;PING p99 超阈值时暂停翻倍;网络错误重试 3 次带抖动,Lua error_reply 不重试。
- 集成(真 Redis):INFO 在 Lua 里取 run_id;fence 键 PTTL=-1;miniredis 不能替代的用例都标在这里。

**B2b**

- classify:真值表逐格各一个用例,加兜底 unclassified;foreign_home_storage_here;frozen;落点版本≥2 有证明与无证明;torn_cross_table。
- raise:owner_stop==0 且其它来源>0 → owner_behind;applied>owner → owner_behind;MaxUint64 溢出;超过 2^53 的值与非规范十进制。
- plan:purge 开始后(pass_count>0)inventory 重跑 → 3;计划含 action=skip 行(storage_elsewhere、home_elsewhere);`expected_inserts` 等于各行 p3.sql 的 insert 汇总;从 `-archive-dir` 加载时缺一个分片或任一 SHA 与清单不符 → 3;计划前态 / P2 后态 / P3 后态判定(屏障带 schema_gen 时 P3 后态按字段白名单判);P2 中途失败后重跑判定为可续;SQL 提交后、HSET 前崩溃的状态判定为可续。
- asset_gate:since = T_ms−300000;分歧 > 10000 行时照常流式写出,确认模板只含 SHA、行数与按 kind 汇总(复查 [5]);确认文件 SHA 不符 → 6;不可证明玩家 → 确认模板;guild 不可达 / 预算耗尽 → 3。
- inventory:schema Drift 非空 → 3 且文本含 migrate 命令;列差异进确认模板;post_t 占比阈值;离群 21 行 → 3,20 行 → 须确认;owner 与基线不同 → 3;归档估算超限 → 3;确认文件缺失 → 6;多出模板之外的节 → 6。
- inventory 见证窗口(复查 [2]):(T−Δ, T] 内生产、恢复后为旧版本的行不触发 3、只计数;(T−15m, T−Δ] 过半不一致(且不少于 20 行)→ 3;少数不一致 → 确认模板 `witness_mismatch`;恢复后围栏大于基线 → 3;判定窗口为空 → 须确认 `quiet_before_t`。
- inventory H / P 复核(复查 [3]):stopped 之后新出现的 foreign_home_storage_here、涉及 Z 的 frozen、清单对不上的 merged_into → 3;多出或缺少 home=Z 玩家 → 3。

**B2c**

- quarantine:范围内条目进隔离键并带 TTL;storage_elsewhere 与解不出的条目回共享收件箱 / 原死信;死信索引字段随条目迁移、指纹不同不动;源死信清空时索引被 DEL;暂存键已登记而 RENAMENX 未执行 / 已执行两种中断后重跑都按清单排空;目标键已存在 → 1;实例登记整键隔离;孤儿 processing 键取自计划。
- purge:归档 sha1 不匹配(PTTL=-1,写者存活)→ archive_mismatch;PTTL>0 冷读产物不论内容删除并计数;屏障带 schema_gen 字段时按整 HASH sha1 判计划前态;映射 / 落点 / location 被改 → route_changed;先抬后删的原子性:同一段 Lua 失败零写入;P2 中途失败后同 rid 重跑,已处理玩家判为计划后态(复核 6.1.1);shard_done 只在同一 pass 内跳过。
- purge 持久副本(复查 [9]):持久副本缺一个分片或 SHA 不符 → 3 且零写入;换一个空的报告目录(模拟换机器)、同一 `-archive-dir` 续跑收敛;证据归档持久副本一侧写失败 → 该批不执行删除、退出 1。
- verify:C' 不等;游标超过切点;已处理玩家 PTTL=-1 的键重新出现;冷读产物被接受;区级出现范围内条目;行数与 `expected_inserts` 不符;guild 复判的 SHA 与确认文件不同。
- verify 与计划全部类别比对(复查 [0]):Z 上有 pin 方式合入的玩家(home=Z、落点钉在 W,即 storage_elsewhere)时 verify 通过;storage_elsewhere 的落点被改 → 3;home_elsewhere 的映射因他区合服变化(仍≠Z)不触发拒绝;出现计划之外的 home=Z 玩家 → 3;计划里的 home=Z 玩家不见了 → 3。
- verify P 复核(复查 [3]):inventory 之后新出现的 foreign_home_storage_here 或涉及 Z 的 frozen → 3,报告列出玩家与 run;把落点恢复到计划值后重跑通过。
- quarantine-drop:清单未记 released / verify 时 → 3;UNLINK 后清单保留。
- 集成:真 Redis 上 P1、P2 与隔离区链路(经包内接口;整条链路经 CLI 在 B2d)。

**B2d**

- 两行都在且围栏、戳都相等 → 不写 SQL;两行 (0,0) → unclassified(上线前收窄,§8 第 2 条);torn 时缺行插入、落后行 CAS,两表围栏 = V、戳 = G(两行戳最大值);torn 落后表只有已盖戳的 `persist_schema_gen`、探针、`merge_state` 等非业务列 → 不判 torn_cross_table,带业务列 → torn_cross_table(复查 [10]);两行都缺时插骨架行,`persist_schema_gen`=0;围栏相等而戳不同 → unclassified;行锁下形状与计划不一致 → shape_changed。
- 屏障:重建后字段为 pending / player_database / player_database_1 / schema_gen,schema_gen == `barrierrestore.SchemaGenFloor(G, G)`;已是同值四字段时幂等;本人仍在桶里 → 拒;storage_elsewhere 的成员留在桶里不影响。
- 崩溃与续跑:SQL 提交后、HSET 前崩溃,重跑收敛(复核 6.1.1);INSERT 提交后、写 `strict-barrier-p<N>.ndjson` 前被杀,重跑后 verify 仍通过(期望插入行数来自计划,复查 [8])。
- 冷读与世代判据:P3 前出现的 PTTL>0 冷读产物被同段删除,PTTL=-1 → cache_present;重建后用 login 的 `beginParentLoadScript` 跑一次冷读校验必须通过;骨架行与对齐后的行过 go/db 的读判据(行世代 ≤ 本二进制世代);SQL 失败绝不发布 ACK;verify 在 P3 之后通过(不再误判 owner_behind)。
- barrierrestore:`SchemaGenFloor` 对 0 按 1、取最大值;原有 schema_gen 用例无回归。
- 放行(复查 [1]):接线闸删除后整条链路(preflight → fence → stopped → inventory → purge → verify → release)经 CLI 在真 Redis + 真库上跑通;ps1 契约测试:strict 不在入口被拒、legacy 仍被拒。

**B3**

- 契约测试:ValidateSet 含八个模式;退出码 5 / 7 的解释;preflight 对他区在线不拒绝、对续跑的 K1 放行;0g 位于 entry-state 之后、Step 1 之前;6b 位于 Step 6 之后;Step 7 复跑 release;中止与放弃文案含 release;**strict 在入口被拒**(B2d 之前,文案指向 R9 B2d)、legacy 在入口被拒(文案指向 §8);deploy-facts Fencing.Mode=legacy 或本机 db.yaml 有效 Fencing.Mode=legacy → 拒;Schema 3 只多 RouteStoreMode / RouteReadRequiresReady;`Get-GlobalFenceReaderProblems`:任一 zone `RouteStore.Mode=db_first`、`ReadRequiresReady=true`、`AllowGateZoneAsHomeZone=true`、没配 `DataServiceRpc` 各自拒绝(不读镜像);`Start-InfraPortForward` 超时与清理;`-ArchiveDir` 缺失、不可写、在报告目录之下 → `-Apply` 拒绝;Step 5 之后的单步命令带 `-archive-dir`;dev_tools 透传 `-RollbackArchiveDir`、`-RollbackWitnessLag` 等;rid 子串校验。
- k8s_deploy 契约:zone-up 后只读 GET 与 WARN 文案;DryRun 不执行;读失败只告警。

**B4**

- 同一 (partition, seq) 组提交重推两次,死信里只有 1 份;重试源副本进死信时被丢弃,`db_task_dead_letter_total` 不增、`duplicate_dropped` 加 1;旧格式收据不判重、计 undeduplicable;forward / requeue 进死信计 undeduplicable;指纹冲突照常入队并计 collision;`deadLetterRetry` 返回 2 视为成功、不打 DATA-LOSS;stranded 重搬返回 2 摘除;LLEN==0 时孤儿索引被删;WRONGTYPE 预检时一条不动。
- 集成(真 Redis 7.2):判重命中分支执行后 `INFO replication` 的 `master_repl_offset` 增加;连接 A 写入首份,连接 B 判重后 WAITAOF 返回的确认覆盖首份。
- 指标:12 条 `db_dead_letter_dedup_total` 与 `queue="dead_index"` 序列预建为 0。

**B5**

- `LastPersistedSnapshot.ReplaceStripsProbeKeepsPresence`:Replace 之后两张子表 `has_stress_test_probe()` 为 false,两个子消息的 presence 不变,入参不被修改。
- `LastPersistedSnapshot.ReplaceDoesNotCreateMissingSubMessage`:入参缺子消息时快照侧也缺。
- `DbTaskDispatchEcs.FastPathHitsWhenLandedCarriesProbe`:手工构造带探针的落地消息走 `Replace`,第二次无改动存盘命中快路径(skipped 加 1)。
- `DbTaskDispatchEcs.BusinessChangeStillWritesWithProbeSnapshot`:落地后改一个业务字段仍真写。
- 现有按字节比快照的用例(db_task_dispatch_test.cpp:621 一带)不受影响。
- producer_partition_audit:op=other 且 verdict=ok 也打一条,字段齐全;与不符日志共用额度。

## 5. 风险(残余)

1. 全部是静态结论:没有编译、没有运行任何测试。INFO in Lua、WAITAOF、HSET 同值传播只能在真 Redis 7.x 上验证。
2. 围栏是永久键:放弃回档而忘了 release,home=Z 玩家一直进不了场景、Z 不能建号、Z 公会写全拒、历练结果消费全服卡住、route_tool 全服暂停。靠三层兜底(告警、Step 7 断言、zone-up WARN)与 runbook。
3. 不踢人:挂机访客可能让 0g 等到超时(退出 5)。按归属 zone 踢下线要 login / gate 的命令通道,不在 R9 内。
4. 不挡的面:他区 login 冷读(只产生 PTTL>0 产物,P2 / verify 已识别;PITR 期间对 Z 库有冷读负载);不带 GateId 的 EnterScene(前提「所有生产调用方都带 GateId」由 runbook 声明);trade、mail、收件箱对他区在线实体的资产投递落在将被丢弃的存盘上,**trade 没有分歧闸**(trade 尚无可证明的已应用流水查询与 write-ahead 清理水位,asset_op_repo.go:1244-1247);收件箱可能重投 T 之后已消费的条目。排空时长越长,这些面越大。
5. 生效前提依赖人:回档窗口内冻结全服 data-service / scene-manager 的发布与配置变更、禁用 merge_zone 的五个模式,都写在 runbook,ps1 只在四个时点核对配置,两次核对之间的变更拦不住;镜像版本不核对,任何 zone 可能部署早于 4493b3c9f 的镜像之前须按 §8 第 4 条补。
6. 工具会写 MySQL:torn 对齐(围栏与戳)、骨架行 INSERT。回档后的库与 T 时刻不再逐字节一致,只是语义等价于「T 时刻没有这一行 = 空骨架」,依赖 proto2mysql 对空消息的列编码与 go/db 写路径一致,由 B2d 真库用例核对。
7. T 之后建的角色以原名变成新角色,T 之后的进度全部丢失;玩家看到的回退点比 T 早一个消费延迟。已接受,运营须对外说明。
8. storage_elsewhere:Step 5 丢掉它们发往别库的在途写,由 saveoutbox 在 Step 6 之后补投。它们的重试收据经 P1 回到共享收件箱,Step 6 之后照常重试。
9. epoch=0 的写者挡不住:守卫判 legacy_zero 直接放行,只能靠围栏、停服检查与 `DbOwnerEpochLegacyZero` 告警兜底;停服之前已过守卫、正卡在 SQL 里的僵尸写依赖 namespace 删除与 PITR 耗时远大于 SQL 超时。
10. owner_behind 会让回档停在 4a,它的修复入口 fencing §9.1 epoch_raise 尚未实现。
11. 规模:全量扫描合计 6 趟(§2.7),每趟与全服键数成正比(3e7 键、缺省速率约 25 分钟),stopped / inventory / verify 三趟在停服关键路径上;preflight 只做预算拒绝,不缩短耗时。分块校验和在停服后与 PITR 后各全表读一遍,大 zone 是分钟到小时级;冷读洪峰没有专门节流(k8s_deploy 没有按 zone 覆盖 `ZoneDefaultRps` 的参数),只在 Step 7 观察 MySQL 负载与 Kafka lag。没有压测数据之前不得据此下性能结论;B2d 的负载验收是唯一依据。
12. 归档文件含玩家资产明文(屏障、资产证据、带 pending / outbox 的父快照),且双写进持久副本目录:报告目录与持久副本目录都必须受访问控制(`run/*` 已被 gitignore,文件 0600、目录 0700;持久副本目录的权限由操作者保证)。
13. 两套版本号口径:`-version` 输出 40 位 `main.buildCommit`,与 shared/buildinfo 的 12 位规则不同,按契约实现。
14. (b) 只改变手工开探针的 L3 数据一致性压测的跳过率口径,常规压测不受影响;下一轮 L3 复盘写明。
15. (a) 动的是 go/db 热路径的入队层。人工删死信条目若不同段按指纹 HDEL,同一消息以后再也进不了死信;`DbTaskDeadIndexOrphans` 告警与 runbook 兜底。手工删 topic 或清 broker 而不清 Redis 时 offset 复用,靠 collision 计数暴露。
16. 不支持 Redis Cluster 与任何 zone 的 `RouteStore.Mode=db_first`(以 3 拒绝,db_first 另批设计)。Redis 按用途拆分之后逐人 Lua 的原子性前提不成立,由第 0 步 f fail-closed。
17. 只支持 strict:出现 legacy 部署(dual → legacy 降级、接入不写严格屏障的旧 scene / login)之前,工具与 ps1 都拒绝 legacy,届时按 §8 第 1、2 条补。B2d 通过真库验证之前 strict 也被拒,`-Apply` 不可用(与 db-write-fencing-mysql.md 文末「上线状态」一致,上线前不是阻塞项)。
18. 将来 db_first 对账 leader 上线后,`MergeFenceMirrorSetScript` 对 K1 回 2,注释写的是「不删,只告警」(route_ready.go:467-468),会产生告警噪声;届时在该告警描述里登记回档围栏值。
19. 并发运维只靠回档侧复核(§6.3):merge_zone 不改,回档窗口内他区归属玩家被搬进 / 搬出 Z 库、给 Z 回填映射时,发生在 inventory 之前的由 inventory 发现即 3(Step 5 之前,中止路径换回原库,搬进旧库的行随原库保留);发生在 inventory 之后的由 verify 以 3 拒绝,此时已过 Step 5,只能先撤销那次运维(落点 / 映射恢复到计划值)再重跑 verify。merge_zone 撞上回档围栏时的报错文案仍会提示手工 DEL,以 runbook 为准(§8 第 5 条)。
20. 持久副本依赖人:`-archive-dir` 的持久性与跨机可达由操作者保证(§7 第 2 条);ps1 只核对存在、可写、不在报告目录之下。它不可达时 purge / verify 以 3 拒绝,不会在没有计划副本的情况下写。
21. 见证窗口(#5)只能发现早于 T−Δ 以上的 PITR 偏差;Δ 以内的偏差与消费延迟不可区分,属于已接受的 RPO。Δ 取得比实际消费延迟小时,少数见证行不一致需要逐行确认;出事时延迟是分钟级的,操作者按 Step 1b (iv) 的 LAG 调大 `-WitnessLag`(上限 10m)。
22. 依赖混版本持久字段保护设计(mixed-version-persist-field-protection.md):P3 的戳与屏障 schema_gen 规则按它的 §3.3、§5.2、§5.4,proto 的 `persist_schema_gen` 与 barrierrestore 的字段白名单已在 HEAD。B2d 须在该设计第 1 批经 Codex 验证之后开工;该设计第 2 批(C++ 屏障的倒退比较与 enforce)尚未开工,落地后须补跑 B2d 的真库用例(enforce 下的首次存盘)。
23. guildcheck 的完整行为用例留在 data_service,经薄转发包跑 shared 实现;只在 go/shared 里跑 `go test` 测不到它们。撤除转发包时要把用例改成标准库断言迁到 shared(§7 后续)。

## 6. 复核意见处置表

原意见全文见附录 A。

### 6.1 数据一致性面

| 编号 | 结论 | 处置 | 落在 |
|---|---|---|---|
| 6.1.1 | 部分采纳 | 采纳不可变逐人计划、U(p) 只在 inventory 算一次、purge / verify 按「计划前态 / 计划后态」判定并补三个用例;不在清单里逐人记 p2_done / p3_done:逐人阶段由键状态(owner 与屏障组合)原子判定,避免清单随玩家数线性膨胀 | §1 #2;§2.2;§2.4 purge / verify;§4 B2b、B2c、B2d |
| 6.1.2 | 部分采纳 | guild 分歧闸采纳:`guildcheck` 迁到 shared,inventory 与 verify 各判一次,确认文件放行;trade 不采纳:trade 没有可证明的已应用流水查询与清理水位,data_service 现行回档闸同样只覆盖 guild,登记残余风险并列为 R9 之外的立项 | §1 #8;§2.4;§3 B1、B2b;§5 第 4 条;§7 |
| 6.1.3 | 采纳 | preflight / fence / stopped 硬拒 foreign_home_storage_here、frozen、在途搬库;merge_zone 冻结 CAS 加源 / 目标落点 zone 围栏,回填改 Lua 查围栏〔v3 收窄:merge_zone 部分移到 §8 第 5 条,改由 inventory / verify 复核兜底,见 §6.3 N5〕 | §1 #4;§2.1;§2.4 preflight;§3 B3 |
| 6.1.4 | 采纳 | `Get-TargetBarrierProtocol` 同时核对 Step 6 目标(`-NodeImage`、新参数 `-GoSvcTag`、本机 db.yaml),现网与目标不一致第 0 步拒绝〔v3 收窄:只留本机 db.yaml 核对,镜像核对与 `-GoSvcTag` 移到 §8 第 3 条,见 §6.3 N3〕 | §1 #3;§2.3;§2.9 C7 |
| 6.1.5 | 采纳 | 清单 player_ids 与 post_t_candidate 求交;merged_into 的 run_id 必须对上清单;owner_stop>0 的 post_t_candidate 须确认文件逐人放行 | §1 #6;§2.4 inventory;§2.6 |
| 6.1.6 | 采纳 | 离群 ≤20 行确认放行、>20 拒;见证行下界判定 PITR 过早;无见证时 quiet_before_t 确认 | §1 #5;§2.4 stopped / inventory |
| 6.1.7 | 部分采纳 | 列交集与列差异确认采纳;「P3 只写实际存在的列」不采纳:Step 6 的 go/db 缺列即拒启(proto_sql/db.go assertSchemaUpToDate),改为 inventory 要求恢复库通过同一份 Drift 校验,不过先跑 cmd/migrate up | §1 #7;§2.3;§2.4 inventory |
| 6.1.8 | 部分采纳 | 载荷 SHA1 不采纳(各副本的捕获时刻、错误串、RetryCount 不同,正常重复会全判冲突);改为跨副本稳定指纹,冲突照常入队计 collision 并告警;删除必须同段 Lua 按指纹 HDEL(采纳);索引键名带 topic 实例标识不采纳:消费路径拿不到 topic 实例标识,同名重建已被 kafka_offset_reset.ps1 拒绝 | §1 (a);§2.10 (a);§3 B4 |
| 6.1.9 | 采纳 | P1 逐条解码分流,范围外与解不出的回原处并逐条记报告,只有范围内条目进带 TTL 的隔离区,索引字段随条目迁移 | §2.4 purge P1;§2.5 |
| 6.1.10 | 部分采纳 | merge_zone 撞围栏与审计文案按持有者区分(采纳;v3 收窄移到 §8 第 5 条,runbook 先写明不得 DEL,见 §6.3 N5);rid 拒绝合服 run_id 形状子串(采纳);K2 值加 `zone_rollback_discard:` 前缀(采纳);deploy-facts 记录 guild MergeMarkerRedis 不采纳:K8s 部署恒复制该段并改写 Host(k8s_deploy.ps1:6676-6682),回档流程只在 K8s 上跑 | §2.1;§2.9;§3 B3 |
| 6.1.11 | 采纳 | 写者租约 `rollback:quarantine:<z>:<rid>:lease`;purge.json 由清单与 ndjson 生成,verify 以计划为逐人对象 | §2.4;§2.7 |
| 6.1.12 | 部分采纳 | 「在途代码编译耦合」不成立:barrierrestore 与 saveoutbox/generation.go 已于 8b9592de3 入库,直接引用并加字面量测试;行号漂移成立,B5 按锚点文字定位 | §2.1 键与值;§2.10 (b);§3 B5 |
| 6.1.13 | 采纳 | 立栏后沉降 30s,阻塞集合为空后再沉降 30s 全量复扫 | §2.1 fence 模式 |
| 6.1.14 | 采纳 | torn_cross_table 断言;校验和改为逐列定长 MD5 拼接、两段 `CAST(... AS UNSIGNED)`,在真 TiDB 上验证 | §2.3;§2.4 stopped;§2.6;§3 B2a、B2d |

### 6.2 水平扩展与运行面

| 编号 | 结论 | 处置 | 落在 |
|---|---|---|---|
| 6.2.1 | 采纳 | 令牌桶、在途字节、worker 缺省 4 上限 8、PoolSize=workers+2、PING 自适应退避、SCAN COUNT 500;全量 SCAN 降到 7 次,inventory / purge 用停服快照加逐人 MGET〔复查 [4] 订正:实际约 18 趟;v3 改为每模式一趟、合计 6 趟,purge 不扫,见 §6.4〕;P1 按已知键名枚举;准入 `-max-concurrent-rollbacks`;压测环境负载验收写进 B2c | §2.4;§2.7;§3 B2c |
| 6.2.2 | 部分采纳 | 归档收窄到证据、Lua 内算 sha1(且去掉 DUMP)、归档字节估算与上限(采纳);集群内 Job + PVC 与自动 RDB 留档不采纳:工具在集群外经端口转发运行是现行契约,收窄后归档量每人约百字节,BGSAVE 的 fork 会给全服登录路径带来延迟尖峰 | §1 #4;§2.4 inventory / purge |
| 6.2.3 | 部分采纳 | 判据下沉 `go/shared/scenenode`(采纳);scene_manager 不改用它:其判死依赖进程内 etcd 视图与本地观察时刻,只共享键名;工具每轮读 etcd 注册表快照证明节点已注销 | §2.1 他区在线判据;§3 B1 |
| 6.2.4 | 采纳 | 全服 data-service / scene-manager 镜像祖先核对、全服 `RouteStore.Mode=redis`、dev 旁路核对、runbook 冻结发布;四个时点复核〔v3 收窄:镜像祖先核对移到 §8 第 4 条,配置核对保留,见 §6.3 N4〕 | §1 #1;§2.1;§2.9 C8 |
| 6.2.5 | 采纳 | 单连接串行(连接池 ≤2)、自适应块、Threads_running 暂停、TiDB 扫描并发 1;校验和只在 stopped 与 inventory 各算一次 | §2.4;§2.7 |
| 6.2.6 | 采纳 | 网络类错误有界重试 3 次带退避与抖动;shard_done 只作同 pass 提示;超时按批计算 | §2.7 |
| 6.2.7 | 采纳 | 同 6.1.3;另把 backfill 改 Lua、在途 relocate 清单拒绝〔v3 收窄:backfill 改 Lua 移到 §8 第 5 条;在途 relocate 清单拒绝保留〕 | §2.4 preflight;§3 B3 |
| 6.2.8 | 部分采纳 | 表达式与分组更正、规则名按仓库惯例为 `SceneManagerHomeZoneFenceHeldTooLong`、Step 7 复跑 release 断言、zone-up WARN(采纳);promtool 规则单测不采纳:仓库没有规则单测设施,改用 `promtool check rules` | §2.1 政策;§2.8;§2.9 C11、C13;§3 B3、B6 |
| 6.2.9 | 采纳 | release 只依赖 Redis 与四个身份参数;fence 只依赖 Redis、etcd 与 preflight 结果 | §2.1 |
| 6.2.10 | 采纳 | purge 不碰 guard 键;verify 只断言两种显式模式为 0 | §2.4 verify;§2.5 |
| 6.2.11 | 部分采纳 | 新增 `-mode quarantine-drop`(UNLINK 异步释放)采纳;redis.yaml 打开 lazyfree-lazy-expire 不采纳:有 quarantine-drop 之后不需要改持久配置 | §2.4;§2.5 |
| 6.2.12 | 部分采纳 | merge_zone 文案(采纳;v3 收窄移到 §8 第 5 条);route_tool 只对作用范围拒绝不采纳:全局暂停是 route_tool 现行设计,runbook 写明代价;RemapHomeZoneForMerge 残余风险不成立:2026-10-12 起 `dataserviceserver.go`:244-261 在鉴权之前一律回 FailedPrecondition + ErrCodeNotImplemented,实现已删除(router.go:579),ErrCodeMergeFenceMissing 不再产生(error_codes.go:97) | §2.1 政策;§3 B3、B6 |
| 6.2.13 | 不采纳 | 前提已过时:barrierrestore 已于 8b9592de3 入库,工具与它同属 go/db module,直接引用 `MaintenanceKey` 并钉字面量,不必下沉 shared | §2.1 键与值 |
| 6.2.14 | 部分采纳 | 删除「冷读洪峰由令牌桶承接」(采纳);Step 7 观察 MySQL 负载与 lag(采纳);Step 6 传低档 zone rps 不采纳:k8s_deploy.ps1 没有按 zone 覆盖 `ZoneDefaultRps` 的参数(`PeakShavingLaunchTier` 是常量表),新增参数属削峰功能,列为 R9 之外的立项 | §1 #7;§2.9 C11;§5 第 11 条;§7 |

### 6.3 上线前收窄(v3)

依据:负责人 2026-10-18 确认尚未上线,db-write-fencing-mysql.md 文末「上线状态」写明 legacy 与 dual 混跑、dual → legacy 降级、旧二进制排除、回档 `-Apply` 不可用在上线前都不是阻塞项,首发以 dual + 严格屏障整批起步。收窄的标准:不损失「严格协议下整 zone 回档」的正确性与水平扩展;只为 legacy、新旧版本并存、旧镜像 / 旧二进制服务的删掉;防并发运维只保留回档自身的 fail-closed 复核。

| 编号 | 删了什么 | 理由 | 上线后何时补 |
|---|---|---|---|
| N1 | legacy 协议回档路径:§2.3 legacy 段(工具绝不写屏障、只允许两个 ACK 字段、`protocol_contradiction`)、现网判世代的 64 桶 ZCARD、P2 后态作为 legacy 终态、verify 的「64 桶为空」与 legacy 冷读产物规则、#5 的 `unknown(legacy)`、purgePlayerScript 的协议参数、B2c「完成后 legacy 可用」与 legacy 整链集成用例 | 上线前没有 Fencing.Mode=legacy 的部署;首发即 dual + 严格屏障。legacy 改为 ps1 入口与工具入口都拒绝 | §8 第 1 条:出现 legacy 部署之前 |
| N2 | strict 下两行 (0,0) 未围栏行的围栏 CAS(与 barrierrestore AllowLegacy 同形) | 只有 legacy 时代写入的行才是 (0,0),首发起不存在;改判 unclassified(blocked),不静默放行 | §8 第 2 条:有 zone 从 legacy 部署升上来之前 |
| N3 | Step 6 目标镜像协议核对:`Get-TargetBarrierProtocol` 的 git 部分(node 提交含 `kSaveWithCommitBarrierLuaScript`、login 提交含 parent_commit_barrier.go)与新参数 `-GoSvcTag`(含 dev_tools 透传) | 只防「回滚到不说严格屏障的镜像」;首发及之后的版本都说严格屏障。保留本机 db.yaml 有效 Fencing.Mode=dual 的核对(与版本无关,且不核对会在 Step 5 之后才暴露) | §8 第 3 条:出现第二个版本且可能指向首发之前的提交,或出现不说严格屏障的版本 |
| N4 | 全服 data-service / scene-manager 镜像祖先核对(4493b3c9f)、「最低提交」表、deploy-facts 的 `DataServiceImage` / `SceneManagerImage`,以及 `Read-GlobalFenceReaders` 读 Deployment 的那一半 | 首发晚于 4493b3c9f,上线前不存在不填 `home_zone_merging` 的在跑镜像。配置核对(RouteStore.Mode、ReadRequiresReady、AllowGateZoneAsHomeZone、DataServiceRpc)与版本无关,保留 | §8 第 4 条:任何 zone 可能部署早于 4493b3c9f 的镜像之前 |
| N5 | merge_zone 围栏配套 10 个文件:fence.go(`describeFenceHolder`)与新测试、audit_checks.go(`verifyFenceReleased`)、placement_ops.go 与测试(多围栏 CAS)、relocate.go 与测试(冻结 CAS 查源 / 目标落点 zone)、pin_placement.go(字段改名)、backfill_home_zone.go 与新测试(改 Lua 查围栏) | 防的是回档窗口内的并发运维,上线前不存在。回档正确性不依赖它们:停服前硬拒规则在 preflight / fence / stopped 执行,stopped 之后的变化由 inventory(Step 5 之前,可中止)与 verify(Step 5 之后,先撤销运维再重跑)在各自那一趟扫描里复核,发现即 3;runbook 禁止回档窗口内运行这些模式并写明不得手工 DEL 回档围栏。代价:stopped 与 inventory 不能复用 fence 的快照,各扫一趟(§2.7) | §8 第 5 条:首次上线后、第一次在生产做合服 / 搬库 / 回填之前 |
| N6 | deploy-facts Schema 3 的 `SceneManagerAllowGateZoneAsHomeZone`、`SceneManagerDataServiceRpc` 两个字段与 `Get-DeploySnapshotKubectlArgs` 加读 scene-manager ConfigMap | 与 C8 全服核对重复 | 不补 |
| N7 | (顺带精简)`go/shared/playercache` 两个新文件 | 没有第二个使用方:login、data_service、match、go/db 都按 proto 全名各自拼 `<消息全名>:<id>`;工具同样由描述符推导,经 `kafka.PlayerTableCacheKey`,辅助缓存名列表由测试钉死 | 不补 |
| N8 | (顺带精简)两个 testdata 样例文件 | 样例以字符串常量内联进 topology_test.go,测试内容不变 | 不补 |

v2 → v3 的文件数变化共 −16:收窄 N5 −10、N7 −2、N8 −2;复查 [6] 的薄转发包 −5;复查 [10] +2(barrierrestore)、+1(混版本设计登记)(§6.4)。N1–N4、N6 只删内容、不减文件。

### 6.4 复查轮(v3,11 条)

| 编号 | 结论 | 处置 | 落在 |
|---|---|---|---|
| [0] | 采纳 | 成立:storage_elsewhere(含 pin 方式合入、home=Z 落点钉在 W 的玩家,pin_placement.go:3-8)不进 v2 的计划,verify 的全量 H 遍必判 3。计划改为写**全部已归类玩家**(action=purge / skip);verify 的 home=Z 集合与计划里 home=Z 的全部类别逐人比对,storage_elsewhere 复核映射与落点逐字节未变。比较对象选计划而不是停服快照:计划已带类别、已进持久副本,而停服快照与计划的 home=Z 集合在 inventory 时已判为一致。补用例 | §2.2 计划;§2.4 verify;§2.6;§4 B2c |
| [1] | 采纳(随收窄调整) | 成立:v2 的 B3 先删 ps1 的 strict 拒绝,B2c 的 verify 只核对「屏障不存在」,中间批次存在 fail-open 窗口。收窄后只支持 strict,改为:B2a 起 CLI 带接线闸(除 release / quarantine-drop / `-version` 外一律 3),B3 **保留** ps1 入口对 strict 的拒绝,两道闸都由 B2d 删除并各补测试;B2c 不再声称任何协议可用;依赖改为 B2d 在 B2c 与 B3 之后 | §1 #3;§2.3;§2.9 C7;§3 依赖、B2a、B2c、B2d、B3;§4 |
| [2] | 采纳 | 成立:`fence_save_seq>>10` 是生产时刻(save_seq.h 的 Next=max(last+1, nowMs<<10)),MySQL 提交还要晚一个消费延迟,零容忍会误判正确的 PITR。判定窗口改为 (T−15m, T−Δ],Δ = `-witness-lag`(缺省 5m、上限 10m,写进确认模板);恢复后围栏大于基线 → 3;过半不一致 → 3;少数不一致逐行确认;(T−Δ, T] 只计数。没有采纳「Δ 取停服时实测延迟」:工具拿不到按时间计的消费延迟(Kafka LAG 是条数),改为显式参数并由操作者参考 Step 1b (iv)。补用例 | §1 #5;§2.4 stopped / inventory;§2.9 C10、C12;§4 B2b;§5 第 21 条 |
| [3] | 采纳(改法调整) | 成立:relocate 冻结 CAS 只查 home 的围栏(relocate.go:278),v2 的 inventory 不扫 P、verify 不复核 P。收窄后不改 merge_zone(§6.3 N5),也就没有「B3 之前的 merge_zone」之分;改为 inventory(Step 5 之前)与 verify 在各自那一趟全量扫描里按停服前硬拒规则复核 P 与 merged_into,不额外加遍数;runbook 禁止回档窗口内运行合服、撤销、relocate、pin-placement、backfill-home-zone(任何版本)。inventory 之前的搬进发生在换库之前,行随原库保留,中止路径可恢复;inventory 之后的变化写入的是恢复库,verify 拒绝后先撤销运维再重跑 | §2.1 挡不住的面;§2.4 inventory / verify;§5 第 19 条;§4 B2b、B2c |
| [4] | 部分采纳 | 遍数低估成立(v2 写 7 次,正文约 18 趟)。(1) 每模式一趟不带 MATCH 的 SCAN、客户端分发:采纳,正文写出真实遍数 6 趟。(2) fence 快照供 stopped / inventory / purge 复用:不采纳 stopped / inventory 部分,收窄后不改 merge_zone,P 集合可以增长,两者必须重扫;purge 采纳为零扫描(孤儿 processing 取自计划、暂存键取自清单登记,新增 `stageSourceScript`)。(3) cursor 续扫:采纳,但检查点写本机报告目录而不是 Redis 清单:部分结果本来就在本机,检查点跟着它走;丢了只是重扫;只读模式不必写 Redis。(4) DBSIZE 预算拒绝、`-drain-wait` 不计扫描:采纳 | §2.0;§2.1 fence;§2.4 preflight / purge;§2.5;§2.7;§4 B2a |
| [5] | 采纳 | 成立:`MaxDivergenceRows`=10000 是给可以缩小范围分批的单人回档定的(guild_divergence.go:39-42)。shared 实现新增流式 `StreamDivergences`,工具不设行数上限,分歧逐行写 `asset-divergence.ndjson`、不逐行打 ERROR;确认模板只记 SHA、行数与按 kind 汇总,按 SHA 放行;verify 复判比 SHA。`ListDivergences` 建在流式之上,data_service 的 10000 上限与 `ErrTooManyDivergences` 不变 | §1 #8;§2.4 inventory / verify;§3 B1、B2b;§4 B1、B2b |
| [6] | 采纳(选薄转发包) | 三点都成立(go/shared/go.mod 没有 testify;rollback_merge_fence_test.go 也 import 旧包;HEAD 说法过时)。处置:shared 的新测试只用标准库,go.mod / go.sum 不动(不引入新依赖,无需负责人确认);data_service/internal/guildcheck 改为类型别名转发的薄包,五个调用方与原 450 行用例一个不改、经转发包跑 shared 实现,B1 由 21 个文件降到 14 个;撤除转发包列为 R9 之外的后续(§7)。HEAD 订正为 c63b22dca(复查意见写的 85a457a55 也已过时),列出有他人未提交改动的 8 个文件与协调要求 | §3 B1、文件归属;§4 B1;§5 第 23 条;§7 |
| [7] | 采纳 | 成立:B2a 的硬拒与 fenceAcquireScript 要用到 B2b 的 topology.go 与 B2c 的 ledger.go。topology.go(含测试,样例内联)与 ledger.go 的键名、全部字段常量与读写函数前移进 B2a;B2c 只补 pass、shard_done、staging 登记、分族计数与 purge.json 汇总。清单字段只有一份定义 | §3 B2a、B2b、B2c;§4 B2a |
| [8] | 采纳 | 成立:v2 没规定「P3 插入的骨架行数」从哪来,按运行记录计会在「INSERT 提交后、写记录前」崩溃时少算。改为 inventory 的 strict 预检按形状把每张表的 keep / cas / insert 写进计划,plan.json 汇总 `expected_inserts`;verify 只用计划推导。补用例 | §2.2 计划;§2.3;§2.4 verify;§4 B2b、B2d |
| [9] | 采纳(位置调整) | 成立:Step 5 之后只能向前,计划却只在操作者本机。新增 `-archive-dir`(ps1 `-ArchiveDir`,`-Apply` 必填):停服基线、计划、inventory.json、帮会分歧文件、确认文件与 P2 证据归档都由工具双写并 fsync,各文件 SHA 记进 Redis 清单;purge / verify 一律从持久副本加载并逐文件校验,缺失或不符 → 3;P2 每批两边 fsync 之后才执行删除。没有采纳「infra 里的 PVC」:仓库 infra 没有对象存储,集群内 Job + PVC 已在 6.2.2 不采纳;持久目录的位置请负责人确认(§7 第 2 条) | §2.0;§2.2;§2.4;§2.7;§2.9 C10、C11;§4 B2a、B2b、B2c;§5 第 20 条;§7 |
| [10] | 采纳 | 成立,且前提比复查意见写的更近:proto 的 `persist_schema_gen`(player_database 21 号、player_database_1 5 号)与 barrierrestore 的 `schema_gen` 字段白名单、`schemaFloor` 都已在 HEAD。处置:torn 的非业务列由 proto 推导(主键、`OptionPersistUncarried`、`fence_*`、`persist_schema_gen`);屏障一律按字段白名单与整 HASH sha1 判,不用 HLEN;P3 写 `schema_gen`,规则用 barrierrestore 导出的 `SchemaGenFloor`(B2d +2 个文件);torn 对齐时两表戳一并对齐到较新那行,骨架行戳为 0;骨架行与 go/db C0–C2 判据在 B2d 真库用例里验证;§5 登记依赖与先后顺序,B6 在该设计的写入方表里登记本工具(+1 个文件) | §2.2 状态表;§2.3;§2.4 Lua 契约;§3 B2d、B6;§4 B2d;§5 第 22 条 |

## 7. 需负责人另行拍板

1. **规模**:R9 授权时 73 个文件;v2 并入复核意见后 125 个;v3 按上线前范围收窄并处理复查意见后 **109** 个(新增 58、修改 51、删除 0;批次 B1→B6 九批不变,增减明细见文首与 §6.3)。仍比授权多 36 个,按 AGENTS §10.2,请确认按 109 文件规模开工 B1。
2. **持久副本位置**(复查 [9]):仓库 infra 没有对象存储。本稿要求 `-ArchiveDir` 指向操作者保证持久、可从其它机器访问的目录(网络共享或对象存储挂载点),工具只保证双写、fsync 与 SHA 校验。请确认这一要求可以接受,或指定具体位置;若要在 infra 新增对象存储或 PVC,属于另批的部署改动。

R9 之外的后续立项(不阻塞 R9,记录备查):trade 回档分歧闸(trade 侧 `ListAppliedAssetOpsSince` 等价查询 + write-ahead 清理水位);k8s_deploy 按 zone 覆盖削峰档位的参数;guild 历练结果按 zone 停放消息;login 侧读 K1 的加闸(上线前所有 login 都会是新版本,加闸后全版本有效,但不是回档正确性的前提);data_service `RollbackFence` 的跨服务实现;按归属 zone 踢下线的命令通道;撤除 `data_service/internal/guildcheck` 薄转发包(用例改标准库断言迁到 shared,改五个调用方);若 infra 引入对象存储,持久副本改写对象存储。

## 8. 上线后再补

本节是 v3 按上线前范围(§6.3)删掉的内容。触发条件出现之前,工具与 ps1 对相应情形一律 fail-closed(拒绝或判 blocked),不会静默放行;触发条件出现时,按「补法」另起一批,规模与验证按当时的 AGENTS 门禁走。

| # | 内容 | 触发条件 | 补法要点 | 原出处 |
|---|---|---|---|---|
| 1 | legacy 协议回档 | 出现 Fencing.Mode=legacy 的部署:dual → legacy 降级,或接入不写严格屏障的旧 scene / login | 现网判定恢复「legacy + 无世代标记 + 64 桶全空 → legacy」;工具绝不 HSET 屏障,屏障只允许 player_database / player_database_1 两个 ACK 字段,出现 pending / outbox_* → inventory 判 3(protocol_contradiction);legacy 终态为 P2 后态;verify 断言 64 个桶为空,冷读产物 PTTL>0 即接受;#5 记 `effective_point=unknown(legacy)`;ps1 放开 `-BarrierProtocol legacy`,deploy-facts 恢复 legacy ⇔ legacy | v2 §1 #3、§2.3、§2.4、§5 第 17 条 |
| 2 | 两行 (0,0) 未围栏行 | 有 zone 从 legacy 部署升上来(库里有 legacy 时代写入的行) | 两行 (0,0) → V=(C',1),两行做围栏 CAS(条件 fence=(0,0),RowsAffected==1;与 barrierrestore AllowLegacy 同形),戳按 §2.3 规则对齐 | v2 §2.3 P3 |
| 3 | Step 6 目标镜像协议核对 | 首次上线后出现第二个版本,且存在不说严格屏障的版本,或 `-NodeImage` / Go 服务镜像可能指向首发之前的提交 | ps1 `Get-TargetBarrierProtocol`:`git show <sha>:cpp/libs/engine/infra/storage/redis_client/redis_client.h` 含 `kSaveWithCommitBarrierLuaScript`;新参数 `-GoSvcTag`(留空 = `-NodeImage` 的 tag,Step 0b 与 Step 6 显式传同一值),`git cat-file -e <sha>:go/login/internal/logic/pkg/dataloader/parent_commit_barrier.go`;现网与目标不一致第 0 步拒绝;dev_tools 透传 `-RollbackGoSvcTag` | 复核 6.1.4 |
| 4 | 全服镜像祖先核对 | 任何 zone 可能部署早于 4493b3c9f 的镜像(只有回滚到首发之前的提交才会出现) | `Read-GlobalFenceReaders` 加读 `deployment` data-service / scene-manager,镜像 tag 必须是 git sha(digest 判不出即拒),`git merge-base --is-ancestor 4493b3c9f <sha>`;deploy-facts 加 `DataServiceImage` / `SceneManagerImage`;最低提交表:进场与建号脚本内复核 4493b3c9f,建号预检 11ba15a0f,guild 建帮 11ba15a0f、经济 440fe26b6、历练 284509ad0(guild 只作纵深防御,不核对) | 复核 6.2.4、核实需改 2 |
| 5 | merge_zone 围栏配套 | 首次上线后、第一次在生产做合服 / 搬库 / 回填之前 | `describeFenceHolder(value)`(JSON 原文案;`zone_rollback_discard:` 与 `save_barrier_restore:` 前缀「禁止 DEL,由持有方工具撤除」;其它「未知持有者,禁止 DEL」),`acquireMergeFence` 撞围栏与 `verifyFenceReleased` 改用它;`placementCAS.FenceZone` 改为去重去 0 的 `FenceZones`,Lua 逐个 EXISTS;relocate 冻结 CAS 查 {home, 源落点 zone, 目标落点 zone},切换与解冻不加;pin 跟随改名;backfill 的 SetNX 改为一段先查 `merge:in_progress:{zone}` 的 Lua;各带测试(约 10 个文件)。落地之后 ps1 再加「回档窗口内使用的 merge_zone 不早于该提交」的核对,stopped / inventory 才可以改为复用 fence 的快照 | 复核 6.1.3、6.1.10、6.2.7、6.2.12,复查 [3]、[4] |

## 附录 A 对抗复核意见原文(v1 §6,原样保留)

### 6.1 数据一致性面（结论：needs_changes）

1. **[high] 断点续跑和 verify 会把本工具自己写入的 C' 当成外来证据。U(p) 现读了 applied_epoch、屏障 pending/ACK、行围栏 fence_owner_epoch，但 P2 会把 applied_epoch 抬到 C'，P3 会把 (0,0)、全零 torn 和骨架行写成 V=(C',1)。之后任何一次重跑盘点判定都会得到 U=C'>owner_stop，于是判成 owner_behind 归入 blocked、退出 3。另外 save_barrier 在 P2 的删除族里，P3 重建的屏障在 verify 中会被当作「仍需删除的键族」，续跑的 P2 也会把它再归档删掉一次。设计只声明重跑接受「owner==C'、各族键已不存在」两种状态，没有覆盖 applied==C'、屏障==V、行围栏==V。**
   - 场景：legacy：P2 处理到一半时 SCAN 第 N 页出错，以 1 退出。用同一 rid 重跑 purge，第 1 步原样重跑 inventory 判定，已处理玩家的 consumer:applied_epoch:<topic>:<pid>:player_database 已是 C'>owner_stop，判 owner_behind，返回 3。此时 Step 5 已执行，只能向前，工具却没有能走通的路径。strict：P3 把两行 (0,0) CAS 成 (C',1) 并写入 pending=(C',1)；5b 的 verify 重跑归类时，屏障 epoch 与 fence_owner_epoch 都是 C'，判 owner_behind，且屏障存在、被当作待删族，verify 必然 3。设计自己列出的用例「SQL 提交后、HSET 前崩溃，重跑收敛」在这条规则下同样过不了。
   - 证据：设计 §2「U(p) 取以下来源的最大值…owner_stop<U → owner_behind」、§4 purge 第 1 步「这次重跑接受『owner==C'、各族键已不存在』」、verify「重跑归类,不能出现任何仍需删除的键族或 blocked 玩家」；go/db/internal/kafka/key_ordered_consumer.go 的 luaMarkAppliedEpoch（只升不降，抬到 C' 后不会回落）；go/login/internal/logic/pkg/dataloader/parent_commit_barrier.go 的 beginParentLoad（pending 的 epoch 进入判定）；设计 §5 键族表 save_barrier「归档后 UNLINK;strict 下 P3 重建」。
   - 改法：inventory 产出一份不可变的逐人计划：pid、class、owner_stop、C'、V、各归档键的预期 sha1、行围栏，计划文件的 SHA 写进清单。purge 和 verify 只判定「当前状态 ∈ {计划前态, 计划后态}」。U(p) 只取停服基线和 inventory 当时读到的值，显式排除本工具写入的 applied==C'、屏障三字段==V、V.epoch==C' 的行围栏。清单逐人记录 p2_done / p3_done，P2 跳过 p3_done 玩家的屏障删除。verify 对已处理玩家改为核对「状态等于计划后态」，不再走 U(p) 归类。补三个用例：P2 中途失败后重跑、P3 的 SQL 提交后崩溃再重跑、strict 下 verify 通过。
2. **[high] 缺少帮会 / 交易资产分歧闸。仓库现行的回档契约要求「任何回档」在写玩家数据之前先向 guild 查询 T 之后已终结的资产操作：有分歧默认拒绝，放行要逐条记录作为补偿依据。整区回档会把玩家钱包、背包和 asset_op_ledger 回到 T，guild、trade 库却不回退。本设计没有任何闸，也不出补偿清单，只在风险 8 里泛泛记了一句「跨服务状态不随回档回退」。**
   - 场景：玩家在 T+10min 向帮会捐 1000 金：scene 扣钱并记账（player_database），guild 侧记 APPLIED、帮会资金 +1000。整区回档后玩家钱包回到 T，1000 金还在，帮会资金不回退，凭空复制 1000。反方向，帮会商店或活动在 T 之后发的奖励随回档消失，guild 侧已终结、不会重投，玩家白白损失。全程没有告警，也没有补偿名单。
   - 证据：go/data_service/internal/logic/rollback_logic.go:128-141「回档把玩家的钱包换回快照,而帮会账本不在快照里…所以任何回档在写任何玩家数据之前…先问 guild」；go/data_service/internal/guildcheck/guild_divergence.go:126 ListDivergences；docs/design/guild-phase2/07-rollback-fail-closed.md §7.8.1；proto/common/database/mysql_database_table.proto:148-151（asset_op_ledger 与 currency 同记录）；tools/scripts/k8s_zone_rollback.ps1 Step 3 文案「不回退全局库(…guild、mail、收件箱、trade)」。
   - 改法：inventory 增加资产分歧闸：对 purge 范围内全部玩家以 since=T 查询 guild 的 ListAppliedAssetOpsSince，trade 的 assetop 表做同类查询。有分歧或超出保留期无法证明的，一律 blocked（退出 6）。只有显式给出 -accept-asset-divergence <清单文件> 才放行，并把每条分歧写进报告作为补偿依据。查询失败按拒绝处理（fail-closed）；purge 之后再复查一次。go/db 模块不能 import data_service/internal/guildcheck，需要在 zonerollback 里新增 guild internal gRPC 客户端适配，文件数相应增加。
3. **[high] home≠Z、落点=Z 的玩家（foreign_home_storage_here）的写者不在停服和围栏的覆盖范围内。C++ 按 home 选 topic，go/db 按处理那一刻的落点选库，所以这类玩家的存盘由他们 home 区那个仍在线的 go/db 写进 zone_Z_db。K1 只挡 home=Z；Step 1 只删 Z 的 namespace；relocate 冻结时的 CAS 只检查 home 区的围栏。设计要到 4a（PITR 之后）才把这类玩家归为 blocked，停服复核「zone_<id>_db 不再有写入」这一前提并不成立。**
   - 场景：玩家 P 的 home=W、落点 Z:2（搬库进来，或 pin 合服留下的）。回档 Z 期间，W 的 go/db 一直在线，持续把 P 的存盘写进 zone_Z_db，跨越停服基线、PITR 和影子库替换。4a 判 foreign_home_storage_here，退出 6，走中止路径「换回原库」，W 的 go/db 在窗口内写进影子库的存盘随影子库一起丢掉。strict 下屏障 ACK 已等于 pending，SQL 行却是旧版本：父快照过期后冷读报「SQL 子表围栏尚未对齐父快照提交屏障」，补投器认为已 ACK、不会补投，玩家卡死。同一窗口里，relocate 把 W 归属的玩家搬进或搬出 Z 库也不受 K1 约束：搬进的行被 PITR 抹掉，搬出的行读的是回退中的库。
   - 证据：go/shared/placement/placement.go 包注释「选库只在 go/db 一处发生:按处理那一刻的记录选」；cpp/libs/services/scene/player/system/player_lifecycle.cpp 的 GetDbTaskTopic(homeZoneId, …)（topic 只按 home_zone 选）；tools/merge_zone/relocate.go:278 placementCAS{… FenceZone: rd.Home}，placement_ops.go:530-531（只追加 mergeFenceKey(FenceZone)）；tools/scripts/k8s_zone_rollback.ps1 Step 1b(iii)「zone_<id>_db 不再有写入」。
   - 改法：preflight 只靠 Redis 就能判出这一类，应与 stopped 一起硬拒：SCAN player:placement:*，有效落点=Z 且 home≠Z（包括 frozen 且 storage=Z）即退出 3，先于 0g 和 Step 1 执行；stopped 再复核一次；拓扑清单目录里有未结束、源库或目标库含 Z 的 relocate run，同样拒绝。tools/merge_zone 的 relocate 和 pin 在冻结与切换的 CAS 里，额外要求源库、目标库所在 zone 的 MergeFenceKey 都不存在（改 relocate.go、placement_ops.go 并补测试，计入文件数）。
4. **[medium] 屏障协议按现网事实判定（Fencing.Mode 加世代标记），但 Step 6 部署的是 -NodeImage 指定的回滚目标镜像。工具重建或删除屏障的形状，可能与回档后实际运行的 C++ / login / go/db 协议不一致。设计只在 legacy 分支上用 git 核对镜像，strict 分支不核对目标镜像。**
   - 场景：现网是 strict（新 C++、go/db dual、标记为 strict），回档同时把 node 回滚到 T 时、尚无严格屏障的旧 sha。工具按 strict 重建 pending=V；旧 C++ 走 kSaveIfGuardLuaScript，只写父快照、不推进屏障。下次登录 login 发现父快照 last_save_seq≠seq(V)，报「父快照水位与提交屏障不符」，全员拒登。反过来，现网判为 legacy、目标镜像却是 strict 时，工具只删不建屏障，owner≠0 的玩家全部报「历史玩家父快照与提交屏障缺失」。如果目标 db 镜像是 legacy 而 Redis 里有 strict 标记，CheckLegacyAllowed 会让 go/db 拒启。这些都发生在 Step 5 之后，没有退路。
   - 证据：tools/scripts/k8s_zone_rollback.ps1:300-307（-NodeImage 必填，「回到哪一版」由操作者显式指定）；go/db/internal/saveoutbox/generation.go 的 CheckLegacyAllowed；go/login/internal/logic/pkg/dataloader/parent_commit_barrier.go:101-111；cpp/libs/engine/infra/storage/redis_client/redis_client.h 的 kSaveWithCommitBarrierLuaScript（缺 owner 键即报 missing owner）。
   - 改法：协议判定同时覆盖 Step 6 将要部署的 node / login / db 镜像。Get-TargetBarrierProtocol 对 strict 也做核对：目标 node 提交含 kSaveWithCommitBarrierLuaScript、目标 login 提交含 parent_commit_barrier.go、目标 db 渲染的 Fencing.Mode 为 dual。现网判定与目标判定不一致时，在第 0 步拒绝，并说明要先统一镜像协议。
5. **[medium] #6 只凭 merge:merged_into 判定「T 之后合入」，没有拿合服、撤销清单里的 PlayerIDs 与 post_t_candidate 求交。merged_into 一旦被撤销路径或人工清理掉，或者合入规模低于占比阈值（5% 且多于 20 人），copy 模式合入的玩家就会被当成 T 之后建角，变成空骨架。**
   - 场景：T+1h 把 15 人的小区 W 以 copy 模式合入 Z，事后 merged_into 被撤销流程或人工删掉，合服清单仍在。PITR 后这 15 人在 Z 库没有行，home=Z，落点无记录，归为 post_t_candidate，又低于占比阈值。strict 下物化空骨架，legacy 下冷读得到空骨架；他们的真实数据只剩 W 库的冷副本，玩家被静默清空。
   - 证据：tools/merge_zone/merge_run.go:586-617（copy 模式步骤 1 先拷行，步骤 5 才写 merged_into 并改映射）；tools/merge_zone/unmerge.go:136（撤销路径）；设计 §4 inventory 第 7 步 -max-post-t-ratio（缺省 0.05，且人数 >20）。
   - 改法：inventory 必须读取 -topology-manifest-dir 下所有 dst=Z 或 src=Z 的合服 / 撤销 / 搬库清单，把清单里的 PlayerIDs 与 post_t_candidate 求交，命中一律判 moved_in_after_t（blocked）。merged_into 记录的 run_id 必须能对上一份清单，缺清单即 topology_unproven。post_t_candidate 中 owner_stop>0 的玩家，逐人列进报告，并要求一份人工确认文件，不能只靠比例阈值放行。
6. **[medium] #5「任一行存盘时刻晚于 T+60s 即以 3 拒绝」会误拒合法数据，而且发现不了 PITR 恢复得过早。save_seq 不是时钟：进程水位可以被种子抬到墙钟之后 60s，超界的种子会挂成个人下限，不受上界约束。**
   - 场景：某玩家曾从时钟超前的节点或被手工改过的库载入，个人下限超前一天，T 之前的正常存盘 fence_save_seq>>10 也大于 T+60s。inventory 以 3 拒绝，又没有豁免开关，整区回档无法执行。另一个方向：PITR 实际恢复到了 T−3h，所有 seq 都早于 T，检查照样通过，多丢 3 小时数据而无人察觉。
   - 证据：cpp/libs/services/scene/player/system/save_seq.h:23/35/106（Observe 接受不超过 (nowMs+60s)<<10 的种子，Next=max(last+1, nowMs<<10)）；player_lifecycle.cpp:4165-4176（超界种子挂成 PlayerSaveSeqFloorComp 个人下限，进程水位不抬）。
   - 改法：改为分布判定加逐人清单：超出 T+60s 的行数超过阈值才拒绝；少量离群的行写进报告，并要求 -acknowledge-seq-outliers <文件>。另加下界判定：停服基线里 seq>>10 落在 (T−Δ, T] 区间的行，PITR 后应仍然存在且值相同；PITR 后的 max(seq>>10) 明显早于 T 时，判为 PITR 过早并拒绝。
7. **[medium] 「列清单不同 → 3」，加上骨架行按编译期 proto 插入，使跨越 schema 变更的回档走不通。而最常见的灾备触发恰恰是一次有问题的发布，这类发布往往带加列。**
   - 场景：T+30min 发布了一个加列的版本（go/db 启动时 ADD COLUMN），出事后回档到 T。PITR 后的库没有新列，inventory 发现列清单不同，以 3 拒绝。即使放宽这一条，P3 用新 proto 的列 INSERT 骨架行也会在旧 schema 上失败，而且发生在 Step 5 之后。
   - 证据：设计 §4 inventory 第 2 步「列清单不同 → 3」、§3 P3「proto2mysql 的 ExistsByPK+Insert 插入」；proto/common/database/mysql_database_table.proto:140-142（老库由 cmd/migrate 只做 ADD COLUMN）；k8s_zone_rollback.ps1:161-162（工具必须与现网 go/db 同一提交，也就是新 proto）。
   - 改法：基线和重算都按两次 information_schema 的列交集计算，列差异写进报告并要求显式确认。strict 预检按 PITR 后实际存在的列做一次骨架行 INSERT 可行性检查（列存在、NOT NULL 列有默认值），不可行就在 Step 5 之前拒绝。P3 只写实际存在的列。
8. **[medium] 死信去重的索引字段只有 <partition>:<seq>，不校验内容。HSETNX 返回 0 即视为重复，直接不入队。offset 一旦被复用，真正的新死信就会被静默丢掉，而调用方仍按「已入队」ACK 了 Kafka 位点。**
   - 场景：dev 或 staging 删除并重建同名 topic（或按 AGENTS §6.2 清了 broker 数据目录）而没清 Redis，offset 从 0 重新计数。一条新的毒消息 (p3, seq=42) 命中旧索引，HSETNX 返回 0，没有 LPUSH，消费者照常 ACK，这笔存盘永久丢失，指标上看不出异常。moveClaimedRetryToDead 同样只 LREM processing。另外，人工 LREM 死信时没有 HDEL，同一消息之后再被重投也会被丢弃。
   - 证据：设计 (a) deadEnqueueLua「if HSETNX(...)==0 then return 0 end」，字段取 msg.Offset+1 / task.seq；go/db/internal/kafka/key_ordered_consumer.go 的 wrapRetryPayload（[seq 8B][partition 4B]，seq=offset+1）；AGENTS.md §6.2（压测前允许清 broker 数据目录）。
   - 改法：索引字段的值存载荷 SHA1（或 task_id+sha1）。HSETNX 失败时比对这个值：相同才判为重复（duplicate_dropped）；不同则照常 LPUSH，并计 collision 指标、触发告警。索引键名带上 topic 实例标识，或者让 topic 重建 / offset 重置流程同步删除索引。现有会把条目搬出死信的人工 runbook，也一并要求在同一段 Lua 里 HDEL。
9. **[medium] P1 按整键 RENAMENX 加 PEXPIRE 720h，把本 topic 下所有玩家的死信和重试收据都放进带 TTL 的隔离区，其中包括不在回档范围内的 storage_elsewhere 玩家，而他们的目标库并不回退。**
   - 场景：home=Z、落点在全局库的玩家有一笔存盘因排序冲突进了死信，目标库不回档，这笔仍需人工裁决。P1 把整个 kafka:dead:queue:<topic> 改名隔离后，DbTaskDeadLetterNonEmpty 随即静默；720h 后隔离键过期，这笔待裁决的写连同证据一起消失。legacy 下，这类玩家的重试收据同样随 Step 5 和 P1 一起被丢弃。
   - 证据：cpp/libs/services/scene/player/system/player_lifecycle.cpp（topic 与 player:save_pending:<homeZone> 都按 home 选，storage_elsewhere 玩家的写也走本 zone 的 topic）；设计 §4 purge P1（RENAMENX + PEXPIRE -quarantine-ttl 缺省 720h）；go/db/internal/kafka/store_failure.go:171-196 sampleRetryQueues（死信长度告警的数据来源）。
   - 改法：P1 对死信和重试条目逐条解码 player_id 分流：purge 范围内的进隔离区；其余留在原队列，或者进一个不设 TTL 的隔离键，并逐条写进报告、保留告警。TTL 只作用于已确认属于回档范围的条目。报告里列出受影响的 storage_elsewhere 玩家。
10. **[medium] 永久围栏 K1 可能被合服工具的现有提示文案引导人手工删除。merge_zone 释放和续期围栏时按子串匹配 run_id，有误续期、误删的面。guild 的围栏读取是可选配置，「不用改任何在线服务」这个前提不完全成立。**
   - 场景：PITR 期间有人对 W→Z 执行合服，SetNX 失败，提示文案让他「确认没有 merge_zone 进程后手工 DEL merge:in_progress:Z」。他照做，回档围栏失效，本 zone 归属的玩家可以在他区重新进场并铸造 owner；下一个模式虽然会以 3 拒绝，但如果已经过了 Step 5，就只能向前。如果显式传入的 -RollbackId 恰好包含某次合服的 run_id，一个残留的 merge_zone keepAlive 会把永久的 K1 PEXPIRE 成带 TTL 的键（fail-open）。guild 没配 MergeMarkerRedis 时，建帮、经济类写不受围栏约束。
   - 证据：tools/merge_zone/fence.go:151-155（「DEL %s manually」）、audit_checks.go:546-550（「DEL them once no merge_zone process is alive」）、merge_run.go:1367-1371；fence.go:73-86 的 releaseFenceScript / refreshFenceScript 用 string.find(cur, ARGV[1], 1, true)；go/guild/internal/config/config.go:60-71（「整段可缺失 = 闸门不生效」）。
   - 改法：merge_zone 遇到非 JSON 值，或以 zone_rollback_discard: / save_barrier_restore: 开头的值时，提示改为「不得手工删除，由持有方工具 -mode release 撤除」（改 fence.go 和 audit_checks.go，计入文件数）。rid 入口校验拒绝形如 merge run_id（src-dst-时间戳-随机串）的值。K2 的值也加 zone_rollback_discard: 前缀，避免与 barrierrestore 的 MaintenanceID 混淆。deploy-facts 记录 guild 的 MergeMarkerRedis 是否配置，未配置时 preflight 告警或拒绝。
11. **[low] 同一 rid 的写模式之间没有执行互斥。purge.json 由进程整体落盘，后写的覆盖先写的，而 verify 以它为输入逐人复核。**
   - 场景：操作者在第一个 purge 还没结束时，另开一个终端用同一 rid 重跑。两个进程各开一轮 pass，后结束的进程写出的 purge.json 覆盖了先结束那份的已处理名单，verify 的逐人复核因此漏人，漏掉的玩家缓存是否删净没有被核对。
   - 证据：设计 §7「没有全局锁」、§4 purge 第 6 步「写 purge.json(累计全部轮次)」、verify「判定参数加 -purge-report」。
   - 改法：每个写模式在 rollback:quarantine:<z>:<rid>:lease 上取一把带令牌、PX 续期的租约，写 Lua 里校验令牌。已处理名单只从 Redis 清单和各 pass 的 ndjson 汇总生成，不依赖单个 purge.json 文件。
12. **[low] 设计依赖他人未提交的在途代码，引用的行号也已漂移。barrierrestore 整个包和 saveoutbox/generation.go 目前都是未跟踪文件；player_lifecycle.cpp 正被他人修改，设计引用的 :4275-4278、:4311-4312 已对不上。**
   - 场景：另一会话改名或删除 barrierrestore，或调整 GenerationKey 的格式，本工具就会编译失败或键名漂移。「工具与线上 go/db 同一提交」的校验，也要求这些文件已经提交。B5 若照旧行号动手，可能改错位置。
   - 证据：git status：?? go/db/internal/barrierrestore/、?? go/db/internal/saveoutbox/generation.go、M cpp/libs/services/scene/player/system/player_lifecycle.cpp；当前快路径在 player_lifecycle.cpp:4332-4334，探针盖章在 4375-4376。
   - 改法：MaintenanceKey 下沉到 shared/savebarrier（或在工具内定义常量，用测试钉死字面量，并在 barrierrestore 存在时交叉断言）。B1 开工前确认 generation.go 已提交，否则在 keys_export 里定义 GenerationKey 并钉死字面量。B5 动手前重新 git diff 定位行号。
13. **[low] 排空的首次全量扫描与在途的 EnterScene 有竞态：scene_manager 先通过 RPC 读 merging 标志，之后才 CAS location，两步不是原子的，所以「他区在线集合只减不增」在围栏刚立起的几秒里不严格成立。**
   - 场景：一次跨区 EnterScene 在围栏写入前 1ms 通过了检查，随后才完成 location CAS。fence 模式如果立即全量扫描，集合为空后马上「再全量确认一次」，两次扫描都可能早于这次 CAS，于是 0g 判为通过；要等到 stopped 才以 3 拒绝，那时 zone 已经停服。
   - 证据：go/scene_manager/internal/logic/home_zone.go:186-188（读到 merging 后拒绝，放行路径在后续步骤才写 location）；go/data_service/internal/logic/rollback_logic.go:144-150 的 guildSettleDelay（立栅栏后先沉降等待的先例）。
   - 改法：获取围栏后先等待 ≥ HomeZoneLookupTimeout + EnterScene 处理上界（建议 30s，同 guildSettleDelay），再做首次全量扫描；集合为空后再隔一个沉降期复扫一次，才判为排空完成。
14. **[low] torn 的处置规则只因为当前 player_database_1 只有探针和围栏列才是安全的；设计已把玩家表推广为全部 OptionPrimaryKey=player_id 的表，以后加列就会失效。分块校验和的 SQL 写法也有精度和分隔歧义问题。**
   - 场景：以后给 player_database_1 加了业务列，或者有新表纳入围栏：torn 时把内容较旧那一行的围栏抬到 V，会把跨表不一致的状态标成「已提交」，工具替玩家绕过了严格协议本来要拦的情形。BIT_XOR(CONV(...)) 不做 CAST AS UNSIGNED 时，字符串到数字的隐式转换可能丢精度；CONCAT_WS('#') 在列值含 '#' 时会碰撞，「全部块相同」可能误判。
   - 证据：proto/common/database/mysql_database_table.proto:171-187（player_database_1 只有 player_id / stress_test_probe / fence_*）；设计 C10（「两张玩家表」改为全部 OptionPrimaryKey=player_id 的表）；设计 §4 stopped 第 3 步的校验和表达式。
   - 改法：torn 处置加一条断言：落后那张表除 player_id、探针、fence_* 外有任何非默认的业务列，即判 blocked（torn_cross_table）。校验和按 pt-table-checksum 的写法：BIT_XOR(CAST(CONV(SUBSTRING(MD5(...),1,16),16,10) AS UNSIGNED))，列值用长度前缀编码或逐列 MD5 后再拼接；并在真 TiDB 上验证。

### 6.2 水平扩展与运行面（结论：needs_changes）

1. **[high] 工具压在全服共用的单实例 Redis 上(所有 zone 的同步登录路径都走它),设计里没有任何限速、在途上限或按延迟退避;全键空间 SCAN 重复跑十几遍。**
   - 场景：purge 时 16 个 worker(上限 64)各推一批 200 段 Lua,每段在 Lua 里做 sha1hex(DUMP 父快照);同时用 pipeline 批量取 DUMP,父快照每人几十 KB,经端口转发慢慢读回。Redis 主线程被占满,回包堆在服务端输出缓冲里(没设 maxmemory,也没设 limits)。后果:其它所有 zone 的 login 拿锁 / EnterGame 超时(EnterLookupTimeout 1.5s),scene_manager 选主锁抖动;最坏情况 Redis Pod 被 OOMKill,全服掉线。SCAN 一次要扫整个实例的键空间(MATCH 在服务端过滤),每名玩家约 30 个键:9 张表缓存、9 个 consumer:applied、PlayerAllData、owner_epoch、location 等。几个 zone 同时回档,负载成倍叠加。
   - 证据：deploy/k8s/manifests/infra/redis.yaml 文件头写明:共享 Redis 在同步登录路径上、仍是单实例;:173-175 不设 maxmemory、保持 noeviction;没有 client-output-buffer-limit 和 lazyfree 配置。tools/scripts/k8s_deploy.ps1 的 Redis 连接总账注释:所有 zone 共连 infra namespace 里同一个 Redis。设计 §7:-workers 缺省 16、上限 64,pipeline 每批 200,SCAN COUNT 1000。§4 P2a 用 pipeline 读 DUMP+PTTL,P2c 在 Lua 内做 sha1hex(DUMP)。全量 SCAN 次数:fence 的 H 遍加排空后全量确认、stopped 的 H/M、inventory 的 M/H/P 三遍、purge 原样重跑 inventory 再加 P1、verify 重跑归类再加 consumer:applied:<topic>:*,合计 12 次以上。
   - 改法：(1) 加全局令牌桶 -redis-ops-per-sec,缺省取保守值;加在途字节上限 -max-inflight-bytes,如 8MiB,按 STRLEN/MEMORY USAGE 分批。Redis 阶段 worker 缺省 4、上限 8,并显式设 go-redis PoolSize=workers+2(不设则默认 10×GOMAXPROCS)。(2) 自适应退避:每批测 PING RTT 或本批 p99,超阈值就暂停并加倍等待。(3) SCAN COUNT 降到 500,页间 sleep。(4) 砍掉重复的全量 SCAN。围栏立起后 home=Z 集合只减不增(前提是另一条 backfill 漏洞修好),stopped 落一份 H 集合快照(64 分片 NDJSON,SHA 记进清单);inventory 和 purge 读快照并逐人 MGET 复核,只在 verify 末尾做一次全量 SCAN 兜底。P1 重试族按已知键名直接枚举:ready、p0..p{N-1}、legacy processing、instances 里登记的实例;只保留一次 processing 前缀 SCAN 兜孤儿。(5) ps1 第 0 步统计 merge:in_progress:* 里值以 zone_rollback_discard: 开头的个数,超过 N 就拒绝。这是准入控制,不是正确性锁。(6) 验收由 Codex 在压测环境测:purge 期间其它 zone 的 entergame p99 退化不超过约定阈值。
2. **[high] 父快照常驻、没有 TTL,所以「删除前把 DUMP 归档到报告目录」实际上是把整个 zone 的玩家数据全量导出,经端口转发写到操作者本机。**
   - 场景：一个 50 万玩家的 zone,父快照每人约 40KB:原始约 20GB,base64 后约 27GB。经 kubectl port-forward(单流、容易断)要传几个小时。中途一断就退出 1,重跑又要先重跑 inventory 的全部判定(见另条)。本机磁盘写满时 fsync 失败,purge 停在半途。玩家资产明文散落在笔记本上。
   - 证据：go/login/internal/logic/pkg/dataloader/new_player_seed.go:70 注释:scene 首次存盘的裸 SET 会去掉 TTL。sync_loader.go:138 的 playerAllDataParentTTL=24h 只在冷读组装时生效。k8s_zone_rollback.ps1 契约 :159 写明 -RedisHost 可以是端口转发;报告目录是 run/logs/zone-rollback/<rid>/。设计 §4 P2a/b:对每个已处理玩家 DUMP 父快照、asset_sql_commit、save_barrier,base64 写进 purge-archive-p<N>-s<NN>.ndjson。
   - 改法：(1) 归档范围收窄到「证据」:save_barrier 和 asset_sql_commit 全量归档(体积小);父快照只归档 strict 下带 pending/outbox 的玩家和 blocked 名单。其余玩家:inventory 在 Lua 里算 sha1hex,只回传 40 字节,purge 的删除 Lua 复核这个 sha1,DUMP 不出 Redis。(2) 全量证据改为停服后给共享 Redis 的 AOF/RDB 留一份副本(ps1 打印运维步骤),或者把工具改成集群内 Job 加 PVC 运行,不走端口转发。(3) inventory 输出预计归档字节数(MEMORY USAGE 抽样累加),超过 -max-archive-bytes 或报告目录可用空间就以 3 拒绝。
3. **[high] 排空和 stopped 判「他区在线」用的是 decideLocationZone,它只判 location 属于哪个 zone,判不了玩家是否真在线。location 没有 TTL,节点崩溃留下的陈旧记录会让排空永远等不完。**
   - 场景：区 Y 的某个 scene Pod 被 OOMKill,上面 30 名 home=Z 玩家的 location 留在 Y,节点已经不在 load 集里,这些玩家之后再没登录。fence 排空会一直把这 30 人算成「他区在线」:30m~6h 后退出 5,重跑还是 5;stopped 的「他区在线即 3」也永远过不去。操作者只能放弃回档,或者手工 DEL location,而那会破坏 scene_manager 交接门禁的证据。
   - 证据：设计 §1 fence 排空:用 decideLocationZone 的同一真值表区分陈旧与他区在线。tools/merge_zone/scene_hot_state.go:277 的 decideLocationZone 只按 zone_id 或 scene:{id}:zone 判归属,不看节点存活。scene_manager/internal/logic/changesceneutil.go 的 UpdatePlayerLocation 是 location 唯一写入口,写入不带 PX;只有 LeaveScene(leavescenelogic.go deletePlayerLocationGuarded)会删。scene_manager 自己判陈旧记录用的是 enterscenelogic.go 的 playerLocationOwnerGone / playerLocationOwnerDead:scene_nodes:zone:<z>:load 的 ZCARD,load_reporter.go:1055 的 IsNodeAlive(ZSCORE),reentryBarrierBlocks;另有 awaitingPlacementExpired。
   - 改法：把 scene_manager 的存活判据下沉到 go/shared/scenenode:纯函数 LocationLiveness 加对应读法,scene_manager 和工具共用,共用测试向量。判法:节点在 scene_nodes:zone:<z>:load 里 → 在线;不在 load 集且 node:zone:<z>:<n>:death_at 已过再入屏障 → 陈旧,可忽略;等待落点按重定向票据是否过期判;读失败或判不出 → 按在线处理(保守)。fence-drain.json 分三类列出:在线 / 陈旧已忽略 / 判不出。新增约 2 个 shared 文件,scene_manager 改 1~2 个,计入总文件数。
4. **[high] 围栏能不能挡住在他区的本 zone 玩家,取决于全服每个 zone 的 data_service 和 scene_manager 都不早于 2026-09-29。设计只核对目标 zone,所以「不改在线服务、滚动升级期间也生效」不成立,旧镜像下会 fail-open。**
   - 场景：zone Y 的 data-service 镜像还停在 2026-09-20,被全局发现选中来应答。home=Z 玩家在 Y 换图时拿到 HomeZoneMerging=false,被放行:location 和 owner_epoch 被推进,存盘写进 topic Z。好的情况是 stopped/inventory 的 owner 复核以 3 拒绝;坏的情况是这发生在 Step 5 切点之后、6b 之前,T 之后的写在 zone-up 后被消费,落进回档后的库。还有一种:以后任何一个 zone 切到 db_first 且 ReadRequiresReady=true,就绪标记不可信时 data_service 回源读库里的 zone_merge_fence 表,只写在 Redis 的 K1 被绕过。
   - 证据：scene_manager/internal/logic/home_zone.go:175:旧版 data_service 不填这个字段,读作 false。home_zone_merging 字段和 rejectHomeZoneMerging 由 4493b3c9f(2026-09-29)引入,git log -S 可查。tools/scripts/k8s_deploy.ps1 的 DataServiceRpc 注释:data-service 按 zone namespace 各部署一份,但发现键 dataservice.rpc 不分 zone,任一 zone 的 scene_manager 都可能由任一 zone 的实例应答。data_service/internal/routing/route_read.go 的 getPlayerHomeZoneAndMergeFenceReady:缓存不可信时调 routeReader.GetHomeAndFence,即 store/route_reads.go:44 的 LEFT JOIN zone_merge_fence。设计 §1 决定 #1 原话「现网所有版本…都只看 EXISTS」。ps1 的 Get-DeployFactsProblems 只读目标 zone 的配置。
   - 改法：ps1 第 0 步 f 和 0g 加全局核对:kubectl 列出所有 zone namespace 加 infra 里的 data-service、scene-manager 镜像 tag,逐个检查 git merge-base --is-ancestor 4493b3c9f <tag>,任一不满足或判不出就拒绝。所有 data-service ConfigMap 必须 RouteStore.Mode=redis 且 ReadRequiresReady 不为 true;否则拒绝,或另批设计让工具同时写 zone_merge_fence。stopped 和 inventory 再核一次,防回档期间别的 zone 发布了旧镜像。runbook 写明:回档窗口内冻结全服 data-service / scene-manager 的发布。
5. **[medium] 全表分块校验和跑在全服共用的 MySQL 上(1 核、128Mi buffer pool),按块并行、不限速,而且 purge 每次重跑都要再全表读一遍。**
   - 场景：一个 20 万玩家、9 张表的 zone:全列 MD5 吃满唯一的 1 核,128Mi buffer pool 被冷数据冲掉。其它 zone 的 go/db 落库延迟飙升、Kafka lag 上涨、冷读登录超时。16~64 条连接远超工具预留 OpsReserve=20,碰上其它 zone 滚动发布时逼近 max_connections=1800。purge 因一次 Redis 抖动退出 1,重跑又是一次全表校验和:停服、inventory、每次 purge,合计至少 3 次全表读。
   - 证据：tools/scripts/k8s_deploy.ps1:所有 zone 的 go/db、data_service、Java gateway 与全局的 friend、trade、player-inbox 共用 infra namespace 里同一台 MySQL;Get-MysqlConnectionPlan 里 OpsReserve=20(含 merge_zone、route_tool、人工会话)。deploy/k8s/manifests/infra/mysql.yaml:172-178:innodb_buffer_pool_size 默认 128Mi,「1 核仍是开服硬瓶颈」;:90 max_connections=1800。设计 §4 stopped:每块 BIT_XOR(…MD5(CONCAT_WS(全部列…)));§7「分块校验和按块并行」;purge 第 1 步「在进程内原样重跑 inventory 的全部判定」,包括重算全部块。
   - 改法：(1) 照 pt-table-checksum 的完整做法来:单连接串行(最多 2 路);按目标块耗时 0.5s 自适应块大小;每块前查 Threads_running,超阈值就暂停(TiDB 用低优先级资源组,并调低 tidb_distsql_scan_concurrency)。MySQL 连接池 SetMaxOpenConns 不超过 2,写进 OpsReserve 说明。(2) 校验和只在 stopped 和 inventory 各算一次,块级摘要写进 inventory.json,SHA 记进清单。purge 和 verify 不重算,只复核 inventory.json 的 SHA 和「停服后库上没有写者」的 Redis 证据:围栏、owner 基线、foreign_home_storage_here=0、没有进行中的搬库(见另条)。
6. **[medium] 进度模型是全有或全无:任何一个错误都取消全部,不做逐项重试,每次重跑都从头盘点。规模一大,长时间的 purge 很难跑完,每次重跑还给共享存储造一次尖峰。**
   - 场景：一个 4 小时的 purge 在第 3 小时因为端口转发断了一次、或单个 pipeline 超过 3s,退出 1。重跑先重做全部判定(多遍全键空间 SCAN 加全表校验和),再从 0 扫全部玩家。高负载下反复失败,可能永远跑不完。
   - 证据：设计 §7:worker 用 errgroup 加可取消的 ctx,任一 SCAN 页、SQL 或 Lua 出错就取消全部、以 1 退出;写操作 MaxRetries=-1,不在工具内重试,由同 rid 的整体重放负责;每次 purge 开新一轮 pass 重扫全部范围;每次 Redis 调用 3s。AGENTS.md §11.3:重试只用于可重试且幂等的操作,须有超时、次数上限、退避与抖动。
   - 改法：SCAN 用同一 cursor 重发、只读 SQL、以及逐人的「读 sha1 → 归档 → Lua」(Lua 接受已抬升、已删除,可重入),遇到网络类错误都做有界重试:3 次,指数退避加抖动。只有命中 owner_advanced / applied_ahead / route_changed / fence_lost / archive_mismatch 这类逻辑拒绝字才整体停。清单按 64 个分片记 shard_done,只作重跑时跳过的提示,正确性仍靠幂等重放;已完成分片在 verify 里抽检。Redis 超时按批大小和在途字节计算,不用固定 3s。
7. **[medium] 围栏只挡 home=Z,挡不住「落点在 Z」和几个离线工具。搬库、钉落点、回填映射都能在回档窗口内改变回档范围,甚至写进正在做 PITR 的库。**
   - 场景：(a) Z 正在 PITR,运维把一批 home=Y 的玩家 relocate 进 zone_Z_db。relocate 按 home=Y 查围栏,不受 K1 阻挡,行写进正在恢复的库:恢复后要么被覆盖,要么留下半截。inventory 判出 foreign_home_storage_here 退出 6 时,PITR 已经做完了。(b) inventory 报 legacy_owned blocked,操作者按提示跑 merge_zone -backfill-home-zone(它不查围栏)。这些玩家变成 home=Z,却不在 stopped 的 owner 基线里,inventory 按「不在基线里却在范围内」以 3 拒绝。停服基线在 PITR 之后无法重做,回档卡死在 4a。
   - 证据：tools/merge_zone/relocate.go:278 placementCAS{FenceZone: rd.Home};pin_placement.go:98 FenceZone: zone;placement_ops.go:472 只查 merge:in_progress:{FenceZone}。go/data_service/internal/routetool/importer.go:40 ImportNote:relocate / pin-placement / backfill 不立围栏,冻结窗口由人保证。tools/merge_zone/backfill_home_zone.go:230 wp.SetNX(player:zone…) 不查 merge:in_progress。设计 §2「这些 Redis 来源在停服之后不会变(围栏加停服)」;§6 foreign_home_storage_here 只在 inventory(PITR 之后)判 blocked;legacy_owned 的修复入口是 merge_zone -backfill-home-zone;§2 inventory「不在基线里却在范围内的玩家也算违例 → 3」。
   - 改法：(1) relocate 和 pin-placement 的 CAS 把源落点 zone、目标落点 zone 的 merge:in_progress 也放进 KEYS,存在即拒。backfill_home_zone 改成在一段 Lua 里先查 merge:in_progress:{zone} 再 SETNX。(2) preflight、fence、stopped 三处只读判 foreign_home_storage_here(player:zone≠Z 且有效落点=Z,只看 Redis 就能判)和 legacy_owned(本库有行而无映射,只读 SQL 可判),存在就以 3 拒绝,要求在立围栏之前修好。(3) -topology-manifest-dir 下有涉及 Z、还没走到 switched 或 aborted 的 relocate 清单就以 3 拒绝。tools/merge_zone 预计多 4~6 个文件(含测试),计入总数。
8. **[medium] 新告警 ZoneEntryFenceHeldTooLong 的指标名和分组标签都写错了,照设计原文落地会永远不触发。而且它只能靠有人重试进场才有样本。**
   - 场景：回档在 Step 4 前放弃,走中止路径 zone-up,但忘了 release。本 zone 归属玩家全部进不了场景,告警规则查的是一个不存在的序列,不会报警,只能等玩家投诉。zone 一直没 zone-up、或玩家已经放弃重试时,同样没有信号。
   - 证据：go/scene_manager/internal/metrics/metrics.go:33 const subsystem = "scene_manager",所以真实指标名是 scene_manager_enter_scene_rejected_total,标签是 {zone_id, reason};zone_id 是 gate zone,见 home_zone.go 的 rejectHomeZoneMerging。deploy/k8s/scene-manager-alerts.yaml 现有规则一律用 scene_manager_enter_scene_rejected_total,按 zone_id 分组。设计 §8 写的是 sum by (namespace)(increase(enter_scene_rejected_total{reason="home_zone_merging"}[30m]))>0。
   - 改法：表达式改为 sum by (zone_id) (increase(scene_manager_enter_scene_rejected_total{reason="home_zone_merging"}[30m])) > 0,补 promtool 规则测试。再加两道不依赖流量的兜底:ps1 Step 7 硬断言 K1、K2 都已不存在;k8s_deploy 的 zone-up 结束时只读 GET merge:in_progress:<zone>,如果值以 zone_rollback_discard: 开头、而报告目录里没有未完成的回档 entry-state,就打印醒目 WARN 和 release 命令。
9. **[medium] release 继承了「每个模式开头」的全部前置检查(deploy-facts、MySQL 身份、玩家表交集、镜像提交一致)。撤围栏会被这些和撤围栏无关的检查卡住,zone 被永久封锁。**
   - 场景：中止路径:Step 3 已经恢复到影子库后放弃,原库改了名、影子库没换回,SELECT DATABASE() 或表交集过不了,release 以 3 拒绝,本 zone 一直封着。另一种:Step 6 用不同的 go/db 镜像起了 zone,工具与镜像提交不一致被拒。还有一种:新开终端里 DSN 环境变量没了。
   - 证据：设计 §4「每个模式开头」:deploy-facts Schema 3;MySQL SELECT DATABASE()==placement.StoreDBName(zone);玩家表 ∩ information_schema。C1:新增 fence / release,沿用公共参数(含 -mysql-dsn-env、-deploy-facts、-barrier-protocol)。契约 :160-162:用 -deploy-facts 的 DbImage 复核构建身份。k8s_zone_rollback.ps1:2467:先恢复到影子库再替换,原库改名保留。
   - 改法：release 只做三件事:INFO 的 run_id 等于 -expect-run-id、cluster_enabled:0、按值 CAS 删 K1 和 K2。不读 deploy-facts、不连 MySQL、不比镜像。只需 -redis、-expect-run-id、-zone、-rollback-id 四个参数;值不等于本 rid 时打印当前值。fence 同样只依赖 Redis 和 preflight 已落盘的结果。补测试:MySQL 不可达、deploy-facts 缺失时 release 仍成功。
10. **[low] kafka:retry:guard 键里 topic 不在前缀位置,按设计的 MATCH 规则拼不出正确的按 topic 匹配,容易误删全服的键。而这些键 5 分钟就自然过期,根本不需要删。**
   - 场景：实现者照键族表用 RetryGuardKeyPrefix 做 SCAN MATCH kafka:retry:guard:*,UNLINK 掉全服所有 zone 的认领 / ack 防重记录。其它 zone 的 go/db 在 go-redis 内部重发时可能重复 POP、重复 ack,还白白多一次全键空间 SCAN。
   - 证据：go/db/internal/kafka/retry_ownership.go:103-104:retryGuardKey = "kafka:retry:guard:" + kind + ":" + processingKey + ":" + nonce,processingKey 形如 kafka:retry:processing:<topic>[:inst];:88 retryGuardTTL = 5 * time.Minute。设计 §5 键族表:kafka:retry:guard:* 用 kafka.RetryGuardKeyPrefix,处置 UNLINK;MATCH 规则为 EscapeRedisGlob(含 topic 的前缀) + 边界 + '*'。
   - 改法：从 purge 里删掉 guard 族:停服加 PITR 远超 5 分钟,这些键早已过期。verify 只对 kafka:retry:guard:{claim,ack}:kafka:retry:processing:<转义后的 topic> 加 ':' 边界这两种显式模式断言为 0。补测试钉住 db_task_zone_11 的 guard 键不被碰到。
11. **[low] 隔离键靠 PEXPIRE 720h 过期释放。共享实例没开 lazyfree-lazy-expire,大列表或大 HASH 到期时会在主线程同步释放。**
   - 场景：30 天后某个时刻,主动过期循环同步释放一个几十万元素的死信列表或死信索引 HASH,主线程阻塞几十到几百毫秒,全服登录同时抖动。时间点不可预期,当时也没人值守。
   - 证据：deploy/k8s/manifests/infra/redis.yaml 没有 lazyfree-lazy-expire(Redis 缺省 no),实例单点且在同步登录路径上。设计 §4 P1:RENAMENX 后 PEXPIRE(-quarantine-ttl,缺省 720h),对象包括 kafka:dead:queue:<topic> 列表和新增的 kafka:dead:index:<topic> HASH;这两者会长期累积。
   - 改法：隔离键不靠 TTL 释放:新增 -mode quarantine-drop,人工复核后用 UNLINK 删除(UNLINK 对大键是异步的);PEXPIRE 只作远期兜底,写进 runbook。也可以单独申请在 redis.yaml 打开 lazyfree-lazy-expire yes,但这是持久配置变更,需负责人批准。
12. **[low] 复用 merge:in_progress 带来全服副作用和误操作面:任何一个 zone 的回档围栏都会挡住全服的 route_tool;合服撞上围栏时的报错文案会引导操作者手工 DEL;RemapHomeZoneForMerge 只看围栏在不在。**
   - 场景：Z 回档持有围栏数小时(排空最多 6h,再加 PITR),这期间全服 route_tool import/repair 都跑不了。另一名操作者对 Z 发起合服撞上围栏,照报错提示手工 DEL,回档中途围栏就没了;只有到下一个复核点才会发现。有人持 admin token 误调 RemapHomeZoneForMerge(source=Z),会因为「围栏存在」被放行。
   - 证据：go/data_service/internal/routetool/importer.go:18 和 repair.go:22:SCAN merge:in_progress:*,有一把就退出 3。tools/merge_zone/fence.go:155-157 的报错原文:if that run is dead, DEL %s manually after confirming no other merge_zone process is alive。data_service 的 constants.ErrCodeMergeFenceMissing:RemapHomeZoneForMerge 只要求源区围栏存在,不校验持有者。
   - 改法：merge_zone 的 acquireMergeFence 撞围栏时识别 zone_rollback_discard: 前缀,报错改为「回档进行中,禁止 DEL,请用 zone_rollback_discard -mode release」。route_tool 只对自己作用范围内的 zone 的围栏拒绝;不改的话,在 runbook 里写明全服暂停的代价。RemapHomeZoneForMerge 不校验持有者,登记为残余风险。tools/merge_zone/fence.go 及其测试计入总文件数。
13. **[low] K2 的键名来自另一会话还没提交(未跟踪)的 barrierrestore 包,回档工具和别人的在途代码有编译耦合。**
   - 场景：B2a 编译依赖一个未提交、可能改名或被放弃的包。对方改了 MaintenanceKey 的拼法,回档与 save_barrier_restore 之间的互斥就静默失效。
   - 证据：git status 显示 ?? go/db/internal/barrierrestore/ 和 ?? go/db/cmd/save_barrier_restore/。设计用 K2 = barrierrestore.MaintenanceKey(restore.go:421),同时声明「不改 barrierrestore」。
   - 改法：B1 把 MaintenanceKey 定义在已跟踪的 go/shared/savebarrier,用测试钉住字面量 player:save_barrier:maintenance:<z>,回档工具只依赖这一份。barrierrestore 落地时改为引用它,由对方会话来改。
14. **[low] 重开之后的冷读洪峰没有真正的节流:zone 令牌桶缺省值很高,登录排队又还没放开。**
   - 场景：6b 撤围栏的一瞬间,排队重试的玩家以最高 2000/s(burst 5000)涌入。#7 删了全部缓存,每人一次 9 表冷读,经 go/db Z 打到 1 核的共享 MySQL,其它 zone 的落库跟着排队。
   - 证据：tools/scripts/k8s_deploy.ps1 削峰档位:ZoneDefaultRps=2000、ZoneDefaultBurst=5000,标注 pending stress-test validation;$script:LoginQueueMergeGateCleared=$false,登录排队不可用。mysql.yaml 注释「1 核仍是开服硬瓶颈」。设计 #7:范围内玩家一律删缓存、首登冷读。
   - 改法：ps1 Step 6 给回档 zone 传显式的低档 zone rps,作为重开档位,具体数值由 Codex 压测给出。Step 7 观察 MySQL 负载和 Kafka lag 后再逐级调高。没有压测数据之前,不写「冷读洪峰由令牌桶承接」。

## 附录 B 两路代码核实结论并入索引

| 核实条目 | 并入位置 |
|---|---|
| 围栏·证实 1 读方清单与判据 | §2.1 读方清单 |
| 围栏·证实 2 能挡住的面 | §2.1 能挡住的面 |
| 围栏·证实 3 与合服互斥 | §2.1 互斥 |
| 围栏·证实 4 同实例前提 | §2.1 同实例前提 |
| 围栏·证实 5 Lua 内 INFO 先例 | §2.1 同实例前提;§2.4 `leaseScript` |
| 围栏·证实 6 被证实的复核意见 | §6 对应行 |
| 围栏·需改 1 证据定位 | §2.1 读方清单 |
| 围栏·需改 2 #1 理由 | §1 #1;§2.1 生效前提 |
| 围栏·需改 3 barrierrestore 先例降格、K2 引用 | §1 #1;§2.1 键与值;§6.1.12、§6.2.13 |
| 围栏·需改 4 K2 定位 | §1 #1;§2.1 键与值 |
| 围栏·需改 5 guild 写全拒 | §2.1 读方清单与政策 |
| 围栏·需改 6 「只减不增」前提 | §2.1 政策与挡不住的面 |
| 围栏·需改 7 RemapHomeZoneForMerge 停用 | §6.2.12 |
| 围栏·需改 8 guild MergeMarkerRedis | §6.1.10 |
| 围栏·需改 9 ps1 契约冲突 | §2.9 C1–C3 |
| 围栏·需改 10 判死不能照搬纯 Redis | §2.1 他区在线判据 |
| 围栏·需改 11 无可复用告警 | §2.8 |
| 围栏·需改 12 release 语义收紧 | §2.1 release;§2.8 退出码 7 |
| 围栏·漏项 A1–A7 挡不住的面 | §2.1 挡不住的面;RollbackFence 差距 |
| 围栏·漏项 B1–B6 误伤 | §2.1 政策;§3 B3、B6 |
| 围栏·漏项 C1–C4 等下线的证明 | §2.1 fence 模式与他区在线判据 |
| 围栏·漏项 D 新旧版本并存 | §2.1 生效前提与最低提交 |
| 围栏·漏项 E 撤围栏与忘撤 | §2.1 政策;§2.8;§2.9 C11、C13 |
| 小项·(a) 证实 1–8 | §1 (a);§2.10 (a) |
| 小项·(a) 需改 A1–A9 | §2.10 (a) 写入点表、脚本、回调结果码、指标 |
| 小项·(a) 漏项 1–6 | §2.10 (a);§3 B4;§4 B4 |
| 小项·(b) 证实 1–7、需改 B1–B3、漏项 7–9 | §1 (b);§2.10 (b);§3 B5、B6;§4 B5 |
| 小项·(c) 证实 1–4、需改 C1、漏项 10 | §1 (c);§2.10 (c);§3 B5 |
| 小项·通用 11 Java 不适用 | §2.11 |

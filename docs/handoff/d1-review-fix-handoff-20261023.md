# 交接:D1 最终语义评审与修复(10-19 ~ 10-23)——移交「映射直读 TiDB」新方向

> **给接手会话**:负责人 2026-10-23 在会话「账号登录逻辑/物理服瓶颈」拍板,映射改为**直读 TiDB**(选项原文「直读 TiDB(推荐)」):去掉 Redis 映射副本,`player_route` 只在 TiDB 一份,登录、进场、go/db 直接 SQL 点查,go/db 在同一 TiDB 事务里核落点;删掉就绪标记、对账 leader、预热、发布等待。库为真源、路由表、围栏落库保留。新方向由该会话统一落码(负责人同日在本会话确认「交给那个会话统一做」)。
>
> 本文记录 D1 最终评审修复这条线做到了哪、哪些结论在新方向下仍然成立、哪些作废,供新设计吸收。**本文提到的所有代码改动都没有由 Claude 编译或运行**(AGENTS §10.1);Codex 10-21 的运行证据只覆盖当时的树,见 §6。

## 0. 结论

- 两轮独立评审(各 6 个维度 + 逐维对抗复核)合并后约 50 条独立问题(原始 103 条,含 must 3 条)。修复分三轮推进,被额度和协调失误打断过多次;10-23 因方向变更停在第三轮。
- **新方向下仍成立、且已落码(未验证)**:merge_zone 围栏接手 / 续期 / 换源拒绝等(§2.1)、五张路由表的 proto 列漂移守卫、`data_consistency_check` 读失败判 fail、落点告警与 `metrics.PrimePlacement`。
- **新方向下作废**:对账 leader 的全部发现(含 must A1-1)、r2-rr#5、go/db 就绪闸、data_service 读方门禁、发布专用 Redis 客户端与 WAITAOF、就绪相关告警与连接总账、k8s_deploy 的 D1 开关粘滞核对(§3)。
- **不在本线**:r2-mz#1(会话「数据覆盖审计交接文档」)、`router.go` 的 `AcquirePlayerLock`(会话「数据服务路由审计收尾」)、`go/db/internal/saveoutbox/replay.go`(会话「数据覆写审计 R2 交接」),见 §4。

## 1. 现状要点(接手前必知)

- **随仓配置已是「全球默认」**(10-21 全球权威线,提交 `1b41d8f1e`):`go/data_service/etc/data_service.yaml` 的 `RouteStore.Mode=db_first`、`Reconcile.ArmReadyMarker=true`、`ReadRequiresReady=true`;`go/db/etc/db.yaml` 的 `Placement.Required=true`、`RequireRouteReady=true`;guild 仍 false。代码组件缺省仍是 redis / false。新方向删掉副本后,这几个开关与 k8s_deploy 入口硬闸(`k8s_deploy.ps1` 约 :741-745)要一起收掉。
- **项目未上线,玩家数据可全部清空**(负责人 10-23):不必为存量表、存量 Redis 键、旧二进制混跑写兼容或迁移。
- 未提交改动(停手时):只剩 ds-read 轨的 `go/data_service/data_service.go`(M1-5 拒启)、`internal/svc/servicecontext.go`(M1-6 预置)、`internal/metrics/metrics.go`(`PrimeRouteEnumerate`)、`internal/config/config.go` 与 `etc/data_service.yaml`(注释)、新测试 `internal/metrics/route_enumerate_prime_test.go`、`internal/svc/route_read_gate_test.go`。都属读方门禁,新方向下删改即可。其余改动已被本机自动快照提交收进 HEAD(`c35e1133d` 起若干「保存…」提交)。

## 2. 新方向下仍成立的结论

### 2.1 tools/merge_zone(已落码,未验证;round3 的复核被打断,**没有经过最终对抗复核**)

| 编号 | 问题 | 现状 |
|---|---|---|
| B1-1 / MZ2-1(must) | `-take-over-fence` 接手成功后,镜像写失败或结果不明时会把刚接手的库围栏整个释放,半合服的两个区失去围栏 | 已改:接手路径与合服续跑失败一律保留围栏(`keepTakenOver` / `keepOnFailure`);用例 `TestAcquireRouteFence_TakeOverMirrorFailureKeepsTheDBFence`、`TestAcquireRouteFence_TakeOverFailureNeverCleans` 等(fence_db_test.go)。新方向去掉镜像后,「镜像写失败」分支消失,但「结果不明不释放」的原则要保留 |
| B1-2 / MZ2-2 / MZ2-3 | 接手只核 run_id;旧进程失权感知窗口长达 ttl/3 | 已改:接手核 zone 对、清单路径、每 zone 只许 0 或 64 片;续期间隔 `min(ttl/3, 30s)`(`routeFenceRenewMaxInterval`);真库用例 `TestIT_Route_TakeOverChecksZonePairAndManifest`,单测 `TestCheckTakeOverShard` 等 |
| MZ2-4 / B1-4 | 批事务已判失权,中止文案却说「围栏仍归本次」 | 已改 `routeFenceAbortLost` / `abortKeepingFence(cause, errs...)`,unmerge 各调用点传错误;`TestRouteFenceAbortLost`、`TestRouteFenceKeptAbortMessage` |
| MZ2-5 | Held 文案指向不存在的释放入口 | 已改为「`-take-over-fence <run_id>`(同一 `-manifest-path`)」 |
| MZ2-6 | 释放 / 清理用 `context.Background()`,库黑洞时退出卡死 | 已改为 30s 时限;`TestRouteFence_ReleaseAndCleanupAreTimeBounded` |
| MZ2-8 | 步骤 7 期间失权只打 WARN、退出码 0 | 已改:打 Done 前再查 gate,失权非 0 退出 |
| B1-5 / MZ2-9 | db 源撤销有 4 处 `log.Fatalf` 不留围栏、无续跑指引 | 已改:7' / 4' / 1' 等 db 源走 `abortKeepingFence`,redis 源逐字不变 |
| B1-3 / R8-2 / B2-1 | 合服清单、relocate 清单不记 route source,换源续跑会把一次合服劈成两个真源 | 已改:两份清单记 `RouteSource`,续跑与 relocate-abort 来源不一致**一律拒绝**,文案给出路(`relocateRouteSourceRefusal`);撤销「清单 db 配 redis」在围栏前拒绝(`checkUnmergeRouteSource`)。新方向只剩 db 源后,这一套可以简化成「清单必须是 db」 |
| B2-4 / R8-3 | `verify:merge_fence` 在 db 源下教运维 DEL 镜像 | 已改:库围栏生效时让持有者释放 / 等过期,禁止 DEL;库读方没装成报 INFRA |
| R8-1 | storage-audit 在 db 源下不核 `writable` | 已改:扫描前后各核 `writable`,不为 1 或读不到即 exit 2;`TestAuditStorageChecked_FrozenRouteWritersGiveNoVerdict` |
| R8-5 | relocate read 在 `writable=0` 时仍往缓存推 | 已改 `RequireWritable` 预检 + 回退提示(缓存部分新方向作废) |
| MZ2-7 / R8-6 / B2-3 | 路由库入口与读原语三套平行实现 | **未做**(去重轨没开始)。另一会话已做 R8-6 最小合并(`scanRouteDBPages` 按页回调)。去掉 redis 源后重复可能自然消失,改写时顺手收成一处 |
| B1-6 | r2-mz#1 新写入是否走同一门禁 | 只读核对:是。copy 合服改为保留屏障(少一个写),撤销 5'b 仍在 `requireFence` 之下删 |

撤销侧、文案类的其余小项(B1-7 / R8-8、B2-2 / R8-4、B2-6、B1-8)已改,多数与发布 / 缓存相关,新方向下随发布一起删。runbook `docs/ops/merge-zone-runbook.md` §3、§15.4 已按上述行为改写,新方向下要随之重写。

### 2.2 其他仍成立的

- **五张路由表的 proto 列漂移守卫**:`proto/common/database/rollback_database_table.proto` 末尾五个 message(已随 `10fa5d939` 生成);`go/data_service/internal/store/schema.go` 用 `routeTableMessages()` 守列名与主键,删了 `routeTableShapes`;四张表的 DDL 已补 `pb:N` 列注释,`TestRouteBootstrapDDLContract` 五张表都核字段号;`store.RouteRow` 写明是领域类型(要表达 NULL)。新方向若删掉 `route_reconcile` 等表,同步删 proto message、DDL 与 `routeTableMessages`。
- **`tools/data_consistency_check`**(A3-2 / M1-3):读失败判 fail(退出 1),db 口径计数改单条 `SELECT COUNT(*) FROM player_route WHERE home_zone IS NOT NULL`,好友样本扫描失败整条判 fail;用例 `TestFriendOrphansScanErrorFailsWholeCheck`、`TestPlayerHomeZoneCountFailureFailsCheck`、`TestPlayerHomeZoneHeuristicUnchanged`。redis 口径随副本删除。
- **落点告警**:新文件 `deploy/k8s/player-placement-alerts.yaml`(stale_topic / missing_required / store_rejected / store_open_errors / frozen / recheck_moved / home_zone_merging);go/db 在消费者构造时调 `metrics.PrimePlacement()` 预置 `db_placement_guard_total`(op × 9 种 outcome)与 `db_placement_store_open_total{result}`。**待做**:据预置去掉四条「出现即报」告警的 `unless … offset 10m` 第二支,同步 `k8s_deploy_contract.tests.ps1` 约 :8273-8276;新方向改了 outcome 集合时一起改。
- **`data_service` 读路径的调用方取消口径**(M1-2):读失败时 ctx 已结束不计 `sql_error`、不回 Unavailable(30)(`callerAbandoned`,`route_read_abandon_test.go`、`server/home_zone_ctx_test.go`)。SQL 点查保留后这个口径仍然需要。
- **部署脚本**(新方向下仍需要的部分):dev_tools 转发 `-MergeZoneTakeOverFence`;`merge-zone-capability-check` 漏参退出码改为 2(与 missing 的 1 区分);`k8s_route_tool.ps1` 入口兜底退出码 2;CI `deploy-config-tests.yml` 的 paths 已补 D1 契约测试读的 Go 源码。未做:B3-10 / OPS-8(脚本里镜像的 Go 常量没有机械守卫)。
- **发布清单**:`docs/ops/release-checklist.md` 加「写过落点记录之后禁止回退到不认落点的 go/db 版本」——**未做**,落点记录仍在,建议新方向一起加。

## 3. 新方向下作废的结论(不必再修)

- 对账 leader:A1-1(must,Arm=false 下水位键重建破坏全量追平判据)、A1-2、A1-3 / LR-3、A1-4 / LR-1(每拍全库 SCAN;10-20 另一会话已改成跨拍增量 SCAN)、A1-7 / LR-9、A1-8、LR-5、LR-8、LR-10、LR-12。
- **r2-rr#5**(R2 审计 §3.7):mapping Redis 重启 / 切主后挂标记要等全键空间 SCAN,全服 go/db 关闸停存盘,积压可能超出 Kafka 保留期。随就绪闸一起消失;它连带的「补投器不看闸、放大 Kafka 写入」由 R2 会话在 `saveoutbox/replay.go` 处理。
- 读方门禁:data_service 的 A1-6 / A2-11 / M1-5 / M1-6,go/db 就绪闸的 B3G-1 / B3G-2 / B3G-4 / B3G-5 / B3G-6 / A3-4 / A3-6 / B3-8,guild 的 M1-4 / M1-8,`go/shared/placement` 的就绪脚本与水位键。
- 发布:A2-1 / LR-4(发布 + WAITAOF 专用 go-redis 客户端 `publish_redis.go`)、Redis 连接总账、`RouteStore.PublishRedisPoolSize`。
- 部署:k8s_deploy 五个 D1 开关的转发、粘滞核对与组合门禁(B3-1 / B3-2 / OPS-1 / OPS-2 / OPS-3 / OPS-9 / S2-missed-0)、`data-service-route-alerts.yaml` 里副本 / 就绪 / 发布类告警(A2-6 / B3-9 / A3-1 / B3G-3)、清库脚本的 leader 探测(OPS-10)。
- 过时文案类(A1-5 / A2-3~A2-10 / LR-7 / LR-11):随相关代码删改。

## 4. 其他线的归属与在途

- **r2-mz#1**(copy 合服保留 save_barrier):会话「数据覆盖审计交接文档」。段落:`player_rows.go` 702-819、`merge_run.go` copy 分支约 514-519、`unmerge.go` 5'b 约 305-309、`audit_resources.go` 第 388 行,新文件 `save_barrier_verify.go`、`player_rows_barrier_test.go`、`save_barrier_integration_test.go`,runbook §10.1 / §8 Step 5 / §5。10-22 只读核对:代码、单测、集成测试齐,静态自洽;没编译过;两种 route 源走同一调用点;没有端到端「copy 合服 / 撤销后断言屏障」的集成测试;审计文档 §5 的 r2-mz#1 行未标已修。
- **`go/data_service/internal/routing/router.go` 的 `AcquirePlayerLock`**(约 637-662)、`lockAcquireFailure`(约 665)、`router_lock_test.go`:会话「数据服务路由审计收尾」。
- **`go/db/internal/saveoutbox/replay.go`**:会话「数据覆写审计 R2 交接」改「闸关时补投跳过、不计次」。另有一处范围问题留给新方向:它跨 zone 迁移索引前直接 `GET player:zone:{id}`,新方向下要改成读 TiDB。
- `tools/merge_zone/blob_lock_integration_test.go` 的 `requireIT(t)`:会话「数据服务路由审计收尾」已补。
- `tools/scripts/k8s_zone_rollback.ps1`:R9 在重写,D1 这边从没碰。

## 5. 协作上踩过的坑(新会话照做)

- **不要给正在跑的 workflow 子代理发 SendMessage**:代理会被「恢复」并丢掉近期上下文,认不出自己刚做的 Edit,误报「另一个写者」;按这个误报去 TaskStop,停掉的是唯一实例。补充信息写进 scratchpad,下一轮提示里带上。
- 同一份交接被贴进过三个会话(本会话、「D1 剩余收尾交接」、续上的旧会话),多次两边同改同一批文件。开工前 `ListAgents` 先分工;批量点 Try again 会把已移交的会话重新拉起来。
- 本机自动快照提交会随时把工作区整个收进 HEAD,核对「谁改了什么」要看 transcript 或进度记录,不能只看 `git diff`。

## 6. 验证状态

- Codex 10-21(见 PROGRESS「服务器全部分支整合:最终必要验证」「当前 DS 四阶段绿」):data_service Build / Unit / Vet / Runtime 通过(Unit 1628 通过、90 外部跳过),routing、routereconcile 两包通过;当时的树不含 10-22 之后的修复。go/db、guild、merge_zone、data_consistency_check、PowerShell 契约测试在 10-22 之后的改动**都没有运行证据**。
- 新方向落码后,至少要让这些测试保持或改绿(删副本时同步改):`go/data_service/internal/store` 的 `TestRouteBootstrapDDLContract` 等;`tools/merge_zone` 全部单测与 `-tags merge_integration` 的围栏接手 / 续期 / 换源用例(先用 `data_service -migrate` 建一次性路由库,`MERGE_ZONE_IT_REQUIRE=1`);`tools/data_consistency_check` 单测与 `-tags integration` 的 `TestRouteDBIntegration`;`go/db/internal/metrics` 的 PrimePlacement 用例;`tools/scripts/tests/` 的 `k8s_deploy_contract`、`dev_tools_merge_zone_contract`、`k8s_route_tool` 契约。真库用例逐条报 PASS / FAIL / SKIP,MySQL 通过不能代替 TiDB。

## 7. 资料

- 两轮评审原文与逐条复核结论、各轨进度、收口笔记:本会话 scratchpad `C:\Users\luyua\AppData\Local\Temp\claude\D--luyuan-xuanming-server-mmo-cpp\963c681b-709d-4243-9371-9dcf9f484ceb\scratchpad\`(`reviews_full.json`、`reviews_index.md`、`findings-*.json`、`progress-*.md`、`notes.md`、`d1-other-session-report-20261022.md`、`report-godb-round2b.json`)。这是临时目录,可能被清理;上面 §2、§3 已摘出全部结论。
- 另一会话「D1 剩余收尾交接」的评审与逐条 Edit 原文:`C:\Users\luyua\AppData\Local\Temp\claude\D--luyuan-xuanming-server-mmo-cpp\4f79c4d0-fb89-4d35-a06b-b3515151a76a\scratchpad\`。
- 前序交接:[placement-line-handoff-20261019.md](placement-line-handoff-20261019.md)(落点线总入口,§3 更正与 §4 剩余工作中副本相关部分随本文作废)、[handoff-d1-remaining-20261019.md](handoff-d1-remaining-20261019.md)。

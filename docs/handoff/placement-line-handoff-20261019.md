# 交接:玩家存储落点这条线——完成度核查与剩余工作(落点 + D1 映射持久化,2026-10-19)

> **2026-10-23 更新**:负责人拍板映射改为直读 TiDB,Redis 副本、就绪标记、对账 leader、发布等待都删掉,由会话「账号登录逻辑/物理服瓶颈」统一落码。本文 §3、§4 里与副本 / 就绪 / rewarm 相关的待办随之作废;D1 评审修复做到哪、哪些结论在新方向下仍成立,见 [d1-review-fix-handoff-20261023.md](d1-review-fix-handoff-20261023.md)。
>
> **给新账号**:先按 AGENTS.md「会话启动门禁」读完规定材料,再读本文。开场指令在 §9,可直接复制。
>
> **怎么来的**:2026-10-19 用户问「09-28 那批落点做完没有」。本会话做了一次只读核查:6 路并行核查,加 1 路复核抽查。只读 git 和文件,**没有跑任何编译、测试或脚本**。核查时 HEAD 为 `c63b22dca`,工作区快照时间 11:03。
>
> **范围**:09-28「玩家存储落点(placement)+ 合服工具缺口修复」,以及它的延续 D1「玩家归属与落点映射持久化」。
>
> **与已有交接的关系**:
> - D1 代码收尾的做法见 [handoff-d1-remaining-20261019.md](handoff-d1-remaining-20261019.md),D1 会话停下前写的,**有几处要按本文 §3 更正**。
> - 开服线其余工作见 [handoff-launch-line-remaining-20261019.md](handoff-launch-line-remaining-20261019.md)。
> - 本文不重复两份交接的内容,只补全局状态、更正和漏项。

## 0. 结论:没做完

| 层面 | 状态 |
|---|---|
| 09-28 落点主线代码 | 已随 `4493b3c9f`(09-29)入库。当前 HEAD 上关键实现都在:`-mode capability-check`、go/db 每条任务按 `player:placement:{id}` 选库、90s 能力心跳、全局库 1000000、C++ db_task topic 世代号 |
| D1 第 0~3 批代码 | 已落码。大部分被本机自动快照提交收进 HEAD,最后一个碰 D1 路径的提交是 `53361d055`。merge_zone 第 2 批写入点等三十多个文件**仍只在工作区**,见 §5.1 |
| D1 收尾 | 没做。包括最终语义评审、路由表 proto 列漂移守卫和若干零碎项,见 §4 第 1 步 |
| 运行验证 | **基本没有**。09-29 那轮是 Claude 执行的,只覆盖当时的树。之后只有三项:data_service 默认单测 10-19 跑绿一轮(518 个顶层);部署契约脚本 10-19 九份全过(当时的树);`cross_zone_test` 313/313。D1 的真库、真 Redis、merge_zone 集成测试、`-race`、L1 都没人跑过,10-12 的复验清单一条也没执行 |
| 启用 | 没有任何环境启用。开关全是安全缺省(§2.3),全局库 `player_store_1000000_db` 从没建过 |
| 上线前大项 | D8 应用级回档、压测、真集群演练、TiDB Phase 1、L-18 复核都没做,见 §6 |
| 负责会话 | D1 会话周额度用完,10:46 写完交接后已停。`tools/merge_zone` 等 D1 文件现在**没有正在跑的负责会话**,见 §5.3 |
| Java 版(AGENTS §12) | 落点对 Java 不适用(Java 单库),PARITY 第 143 行已记为有意差异。D1 行在 Java 工作区已改为「Java 待做(部分不适用)」,**未提交**;Java HEAD `0e4a613` 里仍是纯「不适用(结构不同)」 |

## 1. 必读顺序

1. 本文。
2. [handoff-d1-remaining-20261019.md](handoff-d1-remaining-20261019.md):D1 收尾怎么做,冲突处以本文 §3 为准。
3. 提案 [player-route-mapping-durability-proposal.md](../design/player-route-mapping-durability-proposal.md),依次读:
   - §13:设计修订 M1–M5、各批落码、上线与回退、残余风险;
   - §11.2 进度表;
   - §12.1「对 §12 的更正」。
4. [PROGRESS.md](../PROGRESS.md) 条目「2026-10-19 D1 剩余批次落码完成」(约第 13296 行):列了全部改动文件和交 Codex 的第 0~9 步。第 0 步已经过时,见 §4 第 2 步。
5. 落点设计 [player-storage-placement.md](../design/player-storage-placement.md):§0、§8、§13(上线顺序与回退)。
6. 运维手册 [merge-zone-runbook.md](../ops/merge-zone-runbook.md):§5.1、§9、§14、§15。
7. [placement-reverify-checklist-20261012.md](placement-reverify-checklist-20261012.md):**只当素材,不要原样交 Codex**,原因见 §4 第 2 步。

## 2. 现状细节

### 2.1 09-28 落点主线

- **设计要点**:归属区 `player:zone:{id}` 与落点记录 `player:placement:{id}`(`"{storage_id}:{version}"`)拆开;选库只在 go/db 一处,按当前记录选。两份数据都在 data_service 的 mapping Redis(DB 0)。
- **代码**:`tools/merge_zone/capability_check.go` 自 `4493b3c9f` 起没改过。`go/db/internal/kafka/placement_route.go` 此后被 6 个提交改过,这 6 个提交都没有运行证据:`e4ff1d165`、`ebccaf04d`、`54c9d775d`、`de4960bee`、`8b9592de3`、`53361d055`。
- **09-28 条目「剩余风险 / 待定」(约第 6015–6030 行)的现状**:
  - 已结:
    - 能力标记 TTL 与合服 T-0 冲突(C'7 已落码);
    - L1 清单补包。
  - 部分:
    - 多 data Redis 的 pin 合服:保留拒绝。10-19 又加两道守卫(`checkDataRedisBlobFlags`、拷 blob 前查锁),在工作区,未编译。
    - 删号不删 `player:placement`:生产删映射的路径已停用,W3 拒绝、W4 只供测试;删号与落点记录的关系等删号设计。
  - 无结论:
    - 跨 zone 共享缓存 `{MsgType}:{pid}` 读写竞态;
    - 能力标记管不住滚动发布与回退后 90s 两个窗口;
    - 无 GateId 的 EnterScene 不过合服围栏,前提是生产调用方都带 GateId;
    - dev_tools 漏给 `-MergeDbCapabilityZones` 时退出码是 1,与 missing 同码;
    - 落点告警没进 `deploy/k8s`;
    - 真集群演练没做。

### 2.2 D1 各批

| 批次 | 内容 | 在哪 | 运行证据 |
|---|---|---|---|
| 第 0 批(10-12) | dev Redis 改 noeviction、回档 FLUSHDB 防护、BatchGet 不吞错 | HEAD | 默认单测随 data_service 全量跑过。验证命令里的 `k8s_zone_rollback_flush_guard.tests.ps1` 已被 `8b9592de3` 删掉,改为 discard 测试 |
| 1a、1a 评审修复、G1a、行片 64、P5 | 路由表底座、W1 db_first、route_tool 导入与审计、行片与探针 | HEAD | 10-15 编译过;10-17 / 10-19 默认单测跑过;10-16 Claude(不是 Codex,也没按清单)跑过 route store 真 MySQL(365 PASS,1 个既有 FAIL)和 merge_zone 真库集成 273/0/0 |
| 1b-1(10-18) | route_tool 并进主程序;freeze / reconcile-from-cache / unfreeze | HEAD | 只有 10-19 默认单测。第 0 步红跑与真库都没跑 |
| 1b-2(10-19) | 对账 leader、就绪标记、rewarm | 大部分在 HEAD(`53361d055`)。`reconciler.go` 的增量与 `cache_test.go` / `cache_integration_test.go` 只在工作区 | **无** |
| 1b-3 | 部署载体:k8s_deploy 新参数、`k8s_route_tool.ps1`、`data-service-route-alerts.yaml` | HEAD | `k8s_deploy_contract` 10-19 先红 2 例,Codex 修一行后 241/241。`k8s_route_tool.tests.ps1`、promtool、清库脚本 plan 都没跑 |
| 第 2 批 | merge_zone 的 SQL 写入层与写入点、relocate / R8、`-take-over-fence` | 大部分在工作区:19 个 M、10 个 ?? | **无** |
| 第 3 批 | 读方门禁:data_service `route_reads`、go/db `route_ready_gate`、guild `merge_fence`、`data_consistency_check` | HEAD(`8b9592de3`) | 只有 data_service `route_reads` 的默认单测 |

**注意**:Codex 10-19 为修 data_service 全量先红的问题,改过两份 D1 测试(PROGRESS 约第 11943–12014 行)。一份是 `route_publish_test.go`,拆夹具去掉熔断器引起的随机红;另一份是 `route_reads_test.go`,修了 vet 报错。**评审时不要把这两处改回去**。

### 2.3 开关缺省(静态核对,未运行)

| 服务 | 开关与缺省值 |
|---|---|
| data_service | `RouteStore.Mode=redis`(为空时归一成 redis,拼错拒启)、`ReadRequiresReady=false`、`Reconcile.ArmReadyMarker=false`。对账 leader 只在 db_first 下构造 |
| go/db | `Placement.Required=false`、`RequireRouteReady=false`。K8s ConfigMap 只写连接池与 `RequireRouteReady`;`Required`、`AllowStoreFamilies` 不写,取缺省 |
| guild | `MergeMarkerRedis.RequireRouteReady=false` |
| login | `Placement.PinOnCreate=false`、`NewPlayerStorageId=0` |
| k8s_deploy | 新参数缺省为 redis / false。组合门禁拒绝「Arm 或读方开关已开,但 Mode=redis」和「读方开关已开,但 Arm=false」 |

除测试夹具外,没有任何配置写成 `db_first` 或 `true`。

## 3. 对 handoff-d1-remaining 的更正与补漏

更正:

1. **文首「全部未编译、未测试」不准确**。data_service 默认单测 10-19 跑绿过一轮,覆盖 1a / G1a / P5 / 1b-1 和第 3 批的 data_service 读方;其余确实没有证据,见 §2.2。
2. **§2.2「四张路由表」实为五张**:`player_route`、`zone_merge_fence`、`zone_merged_into`、`route_meta`、`route_reconcile`。§2.2 正文写的就是五个 message;PROGRESS 13296 条目和提案 §13.5 第 1 条写成「四张」,需要改。
   - 只有 `player_route` 的 DDL 预留了 `pb:1..10`(`go/data_service/internal/store/schema.go` 约 200–209 行)。另外四张没有 pb 注释,字段号要新定。
3. **§2.3 第 4 项点名的入口不对**:`k8s_image.ps1` 不部署 Go 服务、不传 `-GoSvcRegistry`,`Assert-RouteSwitchPreflight` 在粘滞核对之前就返回了,所以不会被拒。
   - 会被拒的有两处:`dev_tools.ps1` 的 `k8s-infra-up` / `k8s-zone-up` / `k8s-all-up`(`GoSvcRegistry` 缺省非空,且不转发任何 D1 开关);`k8s_zone_rollback.ps1` 的 Step 6(走 dev_tools `k8s-zone-up`)。
   - 另有一个隐患:回档第 0 步用 `-DryRun` 做静态预检,DryRun 会跳过 D1 粘滞核对。所以在 db_first 下,要到 Step 1 删掉 namespace 之后、Step 6 才会被拒。第 0 步对 GateRouterMode 等有漂移核对,对 D1 开关没有。
   - `k8s_zone_rollback.ps1` 正由 R9 重写,改它之前先协调。目前没有环境是 db_first,上线前不阻塞。
4. **§2.3 第 5 项不用做**:`deploy-config-tests.yml` 用 `find -maxdepth 1` 自动发现 `tools/scripts/tests/*.ps1`,新测试已在 CI 范围内。
   - 真正的缺口在触发路径:`k8s_route_tool.tests.ps1` 会读 `go/data_service/data_service.go` 与 `internal/routetoolcli/cli.go`,这两个文件不在 push / pull_request 的 `paths` 里。
   - 同类缺口:D1 告警用例只拿手写名单比对指标名,不读 `go/data_service/internal/metrics/metrics.go`;告警里的 go/db、guild 指标完全没比对。核查时名字是一致的。

补漏(D1 收尾清单里没有,但要做):

5. **发布 + WAITAOF 改用独立的 go-redis 客户端**(提案 §12.1「对 §12 的更正」第 2 条,原文写「属于 D1 后续批次」)。
   - 现状:`route_publish.go:299` 仍用 `r.mappingRedis.PipelinedCtx`,与 `GetPlayerHomeZone` / `BatchGet` 共用连接池和熔断器。
   - 风险:db_first 下每次 WAITAOF 占一条连接约 0.5s,建角高峰会饿住登录和进场查询。
   - 相关:Q3 要看 `TestPublishRoute_RealRedisSameConnection` 的结果,这个用例从没跑过。
   - 要做:写进 §2.1 评审重点,并在提案 §13.5 登记。
6. **过时文案**:第 2 批已经落码,下面几处说法与 runbook §15 矛盾,会误导运维:
   - `go/data_service/data_service.go:394` 的 `routeStoreDBFirstWarning`:db_first 下每次启动都打印「第 2 批落地前,禁止 merge_zone 任何写模式」;
   - `go/data_service/etc/data_service.yaml:170` 的切换前提 1);
   - `config.go` 里 `RouteStoreModeDBFirst` 的注释;
   - `routing/route_publish.go`、`store/route_store.go` 的文件头。

   另外,`data_service.yaml` 说 K8s 不镜像 `FenceShards`,但 `k8s_deploy.ps1` 约第 8858 行已经渲染它。
7. **合服清单 `manifest.go` 同样不记 route source**,与 relocate 清单是同一类缺口。风险受 `openRouteBackend` 的成对核对限制,主要出在跨 P2 / P4 边界续跑时。
8. **runbook §3 仍教「GET / DEL `merge:in_progress` 键再原命令重跑」**。db 源下这样做是错的,库里的围栏才是真源(`tools/merge_zone/fence.go` 约 587–590 行)。§15.4 也没写 `-take-over-fence` 的用法。
9. **`relocate.go` 的 HomeChanged 文案**「player:zone changed…」也用 Redis 键名,与 Fenced 文案同批改。
10. **提案 §13.5 没有零碎项的条目**:handoff-d1-remaining §2.3.6 要求「§13.5 对应条目标注已做」,但 §13.5 只有 proto 守卫和残余风险两条。先把零碎项补进去,做完才有地方标。
11. **验证步骤缺 `python tools/scripts/gen_docs_index.py --check`**。另外,`docs/README.md` 里 handoff-d1-remaining 那一行的描述与它的 H1 不一致,位置也不按文件名排序,`--check` 会报。本次已顺手改正。

## 4. 剩余工作(按顺序)

### 第 1 步:D1 代码收尾(Claude)

按 handoff-d1-remaining §2 的顺序做,同时吸收本文 §3:

1. **§2.1 最终语义评审**:两个只读评审代理并行,评审重点加上 §3 第 5、6 条;按 must / should 修复。评审脚本 `d1_review.js` 只在另一个会话的临时目录里,随时可能被清掉,评审范围以 handoff-d1-remaining §2.1 的文字为准。
2. **§2.2 五张路由表的 proto 列漂移守卫**:要改 proto,由 Codex 做 no-unity 重生成,并重编 C++ proto 库。
3. **§2.3 零碎项**:按 §3 第 3、4、7~10 条修正后再做。

### 第 2 步:按当前 HEAD 重写一份 Codex 验证单(Claude,只写文档)

10-12 复验清单和 13296 条目的第 0 步都不能原样交出:Codex 的规矩是「红了就停」,会直接卡在第 0 步。原因:

- 第 0 步要求 HEAD 等于 `73b852f32` 加纯文档提交,现在已领先 112 个提交,非文档改动 3463 个文件;
- 要求 go ≥ 1.26.5,但各 go.mod 已统一为 1.27.2;
- 第 1 步要求「go.mod / go.sum 无改动」,现在有 4 个提交改过;
- 「落点代码自 `4493b3c9f` 未改」已不成立,所以拿它当基线做归因也不成立;
- 用例计数全部过时:
  - `go/shared/placement` 现有 27 个顶层用例,附录写的是 16;
  - `RouteRowShards` 现在是 64,附录写的是 16;
  - `dev_tools_merge_zone_contract` 19、`k8s_deploy_contract` 241,与清单不符。

新验证单放 `docs/handoff/`,从本文链接过去。要求:

- **开跑前记录**:起止 HEAD、`git status`、输入文件的 SHA 清单。工作区随时会被自动快照提交,已有 3 轮验证跑到一半 HEAD 被推进(PROGRESS 约第 12010、12135、12371 行)。
- **原生那一轮不带 `-race`**。本机没有 gcc,CGO 也关着,带 `-race` 会 exit 2,后面的全跳过(10-18 发生过)。`-race` 单列一步,在 Linux 容器里跑:`golang:1.27.2-alpine3.24`,加装 gcc / musl-dev;拉镜像前先征得同意。
- **真 Redis 7.2 和 8.x 各跑一轮**:K8s 钉 7.2,本地 compose 与 Java 是 8.10.x。用一次性容器,**不要借 `codex-*` 容器**,它会中途被撤。Windows 上 go test 报 `Access is denied` 时,改用 `go test -c` 编出 exe 再跑。
- **真库用例设 `*_IT_REQUIRE=1`**,逐条报 PASS / FAIL / SKIP;MySQL 通过不能代替 TiDB;口令不进回报。
- 第 2 批之后,merge_zone 集成测试要先用 `data_service -migrate` 建一次性路由库。
- **归因基线**:改用各 PROGRESS 条目写明的基线提交。
- **合并的素材**:13296 条目的第 1~9 步,加上 10-12 清单里仍然有效的部分:
  - 第 8b 步(编解码向量与重试键名比对);
  - 第 11 步(db.exe 版本戳);
  - 第 12 步(INFRA 退出码、P4 真 Redis 探针、本机双 db 实例);
  - §5 列的覆盖缺口。
- **加 `gen_docs_index.py --check`**。
- **结果回写清单**:沿用 10-12 清单 §4,再加提案 §11.2 / §13。

### 第 3 步:Codex 执行、归因、修复、写回

- 红了先分清是不是本线的文件,判断方法见 §5;本线的问题就修,别人的如实报告。
- 回报到达之前,任何文档都不得写「已验证」。

### 第 4 步:09-28 遗留、D1 没接的小项(Claude)

1. **落点告警进 `deploy/k8s`**:`stale_topic`、`frozen_deferred`、`recheck_moved`、`home_zone_merging`、`missing_required`,以及 `db_placement_store_open_total`。指标在 `go/db/internal/metrics/metrics.go` 约 815–819 行和 `go/scene_manager/internal/logic/home_zone.go:63`。落点设计 §13 要求打开新号种子前就位。
2. **`deploy/k8s/README.md`**:告警清单补上 `data-service-route-alerts.yaml`。第 2 批入库后,改掉约第 2447 行「dev_tools 的 k8s-route-tool 转发在下一批统一加」;这句话对 HEAD 仍然成立。
3. **`docs/ops/release-checklist.md` 加「写过落点记录后禁止回退 go/db」**。现在这条只写在设计 §13、runbook §5.1 / §14 和 `login.yaml` 注释里。
4. **文档文首过时**:
   - 落点设计 v2.4 写「K8s ConfigMap 不写 Placement 段」,10-14 起已经写了;
   - §4.4 / §11.4 的 D1 进度还停在 1a;
   - runbook 文首没有反映 §15;
   - runbook §9.2「目前没有重放工具」要加更正注:D1 leader 的全量预热只在 db_first 且 Arm=true 时生效。
5. **提案 §11.2 进度表**:第 1b 行在工作区多出一格(4 列,表头只有 3 列)。
6. **dev_tools 漏给 `-MergeDbCapabilityZones` 时退出码为 1,与 missing 同码**(09-28 遗留)。改成 2,或保持现状只靠 runbook 区分,二选一。

### 第 5 步:整理 §7 拍板清单,一次问负责人

## 5. 工作区与文件归属(11:03 快照)

### 5.1 只属本线、未提交

- `go/data_service/internal/routereconcile/reconciler.go`(M);`cache_test.go`、`cache_integration_test.go`(??)
- `go/data_service/cmd/debugutil/debugutil.go`、`internal/constants/error_codes.go`、`internal/svc/servicecontext_missing_stores_test.go`
- `tools/merge_zone` 19 个 M:
  - `fence.go`、`merged_into.go`、`merge_run.go`、`placement_ops.go`、`pin_placement.go`
  - `unmerge.go`、`backfill_home_zone.go`、`player_blob_migrate.go`、`guild_step.go`、`trade_step.go`
  - `main.go`、`route_store.go`、`route_store_test.go`、`integration_test.go`
  - `relocate_run.go`、`relocate_manifest.go`、`storage_audit.go`、`audit_checks.go`、`audit_resources.go`
- `tools/merge_zone` 10 个 ??:
  - `audit_route_db_test.go`、`backfill_home_zone_db_test.go`、`fence_db_test.go`、`merge_run_db_test.go`、`merged_into_db_test.go`
  - `pin_placement_db_test.go`、`placement_ops_db_test.go`、`relocate_db_test.go`、`relocate_integration_db_test.go`、`unmerge_db_test.go`
- `tools/scripts/tests/dev_tools_merge_zone_contract.tests.ps1`
- `docs/handoff/handoff-d1-remaining-20261019.md`(??)、本文(??)

### 5.2 共享文件

- **工作区 diff 全是本线**:`tools/scripts/dev_tools.ps1`、`go/data_service/data_service.go`、`go/data_service/internal/svc/servicecontext.go`。
- **只有部分段落是本线**:
  - `docs/ops/merge-zone-runbook.md`:§9.3 第 3 条 WaitAOFNumReplicas 一段属 redis-ha,其余是本线;
  - 提案:H1、§11.2 第 1b / 2 / 3 批行、§13 是本线,§12.1 / §12.2 的 WAITAOF 段属 redis-ha;
  - `docs/README.md`;
  - `docs/PROGRESS.md`:本线是「D1 剩余批次落码完成」「D1 剩余收尾交接」和本文对应的那条。
- **工作区 diff 不属本线**(本线的段落已在 HEAD):`tools/scripts/k8s_deploy.ps1`、`tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`go/data_service/etc/data_service.yaml`、`go/db/etc/db.yaml`。这四个文件的工作区改动全是 redis-ha 的。redis-ha 还改写了本线原有的「D1 1b-3 mapping Redis 拓扑」用例。
- **Java 仓 `PARITY.md`**(工作区,未提交):三处改动分属三条线:
  - D1 行改为「Java 待做(部分不适用)」,属本线;
  - 删掉 4 行纯「不适用」,属开服线;
  - 新增 1 行,属 G4a。

其余未提交改动分属十多条别的线(混版本持久字段保护、location 释放 r2、EnterScene 陈旧位置、r2-db、redis-ha、客户端请求回调、可配置模板、R9、第二轮审计、字段 Lua 等),各有自己的交接,**不要碰**。`*.vcxproj.metaproj` 是构建副产物,`third_party` 的子模块指针变动不算任何一条线。

### 5.3 会冲突的线与协作

- **D1 会话已停**。新账号接手本线后,就是 D1 文件的实际负责人;动手前把这一点告诉负责人(§7 第 1 条)。
- **R9**([zone-rollback-r9-handoff-20261019.md](zone-rollback-r9-handoff-20261019.md)):正在重写 `tools/scripts/k8s_zone_rollback.ps1`,不要碰。它要动 go/db `placement_route.go` 的 lookup 附近时,约定先找 D1 会话;现在应该来找本线的接手会话。
- **审计线 r2-mz#1**([data-overwrite-audit-r2-20261019/README.md](data-overwrite-audit-r2-20261019/README.md) §8):
  - 要改 `tools/merge_zone/merge_run.go:493`、`unmerge.go:289`、`player_rows.go`,必须在首次 copy 合服前修;
  - 冲突一:`merge_run.go`、`unmerge.go` 正好有 D1 未提交的改动;
  - 冲突二:开服线交接 §5 写了「不要再改 tools/merge_zone」;
  - 两份交接互相都没提对方,需要负责人定谁改、谁先改。
  - **2026-10-24 已解决**:D1 接手会话同意由「数据覆盖审计交接文档」会话改,r2-mz#1 已落码并经评审修正(未编译,待 Codex)。merge_zone 里它改的位置只有:`player_rows.go` 702-819(删键清单拆出 KeepBarrier 版本,731-733 注释)、`merge_run.go` runMerge copy 分支约 514-533(改调 `invalidatePlayerCachesKeepBarrier`)、`unmerge.go` 5'b 约 305-327(只加注释)、`audit_resources.go` 第 384-388 行(`verifySaveBarriers` 登记与行尾注释)、新文件 `save_barrier_verify.go`、`player_rows_barrier_test.go`、`save_barrier_integration_test.go`;runbook 只动 §5 步骤 1、§8 Step 5 断言表、§10.1。`requireFence`、`routeSource`、`routeReads`、`routeDB` 等路由 / 围栏行都是 D1 的,没有碰。route-source=db 下屏障删 / 留走同一条路径。评审提的「每个 auditor 30s 预算在 `runAuditMode`(约 409 行),大清单可能稳定报 INFRA」属 D1 维护的段落,留给 D1 决定。详见 PROGRESS「2026-10-24 Claude:第二轮审计 r2-mz#1」。
- **自动快照**:工作区随时会被署名 luyuan-cpp 的「补充…」提交整区收走。本线的未提交文件可能挂在无关的提交说明下入库,所以核对现状一律以工作区为准。

## 6. 上线前仍缺的大项(本线前置,只登记,不在本交接执行)

- **D8 应用级回档**:没有设计,也没有代码。
  - `RollbackFence` 在生产仍是 nil,回档写一律 fail-closed(`go/data_service/internal/svc/servicecontext.go` 约 101–110 行、`internal/logic/rollback_logic.go` 约 60–93 行)。
  - R9 是 PITR 灾难回档的清理工具,不是 D8。
  - D8 是 Phase 2 的前置。
- **压测**:09-29 之后没有任何压测,最新一轮是 R19(06-18)。go/db 每条任务多出的 MGET、排序锁、applied_epoch 都没压测过。`stress_summarize.ps1` 不汇总 mapping Redis 与归属查询的耗时,L-14 压测前要先补上。
- **真集群演练**:pin / copy 合服、正反向搬库、kind 上删 Pod,一次都没做过。
- **TiDB Phase 1**:[tidb-phase1-and-launch-readiness.md](../design/tidb-phase1-and-launch-readiness.md) 还是 v0.2,§9.3 约 20 项待拍板。K8s 上仍是 `mysql:8.0`;`docs/ops/tidb-br-pitr-runbook.md` 不存在。
- **L-18 复核**:只做了登记,即提案 §12.2 的六步和 release-checklist A.1(未勾选)。做法见开服线交接 §3 第 3 项。
- **打开 PinOnCreate 之前**:要先重做连接规划(`k8s_deploy.ps1` 约第 1853 行),并建全局库。

## 7. 需要负责人拍板(整理好一次问)

1. **D1 文件的归属**:`tools/merge_zone`、data_service 路由相关、`go/shared/placement` 由谁接手。r2-mz#1 与 D1 第 2 批都要改 `merge_run.go` / `unmerge.go`,谁先改。
2. **Q8**:AGENTS §6.2 的清库要求是否加上「全局库路由表一并清」。提案推荐加,AGENTS 目前没有这条。
3. **提交时机**:本线工作区里的未提交代码;Java 仓 PARITY 的 D1 行。
4. **维护窗口**:`redis.yaml` 改了 Pod 模板(`--enable-debug-command no`),下一次 infra-up 要带 `-ConfirmRedisCoreRestart`。另外,mapping Redis 如果启用 ACL,账号要放行 INFO,否则读方探针会拒启。
5. **独立客户端的时机**:发布 + WAITAOF 改用独立 go-redis 客户端(§3 第 5 条),现在就做,还是等 `TestPublishRoute_RealRedisSameConnection` 的结果再定。
6. **Java PARITY 规则冲突**:表头写「只追加、只更新状态,不删旧行」,而「只记待做」这条规则只存在于记忆里。二选一:改表头,或把规则写进两仓文档。
7. **大项立项**:D8 是否立项;TiDB Phase 1 §9.3 的各项。

## 8. 硬约束与环境

- **AGENTS §10.1**:Claude 只改代码、测试和文档;编译、测试、gofmt、proto 生成、压测都交 Codex,并写清命令与通过标准。不派「能不能编过」的静态评审。
- **不 `git add -A`,不提交,不 push**。工作区多会话并行,改文件前先 `git diff` 分清归属,一律用 Edit 精确替换。
- **PROGRESS 只追加**。新文档在 `docs/README.md` 登记,描述用文档 H1,位置按文件名排序。
- **Java 仓**(`D:\luyuan\xuanming-server-mmo`)**只登记待做项**。
- **额度**:旧账号周额度已用到 95%~97%,据 PROGRESS 记录,10-24 22:00 UTC 才重置。新账号先查自己的用量;同一账号上的多个会话共用额度。工作流分批跑,每个实现代理的提示里都带续做说明。只读代理的提示里写明:按提交读文件用 `git show` 直接输出,不重定向成临时文件。
- **本机环境**:Go 1.27.2,`GOTOOLCHAIN=local`;没有 gcc;C++ MSBuild 严格串行 `/m:1`。

## 9. 给新账号的开场指令(可直接复制)

```text
接手「玩家存储落点 + D1 映射持久化」这条线。仓库 D:\luyuan\xuanming-server-mmo-cpp。
1. 先按 AGENTS.md 启动门禁读完规定材料,再读 docs/handoff/placement-line-handoff-20261019.md(总览、更正、剩余工作),
   然后读 docs/handoff/handoff-d1-remaining-20261019.md(D1 收尾做法;与总览 §3 冲突时以总览为准)。
2. 按总览 §4 的顺序做:
   第 1 步 D1 最终语义评审(两个只读评审代理并行,重点含总览 §3 第 5、6 条)并修复 must / should,再做五张路由表的 proto 守卫和零碎项;
   第 2 步按当前 HEAD 重写一份 Codex 验证单(不要原样交 10-12 复验清单);
   第 4 步补落点告警、release-checklist、文首状态等小项;
   第 5 步把总览 §7 整理成拍板清单问我。
3. 只写代码、测试与文档;协议生成、编译、测试、冒烟交给 Codex,写清命令与通过标准。
   不派「能不能编过」的评审;工作区有多会话并行,不 git add -A、不提交;不碰 tools/scripts/k8s_zone_rollback.ps1。
4. 改 tools/merge_zone/merge_run.go、unmerge.go 之前,先问我 r2-mz#1 由谁来改。
5. 每完成一项在 docs/PROGRESS.md 末尾追加条目(只追加),写明 Java 状态;Java 仓只登记待做项。
6. 遇到新的拍板项、改动超过 30 个文件、要下载镜像或装工具时,先停下来问我。
```

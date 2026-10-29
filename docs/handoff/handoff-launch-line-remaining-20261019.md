# 交接:开服线剩余工作(2026-10-19)

> **给新账号**:先按 AGENTS.md「会话启动门禁」读完规定材料,再读本文,然后从 §3 第 1 项开始。开场指令见 §7,可直接复制。
>
> **上游**:[handoff-launch-readiness-and-d1-20261013.md](handoff-launch-readiness-and-d1-20261013.md)(10-13 起的开服线交接,文首按日期追加了 10-15 ~ 10-18 的进度段)。本文接它在 10-18 收尾之后的状态,上游正文里的「剩余工作」以本文为准。
>
> **(2026-10-20 进度,接手会话追加)**
> - 第 1 项:五项落码只读核对完毕,在当前树里都完整生效。原有的 Codex 命令多处过时,已按当前树重写成 [verify_launch_line_20261020.md](launch-readiness-20261013/verify_launch_line_20261020.md);**仍未执行**,等 Codex 跑。
> - 第 3 项 L-18:静态部分由「账号登录逻辑/物理服瓶颈」会话做,写回提案 §12.3;压测前置要先加指标、再改脚本,是一项单独的代码任务,待负责人指派。
> - 第 4 项:拍板清单已汇总成 [decisions_launch_line_20261020.md](launch-readiness-20261013/decisions_launch_line_20261020.md),等负责人逐项定。
> - 第 5、6 项:现状见各项下的 2026-10-20 注。

## 0. 一句话

- 开服线的**代码已全部落码**,已随用户的整区快照与 10-19 主干合并入库,**全部未编译、未测试**。
- 剩下的是四类事:
  - 把这批代码交 Codex 验证,红了归因、修复;
  - G4a 拆 Redis 设计稿修订(另一个会话正在做;2026-10-19 更正:已完成,只剩拍板清单汇总,见 §3 第 2 项);
  - 负责人拍板与运维前置;
  - 已登记的后续小项。

## 1. 已落码的内容(全部未编译、未测试)

每项在 [PROGRESS.md](../PROGRESS.md) 都有条目,按标题搜索即可。每个条目都写了改动清单、交 Codex 的命令和通过标准。规格(含核实意见)在 [launch-readiness-20261013/](launch-readiness-20261013/)。

| 项 | 内容 | PROGRESS 条目标题(关键词) | 规格 |
|---|---|---|---|
| B0-2 | 删除 C++ 存盘 Lua 对全服单键 `dirty_keys_set` 的写入;存量清理口径在 release-checklist §6.1 | 「删除 C++ 存盘 Lua 的全服键 dirty_keys_set 写入」 | spec_B0-2_dirty_keys_set.md |
| 注释 | 「共享库是 allkeys-lfu」过时注释,12 处改了 9 处 | 「共享 Redis 淘汰策略过时注释更正」 | spec_G4a_revision_scout.md 第二部分 |
| B0-5 | scene_manager 负载刷新改为领导者按 zone 批量刷新,用登记集合 `scene_manager:zones` 替代全库 SCAN | 「scene_manager 负载刷新:领导者按 zone 批量刷新」 | spec_B0-5_scene_manager_zone_scan.md |
| D1 1b-1 | route_tool 并进 data_service 主程序(`-route-tool`);新增 freeze / reconcile-from-cache / unfreeze;就绪标记的编解码;修正发布在熔断时的分类 | 「D1 映射持久化第 1b-1 批落码」 | spec_D1_batch1b_scout.md(以提案 §11.2 为准) |
| B0-4 | gate / scene / battle 启动时把 nofile 软限抬到硬限,不够就拒启;运维豁免开关为 `NODE_OPEN_FILE_LIMIT_MODE=warn` | 「B0-4:gate / scene / battle 启动时抬 RLIMIT_NOFILE」 | spec_B0-4_nofile.md |
| B0-3 第 1 批 | proto `DBTask.reply_instance = 10`、`go/shared/taskresult`;login 同时订旧频道与实例频道,并填回复目标 | 「B0-3 task:result 读回执通知分频道 第 1 批」 | spec_B0-3_task_result_channel.md(文末两节交付说明) |
| B0-3 第 2 批 | go/db 按 reply_instance 定向发布;指标 `db_task_result_notify_total`;独立告警文件 | 「B0-3 … 第 2 批:go/db 按 reply_instance 定向发布」 | 同上 |
| B0-1 | player_locator 会话对账改为按 gate 实例建索引(在会话 Lua 里原子维护);对账改为全服单 leader;去掉全键 SCAN | 「B0-1:player_locator 会话对账按 gate 实例建索引」 | spec_B0-1_reconcile_by_gate.md;设计 [player-locator-gate-session-index.md](../design/player-locator-gate-session-index.md) |

另有一条收尾汇总:「2026-10-18 开服线收尾:代码全部落完,剩 G4a 修订稿」。**交 Codex 的总顺序**就在那条里。

## 2. 负责人已拍板(不要再翻案)

| 日期 | 事项 | 结论 |
|---|---|---|
| 10-13 | 容量目标 | 先定为 S3(1000 万在线),10-15 改为**日活 1 亿、峰值同时在线 2000 万**(容量模型 §7) |
| 10-17 | 核心 Redis 路线 | **单元化 + 单元内 Redis Cluster 按 {player_id} 分片**,两者叠加;D12 / CZ-2 改写为「逻辑单一、物理按 {player_id} 分片」 |
| 10-17 | B0-3 | 授权改动超过 30 个文件;先用 PUBLISH,SPUBLISH 推迟到上 Cluster 那批;分两批落 |
| 10-17 | B0-1 | gate 索引在会话 Lua 里原子维护 |
| 10-17 | 其余 | B0-4 加运维豁免开关、缺省拒启;D1 1b-2 先用单个就绪标记、回档检测容差取 0;存量 `dirty_keys_set` 暂不清理 |
| 10-18 | 上线状态 | **项目尚未上线**,没有存量部署。新旧混跑、降级、灰度类的残余风险,上线前不算阻塞,首次上线整批起步(见存盘围栏设计文末「上线状态」) |

## 3. 剩余工作(按顺序)

### 第 1 项:交 Codex 验证本线代码,收结果并归因

- 总顺序在 PROGRESS「2026-10-18 开服线收尾」条目:
  1. proto 生成核对;
  2. Go 各模块;
  3. C++,MSBuild 串行 `/m:1`;
  4. PowerShell 契约测试;
  5. `gen_docs_index.py --check`。
- 每项的细节命令、修复前红的取证方法(基线提交写在各条目里,多为 `360d56ae5` 或 `a105c3efd`)、通过标准,都在各自的 PROGRESS 条目和规格文末。
- 红了先归因:工作区和主干里有大量别的会话的改动,先判断失败是不是落在本线文件上。本线的就修,修完给出新的验证命令;别人的就如实报告,不要顺手改。
- 本线没有编译、也没有运行证据。在 Codex 跑绿之前,任何文档都不得写「已验证」。
- **(2026-10-20 更正,原文不删)**
  - 命令以 [verify_launch_line_20261020.md](launch-readiness-20261013/verify_launch_line_20261020.md) 为准,它取代各规格文末与 PROGRESS 条目里的命令。原命令的问题包括:
    - 红灯基线写成了 HEAD,会取到假绿;
    - 用例正则漏项,或引用了已删除的用例;
    - `etcd-up`、`open_file_limit_test` 工程这些命令或工程并不存在;
    - 集成测试的 SKIP 会冒充通过;
    - 用工作区 `git diff` 核对改动范围,必然误报。
  - 「没有运行证据」也不准确:B0-5、B0-1 已被别的验收顺带跑过一遍;B0-4 在 Windows 上 21/21,而且哈希与当前一致。但这些都是针对当时的输入,不能代替本单,详见验证单 §2。
  - D1 1b-1 的验证移交落点线重写,见 placement-line-handoff §4 第 2 步。

### 第 2 项:G4a 拆 Redis 设计稿修订

- 10-19 已经用任务卡片单独开了一个会话在做,标题是「修订拆 Redis 设计稿 G4a(单元化+单元内分片)」。
- **(2026-10-19 状态)已完成,不用续做**:修订已落到 [shared-redis-split-by-purpose.md](../design/shared-redis-split-by-purpose.md),新增 §16~§26,三条 must 都已落实;围栏硬依赖登记在 §18.7。完成条目是 PROGRESS「2026-10-18 G4a 拆 Redis 设计稿修订」。接手者只需把该稿 §25.2 的拍板清单汇总给负责人,其中标「2026-10-18 新增」的 12 处(第 2、3、15、16、17、20、21、22 条,以及第 1、6、7、9 条下各一个子项)是这次新出现的,并入下面第 4 项第 6 条一起问;按 §25.2 原文逐条收,不要只照 PROGRESS 条目里的枚举(那里漏列了第 7、9 条子项,已在 PROGRESS 末尾另起更正)。
  - 规格里还有一批其他文档的指向注没做,该稿 §24 末段列了清单,写的是「归各自的维护线」,但目前没有哪条线认领:`cross-zone-scene-travel.md` CZ-2、`cross-zone-matchmaking.md` D12、`tidb-phase1-and-launch-readiness.md` L-06、`no-single-node-horizontal-scaling-20261001.md` §5、`spec_G4a.md` 文首、`db-write-fencing-mysql.md` §5.1 / §6.4 / §9.1、`db-task-pipeline-bottlenecks.md` §6.5(按该稿 §21.6 的指引)。都是只追加带日期注的纯文档活,谁接都行;动手前先 `git diff` 看对方是否正在改。容量模型的三处(§4.2、A6、§6 第 6 条)已补齐。
    - **(2026-10-19 补)上面这批指向注已在 G4a 修订会话里全部补上**,不用再接;见 PROGRESS「2026-10-19 G4a 收尾」条目。G4a 这一项现在只剩 §25.2 拍板清单的汇总。
- 接手前先看 PROGRESS 有没有它的完成条目:
  - 有:只需把它产出的拍板清单汇总给负责人;
  - 没有,而且那个会话已经停了:按 `docs/handoff/launch-readiness-20261013/spec_G4a_revision_scout.md` 第一部分及核实意见续做。
- 硬要求(核实意见的三条 must):
  - 新写的文字里不要再出现「共享库是 allkeys-lfu」;
  - chat 的聊天历史带 TTL(缺省 7 天、每频道 200 条);
  - 登记 db-write-fencing 的「排序锁 / 子表缓存 / 资产证据三键同实例或同 slot」依赖。
- 纯文档,不改代码。

### 第 3 项:开服前复核「登录取逻辑服 / 物理服是否瓶颈」(L-18)

- 负责人 10-19 要求:上线前重新查一遍账号登录时取 home_zone(逻辑服)和 storage_id(物理服)这两步是不是瓶颈、能不能水平扩展。10-18 那次的结论全是推算,不能直接沿用。
- 按 [player-route-mapping-durability-proposal.md](../design/player-route-mapping-durability-proposal.md) §12.2 的六步做,登记为 `tidb-phase1-and-launch-readiness.md` §7 L-18 和 `docs/ops/release-checklist.md` A.1。
- 结论写回提案 §12.1 和 PROGRESS;压测与汇总交 Codex,按 AGENTS §6。
- **(2026-10-20 分工)**
  - 静态部分(§12.2 第 1~4 步与 Java 核对)由「账号登录逻辑/物理服瓶颈」会话做:结论写成提案 §12.3,§12.1 只加一行指向,L-18 标「静态部分已复核、压测未做」,不标完成。
  - 第 5 步的压测前置不只是改 `stress_summarize.ps1`。该会话核对后指出,还缺这几项指标:
    - scene_manager 的 `resolveHomeZone` 不在任何 `enter_scene_stage` 计时段里;
    - data_service 没有归属读的耗时直方图;
    - login 的 `RefreshRoleZones` 回落连计数都没有。
  - 所以压测前置要先加指标、再改 stress_snap / stress_summarize / test_stress_summarize,是一项单独的代码任务,待负责人指派。调研输入是本会话草稿里的 `l18_stress-prereq.json`,该会话会把路径写进任务说明;清单里记为 X-3。

### 第 4 项:需要负责人拍板或运维执行的事项(整理好后问负责人)

1. **TiDB Phase 1**:[tidb-phase1-and-launch-readiness.md](../design/tidb-phase1-and-launch-readiness.md) §9.3 列了约 20 项待拍板(部署方式、版本、迁移方式、发号打散、Codex 可操作的环境、Operator 安装授权等),不拍板就开不了工。另外,容量模型 §4.5「OLTP 与审计至少分两套集群、单元化后每单元一套」要回写进 Phase 1 文档。
2. **存量 `dirty_keys_set` 清理**:在哪个环境执行、什么时候执行;步骤在 `docs/ops/release-checklist.md` §6.1(门禁是版本盘点,用 UNLINK,不用 DEL)。
3. **B0-4 上线前置**:在各集群、各节点池上读一次容器的 nofile 硬限,记进 PROGRESS;读法在 `docs/ops/gate-kernel-tuning-runbook.md` §3.1。单副本的 battle hostPort 一旦拒启,战斗服会整体停服。
4. **B0-1 上线前置**:k8s_deploy.ps1 的 player-locator ConfigMap 已补上 Prometheus 段(9190),要和 manifest 的注解、端口同批发布;告警 `deploy/k8s/player-locator-alerts.yaml` 手工 apply。
5. **B0-3 上线顺序**:login 已经是第 1 批的 zone,改为按 zone 金丝雀发 go/db,不要一次升全部 zone;告警等两侧全量之后再 apply。
6. **G4a 修订稿产出的拍板清单**:mapping 三族 hash tag 的时机与键名、控制面 zone 组大小、redis-cache 形态、账号目录落库位置、核心内存上界与冷玩家卸载阈值(S1 前必须定)、三键同实例与值封装二选一等。

- **(2026-10-20)以上 6 条与第 6 项里要拍板的两条已汇总**,见 [decisions_launch_line_20261020.md](launch-readiness-20261013/decisions_launch_line_20261020.md):
  - 共 60 项,去重后按 TiDB / 拆 Redis / 运维前置 / 登录与其他分组,文首列出建议先定的 8 项;
  - 已定的 3 项单列,不再问。
- 第 1 条后半「容量模型 §4.5 回写 Phase 1 文档」已做:tidb-phase1 §1.2、§2.2 表后、§2.7 末、§9.3 第 12 条各加了 2026-10-20 注。两份文档在「一套集群还是多套」上的冲突没有替负责人定,列为清单 T-12。

### 第 5 项:已登记的后续小项(不阻塞,按需另开任务)

- **过时注释还剩 3 处**:`go/match/etc/match_service.yaml`、`tools/scripts/k8s_deploy.ps1` 的 friend here-string、`deploy/k8s/README.md` 的 friend 段。建议文案在「共享 Redis 淘汰策略过时注释更正」条目里。k8s_deploy.ps1 那行在可展开的 here-string 里,替换文案不得含 `$` 和反引号。
  - **(2026-10-19 已完成)** 3 处都已改完,12 处过时注释全部收尾(只改注释,未编译);见 PROGRESS「2026-10-19 G4a 收尾」条目。
- **B0-2**:提交号要回填到 release-checklist §6.1;`cross-zone-matchmaking.md:298`、`player-inbox-service.md:965` 的行号引用要更新;`MessageSyncRedisClient` 与死工程 `cpp/tests/redis_test` 可以一起删。
  - **(2026-10-20)**
    - 提交号已回填:B0-2 随 `e76f6f747` 入库,用 `git log -S` 核实过。
    - `cross-zone-matchmaking.md` 那一行(现约第 300 行)加了「已删除」的更正注。`player-inbox-service.md` 原来的 :965 引用已不存在,现约第 1040、1106 行两处写的是 `redis_client.h:42`,改成按常量名 `kSaveIfGuardLuaScript` 查。
    - 删 `MessageSyncRedisClient` 与死工程 `redis_test` 还没做:要改 game.sln,单独开任务。
- **B0-4**:
  - k8s_deploy.ps1 给 `NODE_OPEN_FILE_LIMIT_MODE` 接线;
  - C++ Deployment 显式写 `maxUnavailable: 0`;
  - `-WaitReady` 失败诊断时 grep 拒启行;
  - `deploy/k8s/AGENTS.md` 加一条「启动壳不加 ulimit」;
  - 用实测重新核 `kNonClientReserve`;
  - **Java 版待做**:启动时打印 `UnixOperatingSystemMXBean.getMaxFileDescriptorCount()` 并与需求比较,`start-slice.sh` 启动前打印 `ulimit -Sn` / `-Hn`(PARITY 已登记)。
  - **(2026-10-20 核对)**
    - 前四条都还没做:`k8s_deploy.ps1` 里没有 `NODE_OPEN_FILE_LIMIT_MODE`;`New-NodeDeploymentYaml` 没有 strategy 段;没有地方 grep 拒启行;`deploy/k8s/AGENTS.md` 也没加那一条。改这个 AGENTS 文件要负责人确认,见清单 X-2。
    - `kNonClientReserve` = 4096 仍是推算。
    - B0-4 已在 main 里,从 main 出的镜像缺省拒启:硬限低于 24096 / 8192 / 4096 的集群会 CrashLoop。所以前三条应在第一次从 main 出镜像部署之前做。
    - Java PARITY 第 240 行 B0-4 的 commit 列仍写「提交后回填」,实际已随 `e7bb46070` / `8b9592de3` 入库,留给 Java 线回填。
  - **(2026-10-22)前三条已落码、未验证**,详见 PROGRESS「2026-10-22 B0-4 部署侧后续项」条目:
    - `k8s_deploy.ps1 -NodeOpenFileLimitMode enforce|warn` 写进全部五套 C++ 模板,留空不写;
    - `New-NodeDeploymentYaml` 显式写 `maxUnavailable: 0`;
    - `-WaitReady` 失败时用 `kubectl logs --previous` 捞 `FATAL open file limit` 行。
    - 代码 10-20 被快照提交 `206647611` 带进 main;契约测试与文档在工作区。都未经 Codex 跑过,命令见该条目。
    - 第四条(`deploy/k8s/AGENTS.md`)待负责人确认,未改。包装入口(dev_tools / k8s_image / k8s_zone_rollback)尚未透传新参数。
    - (2026-10-22 续)负责人已确认,第四条已写进 `deploy/k8s/AGENTS.md` 的 CONVENTIONS(「C++ 启动壳不加 `ulimit`」)。包装入口透传另开会话在做。
    - (2026-10-22 再续,已落码、未验证)包装入口已透传 `-NodeOpenFileLimitMode`:`dev_tools.ps1`(含 `k8s-zone-rollback`)、`k8s_image.ps1`、`k8s_zone_rollback.ps1` 的 Step 6,留空不透传,与 `-GateRouterMode` 同口径。契约清单已钉住,Codex 命令见 PROGRESS「B0-4 部署侧:包装入口透传 -NodeOpenFileLimitMode」条目。回滚第 0 步不核对集群里的这个 env,以 warn 运行的 zone 回滚时要照传。
    - (2026-10-23 更正,已落码、未验证)回滚脚本第 0 步 a 现在核对:`-NodeOpenFileLimitMode` 留空时,`-Apply` 在 Step 1 停服之前读 gate / scene(含 Agones Fleet)模板里的 `NODE_OPEN_FILE_LIMIT_MODE`,任一为 warn 或读不到即拒绝;显式给值不核对。见 PROGRESS「2026-10-23:回滚脚本第 0 步核对 B0-4 nofile 豁免」。
- **B0-5**:A1 还没做:scene_manager 按 zone 选主、watch 与 knownNodes 收窄到本 zone,以及 rebalance 等领导者循环限定到本 zone。`handleWatchEvent` 的 PUT 换 zone 分支没有按领导者门控。
  - **(2026-10-20 核对)** 两条都仍未做,也没人认领。A1 多了一个新前置:10-19 EnterScene 陈旧位置会话新增的 `isZoneGoneFromRegistry`,以及既有的 `isNodeGoneFromRegistry`,都依赖 knownNodes 是全服镜像。watch 收窄之前,这两处要先改成按 zone 查注册表,否则别的 zone 会一律被判成已下线。另外,上 Cluster 后 `pipelineBatchRejected` 的整批判定要复核(G4a §17)。
- **B0-3**:stress_summarize 补 login 侧的回执送达小表;dispatch 线程池饱和时的回退路径仍不可观测;`TriggerPlayerDataPreload` 是死代码。
  - **(2026-10-20 核对)**
    - 三条都仍在。补充:`task_result_dispatcher.go` 的注释引用了 `pubsub_dispatch_overflow_total`,但这个指标全仓都不存在。
    - 小表并入 L-18 压测前置的指标任务(清单 X-3)。
    - 风险登记:task:result 所在 Redis 切到 Cluster 时,发布还用 PUBLISH 的话,`no_receiver` 与 NoReceiver 告警会误报。要先改 SPUBLISH 再切,已在 10-17 拍板的「SPUBLISH 推迟到上 Cluster 那批」范围内。
- **B0-1**:etcd 成功返回空的或残缺的存活集时没有合理性闸门(这是既有行为,需要另做设计);接线 /metrics 之后用压测校准告警阈值。
  - **(2026-10-20 核对)**
    - 闸门仍没有设计。后果是 etcd 重建、回滚或连错时,约 2 拍后会把全部在线会话判死,按每秒 1000 条转成 DISCONNECTING,30s 后全员被踢。已列入拍板清单 B1-1,阈值请负责人定。
    - 校准告警还缺数据通道:stress_snap 不抓 9190。
    - 另一条后续「给 `k8s_zone_rollback.ps1` 的 `Get-SharedRedisGuardPatterns` 补模式」已过时:该函数已随 8b9592de3 删除,关闭。
    - 文档债仍在:login `session_lifecycle.go` 约第 85 行的「#B-1 SCAN 巡检」、`disconnectlogic.go` 约第 50 行的 TTL 描述。
- **D1**:1b-2、1b-3、第 2 批、第 3 批归「D1 玩家归属与存储持久化」会话,见 §5。

### 第 6 项:至今没有归属的小项(来自全局单点交接 10-16 订正,做之前先核对当时的代码)

- refresh token:每个账号最多 32 枚,而且不作废;
- Java gateway 单 zone 准入容量默认 5000;
- scene_manager 的 `reassignSceneNode` 没有 CAS(`go/scene_manager/internal/logic/world_init.go`);
- match 的 `migrateLegacyQueues` 仍在;
- 每次存盘都打一条 LOG_INFO;
- 任务卡片「修复 Kafka 存盘失败后 MySQL 永久落后」(scene 发 DBTask 失败后,脏比较快照没有作废):10-15 由 go/db 存盘管线会话认领,该会话 10-18 说代码已全部落完,但没提这一项。接手前先在 PROGRESS 里搜索,确认是否已修。

**(2026-10-20 核对结果,只读,原文不删)**

| 项 | 现状 | 建议 |
|---|---|---|
| refresh token 32 枚、不作废 | 仍在。签发方是 go/login 的 `token.go`(`maxRefreshTokensPerAccount = 32`);本仓 java/gateway_node 只转调。Java 版有同样的问题 | 要拍板:收紧上限或签发时作废旧枚,还是加 device_id(客户端契约)。见清单 U-1 |
| gateway 单 zone 准入容量默认 5000 | 仍是 5000:`schema.sql`、`ZoneConfig.java`、`k8s_deploy.ps1` 的 `LoginQueueZoneCapacity`。排队门禁关着,今天不会拒登;本仓 gateway 只拿它算负载档。Java 版排队直接拿它当放行预算 | 先定 C_z(清单 U-2),再同批改 |
| `reassignSceneNode` 无 CAS | 仍无。领导者的 rebalance 与任意副本的 `initWorldScenesForZone` 可能同时改派同一个 scene 到不同节点,留下孤儿 scene 实体,scene_count 也会算错 | 改成 Lua CAS,失败时补 DestroyScene;2~3 个文件,可单独开任务。Java 已有令牌 + 版本号 CAS 的等价实现 |
| match 的 `migrateLegacyQueues` | 仍在。GlobalMatch 关闭时(dev / 本地)每个副本每分钟 SCAN 一次 `match:queue:*` | 项目未上线,可以整体删掉旧格式兼容,约 10 个文件 |
| 每次存盘打 LOG_INFO | 仍在,而且每次真写盘打两条(`player_lifecycle.cpp` 的 enqueued 行与 `HandlePlayerAsyncSaved` 开头);`cross-zone-failure-test-runbook.md` 用这两行取证 | 降为 DEBUG,同时改演练手册的取证方式,2~4 个文件 |
| Kafka 存盘失败后 MySQL 永久落后 | **已由 S5a 处理**(代码已落,未编译):不作废快照,改成按玩家记投递账本,投递失败后强制重存;MySQL 最多落后一个存盘周期。所以上面「该会话没提这一项」的说法不准确 | 不用再写代码;用例已并入验证单 §9 |

**(2026-10-22 注,上表原文不删)`reassignSceneNode` 无 CAS:已落码,未在本线验证。**
- **代码**:10-20 落地,经保存提交 `206647611` / `6ecf275fa` 进 main。
  - `world_init.go` 的 `reassignSceneNode` 改为 Lua CAS:只在属主仍为 oldNode 时,同批改归属、两侧反向索引与 scene_count。init 改派漏记 scene_count 的偏差一并修掉。两处懒改派也改走它。
  - `world_rebalance.go` 的 `migrateWorldChannel` 在 CAS 失败时,对自己预建的 newNode 补一次尽力而为的 DestroyScene;重读确认 newNode 不是当前属主才销毁。
  - 回归用例在 `reassign_cas_test.go`,共 7 条。
- **验证**:10-21 整合记录写了「WorldChannelRace 定向组通过」,但没有用例数,修复前红也没跑。完整命令与通过标准见 PROGRESS 2026-10-22 同名条目。
- **残留**:`enterscenelogic.go` `resolveScene` 的死节点自愈仍是无条件 SET,是同类的第三个写者。修它要先定 CAS 落空时回给客户端的码,未做;另开任务。
  - (2026-10-22 续,已落码、未编译、未测试)已改走 `reassignSceneNode`(新方法 `reassignSceneOffDeadNode`)。CAS 落空时重读一次属主,活着就直接用,没有新开错误码;Agones 名额只在 CAS 成功后转移。回归用例 `resolve_scene_reassign_cas_test.go` 6 条,Codex 命令见 PROGRESS「2026-10-22 resolveScene 死节点自愈改走 reassignSceneNode CAS」。
  - (2026-10-23 再续,已落码、未编译、未测试,上一条的命令作废)对抗评审后又改了一轮:
    - 死节点自愈只重建大世界频道,用 `world_channels` 成员资格确认 conf,提交时在 CAS 脚本里再核对一次(新增 `reassignWorldChannelNode`,防缩容排空交错);副本 / 镜像不再在别处重建,留给死节点收尾销毁。
    - 顺序改为先建实体、再 CAS、最后转名额,与 rebalance 相同。建场景失败时不改归属,下一次请求重来;CAS 落空时回收预建的实体。
    - `TransferAgonesRoomForScene` 的记录改写改为比较并交换。
    - zone 守卫区分「没记录」与「读出错」。
    - 用例 10 条(另有 Agones 转移用例 2 条)。Codex 命令以 PROGRESS「2026-10-23 resolveScene 死节点自愈收窄为只重建频道」为准。
    - 合并后待办:「Go scene_manager 读 Redis 出错不再当成映射不存在」会话的 worktree 合回后,由后合的一方做两处。一是懒改派 `lazyReassignStaleWorldChannel` 第 3 步改走 `reassignWorldChannelNode`;二是把两边同为 3s 的收尾时限常量合成一个。

**(2026-10-22 注,上表原文不删)match 旧格式队列兼容、每次存盘的 INFO:两项都已落码,未编译、未测试。**
- **match**:删掉 `migrateLegacyQueues` / `migrateLegacyQueue` / `legacyQueueSweepInterval`、`legacyMatchQueueScanPattern` / `legacyMatchQueueKey`、只给迁移用的 `ticketSetQueueKeyScript`,以及 `dequeueMemberCtx` 对无 hash tag 键的单 key LREM。`ticketQueueKeys` 改成只返回新键的 `ticketQueueKey`,CancelQueue 与 `leaveQueueForOtherBattle` 跟着改。
  - 用例:删 `TestMigrateLegacyQueues*` 两条、`TestCancelQueueLegacyTicketFallsBack`、`writeLegacyTicket`,删 keys / PVE 人数档 / JoinQueue 自愈里的旧键断言,删 party 撤单回归里两处「无版本遗留」子用例。迁移用例后半段的镜像补写断言改为单列的 `TestMatcherBackfillsMissingRankMirror`,补写路径本身没动。
  - 代码 10-20 写完,已被别的会话的保存提交带进 main(`1b41d8f1e` 等)。`cross-zone-matchmaking.md` 的 D2、§4.2、§10、§11.9 已追加删除注。
- **存盘日志**:`player_lifecycle.cpp` 的两条成功 INFO 降为 DEBUG;失败分支只留汇总 ERROR,原 INFO 里的 home_zone / owner_epoch / save_seq 并进 ERROR。演练手册 §3.1 新增第 7 条(调级方法与不调级时的替代证据),§2.4 与场景 B3a 加了指向注;`cross-zone-scene-travel.md`、`db-task-kafka-partition-contract.md`、`db-task-delivery-and-retention.md` 追加了带日期的更正。
- **验证**:命令见 PROGRESS「2026-10-22 match 旧格式队列兼容删除与存盘日志降级」。
- **新发现,未处理**:`LogLevel` 按 muduo 枚举解释(0=TRACE、1=DEBUG、2=INFO),`node.cpp` 的 `InitLogSystem` 直接强转。`k8s_deploy.ps1` 生成的 node ConfigMap 写的是 `LogLevel: 1`,所以 K8s 上所有 C++ 节点实际跑在 DEBUG:这次降级在 K8s 上不减日志,热路径的 DEBUG 日志也一直开着。`release_preflight.ps1` 的 `debug.cpp.loglevel` 按「0=DEBUG、下限 1」检查,把它放过了;`bin/etc/base_deploy_config.yaml` 的注释对照也是错的。另开任务。
  - **(2026-10-22 已处理,未执行,待 Codex 验证)**:node ConfigMap 改为从 `bin/etc` 镜像 `LogLevel`(默认 2 = INFO,生成期校验 0..5),预检下界改为 2,注释改为 muduo 枚举,契约测试补了三条。K8s 上已在跑的 C++ Pod 要重启才生效(node-config 无配置指纹)。见 PROGRESS「2026-10-22 C++ 节点 LogLevel 口径统一」。

## 4. 硬约束

- **AGENTS §10.1**:Claude 只改代码与文档,不跑编译、测试、gofmt、proto 生成或压测;也不要派「能不能编过」的静态评审,评审只看语义和运行期。每项交付都要附 Codex 可执行的命令。
- **不 `git add -A`,不提交,不 push**;提交等用户发话。
- **多会话并行**:工作区随时会被整区快照提交。改文件前先 `git diff` 分清改动归属;一律用 Edit 精确替换,不回退、不覆盖别人的改动。
- **Java 仓**(`D:\luyuan\xuanming-server-mmo`)**只记待做项**:核对下来「不适用 / 已有等价」的,不往 PARITY.md 加行,结论写回本仓文档。10-19 已撤掉本线之前加的 4 行纯「不适用」(B0-2、B0-5、B0-3 第 2 批、B0-1)。保留的 3 行:B0-4(Java 待做)、B0-3 第 1 批(db_task.proto 的 ContractSync 同步待做)、D1(由 D1 会话维护)。(2026-10-20 核对:B0-3 第 1 批的同步其实已经做完。Java 副本从 cea4d88 起含 `reply_instance = 10`,Codex 10-19 跑过 ContractSync 的 fullsync / fullcheck,原生 0。那一行等 Java 线确认 game-proto 构建通过后收口;本会话没有改 Java 仓。)
- **文档**:PROGRESS 只追加;更正一律追加带日期的更正注,不删原文;新增文档要在 `docs/README.md` 登记,格式与 `tools/scripts/gen_docs_index.py` 一致。
- **额度**:开多代理工作流前先看用量。落码类代理按「代理数 × 5%」估算 5 小时窗口;同一账号上的多个会话共用额度,不够就分批。每个实现代理的提示里都带续做说明(先看现状、在半截改动上续做)。

## 5. 与其他会话的分工(10-18 时的状态)

| 会话 | 负责 | 本线与它的边界 |
|---|---|---|
| D1 玩家归属与存储持久化 | D1 的 1b-2 / 1b-3 / 第 2 批 / 第 3 批;10-18 起 D1 全部文件归它 | 1b-1 的最终文件清单已移交。不要再改 `go/data_service/internal/{routereconcile,routing,routetool,routetoolcli,store,config,metrics,svc}`、`go/shared/placement`、`tools/merge_zone`、提案 §11 |
| go/db pipeline 交接文档 | 存盘管线 R1–R8 | 10-18 说 go/db 代码已全部落完,之后不再改;B0-3 第 2 批的文件清单已记进它的交接 `godb-pipeline-impl-handoff-20261015.md` §7.7 |
| G4a 修订(任务卡片会话) | `docs/design/shared-redis-split-by-purpose.md` | 它做完之前,别的会话不要改这份文件(2026-10-19:已完成,此限制解除;之后改这份文件先 `git diff`,只用 Edit 追加带日期注) |
| redis-ha 部署路径 | 见 `redis-ha-deploy-path-handoff-20261019.md` | 与本线不重叠 |

## 6. 关键文件索引

- 规格与核实意见:`docs/handoff/launch-readiness-20261013/spec_*.md`(调研规格的前半是规格,「# VERDICT」之后是核实意见;D1 的以提案 §11.2 为准)。
- 设计:
  - [capacity-model-hok-scale.md](../design/capacity-model-hok-scale.md) §7(2000 万口径);
  - [shared-redis-split-by-purpose.md](../design/shared-redis-split-by-purpose.md)(G4a);
  - [player-locator-gate-session-index.md](../design/player-locator-gate-session-index.md)(B0-1);
  - [player-route-mapping-durability-proposal.md](../design/player-route-mapping-durability-proposal.md)(D1);
  - [tidb-phase1-and-launch-readiness.md](../design/tidb-phase1-and-launch-readiness.md)。
- 运维:
  - `docs/ops/release-checklist.md`:§6.1 dirty_keys_set 清理,附录 F.B-1 会话定向补救;
  - `docs/ops/gate-kernel-tuning-runbook.md` §3.1(nofile);
  - `docs/ops/scene-node-role-split.md` §6.1(scene_manager 登记集合补登记);
  - `docs/ops/merge-zone-runbook.md` §9.4(route-tool);
  - `docs/ops/godb-pipeline-launch-todo.md` §10(task:result 上线)。
- 告警(都是手工 apply):
  - `deploy/k8s/scene-manager-alerts.yaml`
  - `deploy/k8s/task-result-notify-alerts.yaml`
  - `deploy/k8s/player-locator-alerts.yaml`

## 7. 给新账号的开场指令(可直接复制)

```
请接手 docs/handoff/handoff-launch-line-remaining-20261019.md 的工作。
1. 先按 AGENTS.md「会话启动门禁」读规定材料,再读这份交接全文(尤其 §2 已拍板、§4 硬约束、§5 分工)。
2. 从 §3 第 1 项开始:把本线代码交 Codex 验证,按 PROGRESS「2026-10-18 开服线收尾」条目的总顺序给出命令;拿到结果后归因,本线的问题修复,别人的如实报告。
3. 再看 §3 第 2 项 G4a 是否已由另一个会话完成;没完成且那个会话已停,按规格续做。然后按 §3 第 3 项做 L-18 复核,第 4 项整理成拍板清单问我。
4. 编译、测试、proto 生成、冒烟都交给 Codex:你只改代码和文档,不派「能不能编过」的评审。工作区多会话并行,不 git add -A、不提交。
5. Java 仓只登记待做项。每完成一项在 docs/PROGRESS.md 末尾追加条目(只追加),写明 Java 状态。
6. 遇到新的拍板项、改动超过 30 个文件、要下载镜像或装工具时,先停下来问我。
```

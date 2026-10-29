# 玩家持久收件箱(player_inbox)运维手册

> **状态**:v1.1 — 2026-10-17 按评审修复的实际代码订正:第 A 批(Go 服务)、第 B 批(C++ scene)、第 C 批的部署(C1)、告警(C2)与 scene 周期 WARN(C3),
> 条目编号见交接 `docs/handoff/player-inbox-handoff-20261015.md` §6。v1 首版 2026-10-15 随设计第 4 批「运维、部署与对账」落码。
> 2026-10-18 第 C 批复核修复:写前门禁加线上交叉核对与来源核对(§2.1),回滚只回退镜像、不检出旧 commit 重跑(§2、§11),
> scene 日志的 LogQL 选择器改为 sidecar 真有的标签(§2.1、§5),账本校验口径写明收件箱多查 request_id 为 0(§5.2、§10.4)。
> **全部未编译、未测试、未在任何环境演练**,待 Codex 验证(§15)。本手册以代码为准:代码行为变了,要回来同步这里。
> **依据**:`docs/design/player-inbox-service.md`(下称「设计」)v1.0,尤其 §1.4、§1.6、§7.8、§8、§9、§11.7;
> 计数器倒退的通用处置见 `docs/design/db-write-fencing-mysql.md`(下称「围栏设计」)v0.4 §9.1。
> **部署接线**:`deploy/k8s/README.md`「玩家收件箱 player-inbox」;告警 `deploy/k8s/player-inbox-alerts.yaml`;合服审计见 `docs/ops/merge-zone-runbook.md`。

---

## 1. 一览

| 项 | 值 | 出处 |
|---|---|---|
| 进程 | `go/player_inbox`,全服一份、多副本(K8s 在 `mmorpg-infra`,`Global = $true`) | 设计 §1.4(D3) |
| 端口 | gRPC 50950、metrics 9270(本地 `-Zone 2` 位移为 51950 / 10270) | D14 |
| 服务 | `player_inbox.PlayerInbox`(scene 调 `FetchItems` / `AckItems`)、`player_inbox.PlayerInboxAdmin`(运维) | `proto/player_inbox/` |
| 发现 | etcd 前缀 `PlayerInboxNodeService.rpc`(节点类型 30);自己的 go-zero `Etcd.Key` 为空(D-13) | `bin/etc/base_deploy_config.yaml` |
| C++ deadline | `GrpcClient.CallDeadlineMs.PlayerInboxNodeService = 6000`,部署门禁核对 ≥ 服务 `Timeout` + 2000 | 同上 |
| 库 | 独占库 `mmorpg_player_inbox`:`player_inbox_stream` / `player_inbox_item` / `player_inbox_audit` | `deploy/mysql-init/00_init_zone_dbs.sql` |
| 消费端账本 | 各 zone 库 `player_database.inbox_ledger`(字段 20),与 currency 同一次存盘 | 设计 §4.4 |
| 运维口令 | Secret `player-inbox-admin-token` → 容器 `PLAYER_INBOX_ADMIN_TOKEN`;ConfigMap 只写 `PlayerInbox.AdminTokenEnv`(变量名) | `k8s_deploy.ps1` |
| 入箱开关 | 服务 yaml `PlayerInbox.EnqueueTypes`(空 = 停用入箱)。**v1 只放封锁 / 解封**,列了 `GM_ATTACH_CURRENCY_DEBT` 启动即拒(见下) | 设计 §3.3;`config.ParseEnqueueTypes` |
| 消费开关 | git 跟踪的 `bin/etc/game_config.yaml` 的 `player_inbox.consumer_enabled`(只认小写 `true` / `false`)。K8s 的 node-config 整段复制这一段;`-PlayerInboxConsumer on|off` 只核对、不改值(§2.1) | 设计 §11.5、§11.7;`k8s_deploy.ps1` |
| 分片 | `RowShard = mix64(player_id) % 64`(2026-10-16 起)。启动与 `-migrate` 在表结构对齐之后核对行布局,不一致拒启(§2.2) | `internal/inbox/shard.go` |
| 对账读口 | data_service `GetPlayerInboxLedger`(读 zone Redis 的 `PlayerAllData` blob,不读 MySQL,不校验、原样返回) | 设计 §5.1、§9.3 |

**部署侧与服务配置的键名契约**(K8s ConfigMap 由 `k8s_deploy.ps1` 生成,键名必须与 `go/player_inbox/internal/config` 逐字一致):
`Name` / `ListenOn` / `Timeout` / `Mode` / `Middlewares.StatConf.IgnoreContentMethods` / `Etcd{Hosts, Key:""}` / `ZoneId` / `LeaseTTL` / `MetricsListenAddr` / `KillSwitchPrefix` /
`MySQL{Host, User, Password, DBName, MaxOpenConn, MaxIdleConn}` / `Schema.AutoMigrate` / `CoreRedis{Host, Password, DB, PoolSize}` /
`Kafka{Brokers, WriteTimeoutMs}` / `DataServiceRpc` / `PlayerInbox{AdminTokenEnv, EnqueueTypes, TerminalRetentionDays, SweepIntervalSeconds,
NotifyResendIntervalSeconds, GaugeIntervalSeconds, ReconcileIntervalMinutes, ReconcileWindowHours}`。
- `PlayerInbox.AdminTokenEnv` 是口令所在的环境变量名,非空时从该变量取口令;与字面 `AdminToken` 二选一,两者都非空拒启。仓库里的 `etc/player_inbox.yaml` 永远不带口令值。
- `Middlewares` 段从服务 yaml 整段复制,只屏蔽 `FetchItems` / `AckItems` 的请求体日志(评审 L12)。go-zero 的 Stat 缺省开着,这时没列这两个方法,
  `config.Validate` 就拒启,`player-inbox-migrate` Job 也一样。**所以部署脚本不能晚于这一版 Go 上线**(§2 前置)。
- `CoreRedis.PoolSize` 必须是正整数(当前 20),Redis 连接总账读同一个键;读写超时 1s 与 `ContextTimeoutEnabled` 写在代码里,不是配置项。

**它不是什么**:不是邮件、不是资产通道。收件箱不可用不阻断进场(IB-7):scene 只退避。v1 只有一个域 `SCENE_GM_SANCTION`,注册了三种类型:
`GM_BLOCK_CURRENCY_GAIN` / `GM_UNBLOCK_CURRENCY_GAIN` / `GM_ATTACH_CURRENCY_DEBT`,**v1 只开放前两种**(用户拍板 2026-10-16,交接 §6 M2 / M3):
- `GM_ATTACH_CURRENCY_DEBT` 列进 `EnqueueTypes`,`config.Validate` 与装配层都拒启。GM 工具对它的请求回 `kPlayerInboxDisabled`。
- 原因一:合并语义有缺陷。scene 挂欠款会把同币种的新单合并进旧欠款,并用后来那单覆盖到期时间、原因与操作人。
  一张小单就能让大额欠款提前失效,过期欠款的剩余部分也会被复活。
- 原因二:没有撤销路径。挂错之后,v1 没有任何类型能把它撤掉。
- 合并语义、撤销类型(按金额减少)与单笔上限一起做完之前不开。

---

## 2. 上线顺序与回滚下限(设计 §11.7)

**严格按顺序**,每一步验收通过再进下一步。任一步失败就停在那一步,不要跳。

**前置**:
- 部署脚本要先于或同批于这一版 Go 上线。这里的部署脚本指带 `Middlewares` 整段复制与消费开关门禁的 `k8s_deploy.ps1`。
  旧脚本渲染的 ConfigMap 没有 `Middlewares` 段,新版 Go 与 `player-inbox-migrate` Job 都过不了 `config.Validate`(§1)。
- 用 2026-10-16 之前的版本写过数据的收件箱库,先按 §2.2 处理。

| 步 | 动作 | 验收 |
|---|---|---|
| 1 | 每个 zone 库跑 go/db 的 `cmd/migrate`(先 `-command plan` 再 `-command up`),加 `inbox_ledger` 列;收件箱库与表就位(K8s:`infra-up` 的 `player-inbox-migrate` Job;本地:启动期 AutoMigrate 或 `player_inbox -f etc/player_inbox.yaml -migrate`);TiDB 上做 §12 的索引预切 | `plan` 只出现 `inbox_ledger`(以及已放行的 `fence_*`)的 ADD COLUMN;`SHOW COLUMNS FROM zone_<N>_db.player_database` 末列是 `inbox_ledger`;`kubectl -n mmorpg-infra get job player-inbox-migrate` 为 1/1;`-migrate` 输出里有 `row shard layout check OK` |
| 2 | 全部 zone 的 go/db 升到认识新字段的版本 | `kubectl get deploy db -o jsonpath='{..image}'` 各 zone 一致 |
| 3 | 部署收件箱服务,`EnqueueTypes` **为空** | `player-inbox` 两副本 Ready;etcd 有 `PlayerInboxNodeService.rpc/...` 键;`GmSanctionPlayer` 返回 `kPlayerInboxDisabled`;写路径打印 `player-inbox consumer gate: consumer_enabled=false ... enqueue_types=0 ... OK` |
| 4 | 全部 scene 升级,`consumer_enabled=false`(`bin/etc/game_config.yaml` 里默认就是 `false`;K8s:逐 zone `zone-up`) | **这就是回滚下限**。存档能原样往返 `inbox_ledger`,scene 不发 Fetch(收件箱 `player_inbox_fetch_total` 不涨)。每个 scene 启动时打一行 WARN `[PlayerInbox] metric=inbox_consumer_disabled`,之后每 5 分钟补打一行:这一步出现它是预期的 |
| 5 | 打开消费:把 `bin/etc/game_config.yaml` 的 `player_inbox.consumer_enabled` 改成 `true` 并提交,再逐 zone `zone-up` 并滚动 scene(ConfigMap 变了不会自己重启 C++ 进程)。可以加 `-PlayerInboxConsumer on`,把预期写进命令(§2.1) | 门禁行变为 `consumer_enabled=true`;scene 启动行变为 INFO `[PlayerInbox] consumer_enabled=1 ...`,`metric=inbox_consumer_disabled` 不再出现;`player_inbox_fetch_total{path="fast"}` 随登录上涨,`fence` 以 `NO_STREAM` 为主 |
| 6 | 打开入箱:改服务 yaml 的 `PlayerInbox.EnqueueTypes` 并提交,重跑 `infra-up`。**v1 只能填** `PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN` 与 `PLAYER_INBOX_MSG_GM_UNBLOCK_CURRENCY_GAIN`。第 5 步没做完(有 zone 线上 node-config 还是 `false`),门禁会拒绝(§2.1) | 门禁行为 `enqueue_types=2 ... live=zones_consumer_enabled=N/N OK`(N = 线上 zone 数,两个数相等);生成期打印的 ConfigMap 里 `EnqueueTypes` 有值;一次测试处置能入箱并在测试账号上生效 |

**v1 不开 `GM_ATTACH_CURRENCY_DEBT`**(原因见 §1):
- 白名单里出现它,`config.Validate` 与装配层都拒启(`config.ParseEnqueueTypes`)。
- K8s 上先失败的是 `player-inbox-migrate` Job(同一份 ConfigMap,同样跑 Validate)。staging / prod,或 dev 带 `-WaitReady` 时,
  发布停在 Deployment 之前;dev 不带 `-WaitReady` 时脚本不等 Job,Deployment 照常滚动,新 Pod 表现为 CrashLoopBackOff。
- 部署脚本渲染期不拦它:脚本里不复制第二份禁用清单。

**回滚**:回退到下限(第 4 步的版本)以上的**镜像**都安全。只回退镜像,不回退开关:
- 用 `kubectl rollout undo`,或用**当前检出**的 `k8s_deploy.ps1` 配旧 `-NodeImage` / `-GoSvcTag` 重跑。开关值仍取当前检出的 yaml。
- **不要检出旧 commit 再跑它的部署**。消费开关的权威值就是 git 里的 `bin/etc/game_config.yaml`,检出旧 commit 会把 `consumer_enabled` 一起回退:
  - 下限那一版(第 4 步)本身就是 `false`;
  - 旧 commit 的门禁只比那个 commit 里的两份 yaml(第 6 步之前白名单是 `[]`),所以照样放行;早于 H1 修复的脚本按 `-PlayerInboxConsumer` 缺省 `off` 渲染,根本没有门禁。
  - 结果是 scene 全部停止消费,而线上白名单仍开着:GM 处置回 `created=true`,一条都不执行。
- 确实要重跑旧 commit:先核对它的 `consumer_enabled` 与当前一致;不一致就先按 §2.1「正常关消费的顺序」关掉入箱。
  旧 commit 本身已带线上核对(§2.1 第 2 层,本次修复之后的提交)时,它的门禁读到 infra 线上白名单非空会拒绝;更早的提交没有这一层,不会拒。
- **低于下限**(scene 或 go/db 回到不认识 `inbox_ledger` 的版本)是破坏性变更,按 §11 的前置步骤执行。

**为什么第 4、5 步要分开**:scene 旧版本按组件重建存档会丢掉新字段;go/db 旧版本按主键只更新已知列,账本列会停在旧值(违反 IB-3)。先让全部 scene / go/db 都「认识并原样回写」,再开消费,任何时刻回退都不会丢账本。

### 2.1 消费开关与写前门禁(评审 H1)

以前 `-PlayerInboxConsumer` 默认 off、不粘滞,包装入口也不透传。开了消费之后,任何一次 `k8s-release-zone` / `zone-up` / `zone-rollback`
都会把它写回 `false`;GM 处置照样回 `created=true`,却一条都不执行,也进不了任何告警。现在的规则:

- **权威值只有一份**:git 跟踪的 `bin/etc/game_config.yaml` 的 `player_inbox.consumer_enabled`,与 `EnqueueTypes` 取服务 yaml 同口径。
  `k8s_deploy.ps1` 把 `player_inbox` 段整段复制进 node-config(`periodic_fetch_minutes` 一起带过去)。渲染值不从集群取;
  集群现状只用于下面写前门禁的交叉核对。
- 脚本读的是**工作区文件**,不是 HEAD。staging / prod 的真部署要求这两份 yaml 相对 HEAD 没有未提交改动,否则拒绝;dev 与 DryRun 只告警。
- 值只认小写 `true` / `false`。缺键,或写成 `yes` / `True` 等,生成期直接 throw。
- 这份 yaml 本机 scene 也读,改了它本机同时生效。本机副本 scene 读 `bin/etc/game_config_instance.yaml`,那里没有这一段,所以永远不消费。
  K8s 的副本池与主 scene 共用同一份 node-config,不受影响。
- **开 / 关消费 = 改 yaml 并提交**,再逐 zone `zone-up` 并滚动 scene。
- `-PlayerInboxConsumer on|off` **只核对、不改值**:留空就不核对;给了值而与 yaml 不一致,在任何写操作之前拒绝,并提示去改 yaml。
  所以旧命令 `zone-up -PlayerInboxConsumer on` 不再能打开消费。
- **写前门禁**(`Assert-PlayerInboxConsumerGate`):`zone-up` / `all-up` / `infra-up` 都判,先于任何集群写操作,拦「消费端关、入箱开」。分三层:
  1. **当前检出里的两份 yaml**:`consumer_enabled=false` 而服务 yaml 的 `EnqueueTypes` 非空。
     这一层只拦同一个检出里的矛盾。zone 与 infra 分开部署、可以来自不同 commit,它拦不住「检出旧 commit 重跑 zone-up」,也拦不住「先跑 infra-up」。
  2. **另一侧线上的值**(非 DryRun,只读,`Read-PlayerInboxGateLiveFacts`):
     - 本次把 `false` 渲染进 node-config(`zone-up`、`all-up -SkipInfra`):读 infra 线上 `go-svc-player-inbox-config` 的白名单,非空即拒。
       不带 `-SkipInfra` 的 `all-up` 不读:它先把 yaml 的白名单写进 infra,zone 才滚动,第 1 层核过的就是这一份。
     - 本次把非空白名单渲染进 ConfigMap(`infra-up`、`all-up`):读线上全部 `<-NamespacePrefix>-*` namespace 的 node-config,有 zone 不是 `true` 即拒。
       `all-up` 先部署白名单、后部署 zone,所以「同一提交里开消费又填白名单」不能用一次 `all-up` 完成:先 `all-up -SkipInfra` 或逐 zone `zone-up`。
     - 读不到(kubectl 失败、没有跨 namespace 的读权限、ConfigMap 形状不对)就拒绝。DryRun 不读集群,门禁行写 `live=skipped(dry-run)`。
     - **局限**:读的是 ConfigMap,不是进程。C++ 与 go-zero 只在启动时读配置,ConfigMap 改了而 Pod 没滚动,门禁看到的就不是生效值。
       所以每一步仍要按验收确认生效(scene 启动行、`GmSanctionPlayer` 的回包),再进下一步。
  3. **来源**:staging / prod 的真部署要求两份 yaml 已提交(见上)。
  - 明知如此仍要部署(例如紧急关消费,来不及先清白名单):直接调 `tools/scripts/k8s_deploy.ps1`,显式传 `-AllowPlayerInboxConsumerOff`。
    它只豁免第 1、2 层,打一条警告放行,不改渲染值。staging / prod 仍要先提交改过的 yaml(第 3 层不豁免)。
  - **包装入口没有这个开关**:`dev_tools.ps1`、`k8s_image.ps1`、`k8s_zone_rollback.ps1` 都不认它(加上只会报「找不到参数」)。
    紧急关消费之后 git 停在「`false` + 白名单非空」,此后包装入口的部署与 zone 回档(第 0 步的 DryRun 预检)都会被拒。
    出路是先把 `EnqueueTypes` 改回 `[]` 并提交、重跑 `infra-up`,也就是下面的正常顺序,门禁随之放行。
  - **zone 回档**(`k8s_zone_rollback.ps1`)的第 0 步预检带 `-DryRun`:只判第 1 层;第 2 层不读集群,第 3 层只告警。
    这两层要到第 6 步真 `zone-up` 才判,那时 zone 已经停服。所以回档之前先核对两件事,否则第 6 步被拒、zone 停在停服状态:
    - 当前检出 yaml 里 `consumer_enabled=false` 时,infra 线上的 `EnqueueTypes` 必须已经是空的
      (`kubectl -n mmorpg-infra get cm go-svc-player-inbox-config -o yaml` 里找 `EnqueueTypes`);
    - staging / prod:两份 yaml 没有未提交改动(第 0 步的输出里不能有 `player-inbox consumer gate: ... 有未提交改动` 这条警告)。
  - 放行时打一行 `player-inbox consumer gate: consumer_enabled=<值> (...) enqueue_types=<N> (...) head=<sha> worktree=<clean|dirty> live=<线上核对> OK`,
    发布记录里会留着它。
- **正常关消费的顺序**:
  1. 先把 `EnqueueTypes` 改回 `[]` 并提交,重跑 `infra-up`;确认 `GmSanctionPlayer` 回 `kPlayerInboxDisabled`,这一步才算生效。
  2. 再把 `consumer_enabled` 改回 `false` 并提交,逐 zone `zone-up` 并滚动 scene。
  - 顺序反了:第 2 层读到 infra 线上白名单仍非空,`zone-up` 被拒。前提是非 DryRun,且 infra 的 ConfigMap 确实还是旧值;
    改了 ConfigMap 而 player-inbox 的 Pod 没滚动这种情况它看不出来,所以第 1 步要以 `GmSanctionPlayer` 的回包为准。
- **开消费的顺序**(上线第 5、6 步):全部 zone `zone-up` 并滚动 scene、scene 打出 `consumer_enabled=1` 之后,才提交 `EnqueueTypes` 并跑 `infra-up`。
  顺序反了:`infra-up` 读到有 zone 线上还是 `false`,被拒。

**怀疑开关被关了**:信号是 `PlayerInboxConsumerMaybeDisabled`(warning),或白名单已打开的环境里出现 scene 日志 `metric=inbox_consumer_disabled`。
1. 逐 zone 查 scene 日志。收件箱全服一份、指标不分 zone,只有这条日志能定位到 zone。流选择器必须是 sidecar 真会打的标签
   (C++ 日志没有 `app` 标签,写 `{app=...}` 的查询恒为空,会被误判成「开关开着」):
   ```logql
   sum by (zone) (count_over_time({job="cpp_nodes", service="scene"} |= "[PlayerInbox]" |= "metric=inbox_consumer_disabled" [10m]))
   ```
   单看一个 zone 就在选择器里加 `zone="<zone 名>"`。
2. 核对 git 里的 `bin/etc/game_config.yaml`,以及该 zone node-config 里 `game_config.yaml` 的 `player_inbox` 段。
   常见原因:yaml 被改回 `false`、检出开消费之前的旧 commit 重跑了 `zone-up`(§2「回滚」),或部署时用了 `-AllowPlayerInboxConsumerOff`。
3. 按第 5 步重开并滚动 scene。
- 误报:6 小时内新入箱的目标玩家全部离线时也会亮。这时 `player_inbox_fetch_total{path="fast"}` 仍随登录上涨,说明 scene 在拉。
- 局限:别的 zone 有条目被投递,就会盖住关了开关的那个 zone。逐 zone 只能看第 1 步的日志,写前门禁才是主防线。

### 2.2 分片公式改为 mix64 之前写过的库(评审 L5)

2026-10-16 起 `RowShard = mix64(player_id) % 64`,之前是 `player_id % 64`。旧公式写出的行,对所有按玩家的读写**静默不可见**:
- 锁内判重读不到旧行,同一 `request_id` 重试会再入箱一次(IB-2 失效);
- 旧的 PENDING 行再也拉不到;
- 旧流行若挂着 `notify_due_ms`,所在分片的提醒补发每一轮都整片报错。

新版在表结构对齐之后、接请求之前逐表核对 `(shard, player_id)`(`inbox.CheckRowShards`):
- 启动路径:不一致即拒启。
- `-migrate`:以退出码 4(需人工)退出。K8s 的 `player-inbox-migrate` Job 停下,不反复重试。
- 错误信息写明表名、不一致的行数和第一行。

处置:
- **只在一次性的开发 / 测试库上**清表重来;其他库停下来上报,不要删行。
- 清表(或整库重建)之后,再跑 `player_inbox -f etc/player_inbox.yaml -migrate`,输出里应有 `row shard layout check OK`;直接启动时不拒启即通过:
  ```sql
  -- 只在一次性的开发 / 测试库上执行
  DROP TABLE IF EXISTS mmorpg_player_inbox.player_inbox_stream, mmorpg_player_inbox.player_inbox_item,
    mmorpg_player_inbox.player_inbox_audit, mmorpg_player_inbox.schema_migrations;
  ```
- 被删掉的未决处置不会再执行,还要的话用新的 `request_id` 重下。
- zone 库的 `inbox_ledger` 不用动:新流行的 seq 带时间下界(§8),一定大于旧号。

---

## 3. 运维接口的调用方式

所有 `PlayerInboxAdmin` 方法都要 metadata `x-admin-token`。口令为空 = 整个运维面停用(返回 `PermissionDenied` 或停用错误,以服务实现为准),**这不是故障**。

```powershell
# K8s:从运维机转发(不要把服务暴露到集群外)
kubectl -n mmorpg-infra port-forward svc/player-inbox 50950:50950

# 口令只放进当前进程环境,不要写进命令历史文件或工单
$env:PLAYER_INBOX_ADMIN_TOKEN = '<从密钥库取>'

# 查询某玩家各域概况(含 max_fence_epoch)
grpcurl -plaintext -import-path . -proto proto/player_inbox/player_inbox_admin.proto `
  -H "x-admin-token: $env:PLAYER_INBOX_ADMIN_TOKEN" `
  -d '{"player_id": 123456}' 127.0.0.1:50950 player_inbox.PlayerInboxAdmin/GetSummary
```

- 在仓库根目录执行(proto 的 import 路径以仓库根为准)。
- 其它方法同法:`ListItems`(`player_id` + `domain` + 可选 `state`,`after_seq` 翻页,`limit ≤ 200`)、`LookupByRequest`、`VoidItem`、`ReconcilePlayer`、`GmSanctionPlayer`。
- 枚举在 JSON 里可写名字,例如 `"domain": "PLAYER_INBOX_DOMAIN_SCENE_GM_SANCTION"`、`"state": "PLAYER_INBOX_ITEM_PENDING"`。
- `LookupByRequest` 按 GM 工单查条目:`-d '{"player_id": 123456, "producer_kind": "PLAYER_INBOX_PRODUCER_GM", "request_id": <工单里的 request_id>}'`。
  作废之后确认终态都用它(§5.2、§6)。
- 所有响应的业务错误在 `error_message`(tip 段 29000,`kPlayerInbox*`;参数不合法是通用的 `kInvalidParameter`);只有 `kPlayerInboxStoreUnavailable` 是服务端故障。
  例外:`ReconcilePlayer` 读到结构不合法的账本时也回它,重试不会好(§10.3)。

**审计能不能对到人**(评审 L8):写操作的审计行记 `operator`(调用方自报)和 `peer_addr`(对端地址)。
- 按上面的方法经 `kubectl port-forward` 调用时,`peer_addr` 恒为回环地址;口令又是共享的,所以审计行本身对不到人。
- 要对到人,只能看 kube-apiserver 审计日志里 `pods/portforward` 子资源的记录:谁、什么时候、转发到哪个 Pod。
  前提是集群开了 apiserver 审计,且审计策略记录这个子资源;没开就对不到人。
- 所以每次写操作都要在工单里记下操作人与时间,请求里的 `reason`(处置)或 `note`(作废)写工单号。由凭据推出 operator 是中期项,还没做。

**口令轮换**:换环境变量 `MMORPG_PLAYER_INBOX_ADMIN_TOKEN` 后重跑 `infra-up`。Pod 模板上的口令指纹会变,Deployment 自动滚动,旧口令随旧 Pod 退场。
不注入口令(任何档位)= 运维接口停用;注入了则 staging / prod 的占位口令、短于 32 位,任何档位短于 16 字节或与其它口令相同,部署在写操作之前被拒。
**停用运维面**:不带 `MMORPG_PLAYER_INBOX_ADMIN_TOKEN` 重跑 `infra-up`,残留 Secret 会在新 Deployment 之后删除。

---

## 4. GM 处置与批量处置脚本规范

### 4.1 单条处置的生产方契约(设计 §6.2)

- `request_id` 与 `issued_at_ms` **按工单只生成一次**,先记进工单再发第一次请求;**重试原样带回两者**。
- 刷新 `issued_at_ms` 重试会得到 `kPlayerInboxIdempotencyConflict`:这是保护,不要「换个时间再试」。
- `issued_at_ms` 只接受 `[now−7d, now+5min]`;超过 7 天的工单不能重放,只能用新的 `request_id` 重新下一条。
- 响应 `created=false` = 同键同内容已存在(DUPLICATE),返回原 `seq` 与当前状态,**算成功**。
- `operator` 必填(≤ 64B),`reason` 必填(≤ 256B);`attach_debt.gm_operator` 由服务端用 `operator` 覆盖。
- 处置是按 seq 严格顺序执行的。**要改主意,下一条新的处置**(新 `request_id`):例如封锁之后要解封,就下一条解封;
  v1 没有「人工补发」,把旧条目挪到队尾会颠倒封锁与解封的先后(设计 §7.9)。

### 4.2 批量处置脚本(限速与重试)

批量脚本(按名单对很多玩家下同一类处置)必须满足下面全部规则,评审时逐条核对:

1. **先落盘名单与键**:为每个玩家生成 `(player_id, request_id, issued_at_ms)` 写进本地 CSV / 工单附件,**写完才开始发请求**;
   脚本中断后从这份文件续跑,不重新生成任何键。`request_id` 用 64 位随机数或工单号派生,不能用自增计数器(两次跑会撞)。
2. **限速**:单个脚本并发 ≤ 4、总速率 ≤ 20 次/秒(GM 域按小量设计,入箱持流行锁;值待压测确认)。多个运维同时跑脚本时总速率合计不超过这个数。
3. **只重试可重试的失败**:gRPC `Unavailable` / `DeadlineExceeded`,以及 `kPlayerInboxStoreUnavailable`。
   指数退避(起 200ms、封顶 5s、±20% 抖动),**单条最多 5 次、单条总预算 30s**;超出就把这一条标为「未完成」写回 CSV,继续下一条,不无限重试。
4. **不重试、直接记录并跳过**:`kPlayerInboxPlayerNotFound`、`kPlayerInboxPayloadInvalid`、`kPlayerInboxPayloadTooLarge`、
   `kPlayerInboxTextTooLong`、`kPlayerInboxIssuedOutOfWindow`、`kPlayerInboxUnknownType`、`kPlayerInboxIdempotencyConflict`、`kPlayerInboxFull`。
   `Conflict` 说明名单或脚本有缺陷,**停下整批**查清再继续;`Full` 说明该玩家未决条目已满(`max_pending` 64),先查这名玩家为什么不消费。
5. **`kPlayerInboxDisabled` 立刻停整批**:白名单没开或被关了,不是这条的问题。v1 的 `attach_debt` 一律回它(§1),脚本不要发这类处置。
6. **结束时出对账表**:成功(`created` / DUPLICATE)、跳过(按错误码分组)、未完成三类计数,附在工单上;未完成的按第 1 条续跑。
7. 脚本只打印 `player_id` 与错误码,**不打印口令**,不把口令写进 CSV。

---

## 5. 告警处置(`deploy/k8s/player-inbox-alerts.yaml`)

**收件箱服务的 Prometheus 规则**(四组:correctness / stuck / reconcile / info):

| 告警 | 级别 | 含义 | 处置 |
|---|---|---|---|
| `PlayerInboxFenceRegressed` | critical | 判 STALE,且 Redis 当前值不大于请求 epoch:核心 Redis 的 `owner_epoch` 倒退了 | §7 |
| `PlayerInboxEpochAhead` | critical | 请求 epoch 超过 Redis 当前值(只会来自缺陷或伪造),已拒绝、未改状态 | 按 ERROR 日志里的 `peer` / `scene_node_id` 找发送方,核对版本与 epoch 来源;**不要抬任何计数器** |
| `PlayerInboxCursorAhead` | critical | 消费方账本水位 ≥ 收件箱 `next_seq`:收件箱库被恢复过或流行被删 | §8 |
| `PlayerInboxAckAnomaly` | critical | Ack 出现 MISMATCH / CONFLICT / NOT_DELIVERED / SKIP_NOT_REQUESTED / UNKNOWN_SEQ | §5.1 |
| `PlayerInboxReconcileMismatch` | critical | 对账读数 > 0:由消费方终结的行,已落盘账本里没有它的结局。这是读数,不是事件数,处置之后不会马上消失 | §10.2 |
| `PlayerInboxConsumerMaybeDisabled` | warning | 6 小时内有新入箱,却没有一次锁路径 Fetch:scene 消费开关可能被关了 | §2.1 |
| `PlayerInboxDeliveredUnackedTooOld` | warning | 最近 24 小时内投递过的行里,已投递未确认最老的超过 1 小时 | §6 |
| `PlayerInboxPoisonSuspect` | warning | 最近 24 小时内投递过的行里,有条目投递 ≥ 5 次仍未终结 | §6 |
| `PlayerInboxNotifyBacklog` | warning | 在线提醒从流行最后一次写入起悬了 5 分钟以上没落定(退避中的也算) | §5.3 |
| `PlayerInboxSqlPoolSaturated` | warning | 某副本独占库连接池在用 > 90% 持续 15 分钟(`go_sql_*{db_name="player_inbox"}`) | 看 `go_sql_wait_duration_seconds_total` 与 `fetch_total{path="locked"}`;调大池要同步 MySQL 连接总账与 `max_connections` |
| `PlayerInboxSettledUnknown` | warning | UNKNOWN 报告以 SETTLED_UNKNOWN 收尾 | 对照 scene 的 `[PlayerInbox] applied` 日志与玩家存档人工核实,必要时用新 `request_id` 重下 |
| `PlayerInboxReconcileStalled` | warning | 对账开着(近 7 天跑过),3 小时内却没有一轮成功 | §10.1 |
| `PlayerInboxReconcileRoundsFailing` | warning | 对账轮次 error / timeout | §10.1 |
| `PlayerInboxReconcileTruncated` | warning | 对账有轮次没核对完整个窗口 | §10.1 |
| `PlayerInboxReconcileLedgerInvalid` | warning | 对账读到结构不合法的已落盘账本 | §10.4 |
| `PlayerInboxDeliveredUnackedStale` | info | 有最后一次投递早于 24 小时、仍未确认的行 | §6;一般不用处置 |
| `PlayerInboxForceVoided` | info | 有已投递的行被强制终结(`VoidItem mode=FORCE_MISMATCH`) | 对照审计 `action=FORCE_VOID` 与工单(§5.2) |
| `PlayerInboxOpenItemsTrend` | info | 最老未决条目超过 30 天(多为流失玩家) | 只看趋势 |

**scene 日志信号**(scene 没有 Prometheus 端点。LogQL 写在告警文件头,Loki ruler 还没接线,目前是值班固定查询):
- 流选择器一律写 `{job="cpp_nodes", service="scene"}`,逐 zone 再加 `zone="<zone 名>"`。C++ 业务日志只落文件、由 Alloy sidecar 送 Loki,
  sidecar 不打 `app` 标签:写成 `{app=~"scene.*"}` 的查询恒为空,会被误判成「没有问题」。契约测试核对告警文件里的选择器只用 sidecar 真有的标签。

| 日志 | 级别 | 含义 | 处置 |
|---|---|---|---|
| `metric=inbox_replay_mismatch` | critical | 回放核对失败(IB-9):账本环里同 seq 记的是另一个 request_id(`ledger_request_id=`)。该玩家该域停用,每次落地都会再停 | §5.2 B |
| `metric=inbox_halted` | warning | 该玩家该域停用,`reason=` 给出原因(`unknown_type` / `replay_mismatch` / `ack_anomaly` / `version_skew` 等)。停用对当前实体一直有效,实体重建(重新登录,或换到别的节点)后才重拉 | 按 `reason`:§5.2 / §5.1 |
| `metric=inbox_unknown_type` | warning | scene 不认识这个类型或指令(新类型先于 scene 支持被加进白名单),停在这一条,不记 REJECTED | §5.2 A |
| `metric=inbox_ack_anomaly` | warning | scene 收到异常 Ack 回包(结果异常,或服务端回 InvalidParameter),停用该域 | §5.1 |
| `metric=inbox_ledger_invalid` | warning | 该玩家已落盘账本结构校验不过,收件箱全部域停用 | §10.4 |
| `metric=inbox_registry_broken` | warning | scene 处置注册表自检失败,本节点整体停用消费。启动时一条 ERROR,之后每 5 分钟补打 | §10.4 |
| `metric=inbox_consumer_disabled` | warning | 本节点 `consumer_enabled` 关着。启动时一条 WARN,之后每 5 分钟补打;上线第 4–5 步之间出现是预期的 | §2.1 |
| `metric=inbox_leave_ack` | info | 玩家离开本节点时没能补发 Ack 的行(`result=delivered_undecided` / `not_durable` / `fetch_inflight` / `ack_inflight` / `not_sent`) | §6;不是故障 |
| `metric=inbox_deferred` | info | 延后原因分布(`reason=`) | §6 第 2 步 |

不会自愈的三条(`inbox_ledger_invalid` / `inbox_registry_broken` / `inbox_consumer_disabled`),问题还在时 scene 会按节拍补打同一行。
两行间隔钉在告警的 `[10m]` 窗口之内(scene 规则头文件里 `kSceneLogAlertWindowMs` 的 `static_assert`),所以告警会一直亮着。改窗口要同步改那组常量。

### 5.1 Ack 异常结果

- **CONFLICT**:终态行与 scene 报告的结局不同。**永不翻转**(IB-4)。保留现场:`ListItems` 导出该行、对照 scene 日志里同 seq 的 `[PlayerInbox]` 行与玩家存档,
  判断哪边是真;需要补偿就下一条新处置。不要改库。
- **MISMATCH**:同 seq 的 `request_id` 不一致(IB-9)。最常见于收件箱库恢复之后(§8):恢复回来的旧行与消费方账本记的不是同一条。
  scene 收到它会停用该域,之后每次落地都会再停;按 §5.2 B 解卡。
- **NOT_DELIVERED / SKIP_NOT_REQUESTED / UNKNOWN_SEQ**:scene 报告了从未投递给它的行、或在没有作废指令时报 SKIPPED、或报告了不存在的 seq。
  只会来自 scene 缺陷或内网伪造(设计 §1.6)。按 ERROR 日志里的 `peer` / `scene_node_id` 定位 scene 节点与版本;状态没改,不需要回滚任何东西。
- **整次 Ack 回 InvalidParameter**:scene 发出的确认列表不合法(seq 不升序、超过上限等),只会来自 scene 缺陷。
  只有 scene 日志 `metric=inbox_ack_anomaly` 看得到,收件箱侧没有对应告警。scene 停用该域,不再重发;按 scene 版本查缺陷。

### 5.2 停用的解卡:未知类型与 MISMATCH(评审 M1)

scene 对一条投递的判定顺序是:先按账本分类,只有新条目才看作废指令,最后才查执行器。
- 账本里已执行过(回放):照报原结局,不看指令。
- 水位已过、原结局已挤出最近结局环:报 UNKNOWN,收件箱以 SETTLED_UNKNOWN 收尾。
- 账本环里同 seq 是另一个 request_id(MISMATCH):停用该域,什么都不确认,**不看指令**。
- 新条目挂着 VOID 指令:**不查执行器、不解码载荷**,直接记 SKIPPED。不认识的类型也一样(第 B 批起)。
- 新条目没有指令:查执行器,不认识的类型就停用(不记 REJECTED)。

所以两种停用的解法不同。停用对当前实体一直有效:解卡之后,要等实体重建(退出完成后再登录,或换到别的节点),scene 才重拉。退出保留期内重登会复用同一个实体(同节点、owner_epoch +1),停用仍有效、不重拉;下面各处说的"下次登录"都指实体重建之后的那次。

**A. 未知类型**(`metric=inbox_unknown_type` + `metric=inbox_halted reason=unknown_type`)
1. 先止血:把该类型移出 `EnqueueTypes` 并重跑 `infra-up`(只拦新入箱),或先把 scene 升到支持它的版本。
2. 已入箱的那条有两种办法:
   - scene 升级之后,它会照常执行;
   - 不要它了:普通模式 `VoidItem`(`reason=PLAYER_INBOX_VOID_CANCELLED`,`note` 写工单号)。已投递的行只挂 VOID 指令;
     玩家下次登录时 scene 按「新条目 + VOID」记 SKIPPED,停用解除,后面的条目照常执行。
3. 用 `LookupByRequest` 确认终态:`state=VOIDED`、`finalized_by=CONSUMER`。还是 PENDING 就是玩家还没再登录。
- 日志里是 `directive=` 而不是 `msg_type=`:scene 不认识这个指令值,只会来自版本错位,升级 scene。

**A′. 版本错位**(`metric=inbox_halted reason=version_skew`,前一行 ERROR `Executor reported version skew` 带 `seq` / `type` / `request_id`;2026-10-19 起)

scene 认识这个类型,但本版本不能安全执行这一条。v1 只有一个来源:挂欠款时,玩家同币种的既有欠款带着本版本不认识的字段(更新版本的 scene 写下的,见 `mixed-version-persist-field-protection.md` §4.4)。
出现在新旧 scene 混跑(滚动发布、金丝雀)期间;整体回滚到不认识这些字段的版本之后不会自行消失(见第 3 条)。这一条不记账、不确认,已定的前缀照常确认,行留在收件箱 PENDING。同域排在它后面的处置(例如 `GM_BLOCK_CURRENCY_GAIN`)也一起停住。
1. 不要作废它,它是合法处置。让全网 scene 跑认识这些字段的版本,也就是把滚动发布走完;玩家下次落到这种节点时(实体重建之后)照常执行。
2. 确实不要它了,或必须先让同域后面的处置执行:同 A 第 2 条,普通模式 `VoidItem`。玩家下次登录时按「新条目 + VOID」记 SKIPPED。要重下这笔欠款,等认识这些字段的版本全量上线后再用新的 `request_id` 重下;仍跑旧版本时重下会再次 `version_skew`。
3. 已经整体回滚到不认识这些字段的版本:数据里的欠款仍带着新字段(旧版原样携带),玩家每次重新落地都会再停用一次。只有三种情况会解除:重新升级到认识这些字段的版本;按第 2 条 `VoidItem`;那笔同币种欠款被 `AddCurrency` 扣到还清而删除(冻结或已过期的欠款不会被扣,`WaiveDebt` 目前没有生产入口,不能指望)。全网都是认识这些字段的版本却还在出现,按 scene 版本与发布单查缺陷,不要手改存档。

**B. MISMATCH**(`metric=inbox_replay_mismatch` + `metric=inbox_halted reason=replay_mismatch`,或 Ack MISMATCH 之后的 `reason=ack_anomaly`)

普通 `VoidItem` 解不开:已投递的行只能挂指令,而 MISMATCH 不看指令。要用**强制模式**:服务端先读这名玩家**已落盘**的账本,
证明它在这个 seq 上裁决的是另一条、本行从未执行,才把本行终结为 VOIDED(`finalized_by=ADMIN`),同事务写审计 `action=FORCE_VOID`。

1. 记下 scene 日志里的 `player_id` / `domain` / `seq`、`delivered_request_id` 与 `ledger_request_id`(账本环里同 seq 那一条的 request_id)。
2. `ReconcilePlayer(player_id, domain)`:这一行应出现在 `diffs` 里,`kind=PENDING_BUT_DECIDED`、`state=PENDING`、`request_id` 等于 `delivered_request_id`。
   不在就不是这种情况,停下。
3. 调强制模式。`reason` 必须非 UNSPECIFIED,建议 `CANCELLED`(写进行的 `void_reason`);`note` **必填**,写工单号与依据,空着回 `kInvalidParameter`:
   ```powershell
   grpcurl -plaintext -import-path . -proto proto/player_inbox/player_inbox_admin.proto `
     -H "x-admin-token: $env:PLAYER_INBOX_ADMIN_TOKEN" `
     -d '{"player_id": 123456, "domain": "PLAYER_INBOX_DOMAIN_SCENE_GM_SANCTION", "seq": 789, "reason": "PLAYER_INBOX_VOID_CANCELLED", "operator": "<自己>", "note": "<工单号> MISMATCH ledger_request_id=<值>", "mode": "PLAYER_INBOX_VOID_MODE_FORCE_MISMATCH"}' `
     127.0.0.1:50950 player_inbox.PlayerInboxAdmin/VoidItem
   ```
4. **判成功**:`error_message` 为空,`force_verdict == PLAYER_INBOX_FORCE_VOID_PROVEN`,且 `state == PLAYER_INBOX_ITEM_VOIDED`。三条缺一不算。
   不认识 `mode` 的旧版服务端会按普通模式只挂指令(`void_requested=true`、`force_verdict` 为空),这**不是**成功。
5. `LookupByRequest` 复核:`state=VOIDED`、`finalized_by=ADMIN`、`delivery_count > 0`。普通模式直接作废的行 `delivery_count` 恒为 0,以此区分。
6. 让玩家重新登录。这一行已终结、不再投递;之前停住的前缀按账本回放并确认,后面的条目照常执行。再用 `ReconcilePlayer` 复核,应无差异。
7. 被终结的这条处置在玩家身上**从未生效**;仍要执行,就用新的 `request_id` 重下(§4.1)。

重试规则:
- 回 `kPlayerInboxStoreUnavailable`(读账本或库故障):按 §4.2 第 3 条退避重试。
- 同一条再调一次:行已是终态,回 `kPlayerInboxItemStateConflict`(带当前 `state`)。是否已强制终结,看第 5 步。

`force_verdict` 的拒绝值(都回 `kPlayerInboxItemStateConflict`,行不变、不写审计):

| `force_verdict` | 含义 | 处置 |
|---|---|---|
| `NOT_DELIVERED` | 本行从未投递 | 不是 MISMATCH 行;确要作废就用普通模式,直接 VOIDED |
| `LEDGER_ABSENT` | data_service 读不到该玩家已落盘的存档 | 无从证明,不要强制。查存档为什么不在 zone Redis(Redis 丢数据先按 §7 / 回档流程定真源);玩家落地并存盘之后再试 |
| `NOT_DECIDED` | 账本里该域水位没到这个 seq:消费方还没裁决它 | 停用原因不是 MISMATCH。按 scene 日志 `inbox_halted` 的 `reason` 处理(未知类型见 A) |
| `SEQ_NOT_IN_RING` | 水位已过,环里却没有这个 seq:原结局已挤出环,可能已执行 | 不要强制。scene 对它报 UNKNOWN,收件箱以 SETTLED_UNKNOWN 收尾,按 `PlayerInboxSettledUnknown` 人工核实 |
| `REQUEST_IN_LEDGER` | 环里出现过本行的 request_id:可能已执行 | 不要强制。同 seq、同 request_id 是正常回放(确认丢了),玩家下次登录会补确认。同 seq、同 request_id、只有 msg_type 不同,只可能来自数据损坏,v1 没有工具,停下上报负责人;出现在别的 seq 上也上报 |
| `LEDGER_INVALID` | 账本结构不合法。条件与 scene 判账本损坏相同,另多查一条 request_id 为 0;只有这一项不合法时 scene 不判损坏,表现为 `inbox_replay_mismatch` | §10.4 |
| `UNSPECIFIED` | 行不存在(`kPlayerInboxItemNotFound`)或已是终态 | `LookupByRequest` 看当前状态 |

### 5.3 在线提醒积压(`PlayerInboxNotifyBacklog`)

提醒只是加速:落地拉取与 scene 在线兜底(默认 30 分钟)照常生效,条目不丢。
- 积压从流行最后一次写入(`updated_ms`)起算,悬 5 分钟以上算一条;补发没落定、正在退避的流也计入。
- 落定(清掉 `notify_due_ms`)的结局:`sent` / `offline` / `awaiting_placement` / `no_node`。`no_node` 只剩「位置记录既无节点也无 epoch」,交给落地拉取。
- 其余按退避重试:`error` / `empty_uuid` / `nodes_unsynced` / `node_ambiguous` / `node_missing`(评审 L6)。
  - 退避取「已悬时长」,钳到 30 秒到 30 分钟之间,所以间隔大致按 2 倍增长,封顶 30 分钟。重试用 next_seq 做 CAS,不会丢新入箱的提醒。
  - Fetch / Ack / 作废推进了流行的 `updated_ms`,退避就回到 30 秒起。
- `node_missing`:节点镜像已同步,却查不到位置记录里的节点,多为 etcd 租约抖动或 watch 重连。
  它悬满 1 小时才落定,并打 ERROR `Notify resend settled after the scene node stayed missing`。节点真没了时,告警约亮 50 分钟后自行消失。
- 其余重试类问题不解决,告警就一直亮。

按 `player_inbox_notify_total{result}` 各取值的增速判断是哪一类:
- `error`:查 Kafka 写入与核心 Redis;
- `empty_uuid` / `node_ambiguous`:查 scene 节点在 etcd 里的注册(uuid 缺失、同一身份多条注册);`node_ambiguous` 在旧租约过期后自愈;
- `nodes_unsynced`:收件箱刚启动时正常,持续出现就查 etcd 连接;
- `node_missing`:查位置记录指向的节点还在不在;
- 计数都不涨:看有没有领导者在跑(收件箱 INFO 日志 `Maintenance leader acquired`)。

---

## 6. 卡住与毒消息(设计 §7.9、D11)

v1 **不做自动隔离**。

**告警口径**(评审 M4):
- `PlayerInboxDeliveredUnackedTooOld` 与 `PlayerInboxPoisonSuspect` 只统计**最后一次投递在最近 24 小时内**的行(代码常量,不是配置项)。
- 更早投递、仍未确认的行计入 `player_inbox_delivered_unacked_stale`,只出 info 级的 `PlayerInboxDeliveredUnackedStale`。
  典型原因是 GM 处置之后紧接着封号踢下线,玩家不再上线;玩家下次落地会重拉并补确认,一般不用处置。
  数量持续上涨,或目标玩家其实一直在线,才按下面的步骤查。

**玩家离开时的补发**(第 B 批):玩家离开本节点时,退出存盘收敛之后、实体销毁之前,scene 尽力补发一次 Ack,不等回包。
- 只覆盖「结局已定、且已落盘」的行。
- 覆盖不到的,停在「已投递未确认」,等玩家下次落地重拉或补确认,scene 日志 `metric=inbox_leave_ack` 留一行线索:

| `result=` | 含义 |
|---|---|
| `sent` | 补发了(不等回包)。收件箱那边应已终结;没终结,看收件箱的 Ack ERROR 日志 |
| `not_sent` | 补发没发出去(例如没有可用的收件箱节点) |
| `not_durable` | 结局还没落盘,不能确认(IB-3) |
| `fetch_inflight` | 离开时 Fetch 在途,回包到达时实体已销毁,被丢弃 |
| `ack_inflight` | 离开时这批的 Ack 已在途:服务端照样处理;它若在实体销毁后失败(失败回调只打 DEBUG),这批停在已投递未确认,按 `corr=` 对上 scene 日志 |
| `decided_prefix_queued` | 不是残留:退出期回包被前置闸丢掉时,批首账本里已有结局的条目(回放 / UNKNOWN)先入确认队列、照常确认,只丢需要执行的剩余部分 |
| `delivered_undecided` | 已投递、还没定结局就丢掉了:退出期回包被前置闸丢掉(`source=apply_gate_exiting`;冻结 / 交接与退出同时存在时也算退出期),或执行器延后剩下的条目(`source=leaving`) |

这些都不是故障,不用处置。玩家一直不上线的,24 小时后转入 stale 口径。

**判断与处置**:

1. `ListItems`(`state=PENDING`)找出 `delivery_count` 高、`last_delivered_ms` 老的行;记下 `domain` / `seq` / `request_id` / `msg_type`。
2. 对照 scene 日志(`[PlayerInbox]`,按 player_id 搜)看是哪种:
   - **反复投递、每次都没确认,同时 scene 有崩溃 / 重启**:疑似毒消息(载荷让 scene 崩溃)。
   - **scene 一直判 `inbox_deferred`**(冻结 / 交接 / 战斗中 / epoch 为 0):不是毒消息,等条件解除;战斗中的处置在解冻时立即重查。
     `reason=ledger_invalid` 是账本损坏,按 §10.4 处理。
   - **该域停用**(`metric=inbox_halted`):按 §5.2 / §5.1 处理,作废指令解不开 MISMATCH。
   - **存盘失败导致一直不 Ack**:看 scene / go/db 的存盘告警,先修存盘。
   - **玩家已离开**:看上表的 `metric=inbox_leave_ack`。
3. 确认是毒消息后,`VoidItem`(`reason=PLAYER_INBOX_VOID_QUARANTINED`,`operator` 写自己,`note` 写工单号):
   - 从未投递过(`delivery_count=0`):直接 VOIDED(`finalized_by=ADMIN`)。
   - 已投递:**只挂 VOID 指令**。回包 `void_requested=true`、`state` 仍是 PENDING,**这不等于已取消**。结局要等 scene 下次拉到这一行时裁决:
     - 账本里还没有这一条(新条目):不查执行器、不解码载荷,记 SKIPPED,不会再触发崩溃。收件箱终结为 VOIDED(`finalized_by=CONSUMER`)。
     - 账本里已执行过(回放):照报原结局,收件箱终结为 APPLIED / REJECTED,**作废没有生效**。
     - 水位已过、原结局已挤出环:报 UNKNOWN,收件箱以 SETTLED_UNKNOWN 收尾。
     - 该域已因 MISMATCH 停用:指令不起作用,按 §5.2 B 处理。
4. **必须用 `LookupByRequest` 确认终态**(§3),不要只看 `void_requested`:

| 终态 | 说明 |
|---|---|
| `VOIDED`,`finalized_by=ADMIN`,`delivery_count=0` | 从未投递,直接作废 |
| `VOIDED`,`finalized_by=CONSUMER` | scene 记了 SKIPPED:已取消,没有执行 |
| `APPLIED` / `REJECTED` | scene 在指令之前已裁决:作废没生效。要撤回效果,下一条新的处置(例如解封) |
| `SETTLED_UNKNOWN` | 是否生效无法从账本确认:按 `PlayerInboxSettledUnknown` 人工核实 |
| `PENDING`(`directive=VOID`) | 还没裁决:玩家还没再落地。长期离线的玩家会一直停在这里 |
| `VOIDED`,`finalized_by=ADMIN`,`delivery_count > 0` | 强制终结(§5.2 B) |

5. 隔离之后要不要重下一条修正过的处置,由负责人定;重下一律用新 `request_id`。
6. 作废限速与批量脚本同(§4.2 第 2、3 条)。

---

## 7. owner_epoch 计数器倒退(设计 §7.8、§8 #14;围栏设计 v0.4 §9.1)

**症状**:`PlayerInboxFenceRegressed` critical;某玩家进场后收件箱一直判 STALE,GM 处置不生效。通常同时有 go/db 的 `DbOwnerEpochRegressed` / 存盘被拒告警。

**原则**:**定真源、抬计数器、不归零**。收件箱**没有**归零围栏的接口,也不要直接改库把 `fence_epoch` 调小:僵尸 scene 会把围栏重新推回旧的高值。

**步骤**(按围栏设计 §9.1 a–f,本节只补收件箱这一项):

1. **定真源**、**踢下线**并确认 `player:{id}:location` 不存在(§9.1 a、b)。
2. **取收件箱的高水位**:`GetSummary(player_id)` 的 `max_fence_epoch`(各域流 `fence_epoch` 的最大值),写进工单。
3. **抬计数器**(§9.1 c):`C' = max(当前计数器, 各落点库两张玩家表的 fence_owner_epoch, 各 topic 世代的 applied_epoch,
   收件箱的 max_fence_epoch, 资产证据 player:{id}:asset_sql_commit 里的 epoch) + 余量 K`。入口已定(围栏设计 §14 第 5、20 条),**工具还没落码**:
   - 入口是独立工具 `go/db/cmd/epoch_raise`。按玩家执行,默认只演练,带 `-confirm` 才写;输出每个来源的取值与抬升前后的值。
   - 收件箱这一来源由工具**自己**经 `PlayerInboxAdmin.GetSummary` 读 `max_fence_epoch`,不直连收件箱的库(即收件箱遗留项 C4)。
   - 任一来源读不到,工具默认拒绝执行。所以要用它时,收件箱和它的运维面(§3)都必须可用。
   - `-floor` 只是给未知来源的补充入口,不要拿它代替工具自己读收件箱。
   - 余量 K 是常数(建议 10^6 量级),**不要求先排空** Kafka 积压与重试队列。
   - 工具落码之前:**不要**手写 Lua 或 SQL 改计数器。按围栏设计的人工流程由负责人执行,第 2 步的 `max_fence_epoch` 作为取最大值的一个输入。
4. 缓存与死信按 §9.1 d、e。
5. **验证**:玩家下一次进场拿到 `C'+1`,大于收件箱围栏,判 ADVANCE;`GetSummary` 的 `fence_epoch` 变为新值,积压的处置开始生效。

---

## 8. 收件箱库恢复(设计 §8 #18)

`mmorpg_player_inbox` 在开发 MySQL 上由 `mysql-backup-cronjob.yaml` 的 `mysqldump --all-databases` 覆盖;迁到 TiDB 后要另纳入 BR / PITR(未做)。

**恢复后系统自己会做的**:
- 新 seq 取 `max(next_seq, now_ms << 12)`,带时间下界,一定大于恢复前已分配的号,不会复用;
- 消费方水位领先 `next_seq` 时,Fetch 记 CursorAhead 并在同一事务里把 `next_seq` 抬到水位之上(告警 `PlayerInboxCursorAhead`);
- 恢复回来、已被消费方处理过的旧行,scene 按 IB-9 核对 `(seq, request_id, msg_type)`:一致就回放原结局确认,在环外就以 UNKNOWN 收尾,不一致就 MISMATCH 并停用该玩家该域。

**要人工做的**:
1. 记下恢复点时刻 T0 与恢复完成时刻 T1。
2. **恢复窗口内丢失的条目**(T0 之后入箱、恢复后不存在):对照运维侧工单记录(§4.2 第 1 条的 CSV)与 scene 的 `[PlayerInbox] applied` 日志,逐条判断「是否已经在玩家身上生效」。
   - 已生效:不用补;
   - 未生效、仍要执行:**用新的 `request_id` 重下**。**不能按原 `request_id` 重放**:原条目若已在某个 scene 上应用过,账本按 seq 去重,用原键重放会得到新 seq、被再执行一次。
3. **MISMATCH 停用的玩家**:按 §5.2 B 逐个处理。普通模式的 `VoidItem` 解不开这种停用:已投递的行只能挂指令,而 MISMATCH 不看指令。
   - `ReconcilePlayer` 先看到该行是 `PENDING_BUT_DECIDED`;
   - 再调 `VoidItem(mode=FORCE_MISMATCH)`,`note` 必填;
   - 以 `force_verdict=PROVEN` 且 `state=VOIDED` 判成功,再用 `LookupByRequest` 复核;
   - 玩家重新登录后,该域恢复。
   被终结的那条处置在玩家身上从未生效,仍要执行就用新的 `request_id` 重下(同第 2 条)。
4. 恢复期间收件箱不可用不影响进场(IB-7),不需要停服。

---

## 9. 回档预检(设计 §8 #16、D7)

玩家回档会把账本(`inbox_ledger`)连同货币一起退回快照时刻,而收件箱里那段时间已终结的 APPLIED 行不会再投递 —— 回档之后,那些处置在玩家身上「消失」了。

- **现状**:单人回档的栅栏未接线,`RollbackPlayer` 目前一律返回 NotImplemented(`go/data_service/internal/logic/rollback_logic.go`)。
  预检的自动化与回档接线同批做;在那之前,任何回档(含整区回档)都按下面的人工步骤预检。
- **人工预检**:
  1. 取快照里该玩家 `player_database.inbox_ledger` 各域的 `decided_through_seq`(快照数据的取法见 `docs/ops/online-debug-data-fetch.md`)。
  2. `ListItems(state=PLAYER_INBOX_ITEM_APPLIED)`,列出 seq **大于**快照水位的行 —— 这些就是回档后会「消失」的处置。
  3. 差异清单写进回档工单,负责人**显式接受并写明原因**才继续回档。
  4. 回档完成后,对清单里仍要执行的处置**用新 `request_id` 重下**(§4.1)。
- 整区回档时,这一步对全区玩家逐个做不现实:先用对账作业(§10)的结果圈出受影响的玩家,只对他们做。

---

## 10. 对账与账本损坏(设计 §9.3)

### 10.1 定时对账怎么跑

- 只在领导者上跑。当选后第一轮在 1–5 分钟内随机起跑,之后每 `PlayerInbox.ReconcileIntervalMinutes`(默认 60)分钟一轮;`0` = 关掉对账。
- 每轮看最近 `ReconcileWindowHours`(默认 24)小时内由消费方终结的行,经 data_service `GetPlayerInboxLedger` 读已落盘账本比对(判定见 §10.2)。
  `config.Validate` 要求窗口覆盖至少两轮(窗口小时 × 60 ≥ 2 × 间隔分钟)。每轮预算 min(间隔, 2 分钟)。
- **起始分片按步长轮转**(评审 M5、R1):第 k 个间隔(k = 当前毫秒 / 间隔)从分片 `k × 4 % 64` 起环形扫描。
  - 不依赖内存状态,易主后新领导者从同一个位置接着转。
  - 每轮至少读完从起点起的 4 片,所以连续 16 轮必然把 64 片各读完一遍。
- **工作量上限**:每页 100 行,按 `updated_ms` 降序翻页(最新的先核对);单片一轮最多 500 行;一轮最多 2000 名玩家;
  账本 8 路并发读,每次读上限 2 秒。
- **截断**:触到任一上限,本轮就算截断。`player_inbox_reconcile_truncated_total` 加 1(每轮至多加 1),并打 ERROR `Reconcile round truncated`,
  写明起始分片、扫了几片、哪些分片读满上限、停在哪片。告警是 `PlayerInboxReconcileTruncated`。
  - 截断持续时,每片至多隔 16 轮才读全一次;没读到的分片沿用上次读数,critical 读数可能偏低。
  - 被单片上限截掉的是一片里**最老**的行。这一片一直有新行顶着时,它们可能一直核对不到。
  - 窗口不足 16 个间隔又发生截断时,另打一条 ERROR:有的分片可能整个窗口都轮不到。
  - 处置:量级持续超出(例如批量处置之后)时,缩短 `ReconcileWindowHours`(不少于 16 个间隔),或调大代码里的上限。
- **存活**:`player_inbox_reconcile_rounds_total{result}`,取值 `ok` / `error` / `timeout`;领导权丢失、关停取消的轮次不计。
  - `PlayerInboxReconcileStalled`:3 小时内没有一轮 ok。先看有没有领导者(INFO `Maintenance leader acquired`),再看 error / timeout 与 ERROR `Reconcile round`。
    规则里的 `[3h]` 按默认间隔 60 分钟写死,改间隔要同步改告警。有意关掉对账之后,这条最多再亮 7 天,静默即可。
  - `PlayerInboxReconcileRoundsFailing`:error 多为收集分片时读库失败;timeout 多见于 data_service 读账本变慢,或收件箱库变慢。
- **账本读结果** `player_inbox_reconcile_ledger_read_total{result}`:
  - `found`:正常比对;
  - `absent`:zone Redis 里没有存档 blob,结论 unknown,不告警;
  - `error`:读失败,那一片的读数只是下界,留给之后的轮次;
  - `invalid`:账本结构不合法,结论 unknown,不计入不一致读数。告警 `PlayerInboxReconcileLedgerInvalid`,按 §10.4 处理。
- data_service 侧:`data_service_player_inbox_ledger_read_total{result="error"}` 非零看 data_service 同一时刻的 `[player-inbox-ledger]` ERROR 日志
  (home_zone 查不到回 Unavailable、Redis 读失败 / blob 损坏 / player_id 不符回 Internal,**绝不**伪装成 found=false)。

### 10.2 对账不一致(`PlayerInboxReconcileMismatch`)

**判定**(评审 M6):定时对账与 `ReconcilePlayer` 共用同一个谓词。只比由消费方终结的终态行;管理员终结的行(直接作废、强制终结)不比。
缺口有三种,只写进收件箱 ERROR 日志 `Reconcile mismatch` 的 `gap=`:

| `gap=` | 含义 |
|---|---|
| `beyond_watermark` | 账本水位没到这个 seq(四种终态都比,含 SETTLED_UNKNOWN) |
| `missing_in_ring` | 水位已过,seq 落在最近结局环该覆盖的范围内,环里却查无此 seq:丢掉的结局被之后的条目掩盖了 |
| `ring_mismatch` | 环里同 seq 记的是另一条(request_id / msg_type 不同),或另一个结局 |

- 「环该覆盖的范围」:环不满 32 条(从没裁剪过)时是全部 seq;环满 32 条时是不早于环头的 seq。更早的已被挤出环,无从核对,不报。
- SETTLED_UNKNOWN 只比水位,不比环。
- 以前只比水位,「第一条处置的落盘丢了、之后的条目又把水位推过去」这种丢失看不出来;现在按环内空洞抓得到。

**读数的语义**(评审 L11):`player_inbox_reconcile_inconsistent_rows` 是「当前不一致行数」gauge,不是计数器。
- 它是本任期 64 片各自最近一次读数之和:读全的分片用本轮读数覆盖;只读了一部分的取 max(旧值, 本轮);没读到的保留旧值。
- 只由领导者产出,卸任时归零。新领导者从 0 重建,截断持续时最多 16 轮才读全,这期间偏低;告警用 `max_over_time[2h]` 跨过易主空档。
- **处置之后告警不会马上消失**:同一行只要还在窗口里,每轮都会再报。要等那一行滑出 24 小时窗口、所在分片再被读完一次,
  再过约 2 小时,告警才消失。确认处置完,在 Alertmanager 静默。没有豁免表。

**处置**:
1. 从收件箱 ERROR 日志 `Reconcile mismatch`(带 player_id、domain、seq、request_id、gap)取出玩家清单。
2. 逐个 `ReconcilePlayer` 复核(§10.3)。
3. `FINAL_NOT_IN_LEDGER` 多半是回档,或 Redis 丢了最后一段(AOF 丢尾),处置效果已随数据回退。
   对照 scene 的 `[PlayerInbox] applied` 日志与玩家存档核实,仍要执行就用新 `request_id` 重下。

### 10.3 单人复核 `ReconcilePlayer`

`ReconcilePlayer(player_id[, domain])` 只读。返回各域的账本水位与收件箱 `next_seq`,以及有差异的行:
- `FINAL_NOT_IN_LEDGER`:收件箱已由消费方终结,账本里没有它的结局。§10.2 的三种缺口都报这一个值,具体是哪种只在定时对账日志的 `gap=` 里。
- `PENDING_BUT_DECIDED`:收件箱仍 PENDING,账本水位已过这个 seq。有两种:
  - 确认丢了、玩家一直离线:环里同 seq 就是这一条。玩家下次登录会补确认;长期离线的不用处理。
  - MISMATCH:环里同 seq 是另一个 request_id,scene 每次落地都停用该域。按 §5.2 B 强制终结。
  - 分不清时,先看 scene 日志 `metric=inbox_replay_mismatch`。强制模式对第一种会以 `REQUEST_IN_LEDGER` 拒绝,不会误伤。
- `ledger_found=false`:该玩家在 zone Redis 里没有存档 blob(从未进场或 Redis 丢数据),结论为 unknown,不比对。
- 回 `kPlayerInboxStoreUnavailable`:除了库或 data_service 故障(可重试),还可能是**账本结构不合法**。
  - 后者收件箱同时打 ERROR `ReconcilePlayer failed`,错误里带 `persisted inbox ledger of player=<id> is structurally invalid`;
  - 重试不会好,按 §10.4 处理。回包里没有专门的字段区分两者,看日志。
- 一名玩家最多读 5000 行;超出时打 ERROR `ReconcilePlayer read only the first 5000 rows`,结果不完整。

### 10.4 账本损坏(`metric=inbox_ledger_invalid` / `metric=inbox_registry_broken`)

两种停用的来源不同,处置完全不同。

**`inbox_registry_broken`:节点级,构建缺陷。**
- scene 启动时自检处置注册表,不过就本节点整体停用消费,`detail=` 写明哪一项不过。启动时一条 ERROR,之后每 5 分钟补打。
- 节点上所有在线玩家的处置都不执行;条目留在收件箱 PENDING、不投递,数据没有问题。
- 处置:回滚到上一版 scene,或修好再发。**不要动任何玩家的账本**。玩家落到正常的节点后,照常拉取。

**`inbox_ledger_invalid`:玩家级,数据问题。**
- 信号:
  - scene `source=loader` ERROR:每次加载一条,带 `reason=`;
  - scene `source=apply_gate` WARN:在线期间首次延后一条,之后每 8 次退避一条;
  - 收件箱侧 `PlayerInboxReconcileLedgerInvalid`,以及 ERROR `persisted inbox ledger is structurally invalid`;
  - `ReconcilePlayer` 回 `kPlayerInboxStoreUnavailable`;强制作废回 `LEDGER_INVALID`。
- 不合法的条件(scene 的 `ValidateLedger` 与收件箱同口径):domain 为 0 或重复;单域环超过 32 条;环里 seq 为 0、不严格升序或越过水位;
  结局不是 APPLIED / REJECTED / SKIPPED。
- 收件箱另查 request_id 为 0,scene 不查。只有这一项不合法时,两侧表现不同:
  - 收件箱侧照样计 invalid、亮 `PlayerInboxReconcileLedgerInvalid`,`ReconcilePlayer` 回 `kPlayerInboxStoreUnavailable`,强制作废回 `LEDGER_INVALID`;
  - scene 侧不打 `metric=inbox_ledger_invalid`,照常拉取。同 seq 的行再投递时,环里记的 request_id(0)对不上,打 `metric=inbox_replay_mismatch` 并停用该域。
  - 所以收件箱侧亮了而 scene 侧找不到 `inbox_ledger_invalid` 时,去找该玩家的 `inbox_replay_mismatch`。
- 影响:这名玩家的收件箱**全部域**停用,不拉取、不执行,表面上与离线无异;登录不受影响。scene 不改写这份账本,存盘时原样写回。
- **不要清空账本重建**,也不要把水位改小。账本是「哪些处置已经在这名玩家身上执行过」的唯一记录;
  清空之后,已执行、还没确认的 PENDING 行会被当成新条目,再执行一次。
- 也不要用作废绕开:普通 `VoidItem` 只挂指令,scene 不拉取就没人裁决;强制模式会以 `LEDGER_INVALID` 拒绝。

步骤:
1. 从 scene 日志取 `player_id` 与 `reason=`。
2. 原样留存账本,写进工单。data_service `GetPlayerInboxLedger` 不做校验,原样返回存档里的账本;也可按 `docs/ops/online-debug-data-fetch.md`
   取该玩家的 `PlayerAllData` blob。MySQL `player_database.inbox_ledger` 可能更旧,一并记下。
3. 收件箱一侧导出这名玩家:`GetSummary`(各域 `next_seq` / `open_count` / `fence_epoch`)与 `ListItems`(各域、全部状态)。
   `ReconcilePlayer` 在账本修好之前一律回 `kPlayerInboxStoreUnavailable`,不要重试它。
4. 对照 scene 的 `[PlayerInbox] applied` 日志,逐个 seq 判断哪些已在玩家身上生效。
5. v1 没有修复工具,修复与写回的方式由负责人定。修正后的账本至少要过上面的结构校验(含收件箱多查的 request_id 非 0),各域水位不低于原值。
   水位两个方向的代价不对称:偏高,个别没执行过的条目会以 SETTLED_UNKNOWN 收尾(有告警,可以人工重下);偏低,已执行过的会再执行一次。
6. 写回之后让玩家重新登录。确认 `metric=inbox_ledger_invalid` 不再出现(只是 request_id 为 0 的那种,看该玩家的 `metric=inbox_replay_mismatch`
   不再出现),再用 `ReconcilePlayer` 复核:不再回 `kPlayerInboxStoreUnavailable`,且无差异。

---

## 11. 回滚下限与破坏性降级(设计 §8 #21、§11.7)

- **回滚下限** = 认识 `inbox_ledger` 的 scene 与 go/db 版本(上线第 4 步那一版)。回退到下限以上任意**镜像**都安全:`kubectl rollout undo`,
  或用当前检出的脚本配旧 `-NodeImage` / `-GoSvcTag` 重跑。**开消费之后不要检出旧 commit 重跑它的部署**:开关会跟着回退成那个 commit 的值
  (下限那一版就是 `false`),消费被关掉而线上白名单仍开着(§2「回滚」)。确实要跨过「开消费」那个提交、或回到 H1 修复之前的 commit,先按下面第 1 步关入箱。
- **低于下限的降级**(scene 或 go/db 回到不认识新字段的版本)会丢账本或让账本停在旧值,再升回来时已应用、未确认的处置会被**重复执行**
  (`ATTACH_DEBT` 解禁之后尤其危险:同一笔欠款会挂两次)。必须先做:
  1. **关入箱**:把服务 yaml 的 `EnqueueTypes` 改回 `[]` 并提交,重跑 `infra-up`(确认 `GmSanctionPlayer` 返回 `kPlayerInboxDisabled`)。
  2. **关消费**:把 `bin/etc/game_config.yaml` 的 `player_inbox.consumer_enabled` 改回 `false` 并提交,全部 zone `zone-up` 并滚动 scene;
     确认 `player_inbox_fetch_total` 不再增长、scene 打出 `metric=inbox_consumer_disabled`。顺序不能反:第 1 步要以 `GmSanctionPlayer` 回
     `kPlayerInboxDisabled` 为准再做第 2 步。反了时,非 DryRun 的写前门禁读到 infra 线上白名单仍非空会拒绝 `zone-up`(§2.1 第 2 层);
     但它看的是 ConfigMap,player-inbox 的 Pod 没滚动时看不出来,所以不能只靠它。
  3. **隔离已投递未确认的行**:对每个域 `ListItems(state=PENDING)`,把 `delivery_count > 0` 的行逐条 `VoidItem(reason=QUARANTINED, note=<降级工单号>)`,
     按 §4.2 的限速执行,结果清单附进工单。消费关着,这些行这时只挂上指令、仍是 PENDING。重开消费后,账本已丢失的玩家,scene 把这些行当新条目记 SKIPPED,
     不会再执行;账本还在的,照报原结局。
  4. 执行降级。
  5. **升级回来之后**:先回到第 4 步的版本(`consumer_enabled=false`),逐条按第 3 步清单对照玩家存档核实,确认无误后再按 §2 的第 5、6 步重新打开。
     重开之后用 `LookupByRequest` 抽查第 3 步清单的终态(§6 第 4 步的表)。
- 降级窗口里收件箱服务本身可以照常运行(只是没有生产也没有消费)。

---

## 12. TiDB 索引预切(设计 §4.2,D8)

`PRE_SPLIT_REGIONS` 只预切**行数据**;主键(非聚簇时就是一个唯一索引)、唯一键与二级索引建表时各只有 1 个 Region,
写入尾部(按 64 个 shard 打散)在首日仍压在同一个 Region 上。player_route 为此有 `route_tool tidb-prepare`,但那是 data_service 自己的工具
(读 data_service 的配置、只认它的两张表),**收件箱没有现成的命令**;这里给手工 SQL,MySQL 上**不要执行**(MySQL 没有 Region)。

何时做:TiDB 上建表之后、开服之前(低峰);BR / PITR 恢复之后、TRUNCATE 之后重做一次。

```sql
-- 0) 先核对索引名(proto2mysql 命名:普通索引 idx_<表名>_<序号>,唯一键 uk_<表名>;以 SHOW INDEX 的实际结果为准)
SHOW INDEX FROM mmorpg_player_inbox.player_inbox_stream;
SHOW INDEX FROM mmorpg_player_inbox.player_inbox_item;
SHOW INDEX FROM mmorpg_player_inbox.player_inbox_audit;

-- 1) 预切出来的空 Region 不许被合并回去(与 route_tool tidb-prepare 同一做法)
ALTER TABLE mmorpg_player_inbox.player_inbox_stream ATTRIBUTES 'merge_option=deny';
ALTER TABLE mmorpg_player_inbox.player_inbox_item   ATTRIBUTES 'merge_option=deny';
ALTER TABLE mmorpg_player_inbox.player_inbox_audit  ATTRIBUTES 'merge_option=deny';

-- 2) 每条索引按 shard(0..63)每 4 片切一刀 → 16 段。所有键都以 shard 开头,切点只给 shard,其余列补 0。
--    stream:PRIMARY(shard, player_id, domain)、idx_player_inbox_stream_1(shard, notify_due_ms)
SPLIT TABLE mmorpg_player_inbox.player_inbox_stream INDEX `PRIMARY`
  BY (4,0,0),(8,0,0),(12,0,0),(16,0,0),(20,0,0),(24,0,0),(28,0,0),(32,0,0),(36,0,0),(40,0,0),(44,0,0),(48,0,0),(52,0,0),(56,0,0),(60,0,0);
SPLIT TABLE mmorpg_player_inbox.player_inbox_stream INDEX `idx_player_inbox_stream_1`
  BY (4,0),(8,0),(12,0),(16,0),(20,0),(24,0),(28,0),(32,0),(36,0),(40,0),(44,0),(48,0),(52,0),(56,0),(60,0);
--    item:PRIMARY(shard, player_id, domain, seq)、uk_player_inbox_item(shard, player_id, producer_kind, request_id)、
--          idx_player_inbox_item_1(shard, state, updated_ms)
SPLIT TABLE mmorpg_player_inbox.player_inbox_item INDEX `PRIMARY`
  BY (4,0,0,0),(8,0,0,0),(12,0,0,0),(16,0,0,0),(20,0,0,0),(24,0,0,0),(28,0,0,0),(32,0,0,0),(36,0,0,0),(40,0,0,0),(44,0,0,0),(48,0,0,0),(52,0,0,0),(56,0,0,0),(60,0,0,0);
SPLIT TABLE mmorpg_player_inbox.player_inbox_item INDEX `uk_player_inbox_item`
  BY (4,0,0,0),(8,0,0,0),(12,0,0,0),(16,0,0,0),(20,0,0,0),(24,0,0,0),(28,0,0,0),(32,0,0,0),(36,0,0,0),(40,0,0,0),(44,0,0,0),(48,0,0,0),(52,0,0,0),(56,0,0,0),(60,0,0,0);
SPLIT TABLE mmorpg_player_inbox.player_inbox_item INDEX `idx_player_inbox_item_1`
  BY (4,0,0),(8,0,0),(12,0,0),(16,0,0),(20,0,0),(24,0,0),(28,0,0),(32,0,0),(36,0,0),(40,0,0),(44,0,0),(48,0,0),(52,0,0),(56,0,0),(60,0,0);
--    audit:PRIMARY(shard, player_id, domain, seq, action)
SPLIT TABLE mmorpg_player_inbox.player_inbox_audit INDEX `PRIMARY`
  BY (4,0,0,0,0),(8,0,0,0,0),(12,0,0,0,0),(16,0,0,0,0),(20,0,0,0,0),(24,0,0,0,0),(28,0,0,0,0),(32,0,0,0,0),(36,0,0,0,0),(40,0,0,0,0),(44,0,0,0,0),(48,0,0,0,0),(52,0,0,0,0),(56,0,0,0,0),(60,0,0,0,0);

-- 3) 核对:每条索引应有约 16 个 Region
SHOW TABLE mmorpg_player_inbox.player_inbox_item INDEX `uk_player_inbox_item` REGIONS;
```

- 切点密度(每 4 片一段)是按 GM 域「写入量小、读多」取的,未压测;落地拉取的快路径是流行的主键点读,压力主要在 `player_inbox_stream` 的 `PRIMARY`。
  指标显示热点时再加密切点。
- 行片模数 64 与 `placement.RouteRowShards` 同值但**常量独立**(设计 §4.2),这里的切点按 64 写。改模数 = 新建表,不在这里改。
- 以后若给 go/player_inbox 加 `tidb-prepare` 子命令(照 route_tool,MySQL 上自动跳过),本节改为调用它。

---

## 13. 合服

收件箱三张表按全局唯一的 player_id 存,**没有 zone 列,合服不搬不改**。`tools/merge_zone` 的合服前审计多了 `player_inbox` 一行:
- 源区或目标区 zone 库缺 `player_database.inbox_ledger` 列 → **block**:先对那个 zone 跑 go/db `cmd/migrate up`。否则拷贝玩家行时会以 SCHEMA MISMATCH 停在维护窗口里。
- 收件箱库 / 表查不到 → warn「NOT VERIFIED」(该环境多半没部署收件箱,合服本身不受影响);其余查询失败 → INFRA。
- 正常时 info,报 PENDING 条数。

---

## 14. 已知未完成 / 待确认

- **`go/db/cmd/epoch_raise`** 已定、未落码,由围栏那条线做;收件箱这一来源由工具自己经 `GetSummary` 读(围栏设计 v0.4 §9.1、§14 第 5、20 条;§7)。
- **`GM_ATTACH_CURRENCY_DEBT` v1 不开**:合并语义、撤销类型(按金额减少)、单笔上限与只读欠款查询要一起做完(交接 §6 M2 / M3)。
- **账本损坏没有修复工具**(§10.4);`ReconcilePlayer` 遇到不合法账本只回 `kPlayerInboxStoreUnavailable`,回包没有 `ledger_invalid` 字段。
- **同 seq、同 request_id、只有 msg_type 不同的 MISMATCH**:强制模式按 fail-closed 拒绝(`REQUEST_IN_LEDGER`),只可能来自数据损坏,没有工具,人工上报。
- **确认依据仍是「Redis 已落盘」**:资产通道已改为 SQL 提交证据,收件箱还没有(交接 §6 M7,设计 §7.10 登记为残余风险)。
  强制终结的证明同样依赖已落盘的 Redis 账本。
- **审计对不到人**(§3):要靠 kube-apiserver 的 `pods/portforward` 审计日志;由凭据推出 operator 未做。
- **回档预检自动化**随单人回档栅栏接线同批做(§9)。
- **TiDB 索引预切**只有手工 SQL(§12);收件箱库迁到 TiDB 后的 BR / PITR 未纳入备份。
- **robot 的 GM 处置冒烟场景**(设计 §11.6)未做。
- **scene 日志告警**只是值班固定查询:Loki ruler 还没挂规则目录(§5)。
- 告警文件里的指标名由 metrics 包的 `TestAlertRulesReferenceRegisteredMetrics` 核对;**label 取值没有守卫**,
  `enqueue_total` / `fetch_total` / `void_total` 的取值改名不会红,改的时候对照告警文件头的「label 取值契约」。
- 批量处置脚本的限速值(§4.2)待压测确认。
- K8s 上的抓取前提(指向 9270 的 ServiceMonitor / PodMonitor)仓库里没有,与其它 go-svc 同一缺口。
- **写前门禁的线上核对**(§2.1 第 2 层)看的是 ConfigMap 不是进程;`k8s_zone_rollback.ps1` 第 0 步不读集群,这一层到第 6 步才判。
  在第 0 步预读(照 db 消费组第 0 步 e 的做法)未做,属于回档脚本那条线。
- **还没同步的文档与配置**(本手册之外,不在收件箱这批的改动范围):
  - `docs/ops/release-checklist.md` 回滚一节的 gateway 例外仍写「改用旧 commit 重跑 `k8s_deploy.ps1 zone-up`」,开消费之后照做会连消费一起关掉(§2「回滚」);
    同一节「部署上一版本」用当前脚本换旧镜像参数,不受影响;
  - `deploy/k8s/asset-channel-alerts.yaml`、`deploy/k8s/data-service-audit-alerts.yaml` 的 LogQL 也写 `{app="scene"}` / `{app="battle"}`,同样恒为空;
  - `cpp/libs/engine/config/config.cpp` `readGameConfig` 的注释仍说 K8s node-config 不带 `player_inbox` 段,与现在的整段复制相反。

---

## 15. 验证(交 Codex,未运行)

在仓库根目录:

```powershell
# 部署脚本契约(含连接总账、deadline 门禁、ConfigMap 形状、口令注入、迁移门禁、本地启动契约)
pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_migrate_gate.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/start_game_command_contract.tests.ps1

# data_service 的 GetPlayerInboxLedger 与资产账本(共用读路径)
cd go/data_service; go vet ./... ; go test ./internal/logic/... ./internal/server/... -count=1

# 合服审计
cd tools/merge_zone; go vet ./... ; go test -run 'PlayerInbox|ZoneDBName|RunAuditMode' -count=1 .

# 收件箱服务(含 TestAlertRulesReferenceRegisteredMetrics:读 deploy/k8s/player-inbox-alerts.yaml 核对指标名;
# TestLedgerRecentCapacityMatchesConsumer:读 C++ 头文件核对环容量 32。两者都要在完整仓库检出下跑)
cd go/player_inbox; gofmt -l . ; go vet ./... ; go test ./... -count=1
```

C++ 侧(MSBuild 串行 `/m:1`):`currency_test.exe --gtest_filter=PlayerInboxRules.*:PlayerInboxSystemTest.*`,覆盖 M1 / L1 / L2 / M4 / H1 的状态机回归。

通过标准:三个 `.tests.ps1` 的汇总 fail=0;Go 测试全绿;C++ 用例全绿。部署脚本的用例按 git 里的 `go/player_inbox/etc/player_inbox.yaml`
与 `bin/etc/game_config.yaml` 核对 §1 列出的键、`Middlewares` 段与消费开关。`k8s_deploy_contract.tests.ps1` 里与本手册直接相关的几条:
写前门禁的三层(两份 yaml、线上交叉核对用假 kubectl、来源用注入的 git 状态)、DryRun 不读集群、告警文件 LogQL 选择器只用 sidecar 的流标签。
线上交叉核对在真集群上的行为(跨 namespace 列 ConfigMap 的权限、kubectl 输出形状)没有自动化验证,首次在 staging 跑 `infra-up` / `zone-up` 时看门禁行的 `live=`。各批的完整验证步骤见交接 §4 与 §6.4。

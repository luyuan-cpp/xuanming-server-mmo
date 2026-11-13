# MySQL Backup & PITR Runbook

> **状态**: v1 — 2026-05-15
> **范围**: K8s 部署的 MySQL(`deploy/k8s/manifests/infra/mysql.yaml` + `mysql-backup-cronjob.yaml`)的备份、归档、恢复 SOP。
> **灾难恢复从 §4.0 开始看。** 它覆盖 TiDB 和 MySQL 两档,文件名是历史原因。
> **关联文档**: `docs/design/server_merge_design.md` §5.1(合服前备份要求)。

---

## 1. 资产清单

| 资源 | 用途 | 关键路径 |
|---|---|---|
| `mysql-data-pvc`(20Gi RWO)| MySQL 数据 + binlog | `/var/lib/mysql/{data,binlog}` |
| `mysql-backup-pvc`(50Gi RWX)| 备份归档目标 | `/backup/{dump,binlog}` |
| `mysql-config` ConfigMap | 启用 binlog 的 my.cnf | mounted at `/etc/mysql/conf.d` |
| `mysql-backup` CronJob | 每天 03:17 UTC 跑一次 | `kubectl get cronjob mysql-backup` |

**保留策略**:dumps 30 天,binlog 14 天。改 `mysql-backup-cronjob.yaml` 的 `find -mtime` 数字调整。

---

## 2. 首次部署 / 升级现有集群

### 2.1 全新集群(2026-10-15 按当前部署入口修订)

先按 [K8s 开服手册](k8s-open-server-runbook_zh.md) 准备目标 context、kubeconfig、区服清单、不可变镜像和该环境的密钥注入。下面是只准备共享基础设施的示例:跳过全局 Go 服务并不代表完整游戏栈已经部署,`BattleReplicas=0` 也不会删除已有 battle 池。

当前 `mysql.yaml` 挂载 `mysql-init-sql` ConfigMap,它包含区服库与全局库初始化 SQL,由 `k8s_deploy.ps1 infra-up` 在 MySQL Deployment 之前生成。不能再只对 `mysql.yaml` 单独 apply。

**备份任务的凭据须先准备:** 仓库 `mysql-backup-cronjob.yaml` 的 `MYSQL_PASSWORD` 仍是开发环境的固定值,不会自动跟随 prod 注入或密码轮换。运维必须先在目标 namespace 创建匹配该数据库的凭据 Secret,准备 Git 外的审定清单副本,把 CronJob 的 `MYSQL_PASSWORD` 改为对应 `secretKeyRef` 并核对 `MYSQL_HOST / MYSQL_USER`、权限与 PVC 挂载。下方只 apply 这份已准备的清单,不能直接把仓库开发清单当成 prod 备份任务。

```bash
# 在仓库根目录;替换路径与镜像占位,密钥按开服手册从受控环境注入。
KUBE_CONTEXT='<目标 context>'
KUBE_CONFIG='<kubeconfig 绝对路径>'
INFRA_NS='mmorpg-infra'
NODE_IMAGE='<镜像仓库>/mmorpg-node:<不可变 tag>'
BACKUP_MANIFEST='<Git 外审定的 mysql-backup CronJob 清单绝对路径>'
pwsh -File tools/scripts/k8s_deploy.ps1 -Command infra-up \
  -KubeContext "$KUBE_CONTEXT" -KubeConfig "$KUBE_CONFIG" \
  -InfraNamespace "$INFRA_NS" -ReleaseProfile prod -OpsProfile managed-cloud \
  -ZonesConfigPath deploy/k8s/zones.ops-recommended.yaml -NodeImage "$NODE_IMAGE" \
  -BattleReplicas 0 -SkipGoSvc -WaitReady

# infra-up 不包含备份 CronJob;在同一 namespace 单独安装它。
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  get configmap mysql-init-sql
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  apply -f "$BACKUP_MANIFEST"

# 验证 binlog 已开
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  exec deploy/mysql -- sh -ec \
  'export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:?容器未注入 root 凭据}"; exec mysql -uroot "$@"' \
  mysql-client -e "SHOW VARIABLES LIKE 'log_bin';"
# 期望:Value=ON

# 手动触发一次备份验证 CronJob 工作
BACKUP_JOB="mysql-backup-smoke-$(date +%s)"
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  create job --from=cronjob/mysql-backup "$BACKUP_JOB"
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  logs "job/$BACKUP_JOB" -f
```

以下 Bash 示例沿用本节目标变量,命令均显式指定 context、kubeconfig 和 namespace。SQL 客户端在容器内读取 `MYSQL_ROOT_PASSWORD`,不把口令写进宿主命令、参数或日志;运行时凭据必须与数据库当前口令一致,轮换后须先同步环境 / Secret。容器不提供该变量时,交互检查可使用 `kubectl exec -it ... -- mysql -uroot -p` 手工输入,不要把口令拼进 `-p` 后。首次安装还须验证备份 PVC 可挂载并完成一次恢复检查。

`infra-up` 会处理共享 etcd / Redis / Kafka 等资源,并非只安装 MySQL。存量集群执行前必须先审查对应拓扑与迁移差异;`mysql-init-sql` 只在空数据目录首次启动时执行,不会自动修复存量库。

### 2.2 从老版本(emptyDir + 无 binlog)升级 — 数据迁移

```bash
# 1. 先 mysqldump 老数据到本地(不能丢)
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  exec deploy/mysql -- sh -ec \
  'export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:?容器未注入 root 凭据}"; exec mysqldump -uroot "$@"' \
  mysqldump-client --all-databases --single-transaction > pre-upgrade-dump.sql

# 2. 删老 Deployment(emptyDir 数据随 pod 销毁)
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  delete deployment mysql

# 3. 按上节同一参数调用 infra-up,先生成 mysql-init-sql,再创建 PVC 与 MySQL。
#    删除旧 emptyDir 实例前,必须已经验证步骤 1 的备份可恢复;
#    共享基础设施的其他变更也须已审查,不要只复制一条 apply mysql.yaml。
pwsh -File tools/scripts/k8s_deploy.ps1 -Command infra-up \
  -KubeContext "$KUBE_CONTEXT" -KubeConfig "$KUBE_CONFIG" \
  -InfraNamespace "$INFRA_NS" -ReleaseProfile prod -OpsProfile managed-cloud \
  -ZonesConfigPath deploy/k8s/zones.ops-recommended.yaml -NodeImage "$NODE_IMAGE" \
  -BattleReplicas 0 -SkipGoSvc -WaitReady

# 4. 等新 pod ready
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  rollout status deploy/mysql

# 5. 恢复数据
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  exec -i deploy/mysql -- sh -ec \
  'export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:?容器未注入 root 凭据}"; exec mysql -uroot' \
  < pre-upgrade-dump.sql

# 6. apply CronJob
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  apply -f "$BACKUP_MANIFEST"
```

---

## 3. 日常运维

### 3.1 检查备份是否在跑

```bash
# CronJob 上次成功时间
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  get cronjob mysql-backup -o jsonpath='{.status.lastSuccessfulTime}'

# 最近 7 天的 Job 历史
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  get jobs --sort-by=.metadata.creationTimestamp

# 备份卷里的实际文件
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  run -it --rm peek --image=busybox:1.36 --restart=Never \
  --overrides='{"spec":{"volumes":[{"name":"b","persistentVolumeClaim":{"claimName":"mysql-backup-pvc"}}],"containers":[{"name":"peek","image":"busybox:1.36","stdin":true,"tty":true,"volumeMounts":[{"name":"b","mountPath":"/backup"}]}]}}' \
  -- sh -c 'ls -lh /backup/dump /backup/binlog'
```

### 3.2 手动触发备份(搬库、改表结构等高风险操作前 / 灾难前)

```bash
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  create job --from=cronjob/mysql-backup \
  mysql-backup-pre-$(date +%Y%m%d-%H%M)
```

### 3.3 取回备份文件到本地

```bash
# 用临时 pod 把文件 cp 出来
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  run dumpfetch --image=busybox:1.36 --restart=Never \
  --overrides='{"spec":{"volumes":[{"name":"b","persistentVolumeClaim":{"claimName":"mysql-backup-pvc"}}],"containers":[{"name":"c","image":"busybox:1.36","command":["sleep","3600"],"volumeMounts":[{"name":"b","mountPath":"/backup"}]}]}}'

kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  cp dumpfetch:/backup/dump/dump-20260515-0317.sql.gz ./dump-20260515.sql.gz
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  delete pod dumpfetch
```

---

## 4. 灾难恢复 / PITR(Point-in-Time Recovery)

### 4.0 恢复的整体做法(2026-10-31,未演练)

回档就是回到目标时刻 T 库里已经提交的状态,T 之后的写全部丢弃(负责人 2026-10-17 定)。玩家存档只在数据库里。回档 RPC 和单 zone 回档脚本都已删除,灾难恢复只走本节。

**默认整个地域一起回档**,所有逻辑库回到同一个 T,这样跨库的资产、目录和存档才对得上。只回一个库是例外,要按 §4.1a 处理并人工核账。

1. **挑 T。** T 不能落在搬库或排空的冻结窗口里,否则恢复出来的闸门是关的,写者全部被挡。
2. **先记下部署参数,再停写。** 部署参数不粘滞,namespace 删掉之后 `k8s_deploy.ps1` 没有现状可比,防误关和形态核对都不会触发,漏传的参数会静默落回默认值。所以先从发布记录取出上一次部署的完整命令,对照线上核实,至少包括 `-ReleaseProfile`、`-Region`、`-DirectoryLogicalShards`、`-PlayerStoreShards`、`-SqlClusters`、`-GateRouterMode`、`-ClientEntryMode` 和入口地址、`-RedisSplit`、`-PlayerRedisMasters`、`-NodeOpenFileLimitMode`、`-Login*`。然后逐个 zone 跑 `dev_tools.ps1 -Command k8s-zone-down -ZoneName <zone>`,再用 `kubectl get namespace` 确认已删除。这个命令失败时不报错,不能只看退出码。单独部署的全局服务也要停。
3. **恢复。** TiDB 档按 §4.1b,MySQL 档按 §4.1。先恢复到影子库或新集群核对,原库保留到开流量之后。T 之后搬过库的,恢复到映射现在指向的集群,但备份要用 T 时刻那个集群的。
4. **核对闸门。** 每个逻辑库执行 `SELECT writable,HEX(freeze_run_id) FROM route_meta`,结果应该是可写、没有冻结标记。
   **再核对合服映射**:对目录第 0 片跑 `pwsh -File tools/scripts/dev_tools.ps1 -Command merge-zone-audit -MergeZoneRouteDbDsn <第 0 片 DSN>`,退出码应为 0:映射能解析,而且每一行映射在 `zone_merge_history` 里都有对应的合服记录(两张表同库、同一个事务写,回到同一个 T 之后仍然成对)。退出码 1 说明恢复出来的映射表有坏行或来历不明的行,按[不停服合服手册](merge-zone-runbook.md) §5 处理之后再起服。
5. **跳过 `match-results` 的积压。** T 之后的对局结果和数据库一起丢弃。起服之前把 `match-rating`、`guild-trial` 两个消费组的位点都重置到最新:

   ```powershell
   pwsh -File tools/scripts/dev_tools.ps1 -Command kafka-offset-reset -KafkaTopic match-results -KafkaGroup match-rating -KafkaToLatest
   ```

   ```powershell
   pwsh -File tools/scripts/dev_tools.ps1 -Command kafka-offset-reset -KafkaTopic match-results -KafkaGroup guild-trial -KafkaToLatest
   ```

   不能重置到 T,也不能留着积压:T 之后才开的历练,开局记录已经不在库里,消费者会一直重试这条结果,堵住整个分区。T 时刻还在打的对局走正常的崩溃路径,到期判过期,不发奖。
6. **起服。** 照传第 2 步记下的全部参数。恢复到了新集群的话,只把 `-SqlClusters` 里这个集群的地址改过去。
7. **开流量前**用测试账号把建角、登录、存盘、领奖各走一遍,确认消费组没有卡住。

**这些不用回退,也不用清:**

| 东西 | 为什么 |
|---|---|
| 发号器 | 计数器在两套 etcd 里,不随数据库回退,只增不减。回档之后发的号不会和 T 之后发过的撞 |
| Redis | 没有玩家存档。同步摘要只是提示,回档了只会让客户端多拉一次。`owner_epoch` 铸造取「当前值 + 1」和墙钟的较大者,比回档后的写权行大。会话、位置、战斗锁靠 TTL 或判死收敛 |
| 映射 `/mmorpg/db_placement/*` | 在 etcd 里,描述的是库在哪,不是库里有什么 |
| `gate-cmd`、`scene-cmd` | 命令带目标实例号,重启后的新实例不接旧命令 |
| `transaction_log_topic_g<N>`、`player_snapshot_topic_g<N>` | 只是审计和归档,不影响玩家状态。T 之后的记录照常落库,留作痕迹 |

### 4.1 完整时间点恢复(从 dump + binlog)

**场景**:数据被误删 / Bug 导致脏写,需要恢复到 `2026-05-15 14:23:00 UTC` 的状态。

```bash
# 1. 找最近的 full dump(必须 ≤ 目标时间)
ls -la /backup/dump/dump-*.sql.gz
# 比如最近的是 dump-20260515-0317.sql.gz(03:17 UTC,目标 14:23 之前 ✓)

# 2. 找该 dump 之后的所有 binlog(从 dump 头部读 master coords)
zcat dump-20260515-0317.sql.gz | head -50 | grep -i "MASTER_LOG_FILE\|MASTER_LOG_POS"
# 期望输出类似:
#   -- CHANGE MASTER TO MASTER_LOG_FILE='mysql-bin.000042', MASTER_LOG_POS=1234567;

# 3. 在已准备的隔离 namespace 起影子 MySQL,使用运维审定的清单。
#    清单须包含可恢复的数据卷,并从该 namespace 的 Secret 注入 MYSQL_ROOT_PASSWORD;
#    这里不在 kubectl 参数中传口令,也不复用生产 Pod / PVC。
SHADOW_NS='<隔离恢复 namespace>'
SHADOW_MANIFEST='<Git 外审定的影子 MySQL 清单绝对路径>'
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$SHADOW_NS" \
  apply -f "$SHADOW_MANIFEST"
# 清单中的影子 Pod 名约定为 mysql-shadow,先确认其 Ready 再灌数据。
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$SHADOW_NS" \
  wait --for=condition=Ready --timeout=5m pod/mysql-shadow

# 4. 灌 full dump
zcat dump-20260515-0317.sql.gz | \
  kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$SHADOW_NS" \
    exec -i mysql-shadow -- sh -ec \
    'export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:?影子容器未注入 root 凭据}"; exec mysql -uroot'

# 5. 重放 binlog 到目标时间(从 dump 记录的位置开始)
for f in mysql-bin.000042 mysql-bin.000043 mysql-bin.000044; do
  cat /backup/binlog/$f
done | mysqlbinlog \
  --start-position=1234567 \
  --stop-datetime="2026-05-15 14:23:00" \
  - | kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$SHADOW_NS" \
    exec -i mysql-shadow -- sh -ec \
    'export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:?影子容器未注入 root 凭据}"; exec mysql -uroot'

# 6. 验证影子库数据合理(查关键表)
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$SHADOW_NS" \
  exec mysql-shadow -- sh -ec \
  'export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:?影子容器未注入 root 凭据}"; exec mysql -uroot "$@"' \
  mysql-client -e "SELECT COUNT(DISTINCT player_id) FROM player_store_1001000_db.player_store;"
# player_store_1001000_db 仅为示例(地域 1 的 0 号存档库),必须替换为本次恢复计划中的真实业务库。

# 7a. 若仅是抽取单个玩家:从影子库 dump 该玩家行 → 导回生产
# 7b. 若是全库回档:停生产 mysql,把影子 PVC 推上去。之后接着做 §4.0 的第 4~7 步。
#     发号器的计数器在 etcd 里,不随数据库回退,这里不用处理发号(§4.0)。
```

### 4.1a 单库回档和角色护栏(2026-10-31,未演练)

规格见 [角色护栏](../design/horizontal-scale-logical-db.md) §3。目录(`account_role`)是权威,每个参与者库(`mmorpg_guild`、`mmorpg_trade`、`mmorpg_mail`、`mmorpg_match`、`mmorpg_player_inbox`、`mmorpg_friend`)有一张副本表 `role_fence`,合并规则是**版本大的赢**。只回档其中一个库,版本就对不上了。两个方向处理不同,**都要在开流量之前做完**。

**参与者库回档后:清空本库 `role_fence`。** 回档出来的副本可能比目录旧:目录已经解冻或过户,副本还是冻结或旧主。清空之后下一次访问读权威状态自愈成当前值,不用逐行核对。

```sql
-- 在回档的那个参与者库上执行。目录不用动。
TRUNCATE TABLE role_fence;
```

**目录回档后:把所有 `fence_version` 抬过任何副本可能的值,再开流量。** 回档会让 `fence_version` 倒退,而副本还留着更大的版本。不抬的话目录之后写出来的解冻、过户状态版本比副本小,副本一律不接受,自愈也修不了。

1. 保持停写。逐个参与者库查出 `role_fence` 的最大版本,记下其中最大的一个:

   ```sql
   -- 每个参与者库各跑一次。
   SELECT COALESCE(MAX(version), 0) AS max_version FROM role_fence;
   ```

2. 目录每一片都抬,`<抬升值>` 取「上一步的最大值 + 1000000」,留够余量。**全部片抬同一个值,只增不减**:

   ```sql
   -- K>1 时每个 mmorpg_global_NN 各跑一次。
   UPDATE account_role SET fence_version = GREATEST(fence_version, <抬升值>) + 1;
   ```

3. 目录回档可能把 `prepare_token`、`trade_listing_id` 和收据一起倒回去,投票或冻结状态和回档点一致,不用手工改。等开流量之后 Begin 的重放或后台收尾接着处理:超过 30 秒的 `PREPARING` 会被撤销。
4. 抬完版本的状态比副本都新,副本下一次访问或下一次推送就跟上了,不需要动参与者库。

**过户和回档的关系。** 回档点之后提交的过户,在目录和参与者两边可能只留下一边,这不是护栏能修的:

| 回档的库 | 回档点之后发生过过户或删角 | 处理 |
|---|---|---|
| 参与者库 | 目录还记着新主。回档出来的库里可能有过户之前已经结束、回档后又回到未终局的挂起行(合同、资产指令、票) | 清空 `role_fence` 只能修护栏,修不了这些行。要按手册核账,这批角色的挂起行由人工裁决。不能让它们按旧主继续发资产 |
| 目录 | 目录倒回旧主,参与者库和存档已经按新主写过 | 买卖双方的归属要人工核对,必要时按回档点之后的 `account_role_transfer_receipt` 备份和商城订单重放。抬版本只保证副本能跟上目录,不能证明目录的归属是对的 |

**只回档存档库(`player_store_<id>_db`;zone 存档库 2026-11-11 已退役,总体方案 §7.2 A17c)。** 写权行的代次倒退不用处理,玩家下次认领时代次比库里新。但回档点之后已经提交的资产结果(收件箱、公会、交易的资产指令)在发起方那边已经终局,存档里却没有了,这批玩家要核账。所以能全库一起回就不要只回存档库。

全库一起回档(§4.0)时目录和参与者是同一时刻,版本一致,不用做上面这些。逻辑库分在多个集群时例外,见下一段。

**多集群各自恢复到 T 之后:建角核账(2026-11-05,未演练)。** 每个集群各恢复一次(§4.1b 末段),实际恢复点前后错开。一次建角跨四类库分步写:名字片登记名字 → 玩家片登记路由 → 账号片写 PENDING ref → 存档库写初始行 → 玩家片插 `account_role`(提交点,和路由行同事务核对)→ ref 改 ACTIVE。贴着 T 建的角,`account_role` 在,前面某一步所在的库却恢复到了那一步之前,就留下半截。以 `account_role` 为准(提交点在,前面几步当时都做过),逐名核下面四项(名字未绑定是 2026-11-06 加的,名字行「预留 → 绑定」见 [名字设计](../design/guild-phase2/03-names.md) §3.4a):

| 残留 | 哪个库早了 | 后果 | 判据 | 处理 |
|---|---|---|---|---|
| ref 缺失 | 账号片早于写 ref | 角色列表只按 ref 读,角色从账号里消失,存档还在。它不占名额,账号可能又建满了 | 按 `account_role.account` 到账号片查 `account_role_ref(account, player_id)` | 补一条 ACTIVE ref,和建角最后一步同一条语句,行在且挂着别的过户时不覆盖。补完超过角色上限的照补,输出标 `over_role_cap`;到 ref 硬上限(100)的拒绝 |
| 名字缺失 | 名字片早于登记名字 | 别的账号能再登记同一个名字,一名两角 | 用 `account_role` 里的名字算判重键,到名字片查 `player_name` | 没人登记:按原名重新登记并置为已绑定,和建角同一套登记,幂等。已经登记在别的角色名下:**只报告,两边都不动**,输出占用者、占用者是不是活角色、按「建角早者留名」(路由行 `created_at_ms`)该谁留名。v1 没有改名,也没有强制改名标记可打,由人裁决 |
| 名字未绑定(`name_unbound`) | 名字片早于建角提交之后的绑定,或者那一步当时就失败了 | 名字仍受保护(释放预留态之前要在玩家片证明角色从未提交,它提交了),但没有「只有删角能释放」那一道 | 名字行在本角色名下,`bound_ms=0` | 置为已绑定,和建角提交之后同一步;名字副本缺的一并补上(和名字缺失同属 `-repair name`) |
| 初始存档行缺失 | 存档库早于写初始行 | go/db 认领回 `NO_OWNER`,永远进不了场;建角重放不碰存档 | 按 `player_route.storage_id` 到存档库查 `player_store(player_id, kind=0)` | 这名角色在存档库一行都没有、`initial_player_commit=1`(现行建角流程建的)、`owner_revision=1`(没过过户)、没挂交易或删角:重写建角时那几行初始行。有别的行只缺写权行、旧角色、过过户的、挂着令牌的只报告 |

名字登记表上另有两种,工具也收(2026-11-12,`-target names` 扫名字行,`-target roles` 连墓碑一起扫):

| 残留 | 哪个库早了 / 怎么来的 | 后果 | 判据 | 处理 |
|---|---|---|---|---|
| 删角的名字还登记着(`removed_role_name`) | 名字片早于删角释放 | 名字留在已删的角色名下,小表 `account_role_removal` 那一行已删,后台不会再放它,谁都登记不了 | 墓碑(`account='')里留着原名,按它查到名字行在这名角色名下,或者名字副本还在 | 先复核墓碑,再按 `(player_id, name_norm)` 条件删名字行和名字副本(和删角收尾同一步)。名字副本指着别的名字只报告 |
| 孤儿名字(`orphan_name`) | 建角回滚的释放没做成:玩家片冻结或不可达时证明写不了墓碑;login 判不出结局、来不及补偿 | 名字被一个永远建不成的 id 占着 | 名字行是预留态、登记超过 30 分钟,这个 player_id 没有活角色(没有角色行,或者是不带这个名字的墓碑) | 没有提交证据时同带 token 的 `ReleasePlayerName`:先在玩家片证明永远不会提交(有路由行、没有角色行时写墓碑),再只删预留态。有证据(存档库进过场或存过盘、某个账号上有 ACTIVE 的 ref、落点不在名单里)只报告 |

反方向(`account_role` 不在,名字、路由、ref、初始行在)不在上面的表里;它的名字一项由 `orphan_name` 按证据判。多集群恢复时它有两种来历,**先分清再动**(2026-11-06 评审补):

- 建成了,只是玩家片恢复点早于建角提交。有任何一条已提交的证据就是这种:账号片的 ref 是 ACTIVE(`state=2`,提交点之后才写)、名字行已绑定(`bound_ms≠0`)、存档库有存过盘的行(`save_seq` 大于初始值,或者有初始行以外的 kind)。这时**不要**释放名字:`ReleasePlayerName` 释放预留态之前会在玩家片写墓碑(有路由行时),墓碑不可逆,角色再也补不回来。先看玩家片所在集群能不能恢复到更靠近 T 的点,不能就记工单交负责人定。名字行已绑定时 `ReleasePlayerName` 在证明之前就以 `error_code=38` 拒绝,不写墓碑。
- 没有任何已提交的证据,是 login 当时就放弃了的建角:PENDING ref 由后台判陈旧,初始行没人读得到,名字留着(预留态),按日志带 token 调 `ReleasePlayerName` 释放:它先在玩家片证明这名角色永远不会提交(有路由行就写墓碑),再删预留态的名字行。

后台判陈旧(data_service 一起服就开始扫)对「角色行不在、ref 还是 PENDING、超过 5 分钟」的 ref 写墓碑之前,先看名字行是否已绑定、存档库是否进过场或存过盘、这名角色在某个账号上有没有 ACTIVE 的 ref,路由行、存档库名单、名字登记表缺了算核不全(2026-11-06 起,ACTIVE ref 一条 2026-11-12 起)。有证据或核不全就不写墓碑也不删,把这条 ref 标成保留(`held_ms` 非 0、`held_reason` 是证据),记一次 ERROR `[account-role-ref] stale create ref kept without a tombstone`(带 player 和 evidence)并给 `data_service_account_role_ref_tombstone_held_total` 加一。标了保留的 ref 后台扫描跳过,不再每轮重算、不占名额,只在每轮 8 条的复核名额里轮着重判;还留着多少条看 gauge `data_service_account_role_ref_held`,持续 15 分钟非 0 就报 `DataServiceRoleRefHeldForReview`。按账号片查:`SELECT account,player_id,held_reason FROM account_role_ref WHERE state=1 AND held_ms>0`。人把玩家片恢复到更靠近 T 的点、角色行回来之后,复核轮到它的那一轮自动补 ACTIVE、清掉标记,不用手工摘。ACTIVE 而没有角色行的 ref 本来就只报告不动。它分不出来的只有一种:建成了、从没进过场、提交之后的绑定没做完、ref 也没到 ACTIVE,这种仍会被写墓碑。所以分清反方向残留、以及下面第 1~3 步的 plan 和 apply,仍要在起服之前做完;第 4 步里要等 `ResumeRemovals` 的那几项在起服之后做。

工具是 data_service 的 `cmd/create_residual`,只连 data_service 自己的库(目录各片、名字表、存档库),修名字时另连 data_service 的名字读缓存(`NameCacheRedis`)。维护身份直连,不过写者闸门:每笔写在事务里先锁本库闸门行,冻结的库那一项拒绝(搬库期间不要 apply,见 [搬库手册](db-move-runbook.md) §2)。按片分页扫(角色行按玩家片、名字行按名字片),只做一致读,不锁表。默认只报告;修复要 `-mode apply` 加 `-repair` 选种类,每项修之前重读角色行复核,变了就拒绝,可以重放。名字每修一处都删这名玩家的名字缓存(`player:name:<player>`,正、负缓存同一个键),修完马上查得到。孤儿名字的证明会写墓碑(`account_role` 插一行),维护账号要有这张表的 INSERT 权限;补 ref、补名字、补初始行同样要目录各表、存档库 `player_store` 的写权限。`deploy/sql/authority_operator_role.sql` 只授闸门与写者表,业务表的授权在 apply 之前要先核对(2026-11-12 记为待核,真库用例用的是夹具的维护身份)。

步骤。第 1~3 步在 §4.0 第 4 步核完闸门之后、第 6 步起服之前做(工具直连库,不依赖服务;起服之后后台判陈旧会给反方向残留写墓碑,见上一段),第 4 步起服之后、第 7 步开流量之前做完。单集群恢复也跑一遍 plan,确认是 0:

1. 环境同 `admission_recover`:`MMORPG_SQL_BACKEND`、维护身份 `MMORPG_SQL_OPERATOR_PRINCIPAL`、库映射 `MMORPG_DB_PLACEMENT`(没有映射拒绝运行)。DSN 放进 `CREATE_RESIDUAL_DSN`,库名写目录基名(`SnapshotMySQL.DBName`)。
2. plan:

   ```bash
   # 工作目录 go/data_service。K>1 时可以每片一个进程并行:加 -shard 0 … -shard K-1。默认 -target all:每片先扫角色行(含墓碑),再扫名字行。
   go run ./cmd/create_residual -schema mmorpg_global -logical-shards <K> -storage-ids <全部 storage_id,逗号分隔> -max-roles <AccountDirectory.MaxPlayersPerAccount>
   ```

   每项一行 `finding shard=… player=… kind=… result=…`,要人看的项行末带 `attention=true`;每片每种扫描一行 `shard … target=roles … next_after=… done=…` 或 `target=names … next_after_name="…"`,最后一行 `summary`。`-storage-ids` 要列全,落点不在里面的报 `storage_not_listed`,没核。中途出错时按提示带 `-shard <片> -target roles -after <next_after>` 或 `-shard <片> -target names -after-name "<next_after_name>"` 接着扫。
3. apply,三类一起修(也可以只列其中几类)。带 `name` 时必须给名字读缓存(2026-11-13 起在控制集群上:data_service 配置的 `ControlRedis.Addrs`,逗号分隔;`ControlRedis.Cluster` 为真时再加 `-name-cache-cluster`;K8s 上口令在 Secret `redis-auth`,注入成 `SHARED_REDIS_PASSWORD` 后用 `-name-cache-pass-env` 指过去):

   ```bash
   go run ./cmd/create_residual -mode apply -repair ref,name,initial_rows -schema mmorpg_global -logical-shards <K> -storage-ids <同上> -max-roles <同上> \
     -name-cache-addrs <ControlRedis.Addrs,逗号分隔> [-name-cache-cluster] -name-cache-pass-env SHARED_REDIS_PASSWORD
   ```

4. 再跑一遍 plan,退出码 0 才算干净。apply 那一遍退出码 1 时先看带 `attention=true` 的项:`over_role_cap`(补回 ref 之后这个账号的角色数超过上限:角色是真的,补回比藏着对,超出的名额只挡以后建角,要不要处理由负责人定,记工单)和 `name_cache_not_invalidated`(库里修好了,名字缓存没删掉:`redis-cli -h <NameCacheRedis> DEL player:name:<player>`,或者等负缓存 60 秒、正缓存 24 小时过期)修好了也算要人看,重跑 plan 不会再报它们。剩下的按 `reason` 处理:
   - `name_held_by_other`:`holder_live=false` 是占用者没有活角色。它的登记是预留态的孤儿时,名字行扫描会把它报成 `orphan_name`,apply 收掉之后重跑。占用者是已删的角色、登记已绑定时:看占用者那一片的 `account_role_removal` 里还有没有它那一行,有就等后台删角收尾(data_service 的 `ResumeRemovals`,每分钟一轮,接着做开始超过 5 分钟的那几行)放掉;没有(名字片恢复点早于删角释放)角色行扫描会把占用者报成 `removed_role_name`,apply 放掉之后重跑。`holder_live=true` 是一名两角,`keeper` 是该留名的一方(0 是判不出),两边都不动,记工单交负责人定。
   - `orphan_name` 带 `store_saved`、`ref_active`:那名角色像是建成了、只是玩家片恢复点早于提交(反方向残留,见上),不要释放名字,先看玩家片所在集群能不能恢复到更靠近 T 的点,不能就记工单。带 `live_role_holds_other_name`、`corrupt_role_row`:ID 复用或库被人动过,人工核对。`role_committed`:扫完之后角色建成了,重跑 plan 按 `name_unbound` 处理。
   - `transferred_role`:建角之后过过户,存档库丢的不只是初始行。先看存档库所在集群能不能恢复到更靠近 T 的点,再按过户收据和商城订单处理,不要直接重写初始行。
   - `legacy_role`、`partial_store_rows`、`no_route_or_placement`、`corrupt_role_row`、`name_rules_changed`、`player_holds_other_name`、`ref_hard_cap`、`name_bound`:人工核对。`role_busy`(交易或删角令牌挡住初始行)、`role_removing`(删角进行中,名字三项只报告:补回去的名字可能赶在删角收尾放名字之后,绑在墓碑上没人放)等交易或删角走完再跑。
   - 同一个存档库报出大量 `initial_rows_missing`,说明这个集群的恢复点明显早于别的集群,先考虑把它重新恢复得更靠近 T,再跑 apply。
   - `role_changed`、`writer_gate_frozen`、`store_changed`、`name_cache_unavailable`(没给 `-name-cache-addrs`,名字一项都不修),以及 `result=failed`:补上参数或重跑 plan 再看。
5. 补回的名字修完马上查得到:工具在修好之后删了名字缓存(2026-11-12 起;以前要等负缓存 `PlayerName.NegativeCacheTTL` 的 60 秒)。核账要在起服之前跑:有服务在读名字时,一次在修复之前读库、修复之后才回填的读还能把负缓存写回去,最多再藏 60 秒。

**这些残留不要用摘孤儿删。** `LoginAdmin.RemovePlayersFromAccounts` 对 `initial_player_commit=1` 的角色(`LookupAccountRole` 回这个标记)一律回 `NOT_ORPHAN`,日志提示走本工具:墓碑不可逆,这些角色是真的建成了。

### 4.1b TiDB 档:BR 时间点恢复(2026-10-31,未演练)

命令来自 TiDB 官方文档,按实际版本核对。**日常要先开起来**,没有开日志备份就恢复不到任意时刻。集群不随本仓库部署,由运维负责:

```bash
# 日志备份,一个集群只跑一个任务,持续运行。
tiup br log start --task-name=pitr --pd "${PD_IP}:2379" --storage 's3://<桶>/logbackup'
```

```bash
# 快照备份,定期跑(比如每两天一次),恢复点之前要有一份。
tiup br backup full --pd "${PD_IP}:2379" --storage "s3://<桶>/snapshot-${date}"
```

恢复(接 §4.0 第 3 步):

1. 建一个新集群,恢复到它上面。原集群保留到开流量之后。
2. 执行恢复。`--restored-ts` 要早于 `tiup br log status` 显示的 `checkpoint[global]`:

   ```bash
   tiup br restore point --pd "${NEW_PD_IP}:2379" --storage 's3://<桶>/logbackup' --full-backup-storage "s3://<桶>/snapshot-${date}" --restored-ts '<T,如 2026-05-15 18:00:00+0800>'
   ```

3. 恢复期间这些表不能读写,等命令结束再核对。核对之后接着做 §4.0 第 4~7 步。

一个地域的逻辑库分布在多个集群时,每个集群各恢复一次,`--restored-ts` 用同一个 T。跨集群的提交没有全局一致的时间点,贴着 T 的跨库操作可能只留下一边,这部分按 §4.1a 的表核账。

### 4.2 单玩家 / 单 zone 数据抽取(更常见)

合服失败 / 客服补单这种场景,只想从 PITR 影子库抽某玩家的几行,**不要替换整库**。

> **2026-10-30 更正**:玩家存档只在 `player_store`(`player_id, kind` 一行一组,写权行是 `kind=0`)。必须加 `--no-create-info --skip-add-drop-table`,
> 否则导出的文件会先 `DROP TABLE`,照下面灌回去会清空整张表。`--replace` 让它按主键覆盖,`--single-transaction` 保证这名玩家所有行是同一快照,
> `--hex-blob` 让 payload 以十六进制出现、文件可审阅。
>
> **灌回生产之前玩家必须离线**:写权行 `released=1`,没有现任。灌回的写权行代次比 Redis 的 owner 计数器旧,下次认领时 E 更大,go/db 按正常认领处理,不需要额外修屏障。

```bash
# 在影子库上 dump 单玩家(同一快照,不带建表 / 删表语句)
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$SHADOW_NS" \
  exec mysql-shadow -- sh -ec \
  'export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:?影子容器未注入 root 凭据}"; exec mysqldump -uroot "$@"' \
  mysqldump-client \
  --no-create-info --skip-add-drop-table --replace --single-transaction --hex-blob \
  --where="player_id=12345678" \
  player_store_1001000_db player_store \
  > player-12345678-recovered.sql

# 在生产库 review 后再灌
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  exec -i deploy/mysql -- sh -ec \
  'export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:?容器未注入 root 凭据}"; exec mysql -uroot "$@"' \
  mysql-client player_store_1001000_db < player-12345678-recovered.sql
```

---

## 5. 与其他流程的衔接

### 5.1 合服(别名合服,不停服)

合服只在全局库 `zone_merged_into` 写一行「源区 → 目标区」,不动玩家行、路由行,也不改写公会、聚宝斋等业务数据;按区比较和查询都在读的时候解析。撤销只删这一行(见[不停服合服手册](merge-zone-runbook.md) §1、§6;方案见 [player-data-tidb-scaling-plan.md](../design/player-data-tidb-scaling-plan.md) §7.1)。所以:

- **不要** `k8s-zone-down` 源区或目标区。合服不停服、不踢人;停 zone 反而会把在线玩家全部踢下线。
- **不需要**专门的合服前备份。合服的回退手段是撤销映射,不是从备份恢复。例行备份照常按 §3.1 检查。
- 同一个维护窗口里还要做别的数据操作(搬库 `tools/dbmove`、改表结构等)时,按那项操作自己的手册,在它之前手动备份(§3.2)。

旧口径(2026-10-30 之前)是「先 zone-down 源区和目标区,再手动备份,再合服」。那时合服是改写式的:逐个改玩家归属、拷玩家行,中途出错只能靠备份回退,所以必须先停写再备份。别名合服不改任何一行玩家数据,这两步都不再需要。

### 5.2 不再有单 zone 回档

单 zone 回档脚本 `k8s_zone_rollback.ps1` 和它依赖的清理工具 2026-10-31 退役。玩家不按 zone 存,所有 zone 都能写同一个存档库,停一个 zone 挡不住写入。回档按 §4.0 整个地域一起做,只修一个库走 §4.1a,抽单个玩家走 §4.2。

---

## 6. 故障排查

| 症状 | 原因 | 处置 |
|---|---|---|
| `kubectl get cronjob` 显示 `LAST SCHEDULE` 为空 | controller-manager 挂了 / 时区配错 | 看 kube-controller-manager 日志 |
| Job pod 起来但秒退 | mysqldump 连不上 mysql Service | 在本节同一 context / namespace 中定位备份 Pod,核对其 Secret 引用与数据库账号;交互测试用 `mysql -h mysql -uroot -p`,不在 `-p` 后拼口令 |
| dump 文件大小 = 0 / 接近 0 | dump 命令报错被 gzip 吃掉 | 改 cronjob 加 `set -o pipefail`(下次迭代) |
| binlog 目录不存在 | initContainer 没跑 / 老 pod 升级遗留 | 进 pod `ls /var/lib/mysql-binlog`(PVC 根的 binlog/ 子目录,不在 datadir 里),手动 `mkdir -p` 后重启 |
| 备份卷快满 | 保留策略太宽 / 实例写入暴增 | 改 `mysql-backup-cronjob.yaml` 的 `-mtime +30` 数字,或扩 PVC |
| PITR 时 binlog 缺失 | binlog 在 CronJob 跑之前就过期(>7d) | 改 `binlog_expire_logs_seconds`(`mysql-config`)调到 14d,改 CronJob 改成每 6h 跑一次 |

---

## 7. 没做完的事(未来 iteration)

| 项 | 优先级 | 说明 |
|---|---|---|
| 备份 → S3/OSS 异地归档 | 高 | PVC 只防 pod 重启,不防整个集群 / 机房挂 |
| 自动恢复演练(每月) | 中 | 加一个 CronJob 把昨天的 dump 灌进临时影子库,验 row count;失败告警 |
| 备份失败告警 | 中 | 接 Prometheus AlertManager:`kube_cronjob_status_last_successful_time` 超过 36h 报警 |
| 仓库模板改为 Secret 引用 | 高 | 仓库模板仍含开发固定口令;本手册的 prod 步骤要求先准备 Git 外的 Secret 引用清单,不允许直接发布开发模板 |
| 多副本 / 主从复制 | 低 | 当前 single-instance + Recreate 策略,合服 / 大改动时有几分钟 downtime |

---

## 8. Changelog

- **2026-05-15 v1**: 初版。落地 `mysql.yaml`(PVC + binlog 配置)+ `mysql-backup-cronjob.yaml`(每日 dump + binlog)。
- **2026-10-15**:首次安装与升级改用正规 infra 入口,明确 `mysql-init-sql`、目标 context / namespace、备份 CronJob 仍须单独安装及存量初始化边界。仅修订手册,未执行部署或恢复演练。
- **2026-10-15 补充**:现行命令显式携带目标 context / kubeconfig / namespace;SQL 工具改为在容器内读 root 凭据;备份与影子实例先准备匹配目标环境的 Secret 引用清单,不再发布开发固定口令。
- **2026-10-16**:§4.1 加第 7c 步、§5.2 加一句:回档后删除受影响玩家的资产 SQL 提交证据键 `player:{id}:asset_sql_commit`(db-write-fencing-mysql.md §6.6、§9.2)。仅修订手册,未演练。
- **2026-10-30**:玩家存档只在 `player_store`,§4.1 第 7c 步和 §4.2 的两张表抽取随旧路径删除。仅修订手册,未演练。
- **2026-10-31**:加 §4.1a,单库回档和角色护栏:参与者库回档清空 `role_fence`,目录回档抬 `fence_version`,过户和回档的关系。仅修订手册,未演练。
- **2026-10-31 补充**:加 §4.0 整体做法和 §4.1b TiDB 档;§4.1 第 7b 步去掉已失效的发号水位(发号器已搬到 etcd);单 zone 回档脚本退役,§5.2 随之改写。仅修订手册,未演练。
- **2026-11-05**:文末 10-20 r2-mz#1 那段改写为「合服撤销之后的恢复」:copy 合服与 `-allow-home-fallback` 已删,别名合服撤销后没有要重建的东西。仅修订手册。

> **2026-10-30:本节以下到文末全部作废,只作历史保留。** 玩家存档已不在 Redis,恢复之后不需要重建存盘屏障,也没有 outbox 要裁决。工具 `save_barrier_restore` 已改名 `go/db/cmd/maintenance_phase`,只剩 `-authority-phase`,下文的 `-mode` 都不存在了。恢复玩家数据只需要按 §4 恢复 `player_store`,放流前清掉 Redis 里的 `location`、`handoff` 这些协调键或等 scene_manager 判死,玩家下次认领时代次比库里新,按正常流程进场。

### 2026-10-17 父快照屏障的追加恢复要求

> **已退役(2026-11-01 标注)**:玩家存档已移出 Redis,父快照屏障和 `save_barrier_restore` 工具都已删除,映射副本、就绪标记和水位键也已删除(映射提案 §15)。本节只作历史,不要照做;恢复按 §4.0。

上述 7c 必须同时删除受影响玩家的 `player:{id}:save_barrier`，并清理父/子缓存；旧的两表 ACK 不能证明回档后的 SQL。保持停写维护闸，停止旧 scene 并排空旧 Kafka/retry；核对归属计数器不倒退、同一权威 TiDB 的两张玩家行版本一致之后，按 `db-write-fencing-mysql.md` 的登录冷恢复门禁恢复屏障，再放流。未迁移历史数据会被新登录明确拒绝；不能依赖“下一次存盘自动修好”而先放流。此处仅为手册修订，尚未演练自动恢复。

父快照现在还可能持有尚未投递 Kafka 的完整存盘意图。删除屏障前须保存并裁决其 `outbox_*` 原始内容，核对 `player:save_pending:<home_zone>:<player_id % 64>`；不得因 Kafka lag=0 而假定已排空。正常合服/撤销由 `merge_zone` P4 拒绝任何非空桶，须先让原 DB 补投器凭双 SQL ACK 回收。回档中确需清除时，只处理本次受影响玩家的 member，禁止删整桶或静默丢弃未提交意图。屏障人工恢复与灾备流程仍未演练，恢复完成前不可放流。

### 2026-10-18 历史屏障计划与维护恢复工具

> **已退役(2026-11-01 标注)**:玩家存档已移出 Redis,父快照屏障和 `save_barrier_restore` 工具都已删除,映射副本、就绪标记和水位键也已删除(映射提案 §15)。本节只作历史,不要照做;恢复按 §4.0。

`go/db/cmd/save_barrier_restore` 补充上述「从真实 SQL 建立屏障」步骤。首版只支持 Redis 权威路由、映射/父缓存同实例 DB0、显式稳定 placement 指向 home zone 自己的 `zone_<id>_db`。`db_first`、独立 MappingRedis、非零 Redis DB、异区/全局落点和 Redis Cluster 仍须专门的路由冻结证明，工具会拒绝不支持的形态，不得伪造本区映射绕过。

1. 保持登录维护入口；停止所有受影响旧 scene/login/DB 写者并禁自动重启。先让新版 DB 排空所有世代 Kafka/retry/processing/dead 与存盘 outbox，再停止 DB。恢复 SQL 与 owner 计数器，确认 owner 不倒退；本工具不能恢复丢失的 owner。
2. 保存并裁决未提交原意图，备份后清本次玩家的父/子缓存、旧资产 SQL 证据及需要重建的旧屏障。任何尚存缓存、交接标记、冲突屏障或非空 pending 桶都会被工具拒绝；工具自己不删除这些证据。冷加载尚未完成前，维持停写。
3. 在同一 Redis DB0 建立**无 TTL**的 `player:save_barrier:maintenance:<home_zone>`，值为维护工单号。该标记只供工具校验，不会让游戏进程自动停写，不替代第1步。
4. 连接凭据由受控环境变量注入，运行以下命令（工作目录 `go/db`，示例17区/玩家7仅为占位）。计划文件使用 `O_EXCL` 创建，已有文件不覆盖；将命令输出的 SHA-256 与计划一起归档并审查。零围栏迁移另加 `-allow-legacy`，它只修改两表围栏元数据，不改玩家资产列。

```powershell
# SAVE_BARRIER_RESTORE_DSN、SAVE_BARRIER_RESTORE_REDIS_PASSWORD 已由环境注入；不打印它们。
go run ./cmd/save_barrier_restore -mode plan -schema zone_17_db -zone 17 `
  -players 7 -redis 127.0.0.1:26379 -redis-db 0 -routing-mode redis-same-instance `
  -maintenance-id '<维护工单号>' -plan '<受控目录>/barrier-plan.json' `
  -writers-stopped -queues-drained

go run ./cmd/save_barrier_restore -mode apply -schema zone_17_db -zone 17 `
  -redis 127.0.0.1:26379 -redis-db 0 -routing-mode redis-same-instance `
  -maintenance-id '<同一维护工单号>' -plan '<同一计划文件>' `
  -plan-sha256 '<已审查原始文件SHA-256>' -writers-stopped -queues-drained
```

默认必须连接 TiDB 当前强读会话；`-local-mysql` 仅供本地兼容验证，不能作全球数据层证据，该模式绑定计划，执行时不得切换。执行逐人输出 JSON 回执；任一失败立刻退出，已完成前缀不回退，维护标记始终保留。SQL 围栏提交后 Redis 不可用时，可修复连接后按**同一计划**续跑；资产/归属/映射改变则停止重新审查，不能修改计划摘要强行放行。

整批成功后，先以受控测试号验证缺父键登录确实强读到同版两行、资产收据与 SQL 一致，再按发布单恢复写者、维护入口及删除本次维护标记。工具不会自动开放流量；本地隔离工具验证不代表完整 PITR、跨洲灾备或目标容量验收。2026-10-18 的 Linux 真 TiDB/Redis 四项集成与同包单元共 38 项通过、零跳过，原始收据见 `build/barrierrestore-linux-20261018-030237-63a67b/final-proof.json`；高位自增分配器边界与真实旧写者复活仍未覆盖。

**合服撤销之后的恢复（2026-11-05 按现状改写，原为 2026-10-20 r2-mz#1 补充）**：10-20 曾给 plan 加过 `-allow-home-fallback`，专门恢复 copy 合服撤销之后「没有落点记录、home 已改回源区、行在源区 zone 库」的玩家，配合旧合服手册 §10.1 的「屏障重建」。这条例外和它针对的形态都已不存在，也没有替代开关：

- 合服改成了别名合服（[不停服合服手册](merge-zone-runbook.md) §1）：只在全局库 `zone_merged_into` 写一行「源区 → 目标区」，不拷玩家行，不改 `player_route` 的 `home_zone` 和 `storage_id`；撤销只删这一行（同手册 §6）。撤销之后玩家的归属、落点和存档都和合服之前一样，没有要重建的东西。
- 玩家存档只在 `player_store`，`player:{id}:save_barrier` 已删除，恢复之后不用从 SQL 重建屏障（见上方 2026-10-30 说明）。
- 工具已改名 `go/db/cmd/maintenance_phase`，`-allow-home-fallback` 和全部 `-mode` 都已删除。

合过服的环境（撤没撤销都一样）要回档，做法和没合过服的相同：整体回档按 §4.0，`zone_merged_into` 与合服历史 `zone_merge_history` 都在目录第 0 片，同一个事务写入，随目录回到同一个 T；T 之后的合服和撤销连同它们的历史一起丢弃，要重做就按合服手册再执行一遍。只修一个库按 §4.1a。旧的「屏障重建」步骤只留在[停服版旧手册](../archive/merge-zone-runbook-stop-the-world.md) §10.1 作历史，不要照做。

### 2026-10-18 第二批：db_first、独立映射和全局落点（实现后须核本批验证收据）

> **已退役(2026-11-01 标注)**:玩家存档已移出 Redis,父快照屏障和 `save_barrier_restore` 工具都已删除,映射副本、就绪标记和水位键也已删除(映射提案 §15)。本节只作历史,不要照做;恢复按 §4.0。

第二批计划为 v2，v1 计划仍按上一节同实例本区边界执行。支持 `db_first`、`redis-split`、非零父缓存 DB、稳定异区 zone 库和 `player_store_*_db`；映射始终 DB0，SQL 路由库与玩家库必须同一 TiDB 实例。Redis Cluster 仍不是现行全链协议支持的部署方式。连接地址、DB、schema、强读模式及路由证明全部绑定计划，不得用改参数替换重审。

1. 停止业务、全部归属发号者（包括 scene_manager）、路由服务及全部合服/pin/relocate/回填/修复写者，禁止自动复活，保留维护入口。排空/裁决各代 Kafka、retry、dead、父 outbox；本工具不自动停止这些写者。独立保存并审定所有曾发出的 owner epoch 上界，包含已过期/释放实例、handoff 已预发的新 epoch 与在途发号，取证截点后不再允许旧发号者前进。
2. `db_first` 先按合服手册 §9.4 做 SQL 路由 freeze、必要的反向修库与审计，暂不 unfreeze；撤下 mapping 的就绪标记。**必要反向修库必须先于本工具 guard**，现有 route_tool 会拒绝 merge 围栏。保留同世代、且不领先 SQL 的永久 `mapping:route:watermark`。SQL 冻结和正确水位缺一则拒绝，不能删水位冒充已恢复。
3. 恢复/核对 owner 后运行 guard，再生成和审查计划。v2 必填 `-audited-owner-upper-bound`，每名玩家当前永久 owner 必须严格高于该独立审定上界；不能用回退 SQL 或当前 Redis 计数器推算。缺少可信上界保持维护，工具不铸造 owner。guard 只建立永久父缓存维护标记、mapping 维护标记与 `merge:in_progress:<home>`（值为 `save_barrier_restore:<维护号>`）；部分失败会保留已建闸，不自动清除。它不能替代 SQL freeze 或业务停写。

```powershell
# 工作目录 go/db；DSN 和两个 Redis 密码均由受控环境注入，不写在参数或计划内。
$restoreTarget = @('-schema','player_store_1000000_db','-zone','17',
  '-redis','127.0.0.1:26379','-redis-db','2',
  '-mapping-redis','127.0.0.1:26380','-mapping-redis-db','0',
  '-routing-mode','db_first','-route-schema','mmorpg_global',
  '-audited-owner-upper-bound','<独立证据审定的旧代最大epoch>',
  '-maintenance-id','<同一维护工单号>','-writers-stopped','-queues-drained')
go run ./cmd/save_barrier_restore @restoreTarget -mode guard
go run ./cmd/save_barrier_restore @restoreTarget -mode plan -players 7 `
  -allow-legacy -plan '<受控目录>/barrier-plan-v2.json'
go run ./cmd/save_barrier_restore @restoreTarget -mode apply `
  -plan '<同一原始计划文件>' -plan-sha256 '<已审查原始文件SHA-256>'
```

Redis 为权威且实例分离时使用 `-routing-mode redis-split`，去掉 `-route-schema`；必须存在正确的永久 home/稳定 placement，不能用 SQL 路由副本替 Redis 真源背书。`db_first` 的 mapping 三项则允许全部缺席，否则必须与被锁 SQL 路由及 rev 一致；不会把局部缺失视作权威无玩家。SQL 中已合走 home、有效合服围栏、未冻结/变更的世代/水位/rev、错误物理落点都会拒绝。

执行失败继续保留所有维护闸和计划；已完成前缀可按同计划重入。修复中若路由或资产改变，重新审查，不能篡改计划摘要。整批成功后仍须按发布单验证强读冷加载、SQL 收据与故障恢复，再由维护负责人按原值移除本次标记和广播围栏、恢复 SQL 路由/副本并放流。工具不做这些放流动作；人为删围栏、绕过维护直接写 SQL/Redis 不属于安全恢复前提。

本批当前部署矩阵真库收据为 `build/verification/barrier-restore-v2-20261018/final-real-20261018T094145944Z/proof.json`：带独立旧代上界的库包 64 项通过、零失败、零跳过，包含独立 Redis 权威全局落点恢复/漂移拒绝与 SQL 本区 NULL 落点。90 项输入前后无漂移，随机库与自建键清理断言通过。此前 `real-proof.json` 的62项是补充分支前的历史结果。该收据只证明恢复接缝，不替代旧实例复活和延迟 SQL ACK 宿主演练。

本机双区功能演练入口为 `tools/scripts/tests/run_ownership_fault_fullstack.ps1`。它要求独立 `RunDir`、本轮 `RunId`/`RunStartedUtc`、已编译的两个测试程序，以及实际新建 source scene 的 `SourceReceipt`；不得用配置准备文件代替进程回执。scene 的 PID、启动时刻、镜像路径/SHA、所属区和正式 30 秒存盘周期均须匹配，并先启动独立恢复看门狗。演练只暂停并恢复本轮自有 source，不停止其他实例；失败保留日志和 SQL 证据，先恢复 source，再结束自己的控制器。

`SourceReceipt.process_creation_filetime_utc` 必须是创建器持有的原始进程句柄对应 `GetProcessTimes` 创建 FILETIME 的十进制字符串；`started_utc` 由同一值转换，两者精确相等，不能由 CIM 的近似显示时间反推或预填。暂停与恢复在同一个已打开句柄存活期间核对真实镜像路径、精确 FILETIME 和文件 SHA，再操作该句柄，避免查询 PID 后重新解析 PID 的重用竞态。看门狗绑定主编排单次读取的收据字节 SHA，就绪回执还须匹配全部进程身份；身份不符或恢复失败不得解除看门狗。机器人与控制器的失败清理只操作创建时保留并重新核验的进程句柄，不用 `Stop-Process -Id`。这是演练控制器的准入约束，静态复核不能代替 C# 编译、身份查询验证及真实故障运行证据。

2026-10-19 UTC 已从实际生产脚本提取 C# 编译并完成四例仅查询验证：自身完整身份匹配成功，创建时间错一 tick、镜像路径不符、SHA 不符分别拒绝；脚本首终 SHA 一致。收据为 `build/verification/ownership-private-nodes-20261018T093112231Z-11de2033/identity-query-20261019T025338287Z/proof.json`。这四例没有调用暂停、恢复或终止，真实双区故障演练及失败清理运行仍待验，不能据此宣称宿主场景已通过。

验收必须分别取得：第二表 SQL 锁定期间首 ACK 已到但交接仍未放行，释放后真实跨区往返且资产恰变一次；旧 source 暂停后真实租约死亡、新实例以更高 epoch 落库，恢复旧 source 后出现精确旧 epoch 的 CAS 拒写，且 SQL 不回退。单纯正常退出、客户端迟到回调单测或命令枚举都不能代替这两项。当前两个测试程序仅编译、隔离配置仅准备；C++ 底层后续改动的节点链接证据与完整实跑仍待补齐，不据此开放维护入口。

### 2026-10-19 新持久世代字段的历史恢复补充（真实环境仍待验）

恢复工具已兼容永久父屏障的合法第四字段 `schema_gen`：仅允许 pending、player_database、player_database_1 以及可选 schema_gen，未知字段、任何未裁决outbox、缺少任一精确ACK、TTL或不规范世代均在首次HSET前拒绝。工具不删除任何字段，也不放宽停写、双表提交、旧代上界和路由冻结要求。

正常迁移必须先为两张玩家表建立 `persist_schema_gen` 的 int unsigned / NOT NULL / DEFAULT 0 列。缺列、NULL及非规范uint32会拒绝，不得靠临时补测试列冒充迁移已完成。工具从同一事务已经点锁的两行取最大世代，历史0的有效floor为1；缺hash时建立floor，已有合法floor只升不降，SQL原戳和全部资产列不改。SQL原始戳仍进入全列摘要；增列或0→1等原字节变化后，须重新生成并审查计划，不能手改计划或以有效世代相同为由忽略漂移。该floor不加入可编辑计划字段。

### 2026-10-22 口径变更：`schema_gen` 改为两行最小值、以 SQL 为准覆盖（未编译、真实环境未验）

上一节"取两行最大世代、已有 floor 只升不降"已作废（`docs/design/mixed-version-persist-field-protection.md` §13 第 8 条）。原因：scene 严格屏障（C3）与 login 快路径（C4）比较的"加载世代"是两表戳的**最小值**；恢复写最大值或保留更高的旧值时，两行戳不同（单表进过死信）或 hash 曾被一次没落进 SQL 的写抬高的玩家，恢复后第一次进游戏与存盘都会被判倒退，enforce 下既进不来也存不了——而恢复本身就是倒退的处置手段。现行口径：

- 写入 `schema_gen = max(1, 两行戳的最小值)`（0 按 1），即恢复后冷加载出的数据的加载世代；
- hash 里已有的 `schema_gen` 只是索引，一律覆盖，格式损坏也不再拒绝；
- 字段白名单另接受 `schema_writer` / `schema_regressed`（scene 写入的写方令牌与 warn 倒退证据），写入时两者一并删除，之后任何写方的第一次存盘都重新比较；
- 其余拒绝条件不变（未知字段、未裁决 outbox、缺精确 ACK、TTL、停写与路由冻结）。

代价：两行戳不同的那名玩家，较高那张表里新世代字段的数据会在下一次整行写时按较低世代重写——这正是"以 SQL 为准恢复"接受的损失；执行前在工单里写明受影响玩家。

当前证据仅为私有接缝RED→GREEN：旧实现真实7子例失败；其中4条缺floor断言、3条合法第四字段预检拒绝。最初收据器错误地要求7条require ErrorTrace并停止，该首错/原生exit1原件保留，逐例补充解释后不重跑RED。修复后22顶层单元（含子项103条）与CLI5均通过、0skip，两包vet和CLI build退出0，751输入与22046实际依赖首终无漂移；正规应用7手写路径后canonical与私有GREEN全751相同。原件 `build/verification/barrierrestore-schema-gen-candidate-20261019T093721030724Z/root-unit-preparation/green-after-red-20261019T104347472Z-416cfbdf/proof.json`，SHA `60585D50533B5E5E7E0D0BBFD4C2E25886D90270E26ECA6964343B8F16EDF6AB`。

新增真实TiDB/独立Redis组合尚未执行，原64项真库成绩只绑定旧来源，不作为本批通过。C++ C3写抬floor和login C4读比较仍未实施；本工具格式兼容不能证明混版本世代防倒退已完成。真实双区旧实例复活、延迟SQL ACK、全链放流及灾备验收继续维持未完成。

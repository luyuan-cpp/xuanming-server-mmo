# MySQL Backup & PITR Runbook

> **状态**: v1 — 2026-05-15
> **范围**: K8s 部署的 MySQL(`deploy/k8s/manifests/infra/mysql.yaml` + `mysql-backup-cronjob.yaml`)的备份、归档、恢复 SOP。
> **关联文档**: `docs/design/zone_data_rollback.md` §3(灾难恢复级 zone 回档),`docs/design/server_merge_design.md` §5.1(合服前备份要求)。

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

### 3.2 手动触发备份(合服 / 灾难前)

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
  mysql-client -e "SELECT COUNT(*) FROM zone_101_db.player_database;"
# zone_101_db 仅为示例,必须替换为本次恢复计划中的真实业务库。

# 7a. 若仅是抽取单个玩家:从影子库 dump 该玩家行 → 导回生产
# 7b. 若是全库回档:停生产 mysql,把影子 PVC 推上去
#     ⚠ 全库回档 = 连同 mmorpg_global 里 id_segment 的发号水位一起倒回目标时刻,这是 ID 安全事件。
#     MySQL 里的消费表虽然一起回档,但 Kafka 里的 tx_id / snapshot_id、Redis 里在线玩家 blob 的物品 guid、
#     各进程内存里已经领走的号段都不会回退;不处理,下一次领段就把用过的号再发一遍
#     (login 的 ON DUPLICATE KEY UPDATE 静默覆盖别人的角色行,流水表 INSERT IGNORE 静默丢新流水)。
#     推上去之后、放开流量之前,按 deploy/k8s/README.md「恢复全局库前须先核对 id_segment.max_id」三步做完:
#       ① 停 data-service,重启所有持有号段的进程(login / guild / scene);
#       ② 逐个 biz_tag 拿消费侧最大号(含各 zone 库、Kafka 两个 topic、mmorpg_trade / mmorpg_guild 独占库);
#       ③ max_id 不大于该最大号的,抬到「最大号 + 1 + 至少一个 step」。
#     7a 的单玩家抽取不涉及水位,不需要这一步。
# 7c. 7a / 7b 之后、放开流量之前:删除受影响玩家的资产 SQL 提交证据 player:{<player_id>}:asset_sql_commit
#     (共享 Redis DB 0;7b 为该 zone 全部玩家)。证据指向已被回滚的提交,不删的话资产通道会拿回滚前的账本终结
#     对侧账,回档后的新存盘也发布不上去(docs/design/db-write-fencing-mysql.md §6.6、§9.2)。删除方向安全:
#     待办等玩家下一次存盘重新发布。
```

### 4.2 单玩家 / 单 zone 数据抽取(更常见)

合服失败 / 客服补单这种场景,只想从 PITR 影子库抽某玩家的几行,**不要替换整库**。

> **2026-10-19 更正**:原示例没有 `--no-create-info`。mysqldump 默认带 `--opt`(含 `--add-drop-table`),
> 导出的文件以 `DROP TABLE IF EXISTS` + `CREATE TABLE` 开头,照下面「在生产库 review 后再灌」执行会**清空生产 zone 库整张
> `player_database`**。抽单玩家必须加 `--no-create-info --skip-add-drop-table`;`--single-transaction` 保证两张表同一快照,
> `--hex-blob` 让 blob 列以十六进制出现、文件可审阅;两张玩家表要一起抽。
>
> 另外,灌回生产的旧行带着旧的行内围栏(`fence_owner_epoch` / `fence_save_seq`),与玩家当前的存盘屏障对不上,
> login 冷加载会拒绝该玩家(「SQL 子表围栏尚未对齐父快照提交屏障」)。回灌生产不是本节能单独完成的操作,
> 必须按 [db-write-fencing-mysql.md](../design/db-write-fencing-mysql.md) 的恢复流程处理屏障与归属计数器。
> 只想在本地看或复现,用 [online-debug-data-fetch.md](online-debug-data-fetch.md) 的 player-bundle 导出 / 导入。

```bash
# 在影子库上 dump 单玩家(两张玩家表,同一快照,不带建表 / 删表语句)
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$SHADOW_NS" \
  exec mysql-shadow -- sh -ec \
  'export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:?影子容器未注入 root 凭据}"; exec mysqldump -uroot "$@"' \
  mysqldump-client \
  --no-create-info --skip-add-drop-table --single-transaction --hex-blob \
  --where="player_id=12345678" \
  zone_101_db player_database player_database_1 \
  > player-12345678-recovered.sql

# 在生产库 review 后再灌
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  exec -i deploy/mysql -- sh -ec \
  'export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:?容器未注入 root 凭据}"; exec mysql -uroot "$@"' \
  mysql-client zone_101_db < player-12345678-recovered.sql
```

---

## 5. 与其他流程的衔接

### 5.1 合服前(server_merge_design.md §5.1)

**强制**:在 `k8s-zone-down` source/target 之后,手动触发一次备份,再开始合服:

```bash
BACKUP_JOB="mysql-backup-pre-merge-$(date +%s)"
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  create job --from=cronjob/mysql-backup "$BACKUP_JOB"
# 等 Job 完成
kubectl --context "$KUBE_CONTEXT" --kubeconfig "$KUBE_CONFIG" -n "$INFRA_NS" \
  wait --for=condition=complete --timeout=2h "job/$BACKUP_JOB"
```

### 5.2 zone 灾难回档(zone_data_rollback.md §3)

§3 列的「步骤 2 MySQL PITR」直接用本文 §4.1 的流程。**先恢复到影子库**,确认无误后再决定换正式库还是抽数据。
没有整体清空共享 Redis 时(`k8s_zone_rollback.ps1` 默认拒绝 FLUSHDB),按 §4.1 第 7c 步逐个删除该 zone 玩家的资产 SQL 提交证据键。

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

### 2026-10-17 父快照屏障的追加恢复要求

上述 7c 必须同时删除受影响玩家的 `player:{id}:save_barrier`，并清理父/子缓存；旧的两表 ACK 不能证明回档后的 SQL。保持停写维护闸，停止旧 scene 并排空旧 Kafka/retry；核对归属计数器不倒退、同一权威 TiDB 的两张玩家行版本一致之后，按 `db-write-fencing-mysql.md` 的登录冷恢复门禁恢复屏障，再放流。未迁移历史数据会被新登录明确拒绝；不能依赖“下一次存盘自动修好”而先放流。此处仅为手册修订，尚未演练自动恢复。

父快照现在还可能持有尚未投递 Kafka 的完整存盘意图。删除屏障前须保存并裁决其 `outbox_*` 原始内容，核对 `player:save_pending:<home_zone>:<player_id % 64>`；不得因 Kafka lag=0 而假定已排空。正常合服/撤销由 `merge_zone` P4 拒绝任何非空桶，须先让原 DB 补投器凭双 SQL ACK 回收。回档中确需清除时，只处理本次受影响玩家的 member，禁止删整桶或静默丢弃未提交意图。屏障人工恢复与灾备流程仍未演练，恢复完成前不可放流。

### 2026-10-18 历史屏障计划与维护恢复工具

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

**2026-10-20 补充（r2-mz#1，未编译、未测试，待 Codex 验证）**：上面「显式稳定 placement」的限制新增一个显式例外。plan 加 `-allow-home-fallback` 后，**没有落点记录**、home 恰为 `-zone`、`-schema` 就是该区 zone 库的玩家，按 go/db 的有效落点（记录 ?? home）恢复。copy 合服撤销后的玩家天然是这种形态，流程见 [merge-zone-runbook.md](merge-zone-runbook.md) §10.1「屏障重建」。开关写进计划、计入 SHA-256，apply 与 guard 拒收它；落点记录仍不存在在写屏障的同一段 Lua 里原子复核；落点键存在（含空串）、home≠zone 等其余拒绝条件不变。

### 2026-10-18 第二批：db_first、独立映射和全局落点（实现后须核本批验证收据）

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

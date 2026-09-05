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

### 2.1 全新集群

```bash
kubectl apply -f deploy/k8s/manifests/infra/mysql.yaml
kubectl apply -f deploy/k8s/manifests/infra/mysql-backup-cronjob.yaml

# 验证 binlog 已开
kubectl exec deploy/mysql -- mysql -uroot -pMmorpg#2026db \
  -e "SHOW VARIABLES LIKE 'log_bin';"
# 期望:Value=ON

# 手动触发一次备份验证 CronJob 工作
kubectl create job --from=cronjob/mysql-backup mysql-backup-smoke-$(date +%s)
kubectl logs job/mysql-backup-smoke-<id> -f
```

### 2.2 从老版本(emptyDir + 无 binlog)升级 — ⚠️ 数据迁移

```bash
# 1. 先 mysqldump 老数据到本地(不能丢)
kubectl exec deploy/mysql -- mysqldump -uroot -pMmorpg#2026db \
  --all-databases --single-transaction > pre-upgrade-dump.sql

# 2. 删老 Deployment(emptyDir 数据随 pod 销毁)
kubectl delete deployment mysql

# 3. apply 新 mysql.yaml(创建 PVC + binlog 配置)
kubectl apply -f deploy/k8s/manifests/infra/mysql.yaml

# 4. 等新 pod ready
kubectl rollout status deploy/mysql

# 5. 恢复数据
kubectl exec -i deploy/mysql -- mysql -uroot -pMmorpg#2026db < pre-upgrade-dump.sql

# 6. apply CronJob
kubectl apply -f deploy/k8s/manifests/infra/mysql-backup-cronjob.yaml
```

---

## 3. 日常运维

### 3.1 检查备份是否在跑

```bash
# CronJob 上次成功时间
kubectl get cronjob mysql-backup -o jsonpath='{.status.lastSuccessfulTime}'

# 最近 7 天的 Job 历史
kubectl get jobs -l job-name=mysql-backup --sort-by=.metadata.creationTimestamp

# 备份卷里的实际文件
kubectl run -it --rm peek --image=busybox:1.36 --restart=Never \
  --overrides='{"spec":{"volumes":[{"name":"b","persistentVolumeClaim":{"claimName":"mysql-backup-pvc"}}],"containers":[{"name":"peek","image":"busybox:1.36","stdin":true,"tty":true,"volumeMounts":[{"name":"b","mountPath":"/backup"}]}]}}' \
  -- sh -c 'ls -lh /backup/dump /backup/binlog'
```

### 3.2 手动触发备份(合服 / 灾难前)

```bash
kubectl create job --from=cronjob/mysql-backup \
  mysql-backup-pre-$(date +%Y%m%d-%H%M)
```

### 3.3 取回备份文件到本地

```bash
# 用临时 pod 把文件 cp 出来
kubectl run dumpfetch --image=busybox:1.36 --restart=Never \
  --overrides='{"spec":{"volumes":[{"name":"b","persistentVolumeClaim":{"claimName":"mysql-backup-pvc"}}],"containers":[{"name":"c","image":"busybox:1.36","command":["sleep","3600"],"volumeMounts":[{"name":"b","mountPath":"/backup"}]}]}}'

kubectl cp dumpfetch:/backup/dump/dump-20260515-0317.sql.gz ./dump-20260515.sql.gz
kubectl delete pod dumpfetch
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

# 3. 起一个影子 MySQL pod(不要污染生产)
kubectl run mysql-shadow --image=mysql:8.0 \
  --env="MYSQL_ROOT_PASSWORD=shadow" -- mysqld

# 4. 灌 full dump
zcat dump-20260515-0317.sql.gz | kubectl exec -i mysql-shadow -- \
  mysql -uroot -pshadow

# 5. 重放 binlog 到目标时间(从 dump 记录的位置开始)
for f in mysql-bin.000042 mysql-bin.000043 mysql-bin.000044; do
  cat /backup/binlog/$f
done | mysqlbinlog \
  --start-position=1234567 \
  --stop-datetime="2026-05-15 14:23:00" \
  - | kubectl exec -i mysql-shadow -- mysql -uroot -pshadow

# 6. 验证影子库数据合理(查关键表)
kubectl exec mysql-shadow -- mysql -uroot -pshadow \
  -e "SELECT COUNT(*) FROM game.guild;"

# 7a. 若仅是抽取单个玩家:从影子库 dump 该玩家行 → 导回生产
# 7b. 若是全库回档:停生产 mysql,把影子 PVC 推上去
```

### 4.2 单玩家 / 单 zone 数据抽取(更常见)

合服失败 / 客服补单这种场景,只想从 PITR 影子库抽某玩家的几行,**不要替换整库**。

```bash
# 在影子库上 dump 单玩家
kubectl exec mysql-shadow -- mysqldump -uroot -pshadow \
  --where="player_id=12345678" \
  game player_data player_inventory player_currency \
  > player-12345678-recovered.sql

# 在生产库 review 后再灌
kubectl exec -i deploy/mysql -- mysql -uroot -pMmorpg#2026db < player-12345678-recovered.sql
```

---

## 5. 与其他流程的衔接

### 5.1 合服前(server_merge_design.md §5.1)

**强制**:在 `k8s-zone-down` source/target 之后,手动触发一次备份,再开始合服:

```bash
kubectl create job --from=cronjob/mysql-backup \
  mysql-backup-pre-merge-$(date +%s)
# 等 Job 完成
kubectl wait --for=condition=complete --timeout=2h \
  job/mysql-backup-pre-merge-<stamp>
```

### 5.2 zone 灾难回档(zone_data_rollback.md §3)

§3 列的「步骤 2 MySQL PITR」直接用本文 §4.1 的流程。**先恢复到影子库**,确认无误后再决定换正式库还是抽数据。

---

## 6. 故障排查

| 症状 | 原因 | 处置 |
|---|---|---|
| `kubectl get cronjob` 显示 `LAST SCHEDULE` 为空 | controller-manager 挂了 / 时区配错 | 看 kube-controller-manager 日志 |
| Job pod 起来但秒退 | mysqldump 连不上 mysql Service | `kubectl exec` 进容器 `mysql -h mysql -uroot -p<pw> -e "SELECT 1"` |
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
| 用 Secret 替代明文密码 | 高 | 现在 `MYSQL_PASSWORD` 是明文,任何能 `kubectl get cronjob -o yaml` 的人都能拿到 |
| 多副本 / 主从复制 | 低 | 当前 single-instance + Recreate 策略,合服 / 大改动时有几分钟 downtime |

---

## 8. Changelog

- **2026-05-15 v1**: 初版。落地 `mysql.yaml`(PVC + binlog 配置)+ `mysql-backup-cronjob.yaml`(每日 dump + binlog)。

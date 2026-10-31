# etcd MVCC 压缩与空间维护 Runbook

> **状态**:v1 — 2026-10-15,P0-C 阶段 1a + 1b 落码,**未验证**(待 Codex 执行,见 §9)。
> **v2 — 2026-10-30**:阶段 2 的清单和契约测试已写(负责人 10-30「一起写进文档一起做」),**未验证**。项目未上线,没有要保护的存量集群;首次部署之前先过 §8 的前置。镜像 tag@摘要还没钉,要 Codex 实测 M0。
> v1.1 — 同日审查修订:§2 补 K8s 探针让 NOSPACE 扩大成成员反复重启;§5 补 defrag 时限与「等 Job 结束」的判法;§7 补时间窗、应急改探针(须负责人批准)与告警在时的 defrag 判据;§8、§9 同步。
> **范围**:K8s 的 etcd(`deploy/k8s/manifests/infra/etcd.yaml` + `etcd-maintenance.yaml`)与本机 / Linux 预发共用的 compose etcd(`deploy/docker-compose.yml`)的历史压缩、碎片整理、空间告警处置。
> **依据**:`docs/handoff/global-singletons-p0-findings-20261014.md`「## P0-C」(证据链、推算与原设计);C++ etcd 流重建是 `docs/design/grpc-muduo-transport.md` §10.2 第 2 批。
> **契约测试**:`tools/scripts/tests/etcd_maintenance_contract.tests.ps1`(静态)、`tools/scripts/tests/k8s_deploy_contract.tests.ps1` 的「infra-up 产物里有 etcd-compact」。

---

## 1. 现状一览

| 环境 | 压缩 | 配额 | 备注 |
|---|---|---|---|
| K8s(`infra-up` 部署) | etcd 自动压缩,保留 1000000 个 revision;CronJob `etcd-compact` 每 5 分钟再压一次(稳定一个版本后删,§8 步骤 7) | 3GiB | 探针已排除 NOSPACE |
| K8s(有真实存档、未重跑 `infra-up`) | **没有**,直到按 §4.2 单独 apply | 同上 | 不要为它跑 `infra-up` |
| compose(本机开发 / Linux 预发) | env 自动压缩,保留 1000000 个 revision | 3GiB | **只对新建的容器生效**(§6),镜像是否接受 env 待实测 |

defrag 在任何环境都不自动做,只按 §5 手工逐成员做。

## 2. 原理

- **为什么会涨**:etcd 是多版本存储,每次写入都产生一个新 revision,旧版本一直留着,直到被**压缩**(compact)。本仓库有持续写入方:
  Go `snowflakealloc` 每个持有者每秒一笔水位 Txn(login / scene_manager / match / guild),login 另有每秒一笔毫秒水位,过渡期
  每笔水位 Txn 镜像写新旧两个 key,gate 每 10s 上报一次人数。K8s 默认档推算约 **每 zone 6.2 revision/s,另加全局池约 2/s**。
- **压缩不等于缩小文件**:compact 只把旧版本标成可回收,库文件(bbolt)里的空闲页留给以后复用,文件大小不变;
  **defrag** 才会重写文件、把空间还给文件系统。所以首次在已经涨大的环境上压缩之后,必须紧跟逐成员 defrag(§5)。
- **配额与 NOSPACE**:库文件超过配额(`--quota-backend-bytes`,默认约 2GiB;按物理文件大小判断 —— 外部常识,待实测)时,
  集群拉起 `NOSPACE` 告警并进入只读 + 删除的维护态,告警**不会自动解除**。后果分项:
  - 立即失败:新节点注册、失租重注册、gate 人数上报、全部发号水位 Txn(扩容、滚动发布、崩溃恢复都受阻);
  - 暂不受影响(**只对 compose 的裸 etcd 成立**,K8s 见下一条):已注册且租约仍在续的节点、读与 watch;login 的 PlayerId
    (K8s 默认走号段,不回退 snowflake);
  - **K8s 上会扩大成成员反复重启**(按现有 `etcd.yaml` 探针与上游 `/health` 语义推断,待 §9 探针 A 实测):readiness 的 `/health`
    与 liveness 的 `/health?serializable=true` 都先查告警,有未排除的告警就返回 503 —— `serializable` 只跳过 leader / 一致读检查,
    跳不过告警,只有 `?exclude=NOSPACE` 能跳过。告警经 raft 落到每个成员并持久化,所以约 15s 后三个成员同时 NotReady、Service `etcd`
    没有端点(新连接被拒);约 60s 后(10s × 6)liveness 开始重启成员,重启后告警还在,进入 CrashLoopBackOff,quorum 时有时无,
    所有 watch、租约流与 C++ etcd 双向流都会断。处置的时间窗与应急办法见 §7;
  - 约 2 小时后:仍用 snowflakealloc 发号的 scene_manager / match / guild 因水位过旧拒发(`defaultFenceAfter = 2h`,推断)。
- **压缩与 watch**:从已压缩的 revision 起 watch 会被服务端取消。Go 侧所有 watch 循环出错即全量重同步(推断安全);
  C++ 每 2s 用 Range 刷新续 watch 起点,保留量在分钟级以上时几乎不会撞上。C++ 的 DELETE 处理依赖 `prev_kv`、
  不处理 `compact_revision`,这两条缺口归 C++ etcd 流重建(§10.2 第 2 批),不在本手册。

## 3. 取值(负责人 2026-10-15 定,取值待实测 / 压测复核)

### 3.1 保留量 KEEP_REVISIONS = 1000000

历史窗口 = 保留量 ÷ 写入速率(速率按 `6.2 × zone 数 + 2` revision/s 推算,未实测):

| zone 数 | revision/s(推算) | 历史窗口 |
|---|---|---|
| 1 | 约 8.2 | 约 34 小时 |
| 10 | 约 64 | 约 4.3 小时 |
| 100 | 约 622 | 约 27 分钟 |

- 常态占用约 `KEEP × 每 revision 字节数`;每 revision 字节数未实测(M2),按 200~400 字节估是 200~400 MB。
- 正确性不依赖这个窗口(C++ 起点每 2s 刷新,Go 出错即重同步),它只决定余量和日志噪声。**扩 zone 时同步调大**。
- 改值要**同批改所有出现处**:`etcd-maintenance.yaml` 的 `KEEP_REVISIONS`、`deploy/docker-compose.yml` 的
  `ETCD_AUTO_COMPACTION_RETENTION`,阶段 2 之后还有 `etcd.yaml`。契约测试断言它们相等,且不小于 100000。

### 3.2 配额 3GiB(3221225472)

- 判据:`配额 × 2 + 1Gi ≤ PVC`(etcd.yaml 的 `volumeClaimTemplates` 是 8Gi,且不能在线改),给 defrag 的临时文件留余量。4GiB 不满足。
- compose 和 K8s 的 etcd 都设了(2026-10-30 起)。

## 4. K8s:CronJob `etcd-compact`

### 4.1 它做什么

`deploy/k8s/manifests/infra/etcd-maintenance.yaml`:ConfigMap `etcd-maintenance-scripts`(脚本 `compact.sh`)+ CronJob `etcd-compact`,
标签一律 `app: etcd-maintenance`(**不能**是 `app: etcd`,否则会被 etcd 的 Service / PDB / StatefulSet 选中)。每 5 分钟:

1. 线性一致读一个不存在的键,取应答头里的 revision(读失败或解析不出就退出 1,不盲压);
2. `current - KEEP_REVISIONS > 0` 时压缩到这个位置,否则打印 `nothing to compact`;对已压缩过的位置再压视为成功;
3. 打印 `endpoint status` 表(某个成员不可达只打 WARN,不让任务失败);
4. 查 `alarm list`:查询失败退出 1;有活动告警时以 2 退出(压缩照常做了,恢复要人按 §7 处置)。

| 退出码 | 含义 | 处置 |
|---|---|---|
| 0 | `compacted to revision …` / `already compacted` / `nothing to compact`,且没有告警 | 无 |
| 1 | 读不到 revision、压缩失败、查不到告警 | 看 Job 日志;多半是 etcd 不可达或端点占位没替换 |
| 2 | 有活动告警(多半是 `NOSPACE`) | 按 §7 |

`concurrencyPolicy: Forbid`、`backoffLimit: 0`、`restartPolicy: Never`、`failedJobsHistoryLimit: 5`:失败不重试、留痕,下一轮就是重试。
仓库里还没有 etcd 的抓取与告警规则(设计里的 `etcd-alerts.yaml` 未落),**目前只能靠 Job 状态与 `infra-status` 发现压缩停了**。

镜像与 etcd.yaml 逐字相同(`bitnamilegacy/etcd:latest` + `IfNotPresent`);钉 tag@摘要待 Codex 实测 M0 后与 etcd.yaml、compose 同批改(§8)。
kind 上 Pod 若卡在拉镜像,说明调度到的节点没有这个镜像,保留 `kubectl describe pod` 输出后按 kind 的镜像载入方式处理。

### 4.2 部署

- **新环境 / 可以重跑 infra-up 的环境**:`k8s_deploy.ps1 -Command infra-up` 在 `etcd.yaml` 之后 apply 它(`Apply-Infra` 的 `$infraManifests`)。
  带 `-SkipInfra` 的 zone-up / all-up 不会部署它。
- **有真实存档的环境:不要为它跑 `infra-up`**。`infra-up` 会连带 apply `mysql.yaml`,而 L-04 部署前不得对有真实存档的环境这样做
  (`docs/handoff/godb-pipeline-handoff-20261013.md` §2.4)。只 apply 这一个清单;它带 `__INFRA_NAMESPACE__` 占位,要先替换:

  ```powershell
  $ctx = '<context>'; $ns = 'mmorpg-infra'
  (Get-Content -Raw deploy/k8s/manifests/infra/etcd-maintenance.yaml).Replace('__INFRA_NAMESPACE__', $ns) |
      kubectl --context $ctx apply -n $ns -f -
  ```

  ```bash
  NS=mmorpg-infra
  sed "s/__INFRA_NAMESPACE__/$NS/g" deploy/k8s/manifests/infra/etcd-maintenance.yaml | kubectl --context <context> apply -n "$NS" -f -
  ```

  生产 / 有存档环境由负责人执行;AI(Claude / Codex)不碰生产集群。apply 它不会滚动 etcd(只新增 ConfigMap 与 CronJob)。
- 撤掉:`kubectl --context $ctx -n $ns delete cronjob etcd-compact; kubectl --context $ctx -n $ns delete configmap etcd-maintenance-scripts`。

### 4.3 检查与手工触发

```powershell
kubectl --context $ctx -n $ns get cronjob etcd-compact                 # LAST SCHEDULE 应在 5 分钟内
kubectl --context $ctx -n $ns get jobs -l app=etcd-maintenance         # 有 Failed 就看日志
kubectl --context $ctx -n $ns logs job/<Job 名>
# 手工触发一次(名字自取,不能与已有 Job 重名)
kubectl --context $ctx -n $ns create job --from=cronjob/etcd-compact etcd-compact-manual-$(Get-Date -Format yyyyMMddHHmmss)
kubectl --context $ctx -n $ns wait --for=condition=complete job/<上一步的名字> --timeout=120s
# 注意:有活动告警(NOSPACE)时 Job 照常压缩但以 2 结束、状态是 Failed,上面这条 wait 必然超时;那时按 §5.3 第 1 步看日志判结果。
```

`k8s_deploy.ps1 -Command infra-status` 也会列出 `cronjob` 与 `job`。

## 5. 日常巡检与手工 defrag(逐成员)

### 5.1 巡检

```powershell
kubectl --context $ctx -n $ns exec etcd-0 -- etcdctl endpoint status --cluster -w table   # DB SIZE、IS LEADER、REVISION 等列,列名随版本不同
kubectl --context $ctx -n $ns exec etcd-0 -- etcdctl endpoint health --cluster
kubectl --context $ctx -n $ns exec etcd-0 -- etcdctl alarm list                            # 空 = 无告警
```

DB SIZE 超过配额一半就该安排 defrag;超过 80% 按紧急处理(阈值是建议值,待接上监控后复核)。

### 5.2 defrag 规则

defrag 会阻塞所在成员(期间该成员不服务读写),所以:

- **手工、逐成员**做,**不要**用 `--cluster` 一次做完;
- 先压缩(§4.3 手工触发一次,等 Job 结束并从日志确认压过了,见 §5.3 第 1 步),否则 defrag 腾不出空间;
- 做之前 3/3 健康;做完等该成员恢复健康、Pod `READY 1/1` 再做下一个(NOSPACE 处置时这两道门槛过不去,改用 §7 第 4 步的判据);
- **每个成员的 defrag 要在约 50 秒内做完**(推断,待实测):defrag 期间该成员持有后端读锁,同一成员的 liveness
  (`/health?serializable=true`,超时 5s、连续 6 次失败)会被卡住,约 50~60 秒后 kubelet 杀掉该成员 —— 这正是阶段 2 前置要避免的成员重启。
  只在压缩之后做:压缩后 defrag 只拷活数据,保留量对应的约 200~400MB 通常远用不了这么久(每 revision 字节数待 M2);活数据很大时先停下来找负责人;
- 先做 follower,leader 最后(常见做法,减少 leader 切换);
- 开服窗口与高峰期不做;
- 做之前确认磁盘余量:defrag 需要大约一份库文件大小的临时空间(外部常识),`kubectl exec etcd-N -- df -h /bitnami/etcd`。

### 5.3 命令

```powershell
# 1. 先压缩:按 §4.3 手工触发 etcd-compact,轮询到 Job 结束(Complete 或 Failed 都算结束),再看日志判结果。
#    不要只 wait condition=complete:有活动告警时 Job 以 2 结束、状态是 Failed,那条 wait 必然超时。
kubectl --context $ctx -n $ns get job <上一步的名字> -o jsonpath='{.status.conditions[*].type}'   # 出现 Complete 或 Failed 即结束
kubectl --context $ctx -n $ns logs job/<上一步的名字>
#    有 `compacted to revision` 或 `already compacted` 就算压过了;NOSPACE 下还有一行 `ALARM ACTIVE ...`,后面跟着告警列表
#    (`memberID:... alarm:NOSPACE`)、退出 2,属预期。
#    是 `nothing to compact` 说明这次没有腾出空间:NOSPACE 处置时回 §7 第 3 步,手工压到更近的位置。
# 2. 记录各成员 DB SIZE 与谁是 leader
kubectl --context $ctx -n $ns exec etcd-0 -- etcdctl endpoint status --cluster -w table
# 3. 逐成员,--endpoints 指向成员自己,只整理这一个;大库 defrag 会超过 etcdctl 默认 5s 命令超时,所以显式放宽(取值待实测)
kubectl --context $ctx -n $ns exec etcd-1 -- etcdctl --endpoints=http://127.0.0.1:2379 --command-timeout=120s defrag
kubectl --context $ctx -n $ns exec etcd-1 -- etcdctl --endpoints=http://127.0.0.1:2379 endpoint health
kubectl --context $ctx -n $ns get pod etcd-1
# 4. 依次对其余成员重复,leader 最后
# 5. 再看一遍 DB SIZE 是否下降
kubectl --context $ctx -n $ns exec etcd-0 -- etcdctl endpoint status --cluster -w table
```

## 6. compose(本机开发 / Linux 预发)

这份 compose 也是 Linux 预发压测机的 etcd(`tools/scripts/deploy-staging.sh` 要求 `etcd` 容器在跑),那里没有 CronJob。

### 6.1 env 什么时候生效

`deploy/docker-compose.yml` 的 etcd 服务带三项 env:`ETCD_AUTO_COMPACTION_MODE=revision`、`ETCD_AUTO_COMPACTION_RETENTION=1000000`、
`ETCD_QUOTA_BACKEND_BYTES=3221225472`。

- **只对新建的容器生效**。`tools/scripts/start_game.ps1` 起 compose 带 `--no-recreate`,既有 etcd 容器不会带上新 env,要重建:

  ```powershell
  docker --context desktop-linux compose -f deploy/docker-compose.yml up -d --force-recreate etcd
  ```

  Linux 预发机上去掉 `--context desktop-linux`。数据在命名卷 `etcd-data` 里,重建不丢。
- **重建 = 重启本机 etcd,应与整栈重启一起做**:先停本机开发栈的 C++ / Go / Java 进程,重建 etcd,再整栈起。
  C++ etcd 流重建(`docs/design/grpc-muduo-transport.md` §10.2 第 2 批)修好之前,单独重启 etcd 会让在跑的 C++ 节点进入约 181s 一轮的
  失租重注册循环。
- **未验证**(待 Codex 实测 M3 探针 B / B2):`bitnamilegacy/etcd` 镜像是否把这些 `ETCD_*` 环境变量交给 etcd(若它改用配置文件启动,
  etcd 会忽略环境变量)、已有数据目录重启后是否仍生效。核对办法(指标名凭记忆,待核对):

  ```powershell
  curl.exe -s http://127.0.0.1:2379/metrics | Select-String 'etcd_server_quota_backend_bytes'   # 期望 3.221225472e+09
  ```

### 6.2 env 生效前的手工压缩

```powershell
docker --context desktop-linux exec etcd etcdctl get etcd-maintenance/revision-probe -w json
# 从输出的 "revision":N 取 N;N - 1000000 ≤ 0 就不用压
docker --context desktop-linux exec etcd etcdctl compact <N - 1000000>
docker --context desktop-linux exec etcd etcdctl --command-timeout=120s defrag      # 单成员:defrag 期间整个本机 etcd 停顿,挑空闲时做
docker --context desktop-linux exec etcd etcdctl endpoint status -w table
docker --context desktop-linux exec etcd etcdctl alarm list
```

也可以复用 CronJob 的同一份脚本:把 `etcd-maintenance.yaml` 里 `compact.sh: |` 下面的正文去掉公共缩进存成 `compact.sh`
(契约测试的 `Get-BlockScalar` 就是这个写法),`docker cp` 进容器后
`docker exec -e KEEP_REVISIONS=1000000 -e ETCDCTL_ENDPOINTS=http://127.0.0.1:2379 etcd sh /tmp/compact.sh`。

Git Bash 下,以 `/` 开头的参数(容器内路径、`/mmorpg/killswitch/...` 这类键)要加 `MSYS_NO_PATHCONV=1`,或改用 PowerShell。

## 7. NOSPACE 处置(顺序不能乱)

**K8s 上先看时间窗**(§2:按现有探针,告警一出成员就 NotReady,约 60s 后开始被 liveness 重启并进入 CrashLoopBackOff):
每个成员重启后大约只有 80~90 秒可以 `exec`(liveness 首次探测在启动后 30~40 秒,之后再连续失败 5 个周期 × 10 秒;还要减去 etcd 自身的
启动时间),下面的压缩、defrag、解除告警要抢在窗口里做,被打断就等它下一次起来接着做(压缩与 defrag 可重复执行)。
抢不过来时的应急办法,**须经负责人批准**:

- 只 patch 线上 StatefulSet、**不改仓库里的 `etcd.yaml`**(改仓库文件属阶段 2,理由见本节最后一条;`etcd_maintenance_contract.tests.ps1` 钉住了两个探针的现值,
  会拦):readiness 与 liveness **两个都**加 `exclude=NOSPACE`。只改 liveness 不够:RollingUpdate 先换 `etcd-2`,它的 readiness 仍因告警
  不通过,滚动卡在 `etcd-2`。patch 会按新模板逐个重建成员(此时它们本来就在被重启)。两种 shell 都先写同一份 JSON patch 文件:

  ```bash
  cat > etcd-probe-patch.json <<'EOF'
  [{"op":"replace","path":"/spec/template/spec/containers/0/readinessProbe/httpGet/path","value":"/health?exclude=NOSPACE"},
   {"op":"replace","path":"/spec/template/spec/containers/0/livenessProbe/httpGet/path","value":"/health?exclude=NOSPACE&serializable=true"}]
  EOF
  kubectl --context <ctx> -n <infra> patch statefulset etcd --type=json --patch-file etcd-probe-patch.json
  ```

  ```powershell
  Set-Content -Path etcd-probe-patch.json -Value @'
  [{"op":"replace","path":"/spec/template/spec/containers/0/readinessProbe/httpGet/path","value":"/health?exclude=NOSPACE"},
   {"op":"replace","path":"/spec/template/spec/containers/0/livenessProbe/httpGet/path","value":"/health?exclude=NOSPACE&serializable=true"}]
  '@
  kubectl --context $ctx -n $ns patch statefulset etcd --type=json --patch-file etcd-probe-patch.json
  ```

  `containers/0` 就是 etcd 容器(StatefulSet 里只有这一个容器);patch 前可用
  `kubectl --context $ctx -n $ns get sts etcd -o jsonpath='{.spec.template.spec.containers[0].name}'` 核对是 `etcd`。
- patch 前确认正在跑的版本认 `exclude` 参数:`kubectl --context $ctx -n $ns exec etcd-0 -- etcd --version`(该参数的最低版本凭记忆是 3.5,
  待 §9 探针 A 实测;不认识的版本会忽略它,patch 等于没打)。
- **告警解除后,会 apply `etcd.yaml` 的命令都会把探针改回去并再滚动一次 3 个成员**:`infra-up`、不带 `-SkipInfra` 的 `all-up`
  (含 `dev_tools.ps1` 的 `k8s-infra-up` / `k8s-all-up` / `k8s-release-all`)、单独 apply。Redis 重启闸不管 etcd,没有任何闸会拦。
  **把这两条探针落进仓库也不是免费的**:对打过 patch 的这个集群,仓库模板与线上一致,apply 不产生新的 controller revision、不滚动;
  但用同一份 `etcd.yaml` 部署的**其他环境**(其他生产区、staging、本机 kind)没打过这个 patch,它们下一次跑上面任一条命令就会滚动 3 个成员 ——
  与阶段 2 同一个风险,所以落库同样要等 §8 的前置(C++ etcd 流重建有运行证据),或经负责人批准、逐个环境在全栈停机的维护窗口里做
  (先停 C++ / Go 节点,没有连着的 etcd 流就不怕成员重启)。在此之前:
  - 被 patch 的集群只走 `zone-up` 或带 `-SkipInfra` 的 `all-up`(它们不 apply `etcd.yaml`);确需 `infra-up`(例如改 Redis / Kafka)时,
    它会把探针改回旧值并滚动 etcd,同样放进全栈停机的维护窗口;
  - 仓库里的 `etcd.yaml` 与契约测试保持原样。

1. **确认**:`etcdctl alarm list` 出现 `alarm:NOSPACE`;写入报库空间超限(报错原文待 M3 探针 A 实测)。CronJob 此时以 2 退出。
   暂停扩容与发布(新注册会失败)。
2. **取当前 revision**:`etcdctl get etcd-maintenance/revision-probe -w json`,读应答头的 `"revision"`。
3. **压缩**:手工触发 `etcd-compact`(§4.3;告警在时它照常压缩、以 2 退出,这一步的 2 是预期的),或手工 `etcdctl compact <revision - 1000000>`。
   告警状态下压缩与 defrag 仍被接受(外部常识,待探针 A 实测)。
   若历史本来就不到保留量、压缩腾不出空间,紧急时可临时压到更近的位置(例如 `revision - 100000`);代价是更多 watch 起点落进已压缩区
   (Go 全量重同步;C++ 见 §2 末尾的缺口)。
4. **逐成员 defrag**(§5.3)。**告警在时 §5.2 的两道门槛过不去**:`etcdctl endpoint health` 在告警列表非空时把端点判为
   unhealthy(报 `NOSPACE`),readiness 用的 `/health` 也因告警失败,Pod 永远不会 `READY 1/1`。照字面执行会卡在第一个成员,
   永远走不到第 6 步解除告警。所以本步改用这三条判据,逐个成员确认后再做下一个:defrag 命令成功返回;该成员
   `etcdctl --endpoints=http://127.0.0.1:2379 endpoint status` 能连上;它的 DB SIZE 下降。告警解除之后再回到 §5.2 的常规门槛。
5. **确认空间**:每个成员的 DB SIZE 都明显低于配额。空间没腾出来就解除告警,写入一来马上再次触发。
6. **解除告警**:`etcdctl alarm disarm`,再 `etcdctl alarm list` 确认为空。
7. **验证写入**:`etcdctl put etcd-maintenance/disarm-check ok`,成功后 `etcdctl del etcd-maintenance/disarm-check`。
8. **事后**:看 C++ 节点是否失租重注册、gate 是否摘掉了节点;Go 发号水位恢复写入后 scene_manager / match / guild 的拒发应自行解除(推断);
   再查一次 `etcd-compact` 为什么没挡住(是否在跑、保留量是否过大、写入速率是否超出 §3.1 的推算)。

## 8. 阶段 2:给 etcd 本身开自动压缩与配额

**2026-10-30:步骤 1、3 已落到 `etcd.yaml` 和契约测试,镜像还没钉**(步骤 1、2 的 tag@摘要要 Codex 实测 M0)。项目未上线,没有存量集群要滚动,但下面的前置仍要在首次部署之前跑完:配置写了不等于这个镜像接受。

**前置**(全部满足才能部署):

1. C++ etcd 流重建(`docs/design/grpc-muduo-transport.md` §10.2 第 2 批)修复并经 Codex 验证:删除 / 重启一个 etcd 成员后无节点 fence 且零重注册;
2. M0:取到 `bitnamilegacy/etcd` 的 tag@摘要;各环境逐成员核对正在跑的版本
   (`kubectl get pod etcd-N -o jsonpath='{.status.containerStatuses[0].imageID}'`),钉的版本必须等于正在跑的版本,只许同版本或升级,禁止降级;
3. M3 探针 B / B2 通过:镜像接受 `ETCD_*` env,已有数据目录重启后仍生效;
4. M2 给出每 revision 字节数,复核 §3 的保留量。

**步骤**:

1. `etcd.yaml` 的 StatefulSet:镜像钉到与 compose、`etcd-maintenance.yaml` 逐字相同的 tag@摘要;env 加
   `ETCD_AUTO_COMPACTION_MODE=revision`、`ETCD_AUTO_COMPACTION_RETENTION=1000000`、`ETCD_QUOTA_BACKEND_BYTES=3221225472`(与 compose 同值);
   Pod 模板加 `prometheus.io/scrape` / `port` / `path` 注解(写法见 `deploy/k8s/manifests/go-svc/chat.yaml`);readiness 改
   `/health?exclude=NOSPACE`、liveness 改 `/health?exclude=NOSPACE&serializable=true`(理由见 §2 与 §7,告警时不再把成员摘光、反复重启;
   前提是 §9 探针 A 证实该版本认 `exclude`);更新文件里的注释。
2. compose 与 `etcd-maintenance.yaml` 的镜像同批改成同一个引用。
3. 契约测试:把「阶段 2 未经许可不得提前做」改成「StatefulSet 的三项 env 与 compose 相等、配额 × 2 + 1Gi ≤ PVC」,
   其中钉住的镜像期望值改成新的 tag@摘要,「不得有 `prometheus.io/` 注解」改成断言注解存在,两条探针断言改成 `exclude=NOSPACE` 的新路径
   (§7 应急 patch 过的集群,线上已是新探针,本步对它不会再滚动;其余环境照常滚动)。
4. 滚动:只 apply `etcd.yaml`(同样先替换 `__INFRA_NAMESPACE__`,不跑 `infra-up`,理由同 §4.2);RollingUpdate 逐个替换,
   PDB `minAvailable: 2` 与 readiness 把关。每个成员 Ready 后看 `endpoint health --cluster`,并确认没有 C++ / Go 节点 fence 或重注册。
5. 验证:每个成员的配额指标为 3221225472;压缩 revision 随时间前进。
6. 回滚:去掉 env 再滚一次;已压缩的历史不可恢复,业务不读历史。
7. 稳定一个版本后删除 `etcd-compact`:从 `$infraManifests` 去掉、删清单、改两份契约测试与文档,集群里按 §4.2「撤掉」删除。

## 9. 未验证项(待 Codex)

本手册与对应改动只做了静态编写,没有运行任何命令。

- 两份契约测试的红绿(`etcd_maintenance_contract.tests.ps1`、`k8s_deploy_contract.tests.ps1`);
- `compact.sh` 的行为(临时容器里按 a–f 情形跑:压缩、已压缩、无需压缩、端点不可达退出 1、缺 `KEEP_REVISIONS` 退出非 0、NOSPACE 退出 2);
- 覆盖 `command` 后 `etcdctl` 是否在 PATH 上、是否认 `ETCDCTL_ENDPOINTS`、`-w json` 里 `"revision":` 的写法、「已压缩」的报错原文(M0、探针 A);
- kind 上 apply 后 Job Complete 且 StatefulSet `etcd` 的 `currentRevision` 前后不变(证明没有滚动);
- compose env 是否生效(M3);etcd 默认配额与 NOSPACE 下各类请求的表现(探针 A);每 revision 字节数与真实写入速率(M2、M5)。
- 探针 A 加一步,定论 §2 / §7 的 K8s 推断:在 NOSPACE 的临时容器上依次请求 `/health`、`/health?serializable=true`、
  `/health?exclude=NOSPACE&serializable=true`(例如 `curl -s -o /dev/null -w '%{http_code}' 'http://127.0.0.1:2379/<路径>'`),
  期望依次 503、503、200;并记下 `etcd --version`。另在大库上计时一次 defrag,核对 §5.2 的「约 50 秒」时限。

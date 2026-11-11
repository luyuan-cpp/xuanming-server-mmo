# etcd MVCC 压缩与空间维护 Runbook

> **状态**:v3 — 2026-11-05,P0-C 阶段 2 收尾(负责人 11-05「按最标准的方法做」):etcd 换成官方镜像并按 tag@摘要钉死,压缩只靠 etcd 内置的
> 自动压缩,阶段 1 的压缩 CronJob `etcd-compact` 退役,空间与无主告警落进 `deploy/k8s/etcd-alerts.yaml`。**未验证**(待 Codex 执行,见 §9)。
> 历史:v1(2026-10-15)阶段 1a + 1b 的 CronJob;v1.1 同日审查修订;v2(2026-10-30)`etcd.yaml` 加压缩 / 配额 env、探针排除 NOSPACE、抓取注解。
> **范围**:K8s 的三套 etcd(`deploy/k8s/manifests/infra/etcd.yaml` 主 etcd、`etcd-id-secondary.yaml` 发号器副记录、`etcd-id-registry.yaml` 全局登记处)
> 与本机 / Linux 预发共用的 compose etcd(`deploy/docker-compose.yml`,调试栈 `deploy/debug/compose.game.yml` 同一镜像)的历史压缩、碎片整理、空间告警处置。
> 发号器两套 etcd 的初始化、恢复与演练见 [发号器运维手册](id-issuer-runbook.md),本手册只管它们的空间与镜像。
> **依据**:`docs/handoff/global-singletons-p0-findings-20261014.md`「## P0-C」(证据链、推算与原设计);交接 `docs/handoff/handoff-global-singletons-p0-20261015.md` §8;
> C++ etcd 流重建是 `docs/design/grpc-muduo-transport.md` §10.2 第 2 批。
> **契约测试**:`tools/scripts/tests/etcd_maintenance_contract.tests.ps1`(静态)、`tools/scripts/tests/k8s_deploy_contract.tests.ps1` 的
> 「infra-up 不再下发 etcd-compact」。

---

## 1. 现状一览

| 环境 | 镜像 | 压缩 | 配额 | 探针 |
|---|---|---|---|---|
| K8s 主 etcd(`infra-up`) | `quay.io/coreos/etcd:v3.6.16@sha256:a9c7…a965` | etcd 内置,revision 模式保留 1000000 | 3GiB | 排除 NOSPACE |
| K8s 发号器两套(非 dev 档 / `-HostIdRegistry`) | 同上 | etcd 内置,revision 模式保留 1000 | 512MiB | 未排除(库只有几个键,见 §2) |
| compose(本机开发 / Linux 预发) | 同上 | etcd 内置,revision 模式保留 1000000 | 3GiB | 无 |

- defrag 在任何环境都不自动做,只按 §5 手工逐成员做。
- 告警:`deploy/k8s/etcd-alerts.yaml`(PrometheusRule,需要按注解抓取 2379 端口),见 §4.2。
- 退役(2026-11-05):CronJob `etcd-compact` 与 ConfigMap `etcd-maintenance-scripts`。`infra-up` 会删掉以前 apply 过的残留(§4.3)。

## 2. 原理

- **为什么会涨**:etcd 是多版本存储,每次写入都产生一个新 revision,旧版本一直留着,直到被**压缩**(compact)。本仓库有持续写入方:
  Go `snowflakealloc` 每个持有者每秒一笔水位 Txn(login / scene_manager / match / guild),login 另有每秒一笔毫秒水位,过渡期
  每笔水位 Txn 镜像写新旧两个 key,gate 每 10s 上报一次人数。K8s 默认档推算约 **每 zone 6.2 revision/s,另加全局池约 2/s**。
- **自动压缩怎么做**(etcd 内置,`--auto-compaction-mode=revision`,外部常识):每 5 分钟把历史压到 `最新 revision - retention`。
  这与退役的 CronJob 做的是同一件事、同一个保留量,只是不再需要一个带 shell 的镜像和一份脚本。
- **压缩不等于缩小文件**:compact 只把旧版本标成可回收,库文件(bbolt)里的空闲页留给以后复用,文件大小不变;
  **defrag** 才会重写文件、把空间还给文件系统。所以在已经涨大的环境上第一次压缩之后,必须紧跟逐成员 defrag(§5)。
- **配额与 NOSPACE**:库文件超过配额(`--quota-backend-bytes`,本仓库三套都显式设了;按物理文件大小判断 —— 外部常识,待实测)时,
  集群拉起 `NOSPACE` 告警并进入只读 + 删除的维护态,告警**不会自动解除**。后果分项:
  - 立即失败:新节点注册、失租重注册、gate 人数上报、全部发号水位 Txn(扩容、滚动发布、崩溃恢复都受阻);
  - 暂不受影响:已注册且租约仍在续的节点、读与 watch;login 的 PlayerId(K8s 默认走号段,不回退 snowflake);
  - 主 etcd 的两个探针都带 `exclude=NOSPACE`(2026-10-30 起):告警时成员不会被摘出 Service、也不会被 liveness 反复重启,读和删照常可用
    (上游 `/health` 语义推断,待 §9 探针 A 在 v3.6.16 上实测)。发号器两套没排除:它们的库只有几个键、保留 1000 个版本,NOSPACE 基本不会发生;
    真发生时成员会 NotReady 并被反复重启,处置同 §7,改探针属于发号器那条线;
  - 约 2 小时后:仍用 snowflakealloc 发号的 scene_manager / match / guild 因水位过旧拒发(`defaultFenceAfter = 2h`,推断)。
- **压缩与 watch**:从已压缩的 revision 起 watch 会被服务端取消(`ErrCompacted`),客户端必须全量重新 list 再 watch。换成内置压缩不改变
  压缩的时机与窗口(都是每 5 分钟、保留 1000000),所以不会更常撞上。Go 侧 watch 循环出错即全量重同步(推断安全);
  C++ 的断流重建、重建后 Range 对账与 compacted 处理在 `docs/design/grpc-muduo-transport.md` §10.2 第 2 批的验收条件里,代码已落,**没有运行证据**。

## 3. 取值(负责人 2026-10-15 定,取值待实测 / 压测复核)

### 3.1 保留量 ETCD_AUTO_COMPACTION_RETENTION = 1000000(主 etcd 与 compose)

历史窗口 = 保留量 ÷ 写入速率(速率按 `6.2 × zone 数 + 2` revision/s 推算,未实测):

| zone 数 | revision/s(推算) | 历史窗口 |
|---|---|---|
| 1 | 约 8.2 | 约 34 小时 |
| 10 | 约 64 | 约 4.3 小时 |
| 100 | 约 622 | 约 27 分钟 |

- 常态占用约 `保留量 × 每 revision 字节数`;每 revision 字节数未实测(M2),按 200~400 字节估是 200~400 MB。
- 正确性不依赖这个窗口(watch 撞上已压缩区就全量重同步),它只决定余量和日志噪声。**扩 zone 时同步调大**。
- 改值要**同批改所有出现处**:`etcd.yaml`、`deploy/docker-compose.yml`、`deploy/debug/compose.game.yml`。契约测试断言它们相等,且不小于 100000。
- 发号器两套是另一组取值(保留 1000、配额 512MiB),只有几个键,由发号器那条线定。

### 3.2 配额 3GiB(3221225472)

- 判据:`配额 × 2 + 1Gi ≤ PVC`(etcd.yaml 的 `volumeClaimTemplates` 是 8Gi,且不能在线改),给 defrag 的临时文件留余量。4GiB 不满足。
  发号器两套是 512MiB 配 2Gi 盘,同一判据。契约测试对三套都核。

## 4. K8s:内置自动压缩、告警与退役的 CronJob

### 4.1 自动压缩

`etcd.yaml` 的 env 带 `ETCD_AUTO_COMPACTION_MODE=revision`、`ETCD_AUTO_COMPACTION_RETENTION=1000000`、`ETCD_QUOTA_BACKEND_BYTES=3221225472`。
官方镜像里 etcd 就是 1 号进程,这些 `ETCD_*` 环境变量由 etcd 自己按同名命令行参数解析(etcd 文档「Configuration flags」),没有包装脚本。
确认生效(§9 交给 Codex 实测一次):

```powershell
$ctx = '<context>'; $ns = 'mmorpg-infra'
kubectl --context $ctx -n $ns logs etcd-0 | Select-String 'auto-compaction|quota-backend-bytes' | Select-Object -First 5   # 启动日志里打出的配置
kubectl --context $ctx get --raw "/api/v1/namespaces/$ns/pods/etcd-0:2379/proxy/metrics" | Select-String '^etcd_server_quota_backend_bytes'   # 期望 3.221225472e+09
```

### 4.2 告警

`deploy/k8s/etcd-alerts.yaml`(与其他告警文件同法安装:`kubectl apply -n observability -f deploy/k8s/etcd-alerts.yaml`)。
规则照 etcd 官方 mixin 写:无主(critical)、库文件到配额 80%(warning,本仓库加的一档)/ 95%(critical)、4 小时内会写满(warning)、
碎片超过一半(warning,该 defrag 了)、频繁换主、提案失败、fsync / 提交延迟。三套 etcd 的 Pod 模板都带 `prometheus.io/*` 抓取注解。
没有按注解抓取的配置(或 PodMonitor)时这些规则没有数据,空表达式不报警,和「一切正常」长得一样 —— 装完先在 Prometheus 里查一次
`etcd_server_has_leader` 有三条(每套)。

### 4.3 退役的 CronJob `etcd-compact`

阶段 1 的 CronJob 是在「不能改 `etcd.yaml`」的前提下补的压缩(C++ etcd 流重建前不让 etcd 滚动)。阶段 2 把压缩开进了 etcd 本身,
官方镜像又没有 shell(CronJob 的 `compact.sh` 跑不了),两份压缩器同时压同一段历史没有意义,所以撤掉:

- 仓库:删了 `deploy/k8s/manifests/infra/etcd-maintenance.yaml`,`Apply-Infra` 的 `$infraManifests` 不再带它;
- 集群:`infra-up` 在 apply `etcd.yaml` 之前删掉 `cronjob/etcd-compact` 与 `configmap/etcd-maintenance-scripts`(`--ignore-not-found`,
  没有就是空操作),CronJob 留下的 Job 随它级联删除。不跑 `infra-up` 的集群手工删:

  ```powershell
  kubectl --context $ctx -n $ns delete cronjob etcd-compact --ignore-not-found
  kubectl --context $ctx -n $ns delete configmap etcd-maintenance-scripts --ignore-not-found
  ```

## 5. 日常巡检与手工 defrag(逐成员)

官方镜像没有 shell 和常用命令行工具(distroless):`kubectl exec` 只能直接跑 `etcdctl`(在 PATH 上),不能 `sh -c`、`df`、`curl`。
需要的计算在本机做(下面的 PowerShell)。

### 5.1 巡检

```powershell
kubectl --context $ctx -n $ns exec etcd-0 -- etcdctl endpoint status --cluster -w table   # DB SIZE、IS LEADER、REVISION 等列,列名随版本不同
kubectl --context $ctx -n $ns exec etcd-0 -- etcdctl endpoint health --cluster
kubectl --context $ctx -n $ns exec etcd-0 -- etcdctl alarm list                            # 空 = 无告警
```

DB SIZE 超过配额一半就该安排 defrag;到 80% 时 `EtcdDatabaseQuotaHighUsage` 会响,按紧急处理(阈值是建议值,待压测复核)。
发号器两套把 `etcd-0` 换成 `etcd-id-secondary-0` / `etcd-id-registry-0`。

### 5.2 defrag 规则

defrag 会阻塞所在成员(期间该成员不服务读写),所以:

- **手工、逐成员**做,**不要**用 `--cluster` 一次做完;
- 先压缩(§5.3 第 1 步),否则 defrag 腾不出空间;内置压缩每 5 分钟也会做,手工做一次是为了马上腾出来;
- 做之前 3/3 健康;做完等该成员恢复健康、Pod `READY 1/1` 再做下一个(NOSPACE 处置时这两道门槛过不去,改用 §7 第 4 步的判据);
- **每个成员的 defrag 要在约 50 秒内做完**(推断,待实测):defrag 期间该成员持有后端读锁,同一成员的 liveness
  (`/health?exclude=NOSPACE&serializable=true`,超时 5s、连续 6 次失败)会被卡住,约 50~60 秒后 kubelet 杀掉该成员。
  只在压缩之后做:压缩后 defrag 只拷活数据,保留量对应的约 200~400MB 通常远用不了这么久(每 revision 字节数待 M2);活数据很大时先停下来找负责人;
- 先做 follower,leader 最后(常见做法,减少 leader 切换);
- 开服窗口与高峰期不做;
- 做之前确认磁盘余量:defrag 需要大约一份库文件大小的临时空间(外部常识)。容器里没有 `df`,看 kubelet 的卷指标
  `kubelet_volume_stats_available_bytes{persistentvolumeclaim="etcd-data-etcd-N"}`;集群没接 kubelet 指标时,用临时调试容器
  `kubectl --context $ctx -n $ns debug -it etcd-N --image=<集群可用的 busybox 镜像> --target=etcd -- df -h /proc/1/root/var/lib/etcd`
  (调试容器的镜像按集群的镜像来源准备,不要临时从公网拉)。

### 5.3 命令

```powershell
# 1. 先压缩到「当前 revision - 1000000」。当前 revision 从任意一次读的应答头取(读一个不存在的键,不产生写入)。
$rev = [long]((kubectl --context $ctx -n $ns exec etcd-0 -- etcdctl get etcd-maintenance/revision-probe --consistency=l -w json | ConvertFrom-Json).header.revision)
$target = $rev - 1000000
if ($target -gt 0) { kubectl --context $ctx -n $ns exec etcd-0 -- etcdctl compact $target }   # 报「已压缩」(required revision has been compacted)也算压过了
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

这份 compose 也是 Linux 预发压测机的 etcd(`tools/scripts/deploy-staging.sh` 要求 `etcd` 容器在跑)。

### 6.1 新镜像与参数什么时候生效

`deploy/docker-compose.yml` 的 etcd 服务与 K8s 同一个镜像,带 `ETCD_DATA_DIR=/var/lib/etcd` 与三项 env(`ETCD_AUTO_COMPACTION_MODE=revision`、
`ETCD_AUTO_COMPACTION_RETENTION=1000000`、`ETCD_QUOTA_BACKEND_BYTES=3221225472`)。命名卷 `etcd-data` 改挂在 `/var/lib/etcd`(以前挂在
`/bitnami/etcd/data`,卷根就是数据目录,布局没变),已有数据照用;卷是 bitnamilegacy latest(3.6.1)写的,3.6.16 是补丁升级。

- **只对新建的容器生效**。`tools/scripts/start_game.ps1` 起 compose 带 `--no-recreate`,既有 etcd 容器不会换镜像,要重建:

  ```powershell
  docker --context desktop-linux compose -f deploy/docker-compose.yml up -d --force-recreate etcd
  ```

  Linux 预发机上去掉 `--context desktop-linux`。数据在命名卷 `etcd-data` 里,重建不丢。镜像要先在本机有(没有就要拉,拉之前按 AGENTS 约定先问负责人)。
- **重建 = 重启本机 etcd,应与整栈重启一起做**:先停本机开发栈的 C++ / Go / Java 进程,重建 etcd,再整栈起。
  C++ etcd 流重建(`docs/design/grpc-muduo-transport.md` §10.2 第 2 批)还没有运行证据,单独重启 etcd 可能让在跑的 C++ 节点进入
  约 181s 一轮的失租重注册循环。
- 核对生效:

  ```powershell
  curl.exe -s http://127.0.0.1:2379/metrics | Select-String '^etcd_server_quota_backend_bytes'   # 期望 3.221225472e+09
  docker --context desktop-linux exec etcd etcdctl version                                       # etcdctl version: 3.6.16
  ```

### 6.2 手工压缩与 defrag

容器里没有 shell,`docker exec` 只能直接跑 `etcdctl`:

```powershell
$rev = [long]((docker --context desktop-linux exec etcd etcdctl get etcd-maintenance/revision-probe -w json | ConvertFrom-Json).header.revision)
if ($rev - 1000000 -gt 0) { docker --context desktop-linux exec etcd etcdctl compact ($rev - 1000000) }
docker --context desktop-linux exec etcd etcdctl --command-timeout=120s defrag      # 单成员:defrag 期间整个本机 etcd 停顿,挑空闲时做
docker --context desktop-linux exec etcd etcdctl endpoint status -w table
docker --context desktop-linux exec etcd etcdctl alarm list
```

Git Bash 下,以 `/` 开头的参数(`/mmorpg/killswitch/...` 这类键)要加 `MSYS_NO_PATHCONV=1`,或改用 PowerShell。

## 7. NOSPACE 处置(顺序不能乱)

主 etcd 的探针排除了 NOSPACE(§2),告警期间成员仍在 Service 里、不会被反复重启,下面各步不用抢时间窗。
发号器两套的探针没排除:告警一出它们会 NotReady,约 60 秒后被 liveness 重启,每次起来大约只有 80~90 秒能 `exec`,下面的步骤要抢在窗口里做,
被打断就等它下一次起来接着做(压缩与 defrag 可重复执行);抢不过来时只 patch 线上 StatefulSet 给两个探针加 `exclude=NOSPACE`,须经负责人批准,
仓库里改不改由发号器那条线定。

1. **确认**:`etcdctl alarm list` 出现 `alarm:NOSPACE`;`EtcdDatabaseQuotaLowSpace` 在响;写入报库空间超限(报错原文待 §9 探针 A 实测)。
   暂停扩容与发布(新注册会失败)。
2. **取当前 revision**:§5.3 第 1 步那条 `etcdctl get ... -w json`,读应答头的 `revision`。
3. **压缩**:`etcdctl compact <revision - 1000000>`(发号器两套用 `revision - 1000`)。告警状态下压缩与 defrag 仍被接受(外部常识,待探针 A 实测)。
   若历史本来就不到保留量、压缩腾不出空间,紧急时可临时压到更近的位置(例如 `revision - 100000`);代价是更多 watch 起点落进已压缩区
   (客户端全量重同步,§2 末尾)。
4. **逐成员 defrag**(§5.3)。**告警在时 §5.2 的两道门槛过不去**:`etcdctl endpoint health` 在告警列表非空时把端点判为 unhealthy(报 `NOSPACE`)。
   照字面执行会卡在第一个成员,永远走不到第 6 步解除告警。所以本步改用这三条判据,逐个成员确认后再做下一个:defrag 命令成功返回;
   该成员 `etcdctl --endpoints=http://127.0.0.1:2379 endpoint status` 能连上;它的 DB SIZE 下降。告警解除之后再回到 §5.2 的常规门槛。
5. **确认空间**:每个成员的 DB SIZE 都明显低于配额。空间没腾出来就解除告警,写入一来马上再次触发。
6. **解除告警**:`etcdctl alarm disarm`,再 `etcdctl alarm list` 确认为空。
7. **验证写入**:`etcdctl put etcd-maintenance/disarm-check ok`,成功后 `etcdctl del etcd-maintenance/disarm-check`。
8. **事后**:看 C++ 节点是否失租重注册、gate 是否摘掉了节点;Go 发号水位恢复写入后 scene_manager / match / guild 的拒发应自行解除(推断);
   再查自动压缩为什么没挡住:启动日志里的压缩参数是否生效(§4.1)、保留量是否过大、写入速率是否超出 §3.1 的推算、
   `EtcdDatabaseHighFragmentationRatio` 是否早就在响而没人 defrag。

## 7a. 主 etcd 从快照恢复:按发号安全事件处理

主 etcd 里放着 `snowflakealloc` 的发号水位(`/snowflake/<kind>/c<cluster>/watermark/<slot>` 秒水位、`watermark_ms/<slot>` 毫秒水位,
都**不挂 lease**),login / scene_manager / match / guild 的 worker id 唯一性靠它们和隔离期 Q(缺省 4h)、自 fence 期限 F(= Q/2,缺省 2h)
共同保证:申领者跳过 `now − wm < Q` 的槽,持有者距上次水位写成功超过 F 就停发,不等式 `T_ack + F < wm + Q` 让前任与继任者永不同时发号
(`go/shared/snowflakealloc/allocator.go` 文件头,`docs/design/node-id-overhaul-plan-20260908.md` §3、§5)。从更早的快照恢复会同时破坏两件事:

- **水位回退**:恢复后 etcd 里的 wm 比持有者真正最后写成的值旧。一个旧到超过 Q 的槽会被新进程当成空闲槽立刻申领,而原持有者可能还在用同一个
  worker id 发号,直到它自己 fence(最长 F),两边同时发号就可能撞号。
- **revision 复用**:不带 `--bump-revision` 恢复时 revision 也回退,之后的写会复用旧 revision。失租重挂(reclaim)用水位 key 的 ModRevision
  判断「失联期间有没有别人写过水位」,revision 复用会让这次比较失效,把别人用过的槽重新挂回来。

所以恢复主 etcd 时,二选一,不能两样都不做:

1. **首选:恢复前停掉全部持槽进程**(login、scene_manager、match、guild,以及任何调用 `snowflakealloc` 的工具),恢复完成后再启动。
   没有进程在发号,就没有「前任还在发」的窗口。
2. **停不掉时:恢复后至少等满 F(缺省 2 小时)再允许任何新的持槽进程启动**:这段时间不扩容、不滚动发布、不手动拉起这几类服务。
   仍在运行的持有者要么继续写成水位(槽重新变新,别人申领不到),要么写不成、在 F 内自己 fence 退出,等满 F 之后就不会再有前任在发号。

两种做法都必须这样恢复:`etcdutl snapshot restore <快照> --bump-revision 1000000000 --mark-compacted ...`(与 [搬库手册](db-move-runbook.md)
里映射回退一行同一口径),保证恢复后的 revision 严格大于恢复前任何进程见过的值。恢复之后核对:各服务启动日志里 `[snowflakealloc]` 的申领
都成功、没有 `slot ownership lost`;有的话按「槽被别人接管」处理,该进程已自己停发。

**revision 倒退(换成一套全新的 etcd、revision 从 1 重计,或者没带 `--bump-revision` 从快照恢复)**:除了重启全部节点
(`docs/design/grpc-muduo-transport.md` 的 revision 单调前提),本 zone 控制域 Redis 里的节点号持有者 `node:zone:*:holder`
(scene_manager 写,scene 节点据它认出「自己的节点号已被别的进程接走」,[归属对账](../design/scene-node-ownership-reconcile.md) §4.6)
记着倒退之前的修订号,与新登记不可比。scene_manager 自动处理,不用手工清 Redis:

- 领导者每轮全量同步(约一分钟)先读本 zone 的修订号水位 `scene_manager:etcd_revision_seen:zone:<z>`,再线性一致地读 etcd 当前修订号。
  当前修订号小于水位就换一次 etcd 纪元:`scene_manager:etcd_epoch:zone:<z>` 加一,水位降到当前修订号。ERROR 日志
  `[NodeHolder] etcd revision went back in zone N: current revision C is below the recorded W ...`,计数
  `scene_manager_etcd_revision_regressions_total`,告警 `SceneManagerEtcdRevisionRegressed`(critical)。
- 换纪元之后,快照里在册的号当场改写成新集群里的登记;其余的号旧记录先留着,下一次登记时不比修订号直接覆盖。
- 不要删水位与纪元两个键:删了水位,紧接着的一次倒退认不出来;删了纪元,本 zone 的持有者记录会全部按旧纪元重写一遍(无害,但没必要)。

处置:

1. 计划内的换集群或恢复:换完之后每个 zone 亮一次告警是预期的。核对日志里的当前修订号与水位确实对应这次操作,再按本节重启全部 scene 节点:
   不重启的老进程记着倒退之前的大修订号,新集群里接走它节点号的登记修订号更小,它认不出自己的号已被接走,会一直留着已被改派的场景。
   重启之后逐个核对:每个 scene Pod 的启动时间都晚于换集群的时刻(`kubectl get pods` 的 AGE,或进程日志里的启动行),漏掉的补重启。
2. 不是计划内的:先查清 scene_manager 连的是哪一套 etcd(配置、DNS、负载均衡),是不是有人把 etcd 指到了别的集群或旧备份;发号器的水位
   是否也跟着回退了(按本节开头的「水位回退」处理)。
3. 带 `--bump-revision` 恢复时 revision 不倒退,不会换纪元:恢复之后的登记修订号都更大,持有者记录照旧单调,不用处理。
4. 计划内的换集群之后,某个 zone 一直没亮告警:先看那个 zone 有没有 scene_manager 领导者(没有就等选出来,它的第一轮全量同步会认出倒退)。
   有领导者仍不亮,多半是那个 zone 的水位丢了(控制域丢过数据或有人删了键),这次倒退认不出来。确认 etcd 确实换过之后,在那个 zone 的
   控制域实例上手工换纪元:
   ```
   redis-cli -h <控制域实例> INCR scene_manager:etcd_epoch:zone:<z>
   ```
   效果与自动换纪元相同:该 zone 全部持有者记录让位给下一次登记,领导者下一轮全量同步把在册节点的记录改写成新集群里的登记。
   多做一次也只是全部记录重写一遍,不会误判排空([归属对账](../design/scene-node-ownership-reconcile.md) §9 第 9 条)。

修正之前 scene 节点只会认不出被接走,不会误排空。

发号器(data_service `AllocateIdSegment`)的两套独立 etcd 与全局号源登记处是另一回事,恢复与初始化只走 [发号器运维手册](id-issuer-runbook.md),
**登记处不许从备份恢复**(AGENTS.md §7 不变量 1)。

## 8. 阶段 2(2026-11-05 完成落码)与部署

**落了什么**:`etcd.yaml`(10-30 起已有压缩 / 配额 env、抓取注解、排除 NOSPACE 的探针)换成官方镜像 `quay.io/coreos/etcd:v3.6.16`,
按 Quay 上清单列表的摘要钉死,显式 `command: ["/usr/local/bin/etcd"]`,去掉 bitnami 专用的 `ALLOW_NONE_AUTHENTICATION`,数据目录从
`/bitnami/etcd/data` 改到 `/var/lib/etcd/data`(PVC 挂载点从 `/bitnami/etcd` 改到 `/var/lib/etcd`,`data/` 子目录不变,已有数据照用);
发号器两套与两份 compose 同批换成同一个镜像引用;CronJob 退役(§4.3);告警文件(§4.2)。

**为什么是这个镜像和版本**(依据:2026-11-05 查 Docker Hub 与 Quay 的 API):

- `bitnamilegacy/etcd` 是 Bitnami 停止免费发布之后的归档仓库,`latest` 停在 3.6.1(2025-07-02),之后不再有任何修复;它的启动脚本会不会把
  `ETCD_*` 交给 etcd 也一直没有实测(旧 §6.1 / M3)。官方镜像只有 etcd / etcdctl / etcdutl 三个二进制,没有这层不确定。
- 3.6 线最新补丁 v3.6.16(2026-10-22)。线上跑的是 3.6.1,升 3.6.16 是补丁升级、可逐成员滚动;不选 3.5(3.6 写过的数据目录不能靠换镜像退回 3.5);
  3.7 在 2026-07 才发布,暂不跟。Go 客户端 3.5.15 / 3.6.5 都兼容 3.6 服务端。
- 摘要是 Quay 的 manifest list 摘要 `sha256:a9c7cc12156f4b26ca41f0b57ba5b76c45037ff6c1e47ea3d68cb7908d48a965`。gcr.io/etcd-development/etcd
  同一个 tag 的摘要不同(两个仓库各自推送),所以所有清单只写 quay.io,不要混用两个仓库。拉取之前按 §9 复核一次摘要。

**部署前置与后果**:

1. **全新集群(未上线前的首次部署)**:没有存量 etcd 要滚动,直接 `infra-up`。
2. **已有集群**:换镜像 = 改 Pod 模板,apply `etcd.yaml` 会逐个滚动 3 个成员(PDB `minAvailable: 2` 与 readiness 把关,quorum 一直在)。
   - 先核对每个成员正在跑的版本:`kubectl --context $ctx -n $ns exec etcd-N -- etcd --version`。是 3.6.x 就直接滚;是 3.5.x 时,3.5 → 3.6 要求
     所有成员先到 3.5.32 或更高(etcd 官方升级说明),先换 `quay.io/coreos/etcd:v3.5.35` 滚一遍再上 3.6.16。dev 集群更简单的做法是
     `infra-down` 后重建(etcd 里只有租约绑定的注册与发号槽位,节点重启会重新申领)。
   - C++ etcd 流重建(P0-D,`grpc-muduo-transport.md` §10.2 第 2 批)还没有「删 / 重启一个成员后无 fence 且零重注册」的运行证据。
     有 C++ 节点在跑的集群,这次滚动放进全栈维护窗口(先停 C++ / Go 节点再 `infra-up`),或者等 P0-D 的故障注入验证通过之后再做;
     滚动本身就是那项验证的现成场景。
   - 发号器两套同样滚动;它们不可用时新块预留失败,但手里的块还能用很久(发号器运维手册)。
3. **回滚**:把镜像改回原来的引用再滚一次(只在同一小版本内回滚;3.6 之后不能靠换镜像回 3.5)。已压缩的历史不可恢复,业务不读历史。

## 9. 未验证项(待 Codex)

本手册与对应改动只做了静态编写,没有运行任何命令。

- 契约测试的红绿:`etcd_maintenance_contract.tests.ps1`、`k8s_deploy_contract.tests.ps1`(「infra-up 不再下发 etcd-compact」)、
  `id_issuer_deploy_contract.tests.ps1`(三套 etcd 同一镜像)。
- 摘要复核(只读,不拉镜像):`docker buildx imagetools inspect quay.io/coreos/etcd:v3.6.16`(或 `crane digest`),顶层 Digest 应等于清单里写的
  `sha256:a9c7…a965`;对不上就以实测为准改所有出现处,并在 PROGRESS 记一笔。
- kind 上:新集群 `infra-up`,三个成员 Ready;启动日志里有 `auto-compaction-mode` = revision、retention 1000000、`quota-backend-bytes` 3221225472(§4.1);
  `/metrics` 里有 `etcd-alerts.yaml` 用到的全部指标名;`kubectl exec etcd-0 -- etcdctl endpoint status --cluster -w table` 能跑(证明 exec 形式不依赖 shell);
  从 bitnamilegacy 的旧集群滚到新镜像时成员以原身份、原数据回到集群(`member list` 的 ID 不变),并记录 C++ / Go 节点有无 fence 或重注册(P0-D 证据)。
- 探针 A(定论 §2 / §7 的推断):在 NOSPACE 的临时 etcd(官方镜像 v3.6.16)上依次请求 `/health`、`/health?serializable=true`、
  `/health?exclude=NOSPACE&serializable=true`,期望依次 503、503、200;另在大库上计时一次 defrag,核对 §5.2 的「约 50 秒」时限。
- 每 revision 字节数与真实写入速率(M2、M5),复核 §3 的保留量。

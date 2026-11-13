# K8S KNOWLEDGE BASE

## OVERVIEW
`deploy/k8s/` is the Kubernetes-only release path for game zones. It is separate from local Docker Compose and assumes Linux runtime artifacts.

## STRUCTURE
```text
deploy/k8s/
├── manifests/         # Infra and workload manifests
├── runtime/           # Staged runtime files for Linux image
├── Dockerfile.cpp     # C++ 节点镜像:容器内全量编译(k8s-build-all / cpp-build-ci)
├── Dockerfile.runtime # C++ 节点镜像:打包预编译 staging(k8s-build-image / k8s-release-* / publish_images)
├── zones.*            # One-click zone config examples / ops presets
├── e2e/               # kind 上的端到端核对夹具,部署脚本不读(log-collection-check.yaml:日志采集)
└── README.md          # Authoritative flow description
```

## WHERE TO LOOK
| Task | Location | Notes |
|------|----------|-------|
| End-to-end flow | `README.md` | One-zone/all-zone operations |
| Runtime staging layout | `runtime/README.md` | Required Linux file layout |
| C++ 节点镜像 | `Dockerfile.cpp` / `Dockerfile.runtime` | 两条现役路径,分工见下方「C++ 节点镜像:两条路径」;都不是根 `Dockerfile` |
| 发布打包 | `tools/scripts/publish_images.ps1` 等 | 见下方「发布打包」 |
| Infra manifests | `manifests/infra/` | Shared infra (etcd/redis/kafka/mysql) deployed to `mmorpg-infra` namespace. etcd is a 3-replica StatefulSet + PVC + PDB on the official `quay.io/coreos/etcd` image pinned by digest, compacted by etcd's built-in auto-compaction (the old `etcd-compact` CronJob is retired; non-dev profiles add the issuer's `etcd-id-secondary`, `-HostIdRegistry` adds `etcd-id-registry`); redis and kafka are StatefulSets + PVC (core redis: single instance `redis.yaml` in the dev release profile with the custom ops profile, otherwise 1 primary + 2 replicas + sentinels + 2 proxies `redis-ha.yaml`, overridable by `-RedisReplicas`; `-RedisSplit queue` adds one `redis-queue-g<N>` per zone group for login queue keys and gateway rate-limit buckets, in the same shape as the core redis (`redis-purpose.yaml` or `redis-purpose-ha.yaml`, same `-RedisReplicas`); `-PlayerRedisMasters` adds the player-domain Redis Cluster `redis-player` from `redis-player-cluster.yaml`, required in staging and prod; kafka brokers: 1 in the dev release profile with the custom ops profile, otherwise 3, overridable by `-KafkaBrokers`), mysql is still a single-replica Deployment |
| C++ 日志采集 | `manifests/infra/loki.yaml` + `k8s_deploy.ps1` 的 `New-CppLogSidecar*` | Linux 下 muduo **不写 stdout**,所以每个 C++ Pod 带一个 Alloy **原生 sidecar**(放在 `initContainers` 里 + `restartPolicy: Always`),共享 `node-logs` 卷只读读 `/app/bin/logs/cpp_nodes/*.log` 送 Loki。默认开;`-NoCppLogSidecar` 关,`-CppLogSidecarImage` / `-LokiPushUrl` 换镜像与目标 —— 三个开关 `dev_tools.ps1` / `k8s_image.ps1` 都已透传,不必绕回 `k8s_deploy.ps1`。**原生 sidecar 的代价**:sidecar 镜像拉不到时业务容器**一次都不会启动**,Pod 卡在 `Init:ErrImagePull` / `Init:ImagePullBackOff`,`kubectl describe pod` 的报错在 **Init Containers** 一节;kind 上要先把镜像载入节点,或用 `-NoCppLogSidecar`。**sidecar 与 Loki 分属两条部署路径**:`k8s-infra-up` **只在 sidecar 开着且 `-LokiPushUrl` 留空时**才把 Loki 清单(`loki.yaml`,3 副本时 `loki-ha.yaml`)加进 infra 清单(见 `Apply-Infra` 的 `$infraManifests`),其余情况 infra-up 根本不部署 Loki;只跑 `k8s-zone-up` 或带 `-SkipInfra` 会得到「采了推不出去」的半配置状态(脚本只 `Write-Warning`,部署照样成功)。反向也成立:**在真部署过 Loki 的集群上**,`k8s-infra-down` / `k8s-all-down`(不带 `-SkipInfra`)删整个 infra namespace,`loki-data-pvc`(10Gi)和全部已采日志一起没。Agones Fleet 里那行 `spec.template.spec.container` 现在是**冗余保留**:sidecar 移进 `initContainers` 后 `containers` 只剩 1 个,Agones 自己认得出;留着是为了将来再加第二个普通容器时不重踩「多容器 Fleet 不写这行会被 Agones 拒掉」的老坑,别顺手删。(表里 `k8s-*` 是下一行 `dev_tools.ps1` 的命令名;直接调 `k8s_deploy.ps1 -Command` 时去掉 `k8s-` 前缀。)详见 `docs/ops/grafana-loki-local-logs.md` §6 |
| 容器日志采集(Go / Java、C++ 容器的 stdout / stderr) | `manifests/infra/pod-log-collector.yaml` + `k8s_deploy.ps1` 的 `New-PodLogCollectorManifestText` | 读 `/var/log/pods` 的 Alloy DaemonSet(2026-11-12,未运行),`k8s-infra-up` 部署进 infra namespace,与 C++ sidecar **同开同关**(`-NoCppLogSidecar` 两路一起关)、**推同一个地址**(集群内 `loki:3100` 或 `-LokiPushUrl`,地址带凭据在写路径入口拒绝)、用同一个 Alloy 镜像。只采清单标了 `tier: go-svc` / `java-svc` 的 Pod,以及没有 `tier`、`app` 是 gate / scene / scene-world / scene-instance / battle(金丝雀加 `-canary`)的 C++ Pod:**新加 C++ 工作负载时它的名字要能被那条正则认出来**(契约测试 `k8s_log_collection_contract` 按部署脚本里的工作负载名核对),**新加 Go / Java 清单要带 `tier` 标签**,否则日志没人采。解析规则照搬 `deploy/observability/alloy/config.alloy`,两边逐段一致由同一份契约测试核对,时间用 CRI 时间戳。以 root 只读挂载 hostPath,Pod Security Admission 的 baseline / restricted 会拒;ClusterRoleBinding `mmorpg-pod-log-reader-<infra>` 由 `infra-down` 按名字删。只随 `infra-up` 部署,存量集群要重跑一次。详见 `docs/ops/grafana-loki-local-logs.md` §6.11 |
| Script entrypoint | `tools/scripts/dev_tools.ps1` | `k8s-*` commands drive this subtree |

## CONVENTIONS
- This subtree is Kubernetes-only; do not mix docker-compose/local process assumptions into it.
- C++ 节点镜像只从 `deploy/k8s/Dockerfile.cpp` 或 `deploy/k8s/Dockerfile.runtime` 出,分工见「C++ 节点镜像:两条路径」。发布门禁(不可变 tag + `release_preflight.ps1`)挂在部署侧:`k8s_deploy.ps1 -ReleaseProfile staging|prod` 的 `zone-up` / `all-up` / `infra-up` 对任意 `-NodeImage` 都查,与镜像出自哪个 Dockerfile 无关;`k8s_image.ps1 release-zone|release-all` 只是在构建前多查一次。
- `Dockerfile.runtime` expects Linux binaries staged under `deploy/k8s/runtime/linux/`(根 `.dockerignore` 不得排除该目录)。
- Managed cloud generally uses `LoadBalancer`; bare metal generally uses `NodePort` + external L4.
- Prefer explicit `-OpsProfile managed-cloud` / `-OpsProfile bare-metal` in commands and docs.
- **gate 路由模式(2026-09-29,turn-based §22 D75)**:`k8s_deploy.ps1 -GateRouterMode` **默认 `"1"`**(只写 gate 的 `GATE_CLIENT_RPC_ROUTER`;C++ 默认值不改)。
  前置是 `infra-up` 已部署 `client-rpc-router`(不带 `-SkipGoSvc`、给了 `-GoSvcRegistry`),否则 gate 卡在依赖门、全部 `no_target`,脚本不拦。
  回退 `"0"` = chat / friend / trade 同时不可达、且 gate 不再中继战斗(D66)。**回退态不粘滞**:以 `"0"` 运行的 zone 每次重新部署
  (含 `release-zone`、合服后 `zone-up`、各包装入口)都要显式再传 `-GateRouterMode 0`。
  K8s 路由模式下的 battle-smoke 是事后补验项,补验前不要写"K8s 默认部署下已验证可用"。细节见 `README.md` Optional Flags。
- **集群外入口(D76–D93,设计 `docs/design/k8s-client-entry.md`)**:`-ClientEntryMode podip|external`(默认 podip)与 `-BattleOrchestrator deployment|agones`
  (默认 deployment)是两个正交开关;模式与地址参数**不粘滞**,以 external 运行的环境每次部署都要照传,换形态 / 改地址要 `-AllowDisruptiveSwitch`
  (不给则在写操作之前整条拒绝)。集群外部署须与 `-GateRouterMode 1` 同窗口启用 external。生成器与 preflight 只在
  `tools/scripts/lib/k8s_client_entry.ps1` 一处,不要在 `k8s_deploy.ps1` 里复制第二份规则。
- **gate 入口 = 每实例一个**:login 的票据绑 `gate_node_id`,gate 不符即 `token_gate_node_mismatch`。单一 `gate-entry` Service 只在 podip 且 gate 副本数恰为 1 时生成(D90);
  external 用 StatefulSet + 每序号 Service `gate-<i>`(`externalTrafficPolicy` 默认 Local,保源 IP)。不要写"给 gate 挂一个 LB / Service 就能对外"。
- **包装入口透传**:`dev_tools.ps1` / `k8s_image.ps1` 的 `-GateRouterMode`、`-GateServiceType` 与集群外入口参数一律留空 = 不覆盖,默认值只在 `k8s_deploy.ps1`。
  `k8s_image.ps1` 只透传 gate / battle 侧 6 个参数 + `-GateRouterMode` / `-AllowDisruptiveSwitch` / `-RequireClientEndpoint`;`-Gateway*` 与 `-LoginDevPasswordAuth`
  经 `k8s-image-*` / `k8s-release-*` 给了即报错(它不部署 Go / Java 服务)。完整清单见 `README.md`「包装入口的透传口径」。
- **C++ 启动壳不加 `ulimit`(B0-4,负责人 2026-10-17 拍板;2026-10-22 经负责人确认写入)**:gate / scene / battle 在 `Node::Initialize` 自己把 nofile 软限抬到硬限,
  抬完仍不够就缺省拒启(`cpp/libs/engine/core/node/system/node/open_file_limit.h`)。在 `/bin/sh -lc` 启动壳、镜像或 initContainer 里再加 `ulimit -n`,
  是同一件事的第二份实现,不要加。硬限只能在节点运行时改(containerd / dockerd 的 `LimitNOFILE`,或 CRI 的 rlimits),K8s 清单声明不了。
  应急豁免用 `k8s_deploy.ps1 -NodeOpenFileLimitMode warn`,gate / battle 首选调低准入上限;见 `docs/ops/gate-kernel-tuning-runbook.md` §3.1。
  Kafka broker 的 `ulimit -n` 薄壳(`manifests/infra/kafka.yaml`)是 broker 自己的事,不在此列。
- **kind**:一律 `kind create cluster --name mmorpg --config deploy/k8s/kind-config.yaml --kubeconfig "$env:TEMP\kind-mmorpg.kubeconfig"`(kind 钉 v0.33.0),
  所有命令 fail-closed 点名 context:`k8s_deploy.ps1 -KubeContext kind-mmorpg -KubeConfig <同一文件>`、helm `--kube-context kind-mmorpg --kubeconfig <同一文件>`、
  kubectl `--context kind-mmorpg`。不要再写不带 `--config` 的 `kind create cluster`(会切走默认 current-context);改 `kind-config.yaml` 只追加,不重写。

## ANTI-PATTERNS
- Using the repository root `Dockerfile` as the K8s runtime image.
- Treating `LoadBalancer` as a universal default.
- Assuming Windows `.exe` outputs in `bin/` are deployable to the runtime image.
- Describing `k8s-zone-down` as a partial cleanup; it deletes the full namespace.

## COMMANDS
```bash
# Build all images before deploying
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-build-all

# Deploy shared infrastructure (one-time, all zones share this)
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-infra-up
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-infra-status

# Deploy zones
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up -ZoneName yesterday -ZoneId 101 -OpsProfile managed-cloud -WaitReady -NodeImage ghcr.io/luyuancpp/mmorpg-node:latest
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-all-up -ZonesConfigPath deploy/k8s/zones.yaml -OpsProfile managed-cloud -NodeImage ghcr.io/luyuancpp/mmorpg-node:latest
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName yesterday

# Runtime staging (alternative: pre-built binaries)
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-image-preflight
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-stage-runtime -BinarySourceRoot D:/linux-build/bin -ZoneInfoSource bin/zoneinfo -TableSource generated/tables
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-build-image -ImageRepository ghcr.io/luyuancpp/mmorpg-node -ImageTag v1
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-push-image -ImageRepository ghcr.io/luyuancpp/mmorpg-node -ImageTag v1
```

## NOTES
- **Architecture**: All zones share one set of infra services (etcd, Redis, Kafka, MySQL) in the `mmorpg-infra` namespace. Zone namespaces contain only game nodes and microservices.
- Config templates use cross-namespace K8s DNS: `etcd.mmorpg-infra:2379`, `redis.mmorpg-infra:6379`, etc.
- **etcd 是 3 副本 StatefulSet(`sts/etcd`,成员 `etcd-0/1/2`)+ 每成员一块 PVC + `PodDisruptionBudget minAvailable: 2`**,不是 Deployment。原因:节点 id / 发号槽位全是 lease 绑定,单副本 etcd 重启 = 全体节点 fence。`kubectl exec ... deploy/etcd` 已失效,用 `sts/etcd` 或 `etcd-0`。`infra-up` 会先删旧的 `deployment/etcd`(不迁 emptyDir 数据,节点重启即重新注册),细节在 `README.md`「etcd:3 副本 StatefulSet」。redis 与 kafka 已改为 StatefulSet + PVC(redis 单副本,改 Pod 模板要过 `-ConfirmRedisCoreRestart` 重启闸;kafka 的 broker 数:dev 档且运维档 custom 时 1 个,否则 3 个,`-KafkaBrokers` 可覆盖,见 `Get-KafkaTopology`),mysql 仍是单副本 Deployment。
- **etcd 的 MVCC 历史压缩只靠 etcd 内置的自动压缩(P0-C 阶段 2,2026-11-05)**:`etcd.yaml` 的 `ETCD_AUTO_COMPACTION_MODE=revision`、保留 1000000 个 revision(与两份 compose 同值)、配额 3GiB;阶段 1 的 CronJob `etcd-compact` 已退役,`infra-up` 会删掉它的残留,**不要再加回来**。镜像是官方 `quay.io/coreos/etcd:v3.6.16` 按摘要钉死,`etcd.yaml`、`etcd-id-secondary.yaml`、`etcd-id-registry.yaml`、两份 compose **五处同一个引用,换版本同批改**;官方镜像是 distroless,`kubectl exec` 只能直接跑 `etcdctl`,不能开 `sh`。`tools/scripts/tests/etcd_maintenance_contract.tests.ps1` 守住。改 `etcd.yaml` 的 Pod 模板(镜像、env、注解、探针)= 逐个滚动 3 个成员:已有集群上 C++ etcd 流重建(`docs/design/grpc-muduo-transport.md` §10.2 第 2 批)还没有零重注册的运行证据,有 C++ 节点在跑时放进全栈维护窗口。告警在 `deploy/k8s/etcd-alerts.yaml`;defrag、NOSPACE 处置、取值与部署前置见 `docs/ops/etcd-maintenance-runbook.md`。
- `-ClusterId`(0..31,默认 0)是**部署级常量**:snowflake worker 段 `[cluster5][node12]` 的高 5 位,写进 C++ `node-config`(含 infra 里 battle 的 `battle-node-config`)的 `base_deploy_config.yaml` 与 scene-manager 的 Go ConfigMap。login、guild、match 只由发号器发号,ConfigMap 里不写它(2026-11-12 起 login、guild 也删了)。`infra-up` 与 `zone-up` 读同一个参数,zones 配置里没有它、也不该有。只有 `k8s_deploy.ps1` 收它,`dev_tools.ps1` 的 `k8s-*` 包装还没透传。
- **snowflake 槽位缓存 = 每 Pod 一个 `emptyDir`,只有 scene-manager 有**:脚本常量 `$SnowflakeCacheDir = /tmp/snowflake` 写进 scene-manager ConfigMap 的 `SnowflakeCacheDir`;`manifests/go-svc/scene-manager.yaml` 的 `snowflake-cache` 卷 `mountPath` 是**写死**的同一路径,脚本不替换,改常量要一起改。不需要 PVC,不跨 Pod 共享。login、guild 不申领槽位,清单不挂这个卷、ConfigMap 不写这个键;C++ 的槽位客户端早已删掉,gate / scene / battle 的工作负载也不再注入 `SNOWFLAKE_CACHE_DIR`、不挂这个卷(2026-11-12;`id_issuer_deploy_contract.tests.ps1` 守住)。
- **data-service 的目录和流水库**(脚本常量 `$GlobalDbName` 是基名,ConfigMap `SnapshotMySQL.DBName`;目录 K=1 是基名,否则 `<基名>_00..`,流水库 `<基名>_ledger`)由 `infra-up` 按 `-DirectoryLogicalShards` 生成的 `02_k8s_global_db.sql` 预建 + GRANT appuser;initdb 只对空 PVC 生效,存量 PVC 手工补。login ConfigMap 写死 `IdSegment.Enabled=true`,所以**建角依赖 data-service + 这些库**;`Schema.AutoMigrate` 所有档位 false(常驻 data_service 见到 true 拒启,表只由 `data-service-migrate` Job 建)。data-service 仍按 zone namespace 各部署一份(catalogue 里不是 Global),但库与 go-zero 发现键都不分 zone。
- **Kafka 是 StatefulSet(`sts/kafka`,Pod `kafka-0` 起)+ 每 broker 一块 PVC + PDB**,不是 Deployment。broker 数:dev 档且运维档 custom 时 1 个,否则 3 个,`-KafkaBrokers` 可覆盖(`Get-KafkaTopology`;多 broker 的副本因子、PDB 与选举组见 `README.md`「Kafka:多 broker」)。下面「刻意保持 `replicas: 1`」等单 broker 的说法是 2026-09-08 的历史,以「Kafka:多 broker」一节为准(`routing-identity-audit-20260908.md` R06:emptyDir 上的 broker 一重启丢掉全部 topic / 分区元数据 / offset,停机窗口里的控制面命令在 librdkafka 5 分钟超时后全丢)。`kubectl exec ... deploy/kafka` 已失效,用 `sts/kafka`。`infra-up` 会先删旧的 `deployment/kafka`(Service selector 会同时命中新旧 Pod = 脑裂),**第一次切换会丢掉现有全部 topic 数据,切完必须重跑 `infra-kafka-topics`**。数据目录由 `KAFKA_LOG_DIRS=/var/lib/kafka/data` 显式指定(旧 manifest 把卷挂在 `/tmp/kafka-logs`,而镜像默认 `log.dirs` 是 `/tmp/kraft-combined-logs` —— 那块卷根本没被写过)。**刻意保持 `replicas: 1`**:所有 topic 都是 replication-factor 1,而且广播地址是 ClusterIP Service 名,单改副本数 = 没冗余还路由错乱。探针里必须保留 `KAFKA_HEAP_OPTS=-Xms16m -Xmx64m` 的覆盖(工具脚本会继承容器 env 的 `-Xmx1g`,叠在同一 memory limit 上必 OOMKilled);容器 `command`/`args` 是一层 `ulimit -n` 薄壳,改镜像版本要核 `/__cacert_entrypoint.sh /etc/kafka/docker/run` 还在不在。细节在 `README.md`「Kafka:StatefulSet + PVC」。
- **Kafka 审计 topic 由部署侧预建,不能靠自动创建**:`manifests/infra/kafka-topic-init.yaml` 一次性 Job(`infra-up` 在全部 infra manifest(`$infraManifests`)之后、任何 zone 之前 apply;`-WaitReady` 等它 Complete),建 `transaction_log_topic_g<N>`(6 分区)/ `player_snapshot_topic_g<N>`(3 分区),值全从 `go/data_service/etc/data_service.yaml` 取。broker 是 auto-create + `num.partitions=1`,scene 抢先发消息就会把 topic 建成 1 分区、保留期继承 broker 默认值。**这两个审计 topic 的分区数只许加、可以原地加**(2026-11-06,`docs/design/audit-pipeline-horizontal-scale-20261106.md` §2.A):Job 带 `grow` 参数,少了 `--alter` 加到配置值,data-service 的 `EnsureTopics` 开着 `AllowPartitionGrowth` 也会加;减分区才是 `Kafka.TopicGeneration` +1(新 topic)。命令 / 观战 / 世界频道 topic 照旧不可变,**别把 `grow` 加到它们身上**;换成 PVC 之后普通重启不再丢 topic,但**第一次从旧 emptyDir Deployment 切过来 / `infra-down` 删过 PVC / 手工删过 topic 之后**,必须先跑 `k8s_deploy.ps1 -Command infra-kafka-topics -WaitReady` 再让 scene 产消息。compose 的 `kafka-topic-init` 是同一件事。细节在 `README.md`「Kafka 审计 topic 预建」。
- **Kafka 保留期只有一条规则**(`routing-identity-audit-20260908.md` R11):**消费者可能落后的 topic,保留期必须 > 消费者可能落后的最长时间并留足余量,而且必须显式声明、不许继承 broker 默认值。** C++ 消费者把 `max.poll.interval.ms` 钉在 900000(15 分钟),所以命令 topic 取 1 小时,`$KafkaBrokerRetentionMs`(给迁移窗口里 auto-create 的 `gate-<id>`/`scene-<id>` 兜底)取 30 分钟 / 1 小时。`--create --config retention.ms` 只在新建那一刻生效,所以两条 init 路径都在建完后再 `kafka-configs.sh --alter` 声明一次并读回核对。细节在 `README.md`「Kafka 保留期」。
- **写者 Pod(2026-10-31,未在集群上验证)**:控制文件都在 emptyDir 挂载根下的 `private` 子目录(挂载根过不了私有目录校验),不要写回挂载根。写者 Pod 模板带 label `mmorpg.io/sql-writer: "true"`,写者服务都是 StatefulSet,Pod 序号就是写者名册的槽位;CronJob `sql-writer-pod-reaper` 每分钟删主容器以 78 退出(写过之后崩溃)的 Pod,StatefulSet 按同序号重建、在槽位上换代,名册不消耗,槽位数 ≥ 副本数即可(`docs/design/writer-roster-exhaustion-20261106.md`)。缩容被缩掉的序号由每个写者 StatefulSet 自己的 CronJob `sql-writer-slot-retirer-<StatefulSet>` 结束末代(同文档 §5.9);改写者的渲染或 apply 顺序时,它要跟着写者一起渲染、在写者 apply 之后落地,金丝雀收尾不删它。go/db 的迁移是 `db-migrate` Job,镜像 `mmorpg-db-migrate`。全新集群先跑 `-SqlSchemaOnly` 拿 generation 再写名册。细节在 `README.md`「写者 Pod 的启动目录、迁移和回收」。
- **发号器不用全局库**:恢复 `mmorpg_global` 不再影响发号。发号器的预留记录在 `etcd`、`etcd-id-secondary` 两套本地 etcd 和全世界共用的全局号源登记处(`etcd-id-registry`,`-IdRegistryEtcd` / `-HostIdRegistry`)里。新地域要显式跑一次 `-id-issuer-init`,事故走 `-id-issuer-recover`,登记处不许从备份恢复,见 [发号器运维手册](../../docs/ops/id-issuer-runbook.md)。data-service ConfigMap 的 `IdSegment.AllowAutoSeed` 在 staging/prod 固定 `false` 就是为此。地域码绑定到「部署身份 + 两套本地 etcd 的集群号」(2026-11-11):部署身份是 `kube-system` 的 UID,data-service 用 ServiceAccount `data-service` 运行期读,权限在 `manifests/go-svc/data-service-identity-rbac.yaml`(只读 `namespaces/kube-system`,每个 zone 一个 ClusterRoleBinding,`Apply-OneGoSvc` 先于 StatefulSet apply,`zone-down` 一并删);三套 etcd 的引导令牌带同一个 UID(`__KUBE_CLUSTER_UID__`,`Apply-Infra` 在第一个写操作之前读,读不到就拒绝)。细节见 `README.md`「地域码绑定到 K8s 集群」。
- **guild = 全局池(infra namespace 一份)+ 建表 Job**:`manifests/go-svc/guild.yaml`(写者 StatefulSet)与 `guild-migrate.yaml`,ConfigMap `go-svc-guild-config` 由 `New-GuildServiceConfigYaml` 按 `go/guild/etc/guild.yaml` 生成,带 `DataServiceRpc`、`IdSegment`(guild_id、op_id 只由号段发);不写 `ClusterId` / `SnowflakeCacheDir`,不挂 `snowflake-cache`。细节在 `README.md`「帮会 guild」一节。
- **聚宝斋 trade = 全局池(infra namespace 一份)+ 建表 Job**:`manifests/go-svc/trade.yaml`(50800 / metrics 9230,只经路由服可达;K8s 默认 `-GateRouterMode 0` 下玩家不可达 —— **2026-09-29 更正**:K8s 默认已是 `"1"`(D75),默认部署下按设计可达、待 K8s 事后补验,集群外玩家另需 `-ClientEntryMode external`)与 **`manifests/go-svc/trade-migrate.yaml`**(port-decisions D-14 第 4 条:同镜像、同 ConfigMap、`-migrate`)。`$GoSvcCatalogue.trade.MigrateJob` 登记 Job;`Apply-OneGoSvc` 在 ConfigMap 之后、Deployment 之前 `Apply-GoSvcMigrateJob`(先删再 apply;门禁 **staging/prod 恒等、不受 `-WaitReady` 控制**(D-14 第 4 条),dev 只在 `-WaitReady` 下等:先等 `deploy/mysql` 再轮询 Job,Failed / 超时即中断发布,Deployment 不 apply)。**删之前先查上一次的 Job 是否在途**:迁移 runner 执行 DDL 前写 dirty=1,删在途 Job 会把台账留在 dirty、之后每次 `-migrate` 都以 1 失败 —— 所以存在的 Job 须有 Complete / Failed / FailureTarget 条件,且按该 Job UID 核对所属 Pod 全部为 Succeeded / Failed,才可删除;仍在途、状态未知或查询失败时在预算内等待,等不到就中断且不删;ACTIVE=0 不含 terminating Pod,不能单独作为安全删除判据;不要把 kafka-topic-init 的"无脑先删再建"照搬到有台账的 Job 上,也不要手工 delete 在途迁移 Job。Job 的 `podFailurePolicy`:退出码 1 / 4 → FailJob,3(锁忙)→ backoff 重试,**需 K8s ≥ 1.26 + `restartPolicy: Never`**;initContainer 用同镜像 busybox `nc` 等 3306(mysql 就绪探针走 socket,initdb 期间会谎报 Ready),预算 `WAIT_MYSQL_TIMEOUT_SECONDS=150`(`/proc/uptime` 单调计时,硬上限 +8s);脚本两段 Job 等待(删前在途检查、apply 后终态轮询)的预算 = `max(-WaitTimeoutSeconds, $GoSvcMigrateJobMinWaitSeconds=300)`,**改 initContainer 上限必须同步这个下限**,否则全新集群的正常等待会被误判超时、中断发布。库 `mmorpg_trade` **只登记在 `deploy/mysql-init/00_init_zone_dbs.sql`**(mysql-init-sql 原样带入),不要在脚本里再生成建库 sql;存量 PVC 手工补建 + GRANT(命令在 `README.md`「聚宝斋 trade」)。ConfigMap 的 `Mode` / `Schema.AutoMigrate` dev = 服务 yaml,staging/prod = `pro` / `false`。data-service `IdSegment.Tags` 已含 `trade_listing` 与 `guild_asset_op`(帮会资产指令 op_id,帮会二期 B5a)。以后的建表服务照 `MigrateJob` 字段加,不另写 Job 调度。
- **玩家收件箱 player-inbox = 全局池(infra namespace 一份)+ 建表 Job + 运维口令 Secret**(2026-10-15,`docs/design/player-inbox-service.md` 第 4 批):`manifests/go-svc/player-inbox.yaml`(50950 / metrics 9270,D14)与 `player-inbox-migrate.yaml`,形状与门禁全照 trade;库 `mmorpg_player_inbox` 只登记在 `deploy/mysql-init/00_init_zone_dbs.sql`。调用方是 C++ scene(etcd 前缀 `PlayerInboxNodeService.rpc`,两份前缀表与 `GrpcClient.CallDeadlineMs.PlayerInboxNodeService` 预算门禁都已登记)与运维,不经路由服;弱依赖,不进 scene DependencyGate。运维口令只走 Secret `player-inbox-admin-token`(环境变量 `MMORPG_PLAYER_INBOX_ADMIN_TOKEN` → 容器 `PLAYER_INBOX_ADMIN_TOKEN`),ConfigMap 只写变量名 `PlayerInbox.AdminTokenEnv`(go/player_inbox 须认这个键,补上之前带口令的部署在写操作前被拒);任何档位不注入口令 = 运维接口停用(fail-closed),注入了按档位校验长度 / 占位 / 跨用途复用;Pod 模板的口令指纹注解是占位,由 `Apply-OneGoSvc` 替换,**不要绕过脚本直接 apply**。`PlayerInbox.EnqueueTypes` 镜像服务 yaml,仓库默认列了 Battle 结算、`consumer_enabled` 默认开(未上线,整批起步,2026-11-01);上线之后再加类型要等全部 scene 开 `consumer_enabled`(上线顺序见 `docs/ops/player-inbox-runbook.md`)。scene 的消费开关只有 git 跟踪的 `bin/etc/game_config.yaml` 的 `player_inbox.consumer_enabled` 一份(2026-10-17 起,交接 H1),`node-config` 整段复制它;**开 / 关消费 = 改这份 yaml 并提交**,再 zone-up 并滚动 scene。`-PlayerInboxConsumer` 只核对、不改值(与 yaml 不一致即拒绝),不要再用它开消费。写前门禁:消费端 false 而 `EnqueueTypes` 非空时 zone-up / all-up / infra-up 一律在写操作前拒绝 —— 先比当前检出的两份 yaml,非 DryRun 时再只读另一侧线上的值(zone 与 infra 可以来自不同 commit:zone-up 渲染 false 时看 infra 线上白名单,infra-up / all-up 渲染非空白名单时看各 zone 线上 node-config),staging / prod 真部署还拒绝两份 yaml 带未提交改动;只有显式 `-AllowPlayerInboxConsumerOff` 放行(包装入口没有这个开关,被拒时先把 `EnqueueTypes` 改回 `[]` 并重跑 infra-up)。开消费之后回滚 zone 用当前 HEAD 的脚本配旧 `-NodeImage` 或 `rollout undo`,不要检出开消费之前的旧 commit 重跑 zone-up。ConfigMap 的顶层 `Middlewares` 段(`StatConf.IgnoreContentMethods` 列 FetchItems / AckItems,评审 L12)整段复制自服务 yaml,缺了新版 Go 与 `player-inbox-migrate` Job 都拒启。连接总账已含它(`PlayerInboxReplicas` / `PlayerInboxMaxOpen`),告警 `deploy/k8s/player-inbox-alerts.yaml`。细节在 `README.md`「玩家收件箱 player-inbox」。
- `zones.ops-recommended.yaml` is the best starting point for multi-zone ops.
- `-SkipInfra`, `-DryRun`, and `-WaitReady` are the high-signal operational flags.
- `k8s-all-up` deploys infra first, then all zones. Use `-SkipInfra` to skip infra.
- Current manifests assume `/app/bin` runtime layout and fixed role ports (`gate` 18000, `scene` 20000). These must stay inside the engine's per-role TCP ranges — gate `10000-19999`, everything else `20000-35535` (`cpp/.../node_allocator.cpp`) — because gRPC is derived as TCP+30000 and the two ranges are what keeps TCP and gRPC from overlapping. `scene` was 19000 until the port env vars actually took effect; 19000 sits in gate's range.

## SCENE NODE ROLE SPLIT
- Production scene pods run in two Deployments per zone:
  - `scene-world`    — `env SCENE_NODE_TYPE=0`, hosts persistent main-world channels.
  - `scene-instance` — `env SCENE_NODE_TYPE=1`, hosts on-demand dungeons and battlegrounds.
- The zones YAML expresses this with `scene_world: N` / `scene_instance: N` under `replicas`. Legacy single-pool `scene: N` remains accepted for dev.
- Implemented in `tools/scripts/k8s_deploy.ps1::Resolve-SceneDeploymentPlan`. 兼容规则(权威表在 `docs/ops/scene-node-role-split.md` §3.2.1):出现任一拆分键 → 生成 `scene-world` + `scene-instance` 且**不再生成** `scene`;否则生成单个 `scene`(SCENE_NODE_TYPE=0)。两种模式互斥。
- 命令行等价开关 `-SceneWorldReplicas` / `-SceneInstanceReplicas`(`-1` = 未指定)。
- 切换到拆分模式后,旧的 `scene` Deployment 需要**手动删除**(`kubectl apply` 不会删它),否则该 zone 会同时存在两套容量语义。
- C++ reads `SCENE_NODE_TYPE` and `GAME_CONFIG_PATH` from the pod env; the file baseline stays at `etc/game_config.yaml`. 只有一个 `node-config` ConfigMap,不为角色拆分再生成第二份。
- **共享 Redis 口令(`MMORPG_REDIS_PASSWORD`)不进任何 ConfigMap**(C++ 2026-11-05、Go 服务与 gateway 2026-11-06;`docs/handoff/handoff-global-singletons-p0-20261015.md` §8、§9):值只在 Secret `redis-auth`(键 `password`)里,`Sync-RedisAuthSecret` 在 infra namespace 与每个 zone namespace 各建一份,容器经 `secretKeyRef`(不写 optional)拿值。
  - C++:`node-config` / `battle-node-config` 的 `zoneredis.password`、`scene_control_redis.password` 写空串,Pod 拿 `ZONE_REDIS_PASSWORD`(scene 另拿 `SCENE_CONTROL_REDIS_PASSWORD`),`config.cpp` 用它覆盖 yaml;口令非空时 Pod 模板带 `mmorpg.io/zone-redis-auth-hash`(`Get-CppZoneRedisAuthAnnotations`)。
  - Go 服务(`$script:SharedRedisAuthGoServices`):ConfigMap 里共享 Redis 段的 `Pass` / `Password` 写空串,口令非空时末尾多一行 `SharedRedisPasswordEnv: SHARED_REDIS_PASSWORD`,服务启动时由 `go/shared/redisauth` 填各段(变量缺失即拒启);gateway 的 `spring.data.redis.password` 只写 `"${SHARED_REDIS_PASSWORD}"`。容器拿 `SHARED_REDIS_PASSWORD`,Pod 模板带 `mmorpg.io/shared-redis-auth-hash`(`Add-SharedRedisAuthToManifest`);mail 自带可选引用,只补指纹。
  - 指纹只哈希口令,换口令即滚动;dev 未注入时不注入、不写指纹,产物与引入之前逐字相同。非 dev 档空口令、口令含 `"` / `\` / 控制字符,在任何写操作之前拒绝。新增连共享 Redis 的 Go 服务:ConfigMap 段写空串、进上面那份名单、在 `go/<服务>` 里接 `shared/redisauth`,三处同批改。换口令按 `README.md`「C++ 节点的 zone Redis 口令」的双口令轮换做。
- **其余工作负载密钥同样只放 Secret**(2026-11-10~12,`docs/design/workload-secrets-via-k8s-secret.md`):gate / battle / 观战令牌、login 的 HMAC 密钥、match 运营入口密钥,以及只给 route-tool Job 的 MySQL 部署口令 `mysql-auth`。规则收在 `k8s_deploy.ps1` 的 `$script:WorkloadSecretCatalog` 一张表里(Secret 名、键、变量名、消费者)。
  - **写者不拿共享数据库口令**(2026-11-12):db、data-service、login、gateway 与全局 Go 服务是 SQL 写者,常驻进程读运维预置的 `runtimeConfigSecret`、迁移 Job 读 `schemaConfigSecret`,每个槽位一个专用账号,口令写在那两份 Secret 的配置里。不要给写者 Pod 或迁移 Job 注入 `mysql-auth` 之类的共享数据库口令,也不要在写者的 ConfigMap 模板里指名口令变量;ConfigMap 模板里口令一律写空串。全服同值的密钥(Redis 口令、HMAC 密钥)照常注入写者 Pod。
  - 每把密钥一个单键 Secret,在每个有消费者的 namespace 先于消费者(含迁移 Job)建好;容器环境变量与部署输入同名(`MMORPG_*`),`secretKeyRef` 不写 optional;Pod 模板带只哈希这把密钥的 `mmorpg.io/<Secret 名>-hash`,换密钥即滚动。
  - ConfigMap 里只写空串或变量名:C++ 由 `config.cpp` 用变量覆盖 yaml;Go 服务在配置里写 `*Env` 键,启动时由 `go/shared/secretenv` 填(指名了却没有值即拒启);gateway 写 `"${变量名}"`。route-tool 的清单静态引用 `mysql-auth`(它是唯一直接挂部署脚本 ConfigMap 的 Job);迁移 Job 不引用。
  - **各档位一条注入路径**:dev 的回落值也经 Secret 注入。这一点与共享 Redis 口令不同(dev 没有 Redis 口令,不注入),所以 11-10 起 dev 产物不再与之前逐字相同,是有意的。
  - **新增一把密钥**:目录表加一项、消费者的 ConfigMap 写变量名、服务代码按上面的读法接、契约测试的 `$WorkloadSecretSpecs` 同步,同批改。ConfigMap 生成器里写变量名字面量、不读目录表(契约测试按 AST 单独抽生成器跑),两边一致由契约测试核对。
  - **上线顺序**:凡是让 ConfigMap 依赖服务新读法的改动,先发带新读法的镜像,再用新部署脚本;新读法必须向后兼容(不写指名键时行为不变)。反过来旧镜像会读到空密钥。
  - **经 Spring 占位交给 gateway 的密钥不得含 `${`**(2026-11-11):Spring 会把占位解析出来的值再解析一遍,值被静默改写。`Assert-SpringPlaceholderSafe` 在部署 gateway 时、任何写操作之前检查 gate 令牌、共享 Redis 口令与管理面口令;新增经 `${...}` 交给 gateway 的密钥,要加进那份检查清单。gateway 启动时另由 `GateTokenSecretGuard` 校验 `gate.token-secret`(生产档拒绝空值、未解析的占位、公开占位串与短于 32 字节)。
- Go-side routing is driven by `StrictNodeTypeSeparation` in `scene_manager_service.yaml`. Keep `true` in production; flip to `false` only during the rollout window described in `docs/ops/scene-node-role-split.md`.
- `-DryRun` 会把每份将 apply 的 manifest 打印在 `--- BEGIN MANIFEST --- / --- END MANIFEST ---` 之间(以前只打印临时文件路径,而该文件当场就被删,DryRun 等于无法验证)。

## AGONES(阶段 B 已落码,未上集群)
- 模型:**1 Agones GameServer = 1 个 Scene Node Pod / C++ 进程 = N 个动态创建的 ECS Scene 房间**。不要写成"一个 Scene 一个 GameServer"。设计文档:`docs/design/agones-scene-node-high-density.md`。
- 开关:`-SceneOrchestrator deployment|agones`,默认 `deployment`。**不做**"检测集群装没装 Agones 就自动切换"。
- agones 模式下 `scene-world` / `scene-instance` 生成 `agones.dev/v1 Fleet`,gate 仍是 Deployment。
  (2026-09-29 补:gate **永不进 Agones**(D87);`-ClientEntryMode external` 下 gate 是 StatefulSet + 每序号 Service,podip 下仍是 Deployment。)
- 内部服务,端口用 `portPolicy: None`;不加公网 Service / HostPort / NodePort / LB。SceneManager 继续从 etcd 拿 PodIP。
  (2026-09-29 补:这一条只说 scene。battle 是客户端面,见下面「battle Fleet」。)
- **battle Fleet(集群外入口 D81–D84,2026-09-29 已落码,未上集群)**:`-BattleOrchestrator agones` 在 infra namespace 生成 battle Fleet
  (`tools/scripts/lib/k8s_client_entry.ps1` `New-BattleFleetYaml`),与 scene 的差别:
  - 端口:`spec.ports` 只列 `name: client`、`portPolicy: Dynamic`、containerPort 20000;gRPC 50000 永不进 hostPort / nodePort / Agones ports,容器 ports 只写 grpc。
    进程读 sidecar `status.address` + `status.ports[client]` 自报 `client_endpoint`(external 时 `CLIENT_ENDPOINT_SOURCE=agones`,podip 时 `none`)。
  - 排空:Fleet `allocationOverflow.labels: {mmorpg.io/drain: "true"}`,进程每 5s 读自己的 labels,有标签就拒新房间、打完回 Ready;运维维护节点前手动
    `kubectl label gs <name> mmorpg.io/drain=true`,删标签(`mmorpg.io/drain-`)即恢复。`eviction.safe: Never`。
  - 心跳:battle 开 EventLoop 心跳绑定(超过 10s 未更新就停发 /health,Agones 判 Unhealthy 替换);scene 不开。
  - health 复用 `-AgonesHealth*` 三个参数,initialDelaySeconds 不够覆盖 battle 启动最坏耗时(地址来源 ≤60s + 初始化余量 20s + /ready 重试约 53s + 首次 health 2s,合计 135s;
    默认 period 10 × threshold 3 时 initialDelaySeconds 下限 105s,`lib/k8s_client_entry.ps1:543`、`:560-564`)时由
    `Resolve-BattleFleetHealth` 自动抬高并打说明;`periodSeconds` 必须大于 C++ healthInterval(2s)。
  - 跨语言字符串契约:端口名 `client`、排空标签 `mmorpg.io/drain`、`CreateBattle` 拒绝消息 `battle_not_allocatable`(gRPC UNAVAILABLE,match 不发 DestroyBattle、换节点重试一次)。
  - `-BattleOrchestrator deployment` + external = battle hostPort 20000 Deployment,每节点 1 个、无忙碌保护,只用于验证与回退(D86)。
- **C++ 生命周期代码已下沉**:`cpp/libs/engine/infra/agones/`(`GameServerLifecycle` / `AllocationPermit`,原 `cpp/nodes/scene/agones/` 的 `SceneLifecycle` / `CreatePermit`),
  scene 与 battle 共用,登记在 infra.vcxproj;细节见 `docs/design/agones-scene-node-high-density.md` §12。
- Fleet 的 `health.periodSeconds`(默认 10s)必须大于 C++ 侧心跳间隔(`LifecycleOptions::healthInterval`,默认 2s)。
- `kubectl rollout status` 对 Fleet 无效,`-WaitReady` 在 agones 模式下**不等** scene,只打印该看的命令(`kubectl get fleet ... -o jsonpath='{.status.readyReplicas}'`)。
- 换编排方式会换 kind:`kubectl apply` 不会回收同名的旧 Deployment,必须手动删,否则一个 zone 里有两套 scene 进程和两套容量语义。
- 运行时镜像需要 `libcurl4`(已加进 `Dockerfile.runtime`)。
- **高密度容量(阶段 C)**:`-AgonesHighDensity -AgonesRoomCapacity <N>` 给 Fleet 挂 `counters.rooms`;SceneManager 侧配 `Agones.Enabled` / `Agones.HighDensityEnabled`(都默认 false)。
  - Counters and Lists 是 Agones **beta**,集群要先打开 `CountsAndLists` FeatureGate。
  - `-AgonesRoomCapacity` 没有默认值,不给就报错 —— 每进程房间容量必须来自压测,不许拍脑袋。
  - RBAC:`kubectl -n <zone-ns> apply -f deploy/k8s/manifests/go-svc/scene-manager-agones-rbac.yaml`(全 namespace 级 Role,无 cluster-admin)。启用后 scene-manager Deployment 要把 `serviceAccountName` 指到 `scene-manager`。
  - `Agones.Enabled=true` 但分配器构造失败时 scene-manager 会 **panic 起不来**,不会静默降级 —— 静默降级等于绕过容量约束还没人发现。

## C++ 节点镜像:两条路径(2026-09-16 按代码核对,未实跑)
| | `Dockerfile.cpp` | `Dockerfile.runtime` |
|---|---|---|
| 在哪编译 | 镜像内:deps 阶段编 gRPC `v1.83.0` 等,builder 阶段 `build_linux.sh --skip-deps --relwithdebinfo --split-debug` | 镜像外:Linux 主机 `bash tools/scripts/build_linux.sh`,再 `k8s-stage-runtime` 拷进 `deploy/k8s/runtime/linux/` |
| 调用方 | `dev_tools.ps1 -Command k8s-build-all`;`.github/workflows/cpp-build-ci.yml`(只到 `--target builder`,当编译门禁);`k8s_image.ps1 -DockerfilePath deploy/k8s/Dockerfile.cpp` 也能用,但其预检仍要求 staging 目录齐全 | `tools/scripts/k8s_image.ps1`(`-DockerfilePath` 默认值):`k8s-build-image` / `k8s-push-image` / `k8s-release-zone` / `k8s-release-all`;`publish_images.ps1` 的 cpp 族 |
| 构建入口自带门禁 | 无(`k8s-build-all` 只构建) | 仅 `release-zone` / `release-all` 构建前跑不可变 tag 校验 + `release_preflight.ps1`;`build-image` / `publish_images.ps1` 不跑。部署侧门禁对两条路径一视同仁(见 CONVENTIONS) |
| 运行层内容 | apt 装 `ca-certificates libgcc-s1 libstdc++6 libcurl4 libssl3t64 libzstd1 tzdata`;`bin/{gate,scene,battle}`、`/usr/local/lib/*.so*` + `ldconfig`、`data/scene_nav_bin/`、表文件名转小写、`bin/zoneinfo` 软链 | apt 装 `ca-certificates libgcc-s1 libstdc++6 libcurl4 tzdata`;其余只有 staging 里的东西:`bin/{gate,scene,battle}`、`bin/zoneinfo/`、`generated/generated_tables/`(契约见 `runtime/README.md`) |

- 两条都是现役入口,旧文档里"生产只准用 `Dockerfile.runtime`"与"`Dockerfile.cpp` 是推荐路径"两种说法都不完整,以上表为准。版本化发布(`publish_images.ps1` → `make_release.ps1`)走 `Dockerfile.runtime`,C++ 分离符号取宿主 `bin/symbols/*.debug`。
- ⚠️ **两条路径的运行层不等价**(上表"运行层内容"行,按代码核对,不保证穷尽):`Dockerfile.runtime` 与 staging 契约里没有 `/usr/local/lib/*.so*`(及 `ldconfig`)、导航网格数据、表名转小写,apt 也少了 `libssl3t64`、`libzstd1`;二进制若动态链接 libssl / libzstd,runtime 路径的镜像会在加载共享库时起不来。在 runtime 路径产出的镜像真实起过一次 scene 之前,不要假定二者可互换。
- 根 `.dockerignore` 以前排除了 `deploy/k8s/runtime/linux/`,`k8s-build-image` 的 `COPY ${RUNTIME_ROOT}/` 必然 not found;2026-09-16 已去掉该排除。
- 旧的 KNOWN BREAKAGE 已过时并删除:`tools/scripts/build_linux.sh` 已入库(提交 `31e4d1d4c`);`Dockerfile.cpp` 克隆 gRPC `v1.83.0` 并校验 revision,与 `.gitmodules` 的 `branch = v1.83.x` 一致。

## 发布打包
- 标准与分批方案:`docs/design/release-packaging-standard-20260914.md`;发布步骤清单:`docs/ops/release-checklist.md`。
- 制品根 = `MMORPG_ARTIFACT_ROOT`(缺省 `<仓库父目录>/artifacts`,**不在仓库内**);版本目录不可变、原子上线、带 `sha256sums.txt`;快照轨 `snapshots/images/g<sha12>/`,发布轨 `releases/images/vX.Y.Z/`,`releases/` 永不清理。
- 镜像 tag:给 `-Version vX.Y.Z`(或 `MMORPG_RELEASE_VERSION`)时为 `vX.Y.Z-<sha12>`,否则 `<sha12>`;发布轨拒绝脏树。镜像内 `/app/BUILD_INFO` 与 OCI label `org.opencontainers.image.revision` 自报 commit,`publish_images.ps1` 校验 label == 当前 commit。
- 发布集合只有 cpp / go / java 三族;robot、sandbox-mock 是测试工具镜像,不进制品目录。
- 推 registry 属于 AGENTS §9 受限操作:只推用户明确指定的 dev registry,AI 不执行 push。

```powershell
# 快照轨(工作树必须干净):构建三族镜像 → 逐个 docker save → snapshots/images/g<sha12>/
pwsh -File tools/scripts/publish_images.ps1
# 发布轨:版本号注入镜像 → releases/images/v1.2.3/;CI 重跑加 -SkipBuild -SkipIfExists(-Registry 保持相同)
# -Registry = 最终推送的前缀,镜像 ref 按它命名;不填则出包与推送都用默认 ghcr.io/luyuancpp
pwsh -File tools/scripts/publish_images.ps1 -Version v1.2.3 -Registry <registry>
# (可选)推送并记录 digest:<registry> 必须与出包一致(push 不重新打 tag,make_release 要求 ref 逐一对应);
# 记录文件放仓库外,放仓库内会让工作树变脏,后续带 -Version 的推送被拒。
# k8s_image.ps1 push-image -ImageRepository <registry>/mmorpg-node、java_svc_image.ps1 push 同样收 -Version / -DigestsOut
$digests = Join-Path (Split-Path $PWD.Path -Parent) 'image-digests-v1.2.3.json'   # 当前目录 = 仓库根
pwsh -File tools/scripts/go_svc_image.ps1 -Command push-all -Registry <registry> -Version v1.2.3 -DigestsOut $digests
# release manifest:CHANGELOG.md 必须有 "## [1.2.3]" 段 → releases/manifests/v1.2.3.{md,json}
pwsh -File tools/scripts/make_release.ps1 -Version v1.2.3 -ImageDigestsFile $digests
# 目标机:校验后落地(缺省 deploy/offline-images/v1.2.3)→ 离线导入并逐个核对镜像 ID
pwsh -File tools/scripts/fetch_images.ps1 -Channel release -Version v1.2.3
pwsh -File tools/scripts/import_images.ps1 -Dir deploy/offline-images/v1.2.3
# 快照清理(默认 dry-run,加 -Force 才删;releases/ 不碰)
pwsh -File tools/scripts/artifacts_retention.ps1 -KeepLast 10
```

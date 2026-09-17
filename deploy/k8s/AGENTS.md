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
└── README.md          # Authoritative flow description
```

## WHERE TO LOOK
| Task | Location | Notes |
|------|----------|-------|
| End-to-end flow | `README.md` | One-zone/all-zone operations |
| Runtime staging layout | `runtime/README.md` | Required Linux file layout |
| C++ 节点镜像 | `Dockerfile.cpp` / `Dockerfile.runtime` | 两条现役路径,分工见下方「C++ 节点镜像:两条路径」;都不是根 `Dockerfile` |
| 发布打包 | `tools/scripts/publish_images.ps1` 等 | 见下方「发布打包」 |
| Infra manifests | `manifests/infra/` | Shared infra (etcd/redis/kafka/mysql) deployed to `mmorpg-infra` namespace. etcd is a 3-replica StatefulSet + PVC + PDB; redis/kafka/mysql are still single-replica Deployments |
| Script entrypoint | `tools/scripts/dev_tools.ps1` | `k8s-*` commands drive this subtree |

## CONVENTIONS
- This subtree is Kubernetes-only; do not mix docker-compose/local process assumptions into it.
- C++ 节点镜像只从 `deploy/k8s/Dockerfile.cpp` 或 `deploy/k8s/Dockerfile.runtime` 出;带发布门禁(不可变 tag + `release_preflight.ps1`)的入口目前只有 `Dockerfile.runtime` 这条(`k8s_image.ps1`),分工见「C++ 节点镜像:两条路径」。
- `Dockerfile.runtime` expects Linux binaries staged under `deploy/k8s/runtime/linux/`(根 `.dockerignore` 不得排除该目录)。
- Managed cloud generally uses `LoadBalancer`; bare metal generally uses `NodePort` + external L4.
- Prefer explicit `-OpsProfile managed-cloud` / `-OpsProfile bare-metal` in commands and docs.

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
- **etcd 是 3 副本 StatefulSet(`sts/etcd`,成员 `etcd-0/1/2`)+ 每成员一块 PVC + `PodDisruptionBudget minAvailable: 2`**,不是 Deployment。原因:节点 id / 发号槽位全是 lease 绑定,单副本 etcd 重启 = 全体节点 fence。`kubectl exec ... deploy/etcd` 已失效,用 `sts/etcd` 或 `etcd-0`。`infra-up` 会先删旧的 `deployment/etcd`(不迁 emptyDir 数据,节点重启即重新注册),细节在 `README.md`「etcd:3 副本 StatefulSet」。redis / mysql 仍是单副本 Deployment。
- `-ClusterId`(0..31,默认 0)是**部署级常量**:snowflake worker 段 `[cluster5][node12]` 的高 5 位,写进 C++ `node-config` 的 `base_deploy_config.yaml` 与 login / scene-manager / match 的 Go ConfigMap。`infra-up` 与 `zone-up` 读同一个参数,zones 配置里没有它、也不该有。只有 `k8s_deploy.ps1` 收它,`dev_tools.ps1` 的 `k8s-*` 包装还没透传。
- **snowflake 槽位缓存 = 每 Pod 一个 `emptyDir`**:脚本常量 `$SnowflakeCacheDir = /tmp/snowflake` 写进 login / scene-manager / match ConfigMap 的 `SnowflakeCacheDir` 与 C++ gate / scene Deployment / Fleet 的 `SNOWFLAKE_CACHE_DIR`;`manifests/go-svc/{login,scene-manager,match}.yaml` 的 `snowflake-cache` 卷 `mountPath` 是**写死**的同一路径,脚本不替换,改常量要一起改。不需要 PVC,不跨 Pod 共享。
- **data-service 全局库 `mmorpg_global`**(脚本常量 `$GlobalDbName`,ConfigMap `SnapshotMySQL.DBName`)由 `infra-up` 生成的 `02_k8s_global_db.sql` 预建 + GRANT appuser;initdb 只对空 PVC 生效,存量 PVC 手工补。login ConfigMap 写死 `IdSegment.Enabled=true / FallbackToSnowflake=false`,所以**建角依赖 data-service + 这个库**;`Schema.AutoMigrate` dev=服务 yaml 值、staging/prod=false(部署阶段跑一次 `-migrate`)。data-service 仍按 zone namespace 各部署一份(catalogue 里不是 Global),但库与 go-zero 发现键都不分 zone。
- **Kafka 是单 broker StatefulSet(`sts/kafka`,Pod `kafka-0`)+ 一块 PVC + `PodDisruptionBudget minAvailable: 1`**,不是 Deployment(`routing-identity-audit-20260908.md` R06:emptyDir 上的 broker 一重启丢掉全部 topic / 分区元数据 / offset,停机窗口里的控制面命令在 librdkafka 5 分钟超时后全丢)。`kubectl exec ... deploy/kafka` 已失效,用 `sts/kafka`。`infra-up` 会先删旧的 `deployment/kafka`(Service selector 会同时命中新旧 Pod = 脑裂),**第一次切换会丢掉现有全部 topic 数据,切完必须重跑 `infra-kafka-topics`**。数据目录由 `KAFKA_LOG_DIRS=/var/lib/kafka/data` 显式指定(旧 manifest 把卷挂在 `/tmp/kafka-logs`,而镜像默认 `log.dirs` 是 `/tmp/kraft-combined-logs` —— 那块卷根本没被写过)。**刻意保持 `replicas: 1`**:所有 topic 都是 replication-factor 1,而且广播地址是 ClusterIP Service 名,单改副本数 = 没冗余还路由错乱。探针里必须保留 `KAFKA_HEAP_OPTS=-Xms16m -Xmx64m` 的覆盖(工具脚本会继承容器 env 的 `-Xmx1g`,叠在同一 memory limit 上必 OOMKilled);容器 `command`/`args` 是一层 `ulimit -n` 薄壳,改镜像版本要核 `/__cacert_entrypoint.sh /etc/kafka/docker/run` 还在不在。细节在 `README.md`「Kafka:StatefulSet + PVC」。
- **Kafka 审计 topic 由部署侧预建,不能靠自动创建**:`manifests/infra/kafka-topic-init.yaml` 一次性 Job(`infra-up` 在五个 infra manifest 之后、任何 zone 之前 apply;`-WaitReady` 等它 Complete),建 `transaction_log_topic_g<N>`(6 分区)/ `player_snapshot_topic_g<N>`(3 分区),值全从 `go/data_service/etc/data_service.yaml` 取。broker 是 auto-create + `num.partitions=1`,scene 抢先发消息就会把 topic 建成 1 分区,data-service 的 `EnsureTopics` 从此永远 mismatch。改分区数 = `Kafka.TopicGeneration` +1(新 topic),绝不原地扩;换成 PVC 之后普通重启不再丢 topic,但**第一次从旧 emptyDir Deployment 切过来 / `infra-down` 删过 PVC / 手工删过 topic 之后**,必须先跑 `k8s_deploy.ps1 -Command infra-kafka-topics -WaitReady` 再让 scene 产消息。compose 的 `kafka-topic-init` 是同一件事。细节在 `README.md`「Kafka 审计 topic 预建」。
- **Kafka 保留期只有一条规则**(`routing-identity-audit-20260908.md` R11):**消费者可能落后的 topic,保留期必须 > 消费者可能落后的最长时间并留足余量,而且必须显式声明、不许继承 broker 默认值。** C++ 消费者把 `max.poll.interval.ms` 钉在 900000(15 分钟),所以命令 topic 取 1 小时,`$KafkaBrokerRetentionMs`(给迁移窗口里 auto-create 的 `gate-<id>`/`scene-<id>` 兜底)取 30 分钟 / 1 小时。`--create --config retention.ms` 只在新建那一刻生效,所以两条 init 路径都在建完后再 `kafka-configs.sh --alter` 声明一次并读回核对。`db_task_zone_<N>` 有个额外陷阱:**login 与 db 都会在启动时对它 `IncrementalAlterConfigs`**,而 db 的 ConfigMap 不写 `RetentionMs`(走 Go 默认 24h)、login 写脚本注入值,两边不一致时保留期会随重启顺序横跳 —— 改 `$KafkaDbTaskRetentionMs` 必须与 `go/db/etc/db.yaml` 同拍。细节在 `README.md`「Kafka 保留期」。
- **恢复全局库 `mmorpg_global` 是 ID 安全事件**:`id_segment.max_id` 是发号水位,从备份恢复会把水位倒回去、把已发出的号再发一遍。恢复前必须核对每个 `biz_tag` 的 `max_id` ≥ 消费侧最大号并手工抬上去(player 看各 zone 库、item 看 blob、txlog/snapshot 看 Kafka),data-service ConfigMap 的 `IdSegment.AllowAutoSeed` 在 staging/prod 固定 `false` 就是为了缺行时拒绝领段而不是从 1 重来。步骤在 `README.md`「恢复全局库前须先核对 id_segment.max_id」。`manifests/go-svc/data-service.yaml` 是 `strategy: Recreate` + `replicas: 1`。
- **guild 没有 K8s ConfigMap / manifest**(`$GoSvcCatalogue` 与 `manifests/go-svc/` 都没有它)。补的时候要带 `ClusterId`、`SnowflakeCacheDir`(+ emptyDir)、`DataServiceRpc`、`IdSegment`,形状照 `go/guild/etc/guild.yaml`。
- **聚宝斋 trade = 全局池(infra namespace 一份)+ 建表 Job**:`manifests/go-svc/trade.yaml`(50800 / metrics 9230,只经路由服可达;K8s 默认 `-GateRouterMode 0` 下玩家不可达)与 **`manifests/go-svc/trade-migrate.yaml`**(port-decisions D-14 第 4 条:同镜像、同 ConfigMap、`-migrate`)。`$GoSvcCatalogue.trade.MigrateJob` 登记 Job;`Apply-OneGoSvc` 在 ConfigMap 之后、Deployment 之前 `Apply-GoSvcMigrateJob`(先删再 apply;门禁 **staging/prod 恒等、不受 `-WaitReady` 控制**(D-14 第 4 条),dev 只在 `-WaitReady` 下等:先等 `deploy/mysql` 再轮询 Job,Failed / 超时即中断发布,Deployment 不 apply)。**删之前先查上一次的 Job 是否在途**:迁移 runner 执行 DDL 前写 dirty=1,删在途 Job 会把台账留在 dirty、之后每次 `-migrate` 都以 1 失败 —— 所以存在的 Job 须有 Complete / Failed / FailureTarget 条件,且按该 Job UID 核对所属 Pod 全部为 Succeeded / Failed,才可删除;仍在途、状态未知或查询失败时在预算内等待,等不到就中断且不删;ACTIVE=0 不含 terminating Pod,不能单独作为安全删除判据;不要把 kafka-topic-init 的"无脑先删再建"照搬到有台账的 Job 上,也不要手工 delete 在途迁移 Job。Job 的 `podFailurePolicy`:退出码 1 / 4 → FailJob,3(锁忙)→ backoff 重试,**需 K8s ≥ 1.26 + `restartPolicy: Never`**;initContainer 用同镜像 busybox `nc` 等 3306(mysql 就绪探针走 socket,initdb 期间会谎报 Ready),预算 `WAIT_MYSQL_TIMEOUT_SECONDS=150`(`/proc/uptime` 单调计时,硬上限 +8s);脚本两段 Job 等待(删前在途检查、apply 后终态轮询)的预算 = `max(-WaitTimeoutSeconds, $GoSvcMigrateJobMinWaitSeconds=300)`,**改 initContainer 上限必须同步这个下限**,否则全新集群的正常等待会被误判超时、中断发布。库 `mmorpg_trade` **只登记在 `deploy/mysql-init/00_init_zone_dbs.sql`**(mysql-init-sql 原样带入),不要在脚本里再生成建库 sql;存量 PVC 手工补建 + GRANT(命令在 `README.md`「聚宝斋 trade」)。ConfigMap 的 `Mode` / `Schema.AutoMigrate` dev = 服务 yaml,staging/prod = `pro` / `false`。data-service `BootstrapTags` 已含 `trade_listing`,存量集群要先跑一次 data-service `-migrate` 建这一行(`AllowAutoSeed` 在 staging/prod 是 false)。以后的建表服务照 `MigrateJob` 字段加,不另写 Job 调度。
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
- Go-side routing is driven by `StrictNodeTypeSeparation` in `scene_manager_service.yaml`. Keep `true` in production; flip to `false` only during the rollout window described in `docs/ops/scene-node-role-split.md`.
- `-DryRun` 会把每份将 apply 的 manifest 打印在 `--- BEGIN MANIFEST --- / --- END MANIFEST ---` 之间(以前只打印临时文件路径,而该文件当场就被删,DryRun 等于无法验证)。

## AGONES(阶段 B 已落码,未上集群)
- 模型:**1 Agones GameServer = 1 个 Scene Node Pod / C++ 进程 = N 个动态创建的 ECS Scene 房间**。不要写成"一个 Scene 一个 GameServer"。设计文档:`docs/design/agones-scene-node-high-density.md`。
- 开关:`-SceneOrchestrator deployment|agones`,默认 `deployment`。**不做**"检测集群装没装 Agones 就自动切换"。
- agones 模式下 `scene-world` / `scene-instance` 生成 `agones.dev/v1 Fleet`,gate 仍是 Deployment。
- 内部服务,端口用 `portPolicy: None`;不加公网 Service / HostPort / NodePort / LB。SceneManager 继续从 etcd 拿 PodIP。
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
| 发布门禁 | 无 | `release-zone` / `release-all` 跑不可变 tag 校验 + `release_preflight.ps1` |
| 运行层内容 | `bin/{gate,scene,battle}`、`/usr/local/lib/*.so*`、`data/scene_nav_bin/`、表文件名转小写、`bin/zoneinfo` 软链 | 只有 staging 里的东西:`bin/{gate,scene,battle}`、`bin/zoneinfo/`、`generated/generated_tables/`(契约见 `runtime/README.md`) |

- 两条都是现役入口,旧文档里"生产只准用 `Dockerfile.runtime`"与"`Dockerfile.cpp` 是推荐路径"两种说法都不完整,以上表为准。版本化发布(`publish_images.ps1` → `make_release.ps1`)走 `Dockerfile.runtime`,C++ 分离符号取宿主 `bin/symbols/*.debug`。
- ⚠️ **两条路径的运行层不等价**:staging 契约里没有 `.so`、导航网格数据,也不做表名转小写,而 `Dockerfile.cpp` 对这三样各有一步。在 runtime 路径产出的镜像真实起过一次 scene 之前,不要假定二者可互换。
- 根 `.dockerignore` 以前排除了 `deploy/k8s/runtime/linux/`,`k8s-build-image` 的 `COPY ${RUNTIME_ROOT}/` 必然 not found;2026-09-16 已去掉该排除。
- 旧的 KNOWN BREAKAGE 已过时并删除:`tools/scripts/build_linux.sh` 已入库(提交 `31e4d1d4c`);`Dockerfile.cpp` 克隆 gRPC `v1.83.0` 并校验 revision,与 `.gitmodules` 的 `branch = v1.83.x` 一致。

## 发布打包
- 标准与分批方案:`docs/design/release-packaging-standard-20260914.md`;发布步骤清单:`docs/ops/release-checklist.md`。
- 制品根 = `MMORPG_ARTIFACT_ROOT`(缺省 `<仓库父目录>/artifacts`,**不在仓库内**);版本目录不可变、原子上线、带 `sha256sums.txt`;快照轨 `snapshots/images/g<sha12>/`,发布轨 `releases/images/vX.Y.Z/`,`releases/` 永不清理。
- 镜像 tag:给 `-Version vX.Y.Z`(或 `MMORPG_RELEASE_VERSION`)时为 `vX.Y.Z-<sha12>`,否则 `<sha12>`;发布轨拒绝脏树。镜像内 `/app/BUILD_INFO` 与 OCI label `org.opencontainers.image.revision` 自报 commit,`publish_images.ps1` 校验 label == 当前 commit。
- 发布集合只有 cpp / go / java 三族;robot、sandbox-mock 是测试工具镜像,不进制品目录。
- 推 registry 属于 AGENTS §9 受限操作:只推用户明确指定的 dev registry,AI 不执行 push。

```bash
# 快照轨(工作树必须干净):构建三族镜像 → 逐个 docker save → snapshots/images/g<sha12>/
pwsh -File tools/scripts/publish_images.ps1
# 发布轨:版本号注入镜像 → releases/images/v1.2.3/;CI 重跑加 -SkipBuild -SkipIfExists
pwsh -File tools/scripts/publish_images.ps1 -Version v1.2.3
# (可选)推送并记录 digest;k8s_image.ps1 push-image / java_svc_image.ps1 push 同样收 -Version / -DigestsOut
pwsh -File tools/scripts/go_svc_image.ps1 -Command push-all -Registry <registry> -Version v1.2.3 -DigestsOut ./image-digests.json
# release manifest:CHANGELOG.md 必须有 "## [1.2.3]" 段 → releases/manifests/v1.2.3.{md,json}
pwsh -File tools/scripts/make_release.ps1 -Version v1.2.3 -ImageDigestsFile ./image-digests.json
# 目标机:校验后落地(缺省 deploy/offline-images/v1.2.3)→ 离线导入并逐个核对镜像 ID
pwsh -File tools/scripts/fetch_images.ps1 -Channel release -Version v1.2.3
pwsh -File tools/scripts/import_images.ps1 -Dir deploy/offline-images/v1.2.3
# 快照清理(默认 dry-run,加 -Force 才删;releases/ 不碰)
pwsh -File tools/scripts/artifacts_retention.ps1 -KeepLast 10
```

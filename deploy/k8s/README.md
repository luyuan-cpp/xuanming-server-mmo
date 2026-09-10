# K8s One-Click Open-Server

This directory contains Kubernetes-only deployment assets for opening game zones.

## Scope

- `single zone`: open one zone (for example, `yesterday` zone).
- `all zones`: open all zones defined in a JSON or YAML config file.
- This flow is Kubernetes-only and does not include Docker Compose or local process startup scripts.

## Ops Runbook

- Day-2 operations and release/rollback steps: `docs/ops/k8s-open-server-runbook.md`.

## Directory Layout

- `manifests/infra/`: infra resources applied per namespace (`etcd`, `redis`, `kafka`, `mysql`). `etcd` is a 3-replica StatefulSet with one PVC per member (see "etcd:3 副本 StatefulSet" below) and `kafka` is a **single-broker** StatefulSet with one PVC (see "Kafka:StatefulSet + PVC" below); `redis` / `mysql` are still single-replica Deployments.
- `manifests/go-svc/`: Go micro-service K8s manifests (`db`, `data-service`, `login`, `player-locator`, `scene-manager`, `match`). `guild` has no manifest / ConfigMap yet (see "snowflake 缓存目录 / PlayerId 号段 / data_service 全局库" below).
- `Dockerfile.go-svc`: multi-stage Dockerfile for building Go service images.
- `zones.sample.json`: sample multi-zone definition file (JSON).
- `zones.sample.yaml`: sample multi-zone definition file (YAML).

## Quick Start

1. Ensure `kubectl` is installed and your kube context works.
2. Stage Linux runtime files under `deploy/k8s/runtime/linux/`.
3. Build and push your C++ node runtime image.
4. Run one of the commands below from repo root.

## Runtime Image Flow

- Production K8s image uses `deploy/k8s/Dockerfile.runtime`.
- Do not use the repository root `Dockerfile` as the production K8s runtime image.
- Required staging layout is documented in `deploy/k8s/runtime/README.md`.
- Current manifests assume Linux containers. Windows `.exe` outputs under `bin/` are not deployable to this image.

### Preflight Runtime Image

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-image-preflight
```

### Build C++ Nodes (Docker multi-stage — recommended)

```bash
# Build everything from repo root — outputs gate + scene Linux binaries
docker build -f deploy/k8s/Dockerfile.cpp -t mmorpg-nodes:latest .
```

Or build on a Linux host directly:

```bash
# 1. Build gRPC dependencies (one-time)
bash tools/scripts/build_grpc_linux.sh

# 2. Build gate + scene
bash tools/scripts/build_linux.sh --release
```

### Stage Runtime Files

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-stage-runtime `
  -BinarySourceRoot D:/linux-build/bin `
  -ZoneInfoSource bin/zoneinfo `
  -TableSource generated/tables
```

This copies Linux `gate` / `scene` binaries plus local `zoneinfo` and generated tables into `deploy/k8s/runtime/linux`.

### Build Runtime Image

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-build-image `
  -ImageRepository ghcr.io/luyuancpp/mmorpg-node `
  -ImageTag v1
```

### Push Runtime Image

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-push-image `
  -ImageRepository ghcr.io/luyuancpp/mmorpg-node `
  -ImageTag v1
```

### One-Command Release One Zone

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-release-zone `
  -ZoneName yesterday `
  -ZoneId 101 `
  -OpsProfile managed-cloud `
  -ImageRepository ghcr.io/luyuancpp/mmorpg-node `
  -ImageTag v1 `
  -WaitReady
```

## Go Micro-Service Image Flow

Go services use `deploy/k8s/Dockerfile.go-svc` (multi-stage build). Build context is the `go/` directory.
Each service gets its own image: `{registry}/mmorpg-{service}:{tag}`.

| Service | Image Name |
|---------|------------|
| db | `mmorpg-db` |
| data-service | `mmorpg-data-service` |
| login | `mmorpg-login` |
| player-locator | `mmorpg-player-locator` |
| scene-manager | `mmorpg-scene-manager` |
| match | `mmorpg-match` (global pool, deployed by `infra-up`, not per zone) |

### Build all Go service images

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build-images `
  -GoSvcRegistry ghcr.io/luyuancpp -GoSvcTag v1
```

### Push all Go service images

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-push-images `
  -GoSvcRegistry ghcr.io/luyuancpp -GoSvcTag v1
```

### Deploy Go services alongside a zone

Pass `-GoSvcRegistry` to include Go services in zone deployment.
Each service image is derived as `{GoSvcRegistry}/mmorpg-{svc}:{GoSvcTag}`:

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up `
  -ZoneName yesterday -ZoneId 101 `
  -OpsProfile managed-cloud `
  -NodeImage ghcr.io/luyuancpp/mmorpg-node:latest `
  -GoSvcRegistry ghcr.io/luyuancpp -GoSvcTag v1 `
  -WaitReady
```

Omit `-GoSvcRegistry` or pass `-SkipGoSvc` to deploy only C++ nodes.

## Java Service Image Flow

Java services use `deploy/k8s/Dockerfile.java-svc` (multi-stage build). Build context is the `java/<service>/` directory.

| Service | Image Name |
|---------|------------|
| gateway | `mmorpg-gateway` |

### Build Java service image

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command java-svc-build-image `
  -JavaSvcRegistry ghcr.io/luyuancpp -JavaSvcTag v1
```

### Push Java service image

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command java-svc-push-image `
  -JavaSvcRegistry ghcr.io/luyuancpp -JavaSvcTag v1
```

### Deploy Java services alongside a zone

Pass `-JavaSvcRegistry` to include Java services in zone deployment:

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up `
  -ZoneName yesterday -ZoneId 101 `
  -OpsProfile managed-cloud `
  -NodeImage ghcr.io/luyuancpp/mmorpg-node:latest `
  -GoSvcRegistry ghcr.io/luyuancpp -GoSvcTag v1 `
  -JavaSvcRegistry ghcr.io/luyuancpp -JavaSvcTag v1 `
  -WaitReady
```

Omit `-JavaSvcRegistry` or pass `-SkipJavaSvc` to skip Java service deployment.

## Ops Recommendation

- Config format: prefer YAML for daily operations. It is the normal Kubernetes ops format and easier to review during change windows.
- Keep JSON only for tool interoperability or automated generators.
- External gate exposure:
  - Managed cloud K8s: prefer `-GateServiceType LoadBalancer`.
  - Self-hosted / bare metal K8s: prefer `-GateServiceType NodePort` behind an external L4 load balancer.
  - Exposing the Service is only half of it, and the missing half is **not** in this script: clients never learn gate's address from the Service. `login` reads gate's etcd-registered endpoint — the cluster-internal `POD_IP` — and hands it to the client verbatim (`go/login/internal/svc/servicecontext.go`, `CandidatesForZone`). Until `login` is taught to translate `node_id` to an externally reachable address, external clients cannot connect no matter how the Service is configured. The deploy script warns on any non-`ClusterIP` setting for this reason.
  - The gate process deliberately does not try to discover its own external address. It binds `0.0.0.0` and registers its pod identity; deciding what the outside world should dial is the job of whoever hands the address out.
- Do not treat `LoadBalancer` as the universal default. If the cluster does not have a mature, production-grade LB implementation, `NodePort` plus an external L4 balancer is usually the more stable choice.
- Internal-only services should stay inside the cluster and do not need external exposure.
- Stability baseline per zone: `centre=1`, `gate=2`, `scene=4`.
- You can encode this choice directly with `-OpsProfile managed-cloud` or `-OpsProfile bare-metal`.

### Open One Zone

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up `
  -ZoneName yesterday `
  -ZoneId 101 `
  -OpsProfile managed-cloud `
  -WaitReady `
  -NodeImage ghcr.io/luyuancpp/mmorpg-node:latest
```

### Check One Zone

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-status -ZoneName yesterday
```

### Close One Zone

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName yesterday
```

## Open All Zones (One-Click)

1. Copy either `deploy/k8s/zones.sample.json` to `deploy/k8s/zones.json`, or
  `deploy/k8s/zones.sample.yaml` to `deploy/k8s/zones.yaml`.
2. Edit zone names, IDs, and replica counts.
3. Run:

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-all-up `
  -ZonesConfigPath deploy/k8s/zones.yaml `
  -OpsProfile managed-cloud `
  -NodeImage ghcr.io/luyuancpp/mmorpg-node:latest
```

### Check All Zones

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-all-status -ZonesConfigPath deploy/k8s/zones.yaml
```

### Close All Zones

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-all-down -ZonesConfigPath deploy/k8s/zones.yaml
```

## Runtime Behavior

- Namespace naming: `<NamespacePrefix>-<ZoneName>` (default prefix is `mmorpg-zone`).
- Node config (`base_deploy_config.yaml`, `game_config.yaml`) is generated into a `ConfigMap` per zone.
- `ClusterId` (from `-ClusterId`, default `0`) is written into that ConfigMap's `base_deploy_config.yaml` and into the `login` / `scene-manager` / `match` Go ConfigMaps. It is the high 5 bits of the snowflake worker field (`docs/design/node-id-overhaul-plan-20260908.md` §5): one value per Kubernetes cluster, set once by ops, never per zone.
- Node pods use:
  - `POD_IP` from Kubernetes Downward API.
  - `RPC_PORT`/`NODE_PORT` env vars (fixed per role: gate `18000`, scene `20000`). The value must fall inside the engine's per-role TCP range — gate `10000-19999`, everything else `20000-35535` — since the gRPC port is derived as TCP+30000. A node now refuses to start (retrying, nothing published to etcd) if the requested port is taken, rather than silently picking a different one.
  - `GRPC_SERVER_MAX_POLLERS` 用于限制 gRPC server poller 数（默认 `8`，与 C++ 进程默认值一致）。传 `-GrpcServerMaxPollers 0` 时，Deployment 与 Fleet 都不写该环境变量，交给进程默认值。
- A `gate-entry` Service is created per zone namespace for external TCP access.

## Optional Flags

- `-SkipInfra`: deploy only node workloads (skip `etcd`/`redis`/`kafka`).
- `-ClusterId`: deployment-level cluster number, `0..31`, default `0`. Becomes the `cluster` segment of every snowflake id minted in this cluster, so `infra-up` and every `zone-up` / `all-up` against the same cluster must pass the same value (the script reads one parameter for both paths; `zones.json` deliberately has no per-zone override). Leave it at `0` unless you are standing up a second, independent cluster that must never collide with the first. Only `tools/scripts/k8s_deploy.ps1` accepts it today; `dev_tools.ps1`'s `k8s-*` wrappers do not forward it yet.
- `-DryRun`: print kubectl commands without applying.
- `-WaitReady`: wait for `centre` / `gate` / `scene` deployments to roll out. On `infra-up` / `all-up` it also waits for the `etcd` StatefulSet to reach quorum before the global `match` service is applied, and then for the `kafka-topic-init` Job to complete, so the audit topics exist with their contracted partition counts before any zone is applied (see "Kafka 审计 topic 预建" below). `infra-kafka-topics -WaitReady` waits for the same Job.
- `-WaitTimeoutSeconds`: rollout wait timeout per deployment.
- `-KubeContext`: pass an explicit kube context.
- `-KubeConfig`: pass an explicit kubeconfig path.
- `-NamespacePrefix`: change namespace prefix.
- `-CentreReplicas`, `-GateReplicas`, `-SceneReplicas`: per-zone replica overrides for `k8s-zone-up`.
- `-GrpcServerMaxPollers`：每个 C++ 节点的 gRPC server poller 上限（默认 `8`，与进程默认值一致）。设为 `0` 时，Deployment 与 Fleet 都不写环境变量覆盖。
- `-GateServiceType`: `ClusterIP`, `NodePort`, or `LoadBalancer`.
- `-GateServicePort`: external gate service port.
- `-OpsProfile`: `managed-cloud`, `bare-metal`, or `custom`.
- With `-OpsProfile custom`, the baseline default is `-GateServiceType NodePort` unless overridden.
- `-GoSvcRegistry`: Docker registry for Go micro-services (e.g. `ghcr.io/luyuancpp`). If not set, Go services are skipped.
- `-GoSvcTag`: Image tag for Go service images (default: `latest`).
- `-SkipGoSvc`: explicitly skip Go service deployment even if `-GoSvcRegistry` is set.

## Important Notes

- The default node image is a placeholder. Replace `-NodeImage` with your real image.
- The script assumes Linux containers and `/app/bin` runtime layout.
- Deleting a zone currently deletes the entire namespace (`k8s-zone-down`).
- Do not use `LoadBalancer` in production unless the cluster provides a real, mature LB implementation. Otherwise use `NodePort` plus an external L4 balancer.

## etcd:3 副本 StatefulSet + PVC(2026-09-08,node-id-overhaul-plan §7 Phase 0.5)

`manifests/infra/etcd.yaml` 现在是:ClusterIP Service `etcd`(仍是 `etcd.<InfraNamespace>:2379`,所有 ConfigMap 不用改)
+ headless Service `etcd-headless`(成员间 peer 地址)+ `PodDisruptionBudget minAvailable: 2`
+ StatefulSet `etcd` 3 副本、`podManagementPolicy: Parallel`、每成员一块 `8Gi` PVC(集群默认 StorageClass)。
成员表是静态的(`etcd-0/1/2.etcd-headless.<ns>.svc.cluster.local:2380`),成员重启后凭 PVC 里的数据以原身份回到集群。

- **为什么**:所有节点 id / 发号槽位都是 etcd lease 绑定的。单副本 etcd 一重启(哪怕只是 Pod 漂移),全部 lease 消失,
  每个 C++ / Go 节点都会收到 ownership lost 并自 fence 退出。验收口径:`kubectl -n mmorpg-infra delete pod etcd-1`,
  没有任何 gate / scene / login 被 fence。
- **首次从旧单副本切换**:旧的 `Deployment etcd` 用的是 emptyDir,里面的数据**不迁移**,也不需要迁移 ——
  那里只有 lease 绑定的节点注册键与 snowflake 槽位、killswitch 规则,没有任何玩家数据(玩家数据在 MySQL / Redis)。
  `infra-up` 会先 `kubectl delete deployment etcd --ignore-not-found` 再 apply StatefulSet
  (两者 kind 不同、名字相同,`apply` 不会回收旧 Deployment;而 Service 的 selector 会同时命中新旧 Pod,不删就是脑裂)。
  切换期间 etcd 短暂不可用;切完之后**所有节点必须重启一次重新注册**(node id 都在启动时重新申领,
  `kubectl -n <zone-ns> rollout restart deploy` 即可)。killswitch 规则(`/mmorpg/killswitch/...`)如果有,需要重新 put。
- **`etcdctl` 怎么进**:不再有 `deploy/etcd`,用 StatefulSet 或指定成员:
  `kubectl exec -n mmorpg-infra sts/etcd -- etcdctl endpoint status --cluster -w table`(任一成员)或
  `kubectl exec -n mmorpg-infra etcd-0 -- etcdctl get --prefix --keys-only LoginNodeService.rpc/`。
- **PVC 生命周期**:删 StatefulSet 不删 PVC,删 namespace(`infra-down`)才删。`infra-down` → `infra-up` 之后是一套空 etcd,
  同样按上一条重启节点。
- **kind / minikube**:单节点也能跑(反亲和是 preferred),但 PVC 要有默认 StorageClass(kind 自带 local-path)。
  `infra-status` 现在会列 `sts` / `pvc` / `pdb`;etcd 起不来先看 PVC 是否 Pending。

## snowflake 缓存目录 / PlayerId 号段 / data_service 全局库(2026-09-08,node-id-overhaul-plan §3.5 / §6 / §2.0c)

生成器(`tools/scripts/k8s_deploy.ps1`)与静态 manifest 新增的键。凡是"脚本常量 + 静态文件写死"的组合,改一处必须同步改另一处。

- **`SnowflakeCacheDir: /tmp/snowflake`**(`login` / `scene-manager` / `match` 的 Go ConfigMap)与 **`SNOWFLAKE_CACHE_DIR=/tmp/snowflake`**
  (C++ `gate` / `scene` 的 Deployment 与 Agones Fleet 的环境变量;只有 scene 启用发号槽会写,gate 收到什么也不做)。
  路径常量在脚本顶部 `$SnowflakeCacheDir`;`manifests/go-svc/{login,scene-manager,match}.yaml` 与生成的 C++ Deployment / Fleet
  都在同一路径挂一个 **`emptyDir`**(卷名 `snowflake-cache`)。它是每个 Pod 自己的启动期草稿 —— etcd 不可达时凭它续用上次的槽位(2h 内),
  不跨 Pod 共享、不需要持久卷,Pod 重建就重新申领。Go 默认的 `../../run/snowflake` 在容器里落到 `/run/snowflake`,
  能不能写取决于镜像是否以 root 跑、根文件系统是否只读,部署侧不赌这个。
  三份 go-svc manifest 的 `mountPath` 是写死的(脚本不替换),改 `$SnowflakeCacheDir` 时一起改。
- **login ConfigMap 新增 `DataServiceRpc` + `IdSegment`**:`DataServiceRpc` 走 etcd 发现 `dataservice.rpc`,`Timeout: 3000`,
  `NonBlock: true`(弱依赖:data-service 没起 login 照常起服);`IdSegment: { Enabled: true, Step: <go/login/etc/login.yaml 的 IdSegment.Step>, FallbackToSnowflake: false }`。
  `Enabled` 必须**显式写 true**(整块不写 = 静默回到旧的纯 snowflake 发号,不报错);`Step` 从服务 yaml 取,`Enabled` / `FallbackToSnowflake` 是部署决策,模板里写死并注释。
  后果:**建角要求 data-service 可达且全局库可用**,领段失败即建角失败(§6.4 的决定,不回退 snowflake)。
  data-service 目前仍按 zone namespace 各部署一份(`$GoSvcCatalogue` 里不是 Global),但 go-zero 发现键 `dataservice.rpc` 不分 zone、全局库也只有一份 ——
  多实例共用一张 `id_segment` 表是设计内的(store 里 `SELECT ... FOR UPDATE` 把同 `biz_tag` 的领段串行化,再以 `version` 兜底),不需要改。
- **data-service ConfigMap 新增 `SnapshotMySQL` / `Schema` / `Kafka`**(以前没有这三段,Go 默认 `127.0.0.1:3306/testdb`,K8s 上必然连不上,三个 store 置 nil):
  - `SnapshotMySQL`:`mysql.<InfraNamespace>:3306`,账号沿用 db 服务同一组注入值(`MMORPG_MYSQL_USER` / `MMORPG_MYSQL_PASSWORD`,dev 回落 root),
    `DBName` = 脚本顶部 `$GlobalDbName`(`mmorpg_global`,刻意不叫 testdb)。
  - `Schema.AutoMigrate`:dev 档取服务 yaml 的值(`true`,起了就能用);staging / prod 固定 `false`(多副本并发 ALTER 互相 MDL 阻塞),
    部署阶段单进程跑一次 `data_service -f /app/etc/data_service.yaml -migrate`,与 db 服务的 `AutoMigrateSchema` 同一条纪律。
  - `Kafka`:brokers `kafka.<InfraNamespace>:9092`;topic 名 / 分区数 / 消费组 / 保留期全部从 `go/data_service/etc/data_service.yaml` 取
    (分区数是不可变契约,`EnsureTopics` 会拒绝与 broker 现状不一致的值)。Kafka 不可达不影响 Load / Save,只记日志并后台重试。
  - `Kafka.TopicGeneration`:同样从服务 yaml 镜像(当前 `1`),有效 topic 名 = `<基名>_g<N>`,当前集群消费第几代 topic 在 ConfigMap 里一眼可见;
    改分区数 = 这个数 +1,绝不原地扩分区,见下面「Kafka 审计 topic 预建」。
  - `IdSegment`:`AllowAutoSeed` dev 档取服务 yaml 的值(`true`),staging / prod 固定 `false`(与 `Schema.AutoMigrate` 同一条纪律;为什么见下面「恢复全局库」一条);
    `BootstrapTags: [player, guild, item, txlog, snapshot]` 与服务 yaml / Go 的 `DefaultIdSegmentBootstrapTags` 同一份清单,新增永久身份两边同加。
  - `manifests/go-svc/data-service.yaml` 的 Deployment 现在是 `strategy: Recreate` + `replicas: 1`:快照消费者按 guid 去重是「单条语句 + 间隙锁」,
    两个实例重叠不会写坏数据,但会互相等锁、消费组反复 rebalance,没有任何好处;换版本时短暂停一下,消息留在 topic 里(30 天保留期)。
- **全局库预建**:`infra-up` 的 `mysql-init-sql` ConfigMap 现在多生成一份 `02_k8s_global_db.sql`
  (`CREATE DATABASE IF NOT EXISTS mmorpg_global` + `GRANT ALL ... TO 'appuser'@'%'`),data-service 启动期 / `-migrate` 在里面按 proto 建
  `transaction_log` / `player_snapshot` / `rollback_audit_log` / `id_segment` 四张表。全集群一份,不按 zone 拆。
  initdb 只在 mysql PVC 为空时执行:**存量 PVC 要手工执行这两句**
  (`kubectl exec -n mmorpg-infra deploy/mysql -- sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -e "CREATE DATABASE IF NOT EXISTS mmorpg_global; GRANT ALL PRIVILEGES ON mmorpg_global.* TO '"'"'appuser'"'"'@'"'"'%'"'"';"'`),
  否则 data-service 只剩 `schema auto-migrate failed`,login 建角全部失败。
  本地 compose 的对应物是 `deploy/mysql-init/00_init_zone_dbs.sql` 顺带建的 `testdb`(`go/data_service/etc/data_service.yaml` 用),同样只对空数据卷生效。
- **恢复全局库前须先核对 `id_segment.max_id` ≥ 消费表最大号(这是 ID 安全事件,不是普通的库恢复)**:`id_segment` 每行是一种永久身份的发号水位
  (`biz_tag` = `player` / `guild` / `item` / `txlog` / `snapshot`;`max_id` 是下一个要发出去的号,领段 = `SELECT ... FOR UPDATE` 后 `max_id += step`)。
  把 `mmorpg_global` 从备份恢复,就是把水位倒回备份时刻,而备份之后领走的号早已成了消费侧的真实主键(各 zone 库 `player_database.player_id`、
  `guild.guild_id`、玩家 blob 里 bag 的 item guid、Kafka 两个 topic 里的 tx_id / snapshot_id)—— 水位一回退,下一次领段就把这些号**再发一遍**:
  login 的 `INSERT ... ON DUPLICATE KEY UPDATE` 会静默覆盖别人的角色行,流水表的 `INSERT IGNORE` 会静默丢掉**新**流水。顺序必须是:
  1. 先停 data-service,并重启所有持有号段的进程(login / guild / scene):它们内存里还攥着恢复点之后领到的段,不清掉,抬水位也挡不住它们把段发完。
  2. 对每个 `biz_tag` 拿到消费侧最大号:`player` = **每个 zone 库**的 `SELECT MAX(player_id) FROM player_database` 取最大;`guild` = `SELECT MAX(guild_id) FROM guild`;
     `item` = 各 zone 库玩家 blob 里 bag 的最大 guid;`txlog` / `snapshot` = Kafka `transaction_log_topic_g<N>` / `player_snapshot_topic_g<N>` 里的最大 tx_id / snapshot_id
     (`transaction_log` / `player_snapshot` 表与 `id_segment` 同库,恢复后同库 `MAX()` 不是独立证据)。
  3. `SELECT biz_tag, max_id FROM id_segment`,凡 `max_id` ≤ 对应最大号的:`UPDATE id_segment SET max_id = <最大号> + 1 + 余量 WHERE biz_tag = '<tag>'`(余量 ≥ 一个 step)。
  4. 然后才跑 `-migrate` / 起 data-service。`-migrate` 自带的地板校验(`raiseIdSegmentFloor`)只在**同一个库**里查得到 `player_database` / `guild` 时才抬水位 ——
     K8s 上 player 表在各 zone 库、全局库里没有,它只会打 Info,不代表安全;`item` / `txlog` / `snapshot` 根本没有自动校验。
  `IdSegment.AllowAutoSeed` 在 staging / prod 固定 `false` 正是为这一刻:行不见了(恢复到更早的备份 / 库被 drop 重建)时 data-service 不会自作主张从 1 播种,
  而是 `ErrCodeIdSegmentUnknownTag` 拒绝领段、不写任何行,把决定留给人;dev 档 `true` 只是让空库起了就能建角。
- **guild 还没有 K8s ConfigMap / manifest**(存量缺口:`$GoSvcCatalogue` 没有它,`manifests/go-svc/` 也没有 `guild.yaml`,`go_svc_image.ps1` 之外没人碰它)。
  补的时候除了 MySQL DataSource / Redis 之外,还要写 `ClusterId`、`SnowflakeCacheDir`(+ 同名 emptyDir)、`DataServiceRpc`、`IdSegment`,
  形状照 `go/guild/etc/guild.yaml`;guild_id 号段与 PlayerId 走同一个 `AllocateIdSegment`、同一张 `id_segment` 表(不同 `biz_tag`)。

## Kafka 审计 topic 预建(kafka-topic-init,2026-09-08,node-id-overhaul-plan §2.0c)

data_service 的两条落库消费者(`transaction_log_topic` / `player_snapshot_topic`,C++ scene 生产、key=player_id)对分区数有**不可变契约**
(`go/data_service/etc/data_service.yaml` 的 `Kafka.TransactionLogPartitions: 6` / `Kafka.SnapshotPartitions: 3`,`kafkautil.EnsureTopics` 会拒绝与 broker 现状不一致的值)。
而 broker(compose 与 `manifests/infra/kafka.yaml`)都开着 `auto.create.topics.enable` 且 `num.partitions=1`:只要 scene 先于预建发出第一条消息,
topic 就被自动建成 1 分区,EnsureTopics 从此永远报 `partition contract mismatch`,两条消费者一条都起不来,唯一症状是每 30s 一条 Error 日志,
流水 / 快照全被保留期吃掉。所以 topic 必须由**部署侧**在任何 scene 起来之前按契约预建,不能依赖自动创建:

- **有效 topic 名带代号**:`<基名>_g<Kafka.TopicGeneration>`,当前是 `transaction_log_topic_g1`(6 分区)/ `player_snapshot_topic_g1`(3 分区),
  `retention.ms` = `Kafka.RetentionMs`(30 天)。生产者(C++)与消费者(data-service)必须用同一个有效名。
- **compose**:`deploy/docker-compose.yml` 的 `kafka-topic-init` 一次性容器在 per-node topic 之外预建这两个 topic(`create_topic` 现在带分区参数;
  代号来自 `KAFKA_INIT_AUDIT_TOPIC_GENERATION`,默认 1),建完核对 `PartitionCount`,不符则退出 1(`docker compose ps -a` 里能看到)。
- **K8s**:`manifests/infra/kafka-topic-init.yaml` 是一个一次性 Job,`infra-up` 在五个 infra manifest 之后、任何 zone 之前 apply 它
  (`Apply-KafkaTopicInitJob`);占位 `__KAFKA_AUDIT_*__` 由脚本从 `go/data_service/etc/data_service.yaml` 填(基名 / 分区数 / 保留期 / 代号,
  与 data-service ConfigMap 同源,manifest 里不写任何数字)。Job 自己等 broker 可达(最多 5 分钟),`kafka-topics.sh --create --if-not-exists`,
  再 `--describe` 核对 `PartitionCount`,不符即失败(`backoffLimit: 6`,不会假装成功)。每次 `infra-up` 都先
  `kubectl delete job kafka-topic-init --ignore-not-found` 再 apply —— Job template 不可变,而且必须能重跑(见下面「Kafka Pod 重启」)。
- **`-WaitReady` 会等它 Complete**(`infra-up` / `all-up`,在 etcd quorum 之后):这样 `all-up` 里的 zone 一定晚于 topic 建好。
  不带 `-WaitReady` 时,`zone-up` 之前自己核对 `kubectl -n mmorpg-infra get job kafka-topic-init`(COMPLETIONS `1/1`)/
  `kubectl -n mmorpg-infra logs job/kafka-topic-init`;`infra-status` 现在会列 `job`。
- **改分区数 = 代号 +1,绝不原地扩分区**:原地 `--alter --partitions` 会重映射 key=player_id 的哈希,同一玩家的流水顺序就断了,
  而且 EnsureTopics 的 `__mmorpg_partition_contract_*` 标记 topic 也会与新值冲突。正确做法:`data_service.yaml` 里 `Kafka.TopicGeneration` +1、
  新分区数写进 `Kafka.*Partitions`(C++ 生产者的有效名同步换代)→ `infra-up`(或 `infra-kafka-topics`)建出 `*_g2` → 滚 data-service 与 scene
  → 老 topic 排空(消费者 lag 为 0)后再 `--delete`。data-service ConfigMap 里镜像了 `Kafka.TopicGeneration`,当前集群消费第几代一眼可见。
- **什么时候必须重跑预建**:Kafka 已经是 StatefulSet + PVC(见下面「Kafka:StatefulSet + PVC」),**普通的 Pod 重启不再丢 topic**。
  仍然必须重跑的是这三种情况:① 第一次从旧的 `Deployment kafka`(emptyDir)切过来;② `infra-down` 删过 namespace(PVC 跟着删);
  ③ 有人手工删过 topic。这三种情况下 topic 是空的,任何在跑的 scene 一发消息就会把 `*_g1` 自动建成 1 分区。
  做法是**在让 scene 产消息之前**跑 `pwsh -File tools/scripts/k8s_deploy.ps1 -Command infra-kafka-topics -WaitReady`
  (止血路径,不过发布门禁;`dev_tools.ps1` 还没透传这个命令),之后再 `rollout restart` 各 zone 的 scene 与 data-service。
  如果晚了一步、Job 因 `PartitionCount=1` 失败,删掉那个被抢建的 topic 再重跑:
  `kubectl -n mmorpg-infra exec sts/kafka -- /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --delete --topic <名>`。
- **`kubectl exec deploy/kafka` 已失效**:broker 是 StatefulSet,用 `sts/kafka` 或 Pod 名 `kafka-0`。
- 核对 broker 现状:`kubectl -n mmorpg-infra exec sts/kafka -- /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --describe --topic transaction_log_topic_g1`
  (看 `PartitionCount: 6` 与 `Configs: retention.ms=2592000000`)。
- **保留期是声明出来的,不是继承来的**:`--create --config retention.ms=...` 只在**新建那一刻**生效,已存在的 topic
  (被生产者抢先 auto-create 的、或上一代 Job 用别的值建的)会静默吃 broker 默认保留期。所以 Job 与 compose 的 init
  在建完之后都会再用 `kafka-configs.sh --alter --add-config retention.ms=...` 声明一次并读回核对,不一致就失败。
  与分区数不同,`retention.ms` 是可变配置,重设它没有任何寻址风险。规则见下面「Kafka 保留期」。

## 控制面命令 topic:per-node topic 已停建(2026-09-08)

同一个 `kafka-topic-init` 现在还预建两个**控制面命令 topic**:`gate-cmd_g<N>` 与 `scene-cmd_g<N>`,
分区数取自 `bin/etc/base_deploy_config.yaml` 的 `Kafka.CommandTopicPartitions`(当前 256)/
`Kafka.CommandTopicGeneration`(当前 1),`retention.ms=3600000`(1 小时)。
完整论证见 `docs/design/control-plane-topic-partitioning-20260908.md`。

- **`gate-{id}` / `scene-{id}` / `centre-{id}` 这些 per-node topic 不再被创建**。compose 里那三个
  `KAFKA_INIT_GATE_COUNT` / `KAFKA_INIT_SCENE_COUNT` / `KAFKA_INIT_CENTRE_COUNT` 循环已经删掉,
  K8s 侧本来也没建过。**已经存在的旧 topic 不需要删**:生产者切换后没人再写,按各自的 `retention.ms`
  自然过期;想早点回收磁盘就手工 `--delete`(先确认 `--describe --group gate-group-<id>` 已无成员)。
- **为什么改**:一个节点一个 topic 在 ~10 万 scene 进程的目标规模下,单集群 10 万 topic 会压垮
  controller 元数据、每分区内存与 rebalance 时间;更要命的是 topic 名和 consumer group 名里都带
  被 `node_allocator` 立刻回收复用的 `node_id`,冻结但仍是 group 成员的前任会和继任者一起待在
  `gate-group-5` 里,Kafka 只把分区判给其中一个 —— 判给前任时,继任者的登录绑定 / 进世界 / 踢人 /
  战斗绑定与结算全部被前任消费掉再按 `target_instance_id` 丢弃,继任者一条也收不到。
  现在按 `partition = node_id % P` 寻址、消费端 `assign` 自己的分区不进 group,这条路径就不存在了。
- **分区数是不可变契约**,与审计 topic 同一条纪律:改分区数 = `Kafka.CommandTopicGeneration` +1
  换一批新 topic,绝不原地 `--alter --partitions`(会把 `node_id % P` 重映射,命令落到没人 assign
  的分区上,静默全丢而且 Kafka 不报错)。改的时候四处必须同拍:`bin/etc/base_deploy_config.yaml`、
  `go/shared/kafkacmd/command_topic.go` 的默认值(或 `KAFKA_COMMAND_TOPIC_PARTITIONS` /
  `KAFKA_COMMAND_TOPIC_GENERATION` 环境变量)、`deploy/docker-compose.yml` 的
  `KAFKA_INIT_COMMAND_*`、以及本 Job(值由 `Apply-KafkaTopicInitJob` 从 base_deploy_config 读)。
- **消费端有门禁**:gate/scene 启动时向 broker 核对该 topic 的分区数,不等于契约就**起不来**
  (`KafkaConsumer::init` 的 `partition contract mismatch`)。所以 topic 被生产者抢先 auto-create 成
  1 分区时不会静默降级,而是节点起不来 + Job 失败,两处都能看见。
- **迁移窗口**:`Kafka.DisableLegacyPerNodeTopic` 默认 `false`,gate/scene 在消费新 topic 的同时
  继续用老的 group 订阅方式消费 `gate-{id}` / `scene-{id}`。全部生产者(含 Go login)切到新 topic 之后
  才置 `true`;在那之前老路径上的 P1 仍然存在。

## Local Trial on kind (2026-09-03 实跑记录)

`infra-up` 在本机 kind(v0.33.0 / node v1.37.0,Docker Desktop 29 引擎)上跑通 A 档
(etcd / redis / redis-match-cluster / kafka / mysql + 全局 match)时踩到的坑,都是
kind 本地环境特有,不影响真集群:

```powershell
. E:\work\tools\buildenv.ps1                       # GOPROXY / go 工具链
go install sigs.k8s.io/kind@latest                 # 装到 $(go env GOPATH)\bin
kind create cluster --name mmorpg                  # kubectl context 自动切到 kind-mmorpg
pwsh -File tools/scripts/go_svc_image.ps1 -Command build-all -Registry local -Services match
kind load docker-image local/mmorpg-match:<tag> --name mmorpg
pwsh -File tools/scripts/k8s_deploy.ps1 -Command infra-up -GoSvcRegistry local -ZoneId 101
```

- **kind 节点不共享宿主 Docker 的镜像缓存/镜像源**,直接从 Docker Hub 拉基础镜像
  (redis / kafka / mysql)极慢,而 kubelet 默认串行拉镜像,一个卡住的拉取会把
  所有 Pod 钉在 `ContainerCreating`。把宿主已有镜像 `kind load` 进去即可解开。
- **Docker 29 的 containerd 镜像存储下 `kind load docker-image` 对 Hub 拉下来的多平台
  镜像会失败**(`ctr: content digest sha256:...: not found`,因为 `docker save` 输出
  的 index 引用了本地没有的其他平台清单)。改用
  `docker save --platform linux/amd64 -o x.tar <image>` + `kind load image-archive x.tar`。
  本地 `docker build` 出来的单平台镜像(Go 服务)不受影响。
- `kind load` 之后老 Pod 仍卡在原来的拉取请求上时,直接 `kubectl delete pod`
  让控制器重建,新 Pod 走 `IfNotPresent` 立刻起来。
- `mysql-backup-pvc` 是 ReadWriteMany,kind 自带的 local-path 不支持,会一直 Pending;
  只有备份 CronJob 用它,mysql 本体不受影响。
- go/db 的镜像以前在任何环境都构建不了:`go/db/go.mod` 把 proto2mysql replace 到仓库外
  (`../../../proto2mysql`),以 `go/` 为 build context 带不进去。B 档已修:见下一节。

### B 档:zone-up(2026-09-03 实跑记录)

在 A 档 infra 之上把 `yesterday`(zone_id=1)拉起来:Go 五服务 + Java 网关,C++ 节点用占位镜像。

```powershell
. E:\work	oolsuildenv.ps1
# Go 服务镜像(db 需要宿主上检出 E:\work\proto2mysql;所有服务都会打包 generated/tables)
pwsh -File tools/scripts/go_svc_image.ps1 -Command build-all -Registry local -Tag <tag>
# Java 网关镜像(Dockerfile.java-svc 的 build context 是 java/gateway_node)
pwsh -File tools/scripts/java_svc_image.ps1 -Command build -Registry local -Tag <tag>
# C++ 节点本档不构建:用任意小镜像占位,gate/scene Pod 会 CrashLoop/RunContainerError,属预期
docker tag busybox:1.36 local/mmorpg-node:<tag>
# 逐个 docker save --platform linux/amd64 + kind load image-archive(见上节)
pwsh -File tools/scripts/k8s_deploy.ps1 -Command zone-up -ZoneName yesterday -ZoneId 1 `
    -GoSvcRegistry local -JavaSvcRegistry local -NodeImage local/mmorpg-node:<tag> `
    -GateReplicas 1 -SceneReplicas 1
```

- `-NodeImage` 显式给出时 go/java tag **跟随它的 tag**,所以占位镜像的 tag 必须与
  Go/Java 镜像一致(或显式传 `-GoSvcTag/-JavaSvcTag`)。
- 不要带 `-WaitReady`:它先等 gate,占位镜像的 gate 永远不 Ready 会直接超时。
- 实跑修掉的四个真 bug(都不是 kind 特有):
  1. **Go 镜像里没有策划表**:login / player-locator / scene-manager 启动 `LoadTables`
     (TableDir 默认 `../../generated/tables`)直接 `log.Fatalf` CrashLoop。
     `Dockerfile.go-svc` 现在以 `--build-context tables=<仓库>/generated/tables` 带入
     并落到 `/generated/tables`(WORKDIR /app 向上归一化正好命中默认值);同时**文件名转小写**
     —— 生成的 loader 读 `actoractioncombatstate.json`,导表产物却是 `ActorActionCombatState.json`,
     Windows 不分大小写所以本地从没暴露,Linux 一定读不到。根因在导表/生成器命名不一致,镜像里只是兜底。
  2. **db 的 gRPC 服务从未 Start**:`go/db/db.go` 自 fc9377336 起只 `MustNewServer` 没 `Start()`,
     进程打印 STARTED 却不监听 6000、不注册 etcd `db.rpc`;compose 没探针一直没暴露,K8s 的
     grpc readiness/liveness 永远超时 → 0/1 + 反复重启。已补 `go s.Start()`。
  3. **dev 档 login 拒启**:生成器 dev 档回落占位密钥,而 login 的 secrets 门禁只在 go-zero
     `Mode=dev/test` 才降级为 WARN,ConfigMap 里没写 Mode(默认 pro)→ panic
     "HMAC 密钥配置不合格"。现在 dev 档 login ConfigMap 写 `Mode: dev`(与 go/login/etc/login.yaml 一致),
     staging/prod 不写,门禁不变。
  4. **`-WaitReady` 会等根本没部署的 auth**:`$JavaSvcCatalogue` 有 auth 条目但
     `manifests/java-svc/auth.yaml` 不存在,Apply 只告警跳过,Wait 却照等 → 必超时。已改为 manifest 不存在则跳过。
- **占位镜像别用节点上 kubelet 已经拉过的镜像 ID 重打 tag**:一开始 `docker tag busybox:1.36 local/mmorpg-node:<tag>`
  再 `kind load`,gate/scene 一直 `ErrImagePull`(kubelet 明明配了 IfNotPresent 还去 Docker Hub 拉
  `docker.io/local/mmorpg-node`)。原因是 k8s 1.33+ 的 `KubeletEnsureSecretPulledImages`:
  `/var/lib/kubelet/image_manager/pulled/` 里有 busybox 那个镜像 ID 的 `ImagePulledRecord`
  (credentialMapping 只有 `busybox`),同一个镜像 ID 换个仓库名引用就必须重新验证拉取权限。
  换一个节点上从未被 kubelet 拉过的镜像(alpine:3.20)做占位就正常了;`crictl inspecti` 能查到不等于 kubelet 会用。
- 镜像构建慢的两个来源:`go mod download` 每个服务重下一遍(Dockerfile 无 cache mount);
  temurin 基础镜像从 Docker Hub 直拉约 100KB/s,`docker.m.daocloud.io` 当日返回 unavailable,
  `docker.1ms.run/library/eclipse-temurin:23-jdk` 可用,拉完 `docker tag` 成官方名即可命中本地缓存。

#### B 档验收清单(2026-09-04 复核通过)

```powershell
# 1. Go 五服务 + Java 网关 Ready;gate/scene(占位镜像)CrashLoop 属预期
kubectl get pods -n mmorpg-zone-yesterday
# 2. login / player-locator / scene-manager 已注册进 etcd(zone/1 前缀)
kubectl exec -n mmorpg-infra etcd-0 -- etcdctl get --prefix --keys-only LoginNodeService.rpc/zone/1
#    → LoginNodeService.rpc/zone/1/node_type/5/node_id/{2,3};另有 db.rpc/ login.rpc/ playerlocator.rpc/
#      dataservice.rpc/ scenemanagerservice.rpc/ 的 go-zero 发现键
# 3. MySQL 初始化 SQL 生效:zone_config 在 mmorpg 库,zone_1_db / zone_2_db / zone_101_db / zone_102_db 已建;
#    mmorpg_global(data-service 全局库,02_k8s_global_db.sql)已建且 appuser 有权(2026-09-08 起,存量 PVC 需手工补,见上节)
kubectl exec -n mmorpg-infra deploy/mysql -- sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -e "SELECT * FROM mmorpg.zone_config"'
# 4. Java 网关 zone 目录
kubectl port-forward -n mmorpg-zone-yesterday svc/gateway 18081:8081
curl http://127.0.0.1:18081/api/server-list
#    → {"zones":[{"zone_id":1,"name":"zone-1","status":"MAINTENANCE",...}]}
#      status 是 MAINTENANCE 而不是 OPEN:zone_config.manual_status=0 且没有任何 gate 注册
#      (本档 gate 是占位镜像),C 档 C++ 镜像上来后才会变 OPEN,不是网关的 bug。
```

- **login 起得比 player-locator 早会 fatal 重启几次**:`NewServiceContext` 用 `zrpc.MustNewClient` 同步拨
  `playerlocator.rpc`,etcd 里还没有就 `logx.Must` 直接退出。两副本各重启 3~13 次后随 player-locator Ready
  自愈,不是 bug;要消除只能把客户端改成 lazy dial 或给 login 加 initContainer 等依赖。
- **宿主重启后老 Pod 可能卡 `ErrImagePull`**:kind 节点重启时 kubelet 对已 `kind load` 的镜像仍会去 Docker Hub
  校验(`lookup registry-1.docker.io ... server misbehaving` / `EOF`),gateway 两副本卡了 40 分钟;
  `kubectl delete pod -l app=gateway` 让控制器重建就走 IfNotPresent 立刻起来(与 A 档一节同一个坑)。
- 网关日志里 `login.rpc channel up (zone=1): 127.0.0.1:53000` 三行是 `application.yaml` 里给本地 compose 的静态兜底,
  K8s 上真正生效的是随后 `login.rpc discovery routing changed: {1=2}` 那条 etcd 动态发现,可以忽略。

## data_service 按 C++ 约定注册 + IdSegments 号段配置(2026-09-08,node-id-overhaul-plan §6 / §7.5)

- **为什么**:scene 的 item / txlog / snapshot guid 改由 `DataService.AllocateIdSegment` 发号,scene 经 etcd 前缀
  `DataServiceNodeService.rpc` 发现 data_service;以前 data_service 只写 go-zero 自己的 `dataservice.rpc` 键,C++ 看不见,
  scene 永远过不了 DependencyGate("item segment ready")。
- **data_service 现在多写两把 key**(同一把租约、`LeaseTTL` 秒,keepalive 续租;租约丢失走 CAS 重夺 / 重新分配 node_id,
  `go/data_service/internal/noderegistry` 是 scene_manager 同名包的副本):
  - `DataServiceNodeService.rpc/zone/<ZoneId>/node_type/26/node_id/<N>` → protojson `NodeInfo`(Endpoint / GrpcEndpoint = `POD_IP:9000`)
  - `DataServiceNodeService.rpc/allocated/node_type/26/node_id/<N>` → node uuid(node_id 跨 zone 唯一占位)
  注册在 gRPC 端口真正 accept **之后**才发生(旁路拨端口);失败即 panic(CrashLoopBackOff 可见),关停时删 key + Revoke。
- **新配置键**(`go/data_service/etc/data_service.yaml` ↔ `go-svc-data-service-config`):`ZoneId`(写部署 zone,`${CurrentZoneId}`;
  DataServiceNodeService **不是** zone-scoped 类型,C++ 发现侧不按 zone 过滤,任何 zone 的 scene 都能发现任何 zone 的 data_service)、
  `LeaseTTL`(从服务 yaml 取,默认 60)。`manifests/go-svc/data-service.yaml` 加了 Downward API `POD_IP`(与 login.yaml 同)。
- **C++ 节点 ConfigMap**(`base_deploy_config.yaml`)新增:`service_discovery_prefixes` 里的 `DataServiceNodeService.rpc`,
  以及**顶层 `IdSegments:` 列表**。旧的 `ItemIdSegment:` 段不再生成。
  - **键名 / 结构以 C++ 读法为准,不能自己发明**:`cpp/libs/engine/config/config.cpp:126-140`(`readBaseDeployConfig`)
    只认顶层 `IdSegments` 列表,逐项显式读 `Kind` / `Enabled` / `InitialStep` / `MinStep` / `MaxStep` 五个键,
    填进 `IdSegmentConfig{repeated IdSegmentKindConfig kinds}`(`proto/common/base/config.proto:107`)。
    **写错形状不会报错**:缺键 = proto 默认值 0、整块名字不对 = `kinds` 为空,scene 的 Enable 校验拒绝 →
    DependencyGate "id segments ready" 永不放行,玩家进不来;而这份 ConfigMap 是 readOnly 整目录挂到 `/app/bin/etc`、
    **完全遮蔽**镜像里那份的,所以这里错就是线上错。2026-09-08 修正前生成器写的是一个 C++ 根本不读的
    `GuidSegment` 顶层映射(`Enabled` + `Kinds` 列表,键名 `Tag` / `Step`),没有任何一个键被读到。
  - **生成器不再抄常数**:`k8s_deploy.ps1::Get-AuthoritativeYamlBlock` 把 `bin/etc/base_deploy_config.yaml` 里整个
    `IdSegments:` 块(含每个 Kind 的取值说明注释)逐行原样搬运进 ConfigMap。单一真相在那份文件 ——
    改 step / 加种类(pet / guild …)只改那里,ConfigMap 不可能漂移;文件里没有 `IdSegments:` 则生成期直接 throw(fail-closed)。
  - 当前值(来自那份文件):`item` InitialStep 20000 / [1000, 1000000];`txlog` 50000 / [2000, 2000000];`snapshot` 2000 / [200, 100000]。
    `Kind` 名要与 data-service 的 `IdSegment.BootstrapTags` 及 `id_segment` 表的种子行一一对应(生产缺行 = `ErrCodeIdSegmentUnknownTag`,不自动补种)。
- **审计 topic 世代号 `_g<N>` 已改成配置驱动,ConfigMap 现在生成 `AuditTopicGeneration:`(2026-09-09)**:
  C++ 侧留在代码里的只有**基名**——`cpp/libs/modules/transaction_log/transaction_log_system.h` 的
  `kTransactionLogTopicBase = "transaction_log_topic"`、`cpp/libs/modules/snapshot/snapshot_system.h` 的
  `kPlayerSnapshotTopicBase = "player_snapshot_topic"`;后缀由 `cpp/libs/modules/audit/audit_topic.h::AuditTopicName`
  在发送时按 `BaseDeployConfig.audit_topic_generation`(`proto/common/base/config.proto` 字段 20)拼出来,
  `config.cpp::readBaseDeployConfig` 逐键读 `AuditTopicGeneration`,缺键 / 0 = 第一代 `_g1`
  (与 Go `topicForGeneration` 的 `generation == 0 → 1` 逐字同规则)。
  改这个值不再需要改 C++ 代码,也不需要重出 node 镜像 —— 改配置 + 重启节点即可。
  - **ConfigMap 的值从消费者那份 yaml 取**:`New-NodeConfigMapYaml` 用 `Get-YamlScalar` 读
    `go/data_service/etc/data_service.yaml` 的 `Kafka.TopicGeneration`(缺席回落 1,与 C++ / Go 的 0→1 规则一致),
    而不是从 `bin/etc/base_deploy_config.yaml` 再抄一份。同一个键还喂着 data-service 自己的 ConfigMap
    (`$dsKafkaTopicGeneration`)和 `kafka-topic-init` 预建的 topic 名 —— 生产者、消费者、预建 Job 三边同源,不会各说各话。
  - **漂移风险已消除,但两侧仍须同批次动**:K8s 上改 `Kafka.TopicGeneration` 后必须重跑 `zone-up`(重新生成 node ConfigMap)
    + 重跑 `infra-kafka-topics` 预建新一代 topic,并滚更 gate/scene 让它们重新读配置;
    本地 / 裸机跑的节点用的是 `bin/etc/base_deploy_config.yaml` 的 `AuditTopicGeneration`,那一份要与
    `go/data_service/etc/data_service.yaml` 的 `Kafka.TopicGeneration` 手工保持相等(两份 yaml 各自是各自部署形态的真源)。
    忘了任何一边的症状仍然是静默的:流水 / 快照进了一个没人消费的 topic,30 天后被保留期吃掉。
    `tools/scripts/tests/k8s_deploy_contract.tests.ps1` 里有一条契约用例钉住"node ConfigMap 的 `AuditTopicGeneration`
    == data-service 的 `Kafka.TopicGeneration`"。
- 验收:

```powershell
kubectl exec -n mmorpg-infra etcd-0 -- etcdctl get --prefix --keys-only DataServiceNodeService.rpc
#  → DataServiceNodeService.rpc/zone/<Z>/node_type/26/node_id/1 与 DataServiceNodeService.rpc/allocated/node_type/26/node_id/1
kubectl logs -n mmorpg-zone-<name> deploy/data-service | grep NodeRegistry
#  → [NodeRegistry] DataService registered: ... endpoint=<POD_IP>:9000 lease_ttl=60s
```

## Kafka:StatefulSet + PVC(2026-09-08,routing-identity-audit-20260908.md R06)

`manifests/infra/kafka.yaml` 现在是:ClusterIP Service `kafka`(仍是 `kafka.<InfraNamespace>:9092`,所有 ConfigMap /
`base_deploy_config.yaml` 都不用改)+ headless Service `kafka-headless` + `PodDisruptionBudget minAvailable: 1`
+ StatefulSet `kafka` **1 副本**、一块 `20Gi` PVC(集群默认 StorageClass),数据目录由 `KAFKA_LOG_DIRS=/var/lib/kafka/data` 显式指定。

- **为什么**:旧写法是单副本 Deployment + `emptyDir`,broker 一重启(漂移、OOM、升级)**全部 topic、分区元数据与已提交 offset
  一起消失**。消费者随后静默把 topic 自动重建成 1 分区(broker 开着 `auto.create.topics.enable` + `num.partitions=1`),
  停机窗口里产出的每一条控制面命令在 librdkafka 的 5 分钟消息超时后直接丢,`kafka-topic-init` 还得手工重跑。
- **顺带修掉的一个真 bug**:旧 manifest 把 `emptyDir` 挂在 `/tmp/kafka-logs`,而 apache/kafka 镜像 combined 模式的默认
  `log.dirs` 是 `/tmp/kraft-combined-logs` —— 那块卷根本不是数据目录,日志一直写在容器可写层上。现在 PVC 的 `mountPath`
  与 `KAFKA_LOG_DIRS` 是同一个路径,离线断言里有一条专门盯这个。
- **⚠️ 第一次切换会丢掉现有的全部 topic 数据**。PVC 是空的,broker 会重新 format 存储目录。`infra-up` 会先
  `kubectl delete deployment kafka --ignore-not-found` 再 apply StatefulSet(两者 kind 不同、名字相同,`apply` 不会回收旧
  Deployment;而 Service `kafka` 的 selector `app=kafka` 会同时命中新旧 Pod,客户端和 controller quorum 会在
  "旧 emptyDir broker" 与 "新 PVC broker" 之间随机落点 —— 两边各有一套 topic 和 offset,等于脑裂)。
  **切完必须重跑 `-Command infra-kafka-topics -WaitReady`**(同一次 `infra-up` 里已经跑了一遍;单独止血时用这条),
  否则审计 topic 与 `gate-cmd_g1` / `scene-cmd_g1` 会被抢先的生产者 auto-create 成 1 分区,gate/scene 的分区契约门禁随即让它们起不来。
  切换期间 Kafka 短暂不可用,这段时间产出的控制面命令会丢(与任何一次 broker 重启同性质),建议在没有玩家的窗口做。
- **刻意保持单 broker**。仓库里所有 topic 都是 `replication-factor 1`(`kafka-topic-init` / compose / `kafkautil.EnsureTopics`),
  `min.insync.replicas` 用默认值,而 `KAFKA_ADVERTISED_LISTENERS` 广播的是 ClusterIP Service 的 FQDN。
  **只把 `replicas` 改大 = 没有任何冗余,还会路由错乱**:多 broker 时每个 Pod 必须广播自己的
  `kafka-<n>.kafka-headless.<ns>` 地址,否则客户端拿到的 leader 地址会随机落到别的 broker 上。要上多 broker 是另一张票,
  最少要同时改这三处(副本数 / 广播地址 / 全部 topic 的 replication factor + `min.insync.replicas`),不是改一个数字。
- **PDB 是 `minAvailable: 1`,也就是不允许自愿驱逐**。整个控制面、`db_task` 落库、审计流水都只有这一个 broker,
  自动化的节点排空 / 集群升级顺手赶走它,停机窗口里的命令就没了。代价是 **`kubectl drain <node>` 会挂在这个 Pod 上**;
  真要腾空节点时用 `kubectl delete pod kafka-0 -n mmorpg-infra`(自愿删除不过 PDB,Pod 随即重建)或
  `kubectl drain --disable-eviction`。PVC 是 `ReadWriteOnce`:用 local-path 之类的本地盘时 Pod 只能回到原节点。
- **探针**:`startupProbe` / `readinessProbe` 是 API 级的 `kafka-broker-api-versions.sh`,`livenessProbe` 才是 TCP。
  原因是 KRaft 启动时 SocketServer 先 bind 9092,要到最后一步才 `enableRequestProcessing` —— **日志恢复期间 TCP 连得上、
  请求不受理**,用 TCP 做 readiness 会让 Service 把还没恢复完的 broker 放进 endpoint。
  两个 exec 探针都在命令里把 `KAFKA_HEAP_OPTS` 覆盖成 `-Xms16m -Xmx64m`:容器 env 里的 `-Xms512m -Xmx1g` 会被
  `/opt/kafka/bin/*.sh` 全部继承,和 broker 自己的堆叠在同一个 memory limit 里必 OOMKilled。**这是这个文件最容易踩的雷**。
  `startupProbe` 给到 5 分钟(30 × 10s),非正常关闭后的日志恢复不会被 liveness 误杀。
- **文件描述符**:K8s 没有声明 ulimit 的字段,容器运行时的 nofile 软限因运行时/版本而异(Docker Desktop 给 1048576,
  不少 containerd 配置只给 1024)。光控制面命令 topic 就有 2×256 个分区,每分区至少 3 个打开文件,再加审计 / `db_task` /
  迁移窗口里残留的 per-node topic,以及每个客户端连接一个 fd —— 1024 是必然 `Too many open files`,而症状是分区离线、
  客户端连不上,不是一条清楚的报错。所以容器的 `command`/`args` 是一层薄壳:先 `ulimit -n <硬限>`(软限升到硬限不需要特权),
  再 `exec /__cacert_entrypoint.sh /etc/kafka/docker/run`。**换镜像版本时要核一遍入口路径**:
  `docker image inspect apache/kafka:latest --format '{{json .Config.Entrypoint}} {{json .Config.Cmd}}'`;路径不对会立刻
  CrashLoop 并打出 `not found`,不会静默降级。
- **`CLUSTER_ID` 现在显式钉住**。镜像的 launch 脚本读的是 `CLUSTER_ID`(不是 manifest 里一直写着的 `KAFKA_CLUSTER_ID`),
  它自带的默认值恰好同值,所以以前"碰巧"是对的。换 PVC 之后这件事变要命:`meta.properties` 里记着 format 时用的 cluster id,
  与启动时的值不一致 broker 会拒绝启动。
- **资源**:`requests` 300m / 1Gi,`limits` 2 CPU / **2Gi**(原 1536Mi)。换 PVC 之后 page cache 才真正起作用,
  加上探针 JVM 会短暂再占几十 MB,1536Mi 配 `-Xmx1g` 的非堆余量太薄。堆本身仍由 `-KafkaProfile` 决定(`$KafkaHeapOpts`)。
- **PVC 生命周期**:删 StatefulSet 不删 PVC,删 namespace(`infra-down`)才删。`infra-down` → `infra-up` 之后是一套空 Kafka,
  按上面第三条重跑 `infra-kafka-topics`。
- **kind / minikube**:PVC 要有默认 StorageClass(kind 自带 local-path)。`infra-status` 会列 `sts` / `pvc` / `pdb`;
  kafka 起不来先看 PVC 是否 Pending。`-WaitReady` 下 `infra-up` 会在 etcd quorum 之后、`kafka-topic-init` 之前等
  `sts/kafka` Ready。

### 未改、但值得知道的两件事

- `image: apache/kafka:latest` 是浮动 tag,且没有设 `imagePullPolicy`(`:latest` 默认 `Always`)。PVC 化之后,
  一次重启拉到新版本 broker 去读旧版本格式的日志目录,是一种新的版本漂移风险。本轮没动它(与 R06/R11 无关),
  但生产上应该钉一个具体版本。
- 堆仍是 `-Xms512m -Xmx1g`(`prod-like`)。2×256 个命令分区 + 审计 + 各 zone 的 `db_task` 在 1G 堆上偏紧,
  真上量前应该按分区总数复核一次。

## Kafka 保留期:一条规则(2026-09-08,routing-identity-audit-20260908.md R11)

> **任何"消费者可能落后"的 topic,保留期必须大于消费者可能落后的最长时间,并留足余量;而且必须显式声明,不许继承 broker 默认值。**

落后多久算封顶,取决于消费者:

- C++ 的 gate / scene 消费者把 `max.poll.interval.ms` 钉在 **900000**(15 分钟,
  `cpp/libs/engine/infra/messaging/kafka/kafka_consumer.cpp`)。也就是说 broker 眼里一个消费者"合法地"消失 15 分钟仍然算活着,
  回来时必须还能读到那 15 分钟的消息。保留期低于 900s 时它读不到的不是"旧数据",是 BindSession / RoutePlayer / KickPlayer,
  而且 **Kafka 一个错都不报** —— 表现是玩家卡在登录或进世界。
- `db_task` 那条链的封顶不是 poll 间隔而是 **MySQL 故障时长**:消费者停多久,积压就要留多久,否则丢的是玩家存档。

### 现状表(2026-09-08 盘点)

| topic | 谁建 | 保留期 | 消费者 | 判定 |
|---|---|---|---|---|
| `gate-cmd_g<N>` / `scene-cmd_g<N>` | `kafka-topic-init`(Job + compose) | **显式 1h** | C++ gate / scene(assign) | ✅ 4 × 900s |
| `transaction_log_topic_g<N>` / `player_snapshot_topic_g<N>` | 同上 + data-service `EnsureTopics` | **显式 30d** | data-service | ✅ |
| `db_task_zone_<N>` | login / db 启动时 `EnsureTopics` | **显式**,见下 | go/db | ✅(本轮修复) |
| `match-results` | match `StartResultConsumer` | 显式 7d(match 侧常量) | match | ✅ |
| `gate-<id>` / `scene-<id>`(迁移窗口残留) | broker auto-create | broker 默认值 | C++ legacy group 订阅 | ⚠️ 只能靠 broker 默认值兜底,本轮把它抬到 900s 以上 |
| `game-events` | compose(K8s 侧从不建) | 本轮改为显式 1h | **无**(`Kafka.GroupID` 为空,`KafkaManager::Init` 跳过顶层 subscribe) | ✅ |
| `__mmorpg_partition_contract_*` | `kafkautil.EnsureTopics` | `cleanup.policy=compact`,只是个标记 | 无 | ✅ 不受保留期影响 |

### 本轮改了什么

- **broker 默认保留期**(`$KafkaBrokerRetentionMs` → `KAFKA_LOG_RETENTION_MS`):`dev` 60s → **30 分钟**,
  `prod-like` 300s → **1 小时**,`custom/default` 300s → **1 小时**。它是所有"没有自己 `retention.ms`"的 topic 的保留期,
  也就是迁移窗口里 auto-create 出来的 `gate-<id>` / `scene-<id>` —— **R11 描述的正是这一条**。
  `deploy/env/kafka.dev.env` / `kafka.prod-like.env` 与 `deploy/docker-compose.yml` 的内联默认值同步改。
- **`db_task_zone_<N>`**(`$KafkaDbTaskRetentionMs` → login ConfigMap 的 `Kafka.RetentionMs`):
  `dev` 300s → **1 小时**,`prod-like` 600s → **6 小时**,`custom/default` 900s → **24 小时**。
  除了"低于 900s"之外,这里还有一个更难发现的问题:**login 与 db 都会在启动时对同一个 `db_task_zone_<N>` 执行
  `IncrementalAlterConfigs`**(`kafkautil.EnsureTopics`),而 db 的 K8s ConfigMap 根本不写 `RetentionMs`、走 Go 结构体
  默认值 86400000(24h),login 的 ConfigMap 写的是脚本注入值。于是这个 topic 的保留期会**随两个服务的重启顺序在
  15 分钟和 24 小时之间反复横跳**。把 `custom/default` 对齐到 86400000 之后不再漂;真要改保留期,
  两处(`Apply-KafkaProfileDefaults` 与 `go/db/etc/db.yaml`)必须同拍。
- **保留期改为"声明"而非"创建时附带"**:`kafka-topics.sh --create --config retention.ms=...` 只在新建那一刻生效。
  一个已经存在的 topic(被生产者抢先 auto-create、或上一代 Job 用别的值建的)会静默继承 broker 默认值。
  所以 K8s Job 与 compose 的 init 现在都在建完之后再 `kafka-configs.sh --alter --add-config retention.ms=...`
  并 `--describe` 读回核对,不一致就退出 1。与分区数不同,`retention.ms` 是可变配置,重设没有寻址风险。

### 加新 topic 时

先回答一句话:**这个 topic 的消费者最长可能落后多久?** 然后把保留期设成它的 2 倍以上,并在建 topic 的地方
显式写 `retention.ms`(`kafkautil.TopicSpec.RetentionMs` 或 init 脚本的 `ensure_topic` / `ensure_retention`)。
不要留给 broker 默认值 —— 那个值是给"没有契约的 topic"用的下限,不是给你的 topic 用的。

### 离线校验(本轮改动全部不碰集群)

```powershell
# 1) 语法:0 errors
pwsh -NoProfile -Command "$e=$null;$t=$null;[System.Management.Automation.Language.Parser]::ParseFile('tools/scripts/k8s_deploy.ps1',[ref]$t,[ref]$e)|Out-Null; $e.Count"
# 2) 渲染(bogus context,永不连集群)
pwsh -File tools/scripts/k8s_deploy.ps1 -Command infra-up -DryRun -KubeContext no-such-context-dryrun-only
pwsh -File tools/scripts/k8s_deploy.ps1 -Command zone-up -ZoneName zone-1 -ZoneId 1 -GoSvcRegistry local.test -DryRun -KubeContext no-such-context-dryrun-only
# 3) compose
docker compose -f deploy/docker-compose.yml config
```

再用 PyYAML 断言渲染结果:StatefulSet 有 `volumeClaimTemplates` / 三个探针 / PDB、Service `kafka` 名字与
9092+9093 端口没变、PVC 的 `mountPath` 等于 `KAFKA_LOG_DIRS`、两条 topic-init 路径都对四个 topic 显式设了保留期、
渲染后的 broker 默认保留期 > 900000。manifest 里嵌的 shell 可以用
`yaml.safe_load` 抽出来跑 `sh -n`(Job 的 `command[2]`、kafka 的 `args[0]`、compose 的 `entrypoint[2]`)。

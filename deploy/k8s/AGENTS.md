# K8S KNOWLEDGE BASE

## OVERVIEW
`deploy/k8s/` is the Kubernetes-only release path for game zones. It is separate from local Docker Compose and assumes Linux runtime artifacts.

## STRUCTURE
```text
deploy/k8s/
├── manifests/         # Infra and workload manifests
├── runtime/           # Staged runtime files for Linux image
├── Dockerfile.runtime # Production K8s runtime image
├── zones.*            # One-click zone config examples / ops presets
└── README.md          # Authoritative flow description
```

## WHERE TO LOOK
| Task | Location | Notes |
|------|----------|-------|
| End-to-end flow | `README.md` | One-zone/all-zone operations |
| Runtime staging layout | `runtime/README.md` | Required Linux file layout |
| Runtime image | `Dockerfile.runtime` | Use this, not root Dockerfile |
| Infra manifests | `manifests/infra/` | Shared infra (etcd/redis/kafka/mysql) deployed to `mmorpg-infra` namespace |
| Script entrypoint | `tools/scripts/dev_tools.ps1` | `k8s-*` commands drive this subtree |

## CONVENTIONS
- This subtree is Kubernetes-only; do not mix docker-compose/local process assumptions into it.
- Production image must use `deploy/k8s/Dockerfile.runtime`.
- Runtime expects Linux binaries staged under `deploy/k8s/runtime/linux/`.
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
- `zones.ops-recommended.yaml` is the best starting point for multi-zone ops.
- `-SkipInfra`, `-DryRun`, and `-WaitReady` are the high-signal operational flags.
- `k8s-all-up` deploys infra first, then all zones. Use `-SkipInfra` to skip infra.
- Current manifests assume `/app/bin` runtime layout and fixed role ports (`gate` 18000, `scene` 19000).

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

## KNOWN BREAKAGE — Dockerfile.cpp 构建不出来(未修,需人工决策)
- `Dockerfile.cpp` 第 100 行 `COPY tools/scripts/build_linux.sh` 与第 106 行 `RUN bash tools/scripts/build_linux.sh ...` 引用的脚本**在仓库里不存在**:工作区没有、`git ls-files` 没有、`git log -- tools/scripts/build_linux.sh` 无任何提交记录。因此 `k8s-build-all` 的 C++ 镜像这一段必然在 stage 2 失败。
- 同文件第 38 行克隆 gRPC `--branch v1.78.x`,而 `.gitmodules` 的 `third_party/grpc` 钉的是 `branch = v1.80.x` —— 两条构建路径用的不是同一个 gRPC 大版本。
- 现有规范无法唯一决定该补哪个版本的 `build_linux.sh`、也无法唯一决定 gRPC 该对齐到 1.78 还是 1.80,**不要凭猜测补文件**。
- 当前可用的 K8s 镜像路径仍然是 `Dockerfile.runtime` + `k8s-stage-runtime`(预先在 Linux 上构建好二进制再 staging),见上文 COMMANDS。

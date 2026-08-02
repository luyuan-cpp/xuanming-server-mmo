# K8s One-Click Open-Server

This directory contains Kubernetes-only deployment assets for opening game zones.

## Scope

- `single zone`: open one zone (for example, `yesterday` zone).
- `all zones`: open all zones defined in a JSON or YAML config file.
- This flow is Kubernetes-only and does not include Docker Compose or local process startup scripts.

## Ops Runbook

- Day-2 operations and release/rollback steps: `docs/ops/k8s-open-server-runbook.md`.

## Directory Layout

- `manifests/infra/`: infra resources applied per namespace (`etcd`, `redis`, `kafka`, `mysql`).
- `manifests/go-svc/`: Go micro-service K8s manifests (`db`, `data-service`, `login`, `player-locator`, `scene-manager`).
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
- Node pods use:
  - `POD_IP` from Kubernetes Downward API.
  - `RPC_PORT`/`NODE_PORT` env vars (fixed per role: centre `17000`, gate `18000`, scene `19000`).
  - `GRPC_SERVER_MAX_POLLERS` 用于限制 gRPC server poller 数（默认 `8`，与 C++ 进程默认值一致）。传 `-GrpcServerMaxPollers 0` 时，Deployment 与 Fleet 都不写该环境变量，交给进程默认值。
- A `gate-entry` Service is created per zone namespace for external TCP access.

## Optional Flags

- `-SkipInfra`: deploy only node workloads (skip `etcd`/`redis`/`kafka`).
- `-DryRun`: print kubectl commands without applying.
- `-WaitReady`: wait for `centre` / `gate` / `scene` deployments to roll out.
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

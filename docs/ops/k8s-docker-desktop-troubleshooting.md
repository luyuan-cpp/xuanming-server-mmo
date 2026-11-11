# K8s Docker Desktop Troubleshooting Guide

Lessons learned from deploying 10 zones (170 pods) on Docker Desktop K8s (2026-03-28).

## Loading Images into containerd

Docker Desktop K8s uses containerd. `docker build` puts images in Docker daemon only — K8s can't see them.

```powershell
# Load a single image
docker save ghcr.io/luyuancpp/mmorpg-node:v1 | docker exec -i desktop-control-plane ctr -n k8s.io images import --all-platforms -

# Load all Go/Java images
@("mmorpg-auth","mmorpg-data-service","mmorpg-db","mmorpg-login","mmorpg-player-locator","mmorpg-scene-manager") | ForEach-Object {
    docker save "ghcr.io/luyuancpp/${_}:latest" | docker exec -i desktop-control-plane ctr -n k8s.io images import --all-platforms -
}
```

If manifests use `:latest` but images were built as `:v1`, tag first:
```powershell
docker tag ghcr.io/luyuancpp/mmorpg-login:v1 ghcr.io/luyuancpp/mmorpg-login:latest
```

Verify images in containerd:
```powershell
docker exec desktop-control-plane crictl images | Select-String "mmorpg"
```

## maxPods Limit

Default `maxPods` is 110. With 10 zones × 17 pods = 170, pods get stuck in `Pending` ("Too many pods").

```powershell
# Raise to 250
docker exec desktop-control-plane sh -c 'echo "maxPods: 250" >> /var/lib/kubelet/config.yaml'
docker exec desktop-control-plane sh -c 'kill -HUP $(pidof kubelet)'

# Verify
kubectl get node desktop-control-plane -o jsonpath='{.status.capacity.pods}'  # should show 250
```

## Viewing Deployed Zones

```powershell
# All zone namespaces
kubectl get ns | Select-String mmorpg-zone

# Quick health summary
1..10 | ForEach-Object {
    $ns = "mmorpg-zone-zone-$_"
    $pods = kubectl get pods -n $ns --no-headers 2>$null
    $total = ($pods | Measure-Object).Count
    $running = ($pods | Select-String "Running" | Measure-Object).Count
    Write-Host "zone-$_`: $running/$total Running"
}

# Single zone
kubectl get pods -n mmorpg-zone-zone-1
kubectl get svc  -n mmorpg-zone-zone-1
kubectl logs <pod> -n mmorpg-zone-zone-1
kubectl describe pod <pod> -n mmorpg-zone-zone-1

# Find non-running pods
kubectl get pods -n mmorpg-zone-zone-1 --no-headers | Where-Object { $_ -notmatch "Running" }
```

## Troubleshooting Record

### 1. C++ node segfault — array out of bounds
- **Symptom**: gate/scene pods CrashLoopBackOff, segfault in `InitMessageInfo()`
- **Root cause**: `kMaxRpcMethodCount = 102` but message IDs go up to 117. Writing beyond array bounds corrupts adjacent BSS memory (gRPC `Status::OK` guard variable).
- **Fix**: `kMaxRpcMethodCount = 118` in `rpc_event_registry.h`; use the constant (not literal) in `.cpp` array definition.
- **Diagnosis**: `nm` to get symbol addresses, calculate array span, confirm out-of-bounds write overlaps `Status::OK`.

### 2. C++ Redis hostname resolution
- **Symptom**: Crash at `sockets::fromIpPort` — `inet_pton("redis")` returns 0.
- **Root cause**: muduo `InetAddress(ip, port)` expects numeric IP. K8s service name `"redis"` is a DNS name.
- **Fix**: Use `InetAddress::resolve()` before constructing address in `node.cpp`. Note: etcd (gRPC) and Kafka (librdkafka) handle DNS natively — only Redis/muduo was affected.

### 3. Table filename case sensitivity (Windows → Linux)
- **Symptom**: `File2String("actoractioncombatstate.json")` fails — file on disk is `ActorActionCombatState.json`.
- **Root cause**: C++ code lowercases filenames; works on case-insensitive Windows, breaks on Linux.
- **Fix**: Dockerfile RUN step to lowercase all `.json` table filenames after COPY.

### 4. Go/Java ImagePullBackOff
- **Symptom**: Go/Java pods in ImagePullBackOff, C++ nodes Running.
- **Root cause**: Images in containerd tagged `:v1`, manifests reference `:latest`.
- **Fix**: `docker tag :v1 :latest` then `docker save | ctr images import`.

### 5. db CrashLoopBackOff (startup ordering)
- **Symptom**: db pods Error/CrashLoopBackOff, MySQL is Running.
- **Root cause**: db pods started before MySQL was ready (all pods were Pending initially due to maxPods). By the time MySQL came up, db had cached connection failure.
- **Fix**: `kubectl rollout restart statefulset/db -n <zone-ns>`.

### 6. Go protobuf registration conflict
- **Symptom**: Go services panic on startup with protobuf registration duplicate.
- **Fix**: `GOLANG_PROTOBUF_REGISTRATION_CONFLICT=warn` env var on all 5 Go services.

### 7. Kafka Brokers config type
- **Symptom**: Go services fail to parse Kafka config.
- **Root cause**: `k8s_deploy.ps1` rendered Brokers as comma-separated string; Go expects `[]string` (YAML list).
- **Fix**: Template renders YAML list format for Brokers.

## Key Takeaways

- Always load images into containerd after `docker build` on Docker Desktop K8s.
- Raise `maxPods` before deploying many zones on a single node.
- For startup ordering issues, `kubectl rollout restart` is the fast fix.
- muduo's `InetAddress` needs `resolve()` for DNS hostnames — unlike gRPC and librdkafka which handle DNS natively.
- Windows→Linux file paths: watch for case sensitivity in generated filenames.

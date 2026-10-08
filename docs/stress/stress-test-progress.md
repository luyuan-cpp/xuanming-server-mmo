# 1000-Robot Stress Test Progress

## Test Environment
- Docker Desktop K8s, single node (`desktop-control-plane`)
- 10 zones × 100 robots = 1000 concurrent logins
- Each zone: 2 login pods, 1 player-locator, 1 scene-manager, 2 gate + 2 scene (C++)

## Results Summary

| Round | Result | Fix | Commit |
|-------|--------|-----|--------|
| Baseline | 883/1000 | — | — |
| Etcd lease fix | 915/1000 | Lease TTL tuning | — |
| Lock resilience | 993/1000 | 12× lock retry + bg ctx release | `93cd560c0` |
| Cross-session regression | 0/1000 | PlayerLocator circuit breaker locked | (pod restart) |
| After restart | 945/1000 | login + player-locator restarted | — |
| Zone-scoped Redis key | **989/1000** | `scene_nodes:zone:{N}:load` | `65868a103` |

## Bug Details

### 1. SceneManager Redis Key Collision (945→989)
- **Root cause**: All 10 zone SceneManagers shared infra Redis with single key `scene_nodes:load`. Each zone's `syncSceneNodes()` cleanup removed other zones' entries.
- **Fix**: Zone-scoped key `scene_nodes:zone:{zoneID}:load` in `go/scene_manager/internal/logic/load_reporter.go`.

### 2. Remaining 11/1000 Failures
- All `DeadlineExceeded` on `SceneManager.EnterScene` RPC
- SceneManager EnterScene calls average ~1000ms on this overloaded single-node cluster
- Tail latencies >2s hit the go-zero RPC server timeout (default 2000ms, config says 100000ms — investigating effective value)
- Expected in resource-constrained Docker Desktop environment

## Operational Notes
- After PlayerLocator/SceneManager pod restarts, **login pods MUST also restart** (go-zero circuit breaker + gRPC resolver cache)
- Docker Desktop doesn't auto-sync `:latest` images to containerd; use `docker save | ctr -n k8s.io images import`
- Robot uses `-c /etc/robot/robot.yaml` (ConfigMap mount), not `-login` flag

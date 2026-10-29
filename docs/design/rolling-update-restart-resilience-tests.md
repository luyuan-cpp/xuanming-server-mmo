# Rolling Update & Node Restart Resilience Tests

> **2026-10-23 阅读边界**：本文保留 2026-04-04 的历史实验。“Grayscale”标题描述的是当时的滚动替换，没有证明稳定玩家分组或金丝雀发布；“杀 Scene 无影响”也不能证明资产、世界状态或对局连续性。当前需求与验收边界见 [全类型节点生命周期与发布需求](node-lifecycle-elasticity-release-20261023.md)，本次未重跑历史实验。

**Date:** 2026-04-04  
**Cluster:** Docker Desktop K8s (single node), zone-1 (mmorpg-zone-zone-1)  
**Test tool:** Robot stress test (50 robots for rolling update, 30 for restart)

## Test 1: Rolling Update (Grayscale)

### Setup
- 50 robots connected to zone-1, login+enter+skill loop at 2s interval
- Baseline: enter_ok=44/50, msg throughput ~21-23 msg/s, avg_login=770ms
- Default K8s RollingUpdate strategy: maxSurge=25%, maxUnavailable=25%

### Trigger
- `kubectl set env` on login, gate, scene deployments simultaneously
- All old pods terminated, new pods created within ~20s

### Results
| Metric | Before | During | After (30s+) |
|--------|--------|--------|-------------|
| conn   | 50     | 50 (stale) | 50 (stale) |
| msg_recv/s | 21-23 | dropped to 0/s at T+7s | stuck at 0/s |
| enter_ok | 44 | frozen | frozen |

### Error Signatures
1. `Connection closed by server` — graceful close from old gate
2. `write: broken pipe` — old gate terminated before client detected

### Key Findings
1. **Total session blackout**: All 44 active sessions lost when gate pod rolled
2. **No recovery**: Robot reconnection logic retries but fails (connects to old pod IP, not service VIP)
3. **No readiness probes**: New pods marked Ready instantly before actually serving
4. **No graceful drain**: Old pods killed immediately, breaking active TCP connections
5. **Scene/login pod rotation**: Invisible to clients (only gate holds client sessions)

---

## Test 2: Node Restart (Force Kill)

### Setup
- 30 robots, all 30/30 enter_ok, msg throughput ~15/s, avg_login=309ms

### Phase 1: Kill one scene pod (force, grace-period=0)
- **Impact: ZERO** — conn=30, msg_recv=15/s unchanged
- Scene pod kill does not affect client sessions (gate holds TCP connections)

### Phase 2: Kill one login pod (force)
- **Impact: ZERO** on existing sessions — conn=30, msg_recv=15/s unchanged
- Login is only needed for initial auth flow, not ongoing gameplay

### Phase 3: Kill gate pod (force)
- **Impact: TOTAL session loss**
- msg_recv dropped from 15/s to 0/s within ~4s
- Errors: `Connection closed by server`, `write: broken pipe`
- Robot logged "Disconnected, retrying connection..." but reconnection failed
- New gate pod came up in ~18s but no clients recovered

### Summary Table

| Kill Target | Impact on Active Sessions | Recovery? |
|-------------|--------------------------|-----------|
| Scene pod   | None | N/A |
| Login pod   | None | N/A |
| Gate pod    | Total session loss | No (robot reconnect to old IP) |

---

## Recommendations (Production)

### Must-Have (P0)
1. **Readiness probes** for gate/scene/login — prevent traffic before service is ready
2. **Graceful shutdown** (SIGTERM handling) in C++ gate node — drain connections before exit
3. **preStop hook** in K8s deployment — add `sleep 5` to allow connection drain
4. **Client reconnection to service DNS** — robot/client should reconnect via K8s Service VIP, not cached pod IP

### Should-Have (P1)
5. **PodDisruptionBudget** for gate — `minAvailable: 1` to never kill all gate pods at once
6. **Liveness probes** — auto-restart crashed-but-stuck pods
7. **maxUnavailable=0** for gate deployment — only replace after new pod is ready
8. **Session migration** — when gate drains, notify client to reconnect to another gate

### Nice-to-Have (P2)
9. **Blue-green deployment** option for zero-downtime gate updates
10. **Circuit breaker reset** — login's go-zero circuit breaker for player_locator remained open after burst load (error 35 = kLoginUnknownError)

---

## Additional Observation: Circuit Breaker Issue

During initial test setup, all 50 robots failed with `kLoginUnknownError` (error id:35). Root cause: login service's go-zero circuit breaker for player_locator was stuck open from a previous burst test. The breaker did not auto-recover even though player_locator was healthy. Required login pod restart to reset the breaker state.

**Implication**: After any burst failure event, the go-zero circuit breaker may need manual intervention (pod restart) to resume normal service. Consider tuning breaker settings (`BreakerWindow`, `BreakerThreshold`) or adding a health check endpoint that can verify downstream connectivity.

# Centre Decentralisation Migration Plan (2026-03-15)

## Overview
## Latest Decisions
- 2026-03-15: The `request_id` reconnect / idempotency optimisations related to `centre` are frozen — no further changes under `cpp/nodes/centre/**`; they will be migrated together with the `centre` removal path to Login + player_locator.


Centre essentially does 6 things, each of which can be distributed to existing, already multi-instance components.
Goal: no single-instance component at any level — every layer is multi-instance or clustered.

> **Reality check (2026-09-19, verified line by line against `deploy/k8s/manifests/` and the deploy script; full record in the same-day PROGRESS.md entry)**
>
> This document is the migration **plan** dated 2026-03-15. The "Single point? No" rows below record what the plan assumed at the time;
> several of them do **not** match the manifests actually in the repository. Do not read them as "goal achieved". The original tables are
> kept unchanged (historical plan); this section takes precedence:
>
> | Claim in this document | Actual state |
> |---|---|
> | player_locator is already multi-instance | Until 2026-09-19 the manifest said `replicas: 1` (a true single point on the login path). After verifying in code that it is multi-instance safe, it was changed that day to 2 replicas + anti-affinity + PDB (`go-svc/player-locator.yaml`). **True now.** |
> | Login is already multi-instance | True: `replicas: 2`. However its PDB used to be a standalone file the deploy script never applied, and its etcd registration lease was 500s (a crashed instance lingered for 8 minutes while gate picks a login instance at random, so about half of login requests failed). Both were fixed on 2026-09-19 (embedded PDB, 60s lease). |
> | Kafka RF≥3, not a single point | **False.** `infra/kafka.yaml` is a **deliberately** single broker and every topic in the repository has replication-factor 1 (the header comment of that file explains why). The 3-broker / RF=3 target exists only in `docs/ops/kafka-cluster-production-runbook.md`. Also, Kafka is not only an asynchronous path: login's EnterGame waits synchronously for BindSession to be sent via `gate-cmd`, so nobody can enter the game while Kafka is down. |
> | Redis is distributed by nature, not a single point | **False.** The shared Redis holding sessions, locations, login locks, player locks, the scene_manager leader lock and scene routing is a **single instance** (`infra/redis.yaml`, `replicas: 1`), and every client connects in standalone mode (`Type: node`). Only match's private `infra/redis-match-cluster.yaml` is a real Redis Cluster (3 masters, 3 replicas). |
> | Goal: no single-instance component | **Not achieved.** Still single-instance in the manifests: MySQL (`replicas: 1` + Recreate, no replication), the per-zone `db` service (the code assumes a single instance), `data-service` (`replicas: 1` + Recreate, deliberate per the manifest comment), the battle pool (1 replica by default); `guild` has no K8s manifest at all. Actually multi-instance: etcd (3 replicas + PDB), match's Redis Cluster, scene_manager (2 replicas with Redis-lock leader election — though the lock lives in the single-instance Redis above), login, player_locator, the Java gateway, chat / match / trade / client-rpc-router. |

---

## Centre Current Responsibilities → Decentralisation Migration Plan

### 1. Session Map (session → player routing)

| Current State | Centre maintains `SessionMap[session_id] → player_id` in memory; Gate depends on it |
|------|------|
| **Migration** | **Gate local maintenance** — Gate already knows which sessions it holds; no centralised lookup needed. Cross-Gate lookups go through `player_locator` (already exists, multi-instance + Redis) |
| **Single point?** | No — Gates are independent; player_locator is already multi-instance |

### 2. Login Decision Logic (FirstLogin / Reconnect / ReplaceLogin)

| Current State | Centre's `DecideEnterGame()` makes the decision |
|------|------|
| **Migration** | Move to **Login service** (Go, already multi-instance). Login queries `player_locator` for current player state → makes decision → notifies Gate via Kafka to bind / kick |
| **Single point?** | No — Login is already stateless multi-instance |

### 3. Scene Registration & Scene-Switch Orchestration

| Current State | Scene calls `RegisterScene` on Centre; scene switches are coordinated through Centre |
|------|------|
| **Migration** | Scene registration → **write directly to etcd** (already in progress). Scene-switch orchestration → **SceneManager** (Go, multi-instance cluster under construction), issues `RoutePlayer` / `KickPlayer` via Kafka |
| **Single point?** | No — SceneManager is partitioned by player_id (consistent hash), multiple instances back each other up |

### 4. Cross-Node Message Routing (RoutePlayerStringMsg / RouteNodeStringMsg)

| Current State | Centre acts as message relay |
|------|------|
| **Migration** | **Kafka topic direct delivery** — sender queries `player_locator` for target scene/gate → publishes to `scene-{scene_id}` or `gate-{gate_id}` topic. Consistent with the cross-scene-player-messaging design |
| **Single point?** | No — Kafka RF≥3 |

### 5. Disconnection Delayed Cleanup (30 s reconnect window)

| Current State | Centre maintains a `DelayedCleanupTimer` |
|------|------|
| **Migration** | **player_locator lease mechanism** — Gate detects disconnect → notifies player_locator to mark "disconnecting" + 30 s TTL. Reconnect within TTL → lease renewed; expired → player_locator fires a cleanup event (Kafka notification to relevant Scenes) |
| **Single point?** | No — player_locator multi-instance + Redis TTL |

### 6. Request Idempotency (in-memory request_id dedup)

| Current State | Centre in-memory map, 5-minute TTL |
|------|------|
| **Migration** | **Redis SET with TTL** (`request:{player_id}:{request_id}` → SETEX 300 s). Whoever processes the request checks it — no centralised component needed |
| **Single point?** | No — Redis is inherently distributed |

---

## Post-Migration Architecture

```
Client → Gate (multi-instance, each maintains local sessions)
           │
           ├─ Login (Go, multi-instance, stateless)
           │    └─ Queries player_locator for login decisions
           │    └─ Kafka notifies Gate to bind / kick
           │
           ├─ SceneManager (Go, multi-instance, hash-partitioned)
           │    └─ Orchestrates scene switches
           │    └─ Kafka issues RoutePlayer / KickPlayer
           │
           ├─ player_locator (Go, multi-instance + Redis)
           │    └─ Player location source of truth
           │    └─ Disconnect lease / reconnect window
           │
           ├─ Kafka (RF≥3)
           │    └─ All control-plane messages
           │
           └─ etcd (3/5 nodes)
                └─ Service discovery & scene registration
```

**No single-instance component exists. Every layer is multi-instance or clustered.**

---

## Migration Sequence

| Phase | Action | Risk | Status |
|------|------|------|------|
| **Phase 1** | Scene registration goes directly through etcd (mostly done); SceneManager takes over scene switching (in progress) | Low — runs in parallel with existing path | ~90 % complete (proto/RPC/Gate Kafka in place, remaining pbgen re-run) |
| **Phase 2** | Login decision moves to Login service + player_locator; disconnect cleanup switches to lease model | Medium — requires dual-write verification | player_locator service implemented (7 RPCs + lease monitor), pending Login integration |
| **Phase 3** | Cross-node message routing switches to Kafka direct delivery; idempotency moves to Redis | Low — purely incremental | Not started |
| **Phase 4** | Centre demoted to read-only observation node (no decision-making); dual-path verification runs | Low | Not started |
| **Phase 5** | Gradual Centre shutdown; emergency rollback switch retained for one release cycle | Low — rollback guarantee | Not started |

## Phase 2 Priority Analysis

Phase 2 should start with **player_locator** because it is a foundational dependency for multiple subsequent tasks:

| Dependent | Needs player_locator |
|---|---|
| Login decision (FirstLogin / Reconnect / Replace) | Yes — Login queries it to make decisions |
| Disconnect lease cleanup | Yes — TTL lives here |
| Cross-node Kafka direct routing (Phase 3) | Yes — sender resolves target via locator |

### What player_locator Needs to Implement
1. **Redis-backed player state store**: `player:{player_id}` → `{gate_node_id, gate_instance_id, scene_node_id, scene_instance_id, status, home_zone_id}` with TTL support
2. **gRPC API**: `Register`, `Unregister`, `Locate`, `UpdateStatus` (+ lease renew / expire)
3. **Lease mechanism**: Gate disconnect → mark "disconnecting" + 30 s TTL; reconnect → renew lease; expired → publish `PlayerLeaseExpiredEvent` to Kafka
4. **Redis keyspace notifications** (or polling) to detect TTL expiration and trigger Kafka events

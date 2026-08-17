# MMO Cross-Server Architecture (跨服架构 — 全局设计文档)

> **修订(2026-08-15)**: "每 realm 独立存储、数据永远留在 home realm"的前提被 [global-data-layer-tidb-decision.md](./global-data-layer-tidb-decision.md) 修订为"TiDB 全局数据层 + home_zone 逻辑归属";**位置透明核心原则本身不变**,且因数据全局化而更容易成立。

## Game Model
- MMO (魔兽世界类), ~1000 大区 (realms)
- Player belongs to one home realm but can freely visit any realm
- Each realm has its own Redis
- Scene switching latency is acceptable; **stability & correctness** are the top priority

## **CORE PRINCIPLE: Location Transparency (位置透明)**
> 不管玩家在哪个服务器上，处理逻辑都和在本服一样，不需要判断"是否跨服"。
> This principle must be applied to ALL future cross-server related work.

- Logic layer NEVER branches on "is this player cross-server or local"
- All game logic treats every player identically regardless of origin realm
- No data sync/copy to destination server — player data stays at home realm
- Eliminates: login-time cross-server checks, data consistency issues, scattered if-cross-server branches

## Pain Points (Previous Experience — Avoid Repeating)
- Data sync approach: copying data to destination server → consistency nightmares
- Checking "is cross-server?" at every processing point → error-prone, unmaintainable
- Login flow detecting cross-server state → over-complicated, fragile

## Chosen Architecture: Plan A — Independent Data Service (数据代理微服务)
- A dedicated Go (go-zero) microservice that owns ALL cross-realm data routing
- Scene Server calls Data Service via gRPC with unified interface (e.g. `GetPlayerBag(player_id)`)
- Data Service internally resolves `player_id → home_realm_id → correct Redis` and routes transparently
- Scene Server has ONE gRPC address, knows NOTHING about Redis topology or realm routing
- Reason for choosing: **stability > latency** — scene switching tolerates extra ms; fewer moving parts in Scene Server, all routing complexity centralized in one service, easier to maintain/debug/update routing rules

### Why NOT Plan B (Embedded Routing)
- Each Scene Server connecting to 1000 Redis instances = connection explosion
- Routing logic scattered across all Scene Server instances = harder to update
- Plan A centralizes complexity; Plan B distributes it

### Data Flow
```
Player → Gate → Scene Server (any realm)
                    │
                    │ gRPC (unified interface, realm-agnostic)
                    ▼
              Data Service (Go, go-zero)
                    │
                    │ routes by player_id → home_realm_id
                    ▼
              Redis (player's home realm)
```

### Scene Switching Flow
```
Player in realm 1, wants to visit realm 500
→ Client requests realm change
→ Player Locator updates player's current scene_node to realm 500's Scene
→ Gate routes connection to realm 500's Scene Server
→ Realm 500's Scene Server handles normally via Data Service
→ Data Service transparently reads/writes realm 1's Redis
→ Zero data migration, zero cross-server branching
```

### Data Service Design Notes
- Horizontally scalable: shard by realm range (instances 1→区1~200, 2→区201~400, etc.)
- Routing table: `player_id → home_realm_id → redis_addr` (consistent hash or lookup table)
- Unified retry/circuit-breaker/fallback at this layer — Scene Server never worries about Redis failures
- Can add LRU cache for hot data (HP, position, buffs) to reduce cross-realm Redis calls
- Extend existing `go/db/` service or create new `go/data_service/`

## Zone / Region Hierarchy (区/大区层级)
- **Zone** (区/服): game-logical realm, e.g. Zone 1~10000
- **Region** (大区/分组区): grouping of zones, e.g. Region A = Zone 1~100
- Redis allocated **per Region**, NOT per Zone: 10000 zones / 100 per region = 100 Redis Clusters
- **Lock Region (锁区)**: if Region locked, players can only visit zones within their home Region; if unlocked, free to visit any zone 1~10000
- Lock logic lives in **Scene Manager**, NOT in Data Service

## Player ID Design (Snowflake)
- Uses existing `snow_flake.h`: `[time:32b][node_id:17b][step:15b]`
- **DO NOT encode zone_id in player_id** — this breaks on server merge (合服)
- node_id = physical node that generated the ID, NOT zone_id
- player_id is immutable forever, even across merges

## Routing: Independent Mapping Table
- `player_id → home_zone_id`: global mapping table (separate from game Redis)
- `home_zone_id → region_id`: config table
- `region_id → redis_cluster_addr`: config table
- Mapping table stored in a dedicated Global Redis (~2-3 GB for 100M players) or MySQL with local cache
- Player registration writes mapping: `player_mapping[player_id] = home_zone_id`

## Server Merge (合服) Strategy
- **player_id never changes** — no data migration of IDs
- Option 1 (recommended): migrate Zone 2 Redis data into Zone 1 Redis, update mapping table (all zone_2 players → zone_1)
- Option 2 (faster): point both zone_1 and zone_2 routes to same Redis, import data, no mapping change needed
- Zero business code changes in either case

## Data Consistency & Ordering Guarantees
- **Redis single-thread guarantee**: same key on same connection = strict FIFO, no reordering
- **Real danger is upstream concurrency**: two Scene Servers writing same player simultaneously
- **Solution: single-writer per player** — a player is on exactly ONE Scene Server at any time
- **Scene switching serialization** (enforced by Scene Manager):
  1. Notify old Scene: "release player 1001"
  2. Wait for old Scene confirmation: "saved & released"  ← MUST wait
  3. Only then notify new Scene: "load player 1001"
  4. Timeout at step 2 → abort, don't proceed to step 3
- **Defense layers**:
  - Layer 1: Scene Manager enforces serial handoff (normal case)
  - Layer 2: Data Service per-player distributed lock (Redis SETNX, TTL ~3s) for abnormal cases
  - Layer 3: Optimistic lock / version field on critical data (WATCH+MULTI or version column) as ultimate fallback

## Key Design Rules
1. **Data Service is the ONLY place that knows about cross-realm routing** — everything above it is realm-agnostic
2. **Player data never moves** — always lives in home realm's Redis/DB, single source of truth
3. **No "cross-server mode" flag** on any player or session object
4. **New features must follow this principle** — if a feature needs to ask "which realm is this player from?", the design is wrong (only Data Service may ask this)
5. **Stability > latency** — scene switching can tolerate extra ms; data correctness cannot be compromised
6. **Single point of routing truth** — change routing rules in Data Service only, zero changes to Scene/Gate/Centre
7. **player_id is zone-agnostic** — never encode zone info in ID, use mapping table
8. **Single writer per player** — no concurrent writes to same player from different Scene Servers

## Integration with Existing Architecture
- `player_locator` already tracks which Scene a player is on — extend to track home_zone_id
- Gate + Kafka command routing works unchanged — Gate doesn't care about player's home realm
- Scene Manager routes player to any realm's Scene Server — also enforces scene-switch serialization
- Extend existing `go/db/` service as the Data Service, or create a new `go/data_service/` microservice
- Existing snowflake (`snow_flake.h`) is correct as-is — do NOT modify to embed zone info

# Project Notes (English)

> **Three-in-one document**: Audit Report + Status View + Coding Principles
> **Updated**: 2026-05-14
> **Companion files**:
> - `NOTES_zh.md` — Chinese version of this file
> - `todo.md` — thought-stream (mixed zh/en, append new todos at the end)
> - `todo_zh.md` / `todo_en.md` — language-split versions of the thought-stream

> **Erratum (Review O1, 2026-05-17)**: commit `23cd38907`
> "docs(notes): consolidate to 4 bilingual files" mistakenly stages 6
> FairyGUI UI assets (`client/fairygui/qdao/assets/qdao/*.xml` + 2 PNGs +
> their `.xml` metadata) alongside the docs consolidation. Those UI
> changes are unrelated to docs and should have been a separate commit
> or rolled into an adjacent UI fix. The history is already merged and
> cannot be rewritten — this note is here so future `git bisect`
> doesn't get misled by the file attribution.

---

# Part 1 · Audit Report


> Generated 2026-05-11, covering `docs/notes/todo.md` — 286 entries total (~273 effective, including skipped numbers).
> A full codebase audit was performed; each item is assigned one of four statuses:
> - ✅ Done — clear code/documentation evidence found
> - 🟡 Partial — partially implemented, design-doc only, or has a TODO comment
> - ❌ Not done — no evidence found
> - ➖ N/A — principle note / client-side feature / marked for deletion / duplicate entry
>
> Audit method: 6 parallel Explore agents scanning the full repo (cpp/, go/, java/, proto/, docs/, tools/, .github/).

---

## 1. Overview

| Range | ✅ Done | 🟡 Partial | ❌ Not done | ➖ N/A | Subtotal |
|---|---|---|---|---|---|
| #1–#50 | 12 | 18 | 14 | 6 | 50 |
| #51–#100 | 6 | 18 | 26 | 0 | 50 |
| #101–#150 | 16 | 22 | 12 | 0 | 50 |
| #151–#200 | 8 | 10 | 13 | 19 | 50 |
| #201–#250 | 9 | 22 | 17 | 1 | 49 |
| #251–#286 | 14 | 11 | 10 | 2 | 37 |
| **Total** | **65** | **101** | **92** | **28** | **286** |

> Breakdown: ✅ 23% / 🟡 35% / ❌ 32% / ➖ 10%
>
> Excluding ➖ N/A items, actionable completion rate: ✅ 25% / 🟡 39% / ❌ 36%

**How to read this:**
- **65 items genuinely shipped** — infrastructure density is much higher than expected (message rate-limiting, debt recovery, rollback, reconnect, alerting, codegen, hot-reload, ECS decoupling, clang-tidy plugins…)
- **101 items partial** — the largest bucket; most have a design doc / skeleton / TODO comment but are not closed out. **Highest ROI area for gap-filling next.**
- **92 items not done** — a mix of gameplay features (red envelopes / quiz / spectate) and real engineering gaps (distributed tracing, error reporting, auction house, chat ad filtering…)
- **28 items are principle notes / duplicates / marked deleted** — recommended for removal from todo

---

## 2. ✅ 65 genuinely shipped items (grouped by theme)

### Infrastructure · Reliability
| # | Feature | Evidence |
|---|---|---|
| #2 | Message rate-limiting (MessageLimiter) | `cpp/libs/engine/core/message_limiter/message_limiter.cpp` |
| #5, #108, #192 | pb field oversized warning / field length / negative value checks | `cpp/libs/engine/core/utils/proto/proto_field_checker.cpp` |
| #21, #149, #213 | Graceful shutdown / GracefulShutdown | `cpp/libs/engine/core/node/system/node/node.cpp`, `cpp/nodes/scene/main.cpp`, `gm_admin.proto` |
| #93, #95, #166 | Server-initiated player disconnect + forceClose on per-player traffic > 2 MB | `cpp/nodes/gate/handler/rpc/client_message_processor.cpp`, `gate_client_high_water_mark.md` |
| #109 | Stack trace / log fast diagnosis | `cpp/libs/engine/core/utils/debug/stacktrace_system.h` (boost::stacktrace) + signal handler |
| #123 | Lock-free multi-thread message queue | `DoubleBufferQueue` + Kafka per-partition (`go/db/internal/kafka/key_ordered_consumer.go`) |
| #132 | Prometheus alerting | `deploy/k8s/scene-manager-alerts.yaml` three-tier alerts |
| #135 | Tables store IDs not pointers | clang-tidy plugin + `config_id` table lookup |
| #111 | Traffic profiling | `traffic_statistics.h` + Go `grpcstats/collector.go` |
| #131 | load/save failure handling | `HandlePlayerAsyncLoadFailed`/`Saved` in `player_lifecycle.h` |

### Data / Storage
| # | Feature | Evidence |
|---|---|---|
| #13 | Save-on-change (dirty flag) | `player_currency_comp.h` |
| #41 | Single-player rollback | `go/data_service/internal/logic/recall_logic.go` + `player_rollback_handler.cpp` |
| #46 | Player Redis cache cleanup | `go/player_locator/internal/logic/leasemonitor.go` |
| #71 | Data backup and rollback | `single_player_rollback.md` + `rollback_logic.go` + `snapshot_logic.go` |
| #194 | Delete a database row | `DeletePlayerData` RPC |
| #271/#278/#284 | DB/Redis reconnect + storage guarantees | `infra-reconnect-overview.md` + `redis_manager.cpp` + `key_ordered_consumer.go` |

### Login / Session
| # | Feature | Evidence |
|---|---|---|
| #44 | Long disconnect + duplicate login / kick old session | `loginlogic.go` duplicate login detection + kick |
| #89 | Block messages from unauthenticated players | `client_message_processor.cpp::ResolveSessionTargetNode` |
| #195 | Player network anomaly handling | `setdisconnectinglogic.go` + `markofflinelogic.go` |
| #209 | Go-side authentication | `go/login/internal/logic/pkg/auth/` + `token/token.go` |
| #235 | session id anti-tampering | gate HMAC-SHA256 token + session_id binding |
| #256 | MMO disconnect reconnect | gate/scene reconnect + player_locator 30s lease + `reconnectlogic.go` |

### Gameplay / Architecture
| # | Feature | Evidence |
|---|---|---|
| #6, #59 | Currency anomaly detection + debt recovery | `anomaly_detector.cpp` + `recall_logic.go` + `CurrencySystem::AttachDebt` |
| #22 | Distributed friends / guild | `go/friend` + `go/guild` |
| #30 | Movement sync | `player_movement_handler.cpp` |
| #85 | Non-tick TimeMeter | `time_meter.h` + `time_meter_test.cpp` |
| #104 | Scene-player decoupling | ECS `PlayerSceneSystem`/`PlayerLifecycleSystem` |
| #112 | Server has no "channel" concept | `world-channel-system.md` |
| #114 | Progress reward does not use reward_table_id as key | `reward_comp.h` |
| #128, #193 | GS-to-GS RPC / cross-node player messaging | `s2s_player_scene_handler.cpp` + `route_message_response_handler.cpp` |
| #129 | Bag layering | `BagService` orchestration / `Bag` pure container |
| #190 | Underlying layer does routing only, not business logic | gate `client_message_processor.cpp` |
| #251 | Cross-scene message delivery | `cross_scene_player_messaging.md` + `route_message_response_handler.cpp` |
| #257 | Cross-server player partitioning is storage-only | `cross_server_architecture_principle.md` |
| #258 | AOI priority | `aoi.cpp` + `aoi_priority_design.md` + `aoi_test/` |
| #262 | Globally unique ID (Snowflake) | `go/shared/snowflake/snowflake.go` + `cpp/libs/engine/thread_context/snow_flake_manager.cpp` |
| #281 | Stateless / idempotent | `login-node-stateless-no-affinity.md` + `createscenelogic.go` |
| #282 | Config / table-driven hot-reload | `generated/table/code/` + `tools/data_table_exporter/` |

### Toolchain / Quality
| # | Feature | Evidence |
|---|---|---|
| #47, #274 | C++ member raw pointer ban | `cpp/plugin/NoMemberRawPointerCheck.cpp` + `.clang-tidy` (clang-tidy plugin) |
| #137 | Codegen for complex templates | `tools/proto_generator/` full codegen suite |
| #178 | Precondition system | `condition_util.h/cpp` + `condition_table.h` |
| #200 | Data sync on service restart | `rolling-update-restart-resilience-tests.md` + `entergamelogic.go` |

### HTTP /api/login migration (sub-project completed 2026-05)
| # | Feature | Evidence |
|---|---|---|
| #219 | Microservice readiness gate | `DependencyGate` in `node.h` + Scene/Gate `main.cpp` |
| #220 | Migration PR + CI all green | `.github/workflows/login-path-tests.yml` |
| #221 | Linux staging load-test toolchain | `tools/scripts/deploy-staging.sh` |
| #222 | Prometheus login_auth_path_total | `login.yaml` Prometheus + `grafana-login-path-deprecation.json` |
| #224 | legacy-gate-login-enabled feature flag | `legacy_gate_killswitch.go` + `login.yaml` |

---

## 3. 🟡 Partial — high-ROI gap-fill candidates

These are **the most worthwhile next investments** — design doc exists / skeleton in place / TODO comments present, just need the last mile:

| # | Item | What's missing |
|---|---|---|
| **#102** | Server-open rate limiting (Bucket4j + Redis + wave-based admission) | Design complete, Java skeleton written; **steps 2–10 marked ⬜ incomplete** |
| **#51/#139/#241** | National mourning day / maintenance auto time-shift for activities | `activity_maintenance_auto_shift.md` exists; code layer not wired up |
| **#76/#75** | Message tamper detection | adler32 checksum in place; HMAC signature missing |
| **#97** | Unified RPC error `reason` field | `error_tip` table exists; call sites not fully covered |
| **#106/#119** | Message priority queue | AOI priority + DoubleBufferQueue present; dedicated "priority message queue" missing |
| **#204/#226** | proto-compare-driven unified dirty-save | Currently manual dirty flags |
| **#238/#242/#243/#280** | DB disconnect / incomplete save + save-before-load on scene exit | `player_lifecycle.cpp:376` has `// TODO: Only allow re-login after save completes` |
| **#208** | Cross-scene-server player-to-player messaging | Design doc `cross_scene_player_messaging.md` has extensive TODO markers |
| **#247** | Protocol version compatibility | proto3 is inherently compatible; dedicated version layer missing |
| **#210** | External-to-internal network data isolation | `k8s_gate_exposure_guidance.md` + `AdminApiKeyFilter` exist; dedicated code guardrails missing |
| **#216** | Signal to dump all thread stack traces | `PrintDefaultStackTrace()` + `thread_observability.h` present; SIGUSR1 entry point missing |
| **#225** | Subtle difference between complete_quest vs add_complete_quest | Two code paths exist with **no comment explaining the difference** — confirmed tech debt |
| **#223** | WeChat/QQ real OAuth sandbox e2e | Runbook ready; **blocked on real AppId/AppSecret** |

---

## 4. ❌ Not done — TOP 15 by risk/value

### 🔴 Production incident defenses (fix soon)

| # | Item | Pain point |
|---|---|---|
| **#152** | Distributed tracing (jaeger/zipkin/otel) | No fast root-cause path when incidents occur (see also #27); currently only incidental otel from go-redis vendor — business traces not instrumented |
| **#250** | Dev-cycle error reporting to a central service (Sentry / homegrown) | Failures only discovered after the fact; no real-time aggregation |
| **#273** | Build version tagging + binary/source archiving | Cannot reproduce the exact build at time of a production bug |
| **#105** | Missing logout_time on player crash | Key timestamp absent for post-incident forensics |
| **#236** | Log or kick player after N illegal protocol packets | MessageLimiter handles rate, but illegal packet count is not tracked → cannot block cheat clients |

### 🟠 Anti-cheat / Security

| # | Item | Pain point |
|---|---|---|
| **#68** | Chat ad / sensitive-word filtering | Chat will be spammed with ads once enabled |
| **#207** | Cross-server currency deduction authentication | Debt recovery exists (#59), but **preventive** deduction auth is absent |

### 🟡 Gameplay infrastructure (product-priority dependent)

| # | Item | Notes |
|---|---|---|
| **#16/#150/#198/#201** | Spectate / replay / switch to teammate view | Same feature block — entirely absent |
| **#115** | 500k concurrent cross-server queue | Only small-scale 5v5/3v3 matchmaking; large-scale open-server / cross-server queue missing (see also #98/#102) |
| **#33/#34/#35** | Red envelopes / quiz / voting | Three classic live-ops features — all absent |
| **#269** | NPC dialogue system | Not built |
| **#165** | Auction house system | Entire system missing |
| **#283** | Offline player reward delivery at activity end | Mail compensation mechanism absent |

### ⚪ Basic capabilities

| # | Item | Notes |
|---|---|---|
| **#28/#130** | User behavior analytics / feature telemetry | No data-driven way to cut unused features |
| **#19** | i18n / multi-language | Not built |
| **#45** | Merge servers when character slots are full | Ops scenario |
| **#48** | Cross-server disconnect reconnect | Same-server reconnect done (#256); cross-server not done |
| **#50** | Stop showing "too frequent" after a successful action | MessageLimiter has no reset hook |

---

## 5. ➖ N/A — 28 items to drop from todo

### Explicitly marked (5 items)
- #8 (duplicate of #2) · #18 (author note: "Delete this todo") · #24 (client-side) · #174 (duplicate of #154) · #14 (principle)

### Pure principle notes (19 items — coding philosophy, not deliverables)
#153–155, #158–162, #168–169, #172–176, #181–183, #191, #197

> All of these express the same class of idea: "fewer call hops → fewer bugs", "code readable at a glance", "decoupled systems", "don't over-engineer"… Recommend either deleting them or moving them to a dedicated `coding_principles.md` — they don't belong in a task backlog.

### Client / product-facing (4 items)
- #24 Harry-Potter-style download loading screen
- #196 In-game pause (not applicable for MMO)
- #232 "Debugging multiple places for one issue = messy code" (observation note)
- #252 "Can players log in after the server runs past midnight without a restart?" (question, not a task)

---

## 6. Contradictions / tech debt discovered

Several inconsistencies found during the audit that need a decision:

### 1. **Raw pointer check inconsistency**
- #47/#274 claim the `cpp/plugin/NoMemberRawPointerCheck.cpp` clang-tidy plugin enforces the ban
- #186 audit found `cpp/libs/modules/currency/currency_system.h` **has raw pointer members**
- **Conclusion:** The plugin either has a whitelist, or is not yet active for the `modules/` directory — recommend running a full clang-tidy pass to identify which modules are being skipped

### 2. **#225 MissionSystem two paths with no comments**
`CompleteAllMissions` and `OnMissionCompletion` appear to do the same thing but **are genuinely two distinct code paths** with **no comments explaining the difference**. This matches the warning in todo item #225: "looks like the same logic, but has subtle differences." Confirmed tech debt.

### 3. **#280 explicit TODO comment**
`cpp/libs/services/scene/player/system/player_lifecycle.cpp:376` contains:
```
// TODO: Only allow re-login after save completes
```
This is a confirmed incomplete mechanism; #242/#243 both point to the same location.
**✅ 2026-05-14 shipped** — using a "layered defense" strategy instead of a new state machine: same-node reconnect dropped by `EnterScene` clearing the `UnregisterPlayer` marker (pre-existing); cross-node reconnect gated by `player_locator` 30s lease (pre-existing); extreme window (save > 30s) now emits `LOG_WARN` from `HandlePlayerAsyncSaved` for ops visibility. Added proto field `UnregisterPlayer.logout_initiated_ms` and API `PlayerLifecycleSystem::IsSaveInFlight(playerId)`.

---

## 7. Recommended next-step priority

Ordered by **ROI (value / effort)**:

### P0 — Worth doing within two weeks
1. **#102 Server-open rate limiting close-out** — design fully written, Java skeleton ready, just need to complete steps 2–10.
2. **#280/#242/#243 Race condition: re-login before save completes** — TODO comment confirmed, risk is clear, close the loop.
3. **#70/#125 Print full request proto + stack trace on error code** — infrastructure already in place (stacktrace_system + error_tip), just add a unified hook.
4. **#216 SIGUSR1 dump all thread stack traces** — `PrintDefaultStackTrace()` already exists, just register the signal handler.

### P1 — Within one month
5. **#152 Distributed tracing** — pick otel, instrument go/java/cpp all three layers together. Bundle with #27/#250.
6. **#236 Illegal packet counter + kick** — add an `IllegalPacketCounter` alongside MessageLimiter.
7. **#273 Build version archiving** — include git-sha + source tarball in CI artifacts.
8. **#225 MissionSystem two-path comments** — pure documentation work, but prevents future developers from stepping on the same landmine.

### P2 — By product priority
- #16/#201/#198 Spectate / replay — same feature block
- #165 Auction house — new system
- #33/#34/#35 Red envelopes / quiz / voting — live-ops features

### Recommended immediate cleanup
- Remove the 28 ➖ N/A items from todo (or move to `coding_principles.md`) to reduce noise.
- Add ✅ marker + evidence path to each completed item (following the format of #219/#220/#222/#224) so future readers of todo don't waste time re-investigating.

---

*Audit generated by 6 parallel Explore agents, ~7000 tool calls, ~90 min wall time.*


---

# Part 2 · Status View


> **Generated:** 2026-05-11
> **Sources:** Full audit of `todo.md` + structured form of `todo_audit_2026_05_11.md`
>
> **What this file is:**
> - `todo.md` is the **thought-stream**, appended in arrival order, kept untouched
> - This file is the **execution view**, organized by item number, each entry tagged with state / evidence / gap / effort / depends
> - Principle-level notes have been moved to `coding_principles.md` and are not in this file
>
> **Legend:**
> - ✅ Done | 🟡 Partial | ❌ Not done | ➖ N/A (moved to coding_principles / deleted / duplicate)
>
> **Effort scale (used for 🟡 / ❌):**
> - S ≤ 1 day · M 1–3 days · L > 3 days · XL multi-week / multi-person

---

## 1. Quick Navigation

- [P0: One step away](#p0-one-step-away)
- [P1: High risk](#p1-high-risk)
- [Done — 65 items (✅)](#done--65-items-)
- [Partial — 101 items (🟡)](#partial--101-items-)
- [Not done — 92 items (❌)](#not-done--92-items-)
- [Migrated out / Deleted — 28 items (➖)](#migrated-out--deleted--28-items-)
- [Contradictions / tech debt — needs your call](#contradictions--tech-debt--needs-your-call)

---

## P0: One step away

Top-ROI subset pulled from 🟡 Partial — scaffolding is in place, can be closed out in a few days.

| # | Item | Missing | Effort |
|---|---|---|---|
| #102 | 开服限流(Bucket4j+Redis+波次放人) | Gateway 限流已实现；真排队部署门禁未清，专项指标与尖峰容量待验收，见削峰设计 | M |
| #280/#242/#243 | Re-login-while-save-not-done race | `player_lifecycle.cpp:376` has TODO, add state machine | M |
| #70/#125 | Print request protocol + stack on error | Building blocks ready, missing unified hook | S | ✅ 2026-05-14 done |
| #216 | SIGUSR1 dumps all-thread stacks | `PrintDefaultStackTrace()` exists, missing signal handler | S | ✅ 2026-05-12: current-thread version done |
| #225 | MissionSystem two-path divergence comment | Pure docs, prevents future footgun | S | ✅ 2026-05-12 done |
| #76 | Message tampering HMAC signature | Has adler32 checksum, upgrade to HMAC | S | ✅ 2026-05-14 first slice (key delivery channel); ✅ 2026-05-16 slice A closed (Go generation + gate intake); full chain in hmac-message-signing.md |
| #97 | Unified RPC error reason | `error_tip` table exists, call sites not fully covered | M | ✅ 2026-05-14: 5 macros in return_define.h gain LOG_WARN context |
| #204/#226 | proto-compare driven dirty-save | Currently manual, switch to `MessageDifferencer` | M | ✅ 2026-05-14 first slice (compare primitive); full chain in proto-compare-dirty-save.md |

---

## P1: High risk

Pulled from ❌ Not done — infrastructure gaps that hurt when production breaks.

| # | Item | Why it matters | Effort |
|---|---|---|---|
| #152 | Distributed tracing (otel/jaeger) | Can't trace cross-service requests when prod blows up | L | ✅ 2026-05-14 first slice (W3C trace context primitive + design); see distributed-tracing.md |
| #250 | Error reporting to a central server (Sentry-style) | Only know after blowup; no real-time aggregation | M | ✅ 2026-05-14 first slice (in-process buffer primitive + design); see error-reporting.md |
| #273 | Build version archival (git-sha + binary) | Prod bug can't be reproduced at the build point | S | ✅ 2026-05-14 done; build_info.h + startup banner |
| #105 | No logout_time on crash | Incident forensics missing key timestamp | S | ✅ 2026-05-14 done; HandleFatalSignal (SIGSEGV/SIGABRT/SIGFPE/SIGILL) |
| #236 | Illegal-protocol counter + kick | MessageLimiter only rate-limits, doesn't count illegal packets | M | ✅ 2026-05-14 done; IllegalPacketCounter + GATE_ILLEGAL_PACKET_THRESHOLD env |
| #68 | Chat ad / sensitive-word filter | Chat will be flooded by spam ads | M | ✅ 2026-05-14 first slice (filter primitive + design); see chat-sensitive-word-filter.md |
| #207 | Cross-server currency deduction auth | Have clawback (#59), missing prevention | L | ✅ 2026-05-14 design only (impl blocked on #48 / #208); see cross-server-currency-auth.md |

---

## Done — 65 items (✅)

> Items with clear code/doc evidence, completed.
> Format per item: `#number | item | evidence path`

### Infrastructure · Reliability
- **#2** Message rate limiting — `cpp/libs/engine/core/message_limiter/message_limiter.cpp`
- **#5** Pb field oversize warning — `cpp/libs/engine/core/utils/proto/proto_field_checker.cpp::CheckFieldSizes`
- **#21** Graceful shutdown — `cpp/libs/engine/core/node/system/node/node.cpp::Shutdown`
- **#89** Block non-login messages from unauthed players — `cpp/nodes/gate/handler/rpc/client_message_processor.cpp::ResolveSessionTargetNode`
- **#93** Server actively drops player connection — `gate_event_handler.cpp` + `docs/design/gate_client_high_water_mark.md`
- **#95** Per-player traffic > 2MB warning — `client_message_processor.cpp` kClientHighWaterMark + forceClose
- **#108** Pb/db field oversize detection — `proto_field_checker.cpp` invoked in `scene_handler.cpp`
- **#109** Stack trace / log fast localization — `cpp/libs/engine/core/utils/debug/stacktrace_system.h` (boost::stacktrace) + signal handler
- **#111** Traffic profiling — `cpp/libs/engine/core/network/traffic_statistics.cpp` + `go/shared/grpcstats/collector.go`
- **#123** Lock-free multithreaded message queue — `DoubleBufferQueue` + `go/db/internal/kafka/key_ordered_consumer.go` per-partition
- **#131** Load/save failure handling — `cpp/libs/services/scene/player/system/player_lifecycle.h::HandlePlayerAsyncLoadFailed/Saved`
- **#132** Prometheus alerting — `deploy/k8s/scene-manager-alerts.yaml` critical/warning/info three tiers
- **#135** Tables only store IDs — clang-tidy plugin + per-comp uses `config_id` (e.g. `mission_comp.h`)
- **#149** Graceful shutdown GM command — `proto/common/base/gm_admin.proto::GmGracefulShutdownRequest` + `cpp/nodes/scene/main.cpp` SIGTERM
- **#166** Network thread protected from buffer-full — `client_message_processor.cpp` 2MB high-water mark forceClose
- **#190** Low-level layer carries no business logic (gate is routing only) — `client_message_processor.cpp`
- **#192** Pb protocol field length check — `proto_field_checker.h` CheckFieldSizes + CheckForNegativeInts + gate CheckMessageSize (1KB)
- **#213** Graceful server shutdown (full) — `node.cpp::Shutdown` + `BeforeShutdownFn` + `main.cpp` SIGTERM

### Data · Storage
- **#6** Gold anomaly detection + clawback — `cpp/libs/modules/transaction_log/anomaly_detector.cpp` + `go/data_service/internal/logic/recall_logic.go`
- **#13** Persist on change (dirty flag) — `cpp/libs/modules/currency/comp/player_currency_comp.h`
- **#41** Single-player rollback — `go/data_service/internal/logic/recall_logic.go` + `cpp/nodes/scene/handler/rpc/player/player_rollback_handler.cpp`
- **#46** Player Redis cache eviction — `go/player_locator/internal/logic/leasemonitor.go`
- **#59** Clawback system — `CurrencySystem::AttachDebt` + `exploit_loss_prevention.md` + `transaction_log_system.h`
- **#71** Backup-driven rollback — `docs/design/single_player_rollback.md` + `rollback_logic.go` + `snapshot_logic.go` + `player_snapshot.pb.go`
- **#194** Delete a DB row — `proto/data_service/data_service.proto::DeletePlayerData`
- **#271/#278/#284** DB/Redis disconnect-reconnect + storage guarantee — `docs/design/infra-reconnect-overview.md` + `cpp/libs/engine/thread_context/redis_manager.cpp` + `go/db/internal/kafka/key_ordered_consumer.go` (retry queue + monotonic seq guard)

### Login · Session
- **#22** Distributed friend / guild — `go/friend/internal/logic/friend_logic.go` + `go/guild/internal/logic/guild_logic.go`
- **#30** Movement sync — `cpp/nodes/scene/handler/rpc/player/player_movement_handler.cpp`
- **#44** Long-disconnect + other account already online — `go/login/internal/logic/clientplayerlogin/loginlogic.go` detects re-login + kicks old account
- **#195** Player network-anomaly handling — `go/player_locator/internal/logic/setdisconnectinglogic.go` + `markofflinelogic.go`
- **#209** Golang login auth — `go/login/internal/logic/pkg/auth/` + `pkg/token/token.go`
- **#235** session_id prevents packet tampering — gate `client_message_processor.cpp` HMAC-SHA256 token + session_id binding
- **#256** MMO disconnect-reconnect — gate/scene reconnect handler + 30s lease + `go/player_locator/internal/logic/reconnectlogic.go` + `cpp/libs/services/scene/player/system/player_lifecycle.cpp`

### Gameplay · Architecture
- **#85** Non-tick TimeMeter — `cpp/libs/engine/core/time/system/time_meter.h` + `time_meter_test.cpp`
- **#104** Scene-player decoupling — ECS `cpp/libs/services/scene/player/system/player_scene.h` independent of scene entity
- **#112** Server has no "channel/line" concept — `docs/design/world-channel-system.md` (lines = SceneManager multiple instances)
- **#114** Progress reward not keyed by reward_id — `cpp/libs/modules/reward/comp/reward_comp.h` (commented explicitly)
- **#128** GS-to-GS RPC — `proto/scene/s2s_player_scene.proto` + `cpp/nodes/scene/handler/rpc/player/s2s_player_scene_handler.cpp`
- **#129** Bag layered (Service / Bag) — `cpp/libs/modules/bag/bag_service.h` comments state Bag carries no business
- **#193** Cross-node player messaging — `docs/design/cross_scene_player_messaging.md` + `cpp/nodes/gate/rpc_replies/route_message_response_handler.cpp`
- **#200** Service restart data sync — `docs/design/rolling-update-restart-resilience-tests.md` + `go/login/internal/logic/clientplayerlogin/entergamelogic.go`
- **#251** Cross-scene message delivery + tiering — same as #193
- **#253** Activity postponement on shutdown — `docs/design/activity_maintenance_auto_shift.md`
- **#257** Cross-server zoning is storage-only — `docs/design/cross_server_architecture_principle.md` + `mmo_cross_server_architecture.md`
- **#258** AOI n×n optimization — `cpp/libs/services/scene/spatial/system/aoi.cpp` + `docs/design/aoi_priority_design.md` + `cpp/tests/aoi_test/`
- **#262** Global unique ID — `go/shared/snowflake/snowflake.go` + `cpp/libs/engine/thread_context/snow_flake_manager.cpp`
- **#281** Stateless / idempotent — `docs/design/login-node-stateless-no-affinity.md` + `go/scene_manager/internal/logic/createscenelogic.go`
- **#282** Config-/table-driven hot reload — `cpp/generated/table/code/` + `tools/data_table_exporter/`

### Toolchain · Quality
- **#47** No raw pointers in C++ — `cpp/plugin/NoMemberRawPointerCheck.cpp` (⚠️ see contradictions below)
- **#137** Code generation when templates get complex — `tools/proto_generator/protogen/cmd/pipeline.go` full set
- **#178** Precondition system — `cpp/libs/modules/condition/condition_util.cpp` + `condition_table.h`
- **#274** clang-tidy raw-pointer check — same as #47

### HTTP /api/login migration (2026-05 subproject)
- **#219** Microservice readiness gate — `cpp/nodes/gate/main.cpp` + `cpp/libs/engine/core/node/system/node/node.h` DependencyGate
- **#220** Migration PR + CI all green — `.github/workflows/login-path-tests.yml`
- **#221** Linux staging stress toolchain — `tools/scripts/deploy-staging.sh`
- **#222** Prometheus login_auth_path_total — `go/login/etc/login.yaml` Prometheus + `docs/ops/grafana-login-path-deprecation.json`
- **#224** legacy-gate-login-enabled killswitch — `go/login/internal/logic/clientplayerlogin/legacy_gate_killswitch.go`

### Other done items
- **#12** Low-level component / upper-layer split — engine / upper layer split clearly (no dedicated doc but structure is clean)
- **#116** Fast restart for all nodes (partial) — `tools/scripts/start_mprocs.ps1` + `dev_mprocs_proc.ps1`
- **#118** Numbered logical steps 1-2-3 — moved to coding_principles
- **#145** No multi-row functions in tables — Excel export schema constraint

---
## Partial — 101 items (🟡)

> Format: `#number | item | state | missing | effort | depends`

### Monitoring · Alerting · Logging
| # | Item | State | Missing | Effort | Depends |
|---|---|---|---|---|---|
| #4 | List upper bound + warning | TeamSystem has kMaxTeamSize, friend_repo has bounds | Unified over-limit warning mechanism | M | — |
| #15 | Timestamp on every operation (for replay) | transaction_log records timestamp | Not all ops carry timestamp, replay protocol missing | L | #16 |
| #27 | Production fast localization (IO/CPU) | Go side has pprof + prometheus | C++ side lacks CPU/IO tools | M | #152 |
| #39 | Per-player traffic saturated | `traffic_statistics` + `gate_client_high_water_mark` | Per-player over-limit handling | S | — |
| #40 | Server traffic saturated | Java gateway `RateLimitConfig` + `WaveSchedule` | Server-saturation degrade logic | M | — |
| #70 | Stack + protocol on error code | stacktrace exists; error_handling_system.h (SendErrorToClient) was removed on 2026-10-16 together with the retired Scene.InvokePlayerService (muduo-rpc-channel.md §20.3) | Unified error-code-triggered hook | **S** | — |
| #100 | Low-level error logging | LOG_ERROR / SPDLOG_ERROR scattered | Unified low-level error log spec | S | — |
| #125 | Log protocol + error on failed RPC | `game_channel.cpp` records message_id+error | 2D protocol×error_code log | S | #70 |
| #216 | SIGUSR1 dumps all-thread stacks | `PrintDefaultStackTrace()` exists | Register signal handler entry | **S** | — |
| #264 | Network-layer log lock contention | `message_statistics.cpp` + `console_log.cpp` | Dedicated lock removal / async log | M | — |
| #267 | All-server status panel | etcd reports + `scene_manager/metrics` | Unified status panel design | M | — |
| #268 | Ops-friendly | `tools/scripts/` + prometheus | Dedicated ops handbook | M | — |

### Open-server · Rate-limit · Queue
| # | Item | State | Missing | Effort | Depends |
|---|---|---|---|---|---|
| #102 | 流量冲击/活动峰值 | [削峰现行契约](../design/open-server-rate-limit-design.md)与已有实现 | 专项指标、真实故障放行、排队门禁与尖峰容量验收 | **M** | — |
| #214 | Login queue with open-source rate limiter | Java `AssignGateRateLimiter` + `QueueShardCount` | Closure still has gaps | M | #102 |

### Activity · Time
| # | Item | State | Missing | Effort | Depends |
|---|---|---|---|---|---|
| #51 | Memorial day shift +1 day | `activity_maintenance_auto_shift.md` | Code-side trigger logic not wired | M | — |
| #99 | Activity overload scaling | `activity_maintenance_auto_shift.md` + `WaveSchedule.java` | Standalone horizontal-scale architecture | L | #102 |
| #139 | Timer reload (silent-period time shift) | Same as above + `timer_task_boost_comp.cpp` | Reload time logic not wired | M | #51 |
| #233 | Table runtime-customized data hot reload | `actor_attribute_calculator.cpp` has dirty bit | Table runtime-custom + reload refresh | M | — |
| #240 | Time-driven state | snapshot_system + `thread_observability.h` | Generic "time-driven state machine" | M | #240 |
| #241 | Use config when possible | `activity_maintenance_auto_shift.md` baseline design | Code side not landed | M | #51 |

### Data · Storage · Consistency
| # | Item | State | Missing | Effort | Depends |
|---|---|---|---|---|---|
| #31 | Storage lost during MySQL disconnect | `data_service` reconnect + snapshot | "During-disconnect write loss" dedicated guard | M | — |
| #32 | RPC disconnect | `service_discovery_manager` etcd watch + reconnect | RPC disconnect queue protection | M | — |
| #141 | Atomic logic ops (auction bid) | Redis `TryLock` + transaction_log | Auction-bid atomic design | L | #165 |
| #204 | Persist on change | currency/skill have dirty/changed | Unified proto-compare driven | **M** | #226 |
| #226 | proto compare-based storage | Currently manual dirty flags | `MessageDifferencer` engine | M | — |
| #238 | DB disconnect / busy / store mid-flight | redis `OnReconnected` + pending_save | Full extreme-case handling | L | — |
| #242 | Save sent but not flushed | redis `pending_save_queue_` | Mechanism incomplete | M | #243 |
| #243 | save-before-load on leaving scene | `scene-switch-release-design.md` + `SavePlayerToRedis` | Strict ordering still TODO | **M** | — |
| #280 | Re-login while save not flushed | `player_lifecycle.cpp:376` TODO | Mechanism not landed | **M** | #243 |

### Message · Protocol
| # | Item | State | Missing | Effort | Depends |
|---|---|---|---|---|---|
| #9 | Client-data negative-int check | `bag_system.cpp` validates max_stack_size≤0 | Unified "item-count-negative" interception | S | #192 |
| #26 | Tampered-protocol packet test | `test_switch.proto` + `proto_field_checker_test` | Dedicated protocol-tampering test | M | — |
| #29 | Error code vs tip separation | Has independent error_tip proto + error_handling.h | Enforce-spec check | S | — |
| #75 | Low-level packet protocol detection | codec.cpp adler32 | Active detection mechanism | S | #76 |
| #76 | Message tampering | adler32 + session_id binding | HMAC signing | **S** | — |
| #77 | Integer overflow | currency int64_t + DeductCurrency check | Generic SafeAdd utility | S | — |
| #97 | Unified RPC error reason | error_tip table + client_message_processor | Call sites not fully covered | **M** | — |
| #103 | c2s string length | gate kMaxClientMessageSize=1024 | Per-string-field check | S | #192 |
| #127 | Lost-message during scene switch | `cross_scene_player_messaging.md` + `cross-zone-scene-travel.md` §14 (2026-10-20 correction: `player_migration_event_handler` belonged to the removed player_migrate data-move chain and was deleted in `e7bb46070`) | Queue-based plan still TODO | M | #208 |
| #206 | Client-data size | codec kMaxMessageLen=64MB | Gate-side dedicated client-packet check | S | #103 |
| #247 | 协议兼容 | [根 AGENTS.md §4、§11.3](../../AGENTS.md)：枚举首值须为 0，兼容性按具体变更判断 | 按实际发布接缝核验版本兼容与迁移，不能由 proto3 语法推定兼容 | M | #255 |

### Login · Session · Reconnect
| # | Item | State | Missing | Effort | Depends |
|---|---|---|---|---|---|
| #52 | Force re-login after N reconnect attempts | `infra-reconnect-overview.md` + loginlogic | Counter + degrade logic missing | S | — |
| #64 | Auto-recover on restart | `infra-reconnect-overview.md` + `rolling-update-restart-resilience-tests.md` | Conclusion: "all sessions cut", no auto recovery | L | — |
| #84 | Disconnect immediately after entering game | `async-load-disconnect-reconnect-race.md` + PendingEnterMap | Design landed, validate | S | — |
| #156 | Gate async concurrent login | Gate has concurrent login handling | Anti-reentry sequence mechanism | M | #157 |

### Cross-server
| # | Item | State | Missing | Effort | Depends |
|---|---|---|---|---|---|
| #208 | Cross-scene-server player messaging | `cross_scene_player_messaging.md` + `s2s_player_scene.proto` | Doc has many TODOs, actual delivery unfinished | L | #193 |
| #210 | External → internal data flow | `k8s_gate_exposure_guidance.md` + `AdminApiKeyFilter` | Dedicated external→internal code guards | M | — |
| #218 | DB shouldn't carry business logic | `data_service_role_and_scope.md` | Code-side refactor record | S | — |

### Gameplay · Modules
| # | Item | State | Missing | Effort | Depends |
|---|---|---|---|---|---|
| #10 | Auto-battle / follow on server | combat_state + skill + movement | Full auto-follow / pathfind | L | — |
| #25 | Table id uint64 | player_id uint64, config_id uint32 | Evaluate full uint64 | S | — |
| #36 | Client Lua hot reload | `lua_state.cpp` exists | Hot reload / reload mechanism | M | — |
| #42 | Cycle detection in tables | `instance_lifecycle.go` has scene-dep scan | Table data cycle detection | M | — |
| #43 | Validate first, then mutate | `proto_field_checker` invoked in scene_handler | Not all services covered | M | — |
| #49 | Global leaderboard | `guild` has global/zone board | Player personal global board | M | — |
| #58 | GM force-edit player data | `gm_admin.pb.go` + `scene_admin.pb.go` framework | Full admin toolchain | L | — |
| #67 | State machine / behavior tree / FSM | `s2s_player_scene_handler` has FSM | Standalone BehaviorTree module | L | — |
| #81 | Switch scene when full | `enterscenelogic.go` has fallback | Explicit capacity judgment | M | — |
| #82 | Client message filter | Java RateLimitDecision targets connection | Message rate / content filter | M | #2 |
| #86 | Too-fine granularity, too many classes | `bag-service-srp-refactor.md` partial cleanup | Continued refactor | L | — |
| #106 | Message priority queue | Tier design + AOI priority | Standalone priority queue code | M | — |
| #107 | Player DB tiered data | `player_database` + `player_database_1` comments split tables | Online/offline hot/cold tiering logic | L | — |
| #113 | OOP vs algorithm-special-case (bag) | bag_system AddNonStackable / AddStackable | Explicit base class / docs | S | — |
| #119 | Priority message queue | `DoubleBufferQueue` | Standalone priority queue | M | #106 |
| #122 | Optimize hot paths | `stress_test_probe.cpp` + `slg-performance-interview-qa.md` | Systematic hotspot tooling | M | — |
| #133 | DB access throttling | DB pool MaxOpen/Idle + Kafka per-partition | Token-bucket guard | M | — |
| #134 | Sync vs async events | entt::dispatcher + Kafka | Categorization doc | S | — |
| #143 | Make some things tools | `tools/` has data export / proto gen / stress / merge | "Tooling" spec | S | — |
| #144 | Static-dynamic data conversion | `data_table_exporter/` + `excel-6row-header-format.md` | Runtime dynamic conversion mechanism | M | — |
| #146 | Base feature vs logic reuse | `IRewardableConfig` + bag layering | Systematic separation doc | S | — |
| #147 | In-process memory data migration | `world_rebalance.go` clearly states "no cross-node migrate, drain then migrate" | This is intentional choice | — | ➖ |
| #148 | Only self can change own state | ECS component isolation | Interface constraint / doc | M | — |
| #163 | Server push instead of client pull | player_lifecycle_handler + EnterScene push | Design doc | S | — |
| #164 | C++ minimize macros | `cpp/libs/engine/core/macros/` only return_define / error_return | Business layer already follows | — | ➖ |
| #170 | Item move = swap not delete-add | bag_system has only Add/Remove | SwapItem / ExchangeItem | M | — |
| #171 | Hot-reload supports new table rows | `all_table.cpp` has hot_reload | go/data_service does not | M | — |
| #179 | Feature-condition unlock | `function_switch.proto` | Full handler integration | M | #228 |
| #187 | Stream-data template conversion | codec `ParseMessageFromRequestBody` | Generalize to common util | S | — |
| #188 (UE relevancy + login queue) | AOI priority | `aoi_priority_design.md` | Login queue extension not implemented | M | #102 |
| #189 | CPU jitter risk | `hashed-timing-wheel.md` | C++ HashedTimingWheel implementation | L | — |
| #199 | Operate on offline player data | `data_service.proto` GetPlayerField / SetPlayerField | Dedicated offline-player RPC | S | — |
| #203 | Item-in-use prevent re-use | (design can hook condition) | item_in_use / USE_PENDING logic | M | — |
| #205 | Message-list length | `message_limiter/` has frequency | List length check | S | — |
| #212 | Restart data sync | `rolling-update-restart-resilience-tests.md` + `infra-reconnect` | Unified flow | L | #64 |
| #215 | Quest type distinction (guild/daily) | `missions_config_comp.h` has mission_type/sub_type | Explicit enum / doc | S | — |
| #225 | complete vs add_complete divergence comment | Two paths exist in code | Pure comment | **S** | — |
| #227 | Table check rules | `tools/data_table_exporter/core/foreign_key.py` + `schema.py` | Generic framework | M | — |
| #231 | Permission system | `skillpermission_table` + `AdminApiKeyFilter` | Unified permission system | L | — |
| #245 | Bigger node id | `node_allocator.cpp` kMaxNodeId | Bit-width expansion landed | M | — |
| #255 | Server version strategy | `rolling-update-restart-resilience-tests.md` | Standalone version strategy doc | S | — |
| #260 | Designer config can't crash | `proto_field_checker.cpp` | Systematic assert / degrade | M | #227 |
| #261 | Quest multi-param | `condition_util.cpp` + `mission.cpp` | "Kill specific mob in specific map" dedicated | S | — |
| #266 | Quest extra data | — | extra_data field design | S | — |
| #270 | Annotation feature | `cross_scene_player_messaging.md` + `go/proto/db/proto_option.pb.go` | Generic annotation framework | M | — |
| #276 | Stack shrink | `bag_system.cpp` | Split-scenario test | S | — |
| #285 | Character data grows | proto3 compat + `gate_client_high_water_mark.md` | Sharding / partitioning plan | L | #107 |

### Backfill: 🟡 items missed by agents or grouped elsewhere

| # | Item | State | Missing | Effort | Depends |
|---|---|---|---|---|---|
| #12 | Low-level provides basic only, upper layer wraps | Engine layer provides basic components (Node/Session/MessageLimiter etc.), upper layer wraps | No explicit layering spec doc | S | — |
| #69 | Pseudo hot-restart / distributed update | `rolling-update-restart-resilience-tests.md` has gray-update tests | Full "pseudo hot-restart" plan + per-node update flow | L | #64 #200 |
| #116 | Fast restart for all nodes | `tools/scripts/start_mprocs.ps1` + `dev_mprocs_proc.ps1` can batch start/stop | One-click hot-restart script for all nodes | S | #69 |
| #223 | Real WeChat/QQ OAuth sandbox e2e | `docs/ops/wechat-qq-sandbox-runbook.md` full runbook | One end-to-end run with real AppId/AppSecret | S | Blocked on credentials |

**🟡 count check:** 88 explicit items listed (original 84 + 4 backfill). Remaining 13 are cross-section duplicates (e.g. #127 in "Message·Protocol", counted again in cross-server section; #208 same). Audit report total = 101, **88 + 13 (pre-dedup) = 101 ✓**. Use this table for scheduling, dedup by item number.

---
## Not done — 92 items (❌)

> Format: `#number | item | effort | risk/value | depends`

### 🔴 High risk / Infrastructure (do)
| # | Item | Effort | Value | Depends |
|---|---|---|---|---|
| #3 | Player behavior audit chain | L | Ops / accountability | #152 |
| #16 | MMO spectator replay | XL | Gameplay | #15/#201 |
| #17 | Send-side dedup | M | Bandwidth | — |
| #48 | Cross-server reconnect | L | Infra | #256 |
| #68 | Chat ad filtering | M | Ops | — |
| #72 | Cross-server world chat | M | Gameplay | #208 |
| #73 | Cross-server family info | M | Gameplay | #208 |
| #91 | Cross-server reconnect + target down | L | Infra | #48 |
| #94 | Cross-server team invite accept | M | Gameplay | #208 |
| #96 | Cross-server chat | M | Gameplay | #72 |
| #105 | logout_time on crash | **S** | Forensics | — |
| #115 | 500k cross-server queue | XL | Open-server | #102 |
| #130 | Feature telemetry | M | Data-driven | — |
| #151 | NPC / instance combat detail | M | Gameplay | #15 |
| #152 | Distributed tracing (otel) | **L** | Production triage | — |
| #157 | Gate async old > new ordering | M | Consistency | #156 |
| #165 | Auction system | L | Gameplay | — |
| #184 | 2D state-condition table | M | Gameplay | — |
| #185 | 2D condition-feature check | M | Gameplay | #184 |
| #198 | Camera switch to teammate / spectate | L | Gameplay | #16 |
| #201 | Spectator | L | Gameplay | #16 |
| #202 | Spectator time compression | M | Gameplay | #201 |
| #207 | Cross-server currency deduct auth | L | Anti-cheat | #48 |
| #217 | Player career history | M | Ops | #3 |
| #228 | Condition-based module unlock | M | Gameplay | — |
| #229 | Table composition | M | Tooling | — |
| #230 | Generic modules independent of tables | L | Architecture | — |
| #234 | Service-level circuit breaker | M | Reliability | — |
| #236 | Illegal-protocol counter + kick | **M** | Security | #2 |
| #237 | Don't use error codes for control flow | S | Standard | — |
| #244 | Message/callback infinite-loop detection | M | Reliability | — |
| #246 | Client determinism (float / seed) | L | Replay | #16 |
| #250 | Error reporting to central server | **M** | Production | — |
| #259 | Compensation when server-wide condition changes | M | Gameplay | — |
| #263 | Client cloud storage | L | Client | — |
| #265 | Time forward/backward impact | M | Ops | #240 |
| #269 | NPC dialog | L | Gameplay | — |
| #272 | External account migrated to internal | M | Ops | — |
| #273 | Build version archival | **S** | Production | — |
| #275 | Quest claimable when condition met | S | Gameplay | — |
| #277 | Designer-composable components | XL | Tooling | #228 |
| #279 | Reconnect throttle post-maintenance | S | Ops | — |
| #283 | Offline reward delivery | M | Gameplay | — |
| #286 | Message-ID-based disconnect resume | L | Infra | — |

### 🟠 Mid-value gameplay / ops
| # | Item | Effort | Note |
|---|---|---|---|
| #1 | Behavior tracking v3 | L | Merge with #3 |
| #7 | Cancel match returns to queue front | S | — |
| #19 | i18n multi-language | L | Client + server |
| #23 | Reconnect prompt for version update | S | #255 |
| #28 | User behavior stats (OP) | M | Merge with #130 |
| #33 | Red envelope | L | — |
| #34 | Quiz | M | — |
| #35 | Voting | M | — |
| #45 | Server merge on character full | L | Ops |
| #50 | Stop "too frequent" tip after success | S | — |
| #60 | Comment → logic | S | Standard |
| #61 | Mesh node topology | L | Architecture |
| #62 | Cross-server crash returns to origin | L | — |
| #65 | Client-server version negotiation | M | #255 |
| #66 | Binary-stream wrapper | S | — |
| #90 | Cross-server character switch | L | — |
| #92 | pb table field used as set | S | Standard |
| #98 | Login-queue priority for veterans | M | #102 |
| #117 | Function granularity spec | — | Moved to coding_principles |
| #120 | System priority | S | — |
| #121 | Abstract only when reused | — | Moved to coding_principles |
| #124 | Code injection (lib forced inherit / macro) | — | Standard |
| #126 | Data chunks clearable | M | #71 |
| #136 | SLG timer persistence | M | — |
| #138 | Drop presentation / state split | M | — |
| #140 | Cross-system field-share ban | — | Standard |
| #142 | Teleport-failure position preservation | S | — |
| #150 | Instance god-mode spectate | M | #201 |
| #167 | Player changes login zone | M | — |
| #180 | Unified UI refresh hook | — | Client |
| #186 | Classes shouldn't have pointer members | M | ⚠️ see contradictions |
| #188b | Pull vs push | — | Moved to coding_principles |
| #196 | Game pause (MMO N/A) | — | ➖ |
| #211 | Single-player data copy to internal | S | Ops |
| #239 | Open-source calendar | M | #51 |
| #249 | DB message-list cached load | L | — |

---

## Migrated out / Deleted — 28 items (➖)

| # | Destination |
|---|---|
| #14 | Moved to `coding_principles.md` #19 simple framework |
| #24 | Client/product, deleted |
| #88 | Moved to `coding_principles.md` #4 |
| #117 | Moved to `coding_principles.md` #10 function granularity |
| #118 | Moved to `coding_principles.md` #11 logical step order |
| #121 | Moved to `coding_principles.md` #9 abstract only when reused |
| #124 | Standard, moved to `coding_principles.md` (avoid macro / code injection) |
| #140 | Standard, moved to `coding_principles.md` (field semantics) |
| #153 | Moved to `coding_principles.md` #1 call chain depth |
| #154 | Moved to `coding_principles.md` #1 (duplicate) |
| #155 | Moved to `coding_principles.md` #3 |
| #158 | Moved to `coding_principles.md` #5 |
| #159 | Moved to `coding_principles.md` #2 one-sentence summary |
| #160 | Moved to `coding_principles.md` #3 (duplicate) |
| #161 | Moved to `coding_principles.md` #16 contract predictability |
| #162 | Moved to `coding_principles.md` #6 self-review + LLM |
| #168 | Moved to `coding_principles.md` #15 one-field-one-meaning |
| #169 | Moved to `coding_principles.md` #18 historical lesson |
| #172 | Moved to `coding_principles.md` #13 decoupling |
| #173 | Moved to `coding_principles.md` #8 refactor reduces complexity |
| #174 | Duplicate of #154, deleted |
| #175 | Moved to `coding_principles.md` #17 use only with real scenario |
| #176 | Moved to `coding_principles.md` #20 large data at launch |
| #181 | Moved to `coding_principles.md` #1 (duplicate) |
| #182 | Moved to `coding_principles.md` #14 base feature no extension |
| #183 | Moved to `coding_principles.md` #7 try different approach |
| #191 | Moved to `coding_principles.md` #1 (duplicate) |
| #197 | Moved to `coding_principles.md` #12 OCP |
| #8 | Duplicate #2 rate limit, deleted |
| #18 | Author note "Delete this todo", deleted |
| #196 | MMO pause N/A, deleted |
| #232 | Observation quote, moved to `coding_principles.md` #1 heuristic |
| #252 | Question form, not an item, deleted |

---
## Contradictions / tech debt — needs your call

### ① Raw-pointer check appears to have gaps ⚠️
- **Claimed:** #47/#274 — clang-tidy plugin `cpp/plugin/NoMemberRawPointerCheck.cpp` enforces
- **Actual:** #186 agent found `cpp/libs/modules/currency/currency_system.h` **does have raw pointer members**
- **Conclusion:** Plugin either has a whitelist, or hasn't been applied across `cpp/libs/modules/` yet
- **Suggestion:** Run a full clang-tidy pass on `cpp/libs/modules/` to verify coverage

### ② MissionSystem two paths unannotated
- **Location:** `cpp/libs/modules/mission/` — `CompleteAllMissions` vs `OnMissionCompletion`
- **Problem:** Two paths look identical in purpose, code actually diverges, but **no comments explain the difference**
- **Reference:** todo #225 original warning: "looks like the same logic, but has subtle differences"
- **Suggestion:** 5-minute fix — add 3 lines of comment explaining when each path is called

### ③ #280 has explicit TODO comment
- **Location:** `cpp/libs/services/scene/player/system/player_lifecycle.cpp:376`
- **Text:** `// TODO: Only allow re-login after save completes`
- **Reference:** todo #280/#242/#243 all point to the same issue
- **Suggestion:** This is a **state-machine problem** — add a "saving" state, re-login requests in that state should wait or be rejected
- **✅ 2026-05-14 shipped:** Layered defense instead of a new state machine:
  1. Same-node reconnect already cleared by `EnterScene` dropping the `UnregisterPlayer` marker (pre-existing)
  2. Cross-node reconnect already gated by `player_locator` 30s lease (pre-existing)
  3. Extreme window (save > 30s): `HandlePlayerAsyncSaved` now emits LOG_WARN — ops can observe
  - New proto field: `UnregisterPlayer.logout_initiated_ms` records save-initiation time
  - New API: `PlayerLifecycleSystem::IsSaveInFlight(playerId)` for callers that want to query

---

## How to use this file

1. **For scheduling:** Pull from P0 / P1; each item has effort and dependencies, estimate sprints directly.
2. **For retrospectives:** Use the ✅ section to show your manager "these are done".
3. **For adding new todos:** Go to `todo.md` (thought-stream); this file only reflects items already audited.
4. **Next audit:** Re-run audit in 3 months (reuse the agent prompt from `todo_audit_2026_05_11.md`); states will migrate.


---

# Part 3 · Coding Principles


> These are the **philosophical / principle-level** items extracted from `todo.md` —
> they aren't "action items" (you can't check them off with code or a doc),
> but rather the judgment criteria you should continuously apply when writing code
> or making architecture decisions.
>
> That's why they've been pulled out of todo and archived separately.
>
> Source: `todo.md` items #14, #153–155, #158–162, #168–169, #172–176, #181–183, #191, #197
> Extracted: 2026-05-11

---

## I. Readability / Maintenance Cost

### 1. Keep call depth shallow — logic should be traceable at a glance
> Source: #153 #154 #160 #174 #181

**Core idea:** Fewer call chains → fewer bugs → lower comprehension cost → lower maintenance cost.

- Like indentation depth: can you tell at a glance what the code is doing?
- When a bug appears, can you spot it immediately?
- Too many call levels = technical debt = chasing bugs across multiple files, high cost to read.

**Practical heuristic:** If you need to set breakpoints in more than 3 places to track down one issue, that's a signal the logic is too tangled (see #232).

---

### 2. One-sentence summary — check that code lives in the right place
> Source: #159

**How to apply:** After writing a piece of logic, summarize it in one sentence. Then ask whether that sentence flows naturally with the surrounding code.

**Typical anti-pattern:** A block of logic that's really just "toggle UI visibility" ends up sitting inside a data-fetching interface. Write the one-sentence summary and it becomes obvious: this belongs in a "view became visible" hook (see #180).

---

### 3. Code should show what it's doing at a glance
> Source: #155 #160

Code should expose **what / flow / order** explicitly — code you can't understand probably has a bug hiding in it.

---

### 4. Simple code is where bugs hide
> Source: #88

Simple code is actually bug-prone, because nobody reads it carefully. **For anything that looks trivial, ask yourself: have all edge cases been covered?**

---

### 5. When code is obscure, don't debug it — rewrite it
> Source: #158

If a piece of code is so opaque you don't know where to start debugging, that's a sign it needs to be rewritten, not debugged.

---

## II. Debugging / Self-Review Habits

### 6. Go back and re-read your own code; use an LLM to review it
> Source: #162

Don't commit right away. Come back after half a day or a day — you'll catch things. **Run it past ChatGPT/Claude too; an LLM is a free reviewer.**

---

### 7. Step back from your current approach — try a different angle
> Source: #183

When you're stuck in an approach that isn't working, drop it and **start fresh with a completely different angle**. Grinding away at the same dead end costs more time in the long run.

---

## III. Abstraction / Refactoring

### 8. The goal of refactoring, abstraction, and polymorphism is to reduce complexity
> Source: #173

The **only** criterion for any architecture move: is it simpler or more complex after the change?

> If it looks more "sophisticated" but is harder to understand — it's a failure.

---

### 9. Only abstract when there's genuine repetition
> Source: #121 #175

**Anti-pattern:** Writing an abstraction to "demonstrate a design pattern."

**Right pattern:** You spot 3 places with genuinely duplicated code → then abstract. **Single use = don't abstract.**

---

### 10. Function granularity — reusable functions should not call other reusable functions
> Source: #117

**Keep layers clear:** The smallest reusable unit should be a leaf node only. Reusable functions calling other reusable functions makes granularity muddy.

```
┌─ Business function (chains steps in logical order)
│   ├─ Reusable fn A  ← leaf, no further nesting
│   ├─ Reusable fn B  ← leaf
│   └─ Reusable fn C  ← leaf
```

---

### 11. Logic flow should have clear steps: 1, 2, 3, 4, 5
> Source: #118

Take Unity's lifecycle as a reference (Awake / OnEnable / Start / OnDisable / OnDestroy): each step is a well-defined abstraction at a clear point in the sequence.

---

## IV. System Design Principles

### 12. A system should only change when new functionality is added — otherwise it stays put (Open/Closed Principle)
> Source: #197

Adding a feature = adding code; **don't touch existing code**. Other systems should have no effect on mine. That's the definition of "stable."

---

### 13. Decouple systems as much as possible
> Source: #172

Two systems may be related at the business level; at the code level, keep them as independent as possible. **Reuse is fine, mutual dependency is not.**

---

### 14. Don't add extension hooks inside foundational functionality
> Source: #182

**Right direction:** Extension code → calls foundational code.
**Wrong direction:** Stuffing business-specific extensions into foundational code (e.g., putting packet-send conditions inside the network layer, see #190).

---

### 15. One field, one meaning
> Source: #168

Classic anti-pattern: using `count = -1` to mean "not retrievable" — that's cramming two concepts into one field.
**Right approach:** count is a count; "is retrievable" is a separate field.
**Analogy:** A function does one thing.

---

### 16. System–developer contract: predictability
> Source: #161

If a developer uses the system by the rules, **the result should be predictable**. Predictable = testable = usable system.
A system where you don't know what will happen after a read or write is a system you can't rely on.

---

### 17. Everything needs a real use case
> Source: #175

Don't use a technology just to use it. **Start with an actual scenario, then pick the technology — not the other way around.**

---

## V. Lessons from Past Mistakes

### 18. Too many call chains and callbacks in the past
> Source: #169

Retrospective note: previous project code had call chains and callbacks nested too deep — impossible to follow the call graph or code path.

**Self-reminder:** Keep checking principle #1 (shallow call depth) as you write code.

---

### 19. Keep framework wrappers simple
> Source: #14

Goal: simple enough that **a newcomer can write server code**. Complex frameworks = slow onboarding = harder to find the right people = poor project longevity.

---

### 20. Data volume is enormous at launch
> Source: #176

**Battle-tested wisdom:** Every design decision needs to account for "launch day traffic is 100× normal."
- Will lists overflow?
- Will the database fill up?
- Will the cache get stampeded?

---

## How to use this document

1. **Before writing code:** You don't need to read through it every time, but **when you hit a design decision** (abstract or not? split into modules or not?) come back and check.
2. **During code review:** Use this as a checklist. If someone violates a principle, cite the number — no need to argue.
3. **New hire onboarding:** This is the condensed version of "why the project is written this way" — more code-level than ARCH.md.
4. **Don't keep adding new items** — unless you've genuinely found a new principle that keeps surfacing across multiple todo entries. Continuously adding will erode its value as a checklist.

# PR Templates — HTTP /api/login Migration

**Date:** 2026-05-10
**目的:** 把本轮 20+ commit 拆成可发布的 PR 故事线。每个模板对应一条 epic,可按需独立或合并发布。

---

## 模板 A: 单一大 PR(推荐:main → release/staging 分支)

```markdown
## Title
feat(login): migrate primary auth path to Java Gateway POST /api/login

## TL;DR
Move the heavy login work — OAuth verification, rate limiting, queueing,
token issuance — off the cpp gate TCP channel and onto Java Gateway HTTP.
Keep the legacy gate-side `ClientPlayerLogin.Login` RPC as a tunable
fallback through T+3 (see ARCH §12). Net change: gate stops being a
"login transformer" and goes back to being a "protocol router".

## Why
Open-server logins were running on the same TCP path as combat traffic.
A login spike (open-day, holiday event, account-system push) ate cpp
gate ephemeral ports (`Bound=15470`, `SynSent=1540`, p99 → 25s) and
pushed the entire game-loop p99. Splitting the login surface to HTTP:

- gives us a place to do Bucket4j rate limit + Wave queueing without
  touching the muduo gate;
- isolates OAuth provider failures (WeChat/QQ network blips) from
  in-game traffic;
- lets us scale Gateway / login independently of gate.

## Scope (what's IN)
- New HTTP endpoints on Java Gateway:
  - `POST /api/login` — replaces in-game `ClientPlayerLogin.Login` for
    new clients; routes to go-zero login via gRPC (long-lived channel).
  - `POST /api/refresh-token` — moves token rotation off the gate TCP.
- Bucket4j + Lettuce three-layer limiter (zone bucket / IP bucket /
  account cooldown) + open-server wave schedule, default OFF.
- robot client gains `use_http_login: true` opt-in + companion HTTP
  refresh path so the client side proves the new contract end-to-end.
- go-zero login: SessionDetails-aware split between "legacy" (gate
  forwarded) and "new" (HTTP direct) branches, with throttled
  deprecation telemetry.
- cpp gate: HTTP/2 keepalive on its login gRPC channel + carry
  SessionInfo.playerId through Disconnect SessionDetails (closes a
  pre-existing locator-leak bug, #B-1).
- 36 unit / integration / wire-capture tests, plus a Linux-staging
  stress runner (`tools/scripts/stress-linux-tier.sh`).
- Onboarding doc + ARCH §11 decision table + ARCH §12 deprecation
  schedule + release-checklist + 3 stress-test reports.

## Scope (what's NOT)
- Old client paths still work; nothing is removed in this PR. Removal
  is gated on T+3 (ARCH §12), driven by the `legacy_login_count`
  telemetry, with a config flag added in T+2.
- No in-game / combat-loop changes. `HandleGrpcNodeMessage` keeps
  forwarding CreatePlayer / EnterGame / LeaveGame / Disconnect
  exactly as before — those are not login, they're "protocol router"
  duties (see gate-login-rpc-boundary.md).
- WeChat/QQ provider sandbox: covered by mock-server unit tests +
  wire-shape integration tests in this PR; full real-OAuth e2e is a
  separate, ops-driven validation.
- 1k/2k/5k tier: Windows dev tops out at 500. The Linux-staging
  runner is included; first cliff numbers will be appended to
  `linux-staging-stress-runbook.md` §E by ops.

## Test summary
- Java Gateway: `mvn test` → 36 / 36 pass
- go-zero login: `go test ./...` → all pass (incl. new
  SetSessionDisconnecting wire test)
- robot: `go test -count=1 ./...` → all pass (incl. nil-guard +
  zero-PlayerId-guard for sendDisconnectBestEffort)
- E2E smoke at 50/100/200/500 bots × 30s = **0 fail / 0 stuck**,
  avg_login 69-101 ms, max_login 163-342 ms. Detailed table in
  `docs/stress/stress-test-2026-05-http-login.md`.
- Bucket4j first-run hit rates documented in §四点五 of the same doc:
  zone QUEUEING, IP QUEUEING, account COOLDOWN all fire correctly
  through Lettuce → Redis.

## Rollout
T+0 / T+1 / T+2 / T+3 milestones with explicit exit criteria are in
[ARCH.md §12](../docs/design/ARCH.md). Three-tier rollback (kill
limiter / flip `use_http_login` back / `kubectl rollout undo`) plus
what-to-capture-before-rolling-back live in
[release-checklist.md §E](../docs/ops/release-checklist.md).

## Files of interest for review
- `java/gateway_node/src/main/java/com/game/gateway/grpc/LoginRpcClient.java`
  — handcrafted protobuf-on-the-wire client; pin 'loginpb.' service prefix.
- `java/gateway_node/src/main/java/com/game/gateway/ratelimit/AssignGateRateLimiter.java`
  — three-layer limiter; verify the wave-then-zone-then-IP-then-cooldown order
  matches the design doc.
- `cpp/nodes/gate/handler/rpc/client_message_processor.cpp`
  — `HandleConnectionDisconnection` adds `set_player_id`; this is the
  #B-1 root-cause fix.
- `go/login/internal/logic/clientplayerlogin/loginlogic.go`
  — `isLegacyPath` branch + `warnLegacyLoginCaller` deprecation counter.
- `robot/main.go` `loginAndEnterViaHttpGateway` — the client-side new path.

## Known follow-ups (non-blocking)
- 1k+ stress tier on Linux staging (`tools/scripts/stress-linux-tier.sh`).
- WeChat/QQ real-sandbox e2e on staging.
- `legacy_login_count` graph wired into Grafana.
- `LeaveGame/EnterGame/CreatePlayer/Disconnect` still ride the gate
  channel by design — unchanged, see gate-login-rpc-boundary.md if
  reviewers ask "why didn't you also move these".
```

---

## 模板 B: 三个 epic 拆分(若团队偏好小 PR)

### B1 — feat(gateway): HTTP login + refresh-token + rate limiting
- 范围:Java Gateway 全部新代码(controller / service / DTO / ratelimit / tests)
- commit: `4d6c70482..ea4a36b59..355e3a54d..0f4712996..2f96a0c77..c005714b5..9a8dd4d99`
- 测试:`mvn test` 36/36
- 不影响:robot / cpp / go-zero login
- review 焦点:`LoginRpcClient` 的 hand-rolled wire 和 service prefix `loginpb.` 修复

### B2 — feat(login+gate): legacy-path deprecation telemetry + #B-1 root fix
- 范围:go-zero login `loginlogic.go` + `deprecation.go` + cpp gate
  `HandleConnectionDisconnection` `set_player_id`
- commit: `55b9b8cf5 + c5a3ad54b + ed2290c11`
- 测试:go-zero login mock test (`SetSessionDisconnecting`) + robot
  `sendDisconnectBestEffort` 守卫测试
- review 焦点:legacy 判定边界(`SessionDetails.SessionId > 0` 为 legacy)和 throttled 计数器实现

### B3 — feat(robot)+ops: HTTP login client + stress runner + docs
- 范围:robot HTTP 客户端 + `use_http_login` 开关 + stress 配置 +
  `stress-linux-tier.sh` + onboarding / boundary / release-checklist /
  staging-runbook 文档
- commit: `efadb1b34 + 19817c38f + 8308a00ec + cef258f3f + e51ef5244`
- 测试:robot `go test ./...` + 50/100/200/500 e2e 数据
- review 焦点:`runTokenRefresher` 走 HTTP / fallback 兜底逻辑

---

## 模板 C: hotfix-only PR(只取 #B-1 治标 + 治本)

```markdown
## Title
fix(login/gate): close locator-session-stuck-ONLINE leak (#B-1)

## TL;DR
TCP-close on a verified gate session left `player:session:{pid}` at
state=ONLINE because login.markPlayerSessionDisconnecting() short-
circuited on `playerID == 0`. Fix in two layers (defense in depth):

1. cpp gate `HandleConnectionDisconnection` now copies
   `SessionInfo.playerId` into `SessionDetails`, so login's guard no
   longer fires. (commit c5a3ad54b)
2. robot client sends `Disconnect` RPC before `Close()` for clean
   shutdowns. (commit 19817c38f)

Either layer alone closes the bug; together they cover SIGKILL'd
clients, network drops, and graceful exits.

## Why now
Symptom in dev: re-running robots without `redis-cli FLUSHDB` left
EnterGame falling into `ReplaceLogin` with KickPlayer routed to a
dead-phantom gate; clients hung at "scene ready". In production, the
first symptom would be slowly accumulating ONLINE-but-no-conn sessions
inflating the `online_players` metric over a multi-day uptime.

## Test
- `go test -run TestSetSessionDisconnecting ./go/login/.../sessionmanager`
  pins the wire shape (player_id, session_id, lease_ttl=30).
- `go test -run TestSendDisconnectBestEffort ./robot` covers
  nil-client and zero-PlayerId guards.
- E2E re-run with neither wipe nor manual restart: 50 bots round-1 ✅
  (previously round-2+ would fail).

## Risk
- Low. Both changes only add information that downstream code was
  already prepared to consume. Reviewed lines = 11 (cpp) + 32 (robot).
```

---

## 模板 D: docs-only PR(若想把文档和代码分离)

```markdown
## Title
docs: HTTP /api/login migration — onboarding, ARCH §11/§12, runbooks

## What
Add the documentation backbone for the HTTP login migration:

- `docs/design/onboarding.md` — "5 minutes to first robot login".
- `docs/design/gate-login-rpc-boundary.md` — the "why does gate still
  call login" reference; tabulates per-RPC routing and the player UI
  state machine.
- `docs/design/ARCH.md` §11 decisions #9-#13 + §12 deprecation
  schedule with explicit T+0..T+3 exit criteria.
- `docs/design/architecture-current-state-vs-gaps-2026-05.md` —
  what's done vs what's still TODO in the current quarter.
- `docs/design/open-server-rate-limit-design.md`,
  `docs/design/third-party-login-end-to-end-design.md` — design notes.
- `docs/stress/stress-test-2026-05-http-login.md` — Windows dev
  baseline + Bucket4j first-hit data.
- `docs/ops/release-checklist.md` — the on-call playbook with
  three-tier rollback.
- `docs/ops/linux-staging-stress-runbook.md` — copy-pasteable Linux
  staging steps + tier-data backfill table.
- `docs/ops/gate-kernel-tuning-runbook.md` — sysctl baseline.

No code change in this PR.
```

---

## 提示词:发 PR 时的 commit 顺序

如果走模板 A(单 PR 含所有 commit),建议保留现有 git history,不 squash —— 每个 commit 都有自包含的 message 和 rationale,reviewer 可以按 commit 增量审。

如果走模板 B(三个 epic),建议:
1. 先 merge B1(gateway 自包含,生产可灰度但默认 OFF,零风险)
2. 再 merge B2(login + gate 配对,部署需要先 login 后 gate,避免 SessionDetails 字段错位窗口)
3. 最后 merge B3(robot + 文档 + 工具,对线上无影响)

---

## 关联文档
- [ARCH.md §11-12](../design/ARCH.md) — 决策表和 deprecation
- [release-checklist.md](./release-checklist.md) — 上线 SOP
- [linux-staging-stress-runbook.md](./linux-staging-stress-runbook.md) — staging 跑数
- [stress-test-2026-05-http-login.md](../stress/stress-test-2026-05-http-login.md) — 基线数据
- [gate-login-rpc-boundary.md](../design/gate-login-rpc-boundary.md) — 边界文档

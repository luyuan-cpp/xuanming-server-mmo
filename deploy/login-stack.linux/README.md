# Linux-native login stack (S-2 / backlog #221)

Brings up the **login chain** (sandbox_mock + go-zero login + Java Gateway)
on Linux containers, on top of the existing infra stack (redis / etcd / kafka /
mysql in `../docker-compose.yml`).

Use this when you want the Linux-side performance characteristics of #221
(no Hyper-V port reservations, no Windows fork() overhead, kernel TCP stack
that responds to sysctl tuning) but don't have a real staging cluster yet.
The same Dockerfiles ship to k8s in `../k8s/` unchanged once staging exists.

## What this stack contains

| service          | image                      | host port | notes                                                            |
|------------------|----------------------------|-----------|------------------------------------------------------------------|
| `sandbox-mock`   | `mmorpg-sandbox-mock:dev`  | 18090     | byte-for-byte mock of WeChat / QQ OAuth APIs (see `cmd/sandbox_mock/`) |
| `login`          | `mmorpg-login:dev`         | 51000, 9101 | go-zero login.rpc + Prometheus scrape on 9101                  |
| `gateway`        | `mmorpg-gateway:dev`       | 8081      | Spring Boot Gateway, reaches `login` over container DNS         |

## What this stack does NOT contain

- **cpp gate / scene.** Out of scope for #221's HTTP-login measurement —
  those bind muduo/openssl/etc and need their own image story. Coming in
  a follow-up. The new HTTP login chain hits gateway → login → redis/etcd
  without touching cpp gate, so the 1k/2k/5k tier is meaningful on its own.
- **redis / etcd / kafka / mysql.** Those live in the existing
  `../docker-compose.yml`. Bring that up first.

## Prerequisites

1. Docker Desktop (Linux containers).
2. The shared infra stack already up:
   ```
   docker compose -f deploy/docker-compose.yml up -d redis etcd mysql kafka
   ```
3. Free host ports 8081 / 18090 / 51000 / 9101.
4. Export a local-only robot login secret without echo or shell-history exposure;
   Compose refuses to start when it is absent:
   ```bash
   read -rsp 'Local robot login secret: ' LOGIN_DEV_PASSWORD_SHARED_SECRET
   echo
   export LOGIN_DEV_PASSWORD_SHARED_SECRET
   ```

## Up

```
# from repo root:
docker compose -f deploy/docker-compose.login-stack.yml up -d --build
```

First build takes ~3-5min (Go toolchain + Java/Maven). Subsequent runs cache
all three layers.

## Smoke

```
# 1) verify gateway up + reaches login + guarded dev auth loaded.
# jq reads the secret from its environment; the plaintext is not placed in argv.
jq -nc '{zone_id:1,account:"robot_linux_smoke",password:env.LOGIN_DEV_PASSWORD_SHARED_SECRET}' | \
  curl -s -X POST http://localhost:8081/api/login \
    -H 'Content-Type: application/json' --data-binary @- | jq .

# expected: {"code":0,"players":[],"access_token":"…","refresh_token":"…",…}
```

```
# 2) verify sandbox-mock is reachable from login (offline OAuth)
curl -s -X POST http://localhost:8081/api/login \
  -H 'Content-Type: application/json' \
  -d '{"zone_id":1,"auth_type":"wechat","auth_token":"Usmoke_code"}' | jq .

# expected: code:0; the account derived from the U-prefix code will start
# with "wx_unionid_" because sandbox-mock returns a deterministic unionid.
```

```
# 3) verify Prometheus scrape (T)
curl -s http://localhost:9101/metrics | grep login_auth_path_total

# expected: lines like
#   login_auth_path_total{path="new",auth_type="password"} 1
#   login_auth_path_total{path="new",auth_type="wechat"}   1
```

## Stress

The host-side `tools/scripts/gateway-mock-stress.sh` works unchanged
because gateway is published on the same `:8081` it always was:

```
PER_TIER_SEC=10 tools/scripts/gateway-mock-stress.sh 500 1000 2000
```

A run on Docker Desktop / Linux containers on the same Windows box still
exposes more throughput than the all-Windows path (kernel TCP stack +
real fork()), but the numbers won't match a bare-metal Linux staging
cluster. Use this for relative comparisons and contract-level smoke,
not for cliff measurement.

## Bucket4j ON

By default the limiter is OFF (so smoke runs don't hit cooldown surprises).
Flip it for the limiter-side smoke:

```
docker compose -f deploy/docker-compose.login-stack.yml stop gateway
GATE_RATE_LIMIT_ENABLED=true \
GATE_RATE_LIMIT_ZONE_DEFAULT_RPS=2 \
GATE_RATE_LIMIT_ZONE_DEFAULT_BURST=4 \
  docker compose -f deploy/docker-compose.login-stack.yml up -d gateway

# then burst 10 reqs and confirm 4 pass + 6 QUEUEING (code:100)
for i in $(seq 1 10); do
  jq -nc --arg account "robot_limit_$i" \
    '{zone_id:1,account:$account,password:env.LOGIN_DEV_PASSWORD_SHARED_SECRET}' | \
    curl -s -X POST http://localhost:8081/api/login \
      -H 'Content-Type: application/json' --data-binary @-
  echo
done | grep -oE '"code":[0-9]*' | sort | uniq -c
```

## Switching sandbox-mock → real WeChat / QQ

In `login-stack.linux/login.yaml`, delete the two `Endpoint:` lines under
`AuthProviders.WeChat` / `AuthProviders.QQ`, swap in real AppId/AppSecret,
and `docker compose up -d login --force-recreate`. The gateway and sandbox-mock
containers don't need to restart.

## Tear down

```
docker compose -f deploy/docker-compose.login-stack.yml down
# (does not touch the infra stack — that lives in ../docker-compose.yml)
```

## Troubleshooting

| symptom | likely cause |
|---|---|
| `login` exits with "rpc dial: etcd://… context deadline exceeded" | infra stack not up, or `host.docker.internal` not resolving on Linux. The compose adds `host-gateway` mapping, but on rootless Docker you may need `INFRA_HOST=172.17.0.1`. |
| `gateway` exits with "EOFException: connection unexpectedly lost" on JPA bootstrap | MySQL container started but not yet healthy. `docker compose -f deploy/docker-compose.yml ps mysql` should show `(healthy)`. |
| WeChat smoke returns `errcode 40029 invalid code` | you used the literal string `EXPIRED` as the code; sandbox-mock is doing the right thing. Use any other code. |
| `sandbox-mock` container starts but `login` still calls `api.weixin.qq.com` | login.yaml override didn't get into the container. Confirm with `docker compose exec login cat /app/etc/login.yaml \| grep Endpoint`. |

## How this relates to the other stress tooling

| script                                | what it measures                                              |
|---------------------------------------|---------------------------------------------------------------|
| `gateway-mock-stress.sh` (host)       | Gateway + login throughput, no cpp gate, no real client SDK   |
| `stress-linux-tier.sh` (Linux box)    | full stack at 1k/2k/5k bots — needs real cpp gate available   |
| this compose stack                    | Linux-side of the chain, host-driven; runs anywhere Docker runs |

For the **full-stack** 1k/2k/5k tier (#221 / S) you still need a Linux staging
box with the cpp gate binary deployed. This compose stack closes 80% of the
gap by Linux-izing everything *except* cpp gate.

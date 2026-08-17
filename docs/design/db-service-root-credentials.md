# DB Service: Root Credentials for MySQL

**Date:** 2026-04-12

> **修订(2026-08-15)**: TiDB 化后本决策需重议(用户/权限模型、自动建库策略、凭证改环境变量注入——login 已有 `DSNEnv` 先例),见 [global-data-layer-tidb-decision.md](./global-data-layer-tidb-decision.md) §D9。

## Decision
The `db` (Go) service connects to MySQL as `root`, not a restricted `appuser`.

## Rationale
- db service is the **only** MySQL accessor in the architecture. All other services go through Kafka/gRPC → db service.
- db service needs `CREATE DATABASE` privilege to auto-provision zone databases (`zone_{N}_db`) on first startup.
- Using `appuser` caused Access Denied (error 1044) because `appuser` lacked global `CREATE` privilege.
- A separate admin connection added complexity for zero security benefit (db service is internal-only).

## Password Convention
- All environments use the **same root password format** — keep `db.yaml`, `docker-compose.yml`, and `k8s/mysql.yaml` in sync.
- Local Docker Compose: `deploy/docker-compose.yml` → `MYSQL_ROOT_PASSWORD`
- K8s: `deploy/k8s/manifests/infra/mysql.yaml` → `MYSQL_ROOT_PASSWORD`
- db service config: `go/db/etc/db.yaml` → `Database.Passwd`

## Zone Bootstrap Flow
1. Ops deploys MySQL (set root password)
2. Start db service with desired `ZoneId`
3. db service auto-creates `zone_{N}_db` via `CreateDatabase()` — no manual SQL needed

## appuser Retained
`docker-compose.yml` still creates `appuser` via `MYSQL_USER` — retained for other services that may use it. Not used by db service.

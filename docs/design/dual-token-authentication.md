# Dual Token Authentication Design (Access + Refresh)

**Date:** 2026-04-20
**Status:** Implemented

## Problem

1. Gate connection token (HMAC) TTL was 60s in Java Gateway, causing `token expired` errors when client connects slightly late.
2. After login via third-party (WeChat/QQ/SA-Token), there's no persistent auth credential. If the player disconnects, they must redo the full OAuth flow.
3. No "remember me" capability -- typical WeChat/QQ games allow silent re-login for 30 days.

## Architecture

### Three-Layer Token System

```
Layer 1: Third-Party Auth (WeChat/QQ/SA-Token/Password)
  |
  | (first login only, validates once)
  v
Layer 2: Access Token (2h) + Refresh Token (30d)   <-- NEW
  |
  | (issued after successful auth, stored in Redis)
  v
Layer 3: Gate Token (HMAC, 300s)
  |
  | (one-time connection handshake, unchanged)
  v
  TCP Session (Gate)
```

### Token Comparison

| Token | Type | TTL | Purpose | Storage |
|-------|------|-----|---------|---------|
| Third-party token | OAuth code / SA-Token | Varies | Initial auth | Third-party Redis |
| **Access Token** | Opaque (base64url, 256-bit) | **2 hours** | Reconnect without re-auth | Redis: `access_token:{value}` |
| **Refresh Token** | Opaque (base64url, 256-bit) | **30 days** | Silent access token renewal | Redis: `refresh_token:{value}` |
| Gate Token | HMAC-SHA256 signed protobuf | 300 seconds | One-time gate TCP handshake | Not stored (verified by signature) |

## Flow Diagrams

### First Login (WeChat Example)

```
Client                  Java Gateway           Go Login            Redis
  |                         |                     |                  |
  |-- POST /assign-gate --->|                     |                  |
  |<-- gate_ip + HMAC token |                     |                  |
  |                         |                     |                  |
  |== TCP connect to Gate ==========================|                |
  |-- ClientTokenVerify --->|                     |                  |
  |<-- verified             |                     |                  |
  |                         |                     |                  |
  |-- Login(auth_type:"wechat", auth_token:code) -->|               |
  |                         |      WeChat OAuth API validates code   |
  |                         |                     |-- SET access_token:xxx ->|
  |                         |                     |-- SET refresh_token:yyy ->|
  |<-- LoginResponse{players, access_token, refresh_token} ---------|
```

### Reconnect (Within 2h)

```
Client                         Go Login            Redis
  |                               |                  |
  |-- Login(auth_type:"access_token", auth_token:xxx) -->|
  |                               |-- GET access_token:xxx ->|
  |                               |<-- {account, auth_type}   |
  |<-- LoginResponse{players} ----|                  |
```

### Token Refresh (Access Expired, Refresh Valid)

```
Client                         Go Login            Redis
  |                               |                  |
  |-- RefreshToken(refresh_token:yyy) ->|            |
  |                               |-- GET refresh_token:yyy ->|
  |                               |<-- {account, auth_type}   |
  |                               |-- DEL refresh_token:yyy -->|  (one-time use)
  |                               |-- SET access_token:xxx2 -->|
  |                               |-- SET refresh_token:yyy2 ->|
  |<-- RefreshTokenResponse{new_access, new_refresh} |
  |                               |                  |
  |-- Login(auth_type:"access_token", auth_token:xxx2) -->|
```

### Full Re-Login (Both Tokens Expired, After 30 Days)

```
Client must redo WeChat/QQ OAuth flow from scratch.
```

## Redis Key Schema

| Key Pattern | Value | TTL | Purpose |
|-------------|-------|-----|---------|
| `access_token:{token}` | JSON `{account, auth_type, device_id, created_at}` | 2h | Access token lookup |
| `refresh_token:{token}` | JSON `{account, auth_type, device_id, created_at}` | 30d | Refresh token lookup |
| `account_refresh:{account}` | SET of refresh token strings | 30d | Bulk revocation (password change) |

## Security Properties

1. **Token rotation**: Refresh token is one-time-use. Each refresh issues a new pair and invalidates the old refresh token.
2. **Race protection**: `DEL` is atomic -- if two requests try to use the same refresh token, only one succeeds.
3. **Bulk revocation**: `RevokeAll(account)` deletes all refresh tokens for an account (e.g. on password change).
4. **Opaque tokens**: 256-bit crypto/rand, base64url encoded. Not guessable, not decodable.
5. **Gate token unchanged**: HMAC-SHA256 gate handshake is orthogonal to auth tokens.

## Configuration

### Go Login (`go/login/etc/login.yaml`)

```yaml
TokenConfig:
  AccessTokenTTL: 2h      # Access token lifetime
  RefreshTokenTTL: 720h   # Refresh token lifetime (30 days)
```

### Java Gateway (`application.yaml`)

```yaml
gate:
  token-ttl-seconds: 300   # Gate handshake token (was 60, now matches Go Login)
```

## Files Changed

| File | Change |
|------|--------|
| `proto/login/login.proto` | Added `access_token`, `refresh_token` fields to `LoginResponse`; added `RefreshTokenRequest/Response` messages and `RefreshToken` RPC |
| `go/login/internal/logic/pkg/token/token.go` | **NEW** - Token manager: Issue, ValidateAccess, Refresh, RevokeAll |
| `go/login/internal/logic/pkg/auth/providers.go` | Added `AccessTokenProvider` for `auth_type:"access_token"` |
| `go/login/internal/logic/clientplayerlogin/loginlogic.go` | Issue tokens on successful login (skip for access_token re-auth) |
| `go/login/internal/logic/clientplayerlogin/refreshtokenlogic.go` | **NEW** - RefreshToken RPC handler |
| `go/login/internal/server/clientplayerlogin/clientplayerloginserver.go` | Added `RefreshToken` method |
| `go/login/internal/config/config.go` | Added `TokenConf` struct |
| `go/login/internal/svc/servicecontext.go` | Added `TokenManager` to ServiceContext |
| `go/login/internal/svc/auth_init.go` | Register `access_token` provider |
| `go/login/etc/login.yaml` | Added `TokenConfig` section |
| `java/gateway_node/.../application.yaml` | `token-ttl-seconds: 60` → `300` |
| `robot/pkg/client.go` | Added `AccessToken`, `RefreshToken` fields |
| `robot/login.go` | Store received tokens for reconnect |

## Industry Reference (WeChat/QQ Games)

| Platform | Access Token TTL | Refresh Token TTL | Notes |
|----------|-----------------|-------------------|-------|
| WeChat Mini Game | 2h (session_key) | 30d (via wx.login) | session_key expires, wx.login silently renews |
| QQ Connect | 2h | 30d | Standard OAuth2 |
| Apple Game Center | 48h | 60d | Longer due to platform constraints |
| Google Play Games | 1h | Indefinite (revocable) | Uses Google OAuth2 |
| Our implementation | **2h** | **30d** | Matches WeChat/QQ standard |


## 2026-10-23：Go 与 Java 的 refresh family 契约

Go `token.Manager` 保持 `Issue / ValidateAccess / Refresh / RevokeAll` 四个原接口。新认证生成独立 family，ID 为 16 个加密随机字节的规范 base64url（22 字符），不得以账号、设备或常量充当 family。Go 的 access/refresh JSON 新增 `family_id`，另有 `refresh_family:{id}` HASH，保存 `account / head / created_at / rotated_at`。family 的 TTL 与当前 head refresh 相同，正常轮换保持 family，更新 head 并续 TTL；不能通过读取 access 延长有效期。

签发以单段 Lua 同时提交两枚 token、family 与账号 ZSET。刷新先读旧 JSON，提交时原子核对其原始字节、账号索引成员、family 账号及当前 head，再同时消费旧 refresh、签发新 pair、推进 head 和 TTL。账号索引不缩短尚存的更长 TTL。Go 保持严格一次性：并发刷新只有一个提交者，已消费的 refresh 不能重放；提交成功但回包丢失仍须按现有客户端契约重新认证，不在 Go 引入 Java 的宽限重放。

含 family 的 access 必须核 family 存在且账号一致，并确认当前 head 仍属于该 family、账号及索引。正常轮换前签发且未过期的同 family access 继续有效；family 被撤销或到期后全部拒绝。缺失、跨账号、错误类型、非法 ID、损坏 head 或原始值被替换均 fail-closed。

旧 JSON **缺少** family 字段时，access 仍按原 TTL 兼容；旧 refresh 在下一次成功的原值 CAS 内建立随机 family。显式空串、null 或非法 family 不视为旧 JSON，不以兼容构造掩盖缺失实现。旧无 family access 没有可撤销关联，因此不能宣称它已被新 family 的撤销追溯失效；它只会按原 TTL 到期。

账号撤销与容量淘汰均在单段 Lua 中核验完整候选后提交。Redis Lua 的运行时错误不会回滚先前写入，故所有会影响后续写入的键类型及必要归属核验必须在第一次副作用前完成。只删除目标账号的 refresh；仅当 family 的当前 head 仍等于被淘汰成员时删除该 family，不能用旧候选误删已经轮换的新 head。并发新认证只能在线性化的撤销前或后提交，不能因分步删索引使新 family 失去归属。

两语言对齐的是随机 family、账号归属、head、TTL、撤销和损坏拒绝语义，Redis 键名与内部字段不要求共用。Java 保留其既有受限宽限重放及单调截止行为，Go 保留原严格一次性行为，不因此改变任一客户端的重试合同。

验证覆盖新认证、轮换与旧 access、旧 JSON 收编、family 到期、并发单赢家、跨账号/错误类型/损坏键零部分写入、淘汰旧候选及撤销与新认证交错。`TestTokenFamily_RealRedis` 为具名本机验证入口，显式设置 `LOGIN_TOKEN_REQUIRE_REAL_REDIS=1`、数值 loopback 地址 `LOGIN_TOKEN_TEST_REDIS_ADDR` 与独立 `LOGIN_TOKEN_TEST_REDIS_DB` 后执行；密码仅从 child env 读取。它使用随机账号及精确登记的键，末尾只删这些键，不 FLUSH、不 SCAN、不借原全局数据作为夹具。实现和测试已准备，实际运行结果以本轮 Codex 原件为准。

正 TTL 也是新 family 凭证的不变量。含 family 的 access、当前 head refresh 与 family HASH 被 `PERSIST` 改为永久后均拒绝；刷新和撤销在首个副作用前完成相应检查，不借轮换把损坏凭证重新续期。旧 JSON 缺字段的正常到期兼容规则保持不变。

撤销的显式 `null` 原件回归必须在上述真实 Redis 入口的 `null-retire-preserves-all-candidates` 子项执行，验证损坏候选之后也不能先删正常候选。miniredis v2.35.0 的 `gopher-json` 会把 JSON null 解成 Lua nil；[Redis 所带 CJSON](https://github.com/redis/redis/blob/7.4/deps/lua/src/lua_cjson.c) 则用独立 lightuserdata 表示 null。该用例因此使用真实后端，未跳过断言，也不为替身另造生产 JSON 解析层。空字符串回归保留在 miniredis，永久凭证在两个后端均有回归。

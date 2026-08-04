# Auth Provider Framework

## Overview

Login service supports multiple third-party authentication methods via a strategy pattern.
Each auth method is a `Provider` implementation, registered at startup based on YAML config.
Production password authentication validates Argon2id hashes in authoritative
MySQL and is opt-in/fail-closed. Registration/recovery remain deliberately absent
from the public Login RPC; existing legacy accounts are provisioned once with a
separate no-echo migration command.
Third-party providers (WeChat, QQ, NetEase, SA-Token) are enabled explicitly.
A prefix-restricted shared-secret provider exists only for local robots/dev.

## Proto Interface

```protobuf
// proto/login/login.proto
message LoginRequest {
  string account    = 1; // Account name (used directly for password auth)
  string password   = 2; // Password (for password auth only)
  string auth_type  = 3; // Provider type: "", "password", "wechat", "qq", "netease", "satoken"
  string auth_token = 4; // Third-party code/token (used when auth_type is not "password")
}
```

**Client behavior:**
- Password login: empty `auth_type` and `"password"` are equivalent. Both require
  a configured production MySQL authenticator (or the guarded dev-only provider).
- Third-party login: set `auth_type` + `auth_token`. The `account` field is ignored; the provider resolves the account from the token.

## Go Interface

```go
// go/login/internal/logic/pkg/auth/provider.go

type AuthResult struct {
    Account string
}

type Provider interface {
    Validate(ctx context.Context, token string) (*AuthResult, error)
}

type PasswordAuthenticator interface {
    ValidatePassword(ctx context.Context, account, password string) (*AuthResult, error)
}

// Registry
auth.Register(name string, p Provider)  // Register a provider (panics on duplicates)
auth.Get(name string) Provider          // Lookup by name (returns nil if not found)
auth.RegisterPassword(p PasswordAuthenticator)
auth.GetPassword() PasswordAuthenticator // nil means fail-closed
```

## Built-in Providers

| auth_type   | Provider           | Token source                | Status        |
|-------------|--------------------|-----------------------------|---------------|
| `password`  | Production password authenticator | `account` + `password` | Implemented, default disabled; MySQL Argon2id only |
| `password` (dev only) | `DevelopmentPasswordProvider` | allowed account prefix + env-injected shared secret | Implemented, default disabled |
| `satoken`   | `SaTokenProvider`  | SA-Token Redis key lookup   | Implemented   |
| `wechat`    | `WeChatProvider`   | WeChat OAuth `code`         | Implemented (`/sns/oauth2/access_token`, account = `wx_<unionid|openid>`) |
| `qq`        | `QQProvider`       | QQ Connect `access_token`   | Implemented (`graph.qq.com/oauth2.0/me`, account = `qq_<unionid|openid>`) |
| `netease`   | `NeteaseProvider`  | NetEase auth token          | Stub (TODO)   |

## Login Flow

```
Client -> LoginRequest{auth_type, auth_token}
                |
                v
   resolveAccount(LoginRequest)
       |                        |
  auth_type == ""/"password"    auth_type == "wechat"/"satoken"/...
       |                        |
  passwordAuthenticator  provider = auth.Get(auth_type)
       |                        |
  missing => reject       provider.Validate(ctx, auth_token)
  MySQL lookup + Argon2id verify  |
       |                        |
  return verified account  return verified account
                |
                v
   (continue normal login flow: lock, session, device limit, etc.)
```

## Config (login.yaml)

Password is disabled by default; third-party-only deployments leave `Enabled`
false. Production names an environment variable containing the MySQL DSN. A
missing DSN makes startup fail rather than silently disabling verification:

```yaml
PasswordAuth:
  Enabled: true
  DSNEnv: "LOGIN_PASSWORD_AUTH_DSN"
  MaxOpenConns: 20
  MaxIdleConns: 10
  KDFConcurrency: 2
  KDFWaitTimeout: 500ms
```

`LOGIN_PASSWORD_AUTH_DSN` is injected by the secret manager using a login-service
database principal with `SELECT` on `user_accounts` only, for example
`login_auth:...@tcp(mysql:3306)/game?charset=utf8mb4`. Its value must never be
committed or logged. `PasswordAuth.Enabled=false` disables only password login;
OAuth/access-token providers continue to work.

Local robots may instead opt into the guarded dev provider by naming an
environment variable that contains the shared secret and by listing
non-production account prefixes. The secret value itself must never appear in
YAML, and production and development password providers are mutually exclusive:

```yaml
DevPasswordAuth:
  Enabled: true
  SharedSecretEnv: "LOGIN_DEV_PASSWORD_SHARED_SECRET"
  AllowedAccountPrefixes: ["robot_", "dev_"]
```

The service registers this provider only when go-zero `Mode` is `dev` or `test`.
Enabling it in production mode, or omitting/emptying the environment variable,
makes startup fail. `DevSkipAuth` is removed; retaining it in an old config causes
startup to fail instead of silently bypassing authentication.

Third-party providers are enabled under `AuthProviders`:

```yaml
# All optional. Omit a section to disable that provider.
AuthProviders:
  # SA-Token: validates token via Redis key lookup
  SaToken:
    TokenName: satoken          # Redis key prefix
    LoginType: login            # SA-Token login type
    Redis:
      Host: 127.0.0.1:6379
      Password: ""
      DB: 0

  # WeChat: OAuth code -> openid
  WeChat:
    AppId: "wx1234567890"
    AppSecret: "your-app-secret"

  # QQ: OAuth code -> openid
  QQ:
    AppId: "qq1234567890"
    AppKey: "your-app-key"

  # NetEase: token verification
  NetEase:
    AppKey: "your-app-key"
    AppSecret: "your-app-secret"
```

## File Layout

```
go/login/internal/
├── logic/pkg/auth/
│   ├── provider.go        # Token Provider + PasswordAuthenticator registries
│   └── providers.go       # All provider implementations
├── svc/
│   └── auth_init.go       # InitAuthProviders(): reads config, registers providers
├── config/
│   └── config.go          # AuthConfig, SaTokenAuthConf, WeChatAuthConf, etc.
└── logic/clientplayerlogin/
    └── loginlogic.go      # resolveAccount(): dispatches to provider
```

## How to Add a New Provider

1. **Add config struct** in `config.go`:
   ```go
   type MyPlatformAuthConf struct {
       ApiKey string `json:"ApiKey"`
   }
   ```
   Add field to `AuthConfig`:
   ```go
   MyPlatform *MyPlatformAuthConf `json:"MyPlatform,optional"`
   ```

2. **Implement Provider** in `providers.go`:
   ```go
   type MyPlatformProvider struct {
       ApiKey string
   }

   func (p *MyPlatformProvider) Validate(ctx context.Context, token string) (*AuthResult, error) {
       // Call platform API to verify token, get user ID
       userId, err := myplatform.VerifyToken(ctx, p.ApiKey, token)
       if err != nil {
           return nil, err
       }
       return &AuthResult{Account: userId}, nil
   }
   ```

3. **Register in `auth_init.go`**:
   ```go
   if cfg.MyPlatform != nil {
       auth.Register("myplatform", &auth.MyPlatformProvider{
           ApiKey: cfg.MyPlatform.ApiKey,
       })
       logx.Info("Auth provider registered: myplatform")
   }
   ```

4. **Add config to `login.yaml`**:
   ```yaml
   Auth:
     MyPlatform:
       ApiKey: "your-api-key"
   ```

5. **Client sends**: `LoginRequest{auth_type: "myplatform", auth_token: "token-from-platform"}`

No changes needed to login flow, proto, or other providers.

## Production Password Authority and Cache Boundary

- Authority is MySQL `user_accounts(account, password)`. `password` is an
  Argon2id PHC string (`m=65536,t=3,p=2`, random 16-byte salt, 32-byte result).
- Every password login queries MySQL and verifies with constant-time digest
  comparison. The Redis `account_data:{account}` value is only the player-list
  cache and is read **after** authentication; its existence, absence or expiry
  cannot bypass MySQL verification.
- Unknown accounts execute a dummy Argon2id calculation to reduce account
  enumeration by timing. Empty hashes, legacy plaintext, malformed hashes, and
  hashes below policy are rejected. Login never auto-upgrades them because it
  does not possess a trusted old credential migration contract.
- Argon2id memory is bounded per login process by a semaphore. Each verification
  uses 64 MiB, so the approximate KDF working set is
  `KDFConcurrency * 64 MiB`: default `2` is about 128 MiB. The code hard-rejects
  values above `8` (about 512 MiB), regardless of YAML. Waiting for a slot is
  bounded by the caller context and `KDFWaitTimeout` (default 500 ms, hard maximum
  5 s); cancellation or overload fails closed before any Redis/account/token
  side effect. Unknown-account dummy hashes use the same slots and cannot bypass
  this memory bound. Capacity must be budgeted per replica, then multiplied by
  the maximum simultaneous login replicas when estimating node memory.
- The canonical account returned to the rest of login comes from the MySQL row,
  not from client-controlled casing.

## Existing-database Deployment and Password Migration

1. Back up MySQL and stop account writes.
2. Run `go/login/model/migrations/20260803_password_auth.sql`. It first rejects
   NULL, empty, duplicate, and over-191-character accounts. It never truncates or
   merges data, then makes `account VARCHAR(191) PRIMARY KEY` and bounds the hash
   column to `VARCHAR(255)`.
3. For each existing account, run from `go/login` with the DSN secret in the
   environment:

   ```powershell
   $env:LOGIN_PASSWORD_MIGRATION_DSN = '<short-lived UPDATE credential from secret manager>'
   go run ./cmd/password_admin -account alice
   ```

   The command reads and confirms the password without terminal echo and updates
   exactly one already-existing, not-yet-Argon2id row. It refuses missing,
   duplicate, or already-migrated accounts, so it cannot become an implicit
   registration or password-reset API. For a controlled batch,
   `-password-stdin` accepts exactly two piped lines; plaintext must never be a
   command-line argument or logged.
4. Confirm migrated rows begin with `$argon2id$`. Unmigrated NULL/plaintext rows
   remain unable to use password login but can still use an independently bound
   third-party provider.
5. Inject `LOGIN_PASSWORD_AUTH_DSN`, set `PasswordAuth.Enabled: true`, deploy a
   canary, and test correct/wrong/unknown credentials. A missing DSN or unreachable
   database makes startup fail.
6. Disable the legacy Gate login path after its existing telemetry gate, so the
   Java Gateway's IP/account rate limiter is the single public ingress.

Public self-registration, password recovery, and user-driven password change are
still **not exposed**: they require a product contract, identity proof, refresh-
token revocation and audit policy across Gateway/client. Until those are designed,
operations use the explicit admin path above. In particular, “first password
presented claims an existing account” is forbidden and is not implemented.

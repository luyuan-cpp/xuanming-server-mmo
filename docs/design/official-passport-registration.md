# 官方通行证与账号注册接入设计（Casdoor OIDC）

状态：2026-11-23 初版设计与服务端接缝落码，待 Claude Code 审核与真进程联调；Unity 客户端 UI 与 PKCE 回调交由另一台机器 Codex 按 §7 实施。

关联：[认证提供者框架](./auth-provider-framework.md)、[第三方登录端到端设计](./third-party-login-end-to-end-design.md)、[双 Token 体系](./dual-token-authentication.md)、[账号注销](./account-deletion.md)、[账号角色目录](./account-role-authority.md)。

---

## 1. 需求分析

### 1.1 背景与缺口
当前服务端（`go/login`）已实现微信、QQ、微信/抖音小游戏、Apple、Google 等渠道的首次登录自动建号能力，但**没有面向玩家的自助账号注册体系**：
- 现有 `PasswordAuth` 凭据表（`go/login/model/password_auth_schema.sql`）刻意保持只读，写入只开放给离线运维工具 `cmd/password_admin`，不提供注册、验证码、改密与找回密码能力。
- 同时，另一条并行开发线（Claude Code）正在建设**全球账号库与第三方登录**。

### 1.2 核心目标与硬边界
1. **提供官方自有账号注册能力**：覆盖官网/PC 包、跨端通行证、邮箱/手机号注册、验证码、改密、找回密码及运营后台。
2. **与全球账号库零冲突并行开发**：不碰 `go/data_service`、不碰 `proto/`、不碰 `java/gateway_node`、不修改已有第三方登录与 `idtoken.go` 核心代码，减少一切合并冲突面；完成后交由 Claude Code 审核对接。
3. **不制造双账号主库**：游戏内角色目录、过户、防沉迷、7 天注销冷静期继续统一由游戏全球账号库收口，官方注册服务只承担身份认证（IdP）职责。

### 1.3 外部开源库选型说明（符合 AGENTS.md §11.2）
Go 标准库与仓库现有依赖不包含完整的“面向 C 端用户的注册页面、邮件/短信验证码、找回密码、人机验证与账号运营后台”能力。调研同语言与跨生态高 Star 开源方案对比如下：

| 候选项目 | 语言 | 协议 | 近似 Stars | 评估结论 |
|---|---|---|---|---|
| **Casdoor（`casdoor/casdoor`）** | **Go + React** | **Apache-2.0** | ~14.9k | **采用**。同为 Go 技术栈，原生支持 MySQL/TiDB、标准 OIDC（RS256 + `/.well-known/jwks` + Authorization Code + PKCE）、国内外邮箱/短信验证码、开箱注册/找回密码页与运营后台，且可通过极薄 OIDC 接缝零冲突接入现有 `go/login`。 |
| Ory Kratos（`ory/kratos`） | Go | Apache-2.0 | ~14k–15k | 备选。无头纯 API，适合 100% 端内自画注册 UI，但国内短信与开箱后台成本明显高于 Casdoor。 |
| Heroic Labs Nakama（`heroiclabs/nakama`） | Go | Apache-2.0 | ~13.8k | 不采用。属于整套游戏服务器，与现有自研 MMO 微服务/C++ 场景服高度重叠。 |
| Keycloak / Authentik | Java / Python | Apache-2.0 / MIT | ~33k / ~25k | 不采用。偏企业级 SSO，资源开销重且 C 端游戏注册体验差。 |
| Better Auth / Logto | TypeScript | MIT / MPL-2.0 | ~30k / ~14.6k | 不采用。引入 Node/TS 运行时栈，与当前 Go/Java 运维体系割裂。 |
| Supabase Auth / PocketBase | Go | MIT | 主仓 100k+ / ~61k | 不采用。前者强绑 PostgreSQL（与现有 MySQL/TiDB 栈不一致），后者底层 SQLite 不适合 MMO 全局并发。 |

---

## 2. 思想分析

本方案遵循**「认证注册独立，全球账号库统一收口」**的分层思想（与大厂官方通行证架构一致）：

1. **把自有官方通行证视为一个平级的 OIDC 登录渠道**：对 `go/login` 和全球账号库而言，Casdoor 与 Apple、Google 在协议上完全同构——都是签发 RS256 OIDC `id_token`、公开 JWKS 公钥的身份提供方，渠道名固定为 `auth_type = "passport"`（上线后不改名）。
2. **策略模式扩展（OCP）**：利用 `go/login/internal/logic/pkg/auth` 既有的 `Provider` 策略注册表与 `idTokenVerifier`（JWKS 缓存 + RS256 验签 + Redis 一次性防重放），只新增独立文件 `passport_provider.go` 与 `passport_auth.go`，不对现有 `loginlogic.go`、`resolveAccount`、`authTypeMetricLabel` 与 `anti_addiction.go` 做分支入侵。
3. **单一真源（DRY / 逻辑库自治）**：Casdoor 独占自身 `casdoor` 凭证库，只负责“官方身份认证”；游戏全局账号库独占游戏账号与角色目录，两者之间只通过不可变的 `sub`（`passport_<sub>`）单向解耦，绝不跨库直连 SQL。

---

## 3. 设计分析

### 3.1 端到端链路

```text
Unity 客户端 (mmorpg-client)
  │ 1. 生成 PKCE code_verifier / code_challenge(S256) 与随机 state
  │ 2. 拉起系统浏览器 / 移动端系统认证页打开 Casdoor 托管登录/注册页：
  │    GET <CasdoorOrigin>/login/oauth/authorize
  │        ?client_id=<ClientId>&response_type=code&redirect_uri=<Callback>
  │        &scope=openid&state=<State>&code_challenge=<Challenge>&code_challenge_method=S256
  ▼
Casdoor 官方通行证（独立部署 deploy/passport/）
  │ 3. 玩家完成注册（邮箱/手机 + 验证码 + 密码）或登录、找回密码
  │ 4. 回调 redirect_uri?code=<AuthCode>&state=<State>
  ▼
Unity 客户端
  │ 5. 校验 state 一致，用 code + code_verifier 调 Casdoor 换票：
  │    POST <CasdoorOrigin>/api/login/oauth/access_token
  │         (grant_type=authorization_code, client_id, code, redirect_uri, code_verifier)
  │    → 取响应中的 id_token（紧凑 RS256 JWT，tokenFormat=JWT-Standard）
  │ 6. 调现有游戏网关登录入口（不新增网关接口、不改 proto）：
  │    POST /api/login { "zone_id": <zone>, "auth_type": "passport", "auth_token": "<id_token>" }
  ▼
Java Gateway (gateway_node) ──gRPC──► Go Login (go/login)
  │ 7. auth.Get("passport") → PassportProvider.Validate(ctx, id_token)
  │    a. RS256 + JWKS 验签（GET <Endpoint 或 Issuer>/.well-known/jwks，含缓存与防刷）
  │    b. 校验 iss == Config.Issuer、aud 包含 Config.ClientId、sub 非空且无空白、
  │       len("passport_" + sub) <= 191、exp/iat/nbf 合法
  │    c. 校验令牌生命周期 (exp - iat) <= 2h（防止 Casdoor 误留默认 168h 撑爆 Redis）
  │    d. Redis SET NX 登记 login:idtoken:used:v1:passport:<sha256(token)>（一次性防重放）
  │    e. 解析出账号标识 passport_<sub>，交给全球账号库/账号目录映射并签发游戏双 Token
  ▼
现有 /api/assign-gate → TCP Gate → Login(access_token) → 建角 / EnterGame
```

### 3.2 配置契约（`go/login`）

新增独立配置类型 `PassportAuthConf`（`go/login/internal/config/passport_auth.go`），挂在 `AuthConfig.Passport`（可选，未配即不启用）：

```yaml
AuthProviders:
  Passport:
    # 必填：Casdoor 签发 id_token 的 iss（与 Casdoor app.conf / 环境变量 origin 一致，不带末尾斜杠）。
    # 生产模式强制 https://，仅 Mode=dev/test 允许 http://。
    Issuer: "https://passport.example.com"
    # 必填：Casdoor Application 的公开 ClientId（令牌 aud 必须包含该值）。
    ClientId: "xuanming-game-client"
    # 可选：login 拉取 /.well-known/jwks 的内部基址；留空则直接向 Issuer 拉取。
    # 适用于公网 iss（如 https://passport.example.com）与集群内网拉公钥（如 https://casdoor.internal:8000 或本地 http://sandbox-mock:18090）分离的场景。
    Endpoint: ""
```

### 3.3 跨系统联动边界（不产生额外耦合）
- **全球账号库（Claude Code 审核重点）**：`PassportProvider.Validate` 返回 `&AuthResult{Account: "passport_" + claims.Sub}`，与 `AppleProvider`（`apple_<sub>`）、`GoogleProvider`（`google_<sub>`）完全同型。当全球账号库落地「不可变 `account_id` + `(provider_type, provider_uid)` 绑定表」后，直接按 `provider_type = "passport"`, `provider_uid = claims.Sub` 收口即可。
- **国内实名防沉迷**：`go/login/internal/config/anti_addiction.go` 默认按 fail-closed 规则要求所有不在 `PlatformManagedAuthTypes` 名单里的渠道走游戏方自实名，因此 `passport` 自动受国内实名与防沉迷管辖，无需改动防沉迷代码。
- **账号注销冷静期（后续对接项）**：7 天冷静期到点删除游戏账号时，参考 `apple_revoke.go` 在 `account_deletion` 执行器中追加可选的 Casdoor 管理接口调用，删除或匿名化对应 `sub` 的用户记录以释放邮箱/手机号（本批不修改 `account_deletion.go`，避免与全球账号库改动碰撞）。
- **指标与日志**：`deprecation.go` 的 `authTypeMetricLabel` 自动识别已注册的 `"passport"` 低基数 label；成功日志用 `maskAccount` 对 `sub` 脱敏，不输出完整 `id_token`。

### 3.4 独立部署栈（`deploy/passport/`）
- `deploy/passport/docker-compose.yml`：独立运行官方 `casbin/casdoor:v4.27.1` 与专属 MySQL 8.0（`passport-db`），全部数据库口令通过环境变量必填注入（`${PASSPORT_DB_PASSWORD:?...}`），不把任何密码写入仓库。
- `deploy/passport/conf/app.conf`：无密钥基础配置（`initDataNewOnly = true` 保证首次初始化后重启不覆盖运行时数据）。
- `deploy/passport/init_data.json`：预置组织 `xuanming` 与公开 PKCE 应用 `app-xuanming-game`（`clientId: "xuanming-game-client"`、`enableSignUp: true`、`tokenFormat: "JWT-Standard"`、`expireInHours: 1`、不预置内联 `clientSecret`、不在 Casdoor 内重复开启游戏服已有第三方社交渠道）。
- `go/login/cmd/sandbox_mock/passport.go`：提供离线单测/本地冒烟用的 `GET /.well-known/jwks` 与 `GET /mock/passport/id_token`，无需起 Docker 也可验证整条验签链路。

---

## 4. 漏洞分析与防护措施

| 潜在风险 / 漏洞 | 攻击或故障路径 | 本方案防护措施 |
|---|---|---|
| **1. Casdoor 默认 168h 令牌撑爆 Redis 与放大泄露窗口** | Casdoor 应用模板默认 `expireInHours=168`（7 天）。`login` 的一次性防重放把每枚令牌的 SHA-256 写入 `LoginRedis` 直到 `exp + 1m`，默认 7 天会把重放表内存放大 168 倍，且截获未用令牌 7 天有效。 | 双重防线：① `init_data.json` 固定 `expireInHours: 1`；② `PassportProvider` 硬性校验 `(exp - iat) <= 2h`，超限按 `ErrProviderUnavailable`（配置故障码）直接拒收并告警，防止误配静默上线。 |
| **2. Host 头投毒伪造 `iss`** | Casdoor 未配置 `origin` 时会按请求 `Host` 头推导 `iss`，反向代理配置疏漏时可能导致 `iss` 不一致或被诱导签发错误 `iss`。 | `deploy/passport/docker-compose.yml` 强制注入 `origin`；`go/login` 启动门禁强制校验 `Issuer` 非空、无末尾斜杠、生产模式必须 `https://`，验签严格按字节比对 `claims.Iss == Issuer`。 |
| **3. `sub` 可变导致玩家改名后账号脱钩** | 若 Casdoor 输出可变用户名作 `sub`，玩家修改用户名后会变成新游戏账号、丢失全部角色。 | `init_data.json` 固定 `tokenFormat: "JWT-Standard"`（`sub` 为不可变用户 UUID），上线前真进程冒烟必须核实 `sub` 为 UUID 且改名不变。 |
| **4. 授权码截获与 CSRF** | 桌面 loopback 回调或移动端自定义 URL Scheme 可能被恶意应用截获 `code` 或伪造回调。 | 客户端强制使用 **OAuth2 Authorization Code + PKCE（S256）** + 一次性随机 `state`；且服务端对 `id_token` 复用 `LoginRedis` 的 `SET NX` 一次性防重放守卫。 |
| **5. 客户端内嵌 `client_secret` 泄露** | 把机密客户端密钥打进 Unity 安装包等同公开值，攻击者可冒用应用身份。 | Casdoor 应用配置为 **Public PKCE Client**，Unity 客户端只带公开 `client_id` 与 `code_verifier`，零密钥进客户端包，也不向仓库写入任何 `clientSecret`。 |
| **6. 与游戏服自有社交渠道冲突造成双账号分裂** | 若在 Casdoor 里同时打开微信/Apple/Google 登录，玩家可能一条走游戏服原生 `apple`，一条走 `passport` 里的 Apple，分裂成两个游戏账号。 | Casdoor 组织与 `app-xuanming-game` **仅开启官方自有注册（邮箱/手机/密码/验证码）**，严禁在 Casdoor 内开启已由 `go/login` 原生接入的社交渠道。 |
| **7. JWKS 中间人篡改 / 算法混淆 / 账号超长** | 明文 HTTP 拉取 JWKS 可被篡改公钥；伪造 `alg=none/HS256` 或超长 `sub`（超过 `account_directory` 191 字节上限）导致异常。 | 生产模式强制 `Issuer` 与 `Endpoint` 均为 `https://`；复用 `idtoken.go` 仅接受 `RS256` + ≥2048-bit RSA 公钥；`PassportProvider` 额外拦截含空白或拼接后超 191 字节的 `sub`。 |
| **8. 密钥误入 Git 或未展开 `${VAR}`** | YAML 写 `${CLIENT_ID}`（go-zero 不展开）或把 DB 密码提交进仓库。 | `passport_auth.go` 复用 `containsUnexpandedPlaceholder` 在启动期拒绝 `${...}`；`deploy/passport/` 所有数据库密码均通过 `${VAR:?...}` 运行时环境变量强制注入。 |

---

## 5. 步骤分析与执行步骤

### 5.1 服务端与部署落地步骤（本轮已完成）
1. 落盘本设计文档并运行 `python tools/scripts/gen_docs_index.py` 更新 `docs/README.md`。
2. 新建独立 Casdoor 部署栈：
   - `deploy/passport/docker-compose.yml`
   - `deploy/passport/conf/app.conf`
   - `deploy/passport/init_data.json`
3. 在 `go/login` 中以最小新增文件方式接入 `passport` OIDC Provider（零修改 `idtoken.go` / `idtoken_providers.go` / `loginlogic.go` / `data_service` / `proto`）：
   - 新增 `go/login/internal/config/passport_auth.go` 与 `passport_auth_test.go`
   - `go/login/internal/config/config.go` 与 `auth_providers.go` 极简接线（仅增 `Passport` 字段与校验调用）
   - 新增 `go/login/internal/logic/pkg/auth/passport_provider.go` 与 `passport_provider_test.go`
   - `go/login/internal/svc/auth_init.go` 在 `RegisterIdTokenProviders` 中注册 `auth.PassportAuthType`，并新增 `auth_init_passport_test.go`
   - 新增 `go/login/cmd/sandbox_mock/passport.go` 与 `passport_test.go`，并在 `main.go` 挂载 `/.well-known/jwks` 与 `/mock/passport/id_token`
4. 运行 Go 定向单测验证编译与全部契约断言。

### 5.2 本地启动 Casdoor 与真进程联调命令
```powershell
# 1. 在终端注入本地开发数据库口令（不落盘、不进 Git）
$env:PASSPORT_DB_ROOT_PASSWORD = "<local-root-password>"
$env:PASSPORT_DB_PASSWORD      = "<local-casdoor-password>"
$env:PASSPORT_ORIGIN           = "http://127.0.0.1:8000"

# 2. 启动独立 Casdoor 通行证服务（http://127.0.0.1:8000）
docker compose -f deploy/passport/docker-compose.yml up -d

# 3. 验证 OIDC Discovery 与 JWKS
curl.exe -s http://127.0.0.1:8000/.well-known/openid-configuration
curl.exe -s http://127.0.0.1:8000/.well-known/jwks

# 4. 在本地 login.yaml（Mode: dev）添加：
# AuthProviders:
#   Passport:
#     Issuer: "http://127.0.0.1:8000"
#     ClientId: "xuanming-game-client"
#     Endpoint: "http://127.0.0.1:8000"   # 容器内改为 http://casdoor:8000
```

### 5.3 交付 Claude Code 审核清单
1. **全球账号库对接**：确认全球账号库接纳 `auth_type = "passport"` 及 `passport_<sub>`（或改为 `(provider="passport", uid=sub)` 绑定表映射）。
2. **文件冲突核对**：仅在既有文件 `config.go`、`auth_providers.go`、`auth_init.go`、`cmd/sandbox_mock/main.go` 有数行增量，确认与全球账号库分支无冲突。
3. **生产部署与 K8s 渲染**：上生产前决定是否在 `tools/scripts/k8s_deploy.ps1` 增加 `-LoginPassportIssuer` / `-LoginPassportClientId` / `-LoginPassportEndpoint` 渲染参数，以及注销执行器是否补充 Casdoor 用户停用/匿名化钩子。

---

## 6. 后续演进（不在本批范围）
1. **账号注销联动**：在 `account_deletion` 终局执行器中增加可选 `PassportDeleter` 接口，冷静期满删除游戏账号后异步调用 Casdoor API 删除或匿名化对应 `sub`，释放原邮箱/手机号。
2. **邮件 / 短信发码通道配置**：上线前在 Casdoor 后台配置真实 SMTP（如 AWS SES / 阿里云邮件推送）或短信通道及人机验证（如 Cloudflare Turnstile / 滑块），不在仓库写入任何供应商密钥。
3. **生产 K8s 部署清单**：将 Casdoor 迁移至受管 K8s Deployment + 独立 TiDB/MySQL 库与 HTTPS Ingress。

---

## 7. Unity 客户端 UI 与接入交接（供另一台电脑 Codex 执行）

> **目标仓库**：`mmorpg-client`（`https://github.com/luhailong-cpp/mmorpg-client`）
> **前置**：严格阅读 `mmorpg-client/AGENTS.md`，禁止手改 `Assets/Scripts/Proto/Generated/*` 或 `*Rpc.cs`（本功能不改任何 `.proto`，无需重新导协议）。

### 7.1 任务目标
在 `mmorpg-client` 登录界面接入官方通行证（Casdoor OIDC）注册/登录入口：玩家点击「官方账号登录 / 注册」→ 拉起系统浏览器（桌面/编辑器）或移动端认证页打开 Casdoor OAuth2 + PKCE 注册/登录页 → 拿到一次性 `id_token` → 复用现有 `LoginCredential.ForThirdParty("passport", idToken)` 走 `POST /api/login` → `/api/assign-gate` → Gate 进游戏。

### 7.2 服务端契约（已固化，勿改名字）
- `auth_type` 固定为 `"passport"`。
- 网关登录入口不变：`POST /api/login { "zone_id": <uint>, "auth_type": "passport", "auth_token": "<id_token>" }`。
- `id_token` **一次性消费**：登录后或失败后丢弃，不落盘、不写 `PlayerPrefs`、不进日志；需要重认证（如申请注销）时重新发起一次 OAuth2 + PKCE 授权。
- Casdoor 默认本地开发参数（通过配置注入，禁止硬编码生产域名或密钥）：
  - `PassportOrigin`：`http://127.0.0.1:8000`（生产为 `https://passport.<domain>`）
  - `PassportClientId`：`xuanming-game-client`
  - 授权地址：`<PassportOrigin>/login/oauth/authorize`
  - 换票地址：`<PassportOrigin>/api/login/oauth/access_token`
  - 必填参数：`response_type=code`、`scope=openid`、`client_id`、`redirect_uri`、`state`（至少 128-bit 随机 URL-safe 串）、`code_challenge`（`BASE64URL(SHA256(code_verifier))`）、`code_challenge_method=S256`
  - 换票请求（`application/x-www-form-urlencoded`）：`grant_type=authorization_code`、`client_id`、`code`、`redirect_uri`、`code_verifier`（**不传 `client_secret`**），从 JSON 响应取 `id_token`。

### 7.3 需要改动的文件与精确接缝

1. **`Assets/Scripts/Net/LoginCredential.cs`**
   - 在 `LoginAuthTypes` 新增常量：
     ```csharp
     public const string Passport = "passport"; // 官方通行证(Casdoor OIDC id_token)
     ```
2. **`Assets/Scripts/Core/ClientSettings.cs`**（及启动参数覆写 `ClientLaunchArguments`，若有）
   - 新增官方通行证配置项（空串 = 不显示官方通行证按钮，保持与其他平台开关一致）：
     - `PassportOrigin`（默认空串或开发参数可覆写，例如 `http://127.0.0.1:8000`）
     - `PassportClientId`（例如 `xuanming-game-client`）
     - `PassportRedirectUri`（桌面默认 `http://127.0.0.1:18765/passport/callback`；移动端为自定义 scheme `xuanming://passport/callback`，需与 `deploy/passport/init_data.json` 保持一致）
3. **新增 `Assets/Scripts/Net/PassportSignIn.cs`**（对齐现有 `PlatformSignIn.cs` 与 `MiniGameLogin.cs` 的接口风格）
   - 返回统一结果 `PlatformSignInResult`（复用 `PlatformSignIn.ErrorCanceled` / `ErrorFailed` / `ErrorUnsupported`），回调保证在 Unity 主线程且每次请求恰好触发一次。
   - **PKCE 工具（纯 C# 标准库 `System.Security.Cryptography`，不引入外部 Unity 插件）**：
     - 生成 32 字节随机 `code_verifier`（Base64URL 无填充）与 `code_challenge = Base64Url(SHA256(ASCII(code_verifier)))`；
     - 生成 16 字节随机 `state`；
     - 构造授权 URL 并在桌面/编辑器通过 `Application.OpenURL(url)` 打开系统浏览器，同时在本机 `127.0.0.1:18765` 启动临时单次 `HttpListener`（设 180 秒超时、支持取消、绑定 `127.0.0.1` 绝不监听 `0.0.0.0`），收到回调后：
       1. 校验 `state` 严格相等（不等即按失败拒收）；
       2. 回显简短的“登录已完成，请返回游戏”HTML 并立即关闭监听；
       3. 用 `UnityWebRequest.Post` 调 `<PassportOrigin>/api/login/oauth/access_token` 换取 `id_token`；
       4. 清理内存中的 `code_verifier` / `state` / `code`，回调 `PlatformSignInResult.Success(idToken, null)`。
     - 移动端（iOS/Android）若本批未接原生 `ASWebAuthenticationSession` / Chrome Custom Tabs，先按 `Application.deepLinkActivated` 接 `xuanming://passport/callback` 并在未配置时回 `ErrorUnsupported`，同时明确在交付说明中标注移动端深度链接未真机验证。
4. **`Assets/Scripts/UI/Ugui/QdaoServerSelectView.Visuals.cs` + `QdaoServerSelectView.cs` + `QdaoServerSelectView.LoginUtilities.cs`**
   - **UI 控件布局**（严格遵循现有 `BuildRefreshCredentialPanel()` 风格与生产 prefab 烘焙纪律）：
     - 在 `_credentialPanel` 账号弹窗中增加「官方账号登录 / 注册」按钮 `_passportSignInButton`（建议复用现有 `BuildChannelSignInButton` 或 `ArtButton(..., "clean_tab", ...)` 样式，摆放在账号密码输入区域下方 / 渠道登录位，避免与 `_appleSignInButton` / `_googleSignInButton` 重叠——例如桌面端无 Apple/Google 按钮时直接占 `y=900`，移动端与 Apple/Google 上下错开或横排并列）。
     - 在按钮点击与 `RefreshVisualState()` 中：
       - 当 `PassportSignIn.Available` 为 true（且不在微信/抖音小游戏宿主内）显示 `_passportSignInButton`，`_busy` 时置为不可交互；
       - 点击调用 `BeginChannelSignIn(LoginAuthTypes.Passport)`；
       - 在 `RequestChannel(string authType, Action<PlatformSignInResult> onDone)` 中补上分支：
         ```csharp
         if (authType == LoginAuthTypes.Passport) PassportSignIn.Request(onDone);
         ```
     - **取消等待状态**：若玩家拉起浏览器后直接关掉浏览器窗口未完成授权，账号面板或状态栏需提供「取消授权」入口（调用 `PassportSignIn.Cancel()` 立即释放本地监听并恢复 `_busy = false`），防止界面卡死在“正在等待授权…”。
     - **账号注销重认证联动（`QdaoServerSelectView.LoginUtilities.cs`）**：当 `ClientSettings.LastLoginMethod == LoginAuthTypes.Passport` 且玩家点击「注销账号」时，走渠道重认证流程调用 `PassportSignIn.Request` 获取新 `id_token` 后提交 `/api/account-deletion`（不传 `authorization_code`）。
   - **Prefab 重新烘焙**：如新增 `[SerializeField] private Button _passportSignInButton`，须按 `mmorpg-client` 规范用 `QdaoUguiBuilder` 同步更新烘焙后的生产 Prefab，并在 `ValidatePrefabReferences()` 做兼容性核对。

### 7.4 Codex 验收标准与测试清单
1. **EditMode 单测**（新建 `Assets/Tests/EditMode/Net/PassportSignInTests.cs`）：
   - PKCE `S256` 向量测试（对照 RFC 7636 Appendix B 固定向量验证 `code_challenge`）；
   - `state` 不匹配拒绝、缺 `code` 拒绝、重复回调幂等忽略、`Cancel()` 后回调只触发一次 `ErrorCanceled`；
   - `LoginCredential.ForThirdParty(LoginAuthTypes.Passport, "tok")` 构造与断言。
2. **UI 与编译检查**：
   - 运行 `mmorpg-client/tools/client_compile_check.ps1` 与受影响的 EditMode 测试；
   - 编辑器预览验证登录弹窗在不同分辨率（16:9 / 21:9）下「官方账号登录 / 注册」按钮无遮挡、无文字溢出；
   - 若服务端与 `deploy/passport` 已启动，完成一次端到端手工联调：点「官方账号登录 / 注册」→ Casdoor 注册新邮箱 → 回调返回游戏 → `/api/login` 成功拿双 Token → 选服进角色界面。

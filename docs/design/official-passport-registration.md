# 官方通行证与账号注册接入设计（Casdoor OIDC）

状态：2026-11-24 已完成服务端 OIDC 接缝、独立 Casdoor 部署栈、Unity 道家 Q 版客户端 UI/PKCE 与移动端深度链接构建后处理，并通过真实 Casdoor v4.27.1 容器 → `go/login` `PassportProvider` 端到端实测验证。2026-11-26 Claude Code 对照 Casdoor v4.27.1 源码审核，结论与定案见 §5.4；同日落码（未编译、未运行，验证命令见 `docs/PROGRESS.md` 同日「官方通行证审核」条目）：只收身份令牌（§4 第 11 项）、账号注销时删除通行证用户（§3.5）、初始化数据改为注册必须验证邮箱并新增服务端专用应用、K8s 部署参数、iOS 改用系统认证会话（§4 第 14 项）。同日第二轮独立审查（不沿用现有做法、按行业标准重看）的结论见 §8：初始化数据缺省开人机验证、审计记录保留 180 天（已改，未起容器）；玩家可自删通行证用户、注册缺协议勾选等待办见 §8.3、§8.4。同日负责人确认后落码 §3.6：通行证改密 / 封禁 / 删除之后，此前登录的游戏令牌刷新时整号吊销（未编译、未运行）。

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
  │       len("passport_" + sub) <= 191、exp/iat/nbf 合法、tokenType == "id-token"
  │       (同一次换票给出的访问令牌 / 刷新令牌同钥同 iss/aud/sub,只有这一项不同,按凭证无效拒绝)
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
    # 生产必填：login 调 Casdoor 管理接口用的机密应用（init_data.json 的 app-xuanming-server），账号注销时删除通行证里的用户（§3.5），
    # 刷新游戏令牌前核对改密 / 封禁 / 删除（§3.6）。
    # 不能与 ClientId 相同；密钥只写环境变量名，内联的 AdminClientSecret 只在 Mode=dev/test 放行。
    # 生产模式配了 Passport 却缺这两项，login 拒绝启动；dev/test 可以不配（启动告警，注销后通行证用户留着）。
    AdminClientId: "xuanming-game-server"
    AdminClientSecretEnv: "MMORPG_LOGIN_PASSPORT_ADMIN_CLIENT_SECRET"
```

### 3.3 跨系统联动边界（不产生额外耦合）
- **全球账号库（2026-11-26 定案：不用改）**：`PassportProvider.Validate` 返回 `&AuthResult{Account: "passport_" + claims.Sub}`，与 `AppleProvider`（`apple_<sub>`）、`GoogleProvider`（`google_<sub>`）完全同型。账号目录把账号当不透明串存（上限 191 字节），不解析前缀，`passport_<uuid>` 直接可用；`sub` 是 Casdoor 的用户 id（UUID），玩家在通行证里改用户名、邮箱、手机号都不变，也不会和运维开通的口令账号撞名（口令账号不由玩家自取）。将来若落地「不可变 `account_id` + `(provider_type, provider_uid)` 绑定表」，按 `provider_type = "passport"`、`provider_uid = sub` 收口。
- **国内实名防沉迷**：`go/login/internal/config/anti_addiction.go` 默认按 fail-closed 规则要求所有不在 `PlatformManagedAuthTypes` 名单里的渠道走游戏方自实名，因此 `passport` 自动受国内实名与防沉迷管辖，无需改动防沉迷代码。
- **账号注销**：通行证账号的邮箱、手机号、口令哈希存在 Casdoor 里，注销必须连同通行证里的用户一起删，见 §3.5（2026-11-26 落码）。申请注销时的重新认证与其它渠道一样：客户端重新走一次授权拿新的 `id_token`。
- **指标与日志**：`deprecation.go` 的 `authTypeMetricLabel` 自动识别已注册的 `"passport"` 低基数 label；成功日志用 `maskAccount` 对 `sub` 脱敏，不输出完整 `id_token`。

### 3.4 独立部署栈（`deploy/passport/`）
- `deploy/passport/docker-compose.yml`：独立运行官方 `casbin/casdoor:v4.27.1` 与专属 MySQL 8.0（`passport-db`），全部数据库口令通过环境变量必填注入（`${PASSPORT_DB_PASSWORD:?...}`），不把任何密码写入仓库。
- `deploy/passport/conf/app.conf`：无密钥基础配置（`initDataNewOnly = true` 保证首次初始化后重启不覆盖运行时数据）。
- `deploy/passport/init_data.json`：预置组织 `xuanming`（`passwordType: bcrypt`、`enableSoftDeletion: false`——必须保持关闭，开了之后删用户只打标记，见 §3.5；`recordRetentionDays: 180`，接口审计记录到期自动清，见 §8.2）与两个应用：
  - 公开 PKCE 应用 `app-xuanming-game`（`clientId: "xuanming-game-client"`、`enableSignUp: true`、`tokenFormat: "JWT-Standard"`、`expireInHours: 1`、`grantTypes` 只有 `authorization_code`、不预置内联 `clientSecret`、不在 Casdoor 内重复开启游戏服已有第三方社交渠道）。`providers` 挂内置图片验证码 `provider_captcha_default`、规则 `Always`：注册、发验证码、登录都先过人机验证（§8.2）。注册项 `Email` 的 `rule` 是 `Normal`（必须验证邮箱；2026-11-26 由 `No verification` 改回）：没给应用挂邮件通道之前自助注册发不出验证码、注册不了，这是有意的缺省；本地联调用管理接口建测试用户（§5.2）。
  - 机密应用 `app-xuanming-server`（`clientId: "xuanming-game-server"`，不能登录、没有回调地址）：只给 `go/login` 调管理接口用（注销时删用户、刷新前查用户）。Casdoor 让应用凭据在它自己的组织里有管理权限，所以它必须与客户端的公开应用分开；`clientSecret` 不进仓库，首启时由 Casdoor 随机生成，运维从后台取出放进 login 的密钥。
- `go/login/cmd/sandbox_mock/passport.go`：提供离线单测/本地冒烟用的 `GET /.well-known/jwks`、`GET /mock/passport/id_token`（种子 `ACCESS_TOKEN` / `REFRESH_TOKEN` 签另外两种令牌类型）与管理接口 `GET /api/get-user`、`POST /api/delete-user`（`GET /mock/passport/deleted_users` 查被删的用户；`POST /mock/passport/change_password`、`/mock/passport/forbid_user` 模拟改密与封禁），无需起 Docker 也可验证验签、注销删用户与刷新前核对三条链路。

### 3.5 账号注销时删除通行证用户（2026-11-26）

**为什么必须做**：Apple、Google、微信的账号，玩家的个人信息在渠道那边，游戏只有一个 `sub`。官方通行证不一样，邮箱、手机号、口令哈希存在我们自己的 Casdoor 库里。只删游戏账号的话个人信息还留着，不算注销（App Store 审核指南 5.1.1(v)、Google Play 账号删除要求、个人信息保护法第四十七条；本仓 `AGENTS.md` §9 的「账号真注销」红线）。

**做法**（`go/login/internal/logic/pkg/auth/passport_admin.go`，执行者 `accountdeletion/executor.go`）：

1. 时机：冷静期满、执行者认领（账号行 1 → 2，不可撤销）之后，**角色全部删完、目录收尾（删账号行）之前**。账号行还在「执行中」就是待办记录：这一步没成，执行者下一轮再来；要是先删了账号行，就再没有东西能让执行者想起这名通行证用户。冷静期内不动通行证，玩家还要靠它登录来撤销注销。
2. 调用（Casdoor v4.27.1 管理接口，HTTP Basic 带 `AdminClientId` / 密钥；成功与拒绝都回 HTTP 200，看应答体 `status`）：
   - `GET /api/get-user?userId=<sub>`：按不可变的用户 id 取当前的组织与用户名（用户名可改，每次现查）。`data` 为 `null` = 已经不在，按删完处理。
   - `POST /api/delete-user {owner, name}`：`data` 为 `"Affected"` 才算删掉（Casdoor 同时作废这名用户的令牌与会话）。
   - 再 `GET` 一次，查不到才回成功。组织被人开了软删除时第一次删只打标记，这一步会发现并回错误，下一轮再删一次才真删。
3. 每一步都能重做；失败回错误、账号行留着，由 `data_service_account_deletion_overdue`（状态 2 超过 24 小时）告警。
4. 凭据写错不会误判成「用户不在」：Casdoor 把认不出的 Basic 凭据当匿名，匿名查到用户会被拒（`status=error`），查不到与真的查不到一样回 `null`。
5. 没配管理凭据（只有 dev/test 允许）：通行证账号照常注销，通行证用户留着，每次记 ERROR。

**通行证里删不到的残留**：Casdoor 的 `verification_record`（发过的验证码，含收件邮箱 / 手机号）与 `record`（接口审计日志）不随删用户清理，生产要按隐私政策写明的保留期定期清（§5.4 生产清单）。

注销完成后同一个人用同一邮箱重新注册，Casdoor 给的是新 `sub`，游戏里是全新账号；`/api/login` 的「你之前的账号已于 X 注销」提示按账号串摘要匹配，对通行证账号不会出现（旧 `sub` 已不存在）。

### 3.6 通行证改密 / 封禁 / 删除之后吊销游戏登录（2026-11-26）

**为什么要做**：Casdoor 改密只作废它自己签的令牌和会话（`controllers/user.go` 的 `SetPassword` 末尾），不通知游戏。游戏的 refresh token 有效 30 天，每次续期再续 30 天。账号被盗以后玩家去通行证改了密码，盗号者手里的游戏令牌照样能一直续下去。各家通行证的通行做法是改密即全端下线；封禁、删除同理。

**判定：按会话的认证时刻比，不按账号记水位。**

1. 登录时把身份令牌的 `iat`（通行证的时钟）写进游戏令牌载荷 `TokenData.IdentityIssuedAt`（`AuthResult.IdentityIssuedAt` → `LoginLogic.issueTokens` → `token.Manager.Issue`），同一家族轮换时原样带下去（`token.go` 的 `rotate`）。
2. 刷新时，在轮换之前对 `auth_type = passport` 的令牌做一次核对（`refreshtokenlogic.go` 的 `passportSessionRevoked`）：`token.Manager.ReadRefreshPayload` 读出载荷，再用管理凭据查一次 `GET /api/get-user?userId=<sub>`（`PassportAdmin.CheckSession`）。
   - 用户不在或被标记删除，记 `user_gone`；`isForbidden` 为真，记 `user_forbidden`；`lastChangePasswordTime`（RFC 3339）不早于 `IdentityIssuedAt`，记 `password_changed`。其余照常续期。
   - 不续期时 `RevokeAll` 吊销整个账号的令牌家族：每台设备的 access 立即失效，refresh 续不了。回的码与令牌失效相同（`kLoginAccountNotFound`），客户端照常走完整重登。
3. 这和 Firebase 的 `tokensValidAfterTime`、Microsoft Graph 的 `signInSessionsValidFromDateTime` 是同一个思路：会话带着自己的认证时刻，拿它和用户的「此后才有效」时刻比。

**为什么这样定：**

- 两个时刻都取自通行证自己的时钟，不受 login 与通行证之间的时钟偏差影响。`TokenData.CreatedAt` 不能拿来比：它每次轮换都更新，用的又是 login 的时钟。
- 按会话的认证时刻判，改密之后重新登录的会话不会被误吊销。另一种做法是按账号记一个「上次看到的改密时刻」水位，它要多一个 Redis 键，而且发现改密的那一刻会把改密后新登录的会话一起吊销。
- 签发时刻跨轮换不变。通行证故障期间核对不了、照常续期，续出来的令牌在下一次核对时照样判得出来，没法借一次续期把改密之前的登录变成改密之后的。
- 身份令牌如果在改密之前签发、改密之后才拿来登录（比如被截获、还没用过的令牌，1 小时内有效），它的 iat 早于改密，第一次刷新就会被判出来。
- 核对放在轮换之前。吊销失败就不续期（计 `revoke_failed`），旧令牌还在，下一次刷新再核对、再吊销。如果放在轮换之后，新的一对已经写下，客户端重试走宽限窗口的重放会原样拿到它，绕过核对。
- 同一秒里改密与签发分不出先后，按改密在后处理。注册不写改密时刻；改完密码再回游戏登录，不会和改密落在同一秒。

**故障与代价：**

- 通行证或网络出故障、应答认不出：照常续期，计 `login_passport_session_check_total{result="error"}` 并记 ERROR，不让通行证故障把在线玩家都踢下线。error 持续出现，说明改密已经踢不掉人：`deploy/k8s/login-alerts.yaml` 的 `LoginPassportSessionCheckFailing`（10 分钟内一半以上的核对失败，持续 10 分钟）报 warning，`tools/scripts/tests/gate_pick_alerts_contract.tests.ps1` 钉住指标名与取值。
- 通行证账号每次刷新（约 2 小时一次）多一次内网调用，时限 1 秒（`passportSessionCheckTimeout`，login 的 RPC 超时缺省 4 秒）。每次刷新多一次 Redis 读（先读载荷再轮换）。
- 没配管理凭据（只有 dev/test 允许）时不核对，启动时记 ERROR。生产模式配了通行证就必须配管理凭据（§3.2）。
- 管理接口走 `Endpoint`，生产必须指向内网入口（§8.4 第 2 条）。指错了，每次核对都按故障放行，改密就再也踢不掉人，只在指标里看得到。
- 这个字段加上之前签的通行证令牌，`IdentityIssuedAt` 为 0，按最早算：玩家改过密码的话要重新登录一次。项目未上线，不受影响。

**做不到的（明确接受）：**

- 已经连在 gate 上的会话不踢。这与 `dual-token-authentication.md`「已进游戏的连接」的既有口径一致：吊销只让令牌失效，断线之后重连时才失效。要立刻踢下线需要两样东西：一个新的下线原因提示（改 `Tip.xlsx`、三端重编），以及实时触发（Casdoor 的 webhook，要在 Java 网关新开入口转给 login）。这两样列为后续。
- 从改密到被发现，最长是一个 access token 的寿命（2 小时）：盗号者手里的 access token 过期之前还能拿去连 gate。这个账号任何一个改密之前的会话下一次刷新时，就整号吊销。
- Casdoor 后台的「强制下线」（`/api/logout-user`）不改 `lastChangePasswordTime`，判不出来。要踢盗号者，请玩家或客服改密。

**验证：** 单测钉住判定、跨轮换与刷新逻辑（`passport_admin_test.go` 的 `TestPassportAdminCheckSession`、`token/identity_issued_at_test.go`、`clientplayerlogin/refresh_passport_session_test.go`）。沙箱 mock 加了 `POST /mock/passport/change_password?user=<种子>` 与 `POST /mock/passport/forbid_user?user=<种子>`，冒烟时先登录，再调其中一个，再刷新，应当被拒。都未编译、未运行，命令见 `docs/PROGRESS.md` 同日「官方通行证改密后吊销游戏登录」条目。

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
| **9. Casdoor 管理员首启接管** | Casdoor v4.27.1 首启时 `built-in/admin` 无口令，日志提示「在浏览器里设置」，端口暴露期间任何访问者都能抢先设置管理员口令并接管整个通行证。 | `docker-compose.yml` 强制注入 `initAdminPassword: ${PASSPORT_ADMIN_PASSWORD:?...}`，并默认只绑 `127.0.0.1`（`PASSPORT_BIND` 显式放开）；2026-11-26 真容器实测配置口令可登录、默认 `123` 不可登录。 |
| **10. `redirect_uri` 宽松匹配外泄授权码** | 若 IdP 对回调地址做子串/正则宽松匹配，攻击者可构造带合法回调串的外站地址截走授权码。 | 2026-11-26 真容器实测 v4.27.1 对 10 种变形（外站夹带、前后缀、`.` 通配、子域拼接、`localhost`/其它回环端口）全部拒绝，仅精确的两个回调地址签发授权码；升级 Casdoor 版本时须重跑该探针。 |
| **11. 访问令牌 / 刷新令牌冒充身份令牌** | Casdoor 一次换票给三枚 JWT（身份、访问、刷新），同一把钥匙签，`iss` / `aud` / `sub` 相同，只有 `tokenType` 不同（v4.27.1 `object/token_jwt.go`、`token_standard_jwt.go`）。只核签名与 `iss` / `aud` 的话：身份令牌被一次性消费之后，同一次换票的访问令牌还能再登录一次；刷新令牌 24 小时有效，还会被「有效期超限」当成配置故障，任何玩家都能凭自己的刷新令牌刷出故障码与告警。 | `PassportProvider` 要求 `tokenType == "id-token"`，别的类型按凭证无效拒绝，并排在有效期检查之前（2026-11-26）；单测与 mock 的 `ACCESS_TOKEN` / `REFRESH_TOKEN` 种子钉住。应用的 `grantTypes` 只有 `authorization_code`，刷新令牌在 Casdoor 的换票接口也用不了。 |
| **12. 注销后个人信息留在通行证** | 只删游戏账号，Casdoor 里的邮箱、手机号、口令哈希还在：不满足 App Store 5.1.1(v)、Google Play 账号删除要求与个人信息保护法，属于 `AGENTS.md` §9 的「账号真注销」红线。 | 执行者在删账号行之前调 Casdoor 管理接口删用户（§3.5）；生产模式配了通行证却没配管理凭据，login 拒绝启动。 |
| **13. 删用户「看起来成功」其实没删** | 组织开了软删除时 `delete-user` 只打标记仍回 `Affected`；用户名在查与删之间被改时删不到任何行；管理凭据写错时 Casdoor 不报错而是当匿名处理。 | 删除以「删完再按 id 查不到」为准；回 `Unaffected`、删完还查得到、`status=error`、应答里的 id 对不上都回错误并重试，不当成删完（§3.5）。 |
| **14. iOS 跳到 Safari 登录被审核拒** | 用 `Application.OpenURL` 把玩家带到 Safari 去登录 / 注册，App Store 审核指南 4.0（设计）下是常见拒审原因；登记到 `Info.plist` 的 URL scheme 还能被别的 App 或网页用来把游戏拉起来。属于 `AGENTS.md` §9「可能导致不过审」的一票否决项。 | iOS 改用系统认证会话 `ASWebAuthenticationSession`（App 内弹出、回调只交给本 App），去掉 URL scheme 登记（2026-11-26，§7.3）；Android 不受这条审核规则约束，仍用系统浏览器。 |
| **15. 改密之后盗号者照样续期** | Casdoor 改密只作废它自己签的令牌，不通知游戏；游戏的 refresh token 30 天，续一次再续 30 天。账号被盗后玩家改了密码，盗号者的游戏登录还能一直续。 | 刷新前按会话的认证时刻（身份令牌的 iat，跨轮换不变）与通行证里的改密时刻比，改密 / 封禁 / 删除之后整号吊销；吊销失败不续期，通行证故障才放行并计数（§3.6）。已连着 gate 的会话不踢。 |

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
$env:PASSPORT_ADMIN_PASSWORD   = "<local-casdoor-admin-password>"   # 首启写入 built-in/admin 口令
$env:PASSPORT_ORIGIN           = "http://127.0.0.1:8000"
# 端口默认只绑 127.0.0.1;真机局域网联调时再设 $env:PASSPORT_BIND = "0.0.0.0"

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
#     AdminClientId: "xuanming-game-server"          # 要验注销删通行证用户时才配这两行
#     AdminClientSecret: "<第 6 步取到的 clientSecret>"   # 内联明文只在 Mode=dev/test 放行

# 5. 建测试用户。注册项 Email 的规则是 Normal(必须验证邮箱),本地没挂邮件通道时自助注册发不出验证码,
#    用管理员会话调 add-user 建(管理接口建的用户不走验证码):
$s = New-Object Microsoft.PowerShell.Commands.WebRequestSession
$login = @{ application = "app-built-in"; organization = "built-in"; username = "admin"; password = $env:PASSPORT_ADMIN_PASSWORD; type = "login" } | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:8000/api/login -WebSession $s -ContentType application/json -Body $login
$user = @{ owner = "xuanming"; name = "alice"; displayName = "alice"; password = "<test-password>"; email = "alice@example.com"; signupApplication = "app-xuanming-game" } | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:8000/api/add-user -WebSession $s -ContentType application/json -Body $user

# 6. 取服务端专用应用的密钥(首启时 Casdoor 随机生成,不在仓库里),填进第 4 步或生产的密钥管理:
(Invoke-RestMethod -Uri "http://127.0.0.1:8000/api/get-application?id=admin/app-xuanming-server" -WebSession $s).data.clientSecret
```

### 5.3 真实 Casdoor 容器端到端验收记录（2026-11-24）
使用 `deploy/passport/docker-compose.yml`（真实 `casbin/casdoor:v4.27.1` + `mysql:8.0`，配置独立默认子网 `${PASSPORT_SUBNET:-10.249.88.0/24}`）执行真进程验证并销毁临时容器与卷，全部实测通过：
1. **OIDC Discovery & JWKS**：`/.well-known/openid-configuration` 与 `/.well-known/jwks` 正常返回 `issuer=http://127.0.0.1:8000`、`alg=RS256`、`kid=cert-built-in`。
2. **真实注册 + 无密钥 Public PKCE（S256）换票**：调用 `POST /api/signup` 注册新用户后，经 OAuth2 Authorization Code + PKCE（S256，不传 `client_secret`）换票返回 HTTP 200；实测错误 `code_verifier` 被 Casdoor 直接拒绝（HTTP 400 `invalid_grant`）。
3. **真实 `id_token` 声明核验**：
   - 紧凑大小 `1481` 字节（远低于 `idtoken.go` 的 16 KiB 上限）；
   - Header：`{"alg":"RS256","kid":"cert-built-in","typ":"JWT"}`；
   - `iss = "http://127.0.0.1:8000"`、`aud = ["xuanming-game-client"]`；
   - `sub` 为不可变 UUID v4（非可变用户名 `preferred_username`），重复登录 `sub` 一致；
   - 有效期 `exp - iat = 3600s`（1.00h，符合 `expireInHours: 1` 与 `go/login` 2h 硬上限）。
4. **`go/login` `PassportProvider` 真验签与防重放**：实时拉取 Casdoor JWKS 验签通过并产出 `passport_<uuid>`（日志脱敏为 `passport_4035***10`，不泄露原始令牌），同一枚 `id_token` 二次提交被 `ReplayGuard` 拒绝。
5. **沙箱与客户端校验**：
   - `deploy/login-stack.linux/login.yaml` 已增补 `AuthProviders.Passport` 沙箱段并通过 `TestLinuxLoginStackPassportSectionPointsAtSandbox` 契约单测；
   - `mmorpg-client` 已合入道家 Q 版 5 张切图与 `PassportOAuthProtocol.cs` / `PassportSignIn.cs` / `QdaoServerSelectView.Passport.cs` / `Assets/Editor/PassportMobileDeepLinkBuildPostProcess.cs`（iOS `Info.plist` `CFBundleURLTypes` + Android `AndroidManifest.xml` `VIEW/DEFAULT/BROWSABLE` 深度链接注册；2026-11-26 起 iOS 改走 `ASWebAuthenticationSession`，不再登记 URL scheme，见 §7.3）。

### 5.4 审核结论、定案与上线前生产清单（2026-11-26）

Claude Code 对照 Casdoor v4.27.1 源码（`object/token_jwt.go`、`token_standard_jwt.go`、`token_oauth.go`、`init.go`、`application_util.go`、`controllers/user.go`、`routers/authz_filter.go`、`authz/authz.go`）审核了服务端、部署栈与客户端。整体方案（通行证只做身份认证、游戏账号库统一收口、公开客户端 + PKCE、服务端验签 + 一次性消费）是标准做法，保留。

**原「待 Claude Code 确认」的三项，定案：**

| 项 | 定案 |
|---|---|
| 全球账号库是否接纳 `passport_<sub>` | 接纳，不用改（§3.3）。 |
| 注销执行器是否加 Casdoor 删除钩子 | 必须加，已落码（§3.5）。不是可选项：通行证里存着我们自己收的个人信息。选「删除」不选「停用 / 匿名化」：停用后邮箱仍被占用、信息仍在，都不算注销。 |
| `k8s_deploy.ps1` 是否加通行证参数 | 加，已落码：`-LoginPassportIssuer` / `-LoginPassportClientId` / `-LoginPassportEndpoint` / `-LoginPassportAdminClientId`，管理密钥走工作负载密钥目录 `login-passport-admin`（环境变量 `MMORPG_LOGIN_PASSPORT_ADMIN_CLIENT_SECRET`），写法同 Apple 那一组。 |

**审核查出并已修的问题：** §4 第 9～14 项（第 9、10 项与客户端回调过期处理、等待时限 180s → 600s 由并行的会话同日修掉；第 11～14 项本次落码），以及初始化数据缺省不验证邮箱（改为 `Normal`，§3.4）。

**源码核对过、不用改的：** 回调地址匹配（§4 第 10 项，实测为准）；授权码一次性、PKCE 校验通过才接受空 `client_secret`；应用 `grantTypes` 在换票接口与隐式流程都生效（口令直换、隐式、刷新都被挡）；组织缺省 `isProfilePublic=false`、`masterPassword` 为空；签名证书首启时生成，该版本的仓库里没有固定私钥文件；口令连错 5 次冻结 15 分钟（Casdoor 缺省）；LDAP 监听在不配端口时不启动。

**上线前生产清单**（都要真实域名、外部凭据或真机，仓库里做不了）：

1. **管理后台不对公网开放。** Casdoor 的玩家页面与管理后台是同一个端口。生产入口（Ingress / 反向代理）只放行玩家要用的路径（授权、登录、注册、找回密码页面及它们调用的接口、`/.well-known/*`、静态资源），管理页面与其余 `/api/*` 只走内网或跳板机；`built-in/admin` 的口令经密钥管理注入 `initAdminPassword`，首启之后在后台改掉并开两步验证。管理员能导出签名私钥，等于能冒充任何玩家登录。
2. **换票与取公钥全程 HTTPS。** `PASSPORT_ORIGIN`、`AuthProviders.Passport.Issuer`、客户端 `PassportOrigin` 是同一个 `https://` 域名；`runmode` 设 `prod`。
3. **注册防滥用。** 在后台给 `app-xuanming-game` 挂邮件（或短信）通道与人机验证；注册项 `Email` 保持 `Normal`。注册页要展示并勾选《用户协议》《隐私政策》（注册项加 `Agreement` 并填应用的条款地址）。
4. **取出 `app-xuanming-server` 的密钥**放进 login 的密钥（§5.2 第 6 步的接口，或后台应用页），不进仓库、不进 ConfigMap。
5. **组织 `xuanming` 的软删除保持关闭**；`verification_record`、`record` 两张表按隐私政策的保留期定期清理（§3.5）。
6. **静态资源自托管。** `staticBaseUrl` 不配时登录页的图标、默认头像从 `cdn.casbin.org` 取，国内访问不稳，也等于把玩家 IP 给了第三方；指到自己的 CDN，并在后台上传 `deploy/passport/theme/` 的品牌图。
7. **只暴露 8000。** Casdoor 进程还会在容器里起一个 RADIUS 的 UDP 监听（不配端口时是随机端口，代码里关不掉），组织没开 RADIUS 时不认证任何人；K8s 用 NetworkPolicy 只放行 8000。
8. **多副本要共享会话。** `redisEndpoint` 不配时会话在进程内，Casdoor 只能单副本；要高可用先配 Redis，再加副本。
9. **真机回归。** iOS / Android 各走一遍登录、注册、注销重新认证，含应用被系统回收后从回调冷启动的情况（冷启动时内存里的 `code_verifier` 已丢，本次授权作废，玩家重新点登录）。
10. **升级 Casdoor 之前**重跑：回调地址探针（§4 第 10 项）、三种令牌的 `tokenType` 取值、`get-user` / `delete-user` 的应答形状（§3.5 依赖这些，都是 v4.27.1 的行为）。

第二轮审查对这份清单的补充与修正见 §8.4：第 1 条要落成公网入口的接口白名单（不放行 `/api/delete-user` 等，否则玩家能自删通行证用户），login 的 `Endpoint` 因此必须指向内网入口；第 3 条的人机验证、第 5 条的 `record` 清理已在初始化数据里缺省配好（§8.2），`verification_record` 仍要部署侧定时清。

---

## 6. 不在仓库里做的事

账号注销联动已做（§3.5），login 的 K8s 部署参数已做（§5.4）。剩下的都是生产环境的事，按 §5.4 的清单办：

1. **邮件 / 短信发码通道与人机验证**：在 Casdoor 后台配，供应商密钥不进仓库。
2. **Casdoor 自身的生产部署**：`deploy/passport/` 是本机沙盒（端口缺省只绑回环）。生产把 Casdoor 放进受管的 K8s Deployment + 独立库 + HTTPS Ingress，初始化数据以本目录的 `init_data.json` 为底、换掉回调域名之外不改安全相关的项。
3. **Java 版服务端**：`passport` 渠道、令牌类型校验、注销删通行证用户与刷新前的改密核对（§3.6）都只在本仓（Go）实现，Java 仓 `PARITY.md` 登记待做。

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
     - 构造授权 URL 并在桌面/编辑器通过 `Application.OpenURL(url)` 打开系统浏览器，同时在本机 `127.0.0.1:18765` 启动临时 `HttpListener`（600 秒超时——注册要等邮件 / 短信验证码、支持取消、绑定 `127.0.0.1` 绝不监听 `0.0.0.0`；`state` 对不上的请求回「授权页已过期」并继续监听，不让旧标签页或本机别的页面打断本次授权），收到回调后：
       1. 校验 `state` 严格相等（不等即按失败拒收）；
       2. 回显简短的“登录已完成，请返回游戏”HTML 并立即关闭监听；
       3. 用 `UnityWebRequest.Post` 调 `<PassportOrigin>/api/login/oauth/access_token` 换取 `id_token`；
       4. 清理内存中的 `code_verifier` / `state` / `code`，回调 `PlatformSignInResult.Success(idToken, null)`。
     - iOS（2026-11-26）：原生插件 `Assets/Plugins/iOS/MmorpgPassportWebAuth.mm` 用 `ASWebAuthenticationSession` 在 App 内弹系统认证页，回调地址由系统直接交回发起会话的 App，再走与桌面相同的 `state` 校验与换票。不用 `Application.OpenURL` 跳 Safari：App Store 审核指南 4.0 会拒掉「把用户带到 Safari 登录 / 注册」的做法；也因此不需要在 `Info.plist` 登记 URL scheme（别的 App 截不到回调，App 留在前台也不会丢 `code_verifier`）。未在真机 / Xcode 上编译运行过。
     - Android：系统浏览器 + `Application.deepLinkActivated` 接 `xuanming://passport/callback`（构建后处理给主 Activity 加 intent-filter）；别的 App 抢注同名 scheme 拿到的授权码没有 `code_verifier` 换不了票（PKCE）。应用在浏览器期间被系统回收时本次授权作废，玩家重新点登录。未真机验证。
     - 通行证地址（`PassportOrigin`）只有本机回环允许明文 `http`，其余一律要 `https`（授权码、`code_verifier` 与令牌都经它走）。
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

---

## 8. 独立审查（2026-11-26 第二轮：不沿用现有做法，按行业通行标准重看）

负责人要求不受现有方案影响再查一遍。本轮由另一个 Claude Code 会话做，逐项读了 Casdoor v4.27.1 源码（`raw.githubusercontent.com/casdoor/casdoor/v4.27.1/`，下文的「文件:行」都指这个版本）、`go/login` 与 `mmorpg-client` 的现有代码。没有编译，没有起容器。

### 8.1 结论

整体架构不用改：通行证只管「你是谁」，游戏账号库管「你有什么」，两边只凭标准 OIDC 令牌衔接，是各家官方通行证的通行做法；公开客户端 + PKCE、服务端验签 + 一次性消费、iOS 用系统认证会话也都是标准做法。`go/login` 的验签、配置门禁、删通行证用户三段代码逐行核过，没有发现缺陷。

与行业标准的差距：2 处本轮已改（§8.2）；「改密 / 封禁之后游戏登录不失效」负责人确认后已落码（§3.6）；4 处要负责人拍板或等上线资料（§8.3）；另有 7 条生产清单补充（§8.4，其中第 1 条不做就有实际漏洞）。

### 8.2 本轮已改（只动 `deploy/passport/init_data.json`，未起容器验证）

| 问题 | 依据 | 改法与代价 |
|---|---|---|
| **注册、发验证码、登录都没有人机验证** | 游戏应用没挂任何人机验证提供方，而 Casdoor 的判定是「应用没挂就永远不要求」（`object/check.go:1024`）。生产一旦挂上邮件 / 短信通道，三个入口都对脚本敞开。发码频率只按「来源 IP + 用户」限 60 秒一条（`object/verification.go:83`），不按收件人限：换 IP 就能对同一个邮箱 / 手机号连发（骚扰、刷短信费），也能批量注册小号。原清单把它放在「上线前去后台配」，漏配没有任何报错。 | 应用的 `providers` 挂内置的 `provider_captcha_default`（图片验证码，Casdoor 首启自带，`owner` 是 `admin`，各组织的应用都能引用——`object/provider.go:194`），规则 `Always`。不选 `Dynamic`：它只在「该用户口令连错到上限」后才要验证码，注册发码时还没有用户，等于不拦。不选 `Internet-Only`：它按来源 IP 是否公网判断，反向代理没被认成可信代理时所有请求都成了内网地址，验证码整体失效且没有报错。**代价**：用脚本直接调接口登录 / 发码会被拦（浏览器里人工操作不受影响），脚本联调时用管理员会话把这条规则临时改成 `None`。图片验证码强度一般，生产换成行为验证码（v4.27.1 自带阿里云、极验、Turnstile 的对接），规则仍用 `Always`。 |
| **接口审计记录永久保留** | `record` 表记着账号与来源 IP，没设保留期就一直留，账号注销后仍在。原清单写「生产定期清」，靠人工。 | 组织加 `recordRetentionDays: 180`，Casdoor 自带的清理任务按它删（`object/record_cleanup.go`）。180 天是《网络安全法》第二十一条「网络日志留存不少于六个月」的下限，也符合个人信息保护法第十九条「保存期限为必要的最短时间」；隐私政策里的日志保留期要写成一致。`verification_record` 没有自带清理，见 §8.4。 |

两项都只在全新初始化时生效（`initDataNewOnly = true`）：已经初始化过的库不会跟着变，要在后台手动改，或重建卷。

### 8.3 待负责人拍板

**1. 通行证改密 / 找回密码 / 封禁之后，游戏里的登录不失效。** 2026-11-26 负责人确认后已落码，做法与取舍见 §3.6（未编译、未运行）。原先建议的「按账号记改密时刻水位」没有采用：它要多一个 Redis 键，发现改密时还会把改密后新登录的会话一起吊销；改为按会话的认证时刻比。仍没做的两样（已连着 gate 的会话立刻踢下线、改密实时推送），见 §3.6「做不到的」。

**2. 注册没有《用户协议》《隐私政策》的展示与勾选（提审前必须有）。**

- 现状：通行证注册项没有 `Agreement`；`mmorpg-client` 全仓搜不到协议 / 隐私相关界面，首次启动也没有隐私同意弹窗。
- 属于 `AGENTS.md` §9「可能导致不过审」的一票否决项：国内安卓商店与 App Store 都查首启隐私弹窗与注册时的明示同意（个人信息保护法第十三、十四、十七条）。
- 要先有两份正式文本和可访问的地址（法务 / 负责人）。有了之后：应用填 `termsOfUse`、`privacyPolicy`，注册项加必填的 `Agreement`；客户端首启弹窗是另一项界面工作（要给 Codex 出图）。原 §5.4 第 3 条把它当成「上线前配一下」，这里提升为**开放注册的前置条件**：两个地址没填之前，不要给应用挂邮件 / 短信通道。

**3. 发布包也能用启动参数改登录网关和通行证地址。**

- 现状：`ClientLaunchArguments` 的 `-gateway`、`-passport-origin`、`-passport-client-id`、`-passport-redirect-uri` 不区分开发包与发布包；`ClientSettings` 还会读 PlayerPrefs（Windows 上是当前用户可写的注册表）。
- 后果：一个带参数的快捷方式（「登录器」「加速器」常见的分发形式，杀毒软件不报）就能让正版客户端把玩家带到钓鱼站去输通行证口令，或把口令登录发给假网关。
- 通行做法：生产地址编进发布包，覆盖只在编辑器与开发包生效。改动很小（几行 `#if UNITY_EDITOR || DEVELOPMENT_BUILD`），但现有冒烟脚本可能正用这些参数启动非开发包，所以放到「定发布构建配置」时一起改。发布包的缺省网关现在还是 `http://127.0.0.1:8081`、通行证地址缺省为空，说明发布构建配置本来就还没做。

**4. Android 回调用自定义协议头 `xuanming://`，别的 App 可以注册同名协议头。**

- PKCE 只保证「谁发起、谁换票」，拦不住恶意 App 自己发起一次授权（客户端 ID 是公开的）、让玩家在真的通行证页面登录、再截走回调并以玩家身份登录（RFC 8252 §8.6）。标准做法是 Android App Links：回调改成 `https://<通行证域名>/...`，由系统按 `assetlinks.json` 核对归属，别的 App 接不到（RFC 8252 §7.2）。
- 需要正式域名和签名证书指纹，上线前做；同时把「跳系统浏览器」换成应用内的 Custom Tabs（不离开游戏，不会因为被系统回收而丢掉本次授权）。iOS 已经是系统认证会话，没有这个问题。

**5. 桌面回调固定端口 18765（小问题，有反馈再做）。**

- 两个客户端同时停在「等待授权」时，后一个起不了监听（多开玩家会遇到）；本机任何程序占住这个端口，通行证登录就用不了。RFC 8252 §7.3 的做法是每次随机端口，但 Casdoor 对回调地址是精确匹配，做不到。折中：登记 3～5 个固定端口，客户端依次尝试。

### 8.4 生产清单补充（接 §5.4）

1. **公网入口按白名单放行，白名单里不能有 `/api/delete-user`、`/api/update-user`（必须做）。** §5.4 第 1 条只说了「管理后台不对公网开放」，没说清楚的是：Casdoor 的权限模型允许任何登录用户对**自己**调用一批接口（`object/init.go:534` 的自匹配条款，`authz/authz.go:281` 的名单里有 `/api/delete-user`），`controllers/user.go:568` 对「删自己」没有别的检查。玩家用自己的通行证令牌（客户端换票时就拿得到，客户端 ID 是公开的）调一次 `POST /api/delete-user`，通行证用户立刻被删：
   - 绕过游戏的 7 天冷静期；
   - 游戏账号 `passport_<sub>` 变成孤儿：`sub` 没了，谁也登不上，角色、充值、实名记录都留在游戏库里，玩家没法再从游戏内发起注销；
   - 盗号者拿到口令或一小时内的访问令牌就能永久毁号，冷静期本来是给找回留的。

   Casdoor 对「任何人」放行的接口有 131 条（`authz/authz.go`，含 `/scim`、`/cas`、`/api/oauth/register`、`/api/init-admin-password`、`/api/metrics` 等），玩家用得到的只有十几条。公网入口（Ingress / 反向代理）只放行下面这些，其余一律拒绝。**这份名单是按 v4.27.1 源码推的草案，未实测**：预发环境挂上名单后，把登录、注册、找回密码三条流程各走一遍，页面缺哪个接口再加（缺了页面会直接报错，方向是安全的）。
   - 页面与静态资源：`GET` 非 `/api/`、非 `/scim`、非 `/cas` 的路径（登录 / 注册 / 找回密码页面与管理后台是同一套前端文件，没有接口就拿不到任何数据）。
   - `GET /.well-known/openid-configuration`、`GET /.well-known/jwks`。
   - 页面用：`GET /api/get-app-login`、`GET /api/get-application`、`GET /api/get-account`、`POST /api/login`、`POST /api/signup`、`POST /api/send-verification-code`、`POST /api/verify-code`、`POST /api/set-password`、`GET /api/get-email-and-phone`、`GET /api/get-captcha`、`GET /api/get-captcha-status`、`POST /api/verify-captcha`。
   - 游戏客户端用：`POST /api/login/oauth/access_token`。
2. **`AuthProviders.Passport.Endpoint` 必须配成内网入口。** 公网入口不放行管理接口之后，login 删通行证用户与刷新前核对（§3.6）调的 `/api/get-user`、`/api/delete-user` 就不能再走公网的 `Issuer`：`Endpoint` 留空时所有通行证账号的注销都卡在「执行中」直到 24 小时告警，刷新前的核对也全部按故障放行（改密再也踢不掉人，只在 `login_passport_session_check_total{result="error"}` 上看得到）。生产模式 `Endpoint` 只收 `https://`，所以内网入口要有证书（内部 Ingress 或服务网格）。
3. **`verification_record` 定时清理。** 发过的验证码（含收件邮箱 / 手机号、来源 IP）v4.27.1 里没有任何清理代码。部署侧加定时任务按保留期删；表结构以实际库为准，未实测。
4. **撞库拦截按数据开。** 组织的 `securityPolicy.stuffingFailureThreshold` / `stuffingBlockMinutes`：同一 IP 10 分钟内有 N 个不同账号登录失败就封这个 IP（`object/password_security.go:98`），缺省关。没有直接开：运营商大量用户共用出口 IP，阈值定低了会误封一片；有了登录失败的真实数据再定。人机验证加单账号连错 5 次冻结 15 分钟已经在挡。
5. **口令哈希保持 bcrypt 可以；想更稳就趁没有存量用户换 `argon2id`。** Casdoor 的 bcrypt 写死 cost 10（`cred/bcrypt.go:13`），正好是 OWASP 对 bcrypt 的下限；OWASP 对新系统的首选是 Argon2id，v4.27.1 支持（`passwordType: "argon2id"`）。名字写错会被当成不哈希、口令明文入库（`cred/manager.go:42` 回空，`object/user_cred.go:59` 跳过），改的时候逐字核对。Argon2id 每次校验约占 64 MiB 内存，要给容器留够。上线后再换只能等用户下次登录时重算。本轮没换：没有容器实测，不为它动一条已经实测通过的链路。
6. **镜像按摘要固定。** 生产清单里 `casbin/casdoor:v4.27.1` 写成 `@sha256:…`；通行证是全链路最敏感的第三方组件。
7. **签名私钥在库里是明文**（`cert` 表）。通行证库的备份等同于「能冒充任何玩家登录」的钥匙：备份加密、限制读库权限。

### 8.5 核对过、没有问题的（v4.27.1 源码）

- **自助改资料不能提权。** 初始化数据里组织没写 `accountItems` 时会补缺省规则（`object/init_data.go:475`、`object/init.go:59`）：`ID` 不可改（管理员也不行，所以 `sub` 稳定），用户名、管理员标记、封禁标记只有管理员能改。组织的规则要是被清空，Casdoor 对任何字段都放行（`object/organization.go:483`），所以后台不要删这些规则。
- **注册不进管理组织。** 内置组织 `built-in` 里的任何用户都是全局管理员（`object/user.go:1775`），内置应用缺省开着注册（`object/init.go:310`），但注册接口对 `built-in` 组织一律拒绝，也不接受「应用与组织不配套」的注册（`object/application_util.go:905`）。
- **验证码有次数限制。** 同一目标错 5 次冻结，另按来源 IP 计（`object/verification.go:467`、`object/verification_ip.go:120`）。计数在进程内存里，多副本时各算各的；6 位码 10 分钟有效，影响可以忽略。
- **限速绕不过。** 来源 IP 只在直连对端是内网 / 回环（或在 `trustedProxies` 名单里）时才信 `X-Forwarded-For`，并从右往左取第一个不可信地址（`util/request.go:37-101`）。
- **共用电脑不串号。** 游戏应用 `enableSigninSession: false` 时授权码流程不留通行证会话（`controllers/auth.go:248`），网吧里后一个人点「官方账号登录」不会直接进前一个人的账号。
- **应用凭据的权限限于自己的组织**（`authz/authz.go:210-221`、`object/user_util.go:1187`），`app-xuanming-server` 管不到 `built-in`。
- **注销时删通行证用户（§3.5）依赖的三处行为与源码一致**（上一轮标了「未核实」）：应用凭据按 `userId` 查用户走 `controllers/user.go:185-267`，同组织的应用凭据通过 `object/check.go:467` 的权限判断；用户不在时不论凭据对错都回 `data: null`，用户在而凭据不对回错误；初始化数据里不写 `clientSecret` 的应用会自动生成密钥（`object/application.go:570`）。应答的具体形状仍要真容器跑一次才算数。

### 8.6 体验上与大厂做法不同、但不建议现在改的

- 大厂的自有通行证多数在游戏里直接出登录 / 注册界面，不跳浏览器。跳浏览器是 OAuth 对原生应用的安全建议（RFC 8252），实现最省、风险最低；代价是全屏游戏里要切出去，Android 上游戏可能被系统回收。等上线后看注册转化数据，再决定要不要做游戏内界面（那时人机验证要嵌网页，界面要给 Codex 出图）。
- 国内大厂的主注册方式是手机号 + 短信验证码。现在只开了邮箱；要开手机号时，同批把组织的 `countryCodes` 限到上线地区、配上 `securityPolicy` 的每号每小时 / 每天条数上限（`object/sms_security.go`），防国际短信盗刷。

# 官方通行证与账号注册接入设计（Casdoor OIDC）

状态：2026-11-24 已完成服务端 OIDC 接缝、独立 Casdoor 部署栈、Unity 道家 Q 版客户端 UI/PKCE 与移动端深度链接构建后处理，并通过真实 Casdoor v4.27.1 容器 → `go/login` `PassportProvider` 端到端实测验证。2026-11-26 Claude Code 对照 Casdoor v4.27.1 源码审核，结论与定案见 §5.4；同日落码（未编译、未运行，验证命令见 `docs/PROGRESS.md` 同日「官方通行证审核」条目）：只收身份令牌（§4 第 11 项）、账号注销时删除通行证用户（§3.5）、初始化数据改为注册必须验证邮箱并新增服务端专用应用、K8s 部署参数、iOS 改用系统认证会话（§4 第 14 项）。同日第二轮独立审查（不沿用现有做法、按行业标准重看）的结论见 §8：初始化数据缺省开人机验证、审计记录保留 180 天（已改，未起容器）；玩家可自删通行证用户、注册缺协议勾选等待办见 §8.3、§8.4。同日负责人批准全做之后落码（未编译、未运行，验证交 Gemini，交接单 `docs/handoff/passport-hardening-handoff-20261126.md`）：§3.6 改密 / 封禁 / 删除之后登录与刷新时吊销改密之前的会话并踢下线；§3.7 通行证回调实时推送；§3.8 客户端正式包锁地址、桌面多端口回调、安卓 App Links；§3.9 首启隐私弹窗与注册协议勾选；§3.10 生产部署清单（白名单入口、网络隔离、验证码记录清理、按环境渲染初始化数据）。

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

### 3.6 通行证改密 / 封禁 / 删除之后吊销游戏登录并踢下线（2026-11-26）

**为什么要做**：Casdoor 改密只作废它自己签的令牌和会话（`controllers/user.go` 的 `SetPassword` 末尾），不通知游戏。游戏的 refresh token 有效 30 天，每次续期再续 30 天。账号被盗以后玩家去通行证改了密码，盗号者手里的游戏令牌照样能一直续下去，已经在游戏里的连接也不会断。各家通行证的通行做法是改密即全端下线；封禁、删除同理。

**判定：按会话的认证时刻比，不按账号记水位。**

1. 登录时把身份令牌的 `iat`（通行证的时钟）写进游戏令牌载荷 `TokenData.IdentityIssuedAt`（`AuthResult.IdentityIssuedAt` → `LoginLogic.issueTokens` → `token.Manager.Issue`），同一家族轮换时原样带下去（`token.go` 的 `rotate`）。
2. 查通行证：`PassportAdmin.LookupUser`（`GET /api/get-user?userId=<sub>`）回这名用户此刻的状态（被删 / 被标记删除、被封、最近一次改密时刻）；`PassportUserStatus.SessionVerdict(iat)` 是纯判定：被删记 `user_gone`，被封记 `user_forbidden`，改密时刻不早于这次登录的 `iat` 记 `password_changed`，其余照常。
3. 落到令牌上（`pkg/passportsession` 的 `settle`，三个入口共用）：被删、被封吊销账号的全部令牌（`RevokeAll`）；改过密码只吊销身份令牌签发时刻不晚于改密时刻的家族（`token.Manager.RevokeIssuedBefore`，一段 Lua 按载荷筛、一次原子提交），改密之后重新登录的设备不受影响。吊销了东西就把账号名下在线的角色踢下线（`svc.KickAccountPlayers`：读角色目录、按会话踢 gate 并按正常登出收尾，同注销申请时的踢线），客户端收到新提示码 `kLoginPassportCredentialChanged`「账号在官方通行证修改了密码或已被停用，请重新登录」，不自动重连。每次吊销都在 `settle` 里记一行 INFO `[passport] sessions revoked: phase=… account=…`（安全审计：登录时顺带吊销旧会话的那一种，别处没有日志）。
4. 三个入口：
   - **登录时**（`passportsession.CheckLogin`，`loginlogic.go` 认证之后、签发令牌之前）：身份令牌签发在改密之前（被截获、还没用过的旧令牌）、用户被封被删，拒绝这次登录（回新提示码，网关回 401，客户端说清原因）。不论放不放行，改密之前的旧登录都在这里吊销：玩家改完密码回来登录，盗号者的会话当场失效、在线的被踢下线。
   - **刷新时**（`passportsession.CheckRefresh`，轮换之前）：这一家族的登录早于改密、用户被封被删，不续期，吊销并踢线；回与令牌失效相同的 `kLoginAccountNotFound`，客户端照常重登。
   - **通行证回调时**（§3.7）：改密、改资料、删用户的那一刻就处理，不用等玩家登录或刷新。
5. 这和 Firebase 的 `tokensValidAfterTime`、Microsoft Graph 的 `signInSessionsValidFromDateTime` 是同一个思路：会话带着自己的认证时刻，拿它和用户的「此后才有效」时刻比。

**为什么这样定：**

- 两个时刻都取自通行证自己的时钟，不受 login 与通行证之间的时钟偏差影响。`TokenData.CreatedAt` 不能拿来比：它每次轮换都更新，用的又是 login 的时钟。
- 按会话的认证时刻判，改密之后重新登录的会话不会被误吊销。另一种做法是按账号记一个「上次看到的改密时刻」水位：要多一个 Redis 键，发现改密的那一刻还会把改密后新登录的会话一起吊销（审查稿原先建议的是它，没采用）。
- 签发时刻跨轮换不变。通行证故障期间核对不了、照常续期，续出来的令牌在下一次核对时照样判得出来，没法借一次续期把改密之前的登录变成改密之后的。
- 刷新时的核对放在轮换之前。吊销失败就不续期（计 `revoke_failed`），旧令牌还在，下一次刷新再核对、再吊销。如果放在轮换之后，新的一对已经写下，客户端重试走宽限窗口的重放会原样拿到它，绕过核对。
- 只在确实吊销了东西时踢线：登录时如果改密之前的会话早就清掉了，不再踢，免得把玩家改密之后在别的设备上的会话也踢掉。被封、被删一律踢。
- 同一秒里改密与签发分不出先后，按改密在后处理。注册不写改密时刻；改完密码再回游戏登录，不会和改密落在同一秒。
- 吊销「改密之前的家族」用 Lua 扫账号索引（每账号最多 32 个家族），按载荷的 `identity_issued_at` 筛，与整账号吊销同一套归属核对，数据不对整个报错、一个都不删。

**故障与代价：**

- 通行证或网络出故障、应答认不出：登录、刷新照常放行，计 `login_passport_session_check_total{phase,result="error"}` 并记 ERROR，不让通行证故障把在线玩家都踢下线。持续出错说明改密已经踢不掉人：`deploy/k8s/login-alerts.yaml` 的 `LoginPassportSessionCheckFailing`（10 分钟内一半以上的核对失败，持续 10 分钟）报 warning，`tools/scripts/tests/gate_pick_alerts_contract.tests.ps1` 钉住指标名与取值。
- 通行证账号每次登录、每次刷新（约 2 小时一次）多一次内网调用，时限 1 秒（`lookupTimeout`，login 的 RPC 超时缺省 4 秒）；登录时还多一次 Lua（扫旧家族）。踢线最多 5 秒，与请求的取消脱钩。
- 没配管理凭据（只有 dev/test 允许）时不核对，启动时记 ERROR。生产模式配了通行证就必须配管理凭据（§3.2）。
- 管理接口走 `Endpoint`，生产必须指向集群内入口（§3.10）。指错了，每次核对都按故障放行，只在上面的告警里看得到。
- 这个字段加上之前签的通行证令牌 `IdentityIssuedAt` 为 0，按最早算：玩家改过密码的话要重新登录一次。项目未上线，不受影响。

**仍然做不到的：**

- Casdoor 后台的「强制下线」（`/api/logout-user`）不改 `lastChangePasswordTime`，判不出来；要踢盗号者，请玩家或客服改密（会触发回调与上面的核对）。
- 回调没开（§3.7）时，从改密到被发现，最长是盗号者下一次刷新或玩家下一次登录之间的时间（access token 最长 2 小时）。

### 3.7 通行证回调：改密、封禁的那一刻就吊销并踢线（2026-11-26）

**链路**：Casdoor 回调（webhook）→ 网关 `POST /internal/passport/events`（`PassportEventController`）→ login `LoginPreGate.HandlePassportEvent` → `passportsession.HandleEvent` → 同 §3.6 的 `settle`。

1. **回调只用来找人，不信内容。** 回调体是 Casdoor 的一条操作记录（`object/record.go` 的 `Record`，`isUserExtended` 时多一个 `extendedUser`）。login 从里面取出用户 id 或「组织/用户名」：改密（`set-password`）时记录的组织与用户就是目标用户；后台改资料（`update-user`）时记录的是操作人，目标在请求体原文 `object` 里；管理组织 `built-in` 的人一律不核对，一次最多 4 个。然后逐个现查通行证（`LookupUser` / `LookupUserByName`），按查到的状态处理。回调是伪造、重复、乱序的都无所谓：只会多查几次通行证，改不了任何人的状态。所以不需要签名，固定请求头密钥只是挡掉随手的请求。
2. **网关**：请求头 `X-Passport-Webhook-Secret` 按 SHA-256 摘要常量时间比较 `passport.webhook-secret`（至少 32 字节；没配就回 404，入口关着）；请求体限 64 KiB；原样转给 login。状态码给 Casdoor 的重投用：200 处理完；400 请求体不对，不必重投；401 密钥不对；503 login 暂时处理不了。网关的 Ingress 只路由 `/api`，这个入口公网够不着。
3. **login**：令牌存在全地域共用的登录域，任一 login 都能处理，网关不按区选路、可以重试。核对不了或吊销失败回 `Unavailable`（网关 503，Casdoor 按 `maxRetries: 10`、指数退避重投）；格式不对回 `InvalidArgument`。请求体带玩家邮箱、手机号，`HandlePassportEvent` 进了请求体日志屏蔽名单（`internal/rpclogging`）。
4. **Casdoor 侧**（`deploy/passport/init_data.json` 的 `webhooks`）：回调挂在 `built-in` 组织下、`singleOrgOnly: false`。原因是 v4.27.1 只让 `built-in` 的回调连内网地址（`object/webhook_util.go` 的 `sendWebhook`：别的组织的回调走「只许公网」的 HTTP 客户端，防组织管理员拿回调探内网）。事件是 `set-password`、`update-user`、`delete-user`。仓库里缺省关着（`isEnabled: false`、地址与密钥是占位）；生产由 `deploy/k8s/passport/render-init-data.ps1` 填地址与密钥并打开。
5. **部署**：网关的密钥经 `k8s_deploy.ps1` 工作负载密钥目录 `gateway-passport-webhook` 注入（部署机设 `MMORPG_GATEWAY_PASSPORT_WEBHOOK_SECRET` 才注入，没设就不注入、入口关着），与 Casdoor 回调请求头是同一个值。
6. **删用户**：通行证里删掉用户之后，回调只剩组织与用户名，已经查不到这是谁（`sub` 随用户一起没了），这一条按 `ignored` 计；这个人的游戏令牌在下一次刷新或登录时按 `user_gone` 吊销（§3.6）。

### 3.8 客户端加固：正式包锁地址、桌面多端口回调、安卓 App Links（2026-11-26）

1. **正式包只认构建时写进包里的地址。** 原来网关和通行证地址都能被启动参数（`-gateway`、`-passport-origin` 等）和本机设置（PlayerPrefs，Windows 上是当前用户可写的注册表）覆盖：一个带参数的快捷方式（「登录器」「加速器」常见的分发形式，杀毒软件不报）就能让正版客户端把玩家带到钓鱼页去输通行证口令，或把口令登录发给假网关。现在：
   - 地址放在 `Assets/Resources/ClientEndpoints.json`（`ClientEndpoints`），仓库里是本机联调的值；`tools/build_client.ps1 -GatewayUrl … -PassportOrigin … -TermsUrl … -PrivacyUrl …` 出正式包时写进去、构建完还原。
   - 启动参数与本机设置只在编辑器与开发包里生效（`ClientSettings.EndpointOverridesAllowed`，按 `UNITY_EDITOR || DEVELOPMENT_BUILD` 条件编译）。
   - `ClientEndpointsBuildGuard`（构建前检查，所有平台、所有出包入口都经过）不让正式包带着回环地址、明文 http 或缺协议地址出去；开发包不查。
2. **桌面回调登记五个端口。** 原来只有 `127.0.0.1:18765`：多开的第二个客户端起不了监听，本机任何程序占住这个端口，通行证登录就用不了。RFC 8252 §7.3 建议每次随机端口，但 Casdoor 对回调地址精确匹配，做不到。现在通行证登记 18765~18769 五个，客户端先占端口再拼授权地址（`PassportOAuthProtocol.LoopbackRedirectCandidates`），授权与换票都用实际监听的那一个；开发时改过回调地址就只试它自己。
3. **安卓正式包改用 App Links。** 自定义协议头 `xuanming://` 别的 App 也能注册：PKCE 只保证谁发起谁换票，拦不住恶意 App 自己发起一次授权、让玩家在真的通行证页面登录、再截走回调（RFC 8252 §8.6）。现在通行证是 https 时，安卓的回调是 `https://<通行证域名>/passport/app-callback`（`PassportOAuthProtocol.AppLinkRedirectUri`），构建后处理给主 Activity 登记带 `autoVerify` 的 intent-filter，系统按通行证域名下的 `/.well-known/assetlinks.json` 核对这个链接归本 App；自定义协议头只在开发包里登记（本机联调的 http 通行证用它）。`assetlinks.json` 与回调兜底页由通行证的公网入口发布（§3.10），通行证里登记这条回调由 `render-init-data.ps1` 加。iOS 仍是系统认证会话（回调只交给发起会话的 App），不受影响。没改成 Custom Tabs：系统浏览器同样符合 RFC 8252，Custom Tabs 只是体验更好，要加安卓依赖，留到看注册转化再定。

### 3.9 《用户协议》《隐私政策》：首启弹窗与注册勾选（2026-11-26）

国内应用商店与 App Store 都查：首次运行先弹出协议与隐私政策、由玩家主动点同意，同意之前不收集个人信息；注册时明示同意（个人信息保护法第十三、十四、十七条）。属于 `AGENTS.md` §9「可能导致不过审」的一票否决项。

1. **客户端首启弹窗**（`QdaoServerSelectView.Privacy.cs`、`PrivacyConsent`）：没同意当前版本就盖在登录界面最上层，选服、登录、注册都点不了，Esc 关不掉；「阅读《用户协议》」「阅读《隐私政策》」用系统浏览器打开构建时写进包里的地址；「不同意」第一次只说明后果，再点才退出游戏，不反复弹窗；同意的版本号记在本机（不是个人信息），协议改版把 `PrivacyConsent.CurrentVersion` 加一，所有玩家下次启动重新同意。复用登录界面现有的面板与按钮美术、运行期挂载，不需要新图，不改烘焙的 Prefab。区服列表与公告照常加载（匿名，不涉及个人信息）。
2. **通行证注册页**：注册项加必填的 `Agreement`（勾选框缺省不勾）；应用的 `termsOfUse`、`privacyPolicy` 由 `render-init-data.ps1` 按本环境填。
3. **正式包必须有协议地址**：`ClientEndpointsBuildGuard` 拦缺地址的正式包，`render-init-data.ps1` 也是必填参数。协议正文要法务出，仓库里没有。

### 3.10 生产部署清单（`deploy/k8s/passport/`，2026-11-26）

审查时查出的几条生产问题（§8.4）原本只写在清单里靠人办，现在落成清单与脚本：

1. **公网入口按白名单放行**（`public-entry.yaml`，nginx）。Casdoor 的权限模型允许任何登录用户对自己调 `/api/delete-user`、`/api/update-user`（`object/init.go` 的自匹配条款）：不挡的话，玩家用自己的通行证令牌就能删掉自己的通行证用户，绕过 7 天冷静期、游戏账号变成谁也登不上的孤儿。默认全拒，只放行玩家要用的十几个接口与只读页面；登录、注册、发验证码、找回密码、换票按来源地址每分钟限速（口令哈希是 argon2id，每次校验约 64 MiB，不限速就是现成的内存放大器）。用独立的 nginx 而不是 Ingress 控制器的注解：名单在一处、与控制器无关、契约测试能钉住（`tools/scripts/tests/passport_k8s_contract.tests.ps1`）。
2. **集群内入口给 login**（同一个 nginx 的 8081，内部 Ingress）：只有验签公钥与 `get-user`、`delete-user`。login 生产模式只收 https 的 `Endpoint`，所以走带证书的内部 Ingress，不直连 `casdoor:8000`。
3. **网络隔离**（`networkpolicy.yaml`）：Casdoor 只收代理的 8000（它在容器里起的 RADIUS UDP 监听也就够不着了）；代理只收 Ingress 控制器的流量。
4. **初始化数据按环境渲染**（`render-init-data.ps1`）：以仓库里的 `init_data.json` 为底，只填 App Links 回调、协议地址与回调地址 / 密钥，别的安全配置原样不动；输出 Secret（密钥不进 ConfigMap），用 JSON 解析改字段而不是文本替换。
5. **验证码记录定时清理**（`verification-record-cleanup.yaml`）：Casdoor 没有任何清理 `verification_record` 的代码、删用户也带不走它；每天删 180 天以前的，用只有这张表删除权限的库账号。
6. **镜像按摘要固定**：Casdoor、MySQL、nginx 都写 `@sha256:`（2026-11-26 向 registry-1.docker.io 查的），本机 compose 与生产清单是同一个摘要（契约测试核对）。
7. **初始化数据的安全缺省**（`deploy/passport/init_data.json`，契约测试钉住）：口令哈希 `argon2id`（OWASP 对新系统的首选，项目没有存量用户、可以清库，趁现在换；名字写错会被当成不哈希）；人机验证 `Always`；撞库拦截（同一来源 10 分钟内 30 个不同账号登录失败封 15 分钟，按运营商大出口共用地址留了余量）；短信每号每小时 5 条、每天 10 条、同号段每小时 100 条、只发国内号码、每天 1 万条告警（现在只开了邮箱注册，开手机号时直接生效）；`countryCodes` 只有 `CN`。

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
| **15. 改密之后盗号者照样续期、照样在线** | Casdoor 改密只作废它自己签的令牌，不通知游戏；游戏的 refresh token 30 天，续一次再续 30 天，已在游戏里的连接也不断。 | 登录、刷新、通行证回调三处按会话的认证时刻（身份令牌 iat，跨轮换不变）与通行证里的改密时刻比：只吊销改密之前的会话，被封被删吊销全部，吊销了就踢下线（§3.6、§3.7）。 |
| **16. 玩家自删通行证用户绕过冷静期** | Casdoor 允许登录用户对自己调 `/api/delete-user`（`object/init.go` 的自匹配条款）：冷静期被绕过，游戏账号变成谁也登不上的孤儿。 | 公网入口按白名单放行，`delete-user`、`update-user` 与全部管理接口不放行；login 的管理调用走集群内入口（§3.10）。 |
| **17. 带参数的快捷方式把正版客户端带到钓鱼站** | 启动参数与本机设置能覆盖网关和通行证地址。 | 正式包只认构建时写进包里的地址，覆盖只在编辑器与开发包里生效；构建检查不让正式包带着回环地址或明文 http 出去（§3.8）。 |
| **18. 别的 App 抢注 `xuanming://` 截走安卓的授权回调** | 自定义协议头谁都能注册（RFC 8252 §8.6）。 | 安卓正式包用 App Links（`assetlinks.json` 核对归属），自定义协议头只在开发包里登记（§3.8）。 |
| **19. 没有首启隐私弹窗与注册协议勾选** | 国内应用商店与 App Store 都查，属 `AGENTS.md` §9 一票否决项。 | 首启弹窗 + 注册页必填勾选；正式包缺协议地址出不了包（§3.9）。 |

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

第二轮审查对这份清单的补充已落成 `deploy/k8s/passport/` 的部署清单与初始化数据缺省（§3.10）：第 1 条落成公网入口的接口白名单与集群内入口，第 3 条的人机验证、撞库与短信限额、第 5 条的 `record` 保留期与 `verification_record` 定时清理、第 7 条的网络隔离都有了；仍要人办的见 `deploy/k8s/passport/README.md`「上线前还要人办的」。

---

## 6. 不在仓库里做的事

代码与部署清单都在仓库里（§3.5~§3.10）。剩下的要真实域名、外部凭据、法务或真机，按 `deploy/k8s/passport/README.md` 办：

1. **邮件 / 短信发码通道、行为验证码**：在 Casdoor 后台配，供应商密钥不进仓库。
2. **《用户协议》《隐私政策》正文与页面**：法务出，地址填进客户端正式包与 `render-init-data.ps1`。
3. **域名、证书、安卓签名指纹**：填 `public-entry.yaml` 的两个 Ingress 与 `assetlinks.json`。
4. **Java 版服务端**：`passport` 渠道、令牌类型校验、注销删通行证用户、登录与刷新前的核对与踢线、回调入口（§3.6、§3.7）都只在本仓实现，Java 仓 `PARITY.md` 登记待做；客户端（§3.8、§3.9）两版共用。

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

与行业标准的差距：2 处本轮已改（§8.2）；§8.3 的 5 处与 §8.4 的 7 条，负责人 2026-11-26 批准全做，已落码（§3.6~§3.10，未编译、未运行），各条的去向见下。

### 8.2 本轮已改（只动 `deploy/passport/init_data.json`，未起容器验证）

| 问题 | 依据 | 改法与代价 |
|---|---|---|
| **注册、发验证码、登录都没有人机验证** | 游戏应用没挂任何人机验证提供方，而 Casdoor 的判定是「应用没挂就永远不要求」（`object/check.go:1024`）。生产一旦挂上邮件 / 短信通道，三个入口都对脚本敞开。发码频率只按「来源 IP + 用户」限 60 秒一条（`object/verification.go:83`），不按收件人限：换 IP 就能对同一个邮箱 / 手机号连发（骚扰、刷短信费），也能批量注册小号。原清单把它放在「上线前去后台配」，漏配没有任何报错。 | 应用的 `providers` 挂内置的 `provider_captcha_default`（图片验证码，Casdoor 首启自带，`owner` 是 `admin`，各组织的应用都能引用——`object/provider.go:194`），规则 `Always`。不选 `Dynamic`：它只在「该用户口令连错到上限」后才要验证码，注册发码时还没有用户，等于不拦。不选 `Internet-Only`：它按来源 IP 是否公网判断，反向代理没被认成可信代理时所有请求都成了内网地址，验证码整体失效且没有报错。**代价**：用脚本直接调接口登录 / 发码会被拦（浏览器里人工操作不受影响），脚本联调时用管理员会话把这条规则临时改成 `None`。图片验证码强度一般，生产换成行为验证码（v4.27.1 自带阿里云、极验、Turnstile 的对接），规则仍用 `Always`。 |
| **接口审计记录永久保留** | `record` 表记着账号与来源 IP，没设保留期就一直留，账号注销后仍在。原清单写「生产定期清」，靠人工。 | 组织加 `recordRetentionDays: 180`，Casdoor 自带的清理任务按它删（`object/record_cleanup.go`）。180 天是《网络安全法》第二十一条「网络日志留存不少于六个月」的下限，也符合个人信息保护法第十九条「保存期限为必要的最短时间」；隐私政策里的日志保留期要写成一致。`verification_record` 没有自带清理，见 §8.4。 |

两项都只在全新初始化时生效（`initDataNewOnly = true`）：已经初始化过的库不会跟着变，要在后台手动改，或重建卷。

### 8.3 拍板项的去向（2026-11-26 负责人批准全做）

1. **改密 / 找回密码 / 封禁之后游戏里的登录不失效** → §3.6（登录、刷新时核对，只吊销改密之前的会话，吊销了就踢下线）与 §3.7（通行证回调实时推送）。原先建议的「按账号记改密时刻水位」没有采用，理由在 §3.6。
2. **注册没有协议勾选、客户端没有首启隐私弹窗** → §3.9。弹窗复用登录界面现有美术，不需要新图；协议正文与地址仍要法务出。
3. **正式包也能用启动参数改网关和通行证地址** → §3.8 第 1 条（构建时写进包里、正式包不认覆盖、构建检查拦回环与明文）。
4. **安卓回调用自定义协议头** → §3.8 第 3 条（App Links，自定义协议头只在开发包登记）。Custom Tabs 没做，理由在 §3.8。
5. **桌面回调固定端口** → §3.8 第 2 条（登记五个端口，依次占）。

### 8.4 生产清单补充的去向（接 §5.4）

1. **公网入口按白名单放行，不能放 `/api/delete-user`、`/api/update-user`** → `deploy/k8s/passport/public-entry.yaml`（§3.10 第 1 条），名单由 `passport_k8s_contract.tests.ps1` 钉住。原因：Casdoor 的权限模型允许任何登录用户对自己调这两个接口（`object/init.go:534` 的自匹配条款，`authz/authz.go:281` 的名单），`controllers/user.go:568` 对「删自己」没有别的检查；Casdoor 对「任何人」放行的接口有 131 条（含 `/scim`、`/cas`、`/api/oauth/register`、`/api/init-admin-password`、`/api/metrics`），玩家用得到的只有十几条。名单是按源码推的，预发环境要按 README 第 7 步走一遍。
2. **`AuthProviders.Passport.Endpoint` 必须指向集群内入口** → 同一个 nginx 的 8081 + 内部 Ingress（§3.10 第 2 条）。指错了，注销卡在「执行中」直到 24 小时告警，登录与刷新前的核对全部按故障放行（`LoginPassportSessionCheckFailing` 会响）。
3. **`verification_record` 定时清理** → `verification-record-cleanup.yaml`（每天删 180 天以前的，最小权限库账号）。
4. **撞库拦截** → 初始化数据 `securityPolicy`：同一来源 10 分钟内 30 个不同账号登录失败封 15 分钟（按运营商大出口共用地址留了余量，有了真实失败率再调）。
5. **口令哈希** → 换成 `argon2id`（OWASP 对新系统的首选；项目没有存量用户、可以清库）。代价是每次校验约 64 MiB：Casdoor 的内存上限按同时校验 20 次留量，公网入口对登录类接口限速。
6. **镜像按摘要固定** → Casdoor、MySQL、nginx 都已固定，本机 compose 与生产清单一致。
7. **签名私钥在库里是明文** → 写进 README「上线前还要人办的」：备份加密、限制读库权限。

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
- 国内大厂的主注册方式是手机号 + 短信验证码。现在只开了邮箱；要开手机
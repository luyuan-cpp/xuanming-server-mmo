# QQ / 微信第三方登录端到端联调设计

更新：2026-11-12。HTTP 登录入口已经实现；2026-10-18 补齐上线前的服务端缺口（密钥注入、unionid 策略、错误语义、HTTP 加固、K8s 渲染，见 §四.1 与 §九）；2026-11-12 补上微信、抖音小游戏登录（§十一，已落码未编译）。渠道真实凭据联调、客户端 SDK 和账号绑定仍按各自验收记录判断，**从未与真实微信 / QQ / 抖音联调过**。

前置：[认证提供者框架](./auth-provider-framework.md)、[双 Token 设计](./dual-token-authentication.md)、[渠道沙盒手册](../ops/wechat-qq-sandbox-runbook.md)。

## 一、当前实现与待验收边界

| 组件 | 当前状态 |
|---|---|
| WeChat / QQ provider 与注册入口 | 服务端已有实现；配置和真实渠道联调分别验收 |
| 微信 / 抖音小游戏 provider（`wechat_minigame` / `douyin_minigame`） | 2026-11-12 服务端落码，未编译；K8s 部署脚本还不渲染这两段，见 §十一 |
| Java Gateway `POST /api/login` | 已在 LoginController 实现，委托 LoginService 调 login gRPC |
| `POST /api/assign-gate` | 独立负责分配 gate 与握手票据，不由 /api/login 加发 |
| 客户端 SDK、按钮与完整 OAuth 体验 | 本次未读取客户端仓，状态由客户端任务核对 |
| 跨渠道账号绑定、按 zone 鉴权类型白名单 | 下文的后续设计，不能当作已支持的表或配置项 |

实现入口：[LoginController](../../java/gateway_node/src/main/java/com/game/gateway/controller/LoginController.java)、[LoginService](../../java/gateway_node/src/main/java/com/game/gateway/service/LoginService.java)、[LoginResponse](../../java/gateway_node/src/main/java/com/game/gateway/dto/LoginResponse.java)。

## 二、现有登录链路

```text
客户端取得渠道凭据
  → POST /api/login（auth_type、auth_token、zone_id 等，字段见 LoginRequest 与 Jackson 配置）
  → Gateway 限流（login 的账号冷却与 assign-gate 独立）
  → LoginService 按 zone 调 ClientPlayerLogin.Login
  → Go provider 验证与账号逻辑
  ← 玩家列表、access_token / refresh_token 及过期时间
  → POST /api/assign-gate
  ← gate 地址与握手票据
  → TCP 连 gate、验票
  → gate 上 Login{auth_type:"access_token", auth_token:<上一步的 access_token>}：绑定会话，写 login_session 与设备集
  → 建角 / EnterGame 等玩家 RPC（按会话取账号）
```

Gateway 在 `application.yaml` 中配置 Jackson `SNAKE_CASE`，HTTP JSON 使用 `auth_type`、`zone_id`、`access_token`、`refresh_token` 等名称；Java DTO 内部仍使用驼峰属性。`/api/login` 不返回 gate 地址或 gate_token，也不代表已经完成场景入场。字段以 [LoginRequest](../../java/gateway_node/src/main/java/com/game/gateway/dto/LoginRequest.java)、LoginResponse 及全局序列化配置为准。

旧 gate TCP Login 路径是否开放受 login 的 `LegacyGateLoginEnabled` 控制。这个开关只管 gate 上用口令 / 第三方凭据做的**首认证**；gate 上 `auth_type=access_token` 的会话绑定是新流程的必经一步，不受它控制（`classifyLoginPath`，见 [gate-login-rpc-boundary.md](./gate-login-rpc-boundary.md)）。它与 HTTP 入口共享后端鉴权能力；后续会话恢复与入场见 [登录流程](./player_login_flow.md)。

生产的第三方登录只有一条路径：客户端把渠道凭据交给 `/api/login`，由 Go login 直连渠道校验（App 形态 auth_type=`wechat` / `qq`，小游戏形态 `wechat_minigame` / `douyin_minigame`，见 §十一）。`java/springboot_satoken_auth_starter` 是示例应用，没有部署清单；它的 `/auth/dev-login` 免 OAuth 按任意账号名签 SA-Token，只在 Spring profile `dev` 下注册，不得部署到玩家可达的环境。

## 三、返回码与 Gateway 配置

LoginController 返回 LoginResponse 对象。JSON 的 `code` 是业务码，不能把旧方案里的 401/403/423/429 映射表当成实际 HTTP 状态约定。

| 结果 | 实际返回语义 |
|---|---|
| 成功 | `CODE_OK`，附玩家列表与双 token |
| 前置限流排队 | `CODE_QUEUEING`，JSON 附 retry_after_ms、queue_pos |
| 前置拒绝 | `CODE_RATE_LIMITED` |
| login 回 in-band 业务拒绝码（凭证无效等，Tip.xlsx `fault` 列未标） | `CODE_AUTH_REJECTED`，message 为 `upstream_err=<tip 码>` |
| login 回 in-band 故障码（渠道故障、签发失败、读 Redis 失败等，`fault` 列标 1） | `CODE_INTERNAL`，message 仍为 `upstream_err=<tip 码>`（2026-10-18 起，两版同批，见 [tip-code-axis.md](./tip-code-axis.md) §2.10） |
| login 以 gRPC status 失败：UNAUTHENTICATED / PERMISSION_DENIED | `CODE_AUTH_REJECTED`，message 为 status 名 |
| login 以 gRPC status 失败：UNAVAILABLE / DEADLINE_EXCEEDED / RESOURCE_EXHAUSTED | `CODE_INTERNAL`，message `login_unavailable` |
| login 以其它 gRPC status 失败（UNKNOWN、INTERNAL 等）或网关内部异常 | `CODE_INTERNAL`（2026-10-18 前 UNKNOWN 等被错误地映射成 401） |

login 侧的 in-band 码（2026-10-18 起，fault 列来自 `data/tip/Tip.xlsx`，serverbase 据此告警）：

| 情形 | tip | 故障 |
|---|---|---|
| 玩家凭证无效 / 过期、未知 auth_type、口令认证失败 | `kLoginAccountNotFound`（2000） | 否 |
| 渠道或本服务渠道配置故障：网络 / 超时、非 2xx、应答超 64 KiB 或不可解析、微信系统繁忙 / AppSecret / AppId / 出口 IP / 配额类 errcode、拿不到必需的 unionid | `kLoginAuthProviderUnavailable`（2035） | 是 |
| 账号目录读 Redis 失败 / 记录损坏（以前直接返回 gRPC 错误，网关映射成 401） | `kLoginRedisError` / `kLoginDataParseFailed` | 是 |
| HTTP 路径 token 签发失败（以前回成功但不带 token，渠道 code 已被消费） | `kLoginTokenIssueFailed`（2036） | 是 |

网关按 fault 列定性（2026-10-18 收口原「已知残留」）：导表器多生成一份与 `faults.go` 同源的 Java `TipFaults`，本仓 `LoginService` / `RefreshTokenService` 与 Java 版 `LoginHttpService` 据此把上表的故障码回 `CODE_INTERNAL`，其余 in-band 码仍回 `CODE_AUTH_REJECTED`；HTTP 状态仍是 200，message 的取法不变：无参数文案时通常为 `upstream_err=<tip>`，有 `TipInfoMessage.parameters` 时沿用各版网关既有的解析或拼接结果。保留 tip 码用于排障。当前客户端只按 JSON `code` 分支，登录路径不解析 `upstream_err=<tip>`，也没有通用 tip 文案表。方案比较、版本偏斜与客户端核对项见 [tip-code-axis.md](./tip-code-axis.md) §2.10。连接目标、zone 路由与超时以 Gateway 的 LoginRpcClient 和 `application.yaml` 为准；旧提案中的 `gate.login.grpc-endpoint` / `retry` 配置不能直接复制使用。

客户端收到 500 + `upstream_err=2035` / `2036` 后，恢复登录要重新拉起渠道：微信重新授权拿新 code（2036 时 code 已被消费，2035 时也不能确定没被消费），QQ 重新拉起渠道取得新凭据，不把旧 `auth_token` 原样重发作为恢复流程。QQ token 的可复用性不改变这里约定的恢复策略。

## 四、渠道联调与后续改造

渠道凭据按 `go/login/etc/login.yaml`、对应配置类型与 provider 的现有字段注入；真实 AppSecret 只从部署环境提供，禁止写入仓库与日志。沙盒验证按 [微信/QQ 手册](../ops/wechat-qq-sandbox-runbook.md) 执行。

第三方账号不存凭据：渠道校验通过后，账号名（`wx_<unionid>`、`qq_<unionid>`、`dy_<unionid>`）直接作为全局 SQL 账号目录的键，首次登录由 data_service 自动建目录行（login 的 `readAccountDirectory(..., create=true)`，见 [账号角色目录](./account-role-authority.md)）。目录在 TiDB，同一地域所有区共用一份，跨地域各一份（[全层水平扩展规格](./horizontal-scale-logical-db.md) §9）；login Redis 的 `account:<account>` 自 2026-10-21 起不再是权威。不能执行旧草案中的 `INSERT IGNORE UserAccounts` 或照抄假设存在的 UserAccountsModel。账号绑定和按 zone 白名单仍需独立实现、迁移与测试；旧草案的 `Auth.EnabledTypes` / `PerZoneOverrides` 不是已经支持的配置。

以下保留渠道接入、绑定、安全与监控的后续设计清单；其中 API、表、指标及渠道时效均须在对应任务中核实，不作为本次已经实现或验收的声明。

### 四.1 上线前约束（2026-10-18 落码）

- **密钥**：login 不展开 `${VAR}`。配置写 `AuthProviders.WeChat.AppSecretEnv`（环境变量名），由 `config.ResolveSecrets` 在任何外部连接之前解析；未展开的 `${...}` 任何模式都拒绝启动，生产模式拒绝内联 `AppSecret`、只接受 `https://` 的 Endpoint（`go/login/internal/config/auth_providers.go`）。QQ 不需要 AppKey。
- **K8s**：`tools/scripts/k8s_deploy.ps1 -LoginWeChatAppId / -LoginQQAppId` 渲染 `AuthProviders`，微信密钥经 Secret `login-oauth` 注入 `LOGIN_WECHAT_APP_SECRET`，不进 ConfigMap。
- **unionid**：`RequireUnionId` 缺省 true，拿不到 unionid 即拒绝登录（故障码），不再静默回落 openid；QQ 的 `unionid=1` 由 `RequestUnionId` 控制。
- **HTTP**：先看状态码（非 2xx 即渠道故障）、应答限 64 KiB；错误不带 URL（查询串里有 code / secret）；成功日志的 openid 脱敏。
- **错误语义**：见 §三。

---

## 五、账号合并 / 绑定(后续迭代,不阻塞首发)

### 场景
- 玩家先用 `password` 注册,后想用微信登录同一账号
- 玩家在 iOS 用微信登录,在 Android 用 QQ 登录,要进同一角色

### 推荐方案: 引入 `UserBindings` 表

```sql
CREATE TABLE UserBindings (
  account     VARCHAR(64) NOT NULL,    -- 主账号 (uuid 或 password 账号名)
  bind_type   VARCHAR(16) NOT NULL,    -- "wechat" / "qq" / "phone"
  bind_key    VARCHAR(128) NOT NULL,   -- "wx_<unionid>" / "qq_<unionid>"
  created_at  DATETIME    NOT NULL,
  PRIMARY KEY (bind_type, bind_key),
  KEY idx_account (account)
);
```

登录时:
```
provider.Validate → bind_key
SELECT account FROM UserBindings WHERE bind_type=? AND bind_key=?
  ↓ 命中  → 用主 account 走后续流程
  ↓ 未命中 → 自动注册新账号 + 写一条 binding
```

绑定接口(已登录态):
```
POST /api/bind   { bind_type: "wechat", auth_token: <code> }
   → 服务端验 OAuth,确认未被别人占用,写 UserBindings
```

**首发不必上,先用"unionid 直接当 account"的最简模式。**

---

## 六、客户端集成清单

### 微信
- iOS / Android: 微信 SDK `WXApi.sendReq(SendAuth.Req)`,scope `snsapi_userinfo`
- 拿到 `code`(10 分钟有效、只能换一次) → POST `/api/login {auth_type:"wechat", auth_token:code}`
- ⚠️ AppId 在客户端是公开的,AppSecret 只在 go-zero 服务端

### QQ
- 客户端调 `Tencent.login(scope, listener)`
- 拿到 `access_token` + `openid` → POST `/api/login {auth_type:"qq", auth_token:access_token}`
- 服务端会再调 graph.qq.com 验证

### 微信小游戏
- 小游戏端调 `wx.login()` 拿到 `code`（一次性，约 5 分钟有效）→ POST `/api/login {auth_type:"wechat_minigame", auth_token:code}`
- 不要送 `auth_type:"wechat"`：那是 App / 网站应用的接口，换不了小游戏的 code
- 小游戏要绑定到与 App 相同的微信开放平台账号，才有 unionid，才和 App 是同一个账号（§十一）

### 抖音小游戏
- 小游戏端调 `tt.login({force: true})` 拿到 `code` → POST `/api/login {auth_type:"douyin_minigame", auth_token:code}`
- 用户没登录抖音时只有 `anonymousCode`，不能登录；客户端要先引导用户登录抖音

### 静默登录(双 token)
- 已有 access_token (2h 内) → POST `/api/login {auth_type:"access_token", auth_token:xxx}`
- access 过期但 refresh 有效 → POST `/api/refresh-token {refresh_token:yyy}`
- 都过期 → 重走 OAuth

详见 [dual-token-authentication.md](./dual-token-authentication.md) §Reconnect / Token Refresh。

---

## 七、安全与合规

| 项 | 措施 |
|---|---|
| AppSecret 泄露 | 只放服务端 K8s Secret，配置只写 `AppSecretEnv`（生产模式内联即拒启），代码不打日志 |
| OAuth code 重放 | 微信 code 只能用一次,服务端验过即作废;额外加 device_id 绑定 |
| OpenId 跨应用不一致 | 账号只用 `unionid`（`RequireUnionId` 缺省 true，拿不到即拒绝），需在微信开放平台绑定主体 / QQ 互联申请 unionid 权限 |
| 账号被盗(他人拿走 access_token) | 双 token 一次性 rotation + 异常 IP/设备触发踢号 |
| 未成年防沉迷 | 后续按渠道接入实名认证(从 `auth_type` 派生策略) |
| 苹果审核 | iOS 端必须同时支持 Sign in with Apple → 后续加 `AppleProvider` |

---

## 八、监控 & 埋点

| Metric | 标签 | 用途 |
|---|---|---|
| `login_total` | auth_type, result | 各渠道登录量 / 成功率 |
| `login_provider_latency_ms` | auth_type | 微信/QQ API 调用延迟 |
| `login_provider_error_total` | auth_type, errcode | 第三方接口错误统计 |
| `login_token_refresh_total` | result | 双 token rotation 健康度 |
| `login_first_register_total` | auth_type | 新增注册量 |

告警:
- `wechat` 调用失败率 > 5% → 微信侧故障 / AppSecret 失效
- `login_token_refresh_total{result="invalid"}` 突涨 → Refresh token 风险

---

## 九、实施状态（2026-10-18 订正，原清单已过时）

| 步骤 | 状态 |
|---|---|
| Java Gateway `/api/login` controller + DTO、grpc-java 客户端 | 已完成（LoginController / LoginService / LoginRpcClient） |
| login.yaml 配置 WeChat / QQ（K8s Secret） | 已完成：`AppSecretEnv` + 启动期门禁；`k8s_deploy.ps1` 渲染 + Secret `login-oauth`（2026-10-18） |
| loginlogic 自动注册分支对 wechat / qq 走通 | 已完成（全局 SQL 账号目录，首次登录自动建目录行；2026-10-21 前是 login Redis 的 `account:<account>`） |
| 微信 / 抖音小游戏登录（`wechat_minigame` / `douyin_minigame`） | 2026-11-12 服务端落码，未编译、未测试，待 Codex 验证（§十一） |
| 小游戏登录的 K8s 渲染（部署参数、Secret、防误关） | 未做：`k8s_deploy.ps1` 只渲染 WeChat / QQ 两段（§11.3） |
| 单元测试：mock 渠道 HTTP、错误传播 | 已落码；2026-10-19 Go 定向 7 项通过，全量 519 个顶层与 375 个子测试通过、4 个顶层环境条件跳过，旧行为两例精确断言红灯成立；证据与范围见 [交接文档](../handoff/handoff-tip-fault-gateway-500-20261018.md) |
| 本地沙箱端到端（sandbox_mock） | 工具已有；linux 登录栈 QQ AppId 与 mock 不一致的问题已修（2026-10-18） |
| 真实微信 / QQ 沙箱联调 → /api/login → /api/assign-gate → gate | **未做**，需要有真凭证的人按 runbook §C.2 执行 |
| 单 zone 灰度 24 小时、全量开放 | 未做 |
| `Auth.EnabledTypes` 灰度配置 | 不存在这个配置；按 zone 开关第三方登录仍是后续设计 |
| 网关按 tip 故障列回 500（§三） | 已落码（两版同批），导表与 147 项导表器测试已有证据；本轮主仓 gateway 204 项通过、5 项条件跳过，config_node 367 份源码编译通过且无测试；示例 starter 在 Surefire 加载前失败，后续验收停止。独立 Java 历史定向 42 项仅按输入 SHA 条件复用，不表示全 reactor 通过。详见 [交接文档](../handoff/handoff-tip-fault-gateway-500-20261018.md)；客户端可选文案补丁未打 |
| UserBindings 账号合并表、Apple / Google Provider | 后续 |

---

## 关联
- [docs/design/ARCH.md](./ARCH.md) §7 第三方登录
- [docs/design/auth-provider-framework.md](./auth-provider-framework.md) Provider 框架(权威)
- [docs/design/dual-token-authentication.md](./dual-token-authentication.md) 双 token 体系
- [docs/design/open-server-rate-limit-design.md](./open-server-rate-limit-design.md) /api/login 也要走限流
- [docs/design/architecture-current-state-vs-gaps-2026-05.md](./architecture-current-state-vs-gaps-2026-05.md) 缺口 #11
- 代码: `go/login/internal/logic/pkg/auth/providers.go` (WeChatProvider / QQProvider)
- 代码: `go/login/internal/logic/pkg/auth/minigame_providers.go` (WeChatMiniGameProvider / DouyinMiniGameProvider)
- 代码: `go/login/internal/logic/clientplayerlogin/loginlogic.go` (resolveAccount)
- 代码: `java/gateway_node/src/main/java/com/game/gateway/controller/LoginController.java`、`service/LoginService.java`
- 代码: `go/login/internal/config/auth_providers.go`（第三方配置门禁）

---

## 十、2026-08-15 落地增量:选服闭环 + login.rpc 多节点发现 + Unity 客户端接通

> 本节原编号「七」，与上文「七、安全与合规」重号，2026-10-18 改为「十」；`docs/PROGRESS.md` 2026-08-15 条目里的「§7」指本节。

本节记录已落码的三件事。2026-08-15 已由 Codex 完成
Java 全量单测和 Unity 程序集编译验证；真实多节点服务联调仍需依赖
etcd/Redis/MySQL/login/gate/scene 的完整环境。

### 10.1 区服准入闭环(Java Gateway)

此前 `zone_config` 的 MAINTENANCE/CLOSED/PREVIEW 只影响 `/api/server-list`
展示,直接打 `/api/assign-gate` 仍可进维护区。现在
`AssignGateService.checkZoneAdmission` 对 `/api/assign-gate` 与
`/api/queue-status`(排队中运维切维护也要拦)按手工状态 fail-closed:

| 状态 | HTTP code | error tag |
|---|---|---|
| zone 不存在 | 404 | `zone_not_found` |
| MAINTENANCE | 503 | `zone_maintenance` |
| CLOSED | 503 | `zone_closed` |
| PREVIEW | 503 | `zone_not_open`(白名单表 account 类型不匹配,暂不消费) |
| zone_id<=0(自动) | 放行 | 由 login 决定目标 zone |

自动探测态(etcd DOWN)不在网关二次裁决——login 实时选 gate 本身 fail-closed。
为避免大队列每 2s 轮询把 `zone_config` 放大成玩家数量级 MySQL QPS，
准入结果按 zone 短缓存 1s；同 zone 并发 miss 单航班合并，仅缓存
成功查询(含 NOT_FOUND)，DB 异常不缓存并继续 fail-closed。
单测:`AssignGateServiceZoneAdmissionTest`(10 用例)。

### 10.2 login.rpc 多节点自动发现(Java Gateway)

`login.grpc.endpoints` 的静态 `<zoneId>=host:port` 映射降级为兜底;
`LoginNodeDiscovery` 每 `login.grpc.discovery-interval-ms`(默认 5s)拉 etcd
`LoginNodeService.rpc/` 前缀(与 go/login node.go 的注册路径对齐),按 zone
建动态 channel 池推给 `LoginRpcClient`:

- 同 zone 多 login 实例轮询分摊;重试(UNAVAILABLE/DEADLINE)每次重选实例,
  天然 failover;
- 新增 zone / 扩缩容下个周期自动生效,网关不重启;
- etcd 查询失败保留 last-known-good 路由,不清表;
- 跨 zone 兜底仍然禁止(压测 3zone §I 事故约束不变);
- 开关 `login.grpc.discovery-enabled`(默认 true;测试 profile 关闭)。

### 10.3 Unity 客户端全链路(mmorpg-client)

选服 UI(UGUI,`QdaoServerSelectView`)为登录+选服合一屏,数据全动态:

```
GET /api/server-list ──► 区服卡片(状态灯/负载/推荐/新服/维护文案/分页/搜索/最近登录)
进入 ──► GameClient.EnterZone:
  POST /api/login(zone-pinned,排队 code=100 自动重试)
  POST /api/assign-gate(排队轮询 /api/queue-status,410 重新排队,404/503 拦截文案)
  TCP 连 gate + ClientTokenVerifyRequest 验签
  TCP Login(auth_type=access_token,密码不过游戏 TCP;无 token 时回落 password)
  CreatePlayer(角色为空时)→ EnterGame → 等 NotifyEnterScene 推送才算成功
RedirectToGateNotify(124) ──► 断旧 gate → 连新 gate → 重验签 → 重绑会话 → EnterGame
```

关键修复:gate 的 gRPC 回包不回显 `ClientRequest.id`(`main.cpp` SetIfEmptyHandler
只填 serialized_message+message_id),客户端应答匹配改为「id 精确匹配 → 按
message_id FIFO 兜底」(与 robot 口径一致),否则 Login/CreatePlayer/EnterGame
在客户端必超时。建连后所有终态失败统一先断开再回调，
`NotifyEnterScene` 等待预算与生产 robot 对齐为 60s，避免迟到推送造成
`InGame` 与 UI 状态分裂。协议、proto、C++、Go 均未改动。

### 10.4 原生 uGUI 资源与运行态验收

`Assets/Resources/UI/Ugui/Native` 已提供原生场景、窗框、搜索框、页签、
分类、区服卡片、底栏和状态灯 sprite。`QdaoUguiBuilder.BuildAll` 会先校验并
统一这些纹理的 Sprite importer，再烘焙 `QdaoServerSelect.prefab`；运行时
`QdaoServerSelectView` 仍会重绑状态灯 sprite，并在陈旧 prefab 上自愈重建。

2026-08-15 使用项目同版本 Unity 6000.5.8f1 完成以下验收:

- Builder:Images=67、Buttons=22、TMP=54、旧全屏参考截图引用数为 0；
- EditMode 6/6:覆盖全部 Theme 资源、凭据输入限制与 8 个状态灯；
- PlayMode 1/1:空测试场景经 `AfterSceneLoad` 自动生成 `AppBootstrap`，原生
  uGUI 初始化成功，未进入 FairyGUI 兼容 Router；
- 原生 2560x1080 capture 已复核，账号/密码输入保持可见（密码仍只驻内存）。

该验收覆盖资源导入、prefab 和客户端启动生命周期；本地未启动 8081 gateway，
因此不替代依赖 etcd/Redis/MySQL/login/gate/scene 的真实 HTTP/TCP 联调。

---

## 十一、小游戏登录（2026-11-12，已落码，未编译、未测试，待 Codex 验证）

用户 2026-10-19 定微信、抖音走小游戏形态（[充值设计](./payment-recharge.md) 文首）。小游戏的登录凭证和 App 不是同一套接口，原有的 `wechat` provider 换不了小游戏的 code，所以加两个 provider。

### 11.1 接口与账号

| auth_type | 客户端拿凭证 | login 调的渠道接口 | 账号名 |
|---|---|---|---|
| `wechat_minigame` | `wx.login()` 的 `code` | `GET https://api.weixin.qq.com/sns/jscode2session`，参数 `appid`、`secret`、`js_code`、`grant_type=authorization_code` | `wx_<unionid>` |
| `douyin_minigame` | `tt.login({force: true})` 的 `code` | `GET https://minigame.zijieapi.com/mgplatform/api/apps/jscode2session`，参数 `appid`、`secret`、`code`，请求头 `Content-Type: application/json` | `dy_<unionid>` |

- **为什么是新的 auth_type，不让 `wechat` 自己试两个接口**：两种 code 从外观上分不出来，试错会把一次性的 code 先消耗在错的接口上；App 和小游戏又各有各的 AppId 与密钥。客户端最清楚自己是哪种形态，由它在 auth_type 里说明。
- **微信 App 与小游戏是同一个账号**：账号名都是 `wx_<unionid>`。前提是小游戏绑定到与 App 相同的微信开放平台账号；没绑定时 code2Session 不返回 unionid，缺省拒绝登录（`RequireUnionId`，口径同 §四.1）。
- **抖音**：unionid 是同一开发者主体下各小游戏通用的用户标识，带 `code` 的请求会返回。匿名用户（只有 `anonymousCode`）不能登录：匿名身份跟着设备走，换设备就变，不能当账号。
- **session_key 不留**：code2Session 的应答带 `session_key`，登录用不到。provider 的应答结构体不声明这个字段，它不进内存里的结构体，也就进不了日志和错误文案。支付要用时由支付服务拿新的 code 现取现弃（充值设计 §6）。
- **配置**：`AuthProviders.WeChatMiniGame` / `DouyinMiniGame`，字段是 `AppId`、`AppSecretEnv`（生产必须）、`RequireUnionId`（缺省 true）、`Endpoint`（沙箱）。门禁与 WeChat 段相同（§四.1）。另外 `WeChat.AppId` 与 `WeChatMiniGame.AppId` 相同时拒绝启动：两个应用的应用号不会相同，相同说明有一段抄错了。

### 11.2 错误语义

回给客户端的码仍是 §三 那几个，分类规则：

| 情形 | 归类 |
|---|---|
| 网络 / 超时、非 2xx、应答超 64 KiB 或不可解析、成功应答却没有 openid、拿不到必需的 unionid | 渠道故障（`kLoginAuthProviderUnavailable`） |
| 微信 errcode -1 / 40001 / 40013 / 40125 / 40164 / 41002 / 41004 / 45009 | 渠道故障 |
| 微信其余 errcode：40029（code 无效）、40163（已被使用）、40226（高风险用户被微信拦截）、45011（按用户限频）等 | 凭证无效（`kLoginAccountNotFound`） |
| 抖音 `error` / `errcode` 非 0 | 一律凭证无效 |

两处要在真渠道联调时复核：

- 微信 45011 在 App 的接口上算渠道故障，在 code2Session 上是按用户限频（文档口径是每个用户每分钟 100 次），这里不算故障。2026-11-12 没能打开微信官方页面复核这一条，按 [tip-code-axis.md](./tip-code-axis.md) §2.9「拿不准不算故障」处理。
- 抖音的 code2Session 文档没有给出错误码表，分不清哪些是应用侧故障，所以全部按凭证无效（与 QQ 同一处理）。代价是 AppId / AppSecret 配错时所有抖音登录都失败，却不触发故障告警；errcode 在 login 的 `Auth failed` ERROR 日志里。

### 11.3 没做的

| 项 | 状态 |
|---|---|
| K8s 部署 | 未做。`k8s_deploy.ps1` 只渲染 WeChat / QQ 两段，没有小游戏的部署参数、Secret 与防误关。本机 `login.yaml` 与 linux 登录栈可用 |
| QQ 小游戏 | 未做，没有接入计划。要做时照 `WeChatMiniGameProvider` 加一个 provider |
| 客户端 | 未做。小游戏端调 `wx.login` / `tt.login`，并在 `/api/login` 里送新的 auth_type，归客户端任务 |
| 传输层 | 未做。gate 只有 TCP 入口，没有 WebSocket；小游戏登录成功后还连不上 gate（充值设计把「客户端改 WebSocket」列为非目标，没有别的文档接手） |
| 真渠道联调 | 未做，没有小游戏的开发者账号。现有证据只能来自 mock |
| Java 版 | 待做，已在 Java 仓 `PARITY.md` 登记 |

### 11.4 接口事实的来源

- 微信：开放文档「小程序登录 / code2Session」。2026-11-12 旧地址已迁移、没能打开新页面，请求地址、参数与返回字段取自搜索引擎对该文档的摘录，errcode 表没有复核到。
- 抖音：[小游戏 code2Session](https://developer.open-douyin.com/docs/resource/zh-CN/mini-game/develop/server/log-in/code-2-session)。2026-11-12 页面同样没能直接打开，请求方法、地址、请求头、参数与返回字段取自搜索引擎对该页的摘录。

验证命令见 `docs/PROGRESS.md` 2026-11-12「微信 / 抖音小游戏登录」条目。

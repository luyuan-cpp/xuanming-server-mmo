# QQ / 微信第三方登录端到端联调设计

**Date:** 2026-05-08
**Task:** #11
**前置:** [auth-provider-framework.md](./auth-provider-framework.md), [dual-token-authentication.md](./dual-token-authentication.md)
**目标:** 把现有 `WeChatProvider` / `QQProvider` 串成完整可上线链路

---

## 一、现状盘点

| 组件 | 状态 |
|---|---|
| `WeChatProvider.Validate` (调微信 OAuth API,返回 `wx_<unionid\|openid>`) | ✅ 已实现 |
| `QQProvider.Validate` (调 QQ Graph API,返回 `qq_<unionid\|openid>`) | ✅ 已实现 |
| `auth.Register("wechat" / "qq", ...)` | ✅ 已注册 |
| go-zero `LoginRequest{auth_type, auth_token}` proto 字段 | ✅ 已存在 |
| Java Gateway `/api/login` HTTP 入口 | ❌ **缺失**(目前只有 `/api/assign-gate`) |
| 客户端 SDK 集成(微信/QQ 登录按钮) | ❌ 未做 |
| 账号合并 / 绑定流程(已有 password 账号绑微信) | ❌ 未做 |
| 灰度配置(某 zone 只允许某些 auth_type) | ❌ 未做 |

**核心缺口:** Provider 在 go-zero login 已 ready,但**客户端的 OAuth code 没有 HTTP 入口送进来**。当前架构下游戏客户端拿 gate token 后才连 gate,gate 上才走 Login RPC——这对密码登录没问题,但对第三方登录 UX 极差(玩家要等到 TCP 建好才能验微信)。

---

## 二、推荐链路(改造后)

### 2.1 总览

```
Client(SDK)
   │ ① 拉起微信/QQ 授权 UI
   │ ② 拿到 code / access_token
   ▼
Java Gateway POST /api/login
   { auth_type: "wechat", auth_token: <code>, device_id, zone_id }
   │ (限流/排队 / IP 防刷,见 #10)
   │
   ▼ gRPC Login(LoginRequest{auth_type, auth_token, device_id})
go-zero login
   │ resolveAccount → AuthProvider("wechat").Validate
   │   → 调微信 API,拿 unionid/openid
   │   → account = "wx_<unionid>"
   │
   │ EnterGame 主流程(现有):
   │   RedisLocker → loginstep → DecideEnterGame
   │   issue access_token + refresh_token
   │
   ▼
Java Gateway 收到 LoginResponse,加发 gate 信息
   { code, access_token, refresh_token, players,
     gate_ip, gate_port, gate_token, deadline }
   │
   ▼
Client → 拿 gate_token → TCP 连 gate
```

### 2.2 为什么登录入口要加 `/api/login` 而不是继续走 `/api/assign-gate` + gate

| 方案 | 优 | 劣 |
|---|---|---|
| **新加 `/api/login` (推荐)** | OAuth 失败/账号没建在 HTTP 401 阶段就返回;客户端可走通用错误 UI;Java Gateway 可统一限流/审计 | 多一个端点 |
| 复用 `/api/assign-gate` 兼带登录 | 端点少 | 把"分配 gate"和"账号鉴权"耦合,一个失败两边重试逻辑乱 |
| 只在 gate TCP 阶段做 Login | 现有路径 | OAuth code 必须先建 TCP 才能验,失败后 gate 要主动断,UX 差;HTTP 限流派不上用场 |

**结论:** 加 `/api/login`,与 `/api/assign-gate` 解耦。客户端流程变:
```
1. /api/login                → 拿 access/refresh token + 玩家列表
2. /api/assign-gate          → 拿 gate_ip + gate_token
3. TCP 连 gate + token verify → 进游戏
```

老的密码客户端如果走 gate 的 Login RPC,**保留兼容**(后端是同一个 EnterGame 路径),但官方推荐新客户端走 HTTP `/api/login`。

---

## 三、Java Gateway 改造

### 3.1 新增 endpoint

```java
@PostMapping("/api/login")
public LoginResponse login(@RequestBody LoginRequest req,
                           HttpServletRequest http) {
    // ① 限流(zone + ip + account-cooldown,详见 #10)
    rateLimiter.check(req.zoneId(), extractIp(http), req.account());

    // ② 转发到 go-zero login(gRPC,长连接池)
    var rpcResp = loginRpcClient.login(
        com.game.proto.login.LoginRequest.newBuilder()
            .setAccount(nullSafe(req.account()))
            .setPassword(nullSafe(req.password()))
            .setAuthType(req.authType())     // "wechat" / "qq" / "satoken" / "password"
            .setAuthToken(nullSafe(req.authToken()))
            .setDeviceId(nullSafe(req.deviceId()))
            .build()
    );

    // ③ 直接返回 access/refresh + 玩家列表;gate 由后续 /api/assign-gate 给
    return LoginResponse.from(rpcResp);
}
```

### 3.2 errcode 映射

| go-zero 返回 | HTTP code | 客户端动作 |
|---|---|---|
| OK | 200 | 进入选服 |
| auth provider rejected | 401 | 重新走 OAuth |
| account banned | 403 | 显示封禁原因 |
| zone closed | 423 | 显示维护文案 |
| rate limited | 429 | 排队 / 退避(见 #10) |
| internal | 500 | 上报埋点 + 重试 |

### 3.3 配置示例

```yaml
gate:
  login:
    grpc-endpoint: "etcd:///login.rpc"
    timeout-ms: 3000
    retry: 1
```

---

## 四、go-zero login 侧需要补的小项

### 4.1 配置打开(`go/login/etc/login.yaml`)

```yaml
Auth:
  WeChat:
    AppId: "wx1234567890"
    AppSecret: "${WECHAT_APP_SECRET}"      # 走 K8s Secret
  QQ:
    AppId: "qq1234567890"
    AppKey: "${QQ_APP_KEY}"
```

### 4.2 账号建库(首次 OAuth 登录)

`UserAccounts` 表已有,首次 `wx_<unionid>` 进来要做的事:

```
INSERT IGNORE UserAccounts (account, account_type, created_at, ...)
   VALUES ("wx_xxx", "wechat", now(), ...)
```

这个逻辑应该已经在 `loginlogic.go` 的"账号不存在自动注册"分支里;若没有,需要补:

```go
if user, err := svc.UserAccountsModel.FindByAccount(ctx, account); err == sqlc.ErrNotFound {
    user = &UserAccounts{Account: account, AccountType: deriveType(authType)}
    svc.UserAccountsModel.Insert(ctx, user)
}
```

### 4.3 灰度配置

```yaml
Auth:
  EnabledTypes: ["password", "wechat", "qq"]   # 黑/白名单
  PerZoneOverrides:
    1: ["password"]                            # 测试服只允许密码
    2: ["wechat", "qq"]                        # 海外服只允许第三方
```

`resolveAccount` 入口处加白名单检查,失败返回 `ProviderDisabled`。

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
- 拿到 `code`(5 分钟有效) → POST `/api/login {auth_type:"wechat", auth_token:code}`
- ⚠️ AppId 在客户端是公开的,AppSecret 只在 go-zero 服务端

### QQ
- 客户端调 `Tencent.login(scope, listener)`
- 拿到 `access_token` + `openid` → POST `/api/login {auth_type:"qq", auth_token:access_token}`
- 服务端会再调 graph.qq.com 验证

### 静默登录(双 token)
- 已有 access_token (2h 内) → POST `/api/login {auth_type:"access_token", auth_token:xxx}`
- access 过期但 refresh 有效 → POST `/api/refresh-token {refresh_token:yyy}`
- 都过期 → 重走 OAuth

详见 [dual-token-authentication.md](./dual-token-authentication.md) §Reconnect / Token Refresh。

---

## 七、安全与合规

| 项 | 措施 |
|---|---|
| AppSecret 泄露 | 只放服务端 K8s Secret,代码不打日志 |
| OAuth code 重放 | 微信 code 只能用一次,服务端验过即作废;额外加 device_id 绑定 |
| OpenId 跨应用泄露 | 优先用 `unionid` 而非 `openid`,需在微信开放平台开通 |
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

## 九、实施步骤

1. ✅ 写本文档
2. ⬜ Java Gateway 加 `/api/login` controller + DTO
3. ⬜ 接入 grpc-java 客户端到 go-zero login(若已有则复用)
4. ⬜ go-zero `login.yaml` 配置 WeChat / QQ AppId/Secret(K8s Secret)
5. ⬜ go-zero `loginlogic.go` 校验"自动注册"分支对 wechat/qq account 也走通
6. ⬜ 灰度配置 `Auth.EnabledTypes` 默认仅 `password`
7. ⬜ 单元测试: mock WeChat/QQ HTTP 接口,验证 provider 错误传播
8. ⬜ 联调: 真实微信/QQ 沙箱账号 → /api/login → /api/assign-gate → gate
9. ⬜ 单 zone 灰度 wechat,观察 24 小时
10. ⬜ 全量开 wechat + qq
11. ⬜ (后续) UserBindings 账号合并表
12. ⬜ (后续) AppleProvider / GoogleProvider 海外版

---

## 关联
- [docs/design/ARCH.md](./ARCH.md) §7 第三方登录
- [docs/design/auth-provider-framework.md](./auth-provider-framework.md) Provider 框架(权威)
- [docs/design/dual-token-authentication.md](./dual-token-authentication.md) 双 token 体系
- [docs/design/open-server-rate-limit-design.md](./open-server-rate-limit-design.md) /api/login 也要走限流
- [docs/design/architecture-current-state-vs-gaps-2026-05.md](./architecture-current-state-vs-gaps-2026-05.md) 缺口 #11
- 代码: `go/login/internal/logic/pkg/auth/providers.go` (WeChatProvider / QQProvider)
- 代码: `go/login/internal/logic/clientplayerlogin/loginlogic.go` (resolveAccount)
- 代码: `java/gateway_node/src/main/java/com/game/gateway/controller/` (待加 LoginController)

---

## 七、2026-08-15 落地增量:选服闭环 + login.rpc 多节点发现 + Unity 客户端接通

本节记录已落码的三件事。2026-08-15 已由 Codex 完成
Java 全量单测和 Unity 程序集编译验证；真实多节点服务联调仍需依赖
etcd/Redis/MySQL/login/gate/scene 的完整环境。

### 7.1 区服准入闭环(Java Gateway)

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

### 7.2 login.rpc 多节点自动发现(Java Gateway)

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

### 7.3 Unity 客户端全链路(mmorpg-client)

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

### 7.4 原生 uGUI 资源与运行态验收

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

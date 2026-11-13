# WeChat / QQ Sandbox 接入清单 (todo #223 / U)

**Date:** 2026-05-10(2026-10-18 按上线前缺口修复订正 §B / §C / §D.4 / §E / §G,订正说明见文末)
**目的:** 让接入第三方 OAuth 的人 30 分钟内完成"申请 sandbox AppId → 配置 login.yaml → 跑通端到端"全流程,不需要再翻散落的代码。
**前置:** [auth-provider-framework.md](../design/auth-provider-framework.md), [third-party-login-end-to-end-design.md](../design/third-party-login-end-to-end-design.md), [release-checklist.md](./release-checklist.md)

---

## A. 沙箱凭证申请

### A.1 WeChat Open Platform

**入口:** <https://open.weixin.qq.com/>

| 字段 | 怎么拿 |
|---|---|
| AppId | 控制台 → 管理中心 → 移动应用 / 网站应用 → 创建后得到 `wx1234567890abcdef` 形态 |
| AppSecret | 同页面"查看"按钮(只显示一次,丢了得重置) |
| OAuth 域名白名单 | 必须把 staging gateway 的 host 加进"授权回调域";本地测试用 `127.0.0.1:8081` 不行,**用 ngrok / Cloudflare Tunnel** 套一层 https |
| UnionId | 在 Open Platform(不是公众平台)绑同一主体下的多个 App 后才会下发,**这是我们 `wx_<unionid>` 账号映射的依据**;login 缺省 `RequireUnionId: true`,拿不到就拒绝登录 |

**我们用到的接口**(`go/login/internal/logic/pkg/auth/providers.go` 的 `WeChatProvider.Validate`):
```
GET https://api.weixin.qq.com/sns/oauth2/access_token
    ?appid=<AppId>&secret=<AppSecret>&code=<from-SDK>&grant_type=authorization_code
返回: { access_token, openid, unionid?, expires_in, ... }
```

> **沙箱限制:** 每天 ~5000 次 `code → access_token` 调用上限;够测试,不够压测。

### A.2 QQ Connect

**入口:** <https://connect.qq.com/>

| 字段 | 怎么拿 |
|---|---|
| AppId | "开发者中心 → 应用列表"得到 `10xxxxxxxx` 数字 |
| AppKey | 同页(我们当前**没用**到 AppKey;客户端 SDK 直接拿 access_token,login 配置里也不用写) |
| UnionId 权限 | 需在 QQ 互联申请 unionid 权限;没有时 `/oauth2.0/me` 不返回 unionid,login 缺省拒绝登录(见 §D.4) |
| 回调域 | 同 WeChat,把 staging gateway 域名加白 |

**我们用到的接口**(`providers.go` 的 `QQProvider.Validate`;`unionid=1` 由 `RequestUnionId` 控制,缺省带):
```
GET https://graph.qq.com/oauth2.0/me?access_token=<from-SDK>&unionid=1&fmt=json
返回(JSONP 包裹时也能解):
   { client_id, openid, unionid? }
```

> **客户端拿 access_token,不是 OAuth code。** 不要把 WeChat 的协议套到 QQ 上。

---

## B. 把凭证灌进 login.yaml

**login 不展开 `${VAR}`**(`conf.MustLoad` 没带 `conf.UseEnv()`,刻意不加:它会对整份 yaml 做 `os.ExpandEnv`,
Redis 口令等含 `$` 的值会被静默改写)。旧版本文写的 `AppSecret: "${WECHAT_APP_SECRET}"` 会把这串字面量当成密钥;
现在任何字段里出现未展开的 `${...}`,login 都在启动期拒绝(`go/login/internal/config/auth_providers.go`)。

正确写法:配置里只写**环境变量名**,密钥由 K8s Secret 注入该环境变量。

```yaml
AuthProviders:
  WeChat:
    AppId: "wx0123456789abcdef"            # 公开值,直接写
    AppSecretEnv: "LOGIN_WECHAT_APP_SECRET" # 环境变量名;缺失 / 为空 / 首尾带空白 / 占位串 → 拒绝启动
    # RequireUnionId: true                 # 缺省 true:没有 unionid 就拒绝登录,不回落 openid
  QQ:
    AppId: "101234567"                     # 必须等于 /oauth2.0/me 返回的 client_id
    # RequestUnionId: true                 # 缺省 true:请求时带 unionid=1
    # RequireUnionId: true                 # 缺省 true;RequestUnionId=false 时必须同时写 false
```

| 模式 | 内联 `AppSecret` | `Endpoint` |
|---|---|---|
| `Mode: dev` / `test` | 放行并打 SECURITY WARNING(仅本地沙箱) | `http://` 与 `https://` 都可 |
| 其它(含不写,缺省 pro) | **拒绝启动**,只认 `AppSecretEnv` | 只接受 `https://`,不带用户信息 / 查询串 / 片段 |

K8s:`tools/scripts/k8s_deploy.ps1` 传 `-LoginWeChatAppId` / `-LoginQQAppId` 渲染上面这段,微信密钥从部署机环境变量
`MMORPG_LOGIN_WECHAT_APP_SECRET` 读入 zone namespace 的 Secret `login-oauth`,经 `secretKeyRef` 注入 login 容器的
`LOGIN_WECHAT_APP_SECRET`,不进 ConfigMap(沙箱 Endpoint 与 openid 回落开关见脚本参数注释)。

六个参数(`-LoginWeChatAppId` / `-LoginWeChatEndpoint` / `-LoginWeChatAllowOpenId` / `-LoginQQAppId` / `-LoginQQEndpoint` /
`-LoginQQAllowOpenId`)**不粘滞**:每次部署按本次取值重写 login ConfigMap,没给微信就删掉 Secret `login-oauth`。所以已开第三方登录的
zone,之后每次重新部署都要照传同一组参数,部署机上也要再设一次 `MMORPG_LOGIN_WECHAT_APP_SECRET`。

| 入口 | 第三方登录参数 | 漏传时 |
|---|---|---|
| 直接调 `k8s_deploy.ps1 -Command zone-up / all-up` | 原样生效 | 写操作之前拒绝(见下) |
| `dev_tools.ps1 -Command k8s-zone-up / k8s-all-up` | 同名参数,非空 / 被指定才透传 | 同上(由 `k8s_deploy.ps1` 拒绝) |
| `k8s_image.ps1`、`dev_tools.ps1 -Command k8s-image-* / k8s-release-*` | 不支持(这条路不部署 login,也碰不到 login 的 ConfigMap 与 Secret) | `dev_tools.ps1` 显式给了即拒绝 |

**防误关(fail-closed)**:本次要部署 login 时,脚本先读每个 zone 线上的 login ConfigMap(`AuthProviders` 段)和 Secret
`login-oauth` 是否存在(只用 `-o name`,不读密钥值),再和本次参数比对。下面任一情况都会在任何写操作之前拒绝,所有 zone 一次报全:

- 关掉线上开着的 provider;
- 漏传 Endpoint,沙箱会改连生产;
- 漏传 AllowOpenId,`RequireUnionId` 翻回 true;
- 换了 AppId;
- 线上段认不出,或读不到集群。

报错里给出照传现状的参数串,例如 `要保持现状请照传 -LoginWeChatAppId wx0123456789abcdef -LoginQQAppId 101234567`。线上没开、本次新开,不需要确认。

确要关闭或改动时加 `-ConfirmLoginAuthProviderChange`。它是许可,不是指令:线上和本次一致时给了也不改任何东西。`-DryRun` 不读集群,
所以不做这道核对。`zone-down` 会连 namespace 一起删掉 ConfigMap 和 Secret,之后的 `zone-up` 没有现状可比,这道核对不会触发。
灾难恢复([PITR 手册](./mysql-backup-pitr-runbook.md) §4.0)要在 `zone-down` 之前记下线上的这组值,起服时照传。

```powershell
# 例:已开微信(沙箱 mock)+ QQ 的 staging zone,经包装入口重新部署
$env:MMORPG_LOGIN_WECHAT_APP_SECRET = '<从密钥管理取,不要写进文件>'
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up -ZoneName test1 -ZoneId 101 -ReleaseProfile staging `
    -LoginWeChatAppId wx0123456789abcdef -LoginWeChatEndpoint https://wechat-mock.example.com -LoginQQAppId 101234567
```

本地测试(在 `go/login` 下;配置文件参数是 `-loginService`,不是 `-f`):

```powershell
$env:LOGIN_WECHAT_APP_SECRET = '<从密钥管理取,不要写进文件>'
go run . -loginService etc/login.yaml
```

启动后日志应有:
```
Auth provider registered: wechat (require_unionid=true endpoint_override="")
Auth provider registered: qq (request_unionid=true require_unionid=true endpoint_override="")
```
没出现就是 `AuthProviders.WeChat` 段没写对(缩进);配置写错时 login 直接拒绝启动,panic 信息会点名字段。

---

## C. 端到端联调步骤

### C.1 单元 / 集成层(本机就能跑)

由 Codex 按交付说明执行(Claude 不跑测试,AGENTS.md §10.1):

```bash
# go-zero 这层 mock-server e2e(provider 策略与配置门禁):
cd go/login && go test -count=1 ./internal/logic/pkg/auth/ ./internal/config/

# Java Gateway 这层 wire-format e2e:
cd java/gateway_node && mvn -Dtest='LoginEndpointIntegrationTest' test
# 命中 loginEndpoint_propagatesAuthTypeAndAuthToken_wechat / _qq
```

### C.2 真 Gateway → 真 login → 真微信 / 腾讯

**前置**: §A.1 / §A.2 已拿到 sandbox 凭证;§B 已配置;login + Java Gateway 已起。

```bash
# WeChat 用真 SDK 拿到的 code(从手机扫码 / Web 授权回调)
CODE=<the-5-min-oauth-code-from-sdk>

curl -v -X POST http://${GATEWAY_HOST}:8081/api/login \
  -H 'Content-Type: application/json' \
  -d "{
    \"zone_id\": 1,
    \"auth_type\": \"wechat\",
    \"auth_token\": \"${CODE}\",
    \"device_id\": \"sandbox-test-1\"
  }"
```

**期望返回**:
```json
{
  "code": 0,
  "players": [],
  "access_token": "<32 random bytes hex>",
  "refresh_token": "<32 random bytes hex>",
  "access_token_expire": <now + 7200>,
  "refresh_token_expire": <now + 2592000>
}
```

**典型失败 + 排查**:Java 网关对 login 的 in-band 码按 `data/tip/Tip.xlsx` 的 `fault` 列定性:业务拒绝回 `code:401`,
故障码回 `code:500`(2026-10-18 起,两版同批,见 `docs/design/tip-code-axis.md` §2.10);本节列出的故障通常以 `upstream_err=<tip 码>`
作为 `message`(网关不回显 provider 的错误文本)。message 取法未改,上游带参数文案时沿用各版网关既有的解析或拼接结果。HTTP 状态恒为 200。login 以 gRPC status 失败时,只有 UNAUTHENTICATED / PERMISSION_DENIED
回 401,其余回 500。具体原因看 login 日志 `Auth failed: type=... tip=... err=...`。

| 现象 | 原因 | 解 |
|---|---|---|
| `401 upstream_err=2000` + 日志 `wechat: empty code` | provider 收到空 token | 客户端没填 `auth_token`,检查 SDK 集成 |
| `401 upstream_err=2000` + 日志 `wechat: errcode=40029` / `40163` | code 已用过 / 失效(玩家侧,不算故障) | 重新走 OAuth,**code 一次性 + 10 min 过期**(微信开放平台移动应用文档) |
| `500 upstream_err=2035` + 日志 `errcode=40125` / `40013` / `40164` | AppSecret / AppId 不对,或出口 IP 不在白名单(故障码,in-band 告警) | 核对 Secret 与开放平台白名单 |
| `500 upstream_err=2035` + 日志 `unionid missing` | 应用没绑开放平台主体 / 没有 unionid 权限 | 见 §D.4 |
| `500 upstream_err=2035` + 日志 `http status` / `http error` / `bad json` | 渠道不可达、非 2xx、应答不可解析 | 查出口网络与渠道状态 |
| `500 upstream_err=2036` | token 签发失败(写 Redis 失败;微信 code 已被消费) | 查 login 的 Redis;重试前重新拉起渠道:微信拿新 code,QQ 取新凭据 |
| `500 upstream_err=2021` / `2022` | 账号目录读 Redis 失败 / 记录损坏 | 查 login 的 Redis 与 `account:<account>` 键 |
| `500 login_unavailable` | login.rpc 没起 / 网络不通 / 超时 | `docker ps`、`netstat -an`(找 50000 端口) |
| login 起不来,panic 点名 `AuthProviders.*` | 配置门禁拒绝(未展开的 `${...}`、生产模式内联密钥、http Endpoint 等) | 按 §B 改配置 |

> 2035 / 2036 是 `kLoginAuthProviderUnavailable` / `kLoginTokenIssueFailed`(2026-10-18 新增,导表结果即 2035 / 2036,
> 见 `data/tip/Tip.xlsx` 与 `generated/code/proto/tip/login_error_tip.proto`)。旧版本文的「`code:0` 但 access_token 空」
> 已不会出现:HTTP 路径签发失败现在回 2036。原「网关对 in-band 故障仍回 401」的已知残留已于同日收口:故障码回 500,
> 见 `docs/design/third-party-login-end-to-end-design.md` §三。客户端收到 500 + 2035 / 2036 时,恢复登录要重新拉起渠道:
> 微信重新授权拿新 code,QQ 重新拉起渠道取新凭据,不把旧 `auth_token` 原样重发作为恢复流程。QQ token 可复用不等于恢复流程应复用。

### C.3 验证账号映射

第一次成功后,账号角色目录在 login 的 Redis 里(键前缀 `account:`,`constants.GetAccountDataKey`;**不落库、不设 TTL**,
旧版本文的 `account_data:` 前缀与 MySQL `UserAccounts` 查询都不对):

```bash
docker exec redis redis-cli --scan --pattern 'account:wx_*'
# 应该看到 account:wx_<unionid>(RequireUnionId=false 时才可能出现 account:wx_<openid>)
```

`account` 以 `wx_` / `qq_` 前缀开头 = provider 框架按设计工作。

### C.4 第二次同 unionid 登录(账号复用)

用同一个微信号再走一遍 OAuth(**新 code**),期望:
- 不创建新账号
- 返回**同一个 player_id**(玩家继承上次进度)
- access_token 是新的(每次 issue 新 token)

缺省 `RequireUnionId: true` 时拿不到 unionid 会直接失败(§C.2 的 2035),不会再出现「静默用 openid 建出第二个账号」;
只有显式写了 `RequireUnionId: false` 才可能因为回落 openid 而返回不同 player_id → 看 §A.1 的 UnionId 行,绑同一主体下多个 App。

---

## D. 容易踩的 4 个坑(以本仓库实际经验)

### D.1 OAuth 域名白名单严格匹配

WeChat Open Platform 的"授权回调域"**只接受顶级域 + 二级域**(`example.com`、`*.example.com`),**不接受路径、不接受端口、不接受 IP**。`127.0.0.1:8081` 会被拒。

**解法**:本地测试套 ngrok:
```bash
ngrok http 8081
# 把 ngrok 给的 https://xxx.ngrok-free.app 顶级域填到回调域
```

### D.2 staging vs prod 凭证混用

凭证一旦混(staging code 拿到 prod login,反之亦然)→ `errcode=40029` 一直循环。yaml `${VAR}` 注入 + K8s namespace 隔离,不要用同一个 Secret 跨 namespace。

### D.3 AppSecret 进 git

```bash
# pre-commit hook(可加到 .git/hooks):
git diff --cached | grep -E '(AppSecret|AppKey)\s*[:=]\s*["\x27][^"$\x27]' && {
  echo "ERROR: literal AppSecret/AppKey detected; use \${VAR} placeholder"
  exit 1
}
```

### D.4 unionid 没拿到

控制台没把当前 App 加到"主体"分组 → 微信 API 不返回 unionid,只返回 openid。**openid 跨 App 不一致**——回落到 `wx_<openid>`
会让同一玩家在两个 App 里变成两个账号,无法迁移。所以 2026-10-18 起 login 缺省 `RequireUnionId: true`:拿不到 unionid 就拒绝登录、
回故障码 2035(in-band 告警),**不再静默回落**。QQ 同理(没有 unionid 权限时 `/oauth2.0/me` 不返回 unionid)。

**解法**:Open Platform 控制台 → 主体管理 → 把所有 App 加进同一主体 → 等 ~10 min 生效后重新走 OAuth → 验证返回里 `unionid` 非空。
确认永远只有一个 App、永不接开放平台时,才可以显式写 `RequireUnionId: false`(QQ 没有 unionid 权限时写
`RequestUnionId: false` + `RequireUnionId: false`);login 启动时会为此打 SECURITY WARNING。

---

## E. 上 prod 前的 checklist

- [ ] 微信 AppSecret 在 K8s Secret 里,login 配置只写 `AppSecretEnv`(生产模式内联值会拒绝启动);QQ 不需要密钥
- [ ] login 运行在非 dev 模式,`Endpoint` 为空或 `https://`
- [ ] `RequireUnionId` 保持缺省 true,并已确认 sandbox 返回里 unionid 非空(§D.4)
- [ ] OAuth 回调域已加 prod gateway 顶级域(staging 域不要复用)
- [ ] login 启动日志确认 `Auth provider registered: wechat / qq`,且 `require_unionid=true`
- [ ] sandbox 至少跑通过一次 §C.2 / §C.3(否则就是上 prod 才发现集成不通)
- [ ] 告警接上 `rpc_inband_fault` 里的 2035(渠道故障)与 2036(签发失败):serverbase 按 Tip.xlsx 的 fault 列定性
- [ ] 流量按 zone 灰度:先开放给一两个 zone,观察 24h `account:wx_*` 增长是否合理(没大量重复账号)

---

## F. 我做这件事的进度

完成度 ⚠️ — 本机 dev 没有真 sandbox 凭证,所以 §C.2 / §C.3 由有凭证的同事跑。我做完的部分:

- ✅ §B 配置示例已注释在 `go/login/etc/login.yaml` 里(2026-10-18 改为 `AppSecretEnv` 写法)
- ✅ §C.1 mock-server / wire-format 单元测试在 main(LoginEndpointIntegrationTest 7/7 含 wechat / qq)
- ✅ §D 坑位踩过的对照(`providers_test.go` 验证过 errcode 解析)
- ✅ **`Endpoint` 配置字段** — `WeChatProvider` / `QQProvider` 都支持指向自建 sandbox(详见 §G)
- ✅ **本地 sandbox mock server**(`go/login/cmd/sandbox_mock`)— 在没真凭证时也能跑 wechat/qq 端到端
- ⚠️ §C.2 / §C.3 真沙箱跑数 — 留位,需要拿到真凭证的人执行后填回本文 §C.2 末尾的 fixtures

---

## G. 没真凭证的端到端验证(用 sandbox_mock)

如果暂时拿不到 WeChat Open Platform / QQ Connect 的真 AppId/AppSecret(申请审批 / 网络出口 / 公司流程都可能延迟),不要原地等——本仓库自带一个**字节级模拟**这两个 API 的 mock 服务,可以验证整条 `Client → Gateway → gRPC login → Provider → HTTP API → 账号映射 → token` 链路的正确性,只缺"流量真到了腾讯/微信服务器"这最后一公里。

### G.1 起 mock server

```bash
cd /f/work/mmorpg
go build -o bin/sandbox-mock ./go/login/cmd/sandbox_mock
./bin/sandbox-mock -addr :18090 &
# 默认 :18090,可改 -addr :19091 之类的避端口冲突
```

mock 暴露的 endpoint(响应字节级对齐真 API):

| 路径 | 模拟 | 特殊 code |
|---|---|---|
| `GET /sns/oauth2/access_token` | `api.weixin.qq.com` | `code` 以 `U` 开头→返回 unionid;`code=EXPIRED`→errcode 40029 |
| `GET /oauth2.0/me` | `graph.qq.com` | `access_token=INVALID`→error 100007 |
| `GET /sns/jscode2session` | `api.weixin.qq.com`(微信小游戏,§H) | `js_code` 以 `U` 开头→返回 unionid;`js_code=EXPIRED`→errcode 40029 |
| `GET /mgplatform/api/apps/jscode2session` | `minigame.zijieapi.com`(抖音小游戏,§H) | 任何 `code` 都返回 unionid;`code=EXPIRED`→错误应答 |
| `GET /healthz` | — | 返回 `ok`,用于 liveness |

**deterministic 哈希**:同一 `code`(WeChat)/ `access_token`(QQ)每次返回**完全相同**的 openid / unionid。这是 §C.4 账号复用验证的前提——不会因为 mock 重启就把"老账号"洗成新账号。

### G.2 让 login 走 mock 而非真 API

`go/login/etc/login.yaml`(必须 `Mode: dev`:内联 AppSecret 与 `http://` Endpoint 只在 dev / test 放行):

```yaml
AuthProviders:
  WeChat:
    AppId: "wx_sandbox_test"
    AppSecret: "any_string_ok_here"      # mock 不校验;仅 Mode=dev 允许内联
    Endpoint: "http://127.0.0.1:18090"   # ← 关键:加这一行就走 mock
  QQ:
    AppId: "100000001"                   # 必须等于 mock 固定返回的 client_id(sandbox_mock 的 sandboxQQClientID)
    Endpoint: "http://127.0.0.1:18090"   # 同一个 mock 服务两条 path 都收
```

`deploy/login-stack.linux/login.yaml` 是同一套写法(Endpoint 指向 `http://sandbox-mock:18090`);它的 QQ AppId 以前写成
`qq_sandbox_appid`,与 mock 的 client_id 不一致,QQ 沙箱登录每次都是 client_id mismatch,2026-10-18 已改为 `100000001`
并由 `go/login/cmd/sandbox_mock/main_test.go` 钉住。

重启 `login`,期望日志:
```
Auth provider registered: wechat (require_unionid=true endpoint_override="http://127.0.0.1:18090")
Auth provider registered: qq (request_unionid=true require_unionid=true endpoint_override="http://127.0.0.1:18090")
```

### G.3 端到端 curl

**WeChat 首次登录 — 期望拿 unionid + 进游戏**:

```bash
curl -s -X POST http://127.0.0.1:8081/api/login \
  -H 'Content-Type: application/json' \
  -d '{"zone_id":1,"auth_type":"wechat","auth_token":"Utest1","device_id":"sandbox-1"}'
```

期望返回 `code:0` + access/refresh token。检查 Redis:
```bash
docker exec redis redis-cli --scan --pattern 'account:wx_unionid_*'
# 应看到 account:wx_unionid_<12 位十六进制>
```

**第二次同 code(账号复用)** — 期望同一 player_id:
```bash
curl -s -X POST http://127.0.0.1:8081/api/login -H 'Content-Type: application/json' \
  -d '{"zone_id":1,"auth_type":"wechat","auth_token":"Utest1","device_id":"sandbox-1"}'
# 返回里的 player_id 应该和上一次一致
```

**WeChat code 不以 U 开头(无 unionid)— 缺省期望拒绝**(`RequireUnionId: true`):
```bash
curl -s -X POST http://127.0.0.1:8081/api/login -H 'Content-Type: application/json' \
  -d '{"zone_id":1,"auth_type":"wechat","auth_token":"plain_code","device_id":"sandbox-1"}'
# 期望: {"code":500,"message":"upstream_err=2035"},login 日志 "unionid missing",Redis 里没有新的 account:wx_openid_*
```
要验证回落路径,先在 WeChat 段显式加 `RequireUnionId: false` 再重启 login,同一请求才会建出 `account:wx_openid_*`。

**WeChat code 过期**:
```bash
curl -s -X POST http://127.0.0.1:8081/api/login -H 'Content-Type: application/json' \
  -d '{"zone_id":1,"auth_type":"wechat","auth_token":"EXPIRED","device_id":"sandbox-1"}'
# 期望: {"code":401,"message":"upstream_err=2000"},login 日志 "wechat: errcode=40029"
```

**QQ 端到端**:
```bash
curl -s -X POST http://127.0.0.1:8081/api/login -H 'Content-Type: application/json' \
  -d '{"zone_id":1,"auth_type":"qq","auth_token":"qq_token_1","device_id":"sandbox-1"}'
# 期望: code:0,Redis 里有 account:qq_qq_unionid_*(账号 = "qq_" + mock 返回的 "qq_unionid_<hash>")

curl -s -X POST http://127.0.0.1:8081/api/login -H 'Content-Type: application/json' \
  -d '{"zone_id":1,"auth_type":"qq","auth_token":"INVALID","device_id":"sandbox-1"}'
# 期望: {"code":401,"message":"upstream_err=2000"},login 日志 "qq: error=100007"
```

### G.4 从 mock 切回真 API

只需:
1. 拿到真 `AppId` / `AppSecret`:AppId 写进配置,AppSecret 放进 K8s Secret,配置改写 `AppSecretEnv`(删掉内联 `AppSecret`)
2. **删掉 `Endpoint:` 这行**(或留空字符串)— 代码会自动回 `https://api.weixin.qq.com` / `https://graph.qq.com`
3. 去掉 `Mode: dev`(生产模式),重启 login

整个 chain 的 wire format 完全没变,所以 mock 验过的同一组 curl 命令直接对着真 API 也能跑通。

> **何时该用 mock vs 真 API?**
> - **CI / 集成测试 / 不可联网的环境** → mock(deterministic + 离线)
> - **真凭证已就绪 + 网络出口已开 + 准备最终验收** → 真 API(只验 mock 没法覆盖的:网络白名单 / 限流 / 真 unionid 政策)
> 二者都跑过一遍,才算 §C.4 真正打勾。

---

## H. 小游戏登录(微信小游戏 / 抖音小游戏,2026-11-12 起)

小游戏是另外两个 auth_type(`wechat_minigame` / `douyin_minigame`),不是 `wechat` 的一个开关:小游戏的 code 只能用小游戏自己的
AppId / AppSecret 走 code2Session,发到 App 的接口换不出来。设计、错误分类与没做的事项见
[第三方登录设计 §十一](../design/third-party-login-end-to-end-design.md)。代码未编译,下面的期望值按代码推出,没有跑过。

**K8s 部署脚本还不渲染这两段**:`k8s_deploy.ps1` 没有对应的部署参数、Secret 与防误关。目前只能用本机 `login.yaml` 或 linux 登录栈
(`deploy/login-stack.linux/login.yaml` 已带这两段,指向 mock)。

### H.1 凭证与接口

| 渠道 | 要拿什么 | 备注 |
|---|---|---|
| 微信小游戏 | 小游戏管理后台(<https://mp.weixin.qq.com/>)开发设置里的 AppID 与 AppSecret | 要有 unionid,小游戏得在微信开放平台(<https://open.weixin.qq.com/>)绑定到开放平台账号;与 App 绑在同一个账号下,两边才是同一个游戏账号 |
| 抖音小游戏 | 抖音开放平台开发者后台里小游戏应用的 AppID 与 AppSecret | 客户端用 `tt.login({force: true})`;没登录抖音的匿名用户登不了 |

我们用到的接口(`go/login/internal/logic/pkg/auth/minigame_providers.go`):

```
GET https://api.weixin.qq.com/sns/jscode2session
    ?appid=<AppId>&secret=<AppSecret>&js_code=<wx.login 的 code>&grant_type=authorization_code
返回: { openid, session_key, unionid?, errcode?, errmsg? }

GET https://minigame.zijieapi.com/mgplatform/api/apps/jscode2session
    ?appid=<AppId>&secret=<AppSecret>&code=<tt.login 的 code>          请求头 Content-Type: application/json
返回: { error, openid, session_key, unionid, anonymous_openid };失败时 error 非 0,另带 errcode / errmsg
```

应答里的 `session_key` login 不解析、不记录。

### H.2 配置

```yaml
AuthProviders:
  WeChatMiniGame:
    AppId: "wxfedcba9876543210"                       # 小游戏的 AppID;与 WeChat.AppId 相同会拒绝启动
    AppSecretEnv: "LOGIN_WECHAT_MINIGAME_APP_SECRET"  # 环境变量名,规则同 §B 的 WeChat 段
    # RequireUnionId: true                            # 缺省 true:没有 unionid 就拒绝登录
  DouyinMiniGame:
    AppId: "tt0123456789abcdef"
    AppSecretEnv: "LOGIN_DOUYIN_MINIGAME_APP_SECRET"
```

内联 `AppSecret`、`http://` 的 `Endpoint`、`${...}` 占位符的放行与拒绝规则,和 §B 的表完全相同。

### H.3 用 sandbox_mock 端到端

`go/login/etc/login.yaml`(`Mode: dev`)在 §G.2 的基础上加:

```yaml
AuthProviders:
  WeChatMiniGame:
    AppId: "wx_sandbox_minigame"         # 不能与 WeChat.AppId 相同
    AppSecret: "any_string_ok_here"
    Endpoint: "http://127.0.0.1:18090"
  DouyinMiniGame:
    AppId: "tt_sandbox_minigame"
    AppSecret: "any_string_ok_here"
    Endpoint: "http://127.0.0.1:18090"
```

重启 `login`,期望多出两行日志:
```
Auth provider registered: wechat_minigame (require_unionid=true endpoint_override="http://127.0.0.1:18090")
Auth provider registered: douyin_minigame (require_unionid=true endpoint_override="http://127.0.0.1:18090")
```

```bash
# 微信小游戏:期望 code:0。mock 对 App 的 code 与小游戏的 js_code 用同一条规则派生 unionid,
# 所以它和 §G.3 里 auth_type=wechat、auth_token=Utest1 是同一个账号,返回的角色列表相同。
curl -s -X POST http://127.0.0.1:8081/api/login -H 'Content-Type: application/json' \
  -d '{"zone_id":1,"auth_type":"wechat_minigame","auth_token":"Utest1","device_id":"sandbox-1"}'

# 没有 unionid(小游戏没绑定开放平台账号):期望 {"code":500,"message":"upstream_err=2035"},login 日志 "unionid missing"
curl -s -X POST http://127.0.0.1:8081/api/login -H 'Content-Type: application/json' \
  -d '{"zone_id":1,"auth_type":"wechat_minigame","auth_token":"plain_code","device_id":"sandbox-1"}'

# code 过期:期望 {"code":401,"message":"upstream_err=2000"},login 日志 "wechat_minigame: errcode=40029"
curl -s -X POST http://127.0.0.1:8081/api/login -H 'Content-Type: application/json' \
  -d '{"zone_id":1,"auth_type":"wechat_minigame","auth_token":"EXPIRED","device_id":"sandbox-1"}'

# 抖音小游戏:期望 code:0,账号 dy_unionid_<12 位十六进制>
curl -s -X POST http://127.0.0.1:8081/api/login -H 'Content-Type: application/json' \
  -d '{"zone_id":1,"auth_type":"douyin_minigame","auth_token":"dy_code_1","device_id":"sandbox-1"}'

# 抖音 code 过期:期望 {"code":401,"message":"upstream_err=2000"},login 日志 "douyin_minigame: error=1 errcode=40018"
# (40018 是 mock 的示例值,抖音文档没有给出错误码表)
curl -s -X POST http://127.0.0.1:8081/api/login -H 'Content-Type: application/json' \
  -d '{"zone_id":1,"auth_type":"douyin_minigame","auth_token":"EXPIRED","device_id":"sandbox-1"}'
```

账号落在全局 SQL 账号目录(data_service),不在 Redis。§C.3 与 §G.3 里「查 Redis `account:*`」的步骤是 2026-10-21 账号目录迁到
SQL 之前的写法,现在查不到,待订正。

### H.4 上真渠道前要复核的两件事

- 微信 errcode 45011 在 code2Session 上按「单个用户限频」处理,不算渠道故障;2026-11-12 没能打开微信官方页面复核。
- 抖音没有可核对的错误码表,`error` / `errcode` 非 0 一律按凭证无效(回 401、不告警)。联调时把 AppSecret 配错一次,
  记下实际的 errcode,再决定要不要把它列成渠道故障。

---

## 关联文档
- [auth-provider-framework.md](../design/auth-provider-framework.md) §How to Add — 加新 provider 的 4 步骤(Apple Sign-in / Google Play Games 都按这个加)
- [third-party-login-end-to-end-design.md](../design/third-party-login-end-to-end-design.md) — 完整设计 + UserBindings 表(账号合并未来要用)
- [dual-token-authentication.md](../design/dual-token-authentication.md) — 第三方 token / access / refresh 三层关系
- 代码: `go/login/internal/logic/pkg/auth/providers.go`(WeChatProvider / QQProvider)
- 代码: `go/login/internal/logic/pkg/auth/minigame_providers.go`(WeChatMiniGameProvider / DouyinMiniGameProvider,§H)
- 代码: `go/login/internal/svc/auth_init.go`(provider 注册)
- 代码: `java/gateway_node/.../grpc/LoginRpcClient.java`(auth_type/auth_token wire 透传)
- 部署: `tools/scripts/k8s_deploy.ps1`(`-LoginWeChatAppId` 等六个参数、`Assert-LoginAuthProviderClusterState` 防误关)

---

## 2026-10-18 订正说明

本次随「第三方登录上线前缺口」修复订正(代码见 `go/login/internal/config/auth_providers.go`、`go/login/internal/logic/pkg/auth/providers.go`):

- §B 原写「go-zero 1.10 默认展开 `${VAR}`」与「`./login.exe -f login.yaml`」都不对:本仓 go-zero 只有 `conf.UseEnv()` 才展开,
  login 没带;配置参数是 `-loginService`。改为 `AppSecretEnv` 写法,`${...}` 字面量启动期拒绝。
- §C.3 账号目录键前缀是 `account:`,不是 `account_data:`;账号目录只在 Redis,没有 MySQL `UserAccounts` 表可查。
- §C.2 / §G.3 的失败返回按网关实际行为改为 `upstream_err=<tip 码>`;渠道故障与签发失败有了独立故障码 2035 / 2036。
- §D.4 / §G.3:拿不到 unionid 缺省拒绝,不再回落 openid。
- §B K8s(同日补):包装入口(`dev_tools.ps1` 的 k8s-zone-up / k8s-all-up)照传六个第三方登录参数;
  线上已开的第三方登录会被本次参数关掉或改掉时 fail-closed,确要改加 `-ConfirmLoginAuthProviderChange`(判据在
  `tools/scripts/lib/k8s_client_entry.ps1` `Get-LoginAuthProviderDrift`)。以前经包装入口重部署会静默关掉微信 / QQ 登录并删掉 Secret `login-oauth`。
- §C.2 / §G.3(2026-10-19 补):网关按 `Tip.xlsx` 的 `fault` 列定性后,2035 / 2036 / 2021 / 2022 回 `code:500`,不再是 401;
  删去「对 in-band 故障仍回 401 是已知残留」。微信 code 时效按开放平台移动应用文档改为 10 分钟(原写 5 分钟)。

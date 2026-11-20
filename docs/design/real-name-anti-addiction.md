# 实名认证与未成年人防沉迷

> **状态：设计稿（2026-11-19），未落码。** 规则与平台能力已查清（来源见 §10）；§5 的服务端方案只对 App 形态生效，开不开工等负责人答 §7 第 1 条。§8 的文件清单超过 30 个，落码前要负责人对这一批的明确同意（AGENTS §10.2）。

## 0. 结论

- **国家系统只做两件事**：核身份（姓名 + 身份证号 → 用户唯一标识 pi）、收上下线数据。它不替游戏拦人：未成年人的时段和充值限额由游戏方或渠道平台自己执行。
- **微信 / 抖音小游戏（国内首发形态）**：平台已经接入国家系统，用平台账号的实名信息执行未成年人时段；微信公告写明付费限额也由平台执行。开发者不收身份证、不调国家接口。我们要做的只有手续（§9）：拿版号，在国家系统注册企业、接入游戏，把两个平台加为渠道并授权「实名上报」「数据上报」。**服务端不用写代码。**
- **App 形态（微信 / QQ App 登录、口令登录）**：平台不管，游戏方必须自己接国家接口，并在服务端执行时段和充值限额。§5 是这一形态的方案。
- **海外（apple / google）**：不适用，按部署关闭。

## 1. 现行规则

| 规则 | 内容 | 来源 |
|---|---|---|
| 实名 | 所有网络游戏接入国家系统；用户必须实名注册和登录；不得以任何形式（含游客模式）向未实名用户提供服务 | 国新出发〔2021〕14 号 |
| 时段 | 只在周五、周六、周日和法定节假日每日 20:00–21:00 向未成年人提供 1 小时服务，其余时间不得提供 | 同上；取代 2019 年通知里「22 时至 8 时不得提供、每日 1.5 / 3 小时」的规定 |
| 单次与每月充值 | 未满 8 周岁不得提供付费服务；8–16 周岁单次 ≤ 50 元、每月 ≤ 200 元；16–18 周岁单次 ≤ 100 元、每月 ≤ 400 元 | 国新出发〔2019〕34 号，2021 年通知没有改 |
| 单日累计消费 | 网络服务提供者要合理限制不同年龄段未成年人的单次消费额和单日累计消费额，没有给数字 | 《未成年人网络保护条例》（2024-01-01 施行）。数字由负责人定，已列在充值设计 §18 第 7 条 |

拿不准的：

- **「法定节假日」的口径**：只算法定的那几天，还是国务院放假通知里的全部放假日（含调休凑出来的连休）。两种口径只在「放假的周一到周四」上有差别。见 §7 第 2 条。
- **2026-09-18 网信办新文件**：搜索摘要显示是征求意见稿（反馈截止 2026-10-17），内容没有核实。征求意见稿不据此实现，和充值设计附录 A 对两份旧征求意见稿的处理一致。
- 两份通知的原文本轮没有打开，条款内容取自政府网站转载摘要与新闻报道；充值金额与微信 2020 年平台公告里的数字一致。

## 2. 国家系统接口

来源：国家新闻出版署机关服务中心《网络游戏防沉迷实名认证系统接口对接技术规范》V1.9（2021-08-20），本轮通读了全文。

**接入前提**：有版号；在 wlc.nppa.gov.cn 企业端注册、接入游戏，拿到 `appId`、`secretKey` 和 `bizId`（与游戏备案识别码一致）；调用方 IP 要在白名单里（不在回 1005）；企业端的「接口测试」要全部通过。

| 接口 | 地址与方法 | 限流 |
|---|---|---|
| 实名认证 | `POST https://api.wlc.nppa.gov.cn/idcard/authentication/check`，明文体 `{ai, name, idNum}` | 100 QPS |
| 认证结果查询 | `GET http://api2.wlc.nppa.gov.cn/idcard/authentication/query?ai=` | 300 QPS |
| 上下线上报 | `POST http://api2.wlc.nppa.gov.cn/behavior/collection/loginout`，明文体 `{collections: [...]}` | 10 QPS |

超限会被限流 1 分钟；报文时间戳超时 5 秒，建议客户端超时 5 秒。

- **加密**：请求体用 `secretKey` 做 AES-128-GCM，Base64 后放进 `{"data": "..."}`。规范给了一组样例（密钥、明文、密文），实现的单测直接用它。
- **签名**：请求头带 `appId`、`bizId`、`timestamps`（毫秒）、`sign`。把除 `sign` 之外的系统参数和 URL 参数按键名字典序拼成 `键值键值…`，后面接请求体原文，最前面加 `secretKey`，取 SHA-256 十六进制。规范同样给了样例和期望值。
- **认证结果**：`status` 0 成功、1 认证中、2 失败；成功时带 `pi`。认证中的结果在 48 小时内可以用 `ai` 查；查询成功 300 秒后结果被删；没被删的 `ai` 不能再提交，所以 `ai` 每次提交都新生成。
- **pi 的结构**：38 位，前 6 位是出生日期（26 进制，字符为 0–9 加 a–p），后 32 位是系统编码。规范的样例前缀 `1hpfml` 解出来是 20100101。**年龄从 pi 算，不存身份证号和姓名。**
- **上报字段**：`no`（批内序号 1–128）、`si`（会话标识，最长 32 位，一次上线配一次下线）、`bt`（0 下线 / 1 上线）、`ot`（秒）、`ct`（0 已认证用户 / 2 游客）、`pi`、`di`（游客设备标识）。每次最多 128 条；一批里最早的 `ot` 与上报时刻相差要小于 180 秒，最晚的 `ot` 要早于上报时刻。
- **错误码**：1001–1012 系统类（签名、IP、限流、权限等），2001–2006 实名类（2001 身份证号格式错、2005 姓名不合法、2004 编码被占用等），3001–3010 上报类，4001–4003 接口测试类。

## 3. 小游戏平台能做什么

| | 微信小游戏 | 抖音小游戏 |
|---|---|---|
| 实名 | 平台统一做，绑定微信账号；未实名的用户由平台引导认证 | 平台统一做；关闭游客模式，未实名用户不能登录小游戏 |
| 未成年人时段 | 平台执行（2020 年按当时规则上线，现行时段规则由平台跟进） | 平台 SDK 执行，2021-09-01 公告写明「开发者无需操作」 |
| 未成年人充值限额 | 平台执行（2020 年公告写明 < 8 岁不能付费和两档金额）；另有「未成年人支付退款」入口，开发者要在 2 个工作日内处理 | **页面没写，待核实** |
| 开发者在国家系统里的操作 | **没查到官方页面，待核实**（可能也要添加渠道） | 已获版号的游戏添加渠道「北京抖音信息服务有限公司」，勾选「实名上报」「数据上报」，等平台审批（2023 年 FAQ 第 8 问） |
| 是否把实名状态或年龄给开发者 | 没查到相关接口 | 没查到相关接口 |

平台不把年龄给开发者，所以小游戏用户的防沉迷只能靠平台，服务端也拦不了。这和平台的做法一致：限制落在平台的运行容器和支付上，客户端到点由平台关掉，充值由平台拒绝。

## 4. 各登录形态怎么处理

| auth_type | 实名 | 时段 | 充值限额 | 上下线上报 |
|---|---|---|---|---|
| `wechat_minigame`、`douyin_minigame` | 平台 | 平台 | 平台（抖音待核实） | 平台（凭渠道的数据上报权限） |
| `wechat`、`qq`（App）、`password`、`satoken` | 我们接国家接口（§5.2） | login 执行（§5.3、§5.4） | go/payment 执行（§5.6） | login 上报（§5.5） |
| `access_token` | 沿用签发令牌时的渠道（令牌记录里的 `auth_type`） | 同左 | 同左 | 同左 |
| `apple`、`google` | 海外部署，`AntiAddiction.Enabled=false` | — | — | — |

- **按部署开关**：国内部署 `AntiAddiction.Enabled=true`。开着时，凡不在 `PlatformManagedAuthTypes` 里的 auth_type 都要我们自己实名，新加的登录方式默认也要（fail-closed）。
- **按本次登录的渠道判，不按账号判**：微信 App 和微信小游戏是同一个账号 `wx_<unionid>`（第三方登录设计 §11.1）。同一个账号从小游戏进来由平台管，从 App 进来由我们管。
- **账号已经有 pi 时，任何渠道都按我们的规则限**：我们知道年龄，多一道限制不会违规，规则也只有一条。

## 5. 服务端方案（App 形态）

### 5.1 数据

- **账号目录加一列**：`account_directory.real_name_pi VARBINARY(38) NOT NULL DEFAULT ''`（proto `account_directory` 第 3 号字段）。空串表示没实名。只写一次：已经有值时，写相同的值算成功，写不同的值拒绝（换实名信息走客服）。一个人可以有多个账号，pi 不做唯一键。
- **不存姓名和身份证号**：校验用过就丢，日志里也不出现（《个人信息保护法》的最少必要原则）。年龄每次由 pi 和当前时间算，满 18 岁自动变成成年。
- **认证中的 ai** 放 login 的 Redis：`login_realname_pending:{<账号 tag>}:<account>`，TTL 48 小时。丢了的代价是玩家重新提交一次。
- **每日提交上限**：`login_realname_attempts:{<账号 tag>}:<account>:<北京日期>`，缺省 5 次。不设上限的话，我们的接口能被拿来批量核验别人的姓名和身份证号是否匹配。
- **逻辑库自治**（AGENTS §7.6）：账号目录归 data_service，login 和 payment 只走 RPC。
  - `ReadAccountDirectory` 的应答带 `real_name_pi`（login 已经在登录时读目录，不加调用）；
  - 新增 `SetAccountRealName(account, pi)`，只许 login 调；
  - 新增 `GetAccountRealName(player_id)`，只许 payment 调，给充值闸用（§5.6，要和充值会话对齐）。
- **表结构变更**：`CREATE TABLE IF NOT EXISTS` 不会给已有的表加列。项目没上线，验证前清库重建。

### 5.2 实名提交

```text
/api/login 回 anti_addiction.real_name_required=true(令牌照发,提交要用它)
  → 客户端弹实名窗,POST /api/real-name {access_token, name, id_number}
  → 网关 → login.SubmitRealName
      1. 令牌解出账号与签发时的渠道;本渠道不需要实名,或账号已有 pi → 直接回当前状态
      2. 本地校验:身份证 18 位、校验码、出生日期合法,姓名 1–32 字。不过关回「信息有误」,不调国家接口
      3. 今日提交次数未满上限才往下走
      4. 新生成 ai(32 位随机十六进制),调实名认证接口
         - status=0:SetAccountRealName(pi),回最新状态
         - status=1:记下待查 ai,回「认证中,请稍后重新登录」
         - status=2:回「信息有误」
      5. 国家接口的系统类错误、网络故障、限流 → 故障码(Tip.xlsx fault=1),客户端稍后重试
```

- **认证中的收尾**：下一次 `/api/login` 看到待查 ai，就调查询接口；查到结果按第 4 步处理。不加单独的查询接口。
- **为什么走网关的 HTTP，不走 gate**：实名是账号级的事，在选区、排队之前做完；网关本来就把 `/api/login` 转给 login。

### 5.3 登录与进场拦截

- **`/api/login`**（HTTP）：应答新增 `anti_addiction{real_name_required, minor, play_deadline_ms}`，只用来让客户端决定下一步，不拦。
- **gate 上的 Login**（`auth_type=access_token` 绑定会话）：唯一的拦截点。它是 login_session 的唯一写入点，CreatePlayer、EnterGame 读不到 login_session 就进不了游戏（`loginlogic.go` 的注释写明了这一点）。
  - 本渠道要实名而账号没有 pi → 回 `kLoginRealNameRequired`；
  - 未成年且现在不在可玩时段 → 回 `kLoginMinorNotInPlayTime`；
  - 未成年且在时段内 → 照常绑定，写 `login_anti_addiction:{<连接身份>}`，应答带 `play_deadline_ms`。
- **AssignGate 不改**：正常客户端在 `/api/login` 之后就停了；改过的客户端排完队还是在 gate 的 Login 被拦下。

### 5.4 到点下线与提前提醒

- **可玩日**：周五、周六、周日，或 `AntiAddiction.ExtraPlayDays` 里的日期（法定节假日中不落在周五到周日的那些天，运维按每年的国务院放假通知填）。名单空着就只有周五到周日，偏严，不违规。
- **时段**：20:00–21:00，北京时间。规则是法规定的，写成常量；用 `time.FixedZone("CST", 8*3600)`，不依赖容器里的时区库。
- **到点下线**：复用 gate 定期代发的 RenewSession（`renewsessionlogic.go`，间隔由 login 在应答里给）。
  - 连接有 `login_anti_addiction` 记录、离截止还有时间时，把 `renew_after_seconds` 缩到截止时刻；
  - 到点之后的那次续租，login 用现有的踢线命令（`svc/gate_command.go` 的 `KickPlayerEvent`）踢掉这条连接。
  - gate 把续租间隔夹到至少 5 秒、最多提前五分之一（`login_session_renewal.cpp` 的 `NextInterval`），所以最晚在 21:00 之后约 5 秒踢下线。
- **踢线原因**：`KickPlayerEvent` 加 `reason_tip`（0 = 原来的「账号在别处登录」），gate 的 `KickPlayerEventHandler` 用它填 `GameKickPlayerRequest.reason`。gate 只改这一行。
- **提前提醒**：客户端按 `play_deadline_ms` 在剩 10、5、1 分钟时提示。服务端不推送：提醒漏了不违规，真正的限制是服务端的踢线。

### 5.5 上下线上报

- **只报我们自己实名的会话**（自有渠道、账号有 pi）；小游戏会话由平台报，`ct=2` 的游客永远不用。
- **记录**：gate 上 Login 绑定时写 `login_anti_addiction:{<连接身份>}` = `{pi, deadline_ms, report}`，TTL 跟 login_session 一样，RenewSession 时续期。
  - 绑定时排一条上线（`bt=1`）；
  - Disconnect 时读出并删掉这条记录，`report` 为真就排一条下线（`bt=0`）；
  - `si` 取连接身份做 SHA-256 后的前 32 位十六进制，上线和下线一致。
- **上报器**：每个 login 副本一个，进程内攒批：每批 ≤ 128 条，两次请求至少间隔 1 秒，同一时刻只有一个请求在途。失败就重试，直到这批里最早的事件已过 150 秒，丢弃并计数（`login_nppa_report_dropped_total`）。
- **限流**：每个副本最多 1 QPS，login 副本数 ≤ 10 时合计不超过 10 QPS，容量约每秒 1280 条上下线。副本要超过 10 个时先联系国家系统管理员调流控（规范备注第 4 条），或改成集中上报。
- **已知局限**：进程崩溃会丢还没发出的事件。180 秒的时限让持久化队列的收益很小，这里先不做。

### 5.6 充值限额

- 闸在 go/payment（充值设计 §12），归「游戏应用支付系统接入」会话。本方案只提供数据：`GetAccountRealName(player_id)` 回 pi；从 pi 算出生日期、年龄段的纯函数放 `go/shared/antiaddiction`，login 和 payment 共用一份。
- 小游戏渠道没有 pi，由平台执行限额。go/payment 已经是这个口径（支付会话 2026-11-19 确认）：`Payment.Compliance.MinorSpendingGate` 目前只认 `none`，非 dev / test 模式下 `config.Validate` 拒绝启用 wechat_pay、alipay、apple、google 四个 App 真钱渠道，两个小游戏渠道可以开。
- **接入点**（支付会话给出，那边本轮不预留接缝）：`go/payment/internal/config/config.go` 的 `Validate` 给 `MinorSpendingGate` 加第二个取值，并放开四个 App 渠道；`go/payment/internal/core/service.go` 的 `CreateIntent` 在 `openIntent` 之前按玩家和订单金额判限额，拒绝时回新的 payment_error tip。这两处等 data_service 接口和 `go/shared/antiaddiction` 落码之后改，归本任务第二批，或由负责人另派。
- 单日上限的数字仍是充值设计 §18 第 7 条的待决项。

### 5.7 客户端

- `/api/login` 回 `real_name_required` 时弹实名窗（姓名、身份证号、用途说明），提交到 `/api/real-name`；认证中提示稍后重新登录，失败提示核对信息。
- 未成年人进游戏前提示可玩时段；进游戏后按 `play_deadline_ms` 倒计时提醒。
- 收到原因为 `kLoginMinorPlayTimeOver` 的踢线：显示原因、回登录页、不自动重连。
- 小游戏包不用做：平台管。

### 5.8 配置（login.yaml）

```yaml
AntiAddiction:
  Enabled: false                 # 国内部署 true;开着时下面的 Nppa 必须填全,否则拒绝启动
  PlatformManagedAuthTypes: [wechat_minigame, douyin_minigame]
  ExtraPlayDays: []              # "2026-10-01" 这样的北京日期
  DailySubmitLimit: 5
  Nppa:
    AppId: ""
    BizId: ""
    SecretKeyEnv: LOGIN_NPPA_SECRET_KEY   # 只写变量名,值从 Secret 注入
    CheckUrl: https://api.wlc.nppa.gov.cn/idcard/authentication/check
    QueryUrl: http://api2.wlc.nppa.gov.cn/idcard/authentication/query
    LoginoutUrl: http://api2.wlc.nppa.gov.cn/behavior/collection/loginout
```

查询和上报的官方地址是 http：报文体已经加密，照规范写，不强制 https。接口测试阶段把三个地址换成企业端给的测试地址。

### 5.9 tip 码（login_error 段，从 Tip.xlsx 导表）

| 码名 | 文案 | 故障 |
|---|---|---|
| `kLoginRealNameRequired` | 请先完成实名认证 | |
| `kLoginRealNameInvalid` | 姓名或身份证号有误，请核对后重试 | |
| `kLoginRealNamePending` | 实名认证中，请稍后重新登录 | |
| `kLoginRealNameTooManyAttempts` | 今日实名认证次数已达上限，请明天再试 | |
| `kLoginRealNameAlreadyBound` | 该账号已完成实名认证 | |
| `kLoginRealNameServiceUnavailable` | 实名认证服务繁忙，请稍后重试 | 1 |
| `kLoginMinorNotInPlayTime` | 未成年人仅可在周五、周六、周日和法定节假日 20:00–21:00 进行游戏 | |
| `kLoginMinorPlayTimeOver` | 今日游戏时间已结束 | |

## 6. 与其它会话的分工

- **go/payment 与充值设计 §12**：归「游戏应用支付系统接入」会话。2026-11-19 已对齐：`GetAccountRealName` 由本任务在 data_service 加；那边不碰账号目录，也不改 §12（只追加 §22）；限额闸的两处接入点见 §5.6。
- **Tip.xlsx**：支付会话 2026-11-19 正在追加 `//payment_error base=31000` 段。本任务的码进 login_error 段，等那边写完再动这张表，两边不同时保存 xlsx。
- **小游戏客户端连 gate（WebSocket）**：互不影响。小游戏包不做实名。
- **Java 版**：只在 Java 仓 `PARITY.md` 登记待做。两版共享的客户端契约是 `/api/real-name`、`LoginResponse.anti_addiction`、新 tip 码与踢线原因。

## 7. 待负责人确认

1. **国内首发是否只有小游戏形态？** 只有小游戏的话，§5 推迟到开放 App 形态之前做（推荐），先办 §9 的手续；App 形态也首发，就按 §8 落码。
2. **法定节假日的口径**：推荐按国务院放假通知里的全部放假日填 `ExtraPlayDays`（腾讯等大厂公开的寒暑假可玩日历按放假日排，本轮没有核实到原文）。
3. **实名提交每日上限**：推荐 5 次。
4. **K8s 部署参数是否同批**：推荐同批，否则线上开不了。

## 8. 文件清单（App 形态服务端，约 50 个）

| 部分 | 文件 |
|---|---|
| 协议（4） | `proto/common/database/rollback_database_table.proto`（`account_directory` 第 3 号字段）；`proto/data_service/data_service.proto`（应答带 pi，两个新 RPC）；`proto/login/login.proto`（`AntiAddictionStatus`、`LoginResponse` 新字段、`SubmitRealName`）；`proto/contracts/kafka/gate_event.proto`（`KickPlayerEvent.reason_tip`） |
| tip（1） | `data/tip/Tip.xlsx`（§5.9 八个码，生成物由导表器出） |
| Go 共用（2） | 新增 `go/shared/antiaddiction/antiaddiction.go` 与测试：pi 解出生日期、年龄段、可玩时段、身份证校验码 |
| data_service（4） | `internal/store/account_directory_store.go`、`internal/server/account_directory.go` 与两处测试 |
| login（约 20） | `internal/config/config.go` 与新增的配置门禁和测试；新增 `internal/logic/pkg/nppa/`（客户端、上报器与测试，测试用规范里的加密和签名样例）；`clientplayerlogin/` 的 `loginlogic.go`、`account_directory.go`、`renewsessionlogic.go`、`disconnectlogic.go`，新增 `anti_addiction.go`、`submitrealnamelogic.go` 与测试；`svc/gate_command.go`（踢线原因）、`svc/servicecontext.go`；`pkg/auth/provider.go`、`providers.go`（结果带渠道）；`shared/redisslot/login_keys.go`；`etc/login.yaml`。goctl 包装由 `go/build.ps1` 重生成 |
| C++ gate（1–2） | `cpp/nodes/gate/handler/event/gate_event_handler.cpp`，可能加一个用例 |
| 本仓 Java 网关（约 6） | `/api/real-name` 的 controller、service、`LoginRpcClient`、请求与应答 DTO、测试 |
| 客户端（约 6） | `GatewayHttpClient.cs`、`GameClient.cs`、新增实名窗与倒计时提示、踢线原因处理、EditMode 测试 |
| K8s（约 3） | `tools/scripts/k8s_deploy.ps1`（AppId、BizId、Secret `login-nppa`）、`dev_tools.ps1`、契约测试 |
| go/payment（2 + 测试，可另派） | `internal/config/config.go`、`internal/core/service.go`（§5.6 接入点），等支付会话本轮改完再动 |
| 文档（约 5） | 本文、`docs/README.md`、`docs/PROGRESS.md`、第三方登录设计 §七、Java 仓 `PARITY.md` |

分两批交 Codex：第一批协议、tip、Go 共用、data_service、login、gate；第二批网关、客户端、K8s。

## 9. 负责人要亲自办的手续（Claude 不能代办）

1. 版号。
2. 在 wlc.nppa.gov.cn 企业端注册，接入游戏，拿到 `bizId`、`appId`、`secretKey`。`secretKey` 不要发到聊天里，写进 Secret 后告诉 Claude 变量名。
3. 在企业端添加渠道：抖音填「北京抖音信息服务有限公司」，勾选「实名上报」「数据上报」；微信要不要加、填哪个主体，待核实。
4. App 形态要接国家接口时：登记服务器出口 IP（K8s 要固定的出网 IP），跑完企业端的接口测试。

## 10. 来源

- 国家系统接口：[网络游戏防沉迷实名认证系统接口对接技术规范 V1.9（PDF）](https://wlc.nppa.gov.cn/2022/02/18/a080ffa8a7ff4005b7c3dbbfcce93323.pdf)，2026-11-19 通读全文。
- 2021 年通知：[关于进一步严格管理 切实防止未成年人沉迷网络游戏的通知](https://www.gov.cn/zhengce/zhengceku/2021-09/01/content_5634661.htm)（链接取自充值设计附录 A，本轮没有打开，内容取自[网信办转引](https://www.cac.gov.cn/2021-10/29/c_1637102915442072.htm)与[中国法律译文网](https://www.chinalawtranslate.com/minors-video-games-2021/)的摘要）。
- 2019 年通知：[关于防止未成年人沉迷网络游戏的通知](https://www.gov.cn/zhengce/2019-11/05/content_5448848.htm)（同上，没有打开；生效日期与「仍有效」取自 [China Justice Observer](https://www.chinajusticeobserver.com/law/x/preventing-minors-from-indulging-in-online-games-20191101)）。
- 微信小游戏：[微信开放社区公告](https://developers.weixin.qq.com/community/minigame/doc/000e629bcacbe0c1eb94e3f4966008)（页面靠脚本渲染，正文没读到，内容取自搜索摘录）；2020 年接入健康系统的报道：[快科技](https://news.mydrivers.com/1/680/680777.htm)、[IT 之家](https://www.ithome.com/0/480/420.htm)。
- 抖音小游戏：[未成年人防沉迷调整通知（2021-09-01）](https://developer.open-douyin.com/forum/bulletin/post/612ef9e02ec6ae11f4a3e47d)、[小游戏运营类 FAQ（2023-05-29）](https://developer.open-douyin.com/forum/share/post/647461cda2c591e9aa1ee8f4)，两页正文都读到了；[渠道绑定指引](https://developer.open-douyin.com/docs/resource/zh-CN/mini-game/operation1/agreement-and-norms/norms/realnameauthentication) 靠脚本渲染，正文没读到。
- 仓内：[充值设计](./payment-recharge.md) §12、§18、附录 A；[第三方登录设计](./third-party-login-end-to-end-design.md) §七、§11.1。

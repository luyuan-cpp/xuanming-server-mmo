# 实名认证与未成年人防沉迷

> **状态：2026-11-19 负责人定「App 形态也首发，按清单写」，§5 已落码，未生成、未编译、未测试，待 Codex 验证（命令见 `docs/PROGRESS.md` 同日「实名认证与未成年人防沉迷」落码条目）。** 规则与平台能力的来源见 §10。§8 末尾原先没做的两项都已在 2026-11-19 补落码、未运行：K8s 部署参数，go/payment 的限额闸（§5.6）。2026-11-20 又补了三处原来标为「以后」的：国家接口的全服调用额度（§5.2，原来按副本限速、副本不能超过 10 个）、上报器停机时发完手上的事件（§5.5）、客服重置实名（§5.2 末）；单日充值上限定为单次上限（§5.6）。都未运行。

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
| `wechat_minigame`、`douyin_minigame` | 平台 | 平台 | 平台（抖音待核实）；账号有 pi 时 go/payment 也按我们的规则合并计额（§5.6） | 平台（凭渠道的数据上报权限） |
| `wechat`、`qq`（App）、`password`、`satoken` | 我们接国家接口（§5.2） | login 执行（§5.3、§5.4） | go/payment 执行（§5.6） | login 上报（§5.5） |
| `access_token` | 沿用签发令牌时的渠道（令牌记录里的 `auth_type`） | 同左 | 同左 | 同左 |
| `apple`、`google` | 海外部署，`AntiAddiction.Enabled=false` | — | go/payment 配 `MinorSpendingGate: overseas`（§5.6） | — |

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
- **全服调用额度**（2026-11-20）：国家系统按运营单位限流（实名 100 QPS、查询 300 QPS、上报 10 QPS，超出被封 1 分钟），与 login 有几个副本无关。额度放在登录域 Redis 的一个键里，所有副本共用（`nppa.Quota`，GCRA，按 Redis 服务器的时钟算）。只用到额定值的八成左右：实名 80/s 加 10 个突发、查询 240/s 加 20 个、上报 8/s，任何 1 秒内都不超额。取不到名额时，实名回「繁忙，请稍后重试」，而且先取名额再计当天次数，不占玩家的名额；查询留到下一次登录；上报攒到下一拍。额度读不了按没有名额处理。
- **客服重置实名**（2026-11-20）：`LoginAdmin.ResetAccountRealName`，按角色或账号找到账号，清掉 pi 和认证中的提交，玩家下次登录重新实名。要 `x-admin-token`；必填操作人和理由，与对端地址一起进日志。账号填错了身份证号可能一个角色都建不了，所以也能直接按账号找。已在线的会话不动，下一次登录按新的实名判定。data_service 那一侧是 `ResetAccountRealName`，幂等，只认 login 的证明。

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
- **上报器**：每个 login 副本一个，进程内攒批：每批 ≤ 128 条，每秒一拍，每发一批先从全服共用的上报额度（8/s，见 §5.2「全服调用额度」）里取一个名额。取不到就攒到下一拍，批次自然变大。失败就重试，直到这批里最早的事件已过 150 秒，丢弃并计数（`login_nppa_report_total{result="dropped_age"}`）。
- **限流与扩容**：副本加多少都不超出系统的 10 QPS，全服上限约每秒 1000 条上下线（8 批 × 128 条）。超过这个量要联系国家系统管理员调流控（规范备注第 4 条），与副本数无关。
- **停机**：上报循环结束前，先把队列里剩下的取完，再在 2 秒内一批批发；发不完的计 `dropped_shutdown`。滚动重启不再丢事件。
- **已知局限**：进程被强杀（OOM、kill -9）会丢还没发出的几秒。180 秒的时限让持久化队列的收益很小。要零丢失时，改成登录域 Redis 里的流，由一个副本集中发。
- **会话收尾取不到记录时不重放 Disconnect**（2026-11-19 与断线重连会话对齐）：GETDEL 出错只记日志，Disconnect 照常回成功，这一条下线就不报了。理由有三：
  - 上报器本身至多一次（内存队列、150 秒内重试、进程退出即丢），在 Disconnect 上做到至少一次换不来端到端的保证；
  - GETDEL 结果未知时记录可能已经删掉，重放也取不回来；
  - 让上报这条旁路的抖动决定 Disconnect 成败，会拖住断线收尾（断线租约、player_locator），主次颠倒。
  
  登录域 Redis 真不可用时，Disconnect 关栅栏那一步先失败、回 `Unavailable`，gate 重发时 GETDEL 跟着重试。

### 5.6 充值限额

**2026-11-19 已落码**（「充值按年龄段限额闸接入 go/payment」会话，在支付会话确认 go/payment 与 Tip.xlsx 改完之后），未生成、未编译、未测试，待 Codex 验证（命令见 `docs/PROGRESS.md` 同日「未成年人充值限额闸」条目）。数据接口沿用本方案：`GetAccountRealName(player_id)` 回 pi，出生日期、周岁、单次与每月上限取 `go/shared/antiaddiction`。

- **开关**：`Payment.Compliance.MinorSpendingGate` 三个取值。
  - `none`（缺省）：不接实名，生产模式下 wechat_pay、alipay、apple、google 四个 App 渠道拒启。
  - `realname`（中国大陆）：建意图前读实名并执行限额，四个 App 渠道这时才能在生产模式开。必须配 `DataServiceRpc`；密钥 `MMORPG_ACCOUNT_DIRECTORY_SECRET_PAYMENT` 缺失或不足 32 字节拒启。
  - `overseas`（大陆以外的部署）：不适用国新出发〔2019〕34 号，不读实名，App 渠道照开；当地的未成年人消费规则由商店的家长控制与运营按当地法规执行。和 login 的 `AntiAddiction.Enabled=false` 是同一个部署口径。
- **谁经闸**：闸开着时每个新意图都读实名，渠道只决定「没有实名」怎么办。
  - App 内的渠道（微信支付、支付宝、App Store、Google Play，以及联调用的 devpay；以后新加的默认也算）：没有实名就拒绝。
  - 微信 / 抖音小游戏：平台实名、平台限额，账号没有 pi 照常放行。账号有 pi（同一个人也在 App 实名过，微信 App 与小游戏是同一个账号）时一样按下面的规则判、一起计额：法规按「同一网络游戏企业」合并计算，不合并的话 App 与小游戏各自不超限、合起来却可能超。这也是 §4「账号已经有 pi 时，任何渠道都按我们的规则限」。
  - 闸只在新建意图时判；同一订单再次调起沿用已有意图，不重复判、不重复占。
- **判定**（`go/payment/internal/core/minor.go`，在 `openIntent` 核对完商城订单之后、写意图之前）：
  1. 读 pi。读失败一律回 `kPaymentBusy`（小游戏渠道也一样：放行就可能漏掉这个人在别的渠道已经花的钱）；目录里没有这名角色回 `kPaymentRealNameRequired`；账号没有 pi 时 App 渠道回 `kPaymentRealNameRequired`，小游戏渠道放行、不计额。
  2. 按北京日期算周岁。成年人放行，不计额。
  3. 未满 8 周岁回 `kPaymentMinorNotAllowed`；订单不是人民币回 `kPaymentCurrencyUnsupported`（法规按人民币定额，国内发行的订单都是人民币）；单笔超过年龄段的单次上限回 `kPaymentMinorSingleLimit`。
  4. 在这个人的额度行锁下核对月、日累计，通过才写意图；超限回 `kPaymentMinorMonthlyLimit` / `kPaymentMinorDailyLimit`。
- **额度的口径**（`go/payment/internal/data/spend.go`）：
  - 主体是 pi，同一个人的多个账号合并。库里存 `SHA-256("spender|" + pi)` 的前 16 字节（`payment_intent.spender_hi/lo`），不存 pi（它含出生日期）。成年人的意图不记主体。
  - 不另记计数器，由这个人名下的意图算：还在等付款的（CREATED）全额占着，已付的（PAID）按转已付的时刻 `paid_ms` 计入当月、当日，已关闭的（过期、被渠道撤销）不占。这正是充值设计 §12 的「商城关单、玩家取消都不释放，只在过期或转已付时变」：待付款的意图要到后台关单才释放，我们定价的渠道是支付截止后约两分钟，商店渠道是商城截止后再 72 小时。
  - 核对式：`本月一日零点起已付 + 全部在途 + 本次 ≤ 每月上限`，单日同理（单日上限配了才核）。在途的不分月份全额计入，所以任何一个月最终到账的总额都不会超过上限，在途的意图到账时不用再核。§12 写的是「到账周期与预占周期不同时在到账周期重新核对」，这里少了这一步，保证相同。`paid_ms` 取本服务时钟、不早于意图创建时刻，时钟偏差记到「未来」的也算本月。
  - 并发：`payment_spend` 一人一行。预占在 `INSERT … ON DUPLICATE KEY UPDATE` 拿到的行锁下排队，事务用读已提交 + 锁读，看得见排在前面刚提交的那一笔；同一订单并发建意图只插入一行，核对时不计这张订单自己。
  - 上面的保证不覆盖意图关闭之后才到的钱，这些在同一把锁下单独处理（`ReviveMinorIntent`）：
    - 迟到的付款（意图已过期关闭才到，商店渠道居多）：额度够就照常记账；不够就不发货、转人工（`ORPHAN`；Google 不确认，三天后自动退款）。复核用建意图时记下的周岁 `minor_age`：人只会长大，只会更严。
    - App Store 撤回退款，以及「记账的第二步没做完、意图已到期关闭」：钱在商户手里、商城照常发货，照样计入；超限记合规日志与指标 `payment_minor_gate_total{outcome="recounted_over_limit"}`，人工处理。
  - 退款按净额算：订单绑定的那笔钱退回给用户之后，意图关掉，额度随之释放。
    - 渠道撤销（用户在渠道侧退款）：只有订单绑定的那笔（已绑定、还没报给商城，或取消后才到账、商城为它本身建了退款）的翻转才改意图状态。重复付款、金额或币种不符这些异常付款，以及拒收、转人工的，被撤销或撤回不改意图。这一条同时修了原来的问题：重复付款被撤销会把意图关掉，释放掉正付着的那一单的额度。
    - 商城的退款待办：原因是 `CANCELLED_LATE`、`DELIVERY_REJECTED`（退的是订单绑定的那笔，`CommerceApi.RefundReason.refundsOrdersOwnPayment`）时，渠道退成就关意图，再回执商城；重复付款、金额币种不符的退款不动意图。
    - 不算订单付款的钱（转人工的，金额或币种与意图冻结的对不上的，商城按异常付款退掉、不绑定订单）不记成已付、不占额度（2026-11-20 改，`core.settleIntent`）。我们定价的渠道一个商户单号只能付成一次，意图再也付不成，直接关掉，额度随之释放；商店渠道不动意图，照常到期。原来转人工的那笔让意图一直停在等付款、后台每个租约周期查一次渠道永不停，在途额度也一直占着，这一条一并修了。
- **单日上限**：2026-11-20 按负责人「先按最标准的定」取单次上限，8–16 周岁每日 50 元、16–18 周岁每日 100 元，与单次、每月的数字一起只写在 `shared/antiaddiction.SpendLimitAt` 一处。《未成年人网络保护条例》要求设单日累计上限，但没给数字。按单次上限定，一天至多顶格充一笔，月额度至少要分 4 天才用得完。原来的 `Payment.Compliance.DailyLimitFenAge*` 两个配置项删了：额度是规则，不是部署参数，不能让两处各写一份。
- **tip 码**（Tip.xlsx payment 段末尾，都不是故障）：`kPaymentRealNameRequired`、`kPaymentMinorNotAllowed`、`kPaymentMinorSingleLimit`、`kPaymentMinorDailyLimit`、`kPaymentMinorMonthlyLimit`。
- **已知局限**：只在小游戏里玩、从没在 App 实名的人，我们拿不到他的年龄，他的小游戏消费只受平台的限额；他之后到 App 实名，之前在小游戏的消费也进不了我们的累计（意图上没有主体键）。平台不给开发者实名或年龄（§3），这一段只能由平台管。

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
    SecretKeyEnv: LOGIN_NPPA_SECRET_KEY   # 只写变量名,值从 Secret 注入;K8s 部署脚本渲染成 MMORPG_LOGIN_NPPA_SECRET_KEY(§8 末尾)
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

- **go/payment 与充值设计 §12**：归「游戏应用支付系统接入」会话。2026-11-19 已对齐：`GetAccountRealName` 由本任务在 data_service 加；那边不碰账号目录，也不改 §12（只追加 §22）。限额闸在那边改完 go/payment 之后，由「充值按年龄段限额闸接入 go/payment」会话按 §5.6 落码。
- **Tip.xlsx**：支付会话 2026-11-19 正在追加 `//payment_error base=31000` 段。本任务的码进 login_error 段，等那边写完再动这张表，两边不同时保存 xlsx。
- **小游戏客户端连 gate（WebSocket）**：互不影响。小游戏包不做实名。
- **Java 版**：只在 Java 仓 `PARITY.md` 登记待做。两版共享的客户端契约是 `/api/real-name`、`LoginResponse.anti_addiction`、新 tip 码与踢线原因。

## 7. 负责人的决定

2026-11-19 至 20 都已定，负责人的口径是「先按最标准的定，不等压测」：

1. **国内首发是否只有小游戏形态**：App 形态也首发，§5 已按 §8 落码。
2. **法定节假日的口径**：按国务院放假通知里的全部放假日（含调休凑出来的连休）填 `ExtraPlayDays`，腾讯等大厂公开的寒暑假可玩日历就是这样排的；运维每年按新的放假通知更新。名单漏填只会更严。
3. **实名提交每日上限**：5 次（`AntiAddiction.DailySubmitLimit` 缺省值）。
4. **K8s 部署参数**：同批，已落码。
5. **未成年人单日充值上限**：等于单次上限（§5.6）。

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
| go/payment（2026-11-19 已落码，见 §5.6） | `internal/config/config.go` 与 `config_test.go`；`internal/core/service.go`、`workers.go`（指标登记），新增 `minor.go`、`minor_test.go`；`internal/data/repo.go`、`tables.go`，新增 `spend.go`、`spend_test.go`、`spend_tidb_test.go`；`internal/constants/constants.go`、`internal/server/server.go`、`internal/mall/client.go`（退款原因是否退订单那笔）；`payment.go`、`channels.go`、`etc/payment.yaml`；客户端仓 `Assets/Scripts/Game/Jubaozhai/PaymentClient.cs`（5 个码的文案）；`proto/payment/payment_table.proto`（意图加 4 列、新表 `payment_spend`）；`go/shared/accountdirectory/real_name.go`（构造时校验密钥）；`data/tip/Tip.xlsx` payment 段 5 个码 |
| 文档（约 5） | 本文、`docs/README.md`、`docs/PROGRESS.md`、第三方登录设计 §七、Java 仓 `PARITY.md` |

分两批交 Codex：第一批协议、tip、Go 共用、data_service、login、gate；第二批网关、客户端、K8s。

**落码时的调整（2026-11-19）**：

- **和账号注销共用一次目录读**：注销会话同时在账号行上加注销状态。两边约定 `readAccount` 的同一条 SELECT 带出注销状态与 `real_name_pi`，`ReadAccountDirectory` 一律走 `ReadWithLifecycle`（注销没装配时只是不回注销状态，pi 照回）；login 侧是同一个 `readAccountDirectoryWithRealName`，回角色列表、pi 与注销状态。账号目录列号：注销占 3–7，`real_name_pi` 是第 8 号；`AccountDirectoryResponse.real_name_pi` 是第 3 号，`LoginResponse.anti_addiction` 是第 10 号。
- **gate 上的 Login 先读目录再写登录态**：原来目录读在写登录态之后。现在提到前面，防沉迷拒绝和目录读失败都不会留下能拿去 CreatePlayer / EnterGame 的登录态。
- **实名提交挂在 `LoginPreGate`**（网关专用的服务，客户端经 gate 发不了），请求体登记进 `rpclogging.ProtectCredentials`，go-zero 的慢请求日志不打姓名和身份证号。令牌不能用回 gRPC UNAUTHENTICATED，网关照现有映射回 401，网关不用认具体 tip。
- **客户端实名窗复用账号面板**：账号框填姓名，密码框改明文填身份证号；提交成功后口令登录用原口令重进，渠道登录（凭证一次性）改用 `LoginCredential.ForAccessToken` 凭这次的 access token 重进。

**这一批没做的**：

| 项 | 状态与原因 |
|---|---|
| K8s 部署参数 | **2026-11-19 已落码，未运行，待 Codex 跑契约测试**（命令见 `docs/PROGRESS.md` 同日「实名与防沉迷接进 K8s 部署脚本」条目）。`k8s_deploy.ps1 -LoginNppaAppId / -LoginNppaBizId`（要么都给要么都不给）渲染 login 顶层 `AntiAddiction` 段：`Enabled: true`、`PlatformManagedAuthTypes: [wechat_minigame, douyin_minigame]`、`Nppa.AppId / BizId`、`Nppa.SecretKeyEnv: "MMORPG_LOGIN_NPPA_SECRET_KEY"`；`-LoginAntiAddictionExtraPlayDays`（逗号分隔的北京日期）与接口测试用的 `-LoginNppaCheckUrl / -LoginNppaQueryUrl / -LoginNppaLoginoutUrl` 给了才写，`DailySubmitLimit` 不写（缺省 5）。secretKey 从部署机环境变量 `MMORPG_LOGIN_NPPA_SECRET_KEY`（32 位十六进制，任何档位都没有回落值）经工作负载密钥目录的单键 Secret `login-nppa` 注入 login 容器的同名变量，Pod 模板带指纹。不粘滞、防误关（线上开着而本次漏传或换了 AppId / BizId 就在写操作之前拒绝，确认开关同第三方登录 `-ConfirmLoginAuthProviderChange`）、`dev_tools.ps1` 照传与 `k8s-image-*` / `k8s-release-*` 拒绝、摘要行 `login_anti_addiction=` 都与小游戏同一套。login 是 SQL 写者：`AntiAddiction` 段要照抄进运维预置的 `runtimeConfigSecret` 才生效；部署另把 AppId / BizId 明文注入 login 主容器（`MMORPG_LOGIN_NPPA_EXPECTED_APP_ID` / `_BIZ_ID`），login 启动时与运行配置比（`config.checkExpectedAntiAddiction`），漏抄、关着或抄成别的应用都拒启，不会带着关掉的防沉迷起来；所以开、关、换应用都是先改那份 Secret、不单独滚动 login，紧接着部署。data-service 与 payment 的清单各从 `domain-service-secrets` 注入 `MMORPG_ACCOUNT_DIRECTORY_SECRET_PAYMENT`（键 `account-directory-payment`，照 trade 那把写成 `optional: false`）；data_service 的跨用途复用检查（`validateGlobalCallerSecrets`）也管这一把。这把不用运维预置：2026-11-20 起它和其余 15 把 zone 工作负载引用的内部调用密钥一起由部署维护（托管键表 `$script:ManagedDomainServiceSecrets`，门禁 `Initialize-ManagedDomainServiceSecrets`、写入 `Sync-ManagedDomainServiceSecrets`）：缺的补齐、全集群一个值，值不同、过短或与别的键同值时在任何写操作之前拒绝，带 `-ResyncDomainServiceSecrets` 才统一，见开服手册 §1 与 `docs/PROGRESS.md` 2026-11-20「部署替运维维护 domain-service-secrets 的内部调用密钥」条目。本机 `go_services.ps1` 给 data_service 与 payment 发同一把开发密钥 |
| go/payment 限额闸 | **2026-11-19 已落码，未生成、未编译、未测试**（§5.6；命令见 `docs/PROGRESS.md` 同日「未成年人充值限额闸」条目）。K8s 上 payment 的 ConfigMap 要带 `DataServiceRpc`（集群的 etcd 发现）与 `Payment.Compliance` 的新键，归支付会话的 K8s 批次；密钥注入见上一行 |
| 人工换实名 | 2026-11-20 已落码：`LoginAdmin.ResetAccountRealName`（§5.2「客服重置实名」） |

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

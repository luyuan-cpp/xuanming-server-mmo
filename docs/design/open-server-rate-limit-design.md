# 开服登录削峰与排队入口

> 更新：2026-10-23，按当前源码与部署脚本收口，替代原方案中的骨架、重复更正与实施清单。2026-11-05：令牌桶改为一段 Redis Lua（§二、§六、§七、§11.6）。
> Gateway 限流已有实现；login 真排队已有代码，但 K8s 合入门禁仍关闭。本文只做静态核对，不构成专项运行或容量验收。
> 本文 §11.5 是源码引用的持票轮询契约，保留编号；业务排队算法以[登录排队设计](./login-queue-2026-05.md)为准。

## 一、职责与入口

Java Gateway 的 `/api/login` 与 `/api/assign-gate` 共用前置限流器，控制 HTTP 请求速率；
Go login 的真排队管理有序排位、放行票与容量。两者回答不同问题，不能把限流重试提示当成真实排位。

当前 HTTP 首认证走 `/api/login`，返回账号认证结果与双 Token；之后 `/api/assign-gate` 委托 login 分配 gate 并签握手票。
Gateway 不自行挑 gate、签票或验证排队令牌。完整流程见[第三方登录设计](./third-party-login-end-to-end-design.md)和 [Gate ↔ Login 边界](./gate-login-rpc-boundary.md)。

本限流器不覆盖 `/api/queue-status`、`/api/refresh-token`，也不直接限制后续 TCP EnterGame。
不能据此推断整个登录链已有统一速率或容量保证。

## 二、限流策略与顺序

实现为 [AssignGateRateLimiter](../../java/gateway_node/src/main/java/com/game/gateway/ratelimit/AssignGateRateLimiter.java)：

1. 限流关闭（没有令牌桶存储）时直接放行。
2. 检查开服波次；未开放时返回重试提示。
3. IP 桶 `rl:ip:<ip>` 与 zone 桶 `rl:zone:<zone>` 一次原子地判（`zone_id=0` 只判 IP 桶）：IP 桶不足一个令牌 → 429 IP_RATE_LIMIT，两个桶都不扣；
   zone 桶不足 → 重试提示，retry_after 为 zone 桶补到一个令牌要等的毫秒，IP 令牌也不扣；都够 → 各扣一个。
4. 检查进程内账号冷却，键为 `<scope>:<account>@<ip>`。

2026-11-05 起（[服务器缺口清单](../handoff/server-gap-audit-20261101.md)第 29 条）两个桶由一段 Redis Lua 判定
（[RedisRateLimitStore](../../java/gateway_node/src/main/java/com/game/gateway/ratelimit/RedisRateLimitStore.java)），替代 Bucket4j 的 CAS 后端：
zone 桶是全区请求共用的热键，CAS 版副本越多冲突越多，重试超时即放行，开服洪峰最容易漏；Lua 由 Redis 单线程执行，没有冲突与重试，一次检查一个往返。
顺序同时改为先 IP 后 zone（了结 §11.7 R13 与 §11.4 J1），与独立 Java 版一致。桶是 Hash（`t` = 千分之一令牌为单位的余量，`ms` = 对应的 Redis 时刻），
键不存在 = 满桶；拒绝路径只读不写，放行后键在补满的那一刻过期。两个键在同一段 Lua 里，要求在同一个 Redis 实例上（现状是本 zone 组的 redis-queue 单实例）。

两个 HTTP 入口共享 zone / IP 桶；冷却 scope 分别为 `login`、`assign`，避免正常的连续调用互相冷却。
冷却使用本机 Map 的 get / put，每个 Gateway 副本各一份，是尽力过滤重试，不能称严格账号串行化或 Redis 账号锁。
第三方登录没有 account 时，LoginController 用凭据的 Java hashCode 构造冷却标识；它不是认证后的账号身份。

带非空 `queue_token` 重入 assign-gate 时仍过波次与 zone / IP 桶，只跳过账号冷却。
Gateway 验不了 token，不能因字段非空就绕过限流。权威账号互斥与后台提交校验仍由 login 负责，见 [ARCH §3.3](./ARCH.md#33-并发安全)。

### Layer 1 配额参考

| 来源 | zone 每秒补充 / burst | IP 每秒补充 / burst | 冷却 |
|---|---|---|---|
| 打包 application.yaml | 500 / 1000 | 5 / 20 | 5000ms |
| K8s 开服起步档 | 2000 / 5000 | 5 / 20 | 5000ms |

这些是配置值，不是已测承载能力。通常一次 HTTP 首认证再分配 gate 会消耗两个 IP 令牌；
消耗几个 zone 令牌还取决于首认证是否带正数 zone_id。NAT 后的玩家共享 IP 桶。

## 三、接口结果与真实排位

业务回包使用 HTTP 200 + JSON `code`，代理错误、请求体错误等 HTTP 状态另行判断。
HTTP JSON 使用 snake_case，字段以 [LoginResponse](../../java/gateway_node/src/main/java/com/game/gateway/dto/LoginResponse.java)
和 [AssignGateResponse](../../java/gateway_node/src/main/java/com/game/gateway/dto/AssignGateResponse.java) 为准。

| 结果 | 语义 |
|---|---|
| login 的 `code=100` | 前置限流／波次重试提示，含 retry_after_ms；不是 login 真队列入队成功。 |
| assign 的 `code=100, queue_source="ratelimit"` | 波次／zone 桶要求重试，无排队令牌与真实位次。queue_pos 是桶返回的提示值（波次可为 -1），不能展示为“前面 N 人”。 |
| assign 的 `code=100, queue_source="login"` | login 真队列返回 queue_token、queue_rank、queue_total；rank 从 0 开始。 |
| `code=429` | IP 桶或账号冷却拒绝；assign 的 error 为 IP_RATE_LIMIT / ACCOUNT_COOLDOWN，login 使用其 DTO 的 message。 |
| assign 的 `code=0` | 返回 gate_ip、gate_port、token_payload、token_signature、token_deadline。 |
| `code=410` | 排队令牌失效，丢弃 token 后重新调用 assign-gate；后续可能放行或重新排队。 |
| `code=404` 或 `503 zone_*` | 区服不存在、维护、关闭或未开放；属于业务终态，不能当作状态查询网络失败重入。 |

原稿的 `wait_estimate_sec`、`token/deadline` 示例不再作为当前字段契约。
`queue-timeout-ms` 虽仍在属性与 YAML 中，但没有运行时消费方；DTO 中的 101 常量不代表已实现“五分钟硬封顶”。
当前 QueueEntryTTL 是空闲 TTL，持续查询会续期；排队 token 有独立的 24 小时绝对寿命，详见排队设计。

## 四、开服波次

实际配置路径为 `gate.rate-limit.wave`，由启动时构造的 [WaveSchedule](../../java/gateway_node/src/main/java/com/game/gateway/ratelimit/WaveSchedule.java) 使用。
`start-epoch-sec=0` 取本副本启动时间；跨副本统一波次需要显式时刻。`allow-zones: [-1]` 表示全部，
每档名单替换上一档，不能把后档当作累加。当前没有管理后台动态重载实现。

打包配置关闭波次，K8s 开服档也不自动写 wave 配置。要启用，需在独立任务中明确运营时刻与各档完整名单。

## 五、现有实现与配置

### 5.1 实现入口

依赖版本以 [pom.xml](../../java/gateway_node/pom.xml) 为准，已存在的实现包括
[RateLimitConfig](../../java/gateway_node/src/main/java/com/game/gateway/ratelimit/RateLimitConfig.java)、
[RateLimitProperties](../../java/gateway_node/src/main/java/com/game/gateway/ratelimit/RateLimitProperties.java)、
[RateLimitStore](../../java/gateway_node/src/main/java/com/game/gateway/ratelimit/RateLimitStore.java) /
[RedisRateLimitStore](../../java/gateway_node/src/main/java/com/game/gateway/ratelimit/RedisRateLimitStore.java)（令牌桶 Lua）
及限流器、令牌桶、波次与 IP 解析测试，不再按原稿重复添加骨架。限流打开时 rps / burst 必须为正，否则 gateway 拒启。

### 5.2 配置事实源

打包配置为 [application.yaml](../../java/gateway_node/src/main/resources/application.yaml) 的 `gate.rate-limit`，
默认限流关闭，数值见 §二。Java properties 自身的 IP 缺省为 2 / 10，与打包 YAML 的 5 / 20 要分开判断。
单 zone 覆盖结构是 `zone-overrides.<zone>.rps`、`burst`，不是单个数字。
K8s 渲染的启用状态与阈值见 §十一。

### 5.3 客户端 IP 与冷却

[ClientIpResolver](../../java/gateway_node/src/main/java/com/game/gateway/ratelimit/ClientIpResolver.java)
默认不信任 X-Forwarded-For；只有 socket 对端可信时，才从右侧剥离可信代理跳。
Ingress / LB 后必须准确配置 trusted-proxies，否则玩家可能全部落入代理 IP 的同一个桶。
扩大可信范围会改变客户端 IP 的信任边界，不能靠任意伪造 XFF 作为正式压测或防刷依据。

### 5.4 控制器边界

[LoginController](../../java/gateway_node/src/main/java/com/game/gateway/controller/LoginController.java)
与 [AssignGateController](../../java/gateway_node/src/main/java/com/game/gateway/controller/AssignGateController.java)
调用限流器；[QueueStatusController](../../java/gateway_node/src/main/java/com/game/gateway/controller/QueueStatusController.java)
直接进入 service 的区服准入与 login 查询，不经此限流器。`/api/refresh-token` 也未接入，
其保护需求应独立设计，不能以“低频”代替保证。

## 六、可观测性

2026-11-05 起 Gateway 有两项限流计数，经管理端口的 `/actuator/prometheus` 暴露：
`gateway_rate_limit_decisions_total{outcome}`（pass / wave_queue / zone_queue / ip_deny / cooldown_deny，每次判定记一个）与
`gateway_rate_limit_fail_open_total{reason}`（store_error = 这次碰 Redis 出错，bypass = 熔断期间没碰 Redis 就放过两个桶）。标签只有固定的几个值。
桶后端故障另记 `rate-limit fail-open` WARN，熔断期间不再逐请求打印。原稿列出的五个 `gate_assign_*` 专项指标仍没有对应实现，不能当作现役面板或告警来源。

login 自身已有 metrics 与 K8s 9101 抓取入口，robot 有排队统计，详见[排队判读手册](../ops/login-queue-stress-runbook.md)；
它们不等于 Gateway 的限流计数。桶操作的延迟分布还没有观测。

## 七、后端故障与放行代价

令牌桶存储抛 RuntimeException 时，限流器放过两个桶（fail-open），并熔断 5 秒：期间不再访问 Redis，直接放过两个桶；
到期只放一个请求去探（半开），探通即恢复限流，探测失败再熔断一段。进程内账号冷却不依赖 Redis，fail-open 期间照判。
这是可用性取舍，代价是 Redis 故障时失去两个桶的削峰保护；热键争用不再导致放行（单段 Lua 没有 CAS 冲突）。
超时的那次 EVALSHA 若其实已在 Redis 上执行，就多扣了一个令牌（偏严，可接受）。

专用 Lettuce 连接配置单命令 300ms、未完成请求队列上限 1024，断线时拒绝命令排队。一次检查只有一条 EVALSHA；
Redis 重启或脚本缓存被清后第一次遇到 NOSCRIPT，再带全文 EVAL 一次。这些配置不能解释为整个 HTTP 请求的严格截止时间。
Redis 卡死时，只有熔断前已在途的请求与每轮的那一个探测请求各多等约 300ms，其余请求不碰 Redis；连接半开时要等内核判死连接后
Lettuce 才重连，在那之前每轮探测都超时、限流持续 fail-open。
启动期仍需建立 Redis 连接，失败可导致 Gateway 拒启；没有“Redis 故障时总能正常启动”的保证。

## 八、与登录一致性及真排队的关系

限流只控制到达速率，不替代账号／玩家锁、会话栅栏、属主校验、DB 分区与持久化屏障。
真排队的权威容量、在途占位、放行与回收见[登录排队设计](./login-queue-2026-05.md)；
Gateway 的 ratelimit 重试不创建同等状态。正常持票查询必须遵守 §11.5，避免让前置限流耗掉已放行窗口。

## 九、容量验收边界

撤回旧稿无实测支撑的“login 单实例 1.5 万 QPS”“15 倍缓冲”“15 秒消化完”等结论。
当前数值只作为起步配置，实际还受 EnterGame 预加载、DB worker、Kafka 分区、共享 Redis、Gateway 同步等待与客户端重试影响。
容量结论必须由符合 `AGENTS.md` §6 的基线、分阶段采样和对比表支撑，本次不运行压测。

## 十、实现与待验收状态

| 范围 | 状态 |
|---|---|
| 令牌桶 Lua（2026-11-05 替代 Bucket4j）、熔断、限流计数、controller 接线、波次、IP 解析、冷却、超时及队列上限 | 已有实现与回归测试源码；令牌桶 Lua 与熔断 2026-11-05 落码，未编译、未测试，待 Codex 验证（含对真实 Redis 7.2 / 8.x 的用例）。 |
| K8s Gateway 按档位启用、配置指纹 | 已有部署实现。 |
| login 真排队 | 已有实现；K8s 合入门禁仍关闭，不能由旧矩阵推断可启用。 |
| 专项指标、真实故障放行行为、阈值与尖峰容量 | 按对应验收记录判断；本文静态核对不补充运行通过结论。 |
| 独立客户端、独立 Java 版 | 本次未读取；本仓 robot 的实现不代表它们已经对齐。 |

## 十一、K8s 按发布档位开启（G4b）

部署实现为 [k8s_deploy.ps1](../../tools/scripts/k8s_deploy.ps1)，操作与止血命令统一见
[部署手册](../../deploy/k8s/README.md)的“削峰(G4b)”。本节保留原编号，避免既有源码引用漂移。

### 11.1 当前开关矩阵

| 开关 | dev 的 auto | staging / prod 的 auto | 显式 on | 显式 off |
|---|---|---|---|---|
| GatewayRateLimit | 关闭，沿用打包缺省 | 开启，渲染起步档 | 开启 | 关闭；非 dev 告警 |
| LoginQueue（当前门禁未清） | 关闭 | 关闭并提示门禁 | 在任何 kubectl 前拒绝 | 关闭 |

当前 `LoginQueueMergeGateCleared=false`。三项门槛是指定多 zone 用例、G4b-G 回归和真实 Redis 最小冒烟，
都取得运行证据并登记后，由负责人按[规格 §7](../handoff/launch-readiness-20261013/spec_G4b.md)清门禁。
本次不修改常量，不手改配置绕过。门禁关闭时不要求、解析或写入 QueueToken，不渲染 Queue 段与排队指纹；
DryRun 的测试接缝只能模拟契约，不能打开真实部署。

Gateway 开关独立于排队门禁。显式 off 不粘滞，下次部署不再传入即按档位解析；
login 仍先受门禁限制，不能据此声称下次部署会自动开启排队。

### 11.2 起步值

单一来源为脚本的 `PeakShavingLaunchTier`：zone 2000/s、burst 5000，IP 5/s、burst 20，冷却 5000ms。
排队将来开启时使用的本 zone 容量起步值为 5000，它是人数上限，不是每秒速率；当前不因常量存在就生效。
这些值均待容量验收。脚本不自动写波次、zone 覆盖或未被消费的 queue-timeout-ms。

### 11.3 部署取舍与配置指纹

| 序号 | 当前取舍 | 边界与代价 |
|---|---|---|
| 1 | IP 使用打包配置的 5 / 20 | 两个 HTTP 入口共用桶，NAT 玩家仍相互影响。 |
| 2 | Gateway staging / prod 的 auto 直接开启 | 没有自动单 zone canary；关闭需显式 off 并等待滚动完成。 |
| 3 | login 排队先受合入门禁约束 | 当前所有档位关闭，尚未执行开排队的整体发布。 |
| 4 | 排队预设容量统一为 5000/zone | 未按各 zone 的实测峰值定标，不能当成承载保证。 |
| 5 | 限流／排队配置在启动时读取 | 仅修改 ConfigMap 不会让运行中进程重读；改值要关注对应 Pod 是否实际滚动。 |
| 6 | Redis 口令及将来启用的 QueueToken 仍存在 ConfigMap 明文字段 | 本次未改变注入机制；Redis 口令已有 YAML 特殊字符拒绝检查，QueueToken 的占位判定及原样嵌入 YAML 仍须按部署安全待办核对。 |
| 7 | Gateway 限流指纹覆盖 rate-limit 配置块（含 trusted-proxies）与 Redis 口令；login 排队指纹覆盖 Queue 块与 QueueToken | 两种指纹仅在各自功能开启时写入，关闭时移除也触发模板变化。login 此指纹不含 Redis 口令。单步排队密钥轮换会让在排 token 失效、重新入队，不能当作保排位轮换。 |

关闭功能时的指纹移除会改变 Pod 模板，但实际完成取决于滚动就绪，不能称“秒级止血已验收”。
`kubectl rollout undo` 不会回退 ConfigMap，不能代替配置开关回退。包装脚本不透传这两个开关时，按部署手册使用直接入口。

### 11.4 带票重入与冷却规则

J0 已实现：带 queue_token 的 assign-gate 仍受波次、zone / IP 桶约束，只跳过账号冷却。
J2 已实现：冷却键含端点 scope、未认证账号标识与客户端 IP，避免跨端点互挡，缩小替别人触发冷却的范围。
J1 已做（2026-11-05，随令牌桶 Lua 一起）：先判 IP 再判 zone，两个桶一次原子判定，zone 桶空时 IP 令牌不扣，见 §二。冷却本身不提供原子互斥。

### 11.5 客户端退避要求

本节保留 2026-10-15 已确定的契约，删除此前被更正的相互冲突版本。
`429 / 410 / 503` 等以下均指 JSON code；业务回包承载在 HTTP 200 中。

**持有 queue_token：**

1. 一律轮询 `/api/queue-status` 查排位与取票，不把 assign-gate 当作正常轮询入口；查询与恢复重入均沿用入队请求的 zone_id。
2. 每次查询前等待 `max(retry_after_ms, 下限)`，再加基础间隔 0–20% 的均匀抖动。正常下限 2s；查询连续失败时下限依次为 2s、4s、8s、16s、30s，之后保持 30s。封顶约束的是退避下限，不截断服务端更大的 retry_after_ms。
3. 仅查询出现网络错误、HTTP 非 200 或 JSON code=500 时，带原 token 调一次 assign-gate 作恢复。查询的非失败回包将连续失败数清零，恢复重入的回包不清零。
4. 恢复回 code=0 则放行；回 100（login 或 ratelimit）或 429 时保留 token，按上述间隔回到 queue-status。不得套用无票 429 的 30/60/120s 重调节奏，否则可能错过 AdmitTTL 放行窗口。
5. 查询回 code=503 的 zone_maintenance / zone_closed / zone_not_open，或 404 zone_not_found，是终态，不发恢复重入。代理返回的 HTTP 非 200 与这些业务结果分开判断。
6. 任一端点回 410，丢弃 token，不带 token 重调 assign-gate。恢复重入自身网络／HTTP 失败或返回其它失败码时，按错误结束、交外层。
7. 客户端总等待预算耗尽即停止发新请求。服务端放行槽、空闲 TTL 与 token 绝对寿命仍由排队设计分别约束，不能拿客户端重试代替延长放行票。

**未持有 queue_token：** code=100（ratelimit）按 retry_after_ms 重调 assign-gate，缺省 2s，并打散重试；
真实客户端的 429 按 30/60/120s 退避并加抖动。它与持票规则不同。

本仓 [robot 状态机](../../robot/http_assign_gate.go)已实现持票查询、抖动、恢复重入与预算检查，
[回归测试源码](../../robot/http_assign_gate_test.go)覆盖相关分支；本次未运行。
预算检查覆盖重试阶段的等待、恢复重入与 410 重调；首个 assign-gate 仍会执行，不保证已过期 ctx 零请求。
robot 无票分支保留既有差异：100 不加抖动，429 直接交外层；410 换票时不主动清失败退避计数。
已发出的 HTTP 请求按自身 perCallTimeout 完成，不随排队 ctx 立即取消，截止后收到 code=0 仍按放行处理。
独立客户端与独立 Java 版的实现状态需各自任务验收，本次没有读取或修改它们。

### 11.6 运行期改阈值

2026-11-05 起 rps / burst 每次随调用传进令牌桶 Lua，桶里只存余量与时刻：滚动 Gateway 后已有的桶立即按新阈值计算（余量按新 burst 封顶），
不需要清桶（用例 `RedisRateLimitStoreIntegrationTest.newThresholds_applyToExistingBucketImmediately`）。
只有从 Bucket4j 版本升级时要清一次 `rl:zone:*` / `rl:ip:*`：旧键是 STRING，新脚本按 Hash 读会报 WRONGTYPE 而放行，步骤见部署手册。

### 11.7 已知风险

| 编号 | 当前风险与后续验证 |
|---|---|
| R1 | HTTP 速率不直接保护 EnterGame 下游；按预加载、DB、Kafka 与场景入场实际吞吐确定阈值。 |
| R2 | NAT / CGNAT 共用 IP 桶，robot 把无票 429 当错误；基线与开启限流的结果要分别说明，不能混算容量。 |
| R13 | （2026-11-05 已了结）原先先扣 zone 后判 IP，已被 IP 拒绝的流量也会消耗 zone 额度；现为两个桶一次原子判定、先 IP 后 zone，见 §二。 |
| R14 | IP、任意正数 zone 桶及排队状态可能增长，限流先于 zone 合法性校验；共享 Redis 当前没有由此设计给出的硬容量保证。 |
| R16 | 状态轮询不经前置限流，仍与共享 Redis 的其它业务争用。旧约 2.5Q ops/s 不再作为现行模型；区分客户端往返与 Lua 内部命令，按真实查询与队列规模测量。 |

另需核对 zone 桶热键的 Redis 负载（每次检查一条 EVALSHA，洪峰时 ops/s ≈ 到达请求速率，含被拒的请求）、启动期 Redis 依赖、后端故障放行、
配置指纹、排队密钥轮换与各 zone 在线存量。
阈值与 Redis 拆分的专项任务见[发布就绪规格](../handoff/launch-readiness-20261013/spec_G4b.md)和部署手册；
本文不据已有源码宣称这些验收完成。

### 11.8 验证证据边界

已有 Java 限流／控制器／IP／波次／连接预算测试、robot 状态机测试及 K8s 削峰契约测试源码。
本文只核源码与配置，本次不运行生成器、构建、测试、服务、部署或压测；既有运行结果按对应 PROGRESS 条目判断，
不能把历史通用测试通过等同于当前真排队合入门槛或尖峰容量通过。

## 关联

- [登录排队设计](./login-queue-2026-05.md)：权威队列与放行状态。
- [部署手册](../../deploy/k8s/README.md)：开关、指纹、密钥与运维操作。
- [登录排队判读手册](../ops/login-queue-stress-runbook.md)：实栈观测，运行仍须遵守 AGENTS §6。
- [架构总览](./ARCH.md)：登录互斥、会话与持久化接缝。

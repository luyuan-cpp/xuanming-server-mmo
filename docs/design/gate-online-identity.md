# Gate 在线身份：login 认证、gate 持有、签名传递、提交点复核

日期：2026-11-05（美东）。状态：已落码。C++/Go 版链路由 Codex 编译与真连接验收（见 [审核交接](../handoff/gate-auth-role-fence-review-20261104.md)）；同日删除在线域的 login Redis 回退，未编译，待 Codex 按 [执行清单](../handoff/gate-online-identity-single-source-20261105/CODEX.md) 验证。同日另定简化方案（§9）：第 1 批（统一在线域客户端入口、关闭 guild 无会话冒名口）2026-11-06 已落码，**未编译**，与上一批合并由 Codex 按 [第 1 批执行清单](../handoff/gate-online-identity-simplify-20261105/CODEX.md) 验证；第 2 批（删 friend 入口重复预检）2026-11-06 已落码，**未编译**，验收见同一清单的第 2 批一节，计划见 [简化计划](../handoff/gate-online-identity-simplify-20261105/PLAN.md)；§9.6 第 1 条（guild 三个读方法删无会话分支）2026-11-06 已落码，`go/guild` 的 build / vet / test 2026-11-10 已由 Codex 跑通（集成用例未跑），见同一清单第 7 节；§9.6 第 2、3 条（删 guild 请求体里被忽略的身份与区字段、删全服帮会榜）2026-11-10 已落码，**未编译**，验收见同一清单第 8 节。§7 的四条已知边界 2026-11-11 定了处理方案（§10）：批 1（gate 改 Ed25519 签名、验证方只持公钥）当日已落码，**未编译**，验收见同一清单第 9 节；批 2～4（在线服务端口白名单、Pod 间加密的部署前检、gate 续租登录态）与 §9.3 的读路径归属预检（match、trade 补上，三个服务共用一份）2026-11-12 已落码，**未编译、未运行**，验收见同一清单第 10 节。同日第二批收掉第 10 节报出的残余（§7.1、§7.3、§9.3）：续租在登录态过期后按绑定记录重建、guild 的入口预检与 match / trade 统一为只判归属、预检补不连库的用例、Pod 间加密前检改为逐节点核对 WireGuard 公钥、补集群实测脚本 `k8s_netpol_probe.ps1` 与专用 kind 配置，同样**未编译、未运行**，验收见同一清单第 11 节。本文留作「以后再定」的事项到此都已定案。Java 版待做（§8）。

## 1. 分工

一句话：**login 验凭据；gate 记住验过的结果；下游只认 gate 签名的原身份，不再为每条消息查登录态；业务在提交点按原件复核角色归属。**

| 环节 | 负责方 | 做什么 | 不做什么 |
|---|---|---|---|
| 凭据认证 | login `Login` | 验 access token / 口令，按 `(gate_instance_id, session_incarnation)` 写本连接的 `login_session`（登录域 Redis） | 不信 gate 自报账号 |
| 选角入场 | login `EnterGame` | 从本连接 `login_session` 取账号；SQL 账号目录核「角色归属该账号」并取 `owner_revision`；后台链经 Kafka 发 `BindSessionEvent{player_id, login_generation, authenticated_account, session_incarnation, owner_revision}` 给该 gate 实例 | 不按 player_id 反查当前 owner 补账号 |
| 持有身份 | gate | `gate_authenticated_session::Bind` 只接受本进程 UUID 信封、同一连接代次、字段齐全、非旧登录代次、同代不冲突的绑定，写进 `SessionInfo.authenticatedAccount / authenticatedOwnerRevision`（与场景路由修订分开存） | 不从客户端正文取身份；新连接不继承旧连接 |
| 传递身份 | gate | 每个发往 Go 的请求由 `gate_session_details::Build` 从服务端会话构造 `SessionDetails`（含原账号与原修订），按 §3 签名；路由模式由 client_rpc_router 原样透传 `x-*` 头 | 路由服不重签、不改身份 |
| 验证来源 | 各 Go 服务 | `callerauth.GateInterceptor`：带会话头就必须验签，通过后把原件存进 ctx | 不接受未签名或与原件不符的声明 |
| 取原账号 | 在线域 match / chat / friend / guild / trade / mail / rank | 唯一入口 `roleidentity.EntryInterceptor`（§9.2）：客户端方法（生成的集合）必须带验签原件，且原件带角色、原修订、合法账号，原件放进 ctx、原角色归属捕获进 ctx；各服务的取值函数只读这一份 | **不读 login Redis**，没有回退路径；不带会话的客户端方法一律拒绝，只有登记的匿名只读例外（guild 的 GetPlayerGuild） |
| 提交复核 | 业务事务 | 提交点锁本库 `role_fence`，按原账号 + 原修订核归属、冻结、删除（[逻辑库自治](horizontal-scale-logical-db.md) §3） | 事务内不调 RPC；不用入口检查代替提交检查 |

为什么还要提交点复核：入口证明的是「这条连接、这个账号、这次归属」，不证明此刻角色仍归他。Bind 之后角色可能被冻结、过户、删除或换修订；`role_fence` 在提交点用同一把锁裁决，入口检查只是提前拒绝。只读方法没有提交点：读调用者私有数据的方法另核一次归属，在业务里核或由入口归属预检核，逐服务的做法见 §9.3。

## 2. 生命周期

| 事件 | login_session（登录域 Redis） | gate 会话身份 | 在线请求结果 |
|---|---|---|---|
| Login 成功 | 写入，TTL = `Node.SessionExpireMin`（缺省 30 分钟），并登记账号设备集 | `loginSucceeded`，尚无账号；gate 记下这条连接要续租（§10.4） | 在线域拒绝（未绑角色） |
| EnterGame 受理 → Bind 到达 | 保留（设备集按它判断连接是否还在） | 写入原账号与原修订 | 签名带原账号，在线域放行到业务 |
| 在线期间 | 不被在线域读取。gate 每过三分之一寿命替活连接续租一次（`RenewSession`，§10.4），连接在它就不过期，设备上限按在线连接计数 | 不变；换图只改路由修订 | 正常 |
| 角色过户 / 冻结 / 删除 | — | 不变（原修订） | 提交点 `role_fence` 拒绝 |
| LeaveGame | login 删除 | LeaveGame **发出时**由 `ResetSceneRoute` 清空原账号与原修订（路由模式在 lane 轮到它发出时执行，直连模式收包即发出；2026-11-11 起，原先路由模式在收包时执行）。lane 里排在它前面的请求按原身份先执行；它还排着时受理的请求，入队快照不带原账号；发送前复核失配即回 `kLoginSessionNotFound`（见 [client-request-ordering.md](client-request-ordering.md) §4.3、§4.4） | 在线域拒绝；再进游戏须重新 Login |
| 断线 | Disconnect 删除 | 会话摘除 | 无连接 |
| gate 进程重启 | 旧键没人续租，最长一个寿命后过期（键含旧 UUID，新连接用不上） | 新进程新 UUID，新连接新代次，没有任何旧身份 | 客户端重新取票 → Login（token 有效即可）→ 对原角色 EnterGame；仅凭 player_id 不能恢复。旧会话由 player_locator 的 gate 清扫回收 |

客户端自动重连已在客户端实现（`mmorpg-client` 的 `Assets/Scripts/Game/GameClient.Reconnect.cs`），服务端契约不变：重新登录走的是现有 `ReplaceLogin / LOGIN_RECONNECT` 判定。流程是在游戏中断线后，20 秒总期限内：令牌将到期先用 refresh token 换新（不保存、不重放密码）→ 向网关重新分配 gate 并领新票据 → 连新 gate 验票 → 用访问令牌 Login → 对原角色 EnterGame → 等 NotifyEnterScene；不重新选角。2026-11-11 起期限内对「还没连上新 gate」的暂时故障退避重试（0.5 秒起翻倍、封顶 4 秒、±20% 抖动）：网关暂时连不上，或返回限流 / 内部错误 / 区暂不可用，分到已停的 gate（gate 崩溃后旧注册过期前），验票前断线或超时。票据被拒、排队、区不存在，以及新 gate 接受票据之后登录或进场被拒（令牌失效、被顶号、角色不能进）都是确定结论，立即交回选服流程，不和服务端对抗。被踢线或显式登出不触发自动重连。

## 3. 签名契约（v2，2026-11-11 起；未编译）

签名方只有 C++ gate（`cpp/libs/engine/core/security/caller_auth.h`），验证方是 `go/shared/callerauth`，两边逐字一致：

```
canonical = "mmorpg-gate-identity/v2\n" + caller("gate") + "\n" + 目标方法全名 + "\n"
          + lowercase-hex(sha256(SessionDetails 原始字节)) + "\n" + 毫秒时间戳 + "\n" + nonce
signature = lowercase-hex(Ed25519-Sign(gate 私钥, canonical))   // RFC 8032,64 字节,128 个十六进制字符
```

gate 持私钥，login 与在线域服务只持公钥，持公钥签不出名（§10.1）。v1 是全体共用一把 HMAC-SHA256 密钥，已删除；版本行不同，v1 签名在 v2 下一律验不过。两边的单测用同一条 canonical 字面量，以及 RFC 8032 §7.1 TEST 1 向量，钉住跨语言一致：Go 是 `TestCanonicalV2Vector` / `TestEd25519Rfc8032Vector`，C++ 是 `CanonicalMatchesGoVector` / `Ed25519MatchesRfc8032Vector`。

- metadata：`x-session-detail-bin`（应用层 base64 一层，传输层 `-bin` 再一层，历史口径不改）、`x-caller-id`、`x-caller-ts`、`x-caller-nonce`、`x-caller-sig`（ASCII 原值，不做 base64）。
- 目标方法：直连签实际 RPC；路由模式签消息号对应的业务方法（不是本跳的 Forward），目标服务按自己的 `info.FullMethod` 验。方法名来自生成的 `grpcFullMethod`，两条路径同一张表。
- 窗口：时间偏移 ±30s；nonce 进程内两代轮转，保留 2×窗口；先验签再记 nonce，未认证流量占不了 nonce 表。nonce 签进了 canonical，所以在同一进程内，截获的签名头不论原样重放还是换正文都用不了第二次；打到另一副本则拦不住（§7.1）。
- 每次发送现签：排队请求在出队发送时才签，时间戳是发送时刻。

## 4. 密钥与配置

- **gate**：`MMORPG_INTERNAL_AUTH_SIGNING_KEY` 是 32 字节 Ed25519 种子的十六进制（64 个字符），只注入 gate。gate 同时读 `MMORPG_INTERNAL_AUTH_PUBLIC_KEYS`，启动时核对自己的公钥在列表里；私钥缺失、格式不对或不配对，gate 都拒启（`caller_auth::InitProcessSigner`）。
- **验证方**（login 与 7 个在线服务）：只读 `MMORPG_INTERNAL_AUTH_PUBLIC_KEYS`，逗号分隔、每项 64 个十六进制字符、不许空项与重复；缺失或格式不对拒启（`callerauth.GateInterceptor`，login 是 `newSessionInterceptor`）。login 的 `Secrets` 里不再有 `InternalAuth`。
- **生成**：在 `go/shared` 下执行 `go run ./cmd/identitykeygen`，一次输出一对，私钥只交给密钥管理。已有私钥时，`-derive` 推出配对的公钥。
- **轮换**，不需要专门开关：
  1. 新公钥加进列表，滚动验证方与 gate；
  2. gate 换私钥并滚动；
  3. 旧公钥移出列表。
- **K8s**：私钥只放 Secret `gate-identity-auth`（键 `signing-key`），只有 gate 经 secretKeyRef 引用；公钥列表不是密钥，由 `k8s_deploy.ps1` 明文注入 gate、login 与 7 个在线服务的 env。staging / prod 两个值都必须注入。
- **本机**：本机启动器（`tools/scripts/lib/gate_identity_environment.ps1`）、docker 调试栈与 K8s 的 dev 档，都用 RFC 8032 §7.1 TEST 1 的公开测试密钥对，只在 dev/test 下使用；非 dev/test 必须外部注入。私钥不进 ConfigMap、不进日志。
- 在线域服务（chat / friend / guild / match / mail / trade）**没有** `GlobalLoginSessions`、`LoginRedis` 配置，也不持有 login Redis 连接。

## 5. 可观测性

- `caller_auth_session_verify_total{result}`：在线域验签结果，result 取 `callerauth.Reason` 的有限枚举。稳态 `result!="ok"` 应恒 0；非 0 先查密钥是否同源、时钟偏移、签名头是否被改写。
- `caller_auth_nonce_overflow_total`：nonce 表超上限提前翻代（只缩短重放窗口，不放行验签失败的请求）。上限 500000 条、一代 60 秒，所以单进程验签成功速率超过约 8.3k/s 时，正常负载下也会计数。非 0 先看该进程 `caller_auth_session_verify_total{result="ok"}` 的速率：低于约 8.3k/s 才按异常查；高于它是容量信号，加副本或调大上限。
- `online_entry_rejected_total{reason}`：在线域入口的拒绝。
  - 入口准入（§9.2）四个 reason：`incomplete_identity` 是客户端驱动的（离场后仍发在线请求、绑定事件未到时抢发），稳态应接近 0；`client_without_session`（内部进程不带会话调客户端方法）、`internal_with_session`（带会话调内部方法）、`unverified_session`（会话头没有验签原件，即拦截器装配顺序错）都意味着代码缺陷或伪造，应恒 0。
  - 归属预检（§9.3，guild / match / trade）两个 reason：`ownership_changed` 是角色已不归这份原身份（过户、删角之后旧连接还在发请求；guild 还包括冻结），客户端驱动，稳态接近 0；`ownership_unverified` 是这一次判不了（目录读不到、库故障），持续非 0 先查 data_service 与本库。
- `loginsession_renew_total{result}`（login）：gate 代发的续租（§10.4）。`renewed` 是续上了；`restored` 是登录态已经过期、这次按绑定记录重建的（续租曾连续失败超过一个寿命，只在故障恢复后出现，login 同时逐条打一行 INFO）；`gone` 是已没有可续的记录（离开游戏或断线清理之后迟到的续租），随玩家行为变化，不是故障；`error` 是登录域 Redis 故障。稳态下 `restored` 与 `error` 都应为 0。
- gate 每分钟打一行 `[LoginRenewal] summary …`（排入、发出、续上、已无记录、失败、在途数）；失败数比上一分钟增加时另打一行 WARN。续租一直失败时，连接本身不受影响，只是登录态继续倒计时，过期后这条连接不再计入设备上限。
- login 自己的 `login_caller_auth_total{result,mode}` 与 `login_caller_auth_nonce_overflow_total` 不变。

## 6. 水平扩展

- 验签是本进程 CPU（一次 SHA-256 + 一次 Ed25519 验签，每次数十微秒量级，以 `BenchmarkPublicKeysVerify` 实测为准），不访问任何存储；在线服务加副本不增加 login Redis 的连接数，也不给登录域加读负载。入口的归属预检另算：guild、match、trade 每个带原身份的客户端请求按主键读一次本库护栏副本（不加锁、不开事务），副本缺行、落后或冻结时才读一次目录（§9.3）；friend 原先为每个客户端请求开一个写事务核原件后回滚，与业务事务里的核查重复，2026-11-06 删除，每个请求少一次写事务。
- 续租（§10.4）的负载与在线连接数成正比、与请求量无关：每条连接每三分之一寿命一次，缺省 30 分钟寿命下 10 万在线约每秒 170 次。login 每次一段 Lua（登录态与会话栅栏按连接分布，同槽）加设备集的登记（按账号分布，另一个槽），都是单槽命令，上 Redis Cluster 不需要跨槽事务。每个 gate 进程每秒最多发 200 个、在途不超过 400 个，login 变慢时不会堆积。
- 路由服无状态透传；gate 只持本进程连接的身份，扩 gate 不需要共享会话存储。
- 角色归属的强一致由各逻辑库自己的 `role_fence` 提交锁保证，不依赖跨库 SQL 或集中目录查询。

## 7. 已知边界与处理结果

2026-11-11 起按 §10 逐条处理，2026-11-12 四条都有了结论并落码（未编译、未运行）。下面写的是处理之后的现状和还剩什么。

1. **跨副本重放与正文**（在网络层闭合，§10.2、§10.3）：
   - 应用层的性质没变：nonce 表是进程内的，同一条签名在 ±30s 内可以重放到同一服务的另一副本；签名绑定身份与方法、不覆盖请求正文。
   - 要利用它得先拿到一条签名头，再连上目标服务的端口。现在这两步都被网络层挡住：预发与生产的 Pod 间流量必须透明加密，部署前检核不到就拒绝部署，嗅探不到签名头；login 与 7 个在线服务的 gRPC 端口只放行登记过的调用方，预发与生产必须开白名单，scene、运维工具等清单之外的进程连不上端口。
   - 部署前检核的是每个节点的 WireGuard 公钥，不只是网络插件的开关：开关打开、某个节点内核不支持时，Calico 不报错，只是那个节点的流量不加密（§10.3）。
   - 两项是否真的生效，由集群实测脚本 `tools/scripts/k8s_netpol_probe.ps1` 在部署好的集群上直接连一遍确认：名单里的调用方连得上、名单之外的连不上，加密逐节点核对（§10.2、§10.3）。kind 上的实测配置是 `deploy/k8s/kind-netpol-config.yaml`（Calico）。
   - 还剩：dev 档与本机启动不强制这两项，只用公开的测试密钥，不承载真实玩家；白名单之内的进程（gate、路由服、登记的内部调用方）被攻破后仍能在窗口内跨副本重放自己经手的请求，这一面靠缩小白名单约束，不在签名上加层（§9.5）。
2. **撤销不是跨服务原子的**：踢线 / 离场之前已经受理的请求按各业务契约完成或拒绝；冻结、过户的正确性由提交点护栏保证，不靠异步踢线消息。这是设计如此，不是待修的缺口（§10.5）。过户、删角之后旧连接的只读请求由入口归属预检拒绝（§9.3）。
3. **设备上限按在线连接计数**（§10.4 已改）：原先 `login_session` 是绝对 TTL、没有在线续期，在线超过寿命的连接不再计入 `MaxDevicesPerAccount`。现在 gate 为活连接定期续租，连接在、登录态与设备集成员就在；连接断开或 gate 崩溃后不再续租，最长一个寿命后过期。续租连续失败超过一个寿命（登录域 Redis 或 login 长时间不可用）时登录态会过期；已经进了游戏的连接，恢复后的第一次续租按 login 自己在绑定时留的账号副本重建登录态并重新登记设备，又计入上限（§10.4）。重建只认 login 自己的记录，不信 gate 自报的账号（§1）。还剩：没进游戏（还在选角）的连接没有绑定记录，登录态过期后无从重建，这段时间它不计入上限，客户端下一步操作会被要求重新登录。只影响设备上限，不影响身份与资产。
4. **一把对称钥，持钥即可冒充 gate**（2026-11-11 批 1 已改，未编译，§10.1）：
   - 原问题：gate、login 与 7 个在线服务（chat / friend / guild / match / mail / trade / rank）共用同一把 HMAC 密钥 `MMORPG_INTERNAL_AUTH_SECRET`，任何持钥进程被攻破，都能以 gate 身份为任意玩家签名，去调其他在线服务。
   - 现在：gate 用 Ed25519 私钥签名，验证方只持公钥、签不出名，只剩 gate 本身被攻破这一面，这一面任何方案都绕不开。
   - `x-caller-id` 仍由签名方自己填，但只有持私钥的 gate 签得出能验过的名。
5. **guild 无会话冒名口**（2026-11-05 发现，2026-11-06 第 1 批关闭，未编译）：`GuildService` 的 CreateGuild / GetPlayerGuild / LeaveGuild / DisbandGuild / SetAnnouncement 原先在请求不带会话头时按「内部调用」处理，直接用请求体里的 player_id（`go/guild/internal/logic/guild_logic.go` 的 `callerOf`），写事务只核目标角色的冻结与删除，不核账号与修订。任何能连上 guild 端口的进程不需要签名就能替任意玩家建帮、退帮、解散帮会、改公告，gate 签名对这四个写方法不起作用。现在入口准入拒绝不带会话的客户端方法，四个写方法在业务层另加 `requireClientSession`；唯一保留的无会话调用是 mail 领附件时经 `GuildRPCDirectory` 读 GetPlayerGuild（只读，登记为匿名例外）。

login 与在线域都没有「验签失败告警放行」的宽松档：2026-11-05 起删除 login 的 `InternalAuth.ForceEnforce`，dev/test 与生产同一条强制路径。

## 8. 两版状态

- C++/Go 版：本文全部内容。
- Java 版（`D:\luyuan\xuanming-server-mmo`）：入场时 `SessionContext` 使用 `ownerAccount` 已修（Java 83d57c8f）；在线域是否只认 gateway 绑定的原账号、gate 重启后新连接只能重新登录，**待核对**，已在 Java 仓 `PARITY.md` 记 TODO。Java 内部走既有 Dubbo 认证，不要求与本文签名格式相同。
- §9 第 1～3 批只改本仓服务端内部接缝，不改客户端契约；Java 版内部认证机制不同，不适用，不往 `PARITY.md` 加行。Java 在线域那条 TODO 核对时，一并查有没有「不带会话就按请求体身份执行」的入口（§7.5 的同类问题）。
- §10.1 批 1（Ed25519）只改服务端内部接缝，不改客户端契约。Java 内部走 Dubbo 认证，不照抄；已在 Java 仓 `PARITY.md` 登记待核对项：Java 的网关身份传递是否也是「验证方持有能签名的同一把钥」。
- §9.6 第 2 条删了 guild 六个客户端请求里的字段，是**客户端契约变更**：Java 版本轮不改代码，已在 Java 仓 `PARITY.md` 记 TODO（同步 `game-proto` 里的 `guild.proto`，并确认 Java 版不依赖这些请求体字段）。§9.6 第 3 条删全服帮会榜后两版一致（Java 本来就不提供）。
- §10.4 批 4 在 `ClientPlayerLogin` 上加了 SERVER_ONLY 方法 `RenewSession`（gate 代发，客户端发来的一律在入口拒绝）。它进了全仓共用的消息号表，Unity 客户端的消息号表要随协议生成一起更新，客户端代码不用改。Java 版本轮不改代码，已在 Java 仓 `PARITY.md` 记 TODO：同步 `game-proto` 的 `login.proto` 与消息号表；核对 Java 的设备数上限是不是也按「登录态没过期」计数、在线超过寿命会不会漏计，是的话按 Java 惯用方式补续期。
- §9.3 的读路径归属预检、§10.2 / §10.3 的网络层要求都不改客户端契约。Java 待核对两项，已一并记进 `PARITY.md`：过户、删角之后旧连接的只读请求（战报、挂单、收藏这类不进写事务的方法）有没有核归属；Java 版的部署是否同样要求在线服务端口白名单与 Pod 间加密。

## 9. 简化方案（2026-11-05 定；第 1～3 批 2026-11-06、第 4 批 2026-11-10 落码，均未编译）

目标：在线域只有一处取身份、一条准入规则；不降低 §1～§3 的任何安全属性，并关闭 §7.5 的缺口。落码顺序、文件清单与 Codex 验收见 [简化计划](../handoff/gate-online-identity-simplify-20261105/PLAN.md)。

### 9.1 现状的重复

- **同一份身份解码两次。** `GateInterceptor` 验签后已把原件存进 ctx，各服务又自己从 metadata 解一遍（match 的 `sessionInterceptor`，chat / friend / guild / trade 的 `session.UnaryServerInterceptor`，mail / rank 共用的 `domainhost.Guard`），再由 `SessionAccount` 用 `proto.Equal` 比两份。两份来自同一段字节，比较恒等。
- **「客户端能调哪些方法」有六份手写来源**：friend / guild / trade 的 `ClientMethods`、mail 的 `ClientMethods()`、rank `internal/server/server.go` 里的名单、match 的方法前缀。proto 里已有方法级 `OptionClientRpcKind`（PUSH / SERVER_ONLY），但没有用它生成名单。路由表的 `ClientProtocol` 只看服务级开关，`UpdateGuildScore`（SERVER_ONLY）和各 `Notify*`（PUSH）在路由表里都算客户端协议，现在全靠各服务的手写名单拦。
- **缺原身份时四种处理**：match 在拦截器拒；chat 在拦截器放行，业务 `verifyActor` 再拒；friend / guild 在拦截器拒，另开一次 SQL 预检；guild 五个旧方法无会话时按请求体身份执行（§7.5）。

### 9.2 第 1 批：统一在线域客户端入口

1. **生成客户端方法集合。** protogen 从 proto 产出全局「客户端可发起方法」集合：服务标了 `OptionIsClientProtocolService`，且方法的 `OptionClientRpcKind` 是 REQUEST 或 ONE_WAY；PUSH、SERVER_ONLY 不算。生成到 `go/shared/generated/clientrpc/`，不手改。路由服**不**按它拒绝：gate 代发的 login `Disconnect` 是 SERVER_ONLY，路由模式下要经路由服转给 login。
2. **一个共享入口拦截器。** 放在 `shared/roleidentity`，装在各服务现有会话解码拦截器的位置，不改它与排空、热关停拦截器的相对顺序：

   | 方法 | 带会话头（已验签） | 不带会话头 |
   |---|---|---|
   | 客户端方法 | 原件须带角色、原修订与合法账号，捕获进 ctx 后放行；缺任一项 PermissionDenied | PermissionDenied。只有装配时显式登记的只读方法例外，目前只有 guild 的 GetPlayerGuild（mail 领附件在用） |
   | 非客户端方法 | PermissionDenied | 放行，由方法自己的调用方准入负责（`serviceauth` 等） |

   带会话头却取不到验签原件（拦截器装配顺序错）一律 Unauthenticated。
3. **各服务只删不增。** 删除各自的会话解码拦截器、手写名单与原身份拦截器；`ClientPlayerID`、`GetSessionDetails`、`domainhost.Session` 这类取值函数保留名字，改为读验签原件，业务代码不动。`SessionAccount` 去掉 `proto.Equal`，只核字段。
4. **关闭 guild 冒名口。** CreateGuild / LeaveGuild / DisbandGuild / SetAnnouncement 不再接受无会话调用，`callerOf` 删除请求体回退；GetPlayerGuild 的无会话只读保留并登记为例外。请求体里的 player_id / zone_id 字段当时先留着、服务端一律忽略，2026-11-10 已从协议删除（§9.6 第 2 条）。
5. **login 不改**：登录、选角请求本来就没有账号，login 用自己的适配器。
6. 顺带改正 match 两处过时注释：`match_service.go` 的 `sessionInterceptor` 说 MatchInternal「调用方不验签」，实际 guild / battle / 观战运维入口已用 `serviceauth` 验签；`internal/logic/queue.go` 说请求体 player_id 供无会话内部调用使用，实际无会话调 MatchService 已被拒。

安全变化只有收紧：新服务、新方法不会因为漏写名单、漏挂拦截器而放行；guild 四个写方法从免签名可冒名变为必须带 gate 签名；LeaveGame 之后（会话里仍有 player_id、账号已清），rank 和其他只看 player_id 的读方法也不再受理。

落码要点（2026-11-06，未编译）：

- 共享接缝：`roleidentity.EntryInterceptor(callable, anonymousReads...)` 由装配层传入 `clientrpc.Callable`，本包不依赖生成物；会话只有一个 ctx 键 `roleidentity.Session` / `WithSession`，生产中只有入口准入写它。各服务的取值函数（match `ctxkeys`、chat / friend / guild / trade 的 `session`、`domainhost.Session` / `Player`）保留名字、改读这个键，所以业务逻辑单测仍可用原来的 `WithDetails` 造客户端来源。`SessionAccount` 删除，改由 `SessionOwnership` / `CaptureSession` 只读验签原件；trade 的 `RmbIdentity.Resolve` 去掉了会话参数。
- 拒绝形态统一为入口 gRPC 错误：chat / mail / trade 对缺原身份的客户端请求原先回 in-band `kInvalidParameter`，现在与 match / friend / guild 一样回 `PermissionDenied`，路由服转成 `kServiceUnavailable` 信封。业务层原有的缺身份判定保留为第二道。
- `domainhost.Guard(budget)` = 整请求预算 + 入口准入；`Serve` / `ServeWithReady` 不再收客户端方法名单，PublicationAdmission 的 relay 方法也改按生成集合判定（与原名单等价：mail 5 个、rank 2 个客户端方法）。
- friend 的 `ClientIdentityInterceptor(onlineEntry, original)` 把入口准入与 SQL 预检串在拦截器链的同一位置，链长不变；预检本身留到第 2 批。
- 新指标 `online_entry_rejected_total{reason}`（§5）。

### 9.3 第 2 批：删 friend 入口的重复预检

friend 的入口拦截器为每个客户端请求开一个写事务核原件，然后回滚。2026-11-05 逐方法静态核对业务路径：

| 方法 | 业务路径里的原件核查 |
|---|---|
| AddFriend、AcceptFriend、RejectFriend、RemoveFriend、Block、Unblock | 写事务（`runGuardedWrite` / `beginWriteTx`，以及 `requireRoleActor`） |
| GetFriendList、GetPendingRequests、ListBlocks | `roleReader` |
| RecommendFriends | 在线玩家路径只读 Redis 在线目录与 data_service，**没有**；入口预检是它唯一的归属检查 |

前 9 个方法删掉入口预检后拒绝语义不变；业务侧把核不过映射成 `ErrRoleNotVerified`（不是故障码），不会把排空证明打成 Hold。RecommendFriends 删除后只要求验签原件、不再核归属：它只返回公开的在线玩家推荐，接受这个变化。

落码（2026-11-06，未编译）：删 `go/friend/internal/session/identity.go`（`OriginalIdentityInterceptor` 与 `ClientIdentityInterceptor`）、`Deps.CheckOriginalRole` 与 `FriendRepo.CheckOriginalRole`；拦截器链第 ③ 位直接是共享入口准入，链长不变。删之前逐项推演过预检挡的场景：

- **角色冻结、换主、ABA、删除**：9 个方法的业务事务（写锁 `role_fence`、读经 `roleReader` 持 P）按同一份原件复核，TiDB 用例逐个仓储操作覆盖；新增 `internal/logic/role_not_verified_test.go` 逐方法钉住带内回 `ErrRoleNotVerified`。
- **写者闸门冻结**：原先预检回 `Aborted`；现在业务事务开事务即失败，链里紧贴 handler 的 `serverbase.UnaryInterceptor` 观察到冻结，改写成确定没写的可重试状态，排空不 Hold。
- **自愈读不到目录**：业务回 `ErrRoleNotVerified`，同上不 Hold。
- **库故障**：原先预检回 `Aborted`（不 Hold、也不报故障）；现在由业务回 `ErrStorage`，按故障计数告警。这是有意的变化：库坏了本该告警，原先的 `Aborted` 把存储故障藏成了可重试拒绝。读方法因此在排空期遇到库故障时会 Hold，只是多等，不影响数据。
- **请求预算**：预检拦截器原先替客户端请求套整请求预算；10 个 handler 入口本来就各自套 `withRequestBudget`，不丢截止时间。
- **事务外的 Redis 写**：客户端路径上没有在业务事务之外写 Redis 的地方，预检没有额外覆盖面。

guild 的入口预检保留：它读本库护栏副本、不加锁，是 guild 读方法唯一的归属检查（2026-11-12 起判据与 match、trade 统一，见下文）。

#### 读路径归属（2026-11-12 定案并落码，未编译）

入口准入只证明「这是哪条连接、哪个账号、哪次归属」。写路径在提交点持 `role_fence` 行锁复核；读路径没有提交点，过户、删角之后旧连接（踢线消息丢了、或还没到）发来的只读请求要另有一处核归属。

**规则**：读调用者私有数据的客户端方法必须核「角色此刻仍归这份原身份」；公开数据不核。在哪核由服务定，两种做法都算数：

- 业务里核：方法本来就开事务或读目录的，在那里按同一份原件核，不再加入口预检（friend 就是这样删掉预检的）。
- 入口预检：服务里有不进事务的只读方法时，在入口准入之后装 `roleidentity.OwnershipInterceptor`，对每个带原身份的客户端请求核一次。新加的方法不会漏。

2026-11-12 逐服务核对的结果：

| 服务 | 做法 | 说明 |
|---|---|---|
| guild | 入口预检 | 原有。2026-11-12 起判据与 match、trade 相同（`GuildRepo.CheckOriginalRole` → `rolefence.Guard.VerifyOwner`，只判归属）；原先冻结的角色也拒，见下面「guild 统一的依据」 |
| match | 入口预检（本次补） | 排队、应战、组队、网络探测本来就经 `Fenced` 持锁核；战报列表与回放、作战脚本列表只按会话里的 player_id 读库，原先没有归属检查 |
| trade | 入口预检（本次补） | 合同与人民币寄售的读写本来经 `actorIdentity` 读目录、再按合同里存的原身份筛（`OwnsRecord`）；浏览与详情里的收藏标记、我的货架、收藏的增删、盘口挂单列表原先只看 player_id。盘口挂单列表另补了 `OwnsRecord` 筛选，新主看不到旧主留下的历史挂单 |
| friend | 业务里核 | 上表；RecommendFriends 只回公开推荐 |
| mail | 业务里核 | 读写都在事务里先锁本库护栏（`lockActor`） |
| chat | 业务里核 | 发言与拉历史都先过 `verifyActor`：读目录的权威状态，成功结果在进程内缓存 TTL 秒。过户后旧连接最多还能读 TTL 秒的历史，聊天不是资产，这一点早已接受（`actor_verify.go` 文件头） |
| rank | 不核 | 只有公开榜单 |

**预检怎么判**（guild、match、trade 同一个判据；`rolefence.Guard.VerifyOwner`）：

- 不加锁、不开事务，按主键读一次本库的护栏副本。修订号比原件大，或修订号相同而账号不同（含删角墓碑）：确定不归。修订号比原件小或副本缺行：读一次目录写回副本再判。
- **不看冻结**（`rolefence.CheckOwner`）。冻结期间角色仍归原账号，读自己的数据不必拦；trade 还有必须放行的情形：卖家把自己这名角色挂了人民币寄售，角色就是冻结的，重放与查询要照常返回。写入照旧由提交点的 `rolefence.Check` 拦。
- **冻结的副本先读目录再判。** 过户之前所有参与者都先冻结；过户完成的推送要是丢了，副本会停在「旧主 + 冻结」，只比修订号和账号会把旧主放过去。没冻结的副本不会落后于目录，所以稳态下预检只读本库。
- **guild 统一的依据**（2026-11-12 逐个核过）：guild 的客户端写入全都在提交点持副本行锁按 `rolefence.Check` 复核原身份，冻结的角色写不进去，没有一处只靠入口预检挡冻结。
  - 帮会管理与申请（建帮、退帮、解散、改公告、任免、踢人、转让、申请、撤回、审批）：`GuildRepo.inTx` → `lockRolePrefix`。
  - 捐献、商店、升级：`EconomyRepo` 的三个事务同样经 `inTx`；升级不带额外目标，原身份由 `lockRolePrefix` 自己从 ctx 取。
  - 点灯、团圆领奖：`ActivityRepo.participate` 经 `inTx`。
  - 同道历练：大厅的建立与应答走 SQL（`NewSQLTrialLobbyRepo`，生产只装配它），`inTx` 带上全部队员；建大厅之前向 match 取的战斗占用，match 在自己的提交点按 `rolefence.Check` 拒冻结的角色。
  - 帮会榜、成员缓存这类 Redis 写都是上面某个已提交事务的后续，不是独立的入口。
  所以放宽入口预检不放开任何写入。原先「入口拒冻结」本来也挡不严：预检读完副本、冻结紧接着发生，写入照样要靠提交点。
- 预检的拒绝判据仍是 `guildOwnerRejected`：确定不归（`ErrWriteConflict` 或目录里没有这名角色）→ `PermissionDenied`，其余判不了 → `Aborted`。

**拒绝形态**（三个服务共用同一个拦截器）：

- 确定不归：`PermissionDenied`，计 `online_entry_rejected_total{reason="ownership_changed"}`，不逐条打日志。
- 这一次判不了（目录读不到、库故障）：`Aborted`，计 `reason="ownership_unverified"` 并打一行 INFO。客户端可以重试；不回 `Unavailable`，因为这里只读、确定没写，回它会把排空证明打成 Hold（[逻辑库自治](horizontal-scale-logical-db.md) §3.6）。
- ctx 里没有原身份的调用（内部调用、登记的匿名只读例外）不做预检。
- 路由服把这两种 gRPC 错误都转成 `kServiceUnavailable` 信封，与入口准入的拒绝一样。

**落码**：

- 共享：`go/shared/roleidentity/ownership.go`（`OwnershipInterceptor`，由 guild 的 `session.OriginalIdentityInterceptor` 提上来，guild 那份已删）；`go/shared/rolefence` 的 `CheckOwner` 与 `Guard.VerifyOwner`。
- match：`authority.Store.VerifyOriginalOwner` / `authority.OwnerRejected`，`match_service.go` 里紧跟 `onlineEntry`。
- trade：`commerce.RoleAuthority.VerifyOriginalOwner` / `commerce.OwnerRejected`，`buildUnaryInterceptors` 第 ④ 位。角色护栏原先只在资产通道开启时装配，现在无条件装配：浏览、收藏在通道关闭时照常服务，同样要预检。trade 是护栏参与者，调用方密钥 `MMORPG_ROLE_FENCE_SECRET_TRADE` 各档位都注入；缺了拒启。
- guild：改用共享拦截器，`CheckOriginalRole` 改调 `rolefence.Guard.VerifyOwner`（与 match、trade 同一个判据），拒绝判据（`guildOwnerRejected`）不变；状态文案改成英文。
- 用例：判据本身在 `go/shared/rolefence/owner_test.go`；三个服务的接线各有一组不连库的用例，护栏副本用内存替身 `fencetest.MemoryReplica`（只服务不加锁的读和按版本合并的写，事务与 `FOR UPDATE` 一律报错，免得拿它测提交点），随普通 `go test` 跑：`match/internal/authority/role_identity_owner_test.go`、`trade/internal/commerce/role_authority_owner_test.go`、`guild/internal/data/role_owner_precheck_test.go`。真库用例（`TestAuthorityTiDBVerifyOriginalOwner*`、`TestTiDBVerifyOriginalOwner*`）保留，覆盖真实 SQL。
- trade 的护栏无条件装配由 `trade/internal/svc/role_fence_wiring_test.go` 按源码结构钉住：挪回资产通道的条件里，通道关闭时预检拿到 nil、所有客户端请求被拒，而启动不报错。

### 9.4 nonce 表保留

nonce 签进了 canonical，同一进程内截获的签名头不论原样重放还是换正文都只能用一次（§3），跨副本拦不住（§7.1）。它对单副本服务和同副本复用有效，删掉换来的简化不值，只修 §5 的告警口径。

### 9.5 不做

- **共享 nonce 存储、把正文摘要签进去。** 前者每个请求多一次存储往返；后者在路由模式下要 gate 解析并确定性重编码正文，Go 侧还要拿到原始字节。防内网嗅探和篡改的做法是在网络层加密 Pod 间流量（§10.3），同时收紧端口白名单（§10.2），不在签名上继续加层。
- **login 签发、gate 只转交的非对称身份票据。** 这种做法要配票据过期、续签与撤销，不做。§7.4 的持钥冒充改由 gate 对每个请求用 Ed25519 现签来解决（§10.1），没有票据生命周期。
- **合并 `callerauth` 与 `serviceauth`。** 2026-11-12 定案：不合并。两套回答的问题不同，密钥模型也不同，合成一套只会让其中一边用上不合适的模型：
  - `callerauth` 证明「这是哪名玩家的会话」：签名方只有 gate 一个，非对称（gate 持私钥，全体在线服务只持公钥），签的是会话原件与目标方法。
  - `serviceauth` 证明「是哪个内部服务在调」：每个调用方一把对称钥，只由调用方和受理它的服务持有，签的是方法、调用方名与请求正文的摘要（guild → match、data_service ↔ 护栏参与者、trade → scene）。

  把终端用户身份和服务间身份分成两层也是通行做法。约束不变：新服务不造第三套；玩家会话身份只走 `callerauth`，服务间调用只走 `serviceauth`。

### 9.6 剩余（2026-11-06 记；2026-11-10 三条都已落码，未编译）

1. **guild 三个读方法的无会话分支**（2026-11-06 已删；`go/guild` 单测 2026-11-10 已由 Codex 跑通，集成用例未跑，见 [执行清单](../handoff/gate-online-identity-simplify-20261105/CODEX.md) 第 7 节）：GetGuild、GetGuildRank、GetGuildRankByGuild 原先对不带会话的调用不查归属区、沿用请求体 `zone_id`（全服榜 `zone_id=0`）、不夹页长。生产里入口准入早已拒绝这类调用，只有单测故意走这条路径。现在三个方法与四个写方法一样先过 `requireClientSession`，区一律取 `clientZone`，榜单页长夹到 `MaxRankPageSize`。随之没有调用者的两处一并收掉：`viewerOf`（只为无会话调用回 0，CreateGuild / SetAnnouncement / GetGuild 现在直接传会话里的 player_id）与 `visibleIn` 的「`zoneID==0` 不按区过滤」；`expected_guild.go` 里「只给客户端来源计 absent」的判断同理删掉（五个调用方都只受理客户端来源）。测试改成客户端来源：请求者用本区旁观者（不在被查的帮里），`guildInfoFor` 不给他现算待审数，所以不需要待审数替身，也不用为测试给 `GuildLogic` 新开接缝；「内部调用保留全服榜」改为断言无会话被拒，另加三个读方法无会话被拒的表驱动用例。运维要全服榜，另设带 `serviceauth` 的内部 RPC，不复活这条分支。
2. **guild 请求体里的身份与区字段**（2026-11-10 已删，未编译，验收见 [执行清单](../handoff/gate-online-identity-simplify-20261105/CODEX.md) 第 8 节）：CreateGuild 的 `player_id` / `zone_id`，LeaveGuild、DisbandGuild、SetAnnouncement 的 `player_id`，GetGuildRank、GetGuildRankByGuild 的 `zone_id`，服务端早已一律忽略（身份取会话、区取归属）。项目未上线，按标准做法从协议删掉并把字段号写成 `reserved`，不再给读代码的人留「也许会被采信」的误会。唯一还带 `player_id` 的客户端请求是 GetPlayerGuild，只在不带会话的内部只读调用（mail 领附件）时采信。仓库内的 robot 冒烟同步：建帮不再故意填错区、别区查榜不再伪造区、第 8 步改为按会话身份退帮。这是客户端契约变更：Unity 客户端与协议生成由 Codex 处理；Java 版本轮只在 `PARITY.md` 记 TODO（§8）。
3. **全服帮会榜**（2026-11-10 负责人定删除，当日已删，未编译，验收同上第 8 节）：负责人 2026-11-10 答复：运维 / GM 都不需要全服帮会榜，删掉；现在就做，不等第 1 条的 Codex 回执。问之前逐处核过读者：第 1 条之后全服榜 ZSET `guild_rank` 只写不读，客户端只看本区榜，`tools/merge_zone`、`tools/data_consistency_check`、合服运维手册、robot 与 Java 网关后台都只碰分区榜 `guild_rank:zone:*`，Java 版本来就不提供全服榜。留着它，每次推分多一次 ZADD，全部帮会挤在一个键上，上 Redis Cluster 就是单槽热点。删除范围：`UpdateGuildScore` 只写最终区的分区榜；`RebuildRanks` 只重建分区榜；`RemoveGuildFromRank` 只从各分区榜摘除；data 层 `GetGuildRankPage` / `GetGuildRank` 不给区直接回 `ErrRankZoneRequired`，不再有「`zoneID=0` 读全服榜」的口径。`zone_id=0` 的帮会行是坏数据（帮会都按归属区建），分数照常落库、不进任何榜并打错误日志。三份[帮会排行架构](guild_ranking_architecture_zh.md)文档已按只有分区榜重写。旧环境里残留的 `guild_rank` 键不再有人读写，`RebuildRanks` 只扫 `guild_rank:zone:*`，不会顺手删它；项目未上线，不写迁移代码：压测前清库（`dev_tools.ps1 -Command redis-wipe-local` 或 `-Command k8s-redis-wipe`，都要带 `-ConfirmWipeAllRedis`）时一并清掉，只删这一个键用 `redis-cli -h <guild 的 RedisClient 地址> -n 0 DEL guild_rank`。

## 10. 已知边界的处理方案（2026-11-11 定；批 1 当日落码，批 2～4 2026-11-12 落码；均未编译）

负责人 2026-11-11 要求开始处理 §7 的已知边界。逐条结论如下，按批落码；每批落码后改写 §2～§7 的正文，不在旧文上加「已取代」标注。批 1 的 Codex 验收见 [执行清单](../handoff/gate-online-identity-simplify-20261105/CODEX.md) 第 9 节，批 2～4 与 §9.3 的读路径归属预检见第 10 节。

| 边界 | 结论 | 批次 |
|---|---|---|
| §7.4 一把对称钥 | gate 改用 Ed25519 私钥签名，验证方只持公钥；HMAC v1 整体删除 | 批 1 |
| §7.1 跨副本重放与正文 | 在网络层闭合：在线服务端口加 NetworkPolicy 白名单；预发与生产集群的 Pod 间流量必须透明加密。应用层不加共享 nonce、不签正文 | 批 2、批 3 |
| §7.3 设备软上限 | gate 为在线连接续租 `login_session` 与设备集 | 批 4 |
| §7.2 撤销不是跨服务原子的 | 不是缺陷，不改（§10.5） | 只改文档 |

### 10.1 批 1：gate 改用 Ed25519 签名（§7.4；2026-11-11 落码，未编译）

**为什么是非对称签名。** 对称 HMAC 下签名方和验签方持同一把钥，任何一个在线服务被攻破都能冒充 gate。改成非对称签名后，验证方只持公钥，持公钥签不出名：被攻破的 chat 伪造不出发往 trade 的身份。内部身份令牌的通行做法就是这样（JWT 的 EdDSA / RS256 同理）。

另一个候选是按目标服务分钥（gate 持多把，每个服务只持自己那把），没有选它：
- gate 要多维护一张「方法 → 目标服务」的映射表；
- 部署要给每个服务各发一把密钥。

非对称签名下，验证方的配置里不再有任何密钥，§9.5 原先担心的票据过期、续签、撤销也不存在：签名仍是 gate 每个请求现签，只是换了算法。

- **算法**：Ed25519（RFC 8032），签名是确定性的。C++ 用 OpenSSL 3 的 `EVP_PKEY_ED25519`，Go 用标准库 `crypto/ed25519`。
- **canonical**：结构与 v1 相同，第一行改为 `mmorpg-gate-identity/v2`。`x-caller-sig` 是 64 字节签名的小写十六进制（128 个字符）。头部名称、±30s 时间窗、nonce 表都不变。
- **密钥**：
  - gate：`MMORPG_INTERNAL_AUTH_SIGNING_KEY`，32 字节 Ed25519 种子的十六进制。K8s 由 Secret `gate-identity-auth`（键 `signing-key`）只注入 gate。
  - 验证方（login 与 7 个在线服务）：`MMORPG_INTERNAL_AUTH_PUBLIC_KEYS`，逗号分隔的 32 字节公钥十六进制。公钥不是密钥，K8s 上由 `k8s_deploy.ps1` 明文写进 env，验证方不引用 `gate-identity-auth`。
  - gate 启动时也读这份公钥列表，自己的公钥不在列表里就拒绝启动。这样「签名钥和验证方公钥不配对」在 gate 启动时就暴露，不会等到上线后全量验签失败。
  - 生成：在 `go/shared` 下执行 `go run ./cmd/identitykeygen`，一次输出种子与公钥；`-derive` 由已有种子推出公钥。种子只交给密钥管理，不落盘、不进日志。没有加 `dev_tools.ps1` 命令，原因是那个文件多会话共改，一条 `go run` 就够用。
  - 轮换三步，不需要专门的轮换开关：
    1. 新公钥加进验证方列表，滚动验证方；
    2. gate 换种子，滚动 gate；
    3. 旧公钥移出列表。
  - 本机开发：开发密钥对取 RFC 8032 §7.1 TEST 1 的公开测试向量，单测钉住二者配对。本机启动器、docker 调试栈、`k8s_deploy.ps1` 的 dev 档都用这一对，只在 dev/test 用。本机启动器只给 gate 注入私钥，Go 服务只拿公钥。
- **删除**：v1 HMAC、`MMORPG_INTERNAL_AUTH_SECRET` 及其 `_ADDITIONAL_ENVS` 轮换口、login 配置里的 `Secrets.InternalAuth`，以及部署脚本里以它为由的「排队令牌不得与之复用」检查（改为与签名私钥比较）。项目未上线，不留混跑窗口，gate 与全部验证方同批上线。
- **性能**：
  - Ed25519 的签名和验签都比一次 HMAC 贵一到两个数量级。OpenSSL 与 Go 标准库公开的量级都是每次数十微秒，以 Codex 跑出的基准为准，先不宣称结论。
  - 只有经 gate 发往 Go 服务、带会话头的请求才签名和验签；场景、战斗流量不经过这里。
  - 验签放在调用方、时间窗、nonce 形状这些字符串检查之后，格式不对的流量不消耗验签 CPU。
  - 加了 Go 基准 `BenchmarkPublicKeysVerify`。gate 侧由 `routing_identity_test` 的 `SigningCostPerRequestIsReported` 量出单次签名（一次 SHA-256、一次 Ed25519、一个随机 nonce）的平均耗时并打进测试日志（`[caller_auth] sign cost: … us/request`），给容量估算用；它只设一个 2 毫秒的宽上限拦数量级退化，不当性能结论。私钥只在启动时解析一次，每次签名不重建。
- **不改**：签名头、nonce 表、入口准入（§9.2）、提交点复核。
- **改动范围**：文件清单见执行清单第 9 节。`k8s_deploy.ps1` 与 login 配置正被其他会话大量修改，只做了定点替换。

### 10.2 批 2：在线服务端口白名单（§7.1，并为 §7.4 加纵深；2026-11-12 落码，未运行）

- **规则**：K8s NetworkPolicy。login 与 7 个在线服务的 gRPC 端口默认拒绝入站，只放行清单里的来源。清单是 `k8s_deploy.ps1` 的 `$script:GrpcAllowLists`，一处定义，生成策略、删除策略、契约测试都只读它：

  | 目标 | 策略名 | 放行的调用方（按 `app` 标签） | 为什么要它 |
  |---|---|---|---|
  | login | `login-grpc-allowlist` | gate、client-rpc-router、gateway | 登录与选角（直连和路由两条路径；断线通知、迁移要票、续租在直连模式下也由 gate 直发）；Java 网关分配 gate、查排队与运维接口 |
  | match | `match-grpc-allowlist` | client-rpc-router、gate、guild、battle、scene 三种、data-service | 客户端协议（gate 只在直连模式下直连）；帮会历练开局与占用；battle 交终局（`CommitBattleOutcome`）；scene 读队伍成员与对局终局；护栏协调者 |
  | guild | `guild-grpc-allowlist` | client-rpc-router、mail、data-service | 客户端协议；mail 判帮会邮件可见性（读 GetPlayerGuild）；护栏协调者 |
  | trade | `trade-grpc-allowlist` | client-rpc-router、data-service | 客户端协议；护栏协调者。trade → scene 的资产通道是 trade 往外拨，不经这个口 |
  | friend | `friend-grpc-allowlist` | client-rpc-router、data-service | 客户端协议；护栏协调者 |
  | mail | `mail-grpc-allowlist` | client-rpc-router、rank、scene 三种、data-service | 客户端协议；榜单发奖；战斗掉落放不下时 scene 改投邮件（`MailAdmin.SendPersonalMail`）；护栏协调者 |
  | chat | `chat-grpc-allowlist` | client-rpc-router | 只有客户端协议；世界频道广播走 Kafka |
  | rank | `rank-grpc-allowlist` | client-rpc-router、match | 客户端协议；match 结算后上报评分 |

  scene 的 gRPC 口原来就有一份（`scene-grpc-allowlist`，资产通道的前提），本次不动。两条发布轨道都放行：清单里的 Go 服务与 battle 自动带上 `-canary` 的标签。
- **本次顺带修掉的两处漏项**：原有的 match、mail 清单各少一个真实调用方（battle → match 的 `CommitBattleOutcome`，scene → mail 的 `SendPersonalMail`）。白名单一开，这两条调用会直接连不上；现在补进清单并由契约测试钉住。
- **强制**：`-NetworkPolicies on` 才生成这些策略；`off` 时删掉同名策略。预发与生产必须开（参数或环境变量 `MMORPG_NETWORK_POLICIES=on`），否则真部署拒绝、`-DryRun` 打一行 `WARNING:`。dev 档不强制。
- **清单怎么来的**：从各服务配置里的下游地址与 `serviceauth` 调用方名单逐个核出。清单之外的调用在 K8s 上会直接连不上，所以改调用关系要同批改清单和契约测试（`k8s_deploy_contract.tests.ps1` 逐条钉住每份清单的内容）。
- **效果**：scene、运维工具等不该直连某个在线服务的进程，连端口都连不上；被攻破的服务也只能连到它本来就要调的服务。
- **不管的范围**：策略只管 gRPC 端口，指标端口沿用集群现有做法。`kubectl port-forward` 由 kubelet 在 Pod 自己的网络命名空间里连本机端口，不经过策略，排障仍可用。
- **运维工具**（`go/guild/cmd/assetopfix`、TradeAdmin 造数这类）在清单内的 Pod 里跑，或走 `kubectl port-forward`。确实要多放一个调用方时，改清单并同批改契约测试后重新部署；预发与生产不能靠传 `off` 关掉。
- **前提**：集群的 CNI 必须执行 NetworkPolicy（Calico 或 Cilium）；不执行的 CNI 会接受策略对象却不生效。
- **实测**（2026-11-12 起，`tools/scripts/k8s_netpol_probe.ps1`）：契约测试只证明策略对象写对了，拦不拦得住看网络插件，所以在部署好的集群上直接连一遍：
  - 读出全部准入策略（标签 `mmorpg.io/managed-by=k8s_deploy.ps1`、名字以 `-grpc-allowlist` 结尾），形状与生成器对不上就判失败。
  - 在独立的探测 namespace 里为名单里的每个 app 标签起一个探测 Pod，再起一个不在任何名单里的（`app=netpol-probe-unlisted`）。策略按 app 标签放行、不限 namespace，探测 Pod 与真实调用方受同样的对待；独立 namespace 不会被任何 Service 选中。
  - 先做两个对照，证明探测工具可靠：连 API server 必须连得上，连本机没人监听的口必须连不上。
  - 每份策略的每个靶子：名单里的每个调用方都必须连得上 gRPC 口，名单之外的必须连不上；名单里一个都没连上时判「不确定」，不拿「连不上」当成拦住了。没起的金丝雀目标（`<名字>-canary` 一个 Pod 都没有）只提示，有 Pod 却没就绪算失败。
  - 只在自己的 namespace 里建 Pod；那个 namespace 已存在而不是它建的就拒绝运行。探测 Pod 带着真实调用方的 app 标签，跨 namespace 按 app 标签选 Pod 的运维操作会看到它们，所以优先在 kind 或预发上跑。
  - kind 上用 `deploy/k8s/kind-netpol-config.yaml`：关掉默认的 kindnet，装 Calico，两个节点。
  - 放行条件按 app 标签、不限 namespace：能在集群里任意建 Pod、任意打标签的人能冒充调用方，但那样的权限本身就能做更多事，集群专用于本项目，不再按 namespace 收窄。

### 10.3 批 3：Pod 间流量透明加密（§7.1；2026-11-11 落码，2026-11-12 改为逐节点核对；未运行）

- **要求**：预发与生产集群的 Pod 间流量必须透明加密，用 CNI 的 WireGuard：Cilium 的 `encryption.type=wireguard`，或 Calico 的 `wireguardEnabled`。加密在内核完成，对 muduo 自定义协议与 gRPC 同样生效，应用不用改。
- **部署前检**（`k8s_deploy.ps1` 的 `Assert-PodNetworkEncryptionPreflight`，在任何写操作之前）：
  - 用 `-PodNetworkEncryption cilium-wireguard|calico-wireguard` 声明集群用哪一种；包装入口不透传参数时用环境变量 `MMORPG_POD_NETWORK_ENCRYPTION`。缺省 `none`。
  - 预发与生产没声明：真部署拒绝，什么都不写。
  - 声明了：只读核对集群，两层都要过，对不上就拒绝：
    1. 开关：Cilium 读 `kube-system/cilium-config` 的 `enable-wireguard`，Calico 读 `FelixConfiguration default` 的 `spec.wireguardEnabled`，都要等于 `true`。
    2. 每个节点都真的启用了（2026-11-12 起）：Calico 在启用了 WireGuard 的节点上把公钥写进 Node 的注解 `projectcalico.org/WireguardPublicKey`，Cilium 写进同名 CiliumNode 的注解 `network.cilium.io/wg-pub-key`；每个 Node 都要有。只看开关不够：开关打开、某个节点的内核不支持 WireGuard 时，Calico 不报错，只是那个节点进出的流量不加密。拒绝时点名缺公钥的节点。
  - 核对项与判据在 `tools/scripts/lib/pod_network_encryption.ps1`，部署前检与实测脚本（`k8s_netpol_probe.ps1 -PodNetworkEncryption …`）共用一份。只认 IPv4 的公钥注解：集群是单栈 IPv4。
  - `-DryRun` 不连集群：声明了只打印本次取值，没声明打一行 `WARNING:`（写在标准输出，预览里直接看得到）。
  - dev 档不要求。
- **网格 mTLS**：项目没有上服务网格，前检只认上面两种声明。以后要用网格 mTLS 顶替，先给前检加一种声明和对应的只读核对，不拿它冒充这两种之一。
- **不再加的东西**：加密之后，要截获签名头得先攻破节点或 Pod，那时攻击者能做的远不止重放，所以不再加共享 nonce 存储，也不签正文。路由服本来就能丢弃或改写请求，属于可信转发者，不按攻击者对待。
- **不选应用层 TLS 的原因**：gate → 路由服走 muduo 自定义 RPC，muduo 没有 TLS，补上的代价远大于集群层加密。

### 10.4 批 4：在线连接续租登录态（§7.3；2026-11-12 落码，未编译）

寿命本来只是「连接断了、Disconnect 又没送到」时的回收兜底，不该比活着的连接先到期。做法是连接活着期间由 gate 定期替它续租；连接没了就没人续，记录按寿命过期。

- **协议**：`ClientPlayerLogin.RenewSession`（SERVER_ONLY，请求体为空）。只有 gate 合成：连接身份走签名的 `SessionDetails`，路由模式经路由服、直连模式直发 login，与 gate 代发的断线通知、迁移要票同一个出口（`SendGateSynthesizedLoginRequest`）。客户端发来这个消息号在 gate 入口就被拒（`login_admission::IsGateSynthesizedOnly`）。应答两个字段：`renewed`，以及 `renew_after_seconds`（下一次续租的间隔）。
- **寿命只在 login 配**：间隔由 login 按 `SessionExpireMin` 的三分之一给出，gate 不另配寿命，两边不会配岔。三分之一意味着连续失败两次之后登录态仍在。
- **login 做什么**（`loginsession.Renew`，两步都只动单槽的键）：
  1. 一段 Lua（`sessionfence.Renew`）：会话栅栏标了 `closed` 就什么都不写；否则给还在的 `login_session` 续期，栅栏已绑定角色时把它续到绑定寿命。「连接没关」的检查和续期在同一段脚本里，断线清理之后迟到的续租复活不了登录态。
  2. 登录态还在时，把本连接重新登记进账号设备集并续期（`SADD` + `PEXPIRE`）。用登记而不是只续期：成员万一丢了也在这里补回。
  - 两步之间被 Disconnect 抢先时设备集会留一个陈旧成员，与登录提交的同类竞态一样，由设备数检查时的清理剔除。
  - 没有可续的记录（离开游戏之后、已按断线处理、已过期）回 `renewed=false`，不报错；Redis 故障回 `Unavailable`，gate 退避后重试。
  - 顺带关掉会话栅栏原先的 7 天残留：已绑定的栅栏记录原先写一次 7 天寿命、在线超过 7 天就过期；现在每次续租都把它续满。
  - **登录态过期之后的重建**（2026-11-12 起）：绑定角色时脚本把登录态里的账号抄一份进栅栏记录（字段 `account`），同一连接绑定期间重新登录时副本跟着改，离开游戏解除绑定时连同副本一起删。续租发现登录态没了、连接却没关且还绑着角色时，按这份副本重建登录态，第 2 步随之把连接重新登记进设备集。
    - 副本是 login 自己写的，不取 gate 带来的账号，登录态仍然只来自一次真实的 Login。
    - 删登录态的只有断线与离开游戏两条路径，它们都先关掉重建的来源：断线先把栅栏标成 `closed`，离开游戏先解除绑定。所以重建只会把「按寿命过期、连接还在游戏里」的登录态原样恢复，复活不了被断线或离场清掉的。
    - 没绑定角色的连接（还在选角）没有副本，登录态过期就是没有可续的。
- **gate 做什么**（`gate_login_renewal`，只在 gate 的事件循环线程上跑）：
  - 一条连接收到成功的 Login 回包时排一次续租，下一拍发出；第一次续租同时向 login 取间隔。
  - 到期项放在一个按到期时刻排序的最小堆里，每秒一拍，没有到期项时只做一次堆顶比较。每拍最多发 200 个、在途不超过 400 个：login 故障恢复后所有连接同时到期时按这个速率摊开，5 万条连接约 4 分钟补完，远小于间隔留下的余量。
  - 间隔夹在 5 秒到 24 小时之间，再按会话号提前至多五分之一，开服时成批登录的连接不会每个周期都在同一秒续租。
  - 应答 `renewed`：按间隔排下一次。应答 `renewed=false`：停止，直到这条连接再次登录成功；在途期间又登录成功过的，立刻再问一次。调用失败或没发出：30 秒后重试。
  - 连接断开不用通知这个模块：到期项按会话号与会话代次核对，对不上就丢弃。回调只带会话号与代次，不持有连接（AGENTS.md §11.7）。
- **失败时的后果**：续租失败不影响在线玩家的任何请求。连续失败超过一个寿命时登录态过期，这段时间里这条连接不计入设备数；故障恢复后，已在游戏里的连接由下一次续租重建并重新计数，还在选角的连接要重新登录（§7.3）。
- **水平扩展**：每个 gate 只续自己的连接，gate 之间没有共享状态；login 无状态，任何副本都能受理。负载估算见 §6。
- **观测**：login 的 `loginsession_renew_total{result}`；gate 每分钟一行 `[LoginRenewal] summary`（§5）。
- **与发布的关系**：login 的 goctl 包装（`clientplayerloginserver.go`）是生成物。协议生成之后要再跑 `go/build.ps1` 把 `RenewSession` 接到 `renewsessionlogic.go`，否则 login 回 `Unimplemented`，gate 每 30 秒重试、登录态照旧按寿命过期。项目未上线，gate 与 login 同批上线，不留混跑窗口。

### 10.5 §7.2 不改的理由

撤销（踢线、离场、冻结、过户）在分布式系统里本来就是最终一致的：已经发出的请求不可能被跨服务原子地收回。这里的设计把正确性放在提交点：
- 资产与归属由各库的 `role_fence` 在提交时用同一把锁裁决；
- 撤销之后才提交的写一律被拒；
- 撤销之前已提交的写本来就合法。

在途请求受整请求预算约束，不会无限期悬挂。所以这一条是设计如此，不是待修的缺口，只在 §7 里改写措辞。

### 10.6 两版

Java 版内部走 Dubbo 认证，机制不同，不照抄。批 1 落码时在 Java 仓 `PARITY.md` 登记了待核对项：Java 的网关身份传递是否也是「验证方持有能签名的同一把钥」。批 2～4 与读路径归属预检落码时又登记了三项（§8）：同步 `RenewSession` 的协议与消息号表、设备上限是否按在线连接计数；只读方法有没有核归属；部署是否同样要求端口白名单与 Pod 间加密。Java 本轮不改代码。

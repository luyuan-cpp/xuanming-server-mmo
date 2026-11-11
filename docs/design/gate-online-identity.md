# Gate 在线身份：login 认证、gate 持有、签名传递、提交点复核

日期：2026-11-05（美东）。状态：已落码。C++/Go 版链路由 Codex 编译与真连接验收（见 [审核交接](../handoff/gate-auth-role-fence-review-20261104.md)）；同日删除在线域的 login Redis 回退，未编译，待 Codex 按 [执行清单](../handoff/gate-online-identity-single-source-20261105/CODEX.md) 验证。同日另定简化方案（§9）：第 1 批（统一在线域客户端入口、关闭 guild 无会话冒名口）2026-11-06 已落码，**未编译**，与上一批合并由 Codex 按 [第 1 批执行清单](../handoff/gate-online-identity-simplify-20261105/CODEX.md) 验证；第 2 批（删 friend 入口重复预检）2026-11-06 已落码，**未编译**，验收见同一清单的第 2 批一节，计划见 [简化计划](../handoff/gate-online-identity-simplify-20261105/PLAN.md)；§9.6 第 1 条（guild 三个读方法删无会话分支）2026-11-06 已落码，`go/guild` 的 build / vet / test 2026-11-10 已由 Codex 跑通（集成用例未跑），见同一清单第 7 节；§9.6 第 2、3 条（删 guild 请求体里被忽略的身份与区字段、删全服帮会榜）2026-11-10 已落码，**未编译**，验收见同一清单第 8 节。§7 的四条已知边界 2026-11-11 定了处理方案（§10）：批 1（gate 改 Ed25519 签名、验证方只持公钥）当日已落码，**未编译**，验收见同一清单第 9 节；批 2～4 未开工。Java 版待做（§8）。

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

为什么还要提交点复核：入口证明的是「这条连接、这个账号、这次归属」，不证明此刻角色仍归他。Bind 之后角色可能被冻结、过户、删除或换修订；`role_fence` 在提交点用同一把锁裁决，入口检查只是提前拒绝。

## 2. 生命周期

| 事件 | login_session（登录域 Redis） | gate 会话身份 | 在线请求结果 |
|---|---|---|---|
| Login 成功 | 写入，TTL = `Node.SessionExpireMin`（缺省 30 分钟），并登记账号设备集 | `loginSucceeded`，尚无账号 | 在线域拒绝（未绑角色） |
| EnterGame 受理 → Bind 到达 | 保留（设备集按它判断连接是否还在） | 写入原账号与原修订 | 签名带原账号，在线域放行到业务 |
| 在线期间 | 不被在线域读取；过期只影响设备软上限计数 | 不变；换图只改路由修订 | 正常 |
| 角色过户 / 冻结 / 删除 | — | 不变（原修订） | 提交点 `role_fence` 拒绝 |
| LeaveGame | login 删除 | LeaveGame **发出时**由 `ResetSceneRoute` 清空原账号与原修订（路由模式在 lane 轮到它发出时执行，直连模式收包即发出；2026-11-11 起，原先路由模式在收包时执行）。lane 里排在它前面的请求按原身份先执行；它还排着时受理的请求，入队快照不带原账号；发送前复核失配即回 `kLoginSessionNotFound`（见 [client-request-ordering.md](client-request-ordering.md) §4.3、§4.4） | 在线域拒绝；再进游戏须重新 Login |
| 断线 | Disconnect 删除 | 会话摘除 | 无连接 |
| gate 进程重启 | 旧键按 TTL 自然过期（键含旧 UUID，新连接用不上） | 新进程新 UUID，新连接新代次，没有任何旧身份 | 客户端重新取票 → Login（token 有效即可）→ 对原角色 EnterGame；仅凭 player_id 不能恢复。旧会话由 player_locator 的 gate 清扫回收 |

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
- `online_entry_rejected_total{reason}`：在线域入口准入的拒绝（§9.2）。`incomplete_identity` 是客户端驱动的（离场后仍发在线请求、绑定事件未到时抢发），稳态应接近 0；`client_without_session`（内部进程不带会话调客户端方法）、`internal_with_session`（带会话调内部方法）、`unverified_session`（会话头没有验签原件，即拦截器装配顺序错）都意味着代码缺陷或伪造，应恒 0。
- login 自己的 `login_caller_auth_total{result,mode}` 与 `login_caller_auth_nonce_overflow_total` 不变。

## 6. 水平扩展

- 验签是本进程 CPU（一次 SHA-256 + 一次 Ed25519 验签，每次数十微秒量级，以 `BenchmarkPublicKeysVerify` 实测为准），不访问任何存储；在线服务加副本不增加 login Redis 的连接数，也不给登录域加读负载。入口的归属预检另算：只剩 guild 每请求读一次本库护栏副本（不加锁）；friend 原先为每个客户端请求开一个写事务核原件后回滚，与业务事务里的核查重复，2026-11-06 删除（§9.3），每个请求少一次写事务。
- 路由服无状态透传；gate 只持本进程连接的身份，扩 gate 不需要共享会话存储。
- 角色归属的强一致由各逻辑库自己的 `role_fence` 提交锁保证，不依赖跨库 SQL 或集中目录查询。

## 7. 已知边界（不宣称已解决）

2026-11-11 起按 §10 逐条处理。下面写的是现状，每批落码后改写对应条目。

1. **跨副本重放与正文**：nonce 表是进程内的，同一条签名在 ±30s 内可重放到同一服务的另一副本；HMAC 绑定身份与方法、不覆盖请求正文，能嗅探内网流量的一方可在窗口内把截获的签名头配上别的正文，打到另一副本（同一副本会被 nonce 拒），路由服这类在途转发者则可以直接改正文。现状依赖内网隔离（服务端口不对公网暴露）。彻底闭合需要传输层 mTLS 或正文摘要 + 共享 nonce，需单独立项并做压测对比；选型见 §9.5，处理方案见 §10.2、§10.3。
2. **撤销不是跨服务原子的**：踢线 / 离场之前已经受理的请求按各业务契约完成或拒绝；冻结、过户的正确性由提交点护栏保证，不靠异步踢线消息。这是设计如此，不是待修的缺口（§10.5）。
3. **设备软上限**：`login_session` 是绝对 TTL，没有在线续期；在线超过 TTL 的连接不再计入 `MaxDevicesPerAccount`。只影响软上限，不影响身份与资产。处理方案见 §10.4。
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
4. **关闭 guild 冒名口。** CreateGuild / LeaveGuild / DisbandGuild / SetAnnouncement 不再接受无会话调用，`callerOf` 删除请求体回退；GetPlayerGuild 的无会话只读保留并登记为例外。请求体里的 player_id / zone_id 字段先留着，服务端一律忽略；删字段属于协议清理，另做。
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

guild 的入口预检不动：它读本库护栏副本、不加锁，是 guild 读方法唯一的归属检查。读路径要不要统一核归属，以后单独定。

### 9.4 nonce 表保留

nonce 签进了 canonical，同一进程内截获的签名头不论原样重放还是换正文都只能用一次（§3），跨副本拦不住（§7.1）。它对单副本服务和同副本复用有效，删掉换来的简化不值，只修 §5 的告警口径。

### 9.5 不做

- **共享 nonce 存储、把正文摘要签进去。** 前者每个请求多一次存储往返；后者在路由模式下要 gate 解析并确定性重编码正文，Go 侧还要拿到原始字节。防内网嗅探和篡改的做法是在网络层加密 Pod 间流量（§10.3），同时收紧端口白名单（§10.2），不在签名上继续加层。
- **login 签发、gate 只转交的非对称身份票据。** 这种做法要配票据过期、续签与撤销，不做。§7.4 的持钥冒充改由 gate 对每个请求用 Ed25519 现签来解决（§10.1），没有票据生命周期。
- **合并 `callerauth` 与 `serviceauth`。** 两套内部签名长期应收敛成一套，不在本方案；新服务不要再造第三套。

### 9.6 剩余（2026-11-06 记；2026-11-10 三条都已落码，未编译）

1. **guild 三个读方法的无会话分支**（2026-11-06 已删；`go/guild` 单测 2026-11-10 已由 Codex 跑通，集成用例未跑，见 [执行清单](../handoff/gate-online-identity-simplify-20261105/CODEX.md) 第 7 节）：GetGuild、GetGuildRank、GetGuildRankByGuild 原先对不带会话的调用不查归属区、沿用请求体 `zone_id`（全服榜 `zone_id=0`）、不夹页长。生产里入口准入早已拒绝这类调用，只有单测故意走这条路径。现在三个方法与四个写方法一样先过 `requireClientSession`，区一律取 `clientZone`，榜单页长夹到 `MaxRankPageSize`。随之没有调用者的两处一并收掉：`viewerOf`（只为无会话调用回 0，CreateGuild / SetAnnouncement / GetGuild 现在直接传会话里的 player_id）与 `visibleIn` 的「`zoneID==0` 不按区过滤」；`expected_guild.go` 里「只给客户端来源计 absent」的判断同理删掉（五个调用方都只受理客户端来源）。测试改成客户端来源：请求者用本区旁观者（不在被查的帮里），`guildInfoFor` 不给他现算待审数，所以不需要待审数替身，也不用为测试给 `GuildLogic` 新开接缝；「内部调用保留全服榜」改为断言无会话被拒，另加三个读方法无会话被拒的表驱动用例。运维要全服榜，另设带 `serviceauth` 的内部 RPC，不复活这条分支。
2. **guild 请求体里的身份与区字段**（2026-11-10 已删，未编译，验收见 [执行清单](../handoff/gate-online-identity-simplify-20261105/CODEX.md) 第 8 节）：CreateGuild 的 `player_id` / `zone_id`，LeaveGuild、DisbandGuild、SetAnnouncement 的 `player_id`，GetGuildRank、GetGuildRankByGuild 的 `zone_id`，服务端早已一律忽略（身份取会话、区取归属）。项目未上线，按标准做法从协议删掉并把字段号写成 `reserved`，不再给读代码的人留「也许会被采信」的误会。唯一还带 `player_id` 的客户端请求是 GetPlayerGuild，只在不带会话的内部只读调用（mail 领附件）时采信。仓库内的 robot 冒烟同步：建帮不再故意填错区、别区查榜不再伪造区、第 8 步改为按会话身份退帮。这是客户端契约变更：Unity 客户端与协议生成由 Codex 处理；Java 版本轮只在 `PARITY.md` 记 TODO（§8）。
3. **全服帮会榜**（2026-11-10 负责人定删除，当日已删，未编译，验收同上第 8 节）：负责人 2026-11-10 答复：运维 / GM 都不需要全服帮会榜，删掉；现在就做，不等第 1 条的 Codex 回执。问之前逐处核过读者：第 1 条之后全服榜 ZSET `guild_rank` 只写不读，客户端只看本区榜，`tools/merge_zone`、`tools/data_consistency_check`、合服运维手册、robot 与 Java 网关后台都只碰分区榜 `guild_rank:zone:*`，Java 版本来就不提供全服榜。留着它，每次推分多一次 ZADD，全部帮会挤在一个键上，上 Redis Cluster 就是单槽热点。删除范围：`UpdateGuildScore` 只写最终区的分区榜；`RebuildRanks` 只重建分区榜；`RemoveGuildFromRank` 只从各分区榜摘除；data 层 `GetGuildRankPage` / `GetGuildRank` 不给区直接回 `ErrRankZoneRequired`，不再有「`zoneID=0` 读全服榜」的口径。`zone_id=0` 的帮会行是坏数据（帮会都按归属区建），分数照常落库、不进任何榜并打错误日志。三份[帮会排行架构](guild_ranking_architecture_zh.md)文档已按只有分区榜重写。旧环境里残留的 `guild_rank` 键不再有人读写，`RebuildRanks` 只扫 `guild_rank:zone:*`，不会顺手删它；项目未上线，不写迁移代码：压测前清库（`dev_tools.ps1 -Command redis-wipe-local` 或 `-Command k8s-redis-wipe`，都要带 `-ConfirmWipeAllRedis`）时一并清掉，只删这一个键用 `redis-cli -h <guild 的 RedisClient 地址> -n 0 DEL guild_rank`。

## 10. 已知边界的处理方案（2026-11-11 定；批 1 当日落码，未编译；批 2～4 未开工）

负责人 2026-11-11 要求开始处理 §7 的已知边界。逐条结论如下，按批落码；每批落码后改写 §3、§4、§7 的正文，不在旧文上加「已取代」标注。批 1 已按此改写 §3、§4、§6、§7.4，Codex 验收见 [执行清单](../handoff/gate-online-identity-simplify-20261105/CODEX.md) 第 9 节。

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
  - 加了 Go 基准 `BenchmarkPublicKeysVerify`；gate 侧签名没有单独的微基准，需要时再加。
- **不改**：签名头、nonce 表、入口准入（§9.2）、提交点复核。
- **改动范围**：文件清单见执行清单第 9 节。`k8s_deploy.ps1` 与 login 配置正被其他会话大量修改，只做了定点替换。

### 10.2 批 2：在线服务端口白名单（§7.1，并为 §7.4 加纵深）

- **规则**：K8s NetworkPolicy。login 与 7 个在线服务的 gRPC 端口默认拒绝入站，只放行以下来源：
  - gate；
  - client_rpc_router；
  - 登记过的内部调用方，例如 mail → guild 的 GetPlayerGuild、guild / battle → match 的 MatchInternal。

  指标端口只放行监控命名空间。
- **调用关系**：从各服务配置里的下游地址与 `serviceauth` 调用方名单逐个核出，写成清单与部署契约测试。清单之外的调用在 K8s 上会直接连不上，所以清单就是事实源。
- **效果**：scene、battle、data_service、运维工具等不该直连在线服务的进程，连端口都连不上；被攻破的服务也只能连到它本来就要调的服务。
- **前提**：集群的 CNI 必须执行 NetworkPolicy（Calico 或 Cilium）。本机 kind 冒烟按同一 CNI 起。

### 10.3 批 3：Pod 间流量透明加密（§7.1）

- **要求**：预发与生产集群的 Pod 间流量必须透明加密。
  - 用 CNI 的 WireGuard：Cilium 的 `encryption.type=wireguard`，或 Calico 的 `wireguardEnabled`。
  - 已上服务网格的，用网格 mTLS。

  加密在内核或边车完成，对 muduo 自定义协议与 gRPC 同样生效，应用不用改。
- **部署前检**：非 dev 档要求声明集群的加密方式并核对；核不到就拒绝部署。
- **不再加的东西**：加密之后，要截获签名头得先攻破节点或 Pod，那时攻击者能做的远不止重放，所以不再加共享 nonce 存储，也不签正文。路由服本来就能丢弃或改写请求，属于可信转发者，不按攻击者对待。
- **不选应用层 TLS 的原因**：gate → 路由服走 muduo 自定义 RPC，muduo 没有 TLS，补上的代价远大于集群层加密。

### 10.4 批 4：在线连接续租登录态（§7.3）

- **做法**：gate 按 `SessionExpireMin` 的三分之一为周期，把本进程在线连接的会话栅栏批量交给 login 续租。login 对 `login_session` 与设备集执行 `PEXPIRE`。
- **效果**：连接断开后不再续租，最长 `SessionExpireMin` 后过期；设备数上限因此按在线连接计数，在线超过 TTL 的连接不再漏计。
- **待定细节**：批量大小、按槽分组、login 故障时的处理。落码前与改登录域 Redis 的会话对齐，那边正在审登录总账。

### 10.5 §7.2 不改的理由

撤销（踢线、离场、冻结、过户）在分布式系统里本来就是最终一致的：已经发出的请求不可能被跨服务原子地收回。这里的设计把正确性放在提交点：
- 资产与归属由各库的 `role_fence` 在提交时用同一把锁裁决；
- 撤销之后才提交的写一律被拒；
- 撤销之前已提交的写本来就合法。

在途请求受整请求预算约束，不会无限期悬挂。所以这一条是设计如此，不是待修的缺口，只在 §7 里改写措辞。

### 10.6 两版

Java 版内部走 Dubbo 认证，机制不同，不照抄。批 1 落码时在 Java 仓 `PARITY.md` 登记待核对项：Java 的网关身份传递是否也是「验证方持有能签名的同一把钥」，网络层要求（批 2、批 3）是否同样适用。

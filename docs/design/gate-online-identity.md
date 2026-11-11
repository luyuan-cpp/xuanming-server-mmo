# Gate 在线身份：login 认证、gate 持有、签名传递、提交点复核

日期：2026-11-05（美东）。状态：已落码。C++/Go 版链路由 Codex 编译与真连接验收（见 [审核交接](../handoff/gate-auth-role-fence-review-20261104.md)）；同日删除在线域的 login Redis 回退，未编译，待 Codex 按 [执行清单](../handoff/gate-online-identity-single-source-20261105/CODEX.md) 验证。同日另定简化方案（§9）：第 1 批（统一在线域客户端入口、关闭 guild 无会话冒名口）2026-11-06 已落码，**未编译**，与上一批合并由 Codex 按 [第 1 批执行清单](../handoff/gate-online-identity-simplify-20261105/CODEX.md) 验证；第 2 批（删 friend 入口重复预检）2026-11-06 已落码，**未编译**，验收见同一清单的第 2 批一节，计划见 [简化计划](../handoff/gate-online-identity-simplify-20261105/PLAN.md)；§9.6 第 1 条（guild 三个读方法删无会话分支）2026-11-06 已落码，**未编译**，验收见同一清单第 7 节；§9.6 第 2、3 条（删 guild 请求体里被忽略的身份与区字段、删全服帮会榜）2026-11-10 已落码，**未编译**，验收见同一清单第 8 节。Java 版待做（§8）。

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
| LeaveGame | login 删除 | `ResetSceneRoute` 清空原账号与原修订；排队请求发送前复核失配即回 `kLoginSessionNotFound` | 在线域拒绝；再进游戏须重新 Login |
| 断线 | Disconnect 删除 | 会话摘除 | 无连接 |
| gate 进程重启 | 旧键按 TTL 自然过期（键含旧 UUID，新连接用不上） | 新进程新 UUID，新连接新代次，没有任何旧身份 | 客户端重新取票 → Login（token 有效即可）→ 对原角色 EnterGame；仅凭 player_id 不能恢复。旧会话由 player_locator 的 gate 清扫回收 |

客户端自动重连属于客户端改动，服务端契约不变：重新登录走的是现有 `ReplaceLogin / LOGIN_RECONNECT` 判定。

## 3. 签名契约（v1）

签名方只有 C++ gate（`cpp/libs/engine/core/security/caller_auth.h`），验证方是 `go/shared/callerauth`，两边逐字一致：

```
canonical = "mmorpg-login-caller-auth/v1\n" + caller("gate") + "\n" + 目标方法全名 + "\n"
          + lowercase-hex(sha256(SessionDetails 原始字节)) + "\n" + 毫秒时间戳 + "\n" + nonce
signature = lowercase-hex(HMAC-SHA256(MMORPG_INTERNAL_AUTH_SECRET, canonical))
```

- metadata：`x-session-detail-bin`（应用层 base64 一层，传输层 `-bin` 再一层，历史口径不改）、`x-caller-id`、`x-caller-ts`、`x-caller-nonce`、`x-caller-sig`（ASCII 原值，不做 base64）。
- 目标方法：直连签实际 RPC；路由模式签消息号对应的业务方法（不是本跳的 Forward），目标服务按自己的 `info.FullMethod` 验。方法名来自生成的 `grpcFullMethod`，两条路径同一张表。
- 窗口：时间偏移 ±30s；nonce 进程内两代轮转，保留 2×窗口；先验签再记 nonce，未认证流量占不了 nonce 表。nonce 签进了 canonical，所以在同一进程内，截获的签名头不论原样重放还是换正文都用不了第二次；打到另一副本则拦不住（§7.1）。
- 每次发送现签：排队请求在出队发送时才签，时间戳是发送时刻。

## 4. 密钥与配置

- 同一把内部身份密钥 `MMORPG_INTERNAL_AUTH_SECRET`（≥32 字节）注入 gate、login 与全部在线 Go 服务；gate 与 `GateInterceptor` 缺钥或不足 32 字节拒启。
- 轮换：先把新钥以 `MMORPG_INTERNAL_AUTH_SECRET_ADDITIONAL_ENVS` 列出的环境变量分发给验证方（只验不签），全部就位后切 gate 的签名钥，再撤旧钥。
- K8s 由 Secret `gate-identity-auth`（键 `secret`）注入；本机启动器沿用 `go/login/etc/login.yaml` 的开发值，非 dev/test 必须外部注入。不进 ConfigMap、不进日志。
- 在线域服务（chat / friend / guild / match / mail / trade）**没有** `GlobalLoginSessions`、`LoginRedis` 配置，也不持有 login Redis 连接。

## 5. 可观测性

- `caller_auth_session_verify_total{result}`：在线域验签结果，result 取 `callerauth.Reason` 的有限枚举。稳态 `result!="ok"` 应恒 0；非 0 先查密钥是否同源、时钟偏移、签名头是否被改写。
- `caller_auth_nonce_overflow_total`：nonce 表超上限提前翻代（只缩短重放窗口，不放行验签失败的请求）。上限 500000 条、一代 60 秒，所以单进程验签成功速率超过约 8.3k/s 时，正常负载下也会计数。非 0 先看该进程 `caller_auth_session_verify_total{result="ok"}` 的速率：低于约 8.3k/s 才按异常查；高于它是容量信号，加副本或调大上限。
- `online_entry_rejected_total{reason}`：在线域入口准入的拒绝（§9.2）。`incomplete_identity` 是客户端驱动的（离场后仍发在线请求、绑定事件未到时抢发），稳态应接近 0；`client_without_session`（内部进程不带会话调客户端方法）、`internal_with_session`（带会话调内部方法）、`unverified_session`（会话头没有验签原件，即拦截器装配顺序错）都意味着代码缺陷或伪造，应恒 0。
- login 自己的 `login_caller_auth_total{result,mode}` 与 `login_caller_auth_nonce_overflow_total` 不变。

## 6. 水平扩展

- 验签是本进程 CPU（一次 SHA-256 + 一次 HMAC），不访问任何存储；在线服务加副本不增加 login Redis 的连接数，也不给登录域加读负载。入口的归属预检另算：只剩 guild 每请求读一次本库护栏副本（不加锁）；friend 原先为每个客户端请求开一个写事务核原件后回滚，与业务事务里的核查重复，2026-11-06 删除（§9.3），每个请求少一次写事务。
- 路由服无状态透传；gate 只持本进程连接的身份，扩 gate 不需要共享会话存储。
- 角色归属的强一致由各逻辑库自己的 `role_fence` 提交锁保证，不依赖跨库 SQL 或集中目录查询。

## 7. 已知边界（不宣称已解决）

1. **跨副本重放与正文**：nonce 表是进程内的，同一条签名在 ±30s 内可重放到同一服务的另一副本；HMAC 绑定身份与方法、不覆盖请求正文，能嗅探内网流量的一方可在窗口内把截获的签名头配上别的正文，打到另一副本（同一副本会被 nonce 拒），路由服这类在途转发者则可以直接改正文。现状依赖内网隔离（服务端口不对公网暴露）。彻底闭合需要传输层 mTLS 或正文摘要 + 共享 nonce，需单独立项并做压测对比；选型见 §9.5。
2. **撤销不是跨服务原子的**：踢线 / 离场之前已经受理的请求按各业务契约完成或拒绝；冻结、过户的正确性由提交点护栏保证，不靠异步踢线消息。
3. **设备软上限**：`login_session` 是绝对 TTL，没有在线续期；在线超过 TTL 的连接不再计入 `MaxDevicesPerAccount`。只影响软上限，不影响身份与资产。
4. **一把对称钥，持钥即可冒充 gate**：gate、login 与 7 个在线服务（chat / friend / guild / match / mail / trade / rank）持有同一把 `MMORPG_INTERNAL_AUTH_SECRET`，`x-caller-id` 由签名方自己填写。任何持钥进程被攻破，都能以 gate 身份为任意玩家签名去调其他在线服务。签名真正防住的是不持钥的内部进程（scene、battle、data_service、scene_manager、路由服、运维工具）。要缩小这一面，最便宜的做法是按目标服务分钥（gate 持多把，每个服务只持自己那把）；当前不做。
5. **guild 无会话冒名口**（2026-11-05 发现，2026-11-06 第 1 批关闭，未编译）：`GuildService` 的 CreateGuild / GetPlayerGuild / LeaveGuild / DisbandGuild / SetAnnouncement 原先在请求不带会话头时按「内部调用」处理，直接用请求体里的 player_id（`go/guild/internal/logic/guild_logic.go` 的 `callerOf`），写事务只核目标角色的冻结与删除，不核账号与修订。任何能连上 guild 端口的进程不需要签名就能替任意玩家建帮、退帮、解散帮会、改公告，gate 签名对这四个写方法不起作用。现在入口准入拒绝不带会话的客户端方法，四个写方法在业务层另加 `requireClientSession`；唯一保留的无会话调用是 mail 领附件时经 `GuildRPCDirectory` 读 GetPlayerGuild（只读，登记为匿名例外）。

login 与在线域都没有「验签失败告警放行」的宽松档：2026-11-05 起删除 login 的 `InternalAuth.ForceEnforce`，dev/test 与生产同一条强制路径。

## 8. 两版状态

- C++/Go 版：本文全部内容。
- Java 版（`D:\luyuan\xuanming-server-mmo`）：入场时 `SessionContext` 使用 `ownerAccount` 已修（Java 83d57c8f）；在线域是否只认 gateway 绑定的原账号、gate 重启后新连接只能重新登录，**待核对**，已在 Java 仓 `PARITY.md` 记 TODO。Java 内部走既有 Dubbo 认证，不要求与本文签名格式相同。
- §9 第 1～3 批只改本仓服务端内部接缝，不改客户端契约；Java 版内部认证机制不同，不适用，不往 `PARITY.md` 加行。Java 在线域那条 TODO 核对时，一并查有没有「不带会话就按请求体身份执行」的入口（§7.5 的同类问题）。
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

- **共享 nonce 存储、把正文摘要签进去。** 前者每个请求多一次存储往返；后者在路由模式下要 gate 解析并确定性重编码正文，Go 侧还要拿到原始字节。需要防内网嗅探和篡改时，直接上传输层 TLS（或服务网格），一次解决正文完整性、重放与调用方认证，届时 HMAC 整体退役，不在它上面继续加层。
- **login 签发、gate 只转交的非对称身份票据。** 能防 §7.4 的持钥冒充，但要配票据过期、续签与撤销，当前不值得；上线前做安全评估时再看。
- **合并 `callerauth` 与 `serviceauth`。** 两套内部签名长期应收敛成一套，不在本方案；新服务不要再造第三套。

### 9.6 剩余（2026-11-06 记）

1. **guild 三个读方法的无会话分支**（2026-11-06 已删，未编译，验收见 [执行清单](../handoff/gate-online-identity-simplify-20261105/CODEX.md) 第 7 节）：GetGuild、GetGuildRank、GetGuildRankByGuild 原先对不带会话的调用不查归属区、沿用请求体 `zone_id`（全服榜 `zone_id=0`）、不夹页长。生产里入口准入早已拒绝这类调用，只有单测故意走这条路径。现在三个方法与四个写方法一样先过 `requireClientSession`，区一律取 `clientZone`，榜单页长夹到 `MaxRankPageSize`。随之没有调用者的两处一并收掉：`viewerOf`（只为无会话调用回 0，CreateGuild / SetAnnouncement / GetGuild 现在直接传会话里的 player_id）与 `visibleIn` 的「`zoneID==0` 不按区过滤」；`expected_guild.go` 里「只给客户端来源计 absent」的判断同理删掉（五个调用方都只受理客户端来源）。测试改成客户端来源：请求者用本区旁观者（不在被查的帮里），`guildInfoFor` 不给他现算待审数，所以不需要待审数替身，也不用为测试给 `GuildLogic` 新开接缝；「内部调用保留全服榜」改为断言无会话被拒，另加三个读方法无会话被拒的表驱动用例。运维要全服榜，另设带 `serviceauth` 的内部 RPC，不复活这条分支。
2. **guild 请求体里的身份与区字段**（2026-11-10 已删，未编译，验收见 [执行清单](../handoff/gate-online-identity-simplify-20261105/CODEX.md) 第 8 节）：CreateGuild 的 `player_id` / `zone_id`，LeaveGuild、DisbandGuild、SetAnnouncement 的 `player_id`，GetGuildRank、GetGuildRankByGuild 的 `zone_id`，服务端早已一律忽略（身份取会话、区取归属）。项目未上线，按标准做法从协议删掉并把字段号写成 `reserved`，不再给读代码的人留「也许会被采信」的误会。唯一还带 `player_id` 的客户端请求是 GetPlayerGuild，只在不带会话的内部只读调用（mail 领附件）时采信。仓库内的 robot 冒烟同步：建帮不再故意填错区、别区查榜不再伪造区、第 8 步改为按会话身份退帮。这是客户端契约变更：Unity 客户端与协议生成由 Codex 处理；Java 版本轮只在 `PARITY.md` 记 TODO（§8）。
3. **全服帮会榜**（2026-11-10 负责人定删除，当日已删，未编译，验收同上第 8 节）：负责人 2026-11-10 答复：运维 / GM 都不需要全服帮会榜，删掉；现在就做，不等第 1 条的 Codex 回执。问之前逐处核过读者：第 1 条之后全服榜 ZSET `guild_rank` 只写不读，客户端只看本区榜，`tools/merge_zone`、`tools/data_consistency_check`、合服运维手册、robot 与 Java 网关后台都只碰分区榜 `guild_rank:zone:*`，Java 版本来就不提供全服榜。留着它，每次推分多一次 ZADD，全部帮会挤在一个键上，上 Redis Cluster 就是单槽热点。删除范围：`UpdateGuildScore` 只写最终区的分区榜；`RebuildRanks` 只重建分区榜；`RemoveGuildFromRank` 只从各分区榜摘除；data 层 `GetGuildRankPage` / `GetGuildRank` 不给区直接回 `ErrRankZoneRequired`，不再有「`zoneID=0` 读全服榜」的口径。`zone_id=0` 的帮会行是坏数据（帮会都按归属区建），分数照常落库、不进任何榜并打错误日志。三份[帮会排行架构](guild_ranking_architecture_zh.md)文档已按只有分区榜重写。旧环境里残留的 `guild_rank` 键不再有人读写，`RebuildRanks` 只扫 `guild_rank:zone:*`，不会顺手删它；项目未上线，不写迁移代码：压测前清库（`dev_tools.ps1 -Command redis-wipe-local` 或 `-Command k8s-redis-wipe`，都要带 `-ConfirmWipeAllRedis`）时一并清掉，只删这一个键用 `redis-cli -h <guild 的 RedisClient 地址> -n 0 DEL guild_rank`。

# Gate 在线身份：login 认证、gate 持有、签名传递、提交点复核

日期：2026-11-05（美东）。状态：已落码。C++/Go 版链路由 Codex 编译与真连接验收（见 [审核交接](../handoff/gate-auth-role-fence-review-20261104.md)）；同日删除在线域的 login Redis 回退，未编译，待 Codex 按 [执行清单](../handoff/gate-online-identity-single-source-20261105/CODEX.md) 验证。Java 版待做（§8）。

## 1. 分工

一句话：**login 验凭据；gate 记住验过的结果；下游只认 gate 签名的原身份，不再为每条消息查登录态；业务在提交点按原件复核角色归属。**

| 环节 | 负责方 | 做什么 | 不做什么 |
|---|---|---|---|
| 凭据认证 | login `Login` | 验 access token / 口令，按 `(gate_instance_id, session_incarnation)` 写本连接的 `login_session`（登录域 Redis） | 不信 gate 自报账号 |
| 选角入场 | login `EnterGame` | 从本连接 `login_session` 取账号；SQL 账号目录核「角色归属该账号」并取 `owner_revision`；后台链经 Kafka 发 `BindSessionEvent{player_id, login_generation, authenticated_account, session_incarnation, owner_revision}` 给该 gate 实例 | 不按 player_id 反查当前 owner 补账号 |
| 持有身份 | gate | `gate_authenticated_session::Bind` 只接受本进程 UUID 信封、同一连接代次、字段齐全、非旧登录代次、同代不冲突的绑定，写进 `SessionInfo.authenticatedAccount / authenticatedOwnerRevision`（与场景路由修订分开存） | 不从客户端正文取身份；新连接不继承旧连接 |
| 传递身份 | gate | 每个发往 Go 的请求由 `gate_session_details::Build` 从服务端会话构造 `SessionDetails`（含原账号与原修订），按 §3 签名；路由模式由 client_rpc_router 原样透传 `x-*` 头 | 路由服不重签、不改身份 |
| 验证来源 | 各 Go 服务 | `callerauth.GateInterceptor`：带会话头就必须验签，通过后把原件存进 ctx | 不接受未签名或与原件不符的声明 |
| 取原账号 | 在线域 match / chat / friend / guild / trade / mail | 只调 `roleidentity.SessionAccount` / `CaptureSession`：声明须与验签原件逐字段相同，且带角色、原修订、合法账号 | **不读 login Redis**，没有回退路径 |
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
- 窗口：时间偏移 ±30s；nonce 进程内两代轮转，保留 2×窗口；先验签再记 nonce，未认证流量占不了 nonce 表。
- 每次发送现签：排队请求在出队发送时才签，时间戳是发送时刻。

## 4. 密钥与配置

- 同一把内部身份密钥 `MMORPG_INTERNAL_AUTH_SECRET`（≥32 字节）注入 gate、login 与全部在线 Go 服务；gate 与 `GateInterceptor` 缺钥或不足 32 字节拒启。
- 轮换：先把新钥以 `MMORPG_INTERNAL_AUTH_SECRET_ADDITIONAL_ENVS` 列出的环境变量分发给验证方（只验不签），全部就位后切 gate 的签名钥，再撤旧钥。
- K8s 由 Secret `gate-identity-auth`（键 `secret`）注入；本机启动器沿用 `go/login/etc/login.yaml` 的开发值，非 dev/test 必须外部注入。不进 ConfigMap、不进日志。
- 在线域服务（chat / friend / guild / match / mail / trade）**没有** `GlobalLoginSessions`、`LoginRedis` 配置，也不持有 login Redis 连接。

## 5. 可观测性

- `caller_auth_session_verify_total{result}`：在线域验签结果，result 取 `callerauth.Reason` 的有限枚举。稳态 `result!="ok"` 应恒 0；非 0 先查密钥是否同源、时钟偏移、签名头是否被改写。
- `caller_auth_nonce_overflow_total`：nonce 表超上限提前翻代（只缩短重放窗口，不放行验签失败的请求）。
- login 自己的 `login_caller_auth_total{result,mode}` 与 `login_caller_auth_nonce_overflow_total` 不变。

## 6. 水平扩展

- 验签是本进程 CPU（一次 SHA-256 + 一次 HMAC），不访问任何存储；在线服务加副本不增加 login Redis 的连接数，也不给登录域加读负载。
- 路由服无状态透传；gate 只持本进程连接的身份，扩 gate 不需要共享会话存储。
- 角色归属的强一致由各逻辑库自己的 `role_fence` 提交锁保证，不依赖跨库 SQL 或集中目录查询。

## 7. 已知边界（不宣称已解决）

1. **跨副本重放与正文**：nonce 表是进程内的，同一条签名在 ±30s 内可重放到同一服务的另一副本；HMAC 绑定身份与方法、不覆盖请求正文，能嗅探内网流量的一方可在窗口内换正文。现状依赖内网隔离（服务端口不对公网暴露）。彻底闭合需要传输层 mTLS 或正文摘要 + 共享 nonce，需单独立项并做压测对比。
2. **撤销不是跨服务原子的**：踢线 / 离场之前已经受理的请求按各业务契约完成或拒绝；冻结、过户的正确性由提交点护栏保证，不靠异步踢线消息。
3. **设备软上限**：`login_session` 是绝对 TTL，没有在线续期；在线超过 TTL 的连接不再计入 `MaxDevicesPerAccount`。只影响软上限，不影响身份与资产。

login 与在线域都没有「验签失败告警放行」的宽松档：2026-11-05 起删除 login 的 `InternalAuth.ForceEnforce`，dev/test 与生产同一条强制路径。

## 8. 两版状态

- C++/Go 版：本文全部内容。
- Java 版（`D:\luyuan\xuanming-server-mmo`）：入场时 `SessionContext` 使用 `ownerAccount` 已修（Java 83d57c8f）；在线域是否只认 gateway 绑定的原账号、gate 重启后新连接只能重新登录，**待核对**，已在 Java 仓 `PARITY.md` 记 TODO。Java 内部走既有 Dubbo 认证，不要求与本文签名格式相同。

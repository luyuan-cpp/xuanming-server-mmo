# Dual Token Authentication Design (Access + Refresh)

**Date:** 2026-04-20
**Status:** Implemented

## Problem

1. Gate connection token (HMAC) TTL was 60s in Java Gateway, causing `token expired` errors when client connects slightly late.
2. After login via third-party (WeChat/QQ/SA-Token), there's no persistent auth credential. If the player disconnects, they must redo the full OAuth flow.
3. No "remember me" capability -- typical WeChat/QQ games allow silent re-login for 30 days.

## Architecture

### Three-Layer Token System

```
Layer 1: Third-Party Auth (WeChat/QQ/SA-Token/Password)
  |
  | (first login only, validates once)
  v
Layer 2: Access Token (2h) + Refresh Token (30d)   <-- NEW
  |
  | (issued after successful auth, stored in Redis)
  v
Layer 3: Gate Token (HMAC, 300s)
  |
  | (one-time connection handshake, unchanged)
  v
  TCP Session (Gate)
```

### Token Comparison

| Token | Type | TTL | Purpose | Storage |
|-------|------|-----|---------|---------|
| Third-party token | OAuth code / SA-Token | Varies | Initial auth | Third-party Redis |
| **Access Token** | Opaque (base64url, 256-bit) | **2 hours** | Reconnect without re-auth | Redis: `access_token:{value}` |
| **Refresh Token** | Opaque (base64url, 256-bit) | **30 days** | Silent access token renewal | Redis: `refresh_token:{value}` |
| Gate Token | HMAC-SHA256 signed protobuf | 300 seconds | One-time gate TCP handshake | Not stored (verified by signature) |

## Flow Diagrams

### First Login (WeChat Example)

```
Client                  Java Gateway           Go Login            Redis
  |                         |                     |                  |
  |-- POST /assign-gate --->|                     |                  |
  |<-- gate_ip + HMAC token |                     |                  |
  |                         |                     |                  |
  |== TCP connect to Gate ==========================|                |
  |-- ClientTokenVerify --->|                     |                  |
  |<-- verified             |                     |                  |
  |                         |                     |                  |
  |-- Login(auth_type:"wechat", auth_token:code) -->|               |
  |                         |      WeChat OAuth API validates code   |
  |                         |                     |-- SET access_token:xxx ->|
  |                         |                     |-- SET refresh_token:yyy ->|
  |<-- LoginResponse{players, access_token, refresh_token} ---------|
```

### Reconnect (Within 2h)

```
Client                         Go Login            Redis
  |                               |                  |
  |-- Login(auth_type:"access_token", auth_token:xxx) -->|
  |                               |-- GET access_token:xxx ->|
  |                               |<-- {account, auth_type}   |
  |<-- LoginResponse{players} ----|                  |
```

### Token Refresh (Access Expired, Refresh Valid)

```
Client                         Go Login            Redis
  |                               |                  |
  |-- RefreshToken(refresh_token:yyy) ->|            |
  |                               |-- GET refresh_token:yyy ->|
  |                               |<-- {account, auth_type}   |
  |                               |-- DEL refresh_token:yyy -->|  (one-time use)
  |                               |-- SET access_token:xxx2 -->|
  |                               |-- SET refresh_token:yyy2 ->|
  |<-- RefreshTokenResponse{new_access, new_refresh} |
  |                               |                  |
  |-- Login(auth_type:"access_token", auth_token:xxx2) -->|
```

### Full Re-Login (Both Tokens Expired, After 30 Days)

```
Client must redo WeChat/QQ OAuth flow from scratch.
```

## Redis Key Schema

| Key Pattern | Value | TTL | Purpose |
|-------------|-------|-----|---------|
| `access_token:{<tag>}:<secret>` | JSON `{account, auth_type, device_id, created_at, family_id}` | 2h | Access token lookup |
| `refresh_token:{<tag>}:<secret>` | JSON `{account, auth_type, device_id, created_at, family_id}` | 30d | Refresh token lookup |
| `refresh_family:{<tag>}:<id>` | HASH `{account, head, prev, created_at, rotated_at}` | 30d | Family revocation |
| `refresh_rotated:{<tag>}:<旧 secret>` | HASH `{access, refresh, access_expire, refresh_expire, rotated_at}` | `RefreshReuseGrace` | Same-pair replay inside the grace window |
| `account_refresh:{<tag>}:<account>` | ZSET of refresh secrets, score = expiry | 30d | Bulk revocation (password change) |

2026-10-31 起令牌字符串是 `<tag>.<secret>`,`<tag>` 是账号的 `AccountTag`,一个账号的键全部同槽,见 [redis-player-store-cluster.md](./redis-player-store-cluster.md) §10.2。对客户端仍是不透明串。

## Security Properties

1. **Token rotation**: Each refresh issues a new pair and invalidates the old refresh token. A retry inside the reuse grace window gets the same pair; reuse outside it revokes the whole family (see below).
2. **Race protection**: Rotation is one atomic script that compares the old value. Of two concurrent refreshes one rotates; the other reads the rotation record and gets the same pair.
3. **Bulk revocation**: `RevokeAll(account)` deletes all refresh tokens for an account (e.g. on password change).
4. **Opaque tokens**: 256-bit crypto/rand, base64url encoded. Not guessable, not decodable.
5. **Gate token unchanged**: HMAC-SHA256 gate handshake is orthogonal to auth tokens.

## 轮换重试的宽限窗口(2026-10-15)

2026-10-15 设计,两版同批。Go 版最初的 WATCH 事务实现没有落地(写在旧基线上,与主干后来的 Lua 轮换冲突);2026-11-01 随令牌家族一起按 Lua 重写,落到主线的账号单槽布局上,语义不变。

### 问题

改动前 refresh token 严格一次性。回包在路上丢失时(gate lane 回 1003、gRPC `DEADLINE_EXCEEDED`、断线),服务端其实已经完成轮换,客户端却没拿到新的一对。它手里只有旧 token,拿它重试必然得到 2000,只能走完整的第三方重登。见 [client-request-ordering.md](client-request-ordering.md) §7.2 第 5 条。Java 网关也因此对 RefreshToken 关掉了自动重试(`LoginRpcClient.refreshToken`)。

### 语义

1. **轮换是一个原子步**:作废旧 refresh、写新的一对、推进家族记录、写轮换记录 `refresh_rotated:{<tag>}:<旧 secret>`(TTL = `RefreshReuseGrace`),是同一段 Lua,全部在账号的 slot 里。
2. **旧 token 不在时转重放判定**:
   - 没有轮换记录(从未存在、过期、已过窗口),或记录在读出之后、核对之前刚好过期 → 交给重用检测(见下一节);
   - 有记录,且它换出的新 refresh 仍在、家族仍在 → **原样返回同一对**:不签发,不续期,过期时间也是原来的;
   - 有记录、家族还活着,但新 refresh 已被轮换走(家族头已不是它)→ **按重用处理**:拿到新 refresh 的一方已经用过它,合法客户端丢了回包时不会走到这一步,说明有两方在用同一条链。这也堵住了「攻击者在窗口内连续轮换两次,合法方的重放就只得到普通拒绝」的绕过;
   - 有记录,但新 refresh 的键已过期或被外部删除而家族头仍指着它,或家族已吊销 → 普通拒绝;
   - 记录的剩余 TTL 被 `PERSIST` 或超过 60s 硬上限 → 按损坏拒绝(不能借它延长窗口);落在(当前窗口, 60s] 之间(窗口刚调小后的过渡期)→ 普通拒绝。
3. **并发两次**:输的一路走第 2 步,读到赢的一路在同一原子步里写下的记录,两路拿到同一对,只签发一次。
4. **窗口为 0**:不写记录,严格一次性。此时同一请求的第二份(包括客户端库对读超时的原样重发)就是窗口外重用,会触发吊销;所以窗口 0 不要与吊销同时开(配置注释与启动日志都有提示)。
5. **判定与吊销是一个原子步**:一段 Lua 同时核对轮换记录的剩余 TTL、新 refresh 与家族记录(含家族头),判为重用就在同一段脚本里吊销。
6. **窗口取整毫秒**:脚本按毫秒算。亚毫秒的值(包括 yaml 里漏写单位、被解析成纳秒的数字)会让服务以为窗口开着,而脚本按 0 处理、不写记录,实际变成窗口 0 加吊销,所以两版都在启动时拒绝。

返回的错误不含令牌值。业务拒绝是 `token.ErrRefreshRejected`(Java 返回空),`refreshtokenlogic` 记 Info;存储故障与数据损坏记 Error。客户端看到的码没有变:成功回新的一对,拒绝与存储故障都回 2000。

### 默认 30s、上限 60s 的依据

窗口从服务端提交轮换的那一刻开始计时。要覆盖的是「客户端等到超时 → 重连 → 重发」这一整段:

- gate lane 排队超过 5s 回 1003(O6);
- Java 网关调 login 的超时是 3s(`login.grpc.timeout-ms`);
- 客户端自己的请求超时与重连耗时由独立客户端仓定义,本次没有核实。

30s 能覆盖一次超时加一次重连重发,与 Okta 的默认值相同,比 Supabase 的 10s 宽。上限 60s 取 Okta 可配范围的上界:窗口越长,偷到旧 token 的人能搭车的时间越长,所以给硬上限,越界拒绝启动,不静默截断。

### 盗用风险

威胁模型:攻击者拿到了某枚 refresh token R,来源可能是设备备份、日志、恶意软件等。

| 场景 | 严格一次性 | 宽限窗口 + 家族吊销 |
|---|---|---|
| 攻击者先用 R | 攻击者得到新链,合法客户端下次刷新被拒;攻击者的链可以一直续到 30 天 | 合法客户端在窗口外拿 R 来刷新时触发重用检测,攻击者那条链连同 access 一起作废;在窗口内且攻击者还没再轮换,双方拿到同一对(下一行);攻击者已再轮换过,窗口内也判为重用、吊销 |
| 合法客户端先用 R,攻击者在窗口内重放 R | 拒绝 | **新增风险**:合法客户端还没用新 refresh 时,攻击者拿到与它**同一对**令牌;合法客户端已用过新 refresh 时判为重用、吊销 |
| 合法客户端先用 R,攻击者在窗口外重放 R | 拒绝 | 拒绝,并吊销该家族:合法用户这条登录线被踢(见下一节「误伤分析」) |
| 改密 `RevokeAll` 之后在窗口内重放 R | 拒绝 | 拒绝(家族与新 refresh 都已删除) |

窗口内的新增风险有边界:窗口短且有硬上限,可配成 0;只返回同一对、不另签,不会分叉出第二条链;后继一旦被轮换走,窗口内的重放就判为重用、吊销家族,家族被吊销后窗口立即失效;access 寿命不延长;`refresh_rotated:` 里的明文令牌本来就以明文出现在令牌键名里,TTL ≤ 60s。Auth0 在窗口内会**另签**一枚新 token,我们没有选这种做法。Supabase 被报告过宽限路径在返回子令牌前没有复核会话状态,我们的复核点是「新 refresh 仍在、家族仍在」。

**做不到的绑定**:`RefreshTokenRequest` 只有 `refresh_token` 一个字段,没有设备或会话标识,`TokenData.device_id` 恒为空。重试往往发生在重连之后的新连接上,按连接绑定恰好会误伤这个功能要救的场景。要绑设备得改客户端契约(两版加客户端同批改),列为后续。

### 业界对照

| 方案 | 窗口 | 窗口内重放旧 token | 窗口外重放 |
|---|---|---|---|
| RFC 9700(OAuth 2.0 Security BCP)§4.14.2 | 不讨论宽限 | — | 服务端分不清谁是攻击者,吊销仍活跃的 refresh(整个家族) |
| Auth0 Refresh Token Rotation「Reuse Interval」 | 可配(秒) | 不触发重用检测,**另签**一枚新的 rotating refresh token | 吊销整个 token family,用户须重新认证 |
| Okta「Grace period for token rotation」 | 默认 30s,可配 0–60s | 旧 token 在窗口内仍可用 | 立即作废最近签发的 refresh,以及自用户认证以来签发的全部 access |
| Supabase Auth(GoTrue)reuse interval | 默认 10s | 返回当前活跃的子 refresh token(同一枚) | 整个会话视为终止,其全部 refresh 吊销 |
| **本项目** | 默认 30s,可配 0–60s | 原样返回同一对;新 refresh 已被轮换走则按重用吊销家族;家族已吊销则拒绝 | 吊销整个家族(可切到只观测) |

来源:[RFC 9700 §4.14](https://www.rfc-editor.org/rfc/rfc9700#section-4.14)、[Auth0 Refresh Token Rotation](https://auth0.com/docs/secure/tokens/refresh-tokens/refresh-token-rotation)、[Okta Refresh token rotation](https://developer.okta.com/docs/guides/refresh-tokens/refresh-token-rotation/)、[Supabase User sessions](https://supabase.com/docs/guides/auth/sessions)(2026-10-15 查阅)。

### 仍未解决

- **存储故障仍回 2000**:Redis 不可达时,轮换的结果可能未知(脚本已发出、回包丢失),客户端却会去重登而不是重试。改成可重试的码属于客户端契约变更,需要两版加客户端同批改,本次没有动。Java 截止放弃回 1003 的行为不变。
- **客户端行为**:收到 1003、超时等结果未知的情况,应拿原 token 在窗口内重试;只有收到 2000 才走完整重登;重试预算用完仍不知道结果,就丢掉这枚 refresh,绝不在窗口之外再发它(那会被当成重用、吊销整次登录)。Go 机器人(`robot/login.go` 的 `runTokenRefresher`)已按这三条改:15s 预算内最多 3 次、退避 1s / 2s,被拒或预算用完即丢弃。真实客户端在独立仓库,本次没有改,也没有核实。
- **网关仍不自动重试**:窗口可能被配成 0,那时网关重试就是一次窗口外重用,会把刚签出的那一对连同整条线一起吊销。所以重试留给客户端,本仓 `LoginRpcClient` 只更新了注释。

## 令牌家族、重用检测与家族吊销(2026-11-01 定稿)

**状态:已实现,两版同批(AGENTS.md §12),未编译、未测试,待 Codex 验证。**

**拍板记录**:

- 2026-10-22 负责人批准:家族号嵌进令牌 + 家族记录,开关缺省吊销;Go 宽限窗口按 Lua 重写。
- 2026-10-23 负责人补充:项目未上线,玩家数据可以完全清空,要求数据强一致、代码易读、可水平扩展,删掉旧令牌兼容分支。
- 10-23~30 的 Go 实现按家族号做 hash tag,写在旧基线上,一直停在 worktree(`relaxed-cerf-d5da4d`),没进主线。其间 Redis 集群化第 11 批把令牌键改成账号 tag 单槽。2026-11-01 把同样的语义落到主线布局上,比旧补丁简单:
  - 令牌是 `<AccountTag>.<secret>`,secret 的前 16 字节是家族号。账号 tag 决定槽,家族号用来找家族记录,两件事分开。
  - 账号索引和家族同槽,签发、轮换、吊销都是一段脚本。旧补丁里的撤回签发、登记后复核、吊销后摘索引都不需要了。
  - 判定和吊销在同一段脚本里,没有「判定完、吊销前家族被轮换」的窗口,也没有「判定为重用却吊销失败」这个结局。脚本发不出去时按存储故障计 `error`,在 3 秒独立预算内最多重试 3 次,不随请求截止放弃。
  - Java 版的令牌布局不变(家族号 hash tag)。两版不共享 Redis,客户端把令牌当不透明串,没有兼容问题。

### 要解决的

上一节盗用风险表里「攻击者先用 R」那一行:攻击者拿偷到的 R 抢先轮换,得到一条能一直续下去的链。合法客户端稍后拿 R 来刷新,严格一次性下只是这一次被拒,攻击者那条链照样有效。RFC 9700 §4.14.2 的做法是:同一枚已轮换的 refresh 再次出现,服务端分不清两边谁是合法的,于是把这条链上仍有效的令牌全部吊销。

### 术语

- **家族(family)**:一次完整认证签出的那一对令牌(口令或第三方登录走 `Issue`),加上由它逐次轮换出来的全部后继。一台设备的一次登录就是一个家族,同一账号的多台设备是多个家族。用 access token 重登(`AccessTokenProvider`)不签新令牌,不新开家族。
- **家族头(head)**:家族里当前唯一还活着的那枚 refresh token。

### 为什么不用墓碑

另一种做法是给每枚被轮换的旧 token 留一条墓碑,存活一个检测期,每账号封顶。它的三个问题:检测期要另选,而且内存随轮换次数线性增长(常驻在线时一个家族一周约 84 条);偷到 R 的攻击者自己高频轮换,就能把 R 的墓碑挤出封顶,之后再也检测不到;吊销时还得扫账号下所有 refresh 找同家族的那枚。

把家族号嵌进令牌之后,任何旧 token 都能从自身找到家族记录。检测期就是家族的整个寿命,内存只与活家族数有关,没有可以挤掉的东西。代价是知道家族号的人能拼出「家族号 + 任意」去触发吊销。家族号只有持有过该家族某枚令牌的人才知道,而这种人直接重放手里的旧 token 也能做到同样的事,所以没有新增能力。家族号原值因此不进日志。

### 令牌格式与键布局

- 令牌是 `<AccountTag>.<secret>`。secret 是 base64url_raw(家族号 16 字节 ‖ 随机 16 字节),43 字符;随机半段首字节的最高位是类型位(0 = refresh,1 = access),剩下 127 位随机。只认规范编码(重新编码必须与原串一致),类型位与用途不符也不碰 Redis,直接普通拒绝。
- 家族号字符串是 base64url_raw(家族号),22 字符。
- 一个账号的令牌键、家族记录、轮换记录、账号索引都以 `{<tag>}` 作 hash tag,同在一个 Redis Cluster slot。脚本按「前缀 + secret」现拼的键带同一个 tag。

### 不变量

1. **家族记录是唯一的真相源**。记录不在,就是家族已吊销或已结束:该家族的 refresh 不能轮换,宽限重放不返回,access 不能登录。
2. **每个家族恰有一枚活 refresh,就是记录里的 head;只有 head 能轮换**。
3. **令牌与家族记录都带正 TTL**,家族记录与它的头同一步写入,每次轮换续期。
4. **损坏一律 fail-closed,拒绝时一个字节都不写**。Lua 运行期错误不会回滚已执行的写,所以每段脚本把全部检查放在第一次写之前。

### 各操作

| 操作 | 做法 |
|---|---|
| `Issue` | 一段 Lua 写两枚令牌、家族记录,登记账号索引。三个新键已存在时:内容与本次一致(驱动对读超时的原样重发)按已写入返回,否则报撞键、不覆盖。提交后 best-effort 封顶。 |
| `ValidateAccess` | 解析令牌,类型必须是 access;读 payload,核账号 tag 和家族号;一段只读 Lua 核 access、家族记录、head、账号索引。家族不在即已吊销或已结束。 |
| `Refresh` | 解析令牌,类型必须是 refresh。键在:按值比对后一段 Lua 完成轮换。键不在,或比对落空:走重放判定。 |
| 重放判定 | 一段 Lua:家族不在普通拒绝;头就是这枚(刚过期或被外部删除)普通拒绝;有轮换记录且后继就是头,返回同一对;其余按重用处理,吊销或只观测。 |
| 吊销 | 重放判定脚本内:删家族记录,删头的键,把头从账号索引摘掉。access 和其余 refresh 因家族不在立即失效,键到 TTL 自然消失。 |
| `RevokeAll`、封顶 | `retireRefreshScript`,一段 Lua 淘汰账号索引里最旧的成员及其家族。 |

### 开关

Go `TokenConfig.RefreshReuseRevoke`、Java `game.login.refresh-reuse-revoke`,缺省 `true`。设为 `false` 是只观测:照常检测、记指标与日志,但不吊销,客户端仍被拒绝。上线后误伤若超出预期,不必回滚代码就能退回观测。家族记录的读写与 access 校验不受开关影响。

Go 的 `TokenConfig` 块整块可以不写:它不标 `optional`、每个字段都带 default,go-zero 对整块缺失的非 optional 结构体照样填默认值(K8s 由 `k8s_deploy.ps1` 生成的 login ConfigMap 就不写这个块)。`token_conf_test.go` 钉住这条:标了 `optional` 时整块缺失会得到全零配置,TTL 为 0 让启动校验拒绝启动,开关为 false 会让重用检测静默退成只观测。

### 误伤分析

| 场景 | 严格一次性 | 本设计 |
|---|---|---|
| 回包丢失,在窗口内重试 | 拒绝,重登 | 返回同一对 |
| 移动端挂起超过窗口,醒来拿旧 R 重试 | 拒绝,重登 | 拒绝,重登。同时该家族被吊销,但它的头只在那份丢掉的回包里,没人持有,**实际损失相同** |
| 轮换成功后,客户端没来得及持久化新令牌就崩溃,重启后拿旧 R | 拒绝,重登 | 窗口内返回同一对;窗口外与左边相同 |
| 客户端 bug:同一设备的两个组件各持一份令牌,其中一份用的是已轮换的旧 R;或重复请求迟到超过窗口、或迟到到新 refresh 已被用过之后 | 旧的那份被拒,另一份继续可用 | **整个家族被吊销,持新令牌的那份也掉线**。这是真正新增的误伤,只来自客户端 bug,`replayed=predecessor` 的计数能看到 |
| 客户端把 access 当 refresh 发来(字段写反),或 access 已过期 | 拒绝 | 普通拒绝,不吊销(类型位在碰 Redis 之前就挡住) |
| 客户端发来的令牌夹带换行或多余字符 | 拒绝 | 普通拒绝,不吊销(只认规范编码) |
| 窗口为 0 时同一请求并发两次 | 一个成功,一个拒绝 | 赢的一路刚拿到的那一对随之作废(见上一节第 4 条) |
| 偷到**已轮换**旧 token 的人去重放 | 拒绝,合法用户无感 | 合法用户的这条登录线被踢,小偷等于拿到了「踢下线」的能力。这是 RFC 9700 接受的代价:服务端分不清谁是合法的一方 |
| 同账号的其他设备 | — | **不受影响**:家族按登录划分,不按账号 |
| 已进游戏的连接 | — | 不踢。吊销只让令牌失效,已建立的 gate 会话持续到断线,下次刷新或 access 重登时才失效 |

**不做账号级连坐**:偷到一台设备令牌存储的人只拿到那一个家族;连坐的话,任一台设备的客户端 bug 都会把所有设备踢下线。Okta 吊销的是「自用户这次认证以来签发的令牌」,与家族同义。

### 指标与日志

Go 的 `login_refresh_token_total{result}` 取值:`rotated`、`replayed`、`rejected`(含格式 / 类型不对、记录比当前窗口长)、`rejected_successor_gone`(窗口内重放而新 refresh 已过期或被外部删除)、`rejected_family_gone`(窗口内重放,而家族已被吊销。家族头的持有者撞上吊销时键已删、也没有轮换记录,计 `rejected`,所以这一项不能用来数受害者)、`reuse_revoked`、`reuse_observed`(开关关着)、`abandoned`(提交前已截止)、`error`(存储故障或数据损坏;重放判定发不出去时这次重用检测就漏了,**持续 > 0 要告警**)。

直方图 `login_refresh_reuse_delay_seconds{replayed}`:label 取 `predecessor`(重放的是头的前一枚)或 `older`;值 = now − 家族的 `rotated_at`。对 `predecessor` 它恰好是被重放那枚距它被轮换的时间,可以据此判断宽限窗口是否偏短;对 `older` 只是下界。桶:1、5、10、20、30、45、60s(覆盖窗口的可配范围),2m、5m、15m、1h、6h、1d、7d、30d。

label 全是低基数,不带账号、不带 `player_id`。日志:检测到重用记 Error(go-zero 的 logx 没有 Warn;Java 记 warn),字段有 `account`、`family`(SHA-256(family_id) 的前 8 位 hex,只用来串联同一家族的几行)、`replayed`、`delay`、`family_age`、`revoked`;`rejected_family_gone` 与 access 因家族吊销被拒记 Info。不记令牌,也不记家族号原值(测试捕获日志逐项核对)。Java 不加指标,只记同样字段的日志,与它的宽限窗口实现一致。

### 上线与回滚

- 项目尚未上线(见 [存盘围栏设计](db-write-fencing-mysql.md) 文末「上线状态」),负责人确认玩家数据可以完全清空,所以没有新旧令牌并存窗口、没有兼容分支。部署前清掉令牌族的键(`access_token:*`、`refresh_token:*`、`refresh_family:*`、`refresh_rotated:*`、`account_refresh:*`;部署与压测脚本的清理列表已补上)。不清也不出错:旧格式的键不会再被读到,到 TTL 自然过期,持有旧令牌的客户端重登一次。
- 回滚到 Codex 版或更早的版本:键名不同,所有令牌失效,玩家重登一次;没有数据迁移。
- 上线之后若要改令牌键名或格式,按 [shared-redis-split-by-purpose.md](shared-redis-split-by-purpose.md) §20.7 另行设计停写切换。
- Java 版与本仓不共享 Redis,两版之间没有兼容问题。

### 不做 / 仍未解决

- 已进游戏的连接不踢。要踢可以接现有的顶号 / kick 路径,另立任务。
- 不按设备绑定:请求不带设备标识,理由见上一节「做不到的绑定」。
- 不做账号级连坐,理由见上文。

### 测试

`go/login/internal/logic/pkg/token/`:单测覆盖宽限重放、并发、窗口内外的重用、只观测、类型位、规范编码、损坏拒绝、封顶与撤销;`TestTokenFamily_RealRedis` 在设置 `LOGIN_TOKEN_REQUIRE_REAL_REDIS=1`、`LOGIN_TOKEN_TEST_REDIS_ADDR`、`LOGIN_TOKEN_TEST_REDIS_DB` 时跑真 Redis,7.2 与 8.x 各一轮。

## Configuration

### Go Login (`go/login/etc/login.yaml`)

```yaml
TokenConfig:
  AccessTokenTTL: 2h        # Access token lifetime
  RefreshTokenTTL: 720h     # Refresh token lifetime (30 days);不得短于 AccessTokenTTL
  RefreshReuseGrace: 30s    # 旧 refresh 轮换后的重放宽限窗口;0 = 严格一次性;上限 60s、整毫秒,越界拒绝启动
  RefreshReuseRevoke: true  # 重用(窗口外,或窗口内而后继已被轮换走)即吊销该登录的令牌家族;false = 只观测
```

整块可以不写,每个字段都有同样的默认值(见「开关」)。

### Java Gateway (`application.yaml`)

```yaml
gate:
  token-ttl-seconds: 300   # Gate handshake token (was 60, now matches Go Login)
```

## Files Changed

| File | Change |
|------|--------|
| `proto/login/login.proto` | Added `access_token`, `refresh_token` fields to `LoginResponse`; added `RefreshTokenRequest/Response` messages and `RefreshToken` RPC |
| `go/login/internal/logic/pkg/token/token.go` | **NEW** - Token manager: Issue, ValidateAccess, Refresh, RevokeAll |
| `go/login/internal/logic/pkg/auth/providers.go` | Added `AccessTokenProvider` for `auth_type:"access_token"` |
| `go/login/internal/logic/clientplayerlogin/loginlogic.go` | Issue tokens on successful login (skip for access_token re-auth) |
| `go/login/internal/logic/clientplayerlogin/refreshtokenlogic.go` | **NEW** - RefreshToken RPC handler |
| `go/login/internal/server/clientplayerlogin/clientplayerloginserver.go` | Added `RefreshToken` method |
| `go/login/internal/config/config.go` | Added `TokenConf` struct |
| `go/login/internal/svc/servicecontext.go` | Added `TokenManager` to ServiceContext |
| `go/login/internal/svc/auth_init.go` | Register `access_token` provider |
| `go/login/etc/login.yaml` | Added `TokenConfig` section |
| `java/gateway_node/.../application.yaml` | `token-ttl-seconds: 60` → `300` |
| `robot/pkg/client.go` | Added `AccessToken`, `RefreshToken` fields |
| `robot/login.go` | Store received tokens for reconnect |

## Industry Reference (WeChat/QQ Games)

| Platform | Access Token TTL | Refresh Token TTL | Notes |
|----------|-----------------|-------------------|-------|
| WeChat Mini Game | 2h (session_key) | 30d (via wx.login) | session_key expires, wx.login silently renews |
| QQ Connect | 2h | 30d | Standard OAuth2 |
| Apple Game Center | 48h | 60d | Longer due to platform constraints |
| Google Play Games | 1h | Indefinite (revocable) | Uses Google OAuth2 |
| Our implementation | **2h** | **30d** | Matches WeChat/QQ standard |


## 2026-10-23：Go 与 Java 的 refresh family 契约

> 2026-11-01 更正:本节 Go「严格一次性、不引入宽限重放」和「旧 JSON 兼容」两部分已被上面的「轮换重试的宽限窗口」「令牌家族、重用检测与家族吊销」取代,其余防损坏口径仍然适用。原文保留。

Go `token.Manager` 保持 `Issue / ValidateAccess / Refresh / RevokeAll` 四个原接口。新认证生成独立 family，ID 为 16 个加密随机字节的规范 base64url（22 字符），不得以账号、设备或常量充当 family。Go 的 access/refresh JSON 新增 `family_id`，另有 `refresh_family:{id}` HASH，保存 `account / head / created_at / rotated_at`。family 的 TTL 与当前 head refresh 相同，正常轮换保持 family，更新 head 并续 TTL；不能通过读取 access 延长有效期。

签发以单段 Lua 同时提交两枚 token、family 与账号 ZSET。刷新先读旧 JSON，提交时原子核对其原始字节、账号索引成员、family 账号及当前 head，再同时消费旧 refresh、签发新 pair、推进 head 和 TTL。账号索引不缩短尚存的更长 TTL。Go 保持严格一次性：并发刷新只有一个提交者，已消费的 refresh 不能重放；提交成功但回包丢失仍须按现有客户端契约重新认证，不在 Go 引入 Java 的宽限重放。

含 family 的 access 必须核 family 存在且账号一致，并确认当前 head 仍属于该 family、账号及索引。正常轮换前签发且未过期的同 family access 继续有效；family 被撤销或到期后全部拒绝。缺失、跨账号、错误类型、非法 ID、损坏 head 或原始值被替换均 fail-closed。

旧 JSON **缺少** family 字段时，access 仍按原 TTL 兼容；旧 refresh 在下一次成功的原值 CAS 内建立随机 family。显式空串、null 或非法 family 不视为旧 JSON，不以兼容构造掩盖缺失实现。旧无 family access 没有可撤销关联，因此不能宣称它已被新 family 的撤销追溯失效；它只会按原 TTL 到期。

账号撤销与容量淘汰均在单段 Lua 中核验完整候选后提交。Redis Lua 的运行时错误不会回滚先前写入，故所有会影响后续写入的键类型及必要归属核验必须在第一次副作用前完成。只删除目标账号的 refresh；仅当 family 的当前 head 仍等于被淘汰成员时删除该 family，不能用旧候选误删已经轮换的新 head。并发新认证只能在线性化的撤销前或后提交，不能因分步删索引使新 family 失去归属。

两语言对齐的是随机 family、账号归属、head、TTL、撤销和损坏拒绝语义，Redis 键名与内部字段不要求共用。Java 保留其既有受限宽限重放及单调截止行为，Go 保留原严格一次性行为，不因此改变任一客户端的重试合同。

验证覆盖新认证、轮换与旧 access、旧 JSON 收编、family 到期、并发单赢家、跨账号/错误类型/损坏键零部分写入、淘汰旧候选及撤销与新认证交错。`TestTokenFamily_RealRedis` 为具名本机验证入口，显式设置 `LOGIN_TOKEN_REQUIRE_REAL_REDIS=1`、数值 loopback 地址 `LOGIN_TOKEN_TEST_REDIS_ADDR` 与独立 `LOGIN_TOKEN_TEST_REDIS_DB` 后执行；密码仅从 child env 读取。它使用随机账号及精确登记的键，末尾只删这些键，不 FLUSH、不 SCAN、不借原全局数据作为夹具。实现和测试已准备，实际运行结果以本轮 Codex 原件为准。

正 TTL 也是新 family 凭证的不变量。含 family 的 access、当前 head refresh 与 family HASH 被 `PERSIST` 改为永久后均拒绝；刷新和撤销在首个副作用前完成相应检查，不借轮换把损坏凭证重新续期。旧 JSON 缺字段的正常到期兼容规则保持不变。

撤销的显式 `null` 原件回归必须在上述真实 Redis 入口的 `null-retire-preserves-all-candidates` 子项执行，验证损坏候选之后也不能先删正常候选。miniredis v2.35.0 的 `gopher-json` 会把 JSON null 解成 Lua nil；[Redis 所带 CJSON](https://github.com/redis/redis/blob/7.4/deps/lua/src/lua_cjson.c) 则用独立 lightuserdata 表示 null。该用例因此使用真实后端，未跳过断言，也不为替身另造生产 JSON 解析层。空字符串回归保留在 miniredis，永久凭证在两个后端均有回归。

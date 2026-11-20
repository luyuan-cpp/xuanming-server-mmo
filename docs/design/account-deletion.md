# 账号注销与 Apple / Google 客户端登录接入

状态:2026-11-19 设计稿;同日负责人定案 §9 四项(Apple 撤销用 B、身份令牌一次性消费、冷静期 7 天、§10 三批都同意动码),按 1 → 2 → 3 批落码,进度见 `docs/PROGRESS.md` 同日条目。项目尚未上线,玩家数据可清空,不做新旧数据兼容。Java 版只在 Java 仓 `PARITY.md` 登记待做。

前置:[第三方登录](third-party-login-end-to-end-design.md) §十二(服务端 Apple / Google 身份令牌校验,2026-11-13 已落码、未编译)、[账号角色目录](account-role-authority.md)、[全层水平扩展规格](horizontal-scale-logical-db.md) §3(角色护栏与删角两阶段)、[双 Token](dual-token-authentication.md)。

## 1. 外部要求

| 来源 | 要求 | 落点 |
|---|---|---|
| App Store 审核指南 5.1.1(v);Apple「Offering account deletion in your app」 | 支持建号就要能在 App 内删号;只停用不算;删账号记录和关联的个人数据;法律要求保留的可以留,要告诉用户留什么;不是立即删完要告诉用户多久,删完给确认;用了 Sign in with Apple 的应调 REST 接口撤销用户令牌 | §4、§6、§8 |
| Apple TN3194「Handling account deletions and revoking tokens for Sign in with Apple」 | 撤销走 `/auth/revoke`,要一个有效的 refresh token 或 access token;建号时把令牌应答(含 refresh token)存下;没存令牌也要照常删号,并引导用户到 Apple 设置里手动撤销 | §8 |
| Google Play 账号删除要求(Play 管理中心帮助 13327111) | App 内有删除入口,**另外**要有不用装 App 的网页入口;关联数据要删;出于安全、防欺诈、合规可以保留部分数据,要披露 | §4.6、§6 |

2026-11-19 核对过的页面:Apple 删号指南、TN3194、Google Credential Manager 实现页、Play 删号帮助页。Apple 的 `/auth/token`、`/auth/revoke` 参考页是前端渲染,没取到正文,§8.2 的参数按 Apple 一直公布的口径写,真渠道联调时复核。

## 2. 现状

- 服务端已能验 Apple / Google 身份令牌(`auth_type` 为 `apple` / `google`,账号 `apple_<sub>` / `google_<sub>`),未编译。
- 客户端(`D:\luyuan\mmorpg-client`)只有口令登录界面。`LoginAuthTypes.Apple` / `Google` 与 `LoginCredential.ForThirdParty` 已在;没有任何原生插件(`Assets/Plugins` 下只有 protobuf 的 dll)。登录页有「游戏设置」面板(`QdaoServerSelectView.LoginUtilities.cs`),客户端常量在 `Assets/Scripts/Core/ClientSettings.cs`。
- 全仓没有账号注销。可以直接复用的部件:
  - **删角**:`data_service.RemoveAccountRoles`,两阶段(准备令牌 `RemovalToken` → 六个参与者全通过 → 墓碑);墓碑不可逆、修订号加 1;墓碑之后的收尾(`settleRemovedRole`:释放名字、删 `account_role_removal` 小表)失败由后台 `ResumeRemovals` 接着做。现在唯一的生产入口是运维摘孤儿 `LoginAdmin.RemovePlayersFromAccounts`。**删角今天不删玩家数据**:存档、好友、邮件都留着,只是角色再也进不去。
  - **试探**:协调者 `DryRun`(`PrepareRoleFreeze(dry_run=true)`),只跑参与者的挂起判定,不写。
  - **建角 / 过户的互斥点**:账号分片上 `reserveRef` 持账号行(`account_directory`)锁写 PENDING ref。
  - **令牌**:`token.Manager.RevokeAll(account)` 吊销账号的全部令牌家族。
  - **踢线**:EnterGame 顶号用的 `KickSessionOnGate`(经 Kafka 发 `KickPlayerEvent`)。
- 账号目录行只增不删,读路径依赖这一点(`AccountDirectoryStore.Read` 的注释)。本设计改成「只有注销收尾会删,而且在 ref 清空之后」,论证见 §7.3。

## 3. 客户端 Apple / Google 登录

### 3.1 iOS:Sign in with Apple

- 原生插件 `Assets/Plugins/iOS/MmorpgAppleSignIn.mm`(Objective-C,AuthenticationServices):`ASAuthorizationAppleIDProvider` 建请求,**不要 scope**(账号只用 `sub`,不收邮箱和姓名),`ASAuthorizationController` 发起;代理里取 `identityToken`、`authorizationCode`(UTF-8 字节转字符串)回调 Unity。取消、失败各回一个错误码。
- 编辑器后处理脚本给导出的 Xcode 工程加 Sign in with Apple 能力(`ProjectCapabilityManager.AddSignInWithApple`)。
- 登录:`LoginCredential.ForThirdParty("apple", identityToken)`。`authorizationCode` 登录时送不送取决于 §9 第 1 项。

### 3.2 安卓:Credential Manager 的 Sign in with Google

- 原生模块 `Assets/Plugins/Android/MmorpgGoogleSignIn.androidlib`(Unity 把 `.androidlib` 当 Gradle 子模块,依赖写在它自己的 `build.gradle`,不改工程的 Gradle 模板):`androidx.credentials:credentials`、`androidx.credentials:credentials-play-services-auth`、`com.google.android.libraries.identity.googleid:googleid`。版本用落码时的最新稳定版(Google 实现页 11-19 的示例是 1.7.0-alpha03,不用 alpha)。
- 用 Java 写,不引入 Kotlin 插件。用户点的是按钮,按 Google 文档用 `GetSignInWithGoogleOption.Builder(serverClientId)`,经 `CredentialManager.getCredentialAsync` 发起;结果是 `CustomCredential` 且类型为 `GoogleIdTokenCredential.TYPE_GOOGLE_ID_TOKEN_CREDENTIAL` 时,`GoogleIdTokenCredential.createFrom(data).getIdToken()` 取令牌。取消、没有账号、解析失败各回一个错误码。
- `serverClientId` 填服务端的 Web 类型 OAuth 客户端 ID,与 `AuthProviders.Google.ClientId` 相同;填安卓客户端 ID 的话 `aud` 对不上,登录一律被拒。它是构建常量,放 `ClientSettings.GoogleServerClientId`。
- 注销申请成功后调 `clearCredentialStateAsync`,免得下次自动选中刚注销的账号。

### 3.3 C# 桥与界面

- `Assets/Scripts/Net/PlatformSignIn.cs`:`RequestApple(onDone)`、`RequestGoogle(onDone)`。`#if UNITY_IOS` 走 `[DllImport("__Internal")]`,`#if UNITY_ANDROID` 走 `AndroidJavaClass`,其它平台(含编辑器)直接回「本平台不支持」。原生侧用 `UnitySendMessage` 发一段 JSON(`token` / `code` / `error`)给常驻接收对象;解析和错误码映射是纯函数,EditMode 测试覆盖。
- 登录页账号面板加按钮:iOS 只显示「通过 Apple 登录」(用 Apple 人机界面指南规定的样式与文案),安卓只显示「Google 登录」。拿到令牌后走现有的 `GameClient.EnterZone(zoneId, credential, …)`,进区流程不改。
- 渠道令牌不落盘、不进 `SessionModel`,重连只用服务端签发的 access / refresh token(现状不变)。网关回凭证无效(`code:401`)时重新拉起 SDK,不重发旧令牌。

### 3.4 服务端配合:身份令牌一次性消费(§9 第 2 项选它时)

`idtoken.go` 校验通过之后,在登录域 Redis 写 `SET <前缀>:<令牌 SHA-256> 1 NX PX <exp 剩余毫秒>`;已存在按凭证无效拒绝,截获的令牌在合法客户端用过之后就用不了;Redis 故障按渠道故障(fail-closed)。代价:网关把一次其实已成功的登录因超时重发时会被拒,客户端要重新拉起 SDK,这和微信 code 本来就一次性是同一个结果。

## 4. 注销流程

### 4.1 状态

账号目录行(账号分片)加四列。状态只在这一行上变:

```text
ACTIVE(0) ──申请:重新认证 + 预检通过──▶ PENDING(1) ──冷静期满、执行者认领──▶ DELETING(2) ──角色全部墓碑、ref 清空──▶ 删行(留回执)
    ▲                                        │
    └──────────── 本人撤销(冷静期内)──────────┘
```

| 列 | 含义 |
|---|---|
| `deletion_state` | 0 正常、1 冷静期、2 执行中(不可撤销) |
| `deletion_requested_ms` | 申请时刻(数据库时钟) |
| `deletion_execute_after_ms` | 冷静期满的时刻 = 申请时刻 + 冷静期 |
| `deletion_lease_until_ms` | 执行者租约,免得多个副本同时处理同一账号 |

索引 `(deletion_state, deletion_execute_after_ms)` 给执行者扫到期账号。冷静期是 data_service 的一项配置 `AccountDirectory.DeletionCoolingOff`(缺省 7 天,范围 0~30 天、整秒,0 只给测试):只在一处配,各区 login 不各配一份。

### 4.2 申请

1. 客户端「游戏设置 → 注销账号」:先展示注销说明(删什么、留什么、冷静期多久、冷静期内重新登录可以撤销),玩家勾选确认,再**重新认证**:口令账号重输口令,第三方账号重新拉起渠道 SDK 取新凭据(Apple 同时拿到 `authorizationCode`)。重新认证就是敏感操作的二次确认,不另设验证码。
2. `POST /api/account/delete`,正文与 `/api/login` 同形(`auth_type`,`auth_token` 或 `account` + `password`),Apple 多一个 `authorization_code`。网关限流沿用登录那一桶。
3. login `LoginPreGate.RequestAccountDeletion`:用登录同一个 `resolveAccount` 认证(口令配额照算)。目录里没有这个账号(从没登录过)回「账号不存在」。
4. 若 §9 第 1 项选 B:Apple 账号用 `authorization_code` 换票后立刻撤销,失败则整次申请失败(§8)。
5. data_service `RequestAccountDeletion`:
   - **预检**:按 ref 列出全部角色,逐个 `Lookup` 取护栏状态跑 `DryRun`。有角色挂着寄售(`trade_listing_id` 非 0),或任一参与者回挂起,就回挂起清单 `{player_id, blocker}`,什么都不写。blocker 是参与者现有的低基数枚举串,另加 guild 的 `guild_leader`(§6.3)。问不齐回可重试。
   - **提交**:账号分片一个事务,锁账号行:0 → 1,写申请时刻与到期时刻;已经是 1 原样回(幂等,不刷新时刻);已经是 2 回「执行中」。
6. 提交之后 login 尽力而为地 `RevokeAll(account)`,并按角色的在线会话记录 `KickSessionOnGate` 踢掉还在游戏里的其它设备。失败只记日志:执行时会再做一遍(§4.5);冷静期内进不了游戏靠 §4.3 的检查,不靠踢线。
7. 回 `{state, execute_after}`。客户端清本地令牌、退回登录页,提示「已申请注销,将于 X 完成;冷静期内重新登录可以撤销」。

预检和提交之间,玩家理论上还能制造新的挂起(比如恰好入队)。这只会让执行时那名角色暂时删不掉、执行者重试,不会删错(§7.2)。

### 4.3 冷静期内

- `/api/login` 照常成功、照常发令牌,应答多两项 `account_deletion_state`、`account_deletion_execute_after`。客户端看到 1 只弹「账号注销中,剩余 N 天」和「撤销注销」「退出」两个按钮,不往下走。
- **建角**:data_service `reserveRef` 在账号行锁里看到状态不是 0 就拒绝。它和注销提交是同一把锁,严格互斥。login 回新 tip「账号注销中」。
- **进游戏**:login 的 EnterGame 第一次 `Lookup` 带 `include_account_state`,data_service 顺带点读账号行的状态,不是 0 就拒绝(同一个 tip)。这一读和申请提交不在一个事务里,申请提交前一刻已经过了检查的 EnterGame 会进场。执行时在线的角色只会让删角放弃、执行者先踢线再重试,所以这只影响体验,不影响数据(§7.2)。
- **过户买入**:过户第 1 步同样走 `reserveRef`。买家账号状态不是 0 时,目录回与「买家名额满」同一类的确定性拒绝(`ResourceExhausted`),trade 走现有的先取消再退款出口(`ErrBuyerFull`),不另开分支。
- 冷静期内不吊销刚发的令牌:玩家要靠它撤销注销。

### 4.4 撤销

`POST /api/account/delete/cancel {access_token}` → login `CancelAccountDeletion`:`ValidateAccess` 解出账号 → data_service `CancelAccountDeletion`:锁账号行,1 → 0 并清三个时刻;0 原样回;2 回「已在执行,无法撤销」。

### 4.5 执行与完成

执行者跑在 login(令牌、会话、Apple 密钥都在 login)。每个副本一个后台循环,每 30 秒一轮:

1. data_service `ClaimAccountDeletions(limit=8)`:逐片扫 `deletion_state IN (1,2) AND deletion_execute_after_ms <= now`,逐行条件更新 `SET deletion_state=2, deletion_lease_until_ms=now+120s WHERE … AND deletion_lease_until_ms <= now`,回改成的账号。1 → 2 之后不可撤销。
2. 对每个认领到的账号:`RevokeAll`;读角色(`Read(create=false)`),逐个踢线、再 `RemoveAccountRoles(account, [{player_id, owner_revision}])`(一次一名,修订号取自 `Lookup`)。删角回挂起(`ErrConflict`)或可重试的,这一轮跳过,下一轮再来。
3. 全部删完之后 data_service `FinishAccountDeletion`:先用现有的 `reapRefs` 清掉已能证明陈旧的 ref;再在账号分片一个事务里锁账号行,要求状态是 2 而且一条 ref 都不剩(在途的建角 / 过户 PENDING ref 等它们自己结束,后台 5 分钟内会收),写回执、删行。没删完回「未完成」,下一轮再来;行已不在回完成(幂等)。
4. 状态 2 停留超过 24 小时(预检之后才出现的挂起、ref 被标保留待人工等)由 gauge `data_service_account_deletion_overdue` 告警,运维按挂起原因处理。

回执 `account_deletion_receipt` 在同一账号分片,主键 `account_digest` = SHA-256(账号),列 `requested_ms`、`deleted_ms`、`roles_removed`、`source`(1 本人、2 运维)。这是合规留档,只存摘要、不存原账号。同一个人以后再用同一渠道登录时,目录建新行,data_service 顺带读一次回执,`/api/login` 回 `previous_account_deleted_at`,客户端提示一次「你之前的账号已于 X 注销完成,现在是新账号」。我们没有用户邮箱,这是 Apple「删完给确认」唯一的 App 内渠道。

### 4.6 运维入口(Google Play 要求的网页入口)

网页本身不在本仓:官网放一页说明和客服工单入口。客服核实身份后调 `LoginAdmin.ScheduleAccountDeletion{player_id, operator, reason}`(`x-admin-token`,同摘孤儿),按角色找到账号,走同一个 `RequestAccountDeletion`(预检照做,冷静期照算,`source=2`)。这条路拿不到 Apple 授权码,Apple 账号按 TN3194 引导用户手动撤销(§8.3)。

## 5. 接口

### 5.1 协议与数据

| 位置 | 改动 |
|---|---|
| `proto/common/database/rollback_database_table.proto` | `account_directory` 加字段 3~6(§4.1);新表 `account_deletion_receipt` |
| `proto/data_service/data_service.proto` | 新 RPC `RequestAccountDeletion`、`CancelAccountDeletion`、`ClaimAccountDeletions`、`FinishAccountDeletion`,只认 login 的 `ServiceCallAuth`(同目录其它方法);`AccountDirectoryResponse` 加 `lifecycle = 2`(状态、到期时刻、`previous_deleted_ms`);`LookupAccountRoleRequest.include_account_state = 3`,`AccountRoleOwnership.account_deletion_state = 7` |
| `proto/login/login.proto` | `LoginPreGate` 加 `RequestAccountDeletion`、`CancelAccountDeletion`;`LoginResponse` 加 7~9 三个字段;`LoginAdmin` 加 `ScheduleAccountDeletion` |
| `proto/role_fence/role_fence.proto` | `RoleFenceParticipant` 加 `PurgeRemovedRole`(§6.1) |
| `data/tip/Tip.xlsx` | login 段加四个码:账号注销中、注销预检未通过、注销已在执行无法撤销、注销需要重新授权 |

新 RPC 的结论一律带内回(状态枚举加挂起清单),gRPC 错误只留给鉴权和非法参数,与 `GetRoleFenceState` 同一口径:它们都幂等,没有结局未知要消歧。

### 5.2 HTTP(本仓 Java 网关,客户端可见契约,两版同批)

| 接口 | 请求 | 应答 |
|---|---|---|
| `POST /api/account/delete` | `zone_id` + 同 `/api/login` 的凭据字段(`auth_type`、`auth_token`,或 `account` + `password`),Apple 另加 `authorization_code` | `code`、`message`、`tip`、`state`、`execute_after`、`blockers[]{player_id, reason}` |
| `POST /api/account/delete/cancel` | `zone_id`、`access_token` | `code`、`message`、`tip`、`state`、`execute_after` |
| `POST /api/login`(改) | 不变 | 加 `account_deletion_state`、`account_deletion_execute_after`、`previous_account_deleted_at` |

- `zone_id` 只用来选路:网关按区找 login(与 `/api/login` 同一个选路器);各区 login 共用同一地域的目录,任一区都能办,客户端填最近登录或当前选中的区。
- `code` 沿用网关现有业务码与映射(成功、认证拒绝、故障按 tip 的 `fault` 列、限流),限流沿用登录那一桶。
- `tip` 是 login 回的 tip 码(0 = 没有),客户端按它分支,不解析 `message`:2000 凭证无效(重新拉起渠道)、2035 渠道不可用(稍后再试)、2038 冷静期中、2039 预检没过(看 `blockers`)、2040 已在执行无法撤销、2041 需要重新授权(Apple 授权码无效)。
- `blockers[].reason` 是低基数枚举串:`guild_leader`(帮主)、`role_listed`(角色寄售中)、`team_membership` / `team_invite_index` / `team_application_index` / `team_launch_role`(组队)、`match_authority_ticket` / `match_role_occupancy` / `match_spectator` / `match_challenge_invitation`(匹配、观战、切磋)、`trade_commerce`(交易行订单)、`trade_asset_op` / `mail_asset_op` / `guild_asset_op` / `player_inbox_item` / `guild_trial_*`(物品或奖励还在处理);客户端认不出的按「有未完成的游戏内事务」显示。
- 时刻一律 Unix 秒。

## 6. 删除范围

### 6.1 原则

角色墓碑一提交,这名玩家在全部参与者的护栏副本上就是「已删除」,一切业务写入都被拒,入场登记也进不来(§7.2)。所以墓碑之后清数据不用两阶段:数据已经冻住,各服务各删各的,可重试、可重复,没有回滚。清理挂在现有的删角收尾上:`settleRemovedRole` 释放名字之后、删 `account_role_removal` 小表之前,先清 data_service 自己的数据,再并行调六个参与者的新方法 `PurgeRemovedRole`,全部成功才删小表;任何一步失败小表留着,后台 `ResumeRemovals` 接着做。单删一名角色(运维摘孤儿)也走这一条,删角本来就该清数据。

`PurgeRemovedRole` 由 `shared/rolefence` 统一实现:请求里的状态必须是墓碑(账号为空);先按版本合并进本库副本,再在一个事务里锁副本、复核是墓碑、调服务注入的 `purge(ctx, tx, player)`。不需要清的服务注入 `rolefence.NoPurge`,什么都不注入算装配失败(和 `pending` 一样)。

### 6.2 清单

| 服务 / 库 | 删除 | 保留(去标识)与理由 |
|---|---|---|
| login | 令牌家族、登录态、设备集(`RevokeAll`、踢线、TTL) | 口令凭据 `user_accounts`:口令账号由运维开通,App 内不能注册,login 只读这张表;要删走运维工单 |
| data_service 目录 | 账号行(§4.5)、全部 ref;角色行变墓碑(`account` 置空,`player_id` 永不复用);名字登记(现有收尾) | 墓碑行;`player_route`(路由与「区里建过多少角色」的计数要它);回执(只有摘要) |
| data_service 存档库 | 这名玩家的全部 `player_store` 行(含写权行)、`player_snapshot` | — |
| data_service 流水库 | — | `transaction_log`、`gm_recall_*`、`rollback_audit*`、`player_debt`:资产流水与回收审计,防欺诈;只有 `player_id`,账号已无法还原 |
| friend | `friend`、`friend_request`、`friend_block` 里这名玩家在任一侧的行;`friend_capacity` | — |
| guild | `guild_member`、`guild_player_state`、`guild_application`、`guild_daily_counter`、`guild_player_op_seq`、`guild_client_request` 里这名玩家的行 | 终态的 `guild_asset_op` 与历练记录:资产审计,也是其他成员的活动历史 |
| mail | `mail_box`、收件人是他的 `mail_message`、`mail_state`、`mail_player_op_seq`、`mail_campaign_recipient` | 终态的 `mail_asset_op`:资产审计 |
| player_inbox | `player_inbox_item`、`player_inbox_stream` | `player_inbox_audit` |
| trade | `trade_favorite`、`trade_player_op_seq` | 终态的 `trade_listing`、`trade_commerce`、`trade_asset_op`:与交易对手共有的成交记录,防欺诈与对账;合同里原身份的账号串一并保留,隐私政策写明 |
| match(含组队) | `match_rating`、`match_rating_applied`、`match_network_profile`、`match_player_battle_report`、已到期的 `match_role_occupancy`、终态的观战与意图行、`team_action_receipt` | 对局报告、结算、匹配票原件:多名玩家共有 |
| payment | — | 订单与支付流水:法定保留(税务、退款争议) |
| chat | — | 世界频道与私聊原件按现有 TTL 30 天过期;滥用举证要用,隐私政策写明 |
| rank | — | 本批不做(§11):榜单按代次轮换,名字已释放、显示为空;移除要由榜单的生产者调 `RemoveRankEntry` |
| Redis 缓存(名字、展示) | 删角收尾已有的缓存失效,其余随 TTL | — |

表名取自 `proto/*/` 下的现有表声明,落码时逐表核对主键与按玩家的列;没有按玩家的列的表不碰。

### 6.3 帮主

帮主不能离帮(`ErrLeaderCantLeave`),直接删帮主的成员行会留下没有帮主的帮会。guild 给删角的挂起判定加一条 `guild_leader`,预检时就告诉玩家「请先转让帮主或解散帮会」。不做自动转让:那是替其他玩家改帮会结构的新规则。冷静期内被别人转成帮主(转给离线成员)的极少数情况,删角会一直被挡,走 §4.5 第 4 条的告警人工处理。

## 7. 一致性

### 7.1 提交点

| 动作 | 提交点 | 提交前失败 | 提交后失败 |
|---|---|---|---|
| 申请 | 账号行 0 → 1 | 什么都没写(B 方案里 Apple 若已撤销,只是解除了 Apple 授权,下次登录重新授权即可) | 吊销、踢线是尽力而为,执行时重做 |
| 撤销 | 账号行 1 → 0 | — | — |
| 执行 | 账号行 1 → 2,不可撤销 | — | 之后每一步都是幂等前滚:吊销、踢线、删角(两阶段、可重放)、清数据(墓碑之后)、删行(条件删)。没有补偿,只有重试 |
| 单名角色 | 角色行墓碑(现有) | 有挂起就放弃、清令牌 | 清数据由小表驱动重试 |

### 7.2 并发

- **建角 / 过户与注销**:都在账号行锁里判状态(`reserveRef` 与申请、认领、删行同一把锁)。状态一旦不是 0,就不会再有新 ref;之前写下的 PENDING ref,删行前必须等它结束。
- **进场与删角**:删角的墓碑事务持角色行锁复核入场登记全部关闭(现有),在线角色只会让这一名删角放弃,不会被删掉。执行者每轮先吊销令牌、踢线,EnterGame 又拒绝非 0 状态,角色迟早离线。
- **墓碑之后没有写者**:墓碑让各服务副本拒绝一切业务写入;scene 写盘以入场登记为前提,删角要求这名角色的全部登记已关闭(关闭意味着该实例的实体与迟到回调都已结束),墓碑之后修订号变了,旧世代的登记进不来。所以清掉 `player_store`(含写权行)之后,不会有迟到的写把它写回来。
- **多副本执行者**:租约挡住大部分重复;租约过期后两个副本同时处理同一账号也安全,每一步都幂等,都按版本或条件写。

### 7.3 删行与重新注册

账号行以前只增不删,读路径(行和 ref 分两条语句读)靠这一点。改成只在「状态 2 且 ref 为空」时删,之后不会再有 ref 指向这个账号串:新 ref 要先拿账号行锁,行不在时 `lockDirectory(create=true)` 建出来的是状态 0 的新行。读路径看到的只有「旧行 + 0 条 ref」和「没有行」两种,都是空列表。

删行之后同一个人再登录,建出的是新账号,角色列表为空。旧令牌:执行时已经 `RevokeAll`;另外 `auth_type=access_token` 的登录(HTTP 与 gate 两条路径)改为 `Read(create=false)`:令牌只会发给已有账号行的账号,行不在就是已注销,按凭证无效拒绝,不会凭旧令牌把账号建回来。这样不依赖登录域 Redis 的部署形态。

### 7.4 多地域

账号目录按地域各一份,注销只删请求所在地域的账号。首发单地域;多地域时客户端要对每个有账号的地域各申请一次,本批不做。

## 8. Apple 令牌撤销

### 8.1 三种做法(§9 第 1 项)

| | A:首次登录换票并保存(TN3194 的通行做法) | B:申请注销时当场换票并撤销(推荐) | C:不撤销,只引导手动撤销 |
|---|---|---|---|
| 做法 | Apple 登录时客户端多送 `authorization_code`;这个账号还没存过令牌时 login 换票,refresh token 用 login 持有的密钥加密后存进账号目录行(随删行一起消失);执行注销时解密并撤销 | 注销本来就要求重新认证,Apple 重新认证时客户端同时拿到新的 `authorization_code`,随申请送上;login 换票后立刻撤销,不保存任何 Apple 令牌 | 注销说明里写「到 设置 › Apple ID › 使用 Apple 登录 里移除本游戏」 |
| 密钥 | Sign in with Apple 私钥(.p8)+ 令牌加密密钥 | .p8 | 无 |
| 撤销时机 | 执行时(审核员当场看不到;要在申请时也撤一次) | 申请时 | 不撤 |
| 运维入口(§4.6)的 Apple 账号 | 能撤 | 不能撤,按 C 提示 | 不能撤 |
| 登录路径 | 首登多一次对 Apple 的同步调用(失败不挡登录,下次再换) | 不变 | 不变 |
| 代价与风险 | 多一份要加密、轮换的长期第三方凭据;文件最多 | 冷静期内撤销又反悔的,下次 Apple 登录会再弹一次授权(`sub` 不变,还是同一个账号) | Apple 删号指南写的是「应调 REST 接口撤销」,审核可能不认 |

推荐 B:撤销效果和 A 一样,又不用长期保存第三方凭据、不动登录路径;B 做不到的只有运维入口一种场景,TN3194 的手动撤销本来就是给它兜底的。

### 8.2 接口(A、B 共用)

- `client_secret`:ES256 签名的 JWT。头部 `kid` 为私钥的 Key ID;声明 `iss` 为 Team ID、`iat`、`exp`(Apple 上限 15777000 秒即 6 个月;我们每次现签、5 分钟有效)、`aud` 为 `https://appleid.apple.com`、`sub` 为 `client_id`(即 `AuthProviders.Apple.ClientId`)。只用标准库。
- 换票:`POST https://appleid.apple.com/auth/token`,表单 `client_id`、`client_secret`、`code`、`grant_type=authorization_code`(原生 App 不带 `redirect_uri`)。授权码一次性,约 5 分钟有效。
- 撤销:`POST https://appleid.apple.com/auth/revoke`,表单 `client_id`、`client_secret`、`token`(刚换到的 refresh token)、`token_type_hint=refresh_token`。200 即成功。
- 失败分类沿用第三方登录 §12.2 的口径:授权码无效或已用(`invalid_grant`)回「需要重新授权」,客户端重新拉起 Apple;网络、超时、非 2xx 回渠道故障,稍后重试。错误和日志不带授权码、令牌、`client_secret`。
- 配置:`AuthProviders.Apple` 加 `TeamId`、`KeyId`、`PrivateKeyEnv`(.p8 PEM 所在的环境变量名;生产不收内联值,门禁同 `AppSecretEnv`)。K8s 新建单键 Secret `login-apple-signin`,经工作负载密钥目录的 `secretKeyRef` 注入(`deploy/k8s/AGENTS.md`);`k8s_deploy.ps1` 加 `-LoginAppleTeamId`、`-LoginAppleKeyId`。三项都没配时 Apple 登录照常,Apple 账号的注销按 C 提示(启动日志写明)。
- 沙箱:`go/login/cmd/sandbox_mock` 加 `/auth/token`、`/auth/revoke`,核对 `client_secret` 的签名与声明后回固定应答。

### 8.3 兜底

拿不到可撤销的令牌时(运维入口,或 A 方案里一直没换到票),按 TN3194 照常完成删除,注销说明里写明手动撤销的位置。Apple 的服务器到服务器通知(用户在 Apple 侧撤销授权或删 Apple ID)和客户端的凭据状态检查不在本批(§11)。

## 9. 负责人定案(2026-11-19)

四项都按推荐定:Apple 撤销用 B;身份令牌一次性消费,不做挑战式 nonce;冷静期 7 天;§10 三批都同意动码。下面保留当时的选项与理由。

1. **Apple 撤销**:A / B / C(§8.1),推荐 B。
2. **nonce**:推荐不做挑战式 nonce,改做身份令牌一次性消费(§3.4)。nonce 的作用是把令牌绑到「发出挑战的那次会话」,而 `/api/login` 之前没有会话可绑:截获令牌的人连令牌里的 nonce 一起拿到,服务端只能核「发过、没用过」,效果和一次性消费一样,却要多一个网关接口、一次往返和两端客户端改动。要做完整的挑战式(服务端发 nonce → 客户端带进 SDK 请求 → login 核对并消费),两版服务端和客户端同批改。
3. **冷静期**:推荐 7 天,可配(§4.1)。
4. **同意动码**:§10 三批,可以一起同意,也可以分批。

## 10. 文件清单

只列手写文件。`.meta`、协议生成物、goctl 包装、导表输出不算(由 Codex 生成)。按 §9 推荐项(B、一次性消费)计:选 A 多约 5 个(加密存取、密钥配置与 Secret),选 C 少约 11 个(撤销客户端、Apple 密钥配置、mock、部署);nonce 改做挑战式多约 8 个(网关接口、login RPC、两端客户端)。

### 第 1 批:客户端 Apple / Google 登录(15 个)

| 仓 | 文件 |
|---|---|
| 客户端(新增) | `Assets/Plugins/iOS/MmorpgAppleSignIn.mm`;`Assets/Plugins/Android/MmorpgGoogleSignIn.androidlib/` 下 `build.gradle`、`AndroidManifest.xml`、`src/main/java/com/mmorpg/signin/GoogleSignInBridge.java`;`Assets/Editor/AppleSignInBuildPostProcess.cs`;`Assets/Scripts/Net/PlatformSignIn.cs`;`Assets/Tests/EditMode/Net/PlatformSignInTests.cs` |
| 客户端(修改) | `Assets/Scripts/UI/Ugui/QdaoServerSelectView.cs`、`QdaoServerSelectView.Visuals.cs`;`Assets/Scripts/Core/ClientSettings.cs` |
| 服务端(一次性消费) | `go/login/internal/logic/pkg/auth/idtoken.go`、`idtoken_test.go`;`go/login/internal/svc/auth_init.go` |
| 文档 | `docs/design/third-party-login-end-to-end-design.md`;`docs/ops/wechat-qq-sandbox-runbook.md` |

### 第 2 批:注销主流程(约 60 个)

| 范围 | 文件 |
|---|---|
| 协议 / 表 | `proto/common/database/rollback_database_table.proto`、`proto/data_service/data_service.proto`、`proto/login/login.proto`、`data/tip/Tip.xlsx` |
| data_service | 修改 `internal/store/account_directory_store.go`、`account_directory_steps.go`、`account_role_ref.go`、`internal/server/account_directory.go`、`internal/config/config.go`、`internal/metrics/account_directory.go`、新表登记处(建表与形状核对的表清单);新增 `internal/store/account_deletion.go`、`account_deletion_test.go`、`account_deletion_tidb_test.go`、`internal/server/account_deletion.go`;`go/shared/accountdirectory/client.go` |
| login | 新增 `internal/logic/loginpregate/accountdeletionlogic.go` 与测试、`internal/logic/pkg/accountdeletion/executor.go` 与测试、`internal/logic/loginadmin/scheduleaccountdeletionlogic.go` 与测试、`internal/logic/pkg/auth/apple_revoke.go` 与测试;修改 `internal/logic/clientplayerlogin/loginlogic.go`、`entergamelogic.go`、`createplayerlogic.go`、`internal/svc/servicecontext.go`、`internal/config/auth_providers.go` 与其测试、`etc/login.yaml`、`cmd/sandbox_mock/idtoken.go` 与其测试 |
| Java 网关 | 新增 `controller/AccountDeletionController.java`、`service/AccountDeletionService.java`、`dto/AccountDeletionRequest.java`、`dto/AccountDeletionResponse.java`、`dto/CancelAccountDeletionRequest.java`、两个对应测试;修改 `grpc/LoginRpcClient.java`、`dto/LoginResponse.java`、`service/LoginService.java`、限流配置 |
| 部署 | `tools/scripts/k8s_deploy.ps1`、`tools/scripts/dev_tools.ps1`、`tools/scripts/tests/k8s_login_auth_provider_guard.tests.ps1`、`k8s_deploy_contract.tests.ps1`、工作负载密钥目录、告警规则文件 |
| 客户端 | `Assets/Scripts/UI/Ugui/QdaoServerSelectView.LoginUtilities.cs`、`Assets/Scripts/Net/GatewayHttpClient.cs`、`Assets/Scripts/Game/GameClient.cs`、新增 `Assets/Tests/EditMode/Net/AccountDeletionTests.cs` |
| 文档 | 本文、`account-role-authority.md`(目录行可删)、`horizontal-scale-logical-db.md`(`ResourceExhausted` 多一个来源)、`docs/README.md`、`docs/PROGRESS.md`、Java 仓 `PARITY.md` |

### 第 3 批:删角收尾清数据(约 29 个)

| 范围 | 文件 |
|---|---|
| 协议与框架 | `proto/role_fence/role_fence.proto`;`go/shared/rolefence/participant.go` 与测试 |
| data_service | `internal/rolefencecoord/coordinator.go` 与测试;`internal/store/account_role_name.go`(`settleRemovedRole`);新增 `internal/store/player_purge.go` 与真库测试;`internal/svc/recall_channel.go`(本地参与者注入 `NoPurge`) |
| 六个参与者 | friend、guild、mail、match、player_inbox、trade 各:与 `pending` 同包新增 `role_fence_purge.go` 与测试,装配处(`friend/friend.go`、`guild/guild.go`、`mail/mail.go`、`match/match_service.go`、`player_inbox/player_inbox.go`、`trade/trade.go`)传入 purge;guild 的挂起判定加 `guild_leader` |
| 文档 | `horizontal-scale-logical-db.md` §3.4、§3.5(参与者接口与删角收尾) |

合计约 104 个手写文件,跨本仓、客户端仓与 Java 仓 `PARITY.md`。三批可以按 1 → 2 → 3 顺序各自落码、各自验证;第 3 批不依赖第 2 批的接口,但上架前三批都要做完(只删账号不删数据过不了两家的审核要求)。

## 11. 不做

- rank 移除已删角色:要由榜单生产者(match、guild)调 `RemoveRankEntry`,生产者得先知道这名玩家上过哪些榜,另立任务。
- Apple 服务器到服务器通知、客户端凭据状态检查(`getCredentialState` / `credentialRevokedNotification`)。
- 多地域一起删(§7.4)。
- 删口令凭据(§6.2)。
- 删号前导出个人数据。

## 12. 验证计划(落码后由 Codex 执行,命令写进 PROGRESS)

- data_service 真 TiDB:申请、撤销、认领、删行的状态机;`reserveRef` 在 1、2 状态下拒绝建角与过户;删行后读为空、再登录建新行并读到回执;两个执行者并发认领同一账号。
- `shared/rolefence`:非墓碑的 `PurgeRemovedRole` 拒绝;六个参与者各自的清理真库用例(删掉的表无行、保留的表不动、重复调用幂等)。
- login:口令与第三方的申请、撤销;执行者对假 data_service 的各种回答(挂起、可重试、完成);`access_token` 登录在行不在时拒绝;Apple `client_secret` 签名与 mock 换票、撤销;一次性消费的重放拒绝与 Redis 故障。
- Java 网关:两个新接口的控制器与服务测试,`/api/login` 新字段的序列化。
- 客户端:EditMode 覆盖回调解析、错误映射、注销界面状态;真机(iOS、安卓)需要 Apple 与 Google 开发者账号,现在没有,只能等账号到位。

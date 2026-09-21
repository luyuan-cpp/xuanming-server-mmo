# mail(邮件)M1:全局服务 + 系统邮件 / 定向邮件 + 领取短路

**Created:** 2026-09-20
**状态:** **本文只是设计,未落码、未编译、未运行任何测试;导表器与 proto-gen 都没跑过。** 本文提到的 `proto/mail/*`、`go/mail/*`、`mail_error` 段、`mmorpg_mail` 库、`biz_tag = mail` **今天全都不存在**。文中凡"应该 / 期望"都不是实测结论。§11.1 的各项已于 2026-09-20 全部由用户拍板(结果见 §11.1 拍板记录;§8 决策表里对应条目已改标〔已拍板〕)。2026-09-20 经三视角评审回修一轮,见文末「评审记录」。
**关联:** [friend-handoff-20260920.md](./friend-handoff-20260920.md) §4.1(mail 摸底)/ §4.4(A 仓对照)/ §5(坑);[friend-port-20260918.md](./friend-port-20260918.md)(结构样板);[leaderboard-system.md](./leaderboard-system.md)(发奖出口指向本服务);[microservice-zone-contract-20260914.md](./microservice-zone-contract-20260914.md)(契约 §2–§8);[xuanming-port-decisions-20260910.md](./xuanming-port-decisions-20260910.md) 的 D-9 / D-11 / D-12 / D-13 / D-14;[guild-phase2/04-asset-channel.md](./guild-phase2/04-asset-channel.md)(通用资产通道,I6 流独占);[tip-code-axis.md](./tip-code-axis.md);[snowflake-id-allocation.md](./snowflake-id-allocation.md);`AGENTS.md` §4 / §7 / §10.2 / §11(尤其 §11.6)。

> **A 仓参考不可得,如实说明**:早期摸底稿(`port_plan_mail.json`,含 11 条待拍板)随会话临时目录丢失,A 仓 `services/social/mail` 的源码本轮也拿不到。本文**没有**参考 A 仓的任何做法,全部从 B 仓现有约束(D-14、契约、资产通道、friend / trade 先例、客户端现状)重新推导。凡是"A 仓怎么做的"一律不知道,也不编。

---

## 0. 一句话

新建全局服务 `go/mail`(`MailNodeService = 8`,独占库 `mmorpg_mail`,表以 `proto/mail/mail_table.proto` 为源),客户端经 gate → `client_rpc_router` 可达:能**列表 / 读(即已读)/ 删除(定向邮件软删)/ 清理已读**系统邮件与定向邮件,过期由 sweep 物理清理;**写入方只有一个内部入口 `MailAdmin`**(令牌鉴权 + 调用方幂等键);附件在数据层按 §11.6 的实例层形状存好,但**领取一律短路回 `MailClaimNotOpen`** —— 不写 outbox、不分 seq、不碰 scene。真正的发物要等"开 `ASSET_OP_STREAM_SYSTEM_CREDIT` 流"那一批(§6)。

---

## 1. 背景、范围与裁定

### 1.1 起点(已核实)

| 事实 | 位置 |
|---|---|
| `proto/mail/`、`go/mail/` 都**不存在** | — |
| 节点枚举已有 `MailNodeService = 8` | `proto/common/base/node.proto` |
| Tip 段未开:`go/shared/generated/tip/segments.go` 里没有 base=17000 的段;`Tip.xlsx` 只有 `mail_error 17000` 的**预留注释行**(沿用交接文档 §4.1 的转述,本轮未重读 xlsx) | `data/tip/Tip.xlsx`、`segments.go` |
| `proto_gen.yaml` 有两处 mail 占位(`proto_directories` 的 `- "mail/"`、`proto_dirs` 的 `mail: "mail/"`),`domain_meta` **没有** mail | `tools/proto_generator/protogen/etc/proto_gen.yaml` |
| 旧的邮件占位结构 `MailEntry` / `MailAllData` 挂在 `PlayerAllData.mail_data = 6`,附件复用带 `pos` / `bag_type` 的 `ItemEntry`、货币用两条平行 repeated、`read_state` 一个 uint32 混表四态 —— **违反 §11.6**;文件注释自己写明默认方案是独立 Go 服务、**不填** `MailAllData`;全仓只有 `tools/merge_zone/player_rows.go` 的缓存键名清单引用 `"MailAllData"`,没有读写它的业务代码 | `proto/common/database/bag_quest_mail_data.proto`、`player_cache.proto:36` |
| 通用资产通道**已在 main 上**;`ASSET_OP_STREAM_SYSTEM_CREDIT = 5` 的流规则(Credit,tx 白名单 `TX_SYSTEM_GRANT / TX_MAIL_ATTACHMENT / TX_GM_GRANT`)已在 scene;**阻塞点是调用方白名单** `kAssetOpCallerRules` 只有 guild / trade 两行,且有单测 `SystemCreditAlwaysRejected` 锁定"任何 caller 都拒" | `asset_op_system.cpp` `kAssetOpStreamRules`;`asset_op_auth.cpp`;`cpp/tests/currency_test/asset_op_auth_test.cpp` |
| 仓内**没有任何**现役发信方:无 GM 服务(`GmNodeService` 只是枚举)、Java admin 只有 zone / 公告 / 白名单且不连 Go 服务、C++ 任务领奖是 scene 内同步直写背包、rank 发奖尚未落地 | 见 §2.1 |
| 客户端(只读核实):Mail 的 UI、状态机与 EditMode 测试**已在客户端仓提交**(`42839bd` 起),**缺的只是网络层**(`MailUiRoot` 注释 "There is no mail RPC adapter yet");交接文档 §4.1 表格"Unity 客户端 mail M1 未开工"的说法**不准确** | `mmorpg-client/Assets/Scripts/Game/Mail/*`、`UI/Ugui/Mail/*` |

### 1.2 M1 做什么

1. **系统邮件**(全服广播,一封对所有玩家可见)与**定向邮件**(发给某一个指定玩家;本文把交接文档里的"个人邮件"定义为这个,见 M-2)。
2. 客户端能力:分页列表(带服务端算的未读数 / 待领数 / `server_now_ms`)、读一封(返回正文并标已读)、按 id 批量删除、一键清理已读、领取(**短路**)。
3. 写入方:内部服务 `MailAdmin` 的两个方法(发定向 / 发系统),令牌鉴权 + 调用方幂等键(§2)。
4. 过期:读时按 `expire_ms` 过滤;物理删除**只**交 sweep(照 friend 纪律,默认 `report_only`)。玩家删除定向邮件是**软删**(`deleted_ms`),为的是让发件幂等键活满 `expire + RetentionDays`(§2.4)。
5. S2C:新的**定向**邮件到达时推 `NotifyMailEvent`(at-most-once,只触发客户端去拉;收件人 gate 经共享 Redis 的 `player:session:{id}` 查,§4.5)。
6. 部署:五处登记、独占库、号段 `biz_tag = mail`、Tip 段 17000、K8s manifest + migrate Job、robot `mail-smoke`。

### 1.3 M1 不做什么(理由 + 解禁条件)

| 不做项 | 理由 | 解禁条件 |
|---|---|---|
| **附件领取(真发物)** | 要开 SYSTEM_CREDIT 流:C++ 白名单 + 独立密钥 + 重编 scene + Go 侧 seq/outbox/reconcile,是另一批、跨语言的改动(§6) | §6 清单全部落地并验证;M2 |
| **生产环境发带附件的邮件** | 领取未开放时发出去的附件**领不到**,到期作废 = 玩家可感知的资产损失;宁可在发件入口 fail-closed(M-9) | 与领取同批解禁(M2) |
| **玩家发玩家** | 需要正文审核(仓内唯一敏感词能力在 `go/shared/playername`,只服务角色名;chat 也没有正文过滤)、防骚扰限频、拉黑判定(`friend_block` 只在 friend 进程内,`friend.proto` 没有东西向 `IsBlocked`,D-14 §6 禁跨库读),三件都没有现成件 | 三件齐备;另开 M4 |
| **帮会邮件** | guild 在 K8s 上**没有登记**(`$GoSvcCatalogue` 无 guild、无 `guild.yaml`);mail → guild 的发现键未拍(D-13:首个 Go 调用方出现时同批拍 Key 名与 `.z<N>` 豁免);发件权限归谁判未定 | guild K8s 登记(帮会 BK8s 门禁)+ D-13 拍键;M3 |
| **机器发件边(scene / rank → mail)** | M1 没有任何现存生产者(§2.1);提前开 Kafka topic 或直连通道是 YAGNI。契约 §8 规定这条边只能是 Kafka(带幂等键)或直连 gRPC + 专用 READY 选择器 + 补 `nodeTypeNameMap` / 前缀表 / scene 白名单 | 第一个真实生产者(rank 赛季发奖)立项时;与 M2 一起拍 |
| **按 zone 定向的系统邮件** | 要带 `target_zone` 列 → D-14 §9 要求合服语义与 merge_zone 步骤;M1 没有需求方 | 有需求时 `ADD COLUMN`(schemamigrate 自动做),同批写合服语义 |
| **"新角色看不到建号前发出的全服邮件"** | mail 不存玩家创建时刻;`player_name.created_ms` 在 data_service 全局库,当前**没有**对外返回它的 RPC(D-14 §6 禁跨库读)。M1 无附件,新号看到旧公告无害;**M2 带附件时这就是刷资产通道**,必须重评(§6.3 第 14b 条) | 待用户拍板(§11 第 6 条);要做就给 data_service 加只读 RPC |
| **定时生效的系统邮件(`visible_from`)** | 没有需求方,也没有调用方;加了它就要在 ReadMail / 删除 / 物化 state 行等每条按 id 定位的路径上补"未生效 = 不存在"的判定,漏一处就是提前泄露正文(评审第 6 条) | 有需求时 `ADD COLUMN visible_from_ms`(schemamigrate 自动做),同批给所有按 id 的路径加可见性判定与测试 |
| **M2 的领取状态值进客户端枚举** | `CLAIMING / CLAIMED / NEEDS_MANUAL` 的状态机要等 M2 拍板(§11 第 12 条);proto3 追加枚举值向后兼容,提前冻结无收益 | M2 追加;值号 2/3/4 在 M1 用 `reserved` 占住 |
| **撤回已发出的系统邮件** | 运营误发是真实需求,但 M1 没有运营后台;先把"不可撤回"写进 SendSystemMail 的契约 | 已拍板(用户 2026-09-20 拍板):M1 不做,契约写明不可撤回(§11 第 9 条) |
| **系统邮件的实时推送** | 需对全体在线玩家广播(`BroadcastToAllEvent` 在 Kafka 契约里存在,但 Go 侧是否有现成封装未核实);系统邮件靠入场拉取已足够 | 待用户拍板(§11 第 8 条) |
| **废弃旧的 `MailEntry` / `MailAllData`** | 改它会触发全量 proto-gen、牵动 `PlayerAllData` 与 merge_zone 缓存键清单,和 M1 无关;新表**不引用**它 | 单开一个清理小批:`mail_data = 6` 写 `reserved` + 注释,merge_zone 清单同批删 |
| **邮件的 `BannerKey` / `ActivityId`(活动横幅与跳转)** | 客户端模型里有,但服务端没有活动数据源;属展示 / 活动层 | 活动系统给出数据源后 append 字段 |
| **附件名称 / 图标下发** | 协议只下发 `config_id` + 数量(§11.6 实例层);名称图标是展示态。客户端运行时目前不加载配置表(只读核实,零调用 `AllTable`),这是**客户端**的缺口 | 客户端补表加载;协议不因此带展示字符串 |

### 1.4 M1 / M2 / M3 切分

- **M1(本文)**:上表以外的全部;**唯一的跨服务写依赖是 data_service 号段**。
- **M2 = 附件领取 + 生产发附件**:§6 的 SYSTEM_CREDIT 解禁清单;mail 成为 SYSTEM_CREDIT **唯一** seq 分配者(I6),因此 GM 直发物、系统补偿、rank 赛季奖都必须经 mail 投递(M-13);顺带开第一条机器发件边(rank → mail,契约 §8 二选一)。
- **M3 = 帮会邮件**:guild K8s 登记 + D-13 发现键 + 取成员复用 `GuildService.GetGuild` 的无会话内部口(已存在,返回按 player_id 升序的 `members`)+ 发件权限归属。
- **M4(可选)= 玩家发玩家**:见上表。

> 交接文档 §4.1 原把"帮会邮件"称作 M2(`guild 在 K8s 上没有登记` 一句)。本文把领取提前为 M2,理由:rank 发奖(leaderboard §1.3)与 GM 补偿都卡在领取上,帮会邮件卡在 guild 自己的 K8s 门禁上,前者的解禁条件更近。**这是对交接文档编号的偏离**,若用户要保持原编号,只需把 M2 / M3 对调,内容不变(§11 第 1 条)。

### 1.5 M1 的实际价值(如实)

1. **K8s 上玩家不可达**:`k8s_deploy.ps1` 的 `-GateRouterMode` 默认 `"0"`(gate 直连),mail 与 chat / guild / trade / friend 一样只承诺路由服模式(D-12)。在它翻 1 之前,任何已部署环境里玩家都碰不到 mail。
2. **生产只能发纯文本**(M-9)。与 Java 的 `AdminAnnouncementController`(公告)相比,差异只在"定向到某一个玩家 + 离线可达 + 已读状态"。
3. 所以 M1 的主要产出是:**解锁客户端网络层联调**(客户端 UI / 状态机已在,§1.1)、**把数据形状与幂等语义在 M1 定型**(M2 不必迁移存量行)、**dev 下的附件展示联调**。不应把 M1 描述成"上线后运营就能用邮件"。
4. 这条事实直接影响 §11 第 2 条:若用户接受"M1 只为联调",可以砍掉 `cmd/mailadmin`、令牌注入脚本、`mail-admin-auth` Secret 与令牌四态验证(约 5–6 个文件),生产发件入口与 M2 的 HMAC 一起设计,避免先做共享令牌、M2 再推倒。

---

## 2. 写入方(发件入口)—— M1 是否"有用"的关键

### 2.1 现状:没有任何现成的发信方

- **GM / 运维**:`GmNodeService = 23`、`NODE_GM = 28` 只是枚举,非生成代码零使用方;Java `gateway_node` 的 admin API 只有 `AdminZoneController` / `AdminAnnouncementController` / `AdminWhitelistController`,鉴权是单口令头 `X-Admin-Key`(字符串相等),Java 侧只有调 login 的 gRPC 客户端,不连任何其它 Go 服务。
- **客户端 GM 指令**:提交 `c80d173b6` 已在 gate + scene 双闸关闭 6 条 GM 指令,说明写明"线上改玩家数据的正解是 scene_admin / data_service 的签名运维面" —— **不得**新增"客户端发 GM 包发邮件"的路径。
- **C++ scene**:任务领奖 `PlayerMissionSystem::ClaimReward` 同步直写背包(`BagService::AddItems` + `TX_QUEST_REWARD`),背包满直接返回错误、不消耗领取权、**不溢出到邮件**;帮会二期明确"背包满时物品留在待发放队列,不走邮件"(`guild-phase2/README.md:80`)。
- **rank**:leaderboard 设计已定"结算出名单 → 交给 mail 发",但它本身尚未落码。

**所以 M1 若不自带一个发件入口,收件箱恒为空** —— 但"有发件入口"也只在 dev 与路由模式打开的环境里对玩家可见(§1.5)。下面给推荐。

### 2.2 选定:内部服务 `MailAdmin`,两个方法 —— 已拍板(用户 2026-09-20 拍板)(§11 第 2 条)

- **形态**:新文件 `proto/mail/mail_admin.proto`,`service MailAdmin`,**不标** `OptionIsClientProtocolService`、与客户端协议**分文件**(照 `proto/trade/trade_admin.proto` 的四重隔离:gate 的 `IsClientMessageId` 不收、路由服对 `ClientProtocol=false` 回信封拒绝、mail 会话拦截器对带会话 metadata 的调用回 `PermissionDenied`、客户端 `gen_proto.ps1` 不列本文件)。与客户端服务注册在**同一个 zrpc server**(`go/trade/trade.go` 的 `RegisterTradeAdminServer` 先例),不新增端口。
- **方法**:`SendPersonalMail`(一个收件人)与 `SendSystemMail`(全服)。拆成两个而不是一个带 oneof 的 `SendMail`,因为两者的校验、表、上限、推送完全不同,合在一起只会让每个字段都带"对另一种无意义"的注释(§11.6 的判据在接口层同样适用)。
- **调用方(M1)**:① 运维 CLI `go/mail/cmd/mailadmin`(同 module 的第二个 main,读令牌环境变量,不另起 go.mod);② robot `mail-smoke`。M2 起 rank 等机器调用方经契约 §8 的东西向边接入,**复用同一组方法与同一套幂等语义**,不另开写路径。
- **不做 Kafka 入口**:M1 没有异步生产者;先开 topic 就要先拍契约 §6 的消费组规则与幂等标记 TTL,都是为假想调用方做设计。

### 2.3 鉴权:共享令牌,来自环境变量,空 = 整个 `MailAdmin` 停用

- 仓内现役两种运维鉴权:data_service 的 `x-admin-token`(gRPC metadata 共享口令,`AdminToken` 为空即该 RPC 停用,fail-closed,`ErrCodeAdminAuthRequired`)与 C++ gate / scene 的 GM HMAC(`gate_security::VerifyGmRequestFromEnv`,canonical 串 + 时间窗 + 进程内 nonce 去重)。
- **推荐 M1 取前者的形态,但令牌只从环境变量 `MMORPG_MAIL_ADMIN_TOKEN` 读**,不进 yaml / ConfigMap(ConfigMap 不是 Secret)。规则:
  - 去首尾空白后 < 32 字节视同未配置(与 `assetop.MinSecretLen` 同口径)→ `MailAdmin` 两个方法一律回 gRPC `PermissionDenied`,启动横幅打一行 `mail_admin=disabled`;
  - 比较用常量时间比较(`crypto/subtle.ConstantTimeCompare`);
  - 令牌值不进日志、指标、错误文本;
  - **没有 dev 放行**:本机由 `start_game.ps1` 按 `assetop_dev_secret.ps1` 的做法生成并注入(落 `run/secrets/`,`run/*` 已被 .gitignore),robot 从同一环境变量读。
- **为什么 M1 不上 HMAC**:M1 生产环境发不了附件(M-9),误用后果是"发一封信";HMAC 要新签名串、nonce 表与双端实现,成本不匹配。**M2 开附件时必须重评**(那时误用后果变成"发资产");届时升级为 HMAC 或 mTLS,在 §6 清单里列为解禁条件之一。
- **"无会话 = 内部调用"不能当鉴权**:集群 gRPC 是 Insecure(`asset_op.proto` 的 `AssetBundle` 注释记载 scene `InsecureServerCredentials()`),任意进程可连。会话拦截器只负责把**客户端来源**挡在 `MailAdmin` 外,不负责认"谁是合法运维"。

### 2.4 幂等:调用方幂等键 `(sender_kind, request_id)`

- 请求体必须带 `uint32 sender_kind`(枚举:`OPS_TOOL = 1`、`ROBOT = 2`,M2 加 `RANK_SETTLE` 等)与 `uint64 request_id`(**非 0**,调用方生成;CLI 用密码学随机数,rank 之类用可重算的确定值)。
- 落地为表上的**唯一键**(D-14 每表至多一个,这里正好用掉):
  - `mail_personal`:`UNIQUE (sender_kind, request_id, owner_player_id)` —— 带上收件人,于是 rank 可以用"一次结算一个 request_id"给 N 个人各发一封,而每个 (结算, 玩家) 恰好一封;
  - `mail_system`:`UNIQUE (sender_kind, request_id)`。
- 全用整数列,**刻意不用 string 幂等键**:string 进唯一键要应用层限长 ≤191,且 guild 文档 §22 记载上游 proto2mysql `7dbda68` 一旦被采用,MEDIUMTEXT 键列会被判 `ErrLegacyKeyColumn`。整数键两个风险都不沾。
- 另存 `request_digest`(对请求规范化后的 SHA-256,`bytes`):
  - 同键 + 同摘要 → 返回首次结果(同一个 `mail_id`,`duplicated = true`),**不再推送**;
  - 同键 + 不同摘要 → 业务拒绝 `MailRequestConflict`(调用方 bug:复用了键却改了内容)。
- **判重先于一切依赖"现在"的校验**(评审第 2 条):已经成功的请求重放时,收件箱可能已满、`expire_ms` 可能已落到 now 之前、有效系统邮件可能已到上限 —— 若先做这些校验,重放会拿到 `BoxFull` / `kInvalidParameter` / `SystemMailLimit`,调用方把"已发出"误判成"没发出"而补发。所以顺序固定为:纯形状校验 → Mode 闸门 → **按唯一键查已有行**(命中即比摘要返回)→ 依赖当前时间 / 状态的校验 → 插入(§4.2 逐方法写明)。
- 兜底:插入仍可能撞唯一键(MySQL 1062,系统邮件无守卫行,两个并发首发会走到这里),按唯一键读回原行比摘要。读回为空只可能是该行已被 sweep 物理删掉(见下条)→ 回 `kServiceUnavailable`,调用方重试(重试时会被当新请求,见下条的保留期约束)。
- **幂等键的寿命**(评审第 1 / 18 条):唯一键所在的行**只有 sweep 会物理删**,条件是 `expire_ms < now − RetentionDays`;玩家删除、清理已读都是软删(§3.3),不影响唯一键。所以唯一键行的最短寿命 = `expire_ms + RetentionDays`,**与玩家操作无关**。这是"调用方在重试窗口内重放必得同一结果"的前提,§4.6 第 3 条把它写成契约。

### 2.5 失败语义

- 业务结果**一律 in-band**(`TipInfoMessage`),包括存储故障(`CommonError_kServiceUnavailable`,唯一 fault 码);鉴权失败与客户端来源走 gRPC status(这是东西向接口,不经路由服桥接,调用方看得到 status code)。
- zrpc `Timeout: 4000`(契约 §3 上限,虽然 `MailAdmin` 不经路由服,但同一个 server 的 C2S 方法受它约束);每请求套 `RequestBudget()`(照 friend F15),避免 go-zero 超时拦截器抢先回 `DeadlineExceeded` 吞掉 in-band 结果。
- **调用方可安全重试的前提是幂等键不变**。超时 / 连接断 = 结果未知,调用方用同一 `(sender_kind, request_id)` 重发即可;mail 内部对 MySQL **不做**自动重试。
- 推送失败不影响方法结果(§4.5)。
- 每次调用计 `mail_admin_send_total{method,sender_kind,outcome}`(outcome ∈ `ok|duplicated|rejected|fault|denied`,全部低基数,不带 player_id / request_id);运维误发或入口被刷都要能从这里看见。

---

## 3. 数据模型

### 3.1 系统邮件:读时合并,不做写时扇出(推荐)

| | 写时扇出(发信时每人一行) | **读时合并(推荐)** |
|---|---|---|
| 需要全服玩家名册 | **需要**。data_service 只有按 id 点查 / 批查(`LoadPlayerData` / `GetPlayerHomeZone` / `BatchGetPlayerName` 等),没有列举 RPC;`player_name` 表在 data_service 全局库,D-14 §6 禁跨库读 | 不需要 |
| 写放大 | 全服玩家数行,要分批 + 断点续发 + 进度表 | 一行 |
| 离线 / 新建角色 | 发信后新建的角色漏发;要补发就要第二套逻辑 | 天然覆盖(是否"应该覆盖"见 §11 第 6 条) |
| 玩家侧状态 | 就在本人行上 | 只在**有交互**(已读 / 删除 / M2 领取)时物化一行 `mail_system_state` |
| 读成本 | 一次按 owner 扫 | 多一次"有效系统邮件集"查询(有界,见下)+ 一次按 (player, id IN …) 的点查 |

代价与约束:读时合并要求**有效系统邮件数有界**,否则每次列表都要扫一遍。约束手段:`SendSystemMail` 强制 `expire_ms`(不许永不过期,最长 `Mail.MaxSystemMailTtlDays`),并在发件时检查"当前有效系统邮件数 < `Mail.MaxActiveSystemMails`"(推荐 100)。这个检查在并发发件下是软上限(两个运维同时发可能各自看到 99),接受理由:发件方只有运维,并发极低;读侧另有 `ORDER BY sys_mail_id DESC LIMIT` 硬上限并在截断时打 WARN + 计指标(并有告警,§4.7),所以超额时丢弃的是 **`sys_mail_id` 最小的**几封(号段单调,即最早发出的),不会拖垮查询。没有 `ORDER BY` 时截掉哪几封由执行计划决定(走 `expire_ms` 索引时截掉的是最晚过期的),翻页也可能不稳定 —— 所以排序必须写进 SQL。

**不用"每玩家一个已看最大 sys_mail_id 游标"**:游标只能表达"看过",表达不了单封的已删 / 已领,最后还是要物化行;两套状态并存只会多一处不一致。

### 3.2 定向邮件与系统邮件分表

一张系统邮件行对应所有收件人,一张定向邮件行对应一个 owner。塞进一张表,`owner_player_id` 对系统行无意义 —— 正是 §11.6 的"这个字段在这里被忽略"判据。客户端视图统一成同一个 `MailSummary`,来源用 `kind` 区分。`mail_id` 与 `sys_mail_id` **出自同一个号段 `biz_tag = mail`**,所以全局不撞号,合并列表可以按 id 排序,`ReadMail` / `DeleteMails` 只凭 id 就能定位(先查定向表主键,再查系统表主键,都是点查),**不信任客户端传 kind**。

### 3.3 已读 / 删除 / 过期的表达

| | 定向邮件(`mail_personal`) | 系统邮件(`mail_system` + `mail_system_state`) |
|---|---|---|
| 已读 | 本行 `read_ms`(0 = 未读) | state 行 `read_ms`;无 state 行 = 未读 |
| 删除 | **软删**:本行 `deleted_ms`(0 = 未删);**不物理删**,行留到 sweep(删除本身幂等:删不存在 / 已删 = 成功) | state 行 `deleted_ms`(**不删**系统行) |
| 过期 | 读时按 `expire_ms <= now` 过滤;物理删**只**交 sweep | 同左,sweep 先删 state 行再删系统行(§4.6) |
| 领取(M2 用) | 本行 `claim_state` | state 行 `claim_state` |

两类邮件的删除写法对称:都是"打 `deleted_ms` 标记,物理删只归 sweep"。定向邮件不能物理删的理由是幂等键(§2.4):唯一键就在这一行上,玩家删信即删键,调用方用同一 `request_id` 重试会插进第二封 —— M2 带附件时就是重复发物(评审第 1 / 18 条)。所有读路径(`ListMails` / `ReadMail` / 未读数 / 待领数 / 邮箱现数)一律带 `deleted_ms = 0`。

状态拆成独立列,**不沿用** `MailEntry.read_state` 一个 uint32 混表"未读 / 已读 / 已领 / 过期"的做法:过期是时间推导出的,不是存储态;已读与已领互相独立(读了可以不领)。

**状态列用 uint32,不用 proto enum 作列类型**(照 friend `FriendRequestRecord.status` 的理由:enum 列被写入未知值时 protoreflect 扫行会落到 0,分不清"未知"与"UNSPECIFIED")。取值在 `mail.proto` 的 `MailClaimState` 枚举里定义一次,表注释引用它,Go 侧用枚举常量写库 —— 取值集合只有一个权威来源。M1 的 `claim_state` 取值只有 `0 = NONE`(无附件)、`1 = UNCLAIMED`;值号 2 / 3 / 4 在枚举里 `reserved`,M2 追加 `CLAIMING / CLAIMED / NEEDS_MANUAL` 时使用,不得挪作他用。

**两张表都写推导值**(评审第 7 条):定向行与系统邮件的 state 行在建行时一律按附件推导 —— 附件非空 → `1`,否则 `0`。**`0` 只表示"无附件"**,不表示"M1 没用这一列"。若 state 行在 M1 恒写 0,M2 滚动升级窗口里旧 pod 为带附件的系统邮件物化出的 state 行就是 `NONE`,M2 领取从 `UNCLAIMED` 做 CAS 恒失败(永久领不到),还会被清理已读当"无附件"删掉。

### 3.4 邮箱上限与超限策略

- 定向邮件上限 `Mail.MaxPersonalMailsPerPlayer`(100,已拍板(用户 2026-09-20 拍板));系统邮件**不计入**(有 §3.1 的独立上限)。
- 并发控制:照 friend 的"守卫行"思路但**不存计数**:`mail_box` 表每玩家一行,只作串行化载体。发件事务 `SELECT … FROM mail_box WHERE player_id=? FOR UPDATE` 后**现数** `SELECT COUNT(*) FROM mail_personal WHERE owner_player_id=? AND deleted_ms=0 AND expire_ms>?`(now)—— 口径与 `ListMails` / `unread_count` 一致:玩家看不到的信(已过期未 sweep、已软删)**不占名额**(评审第 3 条:否则 `report_only` 默认下 100 封过期信会让邮箱永久"满")。
  - 走 `idx_mail_personal_0 (owner_player_id, mail_id)` 定位本人的行后回表过滤。本人名下的行数上界 = 有效 ≤ 上限 + 保留期内(已过期或已软删、尚未 sweep)的行;后者的量级 = 该玩家被发信的速率 × (`MaxPersonalMailTtlDays` + `RetentionDays`)。M1 发件方只有运维与 robot,回表可接受,不为它另建覆盖索引;M2 引入 rank 这类批量发件方时按实际速率重评。
  - 为什么不照 friend 存 `friend_count`:存计数就要求每一条删除路径(玩家删、清理已读、sweep)都在同一事务里减它,漏一处就是计数上漂、玩家永远"满"且零报错(friend F12 记载过同一类风险)。现数没有这个失败模式。
  - 删除路径**不需要**拿守卫:删除只会让真实数变少,发件方读到的数只可能偏高 → 只会保守地多拒一次,不会超限。
  - 守卫行在事务外 `INSERT IGNORE` 补齐(照 friend F8:放进事务会形成 insert-intention 死锁)。行数 ≤ 被发过定向邮件的玩家数,不需要回收。
- 超限策略(已拍板(用户 2026-09-20 拍板),§11 第 5 条):**拒收**,回 `MailRecipientBoxFull` 给发件方(运维 / 机器),由发件方决定重试或换通道;**不推荐**"挤掉最旧的已读邮件":M1 没问题,但 M2 有附件后"挤掉"会变成吞资产,策略跨阶段变形。rank 发奖(M2)撞满时的处置在 M2 设计里定。

### 3.5 附件形状(严格按 AGENTS §11.6)

- **实例层只描述"是什么、多少"**:货币 `(currency_type uint32, amount uint64)`、物品 `(config_id uint32, count uint32)`。**没有** `pos` / `slot` / `bag_type` / 展示名 / 图标。
- **协议 message 的来源已拍板(用户 2026-09-20 拍板)(§11 第 3 条):mail 自有** `MailCurrency{currency_type, amount}` / `MailItem{config_id, count}`:客户端 `gen_proto.ps1` 不收 `common/asset`(已核实),import `asset_op.proto` 会把资产通道的内部 schema 带进客户端。与 `asset_op.proto` 的 `CurrencyAmount` / `ItemGrant` 字段一一对应,Go 侧只有**一处**转换函数拼 `AssetBundle`,并有逐字段对齐测试(对 `CurrencyAmount` / `ItemGrant` 的 descriptor 断言字段名、类型、个数相同 —— 通道侧加字段时测试变红),这是 DRY 的守法。若拍板选 import,改回直接引用即可,库表不受影响。
- **不复用** `ItemEntry`(带 `pos` / `bag_type` 两个布局字段);**不沿用** `MailEntry` 的两条平行 repeated(`attached_currency_types` / `attached_currency_amounts` 靠下标对齐,正是 §11.6"集合下标不得承载业务语义"的反例)。
- **协议字段用两个 repeated,不用整个 `AssetBundle`**:`AssetBundle` 还带聚宝斋 P2 的 `item_uuids` / `pet_id`,而 scene 的 `ValidateCreditBundle` 见到这两个字段就终局拒("credit v1 不支持按 guid 发物 / 发宝宝")。把它放进邮件契约,就是留一个永远非法的口子。领取时由 Go 把两个 repeated 拼成 `AssetBundle`(`item_uuids` / `pet_id` 留空)。(另一种选法见 §11 第 3 条。)
- **存储**:照 trade `TradeAssetOpRecord.payload` / guild 的口径,**库表 proto 不 import `asset_op.proto`**,附件存成序列化的 `AssetBundle` 的 `bytes` 列 `attachments`(MEDIUMBLOB)。M2 领取时原样作为 SYSTEM_CREDIT 的 bundle —— 发件时校验过的就是将来入账的,同一份字节。
- **集合顺序**:附件的展示顺序**没有**业务含义;协议注释写明"客户端按 `(currency_type)` / `(config_id)` 自行排序展示,不得依赖下标"。若产品将来要"按运营指定顺序展示",必须新增显式 `display_order` 字段,不能靠 repeated 下标。
- **容量 = scene 单个 Credit 包的上限**,做到"一封邮件 = 一条 op":货币 ≤ 4 条且 `currency_type` 不重复、物品 ≤ 16 条且 `config_id` 不重复、`count ≥ 1`、`1 ≤ amount ≤ INT64_MAX`。**在发件入口校验**(mail 是系统接缝),不等领取时被 scene 终局拒。
  - **`currency_type` 必须在 scene 认可的集合内**(评审第 10 条):scene 的 `IsCurrencyAmountValid`(`asset_op_system.cpp`)要求 `currency_type < kCurrencyMax`;该枚举**只在 C++**(`cpp/libs/modules/currency/constants/currency.h`:0 金币 / 1 钻石 / 2 绑钻,`kCurrencyMax = 3`),没有 proto 定义。M1 用配置 `Mail.AllowedCurrencyTypes: [0,1,2]`,yaml 注释指向 `currency.h` 为权威来源;推荐的终态是把 `CurrencyType` 提升为 proto enum 作唯一来源(单开小批,§11 第 17 条)。
  - ⚠ scene 还要求 `config_id` 存在于 Item 表。mail M1 不加载配置表,**这一条 M1 校验不了**;M1 生产不许发附件(M-9),所以两条缺口都只影响 dev。M2 解禁前 mail 必须加载 Item 表、并把货币白名单换成唯一来源(§6.3 第 14 / 14a 条)。
- **M1 就建好附件列**:理由只有两条 —— dev 联调客户端的附件展示;数据形状在 M1 定型,M2 不改表形状。(M1 生产本来就不发附件,所以不存在"已发出的邮件补不回附件"的问题,早稿的这条理由已删。)

### 3.6 表清单:`proto/mail/mail_table.proto`(草案)

D-14 自检:整数主键 ✔、每表 ≤1 唯一键 ✔、string 不进键 ✔、只用 uint32 / uint64 / string / bytes(schemamigrate 拒绝 `sint*` / `fixed*` / `sfixed*`)✔、时间戳 uint64 毫秒 ✔、TiDB 方言只用 500021–500024 选项 ✔。

```proto
syntax = "proto3";
package mailpb;
option go_package = "mail/proto/mail";
import "proto/db/proto_option.proto";

// mail 独占库 mmorpg_mail 的唯一事实源(D-14)。不 import asset_op.proto:
// 附件存序列化 AssetBundle 的 bytes(与 trade_asset_op.payload 同口径)。
// 状态列一律 uint32,取值定义在 mail.proto 的 MailClaimState(理由见 friend_table.proto)。
// OptionIndex 组只许追加到末尾、不得调序改列(idx_<表>_<n> 按位置命名,schemamigrate 只比名字)。
// 本文件不进客户端 gen_proto.ps1。

// 定向邮件:一行 = 一个收件人的一封信。
message MailPersonalRecord {
  option(OptionTableName) = "mail_personal";
  option(OptionPrimaryKey) = "mail_id";
  // idx_mail_personal_0 (owner_player_id,mail_id):列表 / 现数 / 清理已读 的主路径(deleted_ms / expire_ms 回表过滤)
  // idx_mail_personal_1 (expire_ms):sweep 候选(sweep 是唯一的物理删除者)
  option(OptionIndex) = "owner_player_id,mail_id;expire_ms";
  option(OptionUniqueKey) = "sender_kind,request_id,owner_player_id";   // 发件幂等(§2.4)
  option(OptionTiDBNonclusteredPK) = true;
  option(OptionTiDBShardRowIDBits) = 4;
  option(OptionTiDBPreSplitRegions) = 4;

  uint64 mail_id = 1;             // id_segment biz_tag=mail(与 sys_mail_id 同号段)
  uint64 owner_player_id = 2;
  uint32 sender_kind = 3;         // MailSenderKind
  uint64 request_id = 4;          // 调用方幂等键,非 0
  bytes  request_digest = 5;      // 规范化请求的 SHA-256
  string sender_name = 6;         // 展示用发件人署名(应用层限长),不是身份
  string title = 7;
  string body = 8;
  bytes  attachments = 9;         // 序列化 AssetBundle(只填 currencies / items);无附件为空
  uint32 claim_state = 10;        // MailClaimState;建行时按附件推导:非空 1,否则 0
  uint64 sent_ms = 11;
  uint64 expire_ms = 12;          // 必填,> sent_ms
  uint64 read_ms = 13;            // 0 = 未读
  uint64 updated_ms = 14;
  uint64 deleted_ms = 15;         // 0 = 未删;玩家删除只打此标记,物理删只归 sweep(幂等键寿命,§2.4)
}

// 系统(全服)邮件:一行 = 一封对所有人可见的信。
message MailSystemRecord {
  option(OptionTableName) = "mail_system";
  option(OptionPrimaryKey) = "sys_mail_id";
  option(OptionIndex) = "expire_ms";                        // 有效集查询 + sweep
  option(OptionUniqueKey) = "sender_kind,request_id";
  option(OptionTiDBNonclusteredPK) = true;
  option(OptionTiDBShardRowIDBits) = 4;
  option(OptionTiDBPreSplitRegions) = 4;

  uint64 sys_mail_id = 1;
  uint32 sender_kind = 2;
  uint64 request_id = 3;
  bytes  request_digest = 4;
  string sender_name = 5;
  string title = 6;
  string body = 7;
  bytes  attachments = 8;
  uint64 sent_ms = 9;             // 发出即可见;M1 不做定时生效(§1.3)
  uint64 expire_ms = 10;
  uint64 updated_ms = 11;
}

// 某玩家对某封系统邮件的交互状态,按需物化(首次已读 / 删除时建行)。
message MailSystemStateRecord {
  option(OptionTableName) = "mail_system_state";
  option(OptionPrimaryKey) = "player_id,sys_mail_id";
  option(OptionIndex) = "sys_mail_id";                      // sweep:系统邮件过期后按 id 清物化行
  option(OptionTiDBNonclusteredPK) = true;
  option(OptionTiDBShardRowIDBits) = 4;
  option(OptionTiDBPreSplitRegions) = 4;

  uint64 player_id = 1;
  uint64 sys_mail_id = 2;
  uint64 read_ms = 3;
  uint64 deleted_ms = 4;          // 0 = 未删
  uint32 claim_state = 5;         // MailClaimState;物化时从系统行附件推导(非空 1,否则 0),M1 就写推导值(§3.3)
  uint64 updated_ms = 6;
}

// 定向邮件的每玩家串行化守卫行;只作 FOR UPDATE 载体,不存计数(§3.4)。
message MailBoxRecord {
  option(OptionTableName) = "mail_box";
  option(OptionPrimaryKey) = "player_id";
  option(OptionTiDBNonclusteredPK) = true;
  option(OptionTiDBShardRowIDBits) = 4;
  option(OptionTiDBPreSplitRegions) = 4;

  uint64 player_id = 1;
  uint64 created_ms = 2;
}
```

唯一键 option 名 `OptionUniqueKey`(= 500012)已在 `proto/db/proto_option.proto:89` 核实;落码时仍需核实的是 §11 U1、U2:**复合唯一键的列序**能否被 proto2mysql 原样保留,以及单列 `OptionIndex` 的写法。

**行里没有 `zone_id` / `home_zone`**(D-14 §9 的声明):所有行以全局唯一且合服不变的 `player_id` 或全局号段 id 为键,**合服不需要 merge_zone 步骤**。按 zone 定向的系统邮件若将来加 `target_zone`,届时重做这条声明。

**MySQL 与 TiDB**:MySQL 8.4 下 `NONCLUSTERED` 等选项被 schemamigrate 忽略(注释形式);写事务用 RC(§4.1)是为 MySQL 的间隙锁,TiDB 没有间隙锁 —— **锁序相关的测试只在 MySQL 8.4 上有意义**(§10 假绿陷阱)。

### 3.7 号段:`mail_id` / `sys_mail_id` 走 `biz_tag = mail`

- 仓库口径"永久身份走号段":号从 1 起、上限 2^55,与存量 snowflake 值域不相交;进程崩溃只留空洞不重号(`snowflake-id-allocation.md`、`node-id-overhaul-plan-20260908.md:296`)。
- 用 snowflake 要新登记一个 `snowflakealloc` kind(etcd 槽位),更重,且与 `trade_listing` / `guild_asset_op` 先例不一致。
- 两种 id **共用一个 tag**:合并列表按 id 排序需要全局唯一;分两个 tag 就要四处各加两个且失去这个性质。
- M2 的领取 op_id 另加一个独立 tag(推荐 `mail_asset_op`),照 `trade_listing` / `trade_asset_op` 的"两类值域各自连续,便于对账"先例。**M1 只加 `mail` 一个。**
- svc 层照 `go/trade/internal/svc/servicecontext.go` 的 `NewListingIDSegment`:`shared/idsegment` 客户端,**无 snowflake 回退**,`IdSegment.Enabled=false` 拒启。**不能照 friend 写**(friend 不领号段、不连 data_service)。

---

## 4. 请求流与写路径形状

### 4.1 请求流

```
客户端 ── ClientRequest{message_id} ──▶ gate:会话 → ≤1KB → MessageLimiter → IsClientMessageId
路由服:route.Table[message_id] → MailNodeService(全局池,随机实例),透传 x-session-detail-bin
mail:  grpcstats → killswitch → session(客户端来源:方法白名单;MailAdmin:带会话即 PermissionDenied)
       → serverbase(按 Tip fault 列定性) → logic
运维:  mailadmin CLI / robot ──直连 gRPC(x-mail-admin-token)──▶ mail(同一 server,MailAdmin)
S2C:   mail ─读 SharedRedis player:session:{收件人}(只推 ONLINE)─▶ 组 PlayerGateInfo
       → kafkautil.PushToPlayer → gate-cmd_g<N> → 收件人所在 gate
```

所有写事务:**READ COMMITTED**;判定读在守卫之后且用当前读(照 friend F7 —— RR 的间隙锁在"同一玩家并发插不同行"时成环,且只在 MySQL 上炸)。

### 4.2 各方法的事务边界

| 方法 | 事务与锁 | 幂等 / 失败语义 |
|---|---|---|
| `MailAdmin.SendPersonalMail` | ① 纯形状校验(长度、`request_id≠0`、收件人非 0、附件条目规则含货币白名单)→ ② **Mode 闸门**:`Mode ∉ {dev,test}` 时附件非空回 `MailAttachmentNotOpen`、`sender_kind=ROBOT` 回 `kInvalidParameter`(M-9;都在取号之前,拒绝不消耗号段)→ ③ 事务外取号段 id + `INSERT IGNORE mail_box` → ④ `BEGIN(RC)`:`mail_box` 行 `FOR UPDATE` → **按唯一键 `WHERE sender_kind=? AND request_id=? AND owner_player_id=?` 当前读**,命中即比摘要、`ROLLBACK`、返回 `duplicated=true` 或 `MailRequestConflict` → 未命中才做依赖"现在"的校验:`sent_ms < expire_ms ≤ sent_ms + MaxPersonalMailTtlDays`(sent_ms = 服务端当前时间)→ 现数(§3.4)→ 超限回 `MailRecipientBoxFull` → `INSERT mail_personal`(`claim_state` 按附件推导)→ `COMMIT` → 事务外推送 | 判重在守卫锁内、与现数同一把锁,所以已成功的请求重放**必回同一 `mail_id`**,不会被满箱 / 过期拒(§2.4)。1062 兜底读回仍保留;读回为空 → `kServiceUnavailable`。号段 id 已取但未插入只留空洞,不重号。**不校验收件人是否存在**(见下) |
| `MailAdmin.SendSystemMail` | ① 纯形状校验 → ② Mode 闸门(同上)→ ③ **按唯一键 `WHERE sender_kind=? AND request_id=?` 查已有行**,命中即比摘要返回 → ④ 未命中才校验 `expire_ms` 窗口与有效系统邮件计数(软上限,§3.1)→ 取号 → 单条自动提交 `INSERT mail_system` | 并发首发撞 1062 → 读回比摘要;读回为空 → `kServiceUnavailable`。不推送;**不可撤回**(§11 第 9 条) |
| `ListMails` | 无事务,两条只读查询:定向 `WHERE owner_player_id=? AND mail_id<? AND deleted_ms=0 AND expire_ms>? ORDER BY mail_id DESC LIMIT n+1`;系统有效集 `WHERE expire_ms>? ORDER BY sys_mail_id DESC LIMIT MaxActiveSystemMails`,再按 `(player_id, sys_mail_id IN …)` 点查 state 行,剔除 `deleted_ms>0`;未物化的系统邮件 `claim_state` 按附件推导;内存合并按 id 降序取前 n。未读数 / 待领数由服务端对**全集**算(定向有效 ≤ 上限 + 系统 ≤ 上限,有界) | 只读,天然幂等 |
| `ReadMail` | 定向:单条 `UPDATE mail_personal SET read_ms=? WHERE mail_id=? AND owner_player_id=? AND deleted_ms=0 AND read_ms=0`(自动提交),再读回;系统:**以系统行存在且有效为条件**的 upsert(见下方"系统邮件 state 行的唯一写法")| 已读再读 = 成功;**不是自己的定向邮件 / 已软删 = `MailNotFound`**(不泄露存在性);已过期 = `MailExpired`;系统邮件已被本人删 = `MailNotFound` |
| `DeleteMails`(≤20 个 id) | 定向:一条 `UPDATE mail_personal SET deleted_ms=?, updated_ms=? WHERE owner_player_id=? AND mail_id IN (…) AND deleted_ms=0 AND (claim_state = 0 OR (claim_state = 1 AND expire_ms <= ?))`;系统:对每个有效 id 做条件 upsert `deleted_ms`,用**同一判定**;两段各自自动提交。其余 id 再点查一次,分成"不存在 / 已删"(计成功)与"受保护"(进 `protected_mail_ids`) | 删不存在 / 已删 = 成功;带未领有效附件的 id 不删,原样回在 `protected_mail_ids` 里,**整请求仍成功**(与客户端"有未领附件不许删"一致,见 M-10) |
| `DeleteReadMails` | 定向:`UPDATE … SET deleted_ms=? WHERE owner_player_id=? AND deleted_ms=0 AND read_ms>0 AND (claim_state = 0 OR (claim_state = 1 AND expire_ms <= ?)) LIMIT MaxPersonal`;系统:对有效集里本人已读且满足同一判定的做条件 upsert `deleted_ms` | 重复调用 = 成功,返回实删数 |
| `ClaimMailAttachments` | **短路**:校验会话身份与参数形状后直接回 `MailClaimNotOpen` | 见 §4.3 |

**删除判定是白名单,不是黑名单**(评审第 5 条):早稿写的 `NOT (claim_state IN (1,2,4) AND expire_ms>?)` 会在过期后放行 2(CLAIMING)/ 4(NEEDS_MANUAL)—— 这两态挂着未终结的资产 op,删掉就断了对账线索,与 sweep "2 / 4 永不删"矛盾。M1 只有 0 / 1 两个值,判定写成 `claim_state = 0 OR (claim_state = 1 AND expire_ms <= now)`;**M2 追加状态时只允许往白名单里加 `3`(CLAIMED),2 / 4 无论是否过期一律进 `protected_mail_ids`**,并在代码注释写明。

**系统邮件的"可见"只有一个定义**:`expire_ms > now`(M1 无定时生效,§1.3)。`ReadMail` / `DeleteMails` / `DeleteReadMails` / 将来的 `ClaimMailAttachments` 对不可见的系统邮件一律回 `MailNotFound` 或 `MailExpired`,**不建 state 行**。

**系统邮件 state 行的唯一写法**(评审第 7 / 8 条,合为一条语句):

```sql
INSERT INTO mail_system_state (player_id, sys_mail_id, read_ms, deleted_ms, claim_state, updated_ms)
SELECT ?, sys_mail_id, ?, ?, IF(LENGTH(attachments) > 0, 1, 0), ?
  FROM mail_system WHERE sys_mail_id = ? AND expire_ms > ?
ON DUPLICATE KEY UPDATE read_ms = IF(read_ms = 0, VALUES(read_ms), read_ms), …   -- claim_state 不在 UPDATE 里
```

- 系统行不在或已过期 → 不插入任何行,所以 **sweep 与迟到的 upsert 交错也不可能生出孤儿 state 行**:upsert 只认 `expire_ms > now` 的系统行,sweep 只删 `expire_ms < now − RetentionDays`(RetentionDays ≥ 1)的系统行,两个集合不相交。
- MySQL 对"命中重复键但值没变"返回 `RowsAffected = 0`,与"源行不存在"同值。所以 `RowsAffected = 0` 时再按主键点查一次系统行,区分"不存在 / 已过期"(回 `MailNotFound` / `MailExpired`)与"已是目标状态"(成功)。不依赖驱动的 `clientFoundRows`。

**为什么定向邮件发件不校验收件人存在**:mail 没有玩家名册。可用的旁证是 data_service 的 `GetPlayerHomeZone`,**已核实**(`go/data_service/internal/server/dataserviceserver.go` 的 `GetPlayerHomeZone`):无映射 → gRPC `NotFound`(文案 `no home zone mapping` 是滚动升级契约,login 的 `homezone.IsUnmapped` 靠它识别)、Redis 故障 → `Unavailable`、成功才返回 `home_zone_id`。但它只能证明"有 home zone 映射",**不能当"玩家存在"的权威依据**(存量玩家可能早于映射注册)。M1 发件方只有运维与 robot,输错 id 的后果是"一封没人看的信,到期被 sweep",可接受;`mail_box` 守卫行会因此多一行,量级由运维操作数决定。M2 机器发件方出现时若要校验:`NotFound` 作业务拒绝、`Unavailable` 作 `kServiceUnavailable`,并先解决"存量玩家无映射"的误拒。

**`ListMails` 的两个硬上限都必须在 SQL 里**(照 friend §5.6 可借鉴的一条"列表类读要有 LIMIT 硬上限"):系统有效集 `ORDER BY sys_mail_id DESC LIMIT MaxActiveSystemMails`,截断时 WARN + `mail_list_truncated_total`(有告警,§4.7);state 点查的 `IN` 列表长度 ≤ 该上限。

### 4.3 "领取短路"的确切行为

- `ClaimMailAttachments{mail_ids ≤20 | all=true}`:
  1. 取会话身份,失败 → `CommonError_kInvalidParameter`;
  2. 形状校验(`mail_ids` 与 `all` 二选一、条数上限),失败 → `kInvalidParameter`;
  3. **直接回 `MailError_kMailClaimNotOpen`**。
- **不读库、不写库、不写 outbox、不调 `AllocateSeq`、不调 scene、不改任何 `claim_state`、不计入任何业务指标之外的状态**。只计 `mail_claim_short_circuit_total`(无 label)。
- 为什么 M1 就定义这个方法而不是 M2 再加:客户端 UI 已经有 Claim / ClaimAll 两个操作(`MailOperation`),M1 就能接线并显示准确文案;代价是提前占一个消息号并要配限频行。备选(M2 再加方法)见 §11 第 4 条。
- 码用 mail 段新码而不是 `CommonError_kFeatureUnavailable(1006)`:M1 反正要开 mail 段,多一个码几乎零成本;领域内的码让客户端文案准确("附件领取暂未开放"而非"功能暂未开放"),与 trade 的 `kTradeFeatureDisabled`、match 的 `kMatchModeNotOpen` 同一先例。M2 解禁后本码**不删**(已上线的码不回收),只是不再返回。

### 4.4 失败语义总则

- 全部 in-band;唯一 fault 码 `CommonError_kServiceUnavailable`(MySQL / 号段真故障)。号段取不到(data_service 不可达)也归它 —— 它确实是依赖故障。
- 客户端 10 秒超时后会锁住写操作直到一次成功 `ListMails`(`MailUiState.Tick`,只读核实),所以**所有写方法对同一 id 的重复请求必须回成功**,上表已逐条满足。

### 4.5 推送(S2C)

- 只在 `SendPersonalMail` **首次**成功(非 duplicated)、事务提交之后、事务之外推给收件人:`NotifyMailEvent{reason = NEW_MAIL, ts_ms}`。**不带邮件内容**(不带整份列表,也不带标题 —— 标题是可能被运维填得很长的文本,推送体只做"去拉"的信号)。
- 纪律照 friend F13:推送函数不返回 error;失败只打日志 + `mail_push_total{outcome}`;绝不影响 RPC 结果;at-most-once(契约 §5 三个丢失窗口),**红点真相是 `ListMails` 返回的 `unread_count`**。
- **先找到收件人所在 gate**(评审第 4 / 14 条;契约 §5"调用方负责查会话"):`MailAdmin` 的调用方不是收件人,没有收件人的会话 metadata。推送前经 **SharedRedis** 读 `player:session:{recipient}`(值为 proto 编码的 `plpb.PlayerSession`,写者是 player_locator,mail **只读**;SharedRedis 禁 Cluster),`proto.Unmarshal` 后**只推 `SESSION_STATE_ONLINE`**,用其中的 gate id / gate instance id 组 `PlayerGateInfo`。照 friend 的 `internal/data/session_reader.go` 与 `internal/logic/push.go`:键名用常量前缀拼接,测试与写者共用前缀;无会话 / 非 ONLINE = 离线 = 不推,计 `mail_push_total{outcome="offline"}`;读 Redis 失败计 `outcome="session_error"`,同样不影响 RPC 结果。
- `gate_instance_id` 为空时 fail-closed 拒发(`kafkautil.PushToPlayer` 既有行为)。
- 继承 K8s 存量缺口:K8s 上 Go 服务不注入 `KAFKA_COMMAND_TOPIC_*`,推送落 `gate-cmd_g1` 而 gate 消费 g2,**静默丢失**(friend manifest 注释已记)。mail manifest 与 friend / trade / match 逐字一致地**不写**这两个变量 —— 这是全仓缺口,不在 mail 里打补丁。

### 4.6 过期 sweep(照 friend 纪律)

配置 `Sweep`:`Mode` 默认 `report_only`,只收 `report_only|delete`,空串 / 非法值 `Validate` 拒收;`RetentionDays` 默认 7(过期后再保留 7 天供客服查询),**下界 1,`Validate` 拒 0**(评审第 8 / 25 条:它同时是幂等键寿命的余量与"upsert 与 sweep 不相交"的间隔,§4.2);`BatchLimit` 默认 1000,≤0 拒收;保留期上限 36500 天防回绕;`Interval` 带首轮随机抖动;单轮预算 30s。多副本各跑各的,不引入 leader(DELETE … LIMIT 天然可并发,重复删 = 0 行)。

截止点 `cutoff = now − RetentionDays`;**`cutoff ≤ 0` 本轮什么都不做**(无符号列与负数比较会匹配全表,friend `sweepCutoffMs` 的教训)。计数一律"派生表 LIMIT 的有界 COUNT"。删除一律单条自动提交 `DELETE … LIMIT`(前提 `binlog_format=ROW`,已核实 `deploy/k8s/manifests/infra/mysql.yaml:60`;本地 compose 取 8.4 默认)。

顺序是正确性的一部分:

1. **定向邮件**:`DELETE FROM mail_personal WHERE expire_ms > 0 AND expire_ms < ? AND claim_state IN (0,1) LIMIT ?`(M1 只有 0 / 1;已软删的行同样只按 `expire_ms` 判,软删不缩短寿命)。
   - 注释写明:**M2 起扩成 `IN (0,1,3)`,刻意不含 2(CLAIMING)与 4(NEEDS_MANUAL)**:这两态挂着未终结的资产 op,删了就断了对账线索 —— 必须等 op 终结。写成 `IN` 而不是 `<>`(friend 的索引经验)。
   - `expire_ms = 0` 的行(不应存在,发件校验挡住)**只统计不删**并打 ERROR(fail-closed 保险),计 `mail_sweep_invalid_rows`。
2. **系统邮件,先删物化行再删系统行**:选出 `expire_ms < cutoff` 的 `sys_mail_id`(`LIMIT`)→ 对每个 id 反复 `DELETE FROM mail_system_state WHERE sys_mail_id=? LIMIT ?` 直到 0 行 → 再 `DELETE FROM mail_system WHERE sys_mail_id=? AND expire_ms < ?`(WHERE 复核条件)。
   - 为什么这个顺序:反过来会留下无主的 state 行,而它们唯一的清理索引就是 `sys_mail_id`,系统行没了之后再也没人会按这个 id 去扫 —— 孤儿永久残留。按本顺序,中途崩溃只会留下"系统行还在、state 行删了一半",下一轮继续删。
   - 与 `ReadMail` / `DeleteMails` 的 upsert 交错不会生出孤儿:state 行只能经 §4.2 的条件 upsert 建出,它要求系统行存在且 `expire_ms > now`,与 sweep 的候选集不相交(`RetentionDays ≥ 1`)。
   - M2 起系统邮件的 state 行若有 `claim_state IN (2,4)`,**整封系统邮件本轮跳过**。
3. **幂等键的保留期约束**:唯一键行只有本节会物理删,最短寿命 = `expire_ms + RetentionDays`(与玩家删除无关,§2.4)。行被 sweep 删掉后,同一 `(sender_kind, request_id)` 再来会被当成新请求。所以**调用方重试窗口必须远小于 `expire + RetentionDays`**;`SendSystemMail` / `SendPersonalMail` 的契约注释写明这一点。

**sweep 指标**(全部低基数,不带 player_id;每轮都写,`report_only` 也写):

| 指标 | 类型 | 含义 |
|---|---|---|
| `mail_sweep_candidate_rows{table,mode}` | gauge | 本轮有界计数得到的候选行数(`report_only` 下就是"本该删"的量) |
| `mail_sweep_deleted_total{table}` | counter | 实删行数 |
| `mail_sweep_invalid_rows{table}` | counter | `expire_ms = 0` 保险分支命中数 |
| `mail_sweep_last_run_unixtime` | gauge | 每轮结束时**无论删了几行**都写 `time.Now().Unix()`;循环卡死 / panic 后不再调度时它停止前进 |

### 4.7 告警(评审第 15 条)

只在事件发生时更新的指标发现不了"sweep 卡死"(leaderboard §3.7 / L-21 的教训);而读侧的有效系统邮件集查询要靠 sweep 保持有界。新建 `deploy/k8s/mail-alerts.yaml`,形状照 `deploy/k8s/scene-manager-alerts.yaml`,多副本一律 `max()` 聚合:

| 告警 | 表达式 | 含义 |
|---|---|---|
| `MailSweepStalled` | `time() - max(mail_sweep_last_run_unixtime) > 3 * <Interval 秒>` | sweep 循环卡死或停止调度 |
| `MailSweepInvalidRows` | `increase(mail_sweep_invalid_rows[1h]) > 0` | 出现 `expire_ms = 0` 的行(发件校验被绕过) |
| `MailSystemListTruncated` | `increase(mail_list_truncated_total[10m]) > 0` | 有效系统邮件超软上限,玩家少看到信 |

该文件计入 M1d 文件清单;§10 第 ⑤ 步验证 `:9240/metrics` 上这些指标都有预建的 0 值序列(否则 `increase` 类告警在首个事件前没有序列可比)。

---

## 5. 协议(草案)

### 5.1 服务与文件

| 文件 | 内容 | 进客户端 `gen_proto.ps1`? |
|---|---|---|
| `proto/mail/mail.proto` | `service ClientPlayerMail`(`OptionIsClientProtocolService = true`),5 个 C2S + 1 个 S2C | **是** |
| `proto/mail/mail_admin.proto` | `service MailAdmin`,不标客户端开关 | 否 |
| `proto/mail/mail_table.proto` | §3.6 | 否 |
| `generated/code/proto/tip/mail_error_tip.proto` | 导表器生成 | **是**(导表器开段之后) |

节点类型由 protogen 的三步推导落到目录名 → `MailNodeService`(= 8),路由服 `TargetNodeTypes` 从路由表自动收集,**不需要**加进 `ZoneScopedNodeTypes`(全局服务)。S2C 方法必须和 C2S 在同一个名字含 `ClientPlayer` 的 service 里,生成器才出客户端下行 handler(friend F1 同理)。`MailAdmin` 名字不含 `ClientPlayer` / `GamePlayer`,不会给客户端出桩。

### 5.2 `mail.proto`(草案)

```proto
syntax = "proto3";
package mailpb;
option go_package = "mail/proto/mail";

import "proto/db/proto_option.proto";
import "proto/common/base/empty.proto";
import "proto/common/base/tip.proto";
// 不 import proto/common/asset/asset_op.proto(§3.5 / §11 第 3 条:客户端不收 common/asset)。

// 身份一律取会话(D-9):本文件任何请求体都没有 player_id。
// 时间一律 uint64 毫秒;客户端用 server_now_ms 判过期,不信本地钟(jubaozhai BrowseListings 先例)。

enum MailKind {
  MAIL_KIND_UNSPECIFIED = 0;
  MAIL_KIND_SYSTEM = 1;     // 全服
  MAIL_KIND_PERSONAL = 2;   // 定向(系统 / 运维发给某一个玩家)
}

enum MailClaimState {       // 表里的 uint32 状态列取值的唯一定义处(§3.3)
  MAIL_CLAIM_STATE_NONE = 0;          // 无附件(只表示这个,不表示"未使用")
  MAIL_CLAIM_STATE_UNCLAIMED = 1;     // 有附件未领
  // M2 追加 CLAIMING = 2 / CLAIMED = 3 / NEEDS_MANUAL = 4(状态机待 M2 拍板,§11 第 12 条);值号预留,不得挪作他用。
  reserved 2 to 4;
}

message MailAttachments {
  // 实例层(§11.6):只有"是什么、多少";顺序无业务含义,客户端自行排序,不得依赖下标。
  repeated MailCurrency currencies = 1;     // ≤4,currency_type 不重复且在白名单内
  repeated MailItem items = 2;              // ≤16,config_id 不重复
}

// 与 asset_op.proto 的 CurrencyAmount / ItemGrant 逐字段对应(对齐测试守住);领取时由 Go 一处转换拼 AssetBundle。
message MailCurrency { uint32 currency_type = 1; uint64 amount = 2; }
message MailItem     { uint32 config_id = 1;     uint32 count = 2; }

message MailSummary {
  uint64 mail_id = 1;
  MailKind kind = 2;
  string sender_name = 3;
  string title = 4;
  uint64 sent_ms = 5;
  uint64 expire_ms = 6;
  bool read = 7;
  MailClaimState claim_state = 8;
  MailAttachments attachments = 9;          // 摘要里就带(≤20 条,小),客户端才能做"有附件不许删"
}

message ListMailsRequest {
  uint64 before_mail_id = 1;   // 0 = 从最新开始;游标分页,按 mail_id 降序
  uint32 page_size = 2;        // 0 取默认 20,超过 50 按 50 钳制
}
message ListMailsResponse {
  TipInfoMessage error_message = 1;
  repeated MailSummary mails = 2;   // 按 mail_id 降序;这是协议承诺的排序,不是下标语义
  bool has_more = 3;
  uint32 unread_count = 4;          // 服务端对全集计算,红点真相
  uint32 claimable_count = 5;       // M1:有附件且未过期的封数(领不了也计,客户端据此提示"暂不可领")
  uint64 server_now_ms = 6;
}

message ReadMailRequest { uint64 mail_id = 1; }
message ReadMailResponse {
  TipInfoMessage error_message = 1;
  MailSummary summary = 2;
  string body = 3;
  uint64 server_now_ms = 4;
}

message DeleteMailsRequest { repeated uint64 mail_ids = 1; }   // 1..20
message DeleteMailsResponse {
  TipInfoMessage error_message = 1;
  uint32 deleted_count = 2;
  repeated uint64 protected_mail_ids = 3;   // 因带未领有效附件而未删
}

message DeleteReadMailsRequest {}
message DeleteReadMailsResponse {
  TipInfoMessage error_message = 1;
  uint32 deleted_count = 2;
}

message ClaimMailAttachmentsRequest {
  repeated uint64 mail_ids = 1;   // 1..20;与 all 二选一
  bool all = 2;
}
message ClaimMailAttachmentsResponse {
  TipInfoMessage error_message = 1;   // M1 恒为 MailClaimNotOpen(形状非法时为 InvalidParameter)
}

enum MailEventReason {
  MAIL_EVENT_REASON_UNSPECIFIED = 0;
  MAIL_EVENT_REASON_NEW_MAIL = 1;
}
message MailEventS2C {
  MailEventReason reason = 1;
  uint64 ts_ms = 2;
}

service ClientPlayerMail {
  option (OptionIsClientProtocolService) = true;
  rpc ListMails (ListMailsRequest) returns (ListMailsResponse);
  rpc ReadMail (ReadMailRequest) returns (ReadMailResponse);
  rpc DeleteMails (DeleteMailsRequest) returns (DeleteMailsResponse);
  rpc DeleteReadMails (DeleteReadMailsRequest) returns (DeleteReadMailsResponse);
  rpc ClaimMailAttachments (ClaimMailAttachmentsRequest) returns (ClaimMailAttachmentsResponse);
  // S2C:服务端不实现,且不在会话白名单里;客户端只作接收方。
  rpc NotifyMailEvent (MailEventS2C) returns (Empty);
}
```

写操作的响应**不带**列表快照:客户端 `MailUiState.Complete` 需要一份权威列表,由客户端在写成功后再发一次 `ListMails`。代价是多一次请求(吃 `ListMails` 的 5 次/秒配额,够用);好处是写方法不必各自拼一份分页快照,响应体也不会随邮箱大小膨胀。另一种选法见 §11 第 7 条。

### 5.3 `mail_admin.proto`(草案)

```proto
enum MailSenderKind {
  MAIL_SENDER_KIND_UNSPECIFIED = 0;
  MAIL_SENDER_KIND_OPS_TOOL = 1;
  MAIL_SENDER_KIND_ROBOT = 2;      // 只在 Mode=dev|test 接受;其它 Mode 回 kInvalidParameter(取号之前)
}

message SendPersonalMailRequest {
  MailSenderKind sender_kind = 1;
  uint64 request_id = 2;           // 非 0;同键重放返回首次结果(先判重、后做依赖当前状态的校验);
                                   // 键的最短有效期 = expire_ms + RetentionDays,与收件人删信无关(§2.4 / §4.6 第 3 条)
  uint64 recipient_player_id = 3;
  string sender_name = 4;          // ≤16 字符
  string title = 5;                // ≤32 字符
  string body = 6;                 // ≤1000 字符
  mailpb.MailAttachments attachments = 7;   // M1 生产必须为空(M-9)
  uint64 expire_ms = 8;            // 必填;sent_ms < expire_ms ≤ sent_ms + MaxPersonalMailTtlDays
}
message SendPersonalMailResponse {
  TipInfoMessage error_message = 1;
  uint64 mail_id = 2;
  bool duplicated = 3;
}

message SendSystemMailRequest {
  MailSenderKind sender_kind = 1;
  uint64 request_id = 2;
  string sender_name = 3;
  string title = 4;
  string body = 5;
  mailpb.MailAttachments attachments = 6;   // M1 生产必须为空(M-9)
  uint64 expire_ms = 7;            // 必填;sent_ms < expire_ms ≤ sent_ms + MaxSystemMailTtlDays。发出即可见,不做定时生效(§1.3)
}
message SendSystemMailResponse {
  TipInfoMessage error_message = 1;
  uint64 sys_mail_id = 2;
  bool duplicated = 3;
}

// 内部运维入口。刻意不标 OptionIsClientProtocolService、与客户端协议分文件(照 trade_admin.proto)。
// 鉴权:metadata x-mail-admin-token 须等于环境变量 MMORPG_MAIL_ADMIN_TOKEN(≥32 字节);
// 未配置 = 整个服务停用(PermissionDenied)。带会话 metadata 的调用一律 PermissionDenied。
service MailAdmin {
  rpc SendPersonalMail (SendPersonalMailRequest) returns (SendPersonalMailResponse);
  rpc SendSystemMail (SendSystemMailRequest) returns (SendSystemMailResponse);
}
```

长度上限是推荐值(字符 = Unicode 码点,Go 侧 `utf8.RuneCountInString`),写进 `Mail.*` 配置并在 `Validate` 里给上界;上限的依据是下行包大小(`ListMails` 50 条摘要 × ~250B ≈ 12KB),**gate 下行单包上限本轮未核实**(§11 U4)。

### 5.4 身份与拦截器(D-9)

- 拦截器链照 friend 固定顺序:grpcstats → killswitch → session → serverbase,由可单测的 `buildUnaryInterceptors` 返回。
- `session.ClientMethods` 白名单只放行 5 个 C2S;`NotifyMailEvent` **不在**;`MailAdmin` 的两个方法**不在**(客户端来源调用回 `PermissionDenied`)。测试遍历 `ClientPlayerMail_ServiceDesc.Methods` 断言"除 `NotifyMailEvent` 外全在白名单",再遍历 `MailAdmin_ServiceDesc.Methods` 断言"全不在"。
- `MailAdmin` 的令牌校验放在 admin server 自己(不进公共拦截器):它只对这一个 service 有意义。
- 请求体一律不带 `player_id`;`ListMails` / `ReadMail` 等只作用于会话身份。
- 只承诺路由服模式可达(D-12),`node_util.cpp` 一字不改;业务代码不读 `ZoneId` 做分支(全局服务)。

### 5.5 限频档位(`data/MessageLimiter.xlsx`,**必须在 proto-gen 之后**按那一次的消息号填)

| 方法 | 档位 | 理由 |
|---|---|---|
| `ListMails` | 5 次/秒 | 写后再拉 + 翻页 |
| `ReadMail` | 10 次/秒 | 客户端"选中即读",连续点选 |
| `DeleteMails` / `DeleteReadMails` / `ClaimMailAttachments` | 3 次/秒 | 写操作(= 默认档,仍显式配) |
| `NotifyMailEvent` | 不进表 | S2C,按惯例 |

`tip_message` 列照现有行填 `1000`(= `kRateLimitExceeded`,已核实:`tools/scripts/friend_xlsx_patch.py` 的 `LIMITER_TIP_MESSAGE` 注释)。行由 `tools/scripts/mail_xlsx_patch.py message-limiter` 按 `proto/message_id.txt` 里 `ClientPlayerMail` + 方法名**查号**追加,不在文档或脚本里写死号(§9 M1a)。在配档位之前全部走 gate 默认 3 次/秒,robot 冒烟相邻请求间隔 ≥1.1s。客户端被限频后**不得**自动重试(每次拒绝都计非法包,默认 50 次断开)。`MailAdmin` 不经 gate,不受此表约束。

### 5.6 Tip 段 17000:码清单草案

Tip.xlsx 把预留注释行换成组头 `//mail_error base=17000 width=1000`(**`//` 紧贴组名**;`// ` 带空格的是注释),**只能 `load_workbook` 后在自己的段 `insert_rows`**,不许整表写回(交接文档 §5.4:三个会话同写这张表,丢失零报错)。这一步**脚本化**:`tools/scripts/mail_xlsx_patch.py tip-segment`,照 `friend_xlsx_patch.py` / `guild_b5a_xlsx_patch.py` —— 幂等(已是目标状态只报告)、支持 dry-run、现状与预期不符即中止、保存后核对**其它段的码数不变**、fault 列留空。导表器给 xlsx 的 `MailXxx` 加 `k` 前缀生成 `MailError_kMailXxx`。

| 码名(xlsx A 列) | 中文文案(草案) | fault | 谁会看到 | 何时回 |
|---|---|---|---|---|
| `MailClaimNotOpen` | 附件领取暂未开放 | 空 | 玩家 | M1 的 `ClaimMailAttachments` |
| `MailNotFound` | 邮件不存在或已删除 | 空 | 玩家 | 读 / 领不存在、非本人、已删的邮件 |
| `MailExpired` | 邮件已过期 | 空 | 玩家 | 读 / 领已过期的邮件 |
| `MailRecipientBoxFull` | 收件人邮箱已满 | 空 | 发件方 | `SendPersonalMail` 超上限 |
| `MailAttachmentNotOpen` | 当前环境不允许发送带附件的邮件 | 空 | 发件方 | M1 生产发带附件的邮件(M-9) |
| `MailAttachmentInvalid` | 邮件附件不合法 | 空 | 发件方 | 超 4/16 条、重复 type/config_id、`currency_type` 不在白名单、count=0、amount 越界 |
| `MailRequestConflict` | 发件请求编号重复但内容不同 | 空 | 发件方 | 幂等键冲突(§2.4) |
| `MailSystemMailLimit` | 有效系统邮件数量已达上限 | 空 | 发件方 | §3.1 软上限 |

跨域语义复用 common 段(照 friend constants.go 的理由):`kInvalidParameter`(会话缺失、参数形状、文本超长、`expire_ms` 非法)、`kServiceUnavailable`(**唯一 fault**,MySQL / 号段故障)。**mail 段零 fault 码** —— 发件方被拒是规则生效,不是我方出错;`constants_test.go` 用 `TestTipCodesVerdicts` 把每个码的定性钉死,用 `TestNoHandWrittenTipCodes` 守住"不手写数字"。

---

## 6. 资产通道解禁清单(领取的解禁条件,M2)

这是"开 `ASSET_OP_STREAM_SYSTEM_CREDIT` 流"要改的全部,按依赖顺序。**M1 一行都不做。**

### 6.1 C++(重编 scene)

1. `cpp/libs/services/scene/player/system/asset_op_auth.cpp`:`kAssetOpCallerRules` 追加 `{"mail", "MMORPG_ASSET_OP_SECRET_MAIL", {ASSET_OP_STREAM_SYSTEM_CREDIT}, 1}`;改文件头"SYSTEM_CREDIT 在 v1 没有合法调用方"那段注释。
2. `asset_op_auth.h`:`kCallerNotAllowed` 等处同口径注释。
3. `asset_op_system.cpp`:只改流规则表附近的注释(**流规则那一行已存在**,不用改)。
4. `cpp/tests/currency_test/asset_op_auth_test.cpp`:`SystemCreditAlwaysRejected` 改写为"mail 通过;guild / trade / system / 空 拒绝",补"mail 拿 GUILD / TRADE 流被拒"的用例。
5. 串行 `msbuild … /m:1` 重编 scene 与测试。

### 6.2 proto(只改注释,不改字段)

6. `proto/common/asset/asset_op.proto`:`ASSET_OP_STREAM_SYSTEM_CREDIT` 与 `AssetOpAuth.caller` 的注释写明归 mail。

### 6.3 Go

7. `proto/mail/mail_table.proto` **追加**两张表(照 trade `TradePlayerOpSeqRecord` / `TradeAssetOpRecord`):seq 表 `(player_id, stream, next_seq, epoch, updated_ms)` 主键 `(player_id, stream)`;outbox 表 `op_id` 主键、唯一键 `(player_id, stream, stream_epoch, seq)`、租约 / attempts / last_reason / payload / resolved_by 列,`ref_kind` + `ref_id = mail_id` 挂业务外键。⚠ outbox 表的唯一键已被 seq 占用,"每封邮件至多一条 op"要靠**邮件行的状态机**保证(下一条),不靠第二个唯一键(D-14 每表 ≤1)。
8. 领取事务:同一事务里 ① 把邮件行(或系统邮件的 state 行)`claim_state` 从 `UNCLAIMED` CAS 到 `CLAIMING`(`RowsAffected == 1` 才继续)② `assetop.AllocateSeq` ③ 插 outbox 行;`correlation_id = op_id`(trade 惯例)。系统邮件的幂等粒度是 `(player_id, sys_mail_id)` —— state 行的主键正好是它。
9. 实现 `assetop.Store`(`ListDue` 两段读 / `Claim` / `Finalize` / `Reschedule`)、`ManualResolver`、`PendingAgeReader`、`Signer("mail", env MMORPG_ASSET_OP_SECRET_MAIL)`;`Finalize` 必须 `WHERE op_id=? AND status=<pending>` 且 `RowsAffected==1` 才把邮件翻 `CLAIMED`。
10. 结局映射:`APPLIED` durable → `CLAIMED`;`REJECTED` / 终局 abort → 回 `UNCLAIMED` 并记原因;`APPLIED partial` → `NEEDS_MANUAL`(**不自动回滚、不标已领**,assetop §4.33);`RETRY + kAssetBagFull` → 保持 `CLAIMING`,同 seq 重投;`NOT_HERE`(玩家离线)→ 只退避。
11. 背包满长期挂起的策略(`DeadlineMs` 后 abort 退回 `UNCLAIMED`,还是无限期挂)—— M2 设计时拍(§11 第 12 条)。
12. 一键领取**逐封一条 op**,不合包(合包会突破 4/16 上限并破坏逐封幂等);受 `DefaultLimits{MaxPending:16, MaxSpan:512}` 约束,超限 fail-closed 回"稍后再试"。这是与 scene 1024 位账本窗口配套的**正确性证明,不是可调参数**。
13. 号段新 tag `mail_asset_op`(四处同加,§7.4)。
14. mail 加载 Item 表,在发件入口校验 `config_id` 存在(§3.5)。
14a. **货币类型白名单的唯一来源**(评审第 10 条):把 `CurrencyType` 提升为 proto enum(C++ `currency.h` 与 mail 同源),或至少让 `Mail.AllowedCurrencyTypes` 有一个与 `kCurrencyMax` 对齐的测试。做到之前不解禁生产附件 —— 否则 `currency_type ≥ 3` 的附件会在领取时被 scene 终局拒(REJECTED),玩家看到一封永远领不到的信。
14b. **新号领取系统邮件附件的资格**(评审第 12 条):读时合并让任何有会话的玩家看到全部有效系统邮件;M2 若允许系统邮件带附件,批量新建的小号可以逐个领取 = 刷资产通道。二选一必须在 M2 拍板:① 带附件的系统邮件只对 `created_ms ≤ 系统邮件 sent_ms`(或运营指定截止点)的玩家可领 —— 需要 data_service 暴露建角时刻的只读 RPC;② 发件侧强制**系统邮件不带附件**,全服补偿改走"定向批量"(rank 结算同一形状)。
15. `go/shared/assetop/types.go`:只改"v1 一律验签失败"那句注释。
16. `MailAdmin` 鉴权升级为 HMAC 或 mTLS(§2.3)。
17. 去掉 M-9 的生产附件开关。

### 6.4 脚本与部署

18. `tools/scripts/lib/assetop_dev_secret.ps1`:`$AssetOpDevSecretEnvNames` 加 `MMORPG_ASSET_OP_SECRET_MAIL`(scene 与 mail 必须同值,否则每次都是 27008 且日志无线索)。
19. `cpp_nodes.ps1` / `start_game.ps1`:注释;确认 mail 进程继承到密钥。⚠ `go_services.ps1` 里**零** assetop 引用(trade 代码注释说"由 go_services.ps1 注入"与磁盘不符,实际靠 `start_game.ps1` 注入后子进程继承),单独用 `go_services.ps1` 起 mail 会拿不到密钥 —— M2 顺手补上或写进 runbook。
20. `k8s_deploy.ps1`:`Resolve-InjectedSecret -MinLength 32` 注入(guild / trade 的也还没做)+ scene gRPC NetworkPolicy + mail 自己的 `AssetOp.Enabled` 开关(默认 false,照 trade 的 ConfigMap)。
21. **一把新的、独立的** ≥32 字节密钥;不得与 guild / trade 共用(I6:流独占,一人一把)。

### 6.5 可选

22. data_service 只读 RPC `GetPlayerAssetOpLedger`(`LedgerReader`,已落盘账本回读)—— **目前全仓不存在**,trade / guild 也没有;reconcile 不依赖它也能收敛。

---

## 7. 部署与登记清单

### 7.1 端口:gRPC `50900` / 指标 `:9240`(本轮全仓 grep 复核)

- **复核方法**:Grep `\b(50900|51900|52900|53900|9240|10240|11240)\b`,排除 `third_party/`、`cpp/generated/`、`generated/`、`robot/logs/`、`bin/`、`*.pb.*`。
- **结果**:只命中 `PROGRESS.md:5361`(friend F1 条目顺带记的一句)与 `docs/design/friend-handoff-20260920.md:668 / 907`;另 `53900` 在 `go/friend/internal/data/recommend_repo_mysql_test.go:266` 作为**玩家 id 常量**出现,不是端口。**没有进任何代码、yaml、脚本、manifest 或契约。**
- **本地位移核验**(`go_services.ps1`:`Port + (Zone−1)*1000 + (Index−1)*1`,再经 `Resolve-BindablePort`;顶层单行 `MetricsListenAddr` 同规则位移):

| zone | gRPC | 指标 | 与 `$ServiceCatalogue` 其它条目 |
|---|---|---|---|
| 1 | 50900 | 9240 | 不撞(guild 50300 / friend 50400 / match 50500 / router 50600 / chat 50700 / trade 50800) |
| 2 | 51900 | 10240 | 不撞(各服务 +1000);⚠ 51900 落在机器 B 2026-09-20 实测保留区 **51840–51939**(交接文档 §4.1,本轮未重跑 netsh) |
| 3 | 52900 | 11240 | 不撞 |
| 4 | 53900 | 12240 | 不撞 login 53000/54000 段与 player_locator 53200/54200 段 |

  指标 9240 系列与现有位移指标(match 9170 / friend 9180 / db 9160 / scene_manager 9150 / chat 9210 / trade 9230)及不位移的嵌套写法(login 9101 / player_locator 9190 / guild 9220)都不重叠;与 leaderboard 选的 `51100 / :9250` 相邻不冲突。
- **保留区的结论**:落进去不致命 —— `Resolve-BindablePort` 自动上挪并改用派生 yaml,对等方经 etcd 发现实际端口;trade 的 zone 2(51800)已按同一方式被接受。**Windows 保留区每次开机随机**,换一个号也不能保证下次不撞。**推荐仍用 50900**(§11 第 13 条)。
- **K8s 不位移**:`$GoSvcCatalogue` 现占 6000 / 9000 / 50000 / 50100 / 60000 / 50500 / 50700 / 50600 / 50800 / 50400,50900 空闲。
- **契约 §7 的指标端口分工表**现值只写到 `9220 guild`,缺 `9230 trade`;M1d 同批补上 `9230 trade / 9240 mail`。

### 7.2 五处登记(契约 §7,端口逐字一致)

| # | 位置 | 内容 |
|---|---|---|
| ① | `tools/scripts/go_services.ps1` `$ServiceCatalogue` | `mail`:Port 50900、Tier 1、`AllowMultiInstance = $true`、**不写** `Global`;注释写 zone 2 的保留区情况 |
| ② | `tools/scripts/go_svc_image.ps1` | `mail = @{ Dir = "mail"; Entry = "mail.go"; ImageName = "mmorpg-mail" }`(ImageName 与 k8s 侧逐字一致) |
| ③ | `tools/scripts/k8s_deploy.ps1` `$GoSvcCatalogue` + ConfigMap | `Port = 50900; Global = $true; MigrateJob = "mail-migrate.yaml"`;ConfigMap 的契约值一律用 `Get-AuthoritativeScalar` 从 `go/mail/etc/mail.yaml` 读(friend 有 22 处这种写法),**显式写**:`ListenOn: 0.0.0.0:50900`、`Timeout: 4000`、顶层唯一 `ZoneId`(在所有嵌套段之前,行尾无注释)、`LeaseTTL: 60`、`MetricsListenAddr: ":9240"`、`Etcd: { Hosts: …, Key: "" }`(D-13,省略 Key 会 Fatal → CrashLoop)、`KillSwitchPrefix: ""`、**`Mode: ${mailMode}`**(dev 档用 `Get-AuthoritativeScalar` 从 mail.yaml 取,staging / prod **固定 `pro`**,照 trade 那段注释写"不能带着造数能力上线";M-9 生产禁附件与 `ROBOT` 发件方的放行**都依赖它**)、**共享会话句柄** `Redis: { Host, Type: node, Key: "" }`(照 friend;不写 DB;mail 没有私有 key,只读 `player:session:{id}`,`config.Validate` 拒 `Type: cluster`)、**`DataServiceRpc: { Etcd: { Hosts, Key: dataservice.rpc }, Timeout, NonBlock: true, Middlewares: { Breaker: false } }`**(号段的依赖;这是**调用方的发现键**,指向 zone 内的 data_service,不违反 D-13,理由照 `go/trade/etc/trade.yaml` 的 `DataServiceRpc` 段注释)、`Middlewares.StatConf.IgnoreContentMethods: [/mailpb.MailAdmin/SendPersonalMail, /mailpb.MailAdmin/SendSystemMail]`(契约 §7 要求本地 yaml 与 ConfigMap **两处都带**)、`MySQL.DBName: mmorpg_mail`、`Schema.AutoMigrate: false`(staging / prod)、`IdSegment.Enabled: true` 段照 trade、`Kafka.Brokers`、`Mail.*` 各上限(含 `AllowedCurrencyTypes`)、**`Sweep.{Mode, Interval, RetentionDays, BatchLimit}` 四键全写**(friend ConfigMap 注释记载:optional 段整段缺失时 go-zero 不回填 default,`Validate` 拒启)—— 以上除固定值外全部 `Get-AuthoritativeScalar` 从 `go/mail/etc/mail.yaml` 取。**令牌不进 ConfigMap**,做法见下方"令牌注入(已核实,定稿)" |
| ④ | `deploy/k8s/manifests/go-svc/mail.yaml` + `mail-migrate.yaml`(新) | Service + Deployment + 同文件 PDB,`replicas: 2` + `podAntiAffinity`,gRPC 探针,`preStop` 5s 配进程 24s 硬截止,`POD_IP` 走 Downward API,`prometheus.io/port: "9240"`;**不写** `KAFKA_COMMAND_TOPIC_*`(与 friend / trade / match 逐字一致,§4.5);不挂 snowflake 卷(mail 用号段)。migrate Job:同镜像 `-f <yaml> -migrate`、`wait-mysql` initContainer、`backoffLimit` / `podFailurePolicy` 对齐 D-14 退出码(0 / 1 / 3 可重试 / 4 需人工) |
| ⑤ | `tools/scripts/start_game.ps1` | 照 friend 的形态**改四处**(交接文档说"三行",实际是三行 + 一个预检块):`$services += 'mail'`、`$optionalServices += 'mail'`、MySQL 健康后的 `mmorpg_mail` 库预检块(库缺或无权 → 跳过 mail 并打印补建命令,不拒启整个游戏)、单独一行 `if ('mail' -notin $skippedServices) { Start-LocalGoServices @('mail') }`。**必须放在 data_service 之后**(mail 依赖号段;trade 在第 576 行、data_service 在第 528 行已是这个顺序,mail 追加在 friend 那行之后即可)。只加 `$services` 不加单独启动行,服务根本不会被拉起。另:本机令牌注入(§2.3) |
| 附 | `tools/scripts/tests/k8s_migrate_gate.tests.ps1` | 夹具里加一份 mail 条目副本(`ConfigFile = 'mail.yaml'; ImageName = 'mmorpg-mail'; Port = 50900; Global = $true`) |

**令牌注入(已核实,定稿;原 U6)**:K8s 上**没有**给 Go 服务注入 Secret 的现成机制 —— `tools/scripts/lib/release_common.ps1` 的 `Resolve-InjectedSecret` 只返回字符串;`k8s_deploy.ps1` 里唯一建 Secret 对象的是 `redis-auth`(stringData + apply,值为空时 `delete --ignore-not-found`);`deploy/k8s/manifests` 下只有 `infra/redis.yaml` 用了 `secretKeyRef`,所有 Go 服务 Deployment 零引用;现役的 `GateTokenSecret` / `InternalAuthSecret` 是渲染进 ConfigMap 的。所以"令牌走 Secret + env"是**新增工作**:

1. `k8s_deploy.ps1` 的 `Initialize-InjectedSecrets` 加 `$script:MailAdminToken = Resolve-InjectedSecret -EnvName MMORPG_MAIL_ADMIN_TOKEN -DevFallback "" -MinLength 32`;
2. 照 `redis-auth` 在 apply mail 之前生成 `mail-admin-auth` Secret(stringData,key `token`);值为空时 `delete secret mail-admin-auth --ignore-not-found`;
3. `mail.yaml` Deployment 加 `env: MMORPG_MAIL_ADMIN_TOKEN` ← `valueFrom.secretKeyRef {name: mail-admin-auth, key: token, optional: true}`。Secret 缺失 = 环境变量为空 = `MailAdmin` 停用,符合 fail-closed。**K8s 上默认不注入令牌,预期横幅 `mail_admin=disabled`、不期望能发信**。
4. 本机令牌由 `start_game.ps1` 注入(§2.3);**单独用 `go_services.ps1` 起 mail 拿不到令牌**(与 §6.4 第 19 条资产密钥缺口同形),此时 `MailAdmin` 停用、`mail-smoke` 拿到 `PermissionDenied` —— 冒烟必须经 `start_game.ps1` 起栈,或先手工导出令牌。

以上计入 M1d 文件数(`k8s_deploy.ps1` 本就在清单内,`mail.yaml` 本就新建,增量是脚本内两段与 manifest 的 env 块,不新增文件)。若 §11 第 2 条选"M1 只为联调",这一整块连同 CLI 一起砍掉。

### 7.3 `etc/mail.yaml` 契约锚点(照 `go/friend/etc/friend.yaml`)

顶层 `Mode: dev`(M-9 与 `ROBOT` 发件方的放行依据;K8s staging / prod 被 ConfigMap 固定改写为 `pro`);顶层单行 `ListenOn: 127.0.0.1:50900`;`Timeout: 4000`;顶层唯一 `ZoneId: 1` 写在所有嵌套段之前、行尾不带注释;`LeaseTTL: 60`;顶层单行 `MetricsListenAddr: ":9240"`(注释列出 §4.6 / §4.7 的全部指标);`KillSwitchPrefix: ""`(注释写 `etcdctl put /mmorpg/killswitch/mailpb.ClientPlayerMail/'*' true` 用法);`Etcd.Key: ""`;**`Redis: { Host: 127.0.0.1:6379, Type: node, Key: "" }`**(SharedRedis,只读 `player:session:{id}`,不写 DB;照 `go/friend/etc/friend.yaml` 的 Redis 段注释);**`DataServiceRpc`** 段逐字照 `go/trade/etc/trade.yaml`(`Key: dataservice.rpc`、`NonBlock: true`、`Middlewares.Breaker: false`,连同"不违反 D-13"的注释);MySQL 结构化字段、`DBName: mmorpg_mail`(`config.Validate` 断言,并在 schemamigrate 连上后再断言 `SELECT DATABASE()`);DSN 带 `sql_mode=STRICT_TRANS_TABLES`;`Schema.AutoMigrate: true`(dev);`IdSegment` 段照 `go/trade/etc/trade.yaml`;`Kafka.Brokers` 留空合法(此时推送降级为不推,横幅显示);`Mail.*` 各上限;`Sweep` 段。**邮件正文是运营文本、不是玩家隐私**,但 `SendPersonalMail` 的收件人 + 正文组合仍不应整包进 stat 日志 —— 照 chat 配 `Middlewares.StatConf.IgnoreContentMethods` 列 `/mailpb.MailAdmin/SendPersonalMail`、`/mailpb.MailAdmin/SendSystemMail`(实际 FullMethodName 以生成物为准)。

### 7.4 其余登记

| 项 | 位置 | 说明 |
|---|---|---|
| `domain_meta` | `tools/proto_generator/protogen/etc/proto_gen.yaml` | 照 friend / trade 块:`mail: { source: "{{proto_dir}}mail/", rpc: { type: grpc }, outputs: { go: { proto: "go/generated/mail/proto", handler: "go/generated/mail/handler", grpc: "go/generated/mail/grpc" } } }`,不出 cpp。注释写明"未解析的域会丢号(DV-5)"。**`proto_directories` / `proto_dirs` 两处历史占位**:friend / trade 都不在这两处而只有 `domain_meta`;占位在目录不存在时对生成器有无副作用本轮未核实(§11 U7)。推荐**保留不动**(改它属于顺手整理;真要删单独一批) |
| 建库 | `deploy/mysql-init/00_init_zone_dbs.sql` | `CREATE DATABASE IF NOT EXISTS mmorpg_mail DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;` + ``GRANT ALL PRIVILEGES ON `mmorpg_mail`.* TO 'appuser'@'%';``(D-14 §5:**只登记这一处**)。存量卷不重跑 initdb,`deploy/k8s/README.md` 写手工步骤 |
| 号段 `biz_tag = mail` | 四处:`go/data_service/internal/config/config.go` `DefaultIdSegmentBootstrapTags`、`go/data_service/internal/store/id_segment_store.go` 同名包级变量、`go/data_service/etc/data_service.yaml` `IdSegment.BootstrapTags`、`tools/scripts/k8s_deploy.ps1` data-service ConfigMap | **以磁盘现值为基准追加到末尾**。本轮磁盘现值四处一致为 7 项 `[player, guild, item, txlog, snapshot, trade_listing, guild_asset_op]`(`guild_asset_op` 是帮会 B5a 加的、**未提交**;交接文档写的 6 项已过时)→ 追加后 8 项。四处一致性**没有任何测试守着**(测试比的是包级变量本身),落码后人工逐字比对。字符集 `^[a-z0-9_]{1,64}$`,`mail` 合法 |
| Tip 开段 | `data/tip/Tip.xlsx` 组头 + `cpp/generated/table/CMakeLists.txt` / `table.vcxproj` / `table.vcxproj.filters` | 三个工程文件**一起改**:`tip-code-axis.md` 正文只点名前两个,但同文 §6 记录过 `.filters` 漏改 friend / guild / match 六项。加的是导表器生成的 `mail_error_tip.pb.{h,cc}` |
| D-14 §9 | 设计文档声明 | 行不带 `zone_id` / `home_zone` → 不需要 merge_zone 步骤(§3.6) |
| D-14 §9 | `tools/data_consistency_check/main.go` | 加一项:`mail_system_state` 中 `sys_mail_id` 在 `mail_system` 里不存在的孤儿行计数(§4.2 的条件 upsert + §4.6 的删除顺序保证它应为 0) |
| D-14 §9 | TiDB BR 按库恢复清单(`deploy/k8s/README.md`) | 加 `mmorpg_mail` |
| D-14 §9 | audit auditor(`tools/merge_zone/audit_checks.go`) | **结论:不新增断言**。mail 的行不带 zone 列,合服不搬 mail 数据,merge audit 无可比对象。但不默认跳过:M1d 在 `deploy/k8s/README.md` 的 mail 段照 trade / friend 写一行"上线前未做 / 不适用:audit auditor(理由:无 zone 列、合服不搬)",让下一个人看得见这是结论不是遗漏 |
| 合服 | `tools/merge_zone` | **不改**。`MailAllData` 缓存键清单是旧占位的,与新服务无关(§1.3) |

---

## 8. 决策记录 M-1 … M-20

| # | 决策 | 理由 | 代价 |
|---|---|---|---|
| M-1 | 〔已拍板 §11-1〕M1 = 系统邮件 + 定向邮件的读 / 删 / 过期 + 领取短路;M2 = 领取;M3 = 帮会;M4 = 玩家互发 | 按解禁条件的远近排;M1 唯一的跨服务写依赖是号段 | 对交接文档 M2 编号有偏离(§1.4) |
| M-2 | "个人邮件"= **系统 / 运维发给指定一个玩家**,不是玩家发玩家 | 玩家互发缺审核 / 限频 / 拉黑三件套 | 客户端"玩家来信"场景 M1 没有 |
| M-3 | 〔已拍板 §11-2〕写入口只有内部 `MailAdmin`(两个方法),同 server、分文件、不标客户端开关 | 仓内零发信方,不自带入口 M1 无用;trade_admin 四重隔离先例 | 多一个 proto 文件、一个 admin server、一个 CLI |
| M-4 | 〔已拍板 §11-2〕鉴权 = 环境变量共享令牌,≥32 字节,空即停用,无 dev 放行 | M1 生产无附件,误用后果是一封信;data_service 先例 | 令牌泄露即可冒充官方发信;M2 必须升级 |
| M-5 | 幂等键 = 整数 `(sender_kind, request_id[, owner])` 唯一键 + 请求摘要 | 整数键避开 191 限长与 `ErrLegacyKeyColumn`;带 owner 让批量定向发放逐人幂等 | 占掉两张表各自唯一的 UNIQUE 名额;键寿命受 sweep 限制 |
| M-6 | 〔新号可见性已拍板 §11-6〕系统邮件读时合并 + 按需物化 state 行 | 没有玩家名册来源;写放大为 1 | 有效系统邮件必须有界(上限 + 强制过期);列表多一次查询 |
| M-7 | 〔已拍板 §11-14〕定向 / 系统分表,id 共用 `biz_tag = mail` | §11.6 字段判据;共号段保证合并排序与按 id 定位 | 读 / 删要先后查两张表(都是主键点查) |
| M-8 | 〔已拍板 §11-3〕附件 = 两个 repeated(mail 自有 `MailCurrency` / `MailItem`,与通道 message 逐字段对齐测试),存序列化 `AssetBundle` bytes;容量 = scene Credit 上限 + 货币白名单;发件时校验 | §11.6 实例层;一封 = 一条 op;库表不依赖通道协议;客户端不收 `common/asset` | 第二份形状 + 一处转换 + 对齐测试;config_id 存在性 M1 校验不了 |
| M-9 | **M1 生产禁发附件**,只在 `Mode=dev\|test` 允许(给 robot / 客户端验展示) | 领不到的附件到期作废 = 资产损失 | 运营 M1 不能用邮件发补偿 |
| M-10 | 带**未领且未过期**附件的邮件,服务端**拒删**(返回 `protected_mail_ids`,整请求仍成功) | 与客户端规则一致;M2 起防误删资产 | 领取未开放期间,这类邮件只能等过期(只在 dev 出现) |
| M-11 | 〔上限与超限策略已拍板 §11-5〕邮箱上限用"守卫行 + 现数",不存计数;超限拒收 | 无计数漂移失败模式 | 每次发件多一次 ≤100 行的 COUNT |
| M-12 | 状态列 uint32,取值唯一定义在 `mail.proto` 枚举 | friend 先例(未知值落 0 的问题) | 表注释要引用枚举,靠评审守 |
| M-13 | M2 起 mail 是 SYSTEM_CREDIT **唯一** seq 分配者(I6),GM 直发物 / 系统补偿 / rank 发奖都经 mail | I6 流独占;密钥一人一把 | 将来想要"不经邮件直接到账"的发物,只能另开新流 |
| M-14 | 〔已拍板 §11-4〕领取短路回 mail 段新码 `MailClaimNotOpen`,方法 M1 就定义 | 客户端已有 Claim 操作,可早接线、文案准确 | 提前占一个消息号 + 一行限频 |
| M-15 | 〔已拍板 §11-8〕推送只给定向新邮件;系统邮件不推 | 系统邮件推送要全服广播;入场拉取足够 | 在线玩家看到全服邮件要等下次拉取 |
| M-16 | 〔已定:guild / friend 两个先例一致,见 §11-11〕缺普通索引**升级为拒启**(Up / Plan 两模式),照 guild §6.4 与 friend 的 `missingIndexError`;OptionIndex 组只追加不调序 | schemamigrate 只按名字比索引、对存量表不补建普通索引;"缺索引只 WARN"会让慢查询静默上线 | friend 没这么做,两个先例冲突 —— mail 选 guild 的口径(§11 第 11 条请用户确认统一) |
| M-17 | 定向邮件**软删**(`deleted_ms`),物理删只归 sweep;所有读路径带 `deleted_ms=0` | 幂等唯一键在该行上,物理删 = 删键 = 重放重复投递(M2 重复发物)| 已删行留到 `expire + RetentionDays`;本人名下行数上界多一项(§3.4) |
| M-18 | 发件**先判重、后做依赖"现在"的校验**;定向邮件的判重在守卫锁内 | 已成功的请求重放必须回同一 id,不能被满箱 / 过期 / 上限拒 | 定向发件事务内多一次唯一键点查 |
| M-19 | 删除保护是**白名单**(`0`,或 `1` 且已过期);state 行只经"系统行存在且有效"的条件 upsert 建出,`claim_state` 按附件推导 | 2 / 4 永不因过期被删;杜绝孤儿 state 行与 M2 滚动升级的 `NONE` 误写 | upsert 语句更长;`RowsAffected=0` 要多一次点查区分 |
| M-20 | 系统邮件发出即可见,不做定时生效;`MailClaimState` 只定义 0 / 1,2–4 `reserved` | YAGNI;定时生效要在每条按 id 路径上补可见性判定,漏一处就泄露正文 | 需要时 ADD COLUMN / 追加枚举值 |

**另有照契约照做、不单独论证的**:`shared/noderegistry` 注册且 NodeInfo 的 `Endpoint` 与 `GrpcEndpoint` **双填**(只填前者路由服拨不通,guild 踩过);先 `Start` 再 `RegisterAfterListening`;`advertisedHost` 取值顺序 POD_IP → ListenOn 具体 host → `netx.InternalIp` → 127.0.0.1;`-migrate` / `-allow-modify` / `-version` 与 D-14 退出码照 `go/friend/friend.go`;横幅 `MAIL SERVICE STARTED SUCCESSFULLY` 是 `go_services.ps1` 的就绪判据;lifecycle 是 trade 那份的**第 5 个副本**(24s 硬截止 / 5s 排空),四份已有副本与它必须同改,定稿后该抽到 `go/shared`;失租口径 D-11 默认 `ReallocateNewID`(mail 不用 node_id 派生持久身份)。

---

## 9. 分批(AGENTS §10.2 的 30 文件门禁)

全量约 **71 个不同文件 / 77 次文件改动**(不含生成物;M1c 回改 M1b 已写出的 5 个装配文件,**回改计入本批**),比早期摸底的 46 多 —— 差额主要是 friend 实测的 lifecycle 5 个文件、`MailAdmin` 入口 + CLI、会话读取器、xlsx 补丁脚本、告警文件、令牌注入与数据一致性检查。拆成**四批**,每批 ≤ 30(交接文档估"2–3 批",按实数三批必有一批超 30,偏离登记于此)。若 §11 第 2 条选"M1 只为联调",M1c 减 `cmd/mailadmin` 与 admin 相关 2–3 个文件、M1d 减令牌注入脚本。

### 开工闸门(M1a 之前)

1. **friend 的全量 proto-gen 已落盘、编译通过**(交接文档 §4.0 / §4.5),`proto/message_id.txt` 已含 friend 那次的结果(本轮核实:已有 11 处 `ClientPlayerFriend`)。**mail 的 proto-gen 是独立的一次新运行**,不是"复用 friend 那次":存量消息号不变,mail 的新方法取新号;因此 gate(`rpc_event_registry`)、路由表、robot、Unity 生成物**必须全部取自 mail 这一次并同批发布**(§9.5),混用两次生成物不报错、只会错解包。
2. **✅ 拍板部分已满足(2026-09-20)**;U1 / U2 仍须在落码前核实。原条件:§11 的第 2、3、4、5、6、7、13 条已拍板(第 11 条已不需要拍板,见该条;它们决定 M1a 的 proto 形状、M1b 的 migrate 行为与端口;第 6 条若选"不能",要给 data_service 加 RPC 并入 M1a),**且 U1 / U2 已核实**(复合唯一键列序、单列 `OptionIndex` 写法 —— 决定 `mail_table.proto` 能否一次写对;表 proto 改一次就要再跑一次 proto-gen)。
3. 客户端门禁已有结论(§11 第 10 条)。**✅ 2026-09-20 用户已授权改客户端仓,M1a 同批往客户端 `gen_proto.ps1` 收录 `mail.proto` 与 `mail_error_tip.proto`**;下面是授权前写的约束,收录完成之前仍然成立:mail.proto 落地后,在客户端 `gen_proto.ps1` 收录它之前**不得**用默认 `proto_gen.yaml`(`enable_unity_client: true`)跑 proto-gen,否则生成引用不存在类型的 Mail 桩(CS0246)。改客户端仓需要单独授权。若第 3 条选"import `asset_op.proto`",客户端清单还要**额外**收录 `proto/common/asset/asset_op.proto`(客户端 `gen_proto.ps1` 现只收 `common/base` 与 `common/component`,已核实)。

### M1a —— 协议 / 表 / 段 / 登记(16 文件)

| # | 文件 |
|---|---|
| 1–3 | `proto/mail/mail.proto`、`mail_admin.proto`、`mail_table.proto`(新) |
| 4 | `tools/proto_generator/protogen/etc/proto_gen.yaml`(`domain_meta` 加 mail) |
| 5 | `tools/scripts/mail_xlsx_patch.py`(新;照 `friend_xlsx_patch.py`,两个子命令 `tip-segment` / `message-limiter`,幂等 + dry-run + 改前改后按段计数核对;MessageLimiter 按方法名从 `proto/message_id.txt` 查号) |
| 6 | `data/tip/Tip.xlsx`(由脚本 `tip-segment`:预留注释行 → 组头 + 8 码,`insert_rows`) |
| 7 | `data/MessageLimiter.xlsx`(由脚本 `message-limiter`:5 行,`tip_message=1000`;**必须在 proto-gen 之后**) |
| 8–10 | `cpp/generated/table/CMakeLists.txt`、`table.vcxproj`、`table.vcxproj.filters` |
| 11–14 | 号段四处 |
| 15 | `deploy/mysql-init/00_init_zone_dbs.sql` |
| 16 | `PROGRESS.md`(末尾追加) |

**用户执行序列**(全部在 M1a 验收内完成):`mail_xlsx_patch.py tip-segment` → 导表 → proto-gen → `mail_xlsx_patch.py message-limiter` → 第二次导表。MessageLimiter 的号在 proto-gen 之后即已确定,没有理由拖到 M1d 之后。

**验收**:导表 → `generated/code/proto/tip/mail_error_tip.proto` 恰 8 码落在 17000–17007、`segments.go` 出现 base=17000 段、`faults.go` 不含任何 17xxx、其它段码数与改前一致;全量 proto-gen → `go/generated/mail/*` 生成、`ClientPlayerMail_ServiceDesc.Methods` 恰 6 条、路由表 6 行 `ClientProtocol: true` 且目标 `MailNodeService`、`MailAdmin` 两个方法**不在**路由表或标 `ClientProtocol: false`;第二次导表后 MessageLimiter 恰多 5 行且号与 `message_id.txt` 一致;号段四处逐字相同 8 项。**C++ table 工程能编过**(验证 CMakeLists / vcxproj 改对)。

### M1b —— 服务骨架 + 存储(25 文件,依赖 M1a 的生成物)

`go/mail/`:`mail.go`、`mail_test.go`、`go.mod`、`go.sum`、`etc/mail.yaml`;`internal/config/{config.go,config_test.go}`;`internal/constants/{constants.go,constants_test.go}`;`internal/session/{session.go,session_test.go}`;`internal/lifecycle/{lifecycle.go,lifecycle_test.go,lifecycle_unix.go,lifecycle_unix_test.go,lifecycle_windows.go}`;`internal/metrics/metrics.go`;`internal/svc/servicecontext.go`;`internal/data/{tables.go,mail_repo.go,mail_repo_mysql_test.go,sweep_repo.go,sweep_repo_test.go,session_reader.go,session_reader_test.go}`(会话读取器照 friend `internal/data/session_reader.go`,键名常量前缀拼接,测试与写者共用前缀)。

`go.mod` 必须带与 `go/schemamigrate` 逐字相同的 `replace github.com/luyuancpp/proto2mysql v0.1.1 => github.com/luyuan-cpp/proto2mysql v0.1.1`(D-14 §7:replace 只在主模块生效)。

**验收(限定)**:`go build` / `vet` / `test` 通过;`-migrate` 在空库建出恰好 4 张表、唯一键 2 个;真 MySQL 8.4 下 repo 集成测试 PASS(见 §10 ④)。**不含**"常驻启动 + 横幅 + etcd 双填"—— 此时还没有 server,横幅里的 `mail_admin=` 行无从验证,挪到 M1c。

### M1c —— 业务逻辑 / 入口 / 推送 / CLI(18 文件,其中 5 个回改)

新文件:`internal/logic/{mail_logic.go,mail_logic_test.go,admin_logic.go,admin_logic_test.go,sweep.go,sweep_test.go,push.go}`;`internal/kafka/gate_command_builder.go`;`internal/server/{mail_server.go,admin_server.go,admin_server_test.go,inband_observability_test.go}`;`cmd/mailadmin/main.go`。
**回改(M1b 写出、本批必须改)**:`mail.go`(注册 `ClientPlayerMail` 与 `MailAdmin` 两个 server、接 `StartSweep`、横幅加 `mail_admin=` / `sweep` 行)、`internal/metrics/metrics.go`(§2.5 / §4.5 / §4.6 的指标并预建 0 值序列)、`internal/svc/servicecontext.go`(Kafka 推送客户端、SharedRedis 句柄)、`internal/config/{config.go,config_test.go}`(Mode 闸门、令牌读取、`RetentionDays ≥ 1`、拒 `Redis.Type: cluster`)。照 friend F3"`friend.go` 接上 `logic.StartSweep`(F2 写出但无调用方)"的先例。

**验收**:单测覆盖 §4.2 每条的幂等与失败语义(含"先判重后校验")、短路不触库(假 store 断言零调用)、令牌空 / 短 / 错 / 对四态、客户端来源调 `MailAdmin` 被拒、Mode=pro 四态(§10 ⑥b);**常驻启动 + 横幅 + etcd 双填**(§10 ⑤)。

### M1d —— 部署链 / 冒烟 / 文档(18 文件)

`go_services.ps1`、`go_svc_image.ps1`、`k8s_deploy.ps1`(含 `mail-admin-auth` Secret 与 `Mode` / SharedRedis / `DataServiceRpc` 等 ConfigMap 键,§7.2)、`start_game.ps1`、`tests/k8s_migrate_gate.tests.ps1`、令牌本机注入(新 `tools/scripts/lib/mail_admin_dev_token.ps1`,或把 `assetop_dev_secret.ps1` 泛化 —— 后者会动资产通道的脚本,推荐前者)、`deploy/k8s/manifests/go-svc/{mail.yaml,mail-migrate.yaml}`、`deploy/k8s/mail-alerts.yaml`(§4.7)、`deploy/k8s/README.md`(含 audit auditor 结论行、BR 清单、存量卷手工建库)、`robot/{mail_smoke_scenario.go,etc/mail_smoke.yaml,config.go,main.go}`、`tools/data_consistency_check/main.go`、契约 §7(补 9230 / 9240)、`PROGRESS.md`、本文(回填实测)。

**验收**:见 §10 第 ⑦–⑨ 步。

**依赖顺序**:M1a →(用户:tip-segment → 导表 → proto-gen → message-limiter → 第二次导表)→ M1b → M1c → M1d。M1b 与 M1c 不能并行(logic 依赖 repo 接口)。**多会话纪律**:`go_services.ps1` / `k8s_deploy.ps1` / `start_game.ps1` / `PROGRESS.md` / 号段四处都是多会话同写的文件,显式路径提交,PowerShell 改完用真解析器检查括号(交接文档 §5.1–§5.3)。

### 9.5 发布顺序与回滚(评审第 13 / 19 条)

照 friend-port §8 与契约 §3("新消息号要重编 gate,路由服先、gate 后")、D-12。

**顺序固定**(前一步没完成不做下一步):

1. **data_service**(号段四处改为 8 项)重编 → 跑 `data_service -f <yaml> -migrate`,确认 `SELECT biz_tag FROM id_segment` 出现 `mail` 行。生产形态下缺行时 data_service 拒发(`id_segment_store.go` 的 `ErrIdSegmentUnknownTag`,不自动补种),mail 首次取号即 `kServiceUnavailable`。
2. **`mail-migrate` Job Complete**。已初始化的卷 / PVC 不会重跑 initdb:先手工执行 `00_init_zone_dbs.sql` 里 `mmorpg_mail` 的两句(建库 + GRANT),否则 Job 以 1 退出(`deploy/k8s/README.md` 写手工步骤)。
3. **mail Deployment**。Tip.xlsx 的 mail 段、导表产物与 mail 二进制**同批**发布:码不在表里时 `serverbase` 判 `VerdictUnknown`。
4. **`client_rpc_router`**(出自 mail 那次 proto-gen 的路由表)。
5. **gate**:出自 mail 那次 proto-gen 的 `rpc_event_registry`,串行 `msbuild … /m:1` 重编;第二次导表产出的 MessageLimiter 与 gate 同批(或 gate 重载表)。gate、路由表、robot、Unity 必须出自**同一次** proto-gen、同批发布。

**回滚**:

- **止血**用 killswitch:`etcdctl put /mmorpg/killswitch/mailpb.ClientPlayerMail/'*' true`(恢复:`etcdctl del` 同 key)。
- **二进制可回滚,schema 不回滚**:表只追加(ADD COLUMN / 新表),旧二进制读新表兼容。
- `MailAdmin` 止血:撤掉 `MMORPG_MAIL_ADMIN_TOKEN`(K8s 上删 `mail-admin-auth` Secret)后重启 —— 空即停用。
- **回退 gate 直连** = mail 与 chat / guild / trade / friend **同时**不可达:先 killswitch 关方法,再发公告(D-12)。

---

## 10. 验证清单

**执行顺序是硬约束**,前一步没过不跑下一步。每条"通过标准"都要在报告里写**看到了什么**,不是"命令退出 0"。

| # | 步骤 | 通过标准 | ⚠ 假绿陷阱 |
|---|---|---|---|
| ① | 导表 | 8 个 mail 码生成于 17000–17007,枚举名与 `constants.go` 逐字一致;`faults.go` 不含 17xxx | 导表器在 xlsx 被别的会话整表覆盖后**照样成功**,只是少了行 —— 必须数码 |
| ② | proto-gen | 同 M1a 验收;`git diff --stat` 里客户端 / robot 只多 Mail 的桩;gate `rpc_event_registry`、路由表、robot、Unity 出自**同一次**运行 | 用旧的 `proto-gen.exe` 跑(交接文档 9.4 的 22:10 事故:build 失败脚本仍以 0 退出)—— 先确认生成器是本次新建的 |
| ③ | `go/mail` 静态与单测 | `gofmt -l` 为空;`go mod tidy` 后无意外直接依赖;`build` / `vet` / `test` 通过 | 全体 SKIP 时 `go test` 仍退出 0 |
| ④ | **真 MySQL 8.4 集成**(`MAIL_TEST_MYSQL_DSN` 门控 + `MAIL_REQUIRE_MYSQL_TESTS` 让"没给 DSN"变 FAIL,照 friend) | 报告明写"看到 PASS 而不是 SKIP"。必过:同一收件人 20 路并发 `SendPersonalMail` 在上限 5 时恰好成功 5 封、无 1213;同键重放返回同一 `mail_id`;同键异摘要回 `MailRequestConflict`;删 / 读重复调用成功;带未领附件被保护。**评审回修新增(每条都要能红)**:① 发件 → 收件人 `DeleteMails` → 同键重放:返回同一 `mail_id`、`duplicated=true`、表里不多一行;② 首发成功 → 填满邮箱 → 同键重放:`duplicated=true`、同一 `mail_id`(不是 `BoxFull`);③ 首发成功 → 把注入的时钟推到 `expire_ms` 之后 → 同键重放:`duplicated=true`;④ 100 封全部已过期、sweep=`report_only` 时第 101 封发送成功;⑤ 手工写入 `claim_state=1` 且已过期的行可删,`claim_state` 为未知值(模拟 M2 的 2 / 4)且已过期的行 `DeleteMails` 后仍在、出现在 `protected_mail_ids`;⑥ 系统邮件已过期时 `ReadMail` 回 `MailExpired` 且 `mail_system_state` 无行;⑦ 带附件的系统邮件被 `ReadMail` 物化后 state 行 `claim_state=1`;⑧ sweep 先 state 后系统行 | **TiDB 当 MySQL**:TiDB 没有间隙锁,RC / 锁序相关用例在它上面恒绿 —— 必须 MySQL 8.4 |
| ④b | `EXPLAIN` | 定向列表查询 `key = idx_mail_personal_0`、无 `Using filesort`;系统有效集查询(`ORDER BY sys_mail_id DESC LIMIT`)**允许** filesort —— 行数被有效集规模(≤ MaxActive 量级)限住;sweep 候选走 `idx_mail_personal_1` range;`mail_system_state` 按 `sys_mail_id` 删走 `idx_mail_system_state_0` | 小表上优化器可能选全表扫也很快 —— 看 `key` 列,不看耗时 |
| ④c | data_service 号段 | data_service 重编并跑 `-migrate` 后,`SELECT biz_tag FROM id_segment` 能查到 `mail`;**反向**:缺这一行时常驻 mail 的第一次发件回 `kServiceUnavailable`(不静默走任何回退) | 本机 dev 若开了运行时自动补种,缺行也能发 —— 以生产形态配置验证 |
| ⑤ | 空库 `-migrate` + 常驻启动 | 退出 0、恰好 4 表、主键全整数、唯一键恰 2 个(`uk_mail_personal` / `uk_mail_system`);**再跑一次 0 条语句**;删掉一条普通索引后启动:**按 §11 第 11 条的拍板结果** —— 拒启则被拒,WARN 则启动成功但日志恰有该索引名的 WARN(两种写法都能红);横幅恰有 `  MAIL SERVICE STARTED SUCCESSFULLY`、mysql 目标不含密码、`mail_admin=enabled/disabled` 与令牌状态一致、`sweep mode=report_only`;`:9240/metrics` 上 §2.5 / §4.5 / §4.6 的指标都有预建 0 值序列;`RetentionDays: 0` 的 yaml 被 `Validate` 拒启;etcd 值里 `endpoint` 与 `grpcEndpoint` 都填;Ctrl+C 先注销再排空、<24s | "再跑一次 0 条语句"若在同一进程内复用缓存,证明不了幂等 —— 两次独立进程 |
| ⑥ | 令牌四态 | 不设 / 31 字节 / 错值 / 对值 → 前三者 `PermissionDenied`,日志**不出现令牌值** | 只测"对值成功"证明不了 fail-closed |
| ⑥b | **Mode=pro 四态**(M-9 是 M1 唯一的防资产损失闸门) | pro + 附件 → `MailAttachmentNotOpen`,表中无新行、**号段无消耗**;pro + `sender_kind=ROBOT` → `kInvalidParameter`;dev + 附件 → 成功且 `claim_state=1`;pro + 无附件 `OPS_TOOL` → 成功 | 只在 dev 跑冒烟,pro 分支零覆盖 —— 必须单独起一个 `Mode: pro` 的进程或用 config 注入的单测 |
| ⑦ | 两区 robot `mail-smoke`(两区 gate 都 `GATE_CLIENT_RPC_ROUTER=1`、`mmorpg_mail` 已建;**必须经 `start_game.ps1` 起栈或先手工导出令牌**,否则 `MailAdmin` 停用、冒烟拿到 `PermissionDenied`) | 输出一行 `MAIL_SMOKE_OK …`。逐步:CLI / robot 发一封系统 + 一封无附件定向 + **一封带 1 条货币附件的定向(dev)**;两区玩家都能在 `ListMails` 看到系统邮件,只有收件人看到定向邮件;带附件那封在 `ListMails` 里 `claim_state=UNCLAIMED`、`claimable_count=1`;定向邮件推送在 10s 内到达(**弱断言**:丢了不算失败,但要打印是否收到);`ReadMail` 后 `unread_count` 减 1;另一玩家 `ReadMail` 别人的定向邮件回 `MailNotFound`;`ClaimMailAttachments` 回 `MailClaimNotOpen`,**再 `ListMails` 该封仍是 `UNCLAIMED`**;`DeleteMails` 带附件那封 → 出现在 `protected_mail_ids`、`deleted_count=0`;`DeleteMails` 无附件那封删两次都成功,且之后 `ListMails` 看不到;**D-9 反向断言**:客户端发 `NotifyMailEvent` 与 `MailAdmin` 的消息号拿不到业务回包,**并在同窗口 mail 日志里看到拒绝记录**(信封不保留原始 gRPC code,只看机器人失败不能证明原因) | **冒烟数据集覆盖不到的断言**:冒烟里系统邮件只有 1 封,"有效集 LIMIT 截断"与"跨两表合并分页"在它上面恒绿 —— 这两条只能靠 ④ 的集成测试(构造 >page_size 的混合数据) |
| ⑧ | 过期 sweep | API 造不出已过期的信(发件要求 `sent_ms < expire_ms`),所以**用 SQL 直接 INSERT 两行**:A `expire_ms = now − (RetentionDays+1) 天`、B `expire_ms = now − 1 天`,**`RetentionDays` 保持默认 7 不变**(不放开 0)。`report_only` 下 A、B 都在,`mail_sweep_candidate_rows` = 1,`mail_sweep_last_run_unixtime` 前进;切 `delete` 后只删 A、B 保留;`data_consistency_check` 孤儿数为 0 | 三种结果(都在 / 只删 A / 都删)必须可区分;`cutoff ≤ 0` 分支"什么都不做"会让"没删"看起来像"report_only 生效" —— 看日志里的模式与 cutoff 值 |
| ⑨ | K8s(kind) | 按 §9.5 的顺序:data_service `-migrate` 后 `id_segment` 有 `mail` 行 → `mail-migrate` Job Complete 后 Deployment 才 apply;ConfigMap 里 `Etcd.Key: ""`、`Mode: pro`(staging/prod 档)、`Redis`、`DataServiceRpc`、`IgnoreContentMethods`、Sweep 四键都存在;未注入令牌时横幅恰为 `mail_admin=disabled`,**不期望能发信**;Pod 不 CrashLoop;**不期望**推送到达(g1/g2 存量缺口,§4.5);K8s 上 gate 默认直连,**玩家不可达是设计内的**(D-12) | 把"K8s 上推送没到 / 玩家到不了"当 mail 的缺陷去修 |

**失败时保留**:导表 / 生成日志;`go test -v` 全文;`SHOW ENGINE INNODB STATUS`;启动日志全文;robot 输出 + 两区 gate / 路由服 / mail 日志;`EXPLAIN` 原文。

---

## 11. 拍板记录 / 未核实项

### 11.1 拍板记录(2026-09-20,用户确认;原问题与推荐理由见 git 历史中本节的上一版)

| # | 问题 | 结论 |
|---|---|---|
| 1 | M2 / M3 编号 | **领取排 M2、帮会邮件排 M3**(与交接文档 §4.1 的编号对调;领取的解禁条件更近,rank 发奖依赖它) |
| 2 | 发件入口形态(§2.2) | **`MailAdmin` 两个内部方法 + CLI + robot,鉴权为环境变量共享令牌**(K8s 经 `Initialize-InjectedSecrets` → `mail-admin-auth` Secret → `secretKeyRef optional: true` 注入)。用户明确要"M1 上线后运维就能定向发文本信",所以**不走**"只做 dev 种子入口"的备选;代价是 M2 换 HMAC 时这套令牌可能要重做 |
| 3 | 附件协议字段 | **mail 自有 `MailCurrency{currency_type,amount}` / `MailItem{config_id,count}`**,Go 侧一处转换成 `AssetBundle` + 逐字段对齐测试;不 import `asset_op.proto`,不把内部 schema 带进客户端 |
| 4 | `ClaimMailAttachments` 是否 M1 就定义 | **M1 定义,调用直接回 `MailClaimNotOpen`** |
| 5 | 定向邮件上限与超限策略 | **100 封,超限拒收回 `MailRecipientBoxFull`** |
| 6 | 新角色能否看到建号前的全服邮件 | **M1 能**。仅在"无附件"前提下成立(M-9 生产禁附件);M2 带附件前必须按 §6.3 第 14b 条重评(见第 18 条) |
| 7 | 写操作响应是否带列表快照 | **不带**,客户端写后再拉一次 |
| 8 | 系统邮件是否推送 | **M1 不推** |
| 9 | 系统邮件撤回 | **M1 不做**,契约写明"不可撤回";误发应急 = 运维手工 SQL 改 `expire_ms`,写进 runbook |
| 10 | 客户端门禁 | **授权改客户端仓**:M1a 时往客户端 `gen_proto.ps1` 收录 `mail.proto`(及 `mail_error_tip.proto`,照 trade / team / friend 成对收录),mail 的 proto-gen 一次性连客户端桩一起出 |
| 11 | 缺索引拒启的全仓口径 | **不再需要拍板**:guild 与 friend(2026-09-20 续做,未提交)都已是"缺索引拒启 + `-migrate` 以 4 退出",mail 照做 |
| 12 | M2 背包满长期挂起 | **按推荐:留到 M2 设计时拍**,M1 只保证状态列能表达 |
| 13 | 端口 | **50900 / `:9240`**;zone 2 落保留区由 `Resolve-BindablePort` 兜底,与 trade 同口径 |
| 14 | 号段 | **`biz_tag = mail`**;`mail_asset_op` 到 M2 才加 |
| 15 | 客户端分类映射 | **协议只给 `kind`**,客户端改为"全部 / 系统 / 个人 / 有附件"四个筛选 |
| 16 | 旧 `MailEntry` / `MailAllData` | **单开一个清理小批**,不在 M1 四批内 |
| 17 | 货币类型白名单来源 | **M1 用 `Mail.AllowedCurrencyTypes: [0,1,2]`**(注释指向 `currency.h`);M2 解禁附件前单开小批把 `CurrencyType` 提升为 proto enum |
| 18 | M2 新号领取系统邮件附件的资格 | **按推荐倾向:"系统邮件强制不带附件、全服补偿走定向批量"**,M2 开工前定稿;M1 不受影响 |

**开工闸门(与拍板无关,仍然成立)**:M1a 排在 friend 的全量 proto-gen 与编译通过之后(§9 开工闸门第 1 条);U1 / U2 两条未核实项(§11.2)落码前仍要核。

### 11.2 未核实(落码前自己核)

- **U1**:复合唯一键(3 列)的列序是否被 proto2mysql 原样保留(option 名 `OptionUniqueKey` 已核实)—— 对照 guild `guild_db.proto` 的 `uk_guild` 与 proto2mysql v0.1.1 源码。
- **U2**:`OptionIndex` 在单列时的写法(`"expire_ms"`)与 friend 多列写法同一语法 —— 以 friend / trade 表为准。
- ~~**U3**~~ **已核实**(评审第 11 条):`GetPlayerHomeZone` 无映射 → gRPC `NotFound`(文案 `no home zone mapping` 是滚动升级契约),Redis 故障 → `Unavailable`。它只证明"有 home zone 映射",不等于"玩家存在"。结论并入 §4.2 末段。
- **U4**:gate **下行**单包上限(决定 `ListMails` page_size 上限与文本长度上限)。
- ~~**U5**~~ **已核实**:`tip_message = 1000` = `kRateLimitExceeded`(`tools/scripts/friend_xlsx_patch.py` 的 `LIMITER_TIP_MESSAGE` 及其注释)。
- ~~**U6**~~ **已核实并定稿**(评审第 16 条):K8s 没有给 Go 服务注入 Secret 的现成机制,做法见 §7.2 "令牌注入"。
- **U7**:`proto_gen.yaml` 里 `mail/` 两处占位在 `proto/mail` 不存在时对生成器有无副作用;`proto/mail` 建出来之后这两处与新 `domain_meta` 是否会让 mail 被处理两次。
- **U8**:`Tip.xlsx` 里 `mail_error 17000` 预留注释行的原文(本轮沿用交接文档转述,未打开 xlsx);`grant_error 24000` 预留段是否本来就是给"系统发物 / 邮件附件"的独立域 —— 若是,M2 的发物类码要不要放那段,需要问预留它的人。
- **U9**:TiDB 下 `GET_LOCK` / `KILL QUERY` 语义(`schemamigrate.go`"待验证"段),mail 继承同一风险。
- **U10**:scene 是否有任何 M1 就需要发邮件的业务(活动排期结算、战斗结算奖励)—— 本轮只核实了任务领奖是同步直写背包,`player_activity_schedule.cpp` 与战斗结算的发奖出口未逐一核实。

### 11.3 文档不一致(本轮只列,不改)

- `docs/design/jubaozhai-market.md` J-7 写"17000–19999 留给 mail / chat / rank",而 `Tip.xlsx` 把 rank 钉在 21000。**以 Tip.xlsx 为准**(导表器的输入)。
- 契约 §7 指标端口分工表缺 `9230 trade`(M1d 补)。
- 交接文档 §4.1 "Unity 客户端 mail M1 未开工"不准确:UI / 状态机 / 测试已在客户端仓,缺网络层(§1.1)。
- 交接文档 §4.1 号段现值写 6 项,磁盘已是 7 项(含未提交的 `guild_asset_op`)。
- `go/trade/internal/svc/assetchannel.go` 注释说资产密钥"由 go_services.ps1 注入",与磁盘不符(§6.4 第 19 条)。

---

## 评审记录(2026-09-20,三视角评审 → 回修)

**做法**:correctness / ops / feasibility 三个视角共提 27 条。回修人先通读全文,再逐条对照磁盘核实(默认发现可能是错的),成立的就地改正文,不成立或部分成立的在此说明。本轮只改本文件;未运行任何构建 / 测试 / 生成命令。

**核实过的磁盘证据**:`go/friend/internal/data/session_reader.go`、`internal/svc/servicecontext.go`(SharedRedis)、`internal/logic/push.go`、`etc/friend.yaml`(Redis 段、`KillSwitchPrefix`、sweep 指标);`go/data_service/internal/server/dataserviceserver.go` 的 `GetPlayerHomeZone`;`go/data_service/internal/store/id_segment_store.go` 的 `ErrIdSegmentUnknownTag`;`cpp/libs/modules/currency/constants/currency.h` 与 `asset_op_system.cpp` 的 `IsCurrencyAmountValid`;`go/trade/etc/trade.yaml` 的 `DataServiceRpc` 与 `Mode`;`k8s_deploy.ps1` 的 trade / friend `Mode`、Sweep 四键注释、`redis-auth` Secret;`deploy/k8s` 下 `secretKeyRef` 唯一出现在 `infra/redis.yaml`;`release_common.ps1` 的 `Resolve-InjectedSecret`;`deploy/k8s/README.md` 的 audit auditor 行(trade / friend);`deploy/k8s/scene-manager-alerts.yaml`;`tools/merge_zone/audit_checks.go`;`tools/scripts/friend_xlsx_patch.py`;`proto/message_id.txt`(已有 11 处 `ClientPlayerFriend`);客户端 `mmorpg-client/tools/gen_proto.ps1`(只读);契约 §3 / §5 / §7 与 friend-port §8。

**结果**:27 条中 **25 条确认并改正文**,**2 条部分采纳**(另有 2 组重复,合并处理:第 1 / 18 条同为"物理删丢幂等键",第 13 / 19 条同为"缺发布顺序")。

| # | 结论 | 落点 |
|---|---|---|
| 1 / 18 | 确认。定向邮件改软删 `deleted_ms = 15`,物理删只归 sweep;幂等键最短寿命 = expire + RetentionDays | §1.2、§2.4、§3.3、§3.6、§4.2、§4.6、M-17 |
| 2 | 确认。先判重、后做依赖"现在"的校验;定向判重放进守卫锁内 | §2.4、§4.2、§5.3、M-18、§10 ④ |
| 3 | 确认。现数带 `deleted_ms=0 AND expire_ms>now` | §3.4、§10 ④ |
| 4 / 14(b) | 确认。推送前经 SharedRedis 读 `player:session:{id}`,只推 ONLINE;M1b 加 `session_reader` | §4.1、§4.5、§7.2、§7.3、§9 |
| 5 | 确认。删除判定改白名单,2 / 4 永远受保护 | §4.2、M-19、§10 ④ |
| 6 | **部分采纳**:问题成立,但修法改为**删掉 `visible_from_ms`**(配合第 26 条的 YAGNI)—— 没有定时生效,"未生效就能读正文"的整类问题不存在;可见性统一为 `expire_ms > now` | §1.3、§3.6、§4.2、§5.3、M-20 |
| 7 | 确认。两张表都在建行时写推导值,`0` 只表示无附件 | §3.3、§3.6、§4.2 |
| 8 | 确认。`RetentionDays ≥ 1`;state 行只经"系统行存在且有效"的条件 upsert 建出;补了 `RowsAffected=0` 的歧义处理 | §4.2、§4.6 |
| 9 | 确认。`ORDER BY sys_mail_id DESC LIMIT`,④b 允许 filesort | §3.1、§4.2、§10 ④b |
| 10 | 确认。货币白名单 `[0,1,2]`,M2 前换唯一来源 | §3.5、§6.3 第 14a 条、§11 第 17 条 |
| 11 | 确认。U3 已核实 | §4.2、§11.2 |
| 12 | 确认。M2 新号领取资格二选一 | §1.3、§6.3 第 14b 条、§11 第 6 / 18 条 |
| 13 / 19 | 确认。新增 §9.5 发布顺序与回滚;闸门第 1 条改写("mail 的 proto-gen 是独立一次"),⑤ 前加 ④c 号段检查 | §9 闸门、§9.5、§10 ④c / ⑨ |
| 14 | 确认。ConfigMap / yaml 补 `DataServiceRpc`、共享 Redis、`Mode`、`IgnoreContentMethods`、`KillSwitchPrefix`、Sweep 四键、`Mail.*` | §7.2 ③、§7.3 |
| 15 | 确认。sweep 四个指标 + `mail_admin_send_total` + `mail-alerts.yaml` 三条告警 | §2.5、§4.6、§4.7、§9 M1d、§10 ⑤ |
| 16 | 确认。U6 定稿为 Secret + `secretKeyRef optional` | §7.2、§10 ⑦ / ⑨、§11.2 |
| 17 | 确认。audit auditor 结论"不新增断言",但在 README 显式写 | §7.4 |
| 20 | 确认。决策表标〔待拍板〕;闸门第 2 条补 6 / 7 / 11 与 U1 / U2;⑤ 的缺索引断言按拍板结果两写 | §8、§9 闸门、§10 ⑤ |
| 21 | 确认。Mode 闸门在取号前;新增 ⑥b 四态 | §4.2、§5.3、§7.2、§7.3、§10 ⑥b |
| 22 | 确认。新增 §1.5;§11 第 2 条权衡带上此事实 | §1.5、§2.1、§11 第 2 条 |
| 23 | 确认。`mail_xlsx_patch.py` 两个子命令;MessageLimiter 挪进 M1a | §5.5、§5.6、§9 M1a / M1d |
| 24 | 确认。M1c 计入 5 个回改文件;M1b 验收收窄 | §9 |
| 25 | 确认。⑦ 改带附件的定向邮件;⑧ 用 SQL 造两行、RetentionDays 保持 7 | §10 ⑦ / ⑧ |
| 26 | **部分采纳**:`MailClaimState` 只保留 0 / 1 + `reserved 2 to 4` ✔;删 `mail_system.visible_from_ms` ✔;删 §3.5 的"补不回附件"理由 ✔。**驳回删 `mail_system_state.claim_state`**:第 7 条的滚动升级问题恰恰需要 M1 就把推导值写进这一列;M2 再 ADD COLUMN 时,旧 M1 pod 物化的行默认 0,就是第 7 条的 bug 原样重现 | §1.3、§3.3、§3.5、§5.2、M-20 |
| 27 | 确认,细节更正:客户端 `gen_proto.ps1` 收 `common/base` **与 `common/component`**(评审说"只有 base"不准确),但结论(不含 `common/asset`)成立。据此把 §11 第 3 条的**推荐改为 mail 自有 message** | §3.5、§5.2、M-8、§9 闸门第 3 条、§11 第 3 条 |

**会让接手人做错事的更正(务必看)**:

1. **定向邮件不是物理删**。照早稿写 `DELETE FROM mail_personal` 会让幂等键随删信消失,M2 起就是重复发物。
2. **发件先判重**。照早稿"先校验后插入、1062 再读回",已成功的请求重放会被满箱 / 过期拒,调用方会误补发。
3. **state 行的 `claim_state` M1 就要写推导值**,不是"恒 0";且 state 行只能用 `INSERT … SELECT … FROM mail_system WHERE … AND expire_ms > ?` 建。
4. **推送要先读 SharedRedis 的 `player:session:{id}`**;mail.yaml / ConfigMap 必须有共享 Redis 段、`DataServiceRpc` 段与 `Mode`,缺前两者分别是"推不出去"与"领不到号、发件全失败"。
5. **mail 的 proto-gen 是独立一次**,gate / 路由表 / robot / Unity 必须全部出自这一次;data_service 要先重编并 `-migrate` 出 `mail` 号段行。
6. **附件协议推荐已从"import asset_op.proto"改为 mail 自有 message**(客户端不收 `common/asset` 已核实)。
7. **K8s 上默认 `MailAdmin` 停用、玩家不可达**,都是设计内的;冒烟必须经 `start_game.ps1` 起栈。

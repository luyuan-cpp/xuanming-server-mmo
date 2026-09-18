> 说明:这是设计起草阶段的全局命名契约,**其中被各节偏差与 `90-consistency.md` 覆盖的条目以后者为准**(见 90 清单 G-11)。

# 帮会二期全局契约(所有分节设计必须遵守;有问题写进"契约偏差"而不是私自改名)

日期 2026-09-15。用户拍板见各节开头的"用户决策"。项目未上线:无存量数据、无迁移/版本字段。

## 0. 用户决策(绑定)

1. 银两 = `kCurrencyGold`(客户端"金币"改名"银两"),灵石 = `kCurrencyDiamond`。不新增货币类型。建设物资(道具捐献)放 v1.1,本期界面保持"暂未开放"。
2. 帮会先做**通用**资产通道(见 §4),聚宝斋日后复用;`jubaozhai-market.md` §6.4 的 `Trade*` RPC 改为本契约的通用名。
3. 活动在帮会界面参与:元宵灯会(每人每游戏日点灯一次得帮贡;全帮当期点灯数达阈值,帮会得资金,每期一次)、中秋团圆(帮会同时在线成员数达阈值后,成员每游戏日各领一次奖)、同道历练(帮会成员组队打指定副本,胜利得帮贡与帮会资金)。无地图交互。
4. 帮会全部表迁入独立库 `mmorpg_guild`,proto 为源,`go/schemamigrate` 迁移(照 go/trade),修订 D-14 §8。
5. 默认值(用户未反对):名字全服唯一、2–12 字、建角必填、空名服务端生成;帮主可任免长老、踢任何人、转让;长老可审批申请、踢普通成员;长老上限随帮会等级;申请 72h 过期;每人最多同时 3 个待审申请。**所有数值进配表**。

## 1. 批次(每批手改文件 ≤30,每批开工前需用户授权)

| 批次 | 内容 | 依赖 |
|---|---|---|
| B1 | 存储迁库:`proto/guild/guild_db.proto` 表 message、go/guild 接 schemamigrate(`-migrate` + dev AutoMigrate)、`00_init_zone_dbs.sql` 建库授权、`guild_friend_tables.sql` 删帮会表、merge_zone guild_step 改库、guild zrpc Timeout ≤4000 + config 校验、测试 schema 助手 | — |
| B2 | 管理与审批 + 推送 NotifyGuildChanged + 客户端成员操作/申请列表 + robot | B1 |
| B3a | 名字服务端:data_service 名字注册表 + login 建角 + scene `PlayerProfileComp` + Java 网关解析 | — |
| B3b | 名字客户端建角界面 + 帮会成员显示名字(guild 批量查) | B3a、B2 |
| B4a | 通用资产通道 C++:账本组件、SceneNodeGrpc 三个 RPC、Currency/Bag 带 tx+correlation、存盘确认、GM 货币指令鉴权 | — |
| B4b | 通用资产通道 Go:`go/shared/scenenode`(位置查询 + 节点发现 + 拨号)、`go/shared/assetop`(outbox 行形状 + 重投循环骨架)、jubaozhai 文档改名 | B4a proto |
| B5 | 捐献 / 帮会升级 / 帮会商店(配表 + 逻辑 + 客户端两页) | B1、B2、B4a、B4b |
| B6a | 元宵灯会 / 中秋团圆 | B1、B5(资金/帮贡列)、B4(物品奖励) |
| B6b | 同道历练 | B6a、match 侧上下文字段(与组队会话协调) |

## 2. 共用基础

- **游戏日**:新包 `go/shared/gameday`。时区固定 UTC+8(`time.FixedZone`,不依赖 tzdata),每日 05:00 重置。`DayKey(t) uint32` 返回 `YYYYMMDD`(按 t−5h 的 UTC+8 日期);`WeekKey(t) uint32` 返回 ISO 年×100+周。时钟一律取服务进程时钟(单调时钟只用于超时预算),文档写明各副本 NTP 偏差容忍。
- **时间戳**:`uint64` 毫秒。
- **帮会角色**:0=成员、1=长老、3=帮主(D-4,跳过 2)。新增 `constants.Rank(role) int`,所有权限比较比 rank,不比原值。
- **zrpc 预算**:guild `Timeout` ≤ 4000(路由服 ForwardTimeoutMs 5000 − 1000),`config.Validate` 强制;任何同步跨服务调用(scene 资产 RPC、data_service)必须在 3000ms 内结束。
- **推送**:单一 `rpc NotifyGuildChanged(GuildChangedS2C) returns (Empty)`,占位实现,**不进** `session.ClientMethods`;发送走 `kafkautil.PushToPlayer`/`BroadcastToPlayers`,至多一次;客户端收到只触发拉取(`GetPlayerGuild` 等),拉取是真相。
- **所有写 RPC** 成功时回带刷新后的 `GuildInfo`(或对应视图),客户端不依赖推送。
- **合服闸门**:所有写 RPC(建帮、申请、审批、踢人、转让、任免、捐献、升级、购买、活动参与)都查 `merge:in_progress:{zone}`,失败回 `kGuildZoneMerging`(替换现有 FailedPrecondition)。

## 3. 协议(proto)

### 3.1 `proto/guild/guild.proto`(GuildService,客户端可达,除注明外全部进 ClientMethods)

删除:`JoinGuild`(改为申请制)。保留:CreateGuild、GetGuild、GetPlayerGuild、LeaveGuild、DisbandGuild、SetAnnouncement、GetGuildRank、GetGuildRankByGuild、UpdateGuildScore(仅内部)。

扩展消息:
- `GuildMember` 增 `string name = 7`(展示用,不入库)、`uint64 contribution_total = 8`、`uint64 contribution_balance = 9`;原 `contribution = 5` 删除(开发期可删,须全量重编)。
- `GuildInfo` 增 `uint64 funds = 10`、`uint32 max_officers = 11`、`uint32 officer_count = 12`、`uint64 upgrade_cost_funds = 13`(0=满级)、`string leader_name = 14`、`uint32 pending_application_count = 15`(仅长老及以上非 0)。
- `GuildRankEntry` 增 `string leader_name = 8`。

新增 RPC:
| RPC | 请求 | 响应 | 权限 |
|---|---|---|---|
| SetGuildMemberRole | `{uint64 target_player_id; uint32 role}` | `{TipInfoMessage error_message; GuildInfo guild}` | 帮主;role ∈ {0,1} |
| KickGuildMember | `{uint64 target_player_id}` | 同上 | 帮主踢任何人;长老只踢成员 |
| TransferGuildLeader | `{uint64 target_player_id}` | 同上 | 帮主;原帮主变长老(超上限则变成员) |
| ApplyJoinGuild | `{uint64 guild_id}` | `{error_message}` | 未入帮;同帮重复申请 = 刷新过期时间并成功 |
| CancelGuildApplication | `{uint64 guild_id}` | `{error_message}` | 申请人 |
| ListMyGuildApplications | `{}` | `{error_message; repeated GuildApplicationView applications}` | 本人 |
| ListGuildApplications | `{}` | `{error_message; repeated GuildApplicantView applicants}` | 长老及以上 |
| ReviewGuildApplication | `{uint64 applicant_player_id; bool approve}` | `{error_message; GuildInfo guild}` | 长老及以上 |
| DonateToGuild | `{uint32 donate_id}` | `{error_message; GuildDonationView donation; GuildInfo guild}` | 成员 |
| UpgradeGuild | `{}` | `{error_message; GuildInfo guild}` | 帮主或长老 |
| GetGuildShop | `{}` | `{error_message; repeated GuildShopGoodsView goods; uint64 contribution_balance; repeated GuildShopOrderView pending_orders}` | 成员 |
| BuyGuildShopGoods | `{uint32 goods_id; uint32 count}` | `{error_message; GuildShopOrderView order; uint64 contribution_balance}` | 成员 |
| GetGuildActivities | `{}` | `{error_message; repeated GuildActivityView activities}` | 成员 |
| LightGuildLantern | `{uint32 activity_id}` | `{error_message; GuildActivityView activity}` | 成员 |
| ClaimGuildReunion | `{uint32 activity_id}` | `{error_message; GuildActivityView activity}` | 成员 |
| StartGuildTrial | `{uint32 activity_id; repeated uint64 member_player_ids}` | `{error_message; GuildActivityView activity}` | 发起人须为队伍成员 |
| NotifyGuildChanged | `GuildChangedS2C` | `Empty` | **推送占位,不进白名单** |

视图消息(字段在各节细化,名字固定):`GuildApplicationView`、`GuildApplicantView`、`GuildDonationView`、`GuildShopGoodsView`、`GuildShopOrderView`、`GuildActivityView`、`GuildChangedS2C{GuildChangeKind kind; uint64 guild_id; uint64 actor_player_id; uint64 target_player_id}`、`enum GuildChangeKind`(0 UNSPECIFIED、1 MEMBER_JOINED、2 MEMBER_LEFT、3 MEMBER_KICKED、4 ROLE_CHANGED、5 LEADER_TRANSFERRED、6 DISBANDED、7 APPLICATION_RECEIVED、8 APPLICATION_REJECTED、9 FUNDS_CHANGED、10 LEVEL_UP、11 ANNOUNCEMENT_CHANGED、12 ACTIVITY_CHANGED、13 DELIVERY_DONE)。

### 3.2 `proto/guild/guild_db.proto`(表 message,库 `mmorpg_guild`,只给 schemamigrate / repo 用)

每表整数主键、至多一个 UNIQUE KEY、string 索引 ≤191:
| 表 | 主键 | UNIQUE | 用途 |
|---|---|---|---|
| `guild` | guild_id | name | 增 `funds uint64` |
| `guild_member` | (guild_id, player_id) | player_id | `contribution_total`、`contribution_balance` 替换 `contribution` |
| `guild_application` | (guild_id, player_id) | — (索引 player_id、expire_ms) | 只存待审;通过/拒绝/撤销/过期即删行 |
| `guild_asset_op` | op_id(idsegment biz_tag `guild_asset_op`) | (player_id, stream, seq) | 资产 outbox:kind(DONATE/SHOP/ACTIVITY_REWARD)、status(PENDING/APPLIED/REJECTED/ABORTED)、durable、attempts、next_attempt_ms、deadline_ms、payload(bytes,序列化的 `AssetBundle`)、关联 id |
| `guild_player_op_seq` | (player_id, stream) | — | 每玩家每流下一个 seq |
| `guild_daily_counter` | (player_id, counter_kind, ref_id, period_key) | — | 捐献次数、商店限购、活动每日领取 |
| `guild_activity_progress` | (guild_id, activity_id, period_key) | — | 点灯计数、阈值达成与资金已发标记 |
| `guild_trial_battle` | battle_id | — | 历练结算幂等:guild_id、activity_id、结果已处理 |

`guild_schema_migration` 与 `MigrateLegacyRankScores` 删除(未上线无存量);friend 若依赖该表需在 B1 同步处理。

### 3.3 通用资产通道 `proto/common/asset/asset_op.proto` + `proto/scene_manager/scene_node_service.proto`

- `enum AssetOpStream`:0 UNSPECIFIED、1 GUILD_DEBIT、2 GUILD_CREDIT、3 TRADE_DEBIT、4 TRADE_CREDIT、5 SYSTEM_CREDIT(D1 邮件/GM 发物预留)。**每个服务独占自己的流**,各自分配 seq,互不协调。
- `enum AssetOpOutcome`:0 UNKNOWN、1 APPLIED、2 REJECTED、3 RETRY、4 NOT_HERE。
- `message CurrencyAmount {uint32 currency_type; uint64 amount;}`、`message ItemGrant {uint32 config_id; uint32 count;}`、`message AssetBundle {repeated CurrencyAmount currencies; repeated ItemGrant items;}`(聚宝斋的按 guid 扣物、宠物、角色日后以新字段扩展)。
- SceneNodeGrpc 新增:`AssetDebit(AssetOpRequest) returns (AssetOpResponse)`、`AssetAbortDebit(AssetOpRequest) returns (AssetOpResponse)`、`AssetCredit(AssetOpRequest) returns (AssetOpResponse)`。
- `AssetOpRequest {uint64 player_id; AssetOpStream stream; uint64 seq; uint64 correlation_id; uint32 tx_type; AssetBundle bundle;}`;`AssetOpResponse {AssetOpOutcome outcome; TipInfoMessage reason; bool durable;}`。`durable=true` 表示账本结局与资产变化已一起写入 player blob(Redis)。Go 只有在 `APPLIED && durable` 时才落对侧账;`APPLIED && !durable` 保持 PENDING,稍后以同 seq 重投查询。
- scene 组件 `PlayerAssetOpLedgerComp`(proto `proto/common/component/asset_op_ledger_comp.proto`),存 `player_database` 下一个空闲字段号(当前 15,落码前再核)。
- `TransactionType` 追加 `TX_GUILD_DONATE`、`TX_GUILD_SHOP`、`TX_GUILD_ACTIVITY_REWARD`(追加不复用)。
- GM 货币指令(`GmAddCurrency`/`GmDeductCurrency`):scene 侧仅当环境变量 `MMORPG_ALLOW_CLIENT_GM=1` 时受理,默认拒绝;本地启动脚本与 robot 冒烟显式开启。

### 3.4 名字

- `DataService`(data_service.proto)新增:`ReservePlayerName{uint64 player_id; string name}`→`{uint32 result /*0 ok 1 taken 2 invalid*/}`、`ReleasePlayerName{uint64 player_id}`→`Empty`、`BatchGetPlayerName{repeated uint64 player_ids}`→`{map<uint64,string> names}`。真源:data_service 全局 MySQL 表 `player_name`(player_id 主键、`name_norm` 唯一),Redis 缓存 `player:name:{id}`。
- `CreatePlayerRequest` 增 `string name = 3`;`AccountSimplePlayer` 增 `string name = 5`。
- `PlayerProfileComp {string name = 1;}`,`player_database` 另一个空闲字段号;scene 填 `BattlePlayerSnapshot.player_name`。
- 名字规则:去首尾空白后 2–12 个 Unicode 字符;只允许汉字、字母、数字;唯一性按 `name_norm`(NFKC + 小写)。空名 → 服务端生成 `道友` + 6 位随机字母数字(撞名重试 ≤5 次)。敏感词本期只做内置小词表占位接口,词表来源另议。

## 4. Tip 码(只加 `data/tip/Tip.xlsx` 行,名字固定)

`//guild_error`(接在 `GuildHomeZoneUnknown` 之后):GuildZoneMerging、GuildTargetNotMember、GuildCannotTargetSelf、GuildRankTooLow、GuildOfficerLimit、GuildApplicationNotFound、GuildApplicationLimit、GuildApplicationQueueFull、GuildFundsInsufficient、GuildMaxLevel、GuildDonateLimit、GuildCurrencyInsufficient、GuildAssetPending、GuildAssetRejected、GuildShopGoodsNotFound、GuildShopLevelTooLow、GuildShopLimit、GuildContributionInsufficient、GuildActivityNotOpen、GuildActivityAlreadyClaimed、GuildActivityThresholdNotReached、GuildTrialTeamInvalid、GuildActivityLevelTooLow。

`//login_error`:RoleNameInvalid、RoleNameTaken、RoleNameSensitive。

新组 `//asset_error base=27000 width=1000`(scene 在 `AssetOpResponse.reason` 里给出,Go 映射成业务码):AssetCurrencyInsufficient、AssetBagFull、AssetInBattle、AssetFrozen、AssetInvalidBundle、AssetBlocked、AssetPlayerNotHere。

## 5. 配表(`data/<Sheet>.xlsx` + `data/schema/<sheet>_table.proto`,C++ CMakeLists/vcxproj 手工登记)

| 表 | 列 |
|---|---|
| GuildRule(单行 id=1) | application_expire_hours、max_pending_applications_per_player、max_pending_applications_per_guild、asset_op_deadline_seconds、asset_op_retry_base_ms、reunion_min_online_members(兜底) |
| GuildLevel(id=等级) | max_members、max_officers、upgrade_cost_funds(uint64,0=满级) |
| GuildDonate(id) | name、currency_type、cost_amount(uint64)、contribution_gain(uint64)、funds_gain(uint64)、daily_limit、min_guild_level |
| GuildShop(id) | name、category(1 修行补给/2 帮会珍藏/3 节庆好礼)、item_id [fk Item]、item_count、cost_contribution(uint64)、required_guild_level、limit_period(0 不限/1 日/2 周)、limit_count |
| GuildActivity(id) | name、type(1 灯会/2 团圆/3 历练)、enabled、start_at_ms、end_at_ms(uint64,0/0=常开,仅供开发)、min_guild_level、personal_contribution(uint64)、guild_funds(uint64)、guild_threshold、reward_id [fk Reward,0=无]、dungeon_id [fk Dungeon,0=无]、team_size_min、team_size_max、daily_limit |

## 6. 其它必改清单(各节认领)

- MessageLimiter.xlsx:新消息号读 10/s、写 5/s(proto-gen 分配号之后加)。
- 客户端 `tools/gen_messageids.ps1` 白名单、`GuildClient` 注册 `NotifyGuildChanged`。
- robot:`guild_smoke` 扩展,账号段 `robot_9211`–`robot_9219`(避开 9001–9006、9101–9102、9201–9203、9301–9304、9401–9403)。
- 文档:本设计落 `docs/design/guild-phase2.md`;更新 `guild-zone-client-access.md` §5、D-14 §8、`jubaozhai-market.md` §6.4、`team-system.md` §G.2(名字)、PROGRESS。
- message id 与 Tip.xlsx 与聚宝斋/组队会话共用,proto-gen 与导表每批串行跑,跑前 `git status` 核对他人未提交产物不被覆盖。

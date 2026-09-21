package constants

import (
	"shared/generated/pb/table"
	"shared/serverbase"
)

// Guild member roles.
const (
	RoleMember  uint32 = 0
	RoleOfficer uint32 = 1
	RoleLeader  uint32 = 3
)

// 职位高低的档位:连续、可直接比大小,专供权限判断使用。
//
// 持久化的 role 编码(D-4:0 成员 / 1 长老 / 3 帮主)**不连续**,2 是刻意跳过的空号。
// 所以权限一律比 Rank,不比 role 原值:`role >= RoleOfficer` 会把 2(存量脏数据,
// 或将来插入的新职位)一起放进来,等于凭空发权限。
const (
	RankNone    = 0
	RankMember  = 1
	RankOfficer = 2
	RankLeader  = 3
)

// Rank 把 role 编码映射到上面的档位。未知编码返回 RankNone,于是所有
// `Rank(x) >= RankOfficer` 形式的判断自动拒绝 —— 安全路径 fail-closed,
// default 分支是这条保证的落点,不要改成"透传原值"。
func Rank(role uint32) int {
	switch role {
	case RoleMember:
		return RankMember
	case RoleOfficer:
		return RankOfficer
	case RoleLeader:
		return RankLeader
	default:
		return RankNone
	}
}

// AssignableRole:SetGuildMemberRole 只能设 0(成员)/ 1(长老)。
// 帮主只能经 TransferGuildLeader 产生 —— 否则任免接口就成了"自己给自己升帮主"的后门。
func AssignableRole(role uint32) bool { return role == RoleMember || role == RoleOfficer }

// 公会的 tip 码现在由配表统一发号,不再手写。
//
// # 这里为什么变了
//
// 这些常量最终写进 TipInfoMessage.id,而 TipInfoMessage 承载的是**全仓唯一一条
// 数轴**,客户端拿 id 去查文案。历史上这一段是手写的 [200,219] 私有段 ——
// 一个「约定」:它防不住别人(go/match 就从 1 开始重新数了一遍,ErrInBattle=1
// 直接压在 common 段上),也拿不到中文文案(Tip.xlsx 的文案列当时根本没有出口)。
//
// 2026-09-02 起改成:段由 data/tip/Tip.xlsx 的组头行声明,发号器按段分配,
// 段表与文案表都是生成产物。公会段是 guild_error base=14000。
// 于是「防重叠」从人的自觉变成发号器的机械保证,文案也随表一起发。
//
// 加一个新码 = 往 Tip.xlsx 的 //guild_error 组里加一行(名字 + 中文),
// 重跑导表器,然后在下面加一行引用。不要再手写数字。
const (
	ErrAlreadyInGuild  = uint32(table.GuildError_kGuildAlreadyInGuild)
	ErrGuildNotFound   = uint32(table.GuildError_kGuildNotFound)
	ErrNotInGuild      = uint32(table.GuildError_kGuildNotInGuild)
	ErrGuildFull       = uint32(table.GuildError_kGuildFull)
	ErrLeaderCantLeave = uint32(table.GuildError_kGuildLeaderCantLeave)
	ErrNotLeader       = uint32(table.GuildError_kGuildNotLeader)
	ErrNoPermission    = uint32(table.GuildError_kGuildNoPermission)
	ErrNotRanked       = uint32(table.GuildError_kGuildNotRanked)
	// ErrIDGenUnavailable:发号器已被 fence(worker id 的 etcd 租约丢了),
	// 本次建帮整体失败。客户端重试即可 —— 进程会退出并由编排重拉、重新拿号。
	ErrIDGenUnavailable = uint32(table.GuildError_kGuildIdGenUnavailable)
	// ErrGuildNameInvalid:帮名为空、超过 MaxGuildNameRunes、不是合法 UTF-8 或含控制字符。
	ErrGuildNameInvalid = uint32(table.GuildError_kGuildNameInvalid)
	// ErrGuildNameTaken:uk_guild(name_norm)全局唯一索引拒绝(帮名跨 zone 唯一,合服不必改名)。
	ErrGuildNameTaken = uint32(table.GuildError_kGuildNameTaken)
	// ErrAnnouncementTooLong:公告超过 MaxAnnouncementBytes。
	ErrAnnouncementTooLong = uint32(table.GuildError_kGuildAnnouncementTooLong)
	// ErrHomeZoneUnknown:data_service 里没有该玩家的归属 zone 映射,无法判定他属于哪个区的帮会。
	// 是数据状态不是故障:存量玩家需要运维跑 tools/merge_zone -backfill-home-zone 补映射。
	ErrHomeZoneUnknown = uint32(table.GuildError_kGuildHomeZoneUnknown)
	// ErrZoneMerging:归属 zone 正在合服维护(merge:in_progress:{zone} 存在,或这个键读不到)。
	// 读失败也回它:闸门判不了就当成"正在合服"拒绝,而不是放行(fail-closed)。
	// 取代了原来的 gRPC FailedPrecondition —— 那条路径客户端只能显示通用错误。
	ErrZoneMerging = uint32(table.GuildError_kGuildZoneMerging)
	// ErrTargetNotMember:操作目标不是本帮成员(可能刚退帮、被别人踢了,或客户端拿的是过期快照)。
	ErrTargetNotMember = uint32(table.GuildError_kGuildTargetNotMember)
	// ErrCannotTargetSelf:任免 / 踢人 / 转让 / 审批都不允许把自己当目标。
	ErrCannotTargetSelf = uint32(table.GuildError_kGuildCannotTargetSelf)
	// ErrRankTooLow:MySQL 权威 role 的 Rank 不够(见 Rank)。缓存里的 role 不作数:
	// 授权一律在事务里锁行复核,否则缓存陈旧就等于越权。
	ErrRankTooLow = uint32(table.GuildError_kGuildRankTooLow)
	// ErrOfficerLimit:长老数已达 GuildLevel[guild.level].max_officers。
	// 配表下调上限导致现有长老超额时不强制降级,只拒绝后续任命。
	ErrOfficerLimit = uint32(table.GuildError_kGuildOfficerLimit)
	// ErrApplicationNotFound:申请不存在、已过期(GuildRule.application_expire_hours)、
	// 已被别人处理,或申请人已入他帮 / 归属 zone 与帮会不一致。
	// 后两种情况刻意不单独发码:对审批者来说都是"这条申请已经不能批了",分得更细只会泄露申请人状态。
	ErrApplicationNotFound = uint32(table.GuildError_kGuildApplicationNotFound)
	// ErrApplicationLimit:本人待审申请数已达 GuildRule.max_pending_applications_per_player。
	ErrApplicationLimit = uint32(table.GuildError_kGuildApplicationLimit)
	// ErrApplicationQueueFull:目标帮会待审申请数已达 GuildRule.max_pending_applications_per_guild。
	ErrApplicationQueueFull = uint32(table.GuildError_kGuildApplicationQueueFull)
	// ErrBusyRetry:写事务连续死锁 3 次,或锁等待超时(MySQL 1205,DSN 里钉了
	// innodb_lock_wait_timeout=1)。是业务 tip 不是 gRPC 错误 —— 回 gRPC 错误会让客户端
	// 进入重连隔离,而这里玩家原地重试一次就能成功。
	ErrBusyRetry = uint32(table.GuildError_kGuildBusyRetry)

	// ── 帮会二期 B5(捐献 / 升级 / 商店)新增的 10 个 ──
	// 10 个全是业务拒绝(Tip.xlsx 的 fault 列留空):捐献与兑换失败都有可重试或可理解的原因,
	// 回 gRPC 错误会让客户端进入重连隔离。scene 回的资产原因码(27xxx)**不在这里**加别名:
	// 它们属于 asset_error 段,放进本组会被 TestNoHandWrittenTipCodes 判成"引用其他码轴";
	// 业务代码直接用 assetop.Reason*。

	// ErrFundsInsufficient:帮会资金不足以升到下一级(按**当前等级行**的 upgrade_cost_funds 判)。
	ErrFundsInsufficient = uint32(table.GuildError_kGuildFundsInsufficient)
	// ErrMaxLevel:帮会已在最高级(当前等级行 upgrade_cost_funds == 0)。
	ErrMaxLevel = uint32(table.GuildError_kGuildMaxLevel)
	// ErrDonateLimit:该捐献选项今日次数已用完(计数器在提交 PENDING 时占用,结局拒绝 / 中止才退回)。
	ErrDonateLimit = uint32(table.GuildError_kGuildDonateLimit)
	// ErrCurrencyInsufficient:scene 以 durable 结局拒绝捐献扣款,原因是货币不足。
	ErrCurrencyInsufficient = uint32(table.GuildError_kGuildCurrencyInsufficient)
	// ErrAssetPending:本人未结算的帮会资产操作过多(assetop.ErrTooManyPending),或资产通道未开启。
	// 此时**没有写入任何指令**,响应不带订单视图。
	ErrAssetPending = uint32(table.GuildError_kGuildAssetPending)
	// ErrAssetRejected:scene 以 durable 结局永久拒绝(货币不足之外的原因),本次操作已撤销并退回占用。
	ErrAssetRejected = uint32(table.GuildError_kGuildAssetRejected)
	// ErrShopGoodsNotFound:GuildShop 配表里没有这个 goods_id。
	ErrShopGoodsNotFound = uint32(table.GuildError_kGuildShopGoodsNotFound)
	// ErrShopLevelTooLow:帮会等级不足(捐献选项的 min_guild_level 与商品的 required_guild_level 共用)。
	ErrShopLevelTooLow = uint32(table.GuildError_kGuildShopLevelTooLow)
	// ErrShopLimit:单次份数超过 MaxBuyCount,或本周期限购已满。
	ErrShopLimit = uint32(table.GuildError_kGuildShopLimit)
	// ErrContributionInsufficient:可用帮贡(contribution_balance)不足以兑换。
	ErrContributionInsufficient = uint32(table.GuildError_kGuildContributionInsufficient)
)

// MaxShopBuyCount:帮会商店单次兑换的份数上限(05-economy.md §5.11.2)。
//
// 它同时是 GuildShop.cost_contribution ≤ 1e9 这条配表校验的前提:cost × 20 必须装得进 uint64
// 且远离溢出,改大之前先重算那条校验。实际单次上限还受物品堆叠约束,见 logic.MaxBuyCount。
const MaxShopBuyCount uint32 = 20

// Default limits.
//
// 成员上限不在这里 —— 改由 GuildLevel 配表按等级给(建帮取第 1 级的 max_members)。
// 原先的 DefaultMaxMembers=50 是硬编码,策划改不了,2026-09 随帮会二期 B2 删除。
const (
	DefaultInitLevel uint32 = 1
)

// MaxGuildMembersCap:GuildLevel.max_members 的**校验**上限,不是运行期人数上限。
//
// 它是推送与 GuildInfo 快照的预算前提:一次 BroadcastToPlayers 的收件人、一份成员快照的
// 包体大小都按这个数估过(gate 单包 1KB 的约束见 MaxAnnouncementBytes)。
// 配表把 max_members 填得更大时,启动校验 fail-closed 拒绝启动;真要改大,先重做上述预算。
const MaxGuildMembersCap = 100

// 客户端输入上限。服务端必须自己校验:客户端的限制随时可以被绕过。
const (
	// MaxGuildNameRunes 按 Unicode 字符数计,与客户端输入框的 24 字一致。
	MaxGuildNameRunes = 24
	// MaxAnnouncementBytes 按 UTF-8 **字节**计(约 200 个汉字)。不能按字数给到 500:
	// gate 对单个客户端包有 1KB 上限(CheckMessageSize),500 个汉字 ≈ 1500 字节,
	// 请求在 gate 就被丢掉,玩家只会看到超时。600 字节加上 guild_id / player_id 与信封仍 < 1KB。
	MaxAnnouncementBytes = 600
	// MaxRankPageSize 限制客户端单次拉榜条数;每条都要回表补名字 / 人数。
	MaxRankPageSize uint32 = 50
)

// TipClassifier 返回本服务的 in-band 业务码定性函数,供 serverbase.UnaryInterceptor 使用。
//
// 公会段进配表之后,serverbase.TipVerdict 已经认得这些码(不再判 VerdictUnknown);
// 「哪个码算服务端故障」也进了表 —— Tip.xlsx 的 fault 列,ErrIDGenUnavailable
// (发号器被 fence:本进程已不是该 worker id 的合法持有者,随后会主动退出,必须能被看见)
// 就是在那里标的 1。以前这里有一张本地 faultCodes map 专门补这一条,
// 那正是「码的属性和码定义分家」的病灶,2026-09-05 随 fault 列一起删掉了。
//
// 所以这里直接交回全局判定。**不要再往这里加本地 map** —— 要改某个码的分类,改表。
func TipClassifier() serverbase.Classifier {
	return serverbase.TipVerdict
}

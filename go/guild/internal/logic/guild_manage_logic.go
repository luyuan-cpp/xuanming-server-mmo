package logic

// 帮会管理与审批的 logic 层(设计 docs/design/guild-phase2/02-management.md §10–§12)。
//
// 三条贯穿全文件的纪律:
//
//  1. **授权只认 MySQL**。这里做的是"能不能进到事务"的粗筛(有没有会话、目标是不是自己、
//     role 值合不合法),真正的职位判定一律在 repo 的事务里锁行复核。缓存里的 role / leader_id
//     只能用来展示 —— 玩家刚被降权、帮会刚被合服搬走时,缓存会在整整一个 TTL 内说谎。
//  2. **写 RPC 的前置顺序是固定的**:会话身份 → 归属 zone → 合服闸门 → 业务前置 → 事务。
//     顺序不能换:没有会话就没有可信的操作者,没有归属 zone 就不知道该查哪个区的闸门,
//     闸门没过就一个字节都不该写。
//  3. **业务拒绝一律回 tip,不回 gRPC 错误**。回 gRPC 错误会让客户端进入重连隔离
//     (serverbase 把它当故障),而"职位不够""申请已失效""写冲突重试一次"都是正常玩法。
//     只有真故障(配表缺行、双存储互相矛盾、依赖不可用)才返回 error。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"guild/internal/constants"
	"guild/internal/data"
	base "proto/common/base"
	pb "proto/guild"
	tablepb "shared/generated/pb/table"
	"shared/generated/table"
	"shared/safego"
)

// ── §10.2 配表读取 ────────────────────────────────────────────
//
// 配表只存 id、用时现查:table 包是 atomic 快照,热更会整批换指针,
// 把行指针存进结构体等于永远拿到热更前的值(见 generated/table 顶部契约)。
// 启动时 ValidateGuildTables 已经保证行存在且自洽,所以运行期查不到 =
// 配表被错误替换,一律 fail-closed(gRPC Internal),不给默认值。

// guildRuleRowID:GuildRule 是单行全局规则表,id 固定 1(与 PetRule 同套路)。
const guildRuleRowID uint32 = 1

// myApplicationsLimit:ListMyGuildApplications 单次返回的上限。
// 取 10 而不是读配表:它是 GuildRule.max_pending_applications_per_player 的**校验上限**
// (见 validateGuildTables),配表再怎么调都不会超过它,列表也就不会被截断。
const myApplicationsLimit uint32 = 10

// applicationRulesFromTable 现查 GuildRule 行并换算成 repo 需要的形态。
// ok=false = 配表缺行,调用方必须回 Internal,不得当成"无限制"放行。
func applicationRulesFromTable() (data.ApplicationRules, bool) {
	row, ok := table.GuildRuleTableManagerInstance.FindById(guildRuleRowID)
	if !ok {
		return data.ApplicationRules{}, false
	}
	return data.ApplicationRules{
		TTLMs:        uint64(row.GetApplicationExpireHours()) * uint64(time.Hour/time.Millisecond),
		MaxPerPlayer: row.GetMaxPendingApplicationsPerPlayer(),
		MaxPerGuild:  row.GetMaxPendingApplicationsPerGuild(),
	}, true
}

// officerCapFromTable 满足 data.OfficerCapFunc:按帮会当前等级查长老上限。
// 等级行缺失时返回 ok=false,repo 据此回 ErrGuildLevelConfigMissing(fail-closed),
// 绝不能默认成"不限"—— 那会让任免接口在配表出错时变成无上限。
func officerCapFromTable(level uint32) (uint32, bool) {
	row, ok := table.GuildLevelTableManagerInstance.FindById(level)
	if !ok {
		return 0, false
	}
	return row.GetMaxOfficers(), true
}

// initialMaxMembers 给建帮用:第 1 级的成员上限(默认 30)。
// 上限进配表之后策划可以随时调,代码里不再有 DefaultMaxMembers 那种硬编码。
func initialMaxMembers() (uint32, bool) {
	row, ok := table.GuildLevelTableManagerInstance.FindById(constants.DefaultInitLevel)
	if !ok {
		return 0, false
	}
	return row.GetMaxMembers(), true
}

// levelDisplay 给 GuildInfo 装配用:长老上限与升级花费两个**纯展示**字段。
// 查不到时调用方留 0 并记 ERROR —— 展示缺一个数字不值得让整次读失败,
// 而真正的授权(任免上限)另走 officerCapFromTable,那条路是 fail-closed 的。
func levelDisplay(level uint32) (maxOfficers uint32, upgradeCostFunds uint64, ok bool) {
	row, found := table.GuildLevelTableManagerInstance.FindById(level)
	if !found {
		return 0, 0, false
	}
	return row.GetMaxOfficers(), row.GetUpgradeCostFunds(), true
}

// ── §3.3 启动校验 ─────────────────────────────────────────────

// 配表取值的合法区间。写成常量而不是字面量,是为了让错误文案与判据用同一组数,
// 改区间时不会出现"判据改了、文案还说旧范围"。
const (
	minApplicationExpireHours uint32 = 1
	maxApplicationExpireHours uint32 = 720 // 30 天:申请行按 expire_ms 惰性清理,再长就等于永不过期
	minPendingPerPlayer       uint32 = 1
	maxPendingPerPlayer       uint32 = 10
	minPendingPerGuild        uint32 = 1
	maxPendingPerGuild        uint32 = 500
	minGuildMaxMembers        uint32 = 2 // 至少容得下帮主 + 一个成员
)

// ValidateGuildTables 由 guild.go 在 NewServiceContext 之后、起 gRPC 之前调用,失败即 logx.Must。
//
// 为什么要拒启而不是运行期兜底:成员上限、长老上限、申请有效期全部来自这两张表,
// 表错了会表现为"任免随机失败""申请永不过期"这类静默错误,等玩家报上来时数据已经脏了。
// 启动期拒绝的代价只是一次部署回滚。
func ValidateGuildTables() error {
	rule, _ := table.GuildRuleTableManagerInstance.FindById(guildRuleRowID)
	return validateGuildTables(rule, table.GuildLevelTableManagerInstance.FindAll())
}

// validateGuildTables 是纯函数(便于单测覆盖每条规则)。
// 错误文案一律带表名、行 id、字段名与实际值 —— 策划看到日志要能直接定位到格子。
//
// **唯一性约定**:GuildRule / GuildLevel 的结构规则只在这里写一份。
// B5 的 ValidateEconomyTables 先调用它,再补自己那几列;B6 只校验自己追加的列。
func validateGuildTables(rule *tablepb.GuildRuleTable, levels []*tablepb.GuildLevelTable) error {
	if rule == nil {
		return fmt.Errorf("GuildRule 表缺少 id=%d 的规则行", guildRuleRowID)
	}
	if v := rule.GetApplicationExpireHours(); v < minApplicationExpireHours || v > maxApplicationExpireHours {
		return fmt.Errorf("GuildRule[%d].application_expire_hours=%d 越界,应在 [%d,%d]",
			guildRuleRowID, v, minApplicationExpireHours, maxApplicationExpireHours)
	}
	if v := rule.GetMaxPendingApplicationsPerPlayer(); v < minPendingPerPlayer || v > maxPendingPerPlayer {
		return fmt.Errorf("GuildRule[%d].max_pending_applications_per_player=%d 越界,应在 [%d,%d]",
			guildRuleRowID, v, minPendingPerPlayer, maxPendingPerPlayer)
	}
	if v := rule.GetMaxPendingApplicationsPerGuild(); v < minPendingPerGuild || v > maxPendingPerGuild {
		return fmt.Errorf("GuildRule[%d].max_pending_applications_per_guild=%d 越界,应在 [%d,%d]",
			guildRuleRowID, v, minPendingPerGuild, maxPendingPerGuild)
	}

	if len(levels) == 0 {
		return errors.New("GuildLevel 表为空:建帮要读第 1 级的 max_members")
	}
	// FindAll 的顺序不作假设(导表器与热更都可能改变它),这里自己排一次。
	// 复制一份再排:FindAll 返回的是快照内部切片,原地排序等于改别人的数据。
	sorted := make([]*tablepb.GuildLevelTable, 0, len(levels))
	for i, row := range levels {
		if row == nil {
			return fmt.Errorf("GuildLevel 表第 %d 行为空", i+1)
		}
		sorted = append(sorted, row)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetId() < sorted[j].GetId() })

	var prevMaxMembers, prevMaxOfficers uint32
	for i, row := range sorted {
		level := uint32(i) + 1
		// id 必须从 1 连续:guild.level 直接当 id 查表,缺一级就等于那一级的帮会全部读不到配表。
		// 重复 id 也会在这里被抓住(排序后必然出现 id != 期望值)。
		if row.GetId() != level {
			return fmt.Errorf("GuildLevel.id 必须从 1 连续递增:第 %d 行 id=%d,期望 %d", i+1, row.GetId(), level)
		}
		maxMembers, maxOfficers := row.GetMaxMembers(), row.GetMaxOfficers()
		if maxMembers < minGuildMaxMembers || maxMembers > constants.MaxGuildMembersCap {
			return fmt.Errorf("GuildLevel[%d].max_members=%d 越界,应在 [%d,%d]",
				level, maxMembers, minGuildMaxMembers, constants.MaxGuildMembersCap)
		}
		// 长老上限必须严格小于成员上限:相等就意味着可以把全帮任命成长老,帮主失去唯一性之外的全部区分度。
		if maxOfficers >= maxMembers {
			return fmt.Errorf("GuildLevel[%d].max_officers=%d 必须小于 max_members=%d", level, maxOfficers, maxMembers)
		}
		// 升级只该让帮会变强。递减会让"升级后现有成员超编"成为常态,而本设计不做强制踢人。
		if maxMembers < prevMaxMembers {
			return fmt.Errorf("GuildLevel[%d].max_members=%d 小于上一级的 %d,成员上限不得随等级递减",
				level, maxMembers, prevMaxMembers)
		}
		if maxOfficers < prevMaxOfficers {
			return fmt.Errorf("GuildLevel[%d].max_officers=%d 小于上一级的 %d,长老上限不得随等级递减",
				level, maxOfficers, prevMaxOfficers)
		}
		// upgrade_cost_funds == 0 的语义是"满级",所以只允许出现在最后一行。
		// 写在中间一行会让那一级永远升不上去(花费 0 = 无处可升),且没有任何报错。
		last := i == len(sorted)-1
		if cost := row.GetUpgradeCostFunds(); (cost == 0) != last {
			if last {
				return fmt.Errorf("GuildLevel[%d].upgrade_cost_funds=%d:最后一级必须为 0(满级)", level, cost)
			}
			return fmt.Errorf("GuildLevel[%d].upgrade_cost_funds=0:只有最后一级允许为 0(满级)", level)
		}
		prevMaxMembers, prevMaxOfficers = maxMembers, maxOfficers
	}
	return nil
}

// ── §11 写 RPC 公共前置 ───────────────────────────────────────

// nowMs 是本服务唯一的"现在":申请有效期、apply_ms、建帮时间都用它。
// 统一成一个函数是为了让日后换时钟源(测试注入 / 对时)只改一处。
func nowMs() uint64 { return uint64(time.Now().UnixMilli()) }

// viewerOf:只有客户端来源的请求才有"请求者视角"。
// 内部调用(GM、运维工具、其它服务)一律 0 —— 它们没有玩家身份,
// 不该拿到只给本帮长老看的待审数(§12.4)。
func viewerOf(who caller) uint64 {
	if who.fromClient {
		return who.playerID
	}
	return 0
}

// clientWrite 是新管理 / 申请 RPC 的统一入口。
//
// 只接受带 gate 会话的调用:这些请求体里**没有** player_id(协议刻意不放),
// 内部调用因此没有可信的操作者身份,继续往下走只会拿 0 当操作者。
// 顺序:会话 → 归属 zone → 合服闸门(§11.2)。
//
// 返回约定:err 非 nil = 故障(调用方直接 return nil, err);
// tip 非 nil = 业务拒绝(调用方把 tip 放进响应体,error 回 nil)。
func (l *GuildLogic) clientWrite(ctx context.Context) (playerID uint64, zoneID uint32, tip *base.TipInfoMessage, err error) {
	who := callerOf(ctx, 0)
	if !who.fromClient {
		return 0, 0, nil, status.Error(codes.PermissionDenied, "guild management requires a client session")
	}
	zoneID, tip, err = l.clientZone(ctx, who.playerID)
	if err != nil || tip != nil {
		return who.playerID, 0, tip, err
	}
	return who.playerID, zoneID, l.mergeFenceTip(ctx, zoneID), nil
}

// operatorGuild 定位操作者所在帮会,并在读到 0 时用 MySQL 复核一次(§6.4.2)。
//
// 为什么非复核不可:GetPlayerGuildID 连 0 也缓存 30 分钟,而"审批通过 → 提交成功 →
// 缓存失效失败"这条路径会留下一个说他没入帮的 0。不复核的话,刚被批准的玩家在
// 整整半小时里什么帮会操作都做不了,而且看不出原因。
func (l *GuildLogic) operatorGuild(ctx context.Context, playerID uint64) (uint64, *base.TipInfoMessage, error) {
	id, err := l.repo.GetPlayerGuildID(ctx, playerID)
	if err != nil {
		return 0, nil, err
	}
	if id == 0 {
		if id, err = l.repo.VerifyPlayerGuildID(ctx, playerID, 0); err != nil {
			return 0, nil, err
		}
		if id == 0 {
			return 0, tipErr(constants.ErrNotInGuild, "not in any guild"), nil
		}
	}
	return id, nil, nil
}

// verifyMapping 在事务已经按 MySQL 判定之后,顺手把可能陈旧的 Redis 映射纠正过来。
//
// 失败只记日志:本次要回的 tip 已经由 MySQL 的判定决定了,缓存修不修都不影响这次回答;
// 把缓存修复的失败升级成 RPC 失败,等于让 Redis 抖动直接变成玩家可见的报错。
// repo 为 nil 只在单测里出现(用例专门证明某些分支不碰库),生产永远非 nil。
func (l *GuildLogic) verifyMapping(ctx context.Context, playerID, cached uint64) {
	if l.repo == nil || playerID == 0 {
		return
	}
	if _, err := l.repo.VerifyPlayerGuildID(ctx, playerID, cached); err != nil {
		logx.Errorf("[guild] verify guild mapping of player %d (cached %d): %v", playerID, cached, err)
	}
}

// mapWriteErr 把 repo 的哨兵错误翻成响应(§11.4)。
//
// 返回 (nil, nil) 当且仅当入参 err 为 nil,调用方据此继续走成功分支。
// 契约:业务拒绝回 tip + nil error;只有故障(配表、双存储矛盾、未知错误)才回 error。
// `ErrRankTooLow` 在 DisbandGuild 上要回 kGuildNotLeader,那一处在调用点先行拦截,不在这里分叉 ——
// 否则这个函数就得知道自己被哪个 RPC 调用,那是把上下文倒着传。
// 两个 guild 参数刻意分开,因为它们回答的是两个不同的问题:
//   - logGuildID:**这次操作打的是哪个帮**,只进日志。申请 / 撤回申请时它是目标帮 id,
//     那恰恰是排障最需要的值(热门帮的行锁争用都集中在它身上),不能丢。
//   - cachedGuildID:**我们以为操作者属于哪个帮**,只喂给映射自愈去和 MySQL 比对。
//     申请 / 撤回申请的人按语义不在目标帮里,所以传 0;传目标帮 id 会让 actual(0) 与它永远不等,
//     每次拒绝都白翻一次映射缓存的代数。
func (l *GuildLogic) mapWriteErr(ctx context.Context, actor, logGuildID, cachedGuildID uint64, err error) (*base.TipInfoMessage, error) {
	switch {
	case err == nil:
		return nil, nil
	case errors.Is(err, data.ErrGuildGone), errors.Is(err, data.ErrGuildZoneMismatch):
		// 别区的帮会与不存在的帮会同一答复,不向客户端透露"它在别的区"。
		l.verifyMapping(ctx, actor, cachedGuildID)
		return tipErr(constants.ErrGuildNotFound, "guild not found"), nil
	case errors.Is(err, data.ErrNotGuildMember):
		l.verifyMapping(ctx, actor, cachedGuildID)
		return tipErr(constants.ErrNotInGuild, "not a member of the guild"), nil
	case errors.Is(err, data.ErrTargetNotMember):
		return tipErr(constants.ErrTargetNotMember, "target is not a member"), nil
	case errors.Is(err, data.ErrRankTooLow):
		return tipErr(constants.ErrRankTooLow, "rank too low"), nil
	case errors.Is(err, data.ErrOfficerLimit):
		return tipErr(constants.ErrOfficerLimit, "officer limit reached"), nil
	case errors.Is(err, data.ErrLeaderCantLeave):
		return tipErr(constants.ErrLeaderCantLeave, "leader cannot leave, disband or transfer instead"), nil
	case errors.Is(err, data.ErrGuildFull):
		return tipErr(constants.ErrGuildFull, "guild is full"), nil
	case errors.Is(err, data.ErrPlayerAlreadyInGuild):
		// 缓存说他没入帮、MySQL 说他入了:以 MySQL 为准并顺手把 0 映射纠正回来。
		l.verifyMapping(ctx, actor, 0)
		return tipErr(constants.ErrAlreadyInGuild, "already in a guild"), nil
	case errors.Is(err, data.ErrApplicationNotFound):
		return tipErr(constants.ErrApplicationNotFound, "application not found or expired"), nil
	case errors.Is(err, data.ErrApplicationLimit):
		return tipErr(constants.ErrApplicationLimit, "pending application limit reached"), nil
	case errors.Is(err, data.ErrApplicationQueueFull):
		return tipErr(constants.ErrApplicationQueueFull, "guild application queue is full"), nil
	case errors.Is(err, data.ErrZoneMerging):
		// 事务内的合服闸门拒绝(目前只有解散在事务里判;经济事务先经 economyTip 截走,走不到这里)。
		// 与事务外的 mergeFenceTip 同一答复:合服是可预期的运维窗口,回 tip 而不是 gRPC 错误。
		return tipErr(constants.ErrZoneMerging, "zone merging"), nil
	case errors.Is(err, data.ErrWriteConflict):
		// 记 Info 不记 Error:两个人同时改同一个帮会是正常玩法,客户端原地重试一次就能成功。
		// 这里**不能**回 gRPC 错误 —— 那会让客户端进入重连隔离(见文件头纪律 3)。
		logx.Infof("[guild] guild %d write conflict (player %d), asking the client to retry: %v", logGuildID, actor, err)
		return tipErr(constants.ErrBusyRetry, "guild write conflict"), nil
	case errors.Is(err, data.ErrLeaderMismatch), errors.Is(err, data.ErrGuildLevelConfigMissing):
		// 双存储互相矛盾 / 配表缺行:这不是玩家能修的,必须以故障形式暴露出来让人看见。
		logx.Errorf("[guild] guild %d data or config inconsistent (player %d): %v", logGuildID, actor, err)
		return nil, status.Error(codes.Internal, "guild data or configuration is inconsistent")
	default:
		return nil, err
	}
}

// targetTip:任免 / 踢人 / 转让的公共目标校验,在碰库之前做完。
//
// target==0 回 kGuildTargetNotMember 而不是新发一个"参数非法"码:对玩家来说
// "这个人不在帮里"就是最准确的解释,客户端也已经有这条文案。
func targetTip(actor, target uint64) *base.TipInfoMessage {
	if target == 0 {
		return tipErr(constants.ErrTargetNotMember, "target player id is zero")
	}
	if target == actor {
		return tipErr(constants.ErrCannotTargetSelf, "cannot target self")
	}
	return nil
}

// reviewTip 组一个只带 tip 的审批响应。审批的前置分支特别多(§12.2.1),
// 抽出来免得每条分支都写一遍结构体字面量。
func reviewTip(id uint32, reason string) *pb.ReviewGuildApplicationResponse {
	return &pb.ReviewGuildApplicationResponse{ErrorMessage: tipErr(id, reason)}
}

// applicantZoneResult 是审批时"并行查申请人归属 zone"那条协程的返回值。
type applicantZoneResult struct {
	zone uint32
	err  error
}

// applyPushAllowed:新申请要不要推给审批人。
// 未注入闸门(单测)= 总是放行,与 §10.1 的默认值一致;生产由 guild.go 注入 repo.TryMarkApplyPush,
// 同一 (帮, 申请人) 60 秒内至多推一次,挡住"申请 → 撤回 → 申请"刷屏。
func (l *GuildLogic) applyPushAllowed(ctx context.Context, guildID, playerID uint64) bool {
	if l.applyPushGate == nil {
		return true
	}
	return l.applyPushGate(ctx, guildID, playerID)
}

// ── §12 RPC 处理 ─────────────────────────────────────────────

// SetGuildMemberRole:帮主任免长老(role 只允许 0 / 1)。
func (l *GuildLogic) SetGuildMemberRole(ctx context.Context, req *pb.SetGuildMemberRoleRequest) (*pb.SetGuildMemberRoleResponse, error) {
	actor, _, tip, err := l.clientWrite(ctx)
	if err != nil {
		return nil, err
	}
	if tip != nil {
		return &pb.SetGuildMemberRoleResponse{ErrorMessage: tip}, nil
	}
	target := req.GetTargetPlayerId()
	if tip := targetTip(actor, target); tip != nil {
		return &pb.SetGuildMemberRoleResponse{ErrorMessage: tip}, nil
	}
	// 帮主只能经 TransferGuildLeader 产生:这里放行 role=3 就等于开了"自己升自己"的后门。
	// 2 是持久化编码里刻意跳过的空号,同样拒绝(constants.AssignableRole)。
	if !constants.AssignableRole(req.GetRole()) {
		return &pb.SetGuildMemberRoleResponse{ErrorMessage: tipErr(constants.ErrNoPermission, "role not assignable")}, nil
	}

	guildID, tip, err := l.operatorGuild(ctx, actor)
	if err != nil || tip != nil {
		return &pb.SetGuildMemberRoleResponse{ErrorMessage: tip}, err
	}
	res, err := l.repo.SetMemberRole(ctx, guildID, actor, target, req.GetRole(), officerCapFromTable)
	if tip, err := l.mapWriteErr(ctx, actor, guildID, guildID, err); err != nil || tip != nil {
		return &pb.SetGuildMemberRoleResponse{ErrorMessage: tip}, err
	}
	// Changed=false = 任免成同一职位,库里一个字节都没变;此时推送只会让全帮白拉一次。
	if res.Changed {
		l.notify(pb.GuildChangeKind_GUILD_CHANGE_KIND_ROLE_CHANGED, guildID, actor, target, membersExcept(res.Guild, actor))
	}
	return &pb.SetGuildMemberRoleResponse{Guild: l.guildInfoFor(ctx, res.Guild, actor)}, nil
}

// KickGuildMember:按 rank 踢人(帮主可踢长老与成员,长老只能踢成员,没人能踢帮主)。
func (l *GuildLogic) KickGuildMember(ctx context.Context, req *pb.KickGuildMemberRequest) (*pb.KickGuildMemberResponse, error) {
	actor, _, tip, err := l.clientWrite(ctx)
	if err != nil {
		return nil, err
	}
	if tip != nil {
		return &pb.KickGuildMemberResponse{ErrorMessage: tip}, nil
	}
	target := req.GetTargetPlayerId()
	if tip := targetTip(actor, target); tip != nil {
		return &pb.KickGuildMemberResponse{ErrorMessage: tip}, nil
	}

	guildID, tip, err := l.operatorGuild(ctx, actor)
	if err != nil || tip != nil {
		return &pb.KickGuildMemberResponse{ErrorMessage: tip}, err
	}
	// now 显式传入(B5):事务内把被踢者未结算捐献的截止时间提前到"现在"。
	res, err := l.repo.KickMember(ctx, guildID, actor, target, nowMs())
	if tip, err := l.mapWriteErr(ctx, actor, guildID, guildID, err); err != nil || tip != nil {
		return &pb.KickGuildMemberResponse{ErrorMessage: tip}, err
	}
	// 被踢者已经不在快照里,但他必须收到这条推送 —— 否则他的界面会一直停在
	// "我还在帮里"的旧状态,直到下次手动刷新。
	recipients := append(membersExcept(res.Guild, actor), target)
	l.notify(pb.GuildChangeKind_GUILD_CHANGE_KIND_MEMBER_KICKED, guildID, actor, target, recipients)
	return &pb.KickGuildMemberResponse{Guild: l.guildInfoFor(ctx, res.Guild, actor)}, nil
}

// TransferGuildLeader:转让帮主。原帮主按长老位余量降为长老,满额则降为成员(repo 内判定)。
func (l *GuildLogic) TransferGuildLeader(ctx context.Context, req *pb.TransferGuildLeaderRequest) (*pb.TransferGuildLeaderResponse, error) {
	actor, _, tip, err := l.clientWrite(ctx)
	if err != nil {
		return nil, err
	}
	if tip != nil {
		return &pb.TransferGuildLeaderResponse{ErrorMessage: tip}, nil
	}
	target := req.GetTargetPlayerId()
	if tip := targetTip(actor, target); tip != nil {
		return &pb.TransferGuildLeaderResponse{ErrorMessage: tip}, nil
	}

	guildID, tip, err := l.operatorGuild(ctx, actor)
	if err != nil || tip != nil {
		return &pb.TransferGuildLeaderResponse{ErrorMessage: tip}, err
	}
	res, err := l.repo.TransferLeader(ctx, guildID, actor, target, officerCapFromTable)
	if tip, err := l.mapWriteErr(ctx, actor, guildID, guildID, err); err != nil || tip != nil {
		return &pb.TransferGuildLeaderResponse{ErrorMessage: tip}, err
	}
	l.notify(pb.GuildChangeKind_GUILD_CHANGE_KIND_LEADER_TRANSFERRED, guildID, actor, target, membersExcept(res.Guild, actor))
	return &pb.TransferGuildLeaderResponse{Guild: l.guildInfoFor(ctx, res.Guild, actor)}, nil
}

// ApplyJoinGuild:提交入帮申请(取代原先的直接入帮)。同帮重复申请 = 刷新有效期并成功。
func (l *GuildLogic) ApplyJoinGuild(ctx context.Context, req *pb.ApplyJoinGuildRequest) (*pb.ApplyJoinGuildResponse, error) {
	actor, zoneID, tip, err := l.clientWrite(ctx)
	if err != nil {
		return nil, err
	}
	if tip != nil {
		return &pb.ApplyJoinGuildResponse{ErrorMessage: tip}, nil
	}
	guildID := req.GetGuildId()
	if guildID == 0 {
		return &pb.ApplyJoinGuildResponse{ErrorMessage: tipErr(constants.ErrGuildNotFound, "guild id is zero")}, nil
	}

	// 缓存预检只是省一次事务,不是判据:缓存说他入了帮时先用 MySQL 复核,
	// 免得一个陈旧映射把他永久挡在申请之外(真正的判定在 ApplyToGuild 事务里)。
	cached, err := l.repo.GetPlayerGuildID(ctx, actor)
	if err != nil {
		return nil, err
	}
	if cached > 0 {
		verified, err := l.repo.VerifyPlayerGuildID(ctx, actor, cached)
		if err != nil {
			return nil, err
		}
		if verified > 0 {
			return &pb.ApplyJoinGuildResponse{ErrorMessage: tipErr(constants.ErrAlreadyInGuild, "already in a guild")}, nil
		}
	}

	rules, ok := applicationRulesFromTable()
	if !ok {
		return nil, status.Errorf(codes.Internal, "GuildRule row %d missing", guildRuleRowID)
	}
	res, err := l.repo.ApplyToGuild(ctx, guildID, actor, zoneID, nowMs(), rules)
	// 第三个参数是"操作者**所在**帮会"而不是请求里的目标帮会:申请人按定义不在目标帮里,
	// 把目标帮 id 传进去会让自愈拿一个必然不相等的值去比,每次失败都白翻一次映射缓存。
	// 申请路径上我们相信他"不在任何帮",传 0;真要是已入帮,自愈正好把这条纠正过来。
	if tip, err := l.mapWriteErr(ctx, actor, guildID, 0, err); err != nil || tip != nil {
		return &pb.ApplyJoinGuildResponse{ErrorMessage: tip}, err
	}
	// 只有**新建行**才推:刷新有效期对审批人来说什么都没变,推过去只会让申请列表闪一下。
	if res.Inserted && l.applyPushAllowed(ctx, guildID, actor) {
		l.notify(pb.GuildChangeKind_GUILD_CHANGE_KIND_APPLICATION_RECEIVED, guildID, actor, actor, res.ReviewerIDs)
	}
	return &pb.ApplyJoinGuildResponse{}, nil
}

// CancelGuildApplication:撤回自己的一条申请。撤回从不推送(只影响申请人自己看得到的列表)。
func (l *GuildLogic) CancelGuildApplication(ctx context.Context, req *pb.CancelGuildApplicationRequest) (*pb.CancelGuildApplicationResponse, error) {
	actor, _, tip, err := l.clientWrite(ctx)
	if err != nil {
		return nil, err
	}
	if tip != nil {
		return &pb.CancelGuildApplicationResponse{ErrorMessage: tip}, nil
	}
	guildID := req.GetGuildId()
	if guildID == 0 {
		// 没指定帮会就没有"哪一条申请"可言,与"这条申请已经不在了"同一答复。
		return &pb.CancelGuildApplicationResponse{
			ErrorMessage: tipErr(constants.ErrApplicationNotFound, "guild id is zero"),
		}, nil
	}

	err = l.repo.CancelApplication(ctx, guildID, actor, nowMs())
	// 同 ApplyJoinGuild:撤回申请的人不在目标帮里,自愈的比对基准是 0(见上面的注释)。
	if tip, err := l.mapWriteErr(ctx, actor, guildID, 0, err); err != nil || tip != nil {
		return &pb.CancelGuildApplicationResponse{ErrorMessage: tip}, err
	}
	return &pb.CancelGuildApplicationResponse{}, nil
}

// ListMyGuildApplications:申请人视角的待审列表。读 RPC 要会话(列表是本人私有信息),但不查合服闸门 ——
// 合服窗口里读一眼自己申请了哪些帮会是安全的,拦掉只会让界面变成空白。
func (l *GuildLogic) ListMyGuildApplications(ctx context.Context, _ *pb.ListMyGuildApplicationsRequest) (*pb.ListMyGuildApplicationsResponse, error) {
	who := callerOf(ctx, 0)
	if !who.fromClient {
		return nil, status.Error(codes.PermissionDenied, "guild management requires a client session")
	}
	zoneID, tip, err := l.clientZone(ctx, who.playerID)
	if err != nil {
		return nil, err
	}
	if tip != nil {
		return &pb.ListMyGuildApplicationsResponse{ErrorMessage: tip}, nil
	}

	// 已入帮就没有"我的申请"(I2 通过时已删光他的全部申请)。缓存读到 >0 时复核一次再决定,
	// 否则陈旧映射会让一个其实没入帮的人永远看到空列表。
	cached, err := l.repo.GetPlayerGuildID(ctx, who.playerID)
	if err != nil {
		return nil, err
	}
	if cached > 0 {
		verified, err := l.repo.VerifyPlayerGuildID(ctx, who.playerID, cached)
		if err != nil {
			return nil, err
		}
		if verified > 0 {
			return &pb.ListMyGuildApplicationsResponse{}, nil
		}
	}

	rows, err := l.repo.ListMyApplications(ctx, who.playerID, nowMs(), myApplicationsLimit)
	if err != nil {
		return nil, err
	}
	views, err := l.myApplicationViews(ctx, rows, zoneID)
	if err != nil {
		return nil, err
	}
	return &pb.ListMyGuildApplicationsResponse{Applications: views}, nil
}

// myApplicationViews 把申请人视角的申请行装配成视图,并**一次**批量补上各帮的帮主名(B3b,
// 90-consistency.md Y-07)。
//
// 从 ListMyGuildApplications 里拆出来只为可测:那个 RPC 的前半段(ListMyApplications)要 MySQL,
// 而装配这一段只读帮会缓存与名字 resolver,单测用 miniredis + 假 resolver 就能覆盖。
// zoneID 是请求者的归属区,过滤口径与 visibleIn 一致。
func (l *GuildLogic) myApplicationViews(ctx context.Context, rows []data.ApplicationRow, zoneID uint32) ([]*pb.GuildApplicationView, error) {
	views := make([]*pb.GuildApplicationView, 0, len(rows))
	leaderIDs := make([]uint64, 0, len(rows))
	for _, row := range rows {
		guild, err := l.repo.GetGuild(ctx, row.GuildID)
		if err != nil {
			// 这一条读不出来就整体失败:静默跳过会让玩家看到一份"少了几行"的列表,
			// 而他没有任何办法知道少了什么。
			return nil, err
		}
		// 帮会已解散、或回滚合服后留下的跨区残留行:不展示。残留行最多存在一个有效期(默认 72h)。
		if guild == nil || !visibleIn(guild, zoneID) {
			continue
		}
		views = append(views, &pb.GuildApplicationView{
			GuildId:     guild.GuildID,
			GuildName:   guild.Name,
			Level:       guild.Level,
			MemberCount: uint32(len(guild.Members)),
			MaxMembers:  guild.MaxMembers,
			LeaderId:    guild.LeaderID,
			// LeaderName 在循环之后一次批量回填。
			ApplyMs:  row.ApplyMs,
			ExpireMs: row.ExpireMs,
		})
		leaderIDs = append(leaderIDs, guild.LeaderID)
	}
	// 帮主名是展示字段:取不到就留空(resolveNames fail-open),不让列表失败。
	// 这与上面 GetGuild 失败即整体失败不矛盾 —— 少一行会误导玩家,少一个名字不会。
	names := l.resolveNames(ctx, leaderIDs)
	for _, view := range views {
		view.LeaderName = names[view.LeaderId]
	}
	return views, nil
}

// ListGuildApplications:审批人视角的待审列表。要长老及以上。
//
// 不查申请人的归属 zone:那是每人一次 data_service 调用,50 条就是 50 次往返。
// 跨区残留行在这里仍可见,审批通过时事务内会复核并删掉(§8.5)。
func (l *GuildLogic) ListGuildApplications(ctx context.Context, _ *pb.ListGuildApplicationsRequest) (*pb.ListGuildApplicationsResponse, error) {
	who := callerOf(ctx, 0)
	if !who.fromClient {
		return nil, status.Error(codes.PermissionDenied, "guild management requires a client session")
	}
	guildID, tip, err := l.operatorGuild(ctx, who.playerID)
	if err != nil || tip != nil {
		return &pb.ListGuildApplicationsResponse{ErrorMessage: tip}, err
	}

	// 授权读 MySQL 权威 role(非锁定读):缓存里的 role 在降权后会说谎整整一个 TTL。
	role, found, err := l.repo.MemberRole(ctx, guildID, who.playerID)
	if err != nil {
		return nil, err
	}
	if !found {
		l.verifyMapping(ctx, who.playerID, guildID)
		return &pb.ListGuildApplicationsResponse{
			ErrorMessage: tipErr(constants.ErrNotInGuild, "not a member of the guild"),
		}, nil
	}
	if constants.Rank(role) < constants.RankOfficer {
		return &pb.ListGuildApplicationsResponse{
			ErrorMessage: tipErr(constants.ErrRankTooLow, "officer rank required"),
		}, nil
	}

	rules, ok := applicationRulesFromTable()
	if !ok {
		return nil, status.Errorf(codes.Internal, "GuildRule row %d missing", guildRuleRowID)
	}
	rows, err := l.repo.ListApplicants(ctx, guildID, nowMs(), rules.MaxPerGuild)
	if err != nil {
		return nil, err
	}
	return &pb.ListGuildApplicationsResponse{Applicants: l.applicantViews(ctx, rows)}, nil
}

// applicantViews 把审批人视角的申请行装配成视图:在线状态一次 MGET、名字**一次**批量查询(B3b,
// 90-consistency.md Y-07),两者共用同一份 id 列表。
//
// 拆出来的理由同 myApplicationViews:ListGuildApplications 的前半段(MemberRole / ListApplicants)
// 要 MySQL,装配这一段不碰库。在线状态与名字都是展示字段,读不到就按离线 / 空名显示。
func (l *GuildLogic) applicantViews(ctx context.Context, rows []data.ApplicantRow) []*pb.GuildApplicantView {
	ids := make([]uint64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.PlayerID)
	}
	onlineMap := l.onlineResolver.BatchResolve(ctx, ids)
	names := l.resolveNames(ctx, ids)

	views := make([]*pb.GuildApplicantView, 0, len(rows))
	for _, row := range rows {
		views = append(views, &pb.GuildApplicantView{
			PlayerId: row.PlayerID,
			Name:     names[row.PlayerID],
			Online:   onlineMap[row.PlayerID],
			ApplyMs:  row.ApplyMs,
			ExpireMs: row.ExpireMs,
		})
	}
	return views
}

// ReviewGuildApplication:通过 / 拒绝一条入帮申请。
//
// 通过时必须复核申请人的归属 zone —— 申请行是 72 小时前写的,这期间他可能被合服搬到了别的区,
// 放进来就会出现一个"人在 A 区、帮在 B 区"的成员,而帮会的一切按 zone 隔离。
// data_service 查不到或报错一律不批(fail-closed):放错人进来要手工修数据,拒绝一次只是重试。
func (l *GuildLogic) ReviewGuildApplication(ctx context.Context, req *pb.ReviewGuildApplicationRequest) (*pb.ReviewGuildApplicationResponse, error) {
	who := callerOf(ctx, 0)
	if !who.fromClient {
		return nil, status.Error(codes.PermissionDenied, "guild management requires a client session")
	}
	applicant := req.GetApplicantPlayerId()
	if applicant == 0 {
		return reviewTip(constants.ErrApplicationNotFound, "applicant player id is zero"), nil
	}
	if applicant == who.playerID {
		return reviewTip(constants.ErrCannotTargetSelf, "cannot review own application"), nil
	}

	// 申请人的 zone 与审批人的 zone(clientWrite 里查)并行发起:两次 data_service 往返
	// 串起来会把同步预算翻倍,并行之后仍然是一次 ≤1500ms。
	// 拒绝分支不查:删掉一条申请在任何 zone 下都是安全的。
	var zoneCh chan applicantZoneResult
	if req.GetApprove() && l.homeZones != nil {
		zoneCh = make(chan applicantZoneResult, 1)
		safego.Go("guild.review.applicant_zone", func() {
			// 带缓冲的 channel:即使主流程已经因为闸门 / 会话提前返回,这次写入也不会阻塞,协程不泄漏。
			zone, err := l.homeZones.HomeZone(ctx, applicant)
			zoneCh <- applicantZoneResult{zone: zone, err: err}
		})
	}

	actor, _, tip, err := l.clientWrite(ctx)
	if err != nil {
		return nil, err
	}
	if tip != nil {
		return &pb.ReviewGuildApplicationResponse{ErrorMessage: tip}, nil
	}

	var applicantZone uint32
	if req.GetApprove() {
		if zoneCh == nil {
			// homeZones 没接线:clientWrite 已经会因为同样的原因拒绝,这里是第二道闸,
			// 防止将来有人把 clientWrite 改宽松之后这条路悄悄放行。
			return nil, status.Error(codes.Unavailable, "guild home zone lookup is not configured")
		}
		select {
		case r := <-zoneCh:
			if r.err != nil {
				return nil, r.err
			}
			if r.zone == 0 {
				logx.Infof("[guild] review refused: applicant %d has no home zone mapping", applicant)
				return reviewTip(constants.ErrApplicationNotFound, "applicant home zone unknown"), nil
			}
			applicantZone = r.zone
		case <-ctx.Done():
			// safego 兜住 panic 时 channel 不会有写入,这一支是那种情况下的收尾。
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}

	guildID, tip, err := l.operatorGuild(ctx, actor)
	if err != nil || tip != nil {
		return &pb.ReviewGuildApplicationResponse{ErrorMessage: tip}, err
	}
	res, err := l.repo.ReviewApplication(ctx, guildID, actor, applicant, req.GetApprove(), applicantZone, nowMs())
	if tip, err := l.mapWriteErr(ctx, actor, guildID, guildID, err); err != nil || tip != nil {
		return &pb.ReviewGuildApplicationResponse{ErrorMessage: tip}, err
	}
	if res.Approved {
		// 快照里已经含新成员,所以"全体除审批人"自然把申请人算进去了。
		l.notify(pb.GuildChangeKind_GUILD_CHANGE_KIND_MEMBER_JOINED, guildID, actor, applicant, membersExcept(res.Guild, actor))
	} else {
		// 拒绝只通知申请人本人:告诉全帮"谁被拒了"没有任何价值,只有社交伤害。
		l.notify(pb.GuildChangeKind_GUILD_CHANGE_KIND_APPLICATION_REJECTED, guildID, actor, applicant, []uint64{applicant})
	}
	return &pb.ReviewGuildApplicationResponse{Guild: l.guildInfoFor(ctx, res.Guild, actor)}, nil
}

// ── §12.4 GuildInfo 装配 ──────────────────────────────────────

// guildInfoFor = toProtoGuild + 只给本帮长老 / 帮主看的待审数。
//
// viewerID=0(内部调用)不算;不是本帮成员、或职位不到长老,一律留 0 ——
// 待审数是管理侧信息,任何人查一下就能看到"那个帮有多少人在申请"并不合适。
// 计数直接读 MySQL 且**不进缓存**:它随每次申请 / 审批变化,缓存它等于让角标长期不准;
// 失败只记日志、留 0,纯展示字段不值得让整次读失败。
func (l *GuildLogic) guildInfoFor(ctx context.Context, g *data.GuildData, viewerID uint64) *pb.GuildInfo {
	info := l.toProtoGuild(ctx, g)
	if info == nil || viewerID == 0 {
		return info
	}
	role, ok := memberRoleIn(g, viewerID)
	if !ok || constants.Rank(role) < constants.RankOfficer {
		return info
	}
	count, err := l.repo.CountLiveApplications(ctx, g.GuildID, nowMs())
	if err != nil {
		logx.Errorf("[guild] count pending applications of guild %d: %v", g.GuildID, err)
		return info
	}
	info.PendingApplicationCount = count
	return info
}

// memberRoleIn 在快照里找某人的 role。快照来自事务内的权威读,所以这里只用于
// "该不该给他看待审数"这种展示判断;任何授权判断都不许用它(缓存快照可能是旧的)。
func memberRoleIn(g *data.GuildData, playerID uint64) (uint32, bool) {
	if g == nil {
		return 0, false
	}
	for _, m := range g.Members {
		if m.PlayerID == playerID {
			return m.Role, true
		}
	}
	return 0, false
}

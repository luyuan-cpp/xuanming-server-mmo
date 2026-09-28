package logic

// 帮会活动的 logic 层:元宵灯会、中秋团圆(B6a),以及同道历练两个写 RPC 的桩(B6b 去桩)。
// 设计 docs/design/guild-phase2/06-activities.md §6.6–§6.15;以下订正效力高于 06 正文(06 写于死锁修复之前):
//   - 90-consistency Y-01:身份 = callerOf + operatorGuild;写 RPC 不查归属 zone(合服闸门在事务内按锁住的
//     guild.zone_id 判,part2 §2 第 10 条);读 RPC GetGuildActivities 保留 clientZone + visibleIn;
//     事务回"不是成员 / 帮会不存在"时经 mapWriteErr 调 VerifyPlayerGuildID 自愈映射。
//   - Y-02:依赖经函数式 Option WithActivities 一次装配;ActivityDeps 不带 Notifier(推送走 l.notify),发号器用 IDMinter.Mint。
//   - Y-03 / Y-04:不改 push.go(activity_changed 标签已在);提交后缓存失效由 repo 走 invalidateAfterCommit(opActivity)。
//   - 90 part2 §2 与 D6:事务只经 repo 的 inTx;写冲突(ErrWriteConflict)回 tip GuildBusyRetry,不回 gRPC 错误。
//   - 92-handoff §12.1 裁决 K:没有 HandlerBudget,提交后同步投递的预算从 ctx 截止时间倒推(复用 syncBudgetFor)。
//   - 92-handoff §12.2 / §12.4 的取锁纪律全部落在 data/activity_repo.go:本文件不开事务、不直接碰库锁,
//     只做"事务之外"的事 —— 身份、预检、发号、提交后投递、推送与视图。
//
// 四条贯穿全文件的纪律(与 economy_logic.go 同一套):
//
//  1. **预检只为快速失败和省号,权威判定在事务里**。帮会等级、入帮时长、今日次数都会在事务里按锁住的行复核;
//     这里读缓存 / 普通读,读失败时宁可交给事务裁决,也不凭一次读失败拒绝玩家。唯一例外是团圆人数:
//     它只在事务外数一次(C11,"曾经同时在线"),读不到就整体按故障返回(fail-closed,见 BatchResolveStrict)。
//  2. **业务拒绝回 tip,故障回 gRPC 错误**。gRPC 错误会让客户端进重连隔离,只留给配表坏、库 / Redis 故障。
//  3. **提交之后不再失败**。事务提交后的同步投递、推送、视图回读全部 best-effort:把一次回读失败报成 RPC 失败,
//     玩家会以为没点成功而再点,换来一句"今日已领"。
//  4. **物品奖励 fail-closed**:资产通道关闭(Loop 为 nil)或发号器不可用时,带物品的活动整次拒绝、什么都不写;
//     绝不"先给帮贡、物品以后再说"—— 没有重投循环,插进去的指令行永远到不了玩家手里。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"guild/internal/activity"
	"guild/internal/constants"
	"guild/internal/data"
	assetpb "proto/common/asset"
	base "proto/common/base"
	rollbackpb "proto/common/rollback"
	pb "proto/guild"
	"shared/assetop"
	tablepb "shared/generated/pb/table"
)

// ── 依赖 ──────────────────────────────────────────────────────

// ActivityDeps 是帮会活动 RPC 的全部外部依赖,由 guild.go 经 WithActivities 一次装配(90 Y-02)。
//
// 与 06 §6.6 原稿的差异:删 Notifier(推送走 l.notify,Y-02)、删 HandlerBudget(裁决 K)、
// 暂不放 LocatorRedis / Lobby / Match —— 它们只有 B6b 的历练用,现在加上只会是一组永远为 nil 的字段,
// 由 B6b-srv2 与去桩一起追加。
type ActivityDeps struct {
	// Repo 是活动事务与读查询的唯一入口。nil → WithActivities 不装配(五个 RPC 回 kGuildActivityNotOpen,见 activitiesNotWiredTip)。
	Repo *data.ActivityRepo
	// Loop 是资产重投循环,同步投递与后台重投共用(与 EconomyDeps.Loop 是同一个实例)。
	// nil = 资产通道关闭(AssetOp.Enabled=false,92 §12.1 裁决 D):带物品奖励的活动在发号之前回 kGuildAssetPending;
	// 没有物品奖励的活动(默认配表的灯会)照常 —— 它只动 guild 库。
	Loop *assetop.Loop
	// OpIDs 是 op_id 发号器(号段 guild_asset_op,无 snowflake 回退)。nil → 带物品奖励的写回 kGuildIdGenUnavailable。
	OpIDs IDMinter
	// Now 是业务时钟,一次请求只取一次(周期键、档期状态、租约、视图的 server_time_ms 都由它算)。nil → time.Now。
	Now func() time.Time
	// SyncBudget 是提交后同步投递的上限。0 → 2500ms(= assetop 的 OpBudget,与经济同值)。
	SyncBudget time.Duration
	// Lease 是插行时写下的租约(= AssetOp.LeaseMs):同步投递期间重投循环不领这一行。0 → 10s。
	Lease time.Duration
}

// WithActivities 注入活动依赖,写进 GuildLogic.activities(90 Y-02 的函数式 Option,与 WithEconomy / WithPlayerNames 同一写法)。
//
// 依赖挂在实例字段上、而不是包级表里:包级可变表(以 *GuildLogic 为键)写入后永不删除,每 new 一个 GuildLogic 就泄漏一条,
// 还让"这个实例有没有接线"变成隐式的全局状态(AGENTS.md §11.2 显式依赖)。字段只在 NewGuildLogic 执行 Option 时写一次,
// 之后只读,并发 RPC 读它不需要同步。
//
// d.Repo 为 nil 时**不装配**(字段保持 nil,五个 RPC 回 kGuildActivityNotOpen,见 activitiesNotWiredTip):
// 半装配的依赖比不装配更危险 —— 那会在某条分支上 nil 解引用,而不是明明白白地拒绝(同 WithEconomy)。
func WithActivities(d ActivityDeps) Option {
	return func(l *GuildLogic) {
		if d.Repo == nil {
			return
		}
		deps := d
		if deps.Now == nil {
			deps.Now = time.Now
		}
		if deps.SyncBudget <= 0 {
			deps.SyncBudget = defaultSyncBudget
		}
		if deps.Lease <= 0 {
			deps.Lease = defaultInsertLease
		}
		l.activities = &deps
	}
}

// activitiesNotWiredTip 是活动依赖未注入(guild.go 没传 WithActivities,或传进来的 Repo 为 nil)时五个 RPC 的统一答复。
//
// 为什么回 tip 而不是 gRPC 错误:gRPC 错误会让客户端把整个帮会界面当传输故障进重连隔离;对玩家而言"活动用不了"
// 与"活动没开"是同一件事,回 kGuildActivityNotOpen 让界面停在一句明确的提示上(与 guild.go 无条件装配的理由一致)。
// fail-closed:在任何仓储读写之前返回,什么都不写,也不看会话(未接线时连身份校验都没有意义)。
//
// 风险与补偿:这个码是业务拒绝(Tip 表 fault 列为空),故障指标看不见它,result label 也会记成 not_open。
// 生产由 guild.go 无条件装配、装配失败直接退出进程,走到这里只可能是装配代码被改坏 —— 所以每次都打 ERROR,让人看见。
func activitiesNotWiredTip(rpc string) *base.TipInfoMessage {
	logx.Errorf("[GuildActivity] %s refused: activity deps not wired (logic.WithActivities missing or Repo nil)", rpc)
	return tipErr(constants.ErrActivityNotOpen, "guild activities not wired")
}

// activityNow 取本次请求唯一的"现在",并截到毫秒。
// repo 由 NowMs 还原时刻再算周期键;这里先截好,logic(预检、视图)与 repo(事务)算出的 DayKey / GuildPeriodKey
// 就逐位相同,不会出现"预检按今天、事务按昨天"的切点错位。
func activityNow(d *ActivityDeps) time.Time {
	return time.UnixMilli(d.Now().UnixMilli())
}

// ── 公共前置 ──────────────────────────────────────────────────

// activityActor 是通过公共前置的调用者。guild / member 来自缓存快照(可能落后一个代次),
// 只用于预检与视图;授权一律在事务里按 MySQL 锁住的行复核。
type activityActor struct {
	playerID uint64
	guild    *data.GuildData
	member   data.MemberData
}

// activityPrelude 是五个活动 RPC 的公共前置(06 §6.7,按 90 Y-01 订正)。
//
//  1. 身份只认会话(callerOf);没有会话 → gRPC PermissionDenied(协议里没有 player_id,内部调用拿不出可信操作者)。
//  2. readPath(GetGuildActivities)查归属 zone,帮会不在本区按"未入帮"答复(Y-01 保留 clientZone + visibleIn);
//     写 RPC **不查**:S2 的入帮事务已保证"是成员 ⇒ 归属区一致",合服窗口由事务内闸门兜底,省下 1.5s 预算。
//  3. operatorGuild(缓存读到 0 时以 MySQL 复核)→ 帮会快照 → 快照里有本人。
//  4. 映射指着已不存在的帮、或快照里没有本人时,用 ResolvePlayerGuild 以 MySQL 复核并绕过缓存直读
//     (理由同 economyCaller:只纠映射治不了"映射对、快照旧"那一种,刚入帮的人会整整一个 TTL 点不了灯)。
//
// 返回约定同 economyCaller:err 非 nil = 故障;tip 非 nil = 业务拒绝;两者都为 nil 时 actor 完整可用。
func (l *GuildLogic) activityPrelude(ctx context.Context, readPath bool) (activityActor, *base.TipInfoMessage, error) {
	who := callerOf(ctx, 0)
	if !who.fromClient {
		return activityActor{}, nil, status.Error(codes.PermissionDenied, "guild activities require a client session")
	}
	var zoneID uint32
	if readPath {
		zone, tip, err := l.clientZone(ctx, who.playerID)
		if err != nil || tip != nil {
			return activityActor{}, tip, err
		}
		zoneID = zone
	}
	guildID, tip, err := l.operatorGuild(ctx, who.playerID)
	if err != nil || tip != nil {
		return activityActor{}, tip, err
	}
	g, err := l.repo.GetGuild(ctx, guildID)
	if err != nil {
		return activityActor{}, nil, fmt.Errorf("load guild %d: %w", guildID, err)
	}
	member, ok := memberOf(g, who.playerID)
	if !ok {
		fresh, err := l.repo.ResolvePlayerGuild(ctx, who.playerID)
		if err != nil {
			// 与经济前置同一口径(见 economy_logic.go 的 leftGuildWhileResolving):复核窗口里刚被踢 / 刚退帮
			// 是业务态变化,回未入帮 tip;只有 MySQL 仍说他在某个帮时才按故障抛出(fail-closed)。
			if tip := l.leftGuildWhileResolving(ctx, who.playerID, guildID, err); tip != nil {
				return activityActor{}, tip, nil
			}
			return activityActor{}, nil, fmt.Errorf("resolve guild of player %d (cached guild %d): %w", who.playerID, guildID, err)
		}
		if member, ok = memberOf(fresh, who.playerID); !ok {
			return activityActor{}, tipErr(constants.ErrNotInGuild, "not in any guild"), nil
		}
		if fresh.GuildID != guildID {
			logx.Infof("[GuildActivity] player %d guild mapping healed from stale %d to %d", who.playerID, guildID, fresh.GuildID)
		}
		g = fresh
	}
	// 别区的帮会与没有帮会同一答复(与 GetGuild 的口径一致:不向客户端透露"它在别的区")。
	if readPath && !visibleIn(g, zoneID) {
		return activityActor{}, tipErr(constants.ErrNotInGuild, "guild not visible in home zone"), nil
	}
	return activityActor{playerID: who.playerID, guild: g, member: member}, nil, nil
}

// ── 预检 ──────────────────────────────────────────────────────

// activityPrecheck 是写 RPC 预检通过后的结果,原样带进事务与提交后的视图。
type activityPrecheck struct {
	// row 是 PickForWrite 在本次 now 下选出、且正处于 Open 的配表行(只读,本请求内使用)。
	row *tablepb.GuildActivityTable
	// rule 是 GuildRule 规则行。
	rule *tablepb.GuildRuleTable
	// block 是前四项已判过的输入;团圆读完进度与人数后改写三个团圆字段再判一次第五项。
	block activity.BlockInput
	// used 是预检读到的本人今日已用次数(读失败为 0,交给事务裁决)。
	used uint32
	// bundle 是本活动的物品包;nil = 无物品。
	bundle *assetpb.AssetBundle
}

// precheckActivity 是灯会 / 团圆共用的预检(06 §6.11.1 第 2–6 步)。
//
// 顺序即玩家要先解决的顺序(activity.Blocked 的优先级):未开放 > 帮会等级 > 入帮时长 > 今日次数。
// 团圆人数(第五项)不在这里判:它要一次会话 Redis 批量读,排在前四项之后才不白读,由 claimGuildReunion 接着判。
// 物品通道(资产通道开没开、发号器)排在全部业务判定之后,见 prepareActivityReward —— 活动没开、人数不够时
// 回"资产通道关闭"只会误导玩家。
func (l *GuildLogic) precheckActivity(ctx context.Context, d *ActivityDeps, a activityActor, typ, reqID uint32, now time.Time) (activityPrecheck, *base.TipInfoMessage, error) {
	nowMs := uint64(now.UnixMilli())
	// 只接受"该类型当前选中行且 Open":客户端伪造同类型另一行(比如已结束档期)的 id 一律按未开放(06 §6.13 C15)。
	row, ok := activity.PickForWrite(activity.Rows(), typ, reqID, nowMs)
	if !ok {
		return activityPrecheck{}, tipErr(constants.ErrActivityNotOpen, "guild activity not open"), nil
	}
	rule, ok := activity.Rule()
	if !ok {
		// 启动期 ValidateTables 已保证规则行存在;运行期缺行 = 配表被错误替换,fail-closed。
		return activityPrecheck{}, nil, status.Errorf(codes.Internal, "GuildRule row %d missing", activity.RuleRowID)
	}
	pre := activityPrecheck{row: row, rule: rule}
	pre.used = l.activityUsedToday(ctx, d, a.playerID, row.GetId(), now)
	pre.block = activity.BlockInput{
		State:        activity.StateOpen, // PickForWrite 已保证
		GuildLevel:   a.guild.Level,
		MinLevel:     row.GetMinGuildLevel(),
		Joined:       activity.JoinedLongEnough(a.member.JoinTimeMs, nowMs, rule.GetActivityJoinMinHours()),
		JoinMinHours: rule.GetActivityJoinMinHours(),
		Used:         pre.used,
		DailyLimit:   row.GetDailyLimit(),
		Type:         row.GetType(),
		// 先按"已锁存"跳过团圆人数这一项(见函数头);团圆调用方读完进度与人数后改写再判。
		ThresholdReached: true,
	}
	if tipID, params := activity.Blocked(pre.block); tipID != 0 {
		return activityPrecheck{}, blockedTip(tipID, params), nil
	}

	bundle, err := activity.BuildRewardBundle(row.GetRewardId())
	if err != nil {
		// 启动期已校验奖励包;运行期构建失败 = 配表被错误热更,fail-closed(不能少发物品还报成功)。
		return activityPrecheck{}, nil, status.Errorf(codes.Internal, "GuildActivity[%d] reward bundle: %v", row.GetId(), err)
	}
	pre.bundle = bundle
	return pre, nil, nil
}

// activityUsedToday 预检读本人今日已用次数。读失败既不拒绝也不报故障:事务里带上限的计数 upsert 才是权威判据,
// 这一读只为省一次事务(与 contributionPrecheck 同一口径)。
func (l *GuildLogic) activityUsedToday(ctx context.Context, d *ActivityDeps, playerID uint64, activityID uint32, now time.Time) uint32 {
	usage, err := d.Repo.ActivityUsage(ctx, playerID, activity.DayKey(now))
	if err != nil {
		logx.Errorf("[GuildActivity] precheck usage read failed, leaving it to the transaction (player %d, activity %d): %v",
			playerID, activityID, err)
		return 0
	}
	return usage[activityID]
}

// blockedTip 把"不满足项"转成响应里的 tip。带参数的码(等级 [min_level]、入帮时长 [N]、人数 [online, threshold])
// 只放参数本身:客户端按位次填文案,多塞一段说明会顶掉占位符。不带参数的码沿用 tipErr 的"英文说明作唯一参数"惯例
// (只进日志与排障,文案里没有占位符)。
func blockedTip(id uint32, params []string) *base.TipInfoMessage {
	if len(params) > 0 {
		return &base.TipInfoMessage{Id: id, Parameters: params}
	}
	return tipErr(id, "guild activity blocked")
}

// u32Param 把数值格式化成 tip 参数(十进制),与 activity.Blocked 的参数格式一致。
func u32Param(v uint32) string { return strconv.FormatUint(uint64(v), 10) }

// ── 团圆人数 ──────────────────────────────────────────────────

// reunionEligible 返回快照里入帮已满 minHours 小时的成员:团圆只数他们(防"刷完活动就换帮再刷")。
// 快照最多落后一个代次;人数只用来开门、事务内不重数(06 §6.12.1 第 11 步,C11)。
func reunionEligible(g *data.GuildData, nowMs uint64, minHours uint32) []uint64 {
	if g == nil {
		return nil
	}
	ids := make([]uint64, 0, len(g.Members))
	for _, m := range g.Members {
		if activity.JoinedLongEnough(m.JoinTimeMs, nowMs, minHours) {
			ids = append(ids, m.PlayerID)
		}
	}
	return ids
}

// countReunionOnline 数"入帮满 N 小时且在线"的人数。用严格版:任何一人的状态读不到就回错误(fail-closed),
// 并且有独立超时,Redis 卡住时不会吃光 handler 预算(见 BatchResolveStrict)。
func (l *GuildLogic) countReunionOnline(ctx context.Context, g *data.GuildData, nowMs uint64, minHours uint32) (uint32, error) {
	online, err := l.onlineResolver.BatchResolveStrict(ctx, reunionEligible(g, nowMs, minHours))
	if err != nil {
		return 0, err
	}
	return uint32(len(online)), nil
}

// ── 物品奖励 ──────────────────────────────────────────────────

// activityReward 是一次参与要发的物品:repo 事务要的指令参数 + 提交后同步投递要的原包(同一份,不各自序列化)。
// 零值 = 本活动没有物品。
type activityReward struct {
	op     *data.ActivityRewardOp
	bundle *assetpb.AssetBundle
}

// prepareActivityReward 为带物品的参与准备指令:查通道、序列化、生成租约令牌、发号(06 §6.11.1 第 7 步)。
// 它排在全部业务判定之后、事务之前,所以被业务拒绝的请求既不烧号也不看通道。
//
// 资产通道关闭(Loop 为 nil)在发号之前拒绝,回 kGuildAssetPending(与捐献 / 兑换同一答复):
// 没有重投循环,插进去的物品指令永远到不了玩家手里(纪律 4)。令牌先于发号:令牌生成失败时不白烧一个 op_id
// (号段只进不退)。seq 行不在这里建:repo 在事务里、成员行锁之下建(92 §12.2 第 5 条),事务外建会与离帮删行重新成环。
func (l *GuildLogic) prepareActivityReward(ctx context.Context, d *ActivityDeps, playerID uint64, bundle *assetpb.AssetBundle, nowMs uint64) (activityReward, *base.TipInfoMessage, error) {
	if bundle == nil {
		return activityReward{}, nil, nil
	}
	if d.Loop == nil {
		return activityReward{}, assetChannelDisabledTip(), nil
	}
	payload, err := proto.Marshal(bundle)
	if err != nil {
		return activityReward{}, nil, fmt.Errorf("marshal activity reward bundle: %w", err)
	}
	token, err := newAssetOpToken()
	if err != nil {
		return activityReward{}, nil, err
	}
	opID, tip := mintActivityOpID(ctx, d, playerID)
	if tip != nil {
		return activityReward{}, tip, nil
	}
	return activityReward{
		op: &data.ActivityRewardOp{
			OpID:         opID,
			Payload:      payload,
			LeaseUntilMs: nowMs + durationMs(d.Lease),
			LeaseToken:   token,
		},
		bundle: bundle,
	}, nil, nil
}

// mintActivityOpID 取一个 op_id(同时作 correlation_id)。号段取不到就整体失败,**绝不**用 0 或自造 id:
// op_id 是资产指令主键,也是事后对账(scene 流水的 correlation_id)的唯一线索。失败回 kGuildIdGenUnavailable
// (Tip 表标了 fault)。与 mintAssetOpID 同一口径,不共用是因为那个函数绑在 EconomyDeps 上。
func mintActivityOpID(ctx context.Context, d *ActivityDeps, playerID uint64) (uint64, *base.TipInfoMessage) {
	if d.OpIDs == nil {
		logx.Errorf("[GuildActivity] asset op id minter not wired (player=%d)", playerID)
		return 0, tipErr(constants.ErrIDGenUnavailable, "asset op id generator unavailable")
	}
	id, err := d.OpIDs.Mint(ctx)
	if err != nil {
		logx.Errorf("[GuildActivity] asset op id generator refused to mint (player=%d): %v", playerID, err)
		return 0, tipErr(constants.ErrIDGenUnavailable, "asset op id generator unavailable")
	}
	if id == 0 {
		logx.Errorf("[GuildActivity] asset op id generator returned 0 (player=%d)", playerID)
		return 0, tipErr(constants.ErrIDGenUnavailable, "asset op id generator unavailable")
	}
	return id, nil
}

// ── 事务 ──────────────────────────────────────────────────────

// activityTxInput 组装 repo 事务的输入。时间、令牌、奖励包全部由这里给,repo 不读墙钟(显式依赖)。
func (l *GuildLogic) activityTxInput(a activityActor, pre activityPrecheck, rw activityReward, nowMs uint64) data.ActivityTxInput {
	return data.ActivityTxInput{
		PlayerID:     a.playerID,
		GuildID:      a.guild.GuildID,
		Activity:     pre.row,
		JoinMinHours: pre.rule.GetActivityJoinMinHours(),
		NowMs:        nowMs,
		Reward:       rw.op,
		// 合服闸门在事务内按锁住的 guild.zone_id 判(90 part2 §2 第 10 条),与经济、解散共用同一个实现:
		// 读不到闸门按封锁处理(fail-closed)。
		Fence: l.economyFence,
	}
}

// activityTxTip 把活动事务的错误翻成响应(06 §6.10,按 90 part2 §2 第 5 条订正)。
// 返回 (tip, fault):tip 非 nil = 业务拒绝;fault 非 nil = 故障。入参 err 为 nil 时两者皆空。
//
// online / threshold 只给团圆的"人数不足"做参数,取预检时数到的值(事务内不重数)。
func (l *GuildLogic) activityTxTip(ctx context.Context, a activityActor, pre activityPrecheck, online, threshold uint32, err error) (*base.TipInfoMessage, error) {
	switch {
	case err == nil:
		return nil, nil
	case errors.Is(err, data.ErrGuildLevelTooLow):
		return blockedTip(constants.ErrActivityLevelTooLow, []string{u32Param(pre.row.GetMinGuildLevel())}), nil
	case errors.Is(err, data.ErrActivityJoinTooRecent):
		return blockedTip(constants.ErrActivityJoinTooRecent, []string{u32Param(pre.rule.GetActivityJoinMinHours())}), nil
	case errors.Is(err, data.ErrActivityAlreadyClaimed):
		return tipErr(constants.ErrActivityAlreadyClaimed, "guild activity daily limit reached"), nil
	case errors.Is(err, data.ErrActivityThresholdNotReached):
		return blockedTip(constants.ErrActivityThresholdNotReached, []string{u32Param(online), u32Param(threshold)}), nil
	case errors.Is(err, assetop.ErrTooManyPending):
		// 守卫拒绝不是故障:整事务已回滚,什么都没提交,玩家待发的物品到账几件后再点即可(06 §6.9)。
		logx.Infof("[GuildActivity] player %d has too many pending asset ops: %v", a.playerID, err)
		return tipErr(constants.ErrAssetPending, "too many pending asset ops"), nil
	case errors.Is(err, data.ErrActivityPoison):
		// 帮贡 / 资金相加会溢出:确定性失败,重试也一样。配表有上限,实际不可达;出现就是数据坏了,必须让人看见。
		logx.Errorf("[GuildActivity] deterministic failure (guild %d, player %d, activity %d): %v",
			a.guild.GuildID, a.playerID, pre.row.GetId(), err)
		return nil, status.Error(codes.Internal, "guild activity reward would overflow")
	default:
		// 帮会不存在 / 不是成员(mapWriteErr 内先以 VerifyPlayerGuildID 自愈映射,Y-01)、合服闸门(kGuildZoneMerging)、
		// 写冲突(kGuildBusyRetry,D6)与其余故障,统一交给管理类写 RPC 的映射,不另写一份。
		return l.mapWriteErr(ctx, a.playerID, a.guild.GuildID, a.guild.GuildID, err)
	}
}

// ── 提交之后 ──────────────────────────────────────────────────

// afterActivityCommit 做提交之后的三件事:同步投递物品一次、计指标、达阈值时推送。全部 best-effort(纪律 3)。
func (l *GuildLogic) afterActivityCommit(ctx context.Context, d *ActivityDeps, a activityActor, rw activityReward, res data.ActivityTxResult, typeLabel string) {
	if res.Enqueued && rw.op != nil {
		guildActivityRewardTotal.Inc(typeLabel, activityRewardEnqueued)
		l.deliverActivityReward(ctx, d, assetop.Op{
			OpID:          rw.op.OpID,
			PlayerID:      a.playerID,
			Stream:        assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT,
			Seq:           res.Seq,
			StreamEpoch:   res.StreamEpoch,
			CorrelationID: rw.op.OpID,
			TxType:        uint32(rollbackpb.TransactionType_TX_GUILD_ACTIVITY_REWARD),
			Bundle:        rw.bundle,
			// 活动奖励永不中止(06 §6.9、契约偏差 5):物品属于玩家,背包满就保持 PENDING 等腾出空间后自动到账。
			DeadlineMs: 0,
			LeaseToken: rw.op.LeaseToken,
		})
	} else {
		guildActivityRewardTotal.Inc(typeLabel, activityRewardNoItems)
	}
	if res.FundsGranted {
		guildActivityFundsGrantedTotal.Inc(typeLabel)
	}
	// 只在本档期首次达阈值时推(06 §6.11.3 第 3 步):每盏灯都推会把一次点击放大成 N×M 条推送。
	// 操作者不推,他手上的回包就是最新状态;收件人取缓存快照,最多漏推 / 多推一个刚进出的人,推送本来就是至多一次。
	if res.ReachedNow {
		l.notify(pb.GuildChangeKind_GUILD_CHANGE_KIND_ACTIVITY_CHANGED, a.guild.GuildID, a.playerID, 0, membersExcept(a.guild, a.playerID))
	}
}

// deliverActivityReward 在事务提交之后同步投一次,让玩家当场看到物品到账(06 §6.6,预算按裁决 K)。
//
// 预算 = min(SyncBudget, ctx 剩余 − 1000ms 尾巴),不足 300ms 就不投、计 guild_asset_sync_skipped_total{kind="activity"}:
// 行已带着插行时的租约落库,租约到期后由重投循环接手(C2 / C3)。错误只打日志:结局以视图回读为准。
// withSyncDelivery 让本次终结不再推送 DELIVERY_DONE —— 调用方自己拿着回包。
func (l *GuildLogic) deliverActivityReward(ctx context.Context, d *ActivityDeps, op assetop.Op) {
	if d.Loop == nil {
		// prepareActivityReward 已在 Loop 为 nil 时于发号前拒绝带物品的参与,走到这里是接线被改错了;
		// 行仍在库里,记 ERROR 让人看见。
		logx.Errorf("[GuildActivity] asset loop not wired, reward op left pending op_id=%d", op.OpID)
		return
	}
	budget, ok := syncBudgetFor(ctx, d.SyncBudget)
	if !ok {
		guildAssetSyncSkippedTotal.Inc(assetKindActivity)
		logx.Infof("[GuildActivity] sync delivery skipped, budget %v too small, left to reconcile loop op_id=%d", budget, op.OpID)
		return
	}
	deliverCtx, cancel := context.WithTimeout(withSyncDelivery(ctx), budget)
	defer cancel()
	if _, err := d.Loop.ProcessOne(deliverCtx, op); err != nil {
		logx.Errorf("[GuildActivity] sync delivery failed, outcome decided by readback op_id=%d: %v", op.OpID, err)
	}
}

// ── 视图 ──────────────────────────────────────────────────────

// activityViewData 是装配视图要的全部现成数据,都按同一个 now 读好。map 里没有的键按零值处理
// (没计数行 = 今日 0 次;没进度行 = 未达阈值、资金未发;没待发行 = 无待发)。
type activityViewData struct {
	now      time.Time
	rule     *tablepb.GuildRuleTable
	guild    *data.GuildData
	member   data.MemberData
	usage    map[uint32]uint32
	progress map[data.ProgressKey]activity.Progress
	rewards  map[uint32]data.RewardStatus
	// reunionOnline 只对未锁存的开放团圆行有意义(可见行里每种类型至多一行,所以一个数就够)。
	reunionOnline uint32
}

// buildActivityView 装配一行视图。规则(状态、不满足项、团圆锁存后进度填 0 …)全在 activity.BuildView 里,
// 这里只负责按键取数。
func buildActivityView(row *tablepb.GuildActivityTable, v activityViewData) (*pb.GuildActivityView, error) {
	id := row.GetId()
	reward := v.rewards[id]
	var level uint32
	if v.guild != nil {
		level = v.guild.Level
	}
	return activity.BuildView(row, activity.ViewInput{
		Now:                     v.now,
		Rule:                    v.rule,
		GuildLevel:              level,
		JoinTimeMs:              v.member.JoinTimeMs,
		MyUsedCount:             v.usage[id],
		Progress:                v.progress[data.ProgressKey{ActivityID: id, PeriodKey: activity.GuildPeriodKey(row, v.now)}],
		ReunionOnline:           v.reunionOnline,
		MyPendingRewardCount:    reward.PendingCount,
		MyPendingReasonTipID:    reward.PendingReasonTipID,
		MyLastRewardRejectTipID: reward.LastRejectTipID,
		// B6a 桩:历练的依赖(match 内部服务、邀请房间)还不存在,视图强制 DISABLED + 未开放,
		// 不让界面显示一个点了必失败的按钮(06 §6.7 末段)。B6b 改为"d.Match == nil || d.Lobby == nil"。
		TrialUnavailable: true,
	})
}

// activityViews 读齐视图数据(06 §6.8 第 2–7 步)再逐行装配。
//
// 失败口径:MySQL 读失败返回错误(整页拿不到比拿到错的次数好);团圆在线人数读不到降级为 0 并计指标
// (只读展示,错一个数字不值得让整页失败);单行装配失败(只可能是奖励配表被错误热更)只藏掉那一行并记 ERROR。
func (l *GuildLogic) activityViews(ctx context.Context, d *ActivityDeps, a activityActor, rule *tablepb.GuildRuleTable, now time.Time) ([]*pb.GuildActivityView, error) {
	nowMs := uint64(now.UnixMilli())
	rows := activity.SelectVisible(activity.Rows(), nowMs)
	if len(rows) == 0 {
		return nil, nil
	}
	usage, err := d.Repo.ActivityUsage(ctx, a.playerID, activity.DayKey(now))
	if err != nil {
		return nil, err
	}
	keys := make([]data.ProgressKey, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, data.ProgressKey{ActivityID: row.GetId(), PeriodKey: activity.GuildPeriodKey(row, now)})
	}
	progress, err := d.Repo.GuildProgress(ctx, a.guild.GuildID, keys)
	if err != nil {
		return nil, err
	}
	rewards, err := d.Repo.RewardStatuses(ctx, a.playerID, nowMs)
	if err != nil {
		return nil, err
	}

	vd := activityViewData{
		now:           now,
		rule:          rule,
		guild:         a.guild,
		member:        a.member,
		usage:         usage,
		progress:      progress,
		rewards:       rewards,
		reunionOnline: l.reunionOnlineForView(ctx, rows, progress, a.guild, rule, now),
	}
	views := make([]*pb.GuildActivityView, 0, len(rows))
	for _, row := range rows {
		view, err := buildActivityView(row, vd)
		if err != nil {
			logx.Errorf("[GuildActivity] build view of GuildActivity[%d] failed, row hidden: %v", row.GetId(), err)
			continue
		}
		views = append(views, view)
	}
	return views, nil
}

// reunionOnlineForView 给视图数团圆的合格在线人数:只有"开放且本档期未锁存"的团圆行需要(锁存后进度填 0,不必再数)。
// 用严格版而不是 BatchResolve:同样的判据,但有独立超时 —— BatchResolve 在 Redis 卡住时会把整页拖到约 6s。
// 读不到就降级为 0:这只是展示,真正领奖时 claimGuildReunion 会再数一次,读不到按故障返回。
func (l *GuildLogic) reunionOnlineForView(ctx context.Context, rows []*tablepb.GuildActivityTable, progress map[data.ProgressKey]activity.Progress,
	g *data.GuildData, rule *tablepb.GuildRuleTable, now time.Time) uint32 {
	nowMs := uint64(now.UnixMilli())
	for _, row := range rows {
		if row.GetType() != activity.TypeReunion || activity.StateOf(row, nowMs) != activity.StateOpen {
			continue
		}
		if progress[data.ProgressKey{ActivityID: row.GetId(), PeriodKey: activity.GuildPeriodKey(row, now)}].Latched() {
			return 0
		}
		online, err := l.countReunionOnline(ctx, g, nowMs, rule.GetActivityJoinMinHours())
		if err != nil {
			guildActivityOnlineLookupFailedTotal.Inc(activityOnlinePathView)
			logx.Errorf("[GuildActivity] count reunion online for view failed, showing 0 (guild %d): %v", g.GuildID, err)
			return 0
		}
		return online
	}
	return 0
}

// committedActivityView 在提交(与同步投递)之后重建本活动的视图(06 §6.11.3 第 4 步:只对本行做第 3、4、7 步)。
//
// 进度直接用事务读回的那一行(res.Progress,已含本次 +1 / 锁存 / 资金标记),不再读库。
// 今日次数与待发状态要回读:回读失败只记日志,次数退回"预检值 + 本次 1 次",待发状态留空 —— 写已经提交,
// 回包里少一个准数只是让客户端下次刷新时补上,不能把一次成功的参与报成失败(纪律 3)。
func (l *GuildLogic) committedActivityView(ctx context.Context, d *ActivityDeps, a activityActor, pre activityPrecheck, now time.Time, res data.ActivityTxResult) *pb.GuildActivityView {
	id := pre.row.GetId()
	used := pre.used + 1
	if usage, err := d.Repo.ActivityUsage(ctx, a.playerID, res.DayKey); err != nil {
		logx.Errorf("[GuildActivity] reread usage after commit failed (player %d, activity %d): %v", a.playerID, id, err)
	} else {
		used = usage[id]
	}
	rewards, err := d.Repo.RewardStatuses(ctx, a.playerID, uint64(now.UnixMilli()))
	if err != nil {
		logx.Errorf("[GuildActivity] reread reward status after commit failed (player %d, activity %d): %v", a.playerID, id, err)
		rewards = nil
	}
	view, err := buildActivityView(pre.row, activityViewData{
		now:      now,
		rule:     pre.rule,
		guild:    a.guild,
		member:   a.member,
		usage:    map[uint32]uint32{id: used},
		progress: map[data.ProgressKey]activity.Progress{{ActivityID: id, PeriodKey: res.GuildPeriodKey}: res.Progress},
		rewards:  rewards,
	})
	if err != nil {
		logx.Errorf("[GuildActivity] build view after commit failed (player %d, activity %d): %v", a.playerID, id, err)
		return nil
	}
	return view
}

// ── 指标 ──────────────────────────────────────────────────────

// activityResultOf 从响应推出 result label:故障看 gRPC 码,业务拒绝看 tip id,都为空是 ok。
// 未列出的 tip 归 other_reject,保证 label 集合有界。
func activityResultOf(tip *base.TipInfoMessage, err error) string {
	if err != nil {
		switch status.Code(err) {
		case codes.PermissionDenied:
			return activityResultDenied
		case codes.Unavailable:
			return activityResultUnavailable
		default:
			return activityResultError
		}
	}
	if tip == nil {
		return activityResultOK
	}
	switch tip.GetId() {
	case constants.ErrActivityNotOpen:
		return activityResultNotOpen
	case constants.ErrActivityLevelTooLow:
		return activityResultLevelLow
	case constants.ErrActivityJoinTooRecent:
		return activityResultJoinRecent
	case constants.ErrActivityAlreadyClaimed:
		return activityResultClaimed
	case constants.ErrActivityThresholdNotReached:
		return activityResultThreshold
	case constants.ErrAssetPending:
		return activityResultAssetPending
	case constants.ErrZoneMerging:
		return activityResultMerging
	case constants.ErrNotInGuild, constants.ErrGuildNotFound:
		return activityResultNotInGuild
	case constants.ErrHomeZoneUnknown:
		return activityResultHomeZone
	case constants.ErrBusyRetry:
		return activityResultBusyRetry
	case constants.ErrIDGenUnavailable:
		return activityResultIDUnavailable
	default:
		return activityResultOtherReject
	}
}

// ── RPC:查询(06 §6.8)──────────────────────────────────────

// GetGuildActivities:活动页。每种类型至多一条、按 type 升序;不查合服闸门(只读)。
func (l *GuildLogic) GetGuildActivities(ctx context.Context, req *pb.GetGuildActivitiesRequest) (*pb.GetGuildActivitiesResponse, error) {
	resp, err := l.getGuildActivities(ctx, req)
	guildActivityActionTotal.Inc(activityTypeAll, activityActionView, activityResultOf(resp.GetErrorMessage(), err))
	return resp, err
}

func (l *GuildLogic) getGuildActivities(ctx context.Context, _ *pb.GetGuildActivitiesRequest) (*pb.GetGuildActivitiesResponse, error) {
	d := l.activities
	if d == nil {
		return &pb.GetGuildActivitiesResponse{ErrorMessage: activitiesNotWiredTip("GetGuildActivities")}, nil
	}
	now := activityNow(d)
	a, tip, err := l.activityPrelude(ctx, true)
	if err != nil {
		return nil, err
	}
	if tip != nil {
		return &pb.GetGuildActivitiesResponse{ErrorMessage: tip}, nil
	}
	rule, ok := activity.Rule()
	if !ok {
		return nil, status.Errorf(codes.Internal, "GuildRule row %d missing", activity.RuleRowID)
	}
	views, err := l.activityViews(ctx, d, a, rule, now)
	if err != nil {
		return nil, err
	}
	return &pb.GetGuildActivitiesResponse{Activities: views}, nil
}

// ── RPC:元宵灯会(06 §6.11)─────────────────────────────────

// LightGuildLantern:点灯。流程:前置 → 预检 → [发号] → 事务(repo.LightLanternTx)→ 同步投递 → 推送 → 本行视图。
func (l *GuildLogic) LightGuildLantern(ctx context.Context, req *pb.LightGuildLanternRequest) (*pb.LightGuildLanternResponse, error) {
	resp, err := l.lightGuildLantern(ctx, req)
	guildActivityActionTotal.Inc(activityTypeLantern, activityActionLight, activityResultOf(resp.GetErrorMessage(), err))
	return resp, err
}

func (l *GuildLogic) lightGuildLantern(ctx context.Context, req *pb.LightGuildLanternRequest) (*pb.LightGuildLanternResponse, error) {
	d := l.activities
	if d == nil {
		return &pb.LightGuildLanternResponse{ErrorMessage: activitiesNotWiredTip("LightGuildLantern")}, nil
	}
	now := activityNow(d)
	nowMs := uint64(now.UnixMilli())
	a, tip, err := l.activityPrelude(ctx, false)
	if err != nil {
		return nil, err
	}
	if tip != nil {
		return &pb.LightGuildLanternResponse{ErrorMessage: tip}, nil
	}
	pre, tip, err := l.precheckActivity(ctx, d, a, activity.TypeLantern, req.GetActivityId(), now)
	if err != nil {
		return nil, err
	}
	if tip != nil {
		return &pb.LightGuildLanternResponse{ErrorMessage: tip}, nil
	}
	rw, tip, err := l.prepareActivityReward(ctx, d, a.playerID, pre.bundle, nowMs)
	if err != nil {
		return nil, err
	}
	if tip != nil {
		return &pb.LightGuildLanternResponse{ErrorMessage: tip}, nil
	}

	res, err := d.Repo.LightLanternTx(ctx, l.activityTxInput(a, pre, rw, nowMs))
	if err != nil {
		tip, fault := l.activityTxTip(ctx, a, pre, 0, 0, err)
		if fault != nil {
			return nil, fault
		}
		return &pb.LightGuildLanternResponse{ErrorMessage: tip}, nil
	}
	l.afterActivityCommit(ctx, d, a, rw, res, activityTypeLantern)
	return &pb.LightGuildLanternResponse{Activity: l.committedActivityView(ctx, d, a, pre, now, res)}, nil
}

// ── RPC:中秋团圆(06 §6.12)─────────────────────────────────

// ClaimGuildReunion:团圆领奖。与点灯的差别只在预检多一步"本档期未锁存时数合格在线人数"(06 §6.12.1 第 8–11 步):
// 人数在事务外数一次,只用来开门(observed 交给事务);已锁存则不再数,之后每人每游戏日照常领。
func (l *GuildLogic) ClaimGuildReunion(ctx context.Context, req *pb.ClaimGuildReunionRequest) (*pb.ClaimGuildReunionResponse, error) {
	resp, err := l.claimGuildReunion(ctx, req)
	guildActivityActionTotal.Inc(activityTypeReunion, activityActionClaim, activityResultOf(resp.GetErrorMessage(), err))
	return resp, err
}

func (l *GuildLogic) claimGuildReunion(ctx context.Context, req *pb.ClaimGuildReunionRequest) (*pb.ClaimGuildReunionResponse, error) {
	d := l.activities
	if d == nil {
		return &pb.ClaimGuildReunionResponse{ErrorMessage: activitiesNotWiredTip("ClaimGuildReunion")}, nil
	}
	now := activityNow(d)
	nowMs := uint64(now.UnixMilli())
	a, tip, err := l.activityPrelude(ctx, false)
	if err != nil {
		return nil, err
	}
	if tip != nil {
		return &pb.ClaimGuildReunionResponse{ErrorMessage: tip}, nil
	}
	pre, tip, err := l.precheckActivity(ctx, d, a, activity.TypeReunion, req.GetActivityId(), now)
	if err != nil {
		return nil, err
	}
	if tip != nil {
		return &pb.ClaimGuildReunionResponse{ErrorMessage: tip}, nil
	}

	// 本档期进度(普通读)。读失败按故障返回:不知道是否已锁存,就无法决定要不要数人,也就无法给出正确答复。
	threshold := activity.ReunionThreshold(pre.row, pre.rule)
	key := data.ProgressKey{ActivityID: pre.row.GetId(), PeriodKey: activity.GuildPeriodKey(pre.row, now)}
	progress, err := d.Repo.GuildProgress(ctx, a.guild.GuildID, []data.ProgressKey{key})
	if err != nil {
		return nil, err
	}
	latched := progress[key].Latched()
	var online uint32
	if !latched {
		// 严格版:任何一人的在线状态读不到就整体按故障返回,不当离线少算、更不当在线多算(锁存不可撤销)。
		online, err = l.countReunionOnline(ctx, a.guild, nowMs, pre.rule.GetActivityJoinMinHours())
		if err != nil {
			guildActivityOnlineLookupFailedTotal.Inc(activityOnlinePathClaim)
			return nil, fmt.Errorf("count reunion online of guild %d: %w", a.guild.GuildID, err)
		}
	}
	block := pre.block
	block.ThresholdReached, block.Progress, block.Threshold = latched, online, threshold
	if tipID, params := activity.Blocked(block); tipID != 0 {
		return &pb.ClaimGuildReunionResponse{ErrorMessage: blockedTip(tipID, params)}, nil
	}
	// 未锁存时走到这里必是 observed=true;已锁存时 observed 恒为 false,事务按锁住的进度行不再看它。
	observed := activity.ReunionObserved(online, threshold)

	rw, tip, err := l.prepareActivityReward(ctx, d, a.playerID, pre.bundle, nowMs)
	if err != nil {
		return nil, err
	}
	if tip != nil {
		return &pb.ClaimGuildReunionResponse{ErrorMessage: tip}, nil
	}

	res, err := d.Repo.ClaimReunionTx(ctx, l.activityTxInput(a, pre, rw, nowMs), observed)
	if err != nil {
		tip, fault := l.activityTxTip(ctx, a, pre, online, threshold, err)
		if fault != nil {
			return nil, fault
		}
		return &pb.ClaimGuildReunionResponse{ErrorMessage: tip}, nil
	}
	l.afterActivityCommit(ctx, d, a, rw, res, activityTypeReunion)
	return &pb.ClaimGuildReunionResponse{Activity: l.committedActivityView(ctx, d, a, pre, now, res)}, nil
}

// ── RPC:同道历练(B6a 桩,06 §6.7 末段)──────────────────────

// StartGuildTrial:B6a 桩。公共前置照常走完(无会话、未入帮的答复与 B6b 落地后一致),然后恒回未开放。
// 消息号、白名单、限流已在 B6a 一次占好,B6b 只替换函数体。
func (l *GuildLogic) StartGuildTrial(ctx context.Context, _ *pb.StartGuildTrialRequest) (*pb.StartGuildTrialResponse, error) {
	tip, err := l.trialNotOpenYet(ctx, "StartGuildTrial")
	var resp *pb.StartGuildTrialResponse
	if err == nil {
		resp = &pb.StartGuildTrialResponse{ErrorMessage: tip}
	}
	guildActivityActionTotal.Inc(activityTypeTrial, activityActionInvite, activityResultOf(tip, err))
	return resp, err
}

// RespondGuildTrialInvite:B6a 桩,同 StartGuildTrial。
func (l *GuildLogic) RespondGuildTrialInvite(ctx context.Context, _ *pb.RespondGuildTrialInviteRequest) (*pb.RespondGuildTrialInviteResponse, error) {
	tip, err := l.trialNotOpenYet(ctx, "RespondGuildTrialInvite")
	var resp *pb.RespondGuildTrialInviteResponse
	if err == nil {
		resp = &pb.RespondGuildTrialInviteResponse{ErrorMessage: tip}
	}
	guildActivityActionTotal.Inc(activityTypeTrial, activityActionRespond, activityResultOf(tip, err))
	return resp, err
}

// trialNotOpenYet 是两个历练写 RPC 的共同桩体:返回 (tip, err),tip 恒非 nil 当 err 为 nil。rpc 只进日志。
func (l *GuildLogic) trialNotOpenYet(ctx context.Context, rpc string) (*base.TipInfoMessage, error) {
	if l.activities == nil {
		return activitiesNotWiredTip(rpc), nil
	}
	_, tip, err := l.activityPrelude(ctx, false)
	if err != nil {
		return nil, err
	}
	if tip != nil {
		return tip, nil
	}
	return tipErr(constants.ErrActivityNotOpen, "guild trial not open"), nil
}

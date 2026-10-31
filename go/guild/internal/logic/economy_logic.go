package logic

// 帮会经济的 logic 层:捐献、升级、商店与两个读 RPC(docs/design/guild-phase2/05-economy.md
// §5.24–§5.30;顶部三个覆盖块、90-consistency.md 与 B5b 实现契约效力高于正文)。
//
// 四条贯穿全文件的纪律:
//
//  1. **身份只认会话,帮会只认复核过的映射**(Y-01):economyCaller = callerOf + operatorGuild,
//     快照里没有本人时以 MySQL 复核(ResolvePlayerGuild);事务回"不是成员 / 帮会不存在"时经 mapWriteErr
//     调 VerifyPlayerGuildID 自愈。经济 RPC **不查**归属 zone(R4):S2 的申请事务已保证"是成员 ⇒ 归属区一致",
//     省下的同步预算留给 scene 同步投递。
//  2. (已删:事务内的合服闸门。别名合服不再有闸门,公会合并任务在维护窗口里只影响公会自己;其余纪律编号不变)
//  3. **PENDING 不是错误**:scene 暂时不可达时指令留在 outbox 由重投循环接手,玩家看到的是视图里的
//     "结算中",不是 error_message —— 客户端遇到任何非 0 tip 都会中断后续刷新(05 §5.4)。
//  4. **业务拒绝回 tip,故障回 gRPC 错误**(沿用 guild_manage_logic.go 的纪律 3)。

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"guild/internal/constants"
	"guild/internal/data"
	assetpb "proto/common/asset"
	base "proto/common/base"
	rollbackpb "proto/common/rollback"
	pb "proto/guild"
	scenepb "proto/scene"
	"shared/assetop"
	"shared/gameday"
	tablepb "shared/generated/pb/table"
	"shared/generated/table"
)

// ── 依赖与常量 ────────────────────────────────────────────────

// EconomyDeps 是经济 RPC 的全部外部依赖,由 guild.go 经 WithEconomy 一次装配。
type EconomyDeps struct {
	// Repo 是经济事务与读查询的唯一入口。为 nil 时 WithEconomy 不装配(五个 RPC 回 Unavailable)。
	Repo *data.EconomyRepo
	// Loop 是重投循环,同步投递与后台重投共用它的 ProcessOne。
	// nil = 资产通道关闭(AssetOp.Enabled=false,裁决 D):捐献 / 兑换在发号与建行之前就拒绝,
	// 升级与两个读 RPC 照常 —— 没有签名器就无法同步投递,建出来的行只会卡在 PENDING。
	Loop *assetop.Loop
	// OpIDs 是 op_id 的发号器(biz_tag guild_asset_op,不设 snowflake 回退)。
	// nil = 号段不可用:捐献 / 兑换回 ErrIDGenUnavailable,不自造 id。
	OpIDs IDMinter
	// Now 是本服务的业务时钟(周期键、截止时刻、租约都从同一个 now 算)。nil → time.Now。
	Now func() time.Time
	// SyncBudget 是同步投递的预算上限。0 → 2500ms(= assetop 的 OpBudget,05 §5.25)。
	SyncBudget time.Duration
	// Lease 是插行时写下的租约(= AssetOp.LeaseMs):同步投递期间重投循环不会领走这一行。0 → 10s。
	Lease time.Duration
}

const (
	defaultSyncBudget  = 2500 * time.Millisecond
	defaultInsertLease = 10 * time.Second
	// syncTailReserve 是同步投递之后必须留出的尾巴:assetop 落库用的 settleBudget(700ms,
	// 不继承取消,但会顺延在请求预算之后)+ 回读指令状态与编码回包(300ms)。
	// 预算从 ctx 截止时间倒推,不从"进 handler 起算":RequestBudget 拦截器已按 Timeout−500 设好截止(裁决 K)。
	syncTailReserve = 1000 * time.Millisecond
	// minSyncBudget:低于它就不做同步投递。assetop 单次 RPC 上限 800ms,300ms 连一次正常往返都不稳,
	// 硬投只会让这一行以"超时 → 退避"收场,不如直接交给重投循环(行上的租约到期即被领取)。
	minSyncBudget = 300 * time.Millisecond
	// recentResultWindowMs:两页的"最近结果"只展示 10 分钟内进入终态的指令(协议注释)。
	recentResultWindowMs uint64 = 600000
	// recentScanLimit 是最近结果回读的行数:最多扫 20 行、过滤后取前 recentResultLimit 条(05 §5.21)。
	recentScanLimit = 20
	// recentResultLimit:最近结果至多 5 条,新的在前(协议注释)。
	recentResultLimit = 5
)

// ── 指标(label 全是固定常量,绝不放 player_id / guild_id,AGENTS.md §9) ──

const (
	economyRPCDonateOptions = "get_donate_options"
	economyRPCDonate        = "donate"
	economyRPCUpgrade       = "upgrade"
	economyRPCShop          = "get_shop"
	economyRPCBuy           = "buy_shop_goods"
)

// result 的取值集合。写 RPC 成功时按指令视图状态计(applied / pending / …),读 RPC 成功计 ok,
// 升级 expected_level 不符(已被别人升过)计 unchanged。
const (
	resultOK             = "ok"
	resultUnchanged      = "unchanged"
	resultApplied        = "applied"
	resultPending        = "pending"
	resultRejected       = "rejected"
	resultAborted        = "aborted"
	resultAppliedPartial = "applied_partial"
	resultLimit          = "limit"
	resultInsufficient   = "insufficient"
	resultPendingGuard   = "pending_guard"
	resultBusyRetry      = "busy_retry"
	resultNotMember      = "not_member"
	resultLevel          = "level"
	resultRank           = "rank"
	resultNotFound       = "not_found"
	resultDisabled       = "disabled"
	resultIDUnavailable  = "id_unavailable"
	resultDenied         = "denied"
	resultUnavailable    = "unavailable"
	resultOtherReject    = "other_reject"
	resultError          = "error"
	// 客户端幂等键(withClientRequest):重发拿回了首次结果 / 同一个键换了参数被拒。
	resultReplayed        = "replayed"
	resultRequestConflict = "request_conflict"
)

// assetKind* 是 guild_asset_sync_skipped_total 的 kind 取值。
const (
	assetKindDonate = "donate"
	assetKindShop   = "shop"
)

var (
	guildEconomyRequestsTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "guild",
		Subsystem: "economy",
		Name:      "requests_total",
		Help:      "帮会经济 RPC 的结果。rpc: get_donate_options / donate / upgrade / get_shop / buy_shop_goods;result 为固定集合(见 economy_logic.go)。",
		Labels:    []string{"rpc", "result"},
	})

	guildAssetSyncSkippedTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "guild",
		Subsystem: "asset",
		Name:      "sync_skipped_total",
		Help:      "请求剩余预算不足、跳过同步投递的次数(行在租约到期后由重投循环接手)。持续上升说明预留事务或发号偏慢。",
		Labels:    []string{"kind"},
	})
)

// WithEconomy 注入经济依赖。d.Repo 为 nil 时**不装配**(l.economy 保持 nil,五个 RPC 回 Unavailable):
// 半装配的依赖比不装配更危险 —— 那会在某条分支上 nil 解引用,而不是明明白白地拒绝。
func WithEconomy(d EconomyDeps) Option {
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
		l.economy = &deps
	}
}

func errEconomyNotEnabled() error {
	return status.Error(codes.Unavailable, "guild economy not enabled")
}

// ── 同步投递标记 ──────────────────────────────────────────────

// syncDeliveryKey 标记"这次终结发生在 RPC 自己的同步投递里"。
//
// 标记挂在 ctx 上一路传到 Store.Finalize → OnFinalized:assetop 落库用的 settle ctx 是
// context.WithoutCancel(ctx) 派生的,只丢取消与截止、**保留值**,所以标记在那里仍然可见。
// 调用方自己会拿到回包,再推一条只会让客户端多拉一次。
type syncDeliveryKey struct{}

func withSyncDelivery(ctx context.Context) context.Context {
	return context.WithValue(ctx, syncDeliveryKey{}, true)
}

func isSyncDelivery(ctx context.Context) bool {
	marked, _ := ctx.Value(syncDeliveryKey{}).(bool)
	return marked
}

// ── 公共前置 ──────────────────────────────────────────────────

// economyCaller 是五个经济 RPC 的公共前置(Y-01)。
//
// 快路径:会话身份(callerOf)→ operatorGuild(缓存读到 0 时用 MySQL 复核)→ 帮会快照 → 快照里有本人。
// **不查归属 zone**(R4,见文件头纪律 1)。
//
// 坏路径(映射指着一个已不存在的帮,或快照里没有本人)交给 repo.ResolvePlayerGuild 以 MySQL 复核:
// 它先失效那份可疑的帮会快照、用 VerifyPlayerGuildID 纠正映射,再**绕过缓存**直读权威快照。
// 为什么不只纠正映射就回未入帮:坏路径有两种成因,只纠映射只治得了一种 ——
//   - 映射陈旧(离帮后入了别的帮,审批提交后映射失效失败):纠正映射就够;
//   - 映射是对的、快照陈旧(审批通过后帮会快照失效失败):映射本来就对,纠了等于没纠,
//     刚入帮的玩家会在整个快照 TTL(30 分钟)里捐献 / 兑换 / 打开商店全部回"未入帮",且没有任何自愈路径。
//
// 客户端 GetPlayerGuild 走的是同一个函数(guild_logic.go),两处对"我在哪个帮"的回答因此一致。
// 代价只落在坏路径:几次缓存读 + 两三次主键 / 唯一索引点查。授权仍一律在事务里按锁住的行复核,这里的快照只用于预判与展示。
//
// ResolvePlayerGuild 报错时不直接当故障抛出,先经 leftGuildWhileResolving 以 MySQL 再判一次"此刻还在不在帮":
// 复核映射与直读快照之间恰好被踢 / 退帮,它回的错误与真矛盾长得一样,见该函数注释。
// 整条坏路径只有普通读(不加锁读)与缓存失效,不开事务,不影响任何事务的取锁顺序。
//
// 返回约定同 clientWrite:err 非 nil = 故障;tip 非 nil = 业务拒绝;两者都为 nil 时 g 非 nil 且快照里有本人。
func (l *GuildLogic) economyCaller(ctx context.Context) (uint64, *data.GuildData, *base.TipInfoMessage, error) {
	who := callerOf(ctx, 0)
	if !who.fromClient {
		return 0, nil, nil, status.Error(codes.PermissionDenied, "guild economy requires a client session")
	}
	guildID, tip, err := l.operatorGuild(ctx, who.playerID)
	if err != nil || tip != nil {
		return who.playerID, nil, tip, err
	}
	g, err := l.repo.GetGuild(ctx, guildID)
	if err != nil {
		return who.playerID, nil, nil, fmt.Errorf("load guild %d: %w", guildID, err)
	}
	if _, ok := memberOf(g, who.playerID); ok {
		return who.playerID, g, nil, nil
	}
	fresh, err := l.repo.ResolvePlayerGuild(ctx, who.playerID)
	if err != nil {
		if tip = l.leftGuildWhileResolving(ctx, who.playerID, guildID, err); tip != nil {
			return who.playerID, nil, tip, nil
		}
		return who.playerID, nil, nil, fmt.Errorf("resolve guild of player %d (cached guild %d): %w", who.playerID, guildID, err)
	}
	if fresh == nil {
		return who.playerID, nil, tipErr(constants.ErrNotInGuild, "not in any guild"), nil
	}
	if fresh.GuildID != guildID {
		logx.Infof("[GuildEconomy] player %d guild mapping healed from stale %d to %d", who.playerID, guildID, fresh.GuildID)
	}
	return who.playerID, fresh, nil, nil
}

// leftGuildWhileResolving:ResolvePlayerGuild 报错之后,以 MySQL 再复核一次"他此刻还在不在帮",
// 把"复核窗口里刚被踢 / 刚退帮"这种业务态变化从故障里分出来。返回非 nil = 按未入帮答复;nil = 调用方原错误照回。
// 调用方有三处,答复口径必须一致:经济前置 economyCaller、活动前置 activityPrelude、客户端 GetPlayerGuild
// (后者没有缓存里的帮会 id,cachedGuildID 传 0)。
//
// 为什么要在这里分:ResolvePlayerGuild 先用 uk_guild_member 复核映射(VerifyPlayerGuildID),再绕过缓存直读帮会快照
// (loadGuild:帮会行与成员行是两条独立的普通读,不在同一个一致性快照里)。玩家恰好在复核之后、成员行读到之前
// 被踢或退帮时,直读到的快照里没有他,它回的是与"两份 MySQL 数据互相矛盾"**同一个**不带哨兵的错误
// (guild_manage_repo.go 的 ResolvePlayerGuild 末尾),从错误本身分不出两者。原样抛成 gRPC 错误,
// 一次正常的踢人 / 退帮就会把当事人的客户端推进重连隔离(文件头纪律 4)。
// 同一窗口里帮会被解散时 ResolvePlayerGuild 回 (nil, nil),本来就走未入帮,不经过这里。
//
// 判据只认 uk_guild_member 唯一索引点查(一次,只发生在报错路径上;普通读,不加锁,不开事务):
//   - 此刻不在任何帮 → 回未入帮。即使原错误真是矛盾,"他此刻不在帮"也已是 MySQL 的真相 ——
//     矛盾的前提是 guild_member 说他在帮,这条前提已不成立,不会诱导他在损坏的数据上建出第二个帮;
//   - 此刻仍在某个帮 → 原错误照回(fail-closed)。真矛盾必须以故障暴露;"被踢后瞬间又入了别的帮"这种
//     极罕见的并发也落在这一支,代价只是客户端重连一次,换来的是真矛盾不会被吞成 tip;
//   - 复核本身失败 → 原错误照回,不把故障降级成业务答复。
//
// cachedGuildID 作 VerifyPlayerGuildID 的比对值:不一致(他已离帮)时它顺手失效映射键,下一次读映射直接拿到 0。
// 风险:MySQL 整体故障时,坏路径上的每个请求会多一次注定失败的点查;坏路径本身罕见,可以接受。
func (l *GuildLogic) leftGuildWhileResolving(ctx context.Context, playerID, cachedGuildID uint64, resolveErr error) *base.TipInfoMessage {
	actual, err := l.repo.VerifyPlayerGuildID(ctx, playerID, cachedGuildID)
	if err != nil {
		// 只记 Info:原错误会原样回给调用方并按故障计数,这里再记 Error 只是同一次故障的第二行。
		logx.Infof("[Guild] membership recheck of player %d failed after resolve error: %v (resolve: %v)", playerID, err, resolveErr)
		return nil
	}
	if actual != 0 {
		return nil
	}
	logx.Infof("[Guild] player %d is in no guild on recheck (cached guild %d), answering not-in-guild (resolve: %v)", playerID, cachedGuildID, resolveErr)
	return tipErr(constants.ErrNotInGuild, "not in any guild")
}

// memberOf 在快照里找某人。只用于展示与"省一次事务"的预判,授权一律在事务里复核。
func memberOf(g *data.GuildData, playerID uint64) (data.MemberData, bool) {
	if g == nil {
		return data.MemberData{}, false
	}
	for _, m := range g.Members {
		if m.PlayerID == playerID {
			return m, true
		}
	}
	return data.MemberData{}, false
}

// economyTip 把经济事务的错误翻成响应:先认经济哨兵,余者交给 mapWriteErr(§11.4 的统一映射,
// 含 ErrWriteConflict → kGuildBusyRetry 与"不是成员 / 帮会不存在"时的映射自愈)。
//
// 返回 (tip, result, err):tip 非 nil = 业务拒绝;err 非 nil = 故障;result 是指标 label。
// 入参 err 为 nil 时三者皆空。
func (l *GuildLogic) economyTip(ctx context.Context, playerID, guildID uint64, err error) (*base.TipInfoMessage, string, error) {
	switch {
	case err == nil:
		return nil, "", nil
	case errors.Is(err, data.ErrGuildLevelTooLow):
		// 捐献选项的 min_guild_level 与商品的 required_guild_level 共用一个码(05 §5.12)。
		return tipErr(constants.ErrShopLevelTooLow, "guild level too low"), resultLevel, nil
	case errors.Is(err, data.ErrDonateLimit):
		return tipErr(constants.ErrDonateLimit, "daily donate limit reached"), resultLimit, nil
	case errors.Is(err, data.ErrShopLimit):
		return tipErr(constants.ErrShopLimit, "shop purchase limit reached"), resultLimit, nil
	case errors.Is(err, data.ErrContributionInsufficient):
		return tipErr(constants.ErrContributionInsufficient, "contribution insufficient"), resultInsufficient, nil
	case errors.Is(err, data.ErrGuildMaxLevel):
		return tipErr(constants.ErrMaxLevel, "guild already at max level"), resultLevel, nil
	case errors.Is(err, data.ErrFundsInsufficient):
		return tipErr(constants.ErrFundsInsufficient, "guild funds insufficient"), resultInsufficient, nil
	case errors.Is(err, assetop.ErrTooManyPending):
		// 守卫拒绝不是故障:未决行数 / 跨度超限时再发新指令会破坏 scene 账本窗口的正确性证明。
		logx.Infof("[GuildEconomy] player %d has too many pending asset ops: %v", playerID, err)
		return tipErr(constants.ErrAssetPending, "too many pending asset ops"), resultPendingGuard, nil
	}
	tip, mapped := l.mapWriteErr(ctx, playerID, guildID, guildID, err)
	return tip, resultOfMapped(tip, mapped), mapped
}

// resultOfMapped 给 mapWriteErr 的结果定 label。经济 repo 只会回其中几种哨兵,
// 其余业务 tip 归 other_reject,保证 label 集合有界。
func resultOfMapped(tip *base.TipInfoMessage, err error) string {
	if err != nil {
		return resultError
	}
	switch tip.GetId() {
	case constants.ErrGuildNotFound, constants.ErrNotInGuild:
		return resultNotMember
	case constants.ErrRankTooLow:
		return resultRank
	case constants.ErrBusyRetry:
		return resultBusyRetry
	default:
		return resultOtherReject
	}
}

// resultOfFault 给 gRPC 错误定 label:把"没有会话""未接线"与真故障分开,排障时一眼能分清。
func resultOfFault(err error) string {
	switch status.Code(err) {
	case codes.PermissionDenied:
		return resultDenied
	case codes.Unavailable:
		return resultUnavailable
	default:
		return resultError
	}
}

// assetChannelDisabledTip:资产通道关闭(裁决 D)时捐献 / 兑换的答复。沿用 kGuildAssetPending 的
// "请稍后再试"文案,不另发码:对玩家来说这就是"暂时办不了",而开关属于运维窗口。
func assetChannelDisabledTip() *base.TipInfoMessage {
	return tipErr(constants.ErrAssetPending, "guild asset channel disabled")
}

// mintAssetOpID 取一个 op_id(同时作 correlation_id)。
//
// 号段取不到就整体失败,**绝不**用 0 或自造 id:op_id 是资产指令的主键,也是事后对账
// (scene 流水的 correlation_id)的唯一线索。失败沿用 kGuildIdGenUnavailable(Tip 表标了 fault)。
func (l *GuildLogic) mintAssetOpID(ctx context.Context, eco *EconomyDeps, playerID uint64) (uint64, *base.TipInfoMessage) {
	if eco.OpIDs == nil {
		logx.Errorf("[GuildEconomy] asset op id minter not wired (player=%d)", playerID)
		return 0, tipErr(constants.ErrIDGenUnavailable, "asset op id generator unavailable")
	}
	id, err := eco.OpIDs.Mint(ctx)
	if err != nil {
		logx.Errorf("[GuildEconomy] asset op id generator refused to mint (player=%d): %v", playerID, err)
		return 0, tipErr(constants.ErrIDGenUnavailable, "asset op id generator unavailable")
	}
	if id == 0 {
		logx.Errorf("[GuildEconomy] asset op id generator returned 0 (player=%d)", playerID)
		return 0, tipErr(constants.ErrIDGenUnavailable, "asset op id generator unavailable")
	}
	return id, nil
}

// newAssetOpToken 生成插行时的租约令牌:同步投递的 Reschedule 要带它做 CAS。
//
// 用 crypto/rand 而不是 math/rand:令牌是"这一行现在归我"的唯一凭据,可预测就可能与重投循环的
// 令牌撞上。最低位置 1 保证非 0 —— 0 在表里表示"没有租约"(与 assetop 的令牌同一约定)。
func newAssetOpToken() (uint64, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0, fmt.Errorf("read random lease token: %w", err)
	}
	return binary.LittleEndian.Uint64(buf[:]) | 1, nil
}

func durationMs(d time.Duration) uint64 { return uint64(d / time.Millisecond) }

// ── 同步投递 ──────────────────────────────────────────────────

// syncBudgetFor 算同步投递的预算。ok=false = 不够,跳过同步投递。
//
// 从 ctx 截止时间倒推(裁决 K):RequestBudget 拦截器已经按 Timeout−500 设了截止,
// 预留事务与发号花掉的时间自然扣在里面。ctx 没有截止(单测、内部调用)时按满额算。
func syncBudgetFor(ctx context.Context, syncBudget time.Duration) (time.Duration, bool) {
	remaining := syncBudget + syncTailReserve
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	return clampSyncBudget(syncBudget, remaining)
}

// clampSyncBudget:budget = min(syncBudget, remaining − syncTailReserve),低于 minSyncBudget 判为不够。
// 拆成纯函数只为可测(不依赖墙钟)。
func clampSyncBudget(syncBudget, remaining time.Duration) (time.Duration, bool) {
	budget := min(syncBudget, remaining-syncTailReserve)
	return budget, budget >= minSyncBudget
}

// deliverNow 在预留事务提交之后同步投一次,让玩家当场看到结果。
//
// 错误只打日志:结局以随后 OpState 的回读为准。投递失败、超时、scene 不在线都不是这次 RPC 的
// 失败 —— 行已经落库带着租约,租约到期后重投循环照常接手(05 §5.32 W1)。
func (l *GuildLogic) deliverNow(ctx context.Context, eco *EconomyDeps, op assetop.Op, kind string) {
	budget, ok := syncBudgetFor(ctx, eco.SyncBudget)
	if !ok {
		guildAssetSyncSkippedTotal.Inc(kind)
		logx.Infof("[GuildEconomy] sync delivery skipped, budget %v too small, left to reconcile loop op_id=%d kind=%s",
			budget, op.OpID, kind)
		return
	}
	deliverCtx, cancel := context.WithTimeout(withSyncDelivery(ctx), budget)
	defer cancel()
	if _, err := eco.Loop.ProcessOne(deliverCtx, op); err != nil {
		logx.Errorf("[GuildEconomy] sync delivery failed, outcome decided by readback op_id=%d kind=%s: %v",
			op.OpID, kind, err)
	}
}

// ── 视图 ──────────────────────────────────────────────────────

// orderViewOf 把库里的指令状态映射成客户端视图状态(显式 switch,两个枚举数值恰好相同也不许直接转型:
// 库枚举将来加值时,客户端枚举未必跟着加)。
//
// reason 的取法(05 §5.6):PENDING 取 last_reason(最近一次暂时原因,如战斗中 / 背包满);
// 终态取 reason_tip_id(只有 REJECTED 非 0)。未知状态回 UNSPECIFIED,由调用方记 ERROR。
//
// 库内的 ABANDONED(超 180 天自动放弃,落地恢复 §6.3)不是客户端契约:映射成已有的 ABORTED("超时或人工撤销"),
// 不新增客户端枚举、不新增 tip。它的 reason_tip_id 恒为 0,与 ABORTED 一致。
func orderViewOf(st pb.GuildAssetOpStatus, lastReason, reasonTipID uint32) (pb.GuildAssetOrderStatus, uint32) {
	switch st {
	case pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING:
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, lastReason
	case pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED:
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED, reasonTipID
	case pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED:
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_REJECTED, reasonTipID
	case pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED,
		pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABANDONED:
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_ABORTED, reasonTipID
	case pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL:
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED_PARTIAL, reasonTipID
	default:
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_UNSPECIFIED, 0
	}
}

// isTerminalOpStatus:进入"最近结果"的终态。ABANDONED 也在其中(展示为 ABORTED,见 orderViewOf):
// 漏了它,被放弃的那一行在"待结算"与"最近结果"里都看不到,玩家只会觉得东西凭空消失。
func isTerminalOpStatus(st pb.GuildAssetOpStatus) bool {
	switch st {
	case pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED,
		pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED,
		pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED,
		pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL,
		pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABANDONED:
		return true
	default:
		return false
	}
}

// resultOfOrder:写 RPC 成功路径按视图状态计指标。
func resultOfOrder(order pb.GuildAssetOrderStatus) string {
	switch order {
	case pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING:
		return resultPending
	case pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED:
		return resultApplied
	case pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_REJECTED:
		return resultRejected
	case pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_ABORTED:
		return resultAborted
	case pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED_PARTIAL:
		return resultAppliedPartial
	default:
		return resultError
	}
}

// donationRejectTip:捐献被 scene 永久拒绝时的 error_message(05 §5.4)。货币不足单独给一句,
// 其余拒绝原因统一"已撤销"。非 REJECTED 不填:PENDING 用视图表达,不能塞进 error_message。
func donationRejectTip(order pb.GuildAssetOrderStatus, reason uint32) *base.TipInfoMessage {
	if order != pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_REJECTED {
		return nil
	}
	if reason == assetop.ReasonCurrencyInsufficient {
		return tipErr(constants.ErrCurrencyInsufficient, "currency insufficient")
	}
	return tipErr(constants.ErrAssetRejected, "asset op rejected")
}

// settledView 在同步投递之后回读指令状态,给回包定视图。
//
// 回读失败或行不存在都按"结算中"展示:行已提交、带着租约,最坏也会被重投循环接手;
// 把一次回读失败报成 RPC 失败,玩家会以为没扣钱而再点一次(05 §5.32 W14)。
func (l *GuildLogic) settledView(ctx context.Context, eco *EconomyDeps, opID uint64) (pb.GuildAssetOrderStatus, uint32) {
	st, found, err := eco.Repo.OpState(ctx, opID)
	if err != nil {
		logx.Errorf("[GuildEconomy] read op state failed, showing pending op_id=%d: %v", opID, err)
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, 0
	}
	if !found {
		// 刚提交的行不该消失(清理任务只删 30 天前的终态行):出现就是 bug 或人工删库。
		logx.Errorf("[GuildEconomy] op row vanished right after commit, showing pending op_id=%d", opID)
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, 0
	}
	order, reason := orderViewOf(st.Status, st.LastReason, st.ReasonTipID)
	if order == pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_UNSPECIFIED {
		logx.Errorf("[GuildEconomy] op row has unknown status=%d op_id=%d", st.Status, opID)
	}
	return order, reason
}

// donationViewOf 把一行捐献指令装配成视图。货币种类与数额只在 payload 里(指令行不另存),
// 解不开时只缺这两个展示字段,不让整页失败 —— 状态与收益仍然是对的。
func donationViewOf(row data.AssetOpRow) *pb.GuildDonationView {
	order, reason := orderViewOf(row.Status, row.LastReason, row.ReasonTipID)
	view := &pb.GuildDonationView{
		OpId:             row.OpID,
		DonateId:         row.RefID,
		Status:           order,
		ContributionGain: row.ContributionDelta,
		FundsGain:        row.FundsDelta,
		ReasonTipId:      reason,
		CreatedMs:        row.CreatedMs,
	}
	if currency, ok := donationCurrencyOf(row.Payload); ok {
		view.CurrencyType = currency.GetCurrencyType()
		view.CostAmount = currency.GetAmount()
	} else {
		logx.Errorf("[GuildEconomy] donation payload undecodable op_id=%d", row.OpID)
	}
	return view
}

// donationCurrencyOf 从捐献 payload 里取那唯一一笔货币。形状不是"恰好一笔货币"一律视为解不开。
func donationCurrencyOf(payload []byte) (*assetpb.CurrencyAmount, bool) {
	if len(payload) == 0 {
		return nil, false
	}
	var bundle assetpb.AssetBundle
	if err := proto.Unmarshal(payload, &bundle); err != nil {
		return nil, false
	}
	if len(bundle.GetCurrencies()) != 1 {
		return nil, false
	}
	return bundle.GetCurrencies()[0], true
}

// shopUsedCount 取一件商品在 now 所在周期里已兑换的份数(含待发放)。
//
// 周期键与兑换预留用同一个函数算(gameday.PeriodKey,一次请求只取一次 now):展示与占用若各算各的,
// 跨 05:00 / 周一切点时会出现"页面说还能买 3 份、点下去说限购已满"。不限购(键 0,不占计数行)回 0;
// 非法周期同样回 0 —— 启动校验已拒,运行期出现只可能是配表被错误替换,展示宁可少报也不能读错周期的行。
func shopUsedCount(row *tablepb.GuildShopTable, usage map[data.ShopUsageKey]uint32, now time.Time) uint32 {
	periodKey, ok := gameday.PeriodKey(row.GetLimitPeriod(), now)
	if !ok || periodKey == 0 {
		return 0
	}
	return usage[data.ShopUsageKey{GoodsID: row.GetId(), PeriodKey: periodKey}]
}

// shopOrderViewOf 把一行兑换指令装配成视图:goods_id = ref_id,份数 = ref_count,总帮贡 = contribution_delta。
func shopOrderViewOf(row data.AssetOpRow) *pb.GuildShopOrderView {
	order, reason := orderViewOf(row.Status, row.LastReason, row.ReasonTipID)
	return &pb.GuildShopOrderView{
		OpId:             row.OpID,
		GoodsId:          row.RefID,
		Count:            row.RefCount,
		Status:           order,
		CostContribution: row.ContributionDelta,
		ReasonTipId:      reason,
		CreatedMs:        row.CreatedMs,
	}
}

// recentResults 从 RecentOps(按 (stream_epoch, seq) 倒序,新的在前)里挑出 10 分钟内进入终态、
// 且 keep 认可的行,至多 recentResultLimit 条。
//
// 终态行的 updated_ms 就是终结时刻,所以按它判"最近"。过滤放在 Go 里而不是 SQL:
// 扫描上限固定 20 行,与历史行数无关(05 §5.21)。
func recentResults(rows []data.AssetOpRow, atMs uint64, keep func(data.AssetOpRow) bool) []data.AssetOpRow {
	var cutoff uint64
	if atMs > recentResultWindowMs {
		cutoff = atMs - recentResultWindowMs
	}
	out := make([]data.AssetOpRow, 0, recentResultLimit)
	for _, row := range rows {
		if len(out) == recentResultLimit {
			break
		}
		if !isTerminalOpStatus(row.Status) || row.UpdatedMs < cutoff || !keep(row) {
			continue
		}
		out = append(out, row)
	}
	return out
}

// freshGuildInfo 在写提交之后重读帮会装配回包(APPLIED / 升级之后缓存已由 repo 失效)。
//
// 读失败或本人已不在帮只记日志、回 nil:写已经提交,回包里少一份 GuildInfo 只是让客户端自己再拉一次,
// 不能把一次成功的写报成失败。
func (l *GuildLogic) freshGuildInfo(ctx context.Context, guildID, playerID uint64) *pb.GuildInfo {
	g, err := l.repo.GetGuild(ctx, guildID)
	if err != nil {
		logx.Errorf("[GuildEconomy] reload guild %d for response (player %d): %v", guildID, playerID, err)
		return nil
	}
	if _, ok := memberOf(g, playerID); !ok {
		return nil
	}
	return l.guildInfoFor(ctx, g, playerID)
}

// exceptPlayer 从收件人里去掉操作者(B2 §14:操作者不推,他手上的回包就是最新状态)。
func exceptPlayer(ids []uint64, playerID uint64) []uint64 {
	out := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id != playerID {
			out = append(out, id)
		}
	}
	return out
}

// pendingViewLimit:待结算列表至多显示守卫允许的未决数(assetop.DefaultLimits.MaxPending = 16),
// 与守卫共用一个数,不另写一份。
func pendingViewLimit() int { return int(assetop.DefaultLimits.MaxPending) }

// ── 推送 ──────────────────────────────────────────────────────

// OnAssetFinalized 是 GuildAssetStore.OnFinalized 的实现:一条指令**本次**被终结之后推一条变更提示。
//
//   - 同步投递里终结的不推:调用方自己拿着回包(见 syncDeliveryKey)。
//   - 只推本人:捐献不广播全帮,避免 100 人帮会的推送风暴,其他成员打开界面时自己拉。
//   - DONATE → FUNDS_CHANGED(含拒绝 / 中止:客户端据此重拉捐献页,看到次数已退回);
//     SHOP / ACTIVITY_REWARD → DELIVERY_DONE。未知 kind 记 ERROR 不推 —— 推一个猜出来的类型,
//     客户端会去拉错的页面。
//
// 推送至多一次、失败不影响终结(push.go 纪律);它跑在 Store 提交之后,不能 panic。
//
// 摘要 bump(landing-recovery-sync-digest.md §3.3 表第 8 行)排在同步投递判断**之前**:同步投递的回包同样可能在
// 断线时丢失,摘要必须覆盖这条路径。GuildAssetStore.terminate 在事务提交、缓存失效之后才调 OnFinalized,
// 满足"先提交、先失效、再 bump"(§3.5 第 1、2 条),所以不必在 asset_store.go 里另开一个挂点。
// 只 bump 本人;域按 kind 选(assetFinalizedSyncDomains)。
func (l *GuildLogic) OnAssetFinalized(ctx context.Context, op data.FinalizedOp) {
	l.bumpSync([]uint64{op.PlayerID}, assetFinalizedSyncDomains(op.Kind)...)
	if isSyncDelivery(ctx) {
		return
	}
	var kind pb.GuildChangeKind
	switch op.Kind {
	case pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE:
		kind = pb.GuildChangeKind_GUILD_CHANGE_KIND_FUNDS_CHANGED
	case pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD:
		kind = pb.GuildChangeKind_GUILD_CHANGE_KIND_DELIVERY_DONE
	default:
		logx.Errorf("[GuildEconomy] finalized op of unknown kind=%d op_id=%d, no push", op.Kind, op.OpID)
		return
	}
	l.notify(kind, op.GuildID, 0, op.PlayerID, []uint64{op.PlayerID})
}

// ── 客户端幂等键(05 顶部「客户端幂等键」覆盖块)──────────────────
//
// DonateToGuild / BuyGuildShopGoods 的 client_request_id:同一玩家的同一个非 0 键至多写成一笔指令;
// 重发(超时 / 信封 1003 / 断线重连后带同一个键)回首次那笔指令的**当前**视图,不再发号、不再扣。0 = 旧客户端,不去重。
// 去重记录(guild_client_request)与指令行、计数、扣帮贡在同一个预留事务里提交,寿命见 guild_db.proto 的 GuildClientRequestRecord。
//
// 三道检查,少一道都会在某个窗口里重复执行或答非所问:
//  1. 前置之前先查(replay*):首次已提交就直接按它答复 —— 哪怕玩家此后已离帮、帮会已解散、资产通道已关、正在合服,
//     那笔指令也确实发生了;回"未入帮"会让客户端以为没扣。
//  2. 预留事务在锁住本人成员行之后复核(data.ErrClientRequestReplayed):首次与重发并发在途时,成员行把两者排成一列,
//     后到者在锁内读到先到者已提交的记录,整体回滚(白烧一个 op_id,号段只进不退,可以接受)。
//  3. 本次没写成指令(回了业务拒绝,或第 2 道撞上了首次)就再查一次:首次恰在第 1 道之后提交时,重发会被预判
//     (帮贡不足、次数已满……)挡在事务之外,以首次结果为准,而不是回一个误导的拒绝。

// errClientRequestRaced:预留事务在锁内撞上了首次请求(第 2 道)。只在本文件内流转,由 withClientRequest 换成首次结果。
var errClientRequestRaced = errors.New("guild client request applied by a concurrent attempt")

// withClientRequest 是捐献与兑换共用的幂等包装。key 为 0 时原样执行 once。
//   - replay:按 (会话玩家, key) 找首次结果;ok=false = 没有可用的去重记录(含拿不到会话身份、查询失败 —— 交给 once,
//     事务里的第 2 道兜底,不会因此重复执行)。
//   - once:不带去重的原流程;撞上首次请求时回 errClientRequestRaced。
//   - wrote:once 的回包是否写成了指令(带订单视图)。故障(gRPC 错误)不再查:客户端会带同一个键重发,第 1 道接住。
func withClientRequest[R any](key uint64, replay func() (R, string, bool), once func() (R, string, error), wrote func(R) bool) (R, string, error) {
	if key == 0 {
		return once()
	}
	if resp, result, ok := replay(); ok {
		return resp, result, nil
	}
	resp, result, err := once()
	raced := errors.Is(err, errClientRequestRaced)
	if !raced && (err != nil || wrote(resp)) {
		return resp, result, err
	}
	if replayed, replayedResult, ok := replay(); ok {
		return replayed, replayedResult, nil
	}
	if raced {
		// 锁内读到了去重记录,事后却查不到(查询失败):既给不出首次结果,也绝不能重新执行。按故障回,客户端带同一个键重发即可。
		var zero R
		return zero, resultError, status.Error(codes.Unavailable, "guild client request applied, replay unavailable")
	}
	return resp, result, err
}

// clientRequestHit 查 (会话玩家, key) 的去重记录。ok=false:没有记录,或拿不到会话身份(交给原流程按无会话拒绝),或查询失败。
// 查询失败只记日志、不当故障:预留事务在成员行锁之下还会复核(第 2 道),放行不会重复执行。
func (l *GuildLogic) clientRequestHit(ctx context.Context, eco *EconomyDeps, key uint64) (uint64, data.ClientRequestHit, bool) {
	who := callerOf(ctx, 0)
	if !who.fromClient || who.playerID == 0 {
		return 0, data.ClientRequestHit{}, false
	}
	hit, found, err := eco.Repo.ClientRequest(ctx, who.playerID, key)
	if err != nil {
		logx.Errorf("[GuildEconomy] read client request failed, leaving it to the transaction recheck (player=%d client_request_id=%d): %v",
			who.playerID, key, err)
		return who.playerID, data.ClientRequestHit{}, false
	}
	return who.playerID, hit, found
}

// clientRequestMismatch:同一个键却换了请求(另一个方法、另一个选项 / 商品 / 份数),或去重记录指向的指令行已不在。
// 两者都回 kInvalidParameter(common 段 1005,业务拒绝)、不写入:
//   - 换了参数是客户端复用了键(缺陷)。按首次结果答复,玩家会以为第二个请求成功了;重新执行则违背"一个键至多一笔"。
//   - 指令行已不在(人工删库,或清理让去重记录晚了一轮,见 asset_store.go 的 CleanupOnce):给不出首次结果,也绝不能重新执行。
//
// refCount:捐献恒 1;兑换是归一后的份数(shopBuyCount)。
func clientRequestMismatch(hit data.ClientRequestHit, kind pb.GuildAssetOpKind, refID, refCount uint32, expectedGuildID uint64) *base.TipInfoMessage {
	if !hit.OpFound {
		logx.Errorf("[GuildEconomy] client request record points at a missing op, refusing without re-executing (op_id=%d)", hit.OpID)
		return tipErr(uint32(tablepb.CommonError_kInvalidParameter), "client request record outlived its op")
	}
	if hit.Op.Kind != kind || hit.Op.RefID != refID || hit.Op.RefCount != refCount || (expectedGuildID != 0 && hit.Op.GuildID != expectedGuildID) {
		logx.Errorf("[GuildEconomy] client_request_id reused with different parameters, refusing (player=%d first op_id=%d kind=%d ref=%d count=%d; now kind=%d ref=%d count=%d)",
			hit.Op.PlayerID, hit.OpID, hit.Op.Kind, hit.Op.RefID, hit.Op.RefCount, kind, refID, refCount)
		return tipErr(uint32(tablepb.CommonError_kInvalidParameter), "client_request_id reused with different parameters")
	}
	return nil
}

// ── 捐献(05 §5.25)────────────────────────────────────────────

// DonateToGuild:发起一笔捐献。流程:幂等键查首次结果 → 前置 → 配表与缓存预判 → 发号 → 预留事务(建 PENDING 行、占今日次数、
// 写去重记录)→ 同步投递一次 → 回读状态装配视图(幂等包装见 withClientRequest)。
func (l *GuildLogic) DonateToGuild(ctx context.Context, req *pb.DonateToGuildRequest) (*pb.DonateToGuildResponse, error) {
	resp, result, err := l.donateToGuild(ctx, req)
	guildEconomyRequestsTotal.Inc(economyRPCDonate, result)
	return resp, err
}

func (l *GuildLogic) donateToGuild(ctx context.Context, req *pb.DonateToGuildRequest) (*pb.DonateToGuildResponse, string, error) {
	eco := l.economy
	if eco == nil {
		return nil, resultUnavailable, errEconomyNotEnabled()
	}
	return withClientRequest(req.GetClientRequestId(),
		func() (*pb.DonateToGuildResponse, string, bool) { return l.replayDonation(ctx, eco, req) },
		func() (*pb.DonateToGuildResponse, string, error) { return l.donateOnce(ctx, eco, req) },
		func(resp *pb.DonateToGuildResponse) bool { return resp.GetDonation() != nil })
}

// replayDonation:按去重记录找回首次那笔捐献,装配成与首次同形的回包。视图取指令行的**当前**状态
// (首次回包是 PENDING、此后已 APPLIED,重发看到的就是 APPLIED);数额取行上的值(与首次回包同源:预留时按同一配表行写入)。
func (l *GuildLogic) replayDonation(ctx context.Context, eco *EconomyDeps, req *pb.DonateToGuildRequest) (*pb.DonateToGuildResponse, string, bool) {
	playerID, hit, ok := l.clientRequestHit(ctx, eco, req.GetClientRequestId())
	if !ok {
		return nil, "", false
	}
	if tip := clientRequestMismatch(hit, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE, req.GetDonateId(), 1, req.GetExpectedGuildId()); tip != nil {
		return &pb.DonateToGuildResponse{ErrorMessage: tip}, resultRequestConflict, true
	}
	view := donationViewOf(hit.Op)
	return &pb.DonateToGuildResponse{
		ErrorMessage: donationRejectTip(view.GetStatus(), view.GetReasonTipId()),
		Donation:     view,
		// 帮会取指令绑定的那一个;本人已不在该帮时不填(与首次回包"请求者仍在帮时必填"同一口径)。
		Guild: l.freshGuildInfo(ctx, hit.Op.GuildID, playerID),
	}, resultReplayed, true
}

// donateOnce 是不带去重的捐献流程(加幂等键之前的 donateToGuild 本体)。
func (l *GuildLogic) donateOnce(ctx context.Context, eco *EconomyDeps, req *pb.DonateToGuildRequest) (*pb.DonateToGuildResponse, string, error) {
	// 一次请求只取一次 now:周期键、截止时刻、租约用同一个值,跨切点的请求才不会把
	// "占用次数"与"截止时刻"算进两个不同的游戏日(gameday 包约定)。
	start := eco.Now()
	playerID, g, tip, err := l.economyCaller(ctx)
	if err != nil {
		return nil, resultOfFault(err), err
	}
	if tip != nil {
		return &pb.DonateToGuildResponse{ErrorMessage: tip}, resultNotMember, nil
	}
	// 这里只做纯预检；提交事务锁的仍是这个确定的g.GuildID及其成员行，换帮不会重选目标。
	if req.GetExpectedGuildId() != 0 && req.GetExpectedGuildId() != g.GuildID {
		return &pb.DonateToGuildResponse{ErrorMessage: tipErr(constants.ErrNotInGuild, "guild changed")}, resultNotMember, nil
	}
	if eco.Loop == nil {
		return &pb.DonateToGuildResponse{ErrorMessage: assetChannelDisabledTip()}, resultDisabled, nil
	}

	row, ok := table.GuildDonateTableManagerInstance.FindById(req.GetDonateId())
	if !ok {
		// 客户端的选项来自 GetGuildDonateOptions,查不到只可能是热更删了行或请求被篡改 —— 按拒绝答复。
		return &pb.DonateToGuildResponse{ErrorMessage: tipErr(constants.ErrAssetRejected, "donate option not found")}, resultNotFound, nil
	}
	// 缓存预判只为省一次发号与事务;事务内按 MySQL 的 level 复核。
	if g.Level < row.GetMinGuildLevel() {
		return &pb.DonateToGuildResponse{ErrorMessage: tipErr(constants.ErrShopLevelTooLow, "guild level too low")}, resultLevel, nil
	}
	deadlineAfterMs, ok := assetOpDeadlineMs()
	if !ok {
		return nil, resultError, status.Errorf(codes.Internal, "GuildRule row %d missing", guildRuleRowID)
	}
	bundle := &assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{
		CurrencyType: row.GetCurrencyType(),
		Amount:       row.GetCostAmount(),
	}}}
	payload, err := proto.Marshal(bundle)
	if err != nil {
		return nil, resultError, fmt.Errorf("marshal donate bundle: %w", err)
	}
	// 令牌先于发号:令牌生成失败时不白烧一个 op_id(号段只进不退)。
	token, err := newAssetOpToken()
	if err != nil {
		return nil, resultError, err
	}
	opID, tip := l.mintAssetOpID(ctx, eco, playerID)
	if tip != nil {
		return &pb.DonateToGuildResponse{ErrorMessage: tip}, resultIDUnavailable, nil
	}

	startMs := uint64(start.UnixMilli())
	deadlineMs := startMs + deadlineAfterMs
	reserved, err := eco.Repo.ReserveDonation(ctx, data.DonationReserve{
		OpID:             opID,
		PlayerID:         playerID,
		GuildID:          g.GuildID,
		DonateID:         row.GetId(),
		MinGuildLevel:    row.GetMinGuildLevel(),
		ContributionGain: row.GetContributionGain(),
		FundsGain:        row.GetFundsGain(),
		DailyLimit:       row.GetDailyLimit(),
		PeriodKey:        gameday.DayKey(start),
		DeadlineMs:       deadlineMs,
		LeaseUntilMs:     startMs + durationMs(eco.Lease),
		LeaseToken:       token,
		NowMs:            startMs,
		Payload:          payload,
		ClientRequestID:  req.GetClientRequestId(),
	})
	if errors.Is(err, data.ErrClientRequestReplayed) {
		return nil, resultReplayed, errClientRequestRaced
	}
	if err != nil {
		rejectTip, result, fault := l.economyTip(ctx, playerID, g.GuildID, err)
		if fault != nil {
			return nil, result, fault
		}
		return &pb.DonateToGuildResponse{ErrorMessage: rejectTip}, result, nil
	}
	// 预留已提交:捐献页的待结算列表与今日次数变了。紧接着的同步投递若当场终结,OnAssetFinalized 会再 bump 一次,
	// 两次都只是"去拉一次"的提示;这一次不能省 —— scene 不可达时行停在 PENDING,终结可能在玩家下线之后才发生。
	// 不带 GUILD:预留只占次数、建 PENDING 行,资金与帮贡要等 APPLIED 终结才入账(那时 OnAssetFinalized 带 GUILD)。
	l.bumpSync([]uint64{playerID}, scenepb.SyncDomain_SYNC_DOMAIN_GUILD_ECONOMY)

	l.deliverNow(ctx, eco, assetop.Op{
		OpID:          opID,
		PlayerID:      playerID,
		Stream:        assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT,
		Seq:           reserved.Seq,
		StreamEpoch:   reserved.StreamEpoch,
		CorrelationID: opID,
		TxType:        uint32(rollbackpb.TransactionType_TX_GUILD_DONATE),
		Bundle:        bundle,
		DeadlineMs:    deadlineMs,
		LeaseToken:    token,
	}, assetKindDonate)

	order, reason := l.settledView(ctx, eco, opID)
	return &pb.DonateToGuildResponse{
		ErrorMessage: donationRejectTip(order, reason),
		Donation: &pb.GuildDonationView{
			OpId:             opID,
			DonateId:         row.GetId(),
			Status:           order,
			CurrencyType:     row.GetCurrencyType(),
			CostAmount:       row.GetCostAmount(),
			ContributionGain: row.GetContributionGain(),
			FundsGain:        row.GetFundsGain(),
			ReasonTipId:      reason,
			CreatedMs:        startMs,
		},
		// APPLIED 时资金与帮贡已入账、缓存已失效,重读才看得到新数字。
		Guild: l.freshGuildInfo(ctx, g.GuildID, playerID),
	}, resultOfOrder(order), nil
}

// ── 升级(05 §5.26)────────────────────────────────────────────

// UpgradeGuild:帮主或长老花帮会资金升一级。只动 guild 库,不走资产通道(资产通道关闭时照常)。
func (l *GuildLogic) UpgradeGuild(ctx context.Context, req *pb.UpgradeGuildRequest) (*pb.UpgradeGuildResponse, error) {
	resp, result, err := l.upgradeGuild(ctx, req)
	guildEconomyRequestsTotal.Inc(economyRPCUpgrade, result)
	return resp, err
}

func (l *GuildLogic) upgradeGuild(ctx context.Context, req *pb.UpgradeGuildRequest) (*pb.UpgradeGuildResponse, string, error) {
	eco := l.economy
	if eco == nil {
		return nil, resultUnavailable, errEconomyNotEnabled()
	}
	playerID, g, tip, err := l.economyCaller(ctx)
	if err != nil {
		return nil, resultOfFault(err), err
	}
	if tip != nil {
		return &pb.UpgradeGuildResponse{ErrorMessage: tip}, resultNotMember, nil
	}

	// 职位、闸门、expected_level、花费全部在事务内按锁住的帮会行判定:缓存里的 role / level / funds
	// 都可能说谎一个 TTL,而升级扣的是全帮的钱。
	res, err := eco.Repo.UpgradeGuild(ctx, g.GuildID, playerID, req.GetExpectedLevel(), upgradeLevelLookup)
	if err != nil {
		rejectTip, result, fault := l.economyTip(ctx, playerID, g.GuildID, err)
		if fault != nil {
			return nil, result, fault
		}
		resp := &pb.UpgradeGuildResponse{ErrorMessage: rejectTip}
		// 业务失败也带最新 GuildInfo:资金不足时客户端顺带刷新资金显示。
		// 只有"不在帮 / 帮会不存在"不带 —— 那时已经没有他能看的帮会了。
		if result != resultNotMember {
			resp.Guild = l.freshGuildInfo(ctx, g.GuildID, playerID)
		}
		return resp, result, nil
	}

	result := resultUnchanged
	if res.Changed {
		result = resultOK
		l.notify(pb.GuildChangeKind_GUILD_CHANGE_KIND_LEVEL_UP, g.GuildID, playerID, 0, exceptPlayer(res.MemberIDs, playerID))
		// 等级、成员上限、资金对全帮可见:MemberIDs(含操作者本人)全体 bump GUILD。
		// 同一次扇出带上 GUILD_ECONOMY 与 GUILD_ACTIVITY:捐献档位的 unlocked(min_guild_level)、商品的 unlocked
		// (required_guild_level)、活动行的等级门槛(blocked_tip_id)都随帮会等级变,全帮成员这两个域的拉取结果同样变了
		// (§3.5 第 3 条)。多两个域号只是同一条 Lua 多两个 ARGV,不多一次 Redis 往返,不属于 §3.3"帮会级数值
		// 不逐次扇出"的豁免。Changed=false 时库里没变,不 bump。
		l.bumpSync(res.MemberIDs,
			scenepb.SyncDomain_SYNC_DOMAIN_GUILD,
			scenepb.SyncDomain_SYNC_DOMAIN_GUILD_ECONOMY,
			scenepb.SyncDomain_SYNC_DOMAIN_GUILD_ACTIVITY)
	}
	// Changed=false(expected_level 与库里不符 = 已被别人升过)同样回成功 + 最新 GuildInfo,不再扣资金。
	return &pb.UpgradeGuildResponse{Guild: l.freshGuildInfo(ctx, g.GuildID, playerID)}, result, nil
}

// ── 兑换(05 §5.27)────────────────────────────────────────────

// BuyGuildShopGoods:花可用帮贡兑换商品。流程与捐献同形,区别:GUILD_CREDIT 流、永不中止(deadline 0)、
// 事务内先扣帮贡再投递;scene 永久拒绝时由 Store.Finalize 退帮贡与限购。幂等键同捐献(withClientRequest)。
func (l *GuildLogic) BuyGuildShopGoods(ctx context.Context, req *pb.BuyGuildShopGoodsRequest) (*pb.BuyGuildShopGoodsResponse, error) {
	resp, result, err := l.buyGuildShopGoods(ctx, req)
	guildEconomyRequestsTotal.Inc(economyRPCBuy, result)
	return resp, err
}

func (l *GuildLogic) buyGuildShopGoods(ctx context.Context, req *pb.BuyGuildShopGoodsRequest) (*pb.BuyGuildShopGoodsResponse, string, error) {
	eco := l.economy
	if eco == nil {
		return nil, resultUnavailable, errEconomyNotEnabled()
	}
	return withClientRequest(req.GetClientRequestId(),
		func() (*pb.BuyGuildShopGoodsResponse, string, bool) { return l.replayShopOrder(ctx, eco, req) },
		func() (*pb.BuyGuildShopGoodsResponse, string, error) { return l.buyOnce(ctx, eco, req) },
		func(resp *pb.BuyGuildShopGoodsResponse) bool { return resp.GetOrder() != nil })
}

// shopBuyCount:请求的份数。count=0 是 proto 缺省值(老客户端不填份数),按 1 份处理;幂等键比对参数时用的也是归一后的值。
func shopBuyCount(req *pb.BuyGuildShopGoodsRequest) uint32 {
	if count := req.GetCount(); count != 0 {
		return count
	}
	return 1
}

// replayShopOrder:按去重记录找回首次那笔兑换(视图取指令行的当前状态,同 replayDonation)。
// 余额一律直读 MySQL(首次回包里的"预留后余额"此刻可能已因别的兑换、退款而变):找不到成员行(已离帮)回 0,
// 读失败也回 0 并记 ERROR —— 纯展示,客户端收到订单视图后会自己重拉商店页;不能因为一个展示字段把已发生的订单报成失败。
func (l *GuildLogic) replayShopOrder(ctx context.Context, eco *EconomyDeps, req *pb.BuyGuildShopGoodsRequest) (*pb.BuyGuildShopGoodsResponse, string, bool) {
	playerID, hit, ok := l.clientRequestHit(ctx, eco, req.GetClientRequestId())
	if !ok {
		return nil, "", false
	}
	if tip := clientRequestMismatch(hit, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP, req.GetGoodsId(), shopBuyCount(req), req.GetExpectedGuildId()); tip != nil {
		return &pb.BuyGuildShopGoodsResponse{ErrorMessage: tip}, resultRequestConflict, true
	}
	order := shopOrderViewOf(hit.Op)
	resp := &pb.BuyGuildShopGoodsResponse{
		Order:               order,
		ContributionBalance: l.refundedBalance(ctx, eco, hit.Op.GuildID, playerID, 0),
	}
	if order.GetStatus() == pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_REJECTED {
		// 同首次回包:兑换被拒一律"已撤销"。
		resp.ErrorMessage = tipErr(constants.ErrAssetRejected, "asset op rejected")
	}
	return resp, resultReplayed, true
}

// buyOnce 是不带去重的兑换流程(加幂等键之前的 buyGuildShopGoods 本体)。
func (l *GuildLogic) buyOnce(ctx context.Context, eco *EconomyDeps, req *pb.BuyGuildShopGoodsRequest) (*pb.BuyGuildShopGoodsResponse, string, error) {
	start := eco.Now()
	playerID, g, tip, err := l.economyCaller(ctx)
	if err != nil {
		return nil, resultOfFault(err), err
	}
	if tip != nil {
		return &pb.BuyGuildShopGoodsResponse{ErrorMessage: tip}, resultNotMember, nil
	}
	if req.GetExpectedGuildId() != 0 && req.GetExpectedGuildId() != g.GuildID {
		return &pb.BuyGuildShopGoodsResponse{ErrorMessage: tipErr(constants.ErrNotInGuild, "guild changed")}, resultNotMember, nil
	}
	if eco.Loop == nil {
		return &pb.BuyGuildShopGoodsResponse{ErrorMessage: assetChannelDisabledTip()}, resultDisabled, nil
	}

	row, ok := table.GuildShopTableManagerInstance.FindById(req.GetGoodsId())
	if !ok {
		return &pb.BuyGuildShopGoodsResponse{ErrorMessage: tipErr(constants.ErrShopGoodsNotFound, "shop goods not found")}, resultNotFound, nil
	}
	count := shopBuyCount(req)
	maxStack, ok := itemMaxStack(row.GetItemId())
	if !ok {
		// 启动校验保证商品引用的物品存在,运行期查不到 = 配表被错误替换,fail-closed。
		return nil, resultError, status.Errorf(codes.Internal, "Item row %d of GuildShop[%d] missing", row.GetItemId(), row.GetId())
	}
	if count > MaxBuyCount(row, maxStack) {
		return &pb.BuyGuildShopGoodsResponse{ErrorMessage: tipErr(constants.ErrShopLimit, "count exceeds max buy count")}, resultLimit, nil
	}
	// 以下两条是预判,只为省一次发号与事务;事务内按 MySQL 锁行复核。
	if g.Level < row.GetRequiredGuildLevel() {
		return &pb.BuyGuildShopGoodsResponse{ErrorMessage: tipErr(constants.ErrShopLevelTooLow, "guild level too low")}, resultLevel, nil
	}
	// cost_contribution ≤ 1e9 且 count ≤ MaxShopBuyCount(启动校验),乘积不会溢出。
	cost := row.GetCostContribution() * uint64(count)
	if tip, result := l.contributionPrecheck(ctx, eco, g, playerID, cost); tip != nil {
		return &pb.BuyGuildShopGoodsResponse{ErrorMessage: tip}, result, nil
	}
	periodKey, ok := gameday.PeriodKey(row.GetLimitPeriod(), start)
	if !ok {
		return nil, resultError, status.Errorf(codes.Internal, "GuildShop[%d].limit_period=%d invalid", row.GetId(), row.GetLimitPeriod())
	}
	// item_count × count ≤ max_stack_size(MaxBuyCount 的定义),装得进 uint32。
	bundle := &assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{
		ConfigId: row.GetItemId(),
		Count:    row.GetItemCount() * count,
	}}}
	payload, err := proto.Marshal(bundle)
	if err != nil {
		return nil, resultError, fmt.Errorf("marshal shop bundle: %w", err)
	}
	token, err := newAssetOpToken()
	if err != nil {
		return nil, resultError, err
	}
	opID, tip := l.mintAssetOpID(ctx, eco, playerID)
	if tip != nil {
		return &pb.BuyGuildShopGoodsResponse{ErrorMessage: tip}, resultIDUnavailable, nil
	}

	startMs := uint64(start.UnixMilli())
	reserved, err := eco.Repo.ReserveShopOrder(ctx, data.ShopReserve{
		OpID:               opID,
		PlayerID:           playerID,
		GuildID:            g.GuildID,
		GoodsID:            row.GetId(),
		Count:              count,
		RequiredGuildLevel: row.GetRequiredGuildLevel(),
		Cost:               cost,
		LimitCount:         row.GetLimitCount(),
		PeriodKey:          periodKey,
		LeaseUntilMs:       startMs + durationMs(eco.Lease),
		LeaseToken:         token,
		NowMs:              startMs,
		Payload:            payload,
		ClientRequestID:    req.GetClientRequestId(),
	})
	if errors.Is(err, data.ErrClientRequestReplayed) {
		return nil, resultReplayed, errClientRequestRaced
	}
	if err != nil {
		rejectTip, result, fault := l.economyTip(ctx, playerID, g.GuildID, err)
		if fault != nil {
			return nil, result, fault
		}
		return &pb.BuyGuildShopGoodsResponse{ErrorMessage: rejectTip}, result, nil
	}
	// 预留已提交(帮贡已扣、限购已占、PENDING 行已建):商店页变了。理由同捐献。
	// 另带 GUILD:帮贡余额在预留事务里已扣,本人 GuildInfo.members[] 的 contribution_balance 变了。
	l.bumpSync([]uint64{playerID}, scenepb.SyncDomain_SYNC_DOMAIN_GUILD_ECONOMY, scenepb.SyncDomain_SYNC_DOMAIN_GUILD)

	l.deliverNow(ctx, eco, assetop.Op{
		OpID:          opID,
		PlayerID:      playerID,
		Stream:        assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT,
		Seq:           reserved.Seq,
		StreamEpoch:   reserved.StreamEpoch,
		CorrelationID: opID,
		TxType:        uint32(rollbackpb.TransactionType_TX_GUILD_SHOP),
		Bundle:        bundle,
		// 兑换永不中止(05 R7):物品属于玩家,背包满就等腾出空间后自动到账。
		DeadlineMs: 0,
		LeaseToken: token,
	}, assetKindShop)

	order, reason := l.settledView(ctx, eco, opID)
	resp := &pb.BuyGuildShopGoodsResponse{
		Order: &pb.GuildShopOrderView{
			OpId:             opID,
			GoodsId:          row.GetId(),
			Count:            count,
			Status:           order,
			CostContribution: cost,
			ReasonTipId:      reason,
			CreatedMs:        startMs,
		},
		ContributionBalance: reserved.BalanceAfter,
	}
	switch order {
	case pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_REJECTED:
		// 兑换被拒一律"已撤销"(05 §5.27 第 9 步):scene 的拒绝原因对兑换没有可操作的区分。
		resp.ErrorMessage = tipErr(constants.ErrAssetRejected, "asset op rejected")
		resp.ContributionBalance = l.refundedBalance(ctx, eco, g.GuildID, playerID, reserved.BalanceAfter)
	case pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_ABORTED:
		// 兑换不设截止,同步路径上理论上到不了 ABORTED;真出现也同样已退帮贡,余额照样直读。
		resp.ContributionBalance = l.refundedBalance(ctx, eco, g.GuildID, playerID, reserved.BalanceAfter)
	}
	return resp, resultOfOrder(order), nil
}

// contributionPrecheck 是兑换前的帮贡预判:只为省一次发号与事务,授权仍在事务里按锁住的成员行复核。
// 返回非 nil tip = 拒绝(result 是指标 label);nil = 放行进事务。
//
// 快照说够就放行;快照说不够时先直读 MySQL 确认再拒绝。只按快照拒绝有一条无法自愈的误判:
// 快照里的帮贡靠结算后的缓存失效刷新,失效失败(Redis 抖动超过后台重试窗口)时它会比真实余额**低**一个 TTL,
// 而商店页展示的是 MySQL 直读的余额(05 §5.21)—— 玩家看到"页面显示够、点下去说不够",整整 30 分钟都这样。
// 快照偏高的一侧不必管:事务按锁内余额判,照样回帮贡不足。
// 直读失败既不拒绝也不报故障:交给事务按锁内余额裁决(它本来就是权威判据),代价只是多发一个号。
func (l *GuildLogic) contributionPrecheck(ctx context.Context, eco *EconomyDeps, g *data.GuildData, playerID, cost uint64) (*base.TipInfoMessage, string) {
	if me, _ := memberOf(g, playerID); me.ContributionBalance >= cost {
		return nil, ""
	}
	_, balance, found, err := eco.Repo.MemberContribution(ctx, g.GuildID, playerID)
	switch {
	case err != nil:
		logx.Errorf("[GuildEconomy] confirm contribution before buying failed, leaving it to the transaction (guild %d, player %d): %v",
			g.GuildID, playerID, err)
		return nil, ""
	case !found:
		// 快照说他在帮、MySQL 说不在:以 MySQL 为准,顺手纠正映射(同 mapWriteErr 的 ErrNotGuildMember 一支)。
		l.verifyMapping(ctx, playerID, g.GuildID)
		return tipErr(constants.ErrNotInGuild, "not a member of the guild"), resultNotMember
	case balance < cost:
		return tipErr(constants.ErrContributionInsufficient, "contribution insufficient"), resultInsufficient
	default:
		return nil, ""
	}
}

// refundedBalance:兑换被拒 / 中止后 Finalize 已退回帮贡,余额改为直读 MySQL;幂等重发(replayShopOrder)同样用它直读,fallback 传 0。
// 读失败只记日志、回 fallback(兑换路径是预留时的余额,偏低,纯展示):客户端收到终态后会自己再拉一次。
// 成员行已不在(兑换期间离帮)回 0 —— 离帮本来就清空帮贡。
func (l *GuildLogic) refundedBalance(ctx context.Context, eco *EconomyDeps, guildID, playerID, fallback uint64) uint64 {
	_, balance, found, err := eco.Repo.MemberContribution(ctx, guildID, playerID)
	if err != nil {
		logx.Errorf("[GuildEconomy] read contribution after refund (guild %d, player %d): %v", guildID, playerID, err)
		return fallback
	}
	if !found {
		return 0
	}
	return balance
}

// ── 两个读 RPC(05 §5.28)────────────────────────

// GetGuildDonateOptions:捐献页。客户端不加载配表,选项与今日用量只能由服务端下发(契约偏差 2)。
func (l *GuildLogic) GetGuildDonateOptions(ctx context.Context, req *pb.GetGuildDonateOptionsRequest) (*pb.GetGuildDonateOptionsResponse, error) {
	resp, result, err := l.getGuildDonateOptions(ctx, req)
	guildEconomyRequestsTotal.Inc(economyRPCDonateOptions, result)
	return resp, err
}

func (l *GuildLogic) getGuildDonateOptions(ctx context.Context, _ *pb.GetGuildDonateOptionsRequest) (*pb.GetGuildDonateOptionsResponse, string, error) {
	eco := l.economy
	if eco == nil {
		return nil, resultUnavailable, errEconomyNotEnabled()
	}
	now := eco.Now()
	readMs := uint64(now.UnixMilli())
	playerID, g, tip, err := l.economyCaller(ctx)
	if err != nil {
		return nil, resultOfFault(err), err
	}
	if tip != nil {
		return &pb.GetGuildDonateOptionsResponse{ErrorMessage: tip}, resultNotMember, nil
	}

	// 帮贡直读 MySQL、不走缓存(05 §5.21):结算刚把帮贡加上时,缓存失效失败会让玩家看到旧数。
	// 先读它还有一个作用 —— 成员行不在就说明缓存快照过期,后面几次读都不必做了。
	total, balance, found, err := eco.Repo.MemberContribution(ctx, g.GuildID, playerID)
	if err != nil {
		return nil, resultError, fmt.Errorf("read contribution of player %d: %w", playerID, err)
	}
	if !found {
		l.verifyMapping(ctx, playerID, g.GuildID)
		return &pb.GetGuildDonateOptionsResponse{ErrorMessage: tipErr(constants.ErrNotInGuild, "not a member of the guild")}, resultNotMember, nil
	}
	usage, err := eco.Repo.DonateUsage(ctx, playerID, gameday.DayKey(now))
	if err != nil {
		return nil, resultError, fmt.Errorf("read donate usage of player %d: %w", playerID, err)
	}
	pending, err := eco.Repo.PendingOps(ctx, playerID, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT, pendingViewLimit())
	if err != nil {
		return nil, resultError, fmt.Errorf("read pending donations of player %d: %w", playerID, err)
	}
	recent, err := eco.Repo.RecentOps(ctx, playerID, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT, recentScanLimit)
	if err != nil {
		return nil, resultError, fmt.Errorf("read recent donations of player %d: %w", playerID, err)
	}

	// 只展示**本帮**的捐献:离帮前在别的帮发起的捐献与当前帮会的页面无关。
	ownDonation := func(row data.AssetOpRow) bool {
		return row.Kind == pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE && row.GuildID == g.GuildID
	}
	resp := &pb.GetGuildDonateOptionsResponse{
		ContributionTotal:   total,
		ContributionBalance: balance,
		NextDailyResetMs:    uint64(gameday.NextDailyReset(now).UnixMilli()),
	}
	for _, row := range sortedDonateRows() {
		resp.Options = append(resp.Options, &pb.GuildDonateOptionView{
			DonateId:         row.GetId(),
			Name:             row.GetName(),
			CurrencyType:     row.GetCurrencyType(),
			CostAmount:       row.GetCostAmount(),
			ContributionGain: row.GetContributionGain(),
			FundsGain:        row.GetFundsGain(),
			DailyLimit:       row.GetDailyLimit(),
			UsedToday:        usage[row.GetId()],
			MinGuildLevel:    row.GetMinGuildLevel(),
			Unlocked:         g.Level >= row.GetMinGuildLevel(),
		})
	}
	for _, row := range pending {
		if ownDonation(row) {
			resp.PendingDonations = append(resp.PendingDonations, donationViewOf(row))
		}
	}
	for _, row := range recentResults(recent, readMs, ownDonation) {
		resp.RecentResults = append(resp.RecentResults, donationViewOf(row))
	}
	return resp, resultOK, nil
}

// GetGuildShop:商店页。
func (l *GuildLogic) GetGuildShop(ctx context.Context, req *pb.GetGuildShopRequest) (*pb.GetGuildShopResponse, error) {
	resp, result, err := l.getGuildShop(ctx, req)
	guildEconomyRequestsTotal.Inc(economyRPCShop, result)
	return resp, err
}

func (l *GuildLogic) getGuildShop(ctx context.Context, _ *pb.GetGuildShopRequest) (*pb.GetGuildShopResponse, string, error) {
	eco := l.economy
	if eco == nil {
		return nil, resultUnavailable, errEconomyNotEnabled()
	}
	now := eco.Now()
	readMs := uint64(now.UnixMilli())
	playerID, g, tip, err := l.economyCaller(ctx)
	if err != nil {
		return nil, resultOfFault(err), err
	}
	if tip != nil {
		return &pb.GetGuildShopResponse{ErrorMessage: tip}, resultNotMember, nil
	}

	_, balance, found, err := eco.Repo.MemberContribution(ctx, g.GuildID, playerID)
	if err != nil {
		return nil, resultError, fmt.Errorf("read contribution of player %d: %w", playerID, err)
	}
	if !found {
		l.verifyMapping(ctx, playerID, g.GuildID)
		return &pb.GetGuildShopResponse{ErrorMessage: tipErr(constants.ErrNotInGuild, "not a member of the guild")}, resultNotMember, nil
	}
	usage, err := eco.Repo.ShopUsage(ctx, playerID, gameday.DayKey(now), gameday.WeekKey(now))
	if err != nil {
		return nil, resultError, fmt.Errorf("read shop usage of player %d: %w", playerID, err)
	}
	pending, err := eco.Repo.PendingOps(ctx, playerID, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT, pendingViewLimit())
	if err != nil {
		return nil, resultError, fmt.Errorf("read pending shop orders of player %d: %w", playerID, err)
	}
	recent, err := eco.Repo.RecentOps(ctx, playerID, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT, recentScanLimit)
	if err != nil {
		return nil, resultError, fmt.Errorf("read recent shop orders of player %d: %w", playerID, err)
	}

	// GUILD_CREDIT 流上还有活动奖励(B6),这里只要兑换。兑换不按帮会过滤:物品属于玩家,
	// 离帮前买的东西照常到账,玩家要能在商店页看到它。
	isShopOrder := func(row data.AssetOpRow) bool {
		return row.Kind == pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP
	}
	resp := &pb.GetGuildShopResponse{
		ContributionBalance: balance,
		NextDailyResetMs:    uint64(gameday.NextDailyReset(now).UnixMilli()),
		NextWeeklyResetMs:   uint64(gameday.NextWeeklyReset(now).UnixMilli()),
	}
	for _, row := range sortedShopRows() {
		maxStack, ok := itemMaxStack(row.GetItemId())
		if !ok {
			// 读路径不因一行坏配表整页失败:这件商品显示为不可兑换(单次上限 0),同时留 ERROR。
			logx.Errorf("[GuildEconomy] Item row %d of GuildShop[%d] missing, shown as not purchasable", row.GetItemId(), row.GetId())
		}
		resp.Goods = append(resp.Goods, &pb.GuildShopGoodsView{
			GoodsId:            row.GetId(),
			Name:               row.GetName(),
			Category:           row.GetCategory(),
			ItemId:             row.GetItemId(),
			ItemCount:          row.GetItemCount(),
			CostContribution:   row.GetCostContribution(),
			RequiredGuildLevel: row.GetRequiredGuildLevel(),
			Unlocked:           g.Level >= row.GetRequiredGuildLevel(),
			LimitPeriod:        row.GetLimitPeriod(),
			LimitCount:         row.GetLimitCount(),
			UsedCount:          shopUsedCount(row, usage, now),
			MaxBuyCount:        MaxBuyCount(row, maxStack),
		})
	}
	for _, row := range pending {
		if isShopOrder(row) {
			resp.PendingOrders = append(resp.PendingOrders, shopOrderViewOf(row))
		}
	}
	for _, row := range recentResults(recent, readMs, isShopOrder) {
		resp.RecentOrders = append(resp.RecentOrders, shopOrderViewOf(row))
	}
	return resp, resultOK, nil
}

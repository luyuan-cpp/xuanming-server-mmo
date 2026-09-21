package logic

// 帮会经济的 logic 层:捐献、升级、商店与两个读 RPC(docs/design/guild-phase2/05-economy.md
// §5.24–§5.30;顶部三个覆盖块、90-consistency.md 与 B5b 实现契约效力高于正文)。
//
// 四条贯穿全文件的纪律:
//
//  1. **身份只认会话,帮会只认复核过的映射**(Y-01):economyCaller = callerOf + operatorGuild。
//     经济 RPC **不查**归属 zone(R4):S2 的申请事务已保证"是成员 ⇒ 归属区一致",
//     合服窗口由事务内的闸门兜底,省下的同步预算留给 scene 同步投递。
//  2. **合服闸门在事务里**:zone 一律取事务内读到的 guild.zone_id(缓存里的 zone 在合服后会说谎
//     一个 TTL);闸门读不到按封锁处理(fail-closed),见 economyFence。
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
	"shared/assetop"
	"shared/gameday"
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
	resultFence          = "fence"
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
// 顺序:会话身份 → operatorGuild(缓存读到 0 时用 MySQL 复核)→ 帮会快照 → 快照里有本人。
// **不查归属 zone**(R4,见文件头纪律 1)。快照里没有本人时回未入帮(省一次发号与建行),
// 并与"帮会不存在"一支同口径以 MySQL 复核映射。
//
// 返回约定同 clientWrite:err 非 nil = 故障;tip 非 nil = 业务拒绝;两者都为 nil 时 g 非 nil。
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
	if g == nil {
		// 映射指着一个已不存在的帮会(解散与缓存失效赛跑):以 MySQL 纠正映射后回未入帮。
		l.verifyMapping(ctx, who.playerID, guildID)
		return who.playerID, nil, tipErr(constants.ErrNotInGuild, "not in any guild"), nil
	}
	if _, ok := memberOf(g, who.playerID); !ok {
		// 映射指着一个快照里没有他的帮(典型:离帮后入了别的帮,审批提交后映射失效失败)。operatorGuild 只在
		// 读到 0 时复核,这里不复核的话,这条路径永远纠正不了非 0 的陈旧映射,他在新帮的经济 RPC 会在整个
		// 映射 TTL 内都回未入帮(Y-01)。复核失败只记日志,不改变本次回答。
		l.verifyMapping(ctx, who.playerID, guildID)
		return who.playerID, nil, tipErr(constants.ErrNotInGuild, "not a member of the guild"), nil
	}
	return who.playerID, g, nil, nil
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

// economyFence 满足 data.FenceFunc:repo 在事务内拿读到的 guild.zone_id 调它。
//
// 与 mergeFenceTip 同一套三分支,区别只在返回形态 —— 这里回 data.ErrZoneMerging 让事务回滚,
// 由 economyTip 统一翻成 kGuildZoneMerging:
//   - 闸门未配置或 zone 为 0 → 放行(闸门是可选加固,guild.go 启动时已打过 Info);
//   - 读不到 → **拒绝**(fail-closed):合服窗口里写出来的帮会资金会落在一个正在搬迁的 zone 上;
//   - 标记存在 → 拒绝。
//
// 它在持有成员行锁期间执行一次 Redis 读,多花几毫秒可以接受(05 §5.15)。
func (l *GuildLogic) economyFence(ctx context.Context, zoneID uint32) error {
	if l.mergeFence == nil || zoneID == 0 {
		return nil
	}
	merging, err := l.mergeFence.MergeInProgress(ctx, zoneID)
	if err != nil {
		logx.Errorf("[GuildEconomy] merge fence unreadable for zone %d, refusing (fail closed): %v", zoneID, err)
		return fmt.Errorf("%w: fence unreadable: %v", data.ErrZoneMerging, err)
	}
	if merging {
		logx.Infof("[GuildEconomy] refused: zone %d is merging (%s present)", zoneID, MergeFenceKey(zoneID))
		return data.ErrZoneMerging
	}
	return nil
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
	case errors.Is(err, data.ErrZoneMerging):
		return tipErr(constants.ErrZoneMerging, "zone merging"), resultFence, nil
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
func orderViewOf(st pb.GuildAssetOpStatus, lastReason, reasonTipID uint32) (pb.GuildAssetOrderStatus, uint32) {
	switch st {
	case pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING:
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, lastReason
	case pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED:
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED, reasonTipID
	case pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED:
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_REJECTED, reasonTipID
	case pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED:
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_ABORTED, reasonTipID
	case pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL:
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED_PARTIAL, reasonTipID
	default:
		return pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_UNSPECIFIED, 0
	}
}

// isTerminalOpStatus:进入"最近结果"的四种终态。
func isTerminalOpStatus(st pb.GuildAssetOpStatus) bool {
	switch st {
	case pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED,
		pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED,
		pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED,
		pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL:
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
func (l *GuildLogic) OnAssetFinalized(ctx context.Context, op data.FinalizedOp) {
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

// ── 捐献(05 §5.25)────────────────────────────────────────────

// DonateToGuild:发起一笔捐献。流程:前置 → 配表与缓存预判 → 发号 → 预留事务(建 PENDING 行、占今日次数)
// → 同步投递一次 → 回读状态装配视图。
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
		Fence:            l.economyFence,
	})
	if err != nil {
		rejectTip, result, fault := l.economyTip(ctx, playerID, g.GuildID, err)
		return &pb.DonateToGuildResponse{ErrorMessage: rejectTip}, result, fault
	}

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
	res, err := eco.Repo.UpgradeGuild(ctx, g.GuildID, playerID, req.GetExpectedLevel(), upgradeLevelLookup, l.economyFence)
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
	}
	// Changed=false(expected_level 与库里不符 = 已被别人升过)同样回成功 + 最新 GuildInfo,不再扣资金。
	return &pb.UpgradeGuildResponse{Guild: l.freshGuildInfo(ctx, g.GuildID, playerID)}, result, nil
}

// ── 兑换(05 §5.27)────────────────────────────────────────────

// BuyGuildShopGoods:花可用帮贡兑换商品。流程与捐献同形,区别:GUILD_CREDIT 流、永不中止(deadline 0)、
// 事务内先扣帮贡再投递;scene 永久拒绝时由 Store.Finalize 退帮贡与限购。
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
	start := eco.Now()
	playerID, g, tip, err := l.economyCaller(ctx)
	if err != nil {
		return nil, resultOfFault(err), err
	}
	if tip != nil {
		return &pb.BuyGuildShopGoodsResponse{ErrorMessage: tip}, resultNotMember, nil
	}
	if eco.Loop == nil {
		return &pb.BuyGuildShopGoodsResponse{ErrorMessage: assetChannelDisabledTip()}, resultDisabled, nil
	}

	row, ok := table.GuildShopTableManagerInstance.FindById(req.GetGoodsId())
	if !ok {
		return &pb.BuyGuildShopGoodsResponse{ErrorMessage: tipErr(constants.ErrShopGoodsNotFound, "shop goods not found")}, resultNotFound, nil
	}
	// count=0 是 proto 缺省值(老客户端不填份数),按 1 份处理。
	count := req.GetCount()
	if count == 0 {
		count = 1
	}
	maxStack, ok := itemMaxStack(row.GetItemId())
	if !ok {
		// 启动校验保证商品引用的物品存在,运行期查不到 = 配表被错误替换,fail-closed。
		return nil, resultError, status.Errorf(codes.Internal, "Item row %d of GuildShop[%d] missing", row.GetItemId(), row.GetId())
	}
	if count > MaxBuyCount(row, maxStack) {
		return &pb.BuyGuildShopGoodsResponse{ErrorMessage: tipErr(constants.ErrShopLimit, "count exceeds max buy count")}, resultLimit, nil
	}
	// 以下两条是缓存预判,只为省一次发号与事务;事务内按 MySQL 锁行复核。
	if g.Level < row.GetRequiredGuildLevel() {
		return &pb.BuyGuildShopGoodsResponse{ErrorMessage: tipErr(constants.ErrShopLevelTooLow, "guild level too low")}, resultLevel, nil
	}
	// cost_contribution ≤ 1e9 且 count ≤ MaxShopBuyCount(启动校验),乘积不会溢出。
	cost := row.GetCostContribution() * uint64(count)
	if me, _ := memberOf(g, playerID); me.ContributionBalance < cost {
		return &pb.BuyGuildShopGoodsResponse{ErrorMessage: tipErr(constants.ErrContributionInsufficient, "contribution insufficient")}, resultInsufficient, nil
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
		Fence:              l.economyFence,
	})
	if err != nil {
		rejectTip, result, fault := l.economyTip(ctx, playerID, g.GuildID, err)
		return &pb.BuyGuildShopGoodsResponse{ErrorMessage: rejectTip}, result, fault
	}

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

// refundedBalance:兑换被拒 / 中止后 Finalize 已退回帮贡,余额改为直读 MySQL。
// 读失败只记日志、回预留时的余额(偏低,纯展示):客户端收到终态后会自己再拉一次。
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

// ── 两个读 RPC(05 §5.28;不查合服闸门)────────────────────────

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
		var used uint32
		if periodKey, ok := gameday.PeriodKey(row.GetLimitPeriod(), now); ok && periodKey != 0 {
			used = usage[data.ShopUsageKey{GoodsID: row.GetId(), PeriodKey: periodKey}]
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
			UsedCount:          used,
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

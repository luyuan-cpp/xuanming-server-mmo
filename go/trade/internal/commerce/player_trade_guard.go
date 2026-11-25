package commerce

import (
	"context"
	"database/sql"
	"errors"
	"math"

	tradepb "proto/trade"
	"shared/currencytype"
	"trade/internal/data"
)

const (
	// PlayerTradeModeStrictConsumables（方案 A，个人开发首选）：
	// 禁止游戏内当面交易唯一装备实例、宠物与非耗材大宗核心材料；仅开放低价耗材白名单与单日小额银两互助。
	PlayerTradeModeStrictConsumables = "strict_consumables"
	// PlayerTradeModeAcquaintanceLocked（方案 B，熟人三道硬锁）：
	// 允许熟人当面交易贵重品，但强制执行：互加好友满 N 天（默认 7 天）+ 近 7 日中位参考价偏离拦截或 48 小时冷静锁。
	PlayerTradeModeAcquaintanceLocked = "acquaintance_locked"

	DefaultMinFriendDurationMs     uint64 = 7 * 24 * 3600 * 1000
	DefaultCoolingLockDurationMs   uint64 = 48 * 3600 * 1000
	DefaultMaxReferenceDeviationPct uint32 = 50
	dayMs                          uint64 = 24 * 3600 * 1000
)

// FriendRelationReader 查询两名角色在权威好友服务中的系统好友关系与持续时长（绝不信客户端备注）。
type FriendRelationReader interface {
	FriendshipDurationMs(ctx context.Context, playerA, playerB, nowMs uint64) (isFriend bool, durationMs uint64, err error)
}

// DailyPlayerTradeTracker 查询指定角色当日已通过游戏内当面交易流出的货币总额，用于单日限额防偷渡。
type DailyPlayerTradeTracker interface {
	DailyCurrencyOutflow(ctx context.Context, playerID uint64, currencyType uint32, dayStartMs uint64) (uint64, error)
}

// PlayerTradePolicy 定义游戏内当面交易防骗策略参数。
type PlayerTradePolicy struct {
	Mode                         string
	ConsumableItems              []uint32
	MaxDailyCurrencyAmount       uint64
	ReferenceCurrencyType        uint32
	MinFriendDurationMs          uint64
	MaxReferenceDeviationPct     uint32
	CoolingLockDurationMs        uint64
	AllowAcquaintanceCoolingLock bool
}

// PlayerTradeGuardResult 是当面交易防骗评估结果。
type PlayerTradeGuardResult struct {
	RequireCoolingLock    bool
	CoolingLockDurationMs uint64
	Reason                string
}

// PlayerTradeGuard 对游戏内当面交易实施防站外引流与低价偷渡门禁。
type PlayerTradeGuard struct {
	Policy   PlayerTradePolicy
	Friends  FriendRelationReader
	Prices   ReferencePriceReader
	DailyOut DailyPlayerTradeTracker
}

func selectionEmpty(v *tradepb.TradeAssetSelection) bool {
	return v == nil || (len(v.Items) == 0 && len(v.ItemUuids) == 0 && v.PetId == 0 && v.CurrencyAmount == 0)
}

func (g *PlayerTradeGuard) isConsumableOnly(v *tradepb.TradeAssetSelection) bool {
	if selectionEmpty(v) {
		return true
	}
	if len(v.ItemUuids) > 0 || v.PetId != 0 {
		return false
	}
	for id := range v.Items {
		if !contains(g.Policy.ConsumableItems, id) {
			return false
		}
	}
	return true
}

func (g *PlayerTradeGuard) checkCurrencyCap(ctx context.Context, playerID uint64, sel *tradepb.TradeAssetSelection, nowMs uint64) error {
	if sel == nil || sel.CurrencyAmount == 0 {
		return nil
	}
	if g.Policy.MaxDailyCurrencyAmount == 0 {
		return ErrNotTradable
	}
	if sel.CurrencyAmount > g.Policy.MaxDailyCurrencyAmount {
		return ErrNotTradable
	}
	if g.DailyOut != nil {
		dayStart := (nowMs / dayMs) * dayMs
		used, err := g.DailyOut.DailyCurrencyOutflow(ctx, playerID, sel.CurrencyType, dayStart)
		if err != nil {
			return err
		}
		if used > g.Policy.MaxDailyCurrencyAmount || sel.CurrencyAmount > g.Policy.MaxDailyCurrencyAmount-used {
			return ErrNotTradable
		}
	}
	return nil
}

// Evaluate 校验游戏内当面交易是否符合已启用的防骗策略。
// Mode 为空时不介入（保持与既有历史单测兼容）；启用方案 A 或方案 B 后严格按硬红线 fail-closed 裁决。
func (g *PlayerTradeGuard) Evaluate(ctx context.Context, sellerID, buyerID uint64, sellerAssets, buyerAssets *tradepb.TradeAssetSelection, nowMs uint64) (PlayerTradeGuardResult, error) {
	if g == nil || g.Policy.Mode == "" {
		return PlayerTradeGuardResult{}, nil
	}
	if sellerID == 0 || buyerID == 0 || sellerID == buyerID {
		return PlayerTradeGuardResult{}, ErrInvalid
	}
	if selectionEmpty(sellerAssets) && selectionEmpty(buyerAssets) {
		return PlayerTradeGuardResult{}, ErrInvalid
	}
	switch g.Policy.Mode {
	case PlayerTradeModeStrictConsumables:
		return g.evalStrictConsumables(ctx, sellerID, buyerID, sellerAssets, buyerAssets, nowMs)
	case PlayerTradeModeAcquaintanceLocked:
		return g.evalAcquaintanceLocked(ctx, sellerID, buyerID, sellerAssets, buyerAssets, nowMs)
	default:
		// 未知模式一律 fail-closed，绝不静默放行。
		return PlayerTradeGuardResult{}, ErrNotTradable
	}
}

func (g *PlayerTradeGuard) evalStrictConsumables(ctx context.Context, sellerID, buyerID uint64, sellerAssets, buyerAssets *tradepb.TradeAssetSelection, nowMs uint64) (PlayerTradeGuardResult, error) {
	if !g.isConsumableOnly(sellerAssets) || !g.isConsumableOnly(buyerAssets) {
		return PlayerTradeGuardResult{}, ErrNotTradable
	}
	if err := g.checkCurrencyCap(ctx, sellerID, sellerAssets, nowMs); err != nil {
		return PlayerTradeGuardResult{}, err
	}
	if err := g.checkCurrencyCap(ctx, buyerID, buyerAssets, nowMs); err != nil {
		return PlayerTradeGuardResult{}, err
	}
	return PlayerTradeGuardResult{Reason: "strict_consumables_ok"}, nil
}

func (g *PlayerTradeGuard) evalAcquaintanceLocked(ctx context.Context, sellerID, buyerID uint64, sellerAssets, buyerAssets *tradepb.TradeAssetSelection, nowMs uint64) (PlayerTradeGuardResult, error) {
	oneSided := selectionEmpty(sellerAssets) || selectionEmpty(buyerAssets)
	smallConsumableSwap := !oneSided &&
		g.isConsumableOnly(sellerAssets) && g.isConsumableOnly(buyerAssets) &&
		(g.Policy.MaxDailyCurrencyAmount == 0 ||
			(sellerAssets.GetCurrencyAmount() <= g.Policy.MaxDailyCurrencyAmount &&
				buyerAssets.GetCurrencyAmount() <= g.Policy.MaxDailyCurrencyAmount))
	if smallConsumableSwap {
		if err := g.checkCurrencyCap(ctx, sellerID, sellerAssets, nowMs); err != nil {
			// 超单日小额互助上限时不再视作免检小额，转入下方熟人硬锁判定。
		} else if err := g.checkCurrencyCap(ctx, buyerID, buyerAssets, nowMs); err == nil {
			return PlayerTradeGuardResult{Reason: "small_consumable_swap_ok"}, nil
		}
	}

	// 硬锁 ①：涉及贵重品或单方赠送时，必须查系统权威好友且满足最短好友时长（默认 7 天）。
	if g.Friends == nil {
		return PlayerTradeGuardResult{}, ErrNotTradable
	}
	minFriendMs := g.Policy.MinFriendDurationMs
	if minFriendMs == 0 {
		minFriendMs = DefaultMinFriendDurationMs
	}
	isFriend, durationMs, err := g.Friends.FriendshipDurationMs(ctx, sellerID, buyerID, nowMs)
	if err != nil {
		return PlayerTradeGuardResult{}, err
	}
	if !isFriend || durationMs < minFriendMs {
		return PlayerTradeGuardResult{}, ErrNotTradable
	}

	// 硬锁 ②：近 7 日中位参考价偏离保护（防 0/1 银两偷渡贵重品与高差价逃税）。
	refCurrency := g.Policy.ReferenceCurrencyType
	if refCurrency == 0 {
		refCurrency = currencytype.Gold
	}
	maxDevPct := g.Policy.MaxReferenceDeviationPct
	if maxDevPct == 0 || maxDevPct >= 100 {
		maxDevPct = DefaultMaxReferenceDeviationPct
	}
	sellerVal, sellerUnpriced, err := EstimateSelectionValue(ctx, g.Prices, sellerAssets, refCurrency, nowMs)
	if err != nil {
		return PlayerTradeGuardResult{}, err
	}
	buyerVal, buyerUnpriced, err := EstimateSelectionValue(ctx, g.Prices, buyerAssets, refCurrency, nowMs)
	if err != nil {
		return PlayerTradeGuardResult{}, err
	}

	deviated := oneSided || sellerUnpriced || buyerUnpriced
	if !deviated {
		low, high := sellerVal, buyerVal
		if low > high {
			low, high = high, low
		}
		keepPct := uint64(100 - maxDevPct)
		if high == 0 {
			deviated = true
		} else if high > math.MaxUint64/keepPct || low > math.MaxUint64/100 {
			// 大数防溢出：改用除法比例判定。
			deviated = low < (high/100)*keepPct
		} else {
			deviated = low*100 < high*keepPct
		}
	}

	if !deviated {
		return PlayerTradeGuardResult{Reason: "acquaintance_fair_value_ok"}, nil
	}
	if !g.Policy.AllowAcquaintanceCoolingLock {
		return PlayerTradeGuardResult{}, ErrNotTradable
	}
	lockMs := g.Policy.CoolingLockDurationMs
	if lockMs == 0 {
		lockMs = DefaultCoolingLockDurationMs
	}
	return PlayerTradeGuardResult{
		RequireCoolingLock:    true,
		CoolingLockDurationMs: lockMs,
		Reason:                "acquaintance_deviation_cooling_lock",
	}, nil
}

const maxDailyPlayerTradeScanRows = 1000

func saturatingAddUint64(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

func workflowCurrencyOutflow(rawWorkflow []byte, currencyType uint32, asSeller bool) (uint64, error) {
	if len(rawWorkflow) == 0 {
		return 0, nil
	}
	w, err := data.Workflow(&tradepb.TradeCommerceRecord{WorkflowProto: rawWorkflow})
	if err != nil {
		return 0, err
	}
	sel := w.GetBuyerAssets()
	if asSeller {
		sel = w.GetSellerAssets()
	}
	if sel == nil || sel.GetCurrencyAmount() == 0 || sel.GetCurrencyType() != currencyType {
		return 0, nil
	}
	return sel.GetCurrencyAmount(), nil
}

// SQLDailyPlayerTradeTracker 直接复读 mmorpg_trade.trade_commerce 中当日在途与已结案的游戏内当面交易（kind=2），
// 按买卖双侧走各自索引累加流出总额，堵死拆分小单绕过 MaxDailyCurrencyAmount 的漏洞。
type SQLDailyPlayerTradeTracker struct {
	DB *sql.DB
}

func (t *SQLDailyPlayerTradeTracker) DailyCurrencyOutflow(ctx context.Context, playerID uint64, currencyType uint32, dayStartMs uint64) (uint64, error) {
	if t == nil || t.DB == nil {
		return 0, errors.New("当面交易单日限额存储未装配")
	}
	if playerID == 0 || !currencytype.IsKnown(currencyType) {
		return 0, ErrInvalid
	}
	sellerSum, err := t.sumSide(ctx,
		"SELECT workflow_proto FROM trade_commerce WHERE seller_player_id=? AND kind=2 AND status IN (1,3,4,5,10,11) AND created_ms>=? ORDER BY commerce_id DESC LIMIT ?",
		playerID, currencyType, dayStartMs, true)
	if err != nil {
		return 0, err
	}
	buyerSum, err := t.sumSide(ctx,
		"SELECT workflow_proto FROM trade_commerce WHERE buyer_player_id=? AND kind=2 AND status IN (1,3,4,5,10,11) AND created_ms>=? ORDER BY commerce_id DESC LIMIT ?",
		playerID, currencyType, dayStartMs, false)
	if err != nil {
		return 0, err
	}
	return saturatingAddUint64(sellerSum, buyerSum), nil
}

func (t *SQLDailyPlayerTradeTracker) sumSide(ctx context.Context, query string, playerID uint64, currencyType uint32, dayStartMs uint64, asSeller bool) (uint64, error) {
	rows, err := t.DB.QueryContext(ctx, query, playerID, dayStartMs, maxDailyPlayerTradeScanRows)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var total uint64
	var scanned int
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return 0, err
		}
		scanned++
		part, err := workflowCurrencyOutflow(raw, currencyType, asSeller)
		if err != nil {
			return 0, err
		}
		total = saturatingAddUint64(total, part)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if scanned >= maxDailyPlayerTradeScanRows {
		// 单日单侧当面交易笔数触顶直接饱和拒放，防止刷海量小单绕过累计限额。
		return math.MaxUint64, nil
	}
	return total, nil
}

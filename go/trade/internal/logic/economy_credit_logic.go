package logic

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	tradepb "proto/trade"
	"shared/currencytype"
	"trade/internal/commerce"
	"trade/internal/constants"
	"trade/internal/data"
	"trade/internal/session"
)

const (
	maxSellerReviewCommentRunes = 200
	dayMs                       = uint64(24 * 3600 * 1000)
)

func sellerCreditView(p commerce.SellerCreditProfile) *tradepb.SellerCreditView {
	return &tradepb.SellerCreditView{
		SellerPlayerId:       p.SellerPlayerID,
		OwnerRevision:        p.OwnerRevision,
		OwnershipChanged:     p.OwnershipChanged,
		OwnershipChangedAtMs: p.OwnershipChangedAtMs,
		EpochStartedAtMs:     p.EpochStartedAtMs,
		CompletedOrders:      p.CompletedOrders,
		PositiveReviews:      p.PositiveReviews,
		NeutralReviews:       p.NeutralReviews,
		NegativeReviews:      p.NegativeReviews,
		ViolationCount:       p.ViolationCount,
		EnvCreditCleared:     p.EnvCreditCleared,
		EnvResetCount:        p.EnvResetCount,
		LastEnvChangedAtMs:   p.LastEnvChangedAtMs,
		CreditScore:          p.CreditScore,
		StarRatingTenths:     p.StarRatingTenths,
		PositiveRateBps:      p.PositiveRateBps,
		RiskTier:             string(p.RiskTier),
	}
}

func parseCommerceOrderID(raw string) (uint64, bool) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "commerce:")
	if trimmed == "" {
		return 0, false
	}
	id, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}

// GetSellerCredit 查询指定卖家在当前权威持有周期下的店铺信用档案（严格脱敏，不下发账号/IP/设备）。
func (l *JubaozhaiLogic) GetSellerCredit(in *tradepb.GetSellerCreditRequest) (*tradepb.SellerCreditResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	nowMs := nowMillis(l.deps.Now)
	out := &tradepb.SellerCreditResponse{ServerNowMs: nowMs}

	me, ok := session.ClientPlayerID(l.ctx)
	if !ok {
		out.ErrorMessage = tipOf(constants.ErrInvalidParameter)
		return out, nil
	}
	if l.deps.Credits == nil {
		out.ErrorMessage = tipOf(constants.ErrFeatureDisabled)
		return out, nil
	}
	targetID := in.GetSellerPlayerId()
	if targetID == 0 {
		targetID = me
	}
	prof, err := l.deps.Credits.GetProfile(l.ctx, targetID, nowMs)
	if err != nil {
		code := commerceCode(err)
		if code == constants.ErrServiceUnavailable {
			l.Errorf("[trade] GetSellerCredit failed seller_id=%d: %v", targetID, err)
		}
		out.ErrorMessage = tipOf(code)
		return out, nil
	}
	out.Profile = sellerCreditView(prof)
	return out, nil
}

// SubmitSellerReview 买家对官方托管已结案订单在 7 日窗口内提交好评/中评/差评。
// 订单归属、成交金额与卖家过户修订号一律从权威 trade_commerce 原件提取，绝不采信客户端声称。
func (l *JubaozhaiLogic) SubmitSellerReview(in *tradepb.SubmitSellerReviewRequest) (*tradepb.SubmitSellerReviewResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	nowMs := nowMillis(l.deps.Now)
	out := &tradepb.SubmitSellerReviewResponse{ServerNowMs: nowMs}

	me, ok := session.ClientPlayerID(l.ctx)
	if !ok {
		out.ErrorMessage = tipOf(constants.ErrInvalidParameter)
		return out, nil
	}
	if l.deps.Credits == nil || l.deps.Commerce == nil || l.deps.Commerce.Repo == nil {
		out.ErrorMessage = tipOf(constants.ErrFeatureDisabled)
		return out, nil
	}
	rating := commerce.SellerReviewRating(in.GetRating())
	if rating != commerce.SellerRatingPositive && rating != commerce.SellerRatingNeutral && rating != commerce.SellerRatingNegative {
		out.ErrorMessage = tipOf(constants.ErrInvalidParameter)
		return out, nil
	}
	comment := strings.TrimSpace(in.GetComment())
	if !utf8.ValidString(comment) || utf8.RuneCountInString(comment) > maxSellerReviewCommentRunes {
		out.ErrorMessage = tipOf(constants.ErrInvalidParameter)
		return out, nil
	}
	if comment != "" {
		if rejectTip := l.checkTradeTitle(me, comment); rejectTip != 0 {
			out.ErrorMessage = tipOf(rejectTip)
			return out, nil
		}
	}
	orderNum, validID := parseCommerceOrderID(in.GetOrderId())
	if !validID {
		out.ErrorMessage = tipOf(constants.ErrInvalidParameter)
		return out, nil
	}
	rec, err := l.deps.Commerce.Repo.Get(l.ctx, orderNum)
	if errors.Is(err, sql.ErrNoRows) {
		out.ErrorMessage = tipOf(commerceCode(commerce.ErrMissing))
		return out, nil
	}
	if err != nil {
		l.Errorf("[trade] SubmitSellerReview load order=%d failed: %v", orderNum, err)
		out.ErrorMessage = tipOf(constants.ErrServiceUnavailable)
		return out, nil
	}
	wf, err := data.Workflow(rec)
	if err != nil {
		l.Errorf("[trade] SubmitSellerReview decode order=%d failed: %v", orderNum, err)
		out.ErrorMessage = tipOf(constants.ErrServiceUnavailable)
		return out, nil
	}
	fact, ok := commerce.SettledTradeFact(rec, wf, rec.UpdatedMs)
	if !ok {
		out.ErrorMessage = tipOf(commerceCode(commerce.ErrConflict))
		return out, nil
	}
	if fact.BuyerPlayerID != me {
		out.ErrorMessage = tipOf(commerceCode(commerce.ErrMissing))
		return out, nil
	}
	reviewRec, prof, replayed, err := l.deps.Credits.SubmitReview(l.ctx, commerce.SellerReviewRequest{
		Trade:   fact,
		Rating:  rating,
		Comment: comment,
	}, nowMs)
	if err != nil {
		code := commerceCode(err)
		if code == constants.ErrServiceUnavailable {
			l.Errorf("[trade] SubmitSellerReview persist order=%d failed: %v", orderNum, err)
		}
		out.ErrorMessage = tipOf(code)
		return out, nil
	}
	out.CountedForScore = reviewRec.CountedForScore
	out.Replayed = replayed
	out.Profile = sellerCreditView(prof)
	return out, nil
}

// GetMarketReferencePrice 查询指定标准品在近 7 日真实成交中的中位参考价（p50）及样本量；不足 10 笔明确标记样本不足。
func (l *JubaozhaiLogic) GetMarketReferencePrice(in *tradepb.GetMarketReferencePriceRequest) (*tradepb.GetMarketReferencePriceResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	nowMs := nowMillis(l.deps.Now)
	out := &tradepb.GetMarketReferencePriceResponse{
		ItemConfigId: in.GetItemConfigId(),
		CurrencyType: in.GetCurrencyType(),
		WindowDays:   7,
		ServerNowMs:  nowMs,
	}

	if _, ok := session.ClientPlayerID(l.ctx); !ok {
		out.ErrorMessage = tipOf(constants.ErrInvalidParameter)
		return out, nil
	}
	if in.GetItemConfigId() == 0 || !currencytype.IsKnown(in.GetCurrencyType()) {
		out.ErrorMessage = tipOf(constants.ErrInvalidParameter)
		return out, nil
	}
	if l.deps.ReferencePrices == nil {
		out.ErrorMessage = tipOf(constants.ErrFeatureDisabled)
		return out, nil
	}
	ref, err := l.deps.ReferencePrices.ReferencePrice(l.ctx, in.GetItemConfigId(), in.GetCurrencyType(), nowMs)
	if err != nil {
		l.Errorf("[trade] GetMarketReferencePrice item=%d currency=%d failed: %v", in.GetItemConfigId(), in.GetCurrencyType(), err)
		out.ErrorMessage = tipOf(constants.ErrServiceUnavailable)
		return out, nil
	}
	out.MedianUnitPrice = ref.MedianUnitPrice
	out.SampleCount = uint32(ref.SampleCount)
	out.InsufficientSample = ref.InsufficientSample
	return out, nil
}

// GetPlayerTradeQuota 返回当前游戏内当面交易防骗模式、当日累计已用/剩余银两额度与耗材白名单。
func (l *JubaozhaiLogic) GetPlayerTradeQuota(in *tradepb.GetPlayerTradeQuotaRequest) (*tradepb.GetPlayerTradeQuotaResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	nowMs := nowMillis(l.deps.Now)
	currency := in.GetCurrencyType()
	out := &tradepb.GetPlayerTradeQuotaResponse{
		CurrencyType: currency,
		ServerNowMs:  nowMs,
	}

	me, ok := session.ClientPlayerID(l.ctx)
	if !ok || !currencytype.IsKnown(currency) {
		out.ErrorMessage = tipOf(constants.ErrInvalidParameter)
		return out, nil
	}
	cfg := l.deps.Config.Commerce.PlayerTrade
	out.Mode = cfg.Mode
	out.DailyCurrencyLimit = cfg.MaxDailyCurrencyAmount
	out.ConsumableItemIds = append([]uint32(nil), cfg.ConsumableItems...)
	out.MinFriendDurationMs = cfg.MinFriendDurationMs
	if out.MinFriendDurationMs == 0 && cfg.Mode == commerce.PlayerTradeModeAcquaintanceLocked {
		out.MinFriendDurationMs = commerce.DefaultMinFriendDurationMs
	}
	out.MaxReferenceDeviationPct = cfg.MaxReferenceDeviationPct
	if out.MaxReferenceDeviationPct == 0 && cfg.Mode == commerce.PlayerTradeModeAcquaintanceLocked {
		out.MaxReferenceDeviationPct = commerce.DefaultMaxReferenceDeviationPct
	}
	out.CoolingLockDurationMs = cfg.CoolingLockDurationMs
	if out.CoolingLockDurationMs == 0 && cfg.Mode == commerce.PlayerTradeModeAcquaintanceLocked {
		out.CoolingLockDurationMs = commerce.DefaultCoolingLockDurationMs
	}

	var used uint64
	if l.deps.Commerce != nil && l.deps.Commerce.PlayerTradeGuard != nil && l.deps.Commerce.PlayerTradeGuard.DailyOut != nil {
		dayStart := (nowMs / dayMs) * dayMs
		var err error
		used, err = queryDailyOutflow(l.ctx, l.deps.Commerce.PlayerTradeGuard.DailyOut, me, currency, dayStart)
		if err != nil {
			l.Errorf("[trade] GetPlayerTradeQuota player=%d currency=%d failed: %v", me, currency, err)
			out.ErrorMessage = tipOf(constants.ErrServiceUnavailable)
			return out, nil
		}
	}
	out.DailyCurrencyUsed = used
	if out.DailyCurrencyLimit > used {
		out.DailyCurrencyRemaining = out.DailyCurrencyLimit - used
	}
	return out, nil
}

func queryDailyOutflow(ctx context.Context, tracker commerce.DailyPlayerTradeTracker, playerID uint64, currencyType uint32, dayStartMs uint64) (uint64, error) {
	return tracker.DailyCurrencyOutflow(ctx, playerID, currencyType, dayStartMs)
}

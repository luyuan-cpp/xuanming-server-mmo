package commerce

import (
	"context"
	"errors"
	"strconv"
	"sync"

	dspb "proto/data_service"
)

// SellerReviewRating 是买家对真实已完成托管交易给出的店铺评价档位。
type SellerReviewRating uint8

const (
	SellerRatingUnspecified SellerReviewRating = 0
	SellerRatingPositive    SellerReviewRating = 1 // 好评
	SellerRatingNeutral     SellerReviewRating = 2 // 中评
	SellerRatingNegative    SellerReviewRating = 3 // 差评
)

// SellerRiskTier 是卖家信用派生的风控与额度档位。
type SellerRiskTier string

const (
	SellerRiskTierRestricted       SellerRiskTier = "restricted"         // 有核实违约/拒付或低分受限
	SellerRiskTierNewOrTransferred SellerRiskTier = "new_or_transferred" // 新店或刚过户换人重算保护期
	SellerRiskTierStandard         SellerRiskTier = "standard"           // 正常稳定卖家
	SellerRiskTierTrusted          SellerRiskTier = "trusted"            // 高信誉老店（解锁更高挂单/提现额度）
)

const (
	DefaultSellerInitialScore      uint32 = 60
	DefaultSellerInitialStarTenths uint32 = 30 // 3.0 星
	DefaultReviewWindowMs          uint64 = 7 * 24 * 3600 * 1000
	DefaultPairScoreWindowMs       uint64 = 7 * 24 * 3600 * 1000
	DefaultMaxScoredPairReviews           = 1
	positiveDelta                         = 2
	negativePenalty                       = 6
	violationPenalty                      = 25
)

// RoleOwnershipLookup 查询角色当前权威持有账号与单调递增的过户修订号 owner_revision。
// 生产装配直接复用 RoleTradeAuthority.Lookup。
type RoleOwnershipLookup interface {
	Lookup(ctx context.Context, playerID uint64) (*dspb.AccountRoleOwnership, error)
}

// SellerCreditKey 用 (角色 ID, 归属修订号) 唯一隔离一任持有者的店铺信用；
// 角色发生过户后 owner_revision 递增，天然与前任信用彻底隔离，杜绝“买高分老号行骗”。
type SellerCreditKey struct {
	SellerPlayerID uint64
	OwnerRevision  uint64
}

// SellerCreditProfile 是某角色在当前持有周期内的对外店铺评分档案。
type SellerCreditProfile struct {
	SellerPlayerID       uint64
	OwnerAccount         string
	OwnerRevision        uint64
	OwnershipChanged     bool   // true 表示曾发生换人/过户，历史信用已清零重算
	OwnershipChangedAtMs uint64 // 最近一次换人重算时刻（毫秒）
	EpochStartedAtMs     uint64
	CompletedOrders      uint64
	PositiveReviews      uint64
	NeutralReviews       uint64
	NegativeReviews      uint64
	ViolationCount       uint64
	CreditScore          uint32 // [0, 100]
	StarRatingTenths     uint32 // [10, 50] => 1.0★ ~ 5.0★
	PositiveRateBps      uint32 // [0, 10000] 万分比好评率
	RiskTier             SellerRiskTier
}

// CompletedTradeFact 描述一笔经官方托管真实完成的交易事实。
type CompletedTradeFact struct {
	OrderID             string
	SellerPlayerID      uint64
	SellerAccount       string
	SellerOwnerRevision uint64
	BuyerPlayerID       uint64
	BuyerAccount        string
	AmountMinor         uint64
	SettledAtMs         uint64
}

// SellerReviewRequest 是买家在评价窗口内提交的一次店铺评价。
type SellerReviewRequest struct {
	Trade   CompletedTradeFact
	Rating  SellerReviewRating
	Comment string
}

// SellerReviewRecord 是持久化保存的单笔评价凭证（按 OrderID 幂等唯一）。
type SellerReviewRecord struct {
	OrderID             string
	SellerPlayerID      uint64
	SellerOwnerRevision uint64
	BuyerPlayerID       uint64
	BuyerAccount        string
	Rating              SellerReviewRating
	CountedForScore     bool // 因防刷分窗口或低于最低计分金额时为 false（留痕但不涨分）
	CreatedAtMs         uint64
}

// SellerViolationRecord 是一条经核实的卖家违约/欺诈/拒付扣分记录（按 CaseID 幂等唯一）。
type SellerViolationRecord struct {
	CaseID              string
	OrderID             string
	SellerPlayerID      uint64
	SellerAccount       string
	SellerOwnerRevision uint64
	Reason              string
	RecordedAtMs        uint64
}

// SellerCreditPolicy 控制防刷分窗口与最低有效计分门槛。
type SellerCreditPolicy struct {
	ReviewWindowMs                  uint64
	PairScoreWindowMs               uint64
	MaxScoredReviewsPerPairInWindow int
	MinScoreAmountMinor             uint64
}

func (p SellerCreditPolicy) normalized() SellerCreditPolicy {
	if p.ReviewWindowMs == 0 {
		p.ReviewWindowMs = DefaultReviewWindowMs
	}
	if p.PairScoreWindowMs == 0 {
		p.PairScoreWindowMs = DefaultPairScoreWindowMs
	}
	if p.MaxScoredReviewsPerPairInWindow <= 0 {
		p.MaxScoredReviewsPerPairInWindow = DefaultMaxScoredPairReviews
	}
	return p
}

// SellerCreditStore 抽象卖家信用、成交计数、评价与违约记录的持久化操作。
type SellerCreditStore interface {
	GetProfile(ctx context.Context, key SellerCreditKey) (*SellerCreditProfile, bool, error)
	HasPriorEpoch(ctx context.Context, sellerPlayerID, currentRevision uint64) (bool, error)
	SaveProfile(ctx context.Context, profile SellerCreditProfile) error
	MarkOrderCompleted(ctx context.Context, orderID string, key SellerCreditKey) (inserted bool, err error)
	GetReviewByOrder(ctx context.Context, orderID string) (*SellerReviewRecord, bool, error)
	CountPairScoredReviews(ctx context.Context, key SellerCreditKey, buyerKey string, sinceMs, nowMs uint64) (int, error)
	SaveReview(ctx context.Context, rec SellerReviewRecord) error
	HasViolation(ctx context.Context, caseID string) (bool, error)
	SaveViolation(ctx context.Context, rec SellerViolationRecord) error
}

// SellerCreditService 维护类店铺卖家信用评分，并在角色过户/换人时强制清空历史信用。
type SellerCreditService struct {
	Store     SellerCreditStore
	Ownership RoleOwnershipLookup
	Policy    SellerCreditPolicy
}

func newInitialProfile(playerID uint64, account string, revision uint64, changed bool, nowMs uint64) SellerCreditProfile {
	p := SellerCreditProfile{
		SellerPlayerID:   playerID,
		OwnerAccount:     account,
		OwnerRevision:    revision,
		OwnershipChanged: changed,
		EpochStartedAtMs: nowMs,
		CreditScore:      DefaultSellerInitialScore,
		StarRatingTenths: DefaultSellerInitialStarTenths,
		RiskTier:         SellerRiskTierNewOrTransferred,
	}
	if changed {
		p.OwnershipChangedAtMs = nowMs
	}
	return p
}

func recomputeCreditMetrics(p *SellerCreditProfile) {
	score := int64(DefaultSellerInitialScore) +
		int64(p.PositiveReviews)*positiveDelta -
		int64(p.NegativeReviews)*negativePenalty -
		int64(p.ViolationCount)*violationPenalty
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	p.CreditScore = uint32(score)

	totalRated := p.PositiveReviews + p.NeutralReviews + p.NegativeReviews
	if totalRated > 0 {
		p.PositiveRateBps = uint32((p.PositiveReviews * 10000) / totalRated)
	} else {
		p.PositiveRateBps = 0
	}

	if totalRated == 0 && p.ViolationCount == 0 {
		p.StarRatingTenths = DefaultSellerInitialStarTenths
	} else {
		p.StarRatingTenths = 10 + (p.CreditScore*40+50)/100
	}

	switch {
	case p.ViolationCount > 0 || p.CreditScore < 40:
		p.RiskTier = SellerRiskTierRestricted
	case p.CompletedOrders < 5 || (p.OwnershipChanged && p.CompletedOrders < 10):
		p.RiskTier = SellerRiskTierNewOrTransferred
	case p.CompletedOrders >= 20 && p.CreditScore >= 85 && p.PositiveRateBps >= 9500:
		p.RiskTier = SellerRiskTierTrusted
	default:
		p.RiskTier = SellerRiskTierStandard
	}
}

func (s *SellerCreditService) ensureEpochProfile(ctx context.Context, playerID uint64, account string, revision uint64, nowMs uint64) (SellerCreditProfile, error) {
	key := SellerCreditKey{SellerPlayerID: playerID, OwnerRevision: revision}
	existing, ok, err := s.Store.GetProfile(ctx, key)
	if err != nil {
		return SellerCreditProfile{}, err
	}
	if ok {
		// 即使修订号未变但账号发生变化（异常或后台修复场景），也强制按换人重置保护买家。
		if existing.OwnerAccount != "" && account != "" && existing.OwnerAccount != account {
			reset := newInitialProfile(playerID, account, revision, true, nowMs)
			if err := s.Store.SaveProfile(ctx, reset); err != nil {
				return SellerCreditProfile{}, err
			}
			return reset, nil
		}
		return *existing, nil
	}
	hasPrior, err := s.Store.HasPriorEpoch(ctx, playerID, revision)
	if err != nil {
		return SellerCreditProfile{}, err
	}
	changed := hasPrior || revision > 1
	fresh := newInitialProfile(playerID, account, revision, changed, nowMs)
	if err := s.Store.SaveProfile(ctx, fresh); err != nil {
		return SellerCreditProfile{}, err
	}
	return fresh, nil
}

// OnCharacterTransferred 在角色完成官方过户/换账号时主动清空新持有周期的信用档案并标记换人重算。
func (s *SellerCreditService) OnCharacterTransferred(ctx context.Context, playerID uint64, newAccount string, newOwnerRevision uint64, nowMs uint64) (SellerCreditProfile, error) {
	if s == nil || s.Store == nil {
		return SellerCreditProfile{}, errors.New("卖家信用存储未装配")
	}
	if playerID == 0 || newAccount == "" || newOwnerRevision == 0 {
		return SellerCreditProfile{}, ErrInvalid
	}
	fresh := newInitialProfile(playerID, newAccount, newOwnerRevision, true, nowMs)
	if err := s.Store.SaveProfile(ctx, fresh); err != nil {
		return SellerCreditProfile{}, err
	}
	return fresh, nil
}

// GetProfile 返回卖家在当前权威持有者周期下的店铺信用档案。
// 每次查询均校验 RoleOwnershipLookup，角色一旦换人立即自动切换到全新清空档案。
func (s *SellerCreditService) GetProfile(ctx context.Context, sellerPlayerID uint64, nowMs uint64) (SellerCreditProfile, error) {
	if s == nil || s.Store == nil || s.Ownership == nil {
		return SellerCreditProfile{}, errors.New("卖家信用服务未装配")
	}
	if sellerPlayerID == 0 {
		return SellerCreditProfile{}, ErrInvalid
	}
	owned, err := s.Ownership.Lookup(ctx, sellerPlayerID)
	if err != nil {
		return SellerCreditProfile{}, err
	}
	if owned == nil || owned.OwnerRevision == 0 || owned.Account == "" {
		return SellerCreditProfile{}, ErrConflict
	}
	return s.ensureEpochProfile(ctx, sellerPlayerID, owned.Account, owned.OwnerRevision, nowMs)
}

// RecordCompletedTrade 在一笔官方托管交易最终到账完成后累计卖家的真实成交笔数（按 OrderID 幂等，拒绝对敲自买）。
func (s *SellerCreditService) RecordCompletedTrade(ctx context.Context, fact CompletedTradeFact) (SellerCreditProfile, error) {
	if s == nil || s.Store == nil {
		return SellerCreditProfile{}, errors.New("卖家信用服务未装配")
	}
	if fact.OrderID == "" || fact.SellerPlayerID == 0 || fact.BuyerPlayerID == 0 || fact.SellerOwnerRevision == 0 || fact.SellerAccount == "" {
		return SellerCreditProfile{}, ErrInvalid
	}
	if fact.SellerPlayerID == fact.BuyerPlayerID || (fact.BuyerAccount != "" && fact.SellerAccount == fact.BuyerAccount) {
		return SellerCreditProfile{}, ErrSelf
	}
	key := SellerCreditKey{SellerPlayerID: fact.SellerPlayerID, OwnerRevision: fact.SellerOwnerRevision}
	profile, err := s.ensureEpochProfile(ctx, fact.SellerPlayerID, fact.SellerAccount, fact.SellerOwnerRevision, fact.SettledAtMs)
	if err != nil {
		return SellerCreditProfile{}, err
	}
	inserted, err := s.Store.MarkOrderCompleted(ctx, fact.OrderID, key)
	if err != nil {
		return SellerCreditProfile{}, err
	}
	if inserted {
		profile.CompletedOrders++
		recomputeCreditMetrics(&profile)
		if err := s.Store.SaveProfile(ctx, profile); err != nil {
			return SellerCreditProfile{}, err
		}
	}
	return profile, nil
}

// SubmitReview 处理买家对已完成托管订单的一次评价：
// - 绑定原成交时的 SellerOwnerRevision，角色过户后的迟到评价绝不污染新持有者信用；
// - 禁止同账号自评、禁止重复改评刷分、强制评价窗口期；
// - 同一买家↔同一卖家 7 天滚动窗口内超出上限或低于最低计分金额时只留痕不涨信用分。
func (s *SellerCreditService) SubmitReview(ctx context.Context, in SellerReviewRequest, nowMs uint64) (SellerReviewRecord, SellerCreditProfile, bool, error) {
	if s == nil || s.Store == nil {
		return SellerReviewRecord{}, SellerCreditProfile{}, false, errors.New("卖家信用服务未装配")
	}
	t := in.Trade
	if t.OrderID == "" || t.SellerPlayerID == 0 || t.BuyerPlayerID == 0 || t.SellerOwnerRevision == 0 || t.SellerAccount == "" ||
		(in.Rating != SellerRatingPositive && in.Rating != SellerRatingNeutral && in.Rating != SellerRatingNegative) {
		return SellerReviewRecord{}, SellerCreditProfile{}, false, ErrInvalid
	}
	if t.SellerPlayerID == t.BuyerPlayerID || (t.BuyerAccount != "" && t.SellerAccount == t.BuyerAccount) {
		return SellerReviewRecord{}, SellerCreditProfile{}, false, ErrSelf
	}
	policy := s.Policy.normalized()
	if nowMs < t.SettledAtMs || nowMs-t.SettledAtMs > policy.ReviewWindowMs {
		return SellerReviewRecord{}, SellerCreditProfile{}, false, ErrEnded
	}

	key := SellerCreditKey{SellerPlayerID: t.SellerPlayerID, OwnerRevision: t.SellerOwnerRevision}
	if prior, ok, err := s.Store.GetReviewByOrder(ctx, t.OrderID); err != nil {
		return SellerReviewRecord{}, SellerCreditProfile{}, false, err
	} else if ok {
		if prior.Rating != in.Rating || prior.BuyerPlayerID != t.BuyerPlayerID || prior.SellerPlayerID != t.SellerPlayerID {
			return SellerReviewRecord{}, SellerCreditProfile{}, false, ErrReplay
		}
		profile, err := s.ensureEpochProfile(ctx, t.SellerPlayerID, t.SellerAccount, t.SellerOwnerRevision, nowMs)
		return *prior, profile, true, err
	}

	// 若未先调过 RecordCompletedTrade，评价前同步幂等记入本单真实成交。
	profile, err := s.RecordCompletedTrade(ctx, t)
	if err != nil {
		return SellerReviewRecord{}, SellerCreditProfile{}, false, err
	}

	buyerKey := t.BuyerAccount
	if buyerKey == "" {
		buyerKey = "player:" + strconv.FormatUint(t.BuyerPlayerID, 10)
	}
	var windowStart uint64
	if nowMs > policy.PairScoreWindowMs {
		windowStart = nowMs - policy.PairScoreWindowMs
	}
	pairCount, err := s.Store.CountPairScoredReviews(ctx, key, buyerKey, windowStart, nowMs)
	if err != nil {
		return SellerReviewRecord{}, SellerCreditProfile{}, false, err
	}

	counted := true
	if in.Rating == SellerRatingPositive && t.AmountMinor < policy.MinScoreAmountMinor {
		counted = false
	}
	if pairCount >= policy.MaxScoredReviewsPerPairInWindow {
		counted = false
	}

	rec := SellerReviewRecord{
		OrderID:             t.OrderID,
		SellerPlayerID:      t.SellerPlayerID,
		SellerOwnerRevision: t.SellerOwnerRevision,
		BuyerPlayerID:       t.BuyerPlayerID,
		BuyerAccount:        buyerKey,
		Rating:              in.Rating,
		CountedForScore:     counted,
		CreatedAtMs:         nowMs,
	}
	if err := s.Store.SaveReview(ctx, rec); err != nil {
		return SellerReviewRecord{}, SellerCreditProfile{}, false, err
	}
	if counted {
		switch in.Rating {
		case SellerRatingPositive:
			profile.PositiveReviews++
		case SellerRatingNeutral:
			profile.NeutralReviews++
		case SellerRatingNegative:
			profile.NegativeReviews++
		}
		recomputeCreditMetrics(&profile)
		if err := s.Store.SaveProfile(ctx, profile); err != nil {
			return SellerReviewRecord{}, SellerCreditProfile{}, false, err
		}
	}
	return rec, profile, false, nil
}

// RecordViolation 记入一条经人工/渠道核实的违约、欺诈或恶意拒付，按 CaseID 幂等扣减当任持有者信用并降级风控档。
func (s *SellerCreditService) RecordViolation(ctx context.Context, rec SellerViolationRecord) (SellerCreditProfile, error) {
	if s == nil || s.Store == nil {
		return SellerCreditProfile{}, errors.New("卖家信用服务未装配")
	}
	if rec.CaseID == "" || rec.SellerPlayerID == 0 || rec.SellerOwnerRevision == 0 || rec.SellerAccount == "" {
		return SellerCreditProfile{}, ErrInvalid
	}
	profile, err := s.ensureEpochProfile(ctx, rec.SellerPlayerID, rec.SellerAccount, rec.SellerOwnerRevision, rec.RecordedAtMs)
	if err != nil {
		return SellerCreditProfile{}, err
	}
	exists, err := s.Store.HasViolation(ctx, rec.CaseID)
	if err != nil {
		return SellerCreditProfile{}, err
	}
	if exists {
		return profile, nil
	}
	if err := s.Store.SaveViolation(ctx, rec); err != nil {
		return SellerCreditProfile{}, err
	}
	profile.ViolationCount++
	recomputeCreditMetrics(&profile)
	if err := s.Store.SaveProfile(ctx, profile); err != nil {
		return SellerCreditProfile{}, err
	}
	return profile, nil
}

// MemorySellerCreditStore 是线程安全的内存实现，供单元测试与隔离原型演练使用。
type MemorySellerCreditStore struct {
	mu              sync.Mutex
	profiles        map[SellerCreditKey]SellerCreditProfile
	completedOrders map[string]SellerCreditKey
	reviewsByOrder  map[string]SellerReviewRecord
	reviews         []SellerReviewRecord
	violations      map[string]SellerViolationRecord
}

func NewMemorySellerCreditStore() *MemorySellerCreditStore {
	return &MemorySellerCreditStore{
		profiles:        map[SellerCreditKey]SellerCreditProfile{},
		completedOrders: map[string]SellerCreditKey{},
		reviewsByOrder:  map[string]SellerReviewRecord{},
		violations:      map[string]SellerViolationRecord{},
	}
}

func (m *MemorySellerCreditStore) GetProfile(_ context.Context, key SellerCreditKey) (*SellerCreditProfile, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.profiles[key]
	if !ok {
		return nil, false, nil
	}
	return &p, true, nil
}

func (m *MemorySellerCreditStore) HasPriorEpoch(_ context.Context, sellerPlayerID, currentRevision uint64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.profiles {
		if k.SellerPlayerID == sellerPlayerID && k.OwnerRevision < currentRevision {
			return true, nil
		}
	}
	return false, nil
}

func (m *MemorySellerCreditStore) SaveProfile(_ context.Context, profile SellerCreditProfile) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.profiles[SellerCreditKey{SellerPlayerID: profile.SellerPlayerID, OwnerRevision: profile.OwnerRevision}] = profile
	return nil
}

func (m *MemorySellerCreditStore) MarkOrderCompleted(_ context.Context, orderID string, key SellerCreditKey) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.completedOrders[orderID]; exists {
		return false, nil
	}
	m.completedOrders[orderID] = key
	return true, nil
}

func (m *MemorySellerCreditStore) GetReviewByOrder(_ context.Context, orderID string) (*SellerReviewRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.reviewsByOrder[orderID]
	if !ok {
		return nil, false, nil
	}
	return &r, true, nil
}

func (m *MemorySellerCreditStore) CountPairScoredReviews(_ context.Context, key SellerCreditKey, buyerKey string, sinceMs, nowMs uint64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, r := range m.reviews {
		if r.SellerPlayerID == key.SellerPlayerID && r.SellerOwnerRevision == key.OwnerRevision &&
			r.BuyerAccount == buyerKey && r.CountedForScore && r.CreatedAtMs >= sinceMs && r.CreatedAtMs <= nowMs {
			count++
		}
	}
	return count, nil
}

func (m *MemorySellerCreditStore) SaveReview(_ context.Context, rec SellerReviewRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reviewsByOrder[rec.OrderID] = rec
	m.reviews = append(m.reviews, rec)
	return nil
}

func (m *MemorySellerCreditStore) HasViolation(_ context.Context, caseID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.violations[caseID]
	return ok, nil
}

func (m *MemorySellerCreditStore) SaveViolation(_ context.Context, rec SellerViolationRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.violations[rec.CaseID] = rec
	return nil
}

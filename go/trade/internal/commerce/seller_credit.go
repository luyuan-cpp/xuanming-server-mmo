package commerce

import (
	"context"
	"errors"
	"strconv"
	"strings"
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
	maxKnownSellerDevices                 = 8 // 单持有周期内最多保留的常用设备指纹数（有界防膨胀）
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
// LastIP / LastDeviceID 仅供服务端内部比对与防刷分判定，不得直接下发给其他玩家或作为 Prometheus 标签。
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
	EnvCreditCleared     bool     // true 表示检测到异地陌生新设备登录（私下卖号/借号/盗号风险），历史正面信用已直接清 0
	EnvResetCount        uint64   // 本持有周期内触发异地换机清 0 的次数
	LastEnvChangedAtMs   uint64   // 最近一次触发异地换机清 0 的时刻（毫秒）
	LastIP               string   // 最近一次规范化的卖家登录/操作 IP（仅服务端内部使用）
	LastDeviceID         string   // 最近一次规范化的卖家设备指纹（仅服务端内部使用）
	KnownDevices         []string // 当前持有周期内已登记的常用设备指纹（上限 maxKnownSellerDevices，过户或异地换机重置）
	CreditScore          uint32 // [0, 100]
	StarRatingTenths     uint32 // [10, 50] => 1.0★ ~ 5.0★
	PositiveRateBps      uint32 // [0, 10000] 万分比好评率
	RiskTier             SellerRiskTier
}

// SellerEnvironmentEvent 描述卖家一次登录或交易操作时由服务端可信网关/会话采集的环境信息。
type SellerEnvironmentEvent struct {
	SellerPlayerID uint64
	OwnerAccount   string
	OwnerRevision  uint64
	IP             string
	DeviceID       string
	ObservedAtMs   uint64
}

// CompletedTradeFact 描述一笔经官方托管真实完成的交易事实（含可选的服务端可信 IP/设备指纹用于防刷分）。
type CompletedTradeFact struct {
	OrderID             string
	SellerPlayerID      uint64
	SellerAccount       string
	SellerOwnerRevision uint64
	SellerIP            string
	SellerDeviceID      string
	BuyerPlayerID       uint64
	BuyerAccount        string
	BuyerIP             string
	BuyerDeviceID       string
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
	BuyerIP             string
	BuyerDeviceID       string
	Rating              SellerReviewRating
	CountedForScore     bool // 因同 IP/同设备、防刷分窗口或低于最低计分金额时为 false（留痕但不涨分）
	Comment             string
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

// SellerCreditPolicy 控制防刷分窗口、最低有效计分门槛与异地换机清零后的防突击好评冷静期。
type SellerCreditPolicy struct {
	ReviewWindowMs                  uint64
	PairScoreWindowMs               uint64
	MaxScoredReviewsPerPairInWindow int
	MinScoreAmountMinor             uint64
	// PostEnvChangeScoreCooldownMs 卖家触发异地换机清 0 后的一段防刷分冷静期内，新好评不涨分（0 表示关闭）。
	PostEnvChangeScoreCooldownMs uint64
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
	// CountPairScoredReviews 统计窗口内来自同一买家账号、同一买家 IP 或同一买家设备的已计分评价数，防多小号同机/同网刷分。
	CountPairScoredReviews(ctx context.Context, key SellerCreditKey, buyerKey, buyerIP, buyerDeviceID string, sinceMs, nowMs uint64) (int, error)
	SaveReview(ctx context.Context, rec SellerReviewRecord) error
	HasViolation(ctx context.Context, caseID string) (bool, error)
	SaveViolation(ctx context.Context, rec SellerViolationRecord) error
}

// SellerCreditService 维护类店铺卖家信用评分，并在角色官方过户换人时强制清空、异地换陌生新设备时直接清 0 并严格防刷分。
type SellerCreditService struct {
	Store     SellerCreditStore
	Ownership RoleOwnershipLookup
	Policy    SellerCreditPolicy
}

func normalizeEnvToken(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func containsString(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

func rememberKnownDevice(devices []string, device string) []string {
	if device == "" || containsString(devices, device) {
		return devices
	}
	out := append(append([]string(nil), devices...), device)
	if len(out) > maxKnownSellerDevices {
		out = out[len(out)-maxKnownSellerDevices:]
	}
	return out
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
	baseScore := int64(DefaultSellerInitialScore)
	if p.EnvCreditCleared {
		// 异地陌生新设备触发直接清 0 后，新起点基准为 0（而非回弹到 60），必须重新从 0 积累真实好评。
		baseScore = 0
	}
	score := baseScore +
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

	if !p.EnvCreditCleared && totalRated == 0 && p.ViolationCount == 0 {
		p.StarRatingTenths = DefaultSellerInitialStarTenths
	} else {
		p.StarRatingTenths = 10 + (p.CreditScore*40+50)/100
	}

	switch {
	case p.ViolationCount > 0 || p.CreditScore < 40:
		p.RiskTier = SellerRiskTierRestricted
	case p.CompletedOrders < 5 || (p.OwnershipChanged && p.CompletedOrders < 10) || (p.EnvCreditCleared && p.CompletedOrders < 10):
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

// OnCharacterTransferred 在角色完成官方过户/换账号时主动清空新持有周期的信用档案（含环境基线与扣分计数）并标记换人重算。
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

// RecordSellerEnvironment 记录卖家一次可信登录/挂单环境（IP 与设备指纹）：
// - 首次采集只写入基线，不扣分不清零；
// - 单纯换 IP（如坐高铁回家、出差、切 5G/Wi-Fi，但设备未变）绝不影响信用；
// - 同 IP 下切换设备（如家里同 Wi-Fi 手机切电脑）或切回已登记的常用设备不影响信用；
// - 一旦同时命中「从未见过的陌生新设备」且「IP 也发生变化」（私下卖号/外地借号/盗号换机）：
//   直接将历史成交、好评、中评与信用分清 0（EnvCreditCleared=true, CreditScore=0, 1.0★, Restricted），
//   并将常用设备表重置为仅当前新设备；但保留原有 NegativeReviews 与 ViolationCount，严禁借异地换机洗白黑历史！
func (s *SellerCreditService) RecordSellerEnvironment(ctx context.Context, ev SellerEnvironmentEvent) (profile SellerCreditProfile, cleared bool, err error) {
	if s == nil || s.Store == nil {
		return SellerCreditProfile{}, false, errors.New("卖家信用服务未装配")
	}
	ip := normalizeEnvToken(ev.IP)
	device := normalizeEnvToken(ev.DeviceID)
	if ev.SellerPlayerID == 0 || ev.OwnerRevision == 0 || ev.OwnerAccount == "" || (ip == "" && device == "") {
		return SellerCreditProfile{}, false, ErrInvalid
	}
	profile, err = s.ensureEpochProfile(ctx, ev.SellerPlayerID, ev.OwnerAccount, ev.OwnerRevision, ev.ObservedAtMs)
	if err != nil {
		return SellerCreditProfile{}, false, err
	}
	ipChanged := ip != "" && profile.LastIP != "" && ip != profile.LastIP
	hasDeviceBaseline := len(profile.KnownDevices) > 0 || profile.LastDeviceID != ""
	isUnknownNewDevice := device != "" && hasDeviceBaseline && !containsString(profile.KnownDevices, device) && device != profile.LastDeviceID

	updated := false
	if ip != "" && profile.LastIP != ip {
		profile.LastIP = ip
		updated = true
	}
	// 命中「陌生新设备 + IP 同时变化」：私下卖号 / 外地借号 / 盗号换机 -> 直接清 0 正面信用与分数！
	if isUnknownNewDevice && ipChanged {
		profile.CompletedOrders = 0
		profile.PositiveReviews = 0
		profile.NeutralReviews = 0
		// 刻意不清空 NegativeReviews 与 ViolationCount，防止劣迹账号借异地换机洗白黑历史。
		profile.EnvCreditCleared = true
		profile.EnvResetCount++
		profile.LastEnvChangedAtMs = ev.ObservedAtMs
		profile.LastDeviceID = device
		profile.KnownDevices = []string{device}
		recomputeCreditMetrics(&profile)
		cleared = true
		updated = true
	} else if device != "" {
		if profile.LastDeviceID != device {
			profile.LastDeviceID = device
			updated = true
		}
		if !containsString(profile.KnownDevices, device) {
			profile.KnownDevices = rememberKnownDevice(profile.KnownDevices, device)
			updated = true
		}
	}
	if updated {
		if err := s.Store.SaveProfile(ctx, profile); err != nil {
			return SellerCreditProfile{}, false, err
		}
	}
	return profile, cleared, nil
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
// - 防刷分：买卖双方同 IP/同设备、或同一买家账号/IP/设备在 7 天窗口内超限、或低于最低计分金额、或处于换环境后好评冷静期时，一律留痕但不计分。
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
	sellerIP := normalizeEnvToken(t.SellerIP)
	if sellerIP == "" {
		sellerIP = profile.LastIP
	}
	sellerDev := normalizeEnvToken(t.SellerDeviceID)
	if sellerDev == "" {
		sellerDev = profile.LastDeviceID
	}
	buyerIP := normalizeEnvToken(t.BuyerIP)
	buyerDev := normalizeEnvToken(t.BuyerDeviceID)

	var windowStart uint64
	if nowMs > policy.PairScoreWindowMs {
		windowStart = nowMs - policy.PairScoreWindowMs
	}
	pairCount, err := s.Store.CountPairScoredReviews(ctx, key, buyerKey, buyerIP, buyerDev, windowStart, nowMs)
	if err != nil {
		return SellerReviewRecord{}, SellerCreditProfile{}, false, err
	}

	counted := true
	// 1) 买卖双方同 IP 或同设备互评：防自开小号或工作室同机刷分。
	if (sellerIP != "" && buyerIP != "" && sellerIP == buyerIP) ||
		(sellerDev != "" && buyerDev != "" && sellerDev == buyerDev) {
		counted = false
	}
	// 2) 低于最低计分金额的好评不涨分（防 1 银两刷五星）。
	if in.Rating == SellerRatingPositive && t.AmountMinor < policy.MinScoreAmountMinor {
		counted = false
	}
	// 3) 同一买家账号、同一买家 IP 或同一买家设备在 7 天窗口内超出计分次数上限。
	if pairCount >= policy.MaxScoredReviewsPerPairInWindow {
		counted = false
	}
	// 4) 卖家触发异地换机清 0 后进入防刷分冷静期时，短期突击好评不涨分。
	if in.Rating == SellerRatingPositive && policy.PostEnvChangeScoreCooldownMs > 0 &&
		profile.LastEnvChangedAtMs > 0 && nowMs >= profile.LastEnvChangedAtMs &&
		nowMs-profile.LastEnvChangedAtMs < policy.PostEnvChangeScoreCooldownMs {
		counted = false
	}

	rec := SellerReviewRecord{
		OrderID:             t.OrderID,
		SellerPlayerID:      t.SellerPlayerID,
		SellerOwnerRevision: t.SellerOwnerRevision,
		BuyerPlayerID:       t.BuyerPlayerID,
		BuyerAccount:        buyerKey,
		BuyerIP:             buyerIP,
		BuyerDeviceID:       buyerDev,
		Rating:              in.Rating,
		CountedForScore:     counted,
		Comment:             strings.TrimSpace(in.Comment),
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

func (m *MemorySellerCreditStore) CountPairScoredReviews(_ context.Context, key SellerCreditKey, buyerKey, buyerIP, buyerDeviceID string, sinceMs, nowMs uint64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, r := range m.reviews {
		if r.SellerPlayerID != key.SellerPlayerID || r.SellerOwnerRevision != key.OwnerRevision ||
			!r.CountedForScore || r.CreatedAtMs < sinceMs || r.CreatedAtMs > nowMs {
			continue
		}
		sameBuyer := r.BuyerAccount == buyerKey ||
			(buyerIP != "" && r.BuyerIP != "" && r.BuyerIP == buyerIP) ||
			(buyerDeviceID != "" && r.BuyerDeviceID != "" && r.BuyerDeviceID == buyerDeviceID)
		if sameBuyer {
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

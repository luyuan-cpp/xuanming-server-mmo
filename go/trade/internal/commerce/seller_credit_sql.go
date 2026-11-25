package commerce

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	tradepb "proto/trade"
)

// SQLSellerCreditStore 是 SellerCreditStore 基于 mmorpg_trade 独占库的参数化 SQL 实现。
// 建表由 schemamigrate 读取 proto/trade/trade_table.proto 完成，本实现不含任何 DDL。
type SQLSellerCreditStore struct {
	DB  *sql.DB
	IDs IDs
	Now func() time.Time
}

func (s *SQLSellerCreditStore) nowMs() uint64 {
	if s != nil && s.Now != nil {
		return uint64(s.Now().UnixMilli())
	}
	return uint64(time.Now().UnixMilli())
}

const sellerCreditColumns = "seller_player_id,owner_revision,owner_account,ownership_changed,ownership_changed_at_ms," +
	"epoch_started_at_ms,completed_orders,positive_reviews,neutral_reviews,negative_reviews,violation_count," +
	"env_credit_cleared,env_reset_count,last_env_changed_at_ms,last_ip,last_device_id,known_devices_json," +
	"credit_score,star_rating_tenths,positive_rate_bps,risk_tier,updated_ms"

func (s *SQLSellerCreditStore) GetProfile(ctx context.Context, key SellerCreditKey) (*SellerCreditProfile, bool, error) {
	if s == nil || s.DB == nil {
		return nil, false, errors.New("卖家信用 SQL 存储未装配")
	}
	rec := &tradepb.TradeSellerCreditRecord{}
	err := s.DB.QueryRowContext(ctx,
		"SELECT "+sellerCreditColumns+" FROM trade_seller_credit WHERE seller_player_id=? AND owner_revision=?",
		key.SellerPlayerID, key.OwnerRevision,
	).Scan(
		&rec.SellerPlayerId, &rec.OwnerRevision, &rec.OwnerAccount, &rec.OwnershipChanged, &rec.OwnershipChangedAtMs,
		&rec.EpochStartedAtMs, &rec.CompletedOrders, &rec.PositiveReviews, &rec.NeutralReviews, &rec.NegativeReviews, &rec.ViolationCount,
		&rec.EnvCreditCleared, &rec.EnvResetCount, &rec.LastEnvChangedAtMs, &rec.LastIp, &rec.LastDeviceId, &rec.KnownDevicesJson,
		&rec.CreditScore, &rec.StarRatingTenths, &rec.PositiveRateBps, &rec.RiskTier, &rec.UpdatedMs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var known []string
	if rec.KnownDevicesJson != "" {
		if unmarshalErr := json.Unmarshal([]byte(rec.KnownDevicesJson), &known); unmarshalErr != nil {
			known = nil
		}
	}
	p := SellerCreditProfile{
		SellerPlayerID:       rec.SellerPlayerId,
		OwnerAccount:         rec.OwnerAccount,
		OwnerRevision:        rec.OwnerRevision,
		OwnershipChanged:     rec.OwnershipChanged,
		OwnershipChangedAtMs: rec.OwnershipChangedAtMs,
		EpochStartedAtMs:     rec.EpochStartedAtMs,
		CompletedOrders:      rec.CompletedOrders,
		PositiveReviews:      rec.PositiveReviews,
		NeutralReviews:       rec.NeutralReviews,
		NegativeReviews:      rec.NegativeReviews,
		ViolationCount:       rec.ViolationCount,
		EnvCreditCleared:     rec.EnvCreditCleared,
		EnvResetCount:        rec.EnvResetCount,
		LastEnvChangedAtMs:   rec.LastEnvChangedAtMs,
		LastIP:               rec.LastIp,
		LastDeviceID:         rec.LastDeviceId,
		KnownDevices:         known,
		CreditScore:          rec.CreditScore,
		StarRatingTenths:     rec.StarRatingTenths,
		PositiveRateBps:      rec.PositiveRateBps,
		RiskTier:             SellerRiskTier(rec.RiskTier),
	}
	return &p, true, nil
}

func (s *SQLSellerCreditStore) HasPriorEpoch(ctx context.Context, sellerPlayerID, currentRevision uint64) (bool, error) {
	if s == nil || s.DB == nil {
		return false, errors.New("卖家信用 SQL 存储未装配")
	}
	var dummy int
	err := s.DB.QueryRowContext(ctx,
		"SELECT 1 FROM trade_seller_credit WHERE seller_player_id=? AND owner_revision<? LIMIT 1",
		sellerPlayerID, currentRevision,
	).Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *SQLSellerCreditStore) SaveProfile(ctx context.Context, p SellerCreditProfile) error {
	if s == nil || s.DB == nil {
		return errors.New("卖家信用 SQL 存储未装配")
	}
	knownJSON := "[]"
	if len(p.KnownDevices) > 0 {
		raw, err := json.Marshal(p.KnownDevices)
		if err != nil {
			return err
		}
		knownJSON = string(raw)
	}
	now := s.nowMs()
	_, err := s.DB.ExecContext(ctx,
		"INSERT INTO trade_seller_credit ("+sellerCreditColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) "+
			"ON DUPLICATE KEY UPDATE owner_account=VALUES(owner_account),ownership_changed=VALUES(ownership_changed),"+
			"ownership_changed_at_ms=VALUES(ownership_changed_at_ms),epoch_started_at_ms=VALUES(epoch_started_at_ms),"+
			"completed_orders=VALUES(completed_orders),positive_reviews=VALUES(positive_reviews),neutral_reviews=VALUES(neutral_reviews),"+
			"negative_reviews=VALUES(negative_reviews),violation_count=VALUES(violation_count),env_credit_cleared=VALUES(env_credit_cleared),"+
			"env_reset_count=VALUES(env_reset_count),last_env_changed_at_ms=VALUES(last_env_changed_at_ms),last_ip=VALUES(last_ip),"+
			"last_device_id=VALUES(last_device_id),known_devices_json=VALUES(known_devices_json),credit_score=VALUES(credit_score),"+
			"star_rating_tenths=VALUES(star_rating_tenths),positive_rate_bps=VALUES(positive_rate_bps),risk_tier=VALUES(risk_tier),updated_ms=VALUES(updated_ms)",
		p.SellerPlayerID, p.OwnerRevision, p.OwnerAccount, p.OwnershipChanged, p.OwnershipChangedAtMs,
		p.EpochStartedAtMs, p.CompletedOrders, p.PositiveReviews, p.NeutralReviews, p.NegativeReviews, p.ViolationCount,
		p.EnvCreditCleared, p.EnvResetCount, p.LastEnvChangedAtMs, p.LastIP, p.LastDeviceID, knownJSON,
		p.CreditScore, p.StarRatingTenths, p.PositiveRateBps, string(p.RiskTier), now,
	)
	return err
}

func (s *SQLSellerCreditStore) MarkOrderCompleted(ctx context.Context, orderID string, key SellerCreditKey) (bool, error) {
	if s == nil || s.DB == nil || s.IDs == nil {
		return false, errors.New("卖家信用 SQL 存储或发号器未装配")
	}
	id, err := s.IDs.Next(ctx)
	if err != nil {
		return false, err
	}
	res, err := s.DB.ExecContext(ctx,
		"INSERT IGNORE INTO trade_seller_completed_order (record_id,order_id,seller_player_id,owner_revision,created_ms) VALUES (?,?,?,?,?)",
		id, orderID, key.SellerPlayerID, key.OwnerRevision, s.nowMs(),
	)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (s *SQLSellerCreditStore) GetReviewByOrder(ctx context.Context, orderID string) (*SellerReviewRecord, bool, error) {
	if s == nil || s.DB == nil {
		return nil, false, errors.New("卖家信用 SQL 存储未装配")
	}
	rec := &tradepb.TradeSellerReviewRecord{}
	err := s.DB.QueryRowContext(ctx,
		"SELECT order_id,seller_player_id,seller_owner_revision,buyer_player_id,buyer_account,buyer_ip,buyer_device_id,rating,counted_for_score,created_ms "+
			"FROM trade_seller_review WHERE order_id=?",
		orderID,
	).Scan(
		&rec.OrderId, &rec.SellerPlayerId, &rec.SellerOwnerRevision, &rec.BuyerPlayerId,
		&rec.BuyerAccount, &rec.BuyerIp, &rec.BuyerDeviceId, &rec.Rating, &rec.CountedForScore, &rec.CreatedMs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &SellerReviewRecord{
		OrderID:             rec.OrderId,
		SellerPlayerID:      rec.SellerPlayerId,
		SellerOwnerRevision: rec.SellerOwnerRevision,
		BuyerPlayerID:       rec.BuyerPlayerId,
		BuyerAccount:        rec.BuyerAccount,
		BuyerIP:             rec.BuyerIp,
		BuyerDeviceID:       rec.BuyerDeviceId,
		Rating:              SellerReviewRating(rec.Rating),
		CountedForScore:     rec.CountedForScore,
		CreatedAtMs:         rec.CreatedMs,
	}, true, nil
}

func (s *SQLSellerCreditStore) CountPairScoredReviews(ctx context.Context, key SellerCreditKey, buyerKey, buyerIP, buyerDeviceID string, sinceMs, nowMs uint64) (int, error) {
	if s == nil || s.DB == nil {
		return 0, errors.New("卖家信用 SQL 存储未装配")
	}
	var count int
	err := s.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM trade_seller_review "+
			"WHERE seller_player_id=? AND seller_owner_revision=? AND counted_for_score=1 AND created_ms>=? AND created_ms<=? "+
			"AND (buyer_account=? OR (?<>'' AND buyer_ip=?) OR (?<>'' AND buyer_device_id=?))",
		key.SellerPlayerID, key.OwnerRevision, sinceMs, nowMs,
		buyerKey, buyerIP, buyerIP, buyerDeviceID, buyerDeviceID,
	).Scan(&count)
	return count, err
}

func (s *SQLSellerCreditStore) SaveReview(ctx context.Context, rec SellerReviewRecord) error {
	if s == nil || s.DB == nil || s.IDs == nil {
		return errors.New("卖家信用 SQL 存储或发号器未装配")
	}
	id, err := s.IDs.Next(ctx)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx,
		"INSERT INTO trade_seller_review (review_id,order_id,seller_player_id,seller_owner_revision,buyer_player_id,buyer_account,buyer_ip,buyer_device_id,rating,counted_for_score,comment,created_ms) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		id, rec.OrderID, rec.SellerPlayerID, rec.SellerOwnerRevision, rec.BuyerPlayerID,
		rec.BuyerAccount, rec.BuyerIP, rec.BuyerDeviceID, uint32(rec.Rating), rec.CountedForScore, rec.Comment, rec.CreatedAtMs,
	)
	return err
}

func (s *SQLSellerCreditStore) HasViolation(ctx context.Context, caseID string) (bool, error) {
	if s == nil || s.DB == nil {
		return false, errors.New("卖家信用 SQL 存储未装配")
	}
	var dummy int
	err := s.DB.QueryRowContext(ctx, "SELECT 1 FROM trade_seller_violation WHERE case_id=? LIMIT 1", caseID).Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *SQLSellerCreditStore) SaveViolation(ctx context.Context, rec SellerViolationRecord) error {
	if s == nil || s.DB == nil || s.IDs == nil {
		return errors.New("卖家信用 SQL 存储或发号器未装配")
	}
	id, err := s.IDs.Next(ctx)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx,
		"INSERT INTO trade_seller_violation (violation_id,case_id,order_id,seller_player_id,seller_owner_revision,seller_account,reason,recorded_ms) "+
			"VALUES (?,?,?,?,?,?,?,?)",
		id, rec.CaseID, rec.OrderID, rec.SellerPlayerID, rec.SellerOwnerRevision, rec.SellerAccount, rec.Reason, rec.RecordedAtMs,
	)
	return err
}

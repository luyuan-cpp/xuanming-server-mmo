package commerce

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	assetpb "proto/common/asset"
	tradepb "proto/trade"
	"trade/internal/data"
)

// 价格时间优先只由SQL中的已验证托管订单裁决。Redis深度缓存不是撮合证据。
// 一次事务预留双边数量+写成交交付意图；RPC全在提交后。余额从不进本库。
func (e *Engine) matchMarket(ctx context.Context, id uint64) error {
	taker, err := e.Repo.Get(ctx, id)
	if err != nil {
		return err
	}
	if taker.Kind != 4 || taker.Side == 0 || taker.FilledQuantity == taker.Quantity {
		return nil
	}
	matchID, err := e.IDs.Next(ctx)
	if err != nil {
		return err
	}
	observedID, err := marketCounterparty(ctx, e.Repo.DB, taker)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	observed, err := e.Repo.Get(ctx, observedID)
	if err != nil {
		return err
	}
	// 本事务把坏对手单移出盘口时记下原因与原 stage,提交成功之后才计数、记日志(事务可能回滚重跑)。
	var defect error
	var defectStage string
	err = e.Repo.Tx(ctx, func(tx *sql.Tx) error {
		defect, defectStage = nil, ""
		if err := e.lockRoles(ctx, tx, 0, taker.SellerPlayerId, observed.SellerPlayerId); err != nil {
			return err
		}
		key := sha256.Sum256([]byte(fmt.Sprintf("market:%d:%d:%d", taker.MarketId, taker.ItemConfigId, taker.CurrencyType)))
		guardID := binary.BigEndian.Uint64(key[:8]) | 1<<63
		if _, err := tx.ExecContext(ctx, "INSERT IGNORE INTO trade_commerce_guard(guard_id) VALUES(?)", guardID); err != nil {
			return err
		}
		var guard uint64
		if err := tx.QueryRowContext(ctx, "SELECT guard_id FROM trade_commerce_guard WHERE guard_id=? FOR UPDATE", guardID).Scan(&guard); err != nil {
			return err
		}
		otherID, err := marketCounterparty(ctx, tx, taker)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if otherID != observedID {
			return staleObservation()
		} // 价格时间最优项变化时重新规划，不拿未锁角色的新候选成交(乐观冲突,下一轮立即重撮)。
		first, second := id, otherID
		if first > second {
			first, second = second, first
		}
		a, err := e.Repo.Lock(ctx, tx, first)
		if err != nil {
			return err
		}
		b, err := e.Repo.Lock(ctx, tx, second)
		if err != nil {
			return err
		}
		// 对手单自己不可能再成交(workflow 解不开、缺原归属身份、原主已换)时只把它移出盘口,taker 不报错(2026-10-22):
		// 它停在最优价位上会挡住这个物品的整个盘口,而 taker 自己没有任何问题。以前这里返回的是 taker 的错误,
		// taker 每轮报一次 ERROR,坏单永远留在盘口上。隔离在本事务里做(已持有它的行锁),下一轮按新的最优价重撮。
		other := a
		if b.CommerceId == otherID {
			other = b
		}
		found, err := e.counterpartyDefect(ctx, tx, other)
		if err != nil {
			return err
		}
		if found != nil {
			stage, err := e.quarantineLockedTx(ctx, tx, other)
			if err != nil {
				return err
			}
			defect, defectStage = found, stage
			return nil
		}
		aw, err := data.Workflow(a)
		if err != nil {
			return err
		}
		bw, err := data.Workflow(b)
		if err != nil {
			return err
		}
		for _, pair := range []struct {
			r *tradepb.TradeCommerceRecord
			w *tradepb.TradeCommerceWorkflow
		}{{a, aw}, {b, bw}} {
			if err := e.checkContractIdentities(ctx, tx, pair.r, pair.w); err != nil {
				return err
			}
		}
		if aw.Stage != "market_open" || bw.Stage != "market_open" || aw.CancelRequested || bw.CancelRequested || a.FilledQuantity >= a.Quantity || b.FilledQuantity >= b.Quantity {
			return nil
		}
		buy, sell := a, b
		if buy.Side != 1 {
			buy, sell = sell, buy
		}
		if buy.UnitPrice < sell.UnitPrice || buy.SellerPlayerId == sell.SellerPlayerId {
			return nil
		}
		maker := a
		if b.CreatedMs < a.CreatedMs || b.CreatedMs == a.CreatedMs && b.CommerceId < a.CommerceId {
			maker = b
		}
		price := maker.UnitPrice
		qty := min(buy.Quantity-buy.FilledQuantity, sell.Quantity-sell.FilledQuantity)
		now := e.now()
		m := &tradepb.TradeCommerceRecord{CommerceId: matchID, RequestKey: hex.EncodeToString(key[:]) + ":" + fmt.Sprint(matchID), Kind: 4, Status: Delivering, SellerPlayerId: sell.SellerPlayerId, BuyerPlayerId: buy.SellerPlayerId, MarketId: buy.MarketId, ItemConfigId: buy.ItemConfigId, UnitPrice: price, Quantity: qty, CurrencyType: buy.CurrencyType, CreatedMs: now, UpdatedMs: now, Version: 1, ParentId: buy.CommerceId, CounterpartyId: sell.CommerceId}
		mw := &tradepb.TradeCommerceWorkflow{Stage: "deliver"}
		buyWorkflow, sellWorkflow := aw, bw
		if buy.CommerceId == b.CommerceId {
			buyWorkflow, sellWorkflow = bw, aw
		}
		if buyWorkflow.SellerIdentity == nil || sellWorkflow.SellerIdentity == nil {
			return corrupt(errors.New("撮合父合同缺少原角色身份"))
		}
		mw.SellerIdentity = proto.Clone(sellWorkflow.SellerIdentity).(*tradepb.TradeRoleIdentity)
		mw.BuyerIdentity = proto.Clone(buyWorkflow.SellerIdentity).(*tradepb.TradeRoleIdentity)
		addLeg(mw, "buyer_receive", buy.SellerPlayerId, 4, &assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: buy.ItemConfigId, Count: qty}}})
		addLeg(mw, "seller_receive", sell.SellerPlayerId, 4, &assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: buy.CurrencyType, Amount: uint64(qty) * price}}})
		if buy.UnitPrice > price {
			addLeg(mw, "buyer_surplus", buy.SellerPlayerId, 4, &assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: buy.CurrencyType, Amount: uint64(qty) * (buy.UnitPrice - price)}}})
		}
		m.WorkflowProto, err = proto.MarshalOptions{Deterministic: true}.Marshal(mw)
		if err != nil {
			return err
		}
		if err := e.Repo.Insert(ctx, tx, m); err != nil {
			return err
		}
		a.FilledQuantity += qty
		b.FilledQuantity += qty
		if a.FilledQuantity == a.Quantity {
			a.Status = Delivering
		}
		if b.FilledQuantity == b.Quantity {
			b.Status = Delivering
		}
		if err := e.Repo.Save(ctx, tx, a, aw, now); err != nil {
			return err
		}
		return e.Repo.Save(ctx, tx, b, bw, now)
	})
	if err == nil && defect != nil {
		quarantinedTotal.Inc()
		logx.Errorf("[Commerce] Market counterparty quarantined as manual and taken off the book counterparty_id=%d taker_id=%d stage=%q: %v",
			observedID, id, defectStage, defect)
	}
	return err
}

// counterpartyDefect 判断已锁住的对手单是否因它自己的持久数据而不可能再成交:workflow 解不开、缺原归属身份,
// 或原归属角色已换主(owner_revision 只增不减,不会再对上)。返回的 defect 非 nil 即应移出盘口;
// 存储 / 目录读取失败经 err 原样返回,不能当成对手单坏了。
// 角色被另一条挂单冻结(lockRoles 阶段的 role_conflict)不在此列:那是暂时状态,而且分不清是哪一方被冻结。
func (e *Engine) counterpartyDefect(ctx context.Context, tx *sql.Tx, r *tradepb.TradeCommerceRecord) (defect error, err error) {
	w, decodeErr := data.Workflow(r)
	if decodeErr != nil {
		return corrupt(decodeErr), nil
	}
	switch checkErr := e.checkContractIdentities(ctx, tx, r, w); {
	case checkErr == nil:
		return nil, nil
	case errors.Is(checkErr, errCorruptContract), errors.Is(checkErr, errRoleConflict):
		return checkErr, nil
	default:
		return nil, checkErr
	}
}

type marketQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func marketCounterparty(ctx context.Context, q marketQuery, taker *tradepb.TradeCommerceRecord) (uint64, error) {
	filter, order := "unit_price>=?", "unit_price DESC,created_ms,commerce_id"
	opposite := uint32(1)
	if taker.Side == 1 {
		filter = "unit_price<=?"
		order = "unit_price ASC,created_ms,commerce_id"
		opposite = 2
	}
	var id uint64
	err := q.QueryRowContext(ctx, "SELECT commerce_id FROM trade_commerce WHERE kind=4 AND side=? AND market_id=? AND item_config_id=? AND currency_type=? AND status=3 AND filled_quantity<quantity AND seller_player_id<>? AND "+filter+" ORDER BY "+order+" LIMIT 1", opposite, taker.MarketId, taker.ItemConfigId, taker.CurrencyType, taker.SellerPlayerId, taker.UnitPrice).Scan(&id)
	return id, err
}

func (e *Engine) marketBarrier(ctx context.Context, tx *sql.Tx, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) error {
	if r.Kind != 4 || r.Side == 0 {
		return nil
	}
	var pending, bad int
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(status NOT IN (7,11)),0),COALESCE(SUM(status=10),0) FROM trade_commerce WHERE kind=4 AND side=0 AND (parent_id=? OR counterparty_id=?)", r.CommerceId, r.CommerceId).Scan(&pending, &bad); err != nil {
		return err
	}
	if bad > 0 {
		r.Status = Manual
		w.Stage = "manual"
		return nil
	}
	if w.Stage == "done" && pending > 0 {
		w.Stage = "market_wait"
		r.Status = CancelPending
	}
	if w.Stage == "market_wait" && pending == 0 {
		w.Stage = "done"
		r.Status = Cancelled
	}
	if w.Stage == "market_open" && r.Quantity == r.FilledQuantity && pending == 0 {
		w.Stage = "done"
		r.Status = Settled
	}
	return nil
}

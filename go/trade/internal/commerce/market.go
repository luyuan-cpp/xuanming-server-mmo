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
//
// 「已验证托管」在 SQL 里是 on_book=1(data.OnBook,stage=market_open 的投影):托管阶段的订单 status 也是 3,
// 不按它过滤就会被选成最优对手单、挡住整本盘口(jubaozhai-market.md §15.9)。两边是否在盘口上仍在锁内按 workflow 复核。
func (e *Engine) matchMarket(ctx context.Context, id uint64) error {
	taker, err := e.Repo.Get(ctx, id)
	if err != nil {
		return err
	}
	// 托管还没完成的 taker 不发起撮合:不发成交号、不抢盘口 guard 行锁。它进入 market_open 的那一步 step 会再撮一次。
	if taker.Kind != 4 || taker.Side == 0 || !taker.OnBook || taker.FilledQuantity == taker.Quantity {
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
	err = e.roleTx(ctx, func(tx *sql.Tx) error {
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
			stage, err := e.quarantineLockedTx(ctx, tx, other, manualReasonBadCounterparty, found.Error())
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
		// 提交点复核:选单的 on_book 与 taker 预检只是预选,两边是否还在盘口上以锁内读到的 workflow 为准(AGENTS §11.3)。
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
	// on_book=1:只选托管已完成、挂在盘口上的订单。预读与锁内重读都走这里,同一个条件。
	err := q.QueryRowContext(ctx, "SELECT commerce_id FROM trade_commerce WHERE kind=4 AND side=? AND market_id=? AND item_config_id=? AND currency_type=? AND status=3 AND on_book=1 AND filled_quantity<quantity AND seller_player_id<>? AND "+filter+" ORDER BY "+order+" LIMIT 1", opposite, taker.MarketId, taker.ItemConfigId, taker.CurrencyType, taker.SellerPlayerId, taker.UnitPrice).Scan(&id)
	return id, err
}

func (e *Engine) marketBarrier(ctx context.Context, tx *sql.Tx, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) error {
	if r.Kind != 4 || r.Side == 0 {
		return nil
	}
	// 转人工的子单(status=10)算没收尾:母单等它被人工处理完再结案,不跟着转人工(2026-11-11,jubaozhai-market.md §15.7.1 第 4 类)。
	// 子单的资产在撮合时已从两边母单的托管里划出,由子单自己的腿交付;母单只对剩下的部分负责,剩余挂单量照常成交或撤单。
	// 以前这里把母单也转人工,一张子单出事就冻住母单剩下的挂单量,玩家撤不了单。
	var pending int
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(status NOT IN (7,11)),0) FROM trade_commerce WHERE kind=4 AND side=0 AND (parent_id=? OR counterparty_id=?)", r.CommerceId, r.CommerceId).Scan(&pending); err != nil {
		return err
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

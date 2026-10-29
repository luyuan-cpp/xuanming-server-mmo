package commerce

import (
	"context"
	"database/sql"
	"errors"
	"math"
	tradepb "proto/trade"
	"trade/internal/data"

	"github.com/zeromicro/go-zero/core/logx"
)

func (e *Engine) PlaceBid(ctx context.Context, me, lotID uint64, in *tradepb.PlaceAuctionBidRequest) (*tradepb.TradeCommerceRecord, error) {
	lot, err := e.Repo.Get(ctx, lotID)
	if err != nil {
		return nil, err
	}
	// 先取拍品观察，再查原请求：若并发 worker 已据该出价推进最高价，合同必已提交，不能按下一次加价门槛拒绝重放。
	if old, err := e.priorRequest(ctx, me, in.GetClientRequestId(), in); err != nil || old != nil {
		return old, err
	}
	w, err := data.Workflow(lot)
	if err != nil {
		return nil, err
	}
	if lot.Kind != 5 || w.Stage != "lot_open" || lot.ExpiresMs <= e.now() {
		return nil, ErrEnded
	}
	if me == lot.SellerPlayerId {
		return nil, ErrSelf
	}
	if in.Amount < lot.UnitPrice || in.Amount > math.MaxInt64 || w.HighestBid > 0 && (w.MinimumIncrement > math.MaxInt64-w.HighestBid || in.Amount < w.HighestBid+w.MinimumIncrement) {
		return nil, ErrBid
	}
	return e.Create(ctx, me, in.ClientRequestId, in, &tradepb.TradeCommerceRecord{Kind: BidKind, Status: Escrowing, SellerPlayerId: me, ParentId: lotID, UnitPrice: in.Amount, CurrencyType: lot.CurrencyType, ExpiresMs: lot.ExpiresMs}, &tradepb.TradeCommerceWorkflow{SellerAssets: &tradepb.TradeAssetSelection{CurrencyType: lot.CurrencyType, CurrencyAmount: in.Amount}, Stage: "bid_freeze"})
}

// 每个lot的guard先于任何合同锁。冻结完成只获得参选资格，提交最高价时再次检查结束时间/增幅。
// 多副本/迟到结果输掉竞价时写原币退款待办，不偷换或取消已发出的扣币seq。
func (e *Engine) acceptBid(ctx context.Context, bidID uint64) error {
	candidate, err := e.Repo.Get(ctx, bidID)
	if err != nil {
		return err
	}
	parent, parentErr := e.Repo.Get(ctx, candidate.ParentId)
	if parentErr != nil && !errors.Is(parentErr, sql.ErrNoRows) {
		return parentErr
	}
	parentSeller, parentBuyer := uint64(0), uint64(0)
	if parentErr == nil {
		parentSeller, parentBuyer = parent.SellerPlayerId, parent.BuyerPlayerId
	}
	return e.Repo.Tx(ctx, func(tx *sql.Tx) error {
		if err := e.lockRoles(ctx, tx, 0, candidate.SellerPlayerId, parentSeller, parentBuyer); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT IGNORE INTO trade_commerce_guard(guard_id) VALUES(?)", candidate.ParentId); err != nil {
			return err
		}
		var guard uint64
		if err := tx.QueryRowContext(ctx, "SELECT guard_id FROM trade_commerce_guard WHERE guard_id=? FOR UPDATE", candidate.ParentId).Scan(&guard); err != nil {
			return err
		}
		// 拍品行不存在或 workflow 解不开时这条出价按"不合格"退款,不整轮报错(2026-10-20,第一轮审计 §2.5 go-asset-services#0):
		// bid_ready 的出价还没被拍品接受(接受与 WinningBidId 在同一事务里写),拍品不引用它,原币退回只动出价人自己的冻结资金;
		// 以前这里直接返回错误,拍品坏了的话这条出价每轮重试、冻结的钱永远退不回来。拍品自己由它那一行的推进隔离为人工。
		lot, err := e.Repo.Lock(ctx, tx, candidate.ParentId)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var lw *tradepb.TradeCommerceWorkflow
		lotDefect := "lot missing"
		if err == nil {
			if lot.SellerPlayerId != parentSeller || lot.BuyerPlayerId != parentBuyer {
				// 预读拍品之后别的副本接受了另一条出价:乐观冲突,下一轮重读即可。不能按暂时失败推迟 10 秒 ——
				// 临近截止时那会把这条已冻结的有效出价拖过截止、变成退款,改变拍卖结果(2026-10-22)。
				return staleObservation()
			}
			if lw, err = data.Workflow(lot); err != nil {
				lw, lotDefect = nil, "lot workflow undecodable"
			}
		}
		bid, err := e.Repo.Lock(ctx, tx, bidID)
		if err != nil {
			return err
		}
		bw, err := data.Workflow(bid)
		if err != nil {
			return corrupt(err)
		}
		if bw.Stage != "bid_ready" {
			return nil
		}
		if err := e.checkContractIdentities(ctx, tx, bid, bw); err != nil {
			return err
		}
		if lw != nil {
			// 拍品自己的原归属身份不合格(缺身份的存量数据、原主已换)时它不可能接受这条出价:与拍品缺失同样按不合格退款,
			// 只动出价人自己的冻结资金;只有出价自身的数据坏了才算这条出价 corrupt(2026-10-22)。
			// 以前这里原样返回,拍品缺身份时被隔离为人工的是这条出价,冻结的钱一起进了人工单。
			// 存储 / 目录读取失败照常返回,下一轮重试,不能当成拍品坏了。
			switch checkErr := e.checkContractIdentities(ctx, tx, lot, lw); {
			case checkErr == nil:
			case errors.Is(checkErr, errCorruptContract), errors.Is(checkErr, errRoleConflict):
				lw, lotDefect = nil, "lot identity check failed: "+checkErr.Error()
			default:
				return checkErr
			}
		}
		if lw == nil {
			logx.Errorf("[Commerce] Bid refunded because its lot cannot accept bids bid_id=%d lot_id=%d reason=%s", bid.CommerceId, candidate.ParentId, lotDefect)
		}
		var now uint64
		if err := tx.QueryRowContext(ctx, "SELECT CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)").Scan(&now); err != nil {
			return err
		}
		eligible := lw != nil && lw.Stage == "lot_open" && !lw.CancelRequested && lot.ExpiresMs > now && bid.UnitPrice >= lot.UnitPrice && (lw.HighestBid == 0 || lw.MinimumIncrement <= math.MaxInt64-lw.HighestBid && bid.UnitPrice >= lw.HighestBid+lw.MinimumIncrement)
		// 截止判定用库时钟(多副本同一把尺);updated_ms 用引擎时钟,与其它 Save 一致 —— 它是 Due 的调度键,
		// Due 按引擎时钟取到期行,库时钟领先时用它写 updated_ms 会让刚改好的行晚到期(2026-10-20)。
		saveMs := e.now()
		if !eligible {
			bw.Stage = "bid_refund"
			bid.Status = CancelPending
			return e.Repo.Save(ctx, tx, bid, bw, saveMs)
		}
		// 原最高价出价读不到 / 不在 bid_held 是拍品自相矛盾,不是这条出价坏了,所以不标 corrupt(那会把这条出价连同冻结资金隔离成人工):
		// 按暂时失败推迟,拍品截止后这条出价不再 eligible,走上面的退款分支自动退回。
		if lw.WinningBidId != 0 {
			old, err := e.Repo.Lock(ctx, tx, lw.WinningBidId)
			if err != nil {
				return err
			}
			ow, err := data.Workflow(old)
			if err != nil {
				return err
			}
			if ow.Stage != "bid_held" {
				return errors.New("最高竞价托管状态损坏")
			}
			ow.Stage = "bid_refund"
			old.Status = CancelPending
			if err := e.Repo.Save(ctx, tx, old, ow, saveMs); err != nil {
				return err
			}
		}
		lw.HighestBid = bid.UnitPrice
		lw.HighestBidder = bid.SellerPlayerId
		lw.WinningBidId = bid.CommerceId
		lw.BuyerAssets = bw.SellerAssets
		lot.BuyerPlayerId = bid.SellerPlayerId
		if bw.SellerIdentity == nil {
			return corrupt(errors.New("原竞价合同缺少买家归属"))
		}
		lw.BuyerIdentity = bw.SellerIdentity
		bw.Stage = "bid_held"
		if err := e.Repo.Save(ctx, tx, lot, lw, saveMs); err != nil {
			return err
		}
		return e.Repo.Save(ctx, tx, bid, bw, saveMs)
	})
}

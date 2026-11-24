package commerce

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sort"

	tradepb "proto/trade"
	"trade/internal/data"
)

const (
	// DefaultReferenceWindowMs 是近 7 日真实成交参考价的默认统计窗口（毫秒）。
	DefaultReferenceWindowMs uint64 = 7 * 24 * 3600 * 1000
	// DefaultReferenceMinSamples 是给出中位参考价所需的最小有效成交笔数；不足则明确标记样本不足，防止黑商拿小样本伪造行情。
	DefaultReferenceMinSamples = 10
	maxReferenceScanRows       = 5000
)

// MarketFillSample 是一笔真实已结案市场成交的最小观测样本。
type MarketFillSample struct {
	ItemConfigID  uint32
	CurrencyType  uint32
	UnitPrice     uint64
	Quantity      uint32
	SellerAccount string
	BuyerAccount  string
	SettledAtMs   uint64
}

// ReferencePrice 是服务端基于真实成交计算的近窗中位成交参考价（p50）。
type ReferencePrice struct {
	ItemConfigID       uint32
	CurrencyType       uint32
	MedianUnitPrice    uint64
	SampleCount        int
	InsufficientSample bool
}

// ReferencePriceReader 提供某标准品在指定币种下的近期真实中位成交价查询。
type ReferencePriceReader interface {
	ReferencePrice(ctx context.Context, itemConfigID, currencyType uint32, nowMs uint64) (ReferencePrice, error)
}

// ComputeReferencePrice 从成交样本中剔除同账号对敲、越界与无效样本，计算真实成交单价的 p50 中位数。
// 样本笔数低于 minSamples 时不返回虚假行情，统一置 InsufficientSample=true 且 MedianUnitPrice=0。
func ComputeReferencePrice(samples []MarketFillSample, itemConfigID, currencyType uint32, nowMs, windowMs uint64, minSamples int) ReferencePrice {
	if windowMs == 0 {
		windowMs = DefaultReferenceWindowMs
	}
	if minSamples <= 0 {
		minSamples = DefaultReferenceMinSamples
	}
	var floorMs uint64
	if nowMs > windowMs {
		floorMs = nowMs - windowMs
	}
	prices := make([]uint64, 0, len(samples))
	for _, s := range samples {
		if s.ItemConfigID != itemConfigID || s.CurrencyType != currencyType {
			continue
		}
		if s.UnitPrice == 0 || s.Quantity == 0 {
			continue
		}
		if s.SettledAtMs < floorMs || s.SettledAtMs > nowMs {
			continue
		}
		// 剔除同账号自成交/对敲刷行情样本。
		if s.SellerAccount != "" && s.SellerAccount == s.BuyerAccount {
			continue
		}
		prices = append(prices, s.UnitPrice)
	}
	out := ReferencePrice{
		ItemConfigID: itemConfigID,
		CurrencyType: currencyType,
		SampleCount:  len(prices),
	}
	if len(prices) < minSamples {
		out.InsufficientSample = true
		return out
	}
	sort.Slice(prices, func(i, j int) bool { return prices[i] < prices[j] })
	n := len(prices)
	if n%2 == 1 {
		out.MedianUnitPrice = prices[n/2]
	} else {
		low, high := prices[n/2-1], prices[n/2]
		out.MedianUnitPrice = low + (high-low)/2
	}
	return out
}

// SQLReferencePriceReader 直接基于 mmorpg_trade.trade_commerce 中已结案的市场撮合子单（kind=4, side=0, status=11）
// 计算真实中位成交价，复用单一成交事实源，不额外引入缓存假行情。
type SQLReferencePriceReader struct {
	DB         *sql.DB
	WindowMs   uint64
	MinSamples int
}

func (r *SQLReferencePriceReader) ReferencePrice(ctx context.Context, itemConfigID, currencyType uint32, nowMs uint64) (ReferencePrice, error) {
	if r == nil || r.DB == nil {
		return ReferencePrice{ItemConfigID: itemConfigID, CurrencyType: currencyType, InsufficientSample: true}, errors.New("参考价存储未装配")
	}
	windowMs := r.WindowMs
	if windowMs == 0 {
		windowMs = DefaultReferenceWindowMs
	}
	var floorMs uint64
	if nowMs > windowMs {
		floorMs = nowMs - windowMs
	}
	rows, err := r.DB.QueryContext(ctx,
		"SELECT unit_price,quantity,updated_ms,workflow_proto FROM trade_commerce "+
			"WHERE kind=4 AND side=0 AND status=? AND item_config_id=? AND currency_type=? AND updated_ms>=? AND updated_ms<=? "+
			"ORDER BY updated_ms DESC,commerce_id DESC LIMIT ?",
		Settled, itemConfigID, currencyType, floorMs, nowMs, maxReferenceScanRows)
	if err != nil {
		return ReferencePrice{}, err
	}
	defer rows.Close()

	samples := make([]MarketFillSample, 0, 64)
	for rows.Next() {
		var unitPrice, updatedMs uint64
		var qty uint32
		var rawWorkflow []byte
		if err := rows.Scan(&unitPrice, &qty, &updatedMs, &rawWorkflow); err != nil {
			return ReferencePrice{}, err
		}
		var sellerAcct, buyerAcct string
		if len(rawWorkflow) > 0 {
			w, decodeErr := data.Workflow(&tradepb.TradeCommerceRecord{WorkflowProto: rawWorkflow})
			if decodeErr == nil && w != nil {
				sellerAcct = w.GetSellerIdentity().GetAccount()
				buyerAcct = w.GetBuyerIdentity().GetAccount()
			}
		}
		samples = append(samples, MarketFillSample{
			ItemConfigID:  itemConfigID,
			CurrencyType:  currencyType,
			UnitPrice:     unitPrice,
			Quantity:      qty,
			SellerAccount: sellerAcct,
			BuyerAccount:  buyerAcct,
			SettledAtMs:   updatedMs,
		})
	}
	if err := rows.Err(); err != nil {
		return ReferencePrice{}, err
	}
	return ComputeReferencePrice(samples, itemConfigID, currencyType, nowMs, windowMs, r.MinSamples), nil
}

// EstimateSelectionValue 按参考价 reader 估算一侧资产选择在指定币种下的公允参考总额。
// 若包含唯一装备实例、宠物、不同币种货币或任一物品参考价样本不足，则 hasUnpriced=true（fail-closed）。
func EstimateSelectionValue(ctx context.Context, reader ReferencePriceReader, sel *tradepb.TradeAssetSelection, currencyType uint32, nowMs uint64) (total uint64, hasUnpriced bool, err error) {
	if sel == nil {
		return 0, false, nil
	}
	if len(sel.ItemUuids) > 0 || sel.PetId != 0 {
		hasUnpriced = true
	}
	if sel.CurrencyAmount > 0 {
		if sel.CurrencyType != currencyType {
			hasUnpriced = true
		} else {
			total = sel.CurrencyAmount
		}
	}
	if len(sel.Items) == 0 {
		return total, hasUnpriced, nil
	}
	if reader == nil {
		return total, true, nil
	}
	for cfgID, count := range sel.Items {
		if count == 0 {
			continue
		}
		ref, refErr := reader.ReferencePrice(ctx, cfgID, currencyType, nowMs)
		if refErr != nil {
			return 0, true, refErr
		}
		if ref.InsufficientSample || ref.MedianUnitPrice == 0 {
			hasUnpriced = true
			continue
		}
		if ref.MedianUnitPrice > math.MaxUint64/uint64(count) {
			return math.MaxUint64, true, nil
		}
		part := ref.MedianUnitPrice * uint64(count)
		if total > math.MaxUint64-part {
			return math.MaxUint64, true, nil
		}
		total += part
	}
	return total, hasUnpriced, nil
}

package logic

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"data_service/internal/store"

	dspb "proto/data_service"
)

// 回收计划:把流水算成「要回收什么」。纯函数,没有 I/O,预览和执行走同一份代码,所以摘要相等就是计划相同。

const (
	// maxRecallHintsPerOp 是一行指令带的实例号线索上限,和 scene 的 kAssetOpMaxRecallHints 同值。
	maxRecallHintsPerOp = 64
	// maxRecallOpsPerPlayer 等于 assetop.DefaultLimits.MaxPending:一条流最多 16 行未决。再多的派不出去,不如冻结之前就拒绝。
	maxRecallOpsPerPlayer = 16
)

// errRecallPlanTooLarge:金额溢出,或一名玩家的指令超过上限。缩小范围分批回收。
var errRecallPlanTooLarge = errors.New("recall plan exceeds a per-player limit")

type recallFilter struct {
	Kind         dspb.RecallKind
	PlayerIDs    []uint64
	ItemConfigID uint32
	CurrencyType uint32
	TimeStart    uint64
	TimeEnd      uint64
	TxTypes      []uint32
}

// digest 是过滤条件的摘要,进 request_digest:同一个 request_id 只能对应一组条件。名单和类型排序后再算,顺序不算不同。
func (f recallFilter) digest() []byte {
	players := slices.Sorted(slices.Values(f.PlayerIDs))
	types := slices.Sorted(slices.Values(f.TxTypes))
	sum := sha256.Sum256([]byte(fmt.Sprintf("v1|%d|%d|%d|%d|%d|%v|%v", f.Kind, f.ItemConfigID, f.CurrencyType, f.TimeStart, f.TimeEnd, players, types)))
	return sum[:]
}

// plannedRecallOp 是计划里的一行指令。
type plannedRecallOp struct {
	PlayerID     uint64
	CurrencyType uint32
	ItemConfigID uint32
	Requested    uint64
	Hints        []uint64
	SourceTxIDs  []uint64
}

type recallPlan struct {
	Ops             []*plannedRecallOp
	SourceRows      uint32
	PlayerCount     uint32
	AmountRequested uint64
	Digest          []byte
}

// DigestHex 是回给运维的摘要。
func (p *recallPlan) DigestHex() string { return hex.EncodeToString(p.Digest) }

// recallSourceTxIDs 是这批流水里能算收益的 tx_id。已消费的要先按它查出来再剔除。
func recallSourceTxIDs(filter recallFilter, rows []*store.TransactionLogRow) []uint64 {
	ids := make([]uint64, 0, len(rows))
	for _, row := range rows {
		if recallGainOf(filter, row) > 0 {
			ids = append(ids, row.TxID)
		}
	}
	return ids
}

// recallGainOf 是这条流水给 to_player 带来的收益,不是收益回 0。转出、销毁这类流水不进计划。
func recallGainOf(filter recallFilter, row *store.TransactionLogRow) uint64 {
	if row.ToPlayer == 0 {
		return 0
	}
	// QueryLog 按 from_player OR to_player 查。带名单时只回收名单里收到的,不能回收到名单外的交易对方。
	if len(filter.PlayerIDs) > 0 && !slices.Contains(filter.PlayerIDs, row.ToPlayer) {
		return 0
	}
	if filter.Kind == dspb.RecallKind_RECALL_KIND_ITEM {
		if row.ItemConfigID != filter.ItemConfigID {
			return 0
		}
		return uint64(row.ItemQuantity)
	}
	if row.ItemConfigID != 0 || row.CurrencyType != filter.CurrencyType || row.CurrencyDelta <= 0 {
		return 0
	}
	return uint64(row.CurrencyDelta)
}

// buildRecallPlan 按 (玩家, 资产) 汇总流水。consumed 是已被回收过的 tx_id,一条流水只能回收一次。
func buildRecallPlan(filter recallFilter, rows []*store.TransactionLogRow, consumed map[uint64]bool) (*recallPlan, error) {
	ordered := make([]*store.TransactionLogRow, 0, len(rows))
	seen := make(map[uint64]bool, len(rows))
	for _, row := range rows {
		// 带名单时同一条流水可能查出来两次(from 和 to 都在名单里)。
		if seen[row.TxID] || consumed[row.TxID] || recallGainOf(filter, row) == 0 {
			continue
		}
		seen[row.TxID] = true
		ordered = append(ordered, row)
	}
	slices.SortFunc(ordered, func(a, b *store.TransactionLogRow) int {
		if a.ToPlayer != b.ToPlayer {
			if a.ToPlayer < b.ToPlayer {
				return -1
			}
			return 1
		}
		if a.TxID < b.TxID {
			return -1
		}
		if a.TxID > b.TxID {
			return 1
		}
		return 0
	})

	plan := &recallPlan{SourceRows: uint32(len(ordered))}
	for start := 0; start < len(ordered); {
		end := start
		for end < len(ordered) && ordered[end].ToPlayer == ordered[start].ToPlayer {
			end++
		}
		ops, err := planOnePlayer(filter, ordered[start:end])
		if err != nil {
			return nil, fmt.Errorf("player %d: %w", ordered[start].ToPlayer, err)
		}
		plan.Ops = append(plan.Ops, ops...)
		plan.PlayerCount++
		start = end
	}
	for _, op := range plan.Ops {
		if plan.AmountRequested > math.MaxUint64-op.Requested {
			return nil, errRecallPlanTooLarge
		}
		plan.AmountRequested += op.Requested
	}
	plan.Digest = recallPlanDigest(filter, plan.Ops)
	return plan, nil
}

// planOnePlayer 汇总一名玩家的流水。货币恰好一行,scene 上限是 INT64_MAX。道具按实例号线索和 uint32 数量拆行。
func planOnePlayer(filter recallFilter, rows []*store.TransactionLogRow) ([]*plannedRecallOp, error) {
	limit := uint64(math.MaxUint32)
	if filter.Kind == dspb.RecallKind_RECALL_KIND_CURRENCY {
		limit = math.MaxInt64
	}
	current := &plannedRecallOp{PlayerID: rows[0].ToPlayer, CurrencyType: filter.CurrencyType, ItemConfigID: filter.ItemConfigID}
	ops := []*plannedRecallOp{current}
	for _, row := range rows {
		gain := recallGainOf(filter, row)
		if gain > limit {
			return nil, errRecallPlanTooLarge
		}
		hint := row.ItemUUID
		if filter.Kind == dspb.RecallKind_RECALL_KIND_CURRENCY {
			hint = 0
		}
		newHint := hint != 0 && !slices.Contains(current.Hints, hint)
		if current.Requested > limit-gain || (newHint && len(current.Hints) == maxRecallHintsPerOp) {
			if filter.Kind == dspb.RecallKind_RECALL_KIND_CURRENCY {
				// 货币只有一行:拆开之后第二行的 AttachDebt 会和第一行互相覆盖理由,金额这么大本身就要人看。
				return nil, errRecallPlanTooLarge
			}
			current = &plannedRecallOp{PlayerID: rows[0].ToPlayer, CurrencyType: filter.CurrencyType, ItemConfigID: filter.ItemConfigID}
			ops = append(ops, current)
			newHint = hint != 0
		}
		current.Requested += gain
		current.SourceTxIDs = append(current.SourceTxIDs, row.TxID)
		if newHint {
			current.Hints = append(current.Hints, hint)
		}
	}
	if len(ops) > maxRecallOpsPerPlayer {
		return nil, errRecallPlanTooLarge
	}
	return ops, nil
}

// recallPlanDigest 是计划的摘要。过滤条件和每一行的玩家、数量、线索、流水都进去:人审的是什么,执行的就是什么。
func recallPlanDigest(filter recallFilter, ops []*plannedRecallOp) []byte {
	var b strings.Builder
	b.WriteString(hex.EncodeToString(filter.digest()))
	for _, op := range ops {
		fmt.Fprintf(&b, "\n%d:%d:%d:%d:%v:%v", op.PlayerID, op.CurrencyType, op.ItemConfigID, op.Requested, op.Hints, op.SourceTxIDs)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return sum[:]
}

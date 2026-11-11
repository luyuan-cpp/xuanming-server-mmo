package logic

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"data_service/internal/store"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"
	dspb "proto/data_service"
	"shared/assetop"
)

// 回收计划:把流水算成「要回收什么」。纯函数,没有 I/O(证据由 recall_logic.go 的 loadRecallInputs 一次取齐),
// 预览和执行走同一份代码,所以摘要相等就是计划相同。
//
// 流水只是线索,不是证据(docs/design/exploit_loss_prevention_zh.md「计划怎么算」)。2026-11-05 起 scene 只在存盘
// 提交之后才发流水(TransactionLogSystem 的提交闸门),但发送失败只打日志,提交成功而回包丢失时崩溃也会少一条;
// 混版窗口里没开闸门的旧 scene 仍在内存改动时就发:崩溃后重载、同一条指令重投再应用时,会多出一条同 correlation_id、
// 不同 tx_id 的流水,确定性拒存时整段会话都不落盘,流水却都在。所以一笔收益要进自动回收,必须由玩家已提交的
// 资产账本(player_store)证明它那条指令恰好应用了一次,且这条指令的几条流水能还原成同一次发放。证明不了的列为
// 待人工核对,不进自动扣款。去重按指令(to_player, correlation_id),不按 tx_id。

const (
	// maxRecallHintsPerOp 是一行指令带的实例号线索上限,和 scene 的 kAssetOpMaxRecallHints 同值。
	maxRecallHintsPerOp = 64
	// maxRecallOpsPerPlayer 等于 assetop.DefaultLimits.MaxPending:一条流最多 16 行未决。再多的派不出去,不如冻结之前就拒绝。
	maxRecallOpsPerPlayer = 16
)

// errRecallPlanTooLarge:金额溢出、一名玩家的指令超过上限、要核账本的玩家或同指令流水超过上限。缩小范围分批回收。
var errRecallPlanTooLarge = errors.New("recall plan exceeds a size limit")

// recallProven 表示已提交账本证实这条指令恰好应用了一次。借用原因码的 0 值,它不会出现在待核对的行里。
const recallProven = dspb.RecallReviewReason_RECALL_REVIEW_REASON_UNSPECIFIED

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

// recallInputs 是算计划要的全部输入,由 loadRecallInputs 一次取齐。
type recallInputs struct {
	// Rows 是时间窗内的候选流水,QueryLog 原样:带名单时同一条可能出现两次,也有不是收益的行。
	Rows []*store.TransactionLogRow
	// Siblings 是候选里每条指令不限时间窗的全部流水:同一条指令重投再应用时,多出来的那条可能落在时间窗外。
	Siblings []*store.TransactionLogRow
	// Ledgers 是玩家已提交的资产账本,键是 player_id。缺了的玩家按「没有已提交存档」处理,不会被当成已证实。
	Ledgers map[uint64]PlayerStoreLedger
	// Consumed 是已被回收过的 tx_id。
	Consumed map[uint64]bool
}

// recallInstructionKey 是一条指令:谁收到、指令号是什么。资产通道的指令号是发起方的 op_id,全局唯一。
type recallInstructionKey struct {
	PlayerID      uint64
	CorrelationID uint64
}

// recallInstruction 是一条已证实的指令:已提交账本说它恰好应用了一次,Amount 是那一次发放的数量。
type recallInstruction struct {
	Amount uint64
	// Hint 是道具实例号线索,0 = 没有(货币恒为 0)。
	Hint uint64
	// SourceTxIDs 是这条指令的全部流水,含重投出来的、时间窗外的。冻结时一起消费:哪一条留着,下一批都会把这条指令再回收一次。
	SourceTxIDs []uint64
}

// recallReview 是一条待人工核对的流水:它没进自动回收,原因见 Reason。
type recallReview struct {
	Row    *store.TransactionLogRow
	Reason dspb.RecallReviewReason
}

type recallPlan struct {
	Ops []*plannedRecallOp
	// Reviews 按 (to_player, tx_id) 排序,进摘要。
	Reviews         []recallReview
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

// recallInstructionIDs 给出候选里每名收益玩家要核对的指令号,升序去重。取账本和同指令流水都按它。
func recallInstructionIDs(filter recallFilter, rows []*store.TransactionLogRow) map[uint64][]uint64 {
	seen := make(map[recallInstructionKey]bool)
	out := make(map[uint64][]uint64)
	for _, row := range rows {
		key := recallInstructionKey{PlayerID: row.ToPlayer, CorrelationID: row.CorrelationID}
		if key.CorrelationID == 0 || seen[key] || recallGainOf(filter, row) == 0 {
			continue
		}
		seen[key] = true
		out[key.PlayerID] = append(out[key.PlayerID], key.CorrelationID)
	}
	for _, ids := range out {
		slices.Sort(ids)
	}
	return out
}

// buildRecallPlan 先按指令(to_player, correlation_id)汇总流水,再按 (玩家, 资产) 拼成回收行。
//
// 一笔收益只有三种去向:
//   - 进计划:已提交账本证实它那条指令恰好应用了一次,且几条流水能还原成同一次发放。数量只算一次,
//     同一指令的全部流水(含重投出来的、时间窗外的)一起消费;
//   - 待人工核对:证明不了,原因见 dspb.RecallReviewReason。不进计划,也不消费;
//   - 剔除:它那条指令已经被回收过,或已登记交人工处置(任何一条流水已消费,见 RegisterRecallReviewHandled)。
//     证据以后转正也不会再进计划,人工扣过的不会被自动再扣一次。
func buildRecallPlan(filter recallFilter, in recallInputs) (*recallPlan, error) {
	plan := &recallPlan{}
	groups := make(map[recallInstructionKey][]*store.TransactionLogRow)
	var keys []recallInstructionKey
	for _, row := range distinctGains(filter, in.Rows) {
		if row.CorrelationID == 0 {
			if !in.Consumed[row.TxID] {
				plan.Reviews = append(plan.Reviews, recallReview{Row: row, Reason: dspb.RecallReviewReason_RECALL_REVIEW_NO_CORRELATION})
			}
			continue
		}
		key := recallInstructionKey{PlayerID: row.ToPlayer, CorrelationID: row.CorrelationID}
		if _, ok := groups[key]; !ok {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], row)
	}
	// 时间窗外的同指令流水并进来。只并已在候选里的指令:窗外别的指令不归这一批。
	for _, row := range distinctGains(filter, in.Siblings) {
		key := recallInstructionKey{PlayerID: row.ToPlayer, CorrelationID: row.CorrelationID}
		if rows, ok := groups[key]; ok && !slices.ContainsFunc(rows, func(r *store.TransactionLogRow) bool { return r.TxID == row.TxID }) {
			groups[key] = append(rows, row)
		}
	}
	slices.SortFunc(keys, compareRecallInstructionKeys)

	var players []uint64
	accepted := make(map[uint64][]recallInstruction)
	for _, key := range keys {
		rows := groups[key]
		slices.SortFunc(rows, compareRecallTxID)
		if slices.ContainsFunc(rows, func(r *store.TransactionLogRow) bool { return in.Consumed[r.TxID] }) {
			continue
		}
		if reason := instructionVerdict(in.Ledgers[key.PlayerID], key.CorrelationID); reason != recallProven {
			plan.Reviews = append(plan.Reviews, reviewsOf(rows, reason)...)
			continue
		}
		instruction, ok := oneApplication(filter, rows)
		if !ok {
			plan.Reviews = append(plan.Reviews, reviewsOf(rows, dspb.RecallReviewReason_RECALL_REVIEW_ROWS_DISAGREE)...)
			continue
		}
		if _, ok := accepted[key.PlayerID]; !ok {
			players = append(players, key.PlayerID) // keys 按玩家排过序,players 也是升序
		}
		accepted[key.PlayerID] = append(accepted[key.PlayerID], instruction)
	}
	slices.SortFunc(plan.Reviews, compareRecallReviews)

	for _, playerID := range players {
		ops, err := planOnePlayer(filter, playerID, accepted[playerID])
		if err != nil {
			return nil, fmt.Errorf("player %d: %w", playerID, err)
		}
		plan.Ops = append(plan.Ops, ops...)
		plan.PlayerCount++
	}
	for _, op := range plan.Ops {
		if plan.AmountRequested > math.MaxUint64-op.Requested {
			return nil, errRecallPlanTooLarge
		}
		plan.AmountRequested += op.Requested
		plan.SourceRows += uint32(len(op.SourceTxIDs))
	}
	plan.Digest = recallPlanDigest(filter, plan.Ops, plan.Reviews)
	return plan, nil
}

// distinctGains 去掉重复行(带名单时同一条流水可能查出来两次)和不是收益的行,按 (to_player, tx_id) 排序。
func distinctGains(filter recallFilter, rows []*store.TransactionLogRow) []*store.TransactionLogRow {
	out := make([]*store.TransactionLogRow, 0, len(rows))
	seen := make(map[uint64]bool, len(rows))
	for _, row := range rows {
		if seen[row.TxID] || recallGainOf(filter, row) == 0 {
			continue
		}
		seen[row.TxID] = true
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b *store.TransactionLogRow) int {
		if c := cmp.Compare(a.ToPlayer, b.ToPlayer); c != 0 {
			return c
		}
		return compareRecallTxID(a, b)
	})
	return out
}

// instructionVerdict 拿玩家已提交的资产账本核一条指令,回 recallProven 或待核对的原因。
//
// 资产通道每记一个 seq 的结局,都在同一次提交里留一张收据(seq、correlation_id、请求摘要),收据只留在 1024 位窗口内。
// 只认发放流(*_CREDIT)上的收据:收益只会来自发放。收据唯一、位图说已应用、不是部分应用,才算证实这条指令恰好应用了一次。
// 原始请求不在手上,摘要核不了;靠的是指令号 = 发起方的 op_id,全局唯一。
func instructionVerdict(snapshot PlayerStoreLedger, correlationID uint64) dspb.RecallReviewReason {
	if !snapshot.Found || snapshot.Ledger == nil {
		return dspb.RecallReviewReason_RECALL_REVIEW_NO_PLAYER_STORE
	}
	var (
		stream  *componentpb.AssetOpStreamLedger
		receipt *componentpb.AssetOpReceipt
		matches int
	)
	seenStreams := make(map[assetpb.AssetOpStream]bool)
	for _, candidate := range snapshot.Ledger.GetStreams() {
		if rpc, ok := assetop.ApplyRPCOf(candidate.GetStream()); !ok || rpc != assetop.RPCCredit {
			continue
		}
		if seenStreams[candidate.GetStream()] {
			// 同一条流出现两次是账本损坏,下面按流号分类会读到另一份,说不清。
			return dspb.RecallReviewReason_RECALL_REVIEW_LEDGER_AMBIGUOUS
		}
		seenStreams[candidate.GetStream()] = true
		for _, r := range candidate.GetReceipts() {
			if r.GetCorrelationId() == correlationID {
				stream, receipt = candidate, r
				matches++
			}
		}
	}
	switch {
	case matches == 0:
		return dspb.RecallReviewReason_RECALL_REVIEW_NOT_IN_LEDGER
	case matches > 1:
		return dspb.RecallReviewReason_RECALL_REVIEW_LEDGER_AMBIGUOUS
	}
	outcome, partial, _ := assetop.ClassifyPersisted(snapshot.Ledger, stream.GetStream(), stream.GetStreamEpoch(), receipt.GetSeq())
	switch {
	case outcome == assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED:
		return dspb.RecallReviewReason_RECALL_REVIEW_LEDGER_REJECTED
	case outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED:
		return dspb.RecallReviewReason_RECALL_REVIEW_LEDGER_NOT_APPLIED
	case partial:
		return dspb.RecallReviewReason_RECALL_REVIEW_LEDGER_PARTIAL
	}
	return recallProven
}

// oneApplication 把一条已证实指令的流水还原成落盘的那一次发放。账本说只应用了一次,多出来的流水是重投时
// 内存里又应用了一遍(那一遍随崩溃或拒存丢了),和落盘的那一次一模一样:
//   - 货币:一次发放每个币种恰好一条流水(scene 拒收重复币种),几条流水的数额必须相等;
//   - 道具:只有一条流水时就是那一次。不止一条时,只认同一实例被重投(同 item_uuid、同数量):一次发多件不可叠加物品,
//     和重投时新铸了实例,流水上分不出来;而可叠加物品按数量扣,认错就多扣。
//
// 还原不成回 false,整条指令待人工核对。
func oneApplication(filter recallFilter, rows []*store.TransactionLogRow) (recallInstruction, bool) {
	first := rows[0]
	instruction := recallInstruction{Amount: recallGainOf(filter, first), SourceTxIDs: make([]uint64, 0, len(rows))}
	isItem := filter.Kind == dspb.RecallKind_RECALL_KIND_ITEM
	if isItem {
		instruction.Hint = first.ItemUUID
	}
	for _, row := range rows {
		if recallGainOf(filter, row) != instruction.Amount {
			return recallInstruction{}, false
		}
		if isItem && len(rows) > 1 && (row.ItemUUID == 0 || row.ItemUUID != first.ItemUUID) {
			return recallInstruction{}, false
		}
		instruction.SourceTxIDs = append(instruction.SourceTxIDs, row.TxID)
	}
	return instruction, true
}

func reviewsOf(rows []*store.TransactionLogRow, reason dspb.RecallReviewReason) []recallReview {
	out := make([]recallReview, 0, len(rows))
	for _, row := range rows {
		out = append(out, recallReview{Row: row, Reason: reason})
	}
	return out
}

func compareRecallTxID(a, b *store.TransactionLogRow) int { return cmp.Compare(a.TxID, b.TxID) }

func compareRecallInstructionKeys(a, b recallInstructionKey) int {
	if c := cmp.Compare(a.PlayerID, b.PlayerID); c != 0 {
		return c
	}
	return cmp.Compare(a.CorrelationID, b.CorrelationID)
}

// compareRecallReviews 按 (to_player, tx_id) 排:摘要要确定性,运维看也按人归拢。
func compareRecallReviews(a, b recallReview) int {
	if c := cmp.Compare(a.Row.ToPlayer, b.Row.ToPlayer); c != 0 {
		return c
	}
	return compareRecallTxID(a.Row, b.Row)
}

// planOnePlayer 把一名玩家已证实的指令拼成回收行。货币恰好一行,scene 上限是 INT64_MAX。道具按实例号线索和 uint32 数量拆行。
func planOnePlayer(filter recallFilter, playerID uint64, instructions []recallInstruction) ([]*plannedRecallOp, error) {
	limit := uint64(math.MaxUint32)
	if filter.Kind == dspb.RecallKind_RECALL_KIND_CURRENCY {
		limit = math.MaxInt64
	}
	newOp := func() *plannedRecallOp {
		return &plannedRecallOp{PlayerID: playerID, CurrencyType: filter.CurrencyType, ItemConfigID: filter.ItemConfigID}
	}
	current := newOp()
	ops := []*plannedRecallOp{current}
	for _, instruction := range instructions {
		if instruction.Amount > limit {
			return nil, errRecallPlanTooLarge
		}
		newHint := instruction.Hint != 0 && !slices.Contains(current.Hints, instruction.Hint)
		if current.Requested > limit-instruction.Amount || (newHint && len(current.Hints) == maxRecallHintsPerOp) {
			if filter.Kind == dspb.RecallKind_RECALL_KIND_CURRENCY {
				// 货币只有一行:拆开之后第二行的 AttachDebt 会和第一行互相覆盖理由,金额这么大本身就要人看。
				return nil, errRecallPlanTooLarge
			}
			current = newOp()
			ops = append(ops, current)
			newHint = instruction.Hint != 0
		}
		current.Requested += instruction.Amount
		current.SourceTxIDs = append(current.SourceTxIDs, instruction.SourceTxIDs...)
		if newHint {
			current.Hints = append(current.Hints, instruction.Hint)
		}
	}
	if len(ops) > maxRecallOpsPerPlayer {
		return nil, errRecallPlanTooLarge
	}
	return ops, nil
}

// recallPlanDigest 是计划的摘要。过滤条件,每一行的玩家、数量、线索、流水,以及每条待核对流水和它的原因都进去:
// 人审的是什么,执行的就是什么;证据变了(比如账本后来证实了一条指令),摘要也跟着变。
func recallPlanDigest(filter recallFilter, ops []*plannedRecallOp, reviews []recallReview) []byte {
	var b strings.Builder
	b.WriteString(hex.EncodeToString(filter.digest()))
	for _, op := range ops {
		fmt.Fprintf(&b, "\n%d:%d:%d:%d:%v:%v", op.PlayerID, op.CurrencyType, op.ItemConfigID, op.Requested, op.Hints, op.SourceTxIDs)
	}
	for _, review := range reviews {
		fmt.Fprintf(&b, "\nreview:%d:%d", review.Row.TxID, int32(review.Reason))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return sum[:]
}

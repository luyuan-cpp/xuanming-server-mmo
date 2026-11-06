package logic

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"data_service/internal/constants"
	"data_service/internal/store"
	"data_service/internal/svc"

	dspb "proto/data_service"

	"github.com/zeromicro/go-zero/core/logx"
)

// ── RegisterRecallReviewHandled ────────────────────────────────
//
// needs_review 指令交人工处置的登记(docs/design/exploit_loss_prevention_zh.md「待人工核对的流水」)。
//
// needs_review 的指令不进自动回收,但证据以后可能转正:没落盘的那次改动重投之后落了盘(NOT_IN_LEDGER)、多出来的那张收据
// 滑出了窗口(LEDGER_AMBIGUOUS)、对不上的那条流水过了保留期被删(ROWS_DISAGREE)。转正之后下一批会把它自动扣一次,
// 人工已经扣过的就多扣了。所以人工处置之前先登记:这条指令(to_player, correlation_id)此刻的全部流水,在一个事务里按已消费
// 写进 gm_recall_source(store.RecallStore.RegisterReviewHandled)。写入和自动回收走同一个主键,以后的批次与预览都把它剔除,
// 人工登记的流水没有放回路径。先登记再处置,中间就没有「人工扣了、批次又扣」的窗口。
//
// 只收此刻确实在 needs_review 里的指令,判定用计划的同一份代码(buildRecallPlan),任何一步对不上都不写(fail-closed):
//  1. 按指令号取这条指令的全部收益流水(不限时间窗,与计划补查同一种查法)。一条都没有回 34;
//  2. 有流水已被回收批次消费:它已不在 needs_review 里,回 32。已登记过:幂等,回 0、registered=false;
//  3. 读这名玩家已提交的账本重算:账本证实、能自动扣的不收,回 32,走 BatchRecallItems;
//  4. 预览里看到的 tx_id 集合与原因码要和重算的一致,对不上回 32,重新预览;
//  5. 登记。主键兜并发:回收批次抢先回 32,另一次相同的登记抢先回 0、registered=false。
//
// 没有指令号的流水(NO_CORRELATION)不收:它们永远进不了自动回收,不登记也不会被扣第二次。
// 不看 Recall.Enabled:登记只写来源表与审计表,不碰资产通道。

// RecallReviewHandledReq 是一次登记的入参,字段同 RegisterRecallReviewHandledRequest。
type RecallReviewHandledReq struct {
	Kind          dspb.RecallKind
	ItemConfigID  uint32
	CurrencyType  uint32
	PlayerID      uint64
	CorrelationID uint64
	// TxIDs 是预览 needs_review 里这条指令的全部 tx_id。
	TxIDs []uint64
	// ReviewReason 是预览里这条指令的原因。
	ReviewReason dspb.RecallReviewReason
	Operator     string
	Reason       string
	// PeerAddr 是调用方对端地址,进审计行。
	PeerAddr string
}

// RecallReviewHandledResult 是一次登记的回答。
type RecallReviewHandledResult struct {
	ErrorCode uint32
	// Registered:本次写入了登记。ErrorCode = 0 而它为 false:这条指令早已登记过,什么都没改。
	Registered bool
	// SourceRows 是本次按已消费写进 gm_recall_source 的流水条数。
	SourceRows uint32
	// ReviewReason 是服务端重算的原因。账本已证实、能自动扣,或重算之前就拒绝了,都是 0。
	ReviewReason dspb.RecallReviewReason
}

// filter 是这次登记的计划过滤条件:只看这名玩家收到的这一种资产。时间窗与 tx_type 不进来:同指令流水本来就不限时间窗补查。
func (r *RecallReviewHandledReq) filter() recallFilter {
	return recallFilter{Kind: r.Kind, PlayerIDs: []uint64{r.PlayerID}, ItemConfigID: r.ItemConfigID, CurrencyType: r.CurrencyType}
}

// validate 是不碰库的入参校验。tx_ids 是一个集合:不许有 0,不许重复。
func (r *RecallReviewHandledReq) validate() bool {
	if r.PlayerID == 0 || r.CorrelationID == 0 || len(r.TxIDs) == 0 || len(r.TxIDs) > maxRecallSourceRows ||
		!validRecallAsset(r.Kind, r.ItemConfigID, r.CurrencyType) ||
		!validRecallText(r.Operator, recallOperatorMaxRunes) || !validRecallText(r.Reason, recallReasonMaxRunes) {
		return false
	}
	if _, known := dspb.RecallReviewReason_name[int32(r.ReviewReason)]; !known ||
		r.ReviewReason == recallProven || r.ReviewReason == dspb.RecallReviewReason_RECALL_REVIEW_NO_CORRELATION {
		return false
	}
	seen := make(map[uint64]bool, len(r.TxIDs))
	for _, id := range r.TxIDs {
		if id == 0 || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

// RegisterRecallReviewHandled 把一条 needs_review 指令登记为交人工处置,步骤见本节开头。
func RegisterRecallReviewHandled(ctx context.Context, svcCtx *svc.ServiceContext, req *RecallReviewHandledReq) (RecallReviewHandledResult, error) {
	return registerRecallReviewHandled(ctx, svcCtx, req, time.Now)
}

// registerRecallReviewHandled 是 RegisterRecallReviewHandled 的实现,时钟显式传入。
func registerRecallReviewHandled(ctx context.Context, svcCtx *svc.ServiceContext, req *RecallReviewHandledReq, now func() time.Time) (RecallReviewHandledResult, error) {
	if req == nil || !req.validate() {
		return RecallReviewHandledResult{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if svcCtx == nil || svcCtx.TxLogStore == nil || svcCtx.Recall == nil {
		return RecallReviewHandledResult{ErrorCode: constants.ErrCodeSnapshotDBError}, nil
	}
	result, err := registerListedReview(ctx, svcCtx, req, now)
	auditRecallReviewHandled(req, result)
	return result, err
}

// registerListedReview 是步骤 1~5。
func registerListedReview(ctx context.Context, svcCtx *svc.ServiceContext, req *RecallReviewHandledReq, now func() time.Time) (RecallReviewHandledResult, error) {
	filter := req.filter()
	rows, total, err := svcCtx.TxLogStore.QueryLog(ctx, &store.TransactionLogQuery{
		PlayerID:       req.PlayerID,
		CorrelationIDs: []uint64{req.CorrelationID},
		ItemConfigID:   req.ItemConfigID,
		CurrencyType:   req.CurrencyType,
		ExactCurrency:  req.Kind == dspb.RecallKind_RECALL_KIND_CURRENCY,
		Limit:          batchRecallQueryLimit,
	})
	if err != nil {
		return RecallReviewHandledResult{ErrorCode: constants.ErrCodeSnapshotDBError}, fmt.Errorf("query instruction rows: %w", err)
	}
	if uint64(total) != uint64(len(rows)) {
		return RecallReviewHandledResult{ErrorCode: constants.ErrCodeResultTruncated}, nil
	}
	gains := slices.DeleteFunc(distinctGains(filter, rows), func(row *store.TransactionLogRow) bool {
		return row.CorrelationID != req.CorrelationID
	})
	if len(gains) == 0 {
		return RecallReviewHandledResult{ErrorCode: constants.ErrCodeRecallNotFound}, nil
	}
	txIDs := make([]uint64, 0, len(gains))
	for _, row := range gains {
		txIDs = append(txIDs, row.TxID)
	}

	state, err := svcCtx.Recall.SourceState(ctx, txIDs)
	if err != nil {
		return RecallReviewHandledResult{ErrorCode: constants.ErrCodeSnapshotDBError}, fmt.Errorf("read recall source state: %w", err)
	}
	switch {
	case state.Batch:
		// 已被回收批次消费的指令不在 needs_review 里。
		return RecallReviewHandledResult{ErrorCode: constants.ErrCodeRecallPlanChanged}, nil
	case state.Manual:
		return RecallReviewHandledResult{ErrorCode: constants.ErrCodeOK}, nil
	}

	ledger, err := GetPlayerStoreLedger(ctx, svcCtx, req.PlayerID)
	if err != nil {
		return RecallReviewHandledResult{ErrorCode: constants.ErrCodeSnapshotDBError},
			fmt.Errorf("read committed asset ledger for player %d: %w", req.PlayerID, err)
	}
	plan, err := buildRecallPlan(filter, recallInputs{Rows: gains, Ledgers: map[uint64]PlayerStoreLedger{req.PlayerID: ledger}})
	if errors.Is(err, errRecallPlanTooLarge) {
		return RecallReviewHandledResult{ErrorCode: constants.ErrCodeResultTruncated}, nil
	}
	if err != nil {
		return RecallReviewHandledResult{ErrorCode: constants.ErrCodeSnapshotDBError}, err
	}
	if len(plan.Ops) > 0 || len(plan.Reviews) == 0 {
		// 账本证实、能自动扣:不在 needs_review 里,走 BatchRecallItems。
		return RecallReviewHandledResult{ErrorCode: constants.ErrCodeRecallPlanChanged}, nil
	}
	result := RecallReviewHandledResult{ReviewReason: plan.Reviews[0].Reason}
	reviewed := make([]uint64, 0, len(plan.Reviews))
	for _, review := range plan.Reviews {
		reviewed = append(reviewed, review.Row.TxID)
	}
	// 人看到的就是要登记的:预览里这条指令的原因与 tx_id 集合,都要和此刻重算的一致。
	if result.ReviewReason != req.ReviewReason || !sameRecallTxIDs(req.TxIDs, reviewed) {
		result.ErrorCode = constants.ErrCodeRecallPlanChanged
		return result, nil
	}

	registered, err := svcCtx.Recall.RegisterReviewHandled(ctx, store.RecallReviewHandled{
		PlayerID:      req.PlayerID,
		CorrelationID: req.CorrelationID,
		TxIDs:         reviewed,
		Operator:      req.Operator,
		Reason:        req.Reason,
		Detail: fmt.Sprintf("kind=%d item=%d currency=%d review_reason=%s peer=%s",
			req.Kind, req.ItemConfigID, req.CurrencyType, result.ReviewReason, req.PeerAddr),
	}, uint64(now().UnixMilli()))
	if errors.Is(err, store.ErrRecallSourceConsumed) {
		// 并发:回收批次或另一次相同的登记抢先写了其中某条,本次事务已回滚。重读分辨是谁。
		state, readErr := svcCtx.Recall.SourceState(ctx, reviewed)
		switch {
		case readErr != nil:
			result.ErrorCode = constants.ErrCodeSnapshotDBError
			return result, errors.Join(err, readErr)
		case state.Manual && !state.Batch:
			result.ErrorCode = constants.ErrCodeOK
		default:
			result.ErrorCode = constants.ErrCodeRecallPlanChanged
		}
		return result, nil
	}
	if err != nil {
		result.ErrorCode = constants.ErrCodeSnapshotDBError
		return result, fmt.Errorf("register recall review handled: %w", err)
	}
	result.ErrorCode, result.Registered = constants.ErrCodeOK, registered
	if registered {
		result.SourceRows = uint32(len(reviewed))
	}
	return result, nil
}

// sameRecallTxIDs 报告两组 tx_id 是不是同一个集合。
func sameRecallTxIDs(a, b []uint64) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// auditRecallReviewHandled 打登记的审计行:登记、幂等、拒绝都打(ERROR 级,确保进日志检索),带操作人、指令与结果。
// 操作人和理由按 %q 打:带换行就能伪造出第二条审计行。登记成功时同一事务里另有一行 rollback_audit_log。
func auditRecallReviewHandled(req *RecallReviewHandledReq, result RecallReviewHandledResult) {
	logx.Errorf("[RecallReviewHandled] operator=%q player_id=%d correlation_id=%d kind=%d item=%d currency=%d tx_ids=%d "+
		"review_reason=%s recomputed=%s registered=%v source_rows=%d error_code=%d peer=%s reason=%q",
		req.Operator, req.PlayerID, req.CorrelationID, req.Kind, req.ItemConfigID, req.CurrencyType, len(req.TxIDs),
		req.ReviewReason, result.ReviewReason, result.Registered, result.SourceRows, result.ErrorCode, req.PeerAddr, req.Reason)
}

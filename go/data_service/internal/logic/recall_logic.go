package logic

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"data_service/internal/constants"
	"data_service/internal/store"
	"data_service/internal/svc"

	assetpb "proto/common/asset"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	"shared/assetop"
	"shared/currencytype"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// ── BatchRecallItems ───────────────────────────────────────────
//
// 方案见 docs/design/exploit_loss_prevention_zh.md「BatchRecallItems 执行方案」。
//
// 这里只做两件事:算计划(预览),冻结计划(执行)。真正的扣除是异步的:后台派发、资产通道投递、scene 记账,
// 结果写回 gm_recall_op。所以 error_code == 0 只说明这批已经全部全额回收;受理成功回 ErrCodeRecallPending,
// 不让任何调用方把「计划已冻结」读成「事故已处置」。

type BatchRecallReq struct {
	PlayerIDs    []uint64
	Kind         dspb.RecallKind
	ItemConfigID uint32
	CurrencyType uint32
	TimeStart    uint64
	TimeEnd      uint64
	TxTypes      []uint32
	Reason       string
	Operator     string
	DryRun       bool
	// RequestID 和 ExpectedPlanDigest 只有执行时要。
	RequestID          uint64
	ExpectedPlanDigest string
	// PeerAddr 是调用方对端地址,进批次行。
	PeerAddr string
}

type RecallResult struct {
	PlayerID     uint64
	ItemConfigID uint32
	CurrencyType uint32
	Amount       uint64
	OpID         uint64
	Status       uint32
	Taken        uint64
	Debt         uint64
	Shortfall    uint64
	Success      bool
	ErrorDetail  string
}

type BatchRecallResp struct {
	ErrorCode       uint32
	TotalMatched    uint32
	TotalRecalled   uint32
	TotalFailed     uint32
	TotalPending    uint32
	Results         []RecallResult
	PlanDigest      string
	RequestID       uint64
	BatchState      uint32
	AmountRequested uint64
	AmountTaken     uint64
	AmountDebt      uint64
	AmountShortfall uint64
	CancelledMs     uint64
	// NeedsReview 只有预览回:没进自动回收、要人工核对的流水,按 (玩家, tx_id) 排序。
	NeedsReview []RecallReviewResult
}

// RecallReviewResult 是一条待人工核对的流水。Amount 是这条流水记的收益,不是要扣的数:它没有已提交的证据。
type RecallReviewResult struct {
	PlayerID      uint64
	TxID          uint64
	CorrelationID uint64
	TxType        uint32
	Amount        uint64
	ItemUUID      uint64
	Timestamp     uint64
	Reason        dspb.RecallReviewReason
}

const (
	batchRecallQueryLimit uint32 = 10000
	// maxRecallPlayers 是一次请求的玩家名单上限:每名玩家一次流水查询。再多请按时间窗走不带名单的查询。
	// 算计划时要核已提交账本的玩家(候选里带指令号的收益玩家)也按它封顶:每人一次账本读、一次同指令流水查询。
	maxRecallPlayers = 1000
	// maxRecallSourceRows 是一批的同指令流水(含时间窗外的重投流水)上限:冻结时它们在一个事务里写进 gm_recall_source。
	maxRecallSourceRows = int(batchRecallQueryLimit)
	// recallOperatorMaxRunes、recallReasonMaxRunes 和人工终结同口径:操作人 64 字,理由 191 字。
	recallOperatorMaxRunes = 64
	recallReasonMaxRunes   = 191
	// recallAuditType 是 rollback_audit_log.rollback_type 的取值:4 = 批量回收。
	recallAuditType = 4
)

func clampUint32(total uint64) uint32 {
	const maxUint32 = ^uint32(0)
	if total > uint64(maxUint32) {
		return maxUint32
	}
	return uint32(total)
}

func validRecallText(text string, maxRunes int) bool {
	return text != "" && utf8.ValidString(text) && utf8.RuneCountInString(text) <= maxRunes
}

func (r *BatchRecallReq) filter() recallFilter {
	return recallFilter{Kind: r.Kind, PlayerIDs: r.PlayerIDs, ItemConfigID: r.ItemConfigID, CurrencyType: r.CurrencyType,
		TimeStart: r.TimeStart, TimeEnd: r.TimeEnd, TxTypes: r.TxTypes}
}

// validRecallAsset 校验回收的资产:道具要配置号、不带币种;货币要认识的币种、不带配置号。
// 币种 0 是银两,所以种类由 Kind 决定,不能靠 currency_type 是否为 0 判。
func validRecallAsset(kind dspb.RecallKind, itemConfigID, currencyType uint32) bool {
	switch kind {
	case dspb.RecallKind_RECALL_KIND_ITEM:
		return itemConfigID != 0 && currencyType == 0
	case dspb.RecallKind_RECALL_KIND_CURRENCY:
		return itemConfigID == 0 && currencytype.IsKnown(currencyType)
	default:
		return false
	}
}

// validate 是不碰库的入参校验。
func (r *BatchRecallReq) validate() bool {
	if r.TimeStart == 0 || r.TimeEnd == 0 || r.TimeStart > r.TimeEnd || len(r.PlayerIDs) > maxRecallPlayers {
		return false
	}
	if !validRecallAsset(r.Kind, r.ItemConfigID, r.CurrencyType) {
		return false
	}
	if r.DryRun {
		return true
	}
	return r.RequestID != 0 && r.ExpectedPlanDigest != "" &&
		validRecallText(r.Operator, recallOperatorMaxRunes) && validRecallText(r.Reason, recallReasonMaxRunes)
}

func BatchRecallItems(ctx context.Context, svcCtx *svc.ServiceContext, req *BatchRecallReq) (*BatchRecallResp, error) {
	if req == nil || !req.validate() {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if svcCtx == nil || svcCtx.TxLogStore == nil || svcCtx.Recall == nil {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError}, nil
	}
	if !req.DryRun && svcCtx.SnapshotStore == nil {
		// 非 dry-run 即使被 fail-closed 拒绝也必须留下操作审计;审计库不可用时
		// 不能继续并假装这次高风险 GM 操作已被记录。
		return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError},
			fmt.Errorf("batch recall audit store is unavailable")
	}

	logx.Infof("[BatchRecall] operator=%s reason=%s kind=%d item_config=%d currency_type=%d dry_run=%v request_id=%d",
		req.Operator, req.Reason, req.Kind, req.ItemConfigID, req.CurrencyType, req.DryRun, req.RequestID)

	filter := req.filter()
	// 重放先看批次:计划已经冻结,不再重查流水,流水变了也不影响。
	if !req.DryRun {
		resp, handled, err := replayRecallBatch(ctx, svcCtx, req.RequestID, filter, req.ExpectedPlanDigest)
		if handled || err != nil {
			return resp, err
		}
		if !svcCtx.RecallReady {
			if err := auditRecallRejection(ctx, svcCtx, req, "DISABLED"); err != nil {
				return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError}, err
			}
			logx.Errorf("[BatchRecall] REJECTED: Recall.Enabled is off or its dependencies are not assembled; nothing was recalled, operator=%s", req.Operator)
			return &BatchRecallResp{ErrorCode: constants.ErrCodeRecallDisabled, RequestID: req.RequestID}, nil
		}
	}

	allRows, candidateTotal, truncated, err := queryRecallCandidates(ctx, svcCtx, req)
	if err != nil {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError}, err
	}
	if truncated {
		return rejectTruncatedRecall(ctx, svcCtx, req, candidateTotal, len(allRows))
	}

	// 流水只是线索:计划的证据是玩家已提交的资产账本(recall_plan.go 文件头)。
	in, err := loadRecallInputs(ctx, svcCtx, filter, allRows)
	var plan *recallPlan
	if err == nil {
		plan, err = buildRecallPlan(filter, in)
	}
	if errors.Is(err, errRecallPlanTooLarge) {
		logx.Errorf("[BatchRecall] plan rejected: %v", err)
		return rejectRecallPlan(ctx, svcCtx, req, candidateTotal, constants.ErrCodeResultTruncated, "PLAN_TOO_LARGE")
	}
	if err != nil {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError}, err
	}

	if req.DryRun {
		logx.Infof("[BatchRecall] dry_run complete: candidates=%d eligible=%d ops=%d needs_review=%d",
			candidateTotal, plan.SourceRows, len(plan.Ops), len(plan.Reviews))
		return previewRecallResp(candidateTotal, plan), nil
	}
	if plan.DigestHex() != req.ExpectedPlanDigest {
		// 人审过的不是这份计划。流水晚到、别的批次消费过、已提交账本的证据变了、过滤条件变了都会走到这里:重新预览。
		logx.Errorf("[BatchRecall] REJECTED: plan changed since review request_id=%d expected=%s actual=%s operator=%s",
			req.RequestID, req.ExpectedPlanDigest, plan.DigestHex(), req.Operator)
		return rejectRecallPlan(ctx, svcCtx, req, candidateTotal, constants.ErrCodeRecallPlanChanged, "PLAN_CHANGED")
	}
	if len(plan.Ops) == 0 {
		// 人审过的就是空计划:都不是收益、都已经回收过,或者都待人工核对。不写批次,也不回 0:0 说的是这批全额回收了,这里什么都没动。
		return &BatchRecallResp{ErrorCode: constants.ErrCodeRecallNothingToRecall, TotalMatched: clampUint32(candidateTotal), PlanDigest: plan.DigestHex(), RequestID: req.RequestID}, nil
	}
	return freezeRecallPlan(ctx, svcCtx, req, plan)
}

// replayRecallBatch 看这个 request_id 有没有批次。有:过滤条件和计划都一样回现状,不一样回冲突。没有:handled=false,接着走。
// 计划也要比:带着新预览的摘要重放旧 request_id,回旧批次的现状会让人以为新计划受理了。
func replayRecallBatch(ctx context.Context, svcCtx *svc.ServiceContext, requestID uint64, filter recallFilter, expectedPlanDigest string) (*BatchRecallResp, bool, error) {
	batch, err := svcCtx.Recall.ReadBatch(ctx, requestID)
	if errors.Is(err, store.ErrRecallBatchNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError}, true, fmt.Errorf("read recall batch: %w", err)
	}
	if !bytes.Equal(batch.RequestDigest, filter.digest()) || hex.EncodeToString(batch.PlanDigest) != expectedPlanDigest {
		logx.Errorf("[BatchRecall] REJECTED: request_id=%d already used with different filters or a different plan", requestID)
		return &BatchRecallResp{ErrorCode: constants.ErrCodeRecallRequestConflict, RequestID: requestID}, true, nil
	}
	resp, err := recallBatchResp(ctx, svcCtx, batch)
	return resp, true, err
}

// queryRecallCandidates 查流水。QueryLog 返回的 total 是筛选候选总数,rows 只是最多 10000 条的分页:对不上就是被截断了。
func queryRecallCandidates(ctx context.Context, svcCtx *svc.ServiceContext, req *BatchRecallReq) ([]*store.TransactionLogRow, uint64, bool, error) {
	query := func(playerID uint64) ([]*store.TransactionLogRow, uint32, error) {
		return svcCtx.TxLogStore.QueryLog(ctx, &store.TransactionLogQuery{
			PlayerID:     playerID,
			TimeStart:    req.TimeStart,
			TimeEnd:      req.TimeEnd,
			TxTypes:      req.TxTypes,
			ItemConfigID: req.ItemConfigID,
			CurrencyType: req.CurrencyType,
			// 币种 0 是银两,QueryLog 的 CurrencyType 0 表示不过滤,要显式说。
			ExactCurrency: req.Kind == dspb.RecallKind_RECALL_KIND_CURRENCY,
			Limit:         batchRecallQueryLimit,
		})
	}
	if len(req.PlayerIDs) == 0 {
		rows, total, err := query(0)
		if err != nil {
			return nil, 0, false, fmt.Errorf("query transaction_log: %w", err)
		}
		return rows, uint64(total), uint64(total) != uint64(len(rows)), nil
	}
	var allRows []*store.TransactionLogRow
	var candidateTotal uint64
	truncated := false
	for _, pid := range req.PlayerIDs {
		rows, total, err := query(pid)
		if err != nil {
			logx.Errorf("[BatchRecall] query player %d: %v", pid, err)
			// 指定玩家批次不能静默跳过失败查询,否则响应中的 matched/failed
			// 只覆盖成功查到的子集,运营会误以为整批都已核查。
			return nil, 0, false, fmt.Errorf("query transaction_log for player %d: %w", pid, err)
		}
		candidateTotal += uint64(total)
		if uint64(total) != uint64(len(rows)) {
			truncated = true
		}
		allRows = append(allRows, rows...)
	}
	if candidateTotal > uint64(batchRecallQueryLimit) {
		// 名单逐个查,每个都没截断,合起来也可能超过一批的上限。
		truncated = true
	}
	return allRows, candidateTotal, truncated, nil
}

// loadRecallInputs 取齐算计划要的证据:候选里每名带指令号的收益玩家读一次已提交的资产账本(player_store,
// 和 GetPlayerAssetOpLedger 同一种读法),按这些指令号查一次不限时间窗的同指令流水,最后按 tx_id 查哪些已被回收过。
//
// 任何一次读失败整批失败(fail-closed):少一份证据,算出来的就不是人审的那份计划。这些读都不加锁;
// 执行时重算一遍、和预览比摘要,中间证据变了就回 PLAN_CHANGED。
func loadRecallInputs(ctx context.Context, svcCtx *svc.ServiceContext, filter recallFilter, rows []*store.TransactionLogRow) (recallInputs, error) {
	in := recallInputs{Rows: rows, Ledgers: make(map[uint64]PlayerStoreLedger)}
	byPlayer := recallInstructionIDs(filter, rows)
	if len(byPlayer) > maxRecallPlayers {
		return in, fmt.Errorf("%w: %d players need a committed ledger read, limit %d", errRecallPlanTooLarge, len(byPlayer), maxRecallPlayers)
	}
	players := make([]uint64, 0, len(byPlayer))
	for playerID := range byPlayer {
		players = append(players, playerID)
	}
	slices.Sort(players)
	for _, playerID := range players {
		ledger, err := GetPlayerStoreLedger(ctx, svcCtx, playerID)
		if err != nil {
			return in, fmt.Errorf("read committed asset ledger for player %d: %w", playerID, err)
		}
		in.Ledgers[playerID] = ledger
		// 不带时间窗和 tx_type:同一条指令重投再应用时,多出来的那条流水可能落在窗外,要和窗内的一起消费。
		siblings, total, err := svcCtx.TxLogStore.QueryLog(ctx, &store.TransactionLogQuery{
			PlayerID:       playerID,
			CorrelationIDs: byPlayer[playerID],
			ItemConfigID:   filter.ItemConfigID,
			CurrencyType:   filter.CurrencyType,
			ExactCurrency:  filter.Kind == dspb.RecallKind_RECALL_KIND_CURRENCY,
			Limit:          batchRecallQueryLimit,
		})
		if err != nil {
			return in, fmt.Errorf("query instruction rows for player %d: %w", playerID, err)
		}
		if uint64(total) != uint64(len(siblings)) || len(in.Siblings)+len(siblings) > maxRecallSourceRows {
			return in, fmt.Errorf("%w: instruction rows exceed %d", errRecallPlanTooLarge, maxRecallSourceRows)
		}
		in.Siblings = append(in.Siblings, siblings...)
	}
	txIDs := append(recallSourceTxIDs(filter, rows), recallSourceTxIDs(filter, in.Siblings)...)
	consumed, err := svcCtx.Recall.ConsumedSources(ctx, txIDs)
	if err != nil {
		return in, fmt.Errorf("read consumed recall sources: %w", err)
	}
	in.Consumed = consumed
	return in, nil
}

// rejectTruncatedRecall:不允许把分页误报成完整集合,更不允许对不完整集合执行回收。
func rejectTruncatedRecall(ctx context.Context, svcCtx *svc.ServiceContext, req *BatchRecallReq, candidateTotal uint64, returned int) (*BatchRecallResp, error) {
	resp := &BatchRecallResp{
		ErrorCode:    constants.ErrCodeResultTruncated,
		TotalMatched: clampUint32(candidateTotal),
		TotalFailed:  clampUint32(candidateTotal),
		RequestID:    req.RequestID,
	}
	// 预览不写库。
	if !req.DryRun {
		if err := svcCtx.SnapshotStore.InsertAuditLog(ctx, &store.AuditLogRow{
			RollbackType:    recallAuditType,
			TargetTime:      req.TimeStart,
			PlayersAffected: 0,
			PlayersFailed:   resp.TotalFailed,
			Reason: fmt.Sprintf("batch_recall REJECTED (TRUNCATED): %s (candidate_total=%d returned=%d limit=%d item=%d currency=%d)",
				req.Reason, candidateTotal, returned, batchRecallQueryLimit, req.ItemConfigID, req.CurrencyType),
			Operator:  req.Operator,
			CreatedAt: uint64(time.Now().Unix()),
		}); err != nil {
			resp.ErrorCode = constants.ErrCodeSnapshotDBError
			return resp, fmt.Errorf("write truncated batch recall audit: %w", err)
		}
	}
	logx.Errorf("[BatchRecall] REJECTED: query result truncated candidate_total=%d returned=%d limit=%d operator=%s",
		candidateTotal, returned, batchRecallQueryLimit, req.Operator)
	return resp, nil
}

// auditRecallRejection 给拒绝留痕:运营确实发起过一次回收,这个事实要留下来。players_affected 恒为 0,审计表不替没发生的回收背书。
func auditRecallRejection(ctx context.Context, svcCtx *svc.ServiceContext, req *BatchRecallReq, verdict string) error {
	if err := svcCtx.SnapshotStore.InsertAuditLog(ctx, &store.AuditLogRow{
		RollbackType: recallAuditType,
		TargetTime:   req.TimeStart,
		Reason: fmt.Sprintf("batch_recall REJECTED (%s): %s (request_id=%d item=%d currency=%d)",
			verdict, req.Reason, req.RequestID, req.ItemConfigID, req.CurrencyType),
		Operator:  req.Operator,
		CreatedAt: uint64(time.Now().Unix()),
	}); err != nil {
		return fmt.Errorf("write batch recall audit: %w", err)
	}
	return nil
}

func rejectRecallPlan(ctx context.Context, svcCtx *svc.ServiceContext, req *BatchRecallReq, candidateTotal uint64, code uint32, verdict string) (*BatchRecallResp, error) {
	resp := &BatchRecallResp{ErrorCode: code, TotalMatched: clampUint32(candidateTotal), RequestID: req.RequestID}
	if req.DryRun {
		return resp, nil
	}
	if err := auditRecallRejection(ctx, svcCtx, req, verdict); err != nil {
		resp.ErrorCode = constants.ErrCodeSnapshotDBError
		return resp, err
	}
	return resp, nil
}

func previewRecallResp(candidateTotal uint64, plan *recallPlan) *BatchRecallResp {
	resp := &BatchRecallResp{
		TotalMatched:    clampUint32(candidateTotal),
		PlanDigest:      plan.DigestHex(),
		AmountRequested: plan.AmountRequested,
		Results:         make([]RecallResult, 0, len(plan.Ops)),
		NeedsReview:     make([]RecallReviewResult, 0, len(plan.Reviews)),
	}
	for _, op := range plan.Ops {
		resp.Results = append(resp.Results, RecallResult{PlayerID: op.PlayerID, ItemConfigID: op.ItemConfigID, CurrencyType: op.CurrencyType, Amount: op.Requested})
	}
	for _, review := range plan.Reviews {
		row := review.Row
		amount := uint64(row.ItemQuantity)
		if row.ItemConfigID == 0 {
			amount = uint64(row.CurrencyDelta) // 进得了待核对的都是收益行,货币 delta 恒为正
		}
		resp.NeedsReview = append(resp.NeedsReview, RecallReviewResult{PlayerID: row.ToPlayer, TxID: row.TxID, CorrelationID: row.CorrelationID,
			TxType: row.TxType, Amount: amount, ItemUUID: row.ItemUUID, Timestamp: row.Timestamp, Reason: review.Reason})
	}
	return resp
}

// recallBundleOf 是这一行指令的载荷。签名和收据身份都按它算,所以要确定性序列化。
func recallBundleOf(op *plannedRecallOp, kind dspb.RecallKind) ([]byte, error) {
	bundle := &assetpb.AssetBundle{}
	if kind == dspb.RecallKind_RECALL_KIND_CURRENCY {
		bundle.Currencies = []*assetpb.CurrencyAmount{{CurrencyType: op.CurrencyType, Amount: op.Requested}}
	} else {
		bundle.Items = []*assetpb.ItemGrant{{ConfigId: op.ItemConfigID, Count: uint32(op.Requested)}}
		bundle.ItemUuids = op.Hints
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(bundle)
}

// freezeRecallPlan 是提交点:批次、全部行、消费的流水、受理审计一个事务。提交之后计划不再变。
func freezeRecallPlan(ctx context.Context, svcCtx *svc.ServiceContext, req *BatchRecallReq, plan *recallPlan) (*BatchRecallResp, error) {
	if svcCtx.RecallOpIDs == nil {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError}, errors.New("recall op id source is unavailable")
	}
	ops := make([]*store.RecallOpPlan, 0, len(plan.Ops))
	for _, op := range plan.Ops {
		opID, err := svcCtx.RecallOpIDs.Next(ctx)
		if err != nil {
			return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError}, fmt.Errorf("mint recall op id: %w", err)
		}
		payload, err := recallBundleOf(op, req.Kind)
		if err != nil {
			return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError}, fmt.Errorf("encode recall bundle: %w", err)
		}
		ops = append(ops, &store.RecallOpPlan{OpID: opID, PlayerID: op.PlayerID, Kind: uint32(req.Kind), CurrencyType: op.CurrencyType,
			ItemConfigID: op.ItemConfigID, Requested: op.Requested, Payload: payload, SourceTxIDs: op.SourceTxIDs})
	}
	batch := &store.RecallBatchRow{
		RequestID: req.RequestID, RequestDigest: req.filter().digest(), PlanDigest: plan.Digest,
		Kind: uint32(req.Kind), ItemConfigID: req.ItemConfigID, CurrencyType: req.CurrencyType,
		TimeStart: req.TimeStart, TimeEnd: req.TimeEnd, Operator: req.Operator, Reason: req.Reason, PeerAddr: req.PeerAddr,
		PlayerCount: plan.PlayerCount, OpCount: uint32(len(ops)), SourceRows: plan.SourceRows, AmountRequested: plan.AmountRequested,
	}
	err := svcCtx.Recall.CreateBatch(ctx, batch, ops, uint64(time.Now().UnixMilli()))
	switch {
	case errors.Is(err, store.ErrRecallBatchExists):
		// 同一 request_id 并发:对方已经提交,按重放回现状。
		resp, _, readErr := replayRecallBatch(ctx, svcCtx, req.RequestID, req.filter(), req.ExpectedPlanDigest)
		if resp == nil {
			return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError}, errors.Join(err, readErr)
		}
		return resp, readErr
	case errors.Is(err, store.ErrRecallSourceConsumed):
		// 另一批抢先消费了同一条流水。事务已回滚:重新预览,摘要自然就不一样了。
		logx.Errorf("[BatchRecall] REJECTED: a source transaction was consumed concurrently request_id=%d operator=%s", req.RequestID, req.Operator)
		return &BatchRecallResp{ErrorCode: constants.ErrCodeRecallPlanChanged, RequestID: req.RequestID}, nil
	case err != nil:
		return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError}, fmt.Errorf("freeze recall plan: %w", err)
	}
	logx.Infof("[BatchRecall] plan frozen request_id=%d players=%d ops=%d sources=%d operator=%s peer=%s",
		req.RequestID, plan.PlayerCount, len(ops), plan.SourceRows, req.Operator, req.PeerAddr)
	stored, err := svcCtx.Recall.ReadBatch(ctx, req.RequestID)
	if err == nil {
		var resp *BatchRecallResp
		if resp, err = recallBatchResp(ctx, svcCtx, stored); err == nil {
			return resp, nil
		}
	}
	// 已经提交了,只是读不回来。不能回失败让人以为没受理:回 Pending,进度用 GetRecallBatch 查。
	logx.Errorf("[BatchRecall] read back failed after commit request_id=%d: %v", req.RequestID, err)
	return &BatchRecallResp{ErrorCode: constants.ErrCodeRecallPending, RequestID: req.RequestID, PlanDigest: plan.DigestHex(),
		TotalMatched: plan.SourceRows, TotalPending: uint32(len(ops)), AmountRequested: plan.AmountRequested}, nil
}

// recallBatchResp 把批次现状翻成响应。ErrorCode 只有全部全额回收才是 0。
func recallBatchResp(ctx context.Context, svcCtx *svc.ServiceContext, batch *store.RecallBatchRow) (*BatchRecallResp, error) {
	ops, err := svcCtx.Recall.ListOps(ctx, batch.RequestID)
	if err != nil {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError}, fmt.Errorf("list recall ops: %w", err)
	}
	resp := &BatchRecallResp{
		TotalMatched:    batch.SourceRows,
		TotalRecalled:   batch.OpsComplete,
		TotalFailed:     batch.OpsShortfall + batch.OpsFailed,
		TotalPending:    batch.OpenOps,
		PlanDigest:      hex.EncodeToString(batch.PlanDigest),
		RequestID:       batch.RequestID,
		BatchState:      batch.State,
		AmountRequested: batch.AmountRequested,
		AmountTaken:     batch.AmountTaken,
		AmountDebt:      batch.AmountDebt,
		AmountShortfall: batch.AmountShortfall,
		CancelledMs:     batch.CancelledMs,
		Results:         make([]RecallResult, 0, len(ops)),
	}
	switch {
	case batch.OpenOps > 0:
		resp.ErrorCode = constants.ErrCodeRecallPending
	case resp.TotalFailed > 0:
		resp.ErrorCode = constants.ErrCodeRecallIncomplete
	default:
		resp.ErrorCode = constants.ErrCodeOK
	}
	for _, op := range ops {
		resp.Results = append(resp.Results, recallResultOf(op))
	}
	return resp, nil
}

// recallResultOf 只有 scene 已提交且全额兜底才算成功。
func recallResultOf(op *store.RecallOpRow) RecallResult {
	result := RecallResult{PlayerID: op.PlayerID, ItemConfigID: op.ItemConfigID, CurrencyType: op.CurrencyType, Amount: op.Requested,
		OpID: op.OpID, Status: op.Status, Taken: op.Taken, Debt: op.Debt, Shortfall: op.Shortfall}
	label := store.RecallStatusLabel(op.Status)
	switch {
	case op.IsOpen():
		result.ErrorDetail = fmt.Sprintf("%s: not settled yet, attempts=%d last_reason=%d", label, op.Attempts, op.LastReason)
	case op.IsApplied() && op.Shortfall == 0:
		result.Success = true
	case op.IsApplied():
		result.ErrorDetail = fmt.Sprintf("%s: shortfall=%d", label, op.Shortfall)
	case op.IsPartial():
		// taken 只是下限:scene 报部分应用时是确认过的那部分,人工关成 PARTIAL(ResolveRecallOp)时一律记 0。实际扣了多少只认
		// 已提交账本里这一行的回收收据(correlation_id = op_id,明细在 AssetRecallLine);流水只是线索,扣过的可能没有流水,
		// 照「流水里没有」去补挂欠款会多扣。
		result.ErrorDetail = fmt.Sprintf("%s: taken=%d is only a lower bound and a manually resolved PARTIAL records taken=0; "+
			"read the GM_RECALL receipt with correlation_id=%d via GetPlayerAssetOpLedger(player_id=%d) before any follow-up, "+
			"and never add debt because transaction_log has no matching row", label, op.Taken, op.OpID, op.PlayerID)
	default:
		result.ErrorDetail = fmt.Sprintf("%s: nothing recalled, reason=%d", label, op.LastReason)
	}
	return result
}

// GetRecallBatch 查一批的现状。
func GetRecallBatch(ctx context.Context, svcCtx *svc.ServiceContext, requestID uint64) (*BatchRecallResp, error) {
	if requestID == 0 {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if svcCtx == nil || svcCtx.Recall == nil {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError}, nil
	}
	batch, err := svcCtx.Recall.ReadBatch(ctx, requestID)
	if errors.Is(err, store.ErrRecallBatchNotFound) {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeRecallNotFound, RequestID: requestID}, nil
	}
	if err != nil {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError}, fmt.Errorf("read recall batch: %w", err)
	}
	return recallBatchResp(ctx, svcCtx, batch)
}

// CancelRecallBatch 取消一批,幂等。只拦还没生效的行,回的是取消之后的现状。
func CancelRecallBatch(ctx context.Context, svcCtx *svc.ServiceContext, requestID uint64, operator, reason, peerAddr string) (*BatchRecallResp, error) {
	if requestID == 0 || !validRecallText(operator, recallOperatorMaxRunes) || !validRecallText(reason, recallReasonMaxRunes) {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if svcCtx == nil || svcCtx.Recall == nil {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError}, nil
	}
	err := svcCtx.Recall.CancelBatch(ctx, requestID, operator, reason, uint64(time.Now().UnixMilli()))
	if errors.Is(err, store.ErrRecallBatchNotFound) {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeRecallNotFound, RequestID: requestID}, nil
	}
	if err != nil {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError}, fmt.Errorf("cancel recall batch: %w", err)
	}
	logx.Infof("[BatchRecall] batch cancelled request_id=%d operator=%s peer=%s reason=%s", requestID, operator, peerAddr, reason)
	return GetRecallBatch(ctx, svcCtx, requestID)
}

// ── ResolveRecallOp ────────────────────────────────────────────
//
// 人工终结一行卡死的回收指令(scene 反复回 UNKNOWN,或一直投不出去)。原来只凭运维查流水选结局,接口不查证据;
// 选 REJECTED / ABORTED 时 settle 会把这一行的流水从 gm_recall_source 放回去,scene 其实扣过的话下一批会再扣一遍。
//
// 证据只认这名玩家已提交的资产账本,判据与 trade、guild 的 assetopfix 同一份(assetop.JudgeManual);流水只是线索:
// 提交之后发送失败、提交成功而回包丢失时崩溃,已提交的扣除都可能没有流水。放行按回收自己的结局对表(recallManualAllows)。

// RecallResolveResult 是一次人工终结的回答。
type RecallResolveResult struct {
	ErrorCode uint32
	// Resolved:本次终结了这一行。
	Resolved bool
	// Status 是这一行的 GmRecallOpStatus:终结了是新状态;行不是 PENDING 时是读到的现状;其余是读证据时的 PENDING。
	Status uint32
	// Evidence 是证据说明(recallEvidenceOf),读过已提交账本才有。
	Evidence string
}

// recallManualStatuses 是 ResolveRecallOp 收的三种 final_status(store.RecallManualFinal 的定义域),给证据说明列可选项用。
var recallManualStatuses = [...]uint32{
	uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_REJECTED),
	uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_ABORTED),
	uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_PARTIAL),
}

// recallManualAllows 是回收的人工结局对表。与 trade、guild 的 ManualVerdict.Allows 不同,理由如下:
//   - rejected(账本已拒绝,收据是这一条请求):REJECTED 或 ABORTED。scene 判了拒,资产没动,两种都把来源放回去;
//   - never_applied(写权行已释放、账本没见过、行安静):只能 ABORTED。scene 没见过这个 seq,拒绝不是它的判定;
//     读完之后没人领过、发过,由终结的见证保证;
//   - applied、partial(账本已应用或部分应用):一律不收。这种行由重投循环按收据里的回收明细(requested / taken /
//     debt / shortfall)终结;人工没有明细,settle 只能把整行记成缺口,taken、debt 都会记错;
//   - none(账本判不了):只能 PARTIAL。回收的 PARTIAL 不放回来源:scene 其实扣过,来源仍是已消费,下一批不会再扣;
//     没扣过,只是少回收一次。trade、guild 判不了时一律不终结,因为它们的 applied 要入账、aborted 要退款,
//     判错任一边都多给或少给玩家,没有只往一个方向错的关单;回收错了只会少扣,所以给一个保守关单。
func recallManualAllows(v assetop.ManualVerdict, final assetop.Status) bool {
	switch v {
	case assetop.ManualVerdictRejected:
		return final == assetop.StatusRejected || final == assetop.StatusAborted
	case assetop.ManualVerdictNeverApplied:
		return final == assetop.StatusAborted
	case assetop.ManualVerdictNone:
		return final == assetop.StatusAppliedPartial
	default:
		return false
	}
}

// recallEvidenceOf 是回给调用方、写进审计行的证据说明:先写回收这条证据下能收哪些 final_status(recall_accepts),
// 再接共享的那一句(assetop.DescribeManualEvidence,与 assetopfix 同一份文案)。共享句里的 "allowed=-as" 是两个
// assetopfix 工具的 -as 选项,ResolveRecallOp 能收什么以 recall_accepts 为准。
func recallEvidenceOf(v assetop.ManualVerdict, detail string) string {
	accepts := make([]string, 0, len(recallManualStatuses))
	for _, status := range recallManualStatuses {
		if final, ok := store.RecallManualFinal(status); ok && recallManualAllows(v, final) {
			accepts = append(accepts, store.RecallStatusLabel(status))
		}
	}
	label := strings.Join(accepts, "|")
	if label == "" {
		label = "none (the redelivery loop finalizes it from the receipt's recall line)"
	}
	return fmt.Sprintf("recall_accepts=%s; %s", label, assetop.DescribeManualEvidence(v, detail))
}

// ResolveRecallOp 人工终结一行卡死的回收指令。步骤:
//  1. 读这一行。只收 PENDING:已终结的、还没派发的(PLANNED,用 CancelRecallBatch)回现状,什么都不改。
//     请求与 Claim 下发的同一套还原(store.RecallStore.ReadOpState),载荷解不开按证据不足;
//  2. 读这名玩家已提交的账本(GetPlayerStoreLedger,写权行与账本同一次读)。读失败不终结,回故障码;
//  3. 判证据(assetop.JudgeManual,安静间隔 assetop.ManualQuietGap(svc.RecallLeaseDuration())),按 recallManualAllows 放行;
//  4. 带读行时的租约与发送时刻终结(store.RecallStore.ResolveManuallyIfQuiet,与重投循环同一个 settle)。
//     读证据之后这一行被领过、发过或已终结,终结落空,回 ErrCodeRecallOpChanged,重新调用即重新读证据。
func ResolveRecallOp(ctx context.Context, svcCtx *svc.ServiceContext, opID uint64, final uint32, operator, reason string) (RecallResolveResult, error) {
	return resolveRecallOp(ctx, svcCtx, opID, final, operator, reason, time.Now)
}

// resolveRecallOp 是 ResolveRecallOp 的实现,时钟显式传入:判「安静」与终结时刻用同一个。
func resolveRecallOp(ctx context.Context, svcCtx *svc.ServiceContext, opID uint64, final uint32, operator, reason string, now func() time.Time) (RecallResolveResult, error) {
	if opID == 0 || !validRecallText(operator, recallOperatorMaxRunes) || !validRecallText(reason, recallReasonMaxRunes) {
		return RecallResolveResult{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if svcCtx == nil || svcCtx.Recall == nil {
		return RecallResolveResult{ErrorCode: constants.ErrCodeSnapshotDBError}, nil
	}
	status, ok := store.RecallManualFinal(final)
	if !ok {
		return RecallResolveResult{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	row, err := svcCtx.Recall.ReadOpState(ctx, opID)
	if errors.Is(err, store.ErrRecallOpNotFound) {
		return RecallResolveResult{ErrorCode: constants.ErrCodeRecallNotFound}, nil
	}
	if err != nil {
		return RecallResolveResult{ErrorCode: constants.ErrCodeSnapshotDBError}, fmt.Errorf("read recall op %d: %w", opID, err)
	}
	if !row.IsPending() {
		logx.Infof("[BatchRecall] manual resolve skipped: op_id=%d is %s, not pending", opID, store.RecallStatusLabel(row.Status))
		return RecallResolveResult{ErrorCode: constants.ErrCodeOK, Status: row.Status}, nil
	}

	nowMs := uint64(now().UnixMilli())
	// 账本在行之后读:「没见过」和「已释放」是同一次读的事实,读完之后的变化由终结的见证兜住。
	ledger, err := GetPlayerStoreLedger(ctx, svcCtx, row.Op.PlayerID)
	if err != nil {
		return RecallResolveResult{ErrorCode: constants.ErrCodeSnapshotDBError, Status: row.Status},
			fmt.Errorf("read committed asset ledger of player %d for recall op %d: %w", row.Op.PlayerID, opID, err)
	}
	// Op 里的 LastReason、LastSendMs 就是行上的这两列(ReadOpState 同一次扫出)。
	manual := assetop.ManualRow{Op: row.Op, LastReason: row.Op.LastReason, LeaseUntilMs: row.LeaseUntilMs, LastSendMs: row.Op.LastSendMs}
	verdict, detail := judgeRecallOp(row, manual, ledger, nowMs)
	result := RecallResolveResult{Status: row.Status, Evidence: recallEvidenceOf(verdict, detail)}
	if !recallManualAllows(verdict, status) {
		result.ErrorCode = constants.ErrCodeRecallEvidenceRefused
		auditRecallManual(row, final, operator, reason, verdict, result)
		return result, nil
	}

	resolved, err := assetop.ResolveManually(ctx, recallQuietResolver{recall: svcCtx.Recall, witness: manual.Witness()},
		assetop.ManualResolution{OpID: opID, Final: status, Operator: operator, Reason: reason}, nil, now)
	switch {
	case err != nil:
		result.ErrorCode = constants.ErrCodeSnapshotDBError
	case !resolved:
		result.ErrorCode = constants.ErrCodeRecallOpChanged
	default:
		result.ErrorCode, result.Resolved, result.Status = constants.ErrCodeOK, true, final
	}
	auditRecallManual(row, final, operator, reason, verdict, result)
	return result, err
}

// judgeRecallOp 拿已提交账本判这一行(assetop.JudgeManual)。还原不出请求(载荷解不开)就核不了收据,按证据不足处理。
func judgeRecallOp(row store.RecallOpState, manual assetop.ManualRow, ledger PlayerStoreLedger, nowMs uint64) (assetop.ManualVerdict, string) {
	if row.OpErr != nil {
		return assetop.ManualVerdictNone, fmt.Sprintf("cannot rebuild the request to check its receipt: %v", row.OpErr)
	}
	snapshot := assetop.StoreLedger{Found: ledger.Found, Released: ledger.Released, Ledger: ledger.Ledger}
	return assetop.JudgeManual(snapshot, manual, nowMs, assetop.ManualQuietGap(svc.RecallLeaseDuration()))
}

// recallQuietResolver 把 assetop.ResolveManually 接到带见证的结算上(同 trade、guild 的 assetopfix):
// 入参校验与成功日志走包级函数,落库只认读证据时的租约与发送时刻。
type recallQuietResolver struct {
	recall  svc.RecallStore
	witness assetop.QuietWitness
}

func (q recallQuietResolver) ResolveManually(ctx context.Context, r assetop.ManualResolution, nowMs uint64) (bool, error) {
	return q.recall.ResolveManuallyIfQuiet(ctx, r, q.witness, nowMs)
}

// auditRecallManual 打人工终结的审计行:放行、拒绝、落空都打(ERROR 级,确保进日志检索),带操作人、证据结论与来源。
// 操作人和理由按 %q 打:带换行就能伪造出第二条审计行。
func auditRecallManual(row store.RecallOpState, final uint32, operator, reason string, verdict assetop.ManualVerdict, result RecallResolveResult) {
	logx.Errorf("[RecallOpManual] operator=%q op_id=%d player_id=%d seq=%d final=%s reason=%q evidence=%s evidence_source=player_store resolved=%v error_code=%d",
		operator, row.Op.OpID, row.Op.PlayerID, row.Op.Seq, store.RecallStatusLabel(final), reason, verdict, result.Resolved, result.ErrorCode)
}

// ── QueryTransactionLog ────────────────────────────────────────
// Exposes transaction_log queries to GM/CS tools.

type QueryTxLogReq struct {
	PlayerID     uint64
	TimeStart    uint64
	TimeEnd      uint64
	TxTypes      []uint32
	ItemConfigID uint32
	CurrencyType uint32
	Limit        uint32
	Offset       uint64
}

type TxLogRow struct {
	TxID          uint64
	Timestamp     uint64
	TxType        uint32
	FromPlayer    uint64
	ToPlayer      uint64
	ItemUUID      uint64
	ItemConfigID  uint32
	ItemQuantity  uint32
	CurrencyType  uint32
	CurrencyDelta int64
	BalanceBefore uint64
	BalanceAfter  uint64
	CorrelationID uint64
	Extra         string
}

type QueryTxLogResp struct {
	ErrorCode  uint32
	Rows       []TxLogRow
	TotalCount uint32
}

func QueryTransactionLog(ctx context.Context, svcCtx *svc.ServiceContext, req *QueryTxLogReq) (*QueryTxLogResp, error) {
	if req == nil {
		return &QueryTxLogResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if svcCtx == nil || svcCtx.TxLogStore == nil {
		return &QueryTxLogResp{ErrorCode: constants.ErrCodeSnapshotDBError}, nil
	}

	rows, total, err := svcCtx.TxLogStore.QueryLog(ctx, &store.TransactionLogQuery{
		PlayerID:     req.PlayerID,
		TimeStart:    req.TimeStart,
		TimeEnd:      req.TimeEnd,
		TxTypes:      req.TxTypes,
		ItemConfigID: req.ItemConfigID,
		CurrencyType: req.CurrencyType,
		Limit:        req.Limit,
		Offset:       req.Offset,
	})
	if err != nil {
		return &QueryTxLogResp{ErrorCode: constants.ErrCodeSnapshotDBError},
			fmt.Errorf("query transaction_log: %w", err)
	}

	resp := &QueryTxLogResp{
		TotalCount: total,
	}
	for _, r := range rows {
		resp.Rows = append(resp.Rows, TxLogRow{
			TxID:          r.TxID,
			Timestamp:     r.Timestamp,
			TxType:        r.TxType,
			FromPlayer:    r.FromPlayer,
			ToPlayer:      r.ToPlayer,
			ItemUUID:      r.ItemUUID,
			ItemConfigID:  r.ItemConfigID,
			ItemQuantity:  r.ItemQuantity,
			CurrencyType:  r.CurrencyType,
			CurrencyDelta: r.CurrencyDelta,
			BalanceBefore: r.BalanceBefore,
			BalanceAfter:  r.BalanceAfter,
			CorrelationID: r.CorrelationID,
			Extra:         r.Extra,
		})
	}

	return resp, nil
}

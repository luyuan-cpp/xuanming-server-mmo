package logic

import (
	"context"
	"fmt"
	"time"

	"data_service/internal/constants"
	"data_service/internal/store"
	"data_service/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// ── BatchRecallItems ───────────────────────────────────────────
// Queries transaction_log for matching grants, then issues recalls.
// Supports dry_run mode (report-only without modifying data).

type BatchRecallReq struct {
	PlayerIDs    []uint64
	ItemConfigID uint32
	CurrencyType uint32
	TimeStart    uint64
	TimeEnd      uint64
	TxTypes      []uint32
	Reason       string
	Operator     string
	DryRun       bool
}

type RecallResult struct {
	PlayerID     uint64
	ItemUUID     uint64
	ItemConfigID uint32
	Amount       uint64
	Success      bool
	ErrorDetail  string
}

type BatchRecallResp struct {
	ErrorCode     uint32
	TotalMatched  uint32
	TotalRecalled uint32
	TotalFailed   uint32
	Results       []RecallResult
}

const batchRecallQueryLimit uint32 = 10000

func clampUint32(total uint64) uint32 {
	const maxUint32 = ^uint32(0)
	if total > uint64(maxUint32) {
		return maxUint32
	}
	return uint32(total)
}

func BatchRecallItems(ctx context.Context, svcCtx *svc.ServiceContext, req *BatchRecallReq) (*BatchRecallResp, error) {
	if req == nil {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if svcCtx == nil || svcCtx.TxLogStore == nil {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError}, nil
	}
	if !req.DryRun && svcCtx.SnapshotStore == nil {
		// 非 dry-run 即使被 fail-closed 拒绝也必须留下操作审计；审计库不可用时
		// 不能继续并假装这次高风险 GM 操作已被记录。
		return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError},
			fmt.Errorf("batch recall audit store is unavailable")
	}

	if req.TimeStart == 0 || req.TimeEnd == 0 {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if req.ItemConfigID == 0 && req.CurrencyType == 0 {
		return &BatchRecallResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}

	logx.Infof("[BatchRecall] operator=%s reason=%s item_config=%d currency_type=%d dry_run=%v",
		req.Operator, req.Reason, req.ItemConfigID, req.CurrencyType, req.DryRun)

	// Build query for each target player (or all players)
	var allRows []*store.TransactionLogRow
	var candidateTotal uint64
	var truncated bool
	if len(req.PlayerIDs) > 0 {
		for _, pid := range req.PlayerIDs {
			rows, total, err := svcCtx.TxLogStore.QueryLog(ctx, &store.TransactionLogQuery{
				PlayerID:     pid,
				TimeStart:    req.TimeStart,
				TimeEnd:      req.TimeEnd,
				TxTypes:      req.TxTypes,
				ItemConfigID: req.ItemConfigID,
				CurrencyType: req.CurrencyType,
				Limit:        batchRecallQueryLimit,
			})
			if err != nil {
				logx.Errorf("[BatchRecall] query player %d: %v", pid, err)
				// 指定玩家批次不能静默跳过失败查询，否则响应中的 matched/failed
				// 只覆盖成功查到的子集，运营会误以为整批都已核查。
				return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError},
					fmt.Errorf("query transaction_log for player %d: %w", pid, err)
			}
			candidateTotal += uint64(total)
			if uint64(total) != uint64(len(rows)) {
				truncated = true
			}
			allRows = append(allRows, rows...)
		}
	} else {
		rows, total, err := svcCtx.TxLogStore.QueryLog(ctx, &store.TransactionLogQuery{
			TimeStart:    req.TimeStart,
			TimeEnd:      req.TimeEnd,
			TxTypes:      req.TxTypes,
			ItemConfigID: req.ItemConfigID,
			CurrencyType: req.CurrencyType,
			Limit:        batchRecallQueryLimit,
		})
		if err != nil {
			return &BatchRecallResp{ErrorCode: constants.ErrCodeSnapshotDBError},
				fmt.Errorf("query transaction_log: %w", err)
		}
		allRows = rows
		candidateTotal = uint64(total)
		if uint64(total) != uint64(len(rows)) {
			truncated = true
		}
	}

	resp := &BatchRecallResp{
		TotalMatched: clampUint32(candidateTotal),
	}
	if candidateTotal > uint64(^uint32(0)) {
		truncated = true
	}

	if truncated {
		// QueryLog 返回的 total 是筛选候选总数，rows 只是最多 10000 条的分页。
		// 不允许把分页误报成完整集合，更不允许对不完整集合执行回收。
		resp.ErrorCode = constants.ErrCodeResultTruncated
		resp.TotalFailed = clampUint32(candidateTotal)
		if svcCtx.SnapshotStore != nil {
			if err := svcCtx.SnapshotStore.InsertAuditLog(ctx, &store.AuditLogRow{
				RollbackType:    4, // 4 = batch recall
				TargetTime:      req.TimeStart,
				PlayersAffected: 0,
				PlayersFailed:   resp.TotalFailed,
				Reason: fmt.Sprintf("batch_recall REJECTED (TRUNCATED): %s (candidate_total=%d returned=%d limit=%d item=%d currency=%d)",
					req.Reason, candidateTotal, len(allRows), batchRecallQueryLimit, req.ItemConfigID, req.CurrencyType),
				Operator:  req.Operator,
				CreatedAt: uint64(time.Now().Unix()),
			}); err != nil {
				resp.ErrorCode = constants.ErrCodeSnapshotDBError
				return resp, fmt.Errorf("write truncated batch recall audit: %w", err)
			}
		}
		logx.Errorf("[BatchRecall] REJECTED: query result truncated candidate_total=%d returned=%d limit=%d operator=%s",
			candidateTotal, len(allRows), batchRecallQueryLimit, req.Operator)
		return resp, nil
	}

	if req.DryRun {
		for _, row := range allRows {
			amount := uint64(0)
			if row.ItemConfigID > 0 {
				amount = uint64(row.ItemQuantity)
			} else if row.CurrencyDelta > 0 {
				amount = uint64(row.CurrencyDelta)
			}
			playerId := row.ToPlayer
			if playerId == 0 {
				playerId = row.FromPlayer
			}
			resp.Results = append(resp.Results, RecallResult{
				PlayerID:     playerId,
				ItemUUID:     row.ItemUUID,
				ItemConfigID: row.ItemConfigID,
				Amount:       amount,
				Success:      true,
			})
		}
		logx.Infof("[BatchRecall] dry_run complete: %d transactions matched", resp.TotalMatched)
		return resp, nil
	}

	// ── Execute recalls ──────────────────────────────────────────────────
	//
	// 真正的回收尚未实现:货币要走 SavePlayerData 扣减,道具要落一份持久化的
	// pending-recall 意图并由 C++ scene 节点在 load/save 时消费,两者都还不存在
	// (全仓没有任何 pending_recall 写入方,transaction_log 也没有 Go 侧生产者)。
	//
	// 这里刻意 **fail-closed**:把每条匹配记为失败并返回 ErrCodeNotImplemented,
	// 而不是像旧实现那样只置 `result.Success = true` 就 `TotalRecalled++`。
	// 旧行为的危害不是"少做了一件事",而是**主动说谎**:刷金事故后运营看到
	// total_recalled=N、审计表里也写着"已影响 N 名玩家",于是判定事故已处置,
	// 非法资产却一分未动地留在经济系统里。宁可让这次调用明确失败。
	//
	// dry_run 路径不受影响 —— 它本来就只做匹配统计,语义诚实。
	for _, row := range allRows {
		playerId := row.ToPlayer
		if playerId == 0 {
			playerId = row.FromPlayer
		}

		amount := uint64(0)
		if row.ItemConfigID > 0 {
			amount = uint64(row.ItemQuantity)
		} else if row.CurrencyDelta > 0 {
			amount = uint64(row.CurrencyDelta)
		}

		resp.Results = append(resp.Results, RecallResult{
			PlayerID:     playerId,
			ItemUUID:     row.ItemUUID,
			ItemConfigID: row.ItemConfigID,
			Amount:       amount,
			Success:      false,
			ErrorDetail:  "recall execution not implemented: no currency deduction / pending-recall sink exists",
		})
		resp.TotalFailed++
	}

	resp.ErrorCode = constants.ErrCodeNotImplemented

	// 审计日志照写(运营确实发起过一次回收尝试,这个事实要留痕),
	// 但 players_affected 必须是 0 —— 审计表不能替一次没发生的回收背书。
	now := uint64(time.Now().Unix())
	if err := svcCtx.SnapshotStore.InsertAuditLog(ctx, &store.AuditLogRow{
		RollbackType:    4, // 4 = batch recall
		TargetTime:      req.TimeStart,
		PlayersAffected: 0,
		PlayersFailed:   resp.TotalFailed,
		Reason: fmt.Sprintf("batch_recall NOT EXECUTED (unimplemented): %s (item=%d currency=%d)",
			req.Reason, req.ItemConfigID, req.CurrencyType),
		Operator:  req.Operator,
		CreatedAt: now,
	}); err != nil {
		resp.ErrorCode = constants.ErrCodeSnapshotDBError
		logx.Errorf("[BatchRecall] audit write failed: operator=%s matched=%d err=%v",
			req.Operator, resp.TotalMatched, err)
		return resp, fmt.Errorf("write batch recall audit: %w", err)
	}

	logx.Errorf("[BatchRecall] REJECTED: recall execution is not implemented; matched=%d nothing was recalled, operator=%s reason=%s",
		resp.TotalMatched, req.Operator, req.Reason)

	return resp, nil
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

// ── CreateEventSnapshot ────────────────────────────────────────
// Creates a snapshot triggered by a game event (recharge, large tx, etc.).
// Maps SnapshotEventType to internal SnapshotType with descriptive reason.

const (
	snapshotTypeEvent uint32 = 5 // New type: EVENT (beyond existing 0-4)
)

type CreateEventSnapshotReq struct {
	PlayerID    uint64
	EventType   uint32
	EventDetail string
	Operator    string
}

type CreateEventSnapshotResp struct {
	ErrorCode  uint32
	SnapshotID uint64
	CreatedAt  uint64
}

var eventTypeNames = map[uint32]string{
	0: "large_transaction",
	1: "recharge",
	2: "pre_maintenance",
	3: "level_up",
	4: "first_login",
}

func CreateEventSnapshot(ctx context.Context, svcCtx *svc.ServiceContext, req *CreateEventSnapshotReq) (*CreateEventSnapshotResp, error) {
	if req == nil || req.PlayerID == 0 {
		return &CreateEventSnapshotResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}

	eventName := eventTypeNames[req.EventType]
	if eventName == "" {
		eventName = fmt.Sprintf("event_%d", req.EventType)
	}

	reason := fmt.Sprintf("event_snapshot:%s", eventName)
	if req.EventDetail != "" {
		reason += " " + req.EventDetail
	}

	resp, err := CreatePlayerSnapshot(ctx, svcCtx, &CreateSnapshotReq{
		PlayerID:     req.PlayerID,
		SnapshotType: snapshotTypeEvent,
		Reason:       reason,
		Operator:     req.Operator,
	})
	if err != nil {
		code := constants.ErrCodeSnapshotDBError
		if resp != nil {
			code = resp.ErrorCode
		}
		return &CreateEventSnapshotResp{ErrorCode: code}, err
	}
	if resp == nil {
		return &CreateEventSnapshotResp{ErrorCode: constants.ErrCodeSnapshotDBError},
			fmt.Errorf("create event snapshot returned nil response")
	}

	logx.Infof("[EventSnapshot] player=%d event=%s snapshot=%d",
		req.PlayerID, eventName, resp.SnapshotID)

	return &CreateEventSnapshotResp{
		ErrorCode:  resp.ErrorCode,
		SnapshotID: resp.SnapshotID,
		CreatedAt:  resp.CreatedAt,
	}, nil
}

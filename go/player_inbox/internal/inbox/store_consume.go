package inbox

import (
	"context"
	"database/sql"
	"fmt"

	"player_inbox/internal/constants"
	"player_inbox/internal/metrics"

	base "proto/common/base"
	dspb "proto/data_service"
	inboxpb "proto/player_inbox"

	"github.com/zeromicro/go-zero/core/logx"
)

const (
	fetchPathFast   = "fast"
	fetchPathLocked = "locked"

	// ackLabelSettledUnknown:以 UNKNOWN 收尾为 SETTLED_UNKNOWN 的那一类 FINALIZED,单列一个 result 标签。
	ackLabelSettledUnknown = "finalized_settled_unknown"
)

func tipOf(code uint32) *base.TipInfoMessage {
	if code == 0 {
		return nil
	}
	return &base.TipInfoMessage{Id: code}
}

// fenceRejectTip 是围栏拒绝结局对应的 tip(STALE / EPOCH_REQUIRED / EPOCH_AHEAD;其余为 0)。
func fenceRejectTip(v inboxpb.PlayerInboxFenceVerdict) uint32 {
	switch v {
	case inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_STALE:
		return constants.ErrStaleOwner
	case inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_EPOCH_REQUIRED:
		return constants.ErrEpochRequired
	case inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_EPOCH_AHEAD:
		return constants.ErrEpochAhead
	default:
		return 0
	}
}

// readEpochBound 在事务外读一次 R。失败 = 故障(kPlayerInboxStoreUnavailable),不改任何状态。
func (s *Store) readEpochBound(ctx context.Context, playerID uint64) (uint64, error) {
	readCtx, cancel := context.WithTimeout(ctx, constants.OwnerEpochReadTimeout)
	defer cancel()
	r, err := s.epochs.OwnerEpoch(readCtx, playerID)
	if err != nil {
		return 0, fmt.Errorf("inbox: read owner_epoch player=%d: %w", playerID, err)
	}
	return r, nil
}

// reportFenceReject 记录围栏拒绝的日志与指标。STALE 返回前再读一次 R 做分类(设计 §7.8):
// R ≤ req 记 fence_regressed(critical),否则 zombie;分类读失败只记 unclassified,不改变 STALE 本身。
//
// 每条日志都带发送方身份(评审 L10):peer = gRPC 对端地址(不可伪造),scene_node_id = 请求自报的节点号
// (只用于日志,0 = 旧版 scene 没填)。告警描述让值班按它们定位是哪台 scene 发的。
func (s *Store) reportFenceReject(ctx context.Context, rpc string, v inboxpb.PlayerInboxFenceVerdict,
	playerID uint64, domain inboxpb.PlayerInboxDomain, req, fence uint64, sceneNodeID uint32) {
	log := logx.WithContext(ctx)
	peerAddr := PeerAddr(ctx)
	switch v {
	case inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_STALE:
		kind := StaleUnclassified
		if r, err := s.readEpochBound(ctx, playerID); err == nil {
			kind = ClassifyStale(req, r)
		}
		if kind == StaleFenceRegressed {
			metrics.IncFenceRegressed()
			log.Errorf("[player_inbox] %s STALE classified fence_regressed player=%d domain=%d req_epoch=%d fence=%d "+
				"peer=%s scene_node_id=%d: core Redis owner_epoch went backwards; run epoch-raise per db-write-fencing-mysql.md §9.1 "+
				"(inbox max_fence_epoch is a max source); never reset the fence",
				rpc, playerID, domain, req, fence, peerAddr, sceneNodeID)
			return
		}
		log.Infof("[player_inbox] %s STALE (%s) player=%d domain=%d req_epoch=%d fence=%d peer=%s scene_node_id=%d",
			rpc, kind, playerID, domain, req, fence, peerAddr, sceneNodeID)
	case inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_EPOCH_AHEAD:
		metrics.IncEpochAhead()
		log.Errorf("[player_inbox] %s EPOCH_AHEAD player=%d domain=%d req_epoch=%d fence=%d peer=%s scene_node_id=%d: "+
			"request epoch exceeds core Redis owner_epoch (defect or forgery); no state changed",
			rpc, playerID, domain, req, fence, peerAddr, sceneNodeID)
	case inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_EPOCH_REQUIRED:
		log.Errorf("[player_inbox] %s EPOCH_REQUIRED player=%d domain=%d peer=%s scene_node_id=%d: owner_epoch=0 rejected (fail-closed)",
			rpc, playerID, domain, peerAddr, sceneNodeID)
	}
}

// Fetch在同P内核完整已登记入场，再读stream并按seq投递。空流也不能绕过原件授权。
// SQL stream高水位只升；Redis仅参与诊断。CursorAhead仍只抬next_seq，不重放旧业务效果。
func (s *Store) Fetch(ctx context.Context, req *inboxpb.FetchPlayerInboxRequest) (*inboxpb.FetchPlayerInboxResponse, error) {
	resp := &inboxpb.FetchPlayerInboxResponse{
		PlayerId: req.GetPlayerId(), Domain: req.GetDomain(), CorrelationId: req.GetCorrelationId(),
	}
	domain := req.GetDomain()
	dl := DomainLabel(domain)
	if _, ok := s.registry.Domain(domain); !ok || req.GetPlayerId() == 0 {
		resp.ErrorMessage = tipOf(constants.ErrInvalidParameter)
		return resp, nil
	}
	reqEpoch := req.GetOwnerEpoch()
	if reqEpoch == 0 {
		resp.Fence = inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_EPOCH_REQUIRED
		resp.ErrorMessage = tipOf(constants.ErrEpochRequired)
		s.reportFenceReject(ctx, "Fetch", resp.Fence, req.GetPlayerId(), domain, reqEpoch, 0, req.GetSceneNodeId())
		metrics.ObserveFetch(dl, FenceLabel(resp.Fence), fetchPathFast)
		return resp, nil
	}

	playerID := req.GetPlayerId()
	shard := RowShard(playerID)
	if req.GetAdmission().GetPlayerId() != playerID || req.GetAdmission().GetOwnerEpoch() != reqEpoch {
		return nil, ErrOriginalRole
	}
	decidedThrough := req.GetConsumerDecidedThrough()

	// ── 加锁路径 ──
	maxItems := clampFetchItems(req.GetMaxItems(), constants.FetchDefaultItems, constants.FetchMaxItems)
	var (
		verdict     inboxpb.PlayerInboxFenceVerdict
		lockedFence uint64
		items       []*inboxpb.PlayerInboxItem
		hasMore     bool
		cursorAhead bool
		raisedTo    uint64
		scanBound   uint64
		scanWhole   bool
	)
	err := s.withPlayerTx(ctx, playerID, func(tx *sql.Tx, original *dspb.AccountRoleOwnership) error {
		verdict, lockedFence, items, hasMore, cursorAhead, raisedTo, scanBound, scanWhole =
			inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_UNSPECIFIED, 0, nil, false, false, 0, 0, false
		if err := s.roles.VerifyAdmission(ctx, tx, original, req.GetAdmission()); err != nil {
			return err
		}
		if err := requirePendingOriginalsTx(ctx, tx, original); err != nil {
			return err
		}
		st, err := lockStream(ctx, tx, shard, playerID, domain)
		if err != nil {
			return err
		}
		if st == nil {
			verdict = inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_NO_STREAM
			return nil
		}
		lockedFence = st.GetFenceEpoch()
		// 完整SQL入场原件已经授权该E；Redis只供诊断，不允许历史Redis快照重写授权上界。
		verdict = DecideFence(reqEpoch, lockedFence, reqEpoch)
		if !FenceAllows(verdict) {
			return nil // 不写任何东西
		}

		nowMs := s.nowMs()
		nextSeq, raised := RaiseNextSeq(st.GetNextSeq(), decidedThrough)
		cursorAhead, raisedTo = raised, nextSeq

		pending, err := queryItems(ctx, tx, "SELECT "+itemColumns+" FROM "+itemTable+
			" WHERE `shard` = ? AND `player_id` = ? AND `domain` = ? AND `state` = ? ORDER BY `seq` ASC LIMIT ?",
			shard, playerID, uint32(domain), uint32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING), maxItems+1)
		if err != nil {
			return fmt.Errorf("inbox: scan pending player=%d domain=%d: %w", playerID, domain, err)
		}
		scanBound, scanWhole = fetchScanBound(pending, maxItems+1)
		pending, hasMore = splitPage(pending, maxItems)
		for _, row := range pending {
			if err = requireItemOriginal(row, original); err != nil {
				return err
			}
			upd, err := tx.ExecContext(ctx, "UPDATE "+itemTable+
				" SET `delivery_count` = `delivery_count` + 1, `last_delivered_epoch` = ?, `last_delivered_ms` = ?, `updated_ms` = ?"+
				" WHERE "+itemPK+" AND `state` = ?",
				reqEpoch, nowMs, nowMs, shard, playerID, uint32(domain), row.GetSeq(),
				uint32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING))
			if err != nil {
				return fmt.Errorf("inbox: mark delivered player=%d seq=%d: %w", playerID, row.GetSeq(), err)
			}
			if err := requireOneRow(upd, "mark delivered"); err != nil {
				return err
			}
			items = append(items, deliveredItem(row))
		}
		if write, newFence, newNextSeq := fetchStreamUpdate(verdict, lockedFence, reqEpoch, nextSeq, raised); write {
			upd, err := tx.ExecContext(ctx, "UPDATE "+streamTable+
				" SET `fence_epoch` = ?, `next_seq` = ?, `updated_ms` = ? WHERE "+streamPK,
				newFence, newNextSeq, nowMs, shard, playerID, uint32(domain))
			if err != nil {
				return fmt.Errorf("inbox: write fence player=%d: %w", playerID, err)
			}
			if err := requireOneRow(upd, "write fence"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	resp.Fence = verdict
	if verdict == inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_NO_STREAM {
		metrics.ObserveFetch(dl, FenceLabel(verdict), fetchPathFast)
		return resp, nil
	}
	if !FenceAllows(verdict) {
		resp.ErrorMessage = tipOf(fenceRejectTip(verdict))
		s.reportFenceReject(ctx, "Fetch", verdict, playerID, domain, reqEpoch, lockedFence, req.GetSceneNodeId())
		metrics.ObserveFetch(dl, FenceLabel(verdict), fetchPathLocked)
		return resp, nil
	}
	resp.Items = items
	resp.HasMore = hasMore
	metrics.ObserveFetch(dl, FenceLabel(verdict), fetchPathLocked)
	log := logx.WithContext(ctx)
	// 扫描行数在提交之后单独数(只读、不持锁);失败只少一个样本,不影响本次投递。
	if scanned, err := s.countScannedRows(ctx, shard, playerID, domain, scanBound, scanWhole); err != nil {
		log.Infof("[player_inbox] fetch_scanned_rows sample skipped player=%d domain=%d: %v", playerID, domain, err)
	} else {
		metrics.ObserveFetchScanned(scanned)
	}
	if cursorAhead {
		metrics.IncCursorAhead()
		log.Errorf("[player_inbox] CursorAhead player=%d domain=%d consumer_decided_through=%d raised next_seq to %d: "+
			"inbox DB was restored from backup; old rows below the watermark are reconciled by IB-9 replay checks",
			playerID, domain, decidedThrough, raisedTo)
	}
	for _, it := range items {
		if it.GetDeliveryCount() >= constants.DeliveryWarnThreshold {
			log.Errorf("[player_inbox] Item delivered %d times without ack player=%d domain=%d seq=%d msg_type=%d scene_node_id=%d: "+
				"possible poison message; consider VoidItem(QUARANTINED)",
				it.GetDeliveryCount(), playerID, domain, it.GetSeq(), it.GetMsgType(), req.GetSceneNodeId())
		}
	}
	return resp, nil
}

// deliveredItem 把一行折成 Fetch 回包条目。directive != NONE 时**不带载荷**:作废路径不解码载荷
// (即使载荷会让 scene 崩溃,作废也不会再触发崩溃,设计 §7.9)。delivery_count 含本次。
func deliveredItem(row *inboxpb.PlayerInboxItemRow) *inboxpb.PlayerInboxItem {
	it := &inboxpb.PlayerInboxItem{
		Seq:           row.GetSeq(),
		MsgType:       inboxpb.PlayerInboxMessageType(row.GetMsgType()),
		Directive:     inboxpb.PlayerInboxDirective(row.GetDirective()),
		VoidReason:    inboxpb.PlayerInboxVoidReason(row.GetVoidReason()),
		RequestId:     row.GetRequestId(),
		DeliveryCount: row.GetDeliveryCount() + 1,
		CreatedMs:     row.GetCreatedMs(),
	}
	if it.Directive == inboxpb.PlayerInboxDirective_PLAYER_INBOX_DIRECTIVE_NONE {
		it.Payload = row.GetPayload()
	}
	return it
}

// countScannedRows 数锁路径 PENDING 扫描实际走过的行(范围见 fetchScanBound):主键前缀 + seq ≤ bound,
// whole 时整条流。只走主键索引、不回表,是设计 §4.2「超出数百行就加 (shard,player_id,domain,state,seq) 索引」的依据。
func (s *Store) countScannedRows(ctx context.Context, shard uint32, playerID uint64, domain inboxpb.PlayerInboxDomain,
	bound uint64, whole bool) (int, error) {
	query := "SELECT COUNT(*) FROM " + itemTable + " WHERE `shard` = ? AND `player_id` = ? AND `domain` = ?"
	args := []any{shard, playerID, uint32(domain)}
	if !whole {
		query += " AND `seq` <= ?"
		args = append(args, bound)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("inbox: count scanned rows player=%d domain=%d: %w", playerID, domain, err)
	}
	return n, nil
}

// Ack在同P内核完整原入场与条目owner后，登记消费方已落盘结局。
// 按seq逐项判定并条件终结，open_count与SQL fence_epoch同事务提交。
func (s *Store) Ack(ctx context.Context, req *inboxpb.AckPlayerInboxRequest) (*inboxpb.AckPlayerInboxResponse, error) {
	resp := &inboxpb.AckPlayerInboxResponse{
		PlayerId: req.GetPlayerId(), Domain: req.GetDomain(), CorrelationId: req.GetCorrelationId(),
	}
	domain := req.GetDomain()
	dl := DomainLabel(domain)
	if _, ok := s.registry.Domain(domain); !ok || req.GetPlayerId() == 0 ||
		!validateAckReports(req.GetDecisions(), constants.AckMaxDecisions) {
		resp.ErrorMessage = tipOf(constants.ErrInvalidParameter)
		return resp, nil
	}
	reqEpoch := req.GetOwnerEpoch()
	playerID := req.GetPlayerId()
	if reqEpoch == 0 {
		resp.Fence = inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_EPOCH_REQUIRED
		resp.ErrorMessage = tipOf(constants.ErrEpochRequired)
		s.reportFenceReject(ctx, "Ack", resp.Fence, playerID, domain, reqEpoch, 0, req.GetSceneNodeId())
		metrics.ObserveAck(dl, FenceLabel(resp.Fence))
		return resp, nil
	}

	shard := RowShard(playerID)
	if req.GetAdmission().GetPlayerId() != playerID || req.GetAdmission().GetOwnerEpoch() != reqEpoch {
		return nil, ErrOriginalRole
	}

	seqs := make([]uint64, 0, len(req.GetDecisions()))
	for _, d := range req.GetDecisions() {
		seqs = append(seqs, d.GetSeq())
	}
	var (
		verdict     inboxpb.PlayerInboxFenceVerdict
		lockedFence uint64
		results     []*inboxpb.PlayerInboxAckItem
	)
	err := s.withPlayerTx(ctx, playerID, func(tx *sql.Tx, original *dspb.AccountRoleOwnership) error {
		verdict, lockedFence, results = inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_UNSPECIFIED, 0, nil
		if err := s.roles.VerifyAdmission(ctx, tx, original, req.GetAdmission()); err != nil {
			return err
		}
		if err := requirePendingOriginalsTx(ctx, tx, original); err != nil {
			return err
		}
		st, err := lockStream(ctx, tx, shard, playerID, domain)
		if err != nil {
			return err
		}
		if st == nil {
			verdict = inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_NO_STREAM
			for _, report := range req.GetDecisions() {
				results = append(results, &inboxpb.PlayerInboxAckItem{Seq: report.GetSeq(), Result: inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_UNKNOWN_SEQ})
			}
			return nil
		}
		lockedFence = st.GetFenceEpoch()
		verdict = DecideFence(reqEpoch, lockedFence, reqEpoch)
		if !FenceAllows(verdict) {
			return nil
		}
		rows, err := readItemsBySeq(ctx, tx, shard, playerID, domain, seqs)
		if err != nil {
			return err
		}
		nowMs := s.nowMs()
		var finalized uint32
		for _, report := range req.GetDecisions() {
			if err = requireItemOriginal(rows[report.GetSeq()], original); err != nil {
				return err
			}
			d := decideAck(rows[report.GetSeq()], report)
			// Battle不能把未知、拒绝或跳过变成销账；永久原收据必须明确APPLIED。
			if row := rows[report.GetSeq()]; row != nil && row.GetMsgType() == uint32(inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT) && report.GetOutcome() != inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_APPLIED {
				results = append(results, &inboxpb.PlayerInboxAckItem{Seq: report.GetSeq(), Result: inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_CONFLICT, State: inboxpb.PlayerInboxItemState(row.GetState())})
				continue
			}
			if d.finalize {
				upd, err := tx.ExecContext(ctx, "UPDATE "+itemTable+
					" SET `state` = ?, `finalized_by` = ?, `decided_epoch` = ?, `outcome_reason` = ?, `updated_ms` = ?"+
					" WHERE "+itemPK+" AND `state` = ?",
					uint32(d.newState), uint32(inboxpb.PlayerInboxFinalizer_PLAYER_INBOX_FINALIZER_CONSUMER), reqEpoch,
					ackOutcomeReason(report), nowMs, shard, playerID, uint32(domain), report.GetSeq(),
					uint32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING))
				if err != nil {
					return fmt.Errorf("inbox: finalize player=%d seq=%d: %w", playerID, report.GetSeq(), err)
				}
				if err := requireOneRow(upd, "finalize item"); err != nil {
					return err
				}
				finalized++
			}
			results = append(results, &inboxpb.PlayerInboxAckItem{Seq: report.GetSeq(), Result: d.result, State: d.state})
		}
		write, newOpen, newFence, underflow := ackStreamUpdate(verdict, st.GetOpenCount(), finalized, lockedFence, reqEpoch)
		if !write {
			return nil
		}
		if underflow {
			logx.WithContext(ctx).Errorf("[player_inbox] open_count underflow player=%d domain=%d open_count=%d finalized=%d; clamping to 0",
				playerID, domain, st.GetOpenCount(), finalized)
		}
		upd, err := tx.ExecContext(ctx, "UPDATE "+streamTable+
			" SET `open_count` = ?, `fence_epoch` = ?, `updated_ms` = ? WHERE "+streamPK,
			newOpen, newFence, nowMs, shard, playerID, uint32(domain))
		if err != nil {
			return fmt.Errorf("inbox: update stream after ack player=%d: %w", playerID, err)
		}
		return requireOneRow(upd, "update stream after ack")
	})
	if err != nil {
		return nil, err
	}

	resp.Fence = verdict
	if verdict == inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_NO_STREAM {
		resp.Results = results
		s.reportAckResults(ctx, playerID, domain, req, results)
		return resp, nil
	}
	if !FenceAllows(verdict) {
		resp.ErrorMessage = tipOf(fenceRejectTip(verdict))
		s.reportFenceReject(ctx, "Ack", verdict, playerID, domain, reqEpoch, lockedFence, req.GetSceneNodeId())
		metrics.ObserveAck(dl, FenceLabel(verdict))
		return resp, nil
	}
	resp.Results = results
	s.reportAckResults(ctx, playerID, domain, req, results)
	return resp, nil
}

// reportAckResults 记 Ack 逐条结果的指标;协议异常与 SETTLED_UNKNOWN 都记 ERROR(logx 没有 WARN 级;
// 告警分级由第 4 批的规则按 ack_total{result} 区分 critical / warning)。
// ERROR 日志带发送方身份 peer / scene_node_id(评审 L10,口径同 reportFenceReject)。
func (s *Store) reportAckResults(ctx context.Context, playerID uint64, domain inboxpb.PlayerInboxDomain,
	req *inboxpb.AckPlayerInboxRequest, results []*inboxpb.PlayerInboxAckItem) {
	dl := DomainLabel(domain)
	log := logx.WithContext(ctx)
	peerAddr, sceneNodeID := PeerAddr(ctx), req.GetSceneNodeId()
	for i, res := range results {
		label := ackMetricLabel(res)
		metrics.ObserveAck(dl, label)
		var report *inboxpb.PlayerInboxDecisionReport
		if i < len(req.GetDecisions()) {
			report = req.GetDecisions()[i]
		}
		switch {
		case label == ackLabelSettledUnknown: // warning 告警单列(设计 §9.4)
			log.Errorf("[player_inbox] Item settled as SETTLED_UNKNOWN player=%d domain=%d seq=%d request_id=%d epoch=%d "+
				"peer=%s scene_node_id=%d: consumer watermark passed it but its outcome left the recent ring; verify via reconcile",
				playerID, domain, res.GetSeq(), report.GetRequestId(), req.GetOwnerEpoch(), peerAddr, sceneNodeID)
		case isAckAnomaly(res.GetResult()):
			log.Errorf("[player_inbox] Ack protocol anomaly result=%s player=%d domain=%d seq=%d reported_outcome=%d "+
				"reported_request_id=%d row_state=%d epoch=%d peer=%s scene_node_id=%d: row unchanged; consumer halts this domain",
				AckResultLabel(res.GetResult()), playerID, domain, res.GetSeq(), report.GetOutcome(),
				report.GetRequestId(), res.GetState(), req.GetOwnerEpoch(), peerAddr, sceneNodeID)
		}
	}
}

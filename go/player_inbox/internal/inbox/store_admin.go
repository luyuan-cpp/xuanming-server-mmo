package inbox

import (
	"context"
	"database/sql"
	"fmt"

	"player_inbox/internal/constants"
	"player_inbox/internal/metrics"

	dspb "proto/data_service"
	inboxpb "proto/player_inbox"

	"github.com/zeromicro/go-zero/core/logx"
)

// 运维接口的存储操作(设计 §5.2、§7.9):作废、按玩家概况、列条目、按幂等键查。

// VoidParams 是一次运维作废的输入。
type VoidParams struct {
	PlayerID uint64
	Domain   inboxpb.PlayerInboxDomain
	Seq      uint64
	Reason   inboxpb.PlayerInboxVoidReason
	Operator string
	Note     string
	PeerAddr string
	// Mode 缺省 NORMAL = 原语义;FORCE_MISMATCH 走 forceVoid(要求 ledgers 非 nil、Note 非空)。
	Mode inboxpb.PlayerInboxVoidMode
}

// Void 作废一条条目(设计 §7.9、IB-5)。普通模式:
//   - 从未投递(delivery_count=0):在流行锁下直接改 VOIDED(finalized_by=ADMIN),open_count−1;
//   - 已投递:只挂 directive=VOID,由消费方裁决(还没处理过 → 记 SKIPPED 再确认;已处理过 → 确认原结局);
//   - 已挂过 VOID 指令:幂等成功,不改行;
//   - 已是终态 → kPlayerInboxItemStateConflict;不存在 → kPlayerInboxItemNotFound。
//
// 改动行的分支与审计行(action=VOID,INSERT IGNORE)同库同事务。
// 强制模式(mode=FORCE_MISMATCH)见 forceVoid;ledgers 只有它用,普通模式可传 nil。
func (s *Store) Void(ctx context.Context, p VoidParams, ledgers LedgerReader) (*inboxpb.VoidPlayerInboxItemResponse, error) {
	resp := &inboxpb.VoidPlayerInboxItemResponse{}
	// Battle永久来源不能由通用运维VOID绕过到账；该域只允许真实APPLIED终结。
	if p.Domain == inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT {
		resp.ErrorMessage = tipOf(constants.ErrItemStateConflict)
		return resp, nil
	}
	if _, ok := s.registry.Domain(p.Domain); !ok || p.PlayerID == 0 || p.Seq == 0 ||
		p.Reason == inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_UNSPECIFIED ||
		inboxpb.PlayerInboxVoidReason_name[int32(p.Reason)] == "" ||
		inboxpb.PlayerInboxVoidMode_name[int32(p.Mode)] == "" {
		resp.ErrorMessage = tipOf(constants.ErrInvalidParameter)
		return resp, nil
	}
	if code := ValidateText(p.Operator, constants.MaxOperatorBytes, true); code != 0 {
		resp.ErrorMessage = tipOf(code)
		return resp, nil
	}
	if code := ValidateText(p.Note, constants.MaxReasonBytes, false); code != 0 {
		resp.ErrorMessage = tipOf(code)
		return resp, nil
	}
	if p.Mode == inboxpb.PlayerInboxVoidMode_PLAYER_INBOX_VOID_MODE_FORCE_MISMATCH {
		return s.forceVoid(ctx, p, ledgers)
	}

	shard := RowShard(p.PlayerID)
	var (
		action voidAction
		state  inboxpb.PlayerInboxItemState
	)
	err := s.withPlayerTx(ctx, p.PlayerID, func(tx *sql.Tx, original *dspb.AccountRoleOwnership) error {
		action, state = 0, inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_UNSPECIFIED
		if err := requirePendingOriginalsTx(ctx, tx, original); err != nil {
			return err
		}
		st, err := lockStream(ctx, tx, shard, p.PlayerID, p.Domain)
		if err != nil {
			return err
		}
		if st == nil {
			action = voidNotFound
			return nil
		}
		row, err := readItem(ctx, tx, shard, p.PlayerID, p.Domain, p.Seq)
		if err != nil {
			return err
		}
		if err = requireItemOriginal(row, original); err != nil {
			return err
		}
		action = decideVoid(row)
		nowMs := s.nowMs()
		switch action {
		case voidNotFound:
			return nil
		case voidTerminal:
			state = inboxpb.PlayerInboxItemState(row.GetState())
			return nil
		case voidDirect:
			upd, err := tx.ExecContext(ctx, "UPDATE "+itemTable+
				" SET `state` = ?, `finalized_by` = ?, `void_reason` = ?, `updated_ms` = ?"+
				" WHERE "+itemPK+" AND `state` = ? AND `delivery_count` = 0",
				uint32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_VOIDED),
				uint32(inboxpb.PlayerInboxFinalizer_PLAYER_INBOX_FINALIZER_ADMIN), uint32(p.Reason), nowMs,
				shard, p.PlayerID, uint32(p.Domain), p.Seq, uint32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING))
			if err != nil {
				return fmt.Errorf("inbox: void item player=%d seq=%d: %w", p.PlayerID, p.Seq, err)
			}
			if err := requireOneRow(upd, "void item"); err != nil {
				return err
			}
			openCount, underflow := subtractOpenCount(st.GetOpenCount(), 1)
			if underflow {
				logx.WithContext(ctx).Errorf("[player_inbox] open_count already 0 while voiding a PENDING row player=%d domain=%d seq=%d",
					p.PlayerID, p.Domain, p.Seq)
			}
			upd, err = tx.ExecContext(ctx, "UPDATE "+streamTable+" SET `open_count` = ?, `updated_ms` = ? WHERE "+streamPK,
				openCount, nowMs, shard, p.PlayerID, uint32(p.Domain))
			if err != nil {
				return fmt.Errorf("inbox: update stream after void player=%d: %w", p.PlayerID, err)
			}
			if err := requireOneRow(upd, "update stream after void"); err != nil {
				return err
			}
			state = inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_VOIDED
		case voidDirective:
			upd, err := tx.ExecContext(ctx, "UPDATE "+itemTable+
				" SET `directive` = ?, `void_reason` = ?, `updated_ms` = ? WHERE "+itemPK+" AND `state` = ?",
				uint32(inboxpb.PlayerInboxDirective_PLAYER_INBOX_DIRECTIVE_VOID), uint32(p.Reason), nowMs,
				shard, p.PlayerID, uint32(p.Domain), p.Seq, uint32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING))
			if err != nil {
				return fmt.Errorf("inbox: attach void directive player=%d seq=%d: %w", p.PlayerID, p.Seq, err)
			}
			if err := requireOneRow(upd, "attach void directive"); err != nil {
				return err
			}
			state = inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING
		case voidAlreadyRequested:
			state = inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING
		}
		// 内容列(msg_type / request_id / 载荷等)取自上面读到的 row(评审 L9);void_reason 记本次请求的原因。
		return insertAuditForItem(ctx, tx, row, &inboxpb.PlayerInboxAudit{
			PlayerId:   p.PlayerID,
			Domain:     uint32(p.Domain),
			Seq:        p.Seq,
			Action:     uint32(inboxpb.PlayerInboxAuditAction_PLAYER_INBOX_AUDIT_VOID),
			Operator:   p.Operator,
			PeerAddr:   p.PeerAddr,
			Reason:     p.Note,
			CreatedMs:  nowMs,
			VoidReason: uint32(p.Reason),
		})
	})
	if err != nil {
		return nil, err
	}

	rl := VoidReasonLabel(p.Reason)
	out := voidOutcomeOf(action, state)
	resp.ErrorMessage, resp.State, resp.VoidRequested = tipOf(out.tip), out.state, out.voidRequested
	if out.path != "" {
		metrics.ObserveVoid(DomainLabel(p.Domain), rl, out.path)
	}
	if resp.ErrorMessage == nil {
		logx.WithContext(ctx).Infof("[player_inbox] VoidItem player=%d domain=%d seq=%d reason=%s state=%d void_requested=%t operator=%q peer=%s",
			p.PlayerID, p.Domain, p.Seq, rl, resp.State, resp.VoidRequested, p.Operator, p.PeerAddr)
	}
	return resp, nil
}

// forceVoid 是 VoidItem 的强制模式(设计 §7.9,mode=FORCE_MISMATCH):终结一条被 scene 回放核对判 MISMATCH、
// 因而永久停用该域的已投递行。普通模式对已投递的行只能挂指令,而 scene 先做回放核对、后看指令,挂了也解不开。
//
//  1. 事务外经 ledgers 读该玩家**已落盘**的账本(不在持有行锁时等 data_service / Redis,设计 §10)。
//     读失败返回 error(适配层折成 kPlayerInboxStoreUnavailable,可重试),不改任何状态。
//  2. 开事务锁流行,按主键复读行,decideForceVoidLocked 判定。证明成立才以「仍 PENDING、已投递、request_id 不变」
//     为条件改 VOIDED(finalized_by=ADMIN、void_reason=请求的原因),open_count−1,同事务写审计
//     (action=FORCE_VOID,reason=note)。证明不成立 → kPlayerInboxItemStateConflict + force_verdict,行不变、不写审计。
//
// 先读账本、后在锁内判定没有竞态:账本水位只升不降,环里某个 seq 的结局写定后不再改(只会被挤出环);
// 证明成立时消费方对本行只会判 MISMATCH,这期间无论再投递多少次都不会执行它。唯一的例外是「已落盘账本本身回退」
// (Redis 丢数据),与资产通道一样在保证边界之外(设计 §7.10)。
//
// 终态行重复调用回 kPlayerInboxItemStateConflict(与普通模式一致);是否已强制终结,按 LookupByRequest 看
// state=VOIDED、finalized_by=ADMIN 且 delivery_count>0(普通模式直接作废的行 delivery_count 恒为 0)。
func (s *Store) forceVoid(ctx context.Context, p VoidParams, ledgers LedgerReader) (*inboxpb.VoidPlayerInboxItemResponse, error) {
	resp := &inboxpb.VoidPlayerInboxItemResponse{}
	// 强制终结是高风险操作:审计必须写清工单与依据,note 必填。
	if code := ValidateText(p.Note, constants.MaxReasonBytes, true); code != 0 {
		resp.ErrorMessage = tipOf(code)
		return resp, nil
	}
	if ledgers == nil {
		return nil, fmt.Errorf("inbox: force void has no ledger reader")
	}
	readCtx, cancel := context.WithTimeout(ctx, constants.LedgerReadTimeout)
	ledger, found, err := ledgers.ReadInboxLedger(readCtx, p.PlayerID)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("inbox: force void read persisted ledger player=%d: %w", p.PlayerID, err)
	}

	shard := RowShard(p.PlayerID)
	var (
		action    voidAction
		proof     forceVoidProof
		state     inboxpb.PlayerInboxItemState
		requestID uint64
	)
	err = s.withPlayerTx(ctx, p.PlayerID, func(tx *sql.Tx, original *dspb.AccountRoleOwnership) error {
		action, proof, state, requestID = voidNotFound, forceVoidProof{}, inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_UNSPECIFIED, 0
		if err := requirePendingOriginalsTx(ctx, tx, original); err != nil {
			return err
		}
		st, err := lockStream(ctx, tx, shard, p.PlayerID, p.Domain)
		if err != nil {
			return err
		}
		if st == nil {
			return nil
		}
		row, err := readItem(ctx, tx, shard, p.PlayerID, p.Domain, p.Seq)
		if err != nil {
			return err
		}
		if err = requireItemOriginal(row, original); err != nil {
			return err
		}
		action, proof = decideForceVoidLocked(row, ledger, found)
		if row != nil {
			state, requestID = inboxpb.PlayerInboxItemState(row.GetState()), row.GetRequestId()
		}
		if action != voidForced {
			return nil
		}
		nowMs := s.nowMs()
		upd, err := tx.ExecContext(ctx, "UPDATE "+itemTable+
			" SET `state` = ?, `finalized_by` = ?, `void_reason` = ?, `updated_ms` = ?"+
			" WHERE "+itemPK+" AND `state` = ? AND `delivery_count` > 0 AND `request_id` = ?",
			uint32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_VOIDED),
			uint32(inboxpb.PlayerInboxFinalizer_PLAYER_INBOX_FINALIZER_ADMIN), uint32(p.Reason), nowMs,
			shard, p.PlayerID, uint32(p.Domain), p.Seq, uint32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING), requestID)
		if err != nil {
			return fmt.Errorf("inbox: force void item player=%d seq=%d: %w", p.PlayerID, p.Seq, err)
		}
		if err := requireOneRow(upd, "force void item"); err != nil {
			return err
		}
		openCount, underflow := subtractOpenCount(st.GetOpenCount(), 1)
		if underflow {
			logx.WithContext(ctx).Errorf("[player_inbox] open_count already 0 while force-voiding a PENDING row player=%d domain=%d seq=%d",
				p.PlayerID, p.Domain, p.Seq)
		}
		upd, err = tx.ExecContext(ctx, "UPDATE "+streamTable+" SET `open_count` = ?, `updated_ms` = ? WHERE "+streamPK,
			openCount, nowMs, shard, p.PlayerID, uint32(p.Domain))
		if err != nil {
			return fmt.Errorf("inbox: update stream after force void player=%d: %w", p.PlayerID, err)
		}
		if err := requireOneRow(upd, "update stream after force void"); err != nil {
			return err
		}
		state = inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_VOIDED
		// 证明成立时 row 非 nil(decideForceVoidLocked 对 nil 判 voidNotFound);内容列取自它(评审 L9)。
		return insertAuditForItem(ctx, tx, row, &inboxpb.PlayerInboxAudit{
			PlayerId:   p.PlayerID,
			Domain:     uint32(p.Domain),
			Seq:        p.Seq,
			Action:     uint32(inboxpb.PlayerInboxAuditAction_PLAYER_INBOX_AUDIT_FORCE_VOID),
			Operator:   p.Operator,
			PeerAddr:   p.PeerAddr,
			Reason:     p.Note,
			CreatedMs:  nowMs,
			VoidReason: uint32(p.Reason),
		})
	})
	if err != nil {
		return nil, err
	}

	rl := VoidReasonLabel(p.Reason)
	out := voidOutcomeOf(action, state)
	resp.ErrorMessage, resp.State, resp.VoidRequested = tipOf(out.tip), out.state, out.voidRequested
	resp.ForceVerdict = proof.verdict
	if out.path != "" {
		metrics.ObserveVoid(DomainLabel(p.Domain), rl, out.path)
	}
	log := logx.WithContext(ctx)
	switch action {
	case voidForced:
		log.Infof("[player_inbox] VoidItem FORCE finalized player=%d domain=%d seq=%d request_id=%d reason=%s "+
			"ledger_decided_through=%d ledger_request_id_at_seq=%d operator=%q peer=%s",
			p.PlayerID, p.Domain, p.Seq, requestID, rl, proof.decidedThrough, proof.ringRequestID, p.Operator, p.PeerAddr)
	case voidForceRefused:
		log.Infof("[player_inbox] VoidItem FORCE refused player=%d domain=%d seq=%d request_id=%d verdict=%s "+
			"ledger_decided_through=%d ledger_request_id_at_seq=%d operator=%q peer=%s",
			p.PlayerID, p.Domain, p.Seq, requestID, proof.verdict, proof.decidedThrough, proof.ringRequestID, p.Operator, p.PeerAddr)
	}
	return resp, nil
}

// Summary 按玩家列出各域流的概况,另给 max_fence_epoch 供 epoch-raise 取最大值(设计 §7.8、C4)。
func (s *Store) Summary(ctx context.Context, playerID uint64) (*inboxpb.GetPlayerInboxSummaryResponse, error) {
	if playerID == 0 {
		return &inboxpb.GetPlayerInboxSummaryResponse{ErrorMessage: tipOf(constants.ErrInvalidParameter)}, nil
	}
	shard := RowShard(playerID)
	rows, err := s.db.QueryContext(ctx, "SELECT "+streamColumns+" FROM "+streamTable+
		" WHERE `shard` = ? AND `player_id` = ? ORDER BY `domain` ASC", shard, playerID)
	if err != nil {
		return nil, fmt.Errorf("inbox: summary streams player=%d: %w", playerID, err)
	}
	var streams []*inboxpb.PlayerInboxStream
	for rows.Next() {
		st, err := scanStream(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		streams = append(streams, st)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	aggs := map[uint32]pendingAggregate{}
	arows, err := s.db.QueryContext(ctx, "SELECT `domain`,"+
		" COALESCE(SUM(CASE WHEN `delivery_count` > 0 THEN 1 ELSE 0 END), 0), COALESCE(MIN(`created_ms`), 0)"+
		" FROM "+itemTable+" WHERE `shard` = ? AND `player_id` = ? AND `state` = ? GROUP BY `domain`",
		shard, playerID, uint32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING))
	if err != nil {
		return nil, fmt.Errorf("inbox: summary aggregates player=%d: %w", playerID, err)
	}
	for arows.Next() {
		var d uint32
		var a pendingAggregate
		if err := arows.Scan(&d, &a.deliveredUnacked, &a.oldestCreatedMs); err != nil {
			arows.Close()
			return nil, err
		}
		aggs[d] = a
	}
	if err := arows.Close(); err != nil {
		return nil, err
	}
	return summarizeStreams(playerID, streams, aggs), nil
}

// List 按 player_id + domain(+ 可选 state)分页列条目,seq 升序,after_seq 翻页,limit ≤ 200。
func (s *Store) List(ctx context.Context, req *inboxpb.ListPlayerInboxItemsRequest) (*inboxpb.ListPlayerInboxItemsResponse, error) {
	resp := &inboxpb.ListPlayerInboxItemsResponse{}
	if _, ok := s.registry.Domain(req.GetDomain()); !ok || req.GetPlayerId() == 0 {
		resp.ErrorMessage = tipOf(constants.ErrInvalidParameter)
		return resp, nil
	}
	if st := req.GetState(); st != inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_UNSPECIFIED &&
		inboxpb.PlayerInboxItemState_name[int32(st)] == "" {
		resp.ErrorMessage = tipOf(constants.ErrInvalidParameter)
		return resp, nil
	}
	limit := clampFetchItems(req.GetLimit(), constants.ListDefaultLimit, constants.ListMaxLimit)
	query := "SELECT " + itemColumns + " FROM " + itemTable +
		" WHERE `shard` = ? AND `player_id` = ? AND `domain` = ? AND `seq` > ?"
	args := []any{RowShard(req.GetPlayerId()), req.GetPlayerId(), uint32(req.GetDomain()), req.GetAfterSeq()}
	if st := req.GetState(); st != inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_UNSPECIFIED {
		query += " AND `state` = ?"
		args = append(args, uint32(st))
	}
	query += " ORDER BY `seq` ASC LIMIT ?"
	args = append(args, limit+1)
	rows, err := queryItems(ctx, s.db, query, args...)
	if err != nil {
		return nil, fmt.Errorf("inbox: list items player=%d: %w", req.GetPlayerId(), err)
	}
	rows, resp.HasMore = splitPage(rows, limit)
	for _, row := range rows {
		resp.Items = append(resp.Items, ItemView(row))
	}
	return resp, nil
}

// Lookup 按幂等键 (player_id, producer_kind, request_id) 查条目与结局。
func (s *Store) Lookup(ctx context.Context, req *inboxpb.LookupPlayerInboxRequest) (*inboxpb.LookupPlayerInboxResponse, error) {
	resp := &inboxpb.LookupPlayerInboxResponse{}
	if req.GetPlayerId() == 0 || req.GetRequestId() == 0 || !validProducer(req.GetProducerKind()) {
		resp.ErrorMessage = tipOf(constants.ErrInvalidParameter)
		return resp, nil
	}
	row, err := readItemByRequest(ctx, s.db, RowShard(req.GetPlayerId()), req.GetPlayerId(), req.GetProducerKind(), req.GetRequestId())
	if err != nil {
		return nil, err
	}
	if row == nil {
		resp.ErrorMessage = tipOf(constants.ErrItemNotFound)
		return resp, nil
	}
	resp.Item = ItemView(row)
	return resp, nil
}

// ItemView 把一行折成运维视图(全部列;载荷原样返回,由运维工具按 msg_type 的 payload_message 解码)。
func ItemView(row *inboxpb.PlayerInboxItemRow) *inboxpb.PlayerInboxItemView {
	return &inboxpb.PlayerInboxItemView{
		Domain:             inboxpb.PlayerInboxDomain(row.GetDomain()),
		Seq:                row.GetSeq(),
		MsgType:            inboxpb.PlayerInboxMessageType(row.GetMsgType()),
		ProducerKind:       inboxpb.PlayerInboxProducerKind(row.GetProducerKind()),
		RequestId:          row.GetRequestId(),
		Payload:            row.GetPayload(),
		State:              inboxpb.PlayerInboxItemState(row.GetState()),
		Directive:          inboxpb.PlayerInboxDirective(row.GetDirective()),
		VoidReason:         inboxpb.PlayerInboxVoidReason(row.GetVoidReason()),
		DeliveryCount:      row.GetDeliveryCount(),
		LastDeliveredEpoch: row.GetLastDeliveredEpoch(),
		LastDeliveredMs:    row.GetLastDeliveredMs(),
		OutcomeReason:      row.GetOutcomeReason(),
		FinalizedBy:        inboxpb.PlayerInboxFinalizer(row.GetFinalizedBy()),
		DecidedEpoch:       row.GetDecidedEpoch(),
		IssuedAtMs:         row.GetIssuedAtMs(),
		CreatedMs:          row.GetCreatedMs(),
		UpdatedMs:          row.GetUpdatedMs(),
		OperatorName:       row.GetOperatorName(),
		Note:               row.GetNote(),
	}
}

package inbox

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"player_inbox/internal/constants"
	"player_inbox/internal/metrics"

	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
	dspb "proto/data_service"
	inboxpb "proto/player_inbox"

	"github.com/zeromicro/go-zero/core/logx"
)

// EnqueueParams 是一次入箱的输入(设计 §6.2)。
//
// 生产方契约(写进 runbook 与接口注释):RequestID 与 IssuedAtMs 都按工单只生成一次,重试时两者都原样带回;
// IssuedAtMs 在内容摘要里,刷新它重试会得到 kPlayerInboxIdempotencyConflict(看得见的错误,不会静默重复入箱)。
type EnqueueParams struct {
	PlayerID   uint64
	Type       inboxpb.PlayerInboxMessageType
	Producer   inboxpb.PlayerInboxProducerKind
	RequestID  uint64
	IssuedAtMs uint64
	// Payload 是按类型规格 payload_message 序列化之后的**最终**载荷(GM 的 gm_operator 已由服务端覆盖)。
	Payload []byte
	// Operator 写进条目 operator_name(必填,≤ 64B);Note 写进条目 note(≤ 256B)。
	Operator string
	Note     string
	// 仅签名Match专用入口设置；普通GM不能借此跳过签发窗或捕获新的角色归属。
	battleOriginal *inboxpb.BattleSettlementInboxRequest
}

// EnqueueResult 是入箱结局。Code 为 0 表示成功:Created=true 是新建,false 是 DUPLICATE(同键同摘要,
// 返回原 seq 与当前状态);Code 非 0 是业务拒绝。基础设施故障走 error。
type EnqueueResult struct {
	Code    uint32
	Seq     uint64
	Created bool
	State   inboxpb.PlayerInboxItemState
}

// 同P已经串行全部producer/domain；锁内仍撞1062表示未知旧写者或主键完整性问题，不能无P重读后伪装成功。
var errDuplicateUnderP = errors.New("inbox: duplicate key while original role P is held")

// errStreamVanished 表示 ensureStream 刚确认过的流行在主事务里读不到(流行永不删除,只可能是缺陷或库被动过)。
var errStreamVanished = errors.New("inbox: stream row vanished after ensureStream")

// 仅专用Battle原件的确定性冲突；角色冻结、SQL/传输错误仍可能恢复，不能按文本归此类。
var ErrBattleSettlementOriginal = errors.New("inbox: Battle永久原件冲突")

// Enqueue 是入箱核心(设计 §6.2)。inTx 在主事务里、提交之前执行(GM 生产方用它同事务写审计,设计 §6.1);
// 它返回错误会让整次入箱回滚。inTx 可能因事务重试被调用多次,必须可重跑。
//
// 纯参数/玩家存在预检之后，完整事务首锁原角色P并拒绝冻结，再按永久唯一行核原owner和摘要。
// 判重先于时间窗与容量；只有新请求才捕获当前原owner。中性INSERT IGNORE补流后锁stream，
// 分配seq、插入PENDING、推进计数和写审计同事务提交。1062不走无P成功分支。
// 提交之后的在线提醒尽力而为，不改变持久业务结果。
func (s *Store) Enqueue(ctx context.Context, p EnqueueParams, inTx func(*sql.Tx, uint64) error) (EnqueueResult, error) {
	res, err := s.enqueue(ctx, p, inTx)
	metrics.ObserveEnqueue(TypeLabel(p.Type), enqueueResultLabel(res, err))
	return res, err
}

func (s *Store) enqueue(ctx context.Context, p EnqueueParams, inTx func(*sql.Tx, uint64) error) (EnqueueResult, error) {
	// ── 1. 注册表与字段校验(与时间无关)──
	if p.PlayerID == 0 || p.RequestID == 0 || p.IssuedAtMs == 0 || !validProducer(p.Producer) {
		return EnqueueResult{Code: constants.ErrInvalidParameter}, nil
	}
	if (p.Producer == inboxpb.PlayerInboxProducerKind_PLAYER_INBOX_PRODUCER_MATCH || p.Type == inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT) != (p.battleOriginal != nil) {
		return EnqueueResult{Code: constants.ErrInvalidParameter}, nil
	}
	entry, code := s.registry.ValidatePayload(p.Type, s.allowed, p.Payload)
	if code != 0 {
		return EnqueueResult{Code: code}, nil
	}
	if code := ValidateText(p.Operator, constants.MaxOperatorBytes, true); code != 0 {
		return EnqueueResult{Code: code}, nil
	}
	if code := ValidateText(p.Note, constants.MaxReasonBytes, false); code != 0 {
		return EnqueueResult{Code: code}, nil
	}
	domain := entry.Spec.GetDomain()
	domainSpec, ok := s.registry.Domain(domain)
	if !ok {
		// 注册表自检已保证每个类型的域都有规格;走到这里只可能是装配被绕开。
		return EnqueueResult{}, fmt.Errorf("inbox: type %v domain %v has no domain spec", p.Type, domain)
	}

	// ── 2. 玩家存在校验 ──
	exists, err := s.players.PlayerExists(ctx, p.PlayerID)
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("inbox: player existence check player=%d: %w", p.PlayerID, err)
	}
	if !exists {
		return EnqueueResult{Code: constants.ErrPlayerNotFound}, nil
	}

	// ── 3. shard 与内容摘要 ──
	shard := RowShard(p.PlayerID)
	digest := ContentSHA256(p.Type, p.IssuedAtMs, p.Payload)
	payload := p.Payload
	if payload == nil {
		// 合法的空载荷(如 currency_type=0 的封锁)序列化后是零字节;nil 会被驱动当成 NULL 写进 NOT NULL 列。
		payload = []byte{}
	}

	// P首锁内判重、核原owner，再校验时间并中性补流；封存后不能留下新PENDING。
	var res EnqueueResult
	err = s.withPlayerTx(ctx, p.PlayerID, func(tx *sql.Tx, original *dspb.AccountRoleOwnership) error {
		res = EnqueueResult{} // 可重跑:上一轮的结果作废
		if p.battleOriginal != nil && !battleOriginalMatches(p.battleOriginal, original) {
			return fmt.Errorf("%w: 原账号或revision不符", ErrBattleSettlementOriginal)
		}
		existing, err := readItemByRequest(ctx, tx, shard, p.PlayerID, p.Producer, p.RequestID)
		if err != nil {
			return err
		}
		if err = requireItemOriginal(existing, original); err != nil {
			if p.battleOriginal != nil {
				return fmt.Errorf("%w: 永久收据原归属不符", ErrBattleSettlementOriginal)
			}
			return err
		}
		if existing != nil {
			res = resultForExisting(existing, digest)
			return nil
		}
		nowMs := s.nowMs()
		if (p.battleOriginal == nil && !issuedWithinWindow(p.IssuedAtMs, nowMs)) || (p.battleOriginal != nil && p.IssuedAtMs > nowMs+uint64(constants.IssuedAtMaxFuture.Milliseconds())) {
			res.Code = constants.ErrIssuedOutOfWindow
			return nil
		}
		if err = requirePendingOriginalsTx(ctx, tx, original); err != nil {
			return err
		}
		if err = ensureStreamTx(ctx, tx, shard, p.PlayerID, domain, nowMs); err != nil {
			return err
		}
		st, err := lockStream(ctx, tx, shard, p.PlayerID, domain)
		if err != nil {
			return err
		}
		if st == nil {
			return errStreamVanished
		}
		// 7.2 锁内权威判重(RC 下普通读看得见前一个持锁者已提交的行),7.3 再判满箱:
		// 满箱同步拒绝,不挤掉旧条目也不静默丢弃(设计 §3.4)。两步的先后见 decideEnqueueLocked。
		existing, err = readItemByRequest(ctx, tx, shard, p.PlayerID, p.Producer, p.RequestID)
		if err != nil {
			return err
		}
		if err = requireItemOriginal(existing, original); err != nil {
			return err
		}
		if decided, proceed := decideEnqueueLocked(existing, digest, st.GetOpenCount(), domainSpec.GetMaxPending()); !proceed {
			res = decided
			return nil // 不写任何东西,提交一个空事务
		}
		// 7.4 分配 seq 并插入。时间每次重跑重取:seq 的时间下界按实际提交那一轮算。
		txNowMs := s.nowMs()
		seq, err := NextSeq(st.GetNextSeq(), txNowMs)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO "+itemTable+" ("+itemColumns+") VALUES ("+placeholders(25)+")",
			shard, p.PlayerID, uint32(domain), seq, uint32(p.Type), uint32(p.Producer), p.RequestID,
			digest, payload,
			uint32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING),
			uint32(inboxpb.PlayerInboxDirective_PLAYER_INBOX_DIRECTIVE_NONE),
			uint32(inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_UNSPECIFIED),
			uint32(0), uint64(0), uint64(0), uint32(0),
			uint32(inboxpb.PlayerInboxFinalizer_PLAYER_INBOX_FINALIZER_UNSPECIFIED), uint64(0),
			p.IssuedAtMs, txNowMs, txNowMs, p.Operator, p.Note, original.GetAccount(), original.GetOwnerRevision())
		if err != nil {
			if isDuplicateKeyError(err) {
				return errDuplicateUnderP
			}
			return fmt.Errorf("inbox: insert item player=%d seq=%d: %w", p.PlayerID, seq, err)
		}
		// 7.5 推进流行。notify_due_ms = now:提交后那次提醒成功才以 next_seq 为令牌清零,否则留给补发循环。
		upd, err := tx.ExecContext(ctx, "UPDATE "+streamTable+
			" SET `next_seq` = ?, `open_count` = ?, `notify_due_ms` = ?, `updated_ms` = ? WHERE "+streamPK,
			seq+1, st.GetOpenCount()+1, txNowMs, txNowMs, shard, p.PlayerID, uint32(domain))
		if err != nil {
			return fmt.Errorf("inbox: advance stream player=%d: %w", p.PlayerID, err)
		}
		if err := requireOneRow(upd, "advance stream"); err != nil {
			return err
		}
		// 7.6 同事务审计。
		if inTx != nil {
			if err := inTx(tx, seq); err != nil {
				return err
			}
		}
		res = EnqueueResult{Seq: seq, Created: true, State: inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING}
		return nil
	})
	if err != nil {
		return EnqueueResult{}, err
	}

	// ── 8. 提交之后发提醒(只对新建条目)──
	if res.Created {
		notifyCtx, cancel := context.WithTimeout(ctx, constants.NotifyTimeout)
		result := s.notifyAndClear(notifyCtx, p.PlayerID, domain, res.Seq+1)
		cancel()
		if !result.Settled() {
			logx.WithContext(ctx).Infof("[player_inbox] Post-commit notify not delivered player=%d domain=%d seq=%d result=%s; "+
				"left for resend loop", p.PlayerID, domain, res.Seq, result)
		}
	}
	return res, nil
}

// Battle来源是Match已提交的原件，旧于GM签发窗仍须恢复；全部归属校验仍在同一P锁内。
// Lookup只返回永久SQL原收据，不以当前玩家水位猜测APPLIED。
func (s *Store) EnqueueBattleSettlement(ctx context.Context, req *inboxpb.BattleSettlementInboxRequest) (*inboxpb.BattleSettlementInboxResponse, error) {
	if err := validateBattleOriginal(req); err != nil {
		return nil, err
	}
	p := EnqueueParams{PlayerID: req.PlayerId, Type: inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT,
		Producer: inboxpb.PlayerInboxProducerKind_PLAYER_INBOX_PRODUCER_MATCH, RequestID: req.BattleId,
		IssuedAtMs: req.IssuedAtMs, Payload: req.SettlementPayload, Operator: "match", battleOriginal: req}
	r, err := s.Enqueue(ctx, p, nil)
	if err != nil {
		return nil, err
	}
	if r.Code == constants.ErrIdempotencyConflict {
		return nil, fmt.Errorf("%w: 同局入箱正文冲突", ErrBattleSettlementOriginal)
	}
	return &inboxpb.BattleSettlementInboxResponse{PlayerId: req.PlayerId, BattleId: req.BattleId, Seq: r.Seq, State: r.State,
		ContentSha256: ContentSHA256(p.Type, p.IssuedAtMs, p.Payload), ErrorMessage: tipOf(r.Code)}, nil
}

func (s *Store) LookupBattleSettlement(ctx context.Context, req *inboxpb.BattleSettlementInboxRequest) (*inboxpb.BattleSettlementInboxResponse, error) {
	if err := validateBattleOriginal(req); err != nil {
		return nil, err
	}
	resp := &inboxpb.BattleSettlementInboxResponse{PlayerId: req.PlayerId, BattleId: req.BattleId}
	digest := ContentSHA256(inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT, req.IssuedAtMs, req.SettlementPayload)
	err := s.withPlayerTx(ctx, req.PlayerId, func(tx *sql.Tx, original *dspb.AccountRoleOwnership) error {
		resp.Seq, resp.State, resp.ContentSha256 = 0, 0, nil
		if !battleOriginalMatches(req, original) {
			return fmt.Errorf("%w: 原账号或revision不符", ErrBattleSettlementOriginal)
		}
		row, err := readItemByRequest(ctx, tx, RowShard(req.PlayerId), req.PlayerId, inboxpb.PlayerInboxProducerKind_PLAYER_INBOX_PRODUCER_MATCH, req.BattleId)
		if err != nil {
			return err
		}
		if row == nil {
			return nil
		}
		if err = requireItemOriginal(row, original); err != nil {
			return fmt.Errorf("%w: 永久收据原归属不符", ErrBattleSettlementOriginal)
		}
		if row.GetMsgType() != uint32(inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT) || !bytes.Equal(row.GetContentSha256(), digest) || !bytes.Equal(row.GetPayload(), req.SettlementPayload) || row.GetIssuedAtMs() != req.IssuedAtMs {
			return fmt.Errorf("%w: 原Battle收据正文冲突", ErrBattleSettlementOriginal)
		}
		resp.Seq, resp.State, resp.ContentSha256 = row.GetSeq(), inboxpb.PlayerInboxItemState(row.GetState()), append([]byte(nil), row.GetContentSha256()...)
		return nil
	})
	return resp, err
}

func validateBattleOriginal(req *inboxpb.BattleSettlementInboxRequest) error {
	if req == nil || req.PlayerId == 0 || req.BattleId == 0 || ValidateText(req.ExpectedOwnerAccount, 191, true) != 0 || req.ExpectedOwnerRevision == 0 || req.IssuedAtMs == 0 || len(req.SettlementPayload) == 0 || len(req.SettlementPayload) > 16384 || battlePayloadHasUnknown(req.ProtoReflect()) {
		return fmt.Errorf("%w: Battle原件不完整", ErrBattleSettlementOriginal)
	}
	p := &battlepb.BattleSettlementData{}
	if err := proto.Unmarshal(req.SettlementPayload, p); err != nil {
		return fmt.Errorf("%w: Battle载荷不能解码", ErrBattleSettlementOriginal)
	}
	if p.PlayerId != req.PlayerId || p.BattleId != req.BattleId {
		return fmt.Errorf("%w: Battle载荷身份不符", ErrBattleSettlementOriginal)
	}
	if err := validateBattlePayload(p); err != nil {
		return fmt.Errorf("%w: Battle载荷语义不完整", ErrBattleSettlementOriginal)
	}
	return nil
}

func battleOriginalMatches(req *inboxpb.BattleSettlementInboxRequest, role *dspb.AccountRoleOwnership) bool {
	return role != nil && req.PlayerId == role.GetRole().GetPlayerId() && req.ExpectedOwnerAccount == role.GetAccount() && req.ExpectedOwnerRevision == role.GetOwnerRevision()
}

// resultForExisting 按判重规则把已存在的行折成结局。
func resultForExisting(row *inboxpb.PlayerInboxItemRow, digest []byte) EnqueueResult {
	if decideExisting(row.GetContentSha256(), digest) == existingDuplicate {
		return EnqueueResult{Seq: row.GetSeq(), Created: false, State: inboxpb.PlayerInboxItemState(row.GetState())}
	}
	return EnqueueResult{Code: constants.ErrIdempotencyConflict}
}

// issuedWithinWindow 判定 issued_at_ms ∈ [now − 7d, now + 5min](设计 §6.2 第 5 步)。
func issuedWithinWindow(issuedAtMs, nowMs uint64) bool {
	past := uint64(constants.IssuedAtMaxPast.Milliseconds())
	future := uint64(constants.IssuedAtMaxFuture.Milliseconds())
	if issuedAtMs > nowMs+future {
		return false
	}
	return nowMs < past || issuedAtMs >= nowMs-past
}

func validProducer(p inboxpb.PlayerInboxProducerKind) bool {
	_, ok := inboxpb.PlayerInboxProducerKind_name[int32(p)]
	return ok && p != inboxpb.PlayerInboxProducerKind_PLAYER_INBOX_PRODUCER_UNSPECIFIED
}

// enqueueResultLabel 把入箱结局折成 player_inbox_enqueue_total 的 result 标签(封闭集合)。
func enqueueResultLabel(res EnqueueResult, err error) string {
	if err != nil {
		return "error"
	}
	switch res.Code {
	case 0:
		if res.Created {
			return "created"
		}
		return "duplicate"
	case constants.ErrIdempotencyConflict:
		return "conflict"
	case constants.ErrFull:
		return "full"
	case constants.ErrDisabled:
		return "disabled"
	case constants.ErrUnknownType:
		return "unknown_type"
	case constants.ErrPayloadTooLarge:
		return "payload_too_large"
	case constants.ErrPayloadInvalid:
		return "payload_invalid"
	case constants.ErrTextTooLong:
		return "text_too_long"
	case constants.ErrIssuedOutOfWindow:
		return "out_of_window"
	case constants.ErrPlayerNotFound:
		return "player_not_found"
	default:
		return "invalid"
	}
}

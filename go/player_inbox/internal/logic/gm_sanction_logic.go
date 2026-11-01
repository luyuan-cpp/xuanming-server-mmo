// Package logic 是 player_inbox 的生产方逻辑。v1 唯一的生产方是运维接口 PlayerInboxAdmin.GmSanctionPlayer
// (docs/design/player-inbox-service.md §2.2、§6.1)。
package logic

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"player_inbox/internal/constants"
	"player_inbox/internal/inbox"

	base "proto/common/base"
	inboxpb "proto/player_inbox"
	"shared/rolefence"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// SanctionStore 是 GM 处置需要的存储能力(inbox.Store 实现它;单测注入假实现)。
type SanctionStore interface {
	Enqueue(ctx context.Context, p inbox.EnqueueParams, inTx func(*sql.Tx, uint64) error) (inbox.EnqueueResult, error)
	Registry() *inbox.Registry
}

// errUnknownTypeInAudit:审计回调拿不到类型所属的域(类型未登记)。
var errUnknownTypeInAudit = errors.New("player_inbox: audit for an unregistered message type")

// GmSanctionDeps 是 GmSanctionPlayer 的显式依赖。
type GmSanctionDeps struct {
	Store SanctionStore
	// Audit 在入箱事务里写审计行,生产为 inbox.InsertAudit。
	Audit func(ctx context.Context, tx *sql.Tx, a *inboxpb.PlayerInboxAudit) error
	// Now 是审计行 created_ms 的时间源,生产为 time.Now。
	Now func() time.Time
}

// GmSanctionPlayer 处理一次 GM 处置入箱(设计 §6.1)。鉴权(x-admin-token)已由拦截器完成。
//
// 顺序:
//  1. 字段校验:player_id / request_id / issued_at_ms 非 0,action 必选,operator(≤ 64B)与 reason(≤ 256B)必填;
//  2. 从 oneof 得到 msg_type 与载荷;attach_debt 先用服务端认定的 operator **覆盖** gm_operator,
//     再对最终载荷做大小与解码校验(覆盖值随 CurrencyDebtEntry 永久进玩家存档,不能绕过上限,设计 §3.4);
//  3. inbox.Store.Enqueue:同一个事务里写 player_inbox_audit(seq、operator、对端地址、理由);
//  4. 返回 seq / created / state。DUPLICATE 时 created=false,返回原 seq 与当前状态。
//
// 载荷用确定性序列化:内容摘要包含载荷,同一请求的重试必须得到同一份字节,否则会被判成幂等冲突。
// 业务拒绝 in-band 回 tip;库 / data_service 故障回 kPlayerInboxStoreUnavailable(fault)。角色身份这一次核不了不是故障,回 kPlayerInboxStaleOwner。
func GmSanctionPlayer(ctx context.Context, deps GmSanctionDeps, in *inboxpb.GmSanctionPlayerRequest, peerAddr string) *inboxpb.GmSanctionPlayerResponse {
	reject := func(code uint32) *inboxpb.GmSanctionPlayerResponse {
		return &inboxpb.GmSanctionPlayerResponse{ErrorMessage: &base.TipInfoMessage{Id: code}}
	}
	if in.GetPlayerId() == 0 || in.GetRequestId() == 0 || in.GetIssuedAtMs() == 0 {
		return reject(constants.ErrInvalidParameter)
	}
	if code := inbox.ValidateText(in.GetOperator(), constants.MaxOperatorBytes, true); code != 0 {
		return reject(code)
	}
	if code := inbox.ValidateText(in.GetReason(), constants.MaxReasonBytes, true); code != 0 {
		return reject(code)
	}
	msgType, payloadMsg, ok := sanctionPayload(in)
	if !ok {
		return reject(constants.ErrInvalidParameter)
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(payloadMsg)
	if err != nil {
		return reject(constants.ErrPayloadInvalid)
	}

	now := deps.Now
	if now == nil {
		now = time.Now
	}
	audit := deps.Audit
	if audit == nil {
		audit = inbox.InsertAudit
	}
	entry, known := deps.Store.Registry().Type(msgType)
	auditFn := func(tx *sql.Tx, seq uint64) error {
		if !known {
			// Enqueue 已在注册表校验里拒绝未登记类型,走不到这里;留着防止将来绕开校验时写出 domain=0 的审计。
			return errUnknownTypeInAudit
		}
		createdMs := now().UnixMilli()
		if createdMs < 0 {
			createdMs = 0
		}
		return audit(ctx, tx, &inboxpb.PlayerInboxAudit{
			PlayerId:  in.GetPlayerId(),
			Domain:    uint32(entry.Spec.GetDomain()),
			Seq:       seq,
			Action:    uint32(inboxpb.PlayerInboxAuditAction_PLAYER_INBOX_AUDIT_SANCTION),
			Operator:  in.GetOperator(),
			PeerAddr:  peerAddr,
			Reason:    in.GetReason(),
			CreatedMs: uint64(createdMs),
		})
	}

	res, err := deps.Store.Enqueue(ctx, inbox.EnqueueParams{
		PlayerID:   in.GetPlayerId(),
		Type:       msgType,
		Producer:   inboxpb.PlayerInboxProducerKind_PLAYER_INBOX_PRODUCER_GM,
		RequestID:  in.GetRequestId(),
		IssuedAtMs: in.GetIssuedAtMs(),
		Payload:    payload,
		Operator:   in.GetOperator(),
		// 条目的 note 存处置理由:ListItems / LookupByRequest 不必再去翻审计表就能看到「为什么」。
		Note: in.GetReason(),
	}, auditFn)
	log := logx.WithContext(ctx)
	if errors.Is(err, inbox.ErrOriginalRole) || errors.Is(err, rolefence.ErrUnavailable) {
		// 角色冻结、换了主,或者自愈读不到目录:事务已回滚,没有结局未知。回故障 tip 的话排空证明会被打成 Hold(规格 §3.4)。GM 稍后重试,同一 request_id 幂等。
		log.Infof("[player_inbox] GmSanctionPlayer role not verified player=%d request_id=%d type=%s operator=%q peer=%s: %v",
			in.GetPlayerId(), in.GetRequestId(), inbox.TypeLabel(msgType), in.GetOperator(), peerAddr, err)
		return reject(constants.ErrStaleOwner)
	}
	if err != nil {
		log.Errorf("[player_inbox] GmSanctionPlayer failed player=%d request_id=%d type=%s operator=%q peer=%s: %v",
			in.GetPlayerId(), in.GetRequestId(), inbox.TypeLabel(msgType), in.GetOperator(), peerAddr, err)
		return reject(constants.ErrStoreUnavailable)
	}
	if res.Code != 0 {
		log.Infof("[player_inbox] GmSanctionPlayer rejected code=%d player=%d request_id=%d type=%s operator=%q peer=%s",
			res.Code, in.GetPlayerId(), in.GetRequestId(), inbox.TypeLabel(msgType), in.GetOperator(), peerAddr)
		return reject(res.Code)
	}
	log.Infof("[player_inbox] GmSanctionPlayer ok player=%d request_id=%d type=%s seq=%d created=%t state=%d operator=%q peer=%s",
		in.GetPlayerId(), in.GetRequestId(), inbox.TypeLabel(msgType), res.Seq, res.Created, res.State, in.GetOperator(), peerAddr)
	return &inboxpb.GmSanctionPlayerResponse{Seq: res.Seq, Created: res.Created, State: res.State}
}

// sanctionPayload 把 oneof 映射成 (msg_type, 最终载荷)。attach_debt 返回的是**副本**,gm_operator 已被
// 服务端认定的 operator 覆盖,不改调用方的请求对象。action 缺失返回 ok=false。
func sanctionPayload(in *inboxpb.GmSanctionPlayerRequest) (inboxpb.PlayerInboxMessageType, proto.Message, bool) {
	switch a := in.GetAction().(type) {
	case *inboxpb.GmSanctionPlayerRequest_BlockGain:
		if a.BlockGain == nil {
			return 0, nil, false
		}
		return inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN, a.BlockGain, true
	case *inboxpb.GmSanctionPlayerRequest_UnblockGain:
		if a.UnblockGain == nil {
			return 0, nil, false
		}
		return inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_UNBLOCK_CURRENCY_GAIN, a.UnblockGain, true
	case *inboxpb.GmSanctionPlayerRequest_AttachDebt:
		if a.AttachDebt == nil {
			return 0, nil, false
		}
		debt := proto.Clone(a.AttachDebt).(*inboxpb.GmCurrencyDebtPayload)
		debt.GmOperator = in.GetOperator()
		return inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_ATTACH_CURRENCY_DEBT, debt, true
	default:
		return 0, nil, false
	}
}

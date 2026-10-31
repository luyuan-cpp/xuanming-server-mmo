package inbox

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"player_inbox/internal/constants"
	"player_inbox/internal/metrics"

	componentpb "proto/common/component"
	inboxpb "proto/player_inbox"
	"shared/inboxcontract"
)

// 本文件是存储层的纯判定函数:不碰库、不碰时钟,单测直接对着设计表断言(设计 §4.3、§6.2、§7.8、§7.9、§9)。
// store_* 与 reconcile.go 只负责读写与事务,读出之后「怎么判、写回什么」一律在这里。

// ContentSHA256 = sha256(msg_type ‖ issued_at_ms ‖ payload)(IB-2)。字节布局只在 shared/inboxcontract 写一份,
// 与生产方 Match 核对回执用的是同一个实现(2026-10-30,评审 B-3);这里只保留按类型的入口。
// issued_at_ms 在摘要里,所以生产方刷新它重试会得到 CONFLICT(设计 §6.2 生产方契约)。
func ContentSHA256(t inboxpb.PlayerInboxMessageType, issuedAtMs uint64, payload []byte) []byte {
	return inboxcontract.ContentSHA256(uint32(t), issuedAtMs, payload)
}

// IsTerminalState 报告条目状态是否为终态(只有 PENDING 不是)。
func IsTerminalState(s inboxpb.PlayerInboxItemState) bool {
	switch s {
	case inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_APPLIED,
		inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_REJECTED,
		inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_VOIDED,
		inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_SETTLED_UNKNOWN:
		return true
	default:
		return false
	}
}

// existingVerdict 是幂等键已存在时的结论:同摘要 = DUPLICATE,不同 = CONFLICT。
type existingVerdict uint8

const (
	existingDuplicate existingVerdict = iota + 1
	existingConflict
)

// decideExisting 比对已存在行的内容摘要与本次请求的摘要(IB-2)。
func decideExisting(stored, requested []byte) existingVerdict {
	if len(stored) == sha256.Size && bytes.Equal(stored, requested) {
		return existingDuplicate
	}
	return existingConflict
}

// outcomeToState 把消费方报告的结局映射成终态。非法结局返回 UNSPECIFIED。
func outcomeToState(o inboxpb.PlayerInboxOutcome) inboxpb.PlayerInboxItemState {
	switch o {
	case inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_APPLIED:
		return inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_APPLIED
	case inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_REJECTED:
		return inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_REJECTED
	case inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_SKIPPED:
		return inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_VOIDED
	case inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_UNKNOWN:
		return inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_SETTLED_UNKNOWN
	default:
		return inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_UNSPECIFIED
	}
}

// sameOutcome 是「终态与一条结局一致」的唯一定义:结局映射后的终态与 state 相同;REJECTED 另要求原因码相同
// (消费方回放的是账本里的原结局,原因码必然相同)。stateReason 是行上的 outcome_reason。
// Ack 的 ALREADY_SAME 判定与对账的环内核对(finalizedLedgerGap)共用它,两处口径不得分叉。
func sameOutcome(state inboxpb.PlayerInboxItemState, stateReason uint32, outcome inboxpb.PlayerInboxOutcome, reason uint32) bool {
	want := outcomeToState(outcome)
	if want == inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_UNSPECIFIED || state != want {
		return false
	}
	return want != inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_REJECTED || stateReason == reason
}

// ackDecision 是单条 Ack 报告的判定结果。
type ackDecision struct {
	result inboxpb.PlayerInboxAckResult
	// finalize 为真时把行从 PENDING 改成 newState(写 finalized_by=CONSUMER、decided_epoch、outcome_reason)。
	finalize bool
	newState inboxpb.PlayerInboxItemState
	// state 是处理后的行状态(回给消费方);行不存在时为 UNSPECIFIED。
	state inboxpb.PlayerInboxItemState
}

// decideAck 按设计 §7.8 的 Ack 表判定一条报告。row 为 nil 表示行不存在。
// 报告本身的合法性(seq 非 0、结局已知、升序)由调用方先校验。
//
//	| 行的现状                   | 报告                                   | 结果                       |
//	| 不存在                     | 任意                                   | UNKNOWN_SEQ(告警,不改)    |
//	| request_id 与报告不一致    | 任意                                   | MISMATCH(告警,不改;IB-9) |
//	| PENDING 且 delivery_count=0 | 任意                                  | NOT_DELIVERED(告警,不改)  |
//	| PENDING,没有指令           | SKIPPED                                | SKIP_NOT_REQUESTED(告警)   |
//	| PENDING                    | APPLIED / REJECTED / 有指令时的 SKIPPED / UNKNOWN | 改成对应终态,FINALIZED |
//	| 终态,与报告一致            | —                                      | ALREADY_SAME               |
//	| 终态,与报告不一致          | —                                      | CONFLICT(告警,永不翻转;IB-4) |
//
// 挂了作废指令、消费方却报 APPLIED:照收 APPLIED(管理员的取消晚了一步,以消费方已落盘的事实为准)。
// 「一致」按 sameOutcome:映射后的终态相同;REJECTED 另要求原因码相同。
func decideAck(row *inboxpb.PlayerInboxItemRow, report *inboxpb.PlayerInboxDecisionReport) ackDecision {
	if row == nil {
		return ackDecision{result: inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_UNKNOWN_SEQ}
	}
	state := inboxpb.PlayerInboxItemState(row.GetState())
	if row.GetRequestId() != report.GetRequestId() {
		return ackDecision{result: inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_MISMATCH, state: state}
	}
	want := outcomeToState(report.GetOutcome())
	if state == inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING {
		if row.GetDeliveryCount() == 0 {
			return ackDecision{result: inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_NOT_DELIVERED, state: state}
		}
		if report.GetOutcome() == inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_SKIPPED &&
			inboxpb.PlayerInboxDirective(row.GetDirective()) != inboxpb.PlayerInboxDirective_PLAYER_INBOX_DIRECTIVE_VOID {
			return ackDecision{result: inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_SKIP_NOT_REQUESTED, state: state}
		}
		return ackDecision{
			result:   inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_FINALIZED,
			finalize: true,
			newState: want,
			state:    want,
		}
	}
	if sameOutcome(state, row.GetOutcomeReason(), report.GetOutcome(), report.GetReason()) {
		return ackDecision{result: inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_ALREADY_SAME, state: state}
	}
	return ackDecision{result: inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_CONFLICT, state: state}
}

// ackOutcomeReason 是终结时写进 outcome_reason 的值:REJECTED 写消费方给的 tip id;
// SKIPPED 写消费方回报的作废原因(与行上 void_reason 同源);其余为 0。
func ackOutcomeReason(report *inboxpb.PlayerInboxDecisionReport) uint32 {
	switch report.GetOutcome() {
	case inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_REJECTED, inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_SKIPPED:
		return report.GetReason()
	default:
		return 0
	}
}

// voidAction 是运维作废一条条目的判定结果(设计 §7.9、IB-5)。
type voidAction uint8

const (
	// voidNotFound:条目不存在。
	voidNotFound voidAction = iota + 1
	// voidTerminal:已是终态 → kPlayerInboxItemStateConflict。
	voidTerminal
	// voidDirect:从未投递 → 在流行锁下直接改 VOIDED(finalized_by=ADMIN),载荷从未离开过收件箱。
	voidDirect
	// voidDirective:已投递 → 只挂 directive=VOID,由消费方裁决。
	voidDirective
	// voidAlreadyRequested:已投递且已挂过 VOID 指令 → 幂等成功,不改行。
	voidAlreadyRequested
	// voidForced:强制模式下账本证明成立 → 在流行锁下改 VOIDED(finalized_by=ADMIN)。
	voidForced
	// voidForceRefused:强制模式下行仍 PENDING、但账本证明不成立 → kPlayerInboxItemStateConflict,不改行。
	voidForceRefused
)

// decideVoid 判定作废动作。
func decideVoid(row *inboxpb.PlayerInboxItemRow) voidAction {
	if row == nil {
		return voidNotFound
	}
	if inboxpb.PlayerInboxItemState(row.GetState()) != inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING {
		return voidTerminal
	}
	if row.GetDeliveryCount() == 0 {
		return voidDirect
	}
	if inboxpb.PlayerInboxDirective(row.GetDirective()) == inboxpb.PlayerInboxDirective_PLAYER_INBOX_DIRECTIVE_VOID {
		return voidAlreadyRequested
	}
	return voidDirective
}

// validateAckReports 校验一次 Ack 的报告列表:1..AckMax 条、seq 非 0 且严格升序、结局已知。
func validateAckReports(reports []*inboxpb.PlayerInboxDecisionReport, maxReports int) bool {
	if len(reports) == 0 || len(reports) > maxReports {
		return false
	}
	var prev uint64
	for _, r := range reports {
		if r == nil || r.GetSeq() == 0 || r.GetSeq() <= prev {
			return false
		}
		if outcomeToState(r.GetOutcome()) == inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_UNSPECIFIED {
			return false
		}
		prev = r.GetSeq()
	}
	return true
}

// clampFetchItems 把请求的 max_items 归一到 [1, max]:0 取默认值。
func clampFetchItems(requested uint32, def, maxItems int) int {
	if requested == 0 {
		return def
	}
	return min(int(requested), maxItems)
}

// reconcileDiff 判定一条收件箱行与已落盘账本里该域游标之间的差异(设计 §9.3)。cursor 为 nil = 账本没有该域
// (水位 0、环空;proto 的 getter 对 nil 安全)。
//   - 收件箱仍 PENDING,但账本水位已过这个 seq → PENDING_BUT_DECIDED(确认丢了且玩家一直离线);
//   - 收件箱已是消费方终结的终态,但账本里没有它的结局 → FINAL_NOT_IN_LEDGER(回档或 Redis 丢数据)。
//     「没有」按 finalizedLedgerGap 判(与定时对账同一个谓词):水位没到这个 seq;或水位已过、seq 落在环该覆盖的
//     范围内却查无此 seq;或环里同 seq 记的是另一条 / 另一个结局 —— 后两种是丢掉的结局被之后的条目掩盖(评审 M6)。
//
// 管理员终结的行(finalized_by=ADMIN:从未投递直接作废,或经账本证明强制终结)不是消费方的结局,不比对。
func reconcileDiff(row *inboxpb.PlayerInboxItemRow, cursor *componentpb.PlayerInboxDomainCursor) inboxpb.PlayerInboxReconcileDiffKind {
	if inboxpb.PlayerInboxItemState(row.GetState()) == inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING {
		if row.GetSeq() <= cursor.GetDecidedThroughSeq() {
			return inboxpb.PlayerInboxReconcileDiffKind_PLAYER_INBOX_RECONCILE_PENDING_BUT_DECIDED
		}
		return inboxpb.PlayerInboxReconcileDiffKind_PLAYER_INBOX_RECONCILE_DIFF_UNSPECIFIED
	}
	if finalizedLedgerGap(finalizedRefOfRow(row), cursor) != gapNone {
		return inboxpb.PlayerInboxReconcileDiffKind_PLAYER_INBOX_RECONCILE_FINAL_NOT_IN_LEDGER
	}
	return inboxpb.PlayerInboxReconcileDiffKind_PLAYER_INBOX_RECONCILE_DIFF_UNSPECIFIED
}

// ── 入箱 ─────────────────────────────────────────────────────────────────

// decideEnqueueLocked 是入箱主事务持流行锁之后、写任何东西之前的判定(设计 §6.2 第 7.2–7.3 步)。
// existing 是锁内按唯一键复读到的行(nil = 不存在)。proceed=false 时 res 就是本次结局,事务不写任何东西。
//
// **先判重、后判满箱,顺序本身就是不变量**:原请求与重试赛跑时,输的一方锁内一定看得见赢的一方已提交的行,
// 拿到的是 DUPLICATE 而不是 FULL(设计 §8 #2)。
func decideEnqueueLocked(existing *inboxpb.PlayerInboxItemRow, digest []byte, openCount, maxPending uint32) (res EnqueueResult, proceed bool) {
	if existing != nil {
		return resultForExisting(existing, digest), false
	}
	if openCount >= maxPending {
		return EnqueueResult{Code: constants.ErrFull}, false
	}
	return EnqueueResult{}, true
}

// ── Fetch / Ack ──────────────────────────────────────────────────────────

// fetchNeedsLock 判定 Fetch 要不要走加锁路径(设计 §7.8 Fetch 第 0 步):流上还有未终结条目,
// 或消费方水位已领先 next_seq(CursorAhead,要在锁内抬号)。两者都不是就按快照回空,不写任何东西。
func fetchNeedsLock(snap *inboxpb.PlayerInboxStream, consumerDecidedThrough uint64) bool {
	return snap.GetOpenCount() > 0 || IsCursorAhead(snap.GetNextSeq(), consumerDecidedThrough)
}

// fetchStreamUpdate 是 Fetch 放行之后对流行的写回(设计 §7.8 Fetch 第 2、5 步):FIRST / ADVANCE 把 fence_epoch
// 写成请求的 epoch,CursorAhead 抬过的 next_seq 一并写回;两者都没有就不写(write=false,其余返回值无意义)。
func fetchStreamUpdate(v inboxpb.PlayerInboxFenceVerdict, lockedFence, reqEpoch, nextSeq uint64, raised bool) (write bool, fence, next uint64) {
	fence = lockedFence
	if FenceWrites(v) {
		fence = reqEpoch
	}
	return FenceWrites(v) || raised, fence, nextSeq
}

// splitPage 把按 LIMIT limit+1 读出的行切成一页:多读的那一行只用来判断 has_more,不返回。
func splitPage[T any](rows []T, limit int) (page []T, hasMore bool) {
	if limit < 0 {
		limit = 0
	}
	if len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

// fetchScanBound 给出锁路径 PENDING 扫描实际走过的主键范围(player_inbox_fetch_scanned_rows 的口径,设计 §4.2)。
// 扫描是「主键范围按 seq 升序 + state=PENDING 过滤 + LIMIT」:读满 LIMIT 时数据库停在最后读出那一行,
// 走过的是 seq ≤ 它的全部行(含中间被过滤掉的终态行);没读满说明整条流都走过了(whole=true)。
// read 是那条查询读出的全部行(含判断 has_more 的多读一行),limit 是查询的 LIMIT。
func fetchScanBound(read []*inboxpb.PlayerInboxItemRow, limit int) (bound uint64, whole bool) {
	if len(read) == 0 || len(read) < limit {
		return 0, true
	}
	return read[len(read)-1].GetSeq(), false
}

// subtractOpenCount 从 open_count 里减去刚终结的条数。open_count 小于终结数只可能是缺陷:
// 它只影响满箱判定与 Fetch 快路径,钳到 0 并报告 underflow,由调用方大声记录,不让整个事务失败。
func subtractOpenCount(openCount, finalized uint32) (left uint32, underflow bool) {
	if finalized > openCount {
		return 0, true
	}
	return openCount - finalized, false
}

// ackStreamUpdate 是 Ack 逐条判定之后对流行的写回(设计 §7.8 Ack):open_count 减去本次终结的条数,
// FIRST / ADVANCE 把 fence_epoch 写成请求的 epoch;既没有终结也不推进围栏时不写(write=false)。
func ackStreamUpdate(v inboxpb.PlayerInboxFenceVerdict, openCount, finalized uint32, lockedFence, reqEpoch uint64) (
	write bool, newOpen uint32, newFence uint64, underflow bool) {
	if finalized == 0 && !FenceWrites(v) {
		return false, openCount, lockedFence, false
	}
	newOpen, underflow = subtractOpenCount(openCount, finalized)
	newFence = lockedFence
	if FenceWrites(v) {
		newFence = reqEpoch
	}
	return true, newOpen, newFence, underflow
}

// ackMetricLabel 是一条 Ack 结果在 player_inbox_ack_total{result} 上的标签:以 UNKNOWN 收尾为 SETTLED_UNKNOWN 的
// FINALIZED 单列成 finalized_settled_unknown(设计 §9.4 的 warning 告警按它取),其余取结果名。
func ackMetricLabel(res *inboxpb.PlayerInboxAckItem) string {
	if res.GetResult() == inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_FINALIZED &&
		res.GetState() == inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_SETTLED_UNKNOWN {
		return ackLabelSettledUnknown
	}
	return AckResultLabel(res.GetResult())
}

// isAckAnomaly 报告一条 Ack 结果是不是协议异常(消费方会停用这名玩家的这个域,服务端记 ERROR):
// FINALIZED 与 ALREADY_SAME 之外的一切(设计 §7.7)。
func isAckAnomaly(r inboxpb.PlayerInboxAckResult) bool {
	return r != inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_FINALIZED &&
		r != inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_ALREADY_SAME
}

// ── 运维接口 ─────────────────────────────────────────────────────────────

// voidOutcome 是一次作废在回包与指标上的结局。
type voidOutcome struct {
	tip           uint32 // 0 = 成功
	state         inboxpb.PlayerInboxItemState
	voidRequested bool
	path          string // player_inbox_void_total{path};空 = 不记(未改动任何东西的拒绝)
}

// voidOutcomeOf 把作废动作折成回包字段与指标路径(设计 §7.9)。state 是事务里读到 / 改成的行状态。
func voidOutcomeOf(action voidAction, state inboxpb.PlayerInboxItemState) voidOutcome {
	switch action {
	case voidTerminal:
		return voidOutcome{tip: constants.ErrItemStateConflict, state: state}
	case voidDirect:
		return voidOutcome{state: state, path: "direct"}
	case voidDirective:
		return voidOutcome{state: state, voidRequested: true, path: "directive"}
	case voidAlreadyRequested:
		return voidOutcome{state: state, voidRequested: true, path: "already"}
	case voidForced:
		return voidOutcome{state: state, path: "force"}
	case voidForceRefused:
		return voidOutcome{tip: constants.ErrItemStateConflict, state: state}
	default: // voidNotFound
		return voidOutcome{tip: constants.ErrItemNotFound}
	}
}

// auditRowFor 组出要写的一整行审计(评审 L9)。条目终态行 30 天后物理删除、审计行永不删除,所以审计要能独自
// 回答「做了什么」:
//   - 身份(player_id / domain / seq)、动作、「谁、为什么」(operator / peer_addr / reason / created_ms)
//     与 void_reason 取调用方 a;void_reason 取请求的原因而不是条目列 —— 挂指令时条目列是在同一事务里刚改的,
//     调用方手里的条目快照可能还是旧值;
//   - msg_type / request_id / issued_at_ms / content_sha256 / payload 一律取自条目行 item,a 里的同名字段被忽略:
//     条目内容入箱后不再改,只认这一个来源(DRY),审计记下的就是实际投递 / 作废的那一份字节;
//   - shard 按 player_id 重算(同 InsertAudit 原口径)。
//
// a 与 item 的身份对不上返回 error(fail-closed):宁可让整个事务回滚,也不写出一行张冠李戴的审计。
func auditRowFor(item *inboxpb.PlayerInboxItemRow, a *inboxpb.PlayerInboxAudit) (*inboxpb.PlayerInboxAudit, error) {
	if item == nil || a == nil {
		return nil, errors.New("inbox: audit needs both the item row and the audit fields")
	}
	if item.GetPlayerId() != a.GetPlayerId() || item.GetDomain() != a.GetDomain() || item.GetSeq() != a.GetSeq() {
		return nil, fmt.Errorf("inbox: audit for player=%d domain=%d seq=%d does not match item player=%d domain=%d seq=%d",
			a.GetPlayerId(), a.GetDomain(), a.GetSeq(), item.GetPlayerId(), item.GetDomain(), item.GetSeq())
	}
	// 零字节载荷(如 currency_type=0 的封锁)写成空串而不是 NULL,与条目行同一口径(store_enqueue.go)。
	payload, digest := item.GetPayload(), item.GetContentSha256()
	if payload == nil {
		payload = []byte{}
	}
	if digest == nil {
		digest = []byte{}
	}
	return &inboxpb.PlayerInboxAudit{
		Shard:         RowShard(a.GetPlayerId()),
		PlayerId:      a.GetPlayerId(),
		Domain:        a.GetDomain(),
		Seq:           a.GetSeq(),
		Action:        a.GetAction(),
		Operator:      a.GetOperator(),
		PeerAddr:      a.GetPeerAddr(),
		Reason:        a.GetReason(),
		CreatedMs:     a.GetCreatedMs(),
		MsgType:       item.GetMsgType(),
		RequestId:     item.GetRequestId(),
		IssuedAtMs:    item.GetIssuedAtMs(),
		ContentSha256: digest,
		Payload:       payload,
		VoidReason:    a.GetVoidReason(),
	}, nil
}

// ── 强制作废(VoidItem mode=FORCE_MISMATCH)──────────────────────────────

const (
	forceVoidUnspecified     = inboxpb.PlayerInboxForceVoidVerdict_PLAYER_INBOX_FORCE_VOID_UNSPECIFIED
	forceVoidProven          = inboxpb.PlayerInboxForceVoidVerdict_PLAYER_INBOX_FORCE_VOID_PROVEN
	forceVoidNotDelivered    = inboxpb.PlayerInboxForceVoidVerdict_PLAYER_INBOX_FORCE_VOID_NOT_DELIVERED
	forceVoidLedgerAbsent    = inboxpb.PlayerInboxForceVoidVerdict_PLAYER_INBOX_FORCE_VOID_LEDGER_ABSENT
	forceVoidNotDecided      = inboxpb.PlayerInboxForceVoidVerdict_PLAYER_INBOX_FORCE_VOID_NOT_DECIDED
	forceVoidSeqNotInRing    = inboxpb.PlayerInboxForceVoidVerdict_PLAYER_INBOX_FORCE_VOID_SEQ_NOT_IN_RING
	forceVoidRequestInLedger = inboxpb.PlayerInboxForceVoidVerdict_PLAYER_INBOX_FORCE_VOID_REQUEST_IN_LEDGER
	forceVoidLedgerInvalid   = inboxpb.PlayerInboxForceVoidVerdict_PLAYER_INBOX_FORCE_VOID_LEDGER_INVALID
)

// forceVoidProof 是强制作废的账本证明:结论,加两个只用于日志留证的读数。
type forceVoidProof struct {
	verdict inboxpb.PlayerInboxForceVoidVerdict
	// decidedThrough 是账本该域的水位(账本缺失、结构不合法或没有该域时为 0)。
	decidedThrough uint64
	// ringRequestID 是环里同 seq 那一条记的 request_id(没有这一条时为 0)。
	ringRequestID uint64
}

// decideForceVoidLocked 是强制作废在流行锁内的判定:行不存在 / 已是终态沿用普通模式的结论,不看账本;
// 仍 PENDING 时按 proveNeverExecuted 分流:证明成立 → voidForced,否则 → voidForceRefused(proof.verdict 给原因)。
func decideForceVoidLocked(row *inboxpb.PlayerInboxItemRow, ledger *componentpb.PlayerInboxLedgerComp, found bool) (
	voidAction, forceVoidProof) {
	if row == nil {
		return voidNotFound, forceVoidProof{}
	}
	if inboxpb.PlayerInboxItemState(row.GetState()) != inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING {
		return voidTerminal, forceVoidProof{}
	}
	proof := proveNeverExecuted(row, ledger, found)
	if proof.verdict != forceVoidProven {
		return voidForceRefused, proof
	}
	return voidForced, proof
}

// proveNeverExecuted 用已落盘账本证明一条已投递的 PENDING 行在这一消费方谱系上从未执行(设计 §7.9 强制终结)。
// row 是锁内复读的行(调用方已确认 PENDING);ledger / found 是事务外读到的已落盘账本。
//
// 唯一放行的结论是 PROVEN:本行已投递,账本该域 decided_through ≥ seq,且最近结局环里同 seq 那一条记的是**另一个**
// request_id。消费方按 seq 升序推进水位、每个 seq 只记一次结局(C++ player_inbox_rules::RecordDecision 拒绝
// seq ≤ 水位的记账),所以它在这个 seq 上裁决的是别的条目;水位已过本行,以后再投递到也只会判 MISMATCH,
// 不会执行(ClassifyDelivery)。终结本行不翻转任何已执行的结局(IB-4),也不吞掉任何本该回放的结局(IB-9)。
//
// 其余一律拒绝(fail-closed):
//   - 环里任何一条的 request_id 等于本行:同 seq = 回放(或类型不符的那种 MISMATCH),别的 seq = 同一处置曾以别的 seq
//     被裁决过 —— 都可能已执行 → REQUEST_IN_LEDGER;
//   - 水位已过而环里没有这个 seq(UNKNOWN 一类,原结局已挤出环):可能已执行 → SEQ_NOT_IN_RING;
//   - 账本没有该域或水位没到:消费方还没裁决它,停用原因不是 MISMATCH → NOT_DECIDED;
//   - 读不到存档 → LEDGER_ABSENT;账本结构不合法(validLedger,含某域环超过 ledgerRecentCapacity 条)→ LEDGER_INVALID:
//     那种账本在 scene 侧被 ValidateLedger 整份判坏,根本走不到回放核对,更不能拿来当证据(评审 R2)。
func proveNeverExecuted(row *inboxpb.PlayerInboxItemRow, ledger *componentpb.PlayerInboxLedgerComp, found bool) forceVoidProof {
	if row.GetDeliveryCount() == 0 {
		return forceVoidProof{verdict: forceVoidNotDelivered}
	}
	if !found || ledger == nil {
		return forceVoidProof{verdict: forceVoidLedgerAbsent}
	}
	if !validLedger(ledger) {
		return forceVoidProof{verdict: forceVoidLedgerInvalid}
	}
	cursor := findCursor(ledger, row.GetDomain())
	if cursor == nil {
		return forceVoidProof{verdict: forceVoidNotDecided}
	}
	proof := forceVoidProof{decidedThrough: cursor.GetDecidedThroughSeq()}
	if proof.decidedThrough < row.GetSeq() {
		proof.verdict = forceVoidNotDecided
		return proof
	}
	seqDecided := false
	for _, d := range cursor.GetRecent() {
		if d.GetSeq() == row.GetSeq() {
			seqDecided, proof.ringRequestID = true, d.GetRequestId()
		}
		if d.GetRequestId() == row.GetRequestId() {
			proof.verdict = forceVoidRequestInLedger
			return proof
		}
	}
	if !seqDecided {
		proof.verdict = forceVoidSeqNotInRing
		return proof
	}
	proof.verdict = forceVoidProven
	return proof
}

// findCursor 按 domain 字段找账本里该域的游标(不靠下标,AGENTS §11.6);没有返回 nil。
// 域唯一由 validLedger 先保证。
func findCursor(ledger *componentpb.PlayerInboxLedgerComp, domain uint32) *componentpb.PlayerInboxDomainCursor {
	for _, c := range ledger.GetCursors() {
		if c.GetDomain() == domain {
			return c
		}
	}
	return nil
}

// validLedger 校验已落盘账本能不能当证据(强制作废的证明、两个对账入口共用),整份检查,
// 与 C++ player_inbox_rules::ValidateLedger 逐条同口径:各域 domain 非 0 且唯一;每个域的环不超过
// ledgerRecentCapacity 条;环 seq 非 0、严格升序、都不超过 decided_through,结局是可记账的 APPLIED / REJECTED / SKIPPED。
// 另加一条 request_id 非 0:入箱拒收 request_id=0,环里出现 0 只可能是数据已坏,把它当成「另一个 request_id」
// 会凭空造出证明。
//
// 环长也要查(评审 R2):超长的环 scene 侧整份判坏、挂停用,消费方自己都不认这份账本,它里面的「同 seq 另一个
// request_id」证明不了任何事;对账的空洞判定(finalizedLedgerGap 的「环满」边界)同样以环长 ≤ 容量为前提。
// 别的域坏了也整份拒绝:scene 侧同样整份判坏,而且同一份 blob 里出现坏数据时,这个域的读数也不该再信。
func validLedger(ledger *componentpb.PlayerInboxLedgerComp) bool {
	seen := make(map[uint32]bool, len(ledger.GetCursors()))
	for _, c := range ledger.GetCursors() {
		if c.GetDomain() == 0 || seen[c.GetDomain()] || len(c.GetRecent()) > ledgerRecentCapacity {
			return false
		}
		seen[c.GetDomain()] = true
		var prev uint64
		for _, d := range c.GetRecent() {
			if d.GetSeq() == 0 || d.GetSeq() <= prev || d.GetSeq() > c.GetDecidedThroughSeq() ||
				!isRecordableOutcome(d.GetOutcome()) || d.GetRequestId() == 0 {
				return false
			}
			prev = d.GetSeq()
		}
	}
	return true
}

// isRecordableOutcome 报告一个结局能不能进最近结局环(UNKNOWN 不入环,同 C++ IsRecordableOutcome)。账本里存 uint32。
func isRecordableOutcome(o uint32) bool {
	switch inboxpb.PlayerInboxOutcome(o) {
	case inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_APPLIED,
		inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_REJECTED,
		inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_SKIPPED:
		return true
	default:
		return false
	}
}

// pendingAggregate 是一条流上 PENDING 行的聚合(GetSummary 用)。
type pendingAggregate struct {
	deliveredUnacked uint32
	oldestCreatedMs  uint64
}

// summarizeStreams 组装 GetSummary 的回包(设计 §5.2):按读出顺序列出各域流的概况,PENDING 聚合按域对上
// (没有 PENDING 行的域聚合为 0);max_fence_epoch 取各流 fence_epoch 的最大值,供 epoch-raise 取最大值(§7.8、C4)。
func summarizeStreams(playerID uint64, streams []*inboxpb.PlayerInboxStream, aggs map[uint32]pendingAggregate) *inboxpb.GetPlayerInboxSummaryResponse {
	resp := &inboxpb.GetPlayerInboxSummaryResponse{PlayerId: playerID}
	for _, st := range streams {
		a := aggs[st.GetDomain()]
		resp.Streams = append(resp.Streams, &inboxpb.PlayerInboxStreamSummary{
			Domain:                 inboxpb.PlayerInboxDomain(st.GetDomain()),
			OpenCount:              st.GetOpenCount(),
			NextSeq:                st.GetNextSeq(),
			FenceEpoch:             st.GetFenceEpoch(),
			DeliveredUnackedCount:  a.deliveredUnacked,
			OldestPendingCreatedMs: a.oldestCreatedMs,
			NotifyDueMs:            st.GetNotifyDueMs(),
		})
		resp.MaxFenceEpoch = max(resp.MaxFenceEpoch, st.GetFenceEpoch())
	}
	return resp
}

// ── 后台循环 ─────────────────────────────────────────────────────────────

// shardGaugeScan 是 ScanGauges 在一个分片上的读数。「最老」类读数为 0 表示该分片没有对应的行。
// deliveredUnacked / maxDeliveryCount / minDeliveredMs 只算最后投递不早于 GaugeCutoffs.DeliveredStaleBeforeMs 的行,
// 更早的已投递未确认行只计入 deliveredUnackedStale(评审 M4,由 SQL 一侧分好)。
type shardGaugeScan struct {
	openItems             uint64
	deliveredUnacked      uint64
	deliveredUnackedStale uint64
	maxDeliveryCount      uint64
	minCreatedMs          uint64 // PENDING 行最早的 created_ms
	minDeliveredMs        uint64 // 近期已投递 PENDING 行最早的 last_delivered_ms
	notifyBacklog         uint64
}

// foldScanGauges 把各分片读数折成全局扫描型 gauge(设计 §9.4):计数相加,max_delivery_count 取最大,
// 两个「最老」取各分片非 0 的最小时刻再换算成距 nowMs 的秒数(全部为 0 = 没有这类行,gauge 为 0)。
//
// 已投递未确认的「最老」按最后一次投递时刻算:被反复重投的行会刷新它,崩溃循环另由 max_delivery_count 告警;
// 消费方 halt 之后不再拉取,它就一直变老 —— 这正是要抓的「卡住」信号。但它只在 24 小时窗口内变老(评审 M4):
// 玩家被踢下线后再没回来的行,滑出窗口后进 delivered_unacked_stale,不再把告警永久顶住。
func foldScanGauges(nowMs uint64, shards []shardGaugeScan) metrics.ScanGauges {
	var g metrics.ScanGauges
	var oldestCreated, oldestDelivered uint64
	for _, s := range shards {
		g.OpenItems += s.openItems
		g.DeliveredUnacked += s.deliveredUnacked
		g.DeliveredUnackedStale += s.deliveredUnackedStale
		g.NotifyBacklog += s.notifyBacklog
		g.MaxDeliveryCount = max(g.MaxDeliveryCount, s.maxDeliveryCount)
		oldestCreated = minNonZero(oldestCreated, s.minCreatedMs)
		oldestDelivered = minNonZero(oldestDelivered, s.minDeliveredMs)
	}
	g.OldestOpenAgeSeconds = ageSeconds(nowMs, oldestCreated)
	g.DeliveredUnackedOldestSeconds = ageSeconds(nowMs, oldestDelivered)
	return g
}

// minNonZero 取两个时刻里非 0 的较小者(0 = 没有)。
func minNonZero(a, b uint64) uint64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	default:
		return min(a, b)
	}
}

// ageSeconds 是 sinceMs 距 nowMs 的秒数;sinceMs 为 0(没有这类行)或不早于 nowMs(时钟回拨)时为 0。
func ageSeconds(nowMs, sinceMs uint64) float64 {
	if sinceMs == 0 || sinceMs >= nowMs {
		return 0
	}
	return float64(nowMs-sinceMs) / 1000
}

// 补发没落定时的退避区间(评审 L6)。
const (
	notifyRetryMin = 30 * time.Second
	notifyRetryMax = 30 * time.Minute
	// notifyNodeMissingSettleAfter:NotifyNodeMissing(镜像已同步却查不到节点)悬了这么久仍查不到,补发就落定(评审 R3)。
	// 取 1 小时:租约抖动 / watch 重连 / 注册延迟都在秒到分钟级,退避(30s → 30min 封顶)期间重试 6–7 次足够等到它恢复;
	// scene 侧在线有界兜底默认 30 分钟 ±20%(最长 36 分钟),真在线的玩家到这时已自己拉过。再往后只剩
	// 「location 停在一个真没了的节点上、玩家没再落地」,留着只会一直把积压告警顶亮,交给落地拉取。
	notifyNodeMissingSettleAfter = time.Hour
)

// notifyGivesUp 报告补发这一次没落定的提醒是否就此落定(清 notify_due_ms 而不是推迟):
// 只有 NotifyNodeMissing、且已悬了 notifyNodeMissingSettleAfter 以上(nowMs − pendingSinceMs,口径同 notifyRetryDueMs)。
// pendingSinceMs 为 0 或不早于 nowMs(时钟回拨)时按刚开始悬算,不落定。
func notifyGivesUp(result NotifyResult, nowMs, pendingSinceMs uint64) bool {
	if result != NotifyNodeMissing || pendingSinceMs == 0 || pendingSinceMs >= nowMs {
		return false
	}
	return nowMs-pendingSinceMs >= uint64(notifyNodeMissingSettleAfter.Milliseconds())
}

// notifyRetryDueMs 是一次补发没落定之后 notify_due_ms 的新值:nowMs + 退避,退避 = 这条提醒已经悬了多久
// (nowMs − pendingSinceMs),钳到 [30s, 30min]。每次没落定都把「已悬时长」再等一遍,重试间隔因此按 2 倍递增
// (指数退避),不需要新增列记退避级数。
//
// pendingSinceMs 是流行的 updated_ms。它不晚于这条提醒真正开始悬着的时刻:notify_due_ms 只由入箱置成 now,
// 入箱同一条语句把 updated_ms 也写成同一个 now;之后 Fetch / Ack / 作废只会把 updated_ms 往后推(退避回落到下限,
// 只是多试几次),清零与推迟都不碰它。所以退避永远不超过按真实悬挂时长算出的值。
// pendingSinceMs 为 0 或不早于 nowMs(时钟回拨)时取下限。
func notifyRetryDueMs(nowMs, pendingSinceMs uint64) uint64 {
	minMs, maxMs := uint64(notifyRetryMin.Milliseconds()), uint64(notifyRetryMax.Milliseconds())
	backoff := minMs
	if pendingSinceMs != 0 && pendingSinceMs < nowMs {
		backoff = min(max(nowMs-pendingSinceMs, minMs), maxMs)
	}
	return nowMs + backoff
}

// ── 对账 ─────────────────────────────────────────────────────────────────

// ledgerRecentCapacity 是消费方每域最近结局环的容量。**三处必须同值**:C++ player_inbox_rules.h 的
// kRecentCapacity(32;RecordDecision 超出即挤掉最旧的,ValidateLedger 拒绝更长的环)、Java 版落码时的对应常量
// (PARITY.md 收件箱一行,目前待做)、这里。对账靠它区分两种环:不满 = 从没裁剪过,每个记过账的结局都还在环里;
// 满 = 更早的结局可能已被挤出,早于环头的 seq 无从核对。validLedger 拒绝超过它的环(同 C++ ValidateLedger)。
// reconcile_test.go 的 TestLedgerRecentCapacityMatchesConsumer 从 C++ 头文件里读出 kRecentCapacity 逐值比对。
const ledgerRecentCapacity = 32

// ledgerGap 是一条由消费方终结的行在已落盘账本里的缺口种类。只进日志;对外的 diff kind 一律是 FINAL_NOT_IN_LEDGER。
type ledgerGap uint8

const (
	// gapNone:账本里有这一行的结局,或无从核对(已挤出满环、SETTLED_UNKNOWN、不是消费方终结的行)。
	gapNone ledgerGap = iota
	// gapBeyondWatermark:账本水位没到这个 seq(回档、Redis 丢尾)。
	gapBeyondWatermark
	// gapMissingInRing:水位已过,seq 落在环该覆盖的范围内,环里却查无此 seq —— 丢掉的结局被之后的条目掩盖(评审 M6)。
	gapMissingInRing
	// gapRingMismatch:环里同 seq 记的是另一条(request_id / msg_type 不同)或另一个结局。
	gapRingMismatch
)

func (g ledgerGap) String() string {
	switch g {
	case gapBeyondWatermark:
		return "beyond_watermark"
	case gapMissingInRing:
		return "missing_in_ring"
	case gapRingMismatch:
		return "ring_mismatch"
	default:
		return "none"
	}
}

// finalizedLedgerGap 是两个对账入口(ReconcilePlayer 的 reconcileDiff、定时对账的 ledgerMismatches)共用的谓词:
// 一条收件箱行的结局在已落盘账本里有没有对应(设计 §9.3、评审 M6)。cursor 是账本里该域的游标(nil = 没有该域),
// 账本须先过 validLedger(环按 seq 严格升序、≤ 32 条)。按顺序:
//
//  1. 不是消费方终结的终态行(PENDING、finalized_by=ADMIN)→ 不比对;
//  2. seq > decided_through → gapBeyondWatermark(四种终态都比,含 SETTLED_UNKNOWN);
//  3. SETTLED_UNKNOWN:它的原结局本就不在环里(UNKNOWN 不入环)→ 不再往下比;
//  4. 环满 32 条且 seq 早于环头(环已验证升序,recent[0] 即最小 seq)→ 已挤出环,无从核对;
//  5. 其余必须在环里**按 seq 字段**(不靠下标,AGENTS §11.6)找到同一条:request_id、msg_type 相同且结局一致
//     (sameOutcome:VOIDED ↔ SKIPPED,REJECTED 另比原因码)。找不到 → gapMissingInRing,对不上 → gapRingMismatch。
//
// 第 5 步的依据:消费方按 seq 升序裁决、每次裁决都入环(RecordDecision),水位 = 最后一次裁决的 seq;环不满说明从没裁剪过。
// 所以水位以内、环覆盖范围以内的每个消费方结局(APPLIED / REJECTED / SKIPPED)都必须还在环里 ——
// 这样「首次处置的落盘就丢了、之后的条目把水位推过去」(环里只有后来那条)也抓得到。
func finalizedLedgerGap(f FinalizedRef, cursor *componentpb.PlayerInboxDomainCursor) ledgerGap {
	if f.FinalizedBy != inboxpb.PlayerInboxFinalizer_PLAYER_INBOX_FINALIZER_CONSUMER || !IsTerminalState(f.State) {
		return gapNone
	}
	if f.Seq > cursor.GetDecidedThroughSeq() {
		return gapBeyondWatermark
	}
	if f.State == inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_SETTLED_UNKNOWN {
		return gapNone
	}
	recent := cursor.GetRecent()
	if len(recent) >= ledgerRecentCapacity && f.Seq < recent[0].GetSeq() {
		return gapNone
	}
	for _, d := range recent {
		if d.GetSeq() != f.Seq {
			continue
		}
		if d.GetRequestId() != f.RequestID || d.GetMsgType() != uint32(f.MsgType) ||
			!sameOutcome(f.State, f.OutcomeReason, inboxpb.PlayerInboxOutcome(d.GetOutcome()), d.GetReason()) {
			return gapRingMismatch
		}
		return gapNone
	}
	return gapMissingInRing
}

// finalizedRefOfRow 取一条条目行里对账要用的列(ReconcilePlayer 读整行,定时对账只读这几列)。
func finalizedRefOfRow(row *inboxpb.PlayerInboxItemRow) FinalizedRef {
	return FinalizedRef{
		PlayerID:      row.GetPlayerId(),
		Domain:        inboxpb.PlayerInboxDomain(row.GetDomain()),
		Seq:           row.GetSeq(),
		State:         inboxpb.PlayerInboxItemState(row.GetState()),
		RequestID:     row.GetRequestId(),
		MsgType:       inboxpb.PlayerInboxMessageType(row.GetMsgType()),
		OutcomeReason: row.GetOutcomeReason(),
		FinalizedBy:   inboxpb.PlayerInboxFinalizer(row.GetFinalizedBy()),
		UpdatedMs:     row.GetUpdatedMs(),
	}
}

// reconcileReport 是 ReconcilePlayer 读完之后的纯逻辑部分(设计 §9.3):
//   - cursors:按 domains 的顺序逐域列出账本水位与收件箱 next_seq;收件箱没有流、账本也没有水位的域不列;
//   - diffs:逐行按 reconcileDiff 与账本里该域的游标(含最近结局环)比对,只列有差异的行,顺序同 rows
//     (按 domain、seq 升序读出)。
//
// streams 按域给出流行(没有流的域不在 map 里);ledger 是已落盘账本(非 nil,调用方已确认 validLedger)。
func reconcileReport(domains []inboxpb.PlayerInboxDomain, streams map[inboxpb.PlayerInboxDomain]*inboxpb.PlayerInboxStream,
	ledger *componentpb.PlayerInboxLedgerComp, rows []*inboxpb.PlayerInboxItemRow) (
	cursors []*inboxpb.PlayerInboxReconcileCursor, diffs []*inboxpb.PlayerInboxReconcileDiff) {
	for _, d := range domains {
		st := streams[d]
		dt := DecidedThrough(ledger, d)
		if st == nil && dt == 0 {
			continue
		}
		cursors = append(cursors, &inboxpb.PlayerInboxReconcileCursor{
			Domain: d, LedgerDecidedThrough: dt, InboxNextSeq: st.GetNextSeq(),
		})
	}
	for _, row := range rows {
		d := inboxpb.PlayerInboxDomain(row.GetDomain())
		kind := reconcileDiff(row, findCursor(ledger, row.GetDomain()))
		if kind == inboxpb.PlayerInboxReconcileDiffKind_PLAYER_INBOX_RECONCILE_DIFF_UNSPECIFIED {
			continue
		}
		diffs = append(diffs, &inboxpb.PlayerInboxReconcileDiff{
			Domain:    d,
			Seq:       row.GetSeq(),
			Kind:      kind,
			State:     inboxpb.PlayerInboxItemState(row.GetState()),
			RequestId: row.GetRequestId(),
		})
	}
	return cursors, diffs
}

// ledgerMismatch 是定时对账发现的一处不一致:哪一行,缺口是哪一种(gap 只进日志)。
type ledgerMismatch struct {
	ref FinalizedRef
	gap ledgerGap
}

// ledgerMismatches 是定时对账对一名玩家的判定(设计 §9.3):refs 是这名玩家最近由消费方终结的行,
// 逐行按 finalizedLedgerGap 与账本里该域的游标比对(与 ReconcilePlayer 同一个谓词),返回有缺口的那些,顺序同 refs。
// ledger 须已过 validLedger。
func ledgerMismatches(refs []FinalizedRef, ledger *componentpb.PlayerInboxLedgerComp) []ledgerMismatch {
	var out []ledgerMismatch
	for _, f := range refs {
		if gap := finalizedLedgerGap(f, findCursor(ledger, uint32(f.Domain))); gap != gapNone {
			out = append(out, ledgerMismatch{ref: f, gap: gap})
		}
	}
	return out
}

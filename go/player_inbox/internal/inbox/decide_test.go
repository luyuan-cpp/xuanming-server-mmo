package inbox

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"player_inbox/internal/constants"
	"shared/authoritysql"

	componentpb "proto/common/component"
	inboxpb "proto/player_inbox"

	"github.com/go-sql-driver/mysql"
)

const (
	statePending  = inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING
	stateApplied  = inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_APPLIED
	stateRejected = inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_REJECTED
	stateVoided   = inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_VOIDED
	stateUnknown  = inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_SETTLED_UNKNOWN

	outApplied  = inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_APPLIED
	outRejected = inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_REJECTED
	outSkipped  = inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_SKIPPED
	outUnknown  = inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_UNKNOWN

	directiveVoid = inboxpb.PlayerInboxDirective_PLAYER_INBOX_DIRECTIVE_VOID
)

func row(state inboxpb.PlayerInboxItemState, delivered uint32, directive inboxpb.PlayerInboxDirective, requestID uint64) *inboxpb.PlayerInboxItemRow {
	return &inboxpb.PlayerInboxItemRow{
		Seq: 10, RequestId: requestID, State: uint32(state), DeliveryCount: delivered, Directive: uint32(directive),
	}
}

func report(outcome inboxpb.PlayerInboxOutcome, reason uint32, requestID uint64) *inboxpb.PlayerInboxDecisionReport {
	return &inboxpb.PlayerInboxDecisionReport{Seq: 10, Outcome: outcome, Reason: reason, RequestId: requestID}
}

// TestDecideAckTable 钉住设计 §7.8 的 Ack 表:七种结果 + 终结映射。
func TestDecideAckTable(t *testing.T) {
	const rid = uint64(777)
	none := inboxpb.PlayerInboxDirective_PLAYER_INBOX_DIRECTIVE_NONE
	cases := []struct {
		name     string
		row      *inboxpb.PlayerInboxItemRow
		report   *inboxpb.PlayerInboxDecisionReport
		want     inboxpb.PlayerInboxAckResult
		finalize bool
		state    inboxpb.PlayerInboxItemState
	}{
		{"行不存在", nil, report(outApplied, 0, rid), inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_UNKNOWN_SEQ, false, inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_UNSPECIFIED},
		{"request_id 不一致", row(statePending, 1, none, rid), report(outApplied, 0, rid+1), inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_MISMATCH, false, statePending},
		{"终态行 request_id 不一致也是 MISMATCH", row(stateApplied, 1, none, rid), report(outApplied, 0, rid+1), inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_MISMATCH, false, stateApplied},
		{"从未投递", row(statePending, 0, none, rid), report(outApplied, 0, rid), inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_NOT_DELIVERED, false, statePending},
		{"没有指令却报 SKIPPED", row(statePending, 1, none, rid), report(outSkipped, 1, rid), inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_SKIP_NOT_REQUESTED, false, statePending},
		{"PENDING → APPLIED", row(statePending, 1, none, rid), report(outApplied, 0, rid), inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_FINALIZED, true, stateApplied},
		{"PENDING → REJECTED", row(statePending, 2, none, rid), report(outRejected, 29003, rid), inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_FINALIZED, true, stateRejected},
		{"有 VOID 指令 + SKIPPED → VOIDED", row(statePending, 1, directiveVoid, rid), report(outSkipped, 1, rid), inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_FINALIZED, true, stateVoided},
		{"有 VOID 指令却报 APPLIED:照收 APPLIED", row(statePending, 1, directiveVoid, rid), report(outApplied, 0, rid), inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_FINALIZED, true, stateApplied},
		{"PENDING + UNKNOWN → SETTLED_UNKNOWN", row(statePending, 3, none, rid), report(outUnknown, 0, rid), inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_FINALIZED, true, stateUnknown},
		{"终态一致", row(stateApplied, 1, none, rid), report(outApplied, 0, rid), inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_ALREADY_SAME, false, stateApplied},
		{"终态不一致:永不翻转", row(stateApplied, 1, none, rid), report(outRejected, 5, rid), inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_CONFLICT, false, stateApplied},
		{"已作废但报 APPLIED", row(stateVoided, 1, directiveVoid, rid), report(outApplied, 0, rid), inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_CONFLICT, false, stateVoided},
		{"SETTLED_UNKNOWN 再报 UNKNOWN", row(stateUnknown, 1, none, rid), report(outUnknown, 0, rid), inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_ALREADY_SAME, false, stateUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decideAck(tc.row, tc.report)
			if d.result != tc.want || d.finalize != tc.finalize || d.state != tc.state {
				t.Fatalf("decideAck = {result:%v finalize:%v state:%v}, want {%v %v %v}",
					d.result, d.finalize, d.state, tc.want, tc.finalize, tc.state)
			}
			if d.finalize && d.newState != tc.state {
				t.Fatalf("newState = %v, want %v", d.newState, tc.state)
			}
		})
	}
}

// TestDecideAckRejectedComparesReason:终态 REJECTED 与报告的原因码不同算 CONFLICT(消费方回放的是账本原结局)。
func TestDecideAckRejectedComparesReason(t *testing.T) {
	r := row(stateRejected, 1, inboxpb.PlayerInboxDirective_PLAYER_INBOX_DIRECTIVE_NONE, 9)
	r.OutcomeReason = 29003
	if got := decideAck(r, report(outRejected, 29003, 9)).result; got != inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_ALREADY_SAME {
		t.Errorf("同原因应 ALREADY_SAME,got %v", got)
	}
	if got := decideAck(r, report(outRejected, 29004, 9)).result; got != inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_CONFLICT {
		t.Errorf("不同原因应 CONFLICT,got %v", got)
	}
}

func TestAckOutcomeReason(t *testing.T) {
	if got := ackOutcomeReason(report(outApplied, 7, 1)); got != 0 {
		t.Errorf("APPLIED 的 outcome_reason 应为 0,got %d", got)
	}
	if got := ackOutcomeReason(report(outRejected, 29003, 1)); got != 29003 {
		t.Errorf("REJECTED 应写消费方给的 tip id,got %d", got)
	}
	if got := ackOutcomeReason(report(outSkipped, 2, 1)); got != 2 {
		t.Errorf("SKIPPED 应写作废原因,got %d", got)
	}
}

// TestValidateAckReports:1..32 条、seq 非 0 且严格升序、结局已知。
func TestValidateAckReports(t *testing.T) {
	mk := func(seqs ...uint64) []*inboxpb.PlayerInboxDecisionReport {
		out := make([]*inboxpb.PlayerInboxDecisionReport, 0, len(seqs))
		for _, s := range seqs {
			out = append(out, &inboxpb.PlayerInboxDecisionReport{Seq: s, Outcome: outApplied, RequestId: 1})
		}
		return out
	}
	tooMany := make([]uint64, constants.AckMaxDecisions+1)
	for i := range tooMany {
		tooMany[i] = uint64(i + 1)
	}
	cases := []struct {
		name    string
		reports []*inboxpb.PlayerInboxDecisionReport
		want    bool
	}{
		{"一条", mk(5), true},
		{"升序", mk(1, 2, 10), true},
		{"空", nil, false},
		{"seq 为 0", mk(0), false},
		{"重复", mk(3, 3), false},
		{"降序", mk(4, 3), false},
		{"超过 32 条", mk(tooMany...), false},
		{"未知结局", []*inboxpb.PlayerInboxDecisionReport{{Seq: 1, Outcome: inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_UNSPECIFIED}}, false},
		{"空指针", []*inboxpb.PlayerInboxDecisionReport{nil}, false},
	}
	for _, tc := range cases {
		if got := validateAckReports(tc.reports, constants.AckMaxDecisions); got != tc.want {
			t.Errorf("%s: validateAckReports = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestDecideVoid 钉住设计 §7.9 / IB-5:未投递直接 VOIDED,已投递只挂指令。
func TestDecideVoid(t *testing.T) {
	none := inboxpb.PlayerInboxDirective_PLAYER_INBOX_DIRECTIVE_NONE
	cases := []struct {
		name string
		row  *inboxpb.PlayerInboxItemRow
		want voidAction
	}{
		{"不存在", nil, voidNotFound},
		{"终态", row(stateApplied, 1, none, 1), voidTerminal},
		{"从未投递 → 直接作废", row(statePending, 0, none, 1), voidDirect},
		{"已投递 → 挂指令", row(statePending, 2, none, 1), voidDirective},
		{"已挂过指令 → 幂等", row(statePending, 2, directiveVoid, 1), voidAlreadyRequested},
	}
	for _, tc := range cases {
		if got := decideVoid(tc.row); got != tc.want {
			t.Errorf("%s: decideVoid = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestContentSHA256 钉住摘要的输入:类型、签发时刻、载荷任一不同都会不同(issued_at 不同 → CONFLICT,IB-2)。
func TestContentSHA256(t *testing.T) {
	base := ContentSHA256(gmBlock, 1000, []byte{1, 2})
	if len(base) != 32 {
		t.Fatalf("摘要长度 %d,want 32", len(base))
	}
	if !bytes.Equal(base, ContentSHA256(gmBlock, 1000, []byte{1, 2})) {
		t.Fatal("同输入摘要必须相同")
	}
	for name, other := range map[string][]byte{
		"类型不同":   ContentSHA256(gmUnblock, 1000, []byte{1, 2}),
		"签发时刻不同": ContentSHA256(gmBlock, 1001, []byte{1, 2}),
		"载荷不同":   ContentSHA256(gmBlock, 1000, []byte{1, 3}),
	} {
		if bytes.Equal(base, other) {
			t.Errorf("%s:摘要相同", name)
		}
	}
	if decideExisting(base, base) != existingDuplicate {
		t.Error("同摘要应判 DUPLICATE")
	}
	if decideExisting(base, ContentSHA256(gmBlock, 1001, []byte{1, 2})) != existingConflict {
		t.Error("issued_at 不同应判 CONFLICT")
	}
	if decideExisting(nil, base) != existingConflict {
		t.Error("库里摘要缺失(数据异常)必须判 CONFLICT,不能当成重复")
	}
}

func TestResultForExisting(t *testing.T) {
	digest := ContentSHA256(gmBlock, 5, nil)
	r := &inboxpb.PlayerInboxItemRow{Seq: 42, State: uint32(stateApplied), ContentSha256: digest}
	got := resultForExisting(r, digest)
	if got.Code != 0 || got.Created || got.Seq != 42 || got.State != stateApplied {
		t.Fatalf("DUPLICATE 应返回原 seq 与当前状态,got %+v", got)
	}
	if got := resultForExisting(r, ContentSHA256(gmBlock, 6, nil)); got.Code != constants.ErrIdempotencyConflict {
		t.Fatalf("不同摘要应 CONFLICT,got %+v", got)
	}
}

// TestIssuedWithinWindow 钉住 [now−7d, now+5min](设计 §6.2 第 5 步)。
func TestIssuedWithinWindow(t *testing.T) {
	now := testNowMs
	past := uint64(constants.IssuedAtMaxPast.Milliseconds())
	future := uint64(constants.IssuedAtMaxFuture.Milliseconds())
	cases := []struct {
		issued uint64
		want   bool
	}{
		{now, true},
		{now - past, true},
		{now - past - 1, false},
		{now + future, true},
		{now + future + 1, false},
	}
	for _, tc := range cases {
		if got := issuedWithinWindow(tc.issued, now); got != tc.want {
			t.Errorf("issuedWithinWindow(%d, %d) = %v, want %v", tc.issued, now, got, tc.want)
		}
	}
}

// TestRetryClassifierCoversRequiredCodes:可重试判定必须覆盖 1213 / 1205 / 9007,且显式注入进事务参数(设计 §10;
// assetop 的 IsRetryable 为 nil 时一次都不重试)。守卫判定的提交前中止(R6,COMMIT 没发、确定没写)也可整笔重跑,
// 结局未知不行。
func TestRetryClassifierCoversRequiredCodes(t *testing.T) {
	for _, code := range []uint16{1213, 1205, 9007} {
		err := fmt.Errorf("wrapped: %w", &mysql.MySQLError{Number: code})
		if !IsRetryableTxError(err) {
			t.Errorf("MySQL %d 应可重试", code)
		}
	}
	if !IsRetryableTxError(fmt.Errorf("wrapped: %w", authoritysql.ErrTxAbortedBeforeCommit)) {
		t.Error("aborted-before-commit must be retryable")
	}
	for _, err := range []error{
		&mysql.MySQLError{Number: 1062},
		&mysql.MySQLError{Number: 1146},
		fmt.Errorf("commit: %w", authoritysql.ErrOutcomeUnknown),
		errors.New("plain"),
		nil,
	} {
		if IsRetryableTxError(err) {
			t.Errorf("%v 不应判可重试", err)
		}
	}
	cfg := TxRetryConfig()
	if cfg.IsRetryable == nil {
		t.Fatal("TxRetryConfig 必须注入 IsRetryable,否则 WithTxRetryConfig 一次都不重试")
	}
	if !cfg.IsRetryable(&mysql.MySQLError{Number: 9007}) {
		t.Fatal("注入的判定必须覆盖 9007(TiDB 写冲突)")
	}
	if cfg.Attempts < 2 {
		t.Fatalf("Attempts = %d,至少要重试一次", cfg.Attempts)
	}
}

func TestIsDuplicateKeyError(t *testing.T) {
	if !isDuplicateKeyError(fmt.Errorf("x: %w", &mysql.MySQLError{Number: 1062})) {
		t.Error("1062 应识别为重复键")
	}
	if isDuplicateKeyError(&mysql.MySQLError{Number: 1213}) {
		t.Error("1213 不是重复键")
	}
}

func TestClampFetchItems(t *testing.T) {
	cases := map[uint32]int{0: 16, 1: 1, 16: 16, 17: 16, 1000: 16}
	for in, want := range cases {
		if got := clampFetchItems(in, constants.FetchDefaultItems, constants.FetchMaxItems); got != want {
			t.Errorf("clampFetchItems(%d) = %d, want %d", in, got, want)
		}
	}
}

// TestReconcileDiff 钉住 ReconcilePlayer 的逐行判定(设计 §9.3):两类差异;终态行经 finalizedLedgerGap
// 比水位也比最近结局环(评审 M6,谓词全表见 TestFinalizedLedgerGap);管理员终结的行不比对。
// 账本是真实形状:消费方按 seq 升序裁决过 10(request 1)与 20(request 2),水位 = 环尾 20。
func TestReconcileDiff(t *testing.T) {
	const (
		none   = inboxpb.PlayerInboxReconcileDiffKind_PLAYER_INBOX_RECONCILE_DIFF_UNSPECIFIED
		final  = inboxpb.PlayerInboxReconcileDiffKind_PLAYER_INBOX_RECONCILE_FINAL_NOT_IN_LEDGER
		decide = inboxpb.PlayerInboxReconcileDiffKind_PLAYER_INBOX_RECONCILE_PENDING_BUT_DECIDED
	)
	cursor := rcCursor(gmDomain, rcDecision(10, 1, outApplied, 0), rcDecision(20, 2, outApplied, 0))
	cases := []struct {
		name   string
		row    *inboxpb.PlayerInboxItemRow
		cursor *componentpb.PlayerInboxDomainCursor
		want   inboxpb.PlayerInboxReconcileDiffKind
	}{
		{"终态且在环里", rcRow(10, 1, stateApplied, byConsumer), cursor, none},
		{"终态但账本没到:回档或丢数据", rcRow(21, 3, stateApplied, byConsumer), cursor, final},
		{"终态、水位已过、环里查无此 seq:丢掉的结局被之后的条目掩盖", rcRow(15, 4, stateApplied, byConsumer), cursor, final},
		{"终态、环里同 seq 是另一条", rcRow(20, 9, stateApplied, byConsumer), cursor, final},
		{"PENDING 且账本没到:正常", rcRow(21, 3, statePending, 0), cursor, none},
		{"PENDING 但账本已过:确认丢了", rcRow(20, 2, statePending, 0), cursor, decide},
		{"管理员作废:不比对", rcRow(15, 4, stateVoided, byAdmin), cursor, none},
		{"消费方 SKIPPED 的 VOIDED 也比对", rcRow(21, 3, stateVoided, byConsumer), cursor, final},
		{"账本没有该域:终态按水位 0 报", rcRow(10, 1, stateApplied, byConsumer), nil, final},
		{"账本没有该域:PENDING 不报", rcRow(10, 1, statePending, 0), nil, none},
	}
	for _, tc := range cases {
		if got := reconcileDiff(tc.row, tc.cursor); got != tc.want {
			t.Errorf("%s: reconcileDiff = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestEnqueueResultLabel(t *testing.T) {
	cases := []struct {
		res  EnqueueResult
		err  error
		want string
	}{
		{EnqueueResult{Created: true}, nil, "created"},
		{EnqueueResult{}, nil, "duplicate"},
		{EnqueueResult{Code: constants.ErrFull}, nil, "full"},
		{EnqueueResult{Code: constants.ErrIdempotencyConflict}, nil, "conflict"},
		{EnqueueResult{Code: constants.ErrInvalidParameter}, nil, "invalid"},
		{EnqueueResult{}, errors.New("db down"), "error"},
	}
	for _, tc := range cases {
		if got := enqueueResultLabel(tc.res, tc.err); got != tc.want {
			t.Errorf("enqueueResultLabel(%+v, %v) = %q, want %q", tc.res, tc.err, got, tc.want)
		}
	}
}

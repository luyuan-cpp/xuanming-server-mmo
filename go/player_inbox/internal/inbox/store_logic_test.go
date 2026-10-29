package inbox

// store_* 与 reconcile.go 抽出来的纯逻辑单测(decide.go 下半部分)。读写与事务本身只有
// store_integration_test.go(-tags integration + 真库)覆盖;这里钉住「读出之后怎么判、写回什么」。

import (
	"bytes"
	"context"
	"net"
	"regexp"
	"strings"
	"testing"

	"player_inbox/internal/constants"
	"player_inbox/internal/metrics"

	inboxpb "proto/player_inbox"

	"github.com/zeromicro/go-zero/core/logx/logtest"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/proto"
)

const (
	fenceFirst   = inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_FIRST
	fenceMatch   = inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_MATCH
	fenceAdvance = inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_ADVANCE
)

// ── 入箱 ─────────────────────────────────────────────────────────────────

// TestDecideEnqueueLockedDedupesBeforeFull 钉住锁内「先判重、后判满箱」(设计 §6.2 第 7.2–7.3 步、§8 #2):
// 差一条满箱时原请求与重试赛跑,输的一方看见赢家已提交的行 —— 此时流也满了,结局必须是 DUPLICATE 而不是 FULL。
func TestDecideEnqueueLockedDedupesBeforeFull(t *testing.T) {
	digest := ContentSHA256(gmBlock, testNowMs, []byte{8, 1})
	other := ContentSHA256(gmBlock, testNowMs+1, []byte{8, 1})
	existing := &inboxpb.PlayerInboxItemRow{Seq: 77, State: uint32(statePending), ContentSha256: digest}

	cases := []struct {
		name     string
		existing *inboxpb.PlayerInboxItemRow
		digest   []byte
		open     uint32
		want     EnqueueResult
		proceed  bool
	}{
		{"已存在同摘要 + 满箱 → DUPLICATE", existing, digest, 64, EnqueueResult{Seq: 77, State: statePending}, false},
		{"已存在不同摘要 + 满箱 → CONFLICT", existing, other, 64, EnqueueResult{Code: constants.ErrIdempotencyConflict}, false},
		{"已存在同摘要 + 未满 → DUPLICATE", existing, digest, 1, EnqueueResult{Seq: 77, State: statePending}, false},
		{"不存在 + 满箱 → FULL", nil, digest, 64, EnqueueResult{Code: constants.ErrFull}, false},
		{"不存在 + 超过上限也算满", nil, digest, 65, EnqueueResult{Code: constants.ErrFull}, false},
		{"不存在 + 差一条满 → 放行", nil, digest, 63, EnqueueResult{}, true},
	}
	for _, tc := range cases {
		got, proceed := decideEnqueueLocked(tc.existing, tc.digest, tc.open, 64)
		if got != tc.want || proceed != tc.proceed {
			t.Errorf("%s: got (%+v, %v), want (%+v, %v)", tc.name, got, proceed, tc.want, tc.proceed)
		}
	}
}

// ── Fetch ────────────────────────────────────────────────────────────────

// TestFetchNeedsLock 钉住快路径条件(设计 §7.8 Fetch 第 0 步):没有未终结条目且没有 CursorAhead 才不加锁。
func TestFetchNeedsLock(t *testing.T) {
	cases := []struct {
		name           string
		open           uint32
		nextSeq        uint64
		decidedThrough uint64
		want           bool
	}{
		{"空流、水位在后 → 快路径", 0, 100, 99, false},
		{"空流、新玩家水位 0 → 快路径", 0, 1, 0, false},
		{"有未终结条目 → 加锁", 1, 100, 0, true},
		{"空流但水位 == next_seq(CursorAhead)→ 加锁抬号", 0, 100, 100, true},
		{"空流但水位领先 → 加锁抬号", 0, 100, 5000, true},
	}
	for _, tc := range cases {
		snap := &inboxpb.PlayerInboxStream{OpenCount: tc.open, NextSeq: tc.nextSeq}
		if got := fetchNeedsLock(snap, tc.decidedThrough); got != tc.want {
			t.Errorf("%s: fetchNeedsLock = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestFetchStreamUpdate 钉住 Fetch 对流行的写回(设计 §7.8 Fetch 第 2、5 步):FIRST / ADVANCE 写围栏,
// CursorAhead 写抬过的 next_seq;MATCH 且没抬号不写(锁路径不做多余的写)。
func TestFetchStreamUpdate(t *testing.T) {
	cases := []struct {
		name      string
		v         inboxpb.PlayerInboxFenceVerdict
		raised    bool
		write     bool
		wantFence uint64
	}{
		{"MATCH 不抬号 → 不写", fenceMatch, false, false, 5},
		{"MATCH + CursorAhead → 只写 next_seq,围栏原样", fenceMatch, true, true, 5},
		{"FIRST → 围栏写成请求 epoch", fenceFirst, false, true, 7},
		{"ADVANCE + CursorAhead → 两样都写", fenceAdvance, true, true, 7},
	}
	for _, tc := range cases {
		write, fence, next := fetchStreamUpdate(tc.v, 5, 7, 900, tc.raised)
		if write != tc.write || (write && (fence != tc.wantFence || next != 900)) {
			t.Errorf("%s: got (write=%v fence=%d next=%d), want (write=%v fence=%d next=900)",
				tc.name, write, fence, next, tc.write, tc.wantFence)
		}
	}
}

// TestSplitPage 钉住 LIMIT n+1 读法的切页:多读的那一行只用来判断 has_more。
func TestSplitPage(t *testing.T) {
	rows := []int{1, 2, 3}
	if page, more := splitPage(rows, 2); len(page) != 2 || !more || page[1] != 2 {
		t.Errorf("读到 limit+1 行: page=%v more=%v", page, more)
	}
	if page, more := splitPage(rows, 3); len(page) != 3 || more {
		t.Errorf("恰好 limit 行: page=%v more=%v", page, more)
	}
	if page, more := splitPage(rows, 5); len(page) != 3 || more {
		t.Errorf("不足 limit 行: page=%v more=%v", page, more)
	}
	if page, more := splitPage[int](nil, 16); len(page) != 0 || more {
		t.Errorf("空: page=%v more=%v", page, more)
	}
	if page, more := splitPage(rows, -1); len(page) != 0 || !more {
		t.Errorf("负 limit 按 0: page=%v more=%v", page, more)
	}
}

// TestFetchScanBound 钉住 fetch_scanned_rows 的口径(设计 §4.2):读满 LIMIT 时数到最后读出那一行;
// 没读满说明整条流都走过了。数的是走过的行,不是返回的条数。
func TestFetchScanBound(t *testing.T) {
	rows := func(seqs ...uint64) []*inboxpb.PlayerInboxItemRow {
		out := make([]*inboxpb.PlayerInboxItemRow, 0, len(seqs))
		for _, s := range seqs {
			out = append(out, &inboxpb.PlayerInboxItemRow{Seq: s})
		}
		return out
	}
	if bound, whole := fetchScanBound(rows(10, 20, 30), 3); bound != 30 || whole {
		t.Errorf("读满 LIMIT: bound=%d whole=%v, want 30/false", bound, whole)
	}
	if _, whole := fetchScanBound(rows(10, 20), 3); !whole {
		t.Error("没读满:整条流都走过了,应 whole=true")
	}
	if _, whole := fetchScanBound(nil, 17); !whole {
		t.Error("一行 PENDING 都没读到(open_count>0 却没有 PENDING):整条流都走过了,应 whole=true")
	}
}

// ── Ack / 作废共用的 open_count ────────────────────────────────────────────

func TestSubtractOpenCount(t *testing.T) {
	if left, under := subtractOpenCount(5, 2); left != 3 || under {
		t.Errorf("5-2 = (%d, %v)", left, under)
	}
	if left, under := subtractOpenCount(2, 2); left != 0 || under {
		t.Errorf("2-2 = (%d, %v)", left, under)
	}
	if left, under := subtractOpenCount(1, 3); left != 0 || !under {
		t.Errorf("1-3 应钳到 0 并报 underflow,got (%d, %v)", left, under)
	}
}

// TestAckStreamUpdate 钉住 Ack 末尾对流行的写回(设计 §7.8 Ack):没终结也没推进围栏就不写;
// 终结了就减 open_count;FIRST / ADVANCE 写围栏;open_count 不够减钳到 0 并报告。
func TestAckStreamUpdate(t *testing.T) {
	cases := []struct {
		name      string
		v         inboxpb.PlayerInboxFenceVerdict
		open      uint32
		finalized uint32
		write     bool
		newOpen   uint32
		newFence  uint64
		underflow bool
	}{
		{"MATCH 且全是 ALREADY_SAME / 异常 → 不写", fenceMatch, 3, 0, false, 3, 5, false},
		{"MATCH 终结 2 条", fenceMatch, 3, 2, true, 1, 5, false},
		{"ADVANCE 但没终结 → 只推进围栏", fenceAdvance, 3, 0, true, 3, 9, false},
		{"FIRST 终结 1 条", fenceFirst, 1, 1, true, 0, 9, false},
		{"open_count 不够减 → 钳 0 并报告", fenceMatch, 1, 2, true, 0, 5, true},
	}
	for _, tc := range cases {
		write, newOpen, newFence, under := ackStreamUpdate(tc.v, tc.open, tc.finalized, 5, 9)
		if write != tc.write || under != tc.underflow || (write && (newOpen != tc.newOpen || newFence != tc.newFence)) {
			t.Errorf("%s: got (write=%v open=%d fence=%d underflow=%v), want (%v %d %d %v)",
				tc.name, write, newOpen, newFence, under, tc.write, tc.newOpen, tc.newFence, tc.underflow)
		}
	}
}

// TestAckMetricLabelAndAnomaly 钉住 ack_total{result} 的标签与「协议异常」集合:告警规则按标签正则取
// (deploy/k8s/player-inbox-alerts.yaml),SETTLED_UNKNOWN 必须单列,协议异常恰好是设计 §7.7 列的五种。
func TestAckMetricLabelAndAnomaly(t *testing.T) {
	settled := &inboxpb.PlayerInboxAckItem{Result: inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_FINALIZED, State: stateUnknown}
	if got := ackMetricLabel(settled); got != "finalized_settled_unknown" {
		t.Errorf("SETTLED_UNKNOWN 的标签 = %q", got)
	}
	applied := &inboxpb.PlayerInboxAckItem{Result: inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_FINALIZED, State: stateApplied}
	if got := ackMetricLabel(applied); got != "finalized" {
		t.Errorf("普通 FINALIZED 的标签 = %q", got)
	}
	// 非 FINALIZED 的结果即使带着 SETTLED_UNKNOWN 状态(终态比对)也按结果名记。
	same := &inboxpb.PlayerInboxAckItem{Result: inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_ALREADY_SAME, State: stateUnknown}
	if got := ackMetricLabel(same); got != "already_same" {
		t.Errorf("ALREADY_SAME 的标签 = %q", got)
	}

	anomalies := map[inboxpb.PlayerInboxAckResult]bool{}
	for v := range inboxpb.PlayerInboxAckResult_name {
		r := inboxpb.PlayerInboxAckResult(v)
		if r != inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_UNSPECIFIED && isAckAnomaly(r) {
			anomalies[r] = true
		}
	}
	want := []inboxpb.PlayerInboxAckResult{
		inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_CONFLICT,
		inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_UNKNOWN_SEQ,
		inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_NOT_DELIVERED,
		inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_SKIP_NOT_REQUESTED,
		inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_MISMATCH,
	}
	if len(anomalies) != len(want) {
		t.Fatalf("协议异常集合 = %v, want %v", anomalies, want)
	}
	for _, r := range want {
		if !anomalies[r] {
			t.Errorf("%v 应是协议异常", r)
		}
	}
}

// ── 运维接口 ─────────────────────────────────────────────────────────────

// TestVoidOutcomeOf 钉住作废的回包与指标路径(设计 §7.9):拒绝不记 void_total;已投递(含重复作废)回 void_requested。
func TestVoidOutcomeOf(t *testing.T) {
	cases := []struct {
		name   string
		action voidAction
		state  inboxpb.PlayerInboxItemState
		want   voidOutcome
	}{
		{"不存在", voidNotFound, inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_UNSPECIFIED, voidOutcome{tip: constants.ErrItemNotFound}},
		{"已是终态", voidTerminal, stateApplied, voidOutcome{tip: constants.ErrItemStateConflict, state: stateApplied}},
		{"从未投递 → 直接 VOIDED", voidDirect, stateVoided, voidOutcome{state: stateVoided, path: "direct"}},
		{"已投递 → 挂指令", voidDirective, statePending, voidOutcome{state: statePending, voidRequested: true, path: "directive"}},
		{"重复作废 → 幂等", voidAlreadyRequested, statePending, voidOutcome{state: statePending, voidRequested: true, path: "already"}},
	}
	for _, tc := range cases {
		if got := voidOutcomeOf(tc.action, tc.state); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestAuditRowFor 钉住审计行的组装(评审 L9 回归):条目终态行 30 天后物理删除、审计行永不删除,
// 审计必须独自回答「做了什么」。msg_type / request_id / issued_at_ms / 内容摘要 / 载荷一律取自条目行
// (调用方传入的同名值被忽略,shard 按 player_id 重算);「谁、为什么」与 void_reason 取调用方;
// 与条目对不上的审计 fail-closed(不写出一行张冠李戴的记录)。
func TestAuditRowFor(t *testing.T) {
	const pid, seq, requestID = uint64(42), uint64(77), uint64(9001)
	payload := []byte{8, 3}
	issued := testNowMs - 5
	item := &inboxpb.PlayerInboxItemRow{
		Shard: RowShard(pid), PlayerId: pid, Domain: uint32(gmDomain), Seq: seq,
		MsgType: uint32(gmBlock), RequestId: requestID, IssuedAtMs: issued,
		ContentSha256: ContentSHA256(gmBlock, issued, payload), Payload: payload,
		OperatorName: "ops_producer", Note: "条目自己的 note 不进审计",
	}
	in := &inboxpb.PlayerInboxAudit{
		Shard: (RowShard(pid) + 1) % ShardCount, PlayerId: pid, Domain: uint32(gmDomain), Seq: seq,
		Action:   uint32(inboxpb.PlayerInboxAuditAction_PLAYER_INBOX_AUDIT_VOID),
		Operator: "ops_alice", PeerAddr: "10.0.0.5:51234", Reason: "工单 #7", CreatedMs: testNowMs,
		// 调用方给的内容列一律忽略:内容只认条目这一个来源。
		MsgType: uint32(gmDebt), RequestId: 1, IssuedAtMs: 1, ContentSha256: []byte("forged"), Payload: []byte("forged"),
		VoidReason: uint32(inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_CANCELLED),
	}
	got, err := auditRowFor(item, in)
	if err != nil {
		t.Fatal(err)
	}
	want := &inboxpb.PlayerInboxAudit{
		Shard: RowShard(pid), PlayerId: pid, Domain: uint32(gmDomain), Seq: seq,
		Action:   uint32(inboxpb.PlayerInboxAuditAction_PLAYER_INBOX_AUDIT_VOID),
		Operator: "ops_alice", PeerAddr: "10.0.0.5:51234", Reason: "工单 #7", CreatedMs: testNowMs,
		MsgType: uint32(gmBlock), RequestId: requestID, IssuedAtMs: issued,
		ContentSha256: item.GetContentSha256(), Payload: payload,
		VoidReason: uint32(inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_CANCELLED),
	}
	if !proto.Equal(got, want) {
		t.Fatalf("auditRowFor = %+v\nwant %+v", got, want)
	}
	if !bytes.Equal(ContentSHA256(inboxpb.PlayerInboxMessageType(got.GetMsgType()), got.GetIssuedAtMs(), got.GetPayload()),
		got.GetContentSha256()) {
		t.Fatal("审计行的摘要必须能由它自己的 msg_type / issued_at_ms / payload 复算")
	}

	// 合法的零字节载荷(currency_type=0 的封锁)写成空串而不是 NULL,与条目行同一口径。
	empty := proto.Clone(item).(*inboxpb.PlayerInboxItemRow)
	empty.Payload, empty.ContentSha256 = nil, nil
	if got, err := auditRowFor(empty, in); err != nil || got.Payload == nil || got.ContentSha256 == nil {
		t.Fatalf("零字节载荷: got %+v err=%v, want 非 nil 的空串", got, err)
	}

	// 与条目对不上 → error,不写审计。
	for name, mutate := range map[string]func(a *inboxpb.PlayerInboxAudit){
		"player_id 不同": func(a *inboxpb.PlayerInboxAudit) { a.PlayerId++ },
		"domain 不同":    func(a *inboxpb.PlayerInboxAudit) { a.Domain++ },
		"seq 不同":       func(a *inboxpb.PlayerInboxAudit) { a.Seq++ },
	} {
		bad := proto.Clone(in).(*inboxpb.PlayerInboxAudit)
		mutate(bad)
		if got, err := auditRowFor(item, bad); err == nil {
			t.Errorf("%s: 应 fail-closed,got %+v", name, got)
		}
	}
	if _, err := auditRowFor(nil, in); err == nil {
		t.Error("条目为 nil 应报错")
	}
	if _, err := auditRowFor(item, nil); err == nil {
		t.Error("审计为 nil 应报错")
	}
}

// TestRegisteredPayloadsFitAudit 保留 GM 永久审计的 1KiB 预算。Match 专用 Battle 载荷只进永久条目原件，
// EnqueueBattleSettlement 不传 GM 审计回调且通用 Void 拒绝该域；原接口回归另钉住这些前提。
// 因此仅此具名类型使用已批准的 16KiB 上限，不能据此放宽其它类型的永久审计预算。
func TestRegisteredPayloadsFitAudit(t *testing.T) {
	const auditPayloadBudget = 1024
	r := mustRegistry(t)
	if len(r.types) == 0 {
		t.Fatal("注册表没有任何类型")
	}
	for typ, e := range r.types {
		if typ == inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT {
			if e.Spec.GetDomain() != inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT ||
				e.Spec.GetPayloadMessage() != "BattleSettlementData" || e.Spec.GetMaxPayloadBytes() != 16*1024 {
				t.Errorf("Battle 专用原件规格不符: %+v", e.Spec)
			}
			continue
		}
		if e.Spec.GetMaxPayloadBytes() > auditPayloadBudget {
			t.Errorf("%v 的 max_payload_bytes = %d,超过审计表按原样存载荷的预算 %d", typ, e.Spec.GetMaxPayloadBytes(), auditPayloadBudget)
		}
	}
}

// ── 日志里的发送方身份(评审 L10)───────────────────────────────────────
// 告警(deploy/k8s/player-inbox-alerts.yaml PlayerInboxEpochAhead 等)让值班按 ERROR 日志里的 scene_node_id
// 定位发送方;只有 player_id 时找不到是哪台 scene 发的。对端地址是唯一不可伪造的部分,scene_node_id 是自报的线索。

const (
	logTestPeer      = "10.0.0.5:51234"
	logTestSceneNode = uint32(9)
)

func peerContext(t *testing.T) context.Context {
	t.Helper()
	addr, err := net.ResolveTCPAddr("tcp", logTestPeer)
	if err != nil {
		t.Fatal(err)
	}
	return peer.NewContext(context.Background(), &peer.Peer{Addr: addr})
}

// fixedEpochs 是只读的 R:STALE 分类时读一次。
type fixedEpochs uint64

func (e fixedEpochs) OwnerEpoch(context.Context, uint64) (uint64, error) { return uint64(e), nil }

func requireSender(t *testing.T, what, logs string) {
	t.Helper()
	for _, want := range []string{"peer=" + logTestPeer, "scene_node_id=9"} {
		if !strings.Contains(logs, want) {
			t.Errorf("%s: 日志缺 %q:\n%s", what, want, logs)
		}
	}
}

// TestFenceRejectLogsCarrySender:Fetch / Ack 的围栏拒绝日志带对端地址与 scene_node_id。
// EPOCH_REQUIRED 走公开入口(不碰库);EPOCH_AHEAD 与 STALE(fence_regressed)直接调 reportFenceReject。
func TestFenceRejectLogsCarrySender(t *testing.T) {
	s := &Store{registry: mustRegistry(t), epochs: fixedEpochs(3)}
	ctx := peerContext(t)

	logs := logtest.NewCollector(t)
	resp, err := s.Fetch(ctx, &inboxpb.FetchPlayerInboxRequest{
		PlayerId: 42, Domain: gmDomain, OwnerEpoch: 0, SceneNodeId: logTestSceneNode,
	})
	if err != nil || resp.GetFence() != inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_EPOCH_REQUIRED {
		t.Fatalf("Fetch epoch=0: %+v %v", resp, err)
	}
	requireSender(t, "Fetch EPOCH_REQUIRED", logs.String())

	logs.Reset()
	ack, err := s.Ack(ctx, &inboxpb.AckPlayerInboxRequest{
		PlayerId: 42, Domain: gmDomain, OwnerEpoch: 0, SceneNodeId: logTestSceneNode,
		Decisions: []*inboxpb.PlayerInboxDecisionReport{{Seq: 1, Outcome: outApplied, RequestId: 1}},
	})
	if err != nil || ack.GetFence() != inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_EPOCH_REQUIRED {
		t.Fatalf("Ack epoch=0: %+v %v", ack, err)
	}
	requireSender(t, "Ack EPOCH_REQUIRED", logs.String())

	logs.Reset()
	s.reportFenceReject(ctx, "Ack", inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_EPOCH_AHEAD, 42, gmDomain, 7, 5, logTestSceneNode)
	requireSender(t, "EPOCH_AHEAD", logs.String())

	logs.Reset()
	// R = 3 ≤ req = 4 < fence = 5:核心 Redis 的 owner_epoch 回退了(fence_regressed,critical)。
	s.reportFenceReject(ctx, "Fetch", inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_STALE, 42, gmDomain, 4, 5, logTestSceneNode)
	if out := logs.String(); !strings.Contains(out, "fence_regressed") {
		t.Fatalf("STALE 应分类为 fence_regressed:\n%s", out)
	}
	requireSender(t, "STALE fence_regressed", logs.String())
}

// TestAckResultLogsCarrySender:Ack 逐条的协议异常与 SETTLED_UNKNOWN 日志带对端地址与 scene_node_id。
func TestAckResultLogsCarrySender(t *testing.T) {
	s := &Store{}
	ctx := peerContext(t)
	ackOf := func(report *inboxpb.PlayerInboxDecisionReport) *inboxpb.AckPlayerInboxRequest {
		return &inboxpb.AckPlayerInboxRequest{
			PlayerId: 42, Domain: gmDomain, OwnerEpoch: 5, SceneNodeId: logTestSceneNode,
			Decisions: []*inboxpb.PlayerInboxDecisionReport{report},
		}
	}

	logs := logtest.NewCollector(t)
	s.reportAckResults(ctx, 42, gmDomain, ackOf(&inboxpb.PlayerInboxDecisionReport{Seq: 10, Outcome: outApplied, RequestId: 999}),
		[]*inboxpb.PlayerInboxAckItem{{Seq: 10, Result: inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_MISMATCH, State: statePending}})
	if out := logs.String(); !strings.Contains(out, "Ack protocol anomaly") {
		t.Fatalf("应记协议异常:\n%s", out)
	}
	requireSender(t, "Ack MISMATCH", logs.String())

	logs.Reset()
	s.reportAckResults(ctx, 42, gmDomain, ackOf(&inboxpb.PlayerInboxDecisionReport{Seq: 11, Outcome: outUnknown, RequestId: 11}),
		[]*inboxpb.PlayerInboxAckItem{{Seq: 11, Result: inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_FINALIZED, State: stateUnknown}})
	if out := logs.String(); !strings.Contains(out, "SETTLED_UNKNOWN") {
		t.Fatalf("应记 SETTLED_UNKNOWN:\n%s", out)
	}
	requireSender(t, "Ack SETTLED_UNKNOWN", logs.String())
}

// TestPeerAddr:有对端时回 ip:port,取不到时回 "unknown"(审计的 peer_addr 与日志共用)。
func TestPeerAddr(t *testing.T) {
	if got := PeerAddr(context.Background()); got != "unknown" {
		t.Fatalf("PeerAddr(无对端) = %q, want unknown", got)
	}
	if got := PeerAddr(peerContext(t)); got != logTestPeer {
		t.Fatalf("PeerAddr = %q, want %q", got, logTestPeer)
	}
}

// ── 集成测试护栏(评审 L14)───────────────────────────────────────────────

// inboxTestDBNamePattern:store_integration_test.go 每个用例都 DROP 收件箱三张表与 schema_migrations,
// 只许碰这两类一次性库。appuser 对 mmorpg_player_inbox / testdb / zone_N_db / mmorpg_trade 都有 ALL 权限,
// 黑名单挡不住 DSN 指错,所以用白名单(照 go/guild 的 guildTestDBNamePattern)。
// 放在不带 integration 标签的文件里:集成用例用它,下面的单测不带标签也能钉住它。
var inboxTestDBNamePattern = regexp.MustCompile(`^(player_inbox_it|player_inbox_it_\d+_\d+)$`)

func TestInboxTestDBNamePattern(t *testing.T) {
	for _, ok := range []string{"player_inbox_it", "player_inbox_it_123_4"} {
		if !inboxTestDBNamePattern.MatchString(ok) {
			t.Errorf("%q 应被接受", ok)
		}
	}
	for _, bad := range []string{
		"mmorpg_player_inbox", "testdb", "zone_1_db", "mmorpg_trade", "guild_test", "trade_it", "player_inbox_it2", "",
	} {
		if inboxTestDBNamePattern.MatchString(bad) {
			t.Errorf("%q 必须被拒绝", bad)
		}
	}
}

// TestSummarizeStreams 钉住 GetSummary 的组装:PENDING 聚合按域对上,max_fence_epoch 取各流最大值(epoch-raise 的来源,C4)。
func TestSummarizeStreams(t *testing.T) {
	streams := []*inboxpb.PlayerInboxStream{
		{Domain: 1, OpenCount: 2, NextSeq: 100, FenceEpoch: 9, NotifyDueMs: 5},
		{Domain: 2, OpenCount: 0, NextSeq: 7, FenceEpoch: 12},
	}
	aggs := map[uint32]pendingAggregate{1: {deliveredUnacked: 1, oldestCreatedMs: 1234}}
	resp := summarizeStreams(42, streams, aggs)
	if resp.GetPlayerId() != 42 || resp.GetMaxFenceEpoch() != 12 || len(resp.GetStreams()) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	s := resp.GetStreams()[0]
	if s.GetDomain() != 1 || s.GetOpenCount() != 2 || s.GetNextSeq() != 100 || s.GetFenceEpoch() != 9 ||
		s.GetDeliveredUnackedCount() != 1 || s.GetOldestPendingCreatedMs() != 1234 || s.GetNotifyDueMs() != 5 {
		t.Errorf("域 1 概况: %+v", s)
	}
	if s := resp.GetStreams()[1]; s.GetDeliveredUnackedCount() != 0 || s.GetOldestPendingCreatedMs() != 0 {
		t.Errorf("没有 PENDING 行的域聚合应为 0: %+v", s)
	}
	if empty := summarizeStreams(7, nil, nil); empty.GetPlayerId() != 7 || len(empty.GetStreams()) != 0 || empty.GetMaxFenceEpoch() != 0 {
		t.Errorf("没有流: %+v", empty)
	}
}

// ── 后台循环 ─────────────────────────────────────────────────────────────

// TestFoldScanGauges 钉住扫描型 gauge 的跨分片折叠(设计 §9.4):计数相加、最大投递次数取最大、
// 「最老」取各分片非 0 的最小时刻 —— 0 表示该分片没有这类行,绝不能被当成最老的时刻。
// 陈旧的已投递未确认行(评审 M4)只在 delivered_unacked_stale 里相加:分片读数里它们已被 SQL 排除在
// deliveredUnacked / maxDeliveryCount / minDeliveredMs 之外,折叠不得把它们加回告警口径。
func TestFoldScanGauges(t *testing.T) {
	const now = testNowMs
	shards := []shardGaugeScan{
		{}, // 空分片:全 0
		{openItems: 3, deliveredUnacked: 1, maxDeliveryCount: 2, minCreatedMs: now - 10_000, minDeliveredMs: now - 4_000, notifyBacklog: 1},
		{openItems: 2, deliveredUnacked: 2, maxDeliveryCount: 6, minCreatedMs: now - 60_000, minDeliveredMs: 0, notifyBacklog: 0},
		{openItems: 1, deliveredUnacked: 1, maxDeliveryCount: 1, minCreatedMs: now - 5_000, minDeliveredMs: now - 7_000, notifyBacklog: 2},
		// 只有陈旧行的分片:不影响告警口径的三项。
		{openItems: 4, deliveredUnackedStale: 4, minCreatedMs: now - 30_000},
		{openItems: 2, deliveredUnacked: 1, deliveredUnackedStale: 1, maxDeliveryCount: 3, minCreatedMs: now - 20_000, minDeliveredMs: now - 1_000},
	}
	got := foldScanGauges(now, shards)
	want := metrics.ScanGauges{
		OpenItems: 12, DeliveredUnacked: 5, DeliveredUnackedStale: 5, MaxDeliveryCount: 6, NotifyBacklog: 3,
		OldestOpenAgeSeconds: 60, DeliveredUnackedOldestSeconds: 7,
	}
	if got != want {
		t.Fatalf("fold = %+v, want %+v", got, want)
	}
	if empty := foldScanGauges(now, make([]shardGaugeScan, ShardCount)); empty != (metrics.ScanGauges{}) {
		t.Errorf("全空时所有 gauge 为 0,got %+v", empty)
	}
	// 时钟回拨(最老时刻晚于 now)不报负数年龄。
	if skew := foldScanGauges(now, []shardGaugeScan{{openItems: 1, minCreatedMs: now + 1000}}); skew.OldestOpenAgeSeconds != 0 {
		t.Errorf("时钟回拨时年龄应为 0,got %v", skew.OldestOpenAgeSeconds)
	}
}

func TestMinNonZero(t *testing.T) {
	cases := [][3]uint64{{0, 0, 0}, {0, 5, 5}, {5, 0, 5}, {3, 5, 3}, {5, 3, 3}}
	for _, c := range cases {
		if got := minNonZero(c[0], c[1]); got != c[2] {
			t.Errorf("minNonZero(%d, %d) = %d, want %d", c[0], c[1], got, c[2])
		}
	}
}

// TestNotifyRetryDueMs 钉住补发没落定时的退避(评审 L6):下一次 = now + 「已悬时长」,钳到 [30s, 30min]。
// 每次没落定都把已悬时长再等一遍,重试时刻按 2 倍递增;起算点为 0 或晚于 now(时钟回拨)时取下限。
func TestNotifyRetryDueMs(t *testing.T) {
	const now = testNowMs
	sec := func(n uint64) uint64 { return n * 1000 }
	cases := []struct {
		name  string
		since uint64
		want  uint64
	}{
		{"刚悬 5s:取下限 30s", now - sec(5), now + sec(30)},
		{"恰好 30s", now - sec(30), now + sec(30)},
		{"悬了 100s:再等 100s", now - sec(100), now + sec(100)},
		{"恰好 30min", now - sec(1800), now + sec(1800)},
		{"悬了 3 小时:封顶 30min", now - sec(3*3600), now + sec(1800)},
		{"起算点为 0:取下限", 0, now + sec(30)},
		{"起算点等于 now:取下限", now, now + sec(30)},
		{"时钟回拨(起算点晚于 now):取下限", now + sec(60), now + sec(30)},
	}
	for _, tc := range cases {
		if got := notifyRetryDueMs(now, tc.since); got != tc.want {
			t.Errorf("%s: notifyRetryDueMs = now%+d ms, want now%+d ms", tc.name, int64(got-now), int64(tc.want-now))
		}
	}
	// 连续失败的节拍:入箱 T0 起悬,补发循环第一次在 T0+5s 看到它,之后每次都在上次定下的时刻重试。
	since, at := now, now+sec(5)
	var gaps []uint64
	for i := 0; i < 8; i++ {
		next := notifyRetryDueMs(at, since)
		gaps = append(gaps, (next-at)/1000)
		at = next
	}
	want := []uint64{30, 35, 70, 140, 280, 560, 1120, 1800}
	for i := range want {
		if gaps[i] != want[i] {
			t.Fatalf("连续失败的退避(秒) = %v, want %v(按 2 倍递增、封顶 30 分钟)", gaps, want)
		}
	}
}

// ── 对账 ─────────────────────────────────────────────────────────────────
// 账本一律是真实形状(水位 = 环尾,见 reconcile_test.go 的 rcCursor);判定谓词的全表见 TestFinalizedLedgerGap。

// TestReconcileReport 钉住 ReconcilePlayer 的纯逻辑(设计 §9.3):逐行按该域游标(含最近结局环)判定、
// 管理员终结的行不比对、两边都没有的域不列游标。
// 账本的来历:消费方裁决过 10、12,随后 Redis 丢尾回到 10;12 在收件箱已是终态、不会再投,之后又裁决了 13、15。
// 所以环是 [10, 13, 15]、水位 15 —— 只比水位看不出 12 丢了(评审 M6)。13 已裁决、确认丢了,收件箱仍 PENDING。
func TestReconcileReport(t *testing.T) {
	const otherDomain = inboxpb.PlayerInboxDomain(2)
	rows := []*inboxpb.PlayerInboxItemRow{
		rcRow(10, 1, stateApplied, byConsumer), // 在环里:一致
		rcRow(12, 6, stateApplied, byConsumer), // 水位以内、环里查无此 seq:被之后的条目掩盖的丢失
		rcRow(13, 4, statePending, 0),          // PENDING 但水位已过:确认丢了
		rcRow(15, 7, stateApplied, byConsumer), // 在环里:一致
		rcRow(20, 2, stateApplied, byConsumer), // > 水位:账本没到
		rcRow(25, 3, stateVoided, byAdmin),     // 管理员作废:不比对
		rcRow(30, 5, statePending, 0),          // PENDING 且未处理:一致
	}
	ledger := rcLedger(rcCursor(gmDomain,
		rcDecision(10, 1, outApplied, 0), rcDecision(13, 4, outApplied, 0), rcDecision(15, 7, outApplied, 0)))
	streams := map[inboxpb.PlayerInboxDomain]*inboxpb.PlayerInboxStream{gmDomain: {Domain: uint32(gmDomain), NextSeq: 31}}
	cursors, diffs := reconcileReport([]inboxpb.PlayerInboxDomain{gmDomain, otherDomain}, streams, ledger, rows)

	if len(cursors) != 1 || cursors[0].GetDomain() != gmDomain || cursors[0].GetLedgerDecidedThrough() != 15 || cursors[0].GetInboxNextSeq() != 31 {
		t.Fatalf("cursors = %+v(没有流也没有水位的域 2 不列)", cursors)
	}
	final := inboxpb.PlayerInboxReconcileDiffKind_PLAYER_INBOX_RECONCILE_FINAL_NOT_IN_LEDGER
	decided := inboxpb.PlayerInboxReconcileDiffKind_PLAYER_INBOX_RECONCILE_PENDING_BUT_DECIDED
	want := []struct {
		seq, requestID uint64
		kind           inboxpb.PlayerInboxReconcileDiffKind
		state          inboxpb.PlayerInboxItemState
	}{
		{12, 6, final, stateApplied},
		{13, 4, decided, statePending},
		{20, 2, final, stateApplied},
	}
	if len(diffs) != len(want) {
		t.Fatalf("diffs = %+v, want %d", diffs, len(want))
	}
	for i, w := range want {
		if d := diffs[i]; d.GetDomain() != gmDomain || d.GetSeq() != w.seq || d.GetRequestId() != w.requestID ||
			d.GetKind() != w.kind || d.GetState() != w.state {
			t.Errorf("diffs[%d] = %+v, want seq=%d request_id=%d kind=%v state=%v", i, d, w.seq, w.requestID, w.kind, w.state)
		}
	}

	// 收件箱没有流、账本却有水位(例如流行所在库被恢复过):照样列出游标,next_seq 为 0。
	cursors, _ = reconcileReport([]inboxpb.PlayerInboxDomain{gmDomain}, nil, rcLedger(rcCursor(gmDomain, rcDecision(9, 1, outApplied, 0))), nil)
	if len(cursors) != 1 || cursors[0].GetInboxNextSeq() != 0 || cursors[0].GetLedgerDecidedThrough() != 9 {
		t.Errorf("只有账本水位的域: %+v", cursors)
	}
}

// TestLedgerMismatches 钉住定时对账的判定:与 ReconcilePlayer 同一个谓词(finalizedLedgerGap),逐行带出缺口种类,
// 顺序同输入;账本里没有该域按水位 0。
func TestLedgerMismatches(t *testing.T) {
	ledger := rcLedger(rcCursor(gmDomain, rcDecision(10, 10, outApplied, 0), rcDecision(15, 15, outApplied, 0)))
	otherDomain := rcRef(3, 3, stateApplied, byConsumer)
	otherDomain.Domain = inboxpb.PlayerInboxDomain(2)
	refs := []FinalizedRef{
		rcRef(10, 10, stateApplied, byConsumer), // 在环里:一致
		rcRef(12, 12, stateApplied, byConsumer), // 水位以内、环里查无此 seq
		rcRef(15, 15, stateApplied, byConsumer), // 在环里:一致
		rcRef(16, 16, stateApplied, byConsumer), // 水位没到
		otherDomain,                             // 账本无该域
	}
	got := ledgerMismatches(refs, ledger)
	want := []ledgerMismatch{
		{ref: refs[1], gap: gapMissingInRing},
		{ref: refs[3], gap: gapBeyondWatermark},
		{ref: refs[4], gap: gapBeyondWatermark},
	}
	if len(got) != len(want) {
		t.Fatalf("mismatches = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mismatches[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got := ledgerMismatches([]FinalizedRef{refs[0], refs[2]}, ledger); len(got) != 0 {
		t.Errorf("都在环里时不应有不一致: %+v", got)
	}
	if got := ledgerMismatches(nil, ledger); len(got) != 0 {
		t.Errorf("没有终结行: %+v", got)
	}
}

// TestRegistryDomainsSorted:运维对账逐域输出,顺序必须稳定(不随 map 遍历变)。
func TestRegistryDomainsSorted(t *testing.T) {
	r := mustRegistry(t)
	for i := 0; i < 20; i++ {
		ds := r.Domains()
		for j := 1; j < len(ds); j++ {
			if ds[j-1] >= ds[j] {
				t.Fatalf("Domains() 未按域号升序: %v", ds)
			}
		}
	}
	if got := r.Domains(); len(got) != 2 || got[0] != gmDomain || got[1] != inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT {
		t.Errorf("登记域必须精确为升序 GM 与 Battle，不能漏域或夹带其它域: %v", got)
	}
}

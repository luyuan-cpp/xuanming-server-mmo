package loginqueue

// 排队取走放行时复核放行时选定的 gate(admit_recheck.go,2026-11-05):
//   - 纯逻辑 chooseAdmitGate:仍在分档选出的那一档里就签它(地址取此刻的候选),停写 / 排空 / 满载 / 不在候选里就重挑,
//     没有更好的 gate 时留着它;
//   - NewAdmitSigner:签出的票绑复核之后的 gate;读不到候选时照签放行槽里的 gate,复核本身不回错误;
//   - 经 Queue.Lookup 的端到端:放行之后选定的 gate 卡住,取走时拿到的票绑另一台。

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	commonpb "proto/common/base"

	"github.com/prometheus/client_golang/prometheus"
	gzprom "github.com/zeromicro/go-zero/core/prometheus"
	"google.golang.org/protobuf/proto"
)

// recheckSource 是复核用的候选源替身:固定回一份候选或一个错误,记下被问的 zone。
type recheckSource struct {
	cands []GateCandidate
	err   error
	zones []uint32
}

func (s *recheckSource) CandidatesForZone(_ context.Context, zoneID uint32) ([]GateCandidate, error) {
	s.zones = append(s.zones, zoneID)
	return s.cands, s.err
}

var recheckSecret = []byte("gate-secret")

// admittedToGate1 是放行时 dispatcher 写进放行槽的 gate。每个用例自己造一份,免得被改到。
func admittedToGate1() *AdmitSlot {
	return &AdmitSlot{NodeID: 1, IP: "10.0.0.1", Port: 9000, ZoneID: 3}
}

func decodeGatePayload(t *testing.T, admit *AdmitToken) *commonpb.GateTokenPayload {
	t.Helper()
	p := &commonpb.GateTokenPayload{}
	if err := proto.Unmarshal(admit.TokenPayload, p); err != nil {
		t.Fatalf("unmarshal gate token payload: %v", err)
	}
	return p
}

// admitRecheckValue 读 login_queue_admit_gate_recheck_total{outcome} 的当前值,序列还没出现时回 0。
// 计数器是进程级的,用例读前后差值;要先 gzprom.Enable() 计数器才写。
func admitRecheckValue(t *testing.T, outcome string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "login_queue_admit_gate_recheck_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "outcome" && l.GetValue() == outcome {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// 选定的 gate 仍合格:签它,不抽随机数;地址取此刻候选里的那份(同一个 node_id 以最新的注册记录为准)。
func TestChooseAdmitGate_KeepsEligibleOriginal(t *testing.T) {
	cands := []GateCandidate{
		{NodeID: 2, IP: "10.0.0.2", Port: 9000, PlayerCount: 0, ZoneID: 3},
		{NodeID: 1, IP: "gate-1.z3.example.com", Port: 30001, PlayerCount: 50, MaxConnections: 100, ZoneID: 3},
	}
	gate, outcome := chooseAdmitGate(admittedToGate1(), cands, seqIntn(t))
	if outcome != admitRecheckKept || gate.NodeID != 1 {
		t.Fatalf("got gate %d (%s), want gate 1 kept: an eligible gate is not traded for a lighter one", gate.NodeID, outcome)
	}
	if gate.IP != "gate-1.z3.example.com" || gate.Port != 30001 {
		t.Fatalf("address = %s:%d, want the current candidate's gate-1.z3.example.com:30001", gate.IP, gate.Port)
	}
}

// 选定的 gate 停写、排空、满载或不在候选里:在合格的候选里按快速通道规则重挑,并说明原因。
func TestChooseAdmitGate_RepicksIneligibleOriginal(t *testing.T) {
	others := []GateCandidate{
		{NodeID: 2, IP: "10.0.0.2", Port: 9000, PlayerCount: 40, MaxConnections: 100, ZoneID: 3},
		{NodeID: 3, IP: "10.0.0.3", Port: 9000, PlayerCount: 10, MaxConnections: 100, ZoneID: 3},
	}
	cases := []struct {
		name     string
		original *GateCandidate // nil = 不在此刻的候选里
		want     string
	}{
		{"stale", &GateCandidate{NodeID: 1, IP: "10.0.0.1", Port: 9000, ZoneID: 3, Stale: true}, admitRecheckStale},
		{"draining", &GateCandidate{NodeID: 1, IP: "10.0.0.1", Port: 9000, ZoneID: 3, Draining: true}, admitRecheckDraining},
		{"full", &GateCandidate{NodeID: 1, IP: "10.0.0.1", Port: 9000, PlayerCount: 95, MaxConnections: 100, ZoneID: 3}, admitRecheckFull},
		{"gone", nil, admitRecheckGone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cands := append([]GateCandidate(nil), others...)
			if tc.original != nil {
				cands = append(cands, *tc.original)
			}
			// 合格的只有 2、3 号:i=0 抽中 2 号,j=0 顺延到 3 号,取人少的 3 号。
			gate, outcome := chooseAdmitGate(admittedToGate1(), cands, seqIntn(t, 0, 0))
			if outcome != tc.want || gate.NodeID != 3 {
				t.Fatalf("got gate %d (%s), want gate 3 (%s)", gate.NodeID, outcome, tc.want)
			}
		})
	}
}

// 滚动升级:同一地址上的 gate 换了 node_id。放行槽里的旧 node_id 已不在候选里,照签会被新 gate 以
// token_gate_node_mismatch 拒绝,必须改签此刻在服务的那台。
func TestChooseAdmitGate_ReplacedNodeIDIsRepicked(t *testing.T) {
	cands := []GateCandidate{{NodeID: 9, IP: "10.0.0.1", Port: 9000, ZoneID: 3}}
	gate, outcome := chooseAdmitGate(admittedToGate1(), cands, seqIntn(t))
	if outcome != admitRecheckGone || gate.NodeID != 9 {
		t.Fatalf("got gate %d (%s), want gate 9 (gone)", gate.NodeID, outcome)
	}
}

// 整区都在排空:分档退到连得上的,选定的 gate 就在这一档里,留着它,不为了换而换。
func TestChooseAdmitGate_AllDrainingKeepsOriginal(t *testing.T) {
	cands := []GateCandidate{
		{NodeID: 2, IP: "10.0.0.2", Port: 9000, ZoneID: 3, Draining: true},
		{NodeID: 1, IP: "10.0.0.1", Port: 9000, ZoneID: 3, Draining: true},
	}
	gate, outcome := chooseAdmitGate(admittedToGate1(), cands, seqIntn(t))
	if outcome != admitRecheckKept || gate.NodeID != 1 {
		t.Fatalf("got gate %d (%s), want gate 1 kept under the reachable fallback", gate.NodeID, outcome)
	}
}

// 选定的 gate 在排空、别的都停写:排空中的连得上,留着它;不会被换到卡住的 gate 上(2026-11-06 分档)。
func TestChooseAdmitGate_DrainingOriginalBeatsStaleOthers(t *testing.T) {
	cands := []GateCandidate{
		{NodeID: 2, IP: "10.0.0.2", Port: 9000, ZoneID: 3, Stale: true},
		{NodeID: 1, IP: "10.0.0.1", Port: 9000, ZoneID: 3, Draining: true},
	}
	gate, outcome := chooseAdmitGate(admittedToGate1(), cands, seqIntn(t))
	if outcome != admitRecheckKept || gate.NodeID != 1 {
		t.Fatalf("got gate %d (%s), want draining gate 1 kept instead of the stale one", gate.NodeID, outcome)
	}
}

// 全部停写时退到全部:选定的 gate 只要还在候选里就留着它,与放行时一样,不为了换而换。
func TestChooseAdmitGate_AllIneligibleKeepsOriginal(t *testing.T) {
	cands := []GateCandidate{
		{NodeID: 2, IP: "10.0.0.2", Port: 9000, ZoneID: 3, Stale: true},
		{NodeID: 1, IP: "10.0.0.1", Port: 9000, ZoneID: 3, Stale: true},
	}
	gate, outcome := chooseAdmitGate(admittedToGate1(), cands, seqIntn(t))
	if outcome != admitRecheckKept || gate.NodeID != 1 {
		t.Fatalf("got gate %d (%s), want gate 1 kept under fallback", gate.NodeID, outcome)
	}
}

// 签出的票绑复核之后的 gate:payload 的 gate_node_id、zone 与下发地址都是新那台的,签名有效;问的是放行槽里的 zone。
func TestNewAdmitSigner_SignsForRepickedGate(t *testing.T) {
	src := &recheckSource{cands: []GateCandidate{
		{NodeID: 1, IP: "10.0.0.1", Port: 9000, ZoneID: 3, Stale: true},
		{NodeID: 2, IP: "10.0.0.2", Port: 9100, PlayerCount: 5, ZoneID: 3},
	}}
	admit, err := NewAdmitSigner(context.Background(), src, recheckSecret, time.Minute)(admittedToGate1())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	payload := decodeGatePayload(t, admit)
	if payload.GetGateNodeId() != 2 || payload.GetZoneId() != 3 || admit.IP != "10.0.0.2" || admit.Port != 9100 {
		t.Fatalf("token for gate %d zone %d at %s:%d, want gate 2 zone 3 at 10.0.0.2:9100",
			payload.GetGateNodeId(), payload.GetZoneId(), admit.IP, admit.Port)
	}
	if string(admit.TokenSignature) != string(hmacHex(recheckSecret, admit.TokenPayload)) {
		t.Fatal("token signature does not match the payload")
	}
	if got := fmt.Sprint(src.zones); got != "[3]" {
		t.Fatalf("candidates asked for zones %s, want exactly [3] (the admit slot's zone)", got)
	}
}

// 读不到此刻的候选(出错、为空、没有候选源)时照签放行槽里的 gate,与没有复核时相同;复核本身不回错误。
func TestNewAdmitSigner_UncheckedSignsOriginal(t *testing.T) {
	gzprom.Enable() // 计数器要指标开关打开才写,见 TestLookup_AdmitSlotExpiredUnclaimed_ReturnsExpired
	cases := map[string]GateCandidateSource{
		"read error": &recheckSource{err: errors.New("etcd unavailable")},
		"no gates":   &recheckSource{},
		"no recheck": nil,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			before := admitRecheckValue(t, admitRecheckUnchecked)
			admit, err := NewAdmitSigner(context.Background(), src, recheckSecret, time.Minute)(admittedToGate1())
			if err != nil {
				t.Fatalf("sign: %v (the recheck must never turn into an error)", err)
			}
			if got := decodeGatePayload(t, admit).GetGateNodeId(); got != 1 || admit.IP != "10.0.0.1" || admit.Port != 9000 {
				t.Fatalf("token for gate %d at %s:%d, want the admitted gate 1 at 10.0.0.1:9000", got, admit.IP, admit.Port)
			}
			if got := admitRecheckValue(t, admitRecheckUnchecked) - before; got != 1 {
				t.Fatalf(`login_queue_admit_gate_recheck_total{outcome="unchecked"} grew by %v, want 1`, got)
			}
		})
	}
}

// 没有放行槽可签:回错误,不签票。
func TestNewAdmitSigner_NilSlotIsAnError(t *testing.T) {
	if _, err := NewAdmitSigner(context.Background(), nil, recheckSecret, time.Minute)(nil); err == nil {
		t.Fatal("signing a nil admit slot must fail")
	}
}

// 端到端:放行时选的是 1 号,放行之后 1 号停写;玩家取走时拿到的票绑 2 号,并计一次 stale。
// 变异取红:NewAdmitSigner 里的 recheckAdmitGate(ctx, source, slot) 换成 slotGate(slot),这条与上面两条签票用例红。
func TestLookup_ConsumeRepicksGateThatWentStaleAfterAdmit(t *testing.T) {
	gzprom.Enable()
	q, _, _, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err != nil || n != 1 {
		t.Fatalf("popadmit = (%d, %v), want (1, nil)", n, err)
	}
	src := &recheckSource{cands: []GateCandidate{
		{NodeID: 1, IP: "10.0.0.1", Port: 9000, ZoneID: 1, Stale: true},
		{NodeID: 2, IP: "10.0.0.2", Port: 9000, ZoneID: 1},
	}}

	before := admitRecheckValue(t, admitRecheckStale)
	st, err := q.Lookup(ctx, res.QueueToken, 1, NewAdmitSigner(ctx, src, recheckSecret, time.Minute))
	if err != nil || st.Status != StatusAdmitted {
		t.Fatalf("lookup = (%+v, %v), want ADMITTED", st, err)
	}
	if got := decodeGatePayload(t, st.Admit).GetGateNodeId(); got != 2 || st.Admit.IP != "10.0.0.2" {
		t.Fatalf("token for gate %d at %s, want gate 2: the gate picked at admit time stopped writing its record", got, st.Admit.IP)
	}
	if got := admitRecheckValue(t, admitRecheckStale) - before; got != 1 {
		t.Fatalf(`login_queue_admit_gate_recheck_total{outcome="stale"} grew by %v, want 1`, got)
	}
}

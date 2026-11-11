package loginqueue

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// inflightClock 是手动时钟:只在测试里前进,不碰真实墙钟。
type inflightClock struct {
	mu sync.Mutex
	t  time.Time
}

func newInflightClock() *inflightClock { return &inflightClock{t: time.Unix(1_800_000_000, 0)} }

func (c *inflightClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *inflightClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func inflightOf(cands []GateCandidate) string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, fmt.Sprintf("%d:%d", c.NodeID, c.Inflight))
	}
	return fmt.Sprint(out)
}

// 记几张就是几张;窗口(交票宽限)之外的不再算,恰好到窗口那一刻就过期。
func TestGateInflight_CountsWithinTheHandoffGrace(t *testing.T) {
	clk := newInflightClock()
	g := NewGateInflight(clk.now)
	cands := []GateCandidate{{NodeID: 1}, {NodeID: 2}}

	g.Record(1)
	g.Record(1)
	clk.advance(defaultHandoffGrace / 2)
	g.Record(2)
	if got := inflightOf(g.Annotate(cands)); got != "[1:2 2:1]" {
		t.Fatalf("inflight = %s, want [1:2 2:1]", got)
	}

	clk.advance(defaultHandoffGrace / 2) // 1 号的两张恰好到窗口
	if got := inflightOf(g.Annotate(cands)); got != "[1:0 2:1]" {
		t.Fatalf("inflight at the grace boundary = %s, want [1:0 2:1]", got)
	}
	clk.advance(defaultHandoffGrace)
	if got := inflightOf(g.Annotate(cands)); got != "[1:0 2:0]" {
		t.Fatalf("inflight after the grace = %s, want [1:0 2:0]", got)
	}
}

// Annotate 不改调用方的切片;下线 gate 的记录在窗口过后清掉,表不随下线的 gate 一直变大。
func TestGateInflight_AnnotateCopiesAndPrunesGoneGates(t *testing.T) {
	clk := newInflightClock()
	g := NewGateInflight(clk.now)
	g.Record(9) // 9 号之后下线,不再出现在候选里

	in := []GateCandidate{{NodeID: 1, PlayerCount: 5}}
	g.Record(1)
	out := g.Annotate(in)
	if in[0].Inflight != 0 || out[0].Inflight != 1 || out[0].PlayerCount != 5 {
		t.Fatalf("Annotate = %+v (input %+v), want a copy with Inflight 1 and the input untouched", out, in)
	}

	clk.advance(defaultHandoffGrace)
	g.Annotate(in)
	g.mu.Lock()
	left := len(g.handedOut)
	g.mu.Unlock()
	if left != 0 {
		t.Fatalf("handedOut keeps %d gates after the grace, want 0 (gone gates must not accumulate)", left)
	}
}

// nil 记账:Record 什么都不做,Annotate 原样返回;没接线的调用方与单测可以直接传 nil。
func TestGateInflight_NilIsANoOp(t *testing.T) {
	var g *GateInflight
	g.Record(1)
	cands := []GateCandidate{{NodeID: 1, PlayerCount: 3}}
	if got := g.Annotate(cands); &got[0] != &cands[0] {
		t.Fatalf("nil Annotate returned a copy %+v, want the input itself", got)
	}
	pick := newBatchPickFn(cands)
	if wrapped := recordPicks(pick, nil); fmt.Sprintf("%p", wrapped) != fmt.Sprintf("%p", pick) {
		t.Fatal("recordPicks with a nil ledger must return the pick function unchanged")
	}
}

// 快速通道:在途票算进负载。两台上报人数相同,在途多的那台在随机二选一里输掉。
func TestPickTwoChoices_CountsInflight(t *testing.T) {
	cands := []GateCandidate{
		{NodeID: 1, PlayerCount: 10, Inflight: 5},
		{NodeID: 2, PlayerCount: 12},
	}
	// 先抽 1 号(负载 15)再抽 2 号(负载 12):取 2 号。只看上报人数会取 1 号。
	got, err := pickTwoChoices(cands, seqIntn(t, 0, 0))
	if err != nil || got.NodeID != 2 {
		t.Fatalf("pick = (%+v, %v), want gate 2 (10+5 in flight > 12)", got, err)
	}
}

// dispatcher:每挑定一台记一张在途票;下一拍取候选时带上,不再从同一份滞后人数起算。
func TestRecordPicks_CarriesPicksIntoTheNextTick(t *testing.T) {
	clk := newInflightClock()
	g := NewGateInflight(clk.now)
	snapshot := []GateCandidate{{NodeID: 1, PlayerCount: 50}, {NodeID: 2, PlayerCount: 52}}

	// 第一拍放 3 人:1 号 50→52 先填两人,再按 NodeID 给 1 号第三人。
	tick := recordPicks(newBatchPickFn(g.Annotate(snapshot)), g)
	for range 3 {
		if _, err := tick(1, "q"); err != nil {
			t.Fatal(err)
		}
	}
	if got := inflightOf(g.Annotate(snapshot)); got != "[1:3 2:0]" {
		t.Fatalf("inflight after tick 1 = %s, want [1:3 2:0]", got)
	}

	// 第二拍:上报人数没变(还没到下一次上报),但 1 号已带着 3 张在途票,先填的是 2 号。
	next := recordPicks(newBatchPickFn(g.Annotate(snapshot)), g)
	slot, err := next(1, "q")
	if err != nil || slot.NodeID != 2 {
		t.Fatalf("first pick of tick 2 = (%+v, %v), want gate 2 (gate 1 carries 3 in flight)", slot, err)
	}
}

// 快速通道签票成功才记;挑不出 gate 时什么都不记。
func TestPickAndSignGateToken_RecordsOnlySignedTokens(t *testing.T) {
	clk := newInflightClock()
	g := NewGateInflight(clk.now)
	secret := []byte("0123456789abcdef0123456789abcdef")

	if _, err := PickAndSignGateToken(nil, g, secret, time.Minute); err == nil {
		t.Fatal("no candidates must fail")
	}
	cands := []GateCandidate{{NodeID: 7, IP: "127.0.0.1", Port: 30000, ZoneID: 1}}
	if _, err := PickAndSignGateToken(cands, g, secret, time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := inflightOf(g.Annotate(cands)); got != "[7:1]" {
		t.Fatalf("inflight = %s, want [7:1] (one signed token)", got)
	}
}

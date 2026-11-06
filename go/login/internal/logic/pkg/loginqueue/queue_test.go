package loginqueue

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	gzprom "github.com/zeromicro/go-zero/core/prometheus"
)

// newTestQueue spins up an in-process miniredis + go-redis client and a
// Queue with short TTLs. The returned cleanup function stops miniredis;
// callers should defer it to avoid cross-test goroutine leaks.
func newTestQueue(t *testing.T) (*Queue, *redis.Client, *miniredis.Miniredis, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	q := New(rdb, time.Hour, 60*time.Second, []byte("test-secret"))
	return q, rdb, mr, func() { mr.Close(); _ = rdb.Close() }
}

// TestEnqueueAssignsAscendingRanks 验证已有更早条目时，新入队者排在队尾。
// 既有条目的分数显式固定到更早的正数，不依赖墙钟刚好跨毫秒；同毫秒的字典序另有独立用例。
func TestEnqueueAssignsAscendingRanks(t *testing.T) {
	q, rdb, _, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	res1, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	if res1.Rank != 0 {
		t.Errorf("first enqueue rank = %d, want 0", res1.Rank)
	}

	// Enqueue 用 login 墙钟打分，miniredis 的时钟不控制该分数。
	// 把已经在队列里的 Alice 固定到更早的正分数；仍由下一次真实 Enqueue 写 Bob 并返回名次。
	if err := rdb.ZAdd(ctx, zoneZSetKey(1), redis.Z{Score: 1, Member: res1.QueueID}).Err(); err != nil {
		t.Fatalf("固定第一个条目的早期分数失败: %v", err)
	}
	res2, err := q.Enqueue(ctx, 1, "bob", "dev2")
	if err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	if res2.Rank != 1 {
		t.Errorf("second enqueue rank = %d, want 1", res2.Rank)
	}

	// 保留 Alice 在前，显式让已经入队的 Bob 也早于下一次真实入队。
	if err := rdb.ZAdd(ctx, zoneZSetKey(1), redis.Z{Score: 2, Member: res2.QueueID}).Err(); err != nil {
		t.Fatalf("固定第二个条目的早期分数失败: %v", err)
	}

	res3, err := q.Enqueue(ctx, 1, "carol", "dev3")
	if err != nil {
		t.Fatalf("enqueue 3: %v", err)
	}
	if res3.Rank != 2 {
		t.Errorf("third enqueue rank = %d, want 2", res3.Rank)
	}
	if res3.Total != 3 {
		t.Errorf("total = %d, want 3", res3.Total)
	}
	if res1.QueueToken == res2.QueueToken || res2.QueueToken == res3.QueueToken {
		t.Error("queue tokens must be distinct across enqueues")
	}
}

// 同分条目按 queueId 字典序排位，不能把 UUID 的随机字典序误当入队先后。
// 先经真实 Enqueue 建立令牌和 meta，再确定地制造同分，通过客户端查询面逐条核对名次。
func TestLookup_EqualScoresUseQueueIDLexicographicOrder(t *testing.T) {
	q, rdb, _, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	entries := make([]*EnqueueResult, 0, 3)
	for _, account := range []string{"alice", "bob", "carol"} {
		entry, err := q.Enqueue(ctx, 1, account, "dev")
		if err != nil {
			t.Fatalf("入队 %s 失败: %v", account, err)
		}
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b *EnqueueResult) int { return strings.Compare(a.QueueID, b.QueueID) })
	// 按字典逆序更新到同一个分数，期望顺序仍只由 queueId 决定。
	for i := len(entries) - 1; i >= 0; i-- {
		if err := rdb.ZAdd(ctx, zoneZSetKey(1), redis.Z{Score: 1, Member: entries[i].QueueID}).Err(); err != nil {
			t.Fatalf("固定同分队列失败: %v", err)
		}
	}
	for rank, entry := range entries {
		state, err := q.Lookup(ctx, entry.QueueToken, 1, signMustNotRun(t))
		if err != nil {
			t.Fatalf("查询同分条目失败: %v", err)
		}
		if state == nil || state.Status != StatusQueueing || state.Rank != uint32(rank) || state.Total != 3 {
			t.Fatalf("同分条目状态 = %+v, 期望排队中 rank=%d total=3", state, rank)
		}
	}
}

// TestZonesAreIndependent guards against accidentally mixing zones into the
// same ZSET — a tempting refactor that would break per-zone capacity logic.
func TestZonesAreIndependent(t *testing.T) {
	q, _, _, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := q.Enqueue(ctx, 1, "alice", "dev1"); err != nil {
		t.Fatalf("enqueue zone 1: %v", err)
	}
	res, err := q.Enqueue(ctx, 2, "bob", "dev2")
	if err != nil {
		t.Fatalf("enqueue zone 2: %v", err)
	}
	if res.Rank != 0 {
		t.Errorf("zone 2 first enqueue rank = %d, want 0 (zones must be independent)", res.Rank)
	}

	zone1Len, _ := q.QueueLen(ctx, 1)
	zone2Len, _ := q.QueueLen(ctx, 2)
	if zone1Len != 1 || zone2Len != 1 {
		t.Errorf("zone lengths = (%d, %d), want (1, 1)", zone1Len, zone2Len)
	}
}

// TestPopAdmitWritesAdmitToken exercises the dispatcher path. We pop one
// entry, supply a stub pickFn, and verify Lookup transitions through
// QUEUEING → ADMITTED → EXPIRED on subsequent polls. This pins the
// "exactly-once admit" contract that prevents double-issued gate tokens.
//
// Post-R17-R1 (2026-06-04): the dispatcher only PICKS a gate at admit
// time (writing an AdmitSlot to admit:{queueId}); the gate token's HMAC
// + expire are computed inside Lookup via signFn. The test asserts both
// halves work — pickFn writes the slot, signFn turns it into a token.
//
// (2026-10-18 补注)「We pop one entry」改读:PopAdmit 先 ZRANGE 读候选,再由放行脚本在写放行的同一段里把成员移出
// 队列;放行之后队列为空(补了 ZCARD 断言)。
func TestPopAdmitWritesAdmitToken(t *testing.T) {
	q, rdb, _, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	stubSign := func(slot *AdmitSlot) (*AdmitToken, error) {
		return &AdmitToken{
			IP:            slot.IP,
			Port:          slot.Port,
			TokenDeadline: time.Now().Unix() + 300,
		}, nil
	}

	// Before PopAdmit: status must be QUEUEING.
	state, err := q.Lookup(ctx, res.QueueToken, 1, stubSign)
	if err != nil {
		t.Fatalf("lookup before admit: %v", err)
	}
	if state.Status != StatusQueueing {
		t.Errorf("status before admit = %d, want %d", state.Status, StatusQueueing)
	}

	pickFn := func(zoneID uint32, queueID string) (*AdmitSlot, error) {
		return &AdmitSlot{NodeID: 1, IP: "10.0.0.5", Port: 9000, ZoneID: zoneID}, nil
	}
	n, err := q.PopAdmit(ctx, 1, 5, pickFn)
	if err != nil {
		t.Fatalf("popadmit: %v", err)
	}
	if n != 1 {
		t.Errorf("popadmit count = %d, want 1", n)
	}
	if n, err := rdb.ZCard(ctx, zoneZSetKey(1)).Result(); err != nil || n != 0 {
		t.Errorf("queue length after admit = %d err=%v, want 0 (the admit script removes the member)", n, err)
	}

	// First Lookup after admit: ADMITTED with admit token signed by stubSign
	// at consume time (the slot we wrote at admit time has IP=10.0.0.5).
	state, err = q.Lookup(ctx, res.QueueToken, 1, stubSign)
	if err != nil {
		t.Fatalf("lookup after admit: %v", err)
	}
	if state.Status != StatusAdmitted {
		t.Fatalf("status after admit = %d, want %d", state.Status, StatusAdmitted)
	}
	if state.Admit == nil || state.Admit.IP != "10.0.0.5" || state.Admit.Port != 9000 {
		t.Errorf("admit token mismatch: got %+v", state.Admit)
	}

	// Second Lookup: admit was consumed → EXPIRED, NOT a second ADMITTED.
	// (2026-10-15 更正)此前这里回的是 QUEUEING、名次 0(meta 还在、ZSET 成员已不在),本用例只断言
	// 「不是第二次 ADMITTED」;现在 meta 里的 admitted_at 让 Lookup 回终态 EXPIRED,断言随之收紧。
	state, err = q.Lookup(ctx, res.QueueToken, 1, stubSign)
	if err != nil {
		t.Fatalf("lookup after consume: %v", err)
	}
	if state.Status == StatusAdmitted {
		t.Errorf("admit was consumed twice — leaks gate tokens")
	} else if state.Status != StatusExpired {
		t.Errorf("status after consume = %d, want %d (EXPIRED)", state.Status, StatusExpired)
	}
}

// TestQueueTokenRoundTrip ensures HMAC sign+verify is symmetric and
// rejects tampering. Without this, a forged queue_token would let a
// client jump the queue.
func TestQueueTokenRoundTrip(t *testing.T) {
	secret := []byte("test-secret")
	token, err := signQueueToken(secret, "qid-123", 7, time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	qid, zone, err := ParseAndVerifyQueueToken(secret, token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if qid != "qid-123" || zone != 7 {
		t.Errorf("parse mismatch: qid=%s zone=%d", qid, zone)
	}

	// Tampered token: flip a byte.
	bad := []byte(token)
	bad[0] ^= 0x01
	if _, _, err := ParseAndVerifyQueueToken(secret, string(bad)); err == nil {
		t.Error("verify accepted tampered token")
	}

	// Wrong secret.
	if _, _, err := ParseAndVerifyQueueToken([]byte("other-secret"), token); err == nil {
		t.Error("verify accepted token signed with different secret")
	}

	// Expired.
	old, _ := signQueueToken(secret, "qid-x", 1, time.Now().Add(-time.Second).Unix())
	if _, _, err := ParseAndVerifyQueueToken(secret, old); err == nil {
		t.Error("verify accepted expired token")
	}
}

// fakeProvider is a CapacityProvider stub for FreeSlots tests. It returns a
// fixed list of GateCandidates and an optional config-pinned capacity.
type fakeProvider struct {
	cands map[uint32][]GateCandidate
	caps  map[uint32]uint32
}

func (f *fakeProvider) CandidatesForZone(_ context.Context, zoneID uint32) ([]GateCandidate, error) {
	return f.cands[zoneID], nil
}
func (f *fakeProvider) ZoneCapacity(zoneID uint32) uint32 { return f.caps[zoneID] }

// TestFreeSlotsAccountsForAdmittedInFlight is the core safety property:
// admitted-but-not-yet-connected entries must subtract from freeSlots, or
// the dispatcher over-issues gate tokens during the round-trip window.
func TestFreeSlotsAccountsForAdmittedInFlight(t *testing.T) {
	q, _, _, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	prov := &fakeProvider{
		cands: map[uint32][]GateCandidate{
			1: {{NodeID: 1, IP: "10.0.0.1", Port: 9000, PlayerCount: 50, ZoneID: 1}},
		},
		caps: map[uint32]uint32{1: 100},
	}

	// Baseline: capacity 100, online 50, admitted 0 → free=50.
	free, capacity, online, admitted, err := FreeSlots(ctx, prov, q, 1, 1.5)
	if err != nil {
		t.Fatalf("freeslots: %v", err)
	}
	if free != 50 || capacity != 100 || online != 50 || admitted != 0 {
		t.Errorf("baseline mismatch: free=%d cap=%d online=%d admitted=%d", free, capacity, online, admitted)
	}

	// Enqueue + admit one entry. admitted should now be 1, free should
	// drop by exactly 1.
	res, _ := q.Enqueue(ctx, 1, "alice", "dev1")
	if _, err := q.PopAdmit(ctx, 1, 1, func(z uint32, qid string) (*AdmitSlot, error) {
		return &AdmitSlot{NodeID: 1, IP: "10.0.0.1", Port: 9000, ZoneID: z}, nil
	}); err != nil {
		t.Fatalf("popadmit: %v", err)
	}
	_ = res

	free, _, _, admitted, err = FreeSlots(ctx, prov, q, 1, 1.5)
	if err != nil {
		t.Fatalf("freeslots after admit: %v", err)
	}
	if admitted != 1 {
		t.Errorf("admitted = %d, want 1", admitted)
	}
	if free != 49 {
		t.Errorf("free after admit = %d, want 49", free)
	}
}

// TestFreeSlotsSoftCapFallback verifies the multiplier formula kicks in
// when no ZoneCapacity is configured. Without this, freshly-deployed
// zones (online=0, capacity=0) would lock everyone out.
func TestFreeSlotsSoftCapFallback(t *testing.T) {
	q, _, _, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	prov := &fakeProvider{
		cands: map[uint32][]GateCandidate{
			1: {{NodeID: 1, IP: "x", Port: 1, PlayerCount: 100, ZoneID: 1}},
		},
		caps: map[uint32]uint32{}, // unconfigured
	}

	free, capacity, _, _, err := FreeSlots(ctx, prov, q, 1, 1.5)
	if err != nil {
		t.Fatalf("freeslots: %v", err)
	}
	// online=100 * 1.5 = 150 cap; 150 - 100 = 50 free.
	if capacity != 150 {
		t.Errorf("cap = %d, want 150 (online 100 * 1.5)", capacity)
	}
	if free != 50 {
		t.Errorf("free = %d, want 50", free)
	}
}

// TestFreeSlotsErrorsWhenNoGates ensures we DON'T silently enqueue into a
// zone with zero gates — the entry would never drain.
func TestFreeSlotsErrorsWhenNoGates(t *testing.T) {
	q, _, _, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	prov := &fakeProvider{
		cands: map[uint32][]GateCandidate{}, // zone 1 has nothing
	}
	_, _, _, _, err := FreeSlots(ctx, prov, q, 1, 1.5)
	if err == nil {
		t.Error("freeslots returned nil err for zone with zero gates — would create a black hole")
	}
}

// pickFn 出错时条目不能丢,也不能被挪位置(2026-10-18 改写,原 TestPopAdmitReQueuesOnSignFailure:当时 ZPOPMIN 已弹出,
// 以分数 0 回填队首)。现在条目从未离开队列:分数不变、不写放行槽和占位,错误随返回值交给 dispatcher,下一拍再试。
// 修复前红:err == nil(错误被吞),分数被回填成 0。
func TestPopAdmit_PickFailure_EntryNeverLeavesQueue(t *testing.T) {
	q, rdb, _, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	score, err := rdb.ZScore(ctx, zoneZSetKey(1), res.QueueID).Result()
	if err != nil {
		t.Fatalf("zscore after enqueue: %v", err)
	}

	pickFnFail := func(z uint32, qid string) (*AdmitSlot, error) {
		return nil, context.DeadlineExceeded
	}
	n, err := q.PopAdmit(ctx, 1, 1, pickFnFail)
	if n != 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("popadmit with a failing pickFn = (%d, %v), want (0, error wrapping context.DeadlineExceeded)", n, err)
	}
	if got, err := rdb.ZScore(ctx, zoneZSetKey(1), res.QueueID).Result(); err != nil || got != score {
		t.Fatalf("queue score after a pick failure = %.0f err=%v, want %.0f (unchanged, never re-added at score 0)", got, err, score)
	}
	if n, err := rdb.Exists(ctx, admitKey(res.QueueID)).Result(); err != nil || n != 0 {
		t.Fatalf("admit slot exists=%d err=%v, want none", n, err)
	}
	mustAdmittedCount(t, q, 0, "pick failed")

	// signFn 不该被调用:还没有放行槽。
	st, err := q.Lookup(ctx, res.QueueToken, 1, signMustNotRun(t))
	if err != nil || st.Status != StatusQueueing || st.Rank != 0 || st.Total != 1 {
		t.Fatalf("lookup after a failed pick = (%+v, %v), want QUEUEING rank 0 total 1", st, err)
	}
}

// ── G4b-G:在途占位逐个到期 ─────────────────────────────────────────────

// legacyAdmittedSetKeyZone1 是 G4b-G 之前的在途占位键(SET)。新实现不再读写它,
// 用例拿它断言「旧键没人写」。新旧版本混跑时两种键并存、互不可见。
const legacyAdmittedSetKeyZone1 = "admitted:zone:1"

// fakeClock 是在途占位记账用的可拨时钟。占位的到期分数与清理都读 Queue.now,
// 用例直接拨它:不 sleep,也不依赖 miniredis 的时间(miniredis 里键的 TTL 不随它走)。
type fakeClock struct{ ms atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.ms.Store(1_800_000_000_000)
	return c
}

func (c *fakeClock) Now() time.Time          { return time.UnixMilli(c.ms.Load()) }
func (c *fakeClock) Advance(d time.Duration) { c.ms.Add(d.Milliseconds()) }

// newClockedQueue 在 newTestQueue 的基础上把占位时钟换成假时钟(起点固定,结果可复现)。
func newClockedQueue(t *testing.T) (*Queue, *redis.Client, *fakeClock) {
	t.Helper()
	q, rdb, _, cleanup := newTestQueue(t)
	t.Cleanup(cleanup)
	clk := newFakeClock()
	q.now = clk.Now
	return q, rdb, clk
}

// oneGateProvider:zone 1 一台 gate、固定容量。在线数由用例直接改 cands[1][0].PlayerCount
// (fakeProvider 返回的是同一个切片,不是副本)。
func oneGateProvider(capacity, online uint32) *fakeProvider {
	return &fakeProvider{
		cands: map[uint32][]GateCandidate{
			1: {{NodeID: 1, IP: "10.0.0.1", Port: 9000, PlayerCount: online, ZoneID: 1}},
		},
		caps: map[uint32]uint32{1: capacity},
	}
}

func pickZone1Gate(zoneID uint32, _ string) (*AdmitSlot, error) {
	return &AdmitSlot{NodeID: 1, IP: "10.0.0.1", Port: 9000, ZoneID: zoneID}, nil
}

func mustAdmittedCount(t *testing.T, q *Queue, want uint32, when string) {
	t.Helper()
	got, err := q.AdmittedCount(context.Background(), 1)
	if err != nil {
		t.Fatalf("admitted count (%s): %v", when, err)
	}
	if got != want {
		t.Fatalf("admitted count (%s) = %d, want %d", when, got, want)
	}
}

func mustFree(t *testing.T, prov CapacityProvider, q *Queue, want uint32, when string) {
	t.Helper()
	free, capacity, online, admitted, err := FreeSlots(context.Background(), prov, q, 1, 1.5)
	if err != nil {
		t.Fatalf("freeslots (%s): %v", when, err)
	}
	if free != want {
		t.Fatalf("free (%s) = %d, want %d (cap=%d online=%d admitted=%d)",
			when, free, want, capacity, online, admitted)
	}
}

func mustLegacyKeyUntouched(t *testing.T, rdb *redis.Client) {
	t.Helper()
	n, err := rdb.Exists(context.Background(), legacyAdmittedSetKeyZone1).Result()
	if err != nil || n != 0 {
		t.Fatalf("legacy key %q must not be written any more: exists=%d err=%v", legacyAdmittedSetKeyZone1, n, err)
	}
}

// 20s 是 gate 上报周期(10s)的两倍,见 defaultHandoffGrace 的注释;改它要同步改设计稿与 runbook。
func TestNew_DefaultsHandoffGraceAndClock(t *testing.T) {
	q := New(nil, time.Hour, 60*time.Second, nil)
	if q.handoffGrace != 20*time.Second {
		t.Fatalf("handoffGrace = %v, want 20s", q.handoffGrace)
	}
	if q.now == nil {
		t.Fatal("Queue.now must default to time.Now")
	}
	if got, want := q.admittedKeyTTL(), 2*time.Minute; got != want {
		t.Fatalf("admittedKeyTTL = %v, want %v (max(AdmitTTL, handoffGrace) + 1min)", got, want)
	}
}

// 放行写的是新键 ZSET,分数 = 到期毫秒;没人来取时按自己的分数到期,不靠整键 TTL。
func TestPopAdmit_HoldsPlaceholderInZSetUntilAdmitTTL(t *testing.T) {
	q, rdb, clk := newClockedQueue(t)
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	admittedAt := clk.Now()
	if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err != nil || n != 1 {
		t.Fatalf("popadmit = (%d, %v), want (1, nil)", n, err)
	}

	key := admittedZSetKey(1)
	if key != "admitted:zset:zone:1" {
		t.Fatalf("admitted key = %q, want admitted:zset:zone:1", key)
	}
	if typ, err := rdb.Type(ctx, key).Result(); err != nil || typ != "zset" {
		t.Fatalf("type(%s) = %q err=%v, want zset", key, typ, err)
	}
	score, err := rdb.ZScore(ctx, key, res.QueueID).Result()
	if err != nil {
		t.Fatalf("zscore: %v", err)
	}
	if want := float64(admittedAt.Add(q.admitTTL).UnixMilli()); score != want {
		t.Fatalf("placeholder score = %.0f, want %.0f (admit time + AdmitTTL, unix ms)", score, want)
	}
	if ttl, err := rdb.PTTL(ctx, key).Result(); err != nil || ttl <= q.admitTTL || ttl > q.admittedKeyTTL() {
		t.Fatalf("whole-key PTTL = %v err=%v, want (%v, %v]", ttl, err, q.admitTTL, q.admittedKeyTTL())
	}

	clk.Advance(q.admitTTL - time.Millisecond)
	mustAdmittedCount(t, q, 1, "1ms before AdmitTTL")
	clk.Advance(time.Millisecond)
	mustAdmittedCount(t, q, 0, "at AdmitTTL, never picked up")
	mustLegacyKeyUntouched(t, rdb)
}

func TestTryReserveFastPathSlot_BudgetAndPerMemberExpiry(t *testing.T) {
	q, rdb, clk := newClockedQueue(t)
	ctx := context.Background()
	reserve := func(budget uint32) bool {
		t.Helper()
		ok, err := q.TryReserveFastPathSlot(ctx, 1, budget)
		if err != nil {
			t.Fatalf("reserve(budget=%d): %v", budget, err)
		}
		return ok
	}

	if !reserve(2) || !reserve(2) {
		t.Fatal("first two reservations must fit budget 2")
	}
	if reserve(2) {
		t.Fatal("third reservation must be refused: budget 2 is full")
	}
	if reserve(0) {
		t.Fatal("budget 0 must never reserve")
	}

	wantScore := float64(clk.Now().Add(q.handoffGrace).UnixMilli())
	members, err := rdb.ZRangeWithScores(ctx, admittedZSetKey(1), 0, -1).Result()
	if err != nil || len(members) != 2 {
		t.Fatalf("placeholders = %v err=%v, want 2 members", members, err)
	}
	for _, m := range members {
		name, _ := m.Member.(string)
		if !strings.HasPrefix(name, "fast:") || m.Score != wantScore {
			t.Fatalf("placeholder %q score %.0f, want fast:<uuid> with score %.0f (now + handoff grace)",
				name, m.Score, wantScore)
		}
	}
	// 快速通道脚本给整键续的垃圾回收 TTL(ARGV[5])。miniredis 的 TTL 不随墙钟走、只在 FastForward 里递减,
	// 单位传错(比如 .Seconds() 变成 PEXPIRE 120ms)或者漏掉这条 PEXPIRE,要靠这里(以及
	// TestAdmittedKey_EveryWritePathSetsTTLWhenMissing)发现;
	// 到了真实 Redis 上,整键会在 120ms 后连同未到期的占位一起消失(或永不过期)。
	if ttl, err := rdb.PTTL(ctx, admittedZSetKey(1)).Result(); err != nil || ttl <= q.admitTTL || ttl > q.admittedKeyTTL() {
		t.Fatalf("fast-path whole-key PTTL = %v err=%v, want (%v, %v]", ttl, err, q.admitTTL, q.admittedKeyTTL())
	}

	clk.Advance(q.handoffGrace - time.Millisecond)
	if reserve(2) {
		t.Fatal("placeholders are still live 1ms before they expire")
	}
	clk.Advance(time.Millisecond)
	mustAdmittedCount(t, q, 0, "handoff grace elapsed")
	if !reserve(2) {
		t.Fatal("expired fast-path placeholders must free their slots")
	}
	mustLegacyKeyUntouched(t, rdb)
}

// Q1 回归:每秒 1 人走快速通道,10 秒后(gate 的上报周期)出现在 PlayerCount 里,连续 60 秒。
// 旧实现的 fast:<uuid> 占位永远不单独回收,整集合 TTL 又被每次占位续上:
// 连上 gate 的人同时算进 online 和 admitted,第 55 秒余量归零。
func TestFastPath_SustainedLoginsDoNotStarveBudget(t *testing.T) {
	q, rdb, clk := newClockedQueue(t)
	ctx := context.Background()
	const capacity = 100
	prov := oneGateProvider(capacity, 0)

	for sec := 0; sec < 60; sec++ {
		online := uint32(0)
		if sec > 10 {
			online = uint32(sec - 10)
		}
		prov.cands[1][0].PlayerCount = online

		free, _, _, admitted, err := FreeSlots(ctx, prov, q, 1, 1.5)
		if err != nil {
			t.Fatalf("sec=%d freeslots: %v", sec, err)
		}
		if free == 0 {
			t.Fatalf("sec=%d: free hit 0 with %d online of %d (admitted=%d): connected players are double-counted",
				sec, online, capacity, admitted)
		}
		ok, err := q.TryReserveFastPathSlot(ctx, 1, capacity-online)
		if err != nil || !ok {
			t.Fatalf("sec=%d: fast-path reservation refused (ok=%v err=%v) with %d online of %d",
				sec, ok, err, online, capacity)
		}
		clk.Advance(time.Second)
	}

	// 稳态下占位数 = 最近 handoffGrace 内交出去的票数,不随登录总数增长。
	n, err := q.AdmittedCount(ctx, 1)
	if err != nil {
		t.Fatalf("admitted count: %v", err)
	}
	if limit := uint32(q.handoffGrace / time.Second); n > limit {
		t.Fatalf("placeholders = %d after 60 logins, want <= %d (one per second of handoff grace)", n, limit)
	}
	mustLegacyKeyUntouched(t, rdb)
}

// Q2 回归:取走放行槽之后,gate 要到下一次上报才把人数算进 PlayerCount。
// 这段时间占位必须留着;旧实现取走即 SREM,下一拍按「在线 0、在途 0」再放一满批。
func TestConsume_KeepsPlaceholderForHandoffGraceThenReleases(t *testing.T) {
	q, rdb, clk := newClockedQueue(t)
	ctx := context.Background()
	prov := oneGateProvider(2, 0)

	queueIDs := make(map[string]string) // token → queueId
	for range 6 {
		res, err := q.Enqueue(ctx, 1, "u", "d")
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		queueIDs[res.QueueToken] = res.QueueID
	}
	mustFree(t, prov, q, 2, "before any admit")
	if n, err := q.PopAdmit(ctx, 1, 2, pickZone1Gate); err != nil || n != 2 {
		t.Fatalf("popadmit = (%d, %v), want (2, nil)", n, err)
	}

	clk.Advance(2 * time.Second) // 客户端的下一次轮询
	consumedAt := clk.Now()
	var taken []string
	for token, queueID := range queueIDs {
		st, err := q.Lookup(ctx, token, 1, stubSignFn)
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		if st.Status == StatusAdmitted {
			taken = append(taken, queueID)
		}
	}
	if len(taken) != 2 {
		t.Fatalf("consumed %d admits, want 2", len(taken))
	}
	wantScore := float64(consumedAt.Add(q.handoffGrace).UnixMilli())
	for _, queueID := range taken {
		score, err := rdb.ZScore(ctx, admittedZSetKey(1), queueID).Result()
		if err != nil || score != wantScore {
			t.Fatalf("placeholder %s score = %.0f err=%v, want %.0f (consume time + handoff grace)",
				queueID, score, err, wantScore)
		}
	}

	mustFree(t, prov, q, 0, "right after consume, gate still reports 0 online")
	clk.Advance(q.handoffGrace - time.Millisecond)
	mustFree(t, prov, q, 0, "1ms before the handoff grace ends")
	clk.Advance(time.Millisecond)
	mustAdmittedCount(t, q, 0, "handoff grace elapsed")
	mustFree(t, prov, q, 2, "handoff grace elapsed, players never showed up")
}

// login 的钟比 Redis 快时,占位(login 钟)可能先于放行槽(Redis TTL)到期被清掉。
// 这时取走仍要给这个人占一个交票后宽限:他确实马上要连 gate。所以是普通 ZADD,不是 XX。
func TestConsume_HoldsPlaceholderEvenIfAlreadyExpired(t *testing.T) {
	q, rdb, clk := newClockedQueue(t)
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err != nil || n != 1 {
		t.Fatalf("popadmit = (%d, %v), want (1, nil)", n, err)
	}
	clk.Advance(q.admitTTL) // miniredis 里放行槽的 TTL 不随假时钟走,槽还在
	mustAdmittedCount(t, q, 0, "placeholder expired by score")

	st, err := q.Lookup(ctx, res.QueueToken, 1, stubSignFn)
	if err != nil || st.Status != StatusAdmitted {
		t.Fatalf("lookup = (%+v, %v), want ADMITTED", st, err)
	}
	mustAdmittedCount(t, q, 1, "consume re-holds the slot")
	score, err := rdb.ZScore(ctx, admittedZSetKey(1), res.QueueID).Result()
	if want := float64(clk.Now().Add(q.handoffGrace).UnixMilli()); err != nil || score != want {
		t.Fatalf("placeholder score = %.0f err=%v, want %.0f", score, err, want)
	}
}

// 清理是闭区间:分数 <= now 的成员算已到期。
func TestAdmittedCount_DropsOnlyExpiredMembers(t *testing.T) {
	q, rdb, clk := newClockedQueue(t)
	ctx := context.Background()
	mustAdmittedCount(t, q, 0, "key does not exist")

	now := clk.Now().UnixMilli()
	key := admittedZSetKey(1)
	if err := rdb.ZAdd(ctx, key,
		redis.Z{Score: float64(now - 1), Member: "expired"},
		redis.Z{Score: float64(now), Member: "expires-now"},
		redis.Z{Score: float64(now + 1), Member: "live"},
	).Err(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mustAdmittedCount(t, q, 1, "only the member with score > now is live")
	left, err := rdb.ZRange(ctx, key, 0, -1).Result()
	if err != nil || len(left) != 1 || left[0] != "live" {
		t.Fatalf("members after count = %v err=%v, want [live]", left, err)
	}
}

// 清理、判定、占位在一段 Lua 里:并发调用成功的个数恰好等于 budget。
func TestTryReserveFastPathSlot_ConcurrentNeverExceedsBudget(t *testing.T) {
	q, rdb, _ := newClockedQueue(t)
	ctx := context.Background()
	const budget, callers = 8, 64

	var granted atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := q.TryReserveFastPathSlot(ctx, 1, budget)
			if err != nil {
				errs <- err
				return
			}
			if ok {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("reserve: %v", err)
	}
	if got := granted.Load(); got != budget {
		t.Fatalf("granted %d reservations, want exactly %d", got, budget)
	}
	if n, err := rdb.ZCard(ctx, admittedZSetKey(1)).Result(); err != nil || n != budget {
		t.Fatalf("placeholders = %d err=%v, want %d", n, err, budget)
	}
}

// 新版本只读写新键。旧键 admitted:zone:{z} 靠旧版本最后一次写入后的 TTL 自己消失。
func TestLegacyAdmittedSetKey_NeverWritten(t *testing.T) {
	q, rdb, _ := newClockedQueue(t)
	ctx := context.Background()
	prov := oneGateProvider(10, 0)

	if ok, err := q.TryReserveFastPathSlot(ctx, 1, 10); err != nil || !ok {
		t.Fatalf("reserve = (%v, %v), want (true, nil)", ok, err)
	}
	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	mustFree(t, prov, q, 9, "one fast-path placeholder")
	if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err != nil || n != 1 {
		t.Fatalf("popadmit = (%d, %v), want (1, nil)", n, err)
	}
	if st, err := q.Lookup(ctx, res.QueueToken, 1, stubSignFn); err != nil || st.Status != StatusAdmitted {
		t.Fatalf("lookup = (%+v, %v), want ADMITTED", st, err)
	}
	mustAdmittedCount(t, q, 2, "fast-path placeholder + consumed admit")

	keys, err := rdb.Keys(ctx, "admitted:*").Result()
	if err != nil || len(keys) != 1 || keys[0] != "admitted:zset:zone:1" {
		t.Fatalf("keys admitted:* = %v err=%v, want only admitted:zset:zone:1", keys, err)
	}
}

// Redis 出错时的方向不变:读不出在途占位就不放行。
func TestAdmittedAccounting_RedisDown_FailsClosed(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	// MaxRetries -1 关命令重试;DialerRetries 1 关拨号重试(缺省拨 5 次、每次失败后固定等 100ms)。
	// 下面三条会走到 Redis 的调用各拨号一次,每次最多等一个 DialTimeout。
	rdb := redis.NewClient(&redis.Options{
		Addr:               mr.Addr(),
		MaxRetries:         -1,
		DialTimeout:        200 * time.Millisecond,
		DialerRetries:      1,
		DialerRetryTimeout: time.Millisecond,
	})
	defer rdb.Close()
	q := New(rdb, time.Hour, 60*time.Second, []byte("test-secret"))
	ctx := context.Background()
	prov := oneGateProvider(10, 0)
	mr.Close()

	if _, err := q.AdmittedCount(ctx, 1); err == nil {
		t.Fatal("AdmittedCount must surface the Redis error")
	}
	if free, _, _, _, err := FreeSlots(ctx, prov, q, 1, 1.5); err == nil || free != 0 {
		t.Fatalf("FreeSlots = (free=%d, err=%v), want (0, error): no accounting, no admission", free, err)
	}
	if ok, err := q.TryReserveFastPathSlot(ctx, 1, 5); err == nil || ok {
		t.Fatalf("TryReserveFastPathSlot = (%v, %v), want (false, error)", ok, err)
	}
	// budget 0 不碰 Redis:Redis 挂着也不报错。
	if ok, err := q.TryReserveFastPathSlot(ctx, 1, 0); err != nil || ok {
		t.Fatalf("TryReserveFastPathSlot(budget=0) = (%v, %v), want (false, nil)", ok, err)
	}
}

// 取走时占位记账失败不让这次取走作废(票已签好);但之后读不出记账,容量判定一律不放行。
func TestConsume_PlaceholderWriteFailureDoesNotFailAdmit(t *testing.T) {
	q, rdb, _ := newClockedQueue(t)
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err != nil || n != 1 {
		t.Fatalf("popadmit = (%d, %v), want (1, nil)", n, err)
	}
	// 把占位键换成 STRING:取走时那次 ZADD 必然 WRONGTYPE。
	key := admittedZSetKey(1)
	if err := rdb.Del(ctx, key).Err(); err != nil {
		t.Fatalf("del: %v", err)
	}
	if err := rdb.Set(ctx, key, "not-a-zset", 0).Err(); err != nil {
		t.Fatalf("set: %v", err)
	}

	st, err := q.Lookup(ctx, res.QueueToken, 1, stubSignFn)
	if err != nil || st.Status != StatusAdmitted || st.Admit == nil {
		t.Fatalf("lookup = (%+v, %v), want ADMITTED despite the accounting failure", st, err)
	}
	if _, err := q.AdmittedCount(ctx, 1); err == nil {
		t.Fatal("AdmittedCount must fail on a wrong-type key")
	}
	if ok, err := q.TryReserveFastPathSlot(ctx, 1, 5); err == nil || ok {
		t.Fatalf("TryReserveFastPathSlot = (%v, %v), want (false, error)", ok, err)
	}
}

// 取走只发生一次。取走之后同一令牌还可能再查(回包丢失后的重试、go-redis 超时重发),这些查询不能给占位续期:
// 否则每次续一个交票后宽限,占位永不到期,名额一直被扣着。
// (2026-10-15 更正)此前这里写的是「客户端按协议还会每 2s 轮询(回『排队中、名次 0』,直到条目 1h 过期)」;
// 现在再查回 EXPIRED(见 TestLookup_PollAfterConsume_ReturnsExpired),客户端随即丢掉令牌,但单次再查同样不能续期。
func TestConsume_RepeatPollDoesNotExtendPlaceholder(t *testing.T) {
	q, rdb, clk := newClockedQueue(t)
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err != nil || n != 1 {
		t.Fatalf("popadmit = (%d, %v), want (1, nil)", n, err)
	}
	if st, err := q.Lookup(ctx, res.QueueToken, 1, stubSignFn); err != nil || st.Status != StatusAdmitted {
		t.Fatalf("first lookup = (%+v, %v), want ADMITTED", st, err)
	}
	want := float64(clk.Now().Add(q.handoffGrace).UnixMilli())

	clk.Advance(5 * time.Second)
	st, err := q.Lookup(ctx, res.QueueToken, 1, stubSignFn)
	if err != nil {
		t.Fatalf("repeat lookup: %v", err)
	}
	if st.Status == StatusAdmitted {
		t.Fatal("admit was consumed twice")
	}
	if st.Status != StatusExpired {
		t.Fatalf("repeat lookup status = %d, want %d (EXPIRED since 2026-10-15)", st.Status, StatusExpired)
	}
	score, err := rdb.ZScore(ctx, admittedZSetKey(1), res.QueueID).Result()
	if err != nil || score != want {
		t.Fatalf("placeholder score after a repeat poll = %.0f err=%v, want %.0f (first consume + handoff grace, not extended)",
			score, err, want)
	}
	mustAdmittedCount(t, q, 1, "5s after consume")

	clk.Advance(q.handoffGrace - 5*time.Second)
	mustAdmittedCount(t, q, 0, "handoff grace elapsed since the first consume")
}

// signFn 失败:放行槽已被 GETDEL 删掉,这个人拿不到票;占位不改分数,保持「放行时刻 + AdmitTTL」,到点自己消失。
func TestConsume_SignFailureKeepsAdmitTimeScore(t *testing.T) {
	q, rdb, clk := newClockedQueue(t)
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	admittedAt := clk.Now()
	if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err != nil || n != 1 {
		t.Fatalf("popadmit = (%d, %v), want (1, nil)", n, err)
	}
	want := float64(admittedAt.Add(q.admitTTL).UnixMilli())

	clk.Advance(2 * time.Second) // 客户端的下一次轮询
	failSign := func(*AdmitSlot) (*AdmitToken, error) { return nil, context.DeadlineExceeded }
	if st, err := q.Lookup(ctx, res.QueueToken, 1, failSign); err == nil {
		t.Fatalf("lookup = (%+v, nil), want the signing error", st)
	}
	score, err := rdb.ZScore(ctx, admittedZSetKey(1), res.QueueID).Result()
	if err != nil || score != want {
		t.Fatalf("placeholder score after a failed sign = %.0f err=%v, want %.0f (admit time + AdmitTTL, untouched)",
			score, err, want)
	}
	// 放行槽已经没了:再来轮询也拿不到票。
	if st, err := q.Lookup(ctx, res.QueueToken, 1, stubSignFn); err != nil || st.Status == StatusAdmitted {
		t.Fatalf("lookup after a failed sign = (%+v, %v), want not ADMITTED and no error", st, err)
	}

	clk.Advance(q.admitTTL - 2*time.Second - time.Millisecond)
	mustAdmittedCount(t, q, 1, "1ms before AdmitTTL")
	clk.Advance(time.Millisecond)
	mustAdmittedCount(t, q, 0, "at AdmitTTL after a failed sign")
}

// 整键 TTL 只增不减(spec_G4b-G §11 #6):排队开着时滚动调整 AdmitTTL,新旧两种 W 的副本交替写同一个 zone。
// AdmitTTL 小的副本后写,不能把 AdmitTTL 大的副本续上的整键 TTL 缩短:否则整键先于大值副本写的放行占位
// 消失,下一拍按在途 0 多放一批。三条写路径(快速通道、放行、取走)逐一钉住。
// 反过来,剩余 TTL 不足自己的 W 时照常续上(只增不减不等于「有 TTL 就不动」),三条路径也逐一钉住。
// miniredis 的 TTL 不随墙钟走、只在 FastForward 里递减,两次写之间 PTTL 不会自己变,所以断言写成等式;
// 占位分数走假时钟,FastForward 不影响它们。
// 无条件 PEXPIRE W 的写法在「smaller window: fast path」处红:PTTL 从 6min 被压到 2min。
func TestAdmittedKeyTTL_SmallerWindowNeverShortensIt(t *testing.T) {
	small, rdb, mr, cleanup := newTestQueue(t) // AdmitTTL 60s
	defer cleanup()
	big := New(rdb, time.Hour, 5*time.Minute, []byte("test-secret"))
	clk := newFakeClock()
	small.now, big.now = clk.Now, clk.Now
	ctx := context.Background()

	smallW, bigW := small.admittedKeyTTL(), big.admittedKeyTTL()
	if smallW != 2*time.Minute || bigW != 6*time.Minute {
		t.Fatalf("windows = (%v, %v), want (2m0s, 6m0s): max(AdmitTTL, handoffGrace) + 1min", smallW, bigW)
	}
	key := admittedZSetKey(1)
	mustPTTL := func(want time.Duration, when string) {
		t.Helper()
		got, err := rdb.PTTL(ctx, key).Result()
		if err != nil || got != want {
			t.Fatalf("whole-key PTTL (%s) = %v err=%v, want %v", when, got, err, want)
		}
	}
	enqueue := func(q *Queue) string {
		t.Helper()
		res, err := q.Enqueue(ctx, 1, "u", "d")
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		return res.QueueToken
	}
	admit := func(q *Queue, n int) {
		t.Helper()
		if got, err := q.PopAdmit(ctx, 1, n, pickZone1Gate); err != nil || got != n {
			t.Fatalf("popadmit = (%d, %v), want (%d, nil)", got, err, n)
		}
	}
	consume := func(q *Queue, token string) {
		t.Helper()
		if st, err := q.Lookup(ctx, token, 1, stubSignFn); err != nil || st.Status != StatusAdmitted {
			t.Fatalf("lookup = (%+v, %v), want ADMITTED", st, err)
		}
	}
	reserve := func(q *Queue) {
		t.Helper()
		if ok, err := q.TryReserveFastPathSlot(ctx, 1, 100); err != nil || !ok {
			t.Fatalf("reserve = (%v, %v), want (true, nil)", ok, err)
		}
	}

	// AdmitTTL 大的副本先放行两人。tokB 没人来取,它的占位要活到放行后 5min,整键 TTL 必须盖得住它。
	tokA, tokB := enqueue(big), enqueue(big)
	admit(big, 2)
	mustPTTL(bigW, "larger window admits first")

	// AdmitTTL 小的副本随后走三条写路径,每一条都不能把整键 TTL 缩到 smallW。
	reserve(small)
	mustPTTL(bigW, "smaller window: fast path")
	enqueue(small)
	admit(small, 1)
	mustPTTL(bigW, "smaller window: admit")
	consume(small, tokA) // 取走的是大值副本放行的条目
	mustPTTL(bigW, "smaller window: consume")

	// 拨到整键只剩 smallW − 1s:三条路径各写一次,都要续回 smallW。每段之前再拨 1s。
	// FastForward 同时让小值副本 60s 的放行槽过期;tokB 的放行槽是大值副本写的(5min),还在。
	mr.FastForward(bigW - smallW + time.Second)
	mustPTTL(smallW-time.Second, "fast-forwarded below the smaller window")
	reserve(small)
	mustPTTL(smallW, "renewed by the fast path")
	mr.FastForward(time.Second)
	mustPTTL(smallW-time.Second, "1s after the fast-path renewal")
	enqueue(small)
	admit(small, 1)
	mustPTTL(smallW, "renewed by admit")
	mr.FastForward(time.Second)
	mustPTTL(smallW-time.Second, "1s after the admit renewal")
	consume(small, tokB)
	mustPTTL(smallW, "renewed by consume")

	// 大值副本再写一次,整键 TTL 延长回 bigW。
	reserve(big)
	mustPTTL(bigW, "larger window extends it again")
}

// 占位键不存在、或者在但没有 TTL 时,任何一条写路径写完都一定有整键 TTL:否则 zone 下线之后键永久残留。
// 钉住 admittedKeyTTL 注释里的两个陷阱:换成 PEXPIRE … GT,没有 TTL 的键永远设不上;先读 PTTL 再 ZADD,
// 键不存在时 PEXPIRE 打空,随后 ZADD 建出的键没有 TTL。两种写错都在这里红(写完 PTTL = -1)。
// 「键不存在」覆盖首次写入与止血 DEL 之后;「键在但没有 TTL」用 PERSIST 造(例如有人手工 ZADD 过)。
func TestAdmittedKey_EveryWritePathSetsTTLWhenMissing(t *testing.T) {
	ctx := context.Background()
	key := admittedZSetKey(1)

	shapes := []struct {
		name  string
		apply func(t *testing.T, rdb *redis.Client)
	}{
		{"key missing", func(t *testing.T, rdb *redis.Client) {
			t.Helper()
			if err := rdb.Del(ctx, key).Err(); err != nil {
				t.Fatalf("del: %v", err)
			}
		}},
		{"key without TTL", func(t *testing.T, rdb *redis.Client) {
			t.Helper()
			// 分数远在未来:快速通道脚本先清过期成员,种子若已过期会被清掉、键随之消失,就测不到这种形状。
			if err := rdb.ZAdd(ctx, key, redis.Z{Score: 4e12, Member: "seed"}).Err(); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if err := rdb.Persist(ctx, key).Err(); err != nil {
				t.Fatalf("persist: %v", err)
			}
		}},
	}
	// 每条路径先做自己的准备,再调 shape 把占位键摆成要测的形状,最后做被测的那一次写。
	paths := []struct {
		name string
		run  func(t *testing.T, q *Queue, shape func())
	}{
		{"fast path", func(t *testing.T, q *Queue, shape func()) {
			t.Helper()
			shape()
			if ok, err := q.TryReserveFastPathSlot(ctx, 1, 10); err != nil || !ok {
				t.Fatalf("reserve = (%v, %v), want (true, nil)", ok, err)
			}
		}},
		{"admit", func(t *testing.T, q *Queue, shape func()) {
			t.Helper()
			if _, err := q.Enqueue(ctx, 1, "u", "d"); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			shape()
			if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err != nil || n != 1 {
				t.Fatalf("popadmit = (%d, %v), want (1, nil)", n, err)
			}
		}},
		{"consume", func(t *testing.T, q *Queue, shape func()) {
			t.Helper()
			res, err := q.Enqueue(ctx, 1, "u", "d")
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err != nil || n != 1 {
				t.Fatalf("popadmit = (%d, %v), want (1, nil)", n, err)
			}
			shape()
			if st, err := q.Lookup(ctx, res.QueueToken, 1, stubSignFn); err != nil || st.Status != StatusAdmitted {
				t.Fatalf("lookup = (%+v, %v), want ADMITTED", st, err)
			}
		}},
	}

	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			for _, s := range shapes {
				t.Run(s.name, func(t *testing.T) {
					q, rdb, _ := newClockedQueue(t)
					p.run(t, q, func() {
						t.Helper()
						s.apply(t, rdb)
						// go-redis 把 PTTL 的 -1(无 TTL)/ -2(键不存在)原样放进 Duration,两者都 < 0。
						if ttl, err := rdb.PTTL(ctx, key).Result(); err != nil || ttl >= 0 {
							t.Fatalf("shape %q: PTTL = %v err=%v, want no TTL (-1 or -2)", s.name, ttl, err)
						}
					})
					if ttl, err := rdb.PTTL(ctx, key).Result(); err != nil || ttl != q.admittedKeyTTL() {
						t.Fatalf("whole-key PTTL after the write = %v err=%v, want %v: a placeholder key must never be left without a TTL",
							ttl, err, q.admittedKeyTTL())
					}
				})
			}
		})
	}
}

// ── 放行窗口已过:给终态 EXPIRED,不停在名次 0(2026-10-15) ─────────────────

// expiredTotalValue 从默认 registry 读 login_queue_expired_total{zone, reason} 的当前值,序列还没出现时回 0。
// 计数器是进程级的,用例读前后差值,不依赖绝对值。
func expiredTotalValue(t *testing.T, zone, reason string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "login_queue_expired_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var z, r string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "zone":
					z = l.GetValue()
				case "reason":
					r = l.GetValue()
				}
			}
			if z == zone && r == reason {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// admitTotalValue 从默认 registry 读 login_queue_admit_total{zone} 的当前值,序列还没出现时回 0(2026-10-18,
// 仿 expiredTotalValue)。计数器是进程级的,用例读前后差值;要先 gzprom.Enable() 计数器才写。
func admitTotalValue(t *testing.T, zone string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "login_queue_admit_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "zone" && l.GetValue() == zone {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// signMustNotRun:没有放行槽可取时 signFn 不该被调用。
func signMustNotRun(t *testing.T) SignAdmitFn {
	return func(*AdmitSlot) (*AdmitToken, error) {
		t.Fatal("signFn called although there is no admit slot to take")
		return nil, nil
	}
}

// (i) 放行后没人来取,放行槽按 AdmitTTL 过期:再查回 EXPIRED(gateway 映射 410,客户端丢掉 token 重走
// /assign-gate),记一次 admit_window_passed。
// 修复前红:回「排队中、名次 0」(meta 与 token 索引还在、ZRANK 落空),客户端一直轮询到 meta 1h 过期,也不计数。
// miniredis 的 TTL 只在 FastForward 里递减,拨 AdmitTTL 正好让放行槽过期,meta 与 token 索引(1h)还在。
// 计数器要指标开关打开才写(go-zero metric.update);开关是进程级、打开后关不回去,本包其余用例不依赖它关着
// (dispatcher_test 的 leader gauge 用例自己也会打开它)。
// (2026-10-15 补注)存储布局(admitted_at 字段、meta TTL 不变)挪到 TestPopAdmit_MarksAdmittedAtKeepingMetaTTL;本用例只断言行为:
// EXPIRED、admit_window_passed 计数 +1、这次查询不给在途占位续期(假时钟与 FastForward 同拨 AdmitTTL 后计数为 0)。
// 修复前红在 EXPIRED 断言上(回 QUEUEING、名次 0)。EXPIRED 分支若误把占位改成「现在 + handoffGrace」,红在最后的计数上。
func TestLookup_AdmitSlotExpiredUnclaimed_ReturnsExpired(t *testing.T) {
	gzprom.Enable()
	q, rdb, mr, cleanup := newTestQueue(t)
	defer cleanup()
	// newClockedQueue 不返回 mr,这里手工换成假时钟:放行槽(miniredis TTL)与占位(假时钟分数)要一起拨。
	clk := newFakeClock()
	q.now = clk.Now
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err != nil || n != 1 {
		t.Fatalf("popadmit = (%d, %v), want (1, nil)", n, err)
	}

	// 放行槽(miniredis TTL)与占位(假时钟分数)一起走到「放行时刻 + AdmitTTL」。
	mr.FastForward(q.admitTTL)
	clk.Advance(q.admitTTL)
	if n, err := rdb.Exists(ctx, admitKey(res.QueueID)).Result(); err != nil || n != 0 {
		t.Fatalf("admit slot exists=%d err=%v after AdmitTTL, want gone", n, err)
	}

	before := expiredTotalValue(t, "1", ExpireReasonAdmitWindowPassed)
	st, err := q.Lookup(ctx, res.QueueToken, 1, signMustNotRun(t))
	if err != nil || st.Status != StatusExpired {
		t.Fatalf("lookup after the admit slot expired = (%+v, %v), want EXPIRED, not QUEUEING rank 0 until the 1h meta TTL", st, err)
	}
	if got := expiredTotalValue(t, "1", ExpireReasonAdmitWindowPassed) - before; got != 1 {
		t.Fatalf(`login_queue_expired_total{zone="1",reason="admit_window_passed"} grew by %v, want 1`, got)
	}
	// 占位按「放行时刻 + AdmitTTL」到期(计数的清理含端点);回 EXPIRED 的这次查询不能把它续成「现在 + handoffGrace」。
	mustAdmittedCount(t, q, 0, "at AdmitTTL after an EXPIRED lookup")
}

// (ii) 取走之后同一令牌再查(回包丢失后的重试):EXPIRED,记一次 admit_window_passed;签票只在第一次。
// 修复前红:第二次回「排队中、名次 0」,直到 meta 1h 过期。
func TestLookup_PollAfterConsume_ReturnsExpired(t *testing.T) {
	gzprom.Enable() // 见 TestLookup_AdmitSlotExpiredUnclaimed_ReturnsExpired
	q, _, _ := newClockedQueue(t)
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err != nil || n != 1 {
		t.Fatalf("popadmit = (%d, %v), want (1, nil)", n, err)
	}
	signs := 0
	countingSign := func(slot *AdmitSlot) (*AdmitToken, error) {
		signs++
		return stubSignFn(slot)
	}
	if st, err := q.Lookup(ctx, res.QueueToken, 1, countingSign); err != nil || st.Status != StatusAdmitted {
		t.Fatalf("first lookup = (%+v, %v), want ADMITTED", st, err)
	}

	before := expiredTotalValue(t, "1", ExpireReasonAdmitWindowPassed)
	st, err := q.Lookup(ctx, res.QueueToken, 1, countingSign)
	if err != nil || st.Status != StatusExpired {
		t.Fatalf("lookup after consume = (%+v, %v), want EXPIRED, not QUEUEING rank 0 until the 1h meta TTL", st, err)
	}
	if got := expiredTotalValue(t, "1", ExpireReasonAdmitWindowPassed) - before; got != 1 {
		t.Fatalf(`login_queue_expired_total{zone="1",reason="admit_window_passed"} grew by %v, want 1`, got)
	}
	if signs != 1 {
		t.Fatalf("signFn ran %d times, want exactly 1 (only the first lookup takes the slot)", signs)
	}
}

// (iii) meta 在、没有 admitted_at、又不在队列(2026-10-18 改写,原 TestLookup_InFlightPop_StillQueueingRankZero)。
// 原用例把这个形状当成「dispatcher 已 ZPOPMIN、放行还没落地」的在途窗口,要求回「排队中、名次 0」。现在放行脚本把移出
// 队列与写 admitted_at 放在同一段里,在途窗口不复存在;这个形状只来自整个 zone 空闲后队列键先到期、人工 ZREM / DEL,
// 或排队开着时混跑旧版 leader(见 lookupScript),回 EXPIRED(queue_gone),让客户端重排。手工只 ZREM 成员造出它。
// 不签票,也不给 meta 续期(PTTL 仍等于入队时的 E)。
// (2026-10-18 补注)上一句的「PTTL 仍等于 E」测不出续期:不拨钟时 PTTL 恰为 E,严格的 ttl < E 本来就不触发。
// 改为 ZREM 之后先把 miniredis 拨 1s,让 meta 与 token 索引的 PTTL 落到续期阈值 E 以下,再断言两者仍是 E-1s。
// 续期被挪到判在队(ZRANK)之前的写法红在这两条 PTTL 断言上(会变成 1.25E = 75min)。本用例不放行、不涉及
// 占位记账,不需要假时钟,改用 newTestQueue 拿到 mr。
// 修复前红:回 QUEUEING、名次 0。
func TestLookup_MetaPresentNotQueuedNotAdmitted_ReturnsQueueGone(t *testing.T) {
	gzprom.Enable() // 见 TestLookup_AdmitSlotExpiredUnclaimed_ReturnsExpired
	q, rdb, mr, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if n, err := rdb.ZRem(ctx, zoneZSetKey(1), res.QueueID).Result(); err != nil || n != 1 {
		t.Fatalf("zrem = (%d, %v), want (1, nil)", n, err)
	}
	// 拨 1s,让 meta 与 token 索引的 PTTL 低于续期阈值 E:若 queue_gone 分支误续期,这里一定触发。
	mr.FastForward(time.Second)

	before := expiredTotalValue(t, "1", ExpireReasonQueueGone)
	st, err := q.Lookup(ctx, res.QueueToken, 1, signMustNotRun(t))
	if err != nil || st.Status != StatusExpired {
		t.Fatalf("lookup with meta present but neither queued nor admitted = (%+v, %v), want EXPIRED (queue_gone)", st, err)
	}
	if got := expiredTotalValue(t, "1", ExpireReasonQueueGone) - before; got != 1 {
		t.Fatalf(`login_queue_expired_total{zone="1",reason="queue_gone"} grew by %v, want 1`, got)
	}
	mustKeyPTTL(t, rdb, metaKey(res.QueueID), q.entryTTL-time.Second, "queue_gone must not renew the meta")
	mustKeyPTTL(t, rdb, tokenIndexKey(res.QueueToken), q.entryTTL-time.Second, "queue_gone must not renew the token index")
}

/// (iv) 放行落在两次轮询之间(2026-11-06 改写,原 TestLookup_AdmitLandsBetweenGetDelAndMetaRead_Admits)。lookupScript 在同一段里
// 核对令牌、取放行槽、读 admitted_at,放行脚本在同一段里写 admitted_at 与放行槽,两者互斥原子;原用例模拟的「第一次 GETDEL
// 落空之后放行才落地」的窗口不复存在,注入点 afterAdmitMiss 随之删除。这里钉住合并之后的两条性质:
//   - 刚放行、还没取走的条目,下一次轮询在一段脚本里取到放行槽:ADMITTED、只签一次票,占位改为「取走时刻 + handoffGrace」;
//   - 取走不再单发 GETDEL(拆回三次往返的写法红在 getdel 计数上)。
func TestLookup_AdmitBetweenPolls_TakenInOneScript(t *testing.T) {
	q, rdb, clk := newClockedQueue(t)
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if st, err := q.Lookup(ctx, res.QueueToken, 1, signMustNotRun(t)); err != nil || st.Status != StatusQueueing {
		t.Fatalf("lookup before admit = (%+v, %v), want QUEUEING", st, err)
	}
	if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err != nil || n != 1 {
		t.Fatalf("popadmit = (%d, %v), want (1, nil)", n, err)
	}
	getdels := 0
	rdb.AddHook(cmdHook{process: func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		if cmd.Name() == "getdel" {
			getdels++
		}
		return next(ctx, cmd)
	}})
	signs := 0
	countingSign := func(slot *AdmitSlot) (*AdmitToken, error) {
		signs++
		return stubSignFn(slot)
	}

	st, err := q.Lookup(ctx, res.QueueToken, 1, countingSign)
	if err != nil || st.Status != StatusAdmitted || st.Admit == nil || st.Admit.IP != "10.0.0.1" {
		t.Fatalf("lookup = (%+v, %v), want ADMITTED with the gate picked at admit time", st, err)
	}
	if signs != 1 {
		t.Fatalf("signFn ran %d times, want exactly 1", signs)
	}
	if getdels != 0 {
		t.Fatalf("lookup sent %d standalone GETDEL commands, want 0: the admit slot is taken inside lookupScript", getdels)
	}
	if n, err := rdb.Exists(ctx, admitKey(res.QueueID)).Result(); err != nil || n != 0 {
		t.Fatalf("admit slot exists=%d err=%v after lookup, want taken", n, err)
	}
	score, err := rdb.ZScore(ctx, admittedZSetKey(1), res.QueueID).Result()
	if want := float64(clk.Now().Add(q.handoffGrace).UnixMilli()); err != nil || score != want {
		t.Fatalf("placeholder score = %.0f err=%v, want %.0f (consume time + handoff grace, not admit time + AdmitTTL)",
			score, err, want)
	}
}

// 排队中的一次轮询只有一次 Redis 往返(2026-11-06,缺口清单第 14 条)。先轮询一次让脚本进 Redis 的脚本缓存(进程内首次
// EVALSHA 撞 NOSCRIPT 会多一次 EVAL 回退),再数第二次轮询发了哪些命令。
// 拆回「GET token 索引 + GETDEL + 读状态脚本」的写法红在这里(3 条)。
func TestLookup_QueueingPollIsOneRoundTrip(t *testing.T) {
	q, rdb, _, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if st, err := q.Lookup(ctx, res.QueueToken, 1, signMustNotRun(t)); err != nil || st.Status != StatusQueueing {
		t.Fatalf("warm-up lookup = (%+v, %v), want QUEUEING", st, err)
	}
	var sent []string
	rdb.AddHook(cmdHook{process: func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		sent = append(sent, cmd.Name())
		return next(ctx, cmd)
	}})

	st, err := q.Lookup(ctx, res.QueueToken, 1, signMustNotRun(t))
	if err != nil || st.Status != StatusQueueing || st.Rank != 0 || st.Total != 1 {
		t.Fatalf("lookup = (%+v, %v), want QUEUEING rank 0 of 1", st, err)
	}
	if !slices.Equal(sent, []string{"evalsha"}) {
		t.Fatalf("a queueing poll sent %v, want exactly one EVALSHA", sent)
	}
}

// 令牌正文里的 queueId 被改成别人的(Lookup 不验签,验签在调用方):token 索引按令牌全文建,改过的令牌没有索引,回
// EXPIRED(meta_gone),别人的放行槽原样留着、不被取走,本人的令牌照常取到。去掉 lookupScript 第一段令牌核对的写法红在
// 这里(会按令牌里的 queueId 取走 bob 的放行槽)。改成只看索引在不在(EXISTS)仍是绿的:改过的令牌本来就没有索引,
// 比对索引值是纵深防御,见 lookupScript 注释。
func TestLookup_TokenWithForeignQueueID_DoesNotTakeOthersAdmit(t *testing.T) {
	gzprom.Enable() // 见 TestLookup_AdmitSlotExpiredUnclaimed_ReturnsExpired
	q, rdb, _ := newClockedQueue(t)
	ctx := context.Background()

	alice, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue alice: %v", err)
	}
	bob, err := q.Enqueue(ctx, 1, "bob", "dev2")
	if err != nil {
		t.Fatalf("enqueue bob: %v", err)
	}
	if n, err := q.PopAdmit(ctx, 1, 2, pickZone1Gate); err != nil || n != 2 {
		t.Fatalf("popadmit = (%d, %v), want (2, nil)", n, err)
	}
	forged := retargetQueueToken(t, alice.QueueToken, bob.QueueID)

	before := expiredTotalValue(t, "1", ExpireReasonMetaGone)
	st, err := q.Lookup(ctx, forged, 1, signMustNotRun(t))
	if err != nil || st.Status != StatusExpired {
		t.Fatalf("lookup with a retargeted token = (%+v, %v), want EXPIRED", st, err)
	}
	if got := expiredTotalValue(t, "1", ExpireReasonMetaGone) - before; got != 1 {
		t.Fatalf(`login_queue_expired_total{zone="1",reason="meta_gone"} grew by %v, want 1`, got)
	}
	if n, err := rdb.Exists(ctx, admitKey(bob.QueueID)).Result(); err != nil || n != 1 {
		t.Fatalf("bob's admit slot exists=%d err=%v, want it untouched for bob's own token", n, err)
	}
	if st, err := q.Lookup(ctx, bob.QueueToken, 1, stubSignFn); err != nil || st.Status != StatusAdmitted {
		t.Fatalf("bob's own lookup = (%+v, %v), want ADMITTED", st, err)
	}
}

// retargetQueueToken 把排队令牌正文里的 queueId 换成 queueID,签名原样带着(已经对不上;Lookup 不验签)。
func retargetQueueToken(t *testing.T, token, queueID string) string {
	t.Helper()
	bodyBytes, sig, err := splitQueueToken(token)
	if err != nil {
		t.Fatalf("split queue token: %v", err)
	}
	var body queueTokenBody
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("unmarshal queue token: %v", err)
	}
	body.QueueID = queueID
	forged, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal queue token: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(append(append(forged, '.'), sig...))
}

/ PopAdmit 在 meta 里记下 admitted_at(包注释 Redis 布局:Lookup 据此区分「放行窗口已过」与「在途窗口」),
// 且不动 meta 的 TTL:仍按入队时的 QueueEntryTTL 到期。miniredis 的 TTL 只在 FastForward 里递减,入队后先拨
// waited,meta 剩 entryTTL-waited;写 admitted_at 时被抹掉(-1)、被续期(回到 entryTTL)、被缩短都在等式处红。
// 不拨时间的区间断言 (0, entryTTL] 分不出「保持」与「续期到 entryTTL」。
// 这是存储布局用例,行为由 TestLookup_AdmitSlotExpiredUnclaimed_ReturnsExpired 钉住;修复前红在 admitted_at 断言上。
func TestPopAdmit_MarksAdmittedAtKeepingMetaTTL(t *testing.T) {
	q, rdb, mr, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if v, err := rdb.HGet(ctx, metaKey(res.QueueID), metaFieldAdmittedAt).Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("meta %s before admit = (%q, %v), want absent", metaFieldAdmittedAt, v, err)
	}
	const waited = 10 * time.Minute // 在队里等过的时间,须小于 entryTTL(1h)
	mr.FastForward(waited)
	want := q.entryTTL - waited
	if ttl, err := rdb.PTTL(ctx, metaKey(res.QueueID)).Result(); err != nil || ttl != want {
		t.Fatalf("meta PTTL before admit = %v err=%v, want %v", ttl, err, want)
	}
	if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err != nil || n != 1 {
		t.Fatalf("popadmit = (%d, %v), want (1, nil)", n, err)
	}
	if v, err := rdb.HGet(ctx, metaKey(res.QueueID), metaFieldAdmittedAt).Result(); err != nil || v == "" {
		t.Fatalf("meta %s after admit = (%q, %v), want the admit time", metaFieldAdmittedAt, v, err)
	}
	if ttl, err := rdb.PTTL(ctx, metaKey(res.QueueID)).Result(); err != nil || ttl != want {
		t.Fatalf("meta PTTL after admit = %v err=%v, want %v: writing admitted_at must neither drop, renew nor shorten the entry TTL", ttl, err, want)
	}
}

// 写 admitted_at 不凭空建出 meta:meta 已不在(在队里待满 QueueEntryTTL 或被内存淘汰)时 PopAdmit 照常写放行槽,
// 但不建出一个没有 TTL、永不过期的 meta。直接 HSET 的写法在这里红(meta 存在且 TTL = -1)。
// 修复前也绿(那时不写 meta),钉住的是新写入的安全性。
// (2026-10-15 补注)后半段断言翻转:meta 不在时 PopAdmit 整条丢弃——不写放行槽、不占在途名额、不计入放行数,
// 条目也不回队列。生产里 meta 不在时 token 索引同时不在(Enqueue 同一个 MULTI、同一个 TTL 设下,部署不淘汰键),
// 没有人能取到放行槽,写了只是白占一个 AdmitTTL 的名额。本用例手工只删 meta、留下 token 索引,这个形状生产里
// 只有人工删键才造得出;此时查询回 EXPIRED(meta_gone),客户端重新入队。修复前红在 n == 0 上(回 1)。
// 走真实到期路径的版本见 TestPopAdmit_EntryOlderThanQueueEntryTTL_NotAdmitted。
// (2026-10-18 更正)「整条丢弃」「条目也不回队列」改读:成员由放行脚本的 META_GONE 分支 ZREM 掉(不再是 ZPOPMIN 弹出后
// 丢弃),断言不变。
func TestPopAdmit_MetaGone_DoesNotRecreateMeta(t *testing.T) {
	q, rdb, _ := newClockedQueue(t)
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := rdb.Del(ctx, metaKey(res.QueueID)).Err(); err != nil {
		t.Fatalf("del meta: %v", err)
	}
	if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err != nil || n != 0 {
		t.Fatalf("popadmit = (%d, %v), want (0, nil): an entry whose meta is gone is not admitted", n, err)
	}
	if n, err := rdb.Exists(ctx, metaKey(res.QueueID)).Result(); err != nil || n != 0 {
		t.Fatalf("meta exists=%d err=%v after admit, want still gone", n, err)
	}
	if n, err := rdb.Exists(ctx, admitKey(res.QueueID)).Result(); err != nil || n != 0 {
		t.Fatalf("admit slot exists=%d err=%v, want none written for an orphan", n, err)
	}
	mustAdmittedCount(t, q, 0, "orphan not held")
	if n, err := rdb.ZCard(ctx, zoneZSetKey(1)).Result(); err != nil || n != 0 {
		t.Fatalf("queue length = %d err=%v, want 0 (the admit script ZREMs the orphan, META_GONE)", n, err)
	}
	if st, err := q.Lookup(ctx, res.QueueToken, 1, signMustNotRun(t)); err != nil || st.Status != StatusExpired {
		t.Fatalf("lookup = (%+v, %v), want EXPIRED (meta_gone)", st, err)
	}
}

// 真实到期路径:A 在队里待满 QueueEntryTTL,meta 与 token 索引同时过期(Enqueue 同一个 MULTI、同一个 TTL);
// 队列键因 B 入队续了 TTL,A 仍是队首成员。同一批弹出 A、B:A 不写放行槽、不占在途名额、不计入放行数,
// B 照常放行并能取走。修复前红在 n == 1 上(回 2:孤儿 A 也写了放行槽和占位,白占一个 AdmitTTL 的名额)。
// miniredis 的 TTL 只在 FastForward 里递减;占位走假时钟。
// (2026-10-18 补注)QueueEntryTTL 现为空闲 TTL:A 从未轮询,所以它的 meta 与 token 索引仍在入队后 E 到期,形状不变。
// 「队列键因 B 入队续了 TTL」改读:B 入队走 NX/GT,队列键的 TTL 只增不减(剩 30min < E,延长到 E)。另加 PopAdmit 之后
// ZCARD == 0:A 由放行脚本的 META_GONE 分支 ZREM 掉。
func TestPopAdmit_EntryOlderThanQueueEntryTTL_NotAdmitted(t *testing.T) {
	q, rdb, mr, cleanup := newTestQueue(t)
	defer cleanup()
	q.now = newFakeClock().Now
	ctx := context.Background()

	a, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue A: %v", err)
	}
	mr.FastForward(q.entryTTL / 2)
	b, err := q.Enqueue(ctx, 1, "bob", "dev2") // 队列键剩 30min,GT 延长到 entryTTL;A 的 meta 与 token 索引不续
	if err != nil {
		t.Fatalf("enqueue B: %v", err)
	}
	mr.FastForward(q.entryTTL - q.entryTTL/2)
	if n, err := rdb.Exists(ctx, metaKey(a.QueueID), tokenIndexKey(a.QueueToken)).Result(); err != nil || n != 0 {
		t.Fatalf("A's meta + token index exist=%d err=%v after QueueEntryTTL, want both expired", n, err)
	}
	if n, err := rdb.ZCard(ctx, zoneZSetKey(1)).Result(); err != nil || n != 2 {
		t.Fatalf("queue length = %d err=%v, want 2 (A is still a member)", n, err)
	}

	if n, err := q.PopAdmit(ctx, 1, 2, pickZone1Gate); err != nil || n != 1 {
		t.Fatalf("popadmit = (%d, %v), want (1, nil): only B is admitted", n, err)
	}
	if n, err := rdb.ZCard(ctx, zoneZSetKey(1)).Result(); err != nil || n != 0 {
		t.Fatalf("queue length after popadmit = %d err=%v, want 0 (A removed as META_GONE, B admitted)", n, err)
	}
	if n, err := rdb.Exists(ctx, admitKey(a.QueueID)).Result(); err != nil || n != 0 {
		t.Fatalf("A's admit slot exists=%d err=%v, want none", n, err)
	}
	mustAdmittedCount(t, q, 1, "only B holds a slot")
	if st, err := q.Lookup(ctx, a.QueueToken, 1, signMustNotRun(t)); err != nil || st.Status != StatusExpired {
		t.Fatalf("lookup A = (%+v, %v), want EXPIRED", st, err)
	}
	if st, err := q.Lookup(ctx, b.QueueToken, 1, stubSignFn); err != nil || st.Status != StatusAdmitted {
		t.Fatalf("lookup B = (%+v, %v), want ADMITTED", st, err)
	}
}

// 停服(Stop → cancel)或丢租约(onLost → drainCancel)取消的是 dispatcher 传进来的 ctx(2026-10-18 改写,原
// TestPopAdmit_CtxCanceledMidBatch_StillWritesPoppedEntries:当时 ZPOPMIN 已把整批弹出,取消之后必须照写,否则丢条目)。
// 现在条目在放行脚本写成之前一直在队列里,取消即停:取消之前写成的第 1 条照常放行、计数,其余两条原样留在队列(分数
// 不变、没有放行槽),错误是 context.Canceled,用新的 ctx 下一拍照常放行。用 pickFn 在第 2 次挑 gate 时取消 ctx,
// 确定地落在「第 1 条已写、第 2 条还没写」之间。
// 修复前红:回 (3, nil)(取消之后照样写完,写入预算没被触发)。
func TestPopAdmit_CtxCanceledMidBatch_RestStayQueued(t *testing.T) {
	q, rdb, _ := newClockedQueue(t)
	bg := context.Background()

	tokens := enqueueAccounts(t, q, "alice", "bob", "carol")
	before := queueScores(t, rdb)

	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	var picked []string
	cancelOnSecondPick := func(zoneID uint32, queueID string) (*AdmitSlot, error) {
		picked = append(picked, queueID)
		if len(picked) == 2 {
			cancel() // 第 1 条已写完,第 2、3 条还在队列、还没写
		}
		return pickZone1Gate(zoneID, queueID)
	}
	n, err := q.PopAdmit(ctx, 1, 3, cancelOnSecondPick)
	if n != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("popadmit = (%d, %v), want (1, context.Canceled): stop at the cancel, keep the entry written before it", n, err)
	}
	if len(picked) != 2 {
		t.Fatalf("pickFn ran %d times, want 2 (the batch stops before the third pick)", len(picked))
	}
	after := queueScores(t, rdb)
	if len(after) != 2 {
		t.Fatalf("queue after the cancel = %v, want the two entries not yet admitted", after)
	}
	for queueID, score := range after {
		if score != before[queueID] || queueID == picked[0] {
			t.Fatalf("queued entry %s score %.0f, want an untouched entry with its enqueue score %.0f", queueID, score, before[queueID])
		}
		if n, err := rdb.Exists(bg, admitKey(queueID)).Result(); err != nil || n != 0 {
			t.Fatalf("admit slot of %s exists=%d err=%v, want none", queueID, n, err)
		}
	}
	mustAdmittedCount(t, q, 1, "only the entry written before the cancel holds a slot")
	if st, err := q.Lookup(bg, tokens[picked[0]], 1, stubSignFn); err != nil || st.Status != StatusAdmitted {
		t.Fatalf("lookup %s = (%+v, %v), want ADMITTED", picked[0], st, err)
	}
	for queueID := range after {
		st, err := q.Lookup(bg, tokens[queueID], 1, signMustNotRun(t))
		if err != nil || st.Status != StatusQueueing || st.Total != 2 {
			t.Fatalf("lookup %s = (%+v, %v), want QUEUEING with total 2", queueID, st, err)
		}
	}
	if n, err := q.PopAdmit(bg, 1, 2, pickZone1Gate); err != nil || n != 2 {
		t.Fatalf("popadmit with a fresh ctx = (%d, %v), want (2, nil)", n, err)
	}
}

// 调用方 ctx 在读候选之前就已取消:ZRANGE 随它失败,一个条目都不放行、不写,分数不变(2026-10-18 改名,原
// TestPopAdmit_CtxCanceledBeforePop_PopsNothing;原注释「ZPOPMIN 仍随它失败」「只有弹出之后的写入不随调用方取消」
// 改读:ZRANGE 与放行脚本都随调用方 ctx,停服或丢租约之后不再放行新的一批)。修复前后都绿。
func TestPopAdmit_CtxCanceledBeforeRead_AdmitsNothing(t *testing.T) {
	q, rdb, _ := newClockedQueue(t)
	bg := context.Background()

	res, err := q.Enqueue(bg, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	score, err := rdb.ZScore(bg, zoneZSetKey(1), res.QueueID).Result()
	if err != nil {
		t.Fatalf("zscore after enqueue: %v", err)
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err == nil || n != 0 {
		t.Fatalf("popadmit with a canceled ctx = (%d, %v), want (0, error)", n, err)
	}
	if n, err := rdb.ZCard(bg, zoneZSetKey(1)).Result(); err != nil || n != 1 {
		t.Fatalf("queue length = %d err=%v, want 1 (nothing admitted)", n, err)
	}
	if got, err := rdb.ZScore(bg, zoneZSetKey(1), res.QueueID).Result(); err != nil || got != score {
		t.Fatalf("queue score = %.0f err=%v, want %.0f (unchanged)", got, err, score)
	}
	if n, err := rdb.Exists(bg, admitKey(res.QueueID)).Result(); err != nil || n != 0 {
		t.Fatalf("admit slot exists=%d err=%v, want none", n, err)
	}
}

// enqueueAccounts 往 zone 1 依次入队,返回 queueId → 排队令牌。入队分数是毫秒墙钟,同一毫秒内的条目按 queueId
// 字典序出队,用例不能假定出队顺序等于入队顺序,要按 pickFn 实际看到的 queueId 对号。
func enqueueAccounts(t *testing.T, q *Queue, accounts ...string) map[string]string {
	t.Helper()
	tokens := make(map[string]string, len(accounts))
	for _, account := range accounts {
		res, err := q.Enqueue(context.Background(), 1, account, "dev")
		if err != nil {
			t.Fatalf("enqueue %s: %v", account, err)
		}
		tokens[res.QueueID] = res.QueueToken
	}
	return tokens
}

// (2026-10-18)原 TestPopAdmit_NoCancel_LongBatchNotCutByWriteBudget 与 TestPopAdmit_CtxCanceledMidBatch_WriteBudgetExhausted_DropsRest
// 随写入预算(popAdmitWriteBudget 字段与常量)一起删除:条目在放行脚本写成之前不离开队列,取消即停,「没有取消时不截断」
// 已无可测对象;取消后的行为由 TestPopAdmit_CtxCanceledMidBatch_RestStayQueued 钉住。

// queueScores 读 zone 1 队列的全部成员与分数(2026-10-18)。
func queueScores(t *testing.T, rdb *redis.Client) map[string]float64 {
	t.Helper()
	zs, err := rdb.ZRangeWithScores(context.Background(), zoneZSetKey(1), 0, -1).Result()
	if err != nil {
		t.Fatalf("zrange withscores: %v", err)
	}
	out := make(map[string]float64, len(zs))
	for _, z := range zs {
		member, _ := z.Member.(string)
		out[member] = z.Score
	}
	return out
}

// mustKeyPTTL 断言 key 的剩余 TTL 恰为 want。miniredis 的 TTL 只在 FastForward 里递减,两次拨钟之间不会自己变,
// 所以可以写成等式(2026-10-18)。
func mustKeyPTTL(t *testing.T, rdb *redis.Client, key string, want time.Duration, when string) {
	t.Helper()
	got, err := rdb.PTTL(context.Background(), key).Result()
	if err != nil || got != want {
		t.Fatalf("PTTL %s (%s) = %v err=%v, want %v", key, when, got, err, want)
	}
}

// lookupQueueingRanks 逐个查询 tokens,要求都回 QUEUEING 且总数为 wantTotal,返回排好序的名次(2026-10-18)。
// signFn 不该被调用:还没有放行槽。
func lookupQueueingRanks(t *testing.T, q *Queue, tokens []string, wantTotal uint32, when string) []uint32 {
	t.Helper()
	ranks := make([]uint32, 0, len(tokens))
	for _, token := range tokens {
		st, err := q.Lookup(context.Background(), token, 1, signMustNotRun(t))
		if err != nil || st.Status != StatusQueueing || st.Total != wantTotal {
			t.Fatalf("lookup (%s) = (%+v, %v), want QUEUEING with total %d", when, st, err, wantTotal)
		}
		ranks = append(ranks, st.Rank)
	}
	slices.Sort(ranks)
	return ranks
}

// cmdHook 是通用的命令拦截钩子(2026-10-18):process 拿到每条命令与下一环,自己决定转发还是直接回错误。
// 直接回错误时必须 cmd.SetErr(err) 并返回 err(见 failCmd),调用方的 Cmd 才看得到这个错误。
// (2026-11-06)PopAdmit 探路之后的放行脚本在 pipeline 里发,pipeline 里的命令也逐条交给 process:process 调 next 的
// 照常发出(整块一次发),直接回错误的不发、就地失败。process 里调 next 只是登记「要发」,返回时命令还没执行,
// 要看结果的钩子只能拦单发命令。MULTI(TxPipeline,首条是 multi)整块原样转发,不逐条拦截。
type cmdHook struct {
	process func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error
}

func (h cmdHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h cmdHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error { return h.process(ctx, cmd, next) }
}

func (h cmdHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if len(cmds) > 0 && cmds[0].Name() == "multi" {
			return next(ctx, cmds)
		}
		forward := make([]redis.Cmder, 0, len(cmds))
		var firstErr error
		for _, cmd := range cmds {
			err := h.process(ctx, cmd, func(_ context.Context, c redis.Cmder) error {
				forward = append(forward, c)
				return nil
			})
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if len(forward) > 0 {
			if err := next(ctx, forward); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
}

// admitScriptKey 识别放行脚本:EVAL / EVALSHA 且第一个键(args[3],放行脚本的 KEYS[1] 是放行槽)以 admit: 开头,
// 返回该键。EVAL / EVALSHA 的参数:命令名、脚本或 sha、键数、键...。读状态脚本的第一个键是 queue:meta:,不会误中。
func admitScriptKey(cmd redis.Cmder) (string, bool) {
	if name := cmd.Name(); name != "eval" && name != "evalsha" {
		return "", false
	}
	args := cmd.Args()
	if len(args) < 4 {
		return "", false
	}
	key, ok := args[3].(string)
	if !ok || !strings.HasPrefix(key, "admit:") {
		return "", false
	}
	return key, true
}

// failCmd 让 cmd 以 err 结束,不发往 Redis。
func failCmd(cmd redis.Cmder, err error) error {
	cmd.SetErr(err)
	return err
}

// N1(D1 回归,2026-10-18):Redis 写满(noeviction)时放行脚本被拒。钩子对放行脚本一律回 OOM、不发往 Redis,模拟真实
// Redis 在写段第一条写(HSET,DENYOOM)处拒绝整段(见 admitEntryScript 注释 (c))。条目必须原样留在队列、分数不变、
// 什么都没写,本批在第一条处停下;查询照常回 QUEUEING;写满解除后照常放行。
// 修复前红:ZPOPMIN 不带 DENYOOM 照常弹空了队列,脚本被拒,ZCARD == 0、err == nil(条目既不在队列也没有放行)。
func TestPopAdmit_OOMOnAdmitScript_EntriesStayQueued(t *testing.T) {
	q, rdb, _ := newClockedQueue(t)
	ctx := context.Background()
	tokens := enqueueAccounts(t, q, "alice", "bob", "carol")
	before := queueScores(t, rdb)

	var oom atomic.Bool
	oom.Store(true)
	hits := 0
	rdb.AddHook(cmdHook{process: func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		if _, ok := admitScriptKey(cmd); ok && oom.Load() {
			hits++
			return failCmd(cmd, errors.New("OOM command not allowed when used memory > 'maxmemory'."))
		}
		return next(ctx, cmd)
	}})

	n, err := q.PopAdmit(ctx, 1, 3, pickZone1Gate)
	if n != 0 || err == nil || !strings.Contains(err.Error(), "OOM") {
		t.Fatalf("popadmit under OOM = (%d, %v), want (0, an error mentioning OOM)", n, err)
	}
	if hits != 1 {
		t.Fatalf("admit script attempted %d times, want 1: the batch must stop at the first script error", hits)
	}
	if after := queueScores(t, rdb); !maps.Equal(after, before) {
		t.Fatalf("queue after OOM = %v, want unchanged %v", after, before)
	}
	if keys, err := rdb.Keys(ctx, "admit:*").Result(); err != nil || len(keys) != 0 {
		t.Fatalf("admit slots = %v err=%v, want none", keys, err)
	}
	mustAdmittedCount(t, q, 0, "nothing admitted under OOM")
	for queueID := range tokens {
		if has, err := rdb.HExists(ctx, metaKey(queueID), metaFieldAdmittedAt).Result(); err != nil || has {
			t.Fatalf("meta of %s has %s=%v err=%v, want absent", queueID, metaFieldAdmittedAt, has, err)
		}
	}
	all := slices.Collect(maps.Values(tokens))
	if ranks := lookupQueueingRanks(t, q, all, 3, "under OOM"); !slices.Equal(ranks, []uint32{0, 1, 2}) {
		t.Fatalf("ranks under OOM = %v, want [0 1 2]", ranks)
	}

	oom.Store(false)
	if n, err := q.PopAdmit(ctx, 1, 3, pickZone1Gate); err != nil || n != 3 {
		t.Fatalf("popadmit after OOM cleared = (%d, %v), want (3, nil)", n, err)
	}
	for queueID, token := range tokens {
		if st, err := q.Lookup(ctx, token, 1, stubSignFn); err != nil || st.Status != StatusAdmitted {
			t.Fatalf("lookup %s after OOM cleared = (%+v, %v), want ADMITTED", queueID, st, err)
		}
	}
}

// N2(2026-10-18):脚本中途出错(在途占位键被改成 string,ZADD 报 WRONGTYPE)时本批在第一个出错处停下,剩余条目原样
// 留在队列。出错那条的 admitted_at 与放行槽已写(脚本不回滚,见 admitEntryScript 注释 (d)),客户端仍能取走;占位键
// 修好之后的下一拍走 ALREADY,把它移出队列,不重复放行、不签第二张票。
// 修复前红:ZCARD == 0(三人都已被 ZPOPMIN 弹出,写入出错的条目丢失),err == nil。
func TestPopAdmit_ScriptErrorStopsBatch_PartialWriteCleanedAsAlready(t *testing.T) {
	q, rdb, _ := newClockedQueue(t)
	ctx := context.Background()
	tokens := enqueueAccounts(t, q, "alice", "bob", "carol")
	order, err := rdb.ZRange(ctx, zoneZSetKey(1), 0, -1).Result()
	if err != nil || len(order) != 3 {
		t.Fatalf("queue order = %v err=%v, want 3 members", order, err)
	}
	head := order[0]
	if err := rdb.Set(ctx, admittedZSetKey(1), "not-a-zset", 0).Err(); err != nil {
		t.Fatalf("break the placeholder key: %v", err)
	}

	n, err := q.PopAdmit(ctx, 1, 3, pickZone1Gate)
	if n != 0 || err == nil || !strings.Contains(err.Error(), "WRONGTYPE") {
		t.Fatalf("popadmit with a wrong-type placeholder key = (%d, %v), want (0, a WRONGTYPE error)", n, err)
	}
	if n, err := rdb.ZCard(ctx, zoneZSetKey(1)).Result(); err != nil || n != 3 {
		t.Fatalf("queue length = %d err=%v, want 3 (nothing leaves the queue on a script error)", n, err)
	}
	for i, queueID := range order {
		want := i == 0 // 只有队首执行到了 ZADD 之前的 HSET 与 SET
		has, err := rdb.HExists(ctx, metaKey(queueID), metaFieldAdmittedAt).Result()
		slot, slotErr := rdb.Exists(ctx, admitKey(queueID)).Result()
		if err != nil || slotErr != nil || has != want || (slot == 1) != want {
			t.Fatalf("entry #%d: admitted_at=%v slot=%d (errs %v / %v), want both present=%v", i, has, slot, err, slotErr, want)
		}
	}
	signs := 0
	countingSign := func(slot *AdmitSlot) (*AdmitToken, error) {
		signs++
		return stubSignFn(slot)
	}
	if st, err := q.Lookup(ctx, tokens[head], 1, countingSign); err != nil || st.Status != StatusAdmitted {
		t.Fatalf("lookup head after the partial write = (%+v, %v), want ADMITTED (its admit slot was written)", st, err)
	}

	if err := rdb.Del(ctx, admittedZSetKey(1)).Err(); err != nil {
		t.Fatalf("repair the placeholder key: %v", err)
	}
	if n, err := q.PopAdmit(ctx, 1, 3, pickZone1Gate); err != nil || n != 2 {
		t.Fatalf("popadmit after the repair = (%d, %v), want (2, nil): head cleaned as ALREADY, the other two admitted", n, err)
	}
	if n, err := rdb.ZCard(ctx, zoneZSetKey(1)).Result(); err != nil || n != 0 {
		t.Fatalf("queue length after the repair = %d err=%v, want 0", n, err)
	}
	if st, err := q.Lookup(ctx, tokens[head], 1, countingSign); err != nil || st.Status != StatusExpired {
		t.Fatalf("lookup head again = (%+v, %v), want EXPIRED", st, err)
	}
	if signs != 1 {
		t.Fatalf("signFn ran %d times for the head, want exactly 1", signs)
	}
}

// N3(2026-10-18):重叠 leader 放行同一条目时只写一次。外层 PopAdmit 已 ZRANGE 到这个条目,挑 gate 时另一个 leader
// (嵌套的 PopAdmit)抢先放行了它;外层的脚本第一步发现成员已不在队列,回 GONE,什么都不写、不计数。
// 嵌套调用之后等墙钟走过这一毫秒再让外层继续:两个 leader 恰在同一毫秒放行同一条目时,后到的会被当成 REPLAYED 计数
// (登记过的残余,只影响指标;放行时刻按规格取墙钟,与 createdAt 同源),这一步让结果确定地落在「另一 leader 更早放行」上。
// 修复前红:嵌套调用回 0(外层的 ZPOPMIN 已经把条目弹走)。
func TestPopAdmit_OverlappingLeaders_SameEntryAdmittedOnce(t *testing.T) {
	q, rdb, _ := newClockedQueue(t)
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	nestedRan := false
	var nestedN int
	var nestedErr error
	outerPick := func(zoneID uint32, queueID string) (*AdmitSlot, error) {
		if !nestedRan {
			nestedRan = true
			nestedN, nestedErr = q.PopAdmit(ctx, 1, 1, pickZone1Gate)
			for t0 := time.Now().UnixMilli(); time.Now().UnixMilli() == t0; {
				runtime.Gosched()
			}
		}
		return pickZone1Gate(zoneID, queueID)
	}
	n, err := q.PopAdmit(ctx, 1, 1, outerPick)
	if !nestedRan || nestedErr != nil || nestedN != 1 {
		t.Fatalf("nested leader popadmit = (%d, %v) ran=%v, want (1, nil)", nestedN, nestedErr, nestedRan)
	}
	if err != nil || n != 0 {
		t.Fatalf("outer leader popadmit = (%d, %v), want (0, nil): the entry was already admitted by the other leader (GONE)", n, err)
	}
	if keys, err := rdb.Keys(ctx, "admit:*").Result(); err != nil || len(keys) != 1 {
		t.Fatalf("admit slots = %v err=%v, want exactly one", keys, err)
	}
	mustAdmittedCount(t, q, 1, "one placeholder for one admit")
	signs := 0
	countingSign := func(slot *AdmitSlot) (*AdmitToken, error) {
		signs++
		return stubSignFn(slot)
	}
	if st, err := q.Lookup(ctx, res.QueueToken, 1, countingSign); err != nil || st.Status != StatusAdmitted {
		t.Fatalf("first lookup = (%+v, %v), want ADMITTED", st, err)
	}
	if st, err := q.Lookup(ctx, res.QueueToken, 1, countingSign); err != nil || st.Status != StatusExpired {
		t.Fatalf("second lookup = (%+v, %v), want EXPIRED", st, err)
	}
	if signs != 1 {
		t.Fatalf("signFn ran %d times, want exactly 1", signs)
	}
}

// N4(2026-10-18):meta 被写成别的类型(毒丸)不能卡死整个 zone。查询回 EXPIRED(meta_gone),不报错;出队时脚本按
// META_GONE 把它移出队列,不写放行、不碰那个坏键,排在后面的人照常放行。
// 修复前红:Lookup 回 error(HMGET WRONGTYPE);放行脚本的 HEXISTS 同样 WRONGTYPE,整批停在这一条。
func TestPopAdmit_MetaNotHash_CleanedWithoutStallingZone(t *testing.T) {
	gzprom.Enable() // 见 TestLookup_AdmitSlotExpiredUnclaimed_ReturnsExpired
	q, rdb, _ := newClockedQueue(t)
	ctx := context.Background()

	a, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue A: %v", err)
	}
	b, err := q.Enqueue(ctx, 1, "bob", "dev2")
	if err != nil {
		t.Fatalf("enqueue B: %v", err)
	}
	// 入队分数是墙钟毫秒,同一毫秒内按 queueId 字典序:把 A 的分数改成 0,确定地排到队首。
	if err := rdb.ZAdd(ctx, zoneZSetKey(1), redis.Z{Score: 0, Member: a.QueueID}).Err(); err != nil {
		t.Fatalf("move A to the head: %v", err)
	}
	if err := rdb.Set(ctx, metaKey(a.QueueID), "x", 0).Err(); err != nil {
		t.Fatalf("corrupt A's meta: %v", err)
	}

	before := expiredTotalValue(t, "1", ExpireReasonMetaGone)
	if st, err := q.Lookup(ctx, a.QueueToken, 1, signMustNotRun(t)); err != nil || st.Status != StatusExpired {
		t.Fatalf("lookup A with a non-hash meta = (%+v, %v), want (EXPIRED, nil)", st, err)
	}
	if got := expiredTotalValue(t, "1", ExpireReasonMetaGone) - before; got != 1 {
		t.Fatalf(`login_queue_expired_total{zone="1",reason="meta_gone"} grew by %v, want 1`, got)
	}
	if n, err := q.PopAdmit(ctx, 1, 2, pickZone1Gate); err != nil || n != 1 {
		t.Fatalf("popadmit = (%d, %v), want (1, nil): A removed as META_GONE, B admitted", n, err)
	}
	if n, err := rdb.ZCard(ctx, zoneZSetKey(1)).Result(); err != nil || n != 0 {
		t.Fatalf("queue length = %d err=%v, want 0", n, err)
	}
	if n, err := rdb.Exists(ctx, admitKey(a.QueueID)).Result(); err != nil || n != 0 {
		t.Fatalf("A's admit slot exists=%d err=%v, want none", n, err)
	}
	if typ, err := rdb.Type(ctx, metaKey(a.QueueID)).Result(); err != nil || typ != "string" {
		t.Fatalf("type of A's meta = %q err=%v, want string (left untouched)", typ, err)
	}
	if st, err := q.Lookup(ctx, b.QueueToken, 1, stubSignFn); err != nil || st.Status != StatusAdmitted {
		t.Fatalf("lookup B = (%+v, %v), want ADMITTED", st, err)
	}
}

// N5(2026-10-18):队首的空串 member 交给脚本,由 META_GONE 分支 ZREM 掉,不写放行槽。钉住「ZRANGE 之下不能对空串
// continue 跳过」:那样它会永久卡在队首、每批占一个位置。修复前也绿(ZPOPMIN 会把它弹掉)。
func TestPopAdmit_EmptyMemberAtHead_Removed(t *testing.T) {
	q, rdb, _ := newClockedQueue(t)
	ctx := context.Background()

	if err := rdb.ZAdd(ctx, zoneZSetKey(1), redis.Z{Score: 0, Member: ""}).Err(); err != nil {
		t.Fatalf("seed an empty member: %v", err)
	}
	b, err := q.Enqueue(ctx, 1, "bob", "dev2")
	if err != nil {
		t.Fatalf("enqueue B: %v", err)
	}
	if n, err := q.PopAdmit(ctx, 1, 2, pickZone1Gate); err != nil || n != 1 {
		t.Fatalf("popadmit = (%d, %v), want (1, nil)", n, err)
	}
	if n, err := rdb.ZCard(ctx, zoneZSetKey(1)).Result(); err != nil || n != 0 {
		t.Fatalf("queue length = %d err=%v, want 0 (the empty member is removed)", n, err)
	}
	if n, err := rdb.Exists(ctx, admitKey("")).Result(); err != nil || n != 0 {
		t.Fatalf("admit slot for the empty member exists=%d err=%v, want none", n, err)
	}
	if st, err := q.Lookup(ctx, b.QueueToken, 1, stubSignFn); err != nil || st.Status != StatusAdmitted {
		t.Fatalf("lookup B = (%+v, %v), want ADMITTED", st, err)
	}
}

// N6(D2 回归,不变量 6,2026-10-18):持续轮询的条目不因等待时长过期。每 E/2 轮询一次、共拨 2h(E = 1h),三人每次
// 都回 QUEUEING、名次 {0,1,2};之后照常放行。
// 修复前红:第 2 轮(60min)时 meta 与 token 索引已按入队时的 E 过期,回 EXPIRED。
func TestLookup_ContinuousPollingOutlivesEntryTTL(t *testing.T) {
	q, _, mr, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	tokens := enqueueAccounts(t, q, "alice", "bob", "carol")
	all := slices.Collect(maps.Values(tokens))
	for round := 1; round <= 4; round++ {
		mr.FastForward(q.entryTTL / 2)
		when := fmt.Sprintf("round %d, %v after enqueue", round, time.Duration(round)*q.entryTTL/2)
		if ranks := lookupQueueingRanks(t, q, all, 3, when); !slices.Equal(ranks, []uint32{0, 1, 2}) {
			t.Fatalf("ranks (%s) = %v, want [0 1 2]", when, ranks)
		}
	}
	if n, err := q.PopAdmit(ctx, 1, 3, pickZone1Gate); err != nil || n != 3 {
		t.Fatalf("popadmit after 2h of polling = (%d, %v), want (3, nil)", n, err)
	}
	for queueID, token := range tokens {
		if st, err := q.Lookup(ctx, token, 1, stubSignFn); err != nil || st.Status != StatusAdmitted {
			t.Fatalf("lookup %s = (%+v, %v), want ADMITTED", queueID, st, err)
		}
	}
}

// N7(2026-10-18):续期是滞后的,只在剩余 PTTL < E 时续到 1.25E;meta、token 索引、队列键三把键逐一核对。
// 修复前红在第一处(不续期,PTTL = 60min−1s)。「每次轮询都续」的写法红在第二处;阈值写成 <= 的写法红在第三处。
func TestLookup_RenewalIsLazy_OnlyBelowEntryTTL(t *testing.T) {
	q, rdb, mr, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	e, renew := q.entryTTL, q.entryRenewTTL()
	if renew != 75*time.Minute {
		t.Fatalf("entryRenewTTL = %v, want 75m0s (1.25 × QueueEntryTTL)", renew)
	}
	keys := []string{metaKey(res.QueueID), tokenIndexKey(res.QueueToken), zoneZSetKey(1)}
	poll := func(want time.Duration, when string) {
		t.Helper()
		if st, err := q.Lookup(ctx, res.QueueToken, 1, signMustNotRun(t)); err != nil || st.Status != StatusQueueing {
			t.Fatalf("lookup (%s) = (%+v, %v), want QUEUEING", when, st, err)
		}
		for _, key := range keys {
			mustKeyPTTL(t, rdb, key, want, when)
		}
	}

	mr.FastForward(time.Second)
	poll(renew, "1s after enqueue: PTTL below E, renewed to 1.25E")
	mr.FastForward(time.Second)
	poll(renew-time.Second, "1s later: PTTL still at least E, not renewed")
	mr.FastForward(15*time.Minute - time.Second)
	poll(e, "PTTL exactly E: the threshold is strict, not renewed")
	mr.FastForward(time.Millisecond)
	poll(renew, "1ms below E: renewed to 1.25E")
}

// N8(2026-10-18):空闲超过空闲 TTL 的条目过期,持续轮询的同伴不受影响。A 只在开头轮询一次(续到 75min),B 每 30min
// 轮询一次;拨到 A 自上次续期起空闲满 75min 时,A 回 EXPIRED 并按令牌里的 zone 记 meta_gone,B 仍在排队,随后照常放行
// (A 的成员由 META_GONE 清掉)。
// 修复前红:B 在 60min 时已按入队时的 E 过期;A 的计数标签是 unknown。
func TestLookup_IdleBeyondIdleTTL_Expires(t *testing.T) {
	gzprom.Enable() // 见 TestLookup_AdmitSlotExpiredUnclaimed_ReturnsExpired
	q, rdb, mr, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	a, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue A: %v", err)
	}
	b, err := q.Enqueue(ctx, 1, "bob", "dev2")
	if err != nil {
		t.Fatalf("enqueue B: %v", err)
	}
	mustQueueing := func(res *EnqueueResult, when string) *QueueState {
		t.Helper()
		st, err := q.Lookup(ctx, res.QueueToken, 1, signMustNotRun(t))
		if err != nil || st.Status != StatusQueueing {
			t.Fatalf("lookup %s (%s) = (%+v, %v), want QUEUEING", res.QueueID, when, st, err)
		}
		return st
	}
	mr.FastForward(time.Second)
	mustQueueing(a, "1s")
	mustQueueing(b, "1s")
	mr.FastForward(30 * time.Minute)
	mustQueueing(b, "30min later")
	mr.FastForward(30 * time.Minute)
	mustQueueing(b, "60min later")
	mr.FastForward(15 * time.Minute) // A 自上次续期起已空闲 75min

	before := expiredTotalValue(t, "1", ExpireReasonMetaGone)
	if st, err := q.Lookup(ctx, a.QueueToken, 1, signMustNotRun(t)); err != nil || st.Status != StatusExpired {
		t.Fatalf("lookup A idle for 1.25E = (%+v, %v), want EXPIRED", st, err)
	}
	if got := expiredTotalValue(t, "1", ExpireReasonMetaGone) - before; got != 1 {
		t.Fatalf(`login_queue_expired_total{zone="1",reason="meta_gone"} grew by %v, want 1 (zone taken from the token)`, got)
	}
	if st := mustQueueing(b, "after A expired"); st.Total != 2 {
		t.Fatalf("B's total = %d, want 2 (A's member is still queued until it is popped)", st.Total)
	}
	if n, err := q.PopAdmit(ctx, 1, 2, pickZone1Gate); err != nil || n != 1 {
		t.Fatalf("popadmit = (%d, %v), want (1, nil): A removed as META_GONE, B admitted", n, err)
	}
	if n, err := rdb.ZCard(ctx, zoneZSetKey(1)).Result(); err != nil || n != 0 {
		t.Fatalf("queue length = %d err=%v, want 0", n, err)
	}
	if st, err := q.Lookup(ctx, b.QueueToken, 1, stubSignFn); err != nil || st.Status != StatusAdmitted {
		t.Fatalf("lookup B = (%+v, %v), want ADMITTED", st, err)
	}
}

// N9(2026-10-18):放行之后不再续期。排队中的那次轮询续到 75min,拨 20min 后放行;之后取走(ADMITTED)与再查
// (EXPIRED,admit_window_passed)都不动 meta 与 token 索引的 TTL,两者仍是 55min。
// 修复前红在「续到 75min」那步(旧代码不续期)。「先续期再判 admitted_at」的写法红在最后的 PTTL 断言上。
func TestLookup_NoRenewalAfterAdmit(t *testing.T) {
	gzprom.Enable() // 见 TestLookup_AdmitSlotExpiredUnclaimed_ReturnsExpired
	q, rdb, mr, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	mr.FastForward(time.Second)
	if st, err := q.Lookup(ctx, res.QueueToken, 1, signMustNotRun(t)); err != nil || st.Status != StatusQueueing {
		t.Fatalf("lookup while queueing = (%+v, %v), want QUEUEING", st, err)
	}
	mustKeyPTTL(t, rdb, metaKey(res.QueueID), q.entryRenewTTL(), "renewed while queueing")
	mr.FastForward(20 * time.Minute)
	if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err != nil || n != 1 {
		t.Fatalf("popadmit = (%d, %v), want (1, nil)", n, err)
	}
	if st, err := q.Lookup(ctx, res.QueueToken, 1, stubSignFn); err != nil || st.Status != StatusAdmitted {
		t.Fatalf("first lookup after admit = (%+v, %v), want ADMITTED", st, err)
	}
	before := expiredTotalValue(t, "1", ExpireReasonAdmitWindowPassed)
	if st, err := q.Lookup(ctx, res.QueueToken, 1, signMustNotRun(t)); err != nil || st.Status != StatusExpired {
		t.Fatalf("second lookup after admit = (%+v, %v), want EXPIRED", st, err)
	}
	if got := expiredTotalValue(t, "1", ExpireReasonAdmitWindowPassed) - before; got != 1 {
		t.Fatalf(`login_queue_expired_total{zone="1",reason="admit_window_passed"} grew by %v, want 1`, got)
	}
	want := q.entryRenewTTL() - 20*time.Minute
	mustKeyPTTL(t, rdb, metaKey(res.QueueID), want, "no renewal after admit")
	mustKeyPTTL(t, rdb, tokenIndexKey(res.QueueToken), want, "no renewal after admit")
}

// N10(2026-10-18):Lookup 的 zone 取自令牌。与 meta 的 zone_id 不符时回 EXPIRED(meta_gone,按传入的 zone 记),不续期、
// 不动队列;zone 0 视为损坏,直接回 EXPIRED、不碰 Redis(用一个连着已关闭 miniredis 的 Queue 证明)。
// 修复前红:旧 Lookup 没有 zone 参数,编不过。
func TestLookup_ZoneMismatchOrZero_ReturnsMetaGone(t *testing.T) {
	gzprom.Enable() // 见 TestLookup_AdmitSlotExpiredUnclaimed_ReturnsExpired
	q, rdb, mr, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	mr.FastForward(time.Second)
	before := expiredTotalValue(t, "2", ExpireReasonMetaGone)
	if st, err := q.Lookup(ctx, res.QueueToken, 2, signMustNotRun(t)); err != nil || st.Status != StatusExpired {
		t.Fatalf("lookup with zone 2 for a zone-1 entry = (%+v, %v), want EXPIRED", st, err)
	}
	if got := expiredTotalValue(t, "2", ExpireReasonMetaGone) - before; got != 1 {
		t.Fatalf(`login_queue_expired_total{zone="2",reason="meta_gone"} grew by %v, want 1`, got)
	}
	mustKeyPTTL(t, rdb, metaKey(res.QueueID), q.entryTTL-time.Second, "zone mismatch must not renew")
	if n, err := rdb.ZCard(ctx, zoneZSetKey(1)).Result(); err != nil || n != 1 {
		t.Fatalf("zone 1 queue length = %d err=%v, want 1 (untouched)", n, err)
	}
	if st, err := q.Lookup(ctx, res.QueueToken, 1, signMustNotRun(t)); err != nil || st.Status != StatusQueueing {
		t.Fatalf("lookup with the right zone = (%+v, %v), want QUEUEING", st, err)
	}

	deadMR, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	// 关命令重试与拨号重试(同 TestAdmittedAccounting_RedisDown_FailsClosed):万一碰了 Redis,也很快报错而不是久等。
	deadRdb := redis.NewClient(&redis.Options{
		Addr:               deadMR.Addr(),
		MaxRetries:         -1,
		DialTimeout:        200 * time.Millisecond,
		DialerRetries:      1,
		DialerRetryTimeout: time.Millisecond,
	})
	defer deadRdb.Close()
	deadMR.Close()
	dead := New(deadRdb, time.Hour, 60*time.Second, []byte("test-secret"))
	if st, err := dead.Lookup(ctx, res.QueueToken, 0, signMustNotRun(t)); err != nil || st.Status != StatusExpired {
		t.Fatalf("lookup with zone 0 = (%+v, %v), want (EXPIRED, nil) without touching Redis", st, err)
	}
}

// N11(2026-10-18):QueueEntryTTL 不合法时 Lookup 先回错误、不碰 Redis(纵深防御:绕过起服装配直接 New 时,续期会写出
// PEXPIRE 0、当场删键)。用合法的 Queue 入队,再拿同一个 rdb 建一个 entryTTL=0 的 q0 来查。
// 修复前红:旧代码回 QUEUEING、nil。
func TestLookup_InvalidEntryTTL_FailsWithoutTouchingRedis(t *testing.T) {
	q, rdb, mr, cleanup := newTestQueue(t)
	defer cleanup()
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	mr.FastForward(time.Second)
	q0 := New(rdb, 0, 60*time.Second, []byte("test-secret"))
	cmds := 0
	rdb.AddHook(cmdHook{process: func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		cmds++
		return next(ctx, cmd)
	}})
	st, err := q0.Lookup(ctx, res.QueueToken, 1, signMustNotRun(t))
	if err == nil || !strings.Contains(err.Error(), "ttl must be positive") {
		t.Fatalf("lookup with QueueEntryTTL=0 = (%+v, %v), want a ttl validation error", st, err)
	}
	if cmds != 0 {
		t.Fatalf("lookup with an invalid ttl sent %d Redis commands, want 0", cmds)
	}
	mustKeyPTTL(t, rdb, metaKey(res.QueueID), q.entryTTL-time.Second, "invalid ttl must not touch the meta")
	mustKeyPTTL(t, rdb, tokenIndexKey(res.QueueToken), q.entryTTL-time.Second, "invalid ttl must not touch the token index")
}

// N12(2026-10-18):排队令牌按 queueTokenMaxAge(24h)签,不再等于 QueueEntryTTL。在包内手工解码令牌体
// (base64 RawURL,最后一个 '.' 之前是 JSON),ExpireTS 落在 [入队前 + 24h, 入队后 + 24h](秒)。
// 修复前红:ExpireTS = now + 1h。
func TestEnqueue_QueueTokenExpiresAtMaxAge(t *testing.T) {
	q, _, _, cleanup := newTestQueue(t)
	defer cleanup()
	if queueTokenMaxAge != 24*time.Hour {
		t.Fatalf("queueTokenMaxAge = %v, want 24h0m0s (keep ValidateTTLs and the design doc §6 in sync)", queueTokenMaxAge)
	}

	before := time.Now().Unix()
	res, err := q.Enqueue(context.Background(), 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	after := time.Now().Unix()

	raw, err := base64.RawURLEncoding.DecodeString(res.QueueToken)
	if err != nil {
		t.Fatalf("decode queue token: %v", err)
	}
	dot := bytes.LastIndexByte(raw, '.')
	if dot <= 0 {
		t.Fatalf("queue token %q has no body.signature separator", raw)
	}
	var body queueTokenBody
	if err := json.Unmarshal(raw[:dot], &body); err != nil {
		t.Fatalf("unmarshal queue token body: %v", err)
	}
	maxAge := int64(queueTokenMaxAge / time.Second)
	if body.ExpireTS < before+maxAge || body.ExpireTS > after+maxAge {
		t.Fatalf("queue token ExpireTS = %d, want within [%d, %d] (enqueue time + 24h)", body.ExpireTS, before+maxAge, after+maxAge)
	}
}

// N13(2026-10-18):Enqueue 给队列键的 TTL 只增不减(ExpireNX + ExpireGT)。QueueEntryTTL 小的副本入队不能把大值
// 副本(或读状态脚本)续上的 TTL 压短;剩余不足自己的 E 时照常延长;键没有 TTL 或刚被删掉时一定设上 TTL。
// 修复前红:small 入队把 PTTL 压成 1h。只用 GT 的写法在 PERSIST / 删键两种形状上红(PTTL == -1)。
func TestEnqueue_QueueKeyTTLNeverShortened(t *testing.T) {
	small, rdb, mr, cleanup := newTestQueue(t) // QueueEntryTTL 1h
	defer cleanup()
	big := New(rdb, 2*time.Hour, 60*time.Second, []byte("test-secret"))
	ctx := context.Background()
	key := zoneZSetKey(1)
	enqueue := func(q *Queue) {
		t.Helper()
		if _, err := q.Enqueue(ctx, 1, "u", "d"); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	mustNoTTL := func(when string) {
		t.Helper()
		// go-redis 把 PTTL 的 -1(无 TTL)/ -2(键不存在)原样放进 Duration,两者都 < 0。
		if ttl, err := rdb.PTTL(ctx, key).Result(); err != nil || ttl >= 0 {
			t.Fatalf("PTTL %s (%s) = %v err=%v, want no TTL", key, when, ttl, err)
		}
	}

	enqueue(big)
	mustKeyPTTL(t, rdb, key, 2*time.Hour, "larger QueueEntryTTL enqueues first")
	enqueue(small)
	mustKeyPTTL(t, rdb, key, 2*time.Hour, "smaller QueueEntryTTL must not shorten it")
	mr.FastForward(90 * time.Minute)
	mustKeyPTTL(t, rdb, key, 30*time.Minute, "fast-forwarded 90min")
	enqueue(small)
	mustKeyPTTL(t, rdb, key, time.Hour, "remaining below the smaller E: extended (GT)")

	if err := rdb.Persist(ctx, key).Err(); err != nil {
		t.Fatalf("persist: %v", err)
	}
	mustNoTTL("after PERSIST")
	enqueue(small)
	mustKeyPTTL(t, rdb, key, time.Hour, "key without a TTL gets one (NX)")

	if err := rdb.Del(ctx, key).Err(); err != nil {
		t.Fatalf("del: %v", err)
	}
	mustNoTTL("after DEL")
	enqueue(small)
	mustKeyPTTL(t, rdb, key, time.Hour, "re-created key gets a TTL (NX)")
}

// replayAdmitScriptHook 模拟 go-redis 对单条命令的自动重发:第一次发给 admit:{queueID} 的脚本(EVAL / EVALSHA)
// 执行成功之后,先跑 between(回包丢失期间发生的事),再把同一条命令原样再发一次,以第二次的结果作为这条命令的结果。
// go-redis 对单条命令在读超时、连接断开后会重发(baseClient.process),对 MULTI 读失败不重发(txPipelineProcessCmds)。
type replayAdmitScriptHook struct {
	admitKey string
	between  func()
	replays  *int
}

func (h replayAdmitScriptHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h replayAdmitScriptHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := next(ctx, cmd); err != nil || *h.replays > 0 {
			return err
		}
		if name := cmd.Name(); name != "eval" && name != "evalsha" {
			return nil
		}
		// EVAL / EVALSHA 的参数:命令名、脚本或 sha、键数、键...
		if args := cmd.Args(); len(args) < 4 || args[3] != h.admitKey {
			return nil
		}
		*h.replays++
		h.between()
		return next(ctx, cmd)
	}
}

func (h replayAdmitScriptHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// 放行脚本被重发:第一次已执行、回包丢了,期间客户端取走了放行槽,随后 go-redis 重发同一次脚本。重发不能再写
// 一个放行槽(同一排队票会被签出第二张 gate 票),也不能把占位分数拨回「放行时刻 + AdmitTTL」;这次放行照常计数。
// 修复前(MULTI 写法)红在钩子没有触发:go-redis 不重发 MULTI,这条风险是改成单条脚本之后才有的;
// 脚本去掉 admitted_at 守卫的写法红在「放行槽被重写」上(再往下,第二次 Lookup 会回 ADMITTED、签出第二张票)。
// (2026-10-18 补注)放行脚本改为「先判在队、写完再 ZREM」后,重发看到成员已不在队列、admitted_at 等于本次的放行时刻,
// 回 REPLAYED:什么都不写、照常计数(n == 1 因此保留)。KEYS[1] 仍是放行槽,钩子不用改;另加 ZCARD == 0。
// 去掉 REPLAYED 分支的写法红在 n == 1 上(回 0,GONE 不计数);去掉 ZSCORE / HEXISTS 守卫的写法红在「放行槽被重写」上。
func TestPopAdmit_AdmitScriptReplayedAfterConsume_DoesNotReissue(t *testing.T) {
	q, rdb, clk := newClockedQueue(t)
	ctx := context.Background()

	res, err := q.Enqueue(ctx, 1, "alice", "dev1")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	signs := 0
	countingSign := func(slot *AdmitSlot) (*AdmitToken, error) {
		signs++
		return stubSignFn(slot)
	}
	replays := 0
	var consumedAt time.Time
	rdb.AddHook(replayAdmitScriptHook{
		admitKey: admitKey(res.QueueID),
		replays:  &replays,
		between: func() {
			clk.Advance(2 * time.Second) // 客户端的下一次轮询
			consumedAt = clk.Now()
			if st, err := q.Lookup(ctx, res.QueueToken, 1, countingSign); err != nil || st.Status != StatusAdmitted {
				t.Fatalf("lookup between the two runs = (%+v, %v), want ADMITTED", st, err)
			}
		},
	})

	if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err != nil || n != 1 {
		t.Fatalf("popadmit = (%d, %v), want (1, nil): the replayed admit still counts once", n, err)
	}
	if replays != 1 {
		t.Fatalf("admit script replayed %d times, want 1: the retry window was not exercised", replays)
	}
	if n, err := rdb.ZCard(ctx, zoneZSetKey(1)).Result(); err != nil || n != 0 {
		t.Fatalf("queue length = %d err=%v after the replay, want 0", n, err)
	}
	if n, err := rdb.Exists(ctx, admitKey(res.QueueID)).Result(); err != nil || n != 0 {
		t.Fatalf("admit slot exists=%d err=%v after the replay, want none: the replay must not write a second slot", n, err)
	}
	score, err := rdb.ZScore(ctx, admittedZSetKey(1), res.QueueID).Result()
	if want := float64(consumedAt.Add(q.handoffGrace).UnixMilli()); err != nil || score != want {
		t.Fatalf("placeholder score = %.0f err=%v, want %.0f (consume time + handoff grace, not reset by the replay)",
			score, err, want)
	}
	if st, err := q.Lookup(ctx, res.QueueToken, 1, countingSign); err != nil || st.Status != StatusExpired {
		t.Fatalf("lookup after the replay = (%+v, %v), want EXPIRED, not a second ADMITTED", st, err)
	}
	if signs != 1 {
		t.Fatalf("signFn ran %d times, want exactly 1", signs)
	}
}

// admitTraffic 数放行脚本是怎么发出去的(2026-11-06):单发的条数,以及每个 pipeline 里有几条。只计数,不拦截。
type admitTraffic struct {
	single    int
	pipelines []int
}

func (a *admitTraffic) DialHook(next redis.DialHook) redis.DialHook { return next }

func (a *admitTraffic) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if _, ok := admitScriptKey(cmd); ok {
			a.single++
		}
		return next(ctx, cmd)
	}
}

func (a *admitTraffic) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		admits := 0
		for _, cmd := range cmds {
			if _, ok := admitScriptKey(cmd); ok {
				admits++
			}
		}
		if admits > 0 {
			a.pipelines = append(a.pipelines, admits)
		}
		return next(ctx, cmds)
	}
}

// enqueueN 往 zone 1 排 n 个账号,返回按队列顺序(ZRANGE)排好的 queueId。
func enqueueN(t *testing.T, q *Queue, rdb *redis.Client, n int) []string {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		if _, err := q.Enqueue(ctx, 1, fmt.Sprintf("user-%d", i), "dev"); err != nil {
			t.Fatalf("enqueue #%d: %v", i, err)
		}
	}
	order, err := rdb.ZRange(ctx, zoneZSetKey(1), 0, -1).Result()
	if err != nil || len(order) != n {
		t.Fatalf("queue order = %d members err=%v, want %d", len(order), err, n)
	}
	return order
}

// 放行按块流水线发(2026-11-06,缺口清单第 14 条):第一条单发探路,之后每块至多 popAdmitPipelineChunk 条放进一个 pipeline。
// 1 + 128 + 128 + 43 条 = 1 条单发 + 三个 pipeline;全部放行、队列清空、每人一个在途占位。
// 逐条单发的写法红在 pipeline 数上,不探路直接整批分块的写法红在单发数上。先 SCRIPT LOAD,免得探路那条撞 NOSCRIPT 多一次 EVAL。
func TestPopAdmit_ProbeThenPipelinedChunks(t *testing.T) {
	q, rdb, _ := newClockedQueue(t)
	ctx := context.Background()
	const total = 1 + 2*popAdmitPipelineChunk + 43
	enqueueN(t, q, rdb, total)
	if err := admitEntryScript.Load(ctx, rdb).Err(); err != nil {
		t.Fatalf("script load: %v", err)
	}
	traffic := &admitTraffic{}
	rdb.AddHook(traffic)

	if n, err := q.PopAdmit(ctx, 1, total, pickZone1Gate); err != nil || n != total {
		t.Fatalf("popadmit = (%d, %v), want (%d, nil)", n, err, total)
	}
	if traffic.single != 1 {
		t.Fatalf("admit scripts sent alone = %d, want 1 (the probe)", traffic.single)
	}
	if want := []int{popAdmitPipelineChunk, popAdmitPipelineChunk, 43}; !slices.Equal(traffic.pipelines, want) {
		t.Fatalf("admit scripts per pipeline = %v, want %v", traffic.pipelines, want)
	}
	if n, err := rdb.ZCard(ctx, zoneZSetKey(1)).Result(); err != nil || n != 0 {
		t.Fatalf("queue length = %d err=%v, want 0", n, err)
	}
	mustAdmittedCount(t, q, total, "every admitted entry holds a placeholder")
}

// 一块里有一条脚本出错(2026-11-06):同块其余各条照常执行并计数,出错那条原样留在队列,后面的块不再发,错误带上首个出错的
// queueId、已放行数、出错数与未尝试数。1 条探路 + 一整块 + 10 条,让第一块里的一条出错:放行 1 + 127 条,剩出错那条与未尝试的 10 条。
// 把「出错即停」做进块内(同块出错之后的条目当没执行)或者不停批(后面的块照发)的写法都红在这里。
func TestPopAdmit_ChunkErrorStopsAfterTheChunk(t *testing.T) {
	q, rdb, _ := newClockedQueue(t)
	ctx := context.Background()
	const untried = 10
	order := enqueueN(t, q, rdb, 1+popAdmitPipelineChunk+untried)
	failing := order[6] // 第一块(队列下标 1..128)里的一条
	rdb.AddHook(cmdHook{process: func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		if key, ok := admitScriptKey(cmd); ok && key == admitKey(failing) {
			return failCmd(cmd, errors.New("ERR injected"))
		}
		return next(ctx, cmd)
	}})

	n, err := q.PopAdmit(ctx, 1, len(order), pickZone1Gate)
	const wantAdmitted = popAdmitPipelineChunk // 探路 1 条 + 第一块 128 条里的 127 条
	if n != wantAdmitted || err == nil {
		t.Fatalf("popadmit = (%d, %v), want (%d, an error)", n, err, wantAdmitted)
	}
	for _, part := range []string{
		"ERR injected", "queueId=" + failing, fmt.Sprintf("admitted=%d", wantAdmitted), "failed=1", fmt.Sprintf("untried=%d", untried),
	} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("popadmit error %q, want it to mention %q", err, part)
		}
	}
	left, err := rdb.ZRange(ctx, zoneZSetKey(1), 0, -1).Result()
	if err != nil {
		t.Fatalf("zrange after the failing chunk: %v", err)
	}
	want := append([]string{failing}, order[1+popAdmitPipelineChunk:]...)
	if !slices.Equal(left, want) {
		t.Fatalf("queue after the failing chunk = %v, want the failed entry followed by the untried ones %v", left, want)
	}
	if has, err := rdb.HExists(ctx, metaKey(failing), metaFieldAdmittedAt).Result(); err != nil || has {
		t.Fatalf("meta of the failed entry has %s=%v err=%v, want absent (its script never ran)", metaFieldAdmittedAt, has, err)
	}
}

// AdmitTTL 配成 0(显式写 0s 不回落默认值)时 PopAdmit 一个都不弹出、回错误,条目原样留在队列,不写放行槽。
// 修复前红在「回错误」上:回 (1, nil),写出一个不带 TTL、永不过期的放行槽(go-redis 的 Set(..., 0) 不设过期)。
// 只改放行脚本、不加这道拦截的写法也红,红在回 (0, nil) 上:SET PX 0 报错,条目已弹出、丢失(队列长度 0)。
// (2026-10-15 补注)加了 500µs 一组:放行脚本以 AdmitTTL.Milliseconds() 作 SET PX,不足 1ms 取整为 0,
// miniredis 与 Redis 一样拒绝 SET PX 0。ValidateTTLs 只拒 <=0 的写法红在这一组的「回错误」上(回 (0, nil):
// 条目已弹出、脚本报错、丢失);HEAD 同样红在这里(回 (1, nil),go-redis 的 Set 把 500µs 截成 PX 1)。
// (2026-10-18 更正)上文「一个都不弹出」「条目已弹出、丢失」改读「一个都不放行」:PopAdmit 不再 ZPOPMIN,放行脚本出错时
// 条目本来就留在队列;这道拦截如今只是让一个都不放行、错误在 ZRANGE 之前就给出。断言不变,用例名沿用。
func TestPopAdmit_NonPositiveAdmitTTL_PopsNothing(t *testing.T) {
	for _, admitTTL := range []time.Duration{0, 500 * time.Microsecond} {
		t.Run(admitTTL.String(), func(t *testing.T) {
			_, rdb, _, cleanup := newTestQueue(t)
			defer cleanup()
			q := New(rdb, time.Hour, admitTTL, []byte("test-secret"))
			ctx := context.Background()

			res, err := q.Enqueue(ctx, 1, "alice", "dev1")
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			if n, err := q.PopAdmit(ctx, 1, 1, pickZone1Gate); err == nil || n != 0 {
				t.Fatalf("popadmit with AdmitTTL=%v = (%d, %v), want (0, error)", admitTTL, n, err)
			}
			if n, err := rdb.ZCard(ctx, zoneZSetKey(1)).Result(); err != nil || n != 1 {
				t.Fatalf("queue length = %d err=%v, want 1 (the entry stays queued)", n, err)
			}
			if n, err := rdb.Exists(ctx, admitKey(res.QueueID)).Result(); err != nil || n != 0 {
				t.Fatalf("admit slot exists=%d err=%v, want none", n, err)
			}
		})
	}
}

// AdmitTTL、QueueEntryTTL 非正时排队照样能起,但放行槽写不进(SET PX 报错)或写成永不过期、EXPIRE ≤0 当场删掉
// 整个队列、token 索引不带 TTL。起服装配据 ValidateTTLs 拒绝。不要求 admitTTL < entryTTL(最后一组钉住)。
// 修复前没有这个函数(编不过即红)。
// (2026-10-15 补注)AdmitTTL 另有 1ms 下界(放行脚本的 SET PX 取整到毫秒,见 ValidateTTLs):500µs 一组在只拒 <=0
// 的写法下红,1ms 一组钉住下界本身可用。
// (2026-10-18 补注)QueueEntryTTL 收紧为 [1s, queueTokenMaxAge]:加了 500ms、1s、24h、24h+1s 四组,报错串仍须含
// "ttl must be positive"。修复前红在 500ms 与 24h+1s 两组(被接受)。
func TestValidateTTLs_RejectsNonPositive(t *testing.T) {
	cases := []struct {
		entryTTL, admitTTL time.Duration
		wantErr            bool
	}{
		{0, 60 * time.Second, true},
		{-time.Second, 60 * time.Second, true},
		{time.Hour, 0, true},
		{time.Hour, -time.Second, true},
		{time.Hour, 60 * time.Second, false},
		{time.Hour, 2 * time.Hour, false},
		{time.Hour, 500 * time.Microsecond, true},
		{time.Hour, time.Millisecond, false},
		{500 * time.Millisecond, 60 * time.Second, true},
		{time.Second, 60 * time.Second, false},
		{queueTokenMaxAge, 60 * time.Second, false},
		{queueTokenMaxAge + time.Second, 60 * time.Second, true},
	}
	for _, c := range cases {
		err := ValidateTTLs(c.entryTTL, c.admitTTL)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateTTLs(entry=%v, admit=%v) = %v, want error=%v", c.entryTTL, c.admitTTL, err, c.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), "ttl must be positive") {
			t.Errorf("ValidateTTLs(entry=%v, admit=%v) error = %q, want it to say the ttl must be positive", c.entryTTL, c.admitTTL, err)
		}
	}
}

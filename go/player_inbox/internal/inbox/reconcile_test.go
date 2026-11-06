package inbox

// 对账(设计 §9.3)的纯逻辑单测:两个入口共用的判定谓词 finalizedLedgerGap(评审 M6)、账本可用性、
// 定时对账的分片收集(评审 M5:轮转起点、键集翻页、单片 / 玩家数截断)、有界并发读账本与一轮的组装。
// SQL 一侧(ORDER BY + 键集游标)只有 store_integration_test.go 覆盖。
//
// 账本一律按真实形状构造:消费方每次裁决都入环、水位 = 最后一次裁决的 seq(C++ RecordDecision),
// 所以「有水位、空环」「水位越过环尾」这类生产里不可能出现的账本不在这里出现。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"player_inbox/internal/constants"

	componentpb "proto/common/component"
	inboxpb "proto/player_inbox"

	"github.com/zeromicro/go-zero/core/logx/logtest"
)

const (
	byConsumer = inboxpb.PlayerInboxFinalizer_PLAYER_INBOX_FINALIZER_CONSUMER
	byAdmin    = inboxpb.PlayerInboxFinalizer_PLAYER_INBOX_FINALIZER_ADMIN
)

// rcDecision 是环里的一条结局(类型 GM 封锁,同 rcRow / rcRef)。
func rcDecision(seq, requestID uint64, outcome inboxpb.PlayerInboxOutcome, reason uint32) *componentpb.PlayerInboxDecision {
	return &componentpb.PlayerInboxDecision{Seq: seq, Outcome: uint32(outcome), Reason: reason, RequestId: requestID, MsgType: uint32(gmBlock)}
}

// rcCursor 是真实形状的游标:水位 = 环尾的 seq(环为空时为 0,即从未裁决过)。
func rcCursor(domain inboxpb.PlayerInboxDomain, recent ...*componentpb.PlayerInboxDecision) *componentpb.PlayerInboxDomainCursor {
	c := &componentpb.PlayerInboxDomainCursor{Domain: uint32(domain), Recent: recent}
	if len(recent) > 0 {
		c.DecidedThroughSeq = recent[len(recent)-1].GetSeq()
	}
	return c
}

func rcLedger(cursors ...*componentpb.PlayerInboxDomainCursor) *componentpb.PlayerInboxLedgerComp {
	return &componentpb.PlayerInboxLedgerComp{Cursors: cursors}
}

// rcRing 是从 first 起按 seq 连续、共 n 条的环(跳过 skip;skip=0 不跳),request_id = seq,结局 APPLIED。
func rcRing(first uint64, n int, skip uint64) *componentpb.PlayerInboxDomainCursor {
	var ds []*componentpb.PlayerInboxDecision
	for seq := first; len(ds) < n; seq++ {
		if seq == skip {
			continue
		}
		ds = append(ds, rcDecision(seq, seq, outApplied, 0))
	}
	return rcCursor(gmDomain, ds...)
}

// rcRef 是一条终态行的对账视图(类型 GM 封锁,原因码 0)。
func rcRef(seq, requestID uint64, state inboxpb.PlayerInboxItemState, by inboxpb.PlayerInboxFinalizer) FinalizedRef {
	return FinalizedRef{
		PlayerID: 42, Domain: gmDomain, Seq: seq, State: state, RequestID: requestID, MsgType: gmBlock, FinalizedBy: by,
	}
}

// rcRow 是 rcRef 的整行形状(ReconcilePlayer 读的是整行)。
func rcRow(seq, requestID uint64, state inboxpb.PlayerInboxItemState, by inboxpb.PlayerInboxFinalizer) *inboxpb.PlayerInboxItemRow {
	return &inboxpb.PlayerInboxItemRow{
		PlayerId: 42, Domain: uint32(gmDomain), Seq: seq, RequestId: requestID, MsgType: uint32(gmBlock),
		State: uint32(state), FinalizedBy: uint32(by),
	}
}

// consumerRulesHeader 是 C++ 消费方规则头文件,相对本包目录(go/player_inbox/internal/inbox,go test 的工作目录)。
var consumerRulesHeader = filepath.Join("..", "..", "..", "..",
	"cpp", "libs", "services", "scene", "player", "system", "player_inbox_rules.h")

// cppRecentCapacityDecl 匹配 player_inbox_rules.h 里环容量的定义(`constexpr int kRecentCapacity = 32;`)。
var cppRecentCapacityDecl = regexp.MustCompile(`constexpr\s+int\s+kRecentCapacity\s*=\s*(\d+)\s*;`)

// TestLedgerRecentCapacityMatchesConsumer 钉住环容量与消费方一致(评审 R4):从 C++ player_inbox_rules.h 里读出
// kRecentCapacity,与 ledgerRecentCapacity 逐值比对(Java 版落码时同值)。只改一边,空洞判定的「环满」边界就与
// 消费方的裁剪对不上,validLedger 还会把消费方合法的满环判成不合法 —— 对账整体失效。
// 头文件读不到、定义改了写法都判红:这条守卫不能静默失效。
func TestLedgerRecentCapacityMatchesConsumer(t *testing.T) {
	src, err := os.ReadFile(consumerRulesHeader)
	if err != nil {
		t.Fatalf("读 C++ 消费方规则头文件 %s 失败(移动了文件就同步改这里): %v", consumerRulesHeader, err)
	}
	decls := cppRecentCapacityDecl.FindAllSubmatch(src, -1)
	if len(decls) != 1 {
		t.Fatalf("%s 里应恰有一处 `constexpr int kRecentCapacity = N;`,找到 %d 处(改了写法就同步改这里的正则)",
			consumerRulesHeader, len(decls))
	}
	cpp, err := strconv.Atoi(string(decls[0][1]))
	if err != nil {
		t.Fatal(err)
	}
	if ledgerRecentCapacity != cpp {
		t.Fatalf("ledgerRecentCapacity = %d, C++ kRecentCapacity = %d:两边必须同值", ledgerRecentCapacity, cpp)
	}
	// 同 C++ 的 static_assert:环至少装得下两批(一批在途确认、一批刚落盘,设计 §4.4)。
	if 2*constants.FetchMaxItems > ledgerRecentCapacity {
		t.Fatalf("环容量 %d 装不下两批 Fetch(每批 %d)", ledgerRecentCapacity, constants.FetchMaxItems)
	}
	if c := rcRing(100, ledgerRecentCapacity, 0); len(c.GetRecent()) != ledgerRecentCapacity ||
		c.GetDecidedThroughSeq() != uint64(100+ledgerRecentCapacity-1) {
		t.Fatalf("rcRing 构造的满环不对: len=%d dt=%d", len(c.GetRecent()), c.GetDecidedThroughSeq())
	}
}

// TestFinalizedLedgerGap 钉住两个对账入口共用的谓词(评审 M6)。回归目标:原实现只比水位,
// 「seq=a 已 APPLIED、Redis 丢尾后水位回到 a 之前、之后 b(>a) 一执行水位又越过 a」被完全掩盖。
func TestFinalizedLedgerGap(t *testing.T) {
	short := rcCursor(gmDomain, rcDecision(20, 2, outApplied, 0)) // 环只有 1 条:从没裁剪过
	full := rcRing(100, ledgerRecentCapacity, 120)                // 满环 100..132,中间缺 120
	if len(full.GetRecent()) != ledgerRecentCapacity || full.GetDecidedThroughSeq() != 132 {
		t.Fatalf("满环构造不对: len=%d dt=%d", len(full.GetRecent()), full.GetDecidedThroughSeq())
	}
	rejected := rcCursor(gmDomain, rcDecision(30, 3, outRejected, 7))
	skipped := rcCursor(gmDomain,
		rcDecision(40, 4, outSkipped, uint32(inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_QUARANTINED)))
	otherType := rcDecision(50, 5, outApplied, 0)
	otherType.MsgType = uint32(gmUnblock)
	typed := rcCursor(gmDomain, otherType)

	rejectedRef := func(reason uint32) FinalizedRef {
		f := rcRef(30, 3, stateRejected, byConsumer)
		f.OutcomeReason = reason
		return f
	}
	cases := []struct {
		name   string
		ref    FinalizedRef
		cursor *componentpb.PlayerInboxDomainCursor
		want   ledgerGap
	}{
		// 回归:原实现(只比水位)对下面这几条一律判「无差异」。
		{"首次处置就丢、被之后的条目掩盖(环不满,早于环头也要在环里)", rcRef(10, 1, stateApplied, byConsumer), short, gapMissingInRing},
		{"满环内的空洞", rcRef(120, 120, stateApplied, byConsumer), full, gapMissingInRing},
		{"同 seq 不同 request_id", rcRef(20, 5, stateApplied, byConsumer), short, gapRingMismatch},
		{"同 seq 同 request_id 但结局不同", rcRef(20, 2, stateRejected, byConsumer), short, gapRingMismatch},
		{"REJECTED 原因码不同", rejectedRef(8), rejected, gapRingMismatch},
		{"同 seq 同 request_id 但类型不同", rcRef(50, 5, stateApplied, byConsumer), typed, gapRingMismatch},

		// 原有口径:水位没到。
		{"水位没到这个 seq", rcRef(21, 3, stateApplied, byConsumer), short, gapBeyondWatermark},
		{"账本没有该域(水位 0)", rcRef(10, 1, stateApplied, byConsumer), nil, gapBeyondWatermark},
		{"SETTLED_UNKNOWN 越过水位照样报", rcRef(25, 9, stateUnknown, byConsumer), short, gapBeyondWatermark},
		{"满环之后的 seq 越过水位", rcRef(133, 133, stateApplied, byConsumer), full, gapBeyondWatermark},

		// 一致或无从核对。
		{"在环里且一致", rcRef(20, 2, stateApplied, byConsumer), short, gapNone},
		{"满环的环头", rcRef(100, 100, stateApplied, byConsumer), full, gapNone},
		{"满环、早于环头:已挤出,无从核对", rcRef(99, 99, stateApplied, byConsumer), full, gapNone},
		{"REJECTED 原因码相同", rejectedRef(7), rejected, gapNone},
		{"VOIDED ↔ SKIPPED(原因码不比)", rcRef(40, 4, stateVoided, byConsumer), skipped, gapNone},
		{"SETTLED_UNKNOWN 水位以内不报(UNKNOWN 不入环)", rcRef(15, 9, stateUnknown, byConsumer), short, gapNone},
		{"管理员终结的行不比对", rcRef(15, 9, stateVoided, byAdmin), short, gapNone},
		{"管理员终结、越过水位也不比对", rcRef(25, 9, stateVoided, byAdmin), short, gapNone},
		{"PENDING 不是终态,不比对", rcRef(15, 9, statePending, 0), short, gapNone},
	}
	for _, tc := range cases {
		if got := finalizedLedgerGap(tc.ref, tc.cursor); got != tc.want {
			t.Errorf("%s: gap = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestValidLedgerRingCapacity:结构不合法或环超过 32 条的账本当不了证据(对账结论 unknown,不是「无差异」;
// 强制作废判 LEDGER_INVALID,见 TestProveNeverExecuted)。环长检查原来只在对账一侧(评审 R2),现在并进 validLedger。
func TestValidLedgerRingCapacity(t *testing.T) {
	cases := []struct {
		name   string
		ledger *componentpb.PlayerInboxLedgerComp
		want   bool
	}{
		{"空账本(从未消费过)", rcLedger(), true},
		{"满环 32 条", rcLedger(rcRing(100, ledgerRecentCapacity, 0)), true},
		{"环 33 条:C++ ValidateLedger 同样拒绝", rcLedger(rcRing(100, ledgerRecentCapacity+1, 0)), false},
		{"同一个域两条游标", rcLedger(rcCursor(gmDomain, rcDecision(10, 1, outApplied, 0)), rcCursor(gmDomain)), false},
		{"环非严格升序", rcLedger(&componentpb.PlayerInboxDomainCursor{Domain: uint32(gmDomain), DecidedThroughSeq: 20,
			Recent: []*componentpb.PlayerInboxDecision{rcDecision(20, 2, outApplied, 0), rcDecision(10, 1, outApplied, 0)}}), false},
	}
	for _, tc := range cases {
		if got := validLedger(tc.ledger); got != tc.want {
			t.Errorf("%s: valid = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ── 分片收集(M5)───────────────────────────────────────────────────────

// memShards 是 finalizedPageReader 的内存实现:每片的行按 (updated_ms, player_id, domain, seq) 降序存放,
// 与 RecentlyFinalized 的 SQL 同序;记录每次调用,供断言起点轮转与键集游标。collectFinalized 串行调用,不需要锁。
type memShards struct {
	rows  map[uint32][]FinalizedRef
	calls []memCall
	err   error
}

type memCall struct {
	shard  uint32
	before *FinalizedRef
	limit  int
}

// olderThan 报告 a 是否严格排在 b 之后(降序里更旧)。
func olderThan(a, b FinalizedRef) bool {
	switch {
	case a.UpdatedMs != b.UpdatedMs:
		return a.UpdatedMs < b.UpdatedMs
	case a.PlayerID != b.PlayerID:
		return a.PlayerID < b.PlayerID
	case a.Domain != b.Domain:
		return a.Domain < b.Domain
	default:
		return a.Seq < b.Seq
	}
}

func (m *memShards) read(_ context.Context, shard uint32, before *FinalizedRef, limit int) ([]FinalizedRef, error) {
	m.calls = append(m.calls, memCall{shard: shard, before: before, limit: limit})
	if m.err != nil {
		return nil, m.err
	}
	rows := m.rows[shard]
	start := 0
	if before != nil {
		start = len(rows)
		for i, r := range rows {
			if olderThan(r, *before) {
				start = i
				break
			}
		}
	}
	end := min(start+limit, len(rows))
	return append([]FinalizedRef(nil), rows[start:end]...), nil
}

// firstReadOrder 是各分片第一次被读的顺序。
func (m *memShards) firstReadOrder() []uint32 {
	seen := map[uint32]bool{}
	var out []uint32
	for _, c := range m.calls {
		if !seen[c.shard] {
			seen[c.shard] = true
			out = append(out, c.shard)
		}
	}
	return out
}

// shardRefs 造一个分片里的终态行,按给出的顺序即为降序(updated_ms 依次递减);玩家号满足 player_id % 64 == shard。
func shardRefs(shard uint32, players ...uint64) []FinalizedRef {
	out := make([]FinalizedRef, 0, len(players))
	for i, p := range players {
		out = append(out, FinalizedRef{
			PlayerID: p*ShardCount + uint64(shard), Domain: gmDomain, Seq: 10, State: stateApplied,
			RequestID: 1, MsgType: gmBlock, FinalizedBy: byConsumer, UpdatedMs: testNowMs - uint64(i),
		})
	}
	return out
}

func playerOf(shard uint32, p uint64) uint64 { return p*ShardCount + uint64(shard) }

// TestCollectFinalizedRotatesFromStartShard:从起始分片起环形扫满 64 片((start+i) % 64),没有触到上限不算截断。
// 回归:原实现每轮都从 shard 0 起步,一截断高号分片就永远轮不到。
func TestCollectFinalizedRotatesFromStartShard(t *testing.T) {
	m := &memShards{rows: map[uint32][]FinalizedRef{
		62: shardRefs(62, 1), 63: shardRefs(63, 1), 0: shardRefs(0, 1), 1: shardRefs(1, 1),
	}}
	scan, err := collectFinalized(context.Background(), ringOrder(62), defaultReconcileLimits, m.read)
	if err != nil {
		t.Fatal(err)
	}
	order := m.firstReadOrder()
	if len(order) != ShardCount || order[0] != 62 || order[1] != 63 || order[2] != 0 || order[63] != 61 {
		t.Fatalf("扫描顺序 = %v, want 62, 63, 0, 1, …, 61", order)
	}
	want := []uint64{playerOf(62, 1), playerOf(63, 1), playerOf(0, 1), playerOf(1, 1)}
	if len(scan.players) != len(want) {
		t.Fatalf("players = %v, want %v", scan.players, want)
	}
	for i := range want {
		if scan.players[i] != want[i] {
			t.Fatalf("players = %v, want %v(按扫描顺序首次出现)", scan.players, want)
		}
	}
	if scan.truncated() || scan.scanned != ShardCount || scan.unreached() != 0 {
		t.Fatalf("没触到上限: truncated=%v scanned=%d", scan.truncated(), scan.scanned)
	}
}

// TestCollectFinalizedScansOnlyGivenShards:多副本分片对账时,一个副本只扫自己负责的分片,按给定顺序,
// 别的分片一次都不读;没触到上限时「未读到」按本轮该扫的片数算,不按 64 算。
func TestCollectFinalizedScansOnlyGivenShards(t *testing.T) {
	m := &memShards{rows: map[uint32][]FinalizedRef{
		3: shardRefs(3, 1), 7: shardRefs(7, 1), 9: shardRefs(9, 1),
	}}
	scan, err := collectFinalized(context.Background(), []uint32{9, 3}, defaultReconcileLimits, m.read)
	if err != nil {
		t.Fatal(err)
	}
	if order := m.firstReadOrder(); len(order) != 2 || order[0] != 9 || order[1] != 3 {
		t.Fatalf("扫描顺序 = %v, want [9 3](分片 7 不归本副本,不该读)", order)
	}
	want := []uint64{playerOf(9, 1), playerOf(3, 1)}
	if len(scan.players) != len(want) || scan.players[0] != want[0] || scan.players[1] != want[1] {
		t.Fatalf("players = %v, want %v", scan.players, want)
	}
	if scan.truncated() || scan.scanned != 2 || scan.unreached() != 0 {
		t.Fatalf("truncated=%v scanned=%d unreached=%d, want false/2/0", scan.truncated(), scan.scanned, scan.unreached())
	}
	tallies, _ := scan.shardTallies()
	if len(tallies) != 2 || tallies[0].Shard != 9 || tallies[1].Shard != 3 || !tallies[0].Complete || !tallies[1].Complete {
		t.Fatalf("tallies = %+v, want 9、3 两片都读完", tallies)
	}
}

// TestReconcileShardsRejectsBadOrder:分片越界或重复时一行不读就报错 —— 越界读错分片,重复会把一片的读数记两次。
func TestReconcileShardsRejectsBadOrder(t *testing.T) {
	for _, order := range [][]uint32{{ShardCount}, {1, 2, 1}} {
		m := &memShards{}
		if _, err := reconcileShards(context.Background(), order, defaultReconcileLimits, m.read, nopLedgers{}); err == nil {
			t.Errorf("order %v: want error", order)
		}
		if len(m.calls) != 0 {
			t.Errorf("order %v: read %d pages before rejecting", order, len(m.calls))
		}
	}
	// 本副本不负责任何分片:空序列合法,一行不读、结果为空。
	m := &memShards{}
	res, err := reconcileShards(context.Background(), nil, defaultReconcileLimits, m.read, nopLedgers{})
	if err != nil || len(m.calls) != 0 || res.PlayersPlanned != 0 || len(res.Shards) != 0 || res.Truncated {
		t.Fatalf("empty order: res=%+v err=%v calls=%d", res, err, len(m.calls))
	}
}

// TestCollectFinalizedPerShardCap:单片按键集翻页,读满上限后探一行确认还有剩余才算截断(先核对最新的),
// 然后继续扫下一片;恰好读满上限的分片不算截断。
func TestCollectFinalizedPerShardCap(t *testing.T) {
	limits := reconcileLimits{pageSize: 2, perShard: 3, maxPlayers: 100, readConcurrency: 1}
	m := &memShards{rows: map[uint32][]FinalizedRef{
		5: shardRefs(5, 1, 2, 3, 4), // 4 行:超出上限 3
		6: shardRefs(6, 1, 2, 3),    // 恰好 3 行
	}}
	scan, err := collectFinalized(context.Background(), ringOrder(5), limits, m.read)
	if err != nil {
		t.Fatal(err)
	}
	if !scan.truncated() || len(scan.capped) != 1 || scan.capped[0] != 5 || scan.playerCapHit {
		t.Fatalf("capped = %v playerCapHit=%v, want 只有分片 5 截断", scan.capped, scan.playerCapHit)
	}
	if scan.scanned != ShardCount {
		t.Fatalf("单片截断不停整轮: scanned = %d", scan.scanned)
	}
	// 分片 5 只核对最新的 3 行;第 4 行(最老)被截掉。
	for _, p := range []uint64{1, 2, 3} {
		if len(scan.byPlayer[playerOf(5, p)]) != 1 {
			t.Errorf("分片 5 的第 %d 新的行应被收进来", p)
		}
	}
	if _, ok := scan.byPlayer[playerOf(5, 4)]; ok {
		t.Error("分片 5 最老的那一行超出上限,不应收进来")
	}
	// 分片 5 的调用序列:第一页 (nil, 2) → 第二页 (游标 = 第 2 行, 1) → 探一行 (游标 = 第 3 行, 1)。
	var shard5 []memCall
	for _, c := range m.calls {
		if c.shard == 5 {
			shard5 = append(shard5, c)
		}
	}
	rows5 := m.rows[5]
	if len(shard5) != 3 || shard5[0].before != nil || shard5[0].limit != 2 ||
		shard5[1].before == nil || *shard5[1].before != rows5[1] || shard5[1].limit != 1 ||
		shard5[2].before == nil || *shard5[2].before != rows5[2] || shard5[2].limit != 1 {
		t.Fatalf("分片 5 的翻页调用 = %+v", shard5)
	}
}

// TestCollectFinalizedPlayerCap:新出现的玩家超过上限时整轮停在当前分片,记下停在哪、剩几片;已收进来的玩家
// 后续的行照收;恰好数到上限不算截断。
func TestCollectFinalizedPlayerCap(t *testing.T) {
	rows := map[uint32][]FinalizedRef{
		0: append(shardRefs(0, 1, 2), shardRefs(0, 1)...), // 玩家 1 两行、玩家 2 一行
		1: shardRefs(1, 3, 4),
		2: shardRefs(2, 5),
	}
	rows[0][2].Seq, rows[0][2].UpdatedMs = 9, testNowMs-5 // 玩家 1 的第二行,更旧
	limits := reconcileLimits{pageSize: 10, perShard: 10, maxPlayers: 3, readConcurrency: 1}

	scan, err := collectFinalized(context.Background(), ringOrder(0), limits, (&memShards{rows: rows}).read)
	if err != nil {
		t.Fatal(err)
	}
	if !scan.playerCapHit || scan.stoppedShard != 1 || scan.scanned != 2 || scan.unreached() != ShardCount-2 {
		t.Fatalf("cap=%v stopped=%d scanned=%d unreached=%d, want 停在分片 1、剩 62 片",
			scan.playerCapHit, scan.stoppedShard, scan.scanned, scan.unreached())
	}
	if len(scan.players) != 3 || scan.players[2] != playerOf(1, 3) {
		t.Fatalf("players = %v, want 玩家 1、2、3", scan.players)
	}
	if len(scan.byPlayer[playerOf(0, 1)]) != 2 {
		t.Fatalf("已收进来的玩家的行要收全: %+v", scan.byPlayer[playerOf(0, 1)])
	}

	limits.maxPlayers = 5
	scan, err = collectFinalized(context.Background(), ringOrder(0), limits, (&memShards{rows: rows}).read)
	if err != nil || scan.truncated() || len(scan.players) != 5 || scan.scanned != ShardCount {
		t.Fatalf("恰好数到上限不算截断: truncated=%v players=%v scanned=%d err=%v",
			scan.truncated(), scan.players, scan.scanned, err)
	}
}

// TestCollectFinalizedErrors:读失败与 ctx 结束都返回 error(收集结果不完整,不得拿去判「无差异」)。
func TestCollectFinalizedErrors(t *testing.T) {
	boom := errors.New("db down")
	if _, err := collectFinalized(context.Background(), ringOrder(0), defaultReconcileLimits, (&memShards{err: boom}).read); !errors.Is(err, boom) {
		t.Fatalf("读失败: err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := &memShards{}
	if _, err := collectFinalized(ctx, ringOrder(0), defaultReconcileLimits, m.read); !errors.Is(err, context.Canceled) || len(m.calls) != 0 {
		t.Fatalf("ctx 已结束: err = %v calls = %d", err, len(m.calls))
	}
}

// ── 有界并发读账本 ───────────────────────────────────────────────────────

// barrierLedgers 在恰好 limit 个读同时在途之前让每个读都等着:并发度够,屏障打开、全部通过;
// 实现若是串行的,第一个读就等到自己的超时、返回错误(测试判红)。peak 记录同时在途的最大数。
type barrierLedgers struct {
	limit    int
	mu       sync.Mutex
	inFlight int
	peak     int
	reached  chan struct{}
	once     sync.Once
	calls    atomic.Int64
}

func newBarrierLedgers(limit int) *barrierLedgers {
	return &barrierLedgers{limit: limit, reached: make(chan struct{})}
}

func (b *barrierLedgers) ReadInboxLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerInboxLedgerComp, bool, error) {
	b.calls.Add(1)
	if _, ok := ctx.Deadline(); !ok {
		return nil, false, errors.New("ledger read without deadline")
	}
	b.mu.Lock()
	b.inFlight++
	b.peak = max(b.peak, b.inFlight)
	if b.inFlight >= b.limit {
		b.once.Do(func() { close(b.reached) })
	}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.inFlight--
		b.mu.Unlock()
	}()
	select {
	case <-b.reached:
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
	return rcLedger(rcCursor(gmDomain, rcDecision(playerID, playerID, outApplied, 0))), playerID%2 == 0, nil
}

// TestReadLedgersBoundedConcurrency:并发度恰好到上限、从不超过;结果按下标放回;每次读都带截止时间。
// 回归:原实现串行读,2000 名玩家 × 每次至多 2s 会撞穿 2 分钟的轮预算。
func TestReadLedgersBoundedConcurrency(t *testing.T) {
	const limit = 4
	players := []uint64{10, 11, 12, 13, 14, 15, 16, 17}
	b := newBarrierLedgers(limit)
	got := readLedgersBounded(context.Background(), b, players, limit)
	if b.peak != limit {
		t.Fatalf("同时在途的读最多 %d 个,want 恰好 %d", b.peak, limit)
	}
	for i, p := range players {
		r := got[i]
		if !r.done || r.err != nil || r.found != (p%2 == 0) || r.ledger.GetCursors()[0].GetDecidedThroughSeq() != p {
			t.Fatalf("players[%d]=%d 的结果放错了下标或没读完: %+v", i, p, r)
		}
	}
}

// panicLedgers 读到指定玩家就 panic,其余返回空账本。
type panicLedgers struct{ boom uint64 }

func (p panicLedgers) ReadInboxLedger(_ context.Context, playerID uint64) (*componentpb.PlayerInboxLedgerComp, bool, error) {
	if playerID == p.boom {
		panic("ledger reader exploded")
	}
	return rcLedger(), true, nil
}

// TestReadLedgersBoundedCancelAndPanic:ctx 已结束不发起读;单次读 panic 只丢那一名玩家,这一路继续读后面的。
func TestReadLedgersBoundedCancelAndPanic(t *testing.T) {
	logtest.Discard(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := newBarrierLedgers(1)
	got := readLedgersBounded(ctx, b, []uint64{1, 2, 3}, 2)
	if b.calls.Load() != 0 || got[0].done || got[1].done || got[2].done {
		t.Fatalf("ctx 已结束仍发起了读: calls=%d %+v", b.calls.Load(), got)
	}

	got = readLedgersBounded(context.Background(), panicLedgers{boom: 2}, []uint64{1, 2, 3}, 1)
	if !got[0].done || got[1].done || !got[2].done {
		t.Fatalf("panic 的那名玩家 done=false,其余照常读完: %+v", got)
	}
	if out := readLedgersBounded(context.Background(), panicLedgers{}, nil, 8); len(out) != 0 {
		t.Fatalf("没有玩家: %+v", out)
	}
}

// ── 一轮的组装 ───────────────────────────────────────────────────────────

// mapLedgers 按玩家给出账本(只读 map,可并发调用)。
type mapLedgers struct {
	ledgers map[uint64]*componentpb.PlayerInboxLedgerComp
	errs    map[uint64]error
}

func (m mapLedgers) ReadInboxLedger(_ context.Context, playerID uint64) (*componentpb.PlayerInboxLedgerComp, bool, error) {
	if err := m.errs[playerID]; err != nil {
		return nil, false, err
	}
	l, ok := m.ledgers[playerID]
	return l, ok, nil
}

// TestReconcileRecentRound:一轮里逐个玩家判定 —— 有空洞的计不一致,读不到存档 / 账本不合法 / 读失败
// 都不算「无差异」也不算不一致(各自进 reconcile_ledger_read_total),全部计入已判定的玩家数。
func TestReconcileRecentRound(t *testing.T) {
	logtest.Discard(t)
	ref := func(shard uint32, p, seq, requestID uint64, updatedMs uint64) FinalizedRef {
		return FinalizedRef{PlayerID: playerOf(shard, p), Domain: gmDomain, Seq: seq, State: stateApplied,
			RequestID: requestID, MsgType: gmBlock, FinalizedBy: byConsumer, UpdatedMs: updatedMs}
	}
	m := &memShards{rows: map[uint32][]FinalizedRef{
		// 玩家 A:10 与 20 都已 APPLIED;账本环里只剩 20 —— 10 的落盘丢了、被 20 掩盖。
		1: {ref(1, 1, 20, 2, testNowMs), ref(1, 1, 10, 1, testNowMs-1)},
		2: {ref(2, 1, 5, 5, testNowMs)},  // 玩家 B:读不到存档
		3: {ref(3, 1, 5, 5, testNowMs)},  // 玩家 C:账本环 33 条,不合法
		4: {ref(4, 1, 5, 5, testNowMs)},  // 玩家 D:读失败
		5: {ref(5, 1, 30, 3, testNowMs)}, // 玩家 E:一致
	}}
	ledgers := mapLedgers{
		ledgers: map[uint64]*componentpb.PlayerInboxLedgerComp{
			playerOf(1, 1): rcLedger(rcCursor(gmDomain, rcDecision(20, 2, outApplied, 0))),
			playerOf(3, 1): rcLedger(rcRing(1, ledgerRecentCapacity+1, 0)),
			playerOf(5, 1): rcLedger(rcCursor(gmDomain, rcDecision(29, 9, outApplied, 0), rcDecision(30, 3, outApplied, 0))),
		},
		errs: map[uint64]error{playerOf(4, 1): errors.New("data_service down")},
	}
	res, err := reconcileShards(context.Background(), ringOrder(0), defaultReconcileLimits, m.read, ledgers)
	if err != nil {
		t.Fatal(err)
	}
	if res.Inconsistent != 1 || res.PlayersPlanned != 5 || res.PlayersChecked != 5 || res.Truncated || len(res.Shards) != ShardCount {
		t.Fatalf("res = %+v, want 1 处不一致、5 名玩家全部判定、未截断、64 片读数", res)
	}
	// 分片读数(评审 R1):A 的空洞记在分片 1;D 读失败,分片 4 的 0 只是下界(不 Complete);
	// 读不到存档(B)与账本不合法(C)是确定的结论,不影响 Complete。
	for i, s := range res.Shards {
		wantInconsistent, wantComplete := 0, true
		switch s.Shard {
		case 1:
			wantInconsistent = 1
		case 4:
			wantComplete = false
		}
		if s.Shard != uint32(i) || s.Inconsistent != wantInconsistent || s.Complete != wantComplete {
			t.Errorf("Shards[%d] = %+v, want shard=%d inconsistent=%d complete=%v", i, s, i, wantInconsistent, wantComplete)
		}
	}

	limits := reconcileLimits{pageSize: 1, perShard: 1, maxPlayers: 100, readConcurrency: 2}
	res, err = reconcileShards(context.Background(), ringOrder(0), limits, m.read, ledgers)
	if err != nil || !res.Truncated || res.Inconsistent != 0 {
		t.Fatalf("分片 1 只核对最新的 20(一致),截掉了 10: res=%+v err=%v", res, err)
	}
	if s := res.Shards[1]; s.Shard != 1 || s.Complete || s.Inconsistent != 0 {
		t.Fatalf("分片 1 被单片上限截掉了最老的行,它的 0 只是下界: %+v", s)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if res, err := reconcileShards(ctx, ringOrder(0), defaultReconcileLimits, m.read, ledgers); !errors.Is(err, context.Canceled) ||
		res.PlayersChecked != 0 || res.Inconsistent != 0 || len(res.Shards) != 0 {
		t.Fatalf("ctx 已结束: res=%+v err=%v", res, err)
	}
}

// TestReconcileRecentStoppedShardIsPartial:玩家数触顶时停下的那一片只读了一部分,读数不 Complete;
// 之前读完的分片照常 Complete;没读到的分片不出现在 Shards 里。
func TestReconcileRecentStoppedShardIsPartial(t *testing.T) {
	logtest.Discard(t)
	m := &memShards{rows: map[uint32][]FinalizedRef{
		10: shardRefs(10, 1, 2),
		11: shardRefs(11, 1, 2), // 第 2 名玩家撞上上限 3
		12: shardRefs(12, 1),
	}}
	ledgers := mapLedgers{ledgers: map[uint64]*componentpb.PlayerInboxLedgerComp{}}
	for _, shard := range []uint32{10, 11, 12} {
		for _, p := range []uint64{1, 2} {
			ledgers.ledgers[playerOf(shard, p)] = rcLedger(rcCursor(gmDomain, rcDecision(10, 1, outApplied, 0)))
		}
	}
	limits := reconcileLimits{pageSize: 10, perShard: 10, maxPlayers: 3, readConcurrency: 2}
	res, err := reconcileShards(context.Background(), ringOrder(10), limits, m.read, ledgers)
	if err != nil || !res.Truncated || res.PlayersChecked != 3 || res.Inconsistent != 0 {
		t.Fatalf("res = %+v err=%v", res, err)
	}
	want := []ShardTally{{Shard: 10, Complete: true}, {Shard: 11, Complete: false}}
	if len(res.Shards) != len(want) || res.Shards[0] != want[0] || res.Shards[1] != want[1] {
		t.Fatalf("Shards = %+v, want %+v", res.Shards, want)
	}
}

// TestCollectFinalizedReadsStridePrefix 钉住覆盖下界(评审 R1):生产上限下,不论玩家数在哪一片触顶,
// 从起点起的前 reconcileStartStride 片一定读完。构造最坏情况:每片都塞满 reconcilePerShardLimit 名不同的玩家。
func TestCollectFinalizedReadsStridePrefix(t *testing.T) {
	if reconcileStartStride < 1 || reconcileStartStride*reconcilePerShardLimit > reconcileMaxPlayers {
		t.Fatalf("步长 %d 与上限不匹配:前 stride 片的玩家数 %d 必须不超过玩家数上限 %d",
			reconcileStartStride, reconcileStartStride*reconcilePerShardLimit, reconcileMaxPlayers)
	}
	const start = uint32(62)
	rows := map[uint32][]FinalizedRef{}
	for i := uint32(0); i < reconcileStartStride+2; i++ {
		shard := (start + i) % ShardCount
		players := make([]uint64, reconcilePerShardLimit)
		for p := range players {
			players[p] = uint64(p + 1)
		}
		rows[shard] = shardRefs(shard, players...)
	}
	scan, err := collectFinalized(context.Background(), ringOrder(start), defaultReconcileLimits, (&memShards{rows: rows}).read)
	if err != nil {
		t.Fatal(err)
	}
	if !scan.playerCapHit || scan.scanned != reconcileStartStride+1 || len(scan.capped) != 0 {
		t.Fatalf("playerCapHit=%v scanned=%d capped=%v, want 前 %d 片读完、停在第 %d 片",
			scan.playerCapHit, scan.scanned, scan.capped, reconcileStartStride, reconcileStartStride+1)
	}
	tallies, _ := scan.shardTallies()
	for i := 0; i < reconcileStartStride; i++ {
		if want := (start + uint32(i)) % ShardCount; tallies[i].Shard != want || !tallies[i].Complete {
			t.Fatalf("第 %d 片 = %+v, want shard=%d 且读完", i, tallies[i], want)
		}
	}
	if last := tallies[reconcileStartStride]; last.Complete || last.Shard != scan.stoppedShard {
		t.Fatalf("停下的那一片 = %+v, want 不 Complete、shard=%d", last, scan.stoppedShard)
	}
}

package main

// route_publish.go 的单测。本 module 没有 miniredis(见 gap_fixes_test.go 头注释),Redis 一侧用挂在真 *redis.Client 上的钩子
// (fakeRouteRedis)应答:命令不出进程、不拨号,每条 pipeline 的命令按发送顺序记下 —— 「EVAL 与 WAITAOF 在同一条 pipeline 里」
// 由此可断言;同一条**连接**由真 Redis 用例的 CLIENT ID 夹心证明(route_store_integration_test.go)。
// 脚本的执行语义(set-if-newer、touch、遗留值守卫)只在真 Redis 上测;这里只证本层的分类、重发与拒绝口径。
//
// rev 与回复分类的向量与 go/shared/placement/route_test.go **逐字相同**,改任何一边都必须同步改另一边。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// ── 假 Redis(go-redis 钩子)──────────────────────────────────────────────────

// fakeRouteRedis 截住命令,由 reply 按命令应答。pipeline 参数是第几条 pipeline(从 0 起;单条命令为 -1)。
type fakeRouteRedis struct {
	mu        sync.Mutex
	pipelines [][]redis.Cmder
	singles   []redis.Cmder
	reply     func(pipeline int, cmd redis.Cmder) (any, error)
}

func newFakeRouteRedis(t *testing.T, reply func(pipeline int, cmd redis.Cmder) (any, error)) (*redis.Client, *fakeRouteRedis) {
	t.Helper()
	f := &fakeRouteRedis{reply: reply}
	rdb := redis.NewClient(&redis.Options{Addr: "fake-route-redis.invalid:6379"})
	rdb.AddHook(f)
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, f
}

func (f *fakeRouteRedis) DialHook(redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("fake route redis: dialing is not allowed in unit tests")
	}
}

func (f *fakeRouteRedis) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		f.mu.Lock()
		f.singles = append(f.singles, cmd)
		f.mu.Unlock()
		f.answer(-1, cmd)
		return cmd.Err()
	}
}

func (f *fakeRouteRedis) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(_ context.Context, cmds []redis.Cmder) error {
		f.mu.Lock()
		n := len(f.pipelines)
		f.pipelines = append(f.pipelines, cmds)
		f.mu.Unlock()
		var first error
		for _, c := range cmds {
			f.answer(n, c)
			if first == nil && c.Err() != nil {
				first = c.Err()
			}
		}
		return first
	}
}

func (f *fakeRouteRedis) answer(n int, cmd redis.Cmder) {
	v, err := f.reply(n, cmd)
	if err != nil {
		cmd.SetErr(err)
		return
	}
	switch c := cmd.(type) {
	case *redis.Cmd:
		c.SetVal(v)
	case *redis.StringCmd:
		s, _ := v.(string)
		c.SetVal(s)
	case *redis.StatusCmd:
		s, _ := v.(string)
		c.SetVal(s)
	case *redis.IntCmd:
		i, _ := v.(int64)
		c.SetVal(i)
	case *redis.SliceCmd:
		s, _ := v.([]any)
		c.SetVal(s)
	default:
		cmd.SetErr(fmt.Errorf("fake route redis: unsupported command type %T", cmd))
	}
}

// names 返回第 n 条 pipeline 的命令名(小写)。
func (f *fakeRouteRedis) names(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.pipelines[n] {
		out = append(out, c.Name())
	}
	return out
}

func (f *fakeRouteRedis) pipelineCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pipelines)
}

// fakeRedisReplyError 是一条 Redis 错误回复(实现 redis.Error,go-redis 据此区分「Redis 拒绝」与网络错误)。
type fakeRedisReplyError string

func (e fakeRedisReplyError) Error() string { return string(e) }

func (fakeRedisReplyError) RedisError() {}

// evalArgs 取 EVAL 的 rev / home / placement(ARGV)。
func evalArgs(cmd redis.Cmder) (rev, home, plc string) {
	a := cmd.Args()
	return a[len(a)-3].(string), a[len(a)-2].(string), a[len(a)-1].(string)
}

// applyAll 对每条 EVAL 回 Applied、对 WAITAOF 回 acks。
func applyAll(acks []any) func(int, redis.Cmder) (any, error) {
	return func(_ int, cmd redis.Cmder) (any, error) {
		switch cmd.Name() {
		case "eval":
			rev, _, _ := evalArgs(cmd)
			return []any{int64(1), rev}, nil
		case "waitaof":
			return acks, nil
		}
		return nil, fmt.Errorf("unexpected command %s", cmd.Name())
	}
}

func newTestRoutePublisher(rdb *redis.Client, local, replicas int) *routePublisher {
	dur, err := newRouteDurability(local, replicas, 1500)
	if err != nil {
		panic(err)
	}
	p := newRoutePublisher(rdb, dur)
	p.now = func() time.Time { return time.Unix(1_760_000_000, 0) }
	p.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	p.backoff = func() time.Duration { return time.Millisecond }
	return p
}

// ── rev 与回复分类(向量与 shared route_test.go 逐字相同)──────────────────────

var routeRevValidVectors = []string{
	"1",
	"9",
	"10",
	"18446744073709551615", // MaxUint64
}

var routeRevInvalidVectors = []string{
	"",
	"0",
	"01",
	"-1",
	"1a",
	" 1",
	"1 ",
	"+1",
	"123456789012345678901", // 21 位
	"18446744073709551616",  // MaxUint64 + 1:20 位但溢出
	"99999999999999999999",
}

func TestRouteRevHelpersMatchShared(t *testing.T) {
	for _, s := range routeRevValidVectors {
		if !validRouteRev(s) {
			t.Fatalf("validRouteRev(%q) = false", s)
		}
	}
	for _, s := range routeRevInvalidVectors {
		if validRouteRev(s) {
			t.Fatalf("validRouteRev(%q) = true", s)
		}
	}
	if formatRouteRev(0) != "" || formatRouteRev(18446744073709551615) != "18446744073709551615" {
		t.Fatal("formatRouteRev drifted from shared FormatRev")
	}
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"9", "10", -1},
		{"2", "10", -1},
		{"10", "9", 1},
		{"10", "10", 0},
		{"18446744073709551615", "18446744073709551614", 1},
		{"18446744073709551614", "18446744073709551615", -1},
	} {
		if got, err := compareRouteRev(tc.a, tc.b); err != nil || got != tc.want {
			t.Fatalf("compareRouteRev(%q, %q) = (%d, %v), want %d", tc.a, tc.b, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "01", "18446744073709551616"} {
		if _, err := compareRouteRev(bad, "1"); !errors.Is(err, errPlacementMalformed) {
			t.Fatalf("compareRouteRev(%q, 1) error = %v", bad, err)
		}
	}
	if got := mapRevKey(90002); got != "player:maprev:90002" {
		t.Fatalf("mapRevKey = %q", got)
	}
}

func TestClassifyRoutePublishReplyMatchesShared(t *testing.T) {
	const (
		rev  = "5"
		home = "2"
		plc  = "2:1"
	)
	for _, tc := range []struct {
		name string
		raw  any
		want routePublishReply
	}{
		{"已写入", []any{int64(1), rev}, routePublishReply{Outcome: routePublishApplied, CurRev: rev, CurHome: home, CurPlacement: plc}},
		{"缓存更新_被取代", []any{int64(0), "10", home, plc}, routePublishReply{Outcome: routePublishSuperseded, CurRev: "10", CurHome: home, CurPlacement: plc}},
		{"同版本同值_已确认", []any{int64(0), rev, home, plc}, routePublishReply{Outcome: routePublishConfirmed, CurRev: rev, CurHome: home, CurPlacement: plc}},
		{"同版本_home不同_冲突", []any{int64(0), rev, "3", plc}, routePublishReply{Outcome: routePublishConflict, CurRev: rev, CurHome: "3", CurPlacement: plc}},
		{"同版本_落点不同_冲突", []any{int64(0), rev, home, "2:2"}, routePublishReply{Outcome: routePublishConflict, CurRev: rev, CurHome: home, CurPlacement: "2:2"}},
		{"无maprev_遗留值不同", []any{int64(2), "", "3", ""}, routePublishReply{Outcome: routePublishLegacyConflict, CurHome: "3", CurPlacement: ""}},
		{"合服围栏", []any{int64(3)}, routePublishReply{Outcome: routePublishFenced}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyRoutePublishReply(tc.raw, rev, home, plc)
			if err != nil || got != tc.want {
				t.Fatalf("classifyRoutePublishReply(%#v) = (%+v, %v), want %+v", tc.raw, got, err, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name string
		raw  any
	}{
		{"已写入但rev不是本次", []any{int64(1), "6"}},
		{"已写入但rev不是串", []any{int64(1), int64(5)}},
		{"未改但缓存rev小于本次", []any{int64(0), "4", home, plc}},
		{"未改但缓存rev是20位且溢出", []any{int64(0), "18446744073709551616", home, plc}},
		{"未改但缓存rev有前导零", []any{int64(0), "05", home, plc}},
		{"未改但元素个数不对", []any{int64(0), rev, home}},
		{"遗留回复但值反而相同", []any{int64(2), "", home, plc}},
		{"遗留回复带了rev", []any{int64(2), "1", "3", ""}},
		{"nil", nil},
		{"空数组", []any{}},
		{"首元素不是整数", []any{"1", "3"}},
		{"未知回复码", []any{int64(4)}},
		{"围栏回复多了元素", []any{int64(3), "x"}},
		{"不是数组", "OK"},
	} {
		t.Run("拒绝_"+tc.name, func(t *testing.T) {
			if got, err := classifyRoutePublishReply(tc.raw, rev, home, plc); !errors.Is(err, errUnexpectedRoutePublishReply) {
				t.Fatalf("classifyRoutePublishReply(%#v) = (%+v, %v), want errUnexpectedRoutePublishReply", tc.raw, got, err)
			}
		})
	}
}

func TestRoutePublishAccepted(t *testing.T) {
	for _, tc := range []struct {
		reply routePublishReply
		want  bool
	}{
		{routePublishReply{Outcome: routePublishApplied}, true},
		{routePublishReply{Outcome: routePublishConfirmed}, true},
		{routePublishReply{Outcome: routePublishSuperseded, CurHome: "102", CurPlacement: ""}, true},
		{routePublishReply{Outcome: routePublishSuperseded, CurHome: "103", CurPlacement: ""}, false},
		{routePublishReply{Outcome: routePublishSuperseded, CurHome: "102", CurPlacement: "5:1"}, false},
		{routePublishReply{Outcome: routePublishConflict, CurHome: "102"}, false},
		{routePublishReply{Outcome: routePublishLegacyConflict}, false},
		{routePublishReply{Outcome: routePublishFenced}, false},
	} {
		if got := routePublishAccepted(tc.reply, "102", ""); got != tc.want {
			t.Fatalf("routePublishAccepted(%+v) = %v, want %v", tc.reply, got, tc.want)
		}
	}
	if routePublishLegacyConflict.String() != "legacy_conflict" || routePublishOutcome(0).String() != "unknown" {
		t.Fatal("outcome names drifted from data_service_route_publish_total{result}")
	}
}

// ── WAITAOF 参数与回复 ────────────────────────────────────────────────────────

func TestNewRouteDurability(t *testing.T) {
	d, err := newRouteDurability(1, 0, 1500)
	if err != nil || d.NumLocal != 1 || d.NumReplicas != 0 || d.Timeout != 1500*time.Millisecond {
		t.Fatalf("defaults = (%+v, %v)", d, err)
	}
	if _, err := newRouteDurability(0, 1, routeWaitAOFTimeoutMaxMs); err != nil {
		t.Fatalf("replicas only: %v", err)
	}
	for _, c := range [][3]int{{0, 0, 1500}, {2, 0, 1500}, {-1, 0, 1500}, {1, -1, 1500}, {1, 0, 0}, {1, 0, routeWaitAOFTimeoutMaxMs + 1}} {
		if _, err := newRouteDurability(c[0], c[1], c[2]); err == nil {
			t.Fatalf("newRouteDurability%v must be refused", c)
		}
	}
}

func TestCheckRouteWaitAOFReply(t *testing.T) {
	if err := checkRouteWaitAOFReply([]any{int64(1), int64(1)}, 1, 1); err != nil {
		t.Fatalf("enough acks: %v", err)
	}
	if err := checkRouteWaitAOFReply([]any{int64(1), int64(0)}, 1, 1); !errors.Is(err, errRoutePublishNotDurable) {
		t.Fatalf("short acks error = %v, want errRoutePublishNotDurable (retried)", err)
	}
	for _, bad := range [][]any{{int64(1)}, {"1", int64(0)}, {int64(-1), int64(0)}, nil} {
		if err := checkRouteWaitAOFReply(bad, 1, 0); !errors.Is(err, errUnexpectedRouteWaitAOFReply) {
			t.Fatalf("checkRouteWaitAOFReply(%v) error = %v, want errUnexpectedRouteWaitAOFReply", bad, err)
		}
	}
}

func TestRouteWaitAOFBudgetMs(t *testing.T) {
	now := time.Unix(1_760_000_000, 0)
	if ms, ok := routeWaitAOFBudgetMs(now, time.Time{}, false, 1500*time.Millisecond); !ok || ms != 1500 {
		t.Fatalf("no deadline = (%d, %v)", ms, ok)
	}
	if ms, ok := routeWaitAOFBudgetMs(now, now.Add(600*time.Millisecond), true, 1500*time.Millisecond); !ok || ms != 500 {
		t.Fatalf("near deadline = (%d, %v), want 500 (remaining - margin)", ms, ok)
	}
	if _, ok := routeWaitAOFBudgetMs(now, now.Add(100*time.Millisecond+999*time.Microsecond), true, 1500*time.Millisecond); ok {
		t.Fatal("a budget below 1ms must not be sent (WAITAOF 0 blocks forever)")
	}
}

// ── 发布 ──────────────────────────────────────────────────────────────────────

func TestPublishRoutes_EvalsThenWaitAOFInOnePipeline(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	rdb, f := newFakeRouteRedis(t, applyAll([]any{int64(1), int64(0)}))
	p := newTestRoutePublisher(rdb, 1, 0)
	rep, err := p.publishRoutes(ctx, []mergeRouteRow{
		{PlayerID: 90002, HomeZone: 102, Rev: 5},
		{PlayerID: 7, HomeZone: 102, StorageID: 101, PlacementVersion: 1, Rev: 9},
	})
	if err != nil {
		t.Fatalf("publishRoutes: %v", err)
	}
	if rep.Rows != 2 || rep.Applied != 2 {
		t.Fatalf("report = %s", rep)
	}
	if f.pipelineCount() != 1 || !reflect.DeepEqual(f.names(0), []string{"eval", "eval", "waitaof"}) {
		t.Fatalf("pipelines = %d, first = %v; want one pipeline [eval eval waitaof]", f.pipelineCount(), f.names(0))
	}
	cmds := f.pipelines[0]
	wantEval := []any{"eval", routePublishScript, 3, "player:zone:90002", "player:placement:90002", "player:maprev:90002", "5", "102", ""}
	if !reflect.DeepEqual(cmds[0].Args(), wantEval) {
		t.Fatalf("first EVAL args = %#v", cmds[0].Args())
	}
	if got := cmds[1].Args(); got[6] != "9" || got[7] != "102" || got[8] != "101:1" {
		t.Fatalf("second EVAL ARGV = %#v", got[6:])
	}
	if !reflect.DeepEqual(cmds[2].Args(), []any{"WAITAOF", 1, 0, int64(1500)}) {
		t.Fatalf("WAITAOF args = %#v", cmds[2].Args())
	}
	if !strings.Contains(routePublishScript, "KEYS: 1=player:zone:{id} 2=player:placement:{id} 3=player:maprev:{id}") {
		t.Fatal("the script header no longer documents the KEYS order this file sends")
	}
}

// TestPublishRoutes_ResendsTheWholeBatchWhenWaitAOFIsShort:份数不足 → 整批(EVAL 全部 + WAITAOF)在新的一条 pipeline 里重发。
func TestPublishRoutes_ResendsTheWholeBatchWhenWaitAOFIsShort(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	rdb, f := newFakeRouteRedis(t, func(n int, cmd redis.Cmder) (any, error) {
		switch cmd.Name() {
		case "eval":
			rev, home, plc := evalArgs(cmd)
			if n == 0 {
				return []any{int64(1), rev}, nil
			}
			return []any{int64(0), rev, home, plc}, nil // 第二次:缓存已是这一版(touch 之后再确认)
		case "waitaof":
			if n == 0 {
				return []any{int64(1), int64(0)}, nil
			}
			return []any{int64(1), int64(1)}, nil
		}
		return nil, fmt.Errorf("unexpected %s", cmd.Name())
	})
	p := newTestRoutePublisher(rdb, 1, 1)
	rep, err := p.publishRoutes(ctx, []mergeRouteRow{{PlayerID: 1, HomeZone: 102, Rev: 2}, {PlayerID: 2, HomeZone: 102, Rev: 3}})
	if err != nil {
		t.Fatalf("publishRoutes: %v", err)
	}
	if f.pipelineCount() != 2 || !reflect.DeepEqual(f.names(1), []string{"eval", "eval", "waitaof"}) {
		t.Fatalf("pipelines = %d, second = %v", f.pipelineCount(), f.names(1))
	}
	if rep.Confirmed != 2 || rep.Applied != 0 {
		t.Fatalf("the report must come from the confirmed attempt: %s", rep)
	}
}

func TestPublishRoutes_GivesUpWhenNeverDurable(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	rdb, f := newFakeRouteRedis(t, applyAll([]any{int64(1), int64(0)}))
	p := newTestRoutePublisher(rdb, 1, 1)
	_, err := p.publishRoutes(ctx, []mergeRouteRow{{PlayerID: 1, HomeZone: 102, Rev: 2}})
	if !errors.Is(err, errRoutePublishNotDurable) || f.pipelineCount() != routePublishAttempts {
		t.Fatalf("error = %v after %d pipelines, want errRoutePublishNotDurable after %d", err, f.pipelineCount(), routePublishAttempts)
	}
}

// TestRouteBatchPublishSameRevDifferentValueFails:缓存同 rev 而值不同 = 冲突,零写入、不重试、不伪装成功(规格 §3 表第 2 行)。
func TestRouteBatchPublishSameRevDifferentValueFails(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	rdb, f := newFakeRouteRedis(t, func(_ int, cmd redis.Cmder) (any, error) {
		if cmd.Name() == "eval" {
			rev, _, _ := evalArgs(cmd)
			return []any{int64(0), rev, "103", ""}, nil
		}
		return []any{int64(1), int64(0)}, nil
	})
	p := newTestRoutePublisher(rdb, 1, 0)
	_, err := p.publishRoutes(ctx, []mergeRouteRow{{PlayerID: 1, HomeZone: 102, Rev: 2}})
	if !errors.Is(err, errRoutePublishRejected) || !strings.Contains(err.Error(), "conflict") || f.pipelineCount() != 1 {
		t.Fatalf("error = %v after %d pipelines, want one rejected attempt", err, f.pipelineCount())
	}
}

func TestPublishRoutes_SupersededPassesOnlyWithTheSameValue(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	cached := "102"
	rdb, _ := newFakeRouteRedis(t, func(_ int, cmd redis.Cmder) (any, error) {
		if cmd.Name() == "eval" {
			return []any{int64(0), "8", cached, ""}, nil
		}
		return []any{int64(1), int64(0)}, nil
	})
	p := newTestRoutePublisher(rdb, 1, 0)
	rep, err := p.publishRoutes(ctx, []mergeRouteRow{{PlayerID: 1, HomeZone: 102, Rev: 5}})
	if err != nil || rep.Superseded != 1 {
		t.Fatalf("superseded with the same value = (%s, %v)", rep, err)
	}
	cached = "103"
	if _, err := p.publishRoutes(ctx, []mergeRouteRow{{PlayerID: 1, HomeZone: 102, Rev: 5}}); !errors.Is(err, errRoutePublishRejected) {
		t.Fatalf("superseded with another value error = %v, want errRoutePublishRejected", err)
	}
}

func TestPublishRoutes_ErrorReplyIsDeterministicNetworkErrorRetries(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	rdb, f := newFakeRouteRedis(t, func(_ int, cmd redis.Cmder) (any, error) {
		if cmd.Name() == "eval" {
			return nil, fakeRedisReplyError("ERR route publish: malformed maprev")
		}
		return []any{int64(1), int64(0)}, nil
	})
	p := newTestRoutePublisher(rdb, 1, 0)
	if _, err := p.publishRoutes(ctx, []mergeRouteRow{{PlayerID: 1, HomeZone: 102, Rev: 5}}); !errors.Is(err, errRoutePublishRejected) || f.pipelineCount() != 1 {
		t.Fatalf("script error reply = %v after %d pipelines, want one rejected attempt", err, f.pipelineCount())
	}

	rdb, f = newFakeRouteRedis(t, func(n int, cmd redis.Cmder) (any, error) {
		if n == 0 {
			return nil, errors.New("read tcp: connection reset by peer")
		}
		return applyAll([]any{int64(1), int64(0)})(n, cmd)
	})
	p = newTestRoutePublisher(rdb, 1, 0)
	if _, err := p.publishRoutes(ctx, []mergeRouteRow{{PlayerID: 1, HomeZone: 102, Rev: 5}}); err != nil || f.pipelineCount() != 2 {
		t.Fatalf("network error then success = %v after %d pipelines, want success on the 2nd", err, f.pipelineCount())
	}

	rdb, f = newFakeRouteRedis(t, func(_ int, cmd redis.Cmder) (any, error) {
		if cmd.Name() == "waitaof" {
			return nil, fakeRedisReplyError("ERR WAITAOF cannot be used when numlocal is set but appendonly is disabled.")
		}
		rev, _, _ := evalArgs(cmd)
		return []any{int64(1), rev}, nil
	})
	p = newTestRoutePublisher(rdb, 1, 0)
	if _, err := p.publishRoutes(ctx, []mergeRouteRow{{PlayerID: 1, HomeZone: 102, Rev: 5}}); err == nil ||
		!strings.Contains(err.Error(), "appendonly") || f.pipelineCount() != 1 {
		t.Fatalf("WAITAOF error reply = %v after %d pipelines, want one deterministic failure", err, f.pipelineCount())
	}
}

func TestPublishRoutes_RejectsInvalidRowsBeforeSending(t *testing.T) {
	rdb, f := newFakeRouteRedis(t, applyAll([]any{int64(1), int64(0)}))
	p := newTestRoutePublisher(rdb, 1, 0)
	// 依次:两列都没有(不发布墓碑)、rev 0、player_id 0、落点三列不自洽、同一名玩家出现两次。
	for _, rows := range [][]mergeRouteRow{
		{{PlayerID: 1, Rev: 1}},
		{{PlayerID: 1, HomeZone: 102}},
		{{PlayerID: 0, HomeZone: 102, Rev: 1}},
		{{PlayerID: 1, HomeZone: 102, PlacementVersion: 1, Rev: 1}},
		{{PlayerID: 1, HomeZone: 102, Rev: 1}, {PlayerID: 1, HomeZone: 102, Rev: 2}},
	} {
		if _, err := p.publishRoutes(context.Background(), rows); err == nil {
			t.Fatalf("publishRoutes(%+v) must be refused", rows)
		}
	}
	if f.pipelineCount() != 0 {
		t.Fatalf("invalid rows must never reach Redis, %d pipelines sent", f.pipelineCount())
	}
}

func TestPublishRoutes_SplitsIntoPipelinesOfAtMost500(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	rdb, f := newFakeRouteRedis(t, applyAll([]any{int64(1), int64(0)}))
	p := newTestRoutePublisher(rdb, 1, 0)
	rows := make([]mergeRouteRow, routePublishBatch+1)
	for i := range rows {
		rows[i] = mergeRouteRow{PlayerID: uint64(i + 1), HomeZone: 102, Rev: 1}
	}
	rep, err := p.publishRoutes(ctx, rows)
	if err != nil || rep.Applied != len(rows) {
		t.Fatalf("publishRoutes = (%s, %v)", rep, err)
	}
	if f.pipelineCount() != 2 || len(f.pipelines[0]) != routePublishBatch+1 || len(f.pipelines[1]) != 2 {
		t.Fatalf("pipelines = %d, sizes %d / %d", f.pipelineCount(), len(f.pipelines[0]), len(f.pipelines[1]))
	}
}

// ── catch-up ──────────────────────────────────────────────────────────────────

func TestDecideRouteCatchUp(t *testing.T) {
	row := mergeRouteRow{PlayerID: 9, HomeZone: 102, Rev: 5}
	for _, tc := range []struct {
		name           string
		home, plc, rev any
		want           bool
		wantErr        bool
	}{
		{"缓存三键都没有", nil, nil, nil, true, false},
		{"缓存rev落后", "102", nil, "4", true, false},
		{"同rev同值", "102", nil, "5", false, false},
		{"同rev异值", "103", nil, "5", true, false},
		{"缓存更新且同值", "102", nil, "6", false, false},
		{"缓存更新而值不同", "102", "5:1", "6", true, false},
		{"maprev有前导零", "102", nil, "05", false, true},
		{"元素类型不对", int64(102), nil, "5", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decideRouteCatchUp(row, tc.home, tc.plc, tc.rev)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("decideRouteCatchUp = (%v, %v), want (%v, err=%v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
	if _, err := decideRouteCatchUp(mergeRouteRow{PlayerID: 9, HomeZone: 102}, nil, nil, nil); err == nil {
		t.Fatal("a row with rev 0 must be refused")
	}
}

// TestCatchUpRoutes_PublishesOnlyLaggingRows:库 rev 领先或缓存没有 maprev 的行才发布并确认,已是这一版的行不发。
func TestCatchUpRoutes_PublishesOnlyLaggingRows(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	cache := map[string]any{"player:zone:1": "102", "player:maprev:1": "5", "player:zone:2": "101", "player:maprev:2": "3"}
	rdb, f := newFakeRouteRedis(t, func(_ int, cmd redis.Cmder) (any, error) {
		switch cmd.Name() {
		case "mget":
			var out []any
			for _, k := range cmd.Args()[1:] {
				out = append(out, cache[k.(string)])
			}
			return out, nil
		case "eval":
			rev, _, _ := evalArgs(cmd)
			return []any{int64(1), rev}, nil
		case "waitaof":
			return []any{int64(1), int64(0)}, nil
		}
		return nil, fmt.Errorf("unexpected %s", cmd.Name())
	})
	p := newTestRoutePublisher(rdb, 1, 0)
	rep, err := p.catchUpRoutes(ctx, []mergeRouteRow{
		{PlayerID: 1, HomeZone: 102, Rev: 5},
		{PlayerID: 2, HomeZone: 102, Rev: 4},
		{PlayerID: 3, HomeZone: 102, Rev: 1},
	})
	if err != nil {
		t.Fatalf("catchUpRoutes: %v", err)
	}
	if rep.Current != 1 || rep.Published.Rows != 2 || rep.Published.Applied != 2 {
		t.Fatalf("report = %+v", rep)
	}
	wantMGet := []any{"mget", "player:zone:1", "player:placement:1", "player:maprev:1", "player:zone:2", "player:placement:2",
		"player:maprev:2", "player:zone:3", "player:placement:3", "player:maprev:3"}
	if len(f.singles) != 1 || !reflect.DeepEqual(f.singles[0].Args(), wantMGet) {
		t.Fatalf("catch-up reads = %v", f.singles)
	}
	if f.pipelineCount() != 1 || !reflect.DeepEqual(f.names(0), []string{"eval", "eval", "waitaof"}) {
		t.Fatalf("catch-up publish = %d pipelines %v", f.pipelineCount(), f.names(0))
	}
	if got := f.pipelines[0][0].Args()[3]; got != "player:zone:2" {
		t.Fatalf("first published row = %v, want player 2", got)
	}
}

// ── 围栏镜像与合走标记镜像 ────────────────────────────────────────────────────

func TestRouteFenceHolderEpochIsStricterThanJSONNumber(t *testing.T) {
	if e, ok := routeFenceHolderEpoch(`{"run_id":"1-2-20261018T000000Z-0a1b2c3d","fence_epoch":7}`); !ok || e != 7 {
		t.Fatalf("epoch = (%d, %v)", e, ok)
	}
	for _, raw := range []string{`{"run_id":"x"}`, `{"fence_epoch":-1}`, `{"fence_epoch":1.5}`, `{"fence_epoch":"7"}`, `{"fence_epoch":null}`, `nope`, ``} {
		if _, ok := routeFenceHolderEpoch(raw); ok {
			t.Fatalf("routeFenceHolderEpoch(%q) ok, want not ok (the mirror script treats it as foreign)", raw)
		}
	}
}

func TestRouteFenceMirrorArgs(t *testing.T) {
	holder := `{"run_id":"1-2-20261018T000000Z-0a1b2c3d","fence_epoch":7}`
	keys, args, err := routeFenceMirrorArgs(2, holder, 7, 1000)
	if err != nil {
		t.Fatalf("routeFenceMirrorArgs: %v", err)
	}
	if !reflect.DeepEqual(keys, []string{"merge:in_progress:2"}) || !reflect.DeepEqual(args, []any{holder, "7", "601000"}) {
		t.Fatalf("keys = %v args = %v", keys, args)
	}
	for _, c := range []struct {
		zone       uint32
		holder     string
		epoch, exp uint64
	}{{0, holder, 7, 1000}, {2, holder, 8, 1000}, {2, `{"fence_epoch":0}`, 0, 1000}, {2, `{"run_id":"x"}`, 7, 1000}, {2, holder, 7, 0}} {
		if _, _, err := routeFenceMirrorArgs(c.zone, c.holder, c.epoch, c.exp); !errors.Is(err, errRouteInvalidArgument) {
			t.Fatalf("routeFenceMirrorArgs(%+v) error = %v", c, err)
		}
	}
}

func TestSetFenceMirror(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	holder := `{"run_id":"1-2-20261018T000000Z-0a1b2c3d","fence_epoch":7}`
	reply := int64(1)
	rdb, f := newFakeRouteRedis(t, func(_ int, cmd redis.Cmder) (any, error) {
		if cmd.Name() == "eval" {
			return reply, nil
		}
		return []any{int64(1), int64(0)}, nil
	})
	p := newTestRoutePublisher(rdb, 1, 0)
	if err := p.setFenceMirror(ctx, 2, holder, 7, 1000); err != nil {
		t.Fatalf("setFenceMirror: %v", err)
	}
	if !reflect.DeepEqual(f.names(0), []string{"eval", "waitaof"}) {
		t.Fatalf("pipeline = %v, want [eval waitaof]", f.names(0))
	}
	if got := f.pipelines[0][0].Args(); got[1] != routeFenceMirrorSetScript || got[3] != "merge:in_progress:2" || got[6] != "601000" {
		t.Fatalf("mirror EVAL args = %#v", got)
	}
	reply = 0
	if err := p.setFenceMirror(ctx, 2, holder, 7, 1000); !errors.Is(err, errRouteFenceMirrorStale) {
		t.Fatalf("stale error = %v", err)
	}
	reply = 2
	if err := p.setFenceMirror(ctx, 2, holder, 7, 1000); !errors.Is(err, errRouteFenceMirrorForeign) {
		t.Fatalf("foreign error = %v", err)
	}
	if f.pipelineCount() != 3 {
		t.Fatalf("stale / foreign must not be retried: %d pipelines", f.pipelineCount())
	}
}

func TestDeleteMirrorsCompareTheFullValue(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	deleted := int64(1)
	rdb, f := newFakeRouteRedis(t, func(_ int, cmd redis.Cmder) (any, error) { return deleted, nil })
	p := newTestRoutePublisher(rdb, 1, 0)
	holder := `{"run_id":"r","fence_epoch":3}`
	ok, err := p.deleteFenceMirror(ctx, 2, holder)
	if err != nil || !ok {
		t.Fatalf("deleteFenceMirror = (%v, %v)", ok, err)
	}
	if want := []any{"eval", routeCompareAndDeleteScript, 1, "merge:in_progress:2", holder}; !reflect.DeepEqual(f.singles[0].Args(), want) {
		t.Fatalf("CAD args = %#v", f.singles[0].Args())
	}
	deleted = 0
	value := `{"dst":102,"run_id":"r"}`
	if ok, err := p.deleteMergedIntoMirror(ctx, 101, value); err != nil || ok {
		t.Fatalf("deleteMergedIntoMirror on a changed value = (%v, %v), want (false, nil)", ok, err)
	}
	if got := f.singles[1].Args(); got[3] != "merge:merged_into:101" || got[4] != value {
		t.Fatalf("CAD args = %#v", got)
	}
}

func TestSetMergedIntoMirror(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	rdb, f := newFakeRouteRedis(t, func(_ int, cmd redis.Cmder) (any, error) {
		if cmd.Name() == "set" {
			return "OK", nil
		}
		return []any{int64(1), int64(0)}, nil
	})
	p := newTestRoutePublisher(rdb, 1, 0)
	value, err := encodeRouteMergedIntoValue(102, "101-102-20261018T000000Z-cafebabe")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := p.setMergedIntoMirror(ctx, 101, value); err != nil {
		t.Fatalf("setMergedIntoMirror: %v", err)
	}
	if !reflect.DeepEqual(f.names(0), []string{"set", "waitaof"}) || !reflect.DeepEqual(f.pipelines[0][0].Args(), []any{"set", "merge:merged_into:101", value}) {
		t.Fatalf("pipeline = %v args %#v", f.names(0), f.pipelines[0][0].Args())
	}
	for _, bad := range []string{`{"dst":101,"run_id":"r"}`, `{"dst":0,"run_id":"r"}`, `{"dst":102,"run_id":"bad run"}`, `nope`} {
		if err := p.setMergedIntoMirror(context.Background(), 101, bad); !errors.Is(err, errRouteInvalidArgument) {
			t.Fatalf("setMergedIntoMirror(%q) error = %v", bad, err)
		}
	}
}

// ── 水位键与成对核对 ──────────────────────────────────────────────────────────

func TestDecodeRouteWatermarkMatchesShared(t *testing.T) {
	const gen = "0123456789abcdef0123456789abcdef"
	w, err := decodeRouteWatermark(`{"v":1,"generation":"` + gen + `","watermark_ms":42}`)
	if err != nil || w.V != 1 || w.Generation != gen || w.WatermarkMs != 42 {
		t.Fatalf("decode = (%+v, %v)", w, err)
	}
	if _, err := decodeRouteWatermark(`{"v":2,"watermark_ms":"later"}`); !errors.Is(err, errRouteWatermarkNewerVersion) {
		t.Fatalf("newer version error = %v", err)
	}
	for name, raw := range map[string]string{
		"not json":  "x",
		"v0":        `{"v":0,"generation":"` + gen + `","watermark_ms":1}`,
		"no gen":    `{"v":1,"watermark_ms":1}`,
		"upper gen": `{"v":1,"generation":"AA","watermark_ms":1}`,
		"wm zero":   `{"v":1,"generation":"aa","watermark_ms":0}`,
		"wm string": `{"v":1,"generation":"aa","watermark_ms":"1"}`,
	} {
		if _, err := decodeRouteWatermark(raw); !errors.Is(err, errRouteWatermarkMalformed) {
			t.Fatalf("%s: error = %v, want errRouteWatermarkMalformed", name, err)
		}
	}
}

func TestDecideRoutePairing(t *testing.T) {
	const gen = "0123456789abcdef0123456789abcdef"
	wm := `{"v":1,"generation":"` + gen + `","watermark_ms":42}`
	for _, tc := range []struct {
		name    string
		source  string
		raw     string
		present bool
		ok      bool
	}{
		{"redis_没有水位键", routeSourceRedis, "", false, true},
		{"redis_见到水位键就拒绝", routeSourceRedis, wm, true, false},
		{"redis_畸形水位键也拒绝", routeSourceRedis, "x", true, false},
		{"db_成对", routeSourceDB, wm, true, true},
		{"db_没有水位键", routeSourceDB, "", false, false},
		{"db_世代不同", routeSourceDB, `{"v":1,"generation":"ff","watermark_ms":42}`, true, false},
		{"db_畸形", routeSourceDB, "x", true, false},
		{"db_新版本", routeSourceDB, `{"v":2}`, true, false},
		{"未知source", "mysql", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := decideRoutePairing(tc.source, tc.raw, tc.present, gen)
			if (err == nil) != tc.ok {
				t.Fatalf("decideRoutePairing = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestReadWatermark(t *testing.T) {
	var value any
	rdb, f := newFakeRouteRedis(t, func(_ int, cmd redis.Cmder) (any, error) {
		if value == nil {
			return nil, redis.Nil
		}
		return value, nil
	})
	p := newTestRoutePublisher(rdb, 1, 0)
	if raw, present, err := p.readWatermark(context.Background()); err != nil || present || raw != "" {
		t.Fatalf("absent watermark = (%q, %v, %v)", raw, present, err)
	}
	value = "{}"
	if raw, present, err := p.readWatermark(context.Background()); err != nil || !present || raw != "{}" {
		t.Fatalf("present watermark = (%q, %v, %v)", raw, present, err)
	}
	if got := f.singles[0].Args(); !reflect.DeepEqual(got, []any{"get", "mapping:route:watermark"}) {
		t.Fatalf("watermark read = %#v", got)
	}
}

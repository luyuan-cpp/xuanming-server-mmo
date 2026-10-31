package main

// route_publish.go 的单测。本 module 没有 miniredis,Redis 一侧用挂在真 *redis.Client 上的钩子(fakeRouteRedis)应答:
// 命令不出进程、不拨号,每条 pipeline 的命令按发送顺序记下 —— 「SET 与 WAITAOF 在同一条 pipeline 里」由此可断言。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
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

// ── 合走标记镜像 ──────────────────────────────────────────────────────────────

func TestDeleteMergedIntoMirrorComparesTheFullValue(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	deleted := int64(1)
	rdb, f := newFakeRouteRedis(t, func(_ int, cmd redis.Cmder) (any, error) { return deleted, nil })
	p := newTestRoutePublisher(rdb, 1, 0)
	value := `{"dst":102,"run_id":"r"}`
	ok, err := p.deleteMergedIntoMirror(ctx, 101, value)
	if err != nil || !ok {
		t.Fatalf("deleteMergedIntoMirror = (%v, %v)", ok, err)
	}
	if want := []any{"eval", routeCompareAndDeleteScript, 1, "merge:merged_into:101", value}; !reflect.DeepEqual(f.singles[0].Args(), want) {
		t.Fatalf("CAD args = %#v", f.singles[0].Args())
	}
	deleted = 0
	if ok, err := p.deleteMergedIntoMirror(ctx, 101, value); err != nil || ok {
		t.Fatalf("deleteMergedIntoMirror on a changed value = (%v, %v), want (false, nil)", ok, err)
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

// TestSetMergedIntoMirror_ResendsWhenWaitAOFIsShort:份数不足重发,确认之后才算写好。
func TestSetMergedIntoMirror_ResendsWhenWaitAOFIsShort(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	rdb, f := newFakeRouteRedis(t, func(pipeline int, cmd redis.Cmder) (any, error) {
		if cmd.Name() == "set" {
			return "OK", nil
		}
		if pipeline == 0 {
			return []any{int64(0), int64(0)}, nil
		}
		return []any{int64(1), int64(0)}, nil
	})
	p := newTestRoutePublisher(rdb, 1, 0)
	value, _ := encodeRouteMergedIntoValue(102, "r")
	if err := p.setMergedIntoMirror(ctx, 101, value); err != nil {
		t.Fatalf("setMergedIntoMirror: %v", err)
	}
	if f.pipelineCount() != 2 {
		t.Fatalf("pipelines = %d, want a resend after the short WAITAOF", f.pipelineCount())
	}
}

func TestSetMergedIntoMirror_GivesUpWhenNeverDurable(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	rdb, f := newFakeRouteRedis(t, func(_ int, cmd redis.Cmder) (any, error) {
		if cmd.Name() == "set" {
			return "OK", nil
		}
		return []any{int64(0), int64(0)}, nil
	})
	p := newTestRoutePublisher(rdb, 1, 0)
	value, _ := encodeRouteMergedIntoValue(102, "r")
	if err := p.setMergedIntoMirror(ctx, 101, value); !errors.Is(err, errRoutePublishNotDurable) {
		t.Fatalf("error = %v, want errRoutePublishNotDurable", err)
	}
	if f.pipelineCount() != routePublishAttempts {
		t.Fatalf("pipelines = %d, want %d", f.pipelineCount(), routePublishAttempts)
	}
}

// TestSetMergedIntoMirror_ErrorReplyIsNotRetried:appendonly 关着是确定性错误,重发没有意义。
func TestSetMergedIntoMirror_ErrorReplyIsNotRetried(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	rdb, f := newFakeRouteRedis(t, func(_ int, cmd redis.Cmder) (any, error) {
		if cmd.Name() == "set" {
			return "OK", nil
		}
		return nil, fakeRedisReplyError("ERR WAITAOF cannot be used when numlocal is set but appendonly is disabled")
	})
	p := newTestRoutePublisher(rdb, 1, 0)
	value, _ := encodeRouteMergedIntoValue(102, "r")
	if err := p.setMergedIntoMirror(ctx, 101, value); err == nil {
		t.Fatal("an error reply must fail the write")
	}
	if f.pipelineCount() != 1 {
		t.Fatalf("pipelines = %d, want 1 (no retry on a Redis error reply)", f.pipelineCount())
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
		raw     string
		present bool
		ok      bool
	}{
		{"成对", wm, true, true},
		{"没有水位键", "", false, false},
		{"世代不同", `{"v":1,"generation":"ff","watermark_ms":42}`, true, false},
		{"畸形", "x", true, false},
		{"新版本", `{"v":2}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := decideRoutePairing(tc.raw, tc.present, gen)
			if (err == nil) != tc.ok {
				t.Fatalf("decideRoutePairing = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && !errors.Is(err, errRoutePairing) {
				t.Fatalf("error = %v, want errRoutePairing", err)
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

package routetool

// rewarm(第 1b-2 批收尾,2026-10-19):miniredis + 假库 + 假钟,不连库、不真睡。
// 钉「一条 DEL 撤三把控制键、什么时候拒绝、等什么算重挂、超时与出错的退出码」;leader 怎么重挂由 routereconcile 的用例负责。

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"data_service/internal/store"

	"shared/placement"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// 编译期断言:装配层直接把 go-zero 的 *redis.Redis 与 *store.RouteStore 传进来。
var (
	_ rewarmRedis = (*redis.Redis)(nil)
	_ rewarmDB    = (*store.RouteStore)(nil)
)

// fakeRewarmDB 是 rewarmDB 的内存替身。
type fakeRewarmDB struct {
	meta       store.RouteMeta
	dbNowMs    uint64
	metaCalls  int
	clockCalls int
	// onClock 在读库时钟时执行(用来核对「库时钟在 DEL 之前读」)。
	onClock func()
}

func (f *fakeRewarmDB) ReadRouteMeta(context.Context) (store.RouteMeta, error) {
	f.metaCalls++
	return f.meta, nil
}

func (f *fakeRewarmDB) DBNowMs(context.Context) (uint64, error) {
	f.clockCalls++
	if f.onClock != nil {
		f.onClock()
	}
	return f.dbNowMs, nil
}

func writableRewarmDB(nowMs uint64) *fakeRewarmDB {
	return &fakeRewarmDB{meta: store.RouteMeta{Generation: testGeneration, Writable: 1}, dbNowMs: nowMs}
}

// fakeClock 是 Rewarm 的假钟:Sleep 记下时长并把 now 往前推,再调 onSleep(第几次睡)。
type fakeClock struct {
	now     time.Time
	slept   []time.Duration
	onSleep func(n int)
	sleepFn func(ctx context.Context, d time.Duration) error // 非 nil 时代替默认行为(注入出错)
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_760_000_000, 0)} }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if c.sleepFn != nil {
		return c.sleepFn(ctx, d)
	}
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	if c.onSleep != nil {
		c.onSleep(len(c.slept))
	}
	return nil
}

func runRewarm(t *testing.T, rd rewarmRedis, db rewarmDB, opts RewarmOptions, clock *fakeClock) (RewarmReport, cliRun, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	opts.Stdout, opts.Stderr = &out, &errOut
	if clock != nil {
		opts.Sleep, opts.Now = clock.Sleep, clock.Now
	}
	rep, err := Rewarm(context.Background(), rd, db, opts)
	return rep, cliRun{code: RewarmExitCode(rep, err), stdout: out.String(), stderr: errOut.String()}, err
}

// markerWithSetAt 编一份 set_at_ms = setAtMs 的合规标记(readyMarkerFor 的 set_at_ms 是 watermark + 1)。
func markerWithSetAt(t *testing.T, setAtMs uint64) string {
	t.Helper()
	return readyMarkerFor(t, testIdentity, testGeneration, setAtMs-1)
}

// presetRewarmKeys 写满三把控制键,外加一把 rewarm 不许碰的映射键。
func presetRewarmKeys(t *testing.T, mr *miniredis.Miniredis, marker string) {
	t.Helper()
	wm, err := placement.EncodeRouteWatermark(placement.RouteWatermark{Generation: testGeneration, WatermarkMs: 42})
	require.NoError(t, err)
	kv := map[string]string{
		placement.RouteWarmKey:      strings.Repeat("0a", 16),
		placement.RouteWatermarkKey: wm,
		placement.HomeZoneKey(1):    "1",
	}
	if marker != "" {
		kv[placement.RouteReadyKey] = marker
	}
	setKeys(t, mr, kv)
}

func assertControlKeys(t *testing.T, mr *miniredis.Miniredis, present bool) {
	t.Helper()
	for _, k := range []string{placement.RouteReadyKey, placement.RouteWarmKey, placement.RouteWatermarkKey} {
		assert.Equal(t, present, mr.Exists(k), k)
	}
}

// TestRewarm_DropsAllThreeKeysInOneDel:三把控制键在**同一条** DEL 里撤掉(规格 §13.1 第 4 条:不留「标记没了、令牌还在」之类的中间态),
// 除此之外不发任何写命令;不相干的映射键不动。键本来就不在、或标记不是 string 类型,也是一条 DEL、退出码 0(幂等)。
func TestRewarm_DropsAllThreeKeysInOneDel(t *testing.T) {
	for _, tc := range []struct {
		name        string
		preset      func(t *testing.T, mr *miniredis.Miniredis)
		wantDeleted int
		wantMarker  string
	}{
		{"三把都在", func(t *testing.T, mr *miniredis.Miniredis) {
			presetRewarmKeys(t, mr, markerWithSetAt(t, 1_760_000_000_000))
		}, 3, RewarmMarkerDecoded},
		{"都不在", func(*testing.T, *miniredis.Miniredis) {}, 0, MarkerAbsent},
		{"标记畸形", func(t *testing.T, mr *miniredis.Miniredis) { presetRewarmKeys(t, mr, "ready") }, 3, RewarmMarkerMalformed},
		{"标记不是string", func(t *testing.T, mr *miniredis.Miniredis) {
			presetRewarmKeys(t, mr, "")
			_, err := mr.SAdd(placement.RouteReadyKey, "x")
			require.NoError(t, err)
		}, 3, MarkerWrongType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr, rd := newMappingRedis(t)
			tc.preset(t, mr)
			var (
				mu     sync.Mutex
				dels   [][]string
				writes []string
			)
			mr.Server().SetPreHook(func(_ *server.Peer, cmd string, args ...string) bool {
				mu.Lock()
				defer mu.Unlock()
				switch cmd {
				case "DEL", "UNLINK":
					dels = append(dels, append([]string(nil), args...))
				case "SET", "SETNX", "SETEX", "GETDEL", "EVAL", "EVALSHA", "EXPIRE", "PEXPIRE", "FLUSHDB", "FLUSHALL":
					writes = append(writes, cmd)
				}
				return false
			})
			t.Cleanup(func() { mr.Server().SetPreHook(nil) })

			rep, run, err := runRewarm(t, rd, nil, RewarmOptions{}, nil)

			require.NoError(t, err, run.stderr)
			assert.Equal(t, ExitClean, run.code)
			mu.Lock()
			assert.Equal(t, [][]string{{placement.RouteReadyKey, placement.RouteWarmKey, placement.RouteWatermarkKey}}, dels, "恰好一条 DEL,三把键都在里面")
			assert.Empty(t, writes, "除了那条 DEL 不发任何写命令")
			mu.Unlock()
			assertControlKeys(t, mr, false)
			if tc.wantDeleted > 0 {
				assert.True(t, mr.Exists(placement.HomeZoneKey(1)), "映射键不归 rewarm 管")
			}
			assert.Equal(t, tc.wantDeleted, rep.Deleted)
			assert.Equal(t, tc.wantMarker, rep.MarkerWas)
			assert.Equal(t, "route_rewarm deleted="+strconv.Itoa(tc.wantDeleted)+" marker_was="+tc.wantMarker+
				" waited=false rearmed=false baseline_set_at_ms=0 new_set_at_ms=0\n", run.stdout)
			assert.True(t, strings.HasPrefix(run.stderr, RewarmNote+"\n"), "stderr 第一行是 NOTE")
		})
	}
}

// TestRewarm_WaitNeedsDBFirst:-wait > 0 而 yaml 是 redis 模式 → 退出码 2(没有 leader 重挂),一把键都不动、不读库;
// 同理在任何写之前拒绝的还有:映射写者冻结着(冻结期 leader 不预热)、标记是更新格式版本(重挂的标记也认不出)。
// redis 模式下 -wait=0 照常撤(回退 redis 模式时就用它)。
func TestRewarm_WaitNeedsDBFirst(t *testing.T) {
	marker := markerWithSetAt(t, 1_760_000_000_000)

	t.Run("redis模式_拒绝", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		presetRewarmKeys(t, mr, marker)
		db := writableRewarmDB(1)
		_, run, err := runRewarm(t, rd, db, RewarmOptions{Wait: time.Minute, Poll: time.Second, RedisMode: true}, newFakeClock())
		assert.ErrorIs(t, err, ErrRewarmNeedsDBFirst)
		assert.Equal(t, ExitFailed, run.code)
		assert.Empty(t, run.stdout)
		assert.Zero(t, db.metaCalls+db.clockCalls, "拒绝在连库之前")
		assertControlKeys(t, mr, true)
	})
	t.Run("冻结中_拒绝", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		presetRewarmKeys(t, mr, marker)
		db := writableRewarmDB(1)
		db.meta.Writable = 0
		_, run, err := runRewarm(t, rd, db, RewarmOptions{Wait: time.Minute, Poll: time.Second}, newFakeClock())
		assert.ErrorIs(t, err, ErrRewarmWritersFrozen)
		assert.Equal(t, ExitFailed, run.code)
		assert.Empty(t, run.stdout)
		assertControlKeys(t, mr, true)
	})
	t.Run("标记版本更新_等待时拒绝", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		presetRewarmKeys(t, mr, `{"v":2,"set_at_ms":"x"}`)
		_, run, err := runRewarm(t, rd, writableRewarmDB(1), RewarmOptions{Wait: time.Minute, Poll: time.Second}, newFakeClock())
		require.Error(t, err)
		assert.Equal(t, ExitFailed, run.code)
		assert.Contains(t, run.stderr, "MARKER newer_version")
		assertControlKeys(t, mr, true)
	})
	t.Run("redis模式_不等_照常撤", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		presetRewarmKeys(t, mr, marker)
		_, run, err := runRewarm(t, rd, nil, RewarmOptions{RedisMode: true}, nil)
		require.NoError(t, err, run.stderr)
		assert.Equal(t, ExitClean, run.code)
		assertControlKeys(t, mr, false)
	})
	t.Run("参数不对_退出码2", func(t *testing.T) {
		_, rd := newMappingRedis(t)
		for _, opts := range []RewarmOptions{
			{Wait: -time.Second},
			{Wait: time.Minute},                    // 没给 Poll
			{Wait: time.Minute, Poll: time.Second}, // 没给库
		} {
			_, run, err := runRewarm(t, rd, nil, opts, nil)
			assert.Error(t, err)
			assert.Equal(t, ExitFailed, run.code)
			assert.Empty(t, run.stdout)
		}
	})
}

// TestRewarm_PollWithInjectedSleeper:-wait > 0 时用注入的 Sleeper 轮询,只认「可解码且 set_at_ms 大于基线」的新标记。
//   - 被撤标记可解码 → 基线是它的 set_at_ms,不读库时钟;第一次睡醒时冒出来一份 set_at_ms 等于基线的旧标记(像是从旧 AOF 恢复的)
//     → 不算重挂(打 WARN);第二次睡醒时 leader 挂上 set_at_ms 更大的 → 退出码 0;
//   - 被撤时没有标记 → 基线是 DEL 之前读的库时钟(读的时候三把键都还在);比库时钟旧的标记不算。
func TestRewarm_PollWithInjectedSleeper(t *testing.T) {
	const oldSetAt = uint64(1_760_000_000_000)

	t.Run("基线取被撤标记", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		presetRewarmKeys(t, mr, markerWithSetAt(t, oldSetAt))
		db := writableRewarmDB(oldSetAt + 999_999) // 不该被读
		stale, fresh := markerWithSetAt(t, oldSetAt), markerWithSetAt(t, oldSetAt+5000)
		clock := newFakeClock()
		clock.onSleep = func(n int) {
			switch n {
			case 1:
				require.NoError(t, mr.Set(placement.RouteReadyKey, stale))
			case 2:
				require.NoError(t, mr.Set(placement.RouteReadyKey, fresh))
			}
		}

		rep, run, err := runRewarm(t, rd, db, RewarmOptions{Wait: time.Minute, Poll: 5 * time.Second}, clock)

		require.NoError(t, err, run.stderr)
		assert.Equal(t, ExitClean, run.code)
		assert.Equal(t, []time.Duration{5 * time.Second, 5 * time.Second}, clock.slept, "每次按 -poll 睡,第三次轮询看到新标记")
		assert.True(t, rep.Rearmed)
		assert.Equal(t, oldSetAt, rep.BaselineSetAtMs)
		assert.Equal(t, oldSetAt+5000, rep.NewSetAtMs)
		assert.Zero(t, db.clockCalls, "被撤标记可解码时不读库时钟")
		assert.Equal(t, 1, db.metaCalls, "等待之前核对一次冻结态")
		assert.Contains(t, run.stderr, "reappeared with set_at_ms=")
		assert.Equal(t, "route_rewarm deleted=3 marker_was=decoded waited=true rearmed=true baseline_set_at_ms=1760000000000 new_set_at_ms=1760000005000\n", run.stdout)
	})

	t.Run("被撤时没有标记_基线取库时钟", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		presetRewarmKeys(t, mr, "")
		const dbNow = uint64(2_000_000_000_000)
		db := writableRewarmDB(dbNow)
		warmAtClock := false
		db.onClock = func() { warmAtClock = mr.Exists(placement.RouteWarmKey) }
		older, fresh := markerWithSetAt(t, dbNow-1000), markerWithSetAt(t, dbNow+1)
		clock := newFakeClock()
		clock.onSleep = func(n int) {
			switch n {
			case 1:
				require.NoError(t, mr.Set(placement.RouteReadyKey, older))
			case 3:
				require.NoError(t, mr.Set(placement.RouteReadyKey, fresh))
			}
		}

		rep, run, err := runRewarm(t, rd, db, RewarmOptions{Wait: time.Minute, Poll: 2 * time.Second}, clock)

		require.NoError(t, err, run.stderr)
		assert.True(t, rep.Rearmed)
		assert.Equal(t, 1, db.clockCalls)
		assert.True(t, warmAtClock, "库时钟在 DEL 之前读")
		assert.Equal(t, dbNow, rep.BaselineSetAtMs)
		assert.Equal(t, dbNow+1, rep.NewSetAtMs)
		assert.Len(t, clock.slept, 3)
		assert.Equal(t, MarkerAbsent, rep.MarkerWas)
	})
}

// TestRewarm_TimeoutExitsOne:到 -wait 还没等到新标记 → 退出码 1,统计行照出(rearmed=false),stderr 指向 ArmReadyMarker、
// leader 指标与 arming_refused_* 事件;等待按单调截止时间算(最后一次只睡剩余的时长),键保持撤掉。
// 轮询 GET 出错不算失败(继续等到截止);ctx 结束(-timeout)按失败返回(退出码 2),stderr 写明键已撤掉。
func TestRewarm_TimeoutExitsOne(t *testing.T) {
	t.Run("等不到_退出码1", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		presetRewarmKeys(t, mr, markerWithSetAt(t, 1_760_000_000_000))
		clock := newFakeClock()

		rep, run, err := runRewarm(t, rd, writableRewarmDB(1), RewarmOptions{Wait: 12 * time.Second, Poll: 5 * time.Second}, clock)

		assert.ErrorIs(t, err, ErrRewarmNotRearmed)
		assert.Equal(t, ExitAttention, run.code)
		assert.False(t, rep.Rearmed)
		assert.Equal(t, []time.Duration{5 * time.Second, 5 * time.Second, 2 * time.Second}, clock.slept, "截止时间按 -wait 算,最后一次只睡剩余")
		assert.Equal(t, "route_rewarm deleted=3 marker_was=decoded waited=true rearmed=false baseline_set_at_ms=1760000000000 new_set_at_ms=0\n", run.stdout)
		for _, want := range []string{
			"TIMEOUT", "RouteStore.Reconcile.ArmReadyMarker", "data_service_route_reconcile_leader",
			"data_service_route_reconcile_arm_enabled", "arming_refused_",
		} {
			assert.Contains(t, run.stderr, want)
		}
		assertControlKeys(t, mr, false)
	})

	t.Run("轮询GET出错_不算失败_到期退出码1", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		presetRewarmKeys(t, mr, markerWithSetAt(t, 1_760_000_000_000))
		flaky := &flakyAfterDelRedis{rewarmRedis: rd}

		_, run, err := runRewarm(t, flaky, writableRewarmDB(1), RewarmOptions{Wait: 10 * time.Second, Poll: 5 * time.Second}, newFakeClock())

		assert.ErrorIs(t, err, ErrRewarmNotRearmed)
		assert.Equal(t, ExitAttention, run.code)
		assert.Contains(t, run.stderr, "WARN: poll GET")
		assert.Contains(t, run.stderr, "GET errors")
		assert.Equal(t, 1, strings.Count(run.stderr, "WARN: poll GET"), "只打第一次")
	})

	t.Run("ctx结束_退出码2", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		presetRewarmKeys(t, mr, markerWithSetAt(t, 1_760_000_000_000))
		clock := newFakeClock()
		clock.sleepFn = func(context.Context, time.Duration) error { return context.DeadlineExceeded }

		_, run, err := runRewarm(t, rd, writableRewarmDB(1), RewarmOptions{Wait: time.Minute, Poll: time.Second}, clock)

		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.False(t, errors.Is(err, ErrRewarmNotRearmed))
		assert.Equal(t, ExitFailed, run.code)
		assert.Empty(t, run.stdout)
		assert.Contains(t, run.stderr, "keys ARE dropped")
		assertControlKeys(t, mr, false)
	})
}

// flakyAfterDelRedis 在 DEL 之后的每次 GET 都报错(模拟轮询期间 Redis 抖动)。
type flakyAfterDelRedis struct {
	rewarmRedis
	deleted bool
}

func (f *flakyAfterDelRedis) DelCtx(ctx context.Context, keys ...string) (int, error) {
	f.deleted = true
	return f.rewarmRedis.DelCtx(ctx, keys...)
}

func (f *flakyAfterDelRedis) GetCtx(ctx context.Context, key string) (string, error) {
	if f.deleted {
		return "", errors.New("LOADING Redis is loading the dataset in memory")
	}
	return f.rewarmRedis.GetCtx(ctx, key)
}

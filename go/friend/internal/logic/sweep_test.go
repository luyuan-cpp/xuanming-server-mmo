package logic

// sweep_test.go —— 周期清理的 **ticker 侧**回归(handoff §3 第 6c 条)。
//
// # 这一层该被测什么
//
// sweep.go 文件头的职责分界写明:本层只管节拍、单轮预算、指标与日志;模式判定、LIMIT、
// "updated_ms 恒为 0 就拒删"那道保险全在 SQL 侧(data/sweep_repo.go,由 sweep_repo_test.go
// 在真 MySQL 上覆盖)。所以这里不需要任何库:用假 SweepStore 直接驱动 runSweepRound,钉住
//
//	① 配置与固定时钟**逐字**喂给 SQL 侧 —— 传错一个参数(比如把 BatchLimit 传进 retentionDays)
//	   编译器看不出来,四个都是整数;
//	② 两段互不影响:前一段失败,后一段照跑(sweep.go 里 runSweepRound 的注释解释了为什么);
//	③ Gauge 的刷新纪律:合法模式每轮都刷(含 0),未知模式一条序列都不许造;
//	④ 单轮预算的取值,以及"不启动"的两个分支不 panic。
//
// # 观测面
//
// 调用行为看假 store 的记录;Gauge 看 Prometheus 的 DefaultGatherer(复用 friend_logic_test.go 的
// ensureMetricsRegistered / metricValue)—— 与线上看板读的是同一个面。
// 日志不作断言面:文案不是契约,钉住它只会让改措辞变成改测试。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"friend/internal/config"
	"friend/internal/metrics"
	"friend/internal/svc"
)

// ── 假 SweepStore ───────────────────────────────────────────────

const (
	sweepMethodTerminal = "SweepTerminalRequests"
	sweepMethodIdle     = "SweepIdleCapacityRows"
)

// sweepCall 是假 store 记下的一次调用:全部入参,外加调用那一刻 ctx 的形状。
type sweepCall struct {
	method        string
	mode          string
	retentionDays int
	batchLimit    int
	nowMs         int64

	// ctxErr 是**进入方法那一刻**的 ctx.Err()。runSweepRound 返回时会 cancel 单轮 ctx,
	// 事后再去读恒为 Canceled,看不出调用当时的状态,所以必须当场抄下来。
	ctxErr      error
	deadline    time.Time
	hasDeadline bool
}

// sweepReturn 是假 store 某个方法的预置返回值。
type sweepReturn struct {
	seen    int64
	deleted int64
	err     error
}

// fakeSweepStore 实现 SweepStore。
//
// 加锁:与 fakeFriendStore 不同,StartSweep 的用例里它是被后台 goroutine 调用的,
// 测试 goroutine 同时在读调用记录,不加锁就是一个真实的 data race。
type fakeSweepStore struct {
	mu       sync.Mutex
	calls    []sweepCall
	terminal sweepReturn
	idle     sweepReturn
}

var _ SweepStore = (*fakeSweepStore)(nil)

func (f *fakeSweepStore) record(ctx context.Context, method, mode string, retentionDays, batchLimit int, nowMs int64) {
	deadline, hasDeadline := ctx.Deadline()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sweepCall{
		method: method, mode: mode, retentionDays: retentionDays, batchLimit: batchLimit, nowMs: nowMs,
		ctxErr: ctx.Err(), deadline: deadline, hasDeadline: hasDeadline,
	})
}

func (f *fakeSweepStore) SweepTerminalRequests(ctx context.Context, mode string, retentionDays, batchLimit int, nowMs int64) (int64, int64, error) {
	f.record(ctx, sweepMethodTerminal, mode, retentionDays, batchLimit, nowMs)
	return f.terminal.seen, f.terminal.deleted, f.terminal.err
}

func (f *fakeSweepStore) SweepIdleCapacityRows(ctx context.Context, mode string, retentionDays, batchLimit int, nowMs int64) (int64, int64, error) {
	f.record(ctx, sweepMethodIdle, mode, retentionDays, batchLimit, nowMs)
	return f.idle.seen, f.idle.deleted, f.idle.err
}

// snapshot 返回调用记录的拷贝(后台 goroutine 可能还在 append)。
func (f *fakeSweepStore) snapshot() []sweepCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sweepCall(nil), f.calls...)
}

// ── 夹具 ────────────────────────────────────────────────────────

// testSweepConf 刻意让三个整数**两两不同**,也都不等于 testFriendConf 的默认值:
// 四个入参里有三个是同类型的整数,取值一样的话"传串了位置"测不出来。
func testSweepConf(mode string) config.SweepConf {
	return config.SweepConf{
		Mode:          mode,
		Interval:      5 * time.Minute,
		RetentionDays: 13,
		BatchLimit:    321,
	}
}

// newSweepDeps 手工构造只够 sweep 用的 Deps:sweep.go 只读 SvcCtx.Config、Sweeps、Now 三样,
// 所以不起 miniredis、不装 Repo / Sessions(newFixture 那一套是给请求路径用的)。
//
// sweeps 的形参类型是接口而不是 *fakeSweepStore:要测"未装配"分支时传无类型的 nil,
// 得到的才是真正的 nil 接口;传一个 (*fakeSweepStore)(nil) 进去,`deps.Sweeps == nil` 会是 false。
func newSweepDeps(sweeps SweepStore, sweep config.SweepConf) *Deps {
	cfg := testConfig()
	cfg.Friend.Sweep = sweep
	return &Deps{
		SvcCtx: &svc.ServiceContext{Config: cfg},
		Sweeps: sweeps,
		Now:    func() time.Time { return time.UnixMilli(testNowMs) },
	}
}

const (
	sweepPendingGaugeName = "friend_sweep_pending_rows"
	sweepIdleGaugeName    = "friend_sweep_idle_capacity_rows"
)

// sweepGauge 读某个 sweep Gauge 在给定 mode 下的值;found=false 表示这条序列不存在。
func sweepGauge(t *testing.T, name, mode string) (float64, bool) {
	t.Helper()
	return metricValue(t, name, map[string]string{"mode": mode})
}

// sweepGaugeSentinel 是预置进 Gauge 的哨兵值:Gauge 是进程级全局量,别的用例可能已经把它刷成
// 任意值(包括 0),不先写一个不可能来自本用例返回值的数,"被刷成 0"与"根本没刷"就分不开。
const sweepGaugeSentinel = 987654

func presetSweepGauges(mode string) {
	metrics.SetSweepPendingRows(mode, sweepGaugeSentinel)
	metrics.SetSweepIdleCapacityRows(mode, sweepGaugeSentinel)
}

// ── ① 入参逐字透传 + ③ 合法模式每轮都刷 Gauge ─────────────────────

// TestSweepRoundFeedsConfigAndClockVerbatim:两段都被调用、先终态申请后容量行,
// 且 mode / retentionDays / batchLimit / nowMs 与配置、固定时钟逐字一致。
func TestSweepRoundFeedsConfigAndClockVerbatim(t *testing.T) {
	ensureMetricsRegistered(t)

	for _, mode := range []string{config.SweepModeReportOnly, config.SweepModeDelete} {
		t.Run(mode, func(t *testing.T) {
			store := &fakeSweepStore{
				terminal: sweepReturn{seen: 7, deleted: 0},
				idle:     sweepReturn{seen: 5, deleted: 0},
			}
			sweep := testSweepConf(mode)
			deps := newSweepDeps(store, sweep)
			presetSweepGauges(mode)

			deps.runSweepRound(context.Background())

			calls := store.snapshot()
			require.Len(t, calls, 2, "一轮必须恰好调用两个 Sweep 方法各一次")
			assert.Equal(t, sweepMethodTerminal, calls[0].method, "第一段是终态好友申请")
			assert.Equal(t, sweepMethodIdle, calls[1].method, "第二段是零好友容量行")
			for _, c := range calls {
				assert.Equal(t, sweep.Mode, c.mode, "%s 的 mode", c.method)
				assert.Equal(t, sweep.RetentionDays, c.retentionDays, "%s 的 retentionDays", c.method)
				assert.Equal(t, sweep.BatchLimit, c.batchLimit, "%s 的 batchLimit", c.method)
				assert.Equal(t, testNowMs, c.nowMs, "%s 的 nowMs 必须取 Deps.Now,不许自己读墙钟", c.method)
				assert.NoError(t, c.ctxErr, "%s 拿到的 ctx 不应已经结束", c.method)
			}

			pending, ok := sweepGauge(t, sweepPendingGaugeName, mode)
			require.True(t, ok)
			assert.EqualValues(t, 7, pending)
			idle, ok := sweepGauge(t, sweepIdleGaugeName, mode)
			require.True(t, ok)
			assert.EqualValues(t, 5, idle)
		})
	}
}

// TestSweepRoundRefreshesGaugesWithZero:两段都什么也没看到时 Gauge 仍要被刷成 0。
//
// "长期不更新"是"sweep 根本没在跑"的唯一信号(sweep.go 里 Set 旁的注释);只在 >0 时才刷,
// 会让"没有积压"与"循环死了"在看板上长得一样,而且上一轮留下的非零值会一直挂着。
func TestSweepRoundRefreshesGaugesWithZero(t *testing.T) {
	ensureMetricsRegistered(t)

	for _, mode := range []string{config.SweepModeReportOnly, config.SweepModeDelete} {
		t.Run(mode, func(t *testing.T) {
			store := &fakeSweepStore{} // 两段都返回 (0, 0, nil)
			deps := newSweepDeps(store, testSweepConf(mode))
			presetSweepGauges(mode)

			deps.runSweepRound(context.Background())

			pending, ok := sweepGauge(t, sweepPendingGaugeName, mode)
			require.True(t, ok)
			assert.Zero(t, pending, "终态申请的 Gauge 必须被刷成 0,而不是留着上一轮的值")
			idle, ok := sweepGauge(t, sweepIdleGaugeName, mode)
			require.True(t, ok)
			assert.Zero(t, idle, "容量行的 Gauge 必须被刷成 0,而不是留着上一轮的值")
		})
	}
}

// TestSweepRoundSharesOneBudget:两段共用**同一个**单轮预算,而不是各拿一份。
//
// 各拿一份的话一轮的最坏耗时翻倍,会越过 ticker 间隔、两轮叠在一起压库 ——
// 这正是 sweepRoundBudget 要防的事。用截止时刻相等来断言,不比较耗时,所以不依赖墙钟快慢。
func TestSweepRoundSharesOneBudget(t *testing.T) {
	store := &fakeSweepStore{}
	sweep := testSweepConf(config.SweepModeReportOnly)
	deps := newSweepDeps(store, sweep)

	deps.runSweepRound(context.Background())

	calls := store.snapshot()
	require.Len(t, calls, 2)
	require.True(t, calls[0].hasDeadline, "单轮 ctx 必须带截止时刻,否则一条慢 DELETE 能占连接到下一轮")
	require.True(t, calls[1].hasDeadline)
	assert.True(t, calls[0].deadline.Equal(calls[1].deadline), "两段的截止时刻必须是同一个")
	// 只断言上界:截止时刻 = 进入 runSweepRound 的时刻 + 预算,此刻离它只会更近。
	assert.LessOrEqual(t, time.Until(calls[0].deadline), sweepRoundTimeout(sweep.Interval))
}

// ── ② 两段互不影响 ──────────────────────────────────────────────

// TestSweepRoundRunsIdleCapacityEvenWhenTerminalFails:前一段返回 error,后一段仍然执行。
//
// 回归的是引入第二段之前 runSweepRound 里那处 `return`:原样保留的话,friend_request 上的
// 一条慢语句 / 一次连接故障会连带停掉 friend_capacity 的回收,而后者的增长可由客户端驱动。
func TestSweepRoundRunsIdleCapacityEvenWhenTerminalFails(t *testing.T) {
	ensureMetricsRegistered(t)
	const mode = config.SweepModeDelete

	store := &fakeSweepStore{
		terminal: sweepReturn{err: errors.New("模拟:friend_request 清理失败")},
		idle:     sweepReturn{seen: 4, deleted: 4},
	}
	deps := newSweepDeps(store, testSweepConf(mode))
	presetSweepGauges(mode)

	deps.runSweepRound(context.Background())

	calls := store.snapshot()
	require.Len(t, calls, 2, "前一段失败不得跳过后一段")
	assert.Equal(t, sweepMethodTerminal, calls[0].method)
	assert.Equal(t, sweepMethodIdle, calls[1].method)

	// 失败的那一段不刷 Gauge:它没有拿到可信的数字,刷 0 会把"失败"伪装成"没有积压"。
	pending, ok := sweepGauge(t, sweepPendingGaugeName, mode)
	require.True(t, ok)
	assert.EqualValues(t, sweepGaugeSentinel, pending, "失败的一段不许拿零值刷 Gauge")
	// 后一段不只是"被调用了",它的结果也照常进了指标。
	idle, ok := sweepGauge(t, sweepIdleGaugeName, mode)
	require.True(t, ok)
	assert.EqualValues(t, 4, idle)
}

// TestSweepRoundIdleCapacityFailureIsContained:后一段失败同样只丢它自己 ——
// 不 panic、不刷自己的 Gauge,也不回头影响前一段已经刷好的数字。
func TestSweepRoundIdleCapacityFailureIsContained(t *testing.T) {
	ensureMetricsRegistered(t)
	const mode = config.SweepModeReportOnly

	store := &fakeSweepStore{
		terminal: sweepReturn{seen: 9},
		idle:     sweepReturn{err: errors.New("模拟:friend_capacity 回收失败")},
	}
	deps := newSweepDeps(store, testSweepConf(mode))
	presetSweepGauges(mode)

	require.NotPanics(t, func() { deps.runSweepRound(context.Background()) })

	require.Len(t, store.snapshot(), 2)
	pending, ok := sweepGauge(t, sweepPendingGaugeName, mode)
	require.True(t, ok)
	assert.EqualValues(t, 9, pending)
	idle, ok := sweepGauge(t, sweepIdleGaugeName, mode)
	require.True(t, ok)
	assert.EqualValues(t, sweepGaugeSentinel, idle, "失败的一段不许拿零值刷 Gauge")
}

// ── ③ 未知模式 ──────────────────────────────────────────────────

// TestSweepRoundUnknownModeNeverBecomesAGaugeLabel:配置里拼错的 mode **不得**成为 Gauge 的 label。
//
// mode label 是 metrics 包声明的有限枚举;拿拼错的串去 WithLabelValues 会凭空造出一条
// 没人认识的序列,而两条合法序列从此不再更新 —— 看板上既像"没有积压"又像"sweep 停了"。
//
// 现行实现在未知模式下**仍然调用**两个 store 方法(SQL 侧的契约是未知模式一行不删,见
// SweepStore 的注释),这里按现行行为钉住:入参原样透传,由 SQL 侧而不是 ticker 侧做模式判定。
// 取值用大小写不符的 "Delete":它是最像合法值的错法,也正是契约点名的一种。
func TestSweepRoundUnknownModeNeverBecomesAGaugeLabel(t *testing.T) {
	ensureMetricsRegistered(t)
	const typo = "Delete"
	require.False(t, isKnownSweepMode(typo))

	store := &fakeSweepStore{
		terminal: sweepReturn{seen: 3},
		idle:     sweepReturn{seen: 2},
	}
	deps := newSweepDeps(store, testSweepConf(typo))
	presetSweepGauges(config.SweepModeReportOnly)
	presetSweepGauges(config.SweepModeDelete)

	deps.runSweepRound(context.Background())

	calls := store.snapshot()
	require.Len(t, calls, 2, "未知模式下两段照常交给 SQL 侧(它只数不删)")
	for _, c := range calls {
		assert.Equal(t, typo, c.mode, "%s:mode 必须原样透传,ticker 侧不许替 SQL 侧改写或兜底成某个合法值", c.method)
	}

	for _, name := range []string{sweepPendingGaugeName, sweepIdleGaugeName} {
		_, found := sweepGauge(t, name, typo)
		assert.False(t, found, "%s 不得出现 mode=%q 的序列", name, typo)
		// 也不许"好心"地记到某个合法 mode 头上。
		for _, legal := range []string{config.SweepModeReportOnly, config.SweepModeDelete} {
			v, ok := sweepGauge(t, name, legal)
			require.True(t, ok)
			assert.EqualValues(t, sweepGaugeSentinel, v, "%s{mode=%q} 不应被未知模式的这一轮改写", name, legal)
		}
	}
}

func TestIsKnownSweepMode(t *testing.T) {
	assert.True(t, isKnownSweepMode(config.SweepModeReportOnly))
	assert.True(t, isKnownSweepMode(config.SweepModeDelete))
	for _, mode := range []string{"", "Delete", "REPORT_ONLY", "report-only", " delete"} {
		assert.False(t, isKnownSweepMode(mode), "mode=%q", mode)
	}
}

// ── ④ 单轮预算 ──────────────────────────────────────────────────

// TestSweepRoundTimeout:单轮预算 = min(Interval, sweepRoundBudget)。
// 非正的 interval 走不到这里(StartSweep 先拒掉),但函数自己也不许因此给出非正的超时 ——
// 那会让每一轮一进来就超时,表现为"sweep 在跑却永远清不掉任何东西"。
func TestSweepRoundTimeout(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"interval 小于预算:取 interval,跑得比节拍慢就会两轮叠在一起", sweepRoundBudget - time.Second, sweepRoundBudget - time.Second},
		{"interval 极短", time.Millisecond, time.Millisecond},
		{"interval 等于预算", sweepRoundBudget, sweepRoundBudget},
		{"interval 大于预算:封顶在预算", sweepRoundBudget + time.Second, sweepRoundBudget},
		{"生产默认 5 分钟", 5 * time.Minute, sweepRoundBudget},
		{"interval 为 0:回落到预算", 0, sweepRoundBudget},
		{"interval 为负:回落到预算", -time.Second, sweepRoundBudget},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sweepRoundTimeout(tc.interval))
		})
	}
}

// ── ④ StartSweep 的启动与退出 ───────────────────────────────────

// TestStartSweepDoesNotStartWithoutStore:Sweeps 未装配时不启动,也不 panic。
// 漏掉这个判空的话,第一轮就是对 nil 接口的方法调用 —— safego 会兜住它,然后每个节拍再炸一次。
func TestStartSweepDoesNotStartWithoutStore(t *testing.T) {
	deps := newSweepDeps(nil, testSweepConf(config.SweepModeReportOnly))
	require.Nil(t, deps.Sweeps, "夹具前提:这里必须是真正的 nil 接口")

	assert.NotPanics(t, func() { StartSweep(context.Background(), deps) })
}

// TestStartSweepDoesNotStartWithNonPositiveInterval:Interval <= 0 时不启动。
// 这个分支在起任何 goroutine **之前**同步返回,所以"store 一次都没被调用"在这里是确定的,
// 不需要等待。(0 间隔真的走下去,rand.Int64N(0) 与 time.NewTicker(0) 都会 panic。)
func TestStartSweepDoesNotStartWithNonPositiveInterval(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		store := &fakeSweepStore{}
		sweep := testSweepConf(config.SweepModeReportOnly)
		sweep.Interval = interval
		deps := newSweepDeps(store, sweep)

		assert.NotPanics(t, func() { StartSweep(context.Background(), deps) }, "interval=%v", interval)
		assert.Empty(t, store.snapshot(), "interval=%v 时不应启动循环", interval)
	}
}

// TestStartSweepTicksThenExitsOnCancel:活 ctx 先证明节拍在跑,再取消证明循环退出。
//
// StartSweep 是 fire-and-forget,没有"已退出"的句柄,只能从假 store 侧观察。用 1ms 的 Interval
// + 一个**活着的** ctx 起循环,分两段断言。(不能一上来就传已取消的 ctx:抖动那个 select 会直接
// return,safego.Loop 根本不启动,store 恒为空,后面所有断言都是对空切片的空转 —— 把 Loop 的 ctx
// 换成 Background、删掉回调里的 runSweepRound、抖动后忘了进 Loop,三种改坏全都测不出来。)
//
//  1. 正向的一段:调用记录会**长出来**,两种方法都出现,入参与配置逐字一致且带截止时刻 ——
//     证明走的确实是 抖动 → ticker → runSweepRound → 两段。
//     这一段**不**断言 ctxErr 为 nil:Interval=1ms 时单轮预算也只有 1ms,而 record 是拿到锁之后
//     才求 ctx.Err() 的,慢机器 / GC / -race 下合法地会读到 DeadlineExceeded。
//  2. 退出的一段:cancel 之后调用次数会**停止增长**。循环没退出的话(比如 Loop 被传了
//     context.Background()),1ms 的节拍下每个 50ms 观察窗都会多出一批调用,永远等不到
//     "连续两次读数相同"。select 在 ctx.Done 与 ticker 同时就绪时是随机挑的,所以取消之后
//     **允许**再漏跑有限几轮(次数不作断言),但这些轮次拿到的必须是已结束的 ctx ——
//     SQL 侧会立刻因 ctx 到期返回,漏跑才是无害的。
//
// 墙钟在这里只作等待上限(Eventually 的 2s、观察窗合计 500ms),不参与判定:机器再慢也只是
// 等得久一点,不会把对的实现判成错的,仍符合 AGENTS.md §11.4 对确定性的要求。
//
// 这条用例必须排在文件**最后**:它等 settled 之后才返回,后台轮次对 report_only Gauge 的改写
// 因此不会波及其它用例。
func TestStartSweepTicksThenExitsOnCancel(t *testing.T) {
	store := &fakeSweepStore{}
	sweep := testSweepConf(config.SweepModeReportOnly)
	sweep.Interval = time.Millisecond
	deps := newSweepDeps(store, sweep)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NotPanics(t, func() { StartSweep(ctx, deps) })

	// ── 正向路径 ──
	require.Eventually(t, func() bool { return len(store.snapshot()) >= 2 },
		2*time.Second, 5*time.Millisecond, "StartSweep 起来后必须按节拍调用 store")

	seenMethods := map[string]bool{}
	for _, c := range store.snapshot() {
		seenMethods[c.method] = true
		assert.Equal(t, sweep.Mode, c.mode, "%s 的 mode", c.method)
		assert.Equal(t, sweep.RetentionDays, c.retentionDays, "%s 的 retentionDays", c.method)
		assert.Equal(t, sweep.BatchLimit, c.batchLimit, "%s 的 batchLimit", c.method)
		assert.Equal(t, testNowMs, c.nowMs, "%s 的 nowMs", c.method)
		assert.True(t, c.hasDeadline, "%s:后台轮次同样必须带单轮截止时刻", c.method)
	}
	assert.True(t, seenMethods[sweepMethodTerminal], "节拍里必须跑到终态申请这一段")
	assert.True(t, seenMethods[sweepMethodIdle], "节拍里必须跑到容量行这一段")

	// ── 退出路径 ──
	// 顺序不能反:record 在锁内求 ctx.Err(),snapshot 持同一把锁;先 cancel 再取 n,
	// 才能保证下标 >= n 的调用一定是在 cancel 返回之后求的 Err。
	cancel()
	n := len(store.snapshot())

	const (
		observeWindow = 50 * time.Millisecond
		maxWindows    = 10
	)
	settled := false
	prev := n
	for i := 0; i < maxWindows && !settled; i++ {
		time.Sleep(observeWindow)
		cur := len(store.snapshot())
		settled = cur == prev
		prev = cur
	}
	assert.True(t, settled, "ctx 已取消,sweep 循环却仍在按节拍调用 store(最后一次读数 %d 次)", prev)

	for _, c := range store.snapshot()[n:] {
		assert.Error(t, c.ctxErr, "%s:取消之后漏跑的轮次拿到的必须是已结束的 ctx", c.method)
	}
}

package assetop

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// 事务基座(WithTxRetry / WithTxRetryConfig)的单测。
//
// 为什么不连库:这里要验的是**接缝**——开事务时用的是哪个隔离级、两次尝试之间退避了几次、
// 退避被取消时怎么收场。这三件事全在本包的控制流里,连真库只会把它们藏进 I/O 噪声,
// 还会让 CI 依赖一个外部 MySQL。真库上的锁序与守卫仍由 seq_integration_test.go 负责。
//
// 用 sql.OpenDB + 自定义 Connector 而不是 sql.Register:注册是全局的,名字撞了就 panic,
// 而且会让用例之间共享状态;Connector 每个用例一份,天然隔离(AGENTS §11.4)。

// fakeTxRecorder 记录驱动层实际收到的事务选项。database/sql 只有在驱动实现了
// driver.ConnBeginTx 时才会把隔离级传下来,所以这里必须实现它——否则非默认隔离级
// 会被 database/sql 直接拒掉,测试也就验不到真正下发的值。
type fakeTxRecorder struct {
	mu       sync.Mutex
	begins   []driver.TxOptions
	beginErr error
}

// record 记下这次开事务用的选项,并回报是否该让它失败。两件事合在一把锁里做,
// 免得用例在「已记账、还没读到错误」的半截状态上撞见竞态。
func (r *fakeTxRecorder) record(opts driver.TxOptions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.begins = append(r.begins, opts)
	return r.beginErr
}

func (r *fakeTxRecorder) options() []driver.TxOptions {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]driver.TxOptions, len(r.begins))
	copy(out, r.begins)
	return out
}

func (r *fakeTxRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.begins)
}

// setBeginErr 让用例模拟「连事务都开不起来」。
func (r *fakeTxRecorder) setBeginErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beginErr = err
}

type fakeTxDriver struct{ rec *fakeTxRecorder }

func (d fakeTxDriver) Open(string) (driver.Conn, error) { return &fakeTxConn{rec: d.rec}, nil }

type fakeTxConnector struct{ rec *fakeTxRecorder }

func (c fakeTxConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeTxConn{rec: c.rec}, nil
}

func (c fakeTxConnector) Driver() driver.Driver { return fakeTxDriver{rec: c.rec} }

type fakeTxConn struct{ rec *fakeTxRecorder }

// Prepare 刻意报错:本文件的用例都不执行语句,真要有人加了语句,应当一眼看见而不是静默通过。
func (c *fakeTxConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("假驱动不支持语句")
}

func (c *fakeTxConn) Close() error { return nil }

func (c *fakeTxConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *fakeTxConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.rec.record(opts); err != nil {
		return nil, err
	}
	return fakeTxHandle{}, nil
}

type fakeTxHandle struct{}

func (fakeTxHandle) Commit() error   { return nil }
func (fakeTxHandle) Rollback() error { return nil }

func newFakeTxDB(t *testing.T) (*sql.DB, *fakeTxRecorder) {
	t.Helper()
	rec := &fakeTxRecorder{}
	db := sql.OpenDB(fakeTxConnector{rec: rec})
	t.Cleanup(func() { _ = db.Close() })
	return db, rec
}

// TestWithTxRetryIsolationSeam:默认必须是 READ COMMITTED,且调用方能改。
//
// 默认值是契约的一部分:90-consistency part2 §2 规则 2/6(D7)要求帮会所有写事务——含经
// WithTxRetry 的后台写——统一 RC,而规则 6 的调用形式里没有隔离级参数。默认值一旦退回
// 「跟随连接默认」,这条规则在代码里就没有落脚点,而且同一份资产代码会随 DSN 改变语义。
func TestWithTxRetryIsolationSeam(t *testing.T) {
	t.Run("默认 READ COMMITTED", func(t *testing.T) {
		db, rec := newFakeTxDB(t)
		if err := WithTxRetry(context.Background(), db, 1, nil, func(*sql.Tx) error { return nil }); err != nil {
			t.Fatalf("事务应成功: %v", err)
		}
		got := rec.options()
		if len(got) != 1 {
			t.Fatalf("应只开一次事务,实际 %d 次", len(got))
		}
		if got[0].Isolation != driver.IsolationLevel(sql.LevelReadCommitted) {
			t.Fatalf("默认隔离级应为 READ COMMITTED,实际 %d", got[0].Isolation)
		}
	})

	t.Run("调用方可退回连接默认", func(t *testing.T) {
		db, rec := newFakeTxDB(t)
		cfg := DefaultTxRetryConfig()
		cfg.Attempts = 1
		cfg.Isolation = sql.LevelDefault
		if err := WithTxRetryConfig(context.Background(), db, cfg, func(*sql.Tx) error { return nil }); err != nil {
			t.Fatalf("事务应成功: %v", err)
		}
		got := rec.options()
		if len(got) != 1 || got[0].Isolation != driver.IsolationLevel(sql.LevelDefault) {
			t.Fatalf("显式 LevelDefault 应原样下发,实际 %+v", got)
		}
	})
}

// TestWithTxRetryBackoff:两次尝试之间必须退避,且只在「还有下一次」时退避。
//
// 没有退避的重试在死锁上几乎等于没有重试:被回滚的一方立刻按同样锁序重跑,对手还握着
// 同一批行,几次就把 attempts 耗光,调用方看到的是「重试用尽」而不是「稍后重试能成」。
func TestWithTxRetryBackoff(t *testing.T) {
	injected := errors.New("模拟死锁")
	retryable := func(err error) bool { return errors.Is(err, injected) }

	t.Run("三次尝试退避两次", func(t *testing.T) {
		db, rec := newFakeTxDB(t)
		var slept []time.Duration
		cfg := DefaultTxRetryConfig()
		cfg.Attempts = 3
		cfg.IsRetryable = retryable
		cfg.Rand = func() float64 { return 0.5 } // 中值:抖动系数恰好 1.0,延迟可精确断言
		cfg.sleep = func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		}

		err := WithTxRetryConfig(context.Background(), db, cfg, func(*sql.Tx) error { return injected })
		if !errors.Is(err, injected) {
			t.Fatalf("重试用尽后应带出最后一次业务错误,实际: %v", err)
		}
		if rec.count() != 3 {
			t.Fatalf("应开三次事务,实际 %d 次", rec.count())
		}
		want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
		if len(slept) != len(want) {
			t.Fatalf("应退避 %d 次(最后一次失败后不再退避),实际 %d 次: %v", len(want), len(slept), slept)
		}
		for i := range want {
			if slept[i] != want[i] {
				t.Fatalf("第 %d 次退避应为 %v,实际 %v", i+1, want[i], slept[i])
			}
		}
	})

	t.Run("不可重试的错误不退避", func(t *testing.T) {
		db, rec := newFakeTxDB(t)
		slept := 0
		cfg := DefaultTxRetryConfig()
		cfg.Attempts = 3
		cfg.IsRetryable = func(error) bool { return false }
		cfg.sleep = func(context.Context, time.Duration) error { slept++; return nil }

		err := WithTxRetryConfig(context.Background(), db, cfg, func(*sql.Tx) error { return injected })
		if !errors.Is(err, injected) {
			t.Fatalf("不可重试的错误应原样返回,实际: %v", err)
		}
		if rec.count() != 1 || slept != 0 {
			t.Fatalf("应只开一次事务且不退避,实际 %d 次事务、%d 次退避", rec.count(), slept)
		}
	})

	t.Run("成功不退避", func(t *testing.T) {
		db, rec := newFakeTxDB(t)
		slept := 0
		cfg := DefaultTxRetryConfig()
		cfg.IsRetryable = retryable
		cfg.sleep = func(context.Context, time.Duration) error { slept++; return nil }

		if err := WithTxRetryConfig(context.Background(), db, cfg, func(*sql.Tx) error { return nil }); err != nil {
			t.Fatalf("事务应成功: %v", err)
		}
		if rec.count() != 1 || slept != 0 {
			t.Fatalf("成功路径应只开一次事务且不退避,实际 %d 次事务、%d 次退避", rec.count(), slept)
		}
	})

	// 退避期间预算耗尽:不再开下一个注定超时的事务,并且把业务错误与 ctx 错误一起带出去——
	// 只回 ctx 错误会让调用方看不到「到底是什么冲突」,只回业务错误又会让「是超时还是重试用尽」
	// 分不清,两者都会把排障引到错的方向。
	t.Run("退避被取消时不再尝试", func(t *testing.T) {
		db, rec := newFakeTxDB(t)
		cfg := DefaultTxRetryConfig()
		cfg.Attempts = 3
		cfg.IsRetryable = retryable
		cfg.sleep = func(context.Context, time.Duration) error { return context.DeadlineExceeded }

		err := WithTxRetryConfig(context.Background(), db, cfg, func(*sql.Tx) error { return injected })
		if !errors.Is(err, injected) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("应同时带出业务错误与 ctx 错误,实际: %v", err)
		}
		if rec.count() != 1 {
			t.Fatalf("退避被取消后不应再开事务,实际开了 %d 次", rec.count())
		}
	})
}

// TestTxBackoffShape 钉住退避形状:指数增长、±20% 抖动、按 MaxBackoff 封顶。
// 形状复用 decide.go 的 NextAttemptMs,本用例同时守住「没有人在这里另造一套算法」。
func TestTxBackoffShape(t *testing.T) {
	const base = 10 * time.Millisecond
	const max = 200 * time.Millisecond
	mid := func() float64 { return 0.5 }

	for _, c := range []struct {
		attempt uint32
		want    time.Duration
	}{
		{0, 10 * time.Millisecond},
		{1, 20 * time.Millisecond},
		{2, 40 * time.Millisecond},
		{5, max}, // 10ms<<5 = 320ms,被 MaxBackoff 压回
		{40, max},
	} {
		if got := txBackoff(c.attempt, base, max, mid); got != c.want {
			t.Fatalf("attempt=%d 的中值退避应为 %v,实际 %v", c.attempt, c.want, got)
		}
	}

	// 抖动区间:rnd 取两端时落在 [base×0.8, base×1.2)。同一批被同一行挡住的事务
	// 必须散开回来,否则退避只是把惊群整体推迟了一下。
	lo := txBackoff(0, base, max, func() float64 { return 0 })
	hi := txBackoff(0, base, max, func() float64 { return 0.999 })
	if lo != 8*time.Millisecond {
		t.Fatalf("抖动下界应为 8ms,实际 %v", lo)
	}
	if hi < 11*time.Millisecond || hi >= 12*time.Millisecond {
		t.Fatalf("抖动上界应落在 [11ms, 12ms),实际 %v", hi)
	}
}

// TestWithTxRetryRealBackoffIsCancelable 验**生产那条路径**的退避可取消。
//
// 其余用例都注入假的 sleep(为了能断言"退避了几次、每次多久"而不真的睡),于是真正跑在
// 生产上的那一份(sleepCtx)零覆盖。它一旦退化成 time.Sleep,表现是关停时每次重试都要
// 睡满退避时长 —— 本地几乎看不出来,线上滚动更新才会变成"pod 迟迟不退出"。
//
// 这里不断言墙钟相等(那会在慢 CI 上抖),只断言"远小于退避时长就返回了"。
func TestWithTxRetryRealBackoffIsCancelable(t *testing.T) {
	db, rec := newFakeTxDB(t)

	cfg := DefaultTxRetryConfig()
	cfg.Attempts = 3
	cfg.BaseBackoff = 5 * time.Second // 真睡满就必然超过下面的容差
	cfg.MaxBackoff = 10 * time.Second
	cfg.IsRetryable = func(error) bool { return true }
	cfg.sleep = nil // 关键:走生产用的 sleepCtx,不是注入的假 sleep

	ctx, cancel := context.WithCancel(context.Background())
	busy := errors.New("死锁,可重试")

	// 在业务函数里取消,而不是另起 goroutine 定时取消:后者要赌"取消发生在第一次尝试之后",
	// 赌输了 lastErr 还是 nil,断言会莫名其妙地翻。这样写没有任何墙钟依赖。
	start := time.Now()
	err := WithTxRetryConfig(ctx, db, cfg, func(*sql.Tx) error {
		cancel()
		return busy
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("ctx 被取消且业务一直失败,应当返回错误")
	}
	if !errors.Is(err, busy) {
		t.Errorf("错误里应保留业务错误,实际: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("错误里应带上 ctx 取消的原因,实际: %v", err)
	}
	if elapsed >= cfg.BaseBackoff {
		t.Errorf("退避睡满了 %v —— sleepCtx 没有响应取消", elapsed)
	}
	if got := rec.count(); got != 1 {
		t.Errorf("取消后不应再开新事务,实际开了 %d 次", got)
	}
}

// TestWithTxRetryRejectsBadConfig:参数不合法时一个事务都不许开(fail-closed)。
// 尤其是退避区间:BaseBackoff 小于 2ms 时 ±20% 抖动的下界会被毫秒取整抹成 0,
// 等于悄悄退回"一半的重试没有退避"。
func TestWithTxRetryRejectsBadConfig(t *testing.T) {
	noop := func(*sql.Tx) error { return nil }

	t.Run("attempts 小于 1", func(t *testing.T) {
		db, rec := newFakeTxDB(t)
		if err := WithTxRetry(context.Background(), db, 0, nil, noop); err == nil {
			t.Fatal("attempts=0 应报错")
		}
		if rec.count() != 0 {
			t.Fatalf("参数非法时不应开事务,实际开了 %d 次", rec.count())
		}
	})

	t.Run("退避区间非法", func(t *testing.T) {
		db, rec := newFakeTxDB(t)
		cases := []struct {
			name   string
			mutate func(*TxRetryConfig)
		}{
			{"base 为零", func(c *TxRetryConfig) { c.BaseBackoff = 0 }},
			{"base 亚毫秒", func(c *TxRetryConfig) { c.BaseBackoff = 100 * time.Microsecond }},
			// 1ms 曾经是允许的下限,但 ±20% 抖动的下界 0.8ms 按毫秒取整就是 0,
			// 约一半的重试实际不退避。这条钉住"下限是 2ms"这个选择,免得有人顺手调回 1ms。
			{"base 恰好 1ms(抖动下界会被抹成 0)", func(c *TxRetryConfig) { c.BaseBackoff = time.Millisecond }},
			{"max 小于 base", func(c *TxRetryConfig) { c.MaxBackoff = time.Millisecond }},
		}
		for _, c := range cases {
			cfg := DefaultTxRetryConfig()
			c.mutate(&cfg)
			if err := WithTxRetryConfig(context.Background(), db, cfg, noop); err == nil {
				t.Fatalf("%s 应报错", c.name)
			}
		}
		if rec.count() != 0 {
			t.Fatalf("参数非法时不应开事务,实际开了 %d 次", rec.count())
		}
	})

	t.Run("缺 db 或 fn", func(t *testing.T) {
		db, _ := newFakeTxDB(t)
		if err := WithTxRetry(context.Background(), nil, 1, nil, noop); err == nil {
			t.Fatal("db 为 nil 应报错")
		}
		if err := WithTxRetry(context.Background(), db, 1, nil, nil); err == nil {
			t.Fatal("fn 为 nil 应报错")
		}
	})
}

// TestWithTxRetryStopsOnCanceledContext:ctx 已取消时一次都不尝试。
// 这是重试循环开头那次 ctx 检查的意义:调用方已经走了,再去开事务只会白占一条连接和一批行锁。
func TestWithTxRetryStopsOnCanceledContext(t *testing.T) {
	db, rec := newFakeTxDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := WithTxRetry(ctx, db, 3, func(error) bool { return true }, func(*sql.Tx) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 ctx 错误,实际: %v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("ctx 已取消时不应开事务,实际开了 %d 次", rec.count())
	}
}

// TestWithTxRetryPropagatesBeginError:开事务本身失败也走同一套重试与退避,
// 而不是直接抛给调用方——连接池瞬时打满、TiDB 正在切主都属于「重跑一下就好」。
func TestWithTxRetryPropagatesBeginError(t *testing.T) {
	db, rec := newFakeTxDB(t)
	beginErr := errors.New("连接池打满")
	rec.setBeginErr(beginErr)

	var slept []time.Duration
	cfg := DefaultTxRetryConfig()
	cfg.Attempts = 2
	cfg.IsRetryable = func(err error) bool { return errors.Is(err, beginErr) }
	cfg.Rand = func() float64 { return 0.5 }
	cfg.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }

	err := WithTxRetryConfig(context.Background(), db, cfg, func(*sql.Tx) error { return nil })
	if !errors.Is(err, beginErr) {
		t.Fatalf("应带出开事务的错误,实际: %v", err)
	}
	if rec.count() != 2 {
		t.Fatalf("应尝试两次,实际 %d 次", rec.count())
	}
	if len(slept) != 1 || slept[0] != 10*time.Millisecond {
		t.Fatalf("两次尝试之间应退避一次 10ms,实际 %v", slept)
	}
}

// TestPendingSeqQueryTakesNoLocks 钉住 AllocateSeq 的未决读是**普通读**。
//
// 2026-09-21 死锁审计:这条读原先带 FOR UPDATE,经二级索引 (player_id, stream, stream_epoch, status, seq)
// 取锁是"二级 → 主键",与 Finalize / ResolveManually 的主键 CAS 改 status("主键 → 同一二级项")在同一 op 行上
// 反序成环。谁把锁定子句加回来,这条环就回来了 —— 正确性改由"seq 行串行化 + RC 语句级快照"保证,见 AllocateSeq。
func TestPendingSeqQueryTakesNoLocks(t *testing.T) {
	q := strings.ToUpper(pendingSeqQueryFormat)
	for _, clause := range []string{"FOR UPDATE", "FOR SHARE", "LOCK IN SHARE MODE"} {
		if strings.Contains(q, clause) {
			t.Fatalf("AllocateSeq 的未决读不得带 %q(会与 Finalize 的主键 CAS 反序成环,见 AllocateSeq 注释): %s",
				clause, pendingSeqQueryFormat)
		}
	}
}

// execScript 让假驱动按脚本依次返回 Exec 的错误(用完之后返回成功),并记下被调用了几次。
type execScript struct {
	mu    sync.Mutex
	errs  []error
	calls int
}

func (s *execScript) next() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if i < len(s.errs) {
		return s.errs[i]
	}
	return nil
}

func (s *execScript) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type execScriptConnector struct{ s *execScript }

func (c execScriptConnector) Connect(context.Context) (driver.Conn, error) {
	return execScriptConn{s: c.s}, nil
}

func (c execScriptConnector) Driver() driver.Driver { return execScriptDriver{s: c.s} }

type execScriptDriver struct{ s *execScript }

func (d execScriptDriver) Open(string) (driver.Conn, error) { return execScriptConn{s: d.s}, nil }

// execScriptConn 只支持无事务的 ExecContext(EnsureSeqRow 就是一条自动提交语句);
// 其余入口一律报错,真走到了应当一眼看见。
type execScriptConn struct{ s *execScript }

func (c execScriptConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("假驱动不支持 Prepare")
}
func (c execScriptConn) Close() error { return nil }
func (c execScriptConn) Begin() (driver.Tx, error) {
	return nil, errors.New("假驱动不支持事务")
}
func (c execScriptConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	if err := c.s.next(); err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

// TestEnsureSeqRowRetry:可重试的错误按 DefaultTxRetryConfig 的次数重跑,其余错误一次就返回。
// 退避用真实的 sleepCtx(两次合计约 30ms):EnsureSeqRowRetry 刻意不开放退避参数,用的就是默认值。
func TestEnsureSeqRowRetry(t *testing.T) {
	tables, err := NewSeqTables("unit_seq", "unit_op", 0)
	if err != nil {
		t.Fatalf("表名校验失败: %v", err)
	}
	deadlock := errors.New("模拟 1213")
	other := errors.New("模拟语法错")
	onlyDeadlock := func(err error) bool { return errors.Is(err, deadlock) }

	cases := []struct {
		name        string
		script      []error
		isRetryable func(error) bool
		wantErr     error
		wantCalls   int
	}{
		{"两次死锁后成功", []error{deadlock, deadlock}, onlyDeadlock, nil, 3},
		{"次数用尽仍失败", []error{deadlock, deadlock, deadlock}, onlyDeadlock, deadlock, 3},
		{"不可重试的错误只跑一次", []error{other}, onlyDeadlock, other, 1},
		{"分类函数为 nil 时不重试", []error{deadlock}, nil, deadlock, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := &execScript{errs: tc.script}
			db := sql.OpenDB(execScriptConnector{s: script})
			t.Cleanup(func() { _ = db.Close() })

			err := EnsureSeqRowRetry(context.Background(), db, tables, 1, testStream, 1700000000000, tc.isRetryable)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("应当成功,实际: %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("应带出 %v,实际: %v", tc.wantErr, err)
			}
			if got := script.count(); got != tc.wantCalls {
				t.Fatalf("应执行 %d 次,实际 %d 次", tc.wantCalls, got)
			}
		})
	}
}

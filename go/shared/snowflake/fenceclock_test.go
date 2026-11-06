package snowflake

import (
	"errors"
	"testing"
	"time"
)

// fakeClocks 是可拨的单调钟 + 墙钟,两者独立推进,好分别验证两条规则。
type fakeClocks struct {
	mono time.Duration
	wall time.Time
}

func newFakeClocks() *fakeClocks {
	return &fakeClocks{mono: time.Hour, wall: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
}

func (f *fakeClocks) clock(budget time.Duration) *FenceClock {
	return NewFenceClockWithClocks(budget,
		func() time.Duration { return f.mono },
		func() time.Time { return f.wall })
}

// 从未 Ack 的闸必须是过期的(fail-closed):分配器交出发号器前必须先同步写一次水位。
func TestFenceClock_NeverAckedIsStale(t *testing.T) {
	fc := newFakeClocks().clock(time.Minute)
	if stale, _ := fc.Stale(); !stale {
		t.Fatal("从未 Ack 的 FenceClock 必须判为过期")
	}
}

// 单调钟越过 F 即过期(墙钟被回拨也拦不住)。
func TestFenceClock_MonotonicAgeTrips(t *testing.T) {
	f := newFakeClocks()
	fc := f.clock(time.Minute)
	fc.Ack()
	if stale, _ := fc.Stale(); stale {
		t.Fatal("刚 Ack 不该过期")
	}
	f.mono += 59 * time.Second
	if stale, _ := fc.Stale(); stale {
		t.Fatal("59s < F 不该过期")
	}
	f.mono += 2 * time.Second
	f.wall = f.wall.Add(-time.Hour) // 墙钟回拨,不该救回来
	if stale, age := fc.Stale(); !stale || age < 61*time.Second {
		t.Fatalf("单调钟 61s > F 必须过期, stale=%v age=%v", stale, age)
	}
}

// 墙钟越过 F 即过期 —— VM 挂起时单调钟停走,只有墙钟知道真实流逝。
func TestFenceClock_WallAgeTripsWhenMonotonicStalls(t *testing.T) {
	f := newFakeClocks()
	fc := f.clock(time.Minute)
	fc.Ack()
	f.wall = f.wall.Add(2 * time.Minute) // 单调钟一动不动(挂起)
	if stale, _ := fc.Stale(); !stale {
		t.Fatal("墙钟 2min > F 必须过期,即使单调钟没走")
	}
	// 再次 Ack 即恢复(暂态语义)。
	fc.Ack()
	if stale, _ := fc.Stale(); stale {
		t.Fatal("重新 Ack 后必须恢复")
	}
}

// 本地缓存启动:SeedAck 把上次成功时刻灌进来,两条规则都从那一刻起算。
func TestFenceClock_SeedAckCountsFromThePast(t *testing.T) {
	f := newFakeClocks()
	fc := f.clock(time.Minute)
	fc.SeedAck(f.wall.Add(-50 * time.Second))
	if stale, age := fc.Stale(); stale || age < 50*time.Second {
		t.Fatalf("灌入 50s 前的 Ack:不过期但年龄≥50s, stale=%v age=%v", stale, age)
	}
	f.mono += 11 * time.Second // 只推单调钟:50+11 > 60 → 过期(单调规则也从过去起算)
	if stale, _ := fc.Stale(); !stale {
		t.Fatal("SeedAck 后单调规则必须从灌入时刻起算")
	}
	if got := fc.LastAckWall(); !got.Equal(f.wall.Add(-50 * time.Second)) {
		t.Fatalf("LastAckWall=%v", got)
	}
}

// hasMonotonic 报告 t 是否带单调读数。time.Time 的 == 连单调读数一起比(time 包文档),
// 而 Round(0) 只去掉单调读数、其余不变,所以两者不等 ⇔ 带单调读数。
func hasMonotonic(t time.Time) bool { return t != t.Round(0) }

// 第一轮数据审计 §4 第 20 条回归:生产构造器拿 time.Now 当墙钟,而 time.Now 带单调读数;
// 两个都带单调读数的 Time 相减只看单调读数,墙钟规则会退化成第二条单调规则 —— Linux 的
// CLOCK_MONOTONIC 不含系统挂起时间,VM 挂起之后两条规则都不触发。上面几条用例的墙钟由
// time.Date 构造(本来就不带单调读数),正好把这个问题盖住了。
//
// time 包造不出"墙钟走了、单调读数没走"的 Time,所以这里用真实 time.Now 钉住机制本身:
// 墙钟规则的锚点(LastAckWall)不得带单调读数 —— 只要有一端不带,Sub 就按墙钟相减。
// 修复前 Ack 原样存 time.Now(),SeedAck 原样存入参,两处断言都会失败。
func TestFenceClock_RealWallClockAnchorCarriesNoMonotonicReading(t *testing.T) {
	cases := []struct {
		name string
		fc   *FenceClock
	}{
		{"NewFenceClock", NewFenceClock(time.Minute)},
		{"injected time.Now", NewFenceClockWithClocks(time.Minute, func() time.Duration { return 0 }, time.Now)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.fc.Ack()
			if got := tc.fc.LastAckWall(); hasMonotonic(got) {
				t.Fatalf("Ack 记下的墙钟带着单调读数(%v):墙钟规则会退化成单调规则,VM 挂起后不触发", got)
			}
			if stale, age := tc.fc.Stale(); stale || age >= time.Minute {
				t.Fatalf("刚 Ack 不该过期: stale=%v age=%v", stale, age)
			}

			// 调用方用 time.Now() 推算的时刻同样带单调读数,SeedAck 也得去掉。
			seed := time.Now().Add(-30 * time.Second)
			tc.fc.SeedAck(seed)
			got := tc.fc.LastAckWall()
			if hasMonotonic(got) {
				t.Fatalf("SeedAck 记下的墙钟带着单调读数(%v)", got)
			}
			if !got.Equal(seed) {
				t.Fatalf("SeedAck 记下的时刻变了: got %v want %v", got, seed)
			}
			if stale, age := tc.fc.Stale(); stale || age < 30*time.Second {
				t.Fatalf("灌入 30s 前的 Ack:不过期但年龄≥30s, stale=%v age=%v", stale, age)
			}
		})
	}
}

// Node.Generate 必须在入口执行水位年龄判定:过期 → ErrWatermarkStale,不交号;
// 重新 Ack → 恢复。这是 shared/snowflake 与 login PlayerIDGen 共用的规则。
func TestGenerate_RefusesWhenWatermarkStale(t *testing.T) {
	restore := nowEpoch
	defer func() { nowEpoch = restore }()
	current := uint64(7000)
	nowEpoch = func() uint64 { return current }

	f := newFakeClocks()
	fc := f.clock(time.Minute)
	n := NewNode(9)
	n.SetFenceClock(fc)

	current = 7001
	if id, err := n.Generate(); !errors.Is(err, ErrWatermarkStale) || id != 0 {
		t.Fatalf("从未 Ack 必须拒发 (ErrWatermarkStale, 0), got id=%d err=%v", id, err)
	}

	fc.Ack()
	if _, err := n.Generate(); err != nil {
		t.Fatalf("Ack 后必须能发号: %v", err)
	}

	f.wall = f.wall.Add(61 * time.Second)
	current = 7002
	if id, err := n.Generate(); !errors.Is(err, ErrWatermarkStale) || id != 0 {
		t.Fatalf("墙钟越过 F 必须拒发, got id=%d err=%v", id, err)
	}

	fc.Ack()
	current = 7003
	if _, err := n.Generate(); err != nil {
		t.Fatalf("重新 Ack 后必须恢复: %v", err)
	}

	// 永久 fence 优先级更高,且不受 Ack 影响。
	n.Fence()
	fc.Ack()
	if _, err := n.Generate(); !errors.Is(err, ErrFenced) {
		t.Fatalf("Fence 后必须是 ErrFenced, got %v", err)
	}
}

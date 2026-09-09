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

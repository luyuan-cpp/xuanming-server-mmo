package gatepick

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

var trackerBase = time.Unix(1_800_000_000, 0)

func at(d time.Duration) time.Time { return trackerBase.Add(d) }

func rev(key string, modRev int64) Revision { return Revision{Key: key, ModRev: modRev} }

func wantStale(t *testing.T, got []bool, want ...bool) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("stale = %v, want %v", got, want)
	}
}

// 同一个 ModRevision 从本进程第一次看到起超过 StaleAfter 才判停写:第一次看到不判,恰好等于不判。
func TestRevisionTracker_StaleAfterThresholdFromFirstSighting(t *testing.T) {
	var tr RevisionTracker
	wantStale(t, tr.Observe(7, []Revision{rev("g1", 100)}, at(0)), false)
	wantStale(t, tr.Observe(7, []Revision{rev("g1", 100)}, at(StaleAfter)), false)
	wantStale(t, tr.Observe(7, []Revision{rev("g1", 100)}, at(StaleAfter+time.Millisecond)), true)
}

// 每个上报周期重写一次的健康 gate,读得再密也不判停写。
func TestRevisionTracker_HealthyGateNeverStale(t *testing.T) {
	var tr RevisionTracker
	modRev := int64(100)
	for sec := 0; sec <= 120; sec++ {
		if sec > 0 && time.Duration(sec)*time.Second%ReportPeriod == 0 {
			modRev++
		}
		if got := tr.Observe(7, []Revision{rev("g1", modRev)}, at(time.Duration(sec)*time.Second)); got[0] {
			t.Fatalf("healthy gate judged stale at %ds", sec)
		}
	}
}

// 停写判出之后 gate 恢复上报:新的 ModRevision 重新起算,下一次读就不再判停写。
func TestRevisionTracker_NewWriteResetsClock(t *testing.T) {
	var tr RevisionTracker
	tr.Observe(7, []Revision{rev("g1", 100)}, at(0))
	wantStale(t, tr.Observe(7, []Revision{rev("g1", 100)}, at(40*time.Second)), true)
	wantStale(t, tr.Observe(7, []Revision{rev("g1", 101)}, at(41*time.Second)), false)
	wantStale(t, tr.Observe(7, []Revision{rev("g1", 101)}, at(41*time.Second+StaleAfter)), false)
}

// 并发的读乱序到达:晚到的旧结果不判停写,也不把记下的较新版本冲掉。
func TestRevisionTracker_OlderReadArrivingLateIsIgnored(t *testing.T) {
	var tr RevisionTracker
	tr.Observe(7, []Revision{rev("g1", 100)}, at(0))
	tr.Observe(7, []Revision{rev("g1", 105)}, at(5*time.Second))
	wantStale(t, tr.Observe(7, []Revision{rev("g1", 100)}, at(20*time.Second)), false)
	// 记下的仍是 105、从 5s 起算。
	wantStale(t, tr.Observe(7, []Revision{rev("g1", 105)}, at(5*time.Second+StaleAfter)), false)
	wantStale(t, tr.Observe(7, []Revision{rev("g1", 105)}, at(5*time.Second+StaleAfter+time.Millisecond)), true)
}

// ModRevision 未知(0)当作刚写过,永不判停写。
func TestRevisionTracker_UnknownModRevisionCountsAsWrite(t *testing.T) {
	var tr RevisionTracker
	tr.Observe(7, []Revision{rev("g1", 0)}, at(0))
	wantStale(t, tr.Observe(7, []Revision{rev("g1", 0)}, at(time.Hour)), false)
}

// 每次读的全量结果替换本 zone 的记录:消失的键被丢掉,再出现时按新记录从头算;zone 之间互不影响;读到空结果清空本 zone。
func TestRevisionTracker_ReplacesZoneAndKeepsZonesApart(t *testing.T) {
	var tr RevisionTracker
	tr.Observe(7, []Revision{rev("g1", 100), rev("g2", 200)}, at(0))
	tr.Observe(8, []Revision{rev("g1", 100)}, at(0)) // 另一个 zone 恰好同键同版本
	tr.Observe(7, []Revision{rev("g2", 200)}, at(10*time.Second))

	// g1 在 10s 那次读里消失过,40s 再出现时从 40s 起算;g2 一直是 200,从 0s 起算。
	wantStale(t, tr.Observe(7, []Revision{rev("g1", 100), rev("g2", 200)}, at(40*time.Second)), false, true)
	// zone 8 不受 zone 7 的读影响。
	wantStale(t, tr.Observe(8, []Revision{rev("g1", 100)}, at(40*time.Second)), true)

	tr.Observe(7, nil, at(41*time.Second))
	if _, ok := tr.zones[7]; ok {
		t.Fatal("an empty read must drop the zone's records")
	}
}

// 零值可用,并发调用安全(-race 下才有意义)。
func TestRevisionTracker_ConcurrentObserve(t *testing.T) {
	var tr RevisionTracker
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(zone uint32) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				tr.Observe(zone, []Revision{rev("g1", int64(i)), rev("g2", 1)}, at(time.Duration(i)*time.Second))
			}
		}(uint32(g % 3))
	}
	wg.Wait()
}

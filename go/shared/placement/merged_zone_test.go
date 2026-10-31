package placement

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestResolveMergedZoneFollowsTheChain(t *testing.T) {
	// 恰好 MaxMergedZoneHops 跳的链:1→2→...→17,共 16 跳。
	longest := map[uint32]uint32{}
	for z := uint32(1); z <= MaxMergedZoneHops; z++ {
		longest[z] = z + 1
	}
	// 多一跳的链:1→2→...→18,共 17 跳。
	tooLong := map[uint32]uint32{}
	for z := uint32(1); z <= MaxMergedZoneHops+1; z++ {
		tooLong[z] = z + 1
	}

	for _, tc := range []struct {
		name   string
		merges map[uint32]uint32
		zone   uint32
		want   uint32
		bad    bool
	}{
		{name: "不在表里返回自身", merges: map[uint32]uint32{5: 6}, zone: 7, want: 7},
		{name: "空表返回自身", merges: map[uint32]uint32{}, zone: 7, want: 7},
		{name: "nil 表返回自身", merges: nil, zone: 7, want: 7},
		{name: "一跳", merges: map[uint32]uint32{5: 6}, zone: 5, want: 6},
		{name: "连续合服沿链解析", merges: map[uint32]uint32{5: 6, 6: 7}, zone: 5, want: 7},
		{name: "链中间的区解析到链尾", merges: map[uint32]uint32{5: 6, 6: 7}, zone: 6, want: 7},
		{name: "链尾是自身", merges: map[uint32]uint32{5: 6, 6: 7}, zone: 7, want: 7},
		{name: "恰好 16 跳成功", merges: longest, zone: 1, want: MaxMergedZoneHops + 1},
		{name: "17 跳超限", merges: tooLong, zone: 1, bad: true},
		{name: "自环", merges: map[uint32]uint32{5: 5}, zone: 5, bad: true},
		{name: "两区成环", merges: map[uint32]uint32{5: 6, 6: 5}, zone: 5, bad: true},
		{name: "入环之前有一段尾巴", merges: map[uint32]uint32{4: 5, 5: 6, 6: 5}, zone: 4, bad: true},
		{name: "目标区是 0", merges: map[uint32]uint32{5: 0}, zone: 5, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveMergedZone(tc.merges, tc.zone)
			if tc.bad {
				if !errors.Is(err, ErrMergedZoneUnresolvable) {
					t.Fatalf("resolveMergedZone(%v, %d) = (%d, %v), want ErrMergedZoneUnresolvable", tc.merges, tc.zone, got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("resolveMergedZone(%v, %d) = (%d, %v), want (%d, nil)", tc.merges, tc.zone, got, err, tc.want)
			}
		})
	}
}

func TestNewMergedZonesRejectsBrokenMappings(t *testing.T) {
	tooLong := map[uint32]uint32{}
	for z := uint32(1); z <= MaxMergedZoneHops+1; z++ {
		tooLong[z] = z + 1
	}
	for name, merges := range map[string]map[uint32]uint32{
		"自环":        {5: 5},
		"两区成环":      {5: 6, 6: 5},
		"入环之前有尾巴":   {4: 5, 5: 6, 6: 5},
		"超过 16 跳":   tooLong,
		"目标区是 0":    {5: 0},
		"正常的链和坏链并存": {1: 2, 5: 6, 6: 5},
	} {
		t.Run(name, func(t *testing.T) {
			zones, err := NewMergedZones(merges)
			if !errors.Is(err, ErrMergedZoneUnresolvable) || zones != nil {
				t.Fatalf("NewMergedZones(%v) = (%v, %v), want ErrMergedZoneUnresolvable", merges, zones, err)
			}
		})
	}
}

func TestNewMergedZonesAcceptsExactlySixteenHops(t *testing.T) {
	longest := map[uint32]uint32{}
	for z := uint32(1); z <= MaxMergedZoneHops; z++ {
		longest[z] = z + 1
	}
	zones, err := NewMergedZones(longest)
	if err != nil {
		t.Fatal(err)
	}
	if got := zones.Final(1); got != MaxMergedZoneHops+1 {
		t.Fatalf("Final(1) = %d, want %d", got, MaxMergedZoneHops+1)
	}
	if got := zones.Members(MaxMergedZoneHops + 1); len(got) != MaxMergedZoneHops+1 || !slices.IsSorted(got) {
		t.Fatalf("Members = %v", got)
	}
}

func TestMergedZonesFinalAndMembers(t *testing.T) {
	// 5→6→7 连续合服,3→7 直接合入,8→9 是另一组。
	zones, err := NewMergedZones(map[uint32]uint32{5: 6, 6: 7, 3: 7, 8: 9})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		zone    uint32
		final   uint32
		members []uint32
	}{
		{zone: 7, final: 7, members: []uint32{3, 5, 6, 7}},
		{zone: 9, final: 9, members: []uint32{8, 9}},
		// 没有参与合服的区:解析到自身,成员只有自己。
		{zone: 100, final: 100, members: []uint32{100}},
		// 已经被合走的区:Members 等于 Members(Final(zone)),调用方拿到的永远是完整的一组。
		{zone: 5, final: 7, members: []uint32{3, 5, 6, 7}},
		{zone: 6, final: 7, members: []uint32{3, 5, 6, 7}},
		{zone: 8, final: 9, members: []uint32{8, 9}},
	} {
		if got := zones.Final(tc.zone); got != tc.final {
			t.Errorf("Final(%d) = %d, want %d", tc.zone, got, tc.final)
		}
		if got := zones.Members(tc.zone); !slices.Equal(got, tc.members) {
			t.Errorf("Members(%d) = %v, want %v", tc.zone, got, tc.members)
		}
	}
	if want := map[uint32]uint32{5: 6, 6: 7, 3: 7, 8: 9}; !maps.Equal(zones.Merges(), want) {
		t.Fatalf("Merges() = %v, want %v", zones.Merges(), want)
	}
}

// 快照不可变:入参之后再改、调用方改返回值,都不影响快照。
func TestMergedZonesIsImmutable(t *testing.T) {
	source := map[uint32]uint32{5: 6}
	zones, err := NewMergedZones(source)
	if err != nil {
		t.Fatal(err)
	}
	source[5] = 8
	source[7] = 8
	if got := zones.Final(5); got != 6 {
		t.Fatalf("Final(5) = %d after the source map changed", got)
	}

	members := zones.Members(6)
	members[0] = 99
	if got := zones.Members(6); !slices.Equal(got, []uint32{5, 6}) {
		t.Fatalf("Members(6) = %v after the caller changed its copy", got)
	}
	single := zones.Members(100)
	single[0] = 99
	if got := zones.Members(100); !slices.Equal(got, []uint32{100}) {
		t.Fatalf("Members(100) = %v after the caller changed its copy", got)
	}

	merges := zones.Merges()
	merges[5] = 99
	if got := zones.Merges()[5]; got != 6 {
		t.Fatalf("Merges()[5] = %d after the caller changed its copy", got)
	}
}

// nil 快照的语义:等于没有任何合服映射。要区分「没读到」的调用方必须自己先判 nil。
func TestMergedZonesNilReceiverMeansNoMerges(t *testing.T) {
	var zones *MergedZones
	if got := zones.Final(5); got != 5 {
		t.Fatalf("nil.Final(5) = %d, want 5", got)
	}
	if got := zones.Members(5); !slices.Equal(got, []uint32{5}) {
		t.Fatalf("nil.Members(5) = %v, want [5]", got)
	}
	if got := zones.Merges(); got != nil {
		t.Fatalf("nil.Merges() = %v, want nil", got)
	}
}

func TestNewMergedZonesNilMapIsEmpty(t *testing.T) {
	zones, err := NewMergedZones(nil)
	if err != nil || zones == nil {
		t.Fatalf("NewMergedZones(nil) = (%v, %v)", zones, err)
	}
	if got := zones.Final(5); got != 5 {
		t.Fatalf("Final(5) = %d", got)
	}
	if got := zones.Members(5); !slices.Equal(got, []uint32{5}) {
		t.Fatalf("Members(5) = %v", got)
	}
}

// fakeMergedZoneSource 按预置结果回答 read;每次回答之后往 served 投递一次,单测用它确认某一拍已经跑完。
type fakeMergedZoneSource struct {
	mu     sync.Mutex
	merges map[uint32]uint32
	err    error
	served chan struct{}
	block  chan struct{} // 非 nil 时读取阻塞到 ctx 结束
}

func (f *fakeMergedZoneSource) read(ctx context.Context) (map[uint32]uint32, error) {
	f.mu.Lock()
	merges, err, served, block := f.merges, f.err, f.served, f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-block:
		}
	}
	if served != nil {
		defer func() { served <- struct{}{} }()
	}
	return merges, err
}

func (f *fakeMergedZoneSource) set(merges map[uint32]uint32, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merges, f.err = merges, err
}

func TestMergedZoneCacheFirstReadFailureRefusesToBuild(t *testing.T) {
	boom := errors.New("boom")
	cache, err := NewMergedZoneCache(context.Background(), (&fakeMergedZoneSource{err: boom}).read, MergedZoneCacheOptions{})
	if !errors.Is(err, boom) || cache != nil {
		t.Fatalf("got (%v, %v): without the first read nothing tells no-merge from could-not-read", cache, err)
	}
}

func TestMergedZoneCacheBrokenMappingAtStartRefusesToBuild(t *testing.T) {
	source := &fakeMergedZoneSource{merges: map[uint32]uint32{5: 6, 6: 5}}
	cache, err := NewMergedZoneCache(context.Background(), source.read, MergedZoneCacheOptions{})
	if !errors.Is(err, ErrMergedZoneUnresolvable) || cache != nil {
		t.Fatalf("got (%v, %v), want ErrMergedZoneUnresolvable", cache, err)
	}
}

func TestMergedZoneCacheRejectsBadArguments(t *testing.T) {
	if _, err := NewMergedZoneCache(context.Background(), nil, MergedZoneCacheOptions{}); err == nil {
		t.Fatal("a nil reader must be rejected")
	}
	source := &fakeMergedZoneSource{}
	if _, err := NewMergedZoneCache(context.Background(), source.read, MergedZoneCacheOptions{Interval: -time.Second}); err == nil {
		t.Fatal("a negative interval must be rejected")
	}
}

func TestMergedZoneCacheDefaultInterval(t *testing.T) {
	cache, err := NewMergedZoneCache(context.Background(), (&fakeMergedZoneSource{}).read, MergedZoneCacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cache.interval != 5*time.Second || DefaultMergedZoneRefreshInterval != 5*time.Second {
		t.Fatalf("interval = %s, default = %s, want 5s", cache.interval, DefaultMergedZoneRefreshInterval)
	}
}

// nil 缓存(没装配)回 nil 快照,生命周期方法是空操作、不 panic。
func TestMergedZoneCacheNilReceiver(t *testing.T) {
	var absent *MergedZoneCache
	if got := absent.Snapshot(); got != nil {
		t.Fatalf("nil.Snapshot() = %v, want nil", got)
	}
	absent.Start()
	absent.Stop()
}

// 定时重读:读到新映射就替换;读失败、读到坏映射都保留旧值并回调;恢复后接着替换。拍子由单测手动推进,不依赖真实时钟。
func TestMergedZoneCacheRefreshReplacesAndKeepsOldOnFailure(t *testing.T) {
	source := &fakeMergedZoneSource{merges: map[uint32]uint32{5: 6}}
	var (
		mu      sync.Mutex
		results []error
		changes []map[uint32]uint32
	)
	observed := func() ([]error, []map[uint32]uint32) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(results), slices.Clone(changes)
	}
	cache, err := NewMergedZoneCache(context.Background(), source.read, MergedZoneCacheOptions{
		Interval: time.Hour,
		OnRefresh: func(err error) {
			mu.Lock()
			defer mu.Unlock()
			results = append(results, err)
		},
		OnChange: func(zones *MergedZones) {
			mu.Lock()
			defer mu.Unlock()
			changes = append(changes, zones.Merges())
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, changed := observed(); len(got) != 0 || len(changed) != 1 {
		t.Fatalf("after the first read: refreshes %v, changes %v; the first read counts as a change, not a refresh", got, changed)
	}

	source.mu.Lock()
	source.served = make(chan struct{}, 1)
	source.mu.Unlock()
	tick := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cache.run(ctx, tick)
	}()
	step := func() {
		t.Helper()
		tick <- time.Time{}
		<-source.served
		tick <- time.Time{} // run 取走第二拍,说明上一拍的 refresh 和回调已经全部做完
		<-source.served
	}

	source.set(map[uint32]uint32{5: 6, 6: 7}, nil)
	step()
	if got := cache.Snapshot().Final(5); got != 7 {
		t.Fatalf("a new merge is visible after one refresh: Final(5) = %d", got)
	}

	before, _ := observed()
	source.set(nil, errors.New("db down"))
	step()
	if got := cache.Snapshot().Final(5); got != 7 {
		t.Fatalf("a failed refresh keeps the previous mapping: Final(5) = %d", got)
	}
	got, _ := observed()
	if len(got) <= len(before) || got[len(before)] == nil {
		t.Fatalf("a failed refresh must reach OnRefresh with the error: %v", got)
	}

	before, _ = observed()
	source.set(map[uint32]uint32{5: 6, 6: 5}, nil)
	step()
	if got := cache.Snapshot().Final(5); got != 7 {
		t.Fatalf("a broken mapping keeps the previous snapshot: Final(5) = %d", got)
	}
	got, _ = observed()
	if len(got) <= len(before) || !errors.Is(got[len(before)], ErrMergedZoneUnresolvable) {
		t.Fatalf("a broken mapping must reach OnRefresh as ErrMergedZoneUnresolvable: %v", got)
	}

	// 撤销合服:映射删掉之后归属回到各自的区。
	before, _ = observed()
	source.set(map[uint32]uint32{6: 7}, nil)
	step()
	if got := cache.Snapshot().Final(5); got != 5 {
		t.Fatalf("after unmerging: Final(5) = %d", got)
	}
	got, changed := observed()
	if len(got) <= len(before) || got[len(before)] != nil {
		t.Fatalf("a successful refresh reaches OnRefresh with nil: %v", got)
	}
	if !maps.Equal(changed[len(changed)-1], map[uint32]uint32{6: 7}) {
		t.Fatalf("OnChange got %v", changed[len(changed)-1])
	}

	// 内容没变不换快照、不报 change。
	snapshot := cache.Snapshot()
	_, changesBefore := observed()
	step()
	if cache.Snapshot() != snapshot {
		t.Fatal("an unchanged mapping must keep the same snapshot")
	}
	if _, changesAfter := observed(); len(changesAfter) != len(changesBefore) {
		t.Fatalf("an unchanged mapping must not report a change: %d -> %d", len(changesBefore), len(changesAfter))
	}

	cancel()
	<-done
}

// Stop 要停得下来:取消在途的读、等 goroutine 退出;可重复调用;Stop 之后 Start 不再起任何东西,Snapshot 仍读最后的映射。
// 关停时被取消的那一次不算故障,不回调。
func TestMergedZoneCacheStopCancelsInFlightRefresh(t *testing.T) {
	source := &fakeMergedZoneSource{merges: map[uint32]uint32{5: 6}}
	var refreshes sync.Map
	cache, err := NewMergedZoneCache(context.Background(), source.read, MergedZoneCacheOptions{
		Interval:  time.Millisecond,
		OnRefresh: func(err error) { refreshes.Store(fmt.Sprint(err), true) },
	})
	if err != nil {
		t.Fatal(err)
	}

	source.mu.Lock()
	source.block = make(chan struct{}) // 永远不放行:读只能靠 ctx 取消结束
	source.mu.Unlock()
	cache.Start()
	cache.Start() // 重复调用不起第二条循环
	cache.Stop()
	cache.Stop()

	done := cache.done
	cache.Start()
	if done != cache.done {
		t.Fatal("Start after Stop must not restart the timer")
	}
	if got := cache.Snapshot().Final(5); got != 6 {
		t.Fatalf("Final(5) = %d after Stop", got)
	}
	refreshes.Range(func(key, _ any) bool {
		t.Errorf("a refresh cancelled by shutdown must not be reported: %v", key)
		return true
	})
}

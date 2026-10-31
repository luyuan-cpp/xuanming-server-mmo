package routing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"data_service/internal/store"

	"shared/placement"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMergedZoneLister 按预置结果回答 ListMergedInto;每次回答之后往 served 投递一次,单测用它确认某一拍已经跑完。
type fakeMergedZoneLister struct {
	mu     sync.Mutex
	rows   []store.MergedIntoRow
	err    error
	calls  int
	served chan struct{}
	block  chan struct{} // 非 nil 时读取阻塞到 ctx 结束
}

func (f *fakeMergedZoneLister) ListMergedInto(ctx context.Context) ([]store.MergedIntoRow, error) {
	f.mu.Lock()
	f.calls++
	rows, err, served, block := f.rows, f.err, f.served, f.block
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
	return rows, err
}

func (f *fakeMergedZoneLister) set(rows []store.MergedIntoRow, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows, f.err = rows, err
}

func mergedRow(src, dst uint32) store.MergedIntoRow {
	return store.MergedIntoRow{SrcZone: src, DstZone: dst, RunID: "run-1", MergedAtMs: 1}
}

func TestMergedZoneCache_FirstReadFailureRefusesToBuild(t *testing.T) {
	boom := errors.New("boom")
	cache, err := NewMergedZoneCache(context.Background(), &fakeMergedZoneLister{err: boom}, time.Second)
	require.ErrorIs(t, err, boom, "without the first read nothing tells 'no merge' from 'could not read'")
	assert.Nil(t, cache)
}

func TestMergedZoneCache_RejectsBadArguments(t *testing.T) {
	_, err := NewMergedZoneCache(context.Background(), nil, time.Second)
	require.Error(t, err)
	_, err = NewMergedZoneCache(context.Background(), &fakeMergedZoneLister{}, 0)
	require.Error(t, err)
}

func TestMergedZoneCache_ResolvesChains(t *testing.T) {
	lister := &fakeMergedZoneLister{rows: []store.MergedIntoRow{mergedRow(5, 6), mergedRow(6, 7)}}
	cache, err := NewMergedZoneCache(context.Background(), lister, time.Second)
	require.NoError(t, err)

	for _, tc := range []struct{ in, want uint32 }{{5, 7}, {6, 7}, {7, 7}, {9, 9}} {
		got, err := cache.Resolve(tc.in)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "zone %d", tc.in)
	}
}

// 成环的映射是库里的真实状态:受影响的区逐次 fail-closed,其余区照常回答。
func TestMergedZoneCache_CycleFailsOnlyTheAffectedZones(t *testing.T) {
	lister := &fakeMergedZoneLister{rows: []store.MergedIntoRow{mergedRow(5, 6), mergedRow(6, 5)}}
	cache, err := NewMergedZoneCache(context.Background(), lister, time.Second)
	require.NoError(t, err)

	_, err = cache.Resolve(5)
	require.ErrorIs(t, err, placement.ErrMergedZoneUnresolvable)
	got, err := cache.Resolve(9)
	require.NoError(t, err)
	assert.Equal(t, uint32(9), got)
}

func TestMergedZoneCache_NilAndStatic(t *testing.T) {
	var absent *MergedZoneCache
	_, err := absent.Resolve(5)
	require.ErrorIs(t, err, ErrMergedZoneCacheUnavailable)
	absent.Start() // 空操作,不 panic
	absent.Stop()

	source := map[uint32]uint32{5: 6}
	static := NewStaticMergedZoneCache(source)
	source[5] = 8 // 入参被拷贝
	got, err := static.Resolve(5)
	require.NoError(t, err)
	assert.Equal(t, uint32(6), got)
	static.Start() // 没有真源可重读
	static.Stop()

	empty := NewStaticMergedZoneCache(nil)
	got, err = empty.Resolve(5)
	require.NoError(t, err)
	assert.Equal(t, uint32(5), got)
}

// 定时重读:读到新映射就替换;读失败保留旧值,不把请求打挂;恢复后接着替换。拍子由单测手动推进,不依赖真实时钟。
func TestMergedZoneCache_RefreshReplacesAndKeepsOldOnFailure(t *testing.T) {
	lister := &fakeMergedZoneLister{rows: []store.MergedIntoRow{mergedRow(5, 6)}}
	cache, err := NewMergedZoneCache(context.Background(), lister, time.Hour)
	require.NoError(t, err)

	served := make(chan struct{}, 1)
	lister.mu.Lock()
	lister.served = served
	lister.mu.Unlock()
	tick := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cache.run(ctx, tick)
	}()
	step := func() {
		tick <- time.Time{}
		<-served
		tick <- time.Time{} // run 取走第二拍,说明上一拍的 refresh 已经全部做完
		<-served
	}

	lister.set([]store.MergedIntoRow{mergedRow(5, 6), mergedRow(6, 7)}, nil)
	step()
	got, err := cache.Resolve(5)
	require.NoError(t, err)
	assert.Equal(t, uint32(7), got, "a new merge is visible after one refresh")

	lister.set(nil, errors.New("db down"))
	step()
	got, err = cache.Resolve(5)
	require.NoError(t, err, "a failed refresh must not take requests down")
	assert.Equal(t, uint32(7), got, "a failed refresh keeps the previous mapping")

	// 撤销合服:映射删掉之后归属回到各自的区。
	lister.set([]store.MergedIntoRow{mergedRow(6, 7)}, nil)
	step()
	got, err = cache.Resolve(5)
	require.NoError(t, err)
	assert.Equal(t, uint32(5), got)

	cancel()
	<-done
}

// Stop 要停得下来:取消在途的读、等 goroutine 退出;可重复调用;Stop 之后 Start 不再起任何东西,Resolve 仍读最后的映射。
func TestMergedZoneCache_StopCancelsInFlightRefresh(t *testing.T) {
	lister := &fakeMergedZoneLister{rows: []store.MergedIntoRow{mergedRow(5, 6)}}
	cache, err := NewMergedZoneCache(context.Background(), lister, time.Millisecond)
	require.NoError(t, err)

	lister.mu.Lock()
	lister.block = make(chan struct{}) // 永远不放行:读只能靠 ctx 取消结束
	lister.mu.Unlock()
	cache.Start()
	cache.Start() // 重复调用不起第二条循环
	cache.Stop()
	cache.Stop()

	done := cache.done
	cache.Start()
	assert.True(t, done == cache.done, "Start after Stop must not restart the timer")

	got, err := cache.Resolve(5)
	require.NoError(t, err)
	assert.Equal(t, uint32(6), got)
}

package routing

// 别名合服的映射缓存(docs/design/player-data-tidb-scaling-plan.md §7.1 落码口径)。
//
// GetPlayerHomeZone / BatchGetPlayerHomeZone 的出口用它把 home_zone 解析成最终区,所以每次调用都要读,不能每次回库。
// 真源是全局库 zone_merged_into;合服极少发生,只靠定时重读,滞后上限是一个周期
// (tools/merge_zone 写完映射后等 2 个周期再跑业务合并任务)。方案里说的「变更通知加版本号」先不做。
//
// 状态所有者:映射整张替换(atomic.Pointer),读路径无锁、不会读到半张表;定时任务是唯一写者。

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"data_service/internal/metrics"
	"data_service/internal/store"

	"shared/placement"

	"github.com/zeromicro/go-zero/core/logx"
)

// mergedZoneRefreshTimeout 是定时重读一次的时限。读超时按失败处理(保留旧值),下一拍再读。
const mergedZoneRefreshTimeout = 3 * time.Second

// ErrMergedZoneCacheUnavailable 表示缓存没有装配起来(nil)。解析不出最终区就不能回答归属,调用方 fail-closed。
var ErrMergedZoneCacheUnavailable = errors.New("merged zone cache not assembled")

// MergedZoneLister 是缓存对真源的依赖:全表读 zone_merged_into。*store.RouteStore 逐字满足,单测注入假实现。
type MergedZoneLister interface {
	ListMergedInto(ctx context.Context) ([]store.MergedIntoRow, error)
}

// MergedZoneCache 是 zone_merged_into 的进程内副本。Resolve 并发安全;Start / Stop 只由进程生命周期 owner 调用,可重复调用。
type MergedZoneCache struct {
	lister   MergedZoneLister // nil = 静态表,没有可重读的真源(NewStaticMergedZoneCache)
	interval time.Duration
	merges   atomic.Pointer[map[uint32]uint32]

	lifecycleMu sync.Mutex
	started     bool
	stopped     bool
	cancel      context.CancelFunc
	done        chan struct{}
}

// NewMergedZoneCache 读一次真源并建缓存。读不到回 error,调用方拒绝启动:启动时没有映射,就没法区分「没有合服」和「读不到」。
// 这里不起任何 goroutine,定时重读由 Start 开始。
func NewMergedZoneCache(ctx context.Context, lister MergedZoneLister, interval time.Duration) (*MergedZoneCache, error) {
	if lister == nil {
		return nil, errors.New("merged zone cache: no zone_merged_into reader")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("merged zone cache: refresh interval %s must be positive", interval)
	}
	c := &MergedZoneCache{lister: lister, interval: interval}
	if err := c.refresh(ctx); err != nil {
		return nil, err
	}
	metrics.PrimeMergedZoneRefresh()
	return c, nil
}

// NewStaticMergedZoneCache 建一个不重读的缓存:redis 模式(只给 LegacyNonGlobal 隔离工具,没有 route store,也没有合服)传 nil,
// 等于没有任何映射;单测传固定映射。Start / Stop 对它是空操作。入参被拷贝。
func NewStaticMergedZoneCache(merges map[uint32]uint32) *MergedZoneCache {
	c := &MergedZoneCache{}
	snapshot := make(map[uint32]uint32, len(merges))
	maps.Copy(snapshot, merges)
	c.merges.Store(&snapshot)
	return c
}

// Resolve 把 zone 解析成最终区,规则只有 placement.ResolveMergedZone 一份。
// 成环等解析失败回 placement.ErrMergedZoneUnresolvable;缓存没装配回 ErrMergedZoneCacheUnavailable。两者调用方都必须 fail-closed。
func (c *MergedZoneCache) Resolve(zone uint32) (uint32, error) {
	if c == nil {
		return 0, ErrMergedZoneCacheUnavailable
	}
	merges := c.merges.Load()
	if merges == nil {
		return 0, ErrMergedZoneCacheUnavailable
	}
	return placement.ResolveMergedZone(*merges, zone)
}

// Start 开始定时重读。静态表、重复调用、Stop 之后调用都是空操作。
func (c *MergedZoneCache) Start() {
	if c == nil || c.lister == nil {
		return
	}
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.started || c.stopped {
		return
	}
	c.started = true
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.done = make(chan struct{})
	ticker := time.NewTicker(c.interval)
	go func() {
		defer close(c.done)
		defer ticker.Stop()
		c.run(ctx, ticker.C)
	}()
}

// Stop 停定时器并等在途的重读结束(在途 SQL 被 ctx 取消,不等满时限)。必须早于关闭 route store 的连接池,否则最后一拍会报假故障。
// 停了之后 Resolve 仍可用,读的是最后一次成功的映射。
func (c *MergedZoneCache) Stop() {
	if c == nil {
		return
	}
	c.lifecycleMu.Lock()
	c.stopped = true
	cancel, done := c.cancel, c.done
	c.lifecycleMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// run 是定时循环。tick 由参数注入,单测手动推进,不依赖真实时钟。
func (c *MergedZoneCache) run(ctx context.Context, tick <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
		opCtx, cancel := context.WithTimeout(ctx, mergedZoneRefreshTimeout)
		err := c.refresh(opCtx)
		cancel()
		if err != nil && ctx.Err() != nil {
			return // 关停时被取消的这一次不算故障
		}
		if err != nil {
			// 保留旧值继续回答:合服极少发生,多滞后几拍由业务合并任务兜底;把请求打挂的代价比滞后大得多。
			metrics.ObserveMergedZoneRefresh(metrics.MergedZoneRefreshError)
			logx.Errorf("[MergedZoneCache] refresh failed, keeping the previous mapping: %v", err)
			continue
		}
		metrics.ObserveMergedZoneRefresh(metrics.MergedZoneRefreshOK)
	}
}

// refresh 读一次真源,有变化才替换。解析不了的映射(成环、目标区为 0)照样替换:这是库里的真实状态,
// 受影响的区由 Resolve 逐次 fail-closed,其余区不受影响;只在变化时打 ERROR,不按拍刷屏。
func (c *MergedZoneCache) refresh(ctx context.Context) error {
	rows, err := c.lister.ListMergedInto(ctx)
	if err != nil {
		return fmt.Errorf("list zone_merged_into: %w", err)
	}
	next := make(map[uint32]uint32, len(rows))
	for _, row := range rows {
		next[row.SrcZone] = row.DstZone
	}
	if cur := c.merges.Load(); cur != nil && maps.Equal(*cur, next) {
		return nil
	}
	c.merges.Store(&next)
	logx.Infof("[MergedZoneCache] mapping changed: %d merged zone(s)", len(next))
	for src := range next {
		if _, err := placement.ResolveMergedZone(next, src); err != nil {
			logx.Errorf("[MergedZoneCache] zone_merged_into is broken, requests resolving this zone will fail closed: %v", err)
		}
	}
	return nil
}

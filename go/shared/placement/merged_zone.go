package placement

// 别名合服的归属解析(docs/design/player-data-tidb-scaling-plan.md §7.1)。
//
// 合服只写一条 zone_merged_into「源区 → 目标区」的映射,玩家行和路由行都不动,home_zone 还是源区。
// 所有按区分组的地方(榜单、频道、市场、公会、进场判断)先把 home_zone 解析成最终区再用。
// 解析规则只有 resolveMergedZone 一份,对外只通过 MergedZones:常驻服务用的不可变快照,MergedZoneCache 定时重读它。
//
// 这几处用原始归属,不解析:RegisterPlayerZone 写入、路由工具、match 写进票和队伍记录的区(比较时才解析)。
// go/db 选库读 storage_id,不看 home_zone。

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// MaxMergedZoneHops 是沿合服映射解析的跳数上限。连续合服(5→6→7)每次加一跳,16 跳远超运营上可能发生的合服次数;
// 超过它只可能是映射表写坏了,按成环同样处理。
const MaxMergedZoneHops = 16

// ErrMergedZoneUnresolvable 表示合服映射解析不出最终区:成环、超过 MaxMergedZoneHops 跳,或目标区是 0。
// 调用方必须 fail-closed:解析错了就是把玩家分进错的区(榜单、频道、市场全错),宁可这次请求失败。
var ErrMergedZoneUnresolvable = errors.New("placement: merged zone chain cycles, exceeds the hop limit or targets zone 0")

// resolveMergedZone 沿 merges(源区 → 目标区)解析 zone 的最终区。zone 不在表里返回自身;
// 恰好 MaxMergedZoneHops 跳走完算成功,再多一跳(或成环)回 ErrMergedZoneUnresolvable。
// 纯函数,不改 merges;merges 为 nil 等于空表。
func resolveMergedZone(merges map[uint32]uint32, zone uint32) (uint32, error) {
	cur := zone
	for hops := 0; ; hops++ {
		next, merged := merges[cur]
		if !merged {
			return cur, nil
		}
		if next == 0 || hops == MaxMergedZoneHops {
			return 0, fmt.Errorf("%w: zone %d stuck at %d after %d hops (next %d)", ErrMergedZoneUnresolvable, zone, cur, hops, next)
		}
		cur = next
	}
}

// MergedZones 是合服映射的不可变快照:构造时校验并把解析结果算好,之后只读,并发安全。
//
// nil 接收者等于没有任何合服映射:Final(z) = z,Members(z) = [z],Merges() = nil。
// 要区分「没有合服」和「还没读到映射」的调用方,必须先判 nil 再用,不能指望这两个方法报错。
type MergedZones struct {
	merges  map[uint32]uint32   // 原始映射:源区 → 目标区
	final   map[uint32]uint32   // 映射里每个源区 → 最终区
	members map[uint32][]uint32 // 有区合入的最终区 → 解析到它的全部原始区,含它自己,升序
}

// NewMergedZones 校验 merges 并建快照。任何一条链成环、超过 MaxMergedZoneHops 跳或目标区是 0,
// 都回 ErrMergedZoneUnresolvable。入参被拷贝,之后改它不影响快照。merges 为 nil 等于空表。
func NewMergedZones(merges map[uint32]uint32) (*MergedZones, error) {
	zones := &MergedZones{
		merges:  make(map[uint32]uint32, len(merges)),
		final:   make(map[uint32]uint32, len(merges)),
		members: make(map[uint32][]uint32),
	}
	maps.Copy(zones.merges, merges)
	for src := range merges {
		final, err := resolveMergedZone(merges, src)
		if err != nil {
			return nil, err
		}
		zones.final[src] = final
		zones.members[final] = append(zones.members[final], src)
	}
	for final, group := range zones.members {
		// final 解析成功就不在映射里,所以不会和 group 里的源区重复。
		group = append(group, final)
		slices.Sort(group)
		zones.members[final] = group
	}
	return zones, nil
}

// Final 返回 zone 沿合服映射解析到的最终区;没有被合走的区返回自身。
func (m *MergedZones) Final(zone uint32) uint32 {
	if m != nil {
		if final, merged := m.final[zone]; merged {
			return final
		}
	}
	return zone
}

// Members 返回最终解析到 zone 的所有原始区,含 zone 自己,升序,用于「zone IN (...)」这类按区查询。
//
// zone 已经被合走时按它的最终区回答,等于 Members(Final(zone)):调用方拿到的永远是完整的一组,不会因为少解析一次、
// 或者别的服务的映射缓存滞后一拍,就只查到源区、漏掉目标区的行。返回值是副本,调用方可以随意改。
func (m *MergedZones) Members(zone uint32) []uint32 {
	final := m.Final(zone)
	if m != nil {
		if group, merged := m.members[final]; merged {
			return slices.Clone(group)
		}
	}
	return []uint32{final}
}

// Merges 返回原始映射(源区 → 目标区)的副本。对外传映射(GetMergedZones)时用它,接收方用 NewMergedZones 重建快照。
func (m *MergedZones) Merges() map[uint32]uint32 {
	if m == nil {
		return nil
	}
	return maps.Clone(m.merges)
}

// DefaultMergedZoneRefreshInterval 是 MergedZoneCache 定时重读的缺省周期。这个周期就是归属解析的滞后上限。
const DefaultMergedZoneRefreshInterval = 5 * time.Second

// mergedZoneRefreshTimeout 是定时重读一次的时限。读超时按失败处理(保留旧值),下一拍再读。
const mergedZoneRefreshTimeout = 3 * time.Second

// MergedZoneReader 读一次完整的合服映射(源区 → 目标区)。data_service、match 读 zone_merged_into,guild、trade 调 GetMergedZones。
type MergedZoneReader func(context.Context) (map[uint32]uint32, error)

// MergedZoneCacheOptions 是 MergedZoneCache 的可选参数。
type MergedZoneCacheOptions struct {
	// Interval 是定时重读的周期。0 = DefaultMergedZoneRefreshInterval,负数拒绝。
	Interval time.Duration
	// OnRefresh 在每次定时重读结束后调用一次:err 非 nil = 这次失败,旧快照继续用。调用方用它计数、打日志。nil = 不观测。
	// 关停时被取消的那一次不回调。启动时的第一次读不回调:它失败时 NewMergedZoneCache 直接回错。
	OnRefresh func(err error)
	// OnChange 在新快照替换旧快照之后调用,包括启动时的第一次读。合服极少发生,调用方用它留一条日志。nil = 不观测。
	OnChange func(*MergedZones)
}

// MergedZoneCache 是合服映射的进程内副本。合服极少发生,只靠定时重读,滞后上限是一个周期
// (合服工具写完映射后等 2 个周期才算全部生效);方案里说的「变更通知加版本号」先不做。
//
// 状态所有者:快照整份替换(atomic.Pointer),读路径无锁、不会读到半张表;定时任务是唯一写者。
// Snapshot 并发安全;Start / Stop 只由进程生命周期 owner 调用,可重复调用。
type MergedZoneCache struct {
	read      MergedZoneReader
	interval  time.Duration
	onRefresh func(error)
	onChange  func(*MergedZones)
	current   atomic.Pointer[MergedZones]

	lifecycleMu sync.Mutex
	started     bool
	stopped     bool
	cancel      context.CancelFunc
	done        chan struct{}
}

// NewMergedZoneCache 读一次真源并建缓存。读不到、或读到的映射过不了 NewMergedZones 的校验都回 error,调用方拒绝启动:
// 启动时没有映射,就没法区分「没有合服」和「读不到」。这里不起任何 goroutine,定时重读由 Start 开始。
func NewMergedZoneCache(ctx context.Context, read MergedZoneReader, opts MergedZoneCacheOptions) (*MergedZoneCache, error) {
	if read == nil {
		return nil, errors.New("merged zone cache: no reader")
	}
	if opts.Interval < 0 {
		return nil, fmt.Errorf("merged zone cache: refresh interval %s must not be negative", opts.Interval)
	}
	if opts.Interval == 0 {
		opts.Interval = DefaultMergedZoneRefreshInterval
	}
	c := &MergedZoneCache{read: read, interval: opts.Interval, onRefresh: opts.OnRefresh, onChange: opts.OnChange}
	if err := c.refresh(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// Snapshot 返回当前快照。nil 接收者回 nil(缓存没装配);建好的缓存恒回非 nil。
// 调用方拿不到快照时必须 fail-closed,不能拿 nil 快照的「没有合服」语义继续走。
func (c *MergedZoneCache) Snapshot() *MergedZones {
	if c == nil {
		return nil
	}
	return c.current.Load()
}

// Start 开始定时重读。nil 接收者、重复调用、Stop 之后调用都是空操作。
func (c *MergedZoneCache) Start() {
	if c == nil {
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

// Stop 停定时器并等在途的重读结束(在途的读被 ctx 取消,不等满时限)。必须早于关闭读数函数用的连接,否则最后一拍会报假故障。
// 停了之后 Snapshot 仍可用,读的是最后一次成功的映射。
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
		// 失败保留旧值继续回答:合服极少发生,多滞后几拍的代价比把请求打挂小得多。
		if c.onRefresh != nil {
			c.onRefresh(err)
		}
	}
}

// refresh 读一次真源,有变化才替换。读到的映射过不了校验(表写坏了)按读失败处理:Final 没有错误出口,
// 坏映射不能进快照,所以只保留旧快照并报错,等人把表修好。
func (c *MergedZoneCache) refresh(ctx context.Context) error {
	merges, err := c.read(ctx)
	if err != nil {
		return fmt.Errorf("read merged zones: %w", err)
	}
	if cur := c.current.Load(); cur != nil && maps.Equal(cur.merges, merges) {
		return nil
	}
	next, err := NewMergedZones(merges)
	if err != nil {
		return fmt.Errorf("zone_merged_into is broken: %w", err)
	}
	c.current.Store(next)
	if c.onChange != nil {
		c.onChange(next)
	}
	return nil
}

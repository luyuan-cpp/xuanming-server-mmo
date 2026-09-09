package snowflakealloc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"shared/snowflake"
)

// unreachableClient 指向一个没人监听的端口。不设 DialTimeout ⇒ clientv3.New 不阻塞,
// 每次操作靠自己的 ctx 超时失败。
func unreachableClient(t *testing.T) *clientv3.Client {
	t.Helper()
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{"127.0.0.1:1"}})
	if err != nil {
		t.Fatalf("clientv3.New: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

func writeBootCache(t *testing.T, path string, ackAgo time.Duration) *cacheRecord {
	t.Helper()
	rec := &cacheRecord{
		Kind:          "boot",
		Cluster:       0,
		Slot:          5,
		UUID:          "cached-uuid",
		Incarnation:   42,
		LastWatermark: snowflake.NowEpochSec() - 10, // 前任 10s 前的水位,地板早已越过
		LastAckWall:   time.Now().Add(-ackAgo).UnixMilli(),
	}
	if err := writeCacheFile(path, rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// bootFromCache 用一个连不上的 etcd 走缓存启动路径,返回 Handle。
func bootFromCache(t *testing.T, cli *clientv3.Client, path string) *Handle {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := AllocateWithKeepAlive(ctx, cli, "boot", "host-boot", Options{
		LeaseTTL:   30,
		Quarantine: 20 * time.Minute,
		FenceAfter: 10 * time.Minute,
		CachePath:  path,
	})
	if err != nil {
		t.Fatalf("缓存在 F 内、etcd 不通,必须能起: %v", err)
	}
	t.Cleanup(h.Close)
	return h
}

// F3(a):etcd 不通期间前任继续发号(借位 / 墙钟回拨),那段时间只有本地高水位在推进。
// 同主机在 F 内重启时地板必须取 max(LastWatermark, LocalHighWaterSec) —— 只看
// LastWatermark 会以一个**罩不住前任已发号段**的地板起来,同槽逐位重号。
func TestBootFromCache_LocalHighWaterRaisesTheFloor(t *testing.T) {
	cli := unreachableClient(t)
	path := filepath.Join(t.TempDir(), "snowflake-boot.json")
	rec := writeBootCache(t, path, time.Minute)
	// 前任最后一次**成功写 etcd** 停在 10s 前,但它之后又(在 etcd 不通时)发到了 now+5。
	rec.LocalHighWaterSec = snowflake.NowEpochSec() + 5
	if err := writeCacheFile(path, rec); err != nil {
		t.Fatal(err)
	}

	h := bootFromCache(t, cli, path)
	if h.GuardEpochSec != rec.LocalHighWaterSec+guardLeadSec {
		t.Fatalf("地板必须是 max(lastWatermark, localHighWaterSec)+lead: got %d want %d",
			h.GuardEpochSec, rec.LocalHighWaterSec+guardLeadSec)
	}

	// 第一个号必须落在前任本地高水位**之后**;修复前地板是 lastWatermark(过去),
	// 首个号会落在 now+1 ≤ localHighWaterSec,与前任逐位重号。
	n := h.NewNode()
	id, err := n.Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if sec, _, _, _ := DecodeID(id); sec <= rec.LocalHighWaterSec {
		t.Fatalf("首个号落在逻辑秒 %d,没越过前任本地高水位 %d:同槽重号", sec, rec.LocalHighWaterSec)
	}
}

// F3(b):etcd 不通时水位 Txn 全失败 —— 本地高水位必须照样每拍推进(它是重启地板的
// 唯一来源),而 LastAckWall / LastWatermark **不许**推进(它们是启动资格与已落盘证据)。
func TestLocalHighWaterAdvancesWhileEtcdIsDownButAckDoesNot(t *testing.T) {
	cli := unreachableClient(t)
	path := filepath.Join(t.TempDir(), "snowflake-boot.json")
	rec := writeBootCache(t, path, time.Minute)

	h := bootFromCache(t, cli, path)
	h.NewNode()

	tick := func() *cacheRecord {
		t.Helper()
		// 短 ctx:putWatermarkLocked 继承它,不必等满 etcdOpTimeout。
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if h.advanceGuard(ctx) {
			t.Fatal("etcd 不通时水位不可能写成功")
		}
		got, err := readCacheFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	first := tick()
	if first.LocalHighWaterSec == 0 {
		t.Fatal("第一拍就必须把本地高水位落盘(修复前:etcd 不通 ⇒ 缓存整拍不写)")
	}
	if !h.watermarkDegraded {
		t.Fatal("缓存启动 = etcd 不通,从第一拍起就该按故障期(Txn 之前落盘)对待")
	}
	time.Sleep(1100 * time.Millisecond)
	second := tick()
	if second.LocalHighWaterSec <= first.LocalHighWaterSec {
		t.Fatalf("本地高水位没有随时钟推进: %d → %d", first.LocalHighWaterSec, second.LocalHighWaterSec)
	}
	if second.LastAckWall != rec.LastAckWall || second.LastWatermark != rec.LastWatermark {
		t.Fatalf("etcd 没确认过,LastAckWall / LastWatermark 不许动: ack=%d(want %d) wm=%d(want %d)",
			second.LastAckWall, rec.LastAckWall, second.LastWatermark, rec.LastWatermark)
	}
}

// F3(c):没有新字段的旧缓存文件必须照样能读、能起(字段是 omitempty,缺省读成 0)。
func TestCacheFile_LegacyRecordWithoutHighWaterFieldsLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snowflake-legacy.json")
	lastWm := snowflake.NowEpochSec() - 10
	legacy := fmt.Sprintf(
		`{"kind":"boot","cluster":0,"slot":5,"uuid":"cached-uuid","incarnation":42,"lastWatermark":%d,"lastAckWall":%d}`,
		lastWm, time.Now().Add(-time.Minute).UnixMilli())
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := readCacheFile(path)
	if err != nil {
		t.Fatalf("旧缓存文件必须仍可读: %v", err)
	}
	if rec.LocalHighWaterSec != 0 || rec.LocalHighWaterMs != 0 {
		t.Fatalf("缺省的新字段必须是 0: %+v", rec)
	}

	h := bootFromCache(t, unreachableClient(t), path)
	if h.GuardEpochSec != lastWm+guardLeadSec {
		t.Fatalf("旧缓存的地板仍应是 lastWatermark+lead: got %d want %d", h.GuardEpochSec, lastWm+guardLeadSec)
	}
}

// etcd 不可达 + 缓存在 F 内:必须用缓存的槽起来,发号器能发号,后台处于"未注册"状态。
// 这是设计稿 §3.5 的弱依赖启动(验收项 3.7-5 的单机版)。
func TestAllocate_BootsFromLocalCacheWhenEtcdUnreachable(t *testing.T) {
	cli := unreachableClient(t)
	path := filepath.Join(t.TempDir(), "snowflake-boot.json")
	rec := writeBootCache(t, path, time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := AllocateWithKeepAlive(ctx, cli, "boot", "host-boot", Options{
		LeaseTTL:   30,
		Quarantine: 20 * time.Minute,
		FenceAfter: 10 * time.Minute,
		CachePath:  path,
	})
	if err != nil {
		t.Fatalf("缓存在 F 内、etcd 不通,必须能起: %v", err)
	}
	defer h.Close()

	if h.Slot != rec.Slot || h.WorkerID != rec.Slot || h.UUID != rec.UUID || h.Incarnation() != rec.Incarnation {
		t.Fatalf("缓存字段没接上: slot=%d worker=%d uuid=%s inc=%d", h.Slot, h.WorkerID, h.UUID, h.Incarnation())
	}
	if h.Registered() || h.Lease() != 0 {
		t.Fatal("etcd 不通时不可能已注册")
	}
	if h.GuardEpochSec != rec.LastWatermark+guardLeadSec {
		t.Fatalf("地板必须是 lastWatermark+lead: got %d want %d", h.GuardEpochSec, rec.LastWatermark+guardLeadSec)
	}

	n := h.NewNode()
	if _, err := n.Generate(); err != nil {
		t.Fatalf("缓存启动后 F 内必须能发号(SeedAck 生效): %v", err)
	}
	if isClosed(h.Lost()) {
		t.Fatal("etcd 不通不是失去所有权,Lost() 不得关闭")
	}
	// 缓存里的 lastAckWall 在注册成功前不得推进(否则 F 规则失去意义)。
	stale, age := h.FenceClock().Stale()
	if stale || age < 50*time.Second {
		t.Fatalf("Ack 年龄必须从缓存时刻起算: stale=%v age=%v", stale, age)
	}
}

// 缓存过期(lastAckWall 早于 F)⇒ 协议上这个槽可能已经被别人申领,不许用,回退到报错(等 etcd)。
func TestAllocate_RejectsExpiredLocalCache(t *testing.T) {
	cli := unreachableClient(t)
	path := filepath.Join(t.TempDir(), "snowflake-boot.json")
	writeBootCache(t, path, 20*time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := AllocateWithKeepAlive(ctx, cli, "boot", "host-boot", Options{
		Quarantine: 20 * time.Minute,
		FenceAfter: 10 * time.Minute,
		CachePath:  path,
	})
	if err == nil {
		t.Fatal("缓存超过 F 必须拒绝启动")
	}
	if errors.Is(err, ErrNoSlotAvailable) || errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("错误应是 etcd 不可达而不是 %v", err)
	}
}

// 缓存的 kind / cluster 与本次不符 ⇒ 不是同一个槽池,不许用。
func TestAllocate_RejectsForeignLocalCache(t *testing.T) {
	cli := unreachableClient(t)
	path := filepath.Join(t.TempDir(), "snowflake-boot.json")
	writeBootCache(t, path, time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := AllocateWithKeepAlive(ctx, cli, "boot", "host-boot", Options{
		ClusterID: 1, Quarantine: 20 * time.Minute, FenceAfter: 10 * time.Minute, CachePath: path,
	}); err == nil {
		t.Fatal("cluster 不同的缓存必须拒绝")
	}
	if _, err := AllocateWithKeepAlive(ctx, cli, "other-kind", "host-boot", Options{
		Quarantine: 20 * time.Minute, FenceAfter: 10 * time.Minute, CachePath: path,
	}); err == nil {
		t.Fatal("kind 不同的缓存必须拒绝")
	}
}

// 没配缓存路径 ⇒ 今天的行为:etcd 不通就起不来。
func TestAllocate_NoCachePathFailsWhenEtcdUnreachable(t *testing.T) {
	cli := unreachableClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := AllocateWithKeepAlive(ctx, cli, "boot", "host-boot", Options{}); err == nil {
		t.Fatal("无缓存且 etcd 不通必须报错")
	}
}

// ---- 缓存写盘策略(设计稿 §7.5-5,用户拍板:稳态零写盘;只在申领 / 故障期 / Close 写) ----

// countCacheWrites 把 writeCacheFileFn 换成带计数的包装,测试结束后还原。计数是原子的:
// 集成测试里后台循环在另一个 goroutine 里(guardMu 下)写盘,测试协程直接读。
func countCacheWrites(t *testing.T) *atomic.Int64 {
	t.Helper()
	orig := writeCacheFileFn
	n := new(atomic.Int64)
	writeCacheFileFn = func(path string, rec *cacheRecord) error {
		n.Add(1)
		return orig(path, rec)
	}
	t.Cleanup(func() { writeCacheFileFn = orig })
	return n
}

// cacheHandle 构造一个带假墙钟的 Handle:*now 就是它眼里的墙钟(闸与 LastAckWall 都用它),
// 测试拨它来模拟长时间运行。不连 etcd(cli 由调用方按需注入);F 2 小时与生产默认一致;
// path 为空 = 关闭缓存。
func cacheHandle(t *testing.T, path string, now *time.Time, cli *clientv3.Client) *Handle {
	t.Helper()
	origin := *now
	h := &Handle{
		Kind:     "unit",
		Slot:     3,
		WorkerID: 3,
		UUID:     "uuid-unit",
		cli:      cli,
		host:     "host-unit",
		lost:     make(chan struct{}),
		fence: snowflake.NewFenceClockWithClocks(2*time.Hour,
			func() time.Duration { return now.Sub(origin) },
			func() time.Time { return *now }),
		r: resolvedOptions{cachePath: path, fenceAfter: 2 * time.Hour,
			quarantine: 4 * time.Hour, ttl: 30, clusterBits: ClusterBits, slotBits: SlotBits, maxSlot: 4095},
	}
	h.cache = cacheRecord{Kind: h.Kind, Slot: h.Slot, UUID: h.UUID}
	h.incarnation.Store(100)
	h.guardWritten = 5000
	return h
}

func ack(h *Handle) {
	h.guardMu.Lock()
	h.ackLocked()
	h.guardMu.Unlock()
}

func mustReadCache(t *testing.T, path string) *cacheRecord {
	t.Helper()
	rec, err := readCacheFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// (a):第一次 Ack(身份落盘)写一次;之后无论多少次 Ack、隔多久(这里一路拨到 3 小时后,
// 比 F=2h 还长),一次都不写 —— **没有周期刷新**(用户拍板:稳态零写盘)。内存记录照样跟着
// 走,Close() / 故障首写落盘用的就是它;稳态下本地高水位推进也只改内存。
func TestCacheWrite_SteadyStateAcksNeverWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	now := time.Unix(1_800_000_000, 0)
	writes := countCacheWrites(t)
	h := cacheHandle(t, path, &now, nil)

	ack(h)
	if writes.Load() != 1 {
		t.Fatalf("申领后的第一次 Ack 必须落盘(身份): writes=%d", writes.Load())
	}
	first := mustReadCache(t, path)
	if first.Incarnation != 100 || first.LastAckWall != now.UnixMilli() || first.LastWatermark != 5000 {
		t.Fatalf("首写内容不对: %+v", first)
	}

	// 稳态:每秒一次 Ack 连做 20 拍,再每分钟一次跨过 3 小时。
	for i := 0; i < 20; i++ {
		now = now.Add(time.Second)
		h.guardWritten++
		ack(h)
	}
	for i := 0; i < 180; i++ {
		now = now.Add(time.Minute)
		h.guardWritten++
		ack(h)
	}
	if writes.Load() != 1 {
		t.Fatalf("稳态 Ack 一次都不许写盘(连 3 小时之后也不许): writes=%d (want 1)", writes.Load())
	}
	if got := mustReadCache(t, path); *got != *first {
		t.Fatalf("磁盘内容不该变: got %+v want %+v", got, first)
	}
	// 内存里的记录必须跟着走(Close() / 故障首写落盘用的就是它)。
	if h.cache.LastAckWall != now.UnixMilli() || h.cache.LastWatermark != 5200 {
		t.Fatalf("内存记录没随 Ack 更新: %+v", h.cache)
	}
	// 每拍 advanceGuard 都会先记本地高水位:稳态下同样只改内存。
	h.guardMu.Lock()
	h.noteLocalHighWaterLocked(6000, 0)
	h.guardMu.Unlock()
	if writes.Load() != 1 || h.cache.LocalHighWaterSec != 6000 {
		t.Fatalf("稳态下本地高水位只改内存: writes=%d mem=%d", writes.Load(), h.cache.LocalHighWaterSec)
	}
	if h.watermarkDegraded {
		t.Fatal("稳态不该处于故障模式")
	}
}

// 身份变化(reclaim 换了 incarnation)必须立刻落盘;身份不变则回到零写盘。
func TestCacheWrite_IncarnationChangeWritesImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	now := time.Unix(1_800_000_000, 0)
	writes := countCacheWrites(t)
	h := cacheHandle(t, path, &now, nil)

	ack(h)
	now = now.Add(time.Second)
	h.incarnation.Store(101) // reclaim 之后的第一次 Ack
	ack(h)
	if writes.Load() != 2 {
		t.Fatalf("incarnation 变了必须立刻写: writes=%d (want 2)", writes.Load())
	}
	if got := mustReadCache(t, path); got.Incarnation != 101 || got.LastAckWall != now.UnixMilli() {
		t.Fatalf("磁盘上没有新身份: %+v", got)
	}
	now = now.Add(time.Second)
	ack(h)
	if writes.Load() != 2 {
		t.Fatalf("身份没再变,回到零写盘: writes=%d", writes.Load())
	}
}

// (b):Close() 必须把最后一次(只在内存里的)Ack 写出去,磁盘上的 LastAckWall 才是真实的
// "最后一次确认"。没有变化时(刚写过)不重复写。
func TestCacheWrite_CloseFlushesTheFinalAck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	now := time.Unix(1_800_000_000, 0)
	writes := countCacheWrites(t)
	h := cacheHandle(t, path, &now, nil)

	ack(h)
	for i := 0; i < 5; i++ {
		now = now.Add(time.Second)
		h.guardWritten++
		ack(h)
	}
	if writes.Load() != 1 {
		t.Fatalf("前置条件:稳态零写盘 writes=%d", writes.Load())
	}
	h.Close()
	if writes.Load() != 2 {
		t.Fatalf("Close() 必须补写最后状态: writes=%d (want 2)", writes.Load())
	}
	if got := mustReadCache(t, path); got.LastAckWall != now.UnixMilli() || got.LastWatermark != 5005 {
		t.Fatalf("Close() 之后磁盘不是最终状态: %+v", got)
	}
	// 幂等:再 Close 一次不写(closing 已置位,直接返回)。
	h.Close()
	if writes.Load() != 2 {
		t.Fatalf("重复 Close 不该再写: writes=%d", writes.Load())
	}
}

// (c):水位写失败(etcd 不通)进入故障模式 —— 那时磁盘上的本地高水位是同主机在 F 内重启时
// 唯一罩得住已发号段的地板,每一拍都得落盘。用毫秒水位驱动(值由调用方给,不必等真实秒针
// 走),每次失败恰好写一次(第一次失败落在 Txn 之后立刻写;之后每拍在 Txn 之前写、Txn 之后
// 不重复写)。
func TestCacheWrite_FailedWatermarkWriteFlushesEveryTick(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	now := time.Unix(1_800_000_000, 0)
	writes := countCacheWrites(t)
	h := cacheHandle(t, path, &now, unreachableClient(t))

	ack(h) // 身份落盘 → 1
	acked := mustReadCache(t, path)

	for i := 1; i <= 3; i++ {
		now = now.Add(time.Second) // 远在 30 分钟以内
		ms := uint64(1_800_000_000_000 + i)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err := h.PutMsWatermark(ctx, ms)
		cancel()
		if err == nil {
			t.Fatal("etcd 不通时毫秒水位不可能写成功")
		}
		if writes.Load() != int64(1+i) {
			t.Fatalf("第 %d 次失败后应恰好累计 %d 次写盘, got %d", i, 1+i, writes.Load())
		}
		got := mustReadCache(t, path)
		if got.LocalHighWaterMs != ms {
			t.Fatalf("失败后本地高水位必须已在磁盘上: got %d want %d", got.LocalHighWaterMs, ms)
		}
		if got.LastAckWall != acked.LastAckWall || got.LastWatermarkMs != acked.LastWatermarkMs {
			t.Fatalf("etcd 没确认过,LastAckWall / LastWatermarkMs 不许动: %+v", got)
		}
	}
	if !h.watermarkDegraded {
		t.Fatal("连续失败后应处于故障期")
	}

	// 同一个值再来一次:本地高水位没变,即使在故障期也不写(没有新东西可落)。
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_ = h.PutMsWatermark(ctx, 1_800_000_000_000+3)
	cancel()
	if writes.Load() != 4 {
		t.Fatalf("高水位没推进就不该写盘: writes=%d", writes.Load())
	}
}

// 故障模式恢复:一次成功 Ack 之后退出故障模式、回到零写盘(不因为"刚从故障期出来"而额外写)。
func TestCacheWrite_RecoveryStopsWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	now := time.Unix(1_800_000_000, 0)
	writes := countCacheWrites(t)
	h := cacheHandle(t, path, &now, unreachableClient(t))

	ack(h)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_ = h.PutMsWatermark(ctx, 1_800_000_000_001)
	cancel()
	if writes.Load() != 2 || !h.watermarkDegraded {
		t.Fatalf("前置条件:一次失败已落盘 writes=%d degraded=%v", writes.Load(), h.watermarkDegraded)
	}

	now = now.Add(time.Second)
	ack(h) // 恢复
	if h.watermarkDegraded {
		t.Fatal("Ack 之后必须退出故障期")
	}
	if writes.Load() != 2 {
		t.Fatalf("恢复那一拍的 Ack 不许写: writes=%d", writes.Load())
	}
	// 恢复之后本地高水位推进也不再写盘(稳态)。
	h.guardMu.Lock()
	h.noteLocalHighWaterLocked(0, 1_800_000_000_002)
	h.guardMu.Unlock()
	if writes.Load() != 2 {
		t.Fatalf("稳态下本地高水位推进不许写盘: writes=%d", writes.Load())
	}
}

// degraded 在 guardMu 下读故障模式标志(后台循环在另一个 goroutine 里改它)。
func degraded(h *Handle) bool {
	h.guardMu.Lock()
	defer h.guardMu.Unlock()
	return h.watermarkDegraded
}

// 稳态零写盘的正确性前提:水位 Txn 必须在 watermarkTxnTimeout 内返回失败,且该超时
// < guardLeadSec —— 失败被发现之前发出的号仍在上一拍已 Ack 水位的前推量之内(allocator.go
// 常量注释)。用一个连不上的端点量:etcd 客户端默认 WaitForReady,RPC 会一直等到 ctx 到期,
// 所以这里量到的就是内部超时本身,不是调用方的 ctx(给的是无限)。下界断言排除"连接被拒
// 立即失败"这种没有真正等超时的情况 —— 那种环境下这个测试量不到想量的东西。
func TestWatermarkTxn_FailsWithinTheLeadBound(t *testing.T) {
	if watermarkTxnTimeout >= guardLeadSec*time.Second {
		t.Fatalf("watermarkTxnTimeout %v 必须 < guardLeadSec %ds", watermarkTxnTimeout, guardLeadSec)
	}
	now := time.Now()
	h := cacheHandle(t, "", &now, unreachableClient(t))
	ops := []struct {
		name string
		run  func() bool
	}{
		{"advanceGuard", func() bool { return h.advanceGuard(context.Background()) }},
		{"PutMsWatermark", func() bool { return h.PutMsWatermark(context.Background(), 1_800_000_000_000) == nil }},
	}
	for _, op := range ops {
		start := time.Now()
		ok := op.run()
		elapsed := time.Since(start)
		if ok {
			t.Fatalf("%s: etcd 不通不可能写成功", op.name)
		}
		if elapsed >= guardLeadSec*time.Second {
			t.Fatalf("%s 用了 %v ≥ 前推量 %ds:失败窗口不再被上一拍已 Ack 的水位罩住", op.name, elapsed, guardLeadSec)
		}
		if elapsed < watermarkTxnTimeout/2 {
			t.Fatalf("%s 只用了 %v:没有等到内部超时 %v,量到的不是 Txn 超时本身", op.name, elapsed, watermarkTxnTimeout)
		}
		t.Logf("%s failed after %v (timeout %v < lead %ds)", op.name, elapsed.Truncate(time.Millisecond), watermarkTxnTimeout, guardLeadSec)
	}
	if !degraded(h) {
		t.Fatal("两次失败之后应处于故障模式")
	}
}

// 缓存文件读写往返 + 原子写(临时文件不残留)。
func TestCacheFile_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "c.json")
	want := &cacheRecord{Kind: "k", Cluster: 2, Slot: 9, UUID: "u", Incarnation: 7, LastWatermark: 100, LastWatermarkMs: 5, LastAckWall: 123}
	if err := writeCacheFile(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := readCacheFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *want {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if _, err := readCacheFile(path + ".tmp"); err == nil {
		t.Fatal("临时文件不应残留")
	}
}

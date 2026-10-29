package main

// fence.go 的 -route-source=db 库围栏(W12,规格 §6.3、§13.1 第 3 条、§13.3 第 4 条)单测。替身见 merge_run_db_test.go。
// 续期 goroutine 的节奏(min(ttl/3, 30s)、瞬时错误重试)由 route_store_test.go 的 routeFenceGuard 用例钉住;这里只证本层的顺序:
// 库先提交 → 镜像确认 → 才返回;镜像失败先清库;失权(续期确定失权 / 镜像更大 epoch)让 check 与写 ctx 一起失败;
// 释放先清库、再按完整值删镜像;redis 源逐字不变。

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const testDBHolder = `{"tool":"tools/merge_zone","run_id":"901-902-20261019T000000Z-aaaaaaaa","fence_epoch":7}`

func testRouteFenceParams() routeFenceParams {
	// src 故意比 dst 大:镜像与取锁一律按 zone 升序。
	return routeFenceParams{src: testDBDst, dst: testDBSrc, runID: testDBRun, purpose: routeFencePurposeMerge,
		manifestPath: "m.json", runTimeout: 2 * time.Hour}
}

func grantingFenceDB(req *routeFenceAcquire) *fakeRouteDB {
	db := &fakeRouteDB{}
	db.acquire = func(r routeFenceAcquire) (routeFenceGrant, error) {
		if req != nil {
			*req = r
		}
		return routeFenceGrant{Epoch: 7, ExpiresAtMs: 5_000_000, HolderJSON: testDBHolder}, nil
	}
	db.release = func(zones []uint32, runID string) (map[uint32]int64, error) {
		out := map[uint32]int64{}
		for _, z := range zones {
			out[z] = int64(routeFenceShards)
		}
		return out, nil
	}
	return db
}

// TestAcquireRouteFence_DBThenMirrorsThenRelease:库先提交(按 zone 升序、TTL = max(timeout+30m, 1h))→ 两区镜像(库里的 holder / epoch /
// 过期时刻)确认之后才返回;释放先清库、再按完整值删镜像;重复释放是空操作。
func TestAcquireRouteFence_DBThenMirrorsThenRelease(t *testing.T) {
	ctx := context.Background()
	var req routeFenceAcquire
	db := grantingFenceDB(&req)
	cache := &fakeRouteCache{}
	var mirrored []string
	cache.setFence = func(zone uint32, holder string, epoch, exp uint64) error {
		mirrored = append(mirrored, fmt.Sprintf("%d %s %d %d", zone, holder, epoch, exp))
		return nil
	}
	var deleted []string
	cache.delFence = func(zone uint32, holder string) (bool, error) {
		deleted = append(deleted, fmt.Sprintf("%d %s", zone, holder))
		return true, nil
	}
	f, err := acquireRouteFence(ctx, db, cache, testRouteFenceParams())
	if err != nil {
		t.Fatalf("acquireRouteFence: %v", err)
	}
	if !reflect.DeepEqual(req.Zones, []uint32{testDBSrc, testDBDst}) || req.TTL != 150*time.Minute || req.RunID != testDBRun ||
		req.Purpose != routeFencePurposeMerge || req.Src != testDBDst || req.Dst != testDBSrc || req.Holder.ManifestPath != "m.json" {
		t.Fatalf("acquire request = %+v", req)
	}
	wantMirror := []string{fmt.Sprintf("%d %s 7 5000000", testDBSrc, testDBHolder), fmt.Sprintf("%d %s 7 5000000", testDBDst, testDBHolder)}
	if !reflect.DeepEqual(mirrored, wantMirror) {
		t.Fatalf("mirrors = %v, want %v", mirrored, wantMirror)
	}
	if err := f.check(); err != nil {
		t.Fatalf("check right after acquiring: %v", err)
	}
	f.release(ctx)
	f.release(ctx)
	if got, want := db.called(), []string{"AcquireFence", "ReleaseFence"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("db calls = %v, want %v", got, want)
	}
	if want := []string{fmt.Sprintf("%d %s", testDBSrc, testDBHolder), fmt.Sprintf("%d %s", testDBDst, testDBHolder)}; !reflect.DeepEqual(deleted, want) {
		t.Fatalf("mirror deletes = %v, want %v (compare-and-delete with the full holder value)", deleted, want)
	}
}

// TestAcquireRouteFence_MirrorFailureReleasesTheDBFence:库已提交、镜像没确认 → 先清库、再删已写的镜像,返回错误(收集清单不会开始)。
func TestAcquireRouteFence_MirrorFailureReleasesTheDBFence(t *testing.T) {
	db := grantingFenceDB(nil)
	cache := &fakeRouteCache{}
	cache.setFence = func(zone uint32, _ string, _, _ uint64) error {
		if zone == testDBDst {
			return fmt.Errorf("%w: WAITAOF acked 0", errRoutePublishNotDurable)
		}
		return nil
	}
	f, err := acquireRouteFence(context.Background(), db, cache, testRouteFenceParams())
	if !errors.Is(err, errRoutePublishNotDurable) || f != nil {
		t.Fatalf("acquire = (%v, %v), want (nil, errRoutePublishNotDurable)", f, err)
	}
	if got, want := db.called(), []string{"AcquireFence", "ReleaseFence"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("db calls = %v, want %v", got, want)
	}
	want := []string{
		fmt.Sprintf("setFenceMirror(%d)", testDBSrc), fmt.Sprintf("setFenceMirror(%d)", testDBDst),
		fmt.Sprintf("deleteFenceMirror(%d)", testDBSrc), fmt.Sprintf("deleteFenceMirror(%d)", testDBDst),
	}
	if got := cache.called(); !reflect.DeepEqual(got, want) {
		t.Fatalf("cache calls = %v, want %v", got, want)
	}
}

// TestAcquireRouteFence_FailedAcquireCleansUpOnlyItsOwnRun:获取报非「被别人持有」的错(提交回包可能丢了)→ 按本次 run_id 清一次;
// 被别人持有 → 什么都不清。
func TestAcquireRouteFence_FailedAcquireCleansUpOnlyItsOwnRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		cleanup bool
	}{
		{"commit-reply-lost", errors.New("invalid connection"), true},
		{"held-by-another-run", fmt.Errorf("%w: zone 901", errRouteFenceHeld), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := grantingFenceDB(nil)
			db.acquire = func(routeFenceAcquire) (routeFenceGrant, error) { return routeFenceGrant{}, tc.err }
			var releasedRun string
			db.release = func(_ []uint32, run string) (map[uint32]int64, error) {
				releasedRun = run
				return nil, nil
			}
			if _, err := acquireRouteFence(context.Background(), db, &fakeRouteCache{}, testRouteFenceParams()); !errors.Is(err, tc.err) {
				t.Fatalf("error = %v, want %v", err, tc.err)
			}
			if got := db.count("ReleaseFence") == 1; got != tc.cleanup || (tc.cleanup && releasedRun != testDBRun) {
				t.Fatalf("cleanup release = %v (run %q), want %v", got, releasedRun, tc.cleanup)
			}
		})
	}
}

// TestRouteFence_RenewLossTripsCheckAndCancelsWrites:续期确定失权 → check() 带 errRouteFenceLost;写 ctx 被看门狗以同一原因取消;
// 释放时库里已不是本次(0 片)→ 不碰镜像。
func TestRouteFence_RenewLossTripsCheckAndCancelsWrites(t *testing.T) {
	ctx := context.Background()
	db := grantingFenceDB(nil)
	db.renew = func([]uint32, string, time.Duration) (map[uint32]uint64, error) {
		return nil, fmt.Errorf("%w: zone 901 renewed 0/64 shards", errRouteFenceLost)
	}
	db.release = func(zones []uint32, _ string) (map[uint32]int64, error) {
		return map[uint32]int64{zones[0]: 0, zones[1]: 0}, nil
	}
	cache := &fakeRouteCache{}
	f, err := acquireRouteFence(ctx, db, cache, testRouteFenceParams())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer f.release(ctx)
	if err := f.guard.renewOnce(ctx); !errors.Is(err, errRouteFenceLost) {
		t.Fatalf("renewOnce = %v, want errRouteFenceLost", err)
	}
	if err := f.check(); !errors.Is(err, errRouteFenceLost) {
		t.Fatalf("check after a lost renewal = %v, want errRouteFenceLost", err)
	}
	f.sleep = func(c context.Context, _ time.Duration) error { return c.Err() }
	wctx := f.writeContext(ctx)
	<-wctx.Done()
	if cause := context.Cause(wctx); !errors.Is(cause, errRouteFenceLost) {
		t.Fatalf("write ctx cause = %v, want errRouteFenceLost", cause)
	}
	f.release(ctx)
	for _, c := range cache.called() {
		if strings.HasPrefix(c, "deleteFenceMirror") {
			t.Fatalf("cache calls = %v: a fence that is no longer ours must not have its mirror deleted", cache.called())
		}
	}
}

// TestRouteFence_WriteContextStaysOpenWhileHeld:仍持有时写 ctx 不被取消;释放停下看门狗。
func TestRouteFence_WriteContextStaysOpenWhileHeld(t *testing.T) {
	ctx := context.Background()
	f, err := acquireRouteFence(ctx, grantingFenceDB(nil), &fakeRouteCache{}, testRouteFenceParams())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	wctx := f.writeContext(ctx)
	if wctx == ctx || wctx.Err() != nil {
		t.Fatalf("write ctx = %v (err %v), want a live derived ctx", wctx, wctx.Err())
	}
	f.release(ctx)
	if wctx.Err() != nil {
		t.Fatalf("release cancelled the write ctx (%v); it only stops the watchdog", context.Cause(wctx))
	}
}

// TestRouteFence_NewerMirrorEpochCountsAsLoss:续期后刷新镜像时发现更大的 fence_epoch(另一个持有者)→ 当作失权;刷新用库里的新过期时刻。
func TestRouteFence_NewerMirrorEpochCountsAsLoss(t *testing.T) {
	ctx := context.Background()
	cache := &fakeRouteCache{}
	f, err := acquireRouteFence(ctx, grantingFenceDB(nil), cache, testRouteFenceParams())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer f.release(ctx)
	var refreshed []string
	cache.setFence = func(zone uint32, holder string, epoch, exp uint64) error {
		refreshed = append(refreshed, fmt.Sprintf("%d %d %d", zone, epoch, exp))
		if zone == testDBDst {
			return fmt.Errorf("%w: merge:in_progress:902", errRouteFenceMirrorStale)
		}
		return nil
	}
	if err := f.refreshMirrors(ctx, map[uint32]uint64{testDBSrc: 9_000_000, testDBDst: 9_000_001}); !errors.Is(err, errRouteFenceMirrorStale) {
		t.Fatalf("refreshMirrors = %v", err)
	}
	if want := []string{fmt.Sprintf("%d 7 9000000", testDBSrc), fmt.Sprintf("%d 7 9000001", testDBDst)}; !reflect.DeepEqual(refreshed, want) {
		t.Fatalf("refreshed = %v, want %v", refreshed, want)
	}
	if err := f.check(); !errors.Is(err, errRouteFenceLost) || !errors.Is(err, errRouteFenceMirrorStale) {
		t.Fatalf("check = %v, want errRouteFenceLost wrapping errRouteFenceMirrorStale", err)
	}
}

// TestAcquireRouteFence_TakeOver:-take-over-fence 走 TakeOverFence(旧 run_id 原样传入),不走 AcquireFence。
func TestAcquireRouteFence_TakeOver(t *testing.T) {
	ctx := context.Background()
	db := grantingFenceDB(nil)
	var prev string
	db.takeOver = func(_ routeFenceAcquire, p string) (routeFenceGrant, error) {
		prev = p
		return routeFenceGrant{Epoch: 8, ExpiresAtMs: 6_000_000, HolderJSON: testDBHolder}, nil
	}
	p := testRouteFenceParams()
	p.takeOverRunID = "901-902-20261018T000000Z-deadbeef"
	f, err := acquireRouteFence(ctx, db, &fakeRouteCache{}, p)
	if err != nil {
		t.Fatalf("take over: %v", err)
	}
	f.release(ctx)
	if prev != p.takeOverRunID || db.count("AcquireFence") != 0 || db.count("TakeOverFence") != 1 {
		t.Fatalf("calls = %v prev = %q", db.called(), prev)
	}
}

// TestAcquireRouteFence_TakeOverMirrorFailureKeepsTheDBFence(B1-1 / MZ2-1):接手已提交、镜像没确认 → 库围栏保留在本次 run_id 上
// (不调 ReleaseFence、不删任何镜像),错误写明围栏归谁、fence_epoch 与续跑指引。接手的前提是上一次已写到一半,
// 这时释放 = 在半合服 / 半撤销状态下解封。首跑的同一情形照旧先清库(TestAcquireRouteFence_MirrorFailureReleasesTheDBFence)。
func TestAcquireRouteFence_TakeOverMirrorFailureKeepsTheDBFence(t *testing.T) {
	const prev = "901-902-20261018T000000Z-deadbeef"
	db := grantingFenceDB(nil)
	db.takeOver = func(routeFenceAcquire, string) (routeFenceGrant, error) {
		return routeFenceGrant{Epoch: 8, ExpiresAtMs: 6_000_000, HolderJSON: testDBHolder}, nil
	}
	cache := &fakeRouteCache{}
	cache.setFence = func(zone uint32, _ string, _, _ uint64) error {
		if zone == testDBDst {
			return fmt.Errorf("%w: WAITAOF acked 0", errRoutePublishNotDurable)
		}
		return nil
	}
	p := testRouteFenceParams()
	p.takeOverRunID = prev
	f, err := acquireRouteFence(context.Background(), db, cache, p)
	if !errors.Is(err, errRoutePublishNotDurable) || f != nil {
		t.Fatalf("take over = (%v, %v), want (nil, errRoutePublishNotDurable)", f, err)
	}
	for _, want := range []string{"run_id=" + testDBRun, "run_id=" + prev, "fence_epoch=8", "-manifest-path m.json",
		"-take-over-fence " + testDBRun, mergeFenceKey(testDBDst)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error lacks %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "this run wrote nothing") {
		t.Fatalf("a taken-over fence is not a fresh acquisition:\n%v", err)
	}
	if got, want := db.called(), []string{"TakeOverFence"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("db calls = %v, want %v (the taken-over fence must stay in zone_merge_fence)", got, want)
	}
	if got, want := cache.called(), []string{fmt.Sprintf("setFenceMirror(%d)", testDBSrc), fmt.Sprintf("setFenceMirror(%d)", testDBDst)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cache calls = %v, want %v (no mirror may be deleted)", got, want)
	}
}

// TestAcquireRouteFence_TakeOverFailureNeverCleansUp(B1-1 / MZ2-1):接手事务报错时一律不做清理性 ReleaseFence。结果不明(回包丢了)
// 时提交可能已生效、围栏已归本次,清掉就是解封;错误同时点出两个 run_id,由运维查库再接手。Held / InvalidArgument 原样返回。
func TestAcquireRouteFence_TakeOverFailureNeverCleansUp(t *testing.T) {
	const prev = "901-902-20261018T000000Z-deadbeef"
	for _, tc := range []struct {
		name  string
		err   error
		wants []string
	}{
		{"commit-reply-lost", errors.New("invalid connection"), []string{"run_id=" + prev, "run_id=" + testDBRun, "shard=63", "-manifest-path m.json"}},
		{"held-by-a-third-run", fmt.Errorf("%w: zone 901", errRouteFenceHeld), nil},
		{"mismatched-fence", fmt.Errorf("%w: zone pair", errRouteInvalidArgument), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := grantingFenceDB(nil)
			db.takeOver = func(routeFenceAcquire, string) (routeFenceGrant, error) { return routeFenceGrant{}, tc.err }
			p := testRouteFenceParams()
			p.takeOverRunID = prev
			cache := &fakeRouteCache{}
			f, err := acquireRouteFence(context.Background(), db, cache, p)
			if !errors.Is(err, tc.err) || f != nil {
				t.Fatalf("take over = (%v, %v), want (nil, %v)", f, err, tc.err)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error lacks %q:\n%v", want, err)
				}
			}
			if got, want := db.called(), []string{"TakeOverFence"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("db calls = %v, want %v (no cleanup release on the take-over path)", got, want)
			}
			if len(cache.called()) != 0 {
				t.Fatalf("cache calls = %v, want none", cache.called())
			}
		})
	}
}

// TestAcquireRouteFence_ResumedRunKeepsTheDBFence(B1-1):合服续跑(keepOnFailure:清单已存在且步骤 7 未标记完成,上一次的围栏已过期、
// 不带 -take-over-fence)与接手同一口径 —— 获取之后镜像没确认、获取事务结果不明,都不调 ReleaseFence、不删镜像,错误给出本次 run_id
// 与接手指引;Held 原样返回。首跑(keepOnFailure=false)照旧先清库(TestAcquireRouteFence_MirrorFailureReleasesTheDBFence)。
func TestAcquireRouteFence_ResumedRunKeepsTheDBFence(t *testing.T) {
	t.Run("mirror-not-confirmed", func(t *testing.T) {
		db := grantingFenceDB(nil)
		cache := &fakeRouteCache{}
		cache.setFence = func(zone uint32, _ string, _, _ uint64) error {
			if zone == testDBDst {
				return fmt.Errorf("%w: WAITAOF acked 0", errRoutePublishNotDurable)
			}
			return nil
		}
		p := testRouteFenceParams()
		p.keepOnFailure = true
		f, err := acquireRouteFence(context.Background(), db, cache, p)
		if !errors.Is(err, errRoutePublishNotDurable) || f != nil {
			t.Fatalf("resumed acquire = (%v, %v), want (nil, errRoutePublishNotDurable)", f, err)
		}
		for _, want := range []string{"run_id=" + testDBRun, "fence_epoch=7", "续跑", "-manifest-path m.json",
			"-take-over-fence " + testDBRun, mergeFenceKey(testDBDst)} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error lacks %q:\n%v", want, err)
			}
		}
		if strings.Contains(err.Error(), "this run wrote nothing") {
			t.Fatalf("a resumed run must not claim the zones are clean:\n%v", err)
		}
		if got, want := db.called(), []string{"AcquireFence"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("db calls = %v, want %v (the fence of a resumed run must stay in zone_merge_fence)", got, want)
		}
		if got, want := cache.called(), []string{fmt.Sprintf("setFenceMirror(%d)", testDBSrc), fmt.Sprintf("setFenceMirror(%d)", testDBDst)}; !reflect.DeepEqual(got, want) {
			t.Fatalf("cache calls = %v, want %v (no mirror may be deleted)", got, want)
		}
	})
	// 镜像键里是旧格式值(没有 fence_epoch):leader 不覆盖它,文案不能再说「leader 会按库补镜像」,改为先处理这一把键。
	t.Run("foreign-mirror", func(t *testing.T) {
		db := grantingFenceDB(nil)
		cache := &fakeRouteCache{}
		cache.setFence = func(uint32, string, uint64, uint64) error {
			return fmt.Errorf("%w: merge:in_progress:%d", errRouteFenceMirrorForeign, testDBSrc)
		}
		p := testRouteFenceParams()
		p.keepOnFailure = true
		_, err := acquireRouteFence(context.Background(), db, cache, p)
		if !errors.Is(err, errRouteFenceMirrorForeign) {
			t.Fatalf("resumed acquire over a foreign mirror = %v, want errRouteFenceMirrorForeign", err)
		}
		if !strings.Contains(err.Error(), "不是库围栏的镜像") || strings.Contains(err.Error(), "对账 leader 会按库补镜像") ||
			!strings.Contains(err.Error(), "-take-over-fence "+testDBRun) {
			t.Fatalf("foreign-mirror guidance:\n%v", err)
		}
		if got, want := db.called(), []string{"AcquireFence"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("db calls = %v, want %v", got, want)
		}
	})
	for _, tc := range []struct {
		name  string
		err   error
		wants []string
	}{
		{"commit-reply-lost", errors.New("invalid connection"), []string{"run_id=" + testDBRun, "shard=63", "-take-over-fence " + testDBRun, "-manifest-path m.json"}},
		{"held-by-another-run", fmt.Errorf("%w: zone 901", errRouteFenceHeld), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := grantingFenceDB(nil)
			db.acquire = func(routeFenceAcquire) (routeFenceGrant, error) { return routeFenceGrant{}, tc.err }
			p := testRouteFenceParams()
			p.keepOnFailure = true
			cache := &fakeRouteCache{}
			f, err := acquireRouteFence(context.Background(), db, cache, p)
			if !errors.Is(err, tc.err) || f != nil {
				t.Fatalf("resumed acquire = (%v, %v), want (nil, %v)", f, err, tc.err)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error lacks %q:\n%v", want, err)
				}
			}
			if got, want := db.called(), []string{"AcquireFence"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("db calls = %v, want %v (no cleanup release for a resumed run)", got, want)
			}
			if len(cache.called()) != 0 {
				t.Fatalf("cache calls = %v, want none", cache.called())
			}
		})
	}
}

// TestRouteFence_ReleaseAndCleanupAreTimeBounded(MZ2-6):释放与首跑获取失败后的清理都带时限(routeFenceReleaseTimeout)——
// 路由库 DSN 没有读写超时,库网络黑洞时不能卡住退出。调用方给的是 context.Background()。
func TestRouteFence_ReleaseAndCleanupAreTimeBounded(t *testing.T) {
	db := grantingFenceDB(nil)
	var bounded []bool
	db.releaseCtx = func(ctx context.Context) {
		_, ok := ctx.Deadline()
		bounded = append(bounded, ok)
	}
	f, err := acquireRouteFence(context.Background(), db, &fakeRouteCache{}, testRouteFenceParams())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	f.release(context.Background())
	db.acquire = func(routeFenceAcquire) (routeFenceGrant, error) {
		return routeFenceGrant{}, errors.New("invalid connection")
	}
	if _, err := acquireRouteFence(context.Background(), db, &fakeRouteCache{}, testRouteFenceParams()); err == nil {
		t.Fatal("a failed acquisition must return its error")
	}
	if want := []bool{true, true}; !reflect.DeepEqual(bounded, want) {
		t.Fatalf("ReleaseFence ctx has a deadline = %v, want %v (release, then the cleanup of a failed acquisition)", bounded, want)
	}
}

// TestRouteFenceAbortLost(B1-4 / MZ2-4):本地判据先说失权就用它;本地仍判持有、而本步骤带回的错误里有批事务内的 fencing token 复核
// 判的 errRouteFenceLost(被接手而本地下一次续期还没到)时用后者 —— 中止文案改成「先查清是谁接手」,不再说「仍归本次、对自己接手」。
func TestRouteFenceAbortLost(t *testing.T) {
	local := fmt.Errorf("%w: local deadline passed", errRouteFenceLost)
	batch := fmt.Errorf("remap a batch of 2 players: %w", fmt.Errorf("%w: zone 901 holds run_id=%q", errRouteFenceLost, "other-run"))
	other := fmt.Errorf("%w: WAITAOF acked 0", errRoutePublishNotDurable)
	for _, tc := range []struct {
		name  string
		check error
		errs  []error
		want  error
	}{
		{"local-wins", local, []error{batch}, local},
		{"batch-token", nil, []error{other, batch}, batch},
		{"nil-entries", nil, []error{nil, batch}, batch},
		{"not-a-loss", nil, []error{other}, nil},
		{"nothing", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := routeFenceAbortLost(tc.check, tc.errs...); got != tc.want {
				t.Fatalf("routeFenceAbortLost = %v, want %v", got, tc.want)
			}
		})
	}
	msg := routeFenceKeptAbortMessage("step 5 failed", fencedOpMerge, testDBSrc, testDBDst, testDBRun, "m.json", routeFenceAbortLost(nil, batch))
	if !strings.Contains(msg, "失去合服围栏") || strings.Contains(msg, "LEFT IN PLACE") {
		t.Fatalf("a batch-detected loss must produce the lost message:\n%s", msg)
	}
}

// TestAcquireRouteFence_DryRunOnlyReads:dry-run 只读库:别的 run 持有即拒(-take-over-fence 点名的除外),不写库、不写镜像,check 恒 nil。
func TestAcquireRouteFence_DryRunOnlyReads(t *testing.T) {
	ctx := context.Background()
	db := &fakeRouteDB{}
	db.readFence = func(zone uint32) (routeFenceState, error) {
		if zone == testDBSrc {
			return routeFenceState{Present: true, RunID: "other-run", Active: true, HolderJSON: "{}"}, nil
		}
		return routeFenceState{}, nil
	}
	cache := &fakeRouteCache{}
	p := testRouteFenceParams()
	p.dryRun = true
	if _, err := acquireRouteFence(ctx, db, cache, p); err == nil || !strings.Contains(err.Error(), "another merge is already fencing zone 901") ||
		!strings.Contains(err.Error(), "-take-over-fence other-run") {
		// 撞上别人的围栏时文案给出唯一的出路 -take-over-fence <run_id>(MZ2-5:工具没有别的释放入口)。
		t.Fatalf("dry-run against a live fence: error = %v", err)
	}
	p.takeOverRunID = "other-run"
	f, err := acquireRouteFence(ctx, db, cache, p)
	if err != nil {
		t.Fatalf("dry-run with -take-over-fence: %v", err)
	}
	if f.check() != nil || f.writeContext(ctx) != ctx {
		t.Fatal("a dry-run fence must never trip and never derive a write ctx")
	}
	f.release(ctx)
	for _, c := range db.called() {
		if c != "ReadFence" {
			t.Fatalf("db calls = %v: dry-run may only read", db.called())
		}
	}
	if len(cache.called()) != 0 {
		t.Fatalf("cache calls = %v: dry-run must not touch the mirrors", cache.called())
	}
}

// TestAcquireRunFence_RedisSourceUnchanged:rb == nil 时与今天的 acquireMergeFence 发出同样的 Redis 命令(dry-run:两把 GET);
// redis 源不接受 -take-over-fence。
func TestAcquireRunFence_RedisSourceUnchanged(t *testing.T) {
	ctx := context.Background()
	p := testRouteFenceParams()
	p.dryRun = true
	reply := func(_ int, cmd redis.Cmder) (any, error) {
		if cmd.Name() == "get" {
			return nil, redis.Nil
		}
		return nil, fmt.Errorf("unexpected %v", cmd.Args())
	}
	assertSameRedisTraffic(t, reply,
		func(rdb *redis.Client) error {
			_, err := acquireMergeFence(ctx, rdb, p.src, p.dst, p.runID, p.manifestPath, p.runTimeout, true)
			return err
		},
		func(rdb *redis.Client) error {
			f, err := acquireRunFence(ctx, nil, rdb, p)
			if err == nil {
				if _, ok := f.(*mergeFence); !ok || f.check() != nil || f.writeContext(ctx) != ctx {
					return fmt.Errorf("redis source returned %T", f)
				}
			}
			return err
		})
	p.takeOverRunID = "old-run"
	rdb, _ := newFakeRouteRedis(t, reply)
	if _, err := acquireRunFence(ctx, nil, rdb, p); err == nil {
		t.Fatal("-take-over-fence must be refused on the redis source")
	}
}

func TestCheckTakeOverFenceFlag(t *testing.T) {
	ok := options{mode: "merge", routeSource: routeSourceDB, takeOverFenceRunID: testDBRun}
	for _, tc := range []struct {
		name string
		edit func(o *options)
		want string
	}{
		{"merge-db", func(*options) {}, ""},
		{"unmerge-db", func(o *options) { o.mode = "unmerge" }, ""},
		{"unset", func(o *options) { *o = options{mode: "merge", routeSource: routeSourceRedis} }, ""},
		{"redis", func(o *options) { o.routeSource = routeSourceRedis }, "only applies to -route-source=db"},
		{"relocate", func(o *options) { o.mode = "relocate" }, "only applies to -mode merge and -mode unmerge"},
		{"backfill", func(o *options) { o.backfill = true }, "only applies to -mode merge and -mode unmerge"},
		{"bad-run-id", func(o *options) { o.takeOverFenceRunID = "run id; DROP" }, "is not a run_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := ok
			tc.edit(&o)
			err := checkTakeOverFenceFlag(o)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("checkTakeOverFenceFlag: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestRouteFenceKeptAbortMessage:db 源的中止文案给 -take-over-fence 指引、不给「DEL 镜像」指引;失权时改成「先查清是谁接手」。
func TestRouteFenceKeptAbortMessage(t *testing.T) {
	const manifest = "merge_901_to_902_test.json"
	kept := routeFenceKeptAbortMessage("step X failed", fencedOpMerge, testDBSrc, testDBDst, testDBRun, manifest, nil)
	for _, want := range []string{"step X failed", "zone_merge_fence", "run_id=" + testDBRun, "-take-over-fence " + testDBRun,
		"merge:in_progress:901", "merge:in_progress:902", "不要手工 DEL 镜像", "-manifest-path " + manifest} {
		if !strings.Contains(kept, want) {
			t.Fatalf("kept message lacks %q:\n%s", want, kept)
		}
	}
	if strings.Contains(kept, "(4) DEL") {
		t.Fatalf("db-source message must not tell the operator to DEL the mirror:\n%s", kept)
	}
	lost := routeFenceKeptAbortMessage("step X failed", fencedOpUnmerge, testDBSrc, testDBDst, testDBRun, manifest,
		fmt.Errorf("%w: renewed 0/64", errRouteFenceLost))
	// 失权文案不给接手指引:不出现「对本次 run_id 接手」与 LEFT IN PLACE(原来比对的「立即用原命令加 -take-over-fence」在保留文案
	// 插进 -manifest-path 之后两种文案里都不再出现,那条否定断言恒真,改比对接手命令本身)。
	if !strings.Contains(lost, "失去合服围栏") || strings.Contains(lost, "-take-over-fence "+testDBRun) || strings.Contains(lost, "LEFT IN PLACE") {
		t.Fatalf("lost message:\n%s", lost)
	}
}

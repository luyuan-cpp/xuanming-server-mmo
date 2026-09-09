//go:build integration

package snowflakealloc

// 集成测试:需要本地 etcd (127.0.0.1:2379) 在跑。
// 启动 etcd: pwsh tools/scripts/dev_tools.ps1 -Command etcd-up
// 跑测试:    go test -tags=integration ./snowflakealloc/...
//
// 这些用例验证槽位分配的核心不变量(设计稿 docs/design/node-id-overhaul-plan-20260908.md):
//   1. 从 0 开始分配、从没用过的槽优先(Snowflake worker_id=0 合法,与 NodeInfo.NodeId 不同)
//   2. 同 hostname 优雅重启复用同槽;复用即消费 released(2.0b)
//   3. 不同 hostname / 并发申领互不相同;不同 kind / cluster 互相隔离
//   4. 优雅退出后的槽在隔离期 Q 内不被别人申领;Q 过后最久未用者优先
//   5. lease 抖动 → reclaim 而不是自杀;槽被别的 uuid 接管 → Lost()
//   6. 水位:交出发号器时已落盘、单调不回退、带归属校验
//   7. 历史事故回归:2026-09-02 同主机第二个进程不得抢活着的槽
//
// 用例之间通过独立的 kind 隔离 etcd 状态。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"shared/snowflake"
)

const testEtcdEndpoint = "127.0.0.1:2379"

var (
	kindCounter   int
	kindCounterMu sync.Mutex
)

// uniqueKind 给每个 test case 一个独占的 kind,避免相互污染。
func uniqueKind(t *testing.T) string {
	kindCounterMu.Lock()
	defer kindCounterMu.Unlock()
	kindCounter++
	return fmt.Sprintf("test-%d-%d", time.Now().UnixNano(), kindCounter)
}

func newTestClient(t *testing.T) *clientv3.Client {
	t.Helper()
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{testEtcdEndpoint},
		DialTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Skipf("etcd not available at %s: %v", testEtcdEndpoint, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := cli.Get(ctx, "_ping"); err != nil {
		cli.Close()
		t.Skipf("etcd ping failed: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

func cleanupKind(t *testing.T, cli *clientv3.Client, kind string) {
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = cli.Delete(ctx, "/snowflake/"+kind+"/", clientv3.WithPrefix())
		_, _ = cli.Delete(ctx, "/legacy-"+kind+"/", clientv3.WithPrefix())
	})
}

func alloc(t *testing.T, cli *clientv3.Client, kind, host string, opts Options) *Handle {
	t.Helper()
	if opts.LeaseTTL == 0 {
		opts.LeaseTTL = 30
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := AllocateWithKeepAlive(ctx, cli, kind, host, opts)
	if err != nil {
		t.Fatalf("allocate %s/%s: %v", kind, host, err)
	}
	return h
}

func getKV(t *testing.T, cli *clientv3.Client, key string) *clientv3.GetResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := cli.Get(ctx, key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return resp
}

// 干净环境下第一次分配返回 0(从没用过的槽里最小的)。
func TestAllocateStartsFromZero(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	hd := alloc(t, cli, kind, "host-A", Options{})
	defer hd.Close()

	if hd.Slot != 0 || hd.WorkerID != 0 || hd.Cluster != 0 {
		t.Fatalf("expected first slot=0, got slot=%d worker=%d cluster=%d", hd.Slot, hd.WorkerID, hd.Cluster)
	}
	if hd.Incarnation() <= 0 || !hd.Registered() || hd.Lease() == 0 {
		t.Fatalf("inc=%d registered=%v lease=%x", hd.Incarnation(), hd.Registered(), hd.Lease())
	}
	// 申领后立刻就有水位(否则它在别人眼里仍是"从没用过",隔离期对它无效)。
	if wm, _, err := readWatermarkFloor(context.Background(), cli, kind, hd.r, hd.Slot); err != nil || wm == 0 {
		t.Fatalf("watermark right after claim: %d err=%v", wm, err)
	}
}

// 同 hostname 优雅退出后重启必须复用同槽(released 标记 + 三元组同 lease),
// 并且复用 Txn 要把 released 删掉。
func TestAllocateSameHostnameReusesAfterClose(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	hd1 := alloc(t, cli, kind, "host-A", Options{})
	slot1 := hd1.Slot
	hd1.Close()
	if r := getKV(t, cli, releasedKey(kind, 0, "host-A")); len(r.Kvs) != 1 {
		t.Fatal("Close 必须写 released 标记")
	}

	hd2 := alloc(t, cli, kind, "host-A", Options{})
	defer hd2.Close()
	if hd2.Slot != slot1 {
		t.Fatalf("expected same slot=%d for same hostname after graceful close, got %d", slot1, hd2.Slot)
	}
	if r := getKV(t, cli, releasedKey(kind, 0, "host-A")); len(r.Kvs) != 0 {
		t.Fatal("复用 Txn 必须删掉 released(2.0b)")
	}
	if r := getKV(t, cli, slotKey(kind, 0, slot1)); len(r.Kvs) != 1 || string(r.Kvs[0].Value) != hd2.UUID ||
		clientv3.LeaseID(r.Kvs[0].Lease) != hd2.Lease() {
		t.Fatal("复用后 slots key 必须挂在新进程的 uuid / lease 上")
	}
}

// 2.0b 回归:A 优雅退出 → A' 同主机复用成功 → A” 在 TTL 内再起,**不得**拿到 A 的槽
// (修复前:released 标记还在,A” 凭它把活着的 A' 的槽抢走,A' 收到 Lost 自杀)。
func TestReleasedMarkerConsumedOnReuse_ThirdProcessMustNotSteal(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	a := alloc(t, cli, kind, "host-X", Options{})
	slotA := a.Slot
	a.Close()

	a1 := alloc(t, cli, kind, "host-X", Options{})
	defer a1.Close()
	if a1.Slot != slotA {
		t.Fatalf("A' 应复用 A 的槽 %d, got %d", slotA, a1.Slot)
	}

	a2 := alloc(t, cli, kind, "host-X", Options{})
	defer a2.Close()
	if a2.Slot == slotA {
		t.Fatalf("A'' 在 TTL 内拿到了活着的 A' 的槽 %d:released 没被复用消费掉", slotA)
	}
	select {
	case <-a1.Lost():
		t.Fatal("A' 被第三个进程抢走了所有权")
	case <-time.After(1500 * time.Millisecond):
	}
}

// 不同 hostname 同 kind 必须拿到不同槽。
func TestAllocateDifferentHostnamesUnique(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	hdA := alloc(t, cli, kind, "host-A", Options{})
	defer hdA.Close()
	hdB := alloc(t, cli, kind, "host-B", Options{})
	defer hdB.Close()
	if hdA.Slot == hdB.Slot {
		t.Fatalf("different hostnames should get different slots, both got %d", hdA.Slot)
	}
}

// 并发申领 N 个不同 hostname,所有槽必须互不相同。
func TestAllocateConcurrentUnique(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	const N = 20
	var mu sync.Mutex
	all := make(map[uint64]bool)
	handles := make([]*Handle, 0, N)
	defer func() {
		for _, h := range handles {
			h.Close()
		}
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			host := fmt.Sprintf("host-%d", idx)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			hd, err := AllocateWithKeepAlive(ctx, cli, kind, host, Options{LeaseTTL: 30})
			if err != nil {
				errCh <- fmt.Errorf("alloc %s: %v", host, err)
				return
			}
			mu.Lock()
			handles = append(handles, hd)
			if all[hd.Slot] {
				errCh <- fmt.Errorf("duplicate slot=%d (host=%s)", hd.Slot, host)
			}
			all[hd.Slot] = true
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent uniqueness violated: %v", err)
	}
	if len(all) != N {
		t.Fatalf("expected %d unique slots, got %d", N, len(all))
	}
}

// 不同 kind(guild / scene-manager)互不干扰,各自从 0 开始。
func TestAllocateDifferentKindsIsolated(t *testing.T) {
	cli := newTestClient(t)
	kindA, kindB := uniqueKind(t), uniqueKind(t)
	cleanupKind(t, cli, kindA)
	cleanupKind(t, cli, kindB)

	hdA := alloc(t, cli, kindA, "shared-host", Options{})
	defer hdA.Close()
	hdB := alloc(t, cli, kindB, "shared-host", Options{})
	defer hdB.Close()
	if hdA.Slot != 0 || hdB.Slot != 0 {
		t.Fatalf("expected both kinds to start from 0, got A=%d B=%d", hdA.Slot, hdB.Slot)
	}
}

// 同 kind 不同 cluster:各自 slot 0,但合成的 worker id 不同(cluster 段),
// 同一 etcd 里两个集群天然不撞。
func TestAllocateClustersIsolatedAndComposed(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	c0 := alloc(t, cli, kind, "h", Options{ClusterID: 0})
	defer c0.Close()
	c1 := alloc(t, cli, kind, "h", Options{ClusterID: 1})
	defer c1.Close()
	if c0.Slot != 0 || c1.Slot != 0 {
		t.Fatalf("slots: c0=%d c1=%d", c0.Slot, c1.Slot)
	}
	if c0.WorkerID != 0 || c1.WorkerID != 1<<SlotBits {
		t.Fatalf("workers: c0=%d c1=%d", c0.WorkerID, c1.WorkerID)
	}
	if cl, sl := DecodeWorkerID(c1.WorkerID, ClusterBits, SlotBits); cl != 1 || sl != 0 {
		t.Fatalf("decode(%d)=(%d,%d)", c1.WorkerID, cl, sl)
	}
	// 发号器吃合成后的 worker id,DecodeID 能拆回 cluster。
	n := c1.NewNode()
	id, err := n.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, cl, sl, _ := DecodeID(id); cl != 1 || sl != 0 {
		t.Fatalf("id %d decodes to cluster=%d slot=%d", id, cl, sl)
	}
}

// login 布局:13 位 node 段切 [cluster3][slot10],MaxSlot=1023,worker=(cluster<<10)|slot。
func TestAllocateLoginLayout(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	h := alloc(t, cli, kind, "h", Options{ClusterBits: 3, SlotBits: 10, ClusterID: 2})
	defer h.Close()
	if h.r.maxSlot != 1023 {
		t.Fatalf("maxSlot=%d", h.r.maxSlot)
	}
	if h.WorkerID != 2<<10|h.Slot {
		t.Fatalf("worker=%d slot=%d", h.WorkerID, h.Slot)
	}
}

// host-A 优雅退出后,host-B **不得**在隔离期内拿到同一个槽。
//
// ⚠️ 本用例曾经断言的是**相反**的行为("Close 后 host-B 可以立即抢到原 worker id"),
// 并把它当作卖点。那正是一个真实缺陷:最小空闲选号 ⇒ 刚释放的槽就是下一个启动者
// 优先拿到的;新持有者若时钟落后于前任,它的 now+1 仍可能 ≤ 前任最后发号秒。
// 现在由隔离期 Q(默认 4h)+ 水位墓碑挡住,与 lease 是否 Revoke 无关。
func TestAllocateNoImmediateReclaimAfterClose(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	hdA := alloc(t, cli, kind, "host-A", Options{})
	slotA := hdA.Slot
	hdA.Close()
	// 就算 lease 立刻没了(模拟 TTL 过期),水位墓碑也要挡住。
	if _, err := cli.Revoke(context.Background(), hdA.Lease()); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	hdB := alloc(t, cli, kind, "host-B", Options{})
	defer hdB.Close()
	if hdB.Slot == slotA {
		t.Fatalf("host-B 在隔离期内拿到了 host-A 刚释放的槽 %d:水位墓碑没生效", slotA)
	}
}

// 隔离期语义完整回合:Q 内所有槽都被墓碑挡住 ⇒ ErrNoSlotAvailable(fail-closed);
// Q 过后最久未用的槽被选中。
func TestQuarantine_FailsClosedThenReleasesLeastRecentlyUsed(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	opts := Options{MaxSlot: 1, Quarantine: 4 * time.Second, FenceAfter: 2 * time.Second}
	a := alloc(t, cli, kind, "host-A", opts)
	slotA := a.Slot
	a.Close()
	if _, err := cli.Revoke(context.Background(), a.Lease()); err != nil {
		t.Fatal(err)
	}
	b := alloc(t, cli, kind, "host-B", opts)
	defer b.Close()
	if b.Slot == slotA {
		t.Fatalf("B 拿到了刚释放的槽 %d", slotA)
	}

	// 池 = {0,1}:B 占一个,A 的那个在隔离期 ⇒ 无候选。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := AllocateWithKeepAlive(ctx, cli, kind, "host-C", opts)
	cancel()
	if !errors.Is(err, ErrNoSlotAvailable) {
		t.Fatalf("隔离期内必须 fail-closed(ErrNoSlotAvailable), got %v", err)
	}

	// 等 Q 过去(水位含 2s 前推,所以要 Q + lead + 余量)。
	time.Sleep(opts.Quarantine + guardLeadSec*time.Second + time.Second)
	c := alloc(t, cli, kind, "host-C", opts)
	defer c.Close()
	if c.Slot != slotA {
		t.Fatalf("隔离期过后应拿到最久未用的槽 %d, got %d", slotA, c.Slot)
	}
	// 继任者的地板 = 前任水位。
	if c.GuardEpochSec == 0 {
		t.Fatal("继任者必须继承前任水位作地板")
	}
}

// 旧布局兼容(cluster 0):旧 key 的水位与活 id 同样参与隔离期 / 已占判定。
// 发布一版之后本用例随兼容读一起删。
func TestLegacyKeysStillQuarantineAndOccupy(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)
	legacy := "/legacy-" + kind
	ctx := context.Background()

	now := snowflake.NowEpochSec()
	// slot 0:旧秒级水位刚写过;slot 1:旧毫秒水位(login guard_ms)刚写过;
	// slot 2:旧版本进程仍活着(snowflake_ids/2 存在)。
	mustPut := func(k, v string) {
		if _, err := cli.Put(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	mustPut(legacy+"/snowflake_guard/0", strconv.FormatUint(now, 10))
	mustPut(legacy+"/guard_ms/1", strconv.FormatInt(time.Now().UnixMilli(), 10))
	mustPut(legacy+"/snowflake_ids/2", "old-host")

	h := alloc(t, cli, kind, "h", Options{LegacyPrefix: legacy})
	defer h.Close()
	if h.Slot != 3 {
		t.Fatalf("旧布局的水位 / 活 id 必须被算进去,期望 slot 3, got %d", h.Slot)
	}
	// 新 key 与旧 key 的地板取 max。
	mustPut(legacy+"/snowflake_guard/3", strconv.FormatUint(now+100, 10))
	if floor, _, err := readWatermarkFloor(ctx, cli, kind, h.r, 3); err != nil || floor < now+100 {
		t.Fatalf("floor=%d err=%v, 期望 ≥ %d", floor, err, now+100)
	}
	// login 的毫秒地板同样读旧 key 取 max。
	mustPut(legacy+"/guard_ms/3", "999999999999999")
	if ms, err := h.ReadMsWatermark(ctx); err != nil || ms != 999999999999999 {
		t.Fatalf("ms=%d err=%v", ms, err)
	}
}

// Close 多次调用安全。
func TestHandleCloseIsIdempotent(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	hd := alloc(t, cli, kind, "host-X", Options{})
	hd.Close()
	hd.Close()
	hd.Close()
}

// MaxSlot 设很小,池耗尽时返回 ErrNoSlotAvailable,不 panic。
func TestAllocateExhaustion(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	opts := Options{MaxSlot: 1}
	hd1 := alloc(t, cli, kind, "h1", opts)
	defer hd1.Close()
	hd2 := alloc(t, cli, kind, "h2", opts)
	defer hd2.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AllocateWithKeepAlive(ctx, cli, kind, "h3", opts); !errors.Is(err, ErrNoSlotAvailable) {
		t.Fatalf("expected ErrNoSlotAvailable, got %v", err)
	}
}

// 所有权由 **slots key 的 value** 表达,KeepAlive 只观测 lease。外部行为者(运维误操作 /
// 旧版本二进制)把 slots key 改挂到别的 uuid 上:受害者 lease 依旧存活、KeepAlive 依旧
// 成功 —— 只监 lease 是盲区。必须靠 watch 察觉并 Lost()。
func TestOwnershipLostWhenAnotherHolderTakesOver(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	victim := alloc(t, cli, kind, "host-shared", Options{})
	defer victim.Close()
	sf := victim.NewNode()
	if isClosed(victim.Lost()) {
		t.Fatal("刚分配就报失去所有权")
	}

	intruderLease, err := cli.Grant(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Revoke(context.Background(), intruderLease.ID)
	txn, err := cli.Txn(context.Background()).
		If(clientv3.Compare(clientv3.Value(victim.sKey()), "=", victim.UUID)).
		Then(clientv3.OpPut(victim.sKey(), "intruder-uuid", clientv3.WithLease(intruderLease.ID))).
		Commit()
	if err != nil || !txn.Succeeded {
		t.Fatalf("前提不成立:外部接管写入失败 err=%v", err)
	}

	select {
	case <-victim.Lost():
	case <-time.After(10 * time.Second):
		t.Fatal("被抢占后 Lost() 未触发:两个进程会用同一个 worker id 同时发号")
	}
	if _, err := sf.Generate(); !errors.Is(err, snowflake.ErrFenced) {
		t.Fatalf("Lost 后发号器必须已 fence, got %v", err)
	}
	if ttl, err := cli.TimeToLive(context.Background(), victim.Lease()); err == nil && ttl.TTL <= 0 {
		t.Logf("注意:受害者 lease 已过期(TTL=%d),本次未能覆盖'lease 仍活着'那一支", ttl.TTL)
	}
}

// lease 抖动(过期 / 被撤销)**不再**等于失去所有权:slots key 被删 → reclaim 重新挂上,
// 换一个 incarnation 继续发号;Lost() 不关闭。这是"etcd 是弱依赖"的核心(设计稿 §1.3)。
func TestReclaimAfterLeaseBlip(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	h := alloc(t, cli, kind, "host-blip", Options{})
	defer h.Close()
	sf := h.NewNode()
	oldLease, oldInc := h.Lease(), h.Incarnation()

	// 模拟 lease 过期:直接撤销。etcd 会删掉 slots / affinity;keepalive 流也会结束。
	if _, err := cli.Revoke(context.Background(), oldLease); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if h.Lease() != 0 && h.Lease() != oldLease && h.Registered() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if h.Lease() == 0 || h.Lease() == oldLease {
		t.Fatalf("reclaim 没有换上新 lease: lease=%x old=%x", h.Lease(), oldLease)
	}
	if h.Incarnation() <= oldInc {
		t.Fatalf("incarnation 必须前进: %d -> %d", oldInc, h.Incarnation())
	}
	if isClosed(h.Lost()) {
		t.Fatal("lease 抖动不是失去所有权,Lost() 不得关闭")
	}
	r := getKV(t, cli, h.sKey())
	if len(r.Kvs) != 1 || string(r.Kvs[0].Value) != h.UUID || clientv3.LeaseID(r.Kvs[0].Lease) != h.Lease() {
		t.Fatal("reclaim 后 slots key 必须挂在我们的 uuid / 新 lease 上")
	}
	// 下一拍水位 Txn 成功 ⇒ 闸 Ack ⇒ 继续发号。
	deadline = time.Now().Add(5 * time.Second)
	var genErr error
	for time.Now().Before(deadline) {
		if _, genErr = sf.Generate(); genErr == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if genErr != nil {
		t.Fatalf("reclaim 后必须恢复发号: %v", genErr)
	}
}

// 持久水位的契约是"**任何时刻**已持久化的水位 ≥ 本进程可能发到的最大逻辑秒"。
// 交出发号器的那一刻水位就必须已落盘;前推量必须 > 写入间隔。
func TestGuardWatermarkCoversMintingFromTheFirstID(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	hd := alloc(t, cli, kind, "host-A", Options{})
	defer hd.Close()

	node := hd.NewNode()
	persisted, err := readUint(context.Background(), cli, watermarkKey(kind, 0, hd.Slot))
	if err != nil {
		t.Fatalf("read watermark right after NewNode: %v", err)
	}
	if persisted == 0 {
		t.Fatal("NewNode 返回时水位仍为 0:启动到首个 tick 之间发出的号没有任何地板覆盖")
	}
	for i := 0; i < 200; i++ {
		id, gerr := node.Generate()
		if gerr != nil {
			t.Fatalf("generate: %v", gerr)
		}
		if sec, _, _, _ := DecodeID(id); sec > persisted {
			t.Fatalf("第 %d 个号的逻辑秒 %d 超出已持久化水位 %d:继任者的地板罩不住它", i, sec, persisted)
		}
	}
	if time.Duration(guardLeadSec)*time.Second <= guardWriteInterval {
		t.Fatalf("guardLeadSec=%ds 必须大于写入间隔 %v", guardLeadSec, guardWriteInterval)
	}
}

// 水位**只能单调前进**:并发轰 advanceGuard 并持续回读,持久值从不减小;
// 收尾时内存记录与 etcd 一致。
func TestGuardWatermarkNeverGoesBackwardUnderConcurrency(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	hd := alloc(t, cli, kind, "host-A", Options{})
	defer hd.Close()
	hd.NewNode()
	key := watermarkKey(kind, 0, hd.Slot)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				hd.advanceGuard(ctx)
			}
		}()
	}
	var readErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		var prev uint64
		for ctx.Err() == nil {
			got, rerr := readUint(context.Background(), cli, key)
			if rerr != nil {
				continue
			}
			if got < prev {
				readErr = fmt.Errorf("水位倒退:%d → %d", prev, got)
				return
			}
			prev = got
		}
	}()
	time.Sleep(2 * time.Second)
	cancel()
	wg.Wait()
	if readErr != nil {
		t.Fatal(readErr)
	}
	final, err := readUint(context.Background(), cli, key)
	if err != nil {
		t.Fatal(err)
	}
	hd.guardMu.Lock()
	written := hd.guardWritten
	hd.guardMu.Unlock()
	if final != written {
		t.Fatalf("持久值 %d 与内存记录 %d 不一致", final, written)
	}
}

// 2026-09-02 事故回归:同 hostname 的第二个进程在第一个进程仍存活时**不得**接管其槽
// (本地按 -Zone 起第二个 login 抢走 worker 0,zone1 login 检测到 ownership lost 自杀)。
// 期望:第二次分配拿到不同的槽,且第一个 Handle 的 Lost() 保持未关闭。
func TestAllocateSameHostnameDoesNotStealLiveLease(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	hd1 := alloc(t, cli, kind, "host-live", Options{})
	defer hd1.Close()
	hd2 := alloc(t, cli, kind, "host-live", Options{})
	defer hd2.Close()

	if hd2.Slot == hd1.Slot {
		t.Fatalf("second live process on the same hostname stole slot %d", hd1.Slot)
	}
	select {
	case <-hd1.Lost():
		t.Fatal("first holder lost ownership although it is alive")
	case <-time.After(1500 * time.Millisecond):
	}
	// 第一个进程之后优雅退出、再重启:亲和键已被第二个进程改挂,三元组不一致 ⇒ 不复用、
	// 也不抢第二个进程的槽。
	hd1.Close()
	hd3 := alloc(t, cli, kind, "host-live", Options{})
	defer hd3.Close()
	if hd3.Slot == hd2.Slot {
		t.Fatalf("restart of the first process stole the live second process's slot %d", hd2.Slot)
	}
	if isClosed(hd2.Lost()) {
		t.Fatal("second process lost ownership")
	}
}

// 毫秒水位(login):带归属校验,单调,Ack 闸;读回取 max。
func TestMsWatermark_RoundTripAndOwnershipCheck(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	h := alloc(t, cli, kind, "h", Options{ClusterBits: 3, SlotBits: 10})
	defer h.Close()
	ctx := context.Background()
	if ms, err := h.ReadMsWatermark(ctx); err != nil || ms != 0 {
		t.Fatalf("fresh slot ms watermark=%d err=%v", ms, err)
	}
	if err := h.PutMsWatermark(ctx, 5000); err != nil {
		t.Fatal(err)
	}
	if err := h.PutMsWatermark(ctx, 4000); err != nil { // 不回退
		t.Fatal(err)
	}
	if ms, err := h.ReadMsWatermark(ctx); err != nil || ms != 5000 {
		t.Fatalf("ms=%d err=%v, 期望 5000", ms, err)
	}
	// 槽被删(lease 过期)时写不进去,不 Ack,不 markLost。
	if _, err := cli.Delete(ctx, h.sKey()); err != nil {
		t.Fatal(err)
	}
	if err := h.PutMsWatermark(ctx, 6000); err == nil {
		t.Fatal("槽不在我们的 uuid 上时毫秒水位必须拒写")
	}
}

// 本地缓存在申领后的第一次水位写成功(身份落盘)时写出;etcd 可达时缓存只做日志关联
// (正常申领,不走缓存槽)。
func TestLocalCacheWrittenOnAck(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)
	path := filepath.Join(t.TempDir(), "c.json")

	h := alloc(t, cli, kind, "host-A", Options{CachePath: path})
	h.NewNode()
	rec, err := readCacheFile(path)
	if err != nil {
		t.Fatalf("cache must be written after the first ack: %v", err)
	}
	if rec.Kind != kind || rec.Slot != h.Slot || rec.UUID != h.UUID || rec.Incarnation != h.Incarnation() ||
		rec.LastWatermark == 0 || rec.LastAckWall == 0 {
		t.Fatalf("cache record %+v", rec)
	}
	h.Close()

	// etcd 可达:走正常申领(同主机优雅重启 ⇒ 复用同槽),uuid 是新的,不是缓存里的。
	h2 := alloc(t, cli, kind, "host-A", Options{CachePath: path})
	defer h2.Close()
	if h2.UUID == rec.UUID {
		t.Fatal("etcd 可达时不应沿用缓存的 uuid")
	}
	if !h2.Registered() {
		t.Fatal("etcd 可达时必须直接注册")
	}
}

// §7.5-5 稳态零写盘(真 etcd):申领后的首写之后,后台每秒的水位 Ack 不再重写缓存文件
// (内容与 mtime 都不变);Close() 把最后一次(只在内存里的)Ack 补写出去。
func TestLocalCacheNotRewrittenOnSteadyStateAcks(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)
	path := filepath.Join(t.TempDir(), "c.json")
	writes := countCacheWrites(t)

	h := alloc(t, cli, kind, "host-A", Options{CachePath: path})
	h.NewNode() // 同步再写一次水位:同一 incarnation ⇒ 不落盘
	first, err := readCacheFile(path)
	if err != nil {
		t.Fatalf("首写必须存在: %v", err)
	}
	firstStat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := writes.Load(); got != 1 {
		t.Fatalf("申领 + NewNode 应只落盘一次(身份): writes=%d", got)
	}

	time.Sleep(5500 * time.Millisecond) // ≥ 5 个后台 tick,每个都 Ack 成功
	if stale, age := h.FenceClock().Stale(); stale || age > 2*time.Second {
		t.Fatalf("前置条件不成立:后台循环没有在 Ack(stale=%v age=%v)", stale, age)
	}
	if !h.FenceClock().LastAckWall().After(time.UnixMilli(first.LastAckWall).Add(4 * time.Second)) {
		t.Fatal("前置条件不成立:后台循环没有连续 Ack 至少 5 拍")
	}
	if degraded(h) {
		t.Fatal("稳态不该处于故障模式")
	}
	if got := writes.Load(); got != 1 {
		t.Fatalf("稳态 Ack 不许重写缓存: writes=%d (want 1)", got)
	}
	if again, err := readCacheFile(path); err != nil || *again != *first {
		t.Fatalf("磁盘内容不该变: %+v err=%v", again, err)
	}
	if st, err := os.Stat(path); err != nil || !st.ModTime().Equal(firstStat.ModTime()) {
		t.Fatalf("磁盘 mtime 不该变: %v → %v (err=%v)", firstStat.ModTime(), st.ModTime(), err)
	}

	h.Close()
	if got := writes.Load(); got != 2 {
		t.Fatalf("Close() 必须补写最终状态: writes=%d (want 2)", got)
	}
	final, err := readCacheFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if final.LastAckWall <= first.LastAckWall || final.LastWatermark < first.LastWatermark {
		t.Fatalf("Close() 之后磁盘上必须是最后一次 Ack: first=%+v final=%+v", first, final)
	}
}

// §7.5-5 故障模式(真 etcd;把客户端切到一个没人监听的端点模拟 etcd 不通,再切回来模拟恢复):
//   - 第一次失败的那一拍立刻落盘:LocalHighWaterSec 出现,LastAckWall 是故障前最后一次 Ack;
//   - 之后每拍都写,LocalHighWaterSec 严格递增,LastAckWall / LastWatermark 不动;
//   - etcd 回来之后第一次 Ack 退出故障模式,此后直到 Close() 一次都不写;Close() 写最终值。
//
// etcd 客户端默认 WaitForReady:切到死端点后每拍的水位 Txn 都会等满 watermarkTxnTimeout;
// lease TTL 30s 远大于故障时长,keepalive 不会断、不会触发 reclaim(否则身份变化会多写一次,
// 测试会把它当成回归报出来)。
func TestLocalCache_OutageWritesEveryTickAndRecoveryStopsWriting(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)
	path := filepath.Join(t.TempDir(), "c.json")
	writes := countCacheWrites(t)

	h := alloc(t, cli, kind, "host-A", Options{CachePath: path})
	defer h.Close()
	h.NewNode()
	claimed := mustReadCache(t, path)
	if got := writes.Load(); got != 1 {
		t.Fatalf("申领 + NewNode 应只落盘一次: writes=%d", got)
	}
	inc := h.Incarnation()

	waitWrites := func(want int64, within time.Duration, what string) {
		t.Helper()
		deadline := time.Now().Add(within)
		for writes.Load() < want {
			if time.Now().After(deadline) {
				t.Fatalf("%s: writes=%d < %d after %v", what, writes.Load(), want, within)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// 故障。
	outageStart := time.Now()
	cli.SetEndpoints("127.0.0.1:1")
	t.Cleanup(func() { cli.SetEndpoints(testEtcdEndpoint) })

	waitWrites(2, 6*time.Second, "第一次失败必须立刻落盘")
	first := mustReadCache(t, path)
	if !degraded(h) {
		t.Fatal("首次失败之后必须处于故障模式")
	}
	if first.LocalHighWaterSec == 0 {
		t.Fatalf("故障首写必须带本地高水位: %+v", first)
	}
	// 故障首写的 LastAckWall 必须是故障前最后一次成功 Ack(≥ 申领时那次,且不晚于切端点
	// 之后一笔在途 Txn 还能完成的时间)。
	if first.LastAckWall < claimed.LastAckWall || first.LastAckWall > outageStart.Add(watermarkTxnTimeout).UnixMilli() {
		t.Fatalf("故障首写的 LastAckWall 必须是故障前最后一次 Ack: %d (claimed %d, outage started %d)",
			first.LastAckWall, claimed.LastAckWall, outageStart.UnixMilli())
	}

	waitWrites(4, 8*time.Second, "故障模式每拍都要写")
	later := mustReadCache(t, path)
	if later.LocalHighWaterSec <= first.LocalHighWaterSec {
		t.Fatalf("故障期本地高水位必须随拍推进: %d → %d", first.LocalHighWaterSec, later.LocalHighWaterSec)
	}
	if later.LastAckWall != first.LastAckWall || later.LastWatermark != first.LastWatermark {
		t.Fatalf("故障期 LastAckWall / LastWatermark 不许动: first=%+v later=%+v", first, later)
	}

	// 恢复。
	cli.SetEndpoints(testEtcdEndpoint)
	deadline := time.Now().Add(20 * time.Second)
	for degraded(h) {
		if time.Now().After(deadline) {
			t.Fatal("etcd 回来 20s 后仍没有一次 Ack 退出故障模式")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !h.FenceClock().LastAckWall().After(time.UnixMilli(first.LastAckWall)) {
		t.Fatal("退出故障模式必须伴随一次新的 Ack")
	}
	recovered := writes.Load()
	afterRecovery := mustReadCache(t, path)

	time.Sleep(3500 * time.Millisecond) // ≥ 3 个 Ack 成功的后台 tick
	if h.Incarnation() != inc {
		t.Fatalf("前置条件:恢复过程中不该发生 reclaim(incarnation %d → %d)", inc, h.Incarnation())
	}
	if degraded(h) {
		t.Fatal("恢复之后不该再进故障模式")
	}
	if got := writes.Load(); got != recovered {
		t.Fatalf("恢复后的稳态不许写盘: writes %d → %d", recovered, got)
	}
	if again := mustReadCache(t, path); *again != *afterRecovery {
		t.Fatalf("恢复后磁盘内容不该变: %+v → %+v", afterRecovery, again)
	}

	h.Close()
	if got := writes.Load(); got != recovered+1 {
		t.Fatalf("Close() 必须恰好写一次最终值: writes %d → %d", recovered, got)
	}
	final := mustReadCache(t, path)
	if final.LastAckWall <= first.LastAckWall || final.LastWatermark < later.LastWatermark {
		t.Fatalf("Close() 之后磁盘上必须是恢复后的最后一次 Ack: first=%+v final=%+v", first, final)
	}
}

// ---- F1:滚动升级期的**双向**互斥 --------------------------------------------------
//
// legacyClaimHEAD 是**改造前那版分配器**(git show HEAD:go/shared/snowflakealloc/allocator.go)
// 申领路径的最小复刻:只认 <prefix>/snowflake_ids/ 与 <prefix>/snowflake_nodes/,
// 选"最小空闲",双 key CAS 都要求 CreateRevision==0。
//
// 灰度期它和新二进制在同一个 etcd 上并存,而新二进制的槽写在 /snowflake/<kind>/c0/slots/ ——
// 旧代码根本看不见,于是会把同一个 worker id 再发一次。新代码这边的"避开旧布局"是单向的,
// 挡不住这个方向。所以新代码申领时必须把旧布局的 id 键一起占上(过渡期专用)。
func legacyClaimHEAD(t *testing.T, cli *clientv3.Client, prefix, host string, maxID uint64) (uint64, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	used := make(map[uint64]bool)
	idP := prefix + "/snowflake_ids/"
	idResp, err := cli.Get(ctx, idP, clientv3.WithPrefix())
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range idResp.Kvs {
		if id, perr := strconv.ParseUint(strings.TrimPrefix(string(kv.Key), idP), 10, 64); perr == nil {
			used[id] = true
		}
	}
	nodeResp, err := cli.Get(ctx, prefix+"/snowflake_nodes/", clientv3.WithPrefix())
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range nodeResp.Kvs {
		if id, perr := strconv.ParseUint(string(kv.Value), 10, 64); perr == nil {
			used[id] = true
		}
	}

	target := maxID + 1
	for i := uint64(0); i <= maxID; i++ {
		if !used[i] {
			target = i
			break
		}
	}
	if target > maxID {
		return 0, false
	}
	lease, err := cli.Grant(ctx, 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Revoke(context.Background(), lease.ID) })
	nKey := prefix + "/snowflake_nodes/" + host
	iKey := idP + strconv.FormatUint(target, 10)
	idStr := strconv.FormatUint(target, 10)
	txn, err := cli.Txn(ctx).
		If(
			clientv3.Compare(clientv3.CreateRevision(nKey), "=", 0),
			clientv3.Compare(clientv3.CreateRevision(iKey), "=", 0),
		).
		Then(
			clientv3.OpPut(nKey, idStr, clientv3.WithLease(lease.ID)),
			clientv3.OpPut(iKey, host, clientv3.WithLease(lease.ID)),
		).
		Commit()
	if err != nil {
		t.Fatal(err)
	}
	return target, txn.Succeeded
}

// F1 回归:灰度期旧二进制**不得**拿到新二进制正持有的槽,并且必须能在旧 key 上读到地板。
func TestRollingUpgrade_LegacyBinaryCannotTakeALiveSlot(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)
	legacy := "/legacy-" + kind

	h := alloc(t, cli, kind, "host-new", Options{LegacyPrefix: legacy})
	defer h.Close()
	if h.Slot != 0 {
		t.Fatalf("干净环境下新二进制应拿 slot 0, got %d", h.Slot)
	}
	h.NewNode()

	// 旧二进制此刻申领:它只会扫旧 key。修复前那里什么都没有 ⇒ 它选中 0 并 CAS 成功,
	// 两个进程用同一个 worker id 同时发号。
	id, ok := legacyClaimHEAD(t, cli, legacy, "host-old", 15)
	if !ok {
		t.Fatal("旧二进制应该还能拿到**别的**号,而不是拿不到号")
	}
	if id == h.Slot {
		t.Fatalf("旧二进制拿到了新二进制正持有的 worker id %d:过渡期占位没生效", id)
	}

	// 旧布局的占位键必须挂在新持有者的 lease 上(它一死,槽自然对旧二进制也开放)。
	occ := getKV(t, cli, legacy+"/snowflake_ids/"+strconv.FormatUint(h.Slot, 10))
	if len(occ.Kvs) != 1 || clientv3.LeaseID(occ.Kvs[0].Lease) != h.Lease() {
		t.Fatalf("旧布局占位键必须存在且挂在我们的 lease 上: %+v", occ.Kvs)
	}

	// 水位必须镜像到旧 key:旧二进制接手这个槽时只会读 <prefix>/snowflake_guard/<id>,
	// 读不到地板就会从自己的时钟重新开始发号。
	newWm, err := readUint(context.Background(), cli, watermarkKey(kind, 0, h.Slot))
	if err != nil || newWm == 0 {
		t.Fatalf("new watermark=%d err=%v", newWm, err)
	}
	legacyWm, err := readUint(context.Background(), cli, legacy+"/snowflake_guard/"+strconv.FormatUint(h.Slot, 10))
	if err != nil {
		t.Fatal(err)
	}
	if legacyWm < newWm {
		t.Fatalf("旧布局水位 %d 落后于新水位 %d:灰度期的旧二进制拿不到地板", legacyWm, newWm)
	}
}

// F1 的毫秒面(login):PutMsWatermark 同样要镜像到旧 <prefix>/guard_ms/<slot>。
func TestRollingUpgrade_MsWatermarkMirroredToLegacyKey(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)
	legacy := "/legacy-" + kind

	h := alloc(t, cli, kind, "host-new", Options{ClusterBits: 3, SlotBits: 10, LegacyPrefix: legacy})
	defer h.Close()
	const target = 1893456000000
	if err := h.PutMsWatermark(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	got, err := readUint(context.Background(), cli, legacy+"/guard_ms/"+strconv.FormatUint(h.Slot, 10))
	if err != nil || got != target {
		t.Fatalf("旧布局毫秒水位=%d err=%v, 期望 %d", got, err, target)
	}
}

// ---- F2:reclaim 的两次比较必须是一笔 Txn ------------------------------------------
//
// 槽 key 在两次比较之间消失(旧 lease 的 revoke 落在中间)不是"被别人抢走" —— 那正是
// reclaim 存在的理由。修复前 reclaim 拆成两笔 Txn:第一笔 If CreateRev==0 因为 key 还在
// 而失败,第二笔 If Value==uuid 在一把**已经没了**的 key 上失败,随后无条件 markLost,
// 消费方 Fence + os.Exit(1) —— 而根本没人拿走这个槽。
//
// 这里用一个"反复删掉 / 用同一个 uuid 重挂"的搅动协程把那段时序放大:每一次搅动产生的
// 状态要么是"不存在"(CreateRev==0 成立),要么是"存在且 value 是我们"(Value==uuid 成立),
// 一笔嵌套 Txn 必然命中其一;拆成两笔就会在窗口里两头落空。
func TestReclaimDoesNotDeclareLostWhenTheSlotKeyMerelyVanishes(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	r, err := Options{LeaseTTL: 5}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	h := newHandle(cli, kind, "host-race", r, 0, 0, "uuid-race")
	kaCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() {
		if l := h.Lease(); l != 0 {
			cli.Revoke(context.Background(), l)
		}
	}()

	churnLease, err := cli.Grant(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Revoke(context.Background(), churnLease.ID)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			ctx, c := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = cli.Delete(ctx, h.sKey())
			_, _ = cli.Put(ctx, h.sKey(), h.UUID, clientv3.WithLease(churnLease.ID))
			c()
		}
	}()

	st := &loopState{}
	for i := 0; i < 40 && !isClosed(h.Lost()); i++ {
		h.reclaim(kaCtx, st, "churn")
	}
	close(stop)
	<-done

	if isClosed(h.Lost()) {
		t.Fatal("槽 key 只是在两次比较之间消失了一下,没有任何人接管:不得判成失去所有权")
	}
}

// ---- F4:复用路径读不到水位地板必须 fail-closed --------------------------------------
//
// 复用是**刻意跳过隔离期**的(前任已 fence 并优雅退出),于是地板是这条路径上唯一的
// 跨重启防线。修复前读失败只打一条日志,然后带着 floor=0 继续跑。
func TestReuse_FailsClosedWhenTheWatermarkFloorIsUnreadable(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	a := alloc(t, cli, kind, "host-A", Options{})
	slotA := a.Slot
	a.Close() // 写 released ⇒ 下一次同主机启动走复用路径

	orig := readWatermarkFloorFn
	readWatermarkFloorFn = func(context.Context, *clientv3.Client, string, resolvedOptions, uint64) (uint64, int64, error) {
		return 0, 0, errors.New("injected watermark read failure")
	}
	t.Cleanup(func() { readWatermarkFloorFn = orig })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if h, err := AllocateWithKeepAlive(ctx, cli, kind, "host-A", Options{}); err == nil {
		h.Close()
		t.Fatal("复用成功但地板读不到时必须整体失败(fail-closed)")
	}
	// 失败要清干净:lease 被撤销 ⇒ 槽 key 不留在 etcd 里。
	if r := getKV(t, cli, slotKey(kind, 0, slotA)); len(r.Kvs) != 0 {
		t.Fatalf("fail-closed 之后槽 key 必须随 lease 一起撤销: %+v", r.Kvs)
	}
	// 全新申领(受隔离期保护)则只退化成启动 guard,不阻断启动。
	h, err := AllocateWithKeepAlive(ctx, cli, kind, "host-B", Options{})
	if err != nil {
		t.Fatalf("全新申领不该被地板读失败阻断: %v", err)
	}
	defer h.Close()
	if h.Slot == slotA {
		t.Fatalf("host-B 拿到了隔离期内的槽 %d", slotA)
	}
}

// ---- F5:重新申领要重读地板;水位对 etcd 单调 -----------------------------------------
//
// A 失联 > Q 期间 B 合法拿走这个槽、发了一批号、又释放了;A 回来时槽 key 已经不存在,
// reclaim 的 CreateRev==0 分支把它重新 Create 出来 —— 修复前 A 就这样带着**自己那口
// 时钟**继续发号,B 写下的水位没有任何人去读。
func TestReclaim_ReAppliesTheWatermarkFloorAfterTheKeyDisappeared(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	r, err := Options{LeaseTTL: 10}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	h := newHandle(cli, kind, "host-refloor", r, 0, 0, "uuid-refloor")
	node := snowflake.NewNode(0)
	h.node.Store(node)
	defer func() {
		if l := h.Lease(); l != 0 {
			cli.Revoke(context.Background(), l)
		}
	}()

	// 继任者 B 留下的水位;A 的槽 key 早已随 lease 消失。
	successorWm := snowflake.NowEpochSec() + 600
	if _, err := cli.Put(context.Background(), watermarkKey(kind, 0, 0),
		strconv.FormatUint(successorWm, 10)); err != nil {
		t.Fatal(err)
	}

	kaCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.reclaim(kaCtx, &loopState{}, "test")

	if !h.Registered() {
		t.Fatal("reclaim 应把槽 key 重新挂上")
	}
	h.guardMu.Lock()
	written := h.guardWritten
	h.guardMu.Unlock()
	if written < successorWm {
		t.Fatalf("重新申领后必须把 etcd 里的水位吃成地板: guardWritten=%d < %d", written, successorWm)
	}
	if hw := node.HighWaterEpochSec(); hw < successorWm {
		t.Fatalf("发号器地板没被抬到继任者水位之上: high_water=%d < %d", hw, successorWm)
	}
}

// F5(b):水位写必须相对 **etcd 的存值**单调,不能只相对本进程记的 guardWritten ——
// 后者挡不住"申领时地板没读到 / 失联期间别人写过更高的值"。
func TestAdvanceGuard_NeverLowersAHigherStoredWatermark(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	h := alloc(t, cli, kind, "host-mono", Options{})
	defer h.Close()
	node := h.NewNode()

	key := watermarkKey(kind, 0, h.Slot)
	stored := snowflake.NowEpochSec() + 3600
	if _, err := cli.Put(context.Background(), key, strconv.FormatUint(stored, 10)); err != nil {
		t.Fatal(err)
	}

	h.advanceGuard(context.Background())
	got, err := readUint(context.Background(), cli, key)
	if err != nil {
		t.Fatal(err)
	}
	if got < stored {
		t.Fatalf("水位被写回退了: %d → %d(墓碑一旦变矮,后来的申领者就会用一个罩不住的地板)", stored, got)
	}
	// 更高的存值同时要变成本进程发号器的地板 —— 存值比墙钟超前 1h,借位预算(10s)
	// 兜不住 ⇒ fail-closed,而不是发出落在存值之下的号。
	if _, gerr := node.Generate(); !errors.Is(gerr, snowflake.ErrBorrowLimitExceeded) {
		t.Fatalf("发号器没有吃下更高的存值当地板: %v", gerr)
	}
}

// ---- F6(a):最终水位没写成功就不许写 released ------------------------------------------
//
// released 是"后继进程可以跳过隔离期复用这个槽"的通行证,而复用路径唯一的防线就是地板。
// 修复前 Close() 无条件写它 —— 最终水位写失败(这里用一个坏掉的水位值触发)时,
// 后继进程会带着一个陈旧地板复用同一个槽。
func TestClose_DoesNotWriteReleasedWhenTheFinalWatermarkFails(t *testing.T) {
	cli := newTestClient(t)
	kind := uniqueKind(t)
	cleanupKind(t, cli, kind)

	h := alloc(t, cli, kind, "host-close", Options{})
	h.NewNode()
	// 把水位值弄坏:CAS 落空后读回来解析失败 ⇒ advanceGuard 报错(等价于 etcd 写不进去),
	// 但槽 key 本身完好,所以 released 的归属校验**是能通过的** —— 正是要挡住的那一支。
	if _, err := cli.Put(context.Background(), watermarkKey(kind, 0, h.Slot), "not-a-number"); err != nil {
		t.Fatal(err)
	}
	h.Close()

	if r := getKV(t, cli, releasedKey(kind, 0, "host-close")); len(r.Kvs) != 0 {
		t.Fatal("最终水位没落定时不得写 released:后继进程会带着陈旧地板复用同一个槽")
	}
}

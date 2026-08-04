//go:build integration

package snowflakealloc

// 集成测试:需要本地 etcd (127.0.0.1:2379) 在跑。
// 启动 etcd: pwsh tools/scripts/dev_tools.ps1 -Command etcd-up
// 跑测试:    go test -tags=integration ./snowflakealloc/...
//
// 这些用例验证 Snowflake worker id 分配的核心不变量:
//   1. 从 0 开始分配(Snowflake worker_id=0 是合法的,这与 NodeInfo.NodeId 不同)
//   2. 同 hostname 复用同 worker id (进程重启场景)
//   3. 不同 hostname 拿不同 worker id
//   4. 不同 prefix 互相隔离(guild 和 scene_manager 不会撞)
//   5. lease revoke 后 worker id 立即可被其他 hostname 复用
//   6. Handle.Close 正确释放
//
// 用例之间通过独立的 prefix 隔离 etcd 状态。

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

const testEtcdEndpoint = "127.0.0.1:2379"

// uniquePrefix 给每个 test case 一个独占的 prefix,避免相互污染。
var (
	prefixCounter   int
	prefixCounterMu sync.Mutex
)

func uniquePrefix(t *testing.T) string {
	prefixCounterMu.Lock()
	defer prefixCounterMu.Unlock()
	prefixCounter++
	return fmt.Sprintf("/_snowflakealloc_test/%s_%d_%d", t.Name(), time.Now().UnixNano(), prefixCounter)
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
	return cli
}

func cleanupPrefix(t *testing.T, cli *clientv3.Client, prefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = cli.Delete(ctx, prefix+"/", clientv3.WithPrefix())
}

// TestAllocateStartsFromZero: 干净环境下第一次分配返回 0。
// Snowflake worker_id=0 是合法的(不像 NodeInfo.NodeId 需要从 1 开始)。
func TestAllocateStartsFromZero(t *testing.T) {
	cli := newTestClient(t)
	defer cli.Close()
	prefix := uniquePrefix(t)
	defer cleanupPrefix(t, cli, prefix)

	hd, err := AllocateWithKeepAlive(context.Background(), cli, prefix, "host-A", Options{LeaseTTL: 30})
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	defer hd.Close()

	if hd.WorkerID != 0 {
		t.Fatalf("expected first worker_id=0, got %d", hd.WorkerID)
	}
}

// TestAllocateSameHostnameReuses: 同 hostname 第二次 Allocate 应复用同 worker id。
// 模拟进程重启:第一次 handle 关掉(老 lease revoke),第二次 Allocate 应当看到
// 老 nodeKey 仍存在(取决于 etcd 清理时机) → 走 reclaim 分支,或老 key 已经清 → 走分配新 id 分支
// 但因为 hostname 一致,新分配出来的 id 应当还是 0(最小空闲)。
func TestAllocateSameHostnameReuses(t *testing.T) {
	cli := newTestClient(t)
	defer cli.Close()
	prefix := uniquePrefix(t)
	defer cleanupPrefix(t, cli, prefix)

	hd1, err := AllocateWithKeepAlive(context.Background(), cli, prefix, "host-A", Options{LeaseTTL: 30})
	if err != nil {
		t.Fatalf("allocate 1: %v", err)
	}
	id1 := hd1.WorkerID
	hd1.Close() // revoke lease

	// 等 etcd 清掉老 key
	time.Sleep(100 * time.Millisecond)

	hd2, err := AllocateWithKeepAlive(context.Background(), cli, prefix, "host-A", Options{LeaseTTL: 30})
	if err != nil {
		t.Fatalf("allocate 2: %v", err)
	}
	defer hd2.Close()

	if hd2.WorkerID != id1 {
		t.Fatalf("expected same worker_id=%d for same hostname, got %d", id1, hd2.WorkerID)
	}
}

// TestAllocateDifferentHostnamesUnique: 不同 hostname 同 prefix 必须拿到不同 worker id。
func TestAllocateDifferentHostnamesUnique(t *testing.T) {
	cli := newTestClient(t)
	defer cli.Close()
	prefix := uniquePrefix(t)
	defer cleanupPrefix(t, cli, prefix)

	hdA, err := AllocateWithKeepAlive(context.Background(), cli, prefix, "host-A", Options{LeaseTTL: 30})
	if err != nil {
		t.Fatalf("allocate A: %v", err)
	}
	defer hdA.Close()

	hdB, err := AllocateWithKeepAlive(context.Background(), cli, prefix, "host-B", Options{LeaseTTL: 30})
	if err != nil {
		t.Fatalf("allocate B: %v", err)
	}
	defer hdB.Close()

	if hdA.WorkerID == hdB.WorkerID {
		t.Fatalf("different hostnames should get different worker_id, both got %d", hdA.WorkerID)
	}
}

// TestAllocateConcurrentUnique: 并发申请同 prefix 下 N 个不同 hostname,所有 worker id 必须互不相同。
func TestAllocateConcurrentUnique(t *testing.T) {
	cli := newTestClient(t)
	defer cli.Close()
	prefix := uniquePrefix(t)
	defer cleanupPrefix(t, cli, prefix)

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
			hd, err := AllocateWithKeepAlive(context.Background(), cli, prefix, host, Options{LeaseTTL: 30})
			if err != nil {
				errCh <- fmt.Errorf("alloc %s: %v", host, err)
				return
			}
			mu.Lock()
			handles = append(handles, hd)
			if all[hd.WorkerID] {
				errCh <- fmt.Errorf("duplicate worker_id=%d (host=%s)", hd.WorkerID, host)
			}
			all[hd.WorkerID] = true
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent uniqueness violated: %v", err)
	}
	if len(all) != N {
		t.Fatalf("expected %d unique worker_ids, got %d", N, len(all))
	}
}

// TestAllocateDifferentPrefixIsolated: 两个不同 prefix(模拟 guild / scene_manager)
// 必须互不干扰,各自从 0 开始分配。
func TestAllocateDifferentPrefixIsolated(t *testing.T) {
	cli := newTestClient(t)
	defer cli.Close()
	prefixA := uniquePrefix(t)
	prefixB := uniquePrefix(t)
	defer cleanupPrefix(t, cli, prefixA)
	defer cleanupPrefix(t, cli, prefixB)

	hdA, err := AllocateWithKeepAlive(context.Background(), cli, prefixA, "shared-host", Options{LeaseTTL: 30})
	if err != nil {
		t.Fatalf("alloc A: %v", err)
	}
	defer hdA.Close()

	hdB, err := AllocateWithKeepAlive(context.Background(), cli, prefixB, "shared-host", Options{LeaseTTL: 30})
	if err != nil {
		t.Fatalf("alloc B: %v", err)
	}
	defer hdB.Close()

	// 两个 prefix 互相隔离,即使 hostname 相同也各拿 0
	if hdA.WorkerID != 0 || hdB.WorkerID != 0 {
		t.Fatalf("expected both prefixes to start from 0, got A=%d B=%d", hdA.WorkerID, hdB.WorkerID)
	}
}

// TestAllocateNoImmediateReclaimAfterClose: host-A 优雅退出后,host-B **不得**在 TTL 内
// 拿到同一个 worker id。
//
// ⚠️ 本用例此前断言的是**相反**的行为("Close 后 host-B 可以立即抢到原 worker id"),
// 并把它当作卖点。那正是一个真实缺陷:分配器取最小空闲 id ⇒ 刚释放的 id 就是下一个
// 启动者优先拿到的;而 snowflake.NewNode 的启动 guard 只是"绝不在构造那一秒发号"的
// **点排除**,不是"以前任高水位为地板"。新持有者若时钟落后于前任(NTP 漂移 / 快照恢复),
// 它的 now+1 仍可能 ≤ 前任最后发号的那一秒,于是静默重发前任已发出的号。
// lease TTL 是唯一能吸收这种跨机时钟偏斜的缓冲,Close() 因此刻意不再 Revoke。
func TestAllocateNoImmediateReclaimAfterClose(t *testing.T) {
	cli := newTestClient(t)
	defer cli.Close()
	prefix := uniquePrefix(t)
	defer cleanupPrefix(t, cli, prefix)

	hdA, err := AllocateWithKeepAlive(context.Background(), cli, prefix, "host-A", Options{LeaseTTL: 30})
	if err != nil {
		t.Fatalf("alloc A: %v", err)
	}
	idA := hdA.WorkerID
	hdA.Close()
	time.Sleep(100 * time.Millisecond)

	hdB, err := AllocateWithKeepAlive(context.Background(), cli, prefix, "host-B", Options{LeaseTTL: 30})
	if err != nil {
		t.Fatalf("alloc B: %v", err)
	}
	defer hdB.Close()

	if hdB.WorkerID == idA {
		t.Fatalf("host-B 在 TTL 内拿到了 host-A 刚释放的 worker_id=%d:"+
			"隔离期没生效(Close 是不是又 Revoke 了?),跨机时钟偏斜下会静默重号", idA)
	}
}

// TestAllocateSameHostnameStillReusesAfterClose: 去掉 Revoke 不能破坏 hostname 亲和。
// nodeKey 仍挂在旧 lease 上,复用分支用 Value CAS 把它改挂到新 lease,
// 因此同 hostname 重启必须仍然拿回同一个 worker id(不消耗新槽位)。
func TestAllocateSameHostnameStillReusesAfterClose(t *testing.T) {
	cli := newTestClient(t)
	defer cli.Close()
	prefix := uniquePrefix(t)
	defer cleanupPrefix(t, cli, prefix)

	hdA, err := AllocateWithKeepAlive(context.Background(), cli, prefix, "host-same", Options{LeaseTTL: 30})
	if err != nil {
		t.Fatalf("alloc first: %v", err)
	}
	idFirst := hdA.WorkerID
	hdA.Close()
	time.Sleep(100 * time.Millisecond)

	hdB, err := AllocateWithKeepAlive(context.Background(), cli, prefix, "host-same", Options{LeaseTTL: 30})
	if err != nil {
		t.Fatalf("alloc second: %v", err)
	}
	defer hdB.Close()

	if hdB.WorkerID != idFirst {
		t.Fatalf("同 hostname 重启应复用 worker_id=%d, got %d(hostname 亲和被破坏)",
			idFirst, hdB.WorkerID)
	}
}

// TestHandleCloseIsIdempotent: Close 多次调用安全。
func TestHandleCloseIsIdempotent(t *testing.T) {
	cli := newTestClient(t)
	defer cli.Close()
	prefix := uniquePrefix(t)
	defer cleanupPrefix(t, cli, prefix)

	hd, err := AllocateWithKeepAlive(context.Background(), cli, prefix, "host-X", Options{LeaseTTL: 30})
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}

	hd.Close()
	hd.Close() // should not panic
	hd.Close()
}

// TestAllocateExhaustion: MaxWorkerID 设很小,池耗尽时返回错误,不 panic。
func TestAllocateExhaustion(t *testing.T) {
	cli := newTestClient(t)
	defer cli.Close()
	prefix := uniquePrefix(t)
	defer cleanupPrefix(t, cli, prefix)

	// 池大小 = 2 (worker id 0, 1)
	opts := Options{LeaseTTL: 30, MaxWorkerID: 1}

	hd1, err := AllocateWithKeepAlive(context.Background(), cli, prefix, "h1", opts)
	if err != nil {
		t.Fatalf("alloc 1: %v", err)
	}
	defer hd1.Close()

	hd2, err := AllocateWithKeepAlive(context.Background(), cli, prefix, "h2", opts)
	if err != nil {
		t.Fatalf("alloc 2: %v", err)
	}
	defer hd2.Close()

	// 第三个 hostname 应当失败(池已耗尽)
	_, err = AllocateWithKeepAlive(context.Background(), cli, prefix, "h3", opts)
	if err == nil {
		t.Fatalf("expected exhaustion error, got nil")
	}
}

// TestOwnershipLostWhenAnotherHostTakesOver: 所有权由 **key** 表达,而 KeepAlive 只观测 **lease**。
//
// hostname 复用分支是无条件抢占(只 CAS nodeKey 的 value),别人接管时把 nodeKey/idKey 直接
// Put 到自己的 lease 上。etcd 的 Put 只把 key 从旧 lease 摘下改挂新 lease,**不撤销也不通知旧 lease**
// —— 被抢的一方 KeepAlive 仍然成功、channel 不关、自 fencing 也不触发(它续租得好好的),
// 却已经不再拥有这个 worker id。两个进程于是用同一个 worker id 同时发号,全程零告警。
//
// 修复:额外 watch nodeKey,发现它改挂到别的 lease 就 markLost。本用例钉住这条通道。
func TestOwnershipLostWhenAnotherHostTakesOver(t *testing.T) {
	cli := newTestClient(t)
	defer cli.Close()
	prefix := uniquePrefix(t)
	defer cleanupPrefix(t, cli, prefix)

	const host = "host-shared"
	victim, err := AllocateWithKeepAlive(context.Background(), cli, prefix, host, Options{LeaseTTL: 30})
	if err != nil {
		t.Fatalf("alloc victim: %v", err)
	}
	defer victim.Close()

	select {
	case <-victim.Lost():
		t.Fatal("刚分配就报失去所有权")
	default:
	}

	// 同 hostname 的第二个进程走复用分支,把两个 key 抢到自己的 lease 上。
	// 受害者的 lease 依旧存活、KeepAlive 依旧成功。
	taker, err := AllocateWithKeepAlive(context.Background(), cli, prefix, host, Options{LeaseTTL: 30})
	if err != nil {
		t.Fatalf("alloc taker: %v", err)
	}
	defer taker.Close()
	if taker.WorkerID != victim.WorkerID {
		t.Fatalf("前提不成立:接管者应复用同一个 worker id, victim=%d taker=%d",
			victim.WorkerID, taker.WorkerID)
	}

	// 受害者必须察觉。修复前这里会超时 —— lease 还活着,没有任何通道会通知它。
	select {
	case <-victim.Lost():
	case <-time.After(10 * time.Second):
		t.Fatal("被抢占后 Lost() 未触发:两个进程会用同一个 worker id 同时发号")
	}

	// 受害者的 lease 确实还活着,证明"只监 lease 就是盲区"这一点。
	ttlResp, err := cli.TimeToLive(context.Background(), victim.LeaseID)
	if err != nil {
		t.Fatalf("time to live: %v", err)
	}
	if ttlResp.TTL <= 0 {
		t.Logf("注意:受害者 lease 已过期(TTL=%d),本次未能覆盖'lease 仍活着'那一支", ttlResp.TTL)
	}
}

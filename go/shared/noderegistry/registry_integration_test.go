//go:build integration

package noderegistry

// 集成测试:需要本地 etcd (127.0.0.1:2379) 在跑。
// 启动 etcd: pwsh tools/scripts/dev_tools.ps1 -Command etcd-up
// 跑测试:    go test -tags=integration ./noderegistry/...
//
// 覆盖契约 zone_contract_v1 §2 的 etcd 侧不变量:
//   1. 同一租约、同一 Txn 写双 key;allocKey 值 = uuid,rpcPath 值 = 回调产物
//   2. 已占 node_id 从 key 路径解析(值不可解析也算占用),node_id=0 忽略;并发分配互不相同
//   3. 回调产物不合格 → 拒注册且 etcd 里不留任何 key
//   4. Close 删双 key + Revoke、幂等;id 已被别人接管时不删别人的 key
//   5. 失租 → CAS 重夺原 id;原 id 被占 → ReallocateNewID 换号并回调 / ExitProcess 退出
//   6. 客户端先判失租、服务端旧 key 仍在时重夺成功(不误判成"被抢")
//   7. Close 与重注册交错时不留孤儿 key
//   8. RegisterAfterListening 等端口可连才注册
//
// 用例之间用独立 Prefix 隔离,结束时删掉整个前缀。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

const testEtcdEndpoint = "127.0.0.1:2379"

const (
	itNodeType uint32 = 9
	itZone     uint32 = 2
	itLeaseTTL int64  = 5
)

var prefixCounter atomic.Int64

// uniquePrefix 给每个用例一个独占前缀(不含 "/",满足 Spec.validate)。
func uniquePrefix() string {
	return fmt.Sprintf("NoderegistryTest%d_%d.rpc", time.Now().UnixNano(), prefixCounter.Add(1))
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

func cleanupPrefix(t *testing.T, cli *clientv3.Client, prefix string) {
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = cli.Delete(ctx, prefix+"/", clientv3.WithPrefix())
	})
}

func itSpec(prefix string) Spec {
	return Spec{
		Prefix:   prefix,
		NodeType: itNodeType,
		ZoneId:   itZone,
		LeaseTTL: itLeaseTTL,
		BuildValue: func(nodeID uint32, nodeUUID string) ([]byte, error) {
			return json.Marshal(map[string]any{
				"nodeId":       nodeID,
				"nodeType":     itNodeType,
				"zoneId":       itZone,
				"nodeUuid":     nodeUUID,
				"launchTime":   strconv.FormatInt(time.Now().Unix(), 10),
				"endpoint":     map[string]any{"ip": "127.0.0.1", "port": 50700},
				"grpcEndpoint": map[string]any{"ip": "127.0.0.1", "port": 50700},
				"protocolType": 1,
			})
		},
	}
}

// register 注册并登记 Close 清理(Close 幂等,用例里提前 Close 也没关系)。
func register(t *testing.T, cli *clientv3.Client, spec Spec) *Registration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := Register(ctx, cli, spec)
	if err != nil {
		t.Fatalf("register %s: %v", spec.Prefix, err)
	}
	t.Cleanup(r.Close)
	return r
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

func revoke(t *testing.T, cli *clientv3.Client, lease clientv3.LeaseID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cli.Revoke(ctx, lease); err != nil {
		t.Fatalf("revoke %x: %v", int64(lease), err)
	}
}

func leaseGone(t *testing.T, cli *clientv3.Client, lease clientv3.LeaseID) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ttl, err := cli.TimeToLive(ctx, lease)
	return err == nil && ttl.TTL <= 0
}

func decodeInfo(t *testing.T, raw []byte) nodeInfoMirror {
	t.Helper()
	var m nodeInfoMirror
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("rpcPath value is not JSON: %v", err)
	}
	return m
}

// assertHeldBy 断言 id 的双 key 都在、挂在 lease 上、归属 uuid。
func assertHeldBy(t *testing.T, cli *clientv3.Client, prefix string, id uint32, uuid string, lease clientv3.LeaseID) {
	t.Helper()
	a := getKV(t, cli, AllocationKey(prefix, itNodeType, id))
	if len(a.Kvs) != 1 || string(a.Kvs[0].Value) != uuid || clientv3.LeaseID(a.Kvs[0].Lease) != lease {
		t.Fatalf("allocKey node_id=%d: want uuid=%s lease=%x, got %+v", id, uuid, int64(lease), a.Kvs)
	}
	p := getKV(t, cli, RpcPath(prefix, itZone, itNodeType, id))
	if len(p.Kvs) != 1 || clientv3.LeaseID(p.Kvs[0].Lease) != lease {
		t.Fatalf("rpcPath node_id=%d: want lease=%x, got %+v", id, int64(lease), p.Kvs)
	}
	if m := decodeInfo(t, p.Kvs[0].Value); m.NodeId != id || m.NodeUuid != uuid {
		t.Fatalf("rpcPath node_id=%d value: nodeId=%d nodeUuid=%s", id, m.NodeId, m.NodeUuid)
	}
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for: %s", within, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// swapExit 把 exitProcess 换成记录器,返回"已调用"通道。
func swapExit(t *testing.T) <-chan struct{} {
	called := make(chan struct{})
	var once sync.Once
	orig := exitProcess
	exitProcess = func() { once.Do(func() { close(called) }) }
	t.Cleanup(func() { exitProcess = orig })
	return called
}

// ---- 分配 ---------------------------------------------------------------------------

func TestRegister_WritesBothKeysOnTheSameLease(t *testing.T) {
	cli := newTestClient(t)
	prefix := uniquePrefix()
	cleanupPrefix(t, cli, prefix)

	r := register(t, cli, itSpec(prefix))
	if r.NodeID() != 1 {
		t.Fatalf("fresh prefix should allocate node_id=1, got %d", r.NodeID())
	}
	if r.NodeUUID == "" {
		t.Fatal("NodeUUID must be set")
	}
	lease, id, rpcKey, allocKey, _ := r.snapshot()
	if rpcKey != RpcPath(prefix, itZone, itNodeType, id) || allocKey != AllocationKey(prefix, itNodeType, id) {
		t.Fatalf("internal keys drifted: rpc=%s alloc=%s", rpcKey, allocKey)
	}
	assertHeldBy(t, cli, prefix, id, r.NodeUUID, lease)
}

// 已占判定只看 key 路径:值不是 JSON 的 rpcPath 同样占号(既有副本会把它当空位并覆盖);
// node_id=0 的 key 不占号。
func TestRegister_SkipsIDsOccupiedInKeyPaths(t *testing.T) {
	cli := newTestClient(t)
	prefix := uniquePrefix()
	cleanupPrefix(t, cli, prefix)

	ctx := context.Background()
	for k, v := range map[string]string{
		RpcPath(prefix, 7, itNodeType, 1):       "not-json", // 另一个 zone、值坏掉
		AllocationKey(prefix, itNodeType, 2):    "other-uuid",
		RpcPath(prefix, 3, itNodeType, 0):       "zero-ignored",
		AllocationKey(prefix, itNodeType, 0):    "zero-ignored",
		prefix + "/unrelated/garbage/node_id/x": "ignored",
	} {
		if _, err := cli.Put(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}

	r := register(t, cli, itSpec(prefix))
	if r.NodeID() != 3 {
		t.Fatalf("ids 1 (rpc path) and 2 (alloc key) are taken, want 3, got %d", r.NodeID())
	}
}

func TestRegister_ConcurrentAllocationsAreUnique(t *testing.T) {
	cli := newTestClient(t)
	prefix := uniquePrefix()
	cleanupPrefix(t, cli, prefix)

	const n = 10
	var (
		mu   sync.Mutex
		regs []*Registration
		wg   sync.WaitGroup
	)
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			r, err := Register(ctx, cli, itSpec(prefix))
			if err != nil {
				errCh <- err
				return
			}
			mu.Lock()
			regs = append(regs, r)
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(errCh)
	t.Cleanup(func() {
		for _, r := range regs {
			r.Close()
		}
	})
	for err := range errCh {
		t.Fatalf("concurrent register: %v", err)
	}

	seen := make(map[uint32]string, n)
	for _, r := range regs {
		if other, dup := seen[r.NodeID()]; dup {
			t.Fatalf("node_id=%d handed to both %s and %s", r.NodeID(), other, r.NodeUUID)
		}
		seen[r.NodeID()] = r.NodeUUID
	}
	if len(seen) != n {
		t.Fatalf("want %d unique ids, got %d", n, len(seen))
	}
}

// 回调产物不合格:拒注册,etcd 里什么都不留(校验在任何 Put 之前)。
func TestRegister_InvalidValueWritesNothing(t *testing.T) {
	cli := newTestClient(t)
	prefix := uniquePrefix()
	cleanupPrefix(t, cli, prefix)

	spec := itSpec(prefix)
	good := spec.BuildValue
	spec.BuildValue = func(nodeID uint32, nodeUUID string) ([]byte, error) {
		raw, err := good(nodeID, nodeUUID)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		m["zoneId"] = itZone + 1 // zone 写错
		return json.Marshal(m)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if r, err := Register(ctx, cli, spec); !errors.Is(err, errInvalidValue) {
		if r != nil {
			r.Close()
		}
		t.Fatalf("want errInvalidValue, got %v", err)
	}
	resp, err := cli.Get(ctx, prefix+"/", clientv3.WithPrefix())
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Kvs) != 0 {
		t.Fatalf("rejected registration left keys behind: %+v", resp.Kvs)
	}
}

// ---- 注销 ---------------------------------------------------------------------------

func TestClose_DeletesBothKeysRevokesLeaseAndIsIdempotent(t *testing.T) {
	cli := newTestClient(t)
	prefix := uniquePrefix()
	cleanupPrefix(t, cli, prefix)

	r := register(t, cli, itSpec(prefix))
	lease, _, rpcKey, allocKey, _ := r.snapshot()
	r.Close()
	r.Close()
	r.Close()

	if len(getKV(t, cli, rpcKey).Kvs) != 0 || len(getKV(t, cli, allocKey).Kvs) != 0 {
		t.Fatal("Close must delete both keys")
	}
	if !leaseGone(t, cli, lease) {
		t.Fatal("Close must revoke the lease")
	}
	r.KeepAlive() // Close 之后 KeepAlive 不得复活任何东西
	time.Sleep(500 * time.Millisecond)
	if len(getKV(t, cli, allocKey).Kvs) != 0 {
		t.Fatal("KeepAlive after Close re-created keys")
	}
}

// A 失租、B 合法接管同一 id;A 之后 Close 不得删掉 B 的 key(既有副本无条件删)。
func TestClose_LeavesKeysOfTheInstanceThatTookOverTheID(t *testing.T) {
	cli := newTestClient(t)
	prefix := uniquePrefix()
	cleanupPrefix(t, cli, prefix)

	a := register(t, cli, itSpec(prefix))
	aLease, aID, _, _, _ := a.snapshot()
	revoke(t, cli, aLease) // 模拟 A 失租(没开 KeepAlive,不会重注册)

	b := register(t, cli, itSpec(prefix))
	if b.NodeID() != aID {
		t.Fatalf("precondition: B should take over freed node_id=%d, got %d", aID, b.NodeID())
	}
	bLease, _, _, _, _ := b.snapshot()

	a.Close()
	assertHeldBy(t, cli, prefix, aID, b.NodeUUID, bLease)
}

// Close 返回前必须等 keepalive goroutine 退出:否则它若正处在重注册提交点,commit() 里的 Revoke
// 会被调用方紧接着关闭的 etcd client 打断,新租约上的双 key 在 LeaseTTL 内仍可被发现。
func TestClose_WaitsForKeepAliveGoroutineToExit(t *testing.T) {
	cli := newTestClient(t)
	prefix := uniquePrefix()
	cleanupPrefix(t, cli, prefix)

	r := register(t, cli, itSpec(prefix))
	r.KeepAlive()
	r.Close()
	select {
	case <-r.loopDone:
	default:
		t.Fatal("Close returned while the keepalive goroutine was still running")
	}
}

// ---- 失租重注册 ---------------------------------------------------------------------

func TestKeepAlive_ReclaimsOriginalIDAfterLeaseRevoked(t *testing.T) {
	cli := newTestClient(t)
	prefix := uniquePrefix()
	cleanupPrefix(t, cli, prefix)

	r := register(t, cli, itSpec(prefix))
	oldLease, oldID, _, _, _ := r.snapshot()
	r.KeepAlive()
	revoke(t, cli, oldLease)

	waitFor(t, 15*time.Second, "re-registration on a new lease", func() bool {
		lease, _, _, _, _ := r.snapshot()
		return lease != oldLease
	})
	newLease, _, _, _, _ := r.snapshot()
	if r.NodeID() != oldID {
		t.Fatalf("free original id must be reclaimed: %d -> %d", oldID, r.NodeID())
	}
	assertHeldBy(t, cli, prefix, oldID, r.NodeUUID, newLease)
}

// ReallocateNewID:原 id 已被 B 占用 → 换新 id,回调 (old,new),值里的 nodeId 经 BuildValue 重建,
// B 的 key 不受影响。
func TestKeepAlive_ReallocatesNewIDWhenOriginalTaken(t *testing.T) {
	cli := newTestClient(t)
	prefix := uniquePrefix()
	cleanupPrefix(t, cli, prefix)

	changed := make(chan [2]uint32, 1)
	spec := itSpec(prefix)
	spec.OnNodeIDChanged = func(oldID, newID uint32) {
		select {
		case changed <- [2]uint32{oldID, newID}:
		default:
		}
	}
	a := register(t, cli, spec)
	aLease, aID, _, _, _ := a.snapshot()
	revoke(t, cli, aLease)

	b := register(t, cli, itSpec(prefix))
	if b.NodeID() != aID {
		t.Fatalf("precondition: B should take over node_id=%d, got %d", aID, b.NodeID())
	}
	bLease, _, _, _, _ := b.snapshot()

	a.KeepAlive()
	var ids [2]uint32
	select {
	case ids = <-changed:
	case <-time.After(15 * time.Second):
		t.Fatal("OnNodeIDChanged not called after the original id was taken")
	}
	if ids[0] != aID || ids[1] == aID || ids[1] == 0 {
		t.Fatalf("callback (old,new) = %v, original %d", ids, aID)
	}
	if a.NodeID() != ids[1] {
		t.Fatalf("NodeID() = %d, callback said %d", a.NodeID(), ids[1])
	}
	newLease, _, rpcKey, allocKey, _ := a.snapshot()
	if rpcKey != RpcPath(prefix, itZone, itNodeType, ids[1]) || allocKey != AllocationKey(prefix, itNodeType, ids[1]) {
		t.Fatalf("internal keys not switched to new id: rpc=%s alloc=%s", rpcKey, allocKey)
	}
	assertHeldBy(t, cli, prefix, ids[1], a.NodeUUID, newLease)
	assertHeldBy(t, cli, prefix, aID, b.NodeUUID, bLease)
}

// ExitProcess:原 id 被占 → 不换号、Revoke 新租约、调 exitProcess;etcd 里不留 A 的 key。
func TestKeepAlive_ExitProcessPolicyExitsWhenOriginalTaken(t *testing.T) {
	cli := newTestClient(t)
	prefix := uniquePrefix()
	cleanupPrefix(t, cli, prefix)
	exited := swapExit(t)

	spec := itSpec(prefix)
	spec.OnReclaimFailed = ExitProcess
	var changedCalls atomic.Int32
	spec.OnNodeIDChanged = func(uint32, uint32) { changedCalls.Add(1) }

	a := register(t, cli, spec)
	aLease, aID, _, _, _ := a.snapshot()
	revoke(t, cli, aLease)
	b := register(t, cli, itSpec(prefix))
	if b.NodeID() != aID {
		t.Fatalf("precondition: B should take over node_id=%d, got %d", aID, b.NodeID())
	}
	bLease, _, _, _, _ := b.snapshot()

	a.KeepAlive()
	select {
	case <-exited:
	case <-time.After(15 * time.Second):
		t.Fatal("ExitProcess policy did not exit after the original id was taken")
	}
	if a.NodeID() != aID || changedCalls.Load() != 0 {
		t.Fatalf("ExitProcess must not switch ids: node_id=%d callbacks=%d", a.NodeID(), changedCalls.Load())
	}
	if lease, _, _, _, _ := a.snapshot(); lease != aLease {
		t.Fatalf("ExitProcess must not commit a new lease, got %x", int64(lease))
	}
	assertHeldBy(t, cli, prefix, aID, b.NodeUUID, bLease)

	resp, err := cli.Get(context.Background(), prefix+"/", clientv3.WithPrefix())
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range resp.Kvs {
		if string(kv.Value) == a.NodeUUID {
			t.Fatalf("exiting instance left a key behind: %s", kv.Key)
		}
	}
}

// 客户端判定失租时服务端旧 key 可能还在(值仍是我们的 uuid)。一轮重注册必须重夺成功并改挂
// 新租约,而不是误判"被别人占了"——ExitProcess 策略下误判会直接杀掉一个没人抢的进程。
func TestReRegister_ReclaimsWhileOldKeysAreStillLive(t *testing.T) {
	cli := newTestClient(t)
	prefix := uniquePrefix()
	cleanupPrefix(t, cli, prefix)
	exited := swapExit(t)

	spec := itSpec(prefix)
	spec.OnReclaimFailed = ExitProcess
	r := register(t, cli, spec)
	oldLease, oldID, _, _, _ := r.snapshot()

	if got := r.reRegisterOnce(oldLease); got != attemptRegistered {
		t.Fatalf("reRegisterOnce = %d, want attemptRegistered", got)
	}
	select {
	case <-exited:
		t.Fatal("own live keys were misjudged as taken over; process would have exited")
	default:
	}
	newLease, _, _, _, _ := r.snapshot()
	if newLease == oldLease || r.NodeID() != oldID {
		t.Fatalf("want same id on a new lease: id %d->%d lease %x->%x", oldID, r.NodeID(), int64(oldLease), int64(newLease))
	}
	assertHeldBy(t, cli, prefix, oldID, r.NodeUUID, newLease)
	if !leaseGone(t, cli, oldLease) {
		t.Fatal("old lease should be revoked once both keys moved to the new lease")
	}
}

// Close 与重注册交错:重注册在 Close 之后才提交时,必须 Revoke 新租约(连带其上的 key),
// 不留一个没人续租也没人注销的发现键。
func TestCommitAfterCloseRevokesTheNewLease(t *testing.T) {
	cli := newTestClient(t)
	prefix := uniquePrefix()
	cleanupPrefix(t, cli, prefix)

	r := register(t, cli, itSpec(prefix))
	r.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	grant, err := cli.Grant(ctx, 30)
	if err != nil {
		t.Fatal(err)
	}
	orphan := RpcPath(prefix, itZone, itNodeType, 99)
	if _, err := cli.Put(ctx, orphan, "in-flight", clientv3.WithLease(grant.ID)); err != nil {
		t.Fatal(err)
	}

	if r.commit(grant.ID, 99, "in-flight") {
		t.Fatal("commit after Close must be rejected")
	}
	if !leaseGone(t, cli, grant.ID) || len(getKV(t, cli, orphan).Kvs) != 0 {
		t.Fatal("rejected commit must revoke the new lease and its keys")
	}
}

// ---- 等端口 -------------------------------------------------------------------------

func TestRegisterAfterListening_WaitsForThePort(t *testing.T) {
	cli := newTestClient(t)
	prefix := uniquePrefix()
	cleanupPrefix(t, cli, prefix)

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(probe.Addr().(*net.TCPAddr).Port)
	_ = probe.Close()

	const delay = 400 * time.Millisecond
	lnCh := make(chan net.Listener, 1)
	go func() {
		time.Sleep(delay)
		ln, lerr := net.Listen("tcp", "127.0.0.1:"+port)
		if lerr != nil {
			lnCh <- nil
			return
		}
		lnCh <- ln
	}()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := RegisterAfterListening(ctx, cli, "0.0.0.0:"+port, 5*time.Second, itSpec(prefix))
	elapsed := time.Since(start)
	ln := <-lnCh
	if ln == nil {
		if r != nil {
			r.Close()
		}
		t.Skipf("port %s was taken by someone else in between", port)
	}
	defer ln.Close()
	if err != nil {
		t.Fatalf("RegisterAfterListening: %v", err)
	}
	t.Cleanup(r.Close)
	if elapsed < delay-50*time.Millisecond {
		t.Fatalf("registered after %v, before the port started listening (%v)", elapsed, delay)
	}
	if r.NodeID() == 0 {
		t.Fatal("registration has no node_id")
	}
}

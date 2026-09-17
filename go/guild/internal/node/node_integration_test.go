//go:build integration

package node

// 集成测试:需要本地 etcd (127.0.0.1:2379) 在跑。
// 启动 etcd: pwsh tools/scripts/dev_tools.ps1 -Command etcd-up
// 跑测试:    go test -tags=integration ./internal/node/...
//
// 这些用例验证 node_id 分配的核心不变量:
//   1. node_id 永远从 1 开始,不分配 0
//   2. 同 node_type 跨 zone 全局唯一(修复 Snowflake 冲突)
//   3. 不同 node_type 可复用同一数字 node_id(隔离正确)
//   4. lease revoke 后 node_id 立即可被复用
//   5. CAS 失败时继续尝试下一个 ID
//
// 用例之间通过独立的 nodeType 隔离 etcd 状态,避免相互污染。
// 每个测试结束时会清掉自己用过的 prefix。

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	base "proto/common/base"
)

const testEtcdEndpoint = "127.0.0.1:2379"

// uniqueNodeType 给每个 test case 一个独占的 nodeType,
// 这样并发跑不同测试也不会污染对方的 etcd 状态。
// 使用 uint32 时间戳尾部 + 计数器,落在 ENodeType_name 之外的数字段,
// 不会与真实业务 nodeType 冲突。
var nodeTypeCounter uint32 = 1000000

func uniqueNodeType() uint32 {
	nodeTypeCounter++
	return nodeTypeCounter
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
	// ping
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := cli.Get(ctx, "_ping"); err != nil {
		cli.Close()
		t.Skipf("etcd ping failed: %v", err)
	}
	return cli
}

// cleanupNodeType 清掉某 (nodeType) 在 etcd 里所有遗留 key,确保测试可重复跑。
// 这里用一个 hard-coded prefix —— allocator 用的 rpcPrefix 走的是 ENodeType_name 表,
// uniqueNodeType 返回的数字不在表里,base.ENodeType_name[int32(unknown)] = "",
// 所以 prefix 长这样: ".rpc/zone/.../node_type/.../..."。删整段 ".rpc" 不安全,
// 改成精确按当前测试的 prefix 删。
func cleanupNodeType(t *testing.T, cli *clientv3.Client, nodeType uint32) {
	t.Helper()
	prefix := fmt.Sprintf("%s.rpc", base.ENodeType_name[int32(nodeType)]) // 通常是 ".rpc"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 清两个子前缀: allocation + rpc 路径里的 node_type 段
	_, _ = cli.Delete(ctx,
		fmt.Sprintf("%s/allocated/node_type/%d/", prefix, nodeType),
		clientv3.WithPrefix())
	// per-zone rpcPath 在 prefix/zone/*/node_type/<nt>/ 下,我们扫整段然后过滤
	resp, err := cli.Get(ctx, prefix+"/zone/", clientv3.WithPrefix(), clientv3.WithKeysOnly())
	if err == nil {
		for _, kv := range resp.Kvs {
			k := string(kv.Key)
			if containsNodeTypeSeg(k, nodeType) {
				_, _ = cli.Delete(ctx, k)
			}
		}
	}
}

func containsNodeTypeSeg(key string, nodeType uint32) bool {
	needle := fmt.Sprintf("/node_type/%d/", nodeType)
	for i := 0; i+len(needle) <= len(key); i++ {
		if key[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// makeInfo 构造一个测试用 NodeInfo。每次都给新 uuid,模拟独立进程。
func makeInfo(nodeType, zoneID uint32) *base.NodeInfo {
	return &base.NodeInfo{
		NodeType:     nodeType,
		ZoneId:       zoneID,
		NodeUuid:     uuid.New().String(),
		LaunchTime:   uint64(time.Now().Unix()),
		ProtocolType: uint32(base.ENodeProtocolType_PROTOCOL_GRPC),
		Endpoint:     &base.EndpointComp{Ip: "127.0.0.1", Port: 9000},
	}
}

func grantLease(t *testing.T, cli *clientv3.Client, ttl int64) clientv3.LeaseID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := cli.Grant(ctx, ttl)
	if err != nil {
		t.Fatalf("grant lease: %v", err)
	}
	return resp.ID
}

// TestAllocStartsFromOne: 干净环境下第一次分配必须返回 1,绝不返回 0。
func TestAllocStartsFromOne(t *testing.T) {
	cli := newTestClient(t)
	defer cli.Close()
	nt := uniqueNodeType()
	defer cleanupNodeType(t, cli, nt)

	info := makeInfo(nt, 1)
	lease := grantLease(t, cli, 30)
	defer cli.Revoke(context.Background(), lease)

	prefix := rpcPrefix(nt)
	id, err := allocateNodeID(context.Background(), cli, prefix, info, lease)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if id != 1 {
		t.Fatalf("expected first allocation=1, got %d", id)
	}
}

// TestAllocSkipsZero: 即便 used 集合是 {1,2,3},也不会回退分配 0。
// 通过观察:第二次分配的 id 应当是 4,不是 0。
func TestAllocSkipsZero(t *testing.T) {
	cli := newTestClient(t)
	defer cli.Close()
	nt := uniqueNodeType()
	defer cleanupNodeType(t, cli, nt)

	prefix := rpcPrefix(nt)
	leases := make([]clientv3.LeaseID, 0, 4)
	defer func() {
		for _, l := range leases {
			cli.Revoke(context.Background(), l)
		}
	}()

	for i := 0; i < 3; i++ {
		l := grantLease(t, cli, 30)
		leases = append(leases, l)
		id, err := allocateNodeID(context.Background(), cli, prefix, makeInfo(nt, 1), l)
		if err != nil {
			t.Fatalf("alloc #%d: %v", i, err)
		}
		if id == 0 {
			t.Fatalf("allocator returned 0")
		}
	}

	// 第 4 次分配应当跳过 0,直接拿 4
	l := grantLease(t, cli, 30)
	leases = append(leases, l)
	id, err := allocateNodeID(context.Background(), cli, prefix, makeInfo(nt, 1), l)
	if err != nil {
		t.Fatalf("alloc 4th: %v", err)
	}
	if id == 0 {
		t.Fatalf("allocator returned 0 on 4th call")
	}
	if id != 4 {
		t.Fatalf("expected id=4, got %d", id)
	}
}

// TestAllocCrossZoneUnique: 同 node_type、不同 zone 并发申请,得到的 node_id 集合必须无交集。
// 这是修复 Snowflake worker id 跨 zone 冲突的核心断言。
func TestAllocCrossZoneUnique(t *testing.T) {
	cli := newTestClient(t)
	defer cli.Close()
	nt := uniqueNodeType()
	defer cleanupNodeType(t, cli, nt)

	prefix := rpcPrefix(nt)
	const perZone = 20
	zones := []uint32{1, 2, 3}

	var mu sync.Mutex
	allIDs := make(map[uint32]bool)
	leases := make([]clientv3.LeaseID, 0, len(zones)*perZone)

	defer func() {
		for _, l := range leases {
			cli.Revoke(context.Background(), l)
		}
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, len(zones)*perZone)

	for _, z := range zones {
		for i := 0; i < perZone; i++ {
			wg.Add(1)
			go func(zone uint32) {
				defer wg.Done()
				l := grantLease(t, cli, 30)
				mu.Lock()
				leases = append(leases, l)
				mu.Unlock()

				id, err := allocateNodeID(context.Background(), cli, prefix, makeInfo(nt, zone), l)
				if err != nil {
					errCh <- err
					return
				}
				mu.Lock()
				if allIDs[id] {
					errCh <- fmt.Errorf("duplicate node_id=%d (zone=%d)", id, zone)
				}
				allIDs[id] = true
				mu.Unlock()
			}(z)
		}
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("cross-zone uniqueness violated: %v", err)
	}

	if len(allIDs) != len(zones)*perZone {
		t.Fatalf("expected %d unique IDs, got %d", len(zones)*perZone, len(allIDs))
	}
	if allIDs[0] {
		t.Fatalf("0 was allocated")
	}
}

// TestAllocDifferentTypeReuse: 不同 node_type 之间互相隔离,可以拿到相同数字 node_id。
func TestAllocDifferentTypeReuse(t *testing.T) {
	cli := newTestClient(t)
	defer cli.Close()
	ntA := uniqueNodeType()
	ntB := uniqueNodeType()
	// 合法节点类型有各自的服务前缀；测试用的未定义枚举不能都落入空名前缀 .rpc。
	// 用独立命名空间模拟真实前缀隔离，清理也只覆盖本测试发布的 key。
	namespace := uuid.New().String()
	prefixA := fmt.Sprintf("guild-node-test-%s-type-%d.rpc", namespace, ntA)
	prefixB := fmt.Sprintf("guild-node-test-%s-type-%d.rpc", namespace, ntB)
	defer cli.Delete(context.Background(), prefixA+"/", clientv3.WithPrefix())
	defer cli.Delete(context.Background(), prefixB+"/", clientv3.WithPrefix())

	leaseA := grantLease(t, cli, 30)
	leaseB := grantLease(t, cli, 30)
	defer cli.Revoke(context.Background(), leaseA)
	defer cli.Revoke(context.Background(), leaseB)

	idA, err := allocateNodeID(context.Background(), cli, prefixA, makeInfo(ntA, 1), leaseA)
	if err != nil {
		t.Fatalf("alloc A: %v", err)
	}
	idB, err := allocateNodeID(context.Background(), cli, prefixB, makeInfo(ntB, 1), leaseB)
	if err != nil {
		t.Fatalf("alloc B: %v", err)
	}

	if idA != 1 || idB != 1 {
		t.Fatalf("expected both first allocations =1, got A=%d B=%d", idA, idB)
	}
}

// TestAllocReclaimAfterRevoke: revoke lease 之后,该 node_id 可以被立即重新分配。
func TestAllocReclaimAfterRevoke(t *testing.T) {
	cli := newTestClient(t)
	defer cli.Close()
	nt := uniqueNodeType()
	defer cleanupNodeType(t, cli, nt)

	prefix := rpcPrefix(nt)

	lease1 := grantLease(t, cli, 30)
	id1, err := allocateNodeID(context.Background(), cli, prefix, makeInfo(nt, 1), lease1)
	if err != nil {
		t.Fatalf("alloc 1: %v", err)
	}

	// Revoke lease,节点"死掉",etcd 应自动清理两个 key
	if _, err := cli.Revoke(context.Background(), lease1); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// 等 etcd 把 key 清掉(通常是同步的,但保险起见 sleep 一小下)
	time.Sleep(100 * time.Millisecond)

	lease2 := grantLease(t, cli, 30)
	defer cli.Revoke(context.Background(), lease2)
	id2, err := allocateNodeID(context.Background(), cli, prefix, makeInfo(nt, 1), lease2)
	if err != nil {
		t.Fatalf("alloc 2: %v", err)
	}

	if id2 != id1 {
		t.Fatalf("expected reclaim id=%d, got id=%d", id1, id2)
	}
}

// TestAllocCASRetryOnConflict: 人为占住 id=1 的 allocation key,allocator 应跳到 id=2。
func TestAllocCASRetryOnConflict(t *testing.T) {
	cli := newTestClient(t)
	defer cli.Close()
	nt := uniqueNodeType()
	defer cleanupNodeType(t, cli, nt)

	prefix := rpcPrefix(nt)
	intruderLease := grantLease(t, cli, 30)
	defer cli.Revoke(context.Background(), intruderLease)

	// 直接写 allocation key,但**不写** rpcPath —— 模拟 "alloc key 已被别人占,
	// 但 scanUsedNodeIDs 还没拿到这个状态" 的竞态:
	// 我们不更新 used 集合,allocator 第一次会尝试 id=1,Txn 失败后跳 id=2。
	// 为了让"扫描看不到"这个状态成立,我们绕过 scanUsedNodeIDs 直接调 tryClaimNodeID 测它的 CAS 行为。
	allocKey := allocationKey(prefix, nt, 1)
	if _, err := cli.Put(context.Background(), allocKey, "intruder-uuid", clientv3.WithLease(intruderLease)); err != nil {
		t.Fatalf("seed intruder: %v", err)
	}

	myLease := grantLease(t, cli, 30)
	defer cli.Revoke(context.Background(), myLease)

	info := makeInfo(nt, 1)
	// 直接 try id=1 —— 应失败 (allocationKey.Version != 0)
	ok, err := tryClaimNodeID(context.Background(), cli, prefix, 1, info, myLease)
	if err != nil {
		t.Fatalf("tryClaim id=1: %v", err)
	}
	if ok {
		t.Fatalf("tryClaim id=1 should have failed (intruder holds it)")
	}

	// 完整 allocateNodeID 流程应���成功(它会扫到 used={1}, 跳到 id=2)
	id, err := allocateNodeID(context.Background(), cli, prefix, makeInfo(nt, 1), myLease)
	if err != nil {
		t.Fatalf("allocateNodeID: %v", err)
	}
	if id == 1 {
		t.Fatalf("allocator should not return 1 (held by intruder)")
	}
	if id != 2 {
		t.Fatalf("expected id=2 (next free), got %d", id)
	}
}

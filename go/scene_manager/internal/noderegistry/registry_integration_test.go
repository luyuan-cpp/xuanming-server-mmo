//go:build integration

package noderegistry

// 集成测试:需要本地 etcd (127.0.0.1:2379) 在跑,连不上就 SKIP。
// 启动 etcd: 仓库根 docker compose -f deploy/docker-compose.yml up -d etcd
// 跑测试:    go test -count=1 -tags=integration ./internal/noderegistry/...
//
// 本包已是 shared/noderegistry 的薄适配层,node_id 分配、CAS、续租与重注册的细节由
// go/shared/noderegistry/registry_integration_test.go 覆盖。这里只经本包的公开接口
// (Register / KeepAlive / Close / Info)核对适配层接得对:
//  1. 写进 etcd 的 NodeInfo 字段齐全,Info 回填的 node_id / uuid 与 etcd 一致,双 key 同一租约;Close 删双 key 并 Revoke;
//  2. Close 只删仍归本实例(uuid)的 key,不删接管了同一 node_id 的别的实例;
//  3. 失租后重夺原 node_id;
//  4. 原 node_id 已归别的实例时换新号继续服务,不覆盖对方的注册;
//  5. 注册时跳过别的实例占着的 node_id;
//  6. 跨 zone 并发注册,node_id 不重复。
//
// 节点类型用不存在的枚举值(1300000 起),前缀因此都是 ".rpc",碰不到本机正在跑的真实服务的 key;
// 用例之间靠 node_type 段隔离,开始前与结束后都删掉自己 node_type 的 key。shared 按整个前缀扫已占号,
// 别的 node_type 的 key 也算占用,所以用例不假设分到的 node_id 是几,只核对相互关系;
// 「被别的实例接管」直接往 etcd 写入侵者的双 key,不靠注册顺序去凑。

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	base "proto/common/base"
	"shared/nodeinfo"
	sharedregistry "shared/noderegistry"
	"shared/releasetrack"
)

const (
	testEtcdEndpoint        = "127.0.0.1:2379"
	testLeaseTTL     int64  = 5
	testIP                  = "127.0.0.1"
	testPort         uint32 = 9150
)

var nodeTypeCounter atomic.Uint32

// uniqueNodeType 返回一个不存在的节点类型枚举值,每个用例独占。
func uniqueNodeType() uint32 {
	return 1300000 + nodeTypeCounter.Add(1)
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

// deleteNodeTypeKeys 删掉 nodeType 名下两棵子树(allocated/… 与 zone/…)的全部 key。
func deleteNodeTypeKeys(cli *clientv3.Client, nodeType uint32) {
	prefix := rpcPrefix(nodeType)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = cli.Delete(ctx, fmt.Sprintf("%s/allocated/node_type/%d/", prefix, nodeType), clientv3.WithPrefix())
	resp, err := cli.Get(ctx, prefix+"/zone/", clientv3.WithPrefix(), clientv3.WithKeysOnly())
	if err != nil {
		return
	}
	needle := fmt.Sprintf("/node_type/%d/", nodeType)
	for _, kv := range resp.Kvs {
		if strings.Contains(string(kv.Key), needle) {
			_, _ = cli.Delete(ctx, string(kv.Key))
		}
	}
}

// isolateNodeType 给用例一个独占节点类型:先清掉上次异常退出留下的同名 key,结束时再清一遍。
// 清理登记在 newTestClient 之后,按 t.Cleanup 的后进先出顺序先于关闭 client 执行。
func isolateNodeType(t *testing.T, cli *clientv3.Client) uint32 {
	t.Helper()
	nt := uniqueNodeType()
	deleteNodeTypeKeys(cli, nt)
	t.Cleanup(func() { deleteNodeTypeKeys(cli, nt) })
	return nt
}

// register 经公开的 Register 注册,并登记 Close(Close 幂等,用例里提前 Close 也没关系)。
func register(t *testing.T, cli *clientv3.Client, nodeType, zoneID uint32) *NodeRegistration {
	t.Helper()
	nr, err := Register(cli, nodeType, zoneID, testIP, testPort, testLeaseTTL)
	if err != nil {
		t.Fatalf("Register(node_type=%d zone=%d): %v", nodeType, zoneID, err)
	}
	t.Cleanup(nr.Close)
	return nr
}

func allocKey(nodeType, id uint32) string {
	return sharedregistry.AllocationKey(rpcPrefix(nodeType), nodeType, id)
}

func rpcKey(nodeType, zoneID, id uint32) string {
	return sharedregistry.RpcPath(rpcPrefix(nodeType), zoneID, nodeType, id)
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

// holderOf 读 node_id 的分配键:归属 uuid 与所挂租约;key 不存在时 ok=false。
func holderOf(t *testing.T, cli *clientv3.Client, nodeType, id uint32) (holder string, lease clientv3.LeaseID, ok bool) {
	t.Helper()
	resp := getKV(t, cli, allocKey(nodeType, id))
	if len(resp.Kvs) == 0 {
		return "", 0, false
	}
	return string(resp.Kvs[0].Value), clientv3.LeaseID(resp.Kvs[0].Lease), true
}

// idHeldBy 在 nodeType 的分配子树里找归属 uuid 的 node_id。
func idHeldBy(t *testing.T, cli *clientv3.Client, nodeType uint32, uuid string) (uint32, bool) {
	t.Helper()
	subtree := fmt.Sprintf("%s/allocated/node_type/%d/node_id/", rpcPrefix(nodeType), nodeType)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := cli.Get(ctx, subtree, clientv3.WithPrefix())
	if err != nil {
		t.Fatalf("get %s: %v", subtree, err)
	}
	for _, kv := range resp.Kvs {
		if string(kv.Value) != uuid {
			continue
		}
		id, err := strconv.ParseUint(strings.TrimPrefix(string(kv.Key), subtree), 10, 32)
		if err != nil {
			t.Fatalf("unexpected allocation key %s", kv.Key)
		}
		return uint32(id), true
	}
	return 0, false
}

func leaseGone(t *testing.T, cli *clientv3.Client, lease clientv3.LeaseID) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ttl, err := cli.TimeToLive(ctx, lease)
	return err == nil && ttl.TTL <= 0
}

// simulateLeaseLoss 撤销注册所挂的租约:etcd 随即删掉双 key,效果等同租约过期。返回被撤销的租约。
func simulateLeaseLoss(t *testing.T, cli *clientv3.Client, nodeType, id uint32) clientv3.LeaseID {
	t.Helper()
	_, lease, ok := holderOf(t, cli, nodeType, id)
	if !ok {
		t.Fatalf("node_id=%d is not registered", id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cli.Revoke(ctx, lease); err != nil {
		t.Fatalf("revoke lease %x: %v", int64(lease), err)
	}
	return lease
}

// seedTakeover 模拟另一个实例合法占住 (nodeType, id):在它自己的租约上、同一个 Txn 里写分配键与
// zone 下的发现键;id 意外已被占用就让用例失败,不覆盖。返回入侵者的 uuid。
func seedTakeover(t *testing.T, cli *clientv3.Client, nodeType, zoneID, id uint32) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	grant, err := cli.Grant(ctx, 30)
	if err != nil {
		t.Fatalf("grant intruder lease: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = cli.Revoke(ctx, grant.ID)
	})
	intruder := fmt.Sprintf("intruder-%d-%d", nodeType, id)
	value := fmt.Sprintf(`{"nodeId":%d,"nodeType":%d,"zoneId":%d,"nodeUuid":%q}`, id, nodeType, zoneID, intruder)
	resp, err := cli.Txn(ctx).
		If(clientv3.Compare(clientv3.Version(allocKey(nodeType, id)), "=", 0)).
		Then(
			clientv3.OpPut(allocKey(nodeType, id), intruder, clientv3.WithLease(grant.ID)),
			clientv3.OpPut(rpcKey(nodeType, zoneID, id), value, clientv3.WithLease(grant.ID)),
		).
		Commit()
	if err != nil || !resp.Succeeded {
		t.Fatalf("seed takeover of node_id=%d: succeeded=%v err=%v", id, resp != nil && resp.Succeeded, err)
	}
	return intruder
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

// 注册值由 BuildValue 从模板生成,Info 回填的身份与 etcd 一致:client_rpc_router 拿 Info.NodeUuid 在
// 同轨道路由服里排名次,各服务拿 Info 打启动日志,对不上就是用错了身份。
func TestRegister_WritesNodeInfoAndBackfillsInfo(t *testing.T) {
	t.Setenv(releasetrack.EnvVar, "canary") // 进程轨道要原样进注册值
	cli := newTestClient(t)
	nt := isolateNodeType(t, cli)
	const zone uint32 = 3

	nr := register(t, cli, nt, zone)
	id := nr.Info.GetNodeId()
	if id == 0 || nr.Info.GetNodeUuid() == "" {
		t.Fatalf("Info not backfilled: node_id=%d uuid=%q", id, nr.Info.GetNodeUuid())
	}
	holder, lease, ok := holderOf(t, cli, nt, id)
	if !ok || holder != nr.Info.GetNodeUuid() {
		t.Fatalf("allocation key node_id=%d: holder=%q present=%v, Info says %q", id, holder, ok, nr.Info.GetNodeUuid())
	}
	p := getKV(t, cli, rpcKey(nt, zone, id))
	if len(p.Kvs) != 1 || clientv3.LeaseID(p.Kvs[0].Lease) != lease {
		t.Fatalf("rpc path node_id=%d: want one key on lease %x, got %+v", id, int64(lease), p.Kvs)
	}
	var got base.NodeInfo
	if err := nodeinfo.Unmarshal(p.Kvs[0].Value, &got); err != nil {
		t.Fatalf("rpc path value is not a NodeInfo: %v", err)
	}
	if got.GetNodeId() != id || got.GetNodeUuid() != nr.Info.GetNodeUuid() ||
		got.GetNodeType() != nt || got.GetZoneId() != zone ||
		got.GetEndpoint().GetIp() != testIP || got.GetEndpoint().GetPort() != testPort ||
		got.GetGrpcEndpoint().GetIp() != testIP || got.GetGrpcEndpoint().GetPort() != testPort ||
		got.GetProtocolType() != uint32(base.ENodeProtocolType_PROTOCOL_GRPC) ||
		got.GetReleaseTrack() != releasetrack.Canary.String() ||
		got.GetLaunchTime() != nr.Info.GetLaunchTime() {
		t.Fatalf("registered NodeInfo %v does not match Info %v", &got, nr.Info)
	}

	nr.Close()
	nr.Close() // 幂等
	if _, _, ok := holderOf(t, cli, nt, id); ok || len(getKV(t, cli, rpcKey(nt, zone, id)).Kvs) != 0 {
		t.Fatal("Close must delete both keys")
	}
	if !leaseGone(t, cli, lease) {
		t.Fatal("Close must revoke the lease")
	}
}

// A 失租、另一个实例合法接管同一 node_id;A 随后 Close 不得删掉对方的 key(R3)。
func TestClose_LeavesKeysOfTheInstanceThatTookOver(t *testing.T) {
	cli := newTestClient(t)
	nt := isolateNodeType(t, cli)

	a := register(t, cli, nt, 1)
	id := a.Info.GetNodeId()
	simulateLeaseLoss(t, cli, nt, id) // 没开 KeepAlive,A 不会重注册
	intruder := seedTakeover(t, cli, nt, 2, id)

	a.Close()
	if holder, _, ok := holderOf(t, cli, nt, id); !ok || holder != intruder {
		t.Fatalf("A's Close touched node_id=%d held by %q: holder=%q present=%v", id, intruder, holder, ok)
	}
	if len(getKV(t, cli, rpcKey(nt, 2, id)).Kvs) != 1 {
		t.Fatal("A's Close deleted the rpc path of the instance that took over")
	}
}

// 失租后原 node_id 没人占:在新租约上重夺原号,发现键随之改挂(R4:续约断了不会从此在发现里消失)。
func TestKeepAlive_ReclaimsOriginalIDAfterLeaseLoss(t *testing.T) {
	cli := newTestClient(t)
	nt := isolateNodeType(t, cli)

	nr := register(t, cli, nt, 1)
	id := nr.Info.GetNodeId()
	nr.KeepAlive()
	oldLease := simulateLeaseLoss(t, cli, nt, id)

	var newLease clientv3.LeaseID
	waitFor(t, 15*time.Second, "original node_id reclaimed on a new lease", func() bool {
		holder, lease, ok := holderOf(t, cli, nt, id)
		newLease = lease
		return ok && holder == nr.Info.GetNodeUuid() && lease != oldLease
	})
	p := getKV(t, cli, rpcKey(nt, 1, id))
	if len(p.Kvs) != 1 || clientv3.LeaseID(p.Kvs[0].Lease) != newLease {
		t.Fatalf("rpc path node_id=%d not moved to the new lease %x: %+v", id, int64(newLease), p.Kvs)
	}
}

// 失租后原 node_id 已归别的实例:换新号继续服务,不覆盖对方的注册,uuid 不变;Close 只删自己的新号。
// 钉住的是旧副本修过的历史 bug:失租后直接 Put 旧 key,覆盖别人的 NodeInfo,C++ 把流量路由到错误节点。
func TestKeepAlive_SwitchesIDWithoutOverwritingTheInstanceThatTookOver(t *testing.T) {
	cli := newTestClient(t)
	nt := isolateNodeType(t, cli)

	a := register(t, cli, nt, 1)
	aID := a.Info.GetNodeId()
	uuidA := a.Info.GetNodeUuid()
	simulateLeaseLoss(t, cli, nt, aID)
	intruder := seedTakeover(t, cli, nt, 2, aID)
	a.KeepAlive()

	var newID uint32
	waitFor(t, 15*time.Second, "A re-registered under a new node_id", func() bool {
		id, ok := idHeldBy(t, cli, nt, uuidA)
		newID = id
		return ok
	})
	if newID == aID {
		t.Fatalf("A reclaimed node_id=%d that another instance holds", aID)
	}
	if holder, _, ok := holderOf(t, cli, nt, aID); !ok || holder != intruder {
		t.Fatalf("A overwrote node_id=%d: holder=%q present=%v, want %q", aID, holder, ok, intruder)
	}
	if v := getKV(t, cli, rpcKey(nt, 2, aID)); len(v.Kvs) != 1 || !strings.Contains(string(v.Kvs[0].Value), intruder) {
		t.Fatalf("rpc path of the instance that took over node_id=%d was changed: %+v", aID, v.Kvs)
	}
	p := getKV(t, cli, rpcKey(nt, 1, newID))
	if len(p.Kvs) != 1 {
		t.Fatalf("A's new rpc path node_id=%d is missing", newID)
	}
	var got base.NodeInfo
	if err := nodeinfo.Unmarshal(p.Kvs[0].Value, &got); err != nil || got.GetNodeId() != newID || got.GetNodeUuid() != uuidA {
		t.Fatalf("A's new NodeInfo: node_id=%d uuid=%q err=%v, want node_id=%d uuid=%q",
			got.GetNodeId(), got.GetNodeUuid(), err, newID, uuidA)
	}

	a.Close()
	if _, ok := idHeldBy(t, cli, nt, uuidA); ok || len(getKV(t, cli, rpcKey(nt, 1, newID)).Kvs) != 0 {
		t.Fatal("Close left A's new keys behind")
	}
	if holder, _, ok := holderOf(t, cli, nt, aID); !ok || holder != intruder {
		t.Fatalf("A's Close deleted node_id=%d held by %q", aID, intruder)
	}
}

// 注册时跳过别的实例占着的 node_id,不覆盖它的分配键。
func TestRegister_SkipsNodeIDHeldByAnotherInstance(t *testing.T) {
	cli := newTestClient(t)
	nt := isolateNodeType(t, cli)

	probe := register(t, cli, nt, 1)
	id := probe.Info.GetNodeId()
	probe.Close() // id 空出来,随即被别的实例占住
	intruder := seedTakeover(t, cli, nt, 2, id)

	nr := register(t, cli, nt, 1)
	if nr.Info.GetNodeId() == id {
		t.Fatalf("Register took node_id=%d held by %q", id, intruder)
	}
	if holder, _, ok := holderOf(t, cli, nt, id); !ok || holder != intruder {
		t.Fatalf("Register overwrote node_id=%d: holder=%q present=%v", id, holder, ok)
	}
}

// 跨 zone 并发注册:分配键路径不带 zone,不同 zone 的实例也不会拿到同一个 node_id。
// 这里不开 KeepAlive,租约给足 30s,免得慢机器上早注册的 key 先过期、把号让给后来者。
func TestRegister_ConcurrentAcrossZonesGetsUniqueIDs(t *testing.T) {
	cli := newTestClient(t)
	nt := isolateNodeType(t, cli)

	const (
		perZone  = 10
		leaseTTL = int64(30)
	)
	zones := []uint32{1, 2, 3}
	var (
		mu   sync.Mutex
		regs []*NodeRegistration
		wg   sync.WaitGroup
	)
	errCh := make(chan error, len(zones)*perZone)
	for _, zone := range zones {
		for i := 0; i < perZone; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				nr, err := Register(cli, nt, zone, testIP, testPort, leaseTTL)
				if err != nil {
					errCh <- fmt.Errorf("zone=%d: %w", zone, err)
					return
				}
				mu.Lock()
				regs = append(regs, nr)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	close(errCh)
	for _, nr := range regs {
		t.Cleanup(nr.Close)
	}
	for err := range errCh {
		t.Fatalf("concurrent register: %v", err)
	}

	seen := make(map[uint32]string, len(regs))
	for _, nr := range regs {
		id := nr.Info.GetNodeId()
		if id == 0 {
			t.Fatal("node_id 0 was allocated")
		}
		if other, dup := seen[id]; dup {
			t.Fatalf("node_id=%d handed to both %s and %s", id, other, nr.Info.GetNodeUuid())
		}
		seen[id] = nr.Info.GetNodeUuid()
	}
	if len(seen) != len(zones)*perZone {
		t.Fatalf("want %d unique node_ids, got %d", len(zones)*perZone, len(seen))
	}
}

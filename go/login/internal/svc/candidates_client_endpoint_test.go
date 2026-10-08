package svc

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"login/internal/logic/pkg/loginqueue"
	basepb "proto/common/base"
)

// 本文件盯住 login 下发 gate 地址的出口(集群外入口 D76–D78):
// 候选的 IP/Port 必须是 gate 自报的客户端地址(缺省回落 endpoint),
// RequireClientEndpoint 下缺地址的 gate 必须跳过,同一客户端地址上的陈旧记录必须被去重。
// 规则本身在 shared/clientendpoint 有矩阵测试,这里只验 login 的接线与顺序。

const gateRPCPort = 18000

// gateNode 造一条 gate 的 NodeInfo。endpoint 恒为 podIP:18000(集群内身份);client 为 nil 表示没自报。
func gateNode(nodeID, zoneID uint32, launchTime uint64, podIP string, playerCount uint32, client *basepb.EndpointComp) *basepb.NodeInfo {
	return &basepb.NodeInfo{
		NodeId:         nodeID,
		ZoneId:         zoneID,
		LaunchTime:     launchTime,
		PlayerCount:    playerCount,
		Endpoint:       &basepb.EndpointComp{Ip: podIP, Port: gateRPCPort},
		ClientEndpoint: client,
	}
}

func clientAddr(ip string, port uint32) *basepb.EndpointComp {
	return &basepb.EndpointComp{Ip: ip, Port: port}
}

func assertCandidates(t *testing.T, got, want []loginqueue.GateCandidate) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("候选集不符:\n got  %+v\n want %+v", got, want)
	}
}

func TestBuildGateCandidates_UsesClientEndpoint(t *testing.T) {
	nodes := []*basepb.NodeInfo{
		gateNode(4, 1, 100, "10.244.0.4", 7, clientAddr("127.0.0.1", 30000)),
		gateNode(5, 1, 100, "10.244.0.5", 3, clientAddr("gate-1.z1.example.com", 18000)),
	}
	want := []loginqueue.GateCandidate{
		{NodeID: 4, IP: "127.0.0.1", Port: 30000, PlayerCount: 7, ZoneID: 1},
		{NodeID: 5, IP: "gate-1.z1.example.com", Port: 18000, PlayerCount: 3, ZoneID: 1},
	}
	for _, require := range []bool{false, true} {
		assertCandidates(t, buildGateCandidates(nodes, 1, require), want)
	}
}

// podip 模式 / 滚动过渡期:gate 没自报就回落 PodIP,行为与改动前一致。
func TestBuildGateCandidates_FallsBackToEndpointWhenNotRequired(t *testing.T) {
	nodes := []*basepb.NodeInfo{
		gateNode(4, 1, 100, "10.244.0.4", 7, nil),
		gateNode(5, 1, 100, "10.244.0.5", 3, clientAddr("127.0.0.1", 30001)),
	}
	assertCandidates(t, buildGateCandidates(nodes, 1, false), []loginqueue.GateCandidate{
		{NodeID: 4, IP: "10.244.0.4", Port: gateRPCPort, PlayerCount: 7, ZoneID: 1},
		{NodeID: 5, IP: "127.0.0.1", Port: 30001, PlayerCount: 3, ZoneID: 1},
	})
}

// external 模式:没自报的 gate 不进候选集,绝不把集群外连不上的 PodIP 发给玩家。
func TestBuildGateCandidates_SkipsGateWithoutClientEndpointWhenRequired(t *testing.T) {
	nodes := []*basepb.NodeInfo{
		gateNode(4, 1, 100, "10.244.0.4", 7, nil),
		gateNode(5, 1, 100, "10.244.0.5", 3, clientAddr("127.0.0.1", 30001)),
	}
	assertCandidates(t, buildGateCandidates(nodes, 1, true), []loginqueue.GateCandidate{
		{NodeID: 5, IP: "127.0.0.1", Port: 30001, PlayerCount: 3, ZoneID: 1},
	})

	onlyMissing := []*basepb.NodeInfo{gateNode(4, 1, 100, "10.244.0.4", 7, nil)}
	if got := buildGateCandidates(onlyMissing, 1, true); len(got) != 0 {
		t.Fatalf("全部缺地址时应无候选,got %+v", got)
	}
}

// 半填(只有 ip 或只有 port)等同于没自报:不要求时回落,要求时跳过。
func TestBuildGateCandidates_HalfFilledClientEndpointIsMissing(t *testing.T) {
	for _, half := range []*basepb.EndpointComp{clientAddr("127.0.0.1", 0), clientAddr("", 30000)} {
		nodes := []*basepb.NodeInfo{gateNode(4, 1, 100, "10.244.0.4", 7, half)}

		assertCandidates(t, buildGateCandidates(nodes, 1, false), []loginqueue.GateCandidate{
			{NodeID: 4, IP: "10.244.0.4", Port: gateRPCPort, PlayerCount: 7, ZoneID: 1},
		})
		if got := buildGateCandidates(nodes, 1, true); len(got) != 0 {
			t.Fatalf("半填 %+v 在 require 下应被跳过,got %+v", half, got)
		}
	}
}

// gate-1 崩溃重建:旧记录(node 5)要等 NodeTTL 才过期,与新记录(node 9)报同一个客户端地址。
// 只能留新的,否则票据签给 node 5、玩家连到 node 9,被 token_gate_node_mismatch 拒绝。
func TestBuildGateCandidates_DedupesStaleRecordOnSameClientAddress(t *testing.T) {
	nodes := []*basepb.NodeInfo{
		gateNode(5, 1, 100, "10.244.0.5", 50, clientAddr("127.0.0.1", 30001)), // 陈旧
		gateNode(4, 1, 150, "10.244.0.4", 7, clientAddr("127.0.0.1", 30000)),
		gateNode(9, 1, 200, "10.244.0.9", 0, clientAddr("127.0.0.1", 30001)), // 重建后的新 gate-1
	}
	want := []loginqueue.GateCandidate{
		{NodeID: 4, IP: "127.0.0.1", Port: 30000, PlayerCount: 7, ZoneID: 1},
		{NodeID: 9, IP: "127.0.0.1", Port: 30001, PlayerCount: 0, ZoneID: 1},
	}
	for _, require := range []bool{false, true} {
		assertCandidates(t, buildGateCandidates(nodes, 1, require), want)
	}
}

// 去重按「选中的地址」做,所以回落模式下同一 PodIP:port 上的新旧记录(容器原地重启)同样只留新的。
func TestBuildGateCandidates_DedupesOnFallbackAddressToo(t *testing.T) {
	nodes := []*basepb.NodeInfo{
		gateNode(9, 1, 200, "10.244.0.5", 0, nil),
		gateNode(5, 1, 100, "10.244.0.5", 50, nil),
	}
	assertCandidates(t, buildGateCandidates(nodes, 1, false), []loginqueue.GateCandidate{
		{NodeID: 9, IP: "10.244.0.5", Port: gateRPCPort, PlayerCount: 0, ZoneID: 1},
	})
}

// 去重在按 zone 过滤之后:别的 zone 里更新的同地址记录不能把本 zone 的 gate 挤掉。
func TestBuildGateCandidates_ZoneFilterRunsBeforeDedupe(t *testing.T) {
	nodes := []*basepb.NodeInfo{
		gateNode(4, 1, 100, "10.244.0.4", 7, clientAddr("127.0.0.1", 30000)),
		gateNode(8, 2, 900, "10.244.1.8", 1, clientAddr("127.0.0.1", 30000)), // 配错:与 zone 1 撞地址
	}
	assertCandidates(t, buildGateCandidates(nodes, 1, true), []loginqueue.GateCandidate{
		{NodeID: 4, IP: "127.0.0.1", Port: 30000, PlayerCount: 7, ZoneID: 1},
	})
	assertCandidates(t, buildGateCandidates(nodes, 2, true), []loginqueue.GateCandidate{
		{NodeID: 8, IP: "127.0.0.1", Port: 30000, PlayerCount: 1, ZoneID: 2},
	})
	// zoneID=0 = 不按 zone 过滤,此时两条同地址,只留 launch_time 大的。
	assertCandidates(t, buildGateCandidates(nodes, 0, true), []loginqueue.GateCandidate{
		{NodeID: 8, IP: "127.0.0.1", Port: 30000, PlayerCount: 1, ZoneID: 2},
	})
}

// 没有 endpoint 的畸形记录(以及 nil)照旧直接跳过,即使它带了客户端地址。
func TestBuildGateCandidates_SkipsMalformedRecords(t *testing.T) {
	noIdentity := &basepb.NodeInfo{NodeId: 6, ZoneId: 1, LaunchTime: 100, ClientEndpoint: clientAddr("127.0.0.1", 30002)}
	nodes := []*basepb.NodeInfo{nil, noIdentity, gateNode(4, 1, 100, "10.244.0.4", 7, nil)}
	assertCandidates(t, buildGateCandidates(nodes, 1, false), []loginqueue.GateCandidate{
		{NodeID: 4, IP: "10.244.0.4", Port: gateRPCPort, PlayerCount: 7, ZoneID: 1},
	})
	if got := buildGateCandidates(nil, 1, false); len(got) != 0 {
		t.Fatalf("空输入应无候选,got %+v", got)
	}
}

// newSvcTestRedis 起一个进程内 miniredis,测试结束自动关闭。
func newSvcTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

// 顺序契约:先按客户端地址去重,再剔除 draining。
// gate-1 重建后新记录 node 9 与陈旧记录 node 5 报同一个地址,运维按 node_id 把活着的 node 9 标了排空。
// 顺序对:去重先淘汰 node 5,排空再剔掉 node 9,只剩 node 4。
// 顺序反:排空先剔掉 node 9,node 5 失去竞争者留下来 —— 玩家被派到一台已不存在的 gate,
// 连上同地址的新 gate 被 token_gate_node_mismatch 拒绝。
func TestCandidatesFromNodes_DedupeRunsBeforeDrainingFilter(t *testing.T) {
	rdb, _ := newSvcTestRedis(t)
	ctx := context.Background()

	nodes := []*basepb.NodeInfo{
		gateNode(5, 1, 100, "10.244.0.5", 50, clientAddr("127.0.0.1", 30001)), // 陈旧,没被标记
		gateNode(4, 1, 150, "10.244.0.4", 7, clientAddr("127.0.0.1", 30000)),
		gateNode(9, 1, 200, "10.244.0.9", 3, clientAddr("127.0.0.1", 30001)), // 重建后的新 gate-1
	}

	// 对照:没有排空标记时,去重已经淘汰了 node 5。
	assertCandidates(t, candidatesFromNodes(ctx, rdb, nodes, 1, true), []loginqueue.GateCandidate{
		{NodeID: 4, IP: "127.0.0.1", Port: 30000, PlayerCount: 7, ZoneID: 1},
		{NodeID: 9, IP: "127.0.0.1", Port: 30001, PlayerCount: 3, ZoneID: 1},
	})

	if err := loginqueue.MarkGateDraining(ctx, rdb, 9, time.Now().Unix(), 600); err != nil {
		t.Fatalf("mark gate 9 draining: %v", err)
	}
	want := []loginqueue.GateCandidate{
		{NodeID: 4, IP: "127.0.0.1", Port: 30000, PlayerCount: 7, ZoneID: 1},
	}
	for _, require := range []bool{false, true} {
		got := candidatesFromNodes(ctx, rdb, nodes, 1, require)
		for _, c := range got {
			if c.NodeID == 5 {
				t.Fatalf("require=%v: 陈旧记录 node 5 不得在同地址的新 gate 被排空后顶上来,got %+v", require, got)
			}
		}
		assertCandidates(t, got, want)
	}
}

// rdb 为 nil(队列关闭 / 早期初始化路径)时不做排空过滤,只剩选址与去重。
func TestCandidatesFromNodes_NilRedisSkipsDrainingFilter(t *testing.T) {
	nodes := []*basepb.NodeInfo{
		gateNode(5, 1, 100, "10.244.0.5", 50, clientAddr("127.0.0.1", 30001)),
		gateNode(9, 1, 200, "10.244.0.9", 3, clientAddr("127.0.0.1", 30001)),
	}
	assertCandidates(t, candidatesFromNodes(context.Background(), nil, nodes, 1, true), []loginqueue.GateCandidate{
		{NodeID: 9, IP: "127.0.0.1", Port: 30001, PlayerCount: 3, ZoneID: 1},
	})
}

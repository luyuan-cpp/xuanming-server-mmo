package logic

// 跨 zone 重定向(RedirectToGate)出口的客户端地址选择(k8s-client-entry D76–D78):
//   - 下发给客户端的 TargetGateIp/Port 必须等于 shared/clientendpoint.Select 的结果;
//   - RequireClientEndpoint=true 时跳过没自报 client_endpoint 的 gate,半填视为缺失;
//   - 按「选出的客户端地址」去重,同一地址只留 launch_time 最大的一条,
//     防止 gate 崩溃后残留的旧记录(player_count 更低)被当成最空闲的挑中;
//   - fetchGateNodes 的解析路径(decodeGateNodes)忽略未知字段(D77),坏值逐条跳过。
// 只测纯函数 decodeGateNodes / selectGateTargets / signRedirectToGate,不依赖 etcd、Redis 与墙钟。

import (
	"fmt"
	"testing"
	"time"

	commonpb "proto/common/base"
	"shared/clientendpoint"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"google.golang.org/protobuf/proto"
)

const (
	testRedirectZoneID = uint32(2)
	testRedirectSecret = "test-gate-token-secret"
)

func testEndpoint(ip string, port uint32) *commonpb.EndpointComp {
	return &commonpb.EndpointComp{Ip: ip, Port: port}
}

// testGateNode 造一条 gate NodeInfo。client 传 nil 表示该 gate 没自报客户端地址(podip 模式 / 旧版本)。
func testGateNode(nodeID uint32, launchTime uint64, playerCount uint32, endpoint, client *commonpb.EndpointComp) *commonpb.NodeInfo {
	return &commonpb.NodeInfo{
		NodeId:         nodeID,
		NodeType:       uint32(commonpb.ENodeType_GateNodeService),
		LaunchTime:     launchTime,
		ZoneId:         testRedirectZoneID,
		PlayerCount:    playerCount,
		Endpoint:       endpoint,
		ClientEndpoint: client,
	}
}

func targetNodeIDs(targets []gateTarget) []uint32 {
	ids := make([]uint32, 0, len(targets))
	for _, t := range targets {
		ids = append(ids, t.node.GetNodeId())
	}
	return ids
}

func TestSelectGateTargets_PrefersClientEndpoint(t *testing.T) {
	n := testGateNode(1, 100, 0, testEndpoint("10.0.0.1", 18000), testEndpoint("127.0.0.1", 30000))

	for _, requireClient := range []bool{false, true} {
		targets := selectGateTargets([]*commonpb.NodeInfo{n}, requireClient)
		require.Len(t, targets, 1, "require=%v", requireClient)

		wantHost, wantPort, ok := clientendpoint.Select(
			n.GetClientEndpoint().GetIp(), n.GetClientEndpoint().GetPort(),
			n.GetEndpoint().GetIp(), n.GetEndpoint().GetPort(), requireClient)
		require.True(t, ok)
		assert.Equal(t, wantHost, targets[0].host, "候选地址必须就是 Select 的结果")
		assert.Equal(t, wantPort, targets[0].port)
		assert.Equal(t, "127.0.0.1", targets[0].host, "有可用 client_endpoint 时不得下发集群内 endpoint")
		assert.Equal(t, uint32(30000), targets[0].port)
		assert.Same(t, n, targets[0].node, "候选必须携带原 NodeInfo,票据里的 gate_node_id 取自它")
	}
}

func TestSelectGateTargets_FallsBackToEndpointWhenNotRequired(t *testing.T) {
	n := testGateNode(1, 100, 0, testEndpoint("10.0.0.1", 18000), nil)

	targets := selectGateTargets([]*commonpb.NodeInfo{n}, false)

	require.Len(t, targets, 1)
	assert.Equal(t, "10.0.0.1", targets[0].host, "podip 模式:没自报客户端地址时回落 endpoint")
	assert.Equal(t, uint32(18000), targets[0].port)
}

func TestSelectGateTargets_SkipsGateWithoutClientEndpointWhenRequired(t *testing.T) {
	legacy := testGateNode(1, 100, 0, testEndpoint("10.0.0.1", 18000), nil)
	external := testGateNode(2, 100, 50, testEndpoint("10.0.0.2", 18000), testEndpoint("gate-1.z2.example.com", 18000))

	targets := selectGateTargets([]*commonpb.NodeInfo{legacy, external}, true)

	require.Len(t, targets, 1, "require=true 时没有 client_endpoint 的 gate 必须被跳过,哪怕它更空闲")
	assert.Equal(t, uint32(2), targets[0].node.GetNodeId())
	assert.Equal(t, "gate-1.z2.example.com", targets[0].host, "client_endpoint 可以是 DNS 名")
	assert.Equal(t, uint32(18000), targets[0].port)
}

func TestSelectGateTargets_HalfFilledClientEndpointIsMissing(t *testing.T) {
	cases := []struct {
		name   string
		client *commonpb.EndpointComp
	}{
		{name: "只有 ip", client: testEndpoint("127.0.0.1", 0)},
		{name: "只有 port", client: testEndpoint("", 30000)},
		{name: "port 越界", client: testEndpoint("127.0.0.1", 65536)},
		{name: "空子消息", client: &commonpb.EndpointComp{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := testGateNode(1, 100, 0, testEndpoint("10.0.0.1", 18000), tc.client)

			fallback := selectGateTargets([]*commonpb.NodeInfo{n}, false)
			require.Len(t, fallback, 1)
			assert.Equal(t, "10.0.0.1", fallback[0].host, "半填视为缺失,require=false 回落 endpoint")
			assert.Equal(t, uint32(18000), fallback[0].port)

			assert.Empty(t, selectGateTargets([]*commonpb.NodeInfo{n}, true), "半填视为缺失,require=true 跳过")
		})
	}
}

func TestSelectGateTargets_DedupesStaleRecordBySameClientAddress(t *testing.T) {
	// 稳定地址(NodePort)下 gate-0 崩溃重启:旧记录(node 1)残留到租约过期,新 gate(node 3)沿用同一客户端地址。
	stale := testGateNode(1, 100, 0, testEndpoint("10.0.0.1", 18000), testEndpoint("127.0.0.1", 30000))
	other := testGateNode(2, 150, 10, testEndpoint("10.0.0.2", 18000), testEndpoint("127.0.0.1", 30001))
	fresh := testGateNode(3, 200, 40, testEndpoint("10.0.0.9", 18000), testEndpoint("127.0.0.1", 30000))

	targets := selectGateTargets([]*commonpb.NodeInfo{stale, other, fresh}, true)

	assert.ElementsMatch(t, []uint32{2, 3}, targetNodeIDs(targets),
		"同一客户端地址只留 launch_time 最大的一条;不同端口是不同 gate,都保留")
}

func TestSelectGateTargets_DedupesByEffectiveAddressInFallbackMode(t *testing.T) {
	// podip 模式下容器原地重启(PodIP 不变):两条记录回落后的 endpoint 相同,同样按有效地址去重。
	stale := testGateNode(1, 100, 0, testEndpoint("10.0.0.1", 18000), nil)
	fresh := testGateNode(3, 200, 40, testEndpoint("10.0.0.1", 18000), nil)

	targets := selectGateTargets([]*commonpb.NodeInfo{stale, fresh}, false)

	assert.Equal(t, []uint32{3}, targetNodeIDs(targets))
}

func TestSignRedirectToGate_TargetAddressIsSelectResult(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	busy := testGateNode(1, 100, 80, testEndpoint("10.0.0.1", 18000), testEndpoint("127.0.0.1", 30000))
	idle := testGateNode(2, 100, 5, testEndpoint("10.0.0.2", 18000), testEndpoint("127.0.0.1", 30001))
	const playerID = uint64(9_000_001)
	const targetZone = testRedirectZoneID

	info, err := signRedirectToGate([]*commonpb.NodeInfo{busy, idle}, true, testRedirectSecret, targetZone, playerID, now)

	require.NoError(t, err)
	wantHost, wantPort, ok := clientendpoint.Select(
		idle.GetClientEndpoint().GetIp(), idle.GetClientEndpoint().GetPort(),
		idle.GetEndpoint().GetIp(), idle.GetEndpoint().GetPort(), true)
	require.True(t, ok)
	assert.Equal(t, wantHost, info.GetTargetGateIp(), "TargetGateIp 必须等于 Select 的结果")
	assert.Equal(t, wantPort, info.GetTargetGatePort(), "TargetGatePort 必须等于 Select 的结果")
	assert.NotEqual(t, idle.GetEndpoint().GetIp(), info.GetTargetGateIp(), "external 模式不得下发 PodIP")

	payload := &commonpb.GateTokenPayload{}
	require.NoError(t, proto.Unmarshal(info.GetTokenPayload(), payload))
	assert.Equal(t, uint32(2), payload.GetGateNodeId(), "挑负载最低的 gate,票据绑定它的 node_id")
	assert.Equal(t, testRedirectZoneID, payload.GetZoneId())
	assert.Equal(t, playerID, payload.GetPlayerId())
	assert.Equal(t, targetZone, payload.GetTargetZoneId())
	assert.Equal(t, now.Unix()+redirectTokenTTLSeconds, payload.GetExpireTimestamp())
	assert.Equal(t, payload.GetExpireTimestamp(), info.GetTokenDeadline())
	assert.Equal(t, signHMAC(testRedirectSecret, info.GetTokenPayload()), info.GetTokenSignature())
}

func TestSignRedirectToGate_FallbackAddressWhenNotRequired(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	n := testGateNode(1, 100, 0, testEndpoint("10.0.0.1", 18000), nil)

	info, err := signRedirectToGate([]*commonpb.NodeInfo{n}, false, testRedirectSecret, testRedirectZoneID, 1, now)

	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1", info.GetTargetGateIp(), "podip 模式行为与改动前一致:下发 endpoint")
	assert.Equal(t, uint32(18000), info.GetTargetGatePort())
}

func TestSignRedirectToGate_NeverPicksStaleRecordOfCrashedGate(t *testing.T) {
	// 旧记录 player_count=0,比新 gate 更「空闲」;不去重就会签给旧 node_id,
	// 客户端连上的却是新 gate,被 token_gate_node_mismatch 拒绝(D78)。
	now := time.Unix(1_800_000_000, 0)
	stale := testGateNode(1, 100, 0, testEndpoint("10.0.0.1", 18000), testEndpoint("127.0.0.1", 30000))
	fresh := testGateNode(3, 200, 40, testEndpoint("10.0.0.9", 18000), testEndpoint("127.0.0.1", 30000))

	info, err := signRedirectToGate([]*commonpb.NodeInfo{stale, fresh}, true, testRedirectSecret, testRedirectZoneID, 1, now)

	require.NoError(t, err)
	payload := &commonpb.GateTokenPayload{}
	require.NoError(t, proto.Unmarshal(info.GetTokenPayload(), payload))
	assert.Equal(t, uint32(3), payload.GetGateNodeId(), "票据必须绑定仍在该地址上服务的新 gate")
	assert.Equal(t, "127.0.0.1", info.GetTargetGateIp())
	assert.Equal(t, uint32(30000), info.GetTargetGatePort())
}

func TestSignRedirectToGate_FailsWhenNoGateHasRequiredClientEndpoint(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	nodes := []*commonpb.NodeInfo{
		testGateNode(1, 100, 0, testEndpoint("10.0.0.1", 18000), nil),
		testGateNode(2, 100, 0, testEndpoint("10.0.0.2", 18000), testEndpoint("127.0.0.1", 0)),
	}

	info, err := signRedirectToGate(nodes, true, testRedirectSecret, testRedirectZoneID, 1, now)

	require.Error(t, err, "require=true 且没有任何可下发地址时必须失败,不得回落 PodIP")
	assert.Nil(t, info)
}

func TestSignRedirectToGate_FailsWithoutGates(t *testing.T) {
	info, err := signRedirectToGate(nil, false, testRedirectSecret, testRedirectZoneID, 1, time.Unix(1_800_000_000, 0))

	require.Error(t, err)
	assert.Nil(t, info)
}

// testGateKV 造一条 etcd gate 前缀下的键值。键只进诊断日志,被测的是值。
func testGateKV(nodeID uint32, value string) *mvccpb.KeyValue {
	key := fmt.Sprintf("%s/zone/%d/node_type/%d/%d", gateEtcdPrefix, testRedirectZoneID,
		uint32(commonpb.ENodeType_GateNodeService), nodeID)
	return &mvccpb.KeyValue{Key: []byte(key), Value: []byte(value)}
}

// newerGateNodeInfoJSON 是新版 gate 写进 etcd 的 NodeInfo:带 clientEndpoint,
// 外加一个本进程不认识的虚构字段(模拟滚动窗口里比本进程更新的 gate)。
const newerGateNodeInfoJSON = `{
	"nodeId": 7,
	"launchTime": "200",
	"zoneId": 2,
	"playerCount": 5,
	"endpoint": {"ip": "10.0.0.7", "port": 18000},
	"clientEndpoint": {"ip": "gate-0.z2.example.com", "port": 30100},
	"fieldFromFutureGate": {"anything": [1, 2, 3]}
}`

func TestDecodeGateNodes_NewerGateWithUnknownFieldReachesClientAddress(t *testing.T) {
	// fetchGateNodes 读完 etcd 只调 decodeGateNodes 解析(D77):值里带本进程不认识的字段时,
	// 必须照常解析并一路走到客户端地址,而不是整条丢弃、让跨 zone 重定向无 gate 可选。
	nodes := decodeGateNodes([]*mvccpb.KeyValue{testGateKV(7, newerGateNodeInfoJSON)})

	require.Len(t, nodes, 1, "带未知字段的合法值必须保留")
	targets := selectGateTargets(nodes, true)
	require.Len(t, targets, 1)
	assert.Equal(t, uint32(7), targets[0].node.GetNodeId())
	assert.Equal(t, "gate-0.z2.example.com", targets[0].host)
	assert.Equal(t, uint32(30100), targets[0].port)
}

func TestDecodeGateNodes_SkipsMalformedAndEndpointlessValues(t *testing.T) {
	kvs := []*mvccpb.KeyValue{
		// JSON 语法错误(缺右花括号):宽松解析只放过未知字段,不放过坏 JSON。
		testGateKV(8, `{"nodeId": 8, "zoneId": 2, "endpoint": {"ip": "10.0.0.8", "port": 18000}`),
		testGateKV(7, newerGateNodeInfoJSON),
		// 没有 endpoint:哪怕自报了 clientEndpoint,也不是可路由的 gate 注册记录。
		testGateKV(9, `{"nodeId": 9, "zoneId": 2, "clientEndpoint": {"ip": "gate-2.z2.example.com", "port": 30102}}`),
	}

	nodes := decodeGateNodes(kvs)

	require.Len(t, nodes, 1, "格式错误与缺 endpoint 的值逐条跳过,不连累同批的合法值")
	assert.Equal(t, uint32(7), nodes[0].GetNodeId())
}

package svc

import (
	"fmt"
	"testing"

	"client_rpc_router/generated/pb/game"
	"client_rpc_router/internal/config"
	"client_rpc_router/internal/discovery"

	base "proto/common/base"

	"shared/releasetrack"

	"github.com/stretchr/testify/require"
)

// 目标类型只来自客户端协议 service,且 Battle 被排除(D33);内部服务(DataService 等)不在其中。
func TestTargetNodeTypesFromGeneratedRouteTable(t *testing.T) {
	types := TargetNodeTypes(game.RouteTable)
	set := make(map[base.ENodeType]struct{}, len(types))
	for _, nodeType := range types {
		set[nodeType] = struct{}{}
	}
	for _, want := range []base.ENodeType{
		base.ENodeType_LoginNodeService,
		base.ENodeType_MatchNodeService,
		base.ENodeType_ChatNodeService,
	} {
		_, ok := set[want]
		require.True(t, ok, "路由表里有客户端协议方法指向 %s,必须建 watch", want)
	}
	for _, reject := range []base.ENodeType{
		base.ENodeType_BattleNodeService,
		base.ENodeType_DataServiceNodeService,
		base.ENodeType_EtcdNodeService,
		base.ENodeType_ClientRpcRouterNodeService,
	} {
		_, ok := set[reject]
		require.False(t, ok, "%s 不该成为转发目标", reject)
	}
	for i := 1; i < len(types); i++ {
		require.Less(t, types[i-1], types[i], "结果必须按枚举值升序且去重")
	}
}

func TestTargetNodeTypesIgnoresNonClientAndBattle(t *testing.T) {
	table := map[uint32]game.RouteEntry{
		1: {NodeType: base.ENodeType_LoginNodeService, ClientProtocol: true},
		2: {NodeType: base.ENodeType_LoginNodeService, ClientProtocol: true},
		3: {NodeType: base.ENodeType_BattleNodeService, ClientProtocol: true},
		4: {NodeType: base.ENodeType_FriendNodeService, ClientProtocol: false},
	}
	require.Equal(t, []base.ENodeType{base.ENodeType_LoginNodeService}, TargetNodeTypes(table))
}

// Build 不碰 etcd:每个目标类型一个 watcher、zone 集合按配置、拨号函数有默认值。
func TestBuildCreatesWatcherPerTargetType(t *testing.T) {
	c := config.Config{ForwardTimeoutMs: 1234}
	sc, err := Build(c, nil)
	require.NoError(t, err)
	require.Nil(t, sc.Etcd)
	require.NotNil(t, sc.Dial)
	require.Equal(t, int64(1234)*1e6, sc.ForwardTimeout.Nanoseconds())

	want := TargetNodeTypes(game.RouteTable)
	require.Len(t, sc.Targets, len(want))
	for _, nodeType := range want {
		w, ok := sc.Targets[nodeType]
		require.True(t, ok)
		require.Equal(t, base.ENodeType_name[int32(nodeType)], w.Name())
	}
	require.True(t, sc.IsZoneScoped(base.ENodeType_LoginNodeService))
	require.False(t, sc.IsZoneScoped(base.ENodeType_MatchNodeService))
}

func TestBuildRejectsInvalidConfig(t *testing.T) {
	_, err := Build(config.Config{ForwardTimeoutMs: 0}, nil)
	require.Error(t, err)
	_, err = Build(config.Config{ForwardTimeoutMs: 100, ZoneScopedNodeTypes: []string{"Nope"}}, nil)
	require.Error(t, err)
}

// 转发期限必须短于空闲回收时长:否则一次慢转发正用着的连接可能被当成空闲关掉。
func TestBuildRejectsForwardTimeoutNotShorterThanIdleTimeout(t *testing.T) {
	idleMs := discovery.IdleConnTimeout.Milliseconds()
	_, err := Build(config.Config{ForwardTimeoutMs: idleMs}, nil)
	require.Error(t, err)
	_, err = Build(config.Config{ForwardTimeoutMs: idleMs - 1}, nil)
	require.NoError(t, err)
}

// pickedGlobal 按 stable 会话挑 picks 次,返回挑到过的 endpoint 集合。
func pickedGlobal(t *testing.T, w *discovery.NodeWatcher, picks int) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	for i := 0; i < picks; i++ {
		entry, ok := w.PickRandomForTrack(releasetrack.Stable)
		require.True(t, ok)
		seen[entry.Endpoint] = true
	}
	return seen
}

// 注册之后:全服池目标只在本路由服那段弧上挑,zone-scoped 目标不受影响;同轨道路由服变了就按新名次重算,
// 别的轨道的路由服不影响名次。
func TestSubsetIdentityNarrowsGlobalTargetsAndFollowsPeers(t *testing.T) {
	sc, err := Build(config.Config{ForwardTimeoutMs: 5000, GlobalTargetSubsetSize: 2}, nil)
	require.NoError(t, err)
	require.Equal(t, "ClientRpcRouterNodeService", sc.Peers.Name())
	match := sc.Targets[base.ENodeType_MatchNodeService]
	login := sc.Targets[base.ENodeType_LoginNodeService]
	for i := 0; i < 8; i++ {
		uuid := fmt.Sprintf("t%d", i)
		match.Upsert("match/"+uuid, discovery.NodeEntry{NodeId: uint32(i + 1), ZoneId: 1, NodeUuid: uuid, Endpoint: "match-" + uuid})
		login.Upsert("login/"+uuid, discovery.NodeEntry{NodeId: uint32(i + 1), ZoneId: 1, NodeUuid: uuid, Endpoint: "login-" + uuid})
	}
	for _, uuid := range []string{"r-a", "r-b", "r-c", "r-d"} {
		sc.Peers.Upsert("router/"+uuid, discovery.NodeEntry{NodeUuid: uuid, Endpoint: "router-" + uuid})
	}
	// canary 路由服 r-aa 排在 r-a 与 r-b 之间;算进 stable 的名次的话,下面的期望都会错开一格。
	sc.Peers.Upsert("router/r-aa", discovery.NodeEntry{NodeUuid: "r-aa", Endpoint: "router-r-aa", Track: releasetrack.Canary})

	require.Len(t, pickedGlobal(t, match, 800), 8, "注册之前还不知道名次,在全部实例里挑")

	// 同轨道 4 台、本路由服 r-b 排第 1、k = 2:m = round(2×4/8) = 1,弧是第 [8, 16) 格,每台目标占 4 格 → t2、t3。
	sc.SetSubsetIdentity("r-b", releasetrack.Stable)
	require.Equal(t, map[string]bool{"match-t2": true, "match-t3": true}, pickedGlobal(t, match, 400))

	loginSeen := map[string]bool{}
	for i := 0; i < 800; i++ {
		entry, ok := login.PickRandomInZone(1)
		require.True(t, ok)
		loginSeen[entry.Endpoint] = true
	}
	require.Len(t, loginSeen, 8, "zone-scoped 目标按 zone 挑,不取子集")

	// 新路由服 r-0 上线并排到最前:r-b 变成 5 台里的第 2 名,m = round(2×5/8) = 1,
	// 弧是第 [16, 24) 格,每台目标占 5 格 → t3、t4。
	sc.Peers.Upsert("router/r-0", discovery.NodeEntry{NodeUuid: "r-0", Endpoint: "router-r-0"})
	require.Equal(t, map[string]bool{"match-t3": true, "match-t4": true}, pickedGlobal(t, match, 400))
}

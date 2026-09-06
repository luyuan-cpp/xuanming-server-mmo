package svc

import (
	"testing"

	"client_rpc_router/generated/pb/game"
	"client_rpc_router/internal/config"

	base "proto/common/base"

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

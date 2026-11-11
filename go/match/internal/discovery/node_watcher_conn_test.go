package discovery

// 节点上线时丢掉同一 endpoint 上停在 TRANSIENT_FAILURE 的缓存连接(dropStaleEndpointConn,2026-11-11);正常的连接不动。
// 用本机回环端口与进程内 gRPC 服务端,不起 etcd。只断言连接状态,不断言耗时;等状态的期限只是防挂死。

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

// closedLoopbackEndpoint 返回一个刚关掉的本机端口:拨它会被拒连,缓存连接会停在 TRANSIENT_FAILURE。
func closedLoopbackEndpoint(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	endpoint := lis.Addr().String()
	require.NoError(t, lis.Close())
	return endpoint
}

// servingLoopbackEndpoint 起一个空的进程内 gRPC 服务端,返回它的地址;用例结束时停掉。
func servingLoopbackEndpoint(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	return lis.Addr().String()
}

// waitForConnState 让连接开始建连并等它进入 want;30 秒内没到就失败(Windows 回环拒连要经过 SYN 重试,约 2 秒)。
func waitForConnState(t *testing.T, conn *grpc.ClientConn, want connectivity.State) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn.Connect()
	for {
		state := conn.GetState()
		if state == want {
			return
		}
		require.True(t, conn.WaitForStateChange(ctx, state), "连接 %s 没进入 %s,最后状态 %s", conn.Target(), want, state)
	}
}

// 修复前:新节点在旧地址上线(镜像里是新条目,不触发 onRemove),DialEndpoint 仍返回那条停在 TRANSIENT_FAILURE 的旧连接,
// 不带 WaitForReady 的调用当场 Unavailable,直到旧连接的退避到期。
func TestNodeOnlineDropsTransientFailureConnToSameEndpoint(t *testing.T) {
	cases := []struct {
		name   string
		online func(w *NodeWatcher, endpoint string)
	}{
		{"新 key 上线", func(w *NodeWatcher, endpoint string) {
			w.Upsert(testBattlePrefix+"7", NodeEntry{NodeId: 7, Endpoint: endpoint})
		}},
		{"同 key 换到这个 endpoint", func(w *NodeWatcher, endpoint string) {
			w.Upsert(testBattlePrefix+"8", NodeEntry{NodeId: 8, Endpoint: "127.0.0.1:1"})
			w.Upsert(testBattlePrefix+"8", NodeEntry{NodeId: 8, Endpoint: endpoint})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := closedLoopbackEndpoint(t)
			t.Cleanup(func() { RemoveEndpointConn(endpoint) })
			stale, err := DialEndpoint(endpoint)
			require.NoError(t, err)
			waitForConnState(t, stale, connectivity.TransientFailure)

			tc.online(NewNodeWatcher("battle", testBattlePrefix, nil, nil), endpoint)

			require.Equal(t, connectivity.Shutdown, stale.GetState(), "停在 TRANSIENT_FAILURE 的旧连接要关掉")
			fresh, err := DialEndpoint(endpoint)
			require.NoError(t, err)
			require.NotSame(t, stale, fresh, "节点上线后要新建连接,不能再拿旧连接")
		})
	}
}

// 正常的连接不动:同一进程换 key 重新登记时,旧 key 还在镜像里,这条连接上可能有在途调用。
func TestNodeOnlineKeepsHealthyConnToSameEndpoint(t *testing.T) {
	endpoint := servingLoopbackEndpoint(t)
	t.Cleanup(func() { RemoveEndpointConn(endpoint) })
	live, err := DialEndpoint(endpoint)
	require.NoError(t, err)
	waitForConnState(t, live, connectivity.Ready)

	w := NewNodeWatcher("battle", testBattlePrefix, nil, nil)
	w.Upsert(testBattlePrefix+"9", NodeEntry{NodeId: 9, Endpoint: endpoint})

	require.Equal(t, connectivity.Ready, live.GetState(), "READY 的连接不能被关掉")
	again, err := DialEndpoint(endpoint)
	require.NoError(t, err)
	require.Same(t, live, again, "READY 的连接要继续复用")
}

// 节点只是重复登记(同 key、同 endpoint,例如准入状态变化的 PUT)不算上线,停在 TRANSIENT_FAILURE 的连接也不动:
// 只在新上线时换连接,不因每次 PUT 都重建。
func TestNodeReputSameEndpointLeavesCachedConnAlone(t *testing.T) {
	endpoint := closedLoopbackEndpoint(t)
	t.Cleanup(func() { RemoveEndpointConn(endpoint) })
	w := NewNodeWatcher("battle", testBattlePrefix, nil, nil)
	w.Upsert(testBattlePrefix+"10", NodeEntry{NodeId: 10, Endpoint: endpoint})
	stale, err := DialEndpoint(endpoint)
	require.NoError(t, err)
	waitForConnState(t, stale, connectivity.TransientFailure)

	w.Upsert(testBattlePrefix+"10", NodeEntry{NodeId: 10, Endpoint: endpoint})

	again, err := DialEndpoint(endpoint)
	require.NoError(t, err)
	require.Same(t, stale, again, "同 key 同 endpoint 的重复登记不换连接")
}

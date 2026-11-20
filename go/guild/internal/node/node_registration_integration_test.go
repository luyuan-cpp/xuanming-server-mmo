//go:build integration

package node

// 集成测试:需要本地 etcd (127.0.0.1:2379) 在跑。
// 启动 etcd: pwsh tools/scripts/dev_tools.ps1 -Command etcd-up
// 跑测试:    go test -tags=integration ./internal/node/...
//
// node_id 分配、续租、重注册、注销的不变量由 shared/noderegistry 自己的集成测试覆盖;这里只核对 guild 的字段填法。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/encoding/protojson"

	"guild/internal/config"
	base "proto/common/base"
	sharedregistry "shared/noderegistry"
)

const testEtcdEndpoint = "127.0.0.1:2379"

// 测试用 node_type 落在 ENodeType_name 之外,不会向真实 GuildNodeService 路由池注入测试地址。
const testNodeType uint32 = 1000001

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

// 直接读取 NewNode 发布的注册值，防止进程已监听、路由服却因缺少 grpcEndpoint 忽略节点。
func TestNewNodePublishesGRPCEndpoint(t *testing.T) {
	client := newTestClient(t)
	t.Cleanup(func() { _ = client.Close() })

	previous := config.AppConfig
	config.AppConfig.Node = config.NodeConf{ZoneId: 1, LeaseTTL: 30}
	config.AppConfig.Registry.Etcd = config.EtcdConf{
		Hosts:       []string{testEtcdEndpoint},
		DialTimeout: 3 * time.Second,
	}
	t.Cleanup(func() { config.AppConfig = previous })

	n, err := NewNode(testNodeType, "127.0.0.1", 50300)
	if err != nil {
		t.Fatalf("注册测试节点: %v", err)
	}
	t.Cleanup(func() {
		if err := n.Close(); err != nil {
			t.Errorf("清理测试节点: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	key := sharedregistry.RpcPath(rpcPrefix(n.Info.NodeType), n.Info.ZoneId, n.Info.NodeType, n.Info.NodeId)
	response, err := client.Get(ctx, key)
	if err != nil {
		t.Fatalf("读取测试节点注册值: %v", err)
	}
	if len(response.Kvs) != 1 {
		t.Fatalf("注册 key 数量 = %d，期望 1", len(response.Kvs))
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(response.Kvs[0].Value, &fields); err != nil {
		t.Fatalf("解析注册 JSON: %v", err)
	}
	if _, ok := fields["grpcEndpoint"]; !ok {
		t.Fatal("注册 JSON 缺少 grpcEndpoint，client_rpc_router 会忽略该节点")
	}

	var published base.NodeInfo
	if err := protojson.Unmarshal(response.Kvs[0].Value, &published); err != nil {
		t.Fatalf("解析已发布 NodeInfo: %v", err)
	}
	endpoint := published.GetGrpcEndpoint()
	if endpoint.GetIp() != "127.0.0.1" || endpoint.GetPort() != 50300 {
		t.Fatalf("已发布 gRPC 地址 = %s:%d，期望 127.0.0.1:50300", endpoint.GetIp(), endpoint.GetPort())
	}
	if published.GetProtocolType() != uint32(base.ENodeProtocolType_PROTOCOL_GRPC) {
		t.Fatalf("已发布协议 = %d，期望 gRPC", published.GetProtocolType())
	}
	if published.GetNodeUuid() != n.Info.NodeUuid || published.GetNodeId() != n.Info.NodeId {
		t.Fatalf("已发布身份 = (%d, %q)，期望 Info 回填的 (%d, %q)",
			published.GetNodeId(), published.GetNodeUuid(), n.Info.NodeId, n.Info.NodeUuid)
	}
}

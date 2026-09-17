//go:build integration

package node

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"guild/internal/config"
	base "proto/common/base"
)

// 直接读取 NewNode 发布的注册值，防止进程已监听、路由服却因缺少 grpcEndpoint 忽略节点。
// 使用独立的测试 node_type，避免向真实 GuildNodeService 路由池注入测试地址。
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

	n, err := NewNode(uniqueNodeType(), "127.0.0.1", 50300)
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
	key := rpcPath(rpcPrefix(n.Info.NodeType), n.Info.ZoneId, n.Info.NodeType, n.Info.NodeId)
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
}

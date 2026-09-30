package node

import (
	"context"
	"fmt"
	"login/internal/config"
	login_proto "proto/common/base"
	"shared/nodeinfo"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/namespace"
)

// NodeWatcher watches node events via etcd.
type NodeWatcher struct {
	client *clientv3.Client
	prefix string
}

// NewNodeWatcher creates a new NodeWatcher instance.
func NewNodeWatcher(client *clientv3.Client, prefix string) *NodeWatcher {
	return &NodeWatcher{client: client, prefix: prefix}
}

// FetchAllNodes retrieves all registered node info under the configured prefix.
func (nw *NodeWatcher) FetchAllNodes() ([]*login_proto.NodeInfo, error) {
	kv := namespace.NewKV(nw.client, nw.prefix)

	ctx, cancel := context.WithTimeout(context.Background(), config.AppConfig.Timeouts.ServiceDiscoveryTimeout)
	defer cancel()

	resp, err := kv.Get(ctx, "", clientv3.WithPrefix())
	if err != nil {
		logx.Errorf("Failed to get keys with prefix %s: %v", nw.prefix, err)
		return nil, fmt.Errorf("failed to fetch nodes: %w", err)
	}

	nodes := make([]*login_proto.NodeInfo, 0, len(resp.Kvs))
	for _, item := range resp.Kvs {
		nodeInfo := &login_proto.NodeInfo{}
		// 宽松解析(忽略未知字段,D77):新版本 gate 写入的新字段不能让整台 gate 从候选里消失。
		if err := nodeinfo.Unmarshal(item.Value, nodeInfo); err != nil {
			logx.Errorf("Invalid NodeInfo JSON for key=%s: %v", string(item.Key), err)
			continue
		}
		nodes = append(nodes, nodeInfo)
	}

	return nodes, nil
}

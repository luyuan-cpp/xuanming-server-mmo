package svc

// 停写标记发布循环一拍的逻辑(gate_stale_publisher.go):只给镜像判为停写的 gate 写标记;读镜像失败时什么都不写。
// 写入本身(过期时间、续写、Redis 失败)的用例在 loginqueue/gatestale_test.go。

import (
	"context"
	"errors"
	"testing"

	"login/internal/logic/pkg/node"

	"shared/gatepick"
)

func TestPublishGateStaleMarks_WritesOnlyStaleGates(t *testing.T) {
	rdb, mr := newSvcTestRedis(t)
	healthy := gateNode(4, 1, 100, "10.244.0.4", 7, clientAddr("127.0.0.1", 30000))
	hung := gateNode(9, 1, 200, "10.244.0.9", 3, clientAddr("127.0.0.1", 30001))
	nodes := func(context.Context) ([]node.WatchedNode, error) {
		return []node.WatchedNode{{Info: healthy}, {Info: hung, Stale: true}, {Stale: true}}, nil // 最后一条 Info 为空,跳过
	}

	if err := publishGateStaleMarks(context.Background(), nodes, rdb, svcTestZone); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if !mr.Exists(gatepick.StaleKey(svcTestZone, 9)) {
		t.Fatal("镜像判为停写的 gate 9 应有停写标记")
	}
	if mr.Exists(gatepick.StaleKey(svcTestZone, 4)) {
		t.Fatal("健康的 gate 4 不得有停写标记")
	}
	if got := mr.TTL(gatepick.StaleKey(svcTestZone, 9)); got != gatepick.StaleMarkTTL {
		t.Fatalf("停写标记的过期时间 = %s,want %s", got, gatepick.StaleMarkTTL)
	}
}

// 读镜像失败(镜像与回落直读都出错)时返回错误、什么都不写:读者退回只靠自己的历史,不会凭空多出标记。
func TestPublishGateStaleMarks_NodesErrorWritesNothing(t *testing.T) {
	rdb, mr := newSvcTestRedis(t)
	nodes := func(context.Context) ([]node.WatchedNode, error) { return nil, errors.New("etcd unavailable") }

	if err := publishGateStaleMarks(context.Background(), nodes, rdb, svcTestZone); err == nil {
		t.Fatal("读镜像失败应返回错误,由循环记日志")
	}
	if keys := mr.Keys(); len(keys) != 0 {
		t.Fatalf("读镜像失败时不得写任何键,got %v", keys)
	}
}

// 都不停写(含回落直读、Stale 恒为 false 的一拍)时一个字节都不写。
func TestPublishGateStaleMarks_NoStaleGatesWritesNothing(t *testing.T) {
	rdb, mr := newSvcTestRedis(t)
	nodes := func(context.Context) ([]node.WatchedNode, error) {
		return watched(gateNode(4, 1, 100, "10.244.0.4", 7, nil), gateNode(5, 1, 100, "10.244.0.5", 7, nil)), nil
	}

	if err := publishGateStaleMarks(context.Background(), nodes, rdb, svcTestZone); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if keys := mr.Keys(); len(keys) != 0 {
		t.Fatalf("没有停写的 gate 时不得写任何键,got %v", keys)
	}
}

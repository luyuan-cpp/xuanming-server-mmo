package svc

import (
	"context"
	"errors"
	"testing"

	"github.com/zeromicro/go-zero/zrpc"

	"shared/idsegment"
)

// NewGuildIDMinter 收到 nil *Client(号段关闭)时不能把 typed-nil 塞进 Source 接口,
// 否则 Minter 会去调 nil 客户端的 Next。必须干净地失败。
func TestNewGuildIDMinter_NilClientFailsClosed(t *testing.T) {
	m := NewGuildIDMinter(nil)
	id, err := m.Mint(context.Background())
	if !errors.Is(err, idsegment.ErrNoIDSource) || id != 0 {
		t.Fatalf("id=%d err=%v, want ErrNoIDSource", id, err)
	}
}

func TestHasRpcTarget(t *testing.T) {
	if hasRpcTarget(zrpc.RpcClientConf{}) {
		t.Fatal("empty conf must not count as a target")
	}
	c := zrpc.RpcClientConf{}
	c.Etcd.Key = "dataservice.rpc"
	if !hasRpcTarget(c) {
		t.Fatal("etcd key is a target")
	}
	if !hasRpcTarget(zrpc.RpcClientConf{Endpoints: []string{"127.0.0.1:9000"}}) {
		t.Fatal("endpoints are a target")
	}
	if !hasRpcTarget(zrpc.RpcClientConf{Target: "dns:///data-service:9000"}) {
		t.Fatal("target is a target")
	}
}

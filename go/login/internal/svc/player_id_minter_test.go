package svc

import (
	"testing"

	"github.com/zeromicro/go-zero/zrpc"
)

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

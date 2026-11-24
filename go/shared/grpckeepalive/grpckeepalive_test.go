package grpckeepalive

import (
	"testing"
	"time"
)

// 对端都是缺省强制策略的 grpc-go 服务端(MinTime 5 分钟、没有在途调用时不收 ping):参数越过这条线,
// 对端回 too_many_pings 踢断连接(infra-reconnect-overview.md R16),比不开 keepalive 还糟。
func TestParamsStayWithinServerDefaultEnforcement(t *testing.T) {
	p := params()
	if p.Time < 5*time.Minute || p.PermitWithoutStream {
		t.Fatalf("keepalive %+v would be rejected by a grpc-go server with the default enforcement policy", p)
	}
}

func TestServerParamsDefaultsAndOverrides(t *testing.T) {
	d := serverParams(0, 0)
	if d.MaxConnectionAge != DefaultMaxConnectionAge || d.MaxConnectionAgeGrace != DefaultMaxConnectionAgeGrace {
		t.Fatalf("unexpected defaults: %+v", d)
	}
	c := serverParams(10*time.Minute, 45*time.Second)
	if c.MaxConnectionAge != 10*time.Minute || c.MaxConnectionAgeGrace != 45*time.Second {
		t.Fatalf("unexpected overrides: %+v", c)
	}
	if ServerOption(0, 0) == nil {
		t.Fatal("ServerOption must not be nil")
	}
}

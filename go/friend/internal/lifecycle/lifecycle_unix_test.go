//go:build linux || darwin || freebsd

package lifecycle

import (
	"testing"

	"github.com/zeromicro/go-zero/zrpc"
)

// Linux 上 go-zero 默认收到信号 1s 后自行 GracefulStop;Configure 必须把自动停机与强杀都推迟到硬截止,
// 否则注销慢于 1s 时会先停服、后注销(契约 §2)。
func TestConfigurePostponesFrameworkAutoStop(t *testing.T) {
	var c zrpc.RpcServerConf

	Configure(&c)

	if c.Shutdown.WrapUpTime != HardTimeout || c.Shutdown.WaitTime != HardTimeout {
		t.Fatalf("Shutdown = %+v,期望 WrapUpTime = WaitTime = %v", c.Shutdown, HardTimeout)
	}
}

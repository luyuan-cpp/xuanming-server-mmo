//go:build linux || darwin || freebsd

package shutdownbudget

import (
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"
)

// Apply 把 c.Shutdown.WaitTime 放宽到能盖住最长在途请求。必须在 zrpc.NewServer / MustNewServer 之前调用:
// go-zero 在那里把 c.Shutdown 交给 proc.Setup。不需要放宽时不改配置、不打日志。
func Apply(c *zrpc.RpcServerConf) {
	longest := LongestRequestTimeout(*c)
	current := WaitTimeFor(c.Shutdown.WrapUpTime, c.Shutdown.WaitTime, 0)
	wait := WaitTimeFor(c.Shutdown.WrapUpTime, c.Shutdown.WaitTime, longest)
	if wait <= current {
		return
	}
	c.Shutdown.WaitTime = wait
	logx.Infof("[shutdown] force-quit wait time raised from %v to %v to cover the longest request timeout %v",
		current, wait, longest)
}

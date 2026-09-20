//go:build linux || darwin || freebsd

package lifecycle

import (
	"github.com/zeromicro/go-zero/core/proc"
	"github.com/zeromicro/go-zero/zrpc"
)

// Configure 必须在 zrpc.NewServer 之前调用。
//
// go-zero 没有禁用自动信号停机的公开 API。将其自动 shutdown 和强杀都放到本进程
// 明示的硬截止,正常预算内由 Shutdown 串行收尾;不使用全局 signal.Reset。
// wrap-up listener 仍会在信号时开始,当前 friend 没有在该阶段关闭业务句柄的 listener。
func Configure(c *zrpc.RpcServerConf) {
	c.Shutdown.WrapUpTime = HardTimeout
	c.Shutdown.WaitTime = HardTimeout
	proc.Setup(c.Shutdown)
}

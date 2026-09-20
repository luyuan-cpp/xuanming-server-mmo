//go:build windows

package lifecycle

import "github.com/zeromicro/go-zero/zrpc"

// Configure 在 Windows 上是空操作:proc.ShutdownConf 是空结构,go-zero 也不接管信号;
// 使用同一个 Shutdown 主动排空。
func Configure(_ *zrpc.RpcServerConf) {}

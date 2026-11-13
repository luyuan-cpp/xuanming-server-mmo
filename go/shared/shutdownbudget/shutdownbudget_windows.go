//go:build windows

package shutdownbudget

import "github.com/zeromicro/go-zero/zrpc"

// Apply 在 Windows 上是空操作:proc.ShutdownConf 是空结构,go-zero 也不接管信号(只在本机开发时跑)。
func Apply(_ *zrpc.RpcServerConf) {}

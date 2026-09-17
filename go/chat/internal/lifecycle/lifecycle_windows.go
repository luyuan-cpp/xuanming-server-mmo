//go:build windows

package lifecycle

import "github.com/zeromicro/go-zero/zrpc"

// Windows 的 proc.ShutdownConf 是空结构,也不接管信号;使用同一Shutdown主动排空。
func Configure(_ *zrpc.RpcServerConf) {}

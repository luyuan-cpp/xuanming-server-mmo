// Package shutdownbudget 让 go-zero 的停机强退时限盖住在途请求(docs/design/scene-node-planned-drain.md §9「Go 服务停机时限」)。
//
// go-zero 收到 SIGTERM 之后分三步:
//  1. 立刻跑 wrap-up 监听器(从 etcd 注销,调用方不再把新请求发过来);
//  2. 等 WrapUpTime,再跑 shutdown 监听器(gRPC GracefulStop:不接新请求,等在途请求结束);
//  3. 从收到信号起满 WaitTime,不管还有没有在途请求都强杀进程。
//
// 缺省 WrapUpTime 1 秒、WaitTime 5.5 秒,在途请求只有 4.5 秒。服务端超时(RpcServerConf.Timeout / MethodTimeouts)配得比这长的
// 服务,计划内重启会把还在限时内的请求砍掉:调用方拿到的是 Unavailable,而不是这次请求自己的结果。
//
// 规则只有一条,收在这里:WaitTime ≥ WrapUpTime + 最长服务端超时 + 余量。只放宽、不收紧;服务自己配了更长的 WaitTime 就用它的。
// Pod 的宽限期要再盖住它:terminationGracePeriodSeconds ≥ preStop + WaitTime + 余量。
//
// 自己接管信号、有明确硬截止的服务(friend / trade / chat / player_inbox 的 lifecycle,mail / rank 的 domainhost,db、data_service)
// 不用这个包:它们的时限由各自的停机流程定。
package shutdownbudget

import (
	"time"

	"github.com/zeromicro/go-zero/zrpc"
)

const (
	// DefaultWrapUpTime、DefaultWaitTime 是 go-zero proc 的缺省值(core/proc/shutdown.go)。配置里没写 Shutdown 块时
	// RpcServerConf.Shutdown 是零值,go-zero 按这两个数执行,这里按同样的数推算。
	DefaultWrapUpTime = time.Second
	DefaultWaitTime   = 5500 * time.Millisecond
	// Slack 是 GracefulStop 返回到进程退出之间的收尾余量(main 的 defer、日志冲刷)。
	Slack = time.Second
)

// LongestRequestTimeout 返回服务端对单个一元请求的最长限时:Timeout(毫秒)与各 MethodTimeouts 里最大的那个。
//
// Timeout <= 0 时 go-zero 不装超时拦截器(MethodTimeouts 也随之不生效),请求没有上界,返回 0:WaitTime 盖不住没有上界的
// 请求,保持原值。
func LongestRequestTimeout(c zrpc.RpcServerConf) time.Duration {
	if c.Timeout <= 0 {
		return 0
	}
	longest := time.Duration(c.Timeout) * time.Millisecond
	for _, m := range c.MethodTimeouts {
		if m.Timeout > longest {
			longest = m.Timeout
		}
	}
	return longest
}

// WaitTimeFor 返回 WaitTime 应取的值,纯函数。wrapUp、wait 是配置里的值,<= 0 表示没配(按 go-zero 缺省值算);
// longestRequest <= 0 表示请求没有上界,不调整。结果不小于当前生效的 wait。
func WaitTimeFor(wrapUp, wait, longestRequest time.Duration) time.Duration {
	if wrapUp <= 0 {
		wrapUp = DefaultWrapUpTime
	}
	if wait <= 0 {
		wait = DefaultWaitTime
	}
	if longestRequest <= 0 {
		return wait
	}
	if need := wrapUp + longestRequest + Slack; need > wait {
		return need
	}
	return wait
}

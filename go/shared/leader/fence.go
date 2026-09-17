package leader

import (
	"math"
	"time"
)

// UnboundedCall 表示单次调用可能无限期阻塞:客户端既不认 ctx 截止时间,
// socket 读写也没有超时。作为 FenceAfter 的 maxCall 传入时只会得到 guaranteed=false。
const UnboundedCall = time.Duration(math.MaxInt64)

// CallBudget 把续期 ctx 超时与客户端 socket 读写上界合成单次续期调用的最坏耗时,作为 FenceAfter 的 maxCall。
//
//   - clientBound == 0:客户端认 ctx 截止时间(socket 读写截止取 ctx),整次调用由 ctxTimeout 封顶;
//   - clientBound == UnboundedCall 或 < 0:socket 读写可能无限期阻塞,返回 UnboundedCall;
//   - 否则取两者之和,不是较大者:不认 ctx 的 go-redis 客户端在写 socket 之前的连接池排队、重试退避、
//     拨号都受 ctx 截止时间约束,可能先耗掉最多 ctxTimeout;真正的 socket 写与读只受读写超时约束
//     (clientBound)。每次重试都要先过退避与取连接这两道 ctx 检查,ctx 过期后不会再发起新一轮,
//     所以截止时间之后最多只剩在途的那一轮读写。
//
// 未覆盖的缺口:新建连接时的握手(HELLO / AUTH 等)各自还有一轮 socket 读写,同样不认 ctx,不在其内。
func CallBudget(ctxTimeout, clientBound time.Duration) time.Duration {
	if clientBound == 0 {
		return ctxTimeout
	}
	if clientBound == UnboundedCall || clientBound < 0 {
		return UnboundedCall
	}
	return ctxTimeout + clientBound
}

// FenceAfter 计算锁心跳的自我降级门槛:续期报错且距上次成功续期达到 fenceAfter 即降级。
// shared/leader 与 login pkg/locker 共用这一个公式。
//
// 前提:
//   - lastOK 记最后一次成功续期请求的**发出**时刻(PEXPIRE 不早于它执行,key 最早在 lastOK+ttl 过期),
//     初值是 SETNX 的发出时刻;
//   - 续期按 lastOK 排拍:lastOK 之后第 n 个出错拍定在 lastOK+n·interval(单调时钟),已过则立即发出。
//     不能按心跳起动时刻起 ticker —— 心跳总晚于 SETNX 起动(SETNX 往返、同步当选回调),每拍都会晚这段
//     起动延迟,第 1 个出错拍快速失败逃过门槛后,第 2 个出错拍卡满 maxCall 就会拖过 key 过期;
//   - maxCall 是单次续期调用从发出到返回的最坏耗时(≥ 0,见 CallBudget);
//   - 心跳起动延迟 d(SETNX 发出到心跳 goroutine 起动)满足 d+maxCall < ttl。
//
// 定时器不会提前触发,所以失联后第 n 个出错拍不早于 lastOK+n·interval 发出、正常最晚
// lastOK+n·interval+maxCall 返回并做判定。maxCall < interval 且 2·interval+maxCall < ttl 时
// guaranteed=true,门槛取 2·interval:
//
//	(a) 第 1 个出错拍最晚在 lastOK+interval+maxCall < lastOK+2·interval 返回,够不到门槛,一次抖动不误降级;
//	(b) 第 2 个出错拍定在 lastOK+2·interval,返回时 since ≥ 2·interval,必定降级;
//	(c) 这次降级最晚在 lastOK+2·interval+maxCall < lastOK+ttl 完成,赶在 key 过期之前。
//
// 门槛不取 (interval+maxCall, 2·interval] 的中点:(b) 靠排拍保证 since ≥ 2·interval,低于 2·interval 的
// 余量帮不了第 2 拍,只会削减第 1 拍的容忍度。第 1 个出错拍晚触发(调度停顿)的容忍度是 interval−maxCall;
// 晚触发同样吃 (c) 的余量 ttl−2·interval−maxCall。例:ttl 30s、interval 10s、maxCall 8s → 20s,
// 降级最晚在 28s 完成。
//
// 心跳起动延迟 d > interval 时首拍立即发出:返回时 since 已 ≥ 2·interval 就当拍降级,最晚 d+maxCall < ttl
// 完成;否则第 2 拍仍定在 lastOK+2·interval,按 (b)(c) 降级。都早于 key 过期。
//
// 满足不了时返回 (0, false):任一续期报错都降级(fail-closed)。门槛不取 interval —— 它正压在
// 节拍周期上,是第 1 拍还是第 2 拍降级就取决于毫秒级的触发抖动。此时一次抖动就会让位,但慢调用
// 仍可能拖过 key 过期,调用方应记错误日志并调大 TTL(> 3·maxCall 且留余量)。
func FenceAfter(ttl, interval, maxCall time.Duration) (fenceAfter time.Duration, guaranteed bool) {
	// 先比 maxCall < interval:UnboundedCall 不参与下面的加法,不会溢出。
	guaranteed = maxCall < interval && 2*interval+maxCall < ttl
	if !guaranteed {
		return 0, false
	}
	return 2 * interval, true
}

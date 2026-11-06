package pkg

// gate 应用层心跳的客户端一侧(C++ 仓 docs/design/gate-client-heartbeat.md §3、§9)。
//
// 契约:验签成功、gate 在回包里通告了非 0 的 heartbeat_interval_ms 之后,立即发第一条 ClientHeartbeatRequest,
// 之后每个间隔一条,不看别的流量。gate 对发过心跳的连接按读空闲 heartbeat_timeout_ms 判死并强关,随后照常走断线保留。
// 没有通告(旧 gate、Java 版尚未实现)就一条都不发:旧 gate 不认识心跳类型,发了会被当非法帧强关。
//
// 只往「验签成功的那条连接」发:跟随 gate 重定向时 SwapConn 先换上一条还没验签的新连接(redirect.go),
// 新连接验签之前发心跳会被 gate 当成验签前的心跳断开。重定向到不通告心跳的 gate 时停发。
//
// gate 的心跳回包由 RecvOne / RecvLoop 认领计数(takeHeartbeatReply),不交给业务分发。
//
// 不做的事:客户端一侧不判 gate 已死。vendored muduo 断线后静默重拨、Recv 不报错,这里拿不到可靠的断线信号,
// robot 的会话失败仍靠各请求自己的超时(设计 §12 残余)。
//
// 一个 GameClient 至多一个心跳 goroutine:第一次验签成功且 gate 通告了心跳时起,Close 时停;Close 之后再验签也不重起。

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/luyuancpp/muduoclient/muduo"

	"proto/common/base"
)

// heartbeat 是 GameClient 的心跳状态。mu 保护除 replies 之外的全部字段。
type heartbeat struct {
	mu sync.Mutex
	// conn 是最近一次验签成功、且 gate 通告了心跳的连接,只往它发。nil = 当前 gate 不支持心跳,或已 Close。
	conn *muduo.Client
	// interval / timeout 是 gate 最近一次通告的值。重定向到不支持心跳的 gate 后 interval 保留,只作心跳 goroutine 的节拍。
	interval time.Duration
	timeout  time.Duration
	seq      uint64
	paused   bool
	running  bool          // 心跳 goroutine 已起
	closed   bool          // Close 之后为真,终态
	stop     chan struct{} // 心跳 goroutine 的退出信号,Close 时关闭

	replies atomic.Uint64 // 认领到的心跳回包数,含重定向之前的连接
}

// onGateVerified 在 VerifyGateToken 于连接 c 上验签成功后调用(调用 VerifyGateToken 的 goroutine)。
func (gc *GameClient) onGateVerified(c *muduo.Client, resp *base.ClientTokenVerifyResponse) {
	interval := time.Duration(resp.GetHeartbeatIntervalMs()) * time.Millisecond
	timeout := time.Duration(resp.GetHeartbeatTimeoutMs()) * time.Millisecond
	hb := &gc.hb
	hb.mu.Lock()
	if hb.closed {
		hb.mu.Unlock()
		return
	}
	if interval <= 0 {
		// 这台 gate 不认心跳:不往这条连接发。已起的心跳 goroutine 照常按旧节拍空转,换到支持心跳的 gate 后自动恢复。
		hb.conn, hb.timeout = nil, 0
		hb.mu.Unlock()
		return
	}
	hb.conn, hb.interval, hb.timeout = c, interval, timeout
	var stop chan struct{}
	if !hb.running {
		hb.running = true
		hb.stop = make(chan struct{})
		stop = hb.stop
	}
	hb.mu.Unlock()

	// 验签成功后立即发第一条,不等满一个间隔:gate 从收到第一条心跳起才对这条连接按空闲超时判死。
	gc.heartbeatTick()
	if stop != nil {
		go gc.heartbeatLoop(stop)
	}
}

// heartbeatLoop 每个间隔调一次 heartbeatTick,直到 stop 关闭(Close)。间隔每轮重读:重定向后的 gate 可能通告别的值。
func (gc *GameClient) heartbeatLoop(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-gc.heartbeatWait(gc.heartbeatPeriod()):
		}
		gc.heartbeatTick()
	}
}

// heartbeatTick 往验签过的当前连接发一条心跳。已 Close、已暂停、gate 不支持心跳、当前连接不是验签过的那条
// (重定向途中,新连接还没验签)时不发。发送在锁外:连接快照取自锁内,之后 SwapConn 换掉连接也只是发进旧连接。
func (gc *GameClient) heartbeatTick() {
	c := gc.conn()
	hb := &gc.hb
	hb.mu.Lock()
	if hb.closed || hb.paused || hb.conn == nil || hb.conn != c {
		hb.mu.Unlock()
		return
	}
	hb.seq++
	request := &base.ClientHeartbeatRequest{Seq: hb.seq}
	hb.mu.Unlock()
	gc.sendHeartbeat(c, request)
}

func (gc *GameClient) heartbeatPeriod() time.Duration {
	gc.hb.mu.Lock()
	defer gc.hb.mu.Unlock()
	return gc.hb.interval
}

func (gc *GameClient) heartbeatWait(d time.Duration) <-chan time.Time {
	if gc.heartbeatAfter != nil {
		return gc.heartbeatAfter(d)
	}
	return time.After(d)
}

func (gc *GameClient) sendHeartbeat(c *muduo.Client, request *base.ClientHeartbeatRequest) {
	if gc.heartbeatSend != nil {
		gc.heartbeatSend(c, request)
		return
	}
	c.Send(request)
}

// stopHeartbeat 由 Close 调用:停掉心跳 goroutine,之后不再发,也不再起。幂等。
func (gc *GameClient) stopHeartbeat() {
	hb := &gc.hb
	hb.mu.Lock()
	defer hb.mu.Unlock()
	if hb.closed {
		return
	}
	hb.closed = true
	hb.conn = nil
	if hb.stop != nil {
		close(hb.stop)
	}
}

// takeHeartbeatReply 认领 gate 的心跳回包:是就计数并返回 true,调用方跳过这条消息,不交给业务分发。
func (gc *GameClient) takeHeartbeatReply(msg any) bool {
	if _, ok := msg.(*base.ClientHeartbeatResponse); !ok {
		return false
	}
	gc.hb.replies.Add(1)
	return true
}

// PauseHeartbeat 停发心跳,不再恢复。冒烟用它模拟「客户端静默」:连接仍开着、一帧都不发,
// gate 应在 heartbeat_timeout_ms 之后关掉它(heartbeat_idle_smoke_scenario.go)。
func (gc *GameClient) PauseHeartbeat() {
	gc.hb.mu.Lock()
	defer gc.hb.mu.Unlock()
	gc.hb.paused = true
}

// HeartbeatInterval 返回当前连接的 gate 通告的心跳间隔;0 = 不支持心跳(或还没验签、已 Close)。
func (gc *GameClient) HeartbeatInterval() time.Duration {
	gc.hb.mu.Lock()
	defer gc.hb.mu.Unlock()
	if gc.hb.conn == nil {
		return 0
	}
	return gc.hb.interval
}

// HeartbeatTimeout 返回当前连接的 gate 通告的判死阈值;0 的含义同 HeartbeatInterval。
func (gc *GameClient) HeartbeatTimeout() time.Duration {
	gc.hb.mu.Lock()
	defer gc.hb.mu.Unlock()
	if gc.hb.conn == nil {
		return 0
	}
	return gc.hb.timeout
}

// HeartbeatReplies 返回收到的心跳回包数(含重定向之前的连接)。
func (gc *GameClient) HeartbeatReplies() uint64 {
	return gc.hb.replies.Load()
}

package callerauth

import (
	"sync"
	"time"
)

// nonceSet 是防重放的第二道闸:时间窗内同一个 nonce 只允许用一次。
//
// # 为什么用「两代 map 轮转」而不是「每条带过期时间 + 定期扫」
//
// 带时间戳的方案要么定期全表扫(O(n) 抖动),要么在插入时顺带清理(清理量
// 和插入量耦合,峰值最需要低延迟的时候扫得最狠)。两代轮转的代价是常数:
// 每过 window 把 cur 降级成 prev、cur 换新 map,旧 map 整个交给 GC。
// 查询时查两代,插入只进 cur。
//
// # 保留时长
//
// 一条 nonce 至少活 window、至多活 2×window。调用方把 window 设成
// 2×MaxClockSkew,就能保证「任何一条在时间窗内被接受过的签名」在它还可能
// 被接受的整个期间里都留在表内 —— 这是防重放正确性的关键不变量。
//
// # 过载时的降级方向
//
// 表大小超过 maxEntries 会强制提前翻代:重放窗口临时缩短,但**绝不会**
// 放行一条验签失败的请求。反过来做(超限就拒绝)会把内存压力变成登录不可用,
// 那是更糟的失败模式。翻代由调用方通过 onOverflow 打日志暴露出来。
type nonceSet struct {
	mu         sync.Mutex
	window     time.Duration
	maxEntries int
	onOverflow func()

	cur      map[string]struct{}
	prev     map[string]struct{}
	rotateAt time.Time
}

func newNonceSet(window time.Duration, maxEntries int, onOverflow func()) *nonceSet {
	if window <= 0 {
		window = time.Minute
	}
	if maxEntries <= 0 {
		maxEntries = 500000
	}
	return &nonceSet{
		window:     window,
		maxEntries: maxEntries,
		onOverflow: onOverflow,
		cur:        make(map[string]struct{}),
		prev:       make(map[string]struct{}),
		rotateAt:   time.Now().Add(window),
	}
}

// admit 记下一个 nonce。返回 false 表示这个 nonce 在窗口内已经出现过(重放)。
func (n *nonceSet) admit(nonce string, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.rotateLocked(now)

	if _, ok := n.cur[nonce]; ok {
		return false
	}
	if _, ok := n.prev[nonce]; ok {
		return false
	}
	n.cur[nonce] = struct{}{}

	// 超限强制翻代。放在插入之后:本次的 nonce 一定留在新的一代里,
	// 不会出现「刚记完就被丢掉」。
	if len(n.cur) > n.maxEntries {
		n.rotateNowLocked(now)
		if n.onOverflow != nil {
			n.onOverflow()
		}
	}
	return true
}

// rotateLocked 按时间推进代际。
// 停机很久后重新有流量时,cur/prev 可能都已经彻底过期 —— 这里用
// 「超过两个 window 就两代全清」把它一次收干净,免得留下陈旧 nonce
// 白占内存(留着不影响正确性,只是浪费)。
func (n *nonceSet) rotateLocked(now time.Time) {
	if !now.After(n.rotateAt) {
		return
	}
	if now.After(n.rotateAt.Add(n.window)) {
		n.prev = make(map[string]struct{})
		n.cur = make(map[string]struct{})
		n.rotateAt = now.Add(n.window)
		return
	}
	n.rotateNowLocked(now)
}

func (n *nonceSet) rotateNowLocked(now time.Time) {
	n.prev = n.cur
	n.cur = make(map[string]struct{})
	n.rotateAt = now.Add(n.window)
}

// size 只给测试用,返回两代合计条数。
func (n *nonceSet) size() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.cur) + len(n.prev)
}

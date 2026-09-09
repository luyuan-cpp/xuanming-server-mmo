package snowflake

import (
	"errors"
	"sync"
	"time"
)

// ErrWatermarkStale 表示距上次**水位写成功**已经超过自 fence 期限 F,发号器暂停发号。
//
// 与 ErrFenced 的区别:ErrFenced 是永久的(所有权已确认丢失);本错误是**暂态**的 ——
// 它只说明"此刻无法证明自己仍持有槽位"。原因通常是 etcd 不可达、或进程被冻结 /
// VM 挂起后刚解冻。等下一次水位写成功(写入本身带槽位归属校验)即自动恢复,
// 不需要重启进程。若槽位真被别人拿走,分配器会另行关闭 Lost() 并永久 fence。
//
// 为什么暂态而不是永久:F(2h)内 etcd 抖动不该让进程变成"活着但永远发不了号"的僵尸,
// 那等于把 etcd 重新变回强依赖。协议上的安全性由 T_ack + F < wm + Q 保证
// (设计稿 §1.2),与本错误是否可恢复无关。
//
// 调用方与 ErrFenced 同样处理:这次操作整体失败,不得用 0 或自造 id 继续。
var ErrWatermarkStale = errors.New(
	"snowflake: watermark ack too old (self-fence); cannot prove slot ownership, refusing to mint")

// FenceClock 是"按水位年龄自 fence"的共享判定器。
//
// 设计稿 §3.4:持有者维护 lastAck = 最近一次水位 Put 成功时的**单调时钟**读数 + 同刻墙钟。
// Generate() 先判 `mono_now − lastAckMono > F` **或** `wall_now − lastAckWall > F`,
// 任一触发即拒发。两个时钟都看是因为它们各挡一类故障:
//   - 单调钟:墙钟被 NTP 回拨时仍准确;
//   - 墙钟:VM 挂起 / 容器冻结时**单调钟停走**(CLOCK_MONOTONIC 不含挂起时间),
//     解冻后单调钟看起来"才过了一瞬",只有墙钟知道真实流逝了多久。
//
// 一个 FenceClock 由 snowflakealloc.Handle 持有并 Ack,同时被 shared/snowflake.Node
// 与 login 的 PlayerIDGen(bwmarrin 包装)共用,两种发号器执行同一条规则。
//
// 从未 Ack 过的 FenceClock 视为**已过期**(fail-closed):分配器在交出发号器前必须
// 同步写一次水位;本地缓存启动路径用 SeedAck 把上次成功时刻灌进来。
type FenceClock struct {
	budget  time.Duration
	monoNow func() time.Duration // 自任意固定原点起的单调流逝
	wallNow func() time.Time

	mu      sync.Mutex
	acked   bool
	ackMono time.Duration
	ackWall time.Time
}

// NewFenceClock 用真实时钟构造。budget 即 F。
func NewFenceClock(budget time.Duration) *FenceClock {
	origin := time.Now() // 带单调读数;time.Since 只看单调部分
	return NewFenceClockWithClocks(budget,
		func() time.Duration { return time.Since(origin) },
		time.Now)
}

// NewFenceClockWithClocks 允许注入两个时钟,给测试用(生产路径请用 NewFenceClock)。
// monoNow 返回的是"自某固定原点起的流逝量",wallNow 返回墙钟。
func NewFenceClockWithClocks(budget time.Duration, monoNow func() time.Duration, wallNow func() time.Time) *FenceClock {
	if budget <= 0 {
		panic("snowflake: FenceClock budget must be positive")
	}
	return &FenceClock{budget: budget, monoNow: monoNow, wallNow: wallNow}
}

// Budget 返回 F。
func (c *FenceClock) Budget() time.Duration { return c.budget }

// Ack 记录"此刻水位写成功"。只应在 **etcd 确认写入成功** 后调用。
func (c *FenceClock) Ack() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acked = true
	c.ackMono = c.monoNow()
	c.ackWall = c.wallNow()
}

// SeedAck 用一个**过去的墙钟时刻**灌入上次 Ack(本地缓存启动路径:etcd 不通,
// 但缓存说 lastAckWall 在 F 之内)。单调读数按"墙钟差"回推,于是两条规则都从
// 那一刻起算,不会因为进程刚起、单调钟原点在现在而白送 F 的余量。
func (c *FenceClock) SeedAck(wall time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elapsed := c.wallNow().Sub(wall)
	if elapsed < 0 {
		elapsed = 0
	}
	c.acked = true
	c.ackWall = wall
	c.ackMono = c.monoNow() - elapsed
}

// Stale 判定是否已超过 F。返回的 age 是两个时钟口径下较大的那个年龄;
// 从未 Ack 时 stale=true、age=0。
func (c *FenceClock) Stale() (stale bool, age time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.acked {
		return true, 0
	}
	monoAge := c.monoNow() - c.ackMono
	wallAge := c.wallNow().Sub(c.ackWall)
	age = monoAge
	if wallAge > age {
		age = wallAge
	}
	return monoAge > c.budget || wallAge > c.budget, age
}

// LastAckWall 返回上次 Ack 的墙钟时刻(零值 = 从未 Ack)。供本地缓存落盘。
func (c *FenceClock) LastAckWall() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.acked {
		return time.Time{}
	}
	return c.ackWall
}

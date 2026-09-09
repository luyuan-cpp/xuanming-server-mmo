// Package idsegment 是永久身份(player_id / guild_id / item guid)的号段发号客户端
// (docs/design/node-id-overhaul-plan-20260908.md §6,美团 Leaf-segment 模式)。
//
// # 为什么不用 snowflake
//
// 永久身份只需要「全局唯一 + 不重发」,不需要把铸号时间和铸号节点编进 id 里。
// snowflake 的唯一性建立在 worker id 租约 + 时钟单调之上,冻结 / 重启 / 时钟回拨
// 每一样都要专门的 fence 机制去堵;号段模式没有 lease、没有时钟、没有 worker:
// 持有者用一次 CAS 从表里领走 [lo, hi),协调器(data_service → MySQL)只在续段时碰一次。
//
// # 双 buffer
//
// Client 同时持有当前段和一个预取的下一段。当前段剩余 ≤ PrefetchAt·段长 时后台预取
// (单飞:同一时刻最多一个在途 RPC;上次领段失败后的退避期内也不发);当前段用完而
// 下一段还没到,Next 阻塞在在途的那次 RPC 上(受 ctx 截止时间约束),RPC 失败或超时即
// 返回 ErrSegmentUnavailable。于是库不可用时每个持有者还能发完手里两段 —— 这就是
// §6.5 说的「弱依赖」。
//
// # 动态 step(Leaf 口径,设计稿 §7.5 第 4 条)
//
// Options.Step 只是**初始**步长。每段记录「装为当前段 → 发完最后一个号」的墙钟时长:
// 不到 15 分钟就用完 → 下次领段翻倍(≤ MaxStep);超过 30 分钟才用完 → 减半(≥ MinStep);
// 之间不动。目的只有一个:进程每天重启一次时浪费 ≤ 2×step,把空洞压到最小。这不是
// 正确性需求 —— 唯一性由服务端 CAS 保证,step 怎么变都不会重号。
//
// # 服务端范围校验(fail-closed)
//
// 每个从服务端拿到的范围都要过四道检查:lo ≥ 1、hi > lo、hi ≤ MaxIDExclusive、
// lo ≥ 上一段的 hi(同一客户端拿到的范围必须单调递增)。任何一条不满足就是
// ErrRangeViolation:这段**绝不使用**并大声记日志 —— 服务端把号发重了比发不出号
// 严重得多(player_database 的写路径是 INSERT ... ON DUPLICATE KEY UPDATE,撞号不报错
// 而是静默串档)。
//
// # 与存量 snowflake 号不撞:精确边界
//
// 号段从 1 起、上限 2^55 ≈ 3.6e16(MaxIDExclusive 默认,服务端 CHECK 同样守着)。
// 存量 snowflake 号从哪一刻起落到 2^55 之上,由各自的布局与 epoch 决定:
//
//   - player_id(bwmarrin,login.yaml Snowflake.Epoch = 2024-07-20 11:01:03 UTC,
//     毫秒时间 << 22):时间字段 ≥ 2^33 ms(≈ 99.4 天)后 id ≥ 2^55,即自
//     2024-10-27 21:06 UTC 起铸的号全部 ≥ 2^55(今天 ≈ 2.8e17)。在那之前铸的号
//     (如有)落在 [2^22, 2^55) 内。
//   - guild_id 及其他 17/15 布局(shared/snowflake,epoch 2026-03-14 00:00 UTC,
//     秒 << 32):时间字段 ≥ 2^23 s(≈ 97.1 天)后 id ≥ 2^55,即自 2026-06-19 02:10 UTC
//     起铸的号全部 ≥ 2^55(今天 ≈ 6.7e16)。dev / staging 在那之前铸的行落在
//     [2^32, 2^55) 内。
//
// 边界之前铸的号虽在 [1, 2^55) 里,却都在 2^22 ≈ 4.2e6 / 2^32 ≈ 4.3e9 以上:从 1 起、
// 步长 ≤ MaxStep 的计数器要先发出这么多个号才够得着,按建角 / 建帮的速率是遥不可及的。
// 但这是「够不着」不是「不相交」,所以上限 2^55 **保持不动、不擅自下调**:要不要把
// MaxIDExclusive 收紧到 2^22 / 2^32 以下由项目负责人拍板,本包只把边界写清楚。
package idsegment

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"shared/safego"
)

// AllocateFunc 是领段 RPC 的抽象:返回半开区间 [lo, hi)。RPC 层通过它注入,
// 单测用一个计数器就能当假服务端(见 Adapt 与 client_test.go)。
type AllocateFunc func(ctx context.Context, bizTag string, step uint32) (lo, hi uint64, err error)

// Logger 是本包需要的最小日志面。nil 用 go-zero logx。
type Logger interface {
	Errorf(format string, args ...any)
	Infof(format string, args ...any)
}

// DefaultMaxIDExclusive 是号段值域的上界(不含):2^55,与 id_segment 表的 CHECK 一致。
// 与存量 snowflake 号的精确边界见包注释「与存量 snowflake 号不撞」;不要在这里下调。
const DefaultMaxIDExclusive = uint64(1) << 55

const (
	// DefaultMinStep / DefaultMaxStep 是动态 step 的默认上下限 [10, 1000]。
	// player / guild 的建角 / 建帮速率都很小,上限压在 1000 是为了让每天重启一次的
	// 浪费(≤ 2×step)可以忽略;item 这种大流量 biz 要显式给更大的界。
	DefaultMinStep uint32 = 10
	DefaultMaxStep uint32 = 1000

	// stepGrowBelow / stepShrinkAbove 是 Leaf 的两个门限:一段作为当前段存活不到
	// 15 分钟 → 翻倍;超过 30 分钟 → 减半;[15, 30] 分钟不动。两者之间留一倍的滞回,
	// 否则 step 会在门限附近来回抖。
	stepGrowBelow   = 15 * time.Minute
	stepShrinkAbove = 30 * time.Minute
)

var (
	// ErrInvalidOptions 表示 Options 自相矛盾(空 BizTag、Step=0……),启动前就该发现。
	ErrInvalidOptions = errors.New("idsegment: invalid options")
	// ErrSegmentUnavailable 表示手里两段都用完了、而续段在 ctx 截止前没有成功。
	// 调用方必须让本次操作整体失败(或按自己的策略回退),**不得**自造 id。
	ErrSegmentUnavailable = errors.New("idsegment: segment unavailable")
	// ErrRangeViolation 表示服务端返回的范围不合法(见包注释)。这段一定不会被使用。
	// Next 返回的错误同时 errors.Is ErrSegmentUnavailable 与 ErrRangeViolation。
	ErrRangeViolation = errors.New("idsegment: server returned an invalid range")
	// ErrClosed 表示 Close 之后又调了 Next。
	ErrClosed = errors.New("idsegment: client closed")
)

// Options 控制 Client 行为。零值字段取默认。
type Options struct {
	// BizTag 是号段表里的业务键("player" / "guild" / "item")。必填。
	BizTag string
	// Step 是**初始**领段长度。必填(>0)。之后每段按消耗时长在 [MinStep, MaxStep] 内
	// 自适应(见包注释「动态 step」);按「10 分钟峰值发号量」给初值即可,设计稿 §6.2。
	Step uint32
	// MinStep / MaxStep 是动态 step 的下 / 上限。0 取 DefaultMinStep / DefaultMaxStep;
	// 默认值夹不住 Step 时收成 Step 本身,让显式给的 Step 永远合法。显式给值必须满足
	// MinStep ≤ Step ≤ MaxStep,否则 ErrInvalidOptions —— 配错了要在起服时炸,不能静默改。
	MinStep uint32
	MaxStep uint32
	// PrefetchAt 是触发预取的剩余比例:当前段剩余 ≤ PrefetchAt·段长 时后台领下一段。
	// 默认 0.1;必须在 (0, 1] 内。按段的实际长度算,因为动态 step 下每段长度不同。
	PrefetchAt float64
	// MaxIDExclusive 是允许的最大 id(不含)。默认 2^55。只能调小不能调大 ——
	// 调大就会与存量 snowflake 号的值域相交(精确边界见包注释)。
	MaxIDExclusive uint64
	// RetryBackoff 是一次领段失败后、下一次领段前的最短间隔,防止把已经倒下的
	// 服务端打成雪崩。预取与阻塞续段两条路径都受它约束。默认 500ms。
	RetryBackoff time.Duration
	// FetchTimeout 是单次领段 RPC 的超时。后台预取没有调用方 ctx,必须自带上限。
	// 默认 3s。
	FetchTimeout time.Duration
	// Logger 为 nil 时用 logx。
	Logger Logger
}

// Stats 是 Client 的观测快照。
type Stats struct {
	BizTag string
	// Step 是**当前**动态步长(下一次领段会带给服务端的值);MinStep / MaxStep 是它的界。
	Step    uint32
	MinStep uint32
	MaxStep uint32
	// Issued 是已发出的 id 数。
	Issued uint64
	// Fetches / FetchErrors 是成功 / 失败的领段次数(失败含范围校验不过)。
	Fetches     uint64
	FetchErrors uint64
	// RangeViolations 是被拒绝的服务端范围数。正常应恒 0,非 0 = 服务端有 bug。
	RangeViolations uint64
	// Unavailable 是返回了 ErrSegmentUnavailable 的 Next 次数。
	Unavailable uint64
	// CurrentRemaining 是当前段剩余的 id 数;NextReady 表示下一段已预取到手。
	CurrentRemaining uint64
	NextReady        bool
	// HighWater 是至今校验通过的最大 hi。后续范围的 lo 必须 ≥ 它。
	HighWater uint64
}

// segment 是一个已领到的半开区间,pos 是下一个要发出的 id。
type segment struct {
	pos uint64
	hi  uint64
	// prefetchAt 是这一段触发预取的剩余数(≥1),按段的实际长度算 —— 动态 step 下
	// 每段长度不同,阈值不能再挂在 Options.Step 上。
	prefetchAt uint64
}

func (s *segment) remaining() uint64 {
	if s.pos >= s.hi {
		return 0
	}
	return s.hi - s.pos
}

// fetchState 是一次在途领段。done 在 RPC 结束(成功 / 失败 / 超时)时关闭,
// 之后 err 才可读。等待者靠它区分「这次续段失败了」与「成功但被别人先用光了」。
type fetchState struct {
	done chan struct{}
	err  error
}

// Client 是一个 biz_tag 的号段客户端。并发安全;一个进程一个 biz_tag 建一个即可。
type Client struct {
	rpc AllocateFunc
	opt Options
	log Logger
	// now 是墙钟,只为让单测拨表(动态 step 的门限是分钟级,真等不起)。
	// 退避判断也走它 —— 用假钟的测试要么不制造领段失败,要么显式拨过 RetryBackoff。
	now func() time.Time

	mu sync.Mutex
	// cur 是当前段,next 是预取到的下一段(nil = 还没有)。
	cur  segment
	next *segment
	// step 是下一次领段要带给服务端的动态步长,见 onRangeExhaustedLocked。
	step uint32
	// curInstalledAt 是 cur 被装为当前段的时刻;零值 = 当前段没有在计时
	// (还没领到第一段,或已经结算过)。
	curInstalledAt time.Time
	// inflight 非 nil = 有一次领段在途(单飞)。
	inflight *fetchState
	// notBefore 是上次失败后允许再次领段的最早时刻。
	notBefore time.Time
	// highWater 是校验通过的最大 hi。
	highWater uint64
	closed    bool

	// bg 是后台领段的父 ctx,Close 时取消,让在途 RPC 立刻返回。
	bg     context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// 计数(mu 保护)。fetchesStarted 含在途与失败的,单测用它做「没有多发一次」的
	// 确定性断言,不必 sleep 等 goroutine。
	issued, fetches, fetchErrors, fetchesStarted, rangeViolations, unavailableN uint64
}

// New 构造客户端。不做任何 IO:第一段在首次 Next(或 Warm)时才去领。
func New(rpc AllocateFunc, opt Options) (*Client, error) {
	if rpc == nil {
		return nil, fmt.Errorf("%w: nil AllocateFunc", ErrInvalidOptions)
	}
	if opt.BizTag == "" {
		return nil, fmt.Errorf("%w: empty BizTag", ErrInvalidOptions)
	}
	if opt.Step == 0 {
		return nil, fmt.Errorf("%w: Step must be > 0", ErrInvalidOptions)
	}
	if opt.MinStep == 0 {
		opt.MinStep = min(DefaultMinStep, opt.Step)
	}
	if opt.MaxStep == 0 {
		opt.MaxStep = max(DefaultMaxStep, opt.Step)
	}
	if opt.MinStep > opt.Step || opt.Step > opt.MaxStep {
		return nil, fmt.Errorf("%w: need MinStep(%d) <= Step(%d) <= MaxStep(%d)",
			ErrInvalidOptions, opt.MinStep, opt.Step, opt.MaxStep)
	}
	if opt.PrefetchAt == 0 {
		opt.PrefetchAt = 0.1
	}
	if opt.PrefetchAt < 0 || opt.PrefetchAt > 1 || math.IsNaN(opt.PrefetchAt) {
		return nil, fmt.Errorf("%w: PrefetchAt=%v must be within (0, 1]", ErrInvalidOptions, opt.PrefetchAt)
	}
	if opt.MaxIDExclusive == 0 {
		opt.MaxIDExclusive = DefaultMaxIDExclusive
	}
	if opt.MaxIDExclusive > DefaultMaxIDExclusive {
		// 调大就与 snowflake 值域相交,见包注释。
		return nil, fmt.Errorf("%w: MaxIDExclusive=%d exceeds 2^55", ErrInvalidOptions, opt.MaxIDExclusive)
	}
	if opt.RetryBackoff <= 0 {
		opt.RetryBackoff = 500 * time.Millisecond
	}
	if opt.FetchTimeout <= 0 {
		opt.FetchTimeout = 3 * time.Second
	}
	if opt.Logger == nil {
		opt.Logger = logxLogger{}
	}
	bg, cancel := context.WithCancel(context.Background())
	return &Client{
		rpc:    rpc,
		opt:    opt,
		log:    opt.Logger,
		now:    time.Now,
		step:   opt.Step,
		bg:     bg,
		cancel: cancel,
	}, nil
}

// Next 发一个 id。当前段有号立即返回;当前段用完则切到预取段;两段都空时阻塞在
// 在途的续段上,直到续段结束或 ctx 到期,失败返回 ErrSegmentUnavailable。
//
// 一次 Next 最多等**一次**续段结果(加上退避间隔):续段失败就立刻报
// ErrSegmentUnavailable,而不是抱着调用方的 ctx 一直重试 —— 建角 / 建帮的调用方
// 宁可 3 秒内明确失败,也不要在库倒下时挂到 gRPC 超时。
func (c *Client) Next(ctx context.Context) (uint64, error) {
	c.mu.Lock()
	for {
		if c.closed {
			c.mu.Unlock()
			return 0, ErrClosed
		}
		if c.cur.remaining() > 0 {
			id := c.cur.pos
			c.cur.pos++
			c.issued++
			if c.cur.remaining() == 0 {
				// 先结算 step 再决定预取:若预取恰好在这一刻才发起,它就该带新 step。
				c.onRangeExhaustedLocked()
			}
			// 剩余触底就预取;有在途 / 已有下一段 / 退避期内则不发。
			if c.cur.remaining() <= c.cur.prefetchAt && c.canStartFetchLocked() {
				c.startFetchLocked()
			}
			c.mu.Unlock()
			return id, nil
		}
		if c.next != nil {
			c.cur = *c.next
			c.next = nil
			// 从这一刻起计这一段的存活时长;它躺在 next 里等待的时间不算。
			c.curInstalledAt = c.now()
			continue
		}
		// 两段都空。
		if c.inflight == nil {
			if !c.canStartFetchLocked() {
				// next 与 inflight 在这里都已确认为空,拦住的只可能是退避期:
				// 不持锁睡到 notBefore,再回到循环顶部(期间别人可能已经续上了)。
				wait := c.notBefore.Sub(c.now())
				c.mu.Unlock()
				if err := sleepCtx(ctx, wait); err != nil {
					return 0, c.unavailable(err)
				}
				c.mu.Lock()
				continue
			}
			c.startFetchLocked()
		}
		st := c.inflight
		c.mu.Unlock()

		select {
		case <-st.done:
		case <-ctx.Done():
			return 0, c.unavailable(ctx.Err())
		}
		if st.err != nil {
			return 0, c.unavailable(st.err)
		}
		// 续段成功:回到循环顶部拿号(可能已被别的 goroutine 用光,那就再等一次)。
		c.mu.Lock()
	}
}

// Warm 同步领第一段(受 ctx 约束),供启动时把「配置错 / 服务端不通」尽早暴露出来。
// 失败不影响之后的 Next —— 它们会各自再试。已有号时是空操作。
//
// Warm 是启动时的一次性显式调用,不受 RetryBackoff 约束:它本来就是「现在就去试一次」。
func (c *Client) Warm(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.cur.remaining() > 0 || c.next != nil {
		c.mu.Unlock()
		return nil
	}
	if c.inflight == nil {
		c.startFetchLocked()
	}
	st := c.inflight
	c.mu.Unlock()

	select {
	case <-st.done:
		return st.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close 取消在途领段并等它退出;幂等。之后 Next 一律 ErrClosed。
// 已领到但没发完的号就此作废 —— 号段模式本来就允许浪费,不需要归还。
func (c *Client) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	c.wg.Wait()
}

// Stats 返回观测快照。
func (c *Client) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		BizTag:           c.opt.BizTag,
		Step:             c.step,
		MinStep:          c.opt.MinStep,
		MaxStep:          c.opt.MaxStep,
		Issued:           c.issued,
		Fetches:          c.fetches,
		FetchErrors:      c.fetchErrors,
		RangeViolations:  c.rangeViolations,
		Unavailable:      c.unavailableN,
		CurrentRemaining: c.cur.remaining(),
		NextReady:        c.next != nil,
		HighWater:        c.highWater,
	}
}

// unavailable 记一次拒发并把原因包进 ErrSegmentUnavailable。
func (c *Client) unavailable(cause error) error {
	c.mu.Lock()
	c.unavailableN++
	c.mu.Unlock()
	return fmt.Errorf("%w (biz_tag=%s): %w", ErrSegmentUnavailable, c.opt.BizTag, cause)
}

// canStartFetchLocked 判断此刻能否发起一次领段:没有在途(单飞)、没有已到手的下一段
// (双 buffer 只存一段)、且上次失败的退避期已过。有号时的预取和没号时的阻塞续段都
// 必须过它 —— 预取若绕过退避,服务端倒下期间只要手里还有号,每个 Next 都会再打一次
// RPC,「防雪崩」的退避就成了摆设。
func (c *Client) canStartFetchLocked() bool {
	return c.inflight == nil && c.next == nil && !c.now().Before(c.notBefore)
}

// onRangeExhaustedLocked 在当前段发出最后一个号时按 Leaf 口径调 step(见包注释
// 「动态 step」)。量的是「装为当前段 → 用完」的墙钟时长。只在这里改 step,
// 所以一段的快慢影响的是它之后的**下一次**领段:通常就是下一段进行中的那次预取。
func (c *Client) onRangeExhaustedLocked() {
	if c.curInstalledAt.IsZero() {
		return
	}
	lasted := c.now().Sub(c.curInstalledAt)
	c.curInstalledAt = time.Time{}
	prev := c.step
	switch {
	case lasted < stepGrowBelow:
		// 先在 uint64 里翻倍再夹,MaxStep 允许到 2^32-1,uint32 直接乘会绕回。
		c.step = uint32(min(uint64(c.step)*2, uint64(c.opt.MaxStep)))
	case lasted > stepShrinkAbove:
		c.step = max(c.step/2, c.opt.MinStep)
	}
	if c.step != prev {
		c.log.Infof("[idsegment] biz_tag=%s step %d -> %d (previous range lasted %v as current; bounds [%d, %d])",
			c.opt.BizTag, prev, c.step, lasted, c.opt.MinStep, c.opt.MaxStep)
	}
}

// startFetchLocked 发起一次后台领段。调用方持锁,且已确认没有在途。
// 带出去的 step 在这一刻快照:领段期间 step 可能被 onRangeExhaustedLocked 改掉,
// 但一次 RPC 只能有一个 step。
func (c *Client) startFetchLocked() {
	st := &fetchState{done: make(chan struct{})}
	c.inflight = st
	c.fetchesStarted++
	step := c.step
	c.wg.Add(1)
	safego.Go("idsegment.fetch", func() {
		defer c.wg.Done()
		// finalize 在 defer 里跑:即使 rpc panic(safego 会记点位 + 栈),在途状态也一定
		// 被清掉、等待者一定被唤醒 —— 否则 inflight 永远非 nil,所有 Next 从此挂死。
		var lo, hi uint64
		err := errors.New("idsegment: allocate rpc panicked")
		defer func() { c.finishFetch(st, lo, hi, err) }()
		fctx, cancel := context.WithTimeout(c.bg, c.opt.FetchTimeout)
		defer cancel()
		lo, hi, err = c.rpc(fctx, c.opt.BizTag, step)
	})
}

// finishFetch 校验并落下一段,或记失败;最后唤醒等待者。
func (c *Client) finishFetch(st *fetchState, lo, hi uint64, err error) {
	c.mu.Lock()
	defer func() {
		c.mu.Unlock()
		close(st.done)
	}()
	c.inflight = nil
	if err == nil {
		err = c.validateLocked(lo, hi)
	}
	if err != nil {
		st.err = err
		c.fetchErrors++
		c.notBefore = c.now().Add(c.opt.RetryBackoff)
		if !errors.Is(err, ErrRangeViolation) { // 范围违规已在 validateLocked 里大声记过
			c.log.Errorf("[idsegment] biz_tag=%s allocate failed (current_remaining=%d next_ready=%v): %v",
				c.opt.BizTag, c.cur.remaining(), c.next != nil, err)
		}
		return
	}
	c.next = &segment{pos: lo, hi: hi, prefetchAt: prefetchThreshold(c.opt.PrefetchAt, hi-lo)}
	c.highWater = hi
	c.fetches++
	c.log.Infof("[idsegment] biz_tag=%s got range [%d, %d) (fetches=%d issued=%d step=%d)",
		c.opt.BizTag, lo, hi, c.fetches, c.issued, c.step)
}

// prefetchThreshold 把预取比例换算成「剩余多少个时预取」,至少 1 —— 0 意味着永远不预取,
// 双 buffer 就退化成单 buffer。
func prefetchThreshold(at float64, length uint64) uint64 {
	t := uint64(math.Ceil(at * float64(length)))
	if t < 1 {
		t = 1
	}
	return t
}

// validateLocked 是范围的四道检查(见包注释)。任何一条不过都是 ErrRangeViolation。
func (c *Client) validateLocked(lo, hi uint64) error {
	var reason string
	switch {
	case lo < 1:
		reason = "lo < 1"
	case hi <= lo:
		reason = "hi <= lo"
	case hi > c.opt.MaxIDExclusive:
		reason = fmt.Sprintf("hi > max_id_exclusive(%d)", c.opt.MaxIDExclusive)
	case lo < c.highWater:
		reason = fmt.Sprintf("lo < previous hi(%d): ranges regressed/overlapped", c.highWater)
	default:
		return nil
	}
	c.rangeViolations++
	// 大声:这是服务端 bug 的直接证据,而且离「静默串档」只差一步。
	c.log.Errorf("[idsegment] REJECTED range [%d, %d) for biz_tag=%s: %s — refusing to mint from it "+
		"(server-side bug: an overlapping or out-of-domain range would silently collide with existing ids)",
		lo, hi, c.opt.BizTag, reason)
	return fmt.Errorf("%w: [%d, %d) %s", ErrRangeViolation, lo, hi, reason)
}

// sleepCtx 睡 d,ctx 先到期则返回其错误。
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// logxLogger 把 go-zero logx 适配成 Logger。
type logxLogger struct{}

func (logxLogger) Errorf(format string, args ...any) { logx.Errorf(format, args...) }
func (logxLogger) Infof(format string, args ...any)  { logx.Infof(format, args...) }

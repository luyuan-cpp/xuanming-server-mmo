// Package idissuer 是永久 id 的发号器:AllocateIdSegment 背后的实现。
//
// 设计见 docs/design/player-data-tidb-scaling-plan.md §6.1~§6.3。要点:
//
//   - 从内存切号段。发号器先预留一大块号,记录落盘后,再从这块里切 [lo, hi) 给调用方。
//     一块用完(或用掉一半时后台预取)才碰一次存储,所以没有单行热点和单键争用。
//   - 预留记录写两份,放在两套互相独立的 etcd 里,都只增不减。一块号只有两份都写成功才能发。
//     任何一份丢失或回退,另一份仍然盖得住所有发过的号,下一块自动跳到它之上,不用人工处理。
//   - 块在内存里、从不回读,所以存储回退影响不到手里已有的块。
//   - 时钟只是第三道防线:抬高块的起点,防的是两份记录同时回退。它只能把起点往上抬,所以坏时钟
//     不会造成重号,也不会让发号停下来,最多浪费值域。正常路径不依赖时钟。
//
// 所有种类共用一个计数器,所以系统里任意两个 id 都不相同。biz_tag 只用来校验和记指标。
//
// 计数器结构(63 位):[地域 6][时间 33,秒][序号 24]。客户端不配、不解析,拿到的 [lo, hi)
// 已经包含地域。这个结构一旦开始发正式号就不能改。
package idissuer

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"shared/safego"
)

const (
	regionBits = 6
	timeBits   = 33
	seqBits    = 24

	regionShift = timeBits + seqBits
	valueMask   = uint64(1)<<regionShift - 1

	// MaxRegion 是地域码的上限(含)。
	MaxRegion = uint8(1)<<regionBits - 1

	// DefaultBlockSize 是一块的大小:2^32 个号,约等于 256 秒的序号预算。发号器崩溃时最多作废一块。
	DefaultBlockSize = uint64(1) << 32

	// MaxStep 是单次领段长度上限。拦住把 step 传成 hi 一类的调用错误。
	MaxStep uint32 = 10_000_000

	// DefaultStep 是调用方传 step=0 时用的长度。
	DefaultStep uint32 = 100

	// epochUnix 是时间字段的纪元:2026-01-01 00:00:00 UTC。
	epochUnix = int64(1767225600)

	// maxAheadSeconds:计数器比时钟超前这么多,说明时钟曾经跳到过未来。这不影响唯一性,只是
	// 时间下限在追上来之前不起作用,所以只告警。正常超前量是最近几次预留的块长(每块约 256 秒)。
	maxAheadSeconds = uint64(7 * 24 * 3600)

	// bootstrapMarginSeconds:显式初始化时在当前时间之上再留的余量。两份记录都丢了的时候,
	// 发出去的号可能比时钟超前几个块,留一天盖住它们。
	bootstrapMarginSeconds = uint64(24 * 3600)

	// reserveAttempts 是一次预留遇到 CAS 冲突时的重试上限。冲突只发生在多个副本同时预留时。
	reserveAttempts = 8
	// reserveTimeout 是后台预留的预算。
	reserveTimeout = 10 * time.Second
	// reserveRetryBackoff 是预留失败后,下一次允许预留的最短间隔,防止把已经倒下的 etcd 打成雪崩。
	reserveRetryBackoff = 500 * time.Millisecond
)

var (
	// ErrInvalidTag:biz_tag 不合法。
	ErrInvalidTag = errors.New("id issuer: biz_tag must match ^[a-z0-9_]{1,64}$")
	// ErrUnknownTag:biz_tag 没有登记进 IdSegment.Tags。
	ErrUnknownTag = errors.New("id issuer: biz_tag is not listed in IdSegment.Tags")
	// ErrInvalidStep:step 超过 MaxStep。
	ErrInvalidStep = errors.New("id issuer: step must be within 0..10000000")
	// ErrCounterMissing:两份预留记录都不存在,而且不允许自动初始化。这是 id 安全事件。
	ErrCounterMissing = errors.New("id issuer: both reservation records are missing; " +
		"follow the id issuer runbook in docs/ops/id-issuer-runbook.md, then run data_service -id-issuer-init")
	// ErrRegionMismatch:记录里的地域和本发号器配置的不一致。
	ErrRegionMismatch = errors.New("id issuer: reservation record belongs to another region")
	// ErrExhausted:值域见底,或者时间字段溢出。
	ErrExhausted = errors.New("id issuer: counter space exhausted")
	// ErrClockUnusable:只在显式初始化时用。时钟不在可表示的范围内,算不出初始值。
	ErrClockUnusable = errors.New("id issuer: clock is outside the representable range; fix the clock before init")
	// ErrContended:多个副本同时预留,重试上限内都没抢到。
	ErrContended = errors.New("id issuer: reservation kept conflicting")
	// ErrClosed:发号器已关闭。
	ErrClosed = errors.New("id issuer: closed")
)

var bizTagPattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// IsValidBizTag 给 logic 层做指标 label 消毒:不合法的 tag 不能原样进 Prometheus label。
func IsValidBizTag(tag string) bool {
	return bizTagPattern.MatchString(tag)
}

// Record 是从一份预留记录读到的状态。Revision 是这份记录的 CAS 令牌,调用方不解释它。
type Record struct {
	Value    uint64
	Revision int64
	Exists   bool
}

// CounterStore 是一份预留记录。两份实例必须落在互相独立的存储上。
//
// 契约:
//   - Read 是线性读。值损坏时返回错误,不得当作不存在。
//   - CompareAndSwap 仅当记录仍是 prev 时把值写成 next。false = 记录已被别人改过,没有写入;
//     返回错误 = 结果未知,调用方不得认为写入成功,也不得靠重读「值等于我写的」来认领。
type CounterStore interface {
	Read(ctx context.Context) (Record, error)
	CompareAndSwap(ctx context.Context, prev Record, next uint64) (bool, error)
	Close() error
}

// Options 是发号器的配置。
type Options struct {
	Primary   CounterStore
	Secondary CounterStore
	// Region 是本地域的地域码,不得超过 MaxRegion。
	Region uint8
	// BlockSize 是一块的大小,0 取 DefaultBlockSize。
	BlockSize uint64
	// AllowBootstrap=true:两份记录都不存在时自动初始化(只给 dev)。
	AllowBootstrap bool
	// Tags 是允许的 biz_tag 清单,不能为空。
	Tags []string
	// Now 为 nil 用 time.Now,单测拨表用。
	Now func() time.Time
	// OnReserve 为 nil 不记。result 见 ReserveXxx 常量。
	OnReserve func(result string)
}

// OnReserve 的 result 取值,基数有界。
const (
	ReserveOK           = "ok"
	ReserveConflict     = "conflict"
	ReserveStoreError   = "store_error"
	ReserveRefused      = "refused"
	ReserveOneRecord    = "one_record_missing"
	ReserveRecordsBack  = "record_behind"
	ReserveAheadOfClock = "ahead_of_clock"
)

type block struct {
	pos uint64
	end uint64
	// halfway 之后触发后台预取。
	halfway uint64
}

func (b *block) remaining() uint64 {
	if b.pos >= b.end {
		return 0
	}
	return b.end - b.pos
}

type reservation struct {
	done chan struct{}
	err  error
}

// Issuer 是发号器。并发安全;一个进程一个实例。
type Issuer struct {
	opt   Options
	tags  map[string]struct{}
	limit uint64 // 本地域值域上限(不含)
	base  uint64 // 本地域值域下限

	// monoNow 只用于退避判断,用单调时钟,不受 Options.Now 拨表影响。
	monoNow func() time.Time

	mu        sync.Mutex
	cur       block
	next      *block
	inflight  *reservation
	notBefore time.Time
	closed    bool

	bg     context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New 构造发号器。不做任何 IO:第一块在首次 Allocate 时才预留。
func New(opt Options) (*Issuer, error) {
	if opt.Primary == nil || opt.Secondary == nil {
		return nil, errors.New("id issuer: both reservation records are required")
	}
	if opt.Region > MaxRegion {
		return nil, fmt.Errorf("id issuer: region %d exceeds %d", opt.Region, MaxRegion)
	}
	if opt.BlockSize == 0 {
		opt.BlockSize = DefaultBlockSize
	}
	if opt.BlockSize >= valueMask {
		return nil, fmt.Errorf("id issuer: block size %d too large", opt.BlockSize)
	}
	if opt.BlockSize < uint64(MaxStep) {
		return nil, fmt.Errorf("id issuer: block size %d smaller than MaxStep", opt.BlockSize)
	}
	if len(opt.Tags) == 0 {
		return nil, errors.New("id issuer: Tags is empty")
	}
	tags := make(map[string]struct{}, len(opt.Tags))
	for _, tag := range opt.Tags {
		if !bizTagPattern.MatchString(tag) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidTag, tag)
		}
		tags[tag] = struct{}{}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	base := uint64(opt.Region) << regionShift
	bg, cancel := context.WithCancel(context.Background())
	return &Issuer{
		opt:     opt,
		tags:    tags,
		base:    base,
		limit:   base + valueMask + 1,
		monoNow: time.Now,
		bg:      bg,
		cancel:  cancel,
	}, nil
}

// Allocate 切一段 [lo, hi)。hi-lo 可能小于 step(块尾截短),调用方不得假设相等。
// step=0 取 DefaultStep。
func (i *Issuer) Allocate(ctx context.Context, bizTag string, step uint32) (lo, hi uint64, err error) {
	if !bizTagPattern.MatchString(bizTag) {
		return 0, 0, ErrInvalidTag
	}
	if _, ok := i.tags[bizTag]; !ok {
		return 0, 0, ErrUnknownTag
	}
	if step > MaxStep {
		return 0, 0, ErrInvalidStep
	}
	if step == 0 {
		step = DefaultStep
	}

	i.mu.Lock()
	for {
		if i.closed {
			i.mu.Unlock()
			return 0, 0, ErrClosed
		}
		if i.cur.remaining() > 0 {
			lo = i.cur.pos
			hi = min(lo+uint64(step), i.cur.end)
			i.cur.pos = hi
			if i.cur.pos >= i.cur.halfway && i.next == nil {
				i.startReservationLocked()
			}
			i.mu.Unlock()
			return lo, hi, nil
		}
		if i.next != nil {
			i.cur = *i.next
			i.next = nil
			continue
		}
		// 手里没有号了,等一次预留。一次调用最多等一次结果,失败就明确失败,不抱着 ctx 重试。
		res := i.startReservationLocked()
		if res == nil {
			// 退避期内:上一次预留刚失败,快速失败,调用方有自己的退避和重试。
			i.mu.Unlock()
			return 0, 0, fmt.Errorf("id issuer: no block available, retrying after backoff")
		}
		i.mu.Unlock()
		select {
		case <-res.done:
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		}
		if res.err != nil {
			return 0, 0, res.err
		}
		i.mu.Lock()
	}
}

// Close 停止后台预留。之后 Allocate 返回 ErrClosed。
func (i *Issuer) Close() error {
	i.mu.Lock()
	i.closed = true
	i.mu.Unlock()
	i.cancel()
	i.wg.Wait()
	return errors.Join(i.opt.Primary.Close(), i.opt.Secondary.Close())
}

// Bootstrap 把两份都不存在的记录初始化到当前时间之上。只要有一份存在就什么也不做:
// 只缺一份时,下一次预留会自动补齐。只由部署步骤显式调用,运行路径不走这里。
func (i *Issuer) Bootstrap(ctx context.Context) (bool, error) {
	p, err := i.opt.Primary.Read(ctx)
	if err != nil {
		return false, err
	}
	s, err := i.opt.Secondary.Read(ctx)
	if err != nil {
		return false, err
	}
	if p.Exists || s.Exists {
		return false, nil
	}
	floor := i.timeFloor()
	if floor == 0 {
		return false, ErrClockUnusable
	}
	start := floor + bootstrapMarginSeconds<<seqBits
	if ok, err := i.opt.Primary.CompareAndSwap(ctx, p, start); err != nil || !ok {
		return false, errors.Join(ErrContended, err)
	}
	if ok, err := i.opt.Secondary.CompareAndSwap(ctx, s, start); err != nil || !ok {
		return false, errors.Join(ErrContended, err)
	}
	return true, nil
}

// startReservationLocked 发起后台预留(单飞),返回在途的那次;退避期内返回 nil。
func (i *Issuer) startReservationLocked() *reservation {
	if i.inflight != nil {
		return i.inflight
	}
	if i.monoNow().Before(i.notBefore) {
		return nil
	}
	res := &reservation{done: make(chan struct{})}
	i.inflight = res
	i.wg.Add(1)
	safego.Go("id_issuer.reserve", func() {
		defer i.wg.Done()
		ctx, cancel := context.WithTimeout(i.bg, reserveTimeout)
		defer cancel()
		b, err := i.reserve(ctx)

		i.mu.Lock()
		i.inflight = nil
		if err != nil {
			i.notBefore = i.monoNow().Add(reserveRetryBackoff)
			logx.Errorf("[id-issuer] reserve block failed: %v", err)
		} else if i.cur.remaining() == 0 && i.next == nil {
			i.cur = b
		} else {
			i.next = &b
		}
		res.err = err
		close(res.done)
		i.mu.Unlock()
	})
	return res
}

// reserve 预留一块。只有两份记录都 CAS 成功才返回成功。任何一步失败,这块都作废:
// 已经写成功的那一份留在高位,只浪费值域,不影响唯一性。
func (i *Issuer) reserve(ctx context.Context) (block, error) {
	for attempt := 0; attempt < reserveAttempts; attempt++ {
		p, err := i.opt.Primary.Read(ctx)
		if err != nil {
			i.observe(ReserveStoreError)
			return block{}, fmt.Errorf("read primary record: %w", err)
		}
		s, err := i.opt.Secondary.Read(ctx)
		if err != nil {
			i.observe(ReserveStoreError)
			return block{}, fmt.Errorf("read secondary record: %w", err)
		}
		start, err := i.nextStart(p, s)
		if err != nil {
			i.observe(ReserveRefused)
			return block{}, err
		}
		if start+i.opt.BlockSize >= i.limit {
			i.observe(ReserveRefused)
			return block{}, ErrExhausted
		}
		end := start + i.opt.BlockSize

		// 两份都是严格的仲裁点:任何一份冲突,这块都作废,从头再来。不能因为「重读到的值已经
		// 不小于我的 end」就认领:etcd 回退的那一刻,两个副本可能算出同一块。
		ok, err := i.opt.Primary.CompareAndSwap(ctx, p, end)
		if err != nil {
			i.observe(ReserveStoreError)
			return block{}, fmt.Errorf("write primary record: %w", err)
		}
		if !ok {
			i.observe(ReserveConflict)
			continue
		}
		ok, err = i.opt.Secondary.CompareAndSwap(ctx, s, end)
		if err != nil {
			i.observe(ReserveStoreError)
			return block{}, fmt.Errorf("write secondary record: %w", err)
		}
		if !ok {
			i.observe(ReserveConflict)
			continue
		}
		i.observe(ReserveOK)
		logx.Infof("[id-issuer] reserved block [%d, %d) region=%d", start, end, i.opt.Region)
		return block{pos: start, end: end, halfway: start + i.opt.BlockSize/2}, nil
	}
	return block{}, ErrContended
}

// nextStart 算下一块的起点:两份记录与时间下限的较大者。
func (i *Issuer) nextStart(p, s Record) (uint64, error) {
	for _, r := range []Record{p, s} {
		if r.Exists && r.Value>>regionShift != uint64(i.opt.Region) {
			return 0, fmt.Errorf("%w: record=%d region=%d", ErrRegionMismatch, r.Value, i.opt.Region)
		}
	}
	if !p.Exists && !s.Exists && !i.opt.AllowBootstrap {
		return 0, ErrCounterMissing
	}
	if p.Exists != s.Exists {
		i.observe(ReserveOneRecord)
		logx.Errorf("[id-issuer] one reservation record is missing (primary=%v secondary=%v): "+
			"continuing from the other one, and restoring the missing one", p.Exists, s.Exists)
	} else if p.Exists && p.Value != s.Value {
		// 正常情况下两份相等(预留中途崩溃时主记录可能更高)。差得多说明某一份回退过。
		i.observe(ReserveRecordsBack)
		logx.Errorf("[id-issuer] reservation records differ (primary=%d secondary=%d): "+
			"continuing from the larger one", p.Value, s.Value)
	}

	start := max(p.Value, s.Value, i.base)
	// 时间下限永远生效,不因为跳得远就放弃:它只会把起点往上抬,不可能造成重号,而两份记录同时
	// 回退时它是最后一道防线。记录落后时钟很多也是正常的:用量小的时候,一块号能用几个月。
	floor := i.timeFloor()
	if floor > start {
		start = floor
	} else if floor != 0 && (start-floor)>>seqBits > maxAheadSeconds {
		i.observe(ReserveAheadOfClock)
		logx.Errorf("[id-issuer] counter %d is far ahead of the clock (floor %d): the clock jumped forward at some point; "+
			"ids stay unique, but the time floor is not protecting against a rollback of both records until the clock catches up",
			start, floor)
	}
	if start == 0 {
		start = 1 // 0 是非法 id
	}
	return start, nil
}

// timeFloor 是当前时间对应的计数器值。时钟不在可表示的范围内时返回 0,即这次不采用时间下限:
// 坏时钟不能挡住发号,两份记录仍然保证唯一性。
func (i *Issuer) timeFloor() uint64 {
	sec := i.opt.Now().Unix() - epochUnix
	if sec <= 0 || uint64(sec) >= uint64(1)<<timeBits {
		logx.Errorf("[id-issuer] clock %v is outside the representable range, time floor not applied", i.opt.Now())
		return 0
	}
	return i.base | uint64(sec)<<seqBits
}

func (i *Issuer) observe(result string) {
	if i.opt.OnReserve != nil {
		i.opt.OnReserve(result)
	}
}

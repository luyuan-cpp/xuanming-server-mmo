// Package idissuer 是永久 id 的发号器:AllocateIdSegment 背后的实现。
//
// 设计见 docs/design/player-data-tidb-scaling-plan.md §6.1~§6.3。要点:
//
//   - 从内存切号段。发号器先预留一大块号,记录落盘后,再从这块里切 [lo, hi) 给调用方。
//     一块用完(或用掉一半时后台预取)才碰一次存储,所以没有单行热点和单键争用。
//   - 预留记录写三份,都只增不减:本地域两套互相独立的 etcd 各一份(AGENTS §7 的强规定),
//     全局号源登记处一份(全部地域、全部物理集群共用一套)。一块号只有三份都 CAS 成功才能发。
//     任何一份或两份丢失、回退,剩下的那份仍然盖得住所有发过的号,下一块自动跳到它之上:
//     本地两套同时回档时靠登记处,登记处丢失或回档时靠本地两份。
//   - 登记处同时是地域码的唯一绑定。一个地域码只认一条发号世系(初始化时随机生成的 authority)、
//     一个部署环境(Options.Deployment,生产是运行期读到的 K8s 集群身份)和其中的一对本地 etcd(按 etcd 自己报的集群号)。
//     只比集群号不够:同一份清单部署到两个 K8s 集群,etcd 集群号相同;集群级备份恢复还会把集群号连同数据一起带走。
//     重复部署、克隆旧配置和快照、复用退役码,在预留之前被拒绝。绑定拦不住的情形(整个 K8s 控制面连同 kube-system
//     一起从备份克隆出第二个集群),登记处水位照样让两边串行,登记处完好时不会重号。
//   - 块在内存里、从不回读,所以存储回退影响不到手里已有的块。
//   - 时钟只是最后一道防线:抬高块的起点。它只能把起点往上抬,坏时钟不会造成重号。正常路径不依赖时钟。
//   - 首次初始化和事故恢复分开:Init 只给从没认领过的地域码;Recover 只按幸存记录取上界,拿不到可信上界就拒绝;
//     Retire 留墓碑,码永不再用。
//
// 所有种类共用一个计数器,所以系统里任意两个 id 都不相同。biz_tag 只用来校验和记指标。
//
// 计数器结构(63 位):[地域 6][时间 33,秒][序号 24]。客户端不配、不解析,拿到的 [lo, hi)
// 已经包含地域。这个结构一旦开始发正式号就不能改。
package idissuer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

	// maxAheadSeconds:计数器比时钟超前这么多,说明时钟曾经跳到过未来。这不影响唯一性,只告警。
	// 正常超前量是最近几次预留的块长(每块约 256 秒)。
	maxAheadSeconds = uint64(7 * 24 * 3600)

	// reserveAttempts 是一次预留(或一次运维操作)遇到 CAS 冲突时的重试上限。冲突只发生在多个副本同时写时。
	reserveAttempts = 8
	// reserveTimeout 是后台预留的预算。登记处可能在别的地域,三次读三次写留足余量。
	reserveTimeout = 10 * time.Second
	// reserveRetryBackoff 是预留失败后,下一次允许预留的最短间隔,防止把已经倒下的 etcd 打成雪崩。
	reserveRetryBackoff = 500 * time.Millisecond
)

// 登记处里一个地域码的状态。
const (
	// ClaimActive:这个码正在发号,绑定见 Claim。
	ClaimActive = "active"
	// ClaimRetired:墓碑。这个码永不再发号,也不能重新初始化。
	ClaimRetired = "retired"
)

var (
	// ErrInvalidTag:biz_tag 不合法。
	ErrInvalidTag = errors.New("id issuer: biz_tag must match ^[a-z0-9_]{1,64}$")
	// ErrUnknownTag:biz_tag 没有登记进 IdSegment.Tags。
	ErrUnknownTag = errors.New("id issuer: biz_tag is not listed in IdSegment.Tags")
	// ErrInvalidStep:step 超过 MaxStep。
	ErrInvalidStep = errors.New("id issuer: step must be within 0..10000000")
	// ErrCounterMissing:登记处有这个码,两份本地记录都不存在。这是 id 安全事件,不是首次部署。
	ErrCounterMissing = errors.New("id issuer: both local reservation records are missing; " +
		"follow docs/ops/id-issuer-runbook.md §4.2 and run data_service -id-issuer-recover")
	// ErrRegistryMissing:登记处没有这个地域码。新地域要先 -id-issuer-init;已经发过号的地域是登记处丢了数据。
	ErrRegistryMissing = errors.New("id issuer: this region code is not registered in the id registry; " +
		"a new region runs data_service -id-issuer-init, a region that has issued ids follows docs/ops/id-issuer-runbook.md §4.3")
	// ErrRegistryNotOpened:登记处没有「下一个可认领的码」。世界第一次部署要先开启登记处;部署过的世界是登记处整个丢了。
	ErrRegistryNotOpened = errors.New("id issuer: the id registry is not opened; the first deployment of the world runs " +
		"data_service -id-registry-open, a registry that lost its data follows docs/ops/id-issuer-runbook.md §4.3")
	// ErrRegionClaimed:初始化时发现这个码已经被认领过(无论是谁、无论是否退役),或者它小于登记处里下一个可认领的码。
	ErrRegionClaimed = errors.New("id issuer: this region code was already claimed in the id registry; " +
		"pick a code that was never used (docs/ops/open-new-region-runbook.md §3)")
	// ErrRegionOutOfOrder:初始化的码大于登记处里下一个可认领的码。码只能按顺序认领:跳过去的码之后永远不能再认领,
	// 一次填错(5 写成 50)就会烧掉中间整段,而码一共只有 63 个。初始化什么都不写。
	ErrRegionOutOfOrder = errors.New("id issuer: region codes are claimed in order and this one is above the next claimable code; " +
		"use the next claimable code from the region code ledger (docs/ops/open-new-region-runbook.md §3)")
	// ErrRegionRetired:这个码已退役,永不再发号。
	ErrRegionRetired = errors.New("id issuer: this region code is retired and can never issue again")
	// ErrBindingMismatch:本地记录的世系、本部署环境的身份或两套 etcd 的集群号,和登记处给这个码绑的不一致。
	ErrBindingMismatch = errors.New("id issuer: this deployment, its reservation records or etcd clusters are not the ones bound to this region code in the id registry")
	// ErrRebindRequired:恢复要把这个码的发号资格转到本部署环境和这两套 etcd 上,需要运维显式确认。
	ErrRebindRequired = errors.New("id issuer: recovery would move this region code to this deployment and these etcd clusters; " +
		"confirm the steps in docs/ops/id-issuer-runbook.md §4.2 and rerun with -id-issuer-rebind")
	// ErrNoTrustedBound:登记处和本地都没有这个码的记录,没有任何东西盖得住发过的号,不能恢复。
	ErrNoTrustedBound = errors.New("id issuer: no surviving record covers the ids this region code has issued; " +
		"it cannot be recovered: retire it and initialize a new code (docs/ops/id-issuer-runbook.md §4.4)")
	// ErrSameCluster:两份本地记录,或登记处和某一份本地记录,落在同一套 etcd 上。只有 dev 例外。
	ErrSameCluster = errors.New("id issuer: two reservation records are on the same etcd cluster")
	// ErrRegionMismatch:记录里的地域和本发号器配置的不一致。
	ErrRegionMismatch = errors.New("id issuer: reservation record belongs to another region")
	// ErrExhausted:值域见底,或者时间字段溢出。
	ErrExhausted = errors.New("id issuer: counter space exhausted")
	// ErrContended:多个副本同时写,重试上限内都没抢到。
	ErrContended = errors.New("id issuer: reservation kept conflicting")
	// ErrClosed:发号器已关闭。
	ErrClosed = errors.New("id issuer: closed")
)

var (
	bizTagPattern    = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	authorityPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// deploymentPattern 是部署身份的格式,生产是 k8s:<kube-system 的 UID>(internal/clusteridentity)。
	deploymentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,127}$`)
)

// IsValidBizTag 给 logic 层做指标 label 消毒:不合法的 tag 不能原样进 Prometheus label。
func IsValidBizTag(tag string) bool {
	return bizTagPattern.MatchString(tag)
}

// Record 是从一份本地预留记录读到的状态。Revision 是这份记录的 CAS 令牌,调用方不解释它。
type Record struct {
	Value uint64
	// Authority 是这份记录所属的发号世系:-id-issuer-init 时随机生成,和登记处里的一致才是本地域的记录。
	Authority string
	Revision  int64
	Exists    bool
	// ClusterID 是存储自己报的集群号,记录不存在时也要给。0 = 这种存储给不出(只有内存实现)。
	ClusterID uint64
}

// CounterStore 是一份本地预留记录。两份实例必须落在互相独立的存储上。
//
// 契约:
//   - Read 是线性读。值损坏时返回错误,不得当作不存在。
//   - CompareAndSwap 仅当记录仍是 prev 时把记录写成 (value, authority)。false = 记录已被别人改过,没有写入;
//     返回错误 = 结果未知,调用方不得认为写入成功,也不得靠重读「值等于我写的」来认领。
type CounterStore interface {
	Read(ctx context.Context) (Record, error)
	CompareAndSwap(ctx context.Context, prev Record, value uint64, authority string) (bool, error)
	Close() error
}

// Claim 是登记处里一个地域码的状态。
type Claim struct {
	// State 是 ClaimActive 或 ClaimRetired。
	State string
	// Authority 是这个码的发号世系。墓碑可以为空:登记处重建时按台账补的退役码没有世系可查。
	Authority string
	// Deployment 是这个码绑定的部署环境身份(Options.Deployment 读到的值,生产是 K8s 集群身份)。
	// dev 自动认领的码、墓碑,以及本字段出现之前写的记录可以为空;生产预留拒绝空值,要 rebind 补上。
	Deployment string
	// PrimaryCluster、SecondaryCluster 是这个码绑定的两套本地 etcd 的集群号。
	PrimaryCluster   uint64
	SecondaryCluster uint64
	// HighWater 是这个码已预留号段的上界(不含):这个码发过的每个号都小于它。
	HighWater uint64
	// Blocks 是经登记处提交过的预留次数。0 = 这个码从没发过号,只有这时才允许把没做完的初始化接着做完。
	Blocks uint64
}

// ClaimRecord 是从登记处读到的一个地域码。
type ClaimRecord struct {
	Claim
	Revision int64
	Exists   bool
	// ClusterID 是登记处所在 etcd 自己报的集群号,记录不存在时也要给。0 = 给不出(只有内存实现)。
	ClusterID uint64
}

// Registry 是全局号源登记处:每个地域码一条记录,外加一个「下一个可认领的码」。全部地域、全部物理集群共用一套,
// 不能和任何一个地域的两套本地 etcd 是同一套。
//
// 码只按顺序认领:每次只能认领「下一个可认领的码」本身,认领时把它推过去,所以一个码一生只能被认领一次:哪怕它的
// 那条记录丢了,它也小于「下一个可认领的码」,初始化照样拒绝。不许跳号:跳过去的码之后永远认领不了,填错一次就烧掉一段。
//
// 契约同 CounterStore:读是线性读,值损坏返回错误,不得当作不存在;写返回错误 = 结果未知。
type Registry interface {
	// Read 读一个码的记录。
	Read(ctx context.Context, region uint8) (ClaimRecord, error)
	// CompareAndSwap 仅当这个码的记录仍是 prev 时写入 next。false = 被别人改过,没有写入。
	CompareAndSwap(ctx context.Context, region uint8, prev ClaimRecord, next Claim) (bool, error)
	// ClaimNew 原子地认领一个新码:仅当登记处已开启(否则 ErrRegistryNotOpened)、region 恰好等于下一个可认领的码
	// (小于它是 ErrRegionClaimed,大于它是 ErrRegionOutOfOrder)、这个码没有记录时,写入 claim 并把下一个可认领的码推到 region+1。
	// false = 并发写冲突,没有写入,调用方重读。
	ClaimNew(ctx context.Context, region uint8, claim Claim) (bool, error)
	// Open 开启登记处:写入下一个可认领的码。已开启时返回 false、不写;next 不大于已有记录里的某个码时拒绝。
	Open(ctx context.Context, next uint32) (bool, error)
	Close() error
}

// Options 是发号器的配置。
type Options struct {
	Primary   CounterStore
	Secondary CounterStore
	// Registry 是全局号源登记处。
	Registry Registry
	// Region 是本地域的地域码,不得超过 MaxRegion。
	Region uint8
	// BlockSize 是一块的大小,0 取 DefaultBlockSize。
	BlockSize uint64
	// AllowBootstrap=true 只给 dev、test:缺什么补什么——登记处没有这个码就在第一次预留时认领,本地记录缺了就照
	// 登记处或另一份重建;也允许三份记录放在同一套存储上,不比集群号。
	AllowBootstrap bool
	// Deployment 读本发号器所在部署环境的身份,生产由 svc 接到 internal/clusteridentity(kube-system 命名空间的 UID)。
	// 地域码绑定的是「这个身份 + 两套本地 etcd 的集群号」。身份必须运行期从环境里读:渲染进配置、Secret 或环境变量的值,
	// 会和 etcd 数据目录一起被集群级备份恢复带到另一个集群。生产(AllowBootstrap=false)必须给;dev 可以不给,不核绑定。
	// 第一次需要时才调用,成功后缓存到进程结束;读失败的那次预留或运维操作失败,不写任何记录,下次再读。
	Deployment func(ctx context.Context) (string, error)
	// Tags 是允许的 biz_tag 清单,不能为空。
	Tags []string
	// Now 为 nil 用 time.Now,单测拨表用。
	Now func() time.Time
	// OnReserve 为 nil 不记。result 见 ReserveXxx 常量。
	OnReserve func(result string)
	// NewAuthority 生成发号世系,为 nil 用 crypto/rand。单测注入固定值。
	NewAuthority func() (string, error)
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
	// ReserveLocalBehind:两份本地记录都落后登记处。正常写序(本地两份先、登记处最后)下不会出现,
	// 只能是本地两套 etcd 被旧快照覆盖过。发号不受影响,下一块从登记处水位之上开始。
	ReserveLocalBehind = "records_behind_registry"
	// ReserveRegistryBehind:复核登记处版本未变后,确认它落后本地记录。预留中断和登记处回档都可能造成差距;
	// 多次预留未完成也能累积超过一块,不能仅凭差距认定回档。发号不受影响。
	ReserveRegistryBehind = "registry_behind"
	// ReserveIdentityError:读不到本部署环境的身份(K8s API 不可达,或 ServiceAccount 没有读 kube-system 的权限)。
	// 这次预留什么都没写;手里的块照发,下一次预留再读。
	ReserveIdentityError = "identity_error"
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

	// deployment 是第一次读成功的部署身份,由 deploymentMu 保护。Pod 不会换集群,进程活着它就不变。
	deploymentMu sync.Mutex
	deployment   string

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
	if opt.Primary == nil || opt.Secondary == nil || opt.Registry == nil {
		return nil, errors.New("id issuer: both local reservation records and the id registry are required")
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
	if !opt.AllowBootstrap && opt.Deployment == nil {
		return nil, errors.New("id issuer: production needs Options.Deployment: the region code is bound to the deployment identity")
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
	if opt.NewAuthority == nil {
		opt.NewAuthority = randomAuthority
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

// ValidAuthority 判断发号世系的格式:32 位小写十六进制。存储实现读到别的格式要当作记录损坏。
func ValidAuthority(authority string) bool {
	return authorityPattern.MatchString(authority)
}

// ValidDeployment 判断部署身份的格式。存储实现读到别的格式要当作记录损坏。
func ValidDeployment(deployment string) bool {
	return deploymentPattern.MatchString(deployment)
}

// deploymentID 返回本部署环境的身份,第一次读成功后缓存。没配 Options.Deployment(只有 dev)时返回空串,绑定不核它。
func (i *Issuer) deploymentID(ctx context.Context) (string, error) {
	if i.opt.Deployment == nil {
		return "", nil
	}
	i.deploymentMu.Lock()
	defer i.deploymentMu.Unlock()
	if i.deployment != "" {
		return i.deployment, nil
	}
	deployment, err := i.opt.Deployment(ctx)
	if err != nil {
		return "", fmt.Errorf("read the deployment identity: %w", err)
	}
	if !ValidDeployment(deployment) {
		return "", fmt.Errorf("id issuer: malformed deployment identity %q", deployment)
	}
	i.deployment = deployment
	return deployment, nil
}

func randomAuthority() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("id issuer: generate authority: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
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
	return errors.Join(i.opt.Primary.Close(), i.opt.Secondary.Close(), i.opt.Registry.Close())
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

// view 是依次读到的三份记录,不是跨集群原子快照。每次预留、每次运维操作都重读,不缓存。
type view struct {
	p, s Record
	g    ClaimRecord
}

func (i *Issuer) read(ctx context.Context) (view, error) {
	// 预留最后写登记处,这里先读登记处再读本地:无回档时,后读的两份本地记录不会低于已读到的登记处水位。
	// 若反过来读,并发副本可能在本地读完后提交新块,旧本地值和新登记处值就会被误判为双本地回档。
	g, err := i.opt.Registry.Read(ctx, i.opt.Region)
	if err != nil {
		return view{}, fmt.Errorf("read id registry: %w", err)
	}
	p, err := i.opt.Primary.Read(ctx)
	if err != nil {
		return view{}, fmt.Errorf("read primary record: %w", err)
	}
	s, err := i.opt.Secondary.Read(ctx)
	if err != nil {
		return view{}, fmt.Errorf("read secondary record: %w", err)
	}
	return view{p: p, s: s, g: g}, nil
}

// reserve 预留一块。只有三份记录都 CAS 成功才返回成功。任何一步失败,这块都作废:
// 已经写成功的那几份留在高位,只浪费值域,不影响唯一性。
//
// 写序固定为主记录、副记录、登记处。所以正常情况下登记处不会高过本地两份,本地两份都落后登记处
// 只能是本地被回档(ReserveLocalBehind)。
func (i *Issuer) reserve(ctx context.Context) (block, error) {
	deployment, err := i.deploymentID(ctx)
	if err != nil {
		i.observe(ReserveIdentityError)
		return block{}, err
	}
	for attempt := 0; attempt < reserveAttempts; attempt++ {
		v, err := i.read(ctx)
		if err != nil {
			i.observe(ReserveStoreError)
			return block{}, err
		}
		authority, err := i.reservable(v, deployment)
		if err != nil {
			i.observe(ReserveRefused)
			return block{}, err
		}
		if v.g.Exists && max(v.p.Value, v.s.Value) > v.g.HighWater {
			// 本地后读,可能已经看到了其他副本刚提交的新块。复核登记处仍是同一版本后才能诊断其落后,
			// 否则整轮重读,不能把正常并发说成回档。对齐路径不增加读取;真实差异仍在任何写入前上报。
			latest, err := i.opt.Registry.Read(ctx, i.opt.Region)
			if err != nil {
				i.observe(ReserveStoreError)
				return block{}, fmt.Errorf("recheck id registry: %w", err)
			}
			if latest != v.g {
				i.observe(ReserveConflict)
				continue
			}
		}
		start := i.nextStart(v)
		if start+i.opt.BlockSize >= i.limit {
			i.observe(ReserveRefused)
			return block{}, ErrExhausted
		}
		end := start + i.opt.BlockSize

		// 三份都是严格的仲裁点:任何一份冲突,这块都作废,从头再来。不能因为「重读到的值已经
		// 不小于我的 end」就认领:etcd 回退的那一刻,两个副本可能算出同一块。
		ok, err := i.opt.Primary.CompareAndSwap(ctx, v.p, end, authority)
		if err != nil {
			i.observe(ReserveStoreError)
			return block{}, fmt.Errorf("write primary record: %w", err)
		}
		if !ok {
			i.observe(ReserveConflict)
			continue
		}
		ok, err = i.opt.Secondary.CompareAndSwap(ctx, v.s, end, authority)
		if err != nil {
			i.observe(ReserveStoreError)
			return block{}, fmt.Errorf("write secondary record: %w", err)
		}
		if !ok {
			i.observe(ReserveConflict)
			continue
		}
		ok, err = i.opt.Registry.CompareAndSwap(ctx, i.opt.Region, v.g, i.claimAfter(v, authority, deployment, end))
		if err != nil {
			i.observe(ReserveStoreError)
			return block{}, fmt.Errorf("write id registry: %w", err)
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

// reservable 判断这次能不能预留,能的话返回要写进本地记录的发号世系。
//
// 生产:登记处必须有这个码、未退役;本地记录(在的那几份)的世系等于登记处的;本部署环境的身份和两套 etcd 的集群号
// 等于登记处绑定的;本地两份不能都不存在(那是事故,走 Recover)。dev(AllowBootstrap):只核世系,缺什么在这次预留里补。
func (i *Issuer) reservable(v view, deployment string) (string, error) {
	if err := i.requireIndependent(v); err != nil {
		return "", err
	}
	if err := i.checkRegion(v); err != nil {
		return "", err
	}
	if !v.g.Exists {
		if !i.opt.AllowBootstrap {
			return "", ErrRegistryMissing
		}
		authority, err := localAuthority(v.p, v.s)
		if err != nil || authority != "" {
			return authority, err
		}
		return i.opt.NewAuthority()
	}
	if v.g.State != ClaimActive {
		return "", ErrRegionRetired
	}
	if err := sameAuthority(v); err != nil {
		return "", err
	}
	if !i.boundHere(v, deployment) {
		return "", fmt.Errorf("%w: this is deployment %q with etcd clusters (%x, %x), the registry binds deployment %q with (%x, %x)",
			ErrBindingMismatch, deployment, v.p.ClusterID, v.s.ClusterID, v.g.Deployment, v.g.PrimaryCluster, v.g.SecondaryCluster)
	}
	if !v.p.Exists && !v.s.Exists && !i.opt.AllowBootstrap {
		return "", ErrCounterMissing
	}
	return v.g.Authority, nil
}

// claimAfter 是这块提交之后登记处的样子:绑定不变,水位推到 end。dev 第一次预留时顺带认领。
func (i *Issuer) claimAfter(v view, authority, deployment string, end uint64) Claim {
	c := v.g.Claim
	if !v.g.Exists {
		c = Claim{State: ClaimActive, Authority: authority, Deployment: deployment,
			PrimaryCluster: v.p.ClusterID, SecondaryCluster: v.s.ClusterID}
	}
	c.HighWater = end
	c.Blocks++
	return c
}

// requireIndependent 核对三份记录确实在三套 etcd 上。配置校验只比地址字符串,DNS 别名、负载均衡、IP 和域名混写都能骗过它,
// 所以运行期再比 etcd 自己报的集群号。某一套故障时读记录就失败了,走不到这里。
func (i *Issuer) requireIndependent(v view) error {
	if i.opt.AllowBootstrap {
		return nil
	}
	if sameCluster(v.p.ClusterID, v.s.ClusterID) {
		return fmt.Errorf("%w: primary and secondary report cluster_id=%x", ErrSameCluster, v.p.ClusterID)
	}
	if sameCluster(v.g.ClusterID, v.p.ClusterID) || sameCluster(v.g.ClusterID, v.s.ClusterID) {
		return fmt.Errorf("%w: the id registry shares cluster_id=%x with a local record", ErrSameCluster, v.g.ClusterID)
	}
	return nil
}

func sameCluster(a, b uint64) bool {
	return a != 0 && a == b
}

// checkRegion 核对三份记录里的值都属于本地域。值是 0 的登记处墓碑不核。
func (i *Issuer) checkRegion(v view) error {
	for _, r := range []Record{v.p, v.s} {
		if r.Exists && r.Value>>regionShift != uint64(i.opt.Region) {
			return fmt.Errorf("%w: record=%d region=%d", ErrRegionMismatch, r.Value, i.opt.Region)
		}
	}
	if v.g.Exists && v.g.HighWater != 0 && v.g.HighWater>>regionShift != uint64(i.opt.Region) {
		return fmt.Errorf("%w: registry high_water=%d region=%d", ErrRegionMismatch, v.g.HighWater, i.opt.Region)
	}
	return nil
}

// boundHere:本部署环境和它的两套本地 etcd,就是登记处给这个码绑的那一份。dev 不比。
//
// 只比集群号不够。etcd 的集群号只由成员 peer 地址和引导令牌算出,同一份清单部署到两个 K8s 集群就相同;
// 集群级备份恢复(Velero、卷快照)还会把数据目录里的集群号一起带到别的集群。部署身份是运行期从集群读的,
// 两种情形下都不同。空身份(本字段出现之前写的记录)一律不算绑定,要 rebind 补上。
func (i *Issuer) boundHere(v view, deployment string) bool {
	if i.opt.AllowBootstrap {
		return true
	}
	return deployment != "" && v.g.Deployment == deployment &&
		v.p.ClusterID == v.g.PrimaryCluster && v.s.ClusterID == v.g.SecondaryCluster
}

// sameAuthority:在的本地记录都属于登记处里的世系。
func sameAuthority(v view) error {
	for _, r := range []Record{v.p, v.s} {
		if r.Exists && r.Authority != v.g.Authority {
			return fmt.Errorf("%w: local record authority %s, the registry binds %s", ErrBindingMismatch, r.Authority, v.g.Authority)
		}
	}
	return nil
}

// localAuthority 是两份本地记录共同的世系;两份都不存在时返回空。
func localAuthority(p, s Record) (string, error) {
	switch {
	case p.Exists && s.Exists && p.Authority != s.Authority:
		return "", fmt.Errorf("%w: primary authority %s, secondary authority %s", ErrBindingMismatch, p.Authority, s.Authority)
	case p.Exists:
		return p.Authority, nil
	case s.Exists:
		return s.Authority, nil
	}
	return "", nil
}

// nextStart 算下一块的起点:三份记录与时间下限的最大值。
func (i *Issuer) nextStart(v view) uint64 {
	local := max(v.p.Value, v.s.Value)
	if v.p.Exists != v.s.Exists {
		i.observe(ReserveOneRecord)
		logx.Errorf("[id-issuer] one local reservation record is missing (primary=%v secondary=%v): "+
			"continuing above the surviving records, and restoring the missing one", v.p.Exists, v.s.Exists)
	} else if v.p.Exists && v.p.Value != v.s.Value {
		// 正常情况下两份相等(预留中途崩溃时主记录可能更高)。差得多说明某一份回退过。
		i.observe(ReserveRecordsBack)
		logx.Errorf("[id-issuer] local reservation records differ (primary=%d secondary=%d): "+
			"continuing from the larger one", v.p.Value, v.s.Value)
	}
	if v.g.Exists && (v.p.Exists || v.s.Exists) {
		switch {
		case v.g.HighWater > local:
			i.observe(ReserveLocalBehind)
			logx.Errorf("[id-issuer] both local records (%d) are behind the id registry (%d): both local etcd clusters "+
				"were restored from older data; continuing above the registry and raising them (docs/ops/id-issuer-runbook.md §4.1)",
				local, v.g.HighWater)
		case local > v.g.HighWater:
			i.observe(ReserveRegistryBehind)
			if local-v.g.HighWater > i.opt.BlockSize {
				logx.Errorf("[id-issuer] the id registry (%d) is more than one block behind the local records (%d): "+
					"several reservations may have stopped before their registry writes, or registry data may have been restored; "+
					"continuing above the local records and raising it", v.g.HighWater, local)
			} else {
				logx.Infof("[id-issuer] the id registry (%d) is behind the local records (%d) by at most one block: "+
					"an earlier reservation stopped before its registry write, or another replica is reserving", v.g.HighWater, local)
			}
		}
	}

	start := max(local, v.g.HighWater, i.base)
	// 时间下限永远生效,不因为跳得远就放弃:它只会把起点往上抬,不可能造成重号。记录落后时钟很多也是正常的:
	// 用量小的时候,一块号能用几个月。
	floor := i.timeFloor()
	if floor > start {
		start = floor
	} else if floor != 0 && (start-floor)>>seqBits > maxAheadSeconds {
		i.observe(ReserveAheadOfClock)
		logx.Errorf("[id-issuer] counter %d is far ahead of the clock (floor %d): the clock jumped forward at some point; "+
			"ids stay unique, only the time floor is not adding anything until the clock catches up", start, floor)
	}
	if start == 0 {
		start = 1 // 0 是非法 id
	}
	return start
}

// timeFloor 是当前时间对应的计数器值。时钟不在可表示的范围内时返回 0,即这次不采用时间下限:
// 坏时钟不能挡住发号,三份记录仍然保证唯一性。
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

// Init 给一个从没认领过的地域码初始化:先在登记处认领(登记处里没有这个码才能成功),再建两份本地记录。
// 只由部署步骤显式调用(data_service -id-issuer-init),运行路径不走这里。
//
// 返回 created=false、err=nil:这个码已经由本部署环境的这两套 etcd 初始化过,什么也没做,重复执行是安全的。
// 认领之后、建本地记录之前中断过,而这个码还没发过号(Blocks=0)时,同一个部署环境再跑一次接着做完。
// 其他情形都拒绝,一律不写:
//   - 码已被别的世系、别的部署环境或别的 etcd 认领,或已退役:ErrRegionClaimed / ErrRegionRetired。用过的码永不再分配;
//   - 本地有记录而登记处没有:登记处丢了数据,ErrRegistryMissing,走 Recover;
//   - 发过号的码本地两份记录都没了:事故,ErrCounterMissing,走 Recover。
func (i *Issuer) Init(ctx context.Context) (bool, error) {
	deployment, err := i.deploymentID(ctx)
	if err != nil {
		return false, err
	}
	for attempt := 0; attempt < reserveAttempts; attempt++ {
		v, err := i.read(ctx)
		if err != nil {
			return false, err
		}
		if err := i.requireIndependent(v); err != nil {
			return false, err
		}
		if err := i.checkRegion(v); err != nil {
			return false, err
		}
		if !v.g.Exists {
			if v.p.Exists || v.s.Exists {
				return false, fmt.Errorf("%w: the local records exist but the code is not registered; "+
					"the registry lost data, this is not a new region", ErrRegistryMissing)
			}
			authority, err := i.opt.NewAuthority()
			if err != nil {
				return false, err
			}
			// 全新的码之前没发过任何号,从值域下限起步;第一次预留会被时间下限抬到当前时间。
			claim := Claim{State: ClaimActive, Authority: authority, Deployment: deployment, PrimaryCluster: v.p.ClusterID,
				SecondaryCluster: v.s.ClusterID, HighWater: i.base | 1}
			ok, err := i.opt.Registry.ClaimNew(ctx, i.opt.Region, claim)
			if errors.Is(err, ErrRegionClaimed) || errors.Is(err, ErrRegionOutOfOrder) || errors.Is(err, ErrRegistryNotOpened) {
				return false, err
			}
			if err != nil {
				return false, fmt.Errorf("claim the code in the id registry (outcome unknown, rerun -id-issuer-init): %w", err)
			}
			if !ok {
				continue // 有人同时认领:重读,按已认领处理
			}
			if err := i.createMissing(ctx, v, claim.HighWater, authority); err != nil {
				return false, err
			}
			return true, nil
		}
		if v.g.State != ClaimActive {
			return false, ErrRegionRetired
		}
		if err := sameAuthority(v); err != nil {
			return false, fmt.Errorf("%w: %v", ErrRegionClaimed, err)
		}
		if !i.boundHere(v, deployment) {
			return false, fmt.Errorf("%w: it is bound to deployment %q with etcd clusters (%x, %x), this is deployment %q with (%x, %x)",
				ErrRegionClaimed, v.g.Deployment, v.g.PrimaryCluster, v.g.SecondaryCluster, deployment, v.p.ClusterID, v.s.ClusterID)
		}
		if v.p.Exists && v.s.Exists {
			return false, nil
		}
		if v.g.Blocks != 0 {
			if !v.p.Exists && !v.s.Exists {
				return false, ErrCounterMissing
			}
			return false, nil // 只缺一份:下一次预留按另一份补上
		}
		if err := i.createMissing(ctx, v, v.g.HighWater, v.g.Authority); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, ErrContended
}

// createMissing 建 v 里不存在的本地记录。任何一份写失败都返回错误,重跑同一个命令即可:已建的不会重建。
func (i *Issuer) createMissing(ctx context.Context, v view, value uint64, authority string) error {
	for _, r := range []struct {
		name  string
		store CounterStore
		rec   Record
	}{{"primary", i.opt.Primary, v.p}, {"secondary", i.opt.Secondary, v.s}} {
		if r.rec.Exists {
			continue
		}
		ok, err := r.store.CompareAndSwap(ctx, r.rec, value, authority)
		if err != nil {
			return fmt.Errorf("create the %s record (outcome unknown, rerun the same command): %w", r.name, err)
		}
		if !ok {
			return fmt.Errorf("%w: the %s record was written concurrently, rerun the same command", ErrContended, r.name)
		}
	}
	return nil
}

// Recover 修复事故之后的记录。上界只取幸存的记录,拿不到可信上界就拒绝,从不猜起点。
// 只由运维显式调用(data_service -id-issuer-recover),运行路径不走这里。返回做了什么,给命令打印。
//
//   - 登记处没有这个码,本地还有记录:登记处丢了数据。按本地记录的最大值重新登记,绑定这两套 etcd。
//     本地也一份不剩:ErrNoTrustedBound,这个码不能再用。
//   - 登记处有,本地两份都没了:按登记处水位重建两份本地记录。两份都没了就证明不了这两套 etcd 还是原来那两套,
//     所以要 rebind;同一个部署环境里认领之后从没发过号的码(没做完的初始化)例外。
//   - 部署环境的身份或两套 etcd 的集群号和登记处绑的不一致(灾备到了另一个 K8s 集群,或 etcd 换成了新集群):
//     要 rebind,把绑定改到本部署环境和这两套 etcd。
//   - 本地只缺一份、本地落后登记处、登记处落后本地:不用恢复,下一次预留自动对齐。
//
// rebind=true 把这个码的发号资格转到本部署环境和这两套 etcd 上,原来那一份从此预留被拒(ErrBindingMismatch)。
// 只在确认原来那一份已经不在、或不再用于这个地域之后用,见运维手册 §4.2。
//
// 旧进程不需要先停:三份记录每次预留都重读,旧进程下一次预留只能落在登记处水位之上或被拒;
// 它们内存里的块都小于这里取的上界,接着发也不重号。
func (i *Issuer) Recover(ctx context.Context, rebind bool) (string, error) {
	deployment, err := i.deploymentID(ctx)
	if err != nil {
		return "", err
	}
	for attempt := 0; attempt < reserveAttempts; attempt++ {
		v, err := i.read(ctx)
		if err != nil {
			return "", err
		}
		if err := i.requireIndependent(v); err != nil {
			return "", err
		}
		if err := i.checkRegion(v); err != nil {
			return "", err
		}
		if !v.g.Exists {
			authority, err := localAuthority(v.p, v.s)
			if err != nil {
				return "", err
			}
			if authority == "" {
				return "", ErrNoTrustedBound
			}
			// Blocks 记不回来,写 1:非 0 表示这个码发过号,Init 不会把它当成没做完的初始化。
			claim := Claim{State: ClaimActive, Authority: authority, Deployment: deployment, PrimaryCluster: v.p.ClusterID,
				SecondaryCluster: v.s.ClusterID, HighWater: max(v.p.Value, v.s.Value), Blocks: 1}
			ok, err := i.opt.Registry.CompareAndSwap(ctx, i.opt.Region, v.g, claim)
			if err != nil {
				return "", fmt.Errorf("re-register the code (outcome unknown, rerun -id-issuer-recover): %w", err)
			}
			if !ok {
				continue
			}
			return fmt.Sprintf("re-registered region %d in the id registry for deployment %q above the local records (high_water=%d)",
				i.opt.Region, deployment, claim.HighWater), nil
		}
		if v.g.State != ClaimActive {
			return "", ErrRegionRetired
		}
		if err := sameAuthority(v); err != nil {
			return "", err
		}
		bothMissing := !v.p.Exists && !v.s.Exists
		bound := i.boundHere(v, deployment)
		if bound && !bothMissing {
			return "nothing to repair: a missing or older local record is restored by the next reservation", nil
		}
		if bound && v.g.Blocks == 0 {
			if err := i.createMissing(ctx, v, v.g.HighWater, v.g.Authority); err != nil {
				return "", err
			}
			return "completed an interrupted -id-issuer-init (the code never issued ids)", nil
		}
		if !rebind {
			return "", ErrRebindRequired
		}
		claim := v.g.Claim
		claim.Deployment = deployment
		claim.PrimaryCluster, claim.SecondaryCluster = v.p.ClusterID, v.s.ClusterID
		claim.HighWater = max(v.g.HighWater, v.p.Value, v.s.Value)
		if claim != v.g.Claim {
			ok, err := i.opt.Registry.CompareAndSwap(ctx, i.opt.Region, v.g, claim)
			if err != nil {
				return "", fmt.Errorf("rebind the code (outcome unknown, rerun the same command): %w", err)
			}
			if !ok {
				continue
			}
		}
		if err := i.createMissing(ctx, v, claim.HighWater, claim.Authority); err != nil {
			return "", err
		}
		return fmt.Sprintf("bound region %d to deployment %q and etcd clusters (%x, %x) above high_water=%d",
			i.opt.Region, claim.Deployment, claim.PrimaryCluster, claim.SecondaryCluster, claim.HighWater), nil
	}
	return "", ErrContended
}

// Retire 让这个地域码永久退役:登记处留墓碑,之后预留、初始化、恢复全部拒绝,码永不再分配。
// 登记处没有这个码时也写墓碑:登记处重建时按台账补回退役码,防止它被当成没用过的码重新认领。
// 已经退役时什么也不做。只碰登记处,不碰本地记录;正在跑的发号器发完手里的块就停。
//
// 码还在用(active)时,只能从绑定它的那个部署环境退役(retirableHere)。退役没有撤销入口,码一共只有 63 个;
// staging 和生产共用登记处,配置里地域码写错,就会从别的环境退掉一个在用的生产码。绑定它的地域已经不在、
// 没法从那里执行时,运维确认之后传 unbound=true(-id-issuer-retire-unbound)放行。
func (i *Issuer) Retire(ctx context.Context, unbound bool) (bool, error) {
	for attempt := 0; attempt < reserveAttempts; attempt++ {
		g, err := i.opt.Registry.Read(ctx, i.opt.Region)
		if err != nil {
			return false, fmt.Errorf("read id registry: %w", err)
		}
		if g.Exists && g.State == ClaimRetired {
			return false, nil
		}
		if g.Exists && !unbound {
			if err := i.retirableHere(ctx, g); err != nil {
				return false, err
			}
		}
		claim := g.Claim
		claim.State = ClaimRetired
		ok, err := i.opt.Registry.CompareAndSwap(ctx, i.opt.Region, g, claim)
		if err != nil {
			return false, fmt.Errorf("retire the code (outcome unknown, rerun -id-issuer-retire): %w", err)
		}
		if ok {
			return true, nil
		}
	}
	return false, ErrContended
}

// retirableHere 核对本进程就是登记处给这个在用码绑定的那个部署环境:两份本地记录的地域和世系对得上,
// 部署身份和两套 etcd 的集群号等于登记处绑定的。g 是先读到的登记处记录,这里再读两份本地记录,读序同预留。
// 退役仍按 g 的版本提交:核对之后有人 rebind,提交就冲突,整轮重来。
func (i *Issuer) retirableHere(ctx context.Context, g ClaimRecord) error {
	deployment, err := i.deploymentID(ctx)
	if err != nil {
		return err
	}
	p, err := i.opt.Primary.Read(ctx)
	if err != nil {
		return fmt.Errorf("read primary record: %w", err)
	}
	s, err := i.opt.Secondary.Read(ctx)
	if err != nil {
		return fmt.Errorf("read secondary record: %w", err)
	}
	v := view{p: p, s: s, g: g}
	cause := i.requireIndependent(v)
	if cause == nil {
		cause = i.checkRegion(v)
	}
	if cause == nil {
		cause = sameAuthority(v)
	}
	if cause == nil && !i.boundHere(v, deployment) {
		cause = fmt.Errorf("this is deployment %q with etcd clusters (%x, %x), the registry binds deployment %q with (%x, %x)",
			deployment, p.ClusterID, s.ClusterID, g.Deployment, g.PrimaryCluster, g.SecondaryCluster)
	}
	if cause != nil {
		return fmt.Errorf("%w: refusing to retire region %d, which is in use, from here: %w; run -id-issuer-retire in the region "+
			"bound to it, or add -id-issuer-retire-unbound once that region is confirmed gone (docs/ops/id-issuer-runbook.md §4.6)",
			ErrBindingMismatch, i.opt.Region, cause)
	}
	return nil
}

// MaxNextRegion 是「下一个可认领的码」的上限:等于它表示 0..MaxRegion 全部用过。
const MaxNextRegion = uint32(MaxRegion) + 1

// OpenRegistry 开启登记处,next 是第一个允许 -id-issuer-init 认领的码。只在两种时候由运维显式调用
// (data_service -id-registry-open):世界第一次部署,next=1;登记处整个丢了数据重建时,next 取地域码台账里
// 用过的最大码 +1(运维手册 §4.3)。已开启时什么也不做,返回 false。
//
// 这一步把「新世界」和「丢了数据的旧世界」分开:登记处没开启时,初始化一律拒绝,不会把用过的码当成没用过。
func (i *Issuer) OpenRegistry(ctx context.Context, next uint32) (bool, error) {
	if next > MaxNextRegion {
		return false, fmt.Errorf("id issuer: next region %d exceeds %d", next, MaxNextRegion)
	}
	return i.opt.Registry.Open(ctx, next)
}

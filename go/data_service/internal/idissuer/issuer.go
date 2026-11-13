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
//   - 两份本地记录也写着世系和部署身份。登记处丢了这个码时,它们是「谁是绑定的那一份」唯一幸存的证据:
//     Recover 只让写下本地记录的那个部署环境重新登记;别的环境拿着拷过来的记录不行,那不是可信的上界。
//   - 块在内存里、从不回读,所以存储回退影响不到手里已有的块。
//   - 时钟只是最后一道防线:抬高块的起点。它只能把起点往上抬,坏时钟不会造成重号。正常路径不依赖时钟。
//     登记处丢码、凭本地记录重新登记时,它再给水位加一道上界(reRegisterHighWater),同样只往上抬。
//   - 首次初始化和事故恢复分开:Init 只给从没认领过的地域码;Recover 只按幸存记录取上界,拿不到可信上界就拒绝;
//     Retire 留墓碑,码永不再用。登记处整个丢了数据时先声明重建(BeginRegistryRebuild),补完记录再开放认领。
//   - 活着的发号进程是第四个证人:它记得自己确认过的水位,预留从它之上开始;作证检查发现三份记录低于它
//     (被回档或丢了)就抬回去(checkRecords)。三份记录一起出事时,记录自己证明不了什么,靠的就是它。
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
	mathrand "math/rand/v2"
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

	// maxAheadSeconds:计数器比时钟超前这么多就告警(ahead_of_clock):时钟曾经跳到过未来,或者预留快过了时钟(崩溃循环)。
	// 这不影响唯一性。正常超前量是最近几次预留的块长(每块约 256 秒)。它同时是登记处丢码时重新登记的时间余量
	// (reRegisterHighWater):没告警过,发过的号就都在「当时的时间下限 + 它 + 一块」之下。
	maxAheadSeconds = uint64(7 * 24 * 3600)

	// reserveAttempts 是一次预留(或一次运维操作)遇到 CAS 冲突时的重试上限。冲突只发生在多个副本同时写时。
	reserveAttempts = 8
	// reserveTimeout 是后台预留的预算。登记处可能在别的地域,三次读三次写留足余量。
	reserveTimeout = 10 * time.Second
	// reserveRetryBackoff 是预留失败后,下一次允许预留的最短间隔,防止把已经倒下的 etcd 打成雪崩。
	reserveRetryBackoff = 500 * time.Millisecond

	// reserveConflictBackoffBase、reserveConflictBackoffMax 是一次预留内部、CAS 冲突之后到下一轮重读之前的等待上限:
	// 从 base 起逐轮翻倍,封顶 max,实际等待在 [0, 上限) 里均匀取(见 conflictPause)。7 次等待最多共约 1 秒,
	// 远小于 reserveTimeout。
	reserveConflictBackoffBase = 10 * time.Millisecond
	reserveConflictBackoffMax  = 320 * time.Millisecond

	// recordCheckInterval 是作证检查(checkRecords)的间隔:每个副本每次线性读三份记录,其中一次跨地域。它决定三份记录
	// 同时回档之后,活着的进程最多隔多久把它们抬回去;这段时间里别的副本恰好预留,才可能从回档后的值起步。
	recordCheckInterval = 10 * time.Second
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
	// 这时补墓碑也是它:没有指针可以对照,要先 -id-registry-rebuild 声明。
	ErrRegistryNotOpened = errors.New("id issuer: the id registry is not opened; the first deployment of the world runs " +
		"data_service -id-registry-open, a registry that lost its data follows docs/ops/id-issuer-runbook.md §4.3")
	// ErrRegistryRebuilding:登记处在重建(-id-registry-rebuild 声明了下一个可认领的码,还没开启)。重建期间只补记录,
	// 不认领新码:在用的地域还没全部重新登记时,认领可能撞上台账漏记、又还没登记回来的码。
	ErrRegistryRebuilding = errors.New("id issuer: the id registry is being rebuilt and accepts no new claims yet; " +
		"finish the rebuild with data_service -id-registry-open (docs/ops/id-issuer-runbook.md §4.3)")
	// ErrRegistryNeedsRebuild:登记处没有「下一个可认领的码」,却已经有地域的记录。新世界不会这样,只能是登记处丢过数据、
	// 有地域已经重新登记;直接开启会跳过重建期间不许认领的那段窗口,所以拒绝,什么都不写。
	ErrRegistryNeedsRebuild = errors.New("id issuer: the id registry has region entries but no next claimable code, so it lost its data; " +
		"declare the next claimable code with data_service -id-registry-rebuild first (docs/ops/id-issuer-runbook.md §4.3)")
	// ErrNextRegionMismatch:结束重建时给的下一个可认领的码和登记处里的不一致。重建期间在用的地域重新登记到指针之上、
	// 或者跳过了一个码,都会把它推高;让运维对着地域码台账再确认一次。什么都不写。
	ErrNextRegionMismatch = errors.New("id issuer: the next claimable code given does not match the one the id registry holds; " +
		"check it against the region code ledger and rerun -id-registry-open with it (docs/ops/id-issuer-runbook.md §4.3)")
	// ErrRegionClaimed:初始化时发现这个码已经被认领过(无论是谁、无论是否退役),或者它小于登记处里下一个可认领的码。
	ErrRegionClaimed = errors.New("id issuer: this region code was already claimed in the id registry; " +
		"use the next claimable code from the region code ledger (docs/ops/open-new-region-runbook.md §3)")
	// ErrRegionOutOfOrder:初始化的码大于登记处里下一个可认领的码。码只能按顺序认领:跳过去的码之后永远不能再认领,
	// 一次填错(5 写成 50)就会烧掉中间整段,而码一共只有 63 个。初始化什么都不写。
	// 给一个从没认领过、又大于下一个可认领的码的码补墓碑(Retire)也是它:同样什么都不写。
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
	// ErrForeignRecords:登记处没有这个码,而本地记录不是本部署环境写下的(别的环境的 etcd 数据拷到了这里,或者记录里没有身份)。
	// 拷来的记录可能比原环境发过的号旧,不是可信上界,不能据此重新登记;rebind 也不放行,因为这时没有登记处水位兜底。
	ErrForeignRecords = errors.New("id issuer: the id registry has no entry for this region code and the local reservation records " +
		"were not written by this deployment; copied records may be older than the ids already issued, so this deployment cannot " +
		"re-register the code. Do not retire the code from here: the deployment that wrote the records runs -id-issuer-recover " +
		"(docs/ops/id-issuer-runbook.md §4.3)")
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
	// Deployment 是写下这份记录的部署环境身份(Options.Deployment 读到的值)。每次预留、初始化、恢复建记录时写入。
	// 登记处有这个码时以登记处的绑定为准,不看它;登记处丢了这个码时,它是谁该重新登记唯一幸存的证据(Recover)。
	// dev 写的记录为空。
	Deployment string
	Revision   int64
	Exists     bool
	// ClusterID 是存储自己报的集群号,记录不存在时也要给。0 = 这种存储给不出(只有内存实现)。
	ClusterID uint64
}

// CounterStore 是一份本地预留记录。两份实例必须落在互相独立的存储上。
//
// 契约:
//   - Read 是线性读。值损坏时返回错误,不得当作不存在。
//   - CompareAndSwap 仅当记录仍是 prev 时把记录写成 (value, authority, deployment)。false = 记录已被别人改过,没有写入;
//     返回错误 = 结果未知,调用方不得认为写入成功,也不得靠重读「值等于我写的」来认领。
type CounterStore interface {
	Read(ctx context.Context) (Record, error)
	CompareAndSwap(ctx context.Context, prev Record, value uint64, authority, deployment string) (bool, error)
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
	//
	// prev 不存在时是给一个没有记录的码补一条(Recover 重新登记、Retire 补墓碑、dev 第一次预留时认领),同一个原子写里
	// 守住「有记录的码都小于下一个可认领的码」。守不住的话,顺序认领走到这个码时既认领不了、也推不过去,之后的码全部卡死:
	//   - 还没有下一个可认领的码(新世界没开启,或者登记处整个丢了、还没声明重建):在用的码直接写,之后声明指针时核对它
	//     高于全部已有记录。墓碑返回 ErrRegistryNotOpened、不写:没有指针可以对照,填错的码也会写进去,之后声明指针就被它卡住。
	//   - 码小于下一个可认领的码:直接写,这是一个用过的码丢了记录。
	//   - 码不小于下一个可认领的码:写入,并把下一个可认领的码推到 region+1(重建中照样推,推完仍在重建中)。只接受两种:
	//     在用的码(Recover 凭幸存的本地记录重新登记,说明重建时声明得太低,台账漏记了它);或者 region 恰好等于下一个可认领的码的
	//     墓碑(这个码不用了,跳过)。墓碑大于下一个可认领的码时返回 ErrRegionOutOfOrder、不写:没有证据说明中间的码用过,
	//     填错一次就烧掉一段。
	CompareAndSwap(ctx context.Context, region uint8, prev ClaimRecord, next Claim) (bool, error)
	// ClaimNew 原子地认领一个新码:仅当登记处已开放认领(没有指针是 ErrRegistryNotOpened,重建中是 ErrRegistryRebuilding)、
	// region 恰好等于下一个可认领的码(小于它是 ErrRegionClaimed,大于它是 ErrRegionOutOfOrder)、这个码没有记录时,
	// 写入 claim 并把下一个可认领的码推到 region+1。false = 并发写冲突,没有写入,调用方重读。
	ClaimNew(ctx context.Context, region uint8, claim Claim) (bool, error)
	// BeginRebuild 在登记处整个丢了数据之后声明下一个可认领的码,进入重建:补记录按它核对,还不开放认领。
	// 已经有指针(重建中或已开放)时返回 false、不写;next 不大于已有记录里的某个码时返回 ErrRegionClaimed、不写。
	BeginRebuild(ctx context.Context, next uint32) (bool, error)
	// Open 开放认领。没有指针时写入 next(世界第一次部署),这时不能已有任何码的记录,否则返回 ErrRegistryNeedsRebuild、不写。
	// 重建中时结束重建:next 必须等于登记处里现在的值,否则返回 ErrNextRegionMismatch、不写。已开放时返回 false、不写。
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
	// ReserveBehindIssuer:有记录低于本进程确认过的水位(witness)。三份记录只增不减,这只能是被回档或丢了。
	// 预留时三份都低于它,下一块从它之上开始;作证检查发现时把低的那几份抬回去(checkRecords)。
	ReserveBehindIssuer = "records_behind_issuer"
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
	// pause 是一次预留里 CAS 冲突之后、下一轮重读之前的等待,生产是 conflictPause;单测换成不等待的。
	pause func(ctx context.Context, attempt int) error

	// deployment 是第一次读成功的部署身份,由 deploymentMu 保护。Pod 不会换集群,进程活着它就不变。
	deploymentMu sync.Mutex
	deployment   string

	mu        sync.Mutex
	cur       block
	next      *block
	inflight  *reservation
	notBefore time.Time
	closed    bool
	// witnessHigh 是本进程确认过的水位,witnessAuthority 是它的世系,都由 mu 保护。只升不降,见 witness、checkRecords。
	witnessHigh      uint64
	witnessAuthority string
	// watching:作证检查已经起了,由 mu 保护。checkEvery 是它的间隔,0 = 不起(单测换成 0,直接调 checkRecords)。
	watching   bool
	checkEvery time.Duration

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
		opt:        opt,
		tags:       tags,
		base:       base,
		limit:      base + valueMask + 1,
		monoNow:    time.Now,
		pause:      conflictPause,
		checkEvery: recordCheckInterval,
		bg:         bg,
		cancel:     cancel,
	}, nil
}

// conflictPause 在一次 CAS 冲突之后、下一轮重读之前等一小会儿。attempt 是即将开始的轮次,从 1 起。
// 等待上限从 reserveConflictBackoffBase 起逐轮翻倍,封顶 reserveConflictBackoffMax;实际等待在 [0, 上限) 里均匀取。
//
// 为什么要等:同一个地域的全部 data-service 副本共用这三份记录。它们同时预留时(整地域冷启动),不等就会同步地
// 反复相撞:每撞一次,先写成功的那几份记录白白前进一块(只浪费值域,不会重号),reserveAttempts 轮很快用完。
// 随机错开之后,多数副本下一轮读到的就是别人提交完的视图。ctx 到期或发号器关闭时立即返回它的错误。
func conflictPause(ctx context.Context, attempt int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(mathrand.N(conflictBackoffLimit(attempt)))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// conflictBackoffLimit 是第 attempt 轮(从 1 起)开始之前等待的上限。
func conflictBackoffLimit(attempt int) time.Duration {
	// 移位封顶在 16:轮次再多,上限也早已被 reserveConflictBackoffMax 截住,不会溢出。
	return min(reserveConflictBackoffBase<<min(max(attempt-1, 0), 16), reserveConflictBackoffMax)
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
		} else {
			if i.cur.remaining() == 0 && i.next == nil {
				i.cur = b
			} else {
				i.next = &b
			}
			i.startWatchLocked()
		}
		res.err = err
		close(res.done)
		i.mu.Unlock()
	})
	return res
}

// startWatchLocked 在第一次预留成功之后起作证检查,之后每 checkEvery 跑一次 checkRecords,直到 Close。
// 只有发过号的进程才有东西可以作证,所以运维入口(初始化、恢复、退役)不会起它。
func (i *Issuer) startWatchLocked() {
	if i.watching || i.closed || i.checkEvery <= 0 {
		return
	}
	i.watching = true
	every := i.checkEvery
	i.wg.Add(1)
	safego.Go("id_issuer.witness", func() {
		defer i.wg.Done()
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-i.bg.Done():
				return
			case <-ticker.C:
			}
			ctx, cancel := context.WithTimeout(i.bg, reserveTimeout)
			err := i.checkRecords(ctx)
			cancel()
			if err != nil && i.bg.Err() == nil {
				logx.Errorf("[id-issuer] check the reservation records against this process: %v", err)
			}
		}
	})
}

// witness 记下本进程确认过的水位,只升不降,返回记下之后的值。确认过的意思是:三份记录都到过这个值。
// 来源有两个:本进程三份都写成功的块尾;作证检查读到的登记处水位(登记处最后写,读到的值另外两份也写到过)。
func (i *Issuer) witness(high uint64, authority string) uint64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	if high > i.witnessHigh {
		i.witnessHigh, i.witnessAuthority = high, authority
	}
	return i.witnessHigh
}

// witnessed 返回本进程确认过的水位和它的世系;还没确认过任何东西时是 0。
func (i *Issuer) witnessed() (uint64, string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.witnessHigh, i.witnessAuthority
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
//
// 循环里每一处 continue 都是 CAS 冲突(别的副本也在预留),下一轮开始前先错开一小会儿(pause)。
func (i *Issuer) reserve(ctx context.Context) (block, error) {
	deployment, err := i.deploymentID(ctx)
	if err != nil {
		i.observe(ReserveIdentityError)
		return block{}, err
	}
	for attempt := 0; attempt < reserveAttempts; attempt++ {
		if attempt > 0 {
			if err := i.pause(ctx, attempt); err != nil {
				return block{}, fmt.Errorf("wait before retrying a conflicting reservation: %w", err)
			}
		}
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
		ok, err := i.opt.Primary.CompareAndSwap(ctx, v.p, end, authority, deployment)
		if err != nil {
			i.observe(ReserveStoreError)
			return block{}, fmt.Errorf("write primary record: %w", err)
		}
		if !ok {
			i.observe(ReserveConflict)
			continue
		}
		ok, err = i.opt.Secondary.CompareAndSwap(ctx, v.s, end, authority, deployment)
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
		i.witness(end, authority)
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

// writtenHere:在的本地记录都是本部署环境写下的。只在登记处没有这个码时用(Recover):那时它是绑定唯一幸存的证据,
// 也说明这些记录不是从别的环境拷来的旧数据。没有身份的记录(dev 写的,或本字段出现之前写的)证明不了是谁的,不算。dev 不比。
func (i *Issuer) writtenHere(v view, deployment string) bool {
	if i.opt.AllowBootstrap {
		return true
	}
	for _, r := range []Record{v.p, v.s} {
		if r.Exists && r.Deployment != deployment {
			return false
		}
	}
	return deployment != ""
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

// nextStart 算下一块的起点:三份记录、本进程确认过的水位与时间下限的最大值。
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

	// 本进程确认过的水位:三份都低于它,只能是三份一起被回档或丢了(三处同时出事),这时记录自己证明不了什么。
	own, _ := i.witnessed()
	if own > max(local, v.g.HighWater) {
		i.observe(ReserveBehindIssuer)
		logx.Errorf("[id-issuer] all reservation records (primary=%d secondary=%d registry=%d) are below %d, which this process "+
			"already confirmed: they were restored from older data or lost together; continuing above it (docs/ops/id-issuer-runbook.md §4.1)",
			v.p.Value, v.s.Value, v.g.HighWater, own)
	}

	start := max(local, v.g.HighWater, i.base, own)
	// 时间下限永远生效,不因为跳得远就放弃:它只会把起点往上抬,不可能造成重号。记录落后时钟很多也是正常的:
	// 用量小的时候,一块号能用几个月。
	floor := i.timeFloor()
	if floor > start {
		start = floor
	} else if floor != 0 && (start-floor)>>seqBits > maxAheadSeconds {
		i.observe(ReserveAheadOfClock)
		logx.Errorf("[id-issuer] counter %d is far ahead of the clock (floor %d): the clock jumped forward at some point, blocks were "+
			"reserved faster than the clock moves (a crash loop), or the code was just re-registered after the id registry lost it; "+
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
			if errors.Is(err, ErrRegionClaimed) || errors.Is(err, ErrRegionOutOfOrder) ||
				errors.Is(err, ErrRegistryNotOpened) || errors.Is(err, ErrRegistryRebuilding) {
				return false, err
			}
			if err != nil {
				return false, fmt.Errorf("claim the code in the id registry (outcome unknown, rerun -id-issuer-init): %w", err)
			}
			if !ok {
				continue // 有人同时认领:重读,按已认领处理
			}
			if err := i.raiseLocal(ctx, v, claim.HighWater, authority, deployment); err != nil {
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
		if err := i.raiseLocal(ctx, v, v.g.HighWater, v.g.Authority, deployment); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, ErrContended
}

// raiseLocal 把 v 里的本地记录抬到 value:不存在的建出来,低于它的抬上去,写上世系和本部署环境的身份。已经不低于它的不碰。
// 只抬高,不会造成重号:记录越高,下一块的起点越高。写序同预留,主记录先、副记录后。任何一份写失败都返回错误,
// 重来是安全的:重读之后,已经够高的不会再写。
func (i *Issuer) raiseLocal(ctx context.Context, v view, value uint64, authority, deployment string) error {
	for _, r := range []struct {
		name  string
		store CounterStore
		rec   Record
	}{{"primary", i.opt.Primary, v.p}, {"secondary", i.opt.Secondary, v.s}} {
		if r.rec.Exists && r.rec.Value >= value {
			continue
		}
		ok, err := r.store.CompareAndSwap(ctx, r.rec, value, authority, deployment)
		if err != nil {
			return fmt.Errorf("write the %s record (outcome unknown, safe to retry): %w", r.name, err)
		}
		if !ok {
			return fmt.Errorf("%w: the %s record was written concurrently, safe to retry", ErrContended, r.name)
		}
	}
	return nil
}

// Recover 修复事故之后的记录。上界只取幸存的记录,拿不到可信上界就拒绝,从不猜起点。
// 只由运维显式调用(data_service -id-issuer-recover),运行路径不走这里。返回做了什么,给命令打印。
//
//   - 登记处没有这个码,本地还有记录:登记处丢了数据。重新登记,绑定本部署环境和这两套 etcd,再把本地两份抬到新水位。
//     绑定的证据这时只剩本地记录里的部署身份:在的本地记录都是本部署环境写下的才重新登记(writtenHere)。
//     不是(别的环境的 etcd 数据拷到了这里,或者记录没有身份):ErrForeignRecords,rebind 也不放行。拷来的记录可能比
//     原环境发过的号旧;让它重新登记,原环境会被挡在外面,而这里从过期水位往上发的号会和原环境发过的重叠。
//     登记处在的时候 rebind 是安全的,因为水位由登记处盖住;这里没有登记处,过期的水位没有任何东西纠正。
//     水位不只取本地记录,还要盖住时钟推得出的上界(reRegisterHighWater):本地记录自己也可能刚被回档。
//     本地也一份不剩:ErrNoTrustedBound,这个码不能再用。
//   - 登记处有,本地两份都没了:按登记处水位重建两份本地记录。两份都没了就证明不了这两套 etcd 还是原来那两套,
//     所以要 rebind;同一个部署环境里认领之后从没发过号的码(没做完的初始化)例外。
//   - 部署环境的身份或两套 etcd 的集群号和登记处绑的不一致(灾备到了另一个 K8s 集群,或 etcd 换成了新集群):
//     要 rebind,把绑定改到本部署环境和这两套 etcd。
//   - 本地只缺一份、本地落后登记处、两份本地记录不相等:把本地两份抬到三份里的最高值。不跑也行,下一次预留自动对齐;
//     重新登记之后抬本地记录中断了,重跑一次也走这里做完。登记处落后本地:不用恢复,下一次预留把它抬上去。
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
			if !i.writtenHere(v, deployment) {
				return "", fmt.Errorf("%w: the records were written by deployment %q (primary) and %q (secondary), this is deployment %q",
					ErrForeignRecords, v.p.Deployment, v.s.Deployment, deployment)
			}
			local := max(v.p.Value, v.s.Value)
			high, err := i.reRegisterHighWater(local)
			if err != nil {
				return "", err
			}
			// Blocks 记不回来,写 1:非 0 表示这个码发过号,Init 不会把它当成没做完的初始化。
			claim := Claim{State: ClaimActive, Authority: authority, Deployment: deployment, PrimaryCluster: v.p.ClusterID,
				SecondaryCluster: v.s.ClusterID, HighWater: high, Blocks: 1}
			ok, err := i.opt.Registry.CompareAndSwap(ctx, i.opt.Region, v.g, claim)
			if err != nil {
				return "", fmt.Errorf("re-register the code (outcome unknown, rerun -id-issuer-recover): %w", err)
			}
			if !ok {
				continue
			}
			// 本地两份留在低处也不会重号(下一块从登记处水位之上开始),但下一次预留会把它误报成本地两套被回档。
			if err := i.raiseLocal(ctx, v, high, authority, deployment); err != nil {
				return "", fmt.Errorf("re-registered region %d at high_water=%d but raising the local records to it failed; "+
					"rerun -id-issuer-recover to finish: %w", i.opt.Region, high, err)
			}
			return fmt.Sprintf("re-registered region %d in the id registry for deployment %q at high_water=%d and raised the local "+
				"records to it (they were at %d; the clock bound covers ids issued up to %d days ahead of the clock)",
				i.opt.Region, deployment, high, local, maxAheadSeconds/(24*3600)), nil
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
			high := max(v.g.HighWater, v.p.Value, v.s.Value)
			if v.p.Exists && v.p.Value >= high && v.s.Exists && v.s.Value >= high {
				return "nothing to repair: both local records are at the highest surviving record", nil
			}
			if err := i.raiseLocal(ctx, v, high, v.g.Authority, deployment); err != nil {
				return "", err
			}
			return fmt.Sprintf("raised the local records of region %d to %d, the highest surviving record", i.opt.Region, high), nil
		}
		if bound && v.g.Blocks == 0 {
			if err := i.raiseLocal(ctx, v, v.g.HighWater, v.g.Authority, deployment); err != nil {
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
		if err := i.raiseLocal(ctx, v, claim.HighWater, claim.Authority, deployment); err != nil {
			return "", err
		}
		return fmt.Sprintf("bound region %d to deployment %q and etcd clusters (%x, %x) above high_water=%d",
			i.opt.Region, claim.Deployment, claim.PrimaryCluster, claim.SecondaryCluster, claim.HighWater), nil
	}
	return "", ErrContended
}

// reRegisterHighWater 是登记处丢了这个码、凭本地记录重新登记时的水位。
//
// 本地记录这时是唯一幸存的上界,但它自己证明不了没被回档:同一个 K8s 集群里本地两套 etcd 被恢复到旧快照、登记处又同时
// 丢了这个码(三处同时出事),本地记录就比发过的号旧。所以水位还要盖住时钟推得出的上界:每块的起点比当时的时间下限超前
// 超过 maxAheadSeconds 就会告警(ahead_of_clock);没告警过,发过的号就都在「当时的时间下限 + maxAheadSeconds + 一块」之下,
// 而当时的时间下限不超过现在的。代价是计数器一次跳到时钟前面 7 天,值域浪费可以忽略(每个地域码约 2^57 个号)。
// 活着的发号进程还会先把本地记录抬到它们确认过的水位(checkRecords),所以多数情况下本地记录本身就够高。
//
// 拒绝的两种情形,都什么也不写:
//   - 本地记录比这个上界还高:计数器曾经超前时钟超过 maxAheadSeconds,上面的推理不成立;它要是也被回档过,没有任何东西
//     盖得住,返回 ErrNoTrustedBound(换码,运维手册 §4.4)。
//   - 时钟不在可表示的范围内:修好时钟再跑。
//
// 查不出来的残余:时钟比当初预留时还慢,或者计数器曾经超前时钟 7 天以上、之后又回落到上界以下,而这期间三份记录一起回档、
// 没有活着的进程作证。见设计 §6.3「还会不会重号」。
func (i *Issuer) reRegisterHighWater(local uint64) (uint64, error) {
	floor := i.timeFloor()
	if floor == 0 {
		return 0, fmt.Errorf("id issuer: the clock %v is outside the representable range; re-registering needs it to bound the ids "+
			"issued after the local records were written, fix the clock and rerun -id-issuer-recover", i.opt.Now())
	}
	bound := floor + (maxAheadSeconds+1)<<seqBits + i.opt.BlockSize
	if bound >= i.limit {
		return 0, ErrExhausted
	}
	if local > bound {
		return 0, fmt.Errorf("%w: the local records (%d) are more than %d days ahead of the clock (bound %d), so the clock cannot "+
			"show they were not restored from older data", ErrNoTrustedBound, local, maxAheadSeconds/(24*3600), bound)
	}
	return bound, nil
}

// checkRecords 是作证检查:活着的进程给三份记录作证。本进程确认过的水位 W(witness)以下的号可能已经发出去了,而三份记录
// 只增不减,任何一份低于 W(不存在算 0)都只能是被回档或丢了。这时把低的抬回 W,写序同预留:主记录、副记录、登记处。
//
// 为什么要它:三份记录一起回档或丢失时(同一个 K8s 集群里本地两套 etcd 被恢复到旧快照,登记处同时回档或丢了这个码),
// 记录自己什么也证明不了;但发号进程这时多半还活着,W 在它的内存里。抬回去之后,别的副本的下一块、Recover 的重新登记,
// 都从 W 之上开始。
//   - 登记处丢了这个码:只抬本地两份,不替它建登记处的记录(那是 Recover 的事,要运维看过)。本地两份这时是重新登记
//     唯一的上界,抬高才盖得住本进程确认过的块。
//   - 码已退役、绑定转到了别处(rebind)、世系对不上:不归本进程管,什么也不做,交给预留和 Recover 去拒绝。
//
// 只抬高、不预留新块,和并发的预留冲突了也无害:这一轮到此为止,下一轮重读再看。本进程正在预留时跳过这一轮:
// 预留本身就从 W 之上开始(nextStart)。W 每轮都跟上读到的登记处水位,所以至多落后一个检查间隔:别的副本恰好在这段
// 时间里预留、随即连同三份记录一起丢掉的块,这里盖不住。
func (i *Issuer) checkRecords(ctx context.Context) error {
	high, authority := i.witnessed()
	if high == 0 {
		return nil
	}
	i.mu.Lock()
	reserving := i.inflight != nil
	i.mu.Unlock()
	if reserving {
		return nil
	}
	deployment, err := i.deploymentID(ctx)
	if err != nil {
		return err
	}
	v, err := i.read(ctx)
	if err != nil {
		return err
	}
	if err := i.requireIndependent(v); err != nil {
		return err
	}
	if err := i.checkRegion(v); err != nil {
		return err
	}
	for _, r := range []Record{v.p, v.s} {
		if r.Exists && r.Authority != authority {
			return nil
		}
	}
	if v.g.Exists {
		if v.g.State != ClaimActive || v.g.Authority != authority || !i.boundHere(v, deployment) {
			return nil
		}
		high = i.witness(v.g.HighWater, authority)
	}
	registryBehind := v.g.Exists && v.g.HighWater < high
	if v.p.Exists && v.p.Value >= high && v.s.Exists && v.s.Value >= high && !registryBehind {
		return nil
	}
	i.observe(ReserveBehindIssuer)
	logx.Errorf("[id-issuer] reservation records are below %d, which this process already confirmed (primary=%d exists=%v, "+
		"secondary=%d exists=%v, registry=%d exists=%v): they were restored from older data or lost; raising them back "+
		"(docs/ops/id-issuer-runbook.md §4.1)", high, v.p.Value, v.p.Exists, v.s.Value, v.s.Exists, v.g.HighWater, v.g.Exists)
	if err := i.raiseLocal(ctx, v, high, authority, deployment); err != nil {
		return err
	}
	if !registryBehind {
		return nil
	}
	claim := v.g.Claim
	claim.HighWater = high
	if _, err := i.opt.Registry.CompareAndSwap(ctx, i.opt.Region, v.g, claim); err != nil {
		return fmt.Errorf("raise the id registry to %d (outcome unknown, safe to retry): %w", high, err)
	}
	return nil
}

// Retire 让这个地域码永久退役:登记处留墓碑,之后预留、初始化、恢复全部拒绝,码永不再分配。
// 登记处没有这个码时也写墓碑:登记处重建时按台账补回退役码,防止它被当成没用过的码重新认领。
// 这样的墓碑要有「下一个可认领的码」对照(开放之后,或者 -id-registry-rebuild 声明重建之后),没有时返回 ErrRegistryNotOpened、
// 不写;只能补给用过的码,或者恰好是下一个可认领的码(跳过它);更大的码返回 ErrRegionOutOfOrder、不写,
// 规则在 Registry.CompareAndSwap 的契约里。
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
		if errors.Is(err, ErrRegionOutOfOrder) || errors.Is(err, ErrRegistryNotOpened) {
			return false, err // 确定的拒绝:没有指针可以对照,或者这个码没认领过、也不是下一个可认领的码,什么都没写
		}
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

// OpenRegistry 开放认领,next 是第一个允许 -id-issuer-init 认领的码。只在两种时候由运维显式调用
// (data_service -id-registry-open):世界第一次部署,next=1;登记处整个丢了数据、重建做完时,next 等于登记处里
// 重建中的那个值(运维手册 §4.3)。已开放时什么也不做,返回 false。
//
// 这一步把「新世界」和「丢了数据的旧世界」分开:没开放时,初始化一律拒绝,不会把用过的码当成没用过。
// 有地域记录却没有指针的登记处是丢过数据的,拒绝直接开放(ErrRegistryNeedsRebuild),要先 BeginRegistryRebuild。
func (i *Issuer) OpenRegistry(ctx context.Context, next uint32) (bool, error) {
	if next > MaxNextRegion {
		return false, fmt.Errorf("id issuer: next region %d exceeds %d", next, MaxNextRegion)
	}
	return i.opt.Registry.Open(ctx, next)
}

// BeginRegistryRebuild 在登记处整个丢了数据之后声明下一个可认领的码,next 取地域码台账里用过的最大码 +1
// (data_service -id-registry-rebuild,运维手册 §4.3)。已经声明过(重建中或已开放)时什么也不做,返回 false。
//
// 重建期间:在用的地域照常 Recover 重新登记(码不小于指针时把它推过去,说明台账漏记了它);退役的码按指针核对补墓碑;
// 初始化一律拒绝(ErrRegistryRebuilding),免得新地域认领到一个台账漏记、又还没重新登记回来的码。全部做完之后
// OpenRegistry 开放认领。先声明指针再补墓碑,是因为没有指针时补墓碑没有东西可以对照:填错的码也会写进去,之后声明指针
// 就被它卡住。
func (i *Issuer) BeginRegistryRebuild(ctx context.Context, next uint32) (bool, error) {
	if next > MaxNextRegion {
		return false, fmt.Errorf("id issuer: next region %d exceeds %d", next, MaxNextRegion)
	}
	return i.opt.Registry.BeginRebuild(ctx, next)
}

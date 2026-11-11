// Package createbudget 是 CreatePlayer 持锁后串行几跳跨进程调用的截止分摊
// (docs/design/client-request-ordering.md §5.3、§6.1 订正注的「子预算未重分」)。
//
// 做法是截止传播加下游预留,不是把各跳的固定超时相加(gRPC 的截止传播;Google SRE 书
// 「Addressing Cascading Failures」一章的 deadline propagation;AWS Builders' Library
// 「Timeouts, retries, and backoff with jitter」):
//
//	第 i 跳的截止 = min(开始时刻 + 本跳上限, handler 截止 − 余量_i)
//	余量_i       = Σ_{j>i}(第 j 跳下限 + HopGap)
//
// 剩下的时间连本跳下限都不够,这一跳就不发(一个写都不发出去),由调用方走「放弃建角」的补偿路径。
// handler 截止来自 go-zero 超时拦截器(zrpc Timeout,4000)或上游传播的截止,两者取早。
//
// 为什么要按截止分摊:原先各跳自带固定超时(发号 3s、名字 3s、player_route 3s),名义之和 9s 远超 4s 的
// handler 预算,实际被 handler ctx 截在 4s,截止之后也不再发写(createplayerlogic.go 6c'');但前面一跳慢了,
// 后面的写照样带着所剩无几的时间发出去,多半以「结果未知」告终,还要补偿释放名字、留下 player_route 幽灵映射。
// 按截止分摊之后,一次写只在拿得满它的下限时才发,建角的提交点(SQL 账号目录)总能拿满自己的上限。
//
// 下限的取法:
//   - 名字登记 = 单次 ReservePlayerName 的预算(playernamereg.DefaultReserveTimeout):第一次登记一定拿满单次预算;
//     同名重试仍由「名字这一跳剩下的 ≥ 单次预算」把关(createplayerlogic.go tryReserveName),语义不变;
//   - player_route 登记、SQL 账号目录提交 = 各自的上限:单条写 RPC 拿不满它的预算就不发,
//     「围栏写」(提交点)一定拿满上限;
//   - 发号 = 0(只要求剩下的时间为正):发号对调用方不是写,等号段超时只烧掉号,不留状态。
//
// 上限的取法见各常量;只有 player_route 登记的上限来自配置(HomeZone.RegisterTimeout)。
// 启动门禁 Validate 要求「本地余量 + 各跳上限之和 + 跳间间隙 ≤ zrpc Timeout」:满足它,前置步骤不超过
// LocalAllowance、跳间不超过 HopGap 时每一跳都拿满自己的上限;超出时由 Derive 在发出之前截短或放弃。
//
// 线程模型:Plan 是值类型、只读,可在多个 goroutine 间共享。
package createbudget

import (
	"context"
	"fmt"
	"time"

	"login/internal/logic/pkg/homezone"
	"login/internal/logic/pkg/playernamereg"
)

// Hop 是 CreatePlayer 持锁后按顺序串行的一跳。顺序就是枚举顺序,余量按它累加。
type Hop uint8

const (
	// HopMint:发号(data_service AllocateIdSegment)。常态命中本地号段、不出进程;两段都用完时等后台续段。
	HopMint Hop = iota
	// HopName:名字登记(ReservePlayerName,一次 + 结果未知时同名重试一次;生成名撞名换名重试)。
	HopName
	// HopRegister:player_route 归属与落点登记(RegisterPlayerZone)。
	HopRegister
	// HopCommit:SQL 账号目录提交(CreateAccountRole,含新号初始原件),建角的提交点。
	HopCommit
	// hopCount 固定为最后一项,用作各张表的长度。
	hopCount
)

// hopNames 与 Hop 一一对应,只进日志与错误文案。长度由数组类型 [hopCount] 钉住,budget_test 另查每项非空。
var hopNames = [hopCount]string{
	HopMint:     "mint",
	HopName:     "name",
	HopRegister: "register",
	HopCommit:   "commit",
}

func (h Hop) String() string {
	if h < hopCount {
		return hopNames[h]
	}
	return fmt.Sprintf("hop(%d)", uint8(h))
}

const (
	// LocalAllowance 是四跳之外、handler 起算到发号之前的本地与读步骤的余量:读本连接登录态、拿建角锁(两次 Redis)、
	// 读 SQL 账号目录(一次 data_service 读)、配表与名字预检,外加提交前复核建角锁的那一次 Redis GET 之前的耗时。
	// 常态合计十几毫秒;超出时 Derive 先截短发号、再放弃,不会挤掉后面写的下限。
	LocalAllowance = 250 * time.Millisecond
	// HopGap 是相邻两跳之间留给本地步骤的间隙(日志、构造角色、提交前复核建角锁的 Redis GET)。
	// 余量里每个后续跳都多留一个 HopGap:否则前一跳恰好用到自己的截止时,后一跳只差几微秒就够不着下限,白白放弃。
	HopGap = 50 * time.Millisecond

	// MintCap 是发号这一跳的上限。号段客户端在本地号段里直接出号;两段都用完时才等后台续段
	// (shared/idsegment,续段自带 3s FetchTimeout、不随本次请求取消)。等不到就本次建角失败、客户端重试,
	// 届时续段多半已落地、立即出号。
	MintCap = 300 * time.Millisecond
	// nameRetryGap 是名字这一跳里「第一次登记超时返回」到「判是否同名重试」之间的调度余量。
	nameRetryGap = 100 * time.Millisecond
	// NameCap 是名字这一跳的上限:放得下一次登记 + 结果未知时的同名重试一次(各一个单次预算),再加调度余量。
	// 少了这 100ms,第一次登记恰好超时返回时剩下的就不足一个单次预算,同名重试的门槛永远过不去。
	NameCap = 2*playernamereg.DefaultReserveTimeout + nameRetryGap
	// CommitCap 是 SQL 账号目录提交这一跳的上限,与单条写 RPC 同档。shared/accountdirectory 的客户端另套 2.5s,
	// 实际取两者较早的。提交结果未知时的强读回走独立 ctx(createplayerlogic.go persistAccountRole),不占本预算。
	CommitCap = 800 * time.Millisecond
)

// limit 是一跳的上限与下限。
type limit struct {
	cap   time.Duration // 本跳最多能拿到的时长
	floor time.Duration // 本跳至少要拿到这么多才发;0 = 只要求为正
}

// Plan 是一次建角各跳的上限与下限。零值不可用,用 New 构造。
type Plan struct {
	hops [hopCount]limit
}

// New 按 player_route 登记的上限(HomeZone.RegisterTimeout)构造分摊方案;registerTimeout ≤ 0 取
// homezone.DefaultRegisterTimeout,与 homezone.Resolver 的缺省一致。
func New(registerTimeout time.Duration) Plan {
	if registerTimeout <= 0 {
		registerTimeout = homezone.DefaultRegisterTimeout
	}
	return Plan{hops: [hopCount]limit{
		HopMint:     {cap: MintCap, floor: 0},
		HopName:     {cap: NameCap, floor: playernamereg.DefaultReserveTimeout},
		HopRegister: {cap: registerTimeout, floor: registerTimeout},
		HopCommit:   {cap: CommitCap, floor: CommitCap},
	}}
}

// Cap 返回第 hop 跳的上限。
func (p Plan) Cap(hop Hop) time.Duration { return p.hops[hop].cap }

// Floor 返回第 hop 跳的下限。
func (p Plan) Floor(hop Hop) time.Duration { return p.hops[hop].floor }

// ReserveAfter 返回第 hop 跳必须给后面各跳留的余量:Σ_{j>hop}(下限_j + HopGap)。最后一跳为 0。
func (p Plan) ReserveAfter(hop Hop) time.Duration {
	var reserve time.Duration
	for j := hop + 1; j < hopCount; j++ {
		reserve += p.hops[j].floor + HopGap
	}
	return reserve
}

// Required 返回启动门禁要求 zrpc Timeout 至少有的时长:本地余量 + 各跳上限之和 + 跳间间隙。
func (p Plan) Required() time.Duration {
	total := LocalAllowance + HopGap*time.Duration(hopCount-1)
	for _, l := range p.hops {
		total += l.cap
	}
	return total
}

// Validate 是 login 的启动门禁:serverTimeout(zrpc Timeout)放不下 Required() 就拒绝启动。
// 也拒绝没有上限、下限高于上限的跳 —— 那样的跳永远发不出去,建角必然失败。
func (p Plan) Validate(serverTimeout time.Duration) error {
	if serverTimeout <= 0 {
		return fmt.Errorf("zrpc Timeout must be positive: CreatePlayer derives every hop deadline from the handler deadline")
	}
	for h := Hop(0); h < hopCount; h++ {
		l := p.hops[h]
		if l.cap <= 0 {
			return fmt.Errorf("CreatePlayer hop %s has no upper bound", h)
		}
		if l.floor > l.cap {
			return fmt.Errorf("CreatePlayer hop %s minimum %s exceeds its upper bound %s", h, l.floor, l.cap)
		}
	}
	if need := p.Required(); need > serverTimeout {
		return fmt.Errorf("CreatePlayer hop budgets need %s but zrpc Timeout is %s: local allowance %s + mint %s + name %s + "+
			"register %s + commit %s + %d hop gaps of %s; lower HomeZone.RegisterTimeout or raise Timeout within the router "+
			"forward budget (docs/design/client-request-ordering.md §5.2 I1)",
			need, serverTimeout, LocalAllowance, p.hops[HopMint].cap, p.hops[HopName].cap,
			p.hops[HopRegister].cap, p.hops[HopCommit].cap, hopCount-1, HopGap)
	}
	return nil
}

// Window 计算第 hop 跳的截止:min(now + 上限, handlerDeadline − ReserveAfter(hop))。
// 截止 − now 不到本跳下限(下限为 0 时不为正)时 ok = false,available 是本可拿到的时长(可为负)。
// hasDeadline = false(handler ctx 没有截止,只在单测与离线工具里)时只按上限截、总是 ok。
// 返回的截止一定不晚于 handlerDeadline。
func (p Plan) Window(hop Hop, now, handlerDeadline time.Time, hasDeadline bool) (end time.Time, available time.Duration, ok bool) {
	l := p.hops[hop]
	end = now.Add(l.cap)
	if hasDeadline {
		if latest := handlerDeadline.Add(-p.ReserveAfter(hop)); latest.Before(end) {
			end = latest
		}
	}
	available = end.Sub(now)
	return end, available, available > 0 && available >= l.floor
}

// ShortBudgetError 表示剩下的预算不够这一跳的下限,调用方没有发这一跳。
type ShortBudgetError struct {
	Hop       Hop
	Available time.Duration // 本可拿到的时长(可为负)
	Need      time.Duration // 本跳下限
	Reserve   time.Duration // 给后面各跳留的余量
}

func (e *ShortBudgetError) Error() string {
	return fmt.Sprintf("CreatePlayer %s hop skipped: %s available after reserving %s for later hops, needs %s",
		e.Hop, e.Available, e.Reserve, e.Need)
}

// Derive 从 handler ctx 派生第 hop 跳的 ctx,now 是此刻(由调用方给,单测可注入)。
// parent 已结束时返回 parent.Err();剩下的不够本跳下限时返回 *ShortBudgetError —— 两种情况都不该发这一跳。
// 成功时返回的 ctx 截止 = Window 的截止(不晚于 parent 的截止),调用方用完即 cancel。
func (p Plan) Derive(parent context.Context, hop Hop, now time.Time) (context.Context, context.CancelFunc, error) {
	if err := parent.Err(); err != nil {
		return nil, nil, err
	}
	deadline, hasDeadline := parent.Deadline()
	end, available, ok := p.Window(hop, now, deadline, hasDeadline)
	if !ok {
		return nil, nil, &ShortBudgetError{Hop: hop, Available: available, Need: p.hops[hop].floor, Reserve: p.ReserveAfter(hop)}
	}
	ctx, cancel := context.WithDeadline(parent, end)
	return ctx, cancel, nil
}

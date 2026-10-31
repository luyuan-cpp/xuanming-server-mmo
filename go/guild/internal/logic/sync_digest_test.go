package logic

// 落地恢复摘要 bump 的单测(docs/design/landing-recovery-sync-digest.md §3.3 / §3.5;不连库)。
//
// 两层分开测:
//   - AsyncGuildSyncBumper(push.go):异步、整批一个预算、去重丢 0、不别名调用方切片。替身有两种:
//     fakePlayerBumper 站在 *playersync.Bumper 的位置上(看批次与预算);fakeDigestEvaler 站在 playersync.Evaler
//     的位置上、下面接真的 playersync.Bumper(看每条 Lua 的键与域号、某人失败不中断整批)。
//     预算上下文与起协程都经字段注入(budgetRecorder / heldLauncher):断言比对固定的截止时间、同步数协程,
//     不与真实墙钟比较,也不靠调度顺序(AGENTS §11.4)。
//   - logic 的 bump 点:受影响玩家与域对不对、提交之前的拒绝不 bump、bump 失败不影响 RPC 结果。
//     替身是 recordingSyncBumper(GuildSyncBumper 的同步记录器,同 recordingNotifier 的套路)。
//
// 管理类写(踢人 / 离帮 / 审批 …)与捐献 / 兑换的预留要 MySQL,完整流程在 economy_flow_integration_test.go
// 的 *SyncDigest* 用例(build tag integration);这里覆盖它们共用的纯函数与不碰库的那一段。

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"guild/internal/constants"
	"guild/internal/data"
	battlepb "proto/battle"
	pb "proto/guild"
	scenepb "proto/scene"
	"shared/assetop"
	"shared/playersync"
)

const (
	syncGuild       = scenepb.SyncDomain_SYNC_DOMAIN_GUILD
	syncApplication = scenepb.SyncDomain_SYNC_DOMAIN_GUILD_APPLICATION
	syncEconomy     = scenepb.SyncDomain_SYNC_DOMAIN_GUILD_ECONOMY
	syncActivity    = scenepb.SyncDomain_SYNC_DOMAIN_GUILD_ACTIVITY
)

// ── 测试替身 ──────────────────────────────────────────────────

// recordedBump 是一次 GuildSyncBumper.Bump 调用的原样记录。
type recordedBump struct {
	players []uint64
	domains []scenepb.SyncDomain
}

// recordingSyncBumper 记下每一次 bump。加锁:结算与重投循环的回调可能在别的协程里调用它。
type recordingSyncBumper struct {
	mu    sync.Mutex
	calls []recordedBump
}

func (b *recordingSyncBumper) Bump(players []uint64, domains ...scenepb.SyncDomain) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, recordedBump{players: slices.Clone(players), domains: slices.Clone(domains)})
}

func (b *recordingSyncBumper) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

// countOf 返回含 domain 的调用次数。
func (b *recordingSyncBumper) countOf(domain scenepb.SyncDomain) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, c := range b.calls {
		if slices.Contains(c.domains, domain) {
			n++
		}
	}
	return n
}

func (b *recordingSyncBumper) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = nil
}

// playersOf 返回被 bump 过 domain 的玩家,按 GuildSyncBumper 的契约去重、丢 0,升序。
func (b *recordingSyncBumper) playersOf(domain scenepb.SyncDomain) []uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []uint64{}
	for _, c := range b.calls {
		if !slices.Contains(c.domains, domain) {
			continue
		}
		for _, id := range c.players {
			if id != 0 && !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	slices.Sort(out)
	return out
}

// domainsOf 返回 playerID 被 bump 过的域,去重、升序。
func (b *recordingSyncBumper) domainsOf(playerID uint64) []scenepb.SyncDomain {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []scenepb.SyncDomain{}
	for _, c := range b.calls {
		if !slices.Contains(c.players, playerID) {
			continue
		}
		for _, d := range c.domains {
			if !slices.Contains(out, d) {
				out = append(out, d)
			}
		}
	}
	slices.Sort(out)
	return out
}

// playerBumpBatch 是 PlayerSyncBumper.BumpAll 收到的一批。
type playerBumpBatch struct {
	players     []uint64
	domains     []scenepb.SyncDomain
	hasDeadline bool
	deadline    time.Time
}

// fakePlayerBumper 站在 *playersync.Bumper 的位置上,只看批次与预算。
// gate 非 nil 时记下批次后阻塞,直到 gate 被测试关闭(放行)或 ctx 结束(预算到期),模拟 Redis 卡住;
// entered 非 nil 时每进入一次 BumpAll 发一个信号(带缓冲,不阻塞被测协程)。两者构造时设好、之后只读。
// batches 由被测协程写,读一律经 snapshot。
type fakePlayerBumper struct {
	gate    <-chan struct{}
	entered chan struct{}

	mu      sync.Mutex
	batches []playerBumpBatch
}

func (f *fakePlayerBumper) BumpAll(ctx context.Context, playerIDs []uint64, domains ...scenepb.SyncDomain) error {
	deadline, ok := ctx.Deadline()
	f.mu.Lock()
	f.batches = append(f.batches, playerBumpBatch{
		players: slices.Clone(playerIDs), domains: slices.Clone(domains), hasDeadline: ok, deadline: deadline,
	})
	f.mu.Unlock()
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.gate == nil {
		return nil
	}
	select {
	case <-f.gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakePlayerBumper) snapshot() []playerBumpBatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.batches)
}

// fixedBudgetCtx 是截止时间固定、何时结束由测试决定的上下文:断言直接比对截止时间,不与真实墙钟比较。
// 截止时间只是一个被比对的值,Done 不会因真实时间到达而关闭,只在 expire 时关闭。
type fixedBudgetCtx struct {
	// 内嵌 Background,只借它的 Value;Deadline / Done / Err 由下面的方法覆盖。
	context.Context
	deadline time.Time
	done     chan struct{}
	once     sync.Once
}

func newFixedBudgetCtx(deadline time.Time) *fixedBudgetCtx {
	return &fixedBudgetCtx{Context: context.Background(), deadline: deadline, done: make(chan struct{})}
}

func (c *fixedBudgetCtx) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *fixedBudgetCtx) Done() <-chan struct{}       { return c.done }

func (c *fixedBudgetCtx) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// expire 模拟预算到期(幂等)。
func (c *fixedBudgetCtx) expire() { c.once.Do(func() { close(c.done) }) }

// fixedBudgetDeadline 是 fixedBudgetCtx 的截止时间。取一个固定值只为可比对;fixedBudgetCtx 不会因它到达而结束。
var fixedBudgetDeadline = time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)

// budgetRecorder 是 AsyncGuildSyncBumper.withBudget 的替身:记下每批请求的预算,交出同一个 fixedBudgetCtx,
// 并数 cancel 次数(每批结束后都必须释放预算上下文)。
type budgetRecorder struct {
	ctx *fixedBudgetCtx

	mu      sync.Mutex
	asked   []time.Duration
	cancels int
}

func newBudgetRecorder() *budgetRecorder {
	return &budgetRecorder{ctx: newFixedBudgetCtx(fixedBudgetDeadline)}
}

func (r *budgetRecorder) withBudget(budget time.Duration) (context.Context, context.CancelFunc) {
	r.mu.Lock()
	r.asked = append(r.asked, budget)
	r.mu.Unlock()
	return r.ctx, func() {
		r.mu.Lock()
		r.cancels++
		r.mu.Unlock()
	}
}

func (r *budgetRecorder) snapshot() ([]time.Duration, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.asked), r.cancels
}

// heldLauncher 是 AsyncGuildSyncBumper.launch 的替身:只把交出的批次存起来,由测试决定何时执行。
// "起没起协程"因此可以同步地数;"调用方返回后复用切片"也能在批次执行**之前**做完,不靠调度顺序。
type heldLauncher struct {
	mu       sync.Mutex
	launched int
	held     []func()
}

func (h *heldLauncher) launch(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.launched++
	h.held = append(h.held, fn)
}

func (h *heldLauncher) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.launched
}

// runAll 在测试协程里依次执行已交出、尚未执行的批次。
func (h *heldLauncher) runAll() {
	h.mu.Lock()
	held := h.held
	h.held = nil
	h.mu.Unlock()
	for _, fn := range held {
		fn()
	}
}

// hangGuard 只防测试挂死,不参与判定:被等待的事件在正确实现下必然发生,这个上限只在实现回归成阻塞时才会触发。
const hangGuard = 10 * time.Second

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(hangGuard):
		t.Fatalf("等待「%s」超过 %s:实现阻塞了,或丢了信号(这个上限只防挂死)", what, hangGuard)
	}
}

// digestEvalCall 是 playersync.Evaler 收到的一条 Lua。
type digestEvalCall struct {
	keys        []string
	args        []string
	hasDeadline bool
}

// fakeDigestEvaler 站在 playersync.Evaler 的位置上(下面接真的 playersync.Bumper),记录每条 Lua 的键与参数。
// fail 非 nil 时按键答复(构造时设好、之后只读)。
type fakeDigestEvaler struct {
	fail func(key string) error

	mu    sync.Mutex
	calls []digestEvalCall
}

func (e *fakeDigestEvaler) Eval(ctx context.Context, _ string, keys []string, args ...string) error {
	_, ok := ctx.Deadline()
	e.mu.Lock()
	e.calls = append(e.calls, digestEvalCall{keys: slices.Clone(keys), args: slices.Clone(args), hasDeadline: ok})
	e.mu.Unlock()
	if e.fail != nil && len(keys) > 0 {
		return e.fail(keys[0])
	}
	return nil
}

func (e *fakeDigestEvaler) snapshot() []digestEvalCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.calls)
}

func errRedisDown(string) error { return errors.New("dial tcp 127.0.0.1:6379: connection refused") }

// ── AsyncGuildSyncBumper ──────────────────────────────────────

// TestNewGuildSyncBumperNilIsNoop:没接摘要 Redis 时退回 Noop(与 NewGuildNotifier 同一收口),
// 接上时预算取设计 §3.5 第 6 条的 2 秒。
func TestNewGuildSyncBumperNilIsNoop(t *testing.T) {
	assert.IsType(t, NoopSyncBumper{}, NewGuildSyncBumper(nil))
	assert.NotPanics(t, func() { NoopSyncBumper{}.Bump([]uint64{42}, syncGuild) })

	b, ok := NewGuildSyncBumper(&fakePlayerBumper{}).(*AsyncGuildSyncBumper)
	require.True(t, ok)
	assert.Equal(t, 2*time.Second, b.budget)
	assert.NotNil(t, b.withBudget, "生产装配必须带预算上下文工厂")
	assert.NotNil(t, b.launch, "生产装配必须带协程启动器")

	// 生产的预算上下文:有截止时间,刚建好时未结束。预算取一小时只为让"未结束"与调度无关。
	ctx, cancel := backgroundWithTimeout(time.Hour)
	defer cancel()
	_, hasDeadline := ctx.Deadline()
	assert.True(t, hasDeadline, "整批必须有预算,Redis 卡住时不能无限期占着协程")
	assert.NoError(t, ctx.Err())
}

// TestAsyncGuildSyncBumperFansOutToWholeGuild:全帮扇出(上限 100 人)是**一批**、一个协程、同一组域;
// 0 与重复在交出前丢掉;这一批拿到的是按 guildSyncBumpBudget 新建的预算上下文(不继承请求 ctx),结束后释放;
// 调用方在 Bump 返回后复用切片,不影响已交出、尚未执行的批次。
func TestAsyncGuildSyncBumperFansOutToWholeGuild(t *testing.T) {
	inner := &fakePlayerBumper{}
	budget := newBudgetRecorder()
	launcher := &heldLauncher{}
	b := &AsyncGuildSyncBumper{inner: inner, budget: guildSyncBumpBudget, withBudget: budget.withBudget, launch: launcher.launch}
	players := make([]uint64, 0, constants.MaxGuildMembersCap+2)
	for id := uint64(1); id <= constants.MaxGuildMembersCap; id++ {
		players = append(players, id)
	}
	players = append(players, 0, 1)
	domains := []scenepb.SyncDomain{syncGuild, syncApplication}

	b.Bump(players, domains...)
	require.Equal(t, 1, launcher.count(), "全帮扇出是一批、一个协程")
	// 调用方复用切片(快照成员、可变参数的底层数组都可能被复用)。批次此刻还没执行,执行时不得看到这些改动。
	for i := range players {
		players[i] = 999_999
	}
	domains[0] = scenepb.SyncDomain_SYNC_DOMAIN_UNSPECIFIED
	launcher.runAll()

	batches := inner.snapshot()
	require.Len(t, batches, 1)
	batch := batches[0]
	want := make([]uint64, 0, constants.MaxGuildMembersCap)
	for id := uint64(1); id <= constants.MaxGuildMembersCap; id++ {
		want = append(want, id)
	}
	assert.Equal(t, want, batch.players, "全帮每人恰好一次;0 与重复已丢掉;不别名调用方的切片")
	assert.Equal(t, []scenepb.SyncDomain{syncGuild, syncApplication}, batch.domains)
	require.True(t, batch.hasDeadline)
	assert.Equal(t, fixedBudgetDeadline, batch.deadline, "BumpAll 拿到的是预算上下文,不是别的 ctx")
	asked, cancels := budget.snapshot()
	assert.Equal(t, []time.Duration{guildSyncBumpBudget}, asked, "整批只建一个预算,取 guildSyncBumpBudget")
	assert.Equal(t, 1, cancels, "批次结束后释放预算上下文")
}

// TestAsyncGuildSyncBumperDoesNotBlockCaller:Redis 卡住时 Bump 照样返回(不占 RPC 预算),
// 卡住的批次在预算到期时被 ctx 放出来,并释放预算上下文。
//
// 判定不看耗时:假 Redis 卡在测试持有的 gate 上且从不放行,同步实现会一直卡在 BumpAll 里,Bump 根本返回不了;
// waitSignal 的上限只防挂死。
func TestAsyncGuildSyncBumperDoesNotBlockCaller(t *testing.T) {
	gate := make(chan struct{}) // 从不关闭:Redis 一直卡着
	inner := &fakePlayerBumper{gate: gate, entered: make(chan struct{}, 1)}
	budget := newBudgetRecorder()
	finished := make(chan struct{})
	b := &AsyncGuildSyncBumper{
		inner: inner, budget: time.Second, withBudget: budget.withBudget,
		// 生产启动器外面包一层,只为知道批次何时退出。
		launch: func(fn func()) {
			launchSyncBump(func() {
				defer close(finished)
				fn()
			})
		},
	}

	returned := make(chan struct{})
	go func() {
		b.Bump([]uint64{1, 2, 3}, syncEconomy)
		close(returned)
	}()
	waitSignal(t, returned, "Bump 在 Redis 卡住时返回")
	waitSignal(t, inner.entered, "后台批次开始执行")
	select {
	case <-finished:
		t.Fatal("gate 未放行、预算未到期,批次不该已经退出")
	default:
	}

	budget.ctx.expire()
	waitSignal(t, finished, "卡住的批次在预算到期后退出")
	_, cancels := budget.snapshot()
	assert.Equal(t, 1, cancels, "退出时释放预算上下文")
	require.Len(t, inner.snapshot(), 1)
	assert.Equal(t, []uint64{1, 2, 3}, inner.snapshot()[0].players)
}

// TestAsyncGuildSyncBumperSkipsEmptyBatches:没有人或没有域时什么都不做,连协程都不起。
// 启动器换成计数替身,"起没起"同步可数,不靠后到的批次当栅栏。
func TestAsyncGuildSyncBumperSkipsEmptyBatches(t *testing.T) {
	inner := &fakePlayerBumper{}
	launcher := &heldLauncher{}
	b := &AsyncGuildSyncBumper{inner: inner, budget: time.Second, withBudget: newBudgetRecorder().withBudget, launch: launcher.launch}

	b.Bump(nil, syncGuild)
	b.Bump([]uint64{0, 0}, syncGuild)
	b.Bump([]uint64{1})
	assert.Zero(t, launcher.count(), "没有人或没有域时不起协程")

	b.Bump([]uint64{7}, syncGuild)
	require.Equal(t, 1, launcher.count())
	launcher.runAll()
	batches := inner.snapshot()
	require.Len(t, batches, 1)
	assert.Equal(t, []uint64{7}, batches[0].players)
}

// TestGuildSyncBumperThroughPlayersync:接上真的 playersync.Bumper(下面是假 Evaler),看落到 Redis 的那一层:
// 每人一条 Lua、键是 player:{id}:sync_digest、ARGV[2..] 是域号十进制(GUILD=6、GUILD_APPLICATION=7,§3.3 表);
// 某人失败不中断整批(其余人照样 bump),也不 panic;每条命令都带着整批的预算。
func TestGuildSyncBumperThroughPlayersync(t *testing.T) {
	eval := &fakeDigestEvaler{fail: func(key string) error {
		if key == playersync.Key(2) {
			return errRedisDown(key)
		}
		return nil
	}}
	b := NewGuildSyncBumper(playersync.NewBumper(eval, "guild"))

	b.Bump([]uint64{1, 2, 3, 0, 1}, syncGuild, syncApplication)

	require.Eventually(t, func() bool { return len(eval.snapshot()) == 3 }, 2*time.Second, 5*time.Millisecond,
		"2 失败之后 3 照样 bump")
	for i, id := range []uint64{1, 2, 3} {
		call := eval.snapshot()[i]
		assert.Equal(t, []string{playersync.Key(id)}, call.keys)
		require.Len(t, call.args, 3, "ARGV = TTL 秒 + 两个域号")
		assert.Equal(t, []string{"6", "7"}, call.args[1:])
		assert.True(t, call.hasDeadline)
	}
}

// TestBumpSyncWithoutBumperDoesNotPanic:未经 NewGuildLogic 构造(syncBumper 为 nil)时只记日志 ——
// bumpSync 跑在提交之后,panic 等于把一次写成功的操作报成 RPC 失败。
func TestBumpSyncWithoutBumperDoesNotPanic(t *testing.T) {
	l := &GuildLogic{}
	assert.NotPanics(t, func() { l.bumpSync([]uint64{42}, syncGuild) })
}

// TestWithSyncBumper:默认 Noop;传 nil 被忽略(不覆盖成 nil);显式值原样保留。
func TestWithSyncBumper(t *testing.T) {
	assert.IsType(t, NoopSyncBumper{}, NewGuildLogic(nil, nil, nil, nil).syncBumper)
	assert.IsType(t, NoopSyncBumper{}, NewGuildLogic(nil, nil, nil, nil, WithSyncBumper(nil)).syncBumper)

	rec := &recordingSyncBumper{}
	assert.Same(t, rec, NewGuildLogic(nil, nil, nil, nil, WithSyncBumper(rec)).syncBumper)
}

// ── 纯函数 ────────────────────────────────────────────────────

// TestMembersPlusAndReviewersIn:membersPlus = 快照全体 + 已不在快照里的当事人;reviewersIn 只取长老与帮主
// (与 guildInfoFor 判"看不看得到待审数"同一个判据)。
func TestMembersPlusAndReviewersIn(t *testing.T) {
	g := &data.GuildData{GuildID: 9, Members: []data.MemberData{
		{PlayerID: 42, Role: constants.RoleLeader},
		{PlayerID: 43, Role: constants.RoleOfficer},
		{PlayerID: 44, Role: constants.RoleMember},
	}}

	assert.Equal(t, []uint64{42, 43, 44, 7}, membersPlus(g, 7))
	assert.Equal(t, []uint64{42, 43, 44}, membersPlus(g))
	assert.Equal(t, []uint64{7}, membersPlus(nil, 7), "快照缺失时至少还有当事人")
	assert.Equal(t, []uint64{42, 43}, reviewersIn(g))
	assert.Empty(t, reviewersIn(nil))
}

// TestAssetFinalizedSyncDomains:捐献 / 兑换 → 经济页 + 本人 GuildInfo(对侧账记帮贡 / 退帮贡);
// 活动奖励 → 只有活动页(两个经济页按 kind 过滤掉它,帮贡在入队事务里已记完);
// 未知 kind 三个域都 bump —— 多拉一次无害,漏拉才违反 L-1。
func TestAssetFinalizedSyncDomains(t *testing.T) {
	cases := []struct {
		kind pb.GuildAssetOpKind
		want []scenepb.SyncDomain
	}{
		{pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE, []scenepb.SyncDomain{syncEconomy, syncGuild}},
		{pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP, []scenepb.SyncDomain{syncEconomy, syncGuild}},
		{pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD, []scenepb.SyncDomain{syncActivity}},
		{pb.GuildAssetOpKind(99), []scenepb.SyncDomain{syncEconomy, syncActivity, syncGuild}},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, assetFinalizedSyncDomains(tc.kind), "kind=%d", tc.kind)
	}
}

// ── 资产指令终结(§3.3 表第 8 行)───────────────────────────────

// TestOnAssetFinalizedBumpsOwnerOnEveryPath:终结后只 bump 本人;**同步投递路径同样 bump**
// (推送在那条路上被压掉,因为调用方拿着回包;但回包可能在断线时丢,摘要不能跟着压)。
func TestOnAssetFinalizedBumpsOwnerOnEveryPath(t *testing.T) {
	kinds := []pb.GuildAssetOpKind{
		pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE,
		pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP,
		pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD,
	}
	for _, kind := range kinds {
		for _, syncDelivery := range []bool{false, true} {
			rec := &recordingSyncBumper{}
			notifier := &recordingNotifier{}
			l := NewGuildLogic(nil, nil, nil, nil, WithSyncBumper(rec), WithNotifier(notifier))
			ctx := context.Background()
			if syncDelivery {
				ctx = withSyncDelivery(ctx)
			}

			l.OnAssetFinalized(ctx, data.FinalizedOp{OpID: 1, PlayerID: 42, GuildID: 9, Kind: kind, Status: assetop.StatusApplied})

			require.Equal(t, 1, rec.count(), "kind=%s sync=%t", kind, syncDelivery)
			assert.Equal(t, []uint64{42}, rec.calls[0].players, "只 bump 本人,不扇出全帮")
			assert.Equal(t, assetFinalizedSyncDomains(kind), rec.calls[0].domains)
			if syncDelivery {
				assert.Zero(t, notifier.total(), "同步投递路径的推送仍然压掉")
			} else {
				assert.Equal(t, 1, notifier.total())
			}
		}
	}
}

// ── 灯会 / 团圆(§3.3 表第 9 行)───────────────────────────────

// TestAfterActivityCommitBumps:每次参与都 bump 本人的活动页与 GuildInfo(personal_contribution 记进了他的帮贡);
// 本档期首次达阈值时活动页扇出全帮,本次发了帮会资金时 GuildInfo 扇出全帮(都含本人)。
// 事务要 MySQL,这里直接调提交后的那一段(与 LightGuildLantern / ClaimGuildReunion 同一个函数)。
func TestAfterActivityCommitBumps(t *testing.T) {
	g := activityGuild(1, 42, 43, 44)
	a := activityActor{playerID: 43, guild: &g, member: g.Members[1]}

	t.Run("未达阈值只给本人", func(t *testing.T) {
		rec := &recordingSyncBumper{}
		l := NewGuildLogic(nil, nil, nil, nil, WithSyncBumper(rec))

		l.afterActivityCommit(context.Background(), &ActivityDeps{}, a, activityReward{}, data.ActivityTxResult{}, activityTypeLantern)

		require.Equal(t, 1, rec.count(), "本人一条 Lua 带两个域")
		assert.Equal(t, []uint64{43}, rec.playersOf(syncActivity))
		assert.Equal(t, []uint64{43}, rec.playersOf(syncGuild), "灯会进度是帮会级数值不扇出;本人的帮贡变了")
		assert.Equal(t, []scenepb.SyncDomain{syncGuild, syncActivity}, rec.domainsOf(43))
	})
	t.Run("首次达阈值且发了资金扇出全帮", func(t *testing.T) {
		rec := &recordingSyncBumper{}
		l := NewGuildLogic(nil, nil, nil, nil, WithSyncBumper(rec))

		l.afterActivityCommit(context.Background(), &ActivityDeps{}, a, activityReward{},
			data.ActivityTxResult{ReachedNow: true, FundsGranted: true}, activityTypeReunion)

		assert.Equal(t, []uint64{42, 43, 44}, rec.playersOf(syncActivity), "全帮含操作者本人")
		assert.Equal(t, []uint64{42, 43, 44}, rec.playersOf(syncGuild), "帮会资金变了,全帮 GuildInfo 都变")
	})
	t.Run("达阈值但没发资金只扇出活动页", func(t *testing.T) {
		rec := &recordingSyncBumper{}
		l := NewGuildLogic(nil, nil, nil, nil, WithSyncBumper(rec))

		l.afterActivityCommit(context.Background(), &ActivityDeps{}, a, activityReward{},
			data.ActivityTxResult{ReachedNow: true}, activityTypeLantern)

		assert.Equal(t, []uint64{42, 43, 44}, rec.playersOf(syncActivity))
		assert.Equal(t, []uint64{43}, rec.playersOf(syncGuild), "资金没变:GUILD 只给帮贡变了的本人")
	})
}

// ── 审批(§3.3 表第 6、7 行)────────────────────────────────────

// TestBumpReviewed:通过 → 申请人 GUILD_APPLICATION + 全帮(含申请人与审批人)GUILD;
// 拒绝 → 申请人只有 GUILD_APPLICATION(他仍不在帮,GetPlayerGuild 不变),审批人(长老 / 帮主)GUILD(待审数少了一条)。
func TestBumpReviewed(t *testing.T) {
	const applicant uint64 = 45
	members := []data.MemberData{
		{PlayerID: 42, Role: constants.RoleLeader},
		{PlayerID: 43, Role: constants.RoleOfficer},
		{PlayerID: 44, Role: constants.RoleMember},
	}

	t.Run("通过", func(t *testing.T) {
		rec := &recordingSyncBumper{}
		l := NewGuildLogic(nil, nil, nil, nil, WithSyncBumper(rec))
		snapshot := &data.GuildData{GuildID: 9, Members: append(slices.Clone(members), data.MemberData{PlayerID: applicant, Role: constants.RoleMember})}

		l.bumpReviewed(data.ReviewResult{Guild: snapshot, Approved: true}, applicant)

		assert.Equal(t, []uint64{applicant}, rec.playersOf(syncApplication))
		assert.Equal(t, []uint64{42, 43, 44, applicant}, rec.playersOf(syncGuild))
		assert.Equal(t, []scenepb.SyncDomain{syncGuild, syncApplication}, rec.domainsOf(applicant))
	})
	t.Run("拒绝", func(t *testing.T) {
		rec := &recordingSyncBumper{}
		l := NewGuildLogic(nil, nil, nil, nil, WithSyncBumper(rec))
		snapshot := &data.GuildData{GuildID: 9, Members: slices.Clone(members)}

		l.bumpReviewed(data.ReviewResult{Guild: snapshot, Approved: false}, applicant)

		assert.Equal(t, []uint64{applicant}, rec.playersOf(syncApplication))
		assert.Equal(t, []uint64{42, 43}, rec.playersOf(syncGuild), "只有能看到待审数的人")
		assert.Equal(t, []scenepb.SyncDomain{syncApplication}, rec.domainsOf(applicant))
		assert.Empty(t, rec.domainsOf(44), "普通成员看不到待审数")
	})
}

// ── 提交之前的拒绝不 bump(§3.5 第 1 条)────────────────────────

// TestWritesRefusedBeforeCommitDoNotBump:归属未知时在碰库之前拒绝的全部写 RPC(管理 / 申请 / 建帮 / 离帮 / 解散 / 公告)
// 一次都不 bump —— 先 bump 后提交(或不提交)会让客户端把旧数据记成新版本。
func TestWritesRefusedBeforeCommitDoNotBump(t *testing.T) {
	for _, tc := range clientPrecheckWrites() {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingSyncBumper{}
			l := NewGuildLogic(nil, nil, nil, &fakeHomeZones{zone: 0}, WithSyncBumper(rec))

			id, err := tc.run(l, clientCtx(42))

			require.NoError(t, err)
			require.Equal(t, constants.ErrHomeZoneUnknown, id)
			assert.Zero(t, rec.count())
		})
	}
}

// ── 同道历练(§3.3 表第 9 行)───────────────────────────────────

// TestTrialLobbyChangesBumpWholeRoster:建房、同意、开战都给名单全员(含操作者本人)bump GUILD_ACTIVITY;
// 名单外的成员不 bump;重复点击(房间没变)不 bump。
func TestTrialLobbyChangesBumpWholeRoster(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43, 44, 45), nil)
	rec := &recordingSyncBumper{}
	f.l.syncBumper = rec
	f.setOnline(t, 42, 43, 44, 45)

	lobby := f.mustStart(t, 42, 42, 43, 44)
	require.Equal(t, 1, rec.count(), "建房一次")
	assert.Equal(t, []uint64{42, 43, 44}, rec.playersOf(syncActivity), "名单全员含发起人;名单外的 45 不 bump")
	assert.Equal(t, []scenepb.SyncDomain{syncActivity}, rec.domainsOf(42))

	accepted, err := f.respond(43, lobby.GetLobbyId(), true)
	require.NoError(t, err)
	require.Nil(t, accepted.GetErrorMessage())
	require.Equal(t, 2, rec.count(), "同意一次")
	assert.ElementsMatch(t, []uint64{42, 43, 44}, rec.calls[1].players, "含同意者本人")

	launched, err := f.respond(44, lobby.GetLobbyId(), true)
	require.NoError(t, err)
	require.Nil(t, launched.GetErrorMessage())
	require.Equal(t, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHED, launched.GetActivity().GetTrialLobby().GetState())
	require.Equal(t, 3, rec.count(), "开战一次")
	assert.ElementsMatch(t, []uint64{42, 43, 44}, rec.calls[2].players)

	repeat, err := f.respond(44, lobby.GetLobbyId(), true)
	require.NoError(t, err)
	require.Nil(t, repeat.GetErrorMessage())
	assert.Equal(t, 3, rec.count(), "重复点击房间没变,不 bump")
}

// TestTrialDeclineBumpsAndRejectedStartDoesNot:拒绝让房间结束 → 名单全员 bump;
// 名单不合法在建房之前就拒绝 → 不 bump。
func TestTrialDeclineBumpsAndRejectedStartDoesNot(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43, 44), nil)
	rec := &recordingSyncBumper{}
	f.l.syncBumper = rec
	f.setOnline(t, 42, 43, 44)

	refused, err := f.start(42, 42, 43, 43)
	require.NoError(t, err)
	require.Equal(t, constants.ErrTrialTeamInvalid, refused.GetErrorMessage().GetId())
	assert.Zero(t, rec.count(), "没建房就没有可 bump 的变化")

	lobby := f.mustStart(t, 42, 42, 43, 44)
	declined, err := f.respond(44, lobby.GetLobbyId(), false)
	require.NoError(t, err)
	require.Nil(t, declined.GetErrorMessage())
	require.Equal(t, 2, rec.count())
	assert.ElementsMatch(t, []uint64{42, 43, 44}, rec.calls[1].players, "含拒绝者本人")
}

// TestTrialSettleBumps:负局只 bump 己方参战者的活动页;胜局得奖者的帮贡变了,另 bump GUILD;
// 计了帮会资金的胜局两个域都扇出全帮;重复结果什么都没改,不 bump。
func TestTrialSettleBumps(t *testing.T) {
	useTrialTables(t, nil, nil)

	t.Run("负局只给参战者", func(t *testing.T) {
		f := newTrialFixture(t, activityGuild(1, 42, 43, 44, 45), nil)
		rec := &recordingSyncBumper{}
		f.l.syncBumper = rec
		ev := trialEvent(501, 42, 43)
		ev.Outcome = battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN

		outcome, err := f.l.SettleTrialResult(context.Background(), ev)

		require.NoError(t, err)
		require.Equal(t, TrialOutcomeSettled, outcome)
		assert.Equal(t, []uint64{42, 43}, rec.playersOf(syncActivity))
		assert.Empty(t, rec.playersOf(syncGuild), "没人得帮贡、没计资金:GuildInfo 没变")
	})
	t.Run("胜局未计资金只给得奖者 bump GUILD", func(t *testing.T) {
		f := newTrialFixture(t, activityGuild(1, 42, 43, 44, 45), nil)
		rec := &recordingSyncBumper{}
		f.l.syncBumper = rec
		f.store.settle = func(data.TrialSettleInput) (data.TrialSettleResult, error) {
			return data.TrialSettleResult{
				Status: data.TrialSettleSettled, Result: pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_WIN,
				Rewarded: []uint64{43},
			}, nil
		}
		ev := trialEvent(504, 42, 43)

		_, err := f.l.SettleTrialResult(context.Background(), ev)

		require.NoError(t, err)
		assert.Equal(t, []uint64{42, 43}, rec.playersOf(syncActivity), "参战者全体")
		assert.Equal(t, []uint64{43}, rec.playersOf(syncGuild), "只有得奖者的帮贡变了(42 当日次数已满等原因未得奖)")
	})
	t.Run("计了资金扇出全帮", func(t *testing.T) {
		f := newTrialFixture(t, activityGuild(1, 42, 43, 44, 45), nil)
		rec := &recordingSyncBumper{}
		f.l.syncBumper = rec
		f.store.settle = func(data.TrialSettleInput) (data.TrialSettleResult, error) {
			return data.TrialSettleResult{
				Status: data.TrialSettleSettled, Result: pb.GuildTrialSettleResult_GUILD_TRIAL_SETTLE_RESULT_WIN,
				Rewarded: []uint64{42, 43}, FundsGranted: true, CountedWins: 1,
			}, nil
		}
		ev := trialEvent(502, 42, 43)

		_, err := f.l.SettleTrialResult(context.Background(), ev)

		require.NoError(t, err)
		assert.Equal(t, []uint64{42, 43, 44, 45}, rec.playersOf(syncActivity))
		assert.Equal(t, []uint64{42, 43, 44, 45}, rec.playersOf(syncGuild), "帮会资金变了:搭同一次扇出")
		for _, id := range []uint64{42, 43, 44, 45} {
			assert.Equal(t, []scenepb.SyncDomain{syncGuild, syncActivity}, rec.domainsOf(id), "player=%d", id)
		}
	})
	t.Run("重复结果不 bump", func(t *testing.T) {
		f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
		rec := &recordingSyncBumper{}
		f.l.syncBumper = rec
		f.store.settle = func(data.TrialSettleInput) (data.TrialSettleResult, error) {
			return data.TrialSettleResult{Status: data.TrialSettleDuplicate}, nil
		}
		ev := trialEvent(503, 42, 43)

		outcome, err := f.l.SettleTrialResult(context.Background(), ev)

		require.NoError(t, err)
		require.Equal(t, TrialOutcomeDuplicate, outcome)
		assert.Zero(t, rec.count())
	})
}

// ── bump 失败不影响业务结果(§3.5 第 5 条)──────────────────────

// TestSyncBumpFailureDoesNotAffectRPC:摘要 Redis 全部失败时,RPC 照常成功、回包完整;
// 失败的 bump 仍逐人尝试过(由 playersync 计指标),只是结果被丢弃。
func TestSyncBumpFailureDoesNotAffectRPC(t *testing.T) {
	useTrialTables(t, nil, nil)
	f := newTrialFixture(t, activityGuild(1, 42, 43), nil)
	eval := &fakeDigestEvaler{fail: errRedisDown}
	f.l.syncBumper = NewGuildSyncBumper(playersync.NewBumper(eval, "guild"))
	f.setOnline(t, 42, 43)

	resp, err := f.start(42, 42, 43)

	require.NoError(t, err)
	require.Nil(t, resp.GetErrorMessage(), "bump 失败绝不能变成业务拒绝")
	require.NotNil(t, resp.GetActivity().GetTrialLobby(), "回包照常带房间")
	require.Eventually(t, func() bool { return len(eval.snapshot()) == 2 }, 2*time.Second, 5*time.Millisecond,
		"失败的 bump 照样逐人尝试过")
}

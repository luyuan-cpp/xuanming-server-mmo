package clientplayerloginlogic

// CreatePlayer 按 handler 截止分摊各跳预算的接线回归(createplayer_budget.go;login/internal/logic/pkg/createbudget;
// docs/design/client-request-ordering.md §5.3 / §6.1)。分摊公式本身在 createbudget 包里测;这里测的是 CreatePlayer
// 有没有在每一跳之前派生 ctx、预算不够时是不是在发出之前就放弃、补偿对不对。
//
// 修复前:发号 / 名字 / player_route 各自套固定的 3s(名义之和远超 4s 的 handler 预算),前面慢了,后面的写照样带着
// 所剩无几的时间发出去;下面「剩余预算不足」两例在修复前分别会发出发号与名字登记、发出 player_route 登记。
//
// harness 沿用 createplayer_name_test.go(newNameHarness)。用到墙钟的只有「handler 截止」本身,取值与阈值都差出
// 数百毫秒以上;要模拟「某一跳拖慢」时注入 createPlayerNow,不 sleep。

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"login/internal/logic/pkg/createbudget"
	"login/internal/logic/pkg/homezone"
	"login/internal/logic/pkg/playernamereg"
	pbbase "proto/common/base"
	dspb "proto/data_service"
	"shared/generated/pb/table"
	"shared/idsegment"
	"shared/playername"
)

func budgetSkipCount(t *testing.T, hop createbudget.Hop) float64 {
	t.Helper()
	return createPlayerCounterValue(t, "login_create_player_budget_skip_total", map[string]string{"hop": hop.String()})
}

// requireNothingCreated:建角放弃之后,SQL 账号目录里没有新角色,建角锁已释放。
func requireNothingCreated(t *testing.T, n *nameHarness) {
	t.Helper()
	if players := storedPlayers(t, n.ctx, n.rdb); len(players) != 0 {
		t.Fatalf("account directory has %d player(s) although the create was abandoned", len(players))
	}
	if n.mr.Exists(nameCreateKey) {
		t.Fatal("create lock still held after CreatePlayer gave up")
	}
}

// 回归:handler 剩下的预算放不下「发号之后各跳的下限与间隙」(createbudget.Plan.ReserveAfter(HopMint),缺省 2550ms)
// → 发号之前就放弃:不发号、名字登记 / player_route 登记 / SQL 提交一个都不发,不需要任何补偿。
func TestCreatePlayer_ShortHandlerBudgetSendsNoWrites(t *testing.T) {
	n := newNameHarness(t)
	plan := createbudget.New(0)
	// 2s 比所需的余量少 550ms,建角前面几步(miniredis 往返)只会让剩下的更少,结论不随机器快慢变化。
	budget := 2 * time.Second
	if budget >= plan.ReserveAfter(createbudget.HopMint) {
		t.Fatalf("scenario needs a handler budget below %s", plan.ReserveAfter(createbudget.HopMint))
	}
	ctx, cancel := context.WithTimeout(n.l.ctx, budget)
	defer cancel()
	n.l.ctx = ctx
	skipsBefore := budgetSkipCount(t, createbudget.HopMint)

	resp := n.create(t, "云中君")

	requireTip(t, resp, table.LoginError_kLoginDataSerializeFailed)
	if n.segment.calls != 0 {
		t.Fatalf("minted %d id(s) although the handler budget could not fit the later hops", n.segment.calls)
	}
	if got := strings.Join(n.fake.events, ","); got != "" {
		t.Fatalf("data_service calls = %q, want none", got)
	}
	requireNothingCreated(t, n)
	if got := budgetSkipCount(t, createbudget.HopMint) - skipsBefore; got != 1 {
		t.Fatalf("budget_skip_total{hop=mint} delta = %v, want 1", got)
	}
}

// 回归:名字登记拖到了名字这一跳的截止,之后剩下的不够 player_route 登记的下限(= 上限)加给提交留的余量
// → player_route 登记不发,名字按「确定已登记」释放一次(没有延迟的第二次),SQL 提交不发。
// 「拖慢」用注入的 createPlayerNow 模拟:handler 截止是真的(4s 之后),只把建角眼里的「此刻」往后拨。
func TestCreatePlayer_RegisterSkippedWhenNameHopOverran(t *testing.T) {
	n := newNameHarness(t)
	plan := createbudget.New(0)
	base := time.Now()
	var elapsed time.Duration
	saved := createPlayerNow
	createPlayerNow = func() time.Time { return base.Add(elapsed) }
	t.Cleanup(func() { createPlayerNow = saved })

	const handlerBudget = 4 * time.Second
	ctx, cancel := context.WithDeadline(n.l.ctx, base.Add(handlerBudget))
	defer cancel()
	n.l.ctx = ctx
	n.fake.reserve = func(int, *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
		// 登记成功返回时,剩下的比「player_route 下限 + 提交余量」少 1ms。
		elapsed = handlerBudget - plan.ReserveAfter(createbudget.HopRegister) - plan.Floor(createbudget.HopRegister) + time.Millisecond
		return &dspb.ReservePlayerNameResponse{Result: playername.ReserveOK}, nil
	}
	skipsBefore := budgetSkipCount(t, createbudget.HopRegister)

	resp := n.create(t, "云中君")

	requireTip(t, resp, table.LoginError_kLoginDataSerializeFailed)
	if ctx.Err() != nil {
		t.Fatal("the real handler ctx ended; the scenario must give up on budget, not on the deadline")
	}
	if got := strings.Join(n.fake.events, ","); got != "reserve,release" {
		t.Fatalf("data_service call order = %q, want %q (no player_route registration)", got, "reserve,release")
	}
	if len(n.scheduled) != 0 {
		t.Fatalf("delayed releases scheduled = %v, want none (the name was definitely reserved)", n.scheduled)
	}
	requireNothingCreated(t, n)
	if got := budgetSkipCount(t, createbudget.HopRegister) - skipsBefore; got != 1 {
		t.Fatalf("budget_skip_total{hop=register} delta = %v, want 1", got)
	}
}

// deadlineSegment 记下发号时 ctx 的截止。
type deadlineSegment struct {
	id   uint64
	seen []observedDeadline
}

type observedDeadline struct {
	at  time.Time
	set bool
}

func observe(ctx context.Context) observedDeadline {
	at, ok := ctx.Deadline()
	return observedDeadline{at: at, set: ok}
}

func (s *deadlineSegment) Next(ctx context.Context) (uint64, error) {
	s.seen = append(s.seen, observe(ctx))
	return s.id, nil
}

// deadlineProbeClient 在名字登记与 player_route 登记上记下 RPC ctx 的截止,其余原样交给 fakeRegisterClient。
type deadlineProbeClient struct {
	*fakeRegisterClient
	reserveSeen  []observedDeadline
	registerSeen []observedDeadline
}

func (c *deadlineProbeClient) ReservePlayerName(ctx context.Context, in *dspb.ReservePlayerNameRequest, opts ...grpc.CallOption) (*dspb.ReservePlayerNameResponse, error) {
	c.reserveSeen = append(c.reserveSeen, observe(ctx))
	return c.fakeRegisterClient.ReservePlayerName(ctx, in, opts...)
}

func (c *deadlineProbeClient) RegisterPlayerZone(ctx context.Context, in *dspb.RegisterPlayerZoneRequest, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	c.registerSeen = append(c.registerSeen, observe(ctx))
	return c.fakeRegisterClient.RegisterPlayerZone(ctx, in, opts...)
}

// deadlineDirectory 在 SQL 账号目录提交上记下 ctx 的截止,其余原样交给 testDirectory。
type deadlineDirectory struct {
	*testDirectory
	commitSeen []observedDeadline
}

func (d *deadlineDirectory) CreateWithInitialCommit(ctx context.Context, account string, role *pbbase.AccountSimplePlayer) (*dspb.AccountDirectoryResponse, error) {
	d.commitSeen = append(d.commitSeen, observe(ctx))
	return d.testDirectory.CreateWithInitialCommit(ctx, account, role)
}

// 各跳拿到的截止都不晚于 handler 截止,而且给后面各跳留足了余量:
// 发号 ≤ 截止 − ReserveAfter(发号)、名字登记 ≤ 截止 − ReserveAfter(名字)、player_route ≤ 截止 − ReserveAfter(登记)、
// SQL 提交 ≤ 截止。每一跳都必须带截止(没有截止 = 不受 handler 截止约束)。
func TestCreatePlayer_EveryHopDeadlineWithinHandlerDeadline(t *testing.T) {
	n := newNameHarness(t)
	plan := createbudget.New(0)
	segment := &deadlineSegment{id: hzPlayerID}
	n.l.svcCtx.PlayerIDMinter = &idsegment.Minter{Name: "player_id", Segment: segment}
	probe := &deadlineProbeClient{fakeRegisterClient: n.fake}
	n.l.svcCtx.PlayerNames = playernamereg.New(probe)
	n.l.svcCtx.HomeZone = homezone.New(probe, 0, 0, 0)
	directory := &deadlineDirectory{testDirectory: fixtureDirectory(t, n.rdb)}
	n.l.svcCtx.AccountDirectory = directory

	ctx, cancel := context.WithTimeout(n.l.ctx, 4*time.Second)
	defer cancel()
	n.l.ctx = ctx
	handlerDeadline, _ := ctx.Deadline()

	resp := n.create(t, "云中君")
	if resp.ErrorMessage != nil {
		t.Fatalf("unexpected tip %v", resp.ErrorMessage)
	}

	hops := []struct {
		label  string
		seen   []observedDeadline
		latest time.Time
	}{
		{"mint", segment.seen, handlerDeadline.Add(-plan.ReserveAfter(createbudget.HopMint))},
		{"name", probe.reserveSeen, handlerDeadline.Add(-plan.ReserveAfter(createbudget.HopName))},
		{"register", probe.registerSeen, handlerDeadline.Add(-plan.ReserveAfter(createbudget.HopRegister))},
		{"commit", directory.commitSeen, handlerDeadline},
	}
	for _, hop := range hops {
		if len(hop.seen) != 1 {
			t.Fatalf("%s hop called %d time(s), want 1", hop.label, len(hop.seen))
		}
		got := hop.seen[0]
		if !got.set {
			t.Fatalf("%s hop ran without a deadline", hop.label)
		}
		if got.at.After(handlerDeadline) {
			t.Fatalf("%s hop deadline %s is after the handler deadline %s", hop.label, got.at, handlerDeadline)
		}
		if got.at.After(hop.latest) {
			t.Fatalf("%s hop deadline %s leaves less than the reserve for later hops (latest allowed %s)",
				hop.label, got.at, hop.latest)
		}
	}
}

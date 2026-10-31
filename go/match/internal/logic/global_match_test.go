package logic

import (
	"context"
	"database/sql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"match/internal/authority"
	"match/internal/catalog"
	"match/internal/config"
	"match/internal/discovery"
	"match/internal/poolgroup"
	"match/internal/scope"
	"match/internal/svc"
	battlepb "proto/battle"
	pb "proto/match"
	teampb "proto/team"
	"shared/serviceauth"
	"testing"
	"time"
)

func TestOutcomeCreateResponseRequiresOriginalInstanceAndAdoptedVersion(t *testing.T) {
	original := createBattleFn
	defer func() { createBattleFn = original }()
	req := &battlepb.CreateBattleRequest{BattleId: 17, TargetInstanceId: "original-instance", OutcomeProtocolVersion: 1}
	for _, test := range []struct {
		name     string
		id       uint64
		instance string
		adopted  uint32
		good     bool
	}{
		{"旧回包", 17, "", 0, false}, {"错实例", 17, "new-instance", 1, false}, {"只支持未采用", 17, "original-instance", 0, false}, {"错局", 18, "original-instance", 1, false}, {"原件", 17, "original-instance", 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			createBattleFn = func(string, *battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error) {
				return &battlepb.CreateBattleResponse{BattleId: test.id, TargetInstanceId: test.instance, AdoptedOutcomeProtocolVersion: test.adopted}, nil
			}
			err := createBattle(discovery.NodeEntry{Endpoint: "fixture"}, req)
			if test.good {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestBattleOutcomeRPCRejectsWrongMethodCallerAndChangedOriginal(t *testing.T) {
	const secret = "fixture-only-battle-terminal-signature-000000000"
	v, err := serviceauth.New(map[string]string{"battle": secret})
	require.NoError(t, err)
	s := &svc.ServiceContext{Authority: &authority.Store{}, BattleOutcomeAuth: v, BattleOutcomeSecret: secret}
	r := &pb.CommitBattleOutcomeRequest{BattleId: 17, PlacementOperationId: "original-op", TargetInstanceId: "original-instance", RawResultPayload: []byte{8, 17}}
	_, err = CommitBattleOutcome(context.Background(), s, r)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.NoError(t, serviceauth.Sign(pb.MatchInternal_CommitBattleOutcome_FullMethodName, r, "battle", secret, uint64(time.Now().UnixMilli())))
	changed := proto.Clone(r).(*pb.CommitBattleOutcomeRequest)
	changed.TargetInstanceId = "other"
	_, err = CommitBattleOutcome(context.Background(), s, changed)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.NoError(t, serviceauth.Sign(pb.MatchInternal_CancelActivityBattle_FullMethodName, r, "battle", secret, uint64(time.Now().UnixMilli())))
	_, err = CommitBattleOutcome(context.Background(), s, r)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.NoError(t, serviceauth.Sign(pb.MatchInternal_CommitBattleOutcome_FullMethodName, r, "guild", secret, uint64(time.Now().UnixMilli())))
	_, err = CommitBattleOutcome(context.Background(), s, r)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.NoError(t, serviceauth.Sign(pb.MatchInternal_CommitBattleOutcome_FullMethodName, r, "battle", secret, uint64(time.Now().UnixMilli())))
	_, err = CommitBattleOutcome(context.Background(), s, r)
	require.Equal(t, codes.Unavailable, status.Code(err), "有效签名仍不能把无DB冒成成功")
	s.Authority.DB = new(sql.DB)
	_, err = CommitBattleOutcome(context.Background(), s, r)
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "有效签名下的原PB语义错误在SQL前永久拒绝")
}

func globalCandidate(pid, queued, now uint64, first, second uint32) *pb.MatchAuthorityTicketRecord {
	p := &pb.MatchNetworkProfile{PlanId: "p", SessionBinding: "s"}
	for region, rtt := range map[uint32]uint32{1: first, 2: second} {
		p.Receipts = append(p.Receipts, &pb.NetworkProbeReceipt{Grant: &pb.NetworkProbeGrant{PlayerId: pid, PhysicalRegion: region, ExpiresAtMs: now + 1000}, RttMs: rtt, SampleCount: 3, MeasuredAtMs: now, ExpiresAtMs: now + 1000})
	}
	b, _ := proto.Marshal(p)
	return &pb.MatchAuthorityTicketRecord{PlayerId: pid, Mode: 3, ConfigId: 1, TeamSize: 2, State: 1, EnqueuedMs: queued, NetworkProfile: b, RatingCenti: 150000, ExpiresMs: now + 1000, PreferredRegion: authority.PreferredRegion(p, now)}
}

// 按池撮合的单位输入与成组(global_pool_matcher.go):生产代码 poolUnitInput / poolPolicy 原样参与,
// 只把数据库读到的票换成内存构造的原件。提交点的 SQL 复核另有真库回归。

func poolFixtureCtx(store *authority.Store, c *catalog.Catalog) *svc.ServiceContext {
	return &svc.ServiceContext{Authority: store, Catalog: c, Config: config.Config{GlobalMatch: config.GlobalMatchConf{HardLatencyMs: 180, CrossRegionWaitSeconds: 30}}}
}

// formPoolFixture 把每张票当成一个单排单位,按 pool 的规则在 regions 上成组。
func formPoolFixture(t *testing.T, s *svc.ServiceContext, pool catalog.Pool, regions map[uint32]bool, now uint64, tickets ...*pb.MatchAuthorityTicketRecord) ([]poolgroup.Group, []poolgroup.Unit) {
	t.Helper()
	var inputs []poolgroup.Unit
	for _, ticket := range tickets {
		input, ok := poolUnitInput(s, poolUnit{tickets: []*pb.MatchAuthorityTicketRecord{ticket}}, now)
		if ok {
			inputs = append(inputs, input)
		}
	}
	return poolgroup.Form(inputs, regions, now, poolPolicy(s, pool)), inputs
}

func groupPlayers(group poolgroup.Group, inputs []poolgroup.Unit) []uint64 {
	var players []uint64
	for _, index := range group.Units {
		players = append(players, inputs[index].Players...)
	}
	return players
}

// 单位输入:每个机房取成员中的最大 RTT(只留全员都在硬延迟内的机房),首选机房全员一致才保留;
// 没有这样的机房,单位不参加撮合。
func TestPoolUnitInputIntersectsMemberLatency(t *testing.T) {
	s := poolFixtureCtx(&authority.Store{}, nil)
	n := uint64(100000)
	a := globalCandidate(1, 0, n, 20, 100)
	solo, ok := poolUnitInput(s, poolUnit{tickets: []*pb.MatchAuthorityTicketRecord{a}}, n)
	require.True(t, ok)
	require.Equal(t, map[uint32]uint32{1: 20, 2: 100}, solo.RTT)
	require.Equal(t, uint32(1), solo.Preferred)
	require.Equal(t, a.RatingCenti, solo.RatingCenti)

	b := globalCandidate(2, 0, n, 100, 20)
	launch := &teampb.TeamLaunchRecord{ActorId: 2, EnqueuedMs: 5, MemberCount: 2}
	team, ok := poolUnitInput(s, poolUnit{tickets: []*pb.MatchAuthorityTicketRecord{a, b}, launch: launch}, n)
	require.True(t, ok)
	require.Equal(t, map[uint32]uint32{1: 100, 2: 100}, team.RTT, "每个机房取成员中的最大 RTT")
	require.Zero(t, team.Preferred, "成员首选不一致,只能参加跨机房阶段")
	require.Equal(t, uint64(2), team.ID)
	require.Equal(t, uint64(5), team.EnqueuedMs, "整队按 launch 入队时刻排队")
	require.Equal(t, []uint64{1, 2}, team.Players)
	require.Zero(t, team.RatingCenti, "整队不参加计分模式")

	far, near := globalCandidate(3, 0, n, 250, 20), globalCandidate(4, 0, n, 20, 250)
	_, ok = poolUnitInput(s, poolUnit{tickets: []*pb.MatchAuthorityTicketRecord{far, near}, launch: launch}, n)
	require.False(t, ok, "没有全员都在硬延迟内的机房")
}

// 机房优先级与成员硬延迟(D-16):本机房够人先在本机房成局;跨机房要全员原等待都过阈值;
// 没有全员都在硬延迟内的共同机房就继续等。
func TestPoolFormLocalRegionFirstAndHardMemberLatency(t *testing.T) {
	s := poolFixtureCtx(&authority.Store{}, nil)
	pool := catalog.Pool{Mode: pb.MatchMode_MATCH_MODE_1V1, ConfigId: 1, TeamSize: 2}
	regions := map[uint32]bool{1: true, 2: true}
	n := uint64(100000)
	a := globalCandidate(1, 0, n, 20, 100)
	b := globalCandidate(2, 0, n, 100, 20)
	c := globalCandidate(3, 90000, n, 30, 120)
	groups, inputs := formPoolFixture(t, s, pool, regions, n, a, b, c)
	require.Len(t, groups, 1)
	require.Equal(t, uint32(1), groups[0].Region)
	require.False(t, groups[0].Cross)
	require.ElementsMatch(t, []uint64{1, 3}, groupPlayers(groups[0], inputs), "本机房够人,即使旧票已过跨机房时限也先本机房")

	groups, _ = formPoolFixture(t, s, pool, regions, n, a, b)
	require.Len(t, groups, 1, "双方久等后可进入共同可用机房")
	require.True(t, groups[0].Cross)

	b.EnqueuedMs = 90000
	groups, _ = formPoolFixture(t, s, pool, regions, n, a, b)
	require.Empty(t, groups, "年轻候选不能被锚点的等待时间强行跨机房")

	far, near := globalCandidate(1, 0, n, 20, 250), globalCandidate(2, 0, n, 250, 20)
	groups, _ = formPoolFixture(t, s, pool, regions, n, far, near)
	require.Empty(t, groups, "没有全员都在硬延迟内的共同机房就继续等待")
}

// 单人档一人一局:同一页的两个单人单位各成一组,不会被凑进同一局。
func TestPoolFormSoloPoolOnePlayerPerGroup(t *testing.T) {
	s := poolFixtureCtx(&authority.Store{}, nil)
	pool := catalog.Pool{Mode: pb.MatchMode_MATCH_MODE_PVE_SOLO, ConfigId: 1, TeamSize: 1}
	n := uint64(100000)
	groups, _ := formPoolFixture(t, s, pool, map[uint32]bool{1: true}, n, globalCandidate(1, 0, n, 20, 100), globalCandidate(2, 0, n, 20, 100))
	require.Len(t, groups, 2)
	for _, group := range groups {
		require.Len(t, group.Units, 1)
		require.Equal(t, uint32(1), group.Region)
	}
}

// 范围树:票上的 home zone 经模板目录换成大区位置。同大区不同 zone 的两人,等满大区阈值才成局,层级记为大区。
func TestPoolFormWidensByHomeZoneScope(t *testing.T) {
	c, err := catalog.New(catalog.Rows{ZoneGroups: []scope.Range{{GroupID: 1, ZoneBegin: 1, ZoneEnd: 100}}})
	require.NoError(t, err)
	store := &authority.Store{}
	require.NoError(t, store.ConfigureMatchCatalog(c, scope.Schedule{GroupAfterMs: 10_000, GlobalAfterMs: 20_000}))
	s := poolFixtureCtx(store, c)
	pool := catalog.Pool{Mode: pb.MatchMode_MATCH_MODE_PVE_TEAM, ConfigId: 1, TeamSize: 2}
	n := uint64(100000)
	a, b := globalCandidate(1, n-5_000, n, 20, 100), globalCandidate(2, n-5_000, n, 20, 100)
	a.ZoneId, b.ZoneId = 1, 2
	groups, _ := formPoolFixture(t, s, pool, map[uint32]bool{1: true}, n, a, b)
	require.Empty(t, groups, "同大区不同 zone,还没等满大区阈值")

	a.EnqueuedMs, b.EnqueuedMs = n-10_000, n-10_000
	groups, _ = formPoolFixture(t, s, pool, map[uint32]bool{1: true}, n, a, b)
	require.Len(t, groups, 1)
	require.Equal(t, scope.LevelGroup, groups[0].Level)
}

func TestGlobalJoinRejectsMalformedIdentityBeforeAnySQLRead(t *testing.T) {
	s := &svc.ServiceContext{Authority: &authority.Store{}}
	l := NewJoinQueueLogic(context.Background(), s)
	for _, request := range []string{"", "bad", uuid.Nil.String(), "fF000000-0000-0000-0000-000000000001"} {
		_, e := globalJoin(l, &pb.JoinQueueRequest{PlayerId: 1, RequestId: request}, 2)
		require.Equal(t, codes.InvalidArgument, status.Code(e))
	}
}

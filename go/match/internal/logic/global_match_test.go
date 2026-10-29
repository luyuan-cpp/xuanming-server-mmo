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
	"match/internal/config"
	"match/internal/discovery"
	"match/internal/svc"
	"math"
	battlepb "proto/battle"
	pb "proto/match"
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

// 内存替身只实现候选读取端口，生产调度函数仍是实际被测入口；SQL过滤另有真库回归。
func fixtureIndexedChoice(t *testing.T, s *svc.ServiceContext, anchors, candidates []*pb.MatchAuthorityTicketRecord, nodes []discovery.NodeEntry, now uint64, cross bool) *globalChoice {
	t.Helper()
	choice, err := pickIndexedGlobalGroup(context.Background(), s, anchors, nodes, now, cross, func(_ context.Context, anchor *pb.MatchAuthorityTicketRecord, region uint32, local bool, _ uint64, tolerance float64) ([]*pb.MatchAuthorityTicketRecord, error) {
		var found []*pb.MatchAuthorityTicketRecord
		for _, candidate := range candidates {
			if len(found)+1 >= int(anchor.TeamSize) {
				break
			}
			profile, err := authority.DecodeProfile(candidate.NetworkProfile)
			if err != nil || candidate.PlayerId == anchor.PlayerId || authority.Lane(candidate) != authority.Lane(anchor) || candidate.NotBeforeMs > now || candidate.ExpiresMs <= now {
				continue
			}
			if _, ok := authority.Latencies(profile, now, s.Config.GlobalMatch.HardLatencyMs)[region]; !ok {
				continue
			}
			if local && candidate.PreferredRegion != region || !local && now-min(now, candidate.EnqueuedMs) < uint64(s.Config.GlobalMatch.CrossRegionWaitSeconds)*1000 || math.Abs(float64(candidate.RatingCenti-anchor.RatingCenti)) > tolerance {
				continue
			}
			found = append(found, candidate)
		}
		return found, nil
	})
	require.NoError(t, err)
	return choice
}
func TestGlobalRegionPriorityAndHardMemberLatency(t *testing.T) {
	s := &svc.ServiceContext{Config: config.Config{GlobalMatch: config.GlobalMatchConf{HardLatencyMs: 180, CrossRegionWaitSeconds: 30}}}
	nodes := []discovery.NodeEntry{{NodeId: 1, NodeUuid: "a", PhysicalRegion: 1}, {NodeId: 2, NodeUuid: "b", PhysicalRegion: 2}}
	n := uint64(100000)
	a := globalCandidate(1, 0, n, 20, 100)
	b := globalCandidate(2, 0, n, 100, 20)
	c := globalCandidate(3, 90000, n, 30, 120)
	choice := fixtureIndexedChoice(t, s, []*pb.MatchAuthorityTicketRecord{a, b, c}, []*pb.MatchAuthorityTicketRecord{a, b, c}, nodes, n, true)
	require.NotNil(t, choice)
	require.Equal(t, uint32(1), choice.region)
	require.Equal(t, uint64(3), choice.group[1].PlayerId, "本区够人，即使旧票已过扩区时限也先本区")
	choice = fixtureIndexedChoice(t, s, []*pb.MatchAuthorityTicketRecord{a, b}, []*pb.MatchAuthorityTicketRecord{a, b}, nodes, n, true)
	require.NotNil(t, choice, "双方久等后可进入共同可用机房")
	b.EnqueuedMs = 90000
	require.Nil(t, fixtureIndexedChoice(t, s, []*pb.MatchAuthorityTicketRecord{a, b}, []*pb.MatchAuthorityTicketRecord{a, b}, nodes, n, true), "年轻候选不能被队长等待时间强行扩区")
	b = globalCandidate(2, 0, n, 250, 20)
	a = globalCandidate(1, 0, n, 20, 250)
	require.Nil(t, fixtureIndexedChoice(t, s, []*pb.MatchAuthorityTicketRecord{a, b}, []*pb.MatchAuthorityTicketRecord{a, b}, nodes, n, true), "没有全员低于硬上限的共同机房就继续等待")
}
func TestGlobalSoloDoesNotCollectSecondCandidate(t *testing.T) {
	s := &svc.ServiceContext{Config: config.Config{GlobalMatch: config.GlobalMatchConf{HardLatencyMs: 180, CrossRegionWaitSeconds: 30}}}
	a, b := globalCandidate(1, 0, 100000, 20, 100), globalCandidate(2, 0, 100000, 20, 100)
	a.TeamSize = 1
	b.TeamSize = 1
	choice := fixtureIndexedChoice(t, s, []*pb.MatchAuthorityTicketRecord{a, b}, []*pb.MatchAuthorityTicketRecord{a, b}, []discovery.NodeEntry{{NodeId: 1, NodeUuid: "a", PhysicalRegion: 1}}, 100000, true)
	require.NotNil(t, choice)
	require.Len(t, choice.group, 1)
}

func TestGlobalJoinRejectsMalformedIdentityBeforeAnySQLRead(t *testing.T) {
	s := &svc.ServiceContext{Authority: &authority.Store{}}
	l := NewJoinQueueLogic(context.Background(), s)
	for _, request := range []string{"", "bad", uuid.Nil.String(), "fF000000-0000-0000-0000-000000000001"} {
		_, e := globalJoin(l, &pb.JoinQueueRequest{PlayerId: 1, RequestId: request}, 2, 1)
		require.Equal(t, codes.InvalidArgument, status.Code(e))
	}
}

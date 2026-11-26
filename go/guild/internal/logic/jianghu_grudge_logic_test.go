package logic

import (
	"context"
	"sync"
	"testing"

	"guild/internal/constants"
	"guild/internal/data"

	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	pb "proto/guild"
	matchpb "proto/match"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type fakeGuildJianghuStore struct {
	mu      sync.Mutex
	rivals  map[[2]uint64]*pb.GuildJianghuRivalSummaryView
	records map[uint64][]*pb.GuildJianghuGrudgeRecordView
	seen    map[[2]uint64]bool
}

func newFakeGuildJianghuStore() *fakeGuildJianghuStore {
	return &fakeGuildJianghuStore{
		rivals:  make(map[[2]uint64]*pb.GuildJianghuRivalSummaryView),
		records: make(map[uint64][]*pb.GuildJianghuGrudgeRecordView),
		seen:    make(map[[2]uint64]bool),
	}
}

func (f *fakeGuildJianghuStore) RecordJianghuDuelResult(_ context.Context, ev *kafkapb.BattleResultEvent) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ev == nil || ev.GetMatchMode() != uint32(matchpb.MatchMode_MATCH_MODE_JIANGHU_DUEL) ||
		ev.GetTotalRounds() == 0 || ev.GetOutcome() == battlepb.EBattleOutcome_BATTLE_OUTCOME_VOID {
		return false, nil
	}
	d := ev.GetJianghuDuelContext()
	if d == nil || d.GetSideAGuildId() == 0 || d.GetSideBGuildId() == 0 || d.GetSideAGuildId() == d.GetSideBGuildId() {
		return false, nil
	}
	var outA, outB uint32
	switch ev.GetOutcome() {
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN:
		outA, outB = 1, 2
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN:
		outA, outB = 2, 1
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW:
		outA, outB = 3, 3
	default:
		return false, nil
	}
	if f.seen[[2]uint64{d.GetSideAGuildId(), ev.GetBattleId()}] {
		return false, nil
	}
	apply := func(gid, rid uint64, rname string, out uint32, ourCap, rivalCap string) {
		f.seen[[2]uint64{gid, ev.GetBattleId()}] = true
		k := [2]uint64{gid, rid}
		rv, ok := f.rivals[k]
		if !ok {
			rv = &pb.GuildJianghuRivalSummaryView{RivalGuildId: rid}
			f.rivals[k] = rv
		}
		rv.RivalGuildName = rname
		rv.LastBattleMs = ev.GetFinishedAtMs()
		switch out {
		case 1:
			rv.Wins++
		case 2:
			rv.Losses++
		case 3:
			rv.Draws++
		}
		rec := &pb.GuildJianghuGrudgeRecordView{
			BattleId:         ev.GetBattleId(),
			RivalGuildId:     rid,
			RivalGuildName:   rname,
			OutcomeForGuild:  out,
			WritTier:         d.GetWritTier(),
			TeamSize:         d.GetTeamSize(),
			TotalRounds:      ev.GetTotalRounds(),
			OurCaptainName:   ourCap,
			RivalCaptainName: rivalCap,
			FinishedMs:       ev.GetFinishedAtMs(),
		}
		list := append([]*pb.GuildJianghuGrudgeRecordView{rec}, f.records[gid]...)
		if len(list) > data.MaxGuildJianghuWallRecords {
			list = list[:data.MaxGuildJianghuWallRecords]
		}
		f.records[gid] = list
	}
	apply(d.GetSideAGuildId(), d.GetSideBGuildId(), d.GetSideBGuildName(), outA, d.GetChallengerCaptainName(), d.GetTargetCaptainName())
	apply(d.GetSideBGuildId(), d.GetSideAGuildId(), d.GetSideAGuildName(), outB, d.GetTargetCaptainName(), d.GetChallengerCaptainName())
	return true, nil
}

func (f *fakeGuildJianghuStore) GetGuildJianghuGrudgeWall(_ context.Context, guildID uint64) ([]*pb.GuildJianghuRivalSummaryView, []*pb.GuildJianghuGrudgeRecordView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var rivals []*pb.GuildJianghuRivalSummaryView
	for k, v := range f.rivals {
		if k[0] == guildID {
			rivals = append(rivals, proto.Clone(v).(*pb.GuildJianghuRivalSummaryView))
		}
	}
	var records []*pb.GuildJianghuGrudgeRecordView
	for _, r := range f.records[guildID] {
		records = append(records, proto.Clone(r).(*pb.GuildJianghuGrudgeRecordView))
	}
	return rivals, records, nil
}

func TestGuildJianghuGrudgeWall_PureGuildRecordedIdempotentlyAndMemberScoped(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	seedGuild(t, mr, data.GuildData{
		GuildID: 801, ZoneID: 1, Level: 5,
		Members: []data.MemberData{{PlayerID: 1001, Role: constants.RoleMember}},
	}, 0)
	seedGuild(t, mr, data.GuildData{
		GuildID: 802, ZoneID: 1, Level: 5,
		Members: []data.MemberData{{PlayerID: 2001, Role: constants.RoleMember}},
	}, 0)
	seedMembership(t, mr, 1001, 801)
	seedMembership(t, mr, 2001, 802)

	store := newFakeGuildJianghuStore()
	l := NewGuildLogic(repo, nil, nil, nil, WithJianghuGrudgeStore(store))

	// 1. 纯两帮生死状入账，重复投递幂等。
	ev := &kafkapb.BattleResultEvent{
		BattleId:     70001,
		MatchMode:    uint32(matchpb.MatchMode_MATCH_MODE_JIANGHU_DUEL),
		Outcome:      battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN,
		TotalRounds:  7,
		FinishedAtMs: 1_730_000_000_000,
		JianghuDuelContext: &battlepb.JianghuDuelBattleContext{
			DuelInviteId:          555,
			WritTier:              2,
			ChallengerCaptainId:   1001,
			ChallengerCaptainName: "QingFeng",
			TargetCaptainId:       2001,
			TargetCaptainName:     "MoYuan",
			SideAGuildId:          801,
			SideAGuildName:        "QingYun",
			SideBGuildId:          802,
			SideBGuildName:        "CangLan",
			TeamSize:              3,
		},
	}
	require.NoError(t, l.RecordJianghuDuelResult(context.Background(), ev))
	require.NoError(t, l.RecordJianghuDuelResult(context.Background(), ev)) // 重复投递不重复累计

	// 2. 散人/混编(side_b_guild_id=0)与作废局(VOID)不入帮派恩怨墙。
	mixed := proto.Clone(ev).(*kafkapb.BattleResultEvent)
	mixed.BattleId = 70002
	mixed.JianghuDuelContext.SideBGuildId = 0
	require.NoError(t, l.RecordJianghuDuelResult(context.Background(), mixed))

	voidEv := proto.Clone(ev).(*kafkapb.BattleResultEvent)
	voidEv.BattleId = 70003
	voidEv.Outcome = battlepb.EBattleOutcome_BATTLE_OUTCOME_VOID
	voidEv.TotalRounds = 0
	require.NoError(t, l.RecordJianghuDuelResult(context.Background(), voidEv))

	// 3. 801 成员查询 -> 看到 1 胜 0 负
	w1, err := l.GetGuildJianghuGrudgeWall(clientCtx(1001), &pb.GetGuildJianghuGrudgeWallRequest{})
	require.NoError(t, err)
	require.Zero(t, w1.GetErrorMessage().GetId())
	require.Equal(t, uint64(801), w1.GetGuildId())
	require.Len(t, w1.GetRivals(), 1)
	require.Equal(t, uint32(1), w1.GetRivals()[0].GetWins())
	require.Equal(t, uint32(0), w1.GetRivals()[0].GetLosses())
	require.Len(t, w1.GetRecentRecords(), 1)

	// 4. 802 成员查询 -> 看到 0 胜 1 负
	w2, err := l.GetGuildJianghuGrudgeWall(clientCtx(2001), &pb.GetGuildJianghuGrudgeWallRequest{})
	require.NoError(t, err)
	require.Zero(t, w2.GetErrorMessage().GetId())
	require.Equal(t, uint64(802), w2.GetGuildId())
	require.Len(t, w2.GetRivals(), 1)
	require.Equal(t, uint32(0), w2.GetRivals()[0].GetWins())
	require.Equal(t, uint32(1), w2.GetRivals()[0].GetLosses())

	// 5. 无会话内部调用被 PermissionDenied 拒绝
	_, err = l.GetGuildJianghuGrudgeWall(context.Background(), &pb.GetGuildJianghuGrudgeWallRequest{})
	require.Error(t, err)
}

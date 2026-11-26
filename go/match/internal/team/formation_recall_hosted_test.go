package team

import (
	"context"
	"testing"

	"match/internal/playercontract"

	plpb "proto/player_locator"
	teampb "proto/team"
	"shared/generated/pb/table"
	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestSetFormationRulesAndView(t *testing.T) {
	now := uint64(50_000)
	sessions := Sessions{101: SessionOnline, 102: SessionOnline}
	cfg := RuleConfig{AllowCrossZone: true}

	rec := Apply(CreateOp(101, 7001, 1), nil, now, sessions, cfg).Record
	rec = Apply(ApplyOp(102, 1), rec, now+10, sessions, cfg).Record
	rec = Apply(HandleApplicationOp(101, 102, true), rec, now+20, sessions, cfg).Record

	// 非队长不能切换队形
	dDenied := Apply(SetFormationOp(102, teampb.TeamFollowFormationStyle_TEAM_FOLLOW_FORMATION_ECHELON), rec, now+30, sessions, cfg)
	require.Equal(t, ErrNotLeader, dDenied.Code)
	require.False(t, dDenied.Changed)

	// 非法枚举拒绝
	dInvalid := Apply(SetFormationOp(101, teampb.TeamFollowFormationStyle(99)), rec, now+35, sessions, cfg)
	require.Equal(t, uint32(table.CommonError_kInvalidParameter), dInvalid.Code)

	// 队长切到雁行两翼: Changed=true, FormationChanged=true, Reason=FORMATION_CHANGED
	dSet := Apply(SetFormationOp(101, teampb.TeamFollowFormationStyle_TEAM_FOLLOW_FORMATION_ECHELON), rec, now+40, sessions, cfg)
	require.Equal(t, uint32(0), dSet.Code)
	require.True(t, dSet.Changed)
	require.True(t, dSet.FormationChanged)
	require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_FORMATION_CHANGED, dSet.Reason)
	require.Equal(t, teampb.TeamFollowFormationStyle_TEAM_FOLLOW_FORMATION_ECHELON, dSet.Record.GetFollowFormation())
	require.ElementsMatch(t, []uint64{101, 102}, dSet.Kept)
	rec = dSet.Record

	// 相同队形幂等无变更
	dSame := Apply(SetFormationOp(101, teampb.TeamFollowFormationStyle_TEAM_FOLLOW_FORMATION_ECHELON), rec, now+50, sessions, cfg)
	require.Equal(t, uint32(0), dSame.Code)
	require.False(t, dSame.Changed)
	require.False(t, dSame.FormationChanged)

	// TeamView 正确透传 follow_formation
	view := teamViewFor(102, rec, 3, 1, now+60, displayCache{}, nil)
	require.Equal(t, teampb.TeamFollowFormationStyle_TEAM_FOLLOW_FORMATION_ECHELON, view.GetFollowFormation())
}

func TestRecallMembersCooldownAndRecipients(t *testing.T) {
	now := uint64(100_000)
	sessions := Sessions{101: SessionOnline, 102: SessionOnline, 103: SessionOnline}
	cfg := RuleConfig{AllowCrossZone: true}

	rec := Apply(CreateOp(101, 7002, 1), nil, now, sessions, cfg).Record
	rec = Apply(ApplyOp(102, 1), rec, now+10, sessions, cfg).Record
	rec = Apply(HandleApplicationOp(101, 102, true), rec, now+20, sessions, cfg).Record
	rec = Apply(ApplyOp(103, 1), rec, now+30, sessions, cfg).Record
	rec = Apply(HandleApplicationOp(101, 103, true), rec, now+40, sessions, cfg).Record

	// 队员不能发起一键召集
	dDenied := Apply(RecallMembersOp(102), rec, now+50, sessions, cfg)
	require.Equal(t, ErrNotLeader, dDenied.Code)

	// 队长首次召集成功:只通知非队长成员,记录 LastRecallAtMs,不广播空 reason 快照
	d1 := Apply(RecallMembersOp(101), rec, now+100, sessions, cfg)
	require.Equal(t, uint32(0), d1.Code)
	require.True(t, d1.Changed)
	require.Equal(t, now+100, d1.Record.GetLastRecallAtMs())
	require.ElementsMatch(t, []uint64{102, 103}, d1.RecallRecipients)
	require.Nil(t, snapshotRecipients(&CommitResult{TeamId: 7002, Decision: d1}))
	rec = d1.Record

	// 冷却期内(15s 内)再次召集被限流拒绝
	dCool := Apply(RecallMembersOp(101), rec, now+100+RecallCooldownMs-1, sessions, cfg)
	require.Equal(t, uint32(table.CommonError_kRateLimitExceeded), dCool.Code)
	require.False(t, dCool.Changed)

	// 冷却到期后再次召集成功
	d2 := Apply(RecallMembersOp(101), rec, now+100+RecallCooldownMs, sessions, cfg)
	require.Equal(t, uint32(0), d2.Code)
	require.True(t, d2.Changed)
	require.Equal(t, now+100+RecallCooldownMs, d2.Record.GetLastRecallAtMs())
	require.ElementsMatch(t, []uint64{102, 103}, d2.RecallRecipients)
}

func TestPresenceHostedFlagOnlyWhenOfflineWithHostedHeartbeat(t *testing.T) {
	mr := miniredis.RunT(t)
	rds := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rds.Close()

	// 101: 在线且残留 hosted_presence -> IsOnline=true, IsHosted=false
	onlineRaw, err := proto.Marshal(&plpb.PlayerSession{State: plpb.PlayerSessionState_SESSION_STATE_ONLINE})
	require.NoError(t, err)
	require.NoError(t, mr.Set(playercontract.SessionKey(101), string(onlineRaw)))
	require.NoError(t, mr.Set(redisslot.HostedPresenceKey(101), "scene-node-1"))

	// 102: 离线(无 session)且有 hosted_presence -> IsOnline=false, IsHosted=true
	require.NoError(t, mr.Set(redisslot.HostedPresenceKey(102), "scene-node-2"))

	// 103: 离线且无 hosted_presence -> IsOnline=false, IsHosted=false
	dc := (presenceReader{rds: rds}).loadDisplay(context.Background(), []uint64{101, 102, 103})

	v101 := memberView(101, 1, 1, 101, dc)
	require.True(t, v101.GetIsOnline())
	require.False(t, v101.GetIsHosted())

	v102 := memberView(102, 1, 2, 101, dc)
	require.False(t, v102.GetIsOnline())
	require.True(t, v102.GetIsHosted())

	v103 := memberView(103, 1, 3, 101, dc)
	require.False(t, v103.GetIsOnline())
	require.False(t, v103.GetIsHosted())
}

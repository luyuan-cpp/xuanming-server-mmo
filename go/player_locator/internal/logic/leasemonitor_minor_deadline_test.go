package logic

// 未成年人的离线托管不越过当天的截止时刻(docs/design/real-name-anti-addiction.md §5.4)。
// 可玩时段之外不能以任何形式提供游戏服务:带着月卡断线的未成年人,角色不能留在世界里挂机过夜。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "proto/player_locator"
	"shared/redisslot"
)

func TestHostedLeaseSecondsStopsAtAMinorsPlayDeadline(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	deadlineIn := func(d time.Duration) *pb.PlayerSession {
		return &pb.PlayerSession{MinorPlayDeadlineMs: uint64(now.Add(d).UnixMilli())}
	}
	cases := []struct {
		name    string
		session *pb.PlayerSession
		want    int64
	}{
		{"no deadline keeps the full lease", &pb.PlayerSession{}, afkPassLeaseTTLSeconds},
		{"a deadline far ahead keeps the full lease", deadlineIn(time.Hour), afkPassLeaseTTLSeconds},
		{"a deadline inside the lease caps it", deadlineIn(90 * time.Second), 90},
		{"a fraction of a second rounds up so the next expiry is not early", deadlineIn(1500 * time.Millisecond), 2},
		{"at the deadline hosting ends", deadlineIn(0), 0},
		{"past the deadline hosting ends", deadlineIn(-time.Minute), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, hostedLeaseSeconds(c.session, now))
		})
	}
}

// minorLeaseDue 建一条带月卡、带归位记录的断线会话,租约 1 秒后到期;deadline 是会话记录里的未成年人截止时刻。
func minorLeaseDue(t *testing.T, playerID uint64, deadline time.Time) (*leaseMonitor, *fakeSceneManagerClient, func() bool) {
	t.Helper()
	sc, mr, fake := hostedResumeSetup(t)
	ctx := context.Background()
	session := makeTestSession(playerID)
	session.GateId, session.GateInstanceId = "", ""
	session.SessionId, session.LoginGeneration = hostedResumeSession, hostedResumeGeneration
	session.MinorPlayDeadlineMs = uint64(deadline.UnixMilli())
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: session})
	require.NoError(t, err)
	_, err = NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: session.SessionId, LeaseTtlSeconds: 1,
	})
	require.NoError(t, err)
	hostedResumeSeed(t, mr, playerID, time.Now())
	m := scanTestMonitor(sc, fixedSlots{redisslot.PlayerSlot(playerID)}, LeaseMonitorParams{BatchSize: 10},
		time.Now().Add(5*time.Second))
	return m, fake, func() bool { return mr.Exists(sessionKey(playerID)) }
}

// 截止时刻已过:月卡有效也不续租、不归位,按普通断线收尾。
func TestLeaseMonitor_MinorPastThePlayDeadlineIsNotHosted(t *testing.T) {
	m, fake, _ := minorLeaseDue(t, 31011, time.Now().Add(-time.Second))

	m.tick(context.Background())

	assert.Empty(t, fake.enterCalls(), "过了截止时刻:不归位")
	require.Len(t, fake.leaveCalls(), 1, "按普通断线收尾,scene 存盘下线")
}

// 截止时刻还没到:照常托管(续租、对账归位),和成年人一样。
func TestLeaseMonitor_MinorBeforeThePlayDeadlineIsStillHosted(t *testing.T) {
	m, fake, sessionAlive := minorLeaseDue(t, 31012, time.Now().Add(time.Hour))

	m.tick(context.Background())

	assert.Empty(t, fake.leaveCalls(), "还在可玩时段里:不清理")
	assert.True(t, sessionAlive())
	require.Len(t, fake.enterCalls(), 1)
}

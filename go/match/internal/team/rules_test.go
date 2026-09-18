package team

// 纯函数规则的表驱动单测(设计文档 docs/design/team-system.md §I.3 #1-#5)。
// 不起 Redis、不读墙钟:nowMs 一律是固定常量。

import (
	"testing"

	teampb "proto/team"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const (
	ruleNow  uint64 = 1_900_000_000_000
	ruleTid  uint64 = 7_000_001
	ruleZone uint32 = 1
)

// newRecord 造一支队伍:members[0] 是队长,join_seq 依次 1..n。
func newRecord(members ...uint64) *teampb.TeamRecord {
	rec := &teampb.TeamRecord{TeamId: ruleTid, LeaderId: members[0], ZoneId: ruleZone, CreatedAtMs: ruleNow - 1000}
	for i, pid := range members {
		rec.Members = append(rec.Members, &teampb.TeamMemberRecord{
			PlayerId: pid, ZoneId: ruleZone, JoinedAtMs: ruleNow - 1000, JoinSeq: uint32(i + 1),
		})
	}
	rec.NextJoinSeq = uint32(len(members) + 1)
	return rec
}

func withApplication(rec *teampb.TeamRecord, pid uint64, zone uint32, appliedAt, expireAt uint64) *teampb.TeamRecord {
	rec.Applications = append(rec.Applications, &teampb.TeamApplicationRecord{
		PlayerId: pid, ZoneId: zone, AppliedAtMs: appliedAt, ExpireAtMs: expireAt,
	})
	return rec
}

func withInvite(rec *teampb.TeamRecord, invitee uint64, zone uint32, invitedAt, expireAt uint64) *teampb.TeamRecord {
	rec.Invites = append(rec.Invites, &teampb.TeamInviteRecord{
		InviteeId: invitee, InviterId: rec.GetLeaderId(), ZoneId: zone, InvitedAtMs: invitedAt, ExpireAtMs: expireAt,
	})
	return rec
}

func withLock(rec *teampb.TeamRecord, expireAt uint64) *teampb.TeamRecord {
	rec.MatchLockToken = "lock-token"
	rec.MatchLockExpireAtMs = expireAt
	rec.MatchLockRoster = MemberIds(rec)
	return rec
}

func allOnline(ids ...uint64) Sessions {
	s := Sessions{}
	for _, id := range ids {
		s[id] = SessionOnline
	}
	return s
}

func withCallerTeam(op Op, tid uint64) Op {
	op.CallerTeamId = tid
	return op
}

// #1:§D.5 每一行的拒绝分支。拒绝时不产生记录、不改入参。
func TestApplyRejections(t *testing.T) {
	full := func() *teampb.TeamRecord { return newRecord(1, 2, 3, 4, 5) }
	cases := []struct {
		name      string
		op        Op
		rec       *teampb.TeamRecord
		sessions  Sessions
		cfg       RuleConfig
		wantCode  uint32
		wantParam uint64
	}{
		{name: "create 记录已存在", op: CreateOp(1, ruleTid, ruleZone), rec: newRecord(9), wantCode: ErrInternal},
		{name: "create team_id 为 0", op: CreateOp(1, 0, ruleZone), wantCode: ErrInternal},
		{name: "create zone 未知", op: CreateOp(1, ruleTid, 0), wantCode: ErrHomeZoneUnknown},
		{name: "create 已在队", op: withCallerTeam(CreateOp(1, ruleTid, ruleZone), 42), wantCode: ErrMemberInTeam},
		{name: "非建队记录缺失", op: LeaveOp(1), wantCode: ErrNoTeam},

		{name: "apply 已是成员", op: ApplyOp(2, ruleZone), rec: newRecord(1, 2), wantCode: ErrMemberInTeam},
		{name: "apply 已在别队", op: withCallerTeam(ApplyOp(3, ruleZone), 99), rec: newRecord(1), wantCode: ErrMemberInTeam},
		{name: "apply zone 未知", op: ApplyOp(3, 0), rec: newRecord(1), wantCode: ErrHomeZoneUnknown},
		{name: "apply 跨区", op: ApplyOp(3, 2), rec: newRecord(1), wantCode: ErrCrossZoneDenied},
		{name: "apply 队满", op: ApplyOp(6, ruleZone), rec: full(), wantCode: ErrMembersFull},

		{name: "handle 非队长", op: HandleApplicationOp(2, 3, true),
			rec: withApplication(newRecord(1, 2), 3, ruleZone, ruleNow, ruleNow+1), wantCode: ErrNotLeader},
		{name: "handle 申请不存在", op: HandleApplicationOp(1, 3, true), rec: newRecord(1), wantCode: ErrApplicationNotFound},
		{name: "handle 申请已过期", op: HandleApplicationOp(1, 3, true),
			rec: withApplication(newRecord(1), 3, ruleZone, ruleNow-5, ruleNow), wantCode: ErrApplicationNotFound},
		{name: "handle 开战锁", op: HandleApplicationOp(1, 3, true),
			rec: withLock(withApplication(newRecord(1), 3, ruleZone, ruleNow, ruleNow+1), ruleNow+1), wantCode: ErrInMatch},
		{name: "handle 队满", op: HandleApplicationOp(1, 6, true),
			rec: withApplication(full(), 6, ruleZone, ruleNow, ruleNow+1), wantCode: ErrMembersFull},
		{name: "handle 申请记录跨区", op: HandleApplicationOp(1, 3, true),
			rec: withApplication(newRecord(1), 3, 2, ruleNow, ruleNow+1), wantCode: ErrCrossZoneDenied},

		{name: "invite 非队长", op: InviteOp(2, 3, ruleZone), rec: newRecord(1, 2), wantCode: ErrNotLeader},
		{name: "invite 自己", op: InviteOp(1, 1, ruleZone), rec: newRecord(1), wantCode: ErrPlayerId},
		{name: "invite 0", op: InviteOp(1, 0, ruleZone), rec: newRecord(1), wantCode: ErrPlayerId},
		{name: "invite 目标已是成员", op: InviteOp(1, 2, ruleZone), rec: newRecord(1, 2), wantCode: ErrMemberInTeam, wantParam: 2},
		{name: "invite zone 未知", op: InviteOp(1, 3, 0), rec: newRecord(1), wantCode: ErrHomeZoneUnknown},
		{name: "invite 跨区", op: InviteOp(1, 3, 2), rec: newRecord(1), wantCode: ErrCrossZoneDenied},
		{name: "invite 队满", op: InviteOp(1, 6, ruleZone), rec: full(), wantCode: ErrMembersFull},

		{name: "respond accept 无邀请", op: RespondInviteOp(3, true), rec: newRecord(1), wantCode: ErrInviteNotFound},
		{name: "respond accept 邀请已过期", op: RespondInviteOp(3, true),
			rec: withInvite(newRecord(1), 3, ruleZone, ruleNow-5, ruleNow), wantCode: ErrInviteNotFound},
		{name: "respond accept 开战锁", op: RespondInviteOp(3, true),
			rec: withLock(withInvite(newRecord(1), 3, ruleZone, ruleNow, ruleNow+1), ruleNow+1), wantCode: ErrInMatch},
		{name: "respond accept 已在别队", op: withCallerTeam(RespondInviteOp(3, true), 99),
			rec: withInvite(newRecord(1), 3, ruleZone, ruleNow, ruleNow+1), wantCode: ErrMemberInTeam},
		{name: "respond accept 队满", op: RespondInviteOp(6, true),
			rec: withInvite(full(), 6, ruleZone, ruleNow, ruleNow+1), wantCode: ErrMembersFull},
		{name: "respond accept 邀请记录跨区", op: RespondInviteOp(3, true),
			rec: withInvite(newRecord(1), 3, 2, ruleNow, ruleNow+1), wantCode: ErrCrossZoneDenied},

		{name: "leave 开战锁", op: withCallerTeam(LeaveOp(2), ruleTid), rec: withLock(newRecord(1, 2), ruleNow+1), wantCode: ErrInMatch},

		{name: "kick 非队长", op: KickOp(2, 3), rec: newRecord(1, 2, 3), wantCode: ErrKickNotLeader},
		{name: "kick 自己", op: KickOp(1, 1), rec: newRecord(1, 2), wantCode: ErrKickSelf},
		{name: "kick 目标不在队", op: KickOp(1, 9), rec: newRecord(1, 2), wantCode: ErrMemberNotInTeam, wantParam: 9},
		{name: "kick 开战锁", op: KickOp(1, 2), rec: withLock(newRecord(1, 2), ruleNow+1), wantCode: ErrInMatch},

		{name: "transfer 目标 0", op: TransferLeaderOp(1, 0), rec: newRecord(1, 2), wantCode: ErrPlayerId},
		{name: "transfer 给自己", op: TransferLeaderOp(1, 1), rec: newRecord(1, 2), wantCode: ErrAppointSelf},
		{name: "transfer 非队长", op: TransferLeaderOp(2, 3), rec: newRecord(1, 2, 3), sessions: allOnline(1, 2, 3), wantCode: ErrAppointNotLeader},
		{name: "transfer 目标不在队", op: TransferLeaderOp(1, 9), rec: newRecord(1, 2), wantCode: ErrMemberNotInTeam, wantParam: 9},
		{name: "transfer 目标断线中", op: TransferLeaderOp(1, 2), rec: newRecord(1, 2),
			sessions: Sessions{1: SessionOnline, 2: SessionPresent}, wantCode: ErrMemberOffline, wantParam: 2},
		{name: "transfer 目标会话未知", op: TransferLeaderOp(1, 2), rec: newRecord(1, 2), wantCode: ErrMemberOffline, wantParam: 2},
		{name: "transfer 开战锁", op: TransferLeaderOp(1, 2), rec: withLock(newRecord(1, 2), ruleNow+1),
			sessions: allOnline(1, 2), wantCode: ErrInMatch},

		{name: "disband 非队长", op: DisbandOp(2), rec: newRecord(1, 2), wantCode: ErrDisbandNotLeader},
		{name: "disband 开战锁", op: DisbandOp(1), rec: withLock(newRecord(1, 2), ruleNow+1), wantCode: ErrInMatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var before *teampb.TeamRecord
			if tc.rec != nil {
				before = proto.Clone(tc.rec).(*teampb.TeamRecord)
			}
			d := Apply(tc.op, tc.rec, ruleNow, tc.sessions, tc.cfg)
			require.Equal(t, tc.wantCode, d.Code)
			require.Equal(t, tc.wantParam, d.CodeParam)
			require.False(t, d.Changed, "拒绝不能要求提交")
			require.Nil(t, d.Record)
			if before != nil {
				require.True(t, proto.Equal(before, tc.rec), "规则不得修改入参记录")
			}
		})
	}
}

func TestMatchLockActiveBoundary(t *testing.T) {
	rec := withLock(newRecord(1), ruleNow+10)
	require.True(t, MatchLockActive(rec, ruleNow+9))
	require.False(t, MatchLockActive(rec, ruleNow+10), "now == expire 视为已过期")
	rec.MatchLockToken = ""
	require.False(t, MatchLockActive(rec, ruleNow))
	require.False(t, MatchLockActive(nil, ruleNow))
}

// 锁过期后名单变更照常放行。
func TestExpiredLockAllowsRosterChange(t *testing.T) {
	d := Apply(KickOp(1, 2), withLock(newRecord(1, 2), ruleNow), ruleNow, nil, RuleConfig{})
	require.Zero(t, d.Code)
	require.Equal(t, []uint64{2}, d.Left)
}

func TestCreateBuildsRecord(t *testing.T) {
	d := Apply(CreateOp(11, ruleTid, 3), nil, ruleNow, nil, RuleConfig{})
	require.Zero(t, d.Code)
	require.True(t, d.Changed)
	require.Equal(t, []uint64{11}, d.Joined)
	require.Empty(t, d.Kept)
	require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_CREATED, d.Reason)
	require.Equal(t, ruleTid, d.Record.GetTeamId())
	require.Equal(t, uint64(11), d.Record.GetLeaderId())
	require.Equal(t, uint32(3), d.Record.GetZoneId())
	require.Equal(t, ruleNow, d.Record.GetCreatedAtMs())
	require.Equal(t, uint32(1), d.Record.GetMembers()[0].GetJoinSeq())
	require.Equal(t, uint32(2), d.Record.GetNextJoinSeq())
}

// #2:AllowCrossZone 两组;开关中途改 false 时审批与接受被拦。
func TestCrossZoneSwitch(t *testing.T) {
	allow, deny := RuleConfig{AllowCrossZone: true}, RuleConfig{}

	d := Apply(ApplyOp(3, 2), newRecord(1), ruleNow, nil, allow)
	require.Zero(t, d.Code)
	require.Equal(t, uint32(2), d.Record.GetApplications()[0].GetZoneId(), "申请记录存申请人 zone")

	pending := d.Record
	d = Apply(HandleApplicationOp(1, 3, true), pending, ruleNow, nil, deny)
	require.Equal(t, ErrCrossZoneDenied, d.Code, "开关改 false 后按申请记录里的 zone 复核")

	d = Apply(HandleApplicationOp(1, 3, true), pending, ruleNow, nil, allow)
	require.Zero(t, d.Code)
	require.Equal(t, uint32(2), FindMember(d.Record, 3).GetZoneId(), "成员 zone 写进记录")

	d = Apply(InviteOp(1, 4, 2), newRecord(1), ruleNow, nil, allow)
	require.Zero(t, d.Code)
	invited := d.Record
	require.Equal(t, ErrCrossZoneDenied, Apply(RespondInviteOp(4, true), invited, ruleNow, nil, deny).Code)
	require.Zero(t, Apply(RespondInviteOp(4, true), invited, ruleNow, nil, allow).Code)

	// 已有跨区成员保留,只拦新增。
	mixed := newRecord(1, 2)
	mixed.Members[1].ZoneId = 2
	d = Apply(KickOp(1, 2), mixed, ruleNow, nil, deny)
	require.Zero(t, d.Code)
}

// #3:离队转让、最后一人解散、惰性转让。
func TestLeaderHandover(t *testing.T) {
	t.Run("队长离队转给在线且 join_seq 最小的", func(t *testing.T) {
		sessions := Sessions{2: SessionPresent, 3: SessionOnline, 4: SessionOnline}
		d := Apply(withCallerTeam(LeaveOp(1), ruleTid), newRecord(1, 2, 3, 4), ruleNow, sessions, RuleConfig{})
		require.Zero(t, d.Code)
		require.Equal(t, uint64(3), d.Record.GetLeaderId())
		require.Equal(t, []uint64{1}, d.Left)
		require.Equal(t, []uint64{2, 3, 4}, d.Kept)
		require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MEMBER_LEFT, d.Reason)
	})
	t.Run("全员离线时取 join_seq 最小的", func(t *testing.T) {
		d := Apply(withCallerTeam(LeaveOp(1), ruleTid), newRecord(1, 3, 2), ruleNow, nil, RuleConfig{})
		require.Equal(t, uint64(3), d.Record.GetLeaderId(), "按 join_seq 而非 player_id")
	})
	t.Run("非队长离队不换队长", func(t *testing.T) {
		d := Apply(withCallerTeam(LeaveOp(2), ruleTid), newRecord(1, 2), ruleNow, allOnline(1, 2), RuleConfig{})
		require.Equal(t, uint64(1), d.Record.GetLeaderId())
	})
	t.Run("最后一人离队解散", func(t *testing.T) {
		rec := withInvite(withInvite(newRecord(1), 7, ruleZone, ruleNow, ruleNow+5), 8, ruleZone, ruleNow-9, ruleNow)
		rec = withApplication(rec, 9, ruleZone, ruleNow, ruleNow+5)
		d := Apply(withCallerTeam(LeaveOp(1), ruleTid), rec, ruleNow, nil, RuleConfig{})
		require.Zero(t, d.Code)
		require.True(t, d.Changed)
		require.True(t, d.Disbanded)
		require.Nil(t, d.Record)
		require.Equal(t, []uint64{1}, d.Left)
		require.Empty(t, d.Joined)
		require.Empty(t, d.Kept)
		require.ElementsMatch(t, []uint64{7, 8}, d.InvitesRemoved, "全部邀请(含过期)反查项都要删")
		require.Equal(t, []uint64{7}, d.RevokedInvitees, "只通知未过期邀请的被邀请人")
	})
	t.Run("惰性转让:队长会话不存在", func(t *testing.T) {
		sessions := Sessions{1: SessionAbsent, 2: SessionPresent, 3: SessionOnline}
		d := Apply(RefreshOp(2), newRecord(1, 2, 3), ruleNow, sessions, RuleConfig{})
		require.Zero(t, d.Code)
		require.True(t, d.Changed)
		require.True(t, d.LeaderOfflineTransferred)
		require.Equal(t, uint64(3), d.Record.GetLeaderId())
		require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_LEADER_OFFLINE_TRANSFERRED, d.Reason)
		require.Equal(t, []uint64{1, 2, 3}, d.Kept)
	})
	t.Run("惰性转让:没有在线成员不转", func(t *testing.T) {
		d := Apply(RefreshOp(2), newRecord(1, 2), ruleNow, Sessions{1: SessionAbsent, 2: SessionPresent}, RuleConfig{})
		require.False(t, d.Changed)
	})
	t.Run("惰性转让:断线宽限 / 读失败都不转", func(t *testing.T) {
		for _, state := range []SessionState{SessionPresent, SessionUnknown, SessionOnline} {
			d := Apply(RefreshOp(2), newRecord(1, 2), ruleNow, Sessions{1: state, 2: SessionOnline}, RuleConfig{})
			require.False(t, d.Changed, "state=%d", state)
		}
	})
	t.Run("惰性转让后新队长可直接执行队长操作", func(t *testing.T) {
		sessions := Sessions{1: SessionAbsent, 2: SessionOnline, 3: SessionOnline}
		d := Apply(KickOp(2, 3), newRecord(1, 2, 3), ruleNow, sessions, RuleConfig{})
		require.Zero(t, d.Code)
		require.True(t, d.LeaderOfflineTransferred)
		require.Equal(t, uint64(2), d.Record.GetLeaderId())
		require.Equal(t, []uint64{3}, d.Left)
		require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MEMBER_KICKED, d.Reason)
	})
}

// #4:申请与邀请的 FIFO 淘汰、重复刷新、过期清理。
func TestApplicationsFifoRefreshAndExpiry(t *testing.T) {
	rec := newRecord(1)
	for i := 0; i < MaxApplications; i++ {
		rec = withApplication(rec, uint64(100+i), ruleZone, ruleNow-uint64(100-i), ruleNow+ApplicationTTLMs)
	}
	d := Apply(ApplyOp(200, ruleZone), rec, ruleNow, nil, RuleConfig{})
	require.Zero(t, d.Code)
	require.Len(t, d.Record.GetApplications(), MaxApplications)
	require.Nil(t, findApplication(d.Record, 100), "最早一条被淘汰")
	require.NotNil(t, findApplication(d.Record, 200))
	require.Equal(t, []uint64{1}, d.Kept)
	require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_APPLICATION_CHANGED, d.Reason)

	// 重复申请:刷新 expire,保留 applied_at。
	later := ruleNow + 30_000
	d2 := Apply(ApplyOp(200, ruleZone), d.Record, later, nil, RuleConfig{})
	require.Zero(t, d2.Code)
	app := findApplication(d2.Record, 200)
	require.Equal(t, ruleNow, app.GetAppliedAtMs())
	require.Equal(t, later+ApplicationTTLMs, app.GetExpireAtMs())
	require.Len(t, d2.Record.GetApplications(), MaxApplications)

	// 同一毫秒内挤满:刚加的不能被挤掉。
	same := newRecord(1)
	for i := 0; i < MaxApplications; i++ {
		same = withApplication(same, uint64(500+i), ruleZone, ruleNow, ruleNow+1)
	}
	d3 := Apply(ApplyOp(3, ruleZone), same, ruleNow, nil, RuleConfig{})
	require.NotNil(t, findApplication(d3.Record, 3))
	require.Nil(t, findApplication(d3.Record, 500))

	// 过期清理:expire <= now 即过期,GetMyTeam 发现后要求提交。
	exp := withApplication(withApplication(newRecord(1), 7, ruleZone, ruleNow-10, ruleNow), 8, ruleZone, ruleNow-10, ruleNow+1)
	d4 := Apply(RefreshOp(1), exp, ruleNow, nil, RuleConfig{})
	require.True(t, d4.Changed)
	require.Nil(t, findApplication(d4.Record, 7))
	require.NotNil(t, findApplication(d4.Record, 8))
	require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_APPLICATION_CHANGED, d4.Reason)
	require.False(t, Apply(RefreshOp(1), d4.Record, ruleNow, nil, RuleConfig{}).Changed, "清理完再读不再变化")
}

func TestInvitesFifoRefreshAndExpiry(t *testing.T) {
	rec := newRecord(1)
	for i := 0; i < MaxInvitesPerTeam; i++ {
		rec = withInvite(rec, uint64(100+i), ruleZone, ruleNow-uint64(100-i), ruleNow+InviteTTLMs)
	}
	d := Apply(InviteOp(1, 200, ruleZone), rec, ruleNow, nil, RuleConfig{})
	require.Zero(t, d.Code)
	require.Len(t, d.Record.GetInvites(), MaxInvitesPerTeam)
	require.Nil(t, findInvite(d.Record, 100))
	require.Equal(t, []InviteIndexAdd{{InviteeId: 200, ExpireAtMs: ruleNow + InviteTTLMs}}, d.InvitesAdded)
	require.Equal(t, []uint64{100}, d.InvitesRemoved, "被淘汰者进入 ID")
	require.Equal(t, uint64(200), d.InvitedPlayer)
	require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_INVITE_CHANGED, d.Reason)

	// 重复邀请:刷新 expire,仍写 IA,不进 ID。
	later := ruleNow + 10_000
	d2 := Apply(InviteOp(1, 200, ruleZone), d.Record, later, nil, RuleConfig{})
	require.Zero(t, d2.Code)
	require.Equal(t, later+InviteTTLMs, findInvite(d2.Record, 200).GetExpireAtMs())
	require.Equal(t, []InviteIndexAdd{{InviteeId: 200, ExpireAtMs: later + InviteTTLMs}}, d2.InvitesAdded)
	require.Empty(t, d2.InvitesRemoved)

	// 过期后重邀同一人:清理与重邀重叠,ID := ID \ IA。
	expired := withInvite(withInvite(newRecord(1), 7, ruleZone, ruleNow-100, ruleNow), 8, ruleZone, ruleNow-100, ruleNow)
	d3 := Apply(InviteOp(1, 7, ruleZone), expired, ruleNow, nil, RuleConfig{})
	require.Zero(t, d3.Code)
	require.Equal(t, []uint64{8}, d3.InvitesRemoved)
	require.Equal(t, uint64(7), d3.InvitesAdded[0].InviteeId)
	require.NotNil(t, findInvite(d3.Record, 7))
	require.Nil(t, findInvite(d3.Record, 8))

	// 同一毫秒挤满:刚加的不能被挤掉(否则 IA 写了索引、记录里却没有)。
	same := newRecord(1)
	for i := 0; i < MaxInvitesPerTeam; i++ {
		same = withInvite(same, uint64(500+i), ruleZone, ruleNow, ruleNow+1)
	}
	d4 := Apply(InviteOp(1, 3, ruleZone), same, ruleNow, nil, RuleConfig{})
	require.NotNil(t, findInvite(d4.Record, 3))
	require.Equal(t, []uint64{500}, d4.InvitesRemoved)

	// 只有邀请过期时 Refresh 也要提交并删反查项。
	d5 := Apply(RefreshOp(1), withInvite(newRecord(1), 9, ruleZone, ruleNow-1, ruleNow), ruleNow, nil, RuleConfig{})
	require.True(t, d5.Changed)
	require.Equal(t, []uint64{9}, d5.InvitesRemoved)
	require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_INVITE_CHANGED, d5.Reason)
}

// #5:§D.6 重试安全表逐行。第二次调用吃第一次的结果记录。
func TestRetrySafety(t *testing.T) {
	cfg := RuleConfig{}
	second := func(t *testing.T, op Op, first Decision, sessions Sessions) Decision {
		t.Helper()
		require.Zero(t, first.Code)
		require.True(t, first.Changed)
		return Apply(op, first.Record, ruleNow, sessions, cfg)
	}

	t.Run("CreateTeam 已在队", func(t *testing.T) {
		d := Apply(withCallerTeam(CreateOp(1, ruleTid+1, ruleZone), ruleTid), nil, ruleNow, nil, cfg)
		require.Equal(t, ErrMemberInTeam, d.Code)
	})
	t.Run("ApplyJoinTeam 刷新并成功", func(t *testing.T) {
		op := ApplyOp(3, ruleZone)
		d := second(t, op, Apply(op, newRecord(1), ruleNow, nil, cfg), nil)
		require.Zero(t, d.Code)
		require.Len(t, d.Record.GetApplications(), 1)
	})
	t.Run("InviteToTeam 刷新并成功", func(t *testing.T) {
		op := InviteOp(1, 3, ruleZone)
		d := second(t, op, Apply(op, newRecord(1), ruleNow, nil, cfg), nil)
		require.Zero(t, d.Code)
		require.Len(t, d.Record.GetInvites(), 1)
		require.Len(t, d.InvitesAdded, 1)
	})
	t.Run("HandleApplication 同意重放", func(t *testing.T) {
		op := HandleApplicationOp(1, 3, true)
		first := Apply(op, withApplication(newRecord(1), 3, ruleZone, ruleNow, ruleNow+1), ruleNow, nil, cfg)
		require.Equal(t, []uint64{3}, first.Joined)
		require.Equal(t, []uint64{1}, first.Kept)
		require.Equal(t, uint32(2), FindMember(first.Record, 3).GetJoinSeq())
		require.Equal(t, uint32(3), first.Record.GetNextJoinSeq())
		require.Empty(t, first.Record.GetApplications(), "加入后删申请")
		d := second(t, op, first, nil)
		require.Zero(t, d.Code)
		require.False(t, d.Changed)
	})
	t.Run("HandleApplication 拒绝重放", func(t *testing.T) {
		op := HandleApplicationOp(1, 3, false)
		first := Apply(op, withApplication(newRecord(1), 3, ruleZone, ruleNow, ruleNow+1), ruleNow, nil, cfg)
		require.Equal(t, uint64(3), first.RejectedApplicant)
		d := second(t, op, first, nil)
		require.Zero(t, d.Code)
		require.False(t, d.Changed)
		require.Zero(t, d.RejectedApplicant, "重放不再通知申请人")
	})
	t.Run("RespondInvite 接受重放", func(t *testing.T) {
		op := RespondInviteOp(3, true)
		first := Apply(op, withInvite(newRecord(1), 3, ruleZone, ruleNow, ruleNow+1), ruleNow, nil, cfg)
		require.Equal(t, []uint64{3}, first.Joined)
		require.Equal(t, []uint64{3}, first.InvitesRemoved, "接受后删邀请反查项")
		require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_MEMBER_JOINED, first.Reason)
		d := second(t, withCallerTeam(op, ruleTid), first, nil)
		require.Zero(t, d.Code)
		require.False(t, d.Changed)
	})
	t.Run("RespondInvite 拒绝重放", func(t *testing.T) {
		op := RespondInviteOp(3, false)
		first := Apply(op, withInvite(newRecord(1), 3, ruleZone, ruleNow, ruleNow+1), ruleNow, nil, cfg)
		require.Equal(t, []uint64{3}, first.InvitesRemoved)
		require.Equal(t, []uint64{1}, first.Kept)
		d := second(t, op, first, nil)
		require.Zero(t, d.Code)
		require.False(t, d.Changed)
	})
	t.Run("LeaveTeam 索引仍指向本队但记录里没有我", func(t *testing.T) {
		d := Apply(withCallerTeam(LeaveOp(9), ruleTid), newRecord(1, 2), ruleNow, nil, cfg)
		require.Zero(t, d.Code)
		require.True(t, d.Changed, "提交 L=[caller] 修复索引")
		require.Equal(t, []uint64{9}, d.Left)
		require.Equal(t, []uint64{1, 2}, d.Kept)
		d = Apply(LeaveOp(9), newRecord(1, 2), ruleNow, nil, cfg)
		require.False(t, d.Changed, "索引不指向本队时什么都不写")
	})
	t.Run("TransferLeader 目标已是队长", func(t *testing.T) {
		op := TransferLeaderOp(1, 2)
		first := Apply(op, newRecord(1, 2), ruleNow, allOnline(1, 2), cfg)
		require.Equal(t, uint64(2), first.Record.GetLeaderId())
		d := second(t, op, first, allOnline(1, 2))
		require.Zero(t, d.Code)
		require.False(t, d.Changed)
	})
	t.Run("KickMember 目标已不在队", func(t *testing.T) {
		op := KickOp(1, 2)
		d := second(t, op, Apply(op, newRecord(1, 2), ruleNow, nil, cfg), nil)
		require.Equal(t, ErrMemberNotInTeam, d.Code)
		require.Equal(t, uint64(2), d.CodeParam)
	})
	t.Run("DisbandTeam 记录已删", func(t *testing.T) {
		first := Apply(DisbandOp(1), newRecord(1, 2), ruleNow, nil, cfg)
		require.True(t, first.Disbanded)
		require.ElementsMatch(t, []uint64{1, 2}, first.Left)
		require.Equal(t, ErrNoTeam, Apply(DisbandOp(1), first.Record, ruleNow, nil, cfg).Code)
	})
}

// 加入者在本队同时有申请与邀请:两条都删,邀请反查项进入 ID。
func TestJoinClearsBothApplicationAndInvite(t *testing.T) {
	rec := withInvite(withApplication(newRecord(1), 3, ruleZone, ruleNow, ruleNow+5), 3, ruleZone, ruleNow, ruleNow+5)
	d := Apply(HandleApplicationOp(1, 3, true), rec, ruleNow, nil, RuleConfig{})
	require.Zero(t, d.Code)
	require.Empty(t, d.Record.GetApplications())
	require.Empty(t, d.Record.GetInvites())
	require.Equal(t, []uint64{3}, d.InvitesRemoved)
}

func TestRepairRemoveMember(t *testing.T) {
	d := RepairRemoveMember(newRecord(1, 2, 3), 1, ruleNow, allOnline(3))
	require.True(t, d.Changed)
	require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_HEALED, d.Reason)
	require.Equal(t, uint64(1), d.Actor)
	require.Equal(t, []uint64{1}, d.Left)
	require.Equal(t, []uint64{2, 3}, d.Kept)
	require.Equal(t, uint64(3), d.Record.GetLeaderId(), "被修复移出的是队长时照离队规则转让")

	d = RepairRemoveMember(newRecord(1), 1, ruleNow, nil)
	require.True(t, d.Disbanded)
	require.Nil(t, d.Record)

	require.False(t, RepairRemoveMember(newRecord(1, 2), 9, ruleNow, nil).Changed)

	// 一次移出多人:Left 相对原记录;不在记录里的 id 跳过;Actor 为第一个实际移出的人。
	d = RepairRemoveMembers(newRecord(1, 2, 3, 4), []uint64{9, 3, 1}, ruleNow, allOnline(2, 4))
	require.True(t, d.Changed)
	require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_HEALED, d.Reason)
	require.Equal(t, uint64(3), d.Actor)
	require.Equal(t, []uint64{1, 3}, d.Left)
	require.Equal(t, []uint64{2, 4}, d.Kept)
	require.Equal(t, uint64(2), d.Record.GetLeaderId())

	d = RepairRemoveMembers(newRecord(1, 2), []uint64{2, 1}, ruleNow, nil)
	require.True(t, d.Disbanded, "全员移出即解散")
	require.Nil(t, d.Record)
	require.Equal(t, []uint64{1, 2}, d.Left)
	require.False(t, RepairRemoveMembers(newRecord(1, 2), []uint64{8, 9}, ruleNow, nil).Changed)
}

func TestPruneExpiredDoesNotMutateInput(t *testing.T) {
	rec := withInvite(withApplication(newRecord(1), 3, ruleZone, ruleNow-5, ruleNow), 4, ruleZone, ruleNow, ruleNow+5)
	before := proto.Clone(rec).(*teampb.TeamRecord)
	out := PruneExpired(rec, ruleNow)
	require.True(t, proto.Equal(before, rec))
	require.Empty(t, out.GetApplications())
	require.Len(t, out.GetInvites(), 1)
	require.Nil(t, PruneExpired(nil, ruleNow))
}

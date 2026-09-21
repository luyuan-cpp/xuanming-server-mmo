package team

import (
	"sort"

	teampb "proto/team"
)

// 视图构建(设计文档 docs/design/team-system.md §B.1 TeamView、§C.4、§H.3)。
//
// 本文件只放纯函数:输入"记录 + 版本 + 接收者 epoch + Redis 时钟 + 展示缓存",输出下发给客户端的视图。
// 不做 I/O。
//
// 同源前提(§C.4,客户端按 (membership_epoch, version) 排序的正确性全靠它):
// 一份视图里的 team_id / version / epoch / 记录必须出自同一次原子操作。
// 所以只提供三个入口,各自对应一种原子来源:
//
//	viewFromSnapshot  S_READ(调用者自己的索引 + 记录)
//	viewFromCommit    S_COMMIT(提交后每人的 tid/epoch)
//	viewFromMembers   S_READ_MEMBERS(记录 + 每名成员的 tid/epoch)
//
// 来源里接收者的 tid 与视图 team_id 对不上时一律返回 ok=false,调用方不推 / 改用自由读,
// 绝不把"某时刻的记录"和"另一时刻的 epoch"拼在一起。

// emptyTeamView 无队伍视图:team_id=0、version=0,epoch 为接收者最新值(§C.4;索引缺失时 S_READ 回报
// Redis nowMs,保证整队过期后的空视图仍大于客户端手里的旧 epoch)。
func emptyTeamView(epoch, nowMs uint64) *teampb.TeamView {
	return &teampb.TeamView{
		Capacity:        uint32(Capacity),
		MembershipEpoch: epoch,
		ServerTimeMs:    nowMs,
	}
}

// teamViewFor 为 viewer 构建 rec 的视图。rec 按 nowMs 过滤掉已过期的申请与邀请;
// 申请与已发出邀请只下发给队长,application_count 所有人可见。
func teamViewFor(viewer uint64, rec *teampb.TeamRecord, version, epoch, nowMs uint64, dc displayCache) *teampb.TeamView {
	if rec == nil || rec.GetTeamId() == 0 {
		return emptyTeamView(epoch, nowMs)
	}
	live := PruneExpired(rec, nowMs)
	leader := live.GetLeaderId()
	view := &teampb.TeamView{
		TeamId:           live.GetTeamId(),
		LeaderId:         leader,
		Capacity:         uint32(Capacity),
		ZoneId:           live.GetZoneId(),
		Version:          version,
		MembershipEpoch:  epoch,
		MatchState:       teampb.TeamMatchState_TEAM_MATCH_STATE_IDLE,
		ApplicationCount: uint32(len(live.GetApplications())),
		ServerTimeMs:     nowMs,
	}
	if MatchLockActive(live, nowMs) {
		view.MatchState = teampb.TeamMatchState_TEAM_MATCH_STATE_STARTING
	}
	for _, pid := range MemberIds(live) { // join_seq 升序;客户端仍按 join_seq 字段排序(§11.6)
		m := FindMember(live, pid)
		view.Members = append(view.Members, memberView(pid, m.GetZoneId(), m.GetJoinSeq(), leader, dc))
	}
	if viewer == 0 || viewer != leader {
		return view
	}
	apps := append([]*teampb.TeamApplicationRecord(nil), live.GetApplications()...)
	sort.SliceStable(apps, func(i, j int) bool {
		if apps[i].GetAppliedAtMs() != apps[j].GetAppliedAtMs() {
			return apps[i].GetAppliedAtMs() < apps[j].GetAppliedAtMs()
		}
		return apps[i].GetPlayerId() < apps[j].GetPlayerId()
	})
	for _, a := range apps {
		view.Applications = append(view.Applications, &teampb.TeamApplicationView{
			Player:      memberView(a.GetPlayerId(), a.GetZoneId(), 0, leader, dc),
			AppliedAtMs: a.GetAppliedAtMs(),
			ExpireAtMs:  a.GetExpireAtMs(),
		})
	}
	invites := append([]*teampb.TeamInviteRecord(nil), live.GetInvites()...)
	sort.SliceStable(invites, func(i, j int) bool {
		if invites[i].GetInvitedAtMs() != invites[j].GetInvitedAtMs() {
			return invites[i].GetInvitedAtMs() < invites[j].GetInvitedAtMs()
		}
		return invites[i].GetInviteeId() < invites[j].GetInviteeId()
	})
	for _, inv := range invites {
		view.PendingInvites = append(view.PendingInvites, &teampb.TeamOutgoingInviteView{
			Invitee:    memberView(inv.GetInviteeId(), inv.GetZoneId(), 0, leader, dc),
			ExpireAtMs: inv.GetExpireAtMs(),
		})
	}
	return view
}

// memberView 一名玩家的展示视图；外观和性别复用存档资料，name 暂未接线。
func memberView(playerId uint64, zone, joinSeq uint32, leader uint64, dc displayCache) *teampb.TeamMemberView {
	d := dc[playerId]
	return &teampb.TeamMemberView{
		PlayerId:     playerId,
		Level:        d.Level,
		ClassId:      d.ClassId,
		AppearanceId: d.AppearanceId,
		Gender:       d.Gender,
		IsLeader:     playerId == leader,
		IsOnline:     d.Online,
		InBattle:     d.InBattle,
		ZoneId:       zone,
		JoinSeq:      joinSeq,
	}
}

// incomingInviteView 被邀请人看到的"收到的邀请";rec 里没有给 invitee 的未过期邀请返回 nil。
func incomingInviteView(rec *teampb.TeamRecord, invitee, nowMs uint64, dc displayCache) *teampb.TeamIncomingInviteView {
	if rec == nil {
		return nil
	}
	live := PruneExpired(rec, nowMs)
	inv := findInvite(live, invitee)
	if inv == nil {
		return nil
	}
	leader := live.GetLeaderId()
	inviter := FindMember(live, inv.GetInviterId()) // 邀请人可能已离队:zone / join_seq 取不到时为 0
	return &teampb.TeamIncomingInviteView{
		TeamId:      live.GetTeamId(),
		Inviter:     memberView(inv.GetInviterId(), inviter.GetZoneId(), inviter.GetJoinSeq(), leader, dc),
		LeaderId:    leader,
		MemberCount: uint32(len(live.GetMembers())),
		ZoneId:      live.GetZoneId(),
		ExpireAtMs:  inv.GetExpireAtMs(),
	}
}

// rosterIds 视图里会出现的全部玩家(成员、申请人、被邀请人、邀请人),供一次性加载展示缓存。
func rosterIds(rec *teampb.TeamRecord) []uint64 {
	if rec == nil {
		return nil
	}
	ids := MemberIds(rec)
	for _, a := range rec.GetApplications() {
		ids = append(ids, a.GetPlayerId())
	}
	for _, inv := range rec.GetInvites() {
		ids = append(ids, inv.GetInviteeId(), inv.GetInviterId())
	}
	return uniqueIds(ids)
}

// snapshotViewable S_READ 快照能否为 snap.PlayerId 构建同源视图:
// 调用者无队(索引 tid=0 或缺失),或调用者索引正指向这次读到的、存在的记录。
func snapshotViewable(snap *Snapshot) bool {
	if snap == nil {
		return false
	}
	return snap.PlayerTeamId == 0 || (snap.PlayerTeamId == snap.TeamId && snap.Record != nil)
}

// viewFromSnapshot 由一次 S_READ 构建调用者视图;不可同源构建时 ok=false。
func viewFromSnapshot(snap *Snapshot, dc displayCache) (*teampb.TeamView, bool) {
	switch {
	case !snapshotViewable(snap):
		return nil, false
	case snap.PlayerTeamId == 0:
		return emptyTeamView(snap.PlayerEpoch, snap.NowMs), true
	}
	return teamViewFor(snap.PlayerId, snap.Record, snap.Version, snap.PlayerEpoch, snap.NowMs, dc), true
}

// commitViewable 提交结果能否为 pid 构建同源视图(pid 在 J/K/L 里,且提交后 tid 为 0 或本队)。
func commitViewable(c *CommitResult, pid uint64) bool {
	if c == nil {
		return false
	}
	idx, ok := c.Indexes[pid]
	if !ok {
		return false
	}
	return idx.TeamId == 0 || (idx.TeamId == c.TeamId && c.Decision.Record != nil)
}

// viewFromCommit 由 S_COMMIT 返回的 (tid, epoch) 为 pid 构建视图;不可同源构建时 ok=false
// (例如修复提交里被移出、索引已指向别队的成员)。
func viewFromCommit(c *CommitResult, pid uint64, dc displayCache) (*teampb.TeamView, bool) {
	if !commitViewable(c, pid) {
		return nil, false
	}
	idx := c.Indexes[pid]
	if idx.TeamId == 0 {
		return emptyTeamView(idx.Epoch, c.NowMs), true
	}
	return teamViewFor(pid, c.Decision.Record, c.Version, idx.Epoch, c.NowMs, dc), true
}

// viewFromMembers 由 S_READ_MEMBERS 为成员 pid 构建视图;只给索引 tid == 本队的成员(§C.5)。
func viewFromMembers(m *MembersSnapshot, pid uint64, dc displayCache) (*teampb.TeamView, bool) {
	if m == nil || m.Record == nil {
		return nil, false
	}
	idx, ok := m.Indexes[pid]
	if !ok || idx.TeamId != m.TeamId {
		return nil, false
	}
	return teamViewFor(pid, m.Record, m.Version, idx.Epoch, m.NowMs, dc), true
}

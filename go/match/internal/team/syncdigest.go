package team

import (
	"context"
	"time"

	scenepb "proto/scene"
)

// 落地摘要 bump(设计文档 docs/design/landing-recovery-sync-digest.md §3.3 的 TEAM / TEAM_INVITE 两行、§3.5)。
//
// 挂在 publish 里:那是「名册已落盘」之后唯一的汇合点 —— RPC 写、开战锁、EndMatch、{-2} 修复提交与
// 孤儿自愈都从这里派发副作用。提交与 bump 不原子(bump 不进 S_COMMIT Lua,设计选择提交后 bump):
// 进程恰在两步之间退出时漏一次 bump,客户端要等下次变化或打开面板才恢复,设计接受这一点。
//
// 与推送的区别:
//   - 推送排除 RPC 调用者(他的回包就是视图);bump 必须包含调用者 —— 回包可能在断线时丢失(§3.5 第 3 条);
//   - 推送只推在线的人;bump 不看在线 —— 它正是给离线 / 传送中的人留的;
//   - 纯按时过期的清理(申请 120 秒、邀请 60 秒)不 bump:客户端按 expire_at_ms 自己算(§3.3)。

// syncDigestBudget 一批 bump 的总预算(§3.5 第 6 条)。从 publish 的批预算里切出来、先于推送执行。
const syncDigestBudget = 2 * time.Second

// syncDigestTargets 汇总一批已落盘提交需要 bump 的玩家(纯函数,不做 I/O)。
//
//   - TEAM:每条不是「纯过期清理」(Decision.ExpiryOnly)的提交的 J ∪ K ∪ L —— TeamView 带 version,
//     任何一次提交都会让这些人拉到的视图变化;再加 RPC 调用者本人(caller=0 表示后台提交,不加);
//     另加孤儿自愈把索引置 0 的玩家(healed),他的 GetMyTeam 从「有队」变成「无队」。
//   - TEAM_INVITE:新发 / 刷新邀请的被邀请人(InvitesAdded),以及不是按时过期而被移除的邀请的被邀请人
//     (InvitesRemoved \ InvitesExpired:接受、拒绝、被同队新邀请挤掉、解散撤销)—— 他们的 ListMyInvites 变了。
//
// 两个结果各自去重、去掉 0,保持首次出现的顺序。
func syncDigestTargets(caller uint64, commits []*CommitResult, healed []uint64) (team, invite []uint64) {
	callerAffected := false
	for _, c := range commits {
		if c == nil {
			continue
		}
		d := c.Decision
		if !d.ExpiryOnly {
			callerAffected = true
			team = append(team, d.Joined...)
			team = append(team, d.Kept...)
			team = append(team, d.Left...)
		}
		for _, add := range d.InvitesAdded {
			invite = append(invite, add.InviteeId)
		}
		expired := make(map[uint64]bool, len(d.InvitesExpired))
		for _, id := range d.InvitesExpired {
			expired[id] = true
		}
		for _, id := range d.InvitesRemoved {
			if !expired[id] {
				invite = append(invite, id)
			}
		}
	}
	if callerAffected {
		team = append(team, caller)
	}
	team = append(team, healed...)
	return uniqueIds(team), uniqueIds(invite)
}

// bumpSyncDigest 按 syncDigestTargets 给受影响玩家 bump 摘要。在 publish 的异步上下文里调用,
// 自带 syncDigestBudget 总预算;失败由 playersync 记 WARN 与 player_sync_bump_total{service="team",result="error"},
// 不影响任何 RPC 结果,也不影响随后的推送。
func (s *Service) bumpSyncDigest(ctx context.Context, caller uint64, commits []*CommitResult, healed []uint64) {
	if s.syncDigest == nil {
		return
	}
	teamIds, inviteIds := syncDigestTargets(caller, commits, healed)
	if len(teamIds) == 0 && len(inviteIds) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, syncDigestBudget)
	defer cancel()
	s.svcCtx.HoldRelay(s.syncDigest.BumpAll(ctx, teamIds, scenepb.SyncDomain_SYNC_DOMAIN_TEAM))
	s.svcCtx.HoldRelay(s.syncDigest.BumpAll(ctx, inviteIds, scenepb.SyncDomain_SYNC_DOMAIN_TEAM_INVITE))
}

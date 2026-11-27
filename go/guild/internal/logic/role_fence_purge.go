package logic

import (
	"context"
	"database/sql"

	"guild/internal/data"
	pb "proto/guild"
	scenepb "proto/scene"
	"shared/rolefence"
)

// RoleFencePurge 是 guild 的删角收尾(rolefence.PurgeFunc,docs/design/account-deletion.md §6.2、§6.3),由角色护栏参与者在
// PurgeRemovedRole 的事务里调用:调用方已持这名玩家的副本行锁、核过是墓碑。数据库部分全在 data.PurgeRemovedRole(离帮;
// 帮主让位给职位最高的成员,帮里只剩他就解散;删个人数据)。他在帮里时登记一个提交之后的动作:失效缓存、解散的摘榜
// (GuildRepo.FinishRolePurge),再给留下的人推送与 bump 摘要(afterRolePurge)。登记失败原样返回:清理整笔回滚,协调者下一轮再调。
func (l *GuildLogic) RoleFencePurge(ctx context.Context, tx *sql.Tx, player uint64) error {
	res, err := data.PurgeRemovedRole(ctx, tx, player, nowMs())
	if err != nil {
		return err
	}
	if res.GuildID == 0 {
		return nil
	}
	return rolefence.OnCommit(ctx, func(ctx context.Context) {
		l.repo.FinishRolePurge(ctx, res)
		// 解散了就与 DisbandGuild 一样释放它占领的灵脉,否则已解散的帮会一直占着旗。
		if res.Disbanded && l.war != nil && l.war.Repo != nil {
			_ = l.war.Repo.CleanupGuildWarOnDisband(ctx, res.GuildID, uint64(l.warNow().UnixMilli()))
		}
		l.afterRolePurge(ctx, res)
	})
}

// afterRolePurge 是删角收尾提交之后给留在帮里的人的推送与摘要 bump。被删的人不在收件人里:他已是墓碑,不会再登录。
// 让位推 LEADER_TRANSFERRED(actor 是被删的帮主,target 是接任者,与转让同一种推送);普通成员推 MEMBER_LEFT(与退帮同一种);
// 解散时帮里只剩他,Members 为空,什么都不发。
func (l *GuildLogic) afterRolePurge(ctx context.Context, res data.RolePurgeResult) {
	if len(res.Members) == 0 {
		return
	}
	kind, target := pb.GuildChangeKind_GUILD_CHANGE_KIND_MEMBER_LEFT, res.Player
	if res.Successor != 0 {
		kind, target = pb.GuildChangeKind_GUILD_CHANGE_KIND_LEADER_TRANSFERRED, res.Successor
	}
	l.notify(ctx, kind, res.GuildID, res.Player, target, res.Members)
	l.bumpSync(res.Members, scenepb.SyncDomain_SYNC_DOMAIN_GUILD)
}

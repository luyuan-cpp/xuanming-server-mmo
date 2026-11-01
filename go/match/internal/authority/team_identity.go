package authority

import (
	"context"
	"database/sql"
	"sort"

	pb "proto/match"
	teampb "proto/team"
	"shared/accountdirectory"
	"shared/rolefence"
)

// LockTeamOriginals 复用匹配同一角色P围栏，调用方不得先持Team/launch/票锁。
func (s *Store) LockTeamOriginals(ctx context.Context, tx *sql.Tx, originals []*teampb.TeamOriginalRole) error {
	roles := make([]*pb.MatchAuthorityTicketRecord, 0, len(originals))
	for _, role := range originals {
		roles = append(roles, &pb.MatchAuthorityTicketRecord{PlayerId: role.GetPlayerId(), OwnerAccount: role.GetOwnerAccount(), OwnerRevision: role.GetOwnerRevision()})
	}
	return s.lockOriginalPlayers(ctx, tx, roles)
}

// 私有只读列表仍须核原actor，并持P覆盖一致快照；它不修改任何带zone业务原件。
func (s *Store) LockTeamReadActor(ctx context.Context, tx *sql.Tx, player uint64) error {
	return s.lockActor(ctx, tx, player)
}

// CaptureTeamOriginals 只在新操作捕获新邀请对端。现存成员、申请/邀请与原actor
// 必须保持已捕获账号/修订；不允许以当前目录替换旧原件。全体P按uint64顺序一次获取。
// 新对端的副本缺行时回 StaleError,由 Fenced 在事务外自愈后重来。
func (s *Store) CaptureTeamOriginals(ctx context.Context, tx *sql.Tx, actor uint64, originals []*teampb.TeamOriginalRole, newTarget uint64) ([]*teampb.TeamOriginalRole, error) {
	if s == nil || s.fence == nil || actor == 0 {
		return nil, ErrOriginalPlayer
	}
	caller := originalPlayer(ctx, actor)
	if !accountdirectory.ValidAccount(caller.OwnerAccount) || caller.OwnerRevision == 0 {
		return nil, ErrOriginalPlayer
	}
	byPlayer := make(map[uint64]*teampb.TeamOriginalRole, len(originals)+2)
	for _, role := range append(append([]*teampb.TeamOriginalRole(nil), originals...), &teampb.TeamOriginalRole{PlayerId: actor, OwnerAccount: caller.OwnerAccount, OwnerRevision: caller.OwnerRevision}) {
		if role.GetPlayerId() == 0 || !accountdirectory.ValidAccount(role.GetOwnerAccount()) || role.GetOwnerRevision() == 0 {
			return nil, ErrOriginalPlayer
		}
		if previous := byPlayer[role.PlayerId]; previous != nil && (previous.OwnerAccount != role.OwnerAccount || previous.OwnerRevision != role.OwnerRevision) {
			return nil, ErrOriginalPlayer
		}
		byPlayer[role.PlayerId] = role
	}
	expects := make([]rolefence.Expect, 0, len(byPlayer)+1)
	for player, role := range byPlayer {
		expects = append(expects, rolefence.Expect{PlayerID: player, Account: role.OwnerAccount, OwnerRevision: role.OwnerRevision, Reject: ErrOriginalPlayer})
	}
	if newTarget != 0 && byPlayer[newTarget] == nil {
		expects = append(expects, rolefence.Expect{PlayerID: newTarget, Reject: ErrOriginalPlayer})
	}
	fences, err := rolefence.LockAll(ctx, tx, expects...)
	if err != nil {
		return nil, err
	}
	ids := make([]uint64, 0, len(fences))
	for player := range fences {
		ids = append(ids, player)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]*teampb.TeamOriginalRole, 0, len(ids))
	for _, player := range ids {
		original := byPlayer[player]
		if original == nil {
			original = &teampb.TeamOriginalRole{PlayerId: player, OwnerAccount: fences[player].Account, OwnerRevision: fences[player].OwnerRevision}
		}
		out = append(out, original)
	}
	return out, nil
}

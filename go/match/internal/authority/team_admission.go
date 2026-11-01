package authority

import (
	"context"
	"database/sql"
	"errors"

	base "proto/common/base"
	teampb "proto/team"
	"shared/accountdirectory"
	"shared/rolefence"

	"google.golang.org/protobuf/proto"
)

// ReadTeamAdmission 只接受scene已经在全局SQL登记的完整入场原件。
// 入口还须校验scene服务HMAC；本层绝不拼造gate会话或用当前owner补原actor。
//
// 这个校验防的是过期的入场原件读到这名角色的队伍:旧主的会话、已关闭或被新世代顶掉的世代。两道:
//   - 归属:本库护栏副本,和读队伍同一个事务。修订号对不上或冻结都拒绝。
//   - 登记:account_role_admission 只有 data_service 连,在事务外问目录(VerifyRoleAdmission)。
//
// 登记放在读之后问,窗口才和原来的同事务校验一样小:世代和关闭水位都只增不减,而 scene 登记成功之后才会来读,
// 所以读之后还有效,就说明读的那一刻也有效。原来的行锁在提交时也放了,scene 收到回复之前登记照样能关,
// 那一段靠 scene 回调里再比 RoleAdmissionSystem::ConfirmedIdentity 兜住。这条路径只读,不写任何东西,
// 不需要「副本里记最大已关闭世代」这类同事务的替代品。规格 §3.7。
func (s *Store) ReadTeamAdmission(ctx context.Context, admission *base.RoleAdmissionIdentity) (member *teampb.TeamMembershipRecord, record *teampb.TeamAuthorityRecord, err error) {
	if s == nil || s.fence == nil || s.admissions == nil || !accountdirectory.ValidAdmission(admission) {
		return nil, nil, ErrOriginalPlayer
	}
	err = s.Fenced(ctx, func() (e error) { member, record, e = s.readTeamMembership(ctx, admission); return e })
	if err != nil {
		return nil, nil, err
	}
	valid, err := s.admissions.VerifyRoleAdmission(ctx, admission)
	if err != nil {
		return nil, nil, err
	}
	if !valid {
		return nil, nil, ErrOriginalPlayer
	}
	return member, record, nil
}

func (s *Store) readTeamMembership(ctx context.Context, admission *base.RoleAdmissionIdentity) (*teampb.TeamMembershipRecord, *teampb.TeamAuthorityRecord, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	fence, err := rolefence.Lock(ctx, tx, admission.PlayerId)
	// 原件只有修订号没有账号,账号取副本本身,下面再和队伍原件比。
	if err = rolefence.Check(fence, err, rolefence.Expect{PlayerID: admission.PlayerId, Account: fence.Account, OwnerRevision: admission.OwnerRevision, Reject: ErrOriginalPlayer}); err != nil {
		return nil, nil, err
	}
	member := &teampb.TeamMembershipRecord{PlayerId: admission.PlayerId}
	err = tx.QueryRowContext(ctx, "SELECT team_id,epoch FROM team_membership WHERE player_id=?", admission.PlayerId).Scan(&member.TeamId, &member.Epoch)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return nil, nil, err
	}
	var record *teampb.TeamAuthorityRecord
	if member.TeamId != 0 {
		record = &teampb.TeamAuthorityRecord{TeamId: member.TeamId}
		if err = tx.QueryRowContext(ctx, "SELECT version,payload,state,idle_expires_ms FROM team_authority WHERE team_id=?", member.TeamId).Scan(&record.Version, &record.Payload, &record.State, &record.IdleExpiresMs); err != nil {
			return nil, nil, err
		}
		data := &teampb.TeamAuthorityData{}
		if record.State != 1 || record.Version == 0 || proto.Unmarshal(record.Payload, data) != nil || data.GetRecord().GetTeamId() != member.TeamId {
			return nil, nil, errors.New("team: SQL队伍原件损坏")
		}
		found := false
		for _, original := range data.Members {
			if original.PlayerId == member.PlayerId && original.OwnerRevision == admission.OwnerRevision && original.OwnerAccount == fence.Account {
				found = true
			}
		}
		memberFound := false
		for _, original := range data.GetRecord().GetMembers() {
			if original.PlayerId == member.PlayerId {
				memberFound = true
			}
		}
		if !found || !memberFound {
			return nil, nil, errors.New("team: SQL名册与原成员索引不一致")
		}
	}
	return member, record, tx.Commit()
}

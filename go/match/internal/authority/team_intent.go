package authority

import (
	"context"
	"errors"

	teampb "proto/team"
)

// TeamMatchIntentForeign 是 TeamMatchIntent.state 的确定结论之一:这个请求号的原件记的是角色此前的归属(过户前留下的)。
// 现主人不能恢复它,StartTeamMatch 也拒绝沿它提交;客户端据此释放本机原意图,服务端不把它当故障。
const TeamMatchIntentForeign uint32 = 6

// TeamMatchIntent 只查询原发起者、原 UUID 的持久回执，不根据当前队伍或票据猜原请求结局。
// 已入队但尚无成局、以及已撤销的整队原件都可能没有 battle_id；未找到仍返回 nil，不能当作已撤销。
// 会话本身已不归角色现主人时仍回 ErrOriginalPlayer;只有原件属于此前的归属才给 TeamMatchIntentForeign。
func (s *Store) TeamMatchIntent(ctx context.Context, actor uint64, request string) (*teampb.TeamMatchIntent, error) {
	intent, err := s.teamMatchIntent(ctx, actor, request)
	if errors.Is(err, ErrForeignOriginal) {
		return &teampb.TeamMatchIntent{RequestId: request, State: TeamMatchIntentForeign}, nil
	}
	return intent, err
}

func (s *Store) teamMatchIntent(ctx context.Context, actor uint64, request string) (*teampb.TeamMatchIntent, error) {
	key, err := DirectUUIDKey(1, actor, request)
	if err != nil {
		return nil, err
	}
	launch, err := s.TeamLaunch(ctx, actor, request)
	if err != nil {
		return nil, err
	}
	if launch != nil {
		if launch.BattleId == 0 {
			if launch.State != 1 && launch.State != 2 && launch.State != 5 {
				return nil, ErrDirectConflict
			}
			return &teampb.TeamMatchIntent{RequestId: request, State: launch.State}, nil
		}
		formation, err := s.Formation(ctx, launch.BattleId)
		if err != nil {
			return nil, err
		}
		// launch 的 state5 还表示已正常完成，真实对局的结局只能从原 formation 读取。
		return &teampb.TeamMatchIntent{RequestId: request, BattleId: formation.BattleId, State: formation.State}, nil
	}
	// 保留早期直接成局原件的恢复；这些查询同样核对调用者的原账号与角色修订。
	formation, _, err := s.FindDirect(ctx, key)
	if errors.Is(err, ErrDirectCancelled) {
		return &teampb.TeamMatchIntent{RequestId: request, State: 5}, nil
	}
	if err != nil {
		return nil, err
	}
	if formation != nil {
		return &teampb.TeamMatchIntent{RequestId: request, BattleId: formation.BattleId, State: formation.State}, nil
	}
	// CancelQueue 可在 StartTeamMatch 首次提交前立原 UUID 墓碑。只有确证这份墓碑才允许客户端释放原意图。
	cancelled, err := s.RequestCancelled(ctx, actor, request)
	if err != nil {
		return nil, err
	}
	if cancelled {
		return &teampb.TeamMatchIntent{RequestId: request, State: 5}, nil
	}
	return nil, nil
}

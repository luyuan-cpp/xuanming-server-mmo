package data

import (
	"context"

	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	matchpb "proto/match"
)

// 仅供既有受信结果消费者/结果记录恢复器：在任何Poison前核完整终局与固定原launch。
// 这不替代Kafka/结果存储的部署认证；字段残缺不是“确定性失败后可以解除角色占用”。
func (r *ActivityRepo) ValidateTrialTerminal(ctx context.Context, ev *kafkapb.BattleResultEvent) error {
	if !r.TrialLaunchEnabled() {
		return ErrWriteConflict
	}
	a := ev.GetActivityContext()
	if ev.GetBattleId() == 0 || ev.GetFinishedAtMs() == 0 || ev.GetMatchMode() != uint32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM) || ev.GetOutcome() < battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN || ev.GetOutcome() > battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW || a.GetKind() != battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL || a.GetLaunchRequestId() == 0 {
		return ErrWriteConflict
	}
	row, o, err := r.TrialLaunch(ctx, a.LaunchRequestId)
	if err != nil {
		return err
	}
	if row.BattleId != 0 && row.BattleId != ev.BattleId || ev.BattleConfigId != o.DungeonId || a.GuildId != o.GuildId || a.ActivityId != o.ActivityId || a.PeriodKey != o.PeriodKey || a.GuildPeriodKey != o.GuildPeriodKey || a.InitiatorPlayerId != o.InitiatorPlayerId {
		return ErrWriteConflict
	}
	roster := map[uint64]bool{}
	for _, p := range o.Roster {
		roster[p.PlayerId] = true
	}
	seen := map[uint64]bool{}
	teams := 0
	for _, team := range ev.Teams {
		if team.GetTeamIndex() != 0 {
			continue
		}
		teams++
		for _, p := range team.PlayerIds {
			if !roster[p] || seen[p] {
				return ErrWriteConflict
			}
			seen[p] = true
		}
	}
	if teams != 1 || len(seen) != len(roster) {
		return ErrWriteConflict
	}
	for _, ids := range [][]uint64{ev.FledPlayerIds, ev.DeadPlayerIds} {
		seen = map[uint64]bool{}
		for _, p := range ids {
			if !roster[p] || seen[p] {
				return ErrWriteConflict
			}
			seen[p] = true
		}
	}
	return nil
}

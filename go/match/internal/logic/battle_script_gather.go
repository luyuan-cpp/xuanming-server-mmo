package logic

import (
	"context"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	"match/internal/authority"
	"match/internal/battlescript"
	"match/internal/metrics"
	"match/internal/svc"
	battlepb "proto/battle"
	matchpb "proto/match"
)

// battleScriptResolveTimeout 是建房前解析剧本的总预算:每个玩家方至多读两次本库(选用、审核通过版)。
// 与能力查询、存原件、核对工作租约同属建房前的几次调用,由 matchedTicketTTLFor 的 10s 余量吸收(queue.go gatherCreateStageWorst)。
// 超时只让这一方退回官方绑定或不带剧本,不影响开局。
const battleScriptResolveTimeout = 500 * time.Millisecond

// storePlayerScripts 把 match 库接成 battlescript.PlayerScripts
type storePlayerScripts struct {
	store *authority.Store
}

func (s storePlayerScripts) Loadout(ctx context.Context, player uint64) ([]battlescript.Choice, error) {
	rows, err := s.store.BattleScriptLoadout(ctx, player)
	if err != nil {
		return nil, err
	}
	choices := make([]battlescript.Choice, 0, len(rows))
	for _, row := range rows {
		choices = append(choices, choiceOfRecord(row))
	}
	return choices, nil
}

func (s storePlayerScripts) Approved(ctx context.Context, scriptID, owner uint64) (uint32, *battlepb.BattleScript, error) {
	revision, content, err := s.store.ApprovedBattleScript(ctx, scriptID, owner)
	if err != nil {
		return 0, nil, err
	}
	script := &battlepb.BattleScript{}
	if err = proto.Unmarshal(content, script); err != nil {
		return 0, nil, err
	}
	return revision, script, nil
}

func choiceOfRecord(row *matchpb.MatchBattleScriptLoadoutRecord) battlescript.Choice {
	return battlescript.Choice{
		MatchMode:        row.GetMatchMode(),
		BattleConfigID:   row.GetBattleConfigId(),
		Origin:           battlepb.EBattleScriptOrigin(row.GetOrigin()),
		OfficialScriptID: row.GetOfficialScriptId(),
		PlayerScriptID:   row.GetPlayerScriptId(),
	}
}

// resolveTeamScripts 解析这一局每一方的剧本(docs/design/battle-script.md §5.2),按值冻结进建房原件 team_scripts。
// teams 与 f.Members 同下标(gatherTeamAssignment 的输出);PVE 的怪物方是 1 队。
// 原件随后整份落库,恢复重放用的就是这一份:剧本只在成局第一次 gather 时解析一次。
func resolveTeamScripts(svcCtx *svc.ServiceContext, f *matchpb.MatchFormationRecord, teams []uint32) []*battlepb.BattleTeamScript {
	if svcCtx == nil || svcCtx.BattleScripts == nil {
		return nil
	}
	// 评分局只开官方剧本(battle-script.md §5.2):玩家剧本用于 PVE 与非评分局
	resolver := &battlescript.Resolver{Catalog: svcCtx.BattleScripts, Tables: battlescript.TableLookup{},
		OfficialOnly: isRatedMode(matchpb.MatchMode(f.Mode))}
	if svcCtx.Authority != nil {
		resolver.Players = storePlayerScripts{store: svcCtx.Authority}
	}
	ctx, cancel := context.WithTimeout(context.Background(), battleScriptResolveTimeout)
	defer cancel()

	sides := [2]battlescript.Side{{TeamIndex: 0}, {TeamIndex: 1}}
	for i, member := range f.Members {
		if i >= len(teams) || teams[i] > 1 {
			continue
		}
		side := &sides[teams[i]]
		side.Members = append(side.Members, battlescript.Member{PlayerID: member.GetPlayerId(), Captain: member.GetCaptain()})
	}
	pve := battlescript.IsPveMatchMode(f.Mode)
	var out []*battlepb.BattleTeamScript
	for _, side := range sides {
		label := "player"
		if len(side.Members) == 0 {
			if !pve || side.TeamIndex != 1 {
				continue
			}
			side.Monster = true
			label = "monster"
		}
		result := resolver.Resolve(ctx, f.Mode, f.ConfigId, side)
		metrics.ObserveBattleScriptResolve(label, result.Outcome)
		if result.Outcome == battlescript.OutcomeFellBack {
			logx.Errorf("[gather] Battle script choice not used, fell back battle=%d team=%d: %s", f.BattleId, side.TeamIndex, result.Reason)
		}
		if result.Script != nil {
			out = append(out, result.Script)
		}
	}
	return out
}

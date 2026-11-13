package battlescript

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"
	"proto/battle"
)

// Choice 是一名玩家对某种对局的一条选用(选用表的一行)
type Choice struct {
	MatchMode        uint32
	BattleConfigID   uint32
	Origin           battle.EBattleScriptOrigin
	OfficialScriptID uint32
	PlayerScriptID   uint64
}

// PickChoice 按 LookupKeys(具体 → 通用)取第一条匹配的选用;没有返回 nil
func PickChoice(choices []Choice, matchMode, battleConfigID uint32) *Choice {
	for _, key := range LookupKeys(matchMode, battleConfigID) {
		for i := range choices {
			if choices[i].MatchMode == key.MatchMode && choices[i].BattleConfigID == key.BattleConfigID {
				return &choices[i]
			}
		}
	}
	return nil
}

// Member 是成局原件里一方的一名成员(按阵位顺序)
type Member struct {
	PlayerID uint64
	Captain  bool
}

// Chooser 定一方按谁的选用:有队长取第一位队长,没有(单人、散人匹配)取阵位第一的成员;没有成员返回 0
func Chooser(members []Member) uint64 {
	for _, member := range members {
		if member.Captain {
			return member.PlayerID
		}
	}
	if len(members) > 0 {
		return members[0].PlayerID
	}
	return 0
}

// PlayerScripts 是解析时要读的玩家数据(match 库;生产实现在 logic 包,单测喂内存实现)
type PlayerScripts interface {
	// Loadout 读一名玩家的全部选用
	Loadout(ctx context.Context, player uint64) ([]Choice, error)
	// Approved 读一份玩家剧本的审核通过版:返回通过的版本号与正文;只认 owner 本人的剧本
	Approved(ctx context.Context, scriptID, owner uint64) (uint32, *battle.BattleScript, error)
}

// Side 是建房时的一方:Monster 为真表示 PVE 的怪物方,否则 Members 是这一方的玩家(按阵位顺序)
type Side struct {
	TeamIndex uint32
	Monster   bool
	Members   []Member
}

// 解析结局(指标 battle_script_resolve_total 的 outcome)
const (
	OutcomeChosenOfficial = "chosen_official" // 按选用者的选用:官方剧本
	OutcomeChosenPlayer   = "chosen_player"   // 按选用者的选用:本人审核通过的剧本
	OutcomeBound          = "bound"           // 没有选用,用官方绑定
	OutcomeNone           = "none"            // 没有选用也没有绑定,不带剧本
	OutcomeFellBack       = "fell_back"       // 选用没用上(读库失败、剧本失效、配表变了),退回官方绑定或不带剧本
)

// Resolution 是一方的解析结果
type Resolution struct {
	Script  *battle.BattleTeamScript // nil = 这一方不带剧本,出手照旧
	Outcome string
	Reason  string // Outcome = fell_back 时选用没用上的原因(英文,记日志)
}

// Resolver 解析每一方这一局用哪份剧本。规则(battle-script.md §5.2):
//   - 怪物方:官方绑定(MONSTER);
//   - 玩家方:选用者(队长,没有队长取阵位第一)对这一局的选用(具体 → 通用) > 官方绑定(PLAYER) > 不带。
//
// 选用失效(剧本被删、从未通过、配表改了以后过不了校验)或读库失败时退回官方绑定:剧本只影响挂机 / 超时 / 怪物的出招,
// 不碰资产,不值得为它让整局开不起来(AGENTS §11.3 的 fail-open,配 ERROR 日志与 fell_back 计数)。
// 输出的剧本都是独立副本,建房原件序列化后与目录、库里的那份互不影响。
type Resolver struct {
	Catalog *Catalog
	Tables  Tables
	Players PlayerScripts // nil = 不读玩家选用(只用官方绑定)
	// OfficialOnly 为真时只认官方剧本:选用里指向玩家剧本的条目当作不存在,继续按 具体 → 通用 找官方的选用或绑定。
	// 评分 PVP 用它(负责人 2026-11-11 定:评分局先只开官方剧本,玩家剧本用于 PVE 与非评分局)
	OfficialOnly bool
}

// Resolve 解析一方
func (r *Resolver) Resolve(ctx context.Context, matchMode, battleConfigID uint32, side Side) Resolution {
	if r == nil || r.Catalog == nil {
		return Resolution{Outcome: OutcomeNone}
	}
	if side.Monster {
		if script := r.Catalog.Bound(matchMode, battleConfigID, battle.EBattleScriptSide_BATTLE_SCRIPT_SIDE_MONSTER); script != nil {
			return Resolution{Script: official(side.TeamIndex, script, 0), Outcome: OutcomeBound}
		}
		return Resolution{Outcome: OutcomeNone}
	}

	chooser := Chooser(side.Members)
	reason := ""
	if chooser != 0 && r.Players != nil {
		chosen, outcome, err := r.chosen(ctx, chooser, matchMode, battleConfigID)
		if err != nil {
			reason = err.Error()
		} else if chosen != nil {
			chosen.TeamIndex = side.TeamIndex
			chosen.ChooserPlayerId = chooser
			return Resolution{Script: chosen, Outcome: outcome}
		}
	}
	bound := r.Catalog.Bound(matchMode, battleConfigID, battle.EBattleScriptSide_BATTLE_SCRIPT_SIDE_PLAYER)
	switch {
	case reason != "" && bound != nil:
		return Resolution{Script: official(side.TeamIndex, bound, 0), Outcome: OutcomeFellBack, Reason: reason}
	case reason != "":
		return Resolution{Outcome: OutcomeFellBack, Reason: reason}
	case bound != nil:
		return Resolution{Script: official(side.TeamIndex, bound, 0), Outcome: OutcomeBound}
	default:
		return Resolution{Outcome: OutcomeNone}
	}
}

// chosen 按选用者对这一局的选用取剧本;没有选用返回 (nil, "", nil)。
// 选用写入时校验过一遍,这里按当前配表再校验:配表或官方配置改了以后失效的选用不会带进对局
func (r *Resolver) chosen(ctx context.Context, chooser uint64, matchMode, battleConfigID uint32) (*battle.BattleTeamScript, string, error) {
	choices, err := r.Players.Loadout(ctx, chooser)
	if err != nil {
		return nil, "", fmt.Errorf("read loadout of player %d: %w", chooser, err)
	}
	if r.OfficialOnly {
		choices = officialChoices(choices)
	}
	pick := PickChoice(choices, matchMode, battleConfigID)
	if pick == nil {
		return nil, "", nil
	}
	player := battle.EBattleScriptSide_BATTLE_SCRIPT_SIDE_PLAYER
	switch pick.Origin {
	case battle.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_OFFICIAL:
		script, ok := r.Catalog.Official(pick.OfficialScriptID)
		if !ok {
			return nil, "", fmt.Errorf("official script %d chosen by player %d no longer exists", pick.OfficialScriptID, chooser)
		}
		if err = CheckSide(script, player, r.Tables); err != nil {
			return nil, "", fmt.Errorf("official script chosen by player %d: %w", chooser, err)
		}
		return official(0, script, chooser), OutcomeChosenOfficial, nil
	case battle.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_PLAYER:
		revision, content, err := r.Players.Approved(ctx, pick.PlayerScriptID, chooser)
		if err != nil {
			return nil, "", fmt.Errorf("player script %d of player %d: %w", pick.PlayerScriptID, chooser, err)
		}
		normalized, err := Normalize(content, r.Tables)
		if err == nil {
			err = CheckSide(normalized, player, r.Tables)
		}
		if err != nil {
			return nil, "", fmt.Errorf("player script %d revision %d no longer validates: %w", pick.PlayerScriptID, revision, err)
		}
		normalized.ScriptId = 0 // 玩家剧本的身份是 player_script_id
		return &battle.BattleTeamScript{
			Script:               normalized,
			Origin:               battle.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_PLAYER,
			PlayerScriptId:       pick.PlayerScriptID,
			PlayerScriptRevision: revision,
		}, OutcomeChosenPlayer, nil
	default:
		return nil, "", fmt.Errorf("loadout of player %d has unknown origin %d", chooser, pick.Origin)
	}
}

// officialChoices 只留指向官方剧本的选用(不改入参)
func officialChoices(choices []Choice) []Choice {
	kept := make([]Choice, 0, len(choices))
	for _, choice := range choices {
		if choice.Origin == battle.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_OFFICIAL {
			kept = append(kept, choice)
		}
	}
	return kept
}

func official(teamIndex uint32, script *battle.BattleScript, chooser uint64) *battle.BattleTeamScript {
	return &battle.BattleTeamScript{
		TeamIndex:       teamIndex,
		Script:          proto.Clone(script).(*battle.BattleScript),
		Origin:          battle.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_OFFICIAL,
		ChooserPlayerId: chooser,
	}
}

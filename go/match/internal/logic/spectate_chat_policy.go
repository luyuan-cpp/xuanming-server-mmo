package logic

import (
	"match/internal/config"

	battlepb "proto/battle"
	comp "proto/common/component"
	matchpb "proto/match"
)

// 局外人观战交流开局冻结裁决（docs/planning/battle-spectate-chat-v1.md §3.3）：
//   - 与 spectate_visibility 完全正交：关闭本开关仅禁止观战期间局外人发言交流，绝不影响正常观战可见性；
//   - 建房时调用一次，结果冻结进 CreateBattleRequest.spectate_chat_enabled，整场不可中途改写；
//   - 显式决策表覆盖全部 MatchMode，表外值 fail-closed 返回 false。

type spectateChatModeRule uint8

const (
	spectateChatModeForbid spectateChatModeRule = iota
	spectateChatModePveSolo
	spectateChatModePveTeam
	spectateChatModePvpAllHumans
	spectateChatModePvpTeam
	spectateChatModeTournament
)

// spectateChatRuleOfMode 为每个 MatchMode 显式表态；新增模式未进表时 decided=false（测试即红，运行期 fail-closed）。
func spectateChatRuleOfMode(mode matchpb.MatchMode) (rule spectateChatModeRule, decided bool) {
	switch mode {
	case matchpb.MatchMode_MATCH_MODE_PVE_SOLO:
		return spectateChatModePveSolo, true
	case matchpb.MatchMode_MATCH_MODE_PVE_TEAM:
		return spectateChatModePveTeam, true
	case matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE:
		return spectateChatModePvpAllHumans, true
	case matchpb.MatchMode_MATCH_MODE_JIANGHU_DUEL:
		return spectateChatModePvpAllHumans, true
	case matchpb.MatchMode_MATCH_MODE_PVP_TEAM:
		return spectateChatModePvpTeam, true
	case matchpb.MatchMode_MATCH_MODE_GUILD_WAR_PVP:
		return spectateChatModePvpTeam, true
	case matchpb.MatchMode_MATCH_MODE_GUILD_WAR_SIEGE:
		return spectateChatModePvpTeam, true
	case matchpb.MatchMode_MATCH_MODE_SEASON_TOURNAMENT:
		return spectateChatModeTournament, true
	case matchpb.MatchMode_MATCH_MODE_1V1,
		matchpb.MatchMode_MATCH_MODE_3V3,
		matchpb.MatchMode_MATCH_MODE_5V5,
		matchpb.MatchMode_MATCH_MODE_POLARITY_5V5,
		matchpb.MatchMode_MATCH_MODE_UNSPECIFIED:
		return spectateChatModeForbid, true
	default:
		return spectateChatModeForbid, false
	}
}

func resolvePlayerSpectateChatAllow(pref comp.ESpectateChatPreference, defaultAllow bool) bool {
	switch pref {
	case comp.ESpectateChatPreference_SPECTATE_CHAT_PREF_ALLOW:
		return true
	case comp.ESpectateChatPreference_SPECTATE_CHAT_PREF_FORBID:
		return false
	default:
		return defaultAllow
	}
}

// spectateChatEnabledForBattle 在 gather 建房时一次性裁决本场战斗是否开放「局外人观战交流」。
func spectateChatEnabledForBattle(
	cfg config.SpectateChatConf,
	mode matchpb.MatchMode,
	visibility battlepb.ESpectateVisibility,
	activity *battlepb.BattleActivityContext,
	players []*battlepb.BattlePlayerSnapshot,
) bool {
	if !cfg.IsEnabled() || !isSpectatePublic(visibility) {
		return false
	}
	if activity != nil &&
		activity.GetKind() != battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_NONE &&
		activity.GetKind() != battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_WAR {
		return false
	}
	rule, decided := spectateChatRuleOfMode(mode)
	if !decided || rule == spectateChatModeForbid {
		return false
	}
	defaultAllow := cfg.DefaultAllowsWhenUnset()

	humans := make([]*battlepb.BattlePlayerSnapshot, 0, len(players))
	for _, p := range players {
		if p != nil && !p.GetAiControlled() {
			humans = append(humans, p)
		}
	}
	if len(humans) == 0 {
		return false
	}

	switch rule {
	case spectateChatModePveSolo:
		return resolvePlayerSpectateChatAllow(humans[0].GetSpectateChatPref(), defaultAllow)
	case spectateChatModePveTeam:
		// 组队 PVE 以队长偏好为准；未标记队长时回退首位真人，避免 1 名路人阻塞整队与亲友观众唠嗑。
		for _, p := range humans {
			if p.GetIsTeamCaptain() {
				return resolvePlayerSpectateChatAllow(p.GetSpectateChatPref(), defaultAllow)
			}
		}
		return resolvePlayerSpectateChatAllow(humans[0].GetSpectateChatPref(), defaultAllow)
	case spectateChatModePvpAllHumans:
		return allHumansAllowSpectateChat(humans, defaultAllow)
	case spectateChatModePvpTeam:
		if cfg.TeamPvpCaptainOnly {
			return teamCaptainsAllowSpectateChat(humans, defaultAllow)
		}
		return allHumansAllowSpectateChat(humans, defaultAllow)
	case spectateChatModeTournament:
		if !cfg.IsTournamentEnabled() {
			return false
		}
		if cfg.TeamPvpCaptainOnly {
			return teamCaptainsAllowSpectateChat(humans, defaultAllow)
		}
		return allHumansAllowSpectateChat(humans, defaultAllow)
	default:
		return false
	}
}

func allHumansAllowSpectateChat(humans []*battlepb.BattlePlayerSnapshot, defaultAllow bool) bool {
	for _, p := range humans {
		if !resolvePlayerSpectateChatAllow(p.GetSpectateChatPref(), defaultAllow) {
			return false
		}
	}
	return true
}

func teamCaptainsAllowSpectateChat(humans []*battlepb.BattlePlayerSnapshot, defaultAllow bool) bool {
	// 每队找显式队长；某队无显式队长标记时回退检查该队全部真人（fail-closed，防止漏判野排关闭者）。
	byTeam := make(map[uint32][]*battlepb.BattlePlayerSnapshot, 2)
	for _, p := range humans {
		byTeam[p.GetTeamIndex()] = append(byTeam[p.GetTeamIndex()], p)
	}
	for _, members := range byTeam {
		foundCaptain := false
		for _, p := range members {
			if p.GetIsTeamCaptain() {
				foundCaptain = true
				if !resolvePlayerSpectateChatAllow(p.GetSpectateChatPref(), defaultAllow) {
					return false
				}
			}
		}
		if !foundCaptain {
			if !allHumansAllowSpectateChat(members, defaultAllow) {
				return false
			}
		}
	}
	return true
}

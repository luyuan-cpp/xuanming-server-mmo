package battlescript

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"
	"proto/battle"
)

type fakePlayers struct {
	loadouts map[uint64][]Choice
	approved map[uint64]*battle.BattleScript // script id → 通过版(owner 固定为 7001)
	failRead bool
}

const scriptOwner = 7001

func (f fakePlayers) Loadout(_ context.Context, player uint64) ([]Choice, error) {
	if f.failRead {
		return nil, errors.New("tidb unavailable")
	}
	return f.loadouts[player], nil
}

func (f fakePlayers) Approved(_ context.Context, scriptID, owner uint64) (uint32, *battle.BattleScript, error) {
	script, ok := f.approved[scriptID]
	if !ok || owner != scriptOwner {
		return 0, nil, errors.New("battle script not found")
	}
	return 3, proto.Clone(script).(*battle.BattleScript), nil
}

func resolverFor(t *testing.T, players PlayerScripts, bindings ...*battle.BattleScriptBinding) *Resolver {
	t.Helper()
	catalog, err := BuildCatalog(catalogConfig(bindings...), newFakeTables())
	if err != nil {
		t.Fatal(err)
	}
	return &Resolver{Catalog: catalog, Tables: newFakeTables(), Players: players}
}

func TestPickChoiceGoesFromSpecificToGeneric(t *testing.T) {
	choices := []Choice{
		{MatchMode: 0, BattleConfigID: 0, OfficialScriptID: 1},
		{MatchMode: 5, BattleConfigID: 0, OfficialScriptID: 2},
		{MatchMode: 5, BattleConfigID: 31, OfficialScriptID: 3},
	}
	if got := PickChoice(choices, 5, 31); got.OfficialScriptID != 3 {
		t.Fatalf("mode+dungeon expected, got %d", got.OfficialScriptID)
	}
	if got := PickChoice(choices, 5, 9); got.OfficialScriptID != 2 {
		t.Fatalf("mode expected, got %d", got.OfficialScriptID)
	}
	if got := PickChoice(choices, 3, 9); got.OfficialScriptID != 1 {
		t.Fatalf("global expected, got %d", got.OfficialScriptID)
	}
	if PickChoice(choices[1:], 3, 9) != nil {
		t.Fatal("no matching choice must return nil")
	}
}

func TestChooserPrefersCaptainThenFirstMember(t *testing.T) {
	if got := Chooser([]Member{{PlayerID: 1}, {PlayerID: 2, Captain: true}, {PlayerID: 3, Captain: true}}); got != 2 {
		t.Fatalf("first captain expected, got %d", got)
	}
	if got := Chooser([]Member{{PlayerID: 4}, {PlayerID: 5}}); got != 4 {
		t.Fatalf("first member expected, got %d", got)
	}
	if Chooser(nil) != 0 {
		t.Fatal("empty side has no chooser")
	}
}

func TestResolverUsesChooserChoiceBeforeBinding(t *testing.T) {
	playerScript := baselineScript()
	players := fakePlayers{
		loadouts: map[uint64][]Choice{
			scriptOwner: {{MatchMode: 5, Origin: battle.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_PLAYER, PlayerScriptID: 900}},
			8002:        {{Origin: battle.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_OFFICIAL, OfficialScriptID: 3}},
		},
		approved: map[uint64]*battle.BattleScript{900: playerScript},
	}
	r := resolverFor(t, players, binding(0, 0, sidePlayer, 1), binding(0, 0, sideMonster, 2))

	got := r.Resolve(context.Background(), 5, dungeonID, Side{TeamIndex: 0, Members: []Member{{PlayerID: 8009}, {PlayerID: scriptOwner, Captain: true}}})
	if got.Outcome != OutcomeChosenPlayer || got.Script.GetOrigin() != battle.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_PLAYER ||
		got.Script.GetPlayerScriptId() != 900 || got.Script.GetPlayerScriptRevision() != 3 ||
		got.Script.GetChooserPlayerId() != scriptOwner || got.Script.GetScript().GetScriptId() != 0 {
		t.Fatalf("captain's approved player script expected, got %+v", got)
	}

	official := r.Resolve(context.Background(), 4, 1, Side{TeamIndex: 0, Members: []Member{{PlayerID: 8002}}})
	if official.Outcome != OutcomeChosenOfficial || official.Script.GetScript().GetScriptId() != 3 {
		t.Fatalf("chosen official script expected, got %+v", official)
	}

	bound := r.Resolve(context.Background(), 4, 1, Side{TeamIndex: 0, Members: []Member{{PlayerID: 8003}}})
	if bound.Outcome != OutcomeBound || bound.Script.GetScript().GetScriptId() != 1 || bound.Script.GetChooserPlayerId() != 0 {
		t.Fatalf("official binding expected, got %+v", bound)
	}

	monster := r.Resolve(context.Background(), 4, 1, Side{TeamIndex: 1, Monster: true})
	if monster.Outcome != OutcomeBound || monster.Script.GetScript().GetScriptId() != 2 || monster.Script.GetTeamIndex() != 1 {
		t.Fatalf("monster binding expected, got %+v", monster)
	}
}

func TestResolverFallsBackWhenChoiceIsUnusable(t *testing.T) {
	unknownSkill := baselineScript()
	unknownSkill.Roles[0].Rules[0].SkillId = 4242 // 配表删了这个技能:通过版已不再合法
	cases := map[string]fakePlayers{
		"read failure": {failRead: true},
		"script gone": {loadouts: map[uint64][]Choice{scriptOwner: {{Origin: battle.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_PLAYER, PlayerScriptID: 901}}}},
		"no longer validates": {
			loadouts: map[uint64][]Choice{scriptOwner: {{Origin: battle.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_PLAYER, PlayerScriptID: 902}}},
			approved: map[uint64]*battle.BattleScript{902: unknownSkill},
		},
		"official gone":     {loadouts: map[uint64][]Choice{scriptOwner: {{Origin: battle.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_OFFICIAL, OfficialScriptID: 77}}}},
		"official monster":  {loadouts: map[uint64][]Choice{scriptOwner: {{Origin: battle.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_OFFICIAL, OfficialScriptID: 2}}}},
		"unknown origin":    {loadouts: map[uint64][]Choice{scriptOwner: {{Origin: battle.EBattleScriptOrigin(9)}}}},
	}
	for name, players := range cases {
		withBinding := resolverFor(t, players, binding(0, 0, sidePlayer, 3))
		got := withBinding.Resolve(context.Background(), 5, dungeonID, Side{Members: []Member{{PlayerID: scriptOwner}}})
		if got.Outcome != OutcomeFellBack || got.Reason == "" || got.Script.GetScript().GetScriptId() != 3 {
			t.Errorf("%s: expected fall back to the official binding, got %+v", name, got)
		}
		withoutBinding := resolverFor(t, players)
		got = withoutBinding.Resolve(context.Background(), 5, dungeonID, Side{Members: []Member{{PlayerID: scriptOwner}}})
		if got.Outcome != OutcomeFellBack || got.Script != nil {
			t.Errorf("%s: expected fall back to no script, got %+v", name, got)
		}
	}
}

// 评分局只开官方剧本:玩家剧本的选用当作不存在,继续往通用找官方的选用;都没有就用官方绑定(不算 fell_back)
func TestResolverOfficialOnlySkipsPlayerScripts(t *testing.T) {
	players := fakePlayers{
		loadouts: map[uint64][]Choice{
			scriptOwner: {
				{MatchMode: 3, Origin: battle.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_PLAYER, PlayerScriptID: 900},
				{Origin: battle.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_OFFICIAL, OfficialScriptID: 3},
			},
			8002: {{Origin: battle.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_PLAYER, PlayerScriptID: 900}},
		},
		approved: map[uint64]*battle.BattleScript{900: baselineScript()},
	}
	r := resolverFor(t, players, binding(0, 0, sidePlayer, 1))
	r.OfficialOnly = true

	got := r.Resolve(context.Background(), 3, 0, Side{Members: []Member{{PlayerID: scriptOwner}}})
	if got.Outcome != OutcomeChosenOfficial || got.Script.GetScript().GetScriptId() != 3 {
		t.Fatalf("rated battle must skip the player script and use the official choice, got %+v", got)
	}
	got = r.Resolve(context.Background(), 3, 0, Side{Members: []Member{{PlayerID: 8002}}})
	if got.Outcome != OutcomeBound || got.Script.GetScript().GetScriptId() != 1 {
		t.Fatalf("rated battle with only a player script choice must use the official binding, got %+v", got)
	}
}

func TestResolverOutputIsAnIndependentCopy(t *testing.T) {
	r := resolverFor(t, nil, binding(0, 0, sidePlayer, 1))
	got := r.Resolve(context.Background(), 5, 1, Side{Members: []Member{{PlayerID: 1}}})
	got.Script.Script.Name = "mutated"
	again := r.Resolve(context.Background(), 5, 1, Side{Members: []Member{{PlayerID: 1}}})
	if again.Script.GetScript().GetName() == "mutated" {
		t.Fatal("resolved scripts must not alias the catalog")
	}
}

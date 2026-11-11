package config

import "testing"

func TestPvpTeamSmokeConfigValidate(t *testing.T) {
	valid := PvpTeamSmokeConfig{Zone: 1, BattleConfigId: 1, Phase: PvpTeamSmokePhaseRun, FixturePath: "logs/fixture.json"}
	if err := valid.validate(); err != nil {
		t.Fatalf("合格的配置应通过: %v", err)
	}
	for name, broken := range map[string]func(*PvpTeamSmokeConfig){
		"没有 zone": func(c *PvpTeamSmokeConfig) { c.Zone = 0 },
		"没有模板":    func(c *PvpTeamSmokeConfig) { c.BattleConfigId = 0 },
		"阶段写错":    func(c *PvpTeamSmokeConfig) { c.Phase = "both" },
		"没有夹具路径":  func(c *PvpTeamSmokeConfig) { c.FixturePath = " " },
	} {
		c := valid
		broken(&c)
		if err := c.validate(); err == nil {
			t.Errorf("%s: 应拒绝", name)
		}
	}
}

func TestPvpTeamSmokeConfigDefaultsMatchTheMatchService(t *testing.T) {
	var c PvpTeamSmokeConfig
	if c.GapStepSeconds() != 10 || c.RatingGapMax() != 500 || c.ToleranceBase() != 100 || c.ToleranceStepSeconds() != 5 || c.ToleranceStepDelta() != 100 {
		t.Fatalf("缺省值要与 match 的缺省值一致: %d %d %d %d %d", c.GapStepSeconds(), c.RatingGapMax(), c.ToleranceBase(), c.ToleranceStepSeconds(), c.ToleranceStepDelta())
	}
	custom := PvpTeamSmokeConfig{PremadeGapStepSeconds: 3, PartyRatingGapMax: 200}
	if custom.GapStepSeconds() != 3 || custom.RatingGapMax() != 200 {
		t.Fatal("填了就用填的值")
	}
}

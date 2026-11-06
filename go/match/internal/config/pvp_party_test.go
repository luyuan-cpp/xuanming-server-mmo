package config

// 人数档真人 PVP 计分组排的三个配置项(docs/design/global-match-hierarchical-pools.md 开头第 2、4、10 条):
// 缺省值与设计文档一致,换算成撮合与提交点用的单位。

import (
	"reflect"
	"testing"
)

func TestPvpPartyRuleDefaults(t *testing.T) {
	conf := reflect.TypeOf(GlobalMatchConf{})
	for field, want := range map[string]string{
		"PartyRatingBonusPerMember": ",default=25",
		"PremadeGapStepSeconds":     ",default=10",
		"PartyRatingGapMax":         ",default=500",
	} {
		f, ok := conf.FieldByName(field)
		if !ok {
			t.Fatalf("GlobalMatchConf 缺少字段 %s", field)
		}
		if got := f.Tag.Get("json"); got != want {
			t.Errorf("%s 的缺省值标签 = %q, want %q", field, got, want)
		}
	}

	g := GlobalMatchConf{PartyRatingBonusPerMember: 25, PremadeGapStepSeconds: 10, PartyRatingGapMax: 500}
	if g.PartyBonusCenti() != 2_500 || g.PremadeGapStepMs() != 10_000 || g.PartyRatingGapCenti() != 50_000 {
		t.Fatalf("换算错误: bonus=%d step=%d gap=%d", g.PartyBonusCenti(), g.PremadeGapStepMs(), g.PartyRatingGapCenti())
	}
	var zero GlobalMatchConf
	if zero.PartyBonusCenti() != 0 || zero.PremadeGapStepMs() != 0 || zero.PartyRatingGapCenti() != 0 {
		t.Fatal("直接构造的零值配置:不加成、不限车队差、不限组队评分差")
	}
}

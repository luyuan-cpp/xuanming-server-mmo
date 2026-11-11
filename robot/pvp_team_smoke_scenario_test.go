package main

// pvp-team-smoke 场景里不依赖服务端的部分:情况表本身是否自洽、评分夹具、Elo 预言、等待窗口。

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"robot/config"
)

func TestPvpSmokeCasesAreConsistent(t *testing.T) {
	names := map[string]bool{}
	owners := map[int]string{}
	for _, c := range pvpSmokeCases() {
		if names[c.name] {
			t.Fatalf("情况名重复: %s", c.name)
		}
		names[c.name] = true
		if c.perSide < 1 || c.perSide > 5 {
			t.Fatalf("%s: 每侧人数 %d 越界", c.name, c.perSide)
		}
		if c.wait.gapSteps > 0 && c.wait.toleranceSteps > 0 {
			t.Fatalf("%s: 一种情况只等一条规则放宽", c.name)
		}
		players := 0
		for i, unit := range c.units {
			if len(unit) == 0 {
				t.Fatalf("%s: 第 %d 个单位是空的", c.name, i)
			}
			for _, number := range unit {
				if number < 8701 || number > 8799 {
					t.Fatalf("%s: 账号 %d 不在本场景的 87xx 段", c.name, number)
				}
				if owner, taken := owners[number]; taken {
					t.Fatalf("账号 %d 同时被 %s 和 %s 用", number, owner, c.name)
				}
				owners[number] = c.name
			}
			players += len(unit)
			if c.formsBattle && uint32(len(unit)) > c.perSide {
				t.Fatalf("%s: 成局的情况里单位人数 %d 超过每侧 %d 人", c.name, len(unit), c.perSide)
			}
		}
		if c.formsBattle && uint32(players) != 2*c.perSide {
			t.Fatalf("%s: 成局的情况一共 %d 人,应为 %d", c.name, players, 2*c.perSide)
		}
		for _, pair := range append(append([][2]int{}, c.together...), c.opposite...) {
			if pair[0] < 0 || pair[1] < 0 || pair[0] >= len(c.units) || pair[1] >= len(c.units) || pair[0] == pair[1] {
				t.Fatalf("%s: 单位下标对 %v 越界", c.name, pair)
			}
		}
		if c.settleRatings && !c.formsBattle {
			t.Fatalf("%s: 不成局的情况没有结算可核对", c.name)
		}
	}
}

func TestPvpSmokeSeedsFollowConfigDefaults(t *testing.T) {
	seeds := pvpSmokeSeeds(pvpSmokeCases(), &config.PvpTeamSmokeConfig{})
	want := map[int]int64{
		8703: 150_000, 8704: 170_000, // 起始容差 100 + 一档 100
		8722: 120_000, 8723: 180_000, 8724: 170_000, // 上限 500:超出 100 被拒、恰好等于上限放行
		8725: 150_000, 8726: 160_000, 8727: 140_000, 8728: 150_000, // 极差 200 = 一档之后的容差
	}
	if !reflect.DeepEqual(seeds, want) {
		t.Fatalf("评分夹具 = %v, want %v", seeds, want)
	}
	custom := pvpSmokeSeeds(pvpSmokeCases(), &config.PvpTeamSmokeConfig{PartyRatingGapMax: 300, RatingToleranceBase: 50, RatingToleranceStepDelta: 150})
	if custom[8723] != 160_000 || custom[8724] != 150_000 || custom[8704] != 170_000 || custom[8726] != 160_000 {
		t.Fatalf("评分夹具应跟着配置走: %v", custom)
	}
}

func TestPvpSmokeSelectCases(t *testing.T) {
	all := pvpSmokeCases()
	selected, err := pvpSmokeSelectCases(all, []string{"5v5-duos-split", "1v1"})
	if err != nil || len(selected) != 2 || selected[0].name != "1v1" || selected[1].name != "5v5-duos-split" {
		t.Fatalf("应按定义顺序挑出两种情况: %v %v", selected, err)
	}
	if everything, err := pvpSmokeSelectCases(all, nil); err != nil || len(everything) != len(all) {
		t.Fatalf("不填名字应跑全部: %d %v", len(everything), err)
	}
	if _, err := pvpSmokeSelectCases(all, []string{"1v1", "6v6"}); err == nil {
		t.Fatal("不认识的名字应报错")
	}
}

func TestPvpSmokeCheckRuleConfig(t *testing.T) {
	if err := pvpSmokeCheckRuleConfig(&config.PvpTeamSmokeConfig{}); err != nil {
		t.Fatalf("缺省参数应能区分看评分的情况: %v", err)
	}
	if err := pvpSmokeCheckRuleConfig(&config.PvpTeamSmokeConfig{RatingToleranceBase: 100, RatingToleranceStepDelta: 1}); err == nil {
		t.Fatal("一档只放宽 1 分时分不出「先不成局、一档之后成局」")
	}
}

func TestPvpSmokeExpectedRatings(t *testing.T) {
	sides := map[uint64]uint32{1: 0, 2: 0, 3: 1, 4: 1}
	equal := map[uint64]int64{1: 150_000, 2: 150_000, 3: 150_000, 4: 150_000}
	if got := pvpSmokeExpectedRatings(equal, sides, 1); !reflect.DeepEqual(got, map[uint64]int64{1: 151_600, 2: 151_600, 3: 148_400, 4: 148_400}) {
		t.Fatalf("两队平均分相等时胜方 +16、负方 −16: %v", got)
	}
	if got := pvpSmokeExpectedRatings(equal, sides, 0.5); !reflect.DeepEqual(got, equal) {
		t.Fatalf("两队平均分相等时平局不变: %v", got)
	}
	uneven := map[uint64]int64{1: 160_000, 2: 160_000, 3: 140_000, 4: 140_000}
	if got := pvpSmokeExpectedRatings(uneven, sides, 1); got[1] != 160_769 || got[3] != 139_231 {
		t.Fatalf("强队赢只涨 7.69 分: %v", got)
	}
	// 两队平均分都是 1500,负方里只剩 10 分的那人扣 16 分后封底为 0。
	floor := map[uint64]int64{1: 150_000, 2: 150_000, 3: 299_000, 4: 1_000}
	if got := pvpSmokeExpectedRatings(floor, sides, 1); got[3] != 297_400 || got[4] != 0 {
		t.Fatalf("评分下限是 0: %v", got)
	}
}

func TestPvpSmokeQuietDeadline(t *testing.T) {
	first := time.Unix(1_000, 0)
	last := first.Add(3 * time.Second)
	cfg := &config.PvpTeamSmokeConfig{}
	gap := &pvpSmokeCaseRun{cfg: cfg, c: &pvpSmokeCase{wait: pvpSmokeWait{gapSteps: 2}}}
	if deadline, _, ok := gap.quietDeadline(first, last); !ok || !deadline.Equal(last.Add(18*time.Second)) {
		t.Fatalf("车队差从最后一个单位入队起算 2 个 10 秒步长、提前 2 秒: %v %v", deadline, ok)
	}
	rated := &pvpSmokeCaseRun{cfg: cfg, c: &pvpSmokeCase{wait: pvpSmokeWait{toleranceSteps: 1}}}
	if deadline, _, ok := rated.quietDeadline(first, last); !ok || !deadline.Equal(first.Add(3*time.Second)) {
		t.Fatalf("评分差从锚点入队起算一档 5 秒、提前 2 秒: %v %v", deadline, ok)
	}
	immediate := &pvpSmokeCaseRun{cfg: cfg, c: &pvpSmokeCase{}}
	if _, _, ok := immediate.quietDeadline(first, last); ok {
		t.Fatal("立即可成的情况不观察")
	}
}

func TestPvpSmokeFixtureRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "fixture.json")
	want := pvpSmokeFixture{Mode: 7, Ratings: []pvpSmokeFixtureEntry{{Account: "robot_8722", PlayerID: 11, RatingCenti: 120_000}}}
	if err := pvpSmokeWriteFixture(path, want); err != nil {
		t.Fatalf("写夹具: %v", err)
	}
	got, err := pvpSmokeReadFixture(path)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("读回的夹具 = %+v, %v", got, err)
	}
}

package liveview

import (
	"testing"
	"time"
)

// 测试一律用固定起点 + 手动推进的时刻,不读墙钟。
var t0 = time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)

const testHold = time.Minute

func at(d time.Duration) time.Time { return t0.Add(d) }

func expect(t *testing.T, step string, got, want Verdict) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: verdict = %v, want %v", step, got, want)
	}
}

// 小集群挂一台、大集群挂不到一半都是日常,照常放行,也不留下压住状态。
func TestObserve_OrdinaryLossIsAccepted(t *testing.T) {
	g := NewGate(testHold, RecoverWhenClear)
	expect(t, "两台挂一台", g.Observe(Observation{Known: 2, Missing: 1}, at(0)), Accept)
	expect(t, "三台全缺(少于 MinKnown)", g.Observe(Observation{Known: 3, Missing: 3}, at(0)), Accept)
	expect(t, "四台挂两台(不超过一半)", g.Observe(Observation{Known: 4, Missing: 2}, at(0)), Accept)
	expect(t, "十台挂五台", g.Observe(Observation{Known: 10, Missing: 5}, at(0)), Accept)
	// 前面都没有进入压住:一次正常的小损失之后,再来一次正常的小损失照样放行。
	expect(t, "之后再挂一台", g.Observe(Observation{Known: 10, Missing: 1}, at(time.Hour)), Accept)
}

// 存活集为空而基线不空:不管压了多久都不自动放行。
func TestObserve_EmptyIsHeldForever(t *testing.T) {
	g := NewGate(testHold, RecoverWhenClear)
	for i := 0; i < 10; i++ {
		expect(t, "空集", g.Observe(Observation{Known: 1, Missing: 1, Empty: true}, at(time.Duration(i)*testHold)), HoldEmpty)
	}
	// 基线也为空:没有可比的,放行。
	expect(t, "基线为空", g.Observe(Observation{Known: 0, Missing: 0, Empty: true}, at(11*testHold)), Accept)
}

// 骤降先压住;缺席数连续 hold 不变才放行,Release 只返回一次,放行期内照常放行,放行期过了仍过半就重新压住。
func TestObserve_StableDropIsReleasedOnceAfterHold(t *testing.T) {
	g := NewGate(testHold, RecoverWhenClear)
	drop := Observation{Known: 10, Missing: 6}
	expect(t, "骤降", g.Observe(drop, at(0)), HoldDrop)
	expect(t, "压住中", g.Observe(drop, at(testHold-time.Second)), HoldDrop)
	expect(t, "满 hold", g.Observe(drop, at(testHold)), Release)
	expect(t, "放行期内", g.Observe(drop, at(testHold+time.Second)), Accept)
	expect(t, "放行期内缺席变化也照常放行", g.Observe(Observation{Known: 10, Missing: 7}, at(testHold+2*time.Second)), Accept)
	expect(t, "放行期过了仍过半", g.Observe(drop, at(2*testHold)), HoldDrop)
	expect(t, "第二次满 hold", g.Observe(drop, at(3*testHold)), Release)
}

// 缺席数一变(有成员回来,或又少了)计时从头算。
func TestObserve_ChangeRestartsTheClock(t *testing.T) {
	g := NewGate(testHold, RecoverWhenClear)
	expect(t, "骤降", g.Observe(Observation{Known: 10, Missing: 8}, at(0)), HoldDrop)
	expect(t, "回来两台", g.Observe(Observation{Known: 10, Missing: 6}, at(50*time.Second)), HoldDrop)
	expect(t, "从回来那一刻起不满 hold", g.Observe(Observation{Known: 10, Missing: 6}, at(100*time.Second)), HoldDrop)
	expect(t, "又少一台", g.Observe(Observation{Known: 10, Missing: 7}, at(105*time.Second)), HoldDrop)
	expect(t, "从又少那一刻起不满 hold", g.Observe(Observation{Known: 10, Missing: 7}, at(164*time.Second)), HoldDrop)
	expect(t, "稳定满 hold", g.Observe(Observation{Known: 10, Missing: 7}, at(165*time.Second)), Release)
}

// 压住期间恢复到一半以下也继续压住(还没恢复完的那些多半也正在重新注册),恢复完复位。
func TestObserve_RecoveringBelowHalfStaysHeldUntilClear(t *testing.T) {
	g := NewGate(testHold, RecoverWhenClear)
	expect(t, "骤降", g.Observe(Observation{Known: 10, Missing: 9}, at(0)), HoldDrop)
	expect(t, "恢复到两台缺席", g.Observe(Observation{Known: 10, Missing: 2}, at(10*time.Second)), HoldDrop)
	expect(t, "恢复完", g.Observe(Observation{Known: 10, Missing: 0}, at(20*time.Second)), Accept)
	// 复位之后,一台的日常损失直接放行,不再沿用刚才的压住。
	expect(t, "复位后挂一台", g.Observe(Observation{Known: 10, Missing: 1}, at(30*time.Second)), Accept)
	expect(t, "复位后再次骤降要重新压住", g.Observe(Observation{Known: 10, Missing: 6}, at(40*time.Second)), HoldDrop)
}

// 空集之后回到残缺:空集期间计时一直从头算,回到残缺后还要再稳定满 hold。
func TestObserve_EmptyThenPartialNeedsAFullHold(t *testing.T) {
	g := NewGate(testHold, RecoverWhenClear)
	expect(t, "空集", g.Observe(Observation{Known: 10, Missing: 10, Empty: true}, at(0)), HoldEmpty)
	expect(t, "仍是空集", g.Observe(Observation{Known: 10, Missing: 10, Empty: true}, at(5*testHold)), HoldEmpty)
	// 回来三台,只剩七台缺席:还在压住期(空集已经让它进入压住),计时从这一刻起。
	expect(t, "回到残缺", g.Observe(Observation{Known: 10, Missing: 7}, at(5*testHold+time.Second)), HoldDrop)
	expect(t, "不满 hold", g.Observe(Observation{Known: 10, Missing: 7}, at(6*testHold)), HoldDrop)
	expect(t, "满 hold", g.Observe(Observation{Known: 10, Missing: 7}, at(6*testHold+time.Second)), Release)
	// 放行期里视图又变成空集:不当成「照常放行」,重新压住。
	expect(t, "放行期里变空", g.Observe(Observation{Known: 10, Missing: 10, Empty: true}, at(6*testHold+2*time.Second)), HoldEmpty)
}

// 没有可比的观察(基线为空)不是恢复的证据:压住状态和计时都不动。
func TestObserve_NoBaselineKeepsTheHold(t *testing.T) {
	g := NewGate(testHold, RecoverWhenClear)
	drop := Observation{Known: 4, Missing: 3}
	expect(t, "骤降", g.Observe(drop, at(0)), HoldDrop)
	expect(t, "这一拍没有基线", g.Observe(Observation{}, at(30*time.Second)), Accept)
	expect(t, "回到同一视图,计时没被打断", g.Observe(drop, at(testHold)), Release)
}

// 时刻倒退(只会出现在调用方传错时刻时)按「还没满」处理:宁可多压。
func TestObserve_BackwardsTimeKeepsHolding(t *testing.T) {
	g := NewGate(testHold, RecoverWhenClear)
	drop := Observation{Known: 4, Missing: 4}
	expect(t, "骤降", g.Observe(drop, at(time.Hour)), HoldDrop)
	expect(t, "时刻倒退", g.Observe(drop, at(0)), HoldDrop)
}

func TestVerdictHeld(t *testing.T) {
	for v, want := range map[Verdict]bool{Accept: false, HoldEmpty: true, HoldDrop: true, Release: false} {
		if v.Held() != want {
			t.Fatalf("%v.Held() = %v, want %v", v, v.Held(), want)
		}
	}
}

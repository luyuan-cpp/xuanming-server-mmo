package logic

import (
	"errors"
	"os"
	"regexp"
	"sort"
	"testing"
)

// playerLocatorAlertRulesFile 相对本包目录(go/player_locator/internal/logic)。
const playerLocatorAlertRulesFile = "../../../../deploy/k8s/player-locator-alerts.yaml"

var playerLocatorAlertMetricName = regexp.MustCompile(`\bplayer_locator_[a-z0-9_]+`)

// TestAlertRulesReferenceRegisteredMetrics 是告警文件与指标注册之间的机械守卫(照 player_inbox 的同名用例):
// deploy/k8s/player-locator-alerts.yaml 里凡是以指标前缀开头的词(表达式、注释、描述都算)都必须是
// reconcile_metrics.go 注册过的指标名。指标改名 / 删除而告警漏改时,increase() / max() 对不存在的序列
// 返回空集,告警静默失效,全仓没有别的测试会红。文件读不到、一个名字都抽不到都判红。
func TestAlertRulesReferenceRegisteredMetrics(t *testing.T) {
	raw, err := os.ReadFile(playerLocatorAlertRulesFile)
	if err != nil {
		t.Fatalf("读告警规则 %s 失败(移动了文件就同步改这里): %v", playerLocatorAlertRulesFile, err)
	}
	registered := toSet(reconcileMetricFullNames())
	seen := map[string]bool{}
	for _, m := range playerLocatorAlertMetricName.FindAll(raw, -1) {
		seen[string(m)] = true
	}
	if len(seen) == 0 {
		t.Fatalf("%s 里一个指标名都没抽到(改了写法就同步改这里的正则)", playerLocatorAlertRulesFile)
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := registered[name]; !ok {
			t.Errorf("告警文件引用了未注册的指标 %s(已注册:%v)", name, reconcileMetricFullNames())
		}
	}
}

// 预置为 0 的 label 组合必须覆盖代码会发出的全部取值:漏一个,increase() 就会漏掉它的第一次。
func TestReconcileMetricLabelsArePrimed(t *testing.T) {
	primed := map[[2]string]bool{}
	for _, c := range reconcileRoundCombos {
		if primed[c] {
			t.Errorf("reconcile_round_total 预置组合重复:%v", c)
		}
		primed[c] = true
	}
	for _, stage := range []roundStage{"", stageEtcd, stageRedis, stagePanic} {
		out := roundOutcome{stage: stage}
		if stage != "" {
			out.err = errTestSentinel
		}
		if c := [2]string{reconcileLoopIndex, out.result()}; !primed[c] {
			t.Errorf("死 gate 清扫会发出 %v,但没有预置", c)
		}
		if c := [2]string{reconcileLoopWriters, out.result()}; !primed[c] {
			t.Errorf("写者检查会发出 %v,但没有预置", c)
		}
	}
	for _, c := range [][2]string{
		{reconcileLoopRepair, reconcileResultOK}, {reconcileLoopRepair, reconcileResultEtcdError},
		{reconcileLoopRepair, reconcileResultRedisError}, {reconcileLoopRepair, reconcileResultGenChanged},
		{reconcileLoopRepair, reconcileResultIncomplete}, {reconcileLoopRepair, reconcileResultPanic},
		{reconcileLoopAudit, reconcileResultOK}, {reconcileLoopAudit, reconcileResultRedisError},
	} {
		if !primed[c] {
			t.Errorf("补建 / 审计会发出 %v,但没有预置", c)
		}
	}

	actions := toSet(reconcileSessionActions)
	if len(actions) != len(reconcileSessionActions) {
		t.Errorf("reconcile_sessions_total 预置 action 重复:%v", reconcileSessionActions)
	}
	for _, a := range []string{
		reconcileActionToDisconnecting, reconcileActionCASConflict, reconcileActionPrunedStale,
		reconcileActionPrunedMissing, reconcileActionPrunedMalformed, reconcileActionPrunedBadMember,
		reconcileActionRepaired, reconcileActionAuditRepaired, reconcileActionMemberError, reconcileActionGateError,
	} {
		if _, ok := actions[a]; !ok {
			t.Errorf("action %s 没有预置", a)
		}
	}

	// last_success 的预置(primeReconcileMetrics)按 reconcileLoops 走:它必须覆盖 reconcile_round_total 用到的每个 loop。
	// 漏一个,从未成功过的 Pod 就没有那条序列,Stalled 一类规则对它失明。
	// 随任期清零的只有 leader 专属的 loop:清扫每个副本都在跑,与谁是 leader 无关,换主不能把它清零。
	for _, loop := range reconcileLeaderLoops {
		if loop == reconcileLoopIndex {
			t.Errorf("清扫不是 leader 专属的 loop,不能随任期清零")
		}
	}
	// (单测里 go-zero 的指标后端不生效,读不到运行期的值,只能做表级核对。)
	loops := toSet(reconcileLoops)
	if len(loops) != len(reconcileLoops) {
		t.Errorf("reconcileLoops 有重复:%v", reconcileLoops)
	}
	for _, loop := range []string{reconcileLoopIndex, reconcileLoopWriters, reconcileLoopRepair, reconcileLoopAudit} {
		if _, ok := loops[loop]; !ok {
			t.Errorf("loop %s 不在 reconcileLoops 里,它的 last_success 不会被预置为 0", loop)
		}
	}
	for _, c := range reconcileRoundCombos {
		if _, ok := loops[c[0]]; !ok {
			t.Errorf("reconcile_round_total 用到的 loop %s 不在 reconcileLoops 里", c[0])
		}
	}
}

var errTestSentinel = errors.New("sentinel")

package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// ── 账号目录:建角之后、删角之后「失败只记日志、不改结局」那几步与后台收尾(docs/design/guild-phase2/03-names.md §3.4a)──
//
// 告警规则在 deploy/k8s/data-service-account-directory-alerts.yaml,tools/scripts/tests/data_service_account_directory_alerts_contract.tests.ps1
// 从本文件核对指标名与 label 名。label 只有封闭集合,不带 player_id(AGENTS.md §9),逐条明细在 `[player-name]`、
// `[account-role-removal]`、`[account-role-ref]` 的 ERROR 日志里。单独一把 sync.Once 注册,不动 metrics.go 那张 MustRegister 清单。

// account_role_removal_resume_total{result} 的取值:后台 ResumeRemovals 一轮里每一行的结局(store.RemovalResumeResult)。
const (
	RoleRemovalResumeFinished  = "finished"  // 墓碑已提交、收尾做完
	RoleRemovalResumeAbandoned = "abandoned" // 有挂起,放弃了这次删角
	RoleRemovalResumeUnsettled = "unsettled" // 墓碑已提交,收尾(释放名字、删小表那一行)这一轮还是没做完
	RoleRemovalResumeFailed    = "failed"    // 删角本身这一轮没做下去(参与者不可达、库出错)
)

var (
	roleRemovalResumeResults = []string{RoleRemovalResumeFinished, RoleRemovalResumeAbandoned, RoleRemovalResumeUnsettled, RoleRemovalResumeFailed}

	// 建角提交之后名字没绑上(名字行停在预留态,或者一名两角)。reason 取值在 store.NameBindFailureReasons。
	roleNameBindFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "account_role_name_bind_failures_total",
		Help: "Names of committed roles that were not bound right after the commit, by reason " +
			"(error|taken|conflict|role_mismatch|role_removed|rules_changed). The role stays created; replay the create or run " +
			"create_residual -repair name. taken / conflict mean one name held by two roles and need a decision.",
	}, []string{"reason"})

	// 删角墓碑之后的收尾没做完(前台删角与后台收尾都计)。step 取值在 store.RemovalSettleSteps。
	roleRemovalSettleFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "account_role_removal_settle_failures_total",
		Help: "Role removals whose post-tombstone settle did not finish, by step (release_name|drop_removal_row). " +
			"The removal stays committed; the name stays held by the removed role and the background resumer retries.",
	}, []string{"step"})

	roleRemovalResumeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "account_role_removal_resume_total",
		Help: "Rows handled by the background role removal resumer, by result (finished|abandoned|unsettled|failed). " +
			"unsettled: tombstone committed but the settle (name release, removal row) still failed; failed: the removal itself did not progress.",
	}, []string{"result"})

	// 后台判陈旧标了保留、等人处理的建角 ref 条数(各账号片合计,store.SweepResult.HeldTotal)。每个副本报同一个值,告警取 max。
	// 不预置:这一轮没数成就不写,免得出现一个看着像「没有积压」的 0。
	roleRefHeld = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "account_role_ref_held",
		Help: "Stale create refs kept for review across all account shards (role row missing, committed evidence present or unreadable). " +
			"They are skipped by the background sweep and rechecked in a small separate lane; every replica reports the same total, alert on max.",
	})

	accountDirectoryRegisterOnce sync.Once
)

func registerAccountDirectory() {
	accountDirectoryRegisterOnce.Do(func() {
		prometheus.MustRegister(roleNameBindFailuresTotal, roleRemovalSettleFailuresTotal, roleRemovalResumeTotal, roleRefHeld)
	})
}

// PrimeAccountDirectory 预置全部封闭集合为 0(理由同 PrimeRoute:这几种失败一辈子可能只发生几次,序列不预先存在的话
// `increase(...) > 0` 恰好漏掉第一次)。bindReasons、settleSteps 由装配层从 store 取(store.NameBindFailureReasons /
// RemovalSettleSteps),本包不依赖 store。可重复调用。
func PrimeAccountDirectory(bindReasons, settleSteps []string) {
	registerAccountDirectory()
	for _, reason := range bindReasons {
		roleNameBindFailuresTotal.WithLabelValues(reason).Add(0)
	}
	for _, step := range settleSteps {
		roleRemovalSettleFailuresTotal.WithLabelValues(step).Add(0)
	}
	for _, result := range roleRemovalResumeResults {
		roleRemovalResumeTotal.WithLabelValues(result).Add(0)
	}
}

// ObserveRoleNameBindFailure 记一次建角提交之后名字没绑上。reason 取 store.NameBindFailure* 常量,本函数不校验。
func ObserveRoleNameBindFailure(reason string) {
	registerAccountDirectory()
	roleNameBindFailuresTotal.WithLabelValues(reason).Inc()
}

// ObserveRoleRemovalSettleFailure 记一次删角墓碑之后的收尾没做完。step 取 store.RemovalSettle* 常量,本函数不校验。
func ObserveRoleRemovalSettleFailure(step string) {
	registerAccountDirectory()
	roleRemovalSettleFailuresTotal.WithLabelValues(step).Inc()
}

// ObserveRoleRemovalResume 记一轮后台删角收尾的结局,各结局按行数累加。
func ObserveRoleRemovalResume(finished, abandoned, unsettled, failed int) {
	registerAccountDirectory()
	for _, r := range []struct {
		result string
		n      int
	}{
		{RoleRemovalResumeFinished, finished},
		{RoleRemovalResumeAbandoned, abandoned},
		{RoleRemovalResumeUnsettled, unsettled},
		{RoleRemovalResumeFailed, failed},
	} {
		if r.n > 0 {
			roleRemovalResumeTotal.WithLabelValues(r.result).Add(float64(r.n))
		}
	}
}

// SetRoleRefHeld 写这一轮数出来的保留 ref 总数。
func SetRoleRefHeld(n int) {
	registerAccountDirectory()
	roleRefHeld.Set(float64(n))
}

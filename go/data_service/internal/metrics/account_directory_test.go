package metrics

import "testing"

// TestPrimeAccountDirectoryCreatesAllSeries 锁死账号目录告警(deploy/k8s/data-service-account-directory-alerts.yaml)的前提:
// PrimeAccountDirectory 之后,三个计数在给出的封闭集合上的子序列全部存在(值是 0 或同包别的用例打过的点),
// 后台收尾的四种结局一个不少;没有多出集合之外的取值。这几种失败一辈子可能只发生几次,序列不预先存在,
// `increase(...) > 0` 恰好漏掉第一次(同 TestPrimeRouteCreatesAllSeries)。
func TestPrimeAccountDirectoryCreatesAllSeries(t *testing.T) {
	reasons := []string{"error", "taken", "conflict", "role_mismatch", "role_removed", "rules_changed"}
	steps := []string{"release_name", "drop_removal_row"}
	PrimeAccountDirectory(reasons, steps)

	for _, spec := range []struct {
		family string
		label  string
		want   []string
	}{
		{"data_service_account_role_name_bind_failures_total", "reason", reasons},
		{"data_service_account_role_removal_settle_failures_total", "step", steps},
		{"data_service_account_role_removal_resume_total", "result", roleRemovalResumeResults},
	} {
		want := map[string]bool{}
		for _, v := range spec.want {
			want[v] = true
		}
		seen := gatherFamily(t, spec.family)
		if len(seen) != len(want) {
			t.Fatalf("%s has %d series after priming, want %d", spec.family, len(seen), len(want))
		}
		for _, m := range seen {
			if key := labelKey(m, spec.label); !want[key] {
				t.Fatalf("%s{%s=%q} is not in the closed set", spec.family, spec.label, key)
			}
		}
	}
	if len(roleRemovalResumeResults) != 4 {
		t.Fatalf("roleRemovalResumeResults = %v, want finished / abandoned / unsettled / failed", roleRemovalResumeResults)
	}
}

// TestObserveRoleRemovalResumeAddsEachResult:一轮后台收尾的四种结局各按行数累加到自己的序列上,0 行的不动。
func TestObserveRoleRemovalResumeAddsEachResult(t *testing.T) {
	PrimeAccountDirectory(nil, nil)
	value := func(result string) float64 {
		for _, m := range gatherFamily(t, "data_service_account_role_removal_resume_total") {
			if labelKey(m, "result") == result {
				return m.GetCounter().GetValue()
			}
		}
		t.Fatalf("no series for result=%s", result)
		return 0
	}
	before := map[string]float64{}
	for _, result := range roleRemovalResumeResults {
		before[result] = value(result)
	}
	ObserveRoleRemovalResume(3, 0, 2, 1)
	for result, delta := range map[string]float64{RoleRemovalResumeFinished: 3, RoleRemovalResumeAbandoned: 0, RoleRemovalResumeUnsettled: 2, RoleRemovalResumeFailed: 1} {
		if got := value(result); got != before[result]+delta {
			t.Fatalf("resume_total{result=%s} = %v, want %v", result, got, before[result]+delta)
		}
	}
}

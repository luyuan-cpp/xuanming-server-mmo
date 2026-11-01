package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// ── 角色护栏协调者(docs/design/horizontal-scale-logical-db.md §3.5)──
//
// participant 是固定的六个参与者,result、push_result 是封闭集合,blocker 是参与者给的低基数枚举串。
// 不带 player_id(AGENTS.md §9)。

// role_fence_prepare_total{participant,result} 的取值。
const (
	RoleFenceResultClean       = "clean"
	RoleFenceResultBlocked     = "blocked"
	RoleFenceResultStale       = "stale"
	RoleFenceResultUnreachable = "unreachable"
)

// role_fence_push_total{participant,result} 的取值。
const (
	RoleFencePushOK     = "ok"
	RoleFencePushFailed = "failed"
)

var (
	roleFencePrepareTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "role_fence_prepare_total",
		Help:      "PrepareRoleFreeze calls by participant and result (clean|blocked|stale|unreachable).",
	}, []string{"participant", "result"})

	roleFenceBlockerTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "role_fence_blocker_total",
		Help:      "PrepareRoleFreeze answers that reported a pending item, by participant and blocker.",
	}, []string{"participant", "blocker"})

	roleFencePushTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "role_fence_push_total",
		Help:      "ApplyRoleFence pushes by participant and result (ok|failed). A failed push self-heals on the next access.",
	}, []string{"participant", "result"})

	roleFenceRegisterOnce sync.Once
)

func registerRoleFence() {
	roleFenceRegisterOnce.Do(func() {
		prometheus.MustRegister(roleFencePrepareTotal, roleFenceBlockerTotal, roleFencePushTotal)
	})
}

// PrimeRoleFence 预建全部组合(理由同 PrimeRoute)。blocker 的取值由参与者决定,不预建。
func PrimeRoleFence(participants []string) {
	registerRoleFence()
	for _, participant := range participants {
		for _, result := range []string{RoleFenceResultClean, RoleFenceResultBlocked, RoleFenceResultStale, RoleFenceResultUnreachable} {
			roleFencePrepareTotal.WithLabelValues(participant, result)
		}
		for _, result := range []string{RoleFencePushOK, RoleFencePushFailed} {
			roleFencePushTotal.WithLabelValues(participant, result)
		}
	}
}

// ObserveRoleFencePrepare 记一个参与者一次 PrepareRoleFreeze 的结局。
func ObserveRoleFencePrepare(participant, result string) {
	registerRoleFence()
	roleFencePrepareTotal.WithLabelValues(participant, result).Inc()
}

// ObserveRoleFenceBlocker 记一次挂起原因。
func ObserveRoleFenceBlocker(participant, blocker string) {
	registerRoleFence()
	roleFenceBlockerTotal.WithLabelValues(participant, blocker).Inc()
}

// ObserveRoleFencePush 记一次推送的结局。
func ObserveRoleFencePush(participant, result string) {
	registerRoleFence()
	roleFencePushTotal.WithLabelValues(participant, result).Inc()
}

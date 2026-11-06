package data

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 2026-11-05 修复（r2-guild#1 评审）回归：launch 恢复器的调度纯函数。不连库、不读墙钟。

// 首次检查时刻必须落在开局许可窗口之后:Start 还可能在路上时去取消,取消墓碑会让合法的 Start 被 match 拒绝。
// 宽限本身是否盖住开局总预算由 logic 的 TestTrialRecoveryGraceCoversLaunchBudget 核对(窗口与 RPC 上限定义在 logic / config)。
func TestTrialFirstCheckWaitsPastLaunchWindow(t *testing.T) {
	grace := uint64(TrialRecoveryGrace.Milliseconds())
	// 正常:Prepare 在开局截止之前提交,首次检查 = 截止 + 宽限。
	require.Equal(t, 10_000+grace, trialFirstCheckMs(10_000, 6_000))
	// Prepare 因锁排队拖过截止才提交:许可从提交那一刻起才可能被用,从提交时刻起算。
	require.Equal(t, 12_000+grace, trialFirstCheckMs(10_000, 12_000))
}

func TestTrialRecheckDelayBacksOffToCapWithBoundedJitter(t *testing.T) {
	for attempts, want := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 2 * time.Minute, 2 * time.Minute} {
		require.Equal(t, want, trialRecheckDelay(uint32(attempts), 0), "第 %d 次认领", attempts)
	}
	require.Equal(t, trialRecheckCap, trialRecheckDelay(math.MaxUint32, 0), "认领次数再大也封顶、不溢出")
	// 抖动至多加 1/5。
	require.Equal(t, 12*time.Second, trialRecheckDelay(0, 1))
	require.Less(t, trialRecheckDelay(30, 0.999), trialRecheckCap*6/5)
	// 最短间隔要长于单条检查的上限(logic 的 trialLaunchCallBudget = 2s):认领后的这一次检查做完之前,别的副本认领不到它。
	require.Greater(t, trialRecheckDelay(0, 0), 2*time.Second)
}

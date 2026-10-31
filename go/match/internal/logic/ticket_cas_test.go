package logic

// 备战预算(cross-zone-matchmaking.md D5):按组大小计算,逐跳覆盖最坏链路。

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// matchedWorstCaseSeconds 是 D5 公式里"required 人串行备战最坏耗时(向上取整到秒)+ 10s"
// 的独立算法(测试不复用被测函数的表达式,也不引用 gatherCreateStageWorst,用 gather.go /
// spectate.go 常量逐跳重新拼):每人 RemoveObserver 3s + PrepareBattle 3s;全齐后按两轮计 CreateBattle 5s;
// 失败回滚 3s。
func matchedWorstCaseSeconds(required int) int {
	chain := time.Duration(required)*(removeObserverTimeout+prepareBattleTimeout) +
		createBattleTimeout + createBattleTimeout +
		rollbackTimeout
	return int((chain+time.Second-1)/time.Second) + 10
}

// compensationWorstCaseSeconds 补偿路径最坏耗时:已冻结者逐人 CancelBattlePrepare + 余量。
func compensationWorstCaseSeconds(prepared int) int {
	return prepared*int(rollbackTimeout/time.Second) + 10
}

func TestMatchedTicketTTLFormula(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	// 每档 = ⌈N×6 + 2×5 + 3⌉ + 10(逐跳见 matchedWorstCaseSeconds)。
	// PVE solo 一人:⌈19⌉+10 = 29s,低于配置 30s,取配置。
	require.Equal(t, 29, matchedWorstCaseSeconds(1))
	require.Equal(t, 30, matchedTicketTTLFor(svcCtx, 1))
	// 1V1 两人:⌈25⌉+10 = 35s。
	require.Equal(t, matchedWorstCaseSeconds(2), matchedTicketTTLFor(svcCtx, 2))
	require.Equal(t, 35, matchedTicketTTLFor(svcCtx, 2))
	// PVE_TEAM 五人:⌈43⌉+10 = 53s。
	require.Equal(t, matchedWorstCaseSeconds(5), matchedTicketTTLFor(svcCtx, 5))
	require.Equal(t, 53, matchedTicketTTLFor(svcCtx, 5))
	// 5v5 十人:⌈73⌉+10 = 83s。
	require.Equal(t, matchedWorstCaseSeconds(10), matchedTicketTTLFor(svcCtx, 10))
	require.Equal(t, 83, matchedTicketTTLFor(svcCtx, 10))
	require.Greater(t, matchedTicketTTLFor(svcCtx, 10),
		10*int((removeObserverTimeout+prepareBattleTimeout)/time.Second)+int(createBattleTimeout/time.Second),
		"必须覆盖含观战清退的成功路径")
	// 配置是下限:抬到 50s 时小组取配置,大组仍取公式。
	svcCtx.Config.MatchedTicketTTLSeconds = 50
	require.Equal(t, 50, matchedTicketTTLFor(svcCtx, 1))
	require.Equal(t, 50, matchedTicketTTLFor(svcCtx, 2))
	require.Equal(t, 53, matchedTicketTTLFor(svcCtx, 5))
	// 配置抬高到 120s 时所有组都取配置。
	svcCtx.Config.MatchedTicketTTLSeconds = 120
	require.Equal(t, 120, matchedTicketTTLFor(svcCtx, 10))
	// 漏配(0)不会得到 0 TTL:缺省下限 30s 高于一人的公式值,取下限;两人起取公式。
	svcCtx.Config.MatchedTicketTTLSeconds = 0
	require.Equal(t, 30, matchedTicketTTLFor(svcCtx, 1))
	require.Equal(t, matchedWorstCaseSeconds(2), matchedTicketTTLFor(svcCtx, 2))

	// 补偿窗口:十人全冻结后逐人解冻 10×3+10=40s。
	require.Equal(t, compensationWorstCaseSeconds(10), compensationTicketTTLFor(10))
	require.Equal(t, 40, compensationTicketTTLFor(10))
}

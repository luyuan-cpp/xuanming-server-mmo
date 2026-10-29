package logic

// 观战结论判定的纯函数测试(不依赖 Redis / TiDB):
//   - spectateRejectVerdict:battle AddObserver 拒绝码 → 客户端 tip / outcome / 是否懒剔除,两条路径共用;
//   - globalWatchErrVerdict:全局路径错误 → 客户端结论,钉住判定顺序与故障口径;
//   - closedWatchVerdict:意图上持久化的关闭原因 → 重放结论;
//   - 首答(按错误判定)与重放(按持久化原因判定)逐字一致;
//   - 全局入口参数错在任何 SQL 之前拒绝且计指标。
// 全局路径的 SQL 状态机由 authority 包的 *_native_test.go(MATCH_IT_TIDB)覆盖。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"match/internal/authority"
	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/svc"

	pb "proto/match"

	"shared/generated/pb/table"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestSpectateRejectVerdict(t *testing.T) {
	cases := []struct {
		name        string
		battleTip   uint32
		wantTip     uint32
		wantOutcome string
		wantMissing bool
	}{
		{"房间不存在", uint32(table.CommonError_kEntityIsNull), constants.ErrBattleNotWatchable, "not_found", true},
		{"观众席位已满", uint32(table.MatchError_kMatchSpectateFull), constants.ErrSpectateFull, "full", false},
		{"墓碑表满", uint32(table.CommonError_kRateLimitExceeded), constants.ErrBattleNotWatchable, "rejected", false},
		{"观众是参战者", uint32(table.CommonError_kInvalidParameter), constants.ErrBattleNotWatchable, "rejected", false},
		{"签不出票", uint32(table.CommonError_kServiceUnavailable), constants.ErrBattleNotWatchable, "rejected", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tip, outcome, missing := spectateRejectVerdict(c.battleTip)
			require.Equal(t, c.wantTip, tip)
			require.Equal(t, c.wantOutcome, outcome)
			require.Equal(t, c.wantMissing, missing, "只有房间不存在才允许懒剔除索引")
		})
	}
}

// 细分错误必须保住既有 errors.Is 判断:调用方(Join / Team / 恢复)仍按 ErrMatchEntryOccupied / ErrSpectatorBusy 分支。
func TestSpectatorTypedErrorsKeepLegacyErrorsIs(t *testing.T) {
	for _, err := range []error{authority.ErrTicketQueued, authority.ErrTicketInBattle} {
		require.ErrorIs(t, err, authority.ErrMatchEntryOccupied)
	}
	for _, err := range []error{authority.ErrSpectatorSuperseded, authority.ErrSpectatorOccupied, authority.ErrSpectatorFormationClosed, authority.ErrSpectatorIntentClosed, errGlobalSpectatePending} {
		require.ErrorIs(t, err, authority.ErrSpectatorBusy)
	}
}

func TestGlobalWatchErrVerdictOrder(t *testing.T) {
	// BeginSpectator 的票据占用同时包装观战占用与票据细分(spectator.go)。
	ticketInBattle := fmt.Errorf("%w: %w", authority.ErrSpectatorBusy, authority.ErrTicketInBattle)
	ticketQueued := fmt.Errorf("%w: %w", authority.ErrSpectatorBusy, authority.ErrTicketQueued)
	cases := []struct {
		name        string
		err         error
		wantTip     uint32
		wantOutcome string
		wantFault   bool
	}{
		{"包装后的已建房先于匹配中", ticketInBattle, constants.ErrSpectateWhileInBattle, "in_battle", false},
		{"再包一层仍是已建房", fmt.Errorf("开局占用: %w", ticketInBattle), constants.ErrSpectateWhileInBattle, "in_battle", false},
		{"票排队", ticketQueued, constants.ErrSpectateWhileQueued, "queued", false},
		{"组队或历练占用", authority.ErrMatchEntryOccupied, constants.ErrSpectateWhileQueued, "queued", false},
		{"成局已变", authority.ErrSpectatorFormationClosed, constants.ErrBattleNotWatchable, "not_found", false},
		{"意图已关闭", authority.ErrSpectatorIntentClosed, constants.ErrBattleNotWatchable, "closed", false},
		{"请求头已被替代", authority.ErrSpectatorSuperseded, constants.ErrAlreadyWatching, "already_watching", false},
		{"旧占用未关", authority.ErrSpectatorOccupied, constants.ErrAlreadyWatching, "already_watching", false},
		{"未获终局证明", errGlobalSpectatePending, constants.ErrInternal, "pending", false},
		{"裸 Busy 按故障", authority.ErrSpectatorBusy, constants.ErrInternal, "internal", true},
		{"原角色归属失效 fail-closed", authority.ErrOriginalPlayer, constants.ErrInternal, "internal", true},
		{"存储错误", sql.ErrConnDone, constants.ErrInternal, "internal", true},
		{"超时", context.DeadlineExceeded, constants.ErrInternal, "internal", true},
		{"未知错误", errors.New("dial battle failed"), constants.ErrInternal, "internal", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			verdict := globalWatchErrVerdict(c.err)
			require.Equal(t, c.wantTip, verdict.tip)
			require.Equal(t, c.wantOutcome, verdict.outcome)
			require.Equal(t, c.wantFault, verdict.fault, "只有服务端故障才打 Error 日志")
			require.NotEmpty(t, verdict.msg)
		})
	}
}

// 关闭原因只决定一种结论:首答与同 request_id 重放读的是同一个字段,得到同一个 tip 与 outcome。
func TestClosedWatchVerdict(t *testing.T) {
	cases := []struct {
		name        string
		closeTip    uint32
		wantTip     uint32
		wantOutcome string
	}{
		{"未记录原因", 0, constants.ErrBattleNotWatchable, "closed"},
		{"无可观战", constants.ErrNoWatchableBattle, constants.ErrNoWatchableBattle, "no_battle"},
		{"被更新的请求替代", constants.ErrAlreadyWatching, constants.ErrAlreadyWatching, "already_watching"},
		{"该战斗无法观战", constants.ErrBattleNotWatchable, constants.ErrBattleNotWatchable, "not_found"},
		{"满员", constants.ErrSpectateFull, constants.ErrSpectateFull, "full"},
		{"会话已断开", constants.ErrSpectateOffline, constants.ErrSpectateOffline, "offline"},
		// 本服务从不把「匹配中」写成关闭原因，借它代表滚动升级中新版本写入、本版本不认识的原因。
		{"未知原因照原码回", constants.ErrSpectateWhileQueued, constants.ErrSpectateWhileQueued, "rejected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			verdict := closedWatchVerdict(c.closeTip)
			require.Equal(t, c.wantTip, verdict.tip)
			require.Equal(t, c.wantOutcome, verdict.outcome)
			require.False(t, verdict.fault, "关闭是终局业务结论,不是故障")
			require.Equal(t, verdict, closedWatchVerdict(c.closeTip), "同一原因必须得到同一结论")
		})
	}
}

// 首答由错误判定、重放由意图上持久化的原因判定,两者必须逐字相同(tip / outcome / 文案 / 故障标记)。
// 被替代的旧请求:旧 handler 续做拿到 ErrSpectatorSuperseded,SpectatorIntent 已给它记下 ErrAlreadyWatching;
// 成局已变:prepare 以 ErrBattleNotWatchable 关闭未准备意图。
func TestGlobalWatchFirstAnswerMatchesPersistedReason(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		closeTip uint32
	}{
		{"被更新的请求替代", authority.ErrSpectatorSuperseded, constants.ErrAlreadyWatching},
		{"再包一层的替代", fmt.Errorf("推进原件: %w", authority.ErrSpectatorSuperseded), constants.ErrAlreadyWatching},
		{"成局已变", authority.ErrSpectatorFormationClosed, constants.ErrBattleNotWatchable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first := globalWatchErrVerdict(c.err)
			require.Equal(t, closedWatchVerdict(c.closeTip), first, "首答与同 request_id 重放必须同结论")
			require.False(t, first.fault)
		})
	}
}

// 参数错在任何 SQL 之前拒绝:Store 没有连接池,一旦读库就会 panic;出口仍计 internal。
func TestGlobalWatchRejectsMalformedRequestBeforeSQL(t *testing.T) {
	s := &svc.ServiceContext{Authority: &authority.Store{}}
	l := NewWatchBattleLogic(context.Background(), s)
	requests := []*pb.WatchBattleRequest{
		{RequestId: ""},
		{RequestId: "bad"},
		{RequestId: uuid.Nil.String()},
		{RequestId: "fF000000-0000-0000-0000-000000000001"},
		{RequestId: uuid.NewString(), PlayerId: 3}, // 与会话身份不符
	}
	for _, request := range requests {
		before := metrics.WatchBattleValue("internal")
		resp, err := globalWatchBattle(l, request, 2)
		require.NoError(t, err)
		require.Equal(t, constants.ErrInternal, resp.GetErrorMessage().GetId())
		require.Empty(t, resp.GetSpectatorOperationId(), "未通过校验的请求不回显操作编号")
		require.Equal(t, before+1, metrics.WatchBattleValue("internal"))
	}
}

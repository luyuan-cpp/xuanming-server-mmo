package accountdeletion

import (
	"context"
	"errors"

	dspb "proto/data_service"
	"shared/accountdirectory"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

// 执行者(docs/design/account-deletion.md §4.5):每个 login 副本一个后台循环,每轮向 data_service 认领到期账号(冷静期满,
// 或租约过期的执行中),对每个账号:吊销令牌 → 读角色 → 逐名踢线并走现有的删角(RemoveAccountRoles,两阶段、可重放)
// → 角色全删完让目录收尾(删账号行、写回执)。哪一步没成就留到下一轮,不补偿:认领之后状态已不可撤销,只会前滚。
//
// 删角回挂起(ErrConflict)多半是在途的资产指令、组队、观战,会自己结束;一直结束不了(冷静期里被转成帮主等)由
// data_service 的逾期 gauge 告警,运维按日志里的角色号处理。

// ClaimBatch 是一轮认领的账号数。
const ClaimBatch = 8

// executorTotal 按结局统计执行者处理的账号:finished(账号行已删)、waiting(角色或 ref 还没清完,下一轮再来)、error。
var executorTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "login",
	Subsystem: "account_deletion",
	Name:      "executor_total",
	Help:      "Accounts handled by the deletion executor by result: finished, waiting (roles or refs left, retried next round), error.",
	Labels:    []string{"result"},
})

// RoundResult 是一轮的计数,只给日志用。
type RoundResult struct {
	Claimed, Finished, Waiting, Failed int
}

// RunOnce 认领一批并逐个处理。认领失败回错误(这一轮什么都没做)。
func (s *Service) RunOnce(ctx context.Context) (RoundResult, error) {
	accounts, err := s.Dir.ClaimDeletions(ctx, ClaimBatch)
	if err != nil {
		return RoundResult{}, err
	}
	out := RoundResult{Claimed: len(accounts)}
	for _, account := range accounts {
		if ctx.Err() != nil {
			break
		}
		switch s.execute(ctx, account) {
		case stepFinished:
			out.Finished++
			executorTotal.Inc("finished")
		case stepWaiting:
			out.Waiting++
			executorTotal.Inc("waiting")
		default:
			out.Failed++
			executorTotal.Inc("error")
		}
	}
	return out, nil
}

type stepResult uint8

const (
	stepFinished stepResult = iota + 1
	stepWaiting
	stepFailed
)

func (s *Service) execute(ctx context.Context, account string) stepResult {
	masked := MaskAccount(account)
	if err := s.Sessions.RevokeAll(ctx, account); err != nil {
		logx.WithContext(ctx).Errorf("[account-deletion] executor revoke tokens failed: account=%s err=%v", masked, err)
	}
	dir, err := s.Dir.ReadWithLifecycle(ctx, account, false)
	if errors.Is(err, accountdirectory.ErrMissing) {
		return stepFinished // 别的副本已经收尾
	}
	if err != nil {
		logx.WithContext(ctx).Errorf("[account-deletion] executor read roles failed: account=%s err=%v", masked, err)
		return stepFailed
	}
	if dir.GetLifecycle().GetDeletionState() != dspb.AccountDeletionState_ACCOUNT_DELETION_STATE_DELETING {
		// 认领把它改成了执行中;读到别的状态说明数据不对,不删。
		logx.WithContext(ctx).Errorf("[account-deletion] executor found a claimed account not in DELETING: account=%s state=%s", masked, dir.GetLifecycle().GetDeletionState())
		return stepFailed
	}
	pending := false
	for _, role := range dir.GetPlayers().GetPlayers() {
		if !s.removeRole(ctx, account, role.GetPlayerId()) {
			pending = true
		}
	}
	if pending {
		return stepWaiting
	}
	fin, err := s.Dir.FinishDeletion(ctx, account)
	if err != nil {
		logx.WithContext(ctx).Errorf("[account-deletion] executor finish failed: account=%s err=%v", masked, err)
		return stepFailed
	}
	if !fin.GetDone() {
		logx.WithContext(ctx).Infof("[account-deletion] executor waiting for %d refs to settle: account=%s", fin.GetRemainingRefs(), masked)
		return stepWaiting
	}
	logx.WithContext(ctx).Infof("[account-deletion] account deleted: account=%s", masked)
	return stepFinished
}

// removeRole 踢线并删一名角色,回这名角色是否已经删掉。
func (s *Service) removeRole(ctx context.Context, account string, player uint64) bool {
	if err := s.Sessions.KickPlayer(ctx, player); err != nil {
		logx.WithContext(ctx).Infof("[account-deletion] executor kick failed: player=%d err=%v", player, err)
	}
	owned, err := s.Dir.Lookup(ctx, player)
	if errors.Is(err, accountdirectory.ErrMissing) {
		return true // 已经是墓碑
	}
	if err != nil {
		logx.WithContext(ctx).Infof("[account-deletion] executor lookup failed: player=%d err=%v", player, err)
		return false
	}
	if owned.GetAccount() != account {
		// 过户给了别人(冷静期前已在途的寄售成交),不再是这个账号的角色;目录收尾时它的 ref 会被判陈旧清掉。
		return true
	}
	err = s.Dir.Remove(ctx, account, []*dspb.AccountRoleVersion{{PlayerId: player, OwnerRevision: owned.GetOwnerRevision()}})
	switch {
	case err == nil:
		return true
	case errors.Is(err, accountdirectory.ErrConflict):
		logx.WithContext(ctx).Infof("[account-deletion] role still has unfinished business, retried next round: player=%d", player)
	default:
		logx.WithContext(ctx).Infof("[account-deletion] role removal not finished, retried next round: player=%d err=%v", player, err)
	}
	return false
}

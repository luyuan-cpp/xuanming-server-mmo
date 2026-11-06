package logic

import (
	"context"
	"errors"
	"time"

	"guild/internal/data"
	battlepb "proto/battle"
	pb "proto/guild"
	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
)

type trialLaunchStore interface {
	TrialLaunchEnabled() bool
	PrepareTrialLaunch(context.Context, uint64, data.TrialBattleKey, uint32, []*pb.GuildTrialRoleIdentity, uint64) (*pb.GuildTrialLaunchOriginal, error)
	ExpediteTrialLaunchCheck(context.Context, uint64) error
	ClaimDueTrialLaunches(context.Context, int) (data.DueTrialLaunches, error)
	FinishCancelledTrialLaunch(context.Context, *pb.GuildTrialLaunchOriginal, *matchpb.CancelActivityBattleResponse, uint64) error
}

// 生产仓储必须满足 trialLaunchStore:调用方按类型断言取它,断言失败会静默关掉开战原件、恢复器和终局核验,
// 签名漂移必须在编译期暴露。
var _ trialLaunchStore = (*data.ActivityRepo)(nil)

func prepareOriginalTrialLaunch(ctx context.Context, d *ActivityDeps, lobby *data.TrialLobby, battle *data.TrialBattleKey, dungeon uint32, nowMs uint64) error {
	store, ok := d.trials.(trialLaunchStore)
	if !ok || !store.TrialLaunchEnabled() {
		return nil
	}
	roster, err := d.Lobby.LoadConsents(ctx, lobby)
	if err != nil {
		return err
	}
	original, err := store.PrepareTrialLaunch(ctx, lobby.LobbyID, *battle, dungeon, roster, nowMs)
	if err != nil {
		return err
	}
	if original == nil {
		return errors.New("历练原身份合同未提交")
	}
	battle.LaunchRequestID = original.RequestId
	return nil
}

// expediteTrialLaunchCheck 在 match 已确定地没有开成这一局时(startTrialAtMatch 判定),把原件的首次检查提前到现在
// (data.ActivityRepo.ExpediteTrialLaunchCheck),恢复器下一轮就重放取消、释放占用,不必等开局窗口之后的宽限
// (2026-11-05 修复,r2-guild#1 复审)。requestID 为 0 = 没有永久原件,无事可做。
// 只是调度提示,不参与释放判定:失败只打日志,原件照旧在宽限之后被检查。用独立预算、不继承请求的取消 ——
// 走到这里时请求预算可能已经见底(预算不足没调 match 正是其中一种)。
func expediteTrialLaunchCheck(ctx context.Context, d *ActivityDeps, requestID uint64) {
	if requestID == 0 {
		return
	}
	store, ok := d.trials.(trialLaunchStore)
	if !ok || !store.TrialLaunchEnabled() {
		return
	}
	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), trialAfterLaunchBudget)
	defer cancel()
	if err := store.ExpediteTrialLaunchCheck(postCtx, requestID); err != nil {
		logx.Errorf("[GuildTrial] expedite recovery check of launch original %d failed, it will be checked after the launch grace instead: %v", requestID, err)
	}
}

const (
	// trialLaunchClaimPage:恢复器一次最多认领多少条到期原件(还受剩余预算截断,见 trialLaunchChecksWithin);
	// 一轮预算够就接着认领下一页。
	trialLaunchClaimPage = 20
	// trialLaunchRoundBudget:一轮恢复的总预算;trialLaunchCallBudget:一条原件(取消 RPC + 释放或登记)的上限。
	trialLaunchRoundBudget = 5 * time.Second
	trialLaunchCallBudget  = 2 * time.Second
)

// trialLaunchChecksWithin 是剩余 remaining 的预算里还能完整查几条原件:每条按上限 trialLaunchCallBudget 算,封顶一页。
// 不够一条就是 0。拆成纯函数只为可测(不依赖墙钟)。
func trialLaunchChecksWithin(remaining time.Duration) int {
	if remaining < trialLaunchCallBudget {
		return 0
	}
	return int(min(remaining/trialLaunchCallBudget, trialLaunchClaimPage))
}

// trialLaunchChecksLeft 是 ctx 剩余的预算里还能完整查几条原件(trialLaunchChecksWithin);ctx 已结束或没有截止时为 0。
func trialLaunchChecksLeft(ctx context.Context) int {
	deadline, ok := ctx.Deadline()
	if !ok || ctx.Err() != nil {
		return 0
	}
	return trialLaunchChecksWithin(time.Until(deadline))
}

// newTrialLaunchRecovery 是恢复器的生产装配(StartTrialBackground 用):释放之后经 notifyTrialRoster 给名单推
// ACTIVITY_CHANGED 并 bump GUILD_ACTIVITY(与 launchTrial 的结尾一致),否则名单上的人停在 LAUNCHED,要自己刷新才知道
// 这一局已作废。
func newTrialLaunchRecovery(l *GuildLogic) *trialLaunchRecovery {
	return &trialLaunchRecovery{d: l.activities, notify: func(lobby *data.TrialLobby) { l.notifyTrialRoster(lobby, 0) }}
}

// trialLaunchRecovery 是历练 launch 恢复器(06 §6.21「2026-11-05 修复」)。每个副本都跑;哪一条由哪个副本查、什么时候查,
// 由仓储的到期认领决定(data.ClaimDueTrialLaunches:SQL 时钟的检查时刻 + 单行 CAS),不靠本地游标或单实例内存状态。
// 释放只凭权威终态回执;重放同一取消是幂等的。只被它自己的那一个 goroutine 访问,不需要同步。
type trialLaunchRecovery struct {
	d *ActivityDeps
	// notify 在一份原件释放之后给名单推 ACTIVITY_CHANGED(GuildLogic.notifyTrialRoster);nil = 不推(单测)。
	notify func(*data.TrialLobby)
}

// round 跑一轮:按预算一页页认领到期原件,逐条重放签名取消。返回非 nil = 有原件的取消或释放结局未知(调用方据此 Hold 排空证明),
// 也只有这一种情形返回非 nil —— Hold 是粘性的,不能拿「什么都没发出」的失败去卡住本副本的排空证明:
//   - 解不开的行只打 ERROR、计数:它不是本副本在途的动作,不能让一行坏数据永远挡住所有副本的排空证明;
//   - 认领本身失败(到期扫描或 CAS 出错,通常是 TiDB 抖动):没认领到的原件本副本没向 match 发出任何东西,只打 ERROR、
//     计 claim_error,本轮结束;出错之前已认领的部分照常查(2026-11-05 修复,r2-guild#1 复审);
//   - 每一页只认领剩余预算查得完的条数,每条开查之前再核一次:剩余预算不够一条检查的上限(trialLaunchCallBudget)就结束本轮,
//     不让最后一条的取消 RPC 或释放事务被轮预算截断成「结局未知」。因认领本身耗时而认领了却没查的原件什么都没发出,
//     等各自的下一次检查时刻(多计了一次认领,下一次间隔长一档)。
func (r *trialLaunchRecovery) round(ctx context.Context) (resultErr error) {
	store, ok := r.d.trials.(trialLaunchStore)
	if !ok || !store.TrialLaunchEnabled() || r.d.Cancellation == nil {
		return nil
	}
	budget, cancel := context.WithTimeout(ctx, trialLaunchRoundBudget)
	defer cancel()
	for {
		page := trialLaunchChecksLeft(budget)
		if page == 0 {
			return resultErr
		}
		due, err := store.ClaimDueTrialLaunches(budget, page)
		for _, id := range due.Undecodable {
			guildTrialLaunchRecoveryTotal.Inc(trialLaunchUndecodable)
			logx.Errorf("[GuildTrial] launch original %d is undecodable, skipped and kept for manual repair; its occupancy stays held", id)
		}
		for _, original := range due.Originals {
			if trialLaunchChecksLeft(budget) == 0 {
				return resultErr
			}
			// 轮预算只决定开不开查这一条;开查之后这一条用自己的上限(从 ctx 派生),不会被轮预算截掉一截。
			resultErr = errors.Join(resultErr, r.checkOne(ctx, store, original))
		}
		if err != nil {
			guildTrialLaunchRecoveryTotal.Inc(trialLaunchClaimFailed)
			logx.Errorf("[GuildTrial] claim due launch originals failed, nothing was sent for unclaimed ones, round ends: %v", err)
			return resultErr
		}
		if !due.More {
			return resultErr
		}
	}
}

// checkOne 查一条已认领的原件并计数、打日志、推送。
func (r *trialLaunchRecovery) checkOne(ctx context.Context, store trialLaunchStore, original *pb.GuildTrialLaunchOriginal) error {
	call, cancel := context.WithTimeout(ctx, trialLaunchCallBudget)
	defer cancel()
	released, err := r.recoverOne(call, store, original)
	switch {
	case err != nil:
		guildTrialLaunchRecoveryTotal.Inc(trialLaunchFailed)
		logx.Errorf("[GuildTrial] launch original %d recovery failed, will retry at its next check: %v", original.RequestId, err)
		return err
	case released:
		guildTrialLaunchRecoveryTotal.Inc(trialLaunchReleased)
		logx.Infof("[GuildTrial] launch original %d of guild %d released on an authoritative match cancel receipt", original.RequestId, original.GuildId)
		if r.notify != nil {
			r.notify(trialLaunchLobby(original))
		}
	default:
		guildTrialLaunchRecoveryTotal.Inc(trialLaunchWaiting)
	}
	return nil
}

// trialLaunchLobby 按永久原件还原开战名单:match 要求发起人在首位,其余按原件升序 ——
// 重放的原请求字节不受重启或当前 owner 变化影响,推送也照这份名单发。
func trialLaunchLobby(original *pb.GuildTrialLaunchOriginal) *data.TrialLobby {
	roster := []uint64{original.InitiatorPlayerId}
	for _, p := range original.Roster {
		if p.PlayerId != original.InitiatorPlayerId {
			roster = append(roster, p.PlayerId)
		}
	}
	return &data.TrialLobby{LobbyID: original.RequestId, GuildID: original.GuildId, InitiatorID: original.InitiatorPlayerId, ActivityID: original.ActivityId, Roster: roster}
}

// recoverOne 重放一条原件的签名取消。released=true = 本次拿到权威终态并已提交释放。
func (r *trialLaunchRecovery) recoverOne(ctx context.Context, store trialLaunchStore, original *pb.GuildTrialLaunchOriginal) (released bool, err error) {
	battle := data.TrialBattleKey{GuildID: original.GuildId, ActivityID: original.ActivityId, PeriodKey: original.PeriodKey, GuildPeriodKey: original.GuildPeriodKey, InitiatorPlayerID: original.InitiatorPlayerId, LaunchRequestID: original.RequestId}
	lobby := trialLaunchLobby(original)
	in := &matchpb.StartActivityBattleRequest{RequestId: original.RequestId, BattleConfigId: original.DungeonId, MemberPlayerIds: lobby.Roster, ActivityContext: &battlepb.BattleActivityContext{Kind: battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL, GuildId: battle.GuildID, ActivityId: battle.ActivityID, PeriodKey: battle.PeriodKey, GuildPeriodKey: battle.GuildPeriodKey, InitiatorPlayerId: battle.InitiatorPlayerID, LaunchRequestId: battle.LaunchRequestID}}
	receipt, err := r.d.Cancellation.CancelOriginal(ctx, in)
	if err != nil {
		return false, err
	}
	// 释放只认权威终态(data.TrialCancelReceiptIsTerminal)。开局成功(formation=4)后战斗不出结果(battle 停机作废、崩溃、
	// 活动结果没发出)的局，match 在权威截止后原子提交 SQL ABORT 并回 formation=5；截止前或 RPC 失败一律保留占用，
	// 下一次检查重放同一取消(2026-10-23 修复，审计 r2-guild#1)。
	if data.TrialCancelReceiptIsTerminal(receipt) {
		if err := store.FinishCancelledTrialLaunch(ctx, original, receipt, uint64(r.d.Now().UnixMilli())); err != nil {
			return false, err
		}
		return true, nil
	}
	if receipt.BattleId == 0 {
		return false, errors.New("原match开战请求尚无终局证明，保留原件")
	}
	if receipt.FormationState == 5 {
		return false, errors.New("取消回执自相矛盾(formation=5 却是 COMPLETE)，保留原件等结果事件")
	}
	battle.BattleID = receipt.BattleId
	err = r.d.trials.RegisterTrialBattle(ctx, battle, original.CreatedMs)
	if err == nil && r.d.Lobby != nil && r.d.Lobby.SQLAuthorityEnabled() {
		_, err = r.d.Lobby.Finish(ctx, original.RequestId, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHING, pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHED, receipt.BattleId, 0, nil)
	}
	if errors.Is(err, data.ErrGuildGone) {
		// 帮会已解散、这一局还没终态(2026-11-05 修复,r2-guild#1 评审):没有帮会就没有可登记的对局行,大厅也不必再推进到
		// LAUNCHED(与 recordTrialLaunched 同一口径)。这不是故障:占用只等权威终态 —— 结算 / 标毒的帮会已解散分支,
		// 或截止后的 SQL ABORT 回执 —— 由之后的检查释放。按错误返回会让每次检查都 Hold 排空证明,拖住所有帮会。
		return false, nil
	}
	return false, err
}

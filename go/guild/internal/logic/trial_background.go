package logic

// 同道历练的两个后台循环(设计 docs/design/guild-phase2/06-activities.md §6.32 / §6.33):
//
//	待入队物品循环  结算时某位得奖者的未决指令已满(16 条),他的物品先记进待入队表;本循环等窗口有空位,
//	               把它转成一行资产指令(同一事务里删掉待入队行),之后由资产重投循环投递。**绝不跳过**。
//	巡检器          兜住"对局结果事件丢了":对登记已久仍无结果的对局,去读 battle 落在 SharedRedis 的结果记录直接结算;
//	               连记录都没有且登记更久的,判为 EXPIRED(迟到的结果仍可结算)。
//
// 三条纪律:
//
//  1. **每个副本都跑,靠幂等而不是靠互斥保证正确**。待入队转换在仓储里按玩家的 seq 行串行,后到的副本看到行已不在
//     就整体回滚;巡检的结算与判过期各自在事务里按对局行的状态复核。巡检器另有一把 Redis 租约,只为省掉重复劳动,
//     拿不到租约的后果只是本轮不扫,租约本身不承担任何正确性。
//  2. **单轮 panic 只丢那一轮**(safego.Run),循环照常进入下一轮;ctx 取消后不再开始新的一轮,正在进行的一轮在
//     下一个检查点(每处理一行之前)退出。巡检器从结果记录结算某一局时再兜一层:**那一局 panic 只丢那一局**,
//     同一轮里排在它后面的候选照常处理 —— 候选按登记时刻升序,出问题的局每轮都排在同一个位置,让它中断整轮
//     就等于后面的局永远得不到兜底、也永远判不了过期。
//  3. **时间可注入**:轮内的"现在"一律取 ActivityDeps.Now,两轮之间的节拍才用真实定时器。单测直接调单轮函数
//     (owedRewardLoop.round / trialSweepRound),不依赖墙钟。
//
// 这两个循环**不看历练此刻开没开**(ActivityDeps.trialAvailable):关掉历练之后,关之前留下的待入队物品与
// 在途对局仍要处理完。

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	"guild/internal/data"
	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	"shared/safego"
)

// safego 点位名:直接成为 safego_panic_total 的 label,必须是常量。
const (
	trialOwedLoopPoint = "guild.trial_owed_loop"
	trialSweeperPoint  = "guild.trial_sweeper"
)

const (
	// owedPageSize:待入队循环一页读多少行(06 §6.32 的 50)。
	owedPageSize = 50
	// owedMaxRowsPerRound:一轮最多看多少行。积压很大时把单轮的工作量封顶;没看完的下一轮从游标处接着看
	// (见 owedRewardLoop.cursor),所以封顶不会让排在后面的人饿死。
	owedMaxRowsPerRound = 500
	// trialSweepBatch:巡检一轮最多看多少条超时对局。06 §6.33 原写 50;取大是因为候选按登记时刻升序,
	// 排在最前面的往往是"gather 失败、永远等不来结果"的局(要等到判过期才离开候选),批量太小会把后面
	// 有结果记录可救的局挡住。每条候选只是一次 Redis GET。
	trialSweepBatch = 200
	// trialSweepLeaseMargin:巡检租约比巡检间隔短多少。租约不主动释放,必须在下一轮开始前自然到期,
	// 否则持有者自己下一轮也抢不到。
	trialSweepLeaseMargin = 5 * time.Second
)

// TrialBackgroundConf 是两个后台循环的节律,由 guild.go 从 config.ActivityConf 的四个方法换算后给出。
type TrialBackgroundConf struct {
	// OwedInterval:待入队物品循环两轮的间隔(ActivityConf.OwedLoopInterval)。
	OwedInterval time.Duration
	// SweepInterval:巡检两轮的间隔(ActivityConf.TrialSweepInterval)。
	SweepInterval time.Duration
	// ResultOverdue:对局登记多久之后仍是 STARTED 就进入巡检候选(ActivityConf.TrialResultOverdue)。
	ResultOverdue time.Duration
	// Abandon:候选对局没有结果记录、且登记超过这么久,判 EXPIRED(ActivityConf.TrialAbandon);必须大于 ResultOverdue。
	Abandon time.Duration
	// Owner:巡检租约的持有者标识,每个进程实例唯一(如节点 uuid)。只用于排障时看是谁持有租约。
	Owner string
}

func (c TrialBackgroundConf) validate() error {
	switch {
	case c.OwedInterval <= 0 || c.SweepInterval <= 0:
		return fmt.Errorf("guild trial background: intervals must be positive (owed=%v sweep=%v)", c.OwedInterval, c.SweepInterval)
	case c.ResultOverdue <= 0 || c.Abandon <= c.ResultOverdue:
		return fmt.Errorf("guild trial background: need 0 < ResultOverdue (%v) < Abandon (%v)", c.ResultOverdue, c.Abandon)
	case c.Owner == "":
		return errors.New("guild trial background: sweep lease owner must be non-empty")
	}
	return nil
}

// StartTrialBackground 起待入队物品循环与巡检器(goroutine guild.trial_owed_loop / guild.trial_sweeper),
// 返回的 stop 取消两者并**等它们退出**:循环里的语句跑在 MySQL / Redis 上,必须在关库之前停下
// (与 guild.go 的 startAssetOpCleanup 同一契约,stop 要排在 svcCtx.Stop 之前执行)。
//
// 前置:活动依赖已装配(WithActivities,Repo 非 nil)。Results 为 nil 时巡检器每轮空转并打日志,不判任何对局过期
// (读不到记录不等于没有记录);Lobby 为 nil 时巡检不抢租约、每个副本都扫(幂等,只是多做几次)。
// 首轮在 [0, 间隔) 内随机延迟,把多副本错开。
func (l *GuildLogic) StartTrialBackground(c TrialBackgroundConf) (stop func(), err error) {
	d := l.activities
	if d == nil || d.trials == nil {
		return nil, errors.New("guild trial background: activity deps not wired (logic.WithActivities missing or Repo nil)")
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	owed := &owedRewardLoop{d: d}
	var wg sync.WaitGroup
	wg.Add(2)
	safego.Go(trialOwedLoopPoint, func() {
		// Done 放在 defer 里:safego 兜住 panic 时它照样执行,stop 不会永远等下去。
		defer wg.Done()
		runTrialLoop(ctx, trialOwedLoopPoint, c.OwedInterval, func(ctx context.Context) { owed.round(ctx) })
	})
	safego.Go(trialSweeperPoint, func() {
		defer wg.Done()
		runTrialLoop(ctx, trialSweeperPoint, c.SweepInterval, func(ctx context.Context) { l.trialSweepRound(ctx, d, c) })
	})
	logx.Infof("[GuildTrial] 后台循环已启动: goroutine %s interval=%v;%s interval=%v overdue=%v abandon=%v lease_owner=%s",
		trialOwedLoopPoint, c.OwedInterval, trialSweeperPoint, c.SweepInterval, c.ResultOverdue, c.Abandon, c.Owner)
	return func() {
		cancel()
		wg.Wait()
	}, nil
}

// runTrialLoop 阻塞运行一个定时循环,ctx 取消即返回。每一轮各自 recover(纪律 2)。interval 必须为正。
func runTrialLoop(ctx context.Context, point string, interval time.Duration, round func(ctx context.Context)) {
	timer := time.NewTimer(time.Duration(rand.Int64N(int64(interval))))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		safego.Run(point, func() { round(ctx) })
		timer.Reset(interval)
	}
}

// ── 待入队物品循环(06 §6.32)──────────────────────────────────

// owedRoundStats 是一轮待入队循环的小结(日志与单测用)。
type owedRoundStats struct {
	// scanned:本轮看过的行数(含因"该玩家本轮已判仍满"而没有尝试转换的行)。
	scanned int
	// converted / stillFull / gone / failed:逐行尝试转换的四种去向。
	converted, stillFull, gone, failed int
	// skipped:资产通道关闭或发号器不可用,本轮没有尝试任何转换。
	skipped bool
}

// owedRewardLoop 是待入队物品循环的状态。只被它自己的那一个 goroutine 访问,不需要同步。
type owedRewardLoop struct {
	d *ActivityDeps
	// cursor:上一轮看到哪了。一轮看不完时下一轮从这里接着看,读到表尾回到表头 —— 整张表被轮转着看,
	// 所以"队头恰好是一批背包长期满着的玩家"不会饿死排在后面的人。多副本各有各的游标,互不影响正确性。
	cursor data.OwedRewardCursor
}

// round 跑一轮:从游标处翻页读待入队行,逐行尝试转成资产指令。返回本轮小结。
//
//   - 资产通道关闭(Loop 为 nil)或发号器未接线:本轮不转换(转了也没人投递 / 发不出号),行原样留着;
//   - 发号失败:本轮到此为止(号段故障不会因为换一行就好),游标停在这一行之前,下一轮从它重试;
//   - 某位玩家本轮已判"未决仍满":他名下的其余行本轮不再尝试(同一个窗口,结论相同,省掉注定回滚的事务);
//   - 单行转换出错:记一次失败、看下一行,这一行等游标转回来再试。
//
// 每轮结束上报积压行数(数到上限为止)。
func (o *owedRewardLoop) round(ctx context.Context) (stats owedRoundStats) {
	d := o.d
	defer o.reportBacklog(ctx)
	if d.Loop == nil || d.OpIDs == nil {
		stats.skipped = true
		guildTrialOwedTotal.Inc(trialOwedSkipped)
		return stats
	}

	full := make(map[uint64]struct{})
	for stats.scanned < owedMaxRowsPerRound {
		if ctx.Err() != nil {
			return stats
		}
		page, err := d.trials.ListOwedRewards(ctx, o.cursor, owedPageSize)
		if err != nil {
			if ctx.Err() == nil {
				logx.Errorf("[GuildTrial] list owed rewards failed, round ends: %v", err)
				guildTrialOwedTotal.Inc(trialOwedError)
			}
			return stats
		}
		for _, owed := range page {
			if ctx.Err() != nil {
				return stats
			}
			stats.scanned++
			if _, isFull := full[owed.PlayerID]; !isFull {
				if !o.convert(ctx, owed, full, &stats) {
					return stats
				}
			}
			o.cursor = owed.Cursor()
		}
		if len(page) < owedPageSize {
			// 读到表尾:下一轮从头看起。
			o.cursor = data.OwedRewardCursor{}
			return stats
		}
	}
	return stats
}

// convert 尝试把一行待入队物品转成资产指令。返回 false = 本轮应到此为止(发号失败)。
func (o *owedRewardLoop) convert(ctx context.Context, owed data.OwedReward, full map[uint64]struct{}, stats *owedRoundStats) bool {
	d := o.d
	opID, err := d.OpIDs.Mint(ctx)
	if err != nil || opID == 0 {
		if ctx.Err() == nil {
			logx.Errorf("[GuildTrial] mint op id for owed reward failed, round ends (battle %d): id=%d err=%v", owed.BattleID, opID, err)
			guildTrialOwedTotal.Inc(trialOwedError)
		}
		return false
	}
	status, err := d.trials.ConvertOwedReward(ctx, owed, opID, uint64(d.Now().UnixMilli()))
	switch {
	case err != nil:
		stats.failed++
		if ctx.Err() == nil {
			logx.Errorf("[GuildTrial] convert owed reward failed, will retry on a later round (battle %d activity %d): %v",
				owed.BattleID, owed.ActivityID, err)
			guildTrialOwedTotal.Inc(trialOwedError)
		}
	case status == data.OwedConverted:
		stats.converted++
		guildTrialOwedTotal.Inc(trialOwedConverted)
	case status == data.OwedStillFull:
		stats.stillFull++
		full[owed.PlayerID] = struct{}{}
		guildTrialOwedTotal.Inc(trialOwedStillFull)
	case status == data.OwedGone:
		stats.gone++
		guildTrialOwedTotal.Inc(trialOwedGone)
	default:
		stats.failed++
		logx.Errorf("[GuildTrial] convert owed reward returned unknown status %d (battle %d)", status, owed.BattleID)
		guildTrialOwedTotal.Inc(trialOwedError)
	}
	return true
}

// reportBacklog 上报待入队表的积压行数。读失败只记日志,gauge 保持上一轮的值。
func (o *owedRewardLoop) reportBacklog(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	n, err := o.d.trials.CountOwedRewards(ctx, trialOwedRowsGaugeCap)
	if err != nil {
		if ctx.Err() == nil {
			logx.Errorf("[GuildTrial] count owed rewards failed, backlog gauge not updated: %v", err)
		}
		return
	}
	guildTrialOwedRows.Set(float64(n))
}

// ── 巡检器(06 §6.33)─────────────────────────────────────────

// trialSweepStats 是一轮巡检的小结(日志与单测用)。
type trialSweepStats struct {
	// ran:本轮真的扫了候选(拿到了租约,且列出候选成功)。
	ran bool
	// recovered:从结果记录取回并处理完的对局数;expired:判为 EXPIRED 的对局数;
	// overdue:结果超时、没有记录、但还没到放弃时限的对局数(= 本轮上报的 gauge)。
	recovered, expired, overdue int
}

// sweepLeaseTTL:租约 = 间隔 − trialSweepLeaseMargin,但不短于间隔的一半(间隔很短时不至于变成 0 或负数)。
func sweepLeaseTTL(interval time.Duration) time.Duration {
	return max(interval-trialSweepLeaseMargin, interval/2)
}

// trialSweepRound 跑一轮巡检。逐条看"登记早于 now − ResultOverdue 且仍为 STARTED"的对局:
//   - battle 的结果记录还在 → 按它结算(SettleTrialResult,与消费者同一条路径、同一个幂等闸门);
//   - 没有记录、且登记早于 now − Abandon → 判 EXPIRED;
//   - 没有记录、还没到放弃时限 → 只计数(guild_trial_result_overdue)。
//
// 结果记录的 Redis 读出错时**本轮到此为止**:读不到不等于没有记录,不能据此把对局判过期。
// 按记录结算某一局时出错或 panic,只影响那一局(记录原样留着,下一轮再试),不挡同一轮里排在后面的候选。
// 没拿到租约的副本本轮不扫,并把自己的 overdue gauge 清 0(告警取各副本的最大值,见指标说明)。
func (l *GuildLogic) trialSweepRound(ctx context.Context, d *ActivityDeps, c TrialBackgroundConf) (stats trialSweepStats) {
	if d.Results == nil {
		logx.Error("[GuildTrial] sweeper idle: result records not wired (ActivityDeps.Results nil), overdue battles are neither recovered nor expired")
		return stats
	}
	if d.Lobby != nil {
		acquired, err := d.Lobby.AcquireSweepLease(ctx, c.Owner, sweepLeaseTTL(c.SweepInterval))
		if err != nil {
			if ctx.Err() == nil {
				logx.Errorf("[GuildTrial] acquire sweep lease failed, round skipped: %v", err)
			}
			return stats
		}
		if !acquired {
			guildTrialResultOverdue.Set(0)
			return stats
		}
	}

	nowMs := uint64(d.Now().UnixMilli())
	overdueMs, abandonMs := durationMs(c.ResultOverdue), durationMs(c.Abandon)
	if nowMs <= abandonMs {
		// 只可能出现在时钟接近纪元零点的单测里:此时无从判断"登记早于多久之前",不做任何事。
		return stats
	}
	rows, err := d.trials.ListOverdueTrialBattles(ctx, nowMs-overdueMs, trialSweepBatch)
	if err != nil {
		if ctx.Err() == nil {
			logx.Errorf("[GuildTrial] list overdue trial battles failed, round ends: %v", err)
		}
		return stats
	}
	stats.ran = true
	for _, row := range rows {
		if ctx.Err() != nil {
			return stats
		}
		payload, found, err := d.Results.Load(ctx, row.BattleID)
		if err != nil {
			if ctx.Err() == nil {
				logx.Errorf("[GuildTrial] read result record of battle %d failed, sweep round ends (nothing is expired on a failed read): %v",
					row.BattleID, err)
			}
			return stats
		}
		switch {
		case found:
			// 逐局兜 panic(文件头纪律 2):只丢这一局,下一轮再试;栈由 safego 打在日志里,
			// 计 safego_panic_total{point="guild.trial_sweeper"}(与整轮的兜底同一个点位)。
			recovered := false
			if ok := safego.Run(trialSweeperPoint, func() { recovered = l.recoverTrialResult(ctx, d, row, payload, nowMs) }); !ok {
				logx.Errorf("[GuildTrial] sweeper panicked while settling battle %d of guild %d from its result record, skipped this round and will retry next round",
					row.BattleID, row.GuildID)
			}
			if recovered {
				stats.recovered++
			}
		case row.CreatedMs < nowMs-abandonMs:
			expired, err := d.trials.ExpireTrialBattle(ctx, row.BattleID, row.GuildID, nowMs)
			switch {
			case err != nil:
				if ctx.Err() == nil {
					logx.Errorf("[GuildTrial] expire battle %d of guild %d failed, will retry next round: %v", row.BattleID, row.GuildID, err)
				}
			case expired:
				stats.expired++
				guildTrialResultTotal.Inc(trialResultSweeperExpired)
				logx.Infof("[GuildTrial] battle %d of guild %d expired: no result and no result record %v after it started",
					row.BattleID, row.GuildID, c.Abandon)
			}
		default:
			stats.overdue++
		}
	}
	guildTrialResultOverdue.Set(float64(stats.overdue))
	if stats.recovered > 0 || stats.expired > 0 || stats.overdue > 0 {
		logx.Infof("[GuildTrial] sweep round: candidates=%d recovered=%d expired=%d overdue=%d", len(rows), stats.recovered, stats.expired, stats.overdue)
	}
	return stats
}

// recoverTrialResult 用 battle 落下的结果记录处理一局超时未结算的对局。返回 true = 这条记录处理完了
// (已结算,或判定为重复 / 帮会已解散 / 毒消息等任何"有了定论"的去向)。
//
// 记录解不开、或它描述的根本不是这一局(battle_id 不符、不是帮会历练的上下文):这是确定性的坏数据,
// 把这一局标成 POISON 并销账 —— 否则它会永远停在候选里(有记录就不会被判过期)。
// 结算回暂时性错误时什么都不做,下一轮再来。
func (l *GuildLogic) recoverTrialResult(ctx context.Context, d *ActivityDeps, row data.OverdueTrialBattle, payload []byte, nowMs uint64) bool {
	key := data.TrialBattleKey{BattleID: row.BattleID, GuildID: row.GuildID}
	ev := &kafkapb.BattleResultEvent{}
	cause := ""
	if err := proto.Unmarshal(payload, ev); err != nil {
		cause = fmt.Sprintf("result record undecodable: %v", err)
	} else if ev.GetBattleId() != row.BattleID ||
		ev.GetActivityContext().GetKind() != battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL {
		cause = fmt.Sprintf("result record describes battle %d with activity kind %v, not this guild trial battle",
			ev.GetBattleId(), ev.GetActivityContext().GetKind())
	}
	if cause != "" {
		if _, err := l.poisonTrialBattle(ctx, d, key, 0, nowMs, cause); err != nil {
			if ctx.Err() == nil {
				logx.Errorf("[GuildTrial] sweeper could not mark battle %d as poison, will retry next round: %v", row.BattleID, err)
			}
			return false
		}
		return true
	}
	outcome, err := l.SettleTrialResult(ctx, ev)
	if err != nil {
		if ctx.Err() == nil {
			logx.Errorf("[GuildTrial] sweeper could not settle battle %d of guild %d from its result record, will retry next round: %v",
				row.BattleID, row.GuildID, err)
		}
		return false
	}
	guildTrialResultTotal.Inc(trialResultSweeperRecover)
	logx.Infof("[GuildTrial] sweeper recovered battle %d of guild %d from its result record (outcome %d)", row.BattleID, row.GuildID, outcome)
	return true
}

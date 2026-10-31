package logic

// 评分匹配(设计文档 cross-zone-matchmaking.md §11):
//   - 评分只存全球权威库(TiDB 的 match_rating / match_rating_applied,authority/rating.go),默认 1500;
//   - 结果回流:battle 打完的局经 Kafka match-results 进 ApplyBattleResult,只对
//     PVP 队列模式(1V1 / 5V5)做 Elo(K=32,队伍用平均分,平局 0.5;回合打满按
//     平局),整局在一个 SQL 事务里入账,幂等靠 match_rating_applied;
//   - 容差曲线 ratingTolerance:起始 100、每 5s 放宽 100、上限 1000(可配),
//     两个零历史新号(都 1500)立即互配,分差大的等一会儿就能配上;锚点等满
//     RatingToleranceMaxWaitSeconds 后容差 ∞ 兜底,不会永久饥饿;
//   - 5v5 分队 assignBalancedTeams:按评分排序后蛇形分配 0/1。

import (
	"context"
	"math"
	"sort"
	"strconv"
	"time"

	"match/internal/metrics"
	"match/internal/svc"

	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
)

// defaultRating 无历史的新号评分。
const defaultRating = 1500

// isRatedMode 报告该队列模式是否按评分凑组 / 更新评分:只有 PVP 队列模式
// (1V1 / 5V5)。PVE 组队不看评分沿用等待序(容差 ∞);切磋点名成局、PVE 单人
// 即时开战都不入队也不计分。
func isRatedMode(mode matchpb.MatchMode) bool {
	switch mode {
	case matchpb.MatchMode_MATCH_MODE_1V1, matchpb.MatchMode_MATCH_MODE_5V5:
		return true
	default:
		return false
	}
}

// formatRating 评分的日志形态:保留两位小数。
func formatRating(rating float64) string {
	return strconv.FormatFloat(rating, 'f', 2, 64)
}

// loadRating 读玩家评分;没有记录返回 defaultRating。评分只在权威库。
func loadRating(svcCtx *svc.ServiceContext, playerId uint64) (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rating, e := svcCtx.Authority.Rating(ctx, playerId)
	return float64(rating) / 100, e
}

// loadRatingOrDefault 读评分,失败只记日志回落默认值:评分在这里只是分队与点名成局记录的软约束,
// 计分池的入队另走 loadRating(读不到拒绝入队)。
func loadRatingOrDefault(svcCtx *svc.ServiceContext, playerId uint64) float64 {
	rating, err := loadRating(svcCtx, playerId)
	if err != nil {
		logx.Errorf("[match] Failed to read rating; using default %d player=%d: %v", defaultRating, playerId, err)
		return defaultRating
	}
	return rating
}

// loadRatings 批量读评分(逐人),失败者回落默认值。
func loadRatings(svcCtx *svc.ServiceContext, members []uint64) map[uint64]float64 {
	out := make(map[uint64]float64, len(members))
	for _, pid := range members {
		out[pid] = loadRatingOrDefault(svcCtx, pid)
	}
	return out
}

// ---- 容差曲线 ----

// ratingToleranceFor 返回锚点等待 waitSeconds 后允许的最大分差(§11 容差曲线):
//
//	tol = min(Max, Base + floor(wait / StepSeconds) × StepDelta)
//
// 默认 Base=100 / StepSeconds=5 / StepDelta=100 / Max=1000:0-4s 内只配 ±100,
// 45s 后放宽到 ±1000。Base 取 100 而不是 0 是为了让两个都是 1500 的新号(以及
// 几局内评分小幅分化的号)第一轮就能互配。配置漏配 / 测试直接构造 Config 时
// 用默认值,0 不是合法容差。曲线本身有上限;上限之外的终态兜底见 anchorToleranceFor。
func ratingToleranceFor(svcCtx *svc.ServiceContext, waitSeconds int64) float64 {
	base := float64(svcCtx.Config.RatingToleranceBase)
	if base <= 0 {
		base = 100
	}
	stepSeconds := svcCtx.Config.RatingToleranceStepSeconds
	if stepSeconds <= 0 {
		stepSeconds = 5
	}
	stepDelta := float64(svcCtx.Config.RatingToleranceStepDelta)
	if stepDelta <= 0 {
		stepDelta = 100
	}
	maxTol := float64(svcCtx.Config.RatingToleranceMax)
	if maxTol <= 0 {
		maxTol = 1000
	}
	if waitSeconds < 0 {
		waitSeconds = 0
	}
	tol := base + float64(waitSeconds/stepSeconds)*stepDelta
	if tol > maxTol {
		tol = maxTol
	}
	return tol
}

// ratingToleranceMaxWaitFor 返回容差曲线的终态兜底秒数(RatingToleranceMaxWaitSeconds,
// 0 / 漏配按默认 90):锚点等到这么久后容差 ∞。不支持关闭 —— 曲线上限之外没有
// 兜底就是"分差 > Max 的两人永久饥饿到票据 6h 过期"(复审问题)。
func ratingToleranceMaxWaitFor(svcCtx *svc.ServiceContext) int64 {
	if v := svcCtx.Config.RatingToleranceMaxWaitSeconds; v > 0 {
		return v
	}
	return 90
}

// anchorToleranceFor 返回撮合里锚点实际使用的容差(poolPolicy):非 rated 模式(PVE_TEAM)
// 恒 ∞;rated 模式已等 ≥ RatingToleranceMaxWaitSeconds 时也退化为 ∞(纯等待序,
// 与 PVE_TEAM 同分支),保证"最长等待 N 秒必配";否则走曲线。
func anchorToleranceFor(svcCtx *svc.ServiceContext, rated bool, waitSeconds int64) float64 {
	if !rated || waitSeconds >= ratingToleranceMaxWaitFor(svcCtx) {
		return math.Inf(1)
	}
	return ratingToleranceFor(svcCtx, waitSeconds)
}

// ---- 5v5 分队 ----

// assignBalancedTeams 5v5 按评分贪心平衡分队(§11):按评分降序排,蛇形分配
// 0/1/1/0/0/1/1/0/0/1 —— 第 1、4、5、8、9 名一队,第 2、3、6、7、10 名一队,
// 两队总分尽量接近。评分相同按弹出序(members 里的下标)稳定排序。返回与
// members 同下标的队号。
func assignBalancedTeams(members []uint64, ratings map[uint64]float64) []uint32 {
	order := make([]int, len(members))
	for i := range members {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ra, rb := ratings[members[order[a]]], ratings[members[order[b]]]
		if ra != rb {
			return ra > rb
		}
		return order[a] < order[b]
	})
	teams := make([]uint32, len(members))
	for rank, idx := range order {
		// 蛇形:每两个名次换一次方向 → 名次 1,2 / 3,4 ... 中的 (0,1),(1,0),(0,1)...
		if (rank/2)%2 == 0 {
			teams[idx] = uint32(rank % 2)
		} else {
			teams[idx] = uint32(1 - rank%2)
		}
	}
	return teams
}

// ---- Elo 结果回流 ----

// 结果入账的 outcome(指标 label match_rating_update_total{mode,outcome})。
const (
	ratingOutcomeApplied   = "applied"   // 全员已更新评分
	ratingOutcomeDuplicate = "duplicate" // 同一 battle_id 已入账(Kafka 重复投递)
	ratingOutcomeIgnored   = "ignored"   // PVE / 切磋 / 未结束 / 队伍形态异常,不计分
	ratingOutcomeError     = "error"     // 入账事务失败,调用方可重试
)

// ApplyBattleResult 消费一条对局结果并更新 Elo(§11.5)。返回 outcome 与错误:
// err 非 nil 时 outcome=error,调用方(Kafka 消费者)重试;其余情况已终态。
//
// 规则:
//   - 只对 PVP 队列模式(isRatedMode)计分,PVE / 切磋忽略;
//   - outcome 只认 SIDE_A_WIN(team 0 胜)/ SIDE_B_WIN(team 1 胜)/ DRAW(各 0.5),
//     其余忽略;必须恰好两支非空队伍;
//   - 回合打满按平局:total_rounds ≥ RatingDrawRoundCapFor(config) 的胜负结果按 0.5
//     结算(C++ 引擎回合打满一律判 SIDE_B_WIN,是 PVE"进攻方判负"规则泄漏到 PVP;
//     team 0 = 1v1 锚点 / 5v5 评分最高者所在队,照胜负算会系统性扣他们的分);
//   - Elo 与幂等在 Authority.ApplyRating 的 SQL 事务里:队伍用平均分算期望,K=32,评分下限 0。
func ApplyBattleResult(svcCtx *svc.ServiceContext, event *kafkapb.BattleResultEvent) (string, error) {
	mode := matchpb.MatchMode(event.GetMatchMode())
	modeName := mode.String()
	battleId := event.GetBattleId()
	finish := func(outcome string) string {
		metrics.ObserveRatingUpdate(modeName, outcome)
		return outcome
	}
	if !isRatedMode(mode) {
		return finish(ratingOutcomeIgnored), nil
	}
	var scoreA float64
	switch event.GetOutcome() {
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN:
		scoreA = 1
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN:
		scoreA = 0
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW:
		scoreA = 0.5
	default:
		logx.Infof("[rating] Battle result does not affect rating battle=%d mode=%s outcome=%s", battleId, modeName, event.GetOutcome())
		return finish(ratingOutcomeIgnored), nil
	}
	teamA, teamB, ok := splitResultTeams(event)
	if !ok {
		logx.Errorf("[rating] Invalid team structure in battle result; rating unchanged battle=%d mode=%s teams=%d", battleId, modeName, len(event.GetTeams()))
		return finish(ratingOutcomeIgnored), nil
	}
	if scoreA != 0.5 {
		if roundCap := svcCtx.Config.RatingDrawRoundCapFor(event.GetBattleConfigId()); roundCap > 0 && event.GetTotalRounds() >= roundCap {
			// 回合打满:引擎判的 SIDE_B_WIN 是 PVE 规则,PVP 按平局。最后一回合真打死
			// 也落在这里(total_rounds == 上限,与打满不可区分),保守按平局。
			logx.Infof("[rating] Round limit reached; settling as draw battle=%d mode=%s outcome=%s rounds=%d cap=%d config=%d",
				battleId, modeName, event.GetOutcome(), event.GetTotalRounds(), roundCap, event.GetBattleConfigId())
			metrics.ObserveRatingRoundCapDraw(modeName)
			scoreA = 0.5
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	applied, e := svcCtx.Authority.ApplyRating(ctx, battleId, event, uint32(mode), teamA, teamB, scoreA)
	if e != nil {
		return finish(ratingOutcomeError), e
	}
	if !applied {
		return finish(ratingOutcomeDuplicate), nil
	}
	return finish(ratingOutcomeApplied), nil
}

// splitResultTeams 把结果里的队伍拆成 team 0 / team 1 的成员列表:必须恰好两支
// 且都非空,team_index 只认 0 / 1(与 CreateBattle 的 TeamIndex 口径一致)。
func splitResultTeams(event *kafkapb.BattleResultEvent) (teamA, teamB []uint64, ok bool) {
	if len(event.GetTeams()) != 2 {
		return nil, nil, false
	}
	for _, team := range event.GetTeams() {
		switch team.GetTeamIndex() {
		case 0:
			teamA = append(teamA, team.GetPlayerIds()...)
		case 1:
			teamB = append(teamB, team.GetPlayerIds()...)
		default:
			return nil, nil, false
		}
	}
	if len(teamA) == 0 || len(teamB) == 0 {
		return nil, nil, false
	}
	return teamA, teamB, true
}

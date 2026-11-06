package logic

// 评分匹配(设计文档 cross-zone-matchmaking.md §11、global-match-hierarchical-pools.md §8):
//   - 评分只存全球权威库(TiDB 的 match_rating / match_rating_applied,authority/rating.go),默认 1500;
//     按 (player_id, mode) 分开存,每个计分模式一条评分轨道、一张榜。入队读分(globalJoin)、5v5 分队
//     (gather.teamAssignment)、结算(ApplyBattleResult)用的是同一局的同一个 mode:票与成局原件上的 mode
//     原样写进 CreateBattleRequest.match_mode,battle 回显成结果的 match_mode;
//   - 结果回流:battle 打完的局经 Kafka match-results 进 ApplyBattleResult,只对
//     计分模式(isRatedMode)做 Elo(K=32,队伍用平均分,平局 0.5;回合打满按
//     平局,判定见 ratedScoreA),整局在一个 SQL 事务里入账,幂等靠 match_rating_applied;
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
	"match/internal/ratingrule"
	"match/internal/svc"

	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
)

// defaultRating 无历史的新号评分。
const defaultRating = 1500

// isRatedMode 报告该队列模式是否按评分凑组 / 更新评分:1V1、5V5 与人数档真人 PVP(PVP_TEAM,
// 2026-11-05 起)。PVE 组队不看评分沿用等待序(容差 ∞);切磋点名成局、PVE 单人即时开战都不入队也不计分。
// 判定只在 ratingrule 一处,提交点的组内极差复核用同一份。
func isRatedMode(mode matchpb.MatchMode) bool {
	return ratingrule.Rated(mode)
}

// formatRating 评分的日志形态:保留两位小数。
func formatRating(rating float64) string {
	return strconv.FormatFloat(rating, 'f', 2, 64)
}

// loadRating 读玩家在计分模式 mode 下的评分;没有记录返回 defaultRating。评分只在权威库。
// mode 必须是计分模式(isRatedMode),不计分的模式没有评分轨道,authority 直接报错。
func loadRating(svcCtx *svc.ServiceContext, mode matchpb.MatchMode, playerId uint64) (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rating, e := svcCtx.Authority.Rating(ctx, playerId, uint32(mode))
	return float64(rating) / 100, e
}

// loadRatingOrDefault 读评分,失败只记日志回落默认值:评分在这里只是分队与点名成局记录的软约束,
// 计分池的入队另走 loadRating(读不到拒绝入队)。
func loadRatingOrDefault(svcCtx *svc.ServiceContext, mode matchpb.MatchMode, playerId uint64) float64 {
	rating, err := loadRating(svcCtx, mode, playerId)
	if err != nil {
		logx.Errorf("[match] Failed to read rating; using default %d player=%d mode=%s: %v", defaultRating, playerId, mode, err)
		return defaultRating
	}
	return rating
}

// loadRatings 批量读同一计分模式下的评分(逐人),失败者回落默认值。
func loadRatings(svcCtx *svc.ServiceContext, mode matchpb.MatchMode, members []uint64) map[uint64]float64 {
	out := make(map[uint64]float64, len(members))
	for _, pid := range members {
		out[pid] = loadRatingOrDefault(svcCtx, mode, pid)
	}
	return out
}

// ratingSnapshotCenti 是点名成局(切磋、活动、整队直接开战)票上记的评分快照(×100):计分模式读本模式的评分,
// 读失败回落默认分(点名成局名单固定,这份快照不参与撮合);不计分的模式没有评分轨道,记 0,不为它读库
// (与单排入队、整队排队的非计分票同口径)。
func ratingSnapshotCenti(svcCtx *svc.ServiceContext, mode matchpb.MatchMode, playerId uint64) int64 {
	if !isRatedMode(mode) {
		return 0
	}
	return int64(math.Round(loadRatingOrDefault(svcCtx, mode, playerId) * 100))
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
// 曲线只在 ratingrule.Curve 一处实现,提交点的组内极差复核用同一条。
func ratingToleranceFor(svcCtx *svc.ServiceContext, waitSeconds int64) float64 {
	return ratingrule.CurveOf(svcCtx.Config).Tolerance(waitSeconds)
}

// ratingToleranceMaxWaitFor 返回容差曲线的终态兜底秒数(RatingToleranceMaxWaitSeconds,
// 0 / 漏配按默认 90):锚点等到这么久后容差 ∞。不支持关闭 —— 曲线上限之外没有
// 兜底就是"分差 > Max 的两人永久饥饿到票据 6h 过期"(复审问题)。
func ratingToleranceMaxWaitFor(svcCtx *svc.ServiceContext) int64 {
	return ratingrule.CurveOf(svcCtx.Config).MaxWait()
}

// anchorToleranceFor 返回撮合里锚点实际使用的容差(poolPolicy):非 rated 模式(PVE_TEAM)
// 恒 ∞;rated 模式已等 ≥ RatingToleranceMaxWaitSeconds 时也退化为 ∞(纯等待序,
// 与 PVE_TEAM 同分支),保证"最长等待 N 秒必配";否则走曲线。
func anchorToleranceFor(svcCtx *svc.ServiceContext, rated bool, waitSeconds int64) float64 {
	if !rated {
		return math.Inf(1)
	}
	return ratingrule.CurveOf(svcCtx.Config).Anchor(waitSeconds)
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
//   - 只对计分模式(isRatedMode)计分,PVE / 切磋忽略;评分轨道与榜按结果里的 match_mode 选
//     (成局时写进 CreateBattleRequest 的那个 mode,新终局协议还在提交点核过它与成局原件一致);
//   - outcome 只认 SIDE_A_WIN(team 0 胜)/ SIDE_B_WIN(team 1 胜)/ DRAW(各 0.5),
//     其余忽略;必须恰好两支非空队伍;
//   - 回合打满按平局,判定见 ratedScoreA:引擎回合打满一律判 SIDE_B_WIN,是 PVE"进攻方判负"规则泄漏到 PVP
//     (team 0 = 1v1 按 battle_id 定的一侧 / 5v5 蛇形分边的一侧,照胜负算对 team 0 不公平);
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
	roundLimit := svcCtx.Catalog.RoundLimit(event.GetBattleConfigId())
	scoreA, roundCap, rated := ratedScoreA(event, roundLimit)
	if !rated {
		logx.Infof("[rating] Battle result does not affect rating battle=%d mode=%s outcome=%s", battleId, modeName, event.GetOutcome())
		return finish(ratingOutcomeIgnored), nil
	}
	teamA, teamB, ok := splitResultTeams(event)
	if !ok {
		logx.Errorf("[rating] Invalid team structure in battle result; rating unchanged battle=%d mode=%s teams=%d", battleId, modeName, len(event.GetTeams()))
		return finish(ratingOutcomeIgnored), nil
	}
	if roundCap != "" {
		logx.Infof("[rating] Round limit reached; settling as draw battle=%d mode=%s outcome=%s basis=%s rounds=%d limit=%d config=%d",
			battleId, modeName, event.GetOutcome(), roundCap, event.GetTotalRounds(), roundLimit, event.GetBattleConfigId())
		if roundCap == roundCapBySurvivors && event.GetTotalRounds() < roundLimit {
			// 0 队没全灭却判 1 队胜,回合数又没到本副本按模板推出的上限:battle 用的 Dungeon 表与本副本不是同一版
			// (滚动换表),或者引擎多了一条胜负规则。仍按原件的事实判平局,留日志查原因。
			logx.Errorf("[rating] Side B win without wiping side 0 before this replica's round limit; settled as draw battle=%d mode=%s rounds=%d limit=%d config=%d",
				battleId, modeName, event.GetTotalRounds(), roundLimit, event.GetBattleConfigId())
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
	if roundCap != "" {
		// 只在真正入账时计数:重复投递、重试不重复计。
		metrics.ObserveRatingRoundCapDraw(modeName, roundCap)
	}
	return finish(ratingOutcomeApplied), nil
}

// 回合打满改判平局的依据(指标 match_rating_round_cap_draw_total{mode,basis} 的 basis,封闭集合):
//   - roundCapBySurvivors:完整个人原件(settlement_format_version=1)证明 0 队还有能行动的单位。引擎只有回合打满
//     这一条路会在 0 队没全灭时判 1 队胜,所以这是确定的「打满」,不用回合数猜;
//   - roundCapByRoundLimit:没有个人原件的旧版结果,只能看回合数达到模板回合上限(catalog.RoundLimit,与引擎同一换算)。
const (
	roundCapBySurvivors  = "survivors"
	roundCapByRoundLimit = "round_limit"
)

// ratedScoreA 把计分局的终局换算成 0 队的 Elo 得分:1 胜、0.5 平、0 负。rated=false:这个终局不计分。
// roundCap 非空:引擎判的 1 队胜其实是回合打满,按平局计,值是判定依据。roundLimit 是本局模板的回合上限。
//
// 引擎的胜负规则对所有模式相同(TurnBattleEngine::UpdateOutcome):双方同时全灭判平;一方全灭判另一方胜;
// 都没全灭而回合数到了上限(模板 time_limit 换算,缺省 300 回合),判进攻方(0 队)负。整场期限打满时 battle 节点
// 直接盖平局章(OnBattleDeadline),结果本来就是 DRAW。所以:
//   - DRAW:0.5;
//   - SIDE_A_WIN:1。回合打满只会判 1 队胜,0 队胜一定是把 1 队打到全灭,哪怕发生在最后一回合;
//   - SIDE_B_WIN:有完整个人原件时看 0 队还有没有能行动的单位,有就是回合打满,按平局;0 队全灭才是真输,
//     最后一回合真打死也能分清。没有个人原件的旧版结果只能看回合数:达到上限按平局
//     (分不清最后一回合真打死,保守按平局),否则算输。
func ratedScoreA(event *kafkapb.BattleResultEvent, roundLimit uint32) (scoreA float64, roundCap string, rated bool) {
	switch event.GetOutcome() {
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN:
		return 1, "", true
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW:
		return 0.5, "", true
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN:
	default:
		return 0, "", false
	}
	if event.GetSettlementFormatVersion() == 1 {
		// 版本 1 的个人原件与成员一一对应、队伍一致,结果消费者在入账之前已经核过(authority.ArchiveCompletion)。
		if sideHasActiveUnit(event.GetSettlements(), 0) {
			return 0.5, roundCapBySurvivors, true
		}
		return 0, "", true
	}
	if roundLimit > 0 && event.GetTotalRounds() >= roundLimit {
		return 0.5, roundCapByRoundLimit, true
	}
	return 0, "", true
}

// sideHasActiveUnit 报告终局时 team 这一侧还有没有能行动的单位,口径同引擎的 SideWiped:玩家未阵亡且未逃跑,
// 或他出战的宝宝未阵亡(PVP 不刷怪,一侧只有玩家和宝宝)。宝宝的个人原件没有逃跑标记:PVP 逃跑必然失败
// (ExecuteFlee 只在 PVE 掷骰),宝宝不会因逃跑离场,只看阵亡。
func sideHasActiveUnit(settlements []*battlepb.BattleSettlementData, team uint32) bool {
	for _, settlement := range settlements {
		if settlement.GetPlayerTeamIndex() != team {
			continue
		}
		if !settlement.GetIsDead() && !settlement.GetFled() {
			return true
		}
		for _, pet := range settlement.GetPets() {
			if !pet.GetIsDead() {
				return true
			}
		}
	}
	return false
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

package logic

// 评分匹配(设计文档 cross-zone-matchmaking.md §11):
//   - 评分存储 match:rating:{player_id}(hash,无 TTL,默认 1500);
//   - 结果回流:battle 打完的局经 Kafka match-results 进 ApplyBattleResult,只对
//     PVP 队列模式(1V1 / 5V5)做 Elo(K=32,队伍用平均分,平局 0.5;回合打满按
//     平局)。写分是逐人**增量** Lua(并发可交换),两层幂等:每局
//     match:rating:applied:{battle_id} 标记(applying|Δ_A → done)+ 每人 hash 里的
//     recent_battles,写到一半失败由重投续写、不会算两次(§11.5);
//   - 容差曲线 ratingTolerance:起始 100、每 5s 放宽 100、上限 1000(可配),
//     两个零历史新号(都 1500)立即互配,分差大的等一会儿就能配上;锚点等满
//     RatingToleranceMaxWaitSeconds 后容差 ∞ 兜底,不会永久饥饿;
//   - 5v5 分队 assignBalancedTeams:按评分排序后蛇形分配 0/1。

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"match/internal/metrics"
	"match/internal/svc"

	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// defaultRating 无历史的新号评分。
const defaultRating = 1500

// eloK Elo 更新系数:一局最多变动 32 分。
const eloK = 32

// ratingAppliedTTLSeconds 入账标记 TTL(7 天):Kafka 重复投递不会跨这么久。
const ratingAppliedTTLSeconds = 7 * 24 * 3600

// 评分 hash 字段名。
const (
	ratingFieldRating        = "rating"
	ratingFieldGames         = "games"
	ratingFieldUpdatedAt     = "updated_at_ms"
	ratingFieldRecentBattles = "recent_battles" // 最近入账的 battle_id,逗号分隔,头插,最多 ratingRecentBattlesKeep 个
)

// ratingRecentBattlesKeep 每人 hash 里保留的最近入账 battle_id 个数:逐人幂等的
// 判定窗口。同一局的续写 / 重投最多隔几秒到几分钟,期间一个人打不完 8 局。
const ratingRecentBattlesKeep = 8

// ratingApplyScript 逐人**增量**写一局评分(KEYS=[ratingKey],
// ARGV=[delta, nowMs, battleId, defaultRating, recentKeep]),返回 {是否写入(0/1), 新评分, 累计局数}:
//   - recent_battles 已含本局 battle_id → 不写,返回 {0, 当前评分, 当前局数}(逐人幂等:
//     写到一半失败后续写,已落账的人不会再算一次);
//   - 否则 rating = max(0, 当前评分(缺省 ARGV[4]) + delta)、games+1、updated_at_ms、
//     recent_battles 头插本局并截到 ARGV[5](ratingRecentBattlesKeep)个。
//
// 增量而不是"Go 侧读旧值 + HSET 绝对值":同一玩家连续两局按 battle_id 落在不同
// 分区、被两个 match 实例并发入账时,两份增量可交换、都落账;读旧值再覆盖写会丢
// 其中一局。单 key,集群下可用;评分保留两位小数,与 formatRating 同口径。
const ratingApplyScript = `local recent = redis.call("HGET", KEYS[1], "` + ratingFieldRecentBattles + `") or ""
for id in string.gmatch(recent, "[^,]+") do
	if id == ARGV[3] then
		return {0, redis.call("HGET", KEYS[1], "` + ratingFieldRating + `") or "", redis.call("HGET", KEYS[1], "` + ratingFieldGames + `") or 0}
	end
end
local cur = tonumber(redis.call("HGET", KEYS[1], "` + ratingFieldRating + `") or "") or tonumber(ARGV[4])
local next = cur + tonumber(ARGV[1])
if next < 0 then
	next = 0
end
local rating = string.format("%.2f", next)
local kept = {ARGV[3]}
for id in string.gmatch(recent, "[^,]+") do
	if #kept >= tonumber(ARGV[5]) then
		break
	end
	kept[#kept + 1] = id
end
redis.call("HSET", KEYS[1], "` + ratingFieldRating + `", rating)
redis.call("HSET", KEYS[1], "` + ratingFieldUpdatedAt + `", ARGV[2])
redis.call("HSET", KEYS[1], "` + ratingFieldRecentBattles + `", table.concat(kept, ","))
local games = redis.call("HINCRBY", KEYS[1], "` + ratingFieldGames + `", 1)
return {1, rating, games}`

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

// formatRating 评分的存储 / ARGV 形态:保留两位小数,ZSET score 与 hash 字段同口径。
func formatRating(rating float64) string {
	return strconv.FormatFloat(rating, 'f', 2, 64)
}

// parseRating 解析存储形态的评分;空串 / 非法回落默认值。
func parseRating(raw string) float64 {
	if raw == "" {
		return defaultRating
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return defaultRating
	}
	return v
}

// loadRating 读玩家评分;没有记录返回 defaultRating。评分是 match 私有 key,走 MatchRedis。
func loadRating(svcCtx *svc.ServiceContext, playerId uint64) (float64, error) {
	raw, err := svcCtx.MatchRedis.Hget(matchRatingKey(playerId), ratingFieldRating)
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return defaultRating, nil
		}
		return defaultRating, err
	}
	return parseRating(raw), nil
}

// loadRatingOrDefault 读评分,失败只记日志回落默认值:评分是对局质量的软约束,
// 不能让 Redis 抖动把排队 / 回队首整个拒掉(票据与队列的强一致由别处保证)。
func loadRatingOrDefault(svcCtx *svc.ServiceContext, playerId uint64) float64 {
	rating, err := loadRating(svcCtx, playerId)
	if err != nil {
		logx.Errorf("[match] 读评分失败,按默认 %d 处理 player=%d: %v", defaultRating, playerId, err)
		return defaultRating
	}
	return rating
}

// loadRatings 批量读评分(逐 key;评分按玩家分布不同 slot,不能 MGET),失败者回落默认值。
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

// ratingToleranceSaturationFor 返回容差曲线到达上限所需的等待秒数
// (默认 (1000-100)/100 × 5 = 45s):锚点等过这个时间仍凑不到候选,就是曲线本身
// 救不了的饥饿(段位里没人 / 分差 > Max),popGroup 据此记限频日志。
func ratingToleranceSaturationFor(svcCtx *svc.ServiceContext) int64 {
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
	if maxTol <= base {
		return 0
	}
	return int64(math.Ceil((maxTol-base)/stepDelta)) * stepSeconds
}

// anchorToleranceFor 返回 popGroup 里锚点实际使用的容差:非 rated 模式(PVE_TEAM)
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
	ratingOutcomePartial   = "partial"   // 只落账了一部分玩家,标记留 applying,调用方重试可续写
	ratingOutcomeError     = "error"     // Redis 出错、一人未写,调用方可重试
)

// 入账标记 match:rating:applied:{battle_id} 的值:
//
//	applying|<Δ_A>  已按赛前评分算出 Δ_A,逐人写分进行中;
//	done            全员落账。
//
// 值带 Δ_A 是为了续写:写到一半失败(某玩家所在 master 抖动)后,重投再进来时
// 部分玩家的评分已经变了,重算 Δ 就不再零和;按标记里记录的 Δ 续写 + 逐人幂等
// (ratingApplyScript 的 recent_battles)才能把剩余成员补齐而无人算两次。
const (
	ratingAppliedStateApplying = "applying"
	ratingAppliedStateDone     = "done"
)

// encodeRatingAppliedMarker 组标记值。
func encodeRatingAppliedMarker(state string, deltaA float64) string {
	if state == ratingAppliedStateDone {
		return ratingAppliedStateDone
	}
	return state + "|" + strconv.FormatFloat(deltaA, 'f', -1, 64)
}

// decodeRatingAppliedMarker 解标记值;ok=false 表示不是本版本写的格式(滚动升级窗口内
// 旧实例写的是实例 uuid,旧语义"标记存在 = 已入账",调用方按 duplicate 处理)。
func decodeRatingAppliedMarker(raw string) (state string, deltaA float64, ok bool) {
	if raw == ratingAppliedStateDone {
		return ratingAppliedStateDone, 0, true
	}
	head, rest, found := strings.Cut(raw, "|")
	if !found || head != ratingAppliedStateApplying {
		return "", 0, false
	}
	v, err := strconv.ParseFloat(rest, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return "", 0, false
	}
	return ratingAppliedStateApplying, v, true
}

// 单人写分的有界重试:评分 key 按玩家分布在不同 master,一次抖动只影响一个人,
// 就地重试比把整条结果退回消费者重试更省(消费者重试是第二层)。backoff 是
// 变量以便单测调零。
const ratingWriteAttempts = 3

var ratingWriteBackoff = 50 * time.Millisecond

// ratingAfterLoadHook 测试注入点(生产恒为 nil,照 requeueFrontHook 的包级变量模式):
// ApplyBattleResult 读完全员赛前评分、尚未拿入账标记时调用,用来把"另一实例并发
// 入账同一玩家的另一局"插进中间,证明增量写不丢更新。
var ratingAfterLoadHook func(battleId uint64)

// eloExpected A 对 B 的期望得分:1 / (1 + 10^((B-A)/400))。
func eloExpected(ratingA, ratingB float64) float64 {
	return 1 / (1 + math.Pow(10, (ratingB-ratingA)/400))
}

// teamAverageRating 队伍平均分(队伍为空返回默认值,调用方已排除该情况)。
func teamAverageRating(ratings map[uint64]float64, playerIds []uint64) float64 {
	if len(playerIds) == 0 {
		return defaultRating
	}
	sum := 0.0
	for _, pid := range playerIds {
		sum += ratings[pid]
	}
	return sum / float64(len(playerIds))
}

// applyRatingDelta 给一名玩家写本局增量(ratingApplyScript,有界重试)。
// 返回 (本次是否实际写入, 写后评分的存储形态, err);written=false 且 err=nil 表示
// 这个人本局已入账(续写路径)。
func applyRatingDelta(svcCtx *svc.ServiceContext, battleId uint64, playerId uint64, delta float64, now uint64) (bool, string, error) {
	args := []any{
		strconv.FormatFloat(delta, 'f', -1, 64),
		strconv.FormatUint(now, 10),
		strconv.FormatUint(battleId, 10),
		strconv.Itoa(defaultRating),
		strconv.Itoa(ratingRecentBattlesKeep),
	}
	var lastErr error
	for attempt := 1; attempt <= ratingWriteAttempts; attempt++ {
		res, err := svcCtx.MatchRedis.Eval(ratingApplyScript, []string{matchRatingKey(playerId)}, args...)
		if err == nil {
			arr, _ := res.([]any)
			if len(arr) < 2 {
				return false, "", fmt.Errorf("写评分脚本返回形态异常 player=%d: %v", playerId, res)
			}
			written, _ := arr[0].(int64)
			rating, _ := arr[1].(string)
			return written == 1, rating, nil
		}
		lastErr = err
		if attempt < ratingWriteAttempts && ratingWriteBackoff > 0 {
			time.Sleep(ratingWriteBackoff)
		}
	}
	return false, "", lastErr
}

// ApplyBattleResult 消费一条对局结果并更新 Elo(§11.5)。返回 outcome 与错误:
// err 非 nil 时 outcome=error / partial,调用方(Kafka 消费者)重试会走续写路径把
// 未落账的人补齐(逐人幂等,不会算两次);其余情况已终态。
//
// 规则:
//   - 只对 PVP 队列模式(isRatedMode)计分,PVE / 切磋忽略;
//   - outcome 只认 SIDE_A_WIN(team 0 胜)/ SIDE_B_WIN(team 1 胜)/ DRAW(各 0.5),
//     其余忽略;必须恰好两支非空队伍;
//   - 回合打满按平局:total_rounds ≥ RatingDrawRoundCapFor(config) 的胜负结果按 0.5
//     结算(C++ 引擎回合打满一律判 SIDE_B_WIN,是 PVE"进攻方判负"规则泄漏到 PVP;
//     team 0 = 1v1 锚点 / 5v5 评分最高者所在队,照胜负算会系统性扣他们的分);
//   - Elo:先读全员赛前评分,队伍用平均分算期望,K=32,同队每人同一增量;评分下限 0;
//   - 幂等两层:SETNX match:rating:applied:{battle_id} = applying|Δ_A(TTL 7d),已是
//     done → duplicate,已是 applying → 按记录的 Δ_A 续写;逐人 ratingApplyScript 增量写,
//     hash 的 recent_battles 含本局即跳过。全员落账后标记改 done。写到一半失败:已写
//     的人不回滚,标记留 applying,返回 error / partial 让消费者重试续写。
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
		logx.Infof("[rating] 对局结果不计分 battle=%d mode=%s outcome=%s", battleId, modeName, event.GetOutcome())
		return finish(ratingOutcomeIgnored), nil
	}
	teamA, teamB, ok := splitResultTeams(event)
	if !ok {
		logx.Errorf("[rating] 对局结果队伍形态异常,不计分 battle=%d mode=%s teams=%d", battleId, modeName, len(event.GetTeams()))
		return finish(ratingOutcomeIgnored), nil
	}
	if scoreA != 0.5 {
		if roundCap := svcCtx.Config.RatingDrawRoundCapFor(event.GetBattleConfigId()); roundCap > 0 && event.GetTotalRounds() >= roundCap {
			// 回合打满:引擎判的 SIDE_B_WIN 是 PVE 规则,PVP 按平局。最后一回合真打死
			// 也落在这里(total_rounds == 上限,与打满不可区分),保守按平局。
			logx.Infof("[rating] 回合打满按平局结算 battle=%d mode=%s outcome=%s rounds=%d cap=%d config=%d",
				battleId, modeName, event.GetOutcome(), event.GetTotalRounds(), roundCap, event.GetBattleConfigId())
			metrics.ObserveRatingRoundCapDraw(modeName)
			scoreA = 0.5
		}
	}

	// 先按赛前评分算 Δ_A,再拿标记:标记值要带 Δ_A(续写不重算)。
	all := append(append([]uint64(nil), teamA...), teamB...)
	ratings := loadRatings(svcCtx, all)
	avgA, avgB := teamAverageRating(ratings, teamA), teamAverageRating(ratings, teamB)
	deltaA := eloK * (scoreA - eloExpected(avgA, avgB))
	if ratingAfterLoadHook != nil {
		ratingAfterLoadHook(battleId)
	}

	appliedKey := matchRatingAppliedKey(battleId)
	acquired, err := svcCtx.MatchRedis.SetnxEx(appliedKey, encodeRatingAppliedMarker(ratingAppliedStateApplying, deltaA), ratingAppliedTTLSeconds)
	if err != nil {
		finish(ratingOutcomeError)
		return ratingOutcomeError, fmt.Errorf("写入账标记失败 battle=%d: %w", battleId, err)
	}
	resumed := false
	if !acquired {
		raw, err := svcCtx.MatchRedis.Get(appliedKey)
		if err != nil {
			finish(ratingOutcomeError)
			return ratingOutcomeError, fmt.Errorf("读入账标记失败 battle=%d: %w", battleId, err)
		}
		state, storedDelta, ok := decodeRatingAppliedMarker(raw)
		if !ok || state == ratingAppliedStateDone {
			logx.Infof("[rating] 对局结果已入账,跳过重复投递 battle=%d marker=%q", battleId, raw)
			return finish(ratingOutcomeDuplicate), nil
		}
		// applying:上次(本实例或别的实例)写到一半 —— 按标记里的 Δ_A 续写,逐人幂等。
		deltaA = storedDelta
		resumed = true
		logx.Infof("[rating] 对局结果续写未落账成员 battle=%d mode=%s deltaA=%+.2f", battleId, modeName, deltaA)
	}
	deltaB := -deltaA
	now := nowMs()

	var missing []uint64 // 本次没写成的玩家(留给重试续写)
	var firstErr error
	written := 0
	applyTeam := func(team []uint64, delta float64) {
		for _, pid := range team {
			ok, rating, err := applyRatingDelta(svcCtx, battleId, pid, delta, now)
			if err != nil {
				// 不中断:把其他人先写完,让重试要补的人最少。
				missing = append(missing, pid)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if !ok {
				logx.Infof("[rating] 玩家本局已入账,跳过 battle=%d player=%d rating=%s", battleId, pid, rating)
				continue
			}
			written++
			logx.Infof("[rating] 评分更新 battle=%d mode=%s player=%d %s -> %s (delta=%+.2f)",
				battleId, modeName, pid, formatRating(ratings[pid]), rating, delta)
		}
	}
	applyTeam(teamA, deltaA)
	applyTeam(teamB, deltaB)
	if len(missing) > 0 {
		outcome := ratingOutcomeError
		if written > 0 || resumed {
			outcome = ratingOutcomePartial
		}
		logx.Errorf("[rating] 对局写分未完成,标记留 applying 等重试续写 battle=%d mode=%s 未落账=%v 本次已写=%d: %v",
			battleId, modeName, missing, written, firstErr)
		finish(outcome)
		return outcome, fmt.Errorf("写评分失败 battle=%d 未落账玩家=%v: %w", battleId, missing, firstErr)
	}
	if err := svcCtx.MatchRedis.Setex(appliedKey, encodeRatingAppliedMarker(ratingAppliedStateDone, 0), ratingAppliedTTLSeconds); err != nil {
		// 全员已落账;标记没改成 done 只影响重投的路径 —— 会走续写并被逐人幂等全部跳过,不会算两次。
		logx.Errorf("[rating] 入账标记改 done 失败(全员已落账,重投会被逐人幂等跳过) battle=%d: %v", battleId, err)
	}
	logx.Infof("[rating] 对局入账 battle=%d mode=%s outcome=%s rounds=%d avgA=%.2f avgB=%.2f deltaA=%+.2f resumed=%t teamA=%v teamB=%v",
		battleId, modeName, event.GetOutcome(), event.GetTotalRounds(), avgA, avgB, deltaA, resumed, teamA, teamB)
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

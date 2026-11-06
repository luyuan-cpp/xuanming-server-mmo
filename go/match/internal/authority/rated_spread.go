package authority

import (
	"errors"

	"match/internal/ratingrule"
	"match/internal/scope"

	pb "proto/match"
)

// 计分排队局在提交点复核组内队伍分极差（docs/design/global-match-hierarchical-pools.md §5.5、
// 开头「计分组排」第 7 条）。撮合（poolgroup）已经按锚点容差挑过人，这里用加锁读出的原票和数据库时钟再判一次。

// ErrRatingSpread：计分排队局里，不可拆单位的队伍分最高与最低之差超过组内最长等待者的容差。
var ErrRatingSpread = errors.New("成局成员的评分差距超过容差")

// requireRatedSpread 复核计分排队局：按 queueUnits 分出不可拆单位，每个单位用 ratingrule.UnitCenti 算队伍分
// （成员票上的 rating_centi 快照 + 组队加成），极差不得超过组内最长等待者的容差。
// 撮合用的是锚点的容差，锚点的等待不超过组内最长等待，曲线随等待单调不减，所以这里只会更宽、不会误拒。
// 非计分模式不检查。
func (s *Store) requireRatedSpread(mode uint32, group []*pb.MatchAuthorityTicketRecord, nowMs uint64) error {
	if !ratingrule.Rated(pb.MatchMode(mode)) || len(group) == 0 {
		return nil
	}
	bonus := s.Config.PartyBonusCenti()
	var lowest, highest int64
	var longest uint64
	for index, unit := range queueUnits(group) {
		ratings := make([]int64, 0, len(unit))
		for _, ticket := range unit {
			ratings = append(ratings, ticket.RatingCenti)
			longest = max(longest, scope.WaitMs(ticket.EnqueuedMs, nowMs))
		}
		rating := ratingrule.UnitCenti(ratings, bonus)
		if index == 0 {
			lowest, highest = rating, rating
			continue
		}
		lowest, highest = min(lowest, rating), max(highest, rating)
	}
	if float64(highest-lowest) > s.ratingCurve.AnchorCenti(longest) {
		return ErrRatingSpread
	}
	return nil
}

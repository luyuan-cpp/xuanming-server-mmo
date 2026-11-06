package authority

import (
	"context"
	"database/sql"
	"errors"

	"match/internal/ratingrule"

	pb "proto/match"
)

// ErrPartyRatingGap：计分模式整队开战时，队员在本模式的评分最高与最低之差超过 GlobalMatch.PartyRatingGapMax
// （docs/design/global-match-hierarchical-pools.md 开头第 10 条）。是业务拒绝，team 包把它换成 TeamRatingGapTooLarge。
var ErrPartyRatingGap = errors.New("组队成员的评分差距超过上限")

// queueRatingCentiTx 读整队成员入队时的评分快照（×100），写进成员票的 rating_centi
// （docs/design/global-match-hierarchical-pools.md 开头「计分组排」第 1 条）。撮合算队伍分、提交点复核组内极差
// 都只看票上的这份快照，入队之后评分再变也不改票。
//
// 非计分模式恒为 0（与 PVE 单排票一致）。没有评分记录按默认 1500.00；读故障整笔回滚入队事务，
// 不静默按默认值放行（与单人入队一致，§8）。
//
// 评分按 (player_id, mode) 分开存（R5，2026-11-05）：读的是本模式那条评分轨道，与单人入队同一条 SQL（rating.go 的 ratingCenti）。
func (s *Store) queueRatingCentiTx(ctx context.Context, tx *sql.Tx, player uint64, mode uint32) (int64, error) {
	if !ratingrule.Rated(pb.MatchMode(mode)) {
		return 0, nil
	}
	return ratingCenti(ctx, tx, player, mode)
}

// RequirePartyRatingGapTx 在整队开战的提交事务里复核组队成员评分差上限：计分模式下，队员在本模式的评分
// 最高与最低之差不得超过 GlobalMatch.PartyRatingGapMax（恰好等于可以），否则回 ErrPartyRatingGap。
// 上限为 0、非计分模式、单人队都不读库直接放行；读故障原样返回，由调用方整笔回滚。
// 只在开战时判这一次，已经排上队的队伍不因之后的评分变化被撤。读分与入队快照是同一条 SQL。
func (s *Store) RequirePartyRatingGapTx(ctx context.Context, tx *sql.Tx, mode uint32, players []uint64) error {
	limit := s.Config.PartyRatingGapCenti()
	if limit == 0 || len(players) < 2 || !ratingrule.Rated(pb.MatchMode(mode)) {
		return nil
	}
	var lowest, highest int64
	for index, player := range players {
		rating, err := s.queueRatingCentiTx(ctx, tx, player, mode)
		if err != nil {
			return err
		}
		if index == 0 {
			lowest, highest = rating, rating
			continue
		}
		lowest, highest = min(lowest, rating), max(highest, rating)
	}
	if highest-lowest > limit {
		return ErrPartyRatingGap
	}
	return nil
}

package authority

import (
	"context"
	"database/sql"

	"match/internal/ratingrule"

	pb "proto/match"
)

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

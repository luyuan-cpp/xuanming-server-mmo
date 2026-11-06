package authority

import (
	"context"
	"testing"
	"time"

	"match/internal/config"
	"match/internal/ratingrule"

	pb "proto/match"

	"github.com/stretchr/testify/require"
)

// 计分排队局的组内队伍分极差复核（rated_spread.go）与整队入队读分（party_rating.go）。
// 规则见 docs/design/global-match-hierarchical-pools.md 开头「计分组排」第 1、2、7 条。

const spreadNow = uint64(1_000_000)

func spreadTicket(player, team uint64, rating int64, waitedMs uint64) *pb.MatchAuthorityTicketRecord {
	return &pb.MatchAuthorityTicketRecord{PlayerId: player, TeamId: team, RatingCenti: rating, EnqueuedMs: spreadNow - waitedMs}
}

func TestRequireRatedSpreadOnlyForRatedModes(t *testing.T) {
	s := &Store{}
	far := []*pb.MatchAuthorityTicketRecord{spreadTicket(1, 0, 100_000, 0), spreadTicket(2, 0, 300_000, 0)}
	require.NoError(t, s.requireRatedSpread(uint32(pb.MatchMode_MATCH_MODE_PVE_TEAM), far, spreadNow), "非计分模式不看评分")
	for _, mode := range []pb.MatchMode{pb.MatchMode_MATCH_MODE_1V1, pb.MatchMode_MATCH_MODE_5V5, pb.MatchMode_MATCH_MODE_PVP_TEAM} {
		require.ErrorIs(t, s.requireRatedSpread(uint32(mode), far, spreadNow), ErrRatingSpread, "计分模式 %s 要复核组内极差", mode)
	}
	require.NoError(t, s.requireRatedSpread(uint32(pb.MatchMode_MATCH_MODE_1V1), nil, spreadNow))
}

func TestRequireRatedSpreadUsesLongestWait(t *testing.T) {
	s := &Store{} // 未装配曲线按默认值：0～4 秒 ±100 分，5 秒起 ±200，等满 90 秒不限
	mode := uint32(pb.MatchMode_MATCH_MODE_1V1)
	require.NoError(t, s.requireRatedSpread(mode, []*pb.MatchAuthorityTicketRecord{spreadTicket(1, 0, 150_000, 0), spreadTicket(2, 0, 160_000, 0)}, spreadNow), "恰好等于容差可以成局")
	fresh := []*pb.MatchAuthorityTicketRecord{spreadTicket(1, 0, 150_000, 0), spreadTicket(2, 0, 165_000, 0)}
	require.ErrorIs(t, s.requireRatedSpread(mode, fresh, spreadNow), ErrRatingSpread)
	waited := []*pb.MatchAuthorityTicketRecord{spreadTicket(1, 0, 150_000, 0), spreadTicket(2, 0, 165_000, 5_000)}
	require.NoError(t, s.requireRatedSpread(mode, waited, spreadNow), "按组内最长等待者的容差复核，只会比撮合时更宽")
	terminal := []*pb.MatchAuthorityTicketRecord{spreadTicket(1, 0, 100_000, 0), spreadTicket(2, 0, 300_000, 90_000)}
	require.NoError(t, s.requireRatedSpread(mode, terminal, spreadNow), "等满终态兜底后容差不限")

	custom := &Store{ratingCurve: ratingCurveFixture()}
	require.ErrorIs(t, custom.requireRatedSpread(mode, []*pb.MatchAuthorityTicketRecord{spreadTicket(1, 0, 150_000, 0), spreadTicket(2, 0, 156_000, 0)}, spreadNow), ErrRatingSpread, "装配的曲线优先于默认值")
}

func TestRequireRatedSpreadScoresWholeParties(t *testing.T) {
	mode := uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM)
	// 双排成员 1400 / 1600，队伍分 1500；对面两个 1500 单人。按人算极差 200，按单位算 0。
	group := []*pb.MatchAuthorityTicketRecord{spreadTicket(1, 9, 140_000, 0), spreadTicket(2, 9, 160_000, 0), spreadTicket(3, 0, 150_000, 0), spreadTicket(4, 0, 150_000, 0)}
	require.NoError(t, (&Store{}).requireRatedSpread(mode, group, spreadNow), "整队按队伍分算，不拆成个人")

	// 组队加成每人 25 分：双排队伍分 1525。对面是 1400 时差 125，超出默认的 ±100。
	bonus := &Store{Config: config.GlobalMatchConf{PartyRatingBonusPerMember: 25}}
	require.NoError(t, bonus.requireRatedSpread(mode, group, spreadNow))
	weaker := []*pb.MatchAuthorityTicketRecord{spreadTicket(1, 9, 140_000, 0), spreadTicket(2, 9, 160_000, 0), spreadTicket(3, 0, 140_000, 0), spreadTicket(4, 0, 140_000, 0)}
	require.ErrorIs(t, bonus.requireRatedSpread(mode, weaker, spreadNow), ErrRatingSpread, "组队加成计入队伍分")
	require.NoError(t, (&Store{}).requireRatedSpread(mode, weaker, spreadNow), "没有加成时差 100，恰好在容差内")
}

// ratingCurveFixture 起始 ±50 分，比默认的 ±100 窄。
func ratingCurveFixture() ratingrule.Curve {
	return ratingrule.Curve{Base: 50, StepSeconds: 5, StepDelta: 50, Max: 500, MaxWaitSeconds: 60}
}

func TestAuthorityTiDBQueueRatingSnapshot(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := s.DB.ExecContext(ctx, "INSERT INTO match_rating(player_id,rating_centi,games,updated_ms,score_changed_ms) VALUES(?,?,?,?,?)", 81001, 163_250, 4, 1, 1)
	require.NoError(t, err)
	tx, err := s.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	pvp := uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM)
	rating, err := s.queueRatingCentiTx(ctx, tx, 81001, pvp)
	require.NoError(t, err)
	require.Equal(t, int64(163_250), rating, "计分模式读成员当前评分")
	rating, err = s.queueRatingCentiTx(ctx, tx, 81002, pvp)
	require.NoError(t, err)
	require.Equal(t, int64(150_000), rating, "没有评分记录按 1500.00")
	rating, err = s.queueRatingCentiTx(ctx, tx, 81001, uint32(pb.MatchMode_MATCH_MODE_PVE_TEAM))
	require.NoError(t, err)
	require.Zero(t, rating, "非计分模式不读评分")
}

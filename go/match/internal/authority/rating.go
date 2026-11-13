package authority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"

	"match/internal/metrics"
	"match/internal/ratingrule"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
	pb "proto/match"
	rankpb "proto/rank"
)

// 评分按 (player_id, mode) 分开存,榜也按模式分开(docs/design/global-match-hierarchical-pools.md §8)。
// 哪些模式计分只看 ratingrule.Rated;每个计分模式写哪张榜只看下面这张表。两者由 TestRankBoardTypesCoverExactlyRatedModes
// 绑在一起:计分模式没有榜,它的结果在提交前就被拒,结果消费会卡住。

// rankBoardTypes 是每个计分模式写的那张榜的 board_type(rank 的榜键其余部分固定:GLOBAL、period "all"、第 1 代)。
// 必须一个模式一张榜:批次的 score_version 是「这名玩家在本模式第几局」,两种模式混在一张榜上,1V1 的第 9 局
// 会被 5V5 的第 3 局当成旧版本挡掉(或反过来把新分盖回旧分),榜就错了。
// board_type 是客户端契约(GetRankPage 按它找榜),取值登记在 proto/rank/rank.proto 的 RankBoardType,
// 客户端与两版服务器用同一组生成常量;这里只决定「哪个模式写哪张榜」。增删改要与客户端、Java 版同批。
// 1V1 沿用拆分前唯一那张评分榜的键(robot rank-smoke 读的就是它)。PVP_TEAM 各档每侧人数共用一条评分轨道、一张榜。
var rankBoardTypes = map[pb.MatchMode]rankpb.RankBoardType{
	pb.MatchMode_MATCH_MODE_1V1:      rankpb.RankBoardType_RANK_BOARD_TYPE_MATCH_1V1,
	pb.MatchMode_MATCH_MODE_5V5:      rankpb.RankBoardType_RANK_BOARD_TYPE_MATCH_5V5,
	pb.MatchMode_MATCH_MODE_PVP_TEAM: rankpb.RankBoardType_RANK_BOARD_TYPE_MATCH_PVP_TEAM,
}

// RankBoardType 返回计分模式那张榜的 board_type;不计分的模式没有评分轨道,也没有榜。
func RankBoardType(mode uint32) (uint32, bool) {
	if !ratingrule.Rated(pb.MatchMode(mode)) {
		return 0, false
	}
	boardType, ok := rankBoardTypes[pb.MatchMode(mode)]
	return uint32(boardType), ok
}

// rowQuerier 是 *sql.DB 与 *sql.Tx 共有的单行读。
type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ratingCenti 读一名玩家在一个计分模式下的评分(×100),没有记录按 ratingrule.DefaultCenti。
// 读评分的 SQL 只有这一处:入队(Rating)与整队入队事务里的快照(queueRatingCentiTx)都走它。
func ratingCenti(ctx context.Context, q rowQuerier, player uint64, mode uint32) (int64, error) {
	var rating int64
	e := q.QueryRowContext(ctx, "SELECT rating_centi FROM match_rating WHERE player_id=? AND mode=?", player, mode).Scan(&rating)
	if errors.Is(e, sql.ErrNoRows) {
		return ratingrule.DefaultCenti, nil
	}
	return rating, e
}

// Rating 读玩家在一个计分模式下的评分,默认1500.00。SQL不存在与读故障严格区分，由只影响凑单质量的调用方决定是否用缺省。
// 不计分的模式没有评分轨道,直接报错:调用方先按 ratingrule.Rated 分流,读到这里是接线错误,不能静默给缺省分。
func (s *Store) Rating(ctx context.Context, pid uint64, mode uint32) (int64, error) {
	if _, rated := RankBoardType(mode); !rated {
		return 0, fmt.Errorf("模式 %d 不计分,没有评分", mode)
	}
	return ratingCenti(ctx, s.DB, pid, mode)
}

// rankSourceShards 是投榜来源的片数(docs/design/player-data-tidb-scaling-plan.md §8.1 第 2 条)。
// 片号进主键,改它等于重写来源表和 outbox,建表后不可改。
const rankSourceShards = 64

// rankSourceShard 按对局 id 选片。发号器发的 id 已经打散(AGENTS §7 不变量 1),取模足够均匀。
func rankSourceShard(battle uint64) uint32 { return uint32(battle % rankSourceShards) }

// ApplyRating 把整局评级、重复结果摘要、本模式本片连续投榜游标及 outbox 放在同一 SQL 提交点。
// mode 是这局的计分模式(结果里的 match_mode):评分行、来源片、outbox 都按它分开,不同模式的对局互不锁对方的行。
// 锁序：本局applied行→本模式本片来源行→(player_id升序, mode)评分行。结果未知重放同battle/digest，不会再加一局。
// 重复判定在停写检查之前:已入账的局停写之后迟到重放,照样回「重复」,不会卡住结果消费。
// 本模式的投榜来源停写(StopRankSource,封榜中)之后的新局:评分照记、入账标记照写,只是这局不进榜——不写 outbox、
// 不推进来源游标,StopRankSource 返回的封榜批次总数不变;回 applied=true,另计 match_rating_unpublished_total。
// 不报错:结果消费是所有模式共用的一条流,报错会让它重试到 HOLD,把别的模式、别的局一起卡住。
// 评分是玩家的实力轨道,榜只是它的快照;封榜不吞掉已经打完的计分局。
//
// 来源按对局拆片之后,全局只剩每片自己的顺序,跨片的批次可能乱序到榜。批次里是绝对分(SET),所以每个实体带
// score_version = 这名玩家在本模式第几局:它在 (玩家, 模式) 行锁下递增,与本模式分数的先后完全一致,
// 榜上旧版本迟到不会覆盖新版本。版本只在一个模式内可比,所以榜也按模式分开(rankBoardTypes)。
// achieved_ms 也在这里定(分数最后一次变化的时刻),同分决胜不看到达顺序。
func (s *Store) ApplyRating(ctx context.Context, battle uint64, payload proto.Message, mode uint32, teamA, teamB []uint64, scoreA float64) (bool, error) {
	if battle == 0 || len(teamA) == 0 || len(teamB) == 0 {
		return false, errors.New("评级结果缺少对局/队伍")
	}
	if _, rated := RankBoardType(mode); !rated {
		return false, fmt.Errorf("模式 %d 不计分,没有评分轨道与榜", mode)
	}
	encoded, e := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if e != nil {
		return false, e
	}
	digest := sha256.Sum256(encoded)
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	stamp, e := now(ctx, tx)
	if e != nil {
		return false, e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO match_rating_applied(battle_id,payload_digest,applied_ms) VALUES(?,?,?)", battle, digest[:], stamp)
	if e != nil {
		var duplicate *mysql.MySQLError
		if !errors.As(e, &duplicate) || duplicate.Number != 1062 {
			return false, e
		}
		var prior []byte
		if e = tx.QueryRowContext(ctx, "SELECT payload_digest FROM match_rating_applied WHERE battle_id=?", battle).Scan(&prior); e != nil {
			return false, e
		}
		if !bytes.Equal(prior, digest[:]) {
			return false, errors.New("同对局重复结果内容不一致")
		}
		return false, nil
	}
	shard := rankSourceShard(battle)
	if _, e = tx.ExecContext(ctx, "INSERT INTO match_rank_source(source_epoch,mode,shard,produced_cursor,committed_cursor,board_id,stopped) VALUES(1,?,?,0,0,0,0) ON DUPLICATE KEY UPDATE source_epoch=source_epoch", mode, shard); e != nil {
		return false, e
	}
	var cursor uint64
	var stopped bool
	if e = tx.QueryRowContext(ctx, "SELECT produced_cursor,stopped FROM match_rank_source WHERE source_epoch=1 AND mode=? AND shard=? FOR UPDATE", mode, shard).Scan(&cursor, &stopped); e != nil {
		return false, e
	}
	members := append(append([]uint64{}, teamA...), teamB...)
	sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
	ratings := map[uint64]int64{}
	games := map[uint64]uint64{}
	changedAt := map[uint64]uint64{}
	for i, pid := range members {
		if pid == 0 || i > 0 && members[i-1] == pid {
			return false, errors.New("评级名单为空或重复")
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO match_rating(player_id,mode,rating_centi,games,updated_ms,score_changed_ms) VALUES(?,?,?,0,?,?) ON DUPLICATE KEY UPDATE player_id=player_id", pid, mode, ratingrule.DefaultCenti, stamp, stamp); e != nil {
			return false, e
		}
		var rating int64
		var played, changed uint64
		if e = tx.QueryRowContext(ctx, "SELECT rating_centi,games,score_changed_ms FROM match_rating WHERE player_id=? AND mode=? FOR UPDATE", pid, mode).Scan(&rating, &played, &changed); e != nil {
			return false, e
		}
		ratings[pid] = rating
		games[pid] = played
		changedAt[pid] = changed
	}
	average := func(team []uint64) float64 {
		sum := 0.
		for _, pid := range team {
			sum += float64(ratings[pid]) / 100
		}
		return sum / float64(len(team))
	}
	delta := int64(math.Round(3200 * (scoreA - 1/(1+math.Pow(10, (average(teamB)-average(teamA))/400)))))
	sides := map[uint64]bool{}
	for _, pid := range teamA {
		sides[pid] = true
	}
	batch := &rankpb.SubmitRankScoresRequest{Generation: 1, SourceEpoch: 1, SourceShard: shard, SourceCursor: cursor + 1, RequestId: battle, Mode: rankpb.RankSubmitMode_RANK_SUBMIT_SET}
	for _, pid := range members {
		change := -delta
		if sides[pid] {
			change = delta
		}
		next := max(int64(0), ratings[pid]+change)
		achieved := changedAt[pid]
		if next != ratings[pid] {
			achieved = stamp
		}
		if _, e = tx.ExecContext(ctx, "UPDATE match_rating SET rating_centi=?,games=games+1,updated_ms=?,score_changed_ms=? WHERE player_id=? AND mode=?", next, stamp, achieved, pid, mode); e != nil {
			return false, e
		}
		batch.Entries = append(batch.Entries, &rankpb.RankScoreInput{EntityId: pid, Score: next, AchievedMs: achieved, ScoreVersion: games[pid] + 1})
	}
	if !stopped {
		// 本模式本片的下一个投榜批次与评分同事务写进 outbox,来源游标连续推进。
		var body []byte
		if body, e = (proto.MarshalOptions{Deterministic: true}).Marshal(batch); e != nil {
			return false, e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO match_rank_outbox(source_epoch,mode,shard,source_cursor,request_id,entries) VALUES(1,?,?,?,?,?)", mode, shard, cursor+1, battle, body); e != nil {
			return false, e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE match_rank_source SET produced_cursor=? WHERE source_epoch=1 AND mode=? AND shard=?", cursor+1, mode, shard); e != nil {
			return false, e
		}
	}
	if e = tx.Commit(); e != nil {
		return false, fmt.Errorf("评级提交结果未知，按原局重放: %w", e)
	}
	if stopped {
		metrics.ObserveRatingUnpublished(pb.MatchMode(mode).String())
	}
	return true, nil
}

// StopRankSource 停写一个计分模式的投榜来源:按片号升序逐片与评级争来源行锁,全部标记停写;返回值是本模式各片
// 真实最终游标之和,就是本模式那张榜上该有的批次总数(封榜的 expected_source_cursor),不能用时间猜测。
// 停写只停投榜:之后的局评分照记、不进榜(见 ApplyRating),游标不再前进,所以这个总数之后不变。
// 封榜按模式:每个计分模式一张榜、各自封榜,停写也就按模式停。要停全部计分模式,对每个计分模式各调一次,
// 先后不限、彼此独立:没有跨模式的原子要求,一个模式停了不影响别的模式继续入账与投榜。
// 还没打过对局的片也要建出来并标记,否则之后第一局会在这一片建出没停写的来源行。
// 评级只持本模式的一片,所以这里按序持多片不会和它成环;不同模式的来源行互不相干。
func (s *Store) StopRankSource(ctx context.Context, epoch uint64, mode uint32) (uint64, error) {
	if _, rated := RankBoardType(mode); !rated {
		return 0, fmt.Errorf("模式 %d 不计分,没有投榜来源", mode)
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	var total uint64
	for shard := uint32(0); shard < rankSourceShards; shard++ {
		if _, e = tx.ExecContext(ctx, "INSERT INTO match_rank_source(source_epoch,mode,shard,produced_cursor,committed_cursor,board_id,stopped) VALUES(?,?,?,0,0,0,1) ON DUPLICATE KEY UPDATE source_epoch=source_epoch", epoch, mode, shard); e != nil {
			return 0, e
		}
		var cursor uint64
		if e = tx.QueryRowContext(ctx, "SELECT produced_cursor FROM match_rank_source WHERE source_epoch=? AND mode=? AND shard=? FOR UPDATE", epoch, mode, shard).Scan(&cursor); e != nil {
			return 0, e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE match_rank_source SET stopped=1 WHERE source_epoch=? AND mode=? AND shard=?", epoch, mode, shard); e != nil {
			return 0, e
		}
		total += cursor
	}
	return total, tx.Commit()
}

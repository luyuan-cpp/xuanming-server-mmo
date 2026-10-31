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

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
	rankpb "proto/rank"
)

// Rating 默认1500.00。SQL不存在与读故障严格区分，由只影响凑单质量的调用方决定是否用缺省。
func (s *Store) Rating(ctx context.Context, pid uint64) (int64, error) {
	var rating int64
	e := s.DB.QueryRowContext(ctx, "SELECT rating_centi FROM match_rating WHERE player_id=?", pid).Scan(&rating)
	if errors.Is(e, sql.ErrNoRows) {
		return 150000, nil
	}
	return rating, e
}

// rankSourceShards 是投榜来源的片数(docs/design/player-data-tidb-scaling-plan.md §8.1 第 2 条)。
// 片号进主键,改它等于重写来源表和 outbox,建表后不可改。
const rankSourceShards = 64

// rankSourceShard 按对局 id 选片。发号器发的 id 已经打散(AGENTS §7 不变量 1),取模足够均匀。
func rankSourceShard(battle uint64) uint32 { return uint32(battle % rankSourceShards) }

// ApplyRating 把整局评级、重复结果摘要、本片连续投榜游标及 outbox 放在同一 SQL 提交点。
// 锁序：本局applied行→本片来源行→player_id升序。结果未知重放同battle/digest，不会再加一局。
//
// 来源按对局拆片之后,全局只剩每片自己的顺序,跨片的批次可能乱序到榜。批次里是绝对分(SET),所以每个实体带
// score_version = 这名玩家第几局:它在玩家行锁下递增,与分数的先后完全一致,榜上旧版本迟到不会覆盖新版本。
// achieved_ms 也在这里定(分数最后一次变化的时刻),同分决胜不看到达顺序。
func (s *Store) ApplyRating(ctx context.Context, battle uint64, payload proto.Message, mode uint32, teamA, teamB []uint64, scoreA float64) (bool, error) {
	if battle == 0 || len(teamA) == 0 || len(teamB) == 0 {
		return false, errors.New("评级结果缺少对局/队伍")
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
	if _, e = tx.ExecContext(ctx, "INSERT INTO match_rank_source(source_epoch,shard,produced_cursor,committed_cursor,board_id,stopped) VALUES(1,?,0,0,0,0) ON DUPLICATE KEY UPDATE source_epoch=source_epoch", shard); e != nil {
		return false, e
	}
	var cursor uint64
	var stopped bool
	if e = tx.QueryRowContext(ctx, "SELECT produced_cursor,stopped FROM match_rank_source WHERE source_epoch=1 AND shard=? FOR UPDATE", shard).Scan(&cursor, &stopped); e != nil {
		return false, e
	}
	if stopped {
		return false, errors.New("评级代次已停写，必须显式协调后开启新代")
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
		if _, e = tx.ExecContext(ctx, "INSERT INTO match_rating(player_id,rating_centi,games,updated_ms,score_changed_ms) VALUES(?,150000,0,?,?) ON DUPLICATE KEY UPDATE player_id=player_id", pid, stamp, stamp); e != nil {
			return false, e
		}
		var rating int64
		var played, changed uint64
		if e = tx.QueryRowContext(ctx, "SELECT rating_centi,games,score_changed_ms FROM match_rating WHERE player_id=? FOR UPDATE", pid).Scan(&rating, &played, &changed); e != nil {
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
		if _, e = tx.ExecContext(ctx, "UPDATE match_rating SET rating_centi=?,games=games+1,updated_ms=?,score_changed_ms=? WHERE player_id=?", next, stamp, achieved, pid); e != nil {
			return false, e
		}
		batch.Entries = append(batch.Entries, &rankpb.RankScoreInput{EntityId: pid, Score: next, AchievedMs: achieved, ScoreVersion: games[pid] + 1})
	}
	body, e := proto.MarshalOptions{Deterministic: true}.Marshal(batch)
	if e != nil {
		return false, e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO match_rank_outbox(source_epoch,shard,source_cursor,request_id,entries) VALUES(1,?,?,?,?)", shard, cursor+1, battle, body); e != nil {
		return false, e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE match_rank_source SET produced_cursor=? WHERE source_epoch=1 AND shard=?", cursor+1, shard); e != nil {
		return false, e
	}
	if e = tx.Commit(); e != nil {
		return false, fmt.Errorf("评级提交结果未知，按原局重放: %w", e)
	}
	return true, nil
}

// StopRankSource 按片号升序逐片与评级争来源行锁,全部标记停写:返回值是各片真实最终游标之和,
// 就是榜上该有的批次总数(封榜的 expected_source_cursor),不能用时间猜测。
// 还没打过对局的片也要建出来并标记,否则之后第一局会在这一片建出没停写的来源行。
// 评级只持自己那一片,所以这里按序持多片不会和它成环。
func (s *Store) StopRankSource(ctx context.Context, epoch uint64) (uint64, error) {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	var total uint64
	for shard := uint32(0); shard < rankSourceShards; shard++ {
		if _, e = tx.ExecContext(ctx, "INSERT INTO match_rank_source(source_epoch,shard,produced_cursor,committed_cursor,board_id,stopped) VALUES(?,?,0,0,0,1) ON DUPLICATE KEY UPDATE source_epoch=source_epoch", epoch, shard); e != nil {
			return 0, e
		}
		var cursor uint64
		if e = tx.QueryRowContext(ctx, "SELECT produced_cursor FROM match_rank_source WHERE source_epoch=? AND shard=? FOR UPDATE", epoch, shard).Scan(&cursor); e != nil {
			return 0, e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE match_rank_source SET stopped=1 WHERE source_epoch=? AND shard=?", epoch, shard); e != nil {
			return 0, e
		}
		total += cursor
	}
	return total, tx.Commit()
}

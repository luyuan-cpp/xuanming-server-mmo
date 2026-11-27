package data

import (
	"context"
	"database/sql"
	"errors"

	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	pb "proto/guild"
	matchpb "proto/match"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

const MaxGuildJianghuWallRecords = 20

// JianghuGrudgeStore 抽象帮派生死状恩怨墙的权威 SQL 读写，生产由 *GuildRepo 实现，单测可注入内存替身。
type JianghuGrudgeStore interface {
	RecordJianghuDuelResult(ctx context.Context, ev *kafkapb.BattleResultEvent) (bool, error)
	GetGuildJianghuGrudgeWall(ctx context.Context, guildID uint64) ([]*pb.GuildJianghuRivalSummaryView, []*pb.GuildJianghuGrudgeRecordView, error)
}

var _ JianghuGrudgeStore = (*GuildRepo)(nil)

// RecordJianghuDuelResult 在单 RC 事务内幂等写入两帮的生死状恩怨汇总与近期 20 场恩怨战绩。
// 仅当双方均为纯同帮小队（side_a_guild_id != 0 && side_b_guild_id != 0 && side_a_guild_id != side_b_guild_id）
// 且对局正常完赛（total_rounds > 0 && outcome != VOID）时入账。
// 幂等只认 guild_jianghu_grudge_applied(一局一行、不裁剪):战绩行每帮只留 20 场,被裁掉之后重投的旧结果查不到它,
// 当不了去重键(数据覆写审计 r2 第 4 轮 r2-jh#3)。
func (r *GuildRepo) RecordJianghuDuelResult(ctx context.Context, ev *kafkapb.BattleResultEvent) (bool, error) {
	if r == nil || r.db == nil || ev == nil || ev.GetBattleId() == 0 {
		return false, nil
	}
	if ev.GetMatchMode() != uint32(matchpb.MatchMode_MATCH_MODE_JIANGHU_DUEL) {
		return false, nil
	}
	if ev.GetTotalRounds() == 0 || ev.GetOutcome() == battlepb.EBattleOutcome_BATTLE_OUTCOME_VOID {
		return false, nil
	}
	duel := ev.GetJianghuDuelContext()
	if duel == nil {
		return false, nil
	}
	gidA, gidB := duel.GetSideAGuildId(), duel.GetSideBGuildId()
	if gidA == 0 || gidB == 0 || gidA == gidB {
		return false, nil
	}

	var outA, outB uint32
	switch ev.GetOutcome() {
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN:
		outA, outB = 1, 2
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN:
		outA, outB = 2, 1
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW:
		outA, outB = 3, 3
	default:
		return false, nil
	}
	finishedMs := ev.GetFinishedAtMs()
	if finishedMs == 0 {
		finishedMs = ev.GetBattleId()
	}

	type guildSide struct {
		guildID      uint64
		rivalID      uint64
		rivalName    string
		outcome      uint32
		ourCaptain   string
		rivalCaptain string
	}
	sides := []guildSide{
		{
			guildID:      gidA,
			rivalID:      gidB,
			rivalName:    duel.GetSideBGuildName(),
			outcome:      outA,
			ourCaptain:   duel.GetChallengerCaptainName(),
			rivalCaptain: duel.GetTargetCaptainName(),
		},
		{
			guildID:      gidB,
			rivalID:      gidA,
			rivalName:    duel.GetSideAGuildName(),
			outcome:      outB,
			ourCaptain:   duel.GetTargetCaptainName(),
			rivalCaptain: duel.GetChallengerCaptainName(),
		},
	}
	// 按 guild_id 升序加锁，严格防双帮并发结算交叉死锁（锁序位置 20 -> 21）。
	if sides[0].guildID > sides[1].guildID {
		sides[0], sides[1] = sides[1], sides[0]
	}

	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// 非加锁幂等预检：入账闸门已有这一局就整笔直接幂等返回,重复的事件不必先把两帮的行锁一遍。
	// 普通读不取锁、不占全序位置;权威判定在事务末尾插闸门那一步。
	var applied uint64
	err = tx.QueryRowContext(ctx,
		"SELECT battle_id FROM guild_jianghu_grudge_applied WHERE battle_id=?", ev.GetBattleId()).Scan(&applied)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	// Pass 1：按 guild_id 升序全部写完位置 20 (guild_jianghu_rival_summary)，严格遵守全局表锁序 20 -> 21。
	for _, s := range sides {
		winInc, lossInc, drawInc := uint32(0), uint32(0), uint32(0)
		switch s.outcome {
		case 1:
			winInc = 1
		case 2:
			lossInc = 1
		case 3:
			drawInc = 1
		}
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO guild_jianghu_rival_summary(guild_id,rival_guild_id,rival_guild_name,wins,losses,draws,last_battle_ms)
			 VALUES(?,?,?,?,?,?,?)
			 ON DUPLICATE KEY UPDATE
			   rival_guild_name=VALUES(rival_guild_name),
			   wins=wins+VALUES(wins),
			   losses=losses+VALUES(losses),
			   draws=draws+VALUES(draws),
			   last_battle_ms=GREATEST(last_battle_ms, VALUES(last_battle_ms))`,
			s.guildID, s.rivalID, s.rivalName, winInc, lossInc, drawInc, finishedMs,
		); err != nil {
			return false, err
		}
	}

	// Pass 2：按 guild_id 升序全部写完位置 21 (guild_jianghu_grudge_record 写入与 20 条裁剪)；命中并发 1062 则整事务回滚保幂等。
	for _, s := range sides {
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO guild_jianghu_grudge_record(guild_id,battle_id,rival_guild_id,rival_guild_name,outcome_for_guild,writ_tier,team_size,total_rounds,our_captain_name,rival_captain_name,finished_ms)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			s.guildID, ev.GetBattleId(), s.rivalID, s.rivalName, s.outcome,
			duel.GetWritTier(), duel.GetTeamSize(), ev.GetTotalRounds(),
			s.ourCaptain, s.rivalCaptain, finishedMs,
		); err != nil {
			var dup *mysqlDriver.MySQLError
			if errors.As(err, &dup) && dup.Number == 1062 {
				return false, nil
			}
			return false, err
		}

		if err = trimGuildJianghuRecords(ctx, tx, s.guildID); err != nil {
			return false, err
		}
	}

	// Pass 3：写位置 23 (guild_jianghu_grudge_applied) 入账闸门,本局唯一的去重键。预检之后到这里之间同一局的另一笔先提交了,
	// 或这局的战绩行早已被裁掉(Pass 2 撞不上主键),都在这一步撞 1062,整笔回滚,汇总不会多算。
	if _, err = tx.ExecContext(ctx,
		"INSERT INTO guild_jianghu_grudge_applied(battle_id,applied_ms) VALUES(?,CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED))",
		ev.GetBattleId(),
	); err != nil {
		var dup *mysqlDriver.MySQLError
		if errors.As(err, &dup) && dup.Number == 1062 {
			return false, nil
		}
		return false, err
	}

	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func trimGuildJianghuRecords(ctx context.Context, tx *sql.Tx, guildID uint64) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT battle_id FROM guild_jianghu_grudge_record
		 WHERE guild_id=?
		 ORDER BY finished_ms DESC, battle_id DESC LIMIT 100 OFFSET ?`,
		guildID, MaxGuildJianghuWallRecords)
	if err != nil {
		return err
	}
	var stale []uint64
	for rows.Next() {
		var bid uint64
		if scanErr := rows.Scan(&bid); scanErr != nil {
			rows.Close()
			return scanErr
		}
		stale = append(stale, bid)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, bid := range stale {
		if _, err = tx.ExecContext(ctx,
			"DELETE FROM guild_jianghu_grudge_record WHERE guild_id=? AND battle_id=?",
			guildID, bid); err != nil {
			return err
		}
	}
	return nil
}

func (r *GuildRepo) GetGuildJianghuGrudgeWall(ctx context.Context, guildID uint64) ([]*pb.GuildJianghuRivalSummaryView, []*pb.GuildJianghuGrudgeRecordView, error) {
	if r == nil || r.db == nil || guildID == 0 {
		return nil, nil, nil
	}
	rivalRows, err := r.db.QueryContext(ctx,
		`SELECT rival_guild_id,rival_guild_name,wins,losses,draws,last_battle_ms
		 FROM guild_jianghu_rival_summary
		 WHERE guild_id=?
		 ORDER BY last_battle_ms DESC, rival_guild_id DESC LIMIT ?`,
		guildID, MaxGuildJianghuWallRecords)
	if err != nil {
		return nil, nil, err
	}
	defer rivalRows.Close()

	rivals := make([]*pb.GuildJianghuRivalSummaryView, 0, 8)
	for rivalRows.Next() {
		v := &pb.GuildJianghuRivalSummaryView{}
		if scanErr := rivalRows.Scan(&v.RivalGuildId, &v.RivalGuildName, &v.Wins, &v.Losses, &v.Draws, &v.LastBattleMs); scanErr != nil {
			return nil, nil, scanErr
		}
		rivals = append(rivals, v)
	}
	if err = rivalRows.Err(); err != nil {
		return nil, nil, err
	}

	recRows, err := r.db.QueryContext(ctx,
		`SELECT battle_id,rival_guild_id,rival_guild_name,outcome_for_guild,writ_tier,team_size,total_rounds,our_captain_name,rival_captain_name,finished_ms
		 FROM guild_jianghu_grudge_record
		 WHERE guild_id=?
		 ORDER BY finished_ms DESC, battle_id DESC LIMIT ?`,
		guildID, MaxGuildJianghuWallRecords)
	if err != nil {
		return nil, nil, err
	}
	defer recRows.Close()

	records := make([]*pb.GuildJianghuGrudgeRecordView, 0, MaxGuildJianghuWallRecords)
	for recRows.Next() {
		v := &pb.GuildJianghuGrudgeRecordView{}
		if scanErr := recRows.Scan(
			&v.BattleId, &v.RivalGuildId, &v.RivalGuildName, &v.OutcomeForGuild,
			&v.WritTier, &v.TeamSize, &v.TotalRounds,
			&v.OurCaptainName, &v.RivalCaptainName, &v.FinishedMs,
		); scanErr != nil {
			return nil, nil, scanErr
		}
		records = append(records, v)
	}
	if err = recRows.Err(); err != nil {
		return nil, nil, err
	}
	return rivals, records, nil
}
